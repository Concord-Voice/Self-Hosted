package feedback

import (
	"context"
	"errors"
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
	maxCategoryBytes    = 200
	// maxLogsBytes is the per-field cap on the diagnostic log dump that
	// the client may submit. Lowered from 80_000 to 40_000 (Gitar
	// finding, PR #1547) so the assembled GitHub issue body stays under
	// maxBodyBytes for typical max-payload bug reports — GitHub's REST
	// API rejects issue creation when the body exceeds ~65,536 chars.
	maxLogsBytes = 40_000
	// maxBodyBytes is a hard cap on the assembled GitHub issue body.
	// Stays comfortably under GitHub's 65,536-char limit to leave room
	// for unexpected expansion (markdown rendering, escape sequences).
	// `buildIssue` truncates the logs tail when this cap would be
	// exceeded — never silently submits an oversize body that GitHub
	// would reject with 422.
	maxBodyBytes = 60_000

	// MaxRequestBytes is the total POST body size cap enforced at decode
	// time. Anything larger triggers 413 before we touch JSON parsing.
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
	AppVersion      string       `json:"appVersion"`
	Platform        string       `json:"platform"`
	MachineIDPrefix string       `json:"machineIdPrefix"`
	GPU             *gpuInfo     `json:"gpu,omitempty"`
	Display         *displayInfo `json:"display,omitempty"`
	ConnectionPhase string       `json:"connectionPhase"`
	Logs            string       `json:"logs"`
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
// enforced upstream via middleware.RateLimitByUser(redis, 10, 1*time.Hour) —
// raised from 3/hour per Gitar finding to reduce false lockouts from
// validation typos while still capping spam.
func (h *Handler) Submit(c *gin.Context) {
	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	// Size cap before we touch JSON parsing — protects the parser and
	// prevents large-body resource burn from a misbehaving client. When the
	// cap is exceeded, ShouldBindJSON below surfaces an *http.MaxBytesError
	// which we translate to HTTP 413 (not the generic 400) so the client
	// can distinguish "your payload is too big" from "your payload is
	// malformed."
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxRequestBytes)

	var req submitRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if err := validate(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Defense-in-depth PII re-sanitization. The threat model is a
	// compromised client that bypasses the renderer-side scrub at
	// services/logBufferService.ts — the server is the LAST line before
	// content reaches a public-ish GitHub issue, so every free-text
	// string the client sends gets scrubbed here, not just Logs.
	// (Gitar / @security-reviewer finding, PR #1547.)
	req.Title = Sanitize(req.Title)
	req.Description = Sanitize(req.Description)
	req.Category = Sanitize(req.Category)
	if req.Diagnostics != nil {
		req.Diagnostics.AppVersion = Sanitize(req.Diagnostics.AppVersion)
		req.Diagnostics.Platform = Sanitize(req.Diagnostics.Platform)
		req.Diagnostics.MachineIDPrefix = Sanitize(req.Diagnostics.MachineIDPrefix)
		req.Diagnostics.ConnectionPhase = Sanitize(req.Diagnostics.ConnectionPhase)
		if req.Diagnostics.GPU != nil {
			req.Diagnostics.GPU.Vendor = Sanitize(req.Diagnostics.GPU.Vendor)
			req.Diagnostics.GPU.Renderer = Sanitize(req.Diagnostics.GPU.Renderer)
		}
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
	// Category is optional, but when present must be bounded — the wire
	// schema (openapi.yaml /feedback) describes it as free-text for now;
	// #160 (Feature Request panel) will narrow it to a fixed enum.
	if len(req.Category) > maxCategoryBytes {
		return fmt.Errorf("category exceeds %d bytes", maxCategoryBytes)
	}
	if req.Diagnostics != nil && len(req.Diagnostics.Logs) > maxLogsBytes {
		return fmt.Errorf("logs exceed %d bytes", maxLogsBytes)
	}
	return nil
}

// neutralizeAutolinks inserts a zero-width space (U+200B) after Markdown
// control characters that the GitHub issue renderer would otherwise expand
// into autolinks or embedded content:
//
//   - `@username` / `@org/team` — notification spam (PII enumeration via
//     mentions, harassment via team-pings)
//   - `![alt](url)` — image embed; loads attacker-controlled URL with the
//     triager's browser when the issue is viewed (referrer leak + IP probe)
//   - `#123` — backlinks to unrelated issues/PRs in the feedback repo
//
// ZWSP is invisible in the rendered issue but breaks the autolink trigger
// pattern. Backtick / code-fence escapes are handled separately in
// writeDiagnostics — the logs block uses a dynamic fence length.
//
// (Gitar / @security-reviewer finding, PR #1547.)
func neutralizeAutolinks(s string) string {
	const zwsp = "\u200b"
	if !strings.ContainsAny(s, "@!#") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for i := 0; i < len(s); i++ {
		c := s[i]
		b.WriteByte(c)
		switch c {
		case '@':
			// Always neutralize — any `@`-prefixed token autolinks.
			b.WriteString(zwsp)
		case '!':
			// Only the `![` image syntax is dangerous; bare `!` is fine.
			if i+1 < len(s) && s[i+1] == '[' {
				b.WriteString(zwsp)
			}
		case '#':
			// `#123` / `#GH-123` / `#username/repo#123` all autolink.
			// Conservative: ZWSP when followed by an alphanumeric char.
			if i+1 < len(s) {
				next := s[i+1]
				if isAlphanumeric(next) {
					b.WriteString(zwsp)
				}
			}
		}
	}
	return b.String()
}

func isAlphanumeric(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// longestBacktickRun returns the length of the longest run of consecutive
// backticks in s. Used by writeDiagnostics to choose a code-fence length
// that exceeds any backtick sequence in the user-supplied logs — so the
// content cannot escape the fence and inject markdown / HTML into the
// surrounding issue body.
func longestBacktickRun(s string) int {
	maxRun := 0
	cur := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '`' {
			cur++
			if cur > maxRun {
				maxRun = cur
			}
		} else {
			cur = 0
		}
	}
	return maxRun
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
//
// User-supplied free text (Title, Description, Category) is passed through
// `neutralizeAutolinks` so @-mentions, image embeds, and #-issue-refs are
// rendered visibly but do NOT trigger GitHub's autolink expansions.
//
// If the assembled body would exceed `maxBodyBytes`, the logs section is
// truncated from the start (keeping the most recent log lines) until the
// body fits. The truncation is marked inline so triagers know it happened.
func buildIssue(req *submitRequest, userID string) (title, body string, labels []string) {
	var b strings.Builder

	switch req.Type {
	case reportTypeBug:
		title = "[Bug Report] " + neutralizeAutolinks(req.Title)
		labels = []string{"type: bug"}
		fmt.Fprintf(&b, "**Reported by:** user `%s` (internal ID)\n", userID)
		if req.Diagnostics != nil {
			fmt.Fprintf(&b, "**App Version:** %s\n", neutralizeAutolinks(req.Diagnostics.AppVersion))
			fmt.Fprintf(&b, "**Platform:** %s\n", neutralizeAutolinks(req.Diagnostics.Platform))
		}
		b.WriteString("\n## Description\n\n")
		b.WriteString(neutralizeAutolinks(req.Description))
		if req.Diagnostics != nil {
			writeDiagnostics(&b, req.Diagnostics)
		}

	case reportTypeFeature:
		title = "[Feature Request] " + neutralizeAutolinks(req.Title)
		labels = []string{"type: feature"}
		fmt.Fprintf(&b, "**Requested by:** user `%s` (internal ID)\n", userID)
		if req.Category != "" {
			fmt.Fprintf(&b, "**Category:** %s\n", neutralizeAutolinks(req.Category))
		}
		b.WriteString("\n## Description\n\n")
		b.WriteString(neutralizeAutolinks(req.Description))
	}

	body = b.String()
	if len(body) > maxBodyBytes {
		body = truncateBody(body)
	}
	return title, body, labels
}

// truncateBody applies a tail-only truncation when the assembled body would
// exceed GitHub's issue-body cap. It keeps the first maxBodyBytes-N bytes
// (the header + description) and a marker noting that the diagnostic logs
// were truncated. Returns a body of at most maxBodyBytes bytes.
//
// This is a belt-and-suspenders guard against the case where Description +
// metadata + a max-size Logs section overruns maxBodyBytes; in practice the
// per-field caps + writeDiagnostics's own log-tail truncation keep the body
// well under the limit. The marker is markdown that renders cleanly.
func truncateBody(body string) string {
	const marker = "\n\n_... (body truncated to fit GitHub issue-body limit) ..._\n"
	if len(body) <= maxBodyBytes {
		return body
	}
	keep := maxBodyBytes - len(marker)
	if keep < 0 {
		keep = 0
	}
	return body[:keep] + marker
}

// writeDiagnostics emits the optional "Diagnostics" section in the bug-report
// body. Only fields the client provided are rendered; missing optional fields
// (GPU, display) are silently omitted rather than rendered as "unknown".
//
// User-supplied free-text inside metadata bullets is rendered after
// `neutralizeAutolinks` so a hostile client cannot smuggle `@org` or `![pwn]`
// payloads through (e.g.) the GPU vendor string.
//
// The logs block uses a fence whose length exceeds any backtick run in the
// content, so the user-supplied logs cannot escape the fenced code block to
// inject markdown into the surrounding body.
func writeDiagnostics(b *strings.Builder, d *diagnostics) {
	b.WriteString("\n\n## Diagnostics\n\n")
	fmt.Fprintf(b, "- **Machine ID prefix:** `%s`\n", neutralizeAutolinks(d.MachineIDPrefix))
	fmt.Fprintf(b, "- **Connection phase:** %s\n", neutralizeAutolinks(d.ConnectionPhase))
	if d.GPU != nil {
		fmt.Fprintf(b, "- **GPU:** %s / %s\n",
			neutralizeAutolinks(d.GPU.Vendor),
			neutralizeAutolinks(d.GPU.Renderer))
	}
	if d.Display != nil {
		// Display dimensions are integer/float types — no markdown-injection
		// surface here, so they don't go through neutralizeAutolinks.
		fmt.Fprintf(b, "- **Display:** %dx%d @ %.2fx", d.Display.Width, d.Display.Height, d.Display.ScaleFactor)
		if d.Display.RefreshRate > 0 {
			fmt.Fprintf(b, " (%.0fHz)", d.Display.RefreshRate)
		}
		b.WriteString("\n")
	}
	if d.Logs != "" {
		fenceLen := longestBacktickRun(d.Logs) + 1
		if fenceLen < 3 {
			fenceLen = 3
		}
		fence := strings.Repeat("`", fenceLen)
		b.WriteString("\n### Recent logs (sanitized)\n\n")
		b.WriteString(fence)
		b.WriteString("\n")
		b.WriteString(d.Logs)
		b.WriteString("\n")
		b.WriteString(fence)
		b.WriteString("\n")
	}
}
