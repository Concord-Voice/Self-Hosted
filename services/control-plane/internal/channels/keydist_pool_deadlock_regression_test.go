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

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// VULN-079 regression: every key-distribution transaction holds one pooled
// connection, so a per-target access check that asks the pool for a second one
// deadlocks the pool once enough distributions run at once. The check now runs
// on the distribution transaction, so a small pool stays live.

const (
	deadlockPoolSize     = 4
	deadlockConcurrent   = 8
	deadlockTargets      = 1000
	deadlockProbeBudget  = 2 * time.Second
	deadlockRequestLimit = 20 * time.Second
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
// request, so a 1,000-target distribution is always several requests. Batches
// of 250 keep each connection hold short enough that the 2s probe budget
// measures pool starvation and not the length of one request.
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

		// An unrelated query on the same small pool, arriving mid-burst.
		time.Sleep(200 * time.Millisecond)
		probeCtx, cancel := context.WithTimeout(context.Background(), deadlockProbeBudget)
		defer cancel()
		probeStart := time.Now()
		var one int
		probeErr := routerDB.QueryRowContext(probeCtx, `SELECT 1`).Scan(&one)
		probeElapsed := time.Since(probeStart)

		// A deadlocked pool never lets the requests finish (the test requests
		// carry no deadline), so bound the wait. The cleanup that closes the
		// pool then releases any request still waiting for a connection.
		finished := make(chan struct{})
		go func() { wg.Wait(); close(finished) }()
		select {
		case <-finished:
		case <-time.After(deadlockRequestLimit):
			t.Fatalf("distributions did not finish within %v: the %d-connection pool is deadlocked", deadlockRequestLimit, deadlockPoolSize)
		}
		require.NoError(t, probeErr, "an unrelated query must not starve (waited %v)", probeElapsed)
		assert.Equal(t, 1, one)
		assert.Less(t, probeElapsed, deadlockProbeBudget)

		for i, batches := range results {
			require.NotEmpty(t, batches, "distribution %d", i)
			for _, w := range batches {
				assert.Contains(t, []int{http.StatusOK, http.StatusServiceUnavailable}, w.Code,
					"distribution %d: %s", i, w.Body.String())
			}
			assert.Less(t, elapsed[i], deadlockRequestLimit, "distribution %d", i)
		}
	})
}
