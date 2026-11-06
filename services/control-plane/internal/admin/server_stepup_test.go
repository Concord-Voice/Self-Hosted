package admin_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const stepUpJobID = "fedcba9876543210fedcba9876543210"

// stepUpAgent accepts every action but restart, which it refuses as busy, and
// records the request bodies.
func stepUpAgent(t *testing.T) (string, *[]string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "agent")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "agent.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	var bodies []string
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, r.URL.Path+" "+string(b))
		if r.URL.Path == "/v1/actions/restart" {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"write_in_progress"}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"` + stepUpJobID + `","state":"running"}`))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock, &bodies
}

func TestServerActionStepUp(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	adminID, _, va := enrolledAdmin(t, db, rdb)
	otherID, _, otherVA := enrolledAdmin(t, db, rdb)
	h, err := admin.NewHandler(db, rdb, logger.NewWithWriter(io.Discard), authHandlerCfg())
	require.NoError(t, err)
	sock, bodies := stepUpAgent(t)
	admin.UseServerAgentForTest(h, sock)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	as := adminID
	engine.Use(func(c *gin.Context) { c.Set("admin_id", as); c.Next() })
	engine.POST("/stepup", h.ServerStepUpBegin)
	engine.POST("/actions/:action", h.ServerAction)

	begin := func(action string) (string, string) {
		rec := postJSON(engine, "/stepup", map[string]string{"action": action})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp struct {
			Handle    string                        `json:"handle"`
			PublicKey *protocol.CredentialAssertion `json:"publicKey"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp.Handle, resp.PublicKey.Response.Challenge.String()
	}
	sign := func(challenge string, flags byte) json.RawMessage {
		va.signCount++
		return va.assertionResponse(t, testAdminRPID, challenge, testAdminOrigin, flags, false)
	}
	act := func(action string, body map[string]any) int {
		return postJSON(engine, "/actions/"+action, body).Code
	}
	before := auditRowCount(t, db, admin.EventServerWrite)

	// A fresh assertion bound to update starts the job and passes only the tag.
	handle, challenge := begin("update")
	assert.Equal(t, http.StatusAccepted, act("update", map[string]any{
		"tag": "v1.2.3", "stepup": map[string]any{"handle": handle, "assertion": sign(challenge, flagUP|flagUV)},
	}))
	require.Equal(t, []string{`/v1/actions/update {"tag":"v1.2.3"}`}, *bodies)

	// The handle is single-use.
	assert.Equal(t, http.StatusForbidden, act("update", map[string]any{
		"tag": "v1.2.3", "stepup": map[string]any{"handle": handle, "assertion": sign(challenge, flagUP|flagUV)},
	}))

	// A handle begun for stop cannot authorise restore.
	handle, challenge = begin("stop")
	assert.Equal(t, http.StatusForbidden, act("restore", map[string]any{
		"archive": "concord-backup-20260101T000000Z.tar.gz", "stepup": map[string]any{"handle": handle, "assertion": sign(challenge, flagUP|flagUV)},
	}))

	// User verification is required.
	handle, challenge = begin("stop")
	assert.Equal(t, http.StatusForbidden, act("stop", map[string]any{
		"stepup": map[string]any{"handle": handle, "assertion": sign(challenge, flagUP)},
	}))

	// A tampered signature fails.
	handle, challenge = begin("restart")
	va.signCount++
	assert.Equal(t, http.StatusForbidden, act("restart", map[string]any{
		"stepup": map[string]any{"handle": handle, "assertion": json.RawMessage(va.assertionResponse(t, testAdminRPID, challenge, testAdminOrigin, flagUP|flagUV, true))},
	}))

	// A handle begun by one admin cannot be redeemed by another, even with
	// that other admin's own valid assertion.
	handle, challenge = begin("stop")
	as = otherID
	otherVA.signCount++
	assert.Equal(t, http.StatusForbidden, act("stop", map[string]any{
		"stepup": map[string]any{"handle": handle, "assertion": otherVA.assertionResponse(t, testAdminRPID, challenge, testAdminOrigin, flagUP|flagUV, false)},
	}))

	// An admin with no enrolled key cannot begin a step-up.
	pending, err := admin.NewAdminRepo(db).CreatePending(context.Background(), uniqueAdminUsername("nokey"), "x")
	require.NoError(t, err)
	registerAdminCleanup(t, db, pending.ID)
	as = pending.ID
	assert.Equal(t, http.StatusForbidden, postJSON(engine, "/stepup", map[string]string{"action": "stop"}).Code)
	as = adminID

	assert.Len(t, *bodies, 1, "refused step-ups must not reach the agent")

	// An agent refusal after a good step-up passes through, and the handle is
	// spent all the same.
	handle, challenge = begin("restart")
	body := map[string]any{"stepup": map[string]any{"handle": handle, "assertion": sign(challenge, flagUP|flagUV)}}
	assert.Equal(t, http.StatusConflict, act("restart", body))
	assert.Equal(t, http.StatusForbidden, act("restart", body))
	assert.Len(t, *bodies, 2)

	// 7 begins (6 ok, 1 refused), 8 action attempts, 2 of which reached the
	// agent and so also wrote a requested row.
	assert.Equal(t, before+17, auditRowCount(t, db, admin.EventServerWrite))
	var stepup bool
	require.NoError(t, db.QueryRow(`SELECT (detail->>'stepup')::bool FROM admin_audit_log
		WHERE event_type = $1 AND detail->>'job_id' = $2`, admin.EventServerWrite, stepUpJobID).Scan(&stepup))
	assert.True(t, stepup)
}
