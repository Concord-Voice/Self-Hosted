// Command message-id-reservation-backfill establishes the dormant message-ID
// reservation baseline introduced by migration 000206. It does not enable a
// message writer; a later writer must independently require the completed gate.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	// Register the PostgreSQL driver used by sql.Open.
	_ "github.com/lib/pq"
)

const advisoryLockName = "message-id-reservation-backfill-v1"

type reservationBackfill struct {
	db        *sql.DB
	batchSize int
}

func main() {
	databaseURL := flag.String("database-url", os.Getenv("DATABASE_URL"), "PostgreSQL connection URL")
	batchSize := flag.Int("batch-size", 1_000, "Maximum IDs to reserve per keyset batch")
	runTimeout := flag.Duration("timeout", 0, "Maximum total runtime; 0 disables the total deadline")
	flag.Parse()

	if *databaseURL == "" {
		log.Fatal("database-url is required")
	}
	if *batchSize < 1 {
		log.Fatal("batch-size must be positive")
	}
	if *runTimeout < 0 {
		log.Fatal("timeout must not be negative")
	}

	db, err := sql.Open("postgres", *databaseURL)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			log.Printf("close database: %v", closeErr)
		}
	}()

	ctx, cancel := backfillContext(*runTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("ping database: %v", err)
	}

	if err := (&reservationBackfill{db: db, batchSize: *batchSize}).run(ctx); err != nil {
		log.Fatalf("message ID reservation baseline failed: %v", err)
	}
	log.Println("message ID reservation baseline verified; no message-v1 writer was enabled")
}

func backfillContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout == 0 {
		return context.WithCancel(context.Background())
	}
	return context.WithTimeout(context.Background(), timeout)
}

func (b *reservationBackfill) run(ctx context.Context) error {
	conn, err := b.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire dedicated database connection: %w", err)
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			log.Printf("close dedicated database connection: %v", closeErr)
		}
	}()

	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, advisoryLockName); err != nil {
		return fmt.Errorf("acquire message ID reservation lock: %w", err)
	}
	defer func() {
		if _, unlockErr := conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, advisoryLockName); unlockErr != nil {
			log.Printf("release message ID reservation lock: %v", unlockErr)
		}
	}()

	complete, err := b.backfillComplete(ctx, conn)
	if err != nil {
		return err
	}
	if complete {
		if err := b.verifyBaseline(ctx, conn); err != nil {
			return err
		}
		if err := b.removeDeleteCapture(ctx, conn); err != nil {
			return err
		}
		return nil
	}

	if err := b.verifyNoCrossTableCollision(ctx, conn); err != nil {
		return err
	}
	for _, source := range []string{"messages", "dm_messages"} {
		if err := b.backfillTable(ctx, conn, source); err != nil {
			return err
		}
	}
	if err := b.verifyBaseline(ctx, conn); err != nil {
		return err
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin baseline completion transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			log.Printf("rollback baseline completion transaction: %v", rollbackErr)
		}
	}()

	result, err := tx.ExecContext(ctx, `
		UPDATE message_id_reservation_rollout
		SET backfill_complete = TRUE
		WHERE singleton = TRUE AND backfill_complete = FALSE`)
	if err != nil {
		return fmt.Errorf("mark message ID reservation baseline complete: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read message ID reservation completion result: %w", err)
	}
	if updated != 1 {
		return fmt.Errorf("message ID reservation rollout state changed unexpectedly")
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit message ID reservation baseline: %w", err)
	}
	// The gate is durable once its verification and update commit. Trigger
	// cleanup is retried separately: a short lock timeout must not roll this
	// committed baseline back and force another full keyset scan.
	return b.removeDeleteCapture(ctx, conn)
}

func (b *reservationBackfill) backfillComplete(ctx context.Context, q queryRower) (bool, error) {
	var complete bool
	err := q.QueryRowContext(ctx, `
		SELECT backfill_complete
		FROM message_id_reservation_rollout
		WHERE singleton = TRUE`).Scan(&complete)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("message ID reservation rollout state is missing")
	}
	if err != nil {
		return false, fmt.Errorf("read message ID reservation rollout state: %w", err)
	}
	return complete, nil
}

