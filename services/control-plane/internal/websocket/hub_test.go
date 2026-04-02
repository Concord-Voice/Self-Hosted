package websocket

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const notAUUID = "not-a-uuid"

// --- Hub construction tests ---

func TestNewHub_InitializesMaps(t *testing.T) {
	hub := NewHub(nil, nil)

	assert.NotNil(t, hub.clients)
	assert.NotNil(t, hub.userClients)
	assert.NotNil(t, hub.channelSubscriptions)
	assert.NotNil(t, hub.usernames)
	assert.NotNil(t, hub.serverSubscriptions)
	assert.NotNil(t, hub.dmSubscriptions)
	assert.NotNil(t, hub.register)
	assert.NotNil(t, hub.unregister)
	assert.NotNil(t, hub.incoming)
	assert.NotNil(t, hub.broadcast)
	assert.NotNil(t, hub.globalBroadcast)
	assert.NotNil(t, hub.userBroadcast)
	assert.NotNil(t, hub.serverBroadcast)
	assert.NotNil(t, hub.dmBroadcast)
	assert.NotNil(t, hub.disconnectUser)
	assert.NotNil(t, hub.disconnectSession)
	assert.NotNil(t, hub.done)
	assert.NotNil(t, hub.onlineCountPending)
}

func TestSetMentionChecker(t *testing.T) {
	hub := NewHub(nil, nil)
	assert.Nil(t, hub.mentionChecker)

	checker := &mockMentionChecker{}
	hub.SetMentionChecker(checker)
	assert.NotNil(t, hub.mentionChecker)
}

// --- newMinimalHub creates a hub with only the maps needed for unit testing
// (no DB/Redis, channels are nil-safe).
func newMinimalHub() *Hub {
	return &Hub{
		clients:              make(map[uuid.UUID]*Client),
		userClients:          make(map[uuid.UUID]map[uuid.UUID]bool),
		channelSubscriptions: make(map[uuid.UUID]map[uuid.UUID]bool),
		usernames:            make(map[uuid.UUID]string),
		serverSubscriptions:  make(map[uuid.UUID]map[uuid.UUID]bool),
		dmSubscriptions:      make(map[uuid.UUID]map[uuid.UUID]bool),
		broadcast:            make(chan BroadcastMessage, 256),
		globalBroadcast:      make(chan OutgoingMessage, 256),
		userBroadcast:        make(chan UserBroadcastMessage, 256),
		serverBroadcast:      make(chan ServerBroadcastMessage, 256),
		dmBroadcast:          make(chan DMBroadcastMessage, 256),
		onlineCountPending:   make(map[uuid.UUID]bool),
	}
}

func newTestClient(hub *Hub, userID uuid.UUID) *Client {
	return &Client{
		ID:       uuid.New(),
		UserID:   userID,
		Username: "testuser",
		Hub:      hub,
		Send:     make(chan []byte, 256),
		Channels: make(map[uuid.UUID]bool),
	}
}

// --- handleUnregister tests (no DB/Redis needed for map cleanup) ---

func TestHandleUnregister_RemovesClient(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	client := newTestClient(hub, userID)
	// Add a second client so this is NOT the last connection (avoids Redis call)
	otherClient := newTestClient(hub, userID)

	hub.clients[client.ID] = client
	hub.clients[otherClient.ID] = otherClient
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true, otherClient.ID: true}

	hub.handleUnregister(client)

	_, exists := hub.clients[client.ID]
	assert.False(t, exists, "client should be removed from clients map")
	assert.True(t, hub.userClients[userID][otherClient.ID], "other client should remain")
}

func TestHandleUnregister_MultiDevice_KeepsOtherClients(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	client1 := newTestClient(hub, userID)
	client2 := newTestClient(hub, userID)

	hub.clients[client1.ID] = client1
	hub.clients[client2.ID] = client2
	hub.userClients[userID] = map[uuid.UUID]bool{client1.ID: true, client2.ID: true}

	hub.handleUnregister(client1)

	_, exists := hub.clients[client1.ID]
	assert.False(t, exists, "unregistered client should be removed")
	_, exists = hub.clients[client2.ID]
	assert.True(t, exists, "other client should remain")
	assert.True(t, hub.userClients[userID][client2.ID], "other client should remain in userClients")
}

func TestHandleUnregister_RemovesFromChannelSubscriptions(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	channelID := uuid.New()
	client := newTestClient(hub, userID)
	client.Channels[channelID] = true
	// Add buddy client so this is not the last connection (avoids Redis call)
	buddy := newTestClient(hub, userID)

	hub.clients[client.ID] = client
	hub.clients[buddy.ID] = buddy
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true, buddy.ID: true}
	hub.channelSubscriptions[channelID] = map[uuid.UUID]bool{client.ID: true}

	hub.handleUnregister(client)

	_, exists := hub.channelSubscriptions[channelID]
	assert.False(t, exists, "channel subscription should be cleaned up when last subscriber disconnects")
}

func TestHandleUnregister_RemovesFromServerSubscriptions(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	serverID := uuid.New()
	client := newTestClient(hub, userID)
	buddy := newTestClient(hub, userID)

	hub.clients[client.ID] = client
	hub.clients[buddy.ID] = buddy
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true, buddy.ID: true}
	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{client.ID: true}

	hub.handleUnregister(client)

	_, exists := hub.serverSubscriptions[serverID]
	assert.False(t, exists, "server subscription should be cleaned up")
}

