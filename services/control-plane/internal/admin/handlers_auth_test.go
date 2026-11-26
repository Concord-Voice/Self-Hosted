package admin_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
)

// adminTestPassword is a strong throwaway password meeting ValidatePasswordStrength.
const adminTestPassword = "Adm1n-Str0ng-P@ssw0rd!" // #nosec G101 -- test fixture, not a real credential // pragma: allowlist secret

// authHandlerCfg returns an admin config wired to the virtual-authenticator RP.
func authHandlerCfg() *config.Config {
	return &config.Config{
		AdminWebAuthnRPID:           testAdminRPID,
		AdminWebAuthnRPOrigins:      []string{testAdminOrigin},
		AdminWebAuthnAllowedAAGUIDs: []string{testAllowedAAGID},
	}
}

// enrolledAdmin creates an active admin with one registered virtual-authenticator
// credential and returns the admin id, username, and the virtual authenticator
// (whose key is needed to sign login assertions).
func enrolledAdmin(t *testing.T, db *sql.DB, _ *redis.Client) (adminID, username string, va *virtualAuthenticator) {
	t.Helper()
	ctx := context.Background()
	repo := admin.NewAdminRepo(db)

	svc, err := admin.NewAdminWebAuthn(authHandlerCfg())
	require.NoError(t, err)

	hash, err := auth.HashPassword(adminTestPassword)
	require.NoError(t, err)

	username = uniqueAdminUsername("auth")
	created, err := repo.CreatePending(ctx, username, hash)
	require.NoError(t, err)
	registerAdminCleanup(t, db, created.ID)

	va = newVirtualAuthenticator(t, uuid.MustParse(testAllowedAAGID))
	user := adminWebAuthnUser()
	creation, session := beginAdminRegistration(t, svc, user)
	regBody := va.attestationResponse(t, testAdminRPID, creation.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV)

	_, err = admin.FinishAdminRegistration(ctx, svc, repo, admin.AdminRegistrationInput{
		User: user, Session: *session, Request: httpReq(regBody),
		AdminID: created.ID, CredentialGeneration: created.CredentialGeneration,
		AllowedAAGUIDs: []string{testAllowedAAGID}, CredentialName: "primary",
	})
	require.NoError(t, err)

	return created.ID, username, va
}

// postJSON issues an in-process POST with a JSON body and returns the recorder.
func postJSON(engine *gin.Engine, path string, body any) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(rec, req)
	return rec
}

// adminAuthEngine wires PasswordLogin / WebAuthnLogin / Logout on a bare engine.
func adminAuthEngine(t *testing.T, db *sql.DB, rdb *redis.Client) *gin.Engine {
	return adminAuthEngineWithAgent(t, db, rdb, "")
}

func adminAuthEngineWithAgent(t *testing.T, db *sql.DB, rdb *redis.Client, sock string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h, err := admin.NewHandler(db, rdb, logger.NewWithWriter(&bytes.Buffer{}), authHandlerCfg())
	require.NoError(t, err)
	if sock != "" {
		admin.UseServerAgentForTest(h, sock)
	}
	// These tests model the agent's answers, not the action-credential
	// projection: present it as absent so login behaves as before purposes.
	admin.UseActionCredentialLoaderForTest(h, func(string) (map[string]struct{}, error) {
		return nil, os.ErrNotExist
	})
	r := gin.New()
	r.POST("/admin/api/v1/auth/password", h.PasswordLogin)
	r.POST("/admin/api/v1/auth/webauthn", h.WebAuthnLogin)
	r.POST("/admin/api/v1/auth/logout", h.Logout)
	return r
}

type importedAssertionCall struct {
	Path        string          `json:"-"`
	Method      string          `json:"-"`
	AdminID     string          `json:"admin_id"`
	Assertion   json.RawMessage `json:"assertion"`
	SessionData json.RawMessage `json:"session_data"`
}

