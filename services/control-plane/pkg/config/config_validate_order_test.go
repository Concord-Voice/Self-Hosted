package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestValidateChecksRunInOrder pins the order of validate's leading checks:
// a config that fails every one of them reports the first, and clearing each
// failure in turn exposes the next. Each step asserts the exact message.
func TestValidateChecksRunInOrder(t *testing.T) {
	t.Setenv("CONCORD_ENV", "test")
	t.Setenv("REDIS_URL", "")
	cfg := &Config{
		Environment:                      "staging",
		TURNSecretFile:                   "relative/key",
		RedisCredentialMode:              "automatic",
		ClientMinVersion:                 "1.0",
		ClientMinVersionIOS:              "1.0",
		ClientMinVersionAndroid:          "1.0",
		ControlPlaneReplicaCountExplicit: true,
		ControlPlaneReplicaCount:         2,
	}
	steps := []struct {
		want  string
		clear func(*Config)
	}{
		{"TURN_SECRET_FILE must be absolute and cannot be combined with TURN_SECRET",
			func(c *Config) { c.TURNSecretFile = filepath.Join(t.TempDir(), "key") }},
		{"TURN_SECRET_FILE requires TURN_SERVER_HOST", func(c *Config) { c.TURNSecretFile = "" }},
		{"REDIS_CREDENTIAL_MODE must be legacy or managed", func(c *Config) { c.RedisCredentialMode = "managed" }},
		{"REDIS_ENDPOINT must be a bare host:port", func(c *Config) { c.RedisCredentialMode = "legacy" }},
		{"CLIENT_MIN_VERSION must be a stable X.Y.Z version: ", func(c *Config) { c.ClientMinVersion = "" }},
		{"CLIENT_MIN_VERSION_IOS must be a stable X.Y.Z version: ", func(c *Config) { c.ClientMinVersionIOS = "" }},
		{"CLIENT_MIN_VERSION_ANDROID must be a stable X.Y.Z version: ", func(c *Config) { c.ClientMinVersionAndroid = "" }},
		{"CONTROL_PLANE_REPLICA_COUNT=2 is not supported", func(c *Config) { c.ControlPlaneReplicaCountExplicit = false }},
		{`CONCORD_ENV=test is forbidden unless ENVIRONMENT is development or test (got "staging")`,
			func(c *Config) { c.Environment = "development" }},
	}
	for _, s := range steps {
		err := cfg.validate()
		require.Error(t, err, "want %q", s.want)
		require.True(t, strings.HasPrefix(err.Error(), s.want), "got %q, want prefix %q", err.Error(), s.want)
		s.clear(cfg)
	}
	require.NoError(t, cfg.validate())
}

// A TURN_SERVER_HOST of only spaces still counts as unset.
func TestValidateTURNSecretFileWhitespaceHost(t *testing.T) {
	cfg := &Config{Environment: "development", TURNSecretFile: filepath.Join(t.TempDir(), "key"), TURNServerHost: "  "}
	require.EqualError(t, cfg.validate(), "TURN_SECRET_FILE requires TURN_SERVER_HOST")
	cfg.TURNServerHost = "turn.example.test"
	require.NoError(t, cfg.validate())
}
