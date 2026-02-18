// Package channels provides handlers for managing server channels.
package channels

import (
	"database/sql"
	"net/http"

	"github.com/concord/control-plane/internal/models"
	"github.com/concord/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Handler handles channel-related requests
type Handler struct {
	db  *sql.DB
	log *logger.Logger
}

// NewHandler creates a new channel handler
func NewHandler(db *sql.DB, log *logger.Logger) *Handler {
	return &Handler{
		db:  db,
		log: log,
	}
}

// CreateChannelRequest represents a request to create a channel
type CreateChannelRequest struct {
	ServerID string  `json:"server_id" binding:"required,uuid"`
	Name     string  `json:"name" binding:"required,min=3,max=100"`
	Type     string  `json:"type" binding:"required,oneof=text voice bulletin"`
	Emoji    *string `json:"emoji,omitempty"` // Optional custom emoji
}

// UpdateChannelRequest represents a request to update a channel
type UpdateChannelRequest struct {
	Name string `json:"name" binding:"required,min=3,max=100"`
	Type string `json:"type" binding:"required,oneof=text voice bulletin"`
}

// ListChannels returns all channels in a server
func (h *Handler) ListChannels(c *gin.Context) {
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch channels"})
		return
	}

	if !isMember {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this server"})
		return
	}

	// Get all channels in the server
	query := `
		SELECT id, server_id, name, type, created_at, updated_at
		FROM channels
		WHERE server_id = $1
		ORDER BY created_at ASC
	`

	rows, err := h.db.Query(query, serverID)
	if err != nil {
		h.log.Error("Failed to query channels", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch channels"})
		return
	}
	defer func() { _ = rows.Close() }()

	channels := []models.Channel{}
	for rows.Next() {
		var channel models.Channel
		err := rows.Scan(
			&channel.ID,
			&channel.ServerID,
			&channel.Name,
			&channel.Type,
			&channel.CreatedAt,
			&channel.UpdatedAt,
		)
		if err != nil {
			h.log.Error("Failed to scan channel", "error", err)
			continue
		}
		channels = append(channels, channel)
	}

	c.JSON(http.StatusOK, gin.H{"channels": channels})
}

// CreateChannel creates a new channel in a server
func (h *Handler) CreateChannel(c *gin.Context) {
	userID := c.GetString("user_id")

	var req CreateChannelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Check if user is owner or admin of the server
	var role string
	roleQuery := `
		SELECT role FROM server_members
		WHERE server_id = $1 AND user_id = $2
	`

	err := h.db.QueryRow(roleQuery, req.ServerID, userID).Scan(&role)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this server"})
		return
	} else if err != nil {
		h.log.Error("Failed to check membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create channel"})
		return
	}

	if role != "owner" && role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only owners and admins can create channels"})
		return
	}

	// Create channel
	channelID := uuid.New().String()
	insertQuery := `
		INSERT INTO channels (id, server_id, name, type, emoji, position, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW(), NOW())
		RETURNING created_at, updated_at
	`

	var channel models.Channel
	channel.ID = channelID
	channel.ServerID = req.ServerID
	channel.Name = req.Name
	channel.Type = req.Type
	channel.Emoji = req.Emoji
	channel.Position = 0 // Default position, can be reordered later

	err = h.db.QueryRow(insertQuery, channelID, req.ServerID, req.Name, req.Type, req.Emoji, 0).Scan(
		&channel.CreatedAt,
		&channel.UpdatedAt,
	)
	if err != nil {
		h.log.Error("Failed to create channel", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create channel"})
		return
	}

	h.log.Info("Channel created", "channel_id", channelID, "server_id", req.ServerID, "user_id", userID)

	c.JSON(http.StatusCreated, gin.H{"channel": channel})
}

// GetChannel returns a specific channel
func (h *Handler) GetChannel(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	// Validate channel ID
	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid channel ID"})
		return
	}

	// Get channel and check if user is a member of the server
	query := `
		SELECT c.id, c.server_id, c.name, c.type, c.created_at, c.updated_at
		FROM channels c
		INNER JOIN server_members sm ON c.server_id = sm.server_id
		WHERE c.id = $1 AND sm.user_id = $2
	`

	var channel models.Channel
	err := h.db.QueryRow(query, channelID, userID).Scan(
		&channel.ID,
		&channel.ServerID,
		&channel.Name,
		&channel.Type,
		&channel.CreatedAt,
		&channel.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Channel not found or access denied"})
		return
	} else if err != nil {
		h.log.Error("Failed to fetch channel", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch channel"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"channel": channel})
}

// UpdateChannel updates a channel's details
func (h *Handler) UpdateChannel(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	// Validate channel ID
	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid channel ID"})
		return
	}

	var req UpdateChannelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Get channel's server ID and check user's role
	var serverID string
	var role string
	roleQuery := `
		SELECT c.server_id, sm.role
		FROM channels c
		INNER JOIN server_members sm ON c.server_id = sm.server_id
		WHERE c.id = $1 AND sm.user_id = $2
	`

	err := h.db.QueryRow(roleQuery, channelID, userID).Scan(&serverID, &role)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Channel not found or access denied"})
		return
	} else if err != nil {
		h.log.Error("Failed to check permissions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update channel"})
		return
	}

	if role != "owner" && role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only owners and admins can update channels"})
		return
	}

	// Update channel
	updateQuery := `
		UPDATE channels
		SET name = $1, type = $2, updated_at = NOW()
		WHERE id = $3
		RETURNING server_id, created_at, updated_at
	`

	var channel models.Channel
	channel.ID = channelID
	channel.Name = req.Name
	channel.Type = req.Type

	err = h.db.QueryRow(updateQuery, req.Name, req.Type, channelID).Scan(
		&channel.ServerID,
		&channel.CreatedAt,
		&channel.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Channel not found"})
		return
	} else if err != nil {
		h.log.Error("Failed to update channel", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update channel"})
		return
	}

	h.log.Info("Channel updated", "channel_id", channelID, "user_id", userID)

	c.JSON(http.StatusOK, gin.H{"channel": channel})
}

// DeleteChannel deletes a channel (owner/admin only)
func (h *Handler) DeleteChannel(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	// Validate channel ID
	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid channel ID"})
		return
	}

	// Get channel's server ID and check user's role
	var role string
	roleQuery := `
		SELECT sm.role
		FROM channels c
		INNER JOIN server_members sm ON c.server_id = sm.server_id
		WHERE c.id = $1 AND sm.user_id = $2
	`

	err := h.db.QueryRow(roleQuery, channelID, userID).Scan(&role)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Channel not found or access denied"})
		return
	} else if err != nil {
		h.log.Error("Failed to check permissions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete channel"})
		return
	}

	if role != "owner" && role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only owners and admins can delete channels"})
		return
	}

	// Delete channel
	deleteQuery := `DELETE FROM channels WHERE id = $1`

	_, err = h.db.Exec(deleteQuery, channelID)
	if err != nil {
		h.log.Error("Failed to delete channel", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete channel"})
		return
	}

	h.log.Info("Channel deleted", "channel_id", channelID, "user_id", userID)

	c.JSON(http.StatusOK, gin.H{"message": "Channel deleted successfully"})
}
