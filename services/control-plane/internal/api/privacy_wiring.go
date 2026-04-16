//nolint:revive // "api" is the established package name shared with router.go; renaming is out of scope for this PR.
package api

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/privacy"
	concordsentry "github.com/markdrogersjr/Concord/services/control-plane/internal/sentry"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// privacyBootstrapFailedMsg is the structured-log message used when any of
// the three Sentry config validators rejects the runtime configuration. The
// per-call "error" field carries the specific failure detail.
const privacyBootstrapFailedMsg = "privacy handler bootstrap failed"

// validateSentryProdConfig fail-closes if a production deployment enables
// Sentry (SENTRY_DSN_CONTROL set) but is missing any of the three required
// Sentry admin credentials. When Sentry is intentionally disabled in
// production (DSN unset), the guard passes — the caller (buildPrivacyHandler)
// then chooses between a no-op stub (when SENTRY_ADMIN_TOKEN is also unset)
// and a real Sentry admin client retained for post-disable data erasure
// (when SENTRY_ADMIN_TOKEN is still set). The validator does not enforce
// the noop wiring; it only enforces "if Sentry is enabled, all credentials
// must be present." Extracted so the fail-closed behavior is unit-testable
// without spawning a subprocess to observe log.Fatal.
func validateSentryProdConfig(env, dsn, token, orgSlug, projSlug string) error {
	if env != "production" {
		return nil
	}
	if dsn == "" {
		return nil // Sentry disabled in production — no admin token needed
	}
	if token == "" {
		return errors.New("SENTRY_ADMIN_TOKEN is required when SENTRY_DSN_CONTROL is set in production")
	}
	if orgSlug == "" || projSlug == "" {
		return errors.New("SENTRY_ORG_SLUG and SENTRY_PROJECT_SLUG are required when SENTRY_DSN_CONTROL is set in production")
	}
	return nil
}

// validateSentryDSNShape rejects DSN values that are obviously not Sentry
// DSNs — whitespace-only strings, sentinel placeholders left over from
// secrets-template substitution, or malformed URLs. Accepts the raw env
// value (not trimmed) so a whitespace-only DSN is itself a failure signal:
// the operator set the variable deliberately but to a non-value. A truly
// empty string is the documented "Sentry off" configuration and passes
// silently; the caller decides whether to require the DSN via
// validateSentryEnabledOptIn. A bogus DSN that gets past this guard would
// cause the SDK to silently drop events at runtime — exactly the kind of
// misconfiguration the caller cannot diagnose later.
//
// The URL validation requires the full `https://<key>@<host>/<project>`
// shape that Sentry DSNs use: HTTPS scheme, a non-empty host, a userinfo
// component carrying the public key, and a non-trivial project path.
// Accepting just scheme+host would let a host-only URL (e.g.
// `https://sentry.io`) pass as a valid DSN even though the SDK would then
// reject it at first send.
func validateSentryDSNShape(rawDSN string) error {
	trimmed := strings.TrimSpace(rawDSN)
	if rawDSN != "" && trimmed == "" {
		return fmt.Errorf("SENTRY_DSN_CONTROL is set but contains only whitespace")
	}
	if trimmed == "" {
		return nil
	}
	u, err := url.Parse(trimmed)
	if err != nil ||
		u.Scheme != "https" ||
		u.Host == "" ||
		u.User == nil ||
		u.User.Username() == "" ||
		u.Path == "" ||
		u.Path == "/" {
		return fmt.Errorf("SENTRY_DSN_CONTROL is set but malformed: expected https://<key>@<host>/<project>")
	}
	return nil
}

// validateSentryEnabledOptIn fail-closes when an operator explicitly sets
// SENTRY_ENABLED=true in production but leaves SENTRY_DSN_CONTROL unset.
// Without this check, a typo in the DSN env-var name (e.g. SENTRY_DSN_CONTRL)
// would silently disable Sentry and the new info log would mistakenly
// reassure the operator that Sentry is intentionally off. SENTRY_ENABLED is
// the same explicit signal that internal/sentry.Init already consults
// (see init.go:33) — we mirror its behavior here so the wiring layer
// fails the same way, not silently differently.
func validateSentryEnabledOptIn(env, dsn, sentryEnabled string) error {
	if env != "production" {
		return nil
	}
	if !isSentryEnabledTrue(sentryEnabled) {
		return nil
	}
	if dsn == "" {
		return errors.New("SENTRY_ENABLED=true in production but SENTRY_DSN_CONTROL is unset; refusing to start with silent-inert observability")
	}
	return nil
}

// isSentryEnabledTrue returns true for the canonical truthy values accepted
// by internal/sentry.Init's switch statement; kept in lockstep so both
// layers agree on whether the operator asked for Sentry to be on.
func isSentryEnabledTrue(s string) bool {
	switch s {
	case "true", "TRUE", "True":
		return true
	default:
		return false
	}
}

