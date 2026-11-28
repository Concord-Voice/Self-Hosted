package hostagent

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/mfa"
)

const (
	authorizationCapacity = 64
	// One admin cannot hold the shared ceremony store against the others.
	authorizationsPerAdmin = 4
	authorizationTTL       = 2 * time.Minute
	maxAssertionBytes      = 32 << 10
	// Admin enrollment registers this stable RP user handle; adminID stays in the pending ceremony.
	adminWebAuthnUserHandle = "admin-user-handle"
)

var (
	// ErrAuthorizationDenied means the ceremony did not authorize this action.
	ErrAuthorizationDenied = errors.New("authorization denied")
	// ErrAuthorizationReplay means the one-use handle no longer exists.
	ErrAuthorizationReplay = errors.New("authorization handle unavailable")
	// ErrAuthorizationBusy means the bounded ceremony store is full.
	ErrAuthorizationBusy = errors.New("authorization capacity reached")
	// ErrAuthorizerUnavailable means trusted verification could not complete.
	ErrAuthorizerUnavailable = errors.New("authorizer unavailable")

	// errCloneDetected and errCounterStale are denials the audit names apart.
	errCloneDetected = fmt.Errorf("%w: authenticator clone warning", ErrAuthorizationDenied)
	errCounterStale  = fmt.Errorf("%w: %w", ErrAuthorizationDenied, errAuthorizerCounterStale)
	// errNoActionCredential means root has imported no active credential for
	// this admin, so no root action can be authorized.
	errNoActionCredential = fmt.Errorf("%w: no action credential", ErrAuthorizationDenied)
)

type agentWebAuthn interface {
	BeginLogin(*mfa.WebAuthnUser, ...webauthn.LoginOption) (*protocol.CredentialAssertion, *webauthn.SessionData, error)
	FinishLoginWithBytes(*mfa.WebAuthnUser, webauthn.SessionData, []byte) (*webauthn.Credential, error)
}

type pendingAuthorization struct {
	adminID string
	action  string
	req     actionRequest
	session webauthn.SessionData
	expires time.Time
}

// AuthorizationStore keeps only bounded, one-use agent-local ceremonies.
type AuthorizationStore struct {
	mu          sync.Mutex
	authorizers *AuthorizerStore
	webauthn    agentWebAuthn
	now         func() time.Time
	jobs        *jobs
	pending     map[string]pendingAuthorization
}

// NewAuthorizationStore creates the agent-local bounded ceremony store.
func NewAuthorizationStore(authorizers *AuthorizerStore, verifier agentWebAuthn, now func() time.Time) *AuthorizationStore {
	if now == nil {
		now = time.Now
	}
	return &AuthorizationStore{authorizers: authorizers, webauthn: verifier, now: now, pending: make(map[string]pendingAuthorization)}
}

