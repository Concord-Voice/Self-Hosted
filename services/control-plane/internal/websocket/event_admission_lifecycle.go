package websocket

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// NewHubWithEventAdmission copies immutable startup policy and borrows admitter.
// The caller retains ownership of the engine, including on construction failure.
func NewHubWithEventAdmission(db *sql.DB, redisClient *redis.Client, counters OpsCounter, cfg EventBudgetConfig, admitter EventAdmitter) (*Hub, error) {
	if err := ValidateEventBudgetConfig(cfg); err != nil {
		return nil, fmt.Errorf("websocket event admission: %w", err)
	}
	if cfg.Enabled && admitter == nil {
		return nil, errors.New("enabled websocket event admission requires an admitter")
	}
	hub := newHub(db, redisClient, counters)
	hub.eventBudgetConfig = cfg
	hub.eventAdmitter = admitter
	return hub, nil
}

// initIngressLifecycle runs once before expiry registration or publication. The
// socket inherits shutdown, not the HTTP request, bootstrap, or opening expiry.
func (c *Client) initIngressLifecycle() {
	parent := context.Background()
	if c.Hub != nil && c.Hub.ingressCtx != nil {
		parent = c.Hub.ingressCtx
	}
	c.ingressCtx, c.ingressCancel = context.WithCancel(parent)
	if c.Hub == nil || c.Hub.ingressCtx == nil {
		c.ingressCancel()
	}
}

// cancelIngressLifecycle must run outside client and Hub locks, after transport
// closure where required. Family sweeps call it only after their checked receipt.
func (c *Client) cancelIngressLifecycle() {
	if c.ingressCancel != nil {
		c.ingressCancel()
	}
}

// ingressEventContext snapshots renewable authority for one serialized event.
// Renewal affects the next event; it cannot extend an already-started attempt.
func (c *Client) ingressEventContext() (context.Context, context.CancelFunc) {
	parent := c.ingressCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithDeadline(parent, c.tokenDeadline())
	if !c.ingressAuthorityLive(ctx) {
		cancel()
	}
	return ctx, cancel
}

// ingressAuthorityLive supplements context notification with the actual current
// authority deadline. Call again after waits and before productive work.
func (c *Client) ingressAuthorityLive(ctx context.Context) bool {
	if ctx == nil || ctx.Err() != nil || c.ingressCtx == nil || c.ingressStopped() {
		return false
	}
	deadline := c.tokenDeadline()
	// Snapshot acquisition can wait behind renewal or teardown. Recheck every
	// cancellation/terminal fence after that wait, before using the deadline.
	if ctx.Err() != nil || c.ingressStopped() {
		return false
	}
	now := time.Now()
	if parentDeadline, bounded := ctx.Deadline(); bounded && !now.Before(parentDeadline) {
		return false
	}
	return !deadline.IsZero() && now.Before(deadline)
}

// ingressDone permits legacy fixtures without an ingress lifecycle. Real sockets
// always initialize it before publication; event authority still fails closed.
func (c *Client) ingressDone() <-chan struct{} {
	if c.ingressCtx == nil {
		return nil
	}
	return c.ingressCtx.Done()
}

func (c *Client) ingressStopped() bool {
	if c.securityTerminal.Load() || (c.ingressCtx != nil && c.ingressCtx.Err() != nil) || c.Hub == nil {
		return true
	}
	if c.Hub.ingressCtx != nil && c.Hub.ingressCtx.Err() != nil {
		return true
	}
	select {
	case <-c.Hub.done:
		return true
	default:
		return false
	}
}
