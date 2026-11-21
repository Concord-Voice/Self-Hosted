package voice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/google/uuid"
)

// ServerVoiceLifecycleAdvisoryKeyForTest exposes the production lock identity
// for deterministic cross-connection reconciliation tests.
func ServerVoiceLifecycleAdvisoryKeyForTest(sender uuid.UUID) (int64, error) {
	return voiceLifecycleAdvisoryKey(presence.CategoryServerVoice, sender)
}

// TestServerVoiceMutationResult exposes the atomic participant-move result.
type TestServerVoiceMutationResult struct {
	Applied                bool
	Added                  bool
	RemovedRoomIDs         []uuid.UUID
	RemovedAudienceUnknown bool
	Duplicate              bool
	ReplayMissing          bool
}

// ServerVoiceMutationNeedsReconnectForTest exposes the post-mutation
// convergence decision for an optimistic pre-lock scope observation.
func ServerVoiceMutationNeedsReconnectForTest(
	result TestServerVoiceMutationResult,
	targetRoomID uuid.UUID,
	observedScope presence.Scope,
	hasObservedScope bool,
) bool {
	return serverHeartbeatMutationNeedsReconnect(serverVoiceMutationResult{
		applied: result.Applied, added: result.Added,
		removedRoomIDs:         append([]uuid.UUID(nil), result.RemovedRoomIDs...),
		removedAudienceUnknown: result.RemovedAudienceUnknown,
		duplicate:              result.Duplicate, replayMissing: result.ReplayMissing,
	}, targetRoomID, observedScope, hasObservedScope)
}

// TestPrivateVoiceLifecycleClaim exposes one entry in the atomic Private Call
// participant-set revision for focused cross-replica integration tests.
type TestPrivateVoiceLifecycleClaim struct {
	UserID  uuid.UUID
	Token   uuid.UUID
	Version int64
	Active  bool
}

// ClaimPrivateVoiceLifecyclesForTest exposes the all-or-nothing multi-sender
// Redis fence without coupling external tests to internal status constants.
func (s *NATSSubscriber) ClaimPrivateVoiceLifecyclesForTest(
	ctx context.Context,
	claims []TestPrivateVoiceLifecycleClaim,
) (accepted bool, duplicate bool, err error) {
	internal := make([]privateVoiceParticipantSetClaim, 0, len(claims))
	for _, claim := range claims {
		internal = append(internal, privateVoiceParticipantSetClaim{
			userID: claim.UserID, token: claim.Token,
			version: claim.Version, active: claim.Active,
		})
	}
	status, err := s.claimPrivateVoiceLifecycles(ctx, internal)
	if err != nil {
		return false, false, err
	}
	return status != voiceLifecycleRejected, status == voiceLifecycleDuplicate, nil
}

// ClaimVoiceLifecycleForTest exposes lifecycle CAS semantics for focused
// integration tests without routing synthetic events through unrelated code.
func (s *NATSSubscriber) ClaimVoiceLifecycleForTest(
	ctx context.Context,
	category presence.Category,
	senderID, token uuid.UUID,
	eventAt time.Time,
	active bool,
) (bool, error) {
	return s.claimVoiceLifecycle(ctx, category, senderID, token, eventAt, active)
}

// SetVoiceLifecycleClaimedHookForTest pauses a lifecycle mutation after Redis
// accepted its watermark. The production critical section must remain held
// until the hook and the subsequent database mutation complete.
func (s *NATSSubscriber) SetVoiceLifecycleClaimedHookForTest(
	hook func(presence.Category, uuid.UUID, time.Time),
) {
	s.voiceLifecycleClaimedHook = hook
}

// SetServerVoiceScopeObservedHookForTest pauses a server participant refresh
// after its optimistic scope/audience reads and before the lifecycle lock.
func (s *NATSSubscriber) SetServerVoiceScopeObservedHookForTest(
	hook func(uuid.UUID, uuid.UUID, time.Time),
) {
	s.serverVoiceScopeObservedHook = hook
}

