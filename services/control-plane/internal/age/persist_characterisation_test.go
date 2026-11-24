package age

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
)

// persist must refuse with a bare ErrEpochMismatch (never ErrDenied) when the
// requesting identity is absent or belongs to another user, and must write
// nothing: no age record, no disablement, no family terminalization.
func TestPersist_IdentityAbsentOrForeign_EpochMismatchWritesNothing(t *testing.T) {
	cases := map[string]func(e *ageITEnv) context.Context{
		"absent": func(*ageITEnv) context.Context { return context.Background() },
		"foreign user": func(e *ageITEnv) context.Context {
			identity, ok := loginfamily.AccessIdentityFromContext(e.actor)
			require.True(t, ok)
			identity.UserID = uuid.New()
			return loginfamily.WithAccessIdentity(context.Background(), identity)
		},
	}
	for name, ctxFor := range cases {
		for _, valid := range []bool{true, false} {
			t.Run(name+map[bool]string{true: "/valid", false: "/disable"}[valid], func(t *testing.T) {
				e, cleanup := setupAgeIT(t, true)
				defer cleanup()
				family := e.actorFamily(t)
				claim := e.freshClaim()
				claim.ValidAge = valid

				lastChange, err := e.h.persist(ctxFor(e), claim, "fixture-signature")

				require.Error(t, err)
				assert.ErrorIs(t, err, credepoch.ErrEpochMismatch)
				assert.False(t, errors.Is(err, loginfamily.ErrDenied))
				assert.True(t, lastChange.IsZero())
				assert.False(t, e.userDisabled(t))
				var n int
				require.NoError(t, e.db.QueryRow(`SELECT count(*) FROM age_verification_records WHERE user_id=$1`, e.userID).Scan(&n))
				assert.Zero(t, n)
				var terminal bool
				require.NoError(t, e.db.QueryRow(`SELECT terminal_at IS NOT NULL FROM login_families WHERE family_id=$1`, family).Scan(&terminal))
				assert.False(t, terminal)
			})
		}
	}
}

// A denylist write failure after commit is logged and swallowed: the committed
// disablement still succeeds with the DB-authoritative last_change.
func TestPersist_DenylistSetFailureAfterCommitStillSucceeds(t *testing.T) {
	e, cleanup := setupAgeIT(t, true)
	defer cleanup()
	family := e.actorFamily(t)
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialerRetries: 1})
	defer func() { _ = dead.Close() }()
	e.h.redis = dead
	var logs bytes.Buffer
	e.h.log = logger.NewWithWriter(&logs)
	claim := e.freshClaim()
	claim.ValidAge = false

	lastChange, err := e.h.persist(e.actor, claim, "fixture-signature")

	require.NoError(t, err)
	assert.False(t, lastChange.IsZero())
	assert.True(t, e.userDisabled(t))
	var terminal bool
	require.NoError(t, e.db.QueryRow(`SELECT terminal_at IS NOT NULL FROM login_families WHERE family_id=$1`, family).Scan(&terminal))
	assert.True(t, terminal)
	n, err := e.rdb.Exists(context.Background(), middleware.UserDisabledKey(e.userID)).Result()
	require.NoError(t, err)
	assert.Zero(t, n, "the dead client wrote nothing to the real Redis")
	assert.Contains(t, logs.String(), "age: denylist set failed", "the denylist write was attempted on the dead client")
}

// Positive control for the test above: with a healthy client the same
// disablement writes the denylist key and logs no failure.
func TestPersist_DenylistSetSucceedsWithHealthyRedis(t *testing.T) {
	e, cleanup := setupAgeIT(t, true)
	defer cleanup()
	e.actorFamily(t)
	var logs bytes.Buffer
	e.h.log = logger.NewWithWriter(&logs)
	claim := e.freshClaim()
	claim.ValidAge = false

	_, err := e.h.persist(e.actor, claim, "fixture-signature")

	require.NoError(t, err)
	n, err := e.rdb.Exists(context.Background(), middleware.UserDisabledKey(e.userID)).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	assert.NotContains(t, logs.String(), "age: denylist set failed")
}

// A requesting login revoked before persist takes the user lock is refused
// with ErrEpochMismatch joined with loginfamily.ErrDenied, and writes nothing.
func TestPersist_RevokedRequestingFamily_JoinsEpochMismatchAndDenied(t *testing.T) {
	e, cleanup := setupAgeIT(t, true)
	defer cleanup()
	identity, ok := loginfamily.AccessIdentityFromContext(e.actor)
	require.True(t, ok)

	ctx := context.Background()
	revoke, err := e.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	barrier, err := loginfamily.AcquireWriterBarrierTx(ctx, revoke)
	require.NoError(t, err)
	users, err := loginfamily.LockUsersTx(ctx, revoke, barrier, []uuid.UUID{identity.UserID}, loginfamily.UserNoKeyUpdate)
	require.NoError(t, err)
	user, err := users.ForUser(identity.UserID)
	require.NoError(t, err)
	targets, err := loginfamily.ResolveTargetSIDTx(ctx, revoke, user, identity.SID)
	require.NoError(t, err)
	_, err = loginfamily.RevokeFamiliesTx(ctx, revoke, targets, loginfamily.AuthCause{Scope: loginfamily.ScopeSession, Reason: loginfamily.ReasonLogout})
	require.NoError(t, err)
	require.NoError(t, revoke.Commit())

	for _, valid := range []bool{true, false} {
		claim := e.freshClaim()
		claim.ValidAge = valid

		lastChange, err := e.h.persist(e.actor, claim, "fixture-signature")

		require.Error(t, err)
		assert.ErrorIs(t, err, credepoch.ErrEpochMismatch)
		assert.ErrorIs(t, err, loginfamily.ErrDenied)
		assert.True(t, lastChange.IsZero())
		assert.False(t, e.userDisabled(t))
		var records int
		require.NoError(t, e.db.QueryRow(`SELECT count(*) FROM age_verification_records WHERE user_id=$1`, e.userID).Scan(&records))
		assert.Zero(t, records)
	}
}
