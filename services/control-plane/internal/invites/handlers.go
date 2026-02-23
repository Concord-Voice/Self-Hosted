package invites

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/concord/control-plane/internal/models"
	"github.com/concord/control-plane/internal/websocket"
	"github.com/concord/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Handler handles invite-related requests.
type Handler struct {
	db  *sql.DB
	log *logger.Logger
	hub *websocket.Hub
}

// NewHandler creates a new invite handler.
func NewHandler(db *sql.DB, log *logger.Logger, hub *websocket.Hub) *Handler {
	return &Handler{db: db, log: log, hub: hub}
}

// createInviteRequest is the JSON body for creating an invite.
type createInviteRequest struct {
	MaxUses   *int `json:"max_uses"`   // nil → default 1
	ExpiresIn *int `json:"expires_in"` // seconds; nil → default 86400 (24h)
}

// joinServerRequest is the JSON body for joining via invite code.
type joinServerRequest struct {
	Code string `json:"code" binding:"required"`
}

// --- helpers ---

func (h *Handler) checkOwnerOrAdmin(c *gin.Context, serverID, userID string) (string, bool) {
	var role string
	err := h.db.QueryRow(
		`SELECT role FROM server_members WHERE server_id = $1 AND user_id = $2`,
		serverID, userID,
	).Scan(&role)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this server"})
		return "", false
	}
	if err != nil {
		h.log.Error("Failed to check membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal error"})
		return "", false
	}
	if role != "owner" && role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only owners and admins can manage invites"})
		return "", false
	}
	return role, true
}

// CreateInvite generates a new invite code for a server.
// Default: 1 use, expires in 24 hours. Caller can override.
func (h *Handler) CreateInvite(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid server ID"})
		return
	}

	var req createInviteRequest
	// Allow empty body — defaults apply
	_ = c.ShouldBindJSON(&req)

	if _, ok := h.checkOwnerOrAdmin(c, serverID, userID); !ok {
		return
	}

	// Apply defaults (max_uses: nil → default 1, 0 → unlimited/NULL, >0 → that value)
	var maxUsesPtr *int
	if req.MaxUses != nil && *req.MaxUses > 0 {
		maxUses := *req.MaxUses
		if maxUses > 100 {
			maxUses = 100
		}
		maxUsesPtr = &maxUses
	} else if req.MaxUses == nil {
		defaultMax := 1
		maxUsesPtr = &defaultMax
	}
	// When req.MaxUses is explicitly 0 → maxUsesPtr stays nil → NULL in DB (unlimited)

	expiresInSec := 86400 // 24 hours
	if req.ExpiresIn != nil {
		expiresInSec = *req.ExpiresIn
		if expiresInSec < 300 { // minimum 5 minutes
			expiresInSec = 300
		}
		if expiresInSec > 604800 { // maximum 7 days
			expiresInSec = 604800
		}
	}
	expiresAt := time.Now().UTC().Add(time.Duration(expiresInSec) * time.Second)

	// Generate unique code with retry
	var code string
	var err error
	for attempts := 0; attempts < 5; attempts++ {
		code, err = GenerateCode()
		if err != nil {
			h.log.Error("Failed to generate invite code", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create invite"})
			return
		}
		// Try insert — UNIQUE constraint will catch collisions
		var invite models.ServerInvite
		invite.ID = uuid.New().String()
		invite.ServerID = serverID
		invite.Code = code
		invite.CreatedBy = userID
		invite.MaxUses = maxUsesPtr
		invite.UseCount = 0
		invite.ExpiresAt = &expiresAt
		invite.IsRevoked = false

		insertErr := h.db.QueryRow(`
			INSERT INTO server_invites (id, server_id, code, created_by, max_uses, expires_at, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, NOW())
			RETURNING created_at
		`, invite.ID, serverID, code, userID, maxUsesPtr, expiresAt).Scan(&invite.CreatedAt)

		if insertErr != nil {
			// Likely unique constraint violation — retry with new code
			continue
		}

		h.log.Info("Invite created", "server_id", serverID, "code", code, "created_by", userID)
		c.JSON(http.StatusCreated, gin.H{"invite": invite})
		return
	}

	h.log.Error("Failed to create unique invite code after retries")
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create invite"})
}

