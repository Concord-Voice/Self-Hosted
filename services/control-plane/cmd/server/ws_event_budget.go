package main

import (
	"crypto/tls"
	"errors"
	"fmt"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/database"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedredis"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/redis/go-redis/v9"
)

type wsEventAdmitterFactories struct {
	newLegacy   func(string) (*redis.Client, error)
	newManaged  func(string, *tls.Config, *managedredis.Runtime) (*redis.Client, error)
	newBackend  func(websocket.EventBudgetConfig, *redis.Client) (websocket.AccountBudgetBackend, error)
	newAdmitter func(websocket.EventBudgetConfig, websocket.AccountBudgetBackend) (websocket.EventAdmitter, error)
	closeClient func(*redis.Client) error
}

func initializeWSEventAdmitter(cfg *config.Config, redisRuntime *managedredis.Runtime, redisTLSConfig *tls.Config) (websocket.EventAdmitter, error) {
	return initializeWSEventAdmitterWithFactories(cfg, redisRuntime, redisTLSConfig, wsEventAdmitterFactories{
		newLegacy:   database.NewEventBudgetRedisClient,
		newManaged:  database.NewManagedEventBudgetRedisClient,
		newBackend:  websocket.NewRedisAccountBudgetBackend,
		newAdmitter: websocket.NewEventAdmitter,
		closeClient: (*redis.Client).Close,
	})
}

// Each successful constructor transfers ownership to the next layer. Managed
// credentials remain borrowed from the process's existing runtime throughout.
func initializeWSEventAdmitterWithFactories(cfg *config.Config, redisRuntime *managedredis.Runtime, redisTLSConfig *tls.Config, factories wsEventAdmitterFactories) (websocket.EventAdmitter, error) {
	if cfg == nil {
		return nil, errors.New("websocket event budget configuration unavailable")
	}
	policy := websocket.EventBudgetConfig{Enabled: cfg.WSEventBudget.Enabled, ScalePercent: cfg.WSEventBudget.ScalePercent}
	if err := websocket.ValidateEventBudgetConfig(policy); err != nil {
		return nil, fmt.Errorf("validate websocket event budget configuration: %w", err)
	}
	if !policy.Enabled {
		return factories.newAdmitter(policy, nil)
	}
	client, err := factories.newClient(cfg, redisRuntime, redisTLSConfig)
	if err != nil {
		return nil, fmt.Errorf("construct websocket event budget client: %w", err)
	}
	backend, err := factories.newBackend(policy, client)
	if err != nil {
		closeErr := factories.closeClient(client)
		return nil, errors.Join(fmt.Errorf("construct websocket event budget backend: %w", err), closeErr)
	}
	admitter, err := factories.newAdmitter(policy, backend)
	if err != nil {
		closeErr := backend.Close()
		return nil, errors.Join(fmt.Errorf("construct websocket event admitter: %w", err), closeErr)
	}
	return admitter, nil
}

func (factories wsEventAdmitterFactories) newClient(cfg *config.Config, redisRuntime *managedredis.Runtime, redisTLSConfig *tls.Config) (*redis.Client, error) {
	switch cfg.RedisCredentialMode {
	case "", "legacy":
		return factories.newLegacy(cfg.RedisURL)
	case "managed":
		return factories.newManaged(cfg.RedisEndpoint, redisTLSConfig, redisRuntime)
	default:
		return nil, errors.New("invalid Redis credential mode for websocket event budgets")
	}
}

func closeWSEventAdmitterOnReturn(admitter websocket.EventAdmitter, runErr *error) {
	if err := admitter.Close(); err != nil {
		*runErr = errors.Join(*runErr, fmt.Errorf("close websocket event admitter: %w", err))
	}
}
