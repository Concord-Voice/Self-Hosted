// Package config provides configuration management for the Control Plane service.
package config

import (
	"fmt"
	"log"
	"net"
	"net/url"
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

// GoogleSSOConfig holds Google Sign-In credentials. Empty when SSO is disabled.
// ClientSecret is treated as sensitive — String() redacts it so logging the
// struct never prints the secret.
type GoogleSSOConfig struct {
	Enabled      bool
	ClientID     string
	ClientSecret string // #nosec G117 G101 -- False positive: config field name, actual secret loaded from env
}

// String renders the config with the secret redacted, suitable for logs.
// Renders "[empty]" when no secret is set so empty-vs-redacted is unambiguous
// in operator-facing output.
func (g GoogleSSOConfig) String() string {
	secret := "[empty]"
	if g.ClientSecret != "" {
		secret = "[REDACTED]"
	}
	return fmt.Sprintf("GoogleSSOConfig{Enabled:%t ClientID:%q ClientSecret:%s}",
		g.Enabled, g.ClientID, secret)
}

// AppleSSOConfig holds Apple Sign-In credentials. Empty when SSO is disabled.
// PrivateKey is the PEM-encoded contents of the .p8 key file from Apple
// Developer. Two acceptable env-var formats:
//
//	APPLE_PRIVATE_KEY="<inline PEM with BEGIN/END PRIVATE KEY markers, newlines escaped as \n>"
//	APPLE_PRIVATE_KEY="@/etc/concord/apple_signin.p8"     (file path prefixed with @)
//
// String() redacts PrivateKey to never log key material.
type AppleSSOConfig struct {
	Enabled  bool
	ClientID string // Services ID, e.g. "chat.concordvoice.signin"
	TeamID   string // 10-char Team ID
	KeyID    string // 10-char Key ID
	// PrivateKey holds PEM bytes (resolved at load time if @-prefixed).
	PrivateKey []byte // #nosec G117 G101 -- False positive: config field name, actual key loaded from env
}

// String renders the config with the private key redacted, suitable for logs.
// The byte count is included so operators can distinguish empty-vs-populated
// keys in logs without exposing the key material itself.
func (a AppleSSOConfig) String() string {
	if !a.Enabled {
		return "AppleSSOConfig{Enabled:false}"
	}
	return fmt.Sprintf("AppleSSOConfig{Enabled:true ClientID:%q TeamID:%q KeyID:%q PrivateKey:[REDACTED %d bytes]}",
		a.ClientID, a.TeamID, a.KeyID, len(a.PrivateKey))
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

	// GoogleSSO holds Google Sign-In credentials (#270). Disabled by default.
	// Load() returns an error if Enabled=true but credentials are unset, so
	// misconfiguration is caught at startup in any environment.
	GoogleSSO GoogleSSOConfig

	// AppleSSO holds Apple Sign-In credentials (#271). Disabled by default.
	// Load() returns an error if Enabled=true but any of the four required
	// fields (ClientID, TeamID, KeyID, PrivateKey) is unset, so misconfiguration
	// is caught at startup in any environment. The error message lists ALL
	// missing fields at once rather than failing on the first.
	AppleSSO AppleSSOConfig
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
		AllowedOrigins:        parseOrigins(getEnv("ALLOWED_ORIGINS", "http://localhost:3001,http://localhost:3002,app://concord")),
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

	// Google SSO (#270). Disabled by default. When enabled, both the client
	// ID and secret must be set — return an error from Load() in any
	// environment so misconfiguration surfaces at startup instead of failing
	// on first OAuth callback.
	cfg.GoogleSSO.Enabled = getEnv("GOOGLE_SSO_ENABLED", "false") == "true"
	cfg.GoogleSSO.ClientID = os.Getenv("GOOGLE_CLIENT_ID")
	cfg.GoogleSSO.ClientSecret = os.Getenv("GOOGLE_CLIENT_SECRET")
	if cfg.GoogleSSO.Enabled {
		if cfg.GoogleSSO.ClientID == "" || cfg.GoogleSSO.ClientSecret == "" {
			return nil, fmt.Errorf("GoogleSSO enabled but GOOGLE_CLIENT_ID or GOOGLE_CLIENT_SECRET unset")
		}
	}

	// Apple SSO (#271). Disabled by default. When enabled, all four fields
	// (ClientID, TeamID, KeyID, PrivateKey) must be set. The production guard
	// accumulates ALL missing fields rather than failing on the first, so
	// operators see every problem in one error.
	if err := loadAppleSSOConfig(cfg); err != nil {
		return nil, err
	}

	if err := cfg.validate(); err != nil {
		log.Fatal(err)
	}

	return cfg, nil
}

// loadAppleSSOConfig populates cfg.AppleSSO from environment variables and
// enforces the production guard. Extracted from Load() to keep that function's
// cognitive complexity below the 15 SonarQube threshold.
//
// PrivateKey accepts two transport formats:
//   - Literal PEM with \n escapes (.env files / K8s ConfigMaps store
//     multiline values as single lines; we restore real newlines)
//   - @/path/to/file.p8 — the contents of the file are read at load time
//     (preferred for production: file is mounted via secrets manager /
//     systemd LoadCredential)
//
// When Enabled=true, all four fields (ClientID, TeamID, KeyID, PrivateKey)
// must be set. The production guard accumulates ALL missing fields rather
// than failing on the first, so operators see every problem in one error.
//
// Disabled deployments early-return BEFORE touching the filesystem so that
// stale APPLE_PRIVATE_KEY=@/path env vars from a prior enabled deployment
// don't cause startup to fail when SSO has been turned off.
func loadAppleSSOConfig(cfg *Config) error {
	cfg.AppleSSO.Enabled = getEnv("APPLE_SSO_ENABLED", "false") == "true"
	if !cfg.AppleSSO.Enabled {
		return nil
	}

	cfg.AppleSSO.ClientID = os.Getenv("APPLE_CLIENT_ID")
	cfg.AppleSSO.TeamID = os.Getenv("APPLE_TEAM_ID")
	cfg.AppleSSO.KeyID = os.Getenv("APPLE_KEY_ID")

	rawAppleKey := os.Getenv("APPLE_PRIVATE_KEY")
	if strings.HasPrefix(rawAppleKey, "@") {
		// TrimSpace on the after-`@` portion: APPLE_PRIVATE_KEY="@" or
		// "@   " is treated as missing-PrivateKey and falls through to the
		// production-guard accumulation below — symmetric with the
		// whitespace gate on the literal-PEM branch and avoids a confusing
		// "no such file or directory: \"\"" error from os.ReadFile.
		path := strings.TrimSpace(strings.TrimPrefix(rawAppleKey, "@"))
		if path != "" {
			b, err := os.ReadFile(path) // #nosec G304 G703 -- path is operator-supplied via env var, by design
			if err != nil {
				return fmt.Errorf("APPLE_PRIVATE_KEY file: %w", err)
			}
			cfg.AppleSSO.PrivateKey = b
		}
	} else if strings.TrimSpace(rawAppleKey) != "" {
		// Literal PEM; allow newline escapes from .env files.
		// strings.TrimSpace gate ensures whitespace-only env values
		// (e.g., APPLE_PRIVATE_KEY="   ") are treated as missing rather
		// than passing through to NewAppleProvider, which would surface
		// as a confusing PEM-parse error far from the actual misconfig.
		cfg.AppleSSO.PrivateKey = []byte(strings.ReplaceAll(rawAppleKey, `\n`, "\n"))
	}

	var missing []string
	if cfg.AppleSSO.ClientID == "" {
		missing = append(missing, "APPLE_CLIENT_ID")
	}
	if cfg.AppleSSO.TeamID == "" {
		missing = append(missing, "APPLE_TEAM_ID")
	}
	if cfg.AppleSSO.KeyID == "" {
		missing = append(missing, "APPLE_KEY_ID")
	}
	if len(cfg.AppleSSO.PrivateKey) == 0 {
		missing = append(missing, "APPLE_PRIVATE_KEY")
	}
	if len(missing) > 0 {
		return fmt.Errorf("AppleSSO enabled but %s unset", strings.Join(missing, ", "))
	}
	return nil
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
		// #725 review: MEDIA_PLANE_URL guard parity with other dev-default /
		// unset checks. Rejects empty, dev-default, and root-zone variants.
		{c.MediaPlaneURL == "",
			"MEDIA_PLANE_URL must be set in production. Leaving it unset breaks WebRTC signaling."},
		{c.MediaPlaneURL == "http://localhost:3000",
			"MEDIA_PLANE_URL must be set to a production value. The dev default http://localhost:3000 is not allowed."},
		{mediaPlaneURLIsRoot(c.MediaPlaneURL),
			"MEDIA_PLANE_URL must point to the media subdomain (e.g. https://media.concordvoice.chat), not the root concordvoice.chat. The root zone is DNS-only and 301-redirects through nginx to the .com marketing site, breaking WebRTC signaling. This guard catches variants including trailing slash, explicit port, path suffix, uppercase, and wss:// per #725 review."},
	}

	for _, g := range guards {
		if g.bad {
			return fmt.Errorf("FATAL: %s", g.msg)
		}
	}

	// Non-fatal: warn when production is using the broad RFC1918 fallback
	// for trusted proxies (the provision-secrets.yml heredoc default). This
	// is functional but permissive — see broadRFC1918CIDRs() for context.
	if equalCIDRs(c.TrustedProxyCIDRs, broadRFC1918CIDRs()) {
		log.Printf("WARNING: TRUSTED_PROXY_CIDRS is the broad RFC1918 fallback (%s). Tighten via GH variable for stricter X-Forwarded-For trust. See [internal]sso-secrets-management.md.", strings.Join(c.TrustedProxyCIDRs, ","))
	}

	return nil
}

