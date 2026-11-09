package admin_test

// Regression tests for #216: the admin enroll step must share the admin
// login lockout (per account + per IP) instead of relying on the per-IP
// rate limit alone.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const (
	lockedBody   = `{"error":"too many attempts, try again later"}`
	badEnrollTok = "0000000000000000000000000000000000000000000000000000000000000000"
)

// enrollLockoutEngine wires login and enroll on one handler, as routes.go does.
func enrollLockoutEngine(t *testing.T, db *sql.DB, rdb *redis.Client) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h, err := admin.NewHandler(db, rdb, logger.NewWithWriter(&bytes.Buffer{}), authHandlerCfg())
	require.NoError(t, err)
	r := gin.New()
	r.POST("/admin/api/v1/auth/password", h.PasswordLogin)
	r.POST("/admin/api/v1/enroll/begin", h.EnrollBegin)
	r.POST("/admin/api/v1/enroll/finish", h.EnrollFinish)
	return r
}

func enrollBegin(engine *gin.Engine, username, password, token string) (int, string) {
	rec := postJSON(engine, "/admin/api/v1/enroll/begin", map[string]string{
		"username": username, "password": password, "token": token,
	})
	return rec.Code, rec.Body.String()
}

// enrollBeginFrom sends EnrollBegin from a chosen client IP.
func enrollBeginFrom(engine *gin.Engine, ip, username, password, token string) (int, string) {
	b, _ := json.Marshal(map[string]string{"username": username, "password": password, "token": token})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/v1/enroll/begin", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = ip + ":1234"
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// defaultTestIP is the client IP httptest.NewRequest assigns.
const defaultTestIP = "192.0.2.1"

func acctKey(username string) string { return "admin_lockout:acct:" + username }

func ipKey(ip string) string {
	sum := sha256.Sum256([]byte(ip))
	return "admin_lockout:ip:" + hex.EncodeToString(sum[:])
}

// failCount reads a lockout counter; an absent key is 0.
func failCount(t *testing.T, rdb *redis.Client, key string) int64 {
	t.Helper()
	n, err := rdb.Get(context.Background(), key).Int64()
	if errors.Is(err, redis.Nil) {
		return 0
	}
	require.NoError(t, err)
	return n
}

func passwordLogin(engine *gin.Engine, username, password string) (int, string) {
	rec := postJSON(engine, "/admin/api/v1/auth/password", map[string]string{
		"username": username, "password": password,
	})
	return rec.Code, rec.Body.String()
}

func tokenStillStored(t *testing.T, rdb *redis.Client, token string) bool {
	t.Helper()
	sum := sha256.Sum256([]byte(token))
	n, err := rdb.Exists(context.Background(), "admin_enroll:"+hex.EncodeToString(sum[:])).Result()
	require.NoError(t, err)
	return n == 1
}

func setupEnrollLockout(t *testing.T) (*sql.DB, *redis.Client, *gin.Engine) {
	t.Helper()
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)
	return db, rdb, enrollLockoutEngine(t, db, rdb)
}

// A locked account gets the login 429 at enroll, and the token is not consumed.
func TestEnrollBegin_LockedByLoginFailures_Returns429(t *testing.T) { // regression for #216
	db, rdb, engine := setupEnrollLockout(t)
	_, username, token := pendingAdminWithToken(t, db, rdb)

	for i := 0; i < 5; i++ {
		code, _ := passwordLogin(engine, username, "wrong-password")
		require.Equal(t, http.StatusUnauthorized, code)
	}

	code, body := enrollBegin(engine, username, adminTestPassword, token)
	assert.Equal(t, http.StatusTooManyRequests, code, body)
	assert.JSONEq(t, lockedBody, body)
	assert.True(t, tokenStillStored(t, rdb, token), "a locked enroll must not reach the token")
}

// Wrong-password enroll attempts on an ACTIVE admin use up that admin's login budget.
func TestEnrollBegin_FailuresLockLogin(t *testing.T) { // regression for #216
	db, rdb, engine := setupEnrollLockout(t)
	_, username, _ := enrolledAdmin(t, db, rdb)

	for i := 0; i < 5; i++ {
		code, _ := enrollBegin(engine, username, "wrong-password", badEnrollTok)
		require.Equal(t, http.StatusUnauthorized, code)
	}

	code, body := passwordLogin(engine, username, adminTestPassword)
	assert.Equal(t, http.StatusTooManyRequests, code, body)
}

