package presencehistory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/google/uuid"
)

const (
	pendingOperationGrace = 30 * time.Second
	commitReadbackTimeout = 3 * time.Second
	senderGateStripes     = 64
)

// ErrPendingOperationEligible tells the caller to roll back and hand the
// eligible marker to reconciliation before starting a fresh operation.
var ErrPendingOperationEligible = errors.New("pending presence operation requires reconciliation")

// OperationMode distinguishes ordinary audience writes from privacy-narrowing
// security clears that may supersede an existing pending operation.
type OperationMode uint8

const (
	// OrdinaryAudienceWrite must yield to an existing pending marker.
	OrdinaryAudienceWrite OperationMode = iota
	// ForcedSecurityClear may supersede a pending marker to narrow exposure.
	ForcedSecurityClear
)

// DeliveryMode selects exact audience reconciliation or a conservative reset.
type DeliveryMode uint8

const (
	// DeliveryExactDelta reconciles the prepared pre/post audience difference.
	DeliveryExactDelta DeliveryMode = iota
	// DeliveryConservativeReset clears or disconnects every possibly stale client.
	DeliveryConservativeReset
)

// AudienceOperation is the complete durable evidence needed to classify the
// main transaction and perform acknowledged delivery.
type AudienceOperation struct {
	ID               uuid.UUID
	SenderID         uuid.UUID
	PriorVersion     int64
	Version          int64
	PriorOperationID *uuid.UUID
	ReconcileAfter   time.Time
	Before           CustomTextState
	BeforeTier       int
}

// DeliveryPlan contains prepared, non-locking delivery inputs.
type DeliveryPlan struct {
	Mode             DeliveryMode
	OperationID      uuid.UUID
	SenderID         uuid.UUID
	ClearRecipients  map[uuid.UUID]bool
	UpdateRecipients map[uuid.UUID]bool
	Payload          *CustomTextState
	OverrideVersion  *int
}

// DeliveryAck authorizes marker removal only for the matching operation.
type DeliveryAck struct {
	OperationID uuid.UUID
}

// Delivery performs bounded Custom Status delivery and returns its exact ack.
type Delivery interface {
	DeliverCustomText(context.Context, DeliveryPlan) (DeliveryAck, error)
}

// CommitOutcome classifies an ambiguous main-transaction commit.
type CommitOutcome uint8

const (
	// CommitConfirmed means the exact attempted settings and pending marker are durable.
	CommitConfirmed CommitOutcome = iota
	// RollbackConfirmed means the exact prior version, marker, and state remain.
	RollbackConfirmed
	// WriteSuperseded means a different equal-or-later operation is durable.
	WriteSuperseded
	// CommitUnresolved means primary read-back did not prove another outcome.
	CommitUnresolved
)

type senderGateSet [senderGateStripes]chan struct{}

func newSenderGateSet() senderGateSet {
	var gates senderGateSet
	for index := range gates {
		gates[index] = make(chan struct{}, 1)
	}
	return gates
}

func senderGateIndex(senderID uuid.UUID) int {
	var hash uint32
	for _, value := range senderID {
		hash = hash*33 + uint32(value)
	}
	return int(hash % senderGateStripes)
}

// WithSender serializes one sender's local mutation, commit classification,
// claim acknowledgement, and reconnect snapshot boundary.
func (s *Service) WithSender(
	ctx context.Context,
	senderID uuid.UUID,
	work func() error,
) error {
	gate := s.senderGates[senderGateIndex(senderID)]
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-gate }()
	if err := ctx.Err(); err != nil {
		return err
	}
	return work()
}

