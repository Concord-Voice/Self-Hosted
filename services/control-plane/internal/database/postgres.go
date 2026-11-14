// Package database provides PostgreSQL database connection and migration utilities.
package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/lib/pq" // Register PostgreSQL driver

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/migrationrunner"
)

// migrationsSourceURL is the control plane's migrations directory, relative to
// the working directory the binary runs in.
const migrationsSourceURL = "file://migrations"

var errSchemaNotCurrent = errors.New("schema not current for this binary (verify mode)")

// New creates a new PostgreSQL database connection
func New(databaseURL string) (*sql.DB, error) {
	db, err := sql.Open("postgres", databaseURL)
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
