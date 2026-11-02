package websocket

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var budgetTestUser = uuid.MustParse("11111111-1111-4111-8111-111111111111")

const budgetTestKey = "ratelimit:user:ws:v1:{11111111-1111-4111-8111-111111111111}:profile"

// The hook replaces only the external command boundary. Constructor, validation,
// Script.Run fallback and decoding remain the real production implementation.
type budgetRedisError string

func (e budgetRedisError) Error() string { return string(e) }
func (budgetRedisError) RedisError()     {}

type budgetCommandHook struct {
	run func(context.Context, redis.Cmder) error
}

func (h budgetCommandHook) DialHook(next redis.DialHook) redis.DialHook       { return next }
func (h budgetCommandHook) ProcessHook(_ redis.ProcessHook) redis.ProcessHook { return h.run }
func (h budgetCommandHook) ProcessPipelineHook(_ redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(context.Context, []redis.Cmder) error { return errors.New("unexpected budget pipeline") }
}
func budgetHookClient(t *testing.T, run func(context.Context, redis.Cmder) error) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1, DialerRetries: 1})
	client.AddHook(budgetCommandHook{run: run})
	t.Cleanup(func() {
		err := client.Close()
		if !errors.Is(err, redis.ErrClosed) {
			assert.NoError(t, err)
		}
	})
	return client
}
func budgetReply(t *testing.T, cmd redis.Cmder, value interface{}, err error) error {
	t.Helper()
	result, ok := cmd.(*redis.Cmd)
	require.True(t, ok, "budget command must preserve untyped RESP values")
	result.SetVal(value)
	result.SetErr(err)
	return err
}
func budgetBackend(t *testing.T, client *redis.Client) AccountBudgetBackend {
	t.Helper()
	backend, err := NewRedisAccountBudgetBackend(EventBudgetConfig{Enabled: true, ScalePercent: 100}, client)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, backend.Close()) })
	return backend
}
func budgetWireArgs(cmd redis.Cmder) []string {
	values := make([]string, len(cmd.Args()))
	for i, v := range cmd.Args() {
		values[i] = fmt.Sprint(v)
	}
	return values
}
func TestRedisAccountBudgetConstructorValidation(t *testing.T) {
	for _, cfg := range []EventBudgetConfig{{true, 0}, {true, 24}, {true, 401}, {false, 0}, {false, 100}} {
		t.Run(fmt.Sprintf("enabled_%t_scale_%d", cfg.Enabled, cfg.ScalePercent), func(t *testing.T) {
			calls := 0
			client := budgetHookClient(t, func(_ context.Context, cmd redis.Cmder) error {
				calls++
				return budgetReply(t, cmd, []interface{}{int64(1), int64(1), int64(0)}, nil)
			})
			backend, err := NewRedisAccountBudgetBackend(cfg, client)
			require.Error(t, err)
			assert.Nil(t, backend)
			assert.Zero(t, calls)
			// Failed construction must leave ownership with the caller.
			require.NoError(t, client.Close())
			assert.Zero(t, calls)
		})
	}
	backend, err := NewRedisAccountBudgetBackend(EventBudgetConfig{true, 100}, nil)
	require.Error(t, err)
	assert.Nil(t, backend)
}
func TestRedisAccountBudgetConstructorDoesNoCommands(t *testing.T) {
	calls := 0
	client := budgetHookClient(t, func(_ context.Context, cmd redis.Cmder) error {
		calls++
		return budgetReply(t, cmd, nil, errors.New("unexpected command"))
	})
	backend := budgetBackend(t, client)
	assert.Zero(t, calls)
	require.NoError(t, backend.Close())
	_, err := backend.Decide(t.Context(), budgetTestUser, EventClassProfile, 5000000, 8)
	require.ErrorIs(t, err, ErrAccountBudgetUnavailable)
	assert.Zero(t, calls)
}
func TestRedisAccountBudgetExactCommandAndConfigCopy(t *testing.T) {
	var got []string
	client := budgetHookClient(t, func(_ context.Context, cmd redis.Cmder) error {
		got = budgetWireArgs(cmd)
		return budgetReply(t, cmd, []interface{}{int64(1), int64(1), int64(0)}, nil)
	})
	cfg := EventBudgetConfig{true, 100}
	backend, err := NewRedisAccountBudgetBackend(cfg, client)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, backend.Close()) })
	cfg.ScalePercent = 25
	decision, err := backend.Decide(t.Context(), budgetTestUser, EventClassProfile, 5000000, 8)
	require.NoError(t, err)
	assert.Equal(t, RedisBudgetDecision{Allowed: true}, decision)
	// Frozen production-source fixture cache identity; not an integrity check.
	assert.Equal(t, []string{"evalsha", "aca3e0db97c777545e5ee970f170f6d1c6406fc2", "1", budgetTestKey, "1", "5000000", "8"}, got) // pragma: allowlist secret -- verified public Redis script-cache SHA, not a credential
	_, err = backend.Decide(t.Context(), budgetTestUser, EventClassProfile, 20000000, 2)
	assert.ErrorIs(t, err, ErrAccountBudgetUnavailable)
}
func TestRedisAccountBudgetRejectsInputWithoutCommand(t *testing.T) {
	tests := []struct {
		name            string
		user            uuid.UUID
		class           EventClass
		interval, burst int64
	}{
		{"nil account", uuid.Nil, EventClassProfile, 5000000, 8}, {"content", budgetTestUser, EventClassContent, 5000000, 8},
		{"zero class", budgetTestUser, 0, 5000000, 8}, {"unknown class", budgetTestUser, 255, 5000000, 8},
		{"interval mismatch", budgetTestUser, EventClassProfile, 4999999, 8}, {"burst mismatch", budgetTestUser, EventClassProfile, 5000000, 7},
		{"swapped", budgetTestUser, EventClassProfile, 8, 5000000}, {"negative interval", budgetTestUser, EventClassProfile, -1, 8},
		{"zero burst", budgetTestUser, EventClassProfile, 5000000, 0}, {"huge", budgetTestUser, EventClassProfile, 1 << 62, 1 << 62},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			backend := budgetBackend(t, budgetHookClient(t, func(_ context.Context, cmd redis.Cmder) error {
				calls++
				return budgetReply(t, cmd, []interface{}{int64(1), int64(1), int64(0)}, nil)
			}))
			decision, err := backend.Decide(t.Context(), tt.user, tt.class, tt.interval, tt.burst)
			assert.ErrorIs(t, err, ErrAccountBudgetUnavailable)
			assert.Equal(t, RedisBudgetDecision{}, decision)
			assert.Zero(t, calls)
		})
	}
}
func TestRedisAccountBudgetStrictReply(t *testing.T) {
	tests := []struct {
		name    string
		reply   interface{}
		allowed bool
		delay   time.Duration
		valid   bool
	}{
		{"allow", []interface{}{int64(1), int64(1), int64(0)}, true, 0, true},
		{"deny minimum", []interface{}{int64(1), int64(0), int64(1)}, false, time.Millisecond, true},
		{"deny profile", []interface{}{int64(1), int64(0), int64(5000)}, false, 5 * time.Second, true},
		{"deny maximum", []interface{}{int64(1), int64(0), int64(120000)}, false, 120 * time.Second, true},
		{"nil", nil, false, 0, false}, {"scalar", int64(1), false, 0, false},
		{"short", []interface{}{int64(1), int64(1)}, false, 0, false},
		{"long", []interface{}{int64(1), int64(1), int64(0), int64(0)}, false, 0, false},
	}
	for _, v := range []struct{ version, flag, delay int64 }{{0, 1, 0}, {2, 1, 0}, {1, -1, 0}, {1, 2, 0}, {1, 1, 1}, {1, 0, 0}, {1, 0, -1}, {1, 0, 120001}} {
		tests = append(tests, struct {
			name    string
			reply   interface{}
			allowed bool
			delay   time.Duration
			valid   bool
		}{fmt.Sprintf("integer_%d_%d_%d", v.version, v.flag, v.delay), []interface{}{v.version, v.flag, v.delay}, false, 0, false})
	}
	for i := 0; i < 3; i++ {
		for _, bad := range []interface{}{"1", float64(1), true, nil, int(1), []interface{}{int64(1)}} {
			reply := []interface{}{int64(1), int64(1), int64(0)}
			reply[i] = bad
			tests = append(tests, struct {
				name    string
				reply   interface{}
				allowed bool
				delay   time.Duration
				valid   bool
			}{fmt.Sprintf("field_%d_type_%T", i, bad), reply, false, 0, false})
		}
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			backend := budgetBackend(t, budgetHookClient(t, func(_ context.Context, cmd redis.Cmder) error { calls++; return budgetReply(t, cmd, tt.reply, nil) }))
			decision, err := backend.Decide(t.Context(), budgetTestUser, EventClassProfile, 5000000, 8)
			if tt.valid {
				require.NoError(t, err)
				assert.Equal(t, RedisBudgetDecision{Allowed: tt.allowed, RetryAfter: tt.delay}, decision)
			} else {
				assert.ErrorIs(t, err, ErrAccountBudgetUnavailable)
				assert.Equal(t, RedisBudgetDecision{}, decision)
			}
			assert.Equal(t, 1, calls)
		})
	}
}
func TestRedisAccountBudgetFallbackOnlyForNoScript(t *testing.T) {
	for _, tt := range []struct {
		name              string
		first, errorAfter error
		fallback          bool
	}{
		{"noscript", budgetRedisError("NOSCRIPT No matching script"), nil, true},
		{"fallback failure", budgetRedisError("NOSCRIPT No matching script"), io.EOF, true},
		{"lost reply", io.EOF, nil, false}, {"timeout", context.DeadlineExceeded, nil, false},
		{"other redis error", budgetRedisError("ERR canary-key NOSCRIPT"), nil, false},
		{"invalid state", budgetRedisError("ERR WS_EVENT_BUDGET_INVALID_STATE"), nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls [][]string
			var contexts []context.Context
			client := budgetHookClient(t, func(ctx context.Context, cmd redis.Cmder) error {
				calls = append(calls, budgetWireArgs(cmd))
				contexts = append(contexts, ctx)
				err := tt.first
				if len(calls) > 1 {
					err = tt.errorAfter
				}
				return budgetReply(t, cmd, []interface{}{int64(1), int64(1), int64(0)}, err)
			})
			backend := budgetBackend(t, client)
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Millisecond)
			defer cancel()
			decision, err := backend.Decide(ctx, budgetTestUser, EventClassProfile, 5000000, 8)
			if tt.fallback && tt.errorAfter == nil {
				require.NoError(t, err)
				assert.True(t, decision.Allowed)
			} else {
				if tt.name == "invalid state" {
					assert.ErrorIs(t, err, ErrAccountBudgetInvalidState)
				} else {
					assert.ErrorIs(t, err, ErrAccountBudgetUnavailable)
				}
				assert.Equal(t, RedisBudgetDecision{}, decision)
			}
			wantCalls := 1
			if tt.fallback {
				wantCalls = 2
			}
			require.Len(t, calls, wantCalls)
			assert.Equal(t, "evalsha", calls[0][0])
			for _, observed := range contexts {
				assert.Same(t, ctx, observed)
			}
			if tt.fallback {
				assert.Equal(t, []string{"eval", eventAdmissionLua, "1", budgetTestKey, "1", "5000000", "8"}, calls[1])
			}
			if err != nil {
				assert.NotContains(t, err.Error(), "canary-key")
			}
		})
	}
}
func TestRedisAccountBudgetCloseConcurrentAndRetainsResult(t *testing.T) {
	client := budgetHookClient(t, func(_ context.Context, cmd redis.Cmder) error {
		return budgetReply(t, cmd, []interface{}{int64(1), int64(1), int64(0)}, nil)
	})
	backend := budgetBackend(t, client)
	var wg sync.WaitGroup
	results := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- backend.Close() }()
	}
	wg.Wait()
	close(results)
	for err := range results {
		assert.NoError(t, err)
	}
	assert.ErrorIs(t, client.Close(), redis.ErrClosed)
}

