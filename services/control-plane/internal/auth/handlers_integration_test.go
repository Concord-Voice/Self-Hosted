package auth_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTS(t *testing.T) *testhelpers.TestServer {
	t.Helper()
	return testhelpers.SetupTestServer(t)
}

func registerPayload() map[string]interface{} {
	pub, priv, salt := testhelpers.E2EETestKeys()
	return map[string]interface{}{
		"email":               "newuser@test.concord.chat",
		"username":            "newuser",
		"password":            "TestPassword123!",
		"age_confirmation":    true,
		"public_key":          pub,
		"wrapped_private_key": priv,
		"key_derivation_salt": salt,
	}
}

// --- Registration Tests ---

func TestRegister_Success(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("POST", "/api/v1/auth/register", registerPayload(), nil)

	assert.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.NotEmpty(t, body["access_token"])
	assert.Equal(t, float64(900), body["expires_in"])

	user := body["user"].(map[string]interface{})
	assert.NotEmpty(t, user["id"])
	assert.Equal(t, "newuser", user["username"])
}

func TestRegister_DuplicateEmail(t *testing.T) {
	ts := setupTS(t)

	// First registration
	ts.DoRequest("POST", "/api/v1/auth/register", registerPayload(), nil)

	// Second with same email, different username
	payload := registerPayload()
	payload["username"] = "differentuser"
	w := ts.DoRequest("POST", "/api/v1/auth/register", payload, nil)

	assert.Equal(t, http.StatusConflict, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "Email already in use")
}

func TestRegister_DuplicateUsername(t *testing.T) {
	ts := setupTS(t)

	ts.DoRequest("POST", "/api/v1/auth/register", registerPayload(), nil)

	payload := registerPayload()
	payload["email"] = "different@test.concord.chat"
	w := ts.DoRequest("POST", "/api/v1/auth/register", payload, nil)

	assert.Equal(t, http.StatusConflict, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "Username already taken")
}

