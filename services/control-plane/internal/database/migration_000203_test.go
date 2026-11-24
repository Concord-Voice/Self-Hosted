package database_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
)

// Migrations 000203 and 000204 index admin_audit_log for the admin sign-in
// notice (AuditLog.AdminsAddedSinceLastSignIn). 000201 and 000202 belong to
// other changes; these files stand at 000203 and 000204.

// migrationStatements returns the file's SQL statements, comments dropped.
func migrationStatements(t *testing.T, path string) []string {
	t.Helper()
	var sql []string
	for _, line := range strings.Split(migrationReadFile(t, path), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "--") {
			sql = append(sql, trimmed)
		}
	}
	var out []string
	for _, stmt := range strings.Split(strings.Join(sql, " "), ";") {
		if stmt = strings.TrimSpace(stmt); stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}

// Each file holds exactly one CONCURRENTLY statement: golang-migrate sends a
// file as one simple query, and PostgreSQL wraps a multi-statement query in a
// transaction, where CONCURRENTLY cannot run.
func TestMigration000203_000204_AreSingleConcurrentStatements(t *testing.T) {
	cases := []struct {
		file string
		want string
	}{
		{"../../migrations/000203_admin_audit_login_success_index.up.sql",
			"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_admin_audit_login_success ON admin_audit_log (admin_id, ts DESC) WHERE event_type = 'login_success' AND result = 'success'"},
		{"../../migrations/000203_admin_audit_login_success_index.down.sql",
			"DROP INDEX CONCURRENTLY IF EXISTS idx_admin_audit_login_success"},
		{"../../migrations/000204_admin_audit_event_result_ts_index.up.sql",
			"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_admin_audit_event_result_ts ON admin_audit_log (event_type, result, ts)"},
		{"../../migrations/000204_admin_audit_event_result_ts_index.down.sql",
			"DROP INDEX CONCURRENTLY IF EXISTS idx_admin_audit_event_result_ts"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			assert.Equal(t, []string{tc.want}, migrationStatements(t, tc.file))
		})
	}
}

// After the runner applies them, both indexes exist, are valid, and have the
// pinned definitions, and the sign-in lookup uses the partial index.
func TestMigration000203_000204_IndexesAreValidAndUsed(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)

	for name, def := range map[string]string{
		"idx_admin_audit_login_success":   "(admin_id, ts DESC) WHERE ((event_type = 'login_success'::text) AND (result = 'success'::text))",
		"idx_admin_audit_event_result_ts": "(event_type, result, ts)",
	} {
		var valid bool
		var got string
		require.NoError(t, db.QueryRow(`
			SELECT i.indisvalid, pg_get_indexdef(i.indexrelid)
			FROM pg_index i
			WHERE i.indexrelid = to_regclass($1)`, name).Scan(&valid, &got), name)
		assert.True(t, valid, name)
		assert.Contains(t, got, "ON public.admin_audit_log USING btree "+def, name)
	}

	tx, err := db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	// Planner choice only: no data is needed, so forbid the alternatives.
	_, err = tx.Exec(`SET LOCAL enable_seqscan = off`)
	require.NoError(t, err)
	rows, err := tx.Query(`EXPLAIN SELECT ts FROM admin_audit_log
		WHERE admin_id = '00000000-0000-0000-0000-000000000001' AND event_type = 'login_success' AND result = 'success'
		ORDER BY ts DESC LIMIT 1`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var plan strings.Builder
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		plan.WriteString(line + "\n")
	}
	require.NoError(t, rows.Err())
	assert.Contains(t, plan.String(), "idx_admin_audit_login_success", plan.String())
}
