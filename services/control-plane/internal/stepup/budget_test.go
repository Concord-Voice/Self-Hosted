package stepup

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newBudgetRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, mr
}

func TestBudget_AdmitsUpToTheLimitThen429(t *testing.T) {
	rdb, _ := newBudgetRedis(t)
	b := NewBudget(rdb, "stepup:test:")
	ctx := context.Background()

	for i := 0; i < BudgetLimit; i++ {
		require.Nil(t, b.Consume(ctx, "u1"), "attempt %d must be admitted", i+1)
	}
	e := b.Consume(ctx, "u1")
	require.NotNil(t, e)
	require.Equal(t, http.StatusTooManyRequests, e.Status)
	// The flag lets a client tell this 429 from the route limiter's without
	// matching copy (#3455 X17); the 503's flag must not appear on it.
	require.Equal(t, gin.H{"error": ErrMsgTooManyAttempts, "step_up_budget_exhausted": true}, e.Body)
	require.Nil(t, e.Cause, "an exhausted budget is an outcome, not a fault")
	require.Nil(t, b.Consume(ctx, "u2"), "the budget is per user")
}

func TestBudget_ExhaustedWriteIncludesRetryAfter(t *testing.T) {
	rdb, mr := newBudgetRedis(t)
	b := NewBudget(rdb, "stepup:test:")
	ctx := context.Background()

	for range BudgetLimit {
		require.Nil(t, b.Consume(ctx, "u1"))
	}
	mr.FastForward(500 * time.Millisecond)
	e := b.Consume(ctx, "u1")
	require.NotNil(t, e)
	require.Equal(t, http.StatusTooManyRequests, e.Status)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	e.Write(c)

	retryAfter, err := strconv.Atoi(w.Header().Get("Retry-After"))
	require.NoError(t, err)
	require.Equal(t, int(BudgetWindow.Seconds()), retryAfter)
	require.Equal(t, http.StatusTooManyRequests, w.Code)
}

// An unevaluable budget DENIES, and says so honestly: 503 with its own body,
// never the 429 that blames the user.
func TestBudget_UnwiredOrFailingRedisIs503(t *testing.T) {
	ctx := context.Background()

	e := NewBudget(nil, "stepup:test:").Consume(ctx, "u1")
	require.NotNil(t, e)
	require.Equal(t, http.StatusServiceUnavailable, e.Status)
	require.Equal(t, gin.H{"error": ErrMsgBudgetUnavailable, "step_up_budget_unavailable": true}, e.Body)
	require.ErrorIs(t, e, errBudgetUnwired)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	e.Write(c)
	require.Empty(t, w.Header().Get("Retry-After"))
	require.NotContains(t, w.Body.String(), "step_up_budget_exhausted")

	rdb, mr := newBudgetRedis(t)
	mr.Close()
	e = NewBudget(rdb, "stepup:test:").Consume(ctx, "u1")
	require.NotNil(t, e)
	require.Equal(t, http.StatusServiceUnavailable, e.Status)
	require.Equal(t, gin.H{"error": ErrMsgBudgetUnavailable, "step_up_budget_unavailable": true}, e.Body)
	require.NotNil(t, e.Cause, "the transport error is kept for the caller to log")
}

func TestBudget_ClearResetsAndReportsFailure(t *testing.T) {
	rdb, mr := newBudgetRedis(t)
	b := NewBudget(rdb, "stepup:test:")
	ctx := context.Background()
	for i := 0; i < BudgetLimit; i++ {
		require.Nil(t, b.Consume(ctx, "u1"))
	}
	require.NoError(t, b.Clear(ctx, "u1"))
	require.False(t, mr.Exists(b.Key("u1")))
	require.Nil(t, b.Consume(ctx, "u1"), "a cleared budget admits again")

	require.NoError(t, NewBudget(nil, "stepup:test:").Clear(ctx, "u1"), "an unwired budget has nothing to clear")

	mr.Close()
	err := b.Clear(ctx, "u1")
	require.Error(t, err)
	require.False(t, errors.Is(err, errBudgetUnwired))
}

func TestBudget_KeyIsPrefixPlusUser(t *testing.T) {
	require.Equal(t, "stepup:mfa_settings:abc", NewBudget(nil, "stepup:mfa_settings:").Key("abc"))
}

// pttlHook rewrites the spent-budget PTTL read: an error, or a non-positive
// value (the key expired, or lost its expiry, between the INCR and the read).
type pttlHook struct {
	err error
	val time.Duration
}

func (pttlHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h pttlHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() != "pttl" {
			return next(ctx, cmd)
		}
		if h.err != nil {
			cmd.SetErr(h.err)
			return h.err
		}
		if d, ok := cmd.(*redis.DurationCmd); ok {
			d.SetVal(h.val)
		}
		return nil
	}
}

func (pttlHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// A spent budget whose remaining window cannot be read is a server fault: a
// 503 with Cause and no Retry-After, never a 429 with a made-up delay.
func TestBudget_SpentWithUnreadableWindowIs503(t *testing.T) {
	for name, hook := range map[string]pttlHook{
		"PTTL error":        {err: errors.New("pttl unavailable")},
		"key without TTL":   {val: -1},
		"key already gone":  {val: -2},
		"zero remaining ms": {val: 0},
	} {
		t.Run(name, func(t *testing.T) {
			rdb, _ := newBudgetRedis(t)
			b := NewBudget(rdb, "stepup:test_pttl:")
			ctx := context.Background()
			require.NoError(t, rdb.Set(ctx, b.Key("u1"), BudgetLimit, BudgetWindow).Err())
			rdb.AddHook(hook)

			e := b.Consume(ctx, "u1")
			require.NotNil(t, e)
			assert.Equal(t, http.StatusServiceUnavailable, e.Status)
			assert.Equal(t, true, e.Body["step_up_budget_unavailable"])
			require.Error(t, e.Cause)

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			e.Write(c)
			assert.Empty(t, w.Header().Get("Retry-After"))
		})
	}
}
