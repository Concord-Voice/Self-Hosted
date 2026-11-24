package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/hostagent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// loginChallengeTTL bounds the window between the password step (which returns a
// challenge handle) and the WebAuthn step (which redeems it). Short — the
// operator already has the hardware key in hand; a 5-minute cap limits the
// replay window of a leaked handle.
const loginChallengeTTL = 5 * time.Minute

// loginChallengePrefix namespaces the inter-step WebAuthn challenge in Redis. The
// stored value is the serialized webauthn.SessionData plus the resolved adminID;
// the handle is an opaque 256-bit CSPRNG token (never the username).
const loginChallengePrefix = "admin_login_challenge:"

// Shared error-response message strings, extracted to satisfy go:S1192 (these
// recur across the handler's JSON error responses).
const (
	msgInvalidRequestBody = "invalid request body"
	msgInternalError      = "internal error"
	msgTooManyAttempts    = "too many attempts, try again later"
	logLockoutCheckFailed = "admin lockout check failed"
)

// errInvalidAdminChallenge denotes an absent, expired or already consumed handle.
var errInvalidAdminChallenge = errors.New("invalid admin challenge")

var (
	errImportedCredentialRejected = errors.New("imported credential rejected")
	errHostAgentUnavailable       = errors.New("host agent unavailable")
	errHostAgentProtocol          = errors.New("host agent protocol error")
)

// adminWebAuthnUserHandle is the stable WebAuthn user handle for the admin RP.
// The admin RP authenticates a single logical "operator" identity per browser
// ceremony; the binding to a concrete admin row is carried server-side in the
// challenge handle, not in the WebAuthn user id. It mirrors the value the
// enrollment ceremony registers under.
var adminWebAuthnUserHandle = []byte("admin-user-handle")

// Handler bundles the admin auth surface (#1688): password + WebAuthn login,
// logout, enrollment, and admin-create. It is constructed once by the wiring
// layer and mounted by RegisterRoutes. The `log` field name is load-bearing —
// the log_emissions AST test inspects `<chain>.log.<Method>` calls.
//
//nolint:revive // Admin* package; Handler is the conventional name (no stutter — admin.Handler).
type Handler struct {
	repo     *AdminRepo
	audit    *AuditLog
	sessions *SessionStore
	lockout  *Lockout
	enroll   *EnrollmentStore
	webauthn *mfa.WebAuthnService
	redis    *redis.Client
	cfg      *config.Config
	log      *logger.Logger
	server   *ServerArea

	// dummyPasswordHash is a throwaway Argon2id hash verified against when the
	// requested username does not exist, so the password step takes the same time
	// and returns the same generic error whether or not the account exists
	// (username-enumeration defense, #1688 §7).
	dummyPasswordHash string

	// allowedAAGUIDs is the resolved ADMIN_WEBAUTHN_ALLOWED_AAGUIDS list checked at
	// enrollment.
	allowedAAGUIDs []string

	// loadActionCredentialIDs reads, from the root-owned Host Agent projection,
	// the base64url IDs of an admin's action credentials. Host root assigns
	// that purpose at import; such a key must never complete an admin login,
	// or a compromised control plane could redeem a login touch as a root
	// action.
	loadActionCredentialIDs func(adminID string) (map[string]struct{}, error)
}

// SetSecurityEvents forwards bounded Nightwatch telemetry to this handler's
// authoritative audit writer.
func (h *Handler) SetSecurityEvents(events securityevent.Emitter) {
	h.audit.SetSecurityEvents(events)
}

// NewHandler builds the admin Handler, wiring every Task 4–9 component from the
// shared DB/Redis/config. It computes a throwaway dummy password hash at
// construction (never a hard-coded literal — that would trip detect-secrets and
// be a static fingerprint) for the enumeration-defense path.
func NewHandler(db *sql.DB, rdb *redis.Client, log *logger.Logger, cfg *config.Config) (*Handler, error) {
	webauthnSvc, err := NewAdminWebAuthn(cfg)
	if err != nil {
		return nil, fmt.Errorf("admin handler: build webauthn: %w", err)
	}

	dummyHash, err := newDummyPasswordHash()
	if err != nil {
		return nil, fmt.Errorf("admin handler: seed enumeration-defense hash: %w", err)
	}

	audit := NewAuditLog(db)
	return &Handler{
		repo:              NewAdminRepo(db),
		audit:             audit,
		server:            NewServerArea(audit, log),
		sessions:          NewSessionStore(rdb, nil),
		lockout:           NewLockout(rdb, nil),
		enroll:            NewEnrollmentStore(rdb),
		webauthn:          webauthnSvc,
		redis:             rdb,
		cfg:               cfg,
		log:               log,
		dummyPasswordHash: dummyHash,
		allowedAAGUIDs:    cfg.AdminWebAuthnAllowedAAGUIDs,
		loadActionCredentialIDs: func(adminID string) (map[string]struct{}, error) {
			return hostagent.LoadActionCredentialIDs(agentAuthorizerProjectionPath, adminID)
		},
	}, nil
}

