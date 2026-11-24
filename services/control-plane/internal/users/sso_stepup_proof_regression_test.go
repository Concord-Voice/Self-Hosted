package users_test

// PATCH /users/me/security and DELETE /users/me/sso-identities/:provider judge
// a fresh proof under the users-row lock. These pin three properties of that
// proof: a trust-enabling patch is proved even when trust is already on,
// because the same body can change password login; the shared MFA budget is
// cleared only after a code actually verified; and a factor enrolled between
// the uncharged budget probe and the lock is refused for a retry.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/users"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
)

const ssoProofWrongPassphrase = "not-the-passphrase" // pragma: allowlist secret

func ssoProofLinkGoogle(t *testing.T, db *sql.DB, user testhelpers.TestUser) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
		VALUES ($1, 'google', $2, $3)`, user.ID, "sub-"+user.ID, user.Email)
	require.NoError(t, err)
}

func ssoProofFlags(t *testing.T, db *sql.DB, userID string) (passwordLoginDisabled, trust bool) {
	t.Helper()
	require.NoError(t, db.QueryRow(
		`SELECT password_login_disabled, trust_sso_security FROM users WHERE id = $1`, userID, // pragma: allowlist secret
	).Scan(&passwordLoginDisabled, &trust))
	return passwordLoginDisabled, trust
}

func ssoProofLinked(t *testing.T, db *sql.DB, userID string) bool {
	t.Helper()
	var linked bool
	require.NoError(t, db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM user_sso_identities WHERE user_id = $1 AND provider = 'google')`, userID,
	).Scan(&linked))
	return linked
}

// On an account whose SSO trust is already on, a body that re-sends the trust
// toggle must not waive the proof that changing password login needs.
func TestPatchSecurity_TrustedAccountPasswordLoginChangeNeedsProof(t *testing.T) {
	for i, tc := range []struct {
		name string
		totp bool
		body map[string]any
	}{
		{name: "no proof", body: map[string]any{
			"trust_sso_security": true, "password_login_disabled": true,
		}},
		{name: "wrong passphrase", body: map[string]any{
			"trust_sso_security": true, "password_login_disabled": true, "current_passphrase": ssoProofWrongPassphrase,
		}},
		{name: "passphrase without the enrolled factor", totp: true, body: map[string]any{
			"trust_sso_security": true, "password_login_disabled": true, "current_passphrase": testPassword,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := setupTS(t)
			user := ts.CreateTestUser(t, []string{"trustpld_a", "trustpld_b", "trustpld_c"}[i])
			_, err := ts.DB.Exec(`UPDATE users SET trust_sso_security = TRUE WHERE id = $1`, user.ID)
			require.NoError(t, err)
			ssoProofLinkGoogle(t, ts.DB, user)
			if tc.totp {
				testhelpers.EnrollInlineTOTP(t, ts.DB, user.ID)
			}

			w := ts.DoRequest(methodPatch, urlUsersMeSecurity, tc.body, testhelpers.AuthHeaders(user.AccessToken))

			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			pld, trust := ssoProofFlags(t, ts.DB, user.ID)
			assert.False(t, pld, "password login must not change without a verified proof")
			assert.True(t, trust)
		})
	}
}

// A code that verified nothing must leave the shared MFA budget as it was. An
// account with no inline factor judges no code, so a change it makes while
// carrying one neither charges nor clears the budget.
func TestSSOStepUp_UnverifiedCodeLeavesTheMFABudget(t *testing.T) {
	const charged = 3
	for i, tc := range []struct {
		name    string
		trusted bool
		call    func(*testhelpers.TestServer, testhelpers.TestUser) *httptest.ResponseRecorder
	}{
		{name: "trust enable", call: func(ts *testhelpers.TestServer, user testhelpers.TestUser) *httptest.ResponseRecorder {
			return ts.DoRequest(methodPatch, urlUsersMeSecurity, map[string]any{
				"trust_sso_security": true, "current_passphrase": testPassword, "mfa_code": "123456",
			}, testhelpers.AuthHeaders(user.AccessToken))
		}},
		{name: "trust re-sent on a trusted account", trusted: true, call: func(ts *testhelpers.TestServer, user testhelpers.TestUser) *httptest.ResponseRecorder {
			return ts.DoRequest(methodPatch, urlUsersMeSecurity, map[string]any{
				"trust_sso_security": true, "current_passphrase": testPassword, "mfa_code": "123456",
			}, testhelpers.AuthHeaders(user.AccessToken))
		}},
		{name: "unlink", call: func(ts *testhelpers.TestServer, user testhelpers.TestUser) *httptest.ResponseRecorder {
			return ts.DoRequest(http.MethodDelete, urlUsersMeSSOIdentities+"/google", map[string]any{
				"password": testPassword, "mfa_code": "123456",
			}, testhelpers.AuthHeaders(user.AccessToken))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := setupTS(t)
			user := ts.CreateTestUser(t, []string{"ssobudget_a", "ssobudget_b", "ssobudget_c"}[i])
			ssoProofLinkGoogle(t, ts.DB, user)
			if tc.trusted {
				_, err := ts.DB.Exec(`UPDATE users SET trust_sso_security = TRUE WHERE id = $1`, user.ID)
				require.NoError(t, err)
			}
			ctx := context.Background()
			key := stepup.DangerousActionBudget(ts.Redis).Key(user.ID)
			require.NoError(t, ts.Redis.Set(ctx, key, charged, 0).Err())

			w := tc.call(ts, user)

			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			n, err := ts.Redis.Get(ctx, key).Int()
			require.NoError(t, err, "an unverified code must not clear the budget")
			assert.Equal(t, charged, n)
		})
	}
}

