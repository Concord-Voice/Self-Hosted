package stepup

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

var errSQLBudgetState = errors.New("step-up budget state invalid")

type sqlBudgetRecord struct {
	userID, familyID, windowID uuid.UUID
	startedAt, expiresAt       time.Time
	receipts                   []uuid.UUID
}

func validSQLBudgetActor(actor BudgetActor) bool {
	identity := actor.identity
	if identity.UserID == uuid.Nil || identity.SID == uuid.Nil || actor.expectedFamilyID == uuid.Nil || identity.AccessExpiresAt.IsZero() {
		return false
	}
	if identity.CredentialEpoch == "" {
		return true
	}
	if len(identity.CredentialEpoch) != 32 {
		return false
	}
	for _, c := range identity.CredentialEpoch {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func validSQLBudgetReservation(r budgetReservation) bool {
	return r.userID != uuid.Nil && r.familyID != uuid.Nil && r.windowID != uuid.Nil && r.receiptID != uuid.Nil && !r.expiresAt.IsZero()
}

func seedSQLBudgetRecord(ctx context.Context, tx *sql.Tx, actor BudgetActor, seedID uuid.UUID) (bool, error) {
	var insertedID uuid.UUID
	// This empty expired seed never commits. The locked post-wait clock sample
	// starts the actual first window, just as it replaces an expired old one.
	err := tx.QueryRowContext(ctx, `WITH moment AS MATERIALIZED (SELECT clock_timestamp() AS sampled_at)
 INSERT INTO step_up_attempt_budgets(user_id,family_id,window_id,window_started_at,expires_at,charged_receipts)
 SELECT $1,$2,$3,sampled_at-interval '900 seconds',sampled_at,'{}'::uuid[] FROM moment
 ON CONFLICT(user_id,family_id) DO NOTHING RETURNING window_id`,
		actor.identity.UserID, actor.expectedFamilyID, seedID).Scan(&insertedID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if insertedID != seedID {
		return false, errSQLBudgetState
	}
	return true, nil
}

func readSQLBudgetRecord(ctx context.Context, tx *sql.Tx, userID, familyID uuid.UUID) (sqlBudgetRecord, error) {
	var row sqlBudgetRecord
	var count, dimensions, lower sql.NullInt64
	var values pq.StringArray
	var finite bool
	// Do not allocate an unbounded receipt list if malformed state is encountered.
	// Dimension/bound metadata is checked independently of the text-array parser.
	err := tx.QueryRowContext(ctx, `SELECT user_id,family_id,window_id,window_started_at,expires_at,
 isfinite(window_started_at) AND isfinite(expires_at),
 cardinality(charged_receipts),array_ndims(charged_receipts),array_lower(charged_receipts,1),
 CASE WHEN cardinality(charged_receipts) BETWEEN 0 AND 5 THEN charged_receipts ELSE NULL END
 FROM step_up_attempt_budgets WHERE user_id=$1 AND family_id=$2 FOR UPDATE`, userID, familyID).
		Scan(&row.userID, &row.familyID, &row.windowID, &row.startedAt, &row.expiresAt, &finite, &count, &dimensions, &lower, &values)
	if err != nil {
		return sqlBudgetRecord{}, err
	}
	if !validSQLBudgetRecord(row, userID, familyID, finite) || !validSQLBudgetArray(count, dimensions, lower, values) {
		return sqlBudgetRecord{}, errSQLBudgetState
	}
	row.receipts, err = parseSQLBudgetReceipts(values)
	if err != nil {
		return sqlBudgetRecord{}, err
	}
	return row, nil
}

func validSQLBudgetRecord(row sqlBudgetRecord, userID, familyID uuid.UUID, finite bool) bool {
	return finite && row.userID == userID && row.familyID == familyID && row.windowID != uuid.Nil &&
		!row.startedAt.IsZero() && !row.expiresAt.IsZero() && row.startedAt.Add(sqlBudgetWindow).Equal(row.expiresAt)
}

func parseSQLBudgetReceipts(values pq.StringArray) ([]uuid.UUID, error) {
	if len(values) == 0 {
		return nil, nil
	}
	receipts := make([]uuid.UUID, 0, len(values))
	seen := make(map[uuid.UUID]struct{}, len(values))
	for _, value := range values {
		id, parseErr := uuid.Parse(value)
		if parseErr != nil || id == uuid.Nil {
			return nil, errSQLBudgetState
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, errSQLBudgetState
		}
		seen[id] = struct{}{}
		receipts = append(receipts, id)
	}
	return receipts, nil
}

func validSQLBudgetArray(count, dimensions, lower sql.NullInt64, values pq.StringArray) bool {
	if !count.Valid || count.Int64 < 0 || count.Int64 > sqlBudgetLimit || count.Int64 != int64(len(values)) {
		return false
	}
	if count.Int64 == 0 {
		return !dimensions.Valid && !lower.Valid
	}
	return dimensions.Valid && dimensions.Int64 == 1 && lower.Valid && lower.Int64 == 1
}
