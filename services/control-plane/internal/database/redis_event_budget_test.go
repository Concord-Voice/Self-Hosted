package database

import (
	"crypto/tls"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedredis"
	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func eventBudgetClientCleanup(t *testing.T, c *redis.Client) {
	t.Helper()
	t.Cleanup(func() {
		err := c.Close()
		if !errors.Is(err, redis.ErrClosed) {
			assert.NoError(t, err)
		}
	})
}
func assertEventBudgetOptions(t *testing.T, c *redis.Client) {
	t.Helper()
	opts := c.Options()
	assert.Equal(t, 0, opts.MaxRetries, "NewClient normalizes constructor -1 to zero retries")
	assert.Equal(t, 1, opts.DialerRetries)
	assert.True(t, opts.ContextTimeoutEnabled)
	assert.Equal(t, 32, opts.PoolSize)
	assert.Equal(t, 32, opts.MaxActiveConns)
	assert.Equal(t, 32, opts.MaxConcurrentDials)
	assert.Zero(t, opts.MinIdleConns)
	assert.Equal(t, 100*time.Millisecond, opts.DialTimeout)
	assert.Equal(t, 100*time.Millisecond, opts.ReadTimeout)
	assert.Equal(t, 100*time.Millisecond, opts.WriteTimeout)
	assert.Equal(t, 100*time.Millisecond, opts.PoolTimeout)
	require.NotNil(t, opts.MaintNotificationsConfig)
	assert.Equal(t, maintnotifications.ModeDisabled, opts.MaintNotificationsConfig.Mode)
}
func TestNewEventBudgetRedisClientOptionsAndNoConstructionCommands(t *testing.T) {
	mini := miniredis.RunT(t)
	client, err := NewEventBudgetRedisClient("redis://" + mini.Addr() + "/4?protocol=2&max_retries=12&pool_size=99&min_idle_conns=3&dial_timeout=2s&read_timeout=2s&write_timeout=2s&pool_timeout=2s")
	require.NoError(t, err)
	eventBudgetClientCleanup(t, client)
	assertEventBudgetOptions(t, client)
	assert.Equal(t, 2, client.Options().Protocol)
	assert.Equal(t, 4, client.Options().DB)
	assert.Equal(t, mini.Addr(), client.Options().Addr)
	assert.Zero(t, mini.CommandCount())
	assert.Zero(t, mini.CurrentConnectionCount())
	require.NoError(t, client.Ping(t.Context()).Err())
	assert.Positive(t, mini.CommandCount(), "the deferred client remains usable")
}
func TestNewEventBudgetRedisClientPreservesAuthenticationAndTLS(t *testing.T) {
	mini := miniredis.RunT(t)
	mini.RequireUserAuth("budget-fixture-user", "synthetic-budget-password")
	client, err := NewEventBudgetRedisClient("redis://budget-fixture-user:synthetic-budget-password@" + mini.Addr() + "/3?protocol=2") // pragma: allowlist secret -- synthetic fixture
	require.NoError(t, err)
	eventBudgetClientCleanup(t, client)
	assert.Equal(t, "budget-fixture-user", client.Options().Username)
	assert.Equal(t, "synthetic-budget-password", client.Options().Password)
	require.NoError(t, client.Set(t.Context(), "event-budget-client-fixture", "value", 0).Err())
	value, err := mini.DB(3).Get("event-budget-client-fixture")
	require.NoError(t, err)
	assert.Equal(t, "value", value)
	assert.False(t, mini.DB(0).Exists("event-budget-client-fixture"))
	secure, err := NewEventBudgetRedisClient("rediss://budget-fixture-user:synthetic-budget-password@127.0.0.1:1/7?protocol=3") // pragma: allowlist secret -- synthetic fixture
	require.NoError(t, err)
	eventBudgetClientCleanup(t, secure)
	require.NotNil(t, secure.Options().TLSConfig)
	assert.Equal(t, 3, secure.Options().Protocol)
	assert.Equal(t, 7, secure.Options().DB)
	assert.Equal(t, "budget-fixture-user", secure.Options().Username)
	assertEventBudgetOptions(t, secure)
}
func TestNewEventBudgetRedisClientRejectsMalformedURLWithoutLeakingCredentials(t *testing.T) {
	raw := "redis://fixture-user:synthetic-budget-canary@%zz" // pragma: allowlist secret -- malformed fixture
	client, err := NewEventBudgetRedisClient(raw)
	require.Error(t, err)
	assert.Nil(t, client)
	assert.NotContains(t, err.Error(), "synthetic-budget-canary")
	assert.NotContains(t, err.Error(), raw)
}
func TestNewManagedEventBudgetRedisClientValidatesWithoutFreshnessProbe(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	credential := managedRedisTestCredential(now)
	reader := &rotatingManagedRedisReader{credential: credential}
	cold, err := managedredis.NewRuntime(reader, credential.Scope, "control-plane", func() time.Time { return now })
	require.NoError(t, err)
	mini := miniredis.RunT(t)
	config := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: "fixture.example"}
	warm := managedRedisTestRuntime(t, &now)
	for _, runtime := range []*managedredis.Runtime{cold, warm} {
		client, err := NewManagedEventBudgetRedisClient(mini.Addr(), config, runtime)
		require.NoError(t, err)
		eventBudgetClientCleanup(t, client)
		assertEventBudgetOptions(t, client)
		assert.Same(t, runtime, client.Options().StreamingCredentialsProvider)
		assert.Empty(t, client.Options().Username)
		assert.Empty(t, client.Options().Password)
		assert.Nil(t, client.Options().CredentialsProvider)
		assert.Nil(t, client.Options().CredentialsProviderContext)
		require.NotSame(t, config, client.Options().TLSConfig)
		assert.Equal(t, "fixture.example", client.Options().TLSConfig.ServerName)
		config.ServerName = "mutated.example"
		assert.Equal(t, "fixture.example", client.Options().TLSConfig.ServerName)
		config.ServerName = "fixture.example"
	}
	assert.Zero(t, mini.CommandCount())
	assert.Zero(t, mini.CurrentConnectionCount())
	// An expired runtime is not a constructor prerequisite either; the actual
	// command is where freshness is enforced.
	now = now.Add(2 * time.Hour)
	stale, err := NewManagedEventBudgetRedisClient(mini.Addr(), nil, warm)
	require.NoError(t, err)
	eventBudgetClientCleanup(t, stale)
	require.ErrorContains(t, stale.Ping(t.Context()).Err(), "stale")
	assert.Zero(t, mini.CommandCount())
	for _, endpoint := range []string{"", "malformed", "redis://fixture:synthetic-budget-canary@127.0.0.1:6379", "127.0.0.1:6379/3"} { // pragma: allowlist secret -- synthetic URL tests credential-safe error output
		client, err := NewManagedEventBudgetRedisClient(endpoint, nil, cold)
		require.Error(t, err)
		assert.Nil(t, client)
		assert.NotContains(t, err.Error(), "synthetic-budget-canary")
	}
	client, err := NewManagedEventBudgetRedisClient(mini.Addr(), nil, nil)
	require.Error(t, err)
	assert.Nil(t, client)
}
func TestNewManagedEventBudgetRedisClientUsesFreshnessOnActualCommands(t *testing.T) {
	mini := miniredis.RunT(t)
	now := time.Now().UTC().Truncate(time.Second)
	mini.RequireUserAuth("cv_cp_12", "synthetic-password")
	runtime := managedRedisTestRuntime(t, &now)
	client, err := NewManagedEventBudgetRedisClient(mini.Addr(), nil, runtime)
	require.NoError(t, err)
	eventBudgetClientCleanup(t, client)
	assert.Zero(t, mini.CommandCount())
	require.NoError(t, client.Ping(t.Context()).Err())
	before := mini.CommandCount()
	now = now.Add(2 * time.Hour)
	require.ErrorContains(t, client.Ping(t.Context()).Err(), "stale")
	assert.Equal(t, before, mini.CommandCount())
	pipe := client.Pipeline()
	pipe.Ping(t.Context())
	_, err = pipe.Exec(t.Context())
	require.ErrorContains(t, err, "stale")
	assert.Equal(t, before, mini.CommandCount())
	// A newly allocated client must reject at dial/command freshness too.
	cold, err := NewManagedEventBudgetRedisClient(mini.Addr(), nil, runtime)
	require.NoError(t, err)
	eventBudgetClientCleanup(t, cold)
	require.ErrorContains(t, cold.Ping(t.Context()).Err(), "stale")
	assert.Equal(t, before, mini.CommandCount())
}

