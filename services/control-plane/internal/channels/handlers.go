// Package channels provides handlers for managing server channels.
package channels

import (
	"database/sql"
	"fmt"
	"net/http"

	"github.com/concord/control-plane/internal/models"
	"github.com/concord/control-plane/internal/websocket"
	"github.com/concord/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Handler handles channel-related requests
type Handler struct {
	db  *sql.DB
	log *logger.Logger
	hub *websocket.Hub
}

// NewHandler creates a new channel handler
func NewHandler(db *sql.DB, log *logger.Logger, hub *websocket.Hub) *Handler {
	return &Handler{
		db:  db,
		log: log,
		hub: hub,
	}
}

// CreateChannelRequest represents a request to create a channel
type CreateChannelRequest struct {
	ServerID    string            `json:"server_id" binding:"required,uuid"`
	Name        string            `json:"name" binding:"required,min=3,max=100"`
	Type        string            `json:"type" binding:"required,oneof=text voice bulletin"`
	Emoji       *string           `json:"emoji,omitempty"`        // Optional custom emoji
	IsEncrypted *bool             `json:"is_encrypted,omitempty"` // Defaults to server's e2ee_default
	GroupID     *string           `json:"group_id,omitempty"`     // Channel group (category); nil = uncategorized
	WrappedKeys map[string]string `json:"wrapped_keys,omitempty"` // user_id → wrapped CSK (required if encrypted)
}

// UpdateChannelRequest represents a request to update a channel
type UpdateChannelRequest struct {
	Name             string  `json:"name" binding:"required,min=3,max=100"`
	Type             string  `json:"type" binding:"required,oneof=text voice bulletin"`
	Emoji            *string `json:"emoji,omitempty"`
	AudioQualityTier *string `json:"audio_quality_tier,omitempty"`
	GroupID          *string `json:"group_id"` // pointer: nil=unchanged, ""=uncategorized, "uuid"=set group
}

// Valid audio quality tier values
var validAudioQualityTiers = map[string]bool{
	"minimum": true, "low": true, "moderate": true, "standard": true, "high": true, "hifi": true, "studio": true,
}

// ListChannels returns all channels in a server
func (h *Handler) ListChannels(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	// Validate server ID
	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid server ID"})
		return
	}

	// Check if user is a member of the server
	var isMember bool
	memberQuery := `
		SELECT EXISTS(
			SELECT 1 FROM server_members
			WHERE server_id = $1 AND user_id = $2
		)
	`

	err := h.db.QueryRow(memberQuery, serverID, userID).Scan(&isMember)
	if err != nil {
		h.log.Error("Failed to check membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch channels"})
		return
	}

	if !isMember {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this server"})
		return
	}

	// Get all channels in the server
	query := `
		SELECT id, server_id, name, type, description, emoji, is_encrypted, audio_quality_tier, group_id, linked_voice_channel_id, position, created_at, updated_at
		FROM channels
		WHERE server_id = $1
		ORDER BY position ASC, created_at ASC
	`

	rows, err := h.db.Query(query, serverID)
	if err != nil {
		h.log.Error("Failed to query channels", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch channels"})
		return
	}
	defer func() { _ = rows.Close() }()

	channels := []models.Channel{}
	for rows.Next() {
		var channel models.Channel
		err := rows.Scan(
			&channel.ID,
			&channel.ServerID,
			&channel.Name,
			&channel.Type,
			&channel.Description,
			&channel.Emoji,
			&channel.IsEncrypted,
			&channel.AudioQualityTier,
			&channel.GroupID,
			&channel.LinkedVoiceChannelID,
			&channel.Position,
			&channel.CreatedAt,
			&channel.UpdatedAt,
		)
		if err != nil {
			h.log.Error("Failed to scan channel", "error", err)
			continue
		}
		channels = append(channels, channel)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating channels", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch channels"})
		return
	}

	// Also fetch channel groups for this server
	groupRows, err := h.db.Query(
		`SELECT id, server_id, name, position, created_at, updated_at
		 FROM channel_groups
		 WHERE server_id = $1
		 ORDER BY position ASC, created_at ASC`,
		serverID,
	)
	if err != nil {
		h.log.Error("Failed to query channel groups", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch channels"})
		return
	}
	defer func() { _ = groupRows.Close() }()

	groups := []models.ChannelGroup{}
	for groupRows.Next() {
		var g models.ChannelGroup
		if err := groupRows.Scan(&g.ID, &g.ServerID, &g.Name, &g.Position, &g.CreatedAt, &g.UpdatedAt); err != nil {
			h.log.Error("Failed to scan channel group", "error", err)
			continue
		}
		groups = append(groups, g)
	}
	if err := groupRows.Err(); err != nil {
		h.log.Error("Error iterating channel groups", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch channels"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"channels": channels, "channel_groups": groups})
}

// CreateChannel creates a new channel in a server
func (h *Handler) CreateChannel(c *gin.Context) {
	userID := c.GetString("user_id")

	var req CreateChannelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	// Check if user is owner or admin of the server
	var role string
	roleQuery := `
		SELECT role FROM server_members
		WHERE server_id = $1 AND user_id = $2
	`

	err := h.db.QueryRow(roleQuery, req.ServerID, userID).Scan(&role)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this server"})
		return
	} else if err != nil {
		h.log.Error("Failed to check membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create channel"})
		return
	}

	if role != "owner" && role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only owners and admins can create channels"})
		return
	}

	// Determine encryption setting: use request value or fall back to server's e2ee_default
	isEncrypted := false
	if req.IsEncrypted != nil {
		isEncrypted = *req.IsEncrypted
	} else {
		var e2eeDefault bool
		_ = h.db.QueryRow(`SELECT e2ee_default FROM servers WHERE id = $1`, req.ServerID).Scan(&e2eeDefault)
		isEncrypted = e2eeDefault
	}

	// If encrypted, wrapped keys are required
	if isEncrypted && len(req.WrappedKeys) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Encrypted channels require wrapped keys for all members"})
		return
	}

	// Start transaction for channel + keys
	tx, err := h.db.Begin()
	if err != nil {
		h.log.Error("Failed to start transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create channel"})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback transaction", "error", rbErr)
		}
	}()

	// Create channel — place at end of its group (or end of uncategorized)
	channelID := uuid.New().String()

	// Compute next position: max(position) + 1 within the same group
	var maxPos int
	if req.GroupID != nil {
		_ = tx.QueryRow(
			`SELECT COALESCE(MAX(position), -1) FROM channels WHERE server_id = $1 AND group_id = $2`,
			req.ServerID, *req.GroupID,
		).Scan(&maxPos)
	} else {
		_ = tx.QueryRow(
			`SELECT COALESCE(MAX(position), -1) FROM channels WHERE server_id = $1 AND group_id IS NULL`,
			req.ServerID,
		).Scan(&maxPos)
	}
	nextPos := maxPos + 1

	insertQuery := `
		INSERT INTO channels (id, server_id, name, type, emoji, is_encrypted, group_id, position, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW(), NOW())
		RETURNING created_at, updated_at
	`

	var channel models.Channel
	channel.ID = channelID
	channel.ServerID = req.ServerID
	channel.Name = req.Name
	channel.Type = req.Type
	channel.Emoji = req.Emoji
	channel.IsEncrypted = isEncrypted
	channel.GroupID = req.GroupID
	channel.Position = nextPos

	err = tx.QueryRow(insertQuery, channelID, req.ServerID, req.Name, req.Type, req.Emoji, isEncrypted, req.GroupID, nextPos).Scan(
		&channel.CreatedAt,
		&channel.UpdatedAt,
	)
	if err != nil {
		h.log.Error("Failed to create channel", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create channel"})
		return
	}

	// Store wrapped channel keys if encrypted
	if isEncrypted {
		keyInsert := `
			INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
			VALUES ($1, $2, $3, 1)
		`
		for memberUserID, wrappedKey := range req.WrappedKeys {
			if _, parseErr := uuid.Parse(memberUserID); parseErr != nil {
				continue // skip invalid UUIDs
			}
			_, err = tx.Exec(keyInsert, channelID, memberUserID, wrappedKey)
			if err != nil {
				h.log.Error("Failed to store channel key", "error", err, "user_id", memberUserID)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store encryption keys"})
				return
			}
		}
	}

	// Auto-create linked text channel for voice channels
	var linkedTextChannel *models.Channel
	if req.Type == "voice" {
		linkedTextID := uuid.New().String()
		linkedTextPos := nextPos + 1 // Place right after the voice channel
		linkedInsert := `
			INSERT INTO channels (id, server_id, name, type, is_encrypted, group_id, linked_voice_channel_id, position, created_at, updated_at)
			VALUES ($1, $2, $3, 'text', $4, $5, $6, $7, NOW(), NOW())
			RETURNING created_at, updated_at
		`
		var ltc models.Channel
		ltc.ID = linkedTextID
		ltc.ServerID = req.ServerID
		ltc.Name = req.Name
		ltc.Type = "text"
		ltc.IsEncrypted = isEncrypted
		ltc.GroupID = req.GroupID
		ltc.LinkedVoiceChannelID = &channelID
		ltc.Position = linkedTextPos

		err = tx.QueryRow(linkedInsert, linkedTextID, req.ServerID, req.Name, isEncrypted, req.GroupID, channelID, linkedTextPos).Scan(
			&ltc.CreatedAt,
			&ltc.UpdatedAt,
		)
		if err != nil {
			h.log.Error("Failed to create linked text channel", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create linked text channel"})
			return
		}

		// If encrypted, copy wrapped keys for the linked text channel too
		if isEncrypted {
			keyInsertLinked := `
				INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
				VALUES ($1, $2, $3, 1)
			`
			for memberUserID, wrappedKey := range req.WrappedKeys {
				if _, parseErr := uuid.Parse(memberUserID); parseErr != nil {
					continue
				}
				_, err = tx.Exec(keyInsertLinked, linkedTextID, memberUserID, wrappedKey)
				if err != nil {
					h.log.Error("Failed to store linked text channel key", "error", err, "user_id", memberUserID)
					// Non-fatal — keys can be distributed later
				}
			}
		}

		linkedTextChannel = &ltc
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create channel"})
		return
	}

	h.log.Info("Channel created", "channel_id", channelID, "server_id", req.ServerID, "user_id", userID, "encrypted", isEncrypted)

	// Broadcast new channel to server subscribers so other members see it instantly
	if h.hub != nil {
		if serverUUID, err := uuid.Parse(req.ServerID); err == nil {
			h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
				Type: "channel_created",
				Data: map[string]interface{}{
					"channel": map[string]interface{}{
						"id":           channel.ID,
						"server_id":    channel.ServerID,
						"name":         channel.Name,
						"type":         channel.Type,
						"emoji":        channel.Emoji,
						"is_encrypted": channel.IsEncrypted,
						"group_id":     channel.GroupID,
						"position":     channel.Position,
						"created_at":   channel.CreatedAt,
						"updated_at":   channel.UpdatedAt,
					},
				},
			})

			// Also broadcast the linked text channel creation
			if linkedTextChannel != nil {
				h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
					Type: "channel_created",
					Data: map[string]interface{}{
						"channel": map[string]interface{}{
							"id":                       linkedTextChannel.ID,
							"server_id":                linkedTextChannel.ServerID,
							"name":                     linkedTextChannel.Name,
							"type":                     linkedTextChannel.Type,
							"is_encrypted":              linkedTextChannel.IsEncrypted,
							"group_id":                 linkedTextChannel.GroupID,
							"linked_voice_channel_id":  linkedTextChannel.LinkedVoiceChannelID,
							"position":                 linkedTextChannel.Position,
							"created_at":               linkedTextChannel.CreatedAt,
							"updated_at":               linkedTextChannel.UpdatedAt,
						},
					},
				})
			}
		}
	}

	// Return response: voice channels include the linked text channel
	response := gin.H{"channel": channel}
	if linkedTextChannel != nil {
		response["linked_text_channel"] = linkedTextChannel
	}
	c.JSON(http.StatusCreated, response)
}

