package admin

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/redis/go-redis/v9"
)

// Creating an admin mints an identity that can pass every step-up, so it needs
// a step-up of its own: a fresh, user-verified WebAuthn assertion from the
// acting admin, bound to the create_admin action and redeemed once.
//
// Invariant: an enrollment token for a new admin exists only after
//  1. the acting admin's session passed AdminAuthRequired,
//  2. that same admin, in that same session, answered within adminStepUpTTL a
//     challenge this server issued for (admin id, session, create_admin),
//     with user verification,
//  3. the handle was redeemed exactly once (GETDEL),
//  4. the credential's counter advanced and no clone warning was raised, and
//  5. the admin_created audit row was committed.
//
// It breaks if a handle can be redeemed twice, by another admin, by another
// session of the same admin, or for another action; if an assertion without
// user verification is accepted; or if a token is minted when the audit row
// was not written. The handles live under their own key prefix, so a
// server-action step-up handle can never be redeemed here, nor a create_admin
// handle for a server action, and the key names the admin who began it, so
// another admin's attempt neither redeems nor spends it. The record holds the
// binding of the session that began it (adminSessionBinding, a SHA-256 of the
// session id, never the id itself). Another session of the same admin reaches
// the key, so its attempt is refused with stepup_wrong_session after the
// GETDEL: it spends the handle. That is deliberate: a session holding the
// handle and the assertion is a leak, and burning the handle ends it; the
// owner begins a new step-up.

const (
	adminStepUpPrefix = "admin_create_stepup:"
	// adminStepUpTTL is short: the operator taps the key right after asking.
	adminStepUpTTL = 2 * time.Minute
	// actionCreateAdmin is the only action this step-up authorises.
	actionCreateAdmin = "create_admin"
	// maxAdminStepUpBeginBody is the begin body limit; the body is {}.
	maxAdminStepUpBeginBody = 1 << 10
)

var adminStepUpHandle = regexp.MustCompile(`^[0-9a-f]{64}$`)

// adminStepUpKey is the Redis key of a handle. It names the admin who began
// it, so a redeem by any other admin looks up a key that does not exist and
// cannot spend that admin's handle.
func adminStepUpKey(adminID, handle string) string {
	return adminStepUpPrefix + adminID + ":" + handle
}

// adminStepUp is the value stored under a create_admin step-up handle.
type adminStepUp struct {
	AdminID string `json:"admin_id"`
	// AdminSession is adminSessionBinding of the admin session that began the
	// step-up; only that session may redeem it.
	AdminSession string               `json:"admin_session"`
	Action       string               `json:"action"`
	Session      webauthn.SessionData `json:"session"`
}

// adminStepUpProof is the step-up carried in the create request.
type adminStepUpProof struct {
	Handle    string          `json:"handle"`
	Assertion json.RawMessage `json:"assertion"`
}

// CreateAdminStepUpBegin starts a WebAuthn assertion that authorises one
// admin creation and returns its handle. POST {} (an empty body is accepted;
// any field, or a body over 1 KiB, is a 400). An admin with no enrolled key
// gets 403 stepup_unavailable and a denied row; a storage or ceremony fault
// gets a 5xx and a failure row.
func (h *Handler) CreateAdminStepUpBegin(c *gin.Context) {
	adminID := c.GetString(adminIDContextKey)
	h.beginAdminStepUp(c, actionCreateAdmin, func(ctx context.Context, result, reason string) {
		h.auditCreateAdmin(ctx, adminID, "", result, reason, "")
	})
}

// beginAdminStepUp issues a step-up bound to (admin, session, action) and
// answers the request. audit writes the action's own denied or failure row.
func (h *Handler) beginAdminStepUp(c *gin.Context, action string, audit func(ctx context.Context, result, reason string)) {
	ctx := c.Request.Context()
	adminID := c.GetString(adminIDContextKey)
	adminSession := c.GetString(adminSessionContextKey)
	if adminSession == "" {
		// AdminAuthRequired always sets it; without it the handle could not
		// be bound to a session, so refuse rather than issue an unbound one.
		h.log.Error("admin step-up begin without a session binding", "admin_id", adminID, "action", action)
		audit(ctx, AuditFailure, "stepup_begin_no_session")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal_error"})
		return
	}
	var req struct{}
	if err := decodeStrict(c, &req, maxAdminStepUpBeginBody); err != nil {
		h.log.Warn("admin step-up begin body refused", "admin_id", adminID, "action", action, "error", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}
	user, err := h.webAuthnUserForAdmin(ctx, adminID)
	switch {
	case errors.Is(err, errNoAdminCredentials):
		h.log.Warn("admin step-up refused: no enrolled key", "admin_id", adminID, "action", action)
		audit(ctx, AuditDenied, "stepup_unavailable")
		c.JSON(http.StatusForbidden, gin.H{"error": "stepup_unavailable"})
		return
	case err != nil:
		h.log.Error("admin step-up credential lookup failed", "admin_id", adminID, "action", action, "error", err)
		audit(ctx, AuditFailure, "stepup_begin_unavailable")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errServiceUnavailable})
		return
	}
	assertion, session, err := h.webauthn.BeginLogin(user, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		h.log.Error("admin step-up begin failed", "admin_id", adminID, "action", action, "error", err)
		audit(ctx, AuditFailure, "stepup_begin_failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal_error"})
		return
	}
	handle, err := h.storeAdminStepUp(ctx, adminStepUp{
		AdminID: adminID, AdminSession: adminSession, Action: action, Session: *session,
	})
	if err != nil {
		h.log.Error("admin step-up store failed", "admin_id", adminID, "action", action, "error", err)
		audit(ctx, AuditFailure, "stepup_begin_failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal_error"})
		return
	}
	c.JSON(http.StatusOK, passwordResponse{Handle: handle, PublicKey: assertion})
}

