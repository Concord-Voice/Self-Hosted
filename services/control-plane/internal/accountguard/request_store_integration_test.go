package accountguard

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func requestPolicyInput(now time.Time, class3 bool) decide.Input {
	refs := []decide.CredRef{requestOrigin()}
	if class3 {
		refs = []decide.CredRef{{Kind: decide.CredTOTP, ID: "opaque-totp", Version: 19}, {Kind: decide.CredRecoveryKey, ID: "opaque-anchor", Version: 23}}
	}
	in := decide.Input{Now: now, Proof: decide.Proof{Used: refs}, CurrentKeyID: 7, GuessableBudgetClear: true}
	for _, ref := range refs {
		in.Creds = append(in.Creds, decide.Cred{Ref: ref, MaturesAt: now.Add(-10 * 24 * time.Hour)})
	}
	return in
}
func requestOpenActual(t *testing.T, f requestFixture) uuid.UUID {
	t.Helper()
	in := requestPolicyInput(f.now, false)
	in.Act = decide.OpenBreakGlass{Candidate: requestCandidate(), Reset: true}
	out := decide.Decide(in)
	require.Equal(t, decide.RowT6, out.Row)
	tx := requestTx(t, f.db)
	result, remainder := requestApply(t, tx, f, out, requestPayload())
	require.Empty(t, remainder.Effects)
	require.NotEmpty(t, remainder.Notices, "request-only effects still require notices/source coordination")
	require.NotEqual(t, uuid.Nil, result.openedRequestID)
	require.NoError(t, tx.Commit())
	return result.openedRequestID
}
func TestRequestStoreActualOpenRoundTripCancelFoldAndReopen(t *testing.T) {
	f := newRequestFixture(t)
	id := requestOpenActual(t, f)
	tx := requestTx(t, f.db)
	// Distinct settled/accruing fields expose dropped fields and unit conversion.
	heldSince := f.now.Add(1234*time.Millisecond + 567*time.Microsecond)
	_, err := tx.ExecContext(t.Context(), `UPDATE account_pending_requests SET held_ms=11,held_since=$2 WHERE id=$1`, id, heldSince)
	require.NoError(t, err)
	f.now = heldSince.Add(1234*time.Millisecond + 999*time.Microsecond)
	snap, err := (requestStore{}).loadTx(t.Context(), tx, f.user, f.now)
	require.NoError(t, err)
	require.Len(t, snap.Open, 1)
	r := snap.Open[0]
	require.Equal(t, id.String(), r.ID)
	require.Equal(t, decide.ReqBreakGlass, r.Kind)
	require.Equal(t, decide.StatusPending, r.Status)
	require.Equal(t, decide.C1, r.OriginClass)
	require.Equal(t, []decide.CredRef{requestOrigin()}, r.Origins)
	require.True(t, r.ResetFactors)
	require.Empty(t, r.Targets)
	require.Equal(t, int64(11), r.HeldMs)
	require.True(t, heldSince.Equal(r.HeldSince))
	require.Equal(t, requestCandidate().Keyed, r.Fingerprint)
	require.Equal(t, requestCandidate().Coarse, r.Coarse)
	require.Equal(t, int16(7), r.FingerprintKeyID)
	require.Equal(t, "synthetic-opaque-argon-hash", *snap.pending[id].passwordHash)
	require.True(t, r.DisputedAt.IsZero())
	require.True(t, r.CancelledAt.IsZero())
	require.Empty(t, snap.RecentCancels)
	in := requestPolicyInput(f.now, false)
	in.Reqs = snap.Open
	in.Act = decide.Cancel{IDs: []string{id.String()}}
	out := decide.Decide(in)
	require.Equal(t, decide.RowT7, out.Row)
	_, rem := requestApply(t, tx, f, out, requestPendingPayload{})
	require.Empty(t, rem.Effects)
	require.NotEmpty(t, rem.Notices)
	var status string
	var held int64
	var since, hash sql.NullString
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT status,held_ms,held_since::text,pending_password_hash FROM account_pending_requests WHERE id=$1`, id).Scan(&status, &held, &since, &hash))
	require.Equal(t, "cancelled", status)
	require.Equal(t, int64(1245), held)
	require.False(t, since.Valid)
	require.False(t, hash.Valid)
	after, err := (requestStore{}).loadTx(t.Context(), tx, f.user, f.now)
	require.NoError(t, err)
	require.Empty(t, after.Open)
	require.Empty(t, after.pending)
	require.Len(t, after.RecentCancels, 1)
	require.Equal(t, decide.CancelProof, after.RecentCancels[0].CancelReason)
	require.Equal(t, decide.C1, after.RecentCancels[0].CancellerClass)
	require.Equal(t, r.Fingerprint, after.RecentCancels[0].Fingerprint)
	// Current generation uses keyed equality; a stale generation must fall back
	// to the persisted coarse value even when its keyed bytes differ.
	for _, generation := range []int16{7, 8} {
		probe := requestPolicyInput(f.now, false)
		probe.RecentCancels = after.RecentCancels
		probe.CurrentKeyID = generation
		cand := requestCandidate()
		cand.KeyID = generation
		cand.Keyed[0] ^= 0xff
		probe.Act = decide.OpenBreakGlass{Candidate: cand}
		got := decide.Decide(probe)
		if generation == 7 {
			require.Equal(t, decide.RowT6, got.Row)
		} else {
			require.Equal(t, decide.RowT10, got.Row)
		}
	}
	in.Reqs = nil
	in.RecentCancels = after.RecentCancels
	in.Act = decide.OpenBreakGlass{Candidate: requestCandidate()}
	reopened := decide.Decide(in)
	require.Equal(t, decide.RowT10, reopened.Row)
	opened, _ := requestApply(t, tx, f, reopened, requestPayload())
	require.NotEqual(t, uuid.Nil, opened.disputeGroupID)
	require.NoError(t, tx.Commit())
	require.Equal(t, 2, requestCount(t, f.db))
}
func TestRequestStoreCallerRollbackRemovesAllRowsAndOrigins(t *testing.T) {
	f := newRequestFixture(t)
	in := requestPolicyInput(f.now, false)
	in.Act = decide.OpenBreakGlass{Candidate: requestCandidate()}
	tx := requestTx(t, f.db)
	requestApply(t, tx, f, decide.Decide(in), requestPayload())
	require.NoError(t, tx.Rollback())
	require.Zero(t, requestCount(t, f.db))
	var n int
	require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT count(*) FROM account_pending_request_origins`).Scan(&n))
	require.Zero(t, n)
}
func TestRequestStoreCompletedT6cRetainsPasswordEpochAndNoticeObligations(t *testing.T) {
	f := newRequestFixture(t)
	in := requestPolicyInput(f.now, true)
	in.Act = decide.OpenBreakGlass{}
	out := decide.Decide(in)
	require.Equal(t, decide.RowT6c, out.Row)
	require.Len(t, out.Effects, 1)
	require.True(t, out.SetsPassword)
	require.True(t, out.RotateEpoch)
	require.Equal(t, decide.C3, out.EnrolClass)
	tx := requestTx(t, f.db)
	result, rem := requestApply(t, tx, f, out, requestPendingPayload{})
	require.Empty(t, rem.Effects)
	expected := out
	expected.Effects = nil
	require.Equal(t, expected, rem)
	var status string
	var nulls bool
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT status,num_nonnulls(pending_password_hash,pending_address,request_fingerprint,coarse_fingerprint,fingerprint_key_id,held_since)=0 FROM account_pending_requests WHERE id=$1`, result.openedRequestID).Scan(&status, &nulls))
	require.Equal(t, "completed", status)
	require.True(t, nulls)
	require.NoError(t, tx.Commit())
}
func TestRequestStoreInvalidPlanForeignOrMissingRowCannotWritePrecedingOpen(t *testing.T) {
	f := newRequestFixture(t)
	id := requestOpenActual(t, f)
	for _, target := range []string{uuid.NewString(), id.String()} {
		tx := requestTx(t, f.db)
		user := f.user
		if target == id.String() {
			user = uuid.New()
		}
		plan := requestEffectPlan{effects: []decide.Effect{{Kind: decide.EffectOpen, Open: requestOpening(f.now)}, {Kind: decide.EffectCancel, RequestID: target, Reason: decide.CancelProof, CancellerClass: decide.C1}}}
		prepared, err := prepareRequestOpenings(plan, requestPayload(), uuid.NewRandom)
		require.NoError(t, err)
		_, err = (requestStore{}).applyTx(t.Context(), tx, user, f.now, plan, prepared)
		require.Error(t, err)
		var n int
		require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT count(*) FROM account_pending_requests`).Scan(&n), "row preflight must precede writes/constraints")
		require.Equal(t, 1, n)
		require.NoError(t, tx.Rollback())
	}
	tx := requestTx(t, f.db)
	plan := requestEffectPlan{effects: []decide.Effect{{Kind: decide.EffectOpen, Open: requestOpening(f.now)}, {Kind: 255}}}
	_, err := (requestStore{}).applyTx(t.Context(), tx, f.user, f.now, plan, preparedOpenings{})
	require.Error(t, err)
	require.NoError(t, tx.Commit())
	require.Equal(t, 1, requestCount(t, f.db))
}
func TestRequestStoreMissingOrForeignTargetPreflightPrecedesValidAddressOpen(t *testing.T) {
	f := newRequestFixture(t)
	existing := requestOpenActual(t, f)
	foreignUser := testdb.CreateUser(t, f.db)
	opening := requestOpening(f.now)
	opening.Kind = decide.ReqAddressChange
	opening.Targets = []decide.CredRef{{Kind: decide.CredAddress, ID: "opaque-address", Version: 37}}
	address := "next@example.invalid"
	payload := requestPendingPayload{address: &address}
	for _, tc := range []struct {
		name   string
		user   uuid.UUID
		target string
	}{
		{"missing target", f.user, uuid.NewString()},
		{"foreign target", foreignUser, existing.String()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := requestEffectPlan{effects: []decide.Effect{
				{Kind: decide.EffectOpen, Open: opening},
				{Kind: decide.EffectCancel, RequestID: tc.target, Reason: decide.CancelProof, CancellerClass: decide.C1},
			}}
			prepared, err := prepareRequestOpenings(plan, payload, uuid.NewRandom)
			require.NoError(t, err, "the address opening itself must be valid")
			tx := requestTx(t, f.db)
			_, err = (requestStore{}).applyTx(t.Context(), tx, tc.user, f.now, plan, prepared)
			require.Error(t, err)
			var requests, origins int
			require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM account_pending_requests),(SELECT count(*) FROM account_pending_request_origins)`).Scan(&requests, &origins), "preflight refusal must leave the caller transaction usable")
			require.Equal(t, 1, requests, "a valid address opening must not be inserted before target preflight")
			require.Equal(t, 1, origins)
			require.NoError(t, tx.Rollback())
			require.Equal(t, 1, requestCount(t, f.db))
		})
	}
	// The same opening and payload can actually insert without a target failure;
	// this control excludes an unrelated SQL constraint as the refusal oracle.
	plan := requestEffectPlan{effects: []decide.Effect{{Kind: decide.EffectOpen, Open: opening}}}
	prepared, err := prepareRequestOpenings(plan, payload, uuid.NewRandom)
	require.NoError(t, err)
	tx := requestTx(t, f.db)
	result, err := (requestStore{}).applyTx(t.Context(), tx, f.user, f.now, plan, prepared)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, result.openedRequestID)
	var requests int
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT count(*) FROM account_pending_requests`).Scan(&requests))
	require.Equal(t, 2, requests)
	require.NoError(t, tx.Rollback())
	require.Equal(t, 1, requestCount(t, f.db))
}
func TestRequestStoreLoadRetentionBoundaryAndNullFingerprintRefusal(t *testing.T) {
	f := newRequestFixture(t)
	id := requestOpenActual(t, f)
	tx := requestTx(t, f.db)
	_, err := tx.ExecContext(t.Context(), `UPDATE account_pending_requests SET status='cancelled',cancelled_at=$2,cancel_reason='proof',canceller_class=1,pending_password_hash=NULL WHERE id=$1`, id, f.now.Add(-decide.NCancelCP+time.Microsecond))
	require.NoError(t, err)
	snap, err := (requestStore{}).loadTx(t.Context(), tx, f.user, f.now)
	require.NoError(t, err)
	require.Len(t, snap.RecentCancels, 1)
	_, err = tx.ExecContext(t.Context(), `UPDATE account_pending_requests SET cancelled_at=$2 WHERE id=$1`, id, f.now.Add(-decide.NCancelCP))
	require.NoError(t, err)
	snap, err = (requestStore{}).loadTx(t.Context(), tx, f.user, f.now)
	require.NoError(t, err)
	require.Empty(t, snap.RecentCancels)
	future := f.now.Add(time.Hour)
	_, err = tx.ExecContext(t.Context(), `UPDATE account_pending_requests SET cancelled_at=$2 WHERE id=$1`, id, future)
	require.NoError(t, err)
	snap, err = (requestStore{}).loadTx(t.Context(), tx, f.user, f.now)
	require.NoError(t, err)
	require.Len(t, snap.RecentCancels, 1)
	require.True(t, future.Equal(snap.RecentCancels[0].CancelledAt), "future cancels cannot evade the conservative window")
	_, err = tx.ExecContext(t.Context(), `UPDATE account_pending_requests SET cancelled_at=$2,request_fingerprint=NULL,coarse_fingerprint=NULL,fingerprint_key_id=NULL WHERE id=$1`, id, f.now)
	require.NoError(t, err)
	_, err = (requestStore{}).loadTx(t.Context(), tx, f.user, f.now)
	require.Error(t, err, "in-window redaction cannot disable reopen protection")
	require.NoError(t, tx.Rollback())
}
func TestRequestStoreAddressCompletionAndStaleMaterialVersionKeepRemainder(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "current", true: "stale"}[stale], func(t *testing.T) {
			f := newRequestFixture(t)
			target := decide.CredRef{Kind: decide.CredAddress, ID: "opaque-address", Version: 37}
			opening := requestOpening(f.now)
			opening.Kind = decide.ReqAddressChange
			opening.Targets = []decide.CredRef{target}
			address := "next@example.invalid"
			tx := requestTx(t, f.db)
			opened, _ := requestApply(t, tx, f, decide.Outcome{Effects: []decide.Effect{{Kind: decide.EffectOpen, Open: opening}}}, requestPendingPayload{address: &address})
			f.now = f.now.Add(decide.DeltaCP + time.Second)
			snap, err := (requestStore{}).loadTx(t.Context(), tx, f.user, f.now)
			require.NoError(t, err)
			require.Len(t, snap.Open, 1)
			require.Equal(t, int64(37), snap.Open[0].Targets[0].Version)
			require.Equal(t, address, *snap.pending[opened.openedRequestID].address)
			in := requestPolicyInput(f.now, false)
			in.Act = decide.Complete{ID: opened.openedRequestID.String()}
			in.Reqs = snap.Open
			in.Reqs[0].AcceptedAddressNotice = true
			in.Reqs[0].MatureNoticesSettled = true
			if stale {
				target.Version++
			}
			in.Creds = append(in.Creds, decide.Cred{Ref: target, MaturesAt: opening.OpenedAt.Add(-time.Hour)})
			out := decide.Decide(in)
			require.Equal(t, decide.RowT11, out.Row)
			_, rem := requestApply(t, tx, f, out, requestPendingPayload{})
			require.Empty(t, rem.Effects)
			if stale {
				require.Equal(t, decide.ClassNone, rem.EnrolClass)
			} else {
				require.Equal(t, decide.C1, rem.EnrolClass)
			}
			var status string
			var payloadCleared bool
			require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT status,pending_address IS NULL FROM account_pending_requests WHERE id=$1`, opened.openedRequestID).Scan(&status, &payloadCleared))
			require.Equal(t, map[bool]string{false: "completed", true: "cancelled"}[stale], status)
			require.True(t, payloadCleared)
			require.NoError(t, tx.Commit())
		})
	}
}

// Lock observation is on the database's waiter, not an arbitrary sleep.
func TestRequestStoreLoadWaitsAndSeesCommittedCancellation(t *testing.T) {
	f := newRequestFixture(t)
	id := requestOpenActual(t, f)
	holder := requestTx(t, f.db)
	_, err := holder.ExecContext(t.Context(), `SELECT id FROM account_pending_requests WHERE id=$1 FOR UPDATE`, id)
	require.NoError(t, err)
	other, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	other.SetMaxOpenConns(1)
	conn, err := other.Conn(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	var pid int
	require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	tx, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		e := tx.Rollback()
		if e != sql.ErrTxDone {
			require.NoError(t, e)
		}
	})
	type result struct {
		snap requestSnapshot
		err  error
	}
	done := make(chan result, 1)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		s, e := (requestStore{}).loadTx(ctx, tx, f.user, f.now)
		done <- result{s, e}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-workerDone:
		case <-time.After(4 * time.Second):
			t.Error("request lock waiter leaked")
		}
	})
	require.Eventually(t, func() bool {
		var waiting bool
		e := f.db.QueryRowContext(t.Context(), `SELECT COALESCE(wait_event_type='Lock',false) FROM pg_stat_activity WHERE pid=$1`, pid).Scan(&waiting)
		return e == nil && waiting
	}, time.Second, time.Millisecond)
	_, err = holder.ExecContext(t.Context(), `UPDATE account_pending_requests SET status='cancelled',cancelled_at=$2,cancel_reason='proof',canceller_class=1,pending_password_hash=NULL WHERE id=$1`, id, f.now)
	require.NoError(t, err)
	require.NoError(t, holder.Commit())
	select {
	case got := <-done:
		require.NoError(t, got.err)
		require.Empty(t, got.snap.Open)
		require.Len(t, got.snap.RecentCancels, 1)
	case <-ctx.Done():
		t.Fatal("request lock waiter did not settle")
	}
	require.NoError(t, tx.Rollback())
}

func TestRequestStoreSharedOriginDisputeRedisputeAndActualResolve(t *testing.T) {
	f := newRequestFixture(t)
	tx := requestTx(t, f.db)
	// Equal origins in one dispute group are legal; no group/origin uniqueness shortcut.
	opening := requestOpening(f.now)
	opening.Status = decide.StatusDisputed
	opening.DisputedAt = f.now
	opening.Origins = []decide.CredRef{requestOrigin(), {Kind: decide.CredTOTP, ID: "opaque-six", Version: 5}, {Kind: decide.CredRecoveryDevice, ID: "opaque-ten", Version: 9}}
	first, _ := requestApply(t, tx, f, decide.Outcome{Effects: []decide.Effect{{Kind: decide.EffectOpen, Open: opening}}}, requestPayload())
	group := first.disputeGroupID
	second, _ := requestApply(t, tx, f, decide.Outcome{Effects: []decide.Effect{{Kind: decide.EffectOpen, Open: opening, Group: group.String()}}}, requestPayload())
	f.now = f.now.Add(time.Hour)
	requestApply(t, tx, f, decide.Outcome{Effects: []decide.Effect{{Kind: decide.EffectDispute, RequestID: first.openedRequestID.String(), Group: group.String()}}}, requestPendingPayload{})
	snap, err := (requestStore{}).loadTx(t.Context(), tx, f.user, f.now)
	require.NoError(t, err)
	require.Len(t, snap.Open, 2)
	for _, r := range snap.Open {
		require.True(t, opening.DisputedAt.Equal(r.DisputedAt))
		require.Equal(t, group.String(), r.DisputeGroup)
		require.Equal(t, opening.Origins, r.Origins)
	}
	in := requestPolicyInput(f.now, true)
	in.Reqs = snap.Open
	in.Act = decide.Resolve{Group: group.String(), Chosen: first.openedRequestID.String()}
	out := decide.Decide(in)
	require.Equal(t, decide.RowT12, out.Row)
	require.Equal(t, decide.VerdictApplied, out.Verdict)
	_, rem := requestApply(t, tx, f, out, requestPendingPayload{})
	require.Empty(t, rem.Effects)
	require.NotEmpty(t, rem.Notices)
	require.False(t, rem.RotateEpoch)
	after, err := (requestStore{}).loadTx(t.Context(), tx, f.user, f.now)
	require.NoError(t, err)
	require.Len(t, after.Open, 1)
	require.Len(t, after.RecentCancels, 1)
	chosen := after.Open[0]
	require.Equal(t, first.openedRequestID.String(), chosen.ID)
	require.Equal(t, decide.StatusPending, chosen.Status)
	require.Empty(t, chosen.DisputeGroup)
	require.True(t, chosen.DisputedAt.IsZero())
	require.True(t, opening.OpenedAt.Equal(chosen.OpenedAt))
	require.True(t, opening.CompletesAt.Equal(chosen.CompletesAt))
	cancelled := after.RecentCancels[0]
	require.Equal(t, second.openedRequestID.String(), cancelled.ID)
	require.Equal(t, decide.CancelDisputeResolved, cancelled.CancelReason)
	require.Equal(t, decide.C3, cancelled.CancellerClass)
	require.Equal(t, group.String(), cancelled.DisputeGroup)
	require.True(t, opening.DisputedAt.Equal(cancelled.DisputedAt))
	require.Equal(t, requestCandidate().Keyed, cancelled.Fingerprint)
	require.Len(t, after.pending, 1)
	require.NoError(t, tx.Commit())
}

func TestRequestStoreCorruptOriginsAndNullCancelClassRefuse(t *testing.T) {
	for _, scenario := range []string{"absent-origin", "digest-mismatch", "unknown-origin", "null-proof-class"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRequestFixture(t)
			id := requestOpenActual(t, f)
			tx := requestTx(t, f.db)
			switch scenario {
			case "absent-origin":
				_, err := tx.ExecContext(t.Context(), `DELETE FROM account_pending_request_origins WHERE request_id=$1`, id)
				require.NoError(t, err)
			case "digest-mismatch":
				_, err := tx.ExecContext(t.Context(), `UPDATE account_pending_request_origins SET material_version=material_version+1 WHERE request_id=$1`, id)
				require.NoError(t, err)
			case "unknown-origin":
				_, err := tx.ExecContext(t.Context(), `ALTER TABLE account_pending_request_origins DROP CONSTRAINT ag_origin_kind`)
				require.NoError(t, err)
				_, err = tx.ExecContext(t.Context(), `UPDATE account_pending_request_origins SET credential_kind='future-kind' WHERE request_id=$1`, id)
				require.NoError(t, err)
			case "null-proof-class":
				// PostgreSQL CHECK normally rejects this; dropping only this fixture check tests defensive decoding of legacy corruption.
				_, err := tx.ExecContext(t.Context(), `ALTER TABLE account_pending_requests DROP CONSTRAINT ag_request_cancel`)
				require.NoError(t, err)
				_, err = tx.ExecContext(t.Context(), `UPDATE account_pending_requests SET status='cancelled',cancelled_at=$2,cancel_reason='proof',canceller_class=NULL,pending_password_hash=NULL WHERE id=$1`, id, f.now)
				require.NoError(t, err)
			}
			_, err := (requestStore{}).loadTx(t.Context(), tx, f.user, f.now)
			require.Error(t, err)
			require.NoError(t, tx.Rollback())
		})
	}
}

func TestRequestStoreErasureCascadesOriginsAndMissingHoldFoldCannotWrite(t *testing.T) {
	f := newRequestFixture(t)
	id := requestOpenActual(t, f)
	tx := requestTx(t, f.db)
	_, err := tx.ExecContext(t.Context(), `UPDATE account_pending_requests SET held_since=$2,held_ms=17 WHERE id=$1`, id, f.now)
	require.NoError(t, err)
	plan := requestEffectPlan{effects: []decide.Effect{{Kind: decide.EffectCancel, RequestID: id.String(), Reason: decide.CancelProof, CancellerClass: decide.C1}}}
	_, err = (requestStore{}).applyTx(t.Context(), tx, f.user, f.now.Add(time.Second), plan, preparedOpenings{})
	require.Error(t, err)
	var status string
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT status FROM account_pending_requests WHERE id=$1`, id).Scan(&status))
	require.Equal(t, "pending", status)
	_, err = tx.ExecContext(t.Context(), `DELETE FROM users WHERE id=$1`, f.user)
	require.NoError(t, err)
	var requests, origins int
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM account_pending_requests),(SELECT count(*) FROM account_pending_request_origins)`).Scan(&requests, &origins))
	require.Zero(t, requests)
	require.Zero(t, origins)
	require.NoError(t, tx.Rollback())
}

func TestRequestStoreRejectsMissingAuthorityInputsWithoutRows(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	_, err := (requestStore{}).loadTx(t.Context(), nil, uuid.New(), now)
	require.Error(t, err)
	_, err = (requestStore{}).applyTx(t.Context(), nil, uuid.New(), now, requestEffectPlan{}, preparedOpenings{})
	require.Error(t, err)
	f := newRequestFixture(t)
	tx := requestTx(t, f.db)
	for _, c := range []struct {
		user uuid.UUID
		now  time.Time
	}{{uuid.Nil, f.now}, {f.user, time.Time{}}} {
		_, err = (requestStore{}).loadTx(t.Context(), tx, c.user, c.now)
		require.Error(t, err)
		_, err = (requestStore{}).applyTx(t.Context(), tx, c.user, c.now, requestEffectPlan{}, preparedOpenings{})
		require.Error(t, err)
	}
	require.NoError(t, tx.Commit())
	require.Zero(t, requestCount(t, f.db))
}
