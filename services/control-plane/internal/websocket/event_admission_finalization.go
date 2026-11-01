package websocket

import (
	"context"
	"time"
)

// eventAdmissionObserver receives only closed, aggregate final outcomes. The
// event owner calls it outside engine, client and Hub locks; D owns publication.
type eventAdmissionObserver interface {
	recordFinal(class EventClass, allowed bool, degraded bool)
}

// eventAdmissionState belongs to one event owner, never a shared map or worker.
type eventAdmissionState struct {
	degraded  bool
	finalized bool
}

func (state *eventAdmissionState) observe(decision AdmissionDecision) {
	if state.finalized {
		return
	}
	if decision.Source == AdmissionSourceWarmLocal || decision.Source == AdmissionSourceUnavailable {
		state.degraded = true
	}
}

func (state *eventAdmissionState) finalize(ctx context.Context, c *Client, class EventClass, decision AdmissionDecision) AdmissionDecision {
	abandon := AdmissionDecision{Action: AdmissionAbandon, Source: AdmissionSourceUnavailable}
	if c == nil || c.Hub == nil {
		state.finalized = true
		return abandon
	}
	if !c.Hub.eventBudgetConfig.Enabled {
		return decision
	}
	if state.finalized {
		return abandon
	}
	state.observe(decision)
	// Seal abandonment too: a late result cannot revive the same event.
	state.finalized = true
	if !c.ingressAuthorityLive(ctx) || class < EventClassContent || class > EventClassRenewal ||
		decision.Source < AdmissionSourceExisting || decision.Source > AdmissionSourceUnavailable {
		return abandon
	}
	switch decision.Action {
	case AdmissionAllow, AdmissionRefuse, AdmissionCloseOverload:
		if observer := c.Hub.eventAdmissionObserver; observer != nil {
			observer.recordFinal(class, decision.Action == AdmissionAllow, state.degraded)
		}
		return decision
	default:
		return abandon
	}
}

func (h *Hub) allowContentEvent(c *Client, msg IncomingMessage) bool {
	decision := h.admitContentEvent(c, msg)
	if decision.Action == AdmissionAbandon {
		return false
	}
	if decision.Action != AdmissionAllow {
		h.sendError(msg.ClientID, "Rate limit exceeded, please slow down")
		return false
	}
	return true
}

// admitContentEvent remains at each original Hub-owned subscription/content
// gate. Its context uses the stamped expiry, not the completed read event's
// context, and it retains the one shared content bucket in both modes.
func (h *Hub) admitContentEvent(c *Client, msg IncomingMessage) AdmissionDecision {
	decision := AdmissionDecision{Action: AdmissionAllow, Source: AdmissionSourceExisting}
	if !h.eventBudgetConfig.Enabled {
		if !c.rateLimitAllow() {
			decision.Action = AdmissionRefuse
		}
		return decision
	}
	if c.ingressCtx == nil || msg.TokenExpiresAt.IsZero() || !time.Now().Before(msg.TokenExpiresAt) {
		return abandonedAccountAdmission()
	}
	ctx, cancel := context.WithDeadline(c.ingressCtx, msg.TokenExpiresAt)
	defer cancel()
	if !c.ingressAuthorityLive(ctx) {
		return abandonedAccountAdmission()
	}
	if !c.rateLimitAllow() {
		decision.Action = AdmissionRefuse
	}
	state := eventAdmissionState{}
	return state.finalize(ctx, c, EventClassContent, decision)
}