// ssoProofEnrollingHandler wires a users handler whose Redis client enrolls a
// WebAuthn factor before its first command. In the SSO step-up that command is
// the password reservation: after the budget probe found no factor, and before
// the transaction takes the users-row lock.
func ssoProofEnrollingHandler(t *testing.T, ts *testhelpers.TestServer, userID string) *users.Handler {
	t.Helper()
	h := users.NewHandler(ts.DB, logger.NewWithWriter(io.Discard), nil, krMFAHandler(t, ts.DB, ts.Redis), nil, testCredFence(t, ts.DB), nil)
	h.SetPasswordVerifier(testhelpers.HashVerifier{DB: ts.DB})
	shared := ts.Redis.Options()
	rdb := redis.NewClient(&redis.Options{Addr: shared.Addr, Username: shared.Username, Password: shared.Password, DB: shared.DB})
	t.Cleanup(func() { _ = rdb.Close() })
	rdb.AddHook(kpEnrollOnFenceHook{once: &sync.Once{}, enroll: func() {
		_, err := ts.DB.Exec(`INSERT INTO user_mfa_webauthn (id, user_id, credential_id, credential_name, credential_type, public_key, sign_count, created_at)
			VALUES (gen_random_uuid(), $1, $2, 'Key', 'hardware', '\x00', 0, NOW())`, userID, []byte("sso-cred-"+userID))
		if err == nil {
			_, err = ts.DB.Exec(`UPDATE users SET mfa_enabled = true, mfa_methods = ARRAY['webauthn'] WHERE id = $1`, userID)
		}
		assert.NoError(t, err)
	}})
	h.SetRedis(rdb)
	return h
}

func ssoProofCall(t *testing.T, method, path string, user testhelpers.TestUser, params gin.Params, body map[string]any, handle gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("user_id", user.ID)
	c.Params = params
	c.Request = httptest.NewRequest(method, path, bytes.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")
	testhelpers.AttachAccessIdentity(t, c, user)
	handle(c)
	return w
}

// A factor committed after the uncharged probe must be refused for a retry,
// not judged for free, and nothing may change.
func TestSSOStepUp_FactorEnrolledAfterProbeIsRefused(t *testing.T) {
	for i, route := range []string{"trust enable", "unlink"} {
		t.Run(route, func(t *testing.T) {
			ts := setupTS(t)
			user := ts.CreateTestUser(t, []string{"ssorace_a", "ssorace_b"}[i])
			ssoProofLinkGoogle(t, ts.DB, user)
			h := ssoProofEnrollingHandler(t, ts, user.ID)

			var w *httptest.ResponseRecorder
			if route == "unlink" {
				w = ssoProofCall(t, http.MethodDelete, urlUsersMeSSOIdentities+"/google", user,
					gin.Params{{Key: "provider", Value: "google"}},
					map[string]any{"password": user.Password, "mfa_code": "000000"}, h.DeleteSSOIdentity)
			} else {
				w = ssoProofCall(t, http.MethodPatch, urlUsersMeSecurity, user, nil,
					map[string]any{"trust_sso_security": true, "current_passphrase": user.Password, "mfa_code": "000000"}, h.PatchSecurity)
			}

			require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), "MFA enrollment changed")
			_, err := ts.Redis.Get(context.Background(), stepup.DangerousActionBudget(ts.Redis).Key(user.ID)).Result()
			assert.ErrorIs(t, err, redis.Nil, "the refused request was never charged")
			_, trust := ssoProofFlags(t, ts.DB, user.ID)
			assert.False(t, trust, "the refused patch changed nothing")
			assert.True(t, ssoProofLinked(t, ts.DB, user.ID), "the refused unlink removed nothing")
		})
	}
}
