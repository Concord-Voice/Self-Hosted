package channels_test

import (
	"net/http"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pathChannelsSuffix   = "/channels"
	pathChannelGroupsSfx = "/channel-groups"
	notAUUID             = "not-a-uuid"
	zeroUUID             = "00000000-0000-0000-0000-000000000000"
	pathRotateKey        = "/rotate-key"
	pathValidateEpochs   = "/api/v1/e2ee/validate-epochs"
	pathE2EEKeys         = "/api/v1/e2ee/keys/"
	pathE2EEKeysZero     = pathE2EEKeys + zeroUUID
)

// ===========================================================================
// List Channels — edge cases
// ===========================================================================

func TestListChannelsEdgeCases(t *testing.T) {
	ts, user, serverID, _ := setupWithChannel(t)
	outsider := ts.CreateTestUser(t, "lcedgeouter")

	t.Run("InvalidServerID", func(t *testing.T) {
		w := ts.DoRequest("GET", pathServersPrefix+notAUUID+pathChannelsSuffix, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NoAuth", func(t *testing.T) {
		w := ts.DoRequest("GET", pathServersPrefix+serverID+pathChannelsSuffix, nil, nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("NotMemberOtherUser", func(t *testing.T) {
		w := ts.DoRequest("GET", pathServersPrefix+serverID+pathChannelsSuffix, nil, testhelpers.AuthHeaders(outsider.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("IncludesGroupsAndChannels", func(t *testing.T) {
		// Create a channel group
		w := ts.DoRequest("POST", pathServersPrefix+serverID+pathChannelGroupsSfx, map[string]interface{}{
			"name": "LC Text Channels",
		}, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusCreated, w.Code)

		// List channels — should include both channels and channel_groups
		w = ts.DoRequest("GET", pathServersPrefix+serverID+pathChannelsSuffix, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.NotNil(t, body["channels"])
		assert.NotNil(t, body["channel_groups"])
		groups := body["channel_groups"].([]interface{})
		assert.GreaterOrEqual(t, len(groups), 1)
	})
}

// ===========================================================================
// Create Channel — edge cases
// ===========================================================================

func TestCreateChannelEdgeCases(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "ccedge")
	serverID := ts.CreateTestServer(t, user.ID, "CC Edge Server")

	t.Run("BadRequestBody", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannels, map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("InvalidType", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
			keyServerID: serverID,
			"name":      "bad-type-channel",
			"type":      "invalid",
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("EncryptedWithoutKeys", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
			keyServerID:    serverID,
			"name":         "enc-no-keys",
			"type":         "text",
			keyIsEncrypted: true,
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NameTooShort", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
			keyServerID: serverID,
			"name":      "ab", // min=3
			"type":      "text",
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NoAuth", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
			keyServerID: serverID,
			"name":      "noauth-channel",
			"type":      "text",
		}, nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("VoiceCreatesLinkedText", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
			keyServerID: serverID,
			"name":      "voice-room",
			"type":      "voice",
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusCreated, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channel := body[keyChannel].(map[string]interface{})
		assert.Equal(t, "voice", channel["type"])

		assert.NotNil(t, body["linked_text_channel"])
		ltc := body["linked_text_channel"].(map[string]interface{})
		assert.Equal(t, "text", ltc["type"])
		assert.Equal(t, "voice-room", ltc["name"])
	})

	t.Run("WithGroupID", func(t *testing.T) {
		// Create a channel group first
		w := ts.DoRequest("POST", pathServersPrefix+serverID+pathChannelGroupsSfx, map[string]interface{}{
			"name": "CC Group",
		}, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusCreated, w.Code)
		var groupBody map[string]interface{}
		testhelpers.ParseJSON(t, w, &groupBody)
		groupID := groupBody["channel_group"].(map[string]interface{})["id"].(string)

		w = ts.DoRequest("POST", pathChannels, map[string]interface{}{
			keyServerID: serverID,
			"name":      "grouped-channel",
			"type":      "text",
			"group_id":  groupID,
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusCreated, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channel := body[keyChannel].(map[string]interface{})
		assert.Equal(t, groupID, channel["group_id"])
	})

	t.Run("BulletinType", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
			keyServerID: serverID,
			"name":      "announcements",
			"type":      "bulletin",
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusCreated, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channel := body[keyChannel].(map[string]interface{})
		assert.Equal(t, "bulletin", channel["type"])
	})

	t.Run("WithEmoji", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
			keyServerID: serverID,
			"name":      "emoji-channel",
			"type":      "text",
			"emoji":     "rocket",
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusCreated, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channel := body[keyChannel].(map[string]interface{})
		assert.Equal(t, "rocket", channel["emoji"])
	})
}

func TestCreateChannelPermissions(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "ccpermown")
	admin := ts.CreateTestUser(t, "ccpermadm")
	member := ts.CreateTestUser(t, "ccpermmem")
	serverID := ts.CreateTestServer(t, owner.ID, "CC Perm Server")
	ts.AddMemberToServer(t, serverID, admin.ID, "admin")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	t.Run("AdminCanCreate", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
			keyServerID: serverID,
			"name":      "admin-channel",
			"type":      "text",
		}, testhelpers.AuthHeaders(admin.AccessToken))
		assert.Equal(t, http.StatusCreated, w.Code)
	})

	t.Run("MemberCannotCreate", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
			keyServerID: serverID,
			"name":      "blocked-channel",
			"type":      "text",
		}, testhelpers.AuthHeaders(member.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}

// ===========================================================================
// Get Channel — edge cases
// ===========================================================================

func TestGetChannelEdgeCases(t *testing.T) {
	ts, user, _, channelID := setupWithChannel(t)
	outsider := ts.CreateTestUser(t, "gcedgeouter")

	t.Run("InvalidID", func(t *testing.T) {
		w := ts.DoRequest("GET", pathChannelsPrefix+notAUUID, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NotFound", func(t *testing.T) {
		w := ts.DoRequest("GET", pathChannelsPrefix+zeroUUID, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("NotMember", func(t *testing.T) {
		w := ts.DoRequest("GET", pathChannelsPrefix+channelID, nil, testhelpers.AuthHeaders(outsider.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code) // combined "not found or access denied"
	})

	t.Run("NoAuth", func(t *testing.T) {
		w := ts.DoRequest("GET", pathChannelsPrefix+channelID, nil, nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

// ===========================================================================
// Update Channel — edge cases
// ===========================================================================

func TestUpdateChannelEdgeCases(t *testing.T) {
	ts, user, serverID, channelID := setupWithChannel(t)
	member := ts.CreateTestUser(t, "ucedgemem")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	t.Run("InvalidID", func(t *testing.T) {
		w := ts.DoRequest("PATCH", pathChannelsPrefix+notAUUID, map[string]interface{}{
			"name": "updated",
			"type": "text",
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("BadRequestBody", func(t *testing.T) {
		w := ts.DoRequest("PATCH", pathChannelsPrefix+channelID, map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NotFound", func(t *testing.T) {
		w := ts.DoRequest("PATCH", pathChannelsPrefix+zeroUUID, map[string]interface{}{
			"name": "ghost",
			"type": "text",
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("NotAdmin", func(t *testing.T) {
		w := ts.DoRequest("PATCH", pathChannelsPrefix+channelID, map[string]interface{}{
			"name": "hacked",
			"type": "text",
		}, testhelpers.AuthHeaders(member.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("InvalidAudioQualityTier", func(t *testing.T) {
		w := ts.DoRequest("PATCH", pathChannelsPrefix+channelID, map[string]interface{}{
			"name":               "updated",
			"type":               "text",
			"audio_quality_tier": "ultra-mega",
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("ValidAudioQualityTier", func(t *testing.T) {
		// Create voice channel
		w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
			keyServerID: serverID,
			"name":      "voice-aqt",
			"type":      "voice",
		}, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusCreated, w.Code)
		var createBody map[string]interface{}
		testhelpers.ParseJSON(t, w, &createBody)
		voiceChID := createBody[keyChannel].(map[string]interface{})["id"].(string)

		w = ts.DoRequest("PATCH", pathChannelsPrefix+voiceChID, map[string]interface{}{
			"name":               "voice-aqt",
			"type":               "voice",
			"audio_quality_tier": "hifi",
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channel := body[keyChannel].(map[string]interface{})
		assert.Equal(t, "hifi", channel["audio_quality_tier"])
	})

	t.Run("WithGroupID", func(t *testing.T) {
		// Create a group
		w := ts.DoRequest("POST", pathServersPrefix+serverID+pathChannelGroupsSfx, map[string]interface{}{
			"name": "UC Target Group",
		}, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusCreated, w.Code)
		var groupBody map[string]interface{}
		testhelpers.ParseJSON(t, w, &groupBody)
		groupID := groupBody["channel_group"].(map[string]interface{})["id"].(string)

		w = ts.DoRequest("PATCH", pathChannelsPrefix+channelID, map[string]interface{}{
			"name":     "movable-chan",
			"type":     "text",
			"group_id": groupID,
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channel := body[keyChannel].(map[string]interface{})
		assert.Equal(t, groupID, channel["group_id"])
	})

	t.Run("ClearGroupID", func(t *testing.T) {
		w := ts.DoRequest("PATCH", pathChannelsPrefix+channelID, map[string]interface{}{
			"name":     "grouped-chan",
			"type":     "text",
			"group_id": "",
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channel := body[keyChannel].(map[string]interface{})
		assert.Nil(t, channel["group_id"])
	})

	t.Run("WithEmoji", func(t *testing.T) {
		w := ts.DoRequest("PATCH", pathChannelsPrefix+channelID, map[string]interface{}{
			"name":  "emoji-updated",
			"type":  "text",
			"emoji": "fire",
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channel := body[keyChannel].(map[string]interface{})
		assert.Equal(t, "fire", channel["emoji"])
	})

	t.Run("ChangeType", func(t *testing.T) {
		newCh := ts.CreateTestChannel(t, serverID, "type-change-ch", false)
		w := ts.DoRequest("PATCH", pathChannelsPrefix+newCh, map[string]interface{}{
			"name": "now-voice",
			"type": "voice",
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channel := body[keyChannel].(map[string]interface{})
		assert.Equal(t, "voice", channel["type"])
	})
}

func TestUpdateChannelAsAdmin(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "ucadmown")
	admin := ts.CreateTestUser(t, "ucadm")
	serverID := ts.CreateTestServer(t, owner.ID, "Admin Update Server")
	ts.AddMemberToServer(t, serverID, admin.ID, "admin")
	channelID := ts.CreateTestChannel(t, serverID, "admin-update-target", false)

	w := ts.DoRequest("PATCH", pathChannelsPrefix+channelID, map[string]interface{}{
		"name": "admin-updated",
		"type": "text",
	}, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

// ===========================================================================
// Delete Channel — edge cases
// ===========================================================================

func TestDeleteChannelEdgeCases(t *testing.T) {
	ts, user, serverID, channelID := setupWithChannel(t)

	t.Run("InvalidID", func(t *testing.T) {
		w := ts.DoRequest("DELETE", pathChannelsPrefix+notAUUID, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NotFound", func(t *testing.T) {
		w := ts.DoRequest("DELETE", pathChannelsPrefix+zeroUUID, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("NoAuth", func(t *testing.T) {
		w := ts.DoRequest("DELETE", pathChannelsPrefix+channelID, nil, nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("ThenGetReturnsNotFound", func(t *testing.T) {
		ch := ts.CreateTestChannel(t, serverID, "deleteme", false)
		w := ts.DoRequest("DELETE", pathChannelsPrefix+ch, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		w = ts.DoRequest("GET", pathChannelsPrefix+ch, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestDeleteChannelAsAdmin(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "dcadmown")
	admin := ts.CreateTestUser(t, "dcadm")
	serverID := ts.CreateTestServer(t, owner.ID, "Admin Delete Server")
	ts.AddMemberToServer(t, serverID, admin.ID, "admin")
	channelID := ts.CreateTestChannel(t, serverID, "admin-delete-target", false)

	w := ts.DoRequest("DELETE", pathChannelsPrefix+channelID, nil, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

// ===========================================================================
// Mark Channel Read — edge cases
// ===========================================================================

func TestMarkChannelReadEdgeCases(t *testing.T) {
	ts, user, _, channelID := setupWithChannel(t)
	outsider := ts.CreateTestUser(t, "mcredgeouter")

	t.Run("InvalidID", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannelsPrefix+"not-a-uuid/read", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NotMember", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannelsPrefix+channelID+"/read", nil, testhelpers.AuthHeaders(outsider.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("Idempotent", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannelsPrefix+channelID+"/read", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		w = ts.DoRequest("POST", pathChannelsPrefix+channelID+"/read", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)
	})
}

// ===========================================================================
// Mark Server Read — edge cases
// ===========================================================================

func TestMarkServerReadEdgeCases(t *testing.T) {
	ts, _, serverID, _ := setupWithChannel(t)
	outsider := ts.CreateTestUser(t, "msredgeouter")

	t.Run("InvalidID", func(t *testing.T) {
		w := ts.DoRequest("POST", pathServersPrefix+"not-a-uuid/read", nil, testhelpers.AuthHeaders(outsider.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NotMember", func(t *testing.T) {
		w := ts.DoRequest("POST", pathServersPrefix+serverID+"/read", nil, testhelpers.AuthHeaders(outsider.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}

// ===========================================================================
// Get Unread Counts — edge cases
// ===========================================================================

func TestGetUnreadCountsEdgeCases(t *testing.T) {
	ts, user, serverID, channelID := setupWithChannel(t)
	outsider := ts.CreateTestUser(t, "ucedgeouter")

	t.Run("InvalidServerID", func(t *testing.T) {
		w := ts.DoRequest("GET", pathServersPrefix+"not-a-uuid/unread", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NotMember", func(t *testing.T) {
		w := ts.DoRequest("GET", pathServersPrefix+serverID+"/unread", nil, testhelpers.AuthHeaders(outsider.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("ReturnsZeroAfterMarkRead", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannelsPrefix+channelID+"/read", nil, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusOK, w.Code)

		w = ts.DoRequest("GET", pathServersPrefix+serverID+"/unread", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		unreads := body["unreads"].([]interface{})
		for _, u := range unreads {
			entry := u.(map[string]interface{})
			if entry["channel_id"] == channelID {
				assert.Equal(t, float64(0), entry["unread_count"])
			}
		}
	})
}

// ===========================================================================
// Get Server Unread Status — edge cases
// ===========================================================================

func TestGetServerUnreadStatusEdgeCases(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "susedge")

	t.Run("NoAuth", func(t *testing.T) {
		w := ts.DoRequest("GET", "/api/v1/servers/unread-status", nil, nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("EmptyServers", func(t *testing.T) {
		w := ts.DoRequest("GET", "/api/v1/servers/unread-status", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		serverIDs := body["server_ids"].([]interface{})
		assert.Empty(t, serverIDs)
	})
}

// ===========================================================================
// Distribute Channel Keys — edge cases
// ===========================================================================

func TestDistributeChannelKeysEdgeCases(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)

	t.Run("InvalidChannelID", func(t *testing.T) {
		w := ts.DoRequest("POST", "/api/v1/channels/not-a-uuid/keys", map[string]interface{}{
			keyWrappedKeys: map[string]string{
				owner.ID: testhelpers.ValidCiphertext(),
			},
		}, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("BadBody", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{}, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NotEncrypted", func(t *testing.T) {
		plainCh := ts.CreateTestChannel(t, serverID, "plain-for-dist", false)
		w := ts.DoRequest("POST", pathChannelsPrefix+plainCh+pathKeys, map[string]interface{}{
			keyWrappedKeys: map[string]string{
				owner.ID: testhelpers.ValidCiphertext(),
			},
		}, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NoOwnKey", func(t *testing.T) {
		// New member has no key yet — cannot distribute
		newMember := ts.CreateTestUser(t, "dknoown")
		ts.AddMemberToServer(t, serverID, newMember.ID, roleMember)

		w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
			keyWrappedKeys: map[string]string{
				owner.ID: testhelpers.ValidCiphertext(),
			},
		}, testhelpers.AuthHeaders(newMember.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("WithExplicitVersion", func(t *testing.T) {
		newMember := ts.CreateTestUser(t, "dkexpver")
		ts.AddMemberToServer(t, serverID, newMember.ID, roleMember)

		w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
			keyWrappedKeys: map[string]string{
				newMember.ID: testhelpers.ValidCiphertext(),
			},
			"key_version": 1,
		}, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, float64(1), body["distributed"])
	})
}

// ===========================================================================
// Get Channel Keys — edge cases
// ===========================================================================

func TestGetChannelKeysEdgeCases(t *testing.T) {
	ts, user, _, channelID := setupEncryptedChannel(t)

	t.Run("NoAuth", func(t *testing.T) {
		w := ts.DoRequest("GET", pathChannelsPrefix+channelID+pathKeys, nil, nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("WithVersionParam", func(t *testing.T) {
		w := ts.DoRequest("GET", pathChannelsPrefix+channelID+"/keys?version=1", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		key := body["key"].(map[string]interface{})
		assert.Equal(t, float64(1), key["key_version"])
	})

	t.Run("InvalidVersionParam", func(t *testing.T) {
		w := ts.DoRequest("GET", pathChannelsPrefix+channelID+"/keys?version=abc", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NonExistentVersion", func(t *testing.T) {
		w := ts.DoRequest("GET", pathChannelsPrefix+channelID+"/keys?version=999", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

// ===========================================================================
// Rotate Key — all paths
// ===========================================================================

func TestRotateKeyAllPaths(t *testing.T) {
	ts, user, serverID, channelID := setupEncryptedChannel(t)
	member := ts.CreateTestUser(t, "rkmember")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	t.Run("Success", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathRotateKey, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Contains(t, body, "new_key_version")
		assert.Equal(t, float64(2), body["new_key_version"])
	})

	t.Run("InvalidChannelID", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannelsPrefix+notAUUID+pathRotateKey, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NotFound", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannelsPrefix+zeroUUID+pathRotateKey, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("NotEncrypted", func(t *testing.T) {
		plainCh := ts.CreateTestChannel(t, serverID, "plain-for-rotate", false)
		w := ts.DoRequest("POST", pathChannelsPrefix+plainCh+pathRotateKey, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NotAdmin", func(t *testing.T) {
		w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathRotateKey, nil, testhelpers.AuthHeaders(member.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}

// ===========================================================================
// Validate Epochs — all paths
// ===========================================================================

func TestValidateEpochsAllPaths(t *testing.T) {
	ts, user, _, channelID := setupEncryptedChannel(t)
	outsider := ts.CreateTestUser(t, "epochsouter")

	t.Run("Success", func(t *testing.T) {
		w := ts.DoRequest("POST", pathValidateEpochs, map[string]interface{}{
			"epochs": map[string]int{
				channelID: 1,
			},
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		revocations := body["revocations"].([]interface{})
		assert.Empty(t, revocations, "no revocations should exist for a fresh channel")
	})

	t.Run("BadBody", func(t *testing.T) {
		w := ts.DoRequest("POST", pathValidateEpochs, map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NoAuth", func(t *testing.T) {
		w := ts.DoRequest("POST", pathValidateEpochs, map[string]interface{}{
			"epochs": map[string]int{},
		}, nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("InvalidChannelIDSkipped", func(t *testing.T) {
		w := ts.DoRequest("POST", pathValidateEpochs, map[string]interface{}{
			"epochs": map[string]int{
				notAUUID: 1,
			},
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		revocations := body["revocations"].([]interface{})
		assert.Empty(t, revocations)
	})

	t.Run("NoAccessChannelSkipped", func(t *testing.T) {
		w := ts.DoRequest("POST", pathValidateEpochs, map[string]interface{}{
			"epochs": map[string]int{
				channelID: 1,
			},
		}, testhelpers.AuthHeaders(outsider.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		revocations := body["revocations"].([]interface{})
		assert.Empty(t, revocations)
	})
}

// ===========================================================================
// Get Unified Keys — all paths
// ===========================================================================

func TestGetUnifiedKeysAllPaths(t *testing.T) {
	ts, user, _, channelID := setupEncryptedChannel(t)

	t.Run("ChannelSuccess", func(t *testing.T) {
		w := ts.DoRequest("GET", pathE2EEKeys+channelID, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, "channel", body["context_type"])
		assert.NotNil(t, body["key"])
	})

	t.Run("InvalidContextID", func(t *testing.T) {
		w := ts.DoRequest("GET", pathE2EEKeys+notAUUID, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NotFound", func(t *testing.T) {
		w := ts.DoRequest("GET", pathE2EEKeysZero, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("ChannelNotEncrypted", func(t *testing.T) {
		// Get the server ID from the encrypted channel so we create the plain channel in the same server
		var srvID string
		err := ts.DB.QueryRow(`SELECT server_id FROM channels WHERE id = $1`, channelID).Scan(&srvID)
		require.NoError(t, err)
		plainCh := ts.CreateTestChannel(t, srvID, "plain-unified", false)

		w := ts.DoRequest("GET", pathE2EEKeys+plainCh, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("ChannelWithVersion", func(t *testing.T) {
		w := ts.DoRequest("GET", pathE2EEKeys+channelID+"?version=1", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, "channel", body["context_type"])
		key := body["key"].(map[string]interface{})
		assert.Equal(t, float64(1), key["key_version"])
	})

	t.Run("ChannelInvalidVersion", func(t *testing.T) {
		w := ts.DoRequest("GET", pathE2EEKeys+channelID+"?version=abc", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("ChannelNonExistentVersion", func(t *testing.T) {
		w := ts.DoRequest("GET", pathE2EEKeys+channelID+"?version=999", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("NoAuth", func(t *testing.T) {
		w := ts.DoRequest("GET", pathE2EEKeysZero, nil, nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

func TestGetUnifiedKeysDM(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "ukdm1")
	user2 := ts.CreateTestUser(t, "ukdm2")
	ts.CreateFriendship(t, user1.ID, user2.ID, "accepted")

	t.Run("DMSuccess", func(t *testing.T) {
		convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)
		ts.SeedDMKey(t, convID, user1.ID, 1)

		w := ts.DoRequest("GET", pathE2EEKeys+convID, nil, testhelpers.AuthHeaders(user1.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, "dm", body["context_type"])
		assert.NotNil(t, body["key"])
	})

	t.Run("DMNotEncrypted", func(t *testing.T) {
		convID := ts.CreateDMConversation(t, user1.ID, user2.ID, false)

		w := ts.DoRequest("GET", pathE2EEKeys+convID, nil, testhelpers.AuthHeaders(user1.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("DMNoKey", func(t *testing.T) {
		convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)

		w := ts.DoRequest("GET", pathE2EEKeys+convID, nil, testhelpers.AuthHeaders(user1.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, true, body["pending"])
	})
}

// ===========================================================================
// Distribute Unified Keys — all paths
// ===========================================================================

func TestDistributeUnifiedKeysAllPaths(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	newMember := ts.CreateTestUser(t, "dukch")
	ts.AddMemberToServer(t, serverID, newMember.ID, roleMember)

	t.Run("ChannelSuccess", func(t *testing.T) {
		w := ts.DoRequest("POST", pathE2EEKeys+channelID, map[string]interface{}{
			keyWrappedKeys: map[string]string{
				newMember.ID: testhelpers.ValidCiphertext(),
			},
		}, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, float64(1), body["distributed"])
	})

	t.Run("InvalidContextID", func(t *testing.T) {
		w := ts.DoRequest("POST", pathE2EEKeys+notAUUID, map[string]interface{}{
			keyWrappedKeys: map[string]string{
				owner.ID: testhelpers.ValidCiphertext(),
			},
		}, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("NotFound", func(t *testing.T) {
		w := ts.DoRequest("POST", pathE2EEKeysZero, map[string]interface{}{
			keyWrappedKeys: map[string]string{
				owner.ID: testhelpers.ValidCiphertext(),
			},
		}, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestDistributeUnifiedKeysDM(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dukdm1")
	user2 := ts.CreateTestUser(t, "dukdm2")
	ts.CreateFriendship(t, user1.ID, user2.ID, "accepted")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID, true)

	w := ts.DoRequest("POST", pathE2EEKeys+convID, map[string]interface{}{
		keyWrappedKeys: map[string]string{
			user2.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "dm", body["context_type"])
	assert.Equal(t, float64(1), body["distributed"])
}

// ===========================================================================
// Get Pending Key Requests — edge cases
// ===========================================================================

func TestGetPendingKeyRequestsEdgeCases(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "pkredge")

	t.Run("NoAuth", func(t *testing.T) {
		w := ts.DoRequest("GET", "/api/v1/e2ee/pending-keys", nil, nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("NoChannels", func(t *testing.T) {
		w := ts.DoRequest("GET", "/api/v1/e2ee/pending-keys", nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		requests := body["pending_requests"].([]interface{})
		assert.Empty(t, requests)
	})
}

// ===========================================================================
// Channel Encryption Flow — end-to-end
// ===========================================================================

func TestEncryptedChannelFullFlow(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "e2eeflowown")
	member := ts.CreateTestUser(t, "e2eeflowmem")
	serverID := ts.CreateTestServer(t, owner.ID, "E2EE Flow Server")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	// 1. Owner creates encrypted channel with their own key
	w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
		keyServerID:    serverID,
		"name":         "encrypted-flow",
		"type":         "text",
		keyIsEncrypted: true,
		keyWrappedKeys: map[string]string{
			owner.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	channelID := createBody[keyChannel].(map[string]interface{})["id"].(string)

	// 2. Owner fetches their key
	w = ts.DoRequest("GET", pathChannelsPrefix+channelID+pathKeys, nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// 3. Member tries to fetch key — should get not found (pending)
	w = ts.DoRequest("GET", pathChannelsPrefix+channelID+pathKeys, nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
	var pendingBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &pendingBody)
	assert.Equal(t, true, pendingBody["pending"])

	// 4. Owner distributes key to member
	w = ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{
			member.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var distBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &distBody)
	assert.Equal(t, float64(1), distBody["distributed"])

	// 5. Member can now fetch key
	w = ts.DoRequest("GET", pathChannelsPrefix+channelID+pathKeys, nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// 6. Owner rotates key
	w = ts.DoRequest("POST", pathChannelsPrefix+channelID+pathRotateKey, nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	var rotateBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &rotateBody)
	assert.Equal(t, float64(2), rotateBody["new_key_version"])

	// 7. Validate epochs — no revocations (rotation is initiated but not sealed)
	w = ts.DoRequest("POST", pathValidateEpochs, map[string]interface{}{
		"epochs": map[string]int{
			channelID: 1,
		},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestCreateEncryptedVoiceChannelWithLinkedText(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "encvoice")
	serverID := ts.CreateTestServer(t, user.ID, "Enc Voice Server")

	w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
		keyServerID:    serverID,
		"name":         "encrypted-voice",
		"type":         "voice",
		keyIsEncrypted: true,
		keyWrappedKeys: map[string]string{
			user.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)

	channel := body[keyChannel].(map[string]interface{})
	assert.Equal(t, "voice", channel["type"])
	assert.Equal(t, true, channel["is_encrypted"])

	// Linked text channel should also be encrypted
	assert.NotNil(t, body["linked_text_channel"])
	ltc := body["linked_text_channel"].(map[string]interface{})
	assert.Equal(t, true, ltc["is_encrypted"])
}

// ===========================================================================
// Reorder Channels — additional edge cases (supplements groups_test.go)
// ===========================================================================

func TestReorderChannelsAdditional(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	ch1 := ts.CreateTestChannel(t, serverID, "reorder-a", false)
	ch2 := ts.CreateTestChannel(t, serverID, "reorder-b", false)
	ch3 := ts.CreateTestChannel(t, serverID, "reorder-c", false)
	groupID := createGroup(t, ts, serverID, "Reorder Target", user.AccessToken)

	t.Run("MultipleChannels", func(t *testing.T) {
		w := ts.DoRequest("PUT", reorderPath(serverID), map[string]interface{}{
			"channels": []map[string]interface{}{
				{"channel_id": ch3, "position": 0},
				{"channel_id": ch1, "position": 1},
				{"channel_id": ch2, "position": 2},
			},
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		// Verify order
		w = ts.DoRequest("GET", pathServersPrefix+serverID+pathChannelsSuffix, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)
		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channels := body["channels"].([]interface{})
		assert.GreaterOrEqual(t, len(channels), 3)

		first := channels[0].(map[string]interface{})
		assert.Equal(t, ch3, first["id"])
	})

	t.Run("MoveToGroup", func(t *testing.T) {
		w := ts.DoRequest("PUT", reorderPath(serverID), map[string]interface{}{
			"channels": []map[string]interface{}{
				{"channel_id": ch1, "group_id": groupID, "position": 0},
			},
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		// Verify channel is now in the group
		w = ts.DoRequest("GET", pathChannelsPrefix+ch1, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)
		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channel := body[keyChannel].(map[string]interface{})
		assert.Equal(t, groupID, channel["group_id"])
	})

	t.Run("NoAuth", func(t *testing.T) {
		w := ts.DoRequest("PUT", reorderPath(serverID), map[string]interface{}{
			"channels": []map[string]interface{}{},
		}, nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

// ===========================================================================
// Channel Groups — additional edge cases (supplements groups_test.go)
// ===========================================================================

func TestChannelGroupsAdditional(t *testing.T) {
	ts, owner, serverID := setupWithServer(t)
	admin := ts.CreateTestUser(t, "grpadm")
	member := ts.CreateTestUser(t, "grpmem")
	ts.AddMemberToServer(t, serverID, admin.ID, "admin")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	t.Run("NameTooLong", func(t *testing.T) {
		longName := ""
		for i := 0; i < 101; i++ {
			longName += "x"
		}
		w := ts.DoRequest("POST", groupsPath(serverID), map[string]interface{}{
			"name": longName,
		}, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("AdminCanCreate", func(t *testing.T) {
		w := ts.DoRequest("POST", groupsPath(serverID), map[string]interface{}{
			"name": "Admin Category",
		}, testhelpers.AuthHeaders(admin.AccessToken))
		assert.Equal(t, http.StatusCreated, w.Code)
	})

	t.Run("MemberCanList", func(t *testing.T) {
		w := ts.DoRequest("GET", groupsPath(serverID), nil, testhelpers.AuthHeaders(member.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		groups := body["channel_groups"].([]interface{})
		assert.GreaterOrEqual(t, len(groups), 1)
	})

	t.Run("DeleteGroupChannelsSurvive", func(t *testing.T) {
		groupID := createGroup(t, ts, serverID, "Doomed Group", owner.AccessToken)

		// Create a channel in this group
		w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
			keyServerID: serverID,
			"name":      "survivor-channel",
			"type":      "text",
			"group_id":  groupID,
		}, testhelpers.AuthHeaders(owner.AccessToken))
		require.Equal(t, http.StatusCreated, w.Code)
		var createBody map[string]interface{}
		testhelpers.ParseJSON(t, w, &createBody)
		channelID := createBody[keyChannel].(map[string]interface{})["id"].(string)

		// Delete the group
		w = ts.DoRequest("DELETE", groupPath(serverID, groupID), nil, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		// Channel should still exist with group_id = NULL (ON DELETE SET NULL)
		w = ts.DoRequest("GET", pathChannelsPrefix+channelID, nil, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channel := body[keyChannel].(map[string]interface{})
		assert.Nil(t, channel["group_id"], "channel's group_id should be null after group deletion")
	})
}

// ===========================================================================
// Multiple operations — scenario tests
// ===========================================================================

func TestMultipleChannelOperations(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "multiop")
	serverID := ts.CreateTestServer(t, user.ID, "Multi Op Server")

	t.Run("CreateMultiple", func(t *testing.T) {
		channelNames := []string{"general", "random", "announcements", "dev-talk", "off-topic"}
		for _, name := range channelNames {
			w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
				keyServerID: serverID,
				"name":      name,
				"type":      "text",
			}, testhelpers.AuthHeaders(user.AccessToken))
			assert.Equal(t, http.StatusCreated, w.Code)
		}

		// List all channels
		w := ts.DoRequest("GET", pathServersPrefix+serverID+pathChannelsSuffix, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channels := body["channels"].([]interface{})
		assert.Equal(t, len(channelNames), len(channels))
	})

	t.Run("DeleteAllThenListEmpty", func(t *testing.T) {
		// Create a new server to avoid conflicts with subtests above
		srvID := ts.CreateTestServer(t, user.ID, "Del All Server")
		var ids []string
		for _, name := range []string{"chan-a", "chan-b"} {
			id := ts.CreateTestChannel(t, srvID, name, false)
			ids = append(ids, id)
		}

		for _, id := range ids {
			w := ts.DoRequest("DELETE", pathChannelsPrefix+id, nil, testhelpers.AuthHeaders(user.AccessToken))
			assert.Equal(t, http.StatusOK, w.Code)
		}

		w := ts.DoRequest("GET", pathServersPrefix+srvID+pathChannelsSuffix, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		channels := body["channels"].([]interface{})
		assert.Empty(t, channels)
	})
}
