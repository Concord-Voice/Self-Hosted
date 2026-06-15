package feedback

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

const (
	// Per-field length caps mirror the client-side validation in
	// FeedbackModal / panels. The server still enforces them as defense
	// against a compromised client. Bytes, not runes — we cap below
	// MaxRequestBytes to keep the budget predictable.
	maxTitleBytes       = 200
	maxDescriptionBytes = 8000
	maxLogsBytes        = 80_000 // ring buffer is 500 entries; this caps the dump

	// MaxRequestBytes is the total POST body size cap enforced at decode
	// time. Anything larger triggers a 413 before we touch JSON parsing.
	MaxRequestBytes = 128 * 1024
)

// reportType is the discriminator the client sets. The handler dispatches
// the GitHub issue title / body / labels based on this value.
type reportType string

const (
	reportTypeBug     reportType = "bug"
	reportTypeFeature reportType = "feature"
)

// submitRequest is the JSON shape the client POSTs. Fields not listed are
// ignored — we do NOT pass arbitrary keys through to GitHub.
type submitRequest struct {
	Type        reportType   `json:"type"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	Category    string       `json:"category,omitempty"`
	Diagnostics *diagnostics `json:"diagnostics,omitempty"`
}

type diagnostics struct {
	AppVersion      string     `json:"appVersion"`
	Platform        string     `json:"platform"`
	MachineIDPrefix string     `json:"machineIdPrefix"`
	GPU             *gpuInfo   `json:"gpu,omitempty"`
	Display         *displayInfo `json:"display,omitempty"`
	ConnectionPhase string     `json:"connectionPhase"`
	Logs            string     `json:"logs"`
}

type gpuInfo struct {
	Vendor   string `json:"vendor"`
	Renderer string `json:"renderer"`
}

type displayInfo struct {
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	RefreshRate float64 `json:"refreshRate,omitempty"`
	ScaleFactor float64 `json:"scaleFactor"`
}

// submitResponse is what the client gets back. `IssueURL` is empty on the
// dev-stub path so the client knows the submission was recorded but no
// GitHub issue was actually created. `Dev` is the explicit flag so the
// client can render a clarifying message in the success surface.
type submitResponse struct {
	IssueURL string `json:"issueUrl,omitempty"`
	Dev      bool   `json:"dev"`
}

// GitHubIssueCreator is the narrow interface handlers.Handler depends on so
// tests can swap in a fake. The production implementation is *Client in
// github.go.
type GitHubIssueCreator interface {
	CreateIssue(ctx context.Context, req IssueRequest) (*IssueResponse, error)
}

// Handler owns the POST /api/v1/feedback endpoint.
//
// Dev stub: when `github == nil` the handler logs the assembled issue body
// and returns `{dev: true}` without making any network call. This is the
// stub-in-dev / hard-fail-in-prod posture — config.go's production guard
// fatal-exits before we ever construct a nil-github Handler in production.
type Handler struct {
	log    *logger.Logger
	github GitHubIssueCreator
}

// NewHandler builds a Handler. Pass `nil` for `github` in dev to enable the
// log-and-skip stub.
func NewHandler(log *logger.Logger, github GitHubIssueCreator) *Handler {
	return &Handler{log: log, github: github}
}

// Submit handles POST /api/v1/feedback. Requires the AuthRequired
// middleware upstream so `user_id` is on the Gin context. Rate limit is
// enforced upstream via middleware.RateLimitByUser(redis, 3, 1*time.Hour).
func (h *Handler) Submit(c *gin.Context) {
	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	// Size cap before we touch JSON parsing — protects the parser and
	// prevents large-body resource burn from a misbehaving client.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxRequestBytes)

	var req submitRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if err := validate(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Sanitize user-supplied free-text fields. The user's own title /
	// description are scrubbed too — they can legitimately paste a
	// stack trace, a path, or an email into the description, and that
	// must not survive to the public-ish GitHub issue.
	req.Title = Sanitize(req.Title)
	req.Description = Sanitize(req.Description)
	if req.Diagnostics != nil {
		req.Diagnostics.Logs = Sanitize(req.Diagnostics.Logs)
	}

	title, body, labels := buildIssue(&req, userID)

	// Dev stub: log the body and return success without calling GitHub.
	if h.github == nil {
		h.log.Info("feedback.submit (dev stub — no GitHub call)",
			"type", req.Type,
			"user_id", userID,
			"title", title,
			"label_count", len(labels),
			"body_bytes", len(body),
		)
		c.JSON(http.StatusOK, submitResponse{Dev: true})
		return
	}

	// Production path: post to GitHub. 12s budget (room under the client's
	// http.Client 15s above and the typical client UI timeout).
	ctx, cancel := context.WithTimeout(c.Request.Context(), 12*time.Second)
	defer cancel()

	issue, err := h.github.CreateIssue(ctx, IssueRequest{
		Title:  title,
		Body:   body,
		Labels: labels,
	})
	if err != nil {
		h.log.Warn("feedback.submit: GitHub create failed", "error", err.Error(), "user_id", userID)
		c.JSON(http.StatusBadGateway, gin.H{"error": "feedback submission failed; please try again later"})
		return
	}

	c.JSON(http.StatusOK, submitResponse{IssueURL: issue.HTMLURL, Dev: false})
}

// validate runs cheap structural checks on the parsed request. Heavier
// content checks (sanitization, body assembly) happen after.
func validate(req *submitRequest) error {
	switch req.Type {
	case reportTypeBug, reportTypeFeature:
		// ok
	default:
		return fmt.Errorf("invalid report type")
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		return fmt.Errorf("title is required")
	}
	if len(title) > maxTitleBytes {
		return fmt.Errorf("title exceeds %d bytes", maxTitleBytes)
	}
	desc := strings.TrimSpace(req.Description)
	if desc == "" {
		return fmt.Errorf("description is required")
	}
	if len(desc) > maxDescriptionBytes {
		return fmt.Errorf("description exceeds %d bytes", maxDescriptionBytes)
	}
	if req.Diagnostics != nil && len(req.Diagnostics.Logs) > maxLogsBytes {
		return fmt.Errorf("logs exceed %d bytes", maxLogsBytes)
	}
	return nil
}

// buildIssue assembles the GitHub issue title/body/labels from the
// (already-validated, already-sanitized) request.
//
// The body is rendered with explicit section headers so triagers reading
// the GitHub issue have a consistent layout regardless of which mode the
// user picked. The "Reported by" / "Requested by" line carries the user's
// internal ID for follow-up correlation — it never reaches the public
// issue body's user-visible content (GitHub issues are public-ish; the body
// IS visible to anyone with repo read access, hence the dedicated feedback
// repo per the architectural decision recorded in the PR body).
func buildIssue(req *submitRequest, userID string) (title, body string, labels []string) {
	var b strings.Builder

	switch req.Type {
	case reportTypeBug:
		title = "[Bug Report] " + req.Title
		labels = []string{"type: bug"}
		fmt.Fprintf(&b, "**Reported by:** user `%s` (internal ID)\n", userID)
		if req.Diagnostics != nil {
			fmt.Fprintf(&b, "**App Version:** %s\n", req.Diagnostics.AppVersion)
			fmt.Fprintf(&b, "**Platform:** %s\n", req.Diagnostics.Platform)
		}
		b.WriteString("\n## Description\n\n")
		b.WriteString(req.Description)
		if req.Diagnostics != nil {
			writeDiagnostics(&b, req.Diagnostics)
		}

	case reportTypeFeature:
		title = "[Feature Request] " + req.Title
		labels = []string{"type: feature"}
		fmt.Fprintf(&b, "**Requested by:** user `%s` (internal ID)\n", userID)
		if req.Category != "" {
			fmt.Fprintf(&b, "**Category:** %s\n", req.Category)
		}
		b.WriteString("\n## Description\n\n")
		b.WriteString(req.Description)
	}

	body = b.String()
	return title, body, labels
}

// writeDiagnostics emits the optional "Diagnostics" section in the bug-report
// body. Only fields the client provided are rendered; missing optional fields
// (GPU, display) are silently omitted rather than rendered as "unknown".
func writeDiagnostics(b *strings.Builder, d *diagnostics) {
	b.WriteString("\n\n## Diagnostics\n\n")
	fmt.Fprintf(b, "- **Machine ID prefix:** `%s`\n", d.MachineIDPrefix)
	fmt.Fprintf(b, "- **Connection phase:** %s\n", d.ConnectionPhase)
	if d.GPU != nil {
		fmt.Fprintf(b, "- **GPU:** %s / %s\n", d.GPU.Vendor, d.GPU.Renderer)
	}
	if d.Display != nil {
		fmt.Fprintf(b, "- **Display:** %dx%d @ %.2fx", d.Display.Width, d.Display.Height, d.Display.ScaleFactor)
		if d.Display.RefreshRate > 0 {
			fmt.Fprintf(b, " (%.0fHz)", d.Display.RefreshRate)
		}
		b.WriteString("\n")
	}
	if d.Logs != "" {
		b.WriteString("\n### Recent logs (sanitized)\n\n```\n")
		b.WriteString(d.Logs)
		b.WriteString("\n```\n")
	}
}
