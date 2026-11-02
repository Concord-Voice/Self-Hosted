package websocket

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type renewalAdmissionValidator struct {
	mu    sync.Mutex
	calls int
	run   func(context.Context, int) (accessTokenAuth, error)
}

func (v *renewalAdmissionValidator) validateAccessToken(ctx context.Context, _ string) (accessTokenAuth, error) {
	v.mu.Lock()
	v.calls++
	index := v.calls
	run := v.run
	v.mu.Unlock()
	return run(ctx, index)
}
func (v *renewalAdmissionValidator) count() int { v.mu.Lock(); defer v.mu.Unlock(); return v.calls }
func renewalAdmissionClient(t *testing.T, enabled bool) (*Client, *c2Engine, *finalizationObserver, *renewalAdmissionValidator, time.Time) {
	t.Helper()
	client, engine, _, observer := c2Client(t, enabled)
	server, _ := renewalSocketPair(t)
	client.Conn = server
	client.FamilyID = uuid.New()
	client.SessionID = uuid.NewString()
	client.CredEpoch = "renewal-fixture-epoch"
	old := client.tokenDeadline()
	next := old.Add(time.Hour)
	verified := accessTokenAuth{UserID: client.UserID, FamilyID: client.FamilyID, SessionID: client.SessionID, CredEpoch: client.CredEpoch, ExpiresAt: next}
	validator := &renewalAdmissionValidator{run: func(context.Context, int) (accessTokenAuth, error) { return verified, nil }}
	client.renewAuthority = validator
	return client, engine, observer, validator, next
}
func TestTokenRenewalAdmissionLocalGuardRefusalNeverCallsAccountOrValidator(t *testing.T) {
	client, engine, observer, validator, _ := renewalAdmissionClient(t, true)
	old := client.tokenDeadline()
	client.lastRenewalAttempt = time.Now()
	require.True(t, client.dispatchFrame([]byte(`{"type":"auth_renew","data":{"token":"fixture-token"}}`)))
	assert.Empty(t, engine.snapshot())
	assert.Zero(t, validator.count())
	assert.Equal(t, old, client.tokenDeadline())
	assert.Equal(t, []finalizationRecord{{EventClassRenewal, false, false}}, observer.snapshot())
	require.Len(t, client.Send, 1)
	assert.JSONEq(t, string(authRenewRejectedFrame), string(<-client.Send))
	assert.Empty(t, client.Hub.incoming)
}
func TestTokenRenewalAdmissionPassedGuardCallsAccountOnceAndValidatesOncePerPhase(t *testing.T) {
	client, engine, observer, validator, next := renewalAdmissionClient(t, true)
	require.True(t, client.dispatchFrame([]byte(`{"type":"auth_renew","data":{"token":"fixture-token"}}`)))
	calls := engine.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, admissionPolicy(EventClassRenewal), calls[0].policy)
	assert.True(t, calls[0].hold.IsZero())
	assert.Equal(t, 2, validator.count(), "initial and final authority checks each run once without a second local claim")
	assert.Equal(t, next, client.tokenDeadline())
	require.Len(t, client.Send, 1)
	assert.JSONEq(t, string(authRenewAcceptedFrame), string(<-client.Send))
	assert.Equal(t, []finalizationRecord{{EventClassRenewal, true, false}}, observer.snapshot())
	assert.Empty(t, client.Hub.incoming)
}
func TestTokenRenewalAdmissionRefusalAndAbandonDoNotReachValidator(t *testing.T) {
	for _, decision := range []AdmissionDecision{
		c2Deny(5 * time.Second), admissionExpected(AdmissionRefuse, AdmissionSourceUnavailable, time.Second, false), finalizationUnavailable(),
	} {
		t.Run(fmt.Sprintf("action%d_source%d", decision.Action, decision.Source), func(t *testing.T) {
			client, engine, observer, validator, _ := renewalAdmissionClient(t, true)
			old := client.tokenDeadline()
			engine.run = func(int, c2EngineCall) AdmissionDecision { return decision }
			continued := client.dispatchFrame([]byte(`{"type":"auth_renew","data":{"token":"fixture-token"}}`))
			assert.Equal(t, decision.Action != AdmissionAbandon, continued)
			assert.Len(t, engine.snapshot(), 1)
			assert.Zero(t, validator.count())
			assert.Equal(t, old, client.tokenDeadline())
			if decision.Action == AdmissionAbandon {
				assert.Empty(t, client.Send)
				assert.Empty(t, observer.snapshot())
			} else {
				require.Len(t, client.Send, 1)
				assert.JSONEq(t, string(authRenewRejectedFrame), string(<-client.Send))
				assert.Equal(t, []finalizationRecord{{EventClassRenewal, false, decision.Source == AdmissionSourceUnavailable}}, observer.snapshot())
			}
		})
	}
}
func TestTokenRenewalAdmissionAllowedButInvalidTokenRetainsAllowedMetric(t *testing.T) {
	client, engine, observer, validator, _ := renewalAdmissionClient(t, true)
	old := client.tokenDeadline()
	validator.run = func(context.Context, int) (accessTokenAuth, error) {
		return accessTokenAuth{}, errors.New("synthetic invalid credential")
	}
	require.True(t, client.dispatchFrame([]byte(`{"type":"auth_renew","data":{"token":"invalid-fixture-token"}}`)))
	assert.Len(t, engine.snapshot(), 1)
	assert.Equal(t, 1, validator.count())
	assert.Equal(t, old, client.tokenDeadline())
	assert.Equal(t, []finalizationRecord{{EventClassRenewal, true, false}}, observer.snapshot())
	require.Len(t, client.Send, 1)
	assert.JSONEq(t, string(authRenewRejectedFrame), string(<-client.Send))
}
func TestTokenRenewalAdmissionDisabledPreservesShortCircuitsAndNoNewAccounting(t *testing.T) {
	for _, kind := range []string{"empty token", "nil validator", "valid token"} {
		t.Run(kind, func(t *testing.T) {
			client, engine, observer, validator, next := renewalAdmissionClient(t, false)
			token := "fixture-token"
			if kind == "empty token" {
				token = ""
			}
			if kind == "nil validator" {
				client.renewAuthority = nil
			}
			require.True(t, client.dispatchFrame([]byte(fmt.Sprintf(`{"type":"auth_renew","data":{"token":%q}}`, token))))
			assert.Empty(t, engine.snapshot())
			assert.Empty(t, observer.snapshot())
			assert.Nil(t, client.eventHeartbeatBudget)
			require.Len(t, client.Send, 1)
			if kind == "valid token" {
				assert.Equal(t, 2, validator.count())
				assert.Equal(t, next, client.tokenDeadline())
				assert.JSONEq(t, string(authRenewAcceptedFrame), string(<-client.Send))
			} else {
				assert.Zero(t, validator.count())
				assert.True(t, client.lastRenewalAttempt.IsZero(), "disabled short circuit must not consume local guard")
				assert.JSONEq(t, string(authRenewRejectedFrame), string(<-client.Send))
			}
		})
	}
}
func TestTokenRenewalAdmissionFinalSuccessfulValidatorCannotReviveCanceledAuthority(t *testing.T) {
	client, engine, observer, validator, next := renewalAdmissionClient(t, true)
	old := client.tokenDeadline()
	authority, finish := client.ingressEventContext()
	defer finish()
	parent, cancel := context.WithCancel(authority)
	defer cancel()
	verified := accessTokenAuth{UserID: client.UserID, FamilyID: client.FamilyID, SessionID: client.SessionID, CredEpoch: client.CredEpoch, ExpiresAt: next}
	validator.run = func(_ context.Context, index int) (accessTokenAuth, error) {
		if index == 2 {
			cancel()
		}
		return verified, nil
	}
	continued := client.handleTokenRenewal(parent, IncomingMessage{Type: authRenewMessageType, Data: map[string]interface{}{"token": "fixture-token"}})
	assert.False(t, continued)
	assert.Equal(t, 2, validator.count())
	assert.Len(t, engine.snapshot(), 1)
	assert.Equal(t, old, client.tokenDeadline())
	assert.Empty(t, client.Send, "abandonment must not send either acknowledgment")
	assert.Equal(t, []finalizationRecord{{EventClassRenewal, true, false}}, observer.snapshot(), "later validator cancellation does not retract finalized admission")
}
func TestTokenRenewalAdmissionCancellationDuringAccountWaitSkipsValidatorAndAck(t *testing.T) {
	client, engine, observer, validator, _ := renewalAdmissionClient(t, true)
	old := client.tokenDeadline()
	entered := make(chan struct{})
	engine.run = func(_ int, call c2EngineCall) AdmissionDecision {
		close(entered)
		<-call.ctx.Done()
		return admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, false)
	}
	done := make(chan bool, 1)
	go func() { done <- client.dispatchFrame([]byte(`{"type":"auth_renew","data":{"token":"fixture-token"}}`)) }()
	lifecycleAwait(t, entered, "renewal account wait entered")
	client.cancelIngressLifecycle()
	select {
	case continued := <-done:
		assert.False(t, continued)
	case <-time.After(time.Second):
		t.Fatal("renewal did not stop after authority cancellation")
	}
	assert.Zero(t, validator.count())
	assert.Equal(t, old, client.tokenDeadline())
	assert.Empty(t, client.Send)
	assert.Empty(t, observer.snapshot())
}
func TestTokenRenewalAdmissionValidatorGetsOwnThreeSecondOldAuthorityChild(t *testing.T) {
	for _, short := range []bool{false, true} {
		t.Run(fmt.Sprintf("shortAuthority%t", short), func(t *testing.T) {
			client, _, _, validator, next := renewalAdmissionClient(t, true)
			if short {
				client.deadlineMu.Lock()
				client.TokenExpiresAt = time.Now().Add(time.Second)
				client.deadlineMu.Unlock()
			}
			old := client.tokenDeadline()
			var deadlines []time.Time
			verified := accessTokenAuth{UserID: client.UserID, FamilyID: client.FamilyID, SessionID: client.SessionID, CredEpoch: client.CredEpoch, ExpiresAt: next}
			validator.run = func(ctx context.Context, _ int) (accessTokenAuth, error) {
				deadline, ok := ctx.Deadline()
				assert.True(t, ok)
				deadlines = append(deadlines, deadline)
				return verified, nil
			}
			before := time.Now()
			accepted, abandoned := client.attemptTokenRenewal(c2Authority(t, client), "fixture-token")
			require.True(t, accepted)
			assert.False(t, abandoned)
			require.Len(t, deadlines, 2)
			for _, deadline := range deadlines {
				if short {
					assert.Equal(t, old, deadline)
				} else {
					assert.GreaterOrEqual(t, deadline.UnixNano(), before.Add(3*time.Second).UnixNano())
					assert.LessOrEqual(t, deadline.UnixNano(), time.Now().Add(3*time.Second).UnixNano())
				}
			}
		})
	}
}
func TestTokenRenewalAdmissionExtensionRechecksCancellationAfterDeadlineLock(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel%t", canceled), func(t *testing.T) {
			client := responseClient(t, true)
			old := client.tokenDeadline()
			next := old.Add(time.Hour)
			parent, cancel := context.WithCancel(client.ingressCtx)
			defer cancel()
			ctx := &responseSnapshotContext{Context: parent, checked: make(chan struct{})}
			ctx.armed.Store(true)
			client.deadlineMu.Lock()
			var unlockOnce sync.Once
			t.Cleanup(func() { unlockOnce.Do(client.deadlineMu.Unlock) })
			result := make(chan bool, 1)
			go func() { result <- client.extendDeadline(ctx, next) }()
			lifecycleAwait(t, ctx.checked, "extension passed fast context check before held mutex")
			if canceled {
				cancel()
			}
			unlockOnce.Do(client.deadlineMu.Unlock)
			select {
			case accepted := <-result:
				assert.Equal(t, !canceled, accepted)
			case <-time.After(time.Second):
				t.Fatal("deadline extension did not settle")
			}
			if canceled {
				assert.Equal(t, old, client.tokenDeadline())
			} else {
				assert.Equal(t, next, client.tokenDeadline())
			}
		})
	}
}

