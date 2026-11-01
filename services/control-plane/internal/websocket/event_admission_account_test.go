package websocket

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Independent backend double: the real admitter owns validation, reservation,
// slot acquisition, deadline priority and shutdown registration.
type admissionBackendCall struct {
	context         context.Context
	user            uuid.UUID
	class           EventClass
	interval, burst int64
}
type admissionBackendFake struct {
	mu      sync.Mutex
	calls   []admissionBackendCall
	decide  func(admissionBackendCall) (RedisBudgetDecision, error)
	closeFn func() error
	closes  int
}

func (b *admissionBackendFake) Decide(ctx context.Context, user uuid.UUID, class EventClass, interval, burst int64) (RedisBudgetDecision, error) {
	call := admissionBackendCall{ctx, user, class, interval, burst}
	b.mu.Lock()
	b.calls = append(b.calls, call)
	fn := b.decide
	b.mu.Unlock()
	if fn != nil {
		return fn(call)
	}
	return RedisBudgetDecision{Allowed: true}, nil
}
func (b *admissionBackendFake) Close() error {
	b.mu.Lock()
	b.closes++
	fn := b.closeFn
	b.mu.Unlock()
	if fn != nil {
		return fn()
	}
	return nil
}
func (b *admissionBackendFake) setDecide(fn func(admissionBackendCall) (RedisBudgetDecision, error)) {
	b.mu.Lock()
	b.decide = fn
	b.mu.Unlock()
}
func (b *admissionBackendFake) setReply(reply RedisBudgetDecision, err error) {
	b.setDecide(func(admissionBackendCall) (RedisBudgetDecision, error) { return reply, err })
}
func (b *admissionBackendFake) counts() (int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.calls), b.closes
}
func (b *admissionBackendFake) lastCall() admissionBackendCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[len(b.calls)-1]
}

var admissionTestAccount = uuid.MustParse("22222222-2222-4222-8222-222222222222")

