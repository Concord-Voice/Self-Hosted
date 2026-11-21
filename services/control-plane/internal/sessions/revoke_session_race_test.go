package sessions_test

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	dbtest "github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/testdb"
	"github.com/stretchr/testify/require"
)

const (
	freeRevokeBarrierLockID   int64 = 2457002
	freeRevokeBarrierFunction       = `
		CREATE OR REPLACE FUNCTION free_revoke_effect_barrier() RETURNS trigger AS $$
		BEGIN
			PERFORM pg_advisory_xact_lock(2457002);
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql`
)

type freeRevokeResponse struct {
	code int
	body string
}

// The first request holds the user lock after policy admission but before its
// effect commits. The second must wait for that transaction, then require step-up.
func TestRevokeSession_ConcurrentRequestsAllowOnlyOneFreeRevoke(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "concurrentfreerevoke")
	firstTarget := createSession(t, ts, user.ID, "Device1", testIP1)
	secondTarget := createSession(t, ts, user.ID, "Device2", testIP2)
	createSession(t, ts, user.ID, "Device3", "10.0.0.3")
	createSession(t, ts, user.ID, "Device4", "10.0.0.4")
	assertSessionState(t, ts, user.ID, 4, false)

	installFreeRevokeEffectBarrier(t, ts.DB)
	releaseBarrier := holdFreeRevokeEffectBarrier(t)
	request := func(target string) freeRevokeResponse {
		response := ts.DoRequest(http.MethodDelete, sessionsPath+"/"+target, nil,
			testhelpers.AuthHeaders(user.AccessToken))
		return freeRevokeResponse{code: response.Code, body: response.Body.String()}
	}

	firstDone := make(chan freeRevokeResponse, 1)
	go func() { firstDone <- request(firstTarget) }()
	dbtest.WaitForAdvisoryLockWaiter(t, ts.DB, freeRevokeBarrierLockID)
	classID, objectID := dbtest.AdvisoryKeyHalves(freeRevokeBarrierLockID)
	var firstTxID int64
	require.NoError(t, ts.DB.QueryRow(`
		SELECT x.transactionid::text::bigint
		FROM pg_locks a JOIN pg_locks x ON x.pid=a.pid
		WHERE a.locktype='advisory' AND a.objsubid=1 AND a.classid::bigint=$1
		  AND a.objid::bigint=$2 AND NOT a.granted
		  AND x.locktype='transactionid' AND x.mode='ExclusiveLock' AND x.granted`,
		classID, objectID).Scan(&firstTxID))

	secondDone := make(chan freeRevokeResponse, 1)
	go func() { secondDone <- request(secondTarget) }()
	dbtest.WaitForRowLockWaiter(t, ts.DB, firstTxID)

	releaseBarrier()
	select {
	case first := <-firstDone:
		require.Equal(t, http.StatusOK, first.code, first.body)
	case <-time.After(10 * time.Second):
		t.Fatal("first revoke did not finish after releasing its database barrier")
	}
	select {
	case second := <-secondDone:
		require.Equal(t, http.StatusForbidden, second.code, second.body)
		require.JSONEq(t, `{"error":"password_required","message":"Password verification required to revoke this session"}`, second.body)
	case <-time.After(10 * time.Second):
		t.Fatal("second revoke did not finish after the first committed")
	}

	assertSessionFamilyRevoked(t, ts, user.ID, firstTarget)
	assertSessionFamilyLive(t, ts, user.ID, secondTarget)
	assertSessionState(t, ts, user.ID, 3, true)
}

func installFreeRevokeEffectBarrier(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(freeRevokeBarrierFunction)
	require.NoError(t, err)
	_, err = db.Exec(`
		CREATE TRIGGER free_revoke_effect_barrier AFTER INSERT ON login_family_revocations
		FOR EACH ROW EXECUTE FUNCTION free_revoke_effect_barrier()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, dropErr := db.Exec(`DROP TRIGGER IF EXISTS free_revoke_effect_barrier ON login_family_revocations`); dropErr != nil {
			t.Errorf("drop free-revoke barrier trigger: %v", dropErr)
		}
		if _, dropErr := db.Exec(`DROP FUNCTION IF EXISTS free_revoke_effect_barrier()`); dropErr != nil {
			t.Errorf("drop free-revoke barrier function: %v", dropErr)
		}
	})
}

func holdFreeRevokeEffectBarrier(t *testing.T) func() {
	t.Helper()
	lockDB, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	lockDB.SetMaxOpenConns(1)
	conn, err := lockDB.Conn(context.Background())
	require.NoError(t, err)
	_, err = conn.ExecContext(context.Background(), `SELECT pg_advisory_lock($1)`, freeRevokeBarrierLockID)
	require.NoError(t, err)

	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if _, unlockErr := conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, freeRevokeBarrierLockID); unlockErr != nil {
			t.Errorf("release free-revoke effect barrier: %v", unlockErr)
		}
		if closeErr := conn.Close(); closeErr != nil {
			t.Errorf("close free-revoke barrier connection: %v", closeErr)
		}
		if closeErr := lockDB.Close(); closeErr != nil {
			t.Errorf("close free-revoke barrier database: %v", closeErr)
		}
	}
	t.Cleanup(release)
	return release
}
