package middleware

import (
	"mime"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// LoginCSRF guards the public POST routes that can establish a refresh cookie.
func LoginCSRF(allowedOrigins []string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodPost || !loginCookieRoute(c.FullPath()) {
			c.Next()
			return
		}
		if status, code := loginCSRFRejection(c.Request, allowedOrigins); status != 0 {
			c.AbortWithStatusJSON(status, gin.H{"error": code})
			return
		}
		c.Next()
	}
}

func loginCSRFRejection(r *http.Request, allowedOrigins []string) (int, string) {
	if !hasJSONContentType(r) {
		return http.StatusUnsupportedMediaType, "unsupported_media_type"
	}
	if hasCrossSiteFetch(r) {
		return http.StatusForbidden, "cross_site_request"
	}
	if !hasAllowedLoginOrigin(r, allowedOrigins) {
		return http.StatusForbidden, "invalid_origin"
	}
	return 0, ""
}

func hasJSONContentType(r *http.Request) bool {
	contentTypes := r.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentTypes[0])
	return err == nil && mediaType == "application/json"
}

func hasCrossSiteFetch(r *http.Request) bool {
	for _, site := range r.Header.Values("Sec-Fetch-Site") {
		if strings.EqualFold(strings.TrimSpace(site), "cross-site") {
			return true
		}
	}
	return false
}

func hasAllowedLoginOrigin(r *http.Request, allowedOrigins []string) bool {
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) != 1 || origins[0] == "" || origins[0] == "null" || strings.Contains(origins[0], ",") {
		return false
	}
	for _, candidate := range allowedOrigins {
		if candidate != "*" && origins[0] == candidate {
			return true
		}
	}
	return false
}

func loginCookieRoute(path string) bool {
	switch path {
	case "/api/v1/auth/register/confirm",
		"/api/v1/auth/login",
		"/api/v1/auth/mfa/verify",
		"/api/v1/auth/sso/:provider/session",
		"/api/v1/auth/sso/:provider/complete-link",
		"/api/v1/auth/sso/:provider/complete-registration":
		return true
	default:
		return false
	}
}
