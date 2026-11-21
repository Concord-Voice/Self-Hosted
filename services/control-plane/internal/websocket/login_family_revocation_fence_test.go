package websocket

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/sessions"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	ws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// revocationFencePassword is a throwaway fixture credential for the revoke step-up.
const revocationFencePassword = "Fence-fixture-password-123!" //nolint:gosec // test fixture, not a real secret // pragma: allowlist secret

func awaitFamilyFrame(conn *ws.Conn, want string, within time.Duration) error {
	if err := conn.SetReadDeadline(time.Now().Add(within)); err != nil {
		return err
	}
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var msg struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &msg)
		if msg.Type == want {
			return nil
		}
	}
}

func dialFamilySocket(t *testing.T, r loginFamilySocketRig, server *httptest.Server, sid uuid.UUID) *ws.Conn {
	t.Helper()
	conn, resp, err := ws.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws",
		http.Header{"Authorization": []string{"Bearer " + r.token(t, r.user, sid, "", r.expiry)}})
	require.NoError(t, err)
	_ = resp.Body.Close()
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func addSocketFamily(t *testing.T, r loginFamilySocketRig) (uuid.UUID, uuid.UUID) {
	t.Helper()
	family, sid := uuid.New(), uuid.New()
	_, err := r.db.Exec(`INSERT INTO login_families(family_id,user_id,credential_epoch) VALUES($1,$2,'')`, family, r.user)
	require.NoError(t, err)
	_, err = r.db.Exec(`INSERT INTO login_family_sids(sid,family_id,user_id,access_expires_at) VALUES($1,$2,$3,$4)`, sid, family, r.user, r.expiry)
	require.NoError(t, err)
	return family, sid
}

