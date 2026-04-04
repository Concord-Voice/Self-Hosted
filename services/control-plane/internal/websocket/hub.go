package websocket

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/models"
	"github.com/redis/go-redis/v9"
)

const (
	keyChannelID      = "channel_id"
	keyConversationID = "conversation_id"
	keyUserID         = "user_id"
	keyServerID       = "server_id"
	keyMessage        = "message"
	keyContent        = "content"
	keyIsEncrypted    = "is_encrypted"
	keyKeyVersion     = "key_version"
	keyCreatedAt      = "created_at"
	keyUpdatedAt      = "updated_at"
	keyUsername       = "username"
	keyIsTyping       = "is_typing"
	keyStatus         = "status"
	keyCounts         = "counts"
	keyNonce          = "nonce"
	presenceKeyFmt    = "presence:%s"
	statusOnline      = "online"
	statusOffline     = "offline"
	statusInvisible   = "invisible"
	sessionRevoked    = "session_revoked"
)

// Hub maintains the set of active clients and broadcasts messages
type Hub struct {
	// Database connection for authorization checks
	db *sql.DB

	// Redis client for presence persistence
	redis *redis.Client

	// Mutex protecting maps accessed from outside the Run goroutine
	mu sync.RWMutex

	// Registered clients (client ID -> Client)
	clients map[uuid.UUID]*Client

	// User ID to client IDs mapping (for multi-device support)
	userClients map[uuid.UUID]map[uuid.UUID]bool

	// Channel subscriptions (channel ID -> set of client IDs)
	channelSubscriptions map[uuid.UUID]map[uuid.UUID]bool

	// Username cache (user ID -> username) for typing indicators
	usernames map[uuid.UUID]string

	// Server subscriptions (server ID -> set of client IDs)
	// Used for lightweight unread notifications without full message delivery
	serverSubscriptions map[uuid.UUID]map[uuid.UUID]bool

	// DM conversation subscriptions (conversation ID -> set of client IDs)
	dmSubscriptions map[uuid.UUID]map[uuid.UUID]bool

	// Register requests from clients
	register chan *Client

	// Unregister requests from clients
	unregister chan *Client

	// Incoming messages from clients
	incoming chan IncomingMessage

	// Broadcast messages to all clients in a channel
	broadcast chan BroadcastMessage

	// Global broadcast messages to all connected clients
	globalBroadcast chan OutgoingMessage

	// User-scoped broadcast messages (sent to all clients of a specific user)
	userBroadcast chan UserBroadcastMessage

	// Server-scoped broadcast messages (sent to all clients subscribed to a server)
	serverBroadcast chan ServerBroadcastMessage

	// DM-scoped broadcast messages (sent to all clients subscribed to a DM conversation)
	dmBroadcast chan DMBroadcastMessage

	// Force-disconnect all clients for a user (server-side session termination)
	disconnectUser chan uuid.UUID

	// Force-disconnect a specific session (targeted single-session revocation)
	disconnectSession chan string

	// Signal to recompute and broadcast server voice counts
	voiceCountSignal chan struct{}

	// Shutdown signal
	done chan struct{}

	// Closed when Run() returns, allowing callers to wait for a clean exit
	stopped chan struct{}

	// Debounced online-count broadcasting: instead of hitting DB+Redis on every
	// presence change, we accumulate affected user IDs and flush after a short
	// delay so bursts (e.g., many users reconnecting) collapse into one broadcast.
	onlineCountPending map[uuid.UUID]bool
	onlineCountTimer   *time.Timer

	// Mention permission checker (injected after construction via SetMentionChecker)
	mentionChecker MentionPermissionChecker

	// closeOnce ensures Shutdown is idempotent (safe to call multiple times)
	closeOnce sync.Once
}

// NewHub creates a new Hub
func NewHub(db *sql.DB, redisClient *redis.Client) *Hub {
	return &Hub{
		db:                   db,
		redis:                redisClient,
		clients:              make(map[uuid.UUID]*Client),
		userClients:          make(map[uuid.UUID]map[uuid.UUID]bool),
		channelSubscriptions: make(map[uuid.UUID]map[uuid.UUID]bool),
		usernames:            make(map[uuid.UUID]string),
		serverSubscriptions:  make(map[uuid.UUID]map[uuid.UUID]bool),
		dmSubscriptions:      make(map[uuid.UUID]map[uuid.UUID]bool),
		register:             make(chan *Client),
		unregister:           make(chan *Client),
		incoming:             make(chan IncomingMessage, 256),
		broadcast:            make(chan BroadcastMessage, 256),
		globalBroadcast:      make(chan OutgoingMessage, 256),
		userBroadcast:        make(chan UserBroadcastMessage, 256),
		serverBroadcast:      make(chan ServerBroadcastMessage, 256),
		dmBroadcast:          make(chan DMBroadcastMessage, 256),
		disconnectUser:       make(chan uuid.UUID, 16),
		disconnectSession:    make(chan string, 16),
		voiceCountSignal:     make(chan struct{}, 1),
		done:                 make(chan struct{}),
		stopped:              make(chan struct{}),
		onlineCountPending:   make(map[uuid.UUID]bool),
	}
}

// SetMentionChecker injects the RBAC permission checker for mention enforcement.
// Called after Hub and Resolver are both constructed (breaks circular init dependency).
func (h *Hub) SetMentionChecker(checker MentionPermissionChecker) {
	h.mentionChecker = checker
}

// Run starts the hub's main loop
func (h *Hub) Run() {
	defer close(h.stopped)
	for {
		// nil channel is never selected; active only when a debounce timer is pending
		var onlineCountC <-chan time.Time
		if h.onlineCountTimer != nil {
			onlineCountC = h.onlineCountTimer.C
		}

		select {
		case client := <-h.register:
			h.handleRegister(client)

		case client := <-h.unregister:
			h.handleUnregister(client)

		case message := <-h.incoming:
			h.handleIncoming(message)

		case message := <-h.broadcast:
			h.handleBroadcast(message)

		case message := <-h.globalBroadcast:
			h.handleGlobalBroadcast(message)

		case message := <-h.userBroadcast:
			h.handleUserBroadcast(message)

		case message := <-h.serverBroadcast:
			h.handleServerBroadcast(message)

		case message := <-h.dmBroadcast:
			h.handleDMBroadcast(message)

		case userID := <-h.disconnectUser:
			h.handleDisconnectUser(userID)

		case sessionID := <-h.disconnectSession:
			h.handleDisconnectSession(sessionID)

		case <-onlineCountC:
			h.flushOnlineCounts()

		case <-h.voiceCountSignal:
			h.broadcastServerVoiceCounts()

		case <-h.done:
			// Graceful shutdown: close all client connections
			for _, client := range h.clients {
				close(client.Send)
			}
			log.Printf("Hub shut down, closed %d client connections", len(h.clients))
			return
		}
	}
}

// Shutdown gracefully stops the hub's Run loop, waits for it to exit, and
// closes all client connections. Safe to call multiple times.
func (h *Hub) Shutdown() {
	h.closeOnce.Do(func() {
		close(h.done)
	})
	<-h.stopped
}

// Stopped returns a channel that is closed when the Run loop has exited.
func (h *Hub) Stopped() <-chan struct{} {
	return h.stopped
}

// handleRegister registers a new client
func (h *Hub) handleRegister(client *Client) {
	isFirstConnection := h.registerClient(client)

	if isFirstConnection {
		ctx := context.Background()
		h.redis.Set(ctx, fmt.Sprintf(presenceKeyFmt, client.UserID), statusOnline, 120*time.Second)
		h.broadcastPresenceToAll(client.UserID, statusOnline, time.Now().Unix())
	}

	log.Printf("Client registered: user=%s client=%s total_clients=%d",
		client.UserID, client.ID, len(h.clients))

	h.sendConnectedConfirmation(client)
	h.sendPresenceSnapshot(client)
	go h.sendVoiceCountsSnapshot(client)
}

func (h *Hub) registerClient(client *Client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.clients[client.ID] = client

	if _, ok := h.userClients[client.UserID]; !ok {
		h.userClients[client.UserID] = make(map[uuid.UUID]bool)
	}
	h.userClients[client.UserID][client.ID] = true

	if _, cached := h.usernames[client.UserID]; !cached {
		var username string
		err := h.db.QueryRow("SELECT username FROM users WHERE id = $1", client.UserID).Scan(&username)
		if err == nil {
			h.usernames[client.UserID] = username
		}
	}

	return len(h.userClients[client.UserID]) == 1
}

func (h *Hub) sendConnectedConfirmation(client *Client) {
	confirmMsg := OutgoingMessage{
		Type: "connected",
		Data: map[string]interface{}{
			"client_id": client.ID,
			keyUserID:   client.UserID,
		},
	}
	if data, err := json.Marshal(confirmMsg); err == nil {
		client.Send <- data
	}
}

type userPresenceInfo struct {
	UserID string `json:"user_id"`
	Status string `json:"status"`
}

func (h *Hub) resolveVisibleStatus(uid, viewerID uuid.UUID, ctx context.Context) string {
	status := statusOnline
	if val, err := h.redis.Get(ctx, fmt.Sprintf(presenceKeyFmt, uid)).Result(); err == nil {
		status = val
	}
	if status == statusInvisible && uid != viewerID {
		return statusOffline
	}
	return status
}

