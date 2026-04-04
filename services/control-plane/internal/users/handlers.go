// Package users provides handlers for user profile management and settings.
package users

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/media"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/models"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

const (
	errMsgUserNotFound         = "User not found"
	errMsgUnauthorized         = "Unauthorized"
	errMsgFailedUpdateProfile  = "Failed to update profile"
	errMsgFailedChangePassword = "Failed to change password"
	dataImagePrefix            = "data:image/"
)

// MFAVerifier checks MFA status and verifies codes for sensitive operations.
type MFAVerifier interface {
	IsEnabled(ctx context.Context, userID string) bool
	VerifyCode(ctx context.Context, userID string, code string) (bool, error)
	GetEnabledMethods(ctx context.Context, userID string) ([]string, error)
}

// Handler handles user-related requests including profile management and settings.
type Handler struct {
	db          *sql.DB
	log         *logger.Logger
	hub         *websocket.Hub
	mfaVerifier MFAVerifier
	store       media.ObjectDeleter // nil when object storage is not configured
}

// NewHandler creates a new user handler.
func NewHandler(db *sql.DB, log *logger.Logger, hub *websocket.Hub, mfaVerifier MFAVerifier) *Handler {
	return &Handler{
		db:          db,
		log:         log,
		hub:         hub,
		mfaVerifier: mfaVerifier,
	}
}

// SetMediaStore configures optional object storage for media cleanup on profile image removal.
func (h *Handler) SetMediaStore(store media.ObjectDeleter) {
	h.store = store
}

// GetMe returns the current user's profile
func (h *Handler) GetMe(c *gin.Context) {
	// Get user ID from auth middleware
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
		return
	}

	// Fetch user from database
	var user models.User
	err := h.db.QueryRow(
		`SELECT id, email, username, display_name, bio, avatar_url, header_image_url, color_scheme,
		        COALESCE(links, '[]'::jsonb),
		        email_verified, age_verified, e2ee_preference, created_at, updated_at,
		        username_changed_at
		 FROM users WHERE id = $1`,
		userID,
	).Scan(
		&user.ID,
		&user.Email,
		&user.Username,
		&user.DisplayName,
		&user.Bio,
		&user.AvatarURL,
		&user.HeaderImageURL,
		&user.ColorScheme,
		&user.Links,
		&user.EmailVerified,
		&user.AgeVerified,
		&user.E2EEPreference,
		&user.CreatedAt,
		&user.UpdatedAt,
		&user.UsernameChangedAt,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgUserNotFound})
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

// GetPublicProfile returns another user's public profile (no email or private fields)
func (h *Handler) GetPublicProfile(c *gin.Context) {
	// Ensure the requester is authenticated
	_, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
		return
	}

	targetUserID := c.Param("user_id")

	var user models.User
	err := h.db.QueryRow(
		`SELECT id, email, username, display_name, bio, avatar_url, header_image_url, color_scheme,
		        COALESCE(links, '[]'::jsonb),
		        email_verified, age_verified, e2ee_preference, created_at, updated_at
		 FROM users WHERE id = $1`,
		targetUserID,
	).Scan(
		&user.ID,
		&user.Email,
		&user.Username,
		&user.DisplayName,
		&user.Bio,
		&user.AvatarURL,
		&user.HeaderImageURL,
		&user.ColorScheme,
		&user.Links,
		&user.EmailVerified,
		&user.AgeVerified,
		&user.E2EEPreference,
		&user.CreatedAt,
		&user.UpdatedAt,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgUserNotFound})
			return
		}
		h.log.Error("Failed to fetch user profile", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch user profile"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"user": user.ProfileForOthers(),
	})
}

// GetMyKeys returns the current user's E2EE keys (for key re-wrapping during password change)
func (h *Handler) GetMyKeys(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
		return
	}

	var keys models.UserKeys
	err := h.db.QueryRow(
		`SELECT user_id, wrapped_private_key, key_derivation_salt, key_version, key_derivation_alg
		 FROM user_keys WHERE user_id = $1`,
		userID,
	).Scan(&keys.UserID, &keys.WrappedPrivateKey, &keys.KeyDerivationSalt, &keys.KeyVersion, &keys.KeyDerivationAlg)

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
			"key_derivation_alg":  keys.KeyDerivationAlg,
		},
	})
}

