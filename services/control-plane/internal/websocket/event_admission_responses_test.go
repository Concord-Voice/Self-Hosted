package websocket

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func responseClient(t *testing.T, enabled bool) *Client {
	t.Helper()
	hub, err := NewHubWithEventAdmission(nil, nil, nil, EventBudgetConfig{Enabled: enabled, ScalePercent: 100}, &lifecycleBorrowedAdmitter{})
	require.NoError(t, err)
	t.Cleanup(hub.ingressCancel)
	return lifecycleClient(t, hub)
}
func TestEventAdmissionResponseRetryMilliseconds(t *testing.T) {
	for _, tt := range []struct {
		retry time.Duration
		want  int64
	}{
		{-time.Second, 1000}, {0, 1000}, {time.Nanosecond, 100}, {99*time.Millisecond + time.Nanosecond, 100},
		{100 * time.Millisecond, 100}, {100*time.Millisecond + time.Nanosecond, 101},
		{999*time.Millisecond + time.Nanosecond, 1000}, {5 * time.Second, 5000},
		{29999*time.Millisecond + time.Nanosecond, 30000}, {30 * time.Second, 30000},
		{30*time.Second + time.Nanosecond, 30000}, {time.Duration(math.MaxInt64), 30000},
	} {
		t.Run(fmt.Sprint(tt.retry), func(t *testing.T) { assert.Equal(t, tt.want, eventBudgetRetryMilliseconds(tt.retry)) })
	}
}

type responseControlTransport struct {
	net.Conn
	mu        sync.Mutex
	deadlines []time.Time
	writes    atomic.Int32
	failWrite bool
}