func TestRedisAccountBudgetApprovedPairs(t *testing.T) {
	// Literal contract values, independent of accountEventBudget's arithmetic.
	tests := []struct {
		class           EventClass
		scale           int
		interval, burst int64
	}{
		{EventClassSubscription, 25, 50000, 40}, {EventClassSubscription, 100, 12500, 160}, {EventClassSubscription, 400, 3125, 640}, {EventClassSubscription, 33, 37879, 52},
		{EventClassTyping, 25, 400000, 10}, {EventClassTyping, 100, 100000, 40}, {EventClassTyping, 400, 25000, 160},
		{EventClassProfile, 25, 20000000, 2}, {EventClassProfile, 100, 5000000, 8}, {EventClassProfile, 400, 1250000, 32},
		{EventClassServer, 25, 8000000, 4}, {EventClassServer, 100, 2000000, 16}, {EventClassServer, 400, 500000, 64},
		{EventClassPresence, 25, 2000000, 4}, {EventClassPresence, 100, 500000, 16}, {EventClassPresence, 400, 125000, 64},
		{EventClassReady, 25, 4000000, 4}, {EventClassReady, 100, 1000000, 16}, {EventClassReady, 400, 250000, 64},
		{EventClassRenewal, 25, 8000000, 4}, {EventClassRenewal, 100, 2000000, 16}, {EventClassRenewal, 400, 500000, 64},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s_scale%d", tt.class.String(), tt.scale), func(t *testing.T) {
			var calls [][]string
			client := budgetHookClient(t, func(_ context.Context, cmd redis.Cmder) error {
				calls = append(calls, budgetWireArgs(cmd))
				return budgetReply(t, cmd, []interface{}{int64(1), int64(1), int64(0)}, nil)
			})
			backend, err := NewRedisAccountBudgetBackend(EventBudgetConfig{true, tt.scale}, client)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, backend.Close()) })
			decision, err := backend.Decide(t.Context(), budgetTestUser, tt.class, tt.interval, tt.burst)
			require.NoError(t, err)
			assert.True(t, decision.Allowed)
			require.Len(t, calls, 1)
			require.Len(t, calls[0], 7)
			assert.Equal(t, "ratelimit:user:ws:v1:{11111111-1111-4111-8111-111111111111}:"+tt.class.String(), calls[0][3])
			assert.Equal(t, []string{"1", fmt.Sprint(tt.interval), fmt.Sprint(tt.burst)}, calls[0][4:])
			_, err = backend.Decide(t.Context(), budgetTestUser, tt.class, tt.interval, tt.burst+1)
			assert.ErrorIs(t, err, ErrAccountBudgetUnavailable)
			assert.Len(t, calls, 1)
		})
	}
}
func TestRedisAccountBudgetCloseRetainsCheckedFailure(t *testing.T) {
	client := budgetHookClient(t, func(_ context.Context, cmd redis.Cmder) error {
		return budgetReply(t, cmd, nil, errors.New("unexpected command"))
	})
	backend, err := NewRedisAccountBudgetBackend(EventBudgetConfig{true, 100}, client)
	require.NoError(t, err)
	// Deliberately pre-close to give the owned client a deterministic cleanup
	// failure. The backend must retain that checked result on later Close calls.
	require.NoError(t, client.Close())
	first := backend.Close()
	require.ErrorIs(t, first, redis.ErrClosed)
	assert.Equal(t, first, backend.Close())
}

