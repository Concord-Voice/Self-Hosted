package stepup

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSQLBudgetMalformedInputsRequireNoDatabase(t *testing.T) {
	store := &sqlBudgetStore{}
	reservation, failure := store.reserve(t.Context(), BudgetActor{})
	require.NotNil(t, failure)
	assert.Equal(t, budgetFailureContract, failure.kind)
	assert.Equal(t, budgetReservation{}, reservation)
	failure = store.refundVerifiedCommitted(t.Context(), budgetReservation{})
	require.NotNil(t, failure)
	assert.Equal(t, budgetFailureContract, failure.kind)
}

func TestSQLBudgetFirstReservationUsesDatabaseWindow(t *testing.T) {
	for _, epoch := range []string{"", strings.Repeat("a", 32)} {
		t.Run("epoch-"+epoch, func(t *testing.T) {
			f := newSQLBudgetFixture(t, epoch)
			pool := sqlBudgetOtherPool(t)
			before := sqlBudgetNow(t, f.db)
			reservation, failure := (&sqlBudgetStore{db: pool}).reserve(t.Context(), f.actor)
			require.Nil(t, failure)
			assert.Zero(t, pool.Stats().InUse, "dedicated store pool releases its transaction before return")
			after := sqlBudgetNow(t, f.db)
			assert.Equal(t, f.identity.UserID, reservation.userID)
			assert.Equal(t, f.familyID, reservation.familyID)
			assert.NotEqual(t, uuid.Nil, reservation.receiptID)
			assert.NotEqual(t, uuid.Nil, reservation.windowID)
			row := sqlBudgetRead(t, f.db, f.actor)
			assert.Equal(t, []uuid.UUID{reservation.receiptID}, row.receipts)
			assert.Equal(t, reservation.windowID, row.window)
			assert.True(t, reservation.expiresAt.Equal(row.expires))
			assert.Equal(t, 15*time.Minute, row.expires.Sub(row.started))
			assert.False(t, row.started.Before(before))
			assert.False(t, row.started.After(after))
		})
	}
}

func TestSQLBudgetSixConcurrentChargesShareOneFamily(t *testing.T) {
	f := newSQLBudgetFixture(t, "")
	sibling := sqlBudgetNewFamily(t, f, true)
	other := sqlBudgetOtherPool(t)
	other.SetMaxOpenConns(3)
	other.SetMaxIdleConns(3)
	stores := []*sqlBudgetStore{{db: f.db}, {db: other}}
	actors := []BudgetActor{f.actor, sibling}
	type result struct {
		reservation budgetReservation
		failure     *budgetFailure
	}
	results := make(chan result, 6)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := 0; i < 6; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			<-start
			r, failure := stores[i%2].reserve(t.Context(), actors[i%2])
			results <- result{r, failure}
		}(i)
	}
	close(start)
	workers.Wait()
	close(results)
	var charged []uuid.UUID
	exhausted := 0
	for result := range results {
		if result.failure == nil {
			charged = append(charged, result.reservation.receiptID)
			continue
		}
		assert.Equal(t, budgetReservation{}, result.reservation)
		assert.Equal(t, budgetFailureExhausted, result.failure.kind)
		assert.GreaterOrEqual(t, result.failure.retryAfterSeconds, 1)
		assert.LessOrEqual(t, result.failure.retryAfterSeconds, 900)
		exhausted++
	}
	require.Len(t, charged, 5)
	assert.Len(t, mapSQLBudgetReceipts(charged), 5)
	assert.Equal(t, 1, exhausted)
	assert.ElementsMatch(t, charged, sqlBudgetRead(t, f.db, f.actor).receipts)
	separate := sqlBudgetNewFamily(t, f, false)
	for i := 0; i < 5; i++ {
		r, failure := stores[0].reserve(t.Context(), separate)
		require.Nil(t, failure)
		assert.Equal(t, separate.expectedFamilyID, r.familyID)
	}
	assert.Equal(t, 2, sqlBudgetCount(t, f.db))
}

func mapSQLBudgetReceipts(ids []uuid.UUID) map[uuid.UUID]struct{} {
	out := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out
}

