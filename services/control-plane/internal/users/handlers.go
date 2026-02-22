// Package users provides handlers for user profile management and settings.
package users

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/concord/control-plane/internal/auth"
	"github.com/concord/control-plane/internal/models"
	"github.com/concord/control-plane/internal/websocket"
	"github.com/concord/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Handler handles user-related requests including profile management and settings.
type Handler struct {
	db  *sql.DB
	log *logger.Logger
	hub *websocket.Hub
}

// NewHandler creates a new user handler.
func NewHandler(db *sql.DB, log *logger.Logger, hub *websocket.Hub) *Handler {
	return &Handler{
		db:  db,
		log: log,
		hub: hub,
	}
}

// GetMe returns the current user's profile
func (h *Handler) GetMe(c *gin.Context) {
	// Get user ID from auth middleware
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	// Fetch user from database
	var user models.User
	err := h.db.QueryRow(
		`SELECT id, email, username, display_name, bio, avatar_url, COALESCE(links, '[]'::jsonb),
		        email_verified, age_verified, e2ee_preference, created_at, updated_at
		 FROM users WHERE id = $1`,
		userID,
	).Scan(
		&user.ID,
		&user.Email,
		&user.Username,
		&user.DisplayName,
		&user.Bio,
		&user.AvatarURL,
		&user.Links,
		&user.EmailVerified,
		&user.AgeVerified,
		&user.E2EEPreference,
		&user.CreatedAt,
		&user.UpdatedAt,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
			return
		}
		h.log.Error("Failed to fetch user", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch user"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"user": user.PublicUser(),
	})
}

// GetMyKeys returns the current user's E2EE keys (for key re-wrapping during password change)
func (h *Handler) GetMyKeys(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	var keys models.UserKeys
	err := h.db.QueryRow(
		`SELECT user_id, wrapped_private_key, key_derivation_salt, key_version
		 FROM user_keys WHERE user_id = $1`,
		userID,
	).Scan(&keys.UserID, &keys.WrappedPrivateKey, &keys.KeyDerivationSalt, &keys.KeyVersion)

	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "E2EE keys not found"})
			return
		}
		h.log.Error("Failed to fetch E2EE keys", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch encryption keys"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"e2ee_keys": gin.H{
			"wrapped_private_key": base64.StdEncoding.EncodeToString(keys.WrappedPrivateKey),
			"key_derivation_salt": base64.StdEncoding.EncodeToString(keys.KeyDerivationSalt),
			"key_version":         keys.KeyVersion,
		},
	})
}

// ReplaceKeysRequest represents a request to replace E2EE keys
type ReplaceKeysRequest struct {
	WrappedPrivateKey string `json:"wrapped_private_key" binding:"required"`
	KeyDerivationSalt string `json:"key_derivation_salt" binding:"required"`
}

// ReplaceMyKeys replaces the user's E2EE keys (for key recovery after corruption)
func (h *Handler) ReplaceMyKeys(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	var req ReplaceKeysRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "wrapped_private_key and key_derivation_salt are required"})
		return
	}

	wrappedKeyBytes, err := base64.StdEncoding.DecodeString(req.WrappedPrivateKey)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid wrapped_private_key format"})
		return
	}
	saltBytes, err := base64.StdEncoding.DecodeString(req.KeyDerivationSalt)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid key_derivation_salt format"})
		return
	}

	_, err = h.db.Exec(
		`UPDATE user_keys SET wrapped_private_key = $1, key_derivation_salt = $2,
		 key_version = key_version + 1, updated_at = NOW()
		 WHERE user_id = $3`,
		wrappedKeyBytes, saltBytes, userID,
	)
	if err != nil {
		h.log.Error("Failed to replace E2EE keys", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to replace keys"})
		return
	}

	h.log.Info("E2EE keys replaced", "user_id", userID)

	c.JSON(http.StatusOK, gin.H{
		"message": "Keys replaced successfully",
	})
}

