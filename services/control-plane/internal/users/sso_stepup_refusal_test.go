package users_test

// The SSO step-up refusals: each one must answer its own status and leave the
// account's trust flag and linked identity exactly as they were.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/users"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
)

// ssoRefusalVerifier adapts FakeMFAVerifier to users.MFAVerifier.
type ssoRefusalVerifier struct{ *testhelpers.FakeMFAVerifier }

func (ssoRefusalVerifier) IsEnabled(context.Context, string) bool { return true }
func (v ssoRefusalVerifier) VerifyCode(_ context.Context, _ string, _ stepup.Purpose, code string) (bool, error) {
	return code != "" && code == v.AcceptCode, nil
}

func ssoRefusalHandler(t *testing.T, ts *testhelpers.TestServer, verifier users.MFAVerifier) *users.Handler {
	t.Helper()
	h := users.NewHandler(ts.DB, logger.NewWithWriter(io.Discard), nil, verifier, nil, testCredFence(t, ts.DB), nil)
	h.SetPasswordVerifier(testhelpers.HashVerifier{DB: ts.DB})
	h.SetRedis(ts.Redis)
	return h
}

// ssoRefusalRoutes drives the two SSO step-up routes with one proof.
var ssoRefusalRoutes = []struct {
	name string
	call func(*testing.T, *users.Handler, testhelpers.TestUser, string, string) *httptest.ResponseRecorder
}{
	{name: "unlink", call: func(t *testing.T, h *users.Handler, u testhelpers.TestUser, password, code string) *httptest.ResponseRecorder {
		return ssoProofCall(t, http.MethodDelete, urlUsersMeSSOIdentities+"/google", u,
			gin.Params{{Key: "provider", Value: "google"}},
			map[string]any{"password": password, "mfa_code": code}, h.DeleteSSOIdentity)
	}},
	{name: "trust enable", call: func(t *testing.T, h *users.Handler, u testhelpers.TestUser, password, code string) *httptest.ResponseRecorder {
		return ssoProofCall(t, http.MethodPatch, urlUsersMeSecurity, u, nil,
			map[string]any{"trust_sso_security": true, "current_passphrase": password, "mfa_code": code}, h.PatchSecurity)
	}},
}

func requireSSOUnchanged(t *testing.T, ts *testhelpers.TestServer, userID string) {
	t.Helper()
	_, trust := ssoProofFlags(t, ts.DB, userID)
	assert.False(t, trust, "a refused patch must not enable trust")
	assert.True(t, ssoProofLinked(t, ts.DB, userID), "a refused unlink must not remove the identity")
}

func TestDeleteSSOIdentity_RejectsMalformedRequests(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "ssobadreq")
	ssoProofLinkGoogle(t, ts.DB, user)
	h := ssoRefusalHandler(t, ts, nil)

	t.Run("not JSON", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set("user_id", user.ID)
		c.Params = gin.Params{{Key: "provider", Value: "google"}}
		c.Request = httptest.NewRequest(http.MethodDelete, urlUsersMeSSOIdentities+"/google", bytes.NewReader([]byte("{")))
		c.Request.Header.Set("Content-Type", "application/json")
		testhelpers.AttachAccessIdentity(t, c, user)
		h.DeleteSSOIdentity(c)
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), "invalid_request")
	})
	t.Run("step_up_token is refused", func(t *testing.T) {
		w := ssoProofCall(t, http.MethodDelete, urlUsersMeSSOIdentities+"/google", user,
			gin.Params{{Key: "provider", Value: "google"}},
			map[string]any{"password": user.Password, "step_up_token": "test-token-123"}, h.DeleteSSOIdentity)
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), "invalid_request")
	})
	assert.True(t, ssoProofLinked(t, ts.DB, user.ID))
}

func TestSSOStepUp_LockedAccountIs423(t *testing.T) {
	for i, route := range ssoRefusalRoutes {
		t.Run(route.name, func(t *testing.T) {
			ts := setupTS(t)
			user := ts.CreateTestUser(t, []string{"ssolock_a", "ssolock_b"}[i])
			ssoProofLinkGoogle(t, ts.DB, user)
			require.NoError(t, ts.Redis.Set(context.Background(), "login_lockout:"+user.Email, "1", time.Minute).Err())

			w := route.call(t, ssoRefusalHandler(t, ts, nil), user, user.Password, "")

			require.Equal(t, http.StatusLocked, w.Code, w.Body.String())
			requireSSOUnchanged(t, ts, user.ID)
		})
	}
}

func TestSSOStepUp_SpentMFABudgetIs429(t *testing.T) {
	for i, route := range ssoRefusalRoutes {
		t.Run(route.name, func(t *testing.T) {
			ts := setupTS(t)
			user := ts.CreateTestUser(t, []string{"ssospent_a", "ssospent_b"}[i])
			ssoProofLinkGoogle(t, ts.DB, user)
			testhelpers.EnrollInlineTOTP(t, ts.DB, user.ID)
			budget := stepup.DangerousActionBudget(ts.Redis)
			for range stepup.BudgetLimit {
				require.Nil(t, stepup.Charge(context.Background(), budget, user.ID, "000000"))
			}
			verifier := &testhelpers.FakeMFAVerifier{AcceptCode: "123456"}

			w := route.call(t, ssoRefusalHandler(t, ts, ssoRefusalVerifier{verifier}), user, user.Password, "123456")

			require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
			assert.Zero(t, verifier.Calls(), "a spent budget judges no code")
			requireSSOUnchanged(t, ts, user.ID)
		})
	}
}

func TestSSOStepUp_MissingMFAVerifierFailsClosed(t *testing.T) {
	for i, route := range ssoRefusalRoutes {
		t.Run(route.name, func(t *testing.T) {
			ts := setupTS(t)
			user := ts.CreateTestUser(t, []string{"ssonover_a", "ssonover_b"}[i])
			ssoProofLinkGoogle(t, ts.DB, user)
			testhelpers.EnrollInlineTOTP(t, ts.DB, user.ID)

			w := route.call(t, ssoRefusalHandler(t, ts, nil), user, user.Password, "123456")

			require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), stepup.ErrMsgVerificationFailed)
			requireSSOUnchanged(t, ts, user.ID)
		})
	}
}

// A wrong code changes nothing; the right one applies the change and clears
// the shared budget the request charged.
func TestSSOStepUp_MFACodeIsJudged(t *testing.T) {
	for i, route := range ssoRefusalRoutes {
		t.Run(route.name, func(t *testing.T) {
			ts := setupTS(t)
			user := ts.CreateTestUser(t, []string{"ssocode_a", "ssocode_b"}[i])
			ssoProofLinkGoogle(t, ts.DB, user)
			testhelpers.EnrollInlineTOTP(t, ts.DB, user.ID)
			h := ssoRefusalHandler(t, ts, ssoRefusalVerifier{&testhelpers.FakeMFAVerifier{AcceptCode: "123456"}})

			w := route.call(t, h, user, user.Password, "654321")
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), stepup.ErrMsgInvalidMFACode)
			requireSSOUnchanged(t, ts, user.ID)

			w = route.call(t, h, user, user.Password, "123456")
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			_, err := ts.Redis.Get(context.Background(), stepup.DangerousActionBudget(ts.Redis).Key(user.ID)).Result()
			assert.ErrorIs(t, err, redis.Nil, "a verified, applied change clears the budget")
			if route.name == "unlink" {
				assert.False(t, ssoProofLinked(t, ts.DB, user.ID))
			} else {
				_, trust := ssoProofFlags(t, ts.DB, user.ID)
				assert.True(t, trust)
			}
		})
	}
}
