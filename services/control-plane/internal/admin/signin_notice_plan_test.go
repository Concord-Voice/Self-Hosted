package admin_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
)

// The sign-in notice count, exactly as production sends it, uses migration
// 000202's partial index for the previous sign-in and 000203's index for the
// admin_created rows, also under a generic plan, where bound parameters are
// unknown to the planner. A partial index matches only a predicate spelled
// with the same literals, so a parameter in the inner lookup loses it.
func TestAdminsAddedQuery_UsesNoticeIndexesUnderGenericPlan(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)

	tx, err := db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	// Planner choice only: no data is needed, so forbid the alternatives.
	for _, stmt := range []string{
		`SET LOCAL enable_seqscan = off`,
		`SET LOCAL plan_cache_mode = force_generic_plan`,
		`PREPARE admins_added_plan AS ` + admin.AdminsAddedQueryForTest,
	} {
		_, err = tx.Exec(stmt)
		require.NoError(t, err, stmt)
	}
	rows, err := tx.Query(`EXPLAIN EXECUTE admins_added_plan(
		'00000000-0000-0000-0000-000000000001', 'admin_created', 100, 'success', '^x$')`)
	require.NoError(t, err)
	var plan strings.Builder
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		plan.WriteString(line + "\n")
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	assert.Contains(t, plan.String(), "idx_admin_audit_login_success", plan.String())
	assert.Contains(t, plan.String(), "idx_admin_audit_event_result_ts", plan.String())
}
