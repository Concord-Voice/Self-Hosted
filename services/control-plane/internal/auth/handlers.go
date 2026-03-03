// Package auth provides authentication and authorization functionality including user registration and login.
package auth

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/concord/control-plane/internal/middleware"
	"github.com/concord/control-plane/internal/models"
	"github.com/concord/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// SessionDisconnector allows forcefully disconnecting WebSocket connections.
// Defined as an interface to avoid an import cycle (websocket imports auth).
type SessionDisconnector interface {
	DisconnectUser(userID uuid.UUID)
}

// maxUserAgentLength limits User-Agent storage to reduce metadata footprint.
const maxUserAgentLength = 256

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
	db        *sql.DB
	redis     *redis.Client
	log       *logger.Logger
	jwtSecret string
	hub       SessionDisconnector
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

// RegisterRequest represents registration payload
type RegisterRequest struct {
	Email                string `json:"email" binding:"required,email"`
	Username             string `json:"username" binding:"required,min=3,max=50"`
	Password             string `json:"password" binding:"required"`             // #nosec G117 -- False positive: request field, not stored secret
	AgeConfirmation      bool   `json:"age_confirmation" binding:"required"`
	PublicKey            string `json:"public_key" binding:"required"`           // base64 encoded SPKI
	WrappedPrivateKey    string `json:"wrapped_private_key" binding:"required"` // base64 encoded
	KeyDerivationSalt    string `json:"key_derivation_salt" binding:"required"` // base64 encoded
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create account"})
		return
	}

	// Set refresh token as httpOnly cookie
	setRefreshCookie(c, refreshToken, 30*24*60*60)

	h.log.Info("User registered", "user_id", userID, "username", normalizedUsername)

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
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	ctx := c.Request.Context()

	// Check if account is locked out (generic message — don't reveal lockout)
	if h.checkLoginLockout(ctx, req.Email) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid credentials"})
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
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid credentials"})
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
		h.recordFailedLogin(ctx, req.Email)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid credentials"})
		return
	}

	// Clear failed login attempts on success
	h.clearLoginAttempts(ctx, req.Email)

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

	// Resolve remember_me preference (default: true for backward compat)
	rememberMe := true
	if req.RememberMe != nil {
		rememberMe = *req.RememberMe
	}

	// Store refresh token in database
	tokenHash := HashRefreshToken(refreshToken)
	tokenID := uuid.New().String()
	expiresAt := time.Now().Add(30 * 24 * time.Hour) // 30 days
	deviceName := c.GetHeader("X-Device-Name")
	ipAddress := c.ClientIP()
	userAgent := truncateUserAgent(c.GetHeader("User-Agent"))

	machineID := c.GetHeader("X-Machine-Id")

	// Revoke old sessions from the same device to prevent duplicates.
	// Prefer machine_id match when available; fall back to IP+UA match.
	if machineID != "" {
		_, _ = h.db.Exec(
			`UPDATE refresh_tokens SET revoked_at = NOW()
			 WHERE user_id = $1 AND machine_id = $2
			   AND revoked_at IS NULL`,
			user.ID, machineID,
		)
	} else {
		_, _ = h.db.Exec(
			`UPDATE refresh_tokens SET revoked_at = NOW()
			 WHERE user_id = $1 AND ip_address = $2 AND user_agent = $3
			   AND revoked_at IS NULL`,
			user.ID, ipAddress, userAgent,
		)
	}

	_, err = h.db.Exec(
		`INSERT INTO refresh_tokens (id, user_id, token_hash, device_name, ip_address, user_agent, expires_at, remember_me, machine_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		tokenID,
		user.ID,
		tokenHash,
		deviceName,
		ipAddress,
		userAgent,
		expiresAt,
		rememberMe,
		nilIfEmpty(machineID),
	)
	if err != nil {
		h.log.Error("Failed to store refresh token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Login failed"})
		return
	}

	// Cookie MaxAge: 30 days if remember_me, session-scoped otherwise
	cookieMaxAge := 30 * 24 * 60 * 60 // 30 days in seconds
	if !rememberMe {
		cookieMaxAge = 0 // Session cookie
	}

	// Set refresh token as httpOnly cookie
	setRefreshCookie(c, refreshToken, cookieMaxAge)

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

	h.log.Info("User logged in", "user_id", user.ID)

	// Return access token, refresh token, and user data.
	// The refresh token is returned in both a cookie and the body so that
	// Electron/native clients (file:// origin) can store it explicitly —
	// SameSite=Lax cookies are not sent on cross-site POST requests.
	c.JSON(http.StatusOK, gin.H{
		"access_token":  accessToken,
		"refresh_token": refreshToken,
		"session_id":    tokenID,
		"expires_in":    900, // 15 minutes
		"remember_me":   rememberMe,
		"user":          user.PublicUser(),
		"e2ee_keys": gin.H{
			"wrapped_private_key":  base64.StdEncoding.EncodeToString(keys.WrappedPrivateKey),
			"key_derivation_salt":  base64.StdEncoding.EncodeToString(keys.KeyDerivationSalt),
			"key_version":          keys.KeyVersion,
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
			// Same IP, different machine — pre-MFA: log warning, allow
			h.log.Warn("Refresh from different machine_id but same IP (pre-MFA: allowing)",
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

	// Generate new access token
	accessToken, err := GenerateAccessToken(token.UserID, h.jwtSecret)
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

	// Generate access token
	accessToken, err := GenerateAccessToken(userID, h.jwtSecret)
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
	defer rows.Close()

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
