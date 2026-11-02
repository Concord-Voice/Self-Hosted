package websocket

import (
	"context"
	_ "embed" // Required by the go:embed directive for the budget script below.
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// ErrAccountBudgetUnavailable reports an unproven distributed decision.
var ErrAccountBudgetUnavailable = errors.New("websocket account budget unavailable")

// ErrAccountBudgetInvalidState reports a rejected Redis record or script input.
// It is not a rate denial and never permits replay of an uncertain operation.
var ErrAccountBudgetInvalidState = errors.New("websocket account budget invalid state")

//go:embed event_admission.lua
var eventAdmissionLua string

// RedisBudgetDecision is a validated distributed allowance or rate denial.
type RedisBudgetDecision struct {
	Allowed    bool
	RetryAfter time.Duration
}

// AccountBudgetBackend owns its dedicated Redis client after construction.
// Decide accepts an interval in microseconds and a burst, checked against config.
type AccountBudgetBackend interface {
	Decide(context.Context, uuid.UUID, EventClass, int64, int64) (RedisBudgetDecision, error)
	Close() error
}

type redisAccountBudgetBackend struct {
	config    EventBudgetConfig
	client    *redis.Client
	script    *redis.Script
	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

// NewRedisAccountBudgetBackend takes ownership of client only on success.
// The caller supplies a dedicated client with retries disabled and bounded
// connection settings. Construction performs no Redis command or dial.
func NewRedisAccountBudgetBackend(cfg EventBudgetConfig, client *redis.Client) (AccountBudgetBackend, error) {
	if err := ValidateEventBudgetConfig(cfg); err != nil {
		return nil, err
	}
	if !cfg.Enabled || client == nil {
		return nil, ErrAccountBudgetUnavailable
	}
	return &redisAccountBudgetBackend{
		config: cfg,
		client: client,
		script: redis.NewScript(eventAdmissionLua),
	}, nil
}

func (b *redisAccountBudgetBackend) Decide(ctx context.Context, userID uuid.UUID, class EventClass, intervalMicros, burst int64) (RedisBudgetDecision, error) {
	if b.closed.Load() || accountBudgetContextDone(ctx) || userID == uuid.Nil {
		return RedisBudgetDecision{}, ErrAccountBudgetUnavailable
	}
	parameters, err := accountEventBudget(b.config, class)
	if err != nil || intervalMicros != parameters.Interval.Microseconds() || burst != int64(parameters.Burst) {
		return RedisBudgetDecision{}, ErrAccountBudgetUnavailable
	}
	key := "ratelimit:user:ws:v1:{" + userID.String() + "}:" + class.String()
	// Run permits only the library's NOSCRIPT -> EVAL fallback, with the same
	// context, source, key and arguments. An uncertain debit is never retried.
	reply, err := b.script.Run(ctx, b.client, []string{key},
		"1", strconv.FormatInt(intervalMicros, 10), strconv.FormatInt(burst, 10)).Result()
	if b.closed.Load() || accountBudgetContextDone(ctx) {
		return RedisBudgetDecision{}, ErrAccountBudgetUnavailable
	}
	if err != nil {
		var redisErr redis.Error
		if errors.As(err, &redisErr) && redisErr.Error() == "ERR WS_EVENT_BUDGET_INVALID_STATE" {
			return RedisBudgetDecision{}, ErrAccountBudgetInvalidState
		}
		return RedisBudgetDecision{}, ErrAccountBudgetUnavailable
	}
	return decodeAccountBudgetReply(reply)
}

func accountBudgetContextDone(ctx context.Context) bool {
	if ctx == nil || ctx.Err() != nil {
		return true
	}
	deadline, hasDeadline := ctx.Deadline()
	return hasDeadline && !time.Now().Before(deadline)
}

func decodeAccountBudgetReply(reply interface{}) (RedisBudgetDecision, error) {
	values, ok := reply.([]interface{})
	if !ok || len(values) != 3 {
		return RedisBudgetDecision{}, ErrAccountBudgetUnavailable
	}
	version, versionOK := values[0].(int64)
	allowed, allowedOK := values[1].(int64)
	retry, retryOK := values[2].(int64)
	if !versionOK || !allowedOK || !retryOK || version != 1 {
		return RedisBudgetDecision{}, ErrAccountBudgetUnavailable
	}
	if allowed == 1 && retry == 0 {
		return RedisBudgetDecision{Allowed: true}, nil
	}
	if allowed == 0 && retry >= 1 && retry <= 120000 {
		return RedisBudgetDecision{RetryAfter: time.Duration(retry) * time.Millisecond}, nil
	}
	return RedisBudgetDecision{}, ErrAccountBudgetUnavailable
}

// Close rejects future decisions and closes only the owned client once. Active
// application-call cancellation and joining belong to the account admitter.
func (b *redisAccountBudgetBackend) Close() error {
	b.closeOnce.Do(func() {
		b.closed.Store(true)
		b.closeErr = b.client.Close()
	})
	return b.closeErr
}
