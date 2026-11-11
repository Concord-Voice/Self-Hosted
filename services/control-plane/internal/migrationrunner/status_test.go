package migrationrunner_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/migrationrunner"
)

const notCurrentMessage = "schema not current for this binary (verify mode)"

// twoMigrations writes versions 1 and 2 and returns the file:// URL.
func twoMigrations(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeMigrationPair(t, dir, 1, "a")
	writeMigrationPair(t, dir, 2, "b")
	return "file://" + dir
}

// oneMigration writes version 1 only (same table name as twoMigrations' first).
func oneMigration(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeMigrationPair(t, dir, 1, "a")
	return "file://" + dir
}

func TestReadStatus_Uninitialised(t *testing.T) {
	db := freshSchemaDB(t)

	st, err := migrationrunner.ReadStatus(context.Background(), db, twoMigrations(t))
	require.NoError(t, err)
	// Mutation: report Initialised=true when the version table is absent.
	require.Equal(t, migrationrunner.Status{Initialised: false, Latest: 2}, st)
	require.False(t, st.Current())
}

func TestReadStatus_Current(t *testing.T) {
	db := freshSchemaDB(t)
	src := twoMigrations(t)
	require.NoError(t, migrationrunner.Run(db, src))

	st, err := migrationrunner.ReadStatus(context.Background(), db, src)
	require.NoError(t, err)
	// Mutation: read Latest from the first migration instead of walking to the last.
	require.Equal(t, migrationrunner.Status{Initialised: true, Version: 2, Dirty: false, Latest: 2}, st)
	require.True(t, st.Current())
}

func TestReadStatus_Pending(t *testing.T) {
	db := freshSchemaDB(t)
	require.NoError(t, migrationrunner.Run(db, oneMigration(t)))

	st, err := migrationrunner.ReadStatus(context.Background(), db, twoMigrations(t))
	require.NoError(t, err)
	// Mutation: compute Latest as the database version.
	require.Equal(t, migrationrunner.Status{Initialised: true, Version: 1, Latest: 2}, st)
	require.False(t, st.Current())
}

func TestReadStatus_Ahead(t *testing.T) {
	db := freshSchemaDB(t)
	require.NoError(t, migrationrunner.Run(db, twoMigrations(t)))

	st, err := migrationrunner.ReadStatus(context.Background(), db, oneMigration(t))
	require.NoError(t, err)
	// Mutation: treat Version != Latest as only "pending" by clamping Version to Latest.
	require.Equal(t, migrationrunner.Status{Initialised: true, Version: 2, Latest: 1}, st)
	require.False(t, st.Current())
}

func TestReadStatus_Dirty(t *testing.T) {
	db := freshSchemaDB(t)
	src := twoMigrations(t)
	require.NoError(t, migrationrunner.Run(db, src))
	_, err := db.Exec(`UPDATE schema_migrations SET dirty = TRUE`)
	require.NoError(t, err)

	st, err := migrationrunner.ReadStatus(context.Background(), db, src)
	require.NoError(t, err)
	// Mutation: drop the dirty column from the read, or ignore it in Current().
	require.Equal(t, migrationrunner.Status{Initialised: true, Version: 2, Dirty: true, Latest: 2}, st)
	require.False(t, st.Current(), "a dirty database at the latest version is not current")
}

func TestReadStatus_TwoRowsIsError(t *testing.T) {
	db := freshSchemaDB(t)
	src := twoMigrations(t)
	require.NoError(t, migrationrunner.Run(db, src))
	_, err := db.Exec(`INSERT INTO schema_migrations (version, dirty) VALUES (99, FALSE)`)
	require.NoError(t, err)

	_, err = migrationrunner.ReadStatus(context.Background(), db, src)
	// Mutation: take the first row and ignore any further ones.
	require.Error(t, err)
}

func TestReadStatus_EmptySourceIsError(t *testing.T) {
	db := freshSchemaDB(t)

	_, err := migrationrunner.ReadStatus(context.Background(), db, "file://"+t.TempDir())
	// Mutation: treat an empty source as Latest 0 instead of failing.
	require.Error(t, err)
}

func TestReadStatus_UnreadableSourceIsError(t *testing.T) {
	db := freshSchemaDB(t)

	_, err := migrationrunner.ReadStatus(context.Background(), db, "file://"+filepath.Join(t.TempDir(), "missing"))
	// Mutation: swallow the source-open error and report Latest 0.
	require.Error(t, err)
}

func TestReadStatus_ClosedDatabaseIsError(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://127.0.0.1:1/unreachable?sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, db.Close())

	_, err = migrationrunner.ReadStatus(context.Background(), db, oneMigration(t))
	// Mutation: return a zero Status and nil error when the transaction cannot begin.
	require.Error(t, err)
}

func advisoryLockCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory'
		   AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&n))
	return n
}

func relationCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM pg_class WHERE relnamespace = to_regnamespace(quote_ident(current_schema()))`).Scan(&n))
	return n
}

func TestReadStatus_WritesNothing(t *testing.T) {
	db := freshSchemaDB(t)
	locksBefore := advisoryLockCount(t, db)
	relsBefore := relationCount(t, db)

	st, err := migrationrunner.ReadStatus(context.Background(), db, twoMigrations(t))
	require.NoError(t, err)
	require.False(t, st.Initialised)

	var exists bool
	require.NoError(t, db.QueryRow(
		`SELECT to_regclass(quote_ident(current_schema()) || '.schema_migrations') IS NOT NULL`).Scan(&exists))
	// Mutation: build the migrate driver (postgres.WithInstance), which creates the version table.
	require.False(t, exists, "ReadStatus must not create schema_migrations")
	require.Equal(t, relsBefore, relationCount(t, db), "ReadStatus must create no relation")
	// Mutation: take golang-migrate's advisory lock and leave it held on the pooled connection.
	require.Equal(t, locksBefore, advisoryLockCount(t, db), "ReadStatus must hold no advisory lock")
}

func TestVerify_ErrorsOnEveryNonCurrentCase(t *testing.T) {
	ctx := context.Background()

	t.Run("current passes", func(t *testing.T) {
		db := freshSchemaDB(t)
		src := twoMigrations(t)
		require.NoError(t, migrationrunner.Run(db, src))
		// Mutation: return an error unconditionally.
		require.NoError(t, migrationrunner.Verify(ctx, db, src))
	})

	cases := map[string]func(t *testing.T) (*sql.DB, string){
		"uninitialised": func(t *testing.T) (*sql.DB, string) { return freshSchemaDB(t), twoMigrations(t) },
		"pending": func(t *testing.T) (*sql.DB, string) {
			db := freshSchemaDB(t)
			require.NoError(t, migrationrunner.Run(db, oneMigration(t)))
			return db, twoMigrations(t)
		},
		"ahead": func(t *testing.T) (*sql.DB, string) {
			db := freshSchemaDB(t)
			require.NoError(t, migrationrunner.Run(db, twoMigrations(t)))
			return db, oneMigration(t)
		},
		"dirty": func(t *testing.T) (*sql.DB, string) {
			db := freshSchemaDB(t)
			src := twoMigrations(t)
			require.NoError(t, migrationrunner.Run(db, src))
			_, err := db.Exec(`UPDATE schema_migrations SET dirty = TRUE`)
			require.NoError(t, err)
			return db, src
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			db, src := build(t)
			// Mutation: Verify returns nil for this case (e.g. compares only Version >= Latest).
			require.EqualError(t, migrationrunner.Verify(ctx, db, src), notCurrentMessage)
		})
	}

	t.Run("read failure is an error", func(t *testing.T) {
		db := freshSchemaDB(t)
		// Mutation: Verify swallows a ReadStatus error.
		require.Error(t, migrationrunner.Verify(ctx, db, "file://"+t.TempDir()))
	})

	t.Run("uninitialised verify writes nothing", func(t *testing.T) {
		db := freshSchemaDB(t)
		require.Error(t, migrationrunner.Verify(ctx, db, twoMigrations(t)))
		var exists bool
		require.NoError(t, db.QueryRow(
			`SELECT to_regclass(quote_ident(current_schema()) || '.schema_migrations') IS NOT NULL`).Scan(&exists))
		// Mutation: Verify falls back to Run when uninitialised.
		require.False(t, exists)
	})
}

func TestStatusLine_ExactFormats(t *testing.T) {
	cases := []struct {
		name    string
		st      migrationrunner.Status
		line    string
		current bool
	}{
		{"current", migrationrunner.Status{Initialised: true, Version: 194, Latest: 194}, "current 194", true},
		{"pending", migrationrunner.Status{Initialised: true, Version: 190, Latest: 194}, "pending 190 194", false},
		{"ahead", migrationrunner.Status{Initialised: true, Version: 200, Latest: 194}, "ahead 200 194", false},
		{"dirty", migrationrunner.Status{Initialised: true, Version: 194, Dirty: true, Latest: 194}, "dirty 194", false},
		{"dirty behind", migrationrunner.Status{Initialised: true, Version: 190, Dirty: true, Latest: 194}, "dirty 190", false},
		{"uninitialised", migrationrunner.Status{Latest: 194}, "uninitialised 194", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Mutation: any change to a word, order, separator or trailing newline; or letting dirty lose to pending/ahead.
			require.Equal(t, tc.line, tc.st.Line())
			require.Equal(t, tc.current, tc.st.Current())
		})
	}
}

// keep os imported for future fixtures without lint churn.
var _ = os.ErrNotExist
