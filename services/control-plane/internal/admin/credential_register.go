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
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/redis/go-redis/v9"
)

// A signed-in admin may register another hardware key for themselves (a
// backup). Every key both signs in and, once host root imports it,
// authorizes host actions, so adding one is as sensitive as creating an
// admin and needs the same fresh step-up.
//
// Invariant: a key is added only after
//  1. the session passed AdminAuthRequired,
//  2. that admin, in that session, redeemed once an add_credential step-up
//     signed by one of their existing keys (redeemAdminStepUp),
//  3. the registration challenge was issued to that admin and session, with
//     every existing key excluded, and redeemed once by that session,
//  4. the attestation passed the enrollment checks (resident key, user
//     verification, AAGUID allow-list), and
//  5. under the admin row lock the admin is still active at the credential
//     generation the challenge was bound to and holds fewer than
//     MaxAdminCredentials keys.
//
// All admins share adminWebAuthnUserHandle, so a second resident credential
// on the same authenticator would overwrite the first; excludeCredentials
// makes the browser refuse to register an authenticator that already holds
// one of this admin's keys. A backup must be a different physical key.

const (
	// actionAddCredential is the step-up action that authorises one
	// registration begin.
	actionAddCredential = "add_credential"
	// credentialRegisterPrefix keys a pending registration challenge.
	credentialRegisterPrefix = "admin_credential_register:"
	// maxCredentialRegisterBody bounds both bodies: one assertion or one
	// attestation and a short name.
	maxCredentialRegisterBody = 64 << 10
	// errCodeCredentialLimit is the 409 body when the admin holds the cap.
	errCodeCredentialLimit = "credential_limit_reached"
)

// credentialRegistration is the value stored under a registration handle.
type credentialRegistration struct {
	AdminID              string               `json:"admin_id"`
	AdminSession         string               `json:"admin_session"`
	CredentialGeneration int64                `json:"credential_generation"`
	Session              webauthn.SessionData `json:"session"`
}

func credentialRegisterKey(adminID, handle string) string {
	return credentialRegisterPrefix + adminID + ":" + handle
}

// adminCredentialView is one row of the list response. It names the key
// without exposing its credential ID or public key.
type adminCredentialView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// ListOwnCredentials returns the signed-in admin's own keys, oldest first.
func (h *Handler) ListOwnCredentials(c *gin.Context) {
	ctx := c.Request.Context()
	adminID := c.GetString(adminIDContextKey)
	stored, err := h.repo.ListCredentials(ctx, adminID)
	if err != nil {
		h.log.Error("admin credential list failed", "admin_id", adminID, "error", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errServiceUnavailable})
		return
	}
	views := make([]adminCredentialView, 0, len(stored))
	for _, s := range stored {
		views = append(views, adminCredentialView{ID: s.ID, Name: s.CredentialName, CreatedAt: s.CreatedAt.UTC()})
	}
	c.JSON(http.StatusOK, gin.H{"credentials": views, "max": MaxAdminCredentials})
}

// CredentialStepUpBegin starts the add_credential step-up. POST {}.
func (h *Handler) CredentialStepUpBegin(c *gin.Context) {
	adminID := c.GetString(adminIDContextKey)
	h.beginAdminStepUp(c, actionAddCredential, func(ctx context.Context, result, reason string) {
		h.auditCredentialAdded(ctx, adminID, result, reason)
	})
}

// credentialRegisterBeginRequest carries the redeemed step-up.
type credentialRegisterBeginRequest struct {
	StepUp *adminStepUpProof `json:"stepup"`
}

