package voice_test

import (
	"net/http"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTS(t *testing.T) *testhelpers.TestServer {
	t.Helper()
	return testhelpers.SetupTestServer(t)
}

// --- GetParticipants Tests ---

func TestGetParticipants_Success(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vpowner")
	serverID := ts.CreateTestServer(t, owner.ID, "Voice Test Server")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-general")

	w := ts.DoRequest("GET", "/api/v1/channels/"+channelID+"/voice/participants", nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	participants := body["participants"].([]interface{})
	assert.Len(t, participants, 0, "no participants initially")
}

func TestGetParticipants_NotMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vpowner2")
	outsider := ts.CreateTestUser(t, "vpoutsider")
	serverID := ts.CreateTestServer(t, owner.ID, "Voice NotMember Server")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-private")

	w := ts.DoRequest("GET", "/api/v1/channels/"+channelID+"/voice/participants", nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestGetParticipants_InvalidChannel(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "vpinvalid")

	w := ts.DoRequest("GET", "/api/v1/channels/not-a-uuid/voice/participants", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- AuthorizeJoin Tests ---

func TestAuthorizeJoin_Success(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vjowner")
	member := ts.CreateTestUser(t, "vjmember")
	serverID := ts.CreateTestServer(t, owner.ID, "VoiceJoin Test Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-join")

	w := ts.DoRequest("POST", "/api/v1/channels/"+channelID+"/voice/join", nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, true, body["allowed"])
}

func TestAuthorizeJoin_NotMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vjowner2")
	outsider := ts.CreateTestUser(t, "vjoutsider")
	serverID := ts.CreateTestServer(t, owner.ID, "VoiceJoin NonMember")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-restricted")

	w := ts.DoRequest("POST", "/api/v1/channels/"+channelID+"/voice/join", nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestAuthorizeJoin_NoJoinVoicePermission(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vjowner3")
	member := ts.CreateTestUser(t, "vjnoperm")
	serverID := ts.CreateTestServer(t, owner.ID, "VoiceJoin NoPerm")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-noperm")

	// Get @all role and create channel deny override for JoinVoice
	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)
	ts.CreateChannelOverride(t, channelID, "role", allRoleID, 0, int64(rbac.PermJoinVoice))

	w := ts.DoRequest("POST", "/api/v1/channels/"+channelID+"/voice/join", nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// --- AuthorizeVoiceAction Tests (table-driven to reduce duplication) ---

func TestAuthorizeVoiceAction_ValidActions(t *testing.T) {
	actions := []struct {
		action     string
		expectCode int
	}{
		{"mute", http.StatusOK},
		{"deafen", http.StatusOK},
		{"move", http.StatusOK},
		{"explode", http.StatusBadRequest},
	}

	for _, tc := range actions {
		t.Run(tc.action, func(t *testing.T) {
			ts := setupTS(t)
			owner := ts.CreateTestUser(t, "va_"+tc.action+"_owner")
			target := ts.CreateTestUser(t, "va_"+tc.action+"_target")
			serverID := ts.CreateTestServer(t, owner.ID, "VoiceAction "+tc.action)
			ts.AddMemberToServer(t, serverID, target.ID, "member")
			channelID := ts.CreateVoiceChannel(t, serverID, "voice-"+tc.action)

			w := ts.DoRequest("POST", "/api/v1/channels/"+channelID+"/voice/authorize-action", map[string]interface{}{
				"action":         tc.action,
				"target_user_id": target.ID,
			}, testhelpers.AuthHeaders(owner.AccessToken))
			assert.Equal(t, tc.expectCode, w.Code)
		})
	}
}

func TestAuthorizeVoiceAction_NoPermission(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vaowner5")
	actor := ts.CreateTestUser(t, "vaactor5")
	target := ts.CreateTestUser(t, "vatarget5")
	serverID := ts.CreateTestServer(t, owner.ID, "VoiceAction NoPerm")
	ts.AddMemberToServer(t, serverID, actor.ID, "member")
	ts.AddMemberToServer(t, serverID, target.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-noperm")

	// Base member doesn't have MuteMembers
	w := ts.DoRequest("POST", "/api/v1/channels/"+channelID+"/voice/authorize-action", map[string]interface{}{
		"action":         "mute",
		"target_user_id": target.ID,
	}, testhelpers.AuthHeaders(actor.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestAuthorizeVoiceAction_HierarchyViolation(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vaowner6")
	moderator := ts.CreateTestUser(t, "vamod6")
	admin := ts.CreateTestUser(t, "vaadmin6")
	serverID := ts.CreateTestServer(t, owner.ID, "VoiceAction Hierarchy")
	ts.AddMemberToServer(t, serverID, moderator.ID, "member")
	ts.AddMemberToServer(t, serverID, admin.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-hierarchy")

	// Mod (pos 5) has MuteMembers
	modRoleID := ts.CreateTestRole(t, serverID, "Mod", 5, int64(rbac.PermMuteMembers))
	ts.AssignRoleToUser(t, serverID, moderator.ID, modRoleID)

	// Admin (pos 10) outranks mod
	adminRoleID := ts.CreateTestRole(t, serverID, "Admin", 10, int64(rbac.AdminPermissions))
	ts.AssignRoleToUser(t, serverID, admin.ID, adminRoleID)

	// Mod tries to mute admin → hierarchy violation
	w := ts.DoRequest("POST", "/api/v1/channels/"+channelID+"/voice/authorize-action", map[string]interface{}{
		"action":         "mute",
		"target_user_id": admin.ID,
	}, testhelpers.AuthHeaders(moderator.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestAuthorizeVoiceAction_WithCustomPermission(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vaowner7")
	actor := ts.CreateTestUser(t, "vaactor7")
	target := ts.CreateTestUser(t, "vatarget7")
	serverID := ts.CreateTestServer(t, owner.ID, "VoiceAction CustomPerm")
	ts.AddMemberToServer(t, serverID, actor.ID, "member")
	ts.AddMemberToServer(t, serverID, target.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-customperm")

	// Give actor exactly MuteMembers at a higher position than target
	muterRoleID := ts.CreateTestRole(t, serverID, "Muter", 5, int64(rbac.PermMuteMembers))
	ts.AssignRoleToUser(t, serverID, actor.ID, muterRoleID)

	w := ts.DoRequest("POST", "/api/v1/channels/"+channelID+"/voice/authorize-action", map[string]interface{}{
		"action":         "mute",
		"target_user_id": target.ID,
	}, testhelpers.AuthHeaders(actor.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, true, body["allowed"])
}
