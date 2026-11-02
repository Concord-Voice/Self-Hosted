package websocket

import (
	"context"

	"github.com/google/uuid"
)

func (c *Client) dispatchLegacyEvent(msg IncomingMessage) bool {
	if msg.Type == authRenewMessageType {
		ctx, cancel := c.ingressEventContext()
		defer cancel()
		return c.handleTokenRenewal(ctx, msg)
	}
	// Disabled ordinary routing retains unknown input and allocates no event
	// deadline, heartbeat bucket or admission state.
	return c.enqueueInboundEvent(context.Background(), msg)
}

func (c *Client) dispatchBudgetedEvent(msg IncomingMessage) bool {
	policy, known := classifyInboundEvent(msg.Type)
	if !known {
		return true
	}
	ctx, cancel := c.ingressEventContext()
	defer cancel()
	if !c.ingressAuthorityLive(ctx) {
		return false
	}
	// Renewal owns its local claim before calling account admission, and never
	// carries a credential through Hub.Run.
	if msg.Type == authRenewMessageType {
		return c.handleTokenRenewal(ctx, msg)
	}
	decision := c.admitInboundEvent(ctx, policy)
	return c.dispatchEventAdmission(ctx, msg, policy, decision)
}

func (c *Client) dispatchEventAdmission(ctx context.Context, msg IncomingMessage, policy EventPolicy, decision AdmissionDecision) bool {
	switch decision.Action {
	case AdmissionAllow:
		return c.enqueueInboundEvent(ctx, msg)
	case AdmissionRefuse:
		if policy.MayHold {
			c.closeEventBudgetOverload(ctx, decision.RetryAfter)
			return false
		}
		if policy.Scope == BudgetScopeAccount {
			// The advisory helper accepts only presence/profile/server; typing
			// refusal and local heartbeat refusal remain silent.
			c.sendEventBudgetAdvisory(ctx, policy.Class, decision.RetryAfter)
		}
		return c.ingressAuthorityLive(ctx)
	case AdmissionCloseOverload:
		c.closeEventBudgetOverload(ctx, decision.RetryAfter)
		return false
	default:
		return false
	}
}

// enqueueInboundEvent stamps only authenticated Client state after admission.
// The single reader serializes renewal, so SID and expiry are one snapshot.
func (c *Client) enqueueInboundEvent(ctx context.Context, msg IncomingMessage) bool {
	msg.UserID = c.UserID
	msg.ClientID = c.ID
	msg.CredEpoch = c.CredEpoch
	msg.SID, _ = uuid.Parse(c.SessionID)
	msg.TokenExpiresAt = c.tokenDeadline()
	// The snapshot mutex can wait. Recheck before the queue select; finalized
	// admission stays counted if this downstream wait is subsequently canceled.
	if c.ingressStopped() {
		return false
	}
	var eventDone <-chan struct{}
	// Legacy ordinary events have no per-event authority context. Background
	// preserves their existing queue behavior without adding an expiry gate.
	if c.Hub.eventBudgetConfig.Enabled {
		if !c.ingressAuthorityLive(ctx) {
			return false
		}
		eventDone = ctx.Done()
	}
	select {
	case c.Hub.incoming <- msg:
		return true
	case <-eventDone:
		return false
	case <-c.ingressDone():
		return false
	case <-c.Hub.done:
		return false
	}
}
