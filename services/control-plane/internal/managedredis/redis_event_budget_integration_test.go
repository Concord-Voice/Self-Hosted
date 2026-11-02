//go:build integration || managedredis_integration

package managedredis_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/database"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedredis"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type managedBudgetScriptCounts struct{ evalsha, eval int64 }

func managedBudgetScriptCount(t *testing.T, admin *redis.Client) managedBudgetScriptCounts {
	t.Helper()
	info, err := admin.Info(t.Context(), "commandstats").Result()
	require.NoError(t, err)
	result := managedBudgetScriptCounts{}
	for _, line := range strings.Split(info, "\n") {
		name, stats, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		if name != "cmdstat_evalsha" && name != "cmdstat_eval" {
			continue
		}
		for _, field := range strings.Split(stats, ",") {
			raw, ok := strings.CutPrefix(field, "calls=")
			if !ok {
				continue
			}
			count, err := strconv.ParseInt(raw, 10, 64)
			require.NoError(t, err)
			if name == "cmdstat_evalsha" {
				result.evalsha = count
			} else {
				result.eval = count
			}
		}
	}
	return result
}
func managedBudgetAssertOneScript(t *testing.T, before, after managedBudgetScriptCounts) {
	t.Helper()
	assert.Equal(t, before.evalsha+1, after.evalsha)
	assert.Equal(t, before.eval, after.eval, "preloaded script must not fall back or replay")
}
func managedBudgetUserKey(t *testing.T, suffix string) (uuid.UUID, string) {
	t.Helper()
	user := uuid.NewSHA1(uuid.NameSpaceOID, []byte(t.Name()+"/"+suffix))
	return user, "ratelimit:user:ws:v1:{" + user.String() + "}:profile"
}
func managedBudgetClientCleanup(t *testing.T, client *redis.Client) {
	t.Helper()
	t.Cleanup(func() {
		err := client.Close()
		if !errors.Is(err, redis.ErrClosed) {
			assert.NoError(t, err)
		}
	})
}
func managedBudgetConnectionUsers(ctx context.Context, admin *redis.Client) (map[string]string, error) {
	raw, err := admin.ClientList(ctx).Result()
	if err != nil {
		return nil, err
	}
	users := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		id, user := "", ""
		for _, field := range strings.Fields(line) {
			key, value, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			if key == "id" {
				id = value
			}
			if key == "user" {
				user = value
			}
		}
		if id == "" || user == "" {
			return nil, errors.New("fixture CLIENT LIST omitted identity fields")
		}
		if strings.HasPrefix(user, "cv_cp_") {
			users[id] = user
		}
	}
	return users, nil
}
func managedBudgetRequireIdentity(t *testing.T, client *redis.Client, expected string) {
	t.Helper()
	// These read-only fixture probes await streaming AUTH, not an account
	// decision retry. Retained idle sockets need not eagerly reauthenticate.
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		actual, err := client.ACLWhoAmI(ctx).Result()
		return err == nil && actual == expected
	}, 3*time.Second, time.Millisecond, "client must prove the accepted generation on an actual Redis connection")
}
func managedBudgetObserveDisabledUserConnections(t *testing.T, admin *redis.Client, disabled string) {
	t.Helper()
	users, err := managedBudgetConnectionUsers(t.Context(), admin)
	require.NoError(t, err)
	retained := 0
	for _, user := range users {
		if user == disabled {
			retained++
		}
	}
	// ACL SETUSER off rejects new authentication. It does not disconnect
	// already-authenticated sockets or promise eager idle-pool reauthentication.
	t.Logf("disabled synthetic generation retains %d authenticated fixture connections; no automatic disconnection is asserted", retained)
}
func managedBudgetAssertRevokedCredential(t *testing.T, endpoint string, old managedredis.Credential) {
	t.Helper()
	// Only synthetic fixture credentials enter this URL; constructor errors
	// sanitize it, and no test output prints the endpoint or password.
	value := url.URL{Scheme: "redis", Host: endpoint, User: url.UserPassword(old.Username, old.Password)}
	client, err := database.NewEventBudgetRedisClient(value.String())
	require.NoError(t, err)
	managedBudgetClientCleanup(t, client)
	require.Error(t, client.Ping(t.Context()).Err(), "retired synthetic credential must fail a fresh actual authentication")
	require.NoError(t, client.Close())
}
func managedBudgetDecide(t *testing.T, backend websocket.AccountBudgetBackend, user uuid.UUID) (websocket.RedisBudgetDecision, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Millisecond)
	defer cancel()
	return backend.Decide(ctx, user, websocket.EventClassProfile, 5000000, 8)
}
func managedBudgetExpectRedisAllow(t *testing.T, engine websocket.EventAdmitter, user uuid.UUID) {
	t.Helper()
	require.Equal(t, websocket.AdmissionDecision{Action: websocket.AdmissionAllow, Source: websocket.AdmissionSourceRedis}, engine.Admit(t.Context(), user, websocket.EventPolicy{Class: websocket.EventClassProfile, Scope: websocket.BudgetScopeAccount}, time.Time{}))
}

