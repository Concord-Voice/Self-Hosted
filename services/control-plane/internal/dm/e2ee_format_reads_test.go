package dm_test

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// markDMMessageV1 stores the passive message-v1 format (with the fingerprint
// the migration 000201 checks require) on an existing DM message.
func markDMMessageV1(t *testing.T, ts *testhelpers.TestServer, messageID string) {
	t.Helper()
	_, err := ts.DB.Exec(`UPDATE dm_messages SET type = 'user', e2ee_format = 'message-v1', request_fingerprint = $2 WHERE id = $1`,
		messageID, bytes.Repeat([]byte{0x4d}, 32))
	require.NoError(t, err)
}

func newFormatConversation(t *testing.T, ts *testhelpers.TestServer, name string) (testhelpers.TestUser, string) {
	t.Helper()
	u1 := ts.CreateTestUser(t, name+"a")
	u2 := ts.CreateTestUser(t, name+"b")
	ts.CreateFriendship(t, u1.ID, u2.ID, statusAccepted)
	return u1, ts.CreateDMConversation(t, u1.ID, u2.ID)
}

func TestDMLastMessageExposesStoredFormat(t *testing.T) {
	ts := setupTS(t)
	user, convID := newFormatConversation(t, ts, "dmlmfmt")
	msgID := insertDMMessage(t, ts, convID, user.ID, "preview")

	for _, endpoint := range []string{"list", "single"} {
		lm := dmLastMessageJSON(t, ts, user.AccessToken, convID, endpoint)
		require.NotNil(t, lm, endpoint)
		assert.NotContains(t, lm, "e2ee_format", "legacy preview on %s", endpoint)
	}

	markDMMessageV1(t, ts, msgID)
	for _, endpoint := range []string{"list", "single"} {
		lm := dmLastMessageJSON(t, ts, user.AccessToken, convID, endpoint)
		require.NotNil(t, lm, endpoint)
		assert.Equal(t, "message-v1", lm["e2ee_format"], "message-v1 preview on %s", endpoint)
		assert.NotContains(t, lm, "request_fingerprint", endpoint)
	}
}

func TestDMPinsExposeStoredFormat(t *testing.T) {
	ts := setupTS(t)
	user, convID := newFormatConversation(t, ts, "dmpinfmt")
	v1ID := insertDMMessage(t, ts, convID, user.ID, "v1 pin")
	legacyID := insertDMMessage(t, ts, convID, user.ID, "legacy pin")
	markDMMessageV1(t, ts, v1ID)
	_, err := ts.DB.Exec(`UPDATE dm_messages SET pinned_at = NOW(), pinned_by = $2 WHERE id = ANY($1::uuid[])`,
		"{"+v1ID+","+legacyID+"}", user.ID)
	require.NoError(t, err)

	w := ts.DoRequest("GET", "/api/v1/channels/"+convID+"/pins", nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var body struct {
		Pinned []map[string]any `json:"pinned_messages"`
	}
	testhelpers.ParseJSON(t, w, &body)
	require.Len(t, body.Pinned, 2)
	byID := map[string]map[string]any{}
	for _, m := range body.Pinned {
		id, ok := m["id"].(string)
		require.True(t, ok)
		byID[id] = m
	}
	assert.Equal(t, "message-v1", byID[v1ID]["e2ee_format"])
	assert.NotContains(t, byID[legacyID], "e2ee_format")
	assert.NotContains(t, byID[v1ID], "request_fingerprint")
}

func TestDMUpdateMessageReturnsStoredFormat(t *testing.T) {
	ts := setupTS(t)
	user, convID := newFormatConversation(t, ts, "dmeditfmt")
	msgID := insertDMMessage(t, ts, convID, user.ID, "original")
	markDMMessageV1(t, ts, msgID)

	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+convID+pathMsgSlash+msgID,
		map[string]interface{}{"content": testhelpers.ValidCiphertext(), "key_version": 1},
		testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body map[string]map[string]any
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "message-v1", body["message"]["e2ee_format"])
	assert.NotContains(t, body["message"], "request_fingerprint")
}