func admissionPolicy(class EventClass) EventPolicy {
	return EventPolicy{Class: class, Scope: BudgetScopeAccount, MayHold: class == EventClassSubscription || class == EventClassReady}
}
func admissionExpected(action AdmissionAction, source AdmissionSource, retry time.Duration, definitive bool) AdmissionDecision {
	return AdmissionDecision{Action: action, Source: source, RetryAfter: retry, DefinitiveRateDeny: definitive}
}
func admissionNewPublic(t *testing.T, cfg EventBudgetConfig, backend AccountBudgetBackend) EventAdmitter {
	t.Helper()
	engine, err := NewEventAdmitter(cfg, backend)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, engine.Close()) })
	return engine
}
func admissionAwait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("admission fixture failed to settle")
		var zero T
		return zero
	}
}
func TestEventAdmitterConstructorValidationAndOwnership(t *testing.T) {
	for _, tt := range []struct {
		name        string
		cfg         EventBudgetConfig
		withBackend bool
	}{
		{"enabled missing backend", EventBudgetConfig{true, 100}, false},
		{"disabled supplied backend", EventBudgetConfig{false, 100}, true},
		{"enabled invalid scale", EventBudgetConfig{true, 24}, true},
		{"disabled invalid scale", EventBudgetConfig{false, 401}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			backend := &admissionBackendFake{}
			var input AccountBudgetBackend
			if tt.withBackend {
				input = backend
			}
			engine, err := NewEventAdmitter(tt.cfg, input)
			require.Error(t, err)
			assert.Nil(t, engine)
			calls, closes := backend.counts()
			assert.Zero(t, calls)
			assert.Zero(t, closes, "failed construction must leave ownership with caller")
		})
	}
	backend := &admissionBackendFake{}
	engine := admissionNewPublic(t, EventBudgetConfig{true, 100}, backend)
	calls, closes := backend.counts()
	assert.Zero(t, calls)
	assert.Zero(t, closes)
	require.NoError(t, engine.Close())
	_, closes = backend.counts()
	assert.Equal(t, 1, closes)
}
func TestEventAdmitterDisabledValidatesAccountTuples(t *testing.T) {
	engine := admissionNewPublic(t, EventBudgetConfig{false, 100}, nil)
	for _, class := range []EventClass{EventClassTyping, EventClassSubscription, EventClassPresence, EventClassProfile, EventClassServer, EventClassReady, EventClassRenewal} {
		assert.Equal(t, admissionExpected(AdmissionAllow, AdmissionSourceExisting, 0, false), engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(class), time.Time{}))
	}
	invalid := []EventPolicy{
		{}, {Class: EventClassContent, Scope: BudgetScopeExistingContent},
		{Class: EventClassPresence, Scope: BudgetScopeLocalMaintenance},
		{Class: EventClassProfile, Scope: BudgetScopeAccount, MayHold: true},
		{Class: EventClassSubscription, Scope: BudgetScopeAccount},
		{Class: EventClassReady, Scope: BudgetScopeAccount},
		{Class: 255, Scope: BudgetScopeAccount},
		{Class: EventClassProfile, Scope: 0},
	}
	for _, policy := range invalid {
		assert.Equal(t, admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false), engine.Admit(t.Context(), admissionTestAccount, policy, time.Time{}))
	}
	assert.Equal(t, admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false), engine.Admit(t.Context(), uuid.Nil, admissionPolicy(EventClassProfile), time.Time{}))
	require.NoError(t, engine.Close())
	assert.Equal(t, admissionExpected(AdmissionAbandon, AdmissionSourceUnavailable, 0, false), engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{}))
}
func TestEventAdmitterValidPoliciesReachBackendWithApprovedParameters(t *testing.T) {
	tests := []struct {
		class           EventClass
		interval, burst int64
	}{
		{EventClassTyping, 100000, 40}, {EventClassSubscription, 12500, 160}, {EventClassPresence, 500000, 16},
		{EventClassProfile, 5000000, 8}, {EventClassServer, 2000000, 16}, {EventClassReady, 1000000, 16}, {EventClassRenewal, 2000000, 16},
	}
	for _, tt := range tests {
		t.Run(tt.class.String(), func(t *testing.T) {
			backend := &admissionBackendFake{}
			cfg := EventBudgetConfig{true, 100}
			engine := admissionNewPublic(t, cfg, backend)
			cfg.ScalePercent = 25
			got := engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(tt.class), time.Time{})
			assert.Equal(t, admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, false), got)
			calls, _ := backend.counts()
			require.Equal(t, 1, calls)
			call := backend.lastCall()
			assert.Equal(t, admissionTestAccount, call.user)
			assert.Equal(t, tt.class, call.class)
			assert.Equal(t, tt.interval, call.interval)
			assert.Equal(t, tt.burst, call.burst)
		})
	}
}
func TestEventAdmitterUnavailableAndDefinitiveDenial(t *testing.T) {
	for _, class := range []EventClass{EventClassProfile, EventClassSubscription, EventClassReady} {
		t.Run(class.String(), func(t *testing.T) {
			backend := &admissionBackendFake{}
			backend.setReply(RedisBudgetDecision{}, ErrAccountBudgetUnavailable)
			engine := admissionNewPublic(t, EventBudgetConfig{true, 100}, backend)
			action := AdmissionRefuse
			if class == EventClassSubscription || class == EventClassReady {
				action = AdmissionCloseOverload
			}
			assert.Equal(t, admissionExpected(action, AdmissionSourceUnavailable, time.Second, false), engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(class), time.Time{}))
			backend.setReply(RedisBudgetDecision{RetryAfter: 5 * time.Second}, nil)
			assert.Equal(t, admissionExpected(AdmissionRefuse, AdmissionSourceRedis, 5*time.Second, true), engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(class), time.Time{}))
			calls, _ := backend.counts()
			assert.Equal(t, 2, calls, "neither branch retries an account decision")
		})
	}
}
func TestEventAdmitterRejectsMalformedBackendDecisions(t *testing.T) {
	for _, reply := range []RedisBudgetDecision{
		{Allowed: true, RetryAfter: time.Millisecond}, {RetryAfter: 0}, {RetryAfter: -time.Millisecond},
		{RetryAfter: time.Nanosecond}, {RetryAfter: time.Millisecond + time.Nanosecond},
		{RetryAfter: 119*time.Second + time.Nanosecond}, {RetryAfter: 120*time.Second + time.Millisecond},
	} {
		t.Run(fmt.Sprintf("allow%t_retry%d", reply.Allowed, reply.RetryAfter), func(t *testing.T) {
			backend := &admissionBackendFake{}
			backend.setReply(reply, nil)
			clock := admissionNewClock()
			engine := admissionNewClocked(t, EventBudgetConfig{true, 100}, backend, clock)
			assert.Equal(t, admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false), engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{}))
			calls, _ := backend.counts()
			assert.Equal(t, 1, calls)
			clock.Add(40 * time.Second)
			backend.setReply(RedisBudgetDecision{}, ErrAccountBudgetUnavailable)
			assert.Equal(t, admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false), engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{}), "malformed denial must not allocate refillable warm state")
		})
	}
}
func TestEventAdmitterCloseCancelsAndJoinsCallsBeforeBackendClose(t *testing.T) {
	entered := make(chan context.Context, 1)
	canceled := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	backend := &admissionBackendFake{closeFn: func() error { return errors.New("synthetic backend cleanup") }}
	backend.setDecide(func(call admissionBackendCall) (RedisBudgetDecision, error) {
		entered <- call.context
		<-call.context.Done()
		close(canceled)
		<-release
		return RedisBudgetDecision{Allowed: true}, nil
	})
	engine, err := NewEventAdmitter(EventBudgetConfig{true, 100}, backend)
	require.NoError(t, err)
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		assert.EqualError(t, engine.Close(), "synthetic backend cleanup")
	})
	result := make(chan AdmissionDecision, 1)
	go func() {
		result <- engine.Admit(context.Background(), admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{})
	}()
	child := admissionAwait(t, entered)
	closeResult := make(chan error, 1)
	go func() { closeResult <- engine.Close() }()
	admissionAwait(t, canceled)
	assert.ErrorIs(t, child.Err(), context.Canceled)
	_, closes := backend.counts()
	assert.Zero(t, closes, "backend cannot close before admitted call settles")
	select {
	case err := <-closeResult:
		t.Fatalf("Close returned before admitted call settled: %v", err)
	default:
	}
	assert.Equal(t, admissionExpected(AdmissionAbandon, AdmissionSourceUnavailable, 0, false), engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{}))
	releaseOnce.Do(func() { close(release) })
	assert.Equal(t, admissionExpected(AdmissionAbandon, AdmissionSourceUnavailable, 0, false), admissionAwait(t, result))
	assert.EqualError(t, admissionAwait(t, closeResult), "synthetic backend cleanup")
	assert.EqualError(t, engine.Close(), "synthetic backend cleanup")
	calls, closes := backend.counts()
	assert.Equal(t, 1, calls)
	assert.Equal(t, 1, closes)
}

