package migrationrunner_test

import (
	"context"
	"database/sql"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/migrationrunner"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// Each case builds a version table that can fail in exactly one way, so the
// asserted message names the branch under test and no other.

func TestReadStatus_EmptyVersionTableIsUninitialised(t *testing.T) {
	db := freshSchemaDB(t)
	_, err := db.Exec(`CREATE TABLE schema_migrations (version bigint NOT NULL, dirty boolean NOT NULL)`)
	require.NoError(t, err)

	st, err := migrationrunner.ReadStatus(context.Background(), db, twoMigrations(t))
	require.NoError(t, err)
	// Mutation: set Initialised when the table exists but holds no row.
	require.Equal(t, migrationrunner.Status{Initialised: false, Latest: 2}, st)
}

func TestReadStatus_VersionTableWithoutDirtyIsError(t *testing.T) {
	db := freshSchemaDB(t)
	_, err := db.Exec(`CREATE TABLE schema_migrations (version bigint NOT NULL)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO schema_migrations (version) VALUES (2)`)
	require.NoError(t, err)

	_, err = migrationrunner.ReadStatus(context.Background(), db, twoMigrations(t))
	// Mutation: ignore the query error and report the table uninitialised.
	require.ErrorContains(t, err, "read migration version: ")
	require.ErrorContains(t, err, `"dirty"`)
}

func TestReadStatus_NonIntegerVersionIsError(t *testing.T) {
	db := freshSchemaDB(t)
	_, err := db.Exec(`CREATE TABLE schema_migrations (version text NOT NULL, dirty boolean NOT NULL)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO schema_migrations (version, dirty) VALUES ('two', FALSE)`)
	require.NoError(t, err)

	_, err = migrationrunner.ReadStatus(context.Background(), db, twoMigrations(t))
	// Mutation: skip a row that does not scan instead of failing.
	require.ErrorContains(t, err, "scan migration version: ")
}

func TestReadStatus_NegativeVersionIsError(t *testing.T) {
	db := freshSchemaDB(t)
	src := twoMigrations(t)
	require.NoError(t, migrationrunner.Run(db, src))
	// golang-migrate's own record of a failed down past the first version.
	_, err := db.Exec(`UPDATE schema_migrations SET version = -1, dirty = TRUE`)
	require.NoError(t, err)

	_, err = migrationrunner.ReadStatus(context.Background(), db, src)
	// Mutation: cast the negative version to uint (it would read as a huge "ahead").
	require.ErrorContains(t, err, "recorded version -1 is negative")
}

func TestReadStatus_NoCurrentSchemaIsError(t *testing.T) {
	// SetupTestDB first: it takes the shared test-database lock for this test.
	_, _ = testhelpers.SetupTestDB(t)
	u, err := url.Parse(testdb.DatabaseURL())
	require.NoError(t, err)
	query := u.Query()
	// A search_path naming only a schema that does not exist leaves
	// current_schema() NULL.
	query.Set("search_path", "migrationrunner_no_such_schema")
	u.RawQuery = query.Encode()
	db, err := sql.Open("postgres", u.String())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	_, err = migrationrunner.ReadStatus(context.Background(), db, twoMigrations(t))
	// Mutation: qualify the version table with an empty schema name instead of failing.
	require.ErrorContains(t, err, "read current schema: no current schema")
}
