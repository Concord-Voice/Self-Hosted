// Package auth provides authentication and authorization functionality including user registration and login.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"crypto/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/email"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/middleware"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/models"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// SessionDisconnector allows forcefully disconnecting WebSocket connections.
// Defined as an interface to avoid an import cycle (websocket imports auth).
type SessionDisconnector interface {
	DisconnectUser(userID uuid.UUID)
}

// RecoveryClaims contains the user_id extracted from a validated recovery token.
type RecoveryClaims struct {
	UserID string
	JTI    string
}

// MFAChecker checks if a user has MFA enabled and returns their methods.
// Defined as an interface to avoid circular imports (mfa imports auth).
type MFAChecker interface {
	IsEnabled(ctx context.Context, userID string) bool
	GetEnabledMethods(ctx context.Context, userID string) ([]string, error)
	// GetLoginMethods returns methods eligible for login (excludes recovery-only methods).
	GetLoginMethods(ctx context.Context, userID string) ([]string, error)
	// GenerateLoginChallenge creates a challenge token for the two-step login flow.
	// Returns the signed JWT and the JTI. Stores remember_me in Redis keyed by JTI.
	GenerateLoginChallenge(ctx context.Context, userID string, rememberMe bool) (token string, jti string, err error)
	// GenerateUpgradeChallenge creates a challenge token for pre-MFA sessions that
	// need to verify MFA before continuing. Issues fresh tokens on success.
	GenerateUpgradeChallenge(ctx context.Context, userID string, rememberMe bool) (token string, jti string, err error)
	// BeginWebAuthnLogin starts a WebAuthn assertion ceremony and stores session data
	// in Redis keyed by the challenge JTI. Returns credential request options for the client.
	// Returns nil options if user has no WebAuthn credentials.
	BeginWebAuthnLogin(ctx context.Context, userID string, jti string) (options interface{}, err error)
	// GenerateRecoveryToken creates a recovery-purpose JWT with a 25-hour TTL.
	GenerateRecoveryToken(userID string) (token string, jti string, err error)
	// ValidateRecoveryToken validates a recovery-purpose JWT and returns the claims.
	ValidateRecoveryToken(tokenString string) (*RecoveryClaims, error)
}

const (
	// maxUserAgentLength limits User-Agent storage to reduce metadata footprint.
	maxUserAgentLength = 256

	errMsgInvalidRequestBody      = "Invalid request body"
	errMsgFailedStartTransaction  = "Failed to start transaction"
	errMsgFailedRollbackTx        = "Failed to rollback transaction"
	errMsgFailedCreateAccount     = "Failed to create account"
	errMsgLoginFailed             = "Login failed"
	errMsgInvalidCredentials      = "Invalid credentials" //nolint:gosec // G101 false positive: error message text, not a credential
)

// truncateUserAgent limits the User-Agent string to maxUserAgentLength.
func truncateUserAgent(ua string) string {
	if len(ua) > maxUserAgentLength {
		return ua[:maxUserAgentLength]
	}
	return ua
}

// nilIfEmpty returns nil for empty strings (maps to SQL NULL) or the string pointer.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// setRefreshCookie sets a refresh_token cookie with secure defaults.
func setRefreshCookie(c *gin.Context, value string, maxAge int) {
	secure := c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https"
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie("refresh_token", value, maxAge, "/", "", secure, true)
}

// Handler handles authentication-related requests including registration and login.
type Handler struct {
	db         *sql.DB
	redis      *redis.Client
	log        *logger.Logger
	jwtSecret  string
	hub        SessionDisconnector
	mfaChecker MFAChecker
	emailSvc      *email.Service
	verifyHandler *VerificationHandler
}

// NewHandler creates a new authentication handler.
func NewHandler(db *sql.DB, redisClient *redis.Client, log *logger.Logger, jwtSecret string, hub SessionDisconnector) *Handler {
	return &Handler{
		db:        db,
		redis:     redisClient,
		log:       log,
		jwtSecret: jwtSecret,
		hub:       hub,
	}
}

// SetEmailService sets the email service (called after initialization to avoid circular deps).
func (h *Handler) SetEmailService(svc *email.Service) {
	h.emailSvc = svc
}

// SetVerificationHandler wires the verification handler for use in post-registration code sending.
func (h *Handler) SetVerificationHandler(vh *VerificationHandler) {
	h.verifyHandler = vh
}

// SetMFAChecker sets the MFA checker (called after both handlers are initialized to break circular init).
func (h *Handler) SetMFAChecker(checker MFAChecker) {
	h.mfaChecker = checker
}

// RegisterRequest represents registration payload
type RegisterRequest struct {
	Email                string `json:"email" binding:"required,email"`
	Username             string `json:"username" binding:"required,min=3,max=50"`
	Password             string `json:"password" binding:"required"`             // #nosec G117 -- False positive: request field, not stored secret
	AgeConfirmation      bool   `json:"age_confirmation" binding:"required"`
	PublicKey            string `json:"public_key" binding:"required"`           // base64 encoded SPKI
	WrappedPrivateKey    string `json:"wrapped_private_key" binding:"required"` // base64 encoded
	KeyDerivationSalt    string `json:"key_derivation_salt" binding:"required"` // base64 encoded
	KeyDerivationAlg     string `json:"key_derivation_alg"`                     // "pbkdf2" or "argon2id"; defaults to "argon2id"
	E2EEPreference       bool   `json:"e2ee_preference"`
}

// LoginRequest represents login payload
type LoginRequest struct {
	Email      string `json:"email" binding:"required,email"`
	Password   string `json:"password" binding:"required"` // #nosec G117 -- False positive: request field, not stored secret
	RememberMe *bool  `json:"remember_me"`                 // nil defaults to true for backward compat
}

// Register handles user registration
func (h *Handler) Register(c *gin.Context) {
	var req RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateAccount})
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
		h.log.Error(errMsgFailedStartTransaction, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateAccount})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error(errMsgFailedRollbackTx, "error", rbErr)
		}
	}()

	// Normalize username and email (store lowercase)
	normalizedUsername := NormalizeUsername(req.Username)
	normalizedEmail := strings.ToLower(strings.TrimSpace(req.Email))

	// Create user
	userID := uuid.New().String()
	_, err = tx.Exec(
		`INSERT INTO users (id, email, username, password_hash, age_verified, e2ee_preference)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		userID,
		normalizedEmail,
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateAccount})
		return
	}

	// Store E2EE keys (private key wrapped)
	kdAlg := req.KeyDerivationAlg
	if kdAlg == "" {
		kdAlg = "argon2id" // Default for new registrations
	}
	_, err = tx.Exec(
		`INSERT INTO user_keys (user_id, wrapped_private_key, key_derivation_salt, key_derivation_alg)
		 VALUES ($1, $2, $3, $4)`,
		userID,
		wrappedKey,
		salt,
		kdAlg,
	)
	if err != nil {
		h.log.Error("Failed to store user keys", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateAccount})
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateAccount})
		return
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateAccount})
		return
	}

	// Generate access token (JWT, 15 min) — email NOT yet verified for new registrations
	accessToken, err := GenerateAccessToken(userID, h.jwtSecret, false)
	if err != nil {
		h.log.Error("Failed to generate access token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateAccount})
		return
	}

	// Generate refresh token (random, 30 days)
	refreshToken, err := GenerateRefreshToken()
	if err != nil {
		h.log.Error("Failed to generate refresh token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateAccount})
		return
	}

	// Store refresh token in database
	tokenHash := HashRefreshToken(refreshToken)
	tokenID := uuid.New().String()
	expiresAt := time.Now().Add(30 * 24 * time.Hour) // 30 days
	machineID := c.GetHeader("X-Machine-Id")

	_, err = h.db.Exec(
		`INSERT INTO refresh_tokens (id, user_id, token_hash, device_name, ip_address, user_agent, expires_at, remember_me, machine_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		tokenID,
		userID,
		tokenHash,
		c.GetHeader("X-Device-Name"),
		c.ClientIP(),
		truncateUserAgent(c.GetHeader("User-Agent")),
		expiresAt,
		true, // Registration always defaults to remember_me=true
		nilIfEmpty(machineID),
	)
	if err != nil {
		h.log.Error("Failed to store refresh token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateAccount})
		return
	}

	// Set refresh token as httpOnly cookie
	setRefreshCookie(c, refreshToken, 30*24*60*60)

	h.log.Info("User registered", "user_id", userID, "username", normalizedUsername)

	// Auto-send verification code (non-blocking — don't fail registration if this fails)
	if h.verifyHandler != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := h.verifyHandler.GenerateAndSendCode(ctx, userID, normalizedEmail); err != nil {
				h.log.Error("Failed to send verification code on register", "error", err)
			}
		}()
	}

	// Return access token, refresh token, and user data (same format as login).
	// The refresh token is returned in both a cookie and the body so that
	// Electron/native clients (file:// origin) can store it explicitly —
	// SameSite=Lax cookies are not sent on cross-site POST requests.
	c.JSON(http.StatusCreated, gin.H{
		"access_token":  accessToken,
		"refresh_token": refreshToken,
		"session_id":    tokenID,
		"expires_in":    900, // 15 minutes
		"remember_me":   true,
		"user": gin.H{
			"id":              userID,
			"username":        normalizedUsername,
			"email":           normalizedEmail,
			"email_verified":  false,
			"e2ee_preference": req.E2EEPreference,
		},
	})
}

