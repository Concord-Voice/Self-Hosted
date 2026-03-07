package mfa

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/lib/pq"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/redis/go-redis/v9"
)

// LoginCompleter completes the login flow after MFA verification.
// Implemented by the auth handler to issue tokens and create sessions.
type LoginCompleter interface {
	CompleteLogin(c *gin.Context, userID string, rememberMe bool)
}

// Handler implements MFA API endpoints and the Verifier interface.
type Handler struct {
	db             *sql.DB
	redis          *redis.Client
	log            *logger.Logger
	encKey         []byte // 32-byte AES key for TOTP secret encryption
	jwtSecret      string
	webauthn       *WebAuthnService
	loginCompleter LoginCompleter
	environment    string // "development", "staging", "production"
}

// Ensure Handler implements Verifier at compile time.
var _ Verifier = (*Handler)(nil)

// NewHandler creates a new MFA handler.
func NewHandler(db *sql.DB, redisClient *redis.Client, log *logger.Logger, encKey []byte, jwtSecret string, webauthnSvc *WebAuthnService, environment string) *Handler {
	return &Handler{
		db:          db,
		redis:       redisClient,
		log:         log,
		encKey:      encKey,
		jwtSecret:   jwtSecret,
		webauthn:    webauthnSvc,
		environment: environment,
	}
}

// SetLoginCompleter sets the login completer (called after both handlers are initialized).
func (h *Handler) SetLoginCompleter(lc LoginCompleter) {
	h.loginCompleter = lc
}

// ── Verifier Interface Implementation ────────────────────────────────────────

// IsEnabled returns true if the user has any active MFA method.
func (h *Handler) IsEnabled(ctx context.Context, userID string) bool {
	var enabled bool
	err := h.db.QueryRowContext(ctx, `SELECT mfa_enabled FROM users WHERE id = $1`, userID).Scan(&enabled)
	return err == nil && enabled
}

// VerifyCode checks a TOTP code or backup code against the user's stored MFA secrets.
func (h *Handler) VerifyCode(ctx context.Context, userID string, code string) (bool, error) {
	// Check for WebAuthn inline verification token first (from WebAuthnVerifyInlineFinish)
	if len(code) > 20 {
		tokenKey := fmt.Sprintf("mfa_inline_token:%s:%s", userID, code)
		if h.redis.Exists(ctx, tokenKey).Val() > 0 {
			h.redis.Del(ctx, tokenKey) // Single-use: delete immediately
			return true, nil
		}
	}

	// Try TOTP
	var secretEnc, secretNonce []byte
	var totpEnabled, totpConfirmed bool
	err := h.db.QueryRowContext(ctx,
		`SELECT totp_secret_enc, totp_secret_nonce, enabled, confirmed FROM user_mfa_totp WHERE user_id = $1`,
		userID,
	).Scan(&secretEnc, &secretNonce, &totpEnabled, &totpConfirmed)

	if err == nil && totpEnabled && totpConfirmed {
		secret, decErr := DecryptSecret(secretEnc, secretNonce, h.encKey)
		if decErr != nil {
			h.log.Error("TOTP secret decryption failed — likely encryption key mismatch", "user_id", userID, "error", decErr)
			return false, fmt.Errorf("TOTP secret decryption failed: %w", decErr)
		}
		if ValidateCode(string(secret), code) {
			return true, nil
		}

		// Try as backup code
		var hashes []string
		var used []bool
		_ = h.db.QueryRowContext(ctx,
			`SELECT backup_codes_hash, backup_codes_used FROM user_mfa_totp WHERE user_id = $1`,
			userID,
		).Scan(pq.Array(&hashes), pq.Array(&used))

		if idx, matched := VerifyBackupCode(code, hashes, used); matched {
			// Mark backup code as used
			used[idx] = true
			_, _ = h.db.ExecContext(ctx,
				`UPDATE user_mfa_totp SET backup_codes_used = $1, updated_at = NOW() WHERE user_id = $2`,
				pq.Array(used), userID,
			)
			return true, nil
		}
	}

	return false, nil
}

// GetEnabledMethods returns the list of active MFA methods for a user.
func (h *Handler) GetEnabledMethods(ctx context.Context, userID string) ([]string, error) {
	var methods []string
	err := h.db.QueryRowContext(ctx, `SELECT mfa_methods FROM users WHERE id = $1`, userID).Scan(pq.Array(&methods))
	if err != nil {
		return nil, err
	}
	return methods, nil
}

// GetLoginMethods returns methods eligible for login and sensitive ops — excludes recovery-only methods.
// Recovery-only methods are like a spare key: they can unlock the door (account recovery) but can't start the engine (login).
func (h *Handler) GetLoginMethods(ctx context.Context, userID string) ([]string, error) {
	var methods, recoveryOnly []string
	err := h.db.QueryRowContext(ctx,
		`SELECT mfa_methods, recovery_only_methods FROM users WHERE id = $1`, userID,
	).Scan(pq.Array(&methods), pq.Array(&recoveryOnly))
	if err != nil {
		return nil, err
	}

	if len(recoveryOnly) == 0 {
		return methods, nil
	}

	// Filter out recovery-only methods
	excluded := make(map[string]bool, len(recoveryOnly))
	for _, m := range recoveryOnly {
		excluded[m] = true
	}
	var loginMethods []string
	for _, m := range methods {
		if !excluded[m] {
			loginMethods = append(loginMethods, m)
		}
	}
	return loginMethods, nil
}

// GenerateLoginChallenge creates a challenge token for two-step login and stores
// the remember_me preference in Redis keyed by JTI for retrieval after MFA verify.
func (h *Handler) GenerateLoginChallenge(ctx context.Context, userID string, rememberMe bool) (string, string, error) {
	token, jti, err := GenerateChallengeToken(userID, PurposeLogin, h.jwtSecret)
	if err != nil {
		return "", "", err
	}

	// Store remember_me in Redis so the MFA verify handler can complete login with it
	rememberVal := "0"
	if rememberMe {
		rememberVal = "1"
	}
	key := fmt.Sprintf("mfa_challenge:%s:remember_me", jti)
	h.redis.Set(ctx, key, rememberVal, challengeTTL)

	return token, jti, nil
}

// GenerateUpgradeChallenge creates a challenge token for pre-MFA session upgrades.
// On successful MFA verification, fresh tokens are issued (same as login).
func (h *Handler) GenerateUpgradeChallenge(ctx context.Context, userID string, rememberMe bool) (string, string, error) {
	token, jti, err := GenerateChallengeToken(userID, PurposeMFAUpgrade, h.jwtSecret)
	if err != nil {
		return "", "", err
	}

	rememberVal := "0"
	if rememberMe {
		rememberVal = "1"
	}
	key := fmt.Sprintf("mfa_challenge:%s:remember_me", jti)
	h.redis.Set(ctx, key, rememberVal, challengeTTL)

	return token, jti, nil
}

