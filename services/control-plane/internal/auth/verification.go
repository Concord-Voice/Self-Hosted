package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/email"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/redis/go-redis/v9"
)

const (
	verifyCodeTTL     = 10 * time.Minute // Code expires after 10 minutes
	verifyMaxAttempts = 5                // Max wrong-code attempts before invalidating
)

// verificationRecord is stored in Redis keyed by "email_verify:{user_id}".
type verificationRecord struct {
	CodeHash string `json:"code_hash"`
	Email    string `json:"email"`
	Attempts int    `json:"attempts"`
}

// VerificationHandler handles email verification endpoints.
type VerificationHandler struct {
	db        *sql.DB
	redis     *redis.Client
	log       *logger.Logger
	email     *email.Service
	jwtSecret string
}

// NewVerificationHandler creates a new verification handler.
func NewVerificationHandler(db *sql.DB, redisClient *redis.Client, log *logger.Logger, emailSvc *email.Service, jwtSecret string) *VerificationHandler {
	return &VerificationHandler{
		db:        db,
		redis:     redisClient,
		log:       log,
		email:     emailSvc,
		jwtSecret: jwtSecret,
	}
}

// generateCode produces a cryptographically random 6-digit numeric code.
func generateCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// hashCode returns the SHA-256 hex digest of a verification code.
func hashCode(code string) string {
	h := sha256.Sum256([]byte(code))
	return hex.EncodeToString(h[:])
}

// redisKey returns the Redis key for a user's pending verification.
func redisKey(userID string) string {
	return fmt.Sprintf("email_verify:%s", userID)
}

// GenerateAndSendCode creates a verification code, stores it in Redis, and emails it.
// Shared by the HTTP handler and the registration goroutine to avoid logic duplication.
func (vh *VerificationHandler) GenerateAndSendCode(ctx context.Context, userID, userEmail string) error {
	code, err := generateCode()
	if err != nil {
		return fmt.Errorf("generate code: %w", err)
	}

	record := verificationRecord{
		CodeHash: hashCode(code),
		Email:    userEmail,
		Attempts: 0,
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal verification record: %w", err)
	}

	if err := vh.redis.Set(ctx, redisKey(userID), data, verifyCodeTTL).Err(); err != nil {
		return fmt.Errorf("store code in Redis: %w", err)
	}

	// Update sent_at for defense-in-depth rate limiting
	if _, err := vh.db.ExecContext(ctx,
		`UPDATE users SET email_verification_sent_at = NOW() WHERE id = $1`, userID,
	); err != nil {
		vh.log.Warn("Failed to update email_verification_sent_at", "error", err, "user_id", userID)
		// Non-fatal — sent_at is defense-in-depth, not primary rate limiting
	}

	if err := vh.email.SendVerificationCode(userEmail, code); err != nil {
		return fmt.Errorf("send email: %w", err)
	}

	return nil
}

