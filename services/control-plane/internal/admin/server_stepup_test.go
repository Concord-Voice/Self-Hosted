package admin_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const agentAuthorizationHandle = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

type agentRequest struct {
	Path string
	Body json.RawMessage
}

// authorizationAgent serves the same Unix-socket HTTP boundary as concord-agent.
func authorizationAgent(t *testing.T) (string, func() []agentRequest) {
	t.Helper()
	dir, err := os.MkdirTemp("", "agent")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	sock := filepath.Join(dir, "agent.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	var (
		mu       sync.Mutex
		requests []agentRequest
	)
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			http.Error(w, "read", http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, agentRequest{Path: r.URL.Path, Body: body})
		mu.Unlock()
		if r.URL.Path == "/v1/authorizations" {
			_, _ = w.Write([]byte(`{"handle":"` + agentAuthorizationHandle + `","options":{"challenge":"agent-bound"}}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"fedcba9876543210fedcba9876543210","state":"running"}`))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { require.NoError(t, srv.Close()) })
	return sock, func() []agentRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]agentRequest(nil), requests...)
	}
}

func TestServerWriteProxiesAgentAuthorizationBeforeFinalProof(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)
	created, err := admin.NewAdminRepo(db).CreatePending(context.Background(), uniqueAdminUsername("agent-proxy"), "x")
	require.NoError(t, err)
	adminID := created.ID
	h, err := admin.NewHandler(db, nil, logger.NewWithWriter(io.Discard), authHandlerCfg())
	require.NoError(t, err)
	sock, requests := authorizationAgent(t)
	admin.UseServerAgentForTest(h, sock)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set("admin_id", adminID)
		c.Next()
	})
	engine.POST("/stepup", h.ServerStepUpBegin)
	engine.POST("/actions/:action", h.ServerAction)

	begin := postJSON(engine, "/stepup", map[string]string{"action": "update", "tag": "v1.2.3"})
	require.Equal(t, http.StatusOK, begin.Code, begin.Body.String())
	var challenge struct {
		Handle    string          `json:"handle"`
		PublicKey json.RawMessage `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(begin.Body.Bytes(), &challenge))
	require.Equal(t, agentAuthorizationHandle, challenge.Handle)
	require.JSONEq(t, `{"challenge":"agent-bound"}`, string(challenge.PublicKey))

	action := postJSON(engine, "/actions/update", map[string]any{
		"tag":    "v1.2.3",
		"stepup": map[string]any{"handle": challenge.Handle, "assertion": map[string]any{}},
	})
	require.Equal(t, http.StatusAccepted, action.Code, action.Body.String())

	got := requests()
	require.Len(t, got, 2)
	require.Equal(t, "/v1/authorizations", got[0].Path)
	require.JSONEq(t, `{"admin_id":"`+adminID+`","action":"update","arguments":{"tag":"v1.2.3"}}`, string(got[0].Body))
	require.Equal(t, "/v1/actions/update", got[1].Path)
	require.JSONEq(t, `{"handle":"`+agentAuthorizationHandle+`","assertion":{}}`, string(got[1].Body))
}