// ReplaceKeysRequest represents a request to replace E2EE keys
type ReplaceKeysRequest struct {
	WrappedPrivateKey string `json:"wrapped_private_key" binding:"required"`
	KeyDerivationSalt string `json:"key_derivation_salt" binding:"required"`
	KeyDerivationAlg  string `json:"key_derivation_alg"` // "pbkdf2" or "argon2id"
}

// ReplaceMyKeys replaces the user's E2EE keys (for key recovery after corruption)
func (h *Handler) ReplaceMyKeys(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
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

	kdAlg := req.KeyDerivationAlg
	if kdAlg == "" {
		kdAlg = "argon2id"
	}

	_, err = h.db.Exec(
		`UPDATE user_keys SET wrapped_private_key = $1, key_derivation_salt = $2,
		 key_version = key_version + 1, key_derivation_alg = $3, updated_at = NOW()
		 WHERE user_id = $4`,
		wrappedKeyBytes, saltBytes, kdAlg, userID,
	)
	if err != nil {
		h.log.Error("Failed to replace E2EE keys", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to replace keys"})
		return
	}

	h.log.Info("E2EE keys replaced", "user_id", userID, "alg", kdAlg)

	c.JSON(http.StatusOK, gin.H{
		"message": "Keys replaced successfully",
	})
}

// UpdateProfileRequest represents a request to update profile fields
type UpdateProfileRequest struct {
	Username       *string          `json:"username"`
	DisplayName    *string          `json:"display_name"`
	Bio            *string          `json:"bio"`
	AvatarURL      *string          `json:"avatar_url"`
	HeaderImageURL *string          `json:"header_image_url"`
	ColorScheme    *string          `json:"color_scheme"`
	Links          *json.RawMessage `json:"links"`
}

// profileUpdateBuilder accumulates SET clauses, arguments, and media keys for
// the dynamic UPDATE query in UpdateMe. Extracted to reduce cognitive complexity.
type profileUpdateBuilder struct {
	setClauses        []string
	args              []interface{}
	argIdx            int
	mediaKeysToDelete []string
	usernameChanged   bool
	previousUsername  string
	extraResponse     map[string]interface{} // additional fields to include in error responses
}

func newProfileUpdateBuilder() *profileUpdateBuilder {
	return &profileUpdateBuilder{argIdx: 1}
}

// addClause appends a parameterized SET clause and its argument.
func (b *profileUpdateBuilder) addClause(column string, val interface{}) {
	b.setClauses = append(b.setClauses, fmt.Sprintf("%s = $%d", column, b.argIdx))
	b.args = append(b.args, val)
	b.argIdx++
}

// validateUsername checks cooldown, uniqueness, and appends the username clause.
// Returns an HTTP status + error message on failure, or 0/"" on success.
func (h *Handler) validateUsername(b *profileUpdateBuilder, username string, userID interface{}) (int, string) {
	trimmed := strings.TrimSpace(username)
	if err := auth.ValidateUsername(trimmed); err != nil {
		return http.StatusBadRequest, err.Error()
	}
	normalized := auth.NormalizeUsername(trimmed)

	var currentUsername string
	var usernameChangedAt time.Time
	err := h.db.QueryRow(
		`SELECT username, username_changed_at FROM users WHERE id = $1`,
		userID,
	).Scan(&currentUsername, &usernameChangedAt)
	if err == sql.ErrNoRows {
		return http.StatusNotFound, errMsgUserNotFound
	}
	if err != nil {
		h.log.Error("Failed to check username cooldown", "error", err)
		return http.StatusInternalServerError, errMsgFailedUpdateProfile
	}

	if normalized == currentUsername {
		return 0, "" // no-op
	}

	cooldownEnd := usernameChangedAt.AddDate(0, 0, 365)
	if time.Now().Before(cooldownEnd) {
		b.extraResponse = map[string]interface{}{
			"username_change_eligible_at": cooldownEnd,
		}
		return http.StatusForbidden, "You can only change your username once per year"
	}

	var count int
	err = h.db.QueryRow(
		`SELECT COUNT(*) FROM users WHERE username = $1 AND id != $2`,
		normalized, userID,
	).Scan(&count)
	if err != nil {
		h.log.Error("Failed to check username uniqueness", "error", err)
		return http.StatusInternalServerError, errMsgFailedUpdateProfile
	}
	if count > 0 {
		return http.StatusBadRequest, "Username is already taken"
	}

	b.addClause("username", normalized)
	b.setClauses = append(b.setClauses, "username_changed_at = NOW()")
	b.previousUsername = currentUsername
	b.usernameChanged = true
	return 0, ""
}

// validateDisplayName validates and appends the display_name clause.
func validateDisplayName(b *profileUpdateBuilder, displayName string) (int, string) {
	if len(displayName) > 100 {
		return http.StatusBadRequest, "Display name must be at most 100 characters"
	}
	trimmed := strings.TrimSpace(displayName)
	var val interface{} = trimmed
	if trimmed == "" {
		val = nil
	}
	b.addClause("display_name", val)
	return 0, ""
}

// validateBio validates and appends the bio clause.
func validateBio(b *profileUpdateBuilder, bio string) (int, string) {
	if len(bio) > 500 {
		return http.StatusBadRequest, "Bio must be at most 500 characters"
	}
	trimmed := strings.TrimSpace(bio)
	var val interface{} = trimmed
	if trimmed == "" {
		val = nil
	}
	b.addClause("bio", val)
	return 0, ""
}

// validateAvatarURL validates and appends the avatar_url clause.
func validateAvatarURL(b *profileUpdateBuilder, avatarURL string, userID interface{}) (int, string) {
	if avatarURL == "" {
		b.addClause("avatar_url", nil)
		b.mediaKeysToDelete = append(b.mediaKeysToDelete, fmt.Sprintf("avatars/%s", userID))
		return 0, ""
	}
	expectedAvatarURL := fmt.Sprintf("/api/v1/media/avatars/%s", userID)
	if avatarURL != expectedAvatarURL && !strings.HasPrefix(avatarURL, dataImagePrefix) {
		return http.StatusBadRequest, "Avatar must be an uploaded avatar URL for this user or an image data URL"
	}
	if strings.HasPrefix(avatarURL, dataImagePrefix) && len(avatarURL) > 1500000 {
		return http.StatusBadRequest, "Avatar image too large (max ~1MB)"
	}
	b.addClause("avatar_url", avatarURL)
	return 0, ""
}

// validateHeaderImageURL validates and appends the header_image_url clause.
func validateHeaderImageURL(b *profileUpdateBuilder, headerImageURL string, userID interface{}) (int, string) {
	if headerImageURL == "" {
		b.addClause("header_image_url", nil)
		b.mediaKeysToDelete = append(b.mediaKeysToDelete, fmt.Sprintf("banners/%s", userID))
		return 0, ""
	}
	expectedBannerURL := fmt.Sprintf("/api/v1/media/banners/%s", userID)
	if headerImageURL != expectedBannerURL && !strings.HasPrefix(headerImageURL, dataImagePrefix) {
		return http.StatusBadRequest, "Header image must be an uploaded banner URL for this user or an image data URL"
	}
	if strings.HasPrefix(headerImageURL, dataImagePrefix) && len(headerImageURL) > 3000000 {
		return http.StatusBadRequest, "Banner image too large (max ~2MB)"
	}
	b.addClause("header_image_url", headerImageURL)
	return 0, ""
}

// validateLinks validates and appends the links clause.
func validateLinks(b *profileUpdateBuilder, raw json.RawMessage) (int, string) {
	var links []string
	if err := json.Unmarshal(raw, &links); err != nil {
		return http.StatusBadRequest, "Links must be an array of strings"
	}
	if len(links) > 5 {
		return http.StatusBadRequest, "Maximum of 5 links allowed"
	}
	for _, link := range links {
		link = strings.TrimSpace(link)
		if link != "" && !strings.HasPrefix(link, "http://") && !strings.HasPrefix(link, "https://") {
			return http.StatusBadRequest, "Links must start with http:// or https://"
		}
		if len(link) > 500 {
			return http.StatusBadRequest, "Each link must be at most 500 characters"
		}
	}
	b.addClause("links", string(raw))
	return 0, ""
}

// validateColorScheme validates and appends the color_scheme clause.
func validateColorScheme(b *profileUpdateBuilder, colorScheme string) (int, string) {
	trimmed := strings.TrimSpace(colorScheme)
	if trimmed == "" {
		b.addClause("color_scheme", nil)
		return 0, ""
	}
	if len(trimmed) > 200 {
		return http.StatusBadRequest, "color_scheme must be at most 200 characters"
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return http.StatusBadRequest, "color_scheme must be valid JSON"
	}
	b.addClause("color_scheme", trimmed)
	return 0, ""
}

// profileFieldValidator is a validation function that populates the builder
// and returns an HTTP status + error message on failure, or 0/"" on success.
type profileFieldValidator func(b *profileUpdateBuilder) (int, string)

// buildProfileFields validates all provided profile fields and populates the builder.
// Returns an HTTP status + error message on the first validation failure, or 0/"" on success.
func (h *Handler) buildProfileFields(b *profileUpdateBuilder, req *UpdateProfileRequest, userID interface{}) (int, string) {
	validators := h.collectProfileValidators(req, userID)
	for _, v := range validators {
		if status, msg := v(b); status != 0 {
			return status, msg
		}
	}
	return 0, ""
}

// collectProfileValidators returns a validator for each non-nil field in the request.
func (h *Handler) collectProfileValidators(req *UpdateProfileRequest, userID interface{}) []profileFieldValidator {
	var validators []profileFieldValidator
	if req.Username != nil {
		val := *req.Username
		validators = append(validators, func(b *profileUpdateBuilder) (int, string) {
			return h.validateUsername(b, val, userID)
		})
	}
	if req.DisplayName != nil {
		val := *req.DisplayName
		validators = append(validators, func(b *profileUpdateBuilder) (int, string) {
			return validateDisplayName(b, val)
		})
	}
	if req.Bio != nil {
		val := *req.Bio
		validators = append(validators, func(b *profileUpdateBuilder) (int, string) {
			return validateBio(b, val)
		})
	}
	if req.AvatarURL != nil {
		val := *req.AvatarURL
		validators = append(validators, func(b *profileUpdateBuilder) (int, string) {
			return validateAvatarURL(b, val, userID)
		})
	}
	if req.HeaderImageURL != nil {
		val := *req.HeaderImageURL
		validators = append(validators, func(b *profileUpdateBuilder) (int, string) {
			return validateHeaderImageURL(b, val, userID)
		})
	}
	if req.Links != nil {
		val := *req.Links
		validators = append(validators, func(b *profileUpdateBuilder) (int, string) {
			return validateLinks(b, val)
		})
	}
	if req.ColorScheme != nil {
		val := *req.ColorScheme
		validators = append(validators, func(b *profileUpdateBuilder) (int, string) {
			return validateColorScheme(b, val)
		})
	}
	return validators
}

// executeProfileUpdate runs the UPDATE query and scans the result into a User model.
func (h *Handler) executeProfileUpdate(b *profileUpdateBuilder, userID interface{}) (*models.User, int, string) {
	b.setClauses = append(b.setClauses, "updated_at = NOW()")

	whereClause := fmt.Sprintf("WHERE id = $%d", b.argIdx)
	if b.usernameChanged {
		whereClause = fmt.Sprintf(
			"WHERE id = $%d AND username_changed_at <= NOW() - INTERVAL '365 days'",
			b.argIdx,
		)
	}
	query := fmt.Sprintf( // #nosec G201 -- Safe: setClauses are parameterized, whereClause uses $N placeholders // nosemgrep:concord-go-sql-sprintf
		`UPDATE users SET %s %s
		 RETURNING id, email, username, display_name, bio, avatar_url, header_image_url, color_scheme,
		           COALESCE(links, '[]'::jsonb),
		           email_verified, age_verified, e2ee_preference, created_at, updated_at,
		           username_changed_at`,
		strings.Join(b.setClauses, ", "), whereClause,
	)
	b.args = append(b.args, userID)

	var user models.User
	err := h.db.QueryRow(query, b.args...).Scan(
		&user.ID, &user.Email, &user.Username, &user.DisplayName,
		&user.Bio, &user.AvatarURL, &user.HeaderImageURL, &user.ColorScheme,
		&user.Links, &user.EmailVerified, &user.AgeVerified, &user.E2EEPreference,
		&user.CreatedAt, &user.UpdatedAt, &user.UsernameChangedAt,
	)
	if err == nil {
		return &user, 0, ""
	}
	if err == sql.ErrNoRows {
		if b.usernameChanged {
			return nil, http.StatusForbidden, "You can only change your username once per year"
		}
		return nil, http.StatusNotFound, errMsgUserNotFound
	}
	if strings.Contains(err.Error(), "users_username_key") {
		return nil, http.StatusBadRequest, "Username is already taken"
	}
	h.log.Error(errMsgFailedUpdateProfile, "error", err)
	return nil, http.StatusInternalServerError, errMsgFailedUpdateProfile
}

// UpdateMe updates the current user's profile
func (h *Handler) UpdateMe(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
		return
	}

	var req UpdateProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	b := newProfileUpdateBuilder()

	if status, msg := h.buildProfileFields(b, &req, userID); status != 0 {
		resp := gin.H{"error": msg}
		for k, v := range b.extraResponse {
			resp[k] = v
		}
		c.JSON(status, resp)
		return
	}

	if len(b.setClauses) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No fields to update"})
		return
	}

	user, status, msg := h.executeProfileUpdate(b, userID)
	if status != 0 {
		c.JSON(status, gin.H{"error": msg})
		return
	}

	// Clean up orphaned media objects from storage after successful DB update
	for _, key := range b.mediaKeysToDelete {
		media.CleanupObject(c.Request.Context(), h.db, h.store, h.log, key)
	}

	// Record username change in history for audit/abuse tracking
	if b.usernameChanged {
		_, histErr := h.db.Exec(
			`INSERT INTO username_history (user_id, old_username, new_username) VALUES ($1, $2, $3)`,
			userID, b.previousUsername, user.Username,
		)
		if histErr != nil {
			h.log.Error("Failed to record username history", "error", histErr)
			// Non-fatal — the username change already succeeded
		}
	}

	h.log.Info("Profile updated", "user_id", userID)

	// Broadcast profile update to all connected clients in real time
	if h.hub != nil {
		h.hub.BroadcastToAll(websocket.OutgoingMessage{
			Type: "profile_updated",
			Data: map[string]interface{}{
				"user_id":          user.ID,
				"username":         user.Username,
				"display_name":     user.DisplayName,
				"avatar_url":       user.AvatarURL,
				"header_image_url": user.HeaderImageURL,
				"color_scheme":     user.ColorScheme,
			},
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"user": user.PublicUser(),
	})
}

