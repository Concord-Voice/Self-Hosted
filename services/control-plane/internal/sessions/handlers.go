// Package sessions provides handlers for managing user authentication sessions.
package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/models"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const sessionNotFoundMessage = "Session not found"

const (
	errMsgUnauthorized          = "Unauthorized"
	errMsgFailedRevokeSession   = "Failed to revoke session"
	errMsgMFAVerificationFailed = "MFA verification failed"
	errMsgInvalidMFACode        = "Invalid MFA code"
	errMsgFailedVerifyPassword  = "Failed to verify password"
	errMsgIncorrectPassword     = "Incorrect password"
	errMsgFailedFetchSessions   = "Failed to fetch sessions"
	errMsgCurrentSessionUnknown = "Current session cannot be identified"
	errMsgAuthUnavailable       = "Authentication temporarily unavailable"
	errMsgSocketFencePending    = "Sessions revoked; WebSocket disconnection pending"
)

var (
	errCurrentSessionUnknown      = errors.New("current session cannot be identified")
	errRevokeAuthenticationNeeded = errors.New("session revocation requires authentication")
)

// SessionDisconnector synchronously fences the WebSocket transports of one
// exact login family. Defined as an interface to avoid coupling to the
// websocket package.
type SessionDisconnector interface {
	loginfamily.FamilyCloser
	SnapshotUserFamiliesContext(context.Context, uuid.UUID) ([]uuid.UUID, error)
	DisconnectFamiliesContext(context.Context, uuid.UUID, []uuid.UUID) error
}

// MFAVerifier checks MFA status and verifies codes for sensitive operations.
// purpose scopes the WebAuthn inline token a code may be (see stepup.Purpose).
type MFAVerifier interface {
	IsEnabled(ctx context.Context, userID string) bool
	VerifyCode(ctx context.Context, userID string, purpose stepup.Purpose, code string) (bool, error)
	GetEnabledMethods(ctx context.Context, userID string) ([]string, error)
}

// revokeAction is one route's authenticateForRevoke parameters: the copy its
// refusal names, the route its security events carry, and the step-up purpose
// a WebAuthn inline token must have been minted for. Each route has its own
// purpose, so a token minted to revoke one session cannot revoke them all.
type revokeAction struct {
	desc    string
	route   securityevent.RouteTemplate
	purpose stepup.Purpose
}

var (
	revokeSessionAction            = revokeAction{desc: "revoke this session", route: securityevent.RouteSessionDelete, purpose: stepup.PurposeSessionRevoke}
	revokeAllSessionsAction        = revokeAction{desc: "revoke all sessions", route: securityevent.RouteSessionsRevokeAll, purpose: stepup.PurposeSessionsRevokeAll}
	revocationModeSetAction        = revokeAction{desc: "change revocation mode", route: securityevent.RouteSessionsRevocationMode, purpose: stepup.PurposeRevocationModeSet}
	errSimpleAuthWindowUnavailable = errors.New("simple-mode authentication window Redis client unavailable")
)

// Revocation rate-limiting constants.
const (
	simpleAuthWindowTTL        = 5 * time.Minute
	simpleAuthWindowVersion    = "v2:"
	freeRevokeSessionThreshold = 3 // Must have >= 3 active sessions for a free revoke
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
	db             *sql.DB
	redis          *redis.Client
	log            *logger.Logger
	hub            SessionDisconnector
	mfaVerifier    MFAVerifier
	securityEvents securityevent.Emitter
}

// NewHandler creates a new session handler.
func NewHandler(db *sql.DB, redis *redis.Client, log *logger.Logger, hub SessionDisconnector, mfaVerifier MFAVerifier) *Handler {
	return &Handler{
		db:             db,
		redis:          redis,
		log:            log,
		hub:            hub,
		mfaVerifier:    mfaVerifier,
		securityEvents: securityevent.Discard,
	}
}

// SetSecurityEvents injects bounded Nightwatch telemetry without changing the
// public constructor used by the router and tests.
func (h *Handler) SetSecurityEvents(events securityevent.Emitter) {
	if events == nil {
		events = securityevent.Discard
	}
	h.securityEvents = events
}

func (h *Handler) emitSecurityEvent(ctx context.Context, event securityevent.Event) {
	if h.securityEvents != nil {
		h.securityEvents.Emit(ctx, event)
	}
}

