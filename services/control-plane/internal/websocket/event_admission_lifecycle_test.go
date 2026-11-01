package websocket

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/ingressbudget"
	"github.com/google/uuid"
	ws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Hub borrows this engine. Construction and lifecycle teardown must never
// acquire an account decision or take ownership of engine Close.
type lifecycleBorrowedAdmitter struct{ calls, closes atomic.Int32 }

func (a *lifecycleBorrowedAdmitter) Admit(context.Context, uuid.UUID, EventPolicy, time.Time) AdmissionDecision {
	a.calls.Add(1)
	return AdmissionDecision{Action: AdmissionAllow, Source: AdmissionSourceRedis}
}
func (a *lifecycleBorrowedAdmitter) Close() error { a.closes.Add(1); return nil }
func lifecycleHub(t *testing.T) *Hub {
	t.Helper()
	hub := NewHub(nil, nil)
	t.Cleanup(func() { hub.ingressCancel() })
	return hub
}
func lifecycleClient(t *testing.T, hub *Hub) *Client {
	t.Helper()
	client := &Client{ID: uuid.New(), UserID: uuid.New(), Hub: hub, TokenExpiresAt: time.Now().Add(time.Hour), Send: make(chan []byte, 8), Channels: make(map[uuid.UUID]bool)}
	client.initIngressLifecycle()
	t.Cleanup(client.cancelIngressLifecycle)
	return client
}
func lifecycleAwait(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("lifecycle fixture did not settle: %s", label)
	}
}
func lifecycleRequireActive(ctx context.Context, t *testing.T) {
	t.Helper()
	require.NoError(t, ctx.Err())
	select {
	case <-ctx.Done():
		t.Fatal("authority context unexpectedly canceled")
	default:
	}
}
func TestEventAdmissionLifecycleHubConstructionIsImmutableAndBorrowed(t *testing.T) {
	engine := &lifecycleBorrowedAdmitter{}
	cfg := EventBudgetConfig{Enabled: true, ScalePercent: 100}
	hub, err := NewHubWithEventAdmission(nil, nil, nil, cfg, engine)
	require.NoError(t, err)
	cfg.Enabled = false
	cfg.ScalePercent = 25
	assert.Equal(t, EventBudgetConfig{Enabled: true, ScalePercent: 100}, hub.eventBudgetConfig)
	assert.Same(t, engine, hub.eventAdmitter)
	require.NotNil(t, hub.ingressCtx)
	require.NotNil(t, hub.ingressCancel)
	assert.Zero(t, engine.calls.Load())
	assert.Zero(t, engine.closes.Load())
	go hub.Run()
	hub.Shutdown()
	hub.Shutdown()
	assert.Zero(t, engine.closes.Load(), "Hub never closes its borrowed engine")
	assert.Zero(t, engine.calls.Load(), "C1 introduces no admission/dispatch work")
	legacy := lifecycleHub(t)
	assert.Equal(t, EventBudgetConfig{Enabled: false, ScalePercent: 100}, legacy.eventBudgetConfig)
	assert.Nil(t, legacy.eventAdmitter)
	disabled, err := NewHubWithEventAdmission(nil, nil, nil, EventBudgetConfig{Enabled: false, ScalePercent: 100}, nil)
	require.NoError(t, err)
	t.Cleanup(disabled.ingressCancel)
	assert.Equal(t, EventBudgetConfig{Enabled: false, ScalePercent: 100}, disabled.eventBudgetConfig)
	for _, test := range []struct {
		cfg      EventBudgetConfig
		admitter EventAdmitter
	}{
		{EventBudgetConfig{Enabled: true, ScalePercent: 100}, nil},
		{EventBudgetConfig{Enabled: true, ScalePercent: 24}, engine},
		{EventBudgetConfig{Enabled: false, ScalePercent: 0}, nil},
		{EventBudgetConfig{Enabled: false, ScalePercent: 401}, engine},
	} {
		rejected, err := NewHubWithEventAdmission(nil, nil, nil, test.cfg, test.admitter)
		require.Error(t, err)
		assert.Nil(t, rejected)
	}
	assert.Zero(t, engine.closes.Load(), "construction failure retains caller ownership")
}
func TestEventAdmissionLifecycleFrameDeadlineRenewsWithoutBoundingSocketLifetime(t *testing.T) {
	hub := lifecycleHub(t)
	client := lifecycleClient(t, hub)
	old := client.tokenDeadline()
	if _, bounded := client.ingressCtx.Deadline(); bounded {
		t.Fatal("opening expiry must not bound the renewable socket lifetime")
	}
	first, finishFirst := client.ingressEventContext()
	defer finishFirst()
	deadline, ok := first.Deadline()
	require.True(t, ok)
	assert.Equal(t, old, deadline)
	renewed := old.Add(time.Hour)
	require.True(t, client.extendDeadline(renewed))
	second, finishSecond := client.ingressEventContext()
	defer finishSecond()
	deadline, ok = second.Deadline()
	require.True(t, ok)
	assert.Equal(t, renewed, deadline)
	firstDeadline, _ := first.Deadline()
	assert.Equal(t, old, firstDeadline, "renewal cannot extend an already-started event")
	finishFirst()
	assert.ErrorIs(t, first.Err(), context.Canceled)
	lifecycleRequireActive(second, t)
	lifecycleRequireActive(client.ingressCtx, t)
	client.cancelIngressLifecycle()
	lifecycleAwait(t, second.Done(), "socket cancellation propagates to current event")
	assert.False(t, client.ingressAuthorityLive(second))
}
func TestEventAdmissionLifecycleAuthorityRejectsMissingExpiredAndTerminalState(t *testing.T) {
	for _, state := range []string{"uninitialized", "zero expiry", "expired expiry", "security terminal", "client canceled", "hub canceled"} {
		t.Run(state, func(t *testing.T) {
			hub := lifecycleHub(t)
			client := &Client{Hub: hub, TokenExpiresAt: time.Now().Add(time.Hour)}
			if state != "uninitialized" {
				client.initIngressLifecycle()
				t.Cleanup(client.cancelIngressLifecycle)
			}
			switch state {
			case "zero expiry":
				client.TokenExpiresAt = time.Time{}
			case "expired expiry":
				client.TokenExpiresAt = time.Now().Add(-time.Second)
			case "security terminal":
				client.securityTerminal.Store(true)
			case "client canceled":
				client.cancelIngressLifecycle()
			case "hub canceled":
				hub.ingressCancel()
			}
			ctx, cancel := client.ingressEventContext()
			defer cancel()
			assert.Error(t, ctx.Err(), "invalid authority must not manufacture a usable frame context")
			assert.False(t, client.ingressAuthorityLive(ctx))
		})
	}
	t.Run("actual deadline wins before old frame notification", func(t *testing.T) {
		hub := lifecycleHub(t)
		client := lifecycleClient(t, hub)
		event, cancel := client.ingressEventContext()
		defer cancel()
		client.deadlineMu.Lock()
		client.TokenExpiresAt = time.Now().Add(-time.Second)
		client.deadlineMu.Unlock()
		require.NoError(t, event.Err(), "fixture isolates actual current deadline from old context notification")
		assert.False(t, client.ingressAuthorityLive(event))
	})
}
func TestEventAdmissionLifecycleIsSeparateFromAsyncBootstrap(t *testing.T) {
	hub := lifecycleHub(t)
	client := lifecycleClient(t, hub)
	async, cancelAsync := context.WithCancel(context.Background())
	client.asyncCancel = cancelAsync
	defer cancelAsync()
	event, finish := client.ingressEventContext()
	defer finish()
	cancelAsync()
	require.ErrorIs(t, async.Err(), context.Canceled)
	lifecycleRequireActive(event, t)
	lifecycleRequireActive(client.ingressCtx, t)
	assert.True(t, client.ingressAuthorityLive(event))
}
func TestEventAdmissionLifecycleHubShutdownCancelsBeforeDeliveryLock(t *testing.T) {
	hub := lifecycleHub(t)
	client := lifecycleClient(t, hub)
	event, cancel := client.ingressEventContext()
	defer cancel()
	hub.serverVoiceDeliveryMu.Lock()
	var unlockOnce sync.Once
	t.Cleanup(func() { unlockOnce.Do(hub.serverVoiceDeliveryMu.Unlock) })
	go hub.Run()
	stopped := make(chan struct{})
	go func() { hub.Shutdown(); close(stopped) }()
	lifecycleAwait(t, event.Done(), "shutdown cancellation must precede the locked delivery stage")
	assert.ErrorIs(t, client.ingressCtx.Err(), context.Canceled)
	unlockOnce.Do(hub.serverVoiceDeliveryMu.Unlock)
	lifecycleAwait(t, stopped, "Hub shutdown joins after delivery lock release")
}
func TestEventAdmissionLifecycleOrdinaryReadAndWriteTeardownCancel(t *testing.T) {
	for _, pump := range []string{"read", "write"} {
		t.Run(pump, func(t *testing.T) {
			hub := lifecycleHub(t)
			client := lifecycleClient(t, hub)
			server, peer, transport := familyInstrumentedSocket(t)
			client.Conn = server
			event, cancel := client.ingressEventContext()
			defer cancel()
			originalCancel := client.ingressCancel
			client.ingressCancel = func() {
				assert.Positive(t, transport.calls.Load(), "transport must close before teardown cancellation")
				originalCancel()
			}
			done := make(chan struct{})
			if pump == "read" {
				unregistered := make(chan *Client, 1)
				go func() { unregistered <- <-hub.unregister }()
				go func() { client.readPump(); close(done) }()
				require.NoError(t, peer.Close())
				lifecycleAwait(t, event.Done(), "ordinary reader teardown")
				select {
				case got := <-unregistered:
					assert.Same(t, client, got)
				case <-time.After(3 * time.Second):
					t.Fatal("ordinary read teardown never requested unregister")
				}
			} else {
				close(client.Send)
				go func() { client.writePump(); close(done) }()
				lifecycleAwait(t, event.Done(), "ordinary writer teardown")
			}
			lifecycleAwait(t, done, "pump joins")
		})
	}
}
func TestEventAdmissionLifecycleExpiryCloses1008BeforeCancel(t *testing.T) {
	hub := lifecycleHub(t)
	client := lifecycleClient(t, hub)
	server, peer, transport := familyInstrumentedSocket(t)
	client.Conn = server
	event, cancel := client.ingressEventContext()
	defer cancel()
	originalCancel := client.ingressCancel
	client.ingressCancel = func() { assert.Positive(t, transport.calls.Load()); originalCancel() }
	client.deadlineMu.Lock()
	client.TokenExpiresAt = time.Now().Add(-time.Second)
	client.deadlineMu.Unlock()
	client.onTokenExpiryTimer(make(chan struct{})) // Pending, so unregister must not block this callback.
	lifecycleAwait(t, event.Done(), "expiry cancellation")
	require.NoError(t, peer.SetReadDeadline(time.Now().Add(time.Second)))
	_, _, err := peer.ReadMessage()
	var closed *ws.CloseError
	require.ErrorAs(t, err, &closed)
	assert.Equal(t, ws.ClosePolicyViolation, closed.Code)
	assert.False(t, client.ingressAuthorityLive(event))
}

