package websocket

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- handleMessage integration tests (require DB) ---

func setupMessageTest(t *testing.T, encrypted bool) *hubTestSetup {
	t.Helper()

	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)

	userID := uuid.New()
	hash := "$argon2id$v=19$m=65536,t=3,p=4$3pE9STD1TqLPoZQ2/BTLCg$8SKTCjsZh8Q7pAulEqAIEzJQK9eeOb5ipWhPz4REdCY" //nolint:gosec

	_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified) VALUES ($1, $2, $3, $4, true, true)`,
		userID.String(), "hubmsguser@test.concord.chat", "hubmsguser", hash)
	require.NoError(t, err)

	// Create server
	serverID := uuid.New()
	_, err = db.Exec(`INSERT INTO servers (id, name, owner_id, e2ee_default, allow_embedded_content) VALUES ($1, $2, $3, false, true)`,
		serverID.String(), "Test Server", userID.String())
	require.NoError(t, err)

	// Add user as member
	_, err = db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'owner')`,
		serverID.String(), userID.String())
	require.NoError(t, err)

	// Create channel
	channelID := uuid.New()
	_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type, is_encrypted) VALUES ($1, $2, $3, 'text', $4)`,
		channelID.String(), serverID.String(), "general", encrypted)
	require.NoError(t, err)

	clientID := uuid.New()
	client := &Client{
		ID:       clientID,
		UserID:   userID,
		Username: "hubmsguser",
		Send:     make(chan []byte, 10),
		Hub:      hub,
		Channels: map[uuid.UUID]bool{channelID: true},
	}

	hub.clients[clientID] = client
	hub.userClients[userID] = map[uuid.UUID]bool{clientID: true}
	hub.channelSubscriptions[channelID] = map[uuid.UUID]bool{clientID: true}
	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{clientID: true}

	t.Cleanup(func() {
		if _, err := db.Exec(`TRUNCATE users, servers, server_members, channels, messages CASCADE`); err != nil {
			t.Errorf("failed to truncate tables: %v", err)
		}
	})

	return &hubTestSetup{
		hub:    hub,
		db:     db,
		client: client,
		convID: channelID.String(),
		user1:  userID,
		user2:  serverID, // reusing user2 field for serverID
	}
}

func TestHandleMessage_PlaintextSuccess(t *testing.T) {
	setup := setupMessageTest(t, false)
	channelID := setup.convID

	msg := IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID: channelID,
			keyContent:   "Hello, world!",
			keyNonce:     "nonce-123",
		},
	}

	setup.hub.handleMessage(msg)

	// Should receive message_ack
	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "message_ack", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Equal(t, "nonce-123", data[keyNonce])
	assert.NotEmpty(t, data["id"])
}

func TestHandleMessage_NotSubscribed(t *testing.T) {
	setup := setupMessageTest(t, false)

	// Use a channel the client is NOT subscribed to
	otherChannelID := uuid.New()

	msg := IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID: otherChannelID.String(),
			keyContent:   "should fail",
		},
	}

	setup.hub.handleMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Contains(t, data[keyMessage], "Not subscribed")
}

func TestHandleMessage_EmptyContent(t *testing.T) {
	setup := setupMessageTest(t, false)

	msg := IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID: setup.convID,
			keyContent:   "",
		},
	}

	setup.hub.handleMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Contains(t, data[keyMessage], "content is required")
}

func TestHandleMessage_MissingContent(t *testing.T) {
	setup := setupMessageTest(t, false)

	msg := IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID: setup.convID,
		},
	}

	setup.hub.handleMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
}

func TestHandleMessage_ContentTooLong(t *testing.T) {
	setup := setupMessageTest(t, false)

	longContent := make([]byte, 4001)
	for i := range longContent {
		longContent[i] = 'a'
	}

	msg := IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID: setup.convID,
			keyContent:   string(longContent),
		},
	}

	setup.hub.handleMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Contains(t, data[keyMessage], "maximum length")
}

func TestHandleMessage_EncryptedContentLongerLimit(t *testing.T) {
	setup := setupMessageTest(t, false)

	// Encrypted messages allow up to 24000 chars
	longContent := make([]byte, 5000)
	for i := range longContent {
		longContent[i] = 'a'
	}

	msg := IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID:   setup.convID,
			keyContent:     string(longContent),
			keyIsEncrypted: true,
		},
	}

	setup.hub.handleMessage(msg)

	// Should succeed (5000 < 24000)
	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "message_ack", resp["type"])
}

func TestHandleMessage_PlaintextInEncryptedChannel(t *testing.T) {
	setup := setupMessageTest(t, true) // encrypted channel

	msg := IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID:   setup.convID,
			keyContent:     "plaintext in encrypted channel",
			keyIsEncrypted: false,
		},
	}

	setup.hub.handleMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Contains(t, data[keyMessage], "E2EE")
}

func TestHandleMessage_InvalidChannelID(t *testing.T) {
	setup := setupMessageTest(t, false)

	msg := IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID: "not-a-uuid",
			keyContent:   "test",
		},
	}

	// Should not panic, returns early
	setup.hub.handleMessage(msg)
}

func TestHandleMessage_MissingChannelID(t *testing.T) {
	setup := setupMessageTest(t, false)

	msg := IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data:     map[string]interface{}{keyContent: "test"},
	}

	// Should not panic, returns early
	setup.hub.handleMessage(msg)
}

func TestHandleMessage_ClientNotFound(t *testing.T) {
	setup := setupMessageTest(t, false)

	msg := IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: uuid.New(), // non-existent client
		Data: map[string]interface{}{
			keyChannelID: setup.convID,
			keyContent:   "test",
		},
	}

	// Should not panic
	setup.hub.handleMessage(msg)
}

func TestHandleMessage_RateLimitExceeded(t *testing.T) {
	setup := setupMessageTest(t, false)

	// Exhaust rate limit
	setup.client.rateTokens = 0
	setup.client.rateLastFill = time.Now()

	msg := IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID: setup.convID,
			keyContent:   "rate limited message",
		},
	}

	setup.hub.handleMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Contains(t, data[keyMessage], "Rate limit")
}

func TestHandleMessage_BroadcastToOtherSubscribers(t *testing.T) {
	setup := setupMessageTest(t, false)
	channelUUID, _ := uuid.Parse(setup.convID)

	// Add a second user/client subscribed to the same channel
	otherUser := uuid.New()
	otherClient := &Client{
		ID:       uuid.New(),
		UserID:   otherUser,
		Username: "otheruser",
		Send:     make(chan []byte, 10),
		Hub:      setup.hub,
		Channels: map[uuid.UUID]bool{channelUUID: true},
	}
	setup.hub.clients[otherClient.ID] = otherClient
	setup.hub.userClients[otherUser] = map[uuid.UUID]bool{otherClient.ID: true}
	setup.hub.channelSubscriptions[channelUUID][otherClient.ID] = true

	msg := IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID: setup.convID,
			keyContent:   "broadcast test",
		},
	}

	setup.hub.handleMessage(msg)

	// Sender gets message_ack
	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "message_ack", resp["type"])

	// The broadcast message goes to the hub's broadcast channel
	select {
	case bMsg := <-setup.hub.broadcast:
		assert.Equal(t, channelUUID, bMsg.ChannelID)
		assert.Equal(t, "message", bMsg.Data.Type)
		assert.Equal(t, &setup.user1, bMsg.ExcludeUser)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected broadcast message")
	}
}

// --- handleSubscribe integration tests ---

func TestHandleSubscribe_Success(t *testing.T) {
	setup := setupMessageTest(t, false)
	channelUUID, _ := uuid.Parse(setup.convID)

	// Remove from current subscriptions to test subscribing
	delete(setup.client.Channels, channelUUID)
	delete(setup.hub.channelSubscriptions[channelUUID], setup.client.ID)

	msg := IncomingMessage{
		Type:     "subscribe",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID: setup.convID,
		},
	}

	setup.hub.handleSubscribe(msg)

	// Should receive subscribed confirmation
	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "subscribed", resp["type"])

	// Client should be in channel subscriptions
	assert.True(t, setup.client.Channels[channelUUID])
	assert.True(t, setup.hub.channelSubscriptions[channelUUID][setup.client.ID])
}

func TestHandleSubscribe_InvalidChannelID(t *testing.T) {
	setup := setupMessageTest(t, false)

	msg := IncomingMessage{
		Type:     "subscribe",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID: "not-a-uuid",
		},
	}

	setup.hub.handleSubscribe(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
}

func TestHandleSubscribe_MissingChannelID(t *testing.T) {
	setup := setupMessageTest(t, false)

	msg := IncomingMessage{
		Type:     "subscribe",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data:     map[string]interface{}{},
	}

	setup.hub.handleSubscribe(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
}

func TestHandleSubscribe_ClientNotFound(t *testing.T) {
	setup := setupMessageTest(t, false)

	msg := IncomingMessage{
		Type:     "subscribe",
		ClientID: uuid.New(),
		Data: map[string]interface{}{
			keyChannelID: setup.convID,
		},
	}

	// Should not panic
	setup.hub.handleSubscribe(msg)
}

// --- handleSubscribeServer integration tests ---

func TestHandleSubscribeServer_Success(t *testing.T) {
	setup := setupMessageTest(t, false)
	serverID := setup.user2 // reused field for serverID

	msg := IncomingMessage{
		Type:     "subscribe_server",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyServerID: serverID.String(),
		},
	}

	setup.hub.handleSubscribeServer(msg)

	// Client should be subscribed to server
	assert.True(t, setup.hub.serverSubscriptions[serverID][setup.client.ID])
}

func TestHandleSubscribeServer_InvalidServerID(t *testing.T) {
	setup := setupMessageTest(t, false)

	msg := IncomingMessage{
		Type:     "subscribe_server",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyServerID: "not-a-uuid",
		},
	}

	setup.hub.handleSubscribeServer(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
}

func TestHandleSubscribeServer_MissingServerID(t *testing.T) {
	setup := setupMessageTest(t, false)

	msg := IncomingMessage{
		Type:     "subscribe_server",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data:     map[string]interface{}{},
	}

	setup.hub.handleSubscribeServer(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
}

// --- handleSubscribeDM integration tests ---

func TestHandleSubscribeDM_Success(t *testing.T) {
	setup := setupEpochTest(t, false, false, false)

	msg := IncomingMessage{
		Type:     "subscribe_dm",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: setup.convID,
		},
	}

	// Client is already subscribed from setup, remove first
	convUUID, _ := uuid.Parse(setup.convID)
	delete(setup.hub.dmSubscriptions[convUUID], setup.client.ID)

	setup.hub.handleSubscribeDM(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "dm_subscribed", resp["type"])
	assert.True(t, setup.hub.dmSubscriptions[convUUID][setup.client.ID])
}

func TestHandleSubscribeDM_NotParticipant(t *testing.T) {
	setup := setupEpochTest(t, false, false, false)

	// Create a DM conversation the user is NOT part of
	otherConvID := uuid.New()
	_, err := setup.db.Exec(`INSERT INTO dm_conversations (id, is_group, is_personal, is_encrypted, created_by) VALUES ($1, false, false, false, $2)`,
		otherConvID.String(), setup.user2.String())
	require.NoError(t, err)
	_, err = setup.db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`,
		otherConvID.String(), setup.user2.String())
	require.NoError(t, err)

	msg := IncomingMessage{
		Type:     "subscribe_dm",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: otherConvID.String(),
		},
	}

	setup.hub.handleSubscribeDM(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Contains(t, data[keyMessage], "Not a participant")
}

