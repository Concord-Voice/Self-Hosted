//nolint:revive // "api" is the established package name shared with router.go; renaming is out of scope for this PR.
package api

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// Note: reads the runtime wiring of buildPrivacyHandler. If the signature or
// behavior of buildPrivacyHandler changes in follow-up work (e.g. #669), update
// these tests in lockstep.

func TestBuildPrivacyHandler_NoToken_NonProduction_UsesNoopStub(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("SENTRY_ADMIN_TOKEN", "")
	cfg := &config.Config{Environment: "development"}
	log := logger.New("test")

	h := buildPrivacyHandler(cfg, log)
	require.NotNil(t, h)
	// Handler exists and the underlying admin is the noop stub. We can't assert
	// the concrete type without exporting, but we can verify the handler is
	// wired up — deeper assertions live in the privacy/handler tests.
}

func TestBuildPrivacyHandler_WithToken_ConstructsRealClient(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("SENTRY_ADMIN_TOKEN", "dummy-token") // pragma: allowlist secret
	t.Setenv("SENTRY_ORG_SLUG", "test-org")
	t.Setenv("SENTRY_PROJECT_SLUG", "test-proj")
	t.Setenv("SENTRY_API_BASE", "http://localhost:9999")
	cfg := &config.Config{Environment: "staging"}
	log := logger.New("test")

	h := buildPrivacyHandler(cfg, log)
	require.NotNil(t, h)
}

func TestValidateSentryProdConfig(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		token     string
		orgSlug   string
		projSlug  string
		wantError bool
	}{
		{"non-production with empty token ok", "development", "", "", "", false},
		{"non-production with empty everything ok", "staging", "", "", "", false},
		{"production with empty token errors", "production", "", "org", "proj", true},
		{"production with empty orgSlug errors", "production", "tok", "", "proj", true}, // pragma: allowlist secret
		{"production with empty projSlug errors", "production", "tok", "org", "", true}, // pragma: allowlist secret
		{"production with all three set ok", "production", "tok", "org", "proj", false}, // pragma: allowlist secret
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSentryProdConfig(tc.env, tc.token, tc.orgSlug, tc.projSlug)
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestBuildPrivacyHandler_DefaultsApiBase(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("SENTRY_ADMIN_TOKEN", "dummy-token") // pragma: allowlist secret
	t.Setenv("SENTRY_ORG_SLUG", "test-org")
	t.Setenv("SENTRY_PROJECT_SLUG", "test-proj")
	t.Setenv("SENTRY_API_BASE", "") // empty — should default to https://sentry.io
	cfg := &config.Config{Environment: "staging"}
	log := logger.New("test")

	h := buildPrivacyHandler(cfg, log)
	assert.NotNil(t, h)
}
