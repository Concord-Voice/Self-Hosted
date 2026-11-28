package admin

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/opsmetrics"
)

// Admin auth rate-limit budgets (per IP). The password/webauthn/enroll steps are
// the brute-force surface; lockout (per-account + per-IP, Task 9) is the primary
// velocity control, and RateLimitByIP is the coarse outer cap that stops a flood
// from ever reaching the lockout bookkeeping.
const (
	adminAuthRateLimit     = 20
	adminAuthRateWindow    = 15 * time.Minute
	adminEnrollRateLimit   = 10
	adminMetricsRateLimit  = 60
	adminMetricsRateWindow = time.Minute
	// Each Server read runs a root command through the host agent and writes an
	// audit row, so the budget is tighter than the metrics one.
	adminServerRateLimit = 30
	// Creating an admin is rare and mints a step-up-capable identity, so its
	// budget is the tightest (VULN-139). It is counted per acting admin and
	// client IP, so another admin behind the same address cannot spend it.
	// The step-up begin route has its own bucket of the same size.
	adminCreateRateLimit  = 5
	adminCreateRateWindow = time.Hour
)

var adminMetricsDeniedMethods = []string{
	http.MethodPost,
	http.MethodPut,
	http.MethodPatch,
	http.MethodDelete,
}

// RegisterRoutes mounts the admin auth surface under the given router group
// (typically the bare engine, since admin lives at top-level `/admin`, NOT under
// `/api/v1`). Pre-auth routes (password/webauthn/logout/enroll) carry per-IP rate
// limiting; everything else is gated by AdminAuthRequired (deny-by-default,
// verified by the reflective routes_test).
//
// rdb is needed for the RateLimitByIP middleware; h.sessions backs
// AdminAuthRequired; metrics owns only the closed read response surface.
func RegisterRoutes(rg *gin.RouterGroup, h *Handler, metrics *opsmetrics.AdminHandler, rdb *redis.Client, ui *UI) {
	if ui == nil {
		ui = NewUI(nil)
	}
	grp := rg.Group("/admin")

	grp.GET("/", ui.Index)
	grp.GET("/enroll", ui.Index)
	grp.GET("/assets/*filepath", ui.Asset)

	// --- Pre-auth API routes (rate-limited; the token/password/key IS the auth) ---
	api := grp.Group("/api/v1")
	{
		authRoutes := api.Group("/auth")
		authRoutes.Use(middleware.RateLimitByIPFailClosed(rdb, adminAuthRateLimit, adminAuthRateWindow))
		{
			authRoutes.POST("/password", h.PasswordLogin)
			authRoutes.POST("/webauthn", h.WebAuthnLogin)
			authRoutes.POST("/logout", h.Logout)
		}

		enrollRoutes := api.Group("/enroll")
		enrollRoutes.Use(middleware.RateLimitByIPFailClosed(rdb, adminEnrollRateLimit, adminAuthRateWindow))
		{
			enrollRoutes.POST("/begin", h.EnrollBegin)
			enrollRoutes.POST("/finish", h.EnrollFinish)
		}

		// --- Gated routes (AdminAuthRequired — deny-by-default) ---
		gated := api.Group("/")
		gated.Use(AdminAuthRequired(h.sessions, h.repo))
		{
			// Fail closed: a limiter that cannot count refuses the request.
			adminRoutes := gated.Group("/admins")
			adminRoutes.Use(rateLimitAdminCreate(rdb, adminCreateRateLimit, adminCreateRateWindow))
			{
				adminRoutes.POST("", h.CreateAdmin)
				adminRoutes.POST("/stepup", h.CreateAdminStepUpBegin)
			}
			// The signed-in admin's own keys: list them, or add a backup
			// after a fresh step-up with an existing key.
			credentialRoutes := gated.Group("/credentials")
			credentialRoutes.Use(middleware.RateLimitByIPFailClosedWithHandlers(
				rdb,
				adminAuthRateLimit,
				adminAuthRateWindow,
				adminRateLimited,
				adminRateLimitUnavailable,
			))
			{
				credentialRoutes.GET("", h.ListOwnCredentials)
				credentialRoutes.POST("/stepup", h.CredentialStepUpBegin)
				credentialRoutes.POST("/register/begin", h.CredentialRegisterBegin)
				credentialRoutes.POST("/register/finish", h.CredentialRegisterFinish)
			}
			noticeRoutes := gated.Group("/signin-notice")
			noticeRoutes.Use(middleware.RateLimitByIPFailClosedWithHandlers(
				rdb,
				adminAuthRateLimit,
				adminAuthRateWindow,
				adminRateLimited,
				adminRateLimitUnavailable,
			))
			noticeRoutes.POST("/ack", h.AcknowledgeSignInNotice)
			serverRoutes := gated.Group("/server")
			serverRoutes.Use(middleware.RateLimitByIPFailClosedWithHandlers(
				rdb,
				adminServerRateLimit,
				adminMetricsRateWindow,
				adminRateLimited,
				adminRateLimitUnavailable,
			))
			{
				serverRoutes.GET("/status", h.server.Status)
				serverRoutes.GET("/health", h.server.Health)
				serverRoutes.GET("/logs", h.server.Logs)
				serverRoutes.POST("/stepup", h.ServerStepUpBegin)
				serverRoutes.POST("/actions/:action", h.ServerAction)
				serverRoutes.GET("/jobs", h.server.ServerJobs)
				serverRoutes.GET("/jobs/:id", h.server.ServerJob)
				serverRoutes.GET("/backups", h.server.ServerBackups)
			}

			metricsRoutes := gated.Group("")
			metricsRoutes.Use(middleware.RateLimitByIPFailClosedWithHandlers(
				rdb,
				adminMetricsRateLimit,
				adminMetricsRateWindow,
				metrics.RateLimited,
				metrics.ServiceUnavailable,
			))
			metricsRoutes.GET("/health", metrics.Health)
			metricsRoutes.GET("/metrics/current", metrics.Current)
			metricsRoutes.GET("/metrics/series", metrics.Series)
			metricsRoutes.GET("/counters", metrics.Counters)

			for _, path := range []string{"/health", "/metrics/current", "/metrics/series", "/counters"} {
				metricsRoutes.Match(adminMetricsDeniedMethods, path, metrics.MethodNotAllowed)
			}
		}
	}

}

// adminRateLimited and adminRateLimitUnavailable are the fixed responses of
// the fail-closed admin limiters.
func adminRateLimited(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "rate_limited"})
}

func adminRateLimitUnavailable(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "internal_error"})
}

// preAuthRoutePaths is the explicit allowlist of admin routes that are
// intentionally reachable WITHOUT an admin session. The reflective deny-by-default
// test (routes_test.go) asserts every registered /admin route NOT in this set
// rejects an unauthenticated request. Keep it in lockstep with RegisterRoutes.
var preAuthRoutePaths = map[string]struct{}{
	"/admin/":                     {},
	"/admin/enroll":               {},
	"/admin/assets/*filepath":     {},
	"/admin/api/v1/auth/password": {},
	"/admin/api/v1/auth/webauthn": {},
	"/admin/api/v1/auth/logout":   {},
	"/admin/api/v1/enroll/begin":  {},
	"/admin/api/v1/enroll/finish": {},
}

// IsPreAuthRoute reports whether the given route path is on the pre-auth
// allowlist. Exported for the reflective routes_test (it lives in admin_test).
func IsPreAuthRoute(path string) bool {
	_, ok := preAuthRoutePaths[path]
	return ok
}
