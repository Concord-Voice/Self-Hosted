// Package channels provides handlers for managing server channels.
package channels

import (
	"database/sql"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/models"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

const (
	errMsgInvalidServerID         = "Invalid server ID"
	errMsgInvalidChannelID        = "Invalid channel ID"
	errMsgInvalidRequestBody      = "Invalid request body"
	errMsgInsufficientPerms       = "insufficient permissions"
	errMsgNotMemberOfServer       = "Not a member of this server"
	errMsgChannelNotFound         = "Channel not found"
	errMsgChannelNotEncrypted     = "Channel is not encrypted"
	errMsgFailedFetchChannels     = "Failed to fetch channels"
	errMsgFailedCreateChannel     = "Failed to create channel"
	errMsgFailedUpdateChannel     = "Failed to update channel"
	errMsgFailedDeleteChannel     = "Failed to delete channel"
	errMsgFailedCheckMembership   = "Failed to check membership"
	errMsgFailedFetchKeys         = "Failed to fetch keys"
	errMsgFailedFetchUnreadCounts = "Failed to fetch unread counts"
	errMsgFailedMarkServerRead    = "Failed to mark server read"
	errMsgNoEncryptionKey         = "No encryption key available yet"
	errMsgFailedDistributeKeys    = "Failed to distribute keys"
	logMsgFailedCheckPermissions  = "Failed to check permissions"
)

// Handler handles channel-related requests
type Handler struct {
	db       *sql.DB
	log      *logger.Logger
	hub      *websocket.Hub
	resolver *rbac.Resolver
}

// NewHandler creates a new channel handler
func NewHandler(db *sql.DB, log *logger.Logger, hub *websocket.Hub, resolver *rbac.Resolver) *Handler {
	return &Handler{
		db:       db,
		log:      log,
		hub:      hub,
		resolver: resolver,
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

// ListChannels returns all channels in a server that the user has permission to view.
func (h *Handler) ListChannels(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	var isMember bool
	err := h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, userID,
	).Scan(&isMember)
	if err != nil {
		h.log.Error(errMsgFailedCheckMembership, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchChannels})
		return
	}
	if !isMember {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMemberOfServer})
		return
	}

	visibleIDs, err := h.resolver.GetVisibleChannelIDs(c.Request.Context(), serverID, userID)
	if err != nil {
		h.log.Error("Failed to resolve visible channels", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchChannels})
		return
	}

	visibleSet := make(map[string]bool, len(visibleIDs))
	for _, id := range visibleIDs {
		visibleSet[id] = true
	}

	channels, err := h.queryVisibleChannels(serverID, visibleSet)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchChannels})
		return
	}

	groups, err := h.queryChannelGroups(serverID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchChannels})
		return
	}

	c.JSON(http.StatusOK, gin.H{"channels": channels, "channel_groups": groups})
}

