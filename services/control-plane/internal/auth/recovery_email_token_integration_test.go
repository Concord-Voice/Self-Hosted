package auth_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/require"
)

var recoveryKeyLog = regexp.MustCompile(`account recovery key.*\bkey=([A-Za-z0-9_-]{43})\b`)

// postFrom sends a JSON request from a chosen client address, so a test can
// spread requests over many addresses the way a distributed attacker would.
func postFrom(t *testing.T, ts *testhelpers.TestServer, ip, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = ip + ":40000"
	w := httptest.NewRecorder()
	ts.Router.ServeHTTP(w, req)
	return w
}

// requestRecoveryKey runs begin with format=token and returns the emailed key,
// read from the development-mode mail log.
func requestRecoveryKey(t *testing.T, ts *testhelpers.TestServer, ip, addr string) string {
	t.Helper()
	logs := ts.CaptureLogs(t)
	w := postFrom(t, ts, ip, pathRecoveryBegin, map[string]string{"email": addr, "format": "token"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body map[string]any
	testhelpers.ParseJSON(t, w, &body)
	require.Equal(t, "token", body["format"])
	var key string
	require.Eventually(t, func() bool {
		m := recoveryKeyLog.FindStringSubmatch(logs.String())
		if m == nil {
			return false
		}
		key = m[1]
		return true
	}, 5*time.Second, 10*time.Millisecond)
	return key
}

func randomRecoveryKey(t *testing.T) string {
	t.Helper()
	var b [32]byte
	_, err := rand.Read(b[:])
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func TestRecoveryKeyCompletesRecovery(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "recoverykey1")
	key := requestRecoveryKey(t, ts, "198.51.100.1", user.Email)

	w := postFrom(t, ts, "198.51.100.2", pathRecoveryVerifyCode, map[string]string{"token": key})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body map[string]any
	testhelpers.ParseJSON(t, w, &body)
	require.NotEmpty(t, body["recovery_token"])

	again := postFrom(t, ts, "198.51.100.3", pathRecoveryVerifyCode, map[string]string{"token": key})
	require.Equal(t, http.StatusUnauthorized, again.Code, "a recovery key works once")
}

// The regression this change exists for: wrong submissions for an address,
// from one client address or from many, in either format, cannot stop the
// mailbox owner from completing recovery with the key in their email.
func TestRecoveryKeySurvivesWrongSubmissionsFromManyAddresses(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "recoverykeyflood")
	key := requestRecoveryKey(t, ts, "198.51.100.10", user.Email)

	// One attacker address: the route limiter and the address ban refuse it
	// quickly; neither is keyed by the target account.
	for range 50 {
		w := postFrom(t, ts, "203.0.113.1", pathRecoveryVerifyCode, map[string]string{"email": user.Email, "token": randomRecoveryKey(t)})
		require.Contains(t, []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusForbidden}, w.Code)
	}
	// Many attacker addresses: 1,000 wrong keys, each from its own address, so
	// every one reaches the handler.
	for i := range 1000 {
		ip := fmt.Sprintf("10.%d.%d.%d", i/65536, (i/256)%256, i%256+1)
		w := postFrom(t, ts, ip, pathRecoveryVerifyCode, map[string]string{"email": user.Email, "token": randomRecoveryKey(t)})
		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	}
	// And enough wrong legacy codes to spend the address's whole code budget.
	for i := range 11 {
		ip := fmt.Sprintf("172.31.0.%d", i+1)
		w := postFrom(t, ts, ip, pathRecoveryVerifyCode, map[string]string{"email": user.Email, "code": "000000"})
		require.Contains(t, []int{http.StatusUnauthorized, http.StatusTooManyRequests}, w.Code)
	}
	last := postFrom(t, ts, "172.31.1.1", pathRecoveryVerifyCode, map[string]string{"email": user.Email, "code": "000000"})
	require.Equal(t, http.StatusTooManyRequests, last.Code, "precondition: the legacy code budget is spent")

	owner := postFrom(t, ts, "198.51.100.11", pathRecoveryVerifyCode, map[string]string{"token": key})
	require.Equal(t, http.StatusOK, owner.Code, owner.Body.String())
	var body map[string]any
	testhelpers.ParseJSON(t, owner, &body)
	require.NotEmpty(t, body["recovery_token"])
}

