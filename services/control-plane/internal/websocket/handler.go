package websocket

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/concord/control-plane/internal/auth"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
)

// Handler handles WebSocket connections
type Handler struct {
	hub            *Hub
	db             *sql.DB
	redis          *redis.Client
	jwtSecret      string
	allowedOrigins []string
	upgrader       websocket.Upgrader
}

// NewHandler creates a new WebSocket handler
func NewHandler(hub *Hub, db *sql.DB, redisClient *redis.Client, jwtSecret string, allowedOrigins []string) *Handler {
	h := &Handler{
		hub:            hub,
		db:             db,
		redis:          redisClient,
		jwtSecret:      jwtSecret,
		allowedOrigins: allowedOrigins,
	}
	h.upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			if origin == "" || origin == "null" {
				return true // Allow connections without Origin header (native apps, CLI tools, packaged Electron)
			}
			for _, allowed := range h.allowedOrigins {
				if allowed == "*" || origin == allowed {
					return true
				}
			}
			log.Printf("[websocket] Origin rejected (allowed: %v)", h.allowedOrigins)
			return false
		},
	}
	return h
}

// HandleWebSocket upgrades HTTP connections to WebSocket
func (h *Handler) HandleWebSocket(c *gin.Context) {
	// Authenticate: prefer ticket-based auth, fall back to JWT for backward compat
	userID, sessionID, err := h.authenticateWebSocket(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	// Look up username, display name, and avatar for this user
	var username string
	var displayName *string
	var avatarURL *string
	if err := h.db.QueryRow("SELECT username, display_name, avatar_url FROM users WHERE id = $1", userID).Scan(&username, &displayName, &avatarURL); err != nil {
		log.Printf("Failed to look up username for user %s: %v", userID, err)
		username = "Unknown"
	}

	// Upgrade HTTP connection to WebSocket
	// Origin is validated by h.upgrader.CheckOrigin (configured in NewHandler)
	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil) // nosemgrep: go.gorilla.security.audit.websocket-missing-origin-check.websocket-missing-origin-check
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to upgrade connection"})
		return
	}

	// Create new client
	client := &Client{
		ID:          uuid.New(),
		UserID:      userID,
		SessionID:   sessionID,
		Username:    username,
		DisplayName: displayName,
		AvatarURL:   avatarURL,
		Conn:        conn,
		Hub:         h.hub,
		Send:        make(chan []byte, 256),
		Channels:    make(map[uuid.UUID]bool),
	}

	// Register client with hub
	h.hub.register <- client

	// Start client's read and write pumps
	go client.writePump()
	go client.readPump()
}

// authenticateWebSocket validates the connection using ticket-based auth (preferred)
// or falls back to JWT for backward compatibility. Returns (userID, sessionID, error).
// sessionID is only populated for ticket-based auth when the client provided X-Session-ID.
func (h *Handler) authenticateWebSocket(c *gin.Context) (uuid.UUID, string, error) {
	// Prefer ticket-based auth (short-lived, single-use, not logged in URLs)
	if ticket := c.Query("ticket"); ticket != "" {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		userIDStr, sessionID, err := auth.ValidateTicket(ctx, h.redis, ticket)
		if err != nil {
			return uuid.Nil, "", fmt.Errorf("invalid ticket: %w", err)
		}
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return uuid.Nil, "", fmt.Errorf("invalid user ID in ticket: %w", err)
		}
		return userID, sessionID, nil
	}

	// Fall back to JWT — check Authorization header first, then ?token= query param
	tokenString := ""
	if authHeader := c.GetHeader("Authorization"); authHeader != "" {
		if len(authHeader) > 7 && strings.ToLower(authHeader[0:7]) == "bearer " {
			tokenString = authHeader[7:]
		}
	}
	if tokenString == "" {
		// WebSocket connections from browsers can't set custom headers,
		// so also accept JWT via ?token= query param
		tokenString = c.Query("token")
	}

	if tokenString == "" {
		return uuid.Nil, "", fmt.Errorf("no authentication provided")
	}

	// Parse and validate JWT
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return []byte(h.jwtSecret), nil
	})

	if err != nil {
		return uuid.Nil, "", err
	}

	if claims, ok := token.Claims.(jwt.MapClaims); ok && token.Valid {
		// Check token blacklist (matches AuthRequired middleware behavior)
		if jti, ok := claims["jti"].(string); ok && jti != "" {
			ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
			defer cancel()
			if exists, _ := h.redis.Exists(ctx, fmt.Sprintf("blacklist:%s", jti)).Result(); exists > 0 {
				return uuid.Nil, "", fmt.Errorf("token has been revoked")
			}
		}

		userIDStr, ok := claims["user_id"].(string)
		if !ok {
			return uuid.Nil, "", jwt.ErrTokenInvalidClaims
		}
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return uuid.Nil, "", err
		}
		return userID, "", nil
	}

	return uuid.Nil, "", fmt.Errorf("invalid authentication")
}
