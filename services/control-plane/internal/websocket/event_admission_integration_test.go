package websocket

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Only timer delivery and elapsed scheduling time are synthetic. The real gate,
// wait select, authority checks and once-only finalization remain under test.
type c2Clock struct {
	mu       sync.Mutex
	now      time.Time
	timers   []*c2Timer
	requests chan *c2Timer
}
type c2Timer struct {
	clock     *c2Clock
	due       time.Time
	wait      time.Duration
	values    chan time.Time
	active    bool
	stopCalls atomic.Int32
}

func c2NewClock() *c2Clock        { return &c2Clock{now: time.Now(), requests: make(chan *c2Timer, 64)} }
func (c *c2Clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *c2Clock) NewTimer(wait time.Duration) eventAdmissionTimer {
	c.mu.Lock()
	timer := &c2Timer{clock: c, due: c.now.Add(wait), wait: wait, values: make(chan time.Time, 1), active: true}
	c.timers = append(c.timers, timer)
	if wait <= 0 {
		timer.active = false
		timer.values <- c.now
	}
	c.mu.Unlock()
	c.requests <- timer
	return timer
}
func (c *c2Clock) Advance(delta time.Duration) { c.AdvanceTo(c.Now().Add(delta)) }
func (c *c2Clock) AdvanceTo(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.Before(c.now) {
		panic("C2 fixture clock cannot move backwards")
	}
	c.now = now
	for _, timer := range c.timers {
		if timer.active && !now.Before(timer.due) {
			timer.active = false
			timer.values <- now
		}
	}
}
func (timer *c2Timer) C() <-chan time.Time { return timer.values }
func (timer *c2Timer) Stop() bool {
	timer.stopCalls.Add(1)
	timer.clock.mu.Lock()
	defer timer.clock.mu.Unlock()
	active := timer.active
	timer.active = false
	return active
}
func (timer *c2Timer) isActive() bool {
	timer.clock.mu.Lock()
	defer timer.clock.mu.Unlock()
	return timer.active
}
func c2AwaitTimer(t *testing.T, clock *c2Clock) *c2Timer {
	t.Helper()
	select {
	case timer := <-clock.requests:
		return timer
	case <-time.After(3 * time.Second):
		t.Fatal("gate did not create its expected hold timer")
		return nil
	}
}

type c2EngineCall struct {
	ctx                     context.Context
	user                    uuid.UUID
	policy                  EventPolicy
	hold, start, completion time.Time
}
type c2Engine struct {
	mu    sync.Mutex
	calls []c2EngineCall
	clock *c2Clock
	run   func(int, c2EngineCall) AdmissionDecision
}

func (engine *c2Engine) Admit(ctx context.Context, user uuid.UUID, policy EventPolicy, hold time.Time) AdmissionDecision {
	call := c2EngineCall{ctx: ctx, user: user, policy: policy, hold: hold, start: engine.clock.Now()}
	engine.mu.Lock()
	index := len(engine.calls)
	engine.calls = append(engine.calls, call)
	run := engine.run
	engine.mu.Unlock()
	decision := admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, false)
	if run != nil {
		decision = run(index, call)
	}
	engine.mu.Lock()
	engine.calls[index].completion = engine.clock.Now()
	engine.mu.Unlock()
	return decision
}
func (*c2Engine) Close() error { return nil }
func (engine *c2Engine) snapshot() []c2EngineCall {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return append([]c2EngineCall(nil), engine.calls...)
}
func c2Client(t *testing.T, enabled bool) (*Client, *c2Engine, *c2Clock, *finalizationObserver) {
	t.Helper()
	client := responseClient(t, enabled)
	clock := c2NewClock()
	engine := &c2Engine{clock: clock}
	observer := &finalizationObserver{}
	client.Hub.eventAdmitter = engine
	client.Hub.eventAdmissionObserver = observer
	client.eventAdmissionNow = clock.Now
	client.eventAdmissionNewTimer = clock.NewTimer
	return client, engine, clock, observer
}
func c2Authority(t *testing.T, client *Client) context.Context {
	t.Helper()
	ctx, cancel := client.ingressEventContext()
	t.Cleanup(cancel)
	return ctx
}
func c2AwaitDecision(t *testing.T, result <-chan AdmissionDecision) AdmissionDecision {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(3 * time.Second):
		t.Fatal("gate did not settle")
		return AdmissionDecision{}
	}
}
func c2Deny(delay time.Duration) AdmissionDecision {
	return admissionExpected(AdmissionRefuse, AdmissionSourceRedis, delay, true)
}

