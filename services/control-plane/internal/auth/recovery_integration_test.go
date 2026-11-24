package auth_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/api"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	recoveryCodeKeyPrefix  = "recovery_code:"
	pathRecoveryBegin      = "/api/v1/auth/recovery/begin"
	pathRecoveryVerifyCode = "/api/v1/auth/recovery/verify-code"
	pathRecoveryResetPwd   = "/api/v1/auth/recovery/reset-password" //nolint:gosec // G101 false positive: URL path, not a credential
	pathRecoveryResetAcct  = "/api/v1/auth/recovery/reset-account"
	testRecoveryCode       = "123456"
	testNewPassword        = "NewSecurePassword123!" //nolint:gosec // G101 false positive: test fixture, not a real credential
	testKeyDerivationAlg   = "argon2id"
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
	key := recoveryCodeKeyPrefix + email
	err = ts.Redis.Set(context.Background(), key, data, 10*time.Minute).Err()
	require.NoError(t, err)
}

func waitForRecoveryRecord(t *testing.T, ts *testhelpers.TestServer, key string) string {
	t.Helper()
	require.Eventually(t, func() bool {
		value, err := ts.Redis.Get(context.Background(), key).Result()
		return err == nil && value != ""
	}, 5*time.Second, 10*time.Millisecond)
	value, err := ts.Redis.Get(context.Background(), key).Result()
	require.NoError(t, err)
	return value
}

func setupRecoverySMTPFailureTS(t *testing.T) (*testhelpers.TestServer, <-chan struct{}, func(), *testhelpers.SyncBuffer) {
	t.Helper()
	t.Setenv("CONCORD_ENV", "test")

	db, dbCleanup := testhelpers.SetupTestDB(t)
	redisClient, redisCleanup := testhelpers.SetupTestRedis(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseSMTP := func() {
		releaseOnce.Do(func() { close(release) })
	}
	go func() {
		var acceptedOnce sync.Once
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			acceptedOnce.Do(func() { close(accepted) })
			go func() {
				<-release
				_ = conn.Close()
			}()
		}
	}()

	host, portText, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)

	cfg := &config.Config{
		WSEventBudget:    config.WSEventBudgetConfig{Enabled: false, ScalePercent: 100},
		Environment:      "test",
		Port:             "0",
		JWTSecret:        testhelpers.TestJWTSecret,
		AllowedOrigins:   []string{"*"},
		SMTPHost:         host,
		SMTPPort:         port,
		SMTPFrom:         "Concord Voice <noreply@test.concord.chat>",
		MFAEncryptionKey: "0000000000000000000000000000000000000000000000000000000000000000",
		// Directly-constructed Config: the keyring version gets no getEnv default
		// here, and ParseKeyring fails closed on 0 (#2307).
		MFAEncryptionKeyVersion: 1,
		WebAuthnRPID:            "localhost",
		WebAuthnRPOrigins:       []string{"http://localhost:3001"},
	}

	disclosure := presencehistory.BuildDisclosure(presencehistory.DisclosureOptions{InstanceType: "saas"})
	presenceHistoryService := presencehistory.NewService(
		db,
		disclosure,
		cfg.ActivityHistoryClusterEnabled,
	)
	// Dependency cleanup is registered first so LIFO execution shuts down Hub,
	// operations metrics, the permission enforcer, and NATS before Redis/DB.
	t.Cleanup(func() {
		redisCleanup()
		dbCleanup()
	})
	var logs testhelpers.SyncBuffer
	router, hub, natsClient, opsRuntime, permissionEnforcer, _, closePresence, _, _, _, err := api.NewRouter(
		t.Context(),
		db,
		redisClient,
		cfg,
		nil,
		logger.NewWithWriter(&logs),
		api.RouterDependencies{PresenceHistory: presenceHistoryService},
	)
	require.NoError(t, err)
	if natsClient != nil {
		t.Cleanup(func() { _ = natsClient.Close() })
	}
	t.Cleanup(permissionEnforcer.Close)
	t.Cleanup(func() { require.NoError(t, opsRuntime.Stop(context.Background())) })
	t.Cleanup(func() { hub.Shutdown() })
	// Registered after the hub so it runs BEFORE it: the presence drains
	// disconnect through the hub (#2738).
	t.Cleanup(closePresence)
	// Registered last so a failing assertion releases the blocked SMTP worker
	// before closePresence drains the recovery queue.
	t.Cleanup(releaseSMTP)

	return &testhelpers.TestServer{
		Router:          router,
		Hub:             hub,
		DB:              db,
		Redis:           redisClient,
		PresenceHistory: presenceHistoryService,
	}, accepted, releaseSMTP, &logs
}

