package voice_test

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Security#174 VULN-208: a voice.room_empty that removed nothing on a room that
// is already empty is a determined no-op, unless a peer replica's tombstone shows
// that the peer removed the rows (its local clients then still need the
// reconnect). Every row-removing room_empty records that tombstone in the same
// transaction as its delete.

type roomTombstone struct {
	boot         uuid.UUID
	recordedBy   uuid.UUID
	removedCount int
}

func (f bootBindingFixture) tombstone(t *testing.T) (roomTombstone, bool) {
	t.Helper()
	var stone roomTombstone
	err := f.ts.DB.QueryRow(`
		SELECT media_boot_id, recorded_by, removed_count
		FROM server_voice_room_terminals WHERE channel_id = $1
	`, f.channelID).Scan(&stone.boot, &stone.recordedBy, &stone.removedCount)
	if err == sql.ErrNoRows {
		return stone, false
	}
	require.NoError(t, err)
	return stone, true
}

func (f bootBindingFixture) plantTombstone(t *testing.T, recordedBy uuid.UUID, ago time.Duration) {
	t.Helper()
	_, err := f.ts.DB.Exec(`
		INSERT INTO server_voice_room_terminals (channel_id, media_boot_id, recorded_by, removed_count, recorded_at)
		VALUES ($1, $2, $3, 1, clock_timestamp() - ($4::bigint * INTERVAL '1 millisecond'))
	`, f.channelID, f.bootX, recordedBy, ago.Milliseconds())
	require.NoError(t, err)
}

func TestRoomEmptyRemovingRowsWritesTombstone(t *testing.T) {
	f := newBootBindingFixture(t)
	f.seedRow(t, f.owner.ID, f.bootX, 5*time.Second)

	now := bootBindingNow()
	f.subscriber.HandleRoomEmptyAt(f.roomEmpty(t, f.bootX, now), now)

	assert.Zero(t, countVoiceParticipants(t, f.ts.DB, f.channelID))
	stone, found := f.tombstone(t)
	require.True(t, found, "a room_empty that removed rows wrote no tombstone")
	assert.Equal(t, 1, stone.removedCount)
	assert.Equal(t, f.bootX, stone.boot)
	assert.Equal(t, f.subscriber.ReplicaIDForTest(), stone.recordedBy)
	assert.Zero(t, f.hooks.Load(), "removing rows is the local clear, never a fleet disconnect")
}

func TestNoopRoomEmptyWithoutTombstone(t *testing.T) {
	f := newBootBindingFixture(t)
	now := bootBindingNow()
	f.subscriber.HandleRoomEmptyAt(f.roomEmpty(t, f.bootX, now), now)

	assert.Zero(t, f.hooks.Load(), "a room_empty on an empty room with no tombstone disconnected the replica")
	_, found := f.tombstone(t)
	assert.False(t, found, "a no-op room_empty wrote a tombstone")
}

func TestNoopRoomEmptyOwnTombstone(t *testing.T) {
	f := newBootBindingFixture(t)
	f.plantTombstone(t, f.subscriber.ReplicaIDForTest(), time.Second)

	now := bootBindingNow()
	f.subscriber.HandleRoomEmptyAt(f.roomEmpty(t, f.bootX, now), now)

	assert.Zero(t, f.hooks.Load(), "this replica's own tombstone means its own clients were already cleared")
}

func TestNoopRoomEmptyPeerTombstone(t *testing.T) {
	f := newBootBindingFixture(t)
	f.plantTombstone(t, uuid.New(), time.Second)

	now := bootBindingNow()
	f.subscriber.HandleRoomEmptyAt(f.roomEmpty(t, f.bootX, now), now)

	assert.Equal(t, int32(1), f.hooks.Load(), "a peer's recent removal must still reconnect this replica's clients")
}

func TestNoopRoomEmptyPeerTombstoneExpired(t *testing.T) {
	f := newBootBindingFixture(t)
	f.plantTombstone(t, uuid.New(), voice.RoomEmptyTombstoneWindowForTest+5*time.Second)

	now := bootBindingNow()
	f.subscriber.HandleRoomEmptyAt(f.roomEmpty(t, f.bootX, now), now)

	assert.Zero(t, f.hooks.Load(), "a peer tombstone older than the window was treated as recent")
}

func TestNoopRoomEmptyTombstoneReadError(t *testing.T) {
	f := newBootBindingFixture(t)
	f.subscriber.SetPeerTombstoneReadContextForTest(func(ctx context.Context) context.Context {
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		return canceled
	})

	now := bootBindingNow()
	f.subscriber.HandleRoomEmptyAt(f.roomEmpty(t, f.bootX, now), now)

	assert.Equal(t, int32(1), f.hooks.Load(), "an unreadable tombstone must fail closed with the disconnect")
}

func TestTwoSubscribersOneTombstone(t *testing.T) {
	f := newBootBindingFixture(t)
	peer := f.member(t, "tombstone_peer")
	f.seedRow(t, f.owner.ID, f.bootX, 5*time.Second)
	f.seedRow(t, peer.ID, f.bootX, 5*time.Second)
	loser := newTestSubscriber(f.ts)
	loserHooks := &atomic.Int32{}
	loser.SetDisconnectAllRichPresenceClientsHookForTest(func() { loserHooks.Add(1) })

	now := bootBindingNow()
	event := f.roomEmpty(t, f.bootX, now)
	f.subscriber.HandleRoomEmptyAt(event, now)
	loser.HandleRoomEmptyAt(event, now.Add(time.Millisecond))

	var stones int
	require.NoError(t, f.ts.DB.QueryRow(
		`SELECT COUNT(*) FROM server_voice_room_terminals WHERE channel_id = $1`, f.channelID,
	).Scan(&stones))
	assert.Equal(t, 1, stones)
	stone, _ := f.tombstone(t)
	assert.Equal(t, 2, stone.removedCount)
	assert.Equal(t, f.subscriber.ReplicaIDForTest(), stone.recordedBy)
	assert.Zero(t, f.hooks.Load())
	assert.Equal(t, int32(1), loserHooks.Load(), "the replica that removed nothing must reconnect after a peer's removal")
}

func TestRoomEmptyBroadcastsPerRowWhenOtherBootRowsRemain(t *testing.T) {
	f := newBootBindingFixture(t)
	foreign := f.member(t, "tombstone_foreign")
	f.seedRow(t, f.owner.ID, f.bootX, 5*time.Second)
	f.seedRow(t, foreign.ID, f.bootY, 5*time.Second)
	now := bootBindingNow()
	f.subscriber.HandleRoomEmptyAt(f.roomEmpty(t, f.bootX, now), now)

	_, ownerKept := f.stamp(t, f.owner.ID)
	assert.False(t, ownerKept)
	_, foreignKept := f.stamp(t, foreign.ID)
	assert.True(t, foreignKept, "boot X's room_empty removed boot Y's live row")
	stone, found := f.tombstone(t)
	require.True(t, found)
	assert.Equal(t, 1, stone.removedCount)
	assert.Zero(t, f.hooks.Load())
}