// Future wall base prevents the real context timer from deciding logical-clock
// vectors. All arithmetic still retains Go's monotonic component via Add.
type admissionClock struct {
	mu  sync.Mutex
	now time.Time
}

func admissionNewClock() *admissionClock { return &admissionClock{now: time.Now().Add(time.Hour)} }
func (c *admissionClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *admissionClock) Add(delta time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delta)
	c.mu.Unlock()
}
func admissionNewClocked(t *testing.T, cfg EventBudgetConfig, backend AccountBudgetBackend, clock *admissionClock) *accountEventAdmitter {
	t.Helper()
	engine, err := newEventAdmitterWithClock(cfg, backend, clock.Now)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, engine.Close()) })
	return engine
}
func admissionWarm(t *testing.T, engine EventAdmitter, user uuid.UUID) {
	t.Helper()
	require.Equal(t, admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, false), engine.Admit(t.Context(), user, admissionPolicy(EventClassProfile), time.Time{}))
}
func TestEventAdmitterWarmStateStartsEmptyAndRefillsAtExactBoundary(t *testing.T) {
	clock := admissionNewClock()
	backend := &admissionBackendFake{}
	engine := admissionNewClocked(t, EventBudgetConfig{true, 100}, backend, clock)
	admissionWarm(t, engine, admissionTestAccount)
	backend.setReply(RedisBudgetDecision{}, ErrAccountBudgetUnavailable)
	refuse := admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false)
	allow := admissionExpected(AdmissionAllow, AdmissionSourceWarmLocal, 0, false)
	policy := admissionPolicy(EventClassProfile)
	assert.Equal(t, refuse, engine.Admit(t.Context(), admissionTestAccount, policy, time.Time{}))
	clock.Add(20*time.Second - time.Nanosecond)
	assert.Equal(t, refuse, engine.Admit(t.Context(), admissionTestAccount, policy, time.Time{}))
	clock.Add(time.Nanosecond)
	assert.Equal(t, allow, engine.Admit(t.Context(), admissionTestAccount, policy, time.Time{}))
	assert.Equal(t, refuse, engine.Admit(t.Context(), admissionTestAccount, policy, time.Time{}))
	clock.Add(time.Minute)
	assert.Equal(t, allow, engine.Admit(t.Context(), admissionTestAccount, policy, time.Time{}))
	assert.Equal(t, allow, engine.Admit(t.Context(), admissionTestAccount, policy, time.Time{}))
	assert.Equal(t, refuse, engine.Admit(t.Context(), admissionTestAccount, policy, time.Time{}), "profile fallback caps at two regardless of idle time")
}
func TestEventAdmitterAllWarmSlotsUseIndependentFallbackParameters(t *testing.T) {
	for _, tt := range []struct {
		class    EventClass
		scale    int
		interval time.Duration
		burst    int
	}{
		{EventClassTyping, 100, 400 * time.Millisecond, 10}, {EventClassSubscription, 100, 50 * time.Millisecond, 40},
		{EventClassPresence, 100, 2 * time.Second, 4}, {EventClassProfile, 100, 20 * time.Second, 2},
		{EventClassServer, 100, 8 * time.Second, 4}, {EventClassReady, 100, 4 * time.Second, 4}, {EventClassRenewal, 100, 8 * time.Second, 4},
		{EventClassProfile, 25, 80 * time.Second, 1}, {EventClassProfile, 400, 5 * time.Second, 8},
	} {
		t.Run(fmt.Sprintf("%s_scale%d", tt.class.String(), tt.scale), func(t *testing.T) {
			clock := admissionNewClock()
			backend := &admissionBackendFake{}
			engine := admissionNewClocked(t, EventBudgetConfig{true, tt.scale}, backend, clock)
			admissionWarm(t, engine, admissionTestAccount)
			backend.setReply(RedisBudgetDecision{}, ErrAccountBudgetUnavailable)
			action := AdmissionRefuse
			if tt.class == EventClassSubscription || tt.class == EventClassReady {
				action = AdmissionCloseOverload
			}
			refuse := admissionExpected(action, AdmissionSourceUnavailable, time.Second, false)
			assert.Equal(t, refuse, engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(tt.class), time.Time{}))
			clock.Add(tt.interval * time.Duration(tt.burst))
			for i := 0; i < tt.burst; i++ {
				require.Equal(t, admissionExpected(AdmissionAllow, AdmissionSourceWarmLocal, 0, false), engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(tt.class), time.Time{}))
			}
			assert.Equal(t, refuse, engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(tt.class), time.Time{}))
		})
	}
}
func TestEventAdmitterReservationIsNeverRefunded(t *testing.T) {
	for _, outcome := range []string{"remote allow", "remote deny", "remote error", "parent canceled"} {
		t.Run(outcome, func(t *testing.T) {
			clock := admissionNewClock()
			backend := &admissionBackendFake{}
			engine := admissionNewClocked(t, EventBudgetConfig{true, 100}, backend, clock)
			admissionWarm(t, engine, admissionTestAccount)
			clock.Add(20 * time.Second)
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			want := admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, false)
			switch outcome {
			case "remote deny":
				backend.setReply(RedisBudgetDecision{RetryAfter: 5 * time.Second}, nil)
				want = admissionExpected(AdmissionRefuse, AdmissionSourceRedis, 5*time.Second, true)
			case "remote error":
				backend.setReply(RedisBudgetDecision{}, ErrAccountBudgetUnavailable)
				want = admissionExpected(AdmissionAllow, AdmissionSourceWarmLocal, 0, false)
			case "parent canceled":
				backend.setDecide(func(admissionBackendCall) (RedisBudgetDecision, error) {
					cancel()
					return RedisBudgetDecision{Allowed: true}, nil
				})
				want = admissionExpected(AdmissionAbandon, AdmissionSourceUnavailable, 0, false)
			}
			assert.Equal(t, want, engine.Admit(parent, admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{}))
			backend.setReply(RedisBudgetDecision{}, ErrAccountBudgetUnavailable)
			assert.Equal(t, admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false), engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{}), "the one reservation at 20s stays spent")
		})
	}
}
func TestEventAdmitterValidDenialWarmsButCannotBecomeWarmAllowance(t *testing.T) {
	clock := admissionNewClock()
	backend := &admissionBackendFake{}
	backend.setReply(RedisBudgetDecision{RetryAfter: 5 * time.Second}, nil)
	engine := admissionNewClocked(t, EventBudgetConfig{true, 100}, backend, clock)
	wantDeny := admissionExpected(AdmissionRefuse, AdmissionSourceRedis, 5*time.Second, true)
	assert.Equal(t, wantDeny, engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{}))
	backend.setReply(RedisBudgetDecision{}, ErrAccountBudgetInvalidState)
	assert.Equal(t, admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false), engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{}))
	clock.Add(20 * time.Second)
	backend.setReply(RedisBudgetDecision{RetryAfter: 5 * time.Second}, nil)
	assert.Equal(t, wantDeny, engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{}))
	backend.setReply(RedisBudgetDecision{}, ErrAccountBudgetInvalidState)
	assert.Equal(t, admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false), engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{}))
	clock.Add(20 * time.Second)
	assert.Equal(t, admissionExpected(AdmissionAllow, AdmissionSourceWarmLocal, 0, false), engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{}))
}
func TestEventAdmitterUnprovenReplyNeverAllocatesWarmUser(t *testing.T) {
	for _, outcome := range []string{"unavailable", "invalid state", "malformed", "late", "canceled"} {
		t.Run(outcome, func(t *testing.T) {
			clock := admissionNewClock()
			backend := &admissionBackendFake{}
			engine := admissionNewClocked(t, EventBudgetConfig{true, 100}, backend, clock)
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch outcome {
			case "unavailable":
				backend.setReply(RedisBudgetDecision{}, ErrAccountBudgetUnavailable)
			case "invalid state":
				backend.setReply(RedisBudgetDecision{}, ErrAccountBudgetInvalidState)
			case "malformed":
				backend.setReply(RedisBudgetDecision{Allowed: true, RetryAfter: time.Millisecond}, nil)
			case "late":
				backend.setDecide(func(admissionBackendCall) (RedisBudgetDecision, error) {
					clock.Add(95 * time.Millisecond)
					return RedisBudgetDecision{Allowed: true}, nil
				})
			case "canceled":
				backend.setDecide(func(admissionBackendCall) (RedisBudgetDecision, error) {
					cancel()
					return RedisBudgetDecision{Allowed: true}, nil
				})
			}
			first := engine.Admit(parent, admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{})
			if outcome == "canceled" {
				assert.Equal(t, admissionExpected(AdmissionAbandon, AdmissionSourceUnavailable, 0, false), first)
			} else {
				assert.Equal(t, admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false), first)
			}
			clock.Add(40 * time.Second)
			backend.setReply(RedisBudgetDecision{}, ErrAccountBudgetUnavailable)
			assert.Equal(t, admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false), engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{}), "failed result must not have installed a refillable user")
		})
	}
}
func admissionIndexedUser(index int) uuid.UUID {
	user := uuid.UUID{0: 0x44, 6: 0x40, 8: 0x80}
	user[14] = byte((index >> 8) & 0xff)
	user[15] = byte(index & 0xff)
	return user
}
func TestEventAdmitterFullMapKeepsHealthyAuthorityAndResidents(t *testing.T) {
	clock := admissionNewClock()
	backend := &admissionBackendFake{}
	engine := admissionNewClocked(t, EventBudgetConfig{true, 100}, backend, clock)
	for i := 1; i <= 10000; i++ {
		admissionWarm(t, engine, admissionIndexedUser(i))
	}
	extra := admissionIndexedUser(10001)
	admissionWarm(t, engine, extra)
	clock.Add(20 * time.Second)
	backend.setReply(RedisBudgetDecision{}, ErrAccountBudgetUnavailable)
	assert.Equal(t, admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false), engine.Admit(t.Context(), extra, admissionPolicy(EventClassProfile), time.Time{}), "healthy user10001 must not evict a resident or gain outage credit")
	for _, index := range []int{1, 10000} {
		assert.Equal(t, admissionExpected(AdmissionAllow, AdmissionSourceWarmLocal, 0, false), engine.Admit(t.Context(), admissionIndexedUser(index), admissionPolicy(EventClassProfile), time.Time{}))
	}
	assert.Equal(t, admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false), engine.Admit(t.Context(), admissionIndexedUser(10002), admissionPolicy(EventClassProfile), time.Time{}))
}

