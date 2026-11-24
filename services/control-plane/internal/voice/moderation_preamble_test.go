package voice_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const notMemberBody = `{"error":"Not a member of this server"}`

type preambleRoute struct {
	name   string
	method string
	suffix string
	isMove bool
}

var preambleRoutes = []preambleRoute{
	{"mute", http.MethodPost, pathMute, false},
	{"unmute", http.MethodDelete, pathMute, false},
	{"deafen", http.MethodPost, pathDeafen, false},
	{"undeafen", http.MethodDelete, pathDeafen, false},
	{"user-mute", http.MethodPost, pathUserMute, false},
	{"user-deafen", http.MethodPost, pathUserDeafen, false},
	{"disconnect", http.MethodPost, pathDisconnect, false},
	{"move", http.MethodPost, pathMove, true},
	{"temp-access", http.MethodDelete, pathTempAccess, false},
}

type preambleFixture struct {
	ts        *testhelpers.TestServer
	owner     testhelpers.TestUser
	outsider  testhelpers.TestUser
	plain     testhelpers.TestUser
	inVoice   testhelpers.TestUser
	idle      testhelpers.TestUser
	serverID  string
	voiceChan string
	otherChan string
	textChan  string
}

func newPreambleFixture(t *testing.T, tag string) preambleFixture {
	t.Helper()
	ts := setupTS(t)
	f := preambleFixture{ts: ts}
	f.owner = ts.CreateTestUser(t, tag+"_owner")
	f.outsider = ts.CreateTestUser(t, tag+"_outsider")
	f.plain = ts.CreateTestUser(t, tag+"_plain")
	f.inVoice = ts.CreateTestUser(t, tag+"_invoice")
	f.idle = ts.CreateTestUser(t, tag+"_idle")
	f.serverID = ts.CreateTestServer(t, f.owner.ID, "Preamble "+tag)
	for _, u := range []testhelpers.TestUser{f.plain, f.inVoice, f.idle} {
		ts.AddMemberToServer(t, f.serverID, u.ID, roleMember)
	}
	f.voiceChan = ts.CreateVoiceChannel(t, f.serverID, tag+"-a")
	f.otherChan = ts.CreateVoiceChannel(t, f.serverID, tag+"-b")
	f.textChan = ts.CreateTestChannel(t, f.serverID, tag+"-text")
	joinVoice(t, ts, f.voiceChan, f.inVoice.ID)
	seedOutboxVoiceSession(t, ts, f.voiceChan, f.inVoice.ID)
	return f
}

type preambleState struct {
	name     string
	target   func(f preambleFixture) string
	moveBody func(f preambleFixture) any
	// raw overrides moveBody with a literal string body.
	raw string
}

var preambleStates = []preambleState{
	{name: "target in voice", target: func(f preambleFixture) string { return f.inVoice.ID },
		moveBody: func(f preambleFixture) any { return map[string]string{"target_channel_id": f.otherChan} }},
	{name: "target not in voice", target: func(f preambleFixture) string { return f.idle.ID },
		moveBody: func(f preambleFixture) any { return map[string]string{"target_channel_id": f.otherChan} }},
	{name: "text destination", target: func(f preambleFixture) string { return f.inVoice.ID },
		moveBody: func(f preambleFixture) any { return map[string]string{"target_channel_id": f.textChan} }},
	{name: "missing destination", target: func(f preambleFixture) string { return f.inVoice.ID },
		moveBody: func(_ preambleFixture) any {
			return map[string]string{"target_channel_id": "00000000-0000-4000-8000-000000000000"}
		}},
	{name: "same channel", target: func(f preambleFixture) string { return f.inVoice.ID },
		moveBody: func(f preambleFixture) any { return map[string]string{"target_channel_id": f.voiceChan} }},
	{name: "malformed body", target: func(f preambleFixture) string { return f.inVoice.ID }, raw: "{not json"},
}

