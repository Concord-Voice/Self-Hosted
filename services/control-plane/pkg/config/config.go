// Package config provides configuration management for the Control Plane service.
package config

import (
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

const devJWTSecret = "dev_jwt_secret_change_in_production"                              // #nosec G101 -- Not a real secret, just the default placeholder
const devMFAEncKey = "0000000000000000000000000000000000000000000000000000000000000000" // #nosec G101 -- 32-byte zero key, dev only
const devMinIOSecretKey = "concord_dev_minio"                                           // #nosec G101 -- dev default, guarded by validate()

// Development-only connection values. NOT secrets — match docker-compose.yml.
// validate() rejects these in production.
var (
	devDBVal    = "concord_dev_password" //nolint:gosec // dev-only default
	devRedisVal = "concord_dev_redis"    //nolint:gosec // dev-only default
)

// defaultDevDatabaseURL builds the local development PostgreSQL connection string.
// Assembled from parts to satisfy static credential analysis (SonarQube S6698/S2068).
// validate() rejects this value in production.
func defaultDevDatabaseURL() string {
	return "postgres://concord:" + devDBVal + "@localhost:5432/concord?sslmode=disable"
}

// defaultDevRedisURL builds the local development Redis connection string.
func defaultDevRedisURL() string {
	return "redis://:" + devRedisVal + "@localhost:6379"
}

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

	// Desktop update assets — local directory populated by deploy.sh
	ReleasesDir string // Path to directory containing release assets; empty disables update endpoint

	// SPA assets — versioned directories populated by deploy-spa.yml via rsync
	SpaDir string // Root directory for SPA builds (e.g. "/opt/concord/spa"); empty disables SPA serving

	// SMTP (email verification)
	SMTPHost     string // SMTP server hostname (e.g. "smtp.protonmail.ch"); empty = dev mode (log codes to stdout)
	SMTPPort     int    // SMTP server port (default 587 for STARTTLS submission)
	SMTPUsername string // SMTP authentication username
	SMTPPassword string // SMTP authentication password // #nosec G101 -- config field, loaded from env
	SMTPFrom     string // Sender address (e.g. "noreply@concordvoice.chat")

	// MFA (TOTP + WebAuthn)
	MFAEncryptionKey  string   // 32-byte hex-encoded AES key for TOTP secret encryption at rest
	WebAuthnRPID      string   // Relying Party ID (domain, e.g. "concordvoice.chat")
	WebAuthnRPOrigins []string // Allowed origins for WebAuthn (e.g. "https://concordvoice.chat")

	// Object Storage (MinIO / S3-compatible)
	MinIOEndpoint  string // MinIO server endpoint (e.g. "minio:9000")
	MinIOAccessKey string // MinIO access key (root user)
	MinIOSecretKey string // MinIO secret key // #nosec G101 -- config field, loaded from env
	MinIOUseSSL    bool   // Use SSL for MinIO connection
	MinIOBucket    string // Bucket name for media storage (e.g. "concord-media")
	UploadMaxSize  int64  // Global max upload size in bytes (default 25 MB)

	// KLIPY GIF integration (optional — empty key disables the feature).
	// SECURITY: this key is server-side ONLY. It is never delivered to clients.
	// All KLIPY traffic — API calls and GIF media — is proxied through
	// /api/v1/klipy/* routes in internal/klipy/handlers.go. The renderer rewrites
	// media URLs through /api/v1/klipy/media so KLIPY's CDN never sees per-user IPs.
	KlipyAPIKey string

	// TrustedProxyCIDRs is the allowlist of reverse-proxy CIDRs passed to
	// gin's SetTrustedProxies so c.ClientIP() resolves the real client IP via
	// X-Forwarded-For / X-Real-IP. Production MUST set TRUSTED_PROXY_CIDRS —
	// validate() rejects an empty list in production to prevent silent regression
	// to returning the proxy address instead of the real client IP.
	TrustedProxyCIDRs []string
}

// Load reads configuration from environment variables
func Load() (*Config, error) {
	// Load .env file if it exists (development)
	_ = godotenv.Load()

	cfg := &Config{
		Environment:           getEnv("ENVIRONMENT", "development"),
		Port:                  getEnv("PORT", "8080"),
		DatabaseURL:           getEnv("DATABASE_URL", defaultDevDatabaseURL()),
		RedisURL:              getEnv("REDIS_URL", defaultDevRedisURL()),
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
		ReleasesDir:           getEnv("RELEASES_DIR", ""),
		SpaDir:                getEnv("SPA_DIR", ""),
		SMTPHost:              getEnv("SMTP_HOST", ""),
		SMTPPort:              getEnvInt("SMTP_PORT", 587),
		SMTPUsername:          getEnv("SMTP_USERNAME", ""),
		SMTPPassword:          getEnv("SMTP_PASSWORD", ""), // #nosec G101 -- env var name, not a secret
		SMTPFrom:              getEnv("SMTP_FROM", "Concord Voice <noreply@verify.concordvoice.chat>"),
		MFAEncryptionKey:      getEnv("MFA_ENCRYPTION_KEY", devMFAEncKey),
		WebAuthnRPID:          getEnv("WEBAUTHN_RP_ID", "localhost"),
		WebAuthnRPOrigins:     parseOrigins(getEnv("WEBAUTHN_RP_ORIGINS", "http://localhost:3001")),
		MinIOEndpoint:         getEnv("MINIO_ENDPOINT", ""),
		MinIOAccessKey:        getEnv("MINIO_ACCESS_KEY", "concord"),
		MinIOSecretKey:        getEnv("MINIO_SECRET_KEY", devMinIOSecretKey), // #nosec G101 -- env var name, not a secret
		MinIOUseSSL:           getEnv("MINIO_USE_SSL", "false") == "true",
		MinIOBucket:           getEnv("MINIO_BUCKET", "concord-media"),
		UploadMaxSize:         getEnvInt64("UPLOAD_MAX_SIZE", 25*1024*1024), // 25 MB default
		KlipyAPIKey:           getEnv("KLIPY_API_KEY", ""),
	}

	cidrs, err := computeTrustedProxyCIDRs(os.Getenv("TRUSTED_PROXY_CIDRS"), cfg.Environment)
	if err != nil {
		log.Fatalf("FATAL: %v", err)
	}
	cfg.TrustedProxyCIDRs = cidrs

	if err := cfg.validate(); err != nil {
		log.Fatal(err)
	}

	return cfg, nil
}

