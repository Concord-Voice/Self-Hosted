// Package database provides PostgreSQL database connection and migration utilities.
package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/lib/pq" // also registers the PostgreSQL driver

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/migrationrunner"
)

// migrationsSourceURL is the control plane's migrations directory, relative to
// the working directory the binary runs in.
const migrationsSourceURL = "file://migrations"

var errSchemaNotCurrent = errors.New("schema not current for this binary (verify mode)")

// poolIdleInTxParam bounds how long any pooled session may sit idle inside an
// open transaction (VULN-079), so a stalled request cannot pin a connection
// and the locks it holds. It is a run-time parameter sent at connection
// startup, not a statement timeout: migrations run on this pool and never sit
// idle mid-transaction, so it cannot cut one short. If a connection pooler is
// ever placed in front of PostgreSQL, set it with ALTER ROLE ... SET instead.
const (
	poolIdleInTxParam = "idle_in_transaction_session_timeout"
	poolIdleInTxValue = "60000" // milliseconds
)

// errInvalidDatabaseURL deliberately carries no cause: a parse error can quote
// the URL, and the URL can carry the database password.
var errInvalidDatabaseURL = errors.New("invalid database URL")

// withPoolSessionParams adds poolIdleInTxParam to a URL-form or key=value DSN.
// A value the caller already set, as a parameter or inside options, is kept.
func withPoolSessionParams(databaseURL string) (string, error) {
	cfg, err := pq.NewConfig(databaseURL)
	if err != nil {
		return "", errInvalidDatabaseURL
	}
	if _, set := cfg.Runtime[poolIdleInTxParam]; set || strings.Contains(cfg.Options, poolIdleInTxParam) {
		return databaseURL, nil
	}
	if strings.HasPrefix(databaseURL, "postgres://") || strings.HasPrefix(databaseURL, "postgresql://") {
		u, err := url.Parse(databaseURL)
		if err != nil {
			return "", errInvalidDatabaseURL
		}
		q := u.Query()
		q.Set(poolIdleInTxParam, poolIdleInTxValue)
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	// Appended to as written, never trimmed: a value may end in an escaped
	// space, and trimming it would leave the backslash to escape our separator.
	return databaseURL + " " + poolIdleInTxParam + "=" + poolIdleInTxValue, nil
}

// New creates a new PostgreSQL database connection
func New(databaseURL string) (*sql.DB, error) {
	dsn, err := withPoolSessionParams(databaseURL)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Test the connection
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	// Set connection pool settings
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	return db, nil
}

// RunMigrations runs the control plane's migrations through the shared
// migrationrunner, which refuses a dirty recorded version (#3587 PR-0).
func RunMigrations(db *sql.DB) error {
	return migrationrunner.Run(db, migrationsSourceURL)
}

// MigrationStatus reads the control plane's schema status against its
// migrations without writing anything or taking the migration lock.
func MigrationStatus(ctx context.Context, db *sql.DB) (migrationrunner.Status, error) {
	return migrationrunner.ReadStatus(ctx, db, migrationsSourceURL)
}

// VerifyMigrations fails unless the control plane's schema is current for its
// migrations, naming the status line in the refusal. It never applies a
// migration, so an uninitialised, pending, ahead or dirty schema stays as it is.
func VerifyMigrations(ctx context.Context, db *sql.DB) error {
	st, err := migrationrunner.ReadStatus(ctx, db, migrationsSourceURL)
	if err != nil {
		return fmt.Errorf("read migration status: %w", err)
	}
	if !st.Current() {
		return fmt.Errorf("%w: %s", errSchemaNotCurrent, st.Line())
	}
	return nil
}