// BeginWebAuthnLogin starts a WebAuthn assertion ceremony for login.
// Stores session data in Redis keyed by the challenge JTI.
// Returns credential request options for the client, or nil if user has no WebAuthn credentials.
func (h *Handler) BeginWebAuthnLogin(ctx context.Context, userID string, jti string) (interface{}, error) {
	user, err := h.buildWebAuthnUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("build webauthn user: %w", err)
	}
	if len(user.WebAuthnCredentials()) == 0 {
		return nil, nil
	}

	assertion, session, err := h.webauthn.BeginLogin(user)
	if err != nil {
		return nil, fmt.Errorf("begin login: %w", err)
	}

	sessionJSON, _ := json.Marshal(session)
	sessionKey := fmt.Sprintf("mfa_webauthn_session:%s", jti)
	h.redis.Set(ctx, sessionKey, sessionJSON, challengeTTL)

	return assertion, nil
}

// ── Helper ───────────────────────────────────────────────────────────────────

func (h *Handler) verifyUserPassword(ctx context.Context, userID, password string) (bool, error) {
	var passwordHash string
	if err := h.db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = $1`, userID).Scan(&passwordHash); err != nil {
		return false, fmt.Errorf("fetch password hash: %w", err)
	}
	return auth.VerifyPassword(password, passwordHash)
}

func containsStr(ss []string, target string) bool {
	for _, s := range ss {
		if s == target {
			return true
		}
	}
	return false
}

// updateUserMFAFlags recalculates and updates the denormalized mfa_enabled and mfa_methods on users.
func (h *Handler) updateUserMFAFlags(ctx context.Context, userID string) error {
	methods := make([]string, 0) // must be non-nil for pq.Array to produce '{}' not NULL

	// Check TOTP
	var totpActive bool
	err := h.db.QueryRowContext(ctx,
		`SELECT enabled AND confirmed FROM user_mfa_totp WHERE user_id = $1`, userID,
	).Scan(&totpActive)
	if err == nil && totpActive {
		methods = append(methods, "totp")
	}

	// Check WebAuthn
	var webauthnCount int
	_ = h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM user_mfa_webauthn WHERE user_id = $1`, userID,
	).Scan(&webauthnCount)
	if webauthnCount > 0 {
		methods = append(methods, "webauthn")
	}

	// Check Email/SMS (dev stub — tracked via Redis flag)
	for _, m := range []string{"email", "sms"} {
		if h.redis.Exists(ctx, fmt.Sprintf("mfa_emailsms_enabled:%s:%s", userID, m)).Val() > 0 {
			methods = append(methods, m)
		}
	}

	enabled := len(methods) > 0

	if enabled {
		// Set mfa_enabled_at only on the first activation (NULL → NOW).
		// Pre-existing sessions created before this timestamp will be
		// challenged for MFA on their next refresh.
		_, err = h.db.ExecContext(ctx, `
			UPDATE users
			SET mfa_enabled = TRUE,
			    mfa_methods = $1,
			    mfa_enabled_at = COALESCE(mfa_enabled_at, NOW())
			WHERE id = $2
		`, pq.Array(methods), userID)
	} else {
		// MFA fully disabled — clear the timestamp so re-enabling later
		// sets a fresh one.
		_, err = h.db.ExecContext(ctx, `
			UPDATE users
			SET mfa_enabled = FALSE,
			    mfa_methods = $1,
			    mfa_enabled_at = NULL
			WHERE id = $2
		`, pq.Array(methods), userID)
	}
	return err
}

// requirePasswordAndMFA verifies the user's password and, if MFA is already active,
// also verifies an MFA code. Returns true if verification passed, false if a
// response was already written to c.
func (h *Handler) requirePasswordAndMFA(c *gin.Context, userID, password, mfaCode string) bool {
	ctx := c.Request.Context()

	match, err := h.verifyUserPassword(ctx, userID, password)
	if err != nil {
		h.log.Error("Password verification failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify password"})
		return false
	}
	if !match {
		c.JSON(http.StatusForbidden, gin.H{"error": "Incorrect password"})
		return false
	}

	// If MFA is already active, require an MFA code too
	if h.IsEnabled(ctx, userID) {
		if mfaCode == "" {
			c.JSON(http.StatusForbidden, gin.H{"error": "MFA code is required", "mfa_required": true})
			return false
		}
		ok, verifyErr := h.VerifyCode(ctx, userID, mfaCode)
		if verifyErr != nil || !ok {
			c.JSON(http.StatusForbidden, gin.H{"error": "Invalid MFA code"})
			return false
		}
	}

	return true
}

// ── TOTP Endpoints ───────────────────────────────────────────────────────────