func (h *Handler) queryVisibleChannels(serverID string, visibleSet map[string]bool) ([]models.Channel, error) {
	rows, err := h.db.Query(
		`SELECT id, server_id, name, type, description, emoji, is_encrypted, audio_quality_tier, group_id, linked_voice_channel_id, sync_permissions, position, created_at, updated_at
		FROM channels
		WHERE server_id = $1
		ORDER BY position ASC, created_at ASC`,
		serverID,
	)
	if err != nil {
		h.log.Error("Failed to query channels", "error", err)
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var channels []models.Channel
	for rows.Next() {
		ch, scanErr := scanChannel(rows)
		if scanErr != nil {
			h.log.Error("Failed to scan channel", "error", scanErr)
			continue
		}
		if visibleSet[ch.ID] {
			channels = append(channels, ch)
		}
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating channels", "error", err)
		return nil, err
	}
	if channels == nil {
		channels = []models.Channel{}
	}
	return channels, nil
}

func scanChannel(rows *sql.Rows) (models.Channel, error) {
	var ch models.Channel
	err := rows.Scan(
		&ch.ID, &ch.ServerID, &ch.Name, &ch.Type, &ch.Description,
		&ch.Emoji, &ch.IsEncrypted, &ch.AudioQualityTier, &ch.GroupID,
		&ch.LinkedVoiceChannelID, &ch.SyncPermissions, &ch.Position,
		&ch.CreatedAt, &ch.UpdatedAt,
	)
	return ch, err
}

func (h *Handler) queryChannelGroups(serverID string) ([]models.ChannelGroup, error) {
	groupRows, err := h.db.Query(
		`SELECT id, server_id, name, position, created_at, updated_at
		 FROM channel_groups
		 WHERE server_id = $1
		 ORDER BY position ASC, created_at ASC`,
		serverID,
	)
	if err != nil {
		h.log.Error("Failed to query channel groups", "error", err)
		return nil, err
	}
	defer func() { _ = groupRows.Close() }()

	var groups []models.ChannelGroup
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
		return nil, err
	}
	if groups == nil {
		groups = []models.ChannelGroup{}
	}
	return groups, nil
}

// CreateChannel creates a new channel in a server
func (h *Handler) CreateChannel(c *gin.Context) {
	userID := c.GetString("user_id")

	var req CreateChannelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	// Check permission to manage channels
	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), req.ServerID, userID, "", rbac.PermManageChannels)
	if err != nil {
		h.log.Error(logMsgFailedCheckPermissions, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateChannel})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	isEncrypted := h.resolveEncryption(req)

	// If encrypted, wrapped keys are required
	if isEncrypted && len(req.WrappedKeys) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Encrypted channels require wrapped keys for all members"})
		return
	}

	// Start transaction for channel + keys
	tx, err := h.db.Begin()
	if err != nil {
		h.log.Error("Failed to start transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateChannel})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback transaction", "error", rbErr)
		}
	}()

	nextPos := h.computeNextPosition(tx, req.ServerID, req.GroupID)
	channelID := uuid.New().String()

	channel, err := h.insertChannel(tx, channelID, req, isEncrypted, nextPos)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateChannel})
		return
	}

	// Store wrapped channel keys if encrypted
	if isEncrypted && h.storeWrappedKeys(tx, channelID, req.WrappedKeys) != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store encryption keys"})
		return
	}

	// Auto-create linked text channel for voice channels
	linkedTextChannel, ltcErr := h.maybeCreateLinkedTextChannel(tx, req, channelID, isEncrypted, nextPos)
	if ltcErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create linked text channel"})
		return
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateChannel})
		return
	}

	h.log.Info("Channel created", "channel_id", channelID, "server_id", req.ServerID, "user_id", userID, "encrypted", isEncrypted)

	h.broadcastChannelCreated(req.ServerID, channel, linkedTextChannel)

	// Return response: voice channels include the linked text channel
	response := gin.H{"channel": channel}
	if linkedTextChannel != nil {
		response["linked_text_channel"] = linkedTextChannel
	}
	c.JSON(http.StatusCreated, response)
}

// resolveEncryption determines if a channel should be encrypted based on
// the request value or the server's e2ee_default.
func (h *Handler) resolveEncryption(req CreateChannelRequest) bool {
	if req.IsEncrypted != nil {
		return *req.IsEncrypted
	}
	var e2eeDefault bool
	_ = h.db.QueryRow(`SELECT e2ee_default FROM servers WHERE id = $1`, req.ServerID).Scan(&e2eeDefault)
	return e2eeDefault
}

// computeNextPosition returns the next position for a channel within a group (or uncategorized).
func (h *Handler) computeNextPosition(tx *sql.Tx, serverID string, groupID *string) int {
	var maxPos int
	if groupID != nil {
		_ = tx.QueryRow(
			`SELECT COALESCE(MAX(position), -1) FROM channels WHERE server_id = $1 AND group_id = $2`,
			serverID, *groupID,
		).Scan(&maxPos)
	} else {
		_ = tx.QueryRow(
			`SELECT COALESCE(MAX(position), -1) FROM channels WHERE server_id = $1 AND group_id IS NULL`,
			serverID,
		).Scan(&maxPos)
	}
	return maxPos + 1
}