func TestHandleSubscribeDM_InvalidConversationID(t *testing.T) {
	setup := setupEpochTest(t, false, false, false)

	msg := IncomingMessage{
		Type:     "subscribe_dm",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: "not-a-uuid",
		},
	}

	setup.hub.handleSubscribeDM(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
}

func TestHandleSubscribeDM_MissingConversationID(t *testing.T) {
	setup := setupEpochTest(t, false, false, false)

	msg := IncomingMessage{
		Type:     "subscribe_dm",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data:     map[string]interface{}{},
	}

	setup.hub.handleSubscribeDM(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
}

// --- handleDMMessage additional tests ---

func TestHandleDMMessage_EmptyContent(t *testing.T) {
	setup := setupEpochTest(t, false, false, false)

	msg := IncomingMessage{
		Type:     "dm_message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: setup.convID,
			keyContent:        "",
		},
	}

	setup.hub.handleDMMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Contains(t, data[keyMessage], "content is required")
}

func TestHandleDMMessage_ContentTooLong(t *testing.T) {
	setup := setupEpochTest(t, false, false, false)

	longContent := make([]byte, 4001)
	for i := range longContent {
		longContent[i] = 'a'
	}

	msg := IncomingMessage{
		Type:     "dm_message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: setup.convID,
			keyContent:        string(longContent),
		},
	}

	setup.hub.handleDMMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Contains(t, data[keyMessage], "maximum length")
}

