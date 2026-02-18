package auth

import (
	"database/sql"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/concord/control-plane/internal/models"
	"github.com/concord/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type Handler struct {
	db        *sql.DB
	log       *logger.Logger
	jwtSecret string
}

func NewHandler(db *sql.DB, log *logger.Logger, jwtSecret string) *Handler {
	return &Handler{
		db:        db,
		log:       log,
		jwtSecret: jwtSecret,
	}
}

// RegisterRequest represents registration payload
type RegisterRequest struct {
	Email                string `json:"email" binding:"required,email"`
	Username             string `json:"username" binding:"required,min=3,max=50"`
	Password             string `json:"password" binding:"required"`
	AgeConfirmation      bool   `json:"age_confirmation" binding:"required"`
	PublicKey            string `json:"public_key" binding:"required"`           // base64 encoded SPKI
	WrappedPrivateKey    string `json:"wrapped_private_key" binding:"required"` // base64 encoded
	KeyDerivationSalt    string `json:"key_derivation_salt" binding:"required"` // base64 encoded
	E2EEPreference       bool   `json:"e2ee_preference"`
}

// LoginRequest represents login payload
type LoginRequest struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required"`
}

// Register handles user registration
func (h *Handler) Register(c *gin.Context) {
	var req RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Validate age confirmation (must be 16+)
	if !req.AgeConfirmation {
		c.JSON(http.StatusBadRequest, gin.H{"error": "You must be at least 16 years old to create an account"})
		return
	}

	// Validate username
	if err := ValidateUsername(req.Username); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Validate password strength
	if err := ValidatePasswordStrength(req.Password); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Hash password with Argon2id
	passwordHash, err := HashPassword(req.Password)
	if err != nil {
		h.log.Error("Failed to hash password", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create account"})
		return
	}

	// Decode E2EE keys from base64
	publicKey, err := base64.StdEncoding.DecodeString(req.PublicKey)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid public key format"})
		return
	}

	wrappedKey, err := base64.StdEncoding.DecodeString(req.WrappedPrivateKey)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid wrapped private key format"})
		return
	}

	salt, err := base64.StdEncoding.DecodeString(req.KeyDerivationSalt)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid salt format"})
		return
	}

	// Start transaction
	tx, err := h.db.Begin()
	if err != nil {
		h.log.Error("Failed to start transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create account"})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback transaction", "error", rbErr)
		}
	}()

	// Normalize username (store lowercase)
	normalizedUsername := NormalizeUsername(req.Username)

	// Create user
	userID := uuid.New().String()
	_, err = tx.Exec(
		`INSERT INTO users (id, email, username, password_hash, age_verified, e2ee_preference)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		userID,
		req.Email,
		normalizedUsername,
		passwordHash,
		true, // Age verified via confirmation
		req.E2EEPreference,
	)
	if err != nil {
		h.log.Error("Failed to create user", "error", err)
		// Check for duplicate email or username
		if strings.Contains(err.Error(), "users_email_key") {
			c.JSON(http.StatusConflict, gin.H{"error": "Email already in use"})
			return
		}
		if strings.Contains(err.Error(), "users_username_key") {
			c.JSON(http.StatusConflict, gin.H{"error": "Username already taken"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create account"})
		return
	}

	// Store E2EE keys (private key wrapped)
	_, err = tx.Exec(
		`INSERT INTO user_keys (user_id, wrapped_private_key, key_derivation_salt)
		 VALUES ($1, $2, $3)`,
		userID,
		wrappedKey,
		salt,
	)
	if err != nil {
		h.log.Error("Failed to store user keys", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create account"})
		return
	}

	// Store public key
	keyID := uuid.New().String()
	_, err = tx.Exec(
		`INSERT INTO public_keys (id, user_id, public_key, key_version)
		 VALUES ($1, $2, $3, $4)`,
		keyID,
		userID,
		publicKey,
		1, // Initial key version
	)
	if err != nil {
		h.log.Error("Failed to store public key", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create account"})
		return
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create account"})
		return
	}

	// Generate access token (JWT, 15 min)
	accessToken, err := GenerateAccessToken(userID, h.jwtSecret)
	if err != nil {
		h.log.Error("Failed to generate access token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create account"})
		return
	}

	// Generate refresh token (random, 30 days)
	refreshToken, err := GenerateRefreshToken()
	if err != nil {
		h.log.Error("Failed to generate refresh token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create account"})
		return
	}

	// Store refresh token in database
	tokenHash := HashRefreshToken(refreshToken)
	tokenID := uuid.New().String()
	expiresAt := time.Now().Add(30 * 24 * time.Hour) // 30 days

	_, err = h.db.Exec(
		`INSERT INTO refresh_tokens (id, user_id, token_hash, device_name, ip_address, user_agent, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		tokenID,
		userID,
		tokenHash,
		c.GetHeader("X-Device-Name"),
		c.ClientIP(),
		c.GetHeader("User-Agent"),
		expiresAt,
	)
	if err != nil {
		h.log.Error("Failed to store refresh token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create account"})
		return
	}

	// Set refresh token as httpOnly cookie
	c.SetCookie(
		"refresh_token",
		refreshToken,
		30*24*60*60, // 30 days in seconds
		"/",
		"",
		c.Request.TLS != nil, // Secure only if HTTPS
		true,                  // HttpOnly
	)

	h.log.Info("User registered", "user_id", userID, "email", req.Email, "username", req.Username)

	// Return access token and user data (same format as login)
	c.JSON(http.StatusCreated, gin.H{
		"access_token": accessToken,
		"expires_in":   900, // 15 minutes
		"user": gin.H{
			"id":              userID,
			"username":        req.Username,
			"e2ee_preference": req.E2EEPreference,
		},
	})
}

