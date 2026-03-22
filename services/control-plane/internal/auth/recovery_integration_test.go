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

// seedRecoveryCode inserts a known recovery code into Redis so verify-code can find it.
func seedRecoveryCode(t *testing.T, ts *testhelpers.TestServer, email, code, userID string) {
	t.Helper()
	hash := sha256.Sum256([]byte(code))
	record := map[string]interface{}{
		"code_hash": hex.EncodeToString(hash[:]),
		"user_id":   userID,
		"attempts":  0,
	}
	data, err := json.Marshal(record)
	require.NoError(t, err)
	key := "recovery_code:" + email
	err = ts.Redis.Set(context.Background(), key, data, 10*time.Minute).Err()
	require.NoError(t, err)
}

// --- RecoveryBegin Tests ---

func TestRecoveryBegin_ReturnsOKForExistingEmail(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "recoveruser1")

	w := ts.DoRequest("POST", "/api/v1/auth/recovery/begin", map[string]interface{}{
		"email": user.Email,
	}, nil)

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["message"], "If an account exists")
}

func TestRecoveryBegin_ReturnsOKForNonexistentEmail(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", "/api/v1/auth/recovery/begin", map[string]interface{}{
		"email": "nonexistent@test.concord.chat",
	}, nil)

	// Must return same 200 as existing email (anti-enumeration)
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["message"], "If an account exists")
}

func TestRecoveryBegin_StoresCodeInRedis(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "recoveruser2")

	ts.DoRequest("POST", "/api/v1/auth/recovery/begin", map[string]interface{}{
		"email": user.Email,
	}, nil)

	// Verify a recovery code was stored in Redis
	key := "recovery_code:" + user.Email
	val, err := ts.Redis.Get(context.Background(), key).Result()
	require.NoError(t, err)
	assert.NotEmpty(t, val)

	var record map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(val), &record))
	assert.NotEmpty(t, record["code_hash"])
	assert.Equal(t, user.ID, record["user_id"])
	assert.Equal(t, float64(0), record["attempts"])
}

// --- RecoveryVerifyCode Tests ---

func TestRecoveryVerifyCode_Success(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "verifyuser1")

	// Seed a known code
	seedRecoveryCode(t, ts, user.Email, "123456", user.ID)

	w := ts.DoRequest("POST", "/api/v1/auth/recovery/verify-code", map[string]interface{}{
		"email": user.Email,
		"code":  "123456",
	}, nil)

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.NotEmpty(t, body["recovery_token"])
	// User has no recovery key by default
	assert.Equal(t, false, body["has_recovery_key"])
}

