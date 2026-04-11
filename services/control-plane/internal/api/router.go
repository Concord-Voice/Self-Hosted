// Package api provides HTTP routing and middleware setup for the Control Plane REST API.
package api // revive:disable-line:var-naming

import (
	"context"
	"database/sql"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/channels"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/clientconfig"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/dm"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/email"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/friends"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/invites"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/klipy"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/media"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/members"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/messages"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/mfa"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/middleware"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/ownership"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/servers"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/sessions"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/spa"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/updates"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/users"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/voice"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	natsclient "github.com/markdrogersjr/Concord/services/control-plane/pkg/nats"
	"github.com/redis/go-redis/v9"
)

// Route path constants — extracted to satisfy go:S1192 (no duplicate string literals).
const (
	routeTransferOwnership = "/:id/transfer-ownership"
	routeRecoveryKey       = "/recovery-key"
	routeRecoveryCircle    = "/recovery-circle"
	pathIDMembers          = "/:id/members"
	pathIDMembersUserID    = "/:id/members/:user_id"
	pathIDRead             = "/:id/read"
	pathIDKeys             = "/:id/keys"
	pathIDOverrides        = "/:id/overrides"
	routeVoiceMute         = "/:id/voice/:userId/mute"
	routeVoiceDeafen       = "/:id/voice/:userId/deafen"
)

// mentionCheckerAdapter adapts an rbac.Resolver to the websocket.MentionPermissionChecker
// interface, bridging the rbac.Permission type to int64 without a circular import.
type mentionCheckerAdapter struct {
	resolver *rbac.Resolver
}

func (a *mentionCheckerAdapter) HasMentionPermission(ctx context.Context, serverID, userID, channelID string, permBit int64) (bool, error) {
	return a.resolver.HasPermission(ctx, serverID, userID, channelID, rbac.Permission(permBit))
}

