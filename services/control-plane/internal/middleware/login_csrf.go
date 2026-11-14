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

		contentTypes := c.Request.Header.Values("Content-Type")
		if len(contentTypes) != 1 {
			c.AbortWithStatusJSON(http.StatusUnsupportedMediaType, gin.H{"error": "unsupported_media_type"})
			return
		}
		mediaType, _, err := mime.ParseMediaType(contentTypes[0])
		if err != nil || mediaType != "application/json" {
			c.AbortWithStatusJSON(http.StatusUnsupportedMediaType, gin.H{"error": "unsupported_media_type"})
			return
		}

		for _, site := range c.Request.Header.Values("Sec-Fetch-Site") {
			if strings.EqualFold(strings.TrimSpace(site), "cross-site") {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "cross_site_request"})
				return
			}
		}

		origins := c.Request.Header.Values("Origin")
		if len(origins) > 0 {
			allowed := false
			if len(origins) == 1 && origins[0] != "" && origins[0] != "null" && !strings.Contains(origins[0], ",") {
				for _, candidate := range allowedOrigins {
					if candidate != "*" && origins[0] == candidate {
						allowed = true
						break
					}
				}
			}
			if !allowed {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "invalid_origin"})
				return
			}
		}
		c.Next()
	}
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