func TestHandleDMMessage_RateLimitExceeded(t *testing.T) {
	setup := setupEpochTest(t, false, false, false)

	setup.client.rateTokens = 0
	setup.client.rateLastFill = time.Now()

	msg := IncomingMessage{
		Type:     "dm_message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: setup.convID,
			keyContent:        "rate limited",
		},
	}

	setup.hub.handleDMMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Contains(t, data[keyMessage], "Rate limit")
}

func TestHandleDMMessage_InvalidConversationID(t *testing.T) {
	setup := setupEpochTest(t, false, false, false)

	msg := IncomingMessage{
		Type:     "dm_message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: "not-a-uuid",
			keyContent:        "test",
		},
	}

	// Should return early without panic
	setup.hub.handleDMMessage(msg)
}

func TestHandleDMMessage_MissingConversationID(t *testing.T) {
	setup := setupEpochTest(t, false, false, false)

	msg := IncomingMessage{
		Type:     "dm_message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyContent: "test",
		},
	}

	// Should return early without panic
	setup.hub.handleDMMessage(msg)
}

func TestHandleDMMessage_SystemType(t *testing.T) {
	setup := setupEpochTest(t, false, false, false)

	msg := IncomingMessage{
		Type:     "dm_message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: setup.convID,
			keyContent:        "system message",
			"type":            "system",
		},
	}

	setup.hub.handleDMMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "dm_message_ack", resp["type"])
}

