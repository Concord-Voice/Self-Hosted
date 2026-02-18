// Package sessions provides handlers for managing user authentication sessions.
package sessions

import (
	"database/sql"
	"net/http"

	"github.com/concord/control-plane/internal/auth"
	"github.com/concord/control-plane/internal/models"
	"github.com/concord/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
)

// Handler handles session-related requests for managing user authentication sessions.
type Handler struct {
	db  *sql.DB
	log *logger.Logger
}

// NewHandler creates a new session handler.
func NewHandler(db *sql.DB, log *logger.Logger) *Handler {
	return &Handler{
		db:  db,
		log: log,
	}
}

// ListSessions returns all active sessions for the current user
func (h *Handler) ListSessions(c *gin.Context) {
	// Get user ID from auth middleware
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	// Fetch all active sessions
	rows, err := h.db.Query(
		`SELECT id, device_name, ip_address, user_agent, expires_at, created_at, last_used_at
		 FROM refresh_tokens
		 WHERE user_id = $1 AND expires_at > NOW()
		 ORDER BY last_used_at DESC`,
		userID,
	)
	if err != nil {
		h.log.Error("Failed to fetch sessions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch sessions"})
		return
	}
	defer func() { _ = rows.Close() }()

	sessions := []gin.H{}
	for rows.Next() {
		var session models.RefreshToken
		err := rows.Scan(
			&session.ID,
			&session.DeviceName,
			&session.IPAddress,
			&session.UserAgent,
			&session.ExpiresAt,
			&session.CreatedAt,
			&session.LastUsedAt,
		)
		if err != nil {
			h.log.Error("Failed to scan session", "error", err)
			continue
		}

		sessionData := gin.H{
			"id":          session.ID,
			"device_name": session.DeviceName,
			"ip_address":  session.IPAddress,
			"user_agent":  session.UserAgent,
			"expires_at":  session.ExpiresAt,
			"created_at":  session.CreatedAt,
			"last_used":   session.LastUsedAt,
			"is_current":  false, // Will update if this is current session
		}

		sessions = append(sessions, sessionData)
	}

	c.JSON(http.StatusOK, gin.H{
		"sessions": sessions,
		"total":    len(sessions),
	})
}

// RevokeSession revokes a specific session by ID
func (h *Handler) RevokeSession(c *gin.Context) {
	// Get user ID from auth middleware
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	sessionID := c.Param("id")
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Session ID is required"})
		return
	}

	// Delete the session (only if it belongs to the user)
	result, err := h.db.Exec(
		`DELETE FROM refresh_tokens WHERE id = $1 AND user_id = $2`,
		sessionID,
		userID,
	)
	if err != nil {
		h.log.Error("Failed to revoke session", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to revoke session"})
		return
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Session not found"})
		return
	}

	h.log.Info("Session revoked", "user_id", userID, "session_id", sessionID)

	c.JSON(http.StatusOK, gin.H{
		"message":    "Session revoked successfully",
		"session_id": sessionID,
	})
}

// RevokeAllSessions revokes all sessions except the current one
func (h *Handler) RevokeAllSessions(c *gin.Context) {
	// Get user ID from auth middleware
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	// Get current session token to preserve it
	currentToken, err := c.Cookie("refresh_token")
	if err != nil {
		// If no current token, revoke all sessions
		currentToken = ""
	}

	var result sql.Result
	if currentToken != "" {
		currentTokenHash := auth.HashRefreshToken(currentToken)
		// Delete all sessions except the current one
		result, err = h.db.Exec(
			`DELETE FROM refresh_tokens WHERE user_id = $1 AND token_hash != $2`,
			userID,
			currentTokenHash,
		)
	} else {
		// Delete all sessions
		result, err = h.db.Exec(
			`DELETE FROM refresh_tokens WHERE user_id = $1`,
			userID,
		)
	}

	if err != nil {
		h.log.Error("Failed to revoke sessions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to revoke sessions"})
		return
	}

	rowsAffected, _ := result.RowsAffected()

	h.log.Info("Sessions revoked", "user_id", userID, "count", rowsAffected)

	c.JSON(http.StatusOK, gin.H{
		"message": "All other sessions revoked successfully",
		"count":   rowsAffected,
	})
}