// A correct password with a bad token still counts, so the real token is refused once locked.
func TestEnrollBegin_BadTokenWithCorrectPasswordCounts(t *testing.T) { // regression for #216
	db, rdb, engine := setupEnrollLockout(t)
	_, username, token := pendingAdminWithToken(t, db, rdb)

	for i := 0; i < 5; i++ {
		code, _ := enrollBegin(engine, username, adminTestPassword, badEnrollTok)
		require.Equal(t, http.StatusUnauthorized, code)
	}

	code, body := enrollBegin(engine, username, adminTestPassword, token)
	assert.Equal(t, http.StatusTooManyRequests, code, body)
}

// An active admin's correct password and a token bound to that admin is a
// failure, with the same body as an unknown user, and it counts toward the
// lockout. The token is valid, so only the pending-status check can refuse.
func TestEnrollBegin_ActiveAdminCorrectPasswordCounts(t *testing.T) { // regression for #216
	db, rdb, engine := setupEnrollLockout(t)
	adminID, username, _ := enrolledAdmin(t, db, rdb)
	token, err := admin.NewEnrollmentStore(rdb).MintEnrollmentToken(context.Background(), adminID)
	require.NoError(t, err)

	// The probe comes from another IP so it does not spend the IP axis the
	// five attempts below are counted on.
	unknownCode, unknownBody := enrollBeginFrom(engine, "198.51.100.7", uniqueAdminUsername("ghost"), adminTestPassword, badEnrollTok)
	require.Equal(t, http.StatusUnauthorized, unknownCode)
	for i := 0; i < 5; i++ {
		code, body := enrollBegin(engine, username, adminTestPassword, token)
		require.Equal(t, unknownCode, code)
		require.Equal(t, unknownBody, body)
	}

	assert.True(t, keyExists(t, rdb, acctLockKey(username)), "five refusals must lock the account")
	assert.True(t, tokenStillStored(t, rdb, token), "a non-pending admin must be refused before the token is spent")
}