// mediaPlaneURLIsRoot returns true when the given MediaPlaneURL resolves
// to the zone-root host concordvoice.chat (with or without www.). Uses
// net/url parsing + case-folded host comparison so the following are all
// caught: trailing slash, explicit :443, path suffix (/api, etc.),
// uppercase variants, wss:// scheme. The root is DNS-only and 301-
// redirects through nginx to the .com marketing site, breaking WebRTC
// signaling — none of these variants are valid production values.
//
// Does NOT flag media.concordvoice.chat or any other subdomain. Does NOT
// flag empty-string — that's the responsibility of a separate "unset"
// guard in validate(). Does NOT flag parse-failure — malformed URLs are
// a different class of bug and will fail downstream; here we only want
// to catch the specific root-host misconfiguration.
//
// Per #725 review (Copilot + Seer + 3 subagents).
func mediaPlaneURLIsRoot(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "concordvoice.chat" || host == "www.concordvoice.chat"
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

// broadRFC1918CIDRs returns the conservative fallback used by the
// provision-secrets workflow when TRUSTED_PROXY_CIDRS is unset. Trusting
// this entire range in production is operationally safer than failing
// startup, but it permits any RFC1918 source (including Docker bridge
// neighbors) to spoof X-Forwarded-For. validate() emits a warning when
// production lands on this fallback so operators are aware to tighten it
// via `gh variable set TRUSTED_PROXY_CIDRS --env production --body '<CIDR>'`.
func broadRFC1918CIDRs() []string {
	return []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
}

// equalCIDRs returns true if a and b contain the same CIDR strings in
// the same order. Used by validate() to detect the broad-RFC1918 fallback.
func equalCIDRs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