// newDummyPasswordHash hashes a random throwaway password so VerifyPassword has a
// real Argon2id hash to burn cycles against when an admin username is unknown.
func newDummyPasswordHash() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate dummy password: %w", err)
	}
	hash, err := auth.HashPassword(hex.EncodeToString(raw))
	if err != nil {
		return "", fmt.Errorf("hash dummy password: %w", err)
	}
	return hash, nil
}

// passwordRequest is the POST /admin/api/v1/auth/password body.
type passwordRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// passwordResponse is returned on a successful password step: an opaque challenge
// handle plus the WebAuthn assertion options the browser feeds navigator
// .credentials.get(). The session cookie is NOT set here — only the WebAuthn step
// mints a session.
type passwordResponse struct {
	Handle    string                        `json:"handle"`
	PublicKey *protocol.CredentialAssertion `json:"publicKey"`
}

// loginChallenge is the value stored under the opaque login handle between the
// password and WebAuthn steps.
type loginChallenge struct {
	AdminID  string `json:"admin_id"`
	Username string `json:"username"`
	// IPHash is hashIP of the address the password step ran from. Reset refunds the
	// guess on that address, not on the one that completes the sign-in. The raw IP
	// is never stored.
	IPHash  string               `json:"ip_hash"`
	Session webauthn.SessionData `json:"session"`
}

// maxCredentialBodyBytes bounds the body of the two routes that take a password:
// the guess is counted in Redis under the submitted username before the lookup,
// so an unbounded username would let any caller write megabyte-long keys.
const maxCredentialBodyBytes = 8 << 10

// genericAuthError is the single response shape for every password-step failure
// (unknown user, wrong password, locked out). Keeping it uniform prevents an
// attacker from distinguishing "no such admin" from "wrong password".
var genericAuthError = gin.H{"error": "authentication failed"}

// PasswordLogin handles step 1 of admin login: verify the password (constant
// time, enumeration-safe), and on success begin a WebAuthn assertion ceremony
// whose SessionData is stashed under an opaque handle. The guess is counted, and
// the lock checked, FIRST — before the lookup and the verify.
func (h *Handler) PasswordLogin(c *gin.Context) {
	ctx := c.Request.Context()
	ip := c.ClientIP()

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxCredentialBodyBytes)
	var req passwordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": msgInvalidRequestBody})
		return
	}

	// Count the guess FIRST, in one atomic step with the lock check — a locked
	// account/IP never reaches password verify, and a burst cannot pass the check
	// together before any guess is counted (CWE-367). It runs for every username,
	// known or not, so it adds no enumeration oracle.
	if h.reserveGuess(c, req.Username, ip, EventLoginFailure) {
		return
	}

	admin, lookupErr := h.repo.GetByUsername(ctx, req.Username)
	if h.lookupFailed(c, lookupErr, EventLoginFailure) {
		return
	}

	// Read the action-credential projection before the password verdict, for
	// known and unknown usernames alike, so an unreadable projection answers
	// 503 whatever the password: the status never confirms a password.
	probeAdminID := unknownAdminProbeID
	if lookupErr == nil && admin != nil {
		probeAdminID = admin.ID
	}
	actionIDs, actionErr := h.actionCredentialIDs(probeAdminID)
	if actionErr != nil {
		h.log.Warn("admin action credential projection unavailable", "event", EventLoginFailure)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": msgInternalError})
		return
	}

	// Enumeration defense: when the username is unknown, still run a constant-time
	// verify against the dummy hash so timing/response are uniform.
	verifyHash := h.dummyPasswordHash
	if lookupErr == nil && admin != nil {
		verifyHash = admin.PasswordHash
	}
	ok, verifyErr := auth.VerifyPassword(req.Password, verifyHash)
	h.logVerifyFault(verifyErr, EventLoginFailure)

	// Treat unknown user, disabled admin, verify error, or mismatch identically.
	if lookupErr != nil || admin == nil || verifyErr != nil || !ok || admin.Status != StatusActive {
		h.failPassword(ctx, req.Username, ip, EventLoginFailure)
		c.JSON(http.StatusUnauthorized, genericAuthError)
		return
	}

	// Password verified — begin the WebAuthn assertion ceremony for this admin's
	// registered credentials.
	user, err := h.webAuthnUserForAdmin(ctx, admin.ID)
	if err != nil {
		h.log.Error("admin webauthn user build failed", "error", err)
		// A configured admin with no/unreadable credentials cannot 2FA; fail closed
		// without leaking the cause.
		h.failPassword(ctx, req.Username, ip, EventLoginFailure)
		c.JSON(http.StatusUnauthorized, genericAuthError)
		return
	}
	// Action credentials are never offered for login. An admin left with only
	// action credentials cannot complete 2FA, exactly like one with none.
	user.Credentials = withoutActionCredentials(user.Credentials, actionIDs)
	if len(user.Credentials) == 0 {
		h.log.Warn("admin has no login credential", "event", EventLoginFailure)
		h.failPassword(ctx, req.Username, ip, EventLoginFailure)
		c.JSON(http.StatusUnauthorized, genericAuthError)
		return
	}

	assertion, session, err := h.webauthn.BeginLogin(user, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		h.log.Error("admin begin login failed", "error", err)
		h.failPassword(ctx, req.Username, ip, EventLoginFailure)
		c.JSON(http.StatusUnauthorized, genericAuthError)
		return
	}

	handle, err := h.storeLoginChallenge(ctx, loginChallenge{
		AdminID:  admin.ID,
		Username: admin.Username,
		IPHash:   hashIP(ip),
		Session:  *session,
	})
	if err != nil {
		h.log.Error("admin store login challenge failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalError})
		return
	}

	c.JSON(http.StatusOK, passwordResponse{Handle: handle, PublicKey: assertion})
}