// A lock that lands after a successful EnrollBegin does not block EnrollFinish:
// the handle is unguessable and the token is already spent, so refusing would
// strand the admin. The finished enrollment then clears the counters.
func TestEnrollFinish_LockAfterBegin_StillEnrolls(t *testing.T) { // regression for #216
	db, rdb, engine := setupEnrollLockout(t)
	_, username, token := pendingAdminWithToken(t, db, rdb)

	beginRec := postJSON(engine, "/admin/api/v1/enroll/begin", map[string]string{
		"username": username, "password": adminTestPassword, "token": token,
	})
	require.Equal(t, http.StatusOK, beginRec.Code, beginRec.Body.String())
	var begin struct {
		Handle    string                       `json:"handle"`
		PublicKey *protocol.CredentialCreation `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(beginRec.Body.Bytes(), &begin))

	// The successful begin was the first counted guess; four failures make five.
	for i := 0; i < 4; i++ {
		code, _ := passwordLogin(engine, username, "wrong-password")
		require.Equal(t, http.StatusUnauthorized, code)
	}
	locked, err := admin.NewLockout(rdb, nil).IsLocked(context.Background(), username, defaultTestIP)
	require.NoError(t, err)
	require.True(t, locked)

	va := newVirtualAuthenticator(t, uuid.MustParse(testAllowedAAGID))
	att := va.attestationResponse(t, testAdminRPID, begin.PublicKey.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV)
	finishRec := postJSON(engine, "/admin/api/v1/enroll/finish", map[string]any{ // #nosec G101 -- "credential_name" is a key-name label, not a secret
		"handle": begin.Handle, "attestation": json.RawMessage(att), "credential_name": "primary",
	})
	require.Equal(t, http.StatusOK, finishRec.Code, finishRec.Body.String())

	got, err := admin.NewAdminRepo(db).GetByUsername(context.Background(), username)
	require.NoError(t, err)
	assert.Equal(t, admin.StatusActive, got.Status)
	assert.Zero(t, failCount(t, rdb, acctKey(username)))
	assert.Zero(t, failCount(t, rdb, ipKey(defaultTestIP)))
	assert.False(t, keyExists(t, rdb, acctLockKey(username)), "a finished enrollment lifts the account lock")
	assert.True(t, keyExists(t, rdb, ipLockKey(defaultTestIP)), "a finished enrollment must not lift the IP lock")
}

// A successful begin does not clear earlier failures, and it is itself a counted
// guess; only a finished enrollment clears the count.
func TestEnroll_CounterResetsOnlyAfterFinish(t *testing.T) { // regression for #216
	db, rdb, engine := setupEnrollLockout(t)
	_, username, token := pendingAdminWithToken(t, db, rdb)

	for i := 0; i < 4; i++ {
		code, _ := enrollBegin(engine, username, "wrong-password", token)
		require.Equal(t, http.StatusUnauthorized, code)
	}
	beginRec := postJSON(engine, "/admin/api/v1/enroll/begin", map[string]string{
		"username": username, "password": adminTestPassword, "token": token,
	})
	require.Equal(t, http.StatusOK, beginRec.Code, beginRec.Body.String())

	// Four failures plus the successful begin are five counted guesses, so the
	// next guess is the one that locks: begin did not reset the count.
	code, _ := passwordLogin(engine, username, "wrong-password")
	require.Equal(t, http.StatusTooManyRequests, code, "a successful EnrollBegin must not reset the guess counter")
	locked, err := admin.NewLockout(rdb, nil).IsLocked(context.Background(), username, "")
	require.NoError(t, err)
	assert.True(t, locked)
}

// A finished enrollment clears earlier failures, as a finished login does.
func TestEnroll_FinishResetsCounter(t *testing.T) { // regression for #216
	db, rdb, engine := setupEnrollLockout(t)
	_, username, token := pendingAdminWithToken(t, db, rdb)

	for i := 0; i < 4; i++ {
		code, _ := enrollBegin(engine, username, "wrong-password", token)
		require.Equal(t, http.StatusUnauthorized, code)
	}
	beginRec := postJSON(engine, "/admin/api/v1/enroll/begin", map[string]string{
		"username": username, "password": adminTestPassword, "token": token,
	})
	require.Equal(t, http.StatusOK, beginRec.Code, beginRec.Body.String())
	var begin struct {
		Handle    string                       `json:"handle"`
		PublicKey *protocol.CredentialCreation `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(beginRec.Body.Bytes(), &begin))
	va := newVirtualAuthenticator(t, uuid.MustParse(testAllowedAAGID))
	att := va.attestationResponse(t, testAdminRPID, begin.PublicKey.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV)
	finishRec := postJSON(engine, "/admin/api/v1/enroll/finish", map[string]any{ // #nosec G101 -- "credential_name" is a key-name label, not a secret
		"handle": begin.Handle, "attestation": json.RawMessage(att), "credential_name": "primary",
	})
	require.Equal(t, http.StatusOK, finishRec.Code, finishRec.Body.String())

	assert.Zero(t, failCount(t, rdb, acctKey(username)), "a finished enrollment must reset the account counter")
	assert.Zero(t, failCount(t, rdb, ipKey(defaultTestIP)), "a finished enrollment must reset the IP counter")
}

// An active admin cannot re-enroll a key at the enroll door, even holding a
// token bound to that admin.
func TestEnrollBegin_ActiveAdminWithValidTokenRefused(t *testing.T) { // regression for #216
	db, rdb, engine := setupEnrollLockout(t)
	adminID, username, _ := enrolledAdmin(t, db, rdb)
	token, err := admin.NewEnrollmentStore(rdb).MintEnrollmentToken(context.Background(), adminID)
	require.NoError(t, err)

	code, body := enrollBegin(engine, username, adminTestPassword, token)
	assert.Equal(t, http.StatusUnauthorized, code, body)
	assert.True(t, tokenStillStored(t, rdb, token), "a non-pending admin must be refused before the token is spent")
	assert.Equal(t, int64(1), failCount(t, rdb, acctKey(username)), "the refusal must count toward the lockout")
}

// When the lockout store cannot be read, enroll fails closed: 500, no enrollment.
func TestEnrollBegin_LockoutStoreDown_FailsClosed(t *testing.T) { // regression for #216
	db, rdb, _ := setupEnrollLockout(t)
	_, username, token := pendingAdminWithToken(t, db, rdb)

	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialerRetries: 1})
	t.Cleanup(func() { _ = dead.Close() })
	engine := enrollLockoutEngine(t, db, dead)

	code, body := enrollBegin(engine, username, adminTestPassword, token)
	assert.Equal(t, http.StatusInternalServerError, code, body)
	assert.JSONEq(t, `{"error":"internal error"}`, body)
	assert.True(t, tokenStillStored(t, rdb, token), "a failed lockout read must not reach the token")
}