func TestSQLBudgetRechecksGenuineActorAuthority(t *testing.T) {
	for _, name := range []string{"terminal family", "changed epoch", "wrong expected family", "signed expiry mismatch"} {
		t.Run(name, func(t *testing.T) {
			f := newSQLBudgetFixture(t, "")
			actor := f.actor
			expected := budgetFailureAuthorityDenied
			switch name {
			case "terminal family":
				f.revoke(t)
			case "changed epoch":
				_, err := f.db.ExecContext(t.Context(), `UPDATE users SET credential_epoch=$2 WHERE id=$1`, f.identity.UserID, strings.Repeat("b", 32))
				require.NoError(t, err)
			case "wrong expected family":
				actor.expectedFamilyID = uuid.New()
				expected = budgetFailureContract
			case "signed expiry mismatch":
				actor.identity.AccessExpiresAt = actor.identity.AccessExpiresAt.Add(time.Second)
			}
			reservation, failure := (&sqlBudgetStore{db: f.db}).reserve(t.Context(), actor)
			require.NotNil(t, failure)
			assert.Equal(t, expected, failure.kind)
			assert.Zero(t, failure.retryAfterSeconds)
			assert.Equal(t, budgetReservation{}, reservation)
			assert.Zero(t, sqlBudgetCount(t, f.db))
		})
	}
}

func TestSQLBudgetOwnReceiptRefundIsFixedWindowAndTerminalSafe(t *testing.T) {
	f := newSQLBudgetFixture(t, "")
	store := &sqlBudgetStore{db: f.db}
	successful, failure := store.reserve(t.Context(), f.actor)
	require.Nil(t, failure)
	failed, failure := store.reserve(t.Context(), f.actor)
	require.Nil(t, failure)
	original := sqlBudgetRead(t, f.db, f.actor)
	f.revoke(t)
	_, err := f.db.ExecContext(t.Context(), `UPDATE users SET credential_epoch=$2 WHERE id=$1`, f.identity.UserID, strings.Repeat("c", 32))
	require.NoError(t, err)
	require.Nil(t, store.refundVerifiedCommitted(t.Context(), successful))
	after := sqlBudgetRead(t, f.db, f.actor)
	assert.Equal(t, []uuid.UUID{failed.receiptID}, after.receipts)
	assert.Equal(t, original.window, after.window)
	assert.True(t, original.started.Equal(after.started))
	assert.True(t, original.expires.Equal(after.expires))
	require.Nil(t, store.refundVerifiedCommitted(t.Context(), successful), "duplicate refund is inert")
	assert.Equal(t, after, sqlBudgetRead(t, f.db, f.actor))
	require.Nil(t, store.refundVerifiedCommitted(t.Context(), failed))
	empty := sqlBudgetRead(t, f.db, f.actor)
	assert.Empty(t, empty.receipts)
	assert.Equal(t, original.window, empty.window)
	assert.True(t, original.expires.Equal(empty.expires))
}

func TestSQLBudgetRefundToEmptyDoesNotSlideLiveWindow(t *testing.T) {
	f := newSQLBudgetFixture(t, "")
	store := &sqlBudgetStore{db: f.db}
	first, failure := store.reserve(t.Context(), f.actor)
	require.Nil(t, failure)
	original := sqlBudgetRead(t, f.db, f.actor)
	require.Nil(t, store.refundVerifiedCommitted(t.Context(), first))
	next, failure := store.reserve(t.Context(), f.actor)
	require.Nil(t, failure)
	assert.NotEqual(t, first.receiptID, next.receiptID)
	assert.Equal(t, first.windowID, next.windowID)
	after := sqlBudgetRead(t, f.db, f.actor)
	assert.True(t, original.started.Equal(after.started))
	assert.True(t, original.expires.Equal(after.expires))
	assert.Equal(t, []uuid.UUID{next.receiptID}, after.receipts)
}