// loginLockoutThreshold is the number of failed attempts before lockout.
const loginLockoutThreshold = 5

// loginLockoutDurations defines escalating lockout windows.
var loginLockoutDurations = []time.Duration{15 * time.Minute, 30 * time.Minute, 60 * time.Minute}

// checkLoginLockout returns true if the account is currently locked out.
func (h *Handler) checkLoginLockout(ctx context.Context, email string) bool {
	key := fmt.Sprintf("login_lockout:%s", email)
	if _, err := h.redis.Get(ctx, key).Result(); err == nil {
		return true // lockout key exists
	}
	return false
}

// recordFailedLogin increments the failure counter and applies lockout if threshold is reached.
func (h *Handler) recordFailedLogin(ctx context.Context, email string) {
	attemptsKey := fmt.Sprintf("login_attempts:%s", email)
	lockoutKey := fmt.Sprintf("login_lockout:%s", email)
	lockoutCountKey := fmt.Sprintf("login_lockout_count:%s", email)

	count, _ := h.redis.Incr(ctx, attemptsKey).Result()
	// Set a 1-hour window for the attempts counter to auto-expire
	h.redis.Expire(ctx, attemptsKey, 1*time.Hour)

	if count >= int64(loginLockoutThreshold) {
		// Determine lockout duration (escalating)
		lockoutCount, _ := h.redis.Incr(ctx, lockoutCountKey).Result()
		h.redis.Expire(ctx, lockoutCountKey, 24*time.Hour)
		idx := int(lockoutCount) - 1
		if idx >= len(loginLockoutDurations) {
			idx = len(loginLockoutDurations) - 1
		}
		duration := loginLockoutDurations[idx]

		h.redis.Set(ctx, lockoutKey, "1", duration)
		h.redis.Del(ctx, attemptsKey) // Reset attempts counter
	}
}

// clearLoginAttempts resets counters on successful login.
func (h *Handler) clearLoginAttempts(ctx context.Context, email string) {
	h.redis.Del(ctx,
		fmt.Sprintf("login_attempts:%s", email),
		fmt.Sprintf("login_lockout:%s", email),
		fmt.Sprintf("login_lockout_count:%s", email),
	)
}

// Login handles user authentication
func (h *Handler) Login(c *gin.Context) {
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	ctx := c.Request.Context()

	// Check if account is locked out (generic message — don't reveal lockout)
	if h.checkLoginLockout(ctx, req.Email) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgInvalidCredentials})
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
			// Record failure even for non-existent accounts (timing-safe)
			h.recordFailedLogin(ctx, req.Email)
			c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgInvalidCredentials})
			return
		}
		h.log.Error("Failed to fetch user", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgLoginFailed})
		return
	}

	// Verify password
	valid, err := VerifyPassword(req.Password, user.PasswordHash)
	if err != nil {
		h.log.Error("Failed to verify password", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgLoginFailed})
		return
	}
	if !valid {
		h.recordFailedLogin(ctx, req.Email)
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgInvalidCredentials})
		return
	}

	// Clear failed login attempts on success
	h.clearLoginAttempts(ctx, req.Email)

	// Resolve remember_me preference (default: true for backward compat)
	rememberMe := true
	if req.RememberMe != nil {
		rememberMe = *req.RememberMe
	}

	// Check if user has MFA enabled — if so, return challenge instead of tokens
	// Only present login-eligible methods (excludes recovery-only like Email/SMS when flagged)
	if h.mfaChecker != nil && h.mfaChecker.IsEnabled(ctx, user.ID) {
		allMethods, _ := h.mfaChecker.GetEnabledMethods(ctx, user.ID)
		loginMethods, _ := h.mfaChecker.GetLoginMethods(ctx, user.ID)
		challengeToken, jti, mfaErr := h.mfaChecker.GenerateLoginChallenge(ctx, user.ID, rememberMe)
		if mfaErr != nil {
			h.log.Error("Failed to generate MFA challenge", "error", mfaErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgLoginFailed})
			return
		}

		// Compute recovery-only methods (in allMethods but not in loginMethods)
		loginSet := make(map[string]bool, len(loginMethods))
		for _, m := range loginMethods {
			loginSet[m] = true
		}
		var recoveryOnly []string
		for _, m := range allMethods {
			if !loginSet[m] {
				recoveryOnly = append(recoveryOnly, m)
			}
		}

		h.log.Info("MFA required for login", "user_id", user.ID, "methods", loginMethods)
		resp := gin.H{
			"mfa_required":        true,
			"mfa_challenge_token": challengeToken,
			"methods":             loginMethods,
		}
		if len(recoveryOnly) > 0 {
			resp["recovery_only_methods"] = recoveryOnly
		}
		// If WebAuthn is available, start the assertion ceremony and include options
		if loginSet["webauthn"] {
			if opts, err := h.mfaChecker.BeginWebAuthnLogin(ctx, user.ID, jti); err != nil {
				h.log.Error("Failed to begin WebAuthn login", "error", err)
			} else if opts != nil {
				resp["webauthn_options"] = opts
			}
		}
		c.JSON(http.StatusOK, resp)
		return
	}

	// No MFA — complete login directly
	h.CompleteLogin(c, user.ID, rememberMe)
}