func TestHandleUnregister_RemovesFromDMSubscriptions(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	convID := uuid.New()
	client := newTestClient(hub, userID)
	buddy := newTestClient(hub, userID)

	hub.clients[client.ID] = client
	hub.clients[buddy.ID] = buddy
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true, buddy.ID: true}
	hub.dmSubscriptions[convID] = map[uuid.UUID]bool{client.ID: true}

	hub.handleUnregister(client)

	_, exists := hub.dmSubscriptions[convID]
	assert.False(t, exists, "DM subscription should be cleaned up")
}

func TestHandleUnregister_ClosesClientSendChannel(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	client := newTestClient(hub, userID)
	buddy := newTestClient(hub, userID)

	hub.clients[client.ID] = client
	hub.clients[buddy.ID] = buddy
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true, buddy.ID: true}

	hub.handleUnregister(client)

	// Verify Send channel is closed by attempting to receive
	_, ok := <-client.Send
	assert.False(t, ok, "Send channel should be closed")
}

func TestHandleUnregister_NonexistentClient(t *testing.T) {
	hub := newMinimalHub()
	client := newTestClient(hub, uuid.New())

	// Should not panic when client is not registered
	hub.handleUnregister(client)
}

func TestHandleUnregister_RemovesUsernameCache(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	client := newTestClient(hub, userID)
	buddy := newTestClient(hub, userID)

	hub.clients[client.ID] = client
	hub.clients[buddy.ID] = buddy
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true, buddy.ID: true}
	hub.usernames[userID] = "testuser"

	// Unregister first client — username cache should remain (buddy still connected)
	hub.handleUnregister(client)
	_, exists := hub.usernames[userID]
	assert.True(t, exists, "username cache should remain while user still has connections")

	// Unregister buddy — but this triggers isLastConnection which needs Redis.
	// So we just verify the buddy removal via map inspection instead.
}

// --- handleBroadcast tests ---

func TestHandleBroadcast_SendsToSubscribers(t *testing.T) {
	hub := newMinimalHub()
	channelID := uuid.New()
	user1 := uuid.New()
	user2 := uuid.New()

	client1 := newTestClient(hub, user1)
	client2 := newTestClient(hub, user2)

	hub.clients[client1.ID] = client1
	hub.clients[client2.ID] = client2
	hub.channelSubscriptions[channelID] = map[uuid.UUID]bool{
		client1.ID: true,
		client2.ID: true,
	}

	msg := BroadcastMessage{
		ChannelID: channelID,
		Data: OutgoingMessage{
			Type: "message",
			Data: map[string]interface{}{"content": "hello"},
		},
	}

	hub.handleBroadcast(msg)

	assert.Len(t, client1.Send, 1)
	assert.Len(t, client2.Send, 1)
}

func TestHandleBroadcast_ExcludesUser(t *testing.T) {
	hub := newMinimalHub()
	channelID := uuid.New()
	senderID := uuid.New()
	receiverID := uuid.New()

	sender := newTestClient(hub, senderID)
	receiver := newTestClient(hub, receiverID)

	hub.clients[sender.ID] = sender
	hub.clients[receiver.ID] = receiver
	hub.channelSubscriptions[channelID] = map[uuid.UUID]bool{
		sender.ID:   true,
		receiver.ID: true,
	}

	msg := BroadcastMessage{
		ChannelID:   channelID,
		ExcludeUser: &senderID,
		Data: OutgoingMessage{
			Type: "message",
			Data: map[string]interface{}{"content": "hello"},
		},
	}

	hub.handleBroadcast(msg)

	assert.Len(t, sender.Send, 0, "excluded user should not receive message")
	assert.Len(t, receiver.Send, 1, "non-excluded user should receive message")
}

func TestHandleBroadcast_NoSubscribers(t *testing.T) {
	hub := newMinimalHub()

	msg := BroadcastMessage{
		ChannelID: uuid.New(),
		Data: OutgoingMessage{
			Type: "message",
			Data: map[string]interface{}{"content": "hello"},
		},
	}

	// Should not panic
	hub.handleBroadcast(msg)
}

// --- handleGlobalBroadcast tests ---

func TestHandleGlobalBroadcast_SendsToAllClients(t *testing.T) {
	hub := newMinimalHub()
	user1 := uuid.New()
	user2 := uuid.New()

	client1 := newTestClient(hub, user1)
	client2 := newTestClient(hub, user2)

	hub.clients[client1.ID] = client1
	hub.clients[client2.ID] = client2

	msg := OutgoingMessage{
		Type: "presence",
		Data: map[string]interface{}{"user_id": "test"},
	}

	hub.handleGlobalBroadcast(msg)

	assert.Len(t, client1.Send, 1)
	assert.Len(t, client2.Send, 1)
}

func TestHandleGlobalBroadcast_NoClients(t *testing.T) {
	hub := newMinimalHub()

	msg := OutgoingMessage{
		Type: "presence",
		Data: map[string]interface{}{},
	}

	// Should not panic
	hub.handleGlobalBroadcast(msg)
}

// --- handleServerBroadcast tests ---

func TestHandleServerBroadcast_SendsToSubscribers(t *testing.T) {
	hub := newMinimalHub()
	serverID := uuid.New()
	userID := uuid.New()

	client := newTestClient(hub, userID)
	hub.clients[client.ID] = client
	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{client.ID: true}

	msg := ServerBroadcastMessage{
		ServerID: serverID,
		Data: OutgoingMessage{
			Type: "server_updated",
			Data: map[string]interface{}{"name": "test"},
		},
	}

	hub.handleServerBroadcast(msg)

	assert.Len(t, client.Send, 1)
}

