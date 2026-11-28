package voice_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/stmthook"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/voice"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
)

// participantsRowFragment appears only in the participants SELECT; no resolver
// statement reads voice_participants.
const participantsRowFragment = `vp.is_muted, vp.is_deafened`

// A row the handler cannot scan must fail the request. Dropping it would
// answer 200 with a participant list that silently omits someone.
func TestGetParticipants_ScanErrorAnswers500(t *testing.T) {
	ts := setupTS(t)
	hook, hookedDB := stmthook.Open(t)

	owner := ts.CreateTestUser(t, "vpscanowner")
	serverID := ts.CreateTestServer(t, owner.ID, "VP Scan Error")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-scan-error")
	_, err := ts.DB.Exec(`
		INSERT INTO voice_participants (channel_id, user_id, is_muted, is_deafened, is_video_on, is_screen_sharing, joined_at)
		VALUES ($1, $2, false, false, false, false, NOW())
	`, channelID, owner.ID)
	require.NoError(t, err)

	// The handler runs on the hooked pool; the resolver stays on the plain pool
	// so its queries never pass through the hook.
	h := voice.NewHandler(voice.HandlerDeps{
		DB:       hookedDB,
		Log:      logger.New("test"),
		Hub:      ts.Hub,
		Cfg:      &config.Config{},
		Resolver: rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), logger.New("test")),
	})
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("user_id", owner.ID); c.Next() })
	router.GET("/api/v1/channels/:id/voice/participants", h.GetParticipants)
	get := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, pathChannelsPrefix+channelID+pathVoiceParticipants, nil))
		return w
	}

	// The body the handler gives when the participants query itself fails.
	hook.Arm([]string{participantsRowFragment}, nil, stmthook.ErrInjected)
	failed := get()
	require.Equal(t, http.StatusInternalServerError, failed.Code, "body: %s", failed.Body.String())

	t.Run("control: unmatched fragment leaves the row", func(t *testing.T) {
		hook.ArmRowValue("no statement contains this fragment", 4, "not-a-bool")
		w := get()
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
		assert.Zero(t, hook.RowValueHits())
		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Len(t, testhelpers.JSONField[[]interface{}](t, body, "participants"), 1)
	})

	t.Run("unscannable is_muted answers the query-failure body", func(t *testing.T) {
		hook.ArmRowValue(participantsRowFragment, 4, "not-a-bool")
		w := get()
		assert.GreaterOrEqual(t, hook.RowValueHits(), 1, "the participants row must have been rewritten")
		assert.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
		assert.Equal(t, failed.Body.String(), w.Body.String())
	})
}

// A member removed between the channel lookup and the permission read is
// refused exactly as a missing channel is, not reported as an outage.
func TestGetParticipants_MemberRemovedMidCheckAnswersAsMissing(t *testing.T) {
	ts := setupTS(t)
	hook, hookedDB := stmthook.Open(t)

	owner := ts.CreateTestUser(t, "vpmidowner")
	member := ts.CreateTestUser(t, "vpmidmember")
	serverID := ts.CreateTestServer(t, owner.ID, "VP Mid Check")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-mid-check")

	// Handler and resolver share the hooked pool so both statements are seen.
	h := voice.NewHandler(voice.HandlerDeps{
		DB:       hookedDB,
		Log:      logger.New("test"),
		Hub:      ts.Hub,
		Cfg:      &config.Config{},
		Resolver: rbac.NewResolver(hookedDB, rbac.NewPermissionCache(ts.Redis), logger.New("test")),
	})
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("user_id", member.ID); c.Next() })
	router.GET("/api/v1/channels/:id/voice/participants", h.GetParticipants)
	get := func(id string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, pathChannelsPrefix+id+pathVoiceParticipants, nil))
		return w
	}

	hook.Arm(
		[]string{"INNER JOIN server_members sm ON sm.server_id = c.server_id", "SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id"},
		func() error {
			_, err := ts.DB.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, member.ID)
			return err
		}, nil)
	w := get(channelID)

	seen, betweenErr := hook.Report()
	require.NoError(t, betweenErr)
	require.Equal(t, 2, seen, "both statements must have been reached")
	missing := get(uuid.NewString())
	assert.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, missing.Code, w.Code)
	assert.Equal(t, missing.Body.String(), w.Body.String())
}
