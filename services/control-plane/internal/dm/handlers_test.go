package dm_test

import (
	"net/http"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTS(t *testing.T) *testhelpers.TestServer {
	t.Helper()
	return testhelpers.SetupTestServer(t)
}

// --- RotateKey Tests (Security-Critical: E2EE forward secrecy) ---

func TestRotateKey_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "rotator1")
	user2 := ts.CreateTestUser(t, "rotator2")
	ts.CreateFriendship(t, user1.ID, user2.ID, "accepted")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)

	w := ts.DoRequest("POST", "/api/v1/dm/conversations/"+convID+"/rotate-key", nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(2), body["new_key_version"])
}

func TestRotateKey_RecordsRevocation(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "rotator3")
	user2 := ts.CreateTestUser(t, "rotator4")
	ts.CreateFriendship(t, user1.ID, user2.ID, "accepted")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)

	w := ts.DoRequest("POST", "/api/v1/dm/conversations/"+convID+"/rotate-key", nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

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
	assert.Equal(t, "manual_rotation", reason)
	assert.Equal(t, user1.ID, revokedBy)
}

func TestRotateKey_NoExistingKeys(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "rotator5")
	user2 := ts.CreateTestUser(t, "rotator6")
	ts.CreateFriendship(t, user1.ID, user2.ID, "accepted")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)
	// No keys seeded — maxVersion = 0

	w := ts.DoRequest("POST", "/api/v1/dm/conversations/"+convID+"/rotate-key", nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(1), body["new_key_version"])

	// No revocation should be recorded (guard: if maxVersion > 0)
	var count int
	err := ts.DB.QueryRow(`SELECT COUNT(*) FROM dm_key_revocations WHERE conversation_id = $1`, convID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "no revocation should be recorded when maxVersion=0")
}

func TestRotateKey_NotParticipant(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "rotator7")
	user2 := ts.CreateTestUser(t, "rotator8")
	outsider := ts.CreateTestUser(t, "outsider1")
	ts.CreateFriendship(t, user1.ID, user2.ID, "accepted")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)

	w := ts.DoRequest("POST", "/api/v1/dm/conversations/"+convID+"/rotate-key", nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestRotateKey_InvalidConversationID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "rotator9")

	w := ts.DoRequest("POST", "/api/v1/dm/conversations/not-a-uuid/rotate-key", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRotateKey_MultipleRotations(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "multirot1")
	user2 := ts.CreateTestUser(t, "multirot2")
	ts.CreateFriendship(t, user1.ID, user2.ID, "accepted")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)
	ts.SeedDMKey(t, convID, user1.ID, 1)

	// First rotation
	w := ts.DoRequest("POST", "/api/v1/dm/conversations/"+convID+"/rotate-key", nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Seed the new key version so second rotation has something to revoke
	ts.SeedDMKey(t, convID, user1.ID, 2)

	// Second rotation
	w = ts.DoRequest("POST", "/api/v1/dm/conversations/"+convID+"/rotate-key", nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(3), body["new_key_version"])

	// Should have 2 revocation records
	var count int
	err := ts.DB.QueryRow(`SELECT COUNT(*) FROM dm_key_revocations WHERE conversation_id = $1`, convID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
}

// --- ListConversations Tests ---

func TestListConversations_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "listdm1")
	user2 := ts.CreateTestUser(t, "listdm2")
	ts.CreateFriendship(t, user1.ID, user2.ID, "accepted")
	ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	w := ts.DoRequest("GET", "/api/v1/dm/conversations", nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conversations := body["conversations"].([]interface{})
	assert.GreaterOrEqual(t, len(conversations), 1)
}

func TestListConversations_Empty(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "lonelydm")

	w := ts.DoRequest("GET", "/api/v1/dm/conversations", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conversations := body["conversations"].([]interface{})
	assert.Len(t, conversations, 0)
}

// --- GetConversation Tests ---

func TestGetConversation_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getdm1")
	user2 := ts.CreateTestUser(t, "getdm2")
	ts.CreateFriendship(t, user1.ID, user2.ID, "accepted")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	w := ts.DoRequest("GET", "/api/v1/dm/conversations/"+convID, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conv := body["conversation"].(map[string]interface{})
	assert.Equal(t, convID, conv["id"])
	participants := conv["participants"].([]interface{})
	assert.Len(t, participants, 2)
}

func TestGetConversation_NotParticipant(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getdm3")
	user2 := ts.CreateTestUser(t, "getdm4")
	outsider := ts.CreateTestUser(t, "getdm5")
	ts.CreateFriendship(t, user1.ID, user2.ID, "accepted")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	w := ts.DoRequest("GET", "/api/v1/dm/conversations/"+convID, nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// --- OpenConversation Tests ---

func TestOpenConversation_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "opendm1")
	user2 := ts.CreateTestUser(t, "opendm2")
	ts.CreateFriendship(t, user1.ID, user2.ID, "accepted")

	w := ts.DoRequest("POST", "/api/v1/dm/conversations", map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))

	// Should be 200 or 201
	assert.True(t, w.Code == http.StatusOK || w.Code == http.StatusCreated,
		"expected 200 or 201, got %d", w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conv := body["conversation"].(map[string]interface{})
	assert.NotEmpty(t, conv["id"])
}

func TestOpenConversation_Idempotent(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "opendm3")
	user2 := ts.CreateTestUser(t, "opendm4")
	ts.CreateFriendship(t, user1.ID, user2.ID, "accepted")

	// Open twice — should return same conversation
	w1 := ts.DoRequest("POST", "/api/v1/dm/conversations", map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	require.True(t, w1.Code == http.StatusOK || w1.Code == http.StatusCreated)

	var body1 map[string]interface{}
	testhelpers.ParseJSON(t, w1, &body1)
	conv1 := body1["conversation"].(map[string]interface{})

	w2 := ts.DoRequest("POST", "/api/v1/dm/conversations", map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	require.True(t, w2.Code == http.StatusOK || w2.Code == http.StatusCreated)

	var body2 map[string]interface{}
	testhelpers.ParseJSON(t, w2, &body2)
	conv2 := body2["conversation"].(map[string]interface{})

	assert.Equal(t, conv1["id"], conv2["id"], "same conversation should be returned")
}
