package auth_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pathRegister      = "/api/v1/auth/register"
	pathLogin         = "/api/v1/auth/login"
	pathRefresh       = "/api/v1/auth/refresh"
	pathUsersMe       = "/api/v1/users/me"
	headerContentType = "Content-Type"
	contentTypeJSON   = "application/json"
	testEmail         = "newuser@test.concord.chat"
	testPassword      = "TestPassword123!"  //nolint:gosec // test credential constant
	testWrongPassword = "WrongPassword999!" //nolint:gosec // test credential constant
)

func setupTS(t *testing.T) *testhelpers.TestServer {
	t.Helper()
	return testhelpers.SetupTestServer(t)
}

func registerPayload() map[string]interface{} {
	pub, priv, salt := testhelpers.E2EETestKeys()
	return map[string]interface{}{
		"email":               testEmail,
		"username":            "newuser",
		"password":            testPassword,
		"age_confirmation":    true,
		"public_key":          pub,
		"wrapped_private_key": priv,
		"key_derivation_salt": salt,
	}
}

// --- Registration Tests ---

func TestRegisterSuccess(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("POST", pathRegister, registerPayload(), nil)

	assert.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.NotEmpty(t, body["access_token"])
	assert.Equal(t, float64(900), body["expires_in"])

	user := body["user"].(map[string]interface{})
	assert.NotEmpty(t, user["id"])
	assert.Equal(t, "newuser", user["username"])
}

func TestRegisterDuplicateEmail(t *testing.T) {
	ts := setupTS(t)

	// First registration
	ts.DoRequest("POST", pathRegister, registerPayload(), nil)

	// Second with same email, different username
	payload := registerPayload()
	payload["username"] = "differentuser"
	w := ts.DoRequest("POST", pathRegister, payload, nil)

	assert.Equal(t, http.StatusConflict, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "Email already in use")
}

func TestRegisterDuplicateUsername(t *testing.T) {
	ts := setupTS(t)

	ts.DoRequest("POST", pathRegister, registerPayload(), nil)

	payload := registerPayload()
	payload["email"] = "different@test.concord.chat"
	w := ts.DoRequest("POST", pathRegister, payload, nil)

	assert.Equal(t, http.StatusConflict, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "Username already taken")
}

func TestRegisterWeakPassword(t *testing.T) {
	ts := setupTS(t)

	payload := registerPayload()
	payload["password"] = "weak"
	w := ts.DoRequest("POST", pathRegister, payload, nil)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRegisterMissingE2EEKeys(t *testing.T) {
	ts := setupTS(t)

	payload := map[string]interface{}{
		"email":            "nokeys@test.concord.chat",
		"username":         "nokeysuser",
		"password":         testPassword,
		"age_confirmation": true,
	}
	w := ts.DoRequest("POST", pathRegister, payload, nil)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRegisterNoAgeConfirmation(t *testing.T) {
	ts := setupTS(t)

	payload := registerPayload()
	payload["age_confirmation"] = false
	w := ts.DoRequest("POST", pathRegister, payload, nil)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRegisterSetsRefreshCookie(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("POST", pathRegister, registerPayload(), nil)

	assert.Equal(t, http.StatusCreated, w.Code)
	cookies := w.Result().Cookies()
	var refreshCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "refresh_token" {
			refreshCookie = c
			break
		}
	}
	require.NotNil(t, refreshCookie, "refresh_token cookie should be set")
	assert.True(t, refreshCookie.HttpOnly)
}

// --- Login Tests ---

func TestLoginSuccess(t *testing.T) {
	ts := setupTS(t)

	// Register first
	ts.DoRequest("POST", pathRegister, registerPayload(), nil)

	// Login
	w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":    testEmail,
		"password": testPassword,
	}, nil)

	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.NotEmpty(t, body["access_token"])
	assert.NotNil(t, body["user"])
	assert.NotNil(t, body["e2ee_keys"])
}

func TestLoginWrongPassword(t *testing.T) {
	ts := setupTS(t)

	ts.DoRequest("POST", pathRegister, registerPayload(), nil)

	w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":    testEmail,
		"password": "WrongPassword123!",
	}, nil)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestLoginNonexistentEmail(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":    "nonexistent@test.concord.chat",
		"password": testPassword,
	}, nil)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// --- Logout Tests ---

func TestLogoutSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "logoutuser")

	w := ts.DoRequest("POST", "/api/v1/auth/logout", nil, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestLogoutBlacklistsToken(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "logoutuser2")

	// Logout
	ts.DoRequest("POST", "/api/v1/auth/logout", nil, testhelpers.AuthHeaders(user.AccessToken))

	// Try to use the same token — should fail
	w := ts.DoRequest("GET", pathUsersMe, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// --- Protected Route Tests ---

func TestProtectedRouteNoToken(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", pathUsersMe, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestProtectedRouteInvalidToken(t *testing.T) {
	ts := setupTS(t)

	headers := http.Header{}
	headers.Set("Authorization", "Bearer invalid.token.here")
	w := ts.DoRequest("GET", pathUsersMe, nil, headers)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestProtectedRouteValidToken(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "validuser")

	w := ts.DoRequest("GET", pathUsersMe, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	userData := body["user"].(map[string]interface{})
	assert.Equal(t, user.Username, userData["username"])
}

// --- Refresh Tests ---

func TestRefreshSuccess(t *testing.T) {
	ts := setupTS(t)

	// Register to get a refresh cookie
	w := ts.DoRequest("POST", pathRegister, registerPayload(), nil)
	require.Equal(t, http.StatusCreated, w.Code)

	// Extract refresh_token cookie
	var refreshCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "refresh_token" {
			refreshCookie = c
			break
		}
	}
	require.NotNil(t, refreshCookie)

	// Build request with cookie
	req := httptest.NewRequest("POST", pathRefresh, nil)
	req.AddCookie(refreshCookie)
	req.Header.Set(headerContentType, contentTypeJSON)

	rw := httptest.NewRecorder()
	ts.Router.ServeHTTP(rw, req)

	assert.Equal(t, http.StatusOK, rw.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, rw, &body)
	assert.NotEmpty(t, body["access_token"])
	assert.Equal(t, float64(900), body["expires_in"])
}

func TestRefreshNoCookie(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", pathRefresh, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRefreshInvalidToken(t *testing.T) {
	ts := setupTS(t)

	req := httptest.NewRequest("POST", pathRefresh, nil)
	// nosemgrep: go.lang.security.audit.net.cookie-missing-httponly.cookie-missing-httponly, go.lang.security.audit.net.cookie-missing-secure.cookie-missing-secure
	req.AddCookie(&http.Cookie{Name: "refresh_token", Value: "totally-invalid-token"})
	req.Header.Set(headerContentType, contentTypeJSON)

	rw := httptest.NewRecorder()
	ts.Router.ServeHTTP(rw, req)

	assert.Equal(t, http.StatusUnauthorized, rw.Code)
}

func TestRefreshRotatesToken(t *testing.T) {
	ts := setupTS(t)

	// Register
	w := ts.DoRequest("POST", pathRegister, registerPayload(), nil)
	require.Equal(t, http.StatusCreated, w.Code)

	var firstCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "refresh_token" {
			firstCookie = c
			break
		}
	}
	require.NotNil(t, firstCookie)

	// First refresh — should succeed
	req := httptest.NewRequest("POST", pathRefresh, nil)
	req.AddCookie(firstCookie)
	req.Header.Set(headerContentType, contentTypeJSON)
	rw := httptest.NewRecorder()
	ts.Router.ServeHTTP(rw, req)
	require.Equal(t, http.StatusOK, rw.Code)

	// Replay old token — should fail (breach detection)
	req2 := httptest.NewRequest("POST", pathRefresh, nil)
	req2.AddCookie(firstCookie)
	req2.Header.Set(headerContentType, contentTypeJSON)
	rw2 := httptest.NewRecorder()
	ts.Router.ServeHTTP(rw2, req2)
	assert.Equal(t, http.StatusUnauthorized, rw2.Code)
}

// --- WS Ticket Tests ---

func TestIssueWSTicketSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "wsticketuser")

	w := ts.DoRequest("POST", "/api/v1/auth/ws-ticket", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.NotEmpty(t, body["ticket"])
}

