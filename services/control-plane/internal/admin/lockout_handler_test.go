package admin_test

// Handler-level tests for #221: PasswordLogin and EnrollBegin count each guess
// BEFORE its password is checked, repeated locks escalate across expiries, and a
// credential-save fault at EnrollFinish is a logged 500.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// guessFn sends one wrong-password guess to a handler from a chosen client IP
// and returns the status code.
type guessFn func(engine *gin.Engine, ip, username string) int

func loginGuess(engine *gin.Engine, ip, username string) int {
	return postFrom(engine, "/admin/api/v1/auth/password", ip, map[string]string{
		"username": username, "password": "wrong-password",
	})
}

func enrollGuess(engine *gin.Engine, ip, username string) int {
	return postFrom(engine, "/admin/api/v1/enroll/begin", ip, map[string]string{
		"username": username, "password": "wrong-password", "token": badEnrollTok,
	})
}

// postFrom posts a JSON body from a chosen client IP.
func postFrom(engine *gin.Engine, path, ip string, body any) int {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = ip + ":1234"
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec.Code
}

// burst fires n requests at once and returns the status codes seen.
func burst(n int, send func(i int) int) map[int]int {
	codes := make(map[int]int)
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			code := send(i)
			mu.Lock()
			codes[code]++
			mu.Unlock()
		}(i)
	}
	close(start)
	wg.Wait()
	return codes
}

// burstSize is large enough that, without counting before the check, far more
// than the threshold's worth of guesses would pass the lock check together.
const burstSize = 12

var guessHandlers = []struct {
	name  string
	guess guessFn
}{
	{"PasswordLogin", loginGuess},
	{"EnrollBegin", enrollGuess},
}

// N concurrent wrong passwords for ONE account from N distinct IPs get at most
// five password verifies (a verified wrong password answers 401), then 429.
func TestBurst_AccountAxis_AtMostFiveVerifies(t *testing.T) { // regression for #221
	for _, h := range guessHandlers {
		t.Run(h.name, func(t *testing.T) {
			_, rdb, engine := setupEnrollLockout(t)
			username := uniqueAdminUsername("burst")

			codes := burst(burstSize, func(i int) int {
				return h.guess(engine, fmt.Sprintf("203.0.113.%d", i+1), username)
			})

			assert.Equal(t, 5, codes[http.StatusUnauthorized], "exactly five guesses may reach the password verify: %v", codes)
			assert.Equal(t, burstSize-5, codes[http.StatusTooManyRequests], "the rest are refused before any verify: %v", codes)
			assert.True(t, keyExists(t, rdb, acctLockKey(username)))
		})
	}
}

// N concurrent wrong passwords from ONE IP, spraying N different usernames, get
// at most five verifies: the IP axis bounds what no single account's count can.
func TestBurst_IPAxis_AtMostFiveVerifies(t *testing.T) { // regression for #221
	for _, h := range guessHandlers {
		t.Run(h.name, func(t *testing.T) {
			_, rdb, engine := setupEnrollLockout(t)

			codes := burst(burstSize, func(i int) int {
				return h.guess(engine, "198.51.100.77", uniqueAdminUsername(fmt.Sprintf("spray%d", i)))
			})

			assert.Equal(t, 5, codes[http.StatusUnauthorized], "exactly five guesses may reach the password verify: %v", codes)
			assert.Equal(t, burstSize-5, codes[http.StatusTooManyRequests], "the rest are refused before any verify: %v", codes)
			assert.True(t, keyExists(t, rdb, ipLockKey("198.51.100.77")))
		})
	}
}

