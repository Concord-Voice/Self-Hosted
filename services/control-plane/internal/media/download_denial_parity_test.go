package media

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/stmthook"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
)

const denialCiphertext = "ciphertext-bytes"

// downloadAs runs the download handler for file as caller.
func downloadAs(ts *testSetup, caller, fileID string) *httptest.ResponseRecorder {
	return ts.doJSON(ts.handler.DownloadAttachment, "GET", pathAttachmentsPrefix+fileID, caller, gin.Params{{Key: "file_id", Value: fileID}})
}

// errorField reads the "error" string of a JSON error body.
func errorField(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &m), "body: %s", w.Body.String())
	v, ok := m["error"].(string)
	require.True(t, ok, "error field must be a string; body: %s", w.Body.String())
	return v
}

// requireDenialIsMissingFile asserts denied is indistinguishable from the same
// caller's answer for a random file id that does not exist: status, body bytes
// and Content-Type, with no X-File-* header and no ciphertext.
func requireDenialIsMissingFile(t *testing.T, denied, missing *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusNotFound, missing.Code, "baseline missing-file answer; body: %s", missing.Body.String())
	require.Equal(t, http.StatusNotFound, denied.Code, "a denial must answer 404; body: %s", denied.Body.String())
	assert.Equal(t, missing.Body.Bytes(), denied.Body.Bytes(), "denial body must be byte-equal to the missing-file body")
	assert.Equal(t, missing.Header().Get("Content-Type"), denied.Header().Get("Content-Type"))
	for name := range denied.Header() {
		assert.False(t, strings.HasPrefix(strings.ToLower(name), "x-file-"), "denial must not carry %s", name)
	}
	assert.False(t, bytes.Contains(denied.Body.Bytes(), []byte(denialCiphertext)), "denial must not carry ciphertext")
}

// requireDeniedAsMissing compares a denial already received for caller with
// that caller's response for a random missing file id.
func requireDeniedAsMissing(t *testing.T, ts *testSetup, caller string, denied *httptest.ResponseRecorder) {
	t.Helper()
	requireDenialIsMissingFile(t, denied, downloadAs(ts, caller, uuid.New().String()))
}

// insertChannelFile stores denialCiphertext for a channel-scoped attachment.
func insertChannelFile(t *testing.T, ts *testSetup, uploader, channelID string) string {
	t.Helper()
	fileID := uuid.New().String()
	storageKey := fmt.Sprintf(fmtAttachmentsKey, fileID)
	require.NoError(t, ts.store.PutObject(context.TODO(), storageKey, bytes.NewReader([]byte(denialCiphertext)), int64(len(denialCiphertext)), mimeOctetStream))
	_, err := ts.db.Exec(
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, channel_id)
		 VALUES ($1, $2, 'file', 2, 'application/octet-stream', $3, $4, 1, $5)`,
		fileID, uploader, len(denialCiphertext), storageKey, channelID)
	require.NoError(t, err)
	return fileID
}

func resolverTestSetup(t *testing.T) *testSetup {
	t.Helper()
	db, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)
	rdb := mediaTestRedis(t)
	return mediaTestWithResolver(t, db, rbac.NewResolver(db, rbac.NewPermissionCache(rdb), logger.New("test")))
}

func addMember(t *testing.T, ts *testSetup, serverID, userID string) {
	t.Helper()
	_, err := ts.db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'member')`, serverID, userID)
	require.NoError(t, err)
}

type denialCell struct {
	name string
	// setup returns the harness, the denied caller, the file id, and a control
	// caller who must receive the content (empty when the cell has none).
	setup func(t *testing.T) (ts *testSetup, denied, fileID, control string)
}

