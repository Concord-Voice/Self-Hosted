package voice_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/voice"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/voicecommand"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type authorityFixture struct {
	ts       *testhelpers.TestServer
	resolver *rbac.Resolver
	serverID string
	owner    testhelpers.TestUser
	mover    testhelpers.TestUser
	plain    testhelpers.TestUser
}

func newAuthorityFixture(t *testing.T, tag string) authorityFixture {
	t.Helper()
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, tag+"_owner")
	mover := ts.CreateTestUser(t, tag+"_mover")
	plain := ts.CreateTestUser(t, tag+"_plain")
	serverID := ts.CreateTestServer(t, owner.ID, "Authority "+tag)
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	ts.AddMemberToServer(t, serverID, plain.ID, roleMember)
	role := ts.CreateTestRole(t, serverID, "Organizer", 5, int64(rbac.PermMoveMembers))
	ts.AssignRoleToUser(t, serverID, mover.ID, role)
	return authorityFixture{
		ts:       ts,
		resolver: rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), logger.New("test")),
		serverID: serverID, owner: owner, mover: mover, plain: plain,
	}
}

func (f authorityFixture) tx(t *testing.T) *sql.Tx {
	t.Helper()
	tx, err := f.ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

func (f authorityFixture) deny(t *testing.T, channelID, userID string, perm rbac.Permission) {
	t.Helper()
	f.ts.CreateChannelOverride(t, channelID, "user", userID, 0, int64(perm))
}

func TestFindVoiceChannelsTx_SortedAllRows(t *testing.T) {
	f := newAuthorityFixture(t, "fvc")
	a := f.ts.CreateVoiceChannel(t, f.serverID, "fvc-a")
	b := f.ts.CreateVoiceChannel(t, f.serverID, "fvc-b")
	joinVoice(t, f.ts, a, f.plain.ID)
	joinVoice(t, f.ts, b, f.plain.ID)

	got, err := voice.FindVoiceChannelsTxForTest(context.Background(), f.tx(t), f.serverID, f.plain.ID)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Less(t, got[0], got[1], "ascending by channel id")
	assert.ElementsMatch(t, []string{a, b}, got)

	none, err := voice.FindVoiceChannelsTxForTest(context.Background(), f.tx(t), f.serverID, f.mover.ID)
	require.NoError(t, err)
	assert.Empty(t, none)
}

func TestAuthorizeMoveDestinationTx(t *testing.T) {
	f := newAuthorityFixture(t, "amd")
	ctx := context.Background()
	call := func(t *testing.T, dest, actor string, self bool) error {
		_, err := voice.AuthorizeMoveDestinationTxForTest(ctx, f.tx(t), f.resolver, f.serverID, actor, dest, self)
		return err
	}

	// Positive control: plain visible destination for a Move holder.
	okDest := f.ts.CreateVoiceChannel(t, f.serverID, "amd-ok")
	require.NoError(t, call(t, okDest, f.mover.ID, false))

	otherServer := f.ts.CreateTestServer(t, f.owner.ID, "Authority amd other")
	noView := f.ts.CreateVoiceChannel(t, f.serverID, "amd-noview")
	f.deny(t, noView, f.mover.ID, rbac.PermViewVoiceChannels)
	noJoin := f.ts.CreateVoiceChannel(t, f.serverID, "amd-nojoin")
	f.deny(t, noJoin, f.mover.ID, rbac.PermJoinVoice)
	noMove := f.ts.CreateVoiceChannel(t, f.serverID, "amd-nomove")
	f.deny(t, noMove, f.mover.ID, rbac.PermMoveMembers)

	cases := map[string]string{
		"missing channel":      uuid.New().String(),
		"text channel":         f.ts.CreateTestChannel(t, f.serverID, "amd-text"),
		"other server channel": f.ts.CreateVoiceChannel(t, otherServer, "amd-foreign"),
		"no VIEW":              noView,
		"no JOIN":              noJoin,
		"channel DENY Move":    noMove,
	}
	for name, dest := range cases {
		t.Run(name, func(t *testing.T) {
			err := call(t, dest, f.mover.ID, false)
			require.Error(t, err)
			assert.ErrorIs(t, err, voice.ErrVoiceModPermissionForTest)
		})
	}

	t.Run("owner is not narrowed by a channel DENY Move", func(t *testing.T) {
		f.deny(t, noMove, f.owner.ID, rbac.PermMoveMembers)
		require.NoError(t, call(t, noMove, f.owner.ID, false))
	})
	t.Run("self move needs only VIEW and JOIN", func(t *testing.T) {
		require.NoError(t, call(t, okDest, f.plain.ID, true))
	})
	t.Run("non-self without Move is refused", func(t *testing.T) {
		err := call(t, okDest, f.plain.ID, false)
		assert.ErrorIs(t, err, voice.ErrVoiceModPermissionForTest)
	})
}

func TestAuthorizeMoveSourcesTx(t *testing.T) {
	f := newAuthorityFixture(t, "ams")
	ctx := context.Background()
	hidden := f.ts.CreateVoiceChannel(t, f.serverID, "ams-hidden")
	f.deny(t, hidden, f.mover.ID, rbac.PermViewVoiceChannels)
	visible := f.ts.CreateVoiceChannel(t, f.serverID, "ams-visible")

	t.Run("hidden source is dropped, visible with Move kept", func(t *testing.T) {
		got, err := voice.AuthorizeMoveSourcesTxForTest(ctx, f.tx(t), f.resolver, f.serverID, f.mover.ID, []string{hidden, visible})
		require.NoError(t, err)
		assert.Equal(t, []string{visible}, got)
	})
	t.Run("visible source without Move is refused", func(t *testing.T) {
		noMove := f.ts.CreateVoiceChannel(t, f.serverID, "ams-nomove")
		f.deny(t, noMove, f.mover.ID, rbac.PermMoveMembers)
		_, err := voice.AuthorizeMoveSourcesTxForTest(ctx, f.tx(t), f.resolver, f.serverID, f.mover.ID, []string{visible, noMove})
		assert.ErrorIs(t, err, voice.ErrVoiceModPermissionForTest)
	})
}

func TestFilterAuthorizedSessionSpecsTx(t *testing.T) {
	f := newAuthorityFixture(t, "fas")
	ctx := context.Background()
	muteRole := f.ts.CreateTestRole(t, f.serverID, "Muter", 6, int64(rbac.PermMuteMembers))
	f.ts.AssignRoleToUser(t, f.serverID, f.mover.ID, muteRole)
	hidden := f.ts.CreateVoiceChannel(t, f.serverID, "fas-hidden")
	f.deny(t, hidden, f.mover.ID, rbac.PermViewVoiceChannels)
	allowed := f.ts.CreateVoiceChannel(t, f.serverID, "fas-allowed")
	denied := f.ts.CreateVoiceChannel(t, f.serverID, "fas-denied")
	f.deny(t, denied, f.mover.ID, rbac.PermMuteMembers)
	spec := func(ch string) voicecommand.CommandSpec { return voicecommand.CommandSpec{ChannelID: &ch} }
	run := func(specs ...voicecommand.CommandSpec) ([]voicecommand.CommandSpec, error) {
		return voice.FilterAuthorizedSessionSpecsTxForTest(ctx, f.tx(t), f.resolver, f.serverID, f.mover.ID, rbac.PermMuteMembers, specs)
	}

	t.Run("hidden spec dropped, allowed kept", func(t *testing.T) {
		got, err := run(spec(hidden), spec(allowed))
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, allowed, *got[0].ChannelID)
	})
	t.Run("visible channel lacking the permission is refused", func(t *testing.T) {
		_, err := run(spec(denied), spec(allowed))
		assert.ErrorIs(t, err, voice.ErrVoiceModPermissionForTest)
	})
	t.Run("empty channel id is an internal error, not a permission refusal", func(t *testing.T) {
		empty := ""
		for _, s := range []voicecommand.CommandSpec{{ChannelID: &empty}, {}} {
			_, err := run(s)
			require.Error(t, err)
			assert.NotErrorIs(t, err, voice.ErrVoiceModPermissionForTest)
		}
	})
}