// A code email someone else requested holds the account's send slot, and the
// owner may be unable to use it. The owner's key request must still send a key,
// and that key replaces the code.
func TestRecoveryKeySurvivesACodeHoldingTheSlot(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "recoverykeyslot")
	logs := ts.CaptureLogs(t)
	w := postFrom(t, ts, "203.0.113.20", pathRecoveryBegin, map[string]string{"email": user.Email})
	require.Equal(t, http.StatusOK, w.Code)
	codeLog := regexp.MustCompile(`account recovery code.*\bcode=(\d{6})\b`)
	var code string
	require.Eventually(t, func() bool {
		m := codeLog.FindStringSubmatch(logs.String())
		if m == nil {
			return false
		}
		code = m[1]
		return true
	}, 5*time.Second, 10*time.Millisecond)

	key := requestRecoveryKey(t, ts, "198.51.100.70", user.Email)
	stale := postFrom(t, ts, "198.51.100.71", pathRecoveryVerifyCode, map[string]string{"email": user.Email, "code": code})
	require.Equal(t, http.StatusUnauthorized, stale.Code, "the key replaced the code")
	owner := postFrom(t, ts, "198.51.100.72", pathRecoveryVerifyCode, map[string]string{"token": key})
	require.Equal(t, http.StatusOK, owner.Code, owner.Body.String())
}

func TestRecoveryKeyUniformFailures(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "recoverykeyuniform")
	key := requestRecoveryKey(t, ts, "198.51.100.20", user.Email)
	sum := sha256.Sum256([]byte(key))
	require.NoError(t, ts.Redis.PExpire(context.Background(), "recovery_code:token:"+hex.EncodeToString(sum[:]), time.Millisecond).Err())
	time.Sleep(20 * time.Millisecond)

	bodies := make([]string, 0, 2)
	for i, token := range []string{key, randomRecoveryKey(t)} {
		w := postFrom(t, ts, fmt.Sprintf("198.51.100.%d", 30+i), pathRecoveryVerifyCode, map[string]string{"token": token})
		require.Equal(t, http.StatusUnauthorized, w.Code)
		bodies = append(bodies, w.Body.String())
	}
	require.Equal(t, bodies[0], bodies[1], "an expired key and a key never issued answer alike")

	known := postFrom(t, ts, "198.51.100.40", pathRecoveryBegin, map[string]string{"email": user.Email, "format": "token"})
	unknown := postFrom(t, ts, "198.51.100.41", pathRecoveryBegin, map[string]string{"email": "nobody@test.concord.chat", "format": "token"})
	require.Equal(t, known.Code, unknown.Code)
	require.Equal(t, known.Body.String(), unknown.Body.String())
}

func recoveryKeyRecordKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "recovery_code:token:" + hex.EncodeToString(sum[:])
}

// A key works only while the account still uses the address it was sent to,
// and a refused key gets the same answer as a key never issued.
func TestRecoveryKeyRefusedAfterAddressChange(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()
	user := ts.CreateTestUser(t, "recoverykeymoved")
	key := requestRecoveryKey(t, ts, "198.51.100.80", user.Email)
	_, err := ts.DB.ExecContext(ctx, `UPDATE users SET email = $1 WHERE id = $2`, "moved-"+user.Email, user.ID)
	require.NoError(t, err)

	moved := postFrom(t, ts, "198.51.100.81", pathRecoveryVerifyCode, map[string]string{"token": key})
	never := postFrom(t, ts, "198.51.100.82", pathRecoveryVerifyCode, map[string]string{"token": randomRecoveryKey(t)})
	require.Equal(t, http.StatusUnauthorized, moved.Code, moved.Body.String())
	require.Equal(t, never.Body.String(), moved.Body.String())
	require.Zero(t, ts.Redis.Exists(ctx, recoveryKeyRecordKey(key)).Val(), "a refused key is discarded")
}