// GetChannel returns a specific channel
func (h *Handler) GetChannel(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	// Validate channel ID
	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid channel ID"})
		return
	}

	// Get channel and check if user is a member of the server
	query := `
		SELECT c.id, c.server_id, c.name, c.type, c.description, c.emoji, c.is_encrypted, c.audio_quality_tier, c.group_id, c.linked_voice_channel_id, c.position, c.created_at, c.updated_at
		FROM channels c
		INNER JOIN server_members sm ON c.server_id = sm.server_id
		WHERE c.id = $1 AND sm.user_id = $2
	`

	var channel models.Channel
	err := h.db.QueryRow(query, channelID, userID).Scan(
		&channel.ID,
		&channel.ServerID,
		&channel.Name,
		&channel.Type,
		&channel.Description,
		&channel.Emoji,
		&channel.IsEncrypted,
		&channel.AudioQualityTier,
		&channel.GroupID,
		&channel.LinkedVoiceChannelID,
		&channel.Position,
		&channel.CreatedAt,
		&channel.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Channel not found or access denied"})
		return
	} else if err != nil {
		h.log.Error("Failed to fetch channel", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch channel"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"channel": channel})
}

// UpdateChannel updates a channel's details
func (h *Handler) UpdateChannel(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	// Validate channel ID
	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid channel ID"})
		return
	}

	var req UpdateChannelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	// Get channel's server ID and check user's role
	var serverID string
	var role string
	roleQuery := `
		SELECT c.server_id, sm.role
		FROM channels c
		INNER JOIN server_members sm ON c.server_id = sm.server_id
		WHERE c.id = $1 AND sm.user_id = $2
	`

	err := h.db.QueryRow(roleQuery, channelID, userID).Scan(&serverID, &role)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Channel not found or access denied"})
		return
	} else if err != nil {
		h.log.Error("Failed to check permissions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update channel"})
		return
	}

	if role != "owner" && role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only owners and admins can update channels"})
		return
	}

	// Validate audio quality tier if provided
	if req.AudioQualityTier != nil && *req.AudioQualityTier != "" {
		if !validAudioQualityTiers[*req.AudioQualityTier] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid audio quality tier"})
			return
		}
	}

	// Resolve group_id: nil pointer means "don't change" (use existing), non-nil updates it
	// Empty string (*string pointing to "") = set to NULL (uncategorized)
	var groupIDParam interface{}
	if req.GroupID != nil {
		if *req.GroupID == "" {
			groupIDParam = nil // SQL NULL = uncategorized
		} else {
			groupIDParam = *req.GroupID
		}
	}

	// Update channel (is_encrypted is immutable — cannot be changed after creation)
	var updateQuery string
	var channel models.Channel
	channel.ID = channelID
	channel.Name = req.Name
	channel.Type = req.Type

	if req.GroupID != nil {
		updateQuery = `
			UPDATE channels
			SET name = $1, type = $2, emoji = $3, audio_quality_tier = $4, group_id = $5, updated_at = NOW()
			WHERE id = $6
			RETURNING server_id, emoji, is_encrypted, audio_quality_tier, group_id, linked_voice_channel_id, position, created_at, updated_at
		`
		err = h.db.QueryRow(updateQuery, req.Name, req.Type, req.Emoji, req.AudioQualityTier, groupIDParam, channelID).Scan(
			&channel.ServerID,
			&channel.Emoji,
			&channel.IsEncrypted,
			&channel.AudioQualityTier,
			&channel.GroupID,
			&channel.LinkedVoiceChannelID,
			&channel.Position,
			&channel.CreatedAt,
			&channel.UpdatedAt,
		)
	} else {
		updateQuery = `
			UPDATE channels
			SET name = $1, type = $2, emoji = $3, audio_quality_tier = $4, updated_at = NOW()
			WHERE id = $5
			RETURNING server_id, emoji, is_encrypted, audio_quality_tier, group_id, linked_voice_channel_id, position, created_at, updated_at
		`
		err = h.db.QueryRow(updateQuery, req.Name, req.Type, req.Emoji, req.AudioQualityTier, channelID).Scan(
			&channel.ServerID,
			&channel.Emoji,
			&channel.IsEncrypted,
			&channel.AudioQualityTier,
			&channel.GroupID,
			&channel.LinkedVoiceChannelID,
			&channel.Position,
			&channel.CreatedAt,
			&channel.UpdatedAt,
		)
	}

	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Channel not found"})
		return
	} else if err != nil {
		h.log.Error("Failed to update channel", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update channel"})
		return
	}

	h.log.Info("Channel updated", "channel_id", channelID, "user_id", userID)

	// Broadcast channel update to server subscribers so other members see the change
	if h.hub != nil {
		if serverUUID, err := uuid.Parse(channel.ServerID); err == nil {
			h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
				Type: "channel_updated",
				Data: map[string]interface{}{
					"channel_id":         channelID,
					"server_id":          channel.ServerID,
					"name":               channel.Name,
					"type":               channel.Type,
					"emoji":              channel.Emoji,
					"is_encrypted":       channel.IsEncrypted,
					"audio_quality_tier": channel.AudioQualityTier,
					"group_id":           channel.GroupID,
				},
			})
		}
	}

	c.JSON(http.StatusOK, gin.H{"channel": channel})
}