// CompleteLogin issues tokens and creates a session for the given user.
// Called directly from Login (no MFA) or from the MFA verify handler after successful verification.
func (h *Handler) CompleteLogin(c *gin.Context, userID string, rememberMe bool) {
	// Look up email_verified from DB so the JWT claim reflects current state
	var emailVerified bool
	if err := h.db.QueryRow(`SELECT email_verified FROM users WHERE id = $1`, userID).Scan(&emailVerified); err != nil {
		h.log.Error("Failed to look up email_verified for login", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgLoginFailed})
		return
	}

	// Generate access token (JWT, 15 min)
	accessToken, err := GenerateAccessToken(userID, h.jwtSecret, emailVerified)
	if err != nil {
		h.log.Error("Failed to generate access token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgLoginFailed})
		return
	}

	// Generate refresh token (random, 30 days)
	refreshToken, err := GenerateRefreshToken()
	if err != nil {
		h.log.Error("Failed to generate refresh token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgLoginFailed})
		return
	}

	// Store refresh token in database
	tokenHash := HashRefreshToken(refreshToken)
	tokenID := uuid.New().String()
	expiresAt := time.Now().Add(30 * 24 * time.Hour)
	deviceName := c.GetHeader("X-Device-Name")
	ipAddress := c.ClientIP()
	userAgent := truncateUserAgent(c.GetHeader("User-Agent"))
	machineID := c.GetHeader("X-Machine-Id")

	h.log.Info("CompleteLogin: storing refresh token",
		"hash_prefix", tokenHash[:16],
		"remember_me", rememberMe,
		"machine_id", machineID,
		"user_id", userID,
	)

	// Revoke old sessions from the same device
	if machineID != "" {
		_, _ = h.db.Exec(
			`UPDATE refresh_tokens SET revoked_at = NOW()
			 WHERE user_id = $1 AND machine_id = $2 AND revoked_at IS NULL`,
			userID, machineID,
		)
	} else {
		_, _ = h.db.Exec(
			`UPDATE refresh_tokens SET revoked_at = NOW()
			 WHERE user_id = $1 AND ip_address = $2 AND user_agent = $3 AND revoked_at IS NULL`,
			userID, ipAddress, userAgent,
		)
	}

	_, err = h.db.Exec(
		`INSERT INTO refresh_tokens (id, user_id, token_hash, device_name, ip_address, user_agent, expires_at, remember_me, machine_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		tokenID, userID, tokenHash, deviceName, ipAddress, userAgent, expiresAt, rememberMe, nilIfEmpty(machineID),
	)
	if err != nil {
		h.log.Error("Failed to store refresh token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgLoginFailed})
		return
	}

	// Cookie MaxAge: 30 days if remember_me, session-scoped otherwise
	cookieMaxAge := 30 * 24 * 60 * 60
	if !rememberMe {
		cookieMaxAge = 0
	}
	setRefreshCookie(c, refreshToken, cookieMaxAge)

	// Fetch user data
	var user models.User
	err = h.db.QueryRow(
		`SELECT id, email, username, password_hash, display_name, bio, avatar_url, COALESCE(links, '[]'::jsonb),
		        email_verified, age_verified, e2ee_preference, created_at, updated_at
		 FROM users WHERE id = $1`, userID,
	).Scan(
		&user.ID, &user.Email, &user.Username, &user.PasswordHash,
		&user.DisplayName, &user.Bio, &user.AvatarURL, &user.Links,
		&user.EmailVerified, &user.AgeVerified, &user.E2EEPreference,
		&user.CreatedAt, &user.UpdatedAt,
	)
	if err != nil {
		h.log.Error("Failed to fetch user for login response", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgLoginFailed})
		return
	}

	// Fetch E2EE keys
	var keys models.UserKeys
	err = h.db.QueryRow(
		`SELECT user_id, wrapped_private_key, key_derivation_salt, key_version, key_derivation_alg
		 FROM user_keys WHERE user_id = $1`, userID,
	).Scan(&keys.UserID, &keys.WrappedPrivateKey, &keys.KeyDerivationSalt, &keys.KeyVersion, &keys.KeyDerivationAlg)
	if err != nil {
		h.log.Error("Failed to fetch user keys", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgLoginFailed})
		return
	}

	h.log.Info("User logged in (MFA verified)", "user_id", userID)

	c.JSON(http.StatusOK, gin.H{
		"access_token":  accessToken,
		"refresh_token": refreshToken,
		"session_id":    tokenID,
		"expires_in":    900,
		"remember_me":   rememberMe,
		"user":          user.PublicUser(),
		"e2ee_keys": gin.H{
			"wrapped_private_key": base64.StdEncoding.EncodeToString(keys.WrappedPrivateKey),
			"key_derivation_salt": base64.StdEncoding.EncodeToString(keys.KeyDerivationSalt),
			"key_version":         keys.KeyVersion,
			"key_derivation_alg":  keys.KeyDerivationAlg,
		},
	})
}

// Refresh handles access token refresh with token rotation.
// Each refresh token is single-use: a new refresh token is issued on every refresh.
// If a previously-used token is presented again, the request is rejected and the
// anomaly is logged. The replayed token is already revoked (single-use rotation).
func (h *Handler) Refresh(c *gin.Context) {
	// Get refresh token from cookie first, fall back to X-Refresh-Token header.
	// Electron/native clients loaded from file:// can't send SameSite=Lax
	// cookies on cross-site POST requests, so they pass the token via header.
	refreshToken, err := c.Cookie("refresh_token")
	if err != nil || refreshToken == "" {
		refreshToken = c.GetHeader("X-Refresh-Token")
	}
	if refreshToken == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "No refresh token provided"})
		return
	}

	// Hash the token to look it up
	tokenHash := HashRefreshToken(refreshToken)
	tokenSuffix := refreshToken[len(refreshToken)-8:]
	hashPrefix := tokenHash[:16]
	h.log.Info("Refresh attempt", "token_suffix", tokenSuffix, "hash_prefix", hashPrefix, "machine_id", c.GetHeader("X-Machine-Id"))

	// Fetch token from database (exclude revoked sessions)
	var token models.RefreshToken
	var storedMachineID sql.NullString
	err = h.db.QueryRow(
		`SELECT id, user_id, token_hash, device_name, ip_address, user_agent, expires_at, created_at, last_used_at, remember_me, COALESCE(machine_id, '')
		 FROM refresh_tokens WHERE token_hash = $1 AND revoked_at IS NULL`,
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
		&token.RememberMe,
		&storedMachineID,
	)
	if storedMachineID.Valid {
		token.MachineID = storedMachineID.String
	}
	if err != nil {
		if err == sql.ErrNoRows {
			// Replay detection: check if this token was already used (revoked but exists).
			// Grace period: if the token was revoked very recently (< 30s), treat it as a
			// legitimate crash-recovery replay. This covers the case where the client was
			// killed between the server revoking the old token and the client saving the
			// new one (e.g. process crash, HMR reload, forced quit).
			var revokedUserID, revokedIP, revokedUA, revokedMachineID string
			var revokedAt time.Time
			var revokedTokenID string
			revokeErr := h.db.QueryRow(
				`SELECT id, user_id, revoked_at, ip_address, user_agent, COALESCE(machine_id, '')
				 FROM refresh_tokens WHERE token_hash = $1 AND revoked_at IS NOT NULL`,
				tokenHash,
			).Scan(&revokedTokenID, &revokedUserID, &revokedAt, &revokedIP, &revokedUA, &revokedMachineID)
			if revokeErr == nil && revokedUserID != "" {
				gracePeriod := 30 * time.Second
				requestIP := c.ClientIP()
				requestUA := truncateUserAgent(c.GetHeader("User-Agent"))
				graceRequestMachineID := c.GetHeader("X-Machine-Id")
				// Postgres INET may return CIDR notation (e.g. "127.0.0.1/32"); strip suffix for comparison
				storedIP := revokedIP
				if idx := strings.IndexByte(storedIP, '/'); idx != -1 {
					storedIP = storedIP[:idx]
				}
				// Machine ID must match if both sides have one
				machineIDOk := revokedMachineID == "" || graceRequestMachineID == "" || revokedMachineID == graceRequestMachineID

				if time.Since(revokedAt) < gracePeriod && storedIP == requestIP && revokedUA == requestUA && machineIDOk {
					// Within grace period AND same device fingerprint — likely a crash
					// recovery, not a stolen token replay from a different machine.
					h.log.Info("Refresh token replay within grace period, recovering session",
						"user_id", revokedUserID,
						"revoked_ago_ms", time.Since(revokedAt).Milliseconds(),
						"ip", requestIP,
					)
					h.handleGracePeriodRefresh(c, revokedUserID, revokedTokenID, revokedAt)
					return
				}
				// Outside grace period or different device — genuine stale replay
				h.log.Warn("Refresh token replay detected, stale token replayed",
					"user_id", revokedUserID,
					"token_hash", tokenHash,
					"same_ip", revokedIP == requestIP,
					"same_ua", revokedUA == requestUA,
					"revoked_ago_ms", time.Since(revokedAt).Milliseconds(),
				)
			} else {
				// Token hash not found anywhere — completely unknown token
				h.log.Warn("Refresh token not found in database (not active, not revoked)",
					"token_suffix", tokenSuffix,
					"hash_prefix", hashPrefix,
				)
			}
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

	// Offline-duration expiry: for "Remember Me" tokens, the 30-day timer only
	// counts while the user is offline. If they're online (including invisible),
	// the token never expires from inactivity.
	if token.RememberMe {
		ctx := context.Background()
		presenceKey := fmt.Sprintf("presence:%s", token.UserID)
		_, presenceErr := h.redis.Get(ctx, presenceKey).Result()

		if presenceErr == redis.Nil {
			// User is offline — check how long
			lastSeenKey := fmt.Sprintf("last_seen:%s", token.UserID)
			lastSeenStr, lsErr := h.redis.Get(ctx, lastSeenKey).Result()
			if lsErr == nil && lastSeenStr != "" {
				lastSeenUnix, parseErr := strconv.ParseInt(lastSeenStr, 10, 64)
				if parseErr == nil {
					offlineDuration := time.Since(time.Unix(lastSeenUnix, 0))
					if offlineDuration > 30*24*time.Hour {
						c.JSON(http.StatusUnauthorized, gin.H{
							"error": "Session expired due to prolonged inactivity",
						})
						return
					}
				}
			}
		}
		// presence key exists (online/dnd/invisible) → skip, token stays valid
	}

	// ── Machine ID theft detection (#89) ───────────────────────────────
	requestMachineID := c.GetHeader("X-Machine-Id")
	if token.MachineID != "" && requestMachineID != "" && token.MachineID != requestMachineID {
		requestIP := c.ClientIP()
		storedIP := token.IPAddress
		if idx := strings.IndexByte(storedIP, '/'); idx != -1 {
			storedIP = storedIP[:idx]
		}

		if storedIP == requestIP {
			// Same IP, different machine — suspicious but not theft
			if h.mfaChecker != nil && h.mfaChecker.IsEnabled(c.Request.Context(), token.UserID) {
				// User has MFA — require verification before allowing
				ctx := c.Request.Context()
				allMethods, _ := h.mfaChecker.GetEnabledMethods(ctx, token.UserID)
				loginMethods, _ := h.mfaChecker.GetLoginMethods(ctx, token.UserID)
				challengeToken, jti, mfaErr := h.mfaChecker.GenerateLoginChallenge(ctx, token.UserID, token.RememberMe)
				if mfaErr != nil {
					h.log.Error("Failed to generate suspicious refresh MFA challenge", "error", mfaErr)
					// Fall through to allow (graceful degradation)
				} else {
					h.log.Warn("Suspicious refresh: different machine_id, same IP — MFA required",
						"user_id", token.UserID,
						"stored_machine_id", token.MachineID,
						"request_machine_id", requestMachineID,
						"ip", requestIP,
					)
					resp := gin.H{
						"error":              "suspicious_session_mfa",
						"message":            "Session verification required",
						"mfa_challenge_token": challengeToken,
						"methods":            loginMethods,
					}
					// Include recovery-only methods so frontend can filter them from the method picker
					loginSet := make(map[string]bool, len(loginMethods))
					for _, m := range loginMethods {
						loginSet[m] = true
					}
					var recoveryOnly []string
					for _, m := range allMethods {
						if !loginSet[m] {
							recoveryOnly = append(recoveryOnly, m)
						}
					}
					if len(recoveryOnly) > 0 {
						resp["recovery_only_methods"] = recoveryOnly
					}
					if loginSet["webauthn"] {
						if opts, werr := h.mfaChecker.BeginWebAuthnLogin(ctx, token.UserID, jti); werr != nil {
							h.log.Error("Failed to begin WebAuthn login for suspicious refresh", "error", werr)
						} else if opts != nil {
							resp["webauthn_options"] = opts
						}
					}
					c.JSON(http.StatusForbidden, resp)
					return
				}
			}
			// No MFA or challenge generation failed — log warning, allow (graceful degradation)
			h.log.Warn("Refresh from different machine_id but same IP (no MFA: allowing)",
				"user_id", token.UserID,
				"stored_machine_id", token.MachineID,
				"request_machine_id", requestMachineID,
				"ip", requestIP,
			)
		} else {
			// Different machine AND different IP — HIGH RISK: token theft
			h.log.Error("TOKEN THEFT DETECTED: different machine_id and different IP",
				"user_id", token.UserID,
				"stored_machine_id", token.MachineID,
				"request_machine_id", requestMachineID,
				"stored_ip", storedIP,
				"request_ip", requestIP,
			)

			// Revoke ALL sessions for this user
			_, _ = h.db.Exec(
				`UPDATE refresh_tokens SET revoked_at = NOW() WHERE user_id = $1 AND revoked_at IS NULL`,
				token.UserID,
			)

			// Disconnect all WebSocket connections
			if uid, parseErr := uuid.Parse(token.UserID); parseErr == nil {
				h.hub.DisconnectUser(uid)
			}

			// Trigger E2EE key revocation for all encrypted channels
			h.triggerTheftKeyRevocations(token.UserID)

			c.JSON(http.StatusUnauthorized, gin.H{
				"error":      "Session terminated: potential token theft detected",
				"error_code": "session_theft_detected",
			})
			return
		}
	}

	// ── Pre-MFA session lock (#89) ────────────────────────────────────
	// When a user enables MFA, all sessions created before mfa_enabled_at
	// must pass an MFA challenge on their next refresh. This prevents a
	// stolen-but-not-yet-used session from bypassing MFA.
	if h.mfaChecker != nil && h.mfaChecker.IsEnabled(c.Request.Context(), token.UserID) {
		// Check for bypass (set by MFA verify handler after successful upgrade)
		bypassKey := fmt.Sprintf("mfa_upgrade_bypass:%s", token.UserID)
		if h.redis.Exists(c.Request.Context(), bypassKey).Val() > 0 {
			h.redis.Del(c.Request.Context(), bypassKey)
			h.log.Info("Pre-MFA session bypass consumed",
				"user_id", token.UserID,
			)
			// Skip the pre-MFA check — user just verified MFA
		} else {
			var mfaEnabledAt sql.NullTime
			_ = h.db.QueryRow(
				`SELECT mfa_enabled_at FROM users WHERE id = $1`, token.UserID,
			).Scan(&mfaEnabledAt)

			if mfaEnabledAt.Valid && token.CreatedAt.Before(mfaEnabledAt.Time) {
				ctx := c.Request.Context()
				allMethods, _ := h.mfaChecker.GetEnabledMethods(ctx, token.UserID)
				loginMethods, _ := h.mfaChecker.GetLoginMethods(ctx, token.UserID)
				challengeToken, jti, mfaErr := h.mfaChecker.GenerateUpgradeChallenge(ctx, token.UserID, token.RememberMe)
				if mfaErr != nil {
					h.log.Error("Failed to generate pre-MFA session challenge", "error", mfaErr)
					// Fall through — don't block the user if challenge generation fails
				} else {
					h.log.Info("Pre-MFA session requires MFA verification",
						"user_id", token.UserID,
						"session_created", token.CreatedAt,
						"mfa_enabled_at", mfaEnabledAt.Time,
					)
					resp := gin.H{
						"error":               "mfa_upgrade_required",
						"message":             "This session was created before MFA was enabled. Please verify your identity.",
						"mfa_challenge_token": challengeToken,
						"methods":             loginMethods,
					}
					loginSet := make(map[string]bool, len(loginMethods))
					for _, m := range loginMethods {
						loginSet[m] = true
					}
					var recoveryOnly []string
					for _, m := range allMethods {
						if !loginSet[m] {
							recoveryOnly = append(recoveryOnly, m)
						}
					}
					if len(recoveryOnly) > 0 {
						resp["recovery_only_methods"] = recoveryOnly
					}
					if loginSet["webauthn"] {
						if opts, werr := h.mfaChecker.BeginWebAuthnLogin(ctx, token.UserID, jti); werr != nil {
							h.log.Error("Failed to begin WebAuthn login for upgrade", "error", werr)
						} else if opts != nil {
							resp["webauthn_options"] = opts
						}
					}
					c.JSON(http.StatusForbidden, resp)
					return
				}
			}
		}
	}

	// Revoke the old refresh token (single-use rotation)
	_, err = h.db.Exec(
		`UPDATE refresh_tokens SET revoked_at = NOW(), last_used_at = NOW() WHERE id = $1`,
		token.ID,
	)
	if err != nil {
		h.log.Error("Failed to revoke old refresh token", "error", err)
	}

	// Generate new refresh token
	newRefreshToken, err := GenerateRefreshToken()
	if err != nil {
		h.log.Error("Failed to generate refresh token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Refresh failed"})
		return
	}

	// Determine expiry for the new token
	newExpiry := token.ExpiresAt
	cookieMaxAge := 0
	if token.RememberMe {
		newExpiry = time.Now().Add(30 * 24 * time.Hour) // Rolling 30 days
		cookieMaxAge = 30 * 24 * 60 * 60
	}

	// Store new refresh token (propagate machine_id from request or stored token)
	newTokenHash := HashRefreshToken(newRefreshToken)
	newTokenID := uuid.New().String()
	propagatedMachineID := requestMachineID
	if propagatedMachineID == "" {
		propagatedMachineID = token.MachineID
	}
	h.log.Info("Refresh: rotating token",
		"old_suffix", tokenSuffix,
		"new_suffix", newRefreshToken[len(newRefreshToken)-8:],
		"new_hash_prefix", newTokenHash[:16],
		"user_id", token.UserID,
		"remember_me", token.RememberMe,
	)
	_, err = h.db.Exec(
		`INSERT INTO refresh_tokens (id, user_id, token_hash, device_name, ip_address, user_agent, expires_at, remember_me, machine_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		newTokenID,
		token.UserID,
		newTokenHash,
		token.DeviceName,
		c.ClientIP(),
		truncateUserAgent(c.GetHeader("User-Agent")),
		newExpiry,
		token.RememberMe,
		nilIfEmpty(propagatedMachineID),
	)

	// Auto-purge old revoked sessions for this user (> 90 days) to minimize metadata retention
	_, _ = h.db.Exec(
		`DELETE FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NOT NULL AND revoked_at < NOW() - INTERVAL '90 days'`,
		token.UserID,
	)
	if err != nil {
		h.log.Error("Failed to store new refresh token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Refresh failed"})
		return
	}

	// Set new refresh token cookie
	setRefreshCookie(c, newRefreshToken, cookieMaxAge)

	// Look up current email_verified so the new JWT reflects DB state
	var refreshEmailVerified bool
	if err := h.db.QueryRow(`SELECT email_verified FROM users WHERE id = $1`, token.UserID).Scan(&refreshEmailVerified); err != nil {
		h.log.Error("Failed to look up email_verified for refresh", "error", err, "user_id", token.UserID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Refresh failed"})
		return
	}

	// Generate new access token
	accessToken, err := GenerateAccessToken(token.UserID, h.jwtSecret, refreshEmailVerified)
	if err != nil {
		h.log.Error("Failed to generate access token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Refresh failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"access_token":  accessToken,
		"refresh_token": newRefreshToken,
		"session_id":    newTokenID,
		"expires_in":    900, // 15 minutes
	})
}

// handleGracePeriodRefresh recovers a session when a recently-revoked refresh token
// is replayed within the grace period. This happens when the client process was killed
// between the server revoking the old token and the client persisting the new one.
// We find the successor token (issued during the rotation that revoked the replayed
// token), revoke it, and issue a fresh token pair — effectively re-rotating.
func (h *Handler) handleGracePeriodRefresh(c *gin.Context, userID string, revokedTokenID string, revokedAt time.Time) {
	// Find the successor token: same user, created around the time the replayed token
	// was revoked (within a small window), and not yet revoked itself.
	var successor models.RefreshToken
	var successorMachineID sql.NullString
	err := h.db.QueryRow(
		`SELECT id, user_id, token_hash, device_name, ip_address, user_agent, expires_at, created_at, last_used_at, remember_me, COALESCE(machine_id, '')
		 FROM refresh_tokens
		 WHERE user_id = $1
		   AND revoked_at IS NULL
		   AND created_at >= $2 - INTERVAL '2 seconds'
		   AND created_at <= $2 + INTERVAL '2 seconds'
		 ORDER BY created_at DESC
		 LIMIT 1`,
		userID, revokedAt,
	).Scan(
		&successor.ID,
		&successor.UserID,
		&successor.TokenHash,
		&successor.DeviceName,
		&successor.IPAddress,
		&successor.UserAgent,
		&successor.ExpiresAt,
		&successor.CreatedAt,
		&successor.LastUsedAt,
		&successor.RememberMe,
		&successorMachineID,
	)
	if successorMachineID.Valid {
		successor.MachineID = successorMachineID.String
	}
	if err != nil {
		// No successor found — can't recover; treat as stale replay
		h.log.Warn("Grace period replay but no successor token found",
			"user_id", userID,
			"revoked_token_id", revokedTokenID,
		)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid refresh token"})
		return
	}

	// Revoke the successor (it was never delivered to the client)
	_, err = h.db.Exec(
		`UPDATE refresh_tokens SET revoked_at = NOW(), last_used_at = NOW() WHERE id = $1`,
		successor.ID,
	)
	if err != nil {
		h.log.Error("Failed to revoke successor token during grace recovery", "error", err)
	}

	// Generate a fresh refresh token
	newRefreshToken, err := GenerateRefreshToken()
	if err != nil {
		h.log.Error("Failed to generate refresh token during grace recovery", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Refresh failed"})
		return
	}

	// Determine expiry
	newExpiry := successor.ExpiresAt
	cookieMaxAge := 0
	if successor.RememberMe {
		newExpiry = time.Now().Add(30 * 24 * time.Hour)
		cookieMaxAge = 30 * 24 * 60 * 60
	}

	// Store the new token (propagate machine_id)
	newTokenHash := HashRefreshToken(newRefreshToken)
	newTokenID := uuid.New().String()
	graceMachineID := c.GetHeader("X-Machine-Id")
	if graceMachineID == "" {
		graceMachineID = successor.MachineID
	}
	_, err = h.db.Exec(
		`INSERT INTO refresh_tokens (id, user_id, token_hash, device_name, ip_address, user_agent, expires_at, remember_me, machine_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		newTokenID,
		userID,
		newTokenHash,
		successor.DeviceName,
		c.ClientIP(),
		truncateUserAgent(c.GetHeader("User-Agent")),
		newExpiry,
		successor.RememberMe,
		nilIfEmpty(graceMachineID),
	)
	if err != nil {
		h.log.Error("Failed to store new refresh token during grace recovery", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Refresh failed"})
		return
	}

	// Set cookie
	setRefreshCookie(c, newRefreshToken, cookieMaxAge)

	// Look up current email_verified for grace recovery token
	var graceEmailVerified bool
	if err := h.db.QueryRow(`SELECT email_verified FROM users WHERE id = $1`, userID).Scan(&graceEmailVerified); err != nil {
		h.log.Error("Failed to look up email_verified for grace recovery", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Refresh failed"})
		return
	}

	// Generate access token
	accessToken, err := GenerateAccessToken(userID, h.jwtSecret, graceEmailVerified)
	if err != nil {
		h.log.Error("Failed to generate access token during grace recovery", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Refresh failed"})
		return
	}

	h.log.Info("Session recovered via grace period refresh",
		"user_id", userID,
		"new_session_id", newTokenID,
	)

	c.JSON(http.StatusOK, gin.H{
		"access_token":  accessToken,
		"refresh_token": newRefreshToken,
		"session_id":    newTokenID,
		"expires_in":    900,
	})
}

// triggerTheftKeyRevocations inserts key_revocations records for all E2EE channels
// that the compromised user has access to. This is called when token theft is detected
// (different machine_id + different IP) because the attacker's session could have
// fetched wrapped channel keys.
func (h *Handler) triggerTheftKeyRevocations(userID string) {
	// Find all encrypted channels the user is a member of (via server membership + channel_keys)
	rows, err := h.db.Query(
		`SELECT DISTINCT ck.channel_id, COALESCE(MAX(ck.key_version), 1) AS max_epoch
		 FROM channel_keys ck
		 JOIN channels ch ON ch.id = ck.channel_id
		 WHERE ck.user_id = $1 AND ch.is_encrypted = TRUE
		 GROUP BY ck.channel_id`,
		userID,
	)
	if err != nil {
		h.log.Error("Failed to query channels for theft key revocation", "error", err, "user_id", userID)
		return
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var channelID string
		var maxEpoch int
		if err := rows.Scan(&channelID, &maxEpoch); err != nil {
			h.log.Error("Failed to scan channel for theft key revocation", "error", err)
			continue
		}

		// Insert key_revocations record (ignore conflict — may already be revoked)
		_, err := h.db.Exec(
			`INSERT INTO key_revocations (channel_id, revoked_epoch, successor_epoch, reason, revoked_by)
			 VALUES ($1, $2, $3, 'theft_detected', $4)
			 ON CONFLICT (channel_id, revoked_epoch) DO NOTHING`,
			channelID, maxEpoch, maxEpoch+1, userID,
		)
		if err != nil {
			h.log.Error("Failed to insert key revocation for theft", "error", err, "channel_id", channelID)
		}
	}

	h.log.Info("Key revocations triggered for theft detection", "user_id", userID)
}

// Logout handles session termination
func (h *Handler) Logout(c *gin.Context) {
	// Blacklist the current access token if provided (best-effort)
	if authHeader := c.GetHeader("Authorization"); authHeader != "" {
		if parts := strings.SplitN(authHeader, " ", 2); len(parts) == 2 {
			if claims, err := ValidateAccessToken(parts[1], h.jwtSecret); err == nil && claims.ID != "" {
				remaining := time.Until(claims.ExpiresAt.Time)
				ctx := context.Background()
				if blErr := middleware.BlacklistToken(ctx, h.redis, claims.ID, remaining); blErr != nil {
					h.log.Error("Failed to blacklist access token", "error", blErr)
				}
			}
		}
	}

	// Get refresh token from cookie or header
	refreshToken, err := c.Cookie("refresh_token")
	if err != nil || refreshToken == "" {
		refreshToken = c.GetHeader("X-Refresh-Token")
	}
	if refreshToken == "" {
		c.JSON(http.StatusOK, gin.H{"message": "Already logged out"})
		return
	}

	// Soft-delete: mark session as revoked so it appears in "Past Sessions"
	tokenHash := HashRefreshToken(refreshToken)
	var revokedUserID string
	_ = h.db.QueryRow(`UPDATE refresh_tokens SET revoked_at = NOW() WHERE token_hash = $1 RETURNING user_id`, tokenHash).Scan(&revokedUserID)

	// Force-close any active WebSocket connections for this user.
	// The session_revoked message is a courtesy — the real enforcement is
	// TCP connection termination via handleUnregister.
	if revokedUserID != "" {
		if uid, parseErr := uuid.Parse(revokedUserID); parseErr == nil {
			h.hub.DisconnectUser(uid)
		}
	}

	// Clear cookie
	setRefreshCookie(c, "", -1)

	c.JSON(http.StatusOK, gin.H{"message": "Logged out successfully"})
}

// ── Account Recovery Endpoints ──────────────────────────────────────────────

const (
	recoveryCodeTTL       = 10 * time.Minute // Recovery code expires after 10 minutes
	recoveryMaxAttempts   = 5                // Max wrong-code attempts before invalidating
	recoveryTokenUsedTTL  = 25 * time.Hour   // Must match or exceed mfa.recoveryTTL
)

// recoveryRecord is stored in Redis keyed by "recovery_code:{email}".
type recoveryRecord struct {
	CodeHash string `json:"code_hash"`
	UserID   string `json:"user_id"`
	Attempts int    `json:"attempts"`
}

// generateRecoveryCode produces a cryptographically random 6-digit numeric code.
func generateRecoveryCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// hashRecoveryCode returns the SHA-256 hex digest of a recovery code.
func hashRecoveryCode(code string) string {
	h := sha256.Sum256([]byte(code))
	return hex.EncodeToString(h[:])
}

// recoveryRedisKey returns the Redis key for a pending recovery code.
func recoveryRedisKey(email string) string {
	return fmt.Sprintf("recovery_code:%s", strings.ToLower(email))
}

// RecoveryBegin initiates account recovery by sending a verification code to the user's email.
func (h *Handler) RecoveryBegin(c *gin.Context) {
	var req struct {
		Email string `json:"email" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Email is required"})
		return
	}

	email := strings.ToLower(strings.TrimSpace(req.Email))

	// Always return 200 to prevent email enumeration
	successMsg := gin.H{"message": "If an account exists with that email, a recovery code has been sent"}

	// Look up user by email
	var userID string
	err := h.db.QueryRow(
		`SELECT id FROM users WHERE LOWER(email) = LOWER($1)`, email,
	).Scan(&userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// User not found — return success anyway (prevent enumeration)
			c.JSON(http.StatusOK, successMsg)
			return
		}
		// Real DB error — log it but still return 200 to prevent enumeration
		h.log.Error("Failed to look up user for recovery", "error", err)
		c.JSON(http.StatusOK, successMsg)
		return
	}

	// Generate 6-digit code
	code, err := generateRecoveryCode()
	if err != nil {
		h.log.Error("Failed to generate recovery code", "error", err)
		// Fall through to generic 200 — recovery won't proceed but attacker can't distinguish
		c.JSON(http.StatusOK, successMsg)
		return
	}

	// Store SHA-256 hash in Redis
	record := recoveryRecord{
		CodeHash: hashRecoveryCode(code),
		UserID:   userID,
		Attempts: 0,
	}
	data, err := json.Marshal(record)
	if err != nil {
		h.log.Error("Failed to marshal recovery record", "error", err)
		c.JSON(http.StatusOK, successMsg)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	if err := h.redis.Set(ctx, recoveryRedisKey(email), data, recoveryCodeTTL).Err(); err != nil {
		h.log.Error("Failed to store recovery code in Redis", "error", err)
		c.JSON(http.StatusOK, successMsg)
		return
	}

	// Send code via email
	if err := h.emailSvc.SendRecoveryCode(email, code); err != nil {
		h.log.Error("Failed to send recovery code email", "error", err, "email", email)
		// Still return success to prevent enumeration — the code is in Redis for retry
	}

	h.log.Info("Recovery code sent", "email", email)
	c.JSON(http.StatusOK, successMsg)
}

// RecoveryVerifyCode validates the recovery code and returns a recovery token plus recovery key data.
func (h *Handler) RecoveryVerifyCode(c *gin.Context) {
	var req struct {
		Email string `json:"email" binding:"required"`
		Code  string `json:"code" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Email and code are required"})
		return
	}

	email := strings.ToLower(strings.TrimSpace(req.Email))

	// Trim whitespace/dashes and validate format
	code := strings.TrimSpace(strings.ReplaceAll(req.Code, "-", ""))
	if len(code) != 6 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Recovery code must be 6 digits"})
		return
	}
	for _, ch := range code {
		if ch < '0' || ch > '9' {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Recovery code must be 6 digits"})
			return
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	redisKey := recoveryRedisKey(email)

	// Fetch stored record
	data, err := h.redis.Get(ctx, redisKey).Bytes()
	if err == redis.Nil {
		// Return the same error shape as a wrong code to prevent email enumeration
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired recovery code", "attempts_remaining": 0})
		return
	}
	if err != nil {
		h.log.Error("Failed to fetch recovery record from Redis", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Recovery verification failed"})
		return
	}

	var record recoveryRecord
	if err := json.Unmarshal(data, &record); err != nil {
		h.log.Error("Failed to unmarshal recovery record", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Recovery verification failed"})
		return
	}

	// Check attempt limit — return the same error shape to prevent enumeration
	if record.Attempts >= recoveryMaxAttempts {
		h.redis.Del(ctx, redisKey)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired recovery code", "attempts_remaining": 0})
		return
	}

	// Timing-safe comparison
	if subtle.ConstantTimeCompare([]byte(hashRecoveryCode(code)), []byte(record.CodeHash)) != 1 {
		// Increment attempts
		record.Attempts++
		updated, _ := json.Marshal(record)
		ttl := h.redis.TTL(ctx, redisKey).Val()
		if ttl > 0 {
			h.redis.Set(ctx, redisKey, updated, ttl)
		}
		c.JSON(http.StatusUnauthorized, gin.H{
			"error":              "Invalid or expired recovery code",
			"attempts_remaining": 0,
		})
		return
	}

	// Code matches — delete Redis key (single use)
	h.redis.Del(ctx, redisKey)

	// Generate recovery JWT token
	if h.mfaChecker == nil {
		h.log.Error("MFA checker not configured for recovery token generation")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Recovery verification failed"})
		return
	}
	recoveryToken, _, err := h.mfaChecker.GenerateRecoveryToken(record.UserID)
	if err != nil {
		h.log.Error("Failed to generate recovery token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Recovery verification failed"})
		return
	}

	// Build response
	resp := gin.H{
		"recovery_token": recoveryToken,
	}

	// Fetch recovery key data
	var recoveryWrappedPrivateKey, recoveryKeySalt []byte
	var recoveryWrappedPrefsKey, recoveryPrefsKeySalt []byte
	err = h.db.QueryRow(
		`SELECT recovery_wrapped_private_key, recovery_key_salt, recovery_wrapped_prefs_key, recovery_prefs_key_salt
		 FROM user_recovery_keys WHERE user_id = $1`, record.UserID,
	).Scan(&recoveryWrappedPrivateKey, &recoveryKeySalt, &recoveryWrappedPrefsKey, &recoveryPrefsKeySalt)
	if err == nil {
		resp["has_recovery_key"] = true
		resp["recovery_wrapped_private_key"] = base64.StdEncoding.EncodeToString(recoveryWrappedPrivateKey)
		resp["recovery_key_salt"] = base64.StdEncoding.EncodeToString(recoveryKeySalt)
		if len(recoveryWrappedPrefsKey) > 0 {
			resp["recovery_wrapped_prefs_key"] = base64.StdEncoding.EncodeToString(recoveryWrappedPrefsKey)
		}
		if len(recoveryPrefsKeySalt) > 0 {
			resp["recovery_prefs_key_salt"] = base64.StdEncoding.EncodeToString(recoveryPrefsKeySalt)
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		resp["has_recovery_key"] = false
	} else {
		h.log.Error("Failed to load recovery key data", "error", err, "user_id", record.UserID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Recovery verification failed"})
		return
	}

	// Check for trusted devices (Phase B — table may not exist yet)
	var trustedDeviceCount int
	err = h.db.QueryRow(`SELECT COUNT(*) FROM trusted_recovery_devices WHERE user_id = $1`, record.UserID).Scan(&trustedDeviceCount)
	if err != nil {
		// Table may not exist yet — non-fatal
		resp["has_trusted_devices"] = false
	} else {
		resp["has_trusted_devices"] = trustedDeviceCount > 0
	}

	// Check for recovery circles (Phase C — table may not exist yet)
	var recoveryCircleCount int
	err = h.db.QueryRow(`SELECT COUNT(*) FROM recovery_circles WHERE user_id = $1`, record.UserID).Scan(&recoveryCircleCount)
	if err != nil {
		// Table may not exist yet — non-fatal
		resp["has_recovery_circle"] = false
	} else {
		resp["has_recovery_circle"] = recoveryCircleCount > 0
	}

	h.log.Info("Recovery code verified, token issued", "user_id", record.UserID)
	c.JSON(http.StatusOK, resp)
}

// RecoveryResetPassword resets the user's password using a recovery token and re-wrapped keys.
func (h *Handler) RecoveryResetPassword(c *gin.Context) {
	var req struct {
		RecoveryToken            string `json:"recovery_token" binding:"required"`
		NewPassword              string `json:"new_password" binding:"required"`
		WrappedPrivateKey        string `json:"wrapped_private_key" binding:"required"`
		KeyDerivationSalt        string `json:"key_derivation_salt" binding:"required"`
		KeyDerivationAlg         string `json:"key_derivation_alg" binding:"required"`
		RecoveryWrappedPrivateKey string `json:"recovery_wrapped_private_key"`
		RecoveryKeySalt          string `json:"recovery_key_salt"`
		RecoveryWrappedPrefsKey  string `json:"recovery_wrapped_prefs_key"`
		RecoveryPrefsKeySalt     string `json:"recovery_prefs_key_salt"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	// Validate recovery token
	if h.mfaChecker == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Recovery not configured"})
		return
	}
	claims, err := h.mfaChecker.ValidateRecoveryToken(req.RecoveryToken)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired recovery token"})
		return
	}

	// Enforce single-use recovery token
	ctx := c.Request.Context()
	recoveryUsedKey := fmt.Sprintf("recovery_token_used:%s", claims.JTI)
	_, err = h.redis.SetArgs(ctx, recoveryUsedKey, "1", redis.SetArgs{TTL: recoveryTokenUsedTTL, Mode: "NX"}).Result()
	if errors.Is(err, redis.Nil) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Recovery token has already been used"})
		return
	}
	if err != nil {
		h.log.Error("Failed to check recovery token usage", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process recovery"})
		return
	}

	// Validate new password strength
	if err := ValidatePasswordStrength(req.NewPassword); err != nil {
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Hash new password
	passwordHash, err := HashPassword(req.NewPassword)
	if err != nil {
		h.log.Error("Failed to hash new password", "error", err)
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to reset password"})
		return
	}

	// Decode E2EE key fields
	wrappedKey, err := base64.StdEncoding.DecodeString(req.WrappedPrivateKey)
	if err != nil {
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid wrapped_private_key format"})
		return
	}
	kdSalt, err := base64.StdEncoding.DecodeString(req.KeyDerivationSalt)
	if err != nil {
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid key_derivation_salt format"})
		return
	}

	// BEGIN TRANSACTION
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		h.log.Error(errMsgFailedStartTransaction, "error", err)
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to reset password"})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error(errMsgFailedRollbackTx, "error", rbErr)
		}
	}()

	// 1. Update password
	_, err = tx.Exec(`UPDATE users SET password_hash = $1, updated_at = NOW() WHERE id = $2`, passwordHash, claims.UserID)
	if err != nil {
		h.log.Error("Failed to update password", "error", err, "user_id", claims.UserID)
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to reset password"})
		return
	}

	// 2. Update E2EE keys
	_, err = tx.Exec(`
		UPDATE user_keys SET wrapped_private_key = $1, key_derivation_salt = $2, key_derivation_alg = $3,
		       key_version = key_version + 1, updated_at = NOW()
		WHERE user_id = $4
	`, wrappedKey, kdSalt, req.KeyDerivationAlg, claims.UserID)
	if err != nil {
		h.log.Error("Failed to update user keys", "error", err, "user_id", claims.UserID)
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to reset password"})
		return
	}

	// 3. If recovery key fields provided, upsert recovery key
	if req.RecoveryWrappedPrivateKey != "" && req.RecoveryKeySalt != "" {
		recKey, decErr := base64.StdEncoding.DecodeString(req.RecoveryWrappedPrivateKey)
		if decErr != nil {
			h.redis.Del(ctx, recoveryUsedKey)
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid recovery_wrapped_private_key format"})
			return
		}
		recSalt, decErr := base64.StdEncoding.DecodeString(req.RecoveryKeySalt)
		if decErr != nil {
			h.redis.Del(ctx, recoveryUsedKey)
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid recovery_key_salt format"})
			return
		}

		var recPrefsKey, recPrefsSalt []byte
		if req.RecoveryWrappedPrefsKey != "" {
			recPrefsKey, decErr = base64.StdEncoding.DecodeString(req.RecoveryWrappedPrefsKey)
			if decErr != nil {
				h.redis.Del(ctx, recoveryUsedKey)
				c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid recovery_wrapped_prefs_key format"})
				return
			}
		}
		if req.RecoveryPrefsKeySalt != "" {
			recPrefsSalt, decErr = base64.StdEncoding.DecodeString(req.RecoveryPrefsKeySalt)
			if decErr != nil {
				h.redis.Del(ctx, recoveryUsedKey)
				c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid recovery_prefs_key_salt format"})
				return
			}
		}

		_, err = tx.Exec(`
			INSERT INTO user_recovery_keys (user_id, recovery_wrapped_private_key, recovery_key_salt, recovery_wrapped_prefs_key, recovery_prefs_key_salt)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (user_id) DO UPDATE SET
				recovery_wrapped_private_key = EXCLUDED.recovery_wrapped_private_key,
				recovery_key_salt = EXCLUDED.recovery_key_salt,
				recovery_wrapped_prefs_key = EXCLUDED.recovery_wrapped_prefs_key,
				recovery_prefs_key_salt = EXCLUDED.recovery_prefs_key_salt,
				updated_at = NOW()
		`, claims.UserID, recKey, recSalt, recPrefsKey, recPrefsSalt)
		if err != nil {
			h.log.Error("Failed to upsert recovery key", "error", err, "user_id", claims.UserID)
			h.redis.Del(ctx, recoveryUsedKey)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to reset password"})
			return
		}
	}

	// 4. Revoke all refresh tokens
	_, err = tx.Exec(`UPDATE refresh_tokens SET revoked_at = NOW() WHERE user_id = $1 AND revoked_at IS NULL`, claims.UserID)
	if err != nil {
		h.log.Error("Failed to revoke refresh tokens", "error", err, "user_id", claims.UserID)
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to reset password"})
		return
	}

	// COMMIT
	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit transaction", "error", err, "user_id", claims.UserID)
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to reset password"})
		return
	}

	// Disconnect user's WebSocket sessions
	h.hub.DisconnectUser(uuid.MustParse(claims.UserID))

	h.log.Info("Password reset via recovery", "user_id", claims.UserID)
	c.JSON(http.StatusOK, gin.H{"message": "Password reset successfully. Please sign in with your new password."})
}

