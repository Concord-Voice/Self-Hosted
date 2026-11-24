package channels_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// VULN-080 / VULN-207: a stale wrapped_key_versions entry for a target that
// already holds the key at the batch's epoch-validated version must not write a
// pending key request, and a redundant pending request is deleted after commit.
// A target that lacks the key still gets the #2420 self-heal request.

func seedChannelKey(t *testing.T, ts *testhelpers.TestServer, channelID, userID string, version int) {
	t.Helper()
	_, err := ts.DB.Exec(`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version) VALUES ($1, $2, $3, $4)`,
		channelID, userID, testhelpers.ValidCiphertext(), version)
	require.NoError(t, err)
}

func seedPendingChannelRequest(t *testing.T, ts *testhelpers.TestServer, channelID, userID string) {
	t.Helper()
	_, err := ts.DB.Exec(`INSERT INTO pending_key_requests (channel_id, user_id) VALUES ($1, $2)`, channelID, userID)
	require.NoError(t, err)
}

func seedPendingDMRequest(t *testing.T, ts *testhelpers.TestServer, convID, userID string) {
	t.Helper()
	_, err := ts.DB.Exec(`INSERT INTO dm_pending_key_requests (conversation_id, user_id) VALUES ($1, $2)`, convID, userID)
	require.NoError(t, err)
}

func pendingChannelRows(t *testing.T, ts *testhelpers.TestServer, channelID, userID string) int {
	t.Helper()
	return rowCount(t, ts, `SELECT COUNT(*) FROM pending_key_requests WHERE channel_id = $1 AND user_id = $2`, channelID, userID)
}

func pendingDMRows(t *testing.T, ts *testhelpers.TestServer, convID, userID string) int {
	t.Helper()
	return rowCount(t, ts, `SELECT COUNT(*) FROM dm_pending_key_requests WHERE conversation_id = $1 AND user_id = $2`, convID, userID)
}

// distributionOutcomeCounts reads the channel route's duplicates and
// skipped_stale counters.
func distributionOutcomeCounts(t *testing.T, w *httptest.ResponseRecorder) (duplicates, skippedStale float64) {
	t.Helper()
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	return testhelpers.JSONField[float64](t, body, "duplicates"), testhelpers.JSONField[float64](t, body, "skipped_stale")
}

// channelWithMember is an encrypted channel at key version 1 plus one viewing
// member whose identity key is at publicKeyVersion.
func channelWithMember(t *testing.T, name string, publicKeyVersion int) (*testhelpers.TestServer, testhelpers.TestUser, string, string) {
	t.Helper()
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	member := ts.CreateTestUser(t, name)
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	setRecipientPublicKeyVersion(t, ts, member.ID, publicKeyVersion)
	return ts, owner, channelID, member.ID
}

// dmWithKeys is a two-person DM where both participants hold epoch 1 and
// user2's identity key is at version 6.
func dmWithKeys(t *testing.T, name string) (*testhelpers.TestServer, testhelpers.TestUser, string, string) {
	t.Helper()
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, name+"1")
	user2 := ts.CreateTestUser(t, name+"2")
	ts.CreateFriendship(t, user1.ID, user2.ID, "accepted")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)
	setRecipientPublicKeyVersion(t, ts, user2.ID, 6)
	return ts, user1, convID, user2.ID
}

func TestStaleSkip_TargetHoldsCurrentKey_NoPendingRow(t *testing.T) {
	ts, owner, channelID, member := channelWithMember(t, "heldstale", 6)
	seedChannelKey(t, ts, channelID, member, 1)

	w := distributePost(ts, owner.AccessToken, channelID,
		map[string]string{member: testhelpers.ValidCiphertext()}, map[string]int{member: 5})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	duplicates, skippedStale := distributionOutcomeCounts(t, w)
	assert.Equal(t, float64(1), duplicates, "a target holding the current key is a duplicate")
	assert.Equal(t, float64(0), skippedStale)
	assert.Equal(t, 0, pendingChannelRows(t, ts, channelID, member), "no redundant pending request is written")
}

func TestStaleSkip_TargetHoldsCurrentKey_NoPendingRow_DM(t *testing.T) {
	ts, user1, convID, user2 := dmWithKeys(t, "heldstaledm")

	w := distributePost(ts, user1.AccessToken, convID,
		map[string]string{user2: testhelpers.ValidCiphertext()}, map[string]int{user2: 5})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.Equal(t, 0, pendingDMRows(t, ts, convID, user2), "no redundant pending request is written")
	assert.Equal(t, []int{1}, dmKeyVersions(t, ts, convID, user2), "the held key is untouched")
}

// Positive control for the #2420 self-heal: a stale wrap for a target that
// does NOT hold the key still enqueues a pending request.
func TestStaleSkip_TargetLacksKey_EnqueuesPendingRow(t *testing.T) {
	ts, owner, channelID, member := channelWithMember(t, "lackstale", 6)

	w := distributePost(ts, owner.AccessToken, channelID,
		map[string]string{member: testhelpers.ValidCiphertext()}, map[string]int{member: 5})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	duplicates, skippedStale := distributionOutcomeCounts(t, w)
	assert.Equal(t, float64(0), duplicates)
	assert.Equal(t, float64(1), skippedStale)
	assert.Equal(t, 1, pendingChannelRows(t, ts, channelID, member), "the self-heal request is written")
}