// Begin validates the same closed action parser used by job creation before
// allocating a WebAuthn challenge or an in-memory slot. A request the parser
// refuses returns that *refusal, so its own status and reason reach the caller;
// ErrAuthorizationDenied (as errNoActionCredential) is kept for admins with no
// active imported credential. Every imported credential of the admin is in the
// allow-list: the same key logs in and authorizes actions.
func (s *AuthorizationStore) Begin(adminID, action string, req actionRequest) (string, *protocol.CredentialAssertion, error) {
	if s == nil || s.authorizers == nil || s.webauthn == nil || s.jobs == nil {
		return "", nil, ErrAuthorizerUnavailable
	}
	parsed, err := uuid.Parse(adminID)
	if err != nil || parsed.String() != adminID || actionName(action) == "" || len(req.Tag) > 128 || len(req.Archive) > 128 {
		return "", nil, &refusal{http.StatusBadRequest, "invalid_authorization"}
	}
	if _, _, no := s.jobs.commands(action, req); no != nil {
		return "", nil, no
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if err := s.checkCapacityLocked(adminID, now); err != nil {
		return "", nil, err
	}
	credentials, err := s.authorizers.Credentials(adminID)
	if err != nil {
		log.Printf("concord-agent: authorizer trust unreadable at begin: %v", err)
		return "", nil, ErrAuthorizerUnavailable
	}
	if len(credentials) == 0 {
		return "", nil, errNoActionCredential
	}
	user := &mfa.WebAuthnUser{ID: []byte(adminWebAuthnUserHandle), Name: adminID, DisplayName: adminID, Credentials: credentials}
	options, session, err := s.webauthn.BeginLogin(user, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil || session == nil || options == nil {
		return "", nil, ErrAuthorizerUnavailable
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", nil, ErrAuthorizerUnavailable
	}
	handle := base64.RawURLEncoding.EncodeToString(secret)
	if _, exists := s.pending[handle]; exists {
		return "", nil, ErrAuthorizerUnavailable
	}
	s.pending[handle] = pendingAuthorization{adminID: adminID, action: action, req: req, session: *session, expires: now.Add(authorizationTTL)}
	return handle, options, nil
}

// checkCapacityLocked drops expired ceremonies, then refuses a new one when the
// store or this admin's share of it is full. The caller holds s.mu.
func (s *AuthorizationStore) checkCapacityLocked(adminID string, now time.Time) error {
	held := 0
	for handle, pending := range s.pending {
		switch {
		case !now.Before(pending.expires):
			delete(s.pending, handle)
		case pending.adminID == adminID:
			held++
		}
	}
	if len(s.pending) >= authorizationCapacity || held >= authorizationsPerAdmin {
		return ErrAuthorizationBusy
	}
	return nil
}

// consume removes the handle before checking the assertion. Failed, expired,
// wrong-action and replayed proofs therefore never regain authority.
func (s *AuthorizationStore) consume(handle, action string, assertion []byte) (actionRequest, string, error) {
	if s == nil || s.authorizers == nil || s.webauthn == nil {
		return actionRequest{}, "", ErrAuthorizerUnavailable
	}
	s.mu.Lock()
	pending, exists := s.pending[handle]
	delete(s.pending, handle)
	s.mu.Unlock()
	if !exists {
		return actionRequest{}, "", ErrAuthorizationReplay
	}
	if !s.now().Before(pending.expires) || pending.action != action || len(assertion) == 0 || len(assertion) > maxAssertionBytes {
		return actionRequest{}, pending.adminID, ErrAuthorizationDenied
	}
	credentials, err := s.authorizers.Credentials(pending.adminID)
	if err != nil {
		log.Printf("concord-agent: authorizer trust unreadable at consume: %v", err)
		return actionRequest{}, pending.adminID, ErrAuthorizerUnavailable
	}
	if len(credentials) == 0 {
		return actionRequest{}, pending.adminID, errNoActionCredential
	}
	user := &mfa.WebAuthnUser{ID: []byte(adminWebAuthnUserHandle), Name: pending.adminID, DisplayName: pending.adminID, Credentials: credentials}
	credential, err := s.webauthn.FinishLoginWithBytes(user, pending.session, assertion)
	if err != nil || credential == nil {
		return actionRequest{}, pending.adminID, ErrAuthorizationDenied
	}
	if credential.Authenticator.CloneWarning {
		return actionRequest{}, pending.adminID, errCloneDetected
	}
	// The signing credential's ID, re-checked as active under the file lock,
	// not the ID the client claimed. This is the same root counter a console
	// login advances, so a login and an action can never replay each other.
	if err := s.authorizers.AdvanceCounter(credential.ID, credential.Authenticator.SignCount); err != nil {
		switch {
		case errors.Is(err, errAuthorizerCounterStale):
			return actionRequest{}, pending.adminID, errCounterStale
		case errors.Is(err, errAuthorizerRevoked):
			// Revoked between the credential read and the counter write.
			return actionRequest{}, pending.adminID, ErrAuthorizationDenied
		}
		log.Printf("concord-agent: authorizer counter update failed: %v", err)
		return actionRequest{}, pending.adminID, ErrAuthorizerUnavailable
	}
	return pending.req, pending.adminID, nil
}
