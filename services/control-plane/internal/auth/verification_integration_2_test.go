package auth_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Additional ConfirmCode edge case tests ---

func TestConfirmCode_MissingBody(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUserUnverified(t, "confirmnobody")

	w := ts.DoRequest("POST", pathVerifyEmailConfirm, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestConfirmCode_NonDigitCode(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUserUnverified(t, "confirmnondigit")

	w := ts.DoRequest("POST", pathVerifyEmailConfirm, map[string]interface{}{
		"code": "12ab56",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "6 digits")
}

func TestConfirmCode_TooShortCode(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUserUnverified(t, "confirmshort")

	w := ts.DoRequest("POST", pathVerifyEmailConfirm, map[string]interface{}{
		"code": "123",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestConfirmCode_TooLongCode(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUserUnverified(t, "confirmlong")

	w := ts.DoRequest("POST", pathVerifyEmailConfirm, map[string]interface{}{
		"code": "12345678",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestConfirmCode_AttemptsDecrement(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUserUnverified(t, "confirmattempt")
	seedVerificationCode(t, ts, user.ID, user.Email, testCode)

	// Wrong code attempt 1
	w := ts.DoRequest("POST", pathVerifyEmailConfirm, map[string]interface{}{
		"code": "000000",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	remaining, ok := body["attempts_remaining"].(float64)
	require.True(t, ok)
	assert.Equal(t, float64(4), remaining)

	// Wrong code attempt 2
	w = ts.DoRequest("POST", pathVerifyEmailConfirm, map[string]interface{}{
		"code": "000001",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	testhelpers.ParseJSON(t, w, &body)
	remaining = body["attempts_remaining"].(float64)
	assert.Equal(t, float64(3), remaining)
}

func TestConfirmCode_MaxAttemptsDeletesKey(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUserUnverified(t, "confirmmaxdel")
	seedVerificationCodeWithAttempts(t, ts, user.ID, user.Email, testCode, 5)

	w := ts.DoRequest("POST", pathVerifyEmailConfirm, map[string]interface{}{
		"code": testCode,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusTooManyRequests, w.Code)

	// Redis key should be deleted
	ctx := context.Background()
	_, err := ts.Redis.Get(ctx, redisKeyEmailVerify+user.ID).Result()
	assert.Error(t, err, "Redis key should be deleted after max attempts")
}

func TestConfirmCode_EmailMismatch(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUserUnverified(t, "confirmmismatch")

	// Seed with a different email than what's in the DB
	hash := sha256.Sum256([]byte(testCode))
	record := map[string]interface{}{
		keyCodeHash: hex.EncodeToString(hash[:]),
		"email":     "different@test.concord.chat",
		keyAttempts: 0,
	}
	data, err := json.Marshal(record)
	require.NoError(t, err)
	err = ts.Redis.Set(context.Background(), redisKeyEmailVerify+user.ID, data, 10*time.Minute).Err()
	require.NoError(t, err)

	w := ts.DoRequest("POST", pathVerifyEmailConfirm, map[string]interface{}{
		"code": testCode,
	}, testhelpers.AuthHeaders(user.AccessToken))
	// Should fail because user's DB email doesn't match the Redis record email
	assert.Equal(t, http.StatusConflict, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "email has changed")
}

func TestConfirmCode_WhitespaceHandling(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUserUnverified(t, "confirmws")
	seedVerificationCode(t, ts, user.ID, user.Email, testCode)

	// Submit with whitespace — should be trimmed
	w := ts.DoRequest("POST", pathVerifyEmailConfirm, map[string]interface{}{
		"code": " 123456 ",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

// --- ChangeEmail additional edge cases ---

func TestChangeEmail_InvalidEmailFormat(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUserUnverified(t, "changeinvalid")

	w := ts.DoRequest("POST", pathChangeEmail, map[string]interface{}{
		keyNewEmail: "not-an-email",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestChangeEmail_EmptyBody(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUserUnverified(t, "changeempty")

	w := ts.DoRequest("POST", pathChangeEmail, map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestChangeEmail_CaseNormalization(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUserUnverified(t, "changecase")

	w := ts.DoRequest("POST", pathChangeEmail, map[string]interface{}{
		keyNewEmail: "TEST@Example.COM",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify email was lowercased in DB
	var dbEmail string
	err := ts.DB.QueryRow(`SELECT email FROM users WHERE id = $1`, user.ID).Scan(&dbEmail)
	require.NoError(t, err)
	assert.Equal(t, "test@example.com", dbEmail)
}

func TestChangeEmail_DeletesOldVerificationCode(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUserUnverified(t, "changedelcode")
	ctx := context.Background()

	// Seed an existing verification code
	seedVerificationCode(t, ts, user.ID, user.Email, testCode)

	// Change email — should delete old code and create new one
	w := ts.DoRequest("POST", pathChangeEmail, map[string]interface{}{
		keyNewEmail: "newchangetest@test.concord.chat",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify new code is in Redis with new email
	val, err := ts.Redis.Get(ctx, redisKeyEmailVerify+user.ID).Result()
	require.NoError(t, err)
	var record map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(val), &record))
	assert.Equal(t, "newchangetest@test.concord.chat", record["email"])
}
