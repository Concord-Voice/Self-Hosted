package users_test

// A busy backup-code check on the SSO step-up judged nothing: the shared MFA
// budget gets its attempt back after the transaction ends, and the trust flag
// and linked identity stay as they were.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
)

func TestSSOStepUp_BusyBackupCheckRefundsTheCharge(t *testing.T) {
	for i, route := range []struct {
		name string
		call func(*testhelpers.TestServer, testhelpers.TestUser, string) *httptest.ResponseRecorder
	}{
		{name: "unlink", call: func(ts *testhelpers.TestServer, u testhelpers.TestUser, code string) *httptest.ResponseRecorder {
			return ts.DoRequest(http.MethodDelete, urlUsersMeSSOIdentities+"/google",
				map[string]any{"password": testhelpers.TestAuthPlaintext, "mfa_code": code}, testhelpers.AuthHeaders(u.AccessToken))
		}},
		{name: "trust enable", call: func(ts *testhelpers.TestServer, u testhelpers.TestUser, code string) *httptest.ResponseRecorder {
			return ts.DoRequest(methodPatch, urlUsersMeSecurity,
				map[string]any{"trust_sso_security": true, "current_passphrase": testhelpers.TestAuthPlaintext, "mfa_code": code},
				testhelpers.AuthHeaders(u.AccessToken))
		}},
	} {
		t.Run(route.name, func(t *testing.T) {
			ts := setupTS(t)
			user := ts.CreateTestUser(t, []string{"ssobusy_a", "ssobusy_b"}[i])
			ssoProofLinkGoogle(t, ts.DB, user)
			enableTOTP(t, ts, user)
			setBackupCode(t, ts.DB, user.ID, busyBackupCode)
			shared := stepup.DangerousActionBudget(ts.Redis).Key(user.ID)

			release := holdBackupHashSlots(t)
			// Two busy calls, not BudgetLimit+1: these routes also have a per-route
			// rate limit, which would answer 429 before the budget is the question.
			for range 2 {
				before := redisCount(t, ts.Redis, shared)
				requireVerificationBusy(t, route.call(ts, user, busyBackupCode))
				require.Equal(t, before, redisCount(t, ts.Redis, shared), "a busy check must not cost a shared attempt")
				requireSSOUnchanged(t, ts, user.ID)
			}
			release()

			w := route.call(ts, user, "00000000")
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			require.Equal(t, 1, redisCount(t, ts.Redis, shared), "a wrong code costs exactly one shared attempt")
			requireSSOUnchanged(t, ts, user.ID)
		})
	}
}
