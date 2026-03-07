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
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
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
	db  *sql.DB
	log *logger.Logger
	hub *websocket.Hub
}

// NewHandler creates a new message handler
func NewHandler(db *sql.DB, log *logger.Logger, hub *websocket.Hub) *Handler {
	return &Handler{
		db:  db,
		log: log,
		hub: hub,
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

	// Check if user is a member of the channel's server
	var isMember bool
	memberQuery := `
		SELECT EXISTS(
			SELECT 1 FROM channels c
			INNER JOIN server_members sm ON c.server_id = sm.server_id
			WHERE c.id = $1 AND sm.user_id = $2
		)
	`

	err := h.db.QueryRow(memberQuery, channelID, userID).Scan(&isMember)
	if err != nil {
		h.log.Error("Failed to check membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch messages"})
		return
	}

	if !isMember {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this channel's server"})
		return
	}

	// Build query based on pagination
	var query string
	var args []interface{}

	if before != "" {
		// Get messages before a specific message ID
		query = `
			SELECT m.id, m.channel_id, m.user_id, m.content, m.is_encrypted, COALESCE(m.key_version, 1),
			       m.edited_at, m.created_at, m.updated_at, u.username, u.display_name, u.avatar_url
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
			       m.edited_at, m.created_at, m.updated_at, u.username, u.display_name, u.avatar_url
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch messages"})
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch messages"})
		return
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	// Check if user is a member of the channel's server
	var isMember bool
	memberQuery := `
		SELECT EXISTS(
			SELECT 1 FROM channels c
			INNER JOIN server_members sm ON c.server_id = sm.server_id
			WHERE c.id = $1 AND sm.user_id = $2
		)
	`

	err := h.db.QueryRow(memberQuery, req.ChannelID, userID).Scan(&isMember)
	if err != nil {
		h.log.Error("Failed to check membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send message"})
		return
	}

	if !isMember {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this channel's server"})
		return
	}

	// E2EE enforcement: if channel is encrypted, message MUST be encrypted with valid ciphertext
	var channelEncrypted bool
	encQuery := `SELECT is_encrypted FROM channels WHERE id = $1`
	if err := h.db.QueryRow(encQuery, req.ChannelID).Scan(&channelEncrypted); err != nil {
		h.log.Error("Failed to check channel encryption status", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send message"})
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

	// Epoch enforcement: reject messages encrypted with a revoked key epoch
	if req.IsEncrypted && keyVersion > 0 {
		var epochRevoked bool
		_ = h.db.QueryRow(
			`SELECT EXISTS(SELECT 1 FROM key_revocations WHERE channel_id = $1 AND revoked_epoch = $2)`,
			req.ChannelID, keyVersion,
		).Scan(&epochRevoked)
		if epochRevoked {
			var currentEpoch int
			_ = h.db.QueryRow(
				`SELECT COALESCE(MAX(key_version), 1) FROM channel_keys WHERE channel_id = $1`,
				req.ChannelID,
			).Scan(&currentEpoch)
			c.JSON(http.StatusConflict, gin.H{
				"error":         "Key epoch has been revoked — re-encrypt with current epoch",
				"code":          "epoch_revoked",
				"current_epoch": currentEpoch,
				"channel_id":    req.ChannelID,
			})
			return
		}
	}

	// Create message
	messageID := uuid.New().String()
	insertQuery := `
		INSERT INTO messages (id, channel_id, user_id, content, is_encrypted, key_version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW(), NOW())
		RETURNING created_at, updated_at
	`

	var message models.Message
	message.ID = messageID
	message.ChannelID = req.ChannelID
	message.UserID = userID
	message.Content = req.Content
	message.IsEncrypted = req.IsEncrypted
	message.KeyVersion = keyVersion

	err = h.db.QueryRow(insertQuery, messageID, req.ChannelID, userID, req.Content, req.IsEncrypted, keyVersion).Scan(
		&message.CreatedAt,
		&message.UpdatedAt,
	)
	if err != nil {
		h.log.Error("Failed to create message", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send message"})
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid message ID"})
		return
	}

	var req UpdateMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	// Check if message exists and user is the author
	var authorID string
	authorQuery := `SELECT user_id FROM messages WHERE id = $1`

	err := h.db.QueryRow(authorQuery, messageID).Scan(&authorID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Message not found"})
		return
	} else if err != nil {
		h.log.Error("Failed to check message author", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update message"})
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update message"})
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
		RETURNING channel_id, is_encrypted, key_version, edited_at, created_at, updated_at
	`

	var message models.Message
	message.ID = messageID
	message.UserID = userID
	message.Content = req.Content

	err = h.db.QueryRow(updateQuery, req.Content, messageID).Scan(
		&message.ChannelID,
		&message.IsEncrypted,
		&message.KeyVersion,
		&message.EditedAt,
		&message.CreatedAt,
		&message.UpdatedAt,
	)

	if err != nil {
		h.log.Error("Failed to update message", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update message"})
		return
	}

	h.log.Info("Message updated", "message_id", messageID, "user_id", userID)

	// Broadcast update to channel subscribers via WebSocket
	channelUUID, err := uuid.Parse(message.ChannelID)
	if err == nil {
		h.hub.BroadcastToChannel(channelUUID, websocket.OutgoingMessage{
			Type: "message_update",
			Data: map[string]interface{}{
				"id":           messageID,
				"channel_id":   message.ChannelID,
				"content":      message.Content,
				"is_encrypted": message.IsEncrypted,
				"key_version":  message.KeyVersion,
				"edited_at":    message.EditedAt,
				"updated_at":   message.UpdatedAt,
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid message ID"})
		return
	}

	// Check if message exists and get author + server info + channel ID
	var authorID, channelID, serverID string
	var userRole sql.NullString
	checkQuery := `
		SELECT m.user_id, m.channel_id, c.server_id, sm.role
		FROM messages m
		INNER JOIN channels c ON m.channel_id = c.id
		LEFT JOIN server_members sm ON c.server_id = sm.server_id AND sm.user_id = $2
		WHERE m.id = $1
	`

	err := h.db.QueryRow(checkQuery, messageID, userID).Scan(&authorID, &channelID, &serverID, &userRole)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Message not found"})
		return
	} else if err != nil {
		h.log.Error("Failed to check message permissions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete message"})
		return
	}

	// Check permissions: user must be the author, or owner/admin of the server
	canDelete := authorID == userID || (userRole.Valid && (userRole.String == "owner" || userRole.String == "admin"))

	if !canDelete {
		c.JSON(http.StatusForbidden, gin.H{"error": "You can only delete your own messages or be a server admin/owner"})
		return
	}

	// Delete message
	deleteQuery := `DELETE FROM messages WHERE id = $1`

	_, err = h.db.Exec(deleteQuery, messageID)
	if err != nil {
		h.log.Error("Failed to delete message", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete message"})
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
