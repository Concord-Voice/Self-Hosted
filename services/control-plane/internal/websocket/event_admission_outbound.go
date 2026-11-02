package websocket

import (
	"context"
	"time"
)

// outboundEventAuthority is captured before queue locks. Token renewal can only
// extend the Client deadline, so this snapshot remains a conservative bound.
// The event context retains its own earlier deadline after successful renewal.
type outboundEventAuthority struct {
	expiry time.Time
}

// live is safe under sendMu/bootstrapMu: it never acquires deadlineMu, cancels
// work or invokes an observer. Nil preserves generic outbound behavior.
func (authority *outboundEventAuthority) live(ctx context.Context, c *Client) bool {
	if authority == nil {
		return true
	}
	if ctx == nil || ctx.Err() != nil || c.ingressCtx == nil || c.ingressStopped() {
		return false
	}
	now := time.Now()
	if deadline, bounded := ctx.Deadline(); bounded && !now.Before(deadline) {
		return false
	}
	return !authority.expiry.IsZero() && now.Before(authority.expiry)
}

// enqueueOutboundWithAuthority adds event authority to the existing bootstrap
// and bounded queue path. A live full queue is a dropped response, not abandonment.
func (c *Client) enqueueOutboundWithAuthority(ctx context.Context, data []byte) (enqueued, abandoned bool) {
	authority := outboundEventAuthority{expiry: c.tokenDeadline()}
	enqueued = c.enqueueOutboundChecked(ctx, data, &authority)
	return enqueued, !enqueued && !authority.live(ctx, c)
}