// DeleteChannel deletes a channel (owner/admin only)
func (h *Handler) DeleteChannel(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	// Validate channel ID
	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid channel ID"})
		return
	}

	// Get channel's server ID and check user's role
	var role, serverID string
	roleQuery := `
		SELECT c.server_id, sm.role
		FROM channels c
		INNER JOIN server_members sm ON c.server_id = sm.server_id
		WHERE c.id = $1 AND sm.user_id = $2
	`

	err := h.db.QueryRow(roleQuery, channelID, userID).Scan(&serverID, &role)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Channel not found or access denied"})
		return
	} else if err != nil {
		h.log.Error("Failed to check permissions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete channel"})
		return
	}

	if role != "owner" && role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only owners and admins can delete channels"})
		return
	}

	// Delete channel
	deleteQuery := `DELETE FROM channels WHERE id = $1`

	_, err = h.db.Exec(deleteQuery, channelID)
	if err != nil {
		h.log.Error("Failed to delete channel", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete channel"})
		return
	}

	h.log.Info("Channel deleted", "channel_id", channelID, "user_id", userID)

	// Broadcast deletion to server subscribers so frontends can clean up
	if h.hub != nil {
		if serverUUID, err := uuid.Parse(serverID); err == nil {
			h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
				Type: "channel_deleted",
				Data: map[string]interface{}{
					"channel_id": channelID,
					"server_id":  serverID,
				},
			})
		}
	}

	c.JSON(http.StatusOK, gin.H{"message": "Channel deleted successfully"})
}

