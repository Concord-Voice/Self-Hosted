// Package dm provides handlers for direct message conversations and related operations.
package dm

import (
	"database/sql"
	"encoding/base64"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// Handler handles DM-related requests.
type Handler struct {
	db  *sql.DB
	log *logger.Logger
	hub *websocket.Hub
	cfg *config.Config
}

// NewHandler creates a new DM handler.
func NewHandler(db *sql.DB, log *logger.Logger, hub *websocket.Hub, cfg *config.Config) *Handler {
	return &Handler{
		db:  db,
		log: log,
		hub: hub,
		cfg: cfg,
	}
}

// --- Conversation Endpoints ---

// conversationResponse represents a DM conversation in API responses.
type conversationResponse struct {
	ID           string                `json:"id"`
	IsGroup      bool                  `json:"is_group"`
	IsPersonal   bool                  `json:"is_personal"`
	Name         *string               `json:"name,omitempty"`
	IsEncrypted  bool                  `json:"is_encrypted"`
	Participants []participantResponse `json:"participants"`
	LastMessage  *lastMessageResponse  `json:"last_message,omitempty"`
	UnreadCount  int                   `json:"unread_count"`
	CreatedAt    string                `json:"created_at"`
}

type participantResponse struct {
	UserID      string  `json:"user_id"`
	Username    string  `json:"username"`
	DisplayName *string `json:"display_name,omitempty"`
	AvatarURL   *string `json:"avatar_url,omitempty"`
}

type lastMessageResponse struct {
	Content     string `json:"content"`
	UserID      string `json:"user_id"`
	CreatedAt   string `json:"created_at"`
	IsEncrypted bool   `json:"is_encrypted"`
}

// ListConversations returns the caller's DM conversations with last message preview.
// GET /dm/conversations
func (h *Handler) ListConversations(c *gin.Context) {
	userID := c.GetString("user_id")

	// Fetch conversations ordered by last message.
	// Uses a LEFT JOIN to dm_read_states instead of a correlated subquery.
	query := `
		SELECT dc.id, dc.is_group, dc.is_personal, dc.name, dc.is_encrypted, dc.created_at,
		       dm.content, dm.created_at, dm.user_id, dm.is_encrypted,
		       (SELECT COUNT(*) FROM dm_messages
		        WHERE conversation_id = dc.id
		          AND created_at > COALESCE(drs.last_read_at, '1970-01-01')
		          AND user_id != $1
		       ) AS unread_count
		FROM dm_conversations dc
		JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $1
		LEFT JOIN dm_read_states drs ON drs.conversation_id = dc.id AND drs.user_id = $1
		LEFT JOIN LATERAL (
		    SELECT content, created_at, user_id, is_encrypted FROM dm_messages
		    WHERE conversation_id = dc.id ORDER BY created_at DESC LIMIT 1
		) dm ON TRUE
		ORDER BY COALESCE(dm.created_at, dc.created_at) DESC
	`

	rows, err := h.db.Query(query, userID)
	if err != nil {
		h.log.Error("Failed to query DM conversations", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch conversations"})
		return
	}
	defer func() { _ = rows.Close() }()

	conversations := []conversationResponse{}
	convIDs := []string{}
	for rows.Next() {
		var conv conversationResponse
		var lmContent, lmCreatedAt, lmUserID sql.NullString
		var lmEncrypted sql.NullBool
		if err := rows.Scan(
			&conv.ID, &conv.IsGroup, &conv.IsPersonal, &conv.Name, &conv.IsEncrypted, &conv.CreatedAt,
			&lmContent, &lmCreatedAt, &lmUserID, &lmEncrypted,
			&conv.UnreadCount,
		); err != nil {
			h.log.Error("Failed to scan DM conversation", "error", err)
			continue
		}
		if lmContent.Valid {
			conv.LastMessage = &lastMessageResponse{
				Content:     lmContent.String,
				UserID:      lmUserID.String,
				CreatedAt:   lmCreatedAt.String,
				IsEncrypted: lmEncrypted.Bool,
			}
		}
		convIDs = append(convIDs, conv.ID)
		conversations = append(conversations, conv)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating DM conversations", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch conversations"})
		return
	}

	// Fetch participants for all conversations
	if len(convIDs) > 0 {
		participantMap := make(map[string][]participantResponse)
		pQuery := `
			SELECT dp.conversation_id, u.id, u.username, u.display_name, u.avatar_url
			FROM dm_participants dp
			INNER JOIN users u ON u.id = dp.user_id
			WHERE dp.conversation_id = ANY($1::uuid[])
		`
		// Build UUID array literal
		uuidArray := "{"
		for i, id := range convIDs {
			if i > 0 {
				uuidArray += ","
			}
			uuidArray += id
		}
		uuidArray += "}"

		pRows, err := h.db.Query(pQuery, uuidArray)
		if err != nil {
			h.log.Error("Failed to query DM participants", "error", err)
		} else {
			defer func() { _ = pRows.Close() }()
			for pRows.Next() {
				var convID string
				var p participantResponse
				if err := pRows.Scan(&convID, &p.UserID, &p.Username, &p.DisplayName, &p.AvatarURL); err != nil {
					continue
				}
				participantMap[convID] = append(participantMap[convID], p)
			}
		}

		for i := range conversations {
			conversations[i].Participants = participantMap[conversations[i].ID]
			if conversations[i].Participants == nil {
				conversations[i].Participants = []participantResponse{}
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{"conversations": conversations})
}

// OpenConversationRequest represents a request to open a 1:1 DM.
type OpenConversationRequest struct {
	UserID string `json:"user_id" binding:"required"`
}

// OpenConversation gets or creates a 1:1 DM conversation.
// POST /dm/conversations
func (h *Handler) OpenConversation(c *gin.Context) {
	userID := c.GetString("user_id")

	var req OpenConversationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id is required"})
		return
	}

	if _, err := uuid.Parse(req.UserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user_id"})
		return
	}

	if req.UserID == userID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot create a DM with yourself"})
		return
	}

	// Verify target user exists
	var exists bool
	if err := h.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, req.UserID).Scan(&exists); err != nil || !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}

	// Privacy enforcement: check if messaging is allowed
	var areFriends bool
	_ = h.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM friendships
			WHERE ((requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1))
			  AND status = 'accepted'
		)
	`, userID, req.UserID).Scan(&areFriends)

	if !areFriends {
		// Check target's privacy settings
		// dm_privacy_level: 0=off, 1=friends, 2=friends+server, 3=all
		var dmPrivacyLevel int
		var dmFriendsOfFriends bool
		err := h.db.QueryRow(`
			SELECT dm_privacy_level, dm_friends_of_friends
			FROM privacy_settings WHERE user_id = $1
		`, req.UserID).Scan(&dmPrivacyLevel, &dmFriendsOfFriends)
		if err == sql.ErrNoRows {
			dmPrivacyLevel = 2 // default: friends + server members
			dmFriendsOfFriends = false
		} else if err != nil {
			h.log.Error("Failed to check privacy settings", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to open conversation"})
			return
		}

		if dmPrivacyLevel == 0 {
			// DMs completely disabled
			c.JSON(http.StatusForbidden, gin.H{"error": "dm_disabled"})
			return
		}

		if dmPrivacyLevel < 3 {
			// Check if they're friends
			var isFriend bool
			_ = h.db.QueryRow(`
				SELECT EXISTS(
					SELECT 1 FROM friendships
					WHERE (user_id = $1 AND friend_id = $2) OR (user_id = $2 AND friend_id = $1)
				)
			`, userID, req.UserID).Scan(&isFriend)

			allowed := isFriend

			// Check friends-of-friends if enabled and not already allowed
			if !allowed && dmFriendsOfFriends {
				var isFoF bool
				_ = h.db.QueryRow(`
					SELECT EXISTS(
						SELECT 1 FROM friendships f1
						JOIN friendships f2 ON (f1.friend_id = f2.user_id OR f1.friend_id = f2.friend_id)
						WHERE (f1.user_id = $1 OR f1.friend_id = $1)
						  AND (f2.user_id = $2 OR f2.friend_id = $2)
						  AND f1.friend_id != $1 AND f1.friend_id != $2
					)
				`, userID, req.UserID).Scan(&isFoF)
				allowed = isFoF
			}

			// Check server members if level >= 2 and not already allowed
			if !allowed && dmPrivacyLevel >= 2 {
				var sharesServer bool
				_ = h.db.QueryRow(`
					SELECT EXISTS(
						SELECT 1 FROM server_members sm1
						JOIN server_members sm2 ON sm1.server_id = sm2.server_id
						WHERE sm1.user_id = $1 AND sm2.user_id = $2
					)
				`, userID, req.UserID).Scan(&sharesServer)
				allowed = sharesServer
			}

			if !allowed {
				c.JSON(http.StatusForbidden, gin.H{"error": "privacy_blocked"})
				return
			}
		}
	}

	// Check for existing 1:1 conversation
	var existingID string
	err := h.db.QueryRow(`
		SELECT dc.id FROM dm_conversations dc
		JOIN dm_participants p1 ON p1.conversation_id = dc.id AND p1.user_id = $1
		JOIN dm_participants p2 ON p2.conversation_id = dc.id AND p2.user_id = $2
		WHERE dc.is_group = FALSE
		LIMIT 1
	`, userID, req.UserID).Scan(&existingID)

	if err == nil {
		// Return existing conversation with participants
		conv := h.fetchConversationResponse(existingID)
		if conv != nil {
			c.JSON(http.StatusOK, gin.H{"conversation": conv})
			return
		}
	} else if err != sql.ErrNoRows {
		h.log.Error("Failed to check existing DM", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to open conversation"})
		return
	}

	// Create new 1:1 conversation
	tx, err := h.db.Begin()
	if err != nil {
		h.log.Error("Failed to start transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to open conversation"})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback transaction", "error", rbErr)
		}
	}()

	convID := uuid.New().String()
	_, err = tx.Exec(`
		INSERT INTO dm_conversations (id, is_group, is_encrypted, created_by)
		VALUES ($1, FALSE, TRUE, $2)
	`, convID, userID)
	if err != nil {
		h.log.Error("Failed to create DM conversation", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to open conversation"})
		return
	}

	// Add both participants
	_, err = tx.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, convID, userID)
	if err != nil {
		h.log.Error("Failed to add DM participant", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to open conversation"})
		return
	}
	_, err = tx.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, convID, req.UserID)
	if err != nil {
		h.log.Error("Failed to add DM participant", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to open conversation"})
		return
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit DM creation", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to open conversation"})
		return
	}

	h.log.Info("DM conversation created", "conversation_id", convID, "user_id", userID, "target", req.UserID)

	// Notify the other user
	if h.hub != nil {
		if targetUUID, parseErr := uuid.Parse(req.UserID); parseErr == nil {
			conv := h.fetchConversationResponse(convID)
			if conv != nil {
				h.hub.BroadcastToUser(targetUUID, websocket.OutgoingMessage{
					Type: "dm_conversation_created",
					Data: map[string]interface{}{
						"conversation": conv,
					},
				})
			}
		}
	}

	conv := h.fetchConversationResponse(convID)
	c.JSON(http.StatusCreated, gin.H{"conversation": conv})
}

// CreateGroupRequest represents a request to create a group DM.
type CreateGroupRequest struct {
	UserIDs []string `json:"user_ids" binding:"required"`
	Name    *string  `json:"name"`
}

// CreateGroup creates a group DM conversation.
// POST /dm/conversations/group
func (h *Handler) CreateGroup(c *gin.Context) {
	userID := c.GetString("user_id")

	var req CreateGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_ids is required"})
		return
	}

	if len(req.UserIDs) < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "At least one other user is required"})
		return
	}

	if len(req.UserIDs) > 9 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Group DMs are limited to 10 participants"})
		return
	}

	// Validate all user IDs
	allUserIDs := append([]string{userID}, req.UserIDs...)
	for _, uid := range req.UserIDs {
		if _, err := uuid.Parse(uid); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID: " + uid})
			return
		}
		if uid == userID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot include yourself in user_ids"})
			return
		}
	}

	tx, err := h.db.Begin()
	if err != nil {
		h.log.Error("Failed to start transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create group"})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback transaction", "error", rbErr)
		}
	}()

	convID := uuid.New().String()
	_, err = tx.Exec(`
		INSERT INTO dm_conversations (id, is_group, name, is_encrypted, created_by)
		VALUES ($1, TRUE, $2, TRUE, $3)
	`, convID, req.Name, userID)
	if err != nil {
		h.log.Error("Failed to create group DM", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create group"})
		return
	}

	for _, uid := range allUserIDs {
		_, err = tx.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, convID, uid)
		if err != nil {
			h.log.Error("Failed to add group participant", "error", err, "user_id", uid)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create group"})
			return
		}
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit group DM creation", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create group"})
		return
	}

	h.log.Info("Group DM created", "conversation_id", convID, "user_id", userID, "members", len(allUserIDs))

	// Notify other participants
	if h.hub != nil {
		for _, uid := range req.UserIDs {
			if targetUUID, parseErr := uuid.Parse(uid); parseErr == nil {
				conv := h.fetchConversationResponse(convID)
				if conv != nil {
					h.hub.BroadcastToUser(targetUUID, websocket.OutgoingMessage{
						Type: "dm_conversation_created",
						Data: map[string]interface{}{
							"conversation": conv,
						},
					})
				}
			}
		}
	}

	conv := h.fetchConversationResponse(convID)
	c.JSON(http.StatusCreated, gin.H{"conversation": conv})
}

// GetConversation returns a single conversation with participants.
// GET /dm/conversations/:id
func (h *Handler) GetConversation(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid conversation ID"})
		return
	}

	// Verify participation
	if !h.isParticipant(convID, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a participant"})
		return
	}

	conv := h.fetchConversationResponse(convID)
	if conv == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Conversation not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"conversation": conv})
}

// UpdateConversationRequest represents an update to a group DM.
type UpdateConversationRequest struct {
	Name *string `json:"name"`
}

// UpdateConversation updates a group DM's name.
// PATCH /dm/conversations/:id
func (h *Handler) UpdateConversation(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid conversation ID"})
		return
	}

	var req UpdateConversationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	// Verify participation and that it's a group
	var isGroup bool
	err := h.db.QueryRow(`
		SELECT dc.is_group FROM dm_conversations dc
		JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $2
		WHERE dc.id = $1
	`, convID, userID).Scan(&isGroup)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a participant"})
		return
	} else if err != nil {
		h.log.Error("Failed to check conversation", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update conversation"})
		return
	}

	if !isGroup {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot rename a 1:1 conversation"})
		return
	}

	_, err = h.db.Exec(`UPDATE dm_conversations SET name = $1, updated_at = NOW() WHERE id = $2`, req.Name, convID)
	if err != nil {
		h.log.Error("Failed to update conversation", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update conversation"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Conversation updated"})
}

// --- Message Endpoints ---

// GetMessages returns paginated message history for a DM conversation.
// GET /dm/conversations/:id/messages
func (h *Handler) GetMessages(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid conversation ID"})
		return
	}

	if !h.isParticipant(convID, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a participant"})
		return
	}

	// Cursor-based pagination
	cursor := c.Query("before")
	limit := 50

	var rows *sql.Rows
	var err error
	if cursor != "" {
		if _, parseErr := uuid.Parse(cursor); parseErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid cursor"})
			return
		}
		rows, err = h.db.Query(`
			SELECT m.id, m.conversation_id, m.user_id, m.content, m.is_encrypted, m.type, COALESCE(m.key_version, 1),
			       m.edited_at, m.created_at,
			       u.username, u.display_name, u.avatar_url
			FROM dm_messages m
			INNER JOIN users u ON u.id = m.user_id
			WHERE m.conversation_id = $1
			  AND m.created_at < (SELECT created_at FROM dm_messages WHERE id = $2)
			ORDER BY m.created_at DESC
			LIMIT $3
		`, convID, cursor, limit)
	} else {
		rows, err = h.db.Query(`
			SELECT m.id, m.conversation_id, m.user_id, m.content, m.is_encrypted, m.type, COALESCE(m.key_version, 1),
			       m.edited_at, m.created_at,
			       u.username, u.display_name, u.avatar_url
			FROM dm_messages m
			INNER JOIN users u ON u.id = m.user_id
			WHERE m.conversation_id = $1
			ORDER BY m.created_at DESC
			LIMIT $2
		`, convID, limit)
	}
	if err != nil {
		h.log.Error("Failed to query DM messages", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch messages"})
		return
	}
	defer func() { _ = rows.Close() }()

	type messageResponse struct {
		ID             string  `json:"id"`
		ConversationID string  `json:"conversation_id"`
		UserID         string  `json:"user_id"`
		Content        string  `json:"content"`
		IsEncrypted    bool    `json:"is_encrypted"`
		Type           string  `json:"type"`
		KeyVersion     int     `json:"key_version"`
		EditedAt       *string `json:"edited_at,omitempty"`
		CreatedAt      string  `json:"created_at"`
		Username       string  `json:"username"`
		DisplayName    *string `json:"display_name,omitempty"`
		AvatarURL      *string `json:"avatar_url,omitempty"`
	}

	messages := []messageResponse{}
	for rows.Next() {
		var m messageResponse
		if err := rows.Scan(
			&m.ID, &m.ConversationID, &m.UserID, &m.Content, &m.IsEncrypted, &m.Type, &m.KeyVersion,
			&m.EditedAt, &m.CreatedAt,
			&m.Username, &m.DisplayName, &m.AvatarURL,
		); err != nil {
			h.log.Error("Failed to scan DM message", "error", err)
			continue
		}
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating DM messages", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch messages"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"messages": messages})
}

// MarkRead updates the caller's last_read_at for a DM conversation.
// POST /dm/conversations/:id/read
func (h *Handler) MarkRead(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid conversation ID"})
		return
	}

	if !h.isParticipant(convID, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a participant"})
		return
	}

	_, err := h.db.Exec(`
		INSERT INTO dm_read_states (user_id, conversation_id, last_read_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (user_id, conversation_id) DO UPDATE SET last_read_at = NOW()
	`, userID, convID)
	if err != nil {
		h.log.Error("Failed to mark DM read", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to mark as read"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Marked as read"})
}

// --- E2EE Key Endpoints ---

// GetKeys returns the caller's wrapped key for a DM conversation.
// GET /dm/conversations/:id/keys
func (h *Handler) GetKeys(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid conversation ID"})
		return
	}

	if !h.isParticipant(convID, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a participant"})
		return
	}

	var isEncrypted bool
	if err := h.db.QueryRow(`SELECT is_encrypted FROM dm_conversations WHERE id = $1`, convID).Scan(&isEncrypted); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Conversation not found"})
		return
	}
	if !isEncrypted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Conversation is not encrypted"})
		return
	}

	type keyResponse struct {
		ID             string `json:"id"`
		ConversationID string `json:"conversation_id"`
		UserID         string `json:"user_id"`
		WrappedKey     string `json:"wrapped_key"`
		KeyVersion     int    `json:"key_version"`
		CreatedAt      string `json:"created_at"`
	}

	var key keyResponse
	err := h.db.QueryRow(`
		SELECT id, conversation_id, user_id, wrapped_key, key_version, created_at
		FROM dm_channel_keys
		WHERE conversation_id = $1 AND user_id = $2
		ORDER BY key_version DESC LIMIT 1
	`, convID, userID).Scan(&key.ID, &key.ConversationID, &key.UserID, &key.WrappedKey, &key.KeyVersion, &key.CreatedAt)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "No encryption key available yet", "pending": true})
		return
	} else if err != nil {
		h.log.Error("Failed to fetch DM key", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch keys"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"key": key})
}

// DistributeKeysRequest represents wrapped keys for DM participants.
type DistributeKeysRequest struct {
	WrappedKeys map[string]string `json:"wrapped_keys" binding:"required"`
	KeyVersion  *int              `json:"key_version,omitempty"`
}

// DistributeKeys stores wrapped channel keys for DM participants.
// POST /dm/conversations/:id/keys
func (h *Handler) DistributeKeys(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid conversation ID"})
		return
	}

	var req DistributeKeysRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	if !h.isParticipant(convID, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a participant"})
		return
	}

	// Determine key version
	keyVersion := 1
	if req.KeyVersion != nil {
		keyVersion = *req.KeyVersion
	} else {
		_ = h.db.QueryRow(`SELECT COALESCE(MAX(key_version), 0) + 1 FROM dm_channel_keys WHERE conversation_id = $1`, convID).Scan(&keyVersion)
	}

	distributed := 0
	for memberUserID, wrappedKey := range req.WrappedKeys {
		if _, parseErr := uuid.Parse(memberUserID); parseErr != nil {
			continue
		}

		result, err := h.db.Exec(`
			INSERT INTO dm_channel_keys (conversation_id, user_id, wrapped_key, key_version)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (conversation_id, user_id, key_version) DO NOTHING
		`, convID, memberUserID, wrappedKey, keyVersion)
		if err != nil {
			h.log.Error("Failed to store DM key", "error", err, "user_id", memberUserID)
			continue
		}
		rowsAffected, _ := result.RowsAffected()
		if rowsAffected > 0 {
			// Remove pending request if exists
			_, _ = h.db.Exec(`DELETE FROM dm_pending_key_requests WHERE conversation_id = $1 AND user_id = $2`, convID, memberUserID)

			// Notify recipient
			if h.hub != nil {
				if recipientUUID, parseErr := uuid.Parse(memberUserID); parseErr == nil {
					h.hub.BroadcastToUser(recipientUUID, websocket.OutgoingMessage{
						Type: "key_delivered",
						Data: map[string]interface{}{
							"channel_id": convID,
							"user_id":    memberUserID,
						},
					})
				}
			}
			distributed++
		}
	}

	c.JSON(http.StatusOK, gin.H{"distributed": distributed})
}

// RotateKey handles manual seal & rotate for DM forward secrecy.
// POST /dm/conversations/:id/rotate-key
func (h *Handler) RotateKey(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid conversation ID"})
		return
	}

	if !h.isParticipant(convID, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a participant"})
		return
	}

	// TODO: Rate limit via Redis (5/24h per conversation). For now, accept all.

	// Get current max key version
	var maxVersion int
	err := h.db.QueryRow(`SELECT COALESCE(MAX(key_version), 0) FROM dm_channel_keys WHERE conversation_id = $1`, convID).Scan(&maxVersion)
	if err != nil {
		h.log.Error("Failed to get max key version", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to rotate key"})
		return
	}

	h.log.Info("DM key rotation requested", "conversation_id", convID, "user_id", userID, "current_version", maxVersion)

	// Notify all participants to rotate
	if h.hub != nil {
		h.broadcastToDMParticipants(convID, userID, websocket.OutgoingMessage{
			Type: "key_rotation",
			Data: map[string]interface{}{
				"channel_id":      convID,
				"triggered_by":    userID,
				"new_key_version": maxVersion + 1,
			},
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"message":         "Key rotation initiated",
		"new_key_version": maxVersion + 1,
	})
}

// --- Voice Endpoints ---

// AuthorizeVoiceJoin checks that a user can join a DM voice call.
// POST /dm/conversations/:id/voice/join
func (h *Handler) AuthorizeVoiceJoin(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid conversation ID"})
		return
	}

	// Verify participation
	var isEncrypted bool
	err := h.db.QueryRow(`
		SELECT dc.is_encrypted
		FROM dm_conversations dc
		JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $2
		WHERE dc.id = $1
	`, convID, userID).Scan(&isEncrypted)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a participant"})
		return
	} else if err != nil {
		h.log.Error("Failed to check DM participation", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to authorize"})
		return
	}

	h.log.Info("DM voice join authorized", "user_id", userID, "conversation_id", convID)

	c.JSON(http.StatusOK, gin.H{
		"allowed":          true,
		"media_server_url": h.cfg.MediaPlaneURL,
		"ice_servers":      h.cfg.ICEServers(userID),
		"conversation": gin.H{
			"id":           convID,
			"is_encrypted": isEncrypted,
		},
	})
}

// GetVoiceParticipants returns users currently in a DM voice call.
// GET /dm/conversations/:id/voice/participants
func (h *Handler) GetVoiceParticipants(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid conversation ID"})
		return
	}

	if !h.isParticipant(convID, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a participant"})
		return
	}

	rows, err := h.db.Query(`
		SELECT vp.user_id, u.username, COALESCE(u.display_name, ''), COALESCE(u.avatar_url, ''),
		       vp.is_muted, vp.is_deafened, vp.is_video_on, vp.is_screen_sharing, vp.joined_at
		FROM dm_voice_participants vp
		INNER JOIN users u ON u.id = vp.user_id
		WHERE vp.conversation_id = $1
		ORDER BY vp.joined_at ASC
	`, convID)
	if err != nil {
		h.log.Error("Failed to query DM voice participants", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch participants"})
		return
	}
	defer func() { _ = rows.Close() }()

	type voiceParticipant struct {
		UserID          string `json:"user_id"`
		Username        string `json:"username"`
		DisplayName     string `json:"display_name,omitempty"`
		AvatarURL       string `json:"avatar_url,omitempty"`
		IsMuted         bool   `json:"is_muted"`
		IsDeafened      bool   `json:"is_deafened"`
		IsVideoOn       bool   `json:"is_video_on"`
		IsScreenSharing bool   `json:"is_screen_sharing"`
		JoinedAt        string `json:"joined_at"`
	}

	participants := []voiceParticipant{}
	for rows.Next() {
		var p voiceParticipant
		if err := rows.Scan(
			&p.UserID, &p.Username, &p.DisplayName, &p.AvatarURL,
			&p.IsMuted, &p.IsDeafened, &p.IsVideoOn, &p.IsScreenSharing, &p.JoinedAt,
		); err != nil {
			h.log.Error("Failed to scan DM voice participant", "error", err)
			continue
		}
		participants = append(participants, p)
	}

	c.JSON(http.StatusOK, gin.H{"participants": participants})
}

// --- Personal Thread ---

// GetOrCreatePersonalThread returns the user's personal thread (self-DM), creating it if needed.
// POST /dm/conversations/personal
func (h *Handler) GetOrCreatePersonalThread(c *gin.Context) {
	userID := c.GetString("user_id")

	// Check for existing personal thread
	var existingID string
	err := h.db.QueryRow(`
		SELECT dc.id FROM dm_conversations dc
		JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $1
		WHERE dc.is_personal = TRUE
	`, userID).Scan(&existingID)

	if err == nil {
		conv := h.fetchConversationResponse(existingID)
		if conv != nil {
			c.JSON(http.StatusOK, gin.H{"conversation": conv})
			return
		}
	} else if err != sql.ErrNoRows {
		h.log.Error("Failed to check personal thread", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get personal thread"})
		return
	}

	// Create personal thread
	tx, err := h.db.Begin()
	if err != nil {
		h.log.Error("Failed to start transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create personal thread"})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback transaction", "error", rbErr)
		}
	}()

	convID := uuid.New().String()
	name := "Personal Thread"
	_, err = tx.Exec(`
		INSERT INTO dm_conversations (id, is_group, is_personal, name, is_encrypted, created_by)
		VALUES ($1, FALSE, TRUE, $2, TRUE, $3)
	`, convID, name, userID)
	if err != nil {
		h.log.Error("Failed to create personal thread", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create personal thread"})
		return
	}

	_, err = tx.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, convID, userID)
	if err != nil {
		h.log.Error("Failed to add personal thread participant", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create personal thread"})
		return
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit personal thread", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create personal thread"})
		return
	}

	h.log.Info("Personal thread created", "conversation_id", convID, "user_id", userID)

	conv := h.fetchConversationResponse(convID)
	c.JSON(http.StatusCreated, gin.H{"conversation": conv})
}

// --- Helper methods ---

// isParticipant checks if a user is a participant of a DM conversation.
func (h *Handler) isParticipant(convID, userID string) bool {
	var exists bool
	err := h.db.QueryRow(`
		SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id = $1 AND user_id = $2)
	`, convID, userID).Scan(&exists)
	return err == nil && exists
}

// fetchConversationResponse builds a full conversation response with participants.
func (h *Handler) fetchConversationResponse(convID string) *conversationResponse {
	var conv conversationResponse
	var lmContent, lmCreatedAt, lmUserID sql.NullString
	var lmEncrypted sql.NullBool
	err := h.db.QueryRow(`
		SELECT dc.id, dc.is_group, dc.is_personal, dc.name, dc.is_encrypted, dc.created_at,
		       dm.content, dm.created_at, dm.user_id, dm.is_encrypted
		FROM dm_conversations dc
		LEFT JOIN LATERAL (
		    SELECT content, created_at, user_id, is_encrypted FROM dm_messages
		    WHERE conversation_id = dc.id ORDER BY created_at DESC LIMIT 1
		) dm ON TRUE
		WHERE dc.id = $1
	`, convID).Scan(
		&conv.ID, &conv.IsGroup, &conv.IsPersonal, &conv.Name, &conv.IsEncrypted, &conv.CreatedAt,
		&lmContent, &lmCreatedAt, &lmUserID, &lmEncrypted,
	)
	if err != nil {
		return nil
	}
	if lmContent.Valid {
		conv.LastMessage = &lastMessageResponse{
			Content:     lmContent.String,
			UserID:      lmUserID.String,
			CreatedAt:   lmCreatedAt.String,
			IsEncrypted: lmEncrypted.Bool,
		}
	}

	// Fetch participants
	pRows, err := h.db.Query(`
		SELECT u.id, u.username, u.display_name, u.avatar_url
		FROM dm_participants dp
		INNER JOIN users u ON u.id = dp.user_id
		WHERE dp.conversation_id = $1
	`, convID)
	if err != nil {
		return nil
	}
	defer func() { _ = pRows.Close() }()

	conv.Participants = []participantResponse{}
	for pRows.Next() {
		var p participantResponse
		if err := pRows.Scan(&p.UserID, &p.Username, &p.DisplayName, &p.AvatarURL); err != nil {
			continue
		}
		conv.Participants = append(conv.Participants, p)
	}

	return &conv
}

// UpdateMessage updates a DM message's content.
func (h *Handler) UpdateMessage(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")
	messageID := c.Param("message_id")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid conversation ID"})
		return
	}
	if _, err := uuid.Parse(messageID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid message ID"})
		return
	}

	if !h.isParticipant(convID, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a participant"})
		return
	}

	var req struct {
		Content string `json:"content" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	// Check message exists and user is the author
	var authorID string
	if err := h.db.QueryRow(`SELECT user_id FROM dm_messages WHERE id = $1 AND conversation_id = $2`, messageID, convID).Scan(&authorID); err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Message not found"})
		return
	} else if err != nil {
		h.log.Error("Failed to check DM message author", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update message"})
		return
	}

	if authorID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "You can only edit your own messages"})
		return
	}

	// E2EE enforcement
	var isEncrypted bool
	if err := h.db.QueryRow(`SELECT COALESCE(is_encrypted, false) FROM dm_conversations WHERE id = $1`, convID).Scan(&isEncrypted); err != nil {
		h.log.Error("Failed to check DM encryption", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update message"})
		return
	}
	if isEncrypted {
		decoded, err := base64.StdEncoding.DecodeString(req.Content)
		if err != nil || len(decoded) < 28 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid ciphertext format for encrypted conversation"})
			return
		}
	}

	// Update message
	type msgResult struct {
		Content     string  `json:"content"`
		IsEncrypted bool    `json:"is_encrypted"`
		KeyVersion  int     `json:"key_version"`
		EditedAt    *string `json:"edited_at"`
		CreatedAt   string  `json:"created_at"`
	}
	var result msgResult
	err := h.db.QueryRow(`
		UPDATE dm_messages
		SET content = $1, edited_at = NOW(), updated_at = NOW()
		WHERE id = $2 AND conversation_id = $3
		RETURNING is_encrypted, COALESCE(key_version, 1), edited_at, created_at
	`, req.Content, messageID, convID).Scan(&result.IsEncrypted, &result.KeyVersion, &result.EditedAt, &result.CreatedAt)
	if err != nil {
		h.log.Error("Failed to update DM message", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update message"})
		return
	}
	result.Content = req.Content

	// Broadcast to other participants
	h.broadcastToDMParticipants(convID, userID, websocket.OutgoingMessage{
		Type: "dm_message_update",
		Data: map[string]interface{}{
			"id":              messageID,
			"conversation_id": convID,
			"content":         req.Content,
			"is_encrypted":    result.IsEncrypted,
			"key_version":     result.KeyVersion,
			"edited_at":       result.EditedAt,
		},
	})

	c.JSON(http.StatusOK, gin.H{"message": result})
}

