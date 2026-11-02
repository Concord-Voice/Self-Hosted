package accountguard_test

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/stretchr/testify/require"
)

// A direct assignment in both directions pins the alias, not a converted enum.
var _ accountguard.Source = decide.SourceUserSession
var _ decide.Source = accountguard.SourceRecoveryKey

func sourceConnection(t *testing.T) *sql.Conn {
	t.Helper()
	require.NotEmpty(t, os.Getenv("DATABASE_URL"), "explicit disposable DATABASE_URL required")
	db, _ := testdb.SetupTestDB(t)
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	_, err = conn.ExecContext(t.Context(), `SELECT set_config('concord.auth_change_source','',false)`)
	require.NoError(t, err)
	return conn
}
func sourceTransaction(t *testing.T, conn *sql.Conn) *sql.Tx {
	t.Helper()
	tx, err := conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		err := tx.Rollback()
		if err != sql.ErrTxDone {
			require.NoError(t, err)
		}
	})
	return tx
}

type sourceReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func sourceValue(t *testing.T, reader sourceReader) sql.NullString {
	t.Helper()
	var value sql.NullString
	require.NoError(t, reader.QueryRowContext(t.Context(), `SELECT NULLIF(current_setting('concord.auth_change_source',true),'')`).Scan(&value))
	return value
}
func TestSourceTxAllowedValuesAndCallerCompletion(t *testing.T) {
	for _, source := range []accountguard.Source{accountguard.SourceUserSession, accountguard.SourceLockdown, accountguard.SourceBreakGlass, accountguard.SourceRecoveryKey} {
		for _, commit := range []bool{false, true} {
			t.Run(string(source)+map[bool]string{false: "/rollback", true: "/commit"}[commit], func(t *testing.T) {
				conn := sourceConnection(t)
				tx := sourceTransaction(t, conn)
				require.NoError(t, accountguard.SourceTx(t.Context(), tx, source))
				require.Equal(t, sql.NullString{String: string(source), Valid: true}, sourceValue(t, tx))
				var one int
				require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT 1`).Scan(&one))
				require.Equal(t, 1, one)
				if commit {
					require.NoError(t, tx.Commit())
				} else {
					require.NoError(t, tx.Rollback())
				}
				require.Equal(t, sql.NullString{}, sourceValue(t, conn), "same session must lose the transaction-local value")
				next := sourceTransaction(t, conn)
				require.Equal(t, sql.NullString{}, sourceValue(t, next))
				require.NoError(t, next.Rollback())
			})
		}
	}
}
func TestSourceTxOverwriteAndPinnedSessionIsolation(t *testing.T) {
	conn := sourceConnection(t)
	// Reserve B through a second pool on the same fixture database, without another SetupTestDB reset.
	// A separate session is needed; no pool query can prove isolation.
	db, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	other, err := db.Conn(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	_, err = other.ExecContext(t.Context(), `SELECT set_config('concord.auth_change_source','',false)`)
	require.NoError(t, err)
	tx := sourceTransaction(t, conn)
	require.NoError(t, accountguard.SourceTx(t.Context(), tx, accountguard.SourceLockdown))
	require.Equal(t, sql.NullString{String: "lockdown", Valid: true}, sourceValue(t, tx))
	require.Equal(t, sql.NullString{}, sourceValue(t, other))
	require.NoError(t, accountguard.SourceTx(t.Context(), tx, accountguard.SourceRecoveryKey))
	require.Equal(t, sql.NullString{String: "recovery_key", Valid: true}, sourceValue(t, tx))
	require.NoError(t, tx.Commit())
	require.Equal(t, sql.NullString{}, sourceValue(t, conn))
}
func TestSourceTxRejectsGrammarWithoutAbortingOrChangingCallerTransaction(t *testing.T) {
	conn := sourceConnection(t)
	tx := sourceTransaction(t, conn)
	require.NoError(t, accountguard.SourceTx(t.Context(), tx, accountguard.SourceUserSession))
	for _, source := range []accountguard.Source{accountguard.SourceOperator, accountguard.SourceUnattributed, "", "unknown-private-source", "USER_SESSION", "User_session", " user_session", "user_session ", "user_session\n", "user_session'); SELECT 1; --"} {
		err := accountguard.SourceTx(t.Context(), tx, source)
		require.Error(t, err)
		if source != "" {
			require.NotContains(t, err.Error(), string(source))
		}
		require.Equal(t, sql.NullString{String: "user_session", Valid: true}, sourceValue(t, tx))
		var one int
		require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT 1`).Scan(&one))
		require.Equal(t, 1, one)
	}
	require.NoError(t, tx.Commit())
	require.Equal(t, sql.NullString{}, sourceValue(t, conn))
}
func TestSourceTxErrorsPreserveIdentityAndCallerOwnership(t *testing.T) {
	require.Error(t, accountguard.SourceTx(t.Context(), nil, accountguard.SourceUserSession))
	conn := sourceConnection(t)
	for _, commit := range []bool{false, true} {
		tx := sourceTransaction(t, conn)
		if commit {
			require.NoError(t, tx.Commit())
		} else {
			require.NoError(t, tx.Rollback())
		}
		require.ErrorIs(t, accountguard.SourceTx(t.Context(), tx, accountguard.SourceUserSession), sql.ErrTxDone)
		// Validation must precede SQL even with a completed transaction.
		err := accountguard.SourceTx(t.Context(), tx, "private-invalid-source")
		require.Error(t, err)
		require.NotErrorIs(t, err, sql.ErrTxDone)
		require.NotContains(t, err.Error(), "private-invalid-source")
	}
	tx := sourceTransaction(t, conn)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, accountguard.SourceTx(canceled, tx, accountguard.SourceUserSession), context.Canceled)
	// Context errors need not leave a PostgreSQL transaction usable.
}