func TestHandleDMMessage_EncryptedLongerContentAllowed(t *testing.T) {
	setup := setupEpochTest(t, false, false, false)

	content := make([]byte, 5000)
	for i := range content {
		content[i] = 'a'
	}

	msg := IncomingMessage{
		Type:     "dm_message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: setup.convID,
			keyContent:        string(content),
			keyIsEncrypted:    true,
		},
	}

	setup.hub.handleDMMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "dm_message_ack", resp["type"])
}

func TestHandleDMMessage_BroadcastToSubscribers(t *testing.T) {
	setup := setupEpochTest(t, false, false, false)
	convUUID, _ := uuid.Parse(setup.convID)

	// Add user2 as a subscriber
	client2ID := uuid.New()
	client2 := &Client{
		ID:       client2ID,
		UserID:   setup.user2,
		Username: "hubuser2",
		Send:     make(chan []byte, 10),
		Hub:      setup.hub,
		Channels: make(map[uuid.UUID]bool),
	}
	setup.hub.clients[client2ID] = client2
	setup.hub.userClients[setup.user2] = map[uuid.UUID]bool{client2ID: true}
	setup.hub.dmSubscriptions[convUUID][client2ID] = true

	msg := IncomingMessage{
		Type:     "dm_message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: setup.convID,
			keyContent:        "dm broadcast test",
		},
	}

	setup.hub.handleDMMessage(msg)

	// Sender gets ack
	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "dm_message_ack", resp["type"])

	// Broadcast goes to dmBroadcast channel
	select {
	case dmMsg := <-setup.hub.dmBroadcast:
		assert.Equal(t, convUUID, dmMsg.ConversationID)
		assert.Equal(t, "dm_message", dmMsg.Data.Type)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected DM broadcast")
	}
}

// --- handleSetStatus with valid statuses (requires Redis) ---

