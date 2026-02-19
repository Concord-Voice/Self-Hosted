package websocket

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Hub maintains the set of active clients and broadcasts messages
type Hub struct {
	// Database connection for authorization checks
	db *sql.DB

	// Redis client for presence persistence
	redis *redis.Client

	// Registered clients (client ID -> Client)
	clients map[uuid.UUID]*Client

	// User ID to client IDs mapping (for multi-device support)
	userClients map[uuid.UUID]map[uuid.UUID]bool

	// Channel subscriptions (channel ID -> set of client IDs)
	channelSubscriptions map[uuid.UUID]map[uuid.UUID]bool

	// Username cache (user ID -> username) for typing indicators
	usernames map[uuid.UUID]string

	// Register requests from clients
	register chan *Client

	// Unregister requests from clients
	unregister chan *Client

	// Incoming messages from clients
	incoming chan IncomingMessage

	// Broadcast messages to all clients in a channel
	broadcast chan BroadcastMessage
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
		register:             make(chan *Client),
		unregister:           make(chan *Client),
		incoming:             make(chan IncomingMessage, 256),
		broadcast:            make(chan BroadcastMessage, 256),
	}
}

// Run starts the hub's main loop
func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.handleRegister(client)

		case client := <-h.unregister:
			h.handleUnregister(client)

		case message := <-h.incoming:
			h.handleIncoming(message)

		case message := <-h.broadcast:
			h.handleBroadcast(message)
		}
	}
}

// handleRegister registers a new client
func (h *Hub) handleRegister(client *Client) {
	h.clients[client.ID] = client

	// Track user's clients (multi-device support)
	if _, ok := h.userClients[client.UserID]; !ok {
		h.userClients[client.UserID] = make(map[uuid.UUID]bool)
	}
	h.userClients[client.UserID][client.ID] = true

	// Cache username for typing indicators
	if _, cached := h.usernames[client.UserID]; !cached {
		var username string
		err := h.db.QueryRow("SELECT username FROM users WHERE id = $1", client.UserID).Scan(&username)
		if err == nil {
			h.usernames[client.UserID] = username
		}
	}

	// If this is the user's first connection, broadcast online presence and set Redis
	if len(h.userClients[client.UserID]) == 1 {
		ctx := context.Background()
		h.redis.Set(ctx, fmt.Sprintf("presence:%s", client.UserID), "online", 120*time.Second)
		h.broadcastPresenceToAll(client.UserID, "online", time.Now().Unix())
	}

	log.Printf("Client registered: user=%s client=%s total_clients=%d",
		client.UserID, client.ID, len(h.clients))

	// Send connection confirmation
	confirmMsg := OutgoingMessage{
		Type: "connected",
		Data: map[string]interface{}{
			"client_id": client.ID,
			"user_id":   client.UserID,
		},
	}

	if data, err := json.Marshal(confirmMsg); err == nil {
		client.Send <- data
	}

	// Send enhanced presence snapshot of all currently online users to the new client
	ctx := context.Background()
	type userPresenceInfo struct {
		UserID string `json:"user_id"`
		Status string `json:"status"`
	}
	users := make([]userPresenceInfo, 0, len(h.userClients))
	onlineUserIDs := make([]string, 0, len(h.userClients))
	for uid := range h.userClients {
		uidStr := uid.String()
		status := "online"
		if val, err := h.redis.Get(ctx, fmt.Sprintf("presence:%s", uid)).Result(); err == nil {
			status = val
		}
		// Invisible users appear as offline to others, but show real status to self
		visibleStatus := status
		if status == "invisible" && uid != client.UserID {
			visibleStatus = "offline"
		} else if status == "invisible" && uid == client.UserID {
			// Keep "invisible" so the user knows their own status
			visibleStatus = "invisible"
		}
		// Only add to online_user_ids if visible (not invisible to this viewer)
		if visibleStatus != "offline" {
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

// handleUnregister unregisters a client
func (h *Hub) handleUnregister(client *Client) {
	if _, ok := h.clients[client.ID]; ok {
		// Remove from clients map
		delete(h.clients, client.ID)

		// Remove from user's clients
		if userClients, ok := h.userClients[client.UserID]; ok {
			delete(userClients, client.ID)
			if len(userClients) == 0 {
				delete(h.userClients, client.UserID)
				// User's last client disconnected — update Redis and broadcast offline
				now := time.Now().Unix()
				ctx := context.Background()
				h.redis.Del(ctx, fmt.Sprintf("presence:%s", client.UserID))
				h.redis.Set(ctx, fmt.Sprintf("last_seen:%s", client.UserID), fmt.Sprintf("%d", now), 0)
				h.broadcastPresenceToAll(client.UserID, "offline", now)
				// Clean up username cache
				delete(h.usernames, client.UserID)
			}
		}

		// Remove from all channel subscriptions
		for channelID := range client.Channels {
			if clients, ok := h.channelSubscriptions[channelID]; ok {
				delete(clients, client.ID)
				if len(clients) == 0 {
					delete(h.channelSubscriptions, channelID)
				}
			}
		}

		// Close send channel
		close(client.Send)

		log.Printf("Client unregistered: user=%s client=%s remaining=%d",
			client.UserID, client.ID, len(h.clients))
	}
}

// handleIncoming processes incoming messages from clients
func (h *Hub) handleIncoming(msg IncomingMessage) {
	switch msg.Type {
	case "subscribe":
		h.handleSubscribe(msg)
	case "unsubscribe":
		h.handleUnsubscribe(msg)
	case "message":
		h.handleMessage(msg)
	case "typing":
		h.handleTyping(msg)
	case "heartbeat":
		h.handleHeartbeat(msg)
	case "set_status":
		h.handleSetStatus(msg)
	default:
		log.Printf("Unknown message type: %s", msg.Type)
	}
}

// handleSubscribe subscribes a client to a channel after verifying membership
func (h *Hub) handleSubscribe(msg IncomingMessage) {
	channelID, ok := msg.Data["channel_id"].(string)
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
			"channel_id": channelUUID,
		},
	}
	if data, err := json.Marshal(confirmMsg); err == nil {
		client.Send <- data
	}
}

// handleUnsubscribe unsubscribes a client from a channel
func (h *Hub) handleUnsubscribe(msg IncomingMessage) {
	channelID, ok := msg.Data["channel_id"].(string)
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

// sendError sends an error message to a specific client
func (h *Hub) sendError(clientID uuid.UUID, message string) {
	client, ok := h.clients[clientID]
	if !ok {
		return
	}
	errMsg := OutgoingMessage{
		Type: "error",
		Data: map[string]interface{}{
			"message": message,
		},
	}
	if data, err := json.Marshal(errMsg); err == nil {
		select {
		case client.Send <- data:
		default:
		}
	}
}

// handleMessage handles a chat message: validates, persists to DB, then broadcasts
func (h *Hub) handleMessage(msg IncomingMessage) {
	channelID, ok := msg.Data["channel_id"].(string)
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
		h.sendError(msg.ClientID, "Not subscribed to this channel")
		return
	}

	// Validate content
	content, ok := msg.Data["content"].(string)
	if !ok || content == "" {
		h.sendError(msg.ClientID, "Message content is required")
		return
	}
	if len(content) > 4000 {
		h.sendError(msg.ClientID, "Message content exceeds maximum length")
		return
	}

	isEncrypted, _ := msg.Data["is_encrypted"].(bool)

	// Persist message to database
	messageID := uuid.New()
	var createdAt, updatedAt time.Time
	err = h.db.QueryRow(
		`INSERT INTO messages (id, channel_id, user_id, content, is_encrypted, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, NOW(), NOW())
		 RETURNING created_at, updated_at`,
		messageID, channelUUID, msg.UserID, content, isEncrypted,
	).Scan(&createdAt, &updatedAt)
	if err != nil {
		log.Printf("Failed to persist message: %v", err)
		h.sendError(msg.ClientID, "Failed to save message")
		return
	}

	// Broadcast persisted message to all subscribers except sender
	// (sender sees the message instantly via optimistic UI)
	broadcastMsg := BroadcastMessage{
		ChannelID:   channelUUID,
		ExcludeUser: &msg.UserID,
		Data: OutgoingMessage{
			Type: "message",
			Data: map[string]interface{}{
				"id":           messageID,
				"channel_id":   channelUUID,
				"user_id":      msg.UserID,
				"username":     client.Username,
				"display_name": client.DisplayName,
				"avatar_url":   client.AvatarURL,
				"content":      content,
				"is_encrypted": isEncrypted,
				"created_at":   createdAt,
				"updated_at":   updatedAt,
			},
		},
	}

	h.broadcast <- broadcastMsg
}

// handleTyping handles typing indicator
func (h *Hub) handleTyping(msg IncomingMessage) {
	channelID, ok := msg.Data["channel_id"].(string)
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
				"channel_id": channelUUID,
				"user_id":    msg.UserID,
				"username":   client.Username,
				"is_typing":  msg.Data["is_typing"],
			},
		},
	}

	h.broadcast <- broadcastMsg
}

