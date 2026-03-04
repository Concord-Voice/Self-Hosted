// Package voice provides handlers for voice channel state management.
// It exposes REST endpoints for voice join authorization and participant listing,
// and processes NATS events from the media plane to keep the DB and WS hub in sync.
package voice

import (
	"database/sql"
	"net/http"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Handler handles voice-related requests.
type Handler struct {
	db  *sql.DB
	log *logger.Logger
	hub *websocket.Hub
	cfg *config.Config
}

// NewHandler creates a new voice handler.
func NewHandler(db *sql.DB, log *logger.Logger, hub *websocket.Hub, cfg *config.Config) *Handler {
	return &Handler{
		db:  db,
		log: log,
		hub: hub,
		cfg: cfg,
	}
}

// Participant represents a user currently in a voice channel.
type Participant struct {
	UserID         string `json:"user_id"`
	Username       string `json:"username"`
	DisplayName    string `json:"display_name,omitempty"`
	AvatarURL      string `json:"avatar_url,omitempty"`
	IsMuted        bool   `json:"is_muted"`
	IsDeafened     bool   `json:"is_deafened"`
	IsVideoOn      bool   `json:"is_video_on"`
	IsScreenSharing bool  `json:"is_screen_sharing"`
	JoinedAt       string `json:"joined_at"`
}

// GetParticipants returns all users currently in a voice channel.
// GET /channels/:id/voice/participants
func (h *Handler) GetParticipants(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid channel ID"})
		return
	}

	// Verify the user has access to this channel's server
	var serverID string
	err := h.db.QueryRow(`
		SELECT c.server_id FROM channels c
		INNER JOIN server_members sm ON sm.server_id = c.server_id AND sm.user_id = $2
		WHERE c.id = $1
	`, channelID, userID).Scan(&serverID)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this server"})
		return
	} else if err != nil {
		h.log.Error("Failed to check channel access", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch participants"})
		return
	}

	// Fetch voice participants with user details
	rows, err := h.db.Query(`
		SELECT vp.user_id, u.username, COALESCE(u.display_name, ''), COALESCE(u.avatar_url, ''),
		       vp.is_muted, vp.is_deafened, vp.is_video_on, vp.is_screen_sharing, vp.joined_at
		FROM voice_participants vp
		INNER JOIN users u ON u.id = vp.user_id
		WHERE vp.channel_id = $1
		ORDER BY vp.joined_at ASC
	`, channelID)
	if err != nil {
		h.log.Error("Failed to query voice participants", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch participants"})
		return
	}
	defer func() { _ = rows.Close() }()

	participants := []Participant{}
	for rows.Next() {
		var p Participant
		if err := rows.Scan(
			&p.UserID, &p.Username, &p.DisplayName, &p.AvatarURL,
			&p.IsMuted, &p.IsDeafened, &p.IsVideoOn, &p.IsScreenSharing, &p.JoinedAt,
		); err != nil {
			h.log.Error("Failed to scan voice participant", "error", err)
			continue
		}
		participants = append(participants, p)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating voice participants", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch participants"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"participants": participants})
}

// AuthorizeJoin checks that a user can join a voice channel and returns
// media plane connection details.
// POST /channels/:id/voice/join
func (h *Handler) AuthorizeJoin(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid channel ID"})
		return
	}

	// Fetch channel details + verify membership in one query
	var channelName, channelType, serverID string
	var isEncrypted bool
	var audioQualityTier *string
	err := h.db.QueryRow(`
		SELECT c.id, c.name, c.type, c.server_id, c.is_encrypted, c.audio_quality_tier
		FROM channels c
		INNER JOIN server_members sm ON sm.server_id = c.server_id AND sm.user_id = $2
		WHERE c.id = $1
	`, channelID, userID).Scan(&channelID, &channelName, &channelType, &serverID, &isEncrypted, &audioQualityTier)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": "Channel not found or access denied"})
		return
	} else if err != nil {
		h.log.Error("Failed to fetch channel for voice join", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to authorize"})
		return
	}

	if channelType != "voice" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Not a voice channel"})
		return
	}

	h.log.Info("Voice join authorized", "user_id", userID, "channel_id", channelID, "server_id", serverID)

	c.JSON(http.StatusOK, gin.H{
		"allowed":          true,
		"media_server_url": h.cfg.MediaPlaneURL,
		"ice_servers":      h.cfg.ICEServers(userID),
		"channel": gin.H{
			"id":                 channelID,
			"name":               channelName,
			"server_id":          serverID,
			"is_encrypted":       isEncrypted,
			"audio_quality_tier": audioQualityTier,
		},
	})
}
