package middleware

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// RateLimitConfig defines rate limiting parameters
type RateLimitConfig struct {
	Requests int           // Number of requests allowed
	Window   time.Duration // Time window for the limit
	KeyFunc  func(*gin.Context) string // Function to generate rate limit key
}

// RateLimitByIP creates a rate limiter based on IP address
func RateLimitByIP(redis *redis.Client, requests int, window time.Duration) gin.HandlerFunc {
	config := RateLimitConfig{
		Requests: requests,
		Window:   window,
		KeyFunc: func(c *gin.Context) string {
			return fmt.Sprintf("ratelimit:ip:%s:%s:%s", c.ClientIP(), c.Request.Method, c.FullPath())
		},
	}
	return rateLimit(redis, config)
}

// RateLimitByUser creates a rate limiter based on user ID (for authenticated routes)
func RateLimitByUser(redis *redis.Client, requests int, window time.Duration) gin.HandlerFunc {
	config := RateLimitConfig{
		Requests: requests,
		Window:   window,
		KeyFunc: func(c *gin.Context) string {
			userID, exists := c.Get("user_id")
			if !exists {
				// Fall back to IP if user not authenticated
				return fmt.Sprintf("ratelimit:ip:%s:%s:%s", c.ClientIP(), c.Request.Method, c.FullPath())
			}
			return fmt.Sprintf("ratelimit:user:%v:%s:%s", userID, c.Request.Method, c.FullPath())
		},
	}
	return rateLimit(redis, config)
}

// rateLimit is the core rate limiting middleware
func rateLimit(redis *redis.Client, config RateLimitConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := context.Background()
		key := config.KeyFunc(c)

		// Increment counter
		count, err := redis.Incr(ctx, key).Result()
		if err != nil {
			// If Redis is down, fail open (allow request) to prevent total outage
			c.Next()
			return
		}

		// Set expiry on first request OR if key somehow lost its TTL
		// This prevents keys from persisting indefinitely if Expire fails
		ttl, err := redis.TTL(ctx, key).Result()
		if err != nil || ttl == -1 || count == 1 {
			// TTL is -1 when key exists but has no expiry
			// Always set expiry to ensure window resets properly
			redis.Expire(ctx, key, config.Window)
			ttl = config.Window
		}

		// Set rate limit headers
		c.Header("X-RateLimit-Limit", fmt.Sprintf("%d", config.Requests))
		c.Header("X-RateLimit-Remaining", fmt.Sprintf("%d", maxInt(0, config.Requests-int(count))))
		c.Header("X-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(ttl).Unix()))

		// Check if rate limit exceeded
		if count > int64(config.Requests) {
			c.Header("Retry-After", fmt.Sprintf("%d", int(ttl.Seconds())))
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error":   "Rate limit exceeded",
				"message": fmt.Sprintf("Too many requests. Please try again in %d seconds.", int(ttl.Seconds())),
			})
			c.Abort()
			return
		}

		c.Next()
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