// --- RecoveryBegin Tests ---

func TestRecoveryBeginReturnsOKForExistingEmail(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "recoveruser1")

	w := ts.DoRequest("POST", pathRecoveryBegin, map[string]interface{}{
		"email": user.Email,
	}, nil)

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["message"], "If an account exists")
}

func TestRecoveryBeginReturnsOKForNonexistentEmail(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", pathRecoveryBegin, map[string]interface{}{
		"email": "nonexistent@test.concord.chat",
	}, nil)

	// Must return same 200 as existing email (anti-enumeration)
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["message"], "If an account exists")
}

func TestRecoveryBeginStoresCodeInRedis(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "recoveruser2")

	ts.DoRequest("POST", pathRecoveryBegin, map[string]interface{}{
		"email": user.Email,
	}, nil)

	// Verify a recovery code was stored in Redis
	key := recoveryCodeKeyPrefix + user.Email
	val := waitForRecoveryRecord(t, ts, key)

	var record map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(val), &record))
	assert.NotEmpty(t, record["code_hash"])
	assert.Equal(t, user.ID, record["user_id"])
	assert.Equal(t, float64(0), record["attempts"])
}

func TestRecoveryBeginKeepsLiveRecord(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "recoverylive")
	// The live record comes from a real begin, so the account's send slot is
	// held the way production holds it (#344).
	first := ts.DoRequest("POST", pathRecoveryBegin, map[string]string{"email": user.Email}, nil)
	require.Equal(t, http.StatusOK, first.Code)
	key := recoveryCodeKeyPrefix + user.Email
	before := waitForRecoveryRecord(t, ts, key)
	require.NoError(t, ts.Redis.Del(context.Background(), "ratelimit:ip:192.0.2.1:POST:"+pathRecoveryBegin).Err())

	logs := ts.CaptureLogs(t)
	w := ts.DoRequest("POST", pathRecoveryBegin, map[string]string{"email": user.Email}, nil)
	require.Equal(t, http.StatusOK, w.Code)
	// Begin returns before the worker runs; wait until the job has seen the
	// live record before comparing it.
	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "Recovery code already pending")
	}, 5*time.Second, 10*time.Millisecond)
	after, err := ts.Redis.Get(context.Background(), key).Result()
	require.NoError(t, err)
	assert.Equal(t, before, after, "a live recovery record must retain its code and attempt count")
}

func TestRecoveryAccountBudgetRejectsEleventhGuess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "recoverybudget")
	seedRecoveryCode(t, ts, user.Email, testRecoveryCode, user.ID)

	for i := 0; i < 10; i++ {
		response := ts.DoRequest("POST", pathRecoveryVerifyCode, map[string]string{
			"email": user.Email,
			"code":  "000000", //nolint:gosec // test value, not a credential
		}, nil)
		require.Equal(t, http.StatusUnauthorized, response.Code)
		require.NoError(t, ts.Redis.Del(context.Background(),
			"ratelimit:ip:192.0.2.1:POST:"+pathRecoveryVerifyCode,
			"auth_failures:ip:192.0.2.1",
			"auth_ban:ip:192.0.2.1",
		).Err())
	}

	w := ts.DoRequest("POST", pathRecoveryVerifyCode, map[string]string{
		"email": user.Email,
		"code":  "000000", //nolint:gosec // test value, not a credential
	}, nil)
	require.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.NotEmpty(t, w.Header().Get("Retry-After"))
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equalf(t, true, body["step_up_budget_exhausted"], "response body: %#v", body)
}

