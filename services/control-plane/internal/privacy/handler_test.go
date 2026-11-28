package privacy_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/redistest"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/activepresence"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/privacy"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/users"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
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
	// Per-method failures; nil means success.
	prepareErr      *stepup.Error
	verifiedErr     error
	bindingErr      error
	prepareEmailErr error
}

func (s *stubAccountDeleter) PrepareAccountErasure(
	_ context.Context, _, _, _ string,
) (*stepup.PasswordAttempt, stepup.Charged, *stepup.Error) {
	if s.prepareErr != nil {
		return nil, stepup.Charged{}, s.prepareErr
	}
	return &stepup.PasswordAttempt{}, stepup.Charged{}, nil
}

func (s *stubAccountDeleter) EmailVerified(context.Context, string) (bool, error) {
	return !s.emailUnverified, s.verifiedErr
}

func (s *stubAccountDeleter) EmailErasureConfirmationBinding(
	context.Context, users.AccountErasureRequest,
) (users.EmailErasureConfirmation, error) {
	return s.emailPrepared, s.bindingErr
}

func (s *stubAccountDeleter) PrepareEmailErasureConfirmation(
	context.Context, users.AccountErasureRequest,
) (users.EmailErasureConfirmation, error) {
	return s.emailPrepared, s.prepareEmailErr
}

type recordingErasureEmailSender struct {
	to, url string
	err     error
}

func (s *recordingErasureEmailSender) SendAccountErasureConfirmation(to, confirmationURL string) error {
	s.to, s.url = to, confirmationURL
	return s.err
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

func TestOwnedServerPreviewFailsClosedWithoutReader(t *testing.T) {
	h := privacy.NewHandler(&stubAccountDeleter{}, nil, logger.New("test"))
	c, w := newTestContext(t, "", "preview-user")
	c.Request.Method = http.MethodGet
	h.ListOwnedServers(c)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.JSONEq(t, `{"error":"owned_server_list_unavailable"}`, w.Body.String())
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

// ── Request validation and refusal mapping ────────────────────────────────

func TestEraseAccount_RejectsMalformedRequests(t *testing.T) {
	ordered := []string{"00000000-0000-0000-0000-00000000000a", "00000000-0000-0000-0000-00000000000b"}
	cases := map[string]string{
		"trailing document":       `{"password":"p","confirmed_owned_server_ids":[]}{}`,
		"unknown field":           `{"password":"p","confirmed_owned_server_ids":[],"extra":1}`,
		"proof without the array": `{"password":"p"}`,
		"code without the array":  `{"mfa_code":"123456"}`,
		"confirmation alone":      `{"email_confirmation":{"request_id":"r","token":"t"}}`,
		"not a uuid":              `{"password":"p","confirmed_owned_server_ids":["not-a-uuid"]}`,
		"nil uuid":                `{"password":"p","confirmed_owned_server_ids":["00000000-0000-0000-0000-000000000000"]}`,
		"non-canonical uuid":      `{"password":"p","confirmed_owned_server_ids":["00000000-0000-0000-0000-00000000000A"]}`,
		"out of order":            fmt.Sprintf(`{"password":"p","confirmed_owned_server_ids":["%s","%s"]}`, ordered[1], ordered[0]),
		"duplicate":               fmt.Sprintf(`{"password":"p","confirmed_owned_server_ids":["%s","%s"]}`, ordered[0], ordered[0]),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			stub := &stubAccountDeleter{}
			h := privacy.NewHandler(stub, nil, logger.New("test"))
			c, w := newTestContext(t, body, "malformed-user")
			h.EraseAccount(c)
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.JSONEq(t, `{"error":"invalid_request"}`, w.Body.String())
			assert.False(t, stub.called)
		})
	}
}

func TestEraseAccount_AcceptsOrderedConfirmedServers(t *testing.T) {
	stub := &stubAccountDeleter{}
	h := privacy.NewHandler(stub, nil, logger.New("test"))
	c, w := newTestContext(t, `{"password":"p","confirmed_owned_server_ids":["00000000-0000-0000-0000-00000000000a","00000000-0000-0000-0000-00000000000b"]}`, "ordered-user")
	h.EraseAccount(c)
	assert.Equal(t, http.StatusNoContent, c.Writer.Status(), w.Body.String())
	assert.True(t, stub.called)
}

func TestEraseAccount_RefusesIdentityForAnotherUser(t *testing.T) {
	stub := &stubAccountDeleter{}
	h := privacy.NewHandler(stub, nil, logger.New("test"))
	c, w := newTestContext(t, "", "identity-user")
	c.Set("user_id", uuid.NewString())
	h.EraseAccount(c)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.JSONEq(t, `{"error":"authentication required"}`, w.Body.String())
	assert.False(t, stub.called)
}

