package sessions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// VULN-014 regression: a Simple-mode revoke window belongs to the login family
// that proved its password. Family B proves and earns a window; family A, a
// second session of the same account, must still prove before it can revoke.
// The control (family B revoking the same target without a password) shows the
// window exists, so A's refusal comes from the family scope and nothing else.
func TestVuln014Regression_SimpleWindowIsPerLoginFamily(t *testing.T) {
	rig := newSecurityEventFamilyRig(t)
	rig.router.DELETE("/api/v1/sessions/:id", rig.handler.RevokeSession)
	ctx := context.Background()
	_, err := rig.db.Exec(`UPDATE users SET revocation_mode = 'simple' WHERE id = $1`, rig.user)
	require.NoError(t, err)

	const secret = "security-event-family-test-signing-secret" // pragma: allowlist secret
	minter := auth.NewHandler(rig.db, rig.redis, logger.New("test"), secret, testDisconnector{})
	accessB, _, sidB, err := minter.IssueAccessAndRefresh(ctx, rig.user)
	require.NoError(t, err)
	_, _, firstTarget, err := minter.IssueAccessAndRefresh(ctx, rig.user)
	require.NoError(t, err)
	_, _, secondTarget, err := minter.IssueAccessAndRefresh(ctx, rig.user)
	require.NoError(t, err)

	// Spend the window's one free revoke, so only a Simple-mode window can
	// waive the password below.
	require.NoError(t, rig.redis.ZAdd(ctx, revokeTrackingKey(rig.user), redis.Z{Score: float64(time.Now().Unix()), Member: "existing-revoke"}).Err())

	revoke := func(access, sid, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/"+sid, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer "+access)
		response := httptest.NewRecorder()
		rig.router.ServeHTTP(response, req)
		return response
	}

	proved := revoke(accessB, firstTarget, `{"password":"`+privacyPassword+`"}`)
	require.Equal(t, http.StatusOK, proved.Code, proved.Body.String())

	refused := revoke(rig.access, secondTarget, "")
	require.Equal(t, http.StatusForbidden, refused.Code, "family A must not spend family B's window: %s", refused.Body.String())
	require.JSONEq(t, `{"error":"password_required","message":"Password verification required to revoke this session"}`, refused.Body.String())
	require.True(t, sessionLive(t, rig, secondTarget), "a refused revoke must leave its target live")

	familyB := familyOf(t, rig, sidB)
	windowB, err := rig.redis.Get(ctx, simpleAuthWindowKey(rig.user, familyB)).Result()
	require.NoError(t, err)
	require.Equal(t, "1", windowB)
	windowA, err := rig.redis.Exists(ctx, simpleAuthWindowKey(rig.user, rig.familyID(t))).Result()
	require.NoError(t, err)
	require.Zero(t, windowA, "family A earned no window")

	control := revoke(accessB, secondTarget, "")
	require.Equal(t, http.StatusOK, control.Code, "family B's own window waives its password: %s", control.Body.String())
	require.False(t, sessionLive(t, rig, secondTarget))
}

func familyOf(t *testing.T, rig securityEventFamilyRig, sid string) string {
	t.Helper()
	var family string
	require.NoError(t, rig.db.QueryRow(`SELECT family_id FROM login_family_sids WHERE sid = $1`, sid).Scan(&family))
	return family
}

func sessionLive(t *testing.T, rig securityEventFamilyRig, sid string) bool {
	t.Helper()
	var live bool
	require.NoError(t, rig.db.QueryRow(
		`SELECT f.terminal_at IS NULL FROM login_family_sids s JOIN login_families f ON f.family_id = s.family_id WHERE s.sid = $1`, sid,
	).Scan(&live))
	return live
}
