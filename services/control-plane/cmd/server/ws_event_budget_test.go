package main

import (
	"context"
	"crypto/tls"
	"errors"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedredis"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type c5FactoryBackend struct {
	closes   int
	closeErr error
	client   *redis.Client
}

func (*c5FactoryBackend) Decide(context.Context, uuid.UUID, websocket.EventClass, int64, int64) (websocket.RedisBudgetDecision, error) {
	panic("construction must not decide")
}
func (b *c5FactoryBackend) Close() error {
	b.closes++
	if b.client != nil {
		return errors.Join(b.closeErr, b.client.Close())
	}
	return b.closeErr
}

type c5FactoryAdmitter struct {
	closes   int
	closeErr error
	onClose  func()
}

func (*c5FactoryAdmitter) Admit(context.Context, uuid.UUID, websocket.EventPolicy, time.Time) websocket.AdmissionDecision {
	panic("construction must not admit")
}
func (a *c5FactoryAdmitter) Close() error {
	a.closes++
	if a.onClose != nil {
		a.onClose()
	}
	return a.closeErr
}

type c5FactoryFixture struct {
	factories                                    wsEventAdmitterFactories
	calls                                        []string
	client                                       *redis.Client
	backend                                      *c5FactoryBackend
	engine                                       *c5FactoryAdmitter
	clientErr, backendErr, engineErr, cleanupErr error
	policy                                       websocket.EventBudgetConfig
	url, endpoint                                string
	runtime                                      *managedredis.Runtime
	tls                                          *tls.Config
}

func c5FactoryNewFixture(t *testing.T) *c5FactoryFixture {
	t.Helper()
	// An idle identity token only. No command, PING or dial is performed.
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() {
		err := client.Close()
		if err != nil && !errors.Is(err, redis.ErrClosed) {
			t.Errorf("idle token cleanup: %v", err)
		}
	})
	f := &c5FactoryFixture{client: client, backend: &c5FactoryBackend{client: client}, engine: &c5FactoryAdmitter{}}
	f.factories = wsEventAdmitterFactories{
		newLegacy: func(url string) (*redis.Client, error) {
			f.calls = append(f.calls, "legacy")
			f.url = url
			if f.clientErr != nil {
				return nil, f.clientErr
			}
			return f.client, nil
		},
		newManaged: func(endpoint string, tlsConfig *tls.Config, runtime *managedredis.Runtime) (*redis.Client, error) {
			f.calls = append(f.calls, "managed")
			f.endpoint = endpoint
			f.tls = tlsConfig
			f.runtime = runtime
			if f.clientErr != nil {
				return nil, f.clientErr
			}
			return f.client, nil
		},
		newBackend: func(policy websocket.EventBudgetConfig, client *redis.Client) (websocket.AccountBudgetBackend, error) {
			f.calls = append(f.calls, "backend")
			assert.Same(t, f.client, client)
			f.policy = policy
			if f.backendErr != nil {
				return nil, f.backendErr
			}
			return f.backend, nil
		},
		newAdmitter: func(policy websocket.EventBudgetConfig, backend websocket.AccountBudgetBackend) (websocket.EventAdmitter, error) {
			f.calls = append(f.calls, "engine")
			assert.Equal(t, f.policy, policy)
			if policy.Enabled {
				assert.Same(t, f.backend, backend)
			} else {
				assert.Nil(t, backend)
			}
			if f.engineErr != nil {
				return nil, f.engineErr
			}
			return f.engine, nil
		},
		closeClient: func(client *redis.Client) error {
			f.calls = append(f.calls, "close-client")
			assert.Same(t, f.client, client)
			return errors.Join(f.cleanupErr, client.Close())
		},
	}
	return f
}

func c5FactoryConfig(enabled bool, scale int, mode string) *config.Config {
	return &config.Config{WSEventBudget: config.WSEventBudgetConfig{Enabled: enabled, ScalePercent: scale}, RedisCredentialMode: mode, RedisURL: "redis://synthetic-legacy.invalid:6379/0", RedisEndpoint: "synthetic-managed.invalid:6379"}
}

func TestWSEventBudgetFactorySelectionAndOwnership(t *testing.T) {
	for _, mode := range []string{"", "legacy", "managed"} {
		t.Run("mode-"+mode, func(t *testing.T) {
			f := c5FactoryNewFixture(t)
			cfg := c5FactoryConfig(true, 237, mode)
			runtime := &managedredis.Runtime{}
			tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
			got, err := initializeWSEventAdmitterWithFactories(cfg, runtime, tlsConfig, f.factories)
			require.NoError(t, err)
			assert.Same(t, f.engine, got)
			assert.Equal(t, websocket.EventBudgetConfig{Enabled: true, ScalePercent: 237}, f.policy)
			if mode == "managed" {
				assert.Equal(t, []string{"managed", "backend", "engine"}, f.calls)
				assert.Equal(t, cfg.RedisEndpoint, f.endpoint)
				assert.Same(t, runtime, f.runtime)
				assert.Same(t, tlsConfig, f.tls)
				assert.Empty(t, f.url)
			} else {
				assert.Equal(t, []string{"legacy", "backend", "engine"}, f.calls)
				assert.Equal(t, cfg.RedisURL, f.url)
				assert.Empty(t, f.endpoint)
			}
			assert.Zero(t, f.backend.closes)
			assert.Zero(t, f.engine.closes)
			assert.Zero(t, f.client.PoolStats().TotalConns)
		})
	}
}