func TestRecoveryVerifyCode_WrongCode(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "verifyuser2")

	seedRecoveryCode(t, ts, user.Email, "123456", user.ID)

	w := ts.DoRequest("POST", "/api/v1/auth/recovery/verify-code", map[string]interface{}{
		"email": user.Email,
		"code":  "999999",
	}, nil)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRecoveryVerifyCode_AttemptTracking(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "verifyuser3")

	seedRecoveryCode(t, ts, user.Email, "123456", user.ID)

	// 5 wrong attempts should exhaust the code
	for i := 0; i < 5; i++ {
		w := ts.DoRequest("POST", "/api/v1/auth/recovery/verify-code", map[string]interface{}{
			"email": user.Email,
			"code":  "000000",
		}, nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	}

	// Flush rate limit key so the 6th request reaches the handler
	// (route is rate-limited to 5 req/15min — we've used all 5 above)
	ts.Redis.Del(context.Background(), "ratelimit:ip:192.0.2.1:POST:/api/v1/auth/recovery/verify-code")

	// Even the correct code should now fail (max attempts exceeded)
	// Anti-enumeration: returns same 401 as wrong code
	w := ts.DoRequest("POST", "/api/v1/auth/recovery/verify-code", map[string]interface{}{
		"email": user.Email,
		"code":  "123456",
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRecoveryVerifyCode_DeletesCodeOnSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "verifyuser4")

	seedRecoveryCode(t, ts, user.Email, "654321", user.ID)

	w := ts.DoRequest("POST", "/api/v1/auth/recovery/verify-code", map[string]interface{}{
		"email": user.Email,
		"code":  "654321",
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	// Code should be deleted from Redis (single-use)
	key := "recovery_code:" + user.Email
	_, err := ts.Redis.Get(context.Background(), key).Result()
	assert.Error(t, err) // redis.Nil
}

// --- ResetPassword Token Single-Use Tests ---

func TestRecoveryResetPassword_TokenSingleUse(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "resetuser1")

	// Get a recovery token
	seedRecoveryCode(t, ts, user.Email, "111111", user.ID)
	w := ts.DoRequest("POST", "/api/v1/auth/recovery/verify-code", map[string]interface{}{
		"email": user.Email,
		"code":  "111111",
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var verifyBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &verifyBody)
	recoveryToken := verifyBody["recovery_token"].(string)

	pub, wrappedKey, salt := testhelpers.E2EETestKeys()
	_ = pub // not needed for reset-password

	resetPayload := map[string]interface{}{
		"recovery_token":      recoveryToken,
		"new_password":        "NewSecurePassword123!",
		"wrapped_private_key": wrappedKey,
		"key_derivation_salt": salt,
		"key_derivation_alg":  "argon2id",
	}

	// First reset should succeed
	w = ts.DoRequest("POST", "/api/v1/auth/recovery/reset-password", resetPayload, nil)
	assert.Equal(t, http.StatusOK, w.Code)

	// Second reset with same token should fail (single-use)
	w = ts.DoRequest("POST", "/api/v1/auth/recovery/reset-password", resetPayload, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "already been used")
}

// --- ResetAccount Token Single-Use Tests ---

func TestRecoveryResetAccount_TokenSingleUse(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "resetuser2")

	// Get a recovery token
	seedRecoveryCode(t, ts, user.Email, "222222", user.ID)
	w := ts.DoRequest("POST", "/api/v1/auth/recovery/verify-code", map[string]interface{}{
		"email": user.Email,
		"code":  "222222",
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var verifyBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &verifyBody)
	recoveryToken := verifyBody["recovery_token"].(string)

	pub, wrappedKey, salt := testhelpers.E2EETestKeys()

	resetPayload := map[string]interface{}{
		"recovery_token":       recoveryToken,
		"new_password":         "NewSecurePassword123!",
		"wrapped_private_key":  wrappedKey,
		"key_derivation_salt":  salt,
		"key_derivation_alg":   "argon2id",
		"public_key":           pub,
		"acknowledge_data_loss": true,
	}

	// First reset should succeed
	w = ts.DoRequest("POST", "/api/v1/auth/recovery/reset-account", resetPayload, nil)
	assert.Equal(t, http.StatusOK, w.Code)

	// Second reset with same token should fail (single-use)
	w = ts.DoRequest("POST", "/api/v1/auth/recovery/reset-account", resetPayload, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRecoveryResetAccount_RequiresDataLossAcknowledgement(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "resetuser3")

	seedRecoveryCode(t, ts, user.Email, "333333", user.ID)
	w := ts.DoRequest("POST", "/api/v1/auth/recovery/verify-code", map[string]interface{}{
		"email": user.Email,
		"code":  "333333",
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var verifyBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &verifyBody)
	recoveryToken := verifyBody["recovery_token"].(string)

	pub, wrappedKey, salt := testhelpers.E2EETestKeys()

	// Missing acknowledge_data_loss should fail
	w = ts.DoRequest("POST", "/api/v1/auth/recovery/reset-account", map[string]interface{}{
		"recovery_token":      recoveryToken,
		"new_password":        "NewSecurePassword123!",
		"wrapped_private_key": wrappedKey,
		"key_derivation_salt": salt,
		"key_derivation_alg":  "argon2id",
		"public_key":          pub,
	}, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- Anti-Enumeration Tests ---

func TestRecoveryBegin_SameResponseForValidAndInvalidEmails(t *testing.T) {
	ts := setupTS(t)
	ts.CreateTestUser(t, "enumuser1")

	// Valid email
	w1 := ts.DoRequest("POST", "/api/v1/auth/recovery/begin", map[string]interface{}{
		"email": "enumuser1@test.concord.chat",
	}, nil)

	// Invalid email
	w2 := ts.DoRequest("POST", "/api/v1/auth/recovery/begin", map[string]interface{}{
		"email": "doesnotexist@test.concord.chat",
	}, nil)

	// Both must return identical status and message
	assert.Equal(t, w1.Code, w2.Code)

	var body1, body2 map[string]interface{}
	testhelpers.ParseJSON(t, w1, &body1)
	testhelpers.ParseJSON(t, w2, &body2)
	assert.Equal(t, body1["message"], body2["message"])
}