// webauthnRequest is the POST /admin/api/v1/auth/webauthn body: the handle from
// step 1 plus the raw assertion JSON the authenticator produced.
type webauthnRequest struct {
	Handle    string          `json:"handle"`
	Assertion json.RawMessage `json:"assertion"`
}

// WebAuthnLogin handles step 2 of admin login: redeem the opaque handle, finish
// the WebAuthn assertion (UV required), and — only on success — mint a session +
// set the __Host- cookie + reset lockout + audit EventLoginSuccess.
func (h *Handler) WebAuthnLogin(c *gin.Context) {
	ctx := c.Request.Context()
	ip := c.ClientIP()

	var req webauthnRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": msgInvalidRequestBody})
		return
	}

	challenge, err := h.consumeLoginChallenge(ctx, req.Handle)
	if err != nil {
		h.challengeFailure(c, err, EventLoginFailure)
		return
	}

	// Defense-in-depth: re-check lockout in the WebAuthn step (#1688 / Gitar). The
	// handle is single-use, but an account locked AFTER its handle was issued could
	// otherwise still redeem it once. A pure read (IsLocked counts nothing), so a
	// held handle can never lock the account it is redeeming. The handle itself is
	// not a guessing surface: only a counted password step issues one.
	locked, lerr := h.lockout.IsLocked(ctx, challenge.Username, ip)
	if lerr != nil {
		h.log.Error(logLockoutCheckFailed, "event", EventLoginFailure)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalError})
		return
	}
	if locked {
		h.auditDenied(ctx, challenge.Username, EventLoginFailure, "locked_out")
		c.JSON(http.StatusTooManyRequests, gin.H{"error": msgTooManyAttempts})
		return
	}

	// Re-read the projection: root may have assigned a purpose since the
	// options were issued, and the browser need not honour allowCredentials.
	actionIDs, err := h.actionCredentialIDs(challenge.AdminID)
	if err != nil {
		h.log.Warn("admin action credential projection unavailable", "event", EventLoginFailure)
		h.auditFailure(ctx, challenge.Username, EventLoginFailure)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": msgInternalError})
		return
	}
	if parsed, perr := protocol.ParseCredentialRequestResponseBytes(req.Assertion); perr == nil {
		if _, isAction := actionIDs[base64.RawURLEncoding.EncodeToString(parsed.RawID)]; isAction {
			h.log.Warn("admin login with action credential refused", "event", EventLoginFailure)
			h.auditDenied(ctx, challenge.Username, EventLoginFailure, errCodeActionCredentialNotForLogin)
			c.JSON(http.StatusForbidden, gin.H{"error": errCodeActionCredentialNotForLogin})
			return
		}
	}

	user, err := h.webAuthnUserForAdmin(ctx, challenge.AdminID)
	if err != nil {
		h.log.Error("admin webauthn user build failed", "error", err)
		h.auditFailure(ctx, challenge.Username, EventLoginFailure)
		c.JSON(http.StatusUnauthorized, genericAuthError)
		return
	}
	// An unparseable assertion skipped the check above; with the action keys
	// removed here it can only fail verification.
	user.Credentials = withoutActionCredentials(user.Credentials, actionIDs)

	cred, err := h.webauthn.FinishLoginWithBytes(user, challenge.Session, []byte(req.Assertion))
	if err != nil {
		h.log.Warn("admin webauthn assertion rejected", "error", err)
		h.auditFailure(ctx, challenge.Username, EventLoginFailure)
		c.JSON(http.StatusUnauthorized, genericAuthError)
		return
	}
	if cred.Authenticator.CloneWarning {
		h.log.Warn("admin webauthn clone warning")
		h.auditFailure(ctx, challenge.Username, EventLoginFailure)
		c.JSON(http.StatusUnauthorized, genericAuthError)
		return
	}

	if hostAgentProjectionEnabled(filepath.Dir(agentAuthorizerProjectionPath)) {
		// An imported key has a second, root-owned counter. The agent must
		// reverify this assertion against it and advance it before any session
		// is minted; a key it has never imported answers 404 and continues to
		// the database-only finalization below.
		if verifyErr := h.verifyImportedCredential(ctx, challenge.AdminID, req.Assertion, challenge.Session); verifyErr != nil {
			h.auditFailure(ctx, challenge.Username, EventLoginFailure)
			switch {
			case errors.Is(verifyErr, errImportedCredentialRejected):
				h.log.Warn("admin imported credential assertion rejected")
				c.JSON(http.StatusUnauthorized, genericAuthError)
			case errors.Is(verifyErr, errHostAgentUnavailable):
				h.log.Warn("admin imported credential verification unavailable")
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": msgInternalError})
			default:
				h.log.Error("admin imported credential verification failed")
				c.JSON(http.StatusBadGateway, gin.H{"error": msgInternalError})
			}
			return
		}
	}

	// Every credential, imported or not, persists its advanced counter and mints
	// its generation-bound session in one transaction under the admin row lock.
	// A counter that did not advance, or an admin that is no longer active,
	// mints nothing.
	sid, err := h.repo.FinalizeWebAuthnLogin(ctx, h.sessions, challenge.AdminID, cred.ID, int64(cred.Authenticator.SignCount))
	if err != nil {
		if errors.Is(err, ErrAdminAuthenticationStateChanged) {
			h.log.Warn("admin login state changed during ceremony", "event", EventLoginFailure)
			h.auditFailure(ctx, challenge.Username, EventLoginFailure)
			c.JSON(http.StatusUnauthorized, genericAuthError)
			return
		}
		h.log.Error("admin login finalization failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalError})
		return
	}
	SetAdminSessionCookie(c, sid)

	if err := h.lockout.Reset(ctx, challenge.Username, challenge.IPHash); err != nil {
		// Non-fatal: the login succeeded. A stale counter ages out on its own (the
		// lock within lockoutMaxWindow, the attempts within lockoutAttemptsWindow of
		// the last guess); `admin unlock` clears it sooner.
		h.log.Warn("admin lockout reset failed", "event", EventLoginSuccess)
	}

	adminID := challenge.AdminID
	h.auditOK(ctx, &adminID, challenge.Username, EventLoginSuccess)
	c.JSON(http.StatusOK, gin.H{"status": "authenticated"})
}