func TestRecoveryAccountBudgetClearsAfterCorrectCode(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "recoverybudgetclear")
	seedRecoveryCode(t, ts, user.Email, testRecoveryCode, user.ID)

	require.Equal(t, http.StatusUnauthorized, ts.DoRequest("POST", pathRecoveryVerifyCode, map[string]string{
		"email": user.Email,
		"code":  "000000", //nolint:gosec // test value, not a credential
	}, nil).Code)
	require.Equal(t, http.StatusOK, ts.DoRequest("POST", pathRecoveryVerifyCode, map[string]string{
		"email": user.Email,
		"code":  testRecoveryCode,
	}, nil).Code)

	exists, err := ts.Redis.Exists(context.Background(), "recovery_fail:"+user.Email).Result()
	require.NoError(t, err)
	assert.Zero(t, exists)
}

// Delivery runs off the shared recovery workers, so mail stalled for existing
// accounts cannot delay a later queued address and reveal that the earlier
// addresses have accounts.
func TestRecoveryBeginDeliveryDoesNotHoldWorkers(t *testing.T) {
	ts, smtpAccepted, _, _ := setupRecoverySMTPFailureTS(t)
	// More existing accounts than the recovery worker pool. The fake SMTP
	// server never answers, so every delivery stalls.
	const accounts = 8
	addrs := make([]string, 0, accounts)
	for i := range accounts {
		addrs = append(addrs, ts.CreateTestUser(t, fmt.Sprintf("recoveryhold%d", i)).Email)
	}
	for i, addr := range addrs {
		w := ts.DoRequest("POST", pathRecoveryBegin, map[string]string{"email": addr}, nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.NoError(t, ts.Redis.Del(context.Background(), "ratelimit:ip:192.0.2.1:POST:"+pathRecoveryBegin).Err())
		if i == 0 {
			select {
			case <-smtpAccepted:
			case <-time.After(2 * time.Second):
				t.Fatal("recovery delivery did not reach SMTP")
			}
		}
	}
	// A worker held through delivery would leave the last addresses unstored
	// until the fake server is released at cleanup.
	for _, addr := range addrs {
		waitForRecoveryRecord(t, ts, recoveryCodeKeyPrefix+addr)
	}
}

func TestRecoveryBeginSMTPFailureKeepsGenericResponseAndClearsCode(t *testing.T) {
	ts, smtpAccepted, releaseSMTP, logs := setupRecoverySMTPFailureTS(t)
	user := ts.CreateTestUser(t, "recoverysmtpfail")

	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response <- ts.DoRequest("POST", pathRecoveryBegin, map[string]interface{}{
			"email": user.Email,
		}, nil)
	}()

	select {
	case <-smtpAccepted:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery worker did not reach SMTP")
	}

	var w *httptest.ResponseRecorder
	select {
	case w = <-response:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery response waited for SMTP")
	}

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["message"], "If an account exists")

	key := recoveryCodeKeyPrefix + user.Email
	exists, err := ts.Redis.Exists(context.Background(), key).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), exists)

	releaseSMTP()
	require.Eventually(t, func() bool {
		exists, existsErr := ts.Redis.Exists(context.Background(), key).Result()
		return existsErr == nil && exists == 0
	}, 5*time.Second, 10*time.Millisecond)
	failureLog := findLogLine(logs.String(), "Failed to send recovery code email")
	require.NotEmpty(t, failureLog)
	assert.NotContains(t, failureLog, user.Email)
	assert.Contains(t, failureLog, "failure_class=recovery_email_delivery")

	missing := ts.DoRequest("POST", pathRecoveryBegin, map[string]interface{}{
		"email": "smtp-missing@test.concord.chat",
	}, nil)
	assert.Equal(t, w.Code, missing.Code)

	var missingBody map[string]interface{}
	testhelpers.ParseJSON(t, missing, &missingBody)
	assert.Equal(t, body["message"], missingBody["message"])
}

