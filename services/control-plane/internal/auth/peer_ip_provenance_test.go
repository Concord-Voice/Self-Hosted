package auth_test

// Client-IP provenance regressions for every RecordAuthFailure caller in
// internal/auth (VULN-008 family). Each request comes from an untrusted TCP
// peer (the TestServer trusts no proxy) and claims another address in
// X-Forwarded-For and X-Real-IP. The failure must be counted against the real
// peer, and no Redis key may be built from the claimed address; otherwise an
// attacker rotates the header to dodge the ban, or bans a victim's address.
//
// Mutant killed: a router that trusts every proxy (gin's default when
// SetTrustedProxies is not applied), or a handler that reads a forwarding
// header directly — either keys the counter on the spoofed address.

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

func TestLoginWrongPassword_SpoofedForwardingKeysPeer(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "peeriplogin")

	w := ts.DoRequest(http.MethodPost, "/api/v1/auth/login",
		map[string]string{"email": user.Email, "password": testWrongPassword},
		testhelpers.WithSpoofedForwarding(nil))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())

	testhelpers.RequireAuthFailureKeyedToPeer(t, ts.Redis, testhelpers.TestPeerIP)
}

func TestLoginUnknownEmail_SpoofedForwardingKeysPeer(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest(http.MethodPost, "/api/v1/auth/login",
		map[string]string{"email": "nobody-peerip@test.concord.chat", "password": testWrongPassword},
		testhelpers.WithSpoofedForwarding(nil))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())

	testhelpers.RequireAuthFailureKeyedToPeer(t, ts.Redis, testhelpers.TestPeerIP)
}

// A successful login clears the counter of the real peer, not of the address
// the request claims (ClearAuthFailures shares the key with RecordAuthFailure).
func TestLoginSuccess_SpoofedForwardingClearsPeerCounter(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "peeripclear")
	ctx := context.Background()

	w := ts.DoRequest(http.MethodPost, "/api/v1/auth/login",
		map[string]string{"email": user.Email, "password": testWrongPassword},
		testhelpers.WithSpoofedForwarding(nil))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	testhelpers.RequireAuthFailureKeyedToPeer(t, ts.Redis, testhelpers.TestPeerIP)

	w = ts.DoRequest(http.MethodPost, "/api/v1/auth/login",
		map[string]string{"email": user.Email, "password": user.Password},
		testhelpers.WithSpoofedForwarding(nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	n, err := ts.Redis.Exists(ctx, "auth_failures:ip:"+testhelpers.TestPeerIP).Result()
	require.NoError(t, err)
	assert.Zero(t, n, "a successful login must clear the real peer's failure counter")
	testhelpers.RequireNoRedisKeyMentions(t, ts.Redis, testhelpers.SpoofedClientIP)
}

func TestRecoveryVerifyCodeWrongCode_SpoofedForwardingKeysPeer(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "peeriprecovery")
	seedRecoveryCode(t, ts, user.Email, testRecoveryCode, user.ID)

	w := ts.DoRequest(http.MethodPost, pathRecoveryVerifyCode,
		map[string]string{"email": user.Email, "code": "000000"},
		testhelpers.WithSpoofedForwarding(nil))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())

	testhelpers.RequireAuthFailureKeyedToPeer(t, ts.Redis, testhelpers.TestPeerIP)
}

func TestRecoveryResetPasswordBadToken_SpoofedForwardingKeysPeer(t *testing.T) {
	ts := setupTS(t)
	_, priv, salt := testhelpers.E2EETestKeys()

	w := ts.DoRequest(http.MethodPost, pathRecoveryResetPwd, map[string]string{
		"recovery_token":      "not-a-valid-recovery-token",
		"new_password":        testNewPassword,
		"wrapped_private_key": priv,
		"key_derivation_salt": salt,
		"key_derivation_alg":  testKeyDerivationAlg,
	}, testhelpers.WithSpoofedForwarding(nil))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())

	testhelpers.RequireAuthFailureKeyedToPeer(t, ts.Redis, testhelpers.TestPeerIP)
}
