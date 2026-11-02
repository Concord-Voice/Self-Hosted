package stepup

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The writer owns the canonical barrier and user lock before reserve starts.
// Observing PostgreSQL's lock wait proves that reserve reaches the contested
// authority read before the writer commits a different authority state.
func TestSQLBudgetReserveDeniesAuthorityCommittedDuringUserLockWait(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason loginfamily.ErrorReason
	}{
		{"epoch changed", loginfamily.ReasonEpochMismatch},
		{"family revoked", loginfamily.ReasonFamilyTerminal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSQLBudgetFixture(t, "")
			pool := sqlBudgetOtherPool(t)
			pid := sqlBudgetPID(t, pool)
			writer := sqlBudgetTx(t, f.db)
			barrier, err := loginfamily.AcquireWriterBarrierTx(t.Context(), writer)
			require.NoError(t, err)
			locked, err := loginfamily.LockUsersTx(t.Context(), writer, barrier, []uuid.UUID{f.identity.UserID}, loginfamily.UserNoKeyUpdate)
			require.NoError(t, err)
			user, err := locked.ForUser(f.identity.UserID)
			require.NoError(t, err)
			if tc.name == "epoch changed" {
				_, err = writer.ExecContext(t.Context(), `UPDATE users SET credential_epoch=$2 WHERE id=$1`, f.identity.UserID, strings.Repeat("d", 32))
			} else {
				var targets loginfamily.Targets
				targets, err = loginfamily.ResolveTargetSIDTx(t.Context(), writer, user, f.identity.SID)
				if err == nil {
					_, err = loginfamily.RevokeFamiliesTx(t.Context(), writer, targets, loginfamily.AuthCause{Scope: loginfamily.ScopeSession, Reason: loginfamily.ReasonExplicitRevoke})
				}
			}
			require.NoError(t, err)
			done := sqlBudgetReserveAsync(t.Context(), &sqlBudgetStore{db: pool}, f.actor)
			sqlBudgetWaitForLock(t, f.db, pid)
			require.NoError(t, writer.Commit())
			result := sqlBudgetAwait(t, done)
			require.NotNil(t, result.failure)
			assert.Equal(t, budgetFailureAuthorityDenied, result.failure.kind)
			assert.Equal(t, tc.reason, result.failure.authorityReason)
			assert.Zero(t, result.failure.retryAfterSeconds)
			assert.Equal(t, budgetReservation{}, result.reservation)
			assert.Zero(t, sqlBudgetCount(t, f.db))
		})
	}
}

func TestSQLBudgetExhaustionRetryAdviceCeilsDatabaseRemainingTime(t *testing.T) {
	f := newSQLBudgetFixture(t, "")
	receipts := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	sqlBudgetSeed(t, f, "123.5 seconds", receipts)
	row := sqlBudgetRead(t, f.db, f.actor)
	store := &sqlBudgetStore{db: f.db}
	// A bracket crossing an integer-second edge is inconclusive. Repeat the
	// read-only denial until both database observations imply the same ceil.
	for attempt := 0; attempt < 6; attempt++ {
		before := sqlBudgetNow(t, f.db)
		reservation, failure := store.reserve(t.Context(), f.actor)
		after := sqlBudgetNow(t, f.db)
		require.NotNil(t, failure)
		require.Equal(t, budgetFailureExhausted, failure.kind)
		require.Equal(t, budgetReservation{}, reservation)
		lower := int((row.expires.Sub(after) + time.Second - 1) / time.Second)
		upper := int((row.expires.Sub(before) + time.Second - 1) / time.Second)
		if lower == upper && lower > 1 && lower < 900 {
			assert.Equal(t, lower, failure.retryAfterSeconds, "ceil of the remaining database interval")
			assert.Equal(t, row, sqlBudgetRead(t, f.db, f.actor))
			return
		}
	}
	t.Fatal("database-time bracket never isolated a whole-second retry bucket")
}

func TestSQLBudgetNearExpiryRetryAdviceIsOneSecond(t *testing.T) {
	f := newSQLBudgetFixture(t, "")
	receipts := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	sqlBudgetSeed(t, f, "800 milliseconds", receipts)
	row := sqlBudgetRead(t, f.db, f.actor)
	before := sqlBudgetNow(t, f.db)
	require.True(t, row.expires.After(before))
	reservation, failure := (&sqlBudgetStore{db: f.db}).reserve(t.Context(), f.actor)
	after := sqlBudgetNow(t, f.db)
	require.True(t, row.expires.After(after), "the full window must still be live")
	require.NotNil(t, failure)
	assert.Equal(t, budgetFailureExhausted, failure.kind)
	assert.Equal(t, 1, failure.retryAfterSeconds)
	assert.Equal(t, budgetReservation{}, reservation)
	assert.Equal(t, row, sqlBudgetRead(t, f.db, f.actor))
}

