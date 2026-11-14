package database_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/database"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

type recordedVersion struct {
	version int64
	dirty   bool
}

// setRecordedVersions replaces the shared test database's version table with
// rows. SetupTestDB holds the shared test-database lock for the calling test.
func setRecordedVersions(t *testing.T, db *sql.DB, rows ...recordedVersion) {
	t.Helper()
	_, err := db.Exec(`DELETE FROM schema_migrations`)
	require.NoError(t, err)
	for _, row := range rows {
		_, err := db.Exec(`INSERT INTO schema_migrations (version, dirty) VALUES ($1, $2)`, row.version, row.dirty)
		require.NoError(t, err)
	}
}

func readRecordedVersions(t *testing.T, db *sql.DB) []recordedVersion {
	t.Helper()
	rows, err := db.Query(`SELECT version, dirty FROM schema_migrations ORDER BY version`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var got []recordedVersion
	for rows.Next() {
		var row recordedVersion
		require.NoError(t, rows.Scan(&row.version, &row.dirty))
		got = append(got, row)
	}
	require.NoError(t, rows.Err())
	return got
}

func TestVerifyMigrations_CurrentSchemaPasses(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	t.Chdir("../..")

	// Mutation: return the not-current error unconditionally.
	require.NoError(t, database.VerifyMigrations(context.Background(), db))
}

func TestVerifyMigrations_RefusesWithStatusLineAndWritesNothing(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	var latest int64
	require.NoError(t, db.QueryRow(`SELECT version FROM schema_migrations`).Scan(&latest))
	t.Cleanup(func() { setRecordedVersions(t, db, recordedVersion{version: latest}) })
	t.Chdir("../..")

	cases := []struct {
		name   string
		rows   []recordedVersion
		status string
	}{
		{"uninitialised", nil, fmt.Sprintf("uninitialised %d", latest)},
		{"pending", []recordedVersion{{version: latest - 1}}, fmt.Sprintf("pending %d %d", latest-1, latest)},
		{"ahead", []recordedVersion{{version: latest + 1}}, fmt.Sprintf("ahead %d %d", latest+1, latest)},
		{"dirty", []recordedVersion{{version: latest, dirty: true}}, fmt.Sprintf("dirty %d", latest)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setRecordedVersions(t, db, tc.rows...)

			err := database.VerifyMigrations(context.Background(), db)
			// Mutation: pass this case (e.g. compare only Version >= Latest), or
			// refuse without the status line an operator needs to act on.
			require.EqualError(t, err, "schema not current for this binary (verify mode): "+tc.status)
			// Mutation: fall back to applying migrations when the schema is not current.
			require.Equal(t, tc.rows, readRecordedVersions(t, db))
		})
	}
}

func TestVerifyMigrations_ReadFailureIsAnError(t *testing.T) {
	db, err := sql.Open("postgres", "postgres:///unused?sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, db.Close())

	err = database.VerifyMigrations(context.Background(), db)
	// Mutation: swallow the status read error and report the schema current.
	require.ErrorContains(t, err, "read migration status: begin read-only migration status transaction")
}