// GetStatus returns the user's MFA status across all methods.
func (h *Handler) GetStatus(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	result := gin.H{
		"totp_enabled":           false,
		"totp_confirmed":         false,
		"webauthn_credentials":   0,
		"backup_codes_remaining": 0,
		"mfa_enabled":            false,
		"methods":                []string{},
	}

	// TOTP status
	var totpEnabled, totpConfirmed bool
	var backupUsed []bool
	var backupHashes []string
	err := h.db.QueryRowContext(ctx, `SELECT enabled, confirmed, backup_codes_hash, backup_codes_used FROM user_mfa_totp WHERE user_id = $1`,
		userID,
	).Scan(&totpEnabled, &totpConfirmed, pq.Array(&backupHashes), pq.Array(&backupUsed))
	if err == nil {
		result["totp_enabled"] = totpEnabled
		result["totp_confirmed"] = totpConfirmed
		remaining := 0
		for i, used := range backupUsed {
			if !used && i < len(backupHashes) {
				remaining++
			}
		}
		result["backup_codes_remaining"] = remaining
	}

	// WebAuthn credential count
	var webauthnCount int
	_ = h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_mfa_webauthn WHERE user_id = $1`, userID).Scan(&webauthnCount)
	result["webauthn_credentials"] = webauthnCount

	// Overall — read denormalized flags, then self-heal if stale
	var methods, recoveryOnly []string
	var mfaEnabled, recoveryHardened bool
	var backupEmail sql.NullString
	_ = h.db.QueryRowContext(ctx, `SELECT mfa_enabled, mfa_methods, recovery_only_methods, recovery_hardened, backup_email FROM users WHERE id = $1`, userID).Scan(&mfaEnabled, pq.Array(&methods), pq.Array(&recoveryOnly), &recoveryHardened, &backupEmail)

	// Self-heal: if denormalized flags disagree with actual table data, resync
	actualTOTP := totpEnabled && totpConfirmed
	actualWebAuthn := webauthnCount > 0
	denormTOTP := containsStr(methods, "totp")
	denormWebAuthn := containsStr(methods, "webauthn")
	if actualTOTP != denormTOTP || actualWebAuthn != denormWebAuthn {
		h.log.Warn("MFA flags out of sync, resyncing", "user_id", userID,
			"actual_totp", actualTOTP, "denorm_totp", denormTOTP,
			"actual_webauthn", actualWebAuthn, "denorm_webauthn", denormWebAuthn)
		if syncErr := h.updateUserMFAFlags(ctx, userID); syncErr != nil {
			h.log.Error("Failed to resync MFA flags", "user_id", userID, "error", syncErr)
		} else {
			// Re-read after sync
			_ = h.db.QueryRowContext(ctx, `SELECT mfa_enabled, mfa_methods FROM users WHERE id = $1`, userID).Scan(&mfaEnabled, pq.Array(&methods))
		}
	}

	result["mfa_enabled"] = mfaEnabled
	result["methods"] = methods
	result["recovery_only_methods"] = recoveryOnly
	result["recovery_hardened"] = recoveryHardened
	if backupEmail.Valid {
		result["backup_email"] = backupEmail.String
	} else {
		result["backup_email"] = ""
	}

	c.JSON(http.StatusOK, result)
}

// TOTPSetup initiates TOTP enrollment. Requires password confirmation (and MFA if already active).
func (h *Handler) TOTPSetup(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	var req struct {
		Password string `json:"password" binding:"required"` //nolint:gosec // request field, not a secret
		MFACode  string `json:"mfa_code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Password is required"})
		return
	}

	if !h.requirePasswordAndMFA(c, userID, req.Password, req.MFACode) {
		return
	}

	// Check if already has confirmed TOTP
	var existingConfirmed bool
	checkErr := h.db.QueryRowContext(ctx, `SELECT confirmed FROM user_mfa_totp WHERE user_id = $1`, userID).Scan(&existingConfirmed)
	if checkErr == nil && existingConfirmed {
		c.JSON(http.StatusConflict, gin.H{"error": "TOTP is already enabled. Disable it first to re-enroll."})
		return
	}

	// Get user email for the TOTP issuer label
	var email string
	if dbErr := h.db.QueryRowContext(ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email); dbErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch user"})
		return
	}

	// Generate TOTP secret
	key, err := GenerateSecret(email)
	if err != nil {
		h.log.Error("Failed to generate TOTP secret", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate MFA secret"})
		return
	}

	// Encrypt the secret for storage
	ciphertext, nonce, err := EncryptSecret([]byte(key.Secret()), h.encKey)
	if err != nil {
		h.log.Error("Failed to encrypt TOTP secret", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to secure MFA secret"})
		return
	}

	// Upsert (replace pending setup or insert new)

	_, err = h.db.ExecContext(ctx, `
		INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, enabled, confirmed)
		VALUES ($1, $2, $3, FALSE, FALSE)
		ON CONFLICT (user_id) DO UPDATE SET
			totp_secret_enc = EXCLUDED.totp_secret_enc,
			totp_secret_nonce = EXCLUDED.totp_secret_nonce,
			enabled = FALSE,
			confirmed = FALSE,
			verified_at = NULL,
			confirmed_at = NULL,
			backup_codes_hash = '{}',
			backup_codes_used = '{}',
			updated_at = NOW()
	`, userID, ciphertext, nonce)
	if err != nil {
		h.log.Error("Failed to store TOTP secret", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store MFA secret"})
		return
	}

	// Sync user flags — if re-enrolling, the upsert reset enabled/confirmed
	// to FALSE so mfa_enabled must be cleared until the new setup completes.
	if err := h.updateUserMFAFlags(ctx, userID); err != nil {
		h.log.Error("Failed to sync MFA flags after TOTP re-enrollment", "error", err)
	}

	c.JSON(http.StatusOK, gin.H{
		"otpauth_url": key.URL(),
		"secret":      key.Secret(),
	})
}

// TOTPVerifySetup validates a TOTP code to complete step 1 of enrollment.
// Returns backup codes but does NOT activate MFA yet (requires confirm-setup).
func (h *Handler) TOTPVerifySetup(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	var req struct {
		Code string `json:"code" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Code is required"})
		return
	}

	// Rate limit: check for MFA setup lockout
	lockoutKey := fmt.Sprintf("mfa_setup_lockout:%s", userID)
	if h.redis.Exists(ctx, lockoutKey).Val() > 0 {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "Too many failed attempts. Try again later."})
		return
	}

	// Fetch the pending TOTP secret
	var secretEnc, secretNonce []byte
	var enabled bool

	err := h.db.QueryRowContext(ctx,
		`SELECT totp_secret_enc, totp_secret_nonce, enabled FROM user_mfa_totp WHERE user_id = $1`,
		userID,
	).Scan(&secretEnc, &secretNonce, &enabled)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No TOTP setup in progress. Call /mfa/totp/setup first."})
		return
	}
	if enabled {
		c.JSON(http.StatusConflict, gin.H{"error": "TOTP is already verified"})
		return
	}

	// Decrypt and validate
	secret, err := DecryptSecret(secretEnc, secretNonce, h.encKey)
	if err != nil {
		h.log.Error("Failed to decrypt TOTP secret", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify code"})
		return
	}

	if !ValidateCode(string(secret), req.Code) {
		// Track failed attempts
		attemptsKey := fmt.Sprintf("mfa_setup_attempts:%s", userID)
		attempts := h.redis.Incr(ctx, attemptsKey).Val()
		h.redis.Expire(ctx, attemptsKey, 5*time.Minute)
		if attempts >= 5 {
			h.redis.Set(ctx, lockoutKey, "1", 15*time.Minute)
			h.redis.Del(ctx, attemptsKey)
		}
		c.JSON(http.StatusForbidden, gin.H{"error": "Invalid code"})
		return
	}

	// Code valid — generate backup codes
	codes, hashes, err := GenerateBackupCodes()
	if err != nil {
		h.log.Error("Failed to generate backup codes", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate backup codes"})
		return
	}

	// Mark as enabled (code verified) but NOT confirmed (backup codes not yet acknowledged)
	usedFlags := make([]bool, len(hashes))

	_, err = h.db.ExecContext(ctx, `
		UPDATE user_mfa_totp
		SET enabled = TRUE, verified_at = NOW(), backup_codes_hash = $1, backup_codes_used = $2, updated_at = NOW()
		WHERE user_id = $3
	`, pq.Array(hashes), pq.Array(usedFlags), userID)
	if err != nil {
		h.log.Error("Failed to update TOTP status", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to complete verification"})
		return
	}

	// Clear attempt tracking
	h.redis.Del(ctx, fmt.Sprintf("mfa_setup_attempts:%s", userID))

	c.JSON(http.StatusOK, gin.H{
		"backup_codes": codes,
		"message":      "TOTP verified. Save your backup codes, then call /mfa/totp/confirm-setup to activate MFA.",
	})
}

// TOTPConfirmSetup activates MFA after the user confirms they saved their backup codes.
func (h *Handler) TOTPConfirmSetup(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	// Verify TOTP is enabled (code verified) but not yet confirmed
	var enabled, confirmed bool

	err := h.db.QueryRowContext(ctx,
		`SELECT enabled, confirmed FROM user_mfa_totp WHERE user_id = $1`, userID,
	).Scan(&enabled, &confirmed)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No TOTP setup in progress"})
		return
	}
	if !enabled {
		c.JSON(http.StatusBadRequest, gin.H{"error": "TOTP code not yet verified. Complete verify-setup first."})
		return
	}
	if confirmed {
		c.JSON(http.StatusConflict, gin.H{"error": "TOTP MFA is already active"})
		return
	}

	// Activate MFA

	_, err = h.db.ExecContext(ctx, `
		UPDATE user_mfa_totp SET confirmed = TRUE, confirmed_at = NOW(), updated_at = NOW() WHERE user_id = $1
	`, userID)
	if err != nil {
		h.log.Error("Failed to confirm TOTP setup", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to activate MFA"})
		return
	}

	if err := h.updateUserMFAFlags(ctx, userID); err != nil {
		h.log.Error("Failed to update user MFA flags", "error", err)
	}

	c.JSON(http.StatusOK, gin.H{"message": "MFA is now active"})
}

// TOTPDisable disables TOTP MFA. Requires password + a valid MFA code.
func (h *Handler) TOTPDisable(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	var req struct {
		Password string `json:"password" binding:"required"` //nolint:gosec // request field, not a secret
		Code     string `json:"code" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Password and MFA code are required"})
		return
	}

	// Check if TOTP is actually enrolled before proceeding
	var totpExists bool
	err := h.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_mfa_totp WHERE user_id = $1)`, userID).Scan(&totpExists)
	if err != nil || !totpExists {
		// Row is gone but mfa_methods may still list 'totp' — clean up the denormalized flags
		if syncErr := h.updateUserMFAFlags(ctx, userID); syncErr != nil {
			h.log.Error("Failed to sync MFA flags after missing TOTP row", "error", syncErr)
		}
		c.JSON(http.StatusOK, gin.H{"message": "TOTP MFA has been disabled"})
		return
	}

	// Verify password
	match, err := h.verifyUserPassword(ctx, userID, req.Password)
	if err != nil || !match {
		c.JSON(http.StatusForbidden, gin.H{"error": "Incorrect password"})
		return
	}

	// Verify MFA code
	valid, err := h.VerifyCode(ctx, userID, req.Code)
	if err != nil {
		h.log.Error("MFA code verification error during TOTP disable", "user_id", userID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "MFA verification error. The server may have been restarted with a different encryption key."})
		return
	}
	if !valid {
		c.JSON(http.StatusForbidden, gin.H{"error": "Invalid MFA code. Make sure the code hasn't expired."})
		return
	}

	// Delete TOTP enrollment
	_, _ = h.db.ExecContext(ctx, `DELETE FROM user_mfa_totp WHERE user_id = $1`, userID)

	if err := h.updateUserMFAFlags(ctx, userID); err != nil {
		h.log.Error("Failed to update user MFA flags after TOTP disable", "error", err)
	}

	c.JSON(http.StatusOK, gin.H{"message": "TOTP MFA has been disabled"})
}

// RegenerateBackupCodes generates new backup codes. Requires password + TOTP code.
func (h *Handler) RegenerateBackupCodes(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	var req struct {
		Password string `json:"password" binding:"required"` //nolint:gosec // request field, not a secret
		Code     string `json:"code" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Password and TOTP code are required"})
		return
	}

	match, err := h.verifyUserPassword(ctx, userID, req.Password)
	if err != nil || !match {
		c.JSON(http.StatusForbidden, gin.H{"error": "Incorrect password"})
		return
	}

	// Verify the TOTP code (not backup code — must prove they have the authenticator)
	var secretEnc, secretNonce []byte
	var totpEnabled, totpConfirmed bool
	err = h.db.QueryRowContext(ctx,
		`SELECT totp_secret_enc, totp_secret_nonce, enabled, confirmed FROM user_mfa_totp WHERE user_id = $1`,
		userID,
	).Scan(&secretEnc, &secretNonce, &totpEnabled, &totpConfirmed)
	if err != nil || !totpEnabled || !totpConfirmed {
		c.JSON(http.StatusBadRequest, gin.H{"error": "TOTP is not enabled"})
		return
	}

	secret, decErr := DecryptSecret(secretEnc, secretNonce, h.encKey)
	if decErr != nil || !ValidateCode(string(secret), req.Code) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Invalid TOTP code"})
		return
	}

	// Generate new backup codes
	codes, hashes, err := GenerateBackupCodes()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate backup codes"})
		return
	}

	usedFlags := make([]bool, len(hashes))
	_, err = h.db.ExecContext(ctx, `
		UPDATE user_mfa_totp SET backup_codes_hash = $1, backup_codes_used = $2, updated_at = NOW() WHERE user_id = $3
	`, pq.Array(hashes), pq.Array(usedFlags), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store backup codes"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"backup_codes": codes})
}