// ChangePasswordRequest represents a request to change password
type ChangePasswordRequest struct {
	CurrentPassword   string `json:"current_password" binding:"required"`
	NewPassword       string `json:"new_password" binding:"required"`
	WrappedPrivateKey string `json:"wrapped_private_key" binding:"required"`
	KeyDerivationSalt string `json:"key_derivation_salt" binding:"required"`
	KeyDerivationAlg  string `json:"key_derivation_alg"` // "pbkdf2" or "argon2id"
	MFACode           string `json:"mfa_code"`           // Required when MFA is enabled
}

// verifyCurrentPassword fetches the user's password hash and checks it against the provided password.
// Returns the password hash on success, or an HTTP status + error message on failure.
func (h *Handler) verifyCurrentPassword(userID interface{}, currentPassword string) (string, int, string) {
	var passwordHash string
	err := h.db.QueryRow(
		`SELECT password_hash FROM users WHERE id = $1`,
		userID,
	).Scan(&passwordHash)
	if err == sql.ErrNoRows {
		return "", http.StatusNotFound, errMsgUserNotFound
	}
	if err != nil {
		h.log.Error("Failed to fetch password hash", "error", err)
		return "", http.StatusInternalServerError, errMsgFailedChangePassword
	}

	match, err := auth.VerifyPassword(currentPassword, passwordHash)
	if err != nil {
		h.log.Error("Failed to verify password", "error", err)
		return "", http.StatusInternalServerError, errMsgFailedChangePassword
	}
	if !match {
		return "", http.StatusUnauthorized, "Current password is incorrect"
	}
	return passwordHash, 0, ""
}