func TestEventAdmissionGateAccountTuplesAndSingleFinalization(t *testing.T) {
	for _, class := range []EventClass{EventClassTyping, EventClassSubscription, EventClassPresence, EventClassProfile, EventClassServer, EventClassReady, EventClassRenewal} {
		t.Run(class.String(), func(t *testing.T) {
			client, engine, clock, observer := c2Client(t, true)
			authority := c2Authority(t, client)
			start := clock.Now()
			policy := admissionPolicy(class)
			decision := client.admitInboundEvent(authority, policy)
			assert.Equal(t, admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, false), decision)
			calls := engine.snapshot()
			require.Len(t, calls, 1)
			assert.Equal(t, client.UserID, calls[0].user)
			assert.Equal(t, policy, calls[0].policy)
			if policy.MayHold {
				assert.Equal(t, start.Add(2*time.Second), calls[0].hold)
			} else {
				assert.True(t, calls[0].hold.IsZero())
			}
			deadline, ok := calls[0].ctx.Deadline()
			require.True(t, ok)
			assert.Equal(t, client.tokenDeadline(), deadline, "hold must not replace authority's old token expiry")
			assert.Equal(t, []finalizationRecord{{class, true, false}}, observer.snapshot())
			assert.Empty(t, clock.requests)
			assert.Empty(t, client.Hub.incoming, "machinery is not yet a dispatch caller")
			assert.Empty(t, client.Send)
		})
	}
}
func TestEventAdmissionGateDisabledAndContentBypassNewGates(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		client, engine, clock, observer := c2Client(t, false)
		for _, policy := range []EventPolicy{admissionPolicy(EventClassSubscription), {Class: EventClassPresence, Scope: BudgetScopeLocalMaintenance}} {
			assert.Equal(t, admissionExpected(AdmissionAllow, AdmissionSourceExisting, 0, false), client.admitInboundEvent(c2Authority(t, client), policy))
		}
		assert.Empty(t, engine.snapshot())
		assert.Empty(t, observer.snapshot())
		assert.Empty(t, clock.requests)
		assert.Nil(t, client.eventHeartbeatBudget)
	})
	t.Run("existing content owner", func(t *testing.T) {
		client, engine, clock, observer := c2Client(t, true)
		assert.Equal(t, admissionExpected(AdmissionAllow, AdmissionSourceExisting, 0, false), client.admitInboundEvent(c2Authority(t, client), EventPolicy{Class: EventClassContent, Scope: BudgetScopeExistingContent}))
		assert.Empty(t, engine.snapshot())
		assert.Empty(t, observer.snapshot())
		assert.Empty(t, clock.requests)
	})
}
func TestEventAdmissionGateOnlyDefinitiveDenialCanHold(t *testing.T) {
	for _, tt := range []struct {
		name              string
		class             EventClass
		decision          AdmissionDecision
		allowed, degraded bool
	}{
		{"cold unavailable", EventClassSubscription, admissionExpected(AdmissionCloseOverload, AdmissionSourceUnavailable, time.Second, false), false, true},
		{"warm allowance", EventClassReady, admissionExpected(AdmissionAllow, AdmissionSourceWarmLocal, 0, false), true, true},
		{"nonhold rate denial", EventClassProfile, c2Deny(5 * time.Second), false, false},
		{"ordinary unavailable", EventClassTyping, admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false), false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client, engine, clock, observer := c2Client(t, true)
			engine.run = func(int, c2EngineCall) AdmissionDecision { return tt.decision }
			assert.Equal(t, tt.decision, client.admitInboundEvent(c2Authority(t, client), admissionPolicy(tt.class)))
			assert.Len(t, engine.snapshot(), 1)
			assert.Empty(t, clock.requests)
			assert.Equal(t, []finalizationRecord{{tt.class, tt.allowed, tt.degraded}}, observer.snapshot())
		})
	}
}
func TestEventAdmissionGateHoldSchedulesRawDelayAndKeepsOriginalDeadline(t *testing.T) {
	for _, tt := range []struct {
		name                         string
		completion, retry, nextStart time.Duration
	}{
		{"minimum start spacing", 10 * time.Millisecond, time.Millisecond, 100 * time.Millisecond},
		{"completion plus raw retry", 90 * time.Millisecond, 20 * time.Millisecond, 110 * time.Millisecond},
		{"raw sub-external-floor delay", 10 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client, engine, clock, observer := c2Client(t, true)
			authority := c2Authority(t, client)
			start := clock.Now()
			engine.run = func(index int, _ c2EngineCall) AdmissionDecision {
				if index == 0 {
					clock.Advance(tt.completion)
					return c2Deny(tt.retry)
				}
				return admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, false)
			}
			result := make(chan AdmissionDecision, 1)
			go func() { result <- client.admitInboundEvent(authority, admissionPolicy(EventClassSubscription)) }()
			timer := c2AwaitTimer(t, clock)
			assert.Equal(t, start.Add(tt.nextStart), timer.due)
			assert.Equal(t, tt.nextStart-tt.completion, timer.wait)
			assert.Len(t, engine.snapshot(), 1)
			assert.Empty(t, observer.snapshot(), "intermediate denial must not finalize")
			clock.AdvanceTo(timer.due)
			assert.Equal(t, admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, false), c2AwaitDecision(t, result))
			calls := engine.snapshot()
			require.Len(t, calls, 2)
			assert.Equal(t, start, calls[0].start)
			assert.Equal(t, start.Add(tt.nextStart), calls[1].start)
			for _, call := range calls {
				assert.Equal(t, start.Add(2*time.Second), call.hold)
			}
			assert.Equal(t, []finalizationRecord{{EventClassSubscription, true, false}}, observer.snapshot())
		})
	}
}
func TestEventAdmissionGateHoldNeverStartsAtTwoSecondBoundary(t *testing.T) {
	for _, delay := range []time.Duration{2 * time.Second, 120 * time.Second} {
		t.Run(fmt.Sprint(delay), func(t *testing.T) {
			client, engine, clock, observer := c2Client(t, true)
			engine.run = func(int, c2EngineCall) AdmissionDecision { return c2Deny(delay) }
			assert.Equal(t, admissionExpected(AdmissionCloseOverload, AdmissionSourceRedis, delay, true), client.admitInboundEvent(c2Authority(t, client), admissionPolicy(EventClassReady)))
			assert.Len(t, engine.snapshot(), 1)
			assert.Empty(t, clock.requests)
			assert.Equal(t, []finalizationRecord{{EventClassReady, false, false}}, observer.snapshot())
		})
	}
}
func TestEventAdmissionGateIdealHoldRemainsStrictlyBelowDeadline(t *testing.T) {
	client, engine, clock, observer := c2Client(t, true)
	authority := c2Authority(t, client)
	start := clock.Now()
	engine.run = func(int, c2EngineCall) AdmissionDecision { return c2Deny(time.Millisecond) }
	result := make(chan AdmissionDecision, 1)
	go func() { result <- client.admitInboundEvent(authority, admissionPolicy(EventClassSubscription)) }()
	for index := 1; index <= 19; index++ {
		timer := c2AwaitTimer(t, clock)
		assert.Equal(t, start.Add(time.Duration(index)*100*time.Millisecond), timer.due)
		assert.Empty(t, observer.snapshot())
		clock.AdvanceTo(timer.due)
	}
	assert.Equal(t, admissionExpected(AdmissionCloseOverload, AdmissionSourceRedis, time.Millisecond, true), c2AwaitDecision(t, result))
	calls := engine.snapshot()
	require.Len(t, calls, 20)
	assert.LessOrEqual(t, len(calls), 21)
	for index, call := range calls {
		assert.Equal(t, start.Add(time.Duration(index)*100*time.Millisecond), call.start)
		assert.True(t, call.start.Before(start.Add(2*time.Second)))
		assert.Equal(t, start.Add(2*time.Second), call.hold)
	}
	assert.Equal(t, []finalizationRecord{{EventClassSubscription, false, false}}, observer.snapshot())
}
func TestEventAdmissionGateLateTimerWakeDoesNotRestartHold(t *testing.T) {
	client, engine, clock, observer := c2Client(t, true)
	authority := c2Authority(t, client)
	start := clock.Now()
	engine.run = func(int, c2EngineCall) AdmissionDecision { return c2Deny(20 * time.Millisecond) }
	result := make(chan AdmissionDecision, 1)
	go func() { result <- client.admitInboundEvent(authority, admissionPolicy(EventClassReady)) }()
	timer := c2AwaitTimer(t, clock)
	assert.Equal(t, start.Add(100*time.Millisecond), timer.due)
	clock.AdvanceTo(start.Add(2 * time.Second))
	assert.Equal(t, admissionExpected(AdmissionCloseOverload, AdmissionSourceRedis, 20*time.Millisecond, true), c2AwaitDecision(t, result))
	assert.Len(t, engine.snapshot(), 1)
	assert.Empty(t, clock.requests)
	assert.Equal(t, []finalizationRecord{{EventClassReady, false, false}}, observer.snapshot())
}