// insertChannel creates the primary channel row within a transaction.
func (h *Handler) insertChannel(tx *sql.Tx, channelID string, req CreateChannelRequest, isEncrypted bool, position int) (models.Channel, error) {
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
	channel.Position = position

	err := tx.QueryRow(insertQuery, channelID, req.ServerID, req.Name, req.Type, req.Emoji, isEncrypted, req.GroupID, position).Scan(
		&channel.CreatedAt,
		&channel.UpdatedAt,
	)
	if err != nil {
		h.log.Error("Failed to create channel", "error", err)
		return channel, err
	}
	return channel, nil
}

// storeWrappedKeys inserts wrapped E2EE keys for a channel within a transaction.
func (h *Handler) storeWrappedKeys(tx *sql.Tx, channelID string, wrappedKeys map[string]string) error {
	keyInsert := `
		INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
		VALUES ($1, $2, $3, 1)
	`
	for memberUserID, wrappedKey := range wrappedKeys {
		if _, parseErr := uuid.Parse(memberUserID); parseErr != nil {
			continue // skip invalid UUIDs
		}
		if _, err := tx.Exec(keyInsert, channelID, memberUserID, wrappedKey); err != nil {
			h.log.Error("Failed to store channel key", "error", err, "user_id", memberUserID)
			return err
		}
	}
	return nil
}

// maybeCreateLinkedTextChannel creates a linked text channel for voice channels, or returns nil for other types.
func (h *Handler) maybeCreateLinkedTextChannel(tx *sql.Tx, req CreateChannelRequest, voiceChannelID string, isEncrypted bool, nextPos int) (*models.Channel, error) {
	if req.Type != "voice" {
		return nil, nil
	}
	return h.createLinkedTextChannel(tx, req, voiceChannelID, isEncrypted, nextPos+1)
}

// createLinkedTextChannel creates a linked text channel for a voice channel.
func (h *Handler) createLinkedTextChannel(tx *sql.Tx, req CreateChannelRequest, voiceChannelID string, isEncrypted bool, position int) (*models.Channel, error) {
	linkedTextID := uuid.New().String()
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
	ltc.LinkedVoiceChannelID = &voiceChannelID
	ltc.Position = position

	err := tx.QueryRow(linkedInsert, linkedTextID, req.ServerID, req.Name, isEncrypted, req.GroupID, voiceChannelID, position).Scan(
		&ltc.CreatedAt,
		&ltc.UpdatedAt,
	)
	if err != nil {
		h.log.Error("Failed to create linked text channel", "error", err)
		return nil, err
	}

	// If encrypted, copy wrapped keys for the linked text channel too (non-fatal)
	if isEncrypted {
		h.storeWrappedKeysNonFatal(tx, linkedTextID, req.WrappedKeys)
	}

	return &ltc, nil
}

// storeWrappedKeysNonFatal stores wrapped keys but does not fail on error (keys can be distributed later).
func (h *Handler) storeWrappedKeysNonFatal(tx *sql.Tx, channelID string, wrappedKeys map[string]string) {
	keyInsert := `
		INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
		VALUES ($1, $2, $3, 1)
	`
	for memberUserID, wrappedKey := range wrappedKeys {
		if _, parseErr := uuid.Parse(memberUserID); parseErr != nil {
			continue
		}
		if _, err := tx.Exec(keyInsert, channelID, memberUserID, wrappedKey); err != nil {
			h.log.Error("Failed to store linked text channel key", "error", err, "user_id", memberUserID)
		}
	}
}

