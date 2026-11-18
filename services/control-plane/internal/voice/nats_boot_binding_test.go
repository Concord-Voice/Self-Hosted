package voice_test

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Security#174 §5.6: every Server Voice write is bound to the media boot that
// produced it. These drive the handlers directly with an explicit NATS receipt
// time; the ingress gate that will prove the boot is not yet enforcing.

type bootBindingFixture struct {
	ts         *testhelpers.TestServer
	subscriber *voice.NATSSubscriber
	hooks      *atomic.Int32
	serverID   string
	channelID  string
	owner      testhelpers.TestUser
	bootX      uuid.UUID
	bootY      uuid.UUID
}

func newBootBindingFixture(t *testing.T) bootBindingFixture {
	t.Helper()
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "boot_binding_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "Boot Binding")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-boot-binding")
	subscriber := newTestSubscriber(ts)
	hooks := &atomic.Int32{}
	subscriber.SetDisconnectAllRichPresenceClientsHookForTest(func() { hooks.Add(1) })
	return bootBindingFixture{
		ts: ts, subscriber: subscriber, hooks: hooks, serverID: serverID,
		channelID: channelID, owner: owner, bootX: uuid.New(), bootY: uuid.New(),
	}
}

func (f bootBindingFixture) member(t *testing.T, name string) testhelpers.TestUser {
	t.Helper()
	user := f.ts.CreateTestUser(t, name)
	f.ts.AddMemberToServer(t, f.serverID, user.ID, "member")
	return user
}

// seedRow plants a row stamped with boot (nil boot: unstamped) whose lease was
// last observed observedAgo before now.
func (f bootBindingFixture) seedRow(t *testing.T, userID string, boot uuid.UUID, observedAgo time.Duration) {
	t.Helper()
	var stamp interface{}
	if boot != uuid.Nil {
		stamp = boot
	}
	at := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Microsecond)
	_, err := f.ts.DB.Exec(`
		INSERT INTO voice_participants (channel_id, user_id, joined_at, lifecycle_event_at, media_boot_id)
		VALUES ($1, $2, $3, $3, $4)
	`, f.channelID, userID, at, stamp)
	require.NoError(t, err)
	f.setObservedAgo(t, userID, observedAgo)
}

// setObservedAgo writes the lease column without naming lifecycle_event_at, so
// the 000132/000133 trigger leaves the value as written.
func (f bootBindingFixture) setObservedAgo(t *testing.T, userID string, ago time.Duration) {
	t.Helper()
	_, err := f.ts.DB.Exec(`
		UPDATE voice_participants
		SET lifecycle_observed_at = clock_timestamp() - ($3::bigint * INTERVAL '1 millisecond')
		WHERE channel_id = $1 AND user_id = $2
	`, f.channelID, userID, ago.Milliseconds())
	require.NoError(t, err)
}

func (f bootBindingFixture) stamp(t *testing.T, userID string) (uuid.NullUUID, bool) {
	t.Helper()
	var stamp uuid.NullUUID
	err := f.ts.DB.QueryRow(`
		SELECT media_boot_id FROM voice_participants WHERE channel_id = $1 AND user_id = $2
	`, f.channelID, userID).Scan(&stamp)
	if err == sql.ErrNoRows {
		return stamp, false
	}
	require.NoError(t, err)
	return stamp, true
}

func (f bootBindingFixture) observedAt(t *testing.T, userID string) time.Time {
	t.Helper()
	var observed time.Time
	require.NoError(t, f.ts.DB.QueryRow(`
		SELECT lifecycle_observed_at FROM voice_participants WHERE channel_id = $1 AND user_id = $2
	`, f.channelID, userID).Scan(&observed))
	return observed
}

func (f bootBindingFixture) heartbeat(t *testing.T, boot uuid.UUID, at time.Time, userIDs ...string) []byte {
	t.Helper()
	if userIDs == nil {
		userIDs = []string{}
	}
	return mustJSON(t, map[string]interface{}{
		"channelId": f.channelID, "userIds": userIDs, "nodeBootId": boot.String(),
		"participants_authoritative": len(userIDs) > 0,
		"timestamp":                  at.Format(time.RFC3339Nano),
	})
}