type renewalLaggingDeadlineContext struct {
	context.Context
	deadline time.Time
}

func (ctx renewalLaggingDeadlineContext) Deadline() (time.Time, bool) { return ctx.deadline, true }
func TestTokenRenewalAdmissionExpiredParentDeadlineCannotUseLaterSocketExpiry(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expiredParent%t", expired), func(t *testing.T) {
			client := responseClient(t, true)
			old := client.tokenDeadline()
			deadline := time.Now().Add(time.Minute)
			if expired {
				deadline = time.Unix(1, 0)
			}
			ctx := renewalLaggingDeadlineContext{Context: context.Background(), deadline: deadline}
			require.NoError(t, ctx.Err(), "fixture cancellation notification deliberately lags deadline")
			assert.Equal(t, !expired, client.ingressAuthorityLive(ctx))
			assert.Equal(t, !expired, client.extendDeadline(ctx, old.Add(time.Hour)))
			if expired {
				assert.Equal(t, old, client.tokenDeadline())
			}
		})
	}
}

type renewalAckDeadlineConn struct {
	net.Conn
	arm func()
}

func (conn *renewalAckDeadlineConn) SetReadDeadline(deadline time.Time) error {
	err := conn.Conn.SetReadDeadline(deadline)
	if err == nil {
		conn.arm()
	}
	return err
}
func TestTokenRenewalAdmissionAcknowledgmentRechecksAuthorityAfterQueueLockWait(t *testing.T) {
	for _, kind := range []string{"send", "bootstrap"} {
		for _, cause := range []string{"live", "event canceled", "socket canceled", "old expiry"} {
			t.Run(kind+"/"+cause, func(t *testing.T) {
				client, engine, _, observer := c2Client(t, true)
				server, _, transport := familyInstrumentedSocket(t)
				client.Conn = server
				client.FamilyID = uuid.New()
				client.SessionID = uuid.NewString()
				client.CredEpoch = "ack-fixture"
				old := client.tokenDeadline()
				if cause == "old expiry" {
					old = time.Now().Add(40 * time.Millisecond)
					client.TokenExpiresAt = old
				}
				parent, cancel := context.WithCancel(client.ingressCtx)
				defer cancel()
				ctx := &responseQueueAuthorityContext{Context: parent, deadline: old, checked: make(chan struct{})}
				if kind == "send" {
					ctx.target = 2
				} // Isolate the later sendMu check from the earlier bootstrap check.
				transport.Conn = &renewalAckDeadlineConn{Conn: transport.Conn, arm: func() { ctx.armed.Store(true) }}
				next := old.Add(time.Hour)
				verified := accessTokenAuth{UserID: client.UserID, FamilyID: client.FamilyID, SessionID: client.SessionID, CredEpoch: client.CredEpoch, ExpiresAt: next}
				validator := &renewalAdmissionValidator{run: func(context.Context, int) (accessTokenAuth, error) { return verified, nil }}
				client.renewAuthority = validator
				unlock := responseHoldQueueLock(t, client, kind)
				result := make(chan bool, 1)
				renewalToken := uuid.NewString()
				go func() {
					result <- client.handleTokenRenewal(ctx, IncomingMessage{Type: authRenewMessageType, Data: map[string]interface{}{"token": renewalToken}})
				}()
				lifecycleAwait(t, ctx.checked, "successful renewal reaches final ack check before blocked queue")
				assert.Equal(t, next, client.tokenDeadline(), "validation and extension must succeed before testing acknowledgment wait")
				responseExpireOrCancel(t, client, ctx, cancel, cause)
				unlock()
				select {
				case continued := <-result:
					assert.Equal(t, cause == "live", continued)
				case <-time.After(time.Second):
					t.Fatal("renewal acknowledgment queue wait did not settle")
				}
				assert.Len(t, engine.snapshot(), 1)
				assert.Equal(t, 2, validator.count())
				assert.Equal(t, []finalizationRecord{{EventClassRenewal, true, false}}, observer.snapshot(), "later output abandonment does not retract finalized admission")
				if cause != "live" {
					responseRequireNoBudgetOutput(t, client)
				} else {
					if kind == "bootstrap" {
						require.True(t, client.Hub.completeClientBootstrap(client, []byte(`{"type":"presence_snapshot"}`)))
						<-client.Send
					}
					require.Len(t, client.Send, 1)
					assert.JSONEq(t, string(authRenewAcceptedFrame), string(<-client.Send))
				}
			})
		}
	}
}
