package voice

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Refresh-only lifecycle authority (room placement) is a sibling write path of
// the Server Voice sink and is bound by the same boot rule (Security#174 §5.6).

func TestRefreshOnlyFrameWithoutOriginIsRefused(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	room := uuid.NewString()
	user := uuid.NewString()
	heartbeat, err := json.Marshal(map[string]any{"channelId": room, "userIds": []string{user}, "timestamp": now})
	require.NoError(t, err)
	_, ok := parseRefreshOnlyLifecycleFrame(natsSubjectVoiceHeartbeat, heartbeat)
	require.False(t, ok, "a refresh-only heartbeat with no nodeBootId was accepted")
	joined, err := json.Marshal(map[string]any{"channelId": room, "userId": user, "timestamp": now})
	require.NoError(t, err)
	_, ok = parseRefreshOnlyLifecycleFrame(natsSubjectVoiceJoined, joined)
	require.False(t, ok, "a refresh-only join with no nodeBootId was accepted")

	boot := uuid.New()
	stamped, err := json.Marshal(map[string]any{
		"channelId": room, "userIds": []string{user}, "timestamp": now, "nodeBootId": boot.String(),
	})
	require.NoError(t, err)
	frame, ok := parseRefreshOnlyLifecycleFrame(natsSubjectVoiceHeartbeat, stamped)
	require.True(t, ok)
	require.Equal(t, boot, frame.origin)
}

func TestRefreshOnlyHeartbeatLeavesForeignBootRow(t *testing.T) {
	subscriber, db, _, target, userID := refreshOnlyTestSubscriber(t)
	bootX, bootY := uuid.New(), uuid.New()
	oldTime := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	_, err := db.Exec(`
		INSERT INTO voice_participants (channel_id, user_id, lifecycle_event_at, media_boot_id)
		VALUES ($1, $2, $3, $4)
	`, target, userID, oldTime, bootY)
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Microsecond)

	require.NoError(t, subscriber.refreshOnlyHeartbeat(
		withLifecycleOrigin(context.Background(), bootX), target, []uuid.UUID{userID}, now,
	))
	var eventAt time.Time
	require.NoError(t, db.QueryRow(`SELECT lifecycle_event_at FROM voice_participants WHERE channel_id = $1 AND user_id = $2`, target, userID).Scan(&eventAt))
	require.Equal(t, oldTime, eventAt, "boot X refreshed boot Y's row")

	require.NoError(t, subscriber.refreshOnlyHeartbeat(context.Background(), target, []uuid.UUID{userID}, now))
	require.NoError(t, db.QueryRow(`SELECT lifecycle_event_at FROM voice_participants WHERE channel_id = $1 AND user_id = $2`, target, userID).Scan(&eventAt))
	require.Equal(t, oldTime, eventAt, "a refresh with no origin wrote a row")

	require.NoError(t, subscriber.refreshOnlyHeartbeat(
		withLifecycleOrigin(context.Background(), bootY), target, []uuid.UUID{userID}, now,
	))
	require.NoError(t, db.QueryRow(`SELECT lifecycle_event_at FROM voice_participants WHERE channel_id = $1 AND user_id = $2`, target, userID).Scan(&eventAt))
	require.Equal(t, now, eventAt, "the owning boot must still refresh its row")
}

func TestRefreshOnlyJoinedStampsOrigin(t *testing.T) {
	subscriber, db, _, target, userID := refreshOnlyTestSubscriber(t)
	admissionID := uuid.New()
	_, err := db.Exec(`
		INSERT INTO voice_pending_admissions (channel_id, user_id, admission_id, socket_id, expires_at)
		VALUES ($1, $2, $3, 'socket-boot', clock_timestamp() + INTERVAL '1 minute')
	`, target, userID, admissionID)
	require.NoError(t, err)
	require.NoError(t, subscriber.roomPlacement.MintJoinIntent(t.Context(), target.String(), userID.String()))
	boot := uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)

	require.NoError(t, subscriber.refreshOnlyJoined(
		withLifecycleOrigin(t.Context(), boot), target, userID, now,
		&voiceJoinedAdmission{admissionID: admissionID, socketID: "socket-boot"},
	))
	var stamp uuid.NullUUID
	require.NoError(t, db.QueryRow(`SELECT media_boot_id FROM voice_participants WHERE channel_id = $1 AND user_id = $2`, target, userID).Scan(&stamp))
	require.True(t, stamp.Valid, "a refresh-only join inserted an unstamped row")
	require.Equal(t, boot, stamp.UUID)
}
