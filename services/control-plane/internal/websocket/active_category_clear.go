package websocket

import (
	"log"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/presence"
)

// ClearSenderActiveCategory clears one sender/category only from local clients
// whose connection has previously been given that projection. It does not
// recompute an audience and never disconnects anyone.
//
// It is the active-category analogue of ClearErasedSenderCustomText, and it
// exists for the same reason. The alternative terminal -- a fleet-wide
// DISCONNECT via DisconnectAllRichPresenceClients -- makes the triggering
// mutation a denial-of-service primitive; the red-team PoCs on PR #2840 proved
// that for the Custom Status fan-out, and the exposure here is worse because the
// trigger is an ordinary authenticated group-DM deletion rather than a forged
// bus message, on a route bounded only by RateLimitByUser (5/min/user).
//
// A clear frame is PROPORTIONAL to the claim: the connection-local projection
// index admits only clients that actually received this sender/category state.
// This is also the privacy boundary: capability opt-in alone must not reveal a
// private-call participant identifier or its termination timing.
//
// The dropped-frame cost is bounded better here than for Custom Status. The
// caller deletes the source generation before calling this, so no new viewer can
// see the stale state and no republish can revive it; a viewer that drops the
// frame converges at its next presence_snapshot.
func (h *Hub) ClearSenderActiveCategory(senderID uuid.UUID, category presence.Category) {
	// Fail closed on a nil receiver rather than panicking, matching
	// ClearErasedSenderCustomText: a hub-less replica has no local clients to
	// clear, and a panic inside a reconciler goroutine would take the worker
	// down.
	if h == nil {
		return
	}
	// A nil payload selects the rich_presence_clear shape. No new wire type:
	// this is the same frame DeliverRichPresence sends to revoked viewers,
	// parameterised by category.
	data, err := marshalRichPresenceFrame(senderID, category, false, nil, 0)
	if err != nil {
		log.Printf("[hub] failed to marshal active-category clear: %T", err)
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()
	dropped := 0
	key := richPresenceDeliveryKey{senderID: senderID, category: category}
	for _, client := range h.clients {
		if !activityRichPresenceClient(client) {
			continue
		}
		// enqueueActiveCategoryClear never closes the client and never touches
		// the reconnect-replacement buffer.
		//
		// Plain enqueueOutbound is NOT used: its first act is
		// bufferBootstrapLive, so for a client inside its replacement window a
		// burst latches bootstrapFailed and the bootstrap path disconnects the
		// client on its own goroutine -- the same fleet-wide disconnect, moved
		// out of sight of a fan-out-only assertion.
		//
		// enqueuePrivacyCritical is NOT used either: its close-on-overflow arm
		// is correct only where the frame IS the authorization revocation.
		eligible, delivered := client.enqueueActiveCategoryClear(data, key)
		if eligible && !delivered {
			dropped++
		}
	}
	if dropped > 0 {
		// ONE aggregate line per fan-out, never one per client: per-client
		// logging would hand a flood O(clients) writes per message, re-creating
		// in the log the amplification just removed from the socket. Keep any
		// future edit free of sensitive field labels near a format verb -- the
		// AST guard in the log-emissions test rejects them.
		log.Printf("[hub] active-category clear frames dropped for %d client(s); resource_limit", dropped)
	}
}

func (c *Client) enqueueActiveCategoryClear(
	data []byte,
	key richPresenceDeliveryKey,
) (eligible, delivered bool) {
	if c == nil || c.securityTerminal.Load() {
		return false, false
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	c.bootstrapMu.Lock()
	bootstrapping := c.bootstrapActive || c.bootstrapCanceled
	c.bootstrapMu.Unlock()
	if bootstrapping || c.securityTerminal.Load() || c.sendClosed || c.Send == nil {
		return false, false
	}
	if _, ok := c.richPresenceDelivered[key]; !ok {
		return false, false
	}
	select {
	case c.Send <- data:
		delete(c.richPresenceDelivered, key)
		return true, true
	default:
		return true, false
	}
}
