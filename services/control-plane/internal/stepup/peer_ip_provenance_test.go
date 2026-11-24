package stepup_test

// Client-IP provenance regression for the step-up password mint's
// RecordAuthFailure caller (VULN-008 family). A wrong password sent from an
// untrusted peer that claims another address in X-Forwarded-For / X-Real-IP
// must be counted against the real peer; no Redis key may be derived from the
// claimed address.
//
// Mutant killed: a router that trusts every proxy (gin's default without
// SetTrustedProxies) or a handler that reads a forwarding header directly.

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

func TestMintWrongPassword_SpoofedForwardingKeysPeer(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "peeripstepup")

	w := ts.DoRequest(http.MethodPost, mintPath,
		map[string]string{"current_password": "wrong-password", "purpose": "dm.clear"},
		testhelpers.WithSpoofedForwarding(testhelpers.AuthHeaders(user.AccessToken)))
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())

	testhelpers.RequireAuthFailureKeyedToPeer(t, ts.Redis, testhelpers.TestPeerIP)
}
