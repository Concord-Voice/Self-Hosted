// Package api provides HTTP routing and middleware setup for the Control Plane REST API.
package api // revive:disable-line:var-naming

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/concord/control-plane/internal/auth"
	"github.com/concord/control-plane/internal/channels"
	"github.com/concord/control-plane/internal/invites"
	"github.com/concord/control-plane/internal/members"
	"github.com/concord/control-plane/internal/messages"
	"github.com/concord/control-plane/internal/middleware"
	"github.com/concord/control-plane/internal/servers"
	"github.com/concord/control-plane/internal/sessions"
	"github.com/concord/control-plane/internal/users"
	"github.com/concord/control-plane/internal/websocket"
	"github.com/concord/control-plane/pkg/config"
	"github.com/concord/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// NewRouter creates a new API router
func NewRouter(db *sql.DB, redis *redis.Client, cfg *config.Config, log *logger.Logger) *gin.Engine {
	router := gin.New()

	// Middleware
	router.Use(gin.Recovery())
	router.Use(middleware.Logger(log))
	router.Use(middleware.CORS(cfg.AllowedOrigins))

	// Health check
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status": "healthy",
			"service": "control-plane",
		})
	})

	// Initialize WebSocket hub
	hub := websocket.NewHub(db)
	go hub.Run()

	// Initialize handlers
	authHandler := auth.NewHandler(db, log, cfg.JWTSecret)
	sessionsHandler := sessions.NewHandler(db, log)
	usersHandler := users.NewHandler(db, log)
	serversHandler := servers.NewHandler(db, log)
	channelsHandler := channels.NewHandler(db, log)
	membersHandler := members.NewHandler(db, log)
	messagesHandler := messages.NewHandler(db, log, hub)
	invitesHandler := invites.NewHandler(db, log, hub)
	wsHandler := websocket.NewHandler(hub, db, cfg.JWTSecret, cfg.AllowedOrigins)

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
		}

		// Protected routes
		protected := v1.Group("/")
		protected.Use(middleware.AuthRequired(cfg.JWTSecret))
		{
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

				// Revoke all other sessions (5 requests per minute - more sensitive)
				sessionRoutes.DELETE("",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					sessionsHandler.RevokeAllSessions,
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
		}

		// WebSocket endpoint (requires JWT authentication via query parameter or header)
		v1.GET("/ws", wsHandler.HandleWebSocket)
	}

	return router
}