// DeleteMessage deletes a DM message.
func (h *Handler) DeleteMessage(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")
	messageID := c.Param("message_id")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid conversation ID"})
		return
	}
	if _, err := uuid.Parse(messageID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid message ID"})
		return
	}

	if !h.isParticipant(convID, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a participant"})
		return
	}

	// Check message exists and user is the author
	var authorID string
	if err := h.db.QueryRow(`SELECT user_id FROM dm_messages WHERE id = $1 AND conversation_id = $2`, messageID, convID).Scan(&authorID); err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Message not found"})
		return
	} else if err != nil {
		h.log.Error("Failed to check DM message author", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete message"})
		return
	}

	if authorID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "You can only delete your own messages"})
		return
	}

	if _, err := h.db.Exec(`DELETE FROM dm_messages WHERE id = $1`, messageID); err != nil {
		h.log.Error("Failed to delete DM message", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete message"})
		return
	}

	// Broadcast to other participants
	h.broadcastToDMParticipants(convID, userID, websocket.OutgoingMessage{
		Type: "dm_message_delete",
		Data: map[string]interface{}{
			"id":              messageID,
			"conversation_id": convID,
		},
	})

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// broadcastToDMParticipants sends a WebSocket message to all participants of a DM except the sender.
func (h *Handler) broadcastToDMParticipants(convID, excludeUserID string, msg websocket.OutgoingMessage) {
	rows, err := h.db.Query(`SELECT user_id FROM dm_participants WHERE conversation_id = $1`, convID)
	if err != nil {
		return
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil || uid == excludeUserID {
			continue
		}
		if parsed, parseErr := uuid.Parse(uid); parseErr == nil {
			h.hub.BroadcastToUser(parsed, msg)
		}
	}
}
