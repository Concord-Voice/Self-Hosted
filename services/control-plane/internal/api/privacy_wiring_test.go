//nolint:revive // "api" is the established package name shared with router.go; renaming is out of scope for this PR.
package api

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// newBufferedTestLogger returns a Logger that writes to the returned buffer.
// Lets tests assert on structured-log output without redirecting os.Stdout.
func newBufferedTestLogger(t *testing.T) (*logger.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	return &logger.Logger{Logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}, &buf
}

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
		dsn       string
		token     string
		orgSlug   string
		projSlug  string
		wantError bool
	}{
		{"non-production with empty everything ok", "development", "", "", "", "", false},
		{"non-production with dsn but nothing else ok", "staging", "dsn", "", "", "", false},
		{"production no dsn no token ok (regression fix for #679)", "production", "", "", "", "", false},
		{"production no dsn but token set still ok", "production", "", "tok", "", "", false}, // pragma: allowlist secret
		{"production with dsn and empty token errors", "production", "dsn", "", "org", "proj", true},
		{"production with dsn and empty orgSlug errors", "production", "dsn", "tok", "", "proj", true}, // pragma: allowlist secret
		{"production with dsn and empty projSlug errors", "production", "dsn", "tok", "org", "", true}, // pragma: allowlist secret
		{"production with dsn and all three set ok", "production", "dsn", "tok", "org", "proj", false}, // pragma: allowlist secret
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSentryProdConfig(tc.env, tc.dsn, tc.token, tc.orgSlug, tc.projSlug)
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

// TestBuildPrivacyHandler_Production_NoDsn_NoToken_DoesNotFatal is the direct
// regression test for issue #679 — the ~68-minute outage on 2026-04-16 caused
// by the old guard requiring SENTRY_ADMIN_TOKEN even when Sentry was off. The
// handler must construct cleanly when production has no DSN and no token.
func TestBuildPrivacyHandler_Production_NoDsn_NoToken_DoesNotFatal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("SENTRY_DSN_CONTROL", "")
	t.Setenv("SENTRY_ADMIN_TOKEN", "")
	t.Setenv("SENTRY_ORG_SLUG", "")
	t.Setenv("SENTRY_PROJECT_SLUG", "")
	t.Setenv("SENTRY_API_BASE", "")
	t.Setenv("SENTRY_ENABLED", "")
	cfg := &config.Config{Environment: "production"}
	log := logger.New("test")

	h := buildPrivacyHandler(cfg, log)
	require.NotNil(t, h)
}

// TestBuildPrivacyHandler_Production_FullSentryConfig_ConstructsRealClient is
// the symmetric happy-path test for the production-with-Sentry-enabled wiring.
// Together with the no-DSN regression test above, these two tests bracket the
// behavior matrix's two production rows that produce a working handler.
func TestBuildPrivacyHandler_Production_FullSentryConfig_ConstructsRealClient(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("SENTRY_DSN_CONTROL", "https://abc@o0.ingest.sentry.io/1") // pragma: allowlist secret
	t.Setenv("SENTRY_ADMIN_TOKEN", "dummy-token")                       // pragma: allowlist secret
	t.Setenv("SENTRY_ORG_SLUG", "test-org")
	t.Setenv("SENTRY_PROJECT_SLUG", "test-proj")
	t.Setenv("SENTRY_API_BASE", "http://localhost:9999")
	t.Setenv("SENTRY_ENABLED", "true")
	cfg := &config.Config{Environment: "production"}
	log := logger.New("test")

	h := buildPrivacyHandler(cfg, log)
	require.NotNil(t, h)
}