func TestSQLBudgetPoolAcquisitionUsesInternalDeadlineForBothMethods(t *testing.T) {
	f := newSQLBudgetFixture(t, "")
	initial, failure := (&sqlBudgetStore{db: f.db}).reserve(t.Context(), f.actor)
	require.Nil(t, failure)
	original := sqlBudgetRead(t, f.db, f.actor)
	for _, name := range []string{"reserve", "refund"} {
		t.Run(name, func(t *testing.T) {
			pool := sqlBudgetOtherPool(t)
			holder, err := pool.Conn(t.Context())
			require.NoError(t, err)
			defer func() { assert.NoError(t, holder.Close()) }()
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
			defer cancel()
			done := make(chan *budgetFailure, 1)
			started := time.Now()
			go func() {
				store := &sqlBudgetStore{db: pool}
				if name == "reserve" {
					reservation, failure := store.reserve(ctx, f.actor)
					if reservation != (budgetReservation{}) {
						done <- &budgetFailure{kind: budgetFailureContract}
						return
					}
					done <- failure
					return
				}
				done <- store.refundVerifiedCommitted(ctx, initial)
			}()
			select {
			case failure := <-done:
				require.NotNil(t, failure)
				assert.Equal(t, budgetFailureUnavailable, failure.kind)
				assert.Equal(t, 5, failure.retryAfterSeconds)
				assert.Empty(t, failure.authorityReason)
				assert.Less(t, time.Since(started), 2700*time.Millisecond)
			case <-time.After(2700 * time.Millisecond):
				cancel()
				<-done
				t.Fatal("operation waited beyond the internal pool-acquisition deadline")
			}
			assert.Equal(t, original, sqlBudgetRead(t, f.db, f.actor))
		})
	}
}

func TestSQLBudgetRollbackResultFailureClosesExhaustionAndInertRefund(t *testing.T) {
	f := newSQLBudgetFixture(t, "")
	store := &sqlBudgetStore{db: f.db}
	reservation, failure := store.reserve(t.Context(), f.actor)
	require.Nil(t, failure)
	other := budgetReservation{userID: reservation.userID, familyID: reservation.familyID, windowID: reservation.windowID, expiresAt: reservation.expiresAt, receiptID: uuid.New()}
	row := sqlBudgetRead(t, f.db, f.actor)
	pool, fault := sqlBudgetFaultPool(t)
	fault.loseRollback.Store(true)
	failure = (&sqlBudgetStore{db: pool}).refundVerifiedCommitted(t.Context(), other)
	require.NotNil(t, failure)
	assert.Equal(t, budgetFailureUnavailable, failure.kind)
	assert.Equal(t, 5, failure.retryAfterSeconds)
	assert.Equal(t, int32(1), fault.rollbacks.Load())
	assert.Equal(t, int32(0), fault.commits.Load())
	assert.Equal(t, row, sqlBudgetRead(t, f.db, f.actor))

	full := []uuid.UUID{reservation.receiptID, uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	_, err := f.db.ExecContext(t.Context(), `UPDATE step_up_attempt_budgets SET charged_receipts=ARRAY[$3::uuid,$4::uuid,$5::uuid,$6::uuid,$7::uuid] WHERE user_id=$1 AND family_id=$2`, f.identity.UserID, f.familyID, full[0], full[1], full[2], full[3], full[4])
	require.NoError(t, err)
	fullRow := sqlBudgetRead(t, f.db, f.actor)
	fault.loseRollback.Store(true)
	denied, failure := (&sqlBudgetStore{db: pool}).reserve(t.Context(), f.actor)
	require.NotNil(t, failure)
	assert.Equal(t, budgetFailureUnavailable, failure.kind)
	assert.Equal(t, 5, failure.retryAfterSeconds)
	assert.Equal(t, budgetReservation{}, denied)
	assert.Equal(t, int32(2), fault.rollbacks.Load())
	assert.Equal(t, int32(0), fault.commits.Load())
	assert.Equal(t, fullRow, sqlBudgetRead(t, f.db, f.actor))
}