// Unknown usernames lock exactly like known ones: the count runs before the
// lookup, so the lockout adds no enumeration oracle.
func TestLockout_UnknownUserLocksLikeKnownUser(t *testing.T) { // regression for #221
	for _, h := range guessHandlers {
		t.Run(h.name, func(t *testing.T) {
			db, rdb, engine := setupEnrollLockout(t)
			_, known, _ := enrolledAdmin(t, db, rdb)
			unknown := uniqueAdminUsername("ghost")

			for i := 0; i < 5; i++ {
				kc := h.guess(engine, fmt.Sprintf("10.2.0.%d", i+1), known)
				uc := h.guess(engine, fmt.Sprintf("10.3.0.%d", i+1), unknown)
				require.Equal(t, kc, uc)
			}
			assert.Equal(t, http.StatusTooManyRequests, h.guess(engine, "10.2.9.9", known))
			assert.Equal(t, http.StatusTooManyRequests, h.guess(engine, "10.3.9.9", unknown))
			assert.True(t, keyExists(t, rdb, acctLockKey(unknown)))
		})
	}
}

// After each lock expires the next five failures lock for twice as long, up to
// the 60-minute cap. Expiry is simulated by deleting the lock key (the level key
// is what survives it), never by sleeping.
func TestLockout_EscalatesAcrossExpiries_ThroughHandlers(t *testing.T) { // regression for #221
	for _, h := range guessHandlers {
		t.Run(h.name, func(t *testing.T) {
			_, rdb, engine := setupEnrollLockout(t)
			ctx := context.Background()
			username := uniqueAdminUsername("esc")

			for round, minutes := range []time.Duration{1, 2, 4, 8, 16, 32, 60, 60} {
				for i := 0; i < 5; i++ {
					// A fresh IP per guess keeps the IP axis out of the way.
					code := h.guess(engine, fmt.Sprintf("10.%d.%d.1", round+10, i+1), username)
					require.Equal(t, http.StatusUnauthorized, code, "round %d guess %d", round, i)
				}
				assertWindow(t, rdb, acctLockKey(username), minutes*time.Minute, fmt.Sprintf("round %d", round))
				require.Equal(t, http.StatusTooManyRequests, h.guess(engine, "10.99.99.99", username))
				require.NoError(t, rdb.Del(ctx, acctLockKey(username)).Err()) // the lock expires
			}
		})
	}
}

// A lockout store that cannot be reached refuses before any password verify: a
// wrong password answers 500, not 401, and a correct one does not sign in.
func TestPasswordLogin_LockoutStoreDown_FailsClosed(t *testing.T) { // regression for #221
	db, rdb, _ := setupEnrollLockout(t)
	_, username, _ := enrolledAdmin(t, db, rdb)
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialerRetries: 1})
	t.Cleanup(func() { _ = dead.Close() })
	engine := enrollLockoutEngine(t, db, dead)

	code, body := passwordLogin(engine, username, "wrong-password")
	assert.Equal(t, http.StatusInternalServerError, code, body)
	assert.JSONEq(t, `{"error":"internal error"}`, body)
	code, _ = passwordLogin(engine, username, adminTestPassword)
	assert.Equal(t, http.StatusInternalServerError, code)
}

