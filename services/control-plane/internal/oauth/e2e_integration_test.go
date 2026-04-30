package oauth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSSO_EndToEnd_NewUserScenario walks all three SSO endpoints (Initiate,
// Callback, CompleteRegistration) for a brand-new user end-to-end through a
// real Postgres + Redis stack and a fake Google provider. It then verifies
// the resulting database invariants — covering spec §11 acceptance criteria
// for the new-user (Scenario A) flow including:
//
//   - users.email = provider-asserted email
//   - users.email_verified = TRUE (Google asserted email_verified)
//   - users.password_login_disabled = TRUE (default for SSO-registered users) // pragma: allowlist secret
//   - users.trust_sso_security = FALSE (secure default)
//   - exactly one user_sso_identities row linking the (provider, sub) pair
//
// The test bypasses the browser-redirect step (which is impossible to drive
// in-process) by manually rewriting the in-Redis state record's nonce field
// to match the JWT's nonce claim before invoking Callback. In production the
// renderer would have driven the user through Google's UI to the loopback
// callback, which would carry the same nonce by virtue of the AuthorizationURL
// having included it in the original outbound URL.
func TestSSO_EndToEnd_NewUserScenario(t *testing.T) {
	now := time.Now()
	server, _, _ := fakeGoogle(t, jwt.MapClaims{
		"iss":            "https://accounts.google.com",
		"aud":            "test-client.apps.googleusercontent.com",
		"sub":            "google-sub-e2e",
		"email":          "e2e@example.test",
		"email_verified": true,
		"name":           "End To End",
		"picture":        "https://lh3.googleusercontent.com/a/e2e",
		"nonce":          "nonce-e2e",
		"iat":            now.Unix(),
		"exp":            now.Add(time.Hour).Unix(),
	})
	defer server.Close()

	rig := newSSOCallbackTestRig(t, server.URL)
	rig.Engine.POST("/api/v1/auth/sso/:provider/complete-registration", rig.Handler.CompleteRegistration)

	// Step 1 — Initiate: GET the SSO start endpoint, capture the state value
	// that the handler stored in Redis with a fresh nonce + code_verifier.
	initReq := httptest.NewRequest(http.MethodGet,
		"/api/v1/auth/sso/google?redirect_uri=http%3A%2F%2F127.0.0.1%3A65000%2Foauth%2Fcallback", nil)
	w1 := httptest.NewRecorder()
	rig.Engine.ServeHTTP(w1, initReq)
	require.Equal(t, http.StatusOK, w1.Code)
	var initResp struct {
		AuthURL string `json:"auth_url"`
		State   string `json:"state"`
	}
	require.NoError(t, json.NewDecoder(w1.Body).Decode(&initResp))
	require.NotEmpty(t, initResp.State)

	// Bypass the browser redirect by swapping the in-Redis nonce to match
	// the JWT's nonce claim. (In production, the renderer would have driven
	// the user through Google's UI to the loopback callback, and the nonce
	// returned in the id_token would equal the one Initiate stored.)
	stateKey := "sso_state:" + initResp.State
	rec, err := rig.Redis.Get(context.Background(), stateKey).Bytes()
	require.NoError(t, err)
	var recMap map[string]any
	require.NoError(t, json.Unmarshal(rec, &recMap))
	recMap["nonce"] = "nonce-e2e"
	updated, err := json.Marshal(recMap)
	require.NoError(t, err)
	require.NoError(t, rig.Redis.Set(context.Background(), stateKey, updated, 10*time.Minute).Err())

	// Step 2 — Callback: POST the (code, state) pair. The handler exchanges
	// with the fake Google provider, verifies the id_token, and (since this
	// is a brand-new user) returns sso_registration_required + opaque sso_token.
	cbReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/sso/google/callback",
		strings.NewReader(`{"code":"good-code","state":"`+initResp.State+`"}`))
	cbReq.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	rig.Engine.ServeHTTP(w2, cbReq)
	require.Equal(t, http.StatusOK, w2.Code)
	var cbResp map[string]any
	require.NoError(t, json.NewDecoder(w2.Body).Decode(&cbResp))
	require.Equal(t, true, cbResp["sso_registration_required"])
	ssoToken, ok := cbResp["sso_token"].(string)
	require.True(t, ok, "sso_token must be a string")
	require.NotEmpty(t, ssoToken)

	// Step 3 — CompleteRegistration: POST username/passphrase/wrapped key
	// material. The handler consumes the sso_token, hashes the passphrase,
	// inserts users + user_keys + public_keys + user_sso_identities rows
	// inside a single transaction, and returns an access_token.
	wrap, salt, pub := generateFakeKeyMaterial(t)
	body := map[string]any{
		"sso_token":           ssoToken,
		"username":            "e2etestuser",
		"password":            "EndToEndPassphrase!12", // pragma: allowlist secret
		"wrapped_private_key": wrap,
		"key_derivation_salt": salt,
		"public_key":          pub,
	}
	bodyJSON, err := json.Marshal(body)
	require.NoError(t, err)
	finReq := httptest.NewRequest(http.MethodPost,
		"/api/v1/auth/sso/google/complete-registration", strings.NewReader(string(bodyJSON)))
	finReq.Header.Set("Content-Type", "application/json")
	w3 := httptest.NewRecorder()
	rig.Engine.ServeHTTP(w3, finReq)
	require.Equal(t, http.StatusCreated, w3.Code)
	var finResp map[string]any
	require.NoError(t, json.NewDecoder(w3.Body).Decode(&finResp))
	assert.NotEmpty(t, finResp["access_token"], "CompleteRegistration must return an access_token")

	// Step 4 — Verify all DB invariants in a single query. Each assertion
	// maps to a checklist item from spec §11 acceptance criteria.
	var (
		email                 string
		emailVerified         bool
		passwordLoginDisabled bool
		trustSSOSecurity      bool
		ssoCount              int
	)
	require.NoError(t, rig.DB.QueryRow(`
		SELECT u.email, u.email_verified, u.password_login_disabled, u.trust_sso_security,
		       (SELECT COUNT(*) FROM user_sso_identities WHERE user_id = u.id) AS sso_count
		FROM users u WHERE u.username = $1`, "e2etestuser").Scan(
		&email, &emailVerified, &passwordLoginDisabled, &trustSSOSecurity, &ssoCount,
	))
	assert.Equal(t, "e2e@example.test", email, "users.email must equal provider-asserted email")
	assert.True(t, emailVerified, "email_verified should be TRUE for SSO-registered users")
	assert.True(t, passwordLoginDisabled, // pragma: allowlist secret
		"password_login_disabled should default TRUE for SSO-registered users")
	assert.False(t, trustSSOSecurity, "trust_sso_security should default FALSE")
	assert.Equal(t, 1, ssoCount, "exactly one user_sso_identities row")
}

// generateFakeKeyMaterial returns base64-encoded stub key material sufficient
// to satisfy CompleteRegistration's binding tags and DB INSERT. The bytes are
// not cryptographically meaningful — the handler only base64-decodes and
// stores them. A real RSA-4096 key is generated for the public_key field so
// the size budget mirrors production, but the wrapped private key is just a
// 32-byte literal.
func generateFakeKeyMaterial(t *testing.T) (wrap, salt, pub string) {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 4096)
	require.NoError(t, err)
	pubBytes, err := json.Marshal(rsaKey.PublicKey)
	require.NoError(t, err)
	pub = base64.StdEncoding.EncodeToString(pubBytes)
	wrap = base64.StdEncoding.EncodeToString([]byte("fake-wrapped-private-key-bytes-32"))
	saltBytes := make([]byte, 16)
	_, err = rand.Read(saltBytes)
	require.NoError(t, err)
	salt = base64.StdEncoding.EncodeToString(saltBytes)
	return
}