func TestHandleSetStatus_Online(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)

	userID := uuid.New()
	client := &Client{
		ID:       uuid.New(),
		UserID:   userID,
		Username: "statususer",
		Send:     make(chan []byte, 10),
		Hub:      hub,
		Channels: make(map[uuid.UUID]bool),
	}
	hub.clients[client.ID] = client
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true}

	msg := IncomingMessage{
		Type:   "set_status",
		UserID: userID,
		Data: map[string]interface{}{
			keyStatus: "online",
		},
	}

	hub.handleSetStatus(msg)

	// Verify Redis was set
	val, err := redisClient.Get(context.Background(), "presence:"+userID.String()).Result()
	require.NoError(t, err)
	assert.Equal(t, "online", val)
}

func TestHandleSetStatus_DND(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)

	userID := uuid.New()
	client := &Client{
		ID:       uuid.New(),
		UserID:   userID,
		Send:     make(chan []byte, 10),
		Hub:      hub,
		Channels: make(map[uuid.UUID]bool),
	}
	hub.clients[client.ID] = client
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true}

	msg := IncomingMessage{
		Type:   "set_status",
		UserID: userID,
		Data: map[string]interface{}{
			keyStatus: "dnd",
		},
	}

	hub.handleSetStatus(msg)

	val, err := redisClient.Get(context.Background(), "presence:"+userID.String()).Result()
	require.NoError(t, err)
	assert.Equal(t, "dnd", val)
}

func TestHandleSetStatus_Invisible(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)

	userID := uuid.New()
	client := &Client{
		ID:       uuid.New(),
		UserID:   userID,
		Send:     make(chan []byte, 10),
		Hub:      hub,
		Channels: make(map[uuid.UUID]bool),
	}
	hub.clients[client.ID] = client
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true}

	msg := IncomingMessage{
		Type:   "set_status",
		UserID: userID,
		Data: map[string]interface{}{
			keyStatus: "invisible",
		},
	}

	hub.handleSetStatus(msg)

	// Invisible stores real status in Redis
	val, err := redisClient.Get(context.Background(), "presence:"+userID.String()).Result()
	require.NoError(t, err)
	assert.Equal(t, "invisible", val)

	// But broadcasts as offline
	select {
	case data := <-client.Send:
		var msg map[string]interface{}
		require.NoError(t, json.Unmarshal(data, &msg))
		assert.Equal(t, "presence", msg["type"])
		msgData := msg["data"].(map[string]interface{})
		assert.Equal(t, "offline", msgData["status"])
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected presence broadcast")
	}
}

// --- handleHeartbeat tests (requires Redis) ---

func TestHandleHeartbeat_RefreshesTTL(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)

	userID := uuid.New()
	hub.userClients[userID] = map[uuid.UUID]bool{uuid.New(): true}

	ctx := context.Background()
	key := "presence:" + userID.String()
	require.NoError(t, redisClient.Set(ctx, key, "online", 120*time.Second).Err())

	msg := IncomingMessage{
		Type:   "heartbeat",
		UserID: userID,
	}

	hub.handleHeartbeat(msg)

	ttl := redisClient.TTL(ctx, key).Val()
	assert.Greater(t, ttl, 100*time.Second)
}

func TestHandleHeartbeat_MissingKey_ResetsOnline(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)

	userID := uuid.New()
	hub.userClients[userID] = map[uuid.UUID]bool{uuid.New(): true}

	msg := IncomingMessage{
		Type:   "heartbeat",
		UserID: userID,
	}

	hub.handleHeartbeat(msg)

	ctx := context.Background()
	val, err := redisClient.Get(ctx, "presence:"+userID.String()).Result()
	require.NoError(t, err)
	assert.Equal(t, "online", val)
}

func TestHandleHeartbeat_DND_RefreshesWithoutChangingStatus(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)

	userID := uuid.New()
	hub.userClients[userID] = map[uuid.UUID]bool{uuid.New(): true}

	ctx := context.Background()
	key := "presence:" + userID.String()
	require.NoError(t, redisClient.Set(ctx, key, "dnd", 60*time.Second).Err())

	msg := IncomingMessage{
		Type:   "heartbeat",
		UserID: userID,
	}

	hub.handleHeartbeat(msg)

	// Status should remain dnd
	val, err := redisClient.Get(ctx, key).Result()
	require.NoError(t, err)
	assert.Equal(t, "dnd", val)
}

// --- handleProfileUpdate integration tests ---

