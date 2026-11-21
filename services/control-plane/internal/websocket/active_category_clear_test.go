package websocket

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/presence"
)

// activeClearProbeFrame is the filler these tests queue to saturate a victim's
// send queue. It is deliberately NOT an active-category frame: the drop test
// asserts the queued bytes are untouched, and a frame that could plausibly have
// been produced by the fan-out would make that assertion unreadable.
var activeClearProbeFrame = []byte(`{"type":"probe"}`)

// newTestHubWithRichPresenceClient registers exactly one rich-presence-capable
// client backed by a REAL socket, reusing the erasure-clear fixtures rather than
// inventing a second shape. The live socket is what makes "was anyone
// disconnected?" answerable by connection state instead of by a stubbed hook
// alone -- a future terminal that called client.Conn.Close() directly would slip
// past a hook counter, but not past this.
func newTestHubWithRichPresenceClient(t *testing.T) *Hub {
	t.Helper()
	hub := NewHub(nil, nil)
	connectLiveRichPresenceClient(t, hub, newErasureClearSocketServer(t), erasureClearSendCapacity)
	return hub
}

// soleRichPresenceClient returns the fixture's single capable client, asserting
// the singleton assumption the readers below rely on.
func soleRichPresenceClient(t *testing.T, hub *Hub) *Client {
	t.Helper()
	hub.mu.RLock()
	defer hub.mu.RUnlock()
	var found *Client
	for _, client := range hub.clients {
		if !activityRichPresenceClient(client) {
			continue
		}
		require.Nil(t, found, "the fixture must register exactly one rich-presence client")
		found = client
	}
	require.NotNil(t, found, "the fixture must register a rich-presence client")
	return found
}

// readOneOutboundFrame consumes the fan-out's single delivered frame, asserting
// exactly one was queued: a sum is also produced by a fan-out that duplicates.
func readOneOutboundFrame(t *testing.T, hub *Hub) []byte {
	t.Helper()
	client := soleRichPresenceClient(t, hub)
	require.Len(t, client.Send, 1, "the fan-out must queue exactly one frame")
	return <-client.Send
}

// saturateOutboundQueue fills the fixture client's send queue so the next
// enqueue has no capacity, which is the drop path under test.
func saturateOutboundQueue(t *testing.T, client *Client) {
	t.Helper()
	for len(client.Send) < cap(client.Send) {
		client.Send <- activeClearProbeFrame
	}
	require.Len(t, client.Send, cap(client.Send), "precondition: the send queue is saturated")
}

func primeActiveCategoryState(
	t *testing.T,
	hub *Hub,
	senderID uuid.UUID,
	category presence.Category,
	clients ...*Client,
) {
	t.Helper()
	recipients := make(map[uuid.UUID]bool, len(clients))
	for _, client := range clients {
		recipients[client.UserID] = true
	}
	require.NoError(t, hub.DeliverRichPresence(context.Background(), presence.DeliveryPlan{
		SenderID:         senderID,
		Category:         category,
		Payload:          json.RawMessage(`{"kind":"test"}`),
		UpdatedAt:        1,
		UpdateRecipients: recipients,
	}))
	for _, client := range clients {
		require.Len(t, client.Send, 1, "the setup update must reach every projected client")
		var message map[string]any
		require.NoError(t, json.Unmarshal(<-client.Send, &message))
		require.Equal(t, "rich_presence_update", message["type"])
	}
}

// T-6, the #2840 regression guard. The terminal must clear, never disconnect.
// If a later change routes this through any disconnect sink, this test fails --
// which is the entire reason it exists. Do not relax it.
//
// Our exposure is worse than #2840's: the trigger is an ordinary authenticated
// group-DM deletion on a route bounded only by RateLimitByUser (5/min/user),
// not a forged bus
// message, so a disconnect terminal here is a user-reachable DoS primitive.
func TestClearSenderActiveCategoryDisconnectsNobody(t *testing.T) {
	hub := newTestHubWithRichPresenceClient(t)
	victim := soleRichPresenceClient(t, hub)
	senderID := uuid.New()
	primeActiveCategoryState(t, hub, senderID, presence.CategoryPrivateCall, victim)
	disconnects := 0
	hub.customTextClientDisconnect = func(*Client) error {
		disconnects++
		return nil
	}
	require.True(t, erasureClearSocketOpen(victim), "precondition: the victim starts connected")

	hub.ClearSenderActiveCategory(senderID, presence.CategoryPrivateCall)

	assert.Zero(t, disconnects, "the proportional terminal must never disconnect a client")
	assert.True(t, erasureClearSocketOpen(victim),
		"no client socket may be closed, including outside the disconnect hook")
	// Without this the assertions above are VACUOUS: "nobody disconnected" also
	// holds for a fan-out that reached nobody at all.
	assert.Len(t, victim.Send, 1, "the fan-out must have run, proving the guard is not vacuous")
}