// SetPrivateJoinHooksForTest exposes deterministic boundaries immediately
// before the guarded join mutation and immediately before its base broadcast.
func (s *NATSSubscriber) SetPrivateJoinHooksForTest(
	beforeMutation, beforeBroadcast func(uuid.UUID, uuid.UUID),
) {
	s.privateJoinBeforeMutationHook = beforeMutation
	s.privateJoinBroadcastHook = beforeBroadcast
}

// SetPrivateVoiceParticipantSetLockAttemptHookForTest exposes the handoff
// immediately before a private participant-set lock is acquired.
func (s *NATSSubscriber) SetPrivateVoiceParticipantSetLockAttemptHookForTest(
	hook func(uuid.UUID),
) {
	s.privateVoiceParticipantSetLockAttemptHook = hook
}

// SetPrivateVoiceDurabilityHooksForTest exposes deterministic post-commit and
// base-state boundaries for focused Private Call durability regressions.
func (s *NATSSubscriber) SetPrivateVoiceDurabilityHooksForTest(
	leaveAfterCommit func(),
	heartbeatAfterCommit func(),
	stateBroadcast func(uuid.UUID, uuid.UUID, string),
) {
	s.privateLeaveAfterCommitHook = leaveAfterCommit
	s.dmHeartbeatPostCommitHook = heartbeatAfterCommit
	s.privateVoiceStateBroadcastHook = stateBroadcast
}

// SetDMRoomEmptyVerificationHookForTest injects the final terminal roster read.
func (s *NATSSubscriber) SetDMRoomEmptyVerificationHookForTest(hook func() error) {
	s.dmRoomEmptyVerificationHook = hook
}

// SetDMTerminalDeleteObservedHookForTest observes each DM terminal participant
// delete: (participantID, applied, err). `applied` is the only value that
// distinguishes "the terminal completed" from "the row went away for some other
// reason" -- see the field comment in nats.go for why neither the return value
// nor the logs can do it.
func (s *NATSSubscriber) SetDMTerminalDeleteObservedHookForTest(
	hook func(participantID uuid.UUID, applied bool, err error),
) {
	s.dmTerminalDeleteObservedHook = hook
}

// SetActivityServiceForTest injects RP bridge availability for security-order tests.
func (s *NATSSubscriber) SetActivityServiceForTest(activity *presence.ActivityService) {
	s.activity = activity
}

// CompleteServerVoiceCleanupGraceForTest expires the one-shot startup grace
// without resetting its sync.Once. It returns the deadline armed by the first
// cleanup invocation so tests can assert the lease was bounded correctly.
func (s *NATSSubscriber) CompleteServerVoiceCleanupGraceForTest() time.Time {
	s.serverVoiceCleanupOnce.Do(func() {})
	readyAt := s.serverVoiceCleanupReadyAt
	s.serverVoiceCleanupReadyAt = time.Now().Add(-time.Nanosecond)
	return readyAt
}

// DrainServerVoiceTerminalOutboxCandidateForTest drains one exact candidate so
// external-package tests can verify operation-token fencing without exposing
// the production scheduler.
func (s *NATSSubscriber) DrainServerVoiceTerminalOutboxCandidateForTest(
	ctx context.Context, channelID, userID, operationID uuid.UUID,
) error {
	_, err := s.drainServerVoiceTerminalOutboxCandidate(ctx, serverVoiceTerminalOutboxCandidate{
		channelID: channelID, userID: userID, operationID: operationID,
	}, &serverVoiceTerminalOutcomes{})
	return err
}

// DeleteServerVoiceParticipantForTest exercises the ordinary leave mutation
// without routing a synthetic NATS event through unrelated validation.
func (s *NATSSubscriber) DeleteServerVoiceParticipantForTest(
	ctx context.Context,
	serverID, channelID, participantID uuid.UUID,
	eventAt time.Time,
) (bool, error) {
	mutation := serverVoiceLeaveMutation{
		subscriber: s, serverID: serverID, channelID: channelID,
		senderID: participantID, eventAt: eventAt,
	}
	return mutation.apply(withLifecycleOrigin(ctx, TestMediaBootID))
}