func TestRegister_WeakPassword(t *testing.T) {
	ts := setupTS(t)

	payload := registerPayload()
	payload["password"] = "weak"
	w := ts.DoRequest("POST", "/api/v1/auth/register", payload, nil)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRegister_MissingE2EEKeys(t *testing.T) {
	ts := setupTS(t)

	payload := map[string]interface{}{
		"email":            "nokeys@test.concord.chat",
		"username":         "nokeysuser",
		"password":         "TestPassword123!",
		"age_confirmation": true,
	}
	w := ts.DoRequest("POST", "/api/v1/auth/register", payload, nil)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRegister_NoAgeConfirmation(t *testing.T) {
	ts := setupTS(t)

	payload := registerPayload()
	payload["age_confirmation"] = false
	w := ts.DoRequest("POST", "/api/v1/auth/register", payload, nil)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRegister_SetsRefreshCookie(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("POST", "/api/v1/auth/register", registerPayload(), nil)

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

func TestLogin_Success(t *testing.T) {
	ts := setupTS(t)

	// Register first
	ts.DoRequest("POST", "/api/v1/auth/register", registerPayload(), nil)

	// Login
	w := ts.DoRequest("POST", "/api/v1/auth/login", map[string]interface{}{
		"email":    "newuser@test.concord.chat",
		"password": "TestPassword123!",
	}, nil)

	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.NotEmpty(t, body["access_token"])
	assert.NotNil(t, body["user"])
	assert.NotNil(t, body["e2ee_keys"])
}

func TestLogin_WrongPassword(t *testing.T) {
	ts := setupTS(t)

	ts.DoRequest("POST", "/api/v1/auth/register", registerPayload(), nil)

	w := ts.DoRequest("POST", "/api/v1/auth/login", map[string]interface{}{
		"email":    "newuser@test.concord.chat",
		"password": "WrongPassword123!",
	}, nil)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestLogin_NonexistentEmail(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", "/api/v1/auth/login", map[string]interface{}{
		"email":    "nonexistent@test.concord.chat",
		"password": "TestPassword123!",
	}, nil)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// --- Logout Tests ---

func TestLogout_Success(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "logoutuser")

	w := ts.DoRequest("POST", "/api/v1/auth/logout", nil, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestLogout_BlacklistsToken(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "logoutuser2")

	// Logout
	ts.DoRequest("POST", "/api/v1/auth/logout", nil, testhelpers.AuthHeaders(user.AccessToken))

	// Try to use the same token — should fail
	w := ts.DoRequest("GET", "/api/v1/users/me", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// --- Protected Route Tests ---

func TestProtectedRoute_NoToken(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", "/api/v1/users/me", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestProtectedRoute_InvalidToken(t *testing.T) {
	ts := setupTS(t)

	headers := http.Header{}
	headers.Set("Authorization", "Bearer invalid.token.here")
	w := ts.DoRequest("GET", "/api/v1/users/me", nil, headers)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestProtectedRoute_ValidToken(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "validuser")

	w := ts.DoRequest("GET", "/api/v1/users/me", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	userData := body["user"].(map[string]interface{})
	assert.Equal(t, user.Username, userData["username"])
}

// --- Refresh Tests ---

func TestRefresh_Success(t *testing.T) {
	ts := setupTS(t)

	// Register to get a refresh cookie
	w := ts.DoRequest("POST", "/api/v1/auth/register", registerPayload(), nil)
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
	req := httptest.NewRequest("POST", "/api/v1/auth/refresh", nil)
	req.AddCookie(refreshCookie)
	req.Header.Set("Content-Type", "application/json")

	rw := httptest.NewRecorder()
	ts.Router.ServeHTTP(rw, req)

	assert.Equal(t, http.StatusOK, rw.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, rw, &body)
	assert.NotEmpty(t, body["access_token"])
	assert.Equal(t, float64(900), body["expires_in"])
}

func TestRefresh_NoCookie(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", "/api/v1/auth/refresh", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRefresh_InvalidToken(t *testing.T) {
	ts := setupTS(t)

	req := httptest.NewRequest("POST", "/api/v1/auth/refresh", nil)
	// nosemgrep: go.lang.security.audit.net.cookie-missing-httponly.cookie-missing-httponly, go.lang.security.audit.net.cookie-missing-secure.cookie-missing-secure
	req.AddCookie(&http.Cookie{Name: "refresh_token", Value: "totally-invalid-token"})
	req.Header.Set("Content-Type", "application/json")

	rw := httptest.NewRecorder()
	ts.Router.ServeHTTP(rw, req)

	assert.Equal(t, http.StatusUnauthorized, rw.Code)
}

func TestRefresh_RotatesToken(t *testing.T) {
	ts := setupTS(t)

	// Register
	w := ts.DoRequest("POST", "/api/v1/auth/register", registerPayload(), nil)
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
	req := httptest.NewRequest("POST", "/api/v1/auth/refresh", nil)
	req.AddCookie(firstCookie)
	req.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	ts.Router.ServeHTTP(rw, req)
	require.Equal(t, http.StatusOK, rw.Code)

	// Replay old token — should fail (breach detection)
	req2 := httptest.NewRequest("POST", "/api/v1/auth/refresh", nil)
	req2.AddCookie(firstCookie)
	req2.Header.Set("Content-Type", "application/json")
	rw2 := httptest.NewRecorder()
	ts.Router.ServeHTTP(rw2, req2)
	assert.Equal(t, http.StatusUnauthorized, rw2.Code)
}

// --- WS Ticket Tests ---

func TestIssueWSTicket_Success(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "wsticketuser")

	w := ts.DoRequest("POST", "/api/v1/auth/ws-ticket", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.NotEmpty(t, body["ticket"])
}

func TestIssueWSTicket_Unauthorized(t *testing.T) {
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