// Login handles user authentication
func (h *Handler) Login(c *gin.Context) {
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Fetch user by email
	var user models.User
	err := h.db.QueryRow(
		`SELECT id, email, username, password_hash, display_name, bio, avatar_url, COALESCE(links, '[]'::jsonb),
		        email_verified, age_verified, e2ee_preference, created_at, updated_at
		 FROM users WHERE email = $1`,
		req.Email,
	).Scan(
		&user.ID,
		&user.Email,
		&user.Username,
		&user.PasswordHash,
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
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid email or password"})
			return
		}
		h.log.Error("Failed to fetch user", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Login failed"})
		return
	}

	// Verify password
	valid, err := VerifyPassword(req.Password, user.PasswordHash)
	if err != nil {
		h.log.Error("Failed to verify password", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Login failed"})
		return
	}
	if !valid {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid email or password"})
		return
	}

	// Generate access token (JWT, 15 min)
	accessToken, err := GenerateAccessToken(user.ID, h.jwtSecret)
	if err != nil {
		h.log.Error("Failed to generate access token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Login failed"})
		return
	}

	// Generate refresh token (random, 30 days)
	refreshToken, err := GenerateRefreshToken()
	if err != nil {
		h.log.Error("Failed to generate refresh token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Login failed"})
		return
	}

	// Store refresh token in database
	tokenHash := HashRefreshToken(refreshToken)
	tokenID := uuid.New().String()
	expiresAt := time.Now().Add(30 * 24 * time.Hour) // 30 days

	_, err = h.db.Exec(
		`INSERT INTO refresh_tokens (id, user_id, token_hash, device_name, ip_address, user_agent, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		tokenID,
		user.ID,
		tokenHash,
		c.GetHeader("X-Device-Name"),
		c.ClientIP(),
		c.GetHeader("User-Agent"),
		expiresAt,
	)
	if err != nil {
		h.log.Error("Failed to store refresh token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Login failed"})
		return
	}

	// Set refresh token as httpOnly cookie
	c.SetCookie(
		"refresh_token",
		refreshToken,
		30*24*60*60, // 30 days in seconds
		"/",
		"",
		c.Request.TLS != nil, // Secure only if HTTPS
		true,                  // HttpOnly
	)

	// Fetch E2EE keys
	var keys models.UserKeys
	err = h.db.QueryRow(
		`SELECT user_id, wrapped_private_key, key_derivation_salt, key_version
		 FROM user_keys WHERE user_id = $1`,
		user.ID,
	).Scan(
		&keys.UserID,
		&keys.WrappedPrivateKey,
		&keys.KeyDerivationSalt,
		&keys.KeyVersion,
	)
	if err != nil {
		h.log.Error("Failed to fetch user keys", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Login failed"})
		return
	}

	h.log.Info("User logged in", "user_id", user.ID, "email", user.Email)

	// Return access token and user data
	c.JSON(http.StatusOK, gin.H{
		"access_token": accessToken,
		"expires_in":   900, // 15 minutes
		"user":         user.PublicUser(),
		"e2ee_keys": gin.H{
			"wrapped_private_key":  base64.StdEncoding.EncodeToString(keys.WrappedPrivateKey),
			"key_derivation_salt":  base64.StdEncoding.EncodeToString(keys.KeyDerivationSalt),
			"key_version":          keys.KeyVersion,
		},
	})
}

// Refresh handles access token refresh
func (h *Handler) Refresh(c *gin.Context) {
	// Get refresh token from cookie
	refreshToken, err := c.Cookie("refresh_token")
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "No refresh token provided"})
		return
	}

	// Hash the token to look it up
	tokenHash := HashRefreshToken(refreshToken)

	// Fetch token from database
	var token models.RefreshToken
	err = h.db.QueryRow(
		`SELECT id, user_id, token_hash, device_name, ip_address, user_agent, expires_at, created_at, last_used_at
		 FROM refresh_tokens WHERE token_hash = $1`,
		tokenHash,
	).Scan(
		&token.ID,
		&token.UserID,
		&token.TokenHash,
		&token.DeviceName,
		&token.IPAddress,
		&token.UserAgent,
		&token.ExpiresAt,
		&token.CreatedAt,
		&token.LastUsedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid refresh token"})
			return
		}
		h.log.Error("Failed to fetch refresh token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Refresh failed"})
		return
	}

	// Check if token is expired
	if time.Now().After(token.ExpiresAt) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Refresh token expired"})
		return
	}

	// Update last used timestamp
	_, err = h.db.Exec(
		`UPDATE refresh_tokens SET last_used_at = NOW() WHERE id = $1`,
		token.ID,
	)
	if err != nil {
		h.log.Error("Failed to update refresh token", "error", err)
		// Non-critical, continue
	}

	// Generate new access token
	accessToken, err := GenerateAccessToken(token.UserID, h.jwtSecret)
	if err != nil {
		h.log.Error("Failed to generate access token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Refresh failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"access_token": accessToken,
		"expires_in":   900, // 15 minutes
	})
}

// Logout handles session termination
func (h *Handler) Logout(c *gin.Context) {
	// Get refresh token from cookie
	refreshToken, err := c.Cookie("refresh_token")
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"message": "Already logged out"})
		return
	}

	// Delete refresh token from database
	tokenHash := HashRefreshToken(refreshToken)
	_, err = h.db.Exec(`DELETE FROM refresh_tokens WHERE token_hash = $1`, tokenHash)
	if err != nil {
		h.log.Error("Failed to delete refresh token", "error", err)
		// Continue anyway
	}

	// Clear cookie
	c.SetCookie(
		"refresh_token",
		"",
		-1, // Expire immediately
		"/",
		"",
		c.Request.TLS != nil,
		true,
	)

	c.JSON(http.StatusOK, gin.H{"message": "Logged out successfully"})
}
