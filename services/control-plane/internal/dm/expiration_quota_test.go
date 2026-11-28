package dm_test

import (
	"net/http"
	"testing"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/reservationquota"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDMExpirationQuotaRefusalKeepsPolicy(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "quota_exp_alice")
	bob := ts.CreateTestUser(t, "quota_exp_bob")
	conversationID := ts.CreateDMConversation(t, alice.ID, bob.ID)
	var revisionBefore, reservationsBefore int
	require.NoError(t, ts.DB.QueryRow(`SELECT expiration_revision FROM dm_conversations WHERE id = $1`, conversationID).Scan(&revisionBefore))
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM message_id_reservations`).Scan(&reservationsBefore))
	_, err := ts.DB.Exec(`UPDATE message_id_reservation_quota SET max_issued = issued`)
	require.NoError(t, err)

	w := ts.DoRequest(http.MethodPatch, dmExpirationPath+conversationID+"/expiration",
		map[string]any{"mode": "set", "window_seconds": 3600, "retroactive": "new_only"},
		testhelpers.AuthHeaders(alice.AccessToken))
	require.Equal(t, http.StatusInsufficientStorage, w.Code, w.Body.String())
	var body map[string]any
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, reservationquota.Code, body["code"])
	assert.Equal(t, reservationquota.Message, body["error"])

	var revisionAfter, reservationsAfter, events int
	require.NoError(t, ts.DB.QueryRow(`SELECT expiration_revision FROM dm_conversations WHERE id = $1`, conversationID).Scan(&revisionAfter))
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM message_id_reservations`).Scan(&reservationsAfter))
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM dm_messages WHERE conversation_id = $1 AND type = 'expiration_event'`, conversationID).Scan(&events))
	assert.Equal(t, revisionBefore, revisionAfter)
	assert.Equal(t, reservationsBefore, reservationsAfter)
	assert.Zero(t, events)
}
