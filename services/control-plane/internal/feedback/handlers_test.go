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

// ─── helpers ──────────────────────────────────────────────────────────────

type fakeError string

func (e fakeError) Error() string { return string(e) }

func assertGitHubError(msg string) error { return fakeError("github: " + msg) }
