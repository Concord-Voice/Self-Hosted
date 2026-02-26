package voice

import (
	"database/sql"
	"encoding/json"

	"github.com/concord/control-plane/internal/websocket"
	"github.com/concord/control-plane/pkg/logger"
	natsclient "github.com/concord/control-plane/pkg/nats"
	"github.com/google/uuid"
)

// NATSSubscriber listens for voice events from the media plane and
// updates the database + broadcasts WebSocket messages to clients.
type NATSSubscriber struct {
	db   *sql.DB
	log  *logger.Logger
	hub  *websocket.Hub
	nats *natsclient.Client
}

// NewNATSSubscriber creates a new NATS subscriber for voice events.
func NewNATSSubscriber(db *sql.DB, log *logger.Logger, hub *websocket.Hub, nats *natsclient.Client) *NATSSubscriber {
	return &NATSSubscriber{
		db:   db,
		log:  log,
		hub:  hub,
		nats: nats,
	}
}

// voiceJoinedEvent matches the media plane's voice.joined NATS payload.
type voiceJoinedEvent struct {
	ChannelID   string `json:"channelId"`
	UserID      string `json:"userId"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName,omitempty"`
	Timestamp   string `json:"timestamp"`
}

// voiceLeftEvent matches the media plane's voice.left NATS payload.
type voiceLeftEvent struct {
	ChannelID string `json:"channelId"`
	UserID    string `json:"userId"`
	Timestamp string `json:"timestamp"`
}

// voiceRoomEmptyEvent matches the media plane's voice.room_empty NATS payload.
type voiceRoomEmptyEvent struct {
	ChannelID string `json:"channelId"`
	Timestamp string `json:"timestamp"`
}

// voiceHeartbeatEvent is the per-room heartbeat from the media plane.
type voiceHeartbeatEvent struct {
	ChannelID string   `json:"channelId"`
	UserIDs   []string `json:"userIds"`
	Timestamp string   `json:"timestamp"`
}

// Subscribe registers handlers for all voice NATS subjects.
func (s *NATSSubscriber) Subscribe() error {
	if _, err := s.nats.Subscribe("voice.joined", s.handleJoined); err != nil {
		return err
	}
	if _, err := s.nats.Subscribe("voice.left", s.handleLeft); err != nil {
		return err
	}
	if _, err := s.nats.Subscribe("voice.room_empty", s.handleRoomEmpty); err != nil {
		return err
	}
	if _, err := s.nats.Subscribe("voice.heartbeat", s.handleHeartbeat); err != nil {
		return err
	}

	s.log.Info("Subscribed to voice NATS events")
	return nil
}

func (s *NATSSubscriber) handleJoined(data []byte) {
	var event voiceJoinedEvent
	if err := json.Unmarshal(data, &event); err != nil {
		s.log.Error("Failed to unmarshal voice.joined", "error", err)
		return
	}

	// Insert into voice_participants
	_, err := s.db.Exec(`
		INSERT INTO voice_participants (channel_id, user_id, joined_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (channel_id, user_id) DO UPDATE SET joined_at = NOW()
	`, event.ChannelID, event.UserID)
	if err != nil {
		s.log.Error("Failed to insert voice participant", "error", err, "channel_id", event.ChannelID, "user_id", event.UserID)
		return
	}

	// Look up the server_id for this channel to broadcast to server subscribers
	var serverID string
	if err := s.db.QueryRow("SELECT server_id FROM channels WHERE id = $1", event.ChannelID).Scan(&serverID); err != nil {
		s.log.Error("Failed to look up server for channel", "error", err, "channel_id", event.ChannelID)
		return
	}

	serverUUID, err := uuid.Parse(serverID)
	if err != nil {
		return
	}

	// Broadcast voice_state_update to all server subscribers
	s.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
		Type: "voice_state_update",
		Data: map[string]interface{}{
			"channel_id":   event.ChannelID,
			"user_id":      event.UserID,
			"username":     event.Username,
			"display_name": event.DisplayName,
			"action":       "joined",
			"server_id":    serverID,
		},
	})

	s.log.Info("Voice participant joined", "channel_id", event.ChannelID, "user_id", event.UserID)
}