// HandlePresenceErasureClearedForTest delivers a remote erasure clear to this
// replica without requiring a live NATS connection.
func (s *NATSSubscriber) HandlePresenceErasureClearedForTest(data []byte) {
	s.handlePresenceErasureCleared(data)
}

// ApplyServerVoiceParticipantMutationForTest runs the same one- or two-channel
// delivery fence that production uses around a Server Voice lifecycle mutation.
func (s *NATSSubscriber) ApplyServerVoiceParticipantMutationForTest(
	serverID, channelID uuid.UUID,
	oldScope presence.Scope,
	oldServerID uuid.UUID,
	hasOldScope bool,
	mutation func() (bool, error),
) (bool, error) {
	return s.applyServerVoiceParticipantMutation(serverID, channelID, oldScope, oldServerID, hasOldScope, mutation)
}

// RunVoiceLifecycleMutationForTest exercises the same claim-then-mutate
// ordering as production.
func (s *NATSSubscriber) RunVoiceLifecycleMutationForTest(
	ctx context.Context,
	category presence.Category,
	senderID, token, conversationID uuid.UUID,
	eventAt time.Time,
	active bool,
	mutation func(context.Context, *sql.Tx) (bool, error),
) (bool, error) {
	return s.withVoiceLifecycleClaimInParticipantSet(
		ctx,
		voiceLifecycleClaimRequest{
			category: category, senderID: senderID, token: token,
			eventAt: eventAt, active: active, conversationID: conversationID,
		},
		mutation,
	)
}

// UpsertServerVoiceParticipantForTest exposes the atomic move for integration tests.
func (s *NATSSubscriber) UpsertServerVoiceParticipantForTest(
	ctx context.Context,
	channelID, senderID uuid.UUID,
	eventAt time.Time,
) (TestServerVoiceMutationResult, error) {
	result, err := s.upsertServerVoiceParticipant(
		withLifecycleOrigin(ctx, TestMediaBootID), channelID, senderID, eventAt, nil,
	)
	return TestServerVoiceMutationResult{
		Applied:                result.applied,
		Added:                  result.added,
		RemovedRoomIDs:         append([]uuid.UUID(nil), result.removedRoomIDs...),
		RemovedAudienceUnknown: result.removedAudienceUnknown,
		Duplicate:              result.duplicate,
		ReplayMissing:          result.replayMissing,
	}, err
}

// LoadServerVoiceMutationReplayForTest exposes strict replay decoding errors.
func (s *NATSSubscriber) LoadServerVoiceMutationReplayForTest(
	ctx context.Context,
	senderID, targetRoomID uuid.UUID,
	eventAt time.Time,
) error {
	_, _, err := s.loadServerVoiceMutationReplay(ctx, senderID, targetRoomID, eventAt)
	return err
}

// UpsertPrivateVoiceParticipantForTest exposes the exact-call participant-set
// mutation for deterministic cross-replica cap/concurrency integration tests.
func (s *NATSSubscriber) UpsertPrivateVoiceParticipantForTest(
	ctx context.Context,
	conversationID, senderID, callID uuid.UUID,
	eventAt time.Time,
) (bool, error) {
	applied, _, err := s.upsertPrivateVoiceParticipant(
		ctx, conversationID, senderID, callID, eventAt,
	)
	return applied, err
}

// DeleteCapturedPrivateActivityGenerationsForTest exposes exact-generation
// cleanup classification for focused fail-closed integration tests.
func (s *NATSSubscriber) DeleteCapturedPrivateActivityGenerationsForTest(
	ctx context.Context,
	participantIDs []uuid.UUID,
	generations map[uuid.UUID]presence.ActivityGeneration,
) error {
	return s.deleteCapturedPrivateActivityGenerations(ctx, participantIDs, generations)
}

// Export unexported methods and types for testing.

// TempGrantAllow exposes the temp-grant allow bitmask for assertion in tests (#487 D1).
const TempGrantAllow = tempGrantAllow