func TestSQLBudgetExpiredGenerationReplacedAndOldRefundInert(t *testing.T) {
	f := newSQLBudgetFixture(t, "")
	oldReceipts := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	sqlBudgetSeed(t, f, "-1 second", oldReceipts)
	old := sqlBudgetRead(t, f.db, f.actor)
	receipt := budgetReservation{userID: old.user, familyID: old.family, windowID: old.window, receiptID: oldReceipts[0], expiresAt: old.expires}
	before := sqlBudgetNow(t, f.db)
	store := &sqlBudgetStore{db: f.db}
	current, failure := store.reserve(t.Context(), f.actor)
	require.Nil(t, failure)
	after := sqlBudgetRead(t, f.db, f.actor)
	assert.NotEqual(t, old.window, current.windowID)
	assert.Equal(t, []uuid.UUID{current.receiptID}, after.receipts)
	assert.False(t, after.started.Before(before))
	assert.Equal(t, 15*time.Minute, after.expires.Sub(after.started))
	require.Nil(t, store.refundVerifiedCommitted(t.Context(), receipt))
	assert.Equal(t, after, sqlBudgetRead(t, f.db, f.actor))
}

func TestSQLBudgetSingleConnectionReleasesBeforeEffectAndRefund(t *testing.T) {
	f := newSQLBudgetFixture(t, "")
	pool := sqlBudgetOtherPool(t)
	store := &sqlBudgetStore{db: pool}
	reservation, failure := store.reserve(t.Context(), f.actor)
	require.Nil(t, failure)
	assert.Zero(t, pool.Stats().InUse)
	effect := sqlBudgetTx(t, pool)
	_, err := effect.ExecContext(t.Context(), `SELECT 1`)
	require.NoError(t, err)
	require.NoError(t, effect.Commit())
	require.Nil(t, store.refundVerifiedCommitted(t.Context(), reservation))
	assert.Zero(t, pool.Stats().InUse)
	assert.Empty(t, sqlBudgetRead(t, f.db, f.actor).receipts)
}

func TestSQLBudgetUnknownReservationCommitReturnsNoPermitAndNeverReplays(t *testing.T) {
	f := newSQLBudgetFixture(t, "")
	pool, fault := sqlBudgetFaultPool(t)
	fault.lose.Store(true)
	reservation, failure := (&sqlBudgetStore{db: pool}).reserve(t.Context(), f.actor)
	require.NotNil(t, failure)
	assert.Equal(t, budgetFailureUnavailable, failure.kind)
	assert.Equal(t, 5, failure.retryAfterSeconds)
	assert.Equal(t, budgetReservation{}, reservation)
	assert.Equal(t, int32(1), fault.begins.Load())
	assert.Equal(t, int32(1), fault.commits.Load())
	assert.Len(t, sqlBudgetRead(t, f.db, f.actor).receipts, 1, "real COMMIT happened despite the caller's unknown result")
	assert.Zero(t, pool.Stats().InUse)
}

func TestSQLBudgetUnknownRefundCommitDoesNotReplayOrRemoveAnotherReceipt(t *testing.T) {
	f := newSQLBudgetFixture(t, "")
	store := &sqlBudgetStore{db: f.db}
	eligible, failure := store.reserve(t.Context(), f.actor)
	require.Nil(t, failure)
	failed, failure := store.reserve(t.Context(), f.actor)
	require.Nil(t, failure)
	original := sqlBudgetRead(t, f.db, f.actor)
	pool, fault := sqlBudgetFaultPool(t)
	fault.lose.Store(true)
	failure = (&sqlBudgetStore{db: pool}).refundVerifiedCommitted(t.Context(), eligible)
	require.NotNil(t, failure)
	assert.Equal(t, budgetFailureSettlementUnknown, failure.kind)
	assert.Zero(t, failure.retryAfterSeconds)
	assert.Equal(t, int32(1), fault.begins.Load())
	assert.Equal(t, int32(1), fault.commits.Load())
	after := sqlBudgetRead(t, f.db, f.actor)
	assert.Equal(t, []uuid.UUID{failed.receiptID}, after.receipts)
	assert.Equal(t, original.window, after.window)
	assert.True(t, original.expires.Equal(after.expires))
}

