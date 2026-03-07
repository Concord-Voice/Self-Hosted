// Package api provides HTTP routing and middleware setup for the Control Plane REST API.
package api // revive:disable-line:var-naming

import (
	"database/sql"
	"net/http"
	"time"

	"encoding/hex"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/channels"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/dm"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/friends"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/invites"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/members"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/messages"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/mfa"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/middleware"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/servers"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/sessions"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/users"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/voice"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	natsclient "github.com/markdrogersjr/Concord/services/control-plane/pkg/nats"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// NewRouter creates a new API router and returns the WebSocket hub and NATS client for lifecycle management.
func NewRouter(db *sql.DB, redis *redis.Client, cfg *config.Config, log *logger.Logger) (*gin.Engine, *websocket.Hub, *natsclient.Client) {
	router := gin.New()

	// Middleware
	router.Use(gin.Recovery())
	router.Use(middleware.Logger(log))
	router.Use(middleware.SecurityHeaders(cfg.Environment))
	router.Use(middleware.CORS(cfg.AllowedOrigins))

	// Health check
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":  "healthy",
			"service": "control-plane",
		})
	})

	// Initialize WebSocket hub
	hub := websocket.NewHub(db, redis)
	go hub.Run()

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

	// Initialize handlers
	authHandler := auth.NewHandler(db, redis, log, cfg.JWTSecret, hub)

	// Wire cross-references (breaks circular init dependency)
	authHandler.SetMFAChecker(mfaHandler)
	mfaHandler.SetLoginCompleter(authHandler)
	sessionsHandler := sessions.NewHandler(db, redis, log, hub, mfaHandler)
	usersHandler := users.NewHandler(db, log, hub, mfaHandler)
	serversHandler := servers.NewHandler(db, log, hub)
	channelsHandler := channels.NewHandler(db, log, hub)
	membersHandler := members.NewHandler(db, log, redis, hub)
	messagesHandler := messages.NewHandler(db, log, hub)
	invitesHandler := invites.NewHandler(db, log, hub)
	voiceHandler := voice.NewHandler(db, log, hub, cfg)
	dmHandler := dm.NewHandler(db, log, hub, cfg)
	friendsHandler := friends.NewHandler(db, log, hub)
	wsHandler := websocket.NewHandler(hub, db, redis, cfg.JWTSecret, cfg.AllowedOrigins)
	wsTicketHandler := auth.NewWSTicketHandler(redis, cfg.JWTSecret)

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
		// Auth routes with rate limiting
		authRoutes := v1.Group("/auth")
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
		}

		// Protected routes
		protected := v1.Group("/")
		protected.Use(middleware.AuthRequired(cfg.JWTSecret, redis))
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
			}

			// User routes
			userRoutes := protected.Group("/users")
			{
				userRoutes.GET("/me",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					usersHandler.GetMe,
				)

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
				serverRoutes.GET("/:id/members",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					membersHandler.ListMembers,
				)

				// Add member to server (10 requests per minute)
				serverRoutes.POST("/:id/members",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					membersHandler.AddMember,
				)

				// Update member role (5 requests per minute - sensitive operation)
				serverRoutes.PATCH("/:id/members/:user_id",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					membersHandler.UpdateMember,
				)

				// Remove member from server (5 requests per minute - sensitive operation)
				serverRoutes.DELETE("/:id/members/:user_id",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					membersHandler.RemoveMember,
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
				serverRoutes.POST("/:id/read",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					channelsHandler.MarkServerRead,
				)
			}

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

				// Mark channel as read (30 requests per minute)
				channelRoutes.POST("/:id/read",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					channelsHandler.MarkChannelRead,
				)

				// Get channel encryption keys (for E2EE channels)
				channelRoutes.GET("/:id/keys",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					channelsHandler.GetChannelKeys,
				)

				// Distribute channel keys to new members (E2EE key distribution)
				channelRoutes.POST("/:id/keys",
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

				// Rotate channel encryption key (admin/owner only)
				channelRoutes.POST("/:id/rotate-key",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					channelsHandler.RotateKey,
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

				// Get DM message history (30 requests per minute)
				dmRoutes.GET("/:id/messages",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					dmHandler.GetMessages,
				)

				// Mark DM conversation as read (30 requests per minute)
				dmRoutes.POST("/:id/read",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					dmHandler.MarkRead,
				)

				// DM E2EE key management
				dmRoutes.GET("/:id/keys",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					dmHandler.GetKeys,
				)
				dmRoutes.POST("/:id/keys",
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

		// WebSocket endpoint (requires JWT authentication via query parameter or header)
		v1.GET("/ws", wsHandler.HandleWebSocket)
	}

	return router, hub, natsClient
}