// UpdateProfileRequest represents a request to update profile fields
type UpdateProfileRequest struct {
	Username    *string          `json:"username"`
	DisplayName *string          `json:"display_name"`
	Bio         *string          `json:"bio"`
	AvatarURL   *string          `json:"avatar_url"`
	Links       *json.RawMessage `json:"links"`
}

// UpdateMe updates the current user's profile
func (h *Handler) UpdateMe(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	var req UpdateProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	// Build dynamic update query
	setClauses := []string{}
	args := []interface{}{}
	argIdx := 1

	// Validate and add username if provided
	if req.Username != nil {
		trimmed := strings.TrimSpace(*req.Username)
		if err := auth.ValidateUsername(trimmed); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		normalized := auth.NormalizeUsername(trimmed)

		// Check uniqueness
		var count int
		err := h.db.QueryRow(
			`SELECT COUNT(*) FROM users WHERE username = $1 AND id != $2`,
			normalized, userID,
		).Scan(&count)
		if err != nil {
			h.log.Error("Failed to check username uniqueness", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update profile"})
			return
		}
		if count > 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Username is already taken"})
			return
		}

		setClauses = append(setClauses, fmt.Sprintf("username = $%d", argIdx))
		args = append(args, normalized)
		argIdx++
	}

	// Validate and add display_name if provided
	if req.DisplayName != nil {
		if len(*req.DisplayName) > 100 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Display name must be at most 100 characters"})
			return
		}
		var val interface{}
		trimmed := strings.TrimSpace(*req.DisplayName)
		if trimmed == "" {
			val = nil // Clear display name
		} else {
			val = trimmed
		}
		setClauses = append(setClauses, fmt.Sprintf("display_name = $%d", argIdx))
		args = append(args, val)
		argIdx++
	}

	// Validate and add bio if provided
	if req.Bio != nil {
		if len(*req.Bio) > 500 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Bio must be at most 500 characters"})
			return
		}
		var val interface{}
		trimmed := strings.TrimSpace(*req.Bio)
		if trimmed == "" {
			val = nil // Clear bio
		} else {
			val = trimmed
		}
		setClauses = append(setClauses, fmt.Sprintf("bio = $%d", argIdx))
		args = append(args, val)
		argIdx++
	}

	// Validate and add avatar_url if provided
	if req.AvatarURL != nil {
		if *req.AvatarURL != "" {
			if !strings.HasPrefix(*req.AvatarURL, "data:image/") {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Avatar must be a valid image data URL"})
				return
			}
			if len(*req.AvatarURL) > 500000 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Avatar data URL must be less than 500KB"})
				return
			}
			setClauses = append(setClauses, fmt.Sprintf("avatar_url = $%d", argIdx))
			args = append(args, *req.AvatarURL)
		} else {
			// Empty string means remove avatar
			setClauses = append(setClauses, fmt.Sprintf("avatar_url = $%d", argIdx))
			args = append(args, nil)
		}
		argIdx++
	}

	// Validate and add links if provided
	if req.Links != nil {
		var links []string
		if err := json.Unmarshal(*req.Links, &links); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Links must be an array of strings"})
			return
		}
		if len(links) > 5 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Maximum of 5 links allowed"})
			return
		}
		for _, link := range links {
			link = strings.TrimSpace(link)
			if link != "" && !strings.HasPrefix(link, "http://") && !strings.HasPrefix(link, "https://") {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Links must start with http:// or https://"})
				return
			}
			if len(link) > 500 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Each link must be at most 500 characters"})
				return
			}
		}
		setClauses = append(setClauses, fmt.Sprintf("links = $%d", argIdx))
		args = append(args, string(*req.Links))
		argIdx++
	}

	// Nothing to update
	if len(setClauses) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No fields to update"})
		return
	}

	// Always update updated_at
	setClauses = append(setClauses, "updated_at = NOW()")

	// Build and execute query
	// #nosec G201 -- Safe: setClauses contains only parameterized assignments (e.g., "email = $1")
	// User input is passed through args, not concatenated into SQL string
	query := fmt.Sprintf(
		`UPDATE users SET %s WHERE id = $%d
		 RETURNING id, email, username, display_name, bio, avatar_url, COALESCE(links, '[]'::jsonb),
		           email_verified, age_verified, e2ee_preference, created_at, updated_at`,
		strings.Join(setClauses, ", "), argIdx,
	)
	args = append(args, userID)

	var user models.User
	err := h.db.QueryRow(query, args...).Scan(
		&user.ID,
		&user.Email,
		&user.Username,
		&user.DisplayName,
		&user.Bio,
		&user.AvatarURL,
		&user.Links,
		&user.EmailVerified,
		&user.AgeVerified,
		&user.E2EEPreference,
		&user.CreatedAt,
		&user.UpdatedAt,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
			return
		}
		// Check for unique constraint violation (username)
		if strings.Contains(err.Error(), "users_username_key") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Username is already taken"})
			return
		}
		h.log.Error("Failed to update profile", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update profile"})
		return
	}

	h.log.Info("Profile updated", "user_id", userID)

	// Broadcast profile update to all connected clients in real time
	if h.hub != nil {
		h.hub.BroadcastToAll(websocket.OutgoingMessage{
			Type: "profile_updated",
			Data: map[string]interface{}{
				"user_id":      user.ID,
				"username":     user.Username,
				"display_name": user.DisplayName,
				"avatar_url":   user.AvatarURL,
			},
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"user": user.PublicUser(),
	})
}

