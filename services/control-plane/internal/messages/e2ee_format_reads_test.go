package messages_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/models"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// markMessageV1 stores the passive message-v1 format (with the fingerprint the
// migration 000201 checks require) on an existing channel message.
func markMessageV1(t *testing.T, ts *testhelpers.TestServer, messageID string) {
	t.Helper()
	_, err := ts.DB.Exec(`UPDATE messages SET e2ee_format = 'message-v1', request_fingerprint = $2 WHERE id = $1`,
		messageID, bytes.Repeat([]byte{0x3c}, 32))
	require.NoError(t, err)
}

func TestGetChannelPinsExposesStoredFormat(t *testing.T) {
	ts, user, _, channelID, messageID := setupWithMessage(t)
	legacyID := ts.CreateTestMessage(t, channelID, user, "legacy pin")
	markMessageV1(t, ts, messageID)
	_, err := ts.DB.Exec(`UPDATE messages SET pinned_at = NOW(), pinned_by = $2 WHERE id = ANY($1::uuid[])`,
		"{"+messageID+","+legacyID+"}", user.ID)
	require.NoError(t, err)

	w := ts.DoRequest("GET", "/api/v1/channels/"+channelID+"/pins", nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var body struct {
		Pinned []map[string]any `json:"pinned_messages"`
	}
	testhelpers.ParseJSON(t, w, &body)
	require.Len(t, body.Pinned, 2)
	byID := map[string]map[string]any{}
	for _, m := range body.Pinned {
		byID[m["id"].(string)] = m
	}
	assert.Equal(t, "message-v1", byID[messageID]["e2ee_format"])
	assert.NotContains(t, byID[legacyID], "e2ee_format")
	assert.NotContains(t, byID[messageID], "request_fingerprint")
}

func TestGetMessagesReplyPreviewExposesStoredFormat(t *testing.T) {
	ts, user, _, channelID, originalID := setupWithMessage(t)
	markMessageV1(t, ts, originalID)

	w := ts.DoRequest("POST", sendMessagePath, map[string]interface{}{
		"channel_id":  channelID,
		"content":     testhelpers.ValidCiphertext(),
		"reply_to_id": originalID,
		"key_version": 1,
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	w = ts.DoRequest("GET", "/api/v1/channels/"+channelID+"/messages", nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Messages []json.RawMessage `json:"messages"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	require.GreaterOrEqual(t, len(resp.Messages), 2)
	var reply struct {
		RepliedTo *models.RepliedToSummary `json:"replied_to"`
	}
	require.NoError(t, json.Unmarshal(resp.Messages[0], &reply))
	require.NotNil(t, reply.RepliedTo)
	require.NotNil(t, reply.RepliedTo.E2EEFormat)
	assert.Equal(t, "message-v1", *reply.RepliedTo.E2EEFormat)
	assert.NotContains(t, string(resp.Messages[0]), "request_fingerprint")
}

func TestUpdateMessageReturnsStoredFormat(t *testing.T) {
	ts, user, _, _, messageID := setupWithMessage(t)
	markMessageV1(t, ts, messageID)

	w := ts.DoRequest("PATCH", "/api/v1/messages/"+messageID, map[string]interface{}{
		"content":     testhelpers.ValidCiphertext(),
		"key_version": 1,
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var body map[string]map[string]any
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "message-v1", body["message"]["e2ee_format"])
	assert.NotContains(t, body["message"], "request_fingerprint")
}
