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

	"github.com/markdrogersjr/Concord/services/control-plane/internal/users"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// EventsDeleter is the narrow interface the handler needs. Keeping this as an
// interface lets tests substitute a stub without pulling the HTTP admin client.
type EventsDeleter interface {
	DeleteEventsForUser(ctx context.Context, clientID string) error
}

// Handler exposes the privacy erasure endpoints.
type Handler struct {
	sentry  EventsDeleter
	account users.AccountDeleter
	log     *logger.Logger
}

// NewHandler builds a Handler wired to the given Sentry admin backend
// and account-deletion service. The logger is used to surface
// upstream-deletion and DB failures so operators can diagnose issues
// that would otherwise be opaque 500/502s.
//
// Panics if sentry or account is nil — these are non-nullable
// dependencies that every handler method dereferences unconditionally;
// a nil value would surface as an opaque nil-pointer panic on the first
// request. Panicking at construction makes the mis-wiring obvious at
// startup rather than in production traffic.
//
// A nil logger IS allowed in tests that do not exercise the failure path;
// production callers must always pass a non-nil logger. The log field is
// only dereferenced behind a nil-check on the error path.
func NewHandler(sentry EventsDeleter, account users.AccountDeleter, log *logger.Logger) *Handler {
	if sentry == nil {
		panic("privacy.NewHandler: sentry EventsDeleter must not be nil")
	}
	if account == nil {
		panic("privacy.NewHandler: account AccountDeleter must not be nil")
	}
	return &Handler{sentry: sentry, account: account, log: log}
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
// Ordering: if clientId is present and well-formed, delete Sentry events
// FIRST (so a Sentry outage blocks erasure and the user can retry;
// the reverse order would orphan Sentry events tagged with a deleted
// user.id). Then perform the atomic DB erasure via the AccountDeleter,
// which cascades through every user_id-FK table configured with ON DELETE
// CASCADE and inserts a privacy-safe audit row.
func (h *Handler) EraseAccount(c *gin.Context) {
	userID := c.GetString("user_id")
	// Defense-in-depth: auth middleware is expected to have populated user_id
	// before this handler runs. If a future routing change exposed this
	// endpoint behind optional auth, DeleteAccount would attempt a
	// DELETE WHERE id = '' and the audit row would be written without a
	// meaningful subject. Fail closed instead.
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	var req eraseAccountRequest
	// clientId is optional per spec — accept empty bodies (io.EOF from the
	// JSON decoder) so clients that have no Sentry events to delete can
	// still hit this endpoint for account erasure.
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	sentryAttempted := false
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
		sentryAttempted = true
	}

	if err := h.account.DeleteAccount(c.Request.Context(), userID, sentryAttempted); err != nil {
		if errors.Is(err, users.ErrUserNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "account not found"})
			return
		}
		if h.log != nil {
			h.log.Error("erase-account: account deletion failed",
				"user_id", userID,
				"error", err,
			)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "account deletion failed"})
		return
	}

	c.Status(http.StatusNoContent)
}