func TestEraseAccount_WritesPreparationRefusal(t *testing.T) {
	stub := &stubAccountDeleter{prepareErr: &stepup.Error{Status: http.StatusTooManyRequests, Body: gin.H{"error": "Too many verification attempts"}}}
	h := privacy.NewHandler(stub, nil, logger.New("test"))
	c, w := newTestContext(t, "", "budget-user")
	h.EraseAccount(c)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.JSONEq(t, `{"error":"Too many verification attempts"}`, w.Body.String())
	assert.False(t, stub.called)
}

func TestEraseAccount_EmailStatusUnavailableIs503(t *testing.T) {
	stub := &stubAccountDeleter{verifiedErr: errors.New("db down")}
	h := privacy.NewHandler(stub, nil, logger.New("test"))
	c, w := newTestContext(t, "", "status-user")
	h.EraseAccount(c)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.JSONEq(t, `{"error":"email confirmation status unavailable"}`, w.Body.String())
	assert.False(t, stub.called)
}

func TestEraseAccount_VerifiedAccountReportsStaleEmailConfirmation(t *testing.T) {
	stub := &stubAccountDeleter{}
	h := privacy.NewHandler(stub, nil, logger.New("test"))
	c, w := newTestContext(t, `{"password":"p","confirmed_owned_server_ids":[],"email_confirmation":{"request_id":"r","token":"t"}}`, "verified-user")
	h.EraseAccount(c)
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.JSONEq(t, `{"error_code":"email_confirmation_stale"}`, w.Body.String())
	assert.False(t, stub.called)
}

// erasurePreparationRefusals is every preparation error class and the
// response it must produce, shared by the issue and claim paths.
var erasurePreparationRefusals = []struct {
	name       string
	err        error
	wantStatus int
	wantBody   string
}{
	{"step-up refusal", &stepup.Error{Status: http.StatusForbidden, Body: gin.H{"error": stepup.ErrMsgInvalidPassword}}, http.StatusForbidden, `{"error":"` + stepup.ErrMsgInvalidPassword + `"}`},
	{"owned set drift", &users.ErrErasureOwnedServerSetDrifted{RequiredOwnedServerIDs: []string{"00000000-0000-0000-0000-00000000000a"}}, http.StatusConflict,
		`{"error_code":"owned_server_confirmation_stale","required_owned_server_ids":["00000000-0000-0000-0000-00000000000a"]}`},
	{"already verified", users.ErrErasureEmailAlreadyVerified, http.StatusConflict, `{"error_code":"email_confirmation_stale"}`},
	{"epoch mismatch", fmt.Errorf("wrap: %w", credepoch.ErrEpochMismatch), http.StatusUnauthorized, `{"error":"authentication required"}`},
	{"unknown", errors.New("db down"), http.StatusServiceUnavailable, `{"error":"email confirmation unavailable"}`},
}

func TestEraseAccount_IssueMapsPreparationErrors(t *testing.T) {
	for _, tc := range erasurePreparationRefusals {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubAccountDeleter{emailUnverified: true, prepareEmailErr: tc.err}
			h := privacy.NewHandler(stub, redistest.Client(t), logger.New("test"))
			sender := &recordingErasureEmailSender{}
			h.SetErasureEmailSender(sender)
			c, w := newTestContext(t, "", "issue-refusal-user")
			h.EraseAccount(c)
			assert.Equal(t, tc.wantStatus, w.Code)
			assert.JSONEq(t, tc.wantBody, w.Body.String())
			assert.Empty(t, sender.to, "a refused preparation sends no email")
			assert.False(t, stub.called)
		})
	}
}

func TestEraseAccount_ClaimMapsPreparationErrors(t *testing.T) {
	for _, tc := range erasurePreparationRefusals {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubAccountDeleter{emailUnverified: true, bindingErr: tc.err}
			h := privacy.NewHandler(stub, redistest.Client(t), logger.New("test"))
			c, w := newTestContext(t, `{"password":"p","confirmed_owned_server_ids":[],"email_confirmation":{"request_id":"r","token":"t"}}`, "claim-refusal-user")
			h.EraseAccount(c)
			assert.Equal(t, tc.wantStatus, w.Code)
			assert.JSONEq(t, tc.wantBody, w.Body.String())
			assert.False(t, stub.called)
		})
	}
}

