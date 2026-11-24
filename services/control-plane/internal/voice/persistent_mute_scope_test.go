package voice_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sessionScopeFixture: an owner, an actor holding the given server-scope bits, and
// a base-member target in a visible voice channel with one exact registered
// session. Channel overrides are added per test.
type sessionScopeFixture struct {
	ts        *testhelpers.TestServer
	owner     testhelpers.TestUser
	actor     testhelpers.TestUser
	target    testhelpers.TestUser
	serverID  string
	actorRole string
	channelID string
}

func newSessionScopeFixture(t *testing.T, tag string, perms rbac.Permission) sessionScopeFixture {
	t.Helper()
	ts := setupTS(t)
	f := sessionScopeFixture{ts: ts}
	f.owner = ts.CreateTestUser(t, tag+"_owner")
	f.actor = ts.CreateTestUser(t, tag+"_actor")
	f.target = ts.CreateTestUser(t, tag+"_target")
	f.serverID = ts.CreateTestServer(t, f.owner.ID, "Scope "+tag)
	ts.AddMemberToServer(t, f.serverID, f.actor.ID, roleMember)
	ts.AddMemberToServer(t, f.serverID, f.target.ID, roleMember)
	f.actorRole = ts.CreateTestRole(t, f.serverID, "Moderator", 5, int64(perms))
	ts.AssignRoleToUser(t, f.serverID, f.actor.ID, f.actorRole)
	f.channelID = ts.CreateVoiceChannel(t, f.serverID, tag+"-visible")
	joinVoice(t, ts, f.channelID, f.target.ID)
	seedOutboxVoiceSession(t, ts, f.channelID, f.target.ID)
	return f
}

func (f sessionScopeFixture) post(t *testing.T, targetID, suffix string) (int, string) {
	t.Helper()
	w := f.ts.DoRequest("POST", voiceEnforcePath(f.serverID, targetID, suffix), nil,
		testhelpers.AuthHeaders(f.actor.AccessToken))
	return w.Code, strings.TrimSpace(w.Body.String())
}

// addHiddenChannelRow puts the target in a second voice channel that the actor
// cannot see, with its own exact session, so the target is "mid-move".
func (f sessionScopeFixture) addHiddenChannelRow(t *testing.T, tag string, denyBit rbac.Permission) string {
	t.Helper()
	other := f.ts.CreateVoiceChannel(t, f.serverID, tag)
	f.ts.CreateChannelOverride(t, other, "user", f.actor.ID, 0, int64(denyBit))
	joinVoice(t, f.ts, other, f.target.ID)
	gen := uuid.NewString()
	_, err := f.ts.DB.Exec(`
		INSERT INTO voice_enforcement_sessions
			(session_generation, node_boot_id, room_id, room_kind, user_id, credential_epoch, socket_id)
		VALUES ($1, $2, $3, 'channel', $4, '', $5)
	`, gen, outboxNodeBootID, other, f.target.ID, "second-"+gen)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := f.ts.DB.Exec(`DELETE FROM voice_enforcement_sessions WHERE session_generation = $1`, gen)
		assert.NoError(t, cleanupErr)
	})
	return other
}

func (f sessionScopeFixture) serverMuted(t *testing.T) bool {
	t.Helper()
	var muted bool
	require.NoError(t, f.ts.DB.QueryRow(
		`SELECT server_muted FROM server_members WHERE server_id = $1 AND user_id = $2`,
		f.serverID, f.target.ID).Scan(&muted))
	return muted
}

// TestServerMute_ChannelDenyMute_StillApplies pins D-168c: persistent server mute is
// a server-scope action; a channel DENY of MUTE_MEMBERS does not narrow it.
func TestServerMute_ChannelDenyMute_StillApplies(t *testing.T) {
	f := newSessionScopeFixture(t, "psm_deny", rbac.PermMuteMembers)
	f.ts.CreateChannelOverride(t, f.channelID, "user", f.actor.ID, 0, int64(rbac.PermMuteMembers))

	code, body := f.post(t, f.target.ID, pathMute)
	assert.Equal(t, http.StatusOK, code, body)
	assert.True(t, f.serverMuted(t), "persistent mute is written regardless of channel overrides")
}

