package dm_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
)

// TestPurgeConversation_GroupNonAdminDeletesOwnHidesOthers pins the group
// non-admin branch of spec §5: a plain member deletes only their own messages
// for everyone and hides the rest from their own view, recorded as a group
// purge.
func TestPurgeConversation_GroupNonAdminDeletesOwnHidesOthers(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	creator := ts.CreateTestUser(t, "grpna_creator")
	member := ts.CreateTestUser(t, "grpna_member")
	other := ts.CreateTestUser(t, "grpna_other")
	convID := ts.CreateGroupDMConversation(t, creator.ID, member.ID, other.ID)
	_, err := ts.DB.Exec(
		`UPDATE dm_participants SET role = 'member' WHERE conversation_id = $1 AND user_id = $2`,
		convID, member.ID)
	require.NoError(t, err)

	insertDMMsg(t, ts, convID, creator.ID, "creator-msg")
	insertDMMsg(t, ts, convID, member.ID, "member-msg")
	insertDMMsg(t, ts, convID, other.ID, "other-msg")

	w := ts.DoRequest(http.MethodDelete, purgeConvPath(convID),
		map[string]any{"range": "all", "current_password": member.Password},
		testhelpers.AuthHeaders(member.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, map[string]any{"deleted_count": float64(1), "hidden_count": float64(2)}, resp)
	assert.Equal(t, 2, countDMMessages(t, ts, convID), "only the member's own message is deleted for everyone")

	assert.Empty(t, fetchVisibleMessages(t, ts, convID, member.AccessToken),
		"the other members' messages are hidden from the actor")
	assert.ElementsMatch(t, []string{"creator-msg", "other-msg"},
		fetchVisibleMessages(t, ts, convID, other.AccessToken),
		"the hide applies to the actor's view only")

	var ctxType string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT context_type FROM message_purges WHERE context_id = $1`, convID).Scan(&ctxType))
	assert.Equal(t, "group", ctxType)
}
