package privacy_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/activepresence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/privacy"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
)

// stubAccountDeleter implements users.AccountDeleter for unit tests.
// Configurable to return any error or nil. Records the userID it was
// called with so tests can assert the handler passed the right value.
type stubAccountDeleter struct {
	err             error
	called          bool
	calledWithUser  string
	emailUnverified bool
	emailPrepared   users.EmailErasureConfirmation
}

func (s *stubAccountDeleter) PrepareAccountErasure(
	_ context.Context, _, _, _ string,
) (*stepup.PasswordAttempt, *stepup.Error) {
	return &stepup.PasswordAttempt{}, nil
}

func (s *stubAccountDeleter) EmailVerified(context.Context, string) (bool, error) {
	return !s.emailUnverified, nil
}

func (s *stubAccountDeleter) EmailErasureConfirmationBinding(
	context.Context, users.AccountErasureRequest,
) (users.EmailErasureConfirmation, error) {
	return s.emailPrepared, nil
}

func (s *stubAccountDeleter) PrepareEmailErasureConfirmation(
	context.Context, users.AccountErasureRequest,
) (users.EmailErasureConfirmation, error) {
	return s.emailPrepared, nil
}

type recordingErasureEmailSender struct{ to, url string }

func (s *recordingErasureEmailSender) SendAccountErasureConfirmation(to, confirmationURL string) error {
	s.to, s.url = to, confirmationURL
	return nil
}

func (s *stubAccountDeleter) DeleteAccount(_ context.Context, request users.AccountErasureRequest) error {
	s.called = true
	s.calledWithUser = request.Actor.UserID.String()
	return s.err
}

// newTestContext returns a Gin context wrapping a recorded response, with
// the given JSON body and "user_id" set in context (matching auth middleware).
func newTestContext(t *testing.T, body string, userID string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	if body == "" || body == `{}` {
		body = `{"password":"test-password","confirmed_owned_server_ids":[]}`
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/privacy/erase-account", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	if userID != "" {
		actorID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(userID))
		c.Set("user_id", actorID.String())
		identity := loginfamily.AccessIdentity{UserID: actorID, SID: uuid.New(), CredentialEpoch: "test"}
		c.Request = c.Request.WithContext(loginfamily.WithAccessIdentity(c.Request.Context(), identity))
	}
	return c, w
}

// TestNewHandler_PanicsOnNilAccount asserts the constructor's documented
// fail-fast behavior. Mis-wiring a nil dependency must surface at startup,
// not as an opaque nil-pointer dereference at first request.
func TestNewHandler_PanicsOnNilAccount(t *testing.T) {
	require.Panics(t, func() {
		privacy.NewHandler(nil, nil, logger.New("test"))
	})
}

// TestNewHandler_ToleratesNilLogger pins the documented test-only tolerance
// for a nil logger. The log field is only dereferenced behind a nil-check
// on the error path, so construction must not panic.
func TestNewHandler_ToleratesNilLogger(t *testing.T) {
	stub := &stubAccountDeleter{}
	require.NotPanics(t, func() {
		_ = privacy.NewHandler(stub, nil, nil)
	})
}

func TestEraseAccount_Returns401WhenUserIDMissing(t *testing.T) {
	stub := &stubAccountDeleter{}
	h := privacy.NewHandler(stub, nil, logger.New("test"))

	c, _ := newTestContext(t, `{}`, "")
	h.EraseAccount(c)

	assert.Equal(t, http.StatusUnauthorized, c.Writer.Status())
	assert.False(t, stub.called, "DeleteAccount must not run when user_id is missing")
}

func TestEraseAccount_Returns400OnInvalidJSON(t *testing.T) {
	stub := &stubAccountDeleter{}
	h := privacy.NewHandler(stub, nil, logger.New("test"))

	c, _ := newTestContext(t, `{not-json`, "test-user-uuid")
	h.EraseAccount(c)

	assert.Equal(t, http.StatusBadRequest, c.Writer.Status())
	assert.False(t, stub.called, "DeleteAccount must not run on invalid body")
}

func TestEraseAccount_Returns204OnEmptyBody(t *testing.T) {
	stub := &stubAccountDeleter{}
	h := privacy.NewHandler(stub, nil, logger.New("test"))

	c, _ := newTestContext(t, ``, "test-user-uuid")
	h.EraseAccount(c)

	assert.Equal(t, http.StatusNoContent, c.Writer.Status())
	assert.True(t, stub.called)
	assert.Equal(t, c.GetString("user_id"), stub.calledWithUser)
}

