package channels_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// Regressions for the channel key-distribution transaction (VULN-079,
// VULN-080): concurrent honest distributions must not time out on the channel
// lock, pending-row cleanup must not delete a request a target still needs,
// and a distribution that overruns its bounds must still get a 503 to the
// client before the server's write deadline.

// waitForLockWaiters polls pg_stat_activity until at least n sessions in this
// database wait on a lock while running a query that matches pattern. It
// returns false early when done closes, and after 10 s.
func waitForLockWaiters(t *testing.T, db *sql.DB, pattern string, n int, done <-chan struct{}) bool {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var got int
		require.NoError(t, db.QueryRow(`
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND query ILIKE $1`,
			pattern).Scan(&got))
		if got >= n {
			return true
		}
		select {
		case <-done:
			return false
		case <-time.After(5 * time.Millisecond):
		}
	}
	return false
}

// bulkViewingMembers inserts n users who are members of serverID with its
// default roles and an identity key at version 1. Inserting them in bulk keeps
// a 500-target fixture fast enough for CI.
func bulkViewingMembers(t *testing.T, ts *testhelpers.TestServer, serverID string, n int) []string {
	t.Helper()
	ids := make([]string, n)
	for i := range ids {
		ids[i] = uuid.NewString()
	}
	_, err := ts.DB.Exec(`
		INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		SELECT u, u::text || '@test.local', 'u_' || left(replace(u::text, '-', ''), 20), 'x', true, true
		FROM unnest($1::uuid[]) AS t(u)`, pq.Array(ids))
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		INSERT INTO server_members (server_id, user_id, role)
		SELECT $1, u, 'member' FROM unnest($2::uuid[]) AS t(u)`, serverID, pq.Array(ids))
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		INSERT INTO member_roles (server_id, user_id, role_id)
		SELECT $1, t.u, r.id FROM unnest($2::uuid[]) AS t(u)
		CROSS JOIN roles r WHERE r.server_id = $1 AND r.is_default = TRUE`, serverID, pq.Array(ids))
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		INSERT INTO public_keys (user_id, public_key, key_version)
		SELECT u, $2::bytea, 1 FROM unnest($1::uuid[]) AS t(u)`, pq.Array(ids), []byte("test-public-key"))
	require.NoError(t, err)
	return ids
}

// TestDistribution_ConcurrentHonestDistributionsSucceed: every online holder
// answers the same key_needed fan-out at once. The distributions serialize on
// the channel row lock, so each must hold it briefly enough that the last one
// in the queue still gets it inside lock_timeout. With one access check per
// target the queue behind a 500-target batch ran past 5 s and honest requests
// got 503 on a healthy database; 250 targets still does on that code (about
// 7 s per batch under -race), and leaves the set-based path a wide margin on a
// slow CI runner.
func TestDistribution_ConcurrentHonestDistributionsSucceed(t *testing.T) {
	const holders, targets = 8, 250
	ts, _, serverID, channelID := setupEncryptedChannel(t)
	hs := make([]testhelpers.TestUser, holders)
	for i := range hs {
		hs[i] = ts.CreateTestUser(t, fmt.Sprintf("contend%d", i))
		ts.AddMemberToServer(t, serverID, hs[i].ID, roleMember)
		seedChannelKey(t, ts, channelID, hs[i].ID, 1)
	}
	ids := bulkViewingMembers(t, ts, serverID, targets)
	wrapped := make(map[string]string, targets)
	versions := make(map[string]int, targets)
	for _, id := range ids {
		wrapped[id] = testhelpers.ValidCiphertext()
		versions[id] = 1 // fresh: every target passes the #2420 guard
	}

	results := make([]*httptest.ResponseRecorder, holders)
	took := make([]time.Duration, holders)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range hs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s := time.Now()
			results[i] = distributePost(ts, hs[i].AccessToken, channelID, wrapped, versions)
			took[i] = time.Since(s)
		}()
	}
	close(start)
	wg.Wait()

	var delivered, duplicates float64
	for i, w := range results {
		t.Logf("holder %d: %d in %v", i, w.Code, took[i].Round(time.Millisecond))
		require.Equal(t, http.StatusOK, w.Code, "holder %d: an honest distribution must not time out: %s", i, w.Body.String())
		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		delivered += testhelpers.JSONField[float64](t, body, "distributed")
		duplicates += testhelpers.JSONField[float64](t, body, "duplicates")
	}
	assert.Equal(t, float64(targets), delivered, "each target is delivered exactly once")
	assert.Equal(t, float64(targets*(holders-1)), duplicates, "every other holder sees a duplicate")
	assert.Equal(t, targets, rowCount(t, ts,
		`SELECT COUNT(*) FROM channel_keys WHERE channel_id = $1 AND user_id = ANY($2::uuid[])`, channelID, pq.Array(ids)))
}

// createEncryptedChannel creates a text channel in serverID whose key owner holds.
func createEncryptedChannel(t *testing.T, ts *testhelpers.TestServer, owner testhelpers.TestUser, serverID, name string) string {
	t.Helper()
	w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
		keyServerID: serverID, "name": name, "type": "text",
		keyWrappedKeys: map[string]string{owner.ID: testhelpers.ValidCiphertext()},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	channel, ok := body[keyChannel].(map[string]interface{})
	require.True(t, ok)
	id, ok := channel["id"].(string)
	require.True(t, ok)
	return id
}

// resetIdentityKey has a key reset's effect on what distribution reads: the
// identity key version moves and every wrapped channel key is purged.
func resetIdentityKey(t *testing.T, db *sql.DB, userID string) {
	t.Helper()
	require.NoError(t, resetIdentityKeyTx(db, userID))
}

// resetIdentityKeyTx is resetIdentityKey for a goroutine: it returns the error.
func resetIdentityKeyTx(db *sql.DB, userID string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE public_keys SET key_version = key_version + 1 WHERE user_id = $1`, userID); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	if _, err := tx.Exec(`DELETE FROM channel_keys WHERE user_id = $1`, userID); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}

