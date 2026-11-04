package accountguard

import (
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
	"github.com/stretchr/testify/require"
)

// These rows model a damaged or older schema whose CHECKs did not protect the
// loader. Each transaction drops only the fixture constraints needed to write
// its corrupt row, and rollback restores both schema and baseline request.
func TestRequestStoreLoadRejectsMalformedPersistedRows(t *testing.T) {
	f := newRequestFixture(t)
	id := requestOpenActual(t, f)
	baselineTx := requestTx(t, f.db)
	baseline, err := (requestStore{}).loadTx(t.Context(), baselineTx, f.user, f.now)
	require.NoError(t, err)
	require.Len(t, baseline.Open, 1)
	require.NoError(t, baselineTx.Rollback())

	cases := []struct {
		name       string
		dropChecks []string
		corruptSQL string
	}{
		{"partial target", []string{"ag_request_target"},
			`UPDATE account_pending_requests SET target_kind='address' WHERE id=$1`},
		{"unknown target kind", []string{"ag_request_target"},
			`UPDATE account_pending_requests SET target_kind='future-kind',target_id='opaque-target',target_material_version=17 WHERE id=$1`},
		{"unknown kind", []string{"ag_request_kind", "ag_request_target", "ag_request_reset", "ag_request_payload"},
			`UPDATE account_pending_requests SET kind='future-kind' WHERE id=$1`},
		{"unknown status", []string{"ag_request_status", "ag_request_fingerprint_state", "ag_request_payload"},
			`UPDATE account_pending_requests SET status='future-status' WHERE id=$1`},
		{"zero key generation", []string{"ag_request_fingerprint_shape"},
			`UPDATE account_pending_requests SET fingerprint_key_id=0 WHERE id=$1`},
		{"partial fingerprint triple", []string{"ag_request_fingerprint_shape"},
			`UPDATE account_pending_requests SET coarse_fingerprint=NULL WHERE id=$1`},
		{"empty origin digest", []string{"ag_request_origin_digest"},
			`UPDATE account_pending_requests SET origin_fingerprint='' WHERE id=$1`},
		{"negative held milliseconds", []string{"ag_request_hold"},
			`UPDATE account_pending_requests SET held_ms=-1 WHERE id=$1`},
		{"group without dispute time", []string{"ag_request_group"},
			`UPDATE account_pending_requests SET dispute_group_id='11111111-1111-4111-8111-111111111111' WHERE id=$1`},
		{"cancel with empty reason", []string{"ag_request_cancel"},
			`UPDATE account_pending_requests SET status='cancelled',cancelled_at=opened_at,cancel_reason='',canceller_class=1,pending_password_hash=NULL,request_fingerprint=NULL,coarse_fingerprint=NULL,fingerprint_key_id=NULL WHERE id=$1`}, // pragma: allowlist secret -- SQL corruption fixture clears the password-hash column to NULL.
		{"cancel with unknown reason", []string{"ag_request_cancel"},
			`UPDATE account_pending_requests SET status='cancelled',cancelled_at=opened_at,cancel_reason='future-reason',canceller_class=1,pending_password_hash=NULL,request_fingerprint=NULL,coarse_fingerprint=NULL,fingerprint_key_id=NULL WHERE id=$1`}, // pragma: allowlist secret -- SQL corruption fixture clears the password-hash column to NULL.
		{"cancel with invalid class", []string{"ag_request_cancel"},
			`UPDATE account_pending_requests SET status='cancelled',cancelled_at=opened_at,cancel_reason='proof',canceller_class=4,pending_password_hash=NULL WHERE id=$1`}, // pragma: allowlist secret -- SQL corruption fixture clears the password-hash column to NULL.
		{"completed with fingerprint", []string{"ag_request_fingerprint_state"},
			`UPDATE account_pending_requests SET status='completed',completed_at=opened_at,pending_password_hash=NULL WHERE id=$1`}, // pragma: allowlist secret -- SQL corruption fixture clears the password-hash column to NULL.
		{"completion before due time", []string{"ag_request_due_time"},
			`UPDATE account_pending_requests SET completes_at=opened_at + interval '1 hour' WHERE id=$1`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx := requestTx(t, f.db)
			for _, check := range tc.dropChecks {
				_, err := tx.ExecContext(t.Context(), `ALTER TABLE account_pending_requests DROP CONSTRAINT `+check)
				require.NoError(t, err)
			}
			_, err := tx.ExecContext(t.Context(), tc.corruptSQL, id)
			require.NoError(t, err, "fixture must create the intended corrupt row")
			// Terminal and unknown-status rows are intentionally outside loadTx's
			// recent/open selector, but the same decoder must reject them whenever
			// a transition reads one by ID.
			_, err = scanRequestRow(tx.QueryRowContext(t.Context(),
				`SELECT id,kind,target_kind,target_id,target_material_version,reset_factors,
status,origin_class,origin_fingerprint,request_fingerprint,coarse_fingerprint,fingerprint_key_id,
dispute_group_id,opened_at,completes_at,held_ms,held_since,disputed_at,completed_at,cancelled_at,
cancel_reason,canceller_class,pending_password_hash,pending_address
FROM account_pending_requests WHERE id=$1`, id).Scan)
			require.ErrorIs(t, err, errRequestShape, "corrupt persisted state must not become a partial snapshot")
			require.NoError(t, tx.Rollback())
		})
	}

	controlTx := requestTx(t, f.db)
	control, err := (requestStore{}).loadTx(t.Context(), controlTx, f.user, f.now)
	require.NoError(t, err)
	require.Equal(t, baseline.Open, control.Open, "each rollback must restore the valid persisted row")
	require.NoError(t, controlTx.Rollback())
}

