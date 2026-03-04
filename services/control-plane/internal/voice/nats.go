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

// roomContext holds the resolved context for a voice room.
// Either serverID is set (server channel) or isDM is true (DM conversation).
type roomContext struct {
	isDM       bool
	serverID   string
	serverUUID uuid.UUID
	convUUID   uuid.UUID
}

// resolveRoom performs the dual-lookup: tries channels first, falls back to dm_conversations.
func (s *NATSSubscriber) resolveRoom(channelID string) (*roomContext, error) {
	var serverID string
	err := s.db.QueryRow("SELECT server_id FROM channels WHERE id = $1", channelID).Scan(&serverID)
	if err == nil {
		serverUUID, parseErr := uuid.Parse(serverID)
		if parseErr != nil {
			return nil, parseErr
		}
		return &roomContext{isDM: false, serverID: serverID, serverUUID: serverUUID}, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}

	// Not a server channel — try DM conversation
	var convID string
	err = s.db.QueryRow("SELECT id FROM dm_conversations WHERE id = $1", channelID).Scan(&convID)
	if err != nil {
		return nil, err
	}
	convUUID, parseErr := uuid.Parse(convID)
	if parseErr != nil {
		return nil, parseErr
	}
	return &roomContext{isDM: true, convUUID: convUUID}, nil
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

	ctx, err := s.resolveRoom(event.ChannelID)
	if err != nil {
		s.log.Error("Failed to resolve room for voice.joined", "error", err, "channel_id", event.ChannelID)
		return
	}

	if ctx.isDM {
		// Insert into dm_voice_participants
		_, err := s.db.Exec(`
			INSERT INTO dm_voice_participants (conversation_id, user_id, joined_at)
			VALUES ($1, $2, NOW())
			ON CONFLICT (conversation_id, user_id) DO UPDATE SET joined_at = NOW()
		`, event.ChannelID, event.UserID)
		if err != nil {
			s.log.Error("Failed to insert DM voice participant", "error", err, "conversation_id", event.ChannelID, "user_id", event.UserID)
			return
		}

		s.hub.BroadcastToDM(ctx.convUUID, websocket.OutgoingMessage{
			Type: "dm_voice_state_update",
			Data: map[string]interface{}{
				"conversation_id": event.ChannelID,
				"user_id":         event.UserID,
				"username":        event.Username,
				"display_name":    event.DisplayName,
				"action":          "joined",
			},
		})
	} else {
		// Insert into voice_participants (server channel)
		_, err := s.db.Exec(`
			INSERT INTO voice_participants (channel_id, user_id, joined_at)
			VALUES ($1, $2, NOW())
			ON CONFLICT (channel_id, user_id) DO UPDATE SET joined_at = NOW()
		`, event.ChannelID, event.UserID)
		if err != nil {
			s.log.Error("Failed to insert voice participant", "error", err, "channel_id", event.ChannelID, "user_id", event.UserID)
			return
		}

		s.hub.BroadcastToServer(ctx.serverUUID, websocket.OutgoingMessage{
			Type: "voice_state_update",
			Data: map[string]interface{}{
				"channel_id":   event.ChannelID,
				"user_id":      event.UserID,
				"username":     event.Username,
				"display_name": event.DisplayName,
				"action":       "joined",
				"server_id":    ctx.serverID,
			},
		})
	}

	s.log.Info("Voice participant joined", "channel_id", event.ChannelID, "user_id", event.UserID, "is_dm", ctx.isDM)

	if !ctx.isDM {
		s.hub.BroadcastServerVoiceCounts()
	}
}

func (s *NATSSubscriber) handleLeft(data []byte) {
	var event voiceLeftEvent
	if err := json.Unmarshal(data, &event); err != nil {
		s.log.Error("Failed to unmarshal voice.left", "error", err)
		return
	}

	ctx, err := s.resolveRoom(event.ChannelID)
	if err != nil {
		s.log.Error("Failed to resolve room for voice.left", "error", err, "channel_id", event.ChannelID)
		return
	}

	if ctx.isDM {
		_, err := s.db.Exec(`DELETE FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2`, event.ChannelID, event.UserID)
		if err != nil {
			s.log.Error("Failed to delete DM voice participant", "error", err, "conversation_id", event.ChannelID, "user_id", event.UserID)
			return
		}

		s.hub.BroadcastToDM(ctx.convUUID, websocket.OutgoingMessage{
			Type: "dm_voice_state_update",
			Data: map[string]interface{}{
				"conversation_id": event.ChannelID,
				"user_id":         event.UserID,
				"action":          "left",
			},
		})
	} else {
		_, err := s.db.Exec(`DELETE FROM voice_participants WHERE channel_id = $1 AND user_id = $2`, event.ChannelID, event.UserID)
		if err != nil {
			s.log.Error("Failed to delete voice participant", "error", err, "channel_id", event.ChannelID, "user_id", event.UserID)
			return
		}

		s.hub.BroadcastToServer(ctx.serverUUID, websocket.OutgoingMessage{
			Type: "voice_state_update",
			Data: map[string]interface{}{
				"channel_id": event.ChannelID,
				"user_id":    event.UserID,
				"action":     "left",
				"server_id":  ctx.serverID,
			},
		})
	}

	s.log.Info("Voice participant left", "channel_id", event.ChannelID, "user_id", event.UserID, "is_dm", ctx.isDM)

	if !ctx.isDM {
		s.hub.BroadcastServerVoiceCounts()
	}
}