type resetRaceResult struct {
	distCode, getCode int
	getPending        bool
	keys, pending     int
}

// runResetDuringDistribution: the distribution sends V (who holds the key) and
// W, and stalls on W's identity-key lock. While it is stalled, V resets its key
// and its client asks for the channel key, which reports pending and enrolls V.
// Whatever order the distribution handled V in, V must end with a key or a
// pending request: never with neither after being told pending.
func runResetDuringDistribution(t *testing.T, ts *testhelpers.TestServer, i int, seedRedundant bool) resetRaceResult {
	t.Helper()
	tag := fmt.Sprintf("%d%v", i, seedRedundant)
	owner := ts.CreateTestUser(t, "crown"+tag)
	serverID := ts.CreateTestServer(t, owner.ID, "cr server "+tag)
	channelID := createEncryptedChannel(t, ts, owner, serverID, "cr-"+tag)
	v := ts.CreateTestUser(t, "crv"+tag)
	w := ts.CreateTestUser(t, "crw"+tag)
	ts.AddMemberToServer(t, serverID, v.ID, roleMember)
	ts.AddMemberToServer(t, serverID, w.ID, roleMember)
	seedChannelKey(t, ts, channelID, v.ID, 1)
	setRecipientPublicKeyVersion(t, ts, w.ID, 3)
	if seedRedundant {
		seedPendingChannelRequest(t, ts, channelID, v.ID)
	}

	lockTx, err := ts.DB.Begin()
	require.NoError(t, err)
	defer func() { _ = lockTx.Rollback() }()
	_, err = lockTx.Exec(`SELECT 1 FROM public_keys WHERE user_id = $1 FOR UPDATE`, w.ID)
	require.NoError(t, err)

	var distW, getW *httptest.ResponseRecorder
	distDone, getDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(distDone)
		distW = distributePost(ts, owner.AccessToken, channelID,
			map[string]string{v.ID: testhelpers.ValidCiphertext(), w.ID: testhelpers.ValidCiphertext()},
			map[string]int{w.ID: 3})
	}()
	require.True(t, waitForLockWaiters(t, ts.DB, "%FROM public_keys%FOR SHARE%", 1, distDone), "the distribution never stalled on W")

	resetIdentityKey(t, ts.DB, v.ID)
	go func() {
		defer close(getDone)
		getW = ts.DoRequest("GET", pathE2EEKeys+channelID, nil, testhelpers.AuthHeaders(v.AccessToken))
	}()
	if seedRedundant {
		<-getDone // the existing row absorbs the enroll at once
	} else {
		// The enroll INSERT's foreign-key lock on the channel row queues behind
		// the distribution's FOR UPDATE.
		waitForLockWaiters(t, ts.DB, "%pending_key_requests%", 1, getDone)
	}
	require.NoError(t, lockTx.Commit())
	<-distDone
	<-getDone

	return resetRaceResult{
		distCode:   distW.Code,
		getCode:    getW.Code,
		getPending: getW.Code == http.StatusNotFound && strings.Contains(getW.Body.String(), `"pending":true`),
		keys:       rowCount(t, ts, `SELECT COUNT(*) FROM channel_keys WHERE channel_id = $1 AND user_id = $2`, channelID, v.ID),
		pending:    pendingChannelRows(t, ts, channelID, v.ID),
	}
}

