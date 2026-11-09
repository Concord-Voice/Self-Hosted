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

// An active admin's correct password at enroll is a failure, with the same body
// as an unknown user, and it counts toward the lockout.
func TestEnrollBegin_ActiveAdminCorrectPasswordCounts(t *testing.T) { // regression for #216
	db, rdb, engine := setupEnrollLockout(t)
	_, username, _ := enrolledAdmin(t, db, rdb)

	// The probe comes from another IP so it does not spend the IP axis the
	// five attempts below are counted on.
	b, _ := json.Marshal(map[string]string{"username": uniqueAdminUsername("ghost"), "password": adminTestPassword, "token": badEnrollTok})
	probe := httptest.NewRequest(http.MethodPost, "/admin/api/v1/enroll/begin", bytes.NewReader(b))
	probe.Header.Set("Content-Type", "application/json")
	probe.RemoteAddr = "198.51.100.7:1234"
	probeRec := httptest.NewRecorder()
	engine.ServeHTTP(probeRec, probe)
	unknownCode, unknownBody := probeRec.Code, probeRec.Body.String()
	require.Equal(t, http.StatusUnauthorized, unknownCode)
	for i := 0; i < 5; i++ {
		code, body := enrollBegin(engine, username, adminTestPassword, badEnrollTok)
		require.Equal(t, unknownCode, code)
		require.Equal(t, unknownBody, body)
	}

	locked, _, err := admin.NewLockout(rdb, nil).IsLocked(context.Background(), username, "")
	require.NoError(t, err)
	assert.True(t, locked, "five enroll attempts on an active admin must lock the account")
}

// EnrollFinish refuses a held handle once the account is locked.
func TestEnrollFinish_LockedAfterBegin_Returns429(t *testing.T) { // regression for #216
	db, rdb, engine := setupEnrollLockout(t)
	adminID, username, token := pendingAdminWithToken(t, db, rdb)

	beginRec := postJSON(engine, "/admin/api/v1/enroll/begin", map[string]string{
		"username": username, "password": adminTestPassword, "token": token,
	})
	require.Equal(t, http.StatusOK, beginRec.Code, beginRec.Body.String())
	var begin struct {
		Handle    string                       `json:"handle"`
		PublicKey *protocol.CredentialCreation `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(beginRec.Body.Bytes(), &begin))

	lk := admin.NewLockout(rdb, nil)
	for i := 0; i < 6; i++ {
		require.NoError(t, lk.RecordFailure(context.Background(), username, "203.0.113.9"))
	}

	va := newVirtualAuthenticator(t, uuid.MustParse(testAllowedAAGID))
	att := va.attestationResponse(t, testAdminRPID, begin.PublicKey.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV)
	finishRec := postJSON(engine, "/admin/api/v1/enroll/finish", map[string]any{ // #nosec G101 -- "credential_name" is a key-name label, not a secret
		"handle": begin.Handle, "attestation": json.RawMessage(att), "credential_name": "primary",
	})
	assert.Equal(t, http.StatusTooManyRequests, finishRec.Code, finishRec.Body.String())

	got, err := admin.NewAdminRepo(db).GetByUsername(context.Background(), username)
	require.NoError(t, err)
	assert.Equal(t, admin.StatusPending, got.Status, "a locked finish must not activate admin %s", adminID)
}

// A successful begin does not clear earlier failures; only a finished enrollment does.
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

	// Fifth failure after a successful begin: still locks, because begin did not reset.
	code, _ := passwordLogin(engine, username, "wrong-password")
	require.Equal(t, http.StatusUnauthorized, code)
	locked, _, err := admin.NewLockout(rdb, nil).IsLocked(context.Background(), username, "")
	require.NoError(t, err)
	assert.True(t, locked, "a successful EnrollBegin must not reset the failure counter")
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

	// One more failure would be the fifth if the finish had not reset the count.
	code, _ := passwordLogin(engine, username, "wrong-password")
	require.Equal(t, http.StatusUnauthorized, code)
	locked, _, err := admin.NewLockout(rdb, nil).IsLocked(context.Background(), username, "")
	require.NoError(t, err)
	assert.False(t, locked, "a finished enrollment must reset the failure counter")
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
}