func TestRequestStorePreflightsEveryTargetBeforeFirstTransitionWrite(t *testing.T) {
	f := newRequestFixture(t)
	opening := requestOpening(f.now)
	opening.Kind = decide.ReqAddressChange
	opening.Targets = []decide.CredRef{{Kind: decide.CredAddress, ID: "first-address-slot", Version: 37}}
	firstAddress := "first@example.invalid"
	secondAddress := "second@example.invalid"
	tx := requestTx(t, f.db)
	first, _ := requestApply(t, tx, f, decide.Outcome{Effects: []decide.Effect{{Kind: decide.EffectOpen, Open: opening}}}, requestPendingPayload{address: &firstAddress})
	opening.Targets[0].ID = "second-address-slot"
	second, _ := requestApply(t, tx, f, decide.Outcome{Effects: []decide.Effect{{Kind: decide.EffectOpen, Open: opening}}}, requestPendingPayload{address: &secondAddress})
	goodID, badID := first.openedRequestID, second.openedRequestID
	if goodID.String() > badID.String() {
		goodID, badID = badID, goodID
	}
	// The later sorted target has a digest mismatch; the earlier one is fully
	// valid. Its cancellation must not be written before all target rows load.
	_, err := tx.ExecContext(t.Context(), `UPDATE account_pending_request_origins SET material_version=material_version+1 WHERE request_id=$1`, badID)
	require.NoError(t, err)
	plan := requestEffectPlan{effects: []decide.Effect{
		{Kind: decide.EffectCancel, RequestID: goodID.String(), Reason: decide.CancelProof, CancellerClass: decide.C1},
		{Kind: decide.EffectCancel, RequestID: badID.String(), Reason: decide.CancelProof, CancellerClass: decide.C1},
	}}
	_, err = (requestStore{}).applyTx(t.Context(), tx, f.user, f.now, plan, preparedOpenings{})
	require.ErrorIs(t, err, errRequestShape)
	var pending int
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT count(*) FROM account_pending_requests WHERE status='pending'`).Scan(&pending))
	require.Equal(t, 2, pending, "no target transition may write before the complete preflight")
	// Repairing the same caller transaction proves the refusal was the later
	// target's corrupt origin, rather than an unrelated SQL write failure.
	_, err = tx.ExecContext(t.Context(), `UPDATE account_pending_request_origins SET material_version=material_version-1 WHERE request_id=$1`, badID)
	require.NoError(t, err)
	_, err = (requestStore{}).applyTx(t.Context(), tx, f.user, f.now, plan, preparedOpenings{})
	require.NoError(t, err)
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT count(*) FROM account_pending_requests WHERE status='pending'`).Scan(&pending))
	require.Zero(t, pending)
	require.NoError(t, tx.Rollback())
}
