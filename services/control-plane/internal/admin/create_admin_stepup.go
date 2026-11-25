package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

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
//  2. that same admin answered, within adminStepUpTTL, a challenge this server
//     issued for (admin id, create_admin), with user verification,
//  3. the handle was redeemed exactly once (GETDEL),
//  4. the credential's counter advanced and no clone warning was raised, and
//  5. the admin_created audit row was committed.
//
// It breaks if a handle can be redeemed twice, by another admin, or for
// another action; if an assertion without user verification is accepted; or
// if a token is minted when the audit row was not written. The handles live
// under their own key prefix, so a server-action step-up handle can never be
// redeemed here, nor a create_admin handle for a server action.

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

// adminStepUp is the value stored under a create_admin step-up handle.
type adminStepUp struct {
	AdminID string               `json:"admin_id"`
	Action  string               `json:"action"`
	Session webauthn.SessionData `json:"session"`
}

// adminStepUpProof is the step-up carried in the create request.
type adminStepUpProof struct {
	Handle    string          `json:"handle"`
	Assertion json.RawMessage `json:"assertion"`
}

// CreateAdminStepUpBegin starts a WebAuthn assertion that authorises one
// admin creation and returns its handle. POST {} (an empty body is accepted).
func (h *Handler) CreateAdminStepUpBegin(c *gin.Context) {
	ctx := c.Request.Context()
	adminID := c.GetString(adminIDContextKey)
	var req struct{}
	if decodeStrict(c, &req, maxAdminStepUpBeginBody) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}
	user, err := h.webAuthnUserForAdmin(ctx, adminID)
	if err != nil {
		h.log.Warn("admin create step-up user build failed", "error", err)
		h.auditCreateAdmin(ctx, adminID, "", AuditDenied, "stepup_unavailable")
		c.JSON(http.StatusForbidden, gin.H{"error": "stepup_unavailable"})
		return
	}
	assertion, session, err := h.webauthn.BeginLogin(user, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		h.log.Error("admin create step-up begin failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal_error"})
		return
	}
	handle, err := h.storeAdminStepUp(ctx, adminStepUp{AdminID: adminID, Action: actionCreateAdmin, Session: *session})
	if err != nil {
		h.log.Error("admin create step-up store failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal_error"})
		return
	}
	c.JSON(http.StatusOK, passwordResponse{Handle: handle, PublicKey: assertion})
}

// requireCreateAdminStepUp redeems the request's step-up and writes the
// refusal itself. It reports whether the caller may go on.
func (h *Handler) requireCreateAdminStepUp(c *gin.Context, username string, proof *adminStepUpProof) bool {
	ctx := c.Request.Context()
	adminID := c.GetString(adminIDContextKey)
	reason := ""
	switch {
	case proof == nil:
		reason = "stepup_required"
	case !h.redeemAdminStepUp(ctx, adminID, actionCreateAdmin, proof.Handle, proof.Assertion):
		reason = "stepup_failed"
	default:
		return true
	}
	h.auditCreateAdmin(ctx, adminID, username, AuditDenied, reason)
	c.JSON(http.StatusForbidden, gin.H{"error": reason})
	return false
}

// redeemAdminStepUp redeems a step-up handle once. It must belong to this
// admin and this action, and the assertion must verify with user
// verification, advance the credential's counter and raise no clone warning.
func (h *Handler) redeemAdminStepUp(ctx context.Context, adminID, action, handle string, assertion json.RawMessage) bool {
	if adminID == "" || !adminStepUpHandle.MatchString(handle) || len(assertion) == 0 {
		return false
	}
	raw, err := h.redis.GetDel(ctx, adminStepUpPrefix+handle).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			h.log.Warn("admin create step-up load failed", "error", err)
		}
		return false
	}
	var ch adminStepUp
	switch {
	case json.Unmarshal([]byte(raw), &ch) != nil:
		h.log.Warn("admin create step-up record unreadable")
		return false
	case ch.AdminID != adminID:
		h.log.Warn("admin create step-up handle used by another admin")
		return false
	case ch.Action != action:
		h.log.Warn("admin create step-up handle used for another action")
		return false
	}
	user, err := h.webAuthnUserForAdmin(ctx, adminID)
	if err != nil {
		h.log.Warn("admin create step-up user build failed", "error", err)
		return false
	}
	cred, err := h.webauthn.FinishLoginWithBytes(user, ch.Session, assertion)
	if err != nil {
		h.log.Warn("admin create step-up assertion rejected", "error", err)
		return false
	}
	if cred.Authenticator.CloneWarning {
		h.log.Warn("admin create step-up clone warning")
		return false
	}
	if err := h.repo.UpdateCredentialSignCount(ctx, cred.ID, int64(cred.Authenticator.SignCount)); err != nil {
		h.log.Warn("admin create step-up sign-count persist failed", "error", err)
		return false
	}
	return true
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
	if err := h.redis.Set(ctx, adminStepUpPrefix+handle, payload, adminStepUpTTL).Err(); err != nil {
		return "", fmt.Errorf("store admin step-up: %w", err)
	}
	return handle, nil
}

// auditCreateAdmin records one admin_created refusal or failure against the
// acting admin. These rows are best-effort, like every other admin audit row.
// The success row is not written here: CreateAdmin commits it in the same
// transaction as the pending admin, so no admin exists without one, and it
// carries the new admin's id (detail.created_admin_id), by which the sign-in
// notice counts it. The rows written here carry no id, and the notice reads
// only success rows. A success row can outlive its admin: when the
// enrollment-token mint then fails, the pending admin is removed and a
// token_mint_failed row written here records that the creation did not
// complete; the notice then finds no admin with that id and skips it.
func (h *Handler) auditCreateAdmin(ctx context.Context, actingAdminID, username, result, reason string) {
	acting := actingAdminID
	ev := AuditEvent{AdminID: &acting, Actor: username, EventType: EventAdminCreated, Result: result}
	if reason != "" {
		ev.Detail = map[string]any{"reason": reason}
	}
	h.writeAudit(ctx, ev)
}