func TestEventAdmissionLifecycleFamilyReceiptPrecedesEveryIngressCallback(t *testing.T) {
	hub := lifecycleHub(t)
	user, family := uuid.New(), uuid.New()
	type callbackObservation struct{ allFenced, allClosed, receiptQueued bool }
	callbacks := make(chan callbackObservation, 3)
	receipt := make(chan error, 1)
	command := familyDisconnect{userID: user, familyID: family, done: receipt}
	clients := make([]*Client, 0, 3)
	transports := make([]*familyCloseConn, 0, 3)
	for i := 0; i < 3; i++ {
		server, _, transport := familyInstrumentedSocket(t)
		client := familyPublishedClient(user, family, server)
		client.Hub = hub
		client.initIngressLifecycle()
		clients = append(clients, client)
		transports = append(transports, transport)
		hub.clients[client.ID] = client
		if hub.userClients[user] == nil {
			hub.userClients[user] = make(map[uuid.UUID]bool)
		}
		hub.userClients[user][client.ID] = true
	}
	var receiptObserved atomic.Bool
	var sendCleanupProgress, deadlineCleanupProgress atomic.Int32
	for _, client := range clients {
		originalCancel := client.ingressCancel
		t.Cleanup(originalCancel)
		var once sync.Once
		client.ingressCancel = func() {
			once.Do(func() {
				observation := callbackObservation{allFenced: true, allClosed: true, receiptQueued: len(receipt) == 1 || receiptObserved.Load()}
				for _, sibling := range clients {
					observation.allFenced = observation.allFenced && sibling.securityTerminal.Load()
				}
				for _, transport := range transports {
					observation.allClosed = observation.allClosed && transport.calls.Load() > 0
				}
				callbacks <- observation
				// This is deliberately lock-blocked cleanup. Transport fencing and the
				// checked receipt must already be complete before any callback can wait.
				client.sendMu.Lock()
				sendCleanupProgress.Add(1)
				client.sendMu.Unlock()
				client.deadlineMu.Lock()
				deadlineCleanupProgress.Add(1)
				client.deadlineMu.Unlock()
			})
			originalCancel()
		}
		client.sendMu.Lock()
		client.deadlineMu.Lock()
	}
	var unlockOnce sync.Once
	unlock := func() {
		unlockOnce.Do(func() {
			for _, client := range clients {
				client.sendMu.Unlock()
				client.deadlineMu.Unlock()
			}
		})
	}
	finished := make(chan struct{})
	t.Cleanup(func() {
		unlock()
		select {
		case <-hub.done:
		default:
			close(hub.done)
		}
		lifecycleAwait(t, finished, "family sweep exits after test lock release")
	})
	go func() { hub.handleDisconnectFamily(command); close(finished) }()
	first := func() callbackObservation {
		select {
		case got := <-callbacks:
			return got
		case <-time.After(3 * time.Second):
			t.Fatal("family ingress callback was not reached")
			return callbackObservation{}
		}
	}()
	assert.True(t, first.allFenced)
	assert.True(t, first.allClosed)
	assert.True(t, first.receiptQueued, "checked receipt must precede the first new callback")
	assert.Zero(t, sendCleanupProgress.Load(), "held send lock must still block cleanup after transport receipt")
	assert.Zero(t, deadlineCleanupProgress.Load(), "held authority lock must still block cleanup after transport receipt")
	select {
	case err := <-receipt:
		require.NoError(t, err)
		receiptObserved.Store(true)
	default:
		t.Fatal("lock-blocked ingress callback held back transport receipt")
	}
	for _, transport := range transports {
		assert.Positive(t, transport.calls.Load())
	}
	unlock()
	for i := 1; i < 3; i++ {
		select {
		case got := <-callbacks:
			assert.True(t, got.allFenced)
			assert.True(t, got.allClosed)
			assert.True(t, got.receiptQueued)
		case <-time.After(3 * time.Second):
			t.Fatal("sibling ingress callback was skipped")
		}
	}
	lifecycleAwait(t, finished, "family sweep completion")
	for _, client := range clients {
		lifecycleAwait(t, client.ingressCtx.Done(), "family context canceled")
	}
	assert.Equal(t, int32(3), sendCleanupProgress.Load())
	assert.Equal(t, int32(3), deadlineCleanupProgress.Load())
}
func TestEventAdmissionLifecycleFailedFamilyCloseStillCancelsIngress(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "close error", true: "missing transport"}[missing], func(t *testing.T) {
			hub := lifecycleHub(t)
			user, family := uuid.New(), uuid.New()
			server, _, transport := familyInstrumentedSocket(t)
			client := familyPublishedClient(user, family, server)
			client.Hub = hub
			if missing {
				client.Conn = nil
			} else {
				transport.closeErr = errors.New("synthetic family close failure")
			}
			client.initIngressLifecycle()
			t.Cleanup(client.cancelIngressLifecycle)
			hub.clients[client.ID] = client
			hub.userClients[user] = map[uuid.UUID]bool{client.ID: true}
			receipt := make(chan error, 1)
			originalCancel := client.ingressCancel
			var receiptBeforeCancel atomic.Bool
			client.ingressCancel = func() { receiptBeforeCancel.Store(len(receipt) == 1); originalCancel() }
			hub.handleDisconnectFamily(familyDisconnect{userID: user, familyID: family, done: receipt})
			lifecycleAwait(t, client.ingressCtx.Done(), "failed transport also loses ingress authority")
			assert.True(t, receiptBeforeCancel.Load())
			require.Error(t, <-receipt, "failed Close must not become a successful checked receipt")
			assert.False(t, client.securityCloseConfirmed.Load())
		})
	}
}
func TestEventAdmissionLifecycleUnregisterCancelsBeforeAsyncJoin(t *testing.T) {
	hub := lifecycleHub(t)
	client := lifecycleClient(t, hub)
	sibling := lifecycleClient(t, hub)
	sibling.UserID = client.UserID
	hub.clients[client.ID] = client
	hub.clients[sibling.ID] = sibling
	hub.userClients[client.UserID] = map[uuid.UUID]bool{client.ID: true, sibling.ID: true}
	client.asyncWg.Add(1)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(client.asyncWg.Done) }
	t.Cleanup(release)
	done := make(chan struct{})
	go func() { hub.handleUnregister(client); close(done) }()
	lifecycleAwait(t, client.ingressCtx.Done(), "unregister cannot wait for async work before canceling ingress")
	lifecycleRequireActive(sibling.ingressCtx, t)
	release()
	lifecycleAwait(t, done, "unregister joins after async release")
	// Already-unpublished ordinary teardown must remain idempotent and canceled.
	hub.handleUnregister(client)
	assert.ErrorIs(t, client.ingressCtx.Err(), context.Canceled)
}
func TestEventAdmissionLifecycleFamilyCancelAbandonsActiveAccountDecision(t *testing.T) {
	backend := &admissionBackendFake{}
	entered := make(chan struct{}, 1)
	backend.setDecide(func(call admissionBackendCall) (RedisBudgetDecision, error) {
		entered <- struct{}{}
		<-call.context.Done()
		return RedisBudgetDecision{Allowed: true}, nil
	})
	clock := admissionNewClock()
	engine := admissionNewClocked(t, EventBudgetConfig{Enabled: true, ScalePercent: 100}, backend, clock)
	hub, err := NewHubWithEventAdmission(nil, nil, nil, EventBudgetConfig{Enabled: true, ScalePercent: 100}, engine)
	require.NoError(t, err)
	t.Cleanup(hub.ingressCancel)
	server, _, _ := familyInstrumentedSocket(t)
	client := familyPublishedClient(uuid.New(), uuid.New(), server)
	client.Hub = hub
	client.initIngressLifecycle()
	t.Cleanup(client.cancelIngressLifecycle)
	hub.clients[client.ID] = client
	hub.userClients[client.UserID] = map[uuid.UUID]bool{client.ID: true}
	authority, finish := client.ingressEventContext()
	defer finish()
	result := make(chan AdmissionDecision, 1)
	go func() {
		result <- engine.Admit(authority, client.UserID, admissionPolicy(EventClassProfile), time.Time{})
	}()
	lifecycleAwait(t, entered, "account backend entered")
	receipt := make(chan error, 1)
	hub.handleDisconnectFamily(familyDisconnect{userID: client.UserID, familyID: client.FamilyID, done: receipt})
	require.NoError(t, <-receipt)
	assert.Equal(t, admissionExpected(AdmissionAbandon, AdmissionSourceUnavailable, 0, false), admissionAwait(t, result), "late otherwise-successful backend reply cannot restore family authority")
	_, closes := backend.counts()
	assert.Zero(t, closes, "family teardown only borrows engine ownership")
	select {
	case <-hub.done:
	default:
		close(hub.done)
	}
}

