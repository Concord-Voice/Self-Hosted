package middleware

import (
	"github.com/gin-gonic/gin"
)

// CORS returns a middleware that handles CORS.
// CORS headers are only set when the request includes a non-empty Origin
// that matches the allowlist. Requests without an Origin header (native
// clients, curl) pass through without CORS headers — browsers will simply
// not treat them as cross-origin. The "null" string origin (sandboxed
// iframes, data: URLs) is never reflected to prevent credential leaks.
func CORS(allowedOrigins []string) gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")

		// Non-browser clients omit Origin entirely — let them through
		// without any CORS headers. The "null" string origin is sent by
		// sandboxed iframes and data: URLs; never reflect it back.
		if origin == "" || origin == "null" {
			if c.Request.Method == "OPTIONS" {
				c.AbortWithStatus(204)
				return
			}
			c.Next()
			return
		}

		// Check if origin is allowed
		allowed := false
		for _, allowedOrigin := range allowedOrigins {
			if allowedOrigin == "*" || origin == allowedOrigin {
				allowed = true
				break
			}
		}

		if allowed {
			c.Writer.Header().Set("Access-Control-Allow-Origin", origin)
			c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
			c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With, X-Refresh-Token, X-Session-ID, X-Machine-Id, X-Device-Name")
			c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			c.Writer.Header().Set("Access-Control-Max-Age", "86400")
		}

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	}
}
