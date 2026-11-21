package voice

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
)

// Lifecycle origin: the media-plane boot that produced a voice.* lifecycle
// event, decoded from the signed body's nodeBootId (Security#174 §5.6).
//
// Invariant. A Server Voice row stamped with boot Y is written only by an
// event from Y, by a joined event that consumed an admission, or by a
// heartbeat whose membership and CONNECT resolve passed in the writing
// transaction. It is removed only by an event from Y, or once its database
// lease (lifecycle_observed_at) has expired against the event's NATS receipt
// time. An unstamped row (written before migration 000198) is removable by any
// boot.
//
// What breaks it: a writer that inserts or refreshes voice_participants without
// consulting lifecycleOrigin, a deleter that skips serverVoiceRowRemovable, or a
// receipt time read from a handler's own clock (dispatcher lateness would then
// age a live row into "expired"). This fences a predecessor or peer boot; it is
// not a defence against a holder of the media plane's credential and MAC key,
// who can name any boot.
//
// The origin is its own context value, separate from the placement generation,
// because placement is set only when it is enabled and the origin is always
// required.
type lifecycleOriginKey struct{}

type lifecycleReceivedAtKey struct{}

type serverVoiceClaimAuthorizedKey struct{}

// withLifecycleOrigin records the event's boot. A nil boot records nothing, so
// lifecycleOrigin reports absent and the write sink writes nothing.
func withLifecycleOrigin(ctx context.Context, boot uuid.UUID) context.Context {
	if boot == uuid.Nil {
		return ctx
	}
	return context.WithValue(ctx, lifecycleOriginKey{}, boot)
}

func lifecycleOrigin(ctx context.Context) (uuid.UUID, bool) {
	boot, ok := ctx.Value(lifecycleOriginKey{}).(uuid.UUID)
	return boot, ok && boot != uuid.Nil
}

// withLifecycleReceivedAt records the NATS receipt time the lease test is
// measured against. It must be the dispatcher's receivedAt, never time.Now().
func withLifecycleReceivedAt(ctx context.Context, receivedAt time.Time) context.Context {
	if receivedAt.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, lifecycleReceivedAtKey{}, receivedAt)
}

func lifecycleReceivedAt(ctx context.Context) time.Time {
	receivedAt, _ := ctx.Value(lifecycleReceivedAtKey{}).(time.Time)
	return receivedAt
}

// withServerVoiceClaimAuthorized marks one move call as covered by a CONNECT
// resolve that passed in the same transaction. It is set only on the context
// handed to moveServerVoiceParticipant, never on a handler context.
func withServerVoiceClaimAuthorized(ctx context.Context) context.Context {
	return context.WithValue(ctx, serverVoiceClaimAuthorizedKey{}, true)
}

func serverVoiceClaimAuthorized(ctx context.Context) bool {
	authorized, _ := ctx.Value(serverVoiceClaimAuthorizedKey{}).(bool)
	return authorized
}

// serverVoiceRowRemovable is the removal rule for an event from origin. A row is
// removable when it is unstamped, stamped by origin, or its lease expired before
// receivedAt - presence.ActivityStateTTL. A zero receivedAt never expires a row,
// and a nil origin matches no stamp (migration 000198 refuses the nil UUID).
func serverVoiceRowRemovable(
	stamp uuid.NullUUID,
	observedAt, receivedAt time.Time,
	origin uuid.UUID,
) bool {
	if !stamp.Valid {
		return true
	}
	if origin != uuid.Nil && stamp.UUID == origin {
		return true
	}
	if receivedAt.IsZero() {
		return false
	}
	return observedAt.Before(receivedAt.Add(-presence.ActivityStateTTL))
}

// serverVoiceLeaseCutoff is the observed-lease instant before which a row of
// another boot counts as expired for the event on ctx. Without a receipt time
// no row is expired (NULL, which the SQL reads as "never").
func serverVoiceLeaseCutoff(ctx context.Context) sql.NullTime {
	receivedAt := lifecycleReceivedAt(ctx)
	if receivedAt.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: receivedAt.Add(-presence.ActivityStateTTL), Valid: true}
}