// ChangePasswordRequest represents a request to change password
type ChangePasswordRequest struct {
	CurrentPassword     string `json:"current_password" binding:"required"`
	NewPassword         string `json:"new_password" binding:"required"`
	WrappedPrivateKey   string `json:"wrapped_private_key" binding:"required"`
	KeyDerivationSalt   string `json:"key_derivation_salt" binding:"required"`
}

// ChangePassword changes the current user's password and re-wraps E2EE keys
func (h *Handler) ChangePassword(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	var req ChangePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Current password, new password, and re-wrapped keys are required"})
		return
	}

	// Validate base64 encoding of keys
	wrappedKeyBytes, err := base64.StdEncoding.DecodeString(req.WrappedPrivateKey)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid wrapped_private_key format"})
		return
	}
	saltBytes, err := base64.StdEncoding.DecodeString(req.KeyDerivationSalt)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid key_derivation_salt format"})
		return
	}

	// Fetch current password hash
	var passwordHash string
	err = h.db.QueryRow(
		`SELECT password_hash FROM users WHERE id = $1`,
		userID,
	).Scan(&passwordHash)

	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
			return
		}
		h.log.Error("Failed to fetch password hash", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to change password"})
		return
	}

	// Verify current password
	match, err := auth.VerifyPassword(req.CurrentPassword, passwordHash)
	if err != nil {
		h.log.Error("Failed to verify password", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to change password"})
		return
	}
	if !match {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Current password is incorrect"})
		return
	}

	// Validate new password strength
	if err := auth.ValidatePasswordStrength(req.NewPassword); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Check new password is different from current
	sameAsOld, err := auth.VerifyPassword(req.NewPassword, passwordHash)
	if err != nil {
		h.log.Error("Failed to compare passwords", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to change password"})
		return
	}
	if sameAsOld {
		c.JSON(http.StatusBadRequest, gin.H{"error": "New password must be different from current password"})
		return
	}

	// Hash new password
	newHash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		h.log.Error("Failed to hash new password", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to change password"})
		return
	}

	// Begin transaction to update both password and E2EE keys atomically
	tx, err := h.db.Begin()
	if err != nil {
		h.log.Error("Failed to begin transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to change password"})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback transaction", "error", rbErr)
		}
	}()

	// Update password
	_, err = tx.Exec(
		`UPDATE users SET password_hash = $1, updated_at = NOW() WHERE id = $2`,
		newHash, userID,
	)
	if err != nil {
		h.log.Error("Failed to update password", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to change password"})
		return
	}

	// Update E2EE keys (re-wrapped with new password)
	_, err = tx.Exec(
		`UPDATE user_keys SET wrapped_private_key = $1, key_derivation_salt = $2, updated_at = NOW()
		 WHERE user_id = $3`,
		wrappedKeyBytes, saltBytes, userID,
	)
	if err != nil {
		h.log.Error("Failed to update E2EE keys", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to change password"})
		return
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to change password"})
		return
	}

	h.log.Info("Password changed with key re-wrap", "user_id", userID)

	c.JSON(http.StatusOK, gin.H{
		"message": "Password changed successfully",
	})
}

// GetPublicKey returns a user's latest public key (for E2EE key wrapping).
func (h *Handler) GetPublicKey(c *gin.Context) {
	targetUserID := c.Param("user_id")

	var publicKey []byte
	var keyVersion int
	err := h.db.QueryRow(
		`SELECT public_key, key_version FROM public_keys
		 WHERE user_id = $1 ORDER BY key_version DESC LIMIT 1`,
		targetUserID,
	).Scan(&publicKey, &keyVersion)

	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "Public key not found"})
			return
		}
		h.log.Error("Failed to fetch public key", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch public key"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"user_id":     targetUserID,
		"public_key":  base64.StdEncoding.EncodeToString(publicKey),
		"key_version": keyVersion,
	})
}

