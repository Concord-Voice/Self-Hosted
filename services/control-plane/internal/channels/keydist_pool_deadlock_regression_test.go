package channels_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	dbtest "github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/testdb"
)

// VULN-079 regression: every key-distribution transaction holds one pooled
// connection, so a per-target access check that asks the pool for a second one
// deadlocks the pool once enough distributions run at once. The check now runs
// on the distribution transaction, so a small pool stays live.

// deadlockRequestLimit is a hang guard, not a latency budget: a deadlocked
// pool never finishes, so a generous limit separates the two on a slow runner.
const (
	deadlockPoolSize     = 4
	deadlockConcurrent   = 8
	deadlockTargets      = 1000
	deadlockRequestLimit = 60 * time.Second
)

// strangerTargets returns n well-formed user IDs that belong to no server. The
// per-target access check still runs for each (it is the call that used to
// take a second pool connection) but delivers nothing, so the test measures
// pool behaviour and not insert throughput.
func strangerTargets(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = uuid.NewString()
	}
	return ids
}

// distributor is one distribution's actor: its own owner (the route rate-limits
// per user) with a server and an encrypted channel whose key they hold.
type distributor struct {
	owner     testhelpers.TestUser
	channelID string
}

func newDistributor(t *testing.T, ts *testhelpers.TestServer, name string) distributor {
	t.Helper()
	owner := ts.CreateTestUser(t, name)
	serverID := ts.CreateTestServer(t, owner.ID, name+" server")
	w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
		keyServerID:    serverID,
		"name":         name + "-channel",
		"type":         "text",
		keyWrappedKeys: map[string]string{owner.ID: testhelpers.ValidCiphertext()},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	channel, ok := body[keyChannel].(map[string]interface{})
	require.True(t, ok)
	id, ok := channel["id"].(string)
	require.True(t, ok)
	return distributor{owner: owner, channelID: id}
}

func wrapAll(ids []string) map[string]string {
	wrapped := make(map[string]string, len(ids))
	for _, id := range ids {
		wrapped[id] = testhelpers.ValidCiphertext()
	}
	return wrapped
}

// distribute sends targets to the distributor's channel in several requests and
// returns every response. The route accepts at most 500 wrapped keys per
// request, so a 1,000-target distribution is always several requests.
func (d distributor) distribute(ts *testhelpers.TestServer, targets []string) []*httptest.ResponseRecorder {
	const batch = 250
	var out []*httptest.ResponseRecorder
	for start := 0; start < len(targets); start += batch {
		end := min(start+batch, len(targets))
		out = append(out, distributePost(ts, d.owner.AccessToken, d.channelID, wrapAll(targets[start:end]), nil))
	}
	return out
}

func TestDistribution_SmallPoolStaysLiveUnderConcurrentDistributions(t *testing.T) {
	// The router gets its own pool. Do not resize the SetupTestDB pool: a
	// known testdb leak pins one of its connections.
	_, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	routerDB, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	routerDB.SetMaxOpenConns(deadlockPoolSize)
	t.Cleanup(func() { assert.NoError(t, routerDB.Close()) })

	ts := testhelpers.SetupTestServerWithRouterDB(t, routerDB)
	timed := newDistributor(t, ts, "deadlocktimed")
	distributors := make([]distributor, deadlockConcurrent)
	for i := range distributors {
		distributors[i] = newDistributor(t, ts, fmt.Sprintf("deadlock%d", i))
	}
	targets := strangerTargets(deadlockTargets)

	t.Run("one distribution is timed", func(t *testing.T) {
		start := time.Now()
		for _, w := range timed.distribute(ts, targets) {
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		}
		elapsed := time.Since(start)
		t.Logf("one %d-target distribution took %v", deadlockTargets, elapsed.Round(time.Millisecond))
		assert.Less(t, elapsed, deadlockRequestLimit)
	})

	t.Run("concurrent distributions finish and an unrelated query is served", func(t *testing.T) {
		results := make([][]*httptest.ResponseRecorder, deadlockConcurrent)
		elapsed := make([]time.Duration, deadlockConcurrent)
		var wg sync.WaitGroup
		for i := range distributors {
			wg.Add(1)
			go func() {
				defer wg.Done()
				start := time.Now()
				results[i] = distributors[i].distribute(ts, targets)
				elapsed[i] = time.Since(start)
			}()
		}

		finished := make(chan struct{})
		go func() { wg.Wait(); close(finished) }()

		// Wait until pg_stat_activity shows the burst holding connections. A
		// fast burst can finish first, which is not a deadlock either.
		burstSeen := waitForBusyBackends(t, ts.DB, 2, finished)

		// An unrelated query on the same small pool must be served while the
		// burst is still running. The deadline only guards against a hang.
		probeCtx, cancel := context.WithTimeout(context.Background(), deadlockRequestLimit)
		defer cancel()
		var one int
		probeErr := routerDB.QueryRowContext(probeCtx, `SELECT 1`).Scan(&one)
		select {
		case <-finished:
			t.Logf("burst observed=%v; it finished before the unrelated query returned", burstSeen)
		default:
			t.Logf("burst observed=%v; the unrelated query was served while it was still running", burstSeen)
		}
		require.NoError(t, probeErr, "an unrelated query must not starve behind the burst")
		assert.Equal(t, 1, one)

		select {
		case <-finished:
		case <-time.After(deadlockRequestLimit):
			t.Fatalf("distributions did not finish within %v: the %d-connection pool is deadlocked", deadlockRequestLimit, deadlockPoolSize)
		}

		ok := 0
		for i, batches := range results {
			require.NotEmpty(t, batches, "distribution %d", i)
			for _, w := range batches {
				assert.Contains(t, []int{http.StatusOK, http.StatusServiceUnavailable}, w.Code,
					"distribution %d: %s", i, w.Body.String())
				if w.Code == http.StatusOK {
					ok++
				}
			}
			assert.Less(t, elapsed[i], deadlockRequestLimit, "distribution %d", i)
		}
		require.Positive(t, ok, "at least one distribution must complete: a burst that only times out is not live")
	})
}

// waitForBusyBackends polls pg_stat_activity until at least n other client
// sessions in this database are not idle, or done closes, or the request limit
// passes. It reports whether the sessions were seen.
func waitForBusyBackends(t *testing.T, db *sql.DB, n int, done <-chan struct{}) bool {
	t.Helper()
	deadline := time.Now().Add(deadlockRequestLimit)
	for time.Now().Before(deadline) {
		var busy int
		require.NoError(t, db.QueryRow(`
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND backend_type = 'client backend'
			  AND state <> 'idle' AND pid <> pg_backend_pid()`).Scan(&busy))
		if busy >= n {
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