func (h *Handler) emitHTTPEvent(c *gin.Context, event securityevent.Event) {
	h.emitSecurityEvent(c.Request.Context(), event)
	middleware.MarkNightwatchHandled(c)
}

// ── Revocation Policy Helpers ────────────────────────────────────────────────

func revokeTrackingKey(userID string) string {
	return fmt.Sprintf("session_revokes:%s", userID)
}

func simpleAuthWindowKey(userID string) string {
	return fmt.Sprintf("session_revoke_auth:%s", userID)
}

func (h *Handler) readSimpleAuthWindow(ctx context.Context, userID string) (string, error) {
	if h.redis == nil {
		return "", errSimpleAuthWindowUnavailable
	}
	return h.redis.Get(ctx, simpleAuthWindowKey(userID)).Result()
}

func (h *Handler) simpleAuthWindowMatches(userID, value string, err error, generation int64) bool {
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			h.log.Warn("Simple-mode authentication window unavailable", "user_id", userID, "error", err)
		}
		return false
	}
	return value == fmt.Sprintf("%s%d", simpleAuthWindowVersion, generation)
}

// grantSimpleAuthWindow sets a 5-minute authentication window in Redis.
func (h *Handler) grantSimpleAuthWindow(ctx context.Context, userID string, generation int64) {
	if h.redis == nil {
		h.log.Warn("Could not grant simple-mode authentication window")
		return
	}
	ctx, cancel := stepup.SettleContext(ctx)
	defer cancel()
	if err := h.redis.Set(ctx, simpleAuthWindowKey(userID), fmt.Sprintf("%s%d", simpleAuthWindowVersion, generation), simpleAuthWindowTTL).Err(); err != nil {
		h.log.Warn("Could not grant simple-mode authentication window")
	}
}

// resetRevokeTracking removes the legacy Redis tracking set and auth window.
func (h *Handler) resetRevokeTracking(ctx context.Context, userID string) {
	if h.redis == nil {
		h.log.Warn("Could not clear session-revocation caches")
		return
	}
	ctx, cancel := stepup.SettleContext(ctx)
	defer cancel()
	if err := h.redis.Del(ctx, revokeTrackingKey(userID), simpleAuthWindowKey(userID)).Err(); err != nil {
		h.log.Warn("Could not clear session-revocation caches")
	}
}

// getUserRevocationMode fetches the user's revocation_mode from the database.
// Returns "secure" as the default if the query fails.
func (h *Handler) getUserRevocationMode(ctx context.Context, userID string) string {
	var mode string
	err := h.db.QueryRowContext(ctx, `SELECT revocation_mode FROM users WHERE id = $1`, userID).Scan(&mode)
	if err != nil {
		return "secure"
	}
	return mode
}

// verifyUserPassword fetches the stored Argon2id hash and verifies the provided
// password under the account's shared /auth/login lockout. It returns
// auth.ErrAccountLocked while the account is locked; any other error is a
// lookup, Redis or hash fault and carries no credential.
func (h *Handler) verifyUserPassword(ctx context.Context, userID, password string) (bool, error) {
	var email, passwordHash string
	if err := h.db.QueryRowContext(ctx, `SELECT email, password_hash FROM users WHERE id = $1`, userID).Scan(&email, &passwordHash); err != nil {
		return false, fmt.Errorf("failed to fetch password hash: %w", err)
	}
	match, err := auth.VerifyPasswordWithLockout(ctx, h.redis, email, password, passwordHash)
	if err != nil {
		return false, fmt.Errorf("password verification error: %w", err)
	}
	return match, nil
}

// ── Handlers ─────────────────────────────────────────────────────────────────

// ListSessions returns all active sessions for the current user
func (h *Handler) ListSessions(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
		return
	}

	var currentTokenHash string
	currentToken, cookieErr := c.Cookie("refresh_token")
	if cookieErr == nil && currentToken != "" {
		currentTokenHash = auth.HashRefreshToken(currentToken)
	}

	activeSessions, err := h.fetchActiveSessions(userID.(string), currentTokenHash)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchSessions})
		return
	}

	pastSessions, err := h.fetchPastSessions(userID.(string))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchSessions})
		return
	}
	mode := h.getUserRevocationMode(c.Request.Context(), userID.(string))

	c.JSON(http.StatusOK, gin.H{
		"sessions":        activeSessions,
		"past_sessions":   pastSessions,
		"total":           len(activeSessions),
		"revocation_mode": mode,
	})
}