func TestHandleProfileUpdate_Success(t *testing.T) {
	setup := setupMessageTest(t, false)

	// Update the user's display name in DB
	newDisplayName := "Updated Name"
	_, err := setup.db.Exec(`UPDATE users SET display_name = $1 WHERE id = $2`, newDisplayName, setup.user1.String())
	require.NoError(t, err)

	msg := IncomingMessage{
		Type:     "profile_update",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data:     map[string]interface{}{},
	}

	setup.hub.handleProfileUpdate(msg)

	// Client's cached DisplayName should be updated
	assert.Equal(t, newDisplayName, *setup.client.DisplayName)
}

func TestHandleProfileUpdate_ClientNotFound(t *testing.T) {
	setup := setupMessageTest(t, false)

	msg := IncomingMessage{
		Type:     "profile_update",
		UserID:   setup.user1,
		ClientID: uuid.New(),
		Data:     map[string]interface{}{},
	}

	// Should not panic
	setup.hub.handleProfileUpdate(msg)
}

// --- handleServerUpdate integration tests ---

func TestHandleServerUpdate_Success(t *testing.T) {
	setup := setupMessageTest(t, false)
	serverID := setup.user2 // reused field for serverID

	// Add client as server subscriber
	setup.hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{setup.client.ID: true}

	msg := IncomingMessage{
		Type:     "server_update",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyServerID: serverID.String(),
		},
	}

	setup.hub.handleServerUpdate(msg)

	// Should receive server_updated broadcast
	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "server_updated", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Equal(t, serverID.String(), data[keyServerID])
	assert.Equal(t, "Test Server", data["name"])
}

func TestHandleServerUpdate_InvalidServerID(t *testing.T) {
	setup := setupMessageTest(t, false)

	msg := IncomingMessage{
		Type:     "server_update",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyServerID: "not-a-uuid",
		},
	}

	// Should not panic
	setup.hub.handleServerUpdate(msg)
}

func TestHandleServerUpdate_MissingServerID(t *testing.T) {
	setup := setupMessageTest(t, false)

	msg := IncomingMessage{
		Type:     "server_update",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data:     map[string]interface{}{},
	}

	// Should not panic
	setup.hub.handleServerUpdate(msg)
}

func TestHandleServerUpdate_NonexistentServer(t *testing.T) {
	setup := setupMessageTest(t, false)

	fakeServerID := uuid.New()

	msg := IncomingMessage{
		Type:     "server_update",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyServerID: fakeServerID.String(),
		},
	}

	// Should not panic (DB query fails gracefully)
	setup.hub.handleServerUpdate(msg)
}

func TestHandleServerUpdate_NoSubscribers(t *testing.T) {
	setup := setupMessageTest(t, false)
	serverID := setup.user2

	// Remove all subscribers
	delete(setup.hub.serverSubscriptions, serverID)

	msg := IncomingMessage{
		Type:     "server_update",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyServerID: serverID.String(),
		},
	}

	// Should not panic
	setup.hub.handleServerUpdate(msg)
}

// --- handleIncoming routing tests (additional coverage) ---

// --- broadcastPresenceToAll tests ---

func TestBroadcastPresenceToAll(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)

	userID := uuid.New()
	client := &Client{
		ID:     uuid.New(),
		UserID: uuid.New(), // different user receiving the broadcast
		Send:   make(chan []byte, 10),
	}
	hub.clients[client.ID] = client

	hub.broadcastPresenceToAll(userID, "online", time.Now().Unix())

	resp := readClientMsg(t, client)
	assert.Equal(t, "presence", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Equal(t, userID.String(), data[keyUserID])
	assert.Equal(t, "online", data[keyStatus])
}

// --- sendDMUnreadNotify tests ---

