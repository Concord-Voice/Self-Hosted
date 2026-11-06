package admin_test

import (
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

// stepUpAgent accepts every action and records the request bodies.
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
	h, err := admin.NewHandler(db, rdb, logger.NewWithWriter(io.Discard), authHandlerCfg())
	require.NoError(t, err)
	sock, bodies := stepUpAgent(t)
	admin.UseServerAgentForTest(h, sock)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) { c.Set("admin_id", adminID); c.Next() })
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

	assert.Len(t, *bodies, 1, "refused step-ups must not reach the agent")
	assert.Equal(t, before+5, auditRowCount(t, db, admin.EventServerWrite))
}