func importedAssertionAgent(t *testing.T, status int) (string, func() []importedAssertionCall) {
	t.Helper()
	dir, err := os.MkdirTemp("", "agent")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	sock := filepath.Join(dir, "agent.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	var (
		mu    sync.Mutex
		calls []importedAssertionCall
	)
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call importedAssertionCall
		call.Path = r.URL.Path
		call.Method = r.Method
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			http.Error(w, "invalid", http.StatusBadRequest)
			return
		}
		mu.Lock()
		calls = append(calls, call)
		mu.Unlock()
		if status == http.StatusNotFound {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"authorizer_not_imported"}`))
			return
		}
		w.WriteHeader(status)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { require.NoError(t, srv.Close()) })
	return sock, func() []importedAssertionCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]importedAssertionCall(nil), calls...)
	}
}

func TestPasswordLogin_HappyPath_ReturnsChallengeNoCookie(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	_, username, _ := enrolledAdmin(t, db, rdb)
	engine := adminAuthEngine(t, db, rdb)

	rec := postJSON(engine, "/admin/api/v1/auth/password", map[string]string{
		"username": username,
		"password": adminTestPassword,
	})

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Handle    string                        `json:"handle"`
		PublicKey *protocol.CredentialAssertion `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.Handle, "password step must return a challenge handle")
	require.NotNil(t, resp.PublicKey, "password step must return assertion options")

	// CRITICAL: the password step must NEVER set a session cookie.
	assert.Empty(t, rec.Header().Get("Set-Cookie"), "password alone must not mint a session")
}

func TestPasswordLogin_WrongPassword_GenericUnauthorized(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	_, username, _ := enrolledAdmin(t, db, rdb)
	engine := adminAuthEngine(t, db, rdb)

	rec := postJSON(engine, "/admin/api/v1/auth/password", map[string]string{
		"username": username,
		"password": "wrong-password",
	})
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Empty(t, rec.Header().Get("Set-Cookie"))
}

// TestPasswordLogin_UnknownUser_SameAsWrongPassword asserts username-enumeration
// safety: an unknown username and a known-username-wrong-password produce the
// SAME status code and body shape.
func TestPasswordLogin_UnknownUser_SameAsWrongPassword(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	_, username, _ := enrolledAdmin(t, db, rdb)
	engine := adminAuthEngine(t, db, rdb)

	unknown := postJSON(engine, "/admin/api/v1/auth/password", map[string]string{
		"username": uniqueAdminUsername("ghost"),
		"password": adminTestPassword,
	})
	wrongPw := postJSON(engine, "/admin/api/v1/auth/password", map[string]string{
		"username": username,
		"password": "definitely-wrong",
	})

	assert.Equal(t, http.StatusUnauthorized, unknown.Code)
	assert.Equal(t, http.StatusUnauthorized, wrongPw.Code)
	assert.JSONEq(t, wrongPw.Body.String(), unknown.Body.String(),
		"unknown-user and wrong-password responses must be indistinguishable")
}

// TestWebAuthnLogin_HappyPath_MintsSession drives the full two-step ceremony and
// asserts a session cookie is set only after the WebAuthn step.
func TestWebAuthnLogin_HappyPath_MintsSession(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	adminID, username, va := enrolledAdmin(t, db, rdb)
	engine := adminAuthEngine(t, db, rdb)

	// Step 1: password.
	pwRec := postJSON(engine, "/admin/api/v1/auth/password", map[string]string{
		"username": username,
		"password": adminTestPassword,
	})
	require.Equal(t, http.StatusOK, pwRec.Code, pwRec.Body.String())
	var pwResp struct {
		Handle    string                        `json:"handle"`
		PublicKey *protocol.CredentialAssertion `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(pwRec.Body.Bytes(), &pwResp))

	// Step 2: sign the returned challenge with the virtual authenticator.
	va.signCount++
	assertionBody := va.assertionResponse(t, testAdminRPID, pwResp.PublicKey.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV, false)

	waRec := postJSON(engine, "/admin/api/v1/auth/webauthn", map[string]any{
		"handle":    pwResp.Handle,
		"assertion": json.RawMessage(assertionBody),
	})
	require.Equal(t, http.StatusOK, waRec.Code, waRec.Body.String())

	setCookie := waRec.Header().Get("Set-Cookie")
	require.NotEmpty(t, setCookie, "WebAuthn step must mint a session cookie")
	assert.Contains(t, setCookie, "__Host-cv_admin_sid=")
	assert.Contains(t, setCookie, "HttpOnly")
	sid := sessionIDFromSetCookie(t, setCookie)
	sess, err := admin.NewSessionStore(rdb, nil).Get(context.Background(), sid)
	require.NoError(t, err)
	storedAdmin, err := admin.NewAdminRepo(db).GetByUsername(context.Background(), username)
	require.NoError(t, err)
	assert.Equal(t, storedAdmin.SessionGeneration, sess.SessionGeneration,
		"successful login must bind the administrator's current generation")

	// The advanced authenticator sign count must be persisted (Gitar #1703 fix):
	// clone-detection is defeated if the stored counter never moves.
	var storedCount int64
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT sign_count FROM admin_webauthn_credentials WHERE admin_id = $1`, adminID).Scan(&storedCount))
	assert.Equal(t, int64(va.signCount), storedCount, "login must persist the advanced sign count")
}