// TestDistribution_CleanupKeepsNeededPendingRequest ports the cleanup-race
// proof. A cleanup that deletes by (channel, user) after commit removed a
// request V needed once V's key had been purged. Several runs cover both
// orders in which the old per-target loop could reach V.
func TestDistribution_CleanupKeepsNeededPendingRequest(t *testing.T) {
	ts := setupTS(t)
	for _, seeded := range []bool{true, false} {
		t.Run(fmt.Sprintf("key reset mid-distribution, seeded=%v", seeded), func(t *testing.T) {
			for i := 0; i < 6; i++ {
				r := runResetDuringDistribution(t, ts, i, seeded)
				t.Logf("run %d: dist=%d get=%d getPending=%v keys=%d pending=%d", i, r.distCode, r.getCode, r.getPending, r.keys, r.pending)
				require.Equal(t, http.StatusOK, r.distCode)
				require.True(t, r.getPending, "V's fetch after its reset reports pending")
				require.True(t, r.keys > 0 || r.pending > 0,
					"run %d: V was told pending but ends with no key and no pending request", i)
			}
		})
	}
	t.Run("key reset commits during the cleanup DELETE", func(t *testing.T) {
		resetDuringCleanup(t, ts)
	})
	t.Run("rotation commits around the distribution's cleanup", func(t *testing.T) {
		rotationDuringCleanup(t, ts)
	})
}

// resetDuringCleanup: the distribution finds V already holding the key, and its
// cleanup DELETE is held on V's pending-row lock after it has checked V's key.
// V's key reset then runs. If that check takes no lock, the reset commits, V's
// fetch reports pending and finds the row already there, and the cleanup then
// deletes it: V ends with no key and no request. With FOR SHARE on V's key row
// the reset waits for the distribution, and V's fetch after it enrolls V.
func resetDuringCleanup(t *testing.T, ts *testhelpers.TestServer) {
	owner := ts.CreateTestUser(t, "crdelowner")
	serverID := ts.CreateTestServer(t, owner.ID, "cr delete server")
	channelID := createEncryptedChannel(t, ts, owner, serverID, "cr-delete")
	v := ts.CreateTestUser(t, "crdelv")
	ts.AddMemberToServer(t, serverID, v.ID, roleMember)
	seedChannelKey(t, ts, channelID, v.ID, 1)
	seedPendingChannelRequest(t, ts, channelID, v.ID)

	lockTx, err := ts.DB.Begin()
	require.NoError(t, err)
	defer func() { _ = lockTx.Rollback() }()
	_, err = lockTx.Exec(`SELECT 1 FROM pending_key_requests WHERE channel_id = $1 AND user_id = $2 FOR UPDATE`, channelID, v.ID)
	require.NoError(t, err)

	var distW *httptest.ResponseRecorder
	distDone, resetDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(distDone)
		distW = distributePost(ts, owner.AccessToken, channelID, map[string]string{v.ID: testhelpers.ValidCiphertext()}, nil)
	}()
	require.True(t, waitForLockWaiters(t, ts.DB, "%DELETE FROM pending_key_requests%", 1, distDone), "the cleanup never waited on V's row")

	var resetErr error
	go func() {
		defer close(resetDone)
		resetErr = resetIdentityKeyTx(ts.DB, v.ID)
	}()
	fetch := func() *httptest.ResponseRecorder {
		return ts.DoRequest("GET", pathE2EEKeys+channelID, nil, testhelpers.AuthHeaders(v.AccessToken))
	}
	var getW *httptest.ResponseRecorder
	if waitForLockWaiters(t, ts.DB, "%DELETE FROM channel_keys%", 1, resetDone) {
		// The reset queues on V's key row until the distribution commits.
		require.NoError(t, lockTx.Rollback())
		<-distDone
		<-resetDone
		getW = fetch()
	} else {
		// The reset committed while the cleanup waited; V's client fetches now.
		<-resetDone
		getW = fetch()
		require.NoError(t, lockTx.Rollback())
		<-distDone
	}

	require.NoError(t, resetErr)
	require.Equal(t, http.StatusOK, distW.Code, distW.Body.String())
	require.Equal(t, http.StatusNotFound, getW.Code, getW.Body.String())
	require.Contains(t, getW.Body.String(), `"pending":true`)
	require.Equal(t, 0, rowCount(t, ts,
		`SELECT COUNT(*) FROM channel_keys WHERE channel_id = $1 AND user_id = $2`, channelID, v.ID),
		"precondition: the reset purged V's key")
	assert.Equal(t, 1, pendingChannelRows(t, ts, channelID, v.ID),
		"V was told pending after its reset, so its request must survive the cleanup")
}