// handleHeartbeat refreshes the Redis presence TTL for the user
func (h *Hub) handleHeartbeat(msg IncomingMessage) {
	ctx := context.Background()
	key := fmt.Sprintf("presence:%s", msg.UserID)
	// Refresh TTL for any user-set status (don't change the value, just extend the TTL)
	val, err := h.redis.Get(ctx, key).Result()
	if err == nil {
		// Extend TTL for any valid status
		switch val {
		case "online", "dnd", "invisible":
			h.redis.Expire(ctx, key, 120*time.Second)
		}
	} else if err == redis.Nil {
		// Key expired or missing — re-set to online if user is still connected
		if _, ok := h.userClients[msg.UserID]; ok {
			h.redis.Set(ctx, key, "online", 120*time.Second)
		}
	}
}

// handleSetStatus allows users to manually set their status (online/dnd/invisible)
func (h *Hub) handleSetStatus(msg IncomingMessage) {
	status, ok := msg.Data["status"].(string)
	if !ok {
		return
	}

	// Validate status
	switch status {
	case "online", "dnd", "invisible":
		// valid
	default:
		return
	}

	ctx := context.Background()
	key := fmt.Sprintf("presence:%s", msg.UserID)
	h.redis.Set(ctx, key, status, 120*time.Second)

	// For invisible, broadcast as "offline" to other users (but store real status in Redis)
	broadcastStatus := status
	if status == "invisible" {
		broadcastStatus = "offline"
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

// GetConnectedUsers returns a map of user IDs currently connected
func (h *Hub) GetConnectedUsers() map[uuid.UUID]bool {
	users := make(map[uuid.UUID]bool)
	for userID := range h.userClients {
		users[userID] = true
	}
	return users
}

// GetUserClientCount returns the number of connected clients for a user
func (h *Hub) GetUserClientCount(userID uuid.UUID) int {
	if clients, ok := h.userClients[userID]; ok {
		return len(clients)
	}
	return 0
}

// broadcastPresenceToAll sends a presence update to all connected clients
func (h *Hub) broadcastPresenceToAll(userID uuid.UUID, status string, timestamp int64) {
	msg := OutgoingMessage{
		Type: "presence",
		Data: map[string]interface{}{
			"user_id":   userID.String(),
			"status":    status,
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
}
