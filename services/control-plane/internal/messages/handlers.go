// Package messages provides handlers for managing chat messages.
package messages

import (
	"database/sql"
	"encoding/base64"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/models"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

const (
	errMsgInvalidRequestBody     = "Invalid request body"
	errMsgInvalidMessageID       = "Invalid message ID"
	errMsgInsufficientPerms      = "Insufficient permissions"
	errMsgMessageNotFound        = "Message not found"
	errMsgFailedCheckPerms       = "Failed to check permissions"
	errMsgFailedFetchMessages    = "Failed to fetch messages"
	errMsgFailedSendMessage      = "Failed to send message"
	errMsgFailedUpdateMessage    = "Failed to update message"
	errMsgFailedDeleteMessage    = "Failed to delete message"
	errMsgFailedSuppressEmbeds   = "Failed to suppress embeds"
	errMsgInsufficientPermsLower = "insufficient permissions"
)

// minCiphertextSize is the minimum base64-decoded size for a valid AES-GCM ciphertext:
// 12 bytes IV + 16 bytes auth tag = 28 bytes minimum (empty plaintext).
const minCiphertextSize = 28

// isValidCiphertext checks that content is valid base64 and meets the minimum
// size for an AES-256-GCM ciphertext (12-byte IV + 16-byte auth tag).
func isValidCiphertext(content string) bool {
	decoded, err := base64.StdEncoding.DecodeString(content)
	if err != nil {
		return false
	}
	return len(decoded) >= minCiphertextSize
}

// Handler handles message-related requests
type Handler struct {
	db       *sql.DB
	log      *logger.Logger
	hub      *websocket.Hub
	resolver *rbac.Resolver
}

// NewHandler creates a new message handler
func NewHandler(db *sql.DB, log *logger.Logger, hub *websocket.Hub, resolver *rbac.Resolver) *Handler {
	return &Handler{
		db:       db,
		log:      log,
		hub:      hub,
		resolver: resolver,
	}
}

// SendMessageRequest represents a request to send a message.
// Max content length is 24000 to accommodate encrypted messages (AES-GCM + base64 overhead).
// Plaintext messages are limited to 4000 chars on the client side.
type SendMessageRequest struct {
	ChannelID   string `json:"channel_id" binding:"required,uuid"`
	Content     string `json:"content" binding:"required,min=1,max=24000"`
	IsEncrypted bool   `json:"is_encrypted"`
	KeyVersion  int    `json:"key_version"`
	MentionMeta string `json:"mention_meta,omitempty"` // Accepted but unused — mention routing is WebSocket-only. Field exists so REST clients don't get a 400 for including it.
}

// UpdateMessageRequest represents a request to update a message.
// Max content length is 24000 to accommodate encrypted messages.
type UpdateMessageRequest struct {
	Content string `json:"content" binding:"required,min=1,max=24000"`
}

// GetMessages returns message history for a channel
func (h *Handler) GetMessages(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	// Validate channel ID
	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid channel ID"})
		return
	}

	// Pagination parameters
	limit := 50 // Default limit
	if limitParam := c.Query("limit"); limitParam != "" {
		if l, err := strconv.Atoi(limitParam); err == nil && l > 0 && l <= 100 {
			limit = l
		}
	}

	before := c.Query("before") // Message ID to get messages before (for pagination)
	if before != "" {
		if _, err := uuid.Parse(before); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid 'before' parameter"})
			return
		}
	}

	// Check membership and get server ID for RBAC
	var serverID string
	err := h.db.QueryRow(`
		SELECT c.server_id FROM channels c
		INNER JOIN server_members sm ON c.server_id = sm.server_id
		WHERE c.id = $1 AND sm.user_id = $2
	`, channelID, userID).Scan(&serverID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this channel's server"})
		return
	}
	if err != nil {
		h.log.Error("Failed to check membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchMessages})
		return
	}

	// Check PermReadMessageHistory (with channelID for SBAC per-channel overrides)
	hasPerm, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, rbac.PermReadMessageHistory)
	if permErr != nil {
		h.log.Error(errMsgFailedCheckPerms, "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchMessages})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	// Build query based on pagination
	var query string
	var args []interface{}

	if before != "" {
		// Get messages before a specific message ID
		query = `
			SELECT m.id, m.channel_id, m.user_id, m.content, m.is_encrypted, COALESCE(m.key_version, 1),
			       m.embeds_suppressed, m.edited_at, m.created_at, m.updated_at, u.username, u.display_name, u.avatar_url
			FROM messages m
			INNER JOIN users u ON m.user_id = u.id
			WHERE m.channel_id = $1
			  AND m.created_at < (SELECT created_at FROM messages WHERE id = $2)
			ORDER BY m.created_at DESC
			LIMIT $3
		`
		args = []interface{}{channelID, before, limit}
	} else {
		// Get most recent messages
		query = `
			SELECT m.id, m.channel_id, m.user_id, m.content, m.is_encrypted, COALESCE(m.key_version, 1),
			       m.embeds_suppressed, m.edited_at, m.created_at, m.updated_at, u.username, u.display_name, u.avatar_url
			FROM messages m
			INNER JOIN users u ON m.user_id = u.id
			WHERE m.channel_id = $1
			ORDER BY m.created_at DESC
			LIMIT $2
		`
		args = []interface{}{channelID, limit}
	}

	rows, err := h.db.Query(query, args...)
	if err != nil {
		h.log.Error("Failed to query messages", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchMessages})
		return
	}
	defer func() { _ = rows.Close() }()

	messages := []models.MessageWithUser{}
	for rows.Next() {
		var msg models.MessageWithUser
		err := rows.Scan(
			&msg.ID,
			&msg.ChannelID,
			&msg.UserID,
			&msg.Content,
			&msg.IsEncrypted,
			&msg.KeyVersion,
			&msg.EmbedsSuppressed,
			&msg.EditedAt,
			&msg.CreatedAt,
			&msg.UpdatedAt,
			&msg.Username,
			&msg.DisplayName,
			&msg.AvatarURL,
		)
		if err != nil {
			h.log.Error("Failed to scan message", "error", err)
			continue
		}
		messages = append(messages, msg)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating messages", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchMessages})
		return
	}

	// Batch-load reactions for all fetched messages
	if len(messages) > 0 {
		messageIDs := make([]string, len(messages))
		for i, m := range messages {
			messageIDs[i] = m.ID
		}
		reactionMap, loadErr := loadReactionsForMessages(h.db, messageIDs, userID)
		if loadErr != nil {
			h.log.Error("Failed to load reactions", "error", loadErr)
			// Non-fatal: return messages without reactions
		} else {
			for i := range messages {
				if reactions, ok := reactionMap[messages[i].ID]; ok {
					messages[i].Reactions = reactions
				}
			}
		}
	}

	// Return DESC order (newest first) — consistent with DM message handler.
	// The client reverses to ASC for chronological display.
	c.JSON(http.StatusOK, gin.H{
		"messages": messages,
		"count":    len(messages),
	})
}

