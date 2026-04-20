package users_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/users"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// seedAccountTestUser inserts a bare-minimum user row and returns its id.
// Local to the account-deletion tests so the fixture does not couple to the
// broader setupTS test server used by the handlers test file.
func seedAccountTestUser(t *testing.T, db *sql.DB, username string) string {
	t.Helper()
	email := fmt.Sprintf("%s@test.local", username)
	stmt, err := db.Prepare(
		`INSERT INTO users (username, email, password_hash)
		 VALUES ($1, $2, $3) RETURNING id`,
	)
	require.NoError(t, err)
	defer stmt.Close() //nolint:errcheck
	var id string
	require.NoError(t, stmt.QueryRow(username, email, "not-a-real-hash").Scan(&id))
	return id
}

// TestAccountService_DeleteAccount_HappyPath_SentryAttempted exercises the
// canonical success path: DELETE removes one row, the audit INSERT records
// sentry_delete_attempted = TRUE, and the transaction commits.
func TestAccountService_DeleteAccount_HappyPath_SentryAttempted(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	svc := users.NewAccountService(db, logger.New("test"))
	uid := seedAccountTestUser(t, db, "svc-happy-sentry")

	err := svc.DeleteAccount(context.Background(), uid, true)
	require.NoError(t, err)

	var userCount int
	require.NoError(t,
		db.QueryRow(`SELECT COUNT(*) FROM users WHERE id = $1`, uid).Scan(&userCount),
	)
	assert.Equal(t, 0, userCount, "user row must be deleted")

	var sentryAttempted bool
	var auditUserID sql.NullString
	require.NoError(t, db.QueryRow(
		`SELECT user_id, sentry_delete_attempted FROM account_deletions
		 ORDER BY deleted_at DESC LIMIT 1`,
	).Scan(&auditUserID, &sentryAttempted))
	assert.True(t, sentryAttempted, "audit row must record Sentry attempt")
	assert.False(t, auditUserID.Valid, "audit row user_id must be NULL (post-commit state)")
}

// TestAccountService_DeleteAccount_HappyPath_NoSentry confirms the
// sentry_delete_attempted boolean propagates the false value when the
// handler skipped Sentry.
func TestAccountService_DeleteAccount_HappyPath_NoSentry(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	svc := users.NewAccountService(db, logger.New("test"))
	uid := seedAccountTestUser(t, db, "svc-happy-nosentry")

	err := svc.DeleteAccount(context.Background(), uid, false)
	require.NoError(t, err)

	var sentryAttempted bool
	require.NoError(t, db.QueryRow(
		`SELECT sentry_delete_attempted FROM account_deletions
		 ORDER BY deleted_at DESC LIMIT 1`,
	).Scan(&sentryAttempted))
	assert.False(t, sentryAttempted, "audit row must record no Sentry attempt")
}

// TestAccountService_DeleteAccount_UserNotFound_ReturnsSentinel exercises
// the DELETE-with-zero-rows-affected path. The service must return
// ErrUserNotFound and must NOT write an audit row (nothing happened).
func TestAccountService_DeleteAccount_UserNotFound_ReturnsSentinel(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	svc := users.NewAccountService(db, logger.New("test"))

	// A valid-shape UUID that does not correspond to any seeded row.
	missing := "00000000-0000-0000-0000-000000000001"
	err := svc.DeleteAccount(context.Background(), missing, false)
	require.Error(t, err)
	assert.True(t, errors.Is(err, users.ErrUserNotFound),
		"expected ErrUserNotFound, got %v", err)

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM account_deletions`).Scan(&count))
	assert.Equal(t, 0, count, "no audit row should exist when user was already gone")
}

// TestAccountService_DeleteAccount_AuditInsertFails_Rollback forces the
// audit-INSERT to fail (by renaming the audit table mid-test) and verifies
// that the transaction rolls back cleanly — the user row must still exist.
func TestAccountService_DeleteAccount_AuditInsertFails_Rollback(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	svc := users.NewAccountService(db, logger.New("test"))
	uid := seedAccountTestUser(t, db, "svc-rollback")

	// Rename preserves all attached indexes and constraints; the reverse rename
	// in defer restores state cleanly. defer runs BEFORE the SetupTestDB
	// cleanup (which calls TruncateAllTables and would otherwise fail on the
	// renamed table).
	_, err := db.Exec(`ALTER TABLE account_deletions RENAME TO account_deletions_hidden_unit_test`)
	require.NoError(t, err)
	defer func() {
		_, _ = db.Exec(`ALTER TABLE account_deletions_hidden_unit_test RENAME TO account_deletions`)
	}()

	err = svc.DeleteAccount(context.Background(), uid, false)
	require.Error(t, err)
	assert.False(t, errors.Is(err, users.ErrUserNotFound),
		"audit-INSERT failure must not be reported as ErrUserNotFound")

	// Rollback invariant: the user row must still exist because the DELETE
	// was inside the same transaction as the failed INSERT.
	var count int
	require.NoError(t,
		db.QueryRow(`SELECT COUNT(*) FROM users WHERE id = $1`, uid).Scan(&count),
	)
	assert.Equal(t, 1, count, "user row must survive audit-INSERT rollback")
}

// TestNewAccountService_NilLoggerTolerated exercises the documented
// test-only affordance: constructing the service with a nil logger must
// not panic. The happy-path delete exercised here does not itself fire
// the rollback-error log branch (which is the only place the logger is
// dereferenced), but the constructor and the common path must not panic
// on a nil log value either way. Rollback-error-with-nil-log is an
// uninducible path on real Postgres (BeginTx + Commit don't fail here)
// and is covered by inspection of the nil-check at the rollback site.
func TestNewAccountService_NilLoggerTolerated(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	svc := users.NewAccountService(db, nil)
	require.NotNil(t, svc)

	uid := seedAccountTestUser(t, db, "svc-nil-log")
	err := svc.DeleteAccount(context.Background(), uid, false)
	require.NoError(t, err)
}