// errCodeActionCredentialNotForLogin is the stable 403 code for an assertion
// made with a key host root imported for host actions only.
const errCodeActionCredentialNotForLogin = "action_credential_not_for_login"

// unknownAdminProbeID is the admin ID the projection is read for when the
// username is unknown, so both cases do the same work.
const unknownAdminProbeID = "00000000-0000-0000-0000-000000000000"

// actionCredentialIDs returns adminID's action credential IDs, or nil when no
// Host Agent projection is configured or its file is absent. Any other read
// failure is returned so the caller can fail closed.
func (h *Handler) actionCredentialIDs(adminID string) (map[string]struct{}, error) {
	if !hostAgentProjectionEnabled(filepath.Dir(agentAuthorizerProjectionPath)) {
		return nil, nil
	}
	ids, err := h.loadActionCredentialIDs(adminID)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errHostAgentUnavailable, err)
	}
	return ids, nil
}

// withoutActionCredentials drops every credential whose base64url ID is an
// action credential.
func withoutActionCredentials(creds []webauthn.Credential, actionIDs map[string]struct{}) []webauthn.Credential {
	if len(actionIDs) == 0 {
		return creds
	}
	kept := make([]webauthn.Credential, 0, len(creds))
	for _, cred := range creds {
		if _, isAction := actionIDs[base64.RawURLEncoding.EncodeToString(cred.ID)]; !isAction {
			kept = append(kept, cred)
		}
	}
	return kept
}

// verifyImportedCredential asks the Host Agent to verify the assertion against
// its independent root-owned counter. A 404 is the sole compatibility result:
// the key has never been imported and therefore has no second counter, so
// the caller continues with database-only finalization.
func (h *Handler) verifyImportedCredential(ctx context.Context, adminID string, assertion json.RawMessage, session webauthn.SessionData) error {
	payload, err := json.Marshal(struct {
		AdminID     string               `json:"admin_id"`
		Assertion   json.RawMessage      `json:"assertion"`
		SessionData webauthn.SessionData `json:"session_data"`
	}{AdminID: adminID, Assertion: assertion, SessionData: session})
	if err != nil {
		return fmt.Errorf("marshal imported credential assertion: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent/v1/authorizers/login-assertion", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create imported credential request: %w", err)
	}
	req.Header.Set("Content-Type", contentTypeJSON)
	resp, err := h.server.agent.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", errHostAgentUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<10)+1))
	if err != nil || len(body) > 1<<10 {
		return errHostAgentProtocol
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		var result struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &result) == nil && result.Error == "authorizer_not_imported" {
			return nil
		}
		return errHostAgentProtocol
	case http.StatusForbidden:
		return errImportedCredentialRejected
	case http.StatusServiceUnavailable:
		return errHostAgentUnavailable
	default:
		return errHostAgentProtocol
	}
}