func (f bootBindingFixture) left(t *testing.T, boot uuid.UUID, at time.Time, userID string) []byte {
	t.Helper()
	return mustJSON(t, map[string]interface{}{
		"channelId": f.channelID, "userId": userID, "nodeBootId": boot.String(),
		"timestamp": at.Format(time.RFC3339Nano),
	})
}

func (f bootBindingFixture) roomEmpty(t *testing.T, boot uuid.UUID, at time.Time) []byte {
	t.Helper()
	return mustJSON(t, map[string]interface{}{
		"channelId": f.channelID, "nodeBootId": boot.String(),
		"timestamp": at.Format(time.RFC3339Nano),
	})
}

func bootBindingNow() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

func TestHeartbeatClaimsForeignRowOnlyWhenResolvePasses(t *testing.T) {
	f := newBootBindingFixture(t)
	stranger := f.ts.CreateTestUser(t, "boot_binding_stranger")
	denied := f.member(t, "boot_binding_denied")
	f.ts.CreateChannelOverride(t, f.channelID, "user", denied.ID, 0, int64(rbac.PermJoinVoice))
	connect := f.member(t, "boot_binding_connect")
	// connect and denied hold live rows of boot Y.
	f.seedRow(t, denied.ID, f.bootY, 5*time.Second)
	f.seedRow(t, connect.ID, f.bootY, 5*time.Second)

	now := bootBindingNow()
	f.subscriber.HandleHeartbeatAt(
		f.heartbeat(t, f.bootX, now, stranger.ID, denied.ID, connect.ID, f.owner.ID), now,
	)

	_, strangerRow := f.stamp(t, stranger.ID)
	assert.False(t, strangerRow, "a non-member was written by a heartbeat")
	deniedStamp, deniedRow := f.stamp(t, denied.ID)
	require.True(t, deniedRow)
	assert.Equal(t, f.bootY, deniedStamp.UUID, "a member without JOIN_VOICE had boot Y's row claimed")
	connectStamp, connectRow := f.stamp(t, connect.ID)
	require.True(t, connectRow)
	assert.Equal(t, f.bootX, connectStamp.UUID, "a member with CONNECT was not claimed by boot X")
	ownerStamp, ownerRow := f.stamp(t, f.owner.ID)
	require.True(t, ownerRow, "the rest of the heartbeat must still apply")
	assert.Equal(t, f.bootX, ownerStamp.UUID)
	assert.Zero(t, f.hooks.Load(), "a refused claim is a determined no-op, never a disconnect")
}

func TestHeartbeatForeignRowUntouchedKeepsObservedAt(t *testing.T) {
	f := newBootBindingFixture(t)
	denied := f.member(t, "boot_binding_lease_denied")
	f.ts.CreateChannelOverride(t, f.channelID, "user", denied.ID, 0, int64(rbac.PermJoinVoice))
	f.seedRow(t, denied.ID, f.bootY, 31*time.Second)
	// A future-stamped row of boot Y is the one shape the lease renewer touches
	// on a refused upsert; it must not renew another boot's row either.
	future := f.member(t, "boot_binding_lease_future")
	f.ts.CreateChannelOverride(t, f.channelID, "user", future.ID, 0, int64(rbac.PermJoinVoice))
	f.seedRow(t, future.ID, f.bootY, 31*time.Second)
	_, err := f.ts.DB.Exec(`
		UPDATE voice_participants SET lifecycle_event_at = clock_timestamp() + INTERVAL '60 seconds'
		WHERE channel_id = $1 AND user_id = $2
	`, f.channelID, future.ID)
	require.NoError(t, err)
	f.setObservedAgo(t, future.ID, 31*time.Second)
	deniedBefore := f.observedAt(t, denied.ID)
	futureBefore := f.observedAt(t, future.ID)

	now := bootBindingNow()
	f.subscriber.HandleHeartbeatAt(f.heartbeat(t, f.bootX, now, denied.ID, future.ID), now)

	assert.True(t, f.observedAt(t, denied.ID).Equal(deniedBefore), "a refused claim renewed the lease")
	assert.True(t, f.observedAt(t, future.ID).Equal(futureBefore), "boot X renewed boot Y's future-stamped lease")
	stamp, _ := f.stamp(t, denied.ID)
	assert.Equal(t, f.bootY, stamp.UUID)
	assert.Zero(t, f.hooks.Load())
}

