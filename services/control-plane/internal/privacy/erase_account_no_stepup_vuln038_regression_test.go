package privacy_test

// VULN-038 regression: account erasure must not run on a bearer alone.
// Ported from the scan exploit test and inverted: through the full router,
// a request with no password / MFA proof is refused and the account, its
// owned server, and its channels survive (the FK cascade is never triggered).

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
)

func TestVuln038Regression_EraseAccountBearerOnlyRefused(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	victim := ts.CreateTestUser(t, "vuln038victim")
	serverID := ts.CreateTestServer(t, victim.ID, "vuln038 owned")
	path := "/api/v1/privacy/erase-account"
	headers := testhelpers.AuthHeaders(victim.AccessToken)

	for name, body := range map[string]interface{}{
		"no body":                 nil,
		"empty object":            map[string]interface{}{},
		"acknowledgement only":    map[string]interface{}{"confirmed_owned_server_ids": []string{}},
		"owned server ack only":   map[string]interface{}{"confirmed_owned_server_ids": []string{serverID}},
		"wrong password":          map[string]interface{}{"password": "not-the-password", "confirmed_owned_server_ids": []string{serverID}}, // pragma: allowlist secret
		"legacy clientId, no pwd": map[string]interface{}{"clientId": "attacker", "confirmed_owned_server_ids": []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			w := ts.DoRequest(http.MethodPost, path, body, headers)
			assert.GreaterOrEqual(t, w.Code, 400, w.Body.String())
			assert.Less(t, w.Code, 500, w.Body.String())
			assert.NotEqual(t, http.StatusNoContent, w.Code)

			var users, servers int
			require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM users WHERE id = $1`, victim.ID).Scan(&users))
			require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM servers WHERE id = $1`, serverID).Scan(&servers))
			require.Equal(t, 1, users, "bearer-only erasure must not delete the user")
			require.Equal(t, 1, servers, "owned server must survive a refused erasure")
		})
	}
}