// verifyMFAForPasswordChange checks MFA if enabled. Returns an HTTP status + body on failure.
func (h *Handler) verifyMFAForPasswordChange(c *gin.Context, uid, mfaCode string) (int, gin.H) {
	ctx := c.Request.Context()
	if h.mfaVerifier == nil || !h.mfaVerifier.IsEnabled(ctx, uid) {
		return 0, nil
	}
	if mfaCode == "" {
		methods, _ := h.mfaVerifier.GetEnabledMethods(ctx, uid)
		return http.StatusForbidden, gin.H{
			"error":   "mfa_required",
			"message": "MFA verification required to change password",
			"methods": methods,
		}
	}
	valid, mfaErr := h.mfaVerifier.VerifyCode(ctx, uid, mfaCode)
	if mfaErr != nil {
		h.log.Error("MFA verification error during password change", "error", mfaErr)
		return http.StatusInternalServerError, gin.H{"error": "MFA verification failed"}
	}
	if !valid {
		return http.StatusForbidden, gin.H{"error": "Invalid MFA code"}
	}
	return 0, nil
}

// validateNewPassword checks strength and ensures it differs from the current password.
func (h *Handler) validateNewPassword(newPassword, currentHash string) (string, int, string) {
	if err := auth.ValidatePasswordStrength(newPassword); err != nil {
		return "", http.StatusBadRequest, err.Error()
	}
	sameAsOld, err := auth.VerifyPassword(newPassword, currentHash)
	if err != nil {
		h.log.Error("Failed to compare passwords", "error", err)
		return "", http.StatusInternalServerError, errMsgFailedChangePassword
	}
	if sameAsOld {
		return "", http.StatusBadRequest, "New password must be different from current password"
	}
	newHash, err := auth.HashPassword(newPassword)
	if err != nil {
		h.log.Error("Failed to hash new password", "error", err)
		return "", http.StatusInternalServerError, errMsgFailedChangePassword
	}
	return newHash, 0, ""
}