func TestHeartbeatNilResolverWritesNothing(t *testing.T) {
	f := newBootBindingFixture(t)
	f.subscriber.ClearClaimResolverForTest()
	now := bootBindingNow()
	f.subscriber.HandleHeartbeatAt(f.heartbeat(t, f.bootX, now, f.owner.ID), now)
	_, written := f.stamp(t, f.owner.ID)
	assert.False(t, written, "a claim with no resolver wrote a row")
	assert.Zero(t, f.hooks.Load())
}

func TestRemovalFromOtherBootLeavesLiveRow(t *testing.T) {
	f := newBootBindingFixture(t)
	peer := f.member(t, "boot_binding_peer")
	f.seedRow(t, f.owner.ID, f.bootY, 31*time.Second)
	f.seedRow(t, peer.ID, f.bootY, 31*time.Second)

	now := bootBindingNow()
	f.subscriber.HandleLeftAt(f.left(t, f.bootX, now, f.owner.ID), now)
	_, kept := f.stamp(t, f.owner.ID)
	assert.True(t, kept, "voice.left from boot X removed boot Y's live row")

	// Heartbeat omission: boot X reports the room without either user.
	f.subscriber.HandleHeartbeatAt(f.heartbeat(t, f.bootX, now.Add(time.Millisecond)), now.Add(time.Millisecond))
	assert.Equal(t, 2, countVoiceParticipants(t, f.ts.DB, f.channelID), "heartbeat omission removed boot Y's rows")

	f.subscriber.HandleRoomEmptyAt(f.roomEmpty(t, f.bootX, now.Add(2*time.Millisecond)), now.Add(2*time.Millisecond))
	assert.Equal(t, 2, countVoiceParticipants(t, f.ts.DB, f.channelID), "room_empty from boot X removed boot Y's rows")
	assert.Zero(t, f.hooks.Load(), "a kept foreign row is a determined no-op, never a disconnect")
}

func TestRemovalOfNullAndExpiredRows(t *testing.T) {
	f := newBootBindingFixture(t)
	expired := f.member(t, "boot_binding_expired")
	omitted := f.member(t, "boot_binding_omitted")
	f.seedRow(t, f.owner.ID, uuid.Nil, 5*time.Second)
	f.seedRow(t, expired.ID, f.bootY, 91*time.Second)
	f.seedRow(t, omitted.ID, f.bootY, 91*time.Second)

	now := bootBindingNow()
	f.subscriber.HandleLeftAt(f.left(t, f.bootX, now, f.owner.ID), now)
	_, kept := f.stamp(t, f.owner.ID)
	assert.False(t, kept, "an unstamped row must be removable by any boot")
	f.subscriber.HandleLeftAt(f.left(t, f.bootX, now, expired.ID), now)
	_, kept = f.stamp(t, expired.ID)
	assert.False(t, kept, "a row whose lease expired must be removable by another boot")
	f.subscriber.HandleHeartbeatAt(f.heartbeat(t, f.bootX, now.Add(time.Millisecond)), now.Add(time.Millisecond))
	_, kept = f.stamp(t, omitted.ID)
	assert.False(t, kept, "heartbeat omission must remove an expired row of another boot")
}

func TestSinkWithoutOriginWritesNothing(t *testing.T) {
	f := newBootBindingFixture(t)
	channelID := uuid.MustParse(f.channelID)
	ownerID := uuid.MustParse(f.owner.ID)
	applied, err := f.subscriber.MutateServerVoiceParticipantForTest(
		context.Background(), channelID, ownerID, bootBindingNow(),
	)
	require.NoError(t, err)
	assert.False(t, applied)
	assert.Zero(t, countVoiceParticipants(t, f.ts.DB, f.channelID))
}

