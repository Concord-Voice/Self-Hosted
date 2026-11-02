package stepup

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
)

const (
	sqlBudgetLimit    = 5
	sqlBudgetWindow   = 15 * time.Minute
	sqlBudgetDeadline = 2 * time.Second
)

// sqlBudgetStore is dormant accounting groundwork. It has no production caller
// and grants no permission to perform proof. A future admission owner must check
// authoritative step-up lifecycle state under this transaction's early barrier.
// The borrowed pool is never closed or reconfigured here.
type sqlBudgetStore struct{ db *sql.DB }

type budgetReservation struct {
	userID, familyID, windowID, receiptID uuid.UUID
	expiresAt                             time.Time
}

type budgetFailureKind uint8

const (
	budgetFailureContract budgetFailureKind = iota + 1
	budgetFailureAuthorityDenied
	budgetFailureAuthorityUnavailable
	budgetFailureUnavailable
	budgetFailureExhausted
	budgetFailureSettlementUnknown
)

// Failures carry closed classifications only, never driver causes or identities.
// Nil is success; these private kinds are not a wire or persistence contract.
type budgetFailure struct {
	kind              budgetFailureKind
	retryAfterSeconds int
	authorityReason   loginfamily.ErrorReason
}

func (b *sqlBudgetStore) reserve(ctx context.Context, actor BudgetActor) (reservation budgetReservation, failure *budgetFailure) {
	if b == nil || b.db == nil || ctx == nil || !validSQLBudgetActor(actor) {
		return budgetReservation{}, &budgetFailure{kind: budgetFailureContract}
	}
	ctx, cancel := context.WithTimeout(ctx, sqlBudgetDeadline)
	defer cancel()
	tx, err := b.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return budgetReservation{}, sqlBudgetUnavailable()
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			reservation = budgetReservation{}
			failure = sqlBudgetCleanupFailure(failure)
		}
	}()
	if err = setSQLBudgetTimeouts(ctx, tx); err != nil {
		return budgetReservation{}, sqlBudgetUnavailable()
	}
	if failure = guardSQLBudgetActor(ctx, tx, actor); failure != nil {
		return budgetReservation{}, failure
	}
	reservation, failure = reserveSQLBudgetCharge(ctx, tx, actor)
	if failure != nil {
		return budgetReservation{}, failure
	}
	if err = tx.Commit(); err != nil {
		// The charge may have committed. No receipt, proof or application retry
		// follows an unconfirmed commit, including a context-cancellation error.
		return budgetReservation{}, sqlBudgetUnavailable()
	}
	return reservation, nil
}

// refundVerifiedCommitted may be called only after the actual effect owner has
// established full required proof AND definite complete effect commit. The store
// cannot manufacture that fact. It removes only this receipt, even if the actor's
// family is now terminal. It never reauthorizes or repeats the protected effect.
func (b *sqlBudgetStore) refundVerifiedCommitted(ctx context.Context, r budgetReservation) (failure *budgetFailure) {
	if b == nil || b.db == nil || ctx == nil || !validSQLBudgetReservation(r) {
		return &budgetFailure{kind: budgetFailureContract}
	}
	ctx, cancel := context.WithTimeout(ctx, sqlBudgetDeadline)
	defer cancel()
	tx, err := b.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return sqlBudgetUnavailable()
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			failure = sqlBudgetCleanupFailure(failure)
		}
	}()
	if err = setSQLBudgetTimeouts(ctx, tx); err != nil {
		return sqlBudgetUnavailable()
	}
	changed, failure := refundSQLBudgetReceipt(ctx, tx, r)
	if failure != nil || !changed {
		return failure
	}
	if err = tx.Commit(); err != nil {
		// This eligible receipt may already be gone. Do not claim that the charge
		// definitely remains, and never replay the committed protected effect.
		return &budgetFailure{kind: budgetFailureSettlementUnknown}
	}
	return nil
}

