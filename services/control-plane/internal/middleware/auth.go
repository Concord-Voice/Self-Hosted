// Package middleware provides HTTP middleware for authentication, rate limiting, and CORS.
package middleware

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
)

// authError is the single generic message returned for all authentication failures.
// Detailed diagnostics are intentionally omitted to avoid leaking information.
const authError = "Authentication required"

// AuthRequired returns a middleware that validates JWT tokens and checks the
// token blacklist in Redis. Tokens that have been revoked (e.g. on logout)
// are rejected even if they are otherwise valid.
func AuthRequired(jwtSecret string, redisClient *redis.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": authError})
			c.Abort()
			return
		}

		// Extract token from "Bearer <token>"
		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || parts[0] != "Bearer" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": authError})
			c.Abort()
			return
		}

		tokenString := parts[1]

		// Parse and validate token
		token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
			// Validate signing method
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return []byte(jwtSecret), nil
		})

		if err != nil || !token.Valid {
			c.JSON(http.StatusUnauthorized, gin.H{"error": authError})
			c.Abort()
			return
		}

		// Extract claims
		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": authError})
			c.Abort()
			return
		}

		userID, ok := claims["user_id"].(string)
		if !ok || userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": authError})
			c.Abort()
			return
		}

		// Check token blacklist via JTI
		if jti, ok := claims["jti"].(string); ok && jti != "" {
			ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
			defer cancel()
			if exists, _ := redisClient.Exists(ctx, fmt.Sprintf("blacklist:%s", jti)).Result(); exists > 0 {
				c.JSON(http.StatusUnauthorized, gin.H{"error": authError})
				c.Abort()
				return
			}
		}

		c.Set("user_id", userID)
		c.Next()
	}
}

// BlacklistToken adds a JWT's JTI to the Redis blacklist with a TTL matching
// the token's remaining lifetime. After blacklisting, AuthRequired will reject it.
func BlacklistToken(ctx context.Context, redisClient *redis.Client, jti string, remainingTTL time.Duration) error {
	if jti == "" || remainingTTL <= 0 {
		return nil
	}
	return redisClient.Set(ctx, fmt.Sprintf("blacklist:%s", jti), "1", remainingTTL).Err()
}
