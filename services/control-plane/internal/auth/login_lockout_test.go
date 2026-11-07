package auth

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newLockoutRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, mini
}

const lockoutTestPassword = "correct-horse-battery" // pragma: allowlist secret

func lockoutTestHash(t *testing.T) string {
	t.Helper()
	hash, err := HashPassword(lockoutTestPassword)
	require.NoError(t, err)
	return hash
}

func TestVerifyPasswordWithLockout_CorrectPasswordPassesAndClears(t *testing.T) {
	rdb, mini := newLockoutRedis(t)
	ctx := context.Background()
	hash := lockoutTestHash(t)

	ok, err := VerifyPasswordWithLockout(ctx, rdb, attemptEmail, "wrong", hash)
	require.NoError(t, err)
	require.False(t, ok)
	require.True(t, mini.Exists("login_attempts:"+attemptEmail))

	ok, err = VerifyPasswordWithLockout(ctx, rdb, attemptEmail, lockoutTestPassword, hash)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.False(t, mini.Exists("login_attempts:"+attemptEmail), "success clears the count")
}

func TestVerifyPasswordWithLockout_FifthWrongGuessLocksAndRefusesTheRightOne(t *testing.T) {
	rdb, mini := newLockoutRedis(t)
	ctx := context.Background()
	hash := lockoutTestHash(t)

	for i := 1; i <= loginLockoutThreshold; i++ {
		ok, err := VerifyPasswordWithLockout(ctx, rdb, attemptEmail, "wrong", hash)
		require.NoError(t, err, "guess %d", i)
		require.False(t, ok, "guess %d", i)
	}
	require.True(t, mini.Exists("login_lockout:"+attemptEmail))

	ok, err := VerifyPasswordWithLockout(ctx, rdb, attemptEmail, lockoutTestPassword, hash)
	assert.ErrorIs(t, err, ErrAccountLocked)
	assert.False(t, ok)
}

// The same counter as /auth/login: the handler's own reserve sees a lock the
// exported verifier set.
func TestVerifyPasswordWithLockout_SharesTheHandlerCounter(t *testing.T) {
	h, _, _ := newLoginAttemptRig(t)
	ctx := context.Background()
	hash := lockoutTestHash(t)

	for range loginLockoutThreshold {
		_, err := VerifyPasswordWithLockout(ctx, h.redis, attemptEmail, "wrong", hash)
		require.NoError(t, err)
	}
	assert.False(t, mustReserve(t, h), "a step-up lock refuses /auth/login too")
}

func TestVerifyPasswordWithLockout_RedisDownRefusesAsAnError(t *testing.T) {
	rdb, mini := newLockoutRedis(t)
	mini.Close()

	ok, err := VerifyPasswordWithLockout(context.Background(), rdb, attemptEmail, lockoutTestPassword, lockoutTestHash(t))
	require.Error(t, err)
	assert.False(t, ok, "an uncounted guess never passes")
	assert.NotErrorIs(t, err, ErrAccountLocked, "an outage is not a lock")
	assert.NotContains(t, err.Error(), lockoutTestPassword)
}

func TestVerifyPasswordWithLockout_NilClientRefuses(t *testing.T) {
	ok, err := VerifyPasswordWithLockout(context.Background(), nil, attemptEmail, lockoutTestPassword, "unused")
	assert.ErrorIs(t, err, ErrLockoutUnavailable)
	assert.False(t, ok)
}

func TestVerifyPasswordWithLockout_MalformedHashIsASentinel(t *testing.T) {
	rdb, _ := newLockoutRedis(t)
	malformed := "not-a-valid-argon2-encoding"

	ok, err := VerifyPasswordWithLockout(context.Background(), rdb, attemptEmail, "anything", malformed)
	assert.False(t, ok)
	assert.Equal(t, ErrPasswordHashCheck, err, "the decode error, which can embed the hash, never escapes")
	assert.NotContains(t, err.Error(), malformed)
}

// A Redis fault after the hash check still refuses: the guess cannot be
// shown to have been recorded or cleared.
func TestVerifyPasswordWithLockout_RecordAndClearFaultsRefuse(t *testing.T) {
	for name, password := range map[string]string{"wrong": "wrong", "correct": lockoutTestPassword} { // pragma: allowlist secret
		t.Run(name, func(t *testing.T) {
			rdb, _ := newLockoutRedis(t)
			rdb.AddHook(failAfterHook{allow: 1})
			ok, err := VerifyPasswordWithLockout(context.Background(), rdb, attemptEmail, password, lockoutTestHash(t))
			require.Error(t, err)
			assert.False(t, ok)
			assert.NotErrorIs(t, err, ErrAccountLocked)
		})
	}
}

func TestExportedLockoutFunctions_RedisDown(t *testing.T) {
	rdb, mini := newLockoutRedis(t)
	mini.Close()
	ctx := context.Background()

	_, err := ReserveLoginAttempt(ctx, rdb, attemptEmail)
	assert.Error(t, err)
	assert.Error(t, RecordCheckedFailure(ctx, rdb, attemptEmail))
	assert.Error(t, ClearLoginAttempts(ctx, rdb, attemptEmail))
}

// failAfterHook lets the first allow script calls through (EVALSHA and its
// EVAL fallback count as one) and fails every later command.
type failAfterHook struct{ allow int }

func (failAfterHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (failAfterHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (f failAfterHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	calls := 0
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "evalsha" {
			calls++
		}
		if calls > f.allow {
			return assert.AnError
		}
		return next(ctx, cmd)
	}
}