func TestEraseAccount_EmailConfirmationDeliveryFailures(t *testing.T) {
	prepared := users.EmailErasureConfirmation{
		Recipient: "unverified@example.test", FamilyID: uuid.NewString(), CredentialEpoch: "test", OwnedServerIDs: []string{},
	}
	t.Run("no sender wired", func(t *testing.T) {
		stub := &stubAccountDeleter{emailUnverified: true, emailPrepared: prepared}
		h := privacy.NewHandler(stub, redistest.Client(t), logger.New("test"))
		c, w := newTestContext(t, "", "no-sender-user")
		h.EraseAccount(c)
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.JSONEq(t, `{"error":"email confirmation unavailable"}`, w.Body.String())
	})
	t.Run("send fails", func(t *testing.T) {
		stub := &stubAccountDeleter{emailUnverified: true, emailPrepared: prepared}
		h := privacy.NewHandler(stub, redistest.Client(t), logger.New("test"))
		h.SetErasureEmailSender(&recordingErasureEmailSender{err: errors.New("smtp down")})
		c, w := newTestContext(t, "", "send-fails-user")
		h.EraseAccount(c)
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.JSONEq(t, `{"error":"email confirmation unavailable"}`, w.Body.String())
		assert.False(t, stub.called)
	})
	t.Run("store unavailable", func(t *testing.T) {
		stub := &stubAccountDeleter{emailUnverified: true, emailPrepared: prepared}
		h := privacy.NewHandler(stub, nil, logger.New("test"))
		sender := &recordingErasureEmailSender{}
		h.SetErasureEmailSender(sender)
		c, w := newTestContext(t, "", "store-down-user")
		h.EraseAccount(c)
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.JSONEq(t, `{"error":"email confirmation unavailable"}`, w.Body.String())
		assert.Empty(t, sender.to)
	})
}

func TestEraseAccount_StaleOrIncompleteEmailConfirmation(t *testing.T) {
	prepared := users.EmailErasureConfirmation{
		Recipient: "unverified@example.test", FamilyID: uuid.NewString(), CredentialEpoch: "test", OwnedServerIDs: []string{},
	}
	for name, confirmation := range map[string]string{ //nolint:gosec // pragma: allowlist secret — malformed test tokens
		"empty request id": `{"request_id":"","token":"t"}`,
		"empty token":      `{"request_id":"r","token":""}`,
		"unknown intent":   `{"request_id":"00000000-0000-0000-0000-00000000000a","token":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`,
	} {
		t.Run(name, func(t *testing.T) {
			stub := &stubAccountDeleter{emailUnverified: true, emailPrepared: prepared}
			h := privacy.NewHandler(stub, redistest.Client(t), logger.New("test"))
			c, w := newTestContext(t, `{"password":"p","confirmed_owned_server_ids":[],"email_confirmation":`+confirmation+`}`, "stale-user")
			h.EraseAccount(c)
			assert.Equal(t, http.StatusConflict, w.Code)
			assert.JSONEq(t, `{"error_code":"email_confirmation_stale"}`, w.Body.String())
			assert.False(t, stub.called)
		})
	}
}

// An unresolved commit may have erased the account, so the claim is neither
// committed nor released: a retry of the same link stays refused.
func TestEraseAccount_UnresolvedCommitKeepsEmailConfirmationClaimed(t *testing.T) {
	rdb := redistest.Client(t)
	const recipient = "unverified@example.test"
	familyID := uuid.NewString()
	stub := &stubAccountDeleter{emailUnverified: true, err: presencecapture.ErrCommitUnresolved, emailPrepared: users.EmailErasureConfirmation{
		Recipient: recipient, FamilyID: familyID, CredentialEpoch: "test", OwnedServerIDs: []string{},
	}}
	h := privacy.NewHandler(stub, rdb, logger.New("test"))
	userID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("unresolved-user")).String()
	binding := privacy.EmailConfirmationBinding{Recipient: recipient, UserID: userID, FamilyID: familyID, CredentialEpoch: "test", OwnedServerIDs: []string{}}
	intent, err := privacy.NewEmailConfirmationStore(rdb).Issue(context.Background(), binding)
	require.NoError(t, err)

	c, _ := newTestContext(t, fmt.Sprintf(`{"password":"p","confirmed_owned_server_ids":[],"email_confirmation":{"request_id":"%s","token":"%s"}}`, intent.RequestID, intent.Token), "unresolved-user")
	h.EraseAccount(c)
	require.True(t, stub.called)

	_, err = privacy.NewEmailConfirmationStore(rdb).Claim(context.Background(), intent.RequestID, intent.Token, binding)
	require.Error(t, err, "an unresolved commit must leave the confirmation unusable")
}

func TestOwnedServerPreviewDatabaseFailureIs503(t *testing.T) {
	db, err := sql.Open("postgres", "host=127.0.0.1 port=1 user=test dbname=missing sslmode=disable connect_timeout=1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	h := privacy.NewHandler(&stubAccountDeleter{}, nil, logger.New("test"))
	h.SetOwnedServersDB(db)
	c, w := newTestContext(t, "", "preview-db-user")
	c.Request.Method = http.MethodGet
	h.ListOwnedServers(c)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.JSONEq(t, `{"error":"owned_server_list_unavailable"}`, w.Body.String())
}