// ── WebAuthn Endpoints ──────────────────────────────────────────────────────

// WebAuthnRegisterBegin starts WebAuthn credential registration.
func (h *Handler) WebAuthnRegisterBegin(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	var req struct {
		Password       string `json:"password" binding:"required"` //nolint:gosec // request field, not a secret
		MFACode        string `json:"mfa_code"`
		CredentialName string `json:"credential_name"`
		CredentialType string `json:"credential_type"` // "hardware" or "platform"
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Password is required"})
		return
	}

	if !h.requirePasswordAndMFA(c, userID, req.Password, req.MFACode) {
		return
	}

	// Enforce credential limit: 10 total WebAuthn credentials per user
	credType := req.CredentialType
	if credType != "platform" {
		credType = "hardware"
	}
	var totalCount int
	err := h.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM user_mfa_webauthn WHERE user_id = $1
	`, userID).Scan(&totalCount)
	if err != nil {
		h.log.Error("Failed to check credential counts", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to start registration"})
		return
	}
	if totalCount >= 10 {
		c.JSON(http.StatusConflict, gin.H{"error": "Maximum of 10 WebAuthn credentials reached. Remove an existing key first."})
		return
	}

	// Build WebAuthn user with existing credentials (for exclusion)
	user, err := h.buildWebAuthnUser(ctx, userID)
	if err != nil {
		h.log.Error("Failed to build WebAuthn user", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to start registration"})
		return
	}

	// Set authenticator selection based on credential type
	var regOpts []webauthn.RegistrationOption
	if credType == "platform" {
		regOpts = append(regOpts, webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			AuthenticatorAttachment: protocol.Platform,
			UserVerification:        protocol.VerificationPreferred,
		}))
	} else {
		regOpts = append(regOpts, webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			AuthenticatorAttachment: protocol.CrossPlatform,
			UserVerification:        protocol.VerificationPreferred,
		}))
	}

	creation, session, err := h.webauthn.BeginRegistration(user, regOpts...)
	if err != nil {
		h.log.Error("WebAuthn BeginRegistration failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to start registration"})
		return
	}

	// Store session data in Redis (keyed by user ID, 5-min TTL)
	sessionJSON, _ := json.Marshal(session)
	credName := req.CredentialName
	if credName == "" {
		credName = "Security Key"
	}
	// Store session + metadata together
	meta := map[string]interface{}{
		"session":         string(sessionJSON),
		"credential_name": credName,
		"credential_type": credType,
	}
	metaJSON, _ := json.Marshal(meta)
	h.redis.Set(ctx, fmt.Sprintf("webauthn_reg:%s", userID), metaJSON, 5*time.Minute)

	c.JSON(http.StatusOK, creation)
}

// WebAuthnRegisterFinish completes WebAuthn credential registration.
func (h *Handler) WebAuthnRegisterFinish(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	// Retrieve session data from Redis
	metaJSON, err := h.redis.Get(ctx, fmt.Sprintf("webauthn_reg:%s", userID)).Bytes()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No registration in progress or session expired"})
		return
	}
	h.redis.Del(ctx, fmt.Sprintf("webauthn_reg:%s", userID))

	var meta struct {
		Session        string `json:"session"`
		CredentialName string `json:"credential_name"`
		CredentialType string `json:"credential_type"`
	}
	if err := json.Unmarshal(metaJSON, &meta); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Invalid session data"})
		return
	}

	var session webauthn.SessionData
	if err := json.Unmarshal([]byte(meta.Session), &session); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Invalid session data"})
		return
	}

	user, err := h.buildWebAuthnUser(ctx, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to complete registration"})
		return
	}

	credential, err := h.webauthn.FinishRegistration(user, session, c.Request)
	if err != nil {
		h.log.Error("WebAuthn FinishRegistration failed", "error", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Registration verification failed"})
		return
	}

	// Store credential in DB
	transports := make([]string, 0, len(credential.Transport))
	for _, t := range credential.Transport {
		transports = append(transports, string(t))
	}

	_, err = h.db.ExecContext(ctx, `
		INSERT INTO user_mfa_webauthn (user_id, credential_id, public_key, aaguid, sign_count, credential_name, credential_type, transports)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, userID, credential.ID, credential.PublicKey, credential.Authenticator.AAGUID,
		credential.Authenticator.SignCount, meta.CredentialName, meta.CredentialType, pq.Array(transports))
	if err != nil {
		h.log.Error("Failed to store WebAuthn credential", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store credential"})
		return
	}

	if err := h.updateUserMFAFlags(ctx, userID); err != nil {
		h.log.Error("Failed to update user MFA flags after WebAuthn register", "error", err)
	}

	c.JSON(http.StatusOK, gin.H{
		"message":         "Security key registered successfully",
		"credential_name": meta.CredentialName,
		"credential_type": meta.CredentialType,
	})
}

