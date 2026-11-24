package users_test

// Client-IP provenance regression for the keys/reveal RecordAuthFailure caller
// (VULN-008 family). A wrong password sent from an untrusted peer that claims
// another address in X-Forwarded-For / X-Real-IP must be counted against the
// real peer, and a ban on the claimed address must not stop the real peer;
// no Redis key may be derived from the claimed address.
//
// Mutant killed: a router that trusts every proxy (gin's default without
// SetTrustedProxies) or a handler that reads a forwarding header directly.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

func TestRevealMyKeysWrongPassword_SpoofedForwardingKeysPeer(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "peeripreveal")
	renewed := testhelpers.AuthHeaders(ts.RenewedAccessToken(t, user.ID))

	w := ts.DoRequest(http.MethodPost, urlUsersMeKeys+"/reveal",
		map[string]string{"current_password": "WrongPassword123!"}, // pragma: allowlist secret
		testhelpers.WithSpoofedForwarding(renewed))
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())

	testhelpers.RequireAuthFailureKeyedToPeer(t, ts.Redis, testhelpers.TestPeerIP)
}

// A ban on the address a request claims cannot refuse the real peer, and a
// ban on the real peer cannot be escaped by claiming another address.
func TestRevealMyKeys_SpoofedForwardingCannotDodgeOrRedirectBan(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "peeripban")
	renewed := testhelpers.AuthHeaders(ts.RenewedAccessToken(t, user.ID))
	ctx := context.Background()
	reveal := func() int {
		return ts.DoRequest(http.MethodPost, urlUsersMeKeys+"/reveal",
			map[string]string{"current_password": user.Password},
			testhelpers.WithSpoofedForwarding(renewed)).Code
	}

	require.NoError(t, ts.Redis.Set(ctx, "auth_ban:ip:"+testhelpers.SpoofedClientIP, "1", time.Minute).Err())
	assert.Equal(t, http.StatusOK, reveal(), "a ban on the claimed address must not refuse the real peer")
	require.NoError(t, ts.Redis.Del(ctx, "auth_ban:ip:"+testhelpers.SpoofedClientIP).Err())

	require.NoError(t, ts.Redis.Set(ctx, "auth_ban:ip:"+testhelpers.TestPeerIP, "1", time.Minute).Err())
	assert.Equal(t, http.StatusTooManyRequests, reveal(), "claiming another address must not escape the real peer's ban")
}
