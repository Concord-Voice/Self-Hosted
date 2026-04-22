package config

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetEnv(t *testing.T) {
	t.Run("returns env value when set", func(t *testing.T) {
		t.Setenv("TEST_CONFIG_VAR", "custom_value")

		assert.Equal(t, "custom_value", getEnv("TEST_CONFIG_VAR", "default"))
	})

	t.Run("returns default when not set", func(t *testing.T) {
		assert.Equal(t, "default_value", getEnv("NONEXISTENT_CONFIG_VAR", "default_value"))
	})

	t.Run("empty env value uses default", func(t *testing.T) {
		t.Setenv("TEST_EMPTY_VAR", "")

		assert.Equal(t, "fallback", getEnv("TEST_EMPTY_VAR", "fallback"))
	})
}

func TestParseOrigins(t *testing.T) {
	t.Run("single origin", func(t *testing.T) {
		origins := parseOrigins("http://localhost:3001")
		assert.Equal(t, []string{"http://localhost:3001"}, origins)
	})

	t.Run("multiple origins", func(t *testing.T) {
		origins := parseOrigins("http://localhost:3001,http://localhost:3002")
		assert.Equal(t, []string{"http://localhost:3001", "http://localhost:3002"}, origins)
	})

	t.Run("trims whitespace", func(t *testing.T) {
		origins := parseOrigins("http://localhost:3001 , http://localhost:3002 ")
		assert.Equal(t, []string{"http://localhost:3001", "http://localhost:3002"}, origins)
	})

	t.Run("filters empty strings", func(t *testing.T) {
		origins := parseOrigins("http://localhost:3001,,http://localhost:3002")
		assert.Equal(t, []string{"http://localhost:3001", "http://localhost:3002"}, origins)
	})

	t.Run("empty input returns empty slice", func(t *testing.T) {
		origins := parseOrigins("")
		assert.Empty(t, origins)
	})
}

func TestLoad(t *testing.T) {
	// Save and unset all env vars to test defaults
	envVars := []string{"ENVIRONMENT", "PORT", "DATABASE_URL", "REDIS_URL", "JWT_SECRET", "NATS_URL", "ALLOWED_ORIGINS", "TRUSTED_PROXY_CIDRS"}
	saved := make(map[string]string)
	for _, key := range envVars {
		saved[key] = os.Getenv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("failed to unset %s: %v", key, err)
		}
	}
	t.Cleanup(func() {
		for k, v := range saved {
			if v != "" {
				_ = os.Setenv(k, v) //nolint:errcheck
			}
		}
	})

	t.Run("default values", func(t *testing.T) {
		cfg, err := Load()
		require.NoError(t, err)

		assert.Equal(t, "development", cfg.Environment)
		assert.Equal(t, "8080", cfg.Port)
		assert.Contains(t, cfg.DatabaseURL, "postgres://")
		assert.Contains(t, cfg.RedisURL, "redis://")
		assert.NotEmpty(t, cfg.JWTSecret)
		assert.Contains(t, cfg.NATSUrl, "nats://")
		assert.NotEmpty(t, cfg.AllowedOrigins)
		assert.Equal(t, []string{"172.16.0.0/12"}, cfg.TrustedProxyCIDRs)
	})

	t.Run("env overrides", func(t *testing.T) {
		t.Setenv("PORT", "9090")
		t.Setenv("ENVIRONMENT", "staging")

		cfg, err := Load()
		require.NoError(t, err)

		assert.Equal(t, "9090", cfg.Port)
		assert.Equal(t, "staging", cfg.Environment)
	})
}

func TestLoadTrustedProxyCIDRs(t *testing.T) {
	// Pin ENVIRONMENT=development in every sub-test so Load() doesn't hit the
	// production guards (and fatally exit on unrelated missing prod config)
	// if the CI runner has ENVIRONMENT=production set. Scoped to each sub-test
	// via t.Setenv so it auto-restores on exit.
	t.Run("uses env var when set", func(t *testing.T) {
		t.Setenv("ENVIRONMENT", "development")
		t.Setenv("TRUSTED_PROXY_CIDRS", "10.0.0.0/8,192.168.0.0/16")
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, []string{"10.0.0.0/8", "192.168.0.0/16"}, cfg.TrustedProxyCIDRs)
	})

	t.Run("uses default when unset", func(t *testing.T) {
		t.Setenv("ENVIRONMENT", "development")
		t.Setenv("TRUSTED_PROXY_CIDRS", "")
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, []string{"172.16.0.0/12"}, cfg.TrustedProxyCIDRs)
	})
}

