package voice_test

import (
	"context"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Security#174 VULN-109: a Server Voice row exists only for a join the control
// plane admitted. A voice.joined for a server room with no admission must not
// plant a row, even for a member the claim rule would otherwise accept.

func (f bootBindingFixture) joined(t *testing.T, userID string, admission *uuid.UUID, socketID string, at time.Time) []byte {
	t.Helper()
	payload := map[string]interface{}{
		"channelId": f.channelID, "userId": userID, "username": "join_admission_user",
		"nodeBootId": f.bootX.String(), "timestamp": at.Format(time.RFC3339Nano),
	}
	if admission != nil {
		payload["admissionId"] = admission.String()
		payload["socketId"] = socketID
	}
	return mustJSON(t, payload)
}

func (f bootBindingFixture) reserveAdmission(t *testing.T, userID string, admission uuid.UUID, socketID string) {
	t.Helper()
	_, err := f.ts.DB.Exec(`
		INSERT INTO voice_pending_admissions (channel_id, user_id, admission_id, socket_id, expires_at)
		VALUES ($1, $2, $3, $4, clock_timestamp() + INTERVAL '1 minute')
	`, f.channelID, userID, admission, socketID)
	require.NoError(t, err)
}

func (f bootBindingFixture) pendingAdmissions(t *testing.T, userID string) int {
	t.Helper()
	var pending int
	require.NoError(t, f.ts.DB.QueryRow(`
		SELECT COUNT(*) FROM voice_pending_admissions WHERE channel_id = $1 AND user_id = $2
	`, f.channelID, userID).Scan(&pending))
	return pending
}

func TestServerJoinWithoutAdmissionCreatesNoRow(t *testing.T) {
	f := newBootBindingFixture(t)
	now := bootBindingNow()
	// The owner is a member with CONNECT: only the missing admission refuses it.
	f.subscriber.HandleJoinedWithoutAdmissionAt(f.joined(t, f.owner.ID, nil, "", now), now)

	_, written := f.stamp(t, f.owner.ID)
	assert.False(t, written, "a server voice.joined with no admission planted a row")
	assert.Zero(t, f.hooks.Load(), "a missing admission is a determined no-op, never a disconnect")
}

func TestServerJoinWithMismatchedAdmissionCreatesNoRow(t *testing.T) {
	f := newBootBindingFixture(t)
	f.reserveAdmission(t, f.owner.ID, uuid.New(), "socket-join-admission")
	now := bootBindingNow()
	other := uuid.New()
	f.subscriber.HandleJoinedAt(f.joined(t, f.owner.ID, &other, "socket-join-admission", now), now)

	_, written := f.stamp(t, f.owner.ID)
	assert.False(t, written, "a join whose admission was never reserved planted a row")
	assert.Equal(t, 1, f.pendingAdmissions(t, f.owner.ID), "a mismatched join consumed another reservation")
	assert.Zero(t, f.hooks.Load())
}

func TestServerJoinConsumesSuppliedAdmission(t *testing.T) {
	f := newBootBindingFixture(t)
	admission := uuid.New()
	f.reserveAdmission(t, f.owner.ID, admission, "socket-join-admission")
	now := bootBindingNow()
	event := f.joined(t, f.owner.ID, &admission, "socket-join-admission", now)

	f.subscriber.HandleJoinedAt(event, now)
	stamp, written := f.stamp(t, f.owner.ID)
	require.True(t, written, "an admitted join wrote no row")
	assert.Equal(t, f.bootX, stamp.UUID)
	assert.Zero(t, f.pendingAdmissions(t, f.owner.ID), "the admission was not consumed")
	observed := f.observedAt(t, f.owner.ID)

	// The replay of the same admitted join is the priorAdmitted case: it writes
	// nothing and ejects nobody.
	f.subscriber.HandleJoinedAt(event, now.Add(time.Millisecond))
	assert.Equal(t, 1, countVoiceParticipants(t, f.ts.DB, f.channelID))
	assert.True(t, f.observedAt(t, f.owner.ID).Equal(observed), "the replay rewrote the row")
	assert.Zero(t, f.hooks.Load())
}

func TestDMJoinWithoutAdmissionUnchanged(t *testing.T) {
	f := newBootBindingFixture(t)
	peer := f.ts.CreateTestUser(t, "join_admission_dm_peer")
	conversationID := f.ts.CreateDMConversation(t, f.owner.ID, peer.ID)
	callID := uuid.New()
	require.NoError(t, dm.RefreshDMVoiceCallLease(
		context.Background(), f.ts.Redis,
		dm.VoiceCallLease{
			ConversationID: uuid.MustParse(conversationID),
			CallID:         callID,
			CallerUserID:   uuid.MustParse(f.owner.ID),
		},
		dm.DMVoiceCallLeaseTTL,
		true,
	))
	now := bootBindingNow()
	f.subscriber.HandleJoinedAt(mustJSON(t, map[string]interface{}{
		"channelId": conversationID, "callId": callID.String(),
		"userId": f.owner.ID, "username": f.owner.Username,
		"nodeBootId": f.bootX.String(), "timestamp": now.Format(time.RFC3339Nano),
	}), now)

	var rows int
	require.NoError(t, f.ts.DB.QueryRow(`
		SELECT COUNT(*) FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2
	`, conversationID, f.owner.ID).Scan(&rows))
	assert.Equal(t, 1, rows, "a DM join needs no admission and must still write its row")
	assert.Zero(t, f.hooks.Load())
}
