package accountguard

import (
	"context"
	"database/sql"
	"errors"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"os"
	"testing"
	"time"
)

// Exact prospective two-table contract only. This is not a numbered migration.
const requestFixtureDDL = `CREATE TABLE account_pending_requests (
  id uuid PRIMARY KEY,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  kind text NOT NULL,
  target_kind text,
  target_id text,
  target_material_version bigint,
  reset_factors boolean NOT NULL DEFAULT false,
  status text NOT NULL,
  origin_class smallint NOT NULL,
  origin_fingerprint bytea NOT NULL,
  request_fingerprint bytea,
  coarse_fingerprint bytea,
  fingerprint_key_id smallint,
  dispute_group_id uuid,
  opened_at timestamptz NOT NULL,
  completes_at timestamptz NOT NULL,
  held_ms bigint NOT NULL DEFAULT 0,
  held_since timestamptz,
  disputed_at timestamptz,
  completed_at timestamptz,
  cancelled_at timestamptz,
  cancel_reason text,
  canceller_class smallint,
  pending_password_hash text,
  pending_address text
);
ALTER TABLE account_pending_requests
  ADD CONSTRAINT ag_request_kind CHECK
    (kind IN ('credential_remove','address_change','break_glass')) NOT VALID,
  ADD CONSTRAINT ag_request_status CHECK
    (status IN ('pending','disputed','completed','cancelled')) NOT VALID,
  ADD CONSTRAINT ag_request_class CHECK (origin_class BETWEEN 1 AND 3) NOT VALID,
  ADD CONSTRAINT ag_request_target CHECK (
    (kind = 'break_glass' AND num_nonnulls(target_kind,target_id,target_material_version) = 0)
    OR (kind <> 'break_glass' AND num_nonnulls(target_kind,target_id,target_material_version) = 3
      AND target_id <> '' AND target_material_version > 0
      AND ((kind = 'address_change' AND target_kind IN ('address','backup_address'))
        OR (kind = 'credential_remove' AND target_kind IN
          ('backup_address','sso','email_mfa','totp','webauthn','backup_code_set','recovery_key','recovery_device'))))
  ) NOT VALID,
  ADD CONSTRAINT ag_request_reset CHECK (NOT reset_factors OR kind = 'break_glass') NOT VALID,
  ADD CONSTRAINT ag_request_origin_digest CHECK (octet_length(origin_fingerprint) = 32) NOT VALID,
  ADD CONSTRAINT ag_request_fingerprint_shape CHECK (
    num_nonnulls(request_fingerprint,coarse_fingerprint,fingerprint_key_id) IN (0,3)
    AND (request_fingerprint IS NULL OR
      (octet_length(request_fingerprint) = 32 AND octet_length(coarse_fingerprint) = 32 AND fingerprint_key_id > 0))
  ) NOT VALID,
  ADD CONSTRAINT ag_request_fingerprint_state CHECK (
    (status IN ('pending','disputed') AND request_fingerprint IS NOT NULL)
    OR (status = 'completed' AND request_fingerprint IS NULL)
    OR (status = 'cancelled' AND
      (cancel_reason IN ('proof','dispute_resolved') OR request_fingerprint IS NULL))
  ) NOT VALID,
  ADD CONSTRAINT ag_request_group CHECK (
    (num_nonnulls(disputed_at,dispute_group_id) = 0 OR num_nonnulls(disputed_at,dispute_group_id) = 2)
    AND (status <> 'disputed' OR disputed_at IS NOT NULL)
    AND (status <> 'pending' OR disputed_at IS NULL)
  ) NOT VALID,
  ADD CONSTRAINT ag_request_terminal_time CHECK (
    (status = 'completed') = (completed_at IS NOT NULL)
    AND (status = 'cancelled') = (cancelled_at IS NOT NULL)
    AND (status = 'cancelled') = (cancel_reason IS NOT NULL)
  ) NOT VALID,
  ADD CONSTRAINT ag_request_cancel CHECK (COALESCE((
    (cancel_reason IS NULL AND canceller_class IS NULL)
    OR (cancel_reason = 'proof' AND canceller_class IS NOT NULL AND canceller_class BETWEEN 1 AND 3)
    OR (cancel_reason = 'dispute_resolved' AND canceller_class IS NOT NULL AND canceller_class = 3)
    OR (cancel_reason IN ('superseded','hold_cap','dispute_expired') AND canceller_class IS NULL)
  ), false)) NOT VALID,
  ADD CONSTRAINT ag_request_hold CHECK
    (held_ms >= 0 AND (held_since IS NULL OR status = 'pending')) NOT VALID,
  ADD CONSTRAINT ag_request_payload CHECK (
    (status IN ('completed','cancelled') AND pending_password_hash IS NULL AND pending_address IS NULL)
    OR (status IN ('pending','disputed') AND (
      (kind = 'credential_remove' AND pending_password_hash IS NULL AND pending_address IS NULL)
      OR (kind = 'break_glass' AND pending_password_hash IS NOT NULL AND pending_password_hash <> '' AND pending_address IS NULL)
      OR (kind = 'address_change' AND pending_password_hash IS NULL AND pending_address IS NOT NULL AND pending_address <> '')))
  ) NOT VALID,
  ADD CONSTRAINT ag_request_finite_time CHECK (
    isfinite(opened_at) AND isfinite(completes_at)
    AND (held_since IS NULL OR isfinite(held_since))
    AND (disputed_at IS NULL OR isfinite(disputed_at))
    AND (completed_at IS NULL OR isfinite(completed_at))
    AND (cancelled_at IS NULL OR isfinite(cancelled_at))
  ) NOT VALID,
  ADD CONSTRAINT ag_request_due_time CHECK (
    completes_at >= opened_at + interval '72 hours'
    OR (kind = 'break_glass' AND status = 'completed' AND origin_class = 3
      AND completes_at = opened_at AND completed_at = opened_at)
  ) NOT VALID;
CREATE INDEX ag_request_due_idx ON account_pending_requests(completes_at) WHERE status = 'pending';
CREATE INDEX ag_request_user_status_idx ON account_pending_requests(user_id,status);
CREATE INDEX ag_request_recent_cancel_idx ON account_pending_requests(user_id,request_fingerprint,cancelled_at)
  WHERE cancel_reason IN ('proof','dispute_resolved');
CREATE UNIQUE INDEX ag_request_one_pending_breakglass ON account_pending_requests(user_id)
  WHERE kind = 'break_glass' AND status = 'pending';
CREATE INDEX ag_request_key_idx ON account_pending_requests(fingerprint_key_id);

CREATE TABLE account_pending_request_origins (
  request_id uuid NOT NULL REFERENCES account_pending_requests(id) ON DELETE CASCADE,
  credential_kind text NOT NULL,
  credential_id text NOT NULL,
  material_version bigint NOT NULL,
  PRIMARY KEY (request_id,credential_kind,credential_id,material_version)
);
ALTER TABLE account_pending_request_origins
  ADD CONSTRAINT ag_origin_kind CHECK (credential_kind IN
    ('password','address','backup_address','sso','email_mfa','totp','webauthn','backup_code_set','recovery_key','recovery_device')) NOT VALID,
  ADD CONSTRAINT ag_origin_identity CHECK (credential_id <> '' AND material_version > 0) NOT VALID;`

