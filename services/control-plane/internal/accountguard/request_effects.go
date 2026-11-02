package accountguard

import (
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
	"github.com/google/uuid"
)

type requestEffectPlan struct{ effects []decide.Effect }
type requestPendingPayload struct{ passwordHash, address *string }
type preparedOpenings struct {
	requestID      uuid.UUID
	disputeGroupID uuid.UUID
	payload        requestPendingPayload
}
type requestApplyResult struct{ openedRequestID, disputeGroupID uuid.UUID }

type requestPlanShape struct {
	opening    *decide.Opening
	group      string
	needsGroup bool
}

func requestUUID(value string) (uuid.UUID, error) {
	id, err := uuid.Parse(value)
	if err != nil || id == uuid.Nil || id.String() != value {
		return uuid.Nil, errRequestShape
	}
	return id, nil
}

func validRequestTime(t time.Time) bool {
	return !t.IsZero() && t.Year() >= 1 && t.Year() <= 9999 && t.Nanosecond()%1000 == 0
}

func validRequestCandidate(c decide.Candidate) bool {
	return len(c.Keyed) == 32 && len(c.Coarse) == 32 && c.KeyID > 0
}

func validRequestCancel(reason decide.CancelReason, class decide.Class) bool {
	switch reason {
	case decide.CancelProof:
		return class >= decide.C1 && class <= decide.C3
	case decide.CancelDisputeResolved:
		return class == decide.C3
	case decide.CancelSuperseded, decide.CancelHoldCap, decide.CancelDisputeExpired:
		return class == decide.ClassNone
	default:
		return false
	}
}

func validateRequestOpening(o decide.Opening) error {
	if !validRequestTargets(o.Kind, o.Targets, o.ResetFactors) || len(o.Origins) == 0 || !validRequestRefs(o.Origins) ||
		o.OriginClass < decide.C1 || o.OriginClass > decide.C3 || !validRequestTime(o.OpenedAt) || !validRequestTime(o.CompletesAt) {
		return errRequestShape
	}
	switch o.Status {
	case decide.StatusPending, decide.StatusDisputed:
		if !o.CompletesAt.Equal(o.OpenedAt.Add(decide.DeltaCP)) || !o.CompletedAt.IsZero() || !validRequestCandidate(o.Candidate) {
			return errRequestShape
		}
		if o.Status == decide.StatusPending && !o.DisputedAt.IsZero() {
			return errRequestShape
		}
		if o.Status == decide.StatusDisputed && !o.DisputedAt.Equal(o.OpenedAt) {
			return errRequestShape
		}
	case decide.StatusCompleted:
		if o.Kind != decide.ReqBreakGlass || o.OriginClass != decide.C3 || !o.DisputedAt.IsZero() ||
			!o.CompletedAt.Equal(o.OpenedAt) || !o.CompletesAt.Equal(o.OpenedAt) ||
			len(o.Candidate.Keyed) != 0 || len(o.Candidate.Coarse) != 0 || o.Candidate.KeyID != 0 {
			return errRequestShape
		}
	default:
		return errRequestShape
	}
	return nil
}

// validateRequestEffect checks the closed shape without interpreting a complete
// decision. Non-request effects remain obligations of the future coordinator.
func validateRequestEffect(e decide.Effect) (bool, error) {
	allowed := decide.Effect{Kind: e.Kind}
	isRequest := true
	switch e.Kind {
	case decide.EffectOpen:
		if err := validateRequestOpening(e.Open); err != nil {
			return false, err
		}
		allowed.Open = e.Open
		if e.Open.Status == decide.StatusDisputed {
			allowed.Group = e.Group
		}
	case decide.EffectCancel:
		if !validRequestCancel(e.Reason, e.CancellerClass) {
			return false, errRequestShape
		}
		allowed.RequestID, allowed.Reason, allowed.CancellerClass = e.RequestID, e.Reason, e.CancellerClass
	case decide.EffectDispute:
		allowed.RequestID, allowed.Group = e.RequestID, e.Group
	case decide.EffectReturnToPending, decide.EffectComplete:
		allowed.RequestID = e.RequestID
	case decide.EffectHold:
		if e.HeldMs < 0 || (!e.HeldSince.IsZero() && !validRequestTime(e.HeldSince)) {
			return false, errRequestShape
		}
		allowed.RequestID, allowed.HeldMs, allowed.HeldSince = e.RequestID, e.HeldMs, e.HeldSince
	case decide.EffectRemove, decide.EffectContest, decide.EffectRestorePrimary, decide.EffectReleaseReservation:
		if !validRequestRef(e.Cred) {
			return false, errRequestShape
		}
		allowed.Cred, isRequest = e.Cred, false
	case decide.EffectResyncMFAFlags:
		isRequest = false
	default:
		return false, errRequestShape
	}
	if !reflect.DeepEqual(e, allowed) {
		return false, errRequestShape
	}
	if isRequest && e.Kind != decide.EffectOpen {
		if _, err := requestUUID(e.RequestID); err != nil {
			return false, err
		}
	}
	if e.Group != "" {
		if _, err := requestUUID(e.Group); err != nil {
			return false, err
		}
	}
	return isRequest, nil
}

func cloneRequestEffect(e decide.Effect) decide.Effect {
	e.Open.Targets = slices.Clone(e.Open.Targets)
	e.Open.Origins = slices.Clone(e.Open.Origins)
	e.Open.Candidate.Keyed = slices.Clone(e.Open.Candidate.Keyed)
	e.Open.Candidate.Coarse = slices.Clone(e.Open.Candidate.Coarse)
	return e
}