func TestSendDMUnreadNotify_SendsToUnsubscribedParticipants(t *testing.T) {
	setup := setupEpochTest(t, false, false, false)
	convUUID, _ := uuid.Parse(setup.convID)

	// user2 is a participant but NOT subscribed to the DM
	client2ID := uuid.New()
	client2 := &Client{
		ID:       client2ID,
		UserID:   setup.user2,
		Username: "hubuser2",
		Send:     make(chan []byte, 10),
		Hub:      setup.hub,
		Channels: make(map[uuid.UUID]bool),
	}
	setup.hub.clients[client2ID] = client2
	setup.hub.userClients[setup.user2] = map[uuid.UUID]bool{client2ID: true}

	setup.hub.sendDMUnreadNotify(convUUID, setup.user1)

	select {
	case data := <-client2.Send:
		var msg map[string]interface{}
		require.NoError(t, json.Unmarshal(data, &msg))
		assert.Equal(t, "dm_unread_notify", msg["type"])
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected dm_unread_notify for unsubscribed participant")
	}
}

func TestSendDMUnreadNotify_SkipsSender(t *testing.T) {
	setup := setupEpochTest(t, false, false, false)
	convUUID, _ := uuid.Parse(setup.convID)

	// The sender (user1) should NOT receive unread notify
	// user1's client is already registered
	setup.hub.sendDMUnreadNotify(convUUID, setup.user1)

	select {
	case <-setup.client.Send:
		t.Fatal("sender should not receive dm_unread_notify")
	case <-time.After(100 * time.Millisecond):
		// Expected: no message for sender
	}
}

func TestSendDMUnreadNotify_SkipsSubscribedParticipants(t *testing.T) {
	setup := setupEpochTest(t, false, false, false)
	convUUID, _ := uuid.Parse(setup.convID)

	// user2 is a participant AND subscribed to the DM
	client2ID := uuid.New()
	client2 := &Client{
		ID:       client2ID,
		UserID:   setup.user2,
		Username: "hubuser2",
		Send:     make(chan []byte, 10),
		Hub:      setup.hub,
		Channels: make(map[uuid.UUID]bool),
	}
	setup.hub.clients[client2ID] = client2
	setup.hub.userClients[setup.user2] = map[uuid.UUID]bool{client2ID: true}
	setup.hub.dmSubscriptions[convUUID][client2ID] = true

	setup.hub.sendDMUnreadNotify(convUUID, setup.user1)

	select {
	case <-client2.Send:
		t.Fatal("subscribed participant should not receive dm_unread_notify")
	case <-time.After(100 * time.Millisecond):
		// Expected
	}
}

// --- DisconnectUser / DisconnectSession thread-safe wrappers ---

func TestDisconnectUser_QueuesOnChannel(t *testing.T) {
	hub := NewHub(nil, nil)
	userID := uuid.New()

	hub.DisconnectUser(userID)

	select {
	case received := <-hub.disconnectUser:
		assert.Equal(t, userID, received)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected user ID on disconnectUser channel")
	}
}

func TestDisconnectSession_QueuesOnChannel(t *testing.T) {
	hub := NewHub(nil, nil)

	hub.DisconnectSession("session-abc")

	select {
	case received := <-hub.disconnectSession:
		assert.Equal(t, "session-abc", received)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected session ID on disconnectSession channel")
	}
}

// --- Epoch enforcement in handleMessage ---

func TestHandleMessage_EpochRevoked(t *testing.T) {
	setup := setupMessageTest(t, true)
	channelUUID, _ := uuid.Parse(setup.convID)

	// Seed a channel key and revocation
	_, err := setup.db.Exec(`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version) VALUES ($1, $2, $3, 1)`,
		channelUUID.String(), setup.user1.String(), []byte("test-key"))
	require.NoError(t, err)

	_, err = setup.db.Exec(`INSERT INTO key_revocations (channel_id, revoked_epoch, successor_epoch, reason, revoked_by) VALUES ($1, 1, 2, 'test', $2)`,
		channelUUID.String(), setup.user1.String())
	require.NoError(t, err)

	_, err = setup.db.Exec(`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version) VALUES ($1, $2, $3, 2)`,
		channelUUID.String(), setup.user1.String(), []byte("test-key-v2"))
	require.NoError(t, err)

	msg := IncomingMessage{
		Type:     "message",
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyChannelID:   setup.convID,
			keyContent:     "encrypted with revoked epoch",
			keyIsEncrypted: true,
			keyKeyVersion:  float64(1),
		},
	}

	setup.hub.handleMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Equal(t, "epoch_revoked", data["code"])
	assert.Equal(t, float64(2), data["current_epoch"])
}