func TestComputeTrustedProxyCIDRs(t *testing.T) {
	t.Run("empty env in production returns nil", func(t *testing.T) {
		got, err := computeTrustedProxyCIDRs("", "production")
		require.NoError(t, err)
		assert.Nil(t, got, "production with no env value must leave the slice empty so validate() can fail startup")
	})

	t.Run("empty env in development falls back to default", func(t *testing.T) {
		got, err := computeTrustedProxyCIDRs("", "development")
		require.NoError(t, err)
		assert.Equal(t, []string{"172.16.0.0/12"}, got)
	})

	t.Run("empty env in staging falls back to default", func(t *testing.T) {
		got, err := computeTrustedProxyCIDRs("", "staging")
		require.NoError(t, err)
		assert.Equal(t, []string{"172.16.0.0/12"}, got)
	})

	t.Run("explicit env value in production is honored", func(t *testing.T) {
		got, err := computeTrustedProxyCIDRs("10.0.0.0/8", "production")
		require.NoError(t, err)
		assert.Equal(t, []string{"10.0.0.0/8"}, got)
	})

	t.Run("explicit env value in development overrides default", func(t *testing.T) {
		got, err := computeTrustedProxyCIDRs("10.0.0.0/8", "development")
		require.NoError(t, err)
		assert.Equal(t, []string{"10.0.0.0/8"}, got)
	})

	t.Run("malformed env value returns error in production", func(t *testing.T) {
		_, err := computeTrustedProxyCIDRs("not-a-cidr", "production")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not-a-cidr")
	})

	t.Run("malformed env value returns error in development", func(t *testing.T) {
		_, err := computeTrustedProxyCIDRs("not-a-cidr", "development")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not-a-cidr")
	})
}

// validProductionConfig returns a Config with all production guards satisfied.
func validProductionConfig() *Config { // #nosec G101 -- test fixture with fake credentials
	testVal := "real_pw"                                                          //nolint:gosec // test fixture, not a real credential
	dbURL := "postgres://concord:" + testVal + "@db:5432/concord?sslmode=disable" //nolint:gosec // test fixture
	redisURL := "redis://:" + testVal + "@redis:6379"                             //nolint:gosec // test fixture
	return &Config{                                                               //nolint:gosec // G101: test fixture with fake values, not real credentials
		Environment:       "production",
		JWTSecret:         "real-production-jwt-secret", //nolint:gosec // G101 false positive: test fixture, not a real secret
		DatabaseURL:       dbURL,
		RedisURL:          redisURL,
		MFAEncryptionKey:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		MinIOSecretKey:    "real-minio-secret",
		MinIOAccessKey:    "real-minio-access",
		MinIOEndpoint:     "minio:9000",
		MinIOBucket:       "concord-media",
		SMTPHost:          "smtp.example.com",
		TrustedProxyCIDRs: []string{"10.0.0.0/8"},
		// Satisfy the #725 MEDIA_PLANE_URL production guards: must be set,
		// must not be the dev default, must not be the root zone.
		MediaPlaneURL: "https://media.concordvoice.chat",
	}
}

func TestValidateDevelopmentSkipsAllChecks(t *testing.T) {
	cfg := &Config{Environment: "development"}
	assert.NoError(t, cfg.validate())
}

func TestValidateProductionPassesWithValidConfig(t *testing.T) {
	cfg := validProductionConfig()
	assert.NoError(t, cfg.validate())
}

