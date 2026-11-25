package voice_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	dbtest "github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/voice"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const pathTempAccess = "/temp-access"

// TestRevokeTempAccess_RevokesGrant: a MOVE_MEMBERS holder revoking a target's
// move-granted temp SBAC override → 200 {revoked:true}, the override is deleted, a
// key_revocations row is inserted (CSK rotated). The force-disconnect publish path
// is exercised via revokeTemporaryChannelAccess.
func TestRevokeTempAccess_RevokesGrant(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "tar_grant_owner")
	mover := ts.CreateTestUser(t, "tar_grant_mover")
	target := ts.CreateTestUser(t, "tar_grant_target")
	serverID := ts.CreateTestServer(t, owner.ID, "RevokeTemp Grant")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)

	moverRole := ts.CreateTestRole(t, serverID, "Organizer", 5, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, serverID, mover.ID, moverRole)

	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tar-grant")
	seedTempGrant(t, ts, serverID, channelID, target.ID)
	require.True(t, tempOverrideExists(t, ts.DB, channelID, target.ID))

	w := ts.DoRequest("DELETE", voiceEnforcePath(serverID, target.ID, pathTempAccess),
		map[string]interface{}{"channel_id": channelID}, testhelpers.AuthHeaders(mover.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, true, body["revoked"])
	assert.False(t, tempOverrideExists(t, ts.DB, channelID, target.ID), "temp override deleted")
	assert.Equal(t, 1, keyRevocationCount(t, ts.DB, channelID), "CSK rotated on revoke")
}

func TestRevokeTempAccess_SerializesOnTargetVoiceLifecycle(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "tar_lock_owner")
	mover := ts.CreateTestUser(t, "tar_lock_mover")
	target := ts.CreateTestUser(t, "tar_lock_target")
	serverID := ts.CreateTestServer(t, owner.ID, "RevokeTemp Lifecycle Lock")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	moverRole := ts.CreateTestRole(t, serverID, "Organizer", 5, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, serverID, mover.ID, moverRole)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tar-lock")
	seedTempGrant(t, ts, serverID, channelID, target.ID)
	tgSeedChannelKey(t, ts.DB, channelID, target.ID)
	tgSeedPendingKeyRequest(t, ts.DB, channelID, target.ID)

	blocker, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	targetID := uuid.MustParse(target.ID)
	require.NoError(t, voice.LockServerVoiceLifecycleTx(context.Background(), blocker, targetID))
	lockKey, err := voice.ServerVoiceLifecycleAdvisoryKeyForTest(targetID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blocker.Rollback() })

	result := make(chan int, 1)
	go func() {
		w := ts.DoRequest("DELETE", voiceEnforcePath(serverID, target.ID, pathTempAccess),
			map[string]interface{}{"channel_id": channelID}, testhelpers.AuthHeaders(mover.AccessToken))
		result <- w.Code
	}()
	dbtest.WaitForAdvisoryLockWaiter(t, ts.DB, lockKey)
	assert.True(t, tempOverrideExists(t, ts.DB, channelID, target.ID),
		"moderator revoke must wait for the target lifecycle lock")
	assert.True(t, tgChannelKeyExists(t, ts.DB, channelID, target.ID))
	assert.True(t, tgPendingKeyRequestExists(t, ts.DB, channelID, target.ID))
	assert.Zero(t, keyRevocationCount(t, ts.DB, channelID))

	require.NoError(t, blocker.Commit())
	select {
	case status := <-result:
		assert.Equal(t, http.StatusOK, status)
	case <-time.After(5 * time.Second):
		t.Fatal("moderator revoke did not finish after lifecycle lock release")
	}
	assert.False(t, tempOverrideExists(t, ts.DB, channelID, target.ID))
	assert.False(t, tgChannelKeyExists(t, ts.DB, channelID, target.ID))
	assert.False(t, tgPendingKeyRequestExists(t, ts.DB, channelID, target.ID))
	assert.Equal(t, 1, keyRevocationCount(t, ts.DB, channelID))
}