// SendMessage sends a new message to a channel
func (h *Handler) SendMessage(c *gin.Context) {
	userID := c.GetString("user_id")

	var req SendMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	// Check membership, get server ID for RBAC, and fetch server embed policy
	var serverID string
	var serverAllowEmbeds bool
	err := h.db.QueryRow(`
		SELECT c.server_id, s.allow_embedded_content
		FROM channels c
		INNER JOIN server_members sm ON c.server_id = sm.server_id
		INNER JOIN servers s ON c.server_id = s.id
		WHERE c.id = $1 AND sm.user_id = $2
	`, req.ChannelID, userID).Scan(&serverID, &serverAllowEmbeds)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this channel's server"})
		return
	}
	if err != nil {
		h.log.Error("Failed to check membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSendMessage})
		return
	}

	// Check PermSendMessages (with channelID for SBAC per-channel overrides)
	hasPerm, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, req.ChannelID, rbac.PermSendMessages)
	if permErr != nil {
		h.log.Error(errMsgFailedCheckPerms, "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSendMessage})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	// E2EE enforcement: if channel is encrypted, message MUST be encrypted with valid ciphertext
	var channelEncrypted bool
	encQuery := `SELECT is_encrypted FROM channels WHERE id = $1`
	if err := h.db.QueryRow(encQuery, req.ChannelID).Scan(&channelEncrypted); err != nil {
		h.log.Error("Failed to check channel encryption status", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSendMessage})
		return
	}
	if channelEncrypted && !req.IsEncrypted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "E2EE channel requires encrypted messages"})
		return
	}
	if channelEncrypted && !isValidCiphertext(req.Content) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid ciphertext format for E2EE channel"})
		return
	}

	// Resolve key_version (default to 1 if not provided)
	keyVersion := req.KeyVersion
	if keyVersion <= 0 {
		keyVersion = 1
	}

	// Epoch enforcement: reject messages encrypted with a revoked key epoch (fail-closed)
	if req.IsEncrypted && keyVersion > 0 {
		var epochRevoked bool
		if err := h.db.QueryRow(
			`SELECT EXISTS(SELECT 1 FROM key_revocations WHERE channel_id = $1 AND revoked_epoch = $2)`,
			req.ChannelID, keyVersion,
		).Scan(&epochRevoked); err != nil {
			h.log.Error("Failed to check epoch revocation", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify key epoch"})
			return
		}
		if epochRevoked {
			currentEpoch := 1
			if err := h.db.QueryRow(
				`SELECT COALESCE(MAX(key_version), 1) FROM channel_keys WHERE channel_id = $1`,
				req.ChannelID,
			).Scan(&currentEpoch); err != nil {
				h.log.Error("Failed to fetch current epoch", "error", err, "channel_id", req.ChannelID)
			}
			c.JSON(http.StatusConflict, gin.H{
				"error":         "Key epoch has been revoked — re-encrypt with current epoch",
				"code":          "epoch_revoked",
				"current_epoch": currentEpoch,
				"channel_id":    req.ChannelID,
			})
			return
		}
	}

	// Create message — stamp embeds_suppressed based on server policy.
	// The server is the ONLY entity trusted to set this flag to false (allow).
	// If server policy is OFF (default), embeds_suppressed = true.
	embedsSuppressed := !serverAllowEmbeds

	messageID := uuid.New().String()
	insertQuery := `
		INSERT INTO messages (id, channel_id, user_id, content, is_encrypted, key_version, embeds_suppressed, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), NOW())
		RETURNING created_at, updated_at
	`

	var message models.Message
	message.ID = messageID
	message.ChannelID = req.ChannelID
	message.UserID = userID
	message.Content = req.Content
	message.IsEncrypted = req.IsEncrypted
	message.KeyVersion = keyVersion
	message.EmbedsSuppressed = embedsSuppressed

	err = h.db.QueryRow(insertQuery, messageID, req.ChannelID, userID, req.Content, req.IsEncrypted, keyVersion, embedsSuppressed).Scan(
		&message.CreatedAt,
		&message.UpdatedAt,
	)
	if err != nil {
		h.log.Error("Failed to create message", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSendMessage})
		return
	}

	h.log.Info("Message sent", "message_id", messageID, "channel_id", req.ChannelID, "user_id", userID)

	c.JSON(http.StatusCreated, gin.H{"message": message})
}

