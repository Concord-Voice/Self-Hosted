package dm_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const mimeImagePNG = "image/png"

const (
	pathDMConversationsPrefix = "/api/v1/dm/conversations/"
	pathDMConversations       = "/api/v1/dm/conversations"
	pathRotateKey             = "/rotate-key"
	statusAccepted            = "accepted"

	notAUUID       = "not-a-uuid"
	fmtExpected2xx = "expected 200 or 201, got %d"
	pathGroup      = "/group"
	pathMessages   = "/messages"
	wrappedKey     = "wrapped-key"
	pathVoiceJoin  = "/voice/join"
	pathVoiceParts = "/voice/participants"
	pathPersonal   = "/personal"
	pathMsgSlash   = "/messages/"
)

func setupTS(t *testing.T) *testhelpers.TestServer {
	t.Helper()
	return testhelpers.SetupTestServer(t)
}

// insertDMMessage inserts a DM message directly into the database and returns its ID.
func insertDMMessage(t *testing.T, ts *testhelpers.TestServer, convID, userID, content string, isEncrypted bool) string {
	t.Helper()
	msgID := uuid.New().String()
	_, err := ts.DB.Exec(
		`INSERT INTO dm_messages (id, conversation_id, user_id, content, is_encrypted, type) VALUES ($1, $2, $3, $4, $5, 'text')`,
		msgID, convID, userID, content, isEncrypted,
	)
	require.NoError(t, err, "failed to insert DM message")
	return msgID
}

// setDMPrivacy inserts or updates a user's DM privacy settings.
func setDMPrivacy(t *testing.T, ts *testhelpers.TestServer, userID string, level int, fof bool) {
	t.Helper()
	_, err := ts.DB.Exec(`
		INSERT INTO privacy_settings (user_id, dm_privacy_level, dm_friends_of_friends)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id) DO UPDATE SET dm_privacy_level = $2, dm_friends_of_friends = $3
	`, userID, level, fof)
	require.NoError(t, err, "failed to set DM privacy")
}

// ============================================================================
// RotateKey Tests (Security-Critical: E2EE forward secrecy)
// ============================================================================

func TestRotateKey_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "rotator1")
	user2 := ts.CreateTestUser(t, "rotator2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathRotateKey, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(2), body["new_key_version"])
}

func TestRotateKey_RecordsRevocation(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "rotator3")
	user2 := ts.CreateTestUser(t, "rotator4")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathRotateKey, nil, testhelpers.AuthHeaders(user1.AccessToken))
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
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)
	// No keys seeded — maxVersion = 0

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathRotateKey, nil, testhelpers.AuthHeaders(user1.AccessToken))
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
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathRotateKey, nil, testhelpers.AuthHeaders(outsider.AccessToken))
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
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)
	ts.SeedDMKey(t, convID, user1.ID, 1)

	// First rotation
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathRotateKey, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Seed the new key version so second rotation has something to revoke
	ts.SeedDMKey(t, convID, user1.ID, 2)

	// Second rotation
	w = ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathRotateKey, nil, testhelpers.AuthHeaders(user1.AccessToken))
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

func TestRotateKey_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	convID := uuid.New().String()

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathRotateKey, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ============================================================================
// ListConversations Tests
// ============================================================================

func TestListConversations_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "listdm1")
	user2 := ts.CreateTestUser(t, "listdm2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	w := ts.DoRequest("GET", pathDMConversations, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conversations := body["conversations"].([]interface{})
	assert.GreaterOrEqual(t, len(conversations), 1)
}

func TestListConversations_Empty(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "lonelydm")

	w := ts.DoRequest("GET", pathDMConversations, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conversations := body["conversations"].([]interface{})
	assert.Len(t, conversations, 0)
}

func TestListConversations_IncludesParticipants(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "listpart1")
	user2 := ts.CreateTestUser(t, "listpart2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	w := ts.DoRequest("GET", pathDMConversations, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conversations := body["conversations"].([]interface{})
	require.Len(t, conversations, 1)

	conv := conversations[0].(map[string]interface{})
	participants := conv["participants"].([]interface{})
	assert.Len(t, participants, 2)
}

