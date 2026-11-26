package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"flag"
	"os"
	"testing"
	"time"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReservationBackfillCompletesVerifiedBaseline(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	messageID, dmMessageID := insertReservationFixture(t, db, uuid.New(), uuid.New())
	// SetupTestDB is already at migration 000206, so remove these fixture
	// reservations to model rows that existed before the schema migration.
	_, err := testdb.ExecReplica(db, `DELETE FROM message_id_reservations WHERE message_id IN ($1, $2)`, messageID, dmMessageID)
	require.NoError(t, err)

	require.NoError(t, (&reservationBackfill{db: db, batchSize: 1}).run(context.Background()))

	var complete bool
	require.NoError(t, db.QueryRow(`
		SELECT backfill_complete FROM message_id_reservation_rollout WHERE singleton = TRUE`).Scan(&complete))
	assert.True(t, complete)

	var reserved int
	require.NoError(t, db.QueryRow(`
		SELECT count(*) FROM message_id_reservations WHERE message_id IN ($1, $2)`, messageID, dmMessageID).Scan(&reserved))
	assert.Equal(t, 2, reserved)

	var deleteCaptureTriggers int
	require.NoError(t, db.QueryRow(`
		SELECT count(*)
		FROM pg_trigger
		WHERE tgname = 'message_id_reservations_before_delete'
			AND tgrelid IN ('messages'::regclass, 'dm_messages'::regclass)
			AND NOT tgisinternal`).Scan(&deleteCaptureTriggers))
	assert.Zero(t, deleteCaptureTriggers, "verified completion removes temporary delete capture")
}

func TestReservationBackfillPreservesCommittedGateWhenDeleteCaptureCleanupTimesOut(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	messageID, dmMessageID := insertReservationFixture(t, db, uuid.New(), uuid.New())
	_, err := testdb.ExecReplica(db, `DELETE FROM message_id_reservations WHERE message_id IN ($1, $2)`, messageID, dmMessageID)
	require.NoError(t, err)

	locker, err := db.Conn(context.Background())
	require.NoError(t, err)
	defer func() {
		_, rollbackErr := locker.ExecContext(context.Background(), `ROLLBACK`)
		require.NoError(t, rollbackErr)
		require.NoError(t, locker.Close())
	}()
	_, err = locker.ExecContext(context.Background(), `BEGIN; LOCK TABLE messages IN SHARE ROW EXCLUSIVE MODE`)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	err = (&reservationBackfill{db: db, batchSize: 1}).run(ctx)
	require.ErrorContains(t, err, "remove temporary message ID delete capture")

	var complete bool
	require.NoError(t, db.QueryRow(`
		SELECT backfill_complete FROM message_id_reservation_rollout WHERE singleton = TRUE`).Scan(&complete))
	assert.True(t, complete, "a cleanup lock timeout must not roll back the verified baseline")

	_, err = locker.ExecContext(context.Background(), `ROLLBACK`)
	require.NoError(t, err)

	require.NoError(t, (&reservationBackfill{db: db, batchSize: 1}).run(context.Background()))
	var deleteCaptureTriggers int
	require.NoError(t, db.QueryRow(`
		SELECT count(*)
		FROM pg_trigger
		WHERE tgname = 'message_id_reservations_before_delete'
			AND tgrelid IN ('messages'::regclass, 'dm_messages'::regclass)
			AND NOT tgisinternal`).Scan(&deleteCaptureTriggers))
	assert.Zero(t, deleteCaptureTriggers, "a rerun removes the retained temporary delete capture")
}

func TestReservationBackfillRejectsPreexistingCrossTableCollision(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	collidingID := uuid.New()
	insertReservationFixture(t, db, collidingID, collidingID)

	err := (&reservationBackfill{db: db, batchSize: 1}).run(context.Background())
	require.ErrorContains(t, err, "cross-table collision")

	var complete bool
	require.NoError(t, db.QueryRow(`
		SELECT backfill_complete FROM message_id_reservation_rollout WHERE singleton = TRUE`).Scan(&complete))
	assert.False(t, complete, "collision leaves the activation gate closed")
}