func TestParseTrustedProxyCIDRs(t *testing.T) {
	t.Run("empty input returns empty slice", func(t *testing.T) {
		cidrs, err := parseTrustedProxyCIDRs("")
		require.NoError(t, err)
		assert.Empty(t, cidrs)
	})

	t.Run("single IPv4 CIDR", func(t *testing.T) {
		cidrs, err := parseTrustedProxyCIDRs("172.19.0.0/16")
		require.NoError(t, err)
		assert.Equal(t, []string{"172.19.0.0/16"}, cidrs)
	})

	t.Run("multiple IPv4 CIDRs", func(t *testing.T) {
		cidrs, err := parseTrustedProxyCIDRs("172.19.0.0/16,10.0.0.0/8")
		require.NoError(t, err)
		assert.Equal(t, []string{"172.19.0.0/16", "10.0.0.0/8"}, cidrs)
	})

	t.Run("mixed IPv4 and IPv6 CIDRs", func(t *testing.T) {
		cidrs, err := parseTrustedProxyCIDRs("172.19.0.0/16,::1/128")
		require.NoError(t, err)
		assert.Equal(t, []string{"172.19.0.0/16", "::1/128"}, cidrs)
	})

	t.Run("trims whitespace around entries", func(t *testing.T) {
		cidrs, err := parseTrustedProxyCIDRs(" 172.19.0.0/16 , 10.0.0.0/8 ")
		require.NoError(t, err)
		assert.Equal(t, []string{"172.19.0.0/16", "10.0.0.0/8"}, cidrs)
	})

	t.Run("filters empty entries from double commas", func(t *testing.T) {
		cidrs, err := parseTrustedProxyCIDRs("172.19.0.0/16,,10.0.0.0/8")
		require.NoError(t, err)
		assert.Equal(t, []string{"172.19.0.0/16", "10.0.0.0/8"}, cidrs)
	})

	t.Run("rejects malformed CIDR", func(t *testing.T) {
		_, err := parseTrustedProxyCIDRs("not-a-cidr")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not-a-cidr")
		assert.Contains(t, err.Error(), "TRUSTED_PROXY_CIDRS")
	})

	t.Run("rejects missing mask", func(t *testing.T) {
		_, err := parseTrustedProxyCIDRs("172.19.0.0")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "172.19.0.0")
	})

	t.Run("rejects one bad entry in a list", func(t *testing.T) {
		_, err := parseTrustedProxyCIDRs("172.19.0.0/16,not-a-cidr,10.0.0.0/8")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not-a-cidr")
	})
}

func TestValidateProductionRejectsDevDefaults(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantMsg string
	}{
		{"dev JWT secret", func(c *Config) { c.JWTSecret = devJWTSecret }, "JWT_SECRET"},
		{"dev DATABASE_URL", func(c *Config) { c.DatabaseURL = defaultDevDatabaseURL() }, "DATABASE_URL"},
		{"dev REDIS_URL", func(c *Config) { c.RedisURL = defaultDevRedisURL() }, "REDIS_URL"},
		{"dev MFA encryption key", func(c *Config) { c.MFAEncryptionKey = devMFAEncKey }, "MFA_ENCRYPTION_KEY"},
		{"dev MinIO secret key", func(c *Config) { c.MinIOSecretKey = devMinIOSecretKey }, "MINIO_SECRET_KEY"},
		{"empty SMTP host", func(c *Config) { c.SMTPHost = "" }, "SMTP_HOST"},
		{"empty MinIO access key", func(c *Config) { c.MinIOAccessKey = "" }, "MINIO_ACCESS_KEY"},
		{"empty MinIO endpoint", func(c *Config) { c.MinIOEndpoint = "" }, "MINIO_ENDPOINT"},
		{"empty MinIO bucket", func(c *Config) { c.MinIOBucket = "" }, "MINIO_BUCKET"},
		{"empty TRUSTED_PROXY_CIDRS", func(c *Config) { c.TrustedProxyCIDRs = nil }, "TRUSTED_PROXY_CIDRS"},
		// #725 cross-cutting: MEDIA_PLANE_URL guards. Three classes of failure
		// should all be rejected by the production validate() pass.
		//
		// Class 1 — unset / dev-default:
		{"empty MEDIA_PLANE_URL", func(c *Config) { c.MediaPlaneURL = "" }, "MEDIA_PLANE_URL"},
		{"dev-default MEDIA_PLANE_URL", func(c *Config) { c.MediaPlaneURL = "http://localhost:3000" }, "MEDIA_PLANE_URL"},
		// Class 2 — root-zone exact forms:
		{"MEDIA_PLANE_URL root https", func(c *Config) { c.MediaPlaneURL = "https://concordvoice.chat" }, "MEDIA_PLANE_URL"},
		{"MEDIA_PLANE_URL root http", func(c *Config) { c.MediaPlaneURL = "http://concordvoice.chat" }, "MEDIA_PLANE_URL"},
		// Class 3 — root-zone variants that exact string match would miss.
		// Per #725 review: Copilot + Seer + 3 subagents independently flagged
		// that exact-match guards are bypassed by any of these variants.
		{"MEDIA_PLANE_URL root with trailing slash", func(c *Config) { c.MediaPlaneURL = "https://concordvoice.chat/" }, "MEDIA_PLANE_URL"},
		{"MEDIA_PLANE_URL root with explicit port", func(c *Config) { c.MediaPlaneURL = "https://concordvoice.chat:443" }, "MEDIA_PLANE_URL"},
		{"MEDIA_PLANE_URL root with path", func(c *Config) { c.MediaPlaneURL = "https://concordvoice.chat/api" }, "MEDIA_PLANE_URL"},
		{"MEDIA_PLANE_URL root uppercase", func(c *Config) { c.MediaPlaneURL = "HTTPS://CONCORDVOICE.CHAT" }, "MEDIA_PLANE_URL"},
		{"MEDIA_PLANE_URL root wss scheme", func(c *Config) { c.MediaPlaneURL = "wss://concordvoice.chat" }, "MEDIA_PLANE_URL"},
		{"MEDIA_PLANE_URL root with www", func(c *Config) { c.MediaPlaneURL = "https://www.concordvoice.chat" }, "MEDIA_PLANE_URL"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validProductionConfig()
			tt.mutate(cfg)
			err := cfg.validate()
			require.Error(t, err, "expected error for %s", tt.name)
			assert.True(t, strings.Contains(err.Error(), tt.wantMsg),
				"expected error to contain %q, got: %v", tt.wantMsg, err)
		})
	}
}