// CredentialRegisterBegin redeems a fresh add_credential step-up and begins a
// registration ceremony for the signed-in admin, excluding every key the
// admin already holds. An admin at the cap gets 409 before any proof is spent.
func (h *Handler) CredentialRegisterBegin(c *gin.Context) {
	ctx := c.Request.Context()
	adminID := c.GetString(adminIDContextKey)
	adminSession := c.GetString(adminSessionContextKey)
	var req credentialRegisterBeginRequest
	if err := decodeStrict(c, &req, maxCredentialRegisterBody); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}
	existing, err := h.repo.ListCredentials(ctx, adminID)
	if err != nil {
		h.log.Error("admin credential register lookup failed", "admin_id", adminID, "error", err)
		h.auditCredentialAdded(ctx, adminID, AuditFailure, "credential_lookup_failed")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errServiceUnavailable})
		return
	}
	if len(existing) >= MaxAdminCredentials {
		h.auditCredentialAdded(ctx, adminID, AuditDenied, errCodeCredentialLimit)
		c.JSON(http.StatusConflict, gin.H{"error": errCodeCredentialLimit, "max": MaxAdminCredentials})
		return
	}
	if req.StepUp == nil {
		h.auditCredentialAdded(ctx, adminID, AuditDenied, "stepup_required")
		c.JSON(http.StatusForbidden, gin.H{"error": "stepup_required"})
		return
	}
	if refusal := h.redeemAdminStepUp(ctx, adminID, adminSession, actionAddCredential, req.StepUp.Handle, req.StepUp.Assertion); refusal != nil {
		if refusal.unavailable {
			h.auditCredentialAdded(ctx, adminID, AuditFailure, refusal.reason)
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": errServiceUnavailable})
			return
		}
		h.auditCredentialAdded(ctx, adminID, AuditDenied, refusal.reason)
		c.JSON(http.StatusForbidden, gin.H{"error": "stepup_failed"})
		return
	}
	generation, err := h.repo.ActiveCredentialGeneration(ctx, adminID)
	if err != nil {
		h.log.Error("admin credential register generation lookup failed", "admin_id", adminID, "error", err)
		h.auditCredentialAdded(ctx, adminID, AuditFailure, "generation_lookup_failed")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errServiceUnavailable})
		return
	}
	exclusions := make([]protocol.CredentialDescriptor, 0, len(existing))
	for _, s := range existing {
		exclusions = append(exclusions, protocol.CredentialDescriptor{Type: protocol.PublicKeyCredentialType, CredentialID: s.CredentialID})
	}
	opts := append(adminRegistrationOptions(), webauthn.WithExclusions(exclusions))
	creation, session, err := h.webauthn.BeginRegistration(adminRegistrationUser(), opts...)
	if err != nil {
		h.log.Error("admin credential register begin failed", "admin_id", adminID, "error", err)
		h.auditCredentialAdded(ctx, adminID, AuditFailure, "register_begin_failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal_error"})
		return
	}
	handle, err := h.storeCredentialRegistration(ctx, credentialRegistration{
		AdminID: adminID, AdminSession: adminSession, CredentialGeneration: generation, Session: *session,
	})
	if err != nil {
		h.log.Error("admin credential register store failed", "admin_id", adminID, "error", err)
		h.auditCredentialAdded(ctx, adminID, AuditFailure, "register_begin_failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal_error"})
		return
	}
	c.JSON(http.StatusOK, enrollBeginResponse{Handle: handle, PublicKey: creation})
}

// CredentialRegisterFinish redeems the registration handle once, from the
// session that began it, and stores the new key under the admin row lock.
func (h *Handler) CredentialRegisterFinish(c *gin.Context) {
	ctx := c.Request.Context()
	adminID := c.GetString(adminIDContextKey)
	adminSession := c.GetString(adminSessionContextKey)
	var req enrollFinishRequest
	if err := decodeStrict(c, &req, maxCredentialRegisterBody); err != nil || !validCredentialName(req.CredentialName) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}
	reg, refusal := h.consumeCredentialRegistration(ctx, adminID, adminSession, req.Handle)
	if refusal != nil {
		if refusal.unavailable {
			h.auditCredentialAdded(ctx, adminID, AuditFailure, refusal.reason)
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": errServiceUnavailable})
			return
		}
		h.auditCredentialAdded(ctx, adminID, AuditDenied, refusal.reason)
		c.JSON(http.StatusForbidden, gin.H{"error": "registration_failed"})
		return
	}
	verified, err := verifyAdminRegistration(h.webauthn, AdminRegistrationInput{
		User:           adminRegistrationUser(),
		Session:        reg.Session,
		Request:        httpRequestWithBody(c, req.Attestation),
		AdminID:        adminID,
		AllowedAAGUIDs: h.allowedAAGUIDs,
		CredentialName: req.CredentialName,
	})
	if err != nil {
		h.log.Warn("admin credential registration rejected", "admin_id", adminID, "error", err)
		h.auditCredentialAdded(ctx, adminID, AuditDenied, "attestation_rejected")
		c.JSON(http.StatusForbidden, gin.H{"error": "registration_failed"})
		return
	}
	stored, err := h.repo.AddAdditionalCredential(ctx, reg.CredentialGeneration, verified)
	switch {
	case err == nil:
	case errors.Is(err, ErrCredentialLimit):
		h.auditCredentialAdded(ctx, adminID, AuditDenied, errCodeCredentialLimit)
		c.JSON(http.StatusConflict, gin.H{"error": errCodeCredentialLimit, "max": MaxAdminCredentials})
		return
	case errors.Is(err, ErrDuplicateCredential):
		h.auditCredentialAdded(ctx, adminID, AuditDenied, "credential_already_registered")
		c.JSON(http.StatusConflict, gin.H{"error": "credential_already_registered"})
		return
	case errors.Is(err, ErrEnrollmentStateChanged), errors.Is(err, ErrAdminNotFound):
		h.auditCredentialAdded(ctx, adminID, AuditDenied, "credential_generation_changed")
		c.JSON(http.StatusForbidden, gin.H{"error": "registration_failed"})
		return
	default:
		h.log.Error("admin credential persist failed", "admin_id", adminID, "error", err)
		h.auditCredentialAdded(ctx, adminID, AuditFailure, "credential_persist_failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal_error"})
		return
	}
	h.auditCredentialAdded(ctx, adminID, AuditSuccess, "")
	c.JSON(http.StatusOK, gin.H{"status": "registered", "credential": adminCredentialView{
		ID: stored.ID, Name: stored.CredentialName, CreatedAt: stored.CreatedAt.UTC(),
	}})
}

