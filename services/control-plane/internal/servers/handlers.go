// Package servers provides handlers for managing Concord servers (community-style communities).
package servers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/models"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Handler handles server-related requests
type Handler struct {
	db       *sql.DB
	log      *logger.Logger
	hub      *websocket.Hub
	resolver *rbac.Resolver
}

// NewHandler creates a new server handler
func NewHandler(db *sql.DB, log *logger.Logger, hub *websocket.Hub, resolver *rbac.Resolver) *Handler {
	return &Handler{
		db:       db,
		log:      log,
		hub:      hub,
		resolver: resolver,
	}
}

// CreateServerRequest represents a request to create a server
type CreateServerRequest struct {
	Name        string  `json:"name" binding:"required,min=3,max=100"`
	IconURL     *string `json:"icon_url"`
	BannerURL   *string `json:"banner_url"`
	E2EEDefault *bool   `json:"e2ee_default,omitempty"` // Defaults to true if not specified
}

// UpdateServerRequest represents a request to update a server.
// IconURL uses json.RawMessage so we can distinguish between
// "field absent" (nil → don't touch icon) and "field set to null" (remove icon).
type UpdateServerRequest struct {
	Name                 string          `json:"name" binding:"required,min=3,max=100"`
	IconURL              json.RawMessage `json:"icon_url"`
	BannerURL            json.RawMessage `json:"banner_url"`
	AllowEmbeddedContent *bool           `json:"allow_embedded_content,omitempty"` // Server-level embed policy
}

// ListServers returns all servers the user is a member of
func (h *Handler) ListServers(c *gin.Context) {
	userID := c.GetString("user_id")

	// Get connected user IDs from the hub for online count computation
	connectedUsers := h.hub.GetConnectedUsers()
	connectedIDs := make([]string, 0, len(connectedUsers))
	for uid := range connectedUsers {
		connectedIDs = append(connectedIDs, uid.String())
	}

	query := `
		SELECT s.id, s.name, s.icon_url, s.banner_url, s.owner_id, s.e2ee_default, s.allow_embedded_content, s.created_at, s.updated_at, sm.role,
			(SELECT COUNT(*) FROM server_members WHERE server_id = s.id) AS member_count,
			(SELECT COUNT(*) FROM server_members WHERE server_id = s.id AND user_id = ANY($2::uuid[])) AS online_count
		FROM servers s
		INNER JOIN server_members sm ON s.id = sm.server_id
		WHERE sm.user_id = $1
		ORDER BY s.created_at DESC
	`

	rows, err := h.db.Query(query, userID, pq.Array(connectedIDs))
	if err != nil {
		h.log.Error("Failed to query servers", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch servers"})
		return
	}
	defer func() { _ = rows.Close() }()

	servers := []models.ServerWithRole{}
	for rows.Next() {
		var server models.ServerWithRole
		err := rows.Scan(
			&server.ID,
			&server.Name,
			&server.IconURL,
			&server.BannerURL,
			&server.OwnerID,
			&server.E2EEDefault,
			&server.AllowEmbeddedContent,
			&server.CreatedAt,
			&server.UpdatedAt,
			&server.Role,
			&server.MemberCount,
			&server.OnlineCount,
		)
		if err != nil {
			h.log.Error("Failed to scan server", "error", err)
			continue
		}
		servers = append(servers, server)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating servers", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch servers"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"servers": servers})
}