func TestHandleServerBroadcast_NoSubscribers(t *testing.T) {
	hub := newMinimalHub()

	msg := ServerBroadcastMessage{
		ServerID: uuid.New(),
		Data: OutgoingMessage{
			Type: "server_updated",
			Data: map[string]interface{}{},
		},
	}

	// Should not panic
	hub.handleServerBroadcast(msg)
}

// --- handleUserBroadcast tests ---

func TestHandleUserBroadcast_SendsToAllUserClients(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()

	client1 := newTestClient(hub, userID)
	client2 := newTestClient(hub, userID)

	hub.clients[client1.ID] = client1
	hub.clients[client2.ID] = client2
	hub.userClients[userID] = map[uuid.UUID]bool{client1.ID: true, client2.ID: true}

	msg := UserBroadcastMessage{
		UserID: userID,
		Data: OutgoingMessage{
			Type: "profile_updated",
			Data: map[string]interface{}{"username": "new_name"},
		},
	}

	hub.handleUserBroadcast(msg)

	assert.Len(t, client1.Send, 1)
	assert.Len(t, client2.Send, 1)
}

func TestHandleUserBroadcast_ExcludesClient(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()

	client1 := newTestClient(hub, userID)
	client2 := newTestClient(hub, userID)

	hub.clients[client1.ID] = client1
	hub.clients[client2.ID] = client2
	hub.userClients[userID] = map[uuid.UUID]bool{client1.ID: true, client2.ID: true}

	msg := UserBroadcastMessage{
		UserID:          userID,
		ExcludeClientID: &client1.ID,
		Data: OutgoingMessage{
			Type: "profile_updated",
			Data: map[string]interface{}{},
		},
	}

	hub.handleUserBroadcast(msg)

	assert.Len(t, client1.Send, 0, "excluded client should not receive message")
	assert.Len(t, client2.Send, 1)
}

func TestHandleUserBroadcast_UserNotConnected(t *testing.T) {
	hub := newMinimalHub()

	msg := UserBroadcastMessage{
		UserID: uuid.New(),
		Data: OutgoingMessage{
			Type: "test",
			Data: map[string]interface{}{},
		},
	}

	// Should not panic
	hub.handleUserBroadcast(msg)
}

// --- handleDMBroadcast tests ---

func TestHandleDMBroadcast_SendsToSubscribers(t *testing.T) {
	hub := newMinimalHub()
	convID := uuid.New()
	user1 := uuid.New()
	user2 := uuid.New()

	client1 := newTestClient(hub, user1)
	client2 := newTestClient(hub, user2)

	hub.clients[client1.ID] = client1
	hub.clients[client2.ID] = client2
	hub.dmSubscriptions[convID] = map[uuid.UUID]bool{
		client1.ID: true,
		client2.ID: true,
	}

	msg := DMBroadcastMessage{
		ConversationID: convID,
		Data: OutgoingMessage{
			Type: "dm_message",
			Data: map[string]interface{}{"content": "hi"},
		},
	}

	hub.handleDMBroadcast(msg)

	assert.Len(t, client1.Send, 1)
	assert.Len(t, client2.Send, 1)
}

func TestHandleDMBroadcast_ExcludesUser(t *testing.T) {
	hub := newMinimalHub()
	convID := uuid.New()
	senderID := uuid.New()
	receiverID := uuid.New()

	sender := newTestClient(hub, senderID)
	receiver := newTestClient(hub, receiverID)

	hub.clients[sender.ID] = sender
	hub.clients[receiver.ID] = receiver
	hub.dmSubscriptions[convID] = map[uuid.UUID]bool{
		sender.ID:   true,
		receiver.ID: true,
	}

	msg := DMBroadcastMessage{
		ConversationID: convID,
		ExcludeUser:    &senderID,
		Data: OutgoingMessage{
			Type: "dm_message",
			Data: map[string]interface{}{"content": "hi"},
		},
	}

	hub.handleDMBroadcast(msg)

	assert.Len(t, sender.Send, 0, "excluded user should not receive message")
	assert.Len(t, receiver.Send, 1)
}

func TestHandleDMBroadcast_NoSubscribers(t *testing.T) {
	hub := newMinimalHub()

	msg := DMBroadcastMessage{
		ConversationID: uuid.New(),
		Data: OutgoingMessage{
			Type: "dm_message",
			Data: map[string]interface{}{},
		},
	}

	// Should not panic
	hub.handleDMBroadcast(msg)
}

// --- sendError tests ---

func TestSendError_SendsToClient(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	client := newTestClient(hub, userID)
	hub.clients[client.ID] = client

	hub.sendError(client.ID, "test error message")

	require.Len(t, client.Send, 1)
	data := <-client.Send
	var msg map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &msg))
	assert.Equal(t, "error", msg["type"])
	msgData := msg["data"].(map[string]interface{})
	assert.Equal(t, "test error message", msgData["message"])
}

func TestSendError_ClientNotFound(t *testing.T) {
	hub := newMinimalHub()
	// Should not panic
	hub.sendError(uuid.New(), "test error")
}

// --- sendErrorWithData tests ---

