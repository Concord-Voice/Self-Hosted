package voice_test

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Two control-plane processes run during a blue-green cutover (#253). Core NATS
// hands both the same voice.joined, so the second applier sees an exact
// duplicate whose pending admission the first already consumed. That is a
// replay, not an admission mismatch: it must not eject the peer that just
// joined. Before the fix, every admitted join during the overlap was kicked.

type duplicateJoinFixture struct {
	ts          *testhelpers.TestServer
	publisher   *natsclient.Client
	disconnects chan map[string]interface{}
	channelID   string
	userID      string
	admissionID uuid.UUID
}

func newDuplicateJoinFixture(t *testing.T) duplicateJoinFixture {
	t.Helper()
	publisher, err := natsclient.Connect(natsTestURL())
	if err != nil {
		t.Skipf("NATS unavailable (%v); skipping duplicate-join replay test", err)
	}
	t.Cleanup(func() { _ = publisher.Close() })
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "dup_join_replay_user")
	serverID := ts.CreateTestServer(t, user.ID, "Dup Join Replay")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-dup-replay")

	// Keep only this test's user, so a late disconnect from another test on the
	// shared subject cannot land in the window. Never block the NATS callback.
	observer, err := natsclient.Connect(natsTestURL())
	require.NoError(t, err)
	t.Cleanup(func() { _ = observer.Close() })
	disconnects := make(chan map[string]interface{}, 4)
	subscription, err := observer.Subscribe(natsSubjectEnforceDisconnectForTest, func(data []byte) {
		var payload map[string]interface{}
		if json.Unmarshal(data, &payload) != nil || payload["userId"] != user.ID {
			return
		}
		select {
		case disconnects <- payload:
		default:
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	require.NoError(t, observer.Flush())
	return duplicateJoinFixture{
		ts: ts, publisher: publisher, disconnects: disconnects,
		channelID: channelID, userID: user.ID, admissionID: uuid.New(),
	}
}

func (f duplicateJoinFixture) reserve(t *testing.T) {
	t.Helper()
	_, err := f.ts.DB.Exec(`
		INSERT INTO voice_pending_admissions (channel_id, user_id, admission_id, socket_id, expires_at)
		VALUES ($1, $2, $3, 'socket-dup-replay', clock_timestamp() + INTERVAL '1 minute')
	`, f.channelID, f.userID, f.admissionID)
	require.NoError(t, err)
}

func (f duplicateJoinFixture) joinedEvent(t *testing.T, at time.Time) []byte {
	t.Helper()
	return f.joinedEventFor(t, f.admissionID, at)
}

func (f duplicateJoinFixture) joinedEventFor(t *testing.T, admissionID uuid.UUID, at time.Time) []byte {
	t.Helper()
	return f.joinedEventOn(t, admissionID, "socket-dup-replay", at)
}

func (f duplicateJoinFixture) joinedEventOn(t *testing.T, admissionID uuid.UUID, socketID string, at time.Time) []byte {
	t.Helper()
	return mustJSON(t, map[string]interface{}{
		"channelId": f.channelID, "userId": f.userID, "username": "dup_join_replay_user",
		"admissionId": admissionID.String(), "socketId": socketID,
		"timestamp": at.Format(time.RFC3339Nano),
	})
}

// awaitDisconnect reads the exact-disconnect subject for a bounded window. The
// publish is synchronous inside the handler, so flushing the publisher first
// makes the window cover it.
func (f duplicateJoinFixture) awaitDisconnect(t *testing.T) (map[string]interface{}, bool) {
	t.Helper()
	require.NoError(t, f.publisher.Flush())
	select {
	case payload := <-f.disconnects:
		return payload, true
	case <-time.After(500 * time.Millisecond):
		return nil, false
	}
}

func TestHandleJoined_DuplicateAcrossProcessesIsReplayNotEjection(t *testing.T) {
	f := newDuplicateJoinFixture(t)
	f.reserve(t)
	home := newTestSubscriberWithHubAndNATS(f.ts, f.ts.Hub, f.publisher)
	green := newTestSubscriberWithHubAndNATS(f.ts, f.ts.Hub, f.publisher)
	now := time.Now().UTC().Truncate(time.Microsecond)
	event := f.joinedEvent(t, now)

	home.HandleJoinedAt(event, now)
	green.HandleJoinedAt(event, now.Add(2*time.Millisecond))

	payload, ejected := f.awaitDisconnect(t)
	assert.False(t, ejected, "duplicate voice.joined ejected the peer: %v", payload)
	assert.Equal(t, 1, countVoiceParticipants(t, f.ts.DB, f.channelID))
	var pending int
	require.NoError(t, f.ts.DB.QueryRow(
		`SELECT COUNT(*) FROM voice_pending_admissions WHERE channel_id = $1 AND user_id = $2`,
		f.channelID, f.userID,
	).Scan(&pending))
	assert.Zero(t, pending)
}

// Positive control for the reader above, and the boundary of the replay rule:
// a join whose admission the participant row does NOT hold, arriving with no
// reservation, is still a mismatch and still ejects that exact peer.
func TestHandleJoined_ConsumedAdmissionWithoutCommittedJoinStillEjects(t *testing.T) {
	f := newDuplicateJoinFixture(t)
	f.reserve(t)
	home := newTestSubscriberWithHubAndNATS(f.ts, f.ts.Hub, f.publisher)
	now := time.Now().UTC().Truncate(time.Microsecond)

	home.HandleJoinedAt(f.joinedEvent(t, now), now)
	_, ejected := f.awaitDisconnect(t)
	require.False(t, ejected, "the first admitted join must not eject")

	later := now.Add(time.Second)
	unreserved := uuid.New()
	home.HandleJoinedAt(f.joinedEventFor(t, unreserved, later), later)
	payload, ejected := f.awaitDisconnect(t)
	require.True(t, ejected, "a join whose admission the row does not hold must eject")
	assert.Equal(t, f.userID, payload["userId"])
	assert.Equal(t, "socket-dup-replay", payload["socketId"])
	assert.Equal(t, unreserved.String(), payload["admissionId"])
}

// The other half of the replay rule. A first attempt can claim the lifecycle
// watermark in Redis and then roll back, leaving the reservation in place and
// no row. Its redelivery claims as a duplicate, but it is the FIRST commit of
// that join, so it must still consume the reservation in its own transaction.
func TestHandleJoined_DuplicateClaimAfterRolledBackAttemptStillConsumesAdmission(t *testing.T) {
	f := newDuplicateJoinFixture(t)
	f.reserve(t)
	home := newTestSubscriberWithHubAndNATS(f.ts, f.ts.Hub, f.publisher)
	now := time.Now().UTC().Truncate(time.Microsecond)
	key, err := presence.VoiceLifecycleKey(uuid.MustParse(f.userID), presence.CategoryServerVoice)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, f.ts.Redis.HSet(ctx, key,
		"token", f.channelID, "version", strconv.FormatInt(now.UnixMicro(), 10), "active", "1",
	).Err())
	require.NoError(t, f.ts.Redis.PExpire(ctx, key, presence.ActivityStateTTL).Err())

	home.HandleJoinedAt(f.joinedEvent(t, now), now)

	payload, ejected := f.awaitDisconnect(t)
	assert.False(t, ejected, "first commit of a duplicate-claimed join ejected the peer: %v", payload)
	assert.Equal(t, 1, countVoiceParticipants(t, f.ts.DB, f.channelID))
	var pending int
	require.NoError(t, f.ts.DB.QueryRow(
		`SELECT COUNT(*) FROM voice_pending_admissions WHERE channel_id = $1 AND user_id = $2`,
		f.channelID, f.userID,
	).Scan(&pending))
	assert.Zero(t, pending, "the join committed without consuming its reservation")
}

// A row can carry a join's stamp without that join having committed, for
// example a heartbeat clamped to the same receipt microsecond. The join must
// still consume its own valid reservation rather than leave it to expire, and
// a matching stamp alone must never stand in for the admission.
func TestHandleJoined_StampCollisionStillConsumesValidAdmission(t *testing.T) {
	f := newDuplicateJoinFixture(t)
	f.reserve(t)
	home := newTestSubscriberWithHubAndNATS(f.ts, f.ts.Hub, f.publisher)
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, err := f.ts.DB.Exec(`
		INSERT INTO voice_participants (channel_id, user_id, lifecycle_event_at)
		VALUES ($1, $2, $3)
	`, f.channelID, f.userID, now)
	require.NoError(t, err)

	home.HandleJoinedAt(f.joinedEvent(t, now), now)

	payload, ejected := f.awaitDisconnect(t)
	assert.False(t, ejected, "a valid admission was rejected: %v", payload)
	var pending int
	require.NoError(t, f.ts.DB.QueryRow(
		`SELECT COUNT(*) FROM voice_pending_admissions WHERE channel_id = $1 AND user_id = $2`,
		f.channelID, f.userID,
	).Scan(&pending))
	assert.Zero(t, pending, "a stamp collision stranded a valid reservation")
}

// V3 in the #253 overlap analysis: a producer stamp ahead of receipt is clamped
// to each control plane's own receipt time, so the two deliveries of one join
// carry different stamps. The row holds the join's admission, so the second
// delivery is still a replay and must not eject.
func TestHandleJoined_ClampedStampsAcrossProcessesAreStillReplay(t *testing.T) {
	f := newDuplicateJoinFixture(t)
	f.reserve(t)
	home := newTestSubscriberWithHubAndNATS(f.ts, f.ts.Hub, f.publisher)
	green := newTestSubscriberWithHubAndNATS(f.ts, f.ts.Hub, f.publisher)
	now := time.Now().UTC().Truncate(time.Microsecond)
	event := f.joinedEvent(t, now.Add(time.Second))

	home.HandleJoinedAt(event, now)
	green.HandleJoinedAt(event, now.Add(2*time.Millisecond))

	payload, ejected := f.awaitDisconnect(t)
	assert.False(t, ejected, "a clamped duplicate voice.joined ejected the peer: %v", payload)
	assert.Equal(t, 1, countVoiceParticipants(t, f.ts.DB, f.channelID))
	var stored uuid.NullUUID
	require.NoError(t, f.ts.DB.QueryRow(
		`SELECT admission_id FROM voice_participants WHERE channel_id = $1 AND user_id = $2`,
		f.channelID, f.userID,
	).Scan(&stored))
	assert.Equal(t, uuid.NullUUID{UUID: f.admissionID, Valid: true}, stored)
}

// The replay key is the exact (admission, socket) pair the consume matched. A
// join that reuses a committed admission on another socket is not a replay of
// that join, so it still ejects that exact socket.
func TestHandleJoined_CommittedAdmissionOnAnotherSocketStillEjects(t *testing.T) {
	f := newDuplicateJoinFixture(t)
	f.reserve(t)
	home := newTestSubscriberWithHubAndNATS(f.ts, f.ts.Hub, f.publisher)
	now := time.Now().UTC().Truncate(time.Microsecond)
	home.HandleJoinedAt(f.joinedEvent(t, now), now)
	_, ejected := f.awaitDisconnect(t)
	require.False(t, ejected, "the first admitted join must not eject")

	later := now.Add(time.Second)
	home.HandleJoinedAt(f.joinedEventOn(t, f.admissionID, "socket-other", later), later)
	payload, ejected := f.awaitDisconnect(t)
	require.True(t, ejected, "a committed admission on another socket must eject")
	assert.Equal(t, "socket-other", payload["socketId"])
	assert.Equal(t, f.admissionID.String(), payload["admissionId"])
}