func (h *Hub) sendPresenceSnapshot(client *Client) {
	ctx := context.Background()
	users := make([]userPresenceInfo, 0, len(h.userClients))
	onlineUserIDs := make([]string, 0, len(h.userClients))
	for uid := range h.userClients {
		uidStr := uid.String()
		visibleStatus := h.resolveVisibleStatus(uid, client.UserID, ctx)
		if visibleStatus != statusOffline {
			onlineUserIDs = append(onlineUserIDs, uidStr)
		}
		users = append(users, userPresenceInfo{UserID: uidStr, Status: visibleStatus})
	}
	presenceSnapshot := OutgoingMessage{
		Type: "presence_snapshot",
		Data: map[string]interface{}{
			"online_user_ids": onlineUserIDs,
			"users":           users,
		},
	}
	if data, err := json.Marshal(presenceSnapshot); err == nil {
		client.Send <- data
	}
}

func (h *Hub) sendVoiceCountsSnapshot(client *Client) {
	rows, err := h.db.Query(`
		SELECT c.server_id, COUNT(vp.id)
		FROM voice_participants vp
		JOIN channels c ON c.id = vp.channel_id
		GROUP BY c.server_id
	`)
	if err != nil {
		log.Printf("Failed to query voice counts for new client: %v", err)
		return
	}
	defer func() { _ = rows.Close() }()

	counts := make(map[string]int)
	for rows.Next() {
		var serverID string
		var count int
		if err := rows.Scan(&serverID, &count); err == nil {
			counts[serverID] = count
		}
	}

	msg := OutgoingMessage{
		Type: "server_voice_counts",
		Data: map[string]interface{}{
			keyCounts: counts,
		},
	}
	if data, err := json.Marshal(msg); err == nil {
		select {
		case client.Send <- data:
		default:
		}
	}
}

// handleUnregister unregisters a client
func (h *Hub) handleUnregister(client *Client) {
	h.mu.Lock()
	_, exists := h.clients[client.ID]
	if !exists {
		h.mu.Unlock()
		return
	}

	delete(h.clients, client.ID)
	isLastConnection := h.removeUserClient(client)
	h.removeClientSubscriptions(client)
	remaining := len(h.clients)
	h.mu.Unlock()

	close(client.Send)

	if isLastConnection {
		now := time.Now().Unix()
		ctx := context.Background()
		h.redis.Del(ctx, fmt.Sprintf(presenceKeyFmt, client.UserID))
		h.redis.Set(ctx, fmt.Sprintf("last_seen:%s", client.UserID), fmt.Sprintf("%d", now), 0)
		h.broadcastPresenceToAll(client.UserID, statusOffline, now)
	}

	log.Printf("Client unregistered: user=%s client=%s remaining=%d",
		client.UserID, client.ID, remaining)
}

func (h *Hub) removeUserClient(client *Client) bool {
	userClients, ok := h.userClients[client.UserID]
	if !ok {
		return false
	}
	delete(userClients, client.ID)
	if len(userClients) == 0 {
		delete(h.userClients, client.UserID)
		delete(h.usernames, client.UserID)
		return true
	}
	return false
}

func (h *Hub) removeClientSubscriptions(client *Client) {
	for channelID := range client.Channels {
		h.removeFromSubscriptionMap(h.channelSubscriptions, channelID, client.ID)
	}
	for serverID := range h.serverSubscriptions {
		h.removeFromSubscriptionMap(h.serverSubscriptions, serverID, client.ID)
	}
	for convID := range h.dmSubscriptions {
		h.removeFromSubscriptionMap(h.dmSubscriptions, convID, client.ID)
	}
}

func (h *Hub) removeFromSubscriptionMap(m map[uuid.UUID]map[uuid.UUID]bool, key, clientID uuid.UUID) {
	clients, ok := m[key]
	if !ok {
		return
	}
	delete(clients, clientID)
	if len(clients) == 0 {
		delete(m, key)
	}
}

// handleIncoming processes incoming messages from clients
func (h *Hub) handleIncoming(msg IncomingMessage) {
	switch msg.Type {
	case "subscribe":
		h.handleSubscribe(msg)
	case "unsubscribe":
		h.handleUnsubscribe(msg)
	case "subscribe_server":
		h.handleSubscribeServer(msg)
	case "unsubscribe_server":
		h.handleUnsubscribeServer(msg)
	case "message":
		h.handleMessage(msg)
	case "typing":
		h.handleTyping(msg)
	case "profile_update":
		h.handleProfileUpdate(msg)
	case "heartbeat":
		h.handleHeartbeat(msg)
	case "set_status":
		h.handleSetStatus(msg)
	case "server_update":
		h.handleServerUpdate(msg)
	case "subscribe_dm":
		h.handleSubscribeDM(msg)
	case "unsubscribe_dm":
		h.handleUnsubscribeDM(msg)
	case "dm_message":
		h.handleDMMessage(msg)
	case "dm_typing":
		h.handleDMTyping(msg)
	default:
		log.Printf("Unknown message type: %s", msg.Type)
	}
}

// handleSubscribe subscribes a client to a channel after verifying membership
func (h *Hub) handleSubscribe(msg IncomingMessage) {
	channelID, ok := msg.Data[keyChannelID].(string)
	if !ok {
		h.sendError(msg.ClientID, "Invalid channel_id in subscribe message")
		return
	}

	channelUUID, err := uuid.Parse(channelID)
	if err != nil {
		h.sendError(msg.ClientID, "Invalid channel UUID")
		return
	}

	client, ok := h.clients[msg.ClientID]
	if !ok {
		return
	}

	// Verify the user is a member of the channel's server
	var isMember bool
	err = h.db.QueryRow(
		`SELECT EXISTS(
			SELECT 1 FROM channels c
			INNER JOIN server_members sm ON c.server_id = sm.server_id
			WHERE c.id = $1 AND sm.user_id = $2
		)`,
		channelUUID, client.UserID,
	).Scan(&isMember)
	if err != nil {
		log.Printf("Failed to check channel membership: %v", err)
		h.sendError(msg.ClientID, "Failed to verify channel access")
		return
	}
	if !isMember {
		h.sendError(msg.ClientID, "Not authorized to subscribe to this channel")
		return
	}

	// Add to client's channels
	if client.Channels == nil {
		client.Channels = make(map[uuid.UUID]bool)
	}
	client.Channels[channelUUID] = true

	// Add to channel subscriptions
	if _, ok := h.channelSubscriptions[channelUUID]; !ok {
		h.channelSubscriptions[channelUUID] = make(map[uuid.UUID]bool)
	}
	h.channelSubscriptions[channelUUID][client.ID] = true

	log.Printf("Client %s subscribed to channel %s", client.ID, channelUUID)

	// Send confirmation
	confirmMsg := OutgoingMessage{
		Type: "subscribed",
		Data: map[string]interface{}{
			keyChannelID: channelUUID,
		},
	}
	if data, err := json.Marshal(confirmMsg); err == nil {
		client.Send <- data
	}
}

// handleUnsubscribe unsubscribes a client from a channel
func (h *Hub) handleUnsubscribe(msg IncomingMessage) {
	channelID, ok := msg.Data[keyChannelID].(string)
	if !ok {
		return
	}

	channelUUID, err := uuid.Parse(channelID)
	if err != nil {
		return
	}

	client, ok := h.clients[msg.ClientID]
	if !ok {
		return
	}

	// Remove from client's channels
	delete(client.Channels, channelUUID)

	// Remove from channel subscriptions
	if clients, ok := h.channelSubscriptions[channelUUID]; ok {
		delete(clients, client.ID)
		if len(clients) == 0 {
			delete(h.channelSubscriptions, channelUUID)
		}
	}

	log.Printf("Client %s unsubscribed from channel %s", client.ID, channelUUID)
}

// handleSubscribeServer subscribes a client to server-level notifications (unread pings)
func (h *Hub) handleSubscribeServer(msg IncomingMessage) {
	serverID, ok := msg.Data[keyServerID].(string)
	if !ok {
		h.sendError(msg.ClientID, "Invalid server_id in subscribe_server message")
		return
	}

	serverUUID, err := uuid.Parse(serverID)
	if err != nil {
		h.sendError(msg.ClientID, "Invalid server UUID")
		return
	}

	client, ok := h.clients[msg.ClientID]
	if !ok {
		return
	}

	// Verify the user is a member of this server
	var isMember bool
	err = h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverUUID, client.UserID,
	).Scan(&isMember)
	if err != nil {
		log.Printf("Failed to check server membership: %v", err)
		h.sendError(msg.ClientID, "Failed to verify server access")
		return
	}
	if !isMember {
		h.sendError(msg.ClientID, "Not a member of this server")
		return
	}

	// Add to server subscriptions
	if _, ok := h.serverSubscriptions[serverUUID]; !ok {
		h.serverSubscriptions[serverUUID] = make(map[uuid.UUID]bool)
	}
	h.serverSubscriptions[serverUUID][client.ID] = true

	log.Printf("Client %s subscribed to server %s notifications", client.ID, serverUUID)
}

// handleUnsubscribeServer unsubscribes a client from server-level notifications
func (h *Hub) handleUnsubscribeServer(msg IncomingMessage) {
	serverID, ok := msg.Data[keyServerID].(string)
	if !ok {
		return
	}

	serverUUID, err := uuid.Parse(serverID)
	if err != nil {
		return
	}

	client, ok := h.clients[msg.ClientID]
	if !ok {
		return
	}

	if clients, ok := h.serverSubscriptions[serverUUID]; ok {
		delete(clients, client.ID)
		if len(clients) == 0 {
			delete(h.serverSubscriptions, serverUUID)
		}
	}

	log.Printf("Client %s unsubscribed from server %s notifications", client.ID, serverUUID)
}

