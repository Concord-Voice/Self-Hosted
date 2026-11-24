package voice_test

import (
	"context"
	"github.com/google/uuid"
	"net/http"
	"strings"
	"testing"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/redistest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const pathMove = "/move"

// joinVoice inserts a voice_participants row so the target counts as in-voice.
func joinVoice(t *testing.T, ts *testhelpers.TestServer, channelID, userID string) {
	t.Helper()
	_, err := ts.DB.Exec(`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`, channelID, userID)
	require.NoError(t, err)
}

// hideVoiceChannel denies VIEW_VOICE for the @all role so a plain member cannot see
// (and thus cannot join) the channel — the precondition for a temp grant.
func hideVoiceChannel(t *testing.T, ts *testhelpers.TestServer, serverID, channelID string) {
	t.Helper()
	var allRoleID string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID,
	).Scan(&allRoleID))
	ts.CreateChannelOverride(t, channelID, "role", allRoleID, 0, int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice))
}

// denyViewVoiceForAll denies ONLY VIEW_VOICE for the @all role, leaving JoinVoice
// intact from base perms. This is the CV-CAN-006 dual-bit scenario: the target
// inherits JoinVoice but cannot SEE the channel, so AuthorizeJoin (which now
// requires both bits) would reject the moved client unless the move pregrant
// notices the missing ViewVoice bit and issues a temp grant.
func denyViewVoiceForAll(t *testing.T, ts *testhelpers.TestServer, serverID, channelID string) {
	t.Helper()
	var allRoleID string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID,
	).Scan(&allRoleID))
	ts.CreateChannelOverride(t, channelID, "role", allRoleID, 0, int64(rbac.PermViewVoiceChannels))
}

func moveTempOverrideExists(t *testing.T, ts *testhelpers.TestServer, channelID, userID string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, ts.DB.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM channel_permission_overrides
		   WHERE channel_id=$1 AND target_type='user' AND target_id=$2 AND is_temporary=true)`,
		channelID, userID,
	).Scan(&exists))
	return exists
}

func auditMoveCount(t *testing.T, ts *testhelpers.TestServer, serverID, targetID string) int {
	t.Helper()
	var n int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE server_id=$1 AND target_id=$2 AND action='voice_member_moved'`,
		serverID, targetID,
	).Scan(&n))
	return n
}

// TestServerMove_SelfMove: a user relocating themselves needs no permission, no
// hierarchy, and gets no temp grant.
func TestServerMove_SelfMove(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_self_owner")
	mover := ts.CreateTestUser(t, "mv_self_mover")
	serverID := ts.CreateTestServer(t, owner.ID, "Move Self")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	from := ts.CreateVoiceChannel(t, serverID, "voice-self-from")
	to := ts.CreateVoiceChannel(t, serverID, "voice-self-to")
	joinVoice(t, ts, from, mover.ID)

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, mover.ID, pathMove),
		map[string]interface{}{"target_channel_id": to}, testhelpers.AuthHeaders(mover.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.False(t, moveTempOverrideExists(t, ts, to, mover.ID), "self-move must not insert a temp grant")
	assert.Equal(t, 0, auditMoveCount(t, ts, serverID, mover.ID), "a self-move is never audited")
}

// TestServerMove_WithPermission_HiddenChannel_GrantsTemp: a MOVE_MEMBERS holder
// moving a lower-ranked member into a hidden channel → 200 + temp grant inserted.
func TestServerMove_WithPermission_HiddenChannel_GrantsTemp(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_grant_owner")
	mover := ts.CreateTestUser(t, "mv_grant_mover")
	target := ts.CreateTestUser(t, "mv_grant_target")
	serverID := ts.CreateTestServer(t, owner.ID, "Move Grant")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)

	// Mover (pos 5) has MOVE_MEMBERS; target stays base member (pos 0).
	moverRole := ts.CreateTestRole(t, serverID, "Organizer", 5, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, serverID, mover.ID, moverRole)

	from := ts.CreateVoiceChannel(t, serverID, "voice-grant-from")
	to := ts.CreateVoiceChannel(t, serverID, "voice-grant-to")
	hideVoiceChannel(t, ts, serverID, to) // target cannot see/join `to`
	// A USER override beats the @all role deny; a role allow cannot.
	ts.CreateChannelOverride(t, to, "user", mover.ID, int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice|rbac.PermSpeak), 0)
	joinVoice(t, ts, from, target.ID)

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathMove),
		map[string]interface{}{"target_channel_id": to}, testhelpers.AuthHeaders(mover.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, moveTempOverrideExists(t, ts, to, target.ID), "temp grant inserted when target lacked access")
	assert.Equal(t, 1, auditMoveCount(t, ts, serverID, target.ID), "a move that writes a temporary grant is audited")
	assert.Equal(t, "true", moveAuditMeta(t, ts, serverID, target.ID, "temporary_grant"))
	assert.Equal(t, "false", moveAuditMeta(t, ts, serverID, target.ID, "hierarchy_crossed"))
}

