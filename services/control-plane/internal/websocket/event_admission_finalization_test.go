package websocket

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type finalizationRecord struct {
	class             EventClass
	allowed, degraded bool
}
type finalizationObserver struct {
	mu       sync.Mutex
	records  []finalizationRecord
	onRecord func()
}

func (o *finalizationObserver) recordFinal(class EventClass, allowed, degraded bool) {
	if o.onRecord != nil {
		o.onRecord()
	}
	o.mu.Lock()
	o.records = append(o.records, finalizationRecord{class, allowed, degraded})
	o.mu.Unlock()
}
func (o *finalizationObserver) snapshot() []finalizationRecord {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]finalizationRecord(nil), o.records...)
}
func finalizationUnavailable() AdmissionDecision {
	return admissionExpected(AdmissionAbandon, AdmissionSourceUnavailable, 0, false)
}
func TestEventAdmissionFinalizationCountsExactlyOnceAndSealsEvent(t *testing.T) {
	for _, tt := range []struct {
		name     string
		decision AdmissionDecision
		allowed  bool
	}{
		{"allowed", admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, false), true},
		{"refused", admissionExpected(AdmissionRefuse, AdmissionSourceRedis, 5*time.Second, true), false},
		{"overloaded", admissionExpected(AdmissionCloseOverload, AdmissionSourceUnavailable, time.Second, false), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := responseClient(t, true)
			observer := &finalizationObserver{}
			client.Hub.eventAdmissionObserver = observer
			state := eventAdmissionState{}
			decision := state.finalize(context.Background(), client, EventClassProfile, tt.decision)
			assert.Equal(t, tt.decision, decision)
			wantDegraded := tt.decision.Source == AdmissionSourceUnavailable
			assert.Equal(t, []finalizationRecord{{EventClassProfile, tt.allowed, wantDegraded}}, observer.snapshot())
			assert.True(t, state.finalized)
			assert.Equal(t, finalizationUnavailable(), state.finalize(context.Background(), client, EventClassProfile, tt.decision), "same event cannot produce another productive decision")
			state.observe(admissionExpected(AdmissionAllow, AdmissionSourceWarmLocal, 0, false))
			assert.Len(t, observer.snapshot(), 1)
			assert.Equal(t, wantDegraded, state.degraded, "observations after finalization cannot rewrite its evidence")
		})
	}
}
func TestEventAdmissionFinalizationDegradedEvidenceIsORedAcrossAttempts(t *testing.T) {
	for _, source := range []AdmissionSource{AdmissionSourceWarmLocal, AdmissionSourceUnavailable} {
		t.Run(string(rune(source+'0')), func(t *testing.T) {
			client := responseClient(t, true)
			observer := &finalizationObserver{}
			client.Hub.eventAdmissionObserver = observer
			state := eventAdmissionState{}
			evidence := admissionExpected(AdmissionRefuse, source, time.Second, false)
			if source == AdmissionSourceWarmLocal {
				evidence = admissionExpected(AdmissionAllow, source, 0, false)
			}
			state.observe(evidence)
			state.observe(admissionExpected(AdmissionRefuse, AdmissionSourceRedis, time.Millisecond, true))
			assert.Empty(t, observer.snapshot(), "intermediate evidence must not publish final counters")
			allow := admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, false)
			assert.Equal(t, allow, state.finalize(context.Background(), client, EventClassReady, allow))
			assert.Equal(t, []finalizationRecord{{EventClassReady, true, true}}, observer.snapshot())
		})
	}
}
func TestEventAdmissionFinalizationAllClosedClassesAndOrdinarySources(t *testing.T) {
	for _, class := range []EventClass{EventClassContent, EventClassTyping, EventClassSubscription, EventClassPresence, EventClassProfile, EventClassServer, EventClassReady, EventClassRenewal} {
		for _, source := range []AdmissionSource{AdmissionSourceExisting, AdmissionSourceLocalMaintenance, AdmissionSourceRedis} {
			client := responseClient(t, true)
			observer := &finalizationObserver{}
			client.Hub.eventAdmissionObserver = observer
			state := eventAdmissionState{}
			allow := admissionExpected(AdmissionAllow, source, 0, false)
			assert.Equal(t, allow, state.finalize(context.Background(), client, class, allow))
			assert.Equal(t, []finalizationRecord{{class, true, false}}, observer.snapshot())
		}
	}
}
func TestEventAdmissionFinalizationDisabledAndNilObserverRemainInert(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		client := responseClient(t, false)
		observer := &finalizationObserver{}
		client.Hub.eventAdmissionObserver = observer
		state := eventAdmissionState{}
		allow := admissionExpected(AdmissionAllow, AdmissionSourceExisting, 0, false)
		assert.Equal(t, allow, state.finalize(context.Background(), client, EventClassProfile, allow))
		assert.Empty(t, observer.snapshot())
		assert.False(t, state.finalized)
		assert.False(t, state.degraded)
	})
	t.Run("optional observer absent", func(t *testing.T) {
		client := responseClient(t, true)
		state := eventAdmissionState{}
		deny := admissionExpected(AdmissionRefuse, AdmissionSourceRedis, 5*time.Second, true)
		assert.Equal(t, deny, state.finalize(context.Background(), client, EventClassProfile, deny))
		assert.True(t, state.finalized, "nil publication is not permission to bypass finalization")
	})
}
func TestEventAdmissionFinalizationAbandonAndInvalidEnumsNeverCount(t *testing.T) {
	for _, tt := range []struct {
		name     string
		class    EventClass
		decision AdmissionDecision
	}{
		{"abandon", EventClassProfile, admissionExpected(AdmissionAbandon, AdmissionSourceUnavailable, 0, false)},
		{"zero action", EventClassProfile, AdmissionDecision{Source: AdmissionSourceRedis}},
		{"unknown action", EventClassProfile, AdmissionDecision{Action: 255, Source: AdmissionSourceRedis}},
		{"zero source", EventClassProfile, AdmissionDecision{Action: AdmissionAllow}},
		{"unknown source", EventClassProfile, AdmissionDecision{Action: AdmissionAllow, Source: 255}},
		{"zero class", 0, admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, false)},
		{"unknown class", 255, admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, false)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := responseClient(t, true)
			observer := &finalizationObserver{}
			client.Hub.eventAdmissionObserver = observer
			state := eventAdmissionState{}
			assert.Equal(t, finalizationUnavailable(), state.finalize(context.Background(), client, tt.class, tt.decision))
			assert.Empty(t, observer.snapshot())
			assert.True(t, state.finalized)
			assert.Equal(t, finalizationUnavailable(), state.finalize(context.Background(), client, EventClassProfile, admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, false)), "abandoned event cannot be revived")
			assert.Empty(t, observer.snapshot())
		})
	}
}
func TestEventAdmissionFinalizationChecksActualAuthorityBeforeRecording(t *testing.T) {
	for _, cause := range []string{"parent canceled", "client canceled", "Hub canceled", "security terminal", "current deadline expired"} {
		t.Run(cause, func(t *testing.T) {
			client := responseClient(t, true)
			observer := &finalizationObserver{}
			client.Hub.eventAdmissionObserver = observer
			authority, finish := client.ingressEventContext()
			defer finish()
			parent, cancel := context.WithCancel(authority)
			defer cancel()
			switch cause {
			case "parent canceled":
				cancel()
			case "client canceled":
				client.cancelIngressLifecycle()
			case "Hub canceled":
				client.Hub.ingressCancel()
			case "security terminal":
				client.securityTerminal.Store(true)
			case "current deadline expired":
				client.deadlineMu.Lock()
				client.TokenExpiresAt = time.Now().Add(-time.Second)
				client.deadlineMu.Unlock()
				require.NoError(t, parent.Err(), "fixture distinguishes actual current deadline from context notification")
			}
			state := eventAdmissionState{}
			state.observe(admissionExpected(AdmissionAllow, AdmissionSourceWarmLocal, 0, false))
			assert.Equal(t, finalizationUnavailable(), state.finalize(parent, client, EventClassProfile, admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, false)))
			assert.Empty(t, observer.snapshot(), "pre-finalization abandonment emits none of allowed/throttled/degraded")
		})
	}
}
func TestEventAdmissionFinalizationObserverRunsOutsideAuthorityAndSendLocks(t *testing.T) {
	client := responseClient(t, true)
	observer := &finalizationObserver{onRecord: func() {
		if client.deadlineMu.TryLock() {
			client.deadlineMu.Unlock()
		} else {
			t.Error("observer called under deadline lock")
		}
		if client.sendMu.TryLock() {
			client.sendMu.Unlock()
		} else {
			t.Error("observer called under send lock")
		}
		if client.Hub.mu.TryLock() {
			client.Hub.mu.Unlock()
		} else {
			t.Error("observer called under Hub map lock")
		}
	}}
	client.Hub.eventAdmissionObserver = observer
	state := eventAdmissionState{}
	decision := admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, false)
	assert.Equal(t, decision, state.finalize(context.Background(), client, EventClassContent, decision))
	assert.Equal(t, []finalizationRecord{{EventClassContent, true, false}}, observer.snapshot())
}