// GetUnreadCounts returns unread message counts for all channels in a server.
// For each channel, counts messages created after the user's last_read_at.
func (h *Handler) GetUnreadCounts(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid server ID"})
		return
	}

	// Verify membership
	var isMember bool
	err := h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, userID,
	).Scan(&isMember)
	if err != nil {
		h.log.Error("Failed to check membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch unread counts"})
		return
	}
	if !isMember {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this server"})
		return
	}

	// For each channel in the server, count messages newer than last_read_at.
	// If no read state exists, fall back to the user's join date so pre-existing
	// messages are not counted as unread for first-time members.
	query := `
		SELECT ch.id,
			COUNT(m.id)::int AS unread_count
		FROM channels ch
		LEFT JOIN messages m
			ON m.channel_id = ch.id
			AND m.user_id != $2
			AND m.created_at > COALESCE(
				(SELECT last_read_at FROM channel_read_states WHERE user_id = $2 AND channel_id = ch.id),
				(SELECT joined_at FROM server_members WHERE server_id = $1 AND user_id = $2)
			)
		WHERE ch.server_id = $1
		GROUP BY ch.id
	`

	rows, err := h.db.Query(query, serverID, userID)
	if err != nil {
		h.log.Error("Failed to query unread counts", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch unread counts"})
		return
	}
	defer func() { _ = rows.Close() }()

	type unreadEntry struct {
		ChannelID   string `json:"channel_id"`
		UnreadCount int    `json:"unread_count"`
	}
	unreads := []unreadEntry{}
	for rows.Next() {
		var entry unreadEntry
		if err := rows.Scan(&entry.ChannelID, &entry.UnreadCount); err != nil {
			h.log.Error("Failed to scan unread count", "error", err)
			continue
		}
		unreads = append(unreads, entry)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating unread counts", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch unread counts"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"unreads": unreads})
}