func TestSQLBudgetCancellationRollsBackAndRetainsOtherCharge(t *testing.T) {
	f := newSQLBudgetFixture(t, "")
	store := &sqlBudgetStore{db: f.db}
	original, failure := store.reserve(t.Context(), f.actor)
	require.Nil(t, failure)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	reservation, failure := store.reserve(ctx, f.actor)
	require.NotNil(t, failure)
	assert.Equal(t, budgetReservation{}, reservation)
	assert.Equal(t, budgetFailureUnavailable, failure.kind)
	require.NotNil(t, store.refundVerifiedCommitted(ctx, original))
	assert.Equal(t, []uuid.UUID{original.receiptID}, sqlBudgetRead(t, f.db, f.actor).receipts)
}

// Exact classification is checked at authority vs accounting boundaries, not by
// exposing raw pq errors or factor/route metadata in the private outcome.
func TestSQLBudgetClosedDatabaseIsBudgetUnavailable(t *testing.T) {
	f := newSQLBudgetFixture(t, "")
	pool := sqlBudgetOtherPool(t)
	require.NoError(t, pool.Close())
	reservation, failure := (&sqlBudgetStore{db: pool}).reserve(t.Context(), f.actor)
	require.NotNil(t, failure)
	assert.Equal(t, budgetFailureUnavailable, failure.kind)
	assert.Equal(t, 5, failure.retryAfterSeconds)
	assert.Empty(t, failure.authorityReason)
	assert.Equal(t, budgetReservation{}, reservation)
	assert.Zero(t, sqlBudgetCount(t, f.db))
}

func TestSQLBudgetConflictingFirstInsertSeesCommittedWinner(t *testing.T) {
	f := newSQLBudgetFixture(t, "")
	pool := sqlBudgetOtherPool(t)
	pid := sqlBudgetPID(t, pool)
	holder := sqlBudgetTx(t, f.db)
	winnerWindow, winnerReceipt := uuid.New(), uuid.New()
	_, err := holder.ExecContext(t.Context(), `WITH t AS (SELECT clock_timestamp() AS now) INSERT INTO step_up_attempt_budgets(user_id,family_id,window_id,window_started_at,expires_at,charged_receipts) SELECT $1,$2,$3,now,now+interval '900 seconds',ARRAY[$4::uuid] FROM t`, f.identity.UserID, f.familyID, winnerWindow, winnerReceipt)
	require.NoError(t, err)
	done := sqlBudgetReserveAsync(t.Context(), &sqlBudgetStore{db: pool}, f.actor)
	sqlBudgetWaitForLock(t, f.db, pid)
	require.NoError(t, holder.Commit())
	result := sqlBudgetAwait(t, done)
	require.Nil(t, result.failure, "separate locked SELECT must see the now-committed conflicting insert")
	row := sqlBudgetRead(t, f.db, f.actor)
	assert.Equal(t, winnerWindow, row.window)
	assert.ElementsMatch(t, []uuid.UUID{winnerReceipt, result.reservation.receiptID}, row.receipts)
	assert.Equal(t, winnerWindow, result.reservation.windowID)
}

func TestSQLBudgetRechecksSignedExpiryAfterActualBudgetRowWait(t *testing.T) {
	f := newSQLBudgetFixture(t, "")
	sqlBudgetSeed(t, f, "10 minutes", nil)
	expiry := sqlBudgetNow(t, f.db).Add(2 * time.Second).Truncate(time.Second)
	actor := sqlBudgetActorWithExpiry(t, f, expiry)
	pool := sqlBudgetOtherPool(t)
	pid := sqlBudgetPID(t, pool)
	holder := sqlBudgetTx(t, f.db)
	_, err := holder.ExecContext(t.Context(), `SELECT 1 FROM step_up_attempt_budgets WHERE user_id=$1 AND family_id=$2 FOR UPDATE`, f.identity.UserID, f.familyID)
	require.NoError(t, err)
	// Keep the actual row-lock wait comfortably below production's one-second
	// lock_timeout. This observes real DB time; no logical clock substitutes.
	require.Eventually(t, func() bool {
		remaining := expiry.Sub(sqlBudgetNow(t, f.db))
		return remaining > 0 && remaining <= 450*time.Millisecond
	}, 2*time.Second, time.Millisecond)
	done := sqlBudgetReserveAsync(t.Context(), &sqlBudgetStore{db: pool}, actor)
	sqlBudgetWaitForLock(t, f.db, pid)
	require.Eventually(t, func() bool { return !sqlBudgetNow(t, f.db).Before(expiry) }, 600*time.Millisecond, time.Millisecond)
	require.NoError(t, holder.Commit())
	result := sqlBudgetAwait(t, done)
	require.NotNil(t, result.failure)
	assert.Equal(t, budgetFailureAuthorityDenied, result.failure.kind)
	assert.Equal(t, loginfamily.ReasonAccessExpired, result.failure.authorityReason)
	assert.Zero(t, result.failure.retryAfterSeconds)
	assert.Equal(t, budgetReservation{}, result.reservation)
	assert.Empty(t, sqlBudgetRead(t, f.db, f.actor).receipts)
}