func TestServerMute_NoChannelDeny_Applies(t *testing.T) {
	f := newSessionScopeFixture(t, "psm_ctl", rbac.PermMuteMembers)
	code, body := f.post(t, f.target.ID, pathMute)
	assert.Equal(t, http.StatusOK, code, body)
	assert.True(t, f.serverMuted(t))
}

func TestUserMute_ChannelDenyMute_403NoOutbox(t *testing.T) {
	f := newSessionScopeFixture(t, "um_deny", rbac.PermMuteMembers)
	f.ts.CreateChannelOverride(t, f.channelID, "user", f.actor.ID, 0, int64(rbac.PermMuteMembers))
	code, body := f.post(t, f.target.ID, pathUserMute)
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.JSONEq(t, insufficientBody, body)
	requireNoServerOutboxCommands(t, f.ts.DB)
}

func TestUserMute_NoChannelDeny_Queued(t *testing.T) {
	f := newSessionScopeFixture(t, "um_ctl", rbac.PermMuteMembers)
	code, body := f.post(t, f.target.ID, pathUserMute)
	assert.Equal(t, http.StatusAccepted, code, body)
	command := readServerOutboxCommand(t, f.ts.DB, f.target.ID)
	assert.Equal(t, f.channelID, command.ChannelID.String)
}

func TestUserDeafen_ChannelDenyDeafen_403NoOutbox(t *testing.T) {
	f := newSessionScopeFixture(t, "ud_deny", rbac.PermDeafenMembers)
	f.ts.CreateChannelOverride(t, f.channelID, "user", f.actor.ID, 0, int64(rbac.PermDeafenMembers))
	code, body := f.post(t, f.target.ID, pathUserDeafen)
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.JSONEq(t, insufficientBody, body)
	requireNoServerOutboxCommands(t, f.ts.DB)
}

func TestUserMute_HiddenChannel_NotInVoiceBody(t *testing.T) {
	f := newSessionScopeFixture(t, "um_hidden", rbac.PermMuteMembers)
	f.ts.CreateChannelOverride(t, f.channelID, "user", f.actor.ID, 0, int64(rbac.PermViewVoiceChannels))
	idle := f.ts.CreateTestUser(t, "um_hidden_idle")
	f.ts.AddMemberToServer(t, f.serverID, idle.ID, roleMember)

	codeNIV, bodyNIV := f.post(t, idle.ID, pathUserMute)
	require.Equal(t, http.StatusBadRequest, codeNIV)
	code, body := f.post(t, f.target.ID, pathUserMute)
	assert.Equal(t, http.StatusBadRequest, code, body)
	assert.Equal(t, bodyNIV, body, "a target in a hidden channel must look like a target not in voice")
	requireNoServerOutboxCommands(t, f.ts.DB)
}

func TestUserMute_StaleCachePermissive_RefusedInTx(t *testing.T) {
	f := newSessionScopeFixture(t, "um_stale", rbac.PermMuteMembers)
	perms := rbac.BasePermissions | rbac.PermMuteMembers
	cache := rbac.NewPermissionCache(f.ts.Redis)
	tags := testhelpers.SeedPermissionGenerations(t, f.ts.Redis, f.serverID, f.actor.ID)
	require.NoError(t, cache.Set(context.Background(), f.serverID, f.actor.ID, "", perms, tags))
	require.NoError(t, cache.Set(context.Background(), f.serverID, f.actor.ID, f.channelID, perms, tags))
	// Durable deny written with direct SQL, no cache invalidation.
	f.ts.CreateChannelOverride(t, f.channelID, "user", f.actor.ID, 0, int64(rbac.PermMuteMembers))

	code, body := f.post(t, f.target.ID, pathUserMute)
	assert.Equal(t, http.StatusForbidden, code, body)
	requireNoServerOutboxCommands(t, f.ts.DB)
}