// CreateServer creates a new server
func (h *Handler) CreateServer(c *gin.Context) {
	userID := c.GetString("user_id")

	var req CreateServerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	// Start transaction
	tx, err := h.db.Begin()
	if err != nil {
		h.log.Error("Failed to start transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create server"})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback transaction", "error", rbErr)
		}
	}()

	// Validate icon_url if provided
	if req.IconURL != nil && *req.IconURL != "" {
		if !strings.HasPrefix(*req.IconURL, "data:image/") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Icon must be a valid image data URL"})
			return
		}
		if len(*req.IconURL) > 500000 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Icon data URL must be less than 500KB"})
			return
		}
	}

	// Validate banner_url if provided
	if req.BannerURL != nil && *req.BannerURL != "" {
		if !strings.HasPrefix(*req.BannerURL, "data:image/") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Banner must be a valid image data URL"})
			return
		}
		if len(*req.BannerURL) > 1000000 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Banner data URL must be less than 1MB"})
			return
		}
	}

	// Determine E2EE default (true if not specified)
	e2eeDefault := true
	if req.E2EEDefault != nil {
		e2eeDefault = *req.E2EEDefault
	}

	// Create server (allow_embedded_content defaults to false — safe default)
	serverID := uuid.New().String()
	serverQuery := `
		INSERT INTO servers (id, name, icon_url, banner_url, owner_id, e2ee_default, allow_embedded_content, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, FALSE, NOW(), NOW())
		RETURNING created_at, updated_at
	`

	var server models.Server
	server.ID = serverID
	server.Name = req.Name
	server.IconURL = req.IconURL
	server.BannerURL = req.BannerURL
	server.OwnerID = userID
	server.E2EEDefault = e2eeDefault
	server.AllowEmbeddedContent = false

	err = tx.QueryRow(serverQuery, serverID, req.Name, req.IconURL, req.BannerURL, userID, e2eeDefault).Scan(
		&server.CreatedAt,
		&server.UpdatedAt,
	)
	if err != nil {
		h.log.Error("Failed to create server", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create server"})
		return
	}

	// Add owner as member
	memberQuery := `
		INSERT INTO server_members (server_id, user_id, role, joined_at)
		VALUES ($1, $2, 'owner', NOW())
	`

	_, err = tx.Exec(memberQuery, serverID, userID)
	if err != nil {
		h.log.Error("Failed to add owner as member", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create server"})
		return
	}

	// Create @all default role with base permissions (RBAC Phase 2A)
	// This role is auto-assigned to all server members
	allRoleID := uuid.New().String()
	allRoleQuery := `
		INSERT INTO roles (id, server_id, name, position, permissions, is_default, is_managed)
		VALUES ($1, $2, '@all', 0, $3, TRUE, TRUE)
	`
	_, err = tx.Exec(allRoleQuery, allRoleID, serverID, int64(rbac.BasePermissions))
	if err != nil {
		h.log.Error("Failed to create @all role", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create server"})
		return
	}

	// Assign @all role to the owner
	assignRoleQuery := `
		INSERT INTO member_roles (server_id, user_id, role_id, assigned_by)
		VALUES ($1, $2, $3, $4)
	`
	_, err = tx.Exec(assignRoleQuery, serverID, userID, allRoleID, userID)
	if err != nil {
		h.log.Error("Failed to assign @all role to owner", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create server"})
		return
	}

	// Commit transaction — no default channels are created at server spawn.
	// The owner creates channels through the UI, which handles E2EE key
	// generation and wrapping when the server's e2ee_default is enabled.
	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create server"})
		return
	}

	h.log.Info("Server created", "server_id", serverID, "user_id", userID)

	c.JSON(http.StatusCreated, gin.H{
		"server": server,
		"role":   "owner",
	})
}

// GetServer returns a specific server
func (h *Handler) GetServer(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	// Validate server ID
	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid server ID"})
		return
	}

	// Check if user is a member
	var role string
	memberQuery := `
		SELECT role FROM server_members
		WHERE server_id = $1 AND user_id = $2
	`

	err := h.db.QueryRow(memberQuery, serverID, userID).Scan(&role)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this server"})
		return
	} else if err != nil {
		h.log.Error("Failed to check membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch server"})
		return
	}

	// Get server details
	var server models.Server
	serverQuery := `
		SELECT id, name, icon_url, banner_url, owner_id, e2ee_default, allow_embedded_content, created_at, updated_at
		FROM servers
		WHERE id = $1
	`

	err = h.db.QueryRow(serverQuery, serverID).Scan(
		&server.ID,
		&server.Name,
		&server.IconURL,
		&server.BannerURL,
		&server.OwnerID,
		&server.E2EEDefault,
		&server.AllowEmbeddedContent,
		&server.CreatedAt,
		&server.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Server not found"})
		return
	} else if err != nil {
		h.log.Error("Failed to fetch server", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch server"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"server": server,
		"role":   role,
	})
}

