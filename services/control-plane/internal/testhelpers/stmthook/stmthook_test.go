package stmthook_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/stmthook"
	dbtest "github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/testdb"
)

// Between's deferred rollback is a no-op only after a successful Commit. When an
// earlier step fails it is the real cleanup, and a rollback that fails too must
// reach the caller rather than be dropped behind the first error.
func TestBetween_ReportsARollbackThatFails(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()

	// The statement ends its own session, so it fails, and the rollback that
	// follows runs on a connection the server has already closed.
	sc := stmthook.Scenario{
		Name:      "the interleaved write kills its own connection",
		DeleteSQL: `SELECT pg_terminate_backend(pg_backend_pid()) WHERE $1::text IS NOT NULL`,
	}
	err := sc.Between(db, "owner", "server")()
	require.Error(t, err)
	require.ErrorContains(t, err, "rollback")
}

func scanOne(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	var got string
	require.NoError(t, db.QueryRowContext(context.Background(), query).Scan(&got))
	return got
}

// The arm is spent by the first query that matches it: a later query with the
// same text, and one that never matched, both return what the database sent.
func TestArmRowValue_RewritesOnlyTheNextMatchingQuery(t *testing.T) {
	hook, db := stmthook.Open(t)
	const matching = `SELECT 'real' /* row-value-target */`

	hook.ArmRowValue("row-value-target", 0, "injected")

	assert.Equal(t, "real", scanOne(t, db, `SELECT 'real' /* row-value-other */`), "a non-matching query is not rewritten")
	assert.Equal(t, "injected", scanOne(t, db, matching), "the next matching query scans the injected value")
	assert.Equal(t, "real", scanOne(t, db, matching), "a second matching query is not rewritten")
	assert.Equal(t, 1, hook.RowValueHits())
}

func TestArmRowValue_CountsRewrittenRows(t *testing.T) {
	hook, db := stmthook.Open(t)
	hook.ArmRowValue("row-value-count", 0, "injected")

	rows, err := db.QueryContext(context.Background(), `SELECT 'real' FROM generate_series(1, 3) /* row-value-count */`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	for rows.Next() {
		var got string
		require.NoError(t, rows.Scan(&got))
		require.Equal(t, "injected", got)
	}
	require.NoError(t, rows.Err())

	assert.Equal(t, 3, hook.RowValueHits())
}