func (f preambleFixture) call(t *testing.T, r preambleRoute, s preambleState, token string) (int, string) {
	t.Helper()
	var body any
	if r.isMove {
		if s.raw != "" {
			body = s.raw
		} else {
			body = s.moveBody(f)
		}
	}
	path := voiceEnforcePath(f.serverID, s.target(f), r.suffix)
	if r.isMove && s.raw != "" {
		req := httptest.NewRequest(r.method, path, strings.NewReader(s.raw))
		req.Header = testhelpers.AuthHeaders(token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.ts.Router.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	rec := f.ts.DoRequest(r.method, path, body, testhelpers.AuthHeaders(token))
	return rec.Code, rec.Body.String()
}

func runUniformMatrix(t *testing.T, tag string, actor func(f preambleFixture) testhelpers.TestUser, want string) {
	f := newPreambleFixture(t, tag)
	token := actor(f).AccessToken
	for _, r := range preambleRoutes {
		for _, s := range preambleStates {
			t.Run(r.name+"/"+s.name, func(t *testing.T) {
				code, body := f.call(t, r, s, token)
				assert.Equal(t, http.StatusForbidden, code, body)
				assert.Equal(t, want, body, "raw bytes must be identical across routes and states")
			})
		}
	}
	requireNoServerOutboxCommands(t, f.ts.DB)
}

func TestVoiceModeration_NonMemberUniform403(t *testing.T) {
	runUniformMatrix(t, "pre_nm", func(f preambleFixture) testhelpers.TestUser { return f.outsider }, notMemberBody)
}

func TestVoiceModeration_MemberWithoutBitUniform403(t *testing.T) {
	runUniformMatrix(t, "pre_nb", func(f preambleFixture) testhelpers.TestUser { return f.plain }, insufficientBody)
}

// Positive control: the owner is not refused by the preamble and sees distinct answers.
func TestVoiceModeration_AuthorizedActorSeesDistinctAnswers(t *testing.T) {
	f := newPreambleFixture(t, "pre_ok")
	move := preambleRoutes[7]
	require.True(t, move.isMove)
	state := func(name string) preambleState {
		for _, s := range preambleStates {
			if s.name == name {
				return s
			}
		}
		t.Fatalf("no state %s", name)
		return preambleState{}
	}

	idleCode, _ := f.call(t, move, state("target not in voice"), f.owner.AccessToken)
	assert.Equal(t, http.StatusConflict, idleCode)
	sameCode, _ := f.call(t, move, state("same channel"), f.owner.AccessToken)
	assert.Equal(t, http.StatusBadRequest, sameCode)
	malCode, _ := f.call(t, move, state("malformed body"), f.owner.AccessToken)
	assert.Equal(t, http.StatusBadRequest, malCode)
	for _, name := range []string{"text destination", "missing destination"} {
		code, body := f.call(t, move, state(name), f.owner.AccessToken)
		// An unusable destination reads as a permission refusal, never as a
		// distinct "no such channel" answer.
		assert.Equal(t, http.StatusForbidden, code, name+": "+body)
		assert.JSONEq(t, insufficientBody, body, name)
	}

	// Not-in-voice reads differently from in-voice for user-mute.
	um := preambleRoutes[4]
	idleUM, _ := f.call(t, um, state("target not in voice"), f.owner.AccessToken)
	assert.Equal(t, http.StatusBadRequest, idleUM)
	liveUM, _ := f.call(t, um, state("target in voice"), f.owner.AccessToken)
	assert.Equal(t, http.StatusAccepted, liveUM)

	mute := preambleRoutes[0]
	muteCode, body := f.call(t, mute, state("target in voice"), f.owner.AccessToken)
	assert.Equal(t, http.StatusOK, muteCode, body)

	moveCode, body := f.call(t, move, state("target in voice"), f.owner.AccessToken)
	assert.Equal(t, http.StatusOK, moveCode, body)
	var parsed map[string]any
	assert.NoError(t, json.Unmarshal([]byte(body), &parsed))
}

func TestVoiceModeration_RefusedCallersTakeNoVisibilityLock(t *testing.T) {
	f := newPreambleFixture(t, "pre_lock")
	ctx := context.Background()
	tx, err := f.ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	require.NoError(t, rbac.LockServerVisibilityCapture(ctx, tx, f.serverID))

	move := preambleRoutes[7]
	state := preambleStates[0]

	finishes := func(token string) (int, string, bool) {
		type res struct {
			code int
			body string
		}
		ch := make(chan res, 1)
		go func() {
			c, b := f.call(t, move, state, token)
			ch <- res{c, b}
		}()
		select {
		case r := <-ch:
			return r.code, r.body, true
		case <-time.After(2 * time.Second):
			return 0, "", false
		}
	}

	code, body, ok := finishes(f.outsider.AccessToken)
	require.True(t, ok, "non-member move must not wait on the visibility lock")
	assert.Equal(t, http.StatusForbidden, code)
	assert.Equal(t, notMemberBody, body)

	code, body, ok = finishes(f.plain.AccessToken)
	require.True(t, ok, "member-without-bit move must not wait on the visibility lock")
	assert.Equal(t, http.StatusForbidden, code)
	assert.Equal(t, insufficientBody, body)

	type res struct {
		code int
		body string
	}
	done := make(chan res, 1)
	go func() {
		c, b := f.call(t, move, state, f.owner.AccessToken)
		done <- res{c, b}
	}()
	select {
	case r := <-done:
		t.Fatalf("authorized move completed while the lock was held: %d %s", r.code, r.body)
	case <-time.After(500 * time.Millisecond):
	}
	require.NoError(t, tx.Rollback())
	select {
	case r := <-done:
		assert.Equal(t, http.StatusOK, r.code, r.body)
	case <-time.After(10 * time.Second):
		t.Fatal("authorized move did not complete after the lock was released")
	}
}
