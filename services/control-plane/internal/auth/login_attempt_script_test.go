package auth

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

func mustReserve(t *testing.T, h *Handler) bool {
	t.Helper()
	ok, err := h.reserveLoginAttempt(context.Background(), attemptEmail)
	require.NoError(t, err)
	return ok
}

func newLoginAttemptRig(t *testing.T) (*Handler, *miniredis.Miniredis, *bytes.Buffer) {
	t.Helper()
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	var logs bytes.Buffer
	return &Handler{redis: rdb, log: logger.NewWithWriter(&logs)}, mini, &logs
}

const attemptEmail = "burst@example.test"

func TestReserveLoginAttempt_CountsUpToThresholdThenLocks(t *testing.T) {
	h, mini, _ := newLoginAttemptRig(t)

	for i := 1; i <= loginLockoutThreshold; i++ {
		require.True(t, mustReserve(t, h), "guess %d must be allowed", i)
	}
	assert.False(t, mini.Exists("login_lockout:"+attemptEmail), "reaching the threshold by reserving alone must not lock")

	// The guess over the threshold is refused and locks the account at once.
	assert.False(t, mustReserve(t, h))
	assert.True(t, mini.Exists("login_lockout:"+attemptEmail))
	assert.Equal(t, 15*time.Minute, mini.TTL("login_lockout:"+attemptEmail))
	assert.False(t, mini.Exists("login_attempts:"+attemptEmail), "setting the lock resets the attempts counter")

	// While locked, a reserve is refused without counting.
	assert.False(t, mustReserve(t, h))
	assert.False(t, mini.Exists("login_attempts:"+attemptEmail))
}

func TestRecordCheckedFailure_LocksOnlyAtThreshold(t *testing.T) {
	h, mini, _ := newLoginAttemptRig(t)
	ctx := context.Background()

	for i := 1; i < loginLockoutThreshold; i++ {
		require.True(t, mustReserve(t, h))
		h.recordCheckedFailure(ctx, attemptEmail)
	}
	assert.False(t, mini.Exists("login_lockout:"+attemptEmail))
	v, err := mini.Get("login_attempts:" + attemptEmail)
	require.NoError(t, err)
	assert.Equal(t, "4", v, "a checked failure must not count the guess twice")

	require.True(t, mustReserve(t, h))
	h.recordCheckedFailure(ctx, attemptEmail)
	assert.True(t, mini.Exists("login_lockout:"+attemptEmail))

	// A second failure from the same burst must not escalate the lock again.
	h.recordCheckedFailure(ctx, attemptEmail)
	lc, err := mini.Get("login_lockout_count:" + attemptEmail)
	require.NoError(t, err)
	assert.Equal(t, "1", lc)
}

func TestRecordFailedLogin_CountsAndEscalatesToTheLongestLock(t *testing.T) {
	h, mini, _ := newLoginAttemptRig(t)
	ctx := context.Background()

	want := []time.Duration{15 * time.Minute, 30 * time.Minute, 60 * time.Minute, 60 * time.Minute}
	for _, d := range want {
		for i := 0; i < loginLockoutThreshold; i++ {
			require.NoError(t, h.recordFailedLogin(ctx, attemptEmail))
		}
		require.True(t, mini.Exists("login_lockout:"+attemptEmail))
		assert.Equal(t, d, mini.TTL("login_lockout:"+attemptEmail))
		mini.Del("login_lockout:" + attemptEmail) // the lock expires
	}
}

func TestLoginAttemptScript_RedisErrorsFailClosedAndLog(t *testing.T) {
	h, mini, logs := newLoginAttemptRig(t)
	ctx := context.Background()
	mini.Close()

	ok, err := h.reserveLoginAttempt(ctx, attemptEmail)
	assert.False(t, ok, "a Redis error must refuse the guess")
	assert.Error(t, err, "a Redis error must not look like a lock")
	h.recordCheckedFailure(ctx, attemptEmail)
	assert.Error(t, h.recordFailedLogin(ctx, attemptEmail), "a Redis error must reach the caller")
	h.clearLoginAttempts(ctx, attemptEmail)

	out := logs.String()
	assert.Contains(t, out, "login lockout: reserve attempt failed")
	assert.Contains(t, out, "login lockout: record failure failed")
	assert.Contains(t, out, "login lockout: clear attempts failed")
}

func TestVerifyCredentials_RefusesOverThreshold(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, mini, _ := newLoginAttemptRig(t)
	// A burst already used up the threshold, but no failure has set the lock yet.
	require.NoError(t, mini.Set("login_attempts:"+attemptEmail, "5"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)

	ok := h.verifyCredentials(context.Background(), c, attemptEmail, "irrelevant", "not-a-hash")
	assert.False(t, ok)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), errMsgInvalidCredentials)
	assert.True(t, mini.Exists("login_lockout:"+attemptEmail))
}

func TestVerifyCredentials_RedisFailureIs500NotALock(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, mini, _ := newLoginAttemptRig(t)
	mini.Close()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)

	ok := h.verifyCredentials(context.Background(), c, attemptEmail, "irrelevant", "not-a-hash")
	assert.False(t, ok)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), errMsgLoginFailed)
}

func TestClearLoginAttempts_KeepsALockSetDuringTheGuess(t *testing.T) {
	h, mini, _ := newLoginAttemptRig(t)
	ctx := context.Background()

	// A correct guess reserves a slot, then a burst locks the account before
	// the correct guess finishes. Its success must not lift that lock.
	require.True(t, mustReserve(t, h))
	for i := 0; i < loginLockoutThreshold; i++ {
		mustReserve(t, h)
	}
	require.True(t, mini.Exists("login_lockout:"+attemptEmail))

	h.clearLoginAttempts(ctx, attemptEmail)
	assert.True(t, mini.Exists("login_lockout:"+attemptEmail), "a success must not lift a burst lock")
	assert.True(t, mini.Exists("login_lockout_count:"+attemptEmail), "a success must not reset escalation while locked")
}

func TestClearLoginAttempts_ResetsCountersWhenNotLocked(t *testing.T) {
	h, mini, _ := newLoginAttemptRig(t)
	ctx := context.Background()

	require.NoError(t, mini.Set("login_lockout_count:"+attemptEmail, "2"))
	require.True(t, mustReserve(t, h))
	h.clearLoginAttempts(ctx, attemptEmail)
	assert.False(t, mini.Exists("login_attempts:"+attemptEmail))
	assert.False(t, mini.Exists("login_lockout_count:"+attemptEmail))
}