func TestWebAuthnLoginImportedCredentialFailsClosedBeforeSessionMint(t *testing.T) {
	for _, tc := range []struct {
		name        string
		agentStatus int
		unavailable bool
		wantStatus  int
	}{
		// The root counter is ahead of the DB counter, so a stale clone assertion
		// that the DB accepted must still be refused by the root trust boundary.
		{name: "root counter ahead", agentStatus: http.StatusForbidden, wantStatus: http.StatusUnauthorized},
		{name: "root unavailable", unavailable: true, wantStatus: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, dbCleanup := testhelpers.SetupTestDB(t)
			t.Cleanup(dbCleanup)
			rdb, rCleanup := testhelpers.SetupTestRedis(t)
			t.Cleanup(rCleanup)
			adminID, username, va := enrolledAdmin(t, db, rdb)
			t.Setenv("HOST_AGENT_AUTHORIZER_PROJECTION", "1")

			var calls func() []importedAssertionCall
			sock := filepath.Join(t.TempDir(), "missing-agent.sock")
			if !tc.unavailable {
				sock, calls = importedAssertionAgent(t, tc.agentStatus)
			}
			engine := adminAuthEngineWithAgent(t, db, rdb, sock)
			pwRec := postJSON(engine, "/admin/api/v1/auth/password", map[string]string{
				"username": username,
				"password": adminTestPassword,
			})
			require.Equal(t, http.StatusOK, pwRec.Code, pwRec.Body.String())
			var pwResp struct {
				Handle    string                        `json:"handle"`
				PublicKey *protocol.CredentialAssertion `json:"publicKey"`
			}
			require.NoError(t, json.Unmarshal(pwRec.Body.Bytes(), &pwResp))
			va.signCount++
			assertionBody := va.assertionResponse(t, testAdminRPID, pwResp.PublicKey.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV, false)

			waRec := postJSON(engine, "/admin/api/v1/auth/webauthn", map[string]any{
				"handle":    pwResp.Handle,
				"assertion": json.RawMessage(assertionBody),
			})
			require.Equal(t, tc.wantStatus, waRec.Code, waRec.Body.String())
			assert.Empty(t, waRec.Header().Get("Set-Cookie"), "agent-gated login must not mint a session")
			sessions, err := rdb.Keys(context.Background(), "admin_session:*").Result()
			require.NoError(t, err)
			assert.Empty(t, sessions)
			if tc.unavailable {
				return
			}
			got := calls()
			require.Len(t, got, 1)
			assert.Equal(t, http.MethodPost, got[0].Method)
			assert.Equal(t, "/v1/authorizers/login-assertion", got[0].Path)
			assert.Equal(t, adminID, got[0].AdminID)
			assert.JSONEq(t, string(assertionBody), string(got[0].Assertion))
			assert.True(t, json.Valid(got[0].SessionData), "agent must receive the CP session data")
		})
	}
}