func TestEraseAccount_Returns204OnEmptyJSONBody(t *testing.T) {
	stub := &stubAccountDeleter{}
	h := privacy.NewHandler(stub, nil, logger.New("test"))

	c, _ := newTestContext(t, `{}`, "test-user-uuid")
	h.EraseAccount(c)

	assert.Equal(t, http.StatusNoContent, c.Writer.Status())
	assert.True(t, stub.called)
}

func TestEraseAccount_RevokesAccessTokensFromClaims(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	stub := &stubAccountDeleter{}
	h := privacy.NewHandler(stub, rdb, logger.New("test"))

	const jti = "erase-account-current-token"
	c, _ := newTestContext(t, `{}`, "test-user-uuid")
	c.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{
		"jti": jti,
		"exp": float64(time.Now().Add(15 * time.Minute).Unix()),
	})

	h.EraseAccount(c)

	assert.Equal(t, http.StatusNoContent, c.Writer.Status())
	assert.True(t, stub.called)

	ctx := context.Background()
	val, err := rdb.Get(ctx, "blacklist:"+jti).Result()
	require.NoError(t, err)
	assert.Equal(t, "1", val)

	ttl, err := rdb.TTL(ctx, "blacklist:"+jti).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, time.Duration(0))

	disabledVal, err := rdb.Get(ctx, middleware.UserDisabledKey(c.GetString("user_id"))).Result()
	require.NoError(t, err)
	assert.Equal(t, "1", disabledVal)

	disabledTTL, err := rdb.TTL(ctx, middleware.UserDisabledKey(c.GetString("user_id"))).Result()
	require.NoError(t, err)
	assert.Greater(t, disabledTTL, time.Duration(0))
	assert.LessOrEqual(t, disabledTTL, auth.AccessTokenTTL)
}

func TestEraseAccount_FailsClosedOnMalformedAccessTokenClaims(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name   string
		claims any
	}{
		{
			name:   "unexpected type",
			claims: "not-jwt-map-claims",
		},
		{
			name: "missing jti",
			claims: jwt.MapClaims{
				"exp": float64(now.Add(15 * time.Minute).Unix()),
			},
		},
		{
			name: "invalid expiration",
			claims: jwt.MapClaims{
				"jti": "invalid-expiration",
				"exp": "soon",
			},
		},
		{
			name: "missing expiration",
			claims: jwt.MapClaims{
				"jti": "missing-expiration",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rdb, cleanup := testhelpers.SetupTestRedis(t)
			defer cleanup()

			stub := &stubAccountDeleter{}
			h := privacy.NewHandler(stub, rdb, logger.New("test"))
			c, _ := newTestContext(t, `{}`, "test-user-uuid")
			c.Set(middleware.JWTClaimsContextKey, tt.claims)

			h.EraseAccount(c)

			assert.Equal(t, http.StatusInternalServerError, c.Writer.Status())
			assert.True(t, stub.called, "claims are validated after account deletion succeeds")
		})
	}
}

func TestEraseAccount_Returns404OnUserNotFound(t *testing.T) {
	stub := &stubAccountDeleter{err: users.ErrUserNotFound}
	h := privacy.NewHandler(stub, nil, logger.New("test"))

	c, _ := newTestContext(t, `{}`, "missing-user-uuid")
	h.EraseAccount(c)

	assert.Equal(t, http.StatusNotFound, c.Writer.Status())
}

func TestEraseAccount_Returns500OnUnknownError(t *testing.T) {
	stub := &stubAccountDeleter{err: errors.New("db unavailable")}
	h := privacy.NewHandler(stub, nil, logger.New("test"))

	c, _ := newTestContext(t, `{}`, "test-user-uuid")
	h.EraseAccount(c)

	assert.Equal(t, http.StatusInternalServerError, c.Writer.Status())
}

// TestEraseAccount_IgnoresUnknownClientIdField is the regression guard for
// the #758 transition window: the desktop client (#757 not yet shipped) may
// continue to POST {"clientId":"..."} bodies after the server-side handler
// is Sentry-free. Gin's ShouldBindJSON does not reject unknown fields by
// default, so the handler must accept the extra field and proceed to the
// account-deletion path.
func TestEraseAccount_IgnoresUnknownClientIdField(t *testing.T) {
	const sentinelClientID = "deadbeefcafef00d1122334455667788" //nolint:gosec // pragma: allowlist secret — test sentinel
	stub := &stubAccountDeleter{}
	h := privacy.NewHandler(stub, nil, logger.New("test"))

	body := `{"clientId":"` + sentinelClientID + `","password":"test-password","confirmed_owned_server_ids":[]}`
	c, _ := newTestContext(t, body, "test-user-uuid")
	h.EraseAccount(c)

	assert.Equal(t, http.StatusNoContent, c.Writer.Status(),
		"unknown clientId field must be silently ignored — POST returns 204")
	assert.True(t, stub.called,
		"DeleteAccount must still be invoked when the body has an extra field")
}

