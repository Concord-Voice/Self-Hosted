package websocket

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	ws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEventAdmissionDispatchHeldSubscriptionPrecedesRealHubReadyCounts(t *testing.T) {
	setup := setupMessageTest(t)
	hub := setup.hub
	client := setup.client
	hub.redis = setupHubTestRedis(t)
	clock := c2NewClock()
	engine := &c2Engine{clock: clock}
	observer := &finalizationObserver{}
	hub.eventBudgetConfig = EventBudgetConfig{Enabled: true, ScalePercent: 100}
	hub.eventAdmitter = engine
	hub.eventAdmissionObserver = observer
	client.TokenExpiresAt = time.Now().Add(time.Hour)
	client.SessionID = uuid.NewString()
	client.FamilyID = uuid.New()
	client.initIngressLifecycle()
	client.eventAdmissionNow = clock.Now
	client.eventAdmissionNewTimer = clock.NewTimer
	channel := uuid.MustParse(setup.convID)
	delete(client.Channels, channel)
	delete(hub.channelSubscriptions[channel], client.ID)
	server, peer := renewalSocketPair(t)
	client.Conn = server
	engine.run = func(index int, call c2EngineCall) AdmissionDecision {
		if index == 0 {
			assert.Equal(t, EventClassSubscription, call.policy.Class)
			return c2Deny(time.Millisecond)
		}
		return admissionExpected(AdmissionAllow, AdmissionSourceRedis, 0, false)
	}
	go hub.Run()
	t.Cleanup(hub.Shutdown)
	readDone, writeDone := make(chan struct{}), make(chan struct{})
	go func() { client.readPump(); close(readDone) }()
	go func() { client.writePump(); close(writeDone) }()
	t.Cleanup(func() {
		if err := peer.Close(); err != nil && !alreadyDisconnected(err) {
			t.Error("fixture peer close failed")
		}
		lifecycleAwait(t, readDone, "FIFO reader joins")
		lifecycleAwait(t, writeDone, "FIFO writer joins")
	})
	require.NoError(t, peer.WriteMessage(ws.TextMessage, []byte(fmt.Sprintf(`{"type":"subscribe","data":{"channel_id":%q}}`, channel.String()))))
	timer := c2AwaitTimer(t, clock)
	require.NoError(t, peer.WriteMessage(ws.TextMessage, []byte(`{"type":"connection_ready_probe","data":{}}`)))
	assert.Len(t, engine.snapshot(), 1, "single reader must retain the held predecessor")
	assert.Empty(t, observer.snapshot())
	hub.mu.RLock()
	_, active := hub.lastInboundAt[client.UserID]
	hub.mu.RUnlock()
	assert.False(t, active, "held frame must not refresh Hub activity before admission")
	clock.AdvanceTo(timer.due)
	require.NoError(t, peer.SetReadDeadline(time.Now().Add(3*time.Second)))
	subscribed := false
	ready := false
	for index := 0; index < 10 && !ready; index++ {
		_, raw, err := peer.ReadMessage()
		require.NoError(t, err)
		kind, channels, protocol := dispatchDecodeReady(t, raw)
		if kind == "subscribed" {
			subscribed = true
		}
		if kind == "connection_ready" {
			ready = true
			assert.True(t, subscribed)
			assert.Equal(t, 1, channels)
			assert.Equal(t, 2, protocol)
		}
	}
	require.True(t, ready, "actual Hub must publish its ready frame after subscription work")
	calls := engine.snapshot()
	require.Len(t, calls, 3)
	assert.Equal(t, EventClassSubscription, calls[0].policy.Class)
	assert.Equal(t, EventClassSubscription, calls[1].policy.Class)
	assert.Equal(t, EventClassReady, calls[2].policy.Class)
	assert.Equal(t, calls[0].hold, calls[1].hold, "retry retains predecessor's original hold horizon")
	assert.Equal(t, []finalizationRecord{{EventClassSubscription, true, false}, {EventClassReady, true, false}}, observer.snapshot())
}
func TestEventAdmissionDispatchOverloadedPredecessorClosesBeforeQueuedReady(t *testing.T) {
	client, engine, _, observer := c2Client(t, true)
	server, peer, transport := familyInstrumentedSocket(t)
	client.Conn = server
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	engine.run = func(index int, call c2EngineCall) AdmissionDecision {
		if index == 0 {
			close(entered)
			select {
			case <-release:
			case <-call.ctx.Done():
				return finalizationUnavailable()
			}
		}
		return admissionExpected(AdmissionCloseOverload, AdmissionSourceUnavailable, time.Second, false)
	}
	unregistered := make(chan *Client, 1)
	go func() { unregistered <- <-client.Hub.unregister }()
	done := make(chan struct{})
	go func() { client.readPump(); close(done) }()
	t.Cleanup(func() {
		client.cancelIngressLifecycle()
		if err := server.Close(); err != nil && !alreadyDisconnected(err) {
			t.Error("fixture server close failed")
		}
		lifecycleAwait(t, done, "overload fixture reader joins on cleanup")
	})
	require.NoError(t, peer.WriteMessage(ws.TextMessage, []byte(`{"type":"subscribe","data":{}}`)))
	lifecycleAwait(t, entered, "predecessor account decision entered")
	require.NoError(t, peer.WriteMessage(ws.TextMessage, []byte(`{"type":"connection_ready_probe","data":{}}`)))
	releaseOnce.Do(func() { close(release) })
	require.NoError(t, peer.SetReadDeadline(time.Now().Add(time.Second)))
	_, _, err := peer.ReadMessage()
	var closed *ws.CloseError
	require.ErrorAs(t, err, &closed)
	assert.Equal(t, ws.CloseTryAgainLater, closed.Code)
	assert.Equal(t, "retry_after_ms=1000", closed.Text)
	lifecycleAwait(t, done, "reader stops after overloaded predecessor")
	select {
	case actual := <-unregistered:
		assert.Same(t, client, actual)
	case <-time.After(time.Second):
		t.Fatal("overloaded reader did not unregister")
	}
	calls := engine.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, EventClassSubscription, calls[0].policy.Class)
	assert.Empty(t, client.Hub.incoming)
	assert.Positive(t, transport.calls.Load())
	assert.Equal(t, []finalizationRecord{{EventClassSubscription, false, true}}, observer.snapshot())
}