// BeginAudienceOperation acquires users -> settings -> pending locks and writes
// the new version and durable marker in the caller's transaction.
func (s *Service) BeginAudienceOperation(
	ctx context.Context,
	tx *sql.Tx,
	senderID uuid.UUID,
	mode OperationMode,
) (AudienceOperation, error) {
	if mode != OrdinaryAudienceWrite && mode != ForcedSecurityClear {
		return AudienceOperation{}, fmt.Errorf("invalid audience operation mode")
	}
	if err := lockUser(ctx, tx, senderID); err != nil {
		return AudienceOperation{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO user_presence_settings (user_id)
		VALUES ($1)
		ON CONFLICT (user_id) DO NOTHING
	`, senderID); err != nil {
		return AudienceOperation{}, fmt.Errorf("ensure audience operation settings: %w", err)
	}

	var (
		priorVersion   int64
		priorMarker    uuid.NullUUID
		beforeTier     int
		beforeText     sql.NullString
		beforeEmoji    sql.NullString
		pendingMarker  uuid.UUID
		reconcileAfter time.Time
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT presence_settings_version,
		       presence_settings_operation_id,
		       custom_text_tier,
		       custom_text,
		       custom_text_emoji
		FROM user_presence_settings
		WHERE user_id = $1
		FOR UPDATE
	`, senderID).Scan(
		&priorVersion,
		&priorMarker,
		&beforeTier,
		&beforeText,
		&beforeEmoji,
	); err != nil {
		return AudienceOperation{}, fmt.Errorf("lock audience operation settings: %w", err)
	}

	pendingExists := true
	if err := tx.QueryRowContext(ctx, `
		SELECT operation_id, reconcile_after
		FROM presence_settings_pending_operations
		WHERE user_id = $1
		FOR UPDATE
	`, senderID).Scan(&pendingMarker, &reconcileAfter); errors.Is(err, sql.ErrNoRows) {
		pendingExists = false
	} else if err != nil {
		return AudienceOperation{}, fmt.Errorf("lock pending presence operation: %w", err)
	}

	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return AudienceOperation{}, fmt.Errorf("read audience operation clock: %w", err)
	}
	if pendingExists && mode == OrdinaryAudienceWrite {
		if reconcileAfter.After(now) {
			return AudienceOperation{}, &ServiceError{
				Status:     http.StatusServiceUnavailable,
				Code:       "presence_operation_pending",
				RetryAfter: ceilRetryAfter(reconcileAfter.Sub(now)),
			}
		}
		return AudienceOperation{}, ErrPendingOperationEligible
	}
	if priorVersion == math.MaxInt64 {
		return AudienceOperation{}, fmt.Errorf("audience operation version exhausted")
	}

	operationID := uuid.New()
	nextVersion := priorVersion + 1
	nextReconcileAfter := now.Add(pendingOperationGrace)
	if pendingExists {
		result, err := tx.ExecContext(ctx, `
			UPDATE presence_settings_pending_operations
			SET operation_id = $2,
			    prior_settings_version = $3,
			    created_at = $4,
			    reconcile_after = $5
			WHERE user_id = $1
		`, senderID, operationID, priorVersion, now, nextReconcileAfter)
		if err != nil {
			return AudienceOperation{}, fmt.Errorf("supersede pending presence operation: %w", err)
		}
		if err := requireOneAffected(result, "supersede pending presence operation"); err != nil {
			return AudienceOperation{}, err
		}
	} else if _, err := tx.ExecContext(ctx, `
		INSERT INTO presence_settings_pending_operations (
			user_id,
			operation_id,
			prior_settings_version,
			created_at,
			reconcile_after
		) VALUES ($1, $2, $3, $4, $5)
	`, senderID, operationID, priorVersion, now, nextReconcileAfter); err != nil {
		return AudienceOperation{}, fmt.Errorf("insert pending presence operation: %w", err)
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE user_presence_settings
		SET presence_settings_version = $2,
		    presence_settings_operation_id = $3,
		    updated_at = $4
		WHERE user_id = $1
	`, senderID, nextVersion, operationID, now)
	if err != nil {
		return AudienceOperation{}, fmt.Errorf("store audience operation version: %w", err)
	}
	if err := requireOneAffected(result, "store audience operation version"); err != nil {
		return AudienceOperation{}, err
	}

	operation := AudienceOperation{
		ID:             operationID,
		SenderID:       senderID,
		PriorVersion:   priorVersion,
		Version:        nextVersion,
		ReconcileAfter: nextReconcileAfter,
		Before: normalizeCustomTextState(CustomTextState{
			Text:  nullableString(beforeText),
			Emoji: nullableString(beforeEmoji),
		}),
		BeforeTier: beforeTier,
	}
	if priorMarker.Valid {
		value := priorMarker.UUID
		operation.PriorOperationID = &value
	}
	return operation, nil
}

func ceilRetryAfter(remaining time.Duration) time.Duration {
	if remaining <= 0 {
		return 0
	}
	rounded := remaining.Truncate(time.Second)
	if rounded < remaining {
		rounded += time.Second
	}
	return rounded
}

func requireOneAffected(result sql.Result, operation string) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s row count: %w", operation, err)
	}
	if affected != 1 {
		return fmt.Errorf("%s: exact row count mismatch", operation)
	}
	return nil
}

func nullableString(value sql.NullString) string {
	if !value.Valid {
		return ""
	}
	return value.String
}

type audienceCommitState struct {
	UserExists             bool
	SettingsExists         bool
	Version                int64
	OperationID            *uuid.UUID
	Before                 CustomTextState
	BeforeTier             int
	PendingOperationID     *uuid.UUID
	PendingPriorVersion    int64
	PendingPriorVersionSet bool
}

// ClassifyAudienceCommit uses one bounded primary read-back that survives
// request cancellation, then requires exact durable evidence.
func (s *Service) ClassifyAudienceCommit(
	requestCtx context.Context,
	operation AudienceOperation,
) CommitOutcome {
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(requestCtx), commitReadbackTimeout)
	defer cancel()
	readState := s.readCommitState
	if readState == nil {
		readState = s.readAudienceCommitState
	}
	state, err := readState(readCtx, operation.SenderID)
	if err != nil {
		return CommitUnresolved
	}
	return classifyAudienceCommitState(operation, state)
}

func (s *Service) readAudienceCommitState(
	ctx context.Context,
	senderID uuid.UUID,
) (audienceCommitState, error) {
	var (
		lockedID     uuid.UUID
		settings     bool
		version      int64
		operationID  uuid.NullUUID
		tier         int
		text         sql.NullString
		emoji        sql.NullString
		pendingID    uuid.NullUUID
		pendingPrior sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT owners.id,
		       settings.user_id IS NOT NULL,
		       COALESCE(settings.presence_settings_version, 0),
		       settings.presence_settings_operation_id,
		       COALESCE(settings.custom_text_tier, 0),
		       settings.custom_text,
		       settings.custom_text_emoji,
		       pending.operation_id,
		       pending.prior_settings_version
		FROM users AS owners
		LEFT JOIN user_presence_settings AS settings
		  ON settings.user_id = owners.id
		LEFT JOIN presence_settings_pending_operations AS pending
		  ON pending.user_id = owners.id
		WHERE owners.id = $1
	`, senderID).Scan(
		&lockedID,
		&settings,
		&version,
		&operationID,
		&tier,
		&text,
		&emoji,
		&pendingID,
		&pendingPrior,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return audienceCommitState{}, nil
	}
	if err != nil {
		return audienceCommitState{}, fmt.Errorf("read audience commit state: %w", err)
	}
	state := audienceCommitState{
		UserExists:             true,
		SettingsExists:         settings,
		Version:                version,
		Before:                 normalizeCustomTextState(CustomTextState{Text: nullableString(text), Emoji: nullableString(emoji)}),
		BeforeTier:             tier,
		PendingPriorVersion:    pendingPrior.Int64,
		PendingPriorVersionSet: pendingPrior.Valid,
	}
	if operationID.Valid {
		value := operationID.UUID
		state.OperationID = &value
	}
	if pendingID.Valid {
		value := pendingID.UUID
		state.PendingOperationID = &value
	}
	return state, nil
}

