package sentry

import (
	sentrygo "github.com/getsentry/sentry-go"
	sentrygin "github.com/getsentry/sentry-go/gin"
	"github.com/gin-gonic/gin"
)

// CaptureWithTag captures an exception via the global hub with a categorical tag.
// Use for non-HTTP code (NATS subscribers, background workers, WebSocket goroutines).
func CaptureWithTag(err error, category, value string) {
	defer func() { recover() }() //nolint:errcheck // swallow SDK panics

	sentrygo.WithScope(func(scope *sentrygo.Scope) {
		scope.SetTag(category, value)
		sentrygo.CaptureException(err)
	})
}

// CaptureFromContext captures an exception via the per-request hub from sentrygin.
// Use inside Gin handlers (where *gin.Context is available).
func CaptureFromContext(c *gin.Context, err error, category, value string) {
	defer func() { recover() }() //nolint:errcheck // swallow SDK panics

	if c == nil {
		CaptureWithTag(err, category, value)
		return
	}

	if hub := sentrygin.GetHubFromContext(c); hub != nil {
		hub.WithScope(func(scope *sentrygo.Scope) {
			scope.SetTag(category, value)
			hub.CaptureException(err)
		})
	} else {
		// Fallback to global hub if sentrygin middleware didn't run
		CaptureWithTag(err, category, value)
	}
}
