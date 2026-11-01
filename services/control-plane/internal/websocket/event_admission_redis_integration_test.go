package websocket

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Opt in only with a disposable Redis endpoint supplied through redistest's
// REDIS_URL configuration. These tests never need the developer database,
// database resets or service-global script-cache mutations.
func budgetRealRedis(t *testing.T) *redis.Client {
	t.Helper()
	if os.Getenv("WS_EVENT_BUDGET_REDIS_INTEGRATION") != "1" {
		t.Skip("set WS_EVENT_BUDGET_REDIS_INTEGRATION=1 with an isolated REDIS_URL")
	}
	opts := redistest.Options(t)
	opts.MaxRetries = -1
	opts.DialerRetries = 1
	opts.ContextTimeoutEnabled = true
	opts.DialTimeout = 100 * time.Millisecond
	opts.ReadTimeout = 100 * time.Millisecond
	opts.WriteTimeout = 100 * time.Millisecond
	opts.PoolTimeout = 100 * time.Millisecond
	opts.MaintNotificationsConfig = &maintnotifications.Config{Mode: maintnotifications.ModeDisabled}
	client := redis.NewClient(opts)
	require.NoError(t, client.Ping(t.Context()).Err())
	t.Cleanup(func() {
		err := client.Close()
		if !errors.Is(err, redis.ErrClosed) {
			assert.NoError(t, err)
		}
	})
	return client
}
func budgetRealKey(t *testing.T, client *redis.Client) string {
	t.Helper()
	key := "ratelimit:user:ws:v1:{" + uuid.NewSHA1(uuid.NameSpaceOID, []byte(t.Name())).String() + "}:profile"
	require.NoError(t, client.Del(t.Context(), key).Err())
	t.Cleanup(func() {
		if err := client.Del(context.Background(), key).Err(); !errors.Is(err, redis.ErrClosed) {
			assert.NoError(t, err)
		}
	})
	return key
}
func budgetReplaceOnce(t *testing.T, source, old, replacement string) string {
	t.Helper()
	require.Equal(t, 1, strings.Count(source, old), "test seam must change exactly one expression")
	return strings.Replace(source, old, replacement, 1)
}
func budgetClockScript(t *testing.T, clock string, ttl bool) string {
	t.Helper()
	source := budgetReplaceOnce(t, eventAdmissionLua, "local clock = redis.call('TIME')", "local clock = "+clock)
	if ttl {
		source = "local budget_test_ttl = -1\n" + source
		source = budgetReplaceOnce(t, source, "redis.call('PEXPIRE', key, ttl)", "budget_test_ttl = ttl\nredis.call('PEXPIRE', key, ttl)")
		source = budgetReplaceOnce(t, source, "return {1, allowed, retry}", "return {1, allowed, retry, budget_test_ttl}")
	}
	return source
}
func budgetClockLiteral(wall int64) string {
	return fmt.Sprintf("{'%d','%d'}", wall/1000000, wall%1000000)
}
func budgetSeed(t *testing.T, c *redis.Client, key, last, tat string) {
	t.Helper()
	require.NoError(t, c.HSet(t.Context(), key, "v", "1", "last_us", last, "tat_us", tat).Err())
	require.NoError(t, c.PExpire(t.Context(), key, 10*time.Minute).Err())
}
func budgetEval(t *testing.T, c *redis.Client, script, key string, interval, burst int64) []interface{} {
	t.Helper()
	result, err := c.Eval(t.Context(), script, []string{key}, "1", strconv.FormatInt(interval, 10), strconv.FormatInt(burst, 10)).Result()
	require.NoError(t, err)
	reply, ok := result.([]interface{})
	require.True(t, ok, "reply must be RESP array")
	return reply
}
func TestRedisAccountBudgetIntegrationVectors(t *testing.T) {
	c := budgetRealRedis(t)
	const T int64 = 1800000000000000
	tests := []struct {
		name, last, tat                            string
		wall, interval, burst, allowed, retry, ttl int64
		wantLast, wantTat                          string
	}{
		{"P1 pristine", "", "", T, 5000000, 8, 1, 0, 600000, "1800000000000000", "1800000005000000"},
		{"P3 ninth", "1800000000000000", "1800000040000000", T, 5000000, 8, 0, 5000, 600000, "1800000000000000", "1800000040000000"},
		{"P4 fractional edge", "1800000000000000", "1800000040000000", T + 4999999, 5000000, 8, 0, 1, 600001, "1800000004999999", "1800000040000000"},
		{"P5 exact refill", "1800000000000000", "1800000040000000", T + 5000000, 5000000, 8, 1, 0, 600000, "1800000005000000", "1800000045000000"},
		{"P6 positive 999us", "1800000000000000", "1800000040000000", T + 4999001, 5000000, 8, 0, 1, 600001, "1800000004999001", "1800000040000000"},
		{"R1 rollback", "1800000000000000", "1800000040000000", T - 3600000000, 5000000, 8, 0, 5000, 4200000, "1800000000000000", "1800000040000000"},
		{"R2 rollback 600s later", "1800000000000000", "1800000040000000", T - 3000000000, 5000000, 8, 0, 5000, 3600000, "1800000000000000", "1800000040000000"},
		{"R3 nonaligned rollback", "1800000000000001", "1800000040000001", T - 3600000000 + 1, 5000000, 8, 0, 5000, 4200001, "1800000000000001", "1800000040000001"},
		{"R4 repeated rollback", "1800000000000000", "1800000040000000", T - 7200000000, 5000000, 8, 0, 5000, 7800000, "1800000000000000", "1800000040000000"},
		{"R5 forward jump", "1800000000000000", "1800000040000000", T + 60000000, 5000000, 8, 1, 0, 600000, "1800000060000000", "1800000065000000"},
		{"S1 narrower config", "1800000000000000", "1800000040000000", T, 20000000, 2, 0, 20000, 600000, "1800000000000000", "1800000040000000"},
		{"S2 wider config", "1800000000000000", "1800000040000000", T, 1250000, 32, 0, 1250, 600000, "1800000000000000", "1800000040000000"},
		{"I1 negative idle debt", "1800000000000000", "1799999000000000", T, 5000000, 8, 1, 0, 600000, "1800000000000000", "1800000005000000"},
		{"I2 maximum debt", "1800000000000000", "1800000120000000", T, 5000000, 8, 0, 85000, 600000, "1800000000000000", "1800000120000000"},
		{"E1 retained horizon", "", "", 9007198654740000, 5000000, 8, 1, 0, 600000, "9007198654740000", "9007198659740000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := budgetRealKey(t, c)
			if tt.last != "" {
				budgetSeed(t, c, key, tt.last, tt.tat)
			}
			script := budgetClockScript(t, budgetClockLiteral(tt.wall), true)
			assert.Equal(t, []interface{}{int64(1), tt.allowed, tt.retry, tt.ttl}, budgetEval(t, c, script, key, tt.interval, tt.burst))
			fields, err := c.HGetAll(t.Context(), key).Result()
			require.NoError(t, err)
			assert.Equal(t, map[string]string{"v": "1", "last_us": tt.wantLast, "tat_us": tt.wantTat}, fields)
		})
	}
	t.Run("P2 sequential burst", func(t *testing.T) {
		key := budgetRealKey(t, c)
		script := budgetClockScript(t, budgetClockLiteral(T), true)
		for i := 0; i < 8; i++ {
			assert.Equal(t, []interface{}{int64(1), int64(1), int64(0), int64(600000)}, budgetEval(t, c, script, key, 5000000, 8))
		}
		fields, err := c.HGetAll(t.Context(), key).Result()
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"v": "1", "last_us": "1800000000000000", "tat_us": "1800000040000000"}, fields)
		assert.Equal(t, []interface{}{int64(1), int64(0), int64(5000), int64(600000)}, budgetEval(t, c, script, key, 5000000, 8))
	})
}