func TestWebAuthnLoginUnimportedCredentialKeepsDBOnlyPath(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)
	adminID, username, va := enrolledAdmin(t, db, rdb)
	t.Setenv("HOST_AGENT_AUTHORIZER_PROJECTION", "1")
	sock, calls := importedAssertionAgent(t, http.StatusNotFound)
	engine := adminAuthEngineWithAgent(t, db, rdb, sock)
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
	require.Equal(t, http.StatusOK, waRec.Code, waRec.Body.String())
	require.NotEmpty(t, waRec.Header().Get("Set-Cookie"))
	require.Len(t, calls(), 1)
	var storedCount int64
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT sign_count FROM admin_webauthn_credentials WHERE admin_id = $1`, adminID).Scan(&storedCount))
	assert.Equal(t, int64(va.signCount), storedCount)
}

func TestWebAuthnLoginImportedTombstonedCredentialKeepsRootCounterGate(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)
	adminID, username, va := enrolledAdmin(t, db, rdb)
	t.Setenv("HOST_AGENT_AUTHORIZER_PROJECTION", "1")
	// A 200 models a valid assertion for a retained, revoked root record: it no
	// longer authorizes host writes, but its durable counter still gates login.
	sock, calls := importedAssertionAgent(t, http.StatusOK)
	engine := adminAuthEngineWithAgent(t, db, rdb, sock)
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
	require.Equal(t, http.StatusOK, waRec.Code, waRec.Body.String())
	require.NotEmpty(t, waRec.Header().Get("Set-Cookie"))
	got := calls()
	require.Len(t, got, 1)
	assert.Equal(t, adminID, got[0].AdminID)
	assert.JSONEq(t, string(assertionBody), string(got[0].Assertion))
	var storedCount int64
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT sign_count FROM admin_webauthn_credentials WHERE admin_id = $1`, adminID).Scan(&storedCount))
	assert.Equal(t, int64(va.signCount), storedCount)
}

func TestWebAuthnLogin_CloneWarningRejectsWithoutSessionOrCounterUpdate(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	adminID, username, va := enrolledAdmin(t, db, rdb)
	engine := adminAuthEngine(t, db, rdb)

	pwRec := postJSON(engine, "/admin/api/v1/auth/password", map[string]string{
		"username": username,
		"password": adminTestPassword,
	})
	require.Equal(t, http.StatusOK, pwRec.Code, pwRec.Body.String())
	var pwResp struct {
		Handle    string                        `json:"handle"`
		PublicKey *protocol.CredentialAssertion `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(pwRec.Body.Bytes(), &pwResp))

	// Registration persisted signCount 1. Reusing it yields a valid assertion
	// with a clone warning from go-webauthn, which must never mint a session.
	assertionBody := va.assertionResponse(t, testAdminRPID, pwResp.PublicKey.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV, false)
	successesBefore := auditRows(t, db, username, admin.EventLoginSuccess, admin.AuditSuccess)
	failuresBefore := auditRows(t, db, username, admin.EventLoginFailure, admin.AuditFailure)

	waRec := postJSON(engine, "/admin/api/v1/auth/webauthn", map[string]any{
		"handle":    pwResp.Handle,
		"assertion": json.RawMessage(assertionBody),
	})
	require.Equal(t, http.StatusUnauthorized, waRec.Code, waRec.Body.String())
	assert.Empty(t, waRec.Header().Get("Set-Cookie"))
	assert.Equal(t, successesBefore, auditRows(t, db, username, admin.EventLoginSuccess, admin.AuditSuccess))
	assert.Equal(t, failuresBefore+1, auditRows(t, db, username, admin.EventLoginFailure, admin.AuditFailure))

	var storedCount int64
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT sign_count FROM admin_webauthn_credentials WHERE admin_id = $1`, adminID).Scan(&storedCount))
	assert.Equal(t, int64(va.signCount), storedCount)
	sessions, err := rdb.Keys(context.Background(), "admin_session:*").Result()
	require.NoError(t, err)
	assert.Empty(t, sessions)
}

