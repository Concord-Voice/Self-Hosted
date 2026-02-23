package users_test

import (
	"net/http"
	"testing"

	"github.com/concord/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
)

func setupTS(t *testing.T) *testhelpers.TestServer {
	t.Helper()
	return testhelpers.SetupTestServer(t)
}

// --- Get Me ---

func TestGetMe_Success(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "meuser")

	w := ts.DoRequest("GET", "/api/v1/users/me", nil, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	userData := body["user"].(map[string]interface{})
	assert.Equal(t, user.Username, userData["username"])
	assert.Equal(t, user.Email, userData["email"])
}

func TestGetMe_Unauthorized(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", "/api/v1/users/me", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// --- Update Me ---

func TestUpdateMe_Success(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updateme")

	w := ts.DoRequest("PATCH", "/api/v1/users/me", map[string]interface{}{
		"display_name": "Updated Name",
		"bio":          "My test bio",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	userData := body["user"].(map[string]interface{})
	assert.Equal(t, "Updated Name", userData["display_name"])
	assert.Equal(t, "My test bio", userData["bio"])
}

func TestUpdateMe_InvalidAvatar(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "badavatar")

	w := ts.DoRequest("PATCH", "/api/v1/users/me", map[string]interface{}{
		"avatar_url": "not-a-data-url",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- Get My Keys ---

func TestGetMyKeys_Success(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "keysuser")

	w := ts.DoRequest("GET", "/api/v1/users/me/keys", nil, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	keys := body["e2ee_keys"].(map[string]interface{})
	assert.NotEmpty(t, keys["wrapped_private_key"])
	assert.NotEmpty(t, keys["key_derivation_salt"])
}

// --- Get Public Key ---

func TestGetPublicKey_Success(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "pubkeyuser")

	w := ts.DoRequest("GET", "/api/v1/users/"+user.ID+"/public-key", nil, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.NotEmpty(t, body["public_key"])
}

func TestGetPublicKey_NonexistentUser(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "pubkeylookup")

	w := ts.DoRequest("GET", "/api/v1/users/00000000-0000-0000-0000-000000000000/public-key", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// --- Preferences ---

func TestGetPreferences_Empty(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "prefuser")

	w := ts.DoRequest("GET", "/api/v1/users/me/preferences", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdatePreferences_Success(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "prefupdate")

	w := ts.DoRequest("PUT", "/api/v1/users/me/preferences", map[string]interface{}{
		"encrypted_data": "dGVzdCBwcmVmZXJlbmNlcw==",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
}
