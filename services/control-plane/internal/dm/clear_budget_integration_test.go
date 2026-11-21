package dm_test

// DM Clear charges the shared dangerous-action MFA budget for every request
// that carries an MFA code, before its transaction opens, and clears it only
// after a commit in which a code was verified.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

type clearBudgetFixture struct {
	ts     *testhelpers.TestServer
	actor  testhelpers.TestUser
	convID string
	code   func() string
}

func newClearBudgetFixture(t *testing.T, name string) clearBudgetFixture {
	t.Helper()
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, name+"_actor")
	peer := ts.CreateTestUser(t, name+"_peer")
	ts.CreateFriendship(t, actor.ID, peer.ID, statusAccepted)
	return clearBudgetFixture{
		ts: ts, actor: actor,
		convID: ts.CreateDMConversation(t, actor.ID, peer.ID),
		code:   enableVisibilityTOTP(t, ts, actor),
	}
}

// clear posts one Clear. The route's own fail-closed purge limiter precedes
// the handler, so its independent key is reset after each request to let the
// budget under test, not the limiter, decide the sixth.
func (f clearBudgetFixture) clear(t *testing.T, body map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	w := f.ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+f.convID+"/clear", body, testhelpers.AuthHeaders(f.actor.AccessToken))
	require.NoError(t, f.ts.Redis.Del(context.Background(), "ratelimit:user:"+f.actor.ID+":POST:/api/v1/dm/conversations/:id/clear").Err())
	return w
}

func (f clearBudgetFixture) budgetCount(t *testing.T) (int, bool) {
	t.Helper()
	n, err := f.ts.Redis.Get(context.Background(), stepup.DangerousActionBudget(f.ts.Redis).Key(f.actor.ID)).Int()
	if err != nil {
		return 0, false
	}
	return n, true
}

func clearRangeCount(t *testing.T, ts *testhelpers.TestServer, actorID, convID string) int {
	t.Helper()
	var n int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM dm_message_hidden_ranges WHERE user_id = $1 AND conversation_id = $2`, actorID, convID).Scan(&n))
	return n
}

func TestDMClear_SixthWrongMFACodeIsBudgetRefused(t *testing.T) {
	f := newClearBudgetFixture(t, "clear_budget_sixth")
	for i := 0; i < stepup.BudgetLimit; i++ {
		w := f.clear(t, map[string]string{"mfa_code": "000000"})
		require.Equal(t, http.StatusForbidden, w.Code, "attempt %d: %s", i+1, w.Body.String())
	}
	w := f.clear(t, map[string]string{"mfa_code": "000000"})
	require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "step_up_budget_exhausted")
	assert.NotEmpty(t, w.Header().Get("Retry-After"))

	// The budget refuses a correct code too, before any range is written.
	w = f.clear(t, map[string]string{"mfa_code": f.code()})
	require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
	assert.Zero(t, clearRangeCount(t, f.ts, f.actor.ID, f.convID))
}

func TestDMClear_VerifiedMFAClearResetsTheBudget(t *testing.T) {
	f := newClearBudgetFixture(t, "clear_budget_reset")
	require.Equal(t, http.StatusForbidden, f.clear(t, map[string]string{"mfa_code": "000000"}).Code)
	n, ok := f.budgetCount(t)
	require.True(t, ok, "a wrong code must charge the budget")
	require.Equal(t, 1, n)

	w := f.clear(t, map[string]string{"mfa_code": f.code()})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	_, ok = f.budgetCount(t)
	assert.False(t, ok, "a verified, committed clear must reset the budget")
}

func TestDMClear_RequestWithoutCodeDoesNotChargeTheBudget(t *testing.T) {
	f := newClearBudgetFixture(t, "clear_budget_nocode")
	w := f.clear(t, map[string]string{})
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	_, ok := f.budgetCount(t)
	assert.False(t, ok, "a request carrying no code guesses nothing and must not charge")
}