func bulkRevocationContext(r loginFamilySocketRig, w *httptest.ResponseRecorder) *gin.Context {
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/sessions/revoke-all",
		strings.NewReader(`{"password":"`+revocationFencePassword+`","include_current":false}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("user_id", r.user.String())
	c.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{"user_id": r.user.String(), "sid": r.sid.String(), "cred_epoch": "", "exp": float64(r.expiry.Unix())})
	c.Request = c.Request.WithContext(loginfamily.WithAccessIdentity(c.Request.Context(),
		loginfamily.AccessIdentity{UserID: r.user, SID: r.sid, AccessExpiresAt: r.expiry}))
	return c
}

func startFamilySocketServer(t *testing.T, r loginFamilySocketRig) *httptest.Server {
	t.Helper()
	hash, err := auth.HashPassword(revocationFencePassword)
	require.NoError(t, err)
	_, err = r.db.Exec(`UPDATE users SET password_hash=$2 WHERE id=$1`, r.user, hash)
	require.NoError(t, err)
	_, err = r.db.Exec(`INSERT INTO login_family_rollout(id) VALUES (TRUE) ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/ws", r.h.HandleWebSocket)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	go r.h.hub.Run()
	t.Cleanup(r.h.hub.Shutdown)
	return server
}

// Regression for the PR #4135 red-team PoC: DELETE /sessions/:id answered 200
// while the revoked family's open control socket kept full send/receive
// authority until the 5s reconciler ticker. No reconciler runs here, so the
// only thing that can close the socket is the handler's synchronous fence.
func TestRevokeSessionFencesRevokedSocketBeforeResponse(t *testing.T) {
	r := loginFamilyRig(t)
	_, err := r.db.Exec(`INSERT INTO login_family_rollout(id) VALUES (TRUE) ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	hash, err := auth.HashPassword(revocationFencePassword)
	require.NoError(t, err)
	_, err = r.db.Exec(`UPDATE users SET password_hash=$2 WHERE id=$1`, r.user, hash)
	require.NoError(t, err)
	// An unrelated second login of the same user must survive the revoke.
	siblingFamily, siblingSID := uuid.New(), uuid.New()
	_, err = r.db.Exec(`INSERT INTO login_families(family_id,user_id,credential_epoch) VALUES($1,$2,'')`, siblingFamily, r.user)
	require.NoError(t, err)
	_, err = r.db.Exec(`INSERT INTO login_family_sids(sid,family_id,user_id,access_expires_at) VALUES($1,$2,$3,$4)`, siblingSID, siblingFamily, r.user, r.expiry)
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/ws", r.h.HandleWebSocket)
	srv := httptest.NewServer(router)
	defer srv.Close()
	go r.h.hub.Run()
	t.Cleanup(r.h.hub.Shutdown)

	dial := func(sid uuid.UUID) *ws.Conn {
		t.Helper()
		conn, resp, err := ws.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws",
			http.Header{"Authorization": []string{"Bearer " + r.token(t, r.user, sid, "", r.expiry)}})
		require.NoError(t, err)
		_ = resp.Body.Close()
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	revoked, sibling := dial(r.sid), dial(siblingSID)
	require.NoError(t, awaitFamilyFrame(revoked, "connected", 5*time.Second))
	require.NoError(t, awaitFamilyFrame(sibling, "connected", 5*time.Second))

	// The real DELETE /sessions/:id handler, authenticated as the sibling login.
	handler := sessions.NewHandler(r.db, r.h.redis, logger.NewWithWriter(io.Discard), r.h.hub, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, "/sessions/"+r.sid.String(), strings.NewReader(`{"password":"`+revocationFencePassword+`"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: r.sid.String()}}
	c.Set("user_id", r.user.String())
	c.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{"user_id": r.user.String(), "sid": siblingSID.String(), "cred_epoch": "", "exp": float64(r.expiry.Unix())})
	// AuthRequired attaches the requester's signed identity; the revoke revalidates it.
	c.Request = c.Request.WithContext(loginfamily.WithAccessIdentity(c.Request.Context(),
		loginfamily.AccessIdentity{UserID: r.user, SID: siblingSID, AccessExpiresAt: r.expiry}))
	handler.RevokeSession(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// Immediately after the 200 — far inside the reconciler's 5s tick — the
	// revoked socket must neither answer a frame nor stay open.
	_ = revoked.WriteJSON(map[string]any{"type": "connection_ready_probe"})
	require.Error(t, awaitFamilyFrame(revoked, "connection_ready", 2*time.Second),
		"revoked family's socket must be closed before the revoke response, not answer frames")

	// The unrelated login keeps full authority.
	require.NoError(t, sibling.WriteJSON(map[string]any{"type": "connection_ready_probe"}))
	require.NoError(t, awaitFamilyFrame(sibling, "connection_ready", 3*time.Second),
		"an unrelated session of the same user must stay live")
}

func TestRevokeSessionFencesRealTargetAfterAmbiguousCommitAndLookupFailure(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "rolled back", true: "committed"}[committed], func(t *testing.T) {
			r := loginFamilyRig(t)
			server := startFamilySocketServer(t, r)
			targetFamily, targetSID := addSocketFamily(t, r)
			preserved := dialFamilySocket(t, r, server, r.sid)
			target := dialFamilySocket(t, r, server, targetSID)
			require.NoError(t, awaitFamilyFrame(preserved, "connected", 5*time.Second))
			require.NoError(t, awaitFamilyFrame(target, "connected", 5*time.Second))

			hook, hookedDB := stmthook.Open(t)
			hook.Arm([]string{`UPDATE login_family_revocations SET sealed_at=clock_timestamp()`,
				"SELECT family_id FROM login_families\n WHERE auth_operation_id=$1"}, nil, stmthook.ErrInjected)
			hook.ArmCommit(committed, stmthook.ErrInjected)
			handler := sessions.NewHandler(hookedDB, r.h.redis, logger.NewWithWriter(io.Discard), r.h.hub, nil)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodDelete, "/sessions/"+targetSID.String(),
				strings.NewReader(`{"password":"`+revocationFencePassword+`"}`))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Params = gin.Params{{Key: "id", Value: targetSID.String()}}
			c.Set("user_id", r.user.String())
			c.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{"user_id": r.user.String(), "sid": r.sid.String(), "cred_epoch": "", "exp": float64(r.expiry.Unix())})
			c.Request = c.Request.WithContext(loginfamily.WithAccessIdentity(c.Request.Context(),
				loginfamily.AccessIdentity{UserID: r.user, SID: r.sid, AccessExpiresAt: r.expiry}))
			handler.RevokeSession(c)
			require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
			seen, hookErr := hook.Report()
			require.Equal(t, 2, seen)
			require.NoError(t, hookErr)

			err := awaitFamilyFrame(target, "connection_ready", 2*time.Second)
			require.Error(t, err)
			if timeout, ok := err.(net.Error); ok {
				require.False(t, timeout.Timeout(), "the target transport must actually close")
			}
			require.NoError(t, preserved.WriteJSON(map[string]any{"type": "connection_ready_probe"}))
			require.NoError(t, awaitFamilyFrame(preserved, "connection_ready", 3*time.Second))
			var terminal bool
			require.NoError(t, r.db.QueryRow(`SELECT terminal_at IS NOT NULL FROM login_families WHERE family_id=$1`, targetFamily).Scan(&terminal))
			require.Equal(t, committed, terminal)
		})
	}
}

