package accountguard

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
	"github.com/google/uuid"
)

type requestRow struct {
	id           uuid.UUID
	req          decide.Req
	completedAt  time.Time
	originDigest []byte
	payload      requestPendingPayload
}

func scanRequestRow(scan func(...any) error) (requestRow, error) {
	var row requestRow
	var kind, status string
	var targetKind, targetID, group, reason, hash, address sql.NullString
	var targetVersion, keyID, canceller sql.NullInt64
	var class int64
	var held, disputed, completed, cancelled sql.NullTime
	err := scan(&row.id, &kind, &targetKind, &targetID, &targetVersion, &row.req.ResetFactors,
		&status, &class, &row.originDigest, &row.req.Fingerprint, &row.req.Coarse, &keyID,
		&group, &row.req.OpenedAt, &row.req.CompletesAt, &row.req.HeldMs, &held, &disputed,
		&completed, &cancelled, &reason, &canceller, &hash, &address)
	if err != nil {
		return requestRow{}, fmt.Errorf("accountguard: read request row: %w", err)
	}
	if row.id == uuid.Nil || class < 1 || class > 3 {
		return requestRow{}, errRequestShape
	}
	row.req.ID, row.req.OriginClass = row.id.String(), decide.Class(class)
	if err := row.req.Kind.UnmarshalText([]byte(kind)); err != nil {
		return requestRow{}, errRequestShape
	}
	if err := row.req.Status.UnmarshalText([]byte(status)); err != nil {
		return requestRow{}, errRequestShape
	}
	if err := decodeRequestTarget(&row, targetKind, targetID, targetVersion); err != nil {
		return requestRow{}, err
	}
	if err := decodeRequestFingerprint(&row, keyID); err != nil {
		return requestRow{}, err
	}
	if err := decodeRequestGroup(&row, group); err != nil {
		return requestRow{}, err
	}
	if err := decodeRequestTimes(&row, held, disputed, completed, cancelled); err != nil {
		return requestRow{}, err
	}
	if err := decodeRequestCancellation(&row, reason, canceller); err != nil {
		return requestRow{}, err
	}
	if hash.Valid {
		row.payload.passwordHash = &hash.String
	}
	if address.Valid {
		row.payload.address = &address.String
	}
	if err := validateRequestRow(row); err != nil {
		return requestRow{}, err
	}
	return row, nil
}

func decodeRequestTarget(row *requestRow, targetKind, targetID sql.NullString, targetVersion sql.NullInt64) error {
	if targetKind.Valid != targetID.Valid || targetKind.Valid != targetVersion.Valid {
		return errRequestShape
	}
	if targetKind.Valid {
		ref := decide.CredRef{ID: targetID.String, Version: targetVersion.Int64}
		if err := ref.Kind.UnmarshalText([]byte(targetKind.String)); err != nil {
			return errRequestShape
		}
		row.req.Targets = []decide.CredRef{ref}
	}
	return nil
}

func decodeRequestFingerprint(row *requestRow, keyID sql.NullInt64) error {
	if keyID.Valid {
		if keyID.Int64 <= 0 || keyID.Int64 > 32767 {
			return errRequestShape
		}
		row.req.FingerprintKeyID = int16(keyID.Int64)
	}
	// NULL is different from an empty digest or a zero key ID.
	if (row.req.Fingerprint != nil) != keyID.Valid || (row.req.Coarse != nil) != keyID.Valid {
		return errRequestShape
	}
	return nil
}

func decodeRequestGroup(row *requestRow, group sql.NullString) error {
	if group.Valid {
		if _, err := requestUUID(group.String); err != nil {
			return err
		}
		row.req.DisputeGroup = group.String
	}
	return nil
}

func decodeRequestTimes(row *requestRow, held, disputed, completed, cancelled sql.NullTime) error {
	if held.Valid {
		row.req.HeldSince = held.Time
	}
	if disputed.Valid {
		row.req.DisputedAt = disputed.Time
	}
	if completed.Valid {
		row.completedAt = completed.Time
	}
	if cancelled.Valid {
		row.req.CancelledAt = cancelled.Time
	}
	// A valid SQL timestamp cannot be silently mapped into the pure zero sentinel.
	for _, v := range []sql.NullTime{held, disputed, completed, cancelled} {
		if v.Valid && !validRequestTime(v.Time) {
			return errRequestShape
		}
	}
	return nil
}

func decodeRequestCancellation(row *requestRow, reason sql.NullString, canceller sql.NullInt64) error {
	if reason.Valid {
		if reason.String == "" {
			return errRequestShape
		}
		if err := row.req.CancelReason.UnmarshalText([]byte(reason.String)); err != nil || row.req.CancelReason == decide.CancelNone {
			return errRequestShape
		}
	}
	if canceller.Valid {
		if canceller.Int64 < 1 || canceller.Int64 > 3 {
			return errRequestShape
		}
		row.req.CancellerClass = decide.Class(canceller.Int64)
	}
	return nil
}

func validateRequestRow(row requestRow) error {
	r := row.req
	if row.id == uuid.Nil || !validRequestTargets(r.Kind, r.Targets, r.ResetFactors) ||
		r.OriginClass < decide.C1 || r.OriginClass > decide.C3 || len(row.originDigest) != 32 ||
		!validRequestTime(r.OpenedAt) || !validRequestTime(r.CompletesAt) || r.HeldMs < 0 ||
		!validRequestPayload(r.Kind, r.Status, row.payload) {
		return errRequestShape
	}
	if err := validateRequestRowTimes(row); err != nil {
		return err
	}
	if err := validateRequestRowDispute(row); err != nil {
		return err
	}
	if err := validateRequestRowCancellation(row); err != nil {
		return err
	}
	if err := validateRequestRowFingerprint(row); err != nil {
		return err
	}
	if r.CompletesAt.Before(r.OpenedAt.Add(decide.DeltaCP)) &&
		(r.Kind != decide.ReqBreakGlass || r.Status != decide.StatusCompleted || r.OriginClass != decide.C3 || !r.CompletesAt.Equal(r.OpenedAt) || !row.completedAt.Equal(r.OpenedAt)) {
		return errRequestShape
	}
	return nil
}