// TestTempGrantManager is an exported wrapper around tempGrantManager so external
// voice_test package tests can exercise the grant/revoke convergence (#487 Scope C).
type TestTempGrantManager struct {
	m *tempGrantManager
}

// NewTestTempGrantManager builds a tempGrantManager for testing. nats may be nil
// (publishForceDisconnect is then a no-op).
func NewTestTempGrantManager(db *sql.DB, log *logger.Logger, hub *websocket.Hub, resolver *rbac.Resolver, nats *natsclient.Client) *TestTempGrantManager {
	return &TestTempGrantManager{m: newTempGrantManager(db, log, hub, resolver, nats)}
}

// Grant exposes grantTemporaryChannelAccess.
func (t *TestTempGrantManager) Grant(ctx context.Context, serverID, channelID, userID string) error {
	return t.m.grantTemporaryChannelAccess(ctx, serverID, channelID, userID)
}

// SetGrantCommitForTest replaces only the grant transaction's commit call. It
// permits a test to model a successful COMMIT whose acknowledgement is lost.
func (t *TestTempGrantManager) SetGrantCommitForTest(commit func(*sql.Tx) error) {
	t.m.grantCommit = commit
}

// SetGrantRollbackForTest replaces only the ambiguous-grant rollback call.
func (t *TestTempGrantManager) SetGrantRollbackForTest(rollback func(*sql.Tx) error) {
	t.m.grantRollback = rollback
}

// SetPresenceRecheckForTest wires a focused fake capture executor.
func (t *TestTempGrantManager) SetPresenceRecheckForTest(recheck rbac.PresenceRecheck) {
	t.m.SetPresenceRecheck(recheck)
}

// Revoke exposes revokeTemporaryChannelAccess.
func (t *TestTempGrantManager) Revoke(ctx context.Context, serverID, channelID, userID, actorID string) error {
	_, err := t.m.revokeTemporaryChannelAccess(ctx, serverID, channelID, userID, actorID)
	return err
}

// HasTemporaryGrant exposes hasTemporaryGrant.
func (t *TestTempGrantManager) HasTemporaryGrant(ctx context.Context, channelID, userID string) (bool, error) {
	return t.m.hasTemporaryGrant(ctx, channelID, userID)
}

// SweepOrphanedTempGrants exposes the sweeper's orphan sweep for testing (#487 T9).
// Returns the number of orphaned temp grants revoked.
func (s *TempGrantSweeper) SweepOrphanedTempGrants(ctx context.Context) (int, error) {
	return s.sweepOrphanedTempGrants(ctx)
}

// The four bare wrappers below stamp receipt at the moment the test calls them,
// which for a DIRECT handler call is exactly what the dispatcher would have
// stamped: there is no queue between the two, so there is no lateness to carry.
// Use the *At forms whenever the clamp reference is what the test is about --
// a fixture that needs a receipt clock distinct from wall clock, or one that
// must prove a handler did not substitute its own time.Now() (#3205).
// RevokeOrphanedTempGrantForTest exposes the post-selection guarded cleanup so
// its fresh-rejoin recheck can be verified without weakening production scope.
func (s *TempGrantSweeper) RevokeOrphanedTempGrantForTest(
	ctx context.Context, serverID, channelID, userID string,
) (bool, error) {
	return s.mgr.revokeOrphanedTemporaryChannelAccess(ctx, serverID, channelID, userID)
}

// TestMediaBootID is the media boot the exported lifecycle hooks below model
// when a fixture's payload names none. Every pre-#174 fixture describes one
// media plane, so they share it; a test of the boot binding itself names its
// boots explicitly in the payload, and that value is never replaced.
var TestMediaBootID = uuid.MustParse("6f0c2a4e-1d3b-4c5a-9e7f-8a9b0c1d2e3f")

// withTestMediaBoot adds TestMediaBootID to a lifecycle payload that lacks a
// nodeBootId. A payload that is not a JSON object is returned unchanged, so
// malformed-event fixtures still exercise their parse failure.
func withTestMediaBoot(data []byte) []byte {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return data
	}
	if _, named := fields["nodeBootId"]; named {
		return data
	}
	fields["nodeBootId"] = json.RawMessage(`"` + TestMediaBootID.String() + `"`)
	stamped, err := json.Marshal(fields)
	if err != nil {
		return data
	}
	return stamped
}

