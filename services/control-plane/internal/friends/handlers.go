// Package friends provides handlers for friend relationship management.
package friends

import (
	"database/sql"
	"net/http"
	"strings"
	"time"

	"github.com/concord/control-plane/internal/invites"
	"github.com/concord/control-plane/internal/websocket"
	"github.com/concord/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Handler handles friend-related requests.
type Handler struct {
	db  *sql.DB
	log *logger.Logger
	hub *websocket.Hub
}

// NewHandler creates a new friends handler.
func NewHandler(db *sql.DB, log *logger.Logger, hub *websocket.Hub) *Handler {
	return &Handler{
		db:  db,
		log: log,
		hub: hub,
	}
}

// friendResponse represents a friend in API responses.
type friendResponse struct {
	ID          string  `json:"id"`
	UserID      string  `json:"user_id"`
	Username    string  `json:"username"`
	DisplayName *string `json:"display_name,omitempty"`
	AvatarURL   *string `json:"avatar_url,omitempty"`
	CreatedAt   string  `json:"created_at"`
}

// friendRequestResponse represents a friend request in API responses.
type friendRequestResponse struct {
	ID              string  `json:"id"`
	FromUserID      string  `json:"from_user_id"`
	FromUsername    string  `json:"from_username"`
	FromDisplayName *string `json:"from_display_name,omitempty"`
	FromAvatarURL  *string `json:"from_avatar_url,omitempty"`
	ToUserID        string  `json:"to_user_id"`
	ToUsername      string  `json:"to_username"`
	ToDisplayName  *string `json:"to_display_name,omitempty"`
	ToAvatarURL    *string `json:"to_avatar_url,omitempty"`
	Direction       string  `json:"direction"`
	CreatedAt       string  `json:"created_at"`
}

// ListFriends returns the caller's accepted friendships with user details.
// GET /friends
func (h *Handler) ListFriends(c *gin.Context) {
	userID := c.GetString("user_id")

	query := `
		SELECT f.id, u.id, u.username, u.display_name, u.avatar_url, f.created_at
		FROM friendships f
		INNER JOIN users u ON u.id = CASE
			WHEN f.requester_id = $1 THEN f.addressee_id
			ELSE f.requester_id
		END
		WHERE (f.requester_id = $1 OR f.addressee_id = $1)
		  AND f.status = 'accepted'
		ORDER BY u.username ASC
	`

	rows, err := h.db.Query(query, userID)
	if err != nil {
		h.log.Error("Failed to query friends", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch friends"})
		return
	}
	defer func() { _ = rows.Close() }()

	friends := []friendResponse{}
	for rows.Next() {
		var f friendResponse
		if err := rows.Scan(&f.ID, &f.UserID, &f.Username, &f.DisplayName, &f.AvatarURL, &f.CreatedAt); err != nil {
			h.log.Error("Failed to scan friend", "error", err)
			continue
		}
		friends = append(friends, f)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating friends", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch friends"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"friends": friends})
}

// ListRequests returns pending friend requests (sent and received).
// GET /friends/requests
func (h *Handler) ListRequests(c *gin.Context) {
	userID := c.GetString("user_id")

	query := `
		SELECT f.id,
		       req.id, req.username, req.display_name, req.avatar_url,
		       addr.id, addr.username, addr.display_name, addr.avatar_url,
		       CASE WHEN f.requester_id = $1 THEN 'sent' ELSE 'received' END AS direction,
		       f.created_at
		FROM friendships f
		INNER JOIN users req ON req.id = f.requester_id
		INNER JOIN users addr ON addr.id = f.addressee_id
		WHERE (f.requester_id = $1 OR f.addressee_id = $1)
		  AND f.status = 'pending'
		ORDER BY f.created_at DESC
	`

	rows, err := h.db.Query(query, userID)
	if err != nil {
		h.log.Error("Failed to query friend requests", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch friend requests"})
		return
	}
	defer func() { _ = rows.Close() }()

	requests := []friendRequestResponse{}
	for rows.Next() {
		var r friendRequestResponse
		if err := rows.Scan(
			&r.ID,
			&r.FromUserID, &r.FromUsername, &r.FromDisplayName, &r.FromAvatarURL,
			&r.ToUserID, &r.ToUsername, &r.ToDisplayName, &r.ToAvatarURL,
			&r.Direction, &r.CreatedAt,
		); err != nil {
			h.log.Error("Failed to scan friend request", "error", err)
			continue
		}
		requests = append(requests, r)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating friend requests", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch friend requests"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"requests": requests})
}

// SendRequestBody represents the request to send a friend request.
type SendRequestBody struct {
	UserID   *string `json:"user_id"`
	Username *string `json:"username"`
}

// SendRequest sends a friend request to another user.
// POST /friends/request
func (h *Handler) SendRequest(c *gin.Context) {
	userID := c.GetString("user_id")

	var req SendRequestBody
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	// Resolve target user ID
	var targetUserID string
	if req.UserID != nil {
		if _, err := uuid.Parse(*req.UserID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user_id"})
			return
		}
		targetUserID = *req.UserID
	} else if req.Username != nil {
		err := h.db.QueryRow(`SELECT id FROM users WHERE username = $1`, strings.ToLower(strings.TrimSpace(*req.Username))).Scan(&targetUserID)
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
			return
		} else if err != nil {
			h.log.Error("Failed to look up user by username", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send friend request"})
			return
		}
	} else {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id or username is required"})
		return
	}

	// Can't friend yourself
	if targetUserID == userID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot send a friend request to yourself"})
		return
	}

	// Verify target user exists
	var exists bool
	if err := h.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, targetUserID).Scan(&exists); err != nil || !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}

	// Check for existing friendship in either direction
	var existingStatus string
	err := h.db.QueryRow(`
		SELECT status FROM friendships
		WHERE (requester_id = $1 AND addressee_id = $2)
		   OR (requester_id = $2 AND addressee_id = $1)
	`, userID, targetUserID).Scan(&existingStatus)
	if err == nil {
		switch existingStatus {
		case "accepted":
			c.JSON(http.StatusConflict, gin.H{"error": "Already friends"})
			return
		case "pending":
			c.JSON(http.StatusConflict, gin.H{"error": "Friend request already pending"})
			return
		case "blocked":
			c.JSON(http.StatusForbidden, gin.H{"error": "Cannot send friend request"})
			return
		}
	} else if err != sql.ErrNoRows {
		h.log.Error("Failed to check existing friendship", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send friend request"})
		return
	}

	// Create friendship
	var friendshipID string
	var createdAt string
	err = h.db.QueryRow(`
		INSERT INTO friendships (requester_id, addressee_id, status)
		VALUES ($1, $2, 'pending')
		RETURNING id, created_at
	`, userID, targetUserID).Scan(&friendshipID, &createdAt)
	if err != nil {
		h.log.Error("Failed to create friend request", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send friend request"})
		return
	}

	h.log.Info("Friend request sent", "from", userID, "to", targetUserID)

	// Notify the addressee via WebSocket
	if h.hub != nil {
		if addresseeUUID, parseErr := uuid.Parse(targetUserID); parseErr == nil {
			// Fetch sender info for the notification
			var senderUsername string
			var senderDisplayName *string
			var senderAvatarURL *string
			_ = h.db.QueryRow(`SELECT username, display_name, avatar_url FROM users WHERE id = $1`, userID).
				Scan(&senderUsername, &senderDisplayName, &senderAvatarURL)

			h.hub.BroadcastToUser(addresseeUUID, websocket.OutgoingMessage{
				Type: "friend_request_received",
				Data: map[string]interface{}{
					"id":                friendshipID,
					"from_user_id":      userID,
					"from_username":     senderUsername,
					"from_display_name": senderDisplayName,
					"from_avatar_url":   senderAvatarURL,
					"created_at":        createdAt,
				},
			})
		}
	}

	c.JSON(http.StatusCreated, gin.H{
		"id":         friendshipID,
		"status":     "pending",
		"created_at": createdAt,
	})
}

// RespondRequestBody represents a response to a friend request.
type RespondRequestBody struct {
	Action string `json:"action" binding:"required"` // accept, decline
}

// RespondRequest accepts or declines a friend request.
// PATCH /friends/request/:id
func (h *Handler) RespondRequest(c *gin.Context) {
	userID := c.GetString("user_id")
	requestID := c.Param("id")

	if _, err := uuid.Parse(requestID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request ID"})
		return
	}

	var req RespondRequestBody
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	if req.Action != "accept" && req.Action != "decline" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Action must be 'accept' or 'decline'"})
		return
	}

	// Verify this is a pending request addressed to the caller
	var requesterID string
	err := h.db.QueryRow(`
		SELECT requester_id FROM friendships
		WHERE id = $1 AND addressee_id = $2 AND status = 'pending'
	`, requestID, userID).Scan(&requesterID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Friend request not found"})
		return
	} else if err != nil {
		h.log.Error("Failed to fetch friend request", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to respond to friend request"})
		return
	}

	if req.Action == "accept" {
		_, err = h.db.Exec(`
			UPDATE friendships SET status = 'accepted', updated_at = NOW()
			WHERE id = $1
		`, requestID)
		if err != nil {
			h.log.Error("Failed to accept friend request", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to accept friend request"})
			return
		}

		h.log.Info("Friend request accepted", "request_id", requestID, "user_id", userID)

		// Notify the requester
		if h.hub != nil {
			if requesterUUID, parseErr := uuid.Parse(requesterID); parseErr == nil {
				var acceptorUsername string
				var acceptorDisplayName *string
				var acceptorAvatarURL *string
				_ = h.db.QueryRow(`SELECT username, display_name, avatar_url FROM users WHERE id = $1`, userID).
					Scan(&acceptorUsername, &acceptorDisplayName, &acceptorAvatarURL)

				h.hub.BroadcastToUser(requesterUUID, websocket.OutgoingMessage{
					Type: "friend_request_accepted",
					Data: map[string]interface{}{
						"id":           requestID,
						"user_id":      userID,
						"username":     acceptorUsername,
						"display_name": acceptorDisplayName,
						"avatar_url":   acceptorAvatarURL,
					},
				})
			}
		}

		c.JSON(http.StatusOK, gin.H{"message": "Friend request accepted"})
	} else {
		// Decline — delete the request
		_, err = h.db.Exec(`DELETE FROM friendships WHERE id = $1`, requestID)
		if err != nil {
			h.log.Error("Failed to decline friend request", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to decline friend request"})
			return
		}

		h.log.Info("Friend request declined", "request_id", requestID, "user_id", userID)

		c.JSON(http.StatusOK, gin.H{"message": "Friend request declined"})
	}
}

// RemoveFriend removes a friendship.
// DELETE /friends/:user_id
func (h *Handler) RemoveFriend(c *gin.Context) {
	userID := c.GetString("user_id")
	targetUserID := c.Param("user_id")

	if _, err := uuid.Parse(targetUserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}

	result, err := h.db.Exec(`
		DELETE FROM friendships
		WHERE ((requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1))
		  AND status = 'accepted'
	`, userID, targetUserID)
	if err != nil {
		h.log.Error("Failed to remove friend", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to remove friend"})
		return
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Friendship not found"})
		return
	}

	h.log.Info("Friend removed", "user_id", userID, "target", targetUserID)

	// Notify the other user
	if h.hub != nil {
		if targetUUID, parseErr := uuid.Parse(targetUserID); parseErr == nil {
			h.hub.BroadcastToUser(targetUUID, websocket.OutgoingMessage{
				Type: "friend_removed",
				Data: map[string]interface{}{
					"user_id": userID,
				},
			})
		}
	}

	c.JSON(http.StatusOK, gin.H{"message": "Friend removed"})
}

// BlockUser blocks another user. If a friendship exists, it is updated to 'blocked'.
// If a DM conversation exists, the blocked user's current epoch key is revoked.
// POST /friends/:user_id/block
func (h *Handler) BlockUser(c *gin.Context) {
	userID := c.GetString("user_id")
	targetUserID := c.Param("user_id")

	if _, err := uuid.Parse(targetUserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}

	if targetUserID == userID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot block yourself"})
		return
	}

	tx, err := h.db.Begin()
	if err != nil {
		h.log.Error("Failed to start transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to block user"})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback transaction", "error", rbErr)
		}
	}()

	// Upsert friendship as blocked
	_, err = tx.Exec(`
		INSERT INTO friendships (requester_id, addressee_id, status)
		VALUES ($1, $2, 'blocked')
		ON CONFLICT (requester_id, addressee_id) DO UPDATE SET status = 'blocked', updated_at = NOW()
	`, userID, targetUserID)
	if err != nil {
		// May fail if the reverse direction exists — try that direction
		_, err = tx.Exec(`
			UPDATE friendships SET status = 'blocked', updated_at = NOW()
			WHERE (requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1)
		`, userID, targetUserID)
		if err != nil {
			h.log.Error("Failed to block user", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to block user"})
			return
		}
	}

	// Revoke the blocked user's current epoch key in any shared DM conversation
	// (Preserve History: only delete latest key_version, old epoch keys remain)
	_, _ = tx.Exec(`
		DELETE FROM dm_channel_keys
		WHERE user_id = $2
		  AND conversation_id IN (
			SELECT dp1.conversation_id FROM dm_participants dp1
			INNER JOIN dm_participants dp2 ON dp1.conversation_id = dp2.conversation_id
			WHERE dp1.user_id = $1 AND dp2.user_id = $2
		  )
		  AND key_version = (
			SELECT MAX(key_version) FROM dm_channel_keys dck2
			WHERE dck2.conversation_id = dm_channel_keys.conversation_id
		  )
	`, userID, targetUserID)

	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit block", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to block user"})
		return
	}

	h.log.Info("User blocked", "user_id", userID, "blocked", targetUserID)

	// Notify the blocked user
	if h.hub != nil {
		if targetUUID, parseErr := uuid.Parse(targetUserID); parseErr == nil {
			h.hub.BroadcastToUser(targetUUID, websocket.OutgoingMessage{
				Type: "friend_removed",
				Data: map[string]interface{}{
					"user_id": userID,
				},
			})
		}

		// Notify the blocker to rotate the CSK
		if blockerUUID, parseErr := uuid.Parse(userID); parseErr == nil {
			h.hub.BroadcastToUser(blockerUUID, websocket.OutgoingMessage{
				Type: "key_revocation",
				Data: map[string]interface{}{
					"blocked_user_id": targetUserID,
				},
			})
		}
	}

	c.JSON(http.StatusOK, gin.H{"message": "User blocked"})
}

