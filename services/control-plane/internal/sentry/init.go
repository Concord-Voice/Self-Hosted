package sentry

import (
	"fmt"
	"os"

	sentrygo "github.com/getsentry/sentry-go"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// Version is set at build time via ldflags. Defaults to "dev" for local builds.
var Version = "dev"

// Init configures Sentry for the control-plane. Must be called early in main().
// Callers MUST defer sentry.Flush(2 * time.Second) after calling this.
func Init(cfg *config.Config, log *logger.Logger) error {
	dsn := os.Getenv("SENTRY_DSN_CONTROL")
	enabledStr := os.Getenv("SENTRY_ENABLED")

	var enabled bool
	switch enabledStr {
	case "":
		enabled = dsn != "" && cfg.Environment == "production"
	case "true", "TRUE", "True":
		enabled = true
	default:
		enabled = false
	}

	// Production safety guard — fatal if enabled without DSN.
	if cfg.Environment == "production" && enabled && dsn == "" {
		return fmt.Errorf("SENTRY_ENABLED=true in production but SENTRY_DSN_CONTROL is unset; refusing to start with silent-inert observability")
	}

	// Disable if DSN is empty — SDK can't send events without one.
	if dsn == "" {
		enabled = false
	}

	// An empty DSN disables the SDK; when not enabled we pass "" regardless of
	// whether a DSN was provided.
	activeDSN := dsn
	if !enabled {
		activeDSN = ""
	}

	err := sentrygo.Init(sentrygo.ClientOptions{
		Dsn:              activeDSN,
		Environment:      cfg.Environment,
		Release:          fmt.Sprintf("control-plane@%s", Version),
		SampleRate:       1.0,
		TracesSampleRate: 0.0,
		BeforeSend:       scrubEvent,
		BeforeSendTransaction: func(_ *sentrygo.Event, _ *sentrygo.EventHint) *sentrygo.Event {
			// Defense in depth: drop all transaction events even if
			// TracesSampleRate is accidentally bumped.
			return nil
		},
	})
	if err != nil {
		return fmt.Errorf("sentry init: %w", err)
	}

	if enabled {
		log.Info("Sentry initialized", "environment", cfg.Environment, "release", fmt.Sprintf("control-plane@%s", Version))
	} else {
		log.Info("Sentry disabled", "reason", reasonDisabled(dsn, cfg.Environment, enabledStr))
	}

	return nil
}

func reasonDisabled(dsn, env, enabledStr string) string {
	if dsn == "" {
		return "SENTRY_DSN_CONTROL not set"
	}
	if env != "production" {
		return fmt.Sprintf("environment=%s (not production)", env)
	}
	if enabledStr != "" && enabledStr != "true" {
		return "SENTRY_ENABLED=" + enabledStr
	}
	return "unknown"
}