// ListInvites returns all invites for a server.
func (h *Handler) ListInvites(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid server ID"})
		return
	}

	if _, ok := h.checkOwnerOrAdmin(c, serverID, userID); !ok {
		return
	}

	rows, err := h.db.Query(`
		SELECT si.id, si.server_id, si.code, si.created_by, si.max_uses,
		       si.use_count, si.expires_at, si.is_revoked, si.created_at,
		       u.username AS creator_username
		FROM server_invites si
		INNER JOIN users u ON si.created_by = u.id
		WHERE si.server_id = $1
		ORDER BY si.created_at DESC
	`, serverID)
	if err != nil {
		h.log.Error("Failed to query invites", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list invites"})
		return
	}
	defer func() { _ = rows.Close() }()

	invites := []models.ServerInviteWithCreator{}
	for rows.Next() {
		var inv models.ServerInviteWithCreator
		if err := rows.Scan(
			&inv.ID, &inv.ServerID, &inv.Code, &inv.CreatedBy,
			&inv.MaxUses, &inv.UseCount, &inv.ExpiresAt,
			&inv.IsRevoked, &inv.CreatedAt, &inv.CreatorUsername,
		); err != nil {
			h.log.Error("Failed to scan invite", "error", err)
			continue
		}
		invites = append(invites, inv)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating invites", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list invites"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"invites": invites})
}

// RevokeInvite soft-revokes an invite by setting is_revoked = true.
func (h *Handler) RevokeInvite(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")
	inviteID := c.Param("invite_id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid server ID"})
		return
	}
	if _, err := uuid.Parse(inviteID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid invite ID"})
		return
	}

	if _, ok := h.checkOwnerOrAdmin(c, serverID, userID); !ok {
		return
	}

	result, err := h.db.Exec(`
		UPDATE server_invites SET is_revoked = TRUE
		WHERE id = $1 AND server_id = $2 AND is_revoked = FALSE
	`, inviteID, serverID)
	if err != nil {
		h.log.Error("Failed to revoke invite", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to revoke invite"})
		return
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Invite not found or already revoked"})
		return
	}

	h.log.Info("Invite revoked", "invite_id", inviteID, "server_id", serverID, "revoked_by", userID)
	c.JSON(http.StatusOK, gin.H{"message": "Invite revoked"})
}