// sendError sends an error message to a specific client
func (h *Hub) sendError(clientID uuid.UUID, message string) {
	client, ok := h.clients[clientID]
	if !ok {
		return
	}
	errMsg := OutgoingMessage{
		Type: "error",
		Data: map[string]interface{}{
			keyMessage: message,
		},
	}
	if data, err := json.Marshal(errMsg); err == nil {
		select {
		case client.Send <- data:
		default:
		}
	}
}

// sendErrorWithData sends an error message with additional structured data to a specific client.
func (h *Hub) sendErrorWithData(clientID uuid.UUID, errorCode string, extra map[string]interface{}) {
	client, ok := h.clients[clientID]
	if !ok {
		return
	}
	data := map[string]interface{}{
		keyMessage: errorCode,
		"error":    errorCode,
		"code":     errorCode,
	}
	for k, v := range extra {
		data[k] = v
	}
	errMsg := OutgoingMessage{
		Type: "error",
		Data: data,
	}
	if raw, err := json.Marshal(errMsg); err == nil {
		select {
		case client.Send <- raw:
		default:
		}
	}
}

// messageInput holds the parsed and validated fields from an incoming WebSocket message.
type messageInput struct {
	content         string
	isEncrypted     bool
	keyVersion      int
	mentionAddendum *MentionAddendum
	attachmentIDs   []string
}

// parseMessageInput extracts and validates content, encryption, key version, and mention
// addendum from an incoming WebSocket message. Returns nil on validation failure.
func (h *Hub) parseMessageInput(msg IncomingMessage) *messageInput {
	content, ok := msg.Data[keyContent].(string)
	if !ok || content == "" {
		h.sendError(msg.ClientID, "Message content is required")
		return nil
	}

	isEncrypted, _ := msg.Data[keyIsEncrypted].(bool)

	keyVersion := 1
	if kv, ok := msg.Data[keyKeyVersion].(float64); ok && kv > 0 {
		keyVersion = int(kv)
	}

	maxContentLen := 4000
	if isEncrypted {
		maxContentLen = 24000
	}
	if len(content) > maxContentLen {
		h.sendError(msg.ClientID, "Message content exceeds maximum length")
		return nil
	}

	var mentionAddendum *MentionAddendum
	if metaStr, ok := msg.Data["mention_meta"].(string); ok {
		mentionAddendum = decodeMentionMeta(metaStr)
	}

	attachmentIDs, attachOK := h.parseAttachmentIDs(msg)
	if !attachOK {
		return nil
	}

	return &messageInput{
		content:         content,
		isEncrypted:     isEncrypted,
		keyVersion:      keyVersion,
		mentionAddendum: mentionAddendum,
		attachmentIDs:   attachmentIDs,
	}
}

// parseAttachmentIDs extracts and validates attachment_ids from an incoming message.
// Returns the parsed IDs and true on success; sends an error and returns false on failure.
func (h *Hub) parseAttachmentIDs(msg IncomingMessage) ([]string, bool) {
	raw, exists := msg.Data["attachment_ids"]
	if !exists {
		return nil, true // key absent — no attachments, valid
	}
	rawIDs, ok := raw.([]interface{})
	if !ok {
		h.sendError(msg.ClientID, "attachment_ids must be an array")
		return nil, false
	}
	if len(rawIDs) > 5 {
		h.sendError(msg.ClientID, "Maximum 5 attachments per message")
		return nil, false
	}
	seen := make(map[string]bool, len(rawIDs))
	ids := make([]string, 0, len(rawIDs))
	for _, item := range rawIDs {
		idStr, ok := item.(string)
		if !ok {
			h.sendError(msg.ClientID, "Invalid attachment ID format")
			return nil, false
		}
		if _, err := uuid.Parse(idStr); err != nil {
			h.sendError(msg.ClientID, "Invalid attachment ID")
			return nil, false
		}
		if seen[idStr] {
			continue // deduplicate
		}
		seen[idStr] = true
		ids = append(ids, idStr)
	}
	return ids, true
}

// channelContext holds server-side channel state needed for message persistence.
type channelContext struct {
	channelEncrypted  bool
	serverAllowEmbeds bool
	serverUUID        uuid.UUID
}

// fetchChannelContext queries encryption status, embed policy, and server ID for a channel.
// Returns nil on failure (error already sent to client).
func (h *Hub) fetchChannelContext(msg IncomingMessage, channelUUID uuid.UUID, isEncrypted bool) *channelContext {
	var ctx channelContext
	err := h.db.QueryRow(
		`SELECT c.is_encrypted, s.allow_embedded_content, c.server_id
		 FROM channels c INNER JOIN servers s ON c.server_id = s.id
		 WHERE c.id = $1`, channelUUID,
	).Scan(&ctx.channelEncrypted, &ctx.serverAllowEmbeds, &ctx.serverUUID)
	if err != nil {
		log.Printf("Failed to check channel encryption/embed status: %v", err)
		h.sendError(msg.ClientID, "Failed to verify channel status")
		return nil
	}
	if ctx.channelEncrypted && !isEncrypted {
		h.sendError(msg.ClientID, "E2EE channel requires encrypted messages")
		return nil
	}
	return &ctx
}

// enforceWSEpoch checks that the key epoch is not revoked. Returns false if revoked or on error.
func (h *Hub) enforceWSEpoch(msg IncomingMessage, channelUUID uuid.UUID, channelID string, isEncrypted bool, keyVersion int) bool {
	if !isEncrypted || keyVersion <= 0 {
		return true
	}
	var epochRevoked bool
	if err := h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM key_revocations WHERE channel_id = $1 AND revoked_epoch = $2)`,
		channelUUID, keyVersion,
	).Scan(&epochRevoked); err != nil {
		log.Printf("Failed to check epoch revocation: %v", err)
		h.sendError(msg.ClientID, "Failed to verify key epoch")
		return false
	}
	if epochRevoked {
		currentEpoch := 1
		if err := h.db.QueryRow(
			`SELECT COALESCE(MAX(key_version), 1) FROM channel_keys WHERE channel_id = $1`,
			channelUUID,
		).Scan(&currentEpoch); err != nil {
			log.Printf("Failed to fetch current epoch for channel %s: %v", channelID, err)
		}
		h.sendErrorWithData(msg.ClientID, "epoch_revoked", map[string]interface{}{
			"current_epoch": currentEpoch,
			keyChannelID:    channelID,
		})
		return false
	}
	return true
}

// messageAck holds the fields for a message_ack response.
type messageAck struct {
	Client      *Client
	Nonce       string
	MessageID   uuid.UUID
	ChannelUUID uuid.UUID
	CreatedAt   interface{}
	UpdatedAt   interface{}
	ReplyToID   *string
	Attachments []models.AttachmentSummary
}

// sendMessageAck sends a message_ack to the sender with the server-assigned UUID.
func (h *Hub) sendMessageAck(ack messageAck) {
	ackData := map[string]interface{}{
		keyNonce:     ack.Nonce,
		"id":         ack.MessageID.String(),
		keyChannelID: ack.ChannelUUID.String(),
		keyCreatedAt: ack.CreatedAt,
		keyUpdatedAt: ack.UpdatedAt,
	}
	if ack.ReplyToID != nil {
		ackData["reply_to_id"] = *ack.ReplyToID
	}
	if len(ack.Attachments) > 0 {
		ackData["attachments"] = ack.Attachments
	}
	ackMsg := OutgoingMessage{Type: "message_ack", Data: ackData}
	if data, err := json.Marshal(ackMsg); err == nil {
		select {
		case ack.Client.Send <- data:
		default:
		}
	}
}

// linkChannelAttachments validates and links attachment file_ids to a channel message.
func (h *Hub) linkChannelAttachments(messageID uuid.UUID, userID string, attachmentIDs []string, channelID string) []models.AttachmentSummary {
	return h.linkAttachmentsToTable(attachmentLinkCtx{
		messageID: messageID, userID: userID, channelID: channelID,
		insertSQL: `INSERT INTO message_attachments (message_id, file_id, position) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
	}, attachmentIDs)
}