func TestDownloadAttachment_DenialsMatchNotFound(t *testing.T) {
	channelCell := func(channelType string, perms rbac.Permission, withResolver bool) func(t *testing.T) (*testSetup, string, string, string) {
		return func(t *testing.T) (*testSetup, string, string, string) {
			var ts *testSetup
			if withResolver {
				ts = resolverTestSetup(t)
			} else {
				ts = setupMediaTest(t)
			}
			owner := ts.createTestUser(t, "dnm_owner")
			member := ts.createTestUser(t, "dnm_member")
			serverID := ts.createTestServer(t, owner, "Denial Server")
			channelID := ts.createTestChannelWithType(t, serverID, "denied", channelType)
			if perms != 0 {
				addMember(t, ts, serverID, member)
				ts.createTestRoleWithPerms(t, serverID, member, "cell-role", perms)
			}
			return ts, member, insertChannelFile(t, ts, owner, channelID), owner
		}
	}

	cells := []denialCell{
		{name: "non-member", setup: channelCell("text", 0, false)},
		{name: "view denied, history granted", setup: channelCell("text", rbac.PermReadMessageHistory, true)},
		{name: "history denied, view granted", setup: channelCell("text", rbac.PermViewTextChannels, true)},
		// An unknown type fails closed for every caller, the owner included, so
		// this cell has no control; the text cells above prove the fixture.
		{name: "unknown channel type", setup: func(t *testing.T) (*testSetup, string, string, string) {
			ts, denied, fileID, _ := channelCell("forum", rbac.PermViewTextChannels|rbac.PermViewVoiceChannels|rbac.PermReadMessageHistory, true)(t)
			return ts, denied, fileID, ""
		}},
		{name: "DM non-participant", setup: func(t *testing.T) (*testSetup, string, string, string) {
			ts := setupMediaTest(t)
			u1 := ts.createTestUser(t, "dnm_dm1")
			u2 := ts.createTestUser(t, "dnm_dm2")
			outsider := ts.createTestUser(t, "dnm_dmout")
			convID := ts.createTestDMConversation(t, u1, u2)
			fileID, _ := insertDMDownloadFixture(t, ts, u1, convID, []byte(denialCiphertext))
			return ts, outsider, fileID, u2
		}},
		{name: "DM participant, file hidden by a Clear range", setup: func(t *testing.T) (*testSetup, string, string, string) {
			ts := setupMediaTest(t)
			actor := ts.createTestUser(t, "dnm_clear_actor")
			peer := ts.createTestUser(t, "dnm_clear_peer")
			convID := ts.createTestDMConversation(t, actor, peer)
			fileID, _ := insertDMDownloadFixture(t, ts, actor, convID, []byte(denialCiphertext))
			hideDMMessageForDownload(t, ts, actor, convID, time.Unix(0, 0), time.Now().Add(time.Minute), true)
			return ts, actor, fileID, peer
		}},
	}

	for _, cell := range cells {
		t.Run(cell.name, func(t *testing.T) {
			ts, denied, fileID, control := cell.setup(t)

			w := downloadAs(ts, denied, fileID)
			requireDenialIsMissingFile(t, w, downloadAs(ts, denied, uuid.New().String()))

			if control == "" {
				return
			}
			// Control: someone who may read the file gets the ciphertext, so the
			// fixture is valid and the 404 above is the gate, not a broken file.
			ok := downloadAs(ts, control, fileID)
			assert.Equal(t, http.StatusOK, ok.Code, "control; body: %s", ok.Body.String())
			assert.Equal(t, denialCiphertext, ok.Body.String())
		})
	}
}

func TestUserCanDownloadAttachment_NoScopeAnswersNotFound(t *testing.T) {
	ts := setupMediaTest(t)
	user := ts.createTestUser(t, "dnm_noscope")
	missing := downloadAs(ts, user, uuid.New().String())

	c, w := newCheckSendCtx()
	ok := ts.handler.userCanDownloadAttachment(c, user, user, uuid.New().String(), nil, nil)

	assert.False(t, ok)
	requireDenialIsMissingFile(t, w, missing)
}

func closedDBHandler(t *testing.T) *testSetup {
	t.Helper()
	rdb := mediaTestRedis(t)
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = defaultTestDatabaseURL
	}
	closed, err := sql.Open("postgres", dbURL)
	require.NoError(t, err)
	require.NoError(t, closed.Close())
	return mediaTestWithResolver(t, closed, rbac.NewResolver(closed, rbac.NewPermissionCache(rdb), logger.New("test")))
}