func TestListConversations_WithLastMessage(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "listlm1")
	user2 := ts.CreateTestUser(t, "listlm2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	insertDMMessage(t, ts, convID, user1.ID, "hello there", false)

	w := ts.DoRequest("GET", pathDMConversations, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conversations := body["conversations"].([]interface{})
	require.Len(t, conversations, 1)

	conv := conversations[0].(map[string]interface{})
	lm := conv["last_message"].(map[string]interface{})
	assert.Equal(t, "hello there", lm["content"])
	assert.Equal(t, user1.ID, lm["user_id"])
}

func TestListConversations_UnreadCount(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "unread1")
	user2 := ts.CreateTestUser(t, "unread2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	// user2 sends 3 messages (unread for user1)
	insertDMMessage(t, ts, convID, user2.ID, "msg1", false)
	insertDMMessage(t, ts, convID, user2.ID, "msg2", false)
	insertDMMessage(t, ts, convID, user2.ID, "msg3", false)

	w := ts.DoRequest("GET", pathDMConversations, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conversations := body["conversations"].([]interface{})
	require.Len(t, conversations, 1)
	conv := conversations[0].(map[string]interface{})
	assert.Equal(t, float64(3), conv["unread_count"])
}

func TestListConversations_MultipleConversations(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "listmulti1")
	user2 := ts.CreateTestUser(t, "listmulti2")
	user3 := ts.CreateTestUser(t, "listmulti3")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	ts.CreateFriendship(t, user1.ID, user3.ID, statusAccepted)
	ts.CreateDMConversation(t, user1.ID, user2.ID, false)
	ts.CreateDMConversation(t, user1.ID, user3.ID, false)

	w := ts.DoRequest("GET", pathDMConversations, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conversations := body["conversations"].([]interface{})
	assert.Len(t, conversations, 2)
}

func TestListConversations_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	_ = ts // ensure server is up

	w := ts.DoRequest("GET", pathDMConversations, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ============================================================================
// GetConversation Tests
// ============================================================================

func TestGetConversation_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getdm1")
	user2 := ts.CreateTestUser(t, "getdm2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID, nil, testhelpers.AuthHeaders(user1.AccessToken))
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
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID, nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestGetConversation_InvalidID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "getdminv")

	w := ts.DoRequest("GET", pathDMConversationsPrefix+notAUUID, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetConversation_NonexistentID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "getdmnone")
	fakeID := uuid.New().String()

	w := ts.DoRequest("GET", pathDMConversationsPrefix+fakeID, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code) // Not participant of nonexistent conv
}

func TestGetConversation_IncludesEncryptedFlag(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getenc1")
	user2 := ts.CreateTestUser(t, "getenc2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conv := body["conversation"].(map[string]interface{})
	assert.Equal(t, true, conv["is_encrypted"])
}

func TestGetConversation_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getdmunauth1")
	user2 := ts.CreateTestUser(t, "getdmunauth2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ============================================================================
// OpenConversation Tests
// ============================================================================

func TestOpenConversation_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "opendm1")
	user2 := ts.CreateTestUser(t, "opendm2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))

	// Should be 200 or 201
	assert.True(t, w.Code == http.StatusOK || w.Code == http.StatusCreated,
		fmtExpected2xx, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conv := body["conversation"].(map[string]interface{})
	assert.NotEmpty(t, conv["id"])
}

func TestOpenConversation_Idempotent(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "opendm3")
	user2 := ts.CreateTestUser(t, "opendm4")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)

	// Open twice — should return same conversation
	w1 := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	require.True(t, w1.Code == http.StatusOK || w1.Code == http.StatusCreated)

	var body1 map[string]interface{}
	testhelpers.ParseJSON(t, w1, &body1)
	conv1 := body1["conversation"].(map[string]interface{})

	w2 := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	require.True(t, w2.Code == http.StatusOK || w2.Code == http.StatusCreated)

	var body2 map[string]interface{}
	testhelpers.ParseJSON(t, w2, &body2)
	conv2 := body2["conversation"].(map[string]interface{})

	assert.Equal(t, conv1["id"], conv2["id"], "same conversation should be returned")
}

func TestOpenConversation_MissingUserID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "openmissing")

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestOpenConversation_InvalidUserID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "openinvalid")

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": notAUUID,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestOpenConversation_SelfDM(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "openself")

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": user.ID,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "yourself")
}

func TestOpenConversation_UserNotFound(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "openghost")
	fakeID := uuid.New().String()

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": fakeID,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestOpenConversation_PrivacyBlocked_DMDisabled(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "openprivdis1")
	user2 := ts.CreateTestUser(t, "openprivdis2")
	// No friendship. Target has DMs disabled.
	setDMPrivacy(t, ts, user2.ID, 0, false)

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "dm_disabled", body["error"])
}

func TestOpenConversation_PrivacyBlocked_FriendsOnly(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "openprivfr1")
	user2 := ts.CreateTestUser(t, "openprivfr2")
	// No friendship. Target allows friends only.
	setDMPrivacy(t, ts, user2.ID, 1, false)

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "privacy_blocked", body["error"])
}

func TestOpenConversation_FriendsAllowed(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "openfriendok1")
	user2 := ts.CreateTestUser(t, "openfriendok2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	// Target has friends only, but they are friends
	setDMPrivacy(t, ts, user2.ID, 1, false)

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.True(t, w.Code == http.StatusOK || w.Code == http.StatusCreated,
		fmtExpected2xx, w.Code)
}

