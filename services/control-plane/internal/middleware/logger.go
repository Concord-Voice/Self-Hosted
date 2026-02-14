package middleware

import (
	"time"

	"github.com/concord/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
)

// Logger returns a gin.HandlerFunc that logs requests
func Logger(log *logger.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		method := c.Request.Method

		c.Next()

		duration := time.Since(start)
		status := c.Writer.Status()

		log.Info("HTTP Request",
			"method", method,
			"path", path,
			"status", status,
			"duration", duration,
			"ip", c.ClientIP(),
		)
	}
}