// TestWebAuthnLogin_MintsSessionWithGenerationCurrentAtFinalization proves a
// password challenge cannot carry an obsolete session generation through the
// WebAuthn step. Finalization must read it while holding the admin row lock.
func TestWebAuthnLogin_MintsSessionWithGenerationCurrentAtFinalization(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	adminID, username, va := enrolledAdmin(t, db, rdb)
	engine := adminAuthEngine(t, db, rdb)
	ctx := context.Background()

	pwRec := postJSON(engine, "/admin/api/v1/auth/password", map[string]string{"username": username, "password": adminTestPassword})
	require.Equal(t, http.StatusOK, pwRec.Code, pwRec.Body.String())
	var pwResp struct {
		Handle    string                        `json:"handle"`
		PublicKey *protocol.CredentialAssertion `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(pwRec.Body.Bytes(), &pwResp))

	_, err := db.ExecContext(ctx,
		`UPDATE admin_users SET session_generation = session_generation + 1 WHERE id = $1`, adminID)
	require.NoError(t, err)
	current, err := admin.NewAdminRepo(db).GetByUsername(ctx, username)
	require.NoError(t, err)

	va.signCount++
	assertionBody := va.assertionResponse(t, testAdminRPID, pwResp.PublicKey.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV, false)
	waRec := postJSON(engine, "/admin/api/v1/auth/webauthn", map[string]any{"handle": pwResp.Handle, "assertion": json.RawMessage(assertionBody)})
	require.Equal(t, http.StatusOK, waRec.Code, waRec.Body.String())

	sid := sessionIDFromSetCookie(t, waRec.Header().Get("Set-Cookie"))
	sess, err := admin.NewSessionStore(rdb, nil).Get(ctx, sid)
	require.NoError(t, err)
	assert.Equal(t, current.SessionGeneration, sess.SessionGeneration,
		"the minted session must use the generation locked at finalization, not the password challenge")
}

// TestWebAuthnLogin_AdminStateChanged_NoSession verifies that a valid in-flight
// ceremony cannot mint a session after the administrator becomes pending.
func TestWebAuthnLogin_AdminStateChanged_NoSession(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	adminID, username, va := enrolledAdmin(t, db, rdb)
	engine := adminAuthEngine(t, db, rdb)
	ctx := context.Background()

	pwRec := postJSON(engine, "/admin/api/v1/auth/password", map[string]string{"username": username, "password": adminTestPassword})
	require.Equal(t, http.StatusOK, pwRec.Code, pwRec.Body.String())
	var pwResp struct {
		Handle    string                        `json:"handle"`
		PublicKey *protocol.CredentialAssertion `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(pwRec.Body.Bytes(), &pwResp))

	var initialCount int64
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT sign_count FROM admin_webauthn_credentials WHERE admin_id = $1`, adminID).Scan(&initialCount))
	require.NoError(t, admin.NewAdminRepo(db).SetStatus(ctx, adminID, admin.StatusPending))

	va.signCount++
	assertionBody := va.assertionResponse(t, testAdminRPID, pwResp.PublicKey.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV, false)
	waRec := postJSON(engine, "/admin/api/v1/auth/webauthn", map[string]any{"handle": pwResp.Handle, "assertion": json.RawMessage(assertionBody)})
	require.Equal(t, http.StatusUnauthorized, waRec.Code, waRec.Body.String())
	assert.Empty(t, waRec.Header().Get("Set-Cookie"), "changed admin state must not mint a session")

	var storedCount int64
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT sign_count FROM admin_webauthn_credentials WHERE admin_id = $1`, adminID).Scan(&storedCount))
	assert.Equal(t, initialCount, storedCount, "rejected login must not persist the assertion counter")
}