func TestReservationBackfillRevalidatesCompletedBaseline(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	insertReservationFixture(t, db, uuid.New(), uuid.New())
	backfill := &reservationBackfill{db: db, batchSize: 1}
	require.NoError(t, backfill.run(context.Background()))
	require.NoError(t, backfill.run(context.Background()))
}

func TestReservationBackfillRejectsIncompleteOrMissingState(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	backfill := &reservationBackfill{db: db, batchSize: 1}
	messageID, _ := insertReservationFixture(t, db, uuid.New(), uuid.New())
	_, err := testdb.ExecReplica(db, `DELETE FROM message_id_reservations WHERE message_id = $1`, messageID)
	require.NoError(t, err)
	require.ErrorContains(t, backfill.verifyBaseline(context.Background(), db), "anti-join is incomplete")

	_, err = db.Exec(`DELETE FROM message_id_reservation_rollout WHERE singleton = TRUE`)
	require.NoError(t, err)
	err = backfill.run(context.Background())
	require.ErrorContains(t, err, "rollout state is missing")

	_, _, err = reserveBatch(context.Background(), db, "unknown", nil, 1)
	require.ErrorContains(t, err, "unsupported message ID backfill table")
}

func TestReservationBackfillFailsClosedWhenCompletedBaselineIsIncomplete(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	messageID, _ := insertReservationFixture(t, db, uuid.New(), uuid.New())
	_, err := testdb.ExecReplica(db, `DELETE FROM message_id_reservations WHERE message_id = $1`, messageID)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE message_id_reservation_rollout SET backfill_complete = TRUE WHERE singleton = TRUE`)
	require.NoError(t, err)

	err = (&reservationBackfill{db: db, batchSize: 1}).run(context.Background())
	require.ErrorContains(t, err, "anti-join is incomplete")
}

func TestReservationBackfillFailsClosedForMissingReservationTable(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	_, err := db.Exec(`
		CREATE SCHEMA s177_missing_reservations;
		CREATE TABLE s177_missing_reservations.message_id_reservation_rollout (
			singleton BOOLEAN PRIMARY KEY,
			backfill_complete BOOLEAN NOT NULL
		);
		INSERT INTO s177_missing_reservations.message_id_reservation_rollout (singleton, backfill_complete)
		VALUES (TRUE, FALSE);
		CREATE TABLE s177_missing_reservations.messages (id UUID PRIMARY KEY);
		CREATE TABLE s177_missing_reservations.dm_messages (id UUID PRIMARY KEY)`)
	require.NoError(t, err)
	defer func() {
		_, dropErr := db.Exec(`DROP SCHEMA s177_missing_reservations CASCADE`)
		require.NoError(t, dropErr)
	}()

	backfillDB, err := sql.Open("postgres", testdb.DatabaseURL()+"&search_path=s177_missing_reservations")
	require.NoError(t, err)
	defer func() {
		require.NoError(t, backfillDB.Close())
	}()
	err = (&reservationBackfill{db: backfillDB, batchSize: 1}).run(context.Background())
	require.ErrorContains(t, err, "reserve messages batch")
}

func TestReservationBackfillFailsClosedWhenCompletionCannotPersist(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	_, err := db.Exec(`
		CREATE SCHEMA s177_completion_rejected;
		CREATE TABLE s177_completion_rejected.message_id_reservation_rollout (
			singleton BOOLEAN PRIMARY KEY,
			backfill_complete BOOLEAN NOT NULL CHECK (backfill_complete = FALSE)
		);
		INSERT INTO s177_completion_rejected.message_id_reservation_rollout (singleton, backfill_complete)
		VALUES (TRUE, FALSE);
		CREATE TABLE s177_completion_rejected.message_id_reservations (message_id UUID PRIMARY KEY);
		CREATE TABLE s177_completion_rejected.messages (id UUID PRIMARY KEY);
		CREATE TABLE s177_completion_rejected.dm_messages (id UUID PRIMARY KEY)`)
	require.NoError(t, err)
	defer func() {
		_, dropErr := db.Exec(`DROP SCHEMA s177_completion_rejected CASCADE`)
		require.NoError(t, dropErr)
	}()

	backfillDB, err := sql.Open("postgres", testdb.DatabaseURL()+"&search_path=s177_completion_rejected")
	require.NoError(t, err)
	defer func() {
		require.NoError(t, backfillDB.Close())
	}()
	err = (&reservationBackfill{db: backfillDB, batchSize: 1}).run(context.Background())
	require.ErrorContains(t, err, "mark message ID reservation baseline complete")
}

func TestReservationBackfillDatabaseErrorsFailClosed(t *testing.T) {
	closedDB, err := sql.Open("postgres", testdb.DatabaseURL())
	require.NoError(t, err)
	require.NoError(t, closedDB.Close())

	backfill := &reservationBackfill{db: closedDB, batchSize: 1}
	ctx := context.Background()
	require.ErrorContains(t, backfill.run(ctx), "acquire dedicated database connection")
	_, err = backfill.backfillComplete(ctx, closedDB)
	require.ErrorContains(t, err, "read message ID reservation rollout state")
	require.ErrorContains(t, backfill.verifyBaseline(ctx, closedDB), "check message ID cross-table collisions")
	require.ErrorContains(t, backfill.verifyNoCrossTableCollision(ctx, closedDB), "check message ID cross-table collisions")
	_, _, err = reserveBatch(ctx, closedDB, "messages", nil, 1)
	require.ErrorContains(t, err, "reserve messages batch")
	require.ErrorContains(t, backfill.backfillTable(ctx, closedDB, "messages"), "reserve messages batch")

	openDB, err := sql.Open("postgres", testdb.DatabaseURL())
	require.NoError(t, err)
	conn, err := openDB.Conn(ctx)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	require.ErrorContains(t, backfill.removeDeleteCapture(ctx, conn), "begin delete-capture cleanup transaction")
	require.NoError(t, openDB.Close())
}

func TestRemoveDeleteCaptureFailsClosedForCancelledContext(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	conn, err := db.Conn(context.Background())
	require.NoError(t, err)
	defer func() {
		closeErr := conn.Close()
		if closeErr != nil && !errors.Is(closeErr, sql.ErrConnDone) {
			t.Errorf("close canceled-context connection: %v", closeErr)
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = (&reservationBackfill{}).removeDeleteCapture(ctx, conn)
	require.ErrorIs(t, err, context.Canceled)
}

func TestRemoveDeleteCaptureFailsClosedWhenTriggerLockCannotBeAcquired(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	locker, err := db.Conn(context.Background())
	require.NoError(t, err)
	defer func() {
		_, rollbackErr := locker.ExecContext(context.Background(), `ROLLBACK`)
		require.NoError(t, rollbackErr)
		require.NoError(t, locker.Close())
	}()
	_, err = locker.ExecContext(context.Background(), `BEGIN; LOCK TABLE messages IN SHARE ROW EXCLUSIVE MODE`)
	require.NoError(t, err)

	conn, err := db.Conn(context.Background())
	require.NoError(t, err)
	defer func() {
		closeErr := conn.Close()
		if closeErr != nil && !errors.Is(closeErr, sql.ErrConnDone) {
			t.Errorf("close lock-timeout connection: %v", closeErr)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	err = (&reservationBackfill{}).removeDeleteCapture(ctx, conn)
	require.ErrorContains(t, err, "remove temporary message ID delete capture")
}

func TestDropDeleteCaptureExecutesBothStatementsAndStopsOnFailure(t *testing.T) {
	ctx := context.Background()
	success := &execContextStub{}
	require.NoError(t, dropDeleteCapture(ctx, success))
	require.Equal(t, []string{
		`SET LOCAL lock_timeout = '5s'`,
		`DROP TRIGGER IF EXISTS message_id_reservations_before_delete ON messages`,
		`DROP TRIGGER IF EXISTS message_id_reservations_before_delete ON dm_messages`,
	}, success.statements)

	timeoutFailure := &execContextStub{failAt: 1}
	require.ErrorContains(t, dropDeleteCapture(ctx, timeoutFailure), "set message ID delete-capture lock timeout")
	require.Len(t, timeoutFailure.statements, 1)

	dropFailure := &execContextStub{failAt: 2}
	require.ErrorContains(t, dropDeleteCapture(ctx, dropFailure), "remove temporary message ID delete capture")
	require.Len(t, dropFailure.statements, 2)
}

func TestMainVerifiesCompletedBaseline(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	require.NoError(t, (&reservationBackfill{db: db, batchSize: 1}).run(context.Background()))
	t.Setenv("DATABASE_URL", testdb.DatabaseURL())
	originalArgs, originalFlags := os.Args, flag.CommandLine
	os.Args = []string{originalArgs[0]}
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	t.Cleanup(func() {
		os.Args = originalArgs
		flag.CommandLine = originalFlags
	})

	main()
}

func TestBackfillContextUsesNoDeadlineUnlessConfigured(t *testing.T) {
	unbounded, cancel := backfillContext(0)
	defer cancel()
	_, hasDeadline := unbounded.Deadline()
	assert.False(t, hasDeadline)

	bounded, boundedCancel := backfillContext(time.Minute)
	defer boundedCancel()
	deadline, hasDeadline := bounded.Deadline()
	assert.True(t, hasDeadline)
	assert.WithinDuration(t, time.Now().Add(time.Minute), deadline, time.Second)
}

type execContextStub struct {
	statements []string
	failAt     int
}

func (s *execContextStub) ExecContext(_ context.Context, statement string, _ ...any) (sql.Result, error) {
	s.statements = append(s.statements, statement)
	if s.failAt == len(s.statements) {
		return nil, errors.New("injected exec failure")
	}
	return driver.RowsAffected(0), nil
}

func insertReservationFixture(t *testing.T, db *sql.DB, messageID, dmMessageID uuid.UUID) (uuid.UUID, uuid.UUID) {
	t.Helper()
	userID := testhelpers.CreateUser(t, db)
	serverID := testhelpers.CreateServer(t, db, userID)
	channelID := uuid.New()
	_, err := db.Exec(`
		INSERT INTO channels (id, server_id, name, type)
		VALUES ($1, $2, 'message-id-reservation', 'text')`, channelID, serverID)
	require.NoError(t, err)

	conversationID := uuid.New()
	_, err = db.Exec(`INSERT INTO dm_conversations (id, created_by) VALUES ($1, $2)`, conversationID, userID)
	require.NoError(t, err)

	_, err = db.Exec(`
		INSERT INTO messages (id, channel_id, user_id, content, key_version)
		VALUES ($1, $2, $3, 'fixture', 1)`, messageID, channelID, userID)
	require.NoError(t, err)
	if messageID == dmMessageID {
		// A historical collision can predate the reservation migration. Remove
		// the fixture's automatic insert reservation so the second legacy row
		// models that state and the backfill command can detect it.
		_, err = testdb.ExecReplica(db, `DELETE FROM message_id_reservations WHERE message_id = $1`, messageID)
		require.NoError(t, err)
	}
	insertDM := func() error {
		_, insertErr := db.Exec(`
		INSERT INTO dm_messages (id, conversation_id, user_id, content, key_version)
		VALUES ($1, $2, $3, 'fixture', 1)`, dmMessageID, conversationID, userID)
		return insertErr
	}
	if messageID == dmMessageID {
		// A scoped trigger bypass models a historical collision that predates
		// 000206; it is immediately restored for the rest of this test DB.
		tx, beginErr := db.Begin()
		require.NoError(t, beginErr)
		defer func() {
			rollbackErr := tx.Rollback()
			if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
				t.Errorf("rollback historical collision fixture: %v", rollbackErr)
			}
		}()
		_, err = tx.Exec(`SET LOCAL session_replication_role = replica`)
		require.NoError(t, err)
		_, err = tx.Exec(`ALTER TABLE dm_messages DISABLE TRIGGER message_id_reservations_before_insert`)
		require.NoError(t, err)
		_, err = tx.Exec(`
			INSERT INTO dm_messages (id, conversation_id, user_id, content, key_version)
			VALUES ($1, $2, $3, 'fixture', 1)`, dmMessageID, conversationID, userID)
		require.NoError(t, err)
		_, err = tx.Exec(`ALTER TABLE dm_messages ENABLE ALWAYS TRIGGER message_id_reservations_before_insert`)
		require.NoError(t, err)
		err = tx.Commit()
	} else {
		err = insertDM()
	}
	require.NoError(t, err)
	return messageID, dmMessageID
}