func TestDuplicate_PendingRowDeletedAfterCommit(t *testing.T) {
	t.Run("channel target that already holds the key", func(t *testing.T) {
		ts, owner, channelID, member := channelWithMember(t, "dupclean", 5)
		seedChannelKey(t, ts, channelID, member, 1)
		seedPendingChannelRequest(t, ts, channelID, member)

		w := distributePost(ts, owner.AccessToken, channelID,
			map[string]string{member: testhelpers.ValidCiphertext()}, nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		duplicates, _ := distributionOutcomeCounts(t, w)
		require.Equal(t, float64(1), duplicates, "precondition: the outcome is a duplicate")
		assert.Equal(t, 0, pendingChannelRows(t, ts, channelID, member), "the redundant request is deleted")
	})
	t.Run("channel stale wrap for a holder", func(t *testing.T) {
		ts, owner, channelID, member := channelWithMember(t, "dupcleanstale", 6)
		seedChannelKey(t, ts, channelID, member, 1)
		seedPendingChannelRequest(t, ts, channelID, member)

		w := distributePost(ts, owner.AccessToken, channelID,
			map[string]string{member: testhelpers.ValidCiphertext()}, map[string]int{member: 5})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, 0, pendingChannelRows(t, ts, channelID, member), "the redundant request is deleted")
	})
	t.Run("dm recipient that already holds the epoch", func(t *testing.T) {
		ts, user1, convID, user2 := dmWithKeys(t, "dupcleandm")
		seedPendingDMRequest(t, ts, convID, user2)

		w := distributePost(ts, user1.AccessToken, convID,
			map[string]string{user2: testhelpers.ValidCiphertext()}, map[string]int{user2: 5})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, 0, pendingDMRows(t, ts, convID, user2), "the redundant request is deleted")
	})
}

// A key reset purges the target's wrapped keys and rotates its identity key.
// A distribution wrapped against the old identity key must then keep the
// pending request, until a fresh wrap lands and fulfils it.
func TestStaleSkip_AfterReset_KeepsPendingRow(t *testing.T) {
	ts, owner, channelID, member := channelWithMember(t, "afterreset", 5)
	seedChannelKey(t, ts, channelID, member, 1)
	// ReplaceMyKeys-equivalent: purge the wrapped key, bump the identity key.
	_, err := ts.DB.Exec(`DELETE FROM channel_keys WHERE user_id = $1`, member)
	require.NoError(t, err)
	setRecipientPublicKeyVersion(t, ts, member, 6)

	w := distributePost(ts, owner.AccessToken, channelID,
		map[string]string{member: testhelpers.ValidCiphertext()}, map[string]int{member: 5})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	duplicates, skippedStale := distributionOutcomeCounts(t, w)
	assert.Equal(t, float64(0), duplicates, "a reset target does not hold the key")
	assert.Equal(t, float64(1), skippedStale)
	assert.Equal(t, 1, pendingChannelRows(t, ts, channelID, member), "the pending request survives the stale distribution")

	w = distributePost(ts, owner.AccessToken, channelID,
		map[string]string{member: testhelpers.ValidCiphertext()}, map[string]int{member: 6})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 0, pendingChannelRows(t, ts, channelID, member), "a fresh wrap fulfils and removes it")
}

// The epoch guard must run before any target is processed: a refused batch
// writes neither a key nor a pending request, even for a stale target.
func TestEpochGuardPrecedesTargets(t *testing.T) {
	t.Run("channel: an unissued version is refused first", func(t *testing.T) {
		ts, owner, channelID, member := channelWithMember(t, "epochfirst", 6)
		w := ts.DoRequest(http.MethodPost, pathE2EEKeys+channelID, map[string]interface{}{
			"wrapped_keys":         map[string]string{member: testhelpers.ValidCiphertext()},
			jsonWrappedKeyVersions: map[string]int{member: 5},
			"key_version":          5,
		}, testhelpers.AuthHeaders(owner.AccessToken))
		require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), "Key rotation has not been initiated")
		assert.Equal(t, 0, pendingChannelRows(t, ts, channelID, member), "no target helper ran")
		assert.Equal(t, 0, rowCount(t, ts, `SELECT COUNT(*) FROM channel_keys WHERE channel_id = $1 AND user_id = $2`, channelID, member))
	})
	t.Run("dm: a version below the current epoch is refused first", func(t *testing.T) {
		ts, user1, convID, user2 := dmWithKeys(t, "epochfirstdm")
		ts.SeedDMKey(t, convID, user1.ID, 2)
		w := ts.DoRequest(http.MethodPost, pathE2EEKeys+convID, map[string]interface{}{
			"wrapped_keys":         map[string]string{user2: testhelpers.ValidCiphertext()},
			jsonWrappedKeyVersions: map[string]int{user2: 5},
			"key_version":          1,
		}, testhelpers.AuthHeaders(user1.AccessToken))
		require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), "current_version")
		assert.Equal(t, 0, pendingDMRows(t, ts, convID, user2), "no target helper ran")
		assert.Equal(t, []int{1}, dmKeyVersions(t, ts, convID, user2))
	})
}