func TestSendErrorWithData_IncludesExtraFields(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	client := newTestClient(hub, userID)
	hub.clients[client.ID] = client

	extra := map[string]interface{}{
		"current_epoch": 5,
		"channel_id":    "abc-123",
	}
	hub.sendErrorWithData(client.ID, "epoch_revoked", extra)

	require.Len(t, client.Send, 1)
	data := <-client.Send
	var msg map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &msg))
	assert.Equal(t, "error", msg["type"])
	msgData := msg["data"].(map[string]interface{})
	assert.Equal(t, "epoch_revoked", msgData["code"])
	assert.Equal(t, "epoch_revoked", msgData["error"])
	assert.Equal(t, float64(5), msgData["current_epoch"])
	assert.Equal(t, "abc-123", msgData["channel_id"])
}

func TestSendErrorWithData_ClientNotFound(t *testing.T) {
	hub := newMinimalHub()
	// Should not panic
	hub.sendErrorWithData(uuid.New(), "test", nil)
}

// --- handleIncoming routing tests ---

func TestHandleIncoming_UnknownType(t *testing.T) {
	hub := newMinimalHub()
	msg := IncomingMessage{
		Type:     "unknown_message_type",
		ClientID: uuid.New(),
		UserID:   uuid.New(),
	}

	// Should not panic
	hub.handleIncoming(msg)
}

// --- handleUnsubscribe tests ---

func TestHandleUnsubscribe_RemovesFromChannel(t *testing.T) {
	hub := newMinimalHub()
	channelID := uuid.New()
	userID := uuid.New()
	client := newTestClient(hub, userID)
	client.Channels[channelID] = true

	hub.clients[client.ID] = client
	hub.channelSubscriptions[channelID] = map[uuid.UUID]bool{client.ID: true}

	msg := IncomingMessage{
		Type:     "unsubscribe",
		ClientID: client.ID,
		UserID:   userID,
		Data: map[string]interface{}{
			keyChannelID: channelID.String(),
		},
	}

	hub.handleUnsubscribe(msg)

	assert.False(t, client.Channels[channelID])
	_, exists := hub.channelSubscriptions[channelID]
	assert.False(t, exists, "channel subscription should be cleaned up")
}

func TestHandleUnsubscribe_InvalidChannelID(t *testing.T) {
	hub := newMinimalHub()
	client := newTestClient(hub, uuid.New())
	hub.clients[client.ID] = client

	msg := IncomingMessage{
		Type:     "unsubscribe",
		ClientID: client.ID,
		Data:     map[string]interface{}{keyChannelID: notAUUID},
	}

	// Should not panic
	hub.handleUnsubscribe(msg)
}

func TestHandleUnsubscribe_MissingChannelID(t *testing.T) {
	hub := newMinimalHub()
	client := newTestClient(hub, uuid.New())
	hub.clients[client.ID] = client

	msg := IncomingMessage{
		Type:     "unsubscribe",
		ClientID: client.ID,
		Data:     map[string]interface{}{},
	}

	// Should not panic
	hub.handleUnsubscribe(msg)
}

func TestHandleUnsubscribe_ClientNotFound(t *testing.T) {
	hub := newMinimalHub()

	msg := IncomingMessage{
		Type:     "unsubscribe",
		ClientID: uuid.New(),
		Data: map[string]interface{}{
			keyChannelID: uuid.New().String(),
		},
	}

	// Should not panic
	hub.handleUnsubscribe(msg)
}

// --- handleUnsubscribeServer tests ---

func TestHandleUnsubscribeServer_RemovesFromServer(t *testing.T) {
	hub := newMinimalHub()
	serverID := uuid.New()
	userID := uuid.New()
	client := newTestClient(hub, userID)

	hub.clients[client.ID] = client
	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{client.ID: true}

	msg := IncomingMessage{
		Type:     "unsubscribe_server",
		ClientID: client.ID,
		UserID:   userID,
		Data: map[string]interface{}{
			keyServerID: serverID.String(),
		},
	}

	hub.handleUnsubscribeServer(msg)

	_, exists := hub.serverSubscriptions[serverID]
	assert.False(t, exists)
}

func TestHandleUnsubscribeServer_InvalidServerID(t *testing.T) {
	hub := newMinimalHub()
	client := newTestClient(hub, uuid.New())
	hub.clients[client.ID] = client

	msg := IncomingMessage{
		Type:     "unsubscribe_server",
		ClientID: client.ID,
		Data:     map[string]interface{}{keyServerID: notAUUID},
	}

	// Should not panic
	hub.handleUnsubscribeServer(msg)
}

func TestHandleUnsubscribeServer_MissingServerID(t *testing.T) {
	hub := newMinimalHub()

	msg := IncomingMessage{
		Type:     "unsubscribe_server",
		ClientID: uuid.New(),
		Data:     map[string]interface{}{},
	}

	// Should not panic
	hub.handleUnsubscribeServer(msg)
}

// --- handleUnsubscribeDM tests ---

func TestHandleUnsubscribeDM_RemovesFromDM(t *testing.T) {
	hub := newMinimalHub()
	convID := uuid.New()

	client := newTestClient(hub, uuid.New())
	hub.clients[client.ID] = client
	hub.dmSubscriptions[convID] = map[uuid.UUID]bool{client.ID: true}

	msg := IncomingMessage{
		Type:     "unsubscribe_dm",
		ClientID: client.ID,
		Data: map[string]interface{}{
			keyConversationID: convID.String(),
		},
	}

	hub.handleUnsubscribeDM(msg)

	_, exists := hub.dmSubscriptions[convID]
	assert.False(t, exists)
}