// --- RecoveryVerifyCode Tests ---

func TestRecoveryVerifyCodeSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "verifyuser1")

	// Seed a known code
	seedRecoveryCode(t, ts, user.Email, testRecoveryCode, user.ID)

	w := ts.DoRequest("POST", pathRecoveryVerifyCode, map[string]interface{}{
		"email": user.Email,
		"code":  testRecoveryCode,
	}, nil)

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.NotEmpty(t, body["recovery_token"])
	// User has no recovery key by default
	assert.Equal(t, false, body["has_recovery_key"])
}

func TestRecoveryVerifyCodeWrongCode(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "verifyuser2")

	seedRecoveryCode(t, ts, user.Email, testRecoveryCode, user.ID)

	w := ts.DoRequest("POST", pathRecoveryVerifyCode, map[string]interface{}{
		"email": user.Email,
		"code":  "999999",
	}, nil)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRecoveryVerifyCodeAttemptTracking(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "verifyuser3")

	seedRecoveryCode(t, ts, user.Email, testRecoveryCode, user.ID)

	// 5 wrong attempts should exhaust the code
	for i := 0; i < 5; i++ {
		w := ts.DoRequest("POST", pathRecoveryVerifyCode, map[string]interface{}{
			"email": user.Email,
			"code":  "000000",
		}, nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	}

	// Flush rate limit key so the 6th request reaches the handler
	// (route is rate-limited to 5 req/15min — we've used all 5 above)
	ts.Redis.Del(context.Background(), "ratelimit:ip:192.0.2.1:POST:"+pathRecoveryVerifyCode)

	// Even the correct code should now fail (max attempts exceeded)
	// Anti-enumeration: returns same 401 as wrong code
	w := ts.DoRequest("POST", pathRecoveryVerifyCode, map[string]interface{}{
		"email": user.Email,
		"code":  testRecoveryCode,
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRecoveryVerifyCodeDeletesCodeOnSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "verifyuser4")

	seedRecoveryCode(t, ts, user.Email, "654321", user.ID)

	w := ts.DoRequest("POST", pathRecoveryVerifyCode, map[string]interface{}{
		"email": user.Email,
		"code":  "654321",
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	// Code should be deleted from Redis (single-use)
	key := recoveryCodeKeyPrefix + user.Email
	_, err := ts.Redis.Get(context.Background(), key).Result()
	assert.Error(t, err) // redis.Nil
}

// --- ResetPassword Token Single-Use Tests ---

func TestRecoveryResetPasswordTokenSingleUse(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "resetuser1")

	// Get a recovery token
	seedRecoveryCode(t, ts, user.Email, "111111", user.ID)
	w := ts.DoRequest("POST", pathRecoveryVerifyCode, map[string]interface{}{
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
		"new_password":        testNewPassword,
		"wrapped_private_key": wrappedKey,
		"key_derivation_salt": salt,
		"key_derivation_alg":  testKeyDerivationAlg,
	}

	// First reset should succeed
	w = ts.DoRequest("POST", pathRecoveryResetPwd, resetPayload, nil)
	assert.Equal(t, http.StatusOK, w.Code)

	// Second reset with same token should fail (single-use)
	w = ts.DoRequest("POST", pathRecoveryResetPwd, resetPayload, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "already been used")
}

// --- ResetAccount Token Single-Use Tests ---

func TestRecoveryResetAccountTokenSingleUse(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "resetuser2")

	// Get a recovery token
	seedRecoveryCode(t, ts, user.Email, "222222", user.ID)
	w := ts.DoRequest("POST", pathRecoveryVerifyCode, map[string]interface{}{
		"email": user.Email,
		"code":  "222222",
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var verifyBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &verifyBody)
	recoveryToken := verifyBody["recovery_token"].(string)

	pub, wrappedKey, salt := testhelpers.E2EETestKeys()

	resetPayload := map[string]interface{}{
		"recovery_token":        recoveryToken,
		"new_password":          testNewPassword,
		"wrapped_private_key":   wrappedKey,
		"key_derivation_salt":   salt,
		"key_derivation_alg":    testKeyDerivationAlg,
		"public_key":            pub,
		"acknowledge_data_loss": true,
	}

	// First reset should succeed
	w = ts.DoRequest("POST", pathRecoveryResetAcct, resetPayload, nil)
	assert.Equal(t, http.StatusOK, w.Code)

	// Second reset with same token should fail (single-use)
	w = ts.DoRequest("POST", pathRecoveryResetAcct, resetPayload, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRecoveryResetAccountRequiresDataLossAcknowledgement(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "resetuser3")

	seedRecoveryCode(t, ts, user.Email, "333333", user.ID)
	w := ts.DoRequest("POST", pathRecoveryVerifyCode, map[string]interface{}{
		"email": user.Email,
		"code":  "333333",
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var verifyBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &verifyBody)
	recoveryToken := verifyBody["recovery_token"].(string)

	pub, wrappedKey, salt := testhelpers.E2EETestKeys()

	// Missing acknowledge_data_loss should fail
	w = ts.DoRequest("POST", pathRecoveryResetAcct, map[string]interface{}{
		"recovery_token":      recoveryToken,
		"new_password":        testNewPassword,
		"wrapped_private_key": wrappedKey,
		"key_derivation_salt": salt,
		"key_derivation_alg":  testKeyDerivationAlg,
		"public_key":          pub,
	}, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- Anti-Enumeration Tests ---

func TestRecoveryBeginSameResponseForValidAndInvalidEmails(t *testing.T) {
	ts := setupTS(t)
	ts.CreateTestUser(t, "enumuser1")

	// Valid email
	w1 := ts.DoRequest("POST", pathRecoveryBegin, map[string]interface{}{
		"email": "enumuser1@test.concord.chat",
	}, nil)

	// Invalid email
	w2 := ts.DoRequest("POST", pathRecoveryBegin, map[string]interface{}{
		"email": "doesnotexist@test.concord.chat",
	}, nil)

	// Both must return identical status and message
	assert.Equal(t, w1.Code, w2.Code)

	var body1, body2 map[string]interface{}
	testhelpers.ParseJSON(t, w1, &body1)
	testhelpers.ParseJSON(t, w2, &body2)
	assert.Equal(t, body1["message"], body2["message"])
}

// --- RecoveryResetPassword Happy Path ---

func TestRecoveryResetPasswordSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "resetpwdok")

	// Get recovery token
	seedRecoveryCode(t, ts, user.Email, "999999", user.ID)
	w := ts.DoRequest("POST", pathRecoveryVerifyCode, map[string]interface{}{
		"email": user.Email,
		"code":  "999999",
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var verifyBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &verifyBody)
	recoveryToken := verifyBody["recovery_token"].(string)

	pub, wrappedKey, salt := testhelpers.E2EETestKeys()

	// Reset password
	w = ts.DoRequest("POST", pathRecoveryResetPwd, map[string]interface{}{
		"recovery_token":      recoveryToken,
		"new_password":        testNewPassword,
		"wrapped_private_key": wrappedKey,
		"key_derivation_salt": salt,
		"key_derivation_alg":  testKeyDerivationAlg,
		"public_key":          pub,
	}, nil)
	assert.Equal(t, http.StatusOK, w.Code)

	// Login with new password should work
	w = ts.DoRequest("POST", "/api/v1/auth/login", map[string]interface{}{
		"email":    user.Email,
		"password": testNewPassword,
	}, nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRecoveryResetPasswordWeakPassword(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "resetpwdweak")

	seedRecoveryCode(t, ts, user.Email, "888888", user.ID)
	w := ts.DoRequest("POST", pathRecoveryVerifyCode, map[string]interface{}{
		"email": user.Email,
		"code":  "888888",
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var verifyBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &verifyBody)
	recoveryToken := verifyBody["recovery_token"].(string)

	pub, wrappedKey, salt := testhelpers.E2EETestKeys()

	// Weak password should fail
	w = ts.DoRequest("POST", pathRecoveryResetPwd, map[string]interface{}{
		"recovery_token":      recoveryToken,
		"new_password":        "weak",
		"wrapped_private_key": wrappedKey,
		"key_derivation_salt": salt,
		"key_derivation_alg":  testKeyDerivationAlg,
		"public_key":          pub,
	}, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRecoveryResetPasswordInvalidToken(t *testing.T) {
	ts := setupTS(t)

	pub, wrappedKey, salt := testhelpers.E2EETestKeys()

	w := ts.DoRequest("POST", pathRecoveryResetPwd, map[string]interface{}{
		"recovery_token":      "invalid-token",
		"new_password":        testNewPassword,
		"wrapped_private_key": wrappedKey,
		"key_derivation_salt": salt,
		"key_derivation_alg":  testKeyDerivationAlg,
		"public_key":          pub,
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// --- RecoveryResetAccount Happy Path ---

func TestRecoveryResetAccountSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "resetacctok")

	seedRecoveryCode(t, ts, user.Email, "777777", user.ID)
	w := ts.DoRequest("POST", pathRecoveryVerifyCode, map[string]interface{}{
		"email": user.Email,
		"code":  "777777",
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var verifyBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &verifyBody)
	recoveryToken := verifyBody["recovery_token"].(string)

	pub, wrappedKey, salt := testhelpers.E2EETestKeys()

	w = ts.DoRequest("POST", pathRecoveryResetAcct, map[string]interface{}{
		"recovery_token":        recoveryToken,
		"new_password":          testNewPassword,
		"wrapped_private_key":   wrappedKey,
		"key_derivation_salt":   salt,
		"key_derivation_alg":    testKeyDerivationAlg,
		"public_key":            pub,
		"acknowledge_data_loss": true,
	}, nil)
	assert.Equal(t, http.StatusOK, w.Code)

	// Login with new password
	w = ts.DoRequest("POST", "/api/v1/auth/login", map[string]interface{}{
		"email":    user.Email,
		"password": testNewPassword,
	}, nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

// --- Logout with cookie ---

func TestLogoutWithCookie(t *testing.T) {
	ts := setupTS(t)

	// New flow: register returns pending_id; access token comes from confirm step.
	pub, wrappedKey, salt := testhelpers.E2EETestKeys()
	w := ts.DoRequest("POST", "/api/v1/auth/register", map[string]interface{}{
		"email":               "logoutcookie@test.concord.chat",
		"username":            "logoutcookie",
		"password":            "TestPassword123!", //nolint:gosec // test credential
		"age_confirmation":    true,
		"public_key":          pub,
		"wrapped_private_key": wrappedKey,
		"key_derivation_salt": salt,
	}, nil)
	require.Equal(t, http.StatusCreated, w.Code)

	var regBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &regBody)
	pendingID := regBody["pending_id"].(string)

	code := testhelpers.FetchVerificationCode(t, ts, pendingID)
	confirmW := ts.DoRequest("POST", "/api/v1/auth/register/confirm", map[string]string{
		"pending_id": pendingID, "code": code,
	}, nil)
	require.Equal(t, http.StatusOK, confirmW.Code)

	var confirmBody map[string]interface{}
	testhelpers.ParseJSON(t, confirmW, &confirmBody)
	accessToken := confirmBody["access_token"].(string)

	w = ts.DoRequest("POST", "/api/v1/auth/logout", nil, testhelpers.AuthHeaders(accessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}