func TestEraseAccount_UnverifiedEmailIssuesIntentWithoutDeletion(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()
	familyID := uuid.NewString()
	stub := &stubAccountDeleter{emailUnverified: true, emailPrepared: users.EmailErasureConfirmation{
		Recipient: "unverified@example.test", FamilyID: familyID, CredentialEpoch: "test", OwnedServerIDs: []string{},
	}}
	h := privacy.NewHandler(stub, rdb, logger.New("test"))
	sender := &recordingErasureEmailSender{}
	h.SetErasureEmailSender(sender)
	c, _ := newTestContext(t, `{"password":"test-password","confirmed_owned_server_ids":[]}`, "unverified-user")

	h.EraseAccount(c)

	require.Equal(t, http.StatusAccepted, c.Writer.Status())
	assert.False(t, stub.called, "first unverified-email proof must only issue intent")
	assert.Equal(t, "unverified@example.test", sender.to)
	assert.Regexp(t, `^concord://account-erasure/confirm/[0-9a-f-]{36}/[A-Za-z0-9_-]{43}$`, sender.url)
}

func TestEraseAccount_UnverifiedEmailConfirmationIsOneUseAndReleasesOnFailure(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	const recipient = "unverified@example.test"
	familyID := uuid.NewString()
	stub := &stubAccountDeleter{emailUnverified: true, emailPrepared: users.EmailErasureConfirmation{
		Recipient: recipient, FamilyID: familyID, CredentialEpoch: "test", OwnedServerIDs: []string{},
	}}
	h := privacy.NewHandler(stub, rdb, logger.New("test"))
	store := privacy.NewEmailConfirmationStore(rdb)

	issue := func(t *testing.T) (string, string) {
		t.Helper()
		intent, err := store.Issue(context.Background(), privacy.EmailConfirmationBinding{
			Recipient: recipient, UserID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("unverified-user")).String(),
			FamilyID: familyID, CredentialEpoch: "test", OwnedServerIDs: []string{},
		})
		require.NoError(t, err)
		return intent.RequestID, intent.Token
	}

	requestID, token := issue(t)
	c, _ := newTestContext(t, fmt.Sprintf(`{"password":"test-password","confirmed_owned_server_ids":[],"email_confirmation":{"request_id":"%s","token":"%s"}}`, requestID, token), "unverified-user")
	h.EraseAccount(c)
	require.Equal(t, http.StatusNoContent, c.Writer.Status())
	require.True(t, stub.called)

	_, err := store.Claim(context.Background(), requestID, token, privacy.EmailConfirmationBinding{
		Recipient: recipient, UserID: c.GetString("user_id"), FamilyID: familyID, CredentialEpoch: "test", OwnedServerIDs: []string{},
	})
	require.ErrorIs(t, err, privacy.ErrEmailConfirmationStale, "a committed deletion must consume its email confirmation")

	stub.called = false
	stub.err = errors.New("delete failed before commit")
	requestID, token = issue(t)
	c, _ = newTestContext(t, fmt.Sprintf(`{"password":"test-password","confirmed_owned_server_ids":[],"email_confirmation":{"request_id":"%s","token":"%s"}}`, requestID, token), "unverified-user")
	h.EraseAccount(c)
	require.Equal(t, http.StatusInternalServerError, c.Writer.Status())
	require.True(t, stub.called)

	claim, err := store.Claim(context.Background(), requestID, token, privacy.EmailConfirmationBinding{
		Recipient: recipient, UserID: c.GetString("user_id"), FamilyID: familyID, CredentialEpoch: "test", OwnedServerIDs: []string{},
	})
	require.NoError(t, err, "a pre-commit deletion failure must release the confirmation for retry")
	require.NoError(t, claim.Release(context.Background()))
}

