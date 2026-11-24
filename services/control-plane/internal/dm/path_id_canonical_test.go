package dm_test

import (
	"encoding/json"
	"net/http/httptest"
	"time"

	"github.com/google/uuid"
	gorillaWS "github.com/gorilla/websocket"
	"net/http"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func participantCount(t *testing.T, ts *testhelpers.TestServer, convID, userID string) int {
	t.Helper()
	var n int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`,
		convID, userID).Scan(&n))
	return n
}

func TestRemoveMember_CreatorAlternateSpelling_403(t *testing.T) {
	ts := setupTS(t)
	creator := ts.CreateTestUser(t, "canoncreator")
	admin := ts.CreateTestUser(t, "canonadmin")
	convID := createTestGroup(t, ts, creator, admin)
	_, err := ts.DB.Exec(`UPDATE dm_participants SET role = 'admin' WHERE conversation_id = $1 AND user_id = $2`, convID, admin.ID)
	require.NoError(t, err)

	// Positive control: the canonical spelling is refused for the creator.
	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+"/members/"+creator.ID, nil, testhelpers.AuthHeaders(admin.AccessToken))
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "Cannot remove the group creator")
	require.Equal(t, 1, participantCount(t, ts, convID, creator.ID))

	upper := strings.ToUpper(creator.ID)
	require.NotEqual(t, creator.ID, upper, "uuid must contain a hex letter for the spelling to differ")
	w = ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+"/members/"+upper, nil, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "Cannot remove the group creator")
	assert.Equal(t, 1, participantCount(t, ts, convID, creator.ID), "creator row must survive")
}

func TestRemoveMember_SelfLeaveAlternateSpelling_TreatedAsSelf(t *testing.T) {
	ts := setupTS(t)
	creator := ts.CreateTestUser(t, "canonselfcreator")
	member := ts.CreateTestUser(t, "canonselfmember")
	convID := createTestGroup(t, ts, creator, member)

	upper := strings.ToUpper(member.ID)
	require.NotEqual(t, member.ID, upper)
	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+convID+"/members/"+upper, nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code, "a non-admin leaving under an alternate spelling is a self-leave: %s", w.Body.String())
	assert.Zero(t, participantCount(t, ts, convID, member.ID))
}

func TestDMGroupAdmin_AlternateSpelling_SameAsCanonical(t *testing.T) {
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "canonmuteadmin")
	target := ts.CreateTestUser(t, "canonmutetarget")
	convID := createTestGroup(t, ts, admin, target)

	ticketResponse := ts.DoRequest(http.MethodPost, "/api/v1/auth/ws-ticket", nil, testhelpers.AuthHeaders(admin.AccessToken))
	require.Equal(t, http.StatusOK, ticketResponse.Code, ticketResponse.Body.String())
	var ticketBody struct {
		Ticket string `json:"ticket"`
	}
	testhelpers.ParseJSON(t, ticketResponse, &ticketBody)
	wsServer := httptest.NewServer(ts.Router)
	t.Cleanup(wsServer.Close)
	conn, _, err := gorillaWS.DefaultDialer.Dial("ws"+wsServer.URL[4:]+"/api/v1/ws?ticket="+ticketBody.Ticket, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.Eventually(t, func() bool {
		return ts.Hub.GetUserClientCount(uuid.MustParse(admin.ID)) > 0
	}, time.Second, 10*time.Millisecond)

	require.NoError(t, conn.WriteJSON(map[string]interface{}{"type": "subscribe_dm", "data": map[string]interface{}{"conversation_id": convID}}))
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	for {
		_, payload, readErr := conn.ReadMessage()
		require.NoError(t, readErr, "admin must be subscribed to the conversation")
		if strings.Contains(string(payload), "dm_subscribed") {
			break
		}
	}

	upper := strings.ToUpper(target.ID)
	require.NotEqual(t, target.ID, upper)
	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathVoiceSlash+upper+pathMute, nil, testhelpers.AuthHeaders(admin.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var muted bool
	require.NoError(t, ts.DB.QueryRow(`SELECT server_muted FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, convID, target.ID).Scan(&muted))
	assert.True(t, muted)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	for {
		_, payload, readErr := conn.ReadMessage()
		require.NoError(t, readErr, "admin must receive the voice state update")
		var event struct {
			Type string                 `json:"type"`
			Data map[string]interface{} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(payload, &event))
		if event.Type != "dm_voice_state_update" {
			continue
		}
		assert.Equal(t, convID, event.Data["conversation_id"])
		assert.Equal(t, target.ID, event.Data["user_id"], "emitted ID must be the canonical spelling")
		break
	}
}