func TestEventAdmitterRemoteAndDecisionDeadlineBoundaries(t *testing.T) {
	for _, warm := range []bool{false, true} {
		for _, tt := range []struct {
			name       string
			elapsed    time.Duration
			remoteDeny bool
		}{
			{"before remote", 89 * time.Millisecond, false}, {"at remote", 90 * time.Millisecond, false},
			{"between deadlines", 95 * time.Millisecond, false}, {"late remote denial", 95 * time.Millisecond, true},
			{"at decision", 100 * time.Millisecond, false}, {"after decision", 101 * time.Millisecond, false},
		} {
			t.Run(fmt.Sprintf("warm%t_%s", warm, tt.name), func(t *testing.T) {
				clock := admissionNewClock()
				backend := &admissionBackendFake{}
				engine := admissionNewClocked(t, EventBudgetConfig{true, 100}, backend, clock)
				if warm {
					admissionWarm(t, engine, admissionTestAccount)
					clock.Add(20 * time.Second)
				}
				start := clock.Now()
				backend.setDecide(func(call admissionBackendCall) (RedisBudgetDecision, error) {
					deadline, ok := call.context.Deadline()
					assert.True(t, ok)
					assert.Equal(t, start.Add(90*time.Millisecond), deadline)
					clock.Add(tt.elapsed)
					if tt.remoteDeny {
						return RedisBudgetDecision{RetryAfter: 5 * time.Second}, nil
					}
					return RedisBudgetDecision{Allowed: true}, nil
				})
				want := admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false)
				if tt.elapsed < 90*time.Millisecond {
					want = admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, false)
				} else if warm && tt.elapsed < 100*time.Millisecond {
					want = admissionExpected(AdmissionAllow, AdmissionSourceWarmLocal, 0, false)
				}
				assert.Equal(t, want, engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{}))
			})
		}
	}
}