// TestServerMove_JoinInheritedViewDenied_GrantsTemp: the CV-CAN-006 dual-bit
// regression. A MOVE_MEMBERS holder moves a member who inherits JoinVoice but is
// denied ViewVoice on the destination. Because AuthorizeJoin now requires BOTH
// bits, the pregrant check must also use the ViewVoice+JoinVoice predicate — so a
// temp grant IS inserted even though JoinVoice alone is present. (Checking
// JoinVoice alone would skip the grant and AuthorizeJoin would then reject the
// moved client.)
func TestServerMove_JoinInheritedViewDenied_GrantsTemp(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_viewdenied_owner")
	mover := ts.CreateTestUser(t, "mv_viewdenied_mover")
	target := ts.CreateTestUser(t, "mv_viewdenied_target")
	serverID := ts.CreateTestServer(t, owner.ID, "Move ViewDenied")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)

	// Mover (pos 5) has MOVE_MEMBERS; target stays base member (inherits JoinVoice).
	moverRole := ts.CreateTestRole(t, serverID, "Organizer", 5, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, serverID, mover.ID, moverRole)

	from := ts.CreateVoiceChannel(t, serverID, "voice-viewdenied-from")
	to := ts.CreateVoiceChannel(t, serverID, "voice-viewdenied-to")
	denyViewVoiceForAll(t, ts, serverID, to) // target keeps JoinVoice, loses ViewVoice
	// A USER override beats the @all role deny; a role allow cannot.
	ts.CreateChannelOverride(t, to, "user", mover.ID, int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice|rbac.PermSpeak), 0)
	joinVoice(t, ts, from, target.ID)

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathMove),
		map[string]interface{}{"target_channel_id": to}, testhelpers.AuthHeaders(mover.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, moveTempOverrideExists(t, ts, to, target.ID),
		"temp grant must be inserted when target inherits JoinVoice but is denied ViewVoice")
	assert.Equal(t, 1, auditMoveCount(t, ts, serverID, target.ID), "a move that writes a temporary grant is audited")
}

func TestServerMove_StalePermissionCacheStillGrantsAccess(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_stale_cache_owner")
	mover := ts.CreateTestUser(t, "mv_stale_cache_mover")
	target := ts.CreateTestUser(t, "mv_stale_cache_target")
	serverID := ts.CreateTestServer(t, owner.ID, "Move Stale Cache")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)

	moverRole := ts.CreateTestRole(t, serverID, "Organizer", 5, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, serverID, mover.ID, moverRole)
	from := ts.CreateVoiceChannel(t, serverID, "voice-stale-cache-from")
	to := ts.CreateVoiceChannel(t, serverID, "voice-stale-cache-to")
	hideVoiceChannel(t, ts, serverID, to)
	// A USER override beats the @all role deny; a role allow cannot.
	ts.CreateChannelOverride(t, to, "user", mover.ID, int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice|rbac.PermSpeak), 0)
	joinVoice(t, ts, from, target.ID)

	// The transactional move preflight sees the durable deny; the stale cache
	// must not make the later grant decision skip the required temporary access.
	cache := rbac.NewPermissionCache(ts.Redis)
	tags := testhelpers.SeedPermissionGenerations(t, ts.Redis, serverID, target.ID)
	require.NoError(t, cache.Set(context.Background(), serverID, target.ID, to,
		rbac.PermViewVoiceChannels|rbac.PermJoinVoice, tags))

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathMove),
		map[string]interface{}{"target_channel_id": to}, testhelpers.AuthHeaders(mover.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, moveTempOverrideExists(t, ts, to, target.ID),
		"a stale cached allow must not suppress the preflight-required temporary grant")
}