// TestValidateProductionAcceptsMediaSubdomain confirms valid MEDIA_PLANE_URL
// forms are NOT rejected — regression guard against a future tightening that
// would reject legitimate media-plane hosts along with the root zone.
func TestValidateProductionAcceptsMediaSubdomain(t *testing.T) {
	accepts := []string{
		"https://media.concordvoice.chat",     // canonical production value
		"https://media.concordvoice.chat/",    // trailing slash OK on subdomain
		"https://media.concordvoice.chat:443", // explicit port OK on subdomain
		"https://other.example.com",           // unrelated host passes (operator override)
		"http://media.example.com",            // http OK for non-production-hostname variants
	}
	for _, v := range accepts {
		t.Run(v, func(t *testing.T) {
			cfg := validProductionConfig()
			cfg.MediaPlaneURL = v
			assert.NoError(t, cfg.validate(),
				"%s must be accepted — only the root concordvoice.chat zone is rejected", v)
		})
	}
}

// TestMediaPlaneURLIsRoot covers the helper function directly so future
// refactors can't regress the host-matching logic without a test failure.
func TestMediaPlaneURLIsRoot(t *testing.T) {
	cases := []struct {
		input string
		root  bool
	}{
		// Root zone — should all be flagged:
		{"https://concordvoice.chat", true},
		{"http://concordvoice.chat", true},
		{"https://concordvoice.chat/", true},
		{"https://concordvoice.chat:443", true},
		{"https://concordvoice.chat/api", true},
		{"HTTPS://CONCORDVOICE.CHAT", true},
		{"wss://concordvoice.chat", true},
		{"https://www.concordvoice.chat", true},
		{"https://WWW.Concordvoice.Chat", true},
		// Non-root — should all pass:
		{"https://media.concordvoice.chat", false},
		{"https://api.concordvoice.chat", false},
		{"https://turn.concordvoice.chat", false},
		{"https://example.com", false},
		{"http://localhost:3000", false}, // dev default, rejected by a different guard
		{"", false},                      // empty, rejected by a different guard
		{"not-a-url", false},             // malformed — different guard territory
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			assert.Equal(t, c.root, mediaPlaneURLIsRoot(c.input))
		})
	}
}