// An unreadable key record is a server fault, not a wrong key: it answers 500
// and is left in place rather than spent.
func TestRecoveryKeyMalformedRecordAnswers500(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()
	user := ts.CreateTestUser(t, "recoverykeymalformed")
	key := requestRecoveryKey(t, ts, "198.51.100.85", user.Email)
	require.NoError(t, ts.Redis.Set(ctx, recoveryKeyRecordKey(key), `{}`, time.Minute).Err())

	w := postFrom(t, ts, "198.51.100.86", pathRecoveryVerifyCode, map[string]string{"token": key})
	require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	require.Equal(t, int64(1), ts.Redis.Exists(ctx, recoveryKeyRecordKey(key)).Val())
}

// A completed key recovery clears the address's legacy code budget, as a
// completed code recovery does.
func TestRecoveryKeyCompletionClearsCodeBudget(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()
	user := ts.CreateTestUser(t, "recoverykeybudget")
	key := requestRecoveryKey(t, ts, "198.51.100.90", user.Email)
	budget := "recovery_fail:" + strings.ToLower(user.Email)
	require.NoError(t, ts.Redis.Set(ctx, budget, "7", time.Hour).Err())

	w := postFrom(t, ts, "198.51.100.91", pathRecoveryVerifyCode, map[string]string{"token": key})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Eventually(t, func() bool { return ts.Redis.Exists(ctx, budget).Val() == 0 }, 5*time.Second, 10*time.Millisecond)
}

func TestRecoveryKeyMFAAccountGetsChallenge(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "recoverykeymfa")
	_, err := ts.DB.Exec(`UPDATE users SET mfa_enabled = TRUE, mfa_methods = '{totp,email}', recovery_only_methods = '{totp}' WHERE id = $1`, user.ID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, enabled, confirmed) VALUES ($1, $2, $3, TRUE, TRUE)`, user.ID, []byte("sealed-test-secret"), []byte("test-nonce"))
	require.NoError(t, err)
	key := requestRecoveryKey(t, ts, "198.51.100.50", user.Email)

	w := postFrom(t, ts, "198.51.100.51", pathRecoveryVerifyCode, map[string]string{"token": key})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body map[string]any
	testhelpers.ParseJSON(t, w, &body)
	require.NotContains(t, body, "recovery_token", "a recovery key alone must not grant reset authority")
	require.Equal(t, true, body["mfa_required"])
	require.NotEmpty(t, body["mfa_challenge_token"])
}

func TestRecoveryLegacyCodeStillCompletes(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "recoverylegacy")
	logs := ts.CaptureLogs(t)
	w := postFrom(t, ts, "198.51.100.60", pathRecoveryBegin, map[string]string{"email": user.Email})
	require.Equal(t, http.StatusOK, w.Code)
	var begin map[string]any
	testhelpers.ParseJSON(t, w, &begin)
	require.Equal(t, "code", begin["format"])

	codeLog := regexp.MustCompile(`account recovery code.*\bcode=(\d{6})\b`)
	var code string
	require.Eventually(t, func() bool {
		m := codeLog.FindStringSubmatch(logs.String())
		if m == nil {
			return false
		}
		code = m[1]
		return true
	}, 5*time.Second, 10*time.Millisecond)

	v := postFrom(t, ts, "198.51.100.61", pathRecoveryVerifyCode, map[string]string{"email": user.Email, "code": code})
	require.Equal(t, http.StatusOK, v.Code, v.Body.String())
}