func TestEventAdmissionGateLaterWarmOrUnavailableEvidenceFinalizesOnce(t *testing.T) {
	for _, last := range []AdmissionDecision{
		admissionExpected(AdmissionAllow, AdmissionSourceWarmLocal, 0, false),
		admissionExpected(AdmissionCloseOverload, AdmissionSourceUnavailable, time.Second, false),
	} {
		t.Run(fmt.Sprintf("action%d", last.Action), func(t *testing.T) {
			client, engine, clock, observer := c2Client(t, true)
			authority := c2Authority(t, client)
			engine.run = func(index int, _ c2EngineCall) AdmissionDecision {
				if index == 0 {
					return c2Deny(time.Millisecond)
				}
				return last
			}
			result := make(chan AdmissionDecision, 1)
			go func() { result <- client.admitInboundEvent(authority, admissionPolicy(EventClassSubscription)) }()
			timer := c2AwaitTimer(t, clock)
			assert.Empty(t, observer.snapshot())
			clock.AdvanceTo(timer.due)
			assert.Equal(t, last, c2AwaitDecision(t, result))
			assert.Len(t, engine.snapshot(), 2)
			assert.Empty(t, clock.requests)
			assert.Equal(t, []finalizationRecord{{EventClassSubscription, last.Action == AdmissionAllow, true}}, observer.snapshot())
		})
	}
}
func TestEventAdmissionGateCanceledHoldStopsTimerAndAbandonsWithoutCount(t *testing.T) {
	for _, cause := range []string{"parent canceled", "socket canceled", "Hub canceled", "security terminal", "token expired"} {
		t.Run(cause, func(t *testing.T) {
			client, engine, clock, observer := c2Client(t, true)
			authority := c2Authority(t, client)
			parent, cancel := context.WithCancel(authority)
			defer cancel()
			engine.run = func(int, c2EngineCall) AdmissionDecision { return c2Deny(time.Millisecond) }
			result := make(chan AdmissionDecision, 1)
			go func() { result <- client.admitInboundEvent(parent, admissionPolicy(EventClassReady)) }()
			timer := c2AwaitTimer(t, clock)
			assert.Empty(t, observer.snapshot())
			switch cause {
			case "parent canceled":
				cancel()
			case "socket canceled":
				client.cancelIngressLifecycle()
			case "Hub canceled":
				client.Hub.ingressCancel()
			case "security terminal":
				client.securityTerminal.Store(true)
				clock.AdvanceTo(timer.due)
			case "token expired":
				client.deadlineMu.Lock()
				client.TokenExpiresAt = time.Now().Add(-time.Second)
				client.deadlineMu.Unlock()
				require.NoError(t, parent.Err(), "actual expiry recheck must not rely on delayed context notification")
				clock.AdvanceTo(timer.due)
			}
			assert.Equal(t, finalizationUnavailable(), c2AwaitDecision(t, result))
			assert.Len(t, engine.snapshot(), 1)
			assert.Empty(t, observer.snapshot())
			assert.Empty(t, client.Send)
			assert.False(t, timer.isActive())
			if cause != "security terminal" && cause != "token expired" {
				assert.Positive(t, timer.stopCalls.Load(), "cancellation must stop its still-pending timer")
			}
		})
	}
}
func TestEventAdmissionGateAuthorityWinsOverSuccessfulEngineReply(t *testing.T) {
	for _, cause := range []string{"parent canceled", "terminal", "expired", "engine abandon"} {
		t.Run(cause, func(t *testing.T) {
			client, engine, _, observer := c2Client(t, true)
			authority := c2Authority(t, client)
			parent, cancel := context.WithCancel(authority)
			defer cancel()
			engine.run = func(int, c2EngineCall) AdmissionDecision {
				switch cause {
				case "parent canceled":
					cancel()
				case "terminal":
					client.securityTerminal.Store(true)
				case "expired":
					client.deadlineMu.Lock()
					client.TokenExpiresAt = time.Now().Add(-time.Second)
					client.deadlineMu.Unlock()
				case "engine abandon":
					return finalizationUnavailable()
				}
				return admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, false)
			}
			assert.Equal(t, finalizationUnavailable(), client.admitInboundEvent(parent, admissionPolicy(EventClassProfile)))
			assert.Len(t, engine.snapshot(), 1)
			assert.Empty(t, observer.snapshot())
			assert.Empty(t, client.Send)
		})
	}
}
func TestEventAdmissionGateInvalidPolicyOrMissingEngineFailsClosed(t *testing.T) {
	for _, policy := range []EventPolicy{
		{}, {Class: 255, Scope: BudgetScopeAccount}, {Class: EventClassContent, Scope: BudgetScopeAccount},
		{Class: EventClassProfile, Scope: BudgetScopeAccount, MayHold: true},
		{Class: EventClassReady, Scope: BudgetScopeAccount},
		{Class: EventClassPresence, Scope: BudgetScopeLocalMaintenance, MayHold: true},
		{Class: EventClassTyping, Scope: BudgetScopeLocalMaintenance},
	} {
		client, engine, clock, observer := c2Client(t, true)
		assert.Equal(t, finalizationUnavailable(), client.admitInboundEvent(c2Authority(t, client), policy))
		assert.Empty(t, engine.snapshot())
		assert.Empty(t, observer.snapshot())
		assert.Empty(t, clock.requests)
		assert.Nil(t, client.eventHeartbeatBudget)
	}
	client, _, clock, observer := c2Client(t, true)
	client.Hub.eventAdmitter = nil
	assert.Equal(t, finalizationUnavailable(), client.admitInboundEvent(c2Authority(t, client), admissionPolicy(EventClassProfile)))
	assert.Empty(t, observer.snapshot())
	assert.Empty(t, clock.requests)
}
func TestEventAdmissionGateMalformedEngineDecisionNeverAllowsOrHolds(t *testing.T) {
	for _, decision := range []AdmissionDecision{
		{}, {Action: 255, Source: AdmissionSourceRedis}, {Action: AdmissionAllow, Source: 255},
		admissionExpected(AdmissionAllow, AdmissionSourceRedis, time.Millisecond, false),
		admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, true),
		admissionExpected(AdmissionRefuse, AdmissionSourceRedis, time.Millisecond, false),
		admissionExpected(AdmissionRefuse, AdmissionSourceRedis, 0, true),
		admissionExpected(AdmissionRefuse, AdmissionSourceRedis, -time.Millisecond, true),
		admissionExpected(AdmissionRefuse, AdmissionSourceRedis, time.Millisecond+time.Nanosecond, true),
		admissionExpected(AdmissionRefuse, AdmissionSourceRedis, 120*time.Second+time.Millisecond, true),
		admissionExpected(AdmissionCloseOverload, AdmissionSourceRedis, time.Millisecond, true),
		admissionExpected(AdmissionAllow, AdmissionSourceWarmLocal, time.Millisecond, false),
		admissionExpected(AdmissionAllow, AdmissionSourceUnavailable, 0, false),
		admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false), // Hold class requires overload.
		admissionExpected(AdmissionCloseOverload, AdmissionSourceUnavailable, time.Millisecond, false),
		admissionExpected(AdmissionCloseOverload, AdmissionSourceUnavailable, time.Second, true),
		admissionExpected(AdmissionAbandon, AdmissionSourceUnavailable, time.Second, false),
		admissionExpected(AdmissionAllow, AdmissionSourceExisting, 0, false),
	} {
		t.Run(fmt.Sprintf("action%d_source%d_retry%d_flag%t", decision.Action, decision.Source, decision.RetryAfter, decision.DefinitiveRateDeny), func(t *testing.T) {
			client, engine, clock, observer := c2Client(t, true)
			engine.run = func(int, c2EngineCall) AdmissionDecision { return decision }
			assert.Equal(t, finalizationUnavailable(), client.admitInboundEvent(c2Authority(t, client), admissionPolicy(EventClassSubscription)))
			assert.Len(t, engine.snapshot(), 1)
			assert.Empty(t, clock.requests)
			assert.Empty(t, observer.snapshot())
			assert.Empty(t, client.Send)
		})
	}
}
func TestEventAdmissionGateHeartbeatInitializesOnceWithTwoTokensAndFiveSecondRefill(t *testing.T) {
	client, engine, clock, observer := c2Client(t, true)
	authority := c2Authority(t, client)
	heartbeat := EventPolicy{Class: EventClassPresence, Scope: BudgetScopeLocalMaintenance}
	require.Nil(t, client.eventHeartbeatBudget)
	allow := admissionExpected(AdmissionAllow, AdmissionSourceLocalMaintenance, 0, false)
	assert.Equal(t, allow, client.admitInboundEvent(authority, heartbeat))
	require.NotNil(t, client.eventHeartbeatBudget)
	bucket := client.eventHeartbeatBudget
	assert.Equal(t, allow, client.admitInboundEvent(authority, heartbeat))
	denied := client.admitInboundEvent(authority, heartbeat)
	assert.Equal(t, AdmissionRefuse, denied.Action)
	assert.Equal(t, AdmissionSourceLocalMaintenance, denied.Source)
	assert.False(t, denied.DefinitiveRateDeny)
	clock.Advance(5*time.Second - time.Nanosecond)
	assert.Equal(t, AdmissionRefuse, client.admitInboundEvent(authority, heartbeat).Action)
	clock.Advance(time.Nanosecond)
	assert.Equal(t, allow, client.admitInboundEvent(authority, heartbeat))
	assert.Equal(t, AdmissionRefuse, client.admitInboundEvent(authority, heartbeat).Action)
	clock.Advance(100 * time.Second)
	assert.Equal(t, allow, client.admitInboundEvent(authority, heartbeat))
	assert.Equal(t, allow, client.admitInboundEvent(authority, heartbeat))
	assert.Equal(t, AdmissionRefuse, client.admitInboundEvent(authority, heartbeat).Action)
	assert.Same(t, bucket, client.eventHeartbeatBudget, "later events cannot reset heartbeat credit")
	assert.Empty(t, engine.snapshot())
	assert.Empty(t, clock.requests)
	assert.Empty(t, client.Send)
	assert.Equal(t, []finalizationRecord{
		{EventClassPresence, true, false}, {EventClassPresence, true, false}, {EventClassPresence, false, false},
		{EventClassPresence, false, false}, {EventClassPresence, true, false}, {EventClassPresence, false, false},
		{EventClassPresence, true, false}, {EventClassPresence, true, false}, {EventClassPresence, false, false},
	}, observer.snapshot())
	// A second socket starts with its own two credits; no account map is shared.
	other := responseClient(t, true)
	other.Hub.eventAdmitter = engine
	other.eventAdmissionNow = clock.Now
	other.eventAdmissionNewTimer = clock.NewTimer
	assert.Equal(t, allow, other.admitInboundEvent(c2Authority(t, other), heartbeat))
	assert.Equal(t, allow, other.admitInboundEvent(c2Authority(t, other), heartbeat))
	assert.NotSame(t, bucket, other.eventHeartbeatBudget)
	assert.Empty(t, engine.snapshot())
}
func TestEventAdmissionGateDisabledStillHonorsCanceledAuthority(t *testing.T) {
	client, engine, clock, observer := c2Client(t, false)
	parent, cancel := context.WithCancel(c2Authority(t, client))
	cancel()
	assert.Equal(t, finalizationUnavailable(), client.admitInboundEvent(parent, admissionPolicy(EventClassReady)))
	assert.Empty(t, engine.snapshot())
	assert.Empty(t, observer.snapshot())
	assert.Empty(t, clock.requests)
	assert.Nil(t, client.eventHeartbeatBudget)
}