// A context whose cancellation notification has not caught up with its actual
// deadline. Decisions must inspect the deadline instant as well as Err().
type budgetDeadlineContext struct {
	context.Context
	deadline time.Time
}

func (ctx *budgetDeadlineContext) Deadline() (time.Time, bool) { return ctx.deadline, true }

func TestRedisAccountBudgetInvalidContextDoesNoCommand(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	pastDeadline := &budgetDeadlineContext{Context: context.Background(), deadline: time.Unix(1, 0)}
	require.NoError(t, pastDeadline.Err(), "fixture must isolate the actual-deadline guard")
	for _, tt := range []struct {
		name string
		ctx  context.Context
	}{
		{"nil", nil}, {"already canceled", canceled}, {"expired deadline before cancellation notification", pastDeadline},
	} {
		t.Run(tt.name, func(t *testing.T) {
			commands := 0
			backend := budgetBackend(t, budgetHookClient(t, func(_ context.Context, cmd redis.Cmder) error {
				commands++
				return budgetReply(t, cmd, []interface{}{int64(1), int64(1), int64(0)}, nil)
			}))
			decision, err := backend.Decide(tt.ctx, budgetTestUser, EventClassProfile, 5000000, 8)
			assert.ErrorIs(t, err, ErrAccountBudgetUnavailable)
			assert.Equal(t, RedisBudgetDecision{}, decision)
			assert.Zero(t, commands)
		})
	}
}

