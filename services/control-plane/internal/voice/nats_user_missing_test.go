package voice_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// VULN-108: a lifecycle event naming a user with no users row must be a
// determined no-op for that user. Before the fix the users-row lock failure
// reached both replica-wide disconnects: the ActivityService fail-closed arm
// and the handler's own disconnectAllRichPresenceClients.

// userMissingDelivery counts the ActivityService's replica-wide disconnect
// while delegating everything else to the real hub.
type userMissingDelivery struct {
	presence.Delivery
	all atomic.Int32
}

func (d *userMissingDelivery) DisconnectAllRichPresenceClients(ctx context.Context) error {
	d.all.Add(1)
	return d.Delivery.DisconnectAllRichPresenceClients(ctx)
}

type userMissingRig struct {
	ts        *testhelpers.TestServer
	sub       *voice.NATSSubscriber
	delivery  *userMissingDelivery
	hook      *atomic.Int32
	owner     testhelpers.TestUser
	channelID string
}

func newUserMissingRig(t *testing.T) userMissingRig {
	t.Helper()
	ts := testhelpers.SetupTestServer(t)
	delivery := &userMissingDelivery{Delivery: ts.Hub}
	sub := newTestSubscriberWithDelivery(ts, ts.Hub, nil, delivery)
	hook := &atomic.Int32{}
	sub.SetDisconnectAllRichPresenceClientsHookForTest(func() { hook.Add(1) })
	owner := ts.CreateTestUser(t, "vuln108_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "VULN-108 Server")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-vuln108")
	return userMissingRig{ts: ts, sub: sub, delivery: delivery, hook: hook, owner: owner, channelID: channelID}
}

func (r userMissingRig) assertNoReplicaDisconnect(t *testing.T) {
	t.Helper()
	assert.Zero(t, r.hook.Load(), "handler took the replica-wide disconnect")
	assert.Zero(t, r.delivery.all.Load(), "ActivityService took the replica-wide disconnect")
}

func (r userMissingRig) participantRows(t *testing.T, userID string) int {
	t.Helper()
	var rows int
	require.NoError(t, r.ts.DB.QueryRow(
		`SELECT COUNT(*) FROM voice_participants WHERE channel_id = $1 AND user_id = $2`,
		r.channelID, userID,
	).Scan(&rows))
	return rows
}

func TestJoinedUnknownUserDropsWithoutDisconnect(t *testing.T) {
	r := newUserMissingRig(t)
	ghost := uuid.NewString()
	now := time.Now().UTC().Truncate(time.Microsecond)

	r.sub.HandleJoinedAt(mustJSON(t, map[string]interface{}{
		"channelId": r.channelID, "userId": ghost, "username": "ghost",
		"timestamp": now.Format(time.RFC3339Nano),
	}), now)

	assert.Zero(t, r.participantRows(t, ghost))
	r.assertNoReplicaDisconnect(t)
}

func TestHeartbeatUnknownUserDropsOnlyThatUser(t *testing.T) {
	r := newUserMissingRig(t)
	insertVoiceParticipant(t, r.ts.DB, r.channelID, r.owner.ID)
	ghost := uuid.NewString()
	now := time.Now().UTC().Truncate(time.Microsecond)

	r.sub.HandleHeartbeatAt(mustJSON(t, map[string]interface{}{
		"channelId": r.channelID, "userIds": []string{r.owner.ID, ghost},
		"timestamp": now.Format(time.RFC3339Nano),
	}), now)

	var eventAt time.Time
	require.NoError(t, r.ts.DB.QueryRow(
		`SELECT lifecycle_event_at FROM voice_participants WHERE channel_id = $1 AND user_id = $2`,
		r.channelID, r.owner.ID,
	).Scan(&eventAt))
	assert.WithinDuration(t, now, eventAt, time.Millisecond, "known participant was not refreshed")
	assert.Zero(t, r.participantRows(t, ghost))
	r.assertNoReplicaDisconnect(t)
}

func TestHeartbeatAllUsersUnknownIsNoop(t *testing.T) {
	r := newUserMissingRig(t)
	ghosts := []string{uuid.NewString(), uuid.NewString()}
	now := time.Now().UTC().Truncate(time.Microsecond)

	r.sub.HandleHeartbeatAt(mustJSON(t, map[string]interface{}{
		"channelId": r.channelID, "userIds": ghosts,
		"timestamp": now.Format(time.RFC3339Nano),
	}), now)

	assert.Equal(t, 0, countVoiceParticipants(t, r.ts.DB, r.channelID))
	r.assertNoReplicaDisconnect(t)
}

func TestLockVoiceParticipantUsersMissingIsTyped(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	known := uuid.MustParse(ts.CreateTestUser(t, "vuln108_lock_known").ID)
	ghost := uuid.New()
	ctx := context.Background()

	tx, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	missing, isMissing, err := voice.LockVoiceParticipantUsersForTest(ctx, tx, []uuid.UUID{known, ghost})
	require.NoError(t, tx.Rollback())
	require.Error(t, err, "the exploit control requires the lock to keep failing")
	assert.True(t, isMissing, "error does not wrap errVoiceParticipantUserMissing: %v", err)
	assert.Equal(t, []uuid.UUID{ghost}, missing)

	tx, err = ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	missing, isMissing, err = voice.LockVoiceParticipantUsersForTest(ctx, tx, []uuid.UUID{known})
	require.NoError(t, tx.Rollback())
	require.NoError(t, err)
	assert.False(t, isMissing)
	assert.Empty(t, missing)
}

func TestRefreshMutationMatchesActivityUserMissing(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "vuln108_refresh_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "VULN-108 Refresh")
	channelID := uuid.MustParse(ts.CreateVoiceChannel(t, serverID, "voice-vuln108-refresh"))
	log := logger.New("test")
	resolver := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), log)
	store := presence.NewActivityStore(ts.Redis)
	delivery := &userMissingDelivery{Delivery: ts.Hub}
	activity := presence.NewActivityService(
		ts.PresenceHistory,
		presence.NewActivityBuilder(ts.DB, testCallLeaseVerifier{redis: ts.Redis}, store),
		store, ts.DB, resolver, delivery, permitAllPresence{},
	)
	scope := presence.Scope{
		Category: presence.CategoryServerVoice, RoomID: channelID,
		LifecycleID: channelID, EventAt: time.Now().UTC().Truncate(time.Microsecond),
	}

	err := activity.RefreshServerVoice(context.Background(), uuid.New(), scope,
		func(context.Context) (bool, error) {
			return false, fmt.Errorf("lock voice participant: %w", presence.ErrActivityUserMissing)
		})

	require.Error(t, err)
	assert.ErrorIs(t, err, presence.ErrActivityUserMissing)
	assert.Equal(t, presence.FailureUserMissing, presence.PolicyErrorClass(err))
	assert.Zero(t, delivery.all.Load(), "ActivityService took the replica-wide disconnect")
}
