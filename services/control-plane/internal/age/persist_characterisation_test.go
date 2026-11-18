package age

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
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
}