func TestServerDisconnect_ChannelDenyMove_403NoOutbox(t *testing.T) {
	f := newSessionScopeFixture(t, "dc_deny", rbac.PermMoveMembers)
	f.ts.CreateChannelOverride(t, f.channelID, "user", f.actor.ID, 0, int64(rbac.PermMoveMembers))
	code, body := f.post(t, f.target.ID, pathDisconnect)
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.JSONEq(t, insufficientBody, body)
	requireNoServerOutboxCommands(t, f.ts.DB)
}

func TestServerDisconnect_HiddenChannel_NotInVoiceBody(t *testing.T) {
	f := newSessionScopeFixture(t, "dc_hidden", rbac.PermMoveMembers)
	f.ts.CreateChannelOverride(t, f.channelID, "user", f.actor.ID, 0, int64(rbac.PermViewVoiceChannels))
	idle := f.ts.CreateTestUser(t, "dc_hidden_idle")
	f.ts.AddMemberToServer(t, f.serverID, idle.ID, roleMember)

	codeNIV, bodyNIV := f.post(t, idle.ID, pathDisconnect)
	require.Equal(t, http.StatusConflict, codeNIV)
	code, body := f.post(t, f.target.ID, pathDisconnect)
	assert.Equal(t, http.StatusConflict, code, body)
	assert.Equal(t, bodyNIV, body, "a target in a hidden channel must look like a target not in voice")
	requireNoServerOutboxCommands(t, f.ts.DB)
}

// Review Focus 2: target has rows in a hidden and a visible channel.
func TestServerDisconnect_HiddenPlusAllowed_QueuesVisibleOnly(t *testing.T) {
	f := newSessionScopeFixture(t, "dc_two", rbac.PermMoveMembers)
	f.addHiddenChannelRow(t, "dc-two-hidden", rbac.PermViewVoiceChannels)

	code, body := f.post(t, f.target.ID, pathDisconnect)
	require.Equal(t, http.StatusAccepted, code, body)
	command := readServerOutboxCommand(t, f.ts.DB, f.target.ID)
	assert.Equal(t, f.channelID, command.ChannelID.String, "only the visible channel's session is acted on")
}

func TestServerDisconnect_DeniedPlusAllowed_403NoOutbox(t *testing.T) {
	f := newSessionScopeFixture(t, "dc_mixed", rbac.PermMoveMembers)
	// Visible but MOVE denied on the second channel.
	f.addHiddenChannelRow(t, "dc-mixed-denied", rbac.PermMoveMembers)

	code, body := f.post(t, f.target.ID, pathDisconnect)
	assert.Equal(t, http.StatusForbidden, code, body)
	requireNoServerOutboxCommands(t, f.ts.DB)
}

// Review Focus 4: a server Administrator is not narrowed by a channel deny.
func TestServerDisconnect_AdminWithChannelDeny_Queued(t *testing.T) {
	f := newSessionScopeFixture(t, "dc_admin", rbac.PermMoveMembers|rbac.PermAdministrator)
	f.ts.CreateChannelOverride(t, f.channelID, "user", f.actor.ID, 0, int64(rbac.PermMoveMembers))
	code, body := f.post(t, f.target.ID, pathDisconnect)
	assert.Equal(t, http.StatusAccepted, code, body)
	command := readServerOutboxCommand(t, f.ts.DB, f.target.ID)
	assert.Equal(t, f.channelID, command.ChannelID.String)
}

func TestServerDisconnect_NoChannelDeny_Queued(t *testing.T) {
	f := newSessionScopeFixture(t, "dc_ctl", rbac.PermMoveMembers)
	code, body := f.post(t, f.target.ID, pathDisconnect)
	assert.Equal(t, http.StatusAccepted, code, body)
	command := readServerOutboxCommand(t, f.ts.DB, f.target.ID)
	assert.Equal(t, f.channelID, command.ChannelID.String)
}
