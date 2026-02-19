package websocket

import (
	"database/sql"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Handler handles WebSocket connections
type Handler struct {
	hub            *Hub
	db             *sql.DB
	jwtSecret      string
	allowedOrigins []string
	upgrader       websocket.Upgrader
}

// NewHandler creates a new WebSocket handler
func NewHandler(hub *Hub, db *sql.DB, jwtSecret string, allowedOrigins []string) *Handler {
	h := &Handler{
		hub:            hub,
		db:             db,
		jwtSecret:      jwtSecret,
		allowedOrigins: allowedOrigins,
	}
	h.upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			if origin == "" {
				return true // Allow connections without Origin header (native apps, CLI tools)
			}
			for _, allowed := range h.allowedOrigins {
				if origin == allowed {
					return true
				}
			}
			return false
		},
	}
	return h
}

// HandleWebSocket upgrades HTTP connections to WebSocket
func (h *Handler) HandleWebSocket(c *gin.Context) {
	// Extract and validate JWT token
	userID, err := h.authenticateWebSocket(c)
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

// authenticateWebSocket validates JWT token from WebSocket connection
func (h *Handler) authenticateWebSocket(c *gin.Context) (uuid.UUID, error) {
	// Try to get token from query parameter first (for WebSocket connections)
	tokenString := c.Query("token")

	// Fall back to Authorization header
	if tokenString == "" {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			return uuid.Nil, jwt.ErrSignatureInvalid
		}

		// Remove "Bearer " prefix
		if len(authHeader) > 7 && strings.ToLower(authHeader[0:7]) == "bearer " {
			tokenString = authHeader[7:]
		} else {
			return uuid.Nil, jwt.ErrSignatureInvalid
		}
	}

	// Parse and validate token
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		// Verify signing method
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return []byte(h.jwtSecret), nil
	})

	if err != nil {
		return uuid.Nil, err
	}

	// Extract user ID from claims
	if claims, ok := token.Claims.(jwt.MapClaims); ok && token.Valid {
		userIDStr, ok := claims["user_id"].(string)
		if !ok {
			return uuid.Nil, jwt.ErrTokenInvalidClaims
		}

		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return uuid.Nil, err
		}

		return userID, nil
	}

	return uuid.Nil, jwt.ErrSignatureInvalid
}

// GetHub returns the hub instance (for external access)
func (h *Handler) GetHub() *Hub {
	return h.hub
}
