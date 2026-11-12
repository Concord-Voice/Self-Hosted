package mfa_test

// Every MFA route that takes the account password counts it toward the
// account's shared /auth/login lockout, so a stolen session cannot guess the
// password here without the five-guess limit (CWE-307).

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// resetAttemptLimits clears userID's per-route limiter keys and its MFA
// settings step-up budget, so neither answers 429 before the lockout is
// reached. The shared lockout's own keys are left alone.
func resetAttemptLimits(t *testing.T, ts *testhelpers.TestServer, userID string) {
	t.Helper()
	ctx := context.Background()
	keys, err := ts.Redis.Keys(ctx, "ratelimit:*"+userID+"*").Result()
	require.NoError(t, err)
	keys = append(keys, stepup.NewBudget(nil, stepup.MFASettingsBudgetPrefix).Key(userID))
	require.NoError(t, ts.Redis.Del(ctx, keys...).Err())
}

// lockoutRun sends five wrong passwords and then the correct one through
// send, and asserts the five are refused as wrong and the sixth as locked.
func lockoutRun(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser, send func(password string) *httptest.ResponseRecorder) {
	t.Helper()
	for i := range 5 {
		resetAttemptLimits(t, ts, user.ID)
		w := send(testBadPassword)
		require.Equal(t, http.StatusForbidden, w.Code, "guess %d: %s", i+1, w.Body.String())
	}
	resetAttemptLimits(t, ts, user.ID)
	w := send(testPassword)
	assert.Equal(t, http.StatusLocked, w.Code, "the lockout refuses the correct password too: %s", w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "account_locked", body["error_code"])

	login := ts.DoRequest(http.MethodPost, urlAuthLogin,
		map[string]string{"email": user.Email, "password": user.Password}, nil)
	assert.Equal(t, http.StatusUnauthorized, login.Code, "the same lock holds at /auth/login")
}

func TestRegenerateBackupCodes_FiveWrongPasswordsLockTheAccount(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "backuplock")

	lockoutRun(t, ts, user, func(password string) *httptest.ResponseRecorder {
		return ts.DoRequest(http.MethodPost, urlBackupCodesRegen, map[string]interface{}{
			"password": password, "code": "123456",
		}, testhelpers.AuthHeaders(user.AccessToken))
	})
}

func TestTOTPDisable_FiveWrongPasswordsLockTheAccount(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "totpdislock")
	secret, _ := enrollTOTP(t, ts, user)

	lockoutRun(t, ts, user, func(password string) *httptest.ResponseRecorder {
		code, err := totp.GenerateCodeCustom(secret, time.Now(), totp.ValidateOpts{
			Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
		})
		require.NoError(t, err)
		return ts.DoRequest(http.MethodPost, urlTOTPDisable, map[string]interface{}{
			"password": password, "code": code,
		}, testhelpers.AuthHeaders(user.AccessToken))
	})

	var enrolled bool
	require.NoError(t, ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM user_mfa_totp WHERE user_id = $1)`, user.ID).Scan(&enrolled))
	assert.True(t, enrolled, "a locked account's TOTP factor is not removed")
}

// The settings gate (verifyStepUpFactors) shares the lockout too.
func TestTOTPSetup_FiveWrongPasswordsLockTheAccount(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "totpsetuplock")

	lockoutRun(t, ts, user, func(password string) *httptest.ResponseRecorder {
		return ts.DoRequest(http.MethodPost, urlTOTPSetup, map[string]interface{}{
			"password": password,
		}, testhelpers.AuthHeaders(user.AccessToken))
	})
}

// A Redis fault refuses the guess as a server error: never a lock, never a
// pass.
func TestRegenerateBackupCodes_RedisDownIs500(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "backupredisdown")
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	mr.Close()

	h := mfa.NewHandler(ts.DB, rdb, logger.New("test"), nil, testhelpers.TestJWTSecret, nil, "test")
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("user_id", user.ID) })
	router.POST(urlBackupCodesRegen, h.RegenerateBackupCodes)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, urlBackupCodesRegen,
		bytes.NewBufferString(`{"password":"`+user.Password+`","code":"123456"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
}
