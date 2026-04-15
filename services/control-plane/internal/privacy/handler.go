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
)

// EventsDeleter is the narrow interface the handler needs. Keeping this as an
// interface lets tests substitute a stub without pulling the HTTP admin client.
type EventsDeleter interface {
	DeleteEventsForUser(ctx context.Context, clientID string) error
}

// Handler exposes the privacy erasure endpoints.
type Handler struct {
	sentry EventsDeleter
}

// NewHandler builds a Handler wired to the given Sentry admin backend.
func NewHandler(sentry EventsDeleter) *Handler {
	return &Handler{sentry: sentry}
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
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream deletion failed"})
		return
	}
	c.Status(http.StatusNoContent)
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