// TestServerMove_PermanentViewDeny_RejectsBeforeSignal: the grant-integrity edge
// of the CV-CAN-006 dual-bit gate. The target has a PERMANENT (non-temporary)
// user override denying ViewVoice while still inheriting JoinVoice from base
// perms. The dual-bit predicate makes canJoin false, but
// grantTemporaryChannelAccess must NOT mask a permanent override (§6.3), so it
// no-ops. The move must therefore REJECT (409) instead of signaling a success the
// media-plane AuthorizeJoin would refuse, and no temp grant is inserted.
func TestServerMove_PermanentViewDeny_RejectsBeforeSignal(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_permdeny_owner")
	mover := ts.CreateTestUser(t, "mv_permdeny_mover")
	target := ts.CreateTestUser(t, "mv_permdeny_target")
	serverID := ts.CreateTestServer(t, owner.ID, "Move PermDeny")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)

	moverRole := ts.CreateTestRole(t, serverID, "Organizer", 5, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, serverID, mover.ID, moverRole)

	from := ts.CreateVoiceChannel(t, serverID, "voice-permdeny-from")
	to := ts.CreateVoiceChannel(t, serverID, "voice-permdeny-to")
	// PERMANENT (is_temporary defaults to FALSE) user override denying ONLY
	// ViewVoice; JoinVoice stays inherited from base perms.
	ts.CreateChannelOverride(t, to, "user", target.ID, 0, int64(rbac.PermViewVoiceChannels))
	joinVoice(t, ts, from, target.ID)

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathMove),
		map[string]interface{}{"target_channel_id": to}, testhelpers.AuthHeaders(mover.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code,
		"a permanent ViewVoice deny must reject the move, not signal a false success")
	assert.False(t, moveTempOverrideExists(t, ts, to, target.ID),
		"grant must not insert a temp override on top of a permanent one")
}

// TestServerMove_HierarchyBypass_AuditsCrossing: a MOVE_MEMBERS holder moving a
// HIGHER-ranked member → 200 (no hierarchy block) AND an audit_log entry.
func TestServerMove_HierarchyBypass_AuditsCrossing(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_hier_owner")
	mover := ts.CreateTestUser(t, "mv_hier_mover")
	target := ts.CreateTestUser(t, "mv_hier_target")
	serverID := ts.CreateTestServer(t, owner.ID, "Move Hierarchy")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)

	// Mover (pos 5) has MOVE_MEMBERS; target (pos 10) OUTRANKS the mover.
	moverRole := ts.CreateTestRole(t, serverID, "Mover", 5, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, serverID, mover.ID, moverRole)
	higherRole := ts.CreateTestRole(t, serverID, "Senior", 10, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, serverID, target.ID, higherRole)

	from := ts.CreateVoiceChannel(t, serverID, "voice-hier-from")
	to := ts.CreateVoiceChannel(t, serverID, "voice-hier-to")
	ts.CreateChannelOverride(t, to, "role", moverRole, int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice|rbac.PermSpeak), 0)
	joinVoice(t, ts, from, target.ID)

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathMove),
		map[string]interface{}{"target_channel_id": to}, testhelpers.AuthHeaders(mover.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code, "MOVE_MEMBERS bypasses hierarchy (ADR-0023)")
	assert.Equal(t, 1, auditMoveCount(t, ts, serverID, target.ID), "hierarchy-crossing move must be audited")
}