// WebAuthnListCredentials returns the user's registered WebAuthn credentials.
func (h *Handler) WebAuthnListCredentials(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	rows, err := h.db.QueryContext(ctx, `
		SELECT id, credential_name, credential_type, created_at, last_used_at
		FROM user_mfa_webauthn WHERE user_id = $1 ORDER BY created_at
	`, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list credentials"})
		return
	}
	defer rows.Close() //nolint:errcheck

	type credInfo struct {
		ID             string  `json:"id"`
		CredentialName string  `json:"credential_name"`
		CredentialType string  `json:"credential_type"`
		CreatedAt      string  `json:"created_at"`
		LastUsedAt     *string `json:"last_used_at"`
	}

	var creds []credInfo
	for rows.Next() {
		var ci credInfo
		var lastUsed sql.NullTime
		var createdAt time.Time
		if err := rows.Scan(&ci.ID, &ci.CredentialName, &ci.CredentialType, &createdAt, &lastUsed); err != nil {
			continue
		}
		ci.CreatedAt = createdAt.Format(time.RFC3339)
		if lastUsed.Valid {
			s := lastUsed.Time.Format(time.RFC3339)
			ci.LastUsedAt = &s
		}
		creds = append(creds, ci)
	}

	if creds == nil {
		creds = []credInfo{}
	}
	c.JSON(http.StatusOK, gin.H{"credentials": creds})
}

// WebAuthnDeleteCredential removes a WebAuthn credential.
func (h *Handler) WebAuthnDeleteCredential(c *gin.Context) {
	userID := c.GetString("user_id")
	credentialID := c.Param("id")
	ctx := c.Request.Context()

	var req struct {
		Password string `json:"password" binding:"required"` //nolint:gosec // request field, not a secret
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Password is required"})
		return
	}

	match, err := h.verifyUserPassword(ctx, userID, req.Password)
	if err != nil || !match {
		c.JSON(http.StatusForbidden, gin.H{"error": "Incorrect password"})
		return
	}

	result, err := h.db.ExecContext(ctx,
		`DELETE FROM user_mfa_webauthn WHERE id = $1 AND user_id = $2`, credentialID, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete credential"})
		return
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Credential not found"})
		return
	}

	if err := h.updateUserMFAFlags(ctx, userID); err != nil {
		h.log.Error("Failed to update user MFA flags after WebAuthn delete", "error", err)
	}

	// Return remaining credential IDs so the client can signal the authenticator
	var remainingIDs [][]byte
	credRows, queryErr := h.db.QueryContext(ctx,
		`SELECT credential_id FROM user_mfa_webauthn WHERE user_id = $1`, userID)
	if queryErr == nil {
		defer credRows.Close() //nolint:errcheck
		for credRows.Next() {
			var cid []byte
			if scanErr := credRows.Scan(&cid); scanErr == nil {
				remainingIDs = append(remainingIDs, cid)
			}
		}
	}

	// Base64url-encode remaining credential IDs for the client
	encoded := make([]string, len(remainingIDs))
	for i, cid := range remainingIDs {
		encoded[i] = base64.RawURLEncoding.EncodeToString(cid)
	}

	c.JSON(http.StatusOK, gin.H{
		"message":                  "Credential deleted",
		"remaining_credential_ids": encoded,
		"user_id":                  userID,
	})
}

// ── Shared MFA Verify (Unauthenticated — uses challenge token) ──────────────