// signIn runs the two steps of an admin sign-in; the password step counts as one
// guess, and the WebAuthn step clears the count.
func signIn(t *testing.T, engine *gin.Engine, ip, username string, va *virtualAuthenticator) {
	t.Helper()
	pw, _ := json.Marshal(map[string]string{"username": username, "password": adminTestPassword})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/v1/auth/password", bytes.NewReader(pw))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = ip + ":1234"
	pwRec := httptest.NewRecorder()
	engine.ServeHTTP(pwRec, req)
	require.Equal(t, http.StatusOK, pwRec.Code, pwRec.Body.String())
	var resp struct {
		Handle    string                        `json:"handle"`
		PublicKey *protocol.CredentialAssertion `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(pwRec.Body.Bytes(), &resp))

	va.signCount++
	assertion := va.assertionResponse(t, testAdminRPID, resp.PublicKey.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV, false)
	wb, _ := json.Marshal(map[string]any{"handle": resp.Handle, "assertion": json.RawMessage(assertion)})
	wreq := httptest.NewRequest(http.MethodPost, "/admin/api/v1/auth/webauthn", bytes.NewReader(wb))
	wreq.Header.Set("Content-Type", "application/json")
	wreq.RemoteAddr = ip + ":1234"
	wrec := httptest.NewRecorder()
	engine.ServeHTTP(wrec, wreq)
	require.Equal(t, http.StatusOK, wrec.Code, wrec.Body.String())
}

// A correct password plus hardware key signs in and clears the attempts and the
// lock, but the escalation level is KEPT: a re-attack right after a sign-in
// continues from the level it had reached instead of starting over at one minute.
func TestSignIn_ClearsAttemptsAndLock_KeepsLevel(t *testing.T) { // regression for #221
	db, rdb, _ := setupEnrollLockout(t)
	_, username, va := enrolledAdmin(t, db, rdb)
	engine := adminAuthEngineWithEnroll(t, db, rdb)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		require.Equal(t, http.StatusUnauthorized, loginGuess(engine, fmt.Sprintf("10.4.0.%d", i+1), username))
	}
	require.True(t, keyExists(t, rdb, acctLockKey(username)))
	require.NoError(t, rdb.Del(ctx, acctLockKey(username)).Err()) // the lock expires
	for i := 0; i < 2; i++ {
		require.Equal(t, http.StatusUnauthorized, loginGuess(engine, fmt.Sprintf("10.4.1.%d", i+1), username))
	}

	signIn(t, engine, "10.4.9.9", username, va)

	assert.Zero(t, failCount(t, rdb, acctKey(username)), "a completed sign-in clears the attempts")
	assert.False(t, keyExists(t, rdb, acctLockKey(username)))
	assert.Equal(t, int64(1), failCount(t, rdb, acctLevelKey(username)), "the level survives the sign-in")

	for i := 0; i < 5; i++ {
		require.Equal(t, http.StatusUnauthorized, loginGuess(engine, fmt.Sprintf("10.4.2.%d", i+1), username))
	}
	assertWindow(t, rdb, acctLockKey(username), 2*time.Minute, "a re-attack after a sign-in escalates at once")
}

// adminAuthEngineWithEnroll wires every sign-in and enrollment route on one
// handler, as routes.go does.
func adminAuthEngineWithEnroll(t *testing.T, db *sql.DB, rdb *redis.Client) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h, err := admin.NewHandler(db, rdb, logger.NewWithWriter(&bytes.Buffer{}), authHandlerCfg())
	require.NoError(t, err)
	r := gin.New()
	r.POST("/admin/api/v1/auth/password", h.PasswordLogin)
	r.POST("/admin/api/v1/auth/webauthn", h.WebAuthnLogin)
	r.POST("/admin/api/v1/enroll/begin", h.EnrollBegin)
	r.POST("/admin/api/v1/enroll/finish", h.EnrollFinish)
	return r
}

// The WebAuthn step's lock re-check is a pure read, so a held handle cannot lock
// itself: a sign-in whose password step was the fifth counted guess still finishes.
func TestWebAuthnLogin_FifthGuessHandleStillRedeems(t *testing.T) { // regression for #221
	db, rdb, _ := setupEnrollLockout(t)
	_, username, va := enrolledAdmin(t, db, rdb)
	engine := adminAuthEngineWithEnroll(t, db, rdb)

	for i := 0; i < 4; i++ {
		require.Equal(t, http.StatusUnauthorized, loginGuess(engine, fmt.Sprintf("10.5.0.%d", i+1), username))
	}
	signIn(t, engine, "10.5.9.9", username, va) // password step = fifth counted guess

	assert.False(t, keyExists(t, rdb, acctLockKey(username)))
}

// A credential-save fault at EnrollFinish is a logged 500 with no failure audit
// row; an attestation that is rejected stays a Warn, an audit row and a 401.
func TestEnrollFinish_CredentialPersistFault_Is500(t *testing.T) { // regression for #221
	db, rdb, _ := setupEnrollLockout(t)
	var logs bytes.Buffer
	engine := enrollEngineWithLog(t, db, rdb, &logs)
	_, username, token := pendingAdminWithToken(t, db, rdb)
	handle, creation := enrollBeginOK(t, engine, username, token)

	failCredentialInserts(t, db)

	va := newVirtualAuthenticator(t, uuid.MustParse(testAllowedAAGID))
	att := va.attestationResponse(t, testAdminRPID, creation.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV)
	rec := postJSON(engine, "/admin/api/v1/enroll/finish", map[string]any{ // #nosec G101 -- "credential_name" is a key-name label, not a secret
		"handle": handle, "attestation": json.RawMessage(att), "credential_name": "primary",
	})

	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"error":"internal error"}`, rec.Body.String())
	assert.Contains(t, logs.String(), "level=ERROR", "a storage fault must be logged at Error")
	assert.Contains(t, logs.String(), "admin credential persist failed")
	assert.Zero(t, enrollFailureAuditRows(t, db, username), "a storage fault is not a failed enrollment")
	got, err := admin.NewAdminRepo(db).GetByUsername(context.Background(), username)
	require.NoError(t, err)
	assert.Equal(t, admin.StatusPending, got.Status, "the admin must stay pending")
}

