// Package members provides handlers for managing server membership.
package members

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"

	"github.com/concord/control-plane/internal/models"
	"github.com/concord/control-plane/internal/websocket"
	"github.com/concord/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Handler handles member-related requests
type Handler struct {
	db    *sql.DB
	log   *logger.Logger
	redis *redis.Client
	hub   *websocket.Hub
}

// NewHandler creates a new member handler
func NewHandler(db *sql.DB, log *logger.Logger, redisClient *redis.Client, hub *websocket.Hub) *Handler {
	return &Handler{
		db:    db,
		log:   log,
		redis: redisClient,
		hub:   hub,
	}
}

// AddMemberRequest represents a request to add a member to a server
type AddMemberRequest struct {
	UserID string `json:"user_id" binding:"required,uuid"`
}

// UpdateMemberRequest represents a request to update a member's role
type UpdateMemberRequest struct {
	Role string `json:"role" binding:"required,oneof=admin member"`
}

// MemberWithUser represents a member with user details
type MemberWithUser struct {
	UserID         string  `json:"user_id"`
	Username       string  `json:"username"`
	DisplayName    *string `json:"display_name,omitempty"`
	Bio            *string `json:"bio,omitempty"`
	AvatarURL      *string `json:"avatar_url,omitempty"`
	HeaderImageURL *string `json:"header_image_url,omitempty"`
	ColorScheme    *string `json:"color_scheme,omitempty"`
	Role           string  `json:"role"`
	JoinedAt       string  `json:"joined_at"`
	LastSeen       *int64  `json:"last_seen,omitempty"`
}

// ListMembers returns all members of a server
func (h *Handler) ListMembers(c *gin.Context) {
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch members"})
		return
	}

	if !isMember {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this server"})
		return
	}

	// Get all members of the server with user details
	query := `
		SELECT sm.user_id, u.username, u.display_name, u.bio, u.avatar_url, u.header_image_url, u.color_scheme, sm.role, sm.joined_at
		FROM server_members sm
		INNER JOIN users u ON sm.user_id = u.id
		WHERE sm.server_id = $1
		ORDER BY sm.joined_at ASC
	`

	rows, err := h.db.Query(query, serverID)
	if err != nil {
		h.log.Error("Failed to query members", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch members"})
		return
	}
	defer func() { _ = rows.Close() }()

	members := []MemberWithUser{}
	for rows.Next() {
		var member MemberWithUser
		err := rows.Scan(
			&member.UserID,
			&member.Username,
			&member.DisplayName,
			&member.Bio,
			&member.AvatarURL,
			&member.HeaderImageURL,
			&member.ColorScheme,
			&member.Role,
			&member.JoinedAt,
		)
		if err != nil {
			h.log.Error("Failed to scan member", "error", err)
			continue
		}
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating members", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch members"})
		return
	}

	// Batch fetch last_seen timestamps from Redis
	if len(members) > 0 {
		keys := make([]string, len(members))
		for i, m := range members {
			keys[i] = fmt.Sprintf("last_seen:%s", m.UserID)
		}
		ctx := context.Background()
		vals, err := h.redis.MGet(ctx, keys...).Result()
		if err == nil {
			for i, val := range vals {
				if val != nil {
					if tsStr, ok := val.(string); ok {
						if ts, parseErr := strconv.ParseInt(tsStr, 10, 64); parseErr == nil {
							members[i].LastSeen = &ts
						}
					}
				}
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{"members": members})
}

// AddMember adds a user to a server
func (h *Handler) AddMember(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	// Validate server ID
	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid server ID"})
		return
	}

	var req AddMemberRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	// Check if requesting user is owner or admin
	var role string
	roleQuery := `
		SELECT role FROM server_members
		WHERE server_id = $1 AND user_id = $2
	`

	err := h.db.QueryRow(roleQuery, serverID, userID).Scan(&role)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this server"})
		return
	} else if err != nil {
		h.log.Error("Failed to check permissions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to add member"})
		return
	}

	if role != "owner" && role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only owners and admins can add members"})
		return
	}

	// Check if user to add exists
	var userExists bool
	userQuery := `SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`
	err = h.db.QueryRow(userQuery, req.UserID).Scan(&userExists)
	if err != nil {
		h.log.Error("Failed to check user existence", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to add member"})
		return
	}

	if !userExists {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}

	// TODO: [RBAC] Currently grants full access as 'member' role with no restrictions.
	// When RBAC/SBAC is implemented, new members should receive a configurable default
	// role with restricted permissions.
	insertQuery := `
		INSERT INTO server_members (server_id, user_id, role, joined_at)
		VALUES ($1, $2, 'member', NOW())
		ON CONFLICT (server_id, user_id) DO NOTHING
		RETURNING joined_at
	`

	var member models.ServerMember
	member.ServerID = serverID
	member.UserID = req.UserID
	member.Role = "member"

	err = h.db.QueryRow(insertQuery, serverID, req.UserID).Scan(&member.JoinedAt)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusConflict, gin.H{"error": "User is already a member"})
		return
	} else if err != nil {
		h.log.Error("Failed to add member", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to add member"})
		return
	}

	h.log.Info("Member added", "server_id", serverID, "new_member", req.UserID, "added_by", userID)

	c.JSON(http.StatusCreated, gin.H{"member": member})
}