// GetPreferences returns the current user's encrypted preferences blob.
// Returns { preferences: null } when no preferences exist (first login).
func (h *Handler) GetPreferences(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	var encryptedData string
	var version int
	var updatedAt string
	err := h.db.QueryRow(
		`SELECT encrypted_data, version, updated_at FROM user_preferences WHERE user_id = $1`,
		userID,
	).Scan(&encryptedData, &version, &updatedAt)

	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusOK, gin.H{"preferences": nil})
			return
		}
		h.log.Error("Failed to fetch preferences", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch preferences"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"preferences": gin.H{
			"encrypted_data": encryptedData,
			"version":        version,
			"updated_at":     updatedAt,
		},
	})
}

// UpdatePreferencesRequest represents a request to update encrypted preferences
type UpdatePreferencesRequest struct {
	EncryptedData string `json:"encrypted_data" binding:"required"`
}

// UpdatePreferences upserts the current user's encrypted preferences blob.
// After storing, broadcasts a notification to the user's other connected clients.
func (h *Handler) UpdatePreferences(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	var req UpdatePreferencesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "encrypted_data is required"})
		return
	}

	// Validate base64 format
	if _, err := base64.StdEncoding.DecodeString(req.EncryptedData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "encrypted_data must be valid base64"})
		return
	}

	// Size limit: 64KB of base64 text
	if len(req.EncryptedData) > 65536 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "encrypted_data exceeds 64KB limit"})
		return
	}

	// Upsert preferences
	var version int
	err := h.db.QueryRow(
		`INSERT INTO user_preferences (user_id, encrypted_data, version, updated_at)
		 VALUES ($1, $2, 1, NOW())
		 ON CONFLICT (user_id) DO UPDATE
		 SET encrypted_data = $2, version = user_preferences.version + 1, updated_at = NOW()
		 RETURNING version`,
		userID, req.EncryptedData,
	).Scan(&version)

	if err != nil {
		h.log.Error("Failed to update preferences", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update preferences"})
		return
	}

	// Broadcast to user's other connected clients so they can pull the update
	if h.hub != nil {
		if uid, ok := userID.(string); ok {
			if parsed, err := uuid.Parse(uid); err == nil {
				h.hub.BroadcastToUser(parsed, websocket.OutgoingMessage{
					Type: "preferences_updated",
					Data: map[string]interface{}{
						"version": version,
					},
				})
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"version": version,
	})
}
