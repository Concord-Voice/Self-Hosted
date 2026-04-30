package oauth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/oauth"
)

func newTestGoogleProvider(t *testing.T) *oauth.GoogleProvider {
	t.Helper()
	p, err := oauth.NewGoogleProvider(oauth.GoogleConfig{
		ClientID:     "test-client.apps.googleusercontent.com",
		ClientSecret: "test-secret", //nolint:gosec // test fixture, not a real secret
		RedirectURI:  "http://127.0.0.1:54321/oauth/callback",
	})
	require.NoError(t, err)
	return p
}

func TestGoogleProvider_Name(t *testing.T) {
	p := newTestGoogleProvider(t)
	assert.Equal(t, "google", p.Name())
}

func TestGoogleProvider_AuthorizationURL_Shape(t *testing.T) {
	p := newTestGoogleProvider(t)
	u := p.AuthorizationURL("state-abc", "nonce-xyz", "challenge-pkce")

	parsed, err := url.Parse(u)
	require.NoError(t, err)
	assert.Equal(t, "https", parsed.Scheme)
	assert.Equal(t, "accounts.google.com", parsed.Host)
	assert.Equal(t, "/o/oauth2/v2/auth", parsed.Path)

	q := parsed.Query()
	assert.Equal(t, "test-client.apps.googleusercontent.com", q.Get("client_id"))
	assert.Equal(t, "http://127.0.0.1:54321/oauth/callback", q.Get("redirect_uri"))
	assert.Equal(t, "code", q.Get("response_type"))
	assert.Equal(t, "openid email profile", q.Get("scope"))
	assert.Equal(t, "state-abc", q.Get("state"))
	assert.Equal(t, "nonce-xyz", q.Get("nonce"))
	assert.Equal(t, "challenge-pkce", q.Get("code_challenge"))
	assert.Equal(t, "S256", q.Get("code_challenge_method"))
	assert.Equal(t, "select_account", q.Get("prompt"))
}

func TestNewGoogleProvider_RejectsEmptyClientID(t *testing.T) {
	_, err := oauth.NewGoogleProvider(oauth.GoogleConfig{
		ClientID:     "",
		ClientSecret: "secret", //nolint:gosec // test fixture, not a real secret
		RedirectURI:  "http://127.0.0.1/cb",
	})
	require.Error(t, err)
}

func TestNewGoogleProvider_RejectsEmptyClientSecret(t *testing.T) {
	_, err := oauth.NewGoogleProvider(oauth.GoogleConfig{
		ClientID:     "id",
		ClientSecret: "",
		RedirectURI:  "http://127.0.0.1/cb",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ClientSecret")
}

func TestNewGoogleProvider_RejectsEmptyRedirectURI(t *testing.T) {
	_, err := oauth.NewGoogleProvider(oauth.GoogleConfig{
		ClientID:     "id",
		ClientSecret: "s", //nolint:gosec // test fixture, not a real secret
		RedirectURI:  "",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RedirectURI")
}

// fakeGoogle returns an httptest.Server that mimics Google's /token + /jwks endpoints.
// Returns the server, the signing key, and the kid the JWKS publishes.
func fakeGoogle(t *testing.T, claims jwt.MapClaims) (*httptest.Server, *rsa.PrivateKey, string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	kid := "test-kid-1"

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm()) // #nosec G120 -- test fixture, body bounded by httptest client
		require.NotEmpty(t, r.PostForm.Get("code"))
		require.NotEmpty(t, r.PostForm.Get("code_verifier"))

		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		token.Header["kid"] = kid
		signed, err := token.SignedString(priv)
		require.NoError(t, err)

		_ = json.NewEncoder(w).Encode(map[string]any{
			"id_token":     signed,
			"access_token": "fake-access-token",
			"expires_in":   3600,
			"token_type":   "Bearer",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(priv.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x01})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA", "alg": "RS256", "use": "sig", "kid": kid, "n": n, "e": e,
			}},
		})
	})
	return httptest.NewServer(mux), priv, kid
}

