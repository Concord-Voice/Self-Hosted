//nolint:revive // "api" is the established package name shared with router.go; renaming is out of scope for this PR.
package api

import (
	"context"
	"errors"
	"os"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/privacy"
	concordsentry "github.com/markdrogersjr/Concord/services/control-plane/internal/sentry"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// validateSentryProdConfig fail-closes if a production deployment is missing
// any of the three required Sentry admin credentials. Extracted so the
// fail-closed behavior is unit-testable without spawning a subprocess to
// observe log.Fatal.
func validateSentryProdConfig(env, token, orgSlug, projSlug string) error {
	if env != "production" {
		return nil
	}
	if token == "" {
		return errors.New("SENTRY_ADMIN_TOKEN is required in production")
	}
	if orgSlug == "" || projSlug == "" {
		return errors.New("SENTRY_ORG_SLUG and SENTRY_PROJECT_SLUG are required in production")
	}
	return nil
}

// noopSentryAdmin is the dev-mode stub used when SENTRY_ADMIN_TOKEN is unset.
// Production deployments are guarded in buildPrivacyHandler — this stub only
// runs in non-production environments so local dev works without requiring
// every developer to hold a Sentry admin token.
type noopSentryAdmin struct{}

func (noopSentryAdmin) DeleteEventsForUser(_ context.Context, _ string) error {
	return nil
}

// buildPrivacyHandler constructs the privacy handler wired to either the real
// Sentry admin client (when SENTRY_ADMIN_TOKEN is set) or a no-op stub in
// non-production environments. Panics via log.Fatal if the token is missing
// in production.
func buildPrivacyHandler(cfg *config.Config, log *logger.Logger) *privacy.Handler {
	token := os.Getenv("SENTRY_ADMIN_TOKEN")
	org := os.Getenv("SENTRY_ORG_SLUG")
	proj := os.Getenv("SENTRY_PROJECT_SLUG")
	apiBase := os.Getenv("SENTRY_API_BASE")
	if apiBase == "" {
		apiBase = "https://sentry.io"
	}

	if err := validateSentryProdConfig(cfg.Environment, token, org, proj); err != nil {
		log.Fatal(err.Error())
	}

	var admin privacy.EventsDeleter
	if token == "" {
		log.Warn("SENTRY_ADMIN_TOKEN unset — privacy erasure uses no-op stub (non-production)")
		admin = noopSentryAdmin{}
	} else {
		admin = concordsentry.NewAdminClient(apiBase, org, proj, token)
	}
	return privacy.NewHandler(admin)
}