func TestClearSenderActiveCategorySendsOneClearFrame(t *testing.T) {
	hub := newTestHubWithRichPresenceClient(t)
	senderID := uuid.New()
	primeActiveCategoryState(
		t, hub, senderID, presence.CategoryPrivateCall, soleRichPresenceClient(t, hub),
	)

	hub.ClearSenderActiveCategory(senderID, presence.CategoryPrivateCall)

	frame := readOneOutboundFrame(t, hub)
	var message map[string]any
	require.NoError(t, json.Unmarshal(frame, &message))
	require.Equal(t, "rich_presence_clear", message["type"])

	data, ok := message["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, senderID.String(), data["user_id"])
	assert.Equal(t, string(presence.CategoryPrivateCall), data["category"])
	assert.NotContains(t, data, "payload", "a clear frame carries no payload")
}

// A saturated client must be dropped, never closed. Closing on overflow is what
// made the Custom Status fan-out a DoS primitive in the first place.
func TestClearSenderActiveCategoryDropsRatherThanClosing(t *testing.T) {
	hub := newTestHubWithRichPresenceClient(t)
	victim := soleRichPresenceClient(t, hub)
	// A second capable client with free capacity. Without it every assertion
	// below is VACUOUS -- "not disconnected, queue unchanged" is exactly what an
	// empty fan-out produces too. This one is the delivery witness: same hub,
	// same call, one delivery.
	witness := connectClient(hub, uuid.New())
	witness.activityRichPresenceCapable = true
	senderID := uuid.New()
	primeActiveCategoryState(
		t, hub, senderID, presence.CategoryPrivateCall, victim, witness,
	)
	saturateOutboundQueue(t, victim)
	disconnects := 0
	hub.customTextClientDisconnect = func(*Client) error {
		disconnects++
		return nil
	}

	hub.ClearSenderActiveCategory(senderID, presence.CategoryPrivateCall)

	require.Len(t, witness.Send, 1, "the fan-out must have run for the client that had capacity")
	assert.Zero(t, disconnects)
	assert.True(t, erasureClearSocketOpen(victim), "a full queue must drop the clear, not disconnect")
	// The queue is never inspected, dequeued, or swapped: the already queued
	// frame outranks the new one.
	require.Len(t, victim.Send, cap(victim.Send), "the clear must be dropped, never queued")
	assert.Equal(t, activeClearProbeFrame, <-victim.Send, "the queued frame must be untouched")
}

func TestClearSenderActiveCategoryIsNilSafe(t *testing.T) {
	var hub *Hub
	assert.NotPanics(t, func() {
		hub.ClearSenderActiveCategory(uuid.New(), presence.CategoryPrivateCall)
	})
}

// TestClearSenderActiveCategorySkipsClientWithoutRichPresenceCapability locks the
// capability gate. The capable client sharing the hub is what makes the exclusion
// meaningful: alone, "the legacy client received nothing" also holds for a
// fan-out that ran for nobody.
func TestClearSenderActiveCategorySkipsClientWithoutRichPresenceCapability(t *testing.T) {
	hub := newTestHubWithRichPresenceClient(t)
	legacy := connectClient(hub, uuid.New()) // activityRichPresenceCapable stays false
	capable := soleRichPresenceClient(t, hub)
	senderID := uuid.New()
	primeActiveCategoryState(t, hub, senderID, presence.CategoryServerVoice, capable)

	hub.ClearSenderActiveCategory(senderID, presence.CategoryServerVoice)

	assert.Empty(t, legacy.Send, "a non-rich-presence client must receive nothing")
	assert.Len(t, capable.Send, 1,
		"the capable client proves the fan-out ran; without it the exclusion above is vacuous")
}

func TestClearSenderActiveCategorySkipsClientWithoutDeliveredState(t *testing.T) {
	hub := newTestHubWithRichPresenceClient(t)
	holder := soleRichPresenceClient(t, hub)
	bystander := connectClient(hub, uuid.New())
	bystander.activityRichPresenceCapable = true
	senderID := uuid.New()
	primeActiveCategoryState(t, hub, senderID, presence.CategoryPrivateCall, holder)

	hub.ClearSenderActiveCategory(senderID, presence.CategoryPrivateCall)

	assert.Empty(t, bystander.Send,
		"an opted-in client without the sender/category projection must receive no identifier")
	assert.Len(t, holder.Send, 1,
		"the client holding the projection must still receive its terminating clear")
	<-holder.Send

	hub.ClearSenderActiveCategory(senderID, presence.CategoryPrivateCall)
	assert.Empty(t, holder.Send, "a delivered clear must consume the projection receipt")
}

func TestClearSenderActiveCategoryClearsBootstrapProjection(t *testing.T) {
	hub := newTestHubWithRichPresenceClient(t)
	holder := soleRichPresenceClient(t, hub)
	senderID := uuid.New()
	hub.activitySnapshot = func(context.Context, uuid.UUID) (presence.ActivitySnapshot, error) {
		return presence.ActivitySnapshot{
			senderID: {
				presence.CategoryPrivateCall: {
					Payload:   json.RawMessage(`{"call_type":"group","participant_count":2}`),
					UpdatedAt: 1,
				},
			},
		}, nil
	}
	holder.beginBootstrap()
	hub.runClientBootstrap(context.Background(), holder, presenceSnapshotSeed{
		viewerID: holder.UserID, connectedIDs: []uuid.UUID{holder.UserID},
	})
	var snapshot map[string]any
	require.NoError(t, json.Unmarshal(readOneOutboundFrame(t, hub), &snapshot))
	require.Equal(t, "presence_snapshot", snapshot["type"])

	hub.ClearSenderActiveCategory(senderID, presence.CategoryPrivateCall)

	frame := readOneOutboundFrame(t, hub)
	var message map[string]any
	require.NoError(t, json.Unmarshal(frame, &message))
	assert.Equal(t, "rich_presence_clear", message["type"])
}

func TestClearSenderActiveCategoryBootstrapReceiptReplay(t *testing.T) {
	for _, test := range []struct {
		name      string
		snapshot  bool
		wantClear bool
	}{
		{name: "empty snapshot then buffered update", wantClear: true},
		{name: "populated snapshot then buffered clear", snapshot: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			hub := newTestHubWithRichPresenceClient(t)
			holder := soleRichPresenceClient(t, hub)
			senderID := uuid.New()
			key := richPresenceDeliveryKey{senderID: senderID, category: presence.CategoryPrivateCall}
			holder.beginBootstrap()
			if test.snapshot {
				holder.setRichPresenceSnapshot(map[richPresenceDeliveryKey]struct{}{key: {}})
			} else {
				holder.setRichPresenceSnapshot(nil)
			}
			plan := presence.DeliveryPlan{
				SenderID: senderID, Category: presence.CategoryPrivateCall,
				Payload: json.RawMessage("{}"), UpdatedAt: 1,
			}
			if test.snapshot {
				plan.ClearRecipients = map[uuid.UUID]bool{holder.UserID: true}
			} else {
				plan.UpdateRecipients = map[uuid.UUID]bool{holder.UserID: true}
			}
			require.NoError(t, hub.DeliverRichPresence(context.Background(), plan))
			require.Empty(t, holder.Send, "the live delta must wait behind bootstrap")
			require.True(t, hub.completeClientBootstrap(holder, []byte("{}")))
			require.Len(t, holder.Send, 2)
			<-holder.Send
			<-holder.Send

			hub.ClearSenderActiveCategory(senderID, presence.CategoryPrivateCall)
			if test.wantClear {
				var frame map[string]any
				require.NoError(t, json.Unmarshal(readOneOutboundFrame(t, hub), &frame))
				assert.Equal(t, "rich_presence_clear", frame["type"])
			} else {
				assert.Empty(t, holder.Send, "a buffered clear must consume the snapshot receipt")
			}
		})
	}
}