// UpdateServer updates a server's details
func (h *Handler) UpdateServer(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	// Validate server ID
	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid server ID"})
		return
	}

	var req UpdateServerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	// Check permission to manage server
	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", rbac.PermManageServer)
	if err != nil {
		h.log.Error("Failed to check permissions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update server"})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": "insufficient permissions"})
		return
	}

	// Legacy role for backwards-compat response (dropped in migration 000036)
	var role string
	if err := h.db.QueryRow(`SELECT role FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, userID).Scan(&role); err != nil {
		h.log.Warn("Failed to fetch legacy role for UpdateServer response", "error", err, "server_id", serverID, "user_id", userID)
	}

	// Determine if icon_url was explicitly provided in the request body.
	// json.RawMessage lets us distinguish:
	//   nil             → field absent (don't touch icon)
	//   []byte("null")  → explicitly null (remove icon)
	//   []byte("\"...\"") → new icon value
	iconURLProvided := req.IconURL != nil
	var iconURL *string

	if iconURLProvided {
		if string(req.IconURL) == "null" {
			iconURL = nil
		} else {
			var url string
			if err := json.Unmarshal(req.IconURL, &url); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid icon_url format"})
				return
			}
			iconURL = &url
		}
	}

	// Validate icon_url if a new value was provided
	if iconURL != nil && *iconURL != "" {
		if !strings.HasPrefix(*iconURL, "data:image/") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Icon must be a valid image data URL"})
			return
		}
		if len(*iconURL) > 500000 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Icon data URL must be less than 500KB"})
			return
		}
	}

	// Same pattern for banner_url
	bannerURLProvided := req.BannerURL != nil
	var bannerURL *string

	if bannerURLProvided {
		if string(req.BannerURL) == "null" {
			bannerURL = nil
		} else {
			var url string
			if err := json.Unmarshal(req.BannerURL, &url); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid banner_url format"})
				return
			}
			bannerURL = &url
		}
	}

	// Validate banner_url if a new value was provided
	if bannerURL != nil && *bannerURL != "" {
		if !strings.HasPrefix(*bannerURL, "data:image/") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Banner must be a valid image data URL"})
			return
		}
		if len(*bannerURL) > 1000000 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Banner data URL must be less than 1MB"})
			return
		}
	}

	// Build dynamic UPDATE query based on which fields were provided
	var server models.Server
	server.ID = serverID

	setClauses := []string{"name = $1", "updated_at = NOW()"}
	args := []interface{}{req.Name}
	argIdx := 2

	if iconURLProvided {
		setClauses = append(setClauses, fmt.Sprintf("icon_url = $%d", argIdx))
		args = append(args, iconURL)
		argIdx++
	}
	if bannerURLProvided {
		setClauses = append(setClauses, fmt.Sprintf("banner_url = $%d", argIdx))
		args = append(args, bannerURL)
		argIdx++
	}
	if req.AllowEmbeddedContent != nil {
		setClauses = append(setClauses, fmt.Sprintf("allow_embedded_content = $%d", argIdx))
		args = append(args, *req.AllowEmbeddedContent)
		argIdx++
	}

	args = append(args, serverID)
	updateQuery := fmt.Sprintf("UPDATE servers SET %s WHERE id = $%d RETURNING name, icon_url, banner_url, owner_id, e2ee_default, allow_embedded_content, created_at, updated_at", //nolint:gosec // setClauses are hardcoded column names, argIdx is an integer — no injection risk
		strings.Join(setClauses, ", "), argIdx)

	err = h.db.QueryRow(updateQuery, args...).Scan(
		&server.Name, &server.IconURL, &server.BannerURL, &server.OwnerID, &server.E2EEDefault, &server.AllowEmbeddedContent, &server.CreatedAt, &server.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Server not found"})
		return
	} else if err != nil {
		h.log.Error("Failed to update server", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update server"})
		return
	}

	h.log.Info("Server updated", "server_id", serverID, "user_id", userID)

	// Broadcast update to server subscribers so members see changes in real time
	if serverUUID, err := uuid.Parse(serverID); err == nil {
		h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
			Type: "server_updated",
			Data: map[string]interface{}{
				"server_id":              serverID,
				"name":                   server.Name,
				"icon_url":               server.IconURL,
				"banner_url":             server.BannerURL,
				"allow_embedded_content": server.AllowEmbeddedContent,
			},
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"server": server,
		"role":   role,
	})
}

// DeleteServer deletes a server (owner only)
func (h *Handler) DeleteServer(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	// Validate server ID
	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid server ID"})
		return
	}

	// Check if user is owner
	var ownerID string
	ownerQuery := `
		SELECT owner_id FROM servers WHERE id = $1
	`

	err := h.db.QueryRow(ownerQuery, serverID).Scan(&ownerID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Server not found"})
		return
	} else if err != nil {
		h.log.Error("Failed to check ownership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete server"})
		return
	}

	if ownerID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the server owner can delete the server"})
		return
	}

	// Delete server (cascades to members and channels)
	deleteQuery := `DELETE FROM servers WHERE id = $1`

	_, err = h.db.Exec(deleteQuery, serverID)
	if err != nil {
		h.log.Error("Failed to delete server", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete server"})
		return
	}

	h.log.Info("Server deleted", "server_id", serverID, "user_id", userID)

	// Broadcast deletion to server subscribers so frontends can clean up
	if serverUUID, err := uuid.Parse(serverID); err == nil {
		h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
			Type: "server_deleted",
			Data: map[string]interface{}{
				"server_id": serverID,
			},
		})
	}

	c.JSON(http.StatusOK, gin.H{"message": "Server deleted successfully"})
}