// fetchActiveSessions queries active (non-revoked, non-expired) sessions for a user.
func (h *Handler) fetchActiveSessions(uid, currentTokenHash string) ([]gin.H, error) {
	// Absent metadata normalizes to "" — the DISPLAY treatment, which
	// maskIPAddress then renders as "unknown". The SSO adapter inserts rows
	// omitting device_name, ip_address and user_agent, and database/sql refuses
	// NULL -> string, so before #3290 every SSO session was silently dropped by
	// the scan-error path below while this endpoint still returned 200.
	//
	// host() rather than ::text, measured through lib/pq: a bare inet column
	// renders via inet_out as "192.168.1.42", while ip_address::text yields
	// "192.168.1.42/32" — which net.ParseIP rejects, making maskIPAddress
	// return "unknown" for every session on the list. host() is byte-identical
	// to what this reader receives today, so only the NULL case changes.
	rows, err := h.db.Query(
		`SELECT id, token_hash,
		        COALESCE(device_name, ''), COALESCE(host(ip_address), ''), COALESCE(user_agent, ''),
		        expires_at, created_at, last_used_at, remember_me, COALESCE(machine_id, '')
		 FROM refresh_tokens
		 WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > NOW()
		 ORDER BY last_used_at DESC`,
		uid,
	)
	if err != nil {
		h.log.Error(errMsgFailedFetchSessions, "error", err)
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	activeSessions := []gin.H{}
	for rows.Next() {
		var s models.RefreshToken
		if err := rows.Scan(&s.ID, &s.TokenHash, &s.DeviceName, &s.IPAddress, &s.UserAgent,
			&s.ExpiresAt, &s.CreatedAt, &s.LastUsedAt, &s.RememberMe, &s.MachineID); err != nil {
			// Fail closed. A session the user cannot see is a session they
			// cannot revoke, so a truncated list rendered as complete is a
			// worse outcome than a 500 (#3290).
			h.log.Error("Failed to scan session", "error", err)
			return nil, err
		}
		activeSessions = append(activeSessions, gin.H{
			"id": s.ID, "device_name": s.DeviceName, "ip_address": maskIPAddress(s.IPAddress),
			"user_agent": s.UserAgent, "machine_id": s.MachineID, "expires_at": s.ExpiresAt,
			"created_at": s.CreatedAt, "last_used": s.LastUsedAt, "remember_me": s.RememberMe,
			"is_current": currentTokenHash != "" && s.TokenHash == currentTokenHash,
		})
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating sessions", "error", err)
		return nil, err
	}
	return activeSessions, nil
}

// fetchPastSessions queries recently-revoked sessions (last 30 days) for a user.
func (h *Handler) fetchPastSessions(uid string) ([]gin.H, error) {
	// Same normalization and the same host() reasoning as fetchActiveSessions
	// above (#3290).
	pastRows, err := h.db.Query(
		`SELECT id, COALESCE(device_name, ''), COALESCE(host(ip_address), ''), COALESCE(user_agent, ''),
		        created_at, last_used_at, revoked_at
		 FROM refresh_tokens
		 WHERE user_id = $1 AND revoked_at IS NOT NULL AND revoked_at > NOW() - INTERVAL '30 days'
		 ORDER BY revoked_at DESC
		 LIMIT 10`,
		uid,
	)
	if err != nil {
		h.log.Error("Failed to fetch past sessions", "error", err)
		return nil, err
	}
	defer func() { _ = pastRows.Close() }()

	pastSessions := []gin.H{}
	for pastRows.Next() {
		var s models.RefreshToken
		if err := pastRows.Scan(&s.ID, &s.DeviceName, &s.IPAddress, &s.UserAgent,
			&s.CreatedAt, &s.LastUsedAt, &s.RevokedAt); err != nil {
			// Fail closed, for the same reason as the active list: a partial
			// history rendered as complete is the same lie in a quieter place.
			h.log.Error("Failed to scan past session", "error", err)
			return nil, err
		}
		pastSessions = append(pastSessions, gin.H{
			"id": s.ID, "device_name": s.DeviceName, "ip_address": maskIPAddress(s.IPAddress),
			"user_agent": s.UserAgent, "created_at": s.CreatedAt, "last_used": s.LastUsedAt,
			"revoked_at": s.RevokedAt,
		})
	}
	// This loop had no rows.Err() check at all, unlike its sibling above — an
	// independent backend.md violation fixed alongside (#3290).
	if err := pastRows.Err(); err != nil {
		h.log.Error("Error iterating past sessions", "error", err)
		return nil, err
	}
	return pastSessions, nil
}

// RevokeSession revokes a specific session by ID with server-authoritative
// rate-limiting policy enforcement. The server independently determines whether
// password authentication is required — no client claims are trusted.
func (h *Handler) RevokeSession(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
		return
	}

	sessionID := c.Param("id")
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Session ID is required"})
		return
	}

	uid := userID.(string)
	ctx := c.Request.Context()

	if !h.verifySessionOwnership(ctx, c, sessionID, uid) {
		return
	}

	// Parse optional password from request body
	var req struct {
		Password string `json:"password"` // #nosec G117 -- not a secret field, receives user input for auth verification
		MFACode  string `json:"mfa_code"`
	}
	if c.ContentType() == "application/json" {
		_ = c.ShouldBindJSON(&req)
	}

	result, revokeErr := h.executeRevocation(ctx, uid, sessionID, middleware.TokenCredentialEpoch(c), false)
	authenticated, mfaVerified := false, false
	if errors.Is(revokeErr, errRevokeAuthenticationNeeded) && !errors.Is(revokeErr, loginfamily.ErrUnavailable) {
		blocked, verified := h.authenticateForRevokeWithMFA(ctx, c, uid, req.Password, req.MFACode, revokeSessionAction)
		if blocked {
			return
		}
		authenticated, mfaVerified = true, verified
		result, revokeErr = h.executeRevocation(ctx, uid, sessionID, middleware.TokenCredentialEpoch(c), true)
	}
	if revokeErr != nil {
		h.handleRevokeSessionError(ctx, c, uid, result, revokeErr)
		return
	}
	if err := h.closeCommittedFamily(ctx, result.target); err != nil {
		h.log.Error("Revoked session socket fence unconfirmed", "user_id", uid, "error", err)
		if mfaVerified {
			h.clearRevokeMFABudget(ctx, uid)
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsgSocketFencePending})
		return
	}
	_ = h.closeCommittedFamilies(ctx, result.revocation, []loginfamily.Binding{result.target})
	if authenticated && result.mode == "simple" {
		h.grantSimpleAuthWindow(ctx, uid, result.generation)
	}
	if mfaVerified {
		h.clearRevokeMFABudget(ctx, uid)
	}
	h.log.Info("Session revoked", "user_id", uid)
	h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventSession, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonSessionRevoked, AuthMethod: securityevent.AuthSession, RouteTemplate: securityevent.RouteSessionDelete})
	c.JSON(http.StatusOK, gin.H{
		"message":    "Session revoked successfully",
		"session_id": sessionID,
	})
}