func (s *NATSSubscriber) handleLeft(data []byte) {
	var event voiceLeftEvent
	if err := json.Unmarshal(data, &event); err != nil {
		s.log.Error("Failed to unmarshal voice.left", "error", err)
		return
	}

	// Delete from voice_participants
	_, err := s.db.Exec(`
		DELETE FROM voice_participants WHERE channel_id = $1 AND user_id = $2
	`, event.ChannelID, event.UserID)
	if err != nil {
		s.log.Error("Failed to delete voice participant", "error", err, "channel_id", event.ChannelID, "user_id", event.UserID)
		return
	}

	// Look up server_id
	var serverID string
	if err := s.db.QueryRow("SELECT server_id FROM channels WHERE id = $1", event.ChannelID).Scan(&serverID); err != nil {
		s.log.Error("Failed to look up server for channel", "error", err, "channel_id", event.ChannelID)
		return
	}

	serverUUID, err := uuid.Parse(serverID)
	if err != nil {
		return
	}

	s.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
		Type: "voice_state_update",
		Data: map[string]interface{}{
			"channel_id": event.ChannelID,
			"user_id":    event.UserID,
			"action":     "left",
			"server_id":  serverID,
		},
	})

	s.log.Info("Voice participant left", "channel_id", event.ChannelID, "user_id", event.UserID)
}

func (s *NATSSubscriber) handleRoomEmpty(data []byte) {
	var event voiceRoomEmptyEvent
	if err := json.Unmarshal(data, &event); err != nil {
		s.log.Error("Failed to unmarshal voice.room_empty", "error", err)
		return
	}

	// Delete all participants for this channel (safety net)
	_, err := s.db.Exec(`DELETE FROM voice_participants WHERE channel_id = $1`, event.ChannelID)
	if err != nil {
		s.log.Error("Failed to clear voice participants", "error", err, "channel_id", event.ChannelID)
	}

	// Broadcast room_empty so clients clear any stale voice members for this channel.
	// This is a safety net: handleLeft should have already broadcast individual "left"
	// events, but NATS message ordering isn't guaranteed across subjects.
	var serverID string
	if err := s.db.QueryRow("SELECT server_id FROM channels WHERE id = $1", event.ChannelID).Scan(&serverID); err != nil {
		s.log.Error("Failed to look up server for channel (room_empty)", "error", err, "channel_id", event.ChannelID)
		return
	}

	serverUUID, err := uuid.Parse(serverID)
	if err != nil {
		return
	}

	s.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
		Type: "voice_state_update",
		Data: map[string]interface{}{
			"channel_id": event.ChannelID,
			"action":     "room_empty",
			"server_id":  serverID,
		},
	})

	s.log.Info("Voice room empty", "channel_id", event.ChannelID)
}

// handleHeartbeat reconciles voice_participants against the media plane's
// ground-truth room state. Any DB entries not present in the heartbeat are
// stale (client crashed / network dropped) and get cleaned up.
func (s *NATSSubscriber) handleHeartbeat(data []byte) {
	var event voiceHeartbeatEvent
	if err := json.Unmarshal(data, &event); err != nil {
		s.log.Error("Failed to unmarshal voice.heartbeat", "error", err)
		return
	}

	// Query current DB participants for this channel
	rows, err := s.db.Query(`SELECT user_id FROM voice_participants WHERE channel_id = $1`, event.ChannelID)
	if err != nil {
		s.log.Error("Failed to query voice_participants for reconciliation", "error", err, "channel_id", event.ChannelID)
		return
	}
	defer func() { _ = rows.Close() }()

	dbUsers := make(map[string]bool)
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err == nil {
			dbUsers[uid] = true
		}
	}

	// Nothing in DB — nothing to reconcile
	if len(dbUsers) == 0 {
		return
	}

	// Build set of media-plane users
	mpUsers := make(map[string]bool)
	for _, uid := range event.UserIDs {
		mpUsers[uid] = true
	}

	// Look up server_id (needed for broadcasts)
	var serverID string
	if err := s.db.QueryRow("SELECT server_id FROM channels WHERE id = $1", event.ChannelID).Scan(&serverID); err != nil {
		s.log.Error("Failed to look up server for channel (heartbeat)", "error", err, "channel_id", event.ChannelID)
		return
	}
	serverUUID, err := uuid.Parse(serverID)
	if err != nil {
		return
	}

	// Find and remove stale DB users (in DB but not in media plane)
	for uid := range dbUsers {
		if !mpUsers[uid] {
			_, _ = s.db.Exec(`DELETE FROM voice_participants WHERE channel_id = $1 AND user_id = $2`,
				event.ChannelID, uid)

			s.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
				Type: "voice_state_update",
				Data: map[string]interface{}{
					"channel_id": event.ChannelID,
					"user_id":    uid,
					"action":     "left",
					"server_id":  serverID,
				},
			})

			s.log.Info("Reconciled stale voice participant", "channel_id", event.ChannelID, "user_id", uid)
		}
	}

	// If media plane reports empty room but DB still had entries, broadcast room_empty
	if len(event.UserIDs) == 0 && len(dbUsers) > 0 {
		s.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
			Type: "voice_state_update",
			Data: map[string]interface{}{
				"channel_id": event.ChannelID,
				"action":     "room_empty",
				"server_id":  serverID,
			},
		})
	}
}