// Logout revokes the current admin session and clears the cookie. It is mounted
// pre-auth (a stale/invalid cookie should still be clearable) and is idempotent.
func (h *Handler) Logout(c *gin.Context) {
	ctx := c.Request.Context()
	sid, err := c.Cookie(adminCookieName)
	if err == nil && sid != "" {
		var adminID *string
		if sess, gerr := h.sessions.Get(ctx, sid); gerr == nil {
			id := sess.AdminID
			adminID = &id
		}
		if rerr := h.sessions.Revoke(ctx, sid); rerr != nil {
			h.log.Warn("admin session revoke failed", "error", rerr)
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": msgInternalError})
			return
		}
		h.auditOK(ctx, adminID, "", EventLogout)
	}
	ClearAdminSessionCookie(c)
	c.JSON(http.StatusOK, gin.H{"status": "logged out"})
}

// reserveGuess counts a password guess on both lockout axes before its password
// is checked. It writes the response and returns true when the guess must not go
// on: a lockout store fault is a logged 500 (fail closed, before any password
// verify), a locked account or IP is the uniform 429 plus a locked_out audit row.
func (h *Handler) reserveGuess(c *gin.Context, username, ip, event string) bool {
	ctx := c.Request.Context()
	allowed, err := h.lockout.Reserve(ctx, username, ip)
	if err != nil {
		h.log.Error(logLockoutCheckFailed, "event", event)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalError})
		return true
	}
	if !allowed {
		h.auditDenied(ctx, username, event, "locked_out")
		c.JSON(http.StatusTooManyRequests, gin.H{"error": msgTooManyAttempts})
		return true
	}
	return false
}

// failPassword finishes a reserved guess that failed: it locks any axis that
// reached the threshold (the guess was already counted by reserveGuess) and
// writes the failure audit row. A lockout write fault is logged, never surfaced
// to the client, who always gets the uniform generic error: the guess is
// counted, so the next reserve locks the axis even if this write was lost.
func (h *Handler) failPassword(ctx context.Context, username, ip, event string) {
	if err := h.lockout.RecordCheckedFailure(ctx, username, ip); err != nil {
		h.log.Warn("admin lockout record failed", "event", event)
	}
	h.auditFailure(ctx, username, event)
}

// lookupFailed answers a database fault in the admin lookup with a logged 500 and
// reports true. An unknown username (ErrAdminNotFound) is not a fault: the caller
// carries on and answers it with the uniform 401. Treating a fault as "unknown
// user" would turn a Postgres outage into a 401 for every admin, each one counted
// as a wrong guess and given a failure audit row, with nothing logged. The fault
// does not depend on the username, so the 500 is no enumeration oracle.
func (h *Handler) lookupFailed(c *gin.Context, err error, event string) bool {
	if err == nil || errors.Is(err, ErrAdminNotFound) {
		return false
	}
	h.log.Error("admin lookup failed", "event", event)
	c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalError})
	return true
}

// logVerifyFault records a password-verification fault (a stored hash that does
// not decode) without changing the client's uniform 401. The error itself is not
// logged: an Argon2 decode failure can echo the malformed hash.
func (h *Handler) logVerifyFault(verifyErr error, event string) {
	if verifyErr != nil {
		h.log.Error("admin password verification failed", "event", event)
	}
}

// challengeFailure keeps invalid bearer handles indistinguishable while making
// storage/decode faults observable. Error text may contain stored data or Redis
// command arguments, so log only a fixed failure classification and event.
func (h *Handler) challengeFailure(c *gin.Context, err error, event string) {
	if errors.Is(err, errInvalidAdminChallenge) {
		c.JSON(http.StatusUnauthorized, genericAuthError)
		return
	}
	h.log.Error("admin challenge load failed", "event", event)
	c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalError})
}

// webAuthnUserForAdmin reconstructs the WebAuthnUser (with all registered
// credentials) for an admin so BeginLogin/FinishLogin can operate on the stored
// keys. It returns an error when the admin has no credentials (cannot 2FA).
func (h *Handler) webAuthnUserForAdmin(ctx context.Context, adminID string) (*mfa.WebAuthnUser, error) {
	stored, err := h.repo.ListCredentials(ctx, adminID)
	if err != nil {
		return nil, fmt.Errorf("list admin credentials: %w", err)
	}
	if len(stored) == 0 {
		return nil, errors.New("admin has no registered credentials")
	}
	creds := make([]webauthn.Credential, 0, len(stored))
	for _, s := range stored {
		cred := webauthn.Credential{
			ID:        s.CredentialID,
			PublicKey: s.PublicKey,
			Authenticator: webauthn.Authenticator{
				AAGUID:    s.AAGUID,
				SignCount: uint32(s.SignCount), //nolint:gosec // sign count is a small monotonic counter, no overflow
			},
		}
		for _, t := range s.Transports {
			cred.Transport = append(cred.Transport, protocol.AuthenticatorTransport(t))
		}
		creds = append(creds, cred)
	}
	return &mfa.WebAuthnUser{
		ID:          adminWebAuthnUserHandle,
		Name:        "operator",
		DisplayName: "Operator",
		Credentials: creds,
	}, nil
}

// storeLoginChallenge stashes the inter-step SessionData under a fresh opaque
// handle and returns the handle.
func (h *Handler) storeLoginChallenge(ctx context.Context, ch loginChallenge) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate login handle: %w", err)
	}
	handle := hex.EncodeToString(raw)
	payload, err := json.Marshal(ch)
	if err != nil {
		return "", fmt.Errorf("marshal login challenge: %w", err)
	}
	if err := h.redis.Set(ctx, loginChallengePrefix+handle, payload, loginChallengeTTL).Err(); err != nil {
		return "", fmt.Errorf("store login challenge: %w", err)
	}
	return handle, nil
}

