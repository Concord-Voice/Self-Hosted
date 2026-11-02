package websocket

import (
	"context"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/ingressbudget"
)

const (
	eventAdmissionHoldLimit = 2 * time.Second
	eventAdmissionSpacing   = 100 * time.Millisecond
	eventAdmissionAttempts  = 21
)

type eventAdmissionTimer interface {
	C() <-chan time.Time
	Stop() bool
}

type realEventAdmissionTimer struct{ timer *time.Timer }

func (timer realEventAdmissionTimer) C() <-chan time.Time { return timer.timer.C }
func (timer realEventAdmissionTimer) Stop() bool          { return timer.timer.Stop() }

func (c *Client) eventAdmissionTime() time.Time {
	if c.eventAdmissionNow != nil {
		return c.eventAdmissionNow()
	}
	return time.Now()
}

func (c *Client) newEventAdmissionTimer(wait time.Duration) eventAdmissionTimer {
	if c.eventAdmissionNewTimer != nil {
		return c.eventAdmissionNewTimer(wait)
	}
	return realEventAdmissionTimer{timer: time.NewTimer(wait)}
}

// admitInboundEvent owns one synchronous event decision, including any hold.
// It does not dispatch, send, renew authority or charge the existing content gate.
func (c *Client) admitInboundEvent(ctx context.Context, policy EventPolicy) AdmissionDecision {
	if !c.ingressAuthorityLive(ctx) {
		return abandonedAccountAdmission()
	}
	allowExisting := AdmissionDecision{Action: AdmissionAllow, Source: AdmissionSourceExisting}
	if !c.Hub.eventBudgetConfig.Enabled {
		return allowExisting
	}
	if !validInboundEventPolicy(policy) {
		return abandonedAccountAdmission()
	}
	if policy.Scope == BudgetScopeExistingContent {
		return allowExisting
	}
	state := eventAdmissionState{}
	var decision AdmissionDecision
	if policy.Scope == BudgetScopeLocalMaintenance {
		decision = c.admitHeartbeatEvent()
	} else {
		decision = c.admitAccountEvent(ctx, policy, &state)
	}
	return state.finalize(ctx, c, policy.Class, decision)
}

func validInboundEventPolicy(policy EventPolicy) bool {
	switch policy.Scope {
	case BudgetScopeExistingContent:
		return policy.Class == EventClassContent && !policy.MayHold
	case BudgetScopeLocalMaintenance:
		return policy.Class == EventClassPresence && !policy.MayHold
	case BudgetScopeAccount:
		_, valid := accountPolicyIndex(policy)
		return valid
	default:
		return false
	}
}

func (c *Client) admitHeartbeatEvent() AdmissionDecision {
	// Bucket.Allow deliberately allows nil for other consumers. Initialize once
	// on this read owner before ever calling it for enabled heartbeat admission.
	if c.eventHeartbeatBudget == nil {
		if c.eventAdmissionNow == nil {
			c.eventHeartbeatBudget = ingressbudget.NewBucket(5*time.Second, 2)
		} else {
			c.eventHeartbeatBudget = ingressbudget.NewBucketWithClock(5*time.Second, 2, c.eventAdmissionNow)
		}
	}
	decision := AdmissionDecision{Action: AdmissionRefuse, Source: AdmissionSourceLocalMaintenance}
	if c.eventHeartbeatBudget.Allow() {
		decision.Action = AdmissionAllow
	}
	return decision
}

func (c *Client) admitAccountEvent(ctx context.Context, policy EventPolicy, state *eventAdmissionState) AdmissionDecision {
	if c.Hub.eventAdmitter == nil {
		return abandonedAccountAdmission()
	}
	hold := eventAdmissionHold{}
	for attempt := 0; attempt < eventAdmissionAttempts; attempt++ {
		decision, started, completed := c.attemptAccountEvent(ctx, policy, &hold)
		decision, retry := hold.settle(policy, state, decision, completed)
		if !retry {
			return decision
		}
		next := nextEventAdmissionAttempt(started, completed, decision.RetryAfter)
		if attempt+1 == eventAdmissionAttempts || !next.Before(hold.deadline) || !c.waitEventAdmission(ctx, next, hold.deadline) {
			return exhaustedEventAdmission(hold.lastDenial)
		}
	}
	return exhaustedEventAdmission(hold.lastDenial)
}