// Observing the context's actual wait-channel acquisition gives each wait a
// per-instance barrier. Cancellation cannot win only an earlier entry check.
type lifecycleObservedWait struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (ctx *lifecycleObservedWait) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.waiting) })
	return ctx.Context.Done()
}
func lifecycleObserveWait(client *Client) <-chan struct{} {
	observed := &lifecycleObservedWait{Context: client.ingressCtx, waiting: make(chan struct{})}
	client.ingressCtx = observed
	return observed.waiting
}
func TestEventAdmissionLifecycleSocketCancelWakesBlockedHubEnqueue(t *testing.T) {
	hub := lifecycleHub(t)
	client := lifecycleClient(t, hub)
	hub.incoming = make(chan IncomingMessage, 1)
	hub.incoming <- IncomingMessage{Type: "existing-frame"}
	waiting := lifecycleObserveWait(client)
	result := make(chan bool, 1)
	go func() { result <- client.dispatchFrame([]byte(`{"type":"profile_update","data":{}}`)) }()
	lifecycleAwait(t, waiting, "actual dispatch waits on full Hub ingress")
	lifecycleRequireActive(hub.ingressCtx, t)
	select {
	case <-hub.done:
		t.Fatal("fixture Hub must remain live")
	default:
	}
	client.cancelIngressLifecycle()
	select {
	case continued := <-result:
		assert.False(t, continued)
	case <-time.After(3 * time.Second):
		t.Fatal("socket cancellation did not wake blocked Hub enqueue")
	}
	require.Len(t, hub.incoming, 1)
	assert.Equal(t, "existing-frame", (<-hub.incoming).Type, "canceled frame must not replace or follow queued work")
	assert.Empty(t, hub.incoming)
	lifecycleRequireActive(hub.ingressCtx, t)
	// Positive control reaches the same dispatch boundary with available space.
	live := lifecycleClient(t, hub)
	require.True(t, live.dispatchFrame([]byte(`{"type":"profile_update","data":{}}`)))
	queued := <-hub.incoming
	assert.Equal(t, "profile_update", queued.Type)
	assert.Equal(t, live.UserID, queued.UserID)
}
func TestEventAdmissionLifecycleSocketCancelWakesExhaustedPreJSONPacing(t *testing.T) {
	hub := lifecycleHub(t)
	client := lifecycleClient(t, hub)
	fixedNow := time.Now()
	bucket := ingressbudget.NewBucketWithClock(applicationFrameInterval, applicationFrameBurst, func() time.Time { return fixedNow })
	for i := 0; i < applicationFrameBurst; i++ {
		require.True(t, bucket.Allow())
	}
	require.False(t, bucket.Allow(), "fixture must exhaust the original pre-JSON bucket")
	waiting := lifecycleObserveWait(client)
	result := make(chan bool, 1)
	go func() { result <- client.awaitFrameBudget(bucket) }()
	lifecycleAwait(t, waiting, "actual pacing waits after exhaustion")
	client.cancelIngressLifecycle()
	select {
	case allowed := <-result:
		assert.False(t, allowed)
	case <-time.After(3 * time.Second):
		t.Fatal("socket cancellation did not stop pre-JSON pacing")
	}
	assert.False(t, bucket.Allow(), "cancellation must not manufacture a paced token")
	lifecycleRequireActive(hub.ingressCtx, t)
	live := lifecycleClient(t, hub)
	fresh := ingressbudget.NewBucketWithClock(applicationFrameInterval, applicationFrameBurst, func() time.Time { return fixedNow })
	assert.True(t, live.awaitFrameBudget(fresh), "positive control preserves the original available-bucket behavior")
}
func TestEventAdmissionLifecycleDeliveryGateReleasedBeforeFailedClientCancel(t *testing.T) {
	hub := lifecycleHub(t)
	client := lifecycleClient(t, hub)
	sibling := lifecycleClient(t, hub)
	sibling.UserID = client.UserID
	hub.clients[client.ID] = client
	hub.clients[sibling.ID] = sibling
	hub.userClients[client.UserID] = map[uuid.UUID]bool{client.ID: true, sibling.ID: true}
	serverID, channelID := uuid.New(), uuid.New()
	key := serverVoiceDeliveryKey{serverID: serverID, channelID: channelID}
	gate := hub.serverVoiceDeliveryGate(key)
	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{client.ID: true}
	for i := 0; i < cap(client.Send); i++ {
		client.Send <- []byte("already queued")
	}
	var callbackCalled, gateAvailable, enqueueProtected atomic.Bool
	originalCancel := client.ingressCancel
	client.ingressCancel = func() {
		callbackCalled.Store(true)
		if gate.mu.TryLock() {
			gateAvailable.Store(true)
			gate.mu.Unlock()
		}
		originalCancel()
	}
	hub.serverVoiceDeliveryBeforeEnqueue = func() {
		if gate.mu.TryLock() {
			gate.mu.Unlock()
		} else {
			enqueueProtected.Store(true)
		}
	}
	result := channelDeliveryResult{
		serverID: serverID, channelID: channelID,
		data:                             []byte(`{"type":"voice_state_update"}`),
		decisions:                        []channelDeliveryDecision{{clientID: client.ID, allowed: true}},
		retryOnAuthorizationInvalidation: true,
		serverVoiceMutationEpoch:         gate.epoch,
		authzState:                       hub.presenceAuthzState.Load(),
		authzEpoch:                       hub.serverVoiceAuthzEpochs[key],
	}
	stale := result
	stale.serverVoiceMutationEpoch++
	assert.Equal(t, serverVoiceDeliveryRetry, hub.applyServerBroadcastDeliveryResult(stale))
	assert.False(t, callbackCalled.Load(), "stale generation must not reach failed-client cleanup")
	lifecycleRequireActive(client.ingressCtx, t)
	assert.Equal(t, serverVoiceDeliveryApplied, hub.applyServerBroadcastDeliveryResult(result))
	assert.True(t, enqueueProtected.Load(), "generation validation and outbound enqueue must remain one gated transition")
	assert.True(t, callbackCalled.Load(), "full outbound queue must reach the real unregister cancellation path")
	assert.True(t, gateAvailable.Load(), "ingress cancellation callback must run after releasing the delivery gate")
	assert.ErrorIs(t, client.ingressCtx.Err(), context.Canceled)
	lifecycleRequireActive(sibling.ingressCtx, t)
}

func TestEventAdmissionLifecycleMalformedFrameHonorsIngressStop(t *testing.T) {
	for _, state := range []string{"live", "socket canceled", "security terminal", "Hub canceled"} {
		t.Run(state, func(t *testing.T) {
			hub := lifecycleHub(t)
			client := lifecycleClient(t, hub)
			switch state {
			case "socket canceled":
				client.cancelIngressLifecycle()
			case "security terminal":
				client.securityTerminal.Store(true)
			case "Hub canceled":
				hub.ingressCancel()
			}
			// C1 preserves the existing malformed-frame continuation only while the
			// socket lifecycle is live. It does not introduce an admission/class gate.
			assert.Equal(t, state == "live", client.dispatchFrame([]byte("{")))
			assert.Empty(t, hub.incoming, "malformed input must not enqueue work in any lifecycle state")
		})
	}
}