func partitionRequestEffects(outcome decide.Outcome) (requestEffectPlan, decide.Outcome, error) {
	var plan requestEffectPlan
	remainder := outcome
	remainder.Effects = nil
	remainder.Notices = slices.Clone(outcome.Notices)
	for i := range remainder.Notices {
		remainder.Notices[i].ReservedTo = slices.Clone(outcome.Notices[i].ReservedTo)
	}
	for _, effect := range outcome.Effects {
		owned, err := validateRequestEffect(effect)
		if err != nil {
			return requestEffectPlan{}, decide.Outcome{}, err
		}
		if owned {
			plan.effects = append(plan.effects, cloneRequestEffect(effect))
		} else {
			remainder.Effects = append(remainder.Effects, cloneRequestEffect(effect))
		}
	}
	if _, err := validateRequestPlan(plan); err != nil {
		return requestEffectPlan{}, decide.Outcome{}, err
	}
	return plan, remainder, nil
}

func validateRequestPlan(plan requestEffectPlan) (requestPlanShape, error) {
	var shape requestPlanShape
	transitions, holds := make(map[string]bool), make(map[string]bool)
	for _, e := range plan.effects {
		owned, err := validateRequestEffect(e)
		if err != nil || !owned {
			return requestPlanShape{}, errRequestShape
		}
		switch e.Kind {
		case decide.EffectOpen:
			if shape.opening != nil {
				return requestPlanShape{}, errRequestShape
			}
			o := e.Open
			shape.opening = &o
		case decide.EffectHold:
			if holds[e.RequestID] {
				return requestPlanShape{}, errRequestShape
			}
			holds[e.RequestID] = true
		default:
			if transitions[e.RequestID] {
				return requestPlanShape{}, errRequestShape
			}
			transitions[e.RequestID] = true
		}
		if e.Kind == decide.EffectDispute || (e.Kind == decide.EffectOpen && e.Open.Status == decide.StatusDisputed) {
			if e.Group == "" {
				shape.needsGroup = true
			} else if shape.group != "" && shape.group != e.Group {
				return requestPlanShape{}, errRequestShape
			} else {
				shape.group = e.Group
			}
		}
	}
	if shape.needsGroup && shape.group != "" {
		return requestPlanShape{}, errRequestShape
	}
	return shape, nil
}

func validRequestPayload(kind decide.ReqKind, status decide.ReqStatus, p requestPendingPayload) bool {
	if status == decide.StatusCompleted || status == decide.StatusCancelled {
		return p.passwordHash == nil && p.address == nil // pragma: allowlist secret -- Terminal payload shape, not a credential literal.
	}
	switch kind {
	case decide.ReqBreakGlass:
		return p.passwordHash != nil && *p.passwordHash != "" && p.address == nil // pragma: allowlist secret -- Required payload shape, not a credential literal.
	case decide.ReqAddressChange:
		return p.passwordHash == nil && p.address != nil && normalizedRequestAddress(*p.address) // pragma: allowlist secret -- Exclusive payload shape, not a credential literal.
	case decide.ReqCredentialRemove:
		return p.passwordHash == nil && p.address == nil // pragma: allowlist secret -- Credential removal has no pending payload.
	default:
		return false
	}
}

func cloneRequestPayload(p requestPendingPayload) requestPendingPayload {
	if p.passwordHash != nil { // pragma: allowlist secret -- Copy an existing value; no embedded credential.
		s := *p.passwordHash
		p.passwordHash = &s
	}
	if p.address != nil {
		s := *p.address
		p.address = &s
	}
	return p
}

func validatePreparedPayload(shape requestPlanShape, p requestPendingPayload) error {
	if shape.opening == nil {
		if p.passwordHash != nil || p.address != nil { // pragma: allowlist secret -- Reject an unexpected payload; no credential literal.
			return errRequestShape
		}
	} else if !validRequestPayload(shape.opening.Kind, shape.opening.Status, p) {
		return errRequestShape
	}
	return nil
}

func prepareRequestOpenings(plan requestEffectPlan, payload requestPendingPayload, newID func() (uuid.UUID, error)) (preparedOpenings, error) {
	shape, err := validateRequestPlan(plan)
	if err != nil {
		return preparedOpenings{}, err
	}
	if err := validatePreparedPayload(shape, payload); err != nil {
		return preparedOpenings{}, err
	}
	if (shape.opening != nil || shape.needsGroup) && newID == nil {
		return preparedOpenings{}, errRequestShape
	}
	prepared := preparedOpenings{payload: cloneRequestPayload(payload)}
	allocate := func() (uuid.UUID, error) {
		id, err := newID()
		if err != nil {
			return uuid.Nil, fmt.Errorf("accountguard: prepare request identity: %w", err)
		}
		if id == uuid.Nil || id == prepared.requestID {
			return uuid.Nil, errRequestShape
		}
		for _, e := range plan.effects {
			if id.String() == e.RequestID || id.String() == e.Group {
				return uuid.Nil, errRequestShape
			}
		}
		return id, nil
	}
	if shape.opening != nil {
		prepared.requestID, err = allocate()
		if err != nil {
			return preparedOpenings{}, err
		}
	}
	if shape.needsGroup {
		prepared.disputeGroupID, err = allocate()
		if err != nil {
			return preparedOpenings{}, err
		}
	}
	return prepared, nil
}