func (h *Handler) handleRevokeSessionError(ctx context.Context, c *gin.Context, uid string, result singleRevocationResult, revokeErr error) {
	var fenced []loginfamily.Binding
	if commitOutcomeUnknown(revokeErr) {
		if err := h.closeCommittedFamily(ctx, result.target); err != nil {
			h.log.Warn("Uncertain session revocation socket fence unconfirmed", "user_id", uid, "error", err)
		} else {
			fenced = append(fenced, result.target)
		}
	}
	_ = h.closeCommittedFamilies(ctx, result.revocation, fenced)
	switch {
	case errors.Is(revokeErr, loginfamily.ErrTargetNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": sessionNotFoundMessage})
	case errors.Is(revokeErr, loginfamily.ErrUnavailable):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsgAuthUnavailable})
	case errors.Is(revokeErr, loginfamily.ErrDenied), errors.Is(revokeErr, credepoch.ErrEpochMismatch), errors.Is(revokeErr, credepoch.ErrBlocked):
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
	default:
		h.log.Error("Failed to revoke session")
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedRevokeSession})
	}
}

// verifySessionOwnership checks immutable SID ownership, including terminal families.
// Returns true if valid, false if a response was already written.
func (h *Handler) verifySessionOwnership(ctx context.Context, c *gin.Context, sessionID, uid string) bool {
	if sid, err := uuid.Parse(sessionID); err != nil || sid == uuid.Nil {
		c.JSON(http.StatusNotFound, gin.H{"error": sessionNotFoundMessage})
		return false
	}
	var sessionExists bool
	err := h.db.QueryRowContext(ctx, //nolint:gosec // parameterized query — not injectable
		`SELECT EXISTS(SELECT 1 FROM login_family_sids WHERE sid = $1 AND user_id = $2)`,
		sessionID, uid,
	).Scan(&sessionExists)
	if err != nil {
		h.log.Error("Failed to verify session ownership")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsgAuthUnavailable})
		return false
	}
	if !sessionExists {
		c.JSON(http.StatusNotFound, gin.H{"error": sessionNotFoundMessage})
		return false
	}
	return true
}

