package oauth_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/oauth"
)

const (
	testAppleClientID = "chat.concordvoice.signin"
	testAppleTeamID   = "TEAM123ABC"
	testAppleKeyID    = "KEYID12345"
	testAppleRedirect = "http://127.0.0.1:54321/oauth/callback"
)

// generateP256PEM returns a freshly-generated P-256 ECDSA private key encoded as
// PKCS8 PEM bytes — the wire format Apple's developer portal exports for .p8 files.
func generateP256PEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// generateP384PEM returns a freshly-generated P-384 ECDSA private key as PKCS8 PEM.
// Used to verify NewAppleProvider rejects non-P-256 curves.
func generateP384PEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// generateRSAPEM returns a freshly-generated RSA-2048 private key as PKCS8 PEM.
// Used to verify NewAppleProvider rejects RSA keys (Apple uses ES256).
func generateRSAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func newTestAppleProvider(t *testing.T) *oauth.AppleProvider {
	t.Helper()
	p, err := oauth.NewAppleProvider(oauth.AppleConfig{
		ClientID:    testAppleClientID,
		TeamID:      testAppleTeamID,
		KeyID:       testAppleKeyID,
		PrivateKey:  generateP256PEM(t),
		RedirectURI: testAppleRedirect,
	})
	require.NoError(t, err)
	return p
}

// fakeAppleOpts controls fakeApple's behavior. Defaults are happy-path.
type fakeAppleOpts struct {
	// signMethod overrides the JWT signing method (e.g., HS256 to test
	// algorithm-confusion defense). Default: RS256 with the generated RSA key.
	signMethod jwt.SigningMethod
	// signKey overrides the key used for signing (e.g., a shared secret for HS256).
	signKey interface{}
	// kidHeader overrides the kid in the id_token header. Default: kid value
	// returned alongside the server.
	kidHeader string
	// jwksKidOverride lets tests publish a different kid in JWKS than the one
	// signing — used for the rotation test.
	jwksKidOverride string
	// returnNoIDToken makes /auth/token return a JSON body without id_token.
	returnNoIDToken bool
	// status5xx makes /auth/token return 503.
	status5xx bool
	// captureForm exposes the parsed form body of the most recent /auth/token request.
	captureForm *url.Values
}

// fakeApple returns an httptest.Server that mimics Apple's /auth/token + /auth/keys
// endpoints. The id_token is signed with a per-server RSA key (Apple publishes RSA
// keys in JWKS); fields can be overridden via opts.
func fakeApple(t *testing.T, claims jwt.MapClaims, opts ...fakeAppleOpts) (*httptest.Server, string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	kid := "apple-kid-1"

	o := fakeAppleOpts{}
	if len(opts) > 0 {
		o = opts[0]
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/token", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm()) // #nosec G120 -- test fixture, body bounded by httptest client
		if o.captureForm != nil {
			*o.captureForm = r.PostForm
		}
		if o.status5xx {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"server_error"}`))
			return
		}
		if o.returnNoIDToken {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"no-id-token-here"}`))
			return
		}

		method := o.signMethod
		var key interface{} = priv
		if method == nil {
			method = jwt.SigningMethodRS256
		}
		if o.signKey != nil {
			key = o.signKey
		}

		token := jwt.NewWithClaims(method, claims)
		hdrKid := o.kidHeader
		if hdrKid == "" {
			hdrKid = kid
		}
		token.Header["kid"] = hdrKid
		signed, err := token.SignedString(key)
		require.NoError(t, err)

		_ = json.NewEncoder(w).Encode(map[string]any{ //nolint:gosec // test fixture, "fake-apple-access-token" literal is not a credential
			"id_token":     signed,
			"access_token": "fake-apple-access-token",
			"expires_in":   3600,
			"token_type":   "Bearer",
		})
	})
	mux.HandleFunc("/auth/keys", func(w http.ResponseWriter, _ *http.Request) {
		jwksKid := o.jwksKidOverride
		if jwksKid == "" {
			jwksKid = kid
		}
		n := base64.RawURLEncoding.EncodeToString(priv.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x01})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA", "alg": "RS256", "use": "sig", "kid": jwksKid, "n": n, "e": e,
			}},
		})
	})
	return httptest.NewServer(mux), kid
}

