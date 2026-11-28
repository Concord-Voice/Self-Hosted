package channels_test

import (
	"net/http"
	"testing"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/reservationquota"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelExpirationQuotaRefusalKeepsPolicy(t *testing.T) {
	ts, owner, _, channelID := setupWithChannel(t)
	var revisionBefore, reservationsBefore int
	require.NoError(t, ts.DB.QueryRow(`SELECT expiration_revision FROM channels WHERE id = $1`, channelID).Scan(&revisionBefore))
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM message_id_reservations`).Scan(&reservationsBefore))
	_, err := ts.DB.Exec(`UPDATE message_id_reservation_quota SET max_issued = issued`)
	require.NoError(t, err)

	w := ts.DoRequest(http.MethodPatch, pathChannelsPrefix+channelID+"/expiration",
		map[string]any{"mode": "set", "window_seconds": 3600, "retroactive": "new_only"},
		testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusInsufficientStorage, w.Code, w.Body.String())
	var body map[string]any
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, reservationquota.Code, body["code"])
	assert.Equal(t, reservationquota.Message, body["error"])

	var revisionAfter, reservationsAfter, events int
	require.NoError(t, ts.DB.QueryRow(`SELECT expiration_revision FROM channels WHERE id = $1`, channelID).Scan(&revisionAfter))
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM message_id_reservations`).Scan(&reservationsAfter))
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM messages WHERE channel_id = $1 AND type = 'expiration_event'`, channelID).Scan(&events))
	assert.Equal(t, revisionBefore, revisionAfter)
	assert.Equal(t, reservationsBefore, reservationsAfter)
	assert.Zero(t, events)
}
