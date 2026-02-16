package websocket

import (
	"encoding/json"
	"log"

	"github.com/google/uuid"
)

// Hub maintains the set of active clients and broadcasts messages
type Hub struct {
	// Registered clients (client ID -> Client)
	clients map[uuid.UUID]*Client

	// User ID to client IDs mapping (for multi-device support)
	userClients map[uuid.UUID]map[uuid.UUID]bool

	// Channel subscriptions (channel ID -> set of client IDs)
	channelSubscriptions map[uuid.UUID]map[uuid.UUID]bool

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
func NewHub() *Hub {
	return &Hub{
		clients:              make(map[uuid.UUID]*Client),
		userClients:          make(map[uuid.UUID]map[uuid.UUID]bool),
		channelSubscriptions: make(map[uuid.UUID]map[uuid.UUID]bool),
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
	default:
		log.Printf("Unknown message type: %s", msg.Type)
	}
}

// handleSubscribe subscribes a client to a channel
func (h *Hub) handleSubscribe(msg IncomingMessage) {
	channelID, ok := msg.Data["channel_id"].(string)
	if !ok {
		log.Printf("Invalid channel_id in subscribe message")
		return
	}

	channelUUID, err := uuid.Parse(channelID)
	if err != nil {
		log.Printf("Invalid channel UUID: %v", err)
		return
	}

	client, ok := h.clients[msg.ClientID]
	if !ok {
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

// handleMessage handles a chat message
func (h *Hub) handleMessage(msg IncomingMessage) {
	channelID, ok := msg.Data["channel_id"].(string)
	if !ok {
		return
	}

	channelUUID, err := uuid.Parse(channelID)
	if err != nil {
		return
	}

	// Broadcast to all subscribers of the channel
	broadcastMsg := BroadcastMessage{
		ChannelID: channelUUID,
		Data: OutgoingMessage{
			Type: "message",
			Data: map[string]interface{}{
				"channel_id": channelUUID,
				"user_id":    msg.UserID,
				"content":    msg.Data["content"],
				"timestamp":  msg.Data["timestamp"],
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

	// Broadcast typing indicator to all subscribers except sender
	broadcastMsg := BroadcastMessage{
		ChannelID:   channelUUID,
		ExcludeUser: &msg.UserID,
		Data: OutgoingMessage{
			Type: "typing",
			Data: map[string]interface{}{
				"channel_id": channelUUID,
				"user_id":    msg.UserID,
				"is_typing":  msg.Data["is_typing"],
			},
		},
	}

	h.broadcast <- broadcastMsg
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
			// Client's send channel is full, close it
			close(client.Send)
			delete(h.clients, client.ID)
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
