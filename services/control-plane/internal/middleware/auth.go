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
		tokenString, ok := extractBearerToken(c.GetHeader("Authorization"))
		if !ok {
			abortUnauthorized(c)
			return
		}

		claims, ok := parseAndValidateJWT(tokenString, jwtSecret)
		if !ok {
			abortUnauthorized(c)
			return
		}

		userID, ok := claims["user_id"].(string)
		if !ok || userID == "" {
			abortUnauthorized(c)
			return
		}

		if isTokenBlacklisted(c, redisClient, claims) {
			abortUnauthorized(c)
			return
		}

		c.Set("user_id", userID)
		c.Set("email_verified", emailVerifiedFromClaims(claims))
		c.Next()
	}
}

func abortUnauthorized(c *gin.Context) {
	c.JSON(http.StatusUnauthorized, gin.H{"error": authError})
	c.Abort()
}

func extractBearerToken(authHeader string) (string, bool) {
	parts := strings.Split(authHeader, " ")
	if len(parts) != 2 || parts[0] != "Bearer" {
		return "", false
	}
	return parts[1], true
}

func parseAndValidateJWT(tokenString, jwtSecret string) (jwt.MapClaims, bool) {
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return []byte(jwtSecret), nil
	})
	if err != nil || !token.Valid {
		return nil, false
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	return claims, ok
}

func isTokenBlacklisted(c *gin.Context, redisClient *redis.Client, claims jwt.MapClaims) bool {
	jti, ok := claims["jti"].(string)
	if !ok || jti == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	exists, err := redisClient.Exists(ctx, fmt.Sprintf("blacklist:%s", jti)).Result()
	if err != nil {
		return true // fail closed: treat as blacklisted when Redis is unavailable
	}
	return exists > 0
}

func emailVerifiedFromClaims(claims jwt.MapClaims) bool {
	if ev, ok := claims["email_verified"].(bool); ok {
		return ev
	}
	return true
}

// RequireVerifiedEmail returns a middleware that blocks requests from users
// whose email has not been verified. Must be applied AFTER AuthRequired.
func RequireVerifiedEmail() gin.HandlerFunc {
	return func(c *gin.Context) {
		verified, exists := c.Get("email_verified")
		v, ok := verified.(bool)
		if !exists || !ok || !v {
			c.JSON(http.StatusForbidden, gin.H{
				"error": "Email verification required",
				"code":  "EMAIL_NOT_VERIFIED",
			})
			c.Abort()
			return
		}
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
