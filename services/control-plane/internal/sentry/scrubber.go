// Package sentry provides a fail-closed BeforeSend scrubber for the Go control-plane
// Sentry integration, enforcing an allowlist model over contexts, tags, and event fields
// to prevent PII, key material, and credential leakage.
package sentry

import (
	"regexp"
	"strings"

	sentrygo "github.com/getsentry/sentry-go"
)

// AllowedContexts are the only context keys that survive scrubbing.
var AllowedContexts = map[string]struct{}{
	"os":      {},
	"runtime": {},
	"app":     {},
}

// AllowedTagKeys are the only tag keys that survive scrubbing. Categorical only —
// NEVER includes identifier tags (room.id, user.id, channel.id, server.id).
var AllowedTagKeys = map[string]struct{}{
	"environment":     {},
	"release":         {},
	"level":           {},
	"transaction":     {},
	"handled":         {},
	"http.method":     {},
	"http.route":      {},
	"nats.subject":    {},
	"db.operation":    {},
	"websocket.event": {},
	"worker.job":      {},
}

var sensitivePatterns = []*regexp.Regexp{
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)?`),
	regexp.MustCompile(`[A-Za-z0-9+/=_-]{64,}`),
	regexp.MustCompile(`(?i)\b[a-f0-9]{64}\b`),
	regexp.MustCompile(`[\w.+-]+@[\w-]+\.[\w.]+`),
	regexp.MustCompile(`postgres(?:ql)?://[^\s]+`),
	regexp.MustCompile(`redis://[^\s]+`),
}

// RedactSensitivePatterns replaces known sensitive patterns with [REDACTED].
func RedactSensitivePatterns(message string) string {
	scrubbed := message
	for _, pattern := range sensitivePatterns {
		scrubbed = pattern.ReplaceAllString(scrubbed, "[REDACTED]")
	}
	return scrubbed
}

// scrubEvent is the BeforeSend hook — fail-closed allowlist model.
func scrubEvent(event *sentrygo.Event, _ *sentrygo.EventHint) *sentrygo.Event {
	if event == nil {
		return nil
	}

	// Strip unknown contexts
	for key := range event.Contexts {
		if _, ok := AllowedContexts[key]; !ok {
			delete(event.Contexts, key)
		}
	}

	// Wipe extra, request, and breadcrumbs
	event.Extra = nil
	event.Request = nil
	event.Breadcrumbs = nil

	// Reduce user to redacted ID only
	if event.User.ID != "" || event.User.Email != "" || event.User.IPAddress != "" || event.User.Username != "" {
		event.User = sentrygo.User{
			ID: RedactSensitivePatterns(event.User.ID),
		}
	}

	// Strip unknown tags
	for key := range event.Tags {
		if _, ok := AllowedTagKeys[key]; !ok {
			delete(event.Tags, key)
		}
	}

	// Final paranoia: drop events containing a DSN (check before redaction)
	for _, exc := range event.Exception {
		if strings.Contains(exc.Value, "ingest.sentry.io") {
			return nil
		}
	}

	// Redact exception values
	for i := range event.Exception {
		event.Exception[i].Value = RedactSensitivePatterns(event.Exception[i].Value)
	}

	// Redact message
	if event.Message != "" {
		event.Message = RedactSensitivePatterns(event.Message)
	}

	return event
}