// rotationDuringCleanup: a distribution at epoch 1 finds V already holding the
// key and cleans up V's redundant request. A rotation to epoch 2 enqueues V,
// who lacks the new key. Pending rows are per (channel, user), not per epoch,
// so a cleanup that runs after the rotation commits deletes the epoch-2
// request. The cleanup is held on V's row lock until the rotation has either
// committed or queued behind the distribution.
func rotationDuringCleanup(t *testing.T, ts *testhelpers.TestServer) {
	owner := ts.CreateTestUser(t, "crrotowner")
	serverID := ts.CreateTestServer(t, owner.ID, "cr rotation server")
	channelID := createEncryptedChannel(t, ts, owner, serverID, "cr-rotation")
	v := ts.CreateTestUser(t, "crrotv")
	ts.AddMemberToServer(t, serverID, v.ID, roleMember)
	seedChannelKey(t, ts, channelID, v.ID, 1)
	seedPendingChannelRequest(t, ts, channelID, v.ID)

	lockTx, err := ts.DB.Begin()
	require.NoError(t, err)
	defer func() { _ = lockTx.Rollback() }()
	_, err = lockTx.Exec(`SELECT 1 FROM pending_key_requests WHERE channel_id = $1 AND user_id = $2 FOR UPDATE`, channelID, v.ID)
	require.NoError(t, err)

	var distW, rotW *httptest.ResponseRecorder
	distDone, rotDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(distDone)
		distW = distributePost(ts, owner.AccessToken, channelID, map[string]string{v.ID: testhelpers.ValidCiphertext()}, nil)
	}()
	require.True(t, waitForLockWaiters(t, ts.DB, "%DELETE FROM pending_key_requests%", 1, distDone), "the cleanup never waited on V's row")

	var rotErr error
	go func() {
		defer close(rotDone)
		if _, rotErr = ts.DB.Exec(`
			INSERT INTO key_revocations (
				channel_id, revoked_epoch, successor_epoch, reason, revoked_by, rotation_distributor_claimed
			) VALUES ($1, 1, 2, 'member_removal', $2, FALSE)`, channelID, owner.ID); rotErr != nil {
			return
		}
		rotW = ts.DoRequest("POST", pathE2EEKeys+channelID, map[string]interface{}{
			keyWrappedKeys: map[string]string{owner.ID: testhelpers.ValidCiphertext()},
			"key_version":  2,
			keyFingerprint: rotationFingerprint,
		}, testhelpers.AuthHeaders(owner.AccessToken))
	}()
	// Old shape: the rotation commits while the cleanup waits. New shape: the
	// rotation queues on the channel lock the distribution still holds.
	waitForLockWaiters(t, ts.DB, "%", 2, rotDone)
	require.NoError(t, lockTx.Rollback())
	<-distDone
	<-rotDone

	require.NoError(t, rotErr)
	require.Equal(t, http.StatusOK, distW.Code, distW.Body.String())
	require.Equal(t, http.StatusOK, rotW.Code, rotW.Body.String())
	require.Equal(t, 0, rowCount(t, ts,
		`SELECT COUNT(*) FROM channel_keys WHERE channel_id = $1 AND user_id = $2 AND key_version = 2`, channelID, v.ID),
		"precondition: V lacks the epoch-2 key")
	assert.Equal(t, 1, pendingChannelRows(t, ts, channelID, v.ID),
		"V lacks the epoch-2 key, so its rotation request must survive the epoch-1 cleanup")
}

// sleepOnDistributionWrite makes every statement of kind op on table sleep for
// seconds, but only inside actorID's distribution transaction: the trigger
// keys on the distributor setting that transaction carries.
func sleepOnDistributionWrite(t *testing.T, db *sql.DB, name, op, table, actorID string, seconds int) {
	t.Helper()
	fn := pq.QuoteIdentifier("test_keydist_sleep_" + name)
	trigger := pq.QuoteIdentifier("test_keydist_sleep_" + name)
	_, err := db.Exec(fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $fn$
		BEGIN
			IF current_setting('concord.rotation_distributor_id', true) = %s THEN
				PERFORM pg_sleep(%d);
			END IF;
			RETURN NULL;
		END $fn$`, fn, pq.QuoteLiteral(actorID), seconds))
	require.NoError(t, err)
	_, err = db.Exec(fmt.Sprintf(`CREATE TRIGGER %s AFTER %s ON %s FOR EACH STATEMENT EXECUTE FUNCTION %s()`,
		trigger, op, pq.QuoteIdentifier(table), fn))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, dropErr := db.Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON %s`, trigger, pq.QuoteIdentifier(table)))
		assert.NoError(t, dropErr)
		_, dropErr = db.Exec(fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))
		assert.NoError(t, dropErr)
	})
}