// --- Constructor tests ---

func TestNewAppleProvider_RequiresClientID(t *testing.T) {
	_, err := oauth.NewAppleProvider(oauth.AppleConfig{
		ClientID:    "",
		TeamID:      testAppleTeamID,
		KeyID:       testAppleKeyID,
		PrivateKey:  generateP256PEM(t),
		RedirectURI: testAppleRedirect,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ClientID")
}

func TestNewAppleProvider_RequiresTeamID(t *testing.T) {
	_, err := oauth.NewAppleProvider(oauth.AppleConfig{
		ClientID:    testAppleClientID,
		TeamID:      "",
		KeyID:       testAppleKeyID,
		PrivateKey:  generateP256PEM(t),
		RedirectURI: testAppleRedirect,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TeamID")
}

func TestNewAppleProvider_RequiresKeyID(t *testing.T) {
	_, err := oauth.NewAppleProvider(oauth.AppleConfig{
		ClientID:    testAppleClientID,
		TeamID:      testAppleTeamID,
		KeyID:       "",
		PrivateKey:  generateP256PEM(t),
		RedirectURI: testAppleRedirect,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "KeyID")
}

func TestNewAppleProvider_RequiresPrivateKey(t *testing.T) {
	_, err := oauth.NewAppleProvider(oauth.AppleConfig{
		ClientID:    testAppleClientID,
		TeamID:      testAppleTeamID,
		KeyID:       testAppleKeyID,
		PrivateKey:  nil,
		RedirectURI: testAppleRedirect,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PrivateKey")
}

func TestNewAppleProvider_RequiresRedirectURI(t *testing.T) {
	_, err := oauth.NewAppleProvider(oauth.AppleConfig{
		ClientID:    testAppleClientID,
		TeamID:      testAppleTeamID,
		KeyID:       testAppleKeyID,
		PrivateKey:  generateP256PEM(t),
		RedirectURI: "",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RedirectURI")
}

func TestNewAppleProvider_RejectsMalformedPrivateKey(t *testing.T) {
	_, err := oauth.NewAppleProvider(oauth.AppleConfig{
		ClientID:    testAppleClientID,
		TeamID:      testAppleTeamID,
		KeyID:       testAppleKeyID,
		PrivateKey:  []byte("not a pem block"),
		RedirectURI: testAppleRedirect,
	})
	require.Error(t, err)
}

func TestNewAppleProvider_RejectsRSAPrivateKey(t *testing.T) {
	_, err := oauth.NewAppleProvider(oauth.AppleConfig{
		ClientID:    testAppleClientID,
		TeamID:      testAppleTeamID,
		KeyID:       testAppleKeyID,
		PrivateKey:  generateRSAPEM(t),
		RedirectURI: testAppleRedirect,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "P-256")
}

func TestNewAppleProvider_RejectsP384Curve(t *testing.T) {
	_, err := oauth.NewAppleProvider(oauth.AppleConfig{
		ClientID:    testAppleClientID,
		TeamID:      testAppleTeamID,
		KeyID:       testAppleKeyID,
		PrivateKey:  generateP384PEM(t),
		RedirectURI: testAppleRedirect,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "P-256")
}

// --- Provider basics ---

func TestAppleProvider_Name(t *testing.T) {
	p := newTestAppleProvider(t)
	assert.Equal(t, "apple", p.Name())
}

func TestAppleProvider_AuthorizationURL_Shape(t *testing.T) {
	p := newTestAppleProvider(t)
	u := p.AuthorizationURL("state-abc", "nonce-xyz", "challenge-pkce")

	parsed, err := url.Parse(u)
	require.NoError(t, err)
	assert.Equal(t, "https", parsed.Scheme)
	assert.Equal(t, "appleid.apple.com", parsed.Host)
	assert.Equal(t, "/auth/authorize", parsed.Path)

	q := parsed.Query()
	assert.Equal(t, testAppleClientID, q.Get("client_id"))
	assert.Equal(t, testAppleRedirect, q.Get("redirect_uri"))
	assert.Equal(t, "code", q.Get("response_type"))
	assert.Equal(t, "name email", q.Get("scope"))
	assert.Equal(t, "form_post", q.Get("response_mode"))
	assert.Equal(t, "state-abc", q.Get("state"))
	assert.Equal(t, "nonce-xyz", q.Get("nonce"))
	assert.Equal(t, "challenge-pkce", q.Get("code_challenge"))
	assert.Equal(t, "S256", q.Get("code_challenge_method"))
}

// --- Exchange happy paths ---

func makeAppleClaims(now time.Time, overrides map[string]any) jwt.MapClaims {
	claims := jwt.MapClaims{
		"iss":              "https://appleid.apple.com",
		"aud":              testAppleClientID,
		"sub":              "001234.aabbccddeeff.1234",
		"email":            "jane@example.com",
		"email_verified":   true,
		"is_private_email": false,
		"nonce":            "expected-nonce",
		"nonce_supported":  true,
		"iat":              now.Unix(),
		"exp":              now.Add(time.Hour).Unix(),
	}
	for k, v := range overrides {
		claims[k] = v
	}
	return claims
}

func newAppleProviderForServer(t *testing.T, server *httptest.Server) *oauth.AppleProvider {
	t.Helper()
	p, err := oauth.NewAppleProvider(oauth.AppleConfig{
		ClientID:      testAppleClientID,
		TeamID:        testAppleTeamID,
		KeyID:         testAppleKeyID,
		PrivateKey:    generateP256PEM(t),
		RedirectURI:   testAppleRedirect,
		AuthEndpoint:  server.URL + "/auth/authorize",
		TokenEndpoint: server.URL + "/auth/token",
		JWKSEndpoint:  server.URL + "/auth/keys",
		Issuer:        "https://appleid.apple.com",
	})
	require.NoError(t, err)
	return p
}

func TestAppleProvider_Exchange_HappyPath_RealEmail(t *testing.T) {
	now := time.Now()
	server, _ := fakeApple(t, makeAppleClaims(now, nil))
	defer server.Close()

	p := newAppleProviderForServer(t, server)

	info, err := p.Exchange(context.Background(), oauth.ExchangeParams{
		Code:          "fake-code",
		CodeVerifier:  "fake-verifier",
		Nonce:         "expected-nonce",
		AppleUserData: `{"name":{"firstName":"Jane","lastName":"Doe"},"email":"jane@example.com"}`,
	})
	require.NoError(t, err)
	assert.Equal(t, "apple", info.Provider)
	assert.Equal(t, "001234.aabbccddeeff.1234", info.ProviderUserID)
	assert.Equal(t, "jane@example.com", info.Email)
	assert.True(t, info.EmailVerified)
	assert.Equal(t, "Jane Doe", info.Name)
	assert.Equal(t, "", info.AvatarURL)
	assert.False(t, info.IsRelayEmail)
}

// TestAppleProvider_Exchange_HappyPath_StringBoolClaims pins compatibility
// with Apple's historical quirk of serializing email_verified,
// is_private_email, and nonce_supported as JSON STRINGS ("true"/"false")
// rather than JSON booleans on some endpoint paths. The appleBool custom
// unmarshaler accepts either form. Without it, real Apple tokens would
// fail claim parsing with an opaque oauth_token_invalid error.
func TestAppleProvider_Exchange_HappyPath_StringBoolClaims(t *testing.T) {
	now := time.Now()
	// Override the three boolean claims with their JSON-string equivalents,
	// simulating Apple's string-form delivery.
	server, _ := fakeApple(t, makeAppleClaims(now, map[string]any{
		"email":            "opaque456@privaterelay.appleid.com",
		"email_verified":   "true",
		"is_private_email": "true",
		"nonce_supported":  "true",
	}))
	defer server.Close()

	p := newAppleProviderForServer(t, server)

	info, err := p.Exchange(context.Background(), oauth.ExchangeParams{
		Code:         "fake-code",
		CodeVerifier: "fake-verifier",
		Nonce:        "expected-nonce",
	})
	require.NoError(t, err, "string-form bool claims must parse without error")
	assert.True(t, info.EmailVerified, "string \"true\" must unmarshal to true")
	assert.True(t, info.IsRelayEmail, "string \"true\" on is_private_email must trigger relay flag")
}

// TestAppleProvider_Exchange_StringBoolFalse_NoRelay verifies the negative
// case: string-form "false" on is_private_email maps correctly to false.
func TestAppleProvider_Exchange_StringBoolFalse_NoRelay(t *testing.T) {
	now := time.Now()
	server, _ := fakeApple(t, makeAppleClaims(now, map[string]any{
		"email":            "jane@example.com",
		"email_verified":   "true",
		"is_private_email": "false",
		"nonce_supported":  "true",
	}))
	defer server.Close()

	p := newAppleProviderForServer(t, server)

	info, err := p.Exchange(context.Background(), oauth.ExchangeParams{
		Code:         "fake-code",
		CodeVerifier: "fake-verifier",
		Nonce:        "expected-nonce",
	})
	require.NoError(t, err)
	assert.True(t, info.EmailVerified)
	assert.False(t, info.IsRelayEmail, "string \"false\" + non-relay domain → IsRelayEmail false")
}

func TestAppleProvider_Exchange_HappyPath_RelayEmail_ViaDomain(t *testing.T) {
	now := time.Now()
	server, _ := fakeApple(t, makeAppleClaims(now, map[string]any{
		"email":            "opaque123@privaterelay.appleid.com",
		"is_private_email": true,
	}))
	defer server.Close()

	p := newAppleProviderForServer(t, server)

	info, err := p.Exchange(context.Background(), oauth.ExchangeParams{
		Code:         "fake-code",
		CodeVerifier: "fake-verifier",
		Nonce:        "expected-nonce",
	})
	require.NoError(t, err)
	assert.True(t, info.IsRelayEmail)
}

func TestAppleProvider_Exchange_HappyPath_RelayEmail_ViaClaimOnly(t *testing.T) {
	// Defense in depth: even if Apple sends a non-relay-domain email, the
	// is_private_email=true claim alone should mark the email as relay.
	now := time.Now()
	server, _ := fakeApple(t, makeAppleClaims(now, map[string]any{
		"email":            "weird@example.com",
		"is_private_email": true,
	}))
	defer server.Close()

	p := newAppleProviderForServer(t, server)

	info, err := p.Exchange(context.Background(), oauth.ExchangeParams{
		Code:         "fake-code",
		CodeVerifier: "fake-verifier",
		Nonce:        "expected-nonce",
	})
	require.NoError(t, err)
	assert.True(t, info.IsRelayEmail, "is_private_email=true alone must flag IsRelayEmail")
}

func TestAppleProvider_Exchange_SubsequentAuth_NoName(t *testing.T) {
	now := time.Now()
	server, _ := fakeApple(t, makeAppleClaims(now, nil))
	defer server.Close()

	p := newAppleProviderForServer(t, server)

	info, err := p.Exchange(context.Background(), oauth.ExchangeParams{
		Code:          "fake-code",
		CodeVerifier:  "fake-verifier",
		Nonce:         "expected-nonce",
		AppleUserData: "", // Subsequent auth — Apple does not send user JSON.
	})
	require.NoError(t, err)
	assert.Equal(t, "", info.Name, "subsequent-auth Name must be empty")
}

// --- Exchange error paths ---

func TestAppleProvider_Exchange_NonceMismatch(t *testing.T) {
	now := time.Now()
	server, _ := fakeApple(t, makeAppleClaims(now, map[string]any{
		"nonce": "wrong-nonce",
	}))
	defer server.Close()

	p := newAppleProviderForServer(t, server)
	_, err := p.Exchange(context.Background(), oauth.ExchangeParams{
		Code: "c", CodeVerifier: "v", Nonce: "expected-nonce",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nonce")
}

func TestAppleProvider_Exchange_NonceUnsupported_Rejected(t *testing.T) {
	// Apple has supported nonces since 2020; nonce_supported=false (or omission)
	// is treated as a non-conformant id_token and rejected.
	now := time.Now()
	server, _ := fakeApple(t, makeAppleClaims(now, map[string]any{
		"nonce_supported": false,
	}))
	defer server.Close()

	p := newAppleProviderForServer(t, server)
	_, err := p.Exchange(context.Background(), oauth.ExchangeParams{
		Code: "c", CodeVerifier: "v", Nonce: "expected-nonce",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nonce_supported")
}

func TestAppleProvider_Exchange_BadIssuer(t *testing.T) {
	now := time.Now()
	server, _ := fakeApple(t, makeAppleClaims(now, map[string]any{
		"iss": "https://impostor.example.com",
	}))
	defer server.Close()

	p := newAppleProviderForServer(t, server)
	_, err := p.Exchange(context.Background(), oauth.ExchangeParams{
		Code: "c", CodeVerifier: "v", Nonce: "expected-nonce",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "iss")
}

func TestAppleProvider_Exchange_BadAudience(t *testing.T) {
	now := time.Now()
	server, _ := fakeApple(t, makeAppleClaims(now, map[string]any{
		"aud": "another-services-id",
	}))
	defer server.Close()

	p := newAppleProviderForServer(t, server)
	_, err := p.Exchange(context.Background(), oauth.ExchangeParams{
		Code: "c", CodeVerifier: "v", Nonce: "expected-nonce",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "aud")
}

func TestAppleProvider_Exchange_Expired(t *testing.T) {
	server, _ := fakeApple(t, makeAppleClaims(time.Now(), map[string]any{
		"iat": time.Now().Add(-2 * time.Hour).Unix(),
		"exp": time.Now().Add(-1 * time.Hour).Unix(),
	}))
	defer server.Close()

	p := newAppleProviderForServer(t, server)
	_, err := p.Exchange(context.Background(), oauth.ExchangeParams{
		Code: "c", CodeVerifier: "v", Nonce: "expected-nonce",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exp")
}

// TestAppleProvider_Exchange_FutureIat_Rejected covers the iat (issued-at)
// validation gap. An id_token whose iat claim is in the future would
// otherwise pass through (jwt.WithIssuedAt is what makes the parser reject
// it). Apple's iat reflects Apple's clock, so a future iat would only
// arise from a hostile proxy / bug — defensive rejection.
func TestAppleProvider_Exchange_FutureIat_Rejected(t *testing.T) {
	now := time.Now()
	server, _ := fakeApple(t, makeAppleClaims(now, map[string]any{
		"iat": now.Add(2 * time.Hour).Unix(), // 2h in the future
		"exp": now.Add(3 * time.Hour).Unix(), // exp must be after iat
	}))
	defer server.Close()

	p := newAppleProviderForServer(t, server)
	_, err := p.Exchange(context.Background(), oauth.ExchangeParams{
		Code: "c", CodeVerifier: "v", Nonce: "expected-nonce",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "iat",
		"future iat must be rejected via jwt.WithIssuedAt parser option")
}

func TestAppleProvider_Exchange_RejectsHMACSignedToken(t *testing.T) {
	// Algorithm-confusion defense: an attacker who knows the public key (e.g.,
	// reads it from JWKS) cannot use it as an HMAC secret to forge id_tokens.
	now := time.Now()
	server, _ := fakeApple(t, makeAppleClaims(now, nil), fakeAppleOpts{
		signMethod: jwt.SigningMethodHS256,
		signKey:    []byte("attacker-controlled-secret"),
	})
	defer server.Close()

	p := newAppleProviderForServer(t, server)
	_, err := p.Exchange(context.Background(), oauth.ExchangeParams{
		Code: "c", CodeVerifier: "v", Nonce: "expected-nonce",
	})
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "signing method")
}

// --- Per-request and token request shape ---

func TestAppleProvider_Exchange_PerRequestRedirectURI(t *testing.T) {
	const (
		seedRedirect   = "http://127.0.0.1:0/oauth/callback"
		perReqRedirect = "http://127.0.0.1:54321/oauth/callback"
		expectedReplay = "http://127.0.0.1:54321/oauth/callback"
	)

	now := time.Now()
	var captured url.Values
	server, _ := fakeApple(t, makeAppleClaims(now, nil), fakeAppleOpts{
		captureForm: &captured,
	})
	defer server.Close()

	p, err := oauth.NewAppleProvider(oauth.AppleConfig{
		ClientID:      testAppleClientID,
		TeamID:        testAppleTeamID,
		KeyID:         testAppleKeyID,
		PrivateKey:    generateP256PEM(t),
		RedirectURI:   seedRedirect, // seed must NOT be sent at /auth/token
		TokenEndpoint: server.URL + "/auth/token",
		JWKSEndpoint:  server.URL + "/auth/keys",
		Issuer:        "https://appleid.apple.com",
	})
	require.NoError(t, err)

	_, err = p.Exchange(context.Background(), oauth.ExchangeParams{
		Code:         "c",
		CodeVerifier: "v",
		Nonce:        "expected-nonce",
		RedirectURI:  perReqRedirect,
	})
	require.NoError(t, err)

	require.NotNil(t, captured)
	assert.Equal(t, expectedReplay, captured.Get("redirect_uri"),
		"per-request redirect_uri must override seed at /auth/token")
}

func TestAppleProvider_Exchange_ClientSecretInTokenRequest(t *testing.T) {
	// Apple rejects static client_secret values; we send a freshly-signed ES256
	// JWT instead. This test asserts the form body of the /auth/token request
	// carries a JWT-shaped client_secret (3 dot-separated base64 segments).
	now := time.Now()
	var captured url.Values
	server, _ := fakeApple(t, makeAppleClaims(now, nil), fakeAppleOpts{
		captureForm: &captured,
	})
	defer server.Close()

	p := newAppleProviderForServer(t, server)
	_, err := p.Exchange(context.Background(), oauth.ExchangeParams{
		Code: "c", CodeVerifier: "v", Nonce: "expected-nonce",
	})
	require.NoError(t, err)

	require.NotNil(t, captured)
	clientSecret := captured.Get("client_secret")
	require.NotEmpty(t, clientSecret, "client_secret must be present in token request")

	parts := strings.Split(clientSecret, ".")
	assert.Equal(t, 3, len(parts),
		"client_secret must be a JWT (3 base64 segments separated by dots), got %q", clientSecret)
	for i, part := range parts {
		assert.NotEmpty(t, part, "JWT segment %d must be non-empty", i)
	}

	// Parse and verify the client_secret JWT carries Apple's required claims.
	parsed, _, err := jwt.NewParser().ParseUnverified(clientSecret, jwt.MapClaims{})
	require.NoError(t, err)
	claims, ok := parsed.Claims.(jwt.MapClaims)
	require.True(t, ok)
	assert.Equal(t, testAppleTeamID, claims["iss"], "client_secret iss must be TeamID")
	assert.Equal(t, testAppleClientID, claims["sub"], "client_secret sub must be ClientID (Services ID)")
	assert.Equal(t, "https://appleid.apple.com", claims["aud"])
	assert.Equal(t, testAppleKeyID, parsed.Header["kid"], "client_secret kid must be KeyID")
	assert.Equal(t, "ES256", parsed.Header["alg"])
}

func TestAppleProvider_Exchange_TokenEndpoint5xx(t *testing.T) {
	now := time.Now()
	server, _ := fakeApple(t, makeAppleClaims(now, nil), fakeAppleOpts{status5xx: true})
	defer server.Close()

	p := newAppleProviderForServer(t, server)
	_, err := p.Exchange(context.Background(), oauth.ExchangeParams{
		Code: "c", CodeVerifier: "v", Nonce: "expected-nonce",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "503")
}

func TestAppleProvider_Exchange_TokenResponseMissingIDToken(t *testing.T) {
	now := time.Now()
	server, _ := fakeApple(t, makeAppleClaims(now, nil), fakeAppleOpts{returnNoIDToken: true})
	defer server.Close()

	p := newAppleProviderForServer(t, server)
	_, err := p.Exchange(context.Background(), oauth.ExchangeParams{
		Code: "c", CodeVerifier: "v", Nonce: "expected-nonce",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "id_token")
}

// TestAppleProvider_JWKSRotation verifies the cache-miss → re-fetch behavior
// when Apple advertises a new kid mid-session. Two Exchange calls hit the same
// provider instance with id_tokens carrying different kids; both must succeed.
func TestAppleProvider_JWKSRotation(t *testing.T) {
	now := time.Now()

	// Generate two distinct RSA keys; the JWKS endpoint flips between them.
	priv1, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	priv2, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	const kid1, kid2 = "apple-rot-1", "apple-rot-2"

	var requestCount int32

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/token", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm()) // #nosec G120 -- test fixture, body bounded by httptest client

		count := atomic.AddInt32(&requestCount, 1)
		var (
			activeKid string
			activeKey *rsa.PrivateKey
		)
		if count == 1 {
			activeKid, activeKey = kid1, priv1
		} else {
			activeKid, activeKey = kid2, priv2
		}

		token := jwt.NewWithClaims(jwt.SigningMethodRS256, makeAppleClaims(now, nil))
		token.Header["kid"] = activeKid
		signed, err := token.SignedString(activeKey)
		require.NoError(t, err)

		_ = json.NewEncoder(w).Encode(map[string]any{
			"id_token":   signed,
			"token_type": "Bearer",
		})
	})
	mux.HandleFunc("/auth/keys", func(w http.ResponseWriter, _ *http.Request) {
		// JWKS reflects whichever key is currently active.
		count := atomic.LoadInt32(&requestCount)
		var (
			activeKid string
			activeKey *rsa.PrivateKey
		)
		if count <= 1 {
			activeKid, activeKey = kid1, priv1
		} else {
			activeKid, activeKey = kid2, priv2
		}
		n := base64.RawURLEncoding.EncodeToString(activeKey.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x01})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA", "alg": "RS256", "use": "sig", "kid": activeKid, "n": n, "e": e,
			}},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	p, err := oauth.NewAppleProvider(oauth.AppleConfig{
		ClientID:      testAppleClientID,
		TeamID:        testAppleTeamID,
		KeyID:         testAppleKeyID,
		PrivateKey:    generateP256PEM(t),
		RedirectURI:   testAppleRedirect,
		TokenEndpoint: server.URL + "/auth/token",
		JWKSEndpoint:  server.URL + "/auth/keys",
		Issuer:        "https://appleid.apple.com",
	})
	require.NoError(t, err)

	// First exchange: kid1 in id_token + JWKS.
	_, err = p.Exchange(context.Background(), oauth.ExchangeParams{
		Code: "c1", CodeVerifier: "v1", Nonce: "expected-nonce",
	})
	require.NoError(t, err, "first exchange (kid1) must succeed")

	// Second exchange: kid2 in id_token + JWKS. Cache miss on kid2 → re-fetch.
	_, err = p.Exchange(context.Background(), oauth.ExchangeParams{
		Code: "c2", CodeVerifier: "v2", Nonce: "expected-nonce",
	})
	require.NoError(t, err, "second exchange (kid2, after rotation) must succeed via re-fetch")
}
