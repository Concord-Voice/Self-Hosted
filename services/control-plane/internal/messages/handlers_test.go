package messages_test

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

// Helper: create a full stack and send a message, return the message ID
func setupWithMessage(t *testing.T) (*testhelpers.TestServer, testhelpers.TestUser, string, string, string) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "msguser")
	serverID := ts.CreateTestServer(t, user.ID, "Message Test Server")
	channelID := ts.CreateTestChannel(t, serverID, "general", false)

	w := ts.DoRequest("POST", "/api/v1/messages", map[string]interface{}{
		"channel_id": channelID,
		"content":    "Hello from tests!",
	}, testhelpers.AuthHeaders(user.AccessToken))

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	msg := body["message"].(map[string]interface{})
	msgID := msg["id"].(string)

	return ts, user, serverID, channelID, msgID
}

// --- Send Message ---

func TestSendMessageSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "sendmsg")
	serverID := ts.CreateTestServer(t, user.ID, "Send Server")
	channelID := ts.CreateTestChannel(t, serverID, "general", false)

	w := ts.DoRequest("POST", "/api/v1/messages", map[string]interface{}{
		"channel_id": channelID,
		"content":    "Test message content",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	msg := body["message"].(map[string]interface{})
	assert.Equal(t, "Test message content", msg["content"])
	assert.Equal(t, user.ID, msg["user_id"])
}

func TestSendMessageEmptyContent(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "emptymsg")
	serverID := ts.CreateTestServer(t, user.ID, "Empty Server")
	channelID := ts.CreateTestChannel(t, serverID, "general", false)

	w := ts.DoRequest("POST", "/api/v1/messages", map[string]interface{}{
		"channel_id": channelID,
		"content":    "",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSendMessageNotMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "msgowner")
	outsider := ts.CreateTestUser(t, "msgoutsider")
	serverID := ts.CreateTestServer(t, owner.ID, "Msg Server")
	channelID := ts.CreateTestChannel(t, serverID, "general", false)

	w := ts.DoRequest("POST", "/api/v1/messages", map[string]interface{}{
		"channel_id": channelID,
		"content":    "Unauthorized message",
	}, testhelpers.AuthHeaders(outsider.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestSendMessageEncryptedChannelRequiresCiphertext(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "e2eemsg")
	serverID := ts.CreateTestServer(t, user.ID, "E2EE Server")
	channelID := ts.CreateTestChannel(t, serverID, "encrypted", true)

	// Sending plaintext to encrypted channel should fail
	w := ts.DoRequest("POST", "/api/v1/messages", map[string]interface{}{
		"channel_id": channelID,
		"content":    "plaintext message",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSendMessageEncryptedChannelValidCiphertext(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "e2eemsg2")
	serverID := ts.CreateTestServer(t, user.ID, "E2EE Server 2")
	channelID := ts.CreateTestChannel(t, serverID, "encrypted", true)

	w := ts.DoRequest("POST", "/api/v1/messages", map[string]interface{}{
		"channel_id":   channelID,
		"content":      testhelpers.ValidCiphertext(),
		"is_encrypted": true,
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code)
}

// --- Get Messages ---

func TestGetMessagesSuccess(t *testing.T) {
	ts, user, _, channelID, _ := setupWithMessage(t)

	w := ts.DoRequest("GET", "/api/v1/channels/"+channelID+"/messages?limit=50", nil, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	messages := body["messages"].([]interface{})
	assert.GreaterOrEqual(t, len(messages), 1)
}

func TestGetMessagesEmpty(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "emptymsglist")
	serverID := ts.CreateTestServer(t, user.ID, "Empty Msg Server")
	channelID := ts.CreateTestChannel(t, serverID, "general", false)

	w := ts.DoRequest("GET", "/api/v1/channels/"+channelID+"/messages?limit=50", nil, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	messages := body["messages"].([]interface{})
	assert.Empty(t, messages)
}

func TestGetMessagesNotMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "msglistowner")
	outsider := ts.CreateTestUser(t, "msglistoutsider")
	serverID := ts.CreateTestServer(t, owner.ID, "Private Msg Server")
	channelID := ts.CreateTestChannel(t, serverID, "general", false)

	w := ts.DoRequest("GET", "/api/v1/channels/"+channelID+"/messages?limit=50", nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestGetMessagesPagination(t *testing.T) {
	ts, user, _, channelID, firstMsgID := setupWithMessage(t)

	// Send a second message
	ts.DoRequest("POST", "/api/v1/messages", map[string]interface{}{
		"channel_id": channelID,
		"content":    "Second message",
	}, testhelpers.AuthHeaders(user.AccessToken))

	// Get messages before the first message
	w := ts.DoRequest("GET", "/api/v1/channels/"+channelID+"/messages?limit=50&before="+firstMsgID, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	messages := body["messages"].([]interface{})
	assert.Empty(t, messages, "no messages should exist before the first one")
}

// --- Update Message ---

func TestUpdateMessageSuccess(t *testing.T) {
	ts, user, _, _, msgID := setupWithMessage(t)

	w := ts.DoRequest("PATCH", "/api/v1/messages/"+msgID, map[string]interface{}{
		"content": "Edited content",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	msg := body["message"].(map[string]interface{})
	assert.Equal(t, "Edited content", msg["content"])
	assert.NotNil(t, msg["edited_at"])
}

func TestUpdateMessageNotAuthor(t *testing.T) {
	ts, _, serverID, _, msgID := setupWithMessage(t)

	other := ts.CreateTestUser(t, "msgeditor")
	ts.AddMemberToServer(t, serverID, other.ID, "member")

	w := ts.DoRequest("PATCH", "/api/v1/messages/"+msgID, map[string]interface{}{
		"content": "Unauthorized edit",
	}, testhelpers.AuthHeaders(other.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)
}

// --- Delete Message ---

func TestDeleteMessageAsAuthor(t *testing.T) {
	ts, user, _, _, msgID := setupWithMessage(t)

	w := ts.DoRequest("DELETE", "/api/v1/messages/"+msgID, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestDeleteMessageNotAuthor(t *testing.T) {
	ts, _, serverID, _, msgID := setupWithMessage(t)

	other := ts.CreateTestUser(t, "msgdeleter")
	ts.AddMemberToServer(t, serverID, other.ID, "member")

	w := ts.DoRequest("DELETE", "/api/v1/messages/"+msgID, nil, testhelpers.AuthHeaders(other.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestDeleteMessageAsAdmin(t *testing.T) {
	ts, _, serverID, _, msgID := setupWithMessage(t)

	admin := ts.CreateTestUser(t, "msgadmin")
	ts.AddMemberToServer(t, serverID, admin.ID, "admin")

	w := ts.DoRequest("DELETE", "/api/v1/messages/"+msgID, nil, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}