func TestEventAdmissionGateResultAtHoldBoundaryCannotBecomeAllowance(t *testing.T) {
	for _, priorDenial := range []bool{false, true} {
		for _, completion := range []time.Duration{1999 * time.Millisecond, 2 * time.Second} {
			t.Run(fmt.Sprintf("priorDeny%t_completion%d", priorDenial, completion), func(t *testing.T) {
				client, engine, clock, observer := c2Client(t, true)
				authority := c2Authority(t, client)
				start := clock.Now()
				// A deliberately late but structurally valid fake engine reply exercises
				// C's own hold boundary. B's separate 90/100ms checks are not replaced.
				engine.run = func(index int, _ c2EngineCall) AdmissionDecision {
					if priorDenial && index == 0 {
						return c2Deny(20 * time.Millisecond)
					}
					clock.AdvanceTo(start.Add(completion))
					return admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, false)
				}
				result := make(chan AdmissionDecision, 1)
				go func() { result <- client.admitInboundEvent(authority, admissionPolicy(EventClassReady)) }()
				if priorDenial {
					timer := c2AwaitTimer(t, clock)
					assert.Equal(t, start.Add(100*time.Millisecond), timer.due)
					clock.AdvanceTo(timer.due)
				}
				want := admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, false)
				degraded := false
				if completion == 2*time.Second {
					want = admissionExpected(AdmissionCloseOverload, AdmissionSourceUnavailable, time.Second, false)
					degraded = true
					if priorDenial {
						want = admissionExpected(AdmissionCloseOverload, AdmissionSourceRedis, 20*time.Millisecond, true)
						degraded = false
					}
				}
				assert.Equal(t, want, c2AwaitDecision(t, result))
				expectedCalls := 1
				if priorDenial {
					expectedCalls = 2
				}
				assert.Len(t, engine.snapshot(), expectedCalls)
				assert.Empty(t, clock.requests)
				assert.Equal(t, []finalizationRecord{{EventClassReady, completion < 2*time.Second, degraded}}, observer.snapshot())
			})
		}
	}
}
func TestEventAdmissionGateAuthorityLossWinsAtExhaustedHoldBoundary(t *testing.T) {
	for _, cause := range []string{"parent canceled", "security terminal", "token expired"} {
		t.Run(cause, func(t *testing.T) {
			client, engine, clock, observer := c2Client(t, true)
			authority := c2Authority(t, client)
			parent, cancel := context.WithCancel(authority)
			defer cancel()
			engine.run = func(int, c2EngineCall) AdmissionDecision {
				clock.Advance(2 * time.Second)
				switch cause {
				case "parent canceled":
					cancel()
				case "security terminal":
					client.securityTerminal.Store(true)
				case "token expired":
					client.deadlineMu.Lock()
					client.TokenExpiresAt = time.Now().Add(-time.Second)
					client.deadlineMu.Unlock()
				}
				return admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, false)
			}
			assert.Equal(t, finalizationUnavailable(), client.admitInboundEvent(parent, admissionPolicy(EventClassSubscription)))
			assert.Len(t, engine.snapshot(), 1)
			assert.Empty(t, observer.snapshot())
			assert.Empty(t, clock.requests)
			assert.Empty(t, client.Send)
		})
	}
}

