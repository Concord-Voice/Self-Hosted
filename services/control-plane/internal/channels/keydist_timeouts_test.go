package channels_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// VULN-079 defense in depth: a distribution waiting on a lock another
// transaction holds gives up at lock_timeout (5s) with a retryable 503 and
// writes nothing, instead of pinning a pool connection indefinitely.

// distributeWhileLocked holds lockSQL's row lock from another connection while
// post runs. The wait is bounded: a distribution with no lock_timeout would
// otherwise block until the lock is released, so the guard releases it, joins
// the request, and fails.
func distributeWhileLocked(t *testing.T, ts *testhelpers.TestServer, lockSQL, lockID string, post func() *httptest.ResponseRecorder) (*httptest.ResponseRecorder, time.Duration) {
	t.Helper()
	ctx := context.Background()
	locker, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	released := false
	release := func() {
		if !released {
			released = true
			assert.NoError(t, locker.Rollback())
		}
	}
	defer release()
	var locked string
	require.NoError(t, locker.QueryRowContext(ctx, lockSQL, lockID).Scan(&locked))

	done := make(chan *httptest.ResponseRecorder, 1)
	start := time.Now()
	go func() { done <- post() }()
	select {
	case w := <-done:
		return w, time.Since(start)
	case <-time.After(10 * time.Second):
		release()
		w := <-done
		t.Fatalf("distribution did not give up on the held lock within 10s (status after release %d)", w.Code)
		return nil, 0
	}
}

func TestDistribution_LockTimeoutReturns503(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	member := ts.CreateTestUser(t, "locktimeoutchan")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	w, elapsed := distributeWhileLocked(t, ts, `SELECT id FROM channels WHERE id = $1 FOR UPDATE`, channelID,
		func() *httptest.ResponseRecorder {
			return distributePost(ts, owner.AccessToken, channelID,
				map[string]string{member.ID: testhelpers.ValidCiphertext()}, nil)
		})

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "Failed to distribute keys")
	assert.GreaterOrEqual(t, elapsed, 4*time.Second, "the request ended on the lock wait, not earlier")
	assert.Equal(t, 0, rowCount(t, ts,
		`SELECT COUNT(*) FROM channel_keys WHERE channel_id = $1 AND user_id = $2`, channelID, member.ID),
		"a timed-out distribution writes no key")
}

func TestDistribution_DMLockTimeoutReturns503(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "locktimeoutdm1")
	user2 := ts.CreateTestUser(t, "locktimeoutdm2")
	ts.CreateFriendship(t, user1.ID, user2.ID, "accepted")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w, elapsed := distributeWhileLocked(t, ts, `SELECT id FROM dm_conversations WHERE id = $1 FOR UPDATE`, convID,
		func() *httptest.ResponseRecorder {
			return distributePost(ts, user1.AccessToken, convID,
				map[string]string{user2.ID: testhelpers.ValidCiphertext()}, nil)
		})

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "Failed to distribute keys")
	assert.GreaterOrEqual(t, elapsed, 4*time.Second, "the request ended on the lock wait, not earlier")
	assert.Equal(t, 0, rowCount(t, ts,
		`SELECT COUNT(*) FROM dm_channel_keys WHERE conversation_id = $1`, convID),
		"a timed-out distribution writes no key")
}

func TestRotateDMKey_LockTimeoutReturns503(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "locktimeoutrot1")
	user2 := ts.CreateTestUser(t, "locktimeoutrot2")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)

	w, elapsed := distributeWhileLocked(t, ts, `SELECT id FROM dm_conversations WHERE id = $1 FOR UPDATE`, convID,
		func() *httptest.ResponseRecorder {
			return ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convID+pathRotateKey,
				dmRotationBody(2, user1.ID, user2.ID), testhelpers.AuthHeaders(user1.AccessToken))
		})

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "Failed to rotate key")
	assert.GreaterOrEqual(t, elapsed, 4*time.Second, "the request ended on the lock wait, not earlier")
	assert.Equal(t, []int{1}, dmKeyVersions(t, ts, convID, user1.ID), "no successor wrap is written")
	assert.Equal(t, []int{1}, dmKeyVersions(t, ts, convID, user2.ID), "no successor wrap is written")
	assert.Equal(t, 0, dmRevocationCount(t, ts, convID), "no rotation is recorded")
}
