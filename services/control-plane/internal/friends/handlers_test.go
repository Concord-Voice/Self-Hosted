package friends_test

import (
	"net/http"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pathFriendsPrefix = "/api/v1/friends/"
	pathFriends       = "/api/v1/friends"
	pathFriendRequest = "/api/v1/friends/request"
	pathBlock         = "/block"
	statusAccepted    = "accepted"
)

func setupTS(t *testing.T) *testhelpers.TestServer {
	t.Helper()
	return testhelpers.SetupTestServer(t)
}

// --- BlockUser Tests (Security-Critical: E2EE key revocation) ---

func TestBlockUser_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "blocker")
	user2 := ts.CreateTestUser(t, "blocked")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)

	w := ts.DoRequest("POST", pathFriendsPrefix+user2.ID+pathBlock, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify friendship status changed to blocked
	var status string
	err := ts.DB.QueryRow(
		`SELECT status FROM friendships WHERE (requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1)`,
		user1.ID, user2.ID,
	).Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, "blocked", status)
}

func TestBlockUser_RecordsRevocation(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "blocker2")
	user2 := ts.CreateTestUser(t, "blocked2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)

	w := ts.DoRequest("POST", pathFriendsPrefix+user2.ID+pathBlock, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify revocation record
	var reason, revokedBy string
	var revokedEpoch, successorEpoch int
	err := ts.DB.QueryRow(
		`SELECT revoked_epoch, successor_epoch, reason, revoked_by FROM dm_key_revocations WHERE conversation_id = $1`,
		convID,
	).Scan(&revokedEpoch, &successorEpoch, &reason, &revokedBy)
	require.NoError(t, err)
	assert.Equal(t, 1, revokedEpoch)
	assert.Equal(t, 2, successorEpoch)
	assert.Equal(t, "user_blocked", reason)
	assert.Equal(t, user1.ID, revokedBy)
}

func TestBlockUser_DeletesBlockedUserKey(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "blocker3")
	user2 := ts.CreateTestUser(t, "blocked3")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)

	w := ts.DoRequest("POST", pathFriendsPrefix+user2.ID+pathBlock, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Blocked user's key at version 1 should be deleted
	var keyCount int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM dm_channel_keys WHERE conversation_id = $1 AND user_id = $2`,
		convID, user2.ID,
	).Scan(&keyCount)
	require.NoError(t, err)
	assert.Equal(t, 0, keyCount, "blocked user's key should be deleted")

	// Blocker's key should still exist
	err = ts.DB.QueryRow(
		`SELECT COUNT(*) FROM dm_channel_keys WHERE conversation_id = $1 AND user_id = $2`,
		convID, user1.ID,
	).Scan(&keyCount)
	require.NoError(t, err)
	assert.Equal(t, 1, keyCount, "blocker's key should remain")
}

func TestBlockUser_NoDMConversation(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "blocker4")
	user2 := ts.CreateTestUser(t, "blocked4")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)

	// No DM conversation — should still succeed
	w := ts.DoRequest("POST", pathFriendsPrefix+user2.ID+pathBlock, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var status string
	err := ts.DB.QueryRow(
		`SELECT status FROM friendships WHERE (requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1)`,
		user1.ID, user2.ID,
	).Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, "blocked", status)
}

func TestBlockUser_SelfBlock(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "selfblocker")

	w := ts.DoRequest("POST", pathFriendsPrefix+user.ID+pathBlock, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestBlockUser_InvalidUserID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "blocker5")

	w := ts.DoRequest("POST", "/api/v1/friends/not-a-uuid/block", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestBlockUser_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "blocker6")

	w := ts.DoRequest("POST", pathFriendsPrefix+user.ID+pathBlock, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestBlockUser_NoExistingFriendship(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "blocker7")
	user2 := ts.CreateTestUser(t, "blocked7")

	// No existing friendship — should create a blocked one
	w := ts.DoRequest("POST", pathFriendsPrefix+user2.ID+pathBlock, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var status string
	err := ts.DB.QueryRow(
		`SELECT status FROM friendships WHERE requester_id = $1 AND addressee_id = $2`,
		user1.ID, user2.ID,
	).Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, "blocked", status)
}

// --- ListFriends Tests ---

func TestListFriends_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "listuser1")
	user2 := ts.CreateTestUser(t, "listuser2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)

	w := ts.DoRequest("GET", pathFriends, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	friends := body["friends"].([]interface{})
	assert.Len(t, friends, 1)
	friend := friends[0].(map[string]interface{})
	assert.Equal(t, user2.ID, friend["user_id"])
}

func TestListFriends_Empty(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "lonelyuser")

	w := ts.DoRequest("GET", pathFriends, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	friends := body["friends"].([]interface{})
	assert.Len(t, friends, 0)
}

func TestListFriends_ExcludesBlocked(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "listblock1")
	user2 := ts.CreateTestUser(t, "listblock2")
	ts.CreateFriendship(t, user1.ID, user2.ID, "blocked")

	w := ts.DoRequest("GET", pathFriends, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	friends := body["friends"].([]interface{})
	assert.Len(t, friends, 0, "blocked users should not appear in friends list")
}

// --- SendRequest Tests ---

func TestSendRequest_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "sender1")
	user2 := ts.CreateTestUser(t, "receiver1")

	w := ts.DoRequest("POST", pathFriendRequest, map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "pending", body["status"])
	assert.NotEmpty(t, body["id"])
}

func TestSendRequest_ToSelf(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "selfsender")

	w := ts.DoRequest("POST", pathFriendRequest, map[string]interface{}{
		"user_id": user.ID,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSendRequest_AlreadyFriends(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "sender2")
	user2 := ts.CreateTestUser(t, "receiver2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)

	w := ts.DoRequest("POST", pathFriendRequest, map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestSendRequest_ByUsername(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "sender3")
	user2 := ts.CreateTestUser(t, "receiver3")

	w := ts.DoRequest("POST", pathFriendRequest, map[string]interface{}{
		"username": user2.Username,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)
}

// --- RespondRequest Tests ---

func TestRespondRequest_Accept(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "reqsender")
	user2 := ts.CreateTestUser(t, "reqreceiver")

	// Create pending request
	var requestID string
	err := ts.DB.QueryRow(
		`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'pending') RETURNING id`,
		user1.ID, user2.ID,
	).Scan(&requestID)
	require.NoError(t, err)

	w := ts.DoRequest("PATCH", "/api/v1/friends/request/"+requestID, map[string]interface{}{
		"action": "accept",
	}, testhelpers.AuthHeaders(user2.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify status changed
	var status string
	err = ts.DB.QueryRow(`SELECT status FROM friendships WHERE id = $1`, requestID).Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, "accepted", status)
}

func TestRespondRequest_Decline(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "declsender")
	user2 := ts.CreateTestUser(t, "declreceiver")

	var requestID string
	err := ts.DB.QueryRow(
		`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'pending') RETURNING id`,
		user1.ID, user2.ID,
	).Scan(&requestID)
	require.NoError(t, err)

	w := ts.DoRequest("PATCH", "/api/v1/friends/request/"+requestID, map[string]interface{}{
		"action": "decline",
	}, testhelpers.AuthHeaders(user2.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify friendship was deleted
	var count int
	err = ts.DB.QueryRow(`SELECT COUNT(*) FROM friendships WHERE id = $1`, requestID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

// --- RemoveFriend Tests ---

func TestRemoveFriend_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "remover1")
	user2 := ts.CreateTestUser(t, "removee1")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)

	w := ts.DoRequest("DELETE", pathFriendsPrefix+user2.ID, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var count int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM friendships WHERE (requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1)`,
		user1.ID, user2.ID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestRemoveFriend_NotFriends(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "remover2")
	user2 := ts.CreateTestUser(t, "removee2")

	w := ts.DoRequest("DELETE", pathFriendsPrefix+user2.ID, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// --- ListRequests Tests ---

func TestListRequests_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "reqlist1")
	user2 := ts.CreateTestUser(t, "reqlist2")

	_, err := ts.DB.Exec(
		`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'pending')`,
		user1.ID, user2.ID,
	)
	require.NoError(t, err)

	// Addressee sees the request
	w := ts.DoRequest("GET", "/api/v1/friends/requests", nil, testhelpers.AuthHeaders(user2.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	requests := body["requests"].([]interface{})
	assert.Len(t, requests, 1)
	req := requests[0].(map[string]interface{})
	assert.Equal(t, "received", req["direction"])
}

func TestListRequests_Empty(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "noreqs")

	w := ts.DoRequest("GET", "/api/v1/friends/requests", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	requests := body["requests"].([]interface{})
	assert.Len(t, requests, 0)
}