func TestGoogleProvider_Exchange_Happy(t *testing.T) {
	now := time.Now()
	server, _, _ := fakeGoogle(t, jwt.MapClaims{
		"iss":            "https://accounts.google.com",
		"aud":            "test-client.apps.googleusercontent.com",
		"sub":            "111122223333",
		"email":          "alice@example.test",
		"email_verified": true,
		"name":           "Alice Tester",
		"picture":        "https://lh3.googleusercontent.com/a/alice",
		"nonce":          "expected-nonce",
		"iat":            now.Unix(),
		"exp":            now.Add(time.Hour).Unix(),
	})
	defer server.Close()

	p, err := oauth.NewGoogleProvider(oauth.GoogleConfig{
		ClientID:      "test-client.apps.googleusercontent.com",
		ClientSecret:  "test-secret", //nolint:gosec // test fixture, not a real secret
		RedirectURI:   "http://127.0.0.1:54321/oauth/callback",
		TokenEndpoint: server.URL + "/token",
		JWKSEndpoint:  server.URL + "/jwks",
		Issuer:        "https://accounts.google.com",
	})
	require.NoError(t, err)

	info, err := p.Exchange(context.Background(), oauth.ExchangeParams{
		Code:         "fake-code",
		CodeVerifier: "fake-verifier",
		Nonce:        "expected-nonce",
	})
	require.NoError(t, err)
	assert.Equal(t, "google", info.Provider)
	assert.Equal(t, "111122223333", info.ProviderUserID)
	assert.Equal(t, "alice@example.test", info.Email)
	assert.True(t, info.EmailVerified)
	assert.Equal(t, "Alice Tester", info.Name)
	assert.Equal(t, "https://lh3.googleusercontent.com/a/alice", info.AvatarURL)
	assert.False(t, info.IsRelayEmail)
}

