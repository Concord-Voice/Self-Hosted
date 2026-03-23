package sessions_test

import (
	"bytes"
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

// Helper: register a user via the API, auto-verify email, and return access token + refresh cookie.
func registerAndGetTokens(t *testing.T, ts *testhelpers.TestServer, email, username string) (string, *http.Cookie) {
	t.Helper()
	pub, priv, salt := testhelpers.E2EETestKeys()
	w := ts.DoRequest("POST", "/api/v1/auth/register", map[string]interface{}{
		"email":               email,
		"username":            username,
		"password":            "TestPassword123!",
		"age_confirmation":    true,
		"public_key":          pub,
		"wrapped_private_key": priv,
		"key_derivation_salt": salt,
	}, nil)
	require.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)

	// Extract user ID and auto-verify email so tests can hit verified-only endpoints
	user := body["user"].(map[string]interface{})
	userID := user["id"].(string)
	accessToken := ts.VerifyUserEmail(t, userID)

	var refreshCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "refresh_token" {
			refreshCookie = c
			break
		}
	}
	return accessToken, refreshCookie
}

// --- List Sessions ---

func TestListSessionsSuccess(t *testing.T) {
	ts := setupTS(t)
	accessToken, refreshCookie := registerAndGetTokens(t, ts, "sessions1@test.concord.chat", "sessions1")

	// Build request with both auth header and refresh cookie
	req := httptest.NewRequest("GET", "/api/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if refreshCookie != nil {
		req.AddCookie(refreshCookie)
	}

	rw := httptest.NewRecorder()
	ts.Router.ServeHTTP(rw, req)

	assert.Equal(t, http.StatusOK, rw.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, rw, &body)
	sessions := body["sessions"].([]interface{})
	assert.GreaterOrEqual(t, len(sessions), 1)
	assert.NotNil(t, body["total"])
}

func TestListSessionsUnauthorized(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", "/api/v1/sessions", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// --- Revoke Session ---

func TestRevokeSessionSuccess(t *testing.T) {
	ts := setupTS(t)
	accessToken, refreshCookie := registerAndGetTokens(t, ts, "revoke1@test.concord.chat", "revoke1")

	// List sessions to get the session ID
	req := httptest.NewRequest("GET", "/api/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if refreshCookie != nil {
		req.AddCookie(refreshCookie)
	}
	rw := httptest.NewRecorder()
	ts.Router.ServeHTTP(rw, req)
	require.Equal(t, http.StatusOK, rw.Code)

	var listBody map[string]interface{}
	testhelpers.ParseJSON(t, rw, &listBody)
	sessions := listBody["sessions"].([]interface{})
	require.GreaterOrEqual(t, len(sessions), 1)
	sessionID := sessions[0].(map[string]interface{})["id"].(string)

	// Revoke that session (password required when < 3 active sessions)
	w := ts.DoRequest("DELETE", "/api/v1/sessions/"+sessionID, map[string]interface{}{
		"password": "TestPassword123!",
	}, testhelpers.AuthHeaders(accessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Session revoked successfully", body["message"])
}

func TestRevokeSessionNotFound(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "revokenotfound")

	w := ts.DoRequest("DELETE", "/api/v1/sessions/00000000-0000-0000-0000-000000000000", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// --- Revoke All Sessions ---

func TestRevokeAllSessionsSuccess(t *testing.T) {
	ts := setupTS(t)
	accessToken, _ := registerAndGetTokens(t, ts, "revokeall@test.concord.chat", "revokeall")

	w := ts.DoRequest("POST", "/api/v1/sessions/revoke-all", map[string]interface{}{
		"password":        "TestPassword123!",
		"include_current": true,
	}, testhelpers.AuthHeaders(accessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "All sessions revoked successfully", body["message"])
	assert.Equal(t, true, body["include_current"])
}

func TestRevokeAllSessionsExceptCurrent(t *testing.T) {
	ts := setupTS(t)
	accessToken, refreshCookie := registerAndGetTokens(t, ts, "revokeother@test.concord.chat", "revokeother")

	// Revoke all except current (need cookie for "current" identification)
	jsonBytes, _ := json.Marshal(map[string]interface{}{
		"password": "TestPassword123!",
	})
	req := httptest.NewRequest("POST", "/api/v1/sessions/revoke-all", bytes.NewReader(jsonBytes))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	if refreshCookie != nil {
		req.AddCookie(refreshCookie)
	}

	rw := httptest.NewRecorder()
	ts.Router.ServeHTTP(rw, req)

	assert.Equal(t, http.StatusOK, rw.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, rw, &body)
	assert.Equal(t, "All other sessions revoked successfully", body["message"])
}