func TestSinkWithoutAdmissionWritesNothing(t *testing.T) {
	f := newBootBindingFixture(t)
	stranger := f.ts.CreateTestUser(t, "boot_binding_sink_stranger")
	now := bootBindingNow()
	ctx := voice.WithLifecycleOriginForTest(context.Background(), f.bootX, now)
	applied, err := f.subscriber.MutateServerVoiceParticipantForTest(
		ctx, uuid.MustParse(f.channelID), uuid.MustParse(stranger.ID), now,
	)
	require.NoError(t, err)
	assert.False(t, applied, "the sink wrote a row with no admission for a non-member")
	assert.Zero(t, countVoiceParticipants(t, f.ts.DB, f.channelID))
}

func TestConcurrentMemberRemovalAndClaim(t *testing.T) {
	f := newBootBindingFixture(t)
	member := f.member(t, "boot_binding_removed_member")
	channelID := uuid.MustParse(f.channelID)
	memberID := uuid.MustParse(member.ID)

	done := make(chan struct{})
	var claims sync.WaitGroup
	claims.Add(1)
	go func() {
		defer claims.Done()
		for i := 0; i < 20; i++ {
			at := bootBindingNow()
			ctx := voice.WithLifecycleOriginForTest(context.Background(), f.bootX, at)
			_, err := f.subscriber.MutateServerVoiceParticipantForTest(ctx, channelID, memberID, at)
			assert.NoError(t, err)
		}
	}()
	removal := func() error {
		tx, err := f.ts.DB.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, member.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, f.serverID, member.ID); err != nil {
			return err
		}
		return tx.Commit()
	}
	go func() {
		defer close(done)
		assert.NoError(t, removal())
		claims.Wait()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("member removal and a heartbeat claim did not both finish within 10 s")
	}

	_, err := f.ts.DB.Exec(`DELETE FROM voice_participants WHERE channel_id = $1`, f.channelID)
	require.NoError(t, err)
	at := bootBindingNow()
	applied, err := f.subscriber.MutateServerVoiceParticipantForTest(
		voice.WithLifecycleOriginForTest(context.Background(), f.bootX, at), channelID, memberID, at,
	)
	require.NoError(t, err)
	assert.False(t, applied, "a claim after the membership was removed wrote a row")
}

// A heartbeat claim that moves a user must not delete the user's live row of
// another boot in another channel: that is a removal, bound by the same rule.
func TestHeartbeatClaimDoesNotMoveUserOffAnotherBootsLiveRow(t *testing.T) {
	f := newBootBindingFixture(t)
	elsewhere := f.ts.CreateVoiceChannel(t, f.serverID, "voice-boot-binding-elsewhere")
	at := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Microsecond)
	_, err := f.ts.DB.Exec(`
		INSERT INTO voice_participants (channel_id, user_id, joined_at, lifecycle_event_at, media_boot_id)
		VALUES ($1, $2, $3, $3, $4)
	`, elsewhere, f.owner.ID, at, f.bootY)
	require.NoError(t, err)

	now := bootBindingNow()
	f.subscriber.HandleHeartbeatAt(f.heartbeat(t, f.bootX, now, f.owner.ID), now)

	var kept int
	require.NoError(t, f.ts.DB.QueryRow(`
		SELECT COUNT(*) FROM voice_participants WHERE channel_id = $1 AND user_id = $2
	`, elsewhere, f.owner.ID).Scan(&kept))
	assert.Equal(t, 1, kept, "a heartbeat claim from boot X removed boot Y's live row in another channel")
	_, moved := f.stamp(t, f.owner.ID)
	assert.False(t, moved, "the claim moved the user while boot Y's live row stands")
	assert.Zero(t, f.hooks.Load())

	// Once boot Y's lease has expired the move proceeds.
	_, err = f.ts.DB.Exec(`
		UPDATE voice_participants SET lifecycle_observed_at = clock_timestamp() - INTERVAL '91 seconds'
		WHERE channel_id = $1 AND user_id = $2
	`, elsewhere, f.owner.ID)
	require.NoError(t, err)
	later := now.Add(time.Second)
	f.subscriber.HandleHeartbeatAt(f.heartbeat(t, f.bootX, later, f.owner.ID), later)
	stamp, moved := f.stamp(t, f.owner.ID)
	require.True(t, moved, "an expired row of another boot must not block the move")
	assert.Equal(t, f.bootX, stamp.UUID)
}