func TestEnrollFinish_AttestationRejection_StaysWarn401(t *testing.T) { // regression for #221
	db, rdb, _ := setupEnrollLockout(t)
	var logs bytes.Buffer
	engine := enrollEngineWithLog(t, db, rdb, &logs)
	_, username, token := pendingAdminWithToken(t, db, rdb)
	handle, creation := enrollBeginOK(t, engine, username, token)

	// A key whose model is not on the allow-list is rejected, never persisted.
	va := newVirtualAuthenticator(t, uuid.New())
	att := va.attestationResponse(t, testAdminRPID, creation.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV)
	rec := postJSON(engine, "/admin/api/v1/enroll/finish", map[string]any{ // #nosec G101 -- "credential_name" is a key-name label, not a secret
		"handle": handle, "attestation": json.RawMessage(att), "credential_name": "primary",
	})

	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	assert.Contains(t, logs.String(), "level=WARN")
	assert.NotContains(t, logs.String(), "level=ERROR")
	assert.Equal(t, 1, enrollFailureAuditRows(t, db, username), "a rejected key keeps its failure audit row")
}

func enrollEngineWithLog(t *testing.T, db *sql.DB, rdb *redis.Client, logs *bytes.Buffer) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h, err := admin.NewHandler(db, rdb, logger.NewWithWriter(logs), authHandlerCfg())
	require.NoError(t, err)
	r := gin.New()
	r.POST("/admin/api/v1/enroll/begin", h.EnrollBegin)
	r.POST("/admin/api/v1/enroll/finish", h.EnrollFinish)
	return r
}

func enrollBeginOK(t *testing.T, engine *gin.Engine, username, token string) (string, *protocol.CredentialCreation) {
	t.Helper()
	rec := postJSON(engine, "/admin/api/v1/enroll/begin", map[string]string{
		"username": username, "password": adminTestPassword, "token": token,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var begin struct {
		Handle    string                       `json:"handle"`
		PublicKey *protocol.CredentialCreation `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &begin))
	return begin.Handle, begin.PublicKey
}

// failCredentialInserts makes every insert into admin_webauthn_credentials fail,
// the way a database fault would, until the test ends.
func failCredentialInserts(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	fn := "cv_test_fail_cred_insert_" + strings.ReplaceAll(uuid.NewString(), "-", "_")
	_, err := db.ExecContext(ctx, fmt.Sprintf(
		`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected credential store fault'; END $$`, fn))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, fmt.Sprintf(
		`CREATE TRIGGER %s BEFORE INSERT ON admin_webauthn_credentials FOR EACH ROW EXECUTE FUNCTION %s()`, fn, fn))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON admin_webauthn_credentials`, fn))
		_, _ = db.ExecContext(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))
	})
}

func enrollFailureAuditRows(t *testing.T, db *sql.DB, username string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM admin_audit_log WHERE actor = $1 AND event_type = $2 AND result = $3`,
		username, admin.EventEnrollComplete, admin.AuditFailure).Scan(&n))
	return n
}