func TestRedisAccountBudgetRejectsSuccessfulReplyAfterCancellationOrClose(t *testing.T) {
	for _, cause := range []string{"parent cancellation", "backend close", "actual deadline elapsed"} {
		t.Run(cause, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			delayedDeadline := &budgetDeadlineContext{Context: parent, deadline: time.Now().Add(time.Hour)}
			var backend AccountBudgetBackend
			commands := 0
			client := budgetHookClient(t, func(_ context.Context, cmd redis.Cmder) error {
				commands++
				// The external boundary delivers a valid Redis allowance after the
				// lifecycle change. The post-reply guard must invalidate that allowance.
				switch cause {
				case "parent cancellation":
					cancel()
				case "backend close":
					require.NoError(t, backend.Close())
				case "actual deadline elapsed":
					delayedDeadline.deadline = time.Unix(1, 0)
				}
				return budgetReply(t, cmd, []interface{}{int64(1), int64(1), int64(0)}, nil)
			})
			backend = budgetBackend(t, client)
			decision, err := backend.Decide(delayedDeadline, budgetTestUser, EventClassProfile, 5000000, 8)
			assert.ErrorIs(t, err, ErrAccountBudgetUnavailable)
			assert.Equal(t, RedisBudgetDecision{}, decision)
			assert.Equal(t, 1, commands)
			if cause == "actual deadline elapsed" {
				assert.NoError(t, delayedDeadline.Err(), "post-reply deadline fixture must retain Err()==nil")
			}
			if cause == "backend close" {
				later, err := backend.Decide(context.Background(), budgetTestUser, EventClassProfile, 5000000, 8)
				assert.ErrorIs(t, err, ErrAccountBudgetUnavailable)
				assert.Equal(t, RedisBudgetDecision{}, later)
				assert.Equal(t, 1, commands, "post-reply Close must also reject later decisions before command")
			}
		})
	}
}