func (s *NATSSubscriber) handleRoomEmpty(data []byte) {
	var event voiceRoomEmptyEvent
	if err := json.Unmarshal(data, &event); err != nil {
		s.log.Error("Failed to unmarshal voice.room_empty", "error", err)
		return
	}

	ctx, err := s.resolveRoom(event.ChannelID)
	if err != nil {
		s.log.Error("Failed to resolve room for voice.room_empty", "error", err, "channel_id", event.ChannelID)
		return
	}

	if ctx.isDM {
		_, err := s.db.Exec(`DELETE FROM dm_voice_participants WHERE conversation_id = $1`, event.ChannelID)
		if err != nil {
			s.log.Error("Failed to clear DM voice participants", "error", err, "conversation_id", event.ChannelID)
		}

		s.hub.BroadcastToDM(ctx.convUUID, websocket.OutgoingMessage{
			Type: "dm_voice_state_update",
			Data: map[string]interface{}{
				"conversation_id": event.ChannelID,
				"action":          "room_empty",
			},
		})
	} else {
		_, err := s.db.Exec(`DELETE FROM voice_participants WHERE channel_id = $1`, event.ChannelID)
		if err != nil {
			s.log.Error("Failed to clear voice participants", "error", err, "channel_id", event.ChannelID)
		}

		s.hub.BroadcastToServer(ctx.serverUUID, websocket.OutgoingMessage{
			Type: "voice_state_update",
			Data: map[string]interface{}{
				"channel_id": event.ChannelID,
				"action":     "room_empty",
				"server_id":  ctx.serverID,
			},
		})
	}

	s.log.Info("Voice room empty", "channel_id", event.ChannelID, "is_dm", ctx.isDM)

	if !ctx.isDM {
		s.hub.BroadcastServerVoiceCounts()
	}
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

	ctx, err := s.resolveRoom(event.ChannelID)
	if err != nil {
		s.log.Error("Failed to resolve room for voice.heartbeat", "error", err, "channel_id", event.ChannelID)
		return
	}

	// Query current DB participants
	var participantsQuery string
	if ctx.isDM {
		participantsQuery = `SELECT user_id FROM dm_voice_participants WHERE conversation_id = $1`
	} else {
		participantsQuery = `SELECT user_id FROM voice_participants WHERE channel_id = $1`
	}
	rows, err := s.db.Query(participantsQuery, event.ChannelID)
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

	if len(dbUsers) == 0 {
		return
	}

	// Build set of media-plane users
	mpUsers := make(map[string]bool)
	for _, uid := range event.UserIDs {
		mpUsers[uid] = true
	}

	// Find and remove stale DB users
	removedAny := false
	for uid := range dbUsers {
		if !mpUsers[uid] {
			removedAny = true
			if ctx.isDM {
				_, _ = s.db.Exec(`DELETE FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2`, event.ChannelID, uid)
				s.hub.BroadcastToDM(ctx.convUUID, websocket.OutgoingMessage{
					Type: "dm_voice_state_update",
					Data: map[string]interface{}{
						"conversation_id": event.ChannelID,
						"user_id":         uid,
						"action":          "left",
					},
				})
			} else {
				_, _ = s.db.Exec(`DELETE FROM voice_participants WHERE channel_id = $1 AND user_id = $2`, event.ChannelID, uid)
				s.hub.BroadcastToServer(ctx.serverUUID, websocket.OutgoingMessage{
					Type: "voice_state_update",
					Data: map[string]interface{}{
						"channel_id": event.ChannelID,
						"user_id":    uid,
						"action":     "left",
						"server_id":  ctx.serverID,
					},
				})
			}
			s.log.Info("Reconciled stale voice participant", "channel_id", event.ChannelID, "user_id", uid, "is_dm", ctx.isDM)
		}
	}

	// If media plane reports empty room but DB still had entries, broadcast room_empty
	if len(event.UserIDs) == 0 && len(dbUsers) > 0 {
		if ctx.isDM {
			s.hub.BroadcastToDM(ctx.convUUID, websocket.OutgoingMessage{
				Type: "dm_voice_state_update",
				Data: map[string]interface{}{
					"conversation_id": event.ChannelID,
					"action":          "room_empty",
				},
			})
		} else {
			s.hub.BroadcastToServer(ctx.serverUUID, websocket.OutgoingMessage{
				Type: "voice_state_update",
				Data: map[string]interface{}{
					"channel_id": event.ChannelID,
					"action":     "room_empty",
					"server_id":  ctx.serverID,
				},
			})
		}
	}

	if !ctx.isDM && removedAny {
		s.hub.BroadcastServerVoiceCounts()
	}
}
