package channels_test

import (
	"net/http"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
)

func setupTS(t *testing.T) *testhelpers.TestServer {
	t.Helper()
	return testhelpers.SetupTestServer(t)
}

// Helper: create a user + server + channel combo
func setupWithChannel(t *testing.T) (*testhelpers.TestServer, testhelpers.TestUser, string, string) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "chanuser")
	serverID := ts.CreateTestServer(t, user.ID, "Channel Test Server")
	channelID := ts.CreateTestChannel(t, serverID, "general", false)
	return ts, user, serverID, channelID
}

// --- List Channels ---

func TestListChannels_Success(t *testing.T) {
	ts, user, serverID, _ := setupWithChannel(t)

	w := ts.DoRequest("GET", "/api/v1/servers/"+serverID+"/channels", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	channels := body["channels"].([]interface{})
	assert.GreaterOrEqual(t, len(channels), 1)
}

func TestListChannels_NotMember(t *testing.T) {
	ts, _, serverID, _ := setupWithChannel(t)
	outsider := ts.CreateTestUser(t, "chanoutsider")

	w := ts.DoRequest("GET", "/api/v1/servers/"+serverID+"/channels", nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// --- Create Channel ---

func TestCreateChannel_Success(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "createchan")
	serverID := ts.CreateTestServer(t, user.ID, "Chan Create Server")

	w := ts.DoRequest("POST", "/api/v1/channels", map[string]interface{}{
		"server_id": serverID,
		"name":      "new-channel",
		"type":      "text",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	channel := body["channel"].(map[string]interface{})
	assert.Equal(t, "new-channel", channel["name"])
}

func TestCreateChannel_NotAdmin(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "chanowner2")
	member := ts.CreateTestUser(t, "chanmember2")
	serverID := ts.CreateTestServer(t, owner.ID, "Not Admin Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("POST", "/api/v1/channels", map[string]interface{}{
		"server_id": serverID,
		"name":      "blocked-channel",
		"type":      "text",
	}, testhelpers.AuthHeaders(member.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestCreateChannel_Encrypted(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "encrchan")
	serverID := ts.CreateTestServer(t, user.ID, "Encrypted Server")

	w := ts.DoRequest("POST", "/api/v1/channels", map[string]interface{}{
		"server_id":    serverID,
		"name":         "encrypted-channel",
		"type":         "text",
		"is_encrypted": true,
		"wrapped_keys": map[string]string{
			user.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	channel := body["channel"].(map[string]interface{})
	assert.Equal(t, true, channel["is_encrypted"])
}

// --- Get Channel ---

func TestGetChannel_Success(t *testing.T) {
	ts, user, _, channelID := setupWithChannel(t)

	w := ts.DoRequest("GET", "/api/v1/channels/"+channelID, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

// --- Update Channel ---

func TestUpdateChannel_Success(t *testing.T) {
	ts, user, _, channelID := setupWithChannel(t)

	w := ts.DoRequest("PATCH", "/api/v1/channels/"+channelID, map[string]interface{}{
		"name": "renamed-channel",
		"type": "text",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	channel := body["channel"].(map[string]interface{})
	assert.Equal(t, "renamed-channel", channel["name"])
}

// --- Delete Channel ---

func TestDeleteChannel_AsOwner(t *testing.T) {
	ts, user, _, channelID := setupWithChannel(t)

	w := ts.DoRequest("DELETE", "/api/v1/channels/"+channelID, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestDeleteChannel_NotAdmin(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "dchowner")
	member := ts.CreateTestUser(t, "dchmember")
	serverID := ts.CreateTestServer(t, owner.ID, "Del Chan Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := ts.CreateTestChannel(t, serverID, "protected", false)

	w := ts.DoRequest("DELETE", "/api/v1/channels/"+channelID, nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// --- Mark Read ---

func TestMarkChannelRead_Success(t *testing.T) {
	ts, user, _, channelID := setupWithChannel(t)

	w := ts.DoRequest("POST", "/api/v1/channels/"+channelID+"/read", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestMarkServerRead_Success(t *testing.T) {
	ts, user, serverID, _ := setupWithChannel(t)

	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/read", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

// --- Unread Counts ---

func TestGetUnreadCounts_Success(t *testing.T) {
	ts, user, serverID, _ := setupWithChannel(t)

	w := ts.DoRequest("GET", "/api/v1/servers/"+serverID+"/unread", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestGetServerUnreadStatus_Success(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "unreaduser")

	w := ts.DoRequest("GET", "/api/v1/servers/unread-status", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

// --- Channel Key Management (E2EE) ---

// Helper: create encrypted channel via API (stores initial channel key)
func setupEncryptedChannel(t *testing.T) (*testhelpers.TestServer, testhelpers.TestUser, string, string) {
	t.Helper()
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "e2eeuser")
	serverID := ts.CreateTestServer(t, user.ID, "E2EE Server")

	w := ts.DoRequest("POST", "/api/v1/channels", map[string]interface{}{
		"server_id":    serverID,
		"name":         "secret-channel",
		"type":         "text",
		"is_encrypted": true,
		"wrapped_keys": map[string]string{
			user.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	channel := body["channel"].(map[string]interface{})
	channelID := channel["id"].(string)

	return ts, user, serverID, channelID
}

func TestGetChannelKeys_Success(t *testing.T) {
	ts, user, _, channelID := setupEncryptedChannel(t)

	w := ts.DoRequest("GET", "/api/v1/channels/"+channelID+"/keys", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.NotNil(t, body["key"])
}

func TestGetChannelKeys_NotEncrypted(t *testing.T) {
	ts, user, _, channelID := setupWithChannel(t) // plaintext channel

	w := ts.DoRequest("GET", "/api/v1/channels/"+channelID+"/keys", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetChannelKeys_NotMember(t *testing.T) {
	ts, _, _, channelID := setupEncryptedChannel(t)
	outsider := ts.CreateTestUser(t, "keyoutsider")

	w := ts.DoRequest("GET", "/api/v1/channels/"+channelID+"/keys", nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestGetChannelKeys_InvalidChannelID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "badchid")

	w := ts.DoRequest("GET", "/api/v1/channels/not-a-uuid/keys", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDistributeChannelKeys_Success(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)

	// Add a new member to the server
	newMember := ts.CreateTestUser(t, "newmember")
	ts.AddMemberToServer(t, serverID, newMember.ID, "member")

	// Owner distributes key to the new member
	w := ts.DoRequest("POST", "/api/v1/channels/"+channelID+"/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{
			newMember.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(1), body["distributed"])
	assert.Equal(t, float64(0), body["duplicates"])
}

func TestDistributeChannelKeys_Duplicate(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)

	// Add a new member
	newMember := ts.CreateTestUser(t, "dupmember")
	ts.AddMemberToServer(t, serverID, newMember.ID, "member")

	// Distribute once
	ts.DoRequest("POST", "/api/v1/channels/"+channelID+"/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{
			newMember.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(owner.AccessToken))

	// Distribute again — should be duplicate
	w := ts.DoRequest("POST", "/api/v1/channels/"+channelID+"/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{
			newMember.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(0), body["distributed"])
	assert.Equal(t, float64(1), body["duplicates"])
}

func TestDistributeChannelKeys_NotMember(t *testing.T) {
	ts, _, _, channelID := setupEncryptedChannel(t)
	outsider := ts.CreateTestUser(t, "distoutsider")

	w := ts.DoRequest("POST", "/api/v1/channels/"+channelID+"/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{
			outsider.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(outsider.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)
}

// --- Pending Key Requests ---

func TestGetPendingKeyRequests_Empty(t *testing.T) {
	ts, user, _, _ := setupEncryptedChannel(t)

	w := ts.DoRequest("GET", "/api/v1/e2ee/pending-keys", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	requests := body["pending_requests"].([]interface{})
	assert.Equal(t, 0, len(requests))
}
