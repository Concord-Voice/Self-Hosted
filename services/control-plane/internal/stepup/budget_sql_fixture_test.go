package stepup

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// Prospective schema only: no migration number has been allocated or deployed.
const sqlBudgetFixtureDDL = `
CREATE FUNCTION step_up_receipts_valid(items uuid[]) RETURNS boolean
LANGUAGE sql IMMUTABLE STRICT AS $$
 SELECT CASE WHEN cardinality(items)=0 THEN array_ndims(items) IS NULL
 WHEN array_ndims(items)<>1 OR array_lower(items,1)<>1 OR cardinality(items)>5 THEN false
 ELSE NOT EXISTS (SELECT 1 FROM unnest(items) AS item WHERE item IS NULL OR item='00000000-0000-0000-0000-000000000000'::uuid)
 AND (SELECT count(DISTINCT item) FROM unnest(items) AS item)=cardinality(items) END
$$;
CREATE TABLE step_up_attempt_budgets (
 user_id uuid NOT NULL CHECK (user_id<>'00000000-0000-0000-0000-000000000000'),
 family_id uuid NOT NULL CHECK (family_id<>'00000000-0000-0000-0000-000000000000'),
 window_id uuid NOT NULL CHECK (window_id<>'00000000-0000-0000-0000-000000000000'),
 window_started_at timestamptz NOT NULL CHECK (isfinite(window_started_at)),
 expires_at timestamptz NOT NULL CHECK (isfinite(expires_at)),
 charged_receipts uuid[] NOT NULL CONSTRAINT budget_test_receipts_valid CHECK (step_up_receipts_valid(charged_receipts)),
 PRIMARY KEY(user_id,family_id),
 FOREIGN KEY(family_id,user_id) REFERENCES login_families(family_id,user_id) ON DELETE RESTRICT,
 CONSTRAINT budget_test_window_span CHECK (expires_at=window_started_at+interval '900 seconds')
);`

type sqlBudgetFixture struct {
	budgetActorHTTPFixture
	actor BudgetActor
}

func newSQLBudgetFixture(t *testing.T, epoch string) sqlBudgetFixture {
	t.Helper()
	f := newBudgetActorHTTPFixture(t, epoch)
	_, err := f.db.ExecContext(t.Context(), sqlBudgetFixtureDDL)
	require.NoError(t, err)
	t.Cleanup(func() {
		// Registered after the actor fixture: remove restrictive evidence before its reset.
		_, err := f.db.Exec(`DROP TABLE step_up_attempt_budgets; DROP FUNCTION step_up_receipts_valid(uuid[])`)
		require.NoError(t, err)
	})
	return sqlBudgetFixture{budgetActorHTTPFixture: f, actor: sqlBudgetCaptureActor(t, f)}
}

func sqlBudgetCaptureActor(t *testing.T, f budgetActorHTTPFixture) BudgetActor {
	t.Helper()
	var actor BudgetActor
	w := f.request(t, f.claims(), nil, func(c *gin.Context) {
		var failure *Error
		actor, failure = BudgetActorFromContext(c)
		require.Nil(t, failure)
		c.Status(http.StatusNoContent)
	})
	require.Equal(t, http.StatusNoContent, w.Code)
	requireBudgetActorSnapshot(t, f, actor)
	return actor
}

func sqlBudgetTx(t *testing.T, db *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		err := tx.Rollback()
		if !errors.Is(err, sql.ErrTxDone) {
			require.NoError(t, err)
		}
	})
	return tx
}