// eventAdmissionHold belongs to this call's stack. Its first deadline never
// changes, and its last denial supplies only exhaustion's validated retry.
type eventAdmissionHold struct {
	started    bool
	deadline   time.Time
	lastDenial AdmissionDecision
}

func (c *Client) attemptAccountEvent(ctx context.Context, policy EventPolicy, hold *eventAdmissionHold) (AdmissionDecision, time.Time, time.Time) {
	if !c.ingressAuthorityLive(ctx) {
		return abandonedAccountAdmission(), time.Time{}, time.Time{}
	}
	started := c.eventAdmissionTime()
	if policy.MayHold && !hold.started {
		hold.started = true
		hold.deadline = started.Add(eventAdmissionHoldLimit)
	}
	if policy.MayHold && !started.Before(hold.deadline) {
		return exhaustedEventAdmission(hold.lastDenial), started, started
	}
	decision := c.Hub.eventAdmitter.Admit(ctx, c.UserID, policy, hold.deadline)
	completed := c.eventAdmissionTime()
	if !c.ingressAuthorityLive(ctx) || !validAccountAdmissionDecision(policy, decision) {
		return abandonedAccountAdmission(), started, completed
	}
	return decision, started, completed
}

func (hold *eventAdmissionHold) settle(policy EventPolicy, state *eventAdmissionState, decision AdmissionDecision, completed time.Time) (AdmissionDecision, bool) {
	state.observe(decision)
	// Current unavailability outranks older denial evidence, even at the bound.
	if decision.Action == AdmissionAbandon || decision.Action == AdmissionCloseOverload {
		return decision, false
	}
	if decision.Action == AdmissionRefuse && decision.Source == AdmissionSourceRedis {
		hold.lastDenial = decision
	}
	if policy.MayHold && !completed.Before(hold.deadline) {
		return exhaustedEventAdmission(hold.lastDenial), false
	}
	return decision, policy.MayHold && decision.Action == AdmissionRefuse
}

func nextEventAdmissionAttempt(started, completed time.Time, retry time.Duration) time.Time {
	next := completed.Add(retry)
	if earliest := started.Add(eventAdmissionSpacing); earliest.After(next) {
		return earliest
	}
	return next
}

// validAccountAdmissionDecision admits only tuples emitted by B. Invalid engine
// results cannot invent an allowance, a retry or an aggregate final outcome.
func validAccountAdmissionDecision(policy EventPolicy, decision AdmissionDecision) bool {
	switch decision.Source {
	case AdmissionSourceRedis:
		if decision.Action == AdmissionAllow {
			return decision.RetryAfter == 0 && !decision.DefinitiveRateDeny
		}
		return decision.Action == AdmissionRefuse && decision.DefinitiveRateDeny &&
			decision.RetryAfter >= time.Millisecond && decision.RetryAfter <= 120*time.Second &&
			decision.RetryAfter%time.Millisecond == 0
	case AdmissionSourceWarmLocal:
		return decision.Action == AdmissionAllow && decision.RetryAfter == 0 && !decision.DefinitiveRateDeny
	case AdmissionSourceUnavailable:
		if decision.Action == AdmissionAbandon {
			return decision.RetryAfter == 0 && !decision.DefinitiveRateDeny
		}
		return decision == unavailableAccountAdmission(policy)
	default:
		return false
	}
}

func exhaustedEventAdmission(lastDenial AdmissionDecision) AdmissionDecision {
	if lastDenial.Action == AdmissionRefuse {
		lastDenial.Action = AdmissionCloseOverload
		return lastDenial
	}
	return unavailableAccountAdmission(EventPolicy{MayHold: true})
}

func (c *Client) waitEventAdmission(ctx context.Context, next, holdDeadline time.Time) bool {
	for {
		if !c.ingressAuthorityLive(ctx) {
			return false
		}
		now := c.eventAdmissionTime()
		if !now.Before(holdDeadline) {
			return false
		}
		if !now.Before(next) {
			return true
		}
		timer := c.newEventAdmissionTimer(next.Sub(now))
		select {
		case <-timer.C():
		case <-ctx.Done():
		case <-c.ingressDone():
		case <-c.Hub.done:
		}
		timer.Stop()
		// Every wake rechecks authority and actual logical time. An early timer
		// cannot waive spacing, and a late timer cannot restart the hold window.
	}
}
