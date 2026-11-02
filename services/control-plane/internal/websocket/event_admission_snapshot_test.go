package websocket

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type snapshotScalars struct {
	inflight int64
	warm     int
	misses   uint64
}

func snapshotRead(h *Hub) snapshotScalars {
	inflight, warm, misses := h.EventBudgetSnapshot()
	return snapshotScalars{inflight, warm, misses}
}

type snapshotProviderSpy struct {
	lifecycleBorrowedAdmitter
	reads atomic.Int32
}

func (a *snapshotProviderSpy) eventBudgetSnapshot() (int64, int, uint64) {
	a.reads.Add(1)
	return 7, 19, 41
}

func snapshotHub(t *testing.T, enabled bool, engine EventAdmitter) *Hub {
	t.Helper()
	h, err := NewHubWithEventAdmission(nil, nil, nil, EventBudgetConfig{Enabled: enabled, ScalePercent: 100}, engine)
	require.NoError(t, err)
	t.Cleanup(h.ingressCancel)
	return h
}

func TestEventBudgetSnapshotHubOptionalProviderAndDisabledOwnership(t *testing.T) {
	var absent *Hub
	assert.Equal(t, snapshotScalars{}, snapshotRead(absent))
	assert.Equal(t, snapshotScalars{}, snapshotRead(snapshotHub(t, false, nil)))
	// An enabled nil engine is not constructible; the accessor still handles
	// a zero-value/partial Hub without dereferencing it.
	assert.Equal(t, snapshotScalars{}, snapshotRead(&Hub{eventBudgetConfig: EventBudgetConfig{Enabled: true, ScalePercent: 100}}))
	plain := &lifecycleBorrowedAdmitter{}
	assert.Equal(t, snapshotScalars{}, snapshotRead(snapshotHub(t, true, plain)))
	assert.Zero(t, plain.calls.Load())
	assert.Zero(t, plain.closes.Load())
	spy := &snapshotProviderSpy{}
	disabled := snapshotHub(t, false, spy)
	assert.Equal(t, snapshotScalars{}, snapshotRead(disabled))
	assert.Zero(t, spy.reads.Load(), "disabled Hub must not invoke an optional provider")
	enabled := snapshotHub(t, true, spy)
	assert.Equal(t, snapshotScalars{7, 19, 41}, snapshotRead(enabled))
	assert.Equal(t, int32(1), spy.reads.Load())
	assert.Zero(t, spy.calls.Load())
	assert.Zero(t, spy.closes.Load(), "scalar observation never owns Close")
	var typedNil *accountEventAdmitter
	inflight, warm, misses := typedNil.eventBudgetSnapshot()
	assert.Equal(t, snapshotScalars{}, snapshotScalars{inflight, warm, misses})
	assert.Equal(t, snapshotScalars{}, snapshotRead(snapshotHub(t, true, typedNil)))
}

// The future origin keeps the real backend context deadline out of this test's
// synchronization path. This is an instance clock, never a package-global clock.
func snapshotEngine(t *testing.T, backend AccountBudgetBackend) (*accountEventAdmitter, *atomic.Int64, *atomic.Int64, time.Time) {
	t.Helper()
	origin := time.Now().Add(time.Hour)
	elapsed, reads := &atomic.Int64{}, &atomic.Int64{}
	a, err := newEventAdmitterWithClock(EventBudgetConfig{Enabled: true, ScalePercent: 100}, backend, func() time.Time {
		reads.Add(1)
		return origin.Add(time.Duration(elapsed.Load()))
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, a.Close()) })
	return a, elapsed, reads, origin
}

func TestEventBudgetSnapshotDisabledEngineDoesNoWork(t *testing.T) {
	a := admissionNewPublic(t, EventBudgetConfig{Enabled: false, ScalePercent: 100}, nil).(*accountEventAdmitter)
	// Poison unused dependencies before publication. Disabled observation must
	// not call a clock or try to lock lifecycle/warm state.
	a.now = func() time.Time { panic("disabled snapshot called clock") }
	a.lifecycleMu.Lock()
	a.warmMu.Lock()
	done := make(chan snapshotScalars, 1)
	go func() {
		i, w, m := a.eventBudgetSnapshot()
		done <- snapshotScalars{i, w, m}
	}()
	select {
	case got := <-done:
		assert.Equal(t, snapshotScalars{}, got)
	case <-time.After(3 * time.Second):
		t.Error("disabled snapshot acquired a state lock")
	}
	a.warmMu.Unlock()
	a.lifecycleMu.Unlock()
}