// WithLifecycleOriginForTest binds a context to a media boot, as a lifecycle
// handler does with the decoded nodeBootId.
func WithLifecycleOriginForTest(ctx context.Context, boot uuid.UUID, receivedAt time.Time) context.Context {
	return withLifecycleReceivedAt(withLifecycleOrigin(ctx, boot), receivedAt)
}

// MutateServerVoiceParticipantForTest calls the Server Voice write sink with
// the caller's context exactly as given and no admission.
func (s *NATSSubscriber) MutateServerVoiceParticipantForTest(
	ctx context.Context,
	channelID, senderID uuid.UUID,
	eventAt time.Time,
) (bool, error) {
	_, applied, _, err := s.mutateServerVoiceParticipant(ctx, channelID, senderID, eventAt, nil)
	return applied, err
}

// ClearClaimResolverForTest models a subscriber built without a resolver.
func (s *NATSSubscriber) ClearClaimResolverForTest() { s.resolver = nil }

// testAdmissionNamespace derives a fixture's admission from its exact payload,
// so a replayed delivery of one join carries the same admission pair.
var testAdmissionNamespace = uuid.MustParse("3a4f1c9e-7b2d-4e6a-9c1f-5d8e2b7a0c43")

// testAdmissionsReserved remembers each derived admission once it is reserved,
// so a replay of the same payload finds its reservation consumed, exactly as a
// second delivery of one admitted join does in production.
var testAdmissionsReserved sync.Map