func TestClearSenderActiveCategoryBootstrapDiscardedReceipts(t *testing.T) {
	for _, failFlush := range []bool{false, true} {
		name := "canceled"
		if failFlush {
			name = "failed flush"
		}
		t.Run(name, func(t *testing.T) {
			hub := newTestHubWithRichPresenceClient(t)
			holder := soleRichPresenceClient(t, hub)
			senderID := uuid.New()
			holder.beginBootstrap()
			holder.setRichPresenceSnapshot(nil)
			require.NoError(t, hub.DeliverRichPresence(context.Background(), presence.DeliveryPlan{
				SenderID: senderID, Category: presence.CategoryPrivateCall,
				Payload: json.RawMessage("{}"), UpdatedAt: 1,
				UpdateRecipients: map[uuid.UUID]bool{holder.UserID: true},
			}))
			require.NotEmpty(t, holder.bootstrapRichPresenceLive)
			if failFlush {
				saturateOutboundQueue(t, holder)
				require.False(t, hub.completeClientBootstrap(holder, []byte("{}")))
			} else {
				holder.cancelBootstrap()
			}
			assert.Empty(t, holder.richPresenceDelivered)
			assert.Empty(t, holder.bootstrapRichPresenceLive)
			assert.False(t, holder.bootstrapRichPresenceSnapshotSet)
			if !failFlush {
				holder.beginBootstrap()
				holder.setRichPresenceSnapshot(nil)
				require.True(t, hub.completeClientBootstrap(holder, []byte("{}")))
				<-holder.Send
				hub.ClearSenderActiveCategory(senderID, presence.CategoryPrivateCall)
				assert.Empty(t, holder.Send, "canceled delivery must not gain a receipt")
			}
		})
	}
}
