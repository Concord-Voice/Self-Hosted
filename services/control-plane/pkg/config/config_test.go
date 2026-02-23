package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetEnv(t *testing.T) {
	t.Run("returns env value when set", func(t *testing.T) {
		os.Setenv("TEST_CONFIG_VAR", "custom_value")
		defer os.Unsetenv("TEST_CONFIG_VAR")

		assert.Equal(t, "custom_value", getEnv("TEST_CONFIG_VAR", "default"))
	})

	t.Run("returns default when not set", func(t *testing.T) {
		assert.Equal(t, "default_value", getEnv("NONEXISTENT_CONFIG_VAR", "default_value"))
	})

	t.Run("empty env value uses default", func(t *testing.T) {
		os.Setenv("TEST_EMPTY_VAR", "")
		defer os.Unsetenv("TEST_EMPTY_VAR")

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
	// Unset all env vars to test defaults
	for _, key := range []string{"ENVIRONMENT", "PORT", "DATABASE_URL", "REDIS_URL", "JWT_SECRET", "NATS_URL", "ALLOWED_ORIGINS"} {
		original := os.Getenv(key)
		os.Unsetenv(key)
		defer func(k, v string) {
			if v != "" {
				os.Setenv(k, v)
			}
		}(key, original)
	}

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
		os.Setenv("PORT", "9090")
		os.Setenv("ENVIRONMENT", "staging")
		defer os.Unsetenv("PORT")
		defer os.Unsetenv("ENVIRONMENT")

		cfg, err := Load()
		require.NoError(t, err)

		assert.Equal(t, "9090", cfg.Port)
		assert.Equal(t, "staging", cfg.Environment)
	})
}