type admissionParentDeadline struct {
	context.Context
	deadline time.Time
}

func (ctx admissionParentDeadline) Deadline() (time.Time, bool) { return ctx.deadline, true }
func TestEventAdmitterParentAndHoldDeadlinePriority(t *testing.T) {
	for _, elapsed := range []time.Duration{39 * time.Millisecond, 40 * time.Millisecond} {
		t.Run(fmt.Sprintf("parent_deadline_%d", elapsed), func(t *testing.T) {
			clock := admissionNewClock()
			backend := &admissionBackendFake{}
			engine := admissionNewClocked(t, EventBudgetConfig{true, 100}, backend, clock)
			deadline := clock.Now().Add(40 * time.Millisecond)
			parent := admissionParentDeadline{Context: context.Background(), deadline: deadline}
			backend.setDecide(func(call admissionBackendCall) (RedisBudgetDecision, error) {
				actual, ok := call.context.Deadline()
				assert.True(t, ok)
				assert.Equal(t, deadline, actual)
				clock.Add(elapsed)
				return RedisBudgetDecision{Allowed: true}, nil
			})
			want := admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, false)
			if elapsed == 40*time.Millisecond {
				want = admissionExpected(AdmissionAbandon, AdmissionSourceUnavailable, 0, false)
			}
			assert.Equal(t, want, engine.Admit(parent, admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{}))
			assert.NoError(t, parent.Err(), "actual parent deadline wins before cancellation notification")
		})
	}
	for _, elapsed := range []time.Duration{49 * time.Millisecond, 50 * time.Millisecond} {
		t.Run(fmt.Sprintf("hold_deadline_%d", elapsed), func(t *testing.T) {
			clock := admissionNewClock()
			backend := &admissionBackendFake{}
			engine := admissionNewClocked(t, EventBudgetConfig{true, 100}, backend, clock)
			admissionWarm(t, engine, admissionTestAccount)
			clock.Add(time.Second)
			hold := clock.Now().Add(50 * time.Millisecond)
			backend.setDecide(func(call admissionBackendCall) (RedisBudgetDecision, error) {
				deadline, ok := call.context.Deadline()
				assert.True(t, ok)
				assert.Equal(t, hold, deadline)
				clock.Add(elapsed)
				return RedisBudgetDecision{Allowed: true}, nil
			})
			want := admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, false)
			if elapsed == 50*time.Millisecond {
				want = admissionExpected(AdmissionCloseOverload, AdmissionSourceUnavailable, time.Second, false)
			}
			assert.Equal(t, want, engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(EventClassSubscription), hold))
		})
	}
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("entry_validation_enabled%t", enabled), func(t *testing.T) {
			clock := admissionNewClock()
			backend := &admissionBackendFake{}
			var input AccountBudgetBackend
			if enabled {
				input = backend
			}
			engine := admissionNewClocked(t, EventBudgetConfig{enabled, 100}, input, clock)
			assert.Equal(t, admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false), engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(EventClassProfile), clock.Now().Add(time.Second)))
			assert.Equal(t, admissionExpected(AdmissionCloseOverload, AdmissionSourceUnavailable, time.Second, false), engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(EventClassReady), clock.Now()))
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			assert.Equal(t, admissionExpected(AdmissionAbandon, AdmissionSourceUnavailable, 0, false), engine.Admit(canceled, uuid.Nil, EventPolicy{}, clock.Now().Add(-time.Second)), "parent cancellation wins over invalid input and exhausted hold")
			expired := admissionParentDeadline{Context: context.Background(), deadline: clock.Now().Add(-time.Nanosecond)}
			assert.Equal(t, admissionExpected(AdmissionAbandon, AdmissionSourceUnavailable, 0, false), engine.Admit(expired, admissionTestAccount, admissionPolicy(EventClassReady), time.Time{}))
			calls, _ := backend.counts()
			assert.Zero(t, calls)
		})
	}
}
func TestEventAdmitterParentCancellationWinsAfterRemoteReply(t *testing.T) {
	clock := admissionNewClock()
	backend := &admissionBackendFake{}
	engine := admissionNewClocked(t, EventBudgetConfig{true, 100}, backend, clock)
	admissionWarm(t, engine, admissionTestAccount)
	clock.Add(20 * time.Second)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	backend.setDecide(func(admissionBackendCall) (RedisBudgetDecision, error) {
		clock.Add(95 * time.Millisecond)
		cancel()
		return RedisBudgetDecision{Allowed: true}, nil
	})
	assert.Equal(t, admissionExpected(AdmissionAbandon, AdmissionSourceUnavailable, 0, false), engine.Admit(parent, admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{}))
}
func TestEventAdmitterSaturationIsNonblockingAndKeepsWarmReservation(t *testing.T) {
	clock := admissionNewClock()
	backend := &admissionBackendFake{}
	engine := admissionNewClocked(t, EventBudgetConfig{true, 100}, backend, clock)
	admissionWarm(t, engine, admissionTestAccount)
	clock.Add(20 * time.Second)
	entered := make(chan struct{}, 32)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	backend.setDecide(func(call admissionBackendCall) (RedisBudgetDecision, error) {
		entered <- struct{}{}
		select {
		case <-release:
			return RedisBudgetDecision{Allowed: true}, nil
		case <-call.context.Done():
			return RedisBudgetDecision{}, call.context.Err()
		}
	})
	results := make(chan AdmissionDecision, 32)
	for i := 0; i < 32; i++ {
		go func(index int) {
			results <- engine.Admit(context.Background(), admissionIndexedUser(index+100), admissionPolicy(EventClassProfile), time.Time{})
		}(i)
	}
	for i := 0; i < 32; i++ {
		admissionAwait(t, entered)
	}
	before, _ := backend.counts()
	assert.Equal(t, 33, before)
	// Synchronous completion while all 32 calls remain parked proves no queued
	// remote waiter or detached background request is needed for this decision.
	saturated := make(chan AdmissionDecision, 1)
	go func() {
		saturated <- engine.Admit(context.Background(), admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{})
	}()
	assert.Equal(t, admissionExpected(AdmissionAllow, AdmissionSourceWarmLocal, 0, false), admissionAwait(t, saturated))
	assert.Equal(t, admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false), engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{}))
	assert.Equal(t, admissionExpected(AdmissionCloseOverload, AdmissionSourceUnavailable, time.Second, false), engine.Admit(t.Context(), admissionIndexedUser(400), admissionPolicy(EventClassSubscription), time.Time{}))
	calls, _ := backend.counts()
	assert.Equal(t, before, calls)
	releaseOnce.Do(func() { close(release) })
	for i := 0; i < 32; i++ {
		assert.Equal(t, admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, false), admissionAwait(t, results))
	}
	// Released slots must be reusable; every registered call settled exactly once.
	assert.Equal(t, admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, false), engine.Admit(t.Context(), admissionIndexedUser(500), admissionPolicy(EventClassProfile), time.Time{}))
}
func TestEventAdmitterSweepBoundaryTouchAndRecreation(t *testing.T) {
	for _, idle := range []time.Duration{600*time.Second - time.Nanosecond, 600 * time.Second} {
		t.Run(fmt.Sprint(idle), func(t *testing.T) {
			clock := admissionNewClock()
			backend := &admissionBackendFake{}
			engine := admissionNewClocked(t, EventBudgetConfig{true, 100}, backend, clock)
			admissionWarm(t, engine, admissionTestAccount)
			clock.Add(idle)
			engine.sweepIdle(clock.Now())
			backend.setReply(RedisBudgetDecision{}, ErrAccountBudgetUnavailable)
			want := admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false)
			if idle < 600*time.Second {
				want = admissionExpected(AdmissionAllow, AdmissionSourceWarmLocal, 0, false)
			}
			assert.Equal(t, want, engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{}))
			if idle == 600*time.Second {
				backend.setReply(RedisBudgetDecision{Allowed: true}, nil)
				admissionWarm(t, engine, admissionTestAccount)
				backend.setReply(RedisBudgetDecision{}, ErrAccountBudgetUnavailable)
				assert.Equal(t, admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false), engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{}), "recreated record starts empty")
			}
		})
	}
	t.Run("unavailable attempt touches idle timestamp", func(t *testing.T) {
		clock := admissionNewClock()
		backend := &admissionBackendFake{}
		engine := admissionNewClocked(t, EventBudgetConfig{true, 100}, backend, clock)
		admissionWarm(t, engine, admissionTestAccount)
		clock.Add(599 * time.Second)
		backend.setReply(RedisBudgetDecision{}, ErrAccountBudgetUnavailable)
		require.Equal(t, admissionExpected(AdmissionAllow, AdmissionSourceWarmLocal, 0, false), engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{}))
		clock.Add(time.Second)
		engine.sweepIdle(clock.Now())
		assert.Equal(t, admissionExpected(AdmissionAllow, AdmissionSourceWarmLocal, 0, false), engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{}), "touch must prevent original 600s eviction")
	})
}
func TestEventAdmitterCloseJoinsSweepAndRegistrationRace(t *testing.T) {
	clock := admissionNewClock()
	backend := &admissionBackendFake{}
	engine := admissionNewClocked(t, EventBudgetConfig{true, 100}, backend, clock)
	require.NotNil(t, engine.sweepDone)
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan AdmissionDecision, 64)
	closers := make(chan error, 8)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- engine.Admit(context.Background(), admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{})
		}()
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; closers <- engine.Close() }()
	}
	close(start)
	settled := make(chan struct{})
	go func() { wg.Wait(); close(settled) }()
	admissionAwait(t, settled)
	close(results)
	close(closers)
	for decision := range results {
		assert.Contains(t, []AdmissionDecision{
			admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, false),
			admissionExpected(AdmissionAbandon, AdmissionSourceUnavailable, 0, false),
			admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false),
		}, decision)
	}
	for err := range closers {
		assert.NoError(t, err)
	}
	select {
	case <-engine.sweepDone:
	default:
		t.Fatal("Close returned before its sweep joined")
	}
	calls, closes := backend.counts()
	assert.Equal(t, 1, closes)
	assert.Equal(t, admissionExpected(AdmissionAbandon, AdmissionSourceUnavailable, 0, false), engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{}))
	finalCalls, _ := backend.counts()
	assert.Equal(t, calls, finalCalls)
	disabled := admissionNewClocked(t, EventBudgetConfig{false, 100}, nil, clock)
	assert.Nil(t, disabled.sweepDone, "disabled construction starts no sweep")
	assert.Nil(t, disabled.warm, "disabled construction allocates no warm map")
}