// authenticateForRevoke verifies the user's identity via MFA or password for session revocation.
// Returns true if the request was blocked (response written), false if authentication passed.
func (h *Handler) authenticateForRevoke(ctx context.Context, c *gin.Context, uid, password, mfaCode string, action revokeAction) bool {
	blocked, _ := h.authenticateForRevokeWithMFA(ctx, c, uid, password, mfaCode, action)
	return blocked
}

func (h *Handler) authenticateForRevokeWithMFA(ctx context.Context, c *gin.Context, uid, password, mfaCode string, action revokeAction) (blocked, mfaVerified bool) {
	route := action.route
	hasMFA := h.mfaVerifier != nil && h.mfaVerifier.IsEnabled(ctx, uid)

	if password == "" {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   "password_required",
			"message": "Password verification required to " + action.desc,
		})
		return true, false
	}

	match, verifyErr := h.verifyUserPassword(ctx, uid, password)
	if errors.Is(verifyErr, auth.ErrAccountLocked) {
		h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventSecurityControl, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonAccountLocked, AuthMethod: securityevent.AuthPassword, RouteTemplate: route})
		stepup.LockedError().Write(c)
		return true, false
	}
	if verifyErr != nil {
		h.log.Error("Password verification failed during session revoke", "error", verifyErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyPassword})
		return true, false
	}
	if !match {
		h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonInvalidCredentials, AuthMethod: securityevent.AuthPassword, RouteTemplate: route})
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgIncorrectPassword})
		return true, false
	}

	if hasMFA {
		if mfaCode == "" {
			methods, err := h.mfaVerifier.GetEnabledMethods(ctx, uid)
			if err != nil {
				h.log.Error("Failed to read MFA methods during session revoke", "error", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgMFAVerificationFailed})
				return true, false
			}
			c.JSON(http.StatusForbidden, gin.H{
				"error":   "auth_required",
				"message": "Authentication required to " + action.desc,
				"methods": methods,
			})
			return true, false
		}
		if e := stepup.Charge(ctx, stepup.DangerousActionBudget(h.redis), uid, mfaCode); e != nil {
			if e.Cause != nil {
				h.log.Error("Session-revoke MFA budget unavailable", "error", e.Cause)
			}
			e.Write(c)
			return true, false
		}
		valid, mfaErr := h.mfaVerifier.VerifyCode(ctx, uid, action.purpose, mfaCode)
		if mfaErr != nil {
			h.log.Error("MFA verification error during session revoke", "error", mfaErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgMFAVerificationFailed})
			return true, false
		}
		if !valid {
			h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonChallengeInvalid, RouteTemplate: route})
			c.JSON(http.StatusForbidden, gin.H{"error": errMsgInvalidMFACode})
			return true, false
		}
		mfaVerified = true
	}
	return false, mfaVerified
}

// clearRevokeMFABudget resets the budget after a committed revocation, on a
// context detached from the request so a client hang-up cannot cancel it.
func (h *Handler) clearRevokeMFABudget(ctx context.Context, uid string) {
	ctx, cancel := stepup.SettleContext(ctx)
	defer cancel()
	if err := stepup.DangerousActionBudget(h.redis).Clear(ctx, uid); err != nil {
		h.log.Warn("Could not reset session-revoke MFA budget after a successful change", "user_id", uid, "error", err)
	}
}

type singleRevocationResult struct {
	revocation loginfamily.RevocationResult
	target     loginfamily.Binding
	mode       string
	generation int64
}