// consumeLoginChallenge atomically reads-and-deletes the challenge bound to a
// handle (single-use via GETDEL). An unknown/expired/replayed handle errors.
func (h *Handler) consumeLoginChallenge(ctx context.Context, handle string) (loginChallenge, error) {
	if handle == "" {
		return loginChallenge{}, errInvalidAdminChallenge
	}
	raw, err := h.redis.GetDel(ctx, loginChallengePrefix+handle).Result()
	if errors.Is(err, redis.Nil) {
		return loginChallenge{}, errInvalidAdminChallenge
	}
	if err != nil {
		return loginChallenge{}, fmt.Errorf("load login challenge: %w", err)
	}
	var ch loginChallenge
	if err := json.Unmarshal([]byte(raw), &ch); err != nil {
		return loginChallenge{}, fmt.Errorf("decode login challenge: %w", err)
	}
	return ch, nil
}

// --- audit convenience wrappers (best-effort: a failed audit is logged, not surfaced) ---

func (h *Handler) auditOK(ctx context.Context, adminID *string, actor, event string) {
	h.writeAudit(ctx, AuditEvent{AdminID: adminID, Actor: actor, EventType: event, Result: AuditSuccess})
}

func (h *Handler) auditFailure(ctx context.Context, actor, event string) {
	h.writeAudit(ctx, AuditEvent{Actor: actor, EventType: event, Result: AuditFailure})
}

func (h *Handler) auditDenied(ctx context.Context, actor, event, reason string) {
	h.writeAudit(ctx, AuditEvent{
		Actor: actor, EventType: event, Result: AuditDenied,
		Detail: map[string]any{"reason": reason},
	})
}

func (h *Handler) writeAudit(ctx context.Context, ev AuditEvent) {
	if err := h.audit.Write(ctx, ev); err != nil {
		h.log.Warn("admin audit write failed", "error", err)
	}
}

// -----------------------------------------------------------------------------
// Enrollment (#1688 §5): a pending admin authenticates username+password+token,
// registers a hardware key (AAGUID-checked), and is flipped to active. WebAuthn
// registration is structurally a two-step ceremony — begin (server issues the
// creation challenge) then finish (client posts the attestation) — so it is
// surfaced as /enroll/begin + /enroll/finish, both pre-auth (the token + password
// ARE the authentication). The single-use enrollment token is consumed at begin.
// -----------------------------------------------------------------------------

// enrollChallengePrefix namespaces the inter-step registration SessionData.
const enrollChallengePrefix = "admin_enroll_challenge:"

// enrollChallengeTTL bounds the begin→finish window for the registration ceremony.
const enrollChallengeTTL = 5 * time.Minute

// enrollBeginRequest is the POST /admin/api/v1/enroll/begin body.
type enrollBeginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Token    string `json:"token"`
}

// enrollBeginResponse returns the registration options + an opaque handle.
type enrollBeginResponse struct {
	Handle    string                       `json:"handle"`
	PublicKey *protocol.CredentialCreation `json:"publicKey"`
}

// enrollChallenge is the value stored under the begin handle.
type enrollChallenge struct {
	AdminID              string `json:"admin_id"`
	Username             string `json:"username"`
	CredentialGeneration int64  `json:"credential_generation"`
	// IPHash is hashIP of the address EnrollBegin ran from; see loginChallenge.
	IPHash  string               `json:"ip_hash"`
	Session webauthn.SessionData `json:"session"`
}

