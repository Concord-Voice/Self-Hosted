package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestLoginCSRFGuard(t *testing.T) {
	const allowed = "https://app.example.test:8443"
	r := gin.New()
	auth := r.Group("/api/v1/auth")
	auth.Use(middleware.LoginCSRF([]string{allowed, "*", "null", allowed + ", https://evil.example"}))
	paths := []string{
		"/register/confirm", "/login", "/mfa/verify",
		"/sso/:provider/session", "/sso/:provider/complete-link", "/sso/:provider/complete-registration",
	}
	for _, path := range paths {
		auth.POST(path, func(c *gin.Context) { c.Status(http.StatusNoContent) })
	}
	for _, path := range []string{"/refresh", "/logout", "/register", "/recovery/begin", "/sso/:provider/sign-client-secret"} {
		auth.POST(path, func(c *gin.Context) { c.Status(http.StatusNoContent) })
	}
	r.POST("/admin/api/v1/auth/webauthn", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	cases := []struct {
		name, contentType, fetchSite string
		origins                      []string
		want                         int
		code                         string
	}{
		{"native JSON", "application/json", "", nil, 204, ""},
		{"exact origin with parameter", "application/json; charset=utf-8", "same-origin", []string{allowed}, 204, ""},
		{"missing type", "", "", nil, 415, "unsupported_media_type"},
		{"plain text", "text/plain", "", nil, 415, "unsupported_media_type"},
		{"form", "application/x-www-form-urlencoded", "", nil, 415, "unsupported_media_type"},
		{"multipart", "multipart/form-data; boundary=x", "", nil, 415, "unsupported_media_type"},
		{"malformed type", "application/json; charset=", "", nil, 415, "unsupported_media_type"},
		{"cross-site", "application/json", "CrOsS-SiTe", nil, 403, "cross_site_request"},
		{"foreign origin", "application/json", "", []string{"https://evil.example"}, 403, "invalid_origin"},
		{"wildcard is not a browser origin", "application/json", "", []string{"*"}, 403, "invalid_origin"},
		{"null origin", "application/json", "", []string{"null"}, 403, "invalid_origin"},
		{"prefix origin", "application/json", "", []string{allowed + ".evil"}, 403, "invalid_origin"},
		{"wrong port", "application/json", "", []string{"https://app.example.test:8444"}, 403, "invalid_origin"},
		{"joined origins", "application/json", "", []string{allowed + ", https://evil.example"}, 403, "invalid_origin"},
		{"duplicate origins", "application/json", "", []string{allowed, "https://evil.example"}, 403, "invalid_origin"},
		{"empty origin", "application/json", "", []string{""}, 403, "invalid_origin"},
	}
	for _, path := range paths {
		actual := strings.Replace(path, ":provider", "google", 1)
		for _, tc := range cases {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodPost, "/api/v1/auth"+actual, strings.NewReader(`{}`))
				if tc.contentType != "" {
					req.Header.Set("Content-Type", tc.contentType)
				}
				if tc.fetchSite != "" {
					req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
				}
				for _, origin := range tc.origins {
					req.Header.Add("Origin", origin)
				}
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				require.Equal(t, tc.want, w.Code, w.Body.String())
				if tc.code != "" {
					require.Contains(t, w.Body.String(), `"error":"`+tc.code+`"`)
				}
			})
		}
	}
	for _, path := range []string{"/refresh", "/logout", "/register", "/recovery/begin", "/sso/google/sign-client-secret"} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth"+path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusNoContent, w.Code, path)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/v1/auth/webauthn", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusNoContent, w.Code)
}

func TestLoginCSRFPreflightStaysWithCORS(t *testing.T) {
	r := gin.New()
	r.Use(middleware.CORS([]string{"https://app.example.test"}))
	auth := r.Group("/api/v1/auth")
	auth.Use(middleware.LoginCSRF([]string{"https://app.example.test"}))
	auth.POST("/login", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	req := httptest.NewRequest(http.MethodOptions, "/api/v1/auth/login", nil)
	req.Header.Set("Origin", "https://app.example.test")
	req.Header.Set("Access-Control-Request-Method", "POST")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Equal(t, "https://app.example.test", w.Header().Get("Access-Control-Allow-Origin"))
}