// broadcastChannelCreated sends channel_created events to server subscribers.
func (h *Handler) broadcastChannelCreated(serverID string, channel models.Channel, linkedTextChannel *models.Channel) {
	if h.hub == nil {
		return
	}
	serverUUID, err := uuid.Parse(serverID)
	if err != nil {
		return
	}

	h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
		Type: "channel_created",
		Data: map[string]interface{}{
			"channel": channelToMap(channel),
		},
	})

	if linkedTextChannel != nil {
		h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
			Type: "channel_created",
			Data: map[string]interface{}{
				"channel": channelToMap(*linkedTextChannel),
			},
		})
	}
}

// channelToMap converts a Channel model to a map for broadcast payloads.
func channelToMap(ch models.Channel) map[string]interface{} {
	m := map[string]interface{}{
		"id":           ch.ID,
		"server_id":    ch.ServerID,
		"name":         ch.Name,
		"type":         ch.Type,
		"emoji":        ch.Emoji,
		"is_encrypted": ch.IsEncrypted,
		"group_id":     ch.GroupID,
		"position":     ch.Position,
		"created_at":   ch.CreatedAt,
		"updated_at":   ch.UpdatedAt,
	}
	if ch.LinkedVoiceChannelID != nil {
		m["linked_voice_channel_id"] = ch.LinkedVoiceChannelID
	}
	return m
}