// noopSentryAdmin is the stub used when SENTRY_ADMIN_TOKEN is unset.
// Reachable in two cases: (1) any non-production env without an admin token,
// so local dev works without requiring every developer to hold one;
// (2) production with SENTRY_DSN_CONTROL unset AND SENTRY_ADMIN_TOKEN unset,
// the "Sentry intentionally disabled" configuration. Production deployments
// with Sentry enabled (DSN set) are guarded in buildPrivacyHandler and
// require all credentials.
//
// The stub holds a logger so each erasure call emits a per-request audit
// signal — privacy operators handling GDPR Article 17 requests need to be
// able to answer "did this user's erasure actually delete data?" by reading
// logs. Without this signal, the noop is indistinguishable from a real
// 204-No-Content success.
type noopSentryAdmin struct {
	log *logger.Logger
}

// DeleteEventsForUser is a no-op that intentionally returns nil so the
// erasure endpoint still completes with 204 — the user-visible contract is
// preserved. The clientId is NEVER logged (per [internal]rules/observability.md).
// Only neutral operation metadata and a "noop" reason are emitted because
// this shared deleter is invoked by both EraseTelemetry and EraseAccount
// and does not know which handler endpoint invoked it — the "operation"
// field names the interface method, not the HTTP route. A nil logger is
// tolerated for tests that don't exercise this code path.
func (n noopSentryAdmin) DeleteEventsForUser(_ context.Context, _ string) error {
	if n.log != nil {
		n.log.Warn("privacy erasure short-circuited — Sentry admin not configured",
			"operation", "delete_events_for_user",
			"reason", "noop_sentry_admin",
		)
	}
	return nil
}

// buildPrivacyHandler constructs the privacy handler wired to either the
// real Sentry admin client (when SENTRY_ADMIN_TOKEN is set) or a no-op
// stub (when SENTRY_ADMIN_TOKEN is unset). The token, not the DSN,
// determines which path is taken — operators may keep the admin token
// after unsetting the DSN to retain post-disable data-erasure capability.
//
// Panics via log.Fatal under three conditions, in order:
//  1. SENTRY_DSN_CONTROL is set but malformed (validateSentryDSNShape).
//  2. SENTRY_ENABLED=true in production but SENTRY_DSN_CONTROL is unset
//     (validateSentryEnabledOptIn — guards against env-var-name typos
//     that would silently disable Sentry).
//  3. In production, Sentry is enabled (DSN set) but the admin token or
//     slugs are missing (validateSentryProdConfig).
//
// All three checks run before any network or filesystem side effects, so
// a misconfiguration fails the process during the constructor rather than
// at first request.
func buildPrivacyHandler(cfg *config.Config, log *logger.Logger) *privacy.Handler {
	rawDSN := os.Getenv("SENTRY_DSN_CONTROL")
	token := os.Getenv("SENTRY_ADMIN_TOKEN")
	org := os.Getenv("SENTRY_ORG_SLUG")
	proj := os.Getenv("SENTRY_PROJECT_SLUG")
	apiBase := os.Getenv("SENTRY_API_BASE")
	sentryEnabled := os.Getenv("SENTRY_ENABLED")
	if apiBase == "" {
		apiBase = "https://sentry.io"
	}

	// Validator takes the raw env value so it can distinguish empty
	// (Sentry-off, supported) from whitespace-only (misconfig, rejected).
	if err := validateSentryDSNShape(rawDSN); err != nil {
		log.Fatal(privacyBootstrapFailedMsg, "error", err)
	}
	dsn := strings.TrimSpace(rawDSN)
	if err := validateSentryEnabledOptIn(cfg.Environment, dsn, sentryEnabled); err != nil {
		log.Fatal(privacyBootstrapFailedMsg, "error", err)
	}
	if err := validateSentryProdConfig(cfg.Environment, dsn, token, org, proj); err != nil {
		log.Fatal(privacyBootstrapFailedMsg, "error", err)
	}

	if cfg.Environment == "production" && dsn == "" && token == "" {
		log.Info("Sentry disabled in production — SENTRY_DSN_CONTROL and SENTRY_ADMIN_TOKEN both unset; privacy erasure uses no-op stub")
	}
	if dsn == "" && token != "" {
		log.Warn("SENTRY_ADMIN_TOKEN is set but SENTRY_DSN_CONTROL is unset — privacy erasure will target Sentry even though the app emits nothing to it")
	}

	var admin privacy.EventsDeleter
	if token == "" {
		if cfg.Environment != "production" {
			log.Warn("SENTRY_ADMIN_TOKEN unset — privacy erasure uses no-op stub (non-production)")
		}
		admin = noopSentryAdmin{log: log}
	} else {
		admin = concordsentry.NewAdminClient(apiBase, org, proj, token)
	}
	return privacy.NewHandler(admin, log)
}