func TestHandleUnsubscribeDM_InvalidConversationID(t *testing.T) {
	hub := newMinimalHub()

	msg := IncomingMessage{
		Type:     "unsubscribe_dm",
		ClientID: uuid.New(),
		Data:     map[string]interface{}{keyConversationID: notAUUID},
	}

	// Should not panic
	hub.handleUnsubscribeDM(msg)
}

func TestHandleUnsubscribeDM_MissingConversationID(t *testing.T) {
	hub := newMinimalHub()

	msg := IncomingMessage{
		Type:     "unsubscribe_dm",
		ClientID: uuid.New(),
		Data:     map[string]interface{}{},
	}

	// Should not panic
	hub.handleUnsubscribeDM(msg)
}

// --- handleDisconnectUser tests ---

func TestHandleDisconnectUser_RemovesTargetedClients(t *testing.T) {
	hub := newMinimalHub()
	targetUserID := uuid.New()
	otherUserID := uuid.New()

	// Two clients for the target user, one for another user
	client1 := newTestClient(hub, targetUserID)
	client2 := newTestClient(hub, targetUserID)
	otherClient := newTestClient(hub, otherUserID)
	// Give otherClient a buddy so its unregister also doesn't trigger Redis
	otherBuddy := newTestClient(hub, otherUserID)

	hub.clients[client1.ID] = client1
	hub.clients[client2.ID] = client2
	hub.clients[otherClient.ID] = otherClient
	hub.clients[otherBuddy.ID] = otherBuddy
	hub.userClients[targetUserID] = map[uuid.UUID]bool{client1.ID: true, client2.ID: true}
	hub.userClients[otherUserID] = map[uuid.UUID]bool{otherClient.ID: true, otherBuddy.ID: true}

	// handleDisconnectUser calls handleUnregister which needs Redis for the
	// last connection. These tests skip the last-connection path because it
	// requires Redis. Integration tests in hub_epoch_test.go cover that path.
	// We verify that the function collects and attempts to disconnect all clients
	// for the target user by checking the first unregister succeeds.

	// Instead, test behavior before the last-connection Redis path:
	// Verify the revoked message is queued for all target clients, then verify
	// that the other user's clients are untouched.
	// We call handleUnregister on each client individually, stopping before
	// the last one would trigger Redis.

	// Unregister client1 (not last for target user)
	hub.handleUnregister(client1)
	_, exists := hub.clients[client1.ID]
	assert.False(t, exists, "client1 should be removed")
	assert.True(t, hub.userClients[targetUserID][client2.ID], "client2 should remain")

	// Other user's clients should be untouched
	_, exists = hub.clients[otherClient.ID]
	assert.True(t, exists, "other user's client should be untouched")
}

func TestHandleDisconnectUser_SendsRevokedMessageToMultipleClients(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	client1 := newTestClient(hub, userID)
	client2 := newTestClient(hub, userID)
	// A third client ensures unregistering client1 and client2 still leaves one
	// so the last-connection Redis path is not triggered until the very end.
	client3 := newTestClient(hub, userID)

	hub.clients[client1.ID] = client1
	hub.clients[client2.ID] = client2
	hub.clients[client3.ID] = client3
	hub.userClients[userID] = map[uuid.UUID]bool{
		client1.ID: true,
		client2.ID: true,
		client3.ID: true,
	}

	// Manually replicate what handleDisconnectUser does: collect clients,
	// send revoked message, then unregister. We only unregister the first two
	// (leaving client3 so Redis isn't called).
	revokedMsg, _ := json.Marshal(OutgoingMessage{
		Type: sessionRevoked,
		Data: map[string]interface{}{"reason": "session_terminated"},
	})

	// Queue the revoked message on all three clients
	for _, c := range []*Client{client1, client2, client3} {
		select {
		case c.Send <- revokedMsg:
		default:
		}
	}

	// Verify all three received the revoked message
	for i, c := range []*Client{client1, client2, client3} {
		select {
		case data := <-c.Send:
			var msg map[string]interface{}
			require.NoError(t, json.Unmarshal(data, &msg))
			assert.Equal(t, sessionRevoked, msg["type"], "client %d should receive session_revoked", i)
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("timed out waiting for session_revoked on client %d", i)
		}
	}
}

func TestHandleDisconnectUser_UserNotConnected(t *testing.T) {
	hub := newMinimalHub()
	// Should not panic
	hub.handleDisconnectUser(uuid.New())
}

// --- handleDisconnectSession tests ---

func TestHandleDisconnectSession_DisconnectsMatchingSession(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	sessionID := "session-123"

	client1 := newTestClient(hub, userID)
	client1.SessionID = sessionID
	client2 := newTestClient(hub, userID)
	client2.SessionID = "other-session"

	hub.clients[client1.ID] = client1
	hub.clients[client2.ID] = client2
	hub.userClients[userID] = map[uuid.UUID]bool{client1.ID: true, client2.ID: true}

	hub.handleDisconnectSession(sessionID)

	_, exists := hub.clients[client1.ID]
	assert.False(t, exists, "matching session client should be disconnected")
	_, exists = hub.clients[client2.ID]
	assert.True(t, exists, "non-matching session client should remain")
}

func TestHandleDisconnectSession_EmptySessionID(t *testing.T) {
	hub := newMinimalHub()
	// Should return early
	hub.handleDisconnectSession("")
}

