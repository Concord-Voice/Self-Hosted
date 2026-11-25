package admin_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
)

// The sign-in notice query, exactly as production sends it, finds the
// viewer's sign-ins through migration 000203's partial index also under a
// generic plan, where the bound admin id is unknown to the planner. A partial
// index matches only a predicate spelled with the same literals, so a bound
// event_type or result in a login_success lookup would lose it.
func TestSignInNoticeQuery_UsesIndexesUnderGenericPlan(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)

	tx, err := db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	// Planner choice only: no data is needed, so forbid the alternatives.
	for _, stmt := range []string{
		`SET LOCAL enable_seqscan = off`,
		`SET LOCAL plan_cache_mode = force_generic_plan`,
		`PREPARE signin_notice_plan AS ` + admin.SignInNoticeQueryForTest,
	} {
		_, err = tx.Exec(stmt)
		require.NoError(t, err, stmt)
	}
	rows, err := tx.Query(`EXPLAIN EXECUTE signin_notice_plan('00000000-0000-0000-0000-000000000001')`)
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