// A DURABLE erasure — the account IS gone, only presence delivery failed — must
// NOT be treated as a failed deletion.
//
// Before this was handled, the endpoint answered 500 for an account that no
// longer existed (so a client retry then got 404), and, far worse, it returned
// before revokeAccessTokens — leaving the jti blacklist and the user_disabled
// key unwritten for an erased account. backend.md requires both before
// returning success (code review, PR #2840).
func TestEraseAccount_DurableDeliveryFailureStillRevokesTokens(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rdb := redistest.Client(t)
	userID := "test-user-uuid"
	jti := "durable-delivery-jti"

	stub := &stubAccountDeleter{
		err: fmt.Errorf("delete account: presence delivery: %w",
			presencecapture.ErrPostCommitDelivery),
	}
	h := privacy.NewHandler(stub, rdb, logger.New("test"))

	c, _ := newTestContext(t, `{}`, userID)
	c.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{
		"jti": jti,
		"exp": float64(time.Now().Add(10 * time.Minute).Unix()),
	})

	h.EraseAccount(c)

	require.True(t, stub.called)
	assert.Equal(t, http.StatusServiceUnavailable, c.Writer.Status(),
		"a durable erasure is 503, never the 500 that says nothing happened")

	// The point of the fix, and what the name claims: returning early on the
	// durable arm skipped revokeAccessTokens entirely, leaving an already-issued
	// access token working against an account that is gone. Asserting only the
	// status would not have caught that (CodeRabbit, PR #2840).
	ctx := context.Background()
	blacklisted, err := rdb.Exists(ctx, "blacklist:"+jti).Result()
	require.NoError(t, err)
	assert.EqualValues(t, 1, blacklisted, "the access token's jti must be blacklisted")

	disabled, err := rdb.Exists(ctx, middleware.UserDisabledKey(c.GetString("user_id"))).Result()
	require.NoError(t, err)
	assert.EqualValues(t, 1, disabled, "the user_disabled key must be set")

	ttl, err := rdb.TTL(ctx, middleware.UserDisabledKey(c.GetString("user_id"))).Result()
	require.NoError(t, err)
	assert.Positive(t, ttl, "the disabled marker must carry a TTL, not linger forever")
}

func TestEraseAccount_CandidateSetDriftReturnsConflictWithoutRevocation(t *testing.T) {
	rdb := redistest.Client(t)
	userID, jti := "drift-user-uuid", "drift-current-token"
	stub := &stubAccountDeleter{err: users.ErrErasureCandidateSetDrifted}
	h := privacy.NewHandler(stub, rdb, logger.New("test"))
	c, w := newTestContext(t, `{}`, userID)
	c.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{
		"jti": jti,
		"exp": float64(time.Now().Add(10 * time.Minute).Unix()),
	})

	h.EraseAccount(c)

	require.True(t, stub.called)
	assert.Equal(t, http.StatusConflict, c.Writer.Status())
	assert.JSONEq(t, `{"error":"account state changed; retry the deletion"}`, w.Body.String())
	ctx := context.Background()
	blacklisted, err := rdb.Exists(ctx, "blacklist:"+jti).Result()
	require.NoError(t, err)
	assert.Zero(t, blacklisted, "conflict must not revoke the access token")
	disabled, err := rdb.Exists(ctx, middleware.UserDisabledKey(userID)).Result()
	require.NoError(t, err)
	assert.Zero(t, disabled, "conflict must not disable the account")
}

func TestEraseAccount_JoinedActivePlanDeliveryFailureStillRevokesTokens(t *testing.T) {
	rdb := redistest.Client(t)
	userID, jti := "active-plan-user-uuid", "active-plan-current-token"
	stub := &stubAccountDeleter{err: errors.Join(
		presencecapture.ErrPostCommitDelivery,
		activepresence.ErrDeliveryIncomplete,
	)}
	h := privacy.NewHandler(stub, rdb, logger.New("test"))
	c, _ := newTestContext(t, `{}`, userID)
	c.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{
		"jti": jti,
		"exp": float64(time.Now().Add(10 * time.Minute).Unix()),
	})
	h.EraseAccount(c)

	assert.Equal(t, http.StatusServiceUnavailable, c.Writer.Status())
	blacklisted, err := rdb.Exists(context.Background(), "blacklist:"+jti).Result()
	require.NoError(t, err)
	assert.EqualValues(t, 1, blacklisted)
}

// A genuine pre-commit failure must still be a 500 — the durable arm must not
// swallow real failures.
func TestEraseAccount_PreCommitFailureStill500(t *testing.T) {
	stub := &stubAccountDeleter{err: errors.New("begin tx: connection refused")}
	h := privacy.NewHandler(stub, nil, logger.New("test"))

	c, _ := newTestContext(t, `{}`, "test-user-uuid")
	h.EraseAccount(c)

	assert.Equal(t, http.StatusInternalServerError, c.Writer.Status())
}
