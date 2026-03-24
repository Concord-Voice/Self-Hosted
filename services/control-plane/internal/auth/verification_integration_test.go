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

// seedVerificationCode inserts a known verification code into Redis so confirm-code can find it.
func seedVerificationCode(t *testing.T, ts *testhelpers.TestServer, userID, email, code string) {
	t.Helper()
	hash := sha256.Sum256([]byte(code))
	record := map[string]interface{}{
		"code_hash": hex.EncodeToString(hash[:]),
		"email":     email,
		"attempts":  0,
	}
	data, err := json.Marshal(record)
	require.NoError(t, err)
	key := "email_verify:" + userID
	err = ts.Redis.Set(context.Background(), key, data, 10*time.Minute).Err()
	require.NoError(t, err)
}

// seedVerificationCodeWithAttempts seeds a code with a specific attempt count.
func seedVerificationCodeWithAttempts(t *testing.T, ts *testhelpers.TestServer, userID, email, code string, attempts int) {
	t.Helper()
	hash := sha256.Sum256([]byte(code))
	record := map[string]interface{}{
		"code_hash": hex.EncodeToString(hash[:]),
		"email":     email,
		"attempts":  attempts,
	}
	data, err := json.Marshal(record)
	require.NoError(t, err)
	key := "email_verify:" + userID
	err = ts.Redis.Set(context.Background(), key, data, 10*time.Minute).Err()
	require.NoError(t, err)
}

// --- SendCode Tests ---

func TestSendCode_Success(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUserUnverified(t, "sendcode1")

	w := ts.DoRequest("POST", "/api/v1/auth/verify-email/send", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["message"], "Verification code sent")
	assert.NotNil(t, body["expires_in"])

	// Verify Redis key was created
	key := "email_verify:" + user.ID
	val, err := ts.Redis.Get(context.Background(), key).Result()
	require.NoError(t, err)
	assert.NotEmpty(t, val)

	var record map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(val), &record))
	assert.NotEmpty(t, record["code_hash"])
	assert.Equal(t, user.Email, record["email"])
}

func TestSendCode_AlreadyVerified(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "sendcode2") // email_verified=true

	w := ts.DoRequest("POST", "/api/v1/auth/verify-email/send", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "already verified")
}

// --- ConfirmCode Tests ---

func TestConfirmCode_Success(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUserUnverified(t, "confirm1")
	seedVerificationCode(t, ts, user.ID, user.Email, "123456")

	w := ts.DoRequest("POST", "/api/v1/auth/verify-email/confirm", map[string]interface{}{
		"code": "123456",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["message"], "verified")
	assert.NotEmpty(t, body["access_token"])

	// Verify DB updated
	var emailVerified bool
	err := ts.DB.QueryRow(`SELECT email_verified FROM users WHERE id = $1`, user.ID).Scan(&emailVerified)
	require.NoError(t, err)
	assert.True(t, emailVerified)

	// Verify Redis key deleted
	_, err = ts.Redis.Get(context.Background(), "email_verify:"+user.ID).Result()
	assert.Error(t, err, "Redis key should be deleted after verification")
}

func TestConfirmCode_WrongCode(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUserUnverified(t, "confirm2")
	seedVerificationCode(t, ts, user.ID, user.Email, "123456")

	w := ts.DoRequest("POST", "/api/v1/auth/verify-email/confirm", map[string]interface{}{
		"code": "999999",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "Invalid verification code")

	// Verify attempts incremented
	val, err := ts.Redis.Get(context.Background(), "email_verify:"+user.ID).Result()
	require.NoError(t, err)
	var record map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(val), &record))
	assert.Equal(t, float64(1), record["attempts"])
}

func TestConfirmCode_NoPendingCode(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUserUnverified(t, "confirm3")
	// No Redis entry

	w := ts.DoRequest("POST", "/api/v1/auth/verify-email/confirm", map[string]interface{}{
		"code": "123456",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "No pending verification code")
}

func TestConfirmCode_TooManyAttempts(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUserUnverified(t, "confirm4")
	seedVerificationCodeWithAttempts(t, ts, user.ID, user.Email, "123456", 5)

	w := ts.DoRequest("POST", "/api/v1/auth/verify-email/confirm", map[string]interface{}{
		"code": "123456",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusTooManyRequests, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "Too many attempts")
}

func TestConfirmCode_NormalizesInput(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUserUnverified(t, "confirm5")
	seedVerificationCode(t, ts, user.ID, user.Email, "123456")

	// Submit with dashes — should be normalized to "123456"
	w := ts.DoRequest("POST", "/api/v1/auth/verify-email/confirm", map[string]interface{}{
		"code": "123-456",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestConfirmCode_InvalidFormat(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUserUnverified(t, "confirm6")

	w := ts.DoRequest("POST", "/api/v1/auth/verify-email/confirm", map[string]interface{}{
		"code": "abc",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "6 digits")
}

// --- ChangeEmail Tests ---

func TestChangeEmail_Success(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUserUnverified(t, "changeemail1")

	w := ts.DoRequest("POST", "/api/v1/auth/verify-email/change-email", map[string]interface{}{
		"new_email": "newemail@test.concord.chat",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify DB email updated
	var dbEmail string
	err := ts.DB.QueryRow(`SELECT email FROM users WHERE id = $1`, user.ID).Scan(&dbEmail)
	require.NoError(t, err)
	assert.Equal(t, "newemail@test.concord.chat", dbEmail)

	// Verify new verification code seeded in Redis
	val, err := ts.Redis.Get(context.Background(), "email_verify:"+user.ID).Result()
	require.NoError(t, err)
	var record map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(val), &record))
	assert.Equal(t, "newemail@test.concord.chat", record["email"])
}

func TestChangeEmail_AlreadyVerified(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "changeemail2") // verified

	w := ts.DoRequest("POST", "/api/v1/auth/verify-email/change-email", map[string]interface{}{
		"new_email": "newemail2@test.concord.chat",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestChangeEmail_DuplicateEmail(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUserUnverified(t, "changeemail3")
	user2 := ts.CreateTestUser(t, "existinguser")

	w := ts.DoRequest("POST", "/api/v1/auth/verify-email/change-email", map[string]interface{}{
		"new_email": user2.Email,
	}, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code)
}

// --- Protected Route Test ---

func TestProtectedRoute_BlocksUnverified(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUserUnverified(t, "unverified1")

	// Protected routes require verified email — GET /servers should be blocked
	w := ts.DoRequest("GET", "/api/v1/servers", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestProtectedRoute_AllowsVerified(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "verified1") // email_verified=true

	w := ts.DoRequest("GET", "/api/v1/servers", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}