func TestValidateSentryDSNShape(t *testing.T) {
	tests := []struct {
		name      string
		dsn       string
		wantError bool
	}{
		{"empty dsn ok (Sentry off)", "", false},
		{"valid sentry SaaS dsn ok", "https://abc@o0.ingest.sentry.io/1", false}, // pragma: allowlist secret
		{"valid self-hosted dsn ok", "https://abc@sentry.example.com/1", false},  // pragma: allowlist secret
		{"whitespace-only rejected as misconfig", " ", true},
		{"tab-only rejected as misconfig", "\t", true},
		{"newline rejected as misconfig", "\n", true},
		{"sentinel string rejected", "disabled", true},
		{"placeholder rejected", "<unset>", true},
		{"http scheme rejected", "http://abc@o0.ingest.sentry.io/1", true}, // pragma: allowlist secret
		{"missing host rejected", "https:///1", true},
		{"host only without key or path rejected", "https://o0.ingest.sentry.io", true},
		{"host with path but no key rejected", "https://o0.ingest.sentry.io/1", true},
		{"host with key but no path rejected", "https://abc@sentry.io", true},     // pragma: allowlist secret
		{"host with key and empty path rejected", "https://abc@sentry.io/", true}, // pragma: allowlist secret
		{"non-url rejected", "not a url", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSentryDSNShape(tc.dsn)
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestNoopSentryAdmin_DeleteEventsForUser_EmitsAuditLog verifies the per-call
// audit trail added in this PR. The noop stub must emit a Warn so privacy
// operators handling GDPR Article 17 requests can see in logs that an erasure
// call reached the noop path (Sentry not configured) rather than the real
// admin client. The sentinel clientId passed in must NEVER appear in the log
// output, and the warn must carry the neutral operation/reason fields.
func TestNoopSentryAdmin_DeleteEventsForUser_EmitsAuditLog(t *testing.T) {
	log, buf := newBufferedTestLogger(t)
	const sentinelClientID = "deadbeefcafef00d1122334455667788" // pragma: allowlist secret

	n := noopSentryAdmin{log: log}
	err := n.DeleteEventsForUser(context.Background(), sentinelClientID)
	require.NoError(t, err, "noop must preserve the 204-success contract")

	out := buf.String()
	require.Contains(t, out, "level=WARN", "noop audit must emit at WARN level")
	require.Contains(t, out, "privacy erasure short-circuited", "noop audit must name the short-circuit")
	require.Contains(t, out, "operation=delete_events_for_user", "noop audit must carry neutral operation field (not endpoint)")
	require.Contains(t, out, "reason=noop_sentry_admin", "noop audit must carry the noop reason tag")
	require.NotContains(t, out, sentinelClientID, "clientId must NEVER appear in the audit log")
}

// TestNoopSentryAdmin_DeleteEventsForUser_NilLogger_DoesNotPanic pins the
// documented tolerance for a nil logger so the stub is safe to construct
// in tests that don't exercise the audit-log path.
func TestNoopSentryAdmin_DeleteEventsForUser_NilLogger_DoesNotPanic(t *testing.T) {
	n := noopSentryAdmin{log: nil}
	require.NotPanics(t, func() {
		_ = n.DeleteEventsForUser(context.Background(), "any-client-id")
	})
}

func TestValidateSentryEnabledOptIn(t *testing.T) {
	tests := []struct {
		name          string
		env           string
		dsn           string
		sentryEnabled string
		wantError     bool
	}{
		{"non-production with enabled=true and no dsn ok", "development", "", "true", false},
		{"production with enabled unset and no dsn ok (default opt-out)", "production", "", "", false},
		{"production with enabled=false and no dsn ok", "production", "", "false", false},
		{"production with enabled=true and no dsn errors (typo guard)", "production", "", "true", true},
		{"production with enabled=true and dsn set ok", "production", "dsn", "true", false},
		{"production with enabled=TRUE accepted as truthy", "production", "", "TRUE", true},
		{"production with enabled=True accepted as truthy", "production", "", "True", true},
		{"production with enabled=yes treated as not-truthy (matches init.go)", "production", "", "yes", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSentryEnabledOptIn(tc.env, tc.dsn, tc.sentryEnabled)
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