// GetServerUnreadStatus returns a list of server IDs where the user has unread messages.
// Used to show unread dots on server icons without fetching per-channel counts for every server.
func (h *Handler) GetServerUnreadStatus(c *gin.Context) {
	userID := c.GetString("user_id")

	query := `
		SELECT DISTINCT ch.server_id
		FROM channels ch
		INNER JOIN server_members sm
			ON ch.server_id = sm.server_id AND sm.user_id = $1
		INNER JOIN messages m
			ON m.channel_id = ch.id
			AND m.user_id != $1
			AND m.created_at > COALESCE(
				(SELECT last_read_at FROM channel_read_states WHERE user_id = $1 AND channel_id = ch.id),
				sm.joined_at
			)
	`

	rows, err := h.db.Query(query, userID)
	if err != nil {
		h.log.Error("Failed to query server unread status", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch server unread status"})
		return
	}
	defer func() { _ = rows.Close() }()

	serverIDs := []string{}
	for rows.Next() {
		var serverID string
		if err := rows.Scan(&serverID); err != nil {
			h.log.Error("Failed to scan server ID", "error", err)
			continue
		}
		serverIDs = append(serverIDs, serverID)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating server unread status", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch server unread status"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"server_ids": serverIDs})
}

// MarkChannelRead updates the user's last_read_at for a channel (upsert).
func (h *Handler) MarkChannelRead(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid channel ID"})
		return
	}

	// Verify user is a member of the channel's server
	var isMember bool
	err := h.db.QueryRow(
		`SELECT EXISTS(
			SELECT 1 FROM channels c
			INNER JOIN server_members sm ON c.server_id = sm.server_id
			WHERE c.id = $1 AND sm.user_id = $2
		)`,
		channelID, userID,
	).Scan(&isMember)
	if err != nil {
		h.log.Error("Failed to check channel membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to mark channel read"})
		return
	}
	if !isMember {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this server"})
		return
	}

	// Upsert read state
	_, err = h.db.Exec(
		`INSERT INTO channel_read_states (user_id, channel_id, last_read_at)
		 VALUES ($1, $2, NOW())
		 ON CONFLICT (user_id, channel_id) DO UPDATE SET last_read_at = NOW()`,
		userID, channelID,
	)
	if err != nil {
		h.log.Error("Failed to upsert read state", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to mark channel read"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Channel marked as read"})
}