// TestWebAuthnLogin_LockedAfterHandleIssued_Returns429 covers the Gitar #1703 fix:
// the WebAuthn step re-checks lockout, so a handle issued before the account was
// locked cannot be redeemed once the lock engages.
func TestWebAuthnLogin_LockedAfterHandleIssued_Returns429(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	_, username, va := enrolledAdmin(t, db, rdb)
	engine := adminAuthEngine(t, db, rdb)

	// Step 1: password step succeeds and issues a valid handle (not yet locked).
	pwRec := postJSON(engine, "/admin/api/v1/auth/password", map[string]string{
		"username": username,
		"password": adminTestPassword,
	})
	require.Equal(t, http.StatusOK, pwRec.Code, pwRec.Body.String())
	var pwResp struct {
		Handle    string                        `json:"handle"`
		PublicKey *protocol.CredentialAssertion `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(pwRec.Body.Bytes(), &pwResp))

	// Lock the account (per-account axis) AFTER the handle was issued.
	forceLock(t, admin.NewLockout(rdb, nil), username, "203.0.113.9")

	// Step 2: the held handle must NOT be redeemable while locked → 429, no session.
	va.signCount++
	assertionBody := va.assertionResponse(t, testAdminRPID, pwResp.PublicKey.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV, false)
	waRec := postJSON(engine, "/admin/api/v1/auth/webauthn", map[string]any{
		"handle":    pwResp.Handle,
		"assertion": json.RawMessage(assertionBody),
	})
	require.Equal(t, http.StatusTooManyRequests, waRec.Code, waRec.Body.String())
	assert.Empty(t, waRec.Header().Get("Set-Cookie"), "locked WebAuthn step must not mint a session")
}

// TestWebAuthnLogin_BadAssertion_NoSession asserts a tampered assertion is
// rejected and no session is minted.
func TestWebAuthnLogin_BadAssertion_NoSession(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	_, username, va := enrolledAdmin(t, db, rdb)
	engine := adminAuthEngine(t, db, rdb)

	// Four counted wrong guesses, so the password step below is the fifth: a bad
	// assertion that counted (or ran the checked-failure lock) would lock the
	// account it is redeeming.
	for i := 0; i < 4; i++ {
		require.Equal(t, http.StatusUnauthorized, loginGuess(engine, fmt.Sprintf("10.8.0.%d", i+1), username))
	}
	pwRec := postJSON(engine, "/admin/api/v1/auth/password", map[string]string{
		"username": username,
		"password": adminTestPassword,
	})
	require.Equal(t, http.StatusOK, pwRec.Code)
	var pwResp struct {
		Handle    string                        `json:"handle"`
		PublicKey *protocol.CredentialAssertion `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(pwRec.Body.Bytes(), &pwResp))
	failuresBefore := auditRows(t, db, username, admin.EventLoginFailure, admin.AuditFailure)

	va.signCount++
	// tamperSig=true corrupts the ECDSA signature.
	assertionBody := va.assertionResponse(t, testAdminRPID, pwResp.PublicKey.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV, true)

	waRec := postJSON(engine, "/admin/api/v1/auth/webauthn", map[string]any{
		"handle":    pwResp.Handle,
		"assertion": json.RawMessage(assertionBody),
	})
	assert.Equal(t, http.StatusUnauthorized, waRec.Code)
	assert.Empty(t, waRec.Header().Get("Set-Cookie"))

	// A rejected assertion is audited but never counted.
	assert.Equal(t, failuresBefore+1, auditRows(t, db, username, admin.EventLoginFailure, admin.AuditFailure),
		"a rejected assertion keeps its failure audit row")
	assert.Equal(t, int64(5), failCount(t, rdb, acctKey(username)), "the assertion adds no guess to the count")
	assert.False(t, keyExists(t, rdb, acctLockKey(username)), "a held handle must not lock the account it is redeeming")
}

// TestWebAuthnLogin_UnknownHandle_Rejected asserts a fabricated/expired handle is
// rejected without touching the authenticator.
func TestWebAuthnLogin_UnknownHandle_Rejected(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	_, _, _ = enrolledAdmin(t, db, rdb)
	engine := adminAuthEngine(t, db, rdb)

	waRec := postJSON(engine, "/admin/api/v1/auth/webauthn", map[string]any{
		"handle":    "0000000000000000000000000000000000000000000000000000000000000000",
		"assertion": json.RawMessage(`{}`),
	})
	assert.Equal(t, http.StatusUnauthorized, waRec.Code)
	assert.Empty(t, waRec.Header().Get("Set-Cookie"))
}