// --- Friend Code types ---

// friendCodeResponse represents a friend code in API responses.
type friendCodeResponse struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	Code       string     `json:"code"`
	MaxUses    *int       `json:"max_uses"`
	UseCount   int        `json:"use_count"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	IsRevoked  bool       `json:"is_revoked"`
	AutoAccept bool       `json:"auto_accept"`
	CreatedAt  string     `json:"created_at"`
}

type createFriendCodeRequest struct {
	MaxUses    *int  `json:"max_uses"`
	ExpiresIn  *int  `json:"expires_in"` // seconds; nil → default 3600 (1h)
	AutoAccept *bool `json:"auto_accept"`
}

// CreateFriendCode generates a new friend code for the caller.
// POST /friends/codes
func (h *Handler) CreateFriendCode(c *gin.Context) {
	userID := c.GetString("user_id")

	var req createFriendCodeRequest
	_ = c.ShouldBindJSON(&req)

	// Apply defaults — max 10 uses, max 1 day
	var maxUsesPtr *int
	if req.MaxUses != nil && *req.MaxUses > 0 {
		maxUses := *req.MaxUses
		if maxUses > 10 {
			maxUses = 10
		}
		maxUsesPtr = &maxUses
	} else if req.MaxUses == nil {
		defaultMax := 1
		maxUsesPtr = &defaultMax
	}

	expiresInSec := 3600 // default 1 hour
	if req.ExpiresIn != nil {
		expiresInSec = *req.ExpiresIn
		if expiresInSec < 300 {
			expiresInSec = 300
		}
		if expiresInSec > 86400 {
			expiresInSec = 86400
		}
	}
	expiresAt := time.Now().UTC().Add(time.Duration(expiresInSec) * time.Second)

	autoAccept := false
	if req.AutoAccept != nil {
		autoAccept = *req.AutoAccept
	}

	// Generate unique code with retry (reuse server invite code generator)
	for attempts := 0; attempts < 5; attempts++ {
		code, err := invites.GenerateCode()
		if err != nil {
			h.log.Error("Failed to generate friend code", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create friend code"})
			return
		}

		var fc friendCodeResponse
		insertErr := h.db.QueryRow(`
			INSERT INTO friend_codes (user_id, code, max_uses, expires_at, auto_accept)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING id, user_id, code, max_uses, use_count, expires_at, is_revoked, auto_accept, created_at
		`, userID, code, maxUsesPtr, expiresAt, autoAccept).Scan(
			&fc.ID, &fc.UserID, &fc.Code, &fc.MaxUses, &fc.UseCount,
			&fc.ExpiresAt, &fc.IsRevoked, &fc.AutoAccept, &fc.CreatedAt,
		)
		if insertErr != nil {
			continue // unique constraint collision — retry
		}

		h.log.Info("Friend code created", "user_id", userID, "code", code)
		c.JSON(http.StatusCreated, gin.H{"friend_code": fc})
		return
	}

	h.log.Error("Failed to create unique friend code after retries")
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create friend code"})
}

// ListFriendCodes returns the caller's non-revoked friend codes.
// GET /friends/codes
func (h *Handler) ListFriendCodes(c *gin.Context) {
	userID := c.GetString("user_id")

	rows, err := h.db.Query(`
		SELECT id, user_id, code, max_uses, use_count, expires_at, is_revoked, auto_accept, created_at
		FROM friend_codes
		WHERE user_id = $1 AND is_revoked = FALSE
		ORDER BY created_at DESC
	`, userID)
	if err != nil {
		h.log.Error("Failed to query friend codes", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch friend codes"})
		return
	}
	defer func() { _ = rows.Close() }()

	codes := []friendCodeResponse{}
	for rows.Next() {
		var fc friendCodeResponse
		if err := rows.Scan(
			&fc.ID, &fc.UserID, &fc.Code, &fc.MaxUses, &fc.UseCount,
			&fc.ExpiresAt, &fc.IsRevoked, &fc.AutoAccept, &fc.CreatedAt,
		); err != nil {
			h.log.Error("Failed to scan friend code", "error", err)
			continue
		}
		codes = append(codes, fc)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating friend codes", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch friend codes"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"friend_codes": codes})
}

// RevokeFriendCode soft-revokes a friend code owned by the caller.
// DELETE /friends/codes/:id
func (h *Handler) RevokeFriendCode(c *gin.Context) {
	userID := c.GetString("user_id")
	codeID := c.Param("id")

	if _, err := uuid.Parse(codeID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid code ID"})
		return
	}

	result, err := h.db.Exec(`
		UPDATE friend_codes SET is_revoked = TRUE
		WHERE id = $1 AND user_id = $2 AND is_revoked = FALSE
	`, codeID, userID)
	if err != nil {
		h.log.Error("Failed to revoke friend code", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to revoke friend code"})
		return
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Friend code not found or already revoked"})
		return
	}

	h.log.Info("Friend code revoked", "code_id", codeID, "user_id", userID)
	c.JSON(http.StatusOK, gin.H{"message": "Friend code revoked"})
}

// PreviewFriendCode validates a friend code and returns the owner's profile.
// Does NOT consume a use.
// GET /friends/codes/:code
func (h *Handler) PreviewFriendCode(c *gin.Context) {
	code := c.Param("code")

	if len(code) != 8 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid friend code format"})
		return
	}

	var (
		ownerID     string
		username    string
		displayName *string
		avatarURL   *string
		expiresAt   *time.Time
		isRevoked   bool
		maxUses     *int
		useCount    int
	)

	err := h.db.QueryRow(`
		SELECT fc.user_id, u.username, u.display_name, u.avatar_url,
		       fc.expires_at, fc.is_revoked, fc.max_uses, fc.use_count
		FROM friend_codes fc
		INNER JOIN users u ON fc.user_id = u.id
		WHERE fc.code = $1
	`, code).Scan(
		&ownerID, &username, &displayName, &avatarURL,
		&expiresAt, &isRevoked, &maxUses, &useCount,
	)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Invalid friend code"})
		return
	}
	if err != nil {
		h.log.Error("Failed to fetch friend code", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch friend code"})
		return
	}

	valid := !isRevoked &&
		(expiresAt == nil || expiresAt.After(time.Now().UTC())) &&
		(maxUses == nil || *maxUses == 0 || useCount < *maxUses)

	c.JSON(http.StatusOK, gin.H{
		"user_id":      ownerID,
		"username":     username,
		"display_name": displayName,
		"avatar_url":   avatarURL,
		"valid":        valid,
	})
}

// ClaimFriendCode redeems a friend code to create a friendship.
// If auto_accept is set on the code, the friendship is created as 'accepted' directly.
// Otherwise, a pending friend request is created from the claimer to the code owner.
// POST /friends/codes/:code/claim
func (h *Handler) ClaimFriendCode(c *gin.Context) {
	userID := c.GetString("user_id")
	code := c.Param("code")

	if len(code) != 8 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid friend code format"})
		return
	}

	tx, err := h.db.Begin()
	if err != nil {
		h.log.Error("Failed to begin transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to claim friend code"})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback", "error", rbErr)
		}
	}()

	// Lock the code row to prevent race conditions on use_count
	var (
		codeID     string
		ownerID    string
		maxUses    *int
		useCount   int
		expiresAt  *time.Time
		isRevoked  bool
		autoAccept bool
	)
	err = tx.QueryRow(`
		SELECT id, user_id, max_uses, use_count, expires_at, is_revoked, auto_accept
		FROM friend_codes
		WHERE code = $1
		FOR UPDATE
	`, code).Scan(&codeID, &ownerID, &maxUses, &useCount, &expiresAt, &isRevoked, &autoAccept)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Invalid friend code"})
		return
	}
	if err != nil {
		h.log.Error("Failed to query friend code", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to claim friend code"})
		return
	}

	// Validate code
	if isRevoked {
		c.JSON(http.StatusGone, gin.H{"error": "This friend code has been revoked"})
		return
	}
	if expiresAt != nil && expiresAt.Before(time.Now().UTC()) {
		c.JSON(http.StatusGone, gin.H{"error": "This friend code has expired"})
		return
	}
	if maxUses != nil && *maxUses > 0 && useCount >= *maxUses {
		c.JSON(http.StatusGone, gin.H{"error": "This friend code has reached its maximum uses"})
		return
	}

	// Can't claim your own code
	if ownerID == userID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot claim your own friend code"})
		return
	}

	// Check for existing friendship in either direction
	var existingStatus string
	err = tx.QueryRow(`
		SELECT status FROM friendships
		WHERE (requester_id = $1 AND addressee_id = $2)
		   OR (requester_id = $2 AND addressee_id = $1)
	`, userID, ownerID).Scan(&existingStatus)
	if err == nil {
		switch existingStatus {
		case "accepted":
			c.JSON(http.StatusConflict, gin.H{"error": "Already friends with this user"})
			return
		case "pending":
			c.JSON(http.StatusConflict, gin.H{"error": "Friend request already pending with this user"})
			return
		case "blocked":
			c.JSON(http.StatusForbidden, gin.H{"error": "Cannot add this user as a friend"})
			return
		}
	} else if err != sql.ErrNoRows {
		h.log.Error("Failed to check existing friendship", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to claim friend code"})
		return
	}

	// Create friendship
	status := "pending"
	if autoAccept {
		status = "accepted"
	}

	var friendshipID, createdAt string
	err = tx.QueryRow(`
		INSERT INTO friendships (requester_id, addressee_id, status)
		VALUES ($1, $2, $3)
		RETURNING id, created_at
	`, userID, ownerID, status).Scan(&friendshipID, &createdAt)
	if err != nil {
		h.log.Error("Failed to create friendship", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to claim friend code"})
		return
	}

	// Increment use count
	_, err = tx.Exec(`UPDATE friend_codes SET use_count = use_count + 1 WHERE id = $1`, codeID)
	if err != nil {
		h.log.Error("Failed to increment use count", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to claim friend code"})
		return
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to claim friend code"})
		return
	}

	h.log.Info("Friend code claimed", "code", code, "claimer", userID, "owner", ownerID, "status", status)

	// Fetch user info for WebSocket notifications and response
	var claimerUsername string
	var claimerDisplayName *string
	var claimerAvatarURL *string
	_ = h.db.QueryRow(`SELECT username, display_name, avatar_url FROM users WHERE id = $1`, userID).
		Scan(&claimerUsername, &claimerDisplayName, &claimerAvatarURL)

	var ownerUsername string
	var ownerDisplayName *string
	var ownerAvatarURL *string
	_ = h.db.QueryRow(`SELECT username, display_name, avatar_url FROM users WHERE id = $1`, ownerID).
		Scan(&ownerUsername, &ownerDisplayName, &ownerAvatarURL)

	// WebSocket notifications
	if h.hub != nil {
		if ownerUUID, parseErr := uuid.Parse(ownerID); parseErr == nil {
			// Always notify code owner that their code was claimed
			h.hub.BroadcastToUser(ownerUUID, websocket.OutgoingMessage{
				Type: "friend_code_claimed",
				Data: map[string]interface{}{
					"friendship_id": friendshipID,
					"code_id":       codeID,
					"status":        status,
					"user_id":       userID,
					"username":      claimerUsername,
					"display_name":  claimerDisplayName,
					"avatar_url":    claimerAvatarURL,
					"created_at":    createdAt,
				},
			})

			if autoAccept {
				// Notify claimer that they're now friends
				if claimerUUID, parseErr2 := uuid.Parse(userID); parseErr2 == nil {
					h.hub.BroadcastToUser(claimerUUID, websocket.OutgoingMessage{
						Type: "friend_request_accepted",
						Data: map[string]interface{}{
							"id":           friendshipID,
							"user_id":      ownerID,
							"username":     ownerUsername,
							"display_name": ownerDisplayName,
							"avatar_url":   ownerAvatarURL,
						},
					})
				}
			} else {
				// Pending — notify code owner of incoming request
				h.hub.BroadcastToUser(ownerUUID, websocket.OutgoingMessage{
					Type: "friend_request_received",
					Data: map[string]interface{}{
						"id":                friendshipID,
						"from_user_id":      userID,
						"from_username":     claimerUsername,
						"from_display_name": claimerDisplayName,
						"from_avatar_url":   claimerAvatarURL,
						"created_at":        createdAt,
					},
				})
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"status":        status,
		"friendship_id": friendshipID,
		"user": map[string]interface{}{
			"user_id":      ownerID,
			"username":     ownerUsername,
			"display_name": ownerDisplayName,
			"avatar_url":   ownerAvatarURL,
		},
	})
}