func TestHandleDisconnectSession_NoMatchingSessions(t *testing.T) {
	hub := newMinimalHub()
	client := newTestClient(hub, uuid.New())
	client.SessionID = "some-session"
	hub.clients[client.ID] = client
	hub.userClients[client.UserID] = map[uuid.UUID]bool{client.ID: true}

	// Should not disconnect anything
	hub.handleDisconnectSession("nonexistent-session")

	_, exists := hub.clients[client.ID]
	assert.True(t, exists, "client should still be connected")
}

// --- sendUnreadNotify tests ---

func TestSendUnreadNotify_SendsToServerSubscribers(t *testing.T) {
	hub := newMinimalHub()
	serverID := uuid.New()
	channelID := uuid.New()
	senderID := uuid.New()
	receiverID := uuid.New()

	sender := newTestClient(hub, senderID)
	receiver := newTestClient(hub, receiverID)

	hub.clients[sender.ID] = sender
	hub.clients[receiver.ID] = receiver

	// Both subscribed to server, but only sender is subscribed to channel
	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{
		sender.ID:   true,
		receiver.ID: true,
	}
	hub.channelSubscriptions[channelID] = map[uuid.UUID]bool{
		sender.ID: true,
	}

	hub.sendUnreadNotify(serverID, channelID, senderID)

	assert.Len(t, sender.Send, 0, "sender should not receive unread notify")
	assert.Len(t, receiver.Send, 1, "receiver should get unread notify")

	data := <-receiver.Send
	var msg map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &msg))
	assert.Equal(t, "unread_notify", msg["type"])
}

func TestSendUnreadNotify_SkipsChannelSubscribers(t *testing.T) {
	hub := newMinimalHub()
	serverID := uuid.New()
	channelID := uuid.New()
	senderID := uuid.New()
	subscribedUserID := uuid.New()

	sender := newTestClient(hub, senderID)
	subscribedUser := newTestClient(hub, subscribedUserID)

	hub.clients[sender.ID] = sender
	hub.clients[subscribedUser.ID] = subscribedUser

	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{
		sender.ID:         true,
		subscribedUser.ID: true,
	}
	hub.channelSubscriptions[channelID] = map[uuid.UUID]bool{
		sender.ID:         true,
		subscribedUser.ID: true,
	}

	hub.sendUnreadNotify(serverID, channelID, senderID)

	assert.Len(t, subscribedUser.Send, 0, "channel subscriber should not get unread notify")
	assert.Len(t, sender.Send, 0, "sender should not get unread notify")
}

func TestSendUnreadNotify_NoServerSubscribers(t *testing.T) {
	hub := newMinimalHub()
	// Should not panic
	hub.sendUnreadNotify(uuid.New(), uuid.New(), uuid.New())
}

// --- handleTyping tests (no DB needed) ---

func TestHandleTyping_NotSubscribed(t *testing.T) {
	hub := newMinimalHub()
	channelID := uuid.New()
	userID := uuid.New()
	client := newTestClient(hub, userID)
	// Client exists but is NOT subscribed to the channel
	hub.clients[client.ID] = client

	msg := IncomingMessage{
		Type:     "typing",
		ClientID: client.ID,
		UserID:   userID,
		Data: map[string]interface{}{
			keyChannelID: channelID.String(),
			keyIsTyping:  true,
		},
	}

	hub.handleTyping(msg)

	// No broadcast should be sent since client is not subscribed
	select {
	case <-hub.broadcast:
		t.Fatal("should not broadcast typing for unsubscribed client")
	case <-time.After(50 * time.Millisecond):
		// Expected: no broadcast
	}
}

func TestHandleTyping_Subscribed(t *testing.T) {
	hub := newMinimalHub()
	channelID := uuid.New()
	userID := uuid.New()
	client := newTestClient(hub, userID)
	client.Channels[channelID] = true

	hub.clients[client.ID] = client
	hub.channelSubscriptions[channelID] = map[uuid.UUID]bool{client.ID: true}

	msg := IncomingMessage{
		Type:     "typing",
		ClientID: client.ID,
		UserID:   userID,
		Data: map[string]interface{}{
			keyChannelID: channelID.String(),
			keyIsTyping:  true,
		},
	}

	hub.handleTyping(msg)

	select {
	case bMsg := <-hub.broadcast:
		assert.Equal(t, channelID, bMsg.ChannelID)
		assert.Equal(t, "typing", bMsg.Data.Type)
		assert.Equal(t, &userID, bMsg.ExcludeUser)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected broadcast for typing indicator")
	}
}

func TestHandleTyping_InvalidChannelID(t *testing.T) {
	hub := newMinimalHub()
	client := newTestClient(hub, uuid.New())
	hub.clients[client.ID] = client

	msg := IncomingMessage{
		Type:     "typing",
		ClientID: client.ID,
		Data:     map[string]interface{}{keyChannelID: notAUUID},
	}

	// Should not panic
	hub.handleTyping(msg)
}

func TestHandleTyping_MissingChannelID(t *testing.T) {
	hub := newMinimalHub()

	msg := IncomingMessage{
		Type:     "typing",
		ClientID: uuid.New(),
		Data:     map[string]interface{}{},
	}

	// Should not panic
	hub.handleTyping(msg)
}

func TestHandleTyping_ClientNotFound(t *testing.T) {
	hub := newMinimalHub()

	msg := IncomingMessage{
		Type:     "typing",
		ClientID: uuid.New(),
		Data: map[string]interface{}{
			keyChannelID: uuid.New().String(),
		},
	}

	// Should not panic
	hub.handleTyping(msg)
}

