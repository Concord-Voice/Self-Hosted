package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestNewRouterMediaUnconfiguredFallbacks pins the 503 fallbacks NewRouter
// registers when no media store is configured: the protected upload, download
// and delete routes, and the public feedback-screenshot GET that
// TestNewRouterDMIconPublicMediaRegression does not cover. Each row pins the
// method, path, status and body, so a dropped fallback route (404) or a moved
// one (a different status or body) fails here.
func TestNewRouterMediaUnconfiguredFallbacks(t *testing.T) {
	f := newDMIconRouterFixture(t, false)
	f.controls(t)
	const unavailable = `{"error":"media storage not configured"}`
	const fileID = "22222222-2222-4222-8222-222222222222"
	cases := []struct {
		method, path string
		headers      http.Header
		status       int
		body         string
	}{
		{http.MethodPost, "/api/v1/media/upload/avatar", f.member.headers, http.StatusServiceUnavailable, unavailable},
		{http.MethodPost, "/api/v1/media/upload/banner", f.member.headers, http.StatusServiceUnavailable, unavailable},
		{http.MethodPost, "/api/v1/media/upload/server-icon", f.member.headers, http.StatusServiceUnavailable, unavailable},
		{http.MethodPost, "/api/v1/media/upload/server-banner", f.member.headers, http.StatusServiceUnavailable, unavailable},
		{http.MethodPost, "/api/v1/media/upload/dm-icon", f.member.headers, http.StatusServiceUnavailable, unavailable},
		{http.MethodPost, "/api/v1/media/upload/feedback-screenshot", f.member.headers, http.StatusServiceUnavailable, unavailable},
		{http.MethodPost, "/api/v1/media/upload/attachment", f.member.headers, http.StatusServiceUnavailable, unavailable},
		{http.MethodGet, "/api/v1/media/attachments/" + fileID, f.member.headers, http.StatusServiceUnavailable, unavailable},
		{http.MethodDelete, "/api/v1/media/" + fileID, f.member.headers, http.StatusServiceUnavailable, unavailable},
		{http.MethodGet, "/api/v1/media/feedback-screenshots/" + fileID, nil, http.StatusServiceUnavailable, unavailable},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.headers != nil {
				req.Header = tc.headers.Clone()
			}
			w := httptest.NewRecorder()
			f.router.ServeHTTP(w, req)
			assert.Equal(t, tc.status, w.Code, "%s %s: %s", tc.method, tc.path, w.Body.String())
			assert.JSONEq(t, tc.body, w.Body.String())
		})
	}
}