func TestEventAdmitterEnabledInvalidInputCannotReachBackend(t *testing.T) {
	backend := &admissionBackendFake{}
	engine := admissionNewPublic(t, EventBudgetConfig{true, 100}, backend)
	for _, policy := range []EventPolicy{
		{}, {Class: EventClassContent, Scope: BudgetScopeAccount},
		{Class: EventClassTyping, Scope: BudgetScopeLocalMaintenance},
		{Class: EventClassPresence, Scope: BudgetScopeLocalMaintenance},
		{Class: EventClassProfile, Scope: BudgetScopeAccount, MayHold: true},
		{Class: EventClassSubscription, Scope: BudgetScopeAccount},
		{Class: EventClassReady, Scope: BudgetScopeAccount},
		{Class: EventClassRenewal, Scope: BudgetScopeExistingContent},
	} {
		assert.Equal(t, admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false), engine.Admit(t.Context(), admissionTestAccount, policy, time.Time{}))
	}
	assert.Equal(t, admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false), engine.Admit(t.Context(), uuid.Nil, admissionPolicy(EventClassProfile), time.Time{}))
	calls, _ := backend.counts()
	assert.Zero(t, calls)
}
func TestEventAdmitterValidDenyAdviceBoundaries(t *testing.T) {
	for _, delay := range []time.Duration{time.Millisecond, 5 * time.Second, 120 * time.Second} {
		t.Run(fmt.Sprint(delay), func(t *testing.T) {
			backend := &admissionBackendFake{}
			backend.setReply(RedisBudgetDecision{RetryAfter: delay}, nil)
			engine := admissionNewPublic(t, EventBudgetConfig{true, 100}, backend)
			assert.Equal(t, admissionExpected(AdmissionRefuse, AdmissionSourceRedis, delay, true), engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(EventClassSubscription), time.Time{}))
		})
	}
}

