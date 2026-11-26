package admin

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/securityevent"
)

// adminCreateRateLimitTimeout bounds each limiter Redis call, as the shared
// limiter does, so a hung backend gets the fixed fail-closed response.
const adminCreateRateLimitTimeout = 2 * time.Second

// adminCreateRateLimitKey names one bucket: the acting admin, the client IP
// bucket, the method and the route. Keying on the admin as well as the IP
// means a session of another admin behind the same NAT or proxy cannot spend
// this admin's budget. Admin ids are UUIDs, so they cannot contain ':'.
func adminCreateRateLimitKey(c *gin.Context, adminID string) string {
	return fmt.Sprintf("ratelimit:admin:%s:ip:%s:%s:%s",
		adminID, middleware.ClientIPBucket(c), c.Request.Method, c.FullPath())
}

// rateLimitAdminCreate limits the admin-create routes to requests per window
// per (admin, IP bucket, method, route). It runs after AdminAuthRequired and
// fails closed: no admin id in the context, or a limiter backend that cannot
// count, gets adminRateLimitUnavailable and never reaches the handler. A
// stolen session of the same admin still shares that admin's bucket for its
// IP; that is accepted, since the step-up, not the limit, is what stops it.
func rateLimitAdminCreate(rdb *redis.Client, requests int, window time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		adminID := c.GetString(adminIDContextKey)
		if adminID == "" {
			c.Header(middleware.RetryAfterHeader, "1")
			adminRateLimitUnavailable(c)
			return
		}
		key := adminCreateRateLimitKey(c, adminID)
		ctx, cancel := context.WithTimeout(context.Background(), adminCreateRateLimitTimeout)
		defer cancel()
		allowed, err := middleware.AllowUserAction(ctx, rdb, key, requests, window)
		if err != nil {
			middleware.MarkNightwatchVerdict(c, middleware.NightwatchVerdict{
				EventType: securityevent.EventSecurityControl, Outcome: securityevent.OutcomeDegraded,
				Severity: securityevent.SeverityMedium, Reason: securityevent.ReasonRateLimitBackendUnavailable,
			})
			c.Header(middleware.RetryAfterHeader, "1")
			adminRateLimitUnavailable(c)
			return
		}
		if !allowed {
			middleware.MarkNightwatchVerdict(c, middleware.NightwatchVerdict{
				EventType: securityevent.EventSecurityControl, Outcome: securityevent.OutcomeDenied,
				Severity: securityevent.SeverityMedium, Reason: securityevent.ReasonRateLimitExceeded,
			})
			retryAfter := window
			if ttl, ttlErr := rdb.TTL(ctx, key).Result(); ttlErr == nil && ttl > 0 {
				retryAfter = ttl
			}
			c.Header(middleware.RetryAfterHeader, strconv.Itoa(int(retryAfter.Seconds())))
			adminRateLimited(c)
			return
		}
		c.Next()
	}
}