func classifyAudienceCommitState(
	operation AudienceOperation,
	state audienceCommitState,
) CommitOutcome {
	if operation.ID == uuid.Nil || operation.PriorVersion < 0 ||
		operation.PriorVersion == math.MaxInt64 || operation.Version != operation.PriorVersion+1 ||
		!state.UserExists {
		return CommitUnresolved
	}
	if state.SettingsExists && state.Version == operation.Version &&
		uuidPointerEqual(state.OperationID, &operation.ID) &&
		uuidPointerEqual(state.PendingOperationID, &operation.ID) &&
		state.PendingPriorVersionSet && state.PendingPriorVersion == operation.PriorVersion {
		return CommitConfirmed
	}
	if state.SettingsExists && state.Version >= operation.Version &&
		state.OperationID != nil && *state.OperationID != operation.ID {
		return WriteSuperseded
	}
	if rollbackStateMatches(operation, state) {
		return RollbackConfirmed
	}
	return CommitUnresolved
}

func rollbackStateMatches(operation AudienceOperation, state audienceCommitState) bool {
	if state.PendingOperationID != nil &&
		(operation.PriorOperationID == nil || *state.PendingOperationID != *operation.PriorOperationID) {
		return false
	}
	if !state.SettingsExists {
		return operation.PriorVersion == 0 && operation.PriorOperationID == nil &&
			normalizeCustomTextState(operation.Before) == (CustomTextState{}) && operation.BeforeTier == 0 &&
			state.PendingOperationID == nil
	}
	return state.Version == operation.PriorVersion &&
		uuidPointerEqual(state.OperationID, operation.PriorOperationID) &&
		normalizeCustomTextState(state.Before) == normalizeCustomTextState(operation.Before) &&
		state.BeforeTier == operation.BeforeTier
}

func uuidPointerEqual(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