func (c *responseControlTransport) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.deadlines = append(c.deadlines, deadline)
	c.mu.Unlock()
	return c.Conn.SetWriteDeadline(deadline)
}
func (c *responseControlTransport) Write(data []byte) (int, error) {
	c.writes.Add(1)
	if c.failWrite {
		return 0, errors.New("synthetic control write failure")
	}
	return c.Conn.Write(data)
}
func (c *responseControlTransport) lastDeadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.deadlines) == 0 {
		return time.Time{}
	}
	return c.deadlines[len(c.deadlines)-1]
}
func responseSocket(t *testing.T, client *Client) (*ws.Conn, *familyCloseConn, *responseControlTransport) {
	t.Helper()
	server, peer, transport := familyInstrumentedSocket(t)
	observed := &responseControlTransport{Conn: transport.Conn}
	transport.Conn = observed // Installed before any test writer/pump starts.
	client.Conn = server
	return peer, transport, observed
}
func TestEventAdmissionResponseOverloadWritesExact1013AndCloses(t *testing.T) {
	client := responseClient(t, true)
	peer, transport, observed := responseSocket(t, client)
	before := time.Now()
	require.True(t, client.closeEventBudgetOverload(context.Background(), 100*time.Millisecond+time.Nanosecond))
	deadline := observed.lastDeadline()
	require.False(t, deadline.IsZero())
	assert.GreaterOrEqual(t, deadline.UnixNano(), before.Add(100*time.Millisecond).UnixNano())
	assert.LessOrEqual(t, deadline.UnixNano(), time.Now().Add(100*time.Millisecond).UnixNano(), "control deadline must lie in the real call bracket plus 100ms")
	require.Positive(t, transport.calls.Load())
	require.NoError(t, peer.SetReadDeadline(time.Now().Add(time.Second)))
	_, _, err := peer.ReadMessage()
	var closed *ws.CloseError
	require.ErrorAs(t, err, &closed)
	assert.Equal(t, ws.CloseTryAgainLater, closed.Code)
	assert.Equal(t, "retry_after_ms=101", closed.Text)
	assert.ErrorIs(t, client.ingressCtx.Err(), context.Canceled)
}
func TestEventAdmissionResponseOverloadShortensControlDeadlineToAuthority(t *testing.T) {
	client := responseClient(t, true)
	_, transport, observed := responseSocket(t, client)
	expiry := time.Now().Add(40 * time.Millisecond)
	client.deadlineMu.Lock()
	client.TokenExpiresAt = expiry
	client.deadlineMu.Unlock()
	require.True(t, client.closeEventBudgetOverload(context.Background(), 0))
	assert.Equal(t, expiry, observed.lastDeadline())
	assert.Positive(t, transport.calls.Load())
}
func TestEventAdmissionResponseOverloadClosesEvenWhenCourtesyWriteFails(t *testing.T) {
	client := responseClient(t, true)
	_, transport, observed := responseSocket(t, client)
	observed.failWrite = true
	require.True(t, client.closeEventBudgetOverload(context.Background(), time.Second))
	assert.Positive(t, observed.writes.Load())
	assert.Positive(t, transport.calls.Load())
	assert.ErrorIs(t, client.ingressCtx.Err(), context.Canceled)
}
func TestEventAdmissionResponseOverloadDoesNotWaitForApplicationOrOrdinaryWriterLock(t *testing.T) {
	client := responseClient(t, true)
	peer, transport, _ := responseSocket(t, client)
	transport.blockWrite.Store(true)
	writerDone := make(chan error, 1)
	go func() { writerDone <- client.Conn.WriteMessage(ws.TextMessage, []byte("ordinary pending frame")) }()
	lifecycleAwait(t, transport.writeEntered, "ordinary writer owns Gorilla write mutex")
	var releaseOnce, unlockOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(transport.writeRelease) }) }
	client.sendMu.Lock()
	t.Cleanup(func() { unlockOnce.Do(client.sendMu.Unlock); release() })
	result := make(chan bool, 1)
	go func() { result <- client.closeEventBudgetOverload(context.Background(), time.Second) }()
	select {
	case attempted := <-result:
		assert.True(t, attempted)
	case <-time.After(time.Second):
		t.Fatal("courtesy close exceeded fixture bound behind writer locks")
	}
	assert.Positive(t, transport.calls.Load(), "control timeout must still close the actual transport")
	require.NoError(t, peer.SetReadDeadline(time.Now().Add(time.Second)))
	_, _, err := peer.ReadMessage()
	require.Error(t, err)
	unlockOnce.Do(client.sendMu.Unlock)
	release()
	select {
	case err := <-writerDone:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("fixture ordinary writer did not join")
	}
}
func TestEventAdmissionResponseSuppressedWithoutLiveEnabledAuthority(t *testing.T) {
	for _, state := range []string{"disabled", "terminal", "expired", "client canceled", "parent canceled", "Hub canceled"} {
		t.Run(state, func(t *testing.T) {
			client := responseClient(t, state != "disabled")
			_, transport, observed := responseSocket(t, client)
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch state {
			case "terminal":
				client.securityTerminal.Store(true)
			case "expired":
				client.deadlineMu.Lock()
				client.TokenExpiresAt = time.Now().Add(-time.Second)
				client.deadlineMu.Unlock()
			case "client canceled":
				client.cancelIngressLifecycle()
			case "parent canceled":
				cancel()
			case "Hub canceled":
				client.Hub.ingressCancel()
			}
			assert.False(t, client.closeEventBudgetOverload(parent, time.Second))
			assert.False(t, client.sendEventBudgetAdvisory(parent, EventClassProfile, time.Second))
			assert.Zero(t, observed.writes.Load())
			assert.Zero(t, transport.calls.Load())
			assert.Empty(t, client.Send)
		})
	}
}
func responseAssertAdvisory(t *testing.T, raw []byte, class string, retry int64) {
	t.Helper()
	// This verifies the Go producer shape only. The canonical consumer schema
	// still needs its separate compatibility change before activation.
	require.JSONEq(t, fmt.Sprintf(`{"type":"error","data":{"code":"ws_rate_limited","event_class":%q,"retry_after_ms":%d}}`, class, retry), string(raw))
	var value struct {
		Type string `json:"type"`
		Data struct {
			Retry json.Number `json:"retry_after_ms"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(&value))
	assert.Equal(t, json.Number(fmt.Sprint(retry)), value.Data.Retry, "wire retry must be a canonical integer, not a float/string")
}
func TestEventAdmissionResponseAdvisorySharesOneSecondSlotAcrossClasses(t *testing.T) {
	client := responseClient(t, true)
	now := time.Time{} // Explicit first-use flag must distinguish this from no advice.
	client.eventAdmissionNow = func() time.Time { return now }
	require.True(t, client.sendEventBudgetAdvisory(context.Background(), EventClassProfile, 5*time.Second))
	responseAssertAdvisory(t, <-client.Send, "profile", 5000)
	assert.False(t, client.sendEventBudgetAdvisory(context.Background(), EventClassServer, time.Second))
	now = now.Add(time.Second - time.Nanosecond)
	assert.False(t, client.sendEventBudgetAdvisory(context.Background(), EventClassPresence, time.Second))
	assert.Empty(t, client.Send)
	now = now.Add(time.Nanosecond)
	require.True(t, client.sendEventBudgetAdvisory(context.Background(), EventClassPresence, time.Nanosecond))
	responseAssertAdvisory(t, <-client.Send, "presence", 100)
	now = now.Add(time.Second)
	require.True(t, client.sendEventBudgetAdvisory(context.Background(), EventClassServer, 0))
	responseAssertAdvisory(t, <-client.Send, "server", 1000)
}
func TestEventAdmissionResponseFullQueueConsumesAdvisorySlot(t *testing.T) {
	client := responseClient(t, true)
	client.Send = make(chan []byte, 1)
	client.Send <- []byte("queued")
	now := time.Time{}
	client.eventAdmissionNow = func() time.Time { return now }
	result := make(chan bool, 1)
	go func() { result <- client.sendEventBudgetAdvisory(context.Background(), EventClassProfile, time.Second) }()
	select {
	case sent := <-result:
		assert.False(t, sent)
	case <-time.After(time.Second):
		t.Fatal("full queue must drop advice promptly")
	}
	assert.Equal(t, "queued", string(<-client.Send))
	assert.False(t, client.sendEventBudgetAdvisory(context.Background(), EventClassServer, time.Second), "a dropped first advice still consumes the shared slot")
	now = now.Add(time.Second)
	require.True(t, client.sendEventBudgetAdvisory(context.Background(), EventClassServer, time.Second))
	responseAssertAdvisory(t, <-client.Send, "server", 1000)
}
func TestEventAdmissionResponseAdvisoryPreservesBootstrapOrdering(t *testing.T) {
	client := responseClient(t, true)
	client.beginBootstrap()
	snapshot := []byte(`{"type":"presence_snapshot"}`)
	replay := []byte(`{"type":"custom_replay"}`)
	require.NoError(t, client.appendBootstrapReplay(replay))
	require.True(t, client.sendEventBudgetAdvisory(context.Background(), EventClassProfile, 5*time.Second))
	assert.Empty(t, client.Send, "advice must remain behind bootstrap publication")
	require.True(t, client.Hub.completeClientBootstrap(client, snapshot))
	assert.Equal(t, snapshot, <-client.Send)
	assert.Equal(t, replay, <-client.Send)
	responseAssertAdvisory(t, <-client.Send, "profile", 5000)
	assert.Empty(t, client.Send)
}
func TestEventAdmissionResponseAdvisoryRejectsNonAdvisoryClasses(t *testing.T) {
	for _, class := range []EventClass{0, 255, EventClassContent, EventClassTyping, EventClassSubscription, EventClassReady, EventClassRenewal} {
		client := responseClient(t, true)
		assert.False(t, client.sendEventBudgetAdvisory(context.Background(), class, time.Second))
		assert.Empty(t, client.Send)
	}
}

// Snapshot the initial nil cancellation result before signaling the test. The
// caller can then cancel while the actual authority-deadline mutex is held.
// Only the per-event parent is canceled; Client lifetime remains live, isolating
// the required post-snapshot parent recheck from the other authority guards.
type responseSnapshotContext struct {
	context.Context
	armed   atomic.Bool
	checked chan struct{}
	once    sync.Once
}

func (ctx *responseSnapshotContext) Err() error {
	err := ctx.Context.Err()
	if err == nil && ctx.armed.Load() {
		ctx.once.Do(func() { close(ctx.checked) })
	}
	return err
}
func TestEventAdmissionResponseCancellationDuringAuthoritySnapshotSuppressesOverload(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel%t", canceled), func(t *testing.T) {
			client := responseClient(t, true)
			peer, transport, observed := responseSocket(t, client)
			parent, cancel := context.WithCancel(client.ingressCtx)
			defer cancel()
			ctx := &responseSnapshotContext{Context: parent, checked: make(chan struct{})}
			ctx.armed.Store(true)
			client.deadlineMu.Lock()
			var unlockOnce sync.Once
			t.Cleanup(func() { unlockOnce.Do(client.deadlineMu.Unlock) })
			result := make(chan bool, 1)
			go func() { result <- client.closeEventBudgetOverload(ctx, time.Second) }()
			lifecycleAwait(t, ctx.checked, "overload passes initial cancellation snapshot")
			if canceled {
				cancel()
			}
			unlockOnce.Do(client.deadlineMu.Unlock)
			select {
			case attempted := <-result:
				assert.Equal(t, !canceled, attempted)
			case <-time.After(time.Second):
				t.Fatal("overload authority snapshot did not settle")
			}
			if canceled {
				assert.Zero(t, observed.writes.Load())
				assert.Zero(t, transport.calls.Load())
				lifecycleRequireActive(client.ingressCtx, t)
			} else {
				require.Positive(t, transport.calls.Load())
				require.NoError(t, peer.SetReadDeadline(time.Now().Add(time.Second)))
				_, _, err := peer.ReadMessage()
				var closed *ws.CloseError
				require.ErrorAs(t, err, &closed)
				assert.Equal(t, ws.CloseTryAgainLater, closed.Code)
			}
		})
	}
}
func TestEventAdmissionResponseCancellationDuringFinalSnapshotSuppressesAdvisory(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel%t", canceled), func(t *testing.T) {
			client := responseClient(t, true)
			parent, cancel := context.WithCancel(client.ingressCtx)
			defer cancel()
			ctx := &responseSnapshotContext{Context: parent, checked: make(chan struct{})}
			var armOnce, unlockOnce sync.Once
			client.eventAdmissionNow = func() time.Time {
				// This seam runs after the first live-authority check. Arm only now so
				// extra defensive checks in that first guard cannot trip the barrier.
				armOnce.Do(func() { client.deadlineMu.Lock(); ctx.armed.Store(true) })
				return time.Time{}
			}
			t.Cleanup(func() {
				if ctx.armed.Load() {
					unlockOnce.Do(client.deadlineMu.Unlock)
				}
			})
			result := make(chan bool, 1)
			go func() { result <- client.sendEventBudgetAdvisory(ctx, EventClassProfile, 5*time.Second) }()
			lifecycleAwait(t, ctx.checked, "advisory reaches final authority snapshot")
			if canceled {
				cancel()
			}
			unlockOnce.Do(client.deadlineMu.Unlock)
			select {
			case sent := <-result:
				assert.Equal(t, !canceled, sent)
			case <-time.After(time.Second):
				t.Fatal("advisory authority snapshot did not settle")
			}
			if canceled {
				assert.Empty(t, client.Send)
				lifecycleRequireActive(client.ingressCtx, t)
			} else {
				require.Len(t, client.Send, 1)
				responseAssertAdvisory(t, <-client.Send, "profile", 5000)
			}
		})
	}
}

// Arm only at the final caller-side authority check. The fixed parent deadline
// can expire with a deliberately lagging Err notification while queue locks wait.
type responseQueueAuthorityContext struct {
	context.Context
	deadline     time.Time
	armed        atomic.Bool
	checked      chan struct{}
	snapshotLive atomic.Bool
	checks       atomic.Int32
	target       int32
	once         sync.Once
}

func (ctx *responseQueueAuthorityContext) Deadline() (time.Time, bool) {
	if ctx.armed.Load() {
		wanted := ctx.target
		if wanted == 0 {
			wanted = 1
		}
		if ctx.checks.Add(1) == wanted {
			ctx.once.Do(func() { ctx.snapshotLive.Store(time.Now().Before(ctx.deadline)); close(ctx.checked) })
		}
	}
	return ctx.deadline, true
}
func responseHoldQueueLock(t *testing.T, client *Client, kind string) func() {
	t.Helper()
	var once sync.Once
	if kind == "bootstrap" {
		client.beginBootstrap()
		client.bootstrapMu.Lock()
		unlock := func() { once.Do(client.bootstrapMu.Unlock) }
		t.Cleanup(unlock)
		return unlock
	}
	client.sendMu.Lock()
	unlock := func() { once.Do(client.sendMu.Unlock) }
	t.Cleanup(unlock)
	return unlock
}
func responseExpireOrCancel(t *testing.T, client *Client, ctx *responseQueueAuthorityContext, cancel context.CancelFunc, cause string) {
	t.Helper()
	require.True(t, ctx.snapshotLive.Load(), "authority must be live before queue-lock waiting")
	switch cause {
	case "event canceled":
		cancel()
	case "socket canceled":
		client.cancelIngressLifecycle()
	case "old expiry":
		timer := time.NewTimer(time.Until(ctx.deadline))
		defer timer.Stop()
		<-timer.C
		require.NoError(t, ctx.Err(), "deadline notification deliberately lags actual expiry")
	}
}
func responseRequireNoBudgetOutput(t *testing.T, client *Client) {
	t.Helper()
	assert.Empty(t, client.Send)
	client.bootstrapMu.Lock()
	buffered := len(client.bootstrapLive)
	client.bootstrapMu.Unlock()
	assert.Zero(t, buffered, "abandoned event cannot leak advice or acknowledgment into bootstrap buffer")
}
func TestEventAdmissionResponseAdvisoryRechecksAuthorityAfterQueueLockWait(t *testing.T) {
	for _, kind := range []string{"send", "bootstrap"} {
		for _, cause := range []string{"live", "event canceled", "socket canceled", "old expiry"} {
			t.Run(kind+"/"+cause, func(t *testing.T) {
				client := responseClient(t, true)
				parent, cancel := context.WithCancel(client.ingressCtx)
				defer cancel()
				deadline := time.Now().Add(time.Hour)
				if cause == "old expiry" {
					deadline = time.Now().Add(40 * time.Millisecond)
				}
				ctx := &responseQueueAuthorityContext{Context: parent, deadline: deadline, checked: make(chan struct{})}
				if kind == "send" {
					ctx.target = 2
				} // Last pre-send check is inside the inactive bootstrap stage.
				client.eventAdmissionNow = func() time.Time { ctx.armed.Store(true); return time.Time{} }
				unlock := responseHoldQueueLock(t, client, kind)
				result := make(chan bool, 1)
				go func() { result <- client.sendEventBudgetAdvisory(ctx, EventClassProfile, 5*time.Second) }()
				lifecycleAwait(t, ctx.checked, "advisory final live-authority check before blocked queue")
				responseExpireOrCancel(t, client, ctx, cancel, cause)
				unlock()
				select {
				case sent := <-result:
					assert.Equal(t, cause == "live", sent)
				case <-time.After(time.Second):
					t.Fatal("advisory queue wait did not settle")
				}
				if cause != "live" {
					responseRequireNoBudgetOutput(t, client)
				} else {
					if kind == "bootstrap" {
						require.True(t, client.Hub.completeClientBootstrap(client, []byte(`{"type":"presence_snapshot"}`)))
						<-client.Send
					}
					require.Len(t, client.Send, 1)
					responseAssertAdvisory(t, <-client.Send, "profile", 5000)
				}
			})
		}
	}
}

func TestEventAdmissionResponseOverloadControlDeadlineRespectsEarlierParent(t *testing.T) {
	client := responseClient(t, true)
	_, transport, observed := responseSocket(t, client)
	deadline := time.Now().Add(40 * time.Millisecond)
	ctx, cancel := context.WithDeadline(client.ingressCtx, deadline)
	defer cancel()
	require.True(t, client.closeEventBudgetOverload(ctx, time.Second))
	assert.Equal(t, deadline, observed.lastDeadline(), "parent authority must shorten the 100ms courtesy deadline even when Client expiry is later")
	assert.Positive(t, transport.calls.Load())
}

type responseOverloadDeadlineContext struct {
	context.Context
	deadline time.Time
	snapshot func()
	once     sync.Once
}

func (ctx *responseOverloadDeadlineContext) Deadline() (time.Time, bool) {
	ctx.once.Do(ctx.snapshot)
	return ctx.deadline, true
}
func TestEventAdmissionResponseOverloadRechecksParentAtSecondDeadlineSnapshot(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired%t", expired), func(t *testing.T) {
			client := responseClient(t, true)
			_, transport, observed := responseSocket(t, client)
			deadline := time.Now().Add(40 * time.Millisecond)
			blocked := make(chan struct{})
			var unlockOnce sync.Once
			ctx := &responseOverloadDeadlineContext{Context: context.Background(), deadline: deadline, snapshot: func() { client.deadlineMu.Lock(); close(blocked) }}
			t.Cleanup(func() {
				select {
				case <-blocked:
					unlockOnce.Do(client.deadlineMu.Unlock)
				default:
				}
			})
			result := make(chan bool, 1)
			go func() { result <- client.closeEventBudgetOverload(ctx, time.Second) }()
			lifecycleAwait(t, blocked, "first authority snapshot is live before the separate deadline read")
			require.True(t, time.Now().Before(deadline), "fixture must establish live initial authority")
			if expired {
				timer := time.NewTimer(time.Until(deadline))
				defer timer.Stop()
				<-timer.C
			}
			unlockOnce.Do(client.deadlineMu.Unlock)
			select {
			case attempted := <-result:
				assert.Equal(t, !expired, attempted)
			case <-time.After(time.Second):
				t.Fatal("overload second deadline snapshot did not settle")
			}
			require.NoError(t, ctx.Err(), "actual deadline must dominate lagging cancellation notification")
			if expired {
				assert.Zero(t, observed.writes.Load())
				assert.Zero(t, transport.calls.Load())
				lifecycleRequireActive(client.ingressCtx, t)
			} else {
				assert.Equal(t, deadline, observed.lastDeadline())
				assert.Positive(t, transport.calls.Load())
			}
		})
	}
}
