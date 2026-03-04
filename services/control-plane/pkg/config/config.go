// Package config provides configuration management for the Control Plane service.
package config

import (
	"log"
	"os"
	"strings"

	"github.com/joho/godotenv"
)

const devJWTSecret = "dev_jwt_secret_change_in_production" // #nosec G101 -- Not a real secret, just the default placeholder

// Config holds all configuration for the application
type Config struct {
	Environment string
	Port        string
	DatabaseURL string
	RedisURL    string
	JWTSecret   string // #nosec G117 -- False positive: config field name, actual secret loaded from env
	NATSUrl     string

	// CORS settings
	AllowedOrigins []string

	// Media plane
	MediaPlaneURL string

	// TURN/STUN (coturn)
	TURNServerHost string // Hostname or IP of the TURN server (e.g. "turn.concordvoice.chat")
	TURNSecret     string // Shared secret for HMAC ephemeral credentials
	TURNRealm      string // TURN realm (e.g. "concordvoice.chat")

	// Licensing
	LicensingAuthorityURL string
}

// Load reads configuration from environment variables
func Load() (*Config, error) {
	// Load .env file if it exists (development)
	_ = godotenv.Load()

	cfg := &Config{
		Environment:           getEnv("ENVIRONMENT", "development"),
		Port:                  getEnv("PORT", "8080"),
		DatabaseURL:           getEnv("DATABASE_URL", "postgres://concord:concord_dev_password@localhost:5432/concord?sslmode=disable"),
		RedisURL:              getEnv("REDIS_URL", "redis://:concord_dev_redis@localhost:6379"),
		JWTSecret:             getEnv("JWT_SECRET", devJWTSecret),
		NATSUrl:               getEnv("NATS_URL", "nats://localhost:4222"),
		MediaPlaneURL:         getEnv("MEDIA_PLANE_URL", "http://localhost:3000"),
		TURNServerHost:        getEnv("TURN_SERVER_HOST", ""),
		TURNSecret:            getEnv("TURN_SECRET", ""),
		TURNRealm:             getEnv("TURN_REALM", "concordvoice.chat"),
		LicensingAuthorityURL: getEnv("LICENSING_AUTHORITY_URL", "http://localhost:8082"),
		AllowedOrigins:        parseOrigins(getEnv("ALLOWED_ORIGINS", "http://localhost:3001,http://localhost:3002")),
	}

	cfg.validate()

	return cfg, nil
}

// validate checks for dangerous configuration in production environments.
// Fatally exits if a real JWT_SECRET is not set in production.
func (c *Config) validate() {
	if c.Environment == "production" && c.JWTSecret == devJWTSecret {
		log.Fatal("FATAL: JWT_SECRET must be set to a secure value in production. " +
			"The default dev secret is not allowed.")
	}
}

// parseOrigins splits a comma-separated list of origins into a slice.
func parseOrigins(raw string) []string {
	parts := strings.Split(raw, ",")
	origins := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			origins = append(origins, trimmed)
		}
	}
	return origins
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
