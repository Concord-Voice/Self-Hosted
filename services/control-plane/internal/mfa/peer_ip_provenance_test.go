package mfa_test

// Client-IP provenance regression for the MFA verify RecordAuthFailure caller
// (refuseWrongCode, VULN-008 family). A wrong code sent from an untrusted peer
// that claims another address in X-Forwarded-For / X-Real-IP must be counted
// against the real peer; no Redis key may be derived from the claimed address.
//
// Mutant killed: a router that trusts every proxy (gin's default without
// SetTrustedProxies) or a handler that reads a forwarding header directly.

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

func TestVerifyWrongCode_SpoofedForwardingKeysPeer(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "peeripmfa")
	enrollTOTP(t, ts, user)

	w := ts.DoRequest(http.MethodPost, urlAuthLogin, map[string]interface{}{
		"email":    user.Email,
		"password": testPassword,
	}, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var loginBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &loginBody)
	challengeToken, ok := loginBody["mfa_challenge_token"].(string)
	require.True(t, ok, "login must return an MFA challenge: %v", loginBody)

	w = ts.DoRequest(http.MethodPost, urlMFAVerify, map[string]interface{}{
		"mfa_challenge_token": challengeToken,
		"method":              "totp",
		"code":                "000000",
	}, testhelpers.WithSpoofedForwarding(nil))
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())

	testhelpers.RequireAuthFailureKeyedToPeer(t, ts.Redis, testhelpers.TestPeerIP)
}
