package admin_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// fixedAnswerAgent answers every request with one status and body.
func fixedAnswerAgent(t *testing.T, status int, body string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "agent")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	sock := filepath.Join(dir, "agent.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { require.NoError(t, srv.Close()) })
	return sock
}

// Only a 404 that names authorizer_not_imported allows the database-only
// fallback; every other answer the agent could give must mint nothing.
func TestWebAuthnLoginImportedCredentialAnswers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		wantStatus int
		wantLogin  bool
	}{
		{"not imported falls back", http.StatusNotFound, `{"error":"authorizer_not_imported"}`, http.StatusOK, true},
		{"404 other error", http.StatusNotFound, `{"error":"other"}`, http.StatusBadGateway, false},
		{"404 empty body", http.StatusNotFound, ``, http.StatusBadGateway, false},
		{"500", http.StatusInternalServerError, `{"error":"authorizer_not_imported"}`, http.StatusBadGateway, false},
		{"429", http.StatusTooManyRequests, `{"error":"authorizer_not_imported"}`, http.StatusBadGateway, false},
		{"400", http.StatusBadRequest, `{"error":"authorizer_not_imported"}`, http.StatusBadGateway, false},
		{"oversized body", http.StatusNotFound, `{"error":"authorizer_not_imported","pad":"` + strings.Repeat("x", 1100) + `"}`, http.StatusBadGateway, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, dbCleanup := testhelpers.SetupTestDB(t)
			t.Cleanup(dbCleanup)
			rdb, rCleanup := testhelpers.SetupTestRedis(t)
			t.Cleanup(rCleanup)
			_, username, va := enrolledAdmin(t, db, rdb)
			t.Setenv("HOST_AGENT_AUTHORIZER_PROJECTION", "1")
			engine := adminAuthEngineWithAgent(t, db, rdb, fixedAnswerAgent(t, tc.status, tc.body))

			pwRec := postJSON(engine, "/admin/api/v1/auth/password", map[string]string{"username": username, "password": adminTestPassword})
			require.Equal(t, http.StatusOK, pwRec.Code, pwRec.Body.String())
			var pwResp struct {
				Handle    string                        `json:"handle"`
				PublicKey *protocol.CredentialAssertion `json:"publicKey"`
			}
			require.NoError(t, json.Unmarshal(pwRec.Body.Bytes(), &pwResp))
			va.signCount++
			assertionBody := va.assertionResponse(t, testAdminRPID, pwResp.PublicKey.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV, false)

			waRec := postJSON(engine, "/admin/api/v1/auth/webauthn", map[string]any{"handle": pwResp.Handle, "assertion": json.RawMessage(assertionBody)})
			require.Equal(t, tc.wantStatus, waRec.Code, waRec.Body.String())
			sessions, err := rdb.Keys(context.Background(), "admin_session:*").Result()
			require.NoError(t, err)
			if tc.wantLogin {
				assert.NotEmpty(t, waRec.Header().Get("Set-Cookie"))
				assert.Len(t, sessions, 1)
				return
			}
			assert.Empty(t, waRec.Header().Get("Set-Cookie"), "no session cookie")
			assert.Empty(t, sessions, "no session key")
		})
	}
}