// linkDMAttachments validates and links attachment file_ids to a DM message.
func (h *Hub) linkDMAttachments(messageID uuid.UUID, userID string, attachmentIDs []string, conversationID string) []models.AttachmentSummary {
	return h.linkAttachmentsToTable(attachmentLinkCtx{
		messageID: messageID, userID: userID, conversationID: conversationID,
		insertSQL: `INSERT INTO dm_message_attachments (message_id, file_id, position) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
	}, attachmentIDs)
}

// attachmentLinkCtx holds the context for linking attachments to a message.
type attachmentLinkCtx struct {
	messageID      uuid.UUID
	userID         string
	channelID      string
	conversationID string
	insertSQL      string
}

// linkAttachmentsToTable validates and links attachment file_ids using the provided INSERT query.
func (h *Hub) linkAttachmentsToTable(ctx attachmentLinkCtx, attachmentIDs []string) []models.AttachmentSummary {
	if len(attachmentIDs) == 0 {
		return nil
	}

	var summaries []models.AttachmentSummary
	for i, fileID := range attachmentIDs {
		summary, ok := h.validateAndLinkAttachment(ctx, fileID, i)
		if ok {
			summaries = append(summaries, summary)
		}
	}
	return summaries
}

// validateAndLinkAttachment validates a single attachment and links it to a message.
func (h *Hub) validateAndLinkAttachment(ctx attachmentLinkCtx, fileID string, position int) (models.AttachmentSummary, bool) {
	var summary models.AttachmentSummary
	var uploaderID string
	var fileChannelID, fileConvID *string

	err := h.db.QueryRow(
		`SELECT uploader_id, file_type, mime_type, file_size, channel_id, conversation_id
		 FROM media_files WHERE id = $1 AND deleted_at IS NULL`,
		fileID,
	).Scan(&uploaderID, &summary.FileType, &summary.MimeType, &summary.FileSize, &fileChannelID, &fileConvID)
	if err != nil {
		log.Printf("Attachment %s not found or deleted: %v", fileID, err)
		return summary, false
	}

	if !h.verifyAttachmentAccess(fileID, uploaderID, ctx, fileChannelID, fileConvID) {
		return summary, false
	}

	_, err = h.db.Exec(ctx.insertSQL, ctx.messageID, fileID, position)
	if err != nil {
		log.Printf("Failed to link attachment %s to message %s: %v", fileID, ctx.messageID, err)
		return summary, false
	}

	summary.ID = fileID
	return summary, true
}

// verifyAttachmentAccess checks that the file is owned by the sender and belongs to the correct channel/conversation.
func (h *Hub) verifyAttachmentAccess(fileID, uploaderID string, ctx attachmentLinkCtx, fileChannelID, fileConvID *string) bool {
	if uploaderID != ctx.userID {
		log.Printf("Attachment %s not owned by user %s", fileID, ctx.userID)
		return false
	}
	if ctx.channelID != "" && (fileChannelID == nil || *fileChannelID != ctx.channelID) {
		log.Printf("Attachment %s does not belong to channel %s", fileID, ctx.channelID)
		return false
	}
	if ctx.conversationID != "" && (fileConvID == nil || *fileConvID != ctx.conversationID) {
		log.Printf("Attachment %s does not belong to conversation %s", fileID, ctx.conversationID)
		return false
	}
	return true
}

// persistMessage inserts a message into the database, returning the generated ID and timestamps.
// Returns a specific error message string if the insert fails (empty string on success).
func (h *Hub) persistMessage(
	channelUUID uuid.UUID, userID uuid.UUID, content string,
	isEncrypted bool, keyVersion int, embedsSuppressed bool, replyToID *string,
) (uuid.UUID, time.Time, time.Time, string) {
	messageID := uuid.New()
	var createdAt, updatedAt time.Time
	err := h.db.QueryRow(
		`INSERT INTO messages (id, channel_id, user_id, content, is_encrypted, key_version, embeds_suppressed, reply_to_id, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW(), NOW())
		 RETURNING created_at, updated_at`,
		messageID, channelUUID, userID, content, isEncrypted, keyVersion, embedsSuppressed, replyToID,
	).Scan(&createdAt, &updatedAt)
	if err != nil && isFKViolation(err) {
		return messageID, createdAt, updatedAt, "Reply target message not found"
	}
	if err != nil {
		log.Printf("Failed to persist message: %v", err)
		return messageID, createdAt, updatedAt, "Failed to save message"
	}
	return messageID, createdAt, updatedAt, ""
}

// isFKViolation returns true if the error is a PostgreSQL foreign key violation (23503).
func isFKViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23503"
}

// validateReplyToID checks that reply_to_id is a valid UUID referencing a message
// in the given channel. Returns the validated ID (or nil) and whether to continue.
func (h *Hub) validateReplyToID(msg IncomingMessage, channelID string) (*string, bool) {
	rtID, ok := msg.Data["reply_to_id"].(string)
	if !ok || rtID == "" {
		return nil, true
	}
	if _, parseErr := uuid.Parse(rtID); parseErr != nil {
		h.sendError(msg.ClientID, "Invalid reply_to_id")
		return nil, false
	}
	var replyChannelUUID uuid.UUID
	err := h.db.QueryRow(`SELECT channel_id FROM messages WHERE id = $1`, rtID).Scan(&replyChannelUUID)
	if err == sql.ErrNoRows {
		h.sendError(msg.ClientID, "Reply target message not found")
		return nil, false
	}
	if err != nil {
		log.Printf("Failed to validate reply_to_id %s: %v", rtID, err)
		h.sendError(msg.ClientID, "Failed to validate reply target")
		return nil, false
	}
	parsedChannelID, _ := uuid.Parse(channelID)
	if replyChannelUUID != parsedChannelID {
		h.sendError(msg.ClientID, "Reply target must be in the same channel")
		return nil, false
	}
	return &rtID, true
}

// fetchRepliedToSummary queries the replied-to message for broadcast enrichment.
func (h *Hub) fetchRepliedToSummary(replyToID string) map[string]interface{} {
	var id, userID, username, content string
	var displayName *string
	var isEncrypted bool
	var keyVer int
	err := h.db.QueryRow(`
		SELECT m.id, m.user_id, u.username, u.display_name, m.content, m.is_encrypted, COALESCE(m.key_version, 1)
		FROM messages m INNER JOIN users u ON m.user_id = u.id WHERE m.id = $1
	`, replyToID).Scan(&id, &userID, &username, &displayName, &content, &isEncrypted, &keyVer)
	if err != nil {
		return nil
	}
	return map[string]interface{}{
		"id":           id,
		"user_id":      userID,
		"username":     username,
		"display_name": displayName,
		"content":      content,
		"is_encrypted": isEncrypted,
		"key_version":  keyVer,
	}
}

// messageBroadcastCtx holds the data needed to build a channel message broadcast payload.
type messageBroadcastCtx struct {
	messageID        uuid.UUID
	channelUUID      uuid.UUID
	userID           uuid.UUID
	client           *Client
	input            *messageInput
	embedsSuppressed bool
	createdAt        interface{}
	updatedAt        interface{}
	replyToID        *string
	attachments      []models.AttachmentSummary
}

// buildMessageBroadcast constructs the broadcast data map for a channel message.
func (h *Hub) buildMessageBroadcast(ctx messageBroadcastCtx) map[string]interface{} {
	data := map[string]interface{}{
		"id":                ctx.messageID,
		keyChannelID:        ctx.channelUUID,
		keyUserID:           ctx.userID,
		keyUsername:         ctx.client.Username,
		"display_name":      ctx.client.DisplayName,
		"avatar_url":        ctx.client.AvatarURL,
		keyContent:          ctx.input.content,
		keyIsEncrypted:      ctx.input.isEncrypted,
		keyKeyVersion:       ctx.input.keyVersion,
		"embeds_suppressed": ctx.embedsSuppressed,
		keyCreatedAt:        ctx.createdAt,
		keyUpdatedAt:        ctx.updatedAt,
	}
	if ctx.replyToID != nil {
		data["reply_to_id"] = *ctx.replyToID
		if summary := h.fetchRepliedToSummary(*ctx.replyToID); summary != nil {
			data["replied_to"] = summary
		}
	}
	if len(ctx.attachments) > 0 {
		data["attachments"] = ctx.attachments
	}
	return data
}

// handleMessage handles a chat message: validates, persists to DB, then broadcasts
func (h *Hub) handleMessage(msg IncomingMessage) {
	channelID, ok := msg.Data[keyChannelID].(string)
	if !ok {
		return
	}
	channelUUID, err := uuid.Parse(channelID)
	if err != nil {
		return
	}

	// Verify subscription + rate limit
	client, ok := h.clients[msg.ClientID]
	if !ok {
		return
	}
	if !client.Channels[channelUUID] {
		h.sendError(msg.ClientID, "Not subscribed to this channel")
		return
	}
	if !client.rateLimitAllow() {
		h.sendError(msg.ClientID, "Rate limit exceeded, please slow down")
		return
	}

	// Parse and validate input fields
	input := h.parseMessageInput(msg)
	if input == nil {
		return
	}
	if input.mentionAddendum != nil {
		defer input.mentionAddendum.Wipe()
	}

	// Encryption enforcement + embed policy
	chCtx := h.fetchChannelContext(msg, channelUUID, input.isEncrypted)
	if chCtx == nil {
		return
	}
	if !h.enforceWSEpoch(msg, channelUUID, channelID, input.isEncrypted, input.keyVersion) {
		return
	}
	embedsSuppressed := !chCtx.serverAllowEmbeds

	// Validate optional reply_to_id (same-channel constraint)
	replyToID, replyOK := h.validateReplyToID(msg, channelID)
	if !replyOK {
		return
	}

	// Persist message
	messageID, createdAt, updatedAt, persistErr := h.persistMessage(
		channelUUID, msg.UserID, input.content, input.isEncrypted, input.keyVersion, embedsSuppressed, replyToID,
	)
	if persistErr != "" {
		h.sendError(msg.ClientID, persistErr)
		return
	}

	// Link attachments to message (if any)
	attachmentSummaries := h.linkChannelAttachments(messageID, msg.UserID.String(), input.attachmentIDs, channelID)

	// Ack to sender
	nonce, _ := msg.Data[keyNonce].(string)
	h.sendMessageAck(messageAck{
		Client:      client,
		Nonce:       nonce,
		MessageID:   messageID,
		ChannelUUID: channelUUID,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
		ReplyToID:   replyToID,
		Attachments: attachmentSummaries,
	})

	// Broadcast to channel subscribers (excluding sender)
	broadcastData := h.buildMessageBroadcast(messageBroadcastCtx{
		messageID: messageID, channelUUID: channelUUID, userID: msg.UserID,
		client: client, input: input, embedsSuppressed: embedsSuppressed,
		createdAt: createdAt, updatedAt: updatedAt,
		replyToID: replyToID, attachments: attachmentSummaries,
	})
	h.broadcast <- BroadcastMessage{
		ChannelID:   channelUUID,
		ExcludeUser: &msg.UserID,
		Data:        OutgoingMessage{Type: "message", Data: broadcastData},
	}

	// Send lightweight unread_notify to server subscribers NOT subscribed to this channel.
	h.sendUnreadNotify(chCtx.serverUUID, channelUUID, msg.UserID)

	// Mention routing: enforce RBAC, resolve targets, send enhanced notifications, wipe.
	if input.mentionAddendum != nil {
		h.enforceMentionPermissions(
			chCtx.serverUUID.String(), msg.UserID.String(), channelUUID.String(),
			input.mentionAddendum,
		)
		h.routeMentionNotifications(chCtx.serverUUID, channelUUID, msg.UserID, input.mentionAddendum)
	}
}

// sendUnreadNotify sends a lightweight notification to server subscribers
// who are not subscribed to the given channel (they already get the full message).
func (h *Hub) sendUnreadNotify(serverID, channelID, senderUserID uuid.UUID) {
	serverClients, ok := h.serverSubscriptions[serverID]
	if !ok {
		return
	}

	channelClients := h.channelSubscriptions[channelID] // may be nil, that's ok

	notifyMsg := OutgoingMessage{
		Type: "unread_notify",
		Data: map[string]interface{}{
			keyChannelID: channelID.String(),
			keyServerID:  serverID.String(),
		},
	}
	data, err := json.Marshal(notifyMsg)
	if err != nil {
		return
	}

	for clientID := range serverClients {
		// Skip clients already subscribed to the channel — they got the full message
		if channelClients != nil && channelClients[clientID] {
			continue
		}

		client, ok := h.clients[clientID]
		if !ok {
			continue
		}

		// Skip the sender
		if client.UserID == senderUserID {
			continue
		}

		select {
		case client.Send <- data:
		default:
		}
	}
}

// handleTyping handles typing indicator
func (h *Hub) handleTyping(msg IncomingMessage) {
	channelID, ok := msg.Data[keyChannelID].(string)
	if !ok {
		return
	}

	channelUUID, err := uuid.Parse(channelID)
	if err != nil {
		return
	}

	// Verify the client is subscribed to this channel
	client, ok := h.clients[msg.ClientID]
	if !ok {
		return
	}
	if !client.Channels[channelUUID] {
		return
	}

	// Broadcast typing indicator to all subscribers except sender
	broadcastMsg := BroadcastMessage{
		ChannelID:   channelUUID,
		ExcludeUser: &msg.UserID,
		Data: OutgoingMessage{
			Type: "typing",
			Data: map[string]interface{}{
				keyChannelID: channelUUID,
				keyUserID:    msg.UserID,
				keyUsername:  client.Username,
				keyIsTyping:  msg.Data[keyIsTyping],
			},
		},
	}

	h.broadcast <- broadcastMsg
}

// handleHeartbeat refreshes the Redis presence TTL for the user
func (h *Hub) handleHeartbeat(msg IncomingMessage) {
	ctx := context.Background()
	key := fmt.Sprintf(presenceKeyFmt, msg.UserID)
	// Refresh TTL for any user-set status (don't change the value, just extend the TTL)
	val, err := h.redis.Get(ctx, key).Result()
	switch err {
	case nil:
		// Extend TTL for any valid status
		switch val {
		case statusOnline, "dnd", statusInvisible:
			h.redis.Expire(ctx, key, 120*time.Second)
		}
	case redis.Nil:
		// Key expired or missing — re-set to online if user is still connected
		if _, ok := h.userClients[msg.UserID]; ok {
			h.redis.Set(ctx, key, statusOnline, 120*time.Second)
		}
	}
}

// handleSetStatus allows users to manually set their status (online/dnd/invisible)
func (h *Hub) handleSetStatus(msg IncomingMessage) {
	status, ok := msg.Data[keyStatus].(string)
	if !ok {
		return
	}

	// Validate status
	switch status {
	case statusOnline, "dnd", statusInvisible:
		// valid
	default:
		return
	}

	ctx := context.Background()
	key := fmt.Sprintf(presenceKeyFmt, msg.UserID)
	h.redis.Set(ctx, key, status, 120*time.Second)

	// For invisible, broadcast as offline to other users (but store real status in Redis)
	broadcastStatus := status
	if status == statusInvisible {
		broadcastStatus = statusOffline
	}
	h.broadcastPresenceToAll(msg.UserID, broadcastStatus, time.Now().Unix())
}

// handleBroadcast broadcasts a message to all subscribers of a channel
func (h *Hub) handleBroadcast(msg BroadcastMessage) {
	subscribers, ok := h.channelSubscriptions[msg.ChannelID]
	if !ok {
		return
	}

	messageData, err := json.Marshal(msg.Data)
	if err != nil {
		log.Printf("Failed to marshal broadcast message: %v", err)
		return
	}

	for clientID := range subscribers {
		client, ok := h.clients[clientID]
		if !ok {
			continue
		}

		// Skip if this is the excluded user
		if msg.ExcludeUser != nil && client.UserID == *msg.ExcludeUser {
			continue
		}

		select {
		case client.Send <- messageData:
		default:
			// Client's send channel is full, perform full cleanup
			h.handleUnregister(client)
		}
	}
}

// BroadcastToChannel broadcasts a message to all subscribers of a channel
func (h *Hub) BroadcastToChannel(channelID uuid.UUID, message OutgoingMessage) {
	h.broadcast <- BroadcastMessage{
		ChannelID: channelID,
		Data:      message,
	}
}

// GetConnectedUsers returns a map of user IDs currently connected (thread-safe).
func (h *Hub) GetConnectedUsers() map[uuid.UUID]bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	users := make(map[uuid.UUID]bool, len(h.userClients))
	for userID := range h.userClients {
		users[userID] = true
	}
	return users
}

// GetUserClientCount returns the number of connected clients for a user (thread-safe).
func (h *Hub) GetUserClientCount(userID uuid.UUID) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if clients, ok := h.userClients[userID]; ok {
		return len(clients)
	}
	return 0
}

// handleGlobalBroadcast sends a message to all connected clients
func (h *Hub) handleGlobalBroadcast(msg OutgoingMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Failed to marshal global broadcast: %v", err)
		return
	}
	for _, client := range h.clients {
		select {
		case client.Send <- data:
		default:
		}
	}
}

// BroadcastToAll sends a message to all connected clients (thread-safe).
func (h *Hub) BroadcastToAll(msg OutgoingMessage) {
	h.globalBroadcast <- msg
}

// handleServerBroadcast sends a message to all clients subscribed to a server.
func (h *Hub) handleServerBroadcast(msg ServerBroadcastMessage) {
	subscribers, ok := h.serverSubscriptions[msg.ServerID]
	if !ok {
		return
	}

	data, err := json.Marshal(msg.Data)
	if err != nil {
		log.Printf("Failed to marshal server broadcast: %v", err)
		return
	}

	for clientID := range subscribers {
		client, ok := h.clients[clientID]
		if !ok {
			continue
		}
		select {
		case client.Send <- data:
		default:
		}
	}
}

// BroadcastToServer sends a message to all clients subscribed to a server (thread-safe).
func (h *Hub) BroadcastToServer(serverID uuid.UUID, msg OutgoingMessage) {
	h.serverBroadcast <- ServerBroadcastMessage{
		ServerID: serverID,
		Data:     msg,
	}
}

// handleUserBroadcast sends a message to all clients of a specific user.
func (h *Hub) handleUserBroadcast(msg UserBroadcastMessage) {
	clients, ok := h.userClients[msg.UserID]
	if !ok {
		return
	}

	data, err := json.Marshal(msg.Data)
	if err != nil {
		log.Printf("Failed to marshal user broadcast: %v", err)
		return
	}

	for clientID := range clients {
		if msg.ExcludeClientID != nil && clientID == *msg.ExcludeClientID {
			continue
		}
		client, ok := h.clients[clientID]
		if !ok {
			continue
		}
		select {
		case client.Send <- data:
		default:
		}
	}
}

// BroadcastToUser sends a message to all connected clients of a specific user (thread-safe).
func (h *Hub) BroadcastToUser(userID uuid.UUID, msg OutgoingMessage) {
	h.userBroadcast <- UserBroadcastMessage{
		UserID: userID,
		Data:   msg,
	}
}

// --- DM subscription and message handlers ---

// handleSubscribeDM subscribes a client to a DM conversation after verifying participation.
func (h *Hub) handleSubscribeDM(msg IncomingMessage) {
	convID, ok := msg.Data[keyConversationID].(string)
	if !ok {
		h.sendError(msg.ClientID, "Invalid conversation_id in subscribe_dm message")
		return
	}

	convUUID, err := uuid.Parse(convID)
	if err != nil {
		h.sendError(msg.ClientID, "Invalid conversation UUID")
		return
	}

	client, ok := h.clients[msg.ClientID]
	if !ok {
		return
	}

	// Verify the user is a participant of this DM conversation
	var isParticipant bool
	err = h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id = $1 AND user_id = $2)`,
		convUUID, client.UserID,
	).Scan(&isParticipant)
	if err != nil {
		log.Printf("Failed to check DM participation: %v", err)
		h.sendError(msg.ClientID, "Failed to verify DM access")
		return
	}
	if !isParticipant {
		h.sendError(msg.ClientID, "Not a participant of this conversation")
		return
	}

	// Add to DM subscriptions
	if _, ok := h.dmSubscriptions[convUUID]; !ok {
		h.dmSubscriptions[convUUID] = make(map[uuid.UUID]bool)
	}
	h.dmSubscriptions[convUUID][client.ID] = true

	log.Printf("Client %s subscribed to DM %s", client.ID, convUUID)

	// Send confirmation
	confirmMsg := OutgoingMessage{
		Type: "dm_subscribed",
		Data: map[string]interface{}{
			keyConversationID: convUUID,
		},
	}
	if data, err := json.Marshal(confirmMsg); err == nil {
		client.Send <- data
	}
}

// handleUnsubscribeDM unsubscribes a client from a DM conversation.
func (h *Hub) handleUnsubscribeDM(msg IncomingMessage) {
	convID, ok := msg.Data[keyConversationID].(string)
	if !ok {
		return
	}

	convUUID, err := uuid.Parse(convID)
	if err != nil {
		return
	}

	if clients, ok := h.dmSubscriptions[convUUID]; ok {
		delete(clients, msg.ClientID)
		if len(clients) == 0 {
			delete(h.dmSubscriptions, convUUID)
		}
	}

	log.Printf("Client %s unsubscribed from DM %s", msg.ClientID, convUUID)
}

// dmMessageInput holds the parsed and validated fields from an incoming DM WebSocket message.
type dmMessageInput struct {
	content         string
	isEncrypted     bool
	keyVersion      int
	msgType         string
	mentionAddendum *MentionAddendum
	attachmentIDs   []string
}

// dmUnreadLastMessage holds last-message metadata included in dm_unread_notify
// so that clients can update conversation previews and ordering in real-time.
type dmUnreadLastMessage struct {
	Content     string
	UserID      string
	Username    string
	CreatedAt   time.Time
	IsEncrypted bool
}

func (h *Hub) validateDMMessage(msg IncomingMessage) (*Client, uuid.UUID, *dmMessageInput) {
	convID, ok := msg.Data[keyConversationID].(string)
	if !ok {
		return nil, uuid.Nil, nil
	}
	convUUID, err := uuid.Parse(convID)
	if err != nil {
		return nil, uuid.Nil, nil
	}

	client, ok := h.clients[msg.ClientID]
	if !ok {
		return nil, uuid.Nil, nil
	}
	subscribers, hasSubs := h.dmSubscriptions[convUUID]
	if !hasSubs || !subscribers[client.ID] {
		h.sendError(msg.ClientID, "Not subscribed to this DM conversation")
		return nil, uuid.Nil, nil
	}
	if !client.rateLimitAllow() {
		h.sendError(msg.ClientID, "Rate limit exceeded, please slow down")
		return nil, uuid.Nil, nil
	}

	input, valid := h.parseDMMessageFields(msg)
	if !valid {
		return nil, uuid.Nil, nil
	}

	return client, convUUID, input
}

// parseDMMessageFields extracts and validates content, encryption, type,
// attachments, and mention metadata from a DM message payload.
func (h *Hub) parseDMMessageFields(msg IncomingMessage) (*dmMessageInput, bool) {
	content, ok := msg.Data[keyContent].(string)
	if !ok || content == "" {
		h.sendError(msg.ClientID, "Message content is required")
		return nil, false
	}

	isEncrypted, _ := msg.Data[keyIsEncrypted].(bool)
	msgType := "user"
	if t, ok := msg.Data["type"].(string); ok && t == "system" {
		msgType = "system"
	}

	keyVersion := 1
	if kv, ok := msg.Data[keyKeyVersion].(float64); ok && kv > 0 {
		keyVersion = int(kv)
	}

	if !h.validateContentLength(msg.ClientID, content, isEncrypted) {
		return nil, false
	}

	attachmentIDs, attachOK := h.parseAttachmentIDs(msg)
	if !attachOK {
		return nil, false
	}

	mentionAddendum := h.parseDMMentionMeta(msg)

	return &dmMessageInput{
		content:         content,
		isEncrypted:     isEncrypted,
		keyVersion:      keyVersion,
		msgType:         msgType,
		mentionAddendum: mentionAddendum,
		attachmentIDs:   attachmentIDs,
	}, true
}

// validateContentLength checks that the message content does not exceed the
// allowed length (4000 for plaintext, 24000 for encrypted).
func (h *Hub) validateContentLength(clientID uuid.UUID, content string, isEncrypted bool) bool {
	maxContentLen := 4000
	if isEncrypted {
		maxContentLen = 24000
	}
	if len(content) > maxContentLen {
		h.sendError(clientID, "Message content exceeds maximum length")
		return false
	}
	return true
}

// parseDMMentionMeta extracts and sanitizes mention metadata for DM messages,
// clearing server-only fields (Everyone, Roles).
func (h *Hub) parseDMMentionMeta(msg IncomingMessage) *MentionAddendum {
	metaStr, ok := msg.Data["mention_meta"].(string)
	if !ok {
		return nil
	}
	mentionAddendum := decodeMentionMeta(metaStr)
	if mentionAddendum != nil {
		mentionAddendum.Everyone = false
		mentionAddendum.Roles = nil
	}
	return mentionAddendum
}

func (h *Hub) enforceDMEncryption(msg IncomingMessage, convUUID uuid.UUID, isEncrypted bool, keyVersion int) (convPersonal bool, ok bool) {
	var convEncrypted bool
	encErr := h.db.QueryRow(`SELECT is_encrypted, is_personal FROM dm_conversations WHERE id = $1`, convUUID).Scan(&convEncrypted, &convPersonal)
	if encErr != nil {
		log.Printf("Failed to check DM encryption status: %v", encErr)
		h.sendError(msg.ClientID, "Failed to verify conversation encryption status")
		return false, false
	}
	if convEncrypted && !isEncrypted {
		h.sendError(msg.ClientID, "Encrypted conversation requires encrypted messages")
		return false, false
	}

	if !h.enforceDMEpoch(msg, convUUID, isEncrypted, keyVersion) {
		return false, false
	}
	return convPersonal, true
}

func (h *Hub) enforceDMEpoch(msg IncomingMessage, convUUID uuid.UUID, isEncrypted bool, keyVersion int) bool {
	if !isEncrypted || keyVersion <= 0 {
		return true
	}
	var epochRevoked bool
	if err := h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM dm_key_revocations WHERE conversation_id = $1 AND revoked_epoch = $2)`,
		convUUID, keyVersion,
	).Scan(&epochRevoked); err != nil {
		log.Printf("Failed to check DM epoch revocation: %v", err)
		h.sendError(msg.ClientID, "Failed to verify key epoch")
		return false
	}
	if !epochRevoked {
		return true
	}
	currentEpoch := 1
	if err := h.db.QueryRow(
		`SELECT COALESCE(MAX(key_version), 1) FROM dm_channel_keys WHERE conversation_id = $1`,
		convUUID,
	).Scan(&currentEpoch); err != nil {
		log.Printf("Failed to fetch current epoch for DM %s: %v", convUUID, err)
	}
	convID := convUUID.String()
	h.sendErrorWithData(msg.ClientID, "epoch_revoked", map[string]interface{}{
		"current_epoch":   currentEpoch,
		keyConversationID: convID,
		keyChannelID:      convID,
	})
	return false
}

func (h *Hub) persistDMMessage(convUUID uuid.UUID, userID uuid.UUID, input *dmMessageInput) (uuid.UUID, time.Time, time.Time, error) {
	messageID := uuid.New()
	var createdAt, updatedAt time.Time
	err := h.db.QueryRow(
		`INSERT INTO dm_messages (id, conversation_id, user_id, content, is_encrypted, key_version, type, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), NOW())
		 RETURNING created_at, updated_at`,
		messageID, convUUID, userID, input.content, input.isEncrypted, input.keyVersion, input.msgType,
	).Scan(&createdAt, &updatedAt)
	return messageID, createdAt, updatedAt, err
}

func (h *Hub) sendDMMessageAck(client *Client, nonce string, messageID uuid.UUID, convUUID uuid.UUID, createdAt, updatedAt time.Time, attachments []models.AttachmentSummary) {
	dmAckData := map[string]interface{}{
		keyNonce:          nonce,
		"id":              messageID.String(),
		"conversation_id": convUUID.String(),
		keyCreatedAt:      createdAt,
		keyUpdatedAt:      updatedAt,
	}
	if len(attachments) > 0 {
		dmAckData["attachments"] = attachments
	}
	ackMsg := OutgoingMessage{Type: "dm_message_ack", Data: dmAckData}
	if ackData, ackErr := json.Marshal(ackMsg); ackErr == nil {
		select {
		case client.Send <- ackData:
		default:
		}
	}
}

// dmBroadcastCtx holds the data needed to build a DM message broadcast payload.
type dmBroadcastCtx struct {
	messageID    uuid.UUID
	convUUID     uuid.UUID
	senderUserID uuid.UUID
	client       *Client
	input        *dmMessageInput
	createdAt    time.Time
	updatedAt    time.Time
	attachments  []models.AttachmentSummary
	convPersonal bool
}

func (h *Hub) broadcastDMMessage(ctx dmBroadcastCtx) {
	dmBroadcastData := map[string]interface{}{
		"id":              ctx.messageID,
		keyConversationID: ctx.convUUID,
		keyUserID:         ctx.senderUserID,
		keyUsername:       ctx.client.Username,
		"display_name":    ctx.client.DisplayName,
		"avatar_url":      ctx.client.AvatarURL,
		keyContent:        ctx.input.content,
		keyIsEncrypted:    ctx.input.isEncrypted,
		keyKeyVersion:     ctx.input.keyVersion,
		"type":            ctx.input.msgType,
		keyCreatedAt:      ctx.createdAt,
		keyUpdatedAt:      ctx.updatedAt,
	}
	if len(ctx.attachments) > 0 {
		dmBroadcastData["attachments"] = ctx.attachments
	}

	var excludeUser *uuid.UUID
	if !ctx.convPersonal {
		excludeUser = &ctx.senderUserID
	}
	h.dmBroadcast <- DMBroadcastMessage{
		ConversationID: ctx.convUUID,
		ExcludeUser:    excludeUser,
		Data:           OutgoingMessage{Type: "dm_message", Data: dmBroadcastData},
	}
}

// handleDMMessage handles a DM chat message: validates, persists, then broadcasts.
func (h *Hub) handleDMMessage(msg IncomingMessage) {
	client, convUUID, input := h.validateDMMessage(msg)
	if input == nil {
		return
	}
	if input.mentionAddendum != nil {
		defer input.mentionAddendum.Wipe()
	}

	convPersonal, ok := h.enforceDMEncryption(msg, convUUID, input.isEncrypted, input.keyVersion)
	if !ok {
		return
	}

	messageID, createdAt, updatedAt, err := h.persistDMMessage(convUUID, msg.UserID, input)
	if err != nil {
		log.Printf("Failed to persist DM message: %v", err)
		h.sendError(msg.ClientID, "Failed to save message")
		return
	}

	convID := convUUID.String()
	attachments := h.linkDMAttachments(messageID, msg.UserID.String(), input.attachmentIDs, convID)

	nonce, _ := msg.Data[keyNonce].(string)
	h.sendDMMessageAck(client, nonce, messageID, convUUID, createdAt, updatedAt, attachments)
	h.broadcastDMMessage(dmBroadcastCtx{
		messageID: messageID, convUUID: convUUID, senderUserID: msg.UserID,
		client: client, input: input, createdAt: createdAt, updatedAt: updatedAt,
		attachments: attachments, convPersonal: convPersonal,
	})
	h.sendDMUnreadNotify(convUUID, msg.UserID, dmUnreadLastMessage{
		Content:     input.content,
		UserID:      msg.UserID.String(),
		Username:    client.Username,
		CreatedAt:   createdAt,
		IsEncrypted: input.isEncrypted,
	})

	if input.mentionAddendum != nil {
		h.routeDMMentionNotifications(convUUID, msg.UserID, input.mentionAddendum)
	}
}

// sendDMUnreadNotify sends a notification to DM participants who are not
// subscribed to the conversation (they don't have it active). Includes
// last-message metadata so clients can update previews and ordering.
func (h *Hub) sendDMUnreadNotify(conversationID, senderUserID uuid.UUID, lastMsg dmUnreadLastMessage) {
	participants, err := h.dmUnreadParticipants(conversationID)
	if err != nil {
		return
	}

	notifyMsg := OutgoingMessage{
		Type: "dm_unread_notify",
		Data: map[string]interface{}{
			"conversation_id": conversationID.String(),
			"last_message": map[string]interface{}{
				"content":      lastMsg.Content,
				"user_id":      lastMsg.UserID,
				"username":     lastMsg.Username,
				"created_at":   lastMsg.CreatedAt,
				"is_encrypted": lastMsg.IsEncrypted,
			},
		},
	}
	data, marshalErr := json.Marshal(notifyMsg)
	if marshalErr != nil {
		return
	}

	dmClients := h.dmSubscriptions[conversationID] // may be nil

	for _, uid := range participants {
		if uid == senderUserID {
			continue
		}
		h.notifyUnsubscribedUser(uid, dmClients, data)
	}
}

// dmUnreadParticipants queries all participant UUIDs for a DM conversation.
func (h *Hub) dmUnreadParticipants(conversationID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := h.db.Query(`SELECT user_id FROM dm_participants WHERE conversation_id = $1`, conversationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var participants []uuid.UUID
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			continue
		}
		parsed, err := uuid.Parse(uid)
		if err != nil {
			continue
		}
		participants = append(participants, parsed)
	}
	return participants, nil
}

// userHasDMSubscription checks whether any of a user's clients are subscribed
// to the given DM conversation's client set.
func (h *Hub) userHasDMSubscription(userClientIDs, dmClients map[uuid.UUID]bool) bool {
	if dmClients == nil {
		return false
	}
	for clientID := range userClientIDs {
		if dmClients[clientID] {
			return true
		}
	}
	return false
}

// notifyUnsubscribedUser sends data to all of a user's connected clients,
// but only if none of them are already subscribed to the DM conversation.
func (h *Hub) notifyUnsubscribedUser(uid uuid.UUID, dmClients map[uuid.UUID]bool, data []byte) {
	userClientIDs, ok := h.userClients[uid]
	if !ok {
		return // user not connected
	}
	if h.userHasDMSubscription(userClientIDs, dmClients) {
		return
	}
	h.sendToUserClients(userClientIDs, data)
}

// sendToUserClients sends raw message data to all of a user's connected clients.
func (h *Hub) sendToUserClients(userClientIDs map[uuid.UUID]bool, data []byte) {
	for clientID := range userClientIDs {
		if c, ok := h.clients[clientID]; ok {
			select {
			case c.Send <- data:
			default:
			}
		}
	}
}

// handleDMTyping handles typing indicators for DM conversations.
func (h *Hub) handleDMTyping(msg IncomingMessage) {
	convID, ok := msg.Data[keyConversationID].(string)
	if !ok {
		return
	}

	convUUID, err := uuid.Parse(convID)
	if err != nil {
		return
	}

	client, ok := h.clients[msg.ClientID]
	if !ok {
		return
	}

	// Verify subscription
	subscribers, hasSubs := h.dmSubscriptions[convUUID]
	if !hasSubs || !subscribers[client.ID] {
		return
	}

	h.dmBroadcast <- DMBroadcastMessage{
		ConversationID: convUUID,
		ExcludeUser:    &msg.UserID,
		Data: OutgoingMessage{
			Type: "dm_typing",
			Data: map[string]interface{}{
				keyConversationID: convUUID,
				keyUserID:         msg.UserID,
				keyUsername:       client.Username,
				keyIsTyping:       msg.Data[keyIsTyping],
			},
		},
	}
}

// handleDMBroadcast sends a message to all subscribers of a DM conversation.
func (h *Hub) handleDMBroadcast(msg DMBroadcastMessage) {
	subscribers, ok := h.dmSubscriptions[msg.ConversationID]
	if !ok {
		return
	}

	messageData, err := json.Marshal(msg.Data)
	if err != nil {
		log.Printf("Failed to marshal DM broadcast message: %v", err)
		return
	}

	for clientID := range subscribers {
		client, ok := h.clients[clientID]
		if !ok {
			continue
		}

		if msg.ExcludeUser != nil && client.UserID == *msg.ExcludeUser {
			continue
		}

		select {
		case client.Send <- messageData:
		default:
			h.handleUnregister(client)
		}
	}
}

// BroadcastToDM sends a message to all clients subscribed to a DM conversation (thread-safe).
func (h *Hub) BroadcastToDM(conversationID uuid.UUID, msg OutgoingMessage) {
	h.dmBroadcast <- DMBroadcastMessage{
		ConversationID: conversationID,
		Data:           msg,
	}
}

// --- Force-disconnect handlers (server-side session termination) ---

// handleDisconnectUser disconnects ALL WebSocket clients for a user.
// Sends a courtesy "session_revoked" message before severing each TCP connection.
// The real enforcement is handleUnregister which closes the Send channel and connection.
// A rogue client that ignores session_revoked still gets killed at the TCP level.
func (h *Hub) handleDisconnectUser(userID uuid.UUID) {
	clientIDs, ok := h.userClients[userID]
	if !ok {
		return
	}

	// Collect clients before modifying the map (handleUnregister mutates it)
	clients := make([]*Client, 0, len(clientIDs))
	for clientID := range clientIDs {
		if client, ok := h.clients[clientID]; ok {
			clients = append(clients, client)
		}
	}

	// Courtesy notification + forceful disconnect
	revokedMsg, _ := json.Marshal(OutgoingMessage{
		Type: sessionRevoked,
		Data: map[string]interface{}{"reason": "session_terminated"},
	})
	for _, client := range clients {
		// Best-effort courtesy message (non-blocking)
		if revokedMsg != nil {
			select {
			case client.Send <- revokedMsg:
			default:
			}
		}
		// Real enforcement: sever TCP connection
		h.handleUnregister(client)
	}

	if len(clients) > 0 {
		log.Printf("Force-disconnected %d client(s) for user %s", len(clients), userID)
	}
}

// handleDisconnectSession disconnects WebSocket clients matching a specific session ID.
// Used for targeted single-session revocation from the sessions management UI.
func (h *Hub) handleDisconnectSession(sessionID string) {
	if sessionID == "" {
		return
	}

	revokedMsg, _ := json.Marshal(OutgoingMessage{
		Type: sessionRevoked,
		Data: map[string]interface{}{"reason": sessionRevoked},
	})

	var count int
	for _, client := range h.clients {
		if client.SessionID == sessionID {
			if revokedMsg != nil {
				select {
				case client.Send <- revokedMsg:
				default:
				}
			}
			h.handleUnregister(client)
			count++
		}
	}

	if count > 0 {
		log.Printf("Force-disconnected %d client(s) for session %s", count, sessionID)
	}
}

// DisconnectUser forces all WebSocket connections for a user to close (thread-safe).
// Called from HTTP handlers (logout, credential change, session revocation) when
// a token is blacklisted or session is revoked. The server does not trust the
// client to disconnect itself — this is the authoritative enforcement.
func (h *Hub) DisconnectUser(userID uuid.UUID) {
	h.disconnectUser <- userID
}

// DisconnectSession forces a specific session's WebSocket connections to close (thread-safe).
// Called from the single-session revocation handler for targeted disconnect.
func (h *Hub) DisconnectSession(sessionID string) {
	h.disconnectSession <- sessionID
}

// handleProfileUpdate refreshes cached user info on the hub's Client objects.
// The actual "profile_updated" broadcast is sent by the HTTP handler (UpdateMe)
// to ensure reliable delivery without depending on the client-driven WS round-trip.
func (h *Hub) handleProfileUpdate(msg IncomingMessage) {
	client, ok := h.clients[msg.ClientID]
	if !ok {
		return
	}

	// Re-query user info from DB
	var username string
	var displayName *string
	var avatarURL *string
	err := h.db.QueryRow(
		"SELECT username, display_name, avatar_url FROM users WHERE id = $1",
		msg.UserID,
	).Scan(&username, &displayName, &avatarURL)
	if err != nil {
		log.Printf("Failed to refresh user info for %s: %v", msg.UserID, err)
		return
	}

	// Update cached fields on all of this user's clients
	if userClients, ok := h.userClients[client.UserID]; ok {
		for clientID := range userClients {
			if c, ok := h.clients[clientID]; ok {
				c.Username = username
				c.DisplayName = displayName
				c.AvatarURL = avatarURL
			}
		}
	}
}

// handleServerUpdate re-queries server info from DB and broadcasts to all server subscribers.
func (h *Hub) handleServerUpdate(msg IncomingMessage) {
	serverIDStr, ok := msg.Data[keyServerID].(string)
	if !ok {
		return
	}
	serverID, err := uuid.Parse(serverIDStr)
	if err != nil {
		return
	}

	// Re-query server info from DB
	var name string
	var iconURL *string
	var bannerURL *string
	err = h.db.QueryRow(
		"SELECT name, icon_url, banner_url FROM servers WHERE id = $1",
		serverID,
	).Scan(&name, &iconURL, &bannerURL)
	if err != nil {
		log.Printf("Failed to refresh server info for %s: %v", serverID, err)
		return
	}

	// Broadcast to all subscribers of this server
	broadcastMsg := OutgoingMessage{
		Type: "server_updated",
		Data: map[string]interface{}{
			keyServerID:  serverID.String(),
			"name":       name,
			"icon_url":   iconURL,
			"banner_url": bannerURL,
		},
	}
	data, err := json.Marshal(broadcastMsg)
	if err != nil {
		return
	}

	subscribers, ok := h.serverSubscriptions[serverID]
	if !ok {
		return
	}
	for clientID := range subscribers {
		if c, ok := h.clients[clientID]; ok {
			select {
			case c.Send <- data:
			default:
			}
		}
	}
}

// broadcastPresenceToAll sends a presence update to all connected clients
func (h *Hub) broadcastPresenceToAll(userID uuid.UUID, status string, timestamp int64) {
	msg := OutgoingMessage{
		Type: "presence",
		Data: map[string]interface{}{
			keyUserID:   userID.String(),
			keyStatus:   status,
			"timestamp": timestamp,
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Failed to marshal presence message: %v", err)
		return
	}
	for _, client := range h.clients {
		select {
		case client.Send <- data:
		default:
		}
	}

	// Schedule a debounced recomputation of online counts for all servers
	// the affected user belongs to (batches rapid presence changes).
	h.scheduleOnlineCountBroadcast(userID)
}

// scheduleOnlineCountBroadcast adds userID to the pending set and starts a
// 500ms debounce timer (if not already running). When the timer fires, all
// accumulated user IDs are flushed in a single batched DB+Redis query.
//
// Called from within the hub's Run goroutine, so no locking is needed.
func (h *Hub) scheduleOnlineCountBroadcast(userID uuid.UUID) {
	h.onlineCountPending[userID] = true
	if h.onlineCountTimer == nil {
		h.onlineCountTimer = time.NewTimer(500 * time.Millisecond)
	}
}

// flushOnlineCounts recomputes the visible online member count for every server
// that any pending user belongs to, then broadcasts the updated counts to all
// connected clients. A member is "visible online" if they have an active
// WebSocket connection and their status is not "invisible".
//
// Called from within the hub's Run goroutine when the debounce timer fires.
func (h *Hub) flushOnlineCounts() {
	h.onlineCountTimer = nil

	if len(h.onlineCountPending) == 0 {
		return
	}

	params := h.collectPendingUsers()

	serverMembers, allMemberIDs, err := h.queryServerMemberships(params)
	if err != nil || len(serverMembers) == 0 {
		return
	}

	visibleOnline := h.resolveVisibleOnline(allMemberIDs)

	counts := h.computeServerCounts(serverMembers, visibleOnline)

	h.handleGlobalBroadcast(OutgoingMessage{
		Type: "server_online_counts",
		Data: map[string]interface{}{
			keyCounts: counts,
		},
	})
}

// collectPendingUsers drains onlineCountPending and returns the user IDs as query params.
func (h *Hub) collectPendingUsers() []interface{} {
	params := make([]interface{}, 0, len(h.onlineCountPending))
	for uid := range h.onlineCountPending {
		params = append(params, uid)
	}
	clear(h.onlineCountPending)
	return params
}

// buildPlaceholders creates a "$1,$2,..." parameterized IN clause for the given params.
func buildPlaceholders(n int) string {
	placeholders := make([]byte, 0, n*4)
	for i := range n {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, []byte(fmt.Sprintf("$%d", i+1))...)
	}
	return string(placeholders)
}

// queryServerMemberships returns all server->member mappings for servers that any of
// the given users belong to, plus a set of all unique member IDs.
func (h *Hub) queryServerMemberships(params []interface{}) (map[string][]uuid.UUID, map[uuid.UUID]bool, error) {
	// Placeholders are safe — generated as $1,$2,… from loop index, not user input.
	query := fmt.Sprintf( //nolint:gosec // G201 // nosemgrep:concord-go-sql-sprintf
		`SELECT sm.server_id, sm.user_id
		FROM server_members sm
		WHERE sm.server_id IN (
			SELECT DISTINCT server_id FROM server_members WHERE user_id IN (%s)
		)
	`, buildPlaceholders(len(params)))

	rows, err := h.db.Query(query, params...)
	if err != nil {
		log.Printf("Failed to query memberships for online counts: %v", err)
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()

	serverMembers := make(map[string][]uuid.UUID)
	allMemberIDs := make(map[uuid.UUID]bool)
	for rows.Next() {
		var sid, uid uuid.UUID
		if err := rows.Scan(&sid, &uid); err == nil {
			sidStr := sid.String()
			serverMembers[sidStr] = append(serverMembers[sidStr], uid)
			allMemberIDs[uid] = true
		}
	}
	return serverMembers, allMemberIDs, nil
}

// resolveVisibleOnline determines which member IDs are connected and not invisible,
// using a single Redis MGET round-trip for status checks.
func (h *Hub) resolveVisibleOnline(allMemberIDs map[uuid.UUID]bool) map[uuid.UUID]bool {
	connectedUIDs := make([]uuid.UUID, 0, len(allMemberIDs))
	redisKeys := make([]string, 0, len(allMemberIDs))
	for uid := range allMemberIDs {
		if _, connected := h.userClients[uid]; connected {
			connectedUIDs = append(connectedUIDs, uid)
			redisKeys = append(redisKeys, fmt.Sprintf(presenceKeyFmt, uid))
		}
	}

	visibleOnline := make(map[uuid.UUID]bool, len(connectedUIDs))
	if len(redisKeys) == 0 {
		return visibleOnline
	}

	statuses, err := h.redis.MGet(context.Background(), redisKeys...).Result()
	if err != nil {
		// Redis error — fall back to counting all connected as online
		for _, uid := range connectedUIDs {
			visibleOnline[uid] = true
		}
		return visibleOnline
	}

	for i, val := range statuses {
		if isVisibleStatus(val) {
			visibleOnline[connectedUIDs[i]] = true
		}
	}
	return visibleOnline
}

// isVisibleStatus returns true if a Redis presence value indicates
// the user should be counted as visibly online (nil = online, any
// non-invisible string = online).
func isVisibleStatus(val interface{}) bool {
	if val == nil {
		return true
	}
	s, ok := val.(string)
	return ok && s != statusInvisible
}

// computeServerCounts tallies the number of visibly-online members per server.
func (h *Hub) computeServerCounts(serverMembers map[string][]uuid.UUID, visibleOnline map[uuid.UUID]bool) map[string]int {
	counts := make(map[string]int, len(serverMembers))
	for sid, members := range serverMembers {
		count := 0
		for _, uid := range members {
			if visibleOnline[uid] {
				count++
			}
		}
		counts[sid] = count
	}
	return counts
}

// broadcastServerVoiceCounts queries voice_participants to compute per-server
// voice user counts and broadcasts them to all connected clients.
func (h *Hub) broadcastServerVoiceCounts() {
	rows, err := h.db.Query(`
		SELECT c.server_id, COUNT(vp.id)
		FROM voice_participants vp
		JOIN channels c ON c.id = vp.channel_id
		GROUP BY c.server_id
	`)
	if err != nil {
		log.Printf("Failed to query server voice counts: %v", err)
		return
	}
	defer func() { _ = rows.Close() }()

	counts := make(map[string]int)
	for rows.Next() {
		var serverID string
		var count int
		if err := rows.Scan(&serverID, &count); err == nil {
			counts[serverID] = count
		}
	}

	h.handleGlobalBroadcast(OutgoingMessage{
		Type: "server_voice_counts",
		Data: map[string]interface{}{
			keyCounts: counts,
		},
	})
}

// BroadcastServerVoiceCounts triggers a recompute and broadcast of per-server
// voice participant counts. Safe to call from any goroutine.
func (h *Hub) BroadcastServerVoiceCounts() {
	select {
	case h.voiceCountSignal <- struct{}{}:
	default:
		// Already pending — coalesces rapid bursts
	}
}
