package auth_test

// End-to-end regression for the registration code binding. The router runs on
// a stmthook pool; the hook fires immediately before Promote's
// `SELECT ... FOR UPDATE` (after the code guard accepted the OLD code) and
// commits PendingRepo.UpdateEmail on another pool. Promote must refuse and
// must not create a verified account for the new address.
// Needs PostgreSQL (DATABASE_URL) and Redis (REDIS_URL).

import (
	"context"
	"net/http"
	"testing"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/stmthook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChangeEmailRaceDoesNotVerifyUnownedAddress(t *testing.T) {
	hook, routerDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, routerDB)
	_, err := ts.DB.Exec(`DELETE FROM pending_registrations`)
	require.NoError(t, err)

	w := ts.DoRequest("POST", "/api/v1/auth/register", registerPayload(), nil)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var reg struct {
		PendingID string `json:"pending_id"`
	}
	testhelpers.ParseJSON(t, w, &reg)
	oldCode := testhelpers.FetchVerificationCode(t, ts, reg.PendingID)

	victim := "victim-vuln004@example.com"
	repo := auth.NewPendingRepo(ts.DB)
	hook.ArmArg([]string{"FOR UPDATE"}, reg.PendingID, func() error {
		return repo.UpdateEmail(context.Background(), reg.PendingID, victim)
	}, nil)

	w2 := ts.DoRequest("POST", "/api/v1/auth/register/confirm",
		map[string]string{"pending_id": reg.PendingID, "code": oldCode}, nil)
	seen, betweenErr := hook.Report()
	require.NoError(t, betweenErr)
	require.Equal(t, 1, seen, "hook did not fire at Promote")
	assert.NotEqual(t, http.StatusOK, w2.Code, "confirm accepted a code mailed to another address: %s", w2.Body.String())

	var users int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM users WHERE email = $1`, victim).Scan(&users))
	assert.Zero(t, users, "an account exists for an address that never received the code")
}
