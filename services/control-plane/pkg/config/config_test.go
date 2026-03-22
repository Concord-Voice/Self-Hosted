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
	envVars := []string{"ENVIRONMENT", "PORT", "DATABASE_URL", "REDIS_URL", "JWT_SECRET", "NATS_URL", "ALLOWED_ORIGINS"}
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

// validProductionConfig returns a Config with all production guards satisfied.
func validProductionConfig() *Config { // #nosec G101 -- test fixture with fake credentials
	return &Config{
		Environment:      "production",
		JWTSecret:        "real-production-jwt-secret",
		DatabaseURL:      "postgres://concord:real_pw@db:5432/concord?sslmode=disable",
		RedisURL:         "redis://:real_pw@redis:6379",
		MFAEncryptionKey: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		MinIOSecretKey:   "real-minio-secret",
		MinIOAccessKey:   "real-minio-access",
		MinIOEndpoint:    "minio:9000",
		MinIOBucket:      "concord-media",
		SMTPHost:         "smtp.example.com",
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

func TestValidateProductionRejectsDevDefaults(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantMsg string
	}{
		{"dev JWT secret", func(c *Config) { c.JWTSecret = devJWTSecret }, "JWT_SECRET"},
		{"dev DATABASE_URL", func(c *Config) { c.DatabaseURL = devDatabaseURL }, "DATABASE_URL"},
		{"dev REDIS_URL", func(c *Config) { c.RedisURL = devRedisURL }, "REDIS_URL"},
		{"dev MFA encryption key", func(c *Config) { c.MFAEncryptionKey = devMFAEncKey }, "MFA_ENCRYPTION_KEY"},
		{"dev MinIO secret key", func(c *Config) { c.MinIOSecretKey = devMinIOSecretKey }, "MINIO_SECRET_KEY"},
		{"empty SMTP host", func(c *Config) { c.SMTPHost = "" }, "SMTP_HOST"},
		{"empty MinIO access key", func(c *Config) { c.MinIOAccessKey = "" }, "MINIO_ACCESS_KEY"},
		{"empty MinIO endpoint", func(c *Config) { c.MinIOEndpoint = "" }, "MINIO_ENDPOINT"},
		{"empty MinIO bucket", func(c *Config) { c.MinIOBucket = "" }, "MINIO_BUCKET"},
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
