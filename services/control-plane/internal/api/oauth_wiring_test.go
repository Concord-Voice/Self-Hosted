//nolint:revive // "api" is the established package name shared with router.go.
package api

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// TestBuildOAuthHandler_GoogleSSODisabled verifies the disabled branch returns
// a non-nil handler whose registry is empty — every /sso/:provider lookup
// should 404 with unknown_provider. This is the production posture for any
// deployment that has not opted into Google SSO.
func TestBuildOAuthHandler_GoogleSSODisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log := logger.New("test")

	cfg := &config.Config{
		Environment: "test",
		GoogleSSO: config.GoogleSSOConfig{
			Enabled: false,
		},
	}

	// nil DB / nil Redis are tolerated by buildOAuthHandler — it does not
	// dereference them at construction time, only stores them on HandlerDeps.
	// Behavioral assertions about the registry's empty state happen via the
	// public Registry surface in handler.go's Initiate handler tests.
	h := buildOAuthHandler(nil, nil, cfg, nil, log)
	require.NotNil(t, h, "buildOAuthHandler must return a non-nil handler when SSO is disabled")
}

// TestBuildOAuthHandler_GoogleSSOEnabled verifies the enabled branch
// constructs without panic when ClientID + ClientSecret are present. The seed
// RedirectURI is the documented placeholder; the per-request ExchangeParams
// override carries the real loopback URI at exchange time.
//
// We don't try to drive the registry through Initiate here — that would
// require a real Redis. The assertion is structural: enabled + valid creds
// must not panic and must return a handler.
func TestBuildOAuthHandler_GoogleSSOEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log := logger.New("test")

	cfg := &config.Config{
		Environment: "test",
		GoogleSSO: config.GoogleSSOConfig{
			Enabled:      true,
			ClientID:     "test-client.apps.googleusercontent.com",
			ClientSecret: "test-secret-not-real", //nolint:gosec // test fixture
		},
	}

	require.NotPanics(t, func() {
		h := buildOAuthHandler(nil, nil, cfg, nil, log)
		assert.NotNil(t, h, "enabled SSO must yield a non-nil handler")
	})
}

// TestBuildOAuthHandler_Disabled_DoesNotPanic pins the disabled-branch
// construction. Production main.go always passes a non-nil logger; the
// rationale for keeping a constructor that doesn't dereference log on the
// disabled path is that the wiring helper should fail loudly at the enabled
// path's structural mistakes (missing client ID, etc.) rather than implicitly
// at a nil-log dereference inside an unrelated branch.
//
// The earlier name (TestBuildOAuthHandler_NilLogger_Disabled) was misleading
// — the test passes a non-nil logger.New("test"), not nil. Renamed to reflect
// what it actually asserts.
func TestBuildOAuthHandler_Disabled_DoesNotPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{
		GoogleSSO: config.GoogleSSOConfig{Enabled: false},
	}

	require.NotPanics(t, func() {
		_ = buildOAuthHandler(nil, nil, cfg, nil, logger.New("test"))
	})
}