// executePasswordChange runs the transactional password + key update + token revocation.
func (h *Handler) executePasswordChange(userID interface{}, newHash string, wrappedKeyBytes, saltBytes []byte, kdAlg string) error {
	tx, err := h.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback transaction", "error", rbErr)
		}
	}()

	if _, err := tx.Exec(`UPDATE users SET password_hash = $1, updated_at = NOW() WHERE id = $2`, newHash, userID); err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE user_keys SET wrapped_private_key = $1, key_derivation_salt = $2,
		 key_derivation_alg = $3, updated_at = NOW() WHERE user_id = $4`,
		wrappedKeyBytes, saltBytes, kdAlg, userID,
	); err != nil {
		return fmt.Errorf("update E2EE keys: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE refresh_tokens SET revoked_at = NOW() WHERE user_id = $1 AND revoked_at IS NULL`,
		userID,
	); err != nil {
		return fmt.Errorf("revoke refresh tokens: %w", err)
	}
	return tx.Commit()
}

// ChangePassword changes the current user's password and re-wraps E2EE keys
func (h *Handler) ChangePassword(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
		return
	}

	var req ChangePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Current password, new password, and re-wrapped keys are required"})
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

	passwordHash, status, msg := h.verifyCurrentPassword(userID, req.CurrentPassword)
	if status != 0 {
		c.JSON(status, gin.H{"error": msg})
		return
	}

	uid := userID.(string)
	if mfaStatus, mfaBody := h.verifyMFAForPasswordChange(c, uid, req.MFACode); mfaStatus != 0 {
		c.JSON(mfaStatus, mfaBody)
		return
	}

	newHash, status, msg := h.validateNewPassword(req.NewPassword, passwordHash)
	if status != 0 {
		c.JSON(status, gin.H{"error": msg})
		return
	}

	kdAlg := req.KeyDerivationAlg
	if kdAlg == "" {
		kdAlg = "argon2id"
	}

	if err := h.executePasswordChange(userID, newHash, wrappedKeyBytes, saltBytes, kdAlg); err != nil {
		h.log.Error(errMsgFailedChangePassword, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedChangePassword})
		return
	}

	// Force-close all WebSocket connections for this user.
	if parsedUID, parseErr := uuid.Parse(uid); parseErr == nil {
		h.hub.DisconnectUser(parsedUID)
	}

	h.log.Info("Password changed with key re-wrap", "user_id", userID)
	c.JSON(http.StatusOK, gin.H{"message": "Password changed successfully"})
}