// EnrollBegin verifies password + single-use token, then begins a hardware-key
// registration ceremony bound to the pending admin. The token is consumed here
// (single-use); a failed finish requires a fresh token via reset-enrollment.
//
// Enroll shares the login Lockout (#216): it counts each guess on the same
// per-account and per-IP counters BEFORE the password is checked (#221), so a
// password guessed here costs the same budget as one guessed at login and a
// burst cannot get more than the threshold's worth of guesses verified.
func (h *Handler) EnrollBegin(c *gin.Context) {
	ctx := c.Request.Context()
	ip := c.ClientIP()

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxCredentialBodyBytes)
	var req enrollBeginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": msgInvalidRequestBody})
		return
	}

	if h.reserveGuess(c, req.Username, ip, EventEnrollComplete) {
		return
	}

	adminUser, lookupErr := h.repo.GetByUsername(ctx, req.Username)
	if h.lookupFailed(c, lookupErr, EventEnrollComplete) {
		return
	}
	verifyHash := h.dummyPasswordHash
	if lookupErr == nil && adminUser != nil {
		verifyHash = adminUser.PasswordHash
	}
	ok, verifyErr := auth.VerifyPassword(req.Password, verifyHash)
	h.logVerifyFault(verifyErr, EventEnrollComplete)
	// A non-pending admin is refused AFTER the constant-time verify, never by an
	// early return, so an active admin and an unknown user look the same.
	if lookupErr != nil || adminUser == nil || verifyErr != nil || !ok || adminUser.Status != StatusPending {
		h.failEnroll(c, req.Username, ip)
		return
	}

	// Consume the single-use enrollment token and verify it binds to THIS admin.
	// A store fault is a 500, not a failed guess: it records no failure. (The
	// guess itself was counted before the verify, so the fault still costs one
	// attempt until the next completed enrollment clears the count.)
	boundAdminID, boundGeneration, err := h.enroll.ConsumeEnrollmentToken(ctx, req.Token)
	if err != nil && !errors.Is(err, ErrEnrollTokenInvalid) {
		h.log.Error("admin consume enrollment token failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalError})
		return
	}
	if err != nil || boundAdminID != adminUser.ID || boundGeneration != adminUser.CredentialGeneration {
		h.failEnroll(c, req.Username, ip)
		return
	}

	creation, session, err := h.webauthn.BeginRegistration(adminRegistrationUser(), adminRegistrationOptions()...)
	if err != nil {
		h.log.Error("admin begin registration failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalError})
		return
	}

	handle, err := h.storeEnrollChallenge(ctx, enrollChallenge{
		AdminID:              adminUser.ID,
		Username:             adminUser.Username,
		CredentialGeneration: adminUser.CredentialGeneration,
		IPHash:               hashIP(ip),
		Session:              *session,
	})
	if err != nil {
		h.log.Error("admin store enroll challenge failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalError})
		return
	}

	c.JSON(http.StatusOK, enrollBeginResponse{Handle: handle, PublicKey: creation})
}

// enrollFinishRequest is the POST /admin/api/v1/enroll/finish body.
type enrollFinishRequest struct {
	Handle         string          `json:"handle"`
	Attestation    json.RawMessage `json:"attestation"`
	CredentialName string          `json:"credential_name"`
}

// EnrollFinish redeems the begin handle, completes the registration (AAGUID
// allow-list enforced inside FinishAdminRegistration), persists the credential,
// and flips the admin to active. Audits enroll_complete.
func (h *Handler) EnrollFinish(c *gin.Context) {
	ctx := c.Request.Context()

	var req enrollFinishRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": msgInvalidRequestBody})
		return
	}

	challenge, err := h.consumeEnrollChallenge(ctx, req.Handle)
	if err != nil {
		h.challengeFailure(c, err, EventEnrollComplete)
		return
	}

	// No lockout re-check here, unlike WebAuthnLogin: the handle is 256-bit and
	// only a successful EnrollBegin issues it, so Finish is not a guessing
	// surface. A lock landing between Begin and Finish would burn the already
	// spent enrollment token and strand the admin until an operator resets it.
	user := adminRegistrationUser()
	if _, err := FinishAdminRegistration(ctx, h.webauthn, h.repo, AdminRegistrationInput{
		User:                 user,
		Session:              challenge.Session,
		Request:              httpRequestWithBody(c, req.Attestation),
		AdminID:              challenge.AdminID,
		CredentialGeneration: challenge.CredentialGeneration,
		AllowedAAGUIDs:       h.allowedAAGUIDs,
		CredentialName:       req.CredentialName,
	}); err != nil {
		// A credential-save fault is the server's, not a rejected key: a logged
		// 500 with no failure audit row. Attestation and AAGUID rejections stay a
		// Warn, the audit row and the uniform 401.
		if errors.Is(err, ErrCredentialPersist) {
			h.log.Error("admin credential persist failed", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalError})
			return
		}
		h.log.Warn("admin finish registration rejected", "error", err)
		h.auditFailure(ctx, challenge.Username, EventEnrollComplete)
		c.JSON(http.StatusUnauthorized, genericAuthError)
		return
	}

	// Reset only after the key is enrolled, as WebAuthnLogin resets only after
	// the second factor. Non-fatal: a stale counter ages out on its own (see
	// WebAuthnLogin); `admin unlock` clears it sooner.
	if err := h.lockout.Reset(ctx, challenge.Username, challenge.IPHash); err != nil {
		h.log.Warn("admin lockout reset failed", "event", EventEnrollComplete)
	}

	adminID := challenge.AdminID
	h.auditOK(ctx, &adminID, challenge.Username, EventEnrollComplete)
	c.JSON(http.StatusOK, gin.H{"status": "enrolled"})
}

// failEnroll finishes a reserved EnrollBegin guess that failed and writes the
// uniform 401.
func (h *Handler) failEnroll(c *gin.Context, username, ip string) {
	h.failPassword(c.Request.Context(), username, ip, EventEnrollComplete)
	c.JSON(http.StatusUnauthorized, genericAuthError)
}

// adminRegistrationUser is the WebAuthnUser used for the admin registration
// ceremony (same stable handle as login).
func adminRegistrationUser() *mfa.WebAuthnUser {
	return &mfa.WebAuthnUser{
		ID:          adminWebAuthnUserHandle,
		Name:        "operator",
		DisplayName: "Operator",
	}
}