// executeRevocation reads Redis policy state before the transaction, then
// validates it against the generation read under the revocation's user lock.
func (h *Handler) executeRevocation(ctx context.Context, uid, sessionID, epoch string, authenticated bool) (singleRevocationResult, error) {
	var result singleRevocationResult
	var authWindowValue string
	var authWindowErr error
	if !authenticated {
		authWindowValue, authWindowErr = h.readSimpleAuthWindow(ctx, uid)
	}
	policy := func(ctx context.Context, tx *sql.Tx) error {
		var freeAvailable bool
		var activeCount int
		err := tx.QueryRowContext(ctx, `SELECT revocation_mode, session_revoke_auth_gen,
 (last_session_revoke_at IS NULL OR last_session_revoke_at <= clock_timestamp() - INTERVAL '24 hours'),
 (SELECT count(*) FROM refresh_tokens WHERE user_id=users.id AND revoked_at IS NULL AND expires_at>clock_timestamp()),
 s.user_id,s.family_id
 FROM users JOIN login_family_sids s ON s.user_id=users.id
 WHERE users.id=$1 AND s.sid=$2`, uid, sessionID).Scan(&result.mode, &result.generation, &freeAvailable, &activeCount, &result.target.UserID, &result.target.FamilyID)
		if errors.Is(err, sql.ErrNoRows) {
			return loginfamily.NewError(loginfamily.ReasonTargetUnknown, nil)
		}
		if err != nil {
			return loginfamily.NewError(loginfamily.ReasonSQLUnavailable, err)
		}
		freeRevoke := freeAvailable && activeCount >= freeRevokeSessionThreshold
		if !authenticated && !freeRevoke && (result.mode != "simple" ||
			!h.simpleAuthWindowMatches(uid, authWindowValue, authWindowErr, result.generation)) {
			return errRevokeAuthenticationNeeded
		}
		var updated int
		if err := tx.QueryRowContext(ctx, `UPDATE users SET last_session_revoke_at=clock_timestamp() WHERE id=$1 RETURNING 1`, uid).Scan(&updated); err != nil {
			return loginfamily.NewError(loginfamily.ReasonSQLUnavailable, err)
		}
		return nil
	}
	var err error
	result.revocation, err = auth.RevokeRefreshSessionWithPolicy(ctx, h.db, uid, sessionID, epoch, policy)
	return result, err
}