func TestEventBudgetDedicatedClientCloseKeepsSharedClientAndRuntime(t *testing.T) {
	mini := miniredis.RunT(t)
	now := time.Now().UTC().Truncate(time.Second)
	initial := managedRedisTestCredential(now)
	mini.RequireUserAuth(initial.Username, initial.Password)
	reader := &rotatingManagedRedisReader{credential: initial}
	runtime, err := managedredis.NewRuntime(reader, initial.Scope, "control-plane", func() time.Time { return now })
	require.NoError(t, err)
	require.NoError(t, runtime.Refresh(t.Context()))
	var mu sync.Mutex
	authUsers := make(map[string]int)
	mini.Server().SetPreHook(func(_ *server.Peer, command string, args ...string) bool {
		if strings.EqualFold(command, "AUTH") && len(args) == 2 {
			mu.Lock()
			authUsers[args[0]]++
			mu.Unlock()
		}
		return false
	})
	shared, err := NewManagedRedisClient(t.Context(), mini.Addr(), nil, runtime)
	require.NoError(t, err)
	eventBudgetClientCleanup(t, shared)
	before := *shared.Options()
	dedicated, err := NewManagedEventBudgetRedisClient(mini.Addr(), nil, runtime)
	require.NoError(t, err)
	eventBudgetClientCleanup(t, dedicated)
	require.NotSame(t, shared, dedicated)
	require.NotSame(t, shared.Options(), dedicated.Options())
	assert.Equal(t, before.MaxRetries, shared.Options().MaxRetries)
	assert.Equal(t, before.PoolSize, shared.Options().PoolSize)
	assert.Equal(t, before.MinIdleConns, shared.Options().MinIdleConns)
	assert.Equal(t, before.ReadTimeout, shared.Options().ReadTimeout)
	assert.Equal(t, before.DialTimeout, shared.Options().DialTimeout)
	assert.Equal(t, before.MaintNotificationsConfig.Mode, shared.Options().MaintNotificationsConfig.Mode)
	require.NoError(t, dedicated.Ping(t.Context()).Err())
	require.Eventually(t, func() bool { return mini.CurrentConnectionCount() == 2 }, time.Second, time.Millisecond)
	next := initial
	next.Generation = 13
	next.Username = "cv_cp_13"
	next.Password = "synthetic-next-password" // pragma: allowlist secret -- local miniredis rotation fixture
	next.RotationID = "rotation-13"           // pragma: allowlist secret -- synthetic fixture
	mini.RequireUserAuth(next.Username, next.Password)
	reader.credential = next
	require.NoError(t, runtime.Refresh(t.Context()))
	require.Eventually(t, func() bool {
		if shared.Ping(t.Context()).Err() != nil || dedicated.Ping(t.Context()).Err() != nil {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		return authUsers[next.Username] >= 2
	}, 2*time.Second, time.Millisecond, "both live pooled clients must consume streaming rotation")
	require.NoError(t, dedicated.Close())
	require.Zero(t, dedicated.PoolStats().TotalConns)
	require.ErrorIs(t, dedicated.Ping(t.Context()).Err(), redis.ErrClosed)
	// Streaming reauthentication may retain multiple shared-pool connections.
	// Every remaining server connection must belong to that surviving client.
	require.Eventually(t, func() bool {
		sharedConnections := shared.PoolStats().TotalConns
		return sharedConnections > 0 && mini.CurrentConnectionCount() == int(sharedConnections)
	}, time.Second, time.Millisecond)
	require.NoError(t, shared.Set(t.Context(), "shared-client-survives-budget-close", "value", 0).Err())
	require.NoError(t, runtime.Fresh(now))
	third := next
	third.Generation = 14
	third.Username = "cv_cp_14"
	third.Password = "synthetic-third-password" // pragma: allowlist secret -- local miniredis rotation fixture
	third.RotationID = "rotation-14"            // pragma: allowlist secret -- synthetic fixture
	mini.RequireUserAuth(third.Username, third.Password)
	reader.credential = third
	require.NoError(t, runtime.Refresh(t.Context()))
	require.Eventually(t, func() bool {
		if shared.Ping(t.Context()).Err() != nil {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		return authUsers[third.Username] >= 1
	}, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool {
		sharedConnections := shared.PoolStats().TotalConns
		return sharedConnections > 0 && mini.CurrentConnectionCount() == int(sharedConnections)
	}, time.Second, time.Millisecond)
	require.Zero(t, dedicated.PoolStats().TotalConns, "closed dedicated pool must not reopen during later rotation")
}

func TestLegacyEventBudgetClientCloseKeepsIndependentSharedClient(t *testing.T) {
	mini := miniredis.RunT(t)
	endpoint := "redis://" + mini.Addr() + "?protocol=2"
	shared, err := NewRedisClient(endpoint)
	require.NoError(t, err)
	eventBudgetClientCleanup(t, shared)
	prior := *shared.Options()
	dedicated, err := NewEventBudgetRedisClient(endpoint)
	require.NoError(t, err)
	eventBudgetClientCleanup(t, dedicated)
	require.NotSame(t, shared, dedicated)
	require.NotSame(t, shared.Options(), dedicated.Options())
	assert.Equal(t, prior.MaxRetries, shared.Options().MaxRetries)
	assert.Equal(t, prior.PoolSize, shared.Options().PoolSize)
	assert.Equal(t, prior.ReadTimeout, shared.Options().ReadTimeout)
	assert.Equal(t, prior.MaintNotificationsConfig.Mode, shared.Options().MaintNotificationsConfig.Mode)
	require.NoError(t, dedicated.Ping(t.Context()).Err())
	require.NoError(t, dedicated.Close())
	require.NoError(t, shared.Set(t.Context(), "legacy-shared-survives-budget-close", "value", 0).Err())
	value, err := mini.Get("legacy-shared-survives-budget-close")
	require.NoError(t, err)
	assert.Equal(t, "value", value)
}