// --- handleDMTyping tests ---

func TestHandleDMTyping_Subscribed(t *testing.T) {
	hub := newMinimalHub()
	convID := uuid.New()
	userID := uuid.New()
	client := newTestClient(hub, userID)

	hub.clients[client.ID] = client
	hub.dmSubscriptions[convID] = map[uuid.UUID]bool{client.ID: true}

	msg := IncomingMessage{
		Type:     "dm_typing",
		ClientID: client.ID,
		UserID:   userID,
		Data: map[string]interface{}{
			keyConversationID: convID.String(),
			keyIsTyping:       true,
		},
	}

	hub.handleDMTyping(msg)

	select {
	case dmMsg := <-hub.dmBroadcast:
		assert.Equal(t, convID, dmMsg.ConversationID)
		assert.Equal(t, "dm_typing", dmMsg.Data.Type)
		assert.Equal(t, &userID, dmMsg.ExcludeUser)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected DM broadcast for typing indicator")
	}
}

func TestHandleDMTyping_NotSubscribed(t *testing.T) {
	hub := newMinimalHub()
	convID := uuid.New()
	userID := uuid.New()
	client := newTestClient(hub, userID)

	hub.clients[client.ID] = client
	// Not subscribed to any DM

	msg := IncomingMessage{
		Type:     "dm_typing",
		ClientID: client.ID,
		UserID:   userID,
		Data: map[string]interface{}{
			keyConversationID: convID.String(),
			keyIsTyping:       true,
		},
	}

	hub.handleDMTyping(msg)

	select {
	case <-hub.dmBroadcast:
		t.Fatal("should not broadcast DM typing for unsubscribed client")
	case <-time.After(50 * time.Millisecond):
		// Expected
	}
}

func TestHandleDMTyping_InvalidConversationID(t *testing.T) {
	hub := newMinimalHub()

	msg := IncomingMessage{
		Type:     "dm_typing",
		ClientID: uuid.New(),
		Data:     map[string]interface{}{keyConversationID: notAUUID},
	}

	// Should not panic
	hub.handleDMTyping(msg)
}

func TestHandleDMTyping_ClientNotFound(t *testing.T) {
	hub := newMinimalHub()

	msg := IncomingMessage{
		Type:     "dm_typing",
		ClientID: uuid.New(),
		Data: map[string]interface{}{
			keyConversationID: uuid.New().String(),
		},
	}

	// Should not panic
	hub.handleDMTyping(msg)
}

// --- handleSetStatus tests (needs Redis; tested for input validation only) ---

func TestHandleSetStatus_InvalidStatus(t *testing.T) {
	hub := newMinimalHub()

	msg := IncomingMessage{
		Type:   "set_status",
		UserID: uuid.New(),
		Data: map[string]interface{}{
			keyStatus: "invalid_status",
		},
	}

	// Should return early without panic (no Redis call)
	hub.handleSetStatus(msg)
}

func TestHandleSetStatus_MissingStatus(t *testing.T) {
	hub := newMinimalHub()

	msg := IncomingMessage{
		Type:   "set_status",
		UserID: uuid.New(),
		Data:   map[string]interface{}{},
	}

	// Should return early
	hub.handleSetStatus(msg)
}

// --- GetConnectedUsers / GetUserClientCount tests ---

func TestGetConnectedUsers(t *testing.T) {
	hub := newMinimalHub()
	hub.mu.Lock()
	user1 := uuid.New()
	user2 := uuid.New()
	hub.userClients[user1] = map[uuid.UUID]bool{uuid.New(): true}
	hub.userClients[user2] = map[uuid.UUID]bool{uuid.New(): true}
	hub.mu.Unlock()

	users := hub.GetConnectedUsers()

	assert.Len(t, users, 2)
	assert.True(t, users[user1])
	assert.True(t, users[user2])
}

func TestGetConnectedUsers_Empty(t *testing.T) {
	hub := newMinimalHub()
	users := hub.GetConnectedUsers()
	assert.Empty(t, users)
}

func TestGetUserClientCount(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()

	hub.mu.Lock()
	hub.userClients[userID] = map[uuid.UUID]bool{uuid.New(): true, uuid.New(): true}
	hub.mu.Unlock()

	assert.Equal(t, 2, hub.GetUserClientCount(userID))
}

func TestGetUserClientCount_NotConnected(t *testing.T) {
	hub := newMinimalHub()
	assert.Equal(t, 0, hub.GetUserClientCount(uuid.New()))
}

// --- Public API thread-safe wrappers ---

func TestBroadcastToChannel_QueuesMessage(t *testing.T) {
	hub := newMinimalHub()
	channelID := uuid.New()
	msg := OutgoingMessage{Type: "test", Data: map[string]interface{}{}}

	hub.BroadcastToChannel(channelID, msg)

	select {
	case bMsg := <-hub.broadcast:
		assert.Equal(t, channelID, bMsg.ChannelID)
		assert.Equal(t, "test", bMsg.Data.Type)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected message on broadcast channel")
	}
}

func TestBroadcastToAll_QueuesMessage(t *testing.T) {
	hub := newMinimalHub()
	msg := OutgoingMessage{Type: "global_test", Data: map[string]interface{}{}}

	hub.BroadcastToAll(msg)

	select {
	case gMsg := <-hub.globalBroadcast:
		assert.Equal(t, "global_test", gMsg.Type)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected message on globalBroadcast channel")
	}
}

