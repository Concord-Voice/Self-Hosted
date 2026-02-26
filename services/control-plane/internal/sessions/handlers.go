// Package sessions provides handlers for managing user authentication sessions.
package sessions

import (
	"database/sql"
	"net"
	"net/http"
	"strings"

	"github.com/concord/control-plane/internal/auth"
	"github.com/concord/control-plane/internal/models"
	"github.com/concord/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
)

// maskIPAddress masks the last octet of an IPv4 address (e.g., "192.168.1.x")
// or the last 80 bits of an IPv6 address (keeping the /48 prefix).
// This minimizes PII exposure in API responses while still being useful for session identification.
func maskIPAddress(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return "unknown"
	}

	if parsed.To4() != nil {
		// IPv4: mask last octet
		parts := strings.Split(ip, ".")
		if len(parts) == 4 {
			parts[3] = "x"
			return strings.Join(parts, ".")
		}
	}

	// IPv6: keep first 3 groups (/48), mask rest
	parts := strings.Split(ip, ":")
	if len(parts) > 3 {
		return strings.Join(parts[:3], ":") + "::x"
	}

	return "unknown"
}

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

	// Get current session's refresh token to identify it
	var currentTokenHash string
	currentToken, cookieErr := c.Cookie("refresh_token")
	if cookieErr == nil && currentToken != "" {
		currentTokenHash = auth.HashRefreshToken(currentToken)
	}

	// Fetch active sessions (not revoked, not expired)
	rows, err := h.db.Query(
		`SELECT id, token_hash, device_name, ip_address, user_agent, expires_at, created_at, last_used_at, remember_me
		 FROM refresh_tokens
		 WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > NOW()
		 ORDER BY last_used_at DESC`,
		userID,
	)
	if err != nil {
		h.log.Error("Failed to fetch sessions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch sessions"})
		return
	}
	defer func() { _ = rows.Close() }()

	activeSessions := []gin.H{}
	for rows.Next() {
		var session models.RefreshToken
		err := rows.Scan(
			&session.ID,
			&session.TokenHash,
			&session.DeviceName,
			&session.IPAddress,
			&session.UserAgent,
			&session.ExpiresAt,
			&session.CreatedAt,
			&session.LastUsedAt,
			&session.RememberMe,
		)
		if err != nil {
			h.log.Error("Failed to scan session", "error", err)
			continue
		}

		activeSessions = append(activeSessions, gin.H{
			"id":          session.ID,
			"device_name": session.DeviceName,
			"ip_address":  maskIPAddress(session.IPAddress),
			"user_agent":  session.UserAgent,
			"expires_at":  session.ExpiresAt,
			"created_at":  session.CreatedAt,
			"last_used":   session.LastUsedAt,
			"remember_me": session.RememberMe,
			"is_current":  currentTokenHash != "" && session.TokenHash == currentTokenHash,
		})
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating sessions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch sessions"})
		return
	}

	// Fetch past sessions (revoked within the last 30 days)
	pastRows, err := h.db.Query(
		`SELECT id, device_name, ip_address, user_agent, created_at, last_used_at, revoked_at
		 FROM refresh_tokens
		 WHERE user_id = $1 AND revoked_at IS NOT NULL AND revoked_at > NOW() - INTERVAL '30 days'
		 ORDER BY revoked_at DESC
		 LIMIT 10`,
		userID,
	)
	if err != nil {
		h.log.Error("Failed to fetch past sessions", "error", err)
		// Non-critical: return active sessions without past
		c.JSON(http.StatusOK, gin.H{
			"sessions":      activeSessions,
			"past_sessions": []gin.H{},
			"total":         len(activeSessions),
		})
		return
	}
	defer func() { _ = pastRows.Close() }()

	pastSessions := []gin.H{}
	for pastRows.Next() {
		var session models.RefreshToken
		err := pastRows.Scan(
			&session.ID,
			&session.DeviceName,
			&session.IPAddress,
			&session.UserAgent,
			&session.CreatedAt,
			&session.LastUsedAt,
			&session.RevokedAt,
		)
		if err != nil {
			h.log.Error("Failed to scan past session", "error", err)
			continue
		}

		pastSessions = append(pastSessions, gin.H{
			"id":          session.ID,
			"device_name": session.DeviceName,
			"ip_address":  maskIPAddress(session.IPAddress),
			"user_agent":  session.UserAgent,
			"created_at":  session.CreatedAt,
			"last_used":   session.LastUsedAt,
			"revoked_at":  session.RevokedAt,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"sessions":      activeSessions,
		"past_sessions": pastSessions,
		"total":         len(activeSessions),
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

	// Soft-delete: mark the session as revoked
	result, err := h.db.Exec(
		`UPDATE refresh_tokens SET revoked_at = NOW() WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`,
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

// RevokeAllSessions revokes all sessions after verifying the user's password.
// Requires a JSON body with "password" and optionally "include_current".
// Password re-verification prevents an attacker with a stolen session from
// mass-revoking the victim's sessions as a DoS.
func (h *Handler) RevokeAllSessions(c *gin.Context) {
	// Get user ID from auth middleware
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	// Parse request body for password
	var req struct {
		Password       string `json:"password"`       // #nosec G117 -- not a secret field, receives user input for auth verification
		IncludeCurrent bool   `json:"include_current"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Password == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Password is required to revoke all sessions"})
		return
	}

	// Verify password against stored hash
	var passwordHash string
	err := h.db.QueryRow(
		`SELECT password_hash FROM users WHERE id = $1`, userID,
	).Scan(&passwordHash)
	if err != nil {
		h.log.Error("Failed to fetch password hash for session revocation", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify password"})
		return
	}

	passwordMatch, verifyErr := auth.VerifyPassword(req.Password, passwordHash)
	if verifyErr != nil {
		h.log.Error("Password verification failed", "error", verifyErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify password"})
		return
	}
	if !passwordMatch {
		c.JSON(http.StatusForbidden, gin.H{"error": "Incorrect password"})
		return
	}

	// Get current session token
	currentToken, cookieErr := c.Cookie("refresh_token")
	if cookieErr != nil {
		currentToken = ""
	}

	var result sql.Result
	if currentToken != "" && !req.IncludeCurrent {
		currentTokenHash := auth.HashRefreshToken(currentToken)
		// Revoke all sessions except the current one
		result, err = h.db.Exec(
			`UPDATE refresh_tokens SET revoked_at = NOW()
			 WHERE user_id = $1 AND token_hash != $2 AND revoked_at IS NULL`,
			userID,
			currentTokenHash,
		)
	} else {
		// Revoke all sessions (including current)
		result, err = h.db.Exec(
			`UPDATE refresh_tokens SET revoked_at = NOW()
			 WHERE user_id = $1 AND revoked_at IS NULL`,
			userID,
		)
	}

	if err != nil {
		h.log.Error("Failed to revoke sessions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to revoke sessions"})
		return
	}

	rowsAffected, _ := result.RowsAffected()

	h.log.Info("Sessions revoked", "user_id", userID, "count", rowsAffected, "include_current", req.IncludeCurrent)

	message := "All other sessions revoked successfully"
	if req.IncludeCurrent {
		message = "All sessions revoked successfully"
		// Clear cookie since current session was also revoked
		c.SetCookie("refresh_token", "", -1, "/", "", c.Request.TLS != nil, true)
	}

	c.JSON(http.StatusOK, gin.H{
		"message":         message,
		"count":           rowsAffected,
		"include_current": req.IncludeCurrent,
	})
}