// RevokeAllSessions revokes all sessions after verifying the user's password.
// Requires a JSON body with "password" and optionally "include_current".
// Password re-verification prevents an attacker with a stolen session from
// mass-revoking the victim's sessions as a DoS.
func (h *Handler) RevokeAllSessions(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
		return
	}

	var req struct {
		Password       string `json:"password"` // #nosec G117 -- not a secret field, receives user input for auth verification
		MFACode        string `json:"mfa_code"`
		IncludeCurrent bool   `json:"include_current"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	uid := userID.(string)
	ctx := c.Request.Context()

	blocked, mfaVerified := h.authenticateForRevokeWithMFA(ctx, c, uid, req.Password, req.MFACode, revokeAllSessionsAction)
	if blocked {
		return
	}

	result, err := h.revokeAllSessionsDB(uid, req.IncludeCurrent, c)
	if err != nil {
		var fenced []loginfamily.Binding
		if commitOutcomeUnknown(err) {
			var fenceErr error
			fenced, fenceErr = h.closeCapturedFamilies(ctx, result.localTargets)
			if fenceErr != nil {
				h.log.Warn("Uncertain bulk revocation socket fences unconfirmed", "user_id", uid, "error", fenceErr)
			}
		}
		_ = h.closeCommittedFamilies(ctx, result.revocation, fenced)
		if errors.Is(err, errCurrentSessionUnknown) {
			c.JSON(http.StatusBadRequest, gin.H{"error": errMsgCurrentSessionUnknown})
			return
		}
		if errors.Is(err, credepoch.ErrEpochMismatch) || errors.Is(err, credepoch.ErrBlocked) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
			return
		}
		if errors.Is(err, loginfamily.ErrUnavailable) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsgAuthUnavailable})
			return
		}
		if errors.Is(err, loginfamily.ErrDenied) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
			return
		}
		h.log.Error("Failed to revoke sessions")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to revoke sessions"})
		return
	}

	rowsAffected := result.revocation.RevokedRefreshRows
	fenced, localErr := h.closeCapturedFamilies(ctx, result.localTargets)
	fenceErr := errors.Join(localErr, h.closeCommittedFamilies(ctx, result.revocation, fenced))
	if mfaVerified {
		h.clearRevokeMFABudget(ctx, uid)
	}

	h.resetRevokeTracking(ctx, uid)
	message := "All other sessions revoked successfully"
	if req.IncludeCurrent {
		message = "All sessions revoked successfully"
		c.SetCookie("refresh_token", "", -1, "/", "", c.Request.TLS != nil, true)
	}
	if fenceErr != nil {
		h.log.Error("Revoked session socket fences unconfirmed", "user_id", uid, "error", fenceErr)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsgSocketFencePending})
		return
	}
	h.log.Info("Sessions revoked", "user_id", uid, "count", rowsAffected, "include_current", req.IncludeCurrent)
	h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventSession, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonSessionsRevoked, AuthMethod: securityevent.AuthSession, RouteTemplate: securityevent.RouteSessionsRevokeAll})

	c.JSON(http.StatusOK, gin.H{
		"message":         message,
		"count":           rowsAffected,
		"include_current": req.IncludeCurrent,
	})
}

// closeCommittedFamily uses the transaction's exact target without relying on
// another database read after a definite commit.
func (h *Handler) closeCommittedFamily(ctx context.Context, target loginfamily.Binding) error {
	if h.hub == nil || target.UserID == uuid.Nil || target.FamilyID == uuid.Nil {
		return errors.New("session socket fence unavailable")
	}
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), loginfamily.PostCommitCloseTimeout)
	defer cancel()
	return h.hub.DisconnectFamilyContext(closeCtx, target.UserID, target.FamilyID)
}

func commitOutcomeUnknown(err error) bool {
	var failure *loginfamily.Error
	return errors.As(err, &failure) && failure.Reason() == loginfamily.ReasonCommitUnknown
}

func (h *Handler) closeCapturedFamilies(ctx context.Context, targets []loginfamily.Binding) ([]loginfamily.Binding, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	if h.hub == nil {
		return nil, errors.New("session socket fence unavailable")
	}
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), loginfamily.PostCommitCloseTimeout)
	defer cancel()
	userID := targets[0].UserID
	families := make([]uuid.UUID, 0, len(targets))
	for _, target := range targets {
		if userID == uuid.Nil || target.UserID != userID || target.FamilyID == uuid.Nil {
			return nil, errors.New("invalid captured session socket frontier")
		}
		families = append(families, target.FamilyID)
	}
	if err := h.hub.DisconnectFamiliesContext(closeCtx, userID, families); err != nil {
		return nil, err
	}
	return targets, nil
}

type alreadyFencedCloser struct {
	closer  loginfamily.FamilyCloser
	targets []loginfamily.Binding
}

func (c alreadyFencedCloser) DisconnectFamilyContext(ctx context.Context, userID, familyID uuid.UUID) error {
	for _, target := range c.targets {
		if userID == target.UserID && familyID == target.FamilyID {
			return nil
		}
	}
	return c.closer.DisconnectFamilyContext(ctx, userID, familyID)
}

// closeCommittedFamilies leaves any failed close with the durable owner.
func (h *Handler) closeCommittedFamilies(ctx context.Context, result loginfamily.RevocationResult, alreadyFenced []loginfamily.Binding) error {
	if len(result.Owners) == 0 {
		return nil
	}
	var closer loginfamily.FamilyCloser = h.hub
	if closer != nil && len(alreadyFenced) > 0 {
		closer = alreadyFencedCloser{closer: closer, targets: alreadyFenced}
	}
	err := loginfamily.CloseCommittedFamilies(ctx, h.db, closer, result.Owners)
	if err != nil {
		h.log.Warn("Revoked session socket close deferred to reconciliation", "error", err)
	}
	return err
}

type bulkRevocationResult struct {
	revocation   loginfamily.RevocationResult
	localTargets []loginfamily.Binding
}

// revokeAllSessionsDB revokes all sessions, optionally excluding the current one.
func (h *Handler) revokeAllSessionsDB(uid string, includeCurrent bool, c *gin.Context) (bulkRevocationResult, error) {
	var result bulkRevocationResult
	options := auth.RevokeAllRefreshTokensOptions{
		ExpectedCredentialEpoch: middleware.TokenCredentialEpoch(c),
		EnforceCredentialEpoch:  true,
		ResetRevokeAllowance:    true,
	}
	if !includeCurrent {
		if err := excludeCurrentSession(c, &options); err != nil {
			return result, err
		}
	}
	if h.hub != nil {
		options.CaptureLocalFamilies = func(ctx context.Context, tx *sql.Tx, targets loginfamily.Targets) error {
			return h.captureLocalFamiliesTx(ctx, tx, uid, &result, targets)
		}
	}
	var err error
	result.revocation, err = auth.RevokeAllRefreshTokens(c.Request.Context(), h.db, uid, options)
	if errors.Is(err, auth.ErrNoActiveRefreshSession) {
		return bulkRevocationResult{}, errCurrentSessionUnknown
	}
	return result, err
}

func (h *Handler) captureLocalFamiliesTx(ctx context.Context, tx *sql.Tx, uid string, result *bulkRevocationResult, targets loginfamily.Targets) error {
	userID, err := uuid.Parse(uid)
	if err != nil || userID == uuid.Nil {
		return loginfamily.NewError(loginfamily.ReasonAccessIdentity, err)
	}
	snapshotCtx, cancel := context.WithTimeout(ctx, loginfamily.PostCommitCloseTimeout)
	defer cancel()
	candidates, err := h.hub.SnapshotUserFamiliesContext(snapshotCtx, userID)
	if err != nil {
		h.log.Warn("Local session socket snapshot unavailable", "user_id", uid, "error", err)
		return loginfamily.NewError(loginfamily.ReasonSQLUnavailable, err)
	}
	families, err := loginfamily.CapturedFamilySubsetTx(ctx, tx, targets, candidates)
	if err != nil {
		return err
	}
	for _, family := range families {
		result.localTargets = append(result.localTargets, loginfamily.Binding{UserID: userID, FamilyID: family})
	}
	return nil
}

// excludeCurrentSession spares the requester's own session by its access
// claims and refresh cookie; it fails when neither identifies the session.
func excludeCurrentSession(c *gin.Context, options *auth.RevokeAllRefreshTokensOptions) error {
	options.ExcludeSessionID = middleware.TokenSessionID(c)
	if options.ExcludeSessionID != "" {
		raw, _ := c.Get(middleware.JWTClaimsContextKey)
		claims, ok := raw.(jwt.MapClaims)
		if !ok {
			return errCurrentSessionUnknown
		}
		expiry, err := claims.GetExpirationTime()
		if err != nil || expiry == nil {
			return errCurrentSessionUnknown
		}
		options.ExcludeAccessExpiresAt = expiry.Time
	}
	if currentToken, cookieErr := c.Cookie("refresh_token"); cookieErr == nil && currentToken != "" {
		options.ExcludeTokenHash = auth.HashRefreshToken(currentToken)
	}
	if options.ExcludeSessionID == "" && options.ExcludeTokenHash == "" {
		return errCurrentSessionUnknown
	}
	return nil
}

// UpdateRevocationMode allows users to toggle between "simple" and "secure"
// revocation modes. Requires password verification (+ MFA when available).
// This setting is server-authoritative and bypasses the encrypted preferences system.
func (h *Handler) UpdateRevocationMode(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
		return
	}

	var req struct {
		Mode     string `json:"mode"`     // "simple" or "secure"
		Password string `json:"password"` // #nosec G117 -- not a secret field, receives user input for auth verification
		MFACode  string `json:"mfa_code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	if req.Mode != "simple" && req.Mode != "secure" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Mode must be 'simple' or 'secure'"})
		return
	}

	uid := userID.(string)
	ctx := c.Request.Context()

	blocked, mfaVerified := h.authenticateForRevokeWithMFA(ctx, c, uid, req.Password, req.MFACode, revocationModeSetAction)
	if blocked {
		return
	}

	err := auth.UpdateSessionRevocationMode(ctx, h.db, uid, req.Mode, middleware.TokenCredentialEpoch(c))
	if err != nil {
		h.log.Error("Failed to update revocation mode")
		switch {
		case errors.Is(err, loginfamily.ErrUnavailable):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsgAuthUnavailable})
		case errors.Is(err, loginfamily.ErrDenied), errors.Is(err, credepoch.ErrEpochMismatch), errors.Is(err, credepoch.ErrBlocked):
			c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update revocation mode"})
		}
		return
	}

	if req.Mode == "secure" {
		if h.redis == nil {
			h.log.Warn("Could not clear simple-mode authentication window")
		} else if err := h.redis.Del(ctx, simpleAuthWindowKey(uid)).Err(); err != nil {
			h.log.Warn("Could not clear simple-mode authentication window")
		}
	}
	if mfaVerified {
		h.clearRevokeMFABudget(ctx, uid)
	}

	h.log.Info("Revocation mode changed", "user_id", uid, "mode", req.Mode)
	h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventSession, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonRevocationModeChanged, AuthMethod: securityevent.AuthSession, RouteTemplate: securityevent.RouteSessionsRevocationMode})

	c.JSON(http.StatusOK, gin.H{
		"revocation_mode": req.Mode,
	})
}