// RecoveryResetAccount performs a full account reset (new keys, all encrypted data lost).
func (h *Handler) RecoveryResetAccount(c *gin.Context) {
	var req struct {
		RecoveryToken        string `json:"recovery_token" binding:"required"`
		NewPassword          string `json:"new_password" binding:"required"`
		WrappedPrivateKey    string `json:"wrapped_private_key" binding:"required"`
		KeyDerivationSalt    string `json:"key_derivation_salt" binding:"required"`
		KeyDerivationAlg     string `json:"key_derivation_alg" binding:"required"`
		PublicKey            string `json:"public_key" binding:"required"`
		AcknowledgeDataLoss  bool   `json:"acknowledge_data_loss"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	// REQUIRE explicit acknowledgment of data loss
	if !req.AcknowledgeDataLoss {
		c.JSON(http.StatusBadRequest, gin.H{"error": "You must acknowledge that all encrypted message history will be permanently lost"})
		return
	}

	// Validate recovery token
	if h.mfaChecker == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Recovery not configured"})
		return
	}
	claims, err := h.mfaChecker.ValidateRecoveryToken(req.RecoveryToken)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired recovery token"})
		return
	}

	// Enforce single-use recovery token
	ctx := c.Request.Context()
	recoveryUsedKey := fmt.Sprintf("recovery_token_used:%s", claims.JTI)
	_, err = h.redis.SetArgs(ctx, recoveryUsedKey, "1", redis.SetArgs{TTL: recoveryTokenUsedTTL, Mode: "NX"}).Result()
	if errors.Is(err, redis.Nil) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Recovery token has already been used"})
		return
	}
	if err != nil {
		h.log.Error("Failed to check recovery token usage", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process recovery"})
		return
	}

	// Validate new password strength
	if err := ValidatePasswordStrength(req.NewPassword); err != nil {
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Hash new password
	passwordHash, err := HashPassword(req.NewPassword)
	if err != nil {
		h.log.Error("Failed to hash new password", "error", err)
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to reset account"})
		return
	}

	// Decode E2EE key fields
	wrappedKey, err := base64.StdEncoding.DecodeString(req.WrappedPrivateKey)
	if err != nil {
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid wrapped_private_key format"})
		return
	}
	kdSalt, err := base64.StdEncoding.DecodeString(req.KeyDerivationSalt)
	if err != nil {
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid key_derivation_salt format"})
		return
	}
	publicKey, err := base64.StdEncoding.DecodeString(req.PublicKey)
	if err != nil {
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid public_key format"})
		return
	}

	// BEGIN TRANSACTION
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		h.log.Error(errMsgFailedStartTransaction, "error", err)
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to reset account"})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error(errMsgFailedRollbackTx, "error", rbErr)
		}
	}()

	// 1. Update password
	_, err = tx.Exec(`UPDATE users SET password_hash = $1, updated_at = NOW() WHERE id = $2`, passwordHash, claims.UserID)
	if err != nil {
		h.log.Error("Failed to update password", "error", err, "user_id", claims.UserID)
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to reset account"})
		return
	}

	// 2. Update E2EE wrapped private key
	_, err = tx.Exec(`
		UPDATE user_keys SET wrapped_private_key = $1, key_derivation_salt = $2, key_derivation_alg = $3,
		       key_version = key_version + 1, updated_at = NOW()
		WHERE user_id = $4
	`, wrappedKey, kdSalt, req.KeyDerivationAlg, claims.UserID)
	if err != nil {
		h.log.Error("Failed to update user keys", "error", err, "user_id", claims.UserID)
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to reset account"})
		return
	}

	// 3. Replace public key (new identity)
	_, err = tx.Exec(`
		UPDATE public_keys SET public_key = $1, key_version = key_version + 1, created_at = NOW()
		WHERE user_id = $2
	`, publicKey, claims.UserID)
	if err != nil {
		h.log.Error("Failed to update public key", "error", err, "user_id", claims.UserID)
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to reset account"})
		return
	}

	// 4. Delete recovery keys
	_, err = tx.Exec(`DELETE FROM user_recovery_keys WHERE user_id = $1`, claims.UserID)
	if err != nil {
		h.log.Error("Failed to delete recovery keys", "error", err, "user_id", claims.UserID)
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to reset account"})
		return
	}

	// 5. Delete channel keys (encrypted data is now inaccessible)
	_, err = tx.Exec(`DELETE FROM channel_keys WHERE user_id = $1`, claims.UserID)
	if err != nil {
		h.log.Error("Failed to delete channel keys", "error", err, "user_id", claims.UserID)
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to reset account"})
		return
	}

	// 6. Delete DM channel keys
	_, err = tx.Exec(`DELETE FROM dm_channel_keys WHERE user_id = $1`, claims.UserID)
	if err != nil {
		h.log.Error("Failed to delete DM channel keys", "error", err, "user_id", claims.UserID)
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to reset account"})
		return
	}

	// 7. Revoke all refresh tokens
	_, err = tx.Exec(`UPDATE refresh_tokens SET revoked_at = NOW() WHERE user_id = $1 AND revoked_at IS NULL`, claims.UserID)
	if err != nil {
		h.log.Error("Failed to revoke refresh tokens", "error", err, "user_id", claims.UserID)
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to reset account"})
		return
	}

	// COMMIT
	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit transaction", "error", err, "user_id", claims.UserID)
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to reset account"})
		return
	}

	// Disconnect user's WebSocket sessions
	h.hub.DisconnectUser(uuid.MustParse(claims.UserID))

	h.log.Info("Account reset via recovery (data loss acknowledged)", "user_id", claims.UserID)
	c.JSON(http.StatusOK, gin.H{"message": "Account reset successfully. All encrypted message history has been permanently lost. Please sign in with your new password."})
}

// ── Trusted Device Recovery Endpoints ───────────────────────────────────────

// CreateDeviceRecoveryRequest initiates a trusted-device recovery request.
// This is an unauthenticated endpoint — the caller must provide a valid recovery token.
func (h *Handler) CreateDeviceRecoveryRequest(c *gin.Context) {
	var req struct {
		RecoveryToken      string `json:"recovery_token" binding:"required"`
		EphemeralPublicKey string `json:"ephemeral_public_key" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "recovery_token and ephemeral_public_key are required"})
		return
	}

	// Validate recovery token
	if h.mfaChecker == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Recovery not configured"})
		return
	}
	claims, err := h.mfaChecker.ValidateRecoveryToken(req.RecoveryToken)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired recovery token"})
		return
	}

	ctx := c.Request.Context()

	// Check user has trusted devices
	var deviceCount int
	err = h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM trusted_recovery_devices WHERE user_id = $1`, claims.UserID,
	).Scan(&deviceCount)
	if err != nil {
		h.log.Error("Failed to check trusted devices", "error", err, "user_id", claims.UserID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create recovery request"})
		return
	}
	if deviceCount == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No trusted devices configured"})
		return
	}

	// Base64-decode ephemeral public key
	ephPubKey, err := base64.StdEncoding.DecodeString(req.EphemeralPublicKey)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid ephemeral_public_key format (must be base64)"})
		return
	}

	// Insert recovery request with 15-minute expiry
	var requestID string
	err = h.db.QueryRowContext(ctx, `
		INSERT INTO recovery_requests (user_id, recovery_token_jti, ephemeral_public_key, expires_at)
		VALUES ($1, $2, $3, NOW() + INTERVAL '15 minutes')
		RETURNING id
	`, claims.UserID, claims.JTI, ephPubKey).Scan(&requestID)
	if err != nil {
		h.log.Error("Failed to create recovery request", "error", err, "user_id", claims.UserID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create recovery request"})
		return
	}

	// TODO: Send WebSocket notification to the user's trusted device sessions via
	// hub.BroadcastToUser(uuid.MustParse(claims.UserID), websocket.OutgoingMessage{
	//     Type: "recovery_request",
	//     Data: map[string]interface{}{"request_id": requestID},
	// })
	// Currently the auth handler's hub field is a SessionDisconnector interface which
	// does not expose BroadcastToUser. The client can poll via GET /recovery/device-request/:id
	// as a fallback until the interface is extended.

	h.log.Info("Device recovery request created", "user_id", claims.UserID, "request_id", requestID)
	c.JSON(http.StatusOK, gin.H{"request_id": requestID})
}

// PollDeviceRecoveryRequest polls the status of a trusted-device recovery request.
// This is an unauthenticated endpoint — the caller must provide a valid recovery token.
func (h *Handler) PollDeviceRecoveryRequest(c *gin.Context) {
	requestID := c.Param("id")

	// Prefer Authorization: Bearer header; fall back to query param
	recoveryToken := ""
	if authHeader := c.GetHeader("Authorization"); strings.HasPrefix(authHeader, "Bearer ") {
		recoveryToken = strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	} else {
		recoveryToken = c.Query("recovery_token")
	}
	if recoveryToken == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Recovery token is required (Authorization: Bearer header or recovery_token query parameter)"})
		return
	}

	// Validate recovery token
	if h.mfaChecker == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Recovery not configured"})
		return
	}
	claims, err := h.mfaChecker.ValidateRecoveryToken(recoveryToken)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired recovery token"})
		return
	}

	// Fetch the recovery request (JTI-scoped to prevent cross-token access)
	var status string
	var encryptedPayload, responderPublicKey []byte
	var expiresAt time.Time
	err = h.db.QueryRowContext(c.Request.Context(), `
		SELECT status, encrypted_payload, responder_public_key, expires_at
		FROM recovery_requests
		WHERE id = $1 AND user_id = $2 AND recovery_token_jti = $3
	`, requestID, claims.UserID, claims.JTI).Scan(&status, &encryptedPayload, &responderPublicKey, &expiresAt)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Recovery request not found"})
		return
	}

	// Check if expired
	if time.Now().After(expiresAt) && status == "pending" {
		c.JSON(http.StatusOK, gin.H{"status": "expired"})
		return
	}

	response := gin.H{"status": status}
	if status == "approved" && encryptedPayload != nil {
		response["encrypted_payload"] = base64.StdEncoding.EncodeToString(encryptedPayload)
	}
	if status == "approved" && responderPublicKey != nil {
		response["responder_public_key"] = base64.StdEncoding.EncodeToString(responderPublicKey)
	}

	c.JSON(http.StatusOK, response)
}

// ── Social Recovery Request Endpoints ────────────────────────────────────────

// CreateSocialRecoveryRequest creates a social recovery request for Shamir secret sharing.
// This is an unauthenticated endpoint — the caller provides a valid recovery token.
func (h *Handler) CreateSocialRecoveryRequest(c *gin.Context) {
	var req struct {
		RecoveryToken      string `json:"recovery_token" binding:"required"`
		EphemeralPublicKey string `json:"ephemeral_public_key" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "recovery_token and ephemeral_public_key are required"})
		return
	}

	// Validate recovery token
	if h.mfaChecker == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Recovery not configured"})
		return
	}
	claims, err := h.mfaChecker.ValidateRecoveryToken(req.RecoveryToken)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired recovery token"})
		return
	}

	ctx := c.Request.Context()

	// Find user's recovery circle
	var circleID string
	var thresholdK int
	err = h.db.QueryRowContext(ctx,
		`SELECT id, threshold_k FROM recovery_circles WHERE user_id = $1`, claims.UserID,
	).Scan(&circleID, &thresholdK)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No social recovery circle configured"})
		return
	}

	// Base64-decode ephemeral public key
	ephPubKey, err := base64.StdEncoding.DecodeString(req.EphemeralPublicKey)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid ephemeral_public_key format (must be base64)"})
		return
	}

	// Insert social recovery request with 24-hour expiry
	var requestID string
	err = h.db.QueryRowContext(ctx, `
		INSERT INTO recovery_circle_requests (circle_id, user_id, recovery_token_jti, ephemeral_public_key, expires_at)
		VALUES ($1, $2, $3, $4, NOW() + INTERVAL '24 hours')
		RETURNING id
	`, circleID, claims.UserID, claims.JTI, ephPubKey).Scan(&requestID)
	if err != nil {
		h.log.Error("Failed to create social recovery request", "error", err, "user_id", claims.UserID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create social recovery request"})
		return
	}

	h.log.Info("Social recovery request created", "user_id", claims.UserID, "request_id", requestID, "threshold_k", thresholdK)
	c.JSON(http.StatusOK, gin.H{"request_id": requestID, "threshold_k": thresholdK})
}