// SearchUsers searches for users by username or display_name.
// GET /users/search?q=...
func (h *Handler) SearchUsers(c *gin.Context) {
	userID := c.GetString("user_id")
	query := strings.TrimSpace(c.Query("q"))
	if query == "" || len(query) < 2 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Search query must be at least 2 characters"})
		return
	}

	pattern := "%" + strings.ToLower(query) + "%"
	rows, err := h.db.Query(`
		SELECT u.id, u.username, u.display_name, u.avatar_url
		FROM users u
		LEFT JOIN privacy_settings ps ON ps.user_id = u.id
		WHERE (LOWER(u.username) LIKE $1 OR LOWER(COALESCE(u.display_name, '')) LIKE $1)
		  AND u.id != $2
		  AND COALESCE(ps.searchable_by_username, FALSE) = TRUE
		LIMIT 20
	`, pattern, userID)
	if err != nil {
		h.log.Error("Failed to search users", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to search users"})
		return
	}
	defer func() { _ = rows.Close() }()

	type searchResult struct {
		ID          string  `json:"id"`
		Username    string  `json:"username"`
		DisplayName *string `json:"display_name,omitempty"`
		AvatarURL   *string `json:"avatar_url,omitempty"`
	}

	results := []searchResult{}
	for rows.Next() {
		var r searchResult
		if err := rows.Scan(&r.ID, &r.Username, &r.DisplayName, &r.AvatarURL); err != nil {
			continue
		}
		results = append(results, r)
	}

	c.JSON(http.StatusOK, gin.H{"users": results})
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
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
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
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
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

// --- Privacy Settings ---

// privacySettingsResponse represents the user's privacy settings.
type privacySettingsResponse struct {
	MessagesFriendsOnly   bool   `json:"messages_friends_only"`
	MessagesServerMembers bool   `json:"messages_server_members"`
	DMPrivacyLevel        int    `json:"dm_privacy_level"` // 0=off, 1=friends, 2=friends+server, 3=all
	DMFriendsOfFriends    bool   `json:"dm_friends_of_friends"`
	AutoAcceptFriendCodes bool   `json:"auto_accept_friend_codes"`
	SearchableByUsername  bool   `json:"searchable_by_username"`
	SearchableByEmail     bool   `json:"searchable_by_email"`
	SearchableByPhone     bool   `json:"searchable_by_phone"`
	AllowEmbeddedContent  bool   `json:"allow_embedded_content"`
	UpdatedAt             string `json:"updated_at,omitempty"`
}

// GetPrivacySettings returns the current user's privacy settings.
// Returns defaults if no settings have been saved yet.
// GET /users/me/privacy
func (h *Handler) GetPrivacySettings(c *gin.Context) {
	userID := c.GetString("user_id")

	var ps privacySettingsResponse
	err := h.db.QueryRow(`
		SELECT messages_friends_only, messages_server_members, dm_privacy_level, dm_friends_of_friends,
		       auto_accept_friend_codes, searchable_by_username, searchable_by_email, searchable_by_phone,
		       allow_embedded_content, updated_at
		FROM privacy_settings
		WHERE user_id = $1
	`, userID).Scan(
		&ps.MessagesFriendsOnly, &ps.MessagesServerMembers, &ps.DMPrivacyLevel, &ps.DMFriendsOfFriends,
		&ps.AutoAcceptFriendCodes, &ps.SearchableByUsername, &ps.SearchableByEmail, &ps.SearchableByPhone,
		&ps.AllowEmbeddedContent, &ps.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		// Return full defaults matching the schema defaults (all fields explicit)
		c.JSON(http.StatusOK, gin.H{
			"privacy": privacySettingsResponse{
				MessagesFriendsOnly:   true,
				MessagesServerMembers: true,
				DMPrivacyLevel:        2,
				DMFriendsOfFriends:    false,
				AutoAcceptFriendCodes: false,
				SearchableByUsername:  false,
				SearchableByEmail:     false,
				SearchableByPhone:     false,
				AllowEmbeddedContent:  false,
			},
		})
		return
	}
	if err != nil {
		h.log.Error("Failed to fetch privacy settings", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch privacy settings"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"privacy": ps})
}

// updatePrivacyRequest represents a partial update to privacy settings.
type updatePrivacyRequest struct {
	MessagesFriendsOnly   *bool `json:"messages_friends_only"`
	MessagesServerMembers *bool `json:"messages_server_members"`
	DMPrivacyLevel        *int  `json:"dm_privacy_level"`
	DMFriendsOfFriends    *bool `json:"dm_friends_of_friends"`
	AutoAcceptFriendCodes *bool `json:"auto_accept_friend_codes"`
	SearchableByUsername  *bool `json:"searchable_by_username"`
	SearchableByEmail     *bool `json:"searchable_by_email"`
	SearchableByPhone     *bool `json:"searchable_by_phone"`
	AllowEmbeddedContent  *bool `json:"allow_embedded_content"`
}

// buildPrivacyClauses constructs the SET clauses for a partial privacy settings update.
// Returns an HTTP status + error message on validation failure, or 0/"" on success.
func buildPrivacyClauses(req *updatePrivacyRequest) ([]string, []interface{}, int, string) {
	var setClauses []string
	var args []interface{}
	argIdx := 2 // $1 is reserved for userID

	addBoolClause := func(column string, val *bool) {
		if val == nil {
			return
		}
		setClauses = append(setClauses, fmt.Sprintf("%s = $%d", column, argIdx))
		args = append(args, *val)
		argIdx++
	}

	addBoolClause("messages_friends_only", req.MessagesFriendsOnly)
	addBoolClause("messages_server_members", req.MessagesServerMembers)

	if req.DMPrivacyLevel != nil {
		level := *req.DMPrivacyLevel
		if level < 0 || level > 3 {
			return nil, nil, http.StatusBadRequest, "dm_privacy_level must be 0-3"
		}
		setClauses = append(setClauses, fmt.Sprintf("dm_privacy_level = $%d", argIdx))
		args = append(args, level)
		argIdx++
		setClauses = append(setClauses, dmPrivacyLegacySync(level)...)
	}

	addBoolClause("dm_friends_of_friends", req.DMFriendsOfFriends)
	addBoolClause("auto_accept_friend_codes", req.AutoAcceptFriendCodes)
	addBoolClause("searchable_by_username", req.SearchableByUsername)
	addBoolClause("searchable_by_email", req.SearchableByEmail)
	addBoolClause("searchable_by_phone", req.SearchableByPhone)
	addBoolClause("allow_embedded_content", req.AllowEmbeddedContent)

	return setClauses, args, 0, ""
}

// dmPrivacyLegacySync returns the legacy boolean sync clauses for backward compatibility.
func dmPrivacyLegacySync(level int) []string {
	switch level {
	case 0, 1:
		return []string{"messages_friends_only = TRUE", "messages_server_members = FALSE"}
	case 2:
		return []string{"messages_friends_only = TRUE", "messages_server_members = TRUE"}
	case 3:
		return []string{"messages_friends_only = FALSE", "messages_server_members = TRUE"}
	default:
		return nil
	}
}

// UpdatePrivacySettings updates the current user's privacy settings.
// Accepts a partial JSON body — only provided fields are updated.
// PATCH /users/me/privacy
func (h *Handler) UpdatePrivacySettings(c *gin.Context) {
	userID := c.GetString("user_id")

	var req updatePrivacyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	setClauses, extraArgs, status, msg := buildPrivacyClauses(&req)
	if status != 0 {
		c.JSON(status, gin.H{"error": msg})
		return
	}
	if len(setClauses) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No fields to update"})
		return
	}

	setClauses = append(setClauses, "updated_at = NOW()")
	args := append([]interface{}{userID}, extraArgs...)

	// UPSERT: insert with defaults, update only provided fields
	// #nosec G201 -- Safe: setClauses contains only parameterized assignments
	query := fmt.Sprintf(`
		INSERT INTO privacy_settings (user_id) VALUES ($1)
		ON CONFLICT (user_id) DO UPDATE SET %s
		RETURNING messages_friends_only, messages_server_members, dm_privacy_level, dm_friends_of_friends,
		          auto_accept_friend_codes, searchable_by_username, searchable_by_email, searchable_by_phone,
		          allow_embedded_content, updated_at
	`, strings.Join(setClauses, ", "))

	var ps privacySettingsResponse
	err := h.db.QueryRow(query, args...).Scan(
		&ps.MessagesFriendsOnly, &ps.MessagesServerMembers, &ps.DMPrivacyLevel, &ps.DMFriendsOfFriends,
		&ps.AutoAcceptFriendCodes, &ps.SearchableByUsername, &ps.SearchableByEmail, &ps.SearchableByPhone,
		&ps.AllowEmbeddedContent, &ps.UpdatedAt,
	)
	if err != nil {
		h.log.Error("Failed to update privacy settings", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update privacy settings"})
		return
	}

	h.log.Info("Privacy settings updated", "user_id", userID)
	c.JSON(http.StatusOK, gin.H{"privacy": ps})
}
