package rbac

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// RequireMembership is middleware that checks if the authenticated user is a member of the server
// Expects:
// - user_id in context (set by auth middleware)
// - :id param in route (server ID)
func RequireMembership(resolver *Resolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString("user_id")
		serverID := c.Param("id")

		if userID == "" || serverID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			c.Abort()
			return
		}

		// Check membership via resolver (will return ErrNotMember if not a member)
		_, err := resolver.GetEffectivePermissions(c.Request.Context(), serverID, userID, "")
		if errors.Is(err, ErrNotMember) {
			c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this server"})
			c.Abort()
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to check membership"})
			c.Abort()
			return
		}

		c.Next()
	}
}

// RequirePermission is middleware that checks if the authenticated user has a specific permission
// Expects:
// - user_id in context (set by auth middleware)
// - :id param in route (server ID)
// - channelID param: if checking channel-specific permissions (pass channelID), otherwise pass empty string
func RequirePermission(resolver *Resolver, perm Permission, channelIDParam string) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString("user_id")
		serverID := c.Param("id")

		if userID == "" || serverID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			c.Abort()
			return
		}

		// Extract channelID from param if specified
		channelID := ""
		if channelIDParam != "" {
			channelID = c.Param(channelIDParam)
		}

		// Check permission
		hasPerm, err := resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, perm)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to check permissions"})
			c.Abort()
			return
		}

		if !hasPerm {
			c.JSON(http.StatusForbidden, gin.H{"error": "Insufficient permissions"})
			c.Abort()
			return
		}

		c.Next()
	}
}
