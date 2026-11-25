package dm_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
	natsclient "github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/nats"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Outbound identifiers (Redis keys, WS payloads, NATS enforcement) must carry
// the canonical lower-case spelling even when the path used an alternate one;
// the media plane drops non-canonical enforcement channel IDs.

func upperSpelling(t *testing.T, id string) string {
	t.Helper()
	upper := strings.ToUpper(id)
	require.NotEqual(t, id, upper, "uuid must contain a hex letter for the spelling to differ")
	return upper
}

func newCanonicalSpellingVoiceHandler(t *testing.T, ts *testhelpers.TestServer, placement bool) *dm.Handler {
	t.Helper()
	url := "nats://localhost:4222"
	if configured := os.Getenv("NATS_URL"); configured != "" {
		url = configured
	}
	bus, err := natsclient.Connect(url)
	if err != nil {
		t.Skipf("NATS unavailable (%v); skipping live DM enforcement test", err)
	}
	t.Cleanup(func() { _ = bus.Close() })
	return dm.NewHandler(dm.HandlerDeps{
		DB:                   ts.DB,
		Log:                  logger.New("test"),
		Hub:                  ts.Hub,
		Cfg:                  &config.Config{MediaPlaneURL: "http://media.test", JWTSecret: testhelpers.TestJWTSecret},
		NATS:                 bus,
		Redis:                ts.Redis,
		EntCache:             entitlements.NewCache(ts.Redis, ts.DB),
		RoomPlacementEnabled: placement,
	})
}