// Verify validates an MFA challenge (TOTP, backup code, or WebAuthn).
// This endpoint is unauthenticated — identity comes from the challenge token.
func (h *Handler) Verify(c *gin.Context) {
	var req struct {
		ChallengeToken string          `json:"mfa_challenge_token" binding:"required"`
		Method         string          `json:"method" binding:"required"` // "totp", "backup_code", "webauthn"
		Code           string          `json:"code"`                      // for totp/backup_code
		Assertion      json.RawMessage `json:"assertion"`                 // for webauthn
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	// Try all purposes — the token itself encodes the purpose
	var claims *ChallengeClaims
	var purpose ChallengePurpose
	for _, p := range []ChallengePurpose{PurposeLogin, PurposeSuspiciousRefresh, PurposeMFAUpgrade} {
		if parsed, err := ValidateChallengeToken(req.ChallengeToken, h.jwtSecret, p); err == nil {
			claims = parsed
			purpose = p
			break
		}
	}
	if claims == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired MFA challenge token"})
		return
	}

	ctx := c.Request.Context()

	// Check single-use: ensure this JTI hasn't been consumed
	usedKey := fmt.Sprintf("mfa_challenge_used:%s", claims.ID)
	if h.redis.Exists(ctx, usedKey).Val() > 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "MFA challenge already used"})
		return
	}

	// Rate limit per user
	attemptsKey := fmt.Sprintf("mfa_verify_attempts:%s", claims.UserID)
	lockoutKey := fmt.Sprintf("mfa_verify_lockout:%s", claims.UserID)
	if h.redis.Exists(ctx, lockoutKey).Val() > 0 {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "Too many failed attempts. Try again later."})
		return
	}

	var verified bool

	switch req.Method {
	case "totp", "backup_code":
		if req.Code == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Code is required"})
			return
		}
		valid, err := h.VerifyCode(ctx, claims.UserID, req.Code)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Verification failed"})
			return
		}
		verified = valid

	case "webauthn":
		if len(req.Assertion) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Assertion is required for WebAuthn verification"})
			return
		}

		// Retrieve the session data stored by BeginWebAuthnLogin
		sessionKey := fmt.Sprintf("mfa_webauthn_session:%s", claims.ID)
		sessionJSON, err := h.redis.Get(ctx, sessionKey).Bytes()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "No WebAuthn session found. Request a new challenge."})
			return
		}

		var session webauthn.SessionData
		if err := json.Unmarshal(sessionJSON, &session); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Invalid WebAuthn session"})
			return
		}

		user, err := h.buildWebAuthnUser(ctx, claims.UserID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify"})
			return
		}

		// Use FinishLoginWithBytes since the request body was already consumed by JSON binding
		credential, err := h.webauthn.FinishLoginWithBytes(user, session, req.Assertion)
		if err != nil {
			h.log.Warn("WebAuthn login verification failed", "error", err, "user_id", claims.UserID)
			verified = false
		} else {
			// Update sign count
			_, _ = h.db.ExecContext(ctx, `
				UPDATE user_mfa_webauthn SET sign_count = $1, last_used_at = NOW() WHERE credential_id = $2 AND user_id = $3
			`, credential.Authenticator.SignCount, credential.ID, claims.UserID)
			verified = true
		}
		h.redis.Del(ctx, sessionKey)

	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid method. Use 'totp', 'backup_code', or 'webauthn'"})
		return
	}

	if !verified {
		attempts := h.redis.Incr(ctx, attemptsKey).Val()
		h.redis.Expire(ctx, attemptsKey, 5*time.Minute)
		if attempts >= 5 {
			h.redis.Set(ctx, lockoutKey, "1", 15*time.Minute)
			h.redis.Del(ctx, attemptsKey)
		}
		c.JSON(http.StatusForbidden, gin.H{"error": "Invalid MFA code"})
		return
	}

	// Mark challenge as used (single-use)
	h.redis.Set(ctx, usedKey, "1", 10*time.Minute)
	h.redis.Del(ctx, attemptsKey)

	// Complete the action based on purpose
	switch purpose {
	case PurposeLogin:
		if h.loginCompleter == nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Login completion not configured"})
			return
		}
		// Retrieve remember_me preference from Redis
		rememberKey := fmt.Sprintf("mfa_challenge:%s:remember_me", claims.ID)
		rememberMe := h.redis.Get(ctx, rememberKey).Val() == "1"
		h.redis.Del(ctx, rememberKey)

		h.loginCompleter.CompleteLogin(c, claims.UserID, rememberMe)

	case PurposeSuspiciousRefresh:
		// Return success — the client will retry the refresh
		c.JSON(http.StatusOK, gin.H{
			"verified": true,
			"purpose":  string(purpose),
			"user_id":  claims.UserID,
		})

	case PurposeMFAUpgrade:
		// Pre-MFA session verified. Set a short-lived bypass key so the
		// client's retry of the refresh isn't challenged again. The retry
		// issues a new token with created_at > mfa_enabled_at.
		bypassKey := fmt.Sprintf("mfa_upgrade_bypass:%s", claims.UserID)
		h.redis.Set(ctx, bypassKey, "1", 30*time.Second)
		c.JSON(http.StatusOK, gin.H{
			"verified": true,
			"purpose":  string(purpose),
			"user_id":  claims.UserID,
		})

	default:
		c.JSON(http.StatusOK, gin.H{
			"verified": true,
			"purpose":  string(purpose),
			"user_id":  claims.UserID,
		})
	}
}

// ── Inline WebAuthn Verify (for protected operations) ────────────────────────

// WebAuthnVerifyInlineBegin starts a WebAuthn assertion for MFA verification on
// protected endpoints (setup, revoke, etc.). Returns assertion options for
// navigator.credentials.get(). The session is stored in Redis keyed by user ID.
func (h *Handler) WebAuthnVerifyInlineBegin(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	user, err := h.buildWebAuthnUser(ctx, userID)
	if err != nil {
		h.log.Error("Failed to build WebAuthn user", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to start verification"})
		return
	}
	if len(user.WebAuthnCredentials()) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No WebAuthn credentials registered"})
		return
	}

	assertion, session, err := h.webauthn.BeginLogin(user)
	if err != nil {
		h.log.Error("WebAuthn BeginLogin failed for inline verify", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to start verification"})
		return
	}

	sessionJSON, _ := json.Marshal(session)
	sessionKey := fmt.Sprintf("mfa_inline_session:%s", userID)
	h.redis.Set(ctx, sessionKey, sessionJSON, 2*time.Minute)

	c.JSON(http.StatusOK, assertion)
}

// WebAuthnVerifyInlineFinish validates a WebAuthn assertion for protected
// operations. On success, returns a short-lived verification token that can be
// used as mfa_code on any protected endpoint.
func (h *Handler) WebAuthnVerifyInlineFinish(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	// Read raw body for WebAuthn assertion parsing
	sessionKey := fmt.Sprintf("mfa_inline_session:%s", userID)
	sessionJSON, err := h.redis.Get(ctx, sessionKey).Bytes()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No verification session found. Start a new verification."})
		return
	}
	// Delete session immediately (single-use)
	h.redis.Del(ctx, sessionKey)

	var session webauthn.SessionData
	if err := json.Unmarshal(sessionJSON, &session); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Invalid session data"})
		return
	}

	user, err := h.buildWebAuthnUser(ctx, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify"})
		return
	}

	credential, err := h.webauthn.FinishLogin(user, session, c.Request)
	if err != nil {
		h.log.Warn("WebAuthn inline verify assertion failed", "error", err, "user_id", userID)
		c.JSON(http.StatusForbidden, gin.H{"error": "Verification failed. Try again."})
		return
	}

	// Update sign count
	_, _ = h.db.ExecContext(ctx,
		`UPDATE user_mfa_webauthn SET sign_count = $1, last_used_at = NOW() WHERE credential_id = $2 AND user_id = $3`,
		credential.Authenticator.SignCount, credential.ID, userID,
	)

	// Generate a short-lived verification token (60s, single-use)
	tokenBytes := make([]byte, 24)
	if _, err := rand.Read(tokenBytes); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate token"})
		return
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	tokenKey := fmt.Sprintf("mfa_inline_token:%s:%s", userID, token)
	h.redis.Set(ctx, tokenKey, "1", 60*time.Second)

	c.JSON(http.StatusOK, gin.H{"mfa_token": token})
}

