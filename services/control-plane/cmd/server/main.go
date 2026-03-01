// Package main is the entry point for the Control Plane server.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/concord/control-plane/internal/api"
	"github.com/concord/control-plane/internal/database"
	"github.com/concord/control-plane/internal/websocket"
	"github.com/concord/control-plane/pkg/config"
	"github.com/concord/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func main() {
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Initialize logger
	log := logger.New(cfg.Environment)

	// Initialize database
	db, err := database.New(cfg.DatabaseURL)
	if err != nil {
		log.Fatal("Failed to connect to database", "error", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Error("Error closing database", "error", err)
		}
	}()

	// Run migrations
	if err := database.RunMigrations(db); err != nil {
		log.Fatal("Failed to run migrations", "error", err)
	}

	// Initialize Redis
	redisClient, err := database.NewRedisClient(cfg.RedisURL)
	if err != nil {
		log.Fatal("Failed to connect to Redis", "error", err)
	}
	defer func() {
		if err := redisClient.Close(); err != nil {
			log.Error("Error closing Redis client", "error", err)
		}
	}()

	// Set Gin mode
	if cfg.Environment == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	// Initialize router (starts hub, connects NATS, subscribes to voice events)
	router, hub, natsClient := api.NewRouter(db, redisClient, cfg, log)

	// Start background cleanup job (reaps expired tokens, stale sessions, orphaned presence)
	cleanupCtx, cleanupCancel := context.WithCancel(context.Background())
	defer cleanupCancel()
	go runCleanupJob(cleanupCtx, db, redisClient, hub, log)

	// Create HTTP server
	srv := &http.Server{
		Addr:           fmt.Sprintf(":%s", cfg.Port),
		Handler:        router,
		ReadTimeout:    15 * time.Second,
		WriteTimeout:   15 * time.Second,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1 MB
	}

	// Start server in a goroutine
	go func() {
		log.Info("Starting Control Plane server", "port", cfg.Port, "env", cfg.Environment)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("Server failed to start", "error", err)
		}
	}()

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("Shutting down server...")

	// Stop background cleanup job
	cleanupCancel()

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Shut down WebSocket hub (close all client connections)
	hub.Shutdown()

	// Close NATS connection
	if natsClient != nil {
		natsClient.Close()
	}

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server forced to shutdown", "error", err)
	}

	log.Info("Server exited")
}

// runCleanupJob periodically purges expired tokens, stale sessions, and orphaned
// Redis presence keys. The server must not rely on clients to clean up after
// themselves — this job is the authoritative backstop.
func runCleanupJob(ctx context.Context, db *sql.DB, redisClient *redis.Client, hub *websocket.Hub, log *logger.Logger) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	// Run once at startup to catch anything that accumulated while the server was down
	runCleanup(ctx, db, redisClient, hub, log)

	for {
		select {
		case <-ticker.C:
			runCleanup(ctx, db, redisClient, hub, log)
		case <-ctx.Done():
			log.Info("Cleanup job stopped")
			return
		}
	}
}

func runCleanup(ctx context.Context, db *sql.DB, redisClient *redis.Client, hub *websocket.Hub, log *logger.Logger) {
	taskCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Task 1: Purge naturally expired refresh tokens that were never cleaned up
	// (e.g., user never refreshed, so the per-user opportunistic cleanup never ran)
	res1, err := db.ExecContext(taskCtx,
		`DELETE FROM refresh_tokens WHERE expires_at < NOW() AND revoked_at IS NULL`)
	if err != nil {
		log.Error("Cleanup: failed to purge expired tokens", "error", err)
	} else if n, _ := res1.RowsAffected(); n > 0 {
		log.Info("Cleanup: purged expired tokens", "count", n)
	}

	// Task 2: Purge revoked sessions older than 90 days
	// (global version of the per-user cleanup that runs during token refresh)
	res2, err := db.ExecContext(taskCtx,
		`DELETE FROM refresh_tokens WHERE revoked_at IS NOT NULL AND revoked_at < NOW() - INTERVAL '90 days'`)
	if err != nil {
		log.Error("Cleanup: failed to purge old revoked sessions", "error", err)
	} else if n, _ := res2.RowsAffected(); n > 0 {
		log.Info("Cleanup: purged old revoked sessions", "count", n)
	}

	// Task 3: Clean stale Redis presence keys
	// Compare presence:* keys against the authoritative set of connected users from the hub
	connectedUsers := hub.GetConnectedUsers()
	var staleCount int
	var cursor uint64
	for {
		keys, nextCursor, err := redisClient.Scan(taskCtx, cursor, "presence:*", 100).Result()
		if err != nil {
			log.Error("Cleanup: failed to scan presence keys", "error", err)
			break
		}
		for _, key := range keys {
			// Extract user ID from "presence:<uuid>"
			uidStr := strings.TrimPrefix(key, "presence:")
			uid, parseErr := uuid.Parse(uidStr)
			if parseErr != nil {
				// Malformed key — remove it
				redisClient.Del(taskCtx, key)
				staleCount++
				continue
			}
			if !connectedUsers[uid] {
				redisClient.Del(taskCtx, key)
				staleCount++
			}
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	if staleCount > 0 {
		log.Info("Cleanup: removed stale presence keys", "count", staleCount)
	}

	log.Debug("Cleanup completed")
}