func TestRevokeAllFencesRealLocalSocketsWhenOwnerLookupFails(t *testing.T) {
	r := loginFamilyRig(t)
	server := startFamilySocketServer(t, r)
	targetFamily, targetSID := addSocketFamily(t, r)
	preserved := dialFamilySocket(t, r, server, r.sid)
	revoked := dialFamilySocket(t, r, server, targetSID)
	require.NoError(t, awaitFamilyFrame(preserved, "connected", 5*time.Second))
	require.NoError(t, awaitFamilyFrame(revoked, "connected", 5*time.Second))

	hook, hookedDB := stmthook.Open(t)
	var fresh *ws.Conn
	var freshFamily uuid.UUID
	hook.Arm([]string{"SELECT family_id FROM login_families\n WHERE auth_operation_id=$1"}, func() error {
		var sid uuid.UUID
		freshFamily, sid = addSocketFamily(t, r)
		fresh = dialFamilySocket(t, r, server, sid)
		return awaitFamilyFrame(fresh, "connected", 5*time.Second)
	}, stmthook.ErrInjected)
	handler := sessions.NewHandler(hookedDB, r.h.redis, logger.NewWithWriter(io.Discard), r.h.hub, nil)
	w := httptest.NewRecorder()
	handler.RevokeAllSessions(bulkRevocationContext(r, w))
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	seen, hookErr := hook.Report()
	require.Equal(t, 1, seen)
	require.NoError(t, hookErr)

	_ = revoked.WriteJSON(map[string]any{"type": "connection_ready_probe"})
	err := awaitFamilyFrame(revoked, "connection_ready", 2*time.Second)
	require.Error(t, err)
	if timeout, ok := err.(net.Error); ok {
		require.False(t, timeout.Timeout(), "revoked transport must actually close")
	}
	for _, peer := range []*ws.Conn{preserved, fresh} {
		require.NoError(t, peer.WriteJSON(map[string]any{"type": "connection_ready_probe"}))
		require.NoError(t, awaitFamilyFrame(peer, "connection_ready", 3*time.Second))
	}
	var targetTerminal, freshTerminal bool
	require.NoError(t, r.db.QueryRow(`SELECT terminal_at IS NOT NULL FROM login_families WHERE family_id=$1`, targetFamily).Scan(&targetTerminal))
	require.NoError(t, r.db.QueryRow(`SELECT terminal_at IS NOT NULL FROM login_families WHERE family_id=$1`, freshFamily).Scan(&freshTerminal))
	require.True(t, targetTerminal)
	require.False(t, freshTerminal)
}

func TestRevokeAllFencesEveryCapturedFamilyBeforeClosingAnyTransport(t *testing.T) {
	r := loginFamilyRig(t)
	hash, err := auth.HashPassword(revocationFencePassword)
	require.NoError(t, err)
	_, err = r.db.Exec(`UPDATE users SET password_hash=$2 WHERE id=$1`, r.user, hash)
	require.NoError(t, err)
	firstFamily, _ := addSocketFamily(t, r)
	secondFamily, _ := addSocketFamily(t, r)
	firstConn, _, firstTransport := familyInstrumentedSocket(t)
	secondConn, _, secondTransport := familyInstrumentedSocket(t)
	first := familyPublishedClient(r.user, firstFamily, firstConn)
	second := familyPublishedClient(r.user, secondFamily, secondConn)
	hub := familyHubWithPublished(t, first, second)

	entered, release := make(chan struct{}), make(chan struct{})
	var firstClose, releaseOnce sync.Once
	blockClose := func() { firstClose.Do(func() { close(entered); <-release }) }
	firstTransport.beforeClose = blockClose
	secondTransport.beforeClose = blockClose
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	hook, hookedDB := stmthook.Open(t)
	hook.Arm([]string{"SELECT family_id FROM login_families\n WHERE auth_operation_id=$1"}, nil, stmthook.ErrInjected)
	handler := sessions.NewHandler(hookedDB, r.h.redis, logger.NewWithWriter(io.Discard), hub, nil)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.RevokeAllSessions(bulkRevocationContext(r, w))
	}()
	t.Cleanup(func() {
		unblock()
		familyAwait(t, done, "bulk revoke after transport close")
	})
	familyAwait(t, entered, "first transport close")
	require.True(t, first.securityTerminal.Load(), "the first captured family must be fenced before any transport close")
	require.True(t, second.securityTerminal.Load(), "a stalled close must not leave another captured family productive")
	unblock()
	familyAwait(t, done, "bulk revoke response")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	seen, hookErr := hook.Report()
	require.Equal(t, 1, seen)
	require.NoError(t, hookErr)
}