// GetChannel returns a specific channel
func (h *Handler) GetChannel(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	// Validate channel ID
	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}

	// Get channel and check if user is a member of the server
	query := `
		SELECT c.id, c.server_id, c.name, c.type, c.description, c.emoji, c.is_encrypted, c.audio_quality_tier, c.group_id, c.linked_voice_channel_id, c.sync_permissions, c.position, c.created_at, c.updated_at
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
		&channel.SyncPermissions,
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

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}

	var req UpdateChannelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	serverID, err := h.lookupChannelServerID(channelID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgChannelNotFound})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateChannel})
		return
	}

	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", rbac.PermManageChannels)
	if err != nil {
		h.log.Error(logMsgFailedCheckPermissions, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateChannel})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	if req.AudioQualityTier != nil && *req.AudioQualityTier != "" && !validAudioQualityTiers[*req.AudioQualityTier] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid audio quality tier"})
		return
	}

	channel, err := h.executeChannelUpdate(channelID, req)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgChannelNotFound})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateChannel})
		return
	}

	h.log.Info("Channel updated", "channel_id", channelID, "user_id", userID)
	h.broadcastChannelUpdated(channel)
	c.JSON(http.StatusOK, gin.H{"channel": channel})
}

func (h *Handler) lookupChannelServerID(channelID string) (string, error) {
	var serverID string
	err := h.db.QueryRow(`SELECT server_id FROM channels WHERE id = $1`, channelID).Scan(&serverID)
	if err != nil && err != sql.ErrNoRows {
		h.log.Error("Failed to look up channel", "error", err)
	}
	return serverID, err
}

func resolveGroupIDParam(groupID *string) interface{} {
	if groupID == nil {
		return nil
	}
	if *groupID == "" {
		return nil
	}
	return *groupID
}

func (h *Handler) executeChannelUpdate(channelID string, req UpdateChannelRequest) (models.Channel, error) {
	var channel models.Channel
	channel.ID = channelID
	channel.Name = req.Name
	channel.Type = req.Type

	var err error
	if req.GroupID != nil {
		err = h.db.QueryRow(
			`UPDATE channels
			SET name = $1, type = $2, emoji = $3, audio_quality_tier = $4, group_id = $5, updated_at = NOW()
			WHERE id = $6
			RETURNING server_id, emoji, is_encrypted, audio_quality_tier, group_id, linked_voice_channel_id, sync_permissions, position, created_at, updated_at`,
			req.Name, req.Type, req.Emoji, req.AudioQualityTier, resolveGroupIDParam(req.GroupID), channelID,
		).Scan(
			&channel.ServerID, &channel.Emoji, &channel.IsEncrypted, &channel.AudioQualityTier,
			&channel.GroupID, &channel.LinkedVoiceChannelID, &channel.SyncPermissions,
			&channel.Position, &channel.CreatedAt, &channel.UpdatedAt,
		)
	} else {
		err = h.db.QueryRow(
			`UPDATE channels
			SET name = $1, type = $2, emoji = $3, audio_quality_tier = $4, updated_at = NOW()
			WHERE id = $5
			RETURNING server_id, emoji, is_encrypted, audio_quality_tier, group_id, linked_voice_channel_id, sync_permissions, position, created_at, updated_at`,
			req.Name, req.Type, req.Emoji, req.AudioQualityTier, channelID,
		).Scan(
			&channel.ServerID, &channel.Emoji, &channel.IsEncrypted, &channel.AudioQualityTier,
			&channel.GroupID, &channel.LinkedVoiceChannelID, &channel.SyncPermissions,
			&channel.Position, &channel.CreatedAt, &channel.UpdatedAt,
		)
	}
	if err != nil && err != sql.ErrNoRows {
		h.log.Error("Failed to update channel", "error", err)
	}
	return channel, err
}

func (h *Handler) broadcastChannelUpdated(channel models.Channel) {
	if h.hub == nil {
		return
	}
	serverUUID, err := uuid.Parse(channel.ServerID)
	if err != nil {
		return
	}
	h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
		Type: "channel_updated",
		Data: map[string]interface{}{
			"channel_id":         channel.ID,
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

// DeleteChannel deletes a channel (owner/admin only)
func (h *Handler) DeleteChannel(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	// Validate channel ID
	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}

	// Get channel's server ID
	var serverID string
	err := h.db.QueryRow(`SELECT server_id FROM channels WHERE id = $1`, channelID).Scan(&serverID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgChannelNotFound})
		return
	} else if err != nil {
		h.log.Error("Failed to look up channel", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteChannel})
		return
	}

	// Check permission to manage channels
	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", rbac.PermManageChannels)
	if err != nil {
		h.log.Error(logMsgFailedCheckPermissions, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteChannel})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	// Delete channel
	deleteQuery := `DELETE FROM channels WHERE id = $1`

	_, err = h.db.Exec(deleteQuery, channelID)
	if err != nil {
		h.log.Error("Failed to delete channel", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteChannel})
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
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	// Verify membership
	var isMember bool
	err := h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, userID,
	).Scan(&isMember)
	if err != nil {
		h.log.Error(errMsgFailedCheckMembership, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchUnreadCounts})
		return
	}
	if !isMember {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMemberOfServer})
		return
	}

	// For each channel in the server, count messages newer than last_read_at.
	// If no read state exists, fall back to the user's join date so pre-existing
	// messages are not counted as unread for first-time members.
	// Uses JOINs instead of correlated subqueries for better query planning.
	query := `
		SELECT ch.id,
			COUNT(m.id)::int AS unread_count
		FROM channels ch
		CROSS JOIN (
			SELECT joined_at FROM server_members WHERE server_id = $1 AND user_id = $2
		) sm
		LEFT JOIN channel_read_states crs
			ON crs.channel_id = ch.id AND crs.user_id = $2
		LEFT JOIN messages m
			ON m.channel_id = ch.id
			AND m.user_id != $2
			AND m.created_at > COALESCE(crs.last_read_at, sm.joined_at)
		WHERE ch.server_id = $1
		GROUP BY ch.id
	`

	rows, err := h.db.Query(query, serverID, userID)
	if err != nil {
		h.log.Error("Failed to query unread counts", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchUnreadCounts})
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchUnreadCounts})
		return
	}

	c.JSON(http.StatusOK, gin.H{"unreads": unreads})
}

// GetServerUnreadStatus returns a list of server IDs where the user has unread messages.
// Used to show unread dots on server icons without fetching per-channel counts for every server.
func (h *Handler) GetServerUnreadStatus(c *gin.Context) {
	userID := c.GetString("user_id")

	// Uses a LEFT JOIN to channel_read_states instead of a correlated subquery
	// so the planner can use a hash/merge join instead of nested-loop per row.
	query := `
		SELECT DISTINCT ch.server_id
		FROM channels ch
		INNER JOIN server_members sm
			ON ch.server_id = sm.server_id AND sm.user_id = $1
		LEFT JOIN channel_read_states crs
			ON crs.channel_id = ch.id AND crs.user_id = $1
		INNER JOIN messages m
			ON m.channel_id = ch.id
			AND m.user_id != $1
			AND m.created_at > COALESCE(crs.last_read_at, sm.joined_at)
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
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
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
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMemberOfServer})
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
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return
	}

	// Verify membership
	var isMember bool
	err := h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, userID,
	).Scan(&isMember)
	if err != nil {
		h.log.Error(errMsgFailedCheckMembership, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedMarkServerRead})
		return
	}
	if !isMember {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMemberOfServer})
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedMarkServerRead})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "All channels marked as read"})
}