// refundSQLBudgetReceipt borrows the owner's transaction for statements only.
// A true result means one removal is pending, never that it has committed.
func refundSQLBudgetReceipt(ctx context.Context, tx *sql.Tx, r budgetReservation) (bool, *budgetFailure) {
	// Accounting-only family -> budget order. No users or rollout lock may be
	// acquired afterward, and no live-family capability is constructed here.
	var familyID uuid.UUID
	err := tx.QueryRowContext(ctx, `SELECT family_id FROM login_families
 WHERE family_id=$1 AND user_id=$2 FOR NO KEY UPDATE`, r.familyID, r.userID).Scan(&familyID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil || familyID != r.familyID {
		return false, sqlBudgetUnavailable()
	}
	row, err := readSQLBudgetRecord(ctx, tx, r.userID, r.familyID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, sqlBudgetUnavailable()
	}
	if row.windowID != r.windowID {
		return false, nil
	}
	if !row.expiresAt.Equal(r.expiresAt) {
		return false, &budgetFailure{kind: budgetFailureContract}
	}
	var now time.Time
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return false, sqlBudgetUnavailable()
	}
	if now.Before(row.startedAt) {
		return false, sqlBudgetUnavailable()
	}
	if !row.expiresAt.After(now) || !slices.Contains(row.receipts, r.receiptID) {
		return false, nil
	}
	var updatedWindow uuid.UUID
	err = tx.QueryRowContext(ctx, `UPDATE step_up_attempt_budgets
 SET charged_receipts=array_remove(charged_receipts,$4::uuid)
 WHERE user_id=$1 AND family_id=$2 AND window_id=$3 AND expires_at=$5
 AND expires_at>$6 AND $4::uuid=ANY(charged_receipts)
 RETURNING window_id`, r.userID, r.familyID, r.windowID, r.receiptID, r.expiresAt, now).Scan(&updatedWindow)
	if err != nil || updatedWindow != r.windowID {
		return false, sqlBudgetUnavailable()
	}
	return true, nil
}

func setSQLBudgetTimeouts(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `SELECT set_config('lock_timeout','1s',true), set_config('statement_timeout','2s',true)`)
	return err
}

func guardSQLBudgetActor(ctx context.Context, tx *sql.Tx, actor BudgetActor) *budgetFailure {
	barrier, err := loginfamily.AcquireWriterBarrierTx(ctx, tx)
	if err != nil {
		return sqlBudgetAuthorityFailure(err)
	}
	// The eventual production entrypoint must check the authoritative step-up
	// lifecycle here in THIS transaction. This private dormant core is not an
	// admission entrypoint, and intentionally provides no permissive gate stub.
	users, err := loginfamily.LockUsersTx(ctx, tx, barrier, []uuid.UUID{actor.identity.UserID}, loginfamily.UserShare)
	if err != nil {
		return sqlBudgetAuthorityFailure(err)
	}
	user, err := users.ForUser(actor.identity.UserID)
	if err != nil {
		return sqlBudgetAuthorityFailure(err)
	}
	family, err := loginfamily.GuardAfterUserLockTx(ctx, tx, user, actor.identity, loginfamily.FamilyShare)
	if err != nil {
		return sqlBudgetAuthorityFailure(err)
	}
	binding := family.Binding()
	if binding.UserID != actor.identity.UserID || binding.FamilyID != actor.expectedFamilyID || binding.CredentialEpoch != actor.identity.CredentialEpoch {
		return &budgetFailure{kind: budgetFailureContract}
	}
	return nil
}

// reserveSQLBudgetCharge runs only after the owner's canonical actor guard.
// The returned receipt is provisional until that same owner confirms Commit.
func reserveSQLBudgetCharge(ctx context.Context, tx *sql.Tx, actor BudgetActor) (budgetReservation, *budgetFailure) {
	seedID, err := uuid.NewRandom()
	if err != nil {
		return budgetReservation{}, sqlBudgetUnavailable()
	}
	receiptID, err := uuid.NewRandom()
	if err != nil {
		return budgetReservation{}, sqlBudgetUnavailable()
	}
	inserted, err := seedSQLBudgetRecord(ctx, tx, actor, seedID)
	if err != nil {
		return budgetReservation{}, sqlBudgetUnavailable()
	}
	// A separate READ COMMITTED statement sees a conflicting inserter's commit.
	row, err := readSQLBudgetRecord(ctx, tx, actor.identity.UserID, actor.expectedFamilyID)
	if err != nil {
		return budgetReservation{}, sqlBudgetUnavailable()
	}
	var now time.Time
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return budgetReservation{}, sqlBudgetUnavailable()
	}
	// Expiry can elapse during the budget-row wait. User/family locks still
	// stabilize the other facts checked by the canonical access guard.
	if !actor.identity.AccessExpiresAt.After(now) {
		return budgetReservation{}, &budgetFailure{
			kind: budgetFailureAuthorityDenied, authorityReason: loginfamily.ReasonAccessExpired,
		}
	}
	if now.Before(row.startedAt) || (inserted && now.Before(row.expiresAt)) {
		return budgetReservation{}, sqlBudgetUnavailable()
	}
	return chargeSQLBudgetRecord(ctx, tx, row, now, receiptID)
}