type requestFixture struct {
	db   *sql.DB
	user uuid.UUID
	now  time.Time
}

func newRequestFixture(t *testing.T) requestFixture {
	t.Helper()
	require.NotEmpty(t, os.Getenv("DATABASE_URL"), "explicit disposable DATABASE_URL required")
	db, _ := testdb.SetupTestDB(t)
	_, err := db.ExecContext(t.Context(), `DROP TABLE IF EXISTS account_pending_request_origins; DROP TABLE IF EXISTS account_pending_requests`)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), requestFixtureDDL)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, e := db.ExecContext(context.Background(), `DROP TABLE IF EXISTS account_pending_request_origins; DROP TABLE IF EXISTS account_pending_requests`)
		require.NoError(t, e)
	})
	user := testdb.CreateUser(t, db)
	var now time.Time
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT clock_timestamp()`).Scan(&now))
	return requestFixture{db, user, now}
}
func requestTx(t *testing.T, db *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	t.Cleanup(func() {
		e := tx.Rollback()
		if !errors.Is(e, sql.ErrTxDone) {
			require.NoError(t, e)
		}
	})
	return tx
}
func requestCandidate() decide.Candidate {
	return decide.Candidate{Keyed: []byte("01234567890123456789012345678901"), Coarse: []byte("abcdefghijklmnopqrstuvwxyzABCDEF"), KeyID: 7}
}
func requestOrigin() decide.CredRef {
	return decide.CredRef{Kind: decide.CredPassword, ID: "opaque-password-slot", Version: 11}
}
func requestOpening(now time.Time) decide.Opening {
	return decide.Opening{Kind: decide.ReqBreakGlass, Status: decide.StatusPending, Origins: []decide.CredRef{requestOrigin()}, OriginClass: decide.C1, OpenedAt: now, CompletesAt: now.Add(decide.DeltaCP), Candidate: requestCandidate()}
}
func requestPayload() requestPendingPayload {
	s := "synthetic-opaque-argon-hash"
	return requestPendingPayload{passwordHash: &s}
}
func requestApply(t *testing.T, tx *sql.Tx, f requestFixture, outcome decide.Outcome, payload requestPendingPayload) (requestApplyResult, decide.Outcome) {
	t.Helper()
	plan, remainder, err := partitionRequestEffects(outcome)
	require.NoError(t, err)
	prepared, err := prepareRequestOpenings(plan, payload, uuid.NewRandom)
	require.NoError(t, err)
	result, err := (requestStore{}).applyTx(t.Context(), tx, f.user, f.now, plan, prepared)
	require.NoError(t, err)
	expected := outcome
	expected.Effects = remainder.Effects
	require.Equal(t, expected, remainder, "every non-effect obligation remains coordinator-owned")
	return result, remainder
}
func requestCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM account_pending_requests`).Scan(&n))
	return n
}