// stallChannelPreCheck holds an ACCESS EXCLUSIVE lock on channels for 20 s, so
// the unified route's channel pre-check, a pool read with no lock_timeout,
// waits on it. Only the request deadline can end that wait before the server's
// write deadline. The lock is released when the test ends, or at 20 s.
func stallChannelPreCheck(t *testing.T, ts *testhelpers.TestServer, _ string) {
	t.Helper()
	locker, err := ts.DB.Begin()
	require.NoError(t, err)
	_, err = locker.Exec(`LOCK TABLE channels IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	var once sync.Once
	release := func() { once.Do(func() { assert.NoError(t, locker.Rollback()) }) }
	timer := time.AfterFunc(20*time.Second, release)
	t.Cleanup(func() {
		timer.Stop()
		release()
	})
}

// postOverServerWithWriteTimeout sends one distribution through a real HTTP
// server whose WriteTimeout matches cmd/server/main.go. A handler that answers
// after the write deadline leaves the client with a dropped connection, not a
// status, so a returned status proves the response was written in time.
func postOverServerWithWriteTimeout(t *testing.T, ts *testhelpers.TestServer, token, channelID string, wrapped map[string]string) (int, time.Duration) {
	t.Helper()
	srv := httptest.NewUnstartedServer(ts.Router)
	srv.Config.WriteTimeout = 15 * time.Second
	srv.Start()
	defer srv.Close()

	payload, err := json.Marshal(map[string]interface{}{keyWrappedKeys: wrapped})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, srv.URL+pathE2EEKeys+channelID, bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header = testhelpers.AuthHeaders(token)
	client := &http.Client{Timeout: 30 * time.Second}
	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	require.NoError(t, err, "the client must get a response, not a dropped connection (after %v)", elapsed)
	defer func() { _ = resp.Body.Close() }()
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err, "the response body must arrive whole (after %v)", elapsed)
	return resp.StatusCode, elapsed
}

// TestDistribution_OverrunAnswers503BeforeWriteTimeout: a distribution that
// overruns its statement bound or its overall deadline rolls back and answers
// 503 while the server can still write it. A statement bound at or above the
// 15 s write deadline lets the server drop the connection instead.
func TestDistribution_OverrunAnswers503BeforeWriteTimeout(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, ts *testhelpers.TestServer, actorID string)
	}{
		{"one statement overruns statement_timeout", func(t *testing.T, ts *testhelpers.TestServer, actorID string) {
			sleepOnDistributionWrite(t, ts.DB, "stmt", "INSERT", "channel_keys", actorID, 11)
		}},
		{"statements overrun the distribution deadline", func(t *testing.T, ts *testhelpers.TestServer, actorID string) {
			sleepOnDistributionWrite(t, ts.DB, "deadline_ins", "INSERT", "channel_keys", actorID, 7)
			sleepOnDistributionWrite(t, ts.DB, "deadline_del", "DELETE", "pending_key_requests", actorID, 7)
		}},
		{"the unified route's channel pre-check overruns the deadline", stallChannelPreCheck},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, owner, serverID, channelID := setupEncryptedChannel(t)
			m1 := ts.CreateTestUser(t, "overrun1")
			m2 := ts.CreateTestUser(t, "overrun2")
			ts.AddMemberToServer(t, serverID, m1.ID, roleMember)
			ts.AddMemberToServer(t, serverID, m2.ID, roleMember)
			tc.setup(t, ts, owner.ID)

			code, elapsed := postOverServerWithWriteTimeout(t, ts, owner.AccessToken, channelID,
				map[string]string{m1.ID: testhelpers.ValidCiphertext(), m2.ID: testhelpers.ValidCiphertext()})
			t.Logf("status %d after %v", code, elapsed.Round(time.Millisecond))
			assert.Equal(t, http.StatusServiceUnavailable, code)
			assert.Less(t, elapsed, 15*time.Second)
			assert.Equal(t, 0, rowCount(t, ts,
				`SELECT COUNT(*) FROM channel_keys WHERE channel_id = $1 AND user_id IN ($2, $3)`, channelID, m1.ID, m2.ID),
				"a distribution that overran writes no key")
		})
	}
}