func TestEventBudgetSnapshotBlockedRemoteSlotsAndCancellation(t *testing.T) {
	entered := make(chan struct{}, accountRemoteSlots)
	backend := &admissionBackendFake{}
	backend.setDecide(func(call admissionBackendCall) (RedisBudgetDecision, error) {
		entered <- struct{}{}
		<-call.context.Done()
		return RedisBudgetDecision{}, call.context.Err()
	})
	a, _, clockReads, _ := snapshotEngine(t, backend)
	h := snapshotHub(t, true, a)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	results := make(chan AdmissionDecision, accountRemoteSlots)
	for range accountRemoteSlots {
		go func() { results <- a.Admit(ctx, uuid.New(), admissionPolicy(EventClassProfile), time.Time{}) }()
	}
	for range accountRemoteSlots {
		admissionAwait(t, entered)
	}
	before := clockReads.Load()
	for range 20 {
		assert.Equal(t, snapshotScalars{accountRemoteSlots, 0, 0}, snapshotRead(h))
	}
	assert.Equal(t, before, clockReads.Load(), "snapshot must not read the clock")
	calls, closes := backend.counts()
	assert.Equal(t, accountRemoteSlots, calls, "snapshot must not acquire another backend decision")
	assert.Zero(t, closes)
	// Slot saturation is not a full-warm-map miss.
	assert.Equal(t, AdmissionSourceUnavailable, a.Admit(ctx, uuid.New(), admissionPolicy(EventClassProfile), time.Time{}).Source)
	assert.Equal(t, snapshotScalars{accountRemoteSlots, 0, 0}, snapshotRead(h))
	cancel()
	for range accountRemoteSlots {
		assert.Equal(t, AdmissionAbandon, admissionAwait(t, results).Action)
	}
	assert.Equal(t, snapshotScalars{}, snapshotRead(h))
}

func TestEventBudgetSnapshotWarmCapacityReplySemanticsAndRetainedClose(t *testing.T) {
	backend := &admissionBackendFake{}
	a, elapsed, clockReads, _ := snapshotEngine(t, backend)
	h := snapshotHub(t, true, a)
	tracked := uuid.New()
	assert.Equal(t, AdmissionAllow, a.Admit(t.Context(), tracked, admissionPolicy(EventClassProfile), time.Time{}).Action)
	assert.Equal(t, snapshotScalars{0, 1, 0}, snapshotRead(h))
	backend.setReply(RedisBudgetDecision{RetryAfter: time.Second}, nil)
	denial := a.Admit(t.Context(), uuid.New(), admissionPolicy(EventClassProfile), time.Time{})
	require.True(t, denial.DefinitiveRateDeny)
	assert.Equal(t, snapshotScalars{0, 2, 0}, snapshotRead(h))
	backend.setReply(RedisBudgetDecision{}, ErrAccountBudgetUnavailable)
	a.Admit(t.Context(), uuid.New(), admissionPolicy(EventClassProfile), time.Time{})
	assert.Equal(t, snapshotScalars{0, 2, 0}, snapshotRead(h))
	backend.setReply(RedisBudgetDecision{Allowed: true}, nil)
	for range accountWarmCapacity - 2 {
		require.Equal(t, AdmissionAllow, a.Admit(t.Context(), uuid.New(), admissionPolicy(EventClassProfile), time.Time{}).Action)
	}
	require.Equal(t, snapshotScalars{0, accountWarmCapacity, 0}, snapshotRead(h))
	a.Admit(t.Context(), uuid.New(), admissionPolicy(EventClassProfile), time.Time{})
	assert.Equal(t, snapshotScalars{0, accountWarmCapacity, 1}, snapshotRead(h))
	backend.setReply(RedisBudgetDecision{RetryAfter: time.Second}, nil)
	a.Admit(t.Context(), uuid.New(), admissionPolicy(EventClassProfile), time.Time{})
	assert.Equal(t, snapshotScalars{0, accountWarmCapacity, 2}, snapshotRead(h))
	a.Admit(t.Context(), tracked, admissionPolicy(EventClassProfile), time.Time{})
	assert.Equal(t, snapshotScalars{0, accountWarmCapacity, 2}, snapshotRead(h), "tracked reply does not miss insertion")
	backend.setReply(RedisBudgetDecision{}, ErrAccountBudgetUnavailable)
	a.Admit(t.Context(), uuid.New(), admissionPolicy(EventClassProfile), time.Time{})
	assert.Equal(t, snapshotScalars{0, accountWarmCapacity, 2}, snapshotRead(h), "uncertain reply cannot count a capacity miss")
	// Merely observing expired residents must not sweep them or read a clock.
	elapsed.Store(int64(accountWarmIdleLifetime + time.Second))
	before := clockReads.Load()
	callsBefore, _ := backend.counts()
	assert.Equal(t, snapshotScalars{0, accountWarmCapacity, 2}, snapshotRead(h))
	assert.Equal(t, before, clockReads.Load())
	require.NoError(t, a.Close())
	assert.Equal(t, snapshotScalars{0, accountWarmCapacity, 2}, snapshotRead(h), "Close retains resident state and cumulative misses")
	callsAfter, closes := backend.counts()
	assert.Equal(t, callsBefore, callsAfter)
	assert.Equal(t, 1, closes)
}

