package websocket

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// AdmissionAction is a closed account-admission outcome. Zero is invalid.
type AdmissionAction uint8

// AdmissionAllow and the other actions distinguish refusal, overload and abandonment.
const (
	AdmissionAllow AdmissionAction = iota + 1
	AdmissionRefuse
	AdmissionCloseOverload
	AdmissionAbandon
)

// AdmissionSource identifies the evidence supporting an outcome. Zero is invalid.
type AdmissionSource uint8

// AdmissionSourceExisting and the other sources distinguish admission owners.
const (
	AdmissionSourceExisting AdmissionSource = iota + 1
	AdmissionSourceLocalMaintenance
	AdmissionSourceRedis
	AdmissionSourceWarmLocal
	AdmissionSourceUnavailable
)

// AdmissionDecision separates a definitive rate denial from uncertain execution.
// Abandon carries no overload response or final admission-counter implication.
type AdmissionDecision struct {
	Action             AdmissionAction
	Source             AdmissionSource
	RetryAfter         time.Duration
	DefinitiveRateDeny bool
}

// EventAdmitter owns account admission only; it neither dispatches nor retries.
// The final argument is an absolute hold deadline, zero for non-hold classes.
type EventAdmitter interface {
	Admit(context.Context, uuid.UUID, EventPolicy, time.Time) AdmissionDecision
	Close() error
}

const (
	accountClassCount       = 7
	accountRemoteSlots      = 32
	accountWarmCapacity     = 10000
	accountDecisionBudget   = 100 * time.Millisecond
	accountRemoteBudget     = 90 * time.Millisecond
	accountSweepInterval    = 60 * time.Second
	accountWarmIdleLifetime = 600 * time.Second
)

type warmAccountBudget struct {
	tat         [accountClassCount]time.Duration
	lastTouched time.Duration
}

type accountEventAdmitter struct {
	config   EventBudgetConfig
	backend  AccountBudgetBackend
	now      func() time.Time
	origin   time.Time
	remote   [accountClassCount]eventBudgetParameters
	fallback [accountClassCount]eventBudgetParameters

	warmMu sync.Mutex
	warm   map[uuid.UUID]warmAccountBudget

	// Registration and stopping share a lock, so Add cannot race a zero Wait.
	// Only remote calls need cancellation slots; their fixed array is bounded
	// by the same semaphore as backend work, with no per-request goroutine.
	lifecycleMu sync.Mutex
	stopped     atomic.Bool
	calls       sync.WaitGroup
	slots       chan int
	cancels     [accountRemoteSlots]context.CancelFunc
	stopSweep   chan struct{}
	sweepDone   chan struct{}
	closeOnce   sync.Once
	closeErr    error

	activeRemote   atomic.Int64
	capacityMisses atomic.Uint64
}

// NewEventAdmitter takes ownership of backend only on success. Enabled config
// requires a usable backend; disabled config requires nil and starts no sweep.
func NewEventAdmitter(cfg EventBudgetConfig, backend AccountBudgetBackend) (EventAdmitter, error) {
	admitter, err := newEventAdmitterWithClock(cfg, backend, time.Now)
	if err != nil {
		return nil, err
	}
	return admitter, nil
}

func newEventAdmitterWithClock(cfg EventBudgetConfig, backend AccountBudgetBackend, now func() time.Time) (*accountEventAdmitter, error) {
	if err := ValidateEventBudgetConfig(cfg); err != nil {
		return nil, err
	}
	if now == nil || (cfg.Enabled && backend == nil) || (!cfg.Enabled && backend != nil) {
		return nil, ErrAccountBudgetUnavailable
	}
	a := &accountEventAdmitter{config: cfg, backend: backend, now: now, origin: now()}
	if !cfg.Enabled {
		return a, nil
	}
	for index := range a.remote {
		parameters, err := accountEventBudget(cfg, EventClass(index)+EventClassTyping)
		if err != nil {
			return nil, err
		}
		a.remote[index] = parameters
		a.fallback[index] = eventBudgetParameters{
			Interval: 4 * parameters.Interval,
			Burst:    max(1, parameters.Burst/4),
		}
	}
	a.warm = make(map[uuid.UUID]warmAccountBudget)
	a.slots = make(chan int, accountRemoteSlots)
	for slot := range accountRemoteSlots {
		a.slots <- slot
	}
	a.stopSweep = make(chan struct{})
	a.sweepDone = make(chan struct{})
	go a.runSweep()
	return a, nil
}