// PollSocialRecoveryRequest polls the status of a social recovery request.
// This is an unauthenticated endpoint — the caller provides a valid recovery token.
func (h *Handler) PollSocialRecoveryRequest(c *gin.Context) {
	requestID := c.Param("id")

	// Prefer Authorization: Bearer header; fall back to query param
	recoveryToken := ""
	if authHeader := c.GetHeader("Authorization"); strings.HasPrefix(authHeader, "Bearer ") {
		recoveryToken = strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	} else {
		recoveryToken = c.Query("recovery_token")
	}
	if recoveryToken == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Recovery token is required (Authorization: Bearer header or recovery_token query parameter)"})
		return
	}

	// Validate recovery token
	if h.mfaChecker == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Recovery not configured"})
		return
	}
	claims, err := h.mfaChecker.ValidateRecoveryToken(recoveryToken)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired recovery token"})
		return
	}

	ctx := c.Request.Context()

	// Fetch the social recovery request (JTI-scoped to prevent cross-token access)
	var status string
	var sharesReceived, thresholdK int
	var expiresAt time.Time
	err = h.db.QueryRowContext(ctx, `
		SELECT rr.status, rr.shares_received, rc.threshold_k, rr.expires_at
		FROM recovery_circle_requests rr
		JOIN recovery_circles rc ON rc.id = rr.circle_id
		WHERE rr.id = $1 AND rr.user_id = $2 AND rr.recovery_token_jti = $3
	`, requestID, claims.UserID, claims.JTI).Scan(&status, &sharesReceived, &thresholdK, &expiresAt)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Social recovery request not found"})
		return
	}

	// Check if expired
	if time.Now().After(expiresAt) && status == "pending" {
		c.JSON(http.StatusOK, gin.H{"status": "expired"})
		return
	}

	response := gin.H{
		"status":          status,
		"shares_received": sharesReceived,
		"threshold_k":     thresholdK,
	}

	// If complete, include all responses
	if status == "complete" {
		rows, err := h.db.QueryContext(ctx, `
			SELECT contact_id, encrypted_share
			FROM recovery_circle_responses
			WHERE request_id = $1
		`, requestID)
		if err != nil {
			h.log.Error("Failed to query social recovery responses", "error", err, "request_id", requestID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load recovery responses"})
			return
		}
		defer func() { _ = rows.Close() }()

		type responseEntry struct {
			ContactID      string `json:"contact_id"`
			EncryptedShare string `json:"encrypted_share"`
		}

		responses := []responseEntry{}
		for rows.Next() {
			var r responseEntry
			var encShare []byte
			if err := rows.Scan(&r.ContactID, &encShare); err != nil {
				h.log.Error("Failed to scan social recovery response", "error", err)
				continue
			}
			r.EncryptedShare = base64.StdEncoding.EncodeToString(encShare)
			responses = append(responses, r)
		}
		if err := rows.Err(); err != nil {
			h.log.Error("Error iterating social recovery responses", "error", err, "request_id", requestID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load recovery responses"})
			return
		}

		response["responses"] = responses
	}

	c.JSON(http.StatusOK, response)
}