// A real token bound to ANOTHER admin, sent with this admin's correct
// credentials, is a failure and counts.
func TestEnrollBegin_MismatchedTokenCounts(t *testing.T) { // regression for #216
	db, rdb, engine := setupEnrollLockout(t)
	adminA, usernameA, _ := pendingAdminWithToken(t, db, rdb)
	_, _, tokenB := pendingAdminWithToken(t, db, rdb)

	code, body := enrollBegin(engine, usernameA, adminTestPassword, tokenB)
	assert.Equal(t, http.StatusUnauthorized, code, body)
	assert.Equal(t, int64(1), failCount(t, rdb, acctKey(usernameA)))

	got, err := admin.NewAdminRepo(db).GetByUsername(context.Background(), usernameA)
	require.NoError(t, err)
	assert.Equal(t, admin.StatusPending, got.Status, "admin %s must stay pending", adminA)
}

// Failures on other accounts from one IP lock that IP for every account.
func TestEnrollBegin_IPAxisLocksOtherAccounts(t *testing.T) { // regression for #216
	db, rdb, engine := setupEnrollLockout(t)
	_, username, token := pendingAdminWithToken(t, db, rdb)

	for i := 0; i < 5; i++ {
		code, _ := enrollBeginFrom(engine, "203.0.113.5", uniqueAdminUsername("spray"), "wrong-password", badEnrollTok)
		require.Equal(t, http.StatusUnauthorized, code)
	}
	require.Zero(t, failCount(t, rdb, acctKey(username)), "the target account must be untouched")

	code, body := enrollBeginFrom(engine, "203.0.113.5", username, adminTestPassword, token)
	assert.Equal(t, http.StatusTooManyRequests, code, body)
	assert.JSONEq(t, lockedBody, body)

	code, body = enrollBeginFrom(engine, "198.51.100.20", username, adminTestPassword, token)
	assert.Equal(t, http.StatusOK, code, body)
}

// Failures on one account from many IPs lock that account from a fresh IP.
func TestEnrollBegin_AccountAxisAcrossIPs(t *testing.T) { // regression for #216
	db, rdb, engine := setupEnrollLockout(t)
	_, username, token := pendingAdminWithToken(t, db, rdb)

	ips := []string{"203.0.113.11", "203.0.113.12", "203.0.113.13", "203.0.113.14", "203.0.113.15"}
	for _, ip := range ips {
		code, _ := enrollBeginFrom(engine, ip, username, "wrong-password", badEnrollTok)
		require.Equal(t, http.StatusUnauthorized, code)
	}

	code, body := enrollBeginFrom(engine, "198.51.100.30", username, adminTestPassword, token)
	assert.Equal(t, http.StatusTooManyRequests, code, body)
	assert.True(t, tokenStillStored(t, rdb, token))
}

// A token-store fault after a correct password is a 500, not a failed guess: it
// records no failure and never locks. The guess was counted before the verify, so
// the fault costs exactly one attempt.
func TestEnrollBegin_TokenStoreFault_IsNotAFailure(t *testing.T) { // regression for #216
	db, rdb, engine := setupEnrollLockout(t)
	_, username, token := pendingAdminWithToken(t, db, rdb)

	// A list at the token key makes GETDEL fail with WRONGTYPE, a real store
	// error rather than redis.Nil.
	sum := sha256.Sum256([]byte(token))
	key := "admin_enroll:" + hex.EncodeToString(sum[:])
	ctx := context.Background()
	require.NoError(t, rdb.Del(ctx, key).Err())
	require.NoError(t, rdb.LPush(ctx, key, "x").Err())

	code, body := enrollBegin(engine, username, adminTestPassword, token)
	assert.Equal(t, http.StatusInternalServerError, code, body)
	assert.JSONEq(t, `{"error":"internal error"}`, body)
	assert.Equal(t, int64(1), failCount(t, rdb, acctKey(username)), "the guess was counted before the verify")
	assert.Equal(t, int64(1), failCount(t, rdb, ipKey(defaultTestIP)))
	assert.False(t, keyExists(t, rdb, acctLockKey(username)), "a store fault is not a failure and must not lock")
}