// errServiceUnavailable is the 503 body when a step-up cannot be checked
// because storage failed.
const errServiceUnavailable = "service_unavailable"

// stepUpRefusal says why a create_admin step-up was not accepted. reason is
// the audit row's reason; unavailable marks a storage fault (503 and a
// failure row) as opposed to a proof that did not verify (403 stepup_failed
// and a denied row). Every refusal fails closed.
type stepUpRefusal struct {
	reason      string
	unavailable bool
}

func stepUpDenied(reason string) *stepUpRefusal { return &stepUpRefusal{reason: reason} }

func stepUpUnavailable(reason string) *stepUpRefusal {
	return &stepUpRefusal{reason: reason, unavailable: true}
}

// requireCreateAdminStepUp redeems the request's step-up and writes the
// refusal itself. It reports whether the caller may go on.
func (h *Handler) requireCreateAdminStepUp(c *gin.Context, username string, proof *adminStepUpProof) bool {
	ctx := c.Request.Context()
	adminID := c.GetString(adminIDContextKey)
	if proof == nil {
		h.auditCreateAdmin(ctx, adminID, username, AuditDenied, "stepup_required", "")
		c.JSON(http.StatusForbidden, gin.H{"error": "stepup_required"})
		return false
	}
	adminSession := c.GetString(adminSessionContextKey)
	refusal := h.redeemAdminStepUp(ctx, adminID, adminSession, actionCreateAdmin, proof.Handle, proof.Assertion)
	switch {
	case refusal == nil:
		return true
	case refusal.unavailable:
		h.auditCreateAdmin(ctx, adminID, username, AuditFailure, refusal.reason, "")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errServiceUnavailable})
	default:
		h.auditCreateAdmin(ctx, adminID, username, AuditDenied, refusal.reason, "")
		c.JSON(http.StatusForbidden, gin.H{"error": "stepup_failed"})
	}
	return false
}