func TestSQLBudgetSamplesWindowExpiryAfterActualRowLock(t *testing.T) {
	f := newSQLBudgetFixture(t, "")
	receipts := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	sqlBudgetSeed(t, f, "350 milliseconds", receipts)
	original := sqlBudgetRead(t, f.db, f.actor)
	pool := sqlBudgetOtherPool(t)
	pid := sqlBudgetPID(t, pool)
	holder := sqlBudgetTx(t, f.db)
	_, err := holder.ExecContext(t.Context(), `SELECT 1 FROM step_up_attempt_budgets WHERE user_id=$1 AND family_id=$2 FOR UPDATE`, f.identity.UserID, f.familyID)
	require.NoError(t, err)
	done := sqlBudgetReserveAsync(t.Context(), &sqlBudgetStore{db: pool}, f.actor)
	sqlBudgetWaitForLock(t, f.db, pid)
	require.Eventually(t, func() bool { return !sqlBudgetNow(t, f.db).Before(original.expires) }, 600*time.Millisecond, time.Millisecond)
	require.NoError(t, holder.Commit())
	result := sqlBudgetAwait(t, done)
	require.Nil(t, result.failure, "post-wait expiry must replace the full expired window")
	row := sqlBudgetRead(t, f.db, f.actor)
	assert.NotEqual(t, original.window, row.window)
	assert.Equal(t, []uuid.UUID{result.reservation.receiptID}, row.receipts)
	assert.False(t, row.started.Before(original.expires))
	assert.Equal(t, 15*time.Minute, row.expires.Sub(row.started))
}

func TestSQLBudgetUserLockFailureRetainsAuthorityClassification(t *testing.T) {
	f := newSQLBudgetFixture(t, "")
	pool := sqlBudgetOtherPool(t)
	pid := sqlBudgetPID(t, pool)
	holder := sqlBudgetTx(t, f.db)
	_, err := holder.ExecContext(t.Context(), `SELECT 1 FROM users WHERE id=$1 FOR UPDATE`, f.identity.UserID)
	require.NoError(t, err)
	done := sqlBudgetReserveAsync(t.Context(), &sqlBudgetStore{db: pool}, f.actor)
	sqlBudgetWaitForLock(t, f.db, pid)
	result := sqlBudgetAwait(t, done)
	require.NotNil(t, result.failure)
	assert.Equal(t, budgetFailureAuthorityUnavailable, result.failure.kind)
	assert.Equal(t, loginfamily.ReasonSQLUnavailable, result.failure.authorityReason)
	assert.Zero(t, result.failure.retryAfterSeconds)
	assert.Equal(t, budgetReservation{}, result.reservation)
	assert.Zero(t, sqlBudgetCount(t, f.db))
	require.NoError(t, holder.Rollback())
}

