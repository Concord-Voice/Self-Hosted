package messages

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/models"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
)

const (
	errMsgPinFailed       = "Failed to pin message"
	errMsgUnpinFailed     = "Failed to unpin message"
	errMsgFetchPinsFailed = "Failed to fetch pinned messages"
	errMsgPinLimitReached = "Maximum of 50 pinned messages per channel"
	maxPinsPerChannel     = 50
)

// PinMessage pins a message in its channel. Requires PermPinMessages.
func (h *Handler) PinMessage(c *gin.Context) {
	userID := c.GetString("user_id")
	messageID := c.Param("id")

	if _, err := uuid.Parse(messageID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidMessageID})
		return
	}

	channelID, serverID, ok := h.lookupMessageContext(c, messageID, userID)
	if !ok {
		return
	}

	hasPerm, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, rbac.PermPinMessages)
	if permErr != nil {
		h.log.Error(errMsgFailedCheckPerms, "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgPinFailed})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	// Pin with 50-message limit check via UPDATE subquery.
	// NOTE: Under high concurrency, two simultaneous requests could both see
	// COUNT(*) < 50 and succeed. For production at scale, consider wrapping in
	// a transaction with an advisory lock keyed by channel_id.
	var pinnedAt time.Time
	var pinnedBy string
	err := h.db.QueryRow(`
		UPDATE messages
		SET pinned_at = NOW(), pinned_by = $1, updated_at = NOW()
		WHERE id = $2 AND pinned_at IS NULL
		  AND (SELECT COUNT(*) FROM messages WHERE channel_id = $3 AND pinned_at IS NOT NULL) < $4
		RETURNING pinned_at, pinned_by
	`, userID, messageID, channelID, maxPinsPerChannel).Scan(&pinnedAt, &pinnedBy)

	if err == sql.ErrNoRows {
		// Disambiguate: already pinned vs limit reached
		var existingPinnedAt *time.Time
		var existingPinnedBy *string
		checkErr := h.db.QueryRow(`SELECT pinned_at, pinned_by FROM messages WHERE id = $1`, messageID).Scan(&existingPinnedAt, &existingPinnedBy)
		if checkErr != nil {
			h.log.Error(errMsgPinFailed, "error", checkErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgPinFailed})
			return
		}
		if existingPinnedAt != nil {
			c.JSON(http.StatusOK, gin.H{"message_id": messageID, "pinned_at": existingPinnedAt, "pinned_by": existingPinnedBy, "already_pinned": true})
			return
		}
		c.JSON(http.StatusConflict, gin.H{"error": errMsgPinLimitReached})
		return
	}
	if err != nil {
		h.log.Error(errMsgPinFailed, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgPinFailed})
		return
	}

	// Broadcast to channel
	broadcastPin(h.hub, channelID, messageID, pinnedAt, pinnedBy)

	c.JSON(http.StatusOK, gin.H{"message_id": messageID, "pinned_at": pinnedAt, "pinned_by": pinnedBy})
}

// UnpinMessage unpins a message. Requires PermPinMessages.
func (h *Handler) UnpinMessage(c *gin.Context) {
	userID := c.GetString("user_id")
	messageID := c.Param("id")

	if _, err := uuid.Parse(messageID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidMessageID})
		return
	}

	channelID, serverID, ok := h.lookupMessageContext(c, messageID, userID)
	if !ok {
		return
	}

	hasPerm, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, rbac.PermPinMessages)
	if permErr != nil {
		h.log.Error(errMsgFailedCheckPerms, "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgUnpinFailed})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	result, err := h.db.Exec(`
		UPDATE messages SET pinned_at = NULL, pinned_by = NULL, updated_at = NOW()
		WHERE id = $1 AND pinned_at IS NOT NULL
	`, messageID)
	if err != nil {
		h.log.Error(errMsgUnpinFailed, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgUnpinFailed})
		return
	}

	rowsAffected, raErr := result.RowsAffected()
	if raErr != nil {
		h.log.Error("Failed to get affected rows for unpin", "error", raErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgUnpinFailed})
		return
	}
	if rowsAffected == 0 {
		// Already unpinned — idempotent success
		c.JSON(http.StatusOK, gin.H{"message_id": messageID, "already_unpinned": true})
		return
	}

	// Broadcast to channel
	channelUUID, parseErr := uuid.Parse(channelID)
	if parseErr == nil {
		h.hub.BroadcastToChannel(channelUUID, websocket.OutgoingMessage{
			Type: "message_unpinned",
			Data: map[string]interface{}{
				"message_id": messageID,
				"channel_id": channelID,
			},
		})
	}

	c.JSON(http.StatusOK, gin.H{"message_id": messageID})
}

// GetChannelPins returns all pinned messages for a channel, ordered by pinned_at DESC.
func (h *Handler) GetChannelPins(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, ok := h.checkChannelAccess(c, channelID, userID); !ok {
		return
	}

	rows, err := h.db.Query(`
		SELECT m.id, m.channel_id, m.user_id, m.content, m.is_encrypted, COALESCE(m.key_version, 1),
		       m.embeds_suppressed, m.reply_to_id, m.pinned_at, m.pinned_by, m.edited_at, m.created_at, m.updated_at,
		       u.username, u.display_name, u.avatar_url
		FROM messages m
		INNER JOIN users u ON m.user_id = u.id
		WHERE m.channel_id = $1 AND m.pinned_at IS NOT NULL
		ORDER BY m.pinned_at DESC
	`, channelID)
	if err != nil {
		h.log.Error(errMsgFetchPinsFailed, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFetchPinsFailed})
		return
	}
	defer func() { _ = rows.Close() }()

	messages := []models.MessageWithUser{}
	for rows.Next() {
		var msg models.MessageWithUser
		scanErr := rows.Scan(
			&msg.ID, &msg.ChannelID, &msg.UserID, &msg.Content, &msg.IsEncrypted, &msg.KeyVersion,
			&msg.EmbedsSuppressed, &msg.ReplyToID, &msg.PinnedAt, &msg.PinnedBy, &msg.EditedAt,
			&msg.CreatedAt, &msg.UpdatedAt, &msg.Username, &msg.DisplayName, &msg.AvatarURL,
		)
		if scanErr != nil {
			h.log.Error("Failed to scan pinned message row", "error", scanErr)
			continue
		}
		messages = append(messages, msg)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating pinned messages", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFetchPinsFailed})
		return
	}

	h.enrichMessages(messages, userID)

	c.JSON(http.StatusOK, gin.H{"pinned_messages": messages, "count": len(messages)})
}

// broadcastPin sends a message_pinned event to all channel subscribers.
func broadcastPin(hub *websocket.Hub, channelID, messageID string, pinnedAt time.Time, pinnedBy string) {
	channelUUID, err := uuid.Parse(channelID)
	if err != nil {
		return
	}
	hub.BroadcastToChannel(channelUUID, websocket.OutgoingMessage{
		Type: "message_pinned",
		Data: map[string]interface{}{
			"message_id": messageID,
			"channel_id": channelID,
			"pinned_at":  pinnedAt,
			"pinned_by":  pinnedBy,
		},
	})
}
