// Package privacy provides GDPR Article 17 erasure handlers that bridge
// authenticated user requests to the Sentry admin API without persisting
// or logging the per-installation clientId.
package privacy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"

	"github.com/gin-gonic/gin"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// EventsDeleter is the narrow interface the handler needs. Keeping this as an
// interface lets tests substitute a stub without pulling the HTTP admin client.
type EventsDeleter interface {
	DeleteEventsForUser(ctx context.Context, clientID string) error
}

// Handler exposes the privacy erasure endpoints.
type Handler struct {
	sentry EventsDeleter
	log    *logger.Logger
}

// NewHandler builds a Handler wired to the given Sentry admin backend.
// The logger is used to surface upstream-deletion failures so operators
// can diagnose Sentry API issues that would otherwise be opaque 502s.
// A nil logger is allowed in tests that do not exercise the failure path;
// production callers must always pass a non-nil logger.
func NewHandler(sentry EventsDeleter, log *logger.Logger) *Handler {
	return &Handler{sentry: sentry, log: log}
}

type eraseTelemetryRequest struct {
	ClientID string `json:"clientId"`
}

// clientIDShape matches the two formats produced by client/desktop/src/main/clientId.ts:
//   - 32 lowercase hex chars (deterministic-hash format)
//   - "random-" prefix + 32 hex chars (per-launch fallback format)
var clientIDShape = regexp.MustCompile(`^(?:[a-f0-9]{32}|random-[a-f0-9]{32})$`)

// EraseTelemetry handles POST /api/v1/privacy/erase-telemetry.
// The clientId is NEVER logged by this handler. Access log lines only
// include user_id and endpoint — enforced by the logger middleware and by
// never calling logger.WithField("clientId", ...).
func (h *Handler) EraseTelemetry(c *gin.Context) {
	var req eraseTelemetryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if !clientIDShape.MatchString(req.ClientID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid clientId format"})
		return
	}
	if err := h.sentry.DeleteEventsForUser(c.Request.Context(), req.ClientID); err != nil {
		h.logUpstreamFailure(c, "erase-telemetry", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream deletion failed"})
		return
	}
	c.Status(http.StatusNoContent)
}

// clientIDLeakShape matches the two clientId formats that must never reach a
// log line: the deterministic 32-hex-char hash and the random-prefixed
// fallback. Defense-in-depth scrub applied to upstream error messages before
// they are logged — the upstream admin client is expected to sanitize first,
// but a regression there must never become a privacy violation.
//
// The regex intentionally omits \b anchors: clientIds can appear immediately
// adjacent to word characters in URL-encoded form (e.g. after "user.id%3A"),
// and \b would fail to match because the preceding hex byte of the percent-
// encoded colon is itself a word character. Accepting any 32-hex run is
// preferable to leaking; the scrub is privacy-critical, not presentation.
var clientIDLeakShape = regexp.MustCompile(`random-[a-f0-9]{32}|[a-f0-9]{32}`)

// logUpstreamFailure records a Sentry-admin failure at Error level so
// operators can diagnose token rotation, network, or Sentry API issues
// that would otherwise surface as opaque 502s. Per
// [internal]rules/observability.md, the clientId is NEVER logged — only
// the endpoint name, the user_id set by upstream auth middleware, and a
// scrubbed form of the typed error. Tolerates a nil logger so unit tests
// that do not exercise the failure path can construct a Handler without one.
func (h *Handler) logUpstreamFailure(c *gin.Context, endpoint string, err error) {
	if h.log == nil {
		return
	}
	scrubbed := clientIDLeakShape.ReplaceAllString(err.Error(), "<redacted-client-id>")
	h.log.Error("privacy erasure upstream call failed",
		"endpoint", endpoint,
		"user_id", c.GetString("user_id"),
		"error", scrubbed,
	)
}

type eraseAccountRequest struct {
	ClientID string `json:"clientId,omitempty"`
}

// EraseAccount handles POST /api/v1/privacy/erase-account.
// If clientId is present, internally calls Sentry delete first, then
// proceeds with account deletion. Account-deletion logic is intentionally
// out of scope for this handler — wire to the existing account-deletion
// flow via a follow-up.
func (h *Handler) EraseAccount(c *gin.Context) {
	var req eraseAccountRequest
	// clientId is optional per spec — accept empty bodies (io.EOF from the
	// JSON decoder) so clients that have no Sentry events to delete can still
	// hit this endpoint for account erasure. Matches the established pattern
	// used by sessions/friends/members/invites handlers.
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if req.ClientID != "" {
		if !clientIDShape.MatchString(req.ClientID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid clientId format"})
			return
		}
		if err := h.sentry.DeleteEventsForUser(c.Request.Context(), req.ClientID); err != nil {
			h.logUpstreamFailure(c, "erase-account", err)
			c.JSON(http.StatusBadGateway, gin.H{"error": "upstream deletion failed"})
			return
		}
	}
	// Follow-up: #669 tracks wiring to the new users.DeleteAccount service.
	// Current behavior: 204 after Sentry deletion succeeds (if clientId present)
	// or 204 immediately (if no clientId). Account deletion is out of scope
	// for #642 and greenfield work for #669.
	c.Status(http.StatusNoContent)
}