func TestRequireActorVoiceChannelPermTx(t *testing.T) {
	f := newAuthorityFixture(t, "rav")
	ctx := context.Background()
	ok := f.ts.CreateVoiceChannel(t, f.serverID, "rav-ok")
	hidden := f.ts.CreateVoiceChannel(t, f.serverID, "rav-hidden")
	f.deny(t, hidden, f.mover.ID, rbac.PermViewVoiceChannels)
	noPerm := f.ts.CreateVoiceChannel(t, f.serverID, "rav-noperm")
	f.deny(t, noPerm, f.mover.ID, rbac.PermMoveMembers)

	call := func(ch string) error {
		return voice.RequireActorVoiceChannelPermTxForTest(ctx, f.tx(t), f.resolver, f.serverID, f.mover.ID, ch, rbac.PermMoveMembers)
	}
	require.NoError(t, call(ok))
	for name, ch := range map[string]string{"absent": uuid.New().String(), "not VIEW": hidden, "lacks perm": noPerm} {
		t.Run(name, func(t *testing.T) {
			assert.ErrorIs(t, call(ch), voice.ErrVoiceModPermissionForTest)
		})
	}
}

// Review Focus 1: alternate UUID spellings of the actor in :userId must be
// recognised as a self action (compared canonically).
func TestVoiceModPreamble_AlternateSpellingSelfMove(t *testing.T) {
	f := newAuthorityFixture(t, "alt")
	gin.SetMode(gin.TestMode)
	h := voice.NewHandler(voice.HandlerDeps{DB: f.ts.DB, Log: logger.New("test"), Resolver: f.resolver, Redis: f.ts.Redis})

	type result struct {
		self bool
		ok   bool
	}
	run := func(actorID, targetSpelling string) (result, int) {
		var res result
		r := gin.New()
		r.Use(func(c *gin.Context) { c.Set("user_id", actorID); c.Next() })
		r.POST("/servers/:id/voice/:userId/probe", func(c *gin.Context) {
			res.self, _, res.ok = voice.AuthorizeVoiceModWithForTest(h, c, rbac.PermMoveMembers, true, true)
			if res.ok {
				c.Status(http.StatusOK)
			}
		})
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/servers/"+f.serverID+"/voice/"+url.PathEscape(targetSpelling)+"/probe", nil)
		r.ServeHTTP(w, req)
		return res, w.Code
	}

	spellings := map[string]string{
		"canonical": f.plain.ID,
		"uppercase": strings.ToUpper(f.plain.ID),
		"braces":    "{" + f.plain.ID + "}",
		"raw hex":   strings.ReplaceAll(f.plain.ID, "-", ""),
	}
	for name, spelling := range spellings {
		t.Run(name, func(t *testing.T) {
			res, code := run(f.plain.ID, spelling)
			assert.Equal(t, http.StatusOK, code, "a plain member acting on themselves needs no Move bit")
			assert.True(t, res.ok)
			assert.True(t, res.self)
		})
	}

	t.Run("positive control: another target without Move is refused", func(t *testing.T) {
		res, code := run(f.plain.ID, strings.ToUpper(f.mover.ID))
		assert.Equal(t, http.StatusForbidden, code)
		assert.False(t, res.ok)
	})
}