type managedBudgetCloseObserver struct {
	websocket.AccountBudgetBackend
	closes atomic.Int32
}

func (b *managedBudgetCloseObserver) Close() error {
	b.closes.Add(1)
	return b.AccountBudgetBackend.Close()
}

func TestEventBudgetManagedIntegrationIdentityExpiryAndPostWriteFailure(t *testing.T) {
	fixture := newManagedBudgetFixture(t)
	t.Log("WS_EVENT_BUDGET_MANAGED START")
	admin := fixture.Admin()
	source, err := os.ReadFile(filepath.Join("..", "websocket", "event_admission.lua"))
	require.NoError(t, err)
	require.NoError(t, redis.NewScript(string(source)).Load(t.Context(), admin).Err())
	t.Run("missing PEXPIRE fails after actual HSET", func(t *testing.T) {
		runtime := fixture.NoExpiryRuntime()
		require.NoError(t, runtime.Fresh(fixture.Now()))
		client, err := database.NewManagedEventBudgetRedisClient(fixture.Endpoint(), nil, runtime)
		require.NoError(t, err)
		managedBudgetClientCleanup(t, client)
		require.Same(t, runtime, client.Options().StreamingCredentialsProvider)
		require.NoError(t, client.Ping(t.Context()).Err()) // Prepare before ownership transfer.
		backend, err := websocket.NewRedisAccountBudgetBackend(websocket.EventBudgetConfig{Enabled: true, ScalePercent: 100}, client)
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, backend.Close()) })
		for _, existing := range []bool{false, true} {
			t.Run(fmt.Sprintf("existing%t", existing), func(t *testing.T) {
				user, key := managedBudgetUserKey(t, "post-write")
				require.NoError(t, admin.Del(t.Context(), key).Err())
				t.Cleanup(func() { assert.NoError(t, admin.Del(context.Background(), key).Err()) })
				var previousTat, previousExpiry int64
				if existing {
					now, err := admin.Time(t.Context()).Result()
					require.NoError(t, err)
					last := now.UnixMicro()
					previousTat = last + 5000000
					require.NoError(t, admin.HSet(t.Context(), key, "v", "1", "last_us", strconv.FormatInt(last, 10), "tat_us", strconv.FormatInt(previousTat, 10)).Err())
					require.NoError(t, admin.PExpire(t.Context(), key, 600000*time.Millisecond).Err())
					previousExpiry, err = admin.Do(t.Context(), "PEXPIRETIME", key).Int64()
					require.NoError(t, err)
				}
				before := managedBudgetScriptCount(t, admin)
				decision, err := managedBudgetDecide(t, backend, user)
				require.ErrorIs(t, err, websocket.ErrAccountBudgetUnavailable)
				assert.Equal(t, websocket.RedisBudgetDecision{}, decision)
				after := managedBudgetScriptCount(t, admin)
				managedBudgetAssertOneScript(t, before, after)
				fields, err := admin.HGetAll(t.Context(), key).Result()
				require.NoError(t, err)
				require.Len(t, fields, 3)
				assert.Equal(t, "1", fields["v"])
				last, err := strconv.ParseInt(fields["last_us"], 10, 64)
				require.NoError(t, err)
				tat, err := strconv.ParseInt(fields["tat_us"], 10, 64)
				require.NoError(t, err)
				assert.Equal(t, strconv.FormatInt(last, 10), fields["last_us"])
				assert.Equal(t, strconv.FormatInt(tat, 10), fields["tat_us"])
				if existing {
					assert.Equal(t, previousTat+5000000, tat, "real HSET must add one debit before the ACL error")
					expiry, err := admin.Do(t.Context(), "PEXPIRETIME", key).Int64()
					require.NoError(t, err)
					assert.Equal(t, previousExpiry, expiry, "failed PEXPIRE must leave the prior absolute expiry intact")
				} else {
					assert.Equal(t, int64(5000000), tat-last, "new debt proves the error occurred after mutation")
					ttl, err := admin.Do(t.Context(), "PTTL", key).Int64()
					require.NoError(t, err)
					assert.Equal(t, int64(-1), ttl, "failed PEXPIRE must not invent expiry")
				}
				assert.Equal(t, after, managedBudgetScriptCount(t, admin), "inspecting the uncertain debit cannot replay it")
			})
		}
		require.NoError(t, backend.Close())
		require.Eventually(t, func() bool {
			users, err := managedBudgetConnectionUsers(t.Context(), admin)
			if err != nil {
				return false
			}
			for _, user := range users {
				if user == "cv_cp_99" {
					return false
				}
			}
			return true
		}, time.Second, time.Millisecond)
	})
	t.Run("same runtime rotation freshness and independent shutdown", func(t *testing.T) {
		runtime := fixture.Runtime()
		initial, err := runtime.Snapshot(fixture.Now())
		require.NoError(t, err)
		require.Equal(t, uint64(12), initial.Generation)
		shared, err := database.NewManagedRedisClient(t.Context(), fixture.Endpoint(), nil, runtime)
		require.NoError(t, err)
		managedBudgetClientCleanup(t, shared)
		dedicated, err := database.NewManagedEventBudgetRedisClient(fixture.Endpoint(), nil, runtime)
		require.NoError(t, err)
		managedBudgetClientCleanup(t, dedicated)
		assert.Same(t, runtime, shared.Options().StreamingCredentialsProvider)
		assert.Same(t, runtime, dedicated.Options().StreamingCredentialsProvider)
		require.NoError(t, dedicated.Ping(t.Context()).Err())
		managedBudgetRequireIdentity(t, shared, "cv_cp_12")
		managedBudgetRequireIdentity(t, dedicated, "cv_cp_12")
		realBackend, err := websocket.NewRedisAccountBudgetBackend(websocket.EventBudgetConfig{Enabled: true, ScalePercent: 100}, dedicated)
		require.NoError(t, err)
		backend := &managedBudgetCloseObserver{AccountBudgetBackend: realBackend}
		engine, err := websocket.NewEventAdmitter(websocket.EventBudgetConfig{Enabled: true, ScalePercent: 100}, backend)
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, engine.Close()) })
		user, _ := managedBudgetUserKey(t, "generation12")
		managedBudgetExpectRedisAllow(t, engine, user)
		initialIDs, err := managedBudgetConnectionUsers(t.Context(), admin)
		require.NoError(t, err)
		require.NotEmpty(t, initialIDs)
		require.NoError(t, fixture.Publish(t.Context(), 13))
		require.NoError(t, runtime.Refresh(t.Context()))
		managedBudgetRequireIdentity(t, shared, "cv_cp_13")
		managedBudgetRequireIdentity(t, dedicated, "cv_cp_13")
		user, _ = managedBudgetUserKey(t, "generation13 before retirement")
		managedBudgetExpectRedisAllow(t, engine, user)
		generation13, err := runtime.Snapshot(fixture.Now())
		require.NoError(t, err)
		require.Equal(t, uint64(13), generation13.Generation)
		require.NoError(t, fixture.Disable(t.Context(), 12))
		managedBudgetAssertRevokedCredential(t, fixture.Endpoint(), initial)
		managedBudgetObserveDisabledUserConnections(t, admin, "cv_cp_12")
		managedBudgetRequireIdentity(t, shared, "cv_cp_13")
		managedBudgetRequireIdentity(t, dedicated, "cv_cp_13")
		require.NoError(t, shared.Set(t.Context(), "test_only:managed-budget-shared", "generation13", 0).Err())
		user, _ = managedBudgetUserKey(t, "generation13 after retirement")
		managedBudgetExpectRedisAllow(t, engine, user)
		beforeCloseIDs, err := managedBudgetConnectionUsers(t.Context(), admin)
		require.NoError(t, err)
		require.NoError(t, engine.Close())
		require.NoError(t, engine.Close())
		assert.Equal(t, int32(1), backend.closes.Load())
		assert.Zero(t, dedicated.PoolStats().TotalConns)
		afterCloseIDs, err := managedBudgetConnectionUsers(t.Context(), admin)
		require.NoError(t, err)
		t.Logf("fixture service connection ID observations: initial=%d before-dedicated-close=%d after-close=%d", len(initialIDs), len(beforeCloseIDs), len(afterCloseIDs))
		require.NoError(t, fixture.Publish(t.Context(), 14))
		require.NoError(t, runtime.Refresh(t.Context()))
		managedBudgetRequireIdentity(t, shared, "cv_cp_14")
		require.NoError(t, fixture.Disable(t.Context(), 13))
		managedBudgetAssertRevokedCredential(t, fixture.Endpoint(), generation13)
		managedBudgetObserveDisabledUserConnections(t, admin, "cv_cp_13")
		managedBudgetRequireIdentity(t, shared, "cv_cp_14")
		require.NoError(t, runtime.Fresh(fixture.Now()))
		require.NoError(t, shared.Set(t.Context(), "test_only:managed-budget-shared", "generation14", 0).Err())
		value, err := shared.Get(t.Context(), "test_only:managed-budget-shared").Result()
		require.NoError(t, err)
		assert.Equal(t, "generation14", value)
		assert.Zero(t, dedicated.PoolStats().TotalConns)
		closedUser, _ := managedBudgetUserKey(t, "closed dedicated")
		before := managedBudgetScriptCount(t, admin)
		decision, err := managedBudgetDecide(t, backend, closedUser)
		assert.ErrorIs(t, err, websocket.ErrAccountBudgetUnavailable)
		assert.Equal(t, websocket.RedisBudgetDecision{}, decision)
		assert.Equal(t, before, managedBudgetScriptCount(t, admin), "closed dedicated client must not reopen after later rotation")

		// A distinct fresh owner on the same generation14 runtime proves expiry
		// after the original dedicated client's independent shutdown is complete.
		freshClient, err := database.NewManagedEventBudgetRedisClient(fixture.Endpoint(), nil, runtime)
		require.NoError(t, err)
		managedBudgetClientCleanup(t, freshClient)
		require.NoError(t, freshClient.Ping(t.Context()).Err())
		freshBackend, err := websocket.NewRedisAccountBudgetBackend(websocket.EventBudgetConfig{Enabled: true, ScalePercent: 100}, freshClient)
		require.NoError(t, err)
		freshEngine, err := websocket.NewEventAdmitter(websocket.EventBudgetConfig{Enabled: true, ScalePercent: 100}, freshBackend)
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, freshEngine.Close()) })
		generation14, err := runtime.Snapshot(fixture.Now())
		require.NoError(t, err)
		fixture.Advance(121 * time.Second)
		assert.True(t, runtime.Stale(fixture.Now()), "120-second fetch age is an alert, not refusal")
		require.NoError(t, runtime.Fresh(fixture.Now()))
		user, _ = managedBudgetUserKey(t, "old fetch but credential valid")
		managedBudgetExpectRedisAllow(t, freshEngine, user)
		fixture.Advance(generation14.ValidUntil.Sub(fixture.Now()))
		require.Error(t, runtime.Fresh(fixture.Now()), "actual ValidUntil boundary must refuse")
		staleUser, staleKey := managedBudgetUserKey(t, "expired credential")
		before = managedBudgetScriptCount(t, admin)
		decision, err = managedBudgetDecide(t, freshBackend, staleUser)
		require.ErrorIs(t, err, websocket.ErrAccountBudgetUnavailable)
		assert.Equal(t, websocket.RedisBudgetDecision{}, decision)
		assert.Equal(t, before, managedBudgetScriptCount(t, admin), "freshness hook must stop productive Lua before Redis")
		require.Error(t, shared.Ping(t.Context()).Err())
		exists, err := admin.Exists(t.Context(), staleKey).Result()
		require.NoError(t, err)
		assert.Zero(t, exists)
		engineDecision := freshEngine.Admit(t.Context(), staleUser, websocket.EventPolicy{Class: websocket.EventClassProfile, Scope: websocket.BudgetScopeAccount}, time.Time{})
		assert.Equal(t, websocket.AdmissionDecision{Action: websocket.AdmissionRefuse, Source: websocket.AdmissionSourceUnavailable, RetryAfter: time.Second}, engineDecision)
		assert.Equal(t, before, managedBudgetScriptCount(t, admin))
		require.NoError(t, freshEngine.Close())
		assert.Zero(t, freshClient.PoolStats().TotalConns)
		require.NoError(t, admin.Ping(t.Context()).Err())
	})
	if !t.Failed() {
		t.Log("WS_EVENT_BUDGET_MANAGED PASS")
	}
}