func TestSQLBudgetCanceledActualWaitDoesNotCharge(t *testing.T) {
	f := newSQLBudgetFixture(t, "")
	sqlBudgetSeed(t, f, "10 minutes", nil)
	original := sqlBudgetRead(t, f.db, f.actor)
	pool := sqlBudgetOtherPool(t)
	pid := sqlBudgetPID(t, pool)
	holder := sqlBudgetTx(t, f.db)
	_, err := holder.ExecContext(t.Context(), `SELECT 1 FROM step_up_attempt_budgets WHERE user_id=$1 AND family_id=$2 FOR UPDATE`, f.identity.UserID, f.familyID)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := sqlBudgetReserveAsync(ctx, &sqlBudgetStore{db: pool}, f.actor)
	sqlBudgetWaitForLock(t, f.db, pid)
	cancel()
	result := sqlBudgetAwait(t, done)
	require.NotNil(t, result.failure)
	assert.Equal(t, budgetFailureUnavailable, result.failure.kind)
	assert.Equal(t, 5, result.failure.retryAfterSeconds)
	assert.Equal(t, budgetReservation{}, result.reservation)
	require.NoError(t, holder.Rollback())
	assert.Equal(t, original, sqlBudgetRead(t, f.db, f.actor))
	assert.Zero(t, pool.Stats().InUse)
}