// GetChannelKeys returns the caller's wrapped channel key for an E2EE channel.
func (h *Handler) GetChannelKeys(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
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
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgChannelNotEncrypted})
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
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgNoEncryptionKey, "pending": true})
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
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}

	var req DistributeChannelKeysRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	if err := h.verifyChannelEncrypted(channelID, userID); err != nil {
		h.respondKeyDistError(c, err)
		return
	}

	if !h.callerHasChannelKey(channelID, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You must have the channel key to distribute keys"})
		return
	}

	targetKeyVersion := h.resolveTargetKeyVersion(channelID, req.KeyVersion)

	distributed, duplicates := h.distributeChannelKeysToMembers(channelID, req.WrappedKeys, targetKeyVersion)

	h.log.Info("Channel keys distributed",
		"channel_id", channelID, "by_user", userID,
		"distributed", distributed, "duplicates", duplicates)

	c.JSON(http.StatusOK, gin.H{
		"distributed": distributed,
		"duplicates":  duplicates,
	})
}

type keyDistError struct {
	status  int
	message string
}

func (e *keyDistError) Error() string { return e.message }

func (h *Handler) verifyChannelEncrypted(channelID, userID string) error {
	var isEncrypted bool
	err := h.db.QueryRow(
		`SELECT c.is_encrypted
		 FROM channels c
		 INNER JOIN server_members sm ON c.server_id = sm.server_id
		 WHERE c.id = $1 AND sm.user_id = $2`,
		channelID, userID,
	).Scan(&isEncrypted)
	if err == sql.ErrNoRows {
		return &keyDistError{http.StatusForbidden, "Not a member of this channel's server"}
	}
	if err != nil {
		h.log.Error(logMsgFailedCheckPermissions, "error", err)
		return &keyDistError{http.StatusInternalServerError, errMsgFailedDistributeKeys}
	}
	if !isEncrypted {
		return &keyDistError{http.StatusBadRequest, errMsgChannelNotEncrypted}
	}
	return nil
}

func (h *Handler) respondKeyDistError(c *gin.Context, err error) {
	if kde, ok := err.(*keyDistError); ok {
		c.JSON(kde.status, gin.H{"error": kde.message})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDistributeKeys})
}