// TestWebAuthnLogin_HandleIsSingleUse asserts the challenge handle cannot be
// replayed after a successful login.
func TestWebAuthnLogin_HandleIsSingleUse(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	_, username, va := enrolledAdmin(t, db, rdb)
	engine := adminAuthEngine(t, db, rdb)

	pwRec := postJSON(engine, "/admin/api/v1/auth/password", map[string]string{
		"username": username,
		"password": adminTestPassword,
	})
	require.Equal(t, http.StatusOK, pwRec.Code)
	var pwResp struct {
		Handle    string                        `json:"handle"`
		PublicKey *protocol.CredentialAssertion `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(pwRec.Body.Bytes(), &pwResp))

	va.signCount++
	assertionBody := va.assertionResponse(t, testAdminRPID, pwResp.PublicKey.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV, false)

	first := postJSON(engine, "/admin/api/v1/auth/webauthn", map[string]any{
		"handle":    pwResp.Handle,
		"assertion": json.RawMessage(assertionBody),
	})
	require.Equal(t, http.StatusOK, first.Code)

	// Replaying the same handle must fail (GETDEL consumed it).
	second := postJSON(engine, "/admin/api/v1/auth/webauthn", map[string]any{
		"handle":    pwResp.Handle,
		"assertion": json.RawMessage(assertionBody),
	})
	assert.Equal(t, http.StatusUnauthorized, second.Code)
}

func TestLogout_RevokesSessionAndClearsCookie(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	_, username, va := enrolledAdmin(t, db, rdb)
	engine := adminAuthEngine(t, db, rdb)

	// Full login to obtain a real session cookie.
	pwRec := postJSON(engine, "/admin/api/v1/auth/password", map[string]string{
		"username": username,
		"password": adminTestPassword,
	})
	require.Equal(t, http.StatusOK, pwRec.Code)
	var pwResp struct {
		Handle    string                        `json:"handle"`
		PublicKey *protocol.CredentialAssertion `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(pwRec.Body.Bytes(), &pwResp))
	va.signCount++
	assertionBody := va.assertionResponse(t, testAdminRPID, pwResp.PublicKey.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV, false)
	waRec := postJSON(engine, "/admin/api/v1/auth/webauthn", map[string]any{
		"handle":    pwResp.Handle,
		"assertion": json.RawMessage(assertionBody),
	})
	require.Equal(t, http.StatusOK, waRec.Code)

	sid := sessionIDFromSetCookie(t, waRec.Header().Get("Set-Cookie"))

	// Logout carrying the session cookie.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/v1/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-cv_admin_sid", Value: sid, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	engine.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Set-Cookie"), "Max-Age=0")

	// The session is gone: AdminAuthRequired now rejects it.
	store := admin.NewSessionStore(rdb, nil)
	_, err := store.Get(context.Background(), sid)
	assert.ErrorIs(t, err, admin.ErrSessionInvalid)
}

func TestLogout_RevokeFailurePreservesSessionAndCookie(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://127.0.0.1:1/postgres?sslmode=disable&connect_timeout=1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	store := admin.NewSessionStore(rdb, nil)
	sid, err := store.Mint(context.Background(), "admin-revoke-retry", 0)
	require.NoError(t, err)
	engine := adminAuthEngine(t, db, rdb)

	redisOptions := *rdb.Options()
	require.NoError(t, rdb.Close())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/v1/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-cv_admin_sid", Value: sid, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	engine.ServeHTTP(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Empty(t, rec.Header().Values("Set-Cookie"), "failed revocation must leave the browser cookie intact")

	verifier := redis.NewClient(&redisOptions)
	t.Cleanup(func() { _ = verifier.Close() })
	_, err = admin.NewSessionStore(verifier, nil).Get(context.Background(), sid)
	require.NoError(t, err, "failed revocation must leave the server-side session retryable")
}

// sessionIDFromSetCookie extracts the __Host-cv_admin_sid value from a Set-Cookie
// header.
func sessionIDFromSetCookie(t *testing.T, setCookie string) string {
	t.Helper()
	require.NotEmpty(t, setCookie)
	header := http.Header{}
	header.Add("Set-Cookie", setCookie)
	resp := http.Response{Header: header}
	for _, ck := range resp.Cookies() {
		if ck.Name == "__Host-cv_admin_sid" {
			return ck.Value
		}
	}
	t.Fatalf("no __Host-cv_admin_sid in Set-Cookie: %q", setCookie)
	return ""
}