// UpdateMessage updates a message's content
func (h *Handler) UpdateMessage(c *gin.Context) {
	userID := c.GetString("user_id")
	messageID := c.Param("id")

	// Validate message ID
	if _, err := uuid.Parse(messageID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidMessageID})
		return
	}

	var req UpdateMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	// Check if message exists and user is the author
	var authorID string
	authorQuery := `SELECT user_id FROM messages WHERE id = $1`

	err := h.db.QueryRow(authorQuery, messageID).Scan(&authorID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgMessageNotFound})
		return
	} else if err != nil {
		h.log.Error("Failed to check message author", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateMessage})
		return
	}

	if authorID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "You can only edit your own messages"})
		return
	}

	// E2EE enforcement: if the message was originally encrypted, updates must also be encrypted
	var msgEncrypted bool
	encCheckQuery := `SELECT COALESCE(c.is_encrypted, false) FROM messages m INNER JOIN channels c ON m.channel_id = c.id WHERE m.id = $1`
	if err := h.db.QueryRow(encCheckQuery, messageID).Scan(&msgEncrypted); err != nil {
		h.log.Error("Failed to check channel encryption for update", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateMessage})
		return
	}
	if msgEncrypted && !isValidCiphertext(req.Content) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid ciphertext format for E2EE channel"})
		return
	}

	// Update message
	updateQuery := `
		UPDATE messages
		SET content = $1, edited_at = NOW(), updated_at = NOW()
		WHERE id = $2
		RETURNING channel_id, is_encrypted, key_version, embeds_suppressed, edited_at, created_at, updated_at
	`

	var message models.Message
	message.ID = messageID
	message.UserID = userID
	message.Content = req.Content

	err = h.db.QueryRow(updateQuery, req.Content, messageID).Scan(
		&message.ChannelID,
		&message.IsEncrypted,
		&message.KeyVersion,
		&message.EmbedsSuppressed,
		&message.EditedAt,
		&message.CreatedAt,
		&message.UpdatedAt,
	)

	if err != nil {
		h.log.Error("Failed to update message", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateMessage})
		return
	}

	h.log.Info("Message updated", "message_id", messageID, "user_id", userID)

	// Broadcast update to channel subscribers via WebSocket
	channelUUID, err := uuid.Parse(message.ChannelID)
	if err == nil {
		h.hub.BroadcastToChannel(channelUUID, websocket.OutgoingMessage{
			Type: "message_update",
			Data: map[string]interface{}{
				"id":                messageID,
				"channel_id":        message.ChannelID,
				"content":           message.Content,
				"is_encrypted":      message.IsEncrypted,
				"key_version":       message.KeyVersion,
				"embeds_suppressed": message.EmbedsSuppressed,
				"edited_at":         message.EditedAt,
				"updated_at":        message.UpdatedAt,
			},
		})
	}

	c.JSON(http.StatusOK, gin.H{"message": message})
}