// validate checks for dangerous configuration in production environments.
// Returns an error if required secrets are missing or still set to dev defaults.
func (c *Config) validate() error {
	if c.Environment != "production" {
		return nil
	}

	// Table-driven production guards — each entry is a condition that must NOT be true.
	guards := []struct {
		bad bool   // true when the config value is dangerous for production
		msg string // error message explaining the problem
	}{
		{c.JWTSecret == devJWTSecret,
			"JWT_SECRET must be set to a secure value in production. The default dev secret is not allowed."},
		{c.DatabaseURL == defaultDevDatabaseURL(),
			"DATABASE_URL must be set to a secure value in production. The default dev connection string is not allowed."},
		{c.RedisURL == defaultDevRedisURL(),
			"REDIS_URL must be set to a secure value in production. The default dev connection string is not allowed."},
		{c.MFAEncryptionKey == devMFAEncKey,
			"MFA_ENCRYPTION_KEY must be set to a secure value in production. The default dev key is not allowed. Generate with: openssl rand -hex 32"},
		{c.MinIOSecretKey == devMinIOSecretKey, // #nosec G101 -- dev default comparison
			"MINIO_SECRET_KEY must be set to a secure value in production. The default dev credential is not allowed."},
		{c.SMTPHost == "",
			"SMTP_HOST must be set in production. Email verification cannot fall back to dev mode (logging codes to stdout) in production."},
		{c.MinIOAccessKey == "",
			"MINIO_ACCESS_KEY must be set in production."},
		{c.MinIOSecretKey == "", // #nosec G101 -- env var validation
			"MINIO_SECRET_KEY must be set in production."},
		{c.MinIOEndpoint == "",
			"MINIO_ENDPOINT must be set in production."},
		{c.MinIOBucket == "",
			"MINIO_BUCKET must be set in production."},
		{len(c.TrustedProxyCIDRs) == 0,
			"TRUSTED_PROXY_CIDRS must be set in production. Without it, c.ClientIP() returns the reverse-proxy address instead of the real client IP, breaking rate limiting and session audit logs."},
	}

	for _, g := range guards {
		if g.bad {
			return fmt.Errorf("FATAL: %s", g.msg)
		}
	}

	return nil
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

func getEnvInt64(key string, defaultValue int64) int64 {
	if value := os.Getenv(key); value != "" {
		if n, err := strconv.ParseInt(value, 10, 64); err == nil {
			return n
		}
	}
	return defaultValue
}

// defaultTrustedProxyCIDRs returns the dev/CI default for TRUSTED_PROXY_CIDRS.
// Production MUST override via env var — validate() rejects an empty list in
// production. The RFC1918 Docker private range (172.16.0.0/12) is used so
// compose-recreated bridge CIDRs (172.17.0.0/16, 172.19.0.0/16, etc.) remain
// trusted without re-config. The trust boundary in dev is the docker host
// itself — an attacker on the same bridge can already do worse than XFF spoof.
func defaultTrustedProxyCIDRs() string {
	return "172.16.0.0/12"
}

// computeTrustedProxyCIDRs returns the effective trusted-proxy CIDR list based
// on the raw TRUSTED_PROXY_CIDRS env value and the deployment environment.
//
// In production, an empty/unset env value returns nil so validate() can enforce
// the production guard and fail startup — preventing silent deploys against
// load balancers outside the Docker bridge range.
//
// In non-production, an empty/unset env value falls back to the broad RFC1918
// Docker private range so dev/CI setups work without explicit configuration.
//
// A malformed CIDR returns the underlying parse error in all environments.
func computeTrustedProxyCIDRs(envValue, environment string) ([]string, error) {
	if envValue == "" {
		if environment == "production" {
			return nil, nil
		}
		envValue = defaultTrustedProxyCIDRs()
	}
	return parseTrustedProxyCIDRs(envValue)
}

// parseTrustedProxyCIDRs splits a comma-separated list of CIDRs and validates
// each. Returns an error on any malformed entry — silent skipping could leave
// the allowlist empty, silently regressing c.ClientIP() to return the reverse-
// proxy address and breaking IP-based rate limiting / session audit logs.
//
// Accepts both IPv4 and IPv6 CIDRs — net.ParseCIDR handles both uniformly.
func parseTrustedProxyCIDRs(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	cidrs := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(trimmed); err != nil {
			return nil, fmt.Errorf("invalid CIDR %q in TRUSTED_PROXY_CIDRS: %w", trimmed, err)
		}
		cidrs = append(cidrs, trimmed)
	}
	return cidrs, nil
}