// Ordinary channel-override writes and temporary-grant cleanup may contend for
// the same channel while holding different subject locks. Their shared order
// must be visibility -> lifecycle -> channel: both requests should complete
// under contention instead of forming a lock cycle.
func TestOrdinaryChannelOverrideAndTempCleanupCompleteWithoutDeadlock(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "order_owner")
	target := ts.CreateTestUser(t, "order_target")
	other := ts.CreateTestUser(t, "order_other")
	serverID := ts.CreateTestServer(t, owner.ID, "Authority lock order")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	ts.AddMemberToServer(t, serverID, other.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "order-channel")
	seedTempGrant(t, ts, serverID, channelID, target.ID)

	start := make(chan struct{})
	results := make(chan int, 2)
	go func() {
		<-start
		w := ts.DoRequest("PUT", "/api/v1/channels/"+channelID+"/overrides", map[string]interface{}{
			// Decimal string, as every override bitfield travels since #3406: a
			// JSON number is refused at bind with 400 before either write runs.
			"target_type": "user", "target_id": other.ID, "allow": strconv.FormatInt(int64(rbac.PermViewVoiceChannels), 10),
		}, testhelpers.AuthHeaders(owner.AccessToken))
		results <- w.Code
	}()
	go func() {
		<-start
		w := ts.DoRequest("DELETE", voiceEnforcePath(serverID, target.ID, pathTempAccess),
			map[string]interface{}{"channel_id": channelID}, testhelpers.AuthHeaders(owner.AccessToken))
		results <- w.Code
	}()
	close(start)

	for i := 0; i < 2; i++ {
		select {
		case status := <-results:
			assert.Contains(t, []int{http.StatusOK, http.StatusConflict}, status,
				"contending authority writes must return rather than deadlock")
		case <-time.After(5 * time.Second):
			t.Fatal("ordinary override and temporary cleanup did not complete")
		}
	}
}

func TestRevokeTempAccess_RotatesDuringInitialDistribution(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "tar_defer_owner")
	mover := ts.CreateTestUser(t, "tar_defer_mover")
	target := ts.CreateTestUser(t, "tar_defer_target")
	serverID := ts.CreateTestServer(t, owner.ID, "RevokeTemp Deferred")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	moverRole := ts.CreateTestRole(t, serverID, "Organizer", 5, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, serverID, mover.ID, moverRole)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tar-defer")
	seedTempGrant(t, ts, serverID, channelID, target.ID)
	_, err := ts.DB.Exec(
		`INSERT INTO channel_initial_key_distributions (channel_id, creator_id) VALUES ($1, $2)`, channelID, owner.ID,
	)
	require.NoError(t, err)

	w := ts.DoRequest("DELETE", voiceEnforcePath(serverID, target.ID, pathTempAccess),
		map[string]interface{}{"channel_id": channelID}, testhelpers.AuthHeaders(mover.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	var markerEpoch int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT key_version FROM channel_initial_key_distributions WHERE channel_id = $1`, channelID,
	).Scan(&markerEpoch))
	assert.Equal(t, 2, markerEpoch)
	assert.Equal(t, 1, keyRevocationCount(t, ts.DB, channelID))
}

// TestRevokeTempAccess_PermanentGrantNoOp: when only a PERMANENT override exists
// for (user, channel), the revoke is a no-op → 200 {revoked:false}, the permanent
// override survives, and no CSK rotation happens.
func TestRevokeTempAccess_PermanentGrantNoOp(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "tar_perm_owner")
	target := ts.CreateTestUser(t, "tar_perm_target")
	serverID := ts.CreateTestServer(t, owner.ID, "RevokeTemp Perm")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tar-perm")

	// Permanent (is_temporary defaults false) user override.
	ts.CreateChannelOverride(t, channelID, "user", target.ID,
		int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice), 0)

	w := ts.DoRequest("DELETE", voiceEnforcePath(serverID, target.ID, pathTempAccess),
		map[string]interface{}{"channel_id": channelID}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, false, body["revoked"], "no temp grant → no-op")

	var exists bool
	require.NoError(t, ts.DB.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM channel_permission_overrides WHERE channel_id=$1 AND target_id=$2)`,
		channelID, target.ID,
	).Scan(&exists))
	assert.True(t, exists, "permanent override must survive a temp-access revoke")
	assert.Equal(t, 0, keyRevocationCount(t, ts.DB, channelID), "no rotation when nothing revoked")
}

// TestRevokeTempAccess_NoGrantNoOp: no override at all → 200 {revoked:false}.
func TestRevokeTempAccess_NoGrantNoOp(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "tar_none_owner")
	target := ts.CreateTestUser(t, "tar_none_target")
	serverID := ts.CreateTestServer(t, owner.ID, "RevokeTemp None")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tar-none")

	w := ts.DoRequest("DELETE", voiceEnforcePath(serverID, target.ID, pathTempAccess),
		map[string]interface{}{"channel_id": channelID}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, false, body["revoked"])
}

