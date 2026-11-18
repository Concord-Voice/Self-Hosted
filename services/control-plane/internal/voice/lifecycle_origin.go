package voice

import (
	"context"
	"database/sql"
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
