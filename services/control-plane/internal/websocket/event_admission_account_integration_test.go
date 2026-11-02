package websocket

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The production backend remains the decision owner. This decorator observes
// only the lifecycle boundary, without replacing commands, replies or errors.
type budgetTransportOwnedBackend struct {
	AccountBudgetBackend
	closes atomic.Int32
}

func (b *budgetTransportOwnedBackend) Close() error {
	b.closes.Add(1)
	return b.AccountBudgetBackend.Close()
}
func budgetTransportAssertNoWarmUsers(t *testing.T, engine EventAdmitter) {
	t.Helper()
	concrete, ok := engine.(*accountEventAdmitter)
	require.True(t, ok)
	concrete.warmMu.Lock()
	count := len(concrete.warm)
	concrete.warmMu.Unlock()
	assert.Zero(t, count, "uncertain transport execution must not allocate outage credit")
}
func TestEventAdmitterTransportIntegrationColdLossFailsUnavailable(t *testing.T) {
	for _, tt := range []struct {
		class           EventClass
		interval, burst int64
		action          AdmissionAction
	}{
		{EventClassProfile, 5000000, 8, AdmissionRefuse},
		{EventClassReady, 1000000, 16, AdmissionCloseOverload},
	} {
		t.Run(tt.class.String(), func(t *testing.T) {
			inspector, proxy, client := budgetTransportFixture(t)
			t.Log("WS_EVENT_BUDGET_TRANSPORT START")
			user, key := budgetTransportIdentity(t, inspector, tt.class)
			realBackend, err := NewRedisAccountBudgetBackend(EventBudgetConfig{true, 100}, client)
			require.NoError(t, err)
			backend := &budgetTransportOwnedBackend{AccountBudgetBackend: realBackend}
			engine, err := NewEventAdmitter(EventBudgetConfig{true, 100}, backend)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, engine.Close()) })
			captured := proxy.ArmDrop()
			decision := engine.Admit(t.Context(), user, admissionPolicy(tt.class), time.Time{})
			admissionAwait(t, captured)
			assert.Equal(t, admissionExpected(tt.action, AdmissionSourceUnavailable, time.Second, false), decision)
			require.Equal(t, 1, proxy.ScriptCalls())
			last, tat := budgetTransportTimes(t, inspector, key)
			assert.Equal(t, tt.interval, tat-last)
			budgetTransportAssertNoWarmUsers(t, engine)
			require.NoError(t, engine.Close())
			require.NoError(t, engine.Close())
			assert.Equal(t, int32(1), backend.closes.Load())
			require.NoError(t, inspector.Ping(t.Context()).Err())
			budgetTransportNoReplayAfterClose(t, proxy, 1)
			budgetTransportPass(t)
		})
	}
}
func TestEventAdmitterTransportIntegrationWarmCreditIsSpentOnce(t *testing.T) {
	inspector, proxy, client := budgetTransportFixture(t)
	t.Log("WS_EVENT_BUDGET_TRANSPORT START")
	t.Log("fallback refill uses the private synthetic elapsed clock; this vector does not establish real-time decision latency")
	user, key := budgetTransportIdentity(t, inspector, EventClassProfile)
	realBackend, err := NewRedisAccountBudgetBackend(EventBudgetConfig{true, 100}, client)
	require.NoError(t, err)
	backend := &budgetTransportOwnedBackend{AccountBudgetBackend: realBackend}
	clock := &admissionClock{now: time.Now()}
	engine := admissionNewClocked(t, EventBudgetConfig{true, 100}, backend, clock)
	admissionWarm(t, engine, user)
	require.Equal(t, 1, proxy.ScriptCalls())
	_, firstTat := budgetTransportTimes(t, inspector, key)
	clock.Add(20 * time.Second)
	captured := proxy.ArmDrop()
	decision := engine.Admit(t.Context(), user, admissionPolicy(EventClassProfile), time.Time{})
	admissionAwait(t, captured)
	assert.Equal(t, admissionExpected(AdmissionAllow, AdmissionSourceWarmLocal, 0, false), decision)
	require.Equal(t, 2, proxy.ScriptCalls())
	_, secondTat := budgetTransportTimes(t, inspector, key)
	assert.Equal(t, firstTat+5000000, secondTat, "lost reply still debits the real server once")
	// Fail a second actual socket exchange at the same synthetic elapsed time.
	// The prior reserved token must not be refunded or reused.
	captured = proxy.ArmDrop()
	decision = engine.Admit(t.Context(), user, admissionPolicy(EventClassProfile), time.Time{})
	admissionAwait(t, captured)
	assert.Equal(t, admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false), decision)
	require.Equal(t, 3, proxy.ScriptCalls())
	_, thirdTat := budgetTransportTimes(t, inspector, key)
	assert.Equal(t, secondTat+5000000, thirdTat)
	require.NoError(t, engine.Close())
	assert.Equal(t, int32(1), backend.closes.Load())
	require.NoError(t, inspector.Ping(t.Context()).Err())
	budgetTransportNoReplayAfterClose(t, proxy, 3)
	budgetTransportPass(t)
}
func TestEventAdmitterTransportIntegrationHeldReplyCannotReviveCanceledAuthority(t *testing.T) {
	for _, cause := range []string{"parent cancellation", "engine Close"} {
		t.Run(cause, func(t *testing.T) {
			inspector, proxy, client := budgetTransportFixture(t)
			t.Log("WS_EVENT_BUDGET_TRANSPORT START")
			user, key := budgetTransportIdentity(t, inspector, EventClassProfile)
			realBackend, err := NewRedisAccountBudgetBackend(EventBudgetConfig{true, 100}, client)
			require.NoError(t, err)
			backend := &budgetTransportOwnedBackend{AccountBudgetBackend: realBackend}
			engine, err := NewEventAdmitter(EventBudgetConfig{true, 100}, backend)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, engine.Close()) })
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			captured := proxy.ArmHold()
			result := make(chan AdmissionDecision, 1)
			go func() { result <- engine.Admit(parent, user, admissionPolicy(EventClassProfile), time.Time{}) }()
			admissionAwait(t, captured)
			require.Equal(t, 1, proxy.ScriptCalls())
			last, tat := budgetTransportTimes(t, inspector, key)
			assert.Equal(t, int64(5000000), tat-last)
			var closing chan error
			if cause == "parent cancellation" {
				cancel()
			} else {
				closing = make(chan error, 1)
				go func() { closing <- engine.Close() }()
			}
			// Keep the successful reply withheld until cancellation/shutdown has
			// settled. Harness wait bounds diagnose fixture deadlocks, not an SLO.
			assert.Equal(t, admissionExpected(AdmissionAbandon, AdmissionSourceUnavailable, 0, false), admissionAwait(t, result))
			if closing != nil {
				require.NoError(t, admissionAwait(t, closing))
			} else {
				require.NoError(t, engine.Close())
			}
			budgetTransportAssertNoWarmUsers(t, engine)
			assert.Equal(t, int32(1), backend.closes.Load())
			proxy.Release()
			assert.Equal(t, admissionExpected(AdmissionAbandon, AdmissionSourceUnavailable, 0, false), engine.Admit(t.Context(), user, admissionPolicy(EventClassProfile), time.Time{}))
			require.NoError(t, inspector.Ping(t.Context()).Err(), "client shutdown must leave independent Redis access alive")
			budgetTransportNoReplayAfterClose(t, proxy, 1)
			_, afterTat := budgetTransportTimes(t, inspector, key)
			assert.Equal(t, tat, afterTat)
			budgetTransportPass(t)
		})
	}
}