// adminRegistrationOptions is the admin registration policy: direct attestation
// (so the AAGUID is conveyed for the allow-list check), resident key required,
// cross-platform authenticator, user verification required.
func adminRegistrationOptions() []webauthn.RegistrationOption {
	rk := true
	return []webauthn.RegistrationOption{
		webauthn.WithConveyancePreference(protocol.PreferDirectAttestation),
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			AuthenticatorAttachment: protocol.CrossPlatform,
			RequireResidentKey:      &rk,
			ResidentKey:             protocol.ResidentKeyRequirementRequired,
			UserVerification:        protocol.VerificationRequired,
		}),
	}
}

func (h *Handler) storeEnrollChallenge(ctx context.Context, ch enrollChallenge) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate enroll handle: %w", err)
	}
	handle := hex.EncodeToString(raw)
	payload, err := json.Marshal(ch)
	if err != nil {
		return "", fmt.Errorf("marshal enroll challenge: %w", err)
	}
	challengeKey := enrollChallengePrefix + handle
	indexKey := enrollChallengeIndexKey(ch.AdminID, ch.CredentialGeneration)
	if _, err := h.redis.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Set(ctx, challengeKey, payload, enrollChallengeTTL)
		pipe.SAdd(ctx, indexKey, challengeKey)
		pipe.Expire(ctx, indexKey, enrollChallengeTTL)
		return nil
	}); err != nil {
		return "", fmt.Errorf("store enroll challenge: %w", err)
	}
	return handle, nil
}

func (h *Handler) consumeEnrollChallenge(ctx context.Context, handle string) (enrollChallenge, error) {
	if handle == "" {
		return enrollChallenge{}, errInvalidAdminChallenge
	}
	challengeKey := enrollChallengePrefix + handle
	raw, err := h.redis.GetDel(ctx, challengeKey).Result()
	if errors.Is(err, redis.Nil) {
		return enrollChallenge{}, errInvalidAdminChallenge
	}
	if err != nil {
		return enrollChallenge{}, fmt.Errorf("load enroll challenge: %w", err)
	}
	var ch enrollChallenge
	if err := json.Unmarshal([]byte(raw), &ch); err != nil {
		return enrollChallenge{}, fmt.Errorf("decode enroll challenge: %w", err)
	}
	// The index only lets reset find live ceremonies; the challenge is already
	// consumed, so a failed index cleanup must not spend it. A leftover member
	// names a key that no longer exists and expires with the index.
	if err := h.redis.SRem(ctx, enrollChallengeIndexKey(ch.AdminID, ch.CredentialGeneration), challengeKey).Err(); err != nil {
		h.log.Warn("admin enroll challenge index cleanup failed", "event", EventEnrollComplete)
	}
	return ch, nil
}

// httpRequestWithBody wraps a JSON body in the *http.Request shape FinishRegistration
// parses (it reads request.Body). The gin context's request is reused for its
// context; only the Body is swapped to the supplied attestation JSON.
func httpRequestWithBody(c *gin.Context, body []byte) *http.Request {
	r := c.Request.Clone(c.Request.Context())
	r.Body = io.NopCloser(bytes.NewReader(body))
	return r
}

// -----------------------------------------------------------------------------
// Admin-create (#1688 §5): an authenticated active admin provisions a new pending
// admin and receives a one-time enrollment token in the response body (shown
// once, handed over out-of-band). Behind AdminAuthRequired.
// -----------------------------------------------------------------------------

// createAdminRequest is the POST /admin/api/v1/admins body.
type createAdminRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// CreateAdmin provisions a pending admin and mints a one-time enrollment token,
// returned ONCE in the response body. The acting admin id comes from the session
// (set by AdminAuthRequired). The password set here is a temporary credential the
// new operator changes is out-of-scope; it is hashed with Argon2id and never
// echoed back.
func (h *Handler) CreateAdmin(c *gin.Context) {
	ctx := c.Request.Context()
	actingAdminID := c.GetString(adminIDContextKey)

	var req createAdminRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": msgInvalidRequestBody})
		return
	}
	if req.Username == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "username is required"})
		return
	}
	if err := auth.ValidatePasswordStrength(req.Password); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "password too weak"})
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		h.log.Error("admin hash password failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalError})
		return
	}

	created, err := h.repo.CreatePending(ctx, req.Username, hash)
	if err != nil {
		if errors.Is(err, ErrDuplicateUsername) {
			c.JSON(http.StatusConflict, gin.H{"error": "username already exists"})
			return
		}
		h.log.Error("admin create pending failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalError})
		return
	}

	token, err := h.enroll.MintEnrollmentToken(ctx, created.ID, created.CredentialGeneration)
	if err != nil {
		h.log.Error("admin mint enrollment token failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalError})
		return
	}

	acting := actingAdminID
	h.auditOK(ctx, &acting, req.Username, EventBootstrap)

	c.JSON(http.StatusCreated, gin.H{
		"username": created.Username,
		"status":   string(created.Status),
		// The token is surfaced ONCE here; it is never stored in plaintext and
		// never logged.
		"enrollment_token": token,
	})
}
