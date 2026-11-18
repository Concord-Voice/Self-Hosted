package auth

// VULN-011 regression: a bearer for login family A must not log out the same
// user's other login family B via X-Session-ID. Ported from the scan exploit
// tests (component + end-to-end) and inverted: the request is refused and
// family B (and A) are left byte-for-byte unchanged. Needs PostgreSQL + Redis.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVuln011Regression_BearerCannotLogOutOtherFamilyViaXSessionID(t *testing.T) {
	rig := newFamilyTestRig(t)
	attackerPair, attackerFamily, attackerSID := terminalMint(t, rig) // stolen bearer (family A)
	victimPair, victimFamily, victimSID := terminalMint(t, rig)       // victim's other device (family B)
	require.NotEqual(t, attackerFamily, victimFamily)
	require.NotEqual(t, attackerSID, victimSID)

	beforeVictim := logoutPresenceSnapshot(t, rig, victimFamily)
	beforeAttacker := logoutPresenceSnapshot(t, rig, attackerFamily)

	c, w := familyTestContext()
	c.Request.URL.Path = "/api/v1/auth/logout"
	c.Request.Header.Set("Authorization", "Bearer "+attackerPair.Access)
	c.Request.Header.Set("X-Session-ID", victimSID)
	rig.h.Logout(c)

	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.JSONEq(t, `{"error_code":"invalid_logout_target"}`, w.Body.String())
	for name, family := range map[string]string{"victim": victimFamily, "attacker": attackerFamily} {
		at, owner := terminalState(t, rig, family)
		assert.False(t, at.Valid, name+" family must not be terminated")
		assert.False(t, owner.Valid, name+" family must have no revocation owner")
	}
	assert.Equal(t, beforeVictim, logoutPresenceSnapshot(t, rig, victimFamily), "victim family rows changed")
	assert.Equal(t, beforeAttacker, logoutPresenceSnapshot(t, rig, attackerFamily), "attacker family rows changed")
	logoutPresenceAssertOperationCounts(t, rig, rig.user, 0)
	_ = victimPair
}

// Component level: the selector still names the supplied sid, so the refusal
// must come from the family guard in revokeLogoutSession, not from selection.
func TestVuln011Regression_SelectorDefersToFamilyGuard(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	sel := selectLogoutTarget(c, &Claims{UserID: "u", SessionID: "a"}, "", "b")
	assert.Equal(t, "b", sel.sid)
	assert.True(t, sel.bearerAuthorized)
	assert.Empty(t, sel.tokenHash)
}