func TestEventAdmitterDoesNotStartRemoteAfterWaitingPastRemoteDeadline(t *testing.T) {
	for _, warm := range []bool{false, true} {
		for _, elapsed := range []time.Duration{90 * time.Millisecond, 95 * time.Millisecond, 100 * time.Millisecond} {
			t.Run(fmt.Sprintf("warm%t_elapsed%d", warm, elapsed), func(t *testing.T) {
				clock := admissionNewClock()
				backend := &admissionBackendFake{}
				var observe atomic.Bool
				observed := make(chan time.Time, 1)
				now := func() time.Time {
					instant := clock.Now()
					if observe.Load() {
						select {
						case observed <- instant:
						default:
						}
					}
					return instant
				}
				engine, err := newEventAdmitterWithClock(EventBudgetConfig{true, 100}, backend, now)
				require.NoError(t, err)
				t.Cleanup(func() { assert.NoError(t, engine.Close()) })
				if warm {
					admissionWarm(t, engine, admissionTestAccount)
					clock.Add(20 * time.Second)
				}
				before, _ := backend.counts()
				engine.warmMu.Lock()
				var unlockOnce sync.Once
				t.Cleanup(func() { unlockOnce.Do(engine.warmMu.Unlock) })
				observe.Store(true)
				result := make(chan AdmissionDecision, 1)
				go func() {
					result <- engine.Admit(context.Background(), admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{})
				}()
				admissionAwait(t, observed)
				clock.Add(elapsed)
				unlockOnce.Do(engine.warmMu.Unlock)
				want := admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false)
				if warm && elapsed < 100*time.Millisecond {
					want = admissionExpected(AdmissionAllow, AdmissionSourceWarmLocal, 0, false)
				}
				assert.Equal(t, want, admissionAwait(t, result))
				calls, _ := backend.counts()
				assert.Equal(t, before, calls, "map contention cannot authorize a remote command at or after its 90ms deadline")
			})
		}
	}
}
func TestEventAdmitterRechecksDeadlineAfterPostReplyWarmAllocation(t *testing.T) {
	clock := admissionNewClock()
	backend := &admissionBackendFake{}
	var observe atomic.Bool
	observed := make(chan time.Time, 1)
	now := func() time.Time {
		instant := clock.Now()
		if observe.Load() {
			select {
			case observed <- instant:
			default:
			}
		}
		return instant
	}
	engine, err := newEventAdmitterWithClock(EventBudgetConfig{true, 100}, backend, now)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, engine.Close()) })
	var unlockOnce sync.Once
	t.Cleanup(func() {
		if observe.Load() {
			unlockOnce.Do(engine.warmMu.Unlock)
		}
	})
	backend.setDecide(func(admissionBackendCall) (RedisBudgetDecision, error) {
		engine.warmMu.Lock()
		observe.Store(true)
		return RedisBudgetDecision{Allowed: true}, nil
	})
	result := make(chan AdmissionDecision, 1)
	go func() {
		result <- engine.Admit(context.Background(), admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{})
	}()
	admissionAwait(t, observed) // initial post-reply clock snapshot is still timely
	clock.Add(100 * time.Millisecond)
	unlockOnce.Do(engine.warmMu.Unlock)
	assert.Equal(t, admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false), admissionAwait(t, result), "an earlier valid reply cannot carry allowance across allocation work")
	clock.Add(40 * time.Second)
	backend.setReply(RedisBudgetDecision{}, ErrAccountBudgetUnavailable)
	assert.Equal(t, admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false), engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{}), "late allocation must not leave a refillable record")
}
func TestEventAdmitterConcurrentSweepAndReservationsCannotDoubleSpend(t *testing.T) {
	clock := admissionNewClock()
	backend := &admissionBackendFake{}
	engine := admissionNewClocked(t, EventBudgetConfig{true, 100}, backend, clock)
	admissionWarm(t, engine, admissionTestAccount)
	clock.Add(600 * time.Second)
	backend.setReply(RedisBudgetDecision{}, ErrAccountBudgetUnavailable)
	start := make(chan struct{})
	results := make(chan AdmissionDecision, 64)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- engine.Admit(context.Background(), admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{})
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); <-start; engine.sweepIdle(clock.Now()) }()
	close(start)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	admissionAwait(t, done)
	close(results)
	allowances := 0
	for decision := range results {
		if decision.Action == AdmissionAllow {
			assert.Equal(t, admissionExpected(AdmissionAllow, AdmissionSourceWarmLocal, 0, false), decision)
			allowances++
		} else {
			assert.Equal(t, admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false), decision)
		}
	}
	assert.LessOrEqual(t, allowances, 2, "sweep/touch/reservation must share one serialized bucket state")
	clock.Add(600 * time.Second)
	engine.sweepIdle(clock.Now())
	backend.setReply(RedisBudgetDecision{Allowed: true}, nil)
	admissionWarm(t, engine, admissionTestAccount)
	backend.setReply(RedisBudgetDecision{}, ErrAccountBudgetUnavailable)
	assert.Equal(t, admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false), engine.Admit(t.Context(), admissionTestAccount, admissionPolicy(EventClassProfile), time.Time{}), "recreated state cannot inherit a detached bucket's credit")
}