func TestGoogleProvider_Exchange_NonceMismatch(t *testing.T) {
	now := time.Now()
	server, _, _ := fakeGoogle(t, jwt.MapClaims{
		"iss": "https://accounts.google.com", "aud": "test-client.apps.googleusercontent.com",
		"sub": "x", "email": "x@x", "email_verified": true,
		"nonce": "wrong-nonce",
		"iat":   now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	defer server.Close()

	p, _ := oauth.NewGoogleProvider(oauth.GoogleConfig{
		ClientID: "test-client.apps.googleusercontent.com", ClientSecret: "s", RedirectURI: "http://127.0.0.1/cb", //nolint:gosec // test fixture, not a real secret
		TokenEndpoint: server.URL + "/token", JWKSEndpoint: server.URL + "/jwks", Issuer: "https://accounts.google.com",
	})
	_, err := p.Exchange(context.Background(), oauth.ExchangeParams{
		Code: "c", CodeVerifier: "v", Nonce: "expected-nonce",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nonce")
}

func TestGoogleProvider_Exchange_AudMismatch(t *testing.T) {
	now := time.Now()
	server, _, _ := fakeGoogle(t, jwt.MapClaims{
		"iss": "https://accounts.google.com", "aud": "wrong-aud",
		"sub": "x", "email": "x@x", "email_verified": true,
		"nonce": "n",
		"iat":   now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	defer server.Close()

	p, _ := oauth.NewGoogleProvider(oauth.GoogleConfig{
		ClientID: "test-client.apps.googleusercontent.com", ClientSecret: "s", RedirectURI: "http://127.0.0.1/cb", //nolint:gosec // test fixture, not a real secret
		TokenEndpoint: server.URL + "/token", JWKSEndpoint: server.URL + "/jwks", Issuer: "https://accounts.google.com",
	})
	_, err := p.Exchange(context.Background(), oauth.ExchangeParams{
		Code: "c", CodeVerifier: "v", Nonce: "n",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "aud")
}

func TestGoogleProvider_Exchange_Expired(t *testing.T) {
	server, _, _ := fakeGoogle(t, jwt.MapClaims{
		"iss": "https://accounts.google.com", "aud": "test-client.apps.googleusercontent.com",
		"sub": "x", "email": "x@x", "email_verified": true, "nonce": "n",
		"iat": time.Now().Add(-2 * time.Hour).Unix(),
		"exp": time.Now().Add(-1 * time.Hour).Unix(),
	})
	defer server.Close()

	p, _ := oauth.NewGoogleProvider(oauth.GoogleConfig{
		ClientID: "test-client.apps.googleusercontent.com", ClientSecret: "s", RedirectURI: "http://127.0.0.1/cb", //nolint:gosec // test fixture, not a real secret
		TokenEndpoint: server.URL + "/token", JWKSEndpoint: server.URL + "/jwks", Issuer: "https://accounts.google.com",
	})
	_, err := p.Exchange(context.Background(), oauth.ExchangeParams{Code: "c", CodeVerifier: "v", Nonce: "n"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exp")
}

// TestGoogleProvider_Exchange_PerRequestRedirectURI pins the RFC 6749 §4.1.3
// contract: when ExchangeParams.RedirectURI is non-empty, the provider MUST
// send that value (not the constructor seed) as redirect_uri at the /token
// endpoint. Concord's renderer chooses an ephemeral loopback port at runtime,
// so the seed (port :0 placeholder) and the per-request URI diverge — sending
// the wrong one elicits redirect_uri_mismatch from real Google.
func TestGoogleProvider_Exchange_PerRequestRedirectURI(t *testing.T) {
	const (
		seedRedirect     = "http://127.0.0.1:0/oauth/callback"
		perReqRedirect   = "http://127.0.0.1:54321/oauth/callback"
		expectedRedirect = "http://127.0.0.1:54321/oauth/callback"
	)

	now := time.Now()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	kid := "test-kid-perreq"

	var observedRedirect string

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm()) // #nosec G120 -- test fixture, body bounded by httptest client
		observedRedirect = r.PostForm.Get("redirect_uri")

		token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
			"iss":            "https://accounts.google.com",
			"aud":            "test-client.apps.googleusercontent.com",
			"sub":            "perreq-sub",
			"email":          "perreq@example.test",
			"email_verified": true,
			"nonce":          "perreq-nonce",
			"iat":            now.Unix(),
			"exp":            now.Add(time.Hour).Unix(),
		})
		token.Header["kid"] = kid
		signed, err := token.SignedString(priv)
		require.NoError(t, err)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id_token":     signed,
			"access_token": "fake-access-token",
			"expires_in":   3600,
			"token_type":   "Bearer",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(priv.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x01})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA", "alg": "RS256", "use": "sig", "kid": kid, "n": n, "e": e,
			}},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	p, err := oauth.NewGoogleProvider(oauth.GoogleConfig{
		ClientID:      "test-client.apps.googleusercontent.com",
		ClientSecret:  "test-secret", //nolint:gosec // test fixture, not a real secret
		RedirectURI:   seedRedirect,  // seed — must NOT be the value sent at /token
		TokenEndpoint: server.URL + "/token",
		JWKSEndpoint:  server.URL + "/jwks",
		Issuer:        "https://accounts.google.com",
	})
	require.NoError(t, err)

	info, err := p.Exchange(context.Background(), oauth.ExchangeParams{
		Code:         "fake-code",
		CodeVerifier: "fake-verifier",
		Nonce:        "perreq-nonce",
		RedirectURI:  perReqRedirect, // per-request override
	})
	require.NoError(t, err)
	require.NotNil(t, info)
	assert.Equal(t, "perreq-sub", info.ProviderUserID)

	// Load-bearing assertion: the /token request carried the per-request
	// redirect_uri, NOT the constructor seed. Without this, Google rejects
	// the exchange under RFC 6749 §4.1.3 with redirect_uri_mismatch.
	assert.Equal(t, expectedRedirect, observedRedirect,
		"redirect_uri at /token must be the per-request value, not the seed")
	assert.NotEqual(t, seedRedirect, observedRedirect,
		"redirect_uri at /token must NOT be the constructor seed when ExchangeParams.RedirectURI is set")
}

// TestGoogleProvider_Exchange_FallsBackToSeedRedirectURI documents the
// fallback contract for ExchangeParams.RedirectURI == "": the provider sends
// the constructor seed. This branch keeps existing tests (which omit the
// param) working and gives the production handler a safe default if a future
// caller forgets to set it.
func TestGoogleProvider_Exchange_FallsBackToSeedRedirectURI(t *testing.T) {
	const seedRedirect = "http://127.0.0.1:99999/seed-only/cb"

	now := time.Now()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	kid := "test-kid-fallback"

	var observedRedirect string

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm()) // #nosec G120 -- test fixture, body bounded by httptest client
		observedRedirect = r.PostForm.Get("redirect_uri")

		token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
			"iss":            "https://accounts.google.com",
			"aud":            "test-client.apps.googleusercontent.com",
			"sub":            "fallback-sub",
			"email":          "fallback@example.test",
			"email_verified": true,
			"nonce":          "fallback-nonce",
			"iat":            now.Unix(),
			"exp":            now.Add(time.Hour).Unix(),
		})
		token.Header["kid"] = kid
		signed, err := token.SignedString(priv)
		require.NoError(t, err)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id_token":   signed,
			"token_type": "Bearer",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(priv.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x01})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA", "alg": "RS256", "use": "sig", "kid": kid, "n": n, "e": e,
			}},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	p, err := oauth.NewGoogleProvider(oauth.GoogleConfig{
		ClientID:      "test-client.apps.googleusercontent.com",
		ClientSecret:  "test-secret", //nolint:gosec // test fixture, not a real secret
		RedirectURI:   seedRedirect,
		TokenEndpoint: server.URL + "/token",
		JWKSEndpoint:  server.URL + "/jwks",
		Issuer:        "https://accounts.google.com",
	})
	require.NoError(t, err)

	_, err = p.Exchange(context.Background(), oauth.ExchangeParams{
		Code:         "fake-code",
		CodeVerifier: "fake-verifier",
		Nonce:        "fallback-nonce",
		// RedirectURI deliberately empty — exercises the fallback branch.
	})
	require.NoError(t, err)
	assert.Equal(t, seedRedirect, observedRedirect,
		"empty ExchangeParams.RedirectURI must fall back to the constructor seed")
}