// redeemAdminStepUp redeems a step-up handle once. It must belong to this
// admin, this admin session and this action, and the assertion must verify
// with user verification, advance the credential's counter and raise no
// clone warning. It returns nil only when all of that holds. The GETDEL runs
// before any of those checks, so a refused attempt that reached the key (the
// wrong session, the wrong action, a bad assertion) spends the handle.
func (h *Handler) redeemAdminStepUp(ctx context.Context, adminID, adminSession, action, handle string, assertion json.RawMessage) *stepUpRefusal {
	if adminID == "" || !adminStepUpHandle.MatchString(handle) || len(assertion) == 0 {
		return stepUpDenied("stepup_malformed")
	}
	raw, err := h.redis.GetDel(ctx, adminStepUpKey(adminID, handle)).Result()
	switch {
	case errors.Is(err, redis.Nil):
		// Expired, already redeemed, never issued, or begun by another
		// admin (whose key this lookup cannot reach, so it stays usable).
		return stepUpDenied("stepup_unknown_handle")
	case err != nil:
		h.log.Error("admin create step-up load failed", "admin_id", adminID, "error", err)
		return stepUpUnavailable("stepup_store_unavailable")
	}
	var ch adminStepUp
	if err := json.Unmarshal([]byte(raw), &ch); err != nil {
		h.log.Error("admin create step-up record unreadable", "admin_id", adminID, "error", err)
		return stepUpUnavailable("stepup_record_unreadable")
	}
	switch {
	case ch.AdminID != adminID:
		h.log.Warn("admin create step-up handle used by another admin", "admin_id", adminID)
		return stepUpDenied("stepup_wrong_admin")
	case !sameAdminSession(ch.AdminSession, adminSession):
		h.log.Warn("admin create step-up handle used by another session", "admin_id", adminID)
		return stepUpDenied("stepup_wrong_session")
	case ch.Action != action:
		h.log.Warn("admin create step-up handle used for another action", "admin_id", adminID)
		return stepUpDenied("stepup_wrong_action")
	}
	user, err := h.webAuthnUserForAdmin(ctx, adminID)
	switch {
	case errors.Is(err, errNoAdminCredentials):
		h.log.Warn("admin create step-up refused: no enrolled key", "admin_id", adminID)
		return stepUpDenied("stepup_no_credentials")
	case err != nil:
		h.log.Error("admin create step-up credential lookup failed", "admin_id", adminID, "error", err)
		return stepUpUnavailable("stepup_credential_lookup_failed")
	}
	cred, err := h.webauthn.FinishLoginWithBytes(user, ch.Session, assertion)
	if err != nil {
		h.log.Warn("admin create step-up assertion rejected", "admin_id", adminID, "error", err)
		return stepUpDenied("stepup_assertion_rejected")
	}
	if cred.Authenticator.CloneWarning {
		h.log.Warn("admin create step-up clone warning", "admin_id", adminID)
		return stepUpDenied("stepup_clone_warning")
	}
	if hostAgentProjectionEnabled(filepath.Dir(agentAuthorizerProjectionPath)) {
		// One key, one counter: a key root imported is checked against, and
		// advances, its root-owned counter here as at login, before the
		// database counter. A never-imported key answers 404 and goes on.
		switch err := h.verifyImportedCredential(ctx, adminID, assertion, ch.Session); {
		case err == nil:
		case errors.Is(err, errImportedCredentialRejected):
			h.log.Warn("admin step-up imported credential rejected", "admin_id", adminID)
			return stepUpDenied("stepup_root_counter_rejected")
		default:
			h.log.Warn("admin step-up imported credential verification unavailable", "admin_id", adminID)
			return stepUpUnavailable("stepup_root_counter_unavailable")
		}
	}
	err = h.repo.UpdateCredentialSignCount(ctx, cred.ID, int64(cred.Authenticator.SignCount))
	switch {
	case errors.Is(err, errSignCountNotAdvanced):
		h.log.Warn("admin create step-up counter did not advance", "admin_id", adminID)
		return stepUpDenied("stepup_counter_not_advanced")
	case err != nil:
		h.log.Error("admin create step-up sign-count persist failed", "admin_id", adminID, "error", err)
		return stepUpUnavailable("stepup_sign_count_persist_failed")
	}
	return nil
}

// sameAdminSession reports, in constant time, whether two session bindings
// are equal. An empty binding on either side never matches: a record without
// one, or a request without one, cannot redeem.
func sameAdminSession(stored, current string) bool {
	if stored == "" || current == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(stored), []byte(current)) == 1
}

func (h *Handler) storeAdminStepUp(ctx context.Context, ch adminStepUp) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate admin step-up handle: %w", err)
	}
	handle := hex.EncodeToString(raw)
	payload, err := json.Marshal(ch)
	if err != nil {
		return "", fmt.Errorf("marshal admin step-up: %w", err)
	}
	if err := h.redis.Set(ctx, adminStepUpKey(ch.AdminID, handle), payload, adminStepUpTTL).Err(); err != nil {
		return "", fmt.Errorf("store admin step-up: %w", err)
	}
	return handle, nil
}

// auditCreateAdmin records one admin_created denied or failure row against
// the acting admin, best-effort like every other admin audit row: when the
// write fails it is logged at Error and the request goes on. The success row
// is not written here: it commits with the pending admin inside CreateAdmin's
// WriteWith transaction, so no admin exists without one, and it carries the
// new admin's id (detail.created_admin_id), by which the sign-in notice
// counts it. The notice reads only success rows, so the rows written here
// are never counted, even when they name the created admin's id (a mint or
// commit failure does, so the record shows which admin was removed). A
// success row can outlive its admin: when the enrollment-token mint fails
// after it committed, the pending admin is removed and a token_mint_failed
// row is written here; the notice then finds no admin with that id and
// skips it. The actor is truncated to the username limit.
func (h *Handler) auditCreateAdmin(ctx context.Context, actingAdminID, username, result, reason, createdAdminID string) {
	acting := actingAdminID
	actor := truncateRunes(username, MaxAdminUsernameLength)
	ev := AuditEvent{AdminID: &acting, Actor: actor, EventType: EventAdminCreated, Result: result}
	if reason != "" || createdAdminID != "" {
		ev.Detail = map[string]any{}
		if reason != "" {
			ev.Detail["reason"] = reason
		}
		if createdAdminID != "" {
			ev.Detail[detailCreatedAdminID] = createdAdminID
		}
	}
	if err := h.audit.Write(ctx, ev); err != nil {
		h.log.Error("admin create audit row not written",
			"admin_id", actingAdminID, "created_admin_id", createdAdminID,
			"username", sanitizeAuditString(actor), "result", result, "reason", reason, "error", err)
	}
}

// truncateRunes returns s cut to at most n runes.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
