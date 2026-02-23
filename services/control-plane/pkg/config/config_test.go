package config

import (
	"os"
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