func (a *accountEventAdmitter) Admit(ctx context.Context, userID uuid.UUID, policy EventPolicy, holdDeadline time.Time) AdmissionDecision {
	start := a.now()
	a.lifecycleMu.Lock()
	if a.stopped.Load() {
		a.lifecycleMu.Unlock()
		return abandonedAccountAdmission()
	}
	a.calls.Add(1)
	a.lifecycleMu.Unlock()
	defer a.calls.Done()

	if a.abandoned(ctx, start) {
		return abandonedAccountAdmission()
	}
	index, valid := accountPolicyIndex(policy)
	if userID == uuid.Nil || !valid || (!policy.MayHold && !holdDeadline.IsZero()) {
		return a.finish(ctx, time.Time{}, EventPolicy{}, unavailableAccountAdmission(EventPolicy{}))
	}
	deadline := accountAdmissionDeadline(ctx, start, holdDeadline)
	if !start.Before(deadline) {
		return a.finish(ctx, deadline, policy, unavailableAccountAdmission(policy))
	}
	if !a.config.Enabled {
		return a.finish(ctx, deadline, policy, AdmissionDecision{Action: AdmissionAllow, Source: AdmissionSourceExisting})
	}

	// Reservation precedes even slot acquisition and is never refunded, including
	// after a definitive denial, cancellation or uncertain remote execution.
	reserved := a.reserveLocal(ctx, userID, index, deadline)
	remoteDeadline := start.Add(accountRemoteBudget)
	if deadline.Before(remoteDeadline) {
		remoteDeadline = deadline
	}
	remote, proven := a.decideRemote(ctx, userID, policy.Class, index, remoteDeadline)
	decision := unavailableAccountAdmission(policy)
	if proven {
		a.warmAfterReply(ctx, userID, deadline)
		decision = AdmissionDecision{
			Action: AdmissionRefuse, Source: AdmissionSourceRedis,
			RetryAfter: remote.RetryAfter, DefinitiveRateDeny: !remote.Allowed,
		}
		if remote.Allowed {
			decision.Action = AdmissionAllow
		}
	} else if reserved {
		decision = AdmissionDecision{Action: AdmissionAllow, Source: AdmissionSourceWarmLocal}
	}
	// Warming may have waited on the map lock: evidence observed before 90ms
	// cannot carry an allowance past authority loss or the final 100ms deadline.
	return a.finish(ctx, deadline, policy, decision)
}

func accountAdmissionDeadline(ctx context.Context, start, holdDeadline time.Time) time.Time {
	deadline := start.Add(accountDecisionBudget)
	if parentDeadline, ok := ctx.Deadline(); ok && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	if !holdDeadline.IsZero() && holdDeadline.Before(deadline) {
		deadline = holdDeadline
	}
	return deadline
}

func accountPolicyIndex(policy EventPolicy) (int, bool) {
	if policy.Scope != BudgetScopeAccount || policy.Class < EventClassTyping || policy.Class > EventClassRenewal {
		return 0, false
	}
	mayHold := policy.Class == EventClassSubscription || policy.Class == EventClassReady
	return int(policy.Class - EventClassTyping), policy.MayHold == mayHold
}

func (a *accountEventAdmitter) abandoned(ctx context.Context, now time.Time) bool {
	if a.stopped.Load() || ctx == nil || ctx.Err() != nil {
		return true
	}
	deadline, ok := ctx.Deadline()
	return ok && !now.Before(deadline)
}

func abandonedAccountAdmission() AdmissionDecision {
	return AdmissionDecision{Action: AdmissionAbandon, Source: AdmissionSourceUnavailable}
}

func unavailableAccountAdmission(policy EventPolicy) AdmissionDecision {
	action := AdmissionRefuse
	if policy.MayHold {
		action = AdmissionCloseOverload
	}
	return AdmissionDecision{Action: action, Source: AdmissionSourceUnavailable, RetryAfter: time.Second}
}

func (a *accountEventAdmitter) finish(ctx context.Context, deadline time.Time, policy EventPolicy, decision AdmissionDecision) AdmissionDecision {
	// Order the final allowance check against Close's stopping transition.
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	now := a.now()
	if a.abandoned(ctx, now) {
		return abandonedAccountAdmission()
	}
	if !deadline.IsZero() && !now.Before(deadline) {
		return unavailableAccountAdmission(policy)
	}
	return decision
}

func (a *accountEventAdmitter) reserveLocal(ctx context.Context, userID uuid.UUID, index int, deadline time.Time) bool {
	a.warmMu.Lock()
	defer a.warmMu.Unlock()
	now := a.now()
	if a.abandoned(ctx, now) || !now.Before(deadline) {
		return false
	}
	state, exists := a.warm[userID]
	elapsed := now.Sub(a.origin)
	if !exists || elapsed < 0 || elapsed < state.lastTouched {
		return false
	}
	state.lastTouched = elapsed
	parameters := a.fallback[index]
	allowAt := state.tat[index] - time.Duration(parameters.Burst-1)*parameters.Interval
	base := max(elapsed, state.tat[index])
	allowed := elapsed >= allowAt && base <= time.Duration(math.MaxInt64)-parameters.Interval
	if allowed {
		state.tat[index] = base + parameters.Interval
	}
	a.warm[userID] = state
	return allowed
}

