package users_test

import (
	"net/http"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
)

const (
	urlUsersMe         = "/api/v1/users/me"
	urlUsersMeKeys     = "/api/v1/users/me/keys"
	urlUsersMePassword = "/api/v1/users/me/password" //nolint:gosec // G101 false positive: URL path constant, not a credential
)

func setupTS(t *testing.T) *testhelpers.TestServer {
	t.Helper()
	return testhelpers.SetupTestServer(t)
}

// --- Get Me ---

func TestGetMeSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "meuser")

	w := ts.DoRequest("GET", urlUsersMe, nil, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	userData := body["user"].(map[string]interface{})
	assert.Equal(t, user.Username, userData["username"])
	assert.Equal(t, user.Email, userData["email"])
}

func TestGetMeUnauthorized(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", urlUsersMe, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// --- Update Me ---

func TestUpdateMeSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updateme")

	w := ts.DoRequest("PATCH", urlUsersMe, map[string]interface{}{
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

func TestUpdateMeInvalidAvatar(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "badavatar")

	w := ts.DoRequest("PATCH", urlUsersMe, map[string]interface{}{
		"avatar_url": "not-a-data-url",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- Get My Keys ---

func TestGetMyKeysSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "keysuser")

	w := ts.DoRequest("GET", urlUsersMeKeys, nil, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	keys := body["e2ee_keys"].(map[string]interface{})
	assert.NotEmpty(t, keys["wrapped_private_key"])
	assert.NotEmpty(t, keys["key_derivation_salt"])
}

// --- Get Public Key ---

func TestGetPublicKeySuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "pubkeyuser")

	w := ts.DoRequest("GET", "/api/v1/users/"+user.ID+"/public-key", nil, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.NotEmpty(t, body["public_key"])
}

func TestGetPublicKeyNonexistentUser(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "pubkeylookup")

	w := ts.DoRequest("GET", "/api/v1/users/00000000-0000-0000-0000-000000000000/public-key", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// --- Preferences ---

func TestGetPreferencesEmpty(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "prefuser")

	w := ts.DoRequest("GET", "/api/v1/users/me/preferences", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdatePreferencesSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "prefupdate")

	w := ts.DoRequest("PUT", "/api/v1/users/me/preferences", map[string]interface{}{
		"encrypted_data": "dGVzdCBwcmVmZXJlbmNlcw==",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
}

// --- Replace My Keys ---

func TestReplaceMyKeysSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "replacekeysuser")

	_, newWrapped, newSalt := testhelpers.E2EETestKeys()
	w := ts.DoRequest("PUT", urlUsersMeKeys, map[string]interface{}{
		"wrapped_private_key": newWrapped,
		"key_derivation_salt": newSalt,
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Keys replaced successfully", body["message"])
}

func TestReplaceMyKeysInvalidBase64(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "badkeysuser")

	w := ts.DoRequest("PUT", urlUsersMeKeys, map[string]interface{}{
		"wrapped_private_key": "not-valid-base64!!!",
		"key_derivation_salt": "also-bad!!!",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestReplaceMyKeysMissingFields(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "missingkeysuser")

	w := ts.DoRequest("PUT", urlUsersMeKeys, map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- Username Change Cooldown ---

func TestUpdateMeUsernameChangeCooldown(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "cooldownuser")

	// Username was just created, so username_changed_at = NOW(). Change should be blocked.
	w := ts.DoRequest("PATCH", urlUsersMe, map[string]interface{}{
		"username": "newcooldownname",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "once per year")
	assert.NotNil(t, body["username_change_eligible_at"])
}

func TestUpdateMeUsernameChangeAllowed(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "allowedchange")

	// Set username_changed_at to 366 days ago to allow change
	_, err := ts.DB.Exec(
		`UPDATE users SET username_changed_at = NOW() - INTERVAL '366 days' WHERE id = $1`,
		user.ID,
	)
	assert.NoError(t, err)

	w := ts.DoRequest("PATCH", urlUsersMe, map[string]interface{}{
		"username": "newallowedname",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	userData := body["user"].(map[string]interface{})
	assert.Equal(t, "newallowedname", userData["username"])
}

func TestUpdateMeUsernameChangeSameUsername(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "sameusername")

	// Even though on cooldown, submitting the same username should NOT trigger
	// the cooldown error — it's effectively a no-op since no change occurs.
	w := ts.DoRequest("PATCH", urlUsersMe, map[string]interface{}{
		"username": "sameusername",
	}, testhelpers.AuthHeaders(user.AccessToken))

	// The handler skips the cooldown check for no-op username submissions,
	// but since there are no actual changes, it returns 400 "No fields to update".
	assert.Equal(t, http.StatusBadRequest, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "No fields to update", body["error"])
}

func TestUpdateMeUsernameWithPeriod(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "periodtest")

	// Allow change by setting cooldown to expired
	_, err := ts.DB.Exec(
		`UPDATE users SET username_changed_at = NOW() - INTERVAL '366 days' WHERE id = $1`,
		user.ID,
	)
	assert.NoError(t, err)

	w := ts.DoRequest("PATCH", urlUsersMe, map[string]interface{}{
		"username": "period.test",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	userData := body["user"].(map[string]interface{})
	assert.Equal(t, "period.test", userData["username"])
}

// registerAndVerify registers a user via the API (so Argon2id hash is real),
// then auto-verifies email and returns a verified access token.
func registerAndVerify(t *testing.T, ts *testhelpers.TestServer, email, username string) string {
	t.Helper()
	pub, priv, salt := testhelpers.E2EETestKeys()
	regW := ts.DoRequest("POST", "/api/v1/auth/register", map[string]interface{}{
		"email":               email,
		"username":            username,
		"password":            "TestPassword123!",
		"age_confirmation":    true,
		"public_key":          pub,
		"wrapped_private_key": priv,
		"key_derivation_salt": salt,
	}, nil)
	assert.Equal(t, http.StatusCreated, regW.Code)

	var regBody map[string]interface{}
	testhelpers.ParseJSON(t, regW, &regBody)
	user := regBody["user"].(map[string]interface{})
	userID := user["id"].(string)
	return ts.VerifyUserEmail(t, userID)
}

// --- Change Password ---

func TestChangePasswordSuccess(t *testing.T) {
	ts := setupTS(t)

	// Register via API so password hash is real Argon2id, then auto-verify
	accessToken := registerAndVerify(t, ts, "changepw@test.concord.chat", "changepwuser")

	_, newWrapped, newSalt := testhelpers.E2EETestKeys()
	w := ts.DoRequest("POST", urlUsersMePassword, map[string]interface{}{
		"current_password":    "TestPassword123!",
		"new_password":        "NewStrongPassword456!",
		"wrapped_private_key": newWrapped,
		"key_derivation_salt": newSalt,
	}, testhelpers.AuthHeaders(accessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Password changed successfully", body["message"])
}

func TestChangePasswordWrongCurrentPassword(t *testing.T) {
	ts := setupTS(t)

	accessToken := registerAndVerify(t, ts, "wrongpw@test.concord.chat", "wrongpwuser")

	_, newWrapped, newSalt := testhelpers.E2EETestKeys()
	w := ts.DoRequest("POST", urlUsersMePassword, map[string]interface{}{
		"current_password":    "WrongPassword!!!!",
		"new_password":        "NewStrongPassword456!",
		"wrapped_private_key": newWrapped,
		"key_derivation_salt": newSalt,
	}, testhelpers.AuthHeaders(accessToken))

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestChangePasswordSameAsOld(t *testing.T) {
	ts := setupTS(t)

	accessToken := registerAndVerify(t, ts, "samepw@test.concord.chat", "samepwuser")

	_, newWrapped, newSalt := testhelpers.E2EETestKeys()
	w := ts.DoRequest("POST", urlUsersMePassword, map[string]interface{}{
		"current_password":    "TestPassword123!",
		"new_password":        "TestPassword123!",
		"wrapped_private_key": newWrapped,
		"key_derivation_salt": newSalt,
	}, testhelpers.AuthHeaders(accessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestChangePasswordWeakNewPassword(t *testing.T) {
	ts := setupTS(t)

	accessToken := registerAndVerify(t, ts, "weaknewpw@test.concord.chat", "weaknewpwuser")

	_, newWrapped, newSalt := testhelpers.E2EETestKeys()
	w := ts.DoRequest("POST", urlUsersMePassword, map[string]interface{}{
		"current_password":    "TestPassword123!",
		"new_password":        "weak",
		"wrapped_private_key": newWrapped,
		"key_derivation_salt": newSalt,
	}, testhelpers.AuthHeaders(accessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}
