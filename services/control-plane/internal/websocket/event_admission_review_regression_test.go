package websocket

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEventAdmissionDequeueRejectsCanceledLifetimeInBothModes(t *testing.T) {
	for _, mode := range []struct {
		name    string
		enabled bool
	}{{"disabled", false}, {"enabled", true}} {
		t.Run(mode.name, func(t *testing.T) {
			for _, state := range []string{"live", "socket canceled", "Hub canceled"} {
				t.Run(state, func(t *testing.T) {
					client, _, _, _ := c2Client(t, mode.enabled)
					hub := client.Hub
					hub.clients[client.ID] = client
					hub.userClients[client.UserID] = map[uuid.UUID]bool{client.ID: true}
					require.True(t, client.dispatchFrame([]byte(`{"type":"connection_ready_probe","data":{}}`)))
					require.Len(t, hub.incoming, 1)
					// Cancel after a successful send, before the productive dequeue.
					// This also covers a send winning a ready-send/cancel select.
					switch state {
					case "socket canceled":
						client.cancelIngressLifecycle()
					case "Hub canceled":
						hub.ingressCancel()
					}
					hub.handleIncoming(<-hub.incoming)
					_, active := hub.lastInboundAt[client.UserID]
					assert.Equal(t, state == "live", active, "canceled work must not refresh activity")
					if state == "live" {
						require.Len(t, client.Send, 1, "live control reaches the real ready handler")
						assert.Contains(t, string(<-client.Send), `"type":"connection_ready"`)
					} else {
						assert.Empty(t, client.Send, "canceled work must not execute the ready handler")
					}
				})
			}
		})
	}
}

func TestEventAdmissionFamilyReceiptDoesNotWaitForLivePumpCancellation(t *testing.T) {
	hub := lifecycleHub(t)
	user, family := uuid.New(), uuid.New()
	callbackEntered, releaseCallbacks := make(chan struct{}), make(chan struct{})
	var callbackOnce, releaseOnce sync.Once
	var closedTransports atomic.Int32
	var transports []*familyCloseConn
	var pumps []<-chan struct{}
	finished := make(chan struct{})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseCallbacks) })
		close(hub.done) // Unblock pump unregister and deferred security cleanup.
		for _, done := range pumps {
			lifecycleAwait(t, done, "live reader exits")
		}
		lifecycleAwait(t, finished, "family sweep exits")
	})
	for i := 0; i < 2; i++ {
		server, _, transport := familyInstrumentedSocket(t)
		client := familyPublishedClient(user, family, server)
		client.Hub = hub
		client.initIngressLifecycle()
		originalCancel := client.ingressCancel
		client.ingressCancel = func() {
			callbackOnce.Do(func() { close(callbackEntered) })
			<-releaseCallbacks
			originalCancel()
		}
		var transportOnce sync.Once
		transport.beforeClose = func() {
			transportOnce.Do(func() {
				if closedTransports.Add(1) == 2 {
					// Before the second transport closes, a real reader must have
					// entered cancellation. Sweep cancellation is still unreachable.
					select {
					case <-callbackEntered:
					case <-time.After(3 * time.Second):
						t.Error("first live reader did not reach cancellation before sibling close")
					}
				}
			})
		}
		hub.clients[client.ID] = client
		if hub.userClients[user] == nil {
			hub.userClients[user] = map[uuid.UUID]bool{}
		}
		hub.userClients[user][client.ID] = true
		transports = append(transports, transport)
		done := make(chan struct{})
		pumps = append(pumps, done)
		go func() { client.readPump(); close(done) }()
	}
	receipt := make(chan error, 1)
	go func() {
		hub.handleDisconnectFamily(familyDisconnect{userID: user, familyID: family, done: receipt})
		close(finished)
	}()
	select {
	case err := <-receipt:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("live pump cancellation blocked the checked family receipt")
	}
	for _, transport := range transports {
		assert.Positive(t, transport.calls.Load(), "every sibling transport closes before receipt")
	}
	select {
	case <-finished:
		t.Fatal("sweep cleanup must still be waiting for cancellation callbacks")
	default:
	}
}
