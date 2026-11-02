package accountguard

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
	"github.com/google/uuid"
)

// requestStore owns only request/origin statements. Its future coordinator owns
// the users lock, every other decision obligation, and transaction completion.
type requestStore struct{}
type requestSnapshot struct {
	Open          []decide.Req
	RecentCancels []decide.Req
	pending       map[uuid.UUID]requestPendingPayload
}

func validRequestScope(tx *sql.Tx, userID uuid.UUID, now time.Time) bool {
	return tx != nil && userID != uuid.Nil && validRequestTime(now)
}

func readRequestRows(ctx context.Context, tx *sql.Tx, userID uuid.UUID, now time.Time) (result []requestRow, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+requestColumns+` FROM account_pending_requests
WHERE user_id=$1 AND (status IN ('pending','disputed') OR
  (status='cancelled' AND cancel_reason IN ('proof','dispute_resolved') AND cancelled_at>$2))
ORDER BY id FOR UPDATE`, userID, now.Add(-decide.NCancelCP))
	if err != nil {
		return nil, fmt.Errorf("accountguard: query requests: %w", err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		row, err := scanRequestRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("accountguard: iterate requests: %w", err)
	}
	return result, nil
}

func (requestStore) loadTx(ctx context.Context, tx *sql.Tx, userID uuid.UUID, now time.Time) (requestSnapshot, error) {
	if !validRequestScope(tx, userID, now) {
		return requestSnapshot{}, errRequestShape
	}
	rows, err := readRequestRows(ctx, tx, userID, now)
	if err != nil {
		return requestSnapshot{}, err
	}
	snapshot := requestSnapshot{pending: make(map[uuid.UUID]requestPendingPayload)}
	for i := range rows {
		row := &rows[i]
		if err := loadRequestOrigins(ctx, tx, row); err != nil {
			return requestSnapshot{}, err
		}
		if row.req.Status == decide.StatusCancelled {
			if !validRequestCandidate(decide.Candidate{Keyed: row.req.Fingerprint, Coarse: row.req.Coarse, KeyID: row.req.FingerprintKeyID}) {
				return requestSnapshot{}, errRequestShape
			}
			snapshot.RecentCancels = append(snapshot.RecentCancels, row.req)
		} else {
			snapshot.Open = append(snapshot.Open, row.req)
			snapshot.pending[row.id] = cloneRequestPayload(row.payload)
		}
	}
	return snapshot, nil
}

func validatePreparedOpenings(plan requestEffectPlan, shape requestPlanShape, opens preparedOpenings, now time.Time) (uuid.UUID, error) {
	if err := validatePreparedPayload(shape, opens.payload); err != nil {
		return uuid.Nil, err
	}
	if (shape.opening != nil) != (opens.requestID != uuid.Nil) || shape.needsGroup != (opens.disputeGroupID != uuid.Nil) {
		return uuid.Nil, errRequestShape
	}
	if opens.requestID != uuid.Nil && opens.requestID == opens.disputeGroupID {
		return uuid.Nil, errRequestShape
	}
	if shape.opening != nil && !shape.opening.OpenedAt.Equal(now) {
		return uuid.Nil, errRequestShape
	}
	for _, e := range plan.effects {
		if !e.HeldSince.IsZero() && !e.HeldSince.Equal(now) {
			return uuid.Nil, errRequestShape
		}
		for _, id := range []uuid.UUID{opens.requestID, opens.disputeGroupID} {
			if id != uuid.Nil && (id.String() == e.RequestID || id.String() == e.Group) {
				return uuid.Nil, errRequestShape
			}
		}
	}
	if shape.needsGroup {
		return opens.disputeGroupID, nil
	}
	if shape.group != "" {
		return requestUUID(shape.group)
	}
	return uuid.Nil, nil
}

type requestChange struct {
	row    requestRow
	before decide.ReqStatus
}

func prepareRequestChanges(ctx context.Context, tx *sql.Tx, userID uuid.UUID, now time.Time, plan requestEffectPlan, group uuid.UUID) ([]requestChange, error) {
	transitions, holds := make(map[string]decide.Effect), make(map[string]decide.Effect)
	ids := make(map[string]bool)
	for _, e := range plan.effects {
		if e.Kind == decide.EffectOpen {
			continue
		}
		ids[e.RequestID] = true
		if e.Kind == decide.EffectHold {
			holds[e.RequestID] = e
		} else {
			transitions[e.RequestID] = e
		}
	}
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	changes := make([]requestChange, 0, len(ordered))
	// Lock the complete sorted request set before loading any child origins.
	for _, id := range ordered {
		row, err := scanRequestRow(tx.QueryRowContext(ctx, `SELECT `+requestColumns+`
FROM account_pending_requests WHERE user_id=$1 AND id=$2 FOR UPDATE`, userID, id).Scan)
		if err != nil {
			return nil, err
		}
		changes = append(changes, requestChange{row: row, before: row.req.Status})
	}
	for i := range changes {
		change := &changes[i]
		if err := loadRequestOrigins(ctx, tx, &change.row); err != nil {
			return nil, err
		}
		e, transition := transitions[change.row.req.ID]
		hold, folded := holds[change.row.req.ID]
		if err := stageRequestChange(&change.row, now, e, transition, hold, folded, group); err != nil {
			return nil, err
		}
	}
	return changes, nil
}