func chargeSQLBudgetRecord(ctx context.Context, tx *sql.Tx, row sqlBudgetRecord, now time.Time, receiptID uuid.UUID) (budgetReservation, *budgetFailure) {
	if slices.Contains(row.receipts, receiptID) {
		return budgetReservation{}, sqlBudgetUnavailable()
	}
	nextWindow, start, expiry := row.windowID, row.startedAt, row.expiresAt
	if !expiry.After(now) {
		var err error
		nextWindow, err = uuid.NewRandom()
		if err != nil || nextWindow == row.windowID {
			return budgetReservation{}, sqlBudgetUnavailable()
		}
		start, expiry = now, now.Add(sqlBudgetWindow)
	} else if len(row.receipts) == sqlBudgetLimit {
		seconds := int((expiry.Sub(now) + time.Second - 1) / time.Second)
		return budgetReservation{}, &budgetFailure{
			kind: budgetFailureExhausted, retryAfterSeconds: max(1, min(900, seconds)),
		}
	}
	var storedWindow, storedReceipt uuid.UUID
	var storedExpiry time.Time
	err := tx.QueryRowContext(ctx, `UPDATE step_up_attempt_budgets
 SET window_id=$4, window_started_at=$5, expires_at=$6,
 charged_receipts=CASE WHEN window_id=$4 THEN array_append(charged_receipts,$7::uuid) ELSE ARRAY[$7::uuid] END
 WHERE user_id=$1 AND family_id=$2 AND window_id=$3
 AND (expires_at<=$8 OR cardinality(charged_receipts)<5)
 RETURNING window_id,expires_at,charged_receipts[cardinality(charged_receipts)]`,
		row.userID, row.familyID, row.windowID, nextWindow, start, expiry, receiptID, now).
		Scan(&storedWindow, &storedExpiry, &storedReceipt)
	if err != nil || storedWindow != nextWindow || storedReceipt != receiptID || !storedExpiry.Equal(expiry) {
		return budgetReservation{}, sqlBudgetUnavailable()
	}
	return budgetReservation{
		userID: row.userID, familyID: row.familyID, windowID: storedWindow,
		receiptID: storedReceipt, expiresAt: storedExpiry,
	}, nil
}

func sqlBudgetUnavailable() *budgetFailure {
	return &budgetFailure{kind: budgetFailureUnavailable, retryAfterSeconds: 5}
}

func sqlBudgetAuthorityFailure(err error) *budgetFailure {
	failure := &budgetFailure{kind: budgetFailureContract}
	var familyError *loginfamily.Error
	if errors.As(err, &familyError) {
		failure.authorityReason = familyError.Reason()
	}
	switch {
	case errors.Is(err, loginfamily.ErrDenied):
		failure.kind = budgetFailureAuthorityDenied
	case errors.Is(err, loginfamily.ErrUnavailable):
		failure.kind = budgetFailureAuthorityUnavailable
	}
	return failure
}

// Cleanup failures cannot turn a refusal into success or obscure an unknown
// commit. Already-unavailable primary classifications retain their identity.
func sqlBudgetCleanupFailure(primary *budgetFailure) *budgetFailure {
	if primary != nil {
		switch primary.kind {
		case budgetFailureUnavailable, budgetFailureAuthorityUnavailable, budgetFailureSettlementUnknown:
			return primary
		}
	}
	return sqlBudgetUnavailable()
}