func TestAuthorizeVoiceJoin_AlternateSpelling_CanonicalPlacementKeyAndResponse(t *testing.T) {
	ts := setupTS(t)
	caller := ts.CreateTestUser(t, "canonjoincaller")
	callee := ts.CreateTestUser(t, "canonjoincallee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	h := newCanonicalSpellingVoiceHandler(t, ts, true)
	ctx := context.Background()
	require.NoError(t, ts.Redis.Set(ctx, "media:active_colour", "blue", 0).Err())
	require.NoError(t, ts.Redis.SAdd(ctx, "media:colours:registered", "blue").Err())
	t.Cleanup(func() {
		ts.Redis.Del(ctx, "voice:room_live:"+convID, "voice:room_owner:"+convID,
			"voice:room_live:"+strings.ToUpper(convID), "voice:room_owner:"+strings.ToUpper(convID))
	})

	upper := upperSpelling(t, convID)
	c, response := crossStoreVoiceContext(t, ts, http.MethodPost, "/voice/join", caller.AccessToken, upper, nil)
	h.AuthorizeVoiceJoin(c)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	var body struct {
		Conversation struct {
			ID string `json:"id"`
		} `json:"conversation"`
	}
	testhelpers.ParseJSON(t, response, &body)
	assert.Equal(t, convID, body.Conversation.ID, "response must carry the canonical conversation id")

	canonicalOwner, err := ts.Redis.Exists(ctx, "voice:room_owner:"+convID).Result()
	require.NoError(t, err)
	rawOwner, err := ts.Redis.Exists(ctx, "voice:room_owner:"+upper).Result()
	require.NoError(t, err)
	assert.EqualValues(t, 1, canonicalOwner, "placement claim must use the canonical room key")
	assert.Zero(t, rawOwner, "no placement claim may exist under the raw spelling")
}

func TestAddMember_AlternateSpelling_CanonicalWSPayloads(t *testing.T) {
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "canonaddadmin")
	member := ts.CreateTestUser(t, "canonaddmember")
	newUser := ts.CreateTestUser(t, "canonaddnew")
	convID := createTestGroup(t, ts, admin, member)
	ts.CreateFriendship(t, admin.ID, newUser.ID, statusAccepted)
	ts.SeedDMKey(t, convID, admin.ID, 1)
	conn := dialDMObserver(t, ts, member.ID, convID)

	upper := upperSpelling(t, convID)
	w := ts.DoRequest("POST", pathDMConversationsPrefix+upper+pathMembers,
		map[string]interface{}{"user_id": newUser.ID}, testhelpers.AuthHeaders(admin.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	added := readUntilDMEvent(t, conn, "dm_participant_added")
	assert.Equal(t, convID, added["conversation_id"])
	revoked := readUntilDMEvent(t, conn, "key_revocation")
	assert.Equal(t, convID, revoked["channel_id"])
}

func TestUpdateAndDeleteMessage_AlternateSpelling_CanonicalWSPayloads(t *testing.T) {
	ts := setupTS(t)
	author := ts.CreateTestUser(t, "canonmsgauthor")
	peer := ts.CreateTestUser(t, "canonmsgpeer")
	ts.CreateFriendship(t, author.ID, peer.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, author.ID, peer.ID)
	msgID := insertDMMessage(t, ts, convID, author.ID, "original")
	conn := dialDMObserver(t, ts, peer.ID, convID)
	upperConv, upperMsg := upperSpelling(t, convID), upperSpelling(t, msgID)

	content := base64.StdEncoding.EncodeToString(make([]byte, 48))
	w := ts.DoRequest("PATCH", pathDMConversationsPrefix+upperConv+pathMsgSlash+upperMsg,
		map[string]interface{}{"content": content, "key_version": 1}, testhelpers.AuthHeaders(author.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	updated := readUntilDMEvent(t, conn, "dm_message_update")
	assert.Equal(t, convID, updated["conversation_id"])
	assert.Equal(t, msgID, updated["id"])

	w = ts.DoRequest("DELETE", pathDMConversationsPrefix+upperConv+pathMsgSlash+upperMsg, nil, testhelpers.AuthHeaders(author.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	deleted := readUntilDMEvent(t, conn, "dm_message_delete")
	assert.Equal(t, convID, deleted["conversation_id"])
	assert.Equal(t, msgID, deleted["id"])
}

func TestAuthorizeVoiceJoin_AlternateSpelling_CommitFailureDisconnectChannelCanonical(t *testing.T) {
	ts := setupTS(t)
	caller := ts.CreateTestUser(t, "canonenfcaller")
	callee := ts.CreateTestUser(t, "canonenfcallee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	h := newCanonicalSpellingVoiceHandler(t, ts, false)
	messages := subscribeDMVoiceDisconnect(t)
	dm.SetDMTopologyCommitHookForTest(h, func(*sql.Tx) error { return errors.New("commit failed") })
	t.Cleanup(func() { dm.SetDMTopologyCommitHookForTest(h, nil) })

	c, response := crossStoreVoiceContext(t, ts, http.MethodPost, "/voice/join", caller.AccessToken, upperSpelling(t, convID), nil)
	h.AuthorizeVoiceJoin(c)
	require.Equal(t, http.StatusInternalServerError, response.Code)

	payload := waitDMVoiceEnforcement(t, messages, caller.ID)
	assert.Equal(t, convID, payload["channelId"], "the media plane drops non-canonical channel ids, losing the compensation")
}

func TestAuthorizeDMVoiceForMediaPlane_AlternateSpelling_CommitFailureDisconnectChannelCanonical(t *testing.T) {
	ts := setupTS(t)
	caller := ts.CreateTestUser(t, "canonenfmcaller")
	callee := ts.CreateTestUser(t, "canonenfmcallee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	h := newCanonicalSpellingVoiceHandler(t, ts, false)
	messages := subscribeDMVoiceDisconnect(t)
	callID := seedCanonicalMediaCall(t, ts, caller.ID, convID)
	dm.SetDMTopologyCommitHookForTest(h, func(*sql.Tx) error { return errors.New("commit failed") })
	t.Cleanup(func() { dm.SetDMTopologyCommitHookForTest(h, nil) })

	upper := upperSpelling(t, convID)
	c, response := crossStoreVoiceContext(t, ts, http.MethodPost, "/voice/authorize", caller.AccessToken, upper,
		map[string]string{"call_id": callID})
	c.Request.Header = dmVoiceMediaAuthorizationHeaders(caller.AccessToken, convID, callID)
	h.AuthorizeDMVoiceForMediaPlane(c)
	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())

	payload := waitDMVoiceEnforcement(t, messages, caller.ID)
	assert.Equal(t, convID, payload["channelId"], "the media plane drops non-canonical channel ids, losing the compensation")
}

func seedCanonicalMediaCall(t *testing.T, ts *testhelpers.TestServer, callerID, convID string) string {
	t.Helper()
	convUUID, callUUID, callerUUID := uuid.MustParse(convID), uuid.New(), uuid.MustParse(callerID)
	ctx := context.Background()
	require.NoError(t, dm.RefreshDMVoiceCallLease(ctx, ts.Redis, dm.VoiceCallLease{
		ConversationID: convUUID, CallID: callUUID, CallerUserID: callerUUID,
	}, dm.DMVoiceCallReservationTTL, true))
	require.NoError(t, dm.RememberDMVoiceJoinAdmission(ctx, ts.Redis, convUUID, callerUUID, callUUID, dm.DMVoiceCallReservationTTL))
	return callUUID.String()
}

func TestAddMember_AlternateSpellingUserID_CanonicalPayload(t *testing.T) {
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "canonadduidadmin")
	member := ts.CreateTestUser(t, "canonadduidmember")
	newUser := ts.CreateTestUser(t, "canonadduidnew")
	convID := createTestGroup(t, ts, admin, member)
	ts.CreateFriendship(t, admin.ID, newUser.ID, statusAccepted)
	ts.SeedDMKey(t, convID, admin.ID, 1)
	conn := dialDMObserver(t, ts, member.ID, convID)

	w := ts.DoRequest("POST", pathDMConversationsPrefix+convID+pathMembers,
		map[string]interface{}{"user_id": upperSpelling(t, newUser.ID)}, testhelpers.AuthHeaders(admin.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	added := readUntilDMEvent(t, conn, "dm_participant_added")
	assert.Equal(t, newUser.ID, added["user_id"], "the added member id must be canonical so hub delivery and clients match it")
}

func TestDeleteGroup_AlternateSpelling_CanonicalWSPayload(t *testing.T) {
	ts := setupTS(t)
	admin := ts.CreateTestUser(t, "canondeladmin")
	member := ts.CreateTestUser(t, "canondelmember")
	convID := createTestGroup(t, ts, admin, member)
	conn := dialDMObserver(t, ts, member.ID, convID)

	w := ts.DoRequest("DELETE", pathDMConversationsPrefix+upperSpelling(t, convID), nil, testhelpers.AuthHeaders(admin.AccessToken))
	require.Less(t, w.Code, 300, w.Body.String())

	deleted := readUntilDMEvent(t, conn, "dm_group_deleted")
	assert.Equal(t, convID, deleted["conversation_id"])
}