func stageRequestChange(row *requestRow, now time.Time, e decide.Effect, transition bool, hold decide.Effect, folded bool, group uuid.UUID) error {
	r := &row.req
	if r.Status != decide.StatusPending && r.Status != decide.StatusDisputed {
		return errRequestShape
	}
	if folded {
		if r.Status != decide.StatusPending || hold.HeldMs < r.HeldMs || (transition && !hold.HeldSince.IsZero()) {
			return errRequestShape
		}
		r.HeldMs = hold.HeldMs
	}
	if transition && !r.HeldSince.IsZero() && !folded {
		return errRequestShape
	}
	if folded {
		r.HeldSince = hold.HeldSince
	}
	if transition {
		switch e.Kind {
		case decide.EffectCancel:
			r.Status, r.CancelledAt, r.CancelReason, r.CancellerClass = decide.StatusCancelled, now, e.Reason, e.CancellerClass
			r.HeldSince, row.payload = time.Time{}, requestPendingPayload{}
			if e.Reason != decide.CancelProof && e.Reason != decide.CancelDisputeResolved {
				clearRequestFingerprint(r)
			}
		case decide.EffectDispute:
			if group == uuid.Nil {
				return errRequestShape
			}
			if r.Status == decide.StatusPending {
				r.DisputedAt = now
			}
			r.Status, r.DisputeGroup, r.HeldSince = decide.StatusDisputed, group.String(), time.Time{}
		case decide.EffectReturnToPending:
			if r.Status != decide.StatusDisputed {
				return errRequestShape
			}
			r.Status, r.DisputedAt, r.DisputeGroup = decide.StatusPending, time.Time{}, ""
		case decide.EffectComplete:
			if r.Status != decide.StatusPending {
				return errRequestShape
			}
			r.Status, row.completedAt, r.HeldSince = decide.StatusCompleted, now, time.Time{}
			row.payload = requestPendingPayload{}
			clearRequestFingerprint(r)
		default:
			return errRequestShape
		}
	}
	return validateRequestRow(*row)
}

func clearRequestFingerprint(r *decide.Req) {
	r.Fingerprint, r.Coarse, r.FingerprintKeyID = nil, nil, 0
}

func openingRequestRow(o decide.Opening, opens preparedOpenings, group uuid.UUID) (requestRow, error) {
	digest, err := originFingerprint(o.Origins)
	if err != nil {
		return requestRow{}, err
	}
	row := requestRow{id: opens.requestID, completedAt: o.CompletedAt, originDigest: digest[:], payload: cloneRequestPayload(opens.payload),
		req: decide.Req{ID: opens.requestID.String(), Kind: o.Kind, Status: o.Status, OriginClass: o.OriginClass,
			Origins: o.Origins, Targets: o.Targets, ResetFactors: o.ResetFactors, OpenedAt: o.OpenedAt,
			CompletesAt: o.CompletesAt, DisputedAt: o.DisputedAt, Fingerprint: o.Candidate.Keyed,
			Coarse: o.Candidate.Coarse, FingerprintKeyID: o.Candidate.KeyID}}
	if o.Status == decide.StatusDisputed {
		row.req.DisputeGroup = group.String()
	}
	return row, validateRequestRow(row)
}