// TestServerMove_NoPermission: a base member without MOVE_MEMBERS → 403.
func TestServerMove_NoPermission(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_np_owner")
	mover := ts.CreateTestUser(t, "mv_np_mover")
	target := ts.CreateTestUser(t, "mv_np_target")
	serverID := ts.CreateTestServer(t, owner.ID, "Move NoPerm")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	from := ts.CreateVoiceChannel(t, serverID, "voice-np-from")
	to := ts.CreateVoiceChannel(t, serverID, "voice-np-to")
	joinVoice(t, ts, from, target.ID)

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathMove),
		map[string]interface{}{"target_channel_id": to}, testhelpers.AuthHeaders(mover.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// TestServerMove_CrossServerTarget: target_channel_id in another server → 400.
func TestServerMove_CrossServerTarget(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_cross_owner")
	target := ts.CreateTestUser(t, "mv_cross_target")
	serverID := ts.CreateTestServer(t, owner.ID, "Move Cross A")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	from := ts.CreateVoiceChannel(t, serverID, "voice-cross-from")
	joinVoice(t, ts, from, target.ID)

	// A voice channel in a DIFFERENT server.
	otherServer := ts.CreateTestServer(t, owner.ID, "Move Cross B")
	foreignChannel := ts.CreateVoiceChannel(t, otherServer, "voice-cross-foreign")

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathMove),
		map[string]interface{}{"target_channel_id": foreignChannel}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "Insufficient permissions")
}

// TestServerMove_NonVoiceTarget: target is a text channel → 400.
func TestServerMove_NonVoiceTarget(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_text_owner")
	target := ts.CreateTestUser(t, "mv_text_target")
	serverID := ts.CreateTestServer(t, owner.ID, "Move Text")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	from := ts.CreateVoiceChannel(t, serverID, "voice-text-from")
	textChannel := ts.CreateTestChannel(t, serverID, "text-target")
	joinVoice(t, ts, from, target.ID)

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathMove),
		map[string]interface{}{"target_channel_id": textChannel}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "Insufficient permissions")
}

// TestServerMove_TargetNotInVoice: target is not in any voice channel → 409.
func TestServerMove_TargetNotInVoice(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_niv_owner")
	target := ts.CreateTestUser(t, "mv_niv_target")
	serverID := ts.CreateTestServer(t, owner.ID, "Move NotInVoice")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	to := ts.CreateVoiceChannel(t, serverID, "voice-niv-to")

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathMove),
		map[string]interface{}{"target_channel_id": to}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code)
}

// TestServerMove_AlreadyInTarget: from == to → 400.
func TestServerMove_AlreadyInTarget(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_same_owner")
	target := ts.CreateTestUser(t, "mv_same_target")
	serverID := ts.CreateTestServer(t, owner.ID, "Move Same")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	to := ts.CreateVoiceChannel(t, serverID, "voice-same")
	joinVoice(t, ts, to, target.ID)

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathMove),
		map[string]interface{}{"target_channel_id": to}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestServerMove_MissingBody: no target_channel_id → 400.
func TestServerMove_MissingBody(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_mb_owner")
	target := ts.CreateTestUser(t, "mv_mb_target")
	serverID := ts.CreateTestServer(t, owner.ID, "Move MissingBody")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathMove),
		map[string]interface{}{}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestServerMove_InvalidServerID: a non-UUID :id path param → 400 before any