func TestSQLBudgetFixtureEnforcesReceiptAndWindowStructure(t *testing.T) {
	f := newSQLBudgetFixture(t, "")
	sqlBudgetSeed(t, f, "10 minutes", nil)
	for _, tc := range []struct{ name, query string }{
		{"null receipts", `UPDATE step_up_attempt_budgets SET charged_receipts=NULL`},
		{"nil receipt", `UPDATE step_up_attempt_budgets SET charged_receipts=ARRAY['00000000-0000-0000-0000-000000000000'::uuid]`},
		{"null element", `UPDATE step_up_attempt_budgets SET charged_receipts=ARRAY[NULL::uuid]`},
		{"duplicate", `UPDATE step_up_attempt_budgets SET charged_receipts=ARRAY[$1::uuid,$1::uuid]`},
		{"six receipts", `UPDATE step_up_attempt_budgets SET charged_receipts=ARRAY[$1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,$6::uuid]`},
		{"lower bound zero", `UPDATE step_up_attempt_budgets SET charged_receipts='[0:0]={11111111-1111-4111-8111-111111111111}'::uuid[]`},
		{"two dimensions", `UPDATE step_up_attempt_budgets SET charged_receipts=ARRAY[ARRAY[$1::uuid]]`},
		{"zero user", `UPDATE step_up_attempt_budgets SET user_id='00000000-0000-0000-0000-000000000000'::uuid`},
		{"zero family", `UPDATE step_up_attempt_budgets SET family_id='00000000-0000-0000-0000-000000000000'::uuid`},
		{"zero window", `UPDATE step_up_attempt_budgets SET window_id='00000000-0000-0000-0000-000000000000'::uuid`},
		{"infinite start", `UPDATE step_up_attempt_budgets SET window_started_at='-infinity'::timestamptz`},
		{"infinite expiry", `UPDATE step_up_attempt_budgets SET expires_at='infinity'::timestamptz`},
		{"wrong interval", `UPDATE step_up_attempt_budgets SET expires_at=window_started_at+interval '899 seconds'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Each bad statement runs in its own transaction; rejected data cannot
			// poison subsequent controls or require a production validation bypass.
			var args []any
			for i := 1; i <= 6; i++ {
				if strings.Contains(tc.query, "$"+string(rune('0'+i))) {
					args = append(args, uuid.New())
				}
			}
			_, err := f.db.ExecContext(t.Context(), tc.query, args...)
			require.Error(t, err)
		})
	}
	// The canonical empty row remains admissible after every refusal.
	reservation, failure := (&sqlBudgetStore{db: f.db}).reserve(t.Context(), f.actor)
	require.Nil(t, failure)
	assert.Equal(t, []uuid.UUID{reservation.receiptID}, sqlBudgetRead(t, f.db, f.actor).receipts)
}

func TestSQLBudgetRejectsMalformedActorsAndReceiptsWithoutChangingCharge(t *testing.T) {
	f := newSQLBudgetFixture(t, "")
	store := &sqlBudgetStore{db: f.db}
	valid, failure := store.reserve(t.Context(), f.actor)
	require.Nil(t, failure)
	before := sqlBudgetRead(t, f.db, f.actor)
	for _, name := range []string{"zero user", "zero SID", "zero family", "zero expiry", "invalid epoch"} {
		t.Run("actor/"+name, func(t *testing.T) {
			actor := f.actor
			switch name {
			case "zero user":
				actor.identity.UserID = uuid.Nil
			case "zero SID":
				actor.identity.SID = uuid.Nil
			case "zero family":
				actor.expectedFamilyID = uuid.Nil
			case "zero expiry":
				actor.identity.AccessExpiresAt = time.Time{}
			case "invalid epoch":
				actor.identity.CredentialEpoch = "not-an-epoch"
			}
			reservation, failure := store.reserve(t.Context(), actor)
			require.NotNil(t, failure)
			assert.Equal(t, budgetFailureContract, failure.kind)
			assert.Equal(t, budgetReservation{}, reservation)
			assert.Zero(t, failure.retryAfterSeconds)
		})
	}
	for _, name := range []string{"zero user", "zero family", "zero window", "zero receipt", "zero expiry"} {
		t.Run("receipt/"+name, func(t *testing.T) {
			receipt := valid
			switch name {
			case "zero user":
				receipt.userID = uuid.Nil
			case "zero family":
				receipt.familyID = uuid.Nil
			case "zero window":
				receipt.windowID = uuid.Nil
			case "zero receipt":
				receipt.receiptID = uuid.Nil
			case "zero expiry":
				receipt.expiresAt = time.Time{}
			}
			failure := store.refundVerifiedCommitted(t.Context(), receipt)
			require.NotNil(t, failure)
			assert.Equal(t, budgetFailureContract, failure.kind)
			assert.Zero(t, failure.retryAfterSeconds)
		})
	}
	assert.Equal(t, before, sqlBudgetRead(t, f.db, f.actor))
	t.Run("nil receiver and context", func(t *testing.T) {
		for _, candidate := range []*sqlBudgetStore{nil, {}} {
			reservation, failure := candidate.reserve(t.Context(), f.actor)
			require.NotNil(t, failure)
			assert.Equal(t, budgetFailureContract, failure.kind)
			assert.Equal(t, budgetReservation{}, reservation)
			failure = candidate.refundVerifiedCommitted(t.Context(), valid)
			require.NotNil(t, failure)
			assert.Equal(t, budgetFailureContract, failure.kind)
		}
		reservation, failure := store.reserve(nil, f.actor) //nolint:staticcheck // SA1012: literal nil verifies reserve rejects a missing context as a contract failure.
		require.NotNil(t, failure)
		assert.Equal(t, budgetFailureContract, failure.kind)
		assert.Equal(t, budgetReservation{}, reservation)
		failure = store.refundVerifiedCommitted(nil, valid) //nolint:staticcheck // SA1012: literal nil verifies settlement rejects a missing context as a contract failure.
		require.NotNil(t, failure)
		assert.Equal(t, budgetFailureContract, failure.kind)
	})
}

func TestSQLBudgetValidInertAndMismatchedReceipts(t *testing.T) {
	f := newSQLBudgetFixture(t, "")
	store := &sqlBudgetStore{db: f.db}
	valid, failure := store.reserve(t.Context(), f.actor)
	require.Nil(t, failure)
	original := sqlBudgetRead(t, f.db, f.actor)
	absent := valid
	absent.receiptID = uuid.New()
	require.Nil(t, store.refundVerifiedCommitted(t.Context(), absent))
	differentWindow := valid
	differentWindow.windowID = uuid.New()
	require.Nil(t, store.refundVerifiedCommitted(t.Context(), differentWindow))
	missingFamily := valid
	missingFamily.familyID = uuid.New()
	require.Nil(t, store.refundVerifiedCommitted(t.Context(), missingFamily))
	wrongExpiry := valid
	wrongExpiry.expiresAt = wrongExpiry.expiresAt.Add(time.Microsecond)
	failure = store.refundVerifiedCommitted(t.Context(), wrongExpiry)
	require.NotNil(t, failure)
	assert.Equal(t, budgetFailureContract, failure.kind)
	assert.Zero(t, failure.retryAfterSeconds)
	assert.Equal(t, original, sqlBudgetRead(t, f.db, f.actor))
	_, err := f.db.ExecContext(t.Context(), `DELETE FROM step_up_attempt_budgets WHERE user_id=$1 AND family_id=$2`, valid.userID, valid.familyID)
	require.NoError(t, err)
	require.Nil(t, store.refundVerifiedCommitted(t.Context(), valid), "valid receipt with absent accounting row is inert")
}

func TestSQLBudgetFutureWindowRefusesRatherThanRebases(t *testing.T) {
	f := newSQLBudgetFixture(t, "")
	receiptID := uuid.New()
	sqlBudgetSeed(t, f, "901 seconds", []uuid.UUID{receiptID})
	before := sqlBudgetRead(t, f.db, f.actor)
	store := &sqlBudgetStore{db: f.db}
	reservation, failure := store.reserve(t.Context(), f.actor)
	require.NotNil(t, failure)
	assert.Equal(t, budgetFailureUnavailable, failure.kind)
	assert.Equal(t, 5, failure.retryAfterSeconds)
	assert.Equal(t, budgetReservation{}, reservation)
	receipt := budgetReservation{userID: before.user, familyID: before.family, windowID: before.window, receiptID: receiptID, expiresAt: before.expires}
	failure = store.refundVerifiedCommitted(t.Context(), receipt)
	require.NotNil(t, failure)
	assert.Equal(t, budgetFailureUnavailable, failure.kind)
	assert.Equal(t, 5, failure.retryAfterSeconds)
	assert.Equal(t, before, sqlBudgetRead(t, f.db, f.actor))
}

func TestSQLBudgetStoreRefusesCorruptedPresentReceipts(t *testing.T) {
	f := newSQLBudgetFixture(t, "")
	store := &sqlBudgetStore{db: f.db}
	reservation, failure := store.reserve(t.Context(), f.actor)
	require.Nil(t, failure)
	// Corrupt only this owned disposable test relation after proving its CHECK.
	// This exercises defensive reads; it is not a production schema operation.
	_, err := f.db.ExecContext(t.Context(), `ALTER TABLE step_up_attempt_budgets DROP CONSTRAINT budget_test_receipts_valid`)
	require.NoError(t, err)
	_, err = f.db.ExecContext(t.Context(), `UPDATE step_up_attempt_budgets SET charged_receipts=ARRAY[$1::uuid,$1::uuid]`, reservation.receiptID)
	require.NoError(t, err)
	original := sqlBudgetRead(t, f.db, f.actor)
	next, failure := store.reserve(t.Context(), f.actor)
	require.NotNil(t, failure)
	assert.Equal(t, budgetFailureUnavailable, failure.kind)
	assert.Equal(t, 5, failure.retryAfterSeconds)
	assert.Equal(t, budgetReservation{}, next)
	failure = store.refundVerifiedCommitted(t.Context(), reservation)
	require.NotNil(t, failure)
	assert.Equal(t, budgetFailureUnavailable, failure.kind)
	assert.Equal(t, 5, failure.retryAfterSeconds)
	assert.Equal(t, original, sqlBudgetRead(t, f.db, f.actor))
}

func TestSQLBudgetSettlementDoesNotRequireRetainedPrincipal(t *testing.T) {
	f := newSQLBudgetFixture(t, "")
	store := &sqlBudgetStore{db: f.db}
	own, failure := store.reserve(t.Context(), f.actor)
	require.Nil(t, failure)
	other, failure := store.reserve(t.Context(), f.actor)
	require.Nil(t, failure)
	f.revoke(t)
	_, err := f.db.ExecContext(t.Context(), `DELETE FROM users WHERE id=$1`, f.identity.UserID)
	require.NoError(t, err)
	var retained bool
	require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM login_families WHERE family_id=$1 AND user_id=$2)`, f.familyID, f.identity.UserID).Scan(&retained))
	require.True(t, retained)
	require.Nil(t, store.refundVerifiedCommitted(t.Context(), own))
	assert.Equal(t, []uuid.UUID{other.receiptID}, sqlBudgetRead(t, f.db, f.actor).receipts)
}