type budgetStateSnapshot struct {
	kind, dump string
	expiry     int64
}

func budgetSnapshot(t *testing.T, c *redis.Client, key string) budgetStateSnapshot {
	t.Helper()
	kind, err := c.Type(t.Context(), key).Result()
	require.NoError(t, err)
	dump, err := c.Dump(t.Context(), key).Result()
	if errors.Is(err, redis.Nil) {
		dump = ""
	} else {
		require.NoError(t, err)
	}
	expiry, err := c.Do(t.Context(), "PEXPIRETIME", key).Int64()
	require.NoError(t, err)
	return budgetStateSnapshot{kind, dump, expiry}
}
func budgetAssertInvalidUnchanged(t *testing.T, c *redis.Client, key, script string, keys []string, args ...interface{}) {
	t.Helper()
	before := budgetSnapshot(t, c, key)
	_, err := c.Eval(t.Context(), script, keys, args...).Result()
	require.EqualError(t, err, "ERR WS_EVENT_BUDGET_INVALID_STATE")
	assert.Equal(t, before, budgetSnapshot(t, c, key), "rejection must preserve bytes and absolute expiry")
}
func TestRedisAccountBudgetIntegrationCorruptionWritesNothing(t *testing.T) {
	c := budgetRealRedis(t)
	script := budgetClockScript(t, "{'1800000000','0'}", false)
	valid := map[string]string{"v": "1", "last_us": "1800000000000000", "tat_us": "1800000040000000"}
	values := []string{"", "00", "01", "+1", "-1", "1.0", "1e3", " 1", "1 ", "١", "9007199254740992", "10000000000000000"}
	type corruptionCase struct {
		name      string
		fields    map[string]string
		wrongType bool
	}
	tests := make([]corruptionCase, 0, 5+len(values)*3+2)
	tests = append(tests, []corruptionCase{
		{"wrong string type", nil, true},
		{"one field", map[string]string{"v": "1"}, false},
		{"two fields", map[string]string{"v": "1", "last_us": "1800000000000000"}, false},
		{"four fields", map[string]string{"v": "1", "last_us": "1800000000000000", "tat_us": "1800000040000000", "extra": "1"}, false},
		{"missing version", map[string]string{"other": "1", "last_us": "1800000000000000", "tat_us": "1800000040000000"}, false},
	}...)
	for _, field := range []string{"v", "last_us", "tat_us"} {
		fieldValues := []string{"01", "2"}
		if field != "v" {
			fieldValues = values
		}
		for _, value := range fieldValues {
			fields := make(map[string]string)
			for k, v := range valid {
				fields[k] = v
			}
			fields[field] = value
			tests = append(tests, struct {
				name      string
				fields    map[string]string
				wrongType bool
			}{field + "=" + value, fields, false})
		}
	}
	tests = append(tests, struct {
		name      string
		fields    map[string]string
		wrongType bool
	}{"I3 positive debt overflow", map[string]string{"v": "1", "last_us": "1800000000000000", "tat_us": "1800000120000001"}, false}, struct {
		name      string
		fields    map[string]string
		wrongType bool
	}{"E3 exact maximum", map[string]string{"v": "1", "last_us": "9007199254740991", "tat_us": "9007199254740991"}, false})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := budgetRealKey(t, c)
			if tt.wrongType {
				require.NoError(t, c.Set(t.Context(), key, "corrupt-fixture", 10*time.Minute).Err())
			} else {
				require.NoError(t, c.HSet(t.Context(), key, tt.fields).Err())
				require.NoError(t, c.PExpire(t.Context(), key, 10*time.Minute).Err())
			}
			budgetAssertInvalidUnchanged(t, c, key, script, []string{key}, "1", "5000000", "8")
		})
	}
}
func TestRedisAccountBudgetIntegrationInvalidRequestsWriteNothing(t *testing.T) {
	c := budgetRealRedis(t)
	type requestCase struct {
		name, clock string
		args        []interface{}
		keyCount    int
	}
	tests := make([]requestCase, 0, 27)
	tests = append(tests, []requestCase{
		{"no key", "{'1800000000','0'}", []interface{}{"1", "5000000", "8"}, 0},
		{"two keys", "{'1800000000','0'}", []interface{}{"1", "5000000", "8"}, 2},
		{"short args", "{'1800000000','0'}", []interface{}{"1", "5000000"}, 1},
		{"extra args", "{'1800000000','0'}", []interface{}{"1", "5000000", "8", "0"}, 1},
		{"E2 unsafe horizon", "{'9007198654','740001'}", []interface{}{"1", "5000000", "8"}, 1},
	}...)
	for _, args := range [][]interface{}{{"01", "5000000", "8"}, {"2", "5000000", "8"}, {"1", "05000000", "8"}, {"1", "5e6", "8"}, {"1", "5000000", "08"}, {"1", "3124", "8"}, {"1", "20000001", "2"}, {"1", "5000000", "1"}, {"1", "3125", "641"}, {"1", "20000000", "8"}, {"1", "-1", "8"}} {
		tests = append(tests, struct {
			name, clock string
			args        []interface{}
			keyCount    int
		}{fmt.Sprint(args), "{'1800000000','0'}", args, 1})
	}
	for _, clock := range []string{"{}", "{'1800000000'}", "{'1800000000','0','1'}", "{'1800000000','1000000'}", "{'-1','0'}", "{'01','0'}", "{'1800000000','00'}", "{'9007199255','0'}", "{1800000000,'0'}", "{'1800000000',0}", "{'1800000000','-1'}"} {
		tests = append(tests, struct {
			name, clock string
			args        []interface{}
			keyCount    int
		}{"clock " + clock, clock, []interface{}{"1", "5000000", "8"}, 1})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := budgetRealKey(t, c)
			keys := []string{}
			for i := 0; i < tt.keyCount; i++ {
				keys = append(keys, key)
			}
			budgetAssertInvalidUnchanged(t, c, key, budgetClockScript(t, tt.clock, false), keys, tt.args...)
		})
	}
}
func TestRedisAccountBudgetIntegrationProductionClockAndAdapter(t *testing.T) {
	c := budgetRealRedis(t)
	inspector := budgetRealRedis(t)
	t.Log("Redis server information is recorded by root's isolated fixture; production TIME runs unchanged")
	user := uuid.NewSHA1(uuid.NameSpaceOID, []byte(t.Name()))
	key := "ratelimit:user:ws:v1:{" + user.String() + "}:profile"
	require.NoError(t, inspector.Del(t.Context(), key).Err())
	t.Cleanup(func() {
		assert.NoError(t, inspector.Del(context.Background(), key).Err())
	})
	backend := budgetBackend(t, c)
	decision, err := backend.Decide(t.Context(), user, EventClassProfile, 5000000, 8)
	require.NoError(t, err)
	assert.Equal(t, RedisBudgetDecision{Allowed: true}, decision)
	fields, err := c.HGetAll(t.Context(), key).Result()
	require.NoError(t, err)
	require.Len(t, fields, 3)
	last, err := strconv.ParseInt(fields["last_us"], 10, 64)
	require.NoError(t, err)
	tat, err := strconv.ParseInt(fields["tat_us"], 10, 64)
	require.NoError(t, err)
	assert.Equal(t, int64(5000000), tat-last)
	assert.Equal(t, "1", fields["v"])
	assert.Less(t, time.Since(time.UnixMicro(last)).Abs(), 10*time.Second)
	ttl, err := c.PTTL(t.Context(), key).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, 590*time.Second)
	assert.LessOrEqual(t, ttl, 600001*time.Millisecond)
}
func TestRedisAccountBudgetIntegrationTwoClientsAtomicBurst(t *testing.T) {
	c := budgetRealRedis(t)
	other := budgetRealRedis(t)
	key := budgetRealKey(t, c)
	script := budgetClockScript(t, "{'1800000000','0'}", false)
	type result struct {
		reply interface{}
		err   error
	}
	results := make(chan result, 16)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			client := c
			if index%2 == 1 {
				client = other
			}
			v, err := client.Eval(t.Context(), script, []string{key}, "1", "5000000", "8").Result()
			results <- result{v, err}
		}(i)
	}
	wg.Wait()
	close(results)
	allows := 0
	denies := 0
	for got := range results {
		require.NoError(t, got.err)
		if reflect.DeepEqual([]interface{}{int64(1), int64(1), int64(0)}, got.reply) {
			allows++
		} else {
			assert.Equal(t, []interface{}{int64(1), int64(0), int64(5000)}, got.reply)
			denies++
		}
	}
	assert.Equal(t, 8, allows)
	assert.Equal(t, 8, denies)
	fields, err := c.HGetAll(t.Context(), key).Result()
	require.NoError(t, err)
	assert.Equal(t, "1800000040000000", fields["tat_us"])
}