// DeleteMessage deletes a message
func (h *Handler) DeleteMessage(c *gin.Context) {
	userID := c.GetString("user_id")
	messageID := c.Param("id")

	// Validate message ID
	if _, err := uuid.Parse(messageID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidMessageID})
		return
	}

	// Check if message exists and get author + server info + channel ID
	var authorID, channelID, serverID string
	checkQuery := `
		SELECT m.user_id, m.channel_id, c.server_id
		FROM messages m
		INNER JOIN channels c ON m.channel_id = c.id
		WHERE m.id = $1
	`

	err := h.db.QueryRow(checkQuery, messageID).Scan(&authorID, &channelID, &serverID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgMessageNotFound})
		return
	} else if err != nil {
		h.log.Error("Failed to check message permissions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteMessage})
		return
	}

	// Author can edit/delete own messages (if they have PermManageOwnMessages).
	// Others need PermManageAllMessages to delete other people's messages or suppress embeds.
	canDelete := false
	if authorID == userID {
		has, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, rbac.PermManageOwnMessages)
		if permErr != nil {
			h.log.Error("Failed to check PermManageOwnMessages", "error", permErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckPerms})
			return
		}
		canDelete = has
	}
	if !canDelete {
		has, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, rbac.PermManageAllMessages)
		if permErr != nil {
			h.log.Error("Failed to check PermManageAllMessages", "error", permErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckPerms})
			return
		}
		canDelete = has
	}

	if !canDelete {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPermsLower})
		return
	}

	// Delete message
	deleteQuery := `DELETE FROM messages WHERE id = $1`

	_, err = h.db.Exec(deleteQuery, messageID)
	if err != nil {
		h.log.Error("Failed to delete message", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteMessage})
		return
	}

	h.log.Info("Message deleted", "message_id", messageID, "deleted_by", userID, "author", authorID)

	// Broadcast deletion to channel subscribers via WebSocket
	channelUUID, err := uuid.Parse(channelID)
	if err == nil {
		h.hub.BroadcastToChannel(channelUUID, websocket.OutgoingMessage{
			Type: "message_delete",
			Data: map[string]interface{}{
				"id":         messageID,
				"channel_id": channelID,
			},
		})
	}

	c.JSON(http.StatusOK, gin.H{"message": "Message deleted successfully"})
}

// SuppressEmbeds suppresses embedded content on a message (one-way ratchet).
// Requires PermManageAllMessages. Can only set embeds_suppressed = true, never false.
// Once suppressed, only the server policy can allow embeds on NEW messages.
func (h *Handler) SuppressEmbeds(c *gin.Context) {
	userID := c.GetString("user_id")
	messageID := c.Param("id")

	// Validate message ID
	if _, err := uuid.Parse(messageID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidMessageID})
		return
	}

	// Get message info for permission check
	var channelID, serverID string
	var alreadySuppressed bool
	checkQuery := `
		SELECT m.channel_id, c.server_id, m.embeds_suppressed
		FROM messages m
		INNER JOIN channels c ON m.channel_id = c.id
		WHERE m.id = $1
	`

	err := h.db.QueryRow(checkQuery, messageID).Scan(&channelID, &serverID, &alreadySuppressed)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgMessageNotFound})
		return
	} else if err != nil {
		h.log.Error("Failed to check message for embed suppression", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSuppressEmbeds})
		return
	}

	// Already suppressed — no-op, return success
	if alreadySuppressed {
		c.JSON(http.StatusOK, gin.H{"message": "Embeds already suppressed"})
		return
	}

	// Check PermManageAllMessages
	hasPerm, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, rbac.PermManageAllMessages)
	if permErr != nil {
		h.log.Error(errMsgFailedCheckPerms, "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSuppressEmbeds})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	// One-way ratchet: suppress only (false → true). Never un-suppress.
	_, err = h.db.Exec(
		`UPDATE messages SET embeds_suppressed = TRUE, updated_at = NOW() WHERE id = $1 AND embeds_suppressed = FALSE`,
		messageID,
	)
	if err != nil {
		h.log.Error("Failed to suppress embeds", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSuppressEmbeds})
		return
	}

	h.log.Info("Embeds suppressed", "message_id", messageID, "suppressed_by", userID)

	// Broadcast update to channel subscribers so clients hide the embeds
	channelUUID, parseErr := uuid.Parse(channelID)
	if parseErr == nil {
		h.hub.BroadcastToChannel(channelUUID, websocket.OutgoingMessage{
			Type: "message_update",
			Data: map[string]interface{}{
				"id":                messageID,
				"channel_id":        channelID,
				"embeds_suppressed": true,
			},
		})
	}

	c.JSON(http.StatusOK, gin.H{"message": "Embeds suppressed successfully"})
}
