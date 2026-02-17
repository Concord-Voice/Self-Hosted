package members

import (
	"database/sql"
	"net/http"

	"github.com/concord/control-plane/internal/models"
	"github.com/concord/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Handler handles member-related requests
type Handler struct {
	db  *sql.DB
	log *logger.Logger
}

// NewHandler creates a new member handler
func NewHandler(db *sql.DB, log *logger.Logger) *Handler {
	return &Handler{
		db:  db,
		log: log,
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
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	JoinedAt string `json:"joined_at"`
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
		SELECT sm.user_id, u.username, sm.role, sm.joined_at
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
	defer rows.Close()

	members := []MemberWithUser{}
	for rows.Next() {
		var member MemberWithUser
		err := rows.Scan(
			&member.UserID,
			&member.Username,
			&member.Role,
			&member.JoinedAt,
		)
		if err != nil {
			h.log.Error("Failed to scan member", "error", err)
			continue
		}
		members = append(members, member)
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
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
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

	// Add member with role 'member'
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
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Get requesting user's role and target user's role
	var requesterRole, targetRole string
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
	if requesterRole != "owner" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the server owner can change member roles"})
		return
	}

	if targetRole == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "User is not a member of this server"})
		return
	}

	if targetRole == "owner" {
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
	var requesterRole, targetRole string
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

	if requesterRole == "" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this server"})
		return
	}

	if targetRole == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "User is not a member of this server"})
		return
	}

	// Check permissions
	isSelfRemoval := userID == targetUserID

	if !isSelfRemoval {
		// Kicking someone else - need to be owner or admin
		if requesterRole != "owner" && requesterRole != "admin" {
			c.JSON(http.StatusForbidden, gin.H{"error": "Only owners and admins can remove members"})
			return
		}

		// Can't kick the owner
		if targetRole == "owner" {
			c.JSON(http.StatusForbidden, gin.H{"error": "Cannot remove the server owner"})
			return
		}

		// Admins can't kick other admins or the owner
		if requesterRole == "admin" && (targetRole == "admin" || targetRole == "owner") {
			c.JSON(http.StatusForbidden, gin.H{"error": "Admins cannot remove other admins or the owner"})
			return
		}
	} else {
		// Self-removal (leaving) - owner can't leave their own server
		if requesterRole == "owner" {
			c.JSON(http.StatusForbidden, gin.H{"error": "Server owner cannot leave. Delete the server or transfer ownership first."})
			return
		}
	}

	// Remove member
	deleteQuery := `DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`

	_, err = h.db.Exec(deleteQuery, serverID, targetUserID)
	if err != nil {
		h.log.Error("Failed to remove member", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to remove member"})
		return
	}

	action := "removed"
	if isSelfRemoval {
		action = "left"
	}

	h.log.Info("Member "+action, "server_id", serverID, "target_user", targetUserID, "by_user", userID)

	c.JSON(http.StatusOK, gin.H{"message": "Member " + action + " successfully"})
}