type blockedFamilySnapshot struct {
	hub      *Hub
	captured chan struct{}
	release  chan struct{}
}

func (s *blockedFamilySnapshot) SnapshotUserFamiliesContext(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	families, err := s.hub.SnapshotUserFamiliesContext(ctx, userID)
	if err != nil {
		return nil, err
	}
	close(s.captured)
	select {
	case <-s.release:
		return families, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *blockedFamilySnapshot) DisconnectFamilyContext(ctx context.Context, userID, familyID uuid.UUID) error {
	return s.hub.DisconnectFamilyContext(ctx, userID, familyID)
}

func (s *blockedFamilySnapshot) DisconnectFamiliesContext(ctx context.Context, userID uuid.UUID, families []uuid.UUID) error {
	return s.hub.DisconnectFamiliesContext(ctx, userID, families)
}

func TestRevokeAllRejectsSocketRegisteredAfterLocalSnapshot(t *testing.T) {
	r := loginFamilyRig(t)
	server := startFamilySocketServer(t, r)
	targetFamily, targetSID := addSocketFamily(t, r)
	preserved := dialFamilySocket(t, r, server, r.sid)
	require.NoError(t, awaitFamilyFrame(preserved, "connected", 5*time.Second))

	hook, hookedDB := stmthook.Open(t)
	hook.Arm([]string{"SELECT family_id FROM login_families\n WHERE auth_operation_id=$1"}, nil, stmthook.ErrInjected)
	snapshot := &blockedFamilySnapshot{hub: r.h.hub, captured: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(snapshot.release) }) }
	t.Cleanup(release)
	handler := sessions.NewHandler(hookedDB, r.h.redis, logger.NewWithWriter(io.Discard), snapshot, nil)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.RevokeAllSessions(bulkRevocationContext(r, w))
	}()
	familyAwait(t, snapshot.captured, "local frontier captured before commit")

	racing := dialFamilySocket(t, r, server, targetSID)
	require.Eventually(t, func() bool {
		r.h.hub.mu.RLock()
		defer r.h.hub.mu.RUnlock()
		for _, client := range r.h.hub.clients {
			if client.UserID == r.user && client.FamilyID == targetFamily {
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond, "racing socket must register after the snapshot")
	require.Eventually(t, func() bool {
		var waiting bool
		err := r.db.QueryRow(`SELECT EXISTS (
 SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'
 AND query='SELECT COALESCE(credential_epoch, '''') FROM users WHERE id = $1 FOR SHARE')`).Scan(&waiting)
		return err == nil && waiting
	}, 2*time.Second, 10*time.Millisecond, "post-registration recheck must wait for the captured transaction")
	release()
	familyAwait(t, done, "bulk revoke response")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	seen, hookErr := hook.Report()
	require.Equal(t, 1, seen)
	require.NoError(t, hookErr)
	err := awaitFamilyFrame(racing, "connected", 3*time.Second)
	require.Error(t, err, "a post-snapshot registration cannot admit a revoked family")
	if timeout, ok := err.(net.Error); ok {
		require.False(t, timeout.Timeout(), "the racing transport must actually close")
	}
	require.NoError(t, preserved.WriteJSON(map[string]any{"type": "connection_ready_probe"}))
	require.NoError(t, awaitFamilyFrame(preserved, "connection_ready", 3*time.Second))
}