func TestEventBudgetSnapshotActualSweepAndLifecycleLockIndependence(t *testing.T) {
	a, _, reads, origin := snapshotEngine(t, &admissionBackendFake{})
	h := snapshotHub(t, true, a)
	a.Admit(t.Context(), uuid.New(), admissionPolicy(EventClassTyping), time.Time{})
	require.Equal(t, snapshotScalars{0, 1, 0}, snapshotRead(h))
	a.sweepIdle(origin.Add(accountWarmIdleLifetime - time.Nanosecond))
	assert.Equal(t, snapshotScalars{0, 1, 0}, snapshotRead(h))
	a.sweepIdle(origin.Add(accountWarmIdleLifetime))
	assert.Equal(t, snapshotScalars{}, snapshotRead(h))
	before := reads.Load()
	a.lifecycleMu.Lock()
	done := make(chan snapshotScalars, 1)
	go func() { done <- snapshotRead(h) }()
	select {
	case got := <-done:
		assert.Equal(t, snapshotScalars{}, got)
	case <-time.After(3 * time.Second):
		t.Error("snapshot blocked behind lifecycleMu")
	}
	a.lifecycleMu.Unlock()
	assert.Equal(t, before, reads.Load())
}

func TestEventBudgetSnapshotConcurrentAdmissionSweepAndClose(t *testing.T) {
	backend := &admissionBackendFake{}
	a, _, _, origin := snapshotEngine(t, backend)
	h := snapshotHub(t, true, a)
	var workers sync.WaitGroup
	var invalid atomic.Bool
	start := make(chan struct{})
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for range 100 {
				a.Admit(t.Context(), uuid.New(), admissionPolicy(EventClassTyping), time.Time{})
				got := snapshotRead(h)
				if got.inflight < 0 || got.inflight > accountRemoteSlots || got.warm < 0 || got.warm > accountWarmCapacity || got.misses != 0 {
					invalid.Store(true)
				}
			}
		}()
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		<-start
		for range 100 {
			a.sweepIdle(origin.Add(accountWarmIdleLifetime))
		}
	}()
	closed := make(chan error, 2)
	for range 2 {
		go func() { <-start; closed <- a.Close() }()
	}
	close(start)
	finished := make(chan struct{})
	go func() { workers.Wait(); close(finished) }()
	admissionAwait(t, finished)
	for range 2 {
		require.NoError(t, admissionAwait(t, closed))
	}
	assert.False(t, invalid.Load(), "independent scalar reads must stay within each field's bounds")
	assert.Zero(t, snapshotRead(h).inflight)
	_, closes := backend.counts()
	assert.Equal(t, 1, closes)
}

func TestEventBudgetSnapshotWhileCloseJoinsOccupiedRemote(t *testing.T) {
	backend := &admissionBackendFake{}
	a, _, _, _ := snapshotEngine(t, backend)
	h := snapshotHub(t, true, a)
	require.Equal(t, AdmissionAllow, a.Admit(t.Context(), uuid.New(), admissionPolicy(EventClassProfile), time.Time{}).Action)
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock) // Release before the engine cleanup if an assertion fails.
	backend.setDecide(func(call admissionBackendCall) (RedisBudgetDecision, error) {
		close(entered)
		<-call.context.Done()
		close(canceled)
		<-release
		return RedisBudgetDecision{}, call.context.Err()
	})
	result := make(chan AdmissionDecision, 1)
	go func() { result <- a.Admit(t.Context(), uuid.New(), admissionPolicy(EventClassProfile), time.Time{}) }()
	admissionAwait(t, entered)
	closed := make(chan error, 1)
	go func() { closed <- a.Close() }()
	admissionAwait(t, canceled)
	assert.Equal(t, snapshotScalars{1, 1, 0}, snapshotRead(h), "stopping is not completed slot teardown or resident eviction")
	select {
	case <-closed:
		t.Error("Close returned before its backend call joined")
	default:
	}
	unblock()
	assert.Equal(t, AdmissionAbandon, admissionAwait(t, result).Action)
	require.NoError(t, admissionAwait(t, closed))
	assert.Equal(t, snapshotScalars{0, 1, 0}, snapshotRead(h))
	calls, closes := backend.counts()
	assert.Equal(t, 2, calls)
	assert.Equal(t, 1, closes)
}