// This hook preserves real server execution except for either the first cache
// miss or the reply delivery. Neither scenario changes service-global cache.
type budgetForwardHook struct {
	run func(context.Context, redis.Cmder, redis.ProcessHook) error
}

func (h budgetForwardHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h budgetForwardHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error { return h.run(ctx, cmd, next) }
}
func (h budgetForwardHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func TestRedisAccountBudgetIntegrationNoScriptFallbackExecutesRealScript(t *testing.T) {
	c := budgetRealRedis(t)
	inspector := budgetRealRedis(t)
	var calls [][]string
	c.AddHook(budgetForwardHook{run: func(ctx context.Context, cmd redis.Cmder, next redis.ProcessHook) error {
		if cmd.Name() != "evalsha" && cmd.Name() != "eval" {
			return next(ctx, cmd)
		}
		calls = append(calls, budgetWireArgs(cmd))
		if len(calls) == 1 {
			return budgetReply(t, cmd, nil, budgetRedisError("NOSCRIPT test cache miss"))
		}
		return next(ctx, cmd)
	}})
	backend := budgetBackend(t, c)
	user := uuid.NewSHA1(uuid.NameSpaceOID, []byte(t.Name()))
	key := "ratelimit:user:ws:v1:{" + user.String() + "}:profile"
	require.NoError(t, inspector.Del(t.Context(), key).Err())
	t.Cleanup(func() { assert.NoError(t, inspector.Del(context.Background(), key).Err()) })
	decision, err := backend.Decide(t.Context(), user, EventClassProfile, 5000000, 8)
	require.NoError(t, err)
	assert.True(t, decision.Allowed)
	require.Len(t, calls, 2)
	assert.Equal(t, "evalsha", calls[0][0])
	assert.Equal(t, []string{"eval", eventAdmissionLua, "1", key, "1", "5000000", "8"}, calls[1])
	fields, err := inspector.HGetAll(t.Context(), key).Result()
	require.NoError(t, err)
	require.Len(t, fields, 3)
	last, err := strconv.ParseInt(fields["last_us"], 10, 64)
	require.NoError(t, err)
	tat, err := strconv.ParseInt(fields["tat_us"], 10, 64)
	require.NoError(t, err)
	assert.Equal(t, int64(5000000), tat-last)
}
func TestRedisAccountBudgetIntegrationLostReplyDoesNotReplayDebit(t *testing.T) {
	c := budgetRealRedis(t)
	inspector := budgetRealRedis(t)
	// Administrative fixture preload, never a production adapter command.
	require.NoError(t, redis.NewScript(eventAdmissionLua).Load(t.Context(), inspector).Err())
	commands := 0
	c.AddHook(budgetForwardHook{run: func(ctx context.Context, cmd redis.Cmder, next redis.ProcessHook) error {
		if cmd.Name() != "evalsha" && cmd.Name() != "eval" {
			return next(ctx, cmd)
		}
		commands++
		if err := next(ctx, cmd); err != nil {
			return err
		}
		return budgetReply(t, cmd, nil, io.EOF)
	}})
	backend := budgetBackend(t, c)
	user := uuid.NewSHA1(uuid.NameSpaceOID, []byte(t.Name()))
	key := "ratelimit:user:ws:v1:{" + user.String() + "}:profile"
	require.NoError(t, inspector.Del(t.Context(), key).Err())
	t.Cleanup(func() { assert.NoError(t, inspector.Del(context.Background(), key).Err()) })
	decision, err := backend.Decide(t.Context(), user, EventClassProfile, 5000000, 8)
	assert.ErrorIs(t, err, ErrAccountBudgetUnavailable)
	assert.Equal(t, RedisBudgetDecision{}, decision)
	assert.Equal(t, 1, commands)
	fields, err := inspector.HGetAll(t.Context(), key).Result()
	require.NoError(t, err)
	require.Len(t, fields, 3)
	last, err := strconv.ParseInt(fields["last_us"], 10, 64)
	require.NoError(t, err)
	tat, err := strconv.ParseInt(fields["tat_us"], 10, 64)
	require.NoError(t, err)
	assert.Equal(t, int64(5000000), tat-last, "real debit survives lost delivery and is not repeated")
}

func TestRedisAccountBudgetIntegrationAdapterDebitsAndIsolatesIdentity(t *testing.T) {
	c := budgetRealRedis(t)
	inspector := budgetRealRedis(t)
	backend := budgetBackend(t, c)
	first := uuid.NewSHA1(uuid.NameSpaceOID, []byte(t.Name()+"/first"))
	second := uuid.NewSHA1(uuid.NameSpaceOID, []byte(t.Name()+"/second"))
	keys := []string{"ratelimit:user:ws:v1:{" + first.String() + "}:profile", "ratelimit:user:ws:v1:{" + second.String() + "}:profile", "ratelimit:user:ws:v1:{" + first.String() + "}:server"}
	require.NoError(t, inspector.Del(t.Context(), keys...).Err())
	t.Cleanup(func() { assert.NoError(t, inspector.Del(context.Background(), keys...).Err()) })
	for i := 0; i < 8; i++ {
		decision, err := backend.Decide(t.Context(), first, EventClassProfile, 5000000, 8)
		require.NoError(t, err)
		require.Equal(t, RedisBudgetDecision{Allowed: true}, decision)
	}
	denied, err := backend.Decide(t.Context(), first, EventClassProfile, 5000000, 8)
	require.NoError(t, err)
	assert.False(t, denied.Allowed)
	assert.Greater(t, denied.RetryAfter, time.Duration(0))
	assert.LessOrEqual(t, denied.RetryAfter, 5*time.Second)
	for _, tt := range []struct {
		user            uuid.UUID
		class           EventClass
		interval, burst int64
	}{{second, EventClassProfile, 5000000, 8}, {first, EventClassServer, 2000000, 16}} {
		decision, err := backend.Decide(t.Context(), tt.user, tt.class, tt.interval, tt.burst)
		require.NoError(t, err)
		assert.Equal(t, RedisBudgetDecision{Allowed: true}, decision)
	}
	for _, key := range keys {
		fields, err := inspector.HGetAll(t.Context(), key).Result()
		require.NoError(t, err)
		assert.Len(t, fields, 3)
	}
}
