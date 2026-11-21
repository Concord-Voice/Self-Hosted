package voice_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Security#174 VULN-111: a DM voice.room_empty writes call history only for the
// call that owns the conversation's live lease, and only when every person it
// names is a participant of that conversation. The lease check runs before the
// history write, under the same DM call lifecycle lock.

type dmRoomEmptyFixture struct {
	ts             *testhelpers.TestServer
	subscriber     *voice.NATSSubscriber
	hooks          *atomic.Int32
	conversationID string
	caller         testhelpers.TestUser
	peer           testhelpers.TestUser
}

func newDMRoomEmptyFixture(t *testing.T) dmRoomEmptyFixture {
	t.Helper()
	return newDMRoomEmptyFixtureNamed(t, "dm_binding")
}

func newDMRoomEmptyFixtureNamed(t *testing.T, prefix string) dmRoomEmptyFixture {
	t.Helper()
	ts := testhelpers.SetupTestServer(t)
	caller := ts.CreateTestUser(t, prefix+"_caller")
	peer := ts.CreateTestUser(t, prefix+"_peer")
	subscriber := newTestSubscriber(ts)
	hooks := &atomic.Int32{}
	subscriber.SetDisconnectAllRichPresenceClientsHookForTest(func() { hooks.Add(1) })
	return dmRoomEmptyFixture{
		ts: ts, subscriber: subscriber, hooks: hooks,
		conversationID: ts.CreateDMConversation(t, caller.ID, peer.ID),
		caller:         caller, peer: peer,
	}
}

func (f dmRoomEmptyFixture) lease(t *testing.T, callID uuid.UUID) {
	t.Helper()
	require.NoError(t, dm.RefreshDMVoiceCallLease(
		context.Background(), f.ts.Redis,
		dm.VoiceCallLease{
			ConversationID: uuid.MustParse(f.conversationID),
			CallID:         callID,
			CallerUserID:   uuid.MustParse(f.caller.ID),
		},
		dm.DMVoiceCallLeaseTTL,
		true,
	))
}

func (f dmRoomEmptyFixture) roomEmpty(t *testing.T, callID uuid.UUID, participants ...string) []byte {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	return mustJSON(t, map[string]interface{}{
		"channelId": f.conversationID, "callId": callID.String(),
		"callerUserId": f.caller.ID, "participantUserIds": participants,
		"startedAt": now.Add(-time.Minute).Format(time.RFC3339Nano),
		"timestamp": now.Format(time.RFC3339Nano),
	})
}

func (f dmRoomEmptyFixture) history(t *testing.T, callID uuid.UUID) int {
	t.Helper()
	var count int
	require.NoError(t, f.ts.DB.QueryRow(
		`SELECT COUNT(*) FROM dm_messages WHERE id = $1`, callID,
	).Scan(&count))
	return count
}

func TestDMRoomEmptyUnknownCallWritesNoHistory(t *testing.T) {
	f := newDMRoomEmptyFixture(t)
	f.lease(t, uuid.New())
	unknown := uuid.New()

	f.subscriber.HandleRoomEmpty(f.roomEmpty(t, unknown, f.caller.ID, f.peer.ID))

	assert.Zero(t, f.history(t, unknown), "a room_empty for a call that does not own the lease wrote history")
	assert.Zero(t, f.hooks.Load())
}

func TestDMRoomEmptyStrangerParticipantWritesNoHistory(t *testing.T) {
	// Control: the same terminal with an honest summary. Its disconnect count
	// (the already-empty-room reconnect) is the baseline the mismatch must not
	// exceed.
	control := newDMRoomEmptyFixtureNamed(t, "dm_binding_control")
	controlCall := uuid.New()
	control.lease(t, controlCall)
	control.subscriber.HandleRoomEmpty(control.roomEmpty(t, controlCall, control.caller.ID, control.peer.ID))
	require.Equal(t, 1, control.history(t, controlCall), "the control summary must be written")

	f := newDMRoomEmptyFixture(t)
	stranger := f.ts.CreateTestUser(t, "dm_binding_stranger")
	callID := uuid.New()
	f.lease(t, callID)

	f.subscriber.HandleRoomEmpty(f.roomEmpty(t, callID, f.caller.ID, stranger.ID))

	assert.Zero(t, f.history(t, callID), "a summary naming a non-participant wrote history")
	assert.Equal(t, control.hooks.Load(), f.hooks.Load(),
		"participant_mismatch is a determined no-op and must add no disconnect")
}

func TestDMRoomEmptyLeaseCheckRunsBeforePersist(t *testing.T) {
	f := newDMRoomEmptyFixture(t)
	callID := uuid.New()
	f.lease(t, callID)
	// The lease cannot be read: the room_empty must stop before the history write.
	require.NoError(t, f.ts.Redis.Close())

	f.subscriber.HandleRoomEmpty(f.roomEmpty(t, callID, f.caller.ID, f.peer.ID))

	assert.Zero(t, f.history(t, callID), "history was written before the lease check")
}

func TestDMLeftReturnsBeforeMutationWhenLeaseFails(t *testing.T) {
	f := newDMRoomEmptyFixture(t)
	callID := uuid.New()
	joinedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	_, err := f.ts.DB.Exec(`
		INSERT INTO dm_voice_participants (conversation_id, user_id, joined_at, lifecycle_event_at)
		VALUES ($1, $2, $3, $3)
	`, f.conversationID, f.caller.ID, joinedAt)
	require.NoError(t, err)
	require.NoError(t, f.ts.Redis.Close())

	f.subscriber.HandleLeft(mustJSON(t, map[string]interface{}{
		"channelId": f.conversationID, "callId": callID.String(), "userId": f.caller.ID,
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
	}))

	var rows int
	require.NoError(t, f.ts.DB.QueryRow(`
		SELECT COUNT(*) FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2
	`, f.conversationID, f.caller.ID).Scan(&rows))
	assert.Equal(t, 1, rows, "voice.left mutated the DM roster although the lease could not be read")
}
