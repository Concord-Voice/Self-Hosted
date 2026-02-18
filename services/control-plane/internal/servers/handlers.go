// Package servers provides handlers for managing Concord servers (community-style communities).
package servers

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/concord/control-plane/internal/models"
	"github.com/concord/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Handler handles server-related requests
type Handler struct {
	db  *sql.DB
	log *logger.Logger
}

// NewHandler creates a new server handler
func NewHandler(db *sql.DB, log *logger.Logger) *Handler {
	return &Handler{
		db:  db,
		log: log,
	}
}

// CreateServerRequest represents a request to create a server
type CreateServerRequest struct {
	Name    string  `json:"name" binding:"required,min=3,max=100"`
	IconURL *string `json:"icon_url"`
}

// UpdateServerRequest represents a request to update a server
type UpdateServerRequest struct {
	Name    string  `json:"name" binding:"required,min=3,max=100"`
	IconURL *string `json:"icon_url"`
}

// ListServers returns all servers the user is a member of
func (h *Handler) ListServers(c *gin.Context) {
	userID := c.GetString("user_id")

	query := `
		SELECT s.id, s.name, s.icon_url, s.owner_id, s.created_at, s.updated_at, sm.role
		FROM servers s
		INNER JOIN server_members sm ON s.id = sm.server_id
		WHERE sm.user_id = $1
		ORDER BY s.created_at DESC
	`

	rows, err := h.db.Query(query, userID)
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
			&server.OwnerID,
			&server.CreatedAt,
			&server.UpdatedAt,
			&server.Role,
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
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
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

	// Create server
	serverID := uuid.New().String()
	serverQuery := `
		INSERT INTO servers (id, name, icon_url, owner_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, NOW(), NOW())
		RETURNING created_at, updated_at
	`

	var server models.Server
	server.ID = serverID
	server.Name = req.Name
	server.IconURL = req.IconURL
	server.OwnerID = userID

	err = tx.QueryRow(serverQuery, serverID, req.Name, req.IconURL, userID).Scan(
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

	// Commit transaction
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
		SELECT id, name, icon_url, owner_id, created_at, updated_at
		FROM servers
		WHERE id = $1
	`

	err = h.db.QueryRow(serverQuery, serverID).Scan(
		&server.ID,
		&server.Name,
		&server.IconURL,
		&server.OwnerID,
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
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Check if user is owner or admin
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
		h.log.Error("Failed to check membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update server"})
		return
	}

	if role != "owner" && role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only owners and admins can update server details"})
		return
	}

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

	// Update server
	updateQuery := `
		UPDATE servers
		SET name = $1, icon_url = $2, updated_at = NOW()
		WHERE id = $3
		RETURNING name, icon_url, owner_id, created_at, updated_at
	`

	var server models.Server
	server.ID = serverID

	err = h.db.QueryRow(updateQuery, req.Name, req.IconURL, serverID).Scan(
		&server.Name,
		&server.IconURL,
		&server.OwnerID,
		&server.CreatedAt,
		&server.UpdatedAt,
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

	c.JSON(http.StatusOK, gin.H{"message": "Server deleted successfully"})
}