// validCredentialName bounds the key label to what the Host Agent import
// accepts (at most 128 bytes of UTF-8 with no control characters), so a key
// added here can always be exported and imported.
func validCredentialName(name string) bool {
	return len(name) <= maxCredentialNameBytes && utf8.ValidString(name) && strings.IndexFunc(name, unicode.IsControl) < 0
}

const maxCredentialNameBytes = 128

func (h *Handler) storeCredentialRegistration(ctx context.Context, reg credentialRegistration) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate credential registration handle: %w", err)
	}
	handle := hex.EncodeToString(raw)
	payload, err := json.Marshal(reg)
	if err != nil {
		return "", fmt.Errorf("marshal credential registration: %w", err)
	}
	if err := h.redis.Set(ctx, credentialRegisterKey(reg.AdminID, handle), payload, enrollChallengeTTL).Err(); err != nil {
		return "", fmt.Errorf("store credential registration: %w", err)
	}
	return handle, nil
}

// consumeCredentialRegistration redeems a handle once (GETDEL). Like a
// step-up, an attempt from another session of the same admin spends it.
func (h *Handler) consumeCredentialRegistration(ctx context.Context, adminID, adminSession, handle string) (credentialRegistration, *stepUpRefusal) {
	if adminID == "" || !adminStepUpHandle.MatchString(handle) {
		return credentialRegistration{}, stepUpDenied("registration_malformed")
	}
	raw, err := h.redis.GetDel(ctx, credentialRegisterKey(adminID, handle)).Result()
	switch {
	case errors.Is(err, redis.Nil):
		return credentialRegistration{}, stepUpDenied("registration_unknown_handle")
	case err != nil:
		h.log.Error("admin credential registration load failed", "admin_id", adminID, "error", err)
		return credentialRegistration{}, stepUpUnavailable("registration_store_unavailable")
	}
	var reg credentialRegistration
	if err := json.Unmarshal([]byte(raw), &reg); err != nil {
		h.log.Error("admin credential registration record unreadable", "admin_id", adminID, "error", err)
		return credentialRegistration{}, stepUpUnavailable("registration_record_unreadable")
	}
	if reg.AdminID != adminID {
		return credentialRegistration{}, stepUpDenied("registration_wrong_admin")
	}
	if reg.AdminSession == "" || adminSession == "" || subtle.ConstantTimeCompare([]byte(reg.AdminSession), []byte(adminSession)) != 1 {
		h.log.Warn("admin credential registration used by another session", "admin_id", adminID)
		return credentialRegistration{}, stepUpDenied("registration_wrong_session")
	}
	return reg, nil
}

// auditCredentialAdded records one credential_added row against the admin,
// best-effort like every other admin audit row.
func (h *Handler) auditCredentialAdded(ctx context.Context, adminID, result, reason string) {
	acting := adminID
	ev := AuditEvent{AdminID: &acting, EventType: EventCredentialAdded, Result: result}
	if reason != "" {
		ev.Detail = map[string]any{"reason": reason}
	}
	if err := h.audit.Write(ctx, ev); err != nil {
		h.log.Error("admin credential audit row not written", "admin_id", adminID, "result", result, "reason", reason, "error", err)
	}
}