// TestRevokeTempAccess_NoPermission: a base member without MOVE_MEMBERS → 403.
func TestRevokeTempAccess_NoPermission(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "tar_np_owner")
	actor := ts.CreateTestUser(t, "tar_np_actor")
	target := ts.CreateTestUser(t, "tar_np_target")
	serverID := ts.CreateTestServer(t, owner.ID, "RevokeTemp NoPerm")
	ts.AddMemberToServer(t, serverID, actor.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tar-np")
	seedTempGrant(t, ts, serverID, channelID, target.ID)

	w := ts.DoRequest("DELETE", voiceEnforcePath(serverID, target.ID, pathTempAccess),
		map[string]interface{}{"channel_id": channelID}, testhelpers.AuthHeaders(actor.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
	// Override must be untouched on an unauthorized attempt.
	assert.True(t, tempOverrideExists(t, ts.DB, channelID, target.ID), "denied revoke must not delete the grant")
}

// TestRevokeTempAccess_HierarchyBlocked: revoke RESPECTS hierarchy — a
// MOVE_MEMBERS holder cannot revoke a higher-ranked member's grant → 403.
func TestRevokeTempAccess_HierarchyBlocked(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "tar_hier_owner")
	mover := ts.CreateTestUser(t, "tar_hier_mover")
	target := ts.CreateTestUser(t, "tar_hier_target")
	serverID := ts.CreateTestServer(t, owner.ID, "RevokeTemp Hierarchy")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)

	moverRole := ts.CreateTestRole(t, serverID, "Mover", 5, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, serverID, mover.ID, moverRole)
	higherRole := ts.CreateTestRole(t, serverID, "Senior", 10, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, serverID, target.ID, higherRole)

	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tar-hier")
	seedTempGrant(t, ts, serverID, channelID, target.ID)

	w := ts.DoRequest("DELETE", voiceEnforcePath(serverID, target.ID, pathTempAccess),
		map[string]interface{}{"channel_id": channelID}, testhelpers.AuthHeaders(mover.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, "temp-access revoke enforces hierarchy")
}

// TestRevokeTempAccess_CrossServerIDOR: a moderator with MOVE_MEMBERS in server A
// cannot revoke a temp grant on a voice channel in server B by passing server B's
// channel_id on server A's path. authorizeVoiceMod only authorized the actor for
// server A (:id), so the body channel_id MUST be scoped to that server. Expect 400
// AND that server B's temp override is STILL PRESENT (not deleted) and NO
// key_revocations row was inserted for server B's channel (CSK not rotated).
// Regression lock for the cross-server IDOR G6 fix (#487).
func TestRevokeTempAccess_CrossServerIDOR(t *testing.T) {
	ts := setupTS(t)
	// Server A: actor holds MOVE_MEMBERS.
	ownerA := ts.CreateTestUser(t, "tar_idor_ownerA")
	mover := ts.CreateTestUser(t, "tar_idor_mover")
	target := ts.CreateTestUser(t, "tar_idor_target")
	serverA := ts.CreateTestServer(t, ownerA.ID, "RevokeTemp IDOR A")
	ts.AddMemberToServer(t, serverA, mover.ID, roleMember)
	ts.AddMemberToServer(t, serverA, target.ID, roleMember)
	moverRole := ts.CreateTestRole(t, serverA, "Organizer", 5, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, serverA, mover.ID, moverRole)

	// Server B: a DIFFERENT server with a temp grant for target on a voice channel.
	ownerB := ts.CreateTestUser(t, "tar_idor_ownerB")
	serverB := ts.CreateTestServer(t, ownerB.ID, "RevokeTemp IDOR B")
	ts.AddMemberToServer(t, serverB, target.ID, roleMember)
	channelB := ts.CreateVoiceChannel(t, serverB, "voice-idor-b")
	seedTempGrant(t, ts, serverB, channelB, target.ID)
	require.True(t, tempOverrideExists(t, ts.DB, channelB, target.ID), "precondition: server-B temp grant exists")

	// Actor (authorized for server A only) tries to revoke server B's grant by
	// passing channel B's id on server A's path.
	w := ts.DoRequest("DELETE", voiceEnforcePath(serverA, target.ID, pathTempAccess),
		map[string]interface{}{"channel_id": channelB}, testhelpers.AuthHeaders(mover.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, "cross-server channel_id is refused like any inaccessible channel")

	// The cross-server IDOR guard must leave server B's state untouched.
	assert.True(t, tempOverrideExists(t, ts.DB, channelB, target.ID),
		"cross-server attempt must NOT delete server B's temp override")
	assert.Equal(t, 0, keyRevocationCount(t, ts.DB, channelB),
		"cross-server attempt must NOT rotate server B's channel CSK")
}

// TestRevokeTempAccess_MissingBody: no channel_id in body → 400.
func TestRevokeTempAccess_MissingBody(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "tar_mb_owner")
	target := ts.CreateTestUser(t, "tar_mb_target")
	serverID := ts.CreateTestServer(t, owner.ID, "RevokeTemp MissingBody")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)

	w := ts.DoRequest("DELETE", voiceEnforcePath(serverID, target.ID, pathTempAccess),
		map[string]interface{}{}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// revokeFixture: a mover holding MOVE_MEMBERS and a temp grant for the target on a
// fresh voice channel. Each refusal test adds one channel-scope defect.
func newRevokeScopeFixture(t *testing.T, tag string) (moveFixture, string) {
	t.Helper()
	f := newMoveFixture(t, tag, false)
	ch := f.ts.CreateVoiceChannel(t, f.serverID, tag+"-grant")
	seedTempGrant(t, f.ts, f.serverID, ch, f.target.ID)
	require.True(t, tempOverrideExists(t, f.ts.DB, ch, f.target.ID))
	return f, ch
}

func (f moveFixture) revoke(t *testing.T, channelID string) *httptest.ResponseRecorder {
	t.Helper()
	return f.ts.DoRequest("DELETE", voiceEnforcePath(f.serverID, f.target.ID, pathTempAccess),
		map[string]interface{}{"channel_id": channelID}, testhelpers.AuthHeaders(f.mover.AccessToken))
}

func TestRevokeTempAccess_ScopeControl_Succeeds(t *testing.T) {
	f, ch := newRevokeScopeFixture(t, "tars_ctl")
	w := f.revoke(t, ch)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.False(t, tempOverrideExists(t, f.ts.DB, ch, f.target.ID))
}

func TestRevokeTempAccess_ChannelDenyMove_403(t *testing.T) {
	f, ch := newRevokeScopeFixture(t, "tars_deny")
	f.ts.CreateChannelOverride(t, ch, "user", f.mover.ID, 0, int64(rbac.PermMoveMembers))
	w := f.revoke(t, ch)
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.JSONEq(t, insufficientBody, w.Body.String())
	assert.True(t, tempOverrideExists(t, f.ts.DB, ch, f.target.ID), "refused revoke must leave the grant")
}

func TestRevokeTempAccess_NoView_403(t *testing.T) {
	f, ch := newRevokeScopeFixture(t, "tars_noview")
	f.ts.CreateChannelOverride(t, ch, "user", f.mover.ID, 0, int64(rbac.PermViewVoiceChannels))
	w := f.revoke(t, ch)
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.True(t, tempOverrideExists(t, f.ts.DB, ch, f.target.ID))
}

func TestRevokeTempAccess_TextChannel_403(t *testing.T) {
	f, _ := newRevokeScopeFixture(t, "tars_text")
	text := f.ts.CreateTestChannel(t, f.serverID, "tars-text")
	f.ts.CreateChannelOverride(t, text, "user", f.target.ID, 0, 0)
	_, err := f.ts.DB.Exec(`UPDATE channel_permission_overrides SET is_temporary = true
		WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2`, text, f.target.ID)
	require.NoError(t, err)
	w := f.revoke(t, text)
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.True(t, tempOverrideExists(t, f.ts.DB, text, f.target.ID))
}

func TestRevokeTempAccess_OtherServerChannel_403(t *testing.T) {
	f, _ := newRevokeScopeFixture(t, "tars_other")
	ownerB := f.ts.CreateTestUser(t, "tars_other_ownerB")
	serverB := f.ts.CreateTestServer(t, ownerB.ID, "Revoke Other B")
	f.ts.AddMemberToServer(t, serverB, f.target.ID, roleMember)
	chB := f.ts.CreateVoiceChannel(t, serverB, "tars-other-b")
	seedTempGrant(t, f.ts, serverB, chB, f.target.ID)
	w := f.revoke(t, chB)
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.True(t, tempOverrideExists(t, f.ts.DB, chB, f.target.ID))
}