// roomEmptyTombstoneWindow is how long a peer replica's room_empty tombstone
// counts as the reason this replica observed an already-empty room. It covers
// the activity lease plus delivery slack between the replicas (VULN-208).
const roomEmptyTombstoneWindow = presence.ActivityStateTTL + 30*time.Second

type roomEmptyTombstoneKey struct{}

// withRoomEmptyTombstone marks a reconciliation as a voice.room_empty run by
// replica, so each row it removes records the channel's tombstone in the same
// transaction as the delete. Heartbeat omission never sets it.
func withRoomEmptyTombstone(ctx context.Context, replica uuid.UUID) context.Context {
	if replica == uuid.Nil {
		return ctx
	}
	return context.WithValue(ctx, roomEmptyTombstoneKey{}, replica)
}

func roomEmptyTombstoneReplica(ctx context.Context) (uuid.UUID, bool) {
	replica, ok := ctx.Value(roomEmptyTombstoneKey{}).(uuid.UUID)
	return replica, ok && replica != uuid.Nil
}

// recordRoomEmptyTombstoneTx upserts the channel's tombstone after a
// room_empty delete in tx, so it commits with that delete. Consecutive
// removals by the same replica and boot within the window add to one count;
// any other writer starts a new count.
func recordRoomEmptyTombstoneTx(ctx context.Context, tx *sql.Tx, channelID uuid.UUID) error {
	replica, marked := roomEmptyTombstoneReplica(ctx)
	if !marked {
		return nil
	}
	boot, known := lifecycleOrigin(ctx)
	if !known {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO server_voice_room_terminals
			(channel_id, media_boot_id, recorded_by, removed_count, recorded_at)
		VALUES ($1, $2, $3, 1, clock_timestamp())
		ON CONFLICT (channel_id) DO UPDATE SET
			removed_count = CASE
				WHEN server_voice_room_terminals.recorded_by = EXCLUDED.recorded_by
				 AND server_voice_room_terminals.media_boot_id = EXCLUDED.media_boot_id
				 AND server_voice_room_terminals.recorded_at >=
				     clock_timestamp() - ($4::bigint * INTERVAL '1 millisecond')
				THEN server_voice_room_terminals.removed_count + 1
				ELSE 1
			END,
			media_boot_id = EXCLUDED.media_boot_id,
			recorded_by = EXCLUDED.recorded_by,
			recorded_at = EXCLUDED.recorded_at
	`, channelID, boot, replica, roomEmptyTombstoneWindow.Milliseconds())
	if err != nil {
		return fmt.Errorf("record room_empty tombstone: %w", err)
	}
	return nil
}

// peerTombstoneRecent reports whether another control-plane replica recorded a
// room_empty removal on channelID within roomEmptyTombstoneWindow of
// receivedAt. A zero receivedAt measures the window against the database clock.
func (s *NATSSubscriber) peerTombstoneRecent(
	ctx context.Context,
	channelID uuid.UUID,
	receivedAt time.Time,
) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("room_empty tombstone store unavailable")
	}
	if s.peerTombstoneReadContext != nil {
		ctx = s.peerTombstoneReadContext(ctx)
	}
	reference := sql.NullTime{Time: receivedAt, Valid: !receivedAt.IsZero()}
	var recent bool
	if err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM server_voice_room_terminals
			WHERE channel_id = $1
			  AND recorded_by <> $2
			  AND recorded_at >= COALESCE($3::timestamptz, clock_timestamp())
			                     - ($4::bigint * INTERVAL '1 millisecond')
		)
	`, channelID, s.replicaID, reference, roomEmptyTombstoneWindow.Milliseconds()).Scan(&recent); err != nil {
		return false, fmt.Errorf("read room_empty tombstone: %w", err)
	}
	return recent, nil
}