func TestIssueWSTicketUnauthorized(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", "/api/v1/auth/ws-ticket", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// --- Health Check ---

func TestHealthCheck(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", "/health", nil, nil)
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	err := json.NewDecoder(w.Body).Decode(&body)
	require.NoError(t, err)
	assert.Equal(t, "healthy", body["status"])
}

// --- Login Lockout ---

func TestLoginLockoutAfterFailedAttempts(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "lockoutuser")

	// Fail login 5 times to trigger lockout
	for i := 0; i < 5; i++ {
		ts.DoRequest("POST", pathLogin, map[string]interface{}{
			"email":    user.Email,
			"password": testWrongPassword, //nolint:gosec // test credential
		}, nil)
	}

	// 6th attempt: lockout returns 401 with generic message (anti-enumeration)
	w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":    user.Email,
		"password": testWrongPassword, //nolint:gosec // test credential
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	// Even correct password should fail during lockout
	w = ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":    user.Email,
		"password": testhelpers.TestAuthPlaintext,
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestLoginSucceedsAfterFailedAttempts(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "lockoutok")

	// Fail a few times (not enough for lockout)
	for i := 0; i < 2; i++ {
		ts.DoRequest("POST", pathLogin, map[string]interface{}{
			"email":    user.Email,
			"password": testWrongPassword, //nolint:gosec // test credential
		}, nil)
	}

	// Correct password should still work
	w := ts.DoRequest("POST", pathLogin, map[string]interface{}{
		"email":    user.Email,
		"password": testhelpers.TestAuthPlaintext,
	}, nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

// --- Refresh with Bearer Header ---

func TestRefreshWithXRefreshTokenHeader(t *testing.T) {
	ts := setupTS(t)

	// Register to get a refresh token
	w := ts.DoRequest("POST", pathRegister, registerPayload(), nil)
	require.Equal(t, http.StatusCreated, w.Code)

	var regBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &regBody)
	refreshToken := regBody["refresh_token"].(string)

	// Use X-Refresh-Token header instead of cookie
	headers := http.Header{}
	headers.Set("X-Refresh-Token", refreshToken)
	headers.Set(headerContentType, contentTypeJSON)

	w = ts.DoRequest("POST", pathRefresh, nil, headers)
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.NotEmpty(t, body["access_token"])
}

// --- Device Recovery ---

func TestCreateDeviceRecoveryRequestNoTrustedDevices(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "devrecovery")

	// Seed a recovery code and get a recovery token
	recoveryToken := seedAndVerifyRecovery(t, ts, user)

	w := ts.DoRequest("POST", "/api/v1/auth/recovery/device-request", map[string]interface{}{
		"recovery_token": recoveryToken,
	}, nil)

	// Should fail — no trusted devices configured
	assert.Contains(t, []int{http.StatusBadRequest, http.StatusNotFound, http.StatusConflict}, w.Code)
}

func TestPollDeviceRecoveryRequestNotFound(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", "/api/v1/auth/recovery/device-request/00000000-0000-0000-0000-000000000000", nil, nil)
	assert.Contains(t, []int{http.StatusNotFound, http.StatusBadRequest}, w.Code)
}

// --- Social Recovery ---

func TestCreateSocialRecoveryRequestNoCircle(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "socialrecovery")

	recoveryToken := seedAndVerifyRecovery(t, ts, user)

	w := ts.DoRequest("POST", "/api/v1/auth/recovery/social-request", map[string]interface{}{
		"recovery_token": recoveryToken,
	}, nil)

	// Should fail — no recovery circle configured
	assert.Contains(t, []int{http.StatusBadRequest, http.StatusNotFound, http.StatusConflict}, w.Code)
}

func TestPollSocialRecoveryRequestNotFound(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", "/api/v1/auth/recovery/social-request/00000000-0000-0000-0000-000000000000", nil, nil)
	assert.Contains(t, []int{http.StatusNotFound, http.StatusBadRequest}, w.Code)
}

// --- Helpers ---

// seedAndVerifyRecovery creates a recovery code and verifies it, returning a recovery token.
func seedAndVerifyRecovery(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser) string {
	t.Helper()

	// Seed a known recovery code in Redis
	code := "123456"
	codeHash := sha256Hex(code)
	redisKey := "recovery_code:" + normalizeEmail(user.Email)
	record := map[string]interface{}{
		"code_hash": codeHash,
		"user_id":   user.ID,
		"attempts":  0,
	}
	recordJSON, _ := json.Marshal(record)
	ts.Redis.Set(context.Background(), redisKey, recordJSON, 10*time.Minute)

	// Verify the code to get a recovery token
	w := ts.DoRequest("POST", "/api/v1/auth/recovery/verify-code", map[string]interface{}{
		"email": user.Email,
		"code":  code,
	}, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	return body["recovery_token"].(string)
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func normalizeEmail(e string) string {
	return strings.ToLower(strings.TrimSpace(e))
}