func (h *Handler) callerHasChannelKey(channelID, userID string) bool {
	var hasKey bool
	_ = h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM channel_keys WHERE channel_id = $1 AND user_id = $2)`,
		channelID, userID,
	).Scan(&hasKey)
	return hasKey
}

func (h *Handler) resolveTargetKeyVersion(channelID string, explicitVersion *int) int {
	if explicitVersion != nil && *explicitVersion > 0 {
		return *explicitVersion
	}
	var v int
	_ = h.db.QueryRow(
		`SELECT COALESCE(MAX(key_version), 1) FROM channel_keys WHERE channel_id = $1`,
		channelID,
	).Scan(&v)
	return v
}

func (h *Handler) distributeChannelKeysToMembers(channelID string, wrappedKeys map[string]string, keyVersion int) (distributed, duplicates int) {
	for memberUserID, wrappedKey := range wrappedKeys {
		if _, parseErr := uuid.Parse(memberUserID); parseErr != nil {
			continue
		}

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

		_, _ = h.db.Exec(
			`DELETE FROM pending_key_requests WHERE channel_id = $1 AND user_id = $2`,
			channelID, memberUserID,
		)

		h.notifyKeyDelivered(channelID, memberUserID)
		distributed++
	}
	return
}

func (h *Handler) notifyKeyDelivered(contextID, memberUserID string) {
	if h.hub == nil {
		return
	}
	recipientUUID, err := uuid.Parse(memberUserID)
	if err != nil {
		return
	}
	h.hub.BroadcastToUser(recipientUUID, websocket.OutgoingMessage{
		Type: "key_delivered",
		Data: map[string]interface{}{
			"channel_id": contextID,
			"user_id":    memberUserID,
		},
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

	var channelEncrypted bool
	err := h.db.QueryRow(`
		SELECT c.is_encrypted FROM channels c
		INNER JOIN server_members sm ON c.server_id = sm.server_id AND sm.user_id = $2
		WHERE c.id = $1
	`, contextID, userID).Scan(&channelEncrypted)

	if err == nil {
		h.getChannelKeyResponse(c, contextID, userID, channelEncrypted)
		return
	}
	if err != sql.ErrNoRows {
		h.log.Error("Failed to check channel", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchKeys})
		return
	}

	h.getDMKeyResponse(c, contextID, userID)
}

func (h *Handler) getChannelKeyResponse(c *gin.Context, contextID, userID string, encrypted bool) {
	if !encrypted {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgChannelNotEncrypted})
		return
	}

	key, err := h.fetchChannelKey(contextID, userID, c.Query("version"))
	if err == errInvalidVersion {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid version parameter"})
		return
	}
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgNoEncryptionKey, "pending": true})
		return
	}
	if err != nil {
		h.log.Error("Failed to fetch channel key", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchKeys})
		return
	}

	c.JSON(http.StatusOK, gin.H{"key": key, "context_type": "channel"})
}

var errInvalidVersion = fmt.Errorf("invalid version parameter")

func (h *Handler) fetchChannelKey(channelID, userID, versionStr string) (models.ChannelKey, error) {
	var key models.ChannelKey
	if versionStr != "" {
		var version int
		if _, scanErr := fmt.Sscanf(versionStr, "%d", &version); scanErr != nil || version <= 0 {
			return key, errInvalidVersion
		}
		err := h.db.QueryRow(
			`SELECT id, channel_id, user_id, wrapped_key, key_version, created_at
			 FROM channel_keys
			 WHERE channel_id = $1 AND user_id = $2 AND key_version = $3`,
			channelID, userID, version,
		).Scan(&key.ID, &key.ChannelID, &key.UserID, &key.WrappedKey, &key.KeyVersion, &key.CreatedAt)
		return key, err
	}
	err := h.db.QueryRow(
		`SELECT id, channel_id, user_id, wrapped_key, key_version, created_at
		 FROM channel_keys
		 WHERE channel_id = $1 AND user_id = $2
		 ORDER BY key_version DESC LIMIT 1`,
		channelID, userID,
	).Scan(&key.ID, &key.ChannelID, &key.UserID, &key.WrappedKey, &key.KeyVersion, &key.CreatedAt)
	return key, err
}

type dmKey struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	UserID         string `json:"user_id"`
	WrappedKey     string `json:"wrapped_key"`
	KeyVersion     int    `json:"key_version"`
	CreatedAt      string `json:"created_at"`
}

func (h *Handler) getDMKeyResponse(c *gin.Context, contextID, userID string) {
	var dmEncrypted bool
	err := h.db.QueryRow(`
		SELECT dc.is_encrypted FROM dm_conversations dc
		INNER JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $2
		WHERE dc.id = $1
	`, contextID, userID).Scan(&dmEncrypted)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Context not found or access denied"})
		return
	}
	if err != nil {
		h.log.Error("Failed to check DM conversation", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchKeys})
		return
	}
	if !dmEncrypted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Conversation is not encrypted"})
		return
	}

	var key dmKey
	err = h.db.QueryRow(`
		SELECT id, conversation_id, user_id, wrapped_key, key_version, created_at
		FROM dm_channel_keys
		WHERE conversation_id = $1 AND user_id = $2
		ORDER BY key_version DESC LIMIT 1
	`, contextID, userID).Scan(&key.ID, &key.ConversationID, &key.UserID, &key.WrappedKey, &key.KeyVersion, &key.CreatedAt)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgNoEncryptionKey, "pending": true})
		return
	}
	if err != nil {
		h.log.Error("Failed to fetch DM key", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchKeys})
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

	// Don't parse the request body yet — route to channel or DM first.
	// DistributeChannelKeys will parse the body itself; parsing here would
	// consume the one-shot io.ReadCloser, causing a double-read 400 on delegation.

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
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDistributeKeys})
		return
	}

	if isChannel {
		c.Params = append(c.Params, gin.Param{Key: "id", Value: contextID})
		h.DistributeChannelKeys(c)
		return
	}

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

	var req DistributeChannelKeysRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	distributed := h.distributeDMKeys(contextID, req.WrappedKeys)
	c.JSON(http.StatusOK, gin.H{"distributed": distributed, "context_type": "dm"})
}

func (h *Handler) distributeDMKeys(conversationID string, wrappedKeys map[string]string) int {
	keyVersion := 1
	_ = h.db.QueryRow(
		`SELECT COALESCE(MAX(key_version), 0) + 1 FROM dm_channel_keys WHERE conversation_id = $1`,
		conversationID,
	).Scan(&keyVersion)

	distributed := 0
	for memberUserID, wrappedKey := range wrappedKeys {
		if _, parseErr := uuid.Parse(memberUserID); parseErr != nil {
			continue
		}

		result, err := h.db.Exec(`
			INSERT INTO dm_channel_keys (conversation_id, user_id, wrapped_key, key_version)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (conversation_id, user_id, key_version) DO NOTHING
		`, conversationID, memberUserID, wrappedKey, keyVersion)
		if err != nil {
			continue
		}
		rowsAffected, _ := result.RowsAffected()
		if rowsAffected == 0 {
			continue
		}

		_, _ = h.db.Exec(
			`DELETE FROM dm_pending_key_requests WHERE conversation_id = $1 AND user_id = $2`,
			conversationID, memberUserID,
		)
		h.notifyKeyDelivered(conversationID, memberUserID)
		distributed++
	}
	return distributed
}

// RotateKey handles manual seal & rotate for server channel E2EE.
// POST /channels/:id/rotate-key
func (h *Handler) RotateKey(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}

	// Look up channel's server and verify it's encrypted
	var serverID string
	var isEncrypted bool
	err := h.db.QueryRow(
		`SELECT server_id, is_encrypted FROM channels WHERE id = $1`, channelID,
	).Scan(&serverID, &isEncrypted)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgChannelNotFound})
		return
	} else if err != nil {
		h.log.Error("Failed to look up channel for rotation", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to rotate key"})
		return
	}
	if !isEncrypted {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgChannelNotEncrypted})
		return
	}

	// Check permission to manage crypto rotation
	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", rbac.PermManageCryptoRotation)
	if err != nil {
		h.log.Error("Failed to check permissions for rotation", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to rotate key"})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
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
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
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