// UpdateMember updates a member's role
func (h *Handler) UpdateMember(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")
	targetUserID := c.Param("user_id")

	// Validate IDs
	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid server ID"})
		return
	}
	if _, err := uuid.Parse(targetUserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}

	var req UpdateMemberRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	// Get requesting user's role and target user's role
	var requesterRole, targetRole sql.NullString
	roleQuery := `
		SELECT
			(SELECT role FROM server_members WHERE server_id = $1 AND user_id = $2) as requester_role,
			(SELECT role FROM server_members WHERE server_id = $1 AND user_id = $3) as target_role
	`

	err := h.db.QueryRow(roleQuery, serverID, userID, targetUserID).Scan(&requesterRole, &targetRole)
	if err != nil {
		h.log.Error("Failed to check roles", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update member"})
		return
	}

	// Check permissions
	if !requesterRole.Valid {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this server"})
		return
	}

	if requesterRole.String != "owner" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the server owner can change member roles"})
		return
	}

	if !targetRole.Valid {
		c.JSON(http.StatusNotFound, gin.H{"error": "User is not a member of this server"})
		return
	}

	if targetRole.String == "owner" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Cannot change the owner's role"})
		return
	}

	// Update role
	updateQuery := `
		UPDATE server_members
		SET role = $1
		WHERE server_id = $2 AND user_id = $3
		RETURNING joined_at
	`

	var member models.ServerMember
	member.ServerID = serverID
	member.UserID = targetUserID
	member.Role = req.Role

	err = h.db.QueryRow(updateQuery, req.Role, serverID, targetUserID).Scan(&member.JoinedAt)
	if err != nil {
		h.log.Error("Failed to update member role", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update member"})
		return
	}

	h.log.Info("Member role updated", "server_id", serverID, "target_user", targetUserID, "new_role", req.Role, "updated_by", userID)

	c.JSON(http.StatusOK, gin.H{"member": member})
}