// NewRouter creates a new API router and returns the WebSocket hub and NATS client for lifecycle management.
func NewRouter(db *sql.DB, redis *redis.Client, store media.ObjectStore, cfg *config.Config, liveSpa *config.LiveSpaConfig, log *logger.Logger) (*gin.Engine, *websocket.Hub, *natsclient.Client) {
	router := gin.New()

	// Trust no proxies by default — ClientIP() uses RemoteAddr only.
	// In Docker, nginx is the sole reverse proxy and sets X-Real-IP.
	// This prevents spoofed X-Forwarded-For from bypassing IP-based rate
	// limits and auth failure bans.
	router.SetTrustedProxies(nil) //nolint:errcheck,gosec // nil arg never errors

	// Middleware
	router.Use(gin.Recovery())
	router.Use(middleware.RequestID())
	router.Use(middleware.Logger(log))
	router.Use(middleware.SecurityHeaders(cfg.Environment))
	router.Use(middleware.CORS(cfg.AllowedOrigins))
	router.Use(middleware.ValidateCustomHeaders())

	// Health check
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":  "healthy",
			"service": "control-plane",
		})
	})

	// Initialize WebSocket hub
	hub := websocket.NewHub(db, redis)

	// Initialize NATS (inter-service messaging with media plane)
	var natsClient *natsclient.Client
	nc, err := natsclient.Connect(cfg.NATSUrl)
	if err != nil {
		log.Warn("NATS connection failed — voice state sync disabled", "error", err)
	} else {
		natsClient = nc
	}

	// Initialize MFA handler
	mfaEncKey, err := hex.DecodeString(cfg.MFAEncryptionKey)
	if err != nil || len(mfaEncKey) != 32 {
		log.Fatal("Invalid MFA_ENCRYPTION_KEY: must be 64 hex chars (32 bytes)")
	}
	webauthnSvc, err := mfa.NewWebAuthnService(cfg.WebAuthnRPID, "ConcordVoice", cfg.WebAuthnRPOrigins)
	if err != nil {
		log.Fatal("Failed to create WebAuthn service", "error", err)
	}
	mfaHandler := mfa.NewHandler(db, redis, log, mfaEncKey, cfg.JWTSecret, webauthnSvc, cfg.Environment)

	// Initialize RBAC components (before handlers that depend on resolver)
	permCache := rbac.NewPermissionCache(redis)
	rbacResolver := rbac.NewResolver(db, permCache, log)
	hub.SetMentionChecker(&mentionCheckerAdapter{resolver: rbacResolver})

	// Start hub AFTER all dependencies are injected (avoids data race on mentionChecker)
	go hub.Run()
	auditWriter := rbac.NewAuditWriter(db, log)
	rbacHandler := rbac.NewHandler(db, log, redis, hub, rbacResolver, permCache, auditWriter)

	// Initialize email service
	emailSvc := email.NewService(cfg, log)

	// Initialize handlers
	authHandler := auth.NewHandler(db, redis, log, cfg.JWTSecret, hub)
	authHandler.SetEmailService(emailSvc)
	verificationHandler := auth.NewVerificationHandler(db, redis, log, emailSvc, cfg.JWTSecret)
	authHandler.SetVerificationHandler(verificationHandler)

	// Wire cross-references (breaks circular init dependency)
	authHandler.SetMFAChecker(mfaHandler)
	mfaHandler.SetLoginCompleter(authHandler)
	mfaHandler.SetEmailService(emailSvc)
	sessionsHandler := sessions.NewHandler(db, redis, log, hub, mfaHandler)
	usersHandler := users.NewHandler(db, log, hub, mfaHandler)
	serversHandler := servers.NewHandler(db, log, hub, rbacResolver)
	channelsHandler := channels.NewHandler(db, log, hub, rbacResolver, redis)
	membersHandler := members.NewHandler(db, log, redis, hub, rbacResolver, auditWriter)
	messagesHandler := messages.NewHandler(db, log, hub, rbacResolver)
	invitesHandler := invites.NewHandler(db, log, hub, rbacResolver)
	voiceHandler := voice.NewHandler(db, log, hub, cfg, rbacResolver, natsClient)
	dmHandler := dm.NewHandler(db, log, hub, cfg, natsClient, redis)
	friendsHandler := friends.NewHandler(db, log, hub)
	ownershipHandler := ownership.NewHandler(ownership.HandlerDeps{
		DB:          db,
		Log:         log,
		Hub:         hub,
		Redis:       redis,
		Cache:       permCache,
		Audit:       auditWriter,
		EmailSvc:    emailSvc,
		MFAVerifier: mfaHandler,
	})
	var mediaHandler *media.Handler
	if store != nil {
		mediaHandler = media.NewHandler(db, store, log, cfg, rbacResolver)
		usersHandler.SetMediaStore(store)
		serversHandler.SetMediaStore(store)
	}
	wsHandler := websocket.NewHandler(hub, db, redis, cfg.JWTSecret, cfg.AllowedOrigins)
	wsTicketHandler := auth.NewWSTicketHandler(redis, cfg.JWTSecret)
	clientConfigHandler := clientconfig.NewHandler(cfg, liveSpa, log)
	updatesHandler := updates.NewHandler(cfg, log)
	spaHandler := spa.NewHandler(cfg.SpaDir, log)

	// Start NATS voice event subscriber
	if natsClient != nil {
		voiceSub := voice.NewNATSSubscriber(db, log, hub, natsClient)
		if subErr := voiceSub.Subscribe(); subErr != nil {
			log.Error("Failed to subscribe to voice NATS events", "error", subErr)
		}
	}

	// API v1 routes
	v1 := router.Group("/api/v1")
	{
		// Auth routes with rate limiting + IP-based auth failure ban check
		authRoutes := v1.Group("/auth")
		authRoutes.Use(middleware.AuthBanCheck(redis))
		{
			// Register: 5 attempts per 15 minutes (prevent spam account creation)
			authRoutes.POST("/register",
				middleware.RateLimitByIP(redis, 5, 15*time.Minute),
				authHandler.Register,
			)

			// Login: 10 attempts per 15 minutes (prevent brute force)
			authRoutes.POST("/login",
				middleware.RateLimitByIP(redis, 10, 15*time.Minute),
				authHandler.Login,
			)

			// Refresh: 30 attempts per minute (normal usage, but prevent abuse)
			authRoutes.POST("/refresh",
				middleware.RateLimitByIP(redis, 30, 1*time.Minute),
				authHandler.Refresh,
			)

			// Logout: 10 attempts per minute
			authRoutes.POST("/logout",
				middleware.RateLimitByIP(redis, 10, 1*time.Minute),
				authHandler.Logout,
			)

			// MFA verify (unauthenticated — uses challenge token from login)
			authRoutes.POST("/mfa/verify",
				middleware.RateLimitByIP(redis, 10, 15*time.Minute),
				mfaHandler.Verify,
			)

			// MFA email code delivery (unauthenticated — uses challenge token)
			authRoutes.POST("/mfa/email/send",
				middleware.RateLimitByIP(redis, 3, 15*time.Minute),
				mfaHandler.SendEmailMFACode,
			)

			// Account recovery (unauthenticated — uses email verification + recovery tokens)
			authRoutes.POST("/recovery/begin",
				middleware.RateLimitByIP(redis, 3, 15*time.Minute),
				authHandler.RecoveryBegin,
			)
			authRoutes.POST("/recovery/verify-code",
				middleware.RateLimitByIP(redis, 5, 15*time.Minute),
				authHandler.RecoveryVerifyCode,
			)
			authRoutes.POST("/recovery/reset-password",
				middleware.RateLimitByIP(redis, 5, 15*time.Minute),
				authHandler.RecoveryResetPassword,
			)
			authRoutes.POST("/recovery/reset-account",
				middleware.RateLimitByIP(redis, 3, 15*time.Minute),
				authHandler.RecoveryResetAccount,
			)

			// Trusted device recovery (unauthenticated — uses recovery token)
			authRoutes.POST("/recovery/device-request",
				middleware.RateLimitByIP(redis, 3, 15*time.Minute),
				authHandler.CreateDeviceRecoveryRequest,
			)
			authRoutes.GET("/recovery/device-request/:id",
				middleware.RateLimitByIP(redis, 10, 15*time.Minute),
				authHandler.PollDeviceRecoveryRequest,
			)

			// Social recovery (unauthenticated — uses recovery token)
			authRoutes.POST("/recovery/social-request",
				middleware.RateLimitByIP(redis, 3, 15*time.Minute),
				authHandler.CreateSocialRecoveryRequest,
			)
			authRoutes.GET("/recovery/social-request/:id",
				middleware.RateLimitByIP(redis, 10, 15*time.Minute),
				authHandler.PollSocialRecoveryRequest,
			)
		}

		// Client config (public — pre-auth, clients need this before login)
		v1.GET("/client/config",
			middleware.RateLimitByIP(redis, 30, 1*time.Minute),
			clientConfigHandler.GetConfig,
		)

		// Desktop update assets (public — electron-updater needs this pre-auth)
		v1.GET("/updates/*filename",
			middleware.RateLimitByIP(redis, 30, 1*time.Minute),
			updatesHandler.ServeUpdateAsset,
		)

		// SPA assets (public — served to desktop shells for Tier 3 hot updates)
		// Cloudflare caches hashed assets (immutable/1yr) and index.html (5min).
		if cfg.SpaDir != "" {
			v1.GET("/spa/*filepath",
				middleware.RateLimitByIP(redis, 60, 1*time.Minute),
				spaHandler.ServeAsset,
			)
		}

		// Tier 1 user image proxy GETs (public — required so <img> tags can
		// render without an Authorization header). The opaque user UUID is the
		// only identifier; avatars/banners are intentionally shareable, so the
		// previous auth gate added no real protection while breaking rendering.
		// Uploads + deletes remain authenticated. Rate-limited per IP since
		// there is no user context.
		//
		// server-icons, server-banners, and dm-icons are also public Tier 1
		// media: the unguessable UUID is the only identifier and they need to
		// render via plain <img> tags without an Authorization header.
		// Membership checks have been removed from their handlers.
		if mediaHandler != nil {
			v1.GET("/media/avatars/:user_id",
				middleware.RateLimitByIP(redis, 120, 1*time.Minute),
				mediaHandler.ProxyAvatar,
			)
			v1.GET("/media/banners/:user_id",
				middleware.RateLimitByIP(redis, 120, 1*time.Minute),
				mediaHandler.ProxyBanner,
			)
			v1.GET("/media/server-icons/:server_id",
				middleware.RateLimitByIP(redis, 120, 1*time.Minute),
				mediaHandler.ProxyServerIcon,
			)
			v1.GET("/media/server-banners/:server_id",
				middleware.RateLimitByIP(redis, 120, 1*time.Minute),
				mediaHandler.ProxyServerBanner,
			)
			v1.GET("/media/dm-icons/:conversationId",
				middleware.RateLimitByIP(redis, 60, 1*time.Minute),
				mediaHandler.ProxyDMIcon,
			)
		} else {
			// Match the protected fallback below so misconfiguration fails
			// with a clear 503 instead of a 404 for all public Tier 1 routes.
			mediaUnavailable := func(c *gin.Context) {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "media storage not configured"})
			}
			v1.GET("/media/avatars/:user_id", mediaUnavailable)
			v1.GET("/media/banners/:user_id", mediaUnavailable)
			v1.GET("/media/server-icons/:server_id", mediaUnavailable)
			v1.GET("/media/server-banners/:server_id", mediaUnavailable)
			v1.GET("/media/dm-icons/:conversationId", mediaUnavailable)
		}

		// Protected routes — split into two tiers:
		// pendingOK: authenticated but email may be unverified (verification, logout, basic profile)
		// verified:  authenticated AND email verified (everything else)
		authRequired := v1.Group("/")
		authRequired.Use(middleware.AuthRequired(cfg.JWTSecret, redis))

		// ── Pending-OK routes (unverified email allowed) ──────────────
		{
			// Email verification endpoints
			authRequired.POST("/auth/verify-email/send",
				middleware.RateLimitByUser(redis, 1, 1*time.Minute),
				verificationHandler.SendCode,
			)
			authRequired.POST("/auth/verify-email/confirm",
				middleware.RateLimitByUser(redis, 5, 15*time.Minute),
				verificationHandler.ConfirmCode,
			)
			authRequired.POST("/auth/verify-email/change-email",
				middleware.RateLimitByUser(redis, 3, 15*time.Minute),
				verificationHandler.ChangeEmail,
			)

			// Note: logout is in the public authRoutes group above (uses refresh token, not Bearer)

			// Basic profile read (needed by frontend to check verification status)
			authRequired.GET("/users/me",
				middleware.RateLimitByUser(redis, 30, 1*time.Minute),
				usersHandler.GetMe,
			)
		}

		// ── Verified routes (email must be verified) ──────────────────
		protected := authRequired.Group("/")
		protected.Use(middleware.RequireVerifiedEmail())
		{
			// WebSocket ticket (short-lived, single-use)
			protected.POST("/auth/ws-ticket",
				middleware.RateLimitByUser(redis, 10, 1*time.Minute),
				wsTicketHandler.IssueTicket,
			)

			// MFA management routes (authenticated)
			mfaRoutes := protected.Group("/mfa")
			{
				mfaRoutes.GET("/status",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					mfaHandler.GetStatus,
				)

				// TOTP enrollment
				mfaRoutes.POST("/totp/setup",
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					mfaHandler.TOTPSetup,
				)
				mfaRoutes.POST("/totp/verify-setup",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					mfaHandler.TOTPVerifySetup,
				)
				mfaRoutes.POST("/totp/confirm-setup",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					mfaHandler.TOTPConfirmSetup,
				)
				mfaRoutes.POST("/totp/disable",
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					mfaHandler.TOTPDisable,
				)

				// Backup codes
				mfaRoutes.POST("/backup-codes/regenerate",
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					mfaHandler.RegenerateBackupCodes,
				)

				// WebAuthn credential management
				mfaRoutes.POST("/webauthn/register/begin",
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					mfaHandler.WebAuthnRegisterBegin,
				)
				mfaRoutes.POST("/webauthn/register/finish",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					mfaHandler.WebAuthnRegisterFinish,
				)
				mfaRoutes.GET("/webauthn/credentials",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					mfaHandler.WebAuthnListCredentials,
				)
				mfaRoutes.DELETE("/webauthn/credentials/:id",
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					mfaHandler.WebAuthnDeleteCredential,
				)

				// WebAuthn inline verify (for MFA verification on protected operations)
				mfaRoutes.POST("/webauthn/verify-inline/begin",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					mfaHandler.WebAuthnVerifyInlineBegin,
				)
				mfaRoutes.POST("/webauthn/verify-inline/finish",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					mfaHandler.WebAuthnVerifyInlineFinish,
				)

				// Recovery-only method settings
				mfaRoutes.PUT("/recovery-only",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					mfaHandler.SetRecoveryOnly,
				)

				// Hardened recovery mode (dual-channel Email+SMS)
				mfaRoutes.PUT("/recovery-hardened",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					mfaHandler.SetRecoveryHardened,
				)

				// Email/SMS MFA (dev stub — blocked in production)
				mfaRoutes.POST("/email-sms/setup",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					mfaHandler.EmailSmsSetup,
				)
				mfaRoutes.POST("/email-sms/verify",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					mfaHandler.EmailSmsVerify,
				)
				mfaRoutes.POST("/email-sms/disable",
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					mfaHandler.EmailSmsDisable,
				)
				mfaRoutes.GET("/email-sms/status",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					mfaHandler.EmailSmsStatus,
				)

				// Backup email for recovery
				mfaRoutes.GET("/backup-email",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					mfaHandler.GetBackupEmail,
				)
				mfaRoutes.PUT("/backup-email",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					mfaHandler.SetBackupEmail,
				)

				// Recovery key management
				mfaRoutes.PUT(routeRecoveryKey,
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					mfaHandler.StoreRecoveryKey,
				)
				mfaRoutes.GET(routeRecoveryKey,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					mfaHandler.GetRecoveryKeyStatus,
				)
				mfaRoutes.DELETE(routeRecoveryKey,
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					mfaHandler.DeleteRecoveryKey,
				)

				// Trusted device management
				mfaRoutes.GET("/trusted-devices",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					mfaHandler.ListTrustedDevices,
				)
				mfaRoutes.POST("/trusted-devices",
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					mfaHandler.DesignateTrustedDevice,
				)
				mfaRoutes.DELETE("/trusted-devices/:id",
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					mfaHandler.RemoveTrustedDevice,
				)

				// Recovery request management (authenticated user responds)
				mfaRoutes.GET("/recovery-requests",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					mfaHandler.ListRecoveryRequests,
				)
				mfaRoutes.POST("/recovery-requests/:id/respond",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					mfaHandler.RespondToRecoveryRequest,
				)

				// Recovery circle management (Shamir's Secret Sharing)
				mfaRoutes.GET(routeRecoveryCircle,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					mfaHandler.GetRecoveryCircle,
				)
				mfaRoutes.PUT(routeRecoveryCircle,
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					mfaHandler.UpsertRecoveryCircle,
				)
				mfaRoutes.DELETE(routeRecoveryCircle,
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					mfaHandler.DeleteRecoveryCircle,
				)
				mfaRoutes.GET("/recovery-circle/shares",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					mfaHandler.GetMyRecoveryShares,
				)

				// Social recovery requests (contacts respond)
				mfaRoutes.GET("/recovery-requests/social",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					mfaHandler.ListSocialRecoveryRequests,
				)
				mfaRoutes.POST("/recovery-requests/social/:id/respond",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					mfaHandler.RespondToSocialRecovery,
				)
			}

			// User routes (GET /users/me is in the pendingOK group above)
			userRoutes := protected.Group("/users")
			{
				// Update profile (10 requests per minute)
				userRoutes.PATCH("/me",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					usersHandler.UpdateMe,
				)

				// Get E2EE keys (for password change re-wrapping)
				userRoutes.GET("/me/keys",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					usersHandler.GetMyKeys,
				)

				// Replace E2EE keys (for key recovery)
				userRoutes.PUT("/me/keys",
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					usersHandler.ReplaceMyKeys,
				)

				// Change password (5 requests per minute - sensitive)
				userRoutes.POST("/me/password",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					usersHandler.ChangePassword,
				)

				// Get user's public key (for E2EE key wrapping)
				userRoutes.GET("/:user_id/public-key",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					usersHandler.GetPublicKey,
				)

				// Get user's public profile (for viewing other users)
				userRoutes.GET("/:user_id/profile",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					usersHandler.GetPublicProfile,
				)

				// Encrypted user preferences (cross-device sync)
				userRoutes.GET("/me/preferences",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					usersHandler.GetPreferences,
				)
				userRoutes.PUT("/me/preferences",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					usersHandler.UpdatePreferences,
				)

				// Encrypted saved GIFs (cross-device sync)
				userRoutes.GET("/me/saved-gifs",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					usersHandler.GetSavedGifs,
				)
				userRoutes.PUT("/me/saved-gifs",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					usersHandler.UpdateSavedGifs,
				)

				// Search users by username/display name
				userRoutes.GET("/search",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					usersHandler.SearchUsers,
				)

				// Privacy settings
				userRoutes.GET("/me/privacy",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					usersHandler.GetPrivacySettings,
				)
				userRoutes.PATCH("/me/privacy",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					usersHandler.UpdatePrivacySettings,
				)
			}

			// Session management routes
			sessionRoutes := protected.Group("/sessions")
			{
				// List all active sessions (10 requests per minute)
				sessionRoutes.GET("",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					sessionsHandler.ListSessions,
				)

				// Revoke a specific session (10 requests per minute)
				sessionRoutes.DELETE("/:id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					sessionsHandler.RevokeSession,
				)

				// Revoke all sessions (5 requests per minute - requires password re-verification)
				sessionRoutes.POST("/revoke-all",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					sessionsHandler.RevokeAllSessions,
				)

				// Toggle revocation mode (5 requests per minute - requires password re-verification)
				sessionRoutes.PUT("/revocation-mode",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					sessionsHandler.UpdateRevocationMode,
				)
			}

			// Server routes
			serverRoutes := protected.Group("/servers")
			{
				// List user's servers (30 requests per minute)
				serverRoutes.GET("",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					serversHandler.ListServers,
				)

				// Get unread status across all user's servers (30 requests per minute)
				serverRoutes.GET("/unread-status",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					channelsHandler.GetServerUnreadStatus,
				)

				// Create server (10 requests per minute)
				serverRoutes.POST("",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					serversHandler.CreateServer,
				)

				// Get specific server (30 requests per minute)
				serverRoutes.GET("/:id",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					serversHandler.GetServer,
				)

				// Update server (10 requests per minute)
				serverRoutes.PATCH("/:id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					serversHandler.UpdateServer,
				)

				// Delete server (5 requests per minute - destructive action)
				serverRoutes.DELETE("/:id",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					serversHandler.DeleteServer,
				)

				// List channels in a server (30 requests per minute)
				serverRoutes.GET("/:id/channels",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					channelsHandler.ListChannels,
				)

				// List members in a server (30 requests per minute)
				serverRoutes.GET(pathIDMembers,
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					membersHandler.ListMembers,
				)

				// Add member to server (10 requests per minute)
				serverRoutes.POST(pathIDMembers,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					membersHandler.AddMember,
				)

				// Update member role (5 requests per minute - sensitive operation)
				serverRoutes.PATCH(pathIDMembersUserID,
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					membersHandler.UpdateMember,
				)

				// Remove member from server (5 requests per minute - sensitive operation)
				serverRoutes.DELETE(pathIDMembersUserID,
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					membersHandler.RemoveMember,
				)

				// Ban management
				serverRoutes.POST("/:id/bans/:user_id",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					membersHandler.BanMember,
				)
				serverRoutes.DELETE("/:id/bans/:user_id",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					membersHandler.UnbanMember,
				)
				serverRoutes.GET("/:id/bans",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					membersHandler.ListBans,
				)

				// Create invite for server (10 requests per minute)
				serverRoutes.POST("/:id/invites",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					invitesHandler.CreateInvite,
				)

				// List invites for server (30 requests per minute)
				serverRoutes.GET("/:id/invites",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					invitesHandler.ListInvites,
				)

				// Revoke invite (10 requests per minute)
				serverRoutes.DELETE("/:id/invites/:invite_id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					invitesHandler.RevokeInvite,
				)

				// Channel group routes
				serverRoutes.GET("/:id/channel-groups",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					channelsHandler.ListChannelGroups,
				)
				serverRoutes.POST("/:id/channel-groups",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					channelsHandler.CreateChannelGroup,
				)
				serverRoutes.PATCH("/:id/channel-groups/:group_id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					channelsHandler.UpdateChannelGroup,
				)
				serverRoutes.DELETE("/:id/channel-groups/:group_id",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					channelsHandler.DeleteChannelGroup,
				)

				// Bulk reorder channels (drag-and-drop between groups)
				serverRoutes.PUT("/:id/channels/reorder",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					channelsHandler.ReorderChannels,
				)

				// Get unread counts for all channels in a server (30 requests per minute)
				serverRoutes.GET("/:id/unread",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					channelsHandler.GetUnreadCounts,
				)

				// Mark all channels in a server as read (30 requests per minute)
				serverRoutes.POST(pathIDRead,
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					channelsHandler.MarkServerRead,
				)

				// RBAC: Role management
				serverRoutes.GET("/:id/roles",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					rbac.RequireMembership(rbacResolver),
					rbacHandler.ListRoles,
				)
				serverRoutes.POST("/:id/roles",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					rbac.RequirePermission(rbacResolver, rbac.PermManageRoles, ""),
					rbacHandler.CreateRole,
				)
				// Register /reorder BEFORE /:role_id to prevent Gin matching "reorder" as a wildcard
				serverRoutes.PATCH("/:id/roles/reorder",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					rbac.RequirePermission(rbacResolver, rbac.PermManageRoles, ""),
					rbacHandler.ReorderRoles,
				)
				serverRoutes.PATCH("/:id/roles/:role_id",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					rbac.RequirePermission(rbacResolver, rbac.PermManageRoles, ""),
					rbacHandler.UpdateRole,
				)
				serverRoutes.DELETE("/:id/roles/:role_id",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					rbac.RequirePermission(rbacResolver, rbac.PermManageRoles, ""),
					rbacHandler.DeleteRole,
				)

				// RBAC: Role assignment
				serverRoutes.POST("/:id/members/:user_id/roles",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					rbac.RequirePermission(rbacResolver, rbac.PermManageRolesAssign, ""),
					rbacHandler.AssignRole,
				)
				serverRoutes.DELETE("/:id/members/:user_id/roles/:role_id",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					rbac.RequirePermission(rbacResolver, rbac.PermManageRolesAssign, ""),
					rbacHandler.UnassignRole,
				)

				// RBAC: Computed server permissions
				serverRoutes.GET("/:id/permissions",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					rbac.RequireMembership(rbacResolver),
					rbacHandler.GetMyServerPermissions,
				)

				// RBAC: Audit log
				serverRoutes.GET("/:id/audit-log",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					rbac.RequirePermission(rbacResolver, rbac.PermViewAuditLog, ""),
					rbacHandler.GetAuditLog,
				)

				// Ownership transfer
				serverRoutes.POST(routeTransferOwnership,
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					ownershipHandler.InitiateTransfer,
				)
				serverRoutes.GET(routeTransferOwnership,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					ownershipHandler.GetTransferStatus,
				)
				serverRoutes.DELETE(routeTransferOwnership,
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					ownershipHandler.CancelTransfer,
				)
				// Register /confirm BEFORE transfer-ownership could match as wildcard
				serverRoutes.POST("/:id/transfer-ownership/confirm",
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					ownershipHandler.ConfirmTransfer,
				)

				// Server-enforced voice moderation (#488)
				serverRoutes.POST(routeVoiceMute,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					voiceHandler.ServerMute,
				)
				serverRoutes.DELETE(routeVoiceMute,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					voiceHandler.ServerUnmute,
				)
				serverRoutes.POST(routeVoiceDeafen,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					voiceHandler.ServerDeafen,
				)
				serverRoutes.DELETE(routeVoiceDeafen,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					voiceHandler.ServerUndeafen,
				)
				serverRoutes.POST("/:id/voice/:userId/user-mute",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					voiceHandler.UserMute,
				)
				serverRoutes.POST("/:id/voice/:userId/user-deafen",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					voiceHandler.UserDeafen,
				)
			}

			// Ownership transfer reversal (token-based, outside server routes)
			protected.POST("/ownership/reverse/:token",
				middleware.RateLimitByUser(redis, 3, 15*time.Minute),
				ownershipHandler.ReverseTransfer,
			)

			// Channel routes
			channelRoutes := protected.Group("/channels")
			{
				// Create channel (10 requests per minute)
				channelRoutes.POST("",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					channelsHandler.CreateChannel,
				)

				// Get specific channel (30 requests per minute)
				channelRoutes.GET("/:id",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					channelsHandler.GetChannel,
				)

				// Update channel (10 requests per minute)
				channelRoutes.PATCH("/:id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					channelsHandler.UpdateChannel,
				)

				// Delete channel (5 requests per minute - destructive action)
				channelRoutes.DELETE("/:id",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					channelsHandler.DeleteChannel,
				)

				// Get message history for a channel (30 requests per minute)
				channelRoutes.GET("/:id/messages",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					messagesHandler.GetMessages,
				)

				// Get pinned messages for a channel (30 requests per minute)
				channelRoutes.GET("/:id/pins",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					messagesHandler.GetChannelPins,
				)

				// Bulk message fetch for search backfill (20 requests per minute)
				channelRoutes.GET("/:id/messages/bulk",
					middleware.RateLimitByUser(redis, 20, 1*time.Minute),
					messagesHandler.GetMessagesBulk,
				)

				// Mark channel as read (30 requests per minute)
				channelRoutes.POST(pathIDRead,
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					channelsHandler.MarkChannelRead,
				)

				// Get channel encryption keys (for E2EE channels)
				channelRoutes.GET(pathIDKeys,
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					channelsHandler.GetChannelKeys,
				)

				// Distribute channel keys to new members (E2EE key distribution)
				channelRoutes.POST(pathIDKeys,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					channelsHandler.DistributeChannelKeys,
				)

				// Voice channel routes
				channelRoutes.GET("/:id/voice/participants",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					voiceHandler.GetParticipants,
				)
				channelRoutes.POST("/:id/voice/join",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					voiceHandler.AuthorizeJoin,
				)
				channelRoutes.POST("/:id/voice/authorize-action",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					voiceHandler.AuthorizeVoiceAction,
				)

				// Rotate channel encryption key (admin/owner only)
				channelRoutes.POST("/:id/rotate-key",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					channelsHandler.RotateKey,
				)

				// RBAC: Channel permission overrides
				channelRoutes.GET(pathIDOverrides,
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					rbacHandler.ListChannelOverrides,
				)
				channelRoutes.PUT(pathIDOverrides,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					rbacHandler.UpsertChannelOverride,
				)
				channelRoutes.DELETE("/:id/overrides/:override_id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					rbacHandler.DeleteChannelOverride,
				)

				// RBAC: Computed channel permissions
				channelRoutes.GET("/:id/permissions",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					rbacHandler.GetMyChannelPermissions,
				)

				// RBAC: Channel permission sync (inherit from parent category)
				channelRoutes.PUT("/:id/permission-sync",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					rbacHandler.SetChannelPermissionSync,
				)
			}

			// Category permission override routes
			categoryRoutes := protected.Group("/categories")
			{
				categoryRoutes.GET(pathIDOverrides,
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					rbacHandler.ListCategoryOverrides,
				)
				categoryRoutes.PUT(pathIDOverrides,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					rbacHandler.UpsertCategoryOverride,
				)
				categoryRoutes.DELETE("/:id/overrides/:override_id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					rbacHandler.DeleteCategoryOverride,
				)
			}

			// E2EE key management routes (separate group to avoid gin /:id wildcard conflict)
			e2eeRoutes := protected.Group("/e2ee")
			{
				// Get pending key requests (for E2EE key distribution)
				e2eeRoutes.GET("/pending-keys",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					channelsHandler.GetPendingKeyRequests,
				)

				// Unified key endpoints (resolves server channel vs DM conversation)
				// GET needs a generous limit — every message decryption requires a key fetch
				// when the 5-minute client cache expires or on channel switch
				e2eeRoutes.GET("/keys/:context_id",
					middleware.RateLimitByUser(redis, 120, 1*time.Minute),
					channelsHandler.GetUnifiedKeys,
				)
				e2eeRoutes.POST("/keys/:context_id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					channelsHandler.DistributeUnifiedKeys,
				)

				// Validate cached key epochs on reconnect (pull-based catch-up for missed revocations)
				e2eeRoutes.POST("/validate-epochs",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					channelsHandler.ValidateEpochs,
				)
			}

			// Message routes
			messageRoutes := protected.Group("/messages")
			{
				// Send message (30 requests per minute)
				messageRoutes.POST("",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					messagesHandler.SendMessage,
				)

				// Update message (10 requests per minute)
				messageRoutes.PATCH("/:id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					messagesHandler.UpdateMessage,
				)

				// Delete message (10 requests per minute)
				messageRoutes.DELETE("/:id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					messagesHandler.DeleteMessage,
				)

				// Suppress embeds on a message (moderator one-way ratchet)
				messageRoutes.POST("/:id/suppress-embeds",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					messagesHandler.SuppressEmbeds,
				)

				// Toggle reaction on a message (30 requests per minute)
				messageRoutes.PUT("/:id/reactions",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					messagesHandler.ToggleReaction,
				)

				// Get reactions for a message (30 requests per minute)
				messageRoutes.GET("/:id/reactions",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					messagesHandler.GetReactions,
				)

				// Pin a message (10 requests per minute)
				messageRoutes.POST("/:id/pin",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					messagesHandler.PinMessage,
				)

				// Unpin a message (10 requests per minute)
				messageRoutes.DELETE("/:id/pin",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					messagesHandler.UnpinMessage,
				)
			}

			// KLIPY GIF proxy routes (for Privacy Mode).
			// Routes only exist when KLIPY_API_KEY is set in the environment;
			// when the key is empty the entire integration is disabled and these
			// paths return 404 instead of forwarding to upstream.
			if cfg.KlipyAPIKey != "" {
				klipyHandler := klipy.NewHandler(cfg, log)
				klipyRoutes := protected.Group("/klipy")
				{
					// 30/min on the API endpoints (debounced on the client)
					apiLimiter := middleware.RateLimitByUser(redis, 30, 1*time.Minute)
					// GIF list/search/metadata endpoints — nested under /gifs to
					// match upstream KLIPY API conventions and the client paths
					// in klipyClient.ts (e.g. `/gifs/trending`, `/gifs/items`).
					gifRoutes := klipyRoutes.Group("/gifs")
					gifRoutes.GET("/trending", apiLimiter, klipyHandler.Trending)
					gifRoutes.GET("/search", apiLimiter, klipyHandler.Search)
					gifRoutes.GET("/categories", apiLimiter, klipyHandler.Categories)
					gifRoutes.GET("/recent/:customerID", apiLimiter, klipyHandler.Recent)
					gifRoutes.DELETE("/recent/:customerID", apiLimiter, klipyHandler.HideRecent)
					gifRoutes.GET("/items", apiLimiter, klipyHandler.Items)
					gifRoutes.POST("/share/:slug", apiLimiter, klipyHandler.Share)
					gifRoutes.POST("/report/:slug", apiLimiter, klipyHandler.Report)
					klipyRoutes.GET("/randomid", apiLimiter, klipyHandler.RandomID) //nolint:staticcheck // deprecated but kept for backward compatibility
					klipyRoutes.POST("/customer-id", apiLimiter, klipyHandler.CustomerID)

					// 120/min on the media proxy (chat scrollback bursts)
					klipyRoutes.GET("/media",
						middleware.RateLimitByUser(redis, 120, 1*time.Minute),
						klipyHandler.Media,
					)
				}
			}

			// Invite routes (join + preview)
			inviteRoutes := protected.Group("/invites")
			{
				// Join server via invite code (10 requests per minute)
				inviteRoutes.POST("/join",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					invitesHandler.JoinServer,
				)

				// Get invite info / preview (30 requests per minute)
				inviteRoutes.GET("/:code",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					invitesHandler.GetInviteInfo,
				)
			}

			// DM conversation routes
			dmRoutes := protected.Group("/dm/conversations")
			{
				// List user's DM conversations (30 requests per minute)
				dmRoutes.GET("",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					dmHandler.ListConversations,
				)

				// Open/get-or-create 1:1 DM conversation (10 requests per minute)
				dmRoutes.POST("",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.OpenConversation,
				)

				// Create group DM (10 requests per minute)
				dmRoutes.POST("/group",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.CreateGroup,
				)

				// Get or create personal thread (10 requests per minute)
				dmRoutes.POST("/personal",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.GetOrCreatePersonalThread,
				)

				// Get specific DM conversation (30 requests per minute)
				dmRoutes.GET("/:id",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					dmHandler.GetConversation,
				)

				// Update DM conversation (group name, etc.) (10 requests per minute)
				dmRoutes.PATCH("/:id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.UpdateConversation,
				)

				// Group member management (10 requests per minute)
				dmRoutes.POST(pathIDMembers,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.AddMember,
				)
				dmRoutes.DELETE(pathIDMembers+"/:userId",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.RemoveMember,
				)

				// Group member role management (10 requests per minute)
				dmRoutes.PATCH(pathIDMembers+"/:userId",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.UpdateMemberRole,
				)

				// Delete group DM (5 requests per minute)
				dmRoutes.DELETE("/:id",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					dmHandler.DeleteGroup,
				)

				// Get DM message history (30 requests per minute)
				dmRoutes.GET("/:id/messages",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					dmHandler.GetMessages,
				)

				// Edit DM message (10 requests per minute)
				dmRoutes.PATCH("/:id/messages/:message_id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.UpdateMessage,
				)

				// Delete DM message (10 requests per minute)
				dmRoutes.DELETE("/:id/messages/:message_id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.DeleteMessage,
				)

				// Mark DM conversation as read (30 requests per minute)
				dmRoutes.POST(pathIDRead,
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					dmHandler.MarkRead,
				)

				// DM E2EE key management
				dmRoutes.GET(pathIDKeys,
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					dmHandler.GetKeys,
				)
				dmRoutes.POST(pathIDKeys,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.DistributeKeys,
				)

				// DM voice call
				dmRoutes.POST("/:id/voice/join",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.AuthorizeVoiceJoin,
				)
				dmRoutes.GET("/:id/voice/participants",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					dmHandler.GetVoiceParticipants,
				)

				// DM voice enforcement (#488)
				dmRoutes.POST("/:id/voice/:userId/user-mute",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.DMUserMute,
				)
				dmRoutes.POST(routeVoiceMute,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.DMHardMute,
				)
				dmRoutes.DELETE(routeVoiceMute,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.DMHardUnmute,
				)
				dmRoutes.POST(routeVoiceDeafen,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.DMHardDeafen,
				)
				dmRoutes.DELETE(routeVoiceDeafen,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.DMHardUndeafen,
				)

				// Manual seal & rotate DM encryption key (5 per day per conversation)
				dmRoutes.POST("/:id/rotate-key",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					dmHandler.RotateKey,
				)
			}

			// Friends routes
			friendRoutes := protected.Group("/friends")
			{
				// List accepted friends (30 requests per minute)
				friendRoutes.GET("",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					friendsHandler.ListFriends,
				)

				// List pending friend requests (30 requests per minute)
				friendRoutes.GET("/requests",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					friendsHandler.ListRequests,
				)

				// Send friend request (10 requests per minute)
				friendRoutes.POST("/request",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					friendsHandler.SendRequest,
				)

				// Accept/decline friend request (10 requests per minute)
				friendRoutes.PATCH("/request/:id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					friendsHandler.RespondRequest,
				)

				// Remove friend (5 requests per minute)
				friendRoutes.DELETE("/:user_id",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					friendsHandler.RemoveFriend,
				)

				// Block user (5 requests per minute)
				friendRoutes.POST("/:user_id/block",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					friendsHandler.BlockUser,
				)

				// Friend code routes
				codeRoutes := friendRoutes.Group("/codes")
				{
					// Create friend code (10 requests per minute)
					codeRoutes.POST("",
						middleware.RateLimitByUser(redis, 10, 1*time.Minute),
						friendsHandler.CreateFriendCode,
					)

					// List friend codes (30 requests per minute)
					codeRoutes.GET("",
						middleware.RateLimitByUser(redis, 30, 1*time.Minute),
						friendsHandler.ListFriendCodes,
					)

					// Revoke friend code (10 requests per minute)
					codeRoutes.DELETE("/:id",
						middleware.RateLimitByUser(redis, 10, 1*time.Minute),
						friendsHandler.RevokeFriendCode,
					)

					// Preview friend code (30 requests per minute)
					codeRoutes.GET("/:code",
						middleware.RateLimitByUser(redis, 30, 1*time.Minute),
						friendsHandler.PreviewFriendCode,
					)

					// Claim friend code (10 requests per minute)
					codeRoutes.POST("/:code/claim",
						middleware.RateLimitByUser(redis, 10, 1*time.Minute),
						friendsHandler.ClaimFriendCode,
					)
				}
			}
		}

		// Media routes (object storage uploads, proxy, presigned URLs)
		// Only registered when storage is configured (nil in tests without MinIO).
		// The misconfig fallback lives on the public v1 group above so a single
		// `/api/v1/media/*path` wildcard handles every method/path consistently
		// (registering it on both v1 and protected would collide on the engine
		// route tree).
		mediaRoutes := protected.Group("/media")
		if mediaHandler != nil {
			// Tier 1 uploads (authenticated, server-processed)
			mediaRoutes.POST("/upload/avatar",
				middleware.RateLimitByUser(redis, 5, 1*time.Minute),
				mediaHandler.UploadAvatar,
			)
			mediaRoutes.POST("/upload/banner",
				middleware.RateLimitByUser(redis, 5, 1*time.Minute),
				mediaHandler.UploadBanner,
			)
			mediaRoutes.POST("/upload/server-icon",
				middleware.RateLimitByUser(redis, 5, 1*time.Minute),
				mediaHandler.UploadServerIcon,
			)
			mediaRoutes.POST("/upload/server-banner",
				middleware.RateLimitByUser(redis, 5, 1*time.Minute),
				mediaHandler.UploadServerBanner,
			)
			mediaRoutes.POST("/upload/dm-icon",
				middleware.RateLimitByUser(redis, 10, 1*time.Minute),
				mediaHandler.UploadDMIcon,
			)

			// Tier 2 upload (E2EE attachments — ciphertext stored as-is)
			mediaRoutes.POST("/upload/attachment",
				middleware.RateLimitByUser(redis, 30, 1*time.Minute),
				mediaHandler.UploadAttachment,
			)

			// Tier 2 proxy download (E2EE attachment — proxied, not presigned)
			mediaRoutes.GET("/attachments/:file_id",
				middleware.RateLimitByUser(redis, 60, 1*time.Minute),
				mediaHandler.DownloadAttachment,
			)

			// Delete media (uploader only)
			mediaRoutes.DELETE("/:file_id",
				middleware.RateLimitByUser(redis, 10, 1*time.Minute),
				mediaHandler.DeleteMedia,
			)
		} else {
			mediaUnavailable503 := func(c *gin.Context) {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "media storage not configured"})
			}
			mediaRoutes.POST("/upload/avatar", mediaUnavailable503)
			mediaRoutes.POST("/upload/banner", mediaUnavailable503)
			mediaRoutes.POST("/upload/server-icon", mediaUnavailable503)
			mediaRoutes.POST("/upload/server-banner", mediaUnavailable503)
			mediaRoutes.POST("/upload/dm-icon", mediaUnavailable503)
			mediaRoutes.POST("/upload/attachment", mediaUnavailable503)
			mediaRoutes.GET("/attachments/:file_id", mediaUnavailable503)
			mediaRoutes.DELETE("/:file_id", mediaUnavailable503)
		}

		// WebSocket endpoint (requires JWT authentication via query parameter or header)
		v1.GET("/ws", wsHandler.HandleWebSocket)
	}

	return router, hub, natsClient
}