// SendCode generates a verification code and emails it to the user.
func (vh *VerificationHandler) SendCode(c *gin.Context) {
	userID := c.GetString("user_id")

	// Look up the user's email
	var userEmail string
	var emailVerified bool
	err := vh.db.QueryRow(
		`SELECT email, email_verified FROM users WHERE id = $1`, userID,
	).Scan(&userEmail, &emailVerified)
	if err != nil {
		vh.log.Error("Failed to look up user for verification", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send verification code"})
		return
	}

	if emailVerified {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Email already verified"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	if err := vh.GenerateAndSendCode(ctx, userID, userEmail); err != nil {
		vh.log.Error("Failed to send verification code", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send verification code"})
		return
	}

	vh.log.Info("Verification code sent", "user_id", userID, "email", userEmail)
	c.JSON(http.StatusOK, gin.H{
		"message":    "Verification code sent",
		"expires_in": int(verifyCodeTTL.Seconds()),
	})
}

// ConfirmCodeRequest is the JSON body for the confirm endpoint.
type ConfirmCodeRequest struct {
	Code string `json:"code" binding:"required"`
}

// ConfirmCode validates the submitted code and marks the email as verified.
func (vh *VerificationHandler) ConfirmCode(c *gin.Context) {
	userID := c.GetString("user_id")

	var req ConfirmCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Code is required"})
		return
	}

	// Trim whitespace/dashes and validate format before any Redis lookups
	code := strings.TrimSpace(strings.ReplaceAll(req.Code, "-", ""))
	if len(code) != 6 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Verification code must be 6 digits"})
		return
	}
	for _, ch := range code {
		if ch < '0' || ch > '9' {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Verification code must be 6 digits"})
			return
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()

	// Fetch stored record
	data, err := vh.redis.Get(ctx, redisKey(userID)).Bytes()
	if err == redis.Nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No pending verification code. Please request a new one."})
		return
	}
	if err != nil {
		vh.log.Error("Failed to fetch verification record", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Verification failed"})
		return
	}

	var record verificationRecord
	if err := json.Unmarshal(data, &record); err != nil {
		vh.log.Error("Failed to unmarshal verification record", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Verification failed"})
		return
	}

	// Check attempt limit
	if record.Attempts >= verifyMaxAttempts {
		vh.redis.Del(ctx, redisKey(userID))
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "Too many attempts. Please request a new code."})
		return
	}

	// Timing-safe comparison to prevent side-channel attacks
	if subtle.ConstantTimeCompare([]byte(hashCode(code)), []byte(record.CodeHash)) != 1 {
		// Increment attempts
		record.Attempts++
		updated, _ := json.Marshal(record)
		ttl := vh.redis.TTL(ctx, redisKey(userID)).Val()
		if ttl > 0 {
			vh.redis.Set(ctx, redisKey(userID), updated, ttl)
		}
		remaining := verifyMaxAttempts - record.Attempts
		c.JSON(http.StatusBadRequest, gin.H{
			"error":              "Invalid verification code",
			"attempts_remaining": remaining,
		})
		return
	}

	// Code matches — mark the specific email as verified (defense-in-depth:
	// ensures the email hasn't been changed between code issuance and confirmation)
	result, err := vh.db.Exec(
		`UPDATE users SET email_verified = true WHERE id = $1 AND email = $2`, userID, record.Email,
	)
	if err != nil {
		vh.log.Error("Failed to mark email as verified", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Verification failed"})
		return
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		vh.log.Warn("Email changed between code issuance and confirmation", "user_id", userID, "expected_email", record.Email)
		vh.redis.Del(ctx, redisKey(userID))
		c.JSON(http.StatusConflict, gin.H{"error": "Your email has changed since the code was sent. Please request a new code."})
		return
	}

	// Clean up Redis
	vh.redis.Del(ctx, redisKey(userID))

	// Issue a new access token with email_verified = true
	accessToken, err := GenerateAccessToken(userID, vh.jwtSecret, true)
	if err != nil {
		vh.log.Error("Failed to generate post-verification token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Verification succeeded but token refresh failed"})
		return
	}

	vh.log.Info("Email verified", "user_id", userID)
	c.JSON(http.StatusOK, gin.H{
		"message":      "Email verified successfully",
		"access_token": accessToken,
		"expires_in":   int(AccessTokenTTL.Seconds()),
	})
}

// ChangeEmailRequest is the JSON body for the change-email endpoint.
type ChangeEmailRequest struct {
	NewEmail string `json:"new_email" binding:"required,email"`
}

// ChangeEmail updates the user's email and sends a new verification code.
func (vh *VerificationHandler) ChangeEmail(c *gin.Context) {
	userID := c.GetString("user_id")

	var req ChangeEmailRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Valid email address is required"})
		return
	}

	newEmail := strings.ToLower(strings.TrimSpace(req.NewEmail))

	// Check the user is not already verified (shouldn't be able to change email post-verification via this endpoint)
	var emailVerified bool
	err := vh.db.QueryRow(
		`SELECT email_verified FROM users WHERE id = $1`, userID,
	).Scan(&emailVerified)
	if err != nil {
		vh.log.Error("Failed to check verification status", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to change email"})
		return
	}
	if emailVerified {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Email already verified. Use profile settings to change your email."})
		return
	}

	// Check uniqueness
	var exists bool
	err = vh.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM users WHERE email = $1 AND id != $2)`, newEmail, userID,
	).Scan(&exists)
	if err != nil {
		vh.log.Error("Failed to check email uniqueness", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to change email"})
		return
	}
	if exists {
		c.JSON(http.StatusConflict, gin.H{"error": "Email already in use"})
		return
	}

	// Update email
	_, err = vh.db.Exec(
		`UPDATE users SET email = $1 WHERE id = $2`, newEmail, userID,
	)
	if err != nil {
		vh.log.Error("Failed to update email", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to change email"})
		return
	}

	// Delete old verification code
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	vh.redis.Del(ctx, redisKey(userID))

	// Generate and send new code via shared helper
	sendCtx, sendCancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer sendCancel()

	if err := vh.GenerateAndSendCode(sendCtx, userID, newEmail); err != nil {
		vh.log.Error("Failed to send verification code after email change", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Email updated but failed to send verification code. Please request a new one."})
		return
	}

	vh.log.Info("Email changed and verification code sent", "user_id", userID, "new_email", newEmail)
	c.JSON(http.StatusOK, gin.H{
		"message":    "Email updated and verification code sent",
		"expires_in": int(verifyCodeTTL.Seconds()),
	})
}