// ── WebAuthn Helpers ─────────────────────────────────────────────────────────

func (h *Handler) buildWebAuthnUser(ctx context.Context, userID string) (*WebAuthnUser, error) {
	var email, username, displayName string
	err := h.db.QueryRowContext(ctx,
		`SELECT email, username, COALESCE(display_name, username) FROM users WHERE id = $1`, userID,
	).Scan(&email, &username, &displayName)
	if err != nil {
		return nil, fmt.Errorf("fetch user: %w", err)
	}

	// Load existing credentials for exclusion during registration
	rows, err := h.db.QueryContext(ctx, `
		SELECT credential_id, public_key, aaguid, sign_count, transports
		FROM user_mfa_webauthn WHERE user_id = $1
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("fetch credentials: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var creds []webauthn.Credential
	for rows.Next() {
		var credID, pubKey, aaguid []byte
		var signCount int64
		var transports []string
		if err := rows.Scan(&credID, &pubKey, &aaguid, &signCount, pq.Array(&transports)); err != nil {
			continue
		}
		cred := webauthn.Credential{
			ID:        credID,
			PublicKey: pubKey,
			Authenticator: webauthn.Authenticator{
				AAGUID:    aaguid,
				SignCount: uint32(signCount), //nolint:gosec // sign count won't overflow uint32
			},
		}
		for _, t := range transports {
			cred.Transport = append(cred.Transport, protocol.AuthenticatorTransport(t))
		}
		creds = append(creds, cred)
	}

	return &WebAuthnUser{
		ID:          []byte(userID),
		Name:        username,
		DisplayName: displayName,
		Credentials: creds,
	}, nil
}

// ── Recovery-Only Methods ─────────────────────────────────────────────────────

// SetRecoveryOnly updates which MFA methods are restricted to account recovery only.
// Recovery-only methods can verify identity for recovery flows but are excluded from
// login and sensitive-operation MFA challenges — like a spare key that unlocks the
// door but doesn't start the engine.
func (h *Handler) SetRecoveryOnly(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	var req struct {
		Methods  []string `json:"methods"`                     // e.g. ["email", "sms"] or [] to clear
		Password string   `json:"password" binding:"required"` //nolint:gosec // request field, not a secret
		MFACode  string   `json:"mfa_code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Password is required"})
		return
	}

	if !h.requirePasswordAndMFA(c, userID, req.Password, req.MFACode) {
		return
	}

	if req.Methods == nil {
		req.Methods = []string{}
	}

	// Validate: only enabled methods can be set as recovery-only
	enabledMethods, err := h.GetEnabledMethods(ctx, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch MFA status"})
		return
	}
	enabled := make(map[string]bool, len(enabledMethods))
	for _, m := range enabledMethods {
		enabled[m] = true
	}

	// Can't make ALL methods recovery-only — at least one must remain login-eligible
	loginEligible := 0
	for _, m := range enabledMethods {
		isRecoveryOnly := false
		for _, r := range req.Methods {
			if r == m {
				isRecoveryOnly = true
				break
			}
		}
		if !isRecoveryOnly {
			loginEligible++
		}
	}
	if loginEligible == 0 && len(enabledMethods) > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "At least one MFA method must remain eligible for login"})
		return
	}

	// Filter to only valid (enabled) methods
	var validRecoveryOnly []string
	for _, m := range req.Methods {
		if enabled[m] {
			validRecoveryOnly = append(validRecoveryOnly, m)
		}
	}

	// If email or sms is being added to recovery-only, auto-enable hardened mode
	hasEmailSmsRecovery := false
	for _, m := range validRecoveryOnly {
		if m == "email" || m == "sms" {
			hasEmailSmsRecovery = true
			break
		}
	}

	if hasEmailSmsRecovery {
		_, err = h.db.ExecContext(ctx,
			`UPDATE users SET recovery_only_methods = $1, recovery_hardened = TRUE WHERE id = $2`,
			pq.Array(validRecoveryOnly), userID,
		)
	} else {
		_, err = h.db.ExecContext(ctx,
			`UPDATE users SET recovery_only_methods = $1 WHERE id = $2`,
			pq.Array(validRecoveryOnly), userID,
		)
	}
	if err != nil {
		h.log.Error("Failed to update recovery_only_methods", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update settings"})
		return
	}

	// Read back actual hardened state
	var recoveryHardened bool
	_ = h.db.QueryRowContext(ctx, `SELECT recovery_hardened FROM users WHERE id = $1`, userID).Scan(&recoveryHardened)

	c.JSON(http.StatusOK, gin.H{
		"recovery_only_methods": validRecoveryOnly,
		"recovery_hardened":     recoveryHardened,
	})
}