func TestWSEventBudgetFactoryDisabledAndValidation(t *testing.T) {
	t.Run("disabled bypasses managed dependencies", func(t *testing.T) {
		f := c5FactoryNewFixture(t)
		f.policy = websocket.EventBudgetConfig{Enabled: false, ScalePercent: 400}
		got, err := initializeWSEventAdmitterWithFactories(c5FactoryConfig(false, 400, "managed"), nil, nil, f.factories)
		require.NoError(t, err)
		assert.Same(t, f.engine, got)
		assert.Equal(t, []string{"engine"}, f.calls)
		assert.Zero(t, f.backend.closes)
	})
	for _, tc := range []struct {
		name string
		cfg  *config.Config
	}{
		{"nil", nil}, {"enabled zero", c5FactoryConfig(true, 0, "legacy")}, {"disabled zero", c5FactoryConfig(false, 0, "legacy")},
		{"below", c5FactoryConfig(true, 24, "legacy")}, {"above disabled", c5FactoryConfig(false, 401, "managed")}, {"unknown credential mode", c5FactoryConfig(true, 100, "synthetic-private-mode")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := c5FactoryNewFixture(t)
			got, err := initializeWSEventAdmitterWithFactories(tc.cfg, nil, nil, f.factories)
			require.Error(t, err)
			assert.Nil(t, got)
			assert.Empty(t, f.calls)
			assert.NotContains(t, err.Error(), "synthetic-private-mode")
			assert.Zero(t, f.backend.closes)
		})
	}
}

func TestWSEventBudgetFactoryFailureOwnership(t *testing.T) {
	construction := errors.New("synthetic construction failure")
	cleanup := errors.New("synthetic cleanup failure")
	for _, tc := range []struct {
		name          string
		enabled       bool
		mode          string
		stage         string
		want          []string
		backendCloses int
	}{
		{"legacy client", true, "legacy", "client", []string{"legacy"}, 0},
		{"managed client", true, "managed", "client", []string{"managed"}, 0},
		{"backend", true, "legacy", "backend", []string{"legacy", "backend", "close-client"}, 0},
		{"engine", true, "legacy", "engine", []string{"legacy", "backend", "engine"}, 1},
		{"disabled engine", false, "managed", "engine", []string{"engine"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := c5FactoryNewFixture(t)
			f.policy = websocket.EventBudgetConfig{Enabled: tc.enabled, ScalePercent: 100}
			switch tc.stage {
			case "client":
				f.clientErr = construction
			case "backend":
				f.backendErr = construction
				f.cleanupErr = cleanup
			case "engine":
				f.engineErr = construction
				f.backend.closeErr = cleanup
			}
			got, err := initializeWSEventAdmitterWithFactories(c5FactoryConfig(tc.enabled, 100, tc.mode), nil, nil, f.factories)
			require.Error(t, err)
			assert.Nil(t, got)
			assert.ErrorIs(t, err, construction)
			if tc.stage == "backend" || tc.enabled && tc.stage == "engine" {
				assert.ErrorIs(t, err, cleanup)
			} else {
				assert.NotErrorIs(t, err, cleanup)
			}
			assert.Equal(t, tc.want, f.calls)
			assert.Equal(t, tc.backendCloses, f.backend.closes)
			assert.Zero(t, f.engine.closes)
			assert.NotContains(t, err.Error(), "synthetic-legacy.invalid")
			assert.NotContains(t, err.Error(), "synthetic-managed.invalid")
		})
	}
}

func TestWSEventBudgetRealDisabledAdapter(t *testing.T) {
	engine, err := initializeWSEventAdmitter(c5FactoryConfig(false, 100, "managed"), nil, nil)
	require.NoError(t, err)
	require.NotNil(t, engine)
	require.NoError(t, engine.Close())
	require.NoError(t, engine.Close())
}

func TestWSEventBudgetDeferredCloseJoinsAndPreservesOrder(t *testing.T) {
	for _, name := range []string{"router setup", "later startup stage"} {
		t.Run(name, func(t *testing.T) {
			startup := errors.New(name + " failed")
			cleanup := errors.New("retained engine close failure")
			var order []string
			engine := &c5FactoryAdmitter{closeErr: cleanup, onClose: func() { order = append(order, "engine") }}
			run := func() (runErr error) {
				defer func() { order = append(order, "shared") }()
				defer func() { order = append(order, "poll") }()
				defer closeWSEventAdmitterOnReturn(engine, &runErr)
				order = append(order, name)
				return startup
			}
			err := run()
			assert.ErrorIs(t, err, startup)
			assert.ErrorIs(t, err, cleanup)
			assert.Equal(t, []string{name, "engine", "poll", "shared"}, order)
			assert.Equal(t, 1, engine.closes)
		})
	}
}

func TestWSEventBudgetDeferredCloseChecksRetainedError(t *testing.T) {
	cleanup := errors.New("retained engine close failure")
	engine := &c5FactoryAdmitter{closeErr: cleanup}
	var runErr error
	// Model explicit shutdown followed by the startup safety defer: Close remains checked.
	require.ErrorIs(t, engine.Close(), cleanup)
	closeWSEventAdmitterOnReturn(engine, &runErr)
	assert.ErrorIs(t, runErr, cleanup)
	assert.Equal(t, 2, engine.closes)
	t.Run("successful close preserves original error", func(t *testing.T) {
		original := errors.New("original startup error")
		runErr := original
		engine := &c5FactoryAdmitter{}
		closeWSEventAdmitterOnReturn(engine, &runErr)
		assert.ErrorIs(t, runErr, original)
		assert.Equal(t, 1, engine.closes)
	})
}