// applyTx never accepts a complete Outcome. Success covers these rows only and
// leaves both commit and all retained obligations with the caller.
func (requestStore) applyTx(ctx context.Context, tx *sql.Tx, userID uuid.UUID, now time.Time, plan requestEffectPlan, opens preparedOpenings) (requestApplyResult, error) {
	if !validRequestScope(tx, userID, now) {
		return requestApplyResult{}, errRequestShape
	}
	shape, err := validateRequestPlan(plan)
	if err != nil {
		return requestApplyResult{}, err
	}
	group, err := validatePreparedOpenings(plan, shape, opens, now)
	if err != nil {
		return requestApplyResult{}, err
	}
	var opening requestRow
	if shape.opening != nil {
		opening, err = openingRequestRow(*shape.opening, opens, group)
		if err != nil {
			return requestApplyResult{}, err
		}
	}
	changes, err := prepareRequestChanges(ctx, tx, userID, now, plan, group)
	if err != nil {
		return requestApplyResult{}, err
	}
	// Every shape and referenced row is checked before the first write. Existing
	// transitions precede an insert that might claim the pending-breakglass index.
	for _, change := range changes {
		if err := updateRequestRow(ctx, tx, userID, change); err != nil {
			return requestApplyResult{}, err
		}
	}
	if shape.opening != nil {
		if err := insertRequestRow(ctx, tx, userID, opening); err != nil {
			return requestApplyResult{}, err
		}
	}
	return requestApplyResult{openedRequestID: opens.requestID, disputeGroupID: group}, nil
}

func requestAffected(result sql.Result, err error) error {
	if err != nil {
		return fmt.Errorf("accountguard: write request component: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("accountguard: count request write: %w", err)
	}
	if n != 1 {
		return errRequestShape
	}
	return nil
}

func updateRequestRow(ctx context.Context, tx *sql.Tx, userID uuid.UUID, change requestChange) error {
	r := change.row.req
	_, status, _, reason, err := requestRowText(change.row)
	if err != nil {
		return err
	}
	before, err := change.before.MarshalText()
	if err != nil {
		return err
	}
	return requestAffected(tx.ExecContext(ctx, `UPDATE account_pending_requests SET
status=$3,held_ms=$4,held_since=$5,disputed_at=$6,dispute_group_id=$7,completed_at=$8,
cancelled_at=$9,cancel_reason=$10,canceller_class=$11,pending_password_hash=$12,pending_address=$13,
request_fingerprint=$14,coarse_fingerprint=$15,fingerprint_key_id=$16
WHERE id=$1 AND user_id=$2 AND status=$17`, change.row.id, userID, status, r.HeldMs,
		requestNullTime(r.HeldSince), requestNullTime(r.DisputedAt), requestNullString(r.DisputeGroup),
		requestNullTime(change.row.completedAt), requestNullTime(r.CancelledAt), requestNullString(reason),
		sql.NullInt64{Int64: int64(r.CancellerClass), Valid: r.CancellerClass != decide.ClassNone},
		requestPayloadSQL(change.row.payload.passwordHash), requestPayloadSQL(change.row.payload.address),
		r.Fingerprint, r.Coarse, sql.NullInt64{Int64: int64(r.FingerprintKeyID), Valid: r.FingerprintKeyID != 0}, string(before)))
}

func insertRequestRow(ctx context.Context, tx *sql.Tx, userID uuid.UUID, row requestRow) error {
	r := row.req
	kind, status, targetKind, _, err := requestRowText(row)
	if err != nil {
		return err
	}
	var targetID sql.NullString
	var targetVersion sql.NullInt64
	if len(r.Targets) > 0 {
		targetID = requestNullString(r.Targets[0].ID)
		targetVersion = sql.NullInt64{Int64: r.Targets[0].Version, Valid: true}
	}
	err = requestAffected(tx.ExecContext(ctx, `INSERT INTO account_pending_requests
(id,user_id,kind,target_kind,target_id,target_material_version,reset_factors,status,origin_class,
origin_fingerprint,request_fingerprint,coarse_fingerprint,fingerprint_key_id,dispute_group_id,
opened_at,completes_at,held_ms,held_since,disputed_at,completed_at,cancelled_at,cancel_reason,
canceller_class,pending_password_hash,pending_address)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,0,NULL,$17,$18,NULL,NULL,NULL,$19,$20)`,
		row.id, userID, kind, requestNullString(targetKind), targetID, targetVersion, r.ResetFactors,
		status, int16(r.OriginClass), row.originDigest, r.Fingerprint, r.Coarse,
		sql.NullInt64{Int64: int64(r.FingerprintKeyID), Valid: r.FingerprintKeyID != 0}, requestNullString(r.DisputeGroup),
		r.OpenedAt, r.CompletesAt, requestNullTime(r.DisputedAt), requestNullTime(row.completedAt),
		requestPayloadSQL(row.payload.passwordHash), requestPayloadSQL(row.payload.address)))
	if err != nil {
		return err
	}
	for _, ref := range r.Origins {
		kind, err := ref.Kind.MarshalText()
		if err != nil {
			return err
		}
		if err := requestAffected(tx.ExecContext(ctx, `INSERT INTO account_pending_request_origins
(request_id,credential_kind,credential_id,material_version) VALUES ($1,$2,$3,$4)`, row.id, string(kind), ref.ID, ref.Version)); err != nil {
			return err
		}
	}
	return nil
}
