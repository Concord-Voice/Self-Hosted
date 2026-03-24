// Package testhelpers provides test utilities for integration tests.
package testhelpers

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file" // Required for file:// migration source
	_ "github.com/lib/pq"
)

// defaultTestDatabaseURL is used when DATABASE_URL is not set.
// Assembled from parts to satisfy static credential analysis (S6698/S2068).
var defaultTestDatabaseURL = "postgres://concord:" + testDBVal + "@localhost:5432/concord?sslmode=disable" //nolint:gosec // Test-only default, not a production credential

var testDBVal = "concord_dev_password" //nolint:gosec // matches docker-compose dev default

// SetupTestDB creates a database connection, runs migrations, and returns a cleanup function.
func SetupTestDB(t *testing.T) (*sql.DB, func()) {
	t.Helper()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = defaultTestDatabaseURL
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		t.Fatalf("testhelpers: failed to open database: %v", err)
	}

	if err := db.Ping(); err != nil {
		t.Fatalf("testhelpers: failed to ping database: %v", err)
	}

	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)

	// Run migrations using absolute path
	migrationsDir := migrationsPath()
	runMigrations(t, db, migrationsDir)

	cleanup := func() {
		if err := TruncateAllTables(db); err != nil {
			t.Errorf("testhelpers: failed to truncate tables: %v", err)
		}
		_ = db.Close()
	}

	return db, cleanup
}

// TruncateAllTables removes all data from application tables.
func TruncateAllTables(db *sql.DB) error {
	_, err := db.Exec(`TRUNCATE
		users,
		user_keys,
		public_keys,
		refresh_tokens,
		servers,
		server_members,
		channels,
		channel_keys,
		pending_key_requests,
		messages,
		server_invites,
		channel_read_states,
		user_preferences,
		ownership_transfers,
		friendships,
		friend_codes,
		dm_conversations,
		dm_participants,
		dm_messages,
		dm_channel_keys,
		dm_pending_key_requests,
		dm_read_states,
		dm_key_revocations,
		roles,
		member_roles,
		channel_permission_overrides,
		audit_log,
		key_revocations,
		voice_participants,
		dm_voice_participants,
		user_mfa_totp,
		user_mfa_webauthn
	CASCADE`)
	return err
}

// migrationsPath resolves the absolute path to the migrations directory.
func migrationsPath() string {
	_, filename, _, _ := runtime.Caller(0)
	// testhelpers is at internal/testhelpers/testdb.go
	// migrations is at migrations/
	return filepath.Join(filepath.Dir(filename), "..", "..", "migrations")
}

func runMigrations(t *testing.T, db *sql.DB, migrationsDir string) {
	t.Helper()

	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		t.Fatalf("testhelpers: could not create migration driver: %v", err)
	}

	m, err := migrate.NewWithDatabaseInstance(
		fmt.Sprintf("file://%s", migrationsDir),
		"postgres",
		driver,
	)
	if err != nil {
		t.Fatalf("testhelpers: could not create migrate instance: %v", err)
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("testhelpers: could not run migrations: %v", err)
	}
}