func (b *reservationBackfill) backfillTable(ctx context.Context, q queryRower, table string) error {
	var cursor *string
	for {
		next, count, err := reserveBatch(ctx, q, table, cursor, b.batchSize)
		if err != nil {
			return err
		}
		if count == 0 {
			return nil
		}
		cursor = &next
	}
}

func reserveBatch(ctx context.Context, q queryRower, table string, cursor *string, batchSize int) (string, int, error) {
	var query string
	switch table {
	case "messages":
		query = reserveMessagesBatchSQL
	case "dm_messages":
		query = reserveDMMessagesBatchSQL
	default:
		return "", 0, fmt.Errorf("unsupported message ID backfill table %q", table)
	}

	var next sql.NullString
	var count int
	if err := q.QueryRowContext(ctx, query, cursor, batchSize).Scan(&next, &count); err != nil {
		return "", 0, fmt.Errorf("reserve %s batch: %w", table, err)
	}
	if count == 0 {
		return "", 0, nil
	}
	if !next.Valid {
		return "", 0, fmt.Errorf("reserve %s batch returned rows without a cursor", table)
	}
	return next.String, count, nil
}

func (b *reservationBackfill) verifyBaseline(ctx context.Context, q queryRower) error {
	if err := b.verifyNoCrossTableCollision(ctx, q); err != nil {
		return err
	}
	var missing bool
	err := q.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM messages AS message
			WHERE NOT EXISTS (
				SELECT 1 FROM message_id_reservations AS reservation
				WHERE reservation.message_id = message.id
			)
			UNION ALL
			SELECT 1
			FROM dm_messages AS message
			WHERE NOT EXISTS (
				SELECT 1 FROM message_id_reservations AS reservation
				WHERE reservation.message_id = message.id
			)
		)`).Scan(&missing)
	if err != nil {
		return fmt.Errorf("verify message ID reservation anti-joins: %w", err)
	}
	if missing {
		return fmt.Errorf("message ID reservation anti-join is incomplete")
	}
	return nil
}

func (b *reservationBackfill) verifyNoCrossTableCollision(ctx context.Context, q queryRower) error {
	var collision bool
	err := q.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM messages
			INNER JOIN dm_messages USING (id)
		)`).Scan(&collision)
	if err != nil {
		return fmt.Errorf("check message ID cross-table collisions: %w", err)
	}
	if collision {
		return fmt.Errorf("message ID cross-table collision detected; baseline remains inactive")
	}
	return nil
}

func (b *reservationBackfill) removeDeleteCapture(ctx context.Context, conn *sql.Conn) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete-capture cleanup transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			log.Printf("rollback delete-capture cleanup transaction: %v", rollbackErr)
		}
	}()
	if err := dropDeleteCapture(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete-capture cleanup: %w", err)
	}
	return nil
}

func dropDeleteCapture(ctx context.Context, q execContexter) error {
	if _, err := q.ExecContext(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return fmt.Errorf("set message ID delete-capture lock timeout: %w", err)
	}
	for _, statement := range []string{
		`DROP TRIGGER IF EXISTS message_id_reservations_before_delete ON messages`,
		`DROP TRIGGER IF EXISTS message_id_reservations_before_delete ON dm_messages`,
	} {
		if _, err := q.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("remove temporary message ID delete capture: %w", err)
		}
	}
	return nil
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type execContexter interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

const reserveMessagesBatchSQL = `
WITH candidates AS (
    SELECT id
    FROM messages
    WHERE ($1::uuid IS NULL OR id > $1::uuid)
    ORDER BY id
    LIMIT $2
), reserved AS (
    INSERT INTO message_id_reservations (message_id)
    SELECT id FROM candidates
    ON CONFLICT (message_id) DO NOTHING
)
SELECT
    (SELECT id::text FROM candidates ORDER BY id DESC LIMIT 1),
    (SELECT count(*) FROM candidates)`

const reserveDMMessagesBatchSQL = `
WITH candidates AS (
    SELECT id
    FROM dm_messages
    WHERE ($1::uuid IS NULL OR id > $1::uuid)
    ORDER BY id
    LIMIT $2
), reserved AS (
    INSERT INTO message_id_reservations (message_id)
    SELECT id FROM candidates
    ON CONFLICT (message_id) DO NOTHING
)
SELECT
    (SELECT id::text FROM candidates ORDER BY id DESC LIMIT 1),
    (SELECT count(*) FROM candidates)`