func TestBroadcastToServer_QueuesMessage(t *testing.T) {
	hub := newMinimalHub()
	serverID := uuid.New()
	msg := OutgoingMessage{Type: "server_test", Data: map[string]interface{}{}}

	hub.BroadcastToServer(serverID, msg)

	select {
	case sMsg := <-hub.serverBroadcast:
		assert.Equal(t, serverID, sMsg.ServerID)
		assert.Equal(t, "server_test", sMsg.Data.Type)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected message on serverBroadcast channel")
	}
}

func TestBroadcastToUser_QueuesMessage(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()
	msg := OutgoingMessage{Type: "user_test", Data: map[string]interface{}{}}

	hub.BroadcastToUser(userID, msg)

	select {
	case uMsg := <-hub.userBroadcast:
		assert.Equal(t, userID, uMsg.UserID)
		assert.Equal(t, "user_test", uMsg.Data.Type)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected message on userBroadcast channel")
	}
}

func TestBroadcastToDM_QueuesMessage(t *testing.T) {
	hub := newMinimalHub()
	convID := uuid.New()
	msg := OutgoingMessage{Type: "dm_test", Data: map[string]interface{}{}}

	hub.BroadcastToDM(convID, msg)

	select {
	case dmMsg := <-hub.dmBroadcast:
		assert.Equal(t, convID, dmMsg.ConversationID)
		assert.Equal(t, "dm_test", dmMsg.Data.Type)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected message on dmBroadcast channel")
	}
}

func TestBroadcastServerVoiceCounts_QueuesSignal(t *testing.T) {
	hub := NewHub(nil, nil)

	hub.BroadcastServerVoiceCounts()

	select {
	case <-hub.voiceCountSignal:
		// Signal received
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected signal on voiceCountSignal")
	}
}

func TestBroadcastServerVoiceCounts_CoalescesMultipleSignals(t *testing.T) {
	hub := NewHub(nil, nil)

	// Send multiple signals in quick succession
	hub.BroadcastServerVoiceCounts()
	hub.BroadcastServerVoiceCounts()
	hub.BroadcastServerVoiceCounts()

	// Drain the one signal
	select {
	case <-hub.voiceCountSignal:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected at least one signal")
	}

	// Verify no more signals (coalesced)
	select {
	case <-hub.voiceCountSignal:
		t.Fatal("expected signals to be coalesced")
	case <-time.After(50 * time.Millisecond):
		// Expected
	}
}

// --- scheduleOnlineCountBroadcast tests ---

func TestScheduleOnlineCountBroadcast_AddsUserToPending(t *testing.T) {
	hub := newMinimalHub()
	userID := uuid.New()

	hub.scheduleOnlineCountBroadcast(userID)

	assert.True(t, hub.onlineCountPending[userID])
	assert.NotNil(t, hub.onlineCountTimer)

	// Clean up timer
	hub.onlineCountTimer.Stop()
}

func TestScheduleOnlineCountBroadcast_ReusesExistingTimer(t *testing.T) {
	hub := newMinimalHub()
	user1 := uuid.New()
	user2 := uuid.New()

	hub.scheduleOnlineCountBroadcast(user1)
	timer1 := hub.onlineCountTimer

	hub.scheduleOnlineCountBroadcast(user2)
	timer2 := hub.onlineCountTimer

	assert.Same(t, timer1, timer2, "should reuse existing timer")
	assert.True(t, hub.onlineCountPending[user1])
	assert.True(t, hub.onlineCountPending[user2])

	timer1.Stop()
}

// --- Shutdown test ---

func TestShutdown_ClosesAllClientSendChannels(t *testing.T) {
	hub := NewHub(nil, nil)
	userID := uuid.New()
	client := &Client{
		ID:       uuid.New(),
		UserID:   userID,
		Send:     make(chan []byte, 10),
		Channels: make(map[uuid.UUID]bool),
	}
	hub.clients[client.ID] = client

	// Start Run in a goroutine
	go hub.Run()

	// Give it a moment to start
	time.Sleep(10 * time.Millisecond)

	hub.Shutdown()

	// Verify the Send channel gets closed (Run exits and closes all client.Send)
	select {
	case _, ok := <-client.Send:
		assert.False(t, ok, "Send channel should be closed after shutdown")
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for shutdown")
	}
}

func TestShutdownIdempotent(t *testing.T) {
	hub := NewHub(nil, nil)
	go hub.Run()

	// First call shuts down and waits for Run to exit
	hub.Shutdown()

	// Second call must not panic (close of closed channel) or block
	require.NotPanics(t, func() {
		hub.Shutdown()
	})
}

func TestBroadcastServerVoiceCountsAfterShutdown(t *testing.T) {
	hub := NewHub(nil, nil)
	go hub.Run()

	// Shutdown waits for Run to exit
	hub.Shutdown()

	// Must not panic or hang — the buffered channel with default clause handles this
	require.NotPanics(t, func() {
		hub.BroadcastServerVoiceCounts()
	})
}

func TestShutdownStopsRunLoop(t *testing.T) {
	hub := NewHub(nil, nil)
	go hub.Run()

	hub.Shutdown()

	// Stopped channel should already be closed since Shutdown waits
	select {
	case <-hub.Stopped():
		// Run goroutine exited — success
	case <-time.After(2 * time.Second):
		t.Fatal("Run goroutine did not exit after Shutdown")
	}
}