func (a *accountEventAdmitter) decideRemote(ctx context.Context, userID uuid.UUID, class EventClass, index int, deadline time.Time) (RedisBudgetDecision, bool) {
	now := a.now()
	if a.abandoned(ctx, now) || !now.Before(deadline) {
		return RedisBudgetDecision{}, false
	}
	var slot int
	select {
	case slot = <-a.slots:
	default:
		return RedisBudgetDecision{}, false
	}
	a.activeRemote.Add(1)
	remoteCtx, cancel := context.WithDeadline(ctx, deadline)
	defer a.releaseRemote(slot, cancel)
	a.lifecycleMu.Lock()
	if a.stopped.Load() {
		a.lifecycleMu.Unlock()
		return RedisBudgetDecision{}, false
	}
	a.cancels[slot] = cancel
	a.lifecycleMu.Unlock()
	now = a.now()
	if a.abandoned(remoteCtx, now) || !now.Before(deadline) {
		return RedisBudgetDecision{}, false
	}
	parameters := a.remote[index]
	decision, err := a.backend.Decide(remoteCtx, userID, class, parameters.Interval.Microseconds(), int64(parameters.Burst))
	observed := a.now()
	proven := err == nil && !a.abandoned(remoteCtx, observed) && observed.Before(deadline) && validAccountRedisDecision(decision)
	return decision, proven
}

func (a *accountEventAdmitter) releaseRemote(slot int, cancel context.CancelFunc) {
	cancel()
	a.lifecycleMu.Lock()
	a.cancels[slot] = nil
	a.lifecycleMu.Unlock()
	a.activeRemote.Add(-1)
	a.slots <- slot
}

func validAccountRedisDecision(decision RedisBudgetDecision) bool {
	if decision.Allowed {
		return decision.RetryAfter == 0
	}
	return decision.RetryAfter >= time.Millisecond && decision.RetryAfter <= 120*time.Second && decision.RetryAfter%time.Millisecond == 0
}

func (a *accountEventAdmitter) warmAfterReply(ctx context.Context, userID uuid.UUID, deadline time.Time) {
	a.warmMu.Lock()
	defer a.warmMu.Unlock()
	now := a.now()
	if a.abandoned(ctx, now) || !now.Before(deadline) {
		return
	}
	if _, exists := a.warm[userID]; exists {
		return
	}
	if len(a.warm) >= accountWarmCapacity {
		a.capacityMisses.Add(1)
		return
	}
	elapsed := now.Sub(a.origin)
	if elapsed < 0 {
		return
	}
	state := warmAccountBudget{lastTouched: elapsed}
	for index, parameters := range a.fallback {
		// Configured parameters bound the product to seconds. Check the only
		// lifetime-dependent addition before evaluating it.
		debt := time.Duration(parameters.Burst) * parameters.Interval
		if elapsed > time.Duration(math.MaxInt64)-debt {
			return
		}
		state.tat[index] = elapsed + debt
	}
	a.warm[userID] = state
}

func (a *accountEventAdmitter) sweepIdle(now time.Time) {
	a.warmMu.Lock()
	defer a.warmMu.Unlock()
	if a.stopped.Load() {
		return
	}
	elapsed := now.Sub(a.origin)
	for userID, state := range a.warm {
		if elapsed >= state.lastTouched && elapsed-state.lastTouched >= accountWarmIdleLifetime {
			delete(a.warm, userID)
		}
	}
}

func (a *accountEventAdmitter) runSweep() {
	defer close(a.sweepDone)
	ticker := time.NewTicker(accountSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-a.stopSweep:
			return
		case <-ticker.C:
			a.sweepIdle(a.now())
		}
	}
}

// Close rejects new work, cancels active backend calls, and joins admitted calls
// and the sweep before closing the backend once. No join holds either mutex.
func (a *accountEventAdmitter) Close() error {
	a.closeOnce.Do(func() {
		a.lifecycleMu.Lock()
		a.stopped.Store(true)
		cancels := a.cancels
		if a.stopSweep != nil {
			close(a.stopSweep)
		}
		a.lifecycleMu.Unlock()
		for _, cancel := range cancels {
			if cancel != nil {
				cancel()
			}
		}
		if a.sweepDone != nil {
			<-a.sweepDone
		}
		a.calls.Wait()
		if a.backend != nil {
			a.closeErr = a.backend.Close()
		}
	})
	return a.closeErr
}