// MarkServerRead marks all channels in a server as read for the user.
func (h *Handler) MarkServerRead(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid server ID"})
		return
	}

	// Verify membership
	var isMember bool
	err := h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, userID,
	).Scan(&isMember)
	if err != nil {
		h.log.Error("Failed to check membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to mark server read"})
		return
	}
	if !isMember {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this server"})
		return
	}

	// Upsert read state for every channel in the server
	_, err = h.db.Exec(
		`INSERT INTO channel_read_states (user_id, channel_id, last_read_at)
		 SELECT $1, id, NOW() FROM channels WHERE server_id = $2
		 ON CONFLICT (user_id, channel_id) DO UPDATE SET last_read_at = NOW()`,
		userID, serverID,
	)
	if err != nil {
		h.log.Error("Failed to mark server read", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to mark server read"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "All channels marked as read"})
}

// GetChannelKeys returns the caller's wrapped channel key for an E2EE channel.
func (h *Handler) GetChannelKeys(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid channel ID"})
		return
	}

	// Check membership and channel encryption status
	var isMember bool
	var isEncrypted bool
	err := h.db.QueryRow(
		`SELECT EXISTS(
			SELECT 1 FROM channels c
			INNER JOIN server_members sm ON c.server_id = sm.server_id
			WHERE c.id = $1 AND sm.user_id = $2
		), (SELECT is_encrypted FROM channels WHERE id = $1)`,
		channelID, userID,
	).Scan(&isMember, &isEncrypted)
	if err != nil {
		h.log.Error("Failed to check channel access", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch channel keys"})
		return
	}
	if !isMember {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this channel's server"})
		return
	}
	if !isEncrypted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Channel is not encrypted"})
		return
	}

	// Support ?version=N to fetch a specific key version (for decrypting old messages)
	var key models.ChannelKey
	if versionStr := c.Query("version"); versionStr != "" {
		var version int
		if _, scanErr := fmt.Sscanf(versionStr, "%d", &version); scanErr == nil && version > 0 {
			err = h.db.QueryRow(
				`SELECT id, channel_id, user_id, wrapped_key, key_version, created_at
				 FROM channel_keys
				 WHERE channel_id = $1 AND user_id = $2 AND key_version = $3`,
				channelID, userID, version,
			).Scan(&key.ID, &key.ChannelID, &key.UserID, &key.WrappedKey, &key.KeyVersion, &key.CreatedAt)
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid version parameter"})
			return
		}
	} else {
		// Default: latest version
		err = h.db.QueryRow(
			`SELECT id, channel_id, user_id, wrapped_key, key_version, created_at
			 FROM channel_keys
			 WHERE channel_id = $1 AND user_id = $2
			 ORDER BY key_version DESC LIMIT 1`,
			channelID, userID,
		).Scan(&key.ID, &key.ChannelID, &key.UserID, &key.WrappedKey, &key.KeyVersion, &key.CreatedAt)
	}
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "No encryption key available yet", "pending": true})
		return
	} else if err != nil {
		h.log.Error("Failed to fetch channel key", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch channel keys"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"key": key})
}

// DistributeChannelKeysRequest represents wrapped keys for new members
type DistributeChannelKeysRequest struct {
	WrappedKeys map[string]string `json:"wrapped_keys" binding:"required"` // user_id → wrapped CSK
	KeyVersion  *int              `json:"key_version,omitempty"`           // Explicit epoch for rotation (must be > current max)
}

// DistributeChannelKeys stores wrapped channel keys for new members (key distribution).
// Uses first-response-wins: if a key already exists for a (channel, user), returns 409.
func (h *Handler) DistributeChannelKeys(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid channel ID"})
		return
	}

	var req DistributeChannelKeysRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	// Verify the caller is a member and the channel is encrypted
	var role string
	var isEncrypted bool
	err := h.db.QueryRow(
		`SELECT sm.role, c.is_encrypted
		 FROM channels c
		 INNER JOIN server_members sm ON c.server_id = sm.server_id
		 WHERE c.id = $1 AND sm.user_id = $2`,
		channelID, userID,
	).Scan(&role, &isEncrypted)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this channel's server"})
		return
	} else if err != nil {
		h.log.Error("Failed to check permissions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to distribute keys"})
		return
	}
	if !isEncrypted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Channel is not encrypted"})
		return
	}

	// Caller must have their own key (prove they have access to the CSK)
	var hasKey bool
	_ = h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM channel_keys WHERE channel_id = $1 AND user_id = $2)`,
		channelID, userID,
	).Scan(&hasKey)
	if !hasKey {
		c.JSON(http.StatusForbidden, gin.H{"error": "You must have the channel key to distribute keys"})
		return
	}

	// Resolve key version: use explicit version if provided and valid, else latest
	var targetKeyVersion int
	if req.KeyVersion != nil && *req.KeyVersion > 0 {
		targetKeyVersion = *req.KeyVersion
	} else {
		_ = h.db.QueryRow(
			`SELECT COALESCE(MAX(key_version), 1) FROM channel_keys WHERE channel_id = $1`,
			channelID,
		).Scan(&targetKeyVersion)
	}

	distributed := 0
	duplicates := 0
	for memberUserID, wrappedKey := range req.WrappedKeys {
		if _, parseErr := uuid.Parse(memberUserID); parseErr != nil {
			continue
		}

		keyVersion := targetKeyVersion

		// First-response-wins via ON CONFLICT DO NOTHING — no TOCTOU race
		result, err := h.db.Exec(
			`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (channel_id, user_id, key_version) DO NOTHING`,
			channelID, memberUserID, wrappedKey, keyVersion,
		)
		if err != nil {
			h.log.Error("Failed to store key for member", "error", err, "user_id", memberUserID)
			continue
		}
		rowsAffected, _ := result.RowsAffected()
		if rowsAffected == 0 {
			duplicates++
			continue
		}

		// Remove pending key request if one exists
		_, _ = h.db.Exec(
			`DELETE FROM pending_key_requests WHERE channel_id = $1 AND user_id = $2`,
			channelID, memberUserID,
		)

		// Notify the recipient that their key is ready
		if h.hub != nil {
			if recipientUUID, parseErr := uuid.Parse(memberUserID); parseErr == nil {
				h.hub.BroadcastToUser(recipientUUID, websocket.OutgoingMessage{
					Type: "key_delivered",
					Data: map[string]interface{}{
						"channel_id": channelID,
						"user_id":    memberUserID,
					},
				})
			}
		}

		distributed++
	}

	h.log.Info("Channel keys distributed",
		"channel_id", channelID, "by_user", userID,
		"distributed", distributed, "duplicates", duplicates)

	c.JSON(http.StatusOK, gin.H{
		"distributed": distributed,
		"duplicates":  duplicates,
	})
}

// GetPendingKeyRequests returns pending key requests for channels the caller can service.
func (h *Handler) GetPendingKeyRequests(c *gin.Context) {
	userID := c.GetString("user_id")

	// Return pending requests for channels where the caller already has a key
	query := `
		SELECT pkr.id, pkr.channel_id, pkr.user_id, pkr.created_at
		FROM pending_key_requests pkr
		INNER JOIN channel_keys ck ON pkr.channel_id = ck.channel_id AND ck.user_id = $1
		ORDER BY pkr.created_at ASC
	`

	rows, err := h.db.Query(query, userID)
	if err != nil {
		h.log.Error("Failed to query pending key requests", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch pending key requests"})
		return
	}
	defer func() { _ = rows.Close() }()

	type pendingRequest struct {
		ID        string `json:"id"`
		ChannelID string `json:"channel_id"`
		UserID    string `json:"user_id"`
		CreatedAt string `json:"created_at"`
	}
	requests := []pendingRequest{}
	for rows.Next() {
		var req pendingRequest
		if err := rows.Scan(&req.ID, &req.ChannelID, &req.UserID, &req.CreatedAt); err != nil {
			h.log.Error("Failed to scan pending request", "error", err)
			continue
		}
		requests = append(requests, req)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating pending requests", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch pending key requests"})
		return
	}

	// Also fetch DM pending key requests
	dmQuery := `
		SELECT dpkr.id, dpkr.conversation_id, dpkr.user_id, dpkr.created_at
		FROM dm_pending_key_requests dpkr
		INNER JOIN dm_channel_keys dck ON dpkr.conversation_id = dck.conversation_id AND dck.user_id = $1
		WHERE dpkr.user_id != $1
		ORDER BY dpkr.created_at ASC
	`
	dmRows, dmErr := h.db.Query(dmQuery, userID)
	if dmErr == nil {
		defer func() { _ = dmRows.Close() }()
		for dmRows.Next() {
			var req pendingRequest
			if err := dmRows.Scan(&req.ID, &req.ChannelID, &req.UserID, &req.CreatedAt); err != nil {
				continue
			}
			requests = append(requests, req)
		}
	}

	c.JSON(http.StatusOK, gin.H{"pending_requests": requests})
}

// GetUnifiedKeys resolves a context_id to either a server channel or DM conversation
// and returns the caller's wrapped key.
// GET /e2ee/keys/:context_id
func (h *Handler) GetUnifiedKeys(c *gin.Context) {
	userID := c.GetString("user_id")
	contextID := c.Param("context_id")

	if _, err := uuid.Parse(contextID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid context ID"})
		return
	}

	// Try server channel first
	var channelEncrypted bool
	err := h.db.QueryRow(`
		SELECT c.is_encrypted FROM channels c
		INNER JOIN server_members sm ON c.server_id = sm.server_id AND sm.user_id = $2
		WHERE c.id = $1
	`, contextID, userID).Scan(&channelEncrypted)

	if err == nil {
		// Found as server channel
		if !channelEncrypted {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Channel is not encrypted"})
			return
		}

		var key models.ChannelKey
		if versionStr := c.Query("version"); versionStr != "" {
			var version int
			if _, scanErr := fmt.Sscanf(versionStr, "%d", &version); scanErr == nil && version > 0 {
				err = h.db.QueryRow(
					`SELECT id, channel_id, user_id, wrapped_key, key_version, created_at
					 FROM channel_keys
					 WHERE channel_id = $1 AND user_id = $2 AND key_version = $3`,
					contextID, userID, version,
				).Scan(&key.ID, &key.ChannelID, &key.UserID, &key.WrappedKey, &key.KeyVersion, &key.CreatedAt)
			} else {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid version parameter"})
				return
			}
		} else {
			err = h.db.QueryRow(
				`SELECT id, channel_id, user_id, wrapped_key, key_version, created_at
				 FROM channel_keys
				 WHERE channel_id = $1 AND user_id = $2
				 ORDER BY key_version DESC LIMIT 1`,
				contextID, userID,
			).Scan(&key.ID, &key.ChannelID, &key.UserID, &key.WrappedKey, &key.KeyVersion, &key.CreatedAt)
		}
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "No encryption key available yet", "pending": true})
			return
		} else if err != nil {
			h.log.Error("Failed to fetch channel key", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch keys"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"key": key, "context_type": "channel"})
		return
	}

	if err != sql.ErrNoRows {
		h.log.Error("Failed to check channel", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch keys"})
		return
	}

	// Try DM conversation
	var dmEncrypted bool
	err = h.db.QueryRow(`
		SELECT dc.is_encrypted FROM dm_conversations dc
		INNER JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $2
		WHERE dc.id = $1
	`, contextID, userID).Scan(&dmEncrypted)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Context not found or access denied"})
		return
	} else if err != nil {
		h.log.Error("Failed to check DM conversation", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch keys"})
		return
	}

	if !dmEncrypted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Conversation is not encrypted"})
		return
	}

	type dmKey struct {
		ID             string `json:"id"`
		ConversationID string `json:"conversation_id"`
		UserID         string `json:"user_id"`
		WrappedKey     string `json:"wrapped_key"`
		KeyVersion     int    `json:"key_version"`
		CreatedAt      string `json:"created_at"`
	}

	var key dmKey
	err = h.db.QueryRow(`
		SELECT id, conversation_id, user_id, wrapped_key, key_version, created_at
		FROM dm_channel_keys
		WHERE conversation_id = $1 AND user_id = $2
		ORDER BY key_version DESC LIMIT 1
	`, contextID, userID).Scan(&key.ID, &key.ConversationID, &key.UserID, &key.WrappedKey, &key.KeyVersion, &key.CreatedAt)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "No encryption key available yet", "pending": true})
		return
	} else if err != nil {
		h.log.Error("Failed to fetch DM key", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch keys"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"key": key, "context_type": "dm"})
}

// DistributeUnifiedKeys resolves a context_id and distributes wrapped keys.
// POST /e2ee/keys/:context_id
func (h *Handler) DistributeUnifiedKeys(c *gin.Context) {
	userID := c.GetString("user_id")
	contextID := c.Param("context_id")

	if _, err := uuid.Parse(contextID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid context ID"})
		return
	}

	var req DistributeChannelKeysRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	// Try server channel first
	var isChannel bool
	err := h.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM channels c
			INNER JOIN server_members sm ON c.server_id = sm.server_id AND sm.user_id = $2
			WHERE c.id = $1 AND c.is_encrypted = TRUE
		)
	`, contextID, userID).Scan(&isChannel)
	if err != nil {
		h.log.Error("Failed to check channel", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to distribute keys"})
		return
	}

	if isChannel {
		// Delegate to existing channel key distribution logic
		c.Params = append(c.Params, gin.Param{Key: "id", Value: contextID})
		h.DistributeChannelKeys(c)
		return
	}

	// Try DM conversation
	var isDM bool
	err = h.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM dm_conversations dc
			INNER JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $2
			WHERE dc.id = $1 AND dc.is_encrypted = TRUE
		)
	`, contextID, userID).Scan(&isDM)
	if err != nil || !isDM {
		c.JSON(http.StatusNotFound, gin.H{"error": "Context not found or access denied"})
		return
	}

	// Distribute DM keys
	keyVersion := 1
	_ = h.db.QueryRow(`SELECT COALESCE(MAX(key_version), 0) + 1 FROM dm_channel_keys WHERE conversation_id = $1`, contextID).Scan(&keyVersion)

	distributed := 0
	for memberUserID, wrappedKey := range req.WrappedKeys {
		if _, parseErr := uuid.Parse(memberUserID); parseErr != nil {
			continue
		}

		result, err := h.db.Exec(`
			INSERT INTO dm_channel_keys (conversation_id, user_id, wrapped_key, key_version)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (conversation_id, user_id, key_version) DO NOTHING
		`, contextID, memberUserID, wrappedKey, keyVersion)
		if err != nil {
			continue
		}
		rowsAffected, _ := result.RowsAffected()
		if rowsAffected > 0 {
			_, _ = h.db.Exec(`DELETE FROM dm_pending_key_requests WHERE conversation_id = $1 AND user_id = $2`, contextID, memberUserID)

			if h.hub != nil {
				if recipientUUID, parseErr := uuid.Parse(memberUserID); parseErr == nil {
					h.hub.BroadcastToUser(recipientUUID, websocket.OutgoingMessage{
						Type: "key_delivered",
						Data: map[string]interface{}{
							"channel_id": contextID,
							"user_id":    memberUserID,
						},
					})
				}
			}
			distributed++
		}
	}

	c.JSON(http.StatusOK, gin.H{"distributed": distributed, "context_type": "dm"})
}

// RotateKey handles manual seal & rotate for server channel E2EE.
// POST /channels/:id/rotate-key
func (h *Handler) RotateKey(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid channel ID"})
		return
	}

	// Verify user is admin/owner of this channel's server
	var role string
	var serverID string
	err := h.db.QueryRow(`
		SELECT c.server_id, sm.role FROM channels c
		INNER JOIN server_members sm ON c.server_id = sm.server_id AND sm.user_id = $2
		WHERE c.id = $1 AND c.is_encrypted = TRUE
	`, channelID, userID).Scan(&serverID, &role)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Channel not found or not encrypted"})
		return
	} else if err != nil {
		h.log.Error("Failed to check permissions for rotation", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to rotate key"})
		return
	}

	if role != "owner" && role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only owners and admins can rotate channel keys"})
		return
	}

	// TODO: Rate limit via Redis (3/day basic, 10/day premium per channel). For now, accept all.

	var maxVersion int
	_ = h.db.QueryRow(`SELECT COALESCE(MAX(key_version), 0) FROM channel_keys WHERE channel_id = $1`, channelID).Scan(&maxVersion)

	h.log.Info("Channel key rotation requested", "channel_id", channelID, "user_id", userID, "current_version", maxVersion)

	// Broadcast key_rotation to all server subscribers
	if h.hub != nil {
		if serverUUID, parseErr := uuid.Parse(serverID); parseErr == nil {
			h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
				Type: "key_rotation",
				Data: map[string]interface{}{
					"channel_id":      channelID,
					"server_id":       serverID,
					"triggered_by":    userID,
					"new_key_version": maxVersion + 1,
				},
			})
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"message":         "Key rotation initiated",
		"new_key_version": maxVersion + 1,
	})
}

// ValidateEpochs checks if any of the client's cached key epochs have been revoked.
// Called on reconnect to catch missed key_revocation WebSocket events.
// POST /api/channels/validate-epochs
func (h *Handler) ValidateEpochs(c *gin.Context) {
	userID := c.GetString("user_id")

	var req struct {
		Epochs map[string]int `json:"epochs" binding:"required"` // channel_id → current cached epoch
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	type revocationInfo struct {
		ChannelID      string `json:"channel_id"`
		RevokedEpoch   int    `json:"revoked_epoch"`
		SuccessorEpoch int    `json:"successor_epoch"`
		Reason         string `json:"reason"`
	}

	var revocations []revocationInfo

	for channelID, clientEpoch := range req.Epochs {
		if _, parseErr := uuid.Parse(channelID); parseErr != nil {
			continue
		}

		// Verify the user has access to this channel (member of server or DM participant)
		var hasAccess bool
		_ = h.db.QueryRow(
			`SELECT EXISTS(
				SELECT 1 FROM channels c
				INNER JOIN server_members sm ON c.server_id = sm.server_id
				WHERE c.id = $1 AND sm.user_id = $2
			)`,
			channelID, userID,
		).Scan(&hasAccess)
		if !hasAccess {
			continue
		}

		// Check if the client's epoch has been revoked
		var revokedEpoch, successorEpoch int
		var reason string
		err := h.db.QueryRow(
			`SELECT revoked_epoch, successor_epoch, reason
			 FROM key_revocations
			 WHERE channel_id = $1 AND revoked_epoch = $2`,
			channelID, clientEpoch,
		).Scan(&revokedEpoch, &successorEpoch, &reason)
		if err == nil {
			revocations = append(revocations, revocationInfo{
				ChannelID:      channelID,
				RevokedEpoch:   revokedEpoch,
				SuccessorEpoch: successorEpoch,
				Reason:         reason,
			})
		}
	}

	if revocations == nil {
		revocations = []revocationInfo{}
	}

	c.JSON(http.StatusOK, gin.H{"revocations": revocations})
}