func validateRequestRowTimes(row requestRow) error {
	r := row.req
	for _, t := range []time.Time{r.HeldSince, r.DisputedAt, row.completedAt, r.CancelledAt} {
		if !t.IsZero() && !validRequestTime(t) {
			return errRequestShape
		}
	}
	if !r.HeldSince.IsZero() && r.Status != decide.StatusPending {
		return errRequestShape
	}
	return nil
}

func validateRequestRowDispute(row requestRow) error {
	r := row.req
	if r.DisputedAt.IsZero() != (r.DisputeGroup == "") {
		return errRequestShape
	}
	if r.DisputeGroup != "" {
		if _, err := requestUUID(r.DisputeGroup); err != nil {
			return err
		}
	}
	if r.Status == decide.StatusPending && !r.DisputedAt.IsZero() {
		return errRequestShape
	}
	if r.Status == decide.StatusDisputed && r.DisputedAt.IsZero() {
		return errRequestShape
	}
	return nil
}

func validateRequestRowCancellation(row requestRow) error {
	r := row.req
	if (r.Status == decide.StatusCompleted) != !row.completedAt.IsZero() ||
		(r.Status == decide.StatusCancelled) != !r.CancelledAt.IsZero() {
		return errRequestShape
	}
	if r.Status == decide.StatusCancelled {
		if !validRequestCancel(r.CancelReason, r.CancellerClass) {
			return errRequestShape
		}
	} else if r.CancelReason != decide.CancelNone || r.CancellerClass != decide.ClassNone {
		return errRequestShape
	}
	return nil
}

func validateRequestRowFingerprint(row requestRow) error {
	r := row.req
	hasFingerprint := r.Fingerprint != nil || r.Coarse != nil || r.FingerprintKeyID != 0
	if hasFingerprint && !validRequestCandidate(decide.Candidate{Keyed: r.Fingerprint, Coarse: r.Coarse, KeyID: r.FingerprintKeyID}) {
		return errRequestShape
	}
	switch r.Status {
	case decide.StatusPending, decide.StatusDisputed:
		if !hasFingerprint {
			return errRequestShape
		}
	case decide.StatusCompleted:
		if hasFingerprint {
			return errRequestShape
		}
	case decide.StatusCancelled:
		if hasFingerprint && r.CancelReason != decide.CancelProof && r.CancelReason != decide.CancelDisputeResolved {
			return errRequestShape
		}
	default:
		return errRequestShape
	}
	return nil
}

func loadRequestOrigins(ctx context.Context, tx *sql.Tx, row *requestRow) (err error) {
	rows, err := tx.QueryContext(ctx, `SELECT credential_kind,credential_id,material_version
FROM account_pending_request_origins WHERE request_id=$1
ORDER BY CASE credential_kind WHEN 'password' THEN 1 WHEN 'address' THEN 2
WHEN 'backup_address' THEN 3 WHEN 'sso' THEN 4 WHEN 'email_mfa' THEN 5 WHEN 'totp' THEN 6
WHEN 'webauthn' THEN 7 WHEN 'backup_code_set' THEN 8 WHEN 'recovery_key' THEN 9
WHEN 'recovery_device' THEN 10 ELSE 0 END,credential_id COLLATE "C",material_version`, row.id)
	if err != nil {
		return fmt.Errorf("accountguard: query request origins: %w", err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	var origins []decide.CredRef
	for rows.Next() {
		var kind string
		var ref decide.CredRef
		if err := rows.Scan(&kind, &ref.ID, &ref.Version); err != nil {
			return fmt.Errorf("accountguard: read request origin: %w", err)
		}
		if err := ref.Kind.UnmarshalText([]byte(kind)); err != nil {
			return errRequestShape
		}
		origins = append(origins, ref)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("accountguard: iterate request origins: %w", err)
	}
	digest, err := originFingerprint(origins)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(digest[:], row.originDigest) != 1 {
		return errRequestShape
	}
	row.req.Origins = origins
	return nil
}

func requestNullTime(t time.Time) sql.NullTime  { return sql.NullTime{Time: t, Valid: !t.IsZero()} }
func requestNullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
func requestPayloadSQL(s *string) sql.NullString {
	if s == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *s, Valid: true}
}

// Rows have already passed closed-enum validation; errors still propagate rather
// than silently serializing an unknown value into a different SQL state.
func requestRowText(row requestRow) (kind, status, targetKind, reason string, err error) {
	k, err := row.req.Kind.MarshalText()
	if err != nil {
		return "", "", "", "", err
	}
	s, err := row.req.Status.MarshalText()
	if err != nil {
		return "", "", "", "", err
	}
	r, err := row.req.CancelReason.MarshalText()
	if err != nil {
		return "", "", "", "", err
	}
	if len(row.req.Targets) > 0 {
		t, err := row.req.Targets[0].Kind.MarshalText()
		if err != nil {
			return "", "", "", "", err
		}
		targetKind = string(t)
	}
	return string(k), string(s), targetKind, string(r), nil
}
