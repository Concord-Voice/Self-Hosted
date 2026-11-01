package database

import (
	"crypto/tls"
	"fmt"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedredis"
	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
)

// NewEventBudgetRedisClient constructs a dedicated account-budget client without
// probing Redis. The caller owns it until successful backend construction.
func NewEventBudgetRedisClient(redisURL string) (*redis.Client, error) {
	options, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("invalid event budget Redis URL")
	}
	configureEventBudgetRedis(options)
	return redis.NewClient(options), nil
}

// NewManagedEventBudgetRedisClient constructs a dedicated client using the
// existing runtime's streaming credentials and freshness checks. It neither
// probes Redis nor takes ownership of the runtime; freshness is checked on use.
func NewManagedEventBudgetRedisClient(endpoint string, tlsConfig *tls.Config, runtime *managedredis.Runtime) (*redis.Client, error) {
	if runtime == nil {
		return nil, fmt.Errorf("managed Redis runtime unavailable")
	}
	options, err := managedOptions(endpoint, tlsConfig)
	if err != nil {
		return nil, err
	}
	configureEventBudgetRedis(options)
	options.StreamingCredentialsProvider = runtime
	client := redis.NewClient(options)
	client.AddHook(NewFreshnessHook(runtime))
	return client, nil
}

// Only fresh options enter this helper: changing a shared client's options would
// couple its retries, timeouts and lifetime to account admission.
func configureEventBudgetRedis(options *redis.Options) {
	options.MaxRetries = -1 // NewClient normalizes -1 to zero command retries.
	options.DialerRetries = 1
	options.ContextTimeoutEnabled = true
	options.PoolSize = 32
	options.MaxActiveConns = 32
	options.MaxConcurrentDials = 32
	options.MinIdleConns = 0
	options.DialTimeout = 100 * time.Millisecond
	options.ReadTimeout = 100 * time.Millisecond
	options.WriteTimeout = 100 * time.Millisecond
	options.PoolTimeout = 100 * time.Millisecond
	options.MaintNotificationsConfig = &maintnotifications.Config{
		Mode: maintnotifications.ModeDisabled,
	}
}