// permission/DB work (parseMoveRequest guard).
func TestServerMove_InvalidServerID(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_badsrv_owner")
	target := ts.CreateTestUser(t, "mv_badsrv_target")

	w := ts.DoRequest("POST", voiceEnforcePath("not-a-uuid", target.ID, pathMove),
		map[string]interface{}{"target_channel_id": target.ID}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestServerMove_InvalidUserID: a non-UUID :userId path param → 400 before any
// permission/DB work (parseMoveRequest guard).
func TestServerMove_InvalidUserID(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_baduser_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "Move BadUser")

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, "not-a-uuid", pathMove),
		map[string]interface{}{"target_channel_id": serverID}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestServerMove_Unauthenticated: no token → 401.
func TestServerMove_Unauthenticated(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mv_unauth_owner")
	target := ts.CreateTestUser(t, "mv_unauth_target")
	serverID := ts.CreateTestServer(t, owner.ID, "Move Unauth")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	to := ts.CreateVoiceChannel(t, serverID, "voice-unauth-move")

	w := ts.DoRequest("POST", voiceEnforcePath(serverID, target.ID, pathMove),
		map[string]interface{}{"target_channel_id": to}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func moveAuditMeta(t *testing.T, ts *testhelpers.TestServer, serverID, targetID, key string) string {
	t.Helper()
	var v string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT metadata->>$3 FROM audit_log
		   WHERE server_id=$1 AND target_id=$2 AND action='voice_member_moved'`,
		serverID, targetID, key,
	).Scan(&v))
	return v
}

const insufficientBody = `{"error":"Insufficient permissions"}`

// moveFixture: owner, a mover holding MOVE_MEMBERS (pos 5), a base-member target
// in `from`, and a visible destination `to`. The mover gets VIEW|CONNECT|SPEAK on
// `to` via a role override unless noDestAccess is set.
type moveFixture struct {
	ts        *testhelpers.TestServer
	owner     testhelpers.TestUser
	mover     testhelpers.TestUser
	target    testhelpers.TestUser
	serverID  string
	moverRole string
	from, to  string
}

func newMoveFixture(t *testing.T, tag string, destAccess bool) moveFixture {
	t.Helper()
	ts := setupTS(t)
	f := moveFixture{ts: ts}
	f.owner = ts.CreateTestUser(t, tag+"_owner")
	f.mover = ts.CreateTestUser(t, tag+"_mover")
	f.target = ts.CreateTestUser(t, tag+"_target")
	f.serverID = ts.CreateTestServer(t, f.owner.ID, "Move "+tag)
	ts.AddMemberToServer(t, f.serverID, f.mover.ID, roleMember)
	ts.AddMemberToServer(t, f.serverID, f.target.ID, roleMember)
	f.moverRole = ts.CreateTestRole(t, f.serverID, "Organizer", 5, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, f.serverID, f.mover.ID, f.moverRole)
	f.from = ts.CreateVoiceChannel(t, f.serverID, tag+"-from")
	f.to = ts.CreateVoiceChannel(t, f.serverID, tag+"-to")
	if destAccess {
		ts.CreateChannelOverride(t, f.to, "role", f.moverRole, int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice|rbac.PermSpeak), 0)
	}
	joinVoice(t, ts, f.from, f.target.ID)
	return f
}

func (f moveFixture) move(t *testing.T, actor testhelpers.TestUser, targetID, dest string) (int, string) {
	t.Helper()
	w := f.ts.DoRequest("POST", voiceEnforcePath(f.serverID, targetID, pathMove),
		map[string]interface{}{"target_channel_id": dest}, testhelpers.AuthHeaders(actor.AccessToken))
	return w.Code, strings.TrimSpace(w.Body.String())
}

func (f moveFixture) requireRefusedNoGrant(t *testing.T, code int, body string) {
	t.Helper()
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.JSONEq(t, insufficientBody, body)
	assert.False(t, moveTempOverrideExists(t, f.ts, f.to, f.target.ID), "refused move must not write a grant")
	assert.Equal(t, 0, auditMoveCount(t, f.ts, f.serverID, f.target.ID))
}

// Positive control for the refusal tests below.
func TestServerMove_FixtureControl_Succeeds(t *testing.T) {
	f := newMoveFixture(t, "mvc_ctl", true)
	code, body := f.move(t, f.mover, f.target.ID, f.to)
	assert.Equal(t, http.StatusOK, code, body)
}

func TestServerMove_MoverLacksDestinationView_403NoGrant(t *testing.T) {
	f := newMoveFixture(t, "mvc_noview", true)
	f.ts.CreateChannelOverride(t, f.to, "user", f.mover.ID, 0, int64(rbac.PermViewVoiceChannels))
	code, body := f.move(t, f.mover, f.target.ID, f.to)
	f.requireRefusedNoGrant(t, code, body)
}

func TestServerMove_MoverLacksDestinationJoin_403NoGrant(t *testing.T) {
	f := newMoveFixture(t, "mvc_nojoin", true)
	f.ts.CreateChannelOverride(t, f.to, "user", f.mover.ID, 0, int64(rbac.PermJoinVoice))
	code, body := f.move(t, f.mover, f.target.ID, f.to)
	f.requireRefusedNoGrant(t, code, body)
}

func TestServerMove_DestinationDenyMove_403NoGrant(t *testing.T) {
	f := newMoveFixture(t, "mvc_destdeny", true)
	f.ts.CreateChannelOverride(t, f.to, "user", f.mover.ID, 0, int64(rbac.PermMoveMembers))
	code, body := f.move(t, f.mover, f.target.ID, f.to)
	f.requireRefusedNoGrant(t, code, body)
}

func TestServerMove_SourceDenyMove_403NoGrant(t *testing.T) {
	f := newMoveFixture(t, "mvc_srcdeny", true)
	f.ts.CreateChannelOverride(t, f.from, "user", f.mover.ID, int64(rbac.PermViewVoiceChannels), int64(rbac.PermMoveMembers))
	code, body := f.move(t, f.mover, f.target.ID, f.to)
	f.requireRefusedNoGrant(t, code, body)
}

func TestServerMove_GrantNeededMoverLacksSpeak_403NoGrant(t *testing.T) {
	f := newMoveFixture(t, "mvc_nospeak", true)
	hideVoiceChannel(t, f.ts, f.serverID, f.to) // target needs a temporary grant
	// One user override (unique per channel/target): VIEW|JOIN allowed so the
	// refusal happens at SPEAK, not at VIEW.
	f.ts.CreateChannelOverride(t, f.to, "user", f.mover.ID, int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice), int64(rbac.PermSpeak))
	code, body := f.move(t, f.mover, f.target.ID, f.to)
	f.requireRefusedNoGrant(t, code, body)

	// Control: lifting only the SPEAK deny flips the result, proving the refusal
	// above was at SPEAK and not at VIEW/JOIN.
	_, err := f.ts.DB.Exec(`UPDATE channel_permission_overrides SET deny = 0
		WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2`, f.to, f.mover.ID)
	require.NoError(t, err)
	require.NoError(t, redistest.Reset(context.Background(), f.ts.Redis))
	code, body = f.move(t, f.mover, f.target.ID, f.to)
	assert.Equal(t, http.StatusOK, code, body)
}

func TestServerMove_MissingDestination_SameAsNoAccess(t *testing.T) {
	f := newMoveFixture(t, "mvc_missing", false)
	hideVoiceChannel(t, f.ts, f.serverID, f.to) // mover has no access to `to`
	_, noAccess := f.move(t, f.mover, f.target.ID, f.to)
	code, missing := f.move(t, f.mover, f.target.ID, uuid.NewString())
	assert.Equal(t, http.StatusForbidden, code)
	assert.JSONEq(t, insufficientBody, noAccess)
	assert.Equal(t, noAccess, missing, "a missing destination must not be distinguishable from a hidden one")
}

func TestServerMove_TargetInHiddenChannel_409LikeNotInVoice(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mvc_hidden_owner")
	mover := ts.CreateTestUser(t, "mvc_hidden_mover")
	target := ts.CreateTestUser(t, "mvc_hidden_target")
	serverID := ts.CreateTestServer(t, owner.ID, "Move Hidden Source")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	role := ts.CreateTestRole(t, serverID, "Organizer", 5, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, serverID, mover.ID, role)
	hidden := ts.CreateVoiceChannel(t, serverID, "mvc-hidden-src")
	to := ts.CreateVoiceChannel(t, serverID, "mvc-hidden-to")
	idle := ts.CreateTestUser(t, "mvc_hidden_idle")
	ts.AddMemberToServer(t, serverID, idle.ID, roleMember)
	ts.CreateChannelOverride(t, to, "role", role, int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice|rbac.PermSpeak), 0)
	ts.CreateChannelOverride(t, hidden, "user", mover.ID, 0, int64(rbac.PermViewVoiceChannels))
	joinVoice(t, ts, hidden, target.ID)

	post := func(targetID string) (int, string) {
		w := ts.DoRequest("POST", voiceEnforcePath(serverID, targetID, pathMove),
			map[string]interface{}{"target_channel_id": to}, testhelpers.AuthHeaders(mover.AccessToken))
		return w.Code, strings.TrimSpace(w.Body.String())
	}
	codeNIV, bodyNIV := post(idle.ID)
	require.Equal(t, http.StatusConflict, codeNIV)
	code, body := post(target.ID)
	assert.Equal(t, http.StatusConflict, code, body)
	assert.Equal(t, bodyNIV, body, "a target in a hidden channel must look like a target not in voice")
}

func TestServerMove_TargetInTwoChannels_UsesVisible(t *testing.T) {
	f := newMoveFixture(t, "mvc_two", true)
	// `from` is the visible one; add a hidden second row.
	hidden := f.ts.CreateVoiceChannel(t, f.serverID, "mvc-two-hidden")
	f.ts.CreateChannelOverride(t, hidden, "user", f.mover.ID, 0, int64(rbac.PermViewVoiceChannels))
	joinVoice(t, f.ts, hidden, f.target.ID)
	conn := connectVoiceWireClient(t, f.ts, f.target)

	code, body := f.move(t, f.mover, f.target.ID, f.to)
	require.Equal(t, http.StatusOK, code, body)
	env := waitForVoiceWireType(t, conn, "voice_move")
	assert.Equal(t, f.from, env.Data["from_channel_id"])
}

func TestServerMove_SelfMove_NonMember403(t *testing.T) {
	f := newMoveFixture(t, "mvc_selfnm", true)
	outsider := f.ts.CreateTestUser(t, "mvc_selfnm_outsider")
	code, body := f.move(t, outsider, outsider.ID, f.to)
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.Contains(t, body, "Not a member of this server")
}

func TestServerMove_SelfMove_NoDestinationAccess403(t *testing.T) {
	f := newMoveFixture(t, "mvc_selfnd", false)
	joinVoice(t, f.ts, f.from, f.mover.ID)
	hideVoiceChannel(t, f.ts, f.serverID, f.to)
	code, body := f.move(t, f.mover, f.mover.ID, f.to)
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.JSONEq(t, insufficientBody, body)
	assert.False(t, moveTempOverrideExists(t, f.ts, f.to, f.mover.ID), "self-move must not grant itself access")
}

func TestServerMove_HierarchyCrossNoGrant_Audited(t *testing.T) {
	f := newMoveFixture(t, "mvc_cross", true)
	senior := f.ts.CreateTestRole(t, f.serverID, "Senior", 10, int64(rbac.PermMoveMembers))
	f.ts.AssignRoleToUser(t, f.serverID, f.target.ID, senior)
	code, body := f.move(t, f.mover, f.target.ID, f.to)
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, 1, auditMoveCount(t, f.ts, f.serverID, f.target.ID))
	assert.Equal(t, "false", moveAuditMeta(t, f.ts, f.serverID, f.target.ID, "temporary_grant"))
	assert.Equal(t, "true", moveAuditMeta(t, f.ts, f.serverID, f.target.ID, "hierarchy_crossed"))
}

func TestServerMove_OwnerWithChannelDeny_Succeeds(t *testing.T) {
	f := newMoveFixture(t, "mvc_ownerdeny", false)
	f.ts.CreateChannelOverride(t, f.to, "user", f.owner.ID, 0,
		int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice|rbac.PermMoveMembers))
	code, body := f.move(t, f.owner, f.target.ID, f.to)
	assert.Equal(t, http.StatusOK, code, body)
}

func TestServerMove_StaleCachePermissive_RefusedInTx(t *testing.T) {
	f := newMoveFixture(t, "mvc_stale", true)
	perms := rbac.BasePermissions | rbac.PermMoveMembers | rbac.PermSpeak
	cache := rbac.NewPermissionCache(f.ts.Redis)
	tags := testhelpers.SeedPermissionGenerations(t, f.ts.Redis, f.serverID, f.mover.ID)
	// Warm permissive entries at server scope and on the destination, then revoke
	// the durable access with direct SQL (no cache invalidation).
	require.NoError(t, cache.Set(context.Background(), f.serverID, f.mover.ID, "", perms, tags))
	require.NoError(t, cache.Set(context.Background(), f.serverID, f.mover.ID, f.to, perms, tags))
	f.ts.CreateChannelOverride(t, f.to, "user", f.mover.ID, 0, int64(rbac.PermViewVoiceChannels))

	code, body := f.move(t, f.mover, f.target.ID, f.to)
	f.requireRefusedNoGrant(t, code, body)
}