// SetRecoveryHardened toggles dual-channel hardened mode for Email+SMS recovery.
// When enabled, account recovery requires BOTH an email code AND an SMS code simultaneously.
// An attacker must compromise both channels — neither alone is sufficient.
func (h *Handler) SetRecoveryHardened(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	var req struct {
		Enabled  bool   `json:"enabled"`
		Password string `json:"password" binding:"required"` //nolint:gosec // request field, not a secret
		MFACode  string `json:"mfa_code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Password is required"})
		return
	}

	if !h.requirePasswordAndMFA(c, userID, req.Password, req.MFACode) {
		return
	}

	_, err := h.db.ExecContext(ctx,
		`UPDATE users SET recovery_hardened = $1 WHERE id = $2`,
		req.Enabled, userID,
	)
	if err != nil {
		h.log.Error("Failed to update recovery_hardened", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update settings"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"recovery_hardened": req.Enabled,
	})
}

// ── Email/SMS MFA (Dev Stub) ──────────────────────────────────────────────────
//
// These endpoints provide a testable Email/SMS MFA flow for development.
// In dev mode, generated codes are returned in the API response (displayed on screen).
// In production, these endpoints refuse to operate — real email/SMS delivery is a
// separate issue requiring provider integration (SendGrid, Twilio, etc.).

// generateNumericCode creates a cryptographically random N-digit numeric code.
func generateNumericCode(digits int) (string, error) {
	maxVal := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(digits)), nil)
	n, err := rand.Int(rand.Reader, maxVal)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", digits, n), nil
}

// EmailSmsSetup enables email and/or SMS as MFA methods (dev stub).
// Requires password + MFA (if active). Returns the verification codes on screen (dev only).
func (h *Handler) EmailSmsSetup(c *gin.Context) {
	if h.environment == "production" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Email/SMS MFA is not yet available. Requires email/SMS provider integration."})
		return
	}

	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	var req struct {
		Password string   `json:"password" binding:"required"` //nolint:gosec // request field, not a secret
		MFACode  string   `json:"mfa_code"`
		Methods  []string `json:"methods" binding:"required"` // ["email"], ["sms"], or ["email", "sms"]
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "password and methods are required"})
		return
	}

	if !h.requirePasswordAndMFA(c, userID, req.Password, req.MFACode) {
		return
	}

	// Validate methods
	for _, m := range req.Methods {
		if m != "email" && m != "sms" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "methods must be 'email' and/or 'sms'"})
			return
		}
	}

	// Must already have a "real" MFA method (TOTP or WebAuthn) active
	if !h.IsEnabled(ctx, userID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Enable a Standard or higher MFA method first"})
		return
	}

	// Generate verification codes and store in Redis
	codes := make(map[string]string)
	for _, method := range req.Methods {
		code, err := generateNumericCode(6)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate code"})
			return
		}
		key := fmt.Sprintf("mfa_emailsms_setup:%s:%s", userID, method)
		h.redis.Set(ctx, key, code, 10*time.Minute)
		codes[method] = code
	}

	// Dev mode: return codes in response for on-screen display
	c.JSON(http.StatusOK, gin.H{
		"message":    "Verification codes generated (DEV MODE — codes shown on screen)",
		"dev_codes":  codes,
		"methods":    req.Methods,
		"expires_in": "10 minutes",
		"dev_mode":   true,
	})
}

// EmailSmsVerify verifies the email/SMS code(s) and activates the method(s).
// In hardened mode, both email AND sms codes must be provided and correct.
func (h *Handler) EmailSmsVerify(c *gin.Context) {
	if h.environment == "production" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Email/SMS MFA is not yet available"})
		return
	}

	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	var req struct {
		Codes map[string]string `json:"codes" binding:"required"` // {"email": "123456"} or {"email": "...", "sms": "..."}
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "codes map is required"})
		return
	}

	if len(req.Codes) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "At least one code is required"})
		return
	}

	// Check if hardened mode requires both
	var recoveryHardened bool
	_ = h.db.QueryRowContext(ctx,
		`SELECT recovery_hardened FROM users WHERE id = $1`, userID,
	).Scan(&recoveryHardened)

	if recoveryHardened {
		if _, hasEmail := req.Codes["email"]; !hasEmail {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Hardened mode requires both email and SMS codes"})
			return
		}
		if _, hasSms := req.Codes["sms"]; !hasSms {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Hardened mode requires both email and SMS codes"})
			return
		}
	}

	// Verify each code
	verified := []string{}
	for method, code := range req.Codes {
		if method != "email" && method != "sms" {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid method: %s", method)})
			return
		}

		key := fmt.Sprintf("mfa_emailsms_setup:%s:%s", userID, method)
		stored, err := h.redis.Get(ctx, key).Result()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("No pending %s code. Request a new one.", method)})
			return
		}

		if code != stored {
			c.JSON(http.StatusForbidden, gin.H{"error": fmt.Sprintf("Invalid %s code", method)})
			return
		}

		verified = append(verified, method)
	}

	// Mark methods as enabled in Redis (persistent flag — no expiry)
	for _, method := range verified {
		h.redis.Set(ctx, fmt.Sprintf("mfa_emailsms_enabled:%s:%s", userID, method), "1", 0)
		h.redis.Del(ctx, fmt.Sprintf("mfa_emailsms_setup:%s:%s", userID, method))
	}

	// Update denormalized flags
	if err := h.updateUserMFAFlags(ctx, userID); err != nil {
		h.log.Error("Failed to update MFA flags after email/sms enable", "error", err)
	}

	c.JSON(http.StatusOK, gin.H{
		"message":  "Email/SMS MFA methods activated",
		"verified": verified,
	})
}

// EmailSmsDisable removes email and/or SMS MFA methods.
func (h *Handler) EmailSmsDisable(c *gin.Context) {
	if h.environment == "production" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Email/SMS MFA is not yet available"})
		return
	}

	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	// Remove enabled flags
	for _, m := range []string{"email", "sms"} {
		h.redis.Del(ctx, fmt.Sprintf("mfa_emailsms_enabled:%s:%s", userID, m))
		h.redis.Del(ctx, fmt.Sprintf("mfa_emailsms_setup:%s:%s", userID, m))
	}

	// Also clear from recovery-only if present
	_, _ = h.db.ExecContext(ctx,
		`UPDATE users SET recovery_only_methods = array_remove(array_remove(recovery_only_methods, 'email'), 'sms') WHERE id = $1`,
		userID,
	)

	if err := h.updateUserMFAFlags(ctx, userID); err != nil {
		h.log.Error("Failed to update MFA flags after email/sms disable", "error", err)
	}

	c.JSON(http.StatusOK, gin.H{"message": "Email/SMS MFA methods disabled"})
}

// EmailSmsStatus returns whether email/sms methods are enabled (dev helper).
func (h *Handler) EmailSmsStatus(c *gin.Context) {
	if h.environment == "production" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Email/SMS MFA is not yet available"})
		return
	}

	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	emailEnabled := h.redis.Exists(ctx, fmt.Sprintf("mfa_emailsms_enabled:%s:email", userID)).Val() > 0
	smsEnabled := h.redis.Exists(ctx, fmt.Sprintf("mfa_emailsms_enabled:%s:sms", userID)).Val() > 0

	c.JSON(http.StatusOK, gin.H{
		"email_enabled": emailEnabled,
		"sms_enabled":   smsEnabled,
		"dev_mode":      true,
	})
}

// ── Backup Email ─────────────────────────────────────────────────────────────

// GetBackupEmail returns the user's backup email.
func (h *Handler) GetBackupEmail(c *gin.Context) {
	userID := c.GetString("user_id")
	var backupEmail sql.NullString
	_ = h.db.QueryRowContext(c.Request.Context(),
		`SELECT backup_email FROM users WHERE id = $1`, userID,
	).Scan(&backupEmail)
	email := ""
	if backupEmail.Valid {
		email = backupEmail.String
	}
	c.JSON(http.StatusOK, gin.H{"backup_email": email})
}

// SetBackupEmail sets or clears the user's backup email for recovery.
func (h *Handler) SetBackupEmail(c *gin.Context) {
	userID := c.GetString("user_id")
	var req struct {
		Email string `json:"email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	// Basic email validation (or allow empty to clear)
	if req.Email != "" && !isValidEmail(req.Email) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid email address"})
		return
	}

	var val interface{}
	if req.Email == "" {
		val = nil
	} else {
		val = req.Email
	}

	_, err := h.db.ExecContext(c.Request.Context(),
		`UPDATE users SET backup_email = $1 WHERE id = $2`, val, userID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update backup email"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"backup_email": req.Email})
}

func isValidEmail(email string) bool {
	// Simple validation: has @ with something on both sides
	at := -1
	for i, c := range email {
		if c == '@' {
			if at >= 0 {
				return false // multiple @
			}
			at = i
		}
	}
	return at > 0 && at < len(email)-1
}
