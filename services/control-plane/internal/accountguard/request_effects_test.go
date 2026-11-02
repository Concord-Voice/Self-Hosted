package accountguard

import (
	"errors"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRequestPartitionRetainsAllNonEffectObligationsAndCopiesNestedData(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	id := uuid.NewString()
	input := decide.Outcome{Row: decide.RowT6c, Verdict: decide.VerdictApplied, Source: decide.SourceBreakGlass, RotateEpoch: true, SetsPassword: true, Continuation: true, PermGenBump: true, EnrolClass: decide.C3,
		Effects: []decide.Effect{{Kind: decide.EffectRemove, Cred: requestOrigin()}, {Kind: decide.EffectOpen, Open: requestOpening(now)}, {Kind: decide.EffectContest, Cred: requestOrigin()}, {Kind: decide.EffectCancel, RequestID: id, Reason: decide.CancelProof, CancellerClass: decide.C2}},
		Notices: []decide.Notice{{Kind: decide.NoticeChangeRequested, Subject: requestOrigin(), ReservedTo: []decide.CredRef{requestOrigin()}}}}
	plan, remainder, err := partitionRequestEffects(input)
	require.NoError(t, err)
	require.Equal(t, []decide.Effect{input.Effects[1], input.Effects[3]}, plan.effects)
	expected := input
	expected.Effects = []decide.Effect{input.Effects[0], input.Effects[2]}
	require.Equal(t, expected, remainder)
	input.Effects[1].Open.Origins[0].Version = 999
	input.Effects[1].Open.Candidate.Keyed[0] ^= 0xff
	input.Notices[0].ReservedTo[0].Version = 999
	require.Equal(t, int64(11), plan.effects[0].Open.Origins[0].Version)
	require.Equal(t, byte('0'), plan.effects[0].Open.Candidate.Keyed[0])
	require.Equal(t, int64(11), remainder.Notices[0].ReservedTo[0].Version)
}
func TestRequestPreparationNoAllocationOnInvalidPlanAndExactGroupAllocation(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	open := requestOpening(now)
	open.Status = decide.StatusDisputed
	open.DisputedAt = now
	plan := requestEffectPlan{effects: []decide.Effect{{Kind: decide.EffectOpen, Open: open}, {Kind: decide.EffectDispute, RequestID: uuid.NewString()}}}
	ids := []uuid.UUID{uuid.New(), uuid.New()}
	calls := 0
	prepared, err := prepareRequestOpenings(plan, requestPayload(), func() (uuid.UUID, error) { id := ids[calls]; calls++; return id, nil })
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.ElementsMatch(t, ids, []uuid.UUID{prepared.requestID, prepared.disputeGroupID})
	payload := requestPayload()
	prepared, err = prepareRequestOpenings(plan, payload, uuid.NewRandom)
	require.NoError(t, err)
	*payload.passwordHash = "changed-local-copy" // pragma: allowlist secret -- Synthetic mutation for the copy-isolation oracle.
	require.Equal(t, "synthetic-opaque-argon-hash", *prepared.payload.passwordHash)
	sentinel := errors.New("synthetic entropy failure")
	_, err = prepareRequestOpenings(plan, requestPayload(), func() (uuid.UUID, error) { return uuid.Nil, sentinel })
	require.ErrorIs(t, err, sentinel)
	for _, bad := range []requestEffectPlan{
		{effects: []decide.Effect{{Kind: 255}}},
		{effects: []decide.Effect{{Kind: decide.EffectOpen, Open: open}, {Kind: decide.EffectOpen, Open: open}}},
		{effects: []decide.Effect{{Kind: decide.EffectCancel, RequestID: "not-canonical", Reason: decide.CancelProof, CancellerClass: decide.C1}}},
		{effects: []decide.Effect{{Kind: decide.EffectComplete, RequestID: uuid.NewString(), Cred: requestOrigin()}}},
	} {
		calls = 0
		_, err = prepareRequestOpenings(bad, requestPayload(), func() (uuid.UUID, error) { calls++; return uuid.New(), nil })
		require.Error(t, err)
		require.Zero(t, calls)
	}
	for _, fn := range []func() (uuid.UUID, error){func() (uuid.UUID, error) { return uuid.Nil, nil }, func() (uuid.UUID, error) { return ids[0], nil }} {
		_, err = prepareRequestOpenings(plan, requestPayload(), fn)
		require.Error(t, err)
	}
	_, err = prepareRequestOpenings(requestEffectPlan{}, requestPendingPayload{}, nil)
	require.NoError(t, err)
	_, err = prepareRequestOpenings(plan, requestPayload(), nil)
	require.Error(t, err)
}
func TestRequestPartitionUnknownAndMalformedReturnsNoPlan(t *testing.T) {
	for _, effect := range []decide.Effect{{Kind: 255}, {Kind: decide.EffectCancel, RequestID: uuid.NewString(), Reason: decide.CancelProof}, {Kind: decide.EffectHold, RequestID: uuid.NewString(), HeldMs: -1}} {
		plan, _, err := partitionRequestEffects(decide.Outcome{Effects: []decide.Effect{effect}})
		require.Error(t, err)
		require.Empty(t, plan.effects)
	}
}

func TestRequestPreparationRejectsPayloadAndGenerationShapeBeforeEntropy(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	hash, address, empty := "opaque-hash", "next@example.invalid", ""
	for _, c := range []struct {
		name    string
		open    decide.Opening
		payload requestPendingPayload
	}{
		{"missing-breakglass-payload", requestOpening(now), requestPendingPayload{}},
		{"empty-hash", requestOpening(now), requestPendingPayload{passwordHash: &empty}},
		{"both-payloads", requestOpening(now), requestPendingPayload{passwordHash: &hash, address: &address}},
		{"bad-key-generation", func() decide.Opening { o := requestOpening(now); o.Candidate.KeyID = 0; return o }(), requestPayload()},
		{"short-keyed", func() decide.Opening { o := requestOpening(now); o.Candidate.Keyed = []byte{1}; return o }(), requestPayload()},
		{"missing-origins", func() decide.Opening { o := requestOpening(now); o.Origins = nil; return o }(), requestPayload()},
		{"unordered-numeric-kinds", func() decide.Opening {
			o := requestOpening(now)
			o.Origins = []decide.CredRef{{Kind: decide.CredRecoveryDevice, ID: "a", Version: 1}, {Kind: decide.CredTOTP, ID: "b", Version: 1}}
			return o
		}(), requestPayload()},
	} {
		t.Run(c.name, func(t *testing.T) {
			calls := 0
			_, err := prepareRequestOpenings(requestEffectPlan{effects: []decide.Effect{{Kind: decide.EffectOpen, Open: c.open}}}, c.payload, func() (uuid.UUID, error) { calls++; return uuid.New(), nil })
			require.Error(t, err)
			require.Zero(t, calls)
		})
	}
	for _, id := range []string{uuid.Nil.String(), "AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA", "{aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa}"} {
		_, err := prepareRequestOpenings(requestEffectPlan{effects: []decide.Effect{{Kind: decide.EffectCancel, RequestID: id, Reason: decide.CancelProof, CancellerClass: decide.C1}}}, requestPendingPayload{}, nil)
		require.Error(t, err)
	}
	// An existing canonical group needs no replacement allocation.
	group := uuid.New()
	calls := 0
	prepared, err := prepareRequestOpenings(requestEffectPlan{effects: []decide.Effect{{Kind: decide.EffectDispute, RequestID: uuid.NewString(), Group: group.String()}}}, requestPendingPayload{}, func() (uuid.UUID, error) { calls++; return uuid.New(), nil })
	require.NoError(t, err)
	require.Zero(t, calls)
	require.Equal(t, uuid.Nil, prepared.disputeGroupID)
}

func TestRequestPartitionActualContinuationAndPermissionObligations(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	in := requestPolicyInput(now, false)
	in.Act = decide.SignOutOthers{}
	out := decide.Decide(in)
	require.True(t, out.Continuation)
	require.True(t, out.RotateEpoch)
	plan, rem, err := partitionRequestEffects(out)
	require.NoError(t, err)
	require.Empty(t, plan.effects)
	require.Equal(t, out, rem)
	in = requestPolicyInput(now, true)
	in.Act = decide.SignOutEverywhere{}
	in.Creds = append(in.Creds, decide.Cred{Ref: decide.CredRef{Kind: decide.CredTOTP, ID: "immature-other-factor", Version: 47}, EnrolledAt: now.Add(-time.Hour), MaturesAt: now.Add(time.Hour), EnrolClass: decide.C2})
	out = decide.Decide(in)
	require.True(t, out.PermGenBump)
	require.NotEmpty(t, out.Effects)
	require.NotEmpty(t, out.Notices)
	plan, rem, err = partitionRequestEffects(out)
	require.NoError(t, err)
	require.Empty(t, plan.effects)
	require.Equal(t, out, rem)
}
