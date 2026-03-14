// Package config provides configuration management for the Control Plane service.
package config

import (
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

const devJWTSecret = "dev_jwt_secret_change_in_production"       // #nosec G101 -- Not a real secret, just the default placeholder
const devMFAEncKey = "0000000000000000000000000000000000000000000000000000000000000000" // #nosec G101 -- 32-byte zero key, dev only

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

	// Client config (#155 Tier 2 + Tier 3)
	ClientMinVersion string // Minimum client version allowed (e.g. "0.1.3"); empty = no enforcement
	FeatureVoice     bool   // Enable voice channels
	FeatureVideo     bool   // Enable video/screen share
	FeatureE2EE      bool   // Enable E2EE encryption
	SpaURL           string // Remote SPA URL for Tier 3 hot updates (e.g. "https://app.concordvoice.chat/v1/"); empty = use bundled
	SpaIpcContract   int    // Minimum IPC contract version required by the remote SPA; 0 = no remote SPA
	SpaConfigFile    string // Path to mounted spa.env for hot-reload; empty = static config from env vars

	// MFA (TOTP + WebAuthn)
	MFAEncryptionKey  string // 32-byte hex-encoded AES key for TOTP secret encryption at rest
	WebAuthnRPID      string // Relying Party ID (domain, e.g. "concordvoice.chat")
	WebAuthnRPOrigins []string // Allowed origins for WebAuthn (e.g. "https://concordvoice.chat")
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
		ClientMinVersion:      getEnv("CLIENT_MIN_VERSION", ""),
		FeatureVoice:          getEnv("FEATURE_VOICE", "true") == "true",
		FeatureVideo:          getEnv("FEATURE_VIDEO", "true") == "true",
		FeatureE2EE:           getEnv("FEATURE_E2EE", "true") == "true",
		SpaURL:                getEnv("SPA_URL", ""),
		SpaIpcContract:        getEnvInt("SPA_IPC_CONTRACT", 0),
		SpaConfigFile:         getEnv("SPA_CONFIG_FILE", ""),
		MFAEncryptionKey:      getEnv("MFA_ENCRYPTION_KEY", devMFAEncKey),
		WebAuthnRPID:          getEnv("WEBAUTHN_RP_ID", "localhost"),
		WebAuthnRPOrigins:     parseOrigins(getEnv("WEBAUTHN_RP_ORIGINS", "http://localhost:3001")),
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
	if c.Environment == "production" && c.MFAEncryptionKey == devMFAEncKey {
		log.Fatal("FATAL: MFA_ENCRYPTION_KEY must be set to a secure value in production. " +
			"The default dev key is not allowed. Generate with: openssl rand -hex 32")
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

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if n, err := strconv.Atoi(value); err == nil {
			return n
		}
	}
	return defaultValue
}