func TestEventAdmissionFinalizationCancellationDuringDeadlineSnapshotDoesNotCount(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel%t", canceled), func(t *testing.T) {
			client := responseClient(t, true)
			observer := &finalizationObserver{}
			client.Hub.eventAdmissionObserver = observer
			parent, cancel := context.WithCancel(client.ingressCtx)
			defer cancel()
			ctx := &responseSnapshotContext{Context: parent, checked: make(chan struct{})}
			ctx.armed.Store(true)
			client.deadlineMu.Lock()
			var unlockOnce sync.Once
			t.Cleanup(func() { unlockOnce.Do(client.deadlineMu.Unlock) })
			state := eventAdmissionState{}
			allow := admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, false)
			result := make(chan AdmissionDecision, 1)
			go func() { result <- state.finalize(ctx, client, EventClassProfile, allow) }()
			lifecycleAwait(t, ctx.checked, "finalization passes initial cancellation snapshot")
			if canceled {
				cancel()
			}
			unlockOnce.Do(client.deadlineMu.Unlock)
			select {
			case decision := <-result:
				if canceled {
					assert.Equal(t, finalizationUnavailable(), decision)
					assert.Empty(t, observer.snapshot())
				} else {
					assert.Equal(t, allow, decision)
					assert.Equal(t, []finalizationRecord{{EventClassProfile, true, false}}, observer.snapshot())
				}
			case <-time.After(time.Second):
				t.Fatal("finalization authority snapshot did not settle")
			}
			lifecycleRequireActive(client.ingressCtx, t)
		})
	}
}