// withTestAdmission models the media plane's admitted server join for a
// fixture payload that names none: the media plane always sends the A1 pair on
// a channel join (VULN-109), and the control plane reserved it at A1. It
// reserves a pending admission derived from the payload and adds the pair. A
// payload that already names an admission, a DM room, or anything that cannot
// be reserved is returned unchanged. A test of the missing-admission rule
// itself calls HandleJoinedWithoutAdmissionAt.
func (s *NATSSubscriber) withTestAdmission(data []byte) []byte {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return data
	}
	if _, named := fields["admissionId"]; named {
		return data
	}
	if _, named := fields["socketId"]; named {
		return data
	}
	var channelID, userID string
	if json.Unmarshal(fields["channelId"], &channelID) != nil || json.Unmarshal(fields["userId"], &userID) != nil {
		return data
	}
	if _, err := uuid.Parse(channelID); err != nil || s.db == nil {
		return data
	}
	admission := uuid.NewSHA1(testAdmissionNamespace, data)
	socketID := "test-socket-" + admission.String()[:8]
	if _, replay := testAdmissionsReserved.LoadOrStore(admission, true); !replay {
		if _, err := s.db.Exec(`
			INSERT INTO voice_pending_admissions (channel_id, user_id, admission_id, socket_id, expires_at)
			SELECT c.id, $2, $3, $4, clock_timestamp() + INTERVAL '1 minute'
			FROM channels c WHERE c.id = $1
			ON CONFLICT (channel_id, user_id) DO UPDATE
			SET admission_id = EXCLUDED.admission_id, socket_id = EXCLUDED.socket_id,
			    expires_at = EXCLUDED.expires_at
		`, channelID, userID, admission, socketID); err != nil {
			testAdmissionsReserved.Delete(admission)
			return data
		}
	}
	var isChannel bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM channels WHERE id = $1)`, channelID).Scan(&isChannel); err != nil || !isChannel {
		return data
	}
	fields["admissionId"] = json.RawMessage(`"` + admission.String() + `"`)
	fields["socketId"] = json.RawMessage(`"` + socketID + `"`)
	stamped, err := json.Marshal(fields)
	if err != nil {
		return data
	}
	return stamped
}

// HandleJoined exposes handleJoined for testing.
func (s *NATSSubscriber) HandleJoined(data []byte) {
	s.handleJoined(s.withTestAdmission(withTestMediaBoot(data)), time.Now())
}

// HandleJoinedAt exposes handleJoined with an explicit NATS receipt time.
func (s *NATSSubscriber) HandleJoinedAt(data []byte, receivedAt time.Time) {
	s.handleJoined(s.withTestAdmission(withTestMediaBoot(data)), receivedAt)
}

// HandleJoinedWithoutAdmissionAt delivers voice.joined exactly as given, with
// no admission added.
func (s *NATSSubscriber) HandleJoinedWithoutAdmissionAt(data []byte, receivedAt time.Time) {
	s.handleJoined(withTestMediaBoot(data), receivedAt)
}

// HandleLeft exposes handleLeft for testing.
func (s *NATSSubscriber) HandleLeft(data []byte) { s.handleLeft(withTestMediaBoot(data), time.Now()) }

// HandleLeftAt exposes handleLeft with an explicit NATS receipt time.
func (s *NATSSubscriber) HandleLeftAt(data []byte, receivedAt time.Time) {
	s.handleLeft(withTestMediaBoot(data), receivedAt)
}

// HandleVoiceLifecycleEventForTest exercises the same placement dispatcher
// used for NATS lifecycle delivery.
func (s *NATSSubscriber) HandleVoiceLifecycleEventForTest(subject string, data []byte, receivedAt time.Time) {
	data = withTestMediaBoot(data)
	if subject == natsSubjectVoiceJoined {
		data = s.withTestAdmission(data)
	}
	s.handleVoiceLifecycleEvent(subject, data, receivedAt)
}

// HandleRoomEmpty exposes handleRoomEmpty for testing.
func (s *NATSSubscriber) HandleRoomEmpty(data []byte) {
	s.handleRoomEmpty(withTestMediaBoot(data), time.Now())
}

// HandleRoomEmptyAt exposes handleRoomEmpty with an explicit NATS receipt time.
func (s *NATSSubscriber) HandleRoomEmptyAt(data []byte, receivedAt time.Time) {
	s.handleRoomEmpty(withTestMediaBoot(data), receivedAt)
}

// HandleDMRoomEmptyReplicaForTest exercises one remote replica's terminal path
// without the process-local lifecycle lock shared by in-process test replicas.
//
// receivedAt is a REQUIRED parameter rather than a time.Now() taken inside, so
// no test path can model drain-time clamping (#3205).
func (s *NATSSubscriber) HandleDMRoomEmptyReplicaForTest(
	data []byte,
	conversationID uuid.UUID,
	receivedAt time.Time,
) bool {
	var event voiceRoomEmptyEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return false
	}
	eventAt, _, err := parseVoiceEventTime(event.Timestamp, receivedAt)
	if err != nil {
		return false
	}
	return s.handleDMRoomEmpty(event, conversationID, eventAt)
}

// HandleHeartbeat exposes handleHeartbeat for testing.
func (s *NATSSubscriber) HandleHeartbeat(data []byte) {
	s.handleHeartbeat(withTestMediaBoot(data), time.Now())
}

// HandleHeartbeatAt exposes handleHeartbeat with an explicit NATS receipt time.
func (s *NATSSubscriber) HandleHeartbeatAt(data []byte, receivedAt time.Time) {
	s.handleHeartbeat(withTestMediaBoot(data), receivedAt)
}

// SetDisconnectAllRichPresenceClientsHookForTest prevents the conservative
// reconnect from hiding messages emitted before a failed lifecycle operation.
func (s *NATSSubscriber) SetDisconnectAllRichPresenceClientsHookForTest(hook func()) {
	s.disconnectAllRichPresenceClientsHook = hook
}

// TestRoomContext is an exported wrapper around roomContext for testing.
type TestRoomContext struct {
	IsDM     bool
	ServerID string
}

// ResolveRoom exposes resolveRoom for testing and returns a TestRoomContext.
func (s *NATSSubscriber) ResolveRoom(channelID string) (*TestRoomContext, error) {
	ctx, err := s.resolveRoom(context.Background(), channelID)
	if err != nil {
		return nil, err
	}
	return &TestRoomContext{
		IsDM:     ctx.isDM,
		ServerID: ctx.serverID,
	}, nil
}

// ReEnforceServer exposes reEnforceServer for testing.
func (s *NATSSubscriber) ReEnforceServer(serverID, channelID, userID string) {
	s.reEnforceServer(context.Background(), serverID, channelID, userID)
}

// ReEnforceDM exposes reEnforceDM for testing.
func (s *NATSSubscriber) ReEnforceDM(channelID, userID string) {
	s.reEnforceDM(context.Background(), channelID, userID)
}

// PublishForceDisconnect exposes publishForceDisconnect for testing (#487 P3).
func (s *NATSSubscriber) PublishForceDisconnect(channelID, userID string) {
	s.publishForceDisconnect(channelID, userID)
}

// StaleServerVoiceDiscoverySQLForTest exposes the lease-discovery statement so
// the plan-shape guard runs against the exact SQL production issues.
func StaleServerVoiceDiscoverySQLForTest() string { return staleServerVoiceDiscoverySQL }

// ParseVoiceEventTimeForTest exposes the clamp so its branches are reachable
// without a database or a dispatcher. #3205: nothing pinned this function
// before, and the clamp changes a return VALUE rather than an error, so no
// existing test could have failed on it.
func ParseVoiceEventTimeForTest(
	raw string, receivedAt time.Time,
) (time.Time, time.Duration, error) {
	return parseVoiceEventTime(raw, receivedAt)
}

// MaxVoiceLifecycleForwardSkewForTest exposes the renewal's forward-skew ceiling
// so a regression test binds to the constant rather than to a copied literal.
const MaxVoiceLifecycleForwardSkewForTest = maxVoiceLifecycleForwardSkew

// RenewObservedLeaseForFutureStampedRowsForTest drives the set-based renewal
// directly, so its predicate and degrade-on-failure branch are both reachable.
func (s *NATSSubscriber) RenewObservedLeaseForFutureStampedRowsForTest(
	ctx context.Context,
	channelID uuid.UUID,
	participantIDs []uuid.UUID,
) {
	if _, named := lifecycleOrigin(ctx); !named {
		ctx = withLifecycleOrigin(ctx, TestMediaBootID)
	}
	s.renewObservedLeaseForFutureStampedRows(ctx, channelID, participantIDs)
}

// UseTierViolationQueueForTest gives a subscriber its own queue group, so a
// live-broker test is not sharing control-plane.tier-violation with every other
// control plane a parallel test package starts on the same broker.
func (s *NATSSubscriber) UseTierViolationQueueForTest(queue string) {
	s.tierViolationQueue = queue
}

// BroadcastPrivateVoiceLeft exposes the DM "left" broadcaster (#4179).
func (s *NATSSubscriber) BroadcastPrivateVoiceLeft(conversationID, senderID, callID uuid.UUID) {
	s.broadcastPrivateVoiceLeft(conversationID, senderID, callID)
}

// BroadcastPrivateVoiceRoomEmpty exposes the DM "room_empty" broadcaster (#4179).
func (s *NATSSubscriber) BroadcastPrivateVoiceRoomEmpty(channelID, callID string, conversationID uuid.UUID) {
	s.broadcastPrivateVoiceRoomEmpty(voiceRoomEmptyEvent{ChannelID: channelID, CallID: callID}, conversationID)
}

// LockVoiceParticipantUsersForTest exposes lockVoiceParticipantUsersTx in share
// mode, whether its error is the missing-user sentinel, and the IDs it names.
func LockVoiceParticipantUsersForTest(
	ctx context.Context,
	tx *sql.Tx,
	userIDs []uuid.UUID,
) (missing []uuid.UUID, isMissing bool, err error) {
	err = lockVoiceParticipantUsersTx(ctx, tx, userIDs, dmblock.LockShare)
	var typed *voiceParticipantUsersMissingError
	if errors.As(err, &typed) {
		missing = typed.Missing()
	}
	return missing, errors.Is(err, errVoiceParticipantUserMissing), err
}