func TestOpenConversation_SharedServerAllowed(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "opensrvok1")
	user2 := ts.CreateTestUser(t, "opensrvok2")
	// No friendship, but share a server. Target allows friends + server members (level 2, default).
	serverID := ts.CreateTestServer(t, user1.ID, "Shared Server")
	ts.AddMemberToServer(t, serverID, user2.ID, "member")
	setDMPrivacy(t, ts, user2.ID, 2, false)

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.True(t, w.Code == http.StatusOK || w.Code == http.StatusCreated,
		fmtExpected2xx, w.Code)
}

func TestOpenConversation_OpenToAll(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "openallok1")
	user2 := ts.CreateTestUser(t, "openallok2")
	// No friendship, no shared servers, but target allows all.
	setDMPrivacy(t, ts, user2.ID, 3, false)

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": user2.ID,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.True(t, w.Code == http.StatusOK || w.Code == http.StatusCreated,
		fmtExpected2xx, w.Code)
}

func TestOpenConversation_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	_ = ts

	w := ts.DoRequest("POST", pathDMConversations, map[string]interface{}{
		"user_id": uuid.New().String(),
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ============================================================================
// CreateGroup Tests
// ============================================================================

func TestCreateGroup_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "grp1")
	user2 := ts.CreateTestUser(t, "grp2")
	user3 := ts.CreateTestUser(t, "grp3")

	groupName := "Test Group"
	w := ts.DoRequest("POST", pathDMConversations+pathGroup, map[string]interface{}{
		"user_ids": []string{user2.ID, user3.ID},
		"name":     groupName,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conv := body["conversation"].(map[string]interface{})
	assert.NotEmpty(t, conv["id"])
	assert.Equal(t, true, conv["is_group"])
	assert.Equal(t, groupName, conv["name"])

	participants := conv["participants"].([]interface{})
	assert.Len(t, participants, 3)
}

func TestCreateGroup_NoName(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "grpnoname1")
	user2 := ts.CreateTestUser(t, "grpnoname2")

	w := ts.DoRequest("POST", pathDMConversations+pathGroup, map[string]interface{}{
		"user_ids": []string{user2.ID},
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conv := body["conversation"].(map[string]interface{})
	assert.True(t, conv["is_group"].(bool))
}

func TestCreateGroup_MissingUserIDs(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "grpmissing")

	w := ts.DoRequest("POST", pathDMConversations+pathGroup, map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreateGroup_EmptyUserIDs(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "grpempty")

	w := ts.DoRequest("POST", pathDMConversations+pathGroup, map[string]interface{}{
		"user_ids": []string{},
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreateGroup_TooManyParticipants(t *testing.T) {
	ts := setupTS(t)
	creator := ts.CreateTestUser(t, "grpbig")

	// Create 10 other users — exceeds the limit of 9
	userIDs := make([]string, 10)
	for i := 0; i < 10; i++ {
		u := ts.CreateTestUser(t, fmt.Sprintf("grpbig%d", i))
		userIDs[i] = u.ID
	}

	w := ts.DoRequest("POST", pathDMConversations+pathGroup, map[string]interface{}{
		"user_ids": userIDs,
	}, testhelpers.AuthHeaders(creator.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "10 participants")
}

func TestCreateGroup_InvalidUserID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "grpinvaliduid")

	w := ts.DoRequest("POST", pathDMConversations+pathGroup, map[string]interface{}{
		"user_ids": []string{notAUUID},
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreateGroup_IncludesSelf(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "grpself")

	w := ts.DoRequest("POST", pathDMConversations+pathGroup, map[string]interface{}{
		"user_ids": []string{user.ID},
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "yourself")
}

func TestCreateGroup_MaxParticipants(t *testing.T) {
	ts := setupTS(t)
	creator := ts.CreateTestUser(t, "grpmax")

	// Create exactly 9 other users — total 10 including creator, at the limit
	userIDs := make([]string, 9)
	for i := 0; i < 9; i++ {
		u := ts.CreateTestUser(t, fmt.Sprintf("grpmax%d", i))
		userIDs[i] = u.ID
	}

	w := ts.DoRequest("POST", pathDMConversations+pathGroup, map[string]interface{}{
		"user_ids": userIDs,
	}, testhelpers.AuthHeaders(creator.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestCreateGroup_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	_ = ts

	w := ts.DoRequest("POST", pathDMConversations+pathGroup, map[string]interface{}{
		"user_ids": []string{uuid.New().String()},
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ============================================================================
// UpdateConversation Tests
// ============================================================================

func TestUpdateConversation_RenameGroup(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "upconv1")
	user2 := ts.CreateTestUser(t, "upconv2")
	user3 := ts.CreateTestUser(t, "upconv3")

	// Create a group DM via API
	w := ts.DoRequest("POST", pathDMConversations+pathGroup, map[string]interface{}{
		"user_ids": []string{user2.ID, user3.ID},
		"name":     "Old Name",
	}, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	convID := createBody["conversation"].(map[string]interface{})["id"].(string)

	newName := "New Group Name"
	w = ts.DoRequest("PATCH", pathDMConversationsPrefix+convID, map[string]interface{}{
		"name": newName,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdateConversation_CannotRenameOneOnOne(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "upconv1on1a")
	user2 := ts.CreateTestUser(t, "upconv1on1b")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID, map[string]interface{}{
		"name": "New Name",
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "1:1")
}

func TestUpdateConversation_NotParticipant(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "upconvnp1")
	user2 := ts.CreateTestUser(t, "upconvnp2")
	outsider := ts.CreateTestUser(t, "upconvnp3")

	w := ts.DoRequest("POST", pathDMConversations+pathGroup, map[string]interface{}{
		"user_ids": []string{user2.ID},
		"name":     "Secret Group",
	}, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	convID := createBody["conversation"].(map[string]interface{})["id"].(string)

	w = ts.DoRequest("PATCH", pathDMConversationsPrefix+convID, map[string]interface{}{
		"name": "Hacked Name",
	}, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUpdateConversation_InvalidConversationID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "upconvinv")

	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+notAUUID, map[string]interface{}{
		"name": "New Name",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateConversation_InvalidBody(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "upconvbadbody1")
	user2 := ts.CreateTestUser(t, "upconvbadbody2")

	w := ts.DoRequest("POST", pathDMConversations+pathGroup, map[string]interface{}{
		"user_ids": []string{user2.ID},
	}, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	convID := createBody["conversation"].(map[string]interface{})["id"].(string)

	// Send request with no JSON body
	w = ts.DoRequest("PATCH", pathDMConversationsPrefix+convID, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateConversation_ClearName(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "upconvclear1")
	user2 := ts.CreateTestUser(t, "upconvclear2")

	w := ts.DoRequest("POST", pathDMConversations+pathGroup, map[string]interface{}{
		"user_ids": []string{user2.ID},
		"name":     "Has Name",
	}, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	convID := createBody["conversation"].(map[string]interface{})["id"].(string)

	// Setting name to null clears it
	w = ts.DoRequest("PATCH", pathDMConversationsPrefix+convID, map[string]interface{}{
		"name": nil,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

// ============================================================================
// GetMessages Tests
// ============================================================================

func TestGetMessages_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getmsg1")
	user2 := ts.CreateTestUser(t, "getmsg2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	insertDMMessage(t, ts, convID, user1.ID, "hello", false)
	insertDMMessage(t, ts, convID, user2.ID, "hi there", false)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+pathMessages, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	messages := body["messages"].([]interface{})
	assert.Len(t, messages, 2)
}

func TestGetMessages_Empty(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getmsge1")
	user2 := ts.CreateTestUser(t, "getmsge2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+pathMessages, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	messages := body["messages"].([]interface{})
	assert.Len(t, messages, 0)
}

func TestGetMessages_NotParticipant(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getmsgnp1")
	user2 := ts.CreateTestUser(t, "getmsgnp2")
	outsider := ts.CreateTestUser(t, "getmsgnp3")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+pathMessages, nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestGetMessages_InvalidConversationID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "getmsginv")

	w := ts.DoRequest("GET", pathDMConversationsPrefix+"not-a-uuid/messages", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetMessages_CursorPagination(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getmsgcur1")
	user2 := ts.CreateTestUser(t, "getmsgcur2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	// Insert some messages
	insertDMMessage(t, ts, convID, user1.ID, "msg1", false)
	insertDMMessage(t, ts, convID, user1.ID, "msg2", false)
	cursorMsgID := insertDMMessage(t, ts, convID, user1.ID, "msg3", false)

	// Fetch messages before the cursor
	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+"/messages?before="+cursorMsgID, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	messages := body["messages"].([]interface{})
	// msg3 is the cursor, so we should get msg1 and msg2 (created before msg3)
	assert.Equal(t, 2, len(messages))
}

func TestGetMessages_InvalidCursor(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getmsgbadc1")
	user2 := ts.CreateTestUser(t, "getmsgbadc2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+"/messages?before=not-a-uuid", nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetMessages_MessageFields(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getmsgfields1")
	user2 := ts.CreateTestUser(t, "getmsgfields2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	insertDMMessage(t, ts, convID, user1.ID, "test content", false)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+pathMessages, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	messages := body["messages"].([]interface{})
	require.Len(t, messages, 1)

	msg := messages[0].(map[string]interface{})
	assert.NotEmpty(t, msg["id"])
	assert.Equal(t, convID, msg["conversation_id"])
	assert.Equal(t, user1.ID, msg["user_id"])
	assert.Equal(t, "test content", msg["content"])
	assert.Equal(t, false, msg["is_encrypted"])
	assert.Equal(t, "text", msg["type"])
	assert.NotEmpty(t, msg["created_at"])
	assert.NotEmpty(t, msg["username"])
}

func TestGetMessages_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getmsgunauth1")
	user2 := ts.CreateTestUser(t, "getmsgunauth2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+pathMessages, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ============================================================================
// MarkRead Tests
// ============================================================================

func TestMarkRead_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "markread1")
	user2 := ts.CreateTestUser(t, "markread2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/read", nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Marked as read", body["message"])
}

func TestMarkRead_Idempotent(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "markread3")
	user2 := ts.CreateTestUser(t, "markread4")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	// Mark read twice — should succeed both times (UPSERT)
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/read", nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	w = ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/read", nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestMarkRead_NotParticipant(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "markreadnp1")
	user2 := ts.CreateTestUser(t, "markreadnp2")
	outsider := ts.CreateTestUser(t, "markreadnp3")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/read", nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestMarkRead_InvalidConversationID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "markreadinv")

	w := ts.DoRequest("POST", pathDMConversationsPrefix+"not-a-uuid/read", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestMarkRead_ResetsUnreadCount(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "markreadreset1")
	user2 := ts.CreateTestUser(t, "markreadreset2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	// user2 sends messages
	insertDMMessage(t, ts, convID, user2.ID, "unread1", false)
	insertDMMessage(t, ts, convID, user2.ID, "unread2", false)

	// Verify unread count > 0
	w := ts.DoRequest("GET", pathDMConversations, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var listBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &listBody)
	convs := listBody["conversations"].([]interface{})
	require.Len(t, convs, 1)
	assert.Equal(t, float64(2), convs[0].(map[string]interface{})["unread_count"])

	// Mark as read
	w = ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/read", nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Verify unread count is now 0
	w = ts.DoRequest("GET", pathDMConversations, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	testhelpers.ParseJSON(t, w, &listBody)
	convs = listBody["conversations"].([]interface{})
	require.Len(t, convs, 1)
	assert.Equal(t, float64(0), convs[0].(map[string]interface{})["unread_count"])
}

// ============================================================================
// GetKeys Tests
// ============================================================================

func TestGetKeys_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getkeys1")
	user2 := ts.CreateTestUser(t, "getkeys2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)
	ts.SeedDMKey(t, convID, user1.ID, 1)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+"/keys", nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	key := body["key"].(map[string]interface{})
	assert.Equal(t, convID, key["conversation_id"])
	assert.Equal(t, user1.ID, key["user_id"])
	assert.Equal(t, float64(1), key["key_version"])
}

func TestGetKeys_NotEncrypted(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getkeysnenc1")
	user2 := ts.CreateTestUser(t, "getkeysnenc2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false) // not encrypted

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+"/keys", nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "not encrypted")
}

func TestGetKeys_NoKeysYet(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getkeysnone1")
	user2 := ts.CreateTestUser(t, "getkeysnone2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)
	// No keys seeded

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+"/keys", nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, true, body["pending"])
}

func TestGetKeys_NotParticipant(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getkeysnp1")
	user2 := ts.CreateTestUser(t, "getkeysnp2")
	outsider := ts.CreateTestUser(t, "getkeysnp3")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+"/keys", nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestGetKeys_InvalidConversationID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "getkeysinv")

	w := ts.DoRequest("GET", pathDMConversationsPrefix+"not-a-uuid/keys", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetKeys_ReturnsLatestVersion(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getkeyslat1")
	user2 := ts.CreateTestUser(t, "getkeyslat2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user1.ID, 2)
	ts.SeedDMKey(t, convID, user1.ID, 3)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+"/keys", nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	key := body["key"].(map[string]interface{})
	assert.Equal(t, float64(3), key["key_version"])
}

// ============================================================================
// DistributeKeys Tests
// ============================================================================

func TestDistributeKeys_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "distkeys1")
	user2 := ts.CreateTestUser(t, "distkeys2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{
			user1.ID: "wrapped-key-for-user1",
			user2.ID: "wrapped-key-for-user2",
		},
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(2), body["distributed"])
}

func TestDistributeKeys_WithExplicitVersion(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "distkeyver1")
	user2 := ts.CreateTestUser(t, "distkeyver2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)

	keyVersion := 5
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{
			user2.ID: "wrapped-key-v5",
		},
		"key_version": keyVersion,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(1), body["distributed"])
}

func TestDistributeKeys_NotParticipant(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "distkeysnp1")
	user2 := ts.CreateTestUser(t, "distkeysnp2")
	outsider := ts.CreateTestUser(t, "distkeysnp3")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{
			user1.ID: wrappedKey,
		},
	}, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestDistributeKeys_InvalidConversationID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "distkeysinv")

	w := ts.DoRequest("POST", pathDMConversationsPrefix+"not-a-uuid/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{
			user.ID: wrappedKey,
		},
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDistributeKeys_InvalidRequestBody(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "distkeysbad1")
	user2 := ts.CreateTestUser(t, "distkeysbad2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/keys", map[string]interface{}{}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDistributeKeys_DuplicateKeyVersionSkipped(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "distkeysdup1")
	user2 := ts.CreateTestUser(t, "distkeysdup2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)

	// Distribute once
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{
			user2.ID: "wrapped-key-v1",
		},
		"key_version": 1,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Distribute again with same version — ON CONFLICT DO NOTHING
	w = ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{
			user2.ID: "wrapped-key-v1-again",
		},
		"key_version": 1,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(0), body["distributed"], "duplicate should not be distributed")
}

func TestDistributeKeys_InvalidMemberUUID(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "distkeysbaduuid1")
	user2 := ts.CreateTestUser(t, "distkeysbaduuid2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)

	// Invalid member UUID should be silently skipped
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{
			notAUUID: wrappedKey,
		},
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(0), body["distributed"])
}

// ============================================================================
// AuthorizeVoiceJoin Tests
// ============================================================================

func TestAuthorizeVoiceJoin_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "voice1")
	user2 := ts.CreateTestUser(t, "voice2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceJoin, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, true, body["allowed"])
	assert.NotNil(t, body["ice_servers"])

	conv := body["conversation"].(map[string]interface{})
	assert.Equal(t, convID, conv["id"])
}

func TestAuthorizeVoiceJoin_EncryptedConversation(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "voiceenc1")
	user2 := ts.CreateTestUser(t, "voiceenc2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceJoin, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conv := body["conversation"].(map[string]interface{})
	assert.Equal(t, true, conv["is_encrypted"])
}

func TestAuthorizeVoiceJoin_NotParticipant(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "voicenp1")
	user2 := ts.CreateTestUser(t, "voicenp2")
	outsider := ts.CreateTestUser(t, "voicenp3")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceJoin, nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestAuthorizeVoiceJoin_InvalidConversationID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "voiceinv")

	w := ts.DoRequest("POST", pathDMConversationsPrefix+"not-a-uuid/voice/join", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAuthorizeVoiceJoin_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	_ = ts
	convID := uuid.New().String()

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceJoin, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ============================================================================
// GetVoiceParticipants Tests
// ============================================================================

func TestGetVoiceParticipants_EmptyList(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getvp1")
	user2 := ts.CreateTestUser(t, "getvp2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+pathVoiceParts, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	participants := body["participants"].([]interface{})
	assert.Len(t, participants, 0)
}

func TestGetVoiceParticipants_WithParticipant(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getvpwith1")
	user2 := ts.CreateTestUser(t, "getvpwith2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	// Insert a voice participant directly
	_, err := ts.DB.Exec(
		`INSERT INTO dm_voice_participants (conversation_id, user_id, is_muted, is_deafened, is_video_on, is_screen_sharing) VALUES ($1, $2, false, false, false, false)`,
		convID, user1.ID,
	)
	require.NoError(t, err)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+pathVoiceParts, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	participants := body["participants"].([]interface{})
	assert.Len(t, participants, 1)

	p := participants[0].(map[string]interface{})
	assert.Equal(t, user1.ID, p["user_id"])
	assert.NotEmpty(t, p["username"])
}

func TestGetVoiceParticipants_NotParticipant(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "getvpnp1")
	user2 := ts.CreateTestUser(t, "getvpnp2")
	outsider := ts.CreateTestUser(t, "getvpnp3")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+pathVoiceParts, nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestGetVoiceParticipants_InvalidConversationID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "getvpinv")

	w := ts.DoRequest("GET", pathDMConversationsPrefix+"not-a-uuid/voice/participants", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ============================================================================
// GetOrCreatePersonalThread Tests
// ============================================================================

func TestGetOrCreatePersonalThread_Create(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "personal1")

	w := ts.DoRequest("POST", pathDMConversations+pathPersonal, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	conv := body["conversation"].(map[string]interface{})
	assert.NotEmpty(t, conv["id"])
	assert.Equal(t, true, conv["is_personal"])
	assert.Equal(t, false, conv["is_group"])

	participants := conv["participants"].([]interface{})
	assert.Len(t, participants, 1, "personal thread should have only the creator")
}

func TestGetOrCreatePersonalThread_Idempotent(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "personal2")

	// First call creates
	w1 := ts.DoRequest("POST", pathDMConversations+pathPersonal, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusCreated, w1.Code)

	var body1 map[string]interface{}
	testhelpers.ParseJSON(t, w1, &body1)
	conv1ID := body1["conversation"].(map[string]interface{})["id"].(string)

	// Second call returns existing
	w2 := ts.DoRequest("POST", pathDMConversations+pathPersonal, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w2.Code)

	var body2 map[string]interface{}
	testhelpers.ParseJSON(t, w2, &body2)
	conv2ID := body2["conversation"].(map[string]interface{})["id"].(string)

	assert.Equal(t, conv1ID, conv2ID, "same personal thread should be returned")
}

func TestGetOrCreatePersonalThread_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	_ = ts

	w := ts.DoRequest("POST", pathDMConversations+pathPersonal, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ============================================================================
// UpdateMessage Tests
// ============================================================================

func TestUpdateMessage_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "updatemsg1")
	user2 := ts.CreateTestUser(t, "updatemsg2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)
	msgID := insertDMMessage(t, ts, convID, user1.ID, "original text", false)

	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMsgSlash+msgID, map[string]interface{}{
		"content": "edited text",
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	msg := body["message"].(map[string]interface{})
	assert.Equal(t, "edited text", msg["content"])
	assert.NotNil(t, msg["edited_at"], "edited_at should be set")
}

func TestUpdateMessage_NotAuthor(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "updatemsgna1")
	user2 := ts.CreateTestUser(t, "updatemsgna2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)
	msgID := insertDMMessage(t, ts, convID, user1.ID, "user1's message", false)

	// user2 tries to edit user1's message
	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMsgSlash+msgID, map[string]interface{}{
		"content": "hacked",
	}, testhelpers.AuthHeaders(user2.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "own messages")
}

func TestUpdateMessage_NotParticipant(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "updatemsgnp1")
	user2 := ts.CreateTestUser(t, "updatemsgnp2")
	outsider := ts.CreateTestUser(t, "updatemsgnp3")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)
	msgID := insertDMMessage(t, ts, convID, user1.ID, "message", false)

	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMsgSlash+msgID, map[string]interface{}{
		"content": "hacked",
	}, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUpdateMessage_InvalidConversationID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updatemsginv1")
	msgID := uuid.New().String()

	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+"not-a-uuid/messages/"+msgID, map[string]interface{}{
		"content": "edited",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateMessage_InvalidMessageID(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "updatemsginv2a")
	user2 := ts.CreateTestUser(t, "updatemsginv2b")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+"/messages/not-a-uuid", map[string]interface{}{
		"content": "edited",
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateMessage_MessageNotFound(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "updatemsgnotf1")
	user2 := ts.CreateTestUser(t, "updatemsgnotf2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)
	fakeMsgID := uuid.New().String()

	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMsgSlash+fakeMsgID, map[string]interface{}{
		"content": "edited",
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUpdateMessage_MissingContent(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "updatemsgnocon1")
	user2 := ts.CreateTestUser(t, "updatemsgnocon2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)
	msgID := insertDMMessage(t, ts, convID, user1.ID, "original", false)

	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMsgSlash+msgID, map[string]interface{}{}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateMessage_EncryptedConversation_ValidCiphertext(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "updatemsgenc1")
	user2 := ts.CreateTestUser(t, "updatemsgenc2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)

	ciphertext := testhelpers.ValidCiphertext()
	msgID := insertDMMessage(t, ts, convID, user1.ID, ciphertext, true)

	newCiphertext := testhelpers.ValidCiphertext()
	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMsgSlash+msgID, map[string]interface{}{
		"content": newCiphertext,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdateMessage_EncryptedConversation_InvalidCiphertext(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "updatemsgencinv1")
	user2 := ts.CreateTestUser(t, "updatemsgencinv2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)

	ciphertext := testhelpers.ValidCiphertext()
	msgID := insertDMMessage(t, ts, convID, user1.ID, ciphertext, true)

	// Plaintext in encrypted conversation should be rejected
	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMsgSlash+msgID, map[string]interface{}{
		"content": "plaintext in encrypted conv",
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "ciphertext")
}

// ============================================================================
// DeleteMessage Tests
// ============================================================================

func TestDeleteMessage_Success(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "delmsg1")
	user2 := ts.CreateTestUser(t, "delmsg2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)
	msgID := insertDMMessage(t, ts, convID, user1.ID, "to be deleted", false)

	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+pathMsgSlash+msgID, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, true, body["success"])

	// Verify message is actually deleted
	var count int
	err := ts.DB.QueryRow(`SELECT COUNT(*) FROM dm_messages WHERE id = $1`, msgID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "message should be deleted from database")
}

func TestDeleteMessage_NotAuthor(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "delmsgna1")
	user2 := ts.CreateTestUser(t, "delmsgna2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)
	msgID := insertDMMessage(t, ts, convID, user1.ID, "user1's msg", false)

	// user2 tries to delete user1's message
	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+pathMsgSlash+msgID, nil, testhelpers.AuthHeaders(user2.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "own messages")
}

func TestDeleteMessage_NotParticipant(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "delmsgnp1")
	user2 := ts.CreateTestUser(t, "delmsgnp2")
	outsider := ts.CreateTestUser(t, "delmsgnp3")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)
	msgID := insertDMMessage(t, ts, convID, user1.ID, "message", false)

	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+pathMsgSlash+msgID, nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestDeleteMessage_InvalidConversationID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "delmsginv1")
	msgID := uuid.New().String()

	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+"not-a-uuid/messages/"+msgID, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDeleteMessage_InvalidMessageID(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "delmsginv2a")
	user2 := ts.CreateTestUser(t, "delmsginv2b")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+"/messages/not-a-uuid", nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDeleteMessage_MessageNotFound(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "delmsgnotf1")
	user2 := ts.CreateTestUser(t, "delmsgnotf2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)
	fakeMsgID := uuid.New().String()

	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+pathMsgSlash+fakeMsgID, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestDeleteMessage_Unauthorized(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "delmsgunauth1")
	user2 := ts.CreateTestUser(t, "delmsgunauth2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)
	msgID := insertDMMessage(t, ts, convID, user1.ID, "msg", false)

	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+pathMsgSlash+msgID, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ============================================================================
// DM Attachment Enrichment Tests (#178)
// ============================================================================

func insertDMMediaFile(t *testing.T, ts *testhelpers.TestServer, uploaderID, convID, fileType, mimeType string, fileSize int64) string {
	t.Helper()
	var fileID string
	err := ts.DB.QueryRow(
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, is_encrypted, key_version, conversation_id, created_at)
		 VALUES (gen_random_uuid(), $1, $2, 2, $3, $4, 'attachments/' || gen_random_uuid(), true, 1, $5, NOW())
		 RETURNING id`,
		uploaderID, fileType, mimeType, fileSize, convID,
	).Scan(&fileID)
	require.NoError(t, err)
	return fileID
}

func insertDMMessageAttachment(t *testing.T, ts *testhelpers.TestServer, messageID, fileID string, position int) {
	t.Helper()
	_, err := ts.DB.Exec(
		`INSERT INTO dm_message_attachments (message_id, file_id, position) VALUES ($1, $2, $3)`,
		messageID, fileID, position,
	)
	require.NoError(t, err)
}

func TestGetMessagesWithAttachments(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmatt1")
	user2 := ts.CreateTestUser(t, "dmatt2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	msgID := insertDMMessage(t, ts, convID, user1.ID, "see attached", false)
	fileID := insertDMMediaFile(t, ts, user1.ID, convID, "photo", mimeImagePNG, 12345)
	insertDMMessageAttachment(t, ts, msgID, fileID, 0)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+pathMessages, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body struct {
		Messages []struct {
			ID          string `json:"id"`
			Attachments []struct {
				ID       string `json:"id"`
				FileType string `json:"file_type"`
				MimeType string `json:"mime_type"`
				FileSize int64  `json:"file_size"`
			} `json:"attachments"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	var found bool
	for _, msg := range body.Messages {
		if msg.ID == msgID {
			found = true
			require.Len(t, msg.Attachments, 1)
			assert.Equal(t, fileID, msg.Attachments[0].ID)
			assert.Equal(t, "photo", msg.Attachments[0].FileType)
			assert.Equal(t, mimeImagePNG, msg.Attachments[0].MimeType)
			assert.Equal(t, int64(12345), msg.Attachments[0].FileSize)
			break
		}
	}
	assert.True(t, found, "DM message with attachment not found")
}

func TestGetMessagesWithMultipleAttachmentsOrderedByPosition(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmmulti1")
	user2 := ts.CreateTestUser(t, "dmmulti2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	msgID := insertDMMessage(t, ts, convID, user1.ID, "multi", false)
	f1 := insertDMMediaFile(t, ts, user1.ID, convID, "photo", mimeImagePNG, 100)
	f2 := insertDMMediaFile(t, ts, user1.ID, convID, "file", "application/pdf", 200)

	// Insert in reverse order to verify position sorting
	insertDMMessageAttachment(t, ts, msgID, f2, 1)
	insertDMMessageAttachment(t, ts, msgID, f1, 0)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+pathMessages, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body struct {
		Messages []struct {
			ID          string `json:"id"`
			Attachments []struct {
				ID string `json:"id"`
			} `json:"attachments"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	for _, msg := range body.Messages {
		if msg.ID == msgID {
			require.Len(t, msg.Attachments, 2)
			assert.Equal(t, f1, msg.Attachments[0].ID)
			assert.Equal(t, f2, msg.Attachments[1].ID)
			return
		}
	}
	t.Fatal("DM message not found")
}

func TestGetMessagesDMNoAttachments(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmnoatt1")
	user2 := ts.CreateTestUser(t, "dmnoatt2")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

	insertDMMessage(t, ts, convID, user1.ID, "plain msg", false)

	w := ts.DoRequest("GET", pathDMConversationsPrefix+convID+pathMessages, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body struct {
		Messages []struct {
			Attachments []struct{ ID string } `json:"attachments"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.NotEmpty(t, body.Messages)
	assert.Empty(t, body.Messages[0].Attachments)
}
