package dm

import (
	"context"
	"testing"
	"time"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Security#174 VULN-111: a completed-call row is history in a conversation, so
// its caller and every participant it names must be participants of that
// conversation. A summary naming anyone else writes nothing.
func TestInsertCompletedCallEventRefusesNonParticipant(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	convID, caller, peer := seedHiddenConv(t, db)
	conversation := uuid.MustParse(convID)
	callerID := uuid.MustParse(caller)
	peerID := uuid.MustParse(peer)
	strangerID := uuid.MustParse(seedHiddenUser(t, db))
	now := time.Now().UTC().Truncate(time.Microsecond)

	cases := []struct {
		name         string
		caller       uuid.UUID
		participants []uuid.UUID
	}{
		{name: "stranger participant", caller: callerID, participants: []uuid.UUID{callerID, strangerID}},
		{name: "stranger caller", caller: strangerID, participants: []uuid.UUID{strangerID, peerID}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			callID := uuid.New()
			_, err := InsertCompletedCallEvent(context.Background(), db, conversation, CompletedCallSummary{
				CallID: callID, CallerUserID: tc.caller, ParticipantUserIDs: tc.participants,
				StartedAt: now, EndedAt: now.Add(time.Second),
			})
			require.ErrorIs(t, err, ErrCallParticipantMismatch)
			var count int
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_messages WHERE id = $1`, callID).Scan(&count))
			assert.Zero(t, count, "a summary naming a non-participant wrote history")
		})
	}

	callID := uuid.New()
	_, err := InsertCompletedCallEvent(context.Background(), db, conversation, CompletedCallSummary{
		CallID: callID, CallerUserID: callerID, ParticipantUserIDs: []uuid.UUID{callerID, peerID},
		StartedAt: now, EndedAt: now.Add(time.Second),
	})
	require.NoError(t, err, "the participants' own summary must still be written")
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_messages WHERE id = $1`, callID).Scan(&count))
	assert.Equal(t, 1, count)
}
