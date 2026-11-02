package websocket

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
)

// eventBudgetRetryMilliseconds is the external retry grammar shared by overload
// close and advisory. Unavailable callers pass zero; scheduling keeps raw delays.
func eventBudgetRetryMilliseconds(retry time.Duration) int64 {
	if retry <= 0 {
		return 1000
	}
	if retry >= 30*time.Second {
		return 30000
	}
	milliseconds := int64(retry / time.Millisecond)
	if retry%time.Millisecond != 0 {
		milliseconds++
	}
	if milliseconds < 100 {
		return 100
	}
	return milliseconds
}

// closeEventBudgetOverload does not acquire the application writer/send lock.
// Gorilla bounds its own control-write serialization by the supplied deadline.
// Security transport closure remains independent and can interrupt this write.
func (c *Client) closeEventBudgetOverload(ctx context.Context, retry time.Duration) bool {
	if c.Hub == nil || !c.Hub.eventBudgetConfig.Enabled || !c.ingressAuthorityLive(ctx) || c.Conn == nil {
		return false
	}
	expiry := c.tokenDeadline()
	// This separate snapshot can also wait; do not emit using authority checked
	// before the wait, even when the earlier ingressAuthorityLive check passed.
	if ctx.Err() != nil || c.ingressStopped() || expiry.IsZero() {
		return false
	}
	now := time.Now()
	parentDeadline, bounded := ctx.Deadline()
	if bounded && !now.Before(parentDeadline) {
		return false
	}
	if !now.Before(expiry) {
		return false
	}
	deadline := now.Add(100 * time.Millisecond)
	if expiry.Before(deadline) {
		deadline = expiry
	}
	if bounded && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	reason := "retry_after_ms=" + strconv.FormatInt(eventBudgetRetryMilliseconds(retry), 10)
	if err := c.Conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseTryAgainLater, reason), deadline); err != nil {
		log.Printf("WebSocket event budget close frame failed: %s", describeSocketFailure(err))
	}
	if err := c.Conn.Close(); err != nil && !alreadyDisconnected(err) {
		log.Printf("WebSocket event budget transport close failed: %s", describeSocketFailure(err))
	}
	c.cancelIngressLifecycle()
	return true
}

// sendEventBudgetAdvisory is owned by the read goroutine. The single slot is
// consumed even when enqueue rejects, so backpressure cannot cause hot retries.
func (c *Client) sendEventBudgetAdvisory(ctx context.Context, class EventClass, retry time.Duration) bool {
	if c.Hub == nil || !c.Hub.eventBudgetConfig.Enabled || !c.ingressAuthorityLive(ctx) {
		return false
	}
	switch class {
	case EventClassPresence, EventClassProfile, EventClassServer:
	default:
		return false
	}
	now := time.Now()
	if c.eventAdmissionNow != nil {
		now = c.eventAdmissionNow()
	}
	if c.eventAdvisoryUsed && now.Sub(c.eventAdvisoryAt) < time.Second {
		return false
	}
	c.eventAdvisoryUsed = true
	c.eventAdvisoryAt = now
	frame := struct {
		Type string `json:"type"`
		Data struct {
			Code         string `json:"code"`
			EventClass   string `json:"event_class"`
			RetryAfterMS int64  `json:"retry_after_ms"`
		} `json:"data"`
	}{Type: "error"}
	frame.Data.Code = "ws_rate_limited"
	frame.Data.EventClass = class.String()
	frame.Data.RetryAfterMS = eventBudgetRetryMilliseconds(retry)
	data, err := json.Marshal(frame)
	if err != nil {
		log.Printf("WebSocket event budget advisory encoding failed")
		return false
	}
	if !c.ingressAuthorityLive(ctx) {
		return false
	}
	enqueued, _ := c.enqueueOutboundWithAuthority(ctx, data)
	return enqueued
}