func sqlBudgetOtherPool(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	require.NotEmpty(t, dsn, "explicit disposable DATABASE_URL required")
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

type sqlBudgetRow struct {
	user, family, window uuid.UUID
	started, expires     time.Time
	receipts             []uuid.UUID
}

func sqlBudgetRead(t *testing.T, db *sql.DB, actor BudgetActor) sqlBudgetRow {
	t.Helper()
	var row sqlBudgetRow
	var receipts pq.StringArray
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT user_id,family_id,window_id,window_started_at,expires_at,charged_receipts FROM step_up_attempt_budgets WHERE user_id=$1 AND family_id=$2`, actor.identity.UserID, actor.expectedFamilyID).Scan(&row.user, &row.family, &row.window, &row.started, &row.expires, &receipts))
	for _, item := range receipts {
		id, err := uuid.Parse(item)
		require.NoError(t, err)
		row.receipts = append(row.receipts, id)
	}
	return row
}
func sqlBudgetCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM step_up_attempt_budgets`).Scan(&count))
	return count
}
func sqlBudgetNow(t *testing.T, db *sql.DB) time.Time {
	t.Helper()
	var now time.Time
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT clock_timestamp()`).Scan(&now))
	return now
}
func sqlBudgetSeed(t *testing.T, f sqlBudgetFixture, expiryOffset string, receipts []uuid.UUID) {
	t.Helper()
	values := make([]string, len(receipts))
	for i, id := range receipts {
		values[i] = id.String()
	}
	_, err := f.db.ExecContext(t.Context(), `WITH moment AS (SELECT clock_timestamp()+$4::interval AS expiry) INSERT INTO step_up_attempt_budgets(user_id,family_id,window_id,window_started_at,expires_at,charged_receipts) SELECT $1,$2,$3,expiry-interval '900 seconds',expiry,$5 FROM moment`, f.actor.identity.UserID, f.actor.expectedFamilyID, uuid.New(), expiryOffset, pq.Array(values))
	require.NoError(t, err)
}
func sqlBudgetNewFamily(t *testing.T, f sqlBudgetFixture, sameFamily bool) BudgetActor {
	t.Helper()
	next := f.budgetActorHTTPFixture
	next.identity.SID = uuid.New()
	tx := sqlBudgetTx(t, f.db)
	barrier, err := loginfamily.AcquireWriterBarrierTx(t.Context(), tx)
	require.NoError(t, err)
	users, err := loginfamily.LockUsersTx(t.Context(), tx, barrier, []uuid.UUID{next.identity.UserID}, loginfamily.UserNoKeyUpdate)
	require.NoError(t, err)
	user, err := users.ForUser(next.identity.UserID)
	require.NoError(t, err)
	var family loginfamily.LiveFamily
	if sameFamily {
		family, err = loginfamily.GuardAfterUserLockTx(t.Context(), tx, user, f.identity, loginfamily.FamilyNoKeyUpdate)
	} else {
		family, err = loginfamily.CreateFamilyTx(t.Context(), tx, user, next.identity.CredentialEpoch)
	}
	require.NoError(t, err)
	require.NoError(t, loginfamily.BindSIDTx(t.Context(), tx, family, next.identity.SID, next.identity.AccessExpiresAt))
	next.familyID = family.Binding().FamilyID
	require.NoError(t, tx.Commit())
	return sqlBudgetCaptureActor(t, next)
}

// This wraps real PostgreSQL transactions, not SQL results or production hooks.
// A successful real COMMIT followed by a synthetic transport result models an
// unknown outcome. Never use ErrBadConn: database/sql must not replay anything.
type sqlBudgetCommitFault struct {
	begins, commits atomic.Int32
	lose            atomic.Bool
}
type sqlBudgetConnector struct {
	base  driver.Connector
	fault *sqlBudgetCommitFault
}

func (c *sqlBudgetConnector) Driver() driver.Driver { return c.base.Driver() }
func (c *sqlBudgetConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &sqlBudgetFaultConn{Conn: conn, fault: c.fault}, nil
}

type sqlBudgetFaultConn struct {
	driver.Conn
	fault *sqlBudgetCommitFault
}

func (c *sqlBudgetFaultConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.fault.begins.Add(1)
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &sqlBudgetFaultTx{Tx: tx, fault: c.fault}, nil
}
func (c *sqlBudgetFaultConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
}
func (c *sqlBudgetFaultConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}

// Forward libpq optional connection semantics rather than changing conversion,
// pooled connection validation or context behavior around the real transaction.
func (c *sqlBudgetFaultConn) CheckNamedValue(value *driver.NamedValue) error {
	if checker, ok := c.Conn.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(value)
	}
	return driver.ErrSkip
}
func (c *sqlBudgetFaultConn) ResetSession(ctx context.Context) error {
	if resetter, ok := c.Conn.(driver.SessionResetter); ok {
		return resetter.ResetSession(ctx)
	}
	return nil
}
func (c *sqlBudgetFaultConn) IsValid() bool {
	if validator, ok := c.Conn.(driver.Validator); ok {
		return validator.IsValid()
	}
	return true
}
func (c *sqlBudgetFaultConn) Ping(ctx context.Context) error {
	if pinger, ok := c.Conn.(driver.Pinger); ok {
		return pinger.Ping(ctx)
	}
	return nil
}

type sqlBudgetFaultTx struct {
	driver.Tx
	fault *sqlBudgetCommitFault
}

func (tx *sqlBudgetFaultTx) Commit() error {
	tx.fault.commits.Add(1)
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	if tx.fault.lose.Swap(false) {
		return errors.New("synthetic lost COMMIT result")
	}
	return nil
}
func sqlBudgetFaultPool(t *testing.T) (*sql.DB, *sqlBudgetCommitFault) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	require.NotEmpty(t, dsn, "explicit disposable DATABASE_URL required")
	connector, err := pq.NewConnector(dsn)
	require.NoError(t, err)
	fault := &sqlBudgetCommitFault{}
	db := sql.OpenDB(&sqlBudgetConnector{base: connector, fault: fault})
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db, fault
}

func sqlBudgetPID(t *testing.T, db *sql.DB) int {
	t.Helper()
	var pid int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid))
	return pid
}
func sqlBudgetWaitForLock(t *testing.T, observer *sql.DB, pid int) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting bool
		err := observer.QueryRowContext(t.Context(), `SELECT COALESCE(wait_event_type='Lock',false) FROM pg_stat_activity WHERE pid=$1`, pid).Scan(&waiting)
		return err == nil && waiting
	}, 400*time.Millisecond, time.Millisecond, "operation must actually reach a PostgreSQL lock wait")
}

type sqlBudgetAttemptResult struct {
	reservation budgetReservation
	failure     *budgetFailure
}

func sqlBudgetReserveAsync(ctx context.Context, store *sqlBudgetStore, actor BudgetActor) <-chan sqlBudgetAttemptResult {
	done := make(chan sqlBudgetAttemptResult, 1)
	go func() {
		reservation, failure := store.reserve(ctx, actor)
		done <- sqlBudgetAttemptResult{reservation, failure}
	}()
	return done
}
func sqlBudgetAwait(t *testing.T, done <-chan sqlBudgetAttemptResult) sqlBudgetAttemptResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(3 * time.Second):
		t.Fatal("bounded reservation did not finish")
		return sqlBudgetAttemptResult{}
	}
}
func sqlBudgetActorWithExpiry(t *testing.T, f sqlBudgetFixture, expiry time.Time) BudgetActor {
	t.Helper()
	next := f.budgetActorHTTPFixture
	next.identity.SID = uuid.New()
	next.identity.AccessExpiresAt = expiry
	tx := sqlBudgetTx(t, f.db)
	barrier, err := loginfamily.AcquireWriterBarrierTx(t.Context(), tx)
	require.NoError(t, err)
	users, err := loginfamily.LockUsersTx(t.Context(), tx, barrier, []uuid.UUID{f.identity.UserID}, loginfamily.UserNoKeyUpdate)
	require.NoError(t, err)
	user, err := users.ForUser(f.identity.UserID)
	require.NoError(t, err)
	family, err := loginfamily.GuardAfterUserLockTx(t.Context(), tx, user, f.identity, loginfamily.FamilyNoKeyUpdate)
	require.NoError(t, err)
	require.NoError(t, loginfamily.BindSIDTx(t.Context(), tx, family, next.identity.SID, expiry))
	require.NoError(t, tx.Commit())
	return sqlBudgetCaptureActor(t, next)
}