// A failed lookup is a server fault, never a denial: answering it 404 would
// make an outage look like a missing file.
func TestDownloadChannelAccessAllowed_LookupError_500(t *testing.T) {
	ts := closedDBHandler(t)
	channelID := uuid.New().String()

	c, w := newCheckSendCtx()
	ok := ts.handler.userCanDownloadAttachment(c, uuid.New().String(), uuid.New().String(), uuid.New().String(), &channelID, nil)

	assert.False(t, ok)
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, errMsgFailedVerifyAccess, errorField(t, w))
}

func TestDownloadChannelAccessAllowed_ResolverError_500(t *testing.T) {
	db, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)
	rdb := mediaTestRedis(t)
	ts := mediaTestWithResolver(t, db, mediaBrokenResolver(t, rdb))
	owner := ts.createTestUser(t, "dnm_res_err")
	serverID := ts.createTestServer(t, owner, "Resolver Error")
	channelID := ts.createTestChannel(t, serverID, "res-err")

	c, w := newCheckSendCtx()
	// The decision lookup (working DB) succeeds; the resolver (closed DB) errors.
	ok := ts.handler.userCanDownloadAttachment(c, owner, owner, uuid.New().String(), &channelID, nil)

	assert.False(t, ok)
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, errMsgFailedVerifyAccess, errorField(t, w))
}

// A member removed between the decision lookup and the resolver's membership
// read is a denial (the resolver reports ErrNotMember), not a server fault.
func TestDownloadAttachment_MemberRemovedMidCheckAnswersNotFound(t *testing.T) {
	ts := resolverTestSetup(t)
	owner := ts.createTestUser(t, "dnm_mid_owner")
	member := ts.createTestUser(t, "dnm_mid_member")
	serverID := ts.createTestServer(t, owner, "Mid Check")
	channelID := ts.createTestChannel(t, serverID, "mid")
	addMember(t, ts, serverID, member)
	ts.createTestRoleWithPerms(t, serverID, member, "viewer", rbac.PermViewTextChannels|rbac.PermReadMessageHistory)
	fileID := insertChannelFile(t, ts, owner, channelID)

	hook, hookDB := stmthook.Open(t)
	rdb := mediaTestRedis(t)
	resolver := rbac.NewResolver(hookDB, rbac.NewPermissionCache(rdb), logger.New("test"))
	handler := NewHandler(hookDB, ts.store, logger.New("test"), &config.Config{UploadMaxSize: 25 * 1024 * 1024}, resolver, freeTierStub{})
	hts := &testSetup{handler: handler, store: ts.store, db: hookDB}

	hook.Arm(
		[]string{"FROM channels ch", "SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id"},
		func() error {
			_, err := ts.db.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, member)
			return err
		}, nil)

	w := downloadAs(hts, member, fileID)

	seen, betweenErr := hook.Report()
	require.NoError(t, betweenErr)
	require.Equal(t, 2, seen, "both statements must have been reached")
	requireDenialIsMissingFile(t, w, downloadAs(hts, member, uuid.New().String()))
}

// Upload names a channel the caller chose, so its denial stays a 403.
func TestUploadAttachment_ViewDeniedStill403(t *testing.T) {
	ts := resolverTestSetup(t)
	owner := ts.createTestUser(t, "dnm_up_owner")
	member := ts.createTestUser(t, "dnm_up_member")
	serverID := ts.createTestServer(t, owner, "Upload View Denied")
	channelID := ts.createTestChannel(t, serverID, "up-hidden")
	addMember(t, ts, serverID, member)
	// Everything but the view bit, so the view gate is the only one that denies.
	ts.createTestRoleWithPerms(t, serverID, member, "no-view", rbac.PermSendMessages|rbac.PermAttachFiles|rbac.PermReadMessageHistory)

	body, ct := multipartBody(t, "file", fileEncryptedBin, []byte("ciphertext"), map[string]string{
		"channel_id":  channelID,
		"file_type":   "file",
		"mime_type":   mimeOctetStream,
		"key_version": "1",
	})
	w := ts.doMultipart(ts.handler.UploadAttachment, "POST", pathUploadAttachment, member, body, ct)

	require.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, errMsgAccessDenied, errorField(t, w))
}