func TestEventAdmissionGateCurrentUnavailableSupersedesEarlierRateDenialAtHoldEnd(t *testing.T) {
	for _, completion := range []time.Duration{1999 * time.Millisecond, 2 * time.Second} {
		t.Run(fmt.Sprint(completion), func(t *testing.T) {
			client, engine, clock, observer := c2Client(t, true)
			authority := c2Authority(t, client)
			start := clock.Now()
			unavailable := admissionExpected(AdmissionCloseOverload, AdmissionSourceUnavailable, time.Second, false)
			engine.run = func(index int, _ c2EngineCall) AdmissionDecision {
				if index == 0 {
					return c2Deny(20 * time.Millisecond)
				}
				clock.AdvanceTo(start.Add(completion))
				return unavailable
			}
			result := make(chan AdmissionDecision, 1)
			go func() { result <- client.admitInboundEvent(authority, admissionPolicy(EventClassSubscription)) }()
			timer := c2AwaitTimer(t, clock)
			assert.Equal(t, start.Add(100*time.Millisecond), timer.due)
			assert.Empty(t, observer.snapshot())
			clock.AdvanceTo(timer.due)
			// The current failure is uncertain execution, even at hold exhaustion.
			// An earlier rate denial cannot turn it into a definitive Redis refusal.
			assert.Equal(t, unavailable, c2AwaitDecision(t, result))
			assert.Len(t, engine.snapshot(), 2)
			assert.Empty(t, clock.requests)
			assert.Equal(t, []finalizationRecord{{EventClassSubscription, false, true}}, observer.snapshot())
			assert.Empty(t, client.Send)
		})
	}
}