// RemoveMember removes a member from a server (kick or leave)
func (h *Handler) RemoveMember(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")
	targetUserID := c.Param("user_id")

	// Validate IDs
	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid server ID"})
		return
	}
	if _, err := uuid.Parse(targetUserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}

	// Get roles
	var requesterRole, targetRole sql.NullString
	roleQuery := `
		SELECT
			(SELECT role FROM server_members WHERE server_id = $1 AND user_id = $2) as requester_role,
			(SELECT role FROM server_members WHERE server_id = $1 AND user_id = $3) as target_role
	`

	err := h.db.QueryRow(roleQuery, serverID, userID, targetUserID).Scan(&requesterRole, &targetRole)
	if err != nil {
		h.log.Error("Failed to check roles", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to remove member"})
		return
	}

	if !requesterRole.Valid {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this server"})
		return
	}

	if !targetRole.Valid {
		c.JSON(http.StatusNotFound, gin.H{"error": "User is not a member of this server"})
		return
	}

	// Check permissions
	isSelfRemoval := userID == targetUserID

	if !isSelfRemoval {
		// Kicking someone else - need to be owner or admin
		if requesterRole.String != "owner" && requesterRole.String != "admin" {
			c.JSON(http.StatusForbidden, gin.H{"error": "Only owners and admins can remove members"})
			return
		}

		// Can't kick the owner
		if targetRole.String == "owner" {
			c.JSON(http.StatusForbidden, gin.H{"error": "Cannot remove the server owner"})
			return
		}

		// Admins can't kick other admins or the owner
		if requesterRole.String == "admin" && (targetRole.String == "admin" || targetRole.String == "owner") {
			c.JSON(http.StatusForbidden, gin.H{"error": "Admins cannot remove other admins or the owner"})
			return
		}
	} else {
		// Self-removal (leaving) - owner can't leave their own server
		if requesterRole.String == "owner" {
			c.JSON(http.StatusForbidden, gin.H{"error": "Server owner cannot leave. Delete the server or transfer ownership first."})
			return
		}
	}

	// Remove member and clean up related data in a transaction
	tx, err := h.db.Begin()
	if err != nil {
		h.log.Error("Failed to start transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to remove member"})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback transaction", "error", rbErr)
		}
	}()

	// Delete the membership
	deleteQuery := `DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`
	_, err = tx.Exec(deleteQuery, serverID, targetUserID)
	if err != nil {
		h.log.Error("Failed to remove member", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to remove member"})
		return
	}

	// Revoke E2EE keys — delete all wrapped keys for this user in this server's channels
	keysQuery := `
		DELETE FROM channel_keys
		WHERE user_id = $1
		  AND channel_id IN (SELECT id FROM channels WHERE server_id = $2)
	`
	_, err = tx.Exec(keysQuery, targetUserID, serverID)
	if err != nil {
		h.log.Error("Failed to clean up channel keys", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to remove member"})
		return
	}

	// Clean up pending key requests
	pendingQuery := `
		DELETE FROM pending_key_requests
		WHERE user_id = $1
		  AND channel_id IN (SELECT id FROM channels WHERE server_id = $2)
	`
	_, err = tx.Exec(pendingQuery, targetUserID, serverID)
	if err != nil {
		h.log.Error("Failed to clean up pending key requests", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to remove member"})
		return
	}

	// Clean up read states
	readQuery := `
		DELETE FROM channel_read_states
		WHERE user_id = $1
		  AND channel_id IN (SELECT id FROM channels WHERE server_id = $2)
	`
	_, err = tx.Exec(readQuery, targetUserID, serverID)
	if err != nil {
		h.log.Error("Failed to clean up read states", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to remove member"})
		return
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to remove member"})
		return
	}

	action := "removed"
	if isSelfRemoval {
		action = "left"
	}

	h.log.Info("Member "+action, "server_id", serverID, "target_user", targetUserID, "by_user", userID)

	// Broadcast to server subscribers
	serverUUID, _ := uuid.Parse(serverID)
	h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
		Type: "member_removed",
		Data: map[string]interface{}{
			"server_id": serverID,
			"user_id":   targetUserID,
		},
	})

	// ── CSK rotation trigger (#96): revoke current epoch for all E2EE channels ──
	h.triggerKeyRevocationsForServer(serverID, targetUserID, userID)

	c.JSON(http.StatusOK, gin.H{"message": "Member " + action + " successfully"})
}

// triggerKeyRevocationsForServer creates key_revocations records and broadcasts
// key_revocation events for all E2EE channels in a server. Called after a member
// is removed so remaining clients rotate to a new epoch the removed user can't decrypt.
func (h *Handler) triggerKeyRevocationsForServer(serverID, removedUserID, actorID string) {
	rows, err := h.db.Query(
		`SELECT c.id, COALESCE(MAX(ck.key_version), 1)
		 FROM channels c
		 LEFT JOIN channel_keys ck ON ck.channel_id = c.id
		 WHERE c.server_id = $1 AND c.is_encrypted = TRUE
		 GROUP BY c.id`,
		serverID,
	)
	if err != nil {
		h.log.Error("Failed to query E2EE channels for key revocation", "error", err, "server_id", serverID)
		return
	}
	defer rows.Close()

	serverUUID, _ := uuid.Parse(serverID)

	for rows.Next() {
		var channelID string
		var maxEpoch int
		if err := rows.Scan(&channelID, &maxEpoch); err != nil {
			h.log.Error("Failed to scan channel for key revocation", "error", err)
			continue
		}

		// Insert key_revocations record (ignore conflict if already revoked)
		_, err := h.db.Exec(
			`INSERT INTO key_revocations (channel_id, revoked_epoch, successor_epoch, reason, revoked_by)
			 VALUES ($1, $2, $3, 'member_removal', $4)
			 ON CONFLICT (channel_id, revoked_epoch) DO NOTHING`,
			channelID, maxEpoch, maxEpoch+1, actorID,
		)
		if err != nil {
			h.log.Error("Failed to insert key revocation", "error", err, "channel_id", channelID)
			continue
		}

		// Broadcast key_revocation to remaining server members
		h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
			Type: "key_revocation",
			Data: map[string]interface{}{
				"channel_id":      channelID,
				"server_id":       serverID,
				"revoked_epoch":   maxEpoch,
				"new_epoch":       maxEpoch + 1,
				"reason":          "member_removal",
				"removed_user_id": removedUserID,
			},
		})
	}

	h.log.Info("Key revocations triggered for member removal",
		"server_id", serverID, "removed_user", removedUserID, "actor", actorID)
}