// JoinServer allows any authenticated user to join a server via invite code.
func (h *Handler) JoinServer(c *gin.Context) {
	userID := c.GetString("user_id")

	var req joinServerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invite code is required"})
		return
	}

	if len(req.Code) != 8 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid invite code format"})
		return
	}

	tx, err := h.db.Begin()
	if err != nil {
		h.log.Error("Failed to begin transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to join server"})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback", "error", rbErr)
		}
	}()

	// Lock the invite row to prevent race conditions on use_count
	var invite models.ServerInvite
	err = tx.QueryRow(`
		SELECT id, server_id, code, created_by, max_uses, use_count, expires_at, is_revoked, created_at
		FROM server_invites
		WHERE code = $1
		FOR UPDATE
	`, req.Code).Scan(
		&invite.ID, &invite.ServerID, &invite.Code, &invite.CreatedBy,
		&invite.MaxUses, &invite.UseCount, &invite.ExpiresAt,
		&invite.IsRevoked, &invite.CreatedAt,
	)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Invalid invite code"})
		return
	}
	if err != nil {
		h.log.Error("Failed to query invite", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to join server"})
		return
	}

	// Validate invite
	if invite.IsRevoked {
		c.JSON(http.StatusGone, gin.H{"error": "This invite has been revoked"})
		return
	}
	if invite.ExpiresAt != nil && invite.ExpiresAt.Before(time.Now().UTC()) {
		c.JSON(http.StatusGone, gin.H{"error": "This invite has expired"})
		return
	}
	if invite.MaxUses != nil && *invite.MaxUses > 0 && invite.UseCount >= *invite.MaxUses {
		c.JSON(http.StatusGone, gin.H{"error": "This invite has reached its maximum uses"})
		return
	}

	// Check if already a member
	var exists bool
	err = tx.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		invite.ServerID, userID,
	).Scan(&exists)
	if err != nil {
		h.log.Error("Failed to check membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to join server"})
		return
	}
	if exists {
		c.JSON(http.StatusConflict, gin.H{"error": "You are already a member of this server"})
		return
	}

	// Add member
	// TODO: [RBAC] Currently grants full access as 'member' role with no restrictions.
	// When RBAC/SBAC is implemented, new members should receive a configurable default
	// role with restricted permissions.
	_, err = tx.Exec(`
		INSERT INTO server_members (server_id, user_id, role, joined_at)
		VALUES ($1, $2, 'member', NOW())
	`, invite.ServerID, userID)
	if err != nil {
		h.log.Error("Failed to add member", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to join server"})
		return
	}

	// Increment use count
	_, err = tx.Exec(`UPDATE server_invites SET use_count = use_count + 1 WHERE id = $1`, invite.ID)
	if err != nil {
		h.log.Error("Failed to increment use count", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to join server"})
		return
	}

	// Fetch server details for response
	var server models.Server
	err = tx.QueryRow(`
		SELECT id, name, icon_url, owner_id, e2ee_default, created_at, updated_at
		FROM servers WHERE id = $1
	`, invite.ServerID).Scan(
		&server.ID, &server.Name, &server.IconURL, &server.OwnerID,
		&server.E2EEDefault, &server.CreatedAt, &server.UpdatedAt,
	)
	if err != nil {
		h.log.Error("Failed to fetch server", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to join server"})
		return
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to join server"})
		return
	}

	h.log.Info("User joined server", "user_id", userID, "server_id", invite.ServerID, "code", req.Code)

	// Broadcast member_joined to all connected clients so member lists update in real time
	var username string
	var displayName *string
	var avatarURL *string
	_ = h.db.QueryRow(
		"SELECT username, display_name, avatar_url FROM users WHERE id = $1", userID,
	).Scan(&username, &displayName, &avatarURL)

	if serverUUID, parseErr := uuid.Parse(invite.ServerID); parseErr == nil {
		h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
			Type: "member_joined",
			Data: map[string]interface{}{
				"server_id":    invite.ServerID,
				"user_id":      userID,
				"username":     username,
				"display_name": displayName,
				"avatar_url":   avatarURL,
				"role":         "member",
			},
		})
	}

	// Create pending key requests for all E2EE channels in the server.
	// This enables existing members with the channel key to distribute it to the new member.
	rows, qErr := h.db.Query(`
		INSERT INTO pending_key_requests (channel_id, user_id)
		SELECT c.id, $1
		FROM channels c
		WHERE c.server_id = $2 AND c.is_encrypted = TRUE
		ON CONFLICT (channel_id, user_id) DO NOTHING
		RETURNING channel_id
	`, userID, invite.ServerID)
	if qErr != nil {
		h.log.Error("Failed to create pending key requests", "error", qErr)
	} else {
		var pendingChannels []string
		for rows.Next() {
			var chID string
			if sErr := rows.Scan(&chID); sErr == nil {
				pendingChannels = append(pendingChannels, chID)
			}
		}
		_ = rows.Close()

		// Broadcast key_needed to server subscribers so they can auto-distribute
		if len(pendingChannels) > 0 {
			if serverUUID, parseErr := uuid.Parse(invite.ServerID); parseErr == nil {
				h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
					Type: "key_needed",
					Data: map[string]interface{}{
						"server_id":   invite.ServerID,
						"user_id":     userID,
						"channel_ids": pendingChannels,
					},
				})
			}
			h.log.Info("Pending key requests created",
				"user_id", userID, "server_id", invite.ServerID,
				"channels", len(pendingChannels))
		}
	}

	c.JSON(http.StatusOK, gin.H{"server": server, "role": "member"})
}

// GetInviteInfo returns a preview of the server for an invite code.
func (h *Handler) GetInviteInfo(c *gin.Context) {
	code := c.Param("code")

	if len(code) != 8 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid invite code"})
		return
	}

	var (
		serverID    string
		expiresAt   *time.Time
		isRevoked   bool
		maxUses     *int
		useCount    int
		serverName  string
		serverIcon  *string
		memberCount int
	)

	err := h.db.QueryRow(`
		SELECT si.server_id, si.expires_at, si.is_revoked, si.max_uses, si.use_count,
		       s.name, s.icon_url,
		       (SELECT COUNT(*) FROM server_members WHERE server_id = s.id)
		FROM server_invites si
		INNER JOIN servers s ON si.server_id = s.id
		WHERE si.code = $1
	`, code).Scan(
		&serverID, &expiresAt, &isRevoked, &maxUses, &useCount,
		&serverName, &serverIcon, &memberCount,
	)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Invalid invite code"})
		return
	}
	if err != nil {
		h.log.Error("Failed to fetch invite info", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch invite info"})
		return
	}

	valid := !isRevoked &&
		(expiresAt == nil || expiresAt.After(time.Now().UTC())) &&
		(maxUses == nil || *maxUses == 0 || useCount < *maxUses)

	c.JSON(http.StatusOK, gin.H{
		"server_name":  serverName,
		"server_icon":  serverIcon,
		"member_count": memberCount,
		"valid":        valid,
	})
}
