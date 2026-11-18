package oauth_test

// Regression for login CSRF on POST /api/v1/auth/sso/:provider/session and for
// the SSO complete-link MFA bypass. Both drive the real handlers without a
// database: miniredis, an in-test JWKS server, and a stdlib fake SQL driver.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/oauth"
)

// ---- minimal stdlib fake SQL driver (no external deps) ----

const loginCSRFAttackerUID = "11111111-2222-3333-4444-555555555555"

type lcsrfDriver struct{}
type lcsrfConn struct{}
type lcsrfRows struct {
	cols []string
	vals [][]driver.Value
	i    int
}

func (lcsrfDriver) Open(string) (driver.Conn, error)  { return lcsrfConn{}, nil }
func (lcsrfConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (lcsrfConn) Close() error                        { return nil }
func (lcsrfConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }
func (r *lcsrfRows) Columns() []string                { return r.cols }
func (r *lcsrfRows) Close() error                     { return nil }
func (r *lcsrfRows) Next(dest []driver.Value) error {
	if r.i >= len(r.vals) {
		return io.EOF
	}
	copy(dest, r.vals[r.i])
	r.i++
	return nil
}
func (lcsrfConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(q, "trust_sso_security"):
		// respondExistingSSO: the attacker's own account, trust_sso_security=TRUE.
		return &lcsrfRows{cols: []string{"id", "trust"}, vals: [][]driver.Value{{loginCSRFAttackerUID, true}}}, nil
	case strings.Contains(q, "FROM user_sso_identities"):
		// classifyCallback: attacker's provider identity is already linked.
		return &lcsrfRows{cols: []string{"user_id"}, vals: [][]driver.Value{{loginCSRFAttackerUID}}}, nil
	}
	return &lcsrfRows{cols: []string{"x"}}, nil
}
func (lcsrfConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}

func init() { sql.Register("loginCSRFFakeSQL", lcsrfDriver{}) }

const loginCSRFAllowedOrigin = "https://concord.example"

// postSessionAsAttacker seeds an attacker-owned SSO state + signed id_token and
// submits it to ProviderSession through the production middleware chain.
func postSessionAsAttacker(t *testing.T, contentType, origin, fetchSite string, textPlainForm bool) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.HandleFunc("/certs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k1",
			"n": base64.RawURLEncoding.EncodeToString(priv.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
		}}})
	})
	jwks := httptest.NewServer(mux)
	t.Cleanup(jwks.Close)

	google, err := oauth.NewGoogleProvider(oauth.GoogleConfig{
		ClientID: "cid.apps.googleusercontent.com", RedirectURI: "http://127.0.0.1:0/cb",
		JWKSEndpoint: jwks.URL + "/certs", Issuer: "https://accounts.google.com",
	})
	require.NoError(t, err)
	reg := oauth.NewRegistry()
	reg.Register(google)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	db, err := sql.Open("loginCSRFFakeSQL", "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	h := oauth.NewHandler(oauth.HandlerDeps{Registry: reg, Redis: rdb, DB: db, AuthHandler: &fakeAuthAdapter{}})

	// Same ordering as router.go: global chain, then the auth-group guard.
	r := gin.New()
	r.Use(middleware.SecurityHeaders("production", ""))
	r.Use(middleware.CORS([]string{loginCSRFAllowedOrigin}))
	r.Use(middleware.ValidateCustomHeaders())
	authRoutes := r.Group("/api/v1/auth")
	authRoutes.Use(middleware.LoginCSRF([]string{loginCSRFAllowedOrigin}))
	authRoutes.POST("/sso/:provider/session", h.ProviderSession)

	state := "attacker-state-" + time.Now().Format("150405.000000")
	nonce := "attacker-nonce"
	bindingRaw := make([]byte, 32)
	_, err = rand.Read(bindingRaw)
	require.NoError(t, err)
	digest := sha256.Sum256(append([]byte("concord-sso-client-binding-v1\x00"), bindingRaw...))
	rec, err := json.Marshal(map[string]any{"provider": "google", "state": state, "nonce": nonce,
		"client_binding_digest": digest[:],
		"code_verifier":         "", "redirect_uri": "http://127.0.0.1:0/cb", "created_at": time.Now().UTC()})
	require.NoError(t, err)
	require.NoError(t, rdb.Set(context.Background(), "sso_state:"+state, rec, 10*time.Minute).Err())

	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": "https://accounts.google.com", "aud": "cid.apps.googleusercontent.com",
		"sub": "attacker-google-sub", "email": "attacker@evil.example", "email_verified": true,
		"nonce": nonce, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	tok.Header["kid"] = "k1"
	idToken, err := tok.SignedString(priv)
	require.NoError(t, err)

	var body string
	if textPlainForm {
		// enctype=text/plain serialises one field as name=value CRLF.
		body = `{"id_token":"` + idToken + `","state":"` + state + `","apple_user_data":"` + "=" + `"}` + "\r\n"
	} else {
		raw, mErr := json.Marshal(map[string]string{"id_token": idToken, "state": state})
		require.NoError(t, mErr)
		body = string(raw)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/sso/google/session", strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	if !textPlainForm {
		// A cross-site HTML form cannot set custom headers; a real client does.
		req.Header.Set("X-Concord-SSO-Client-Binding", base64.RawURLEncoding.EncodeToString(bindingRaw))
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if fetchSite != "" {
		req.Header.Set("Sec-Fetch-Site", fetchSite)
	}
	req.Header.Set("X-Forwarded-Proto", "https")
	req.RemoteAddr = "198.51.100.7:44444"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// Inverted from the cross-site text/plain exploit: the victim browser must not
// receive the attacker's refresh cookie.
func TestLoginCSRF_CrossSiteTextPlainFormCannotSetRefreshCookie(t *testing.T) {
	w := postSessionAsAttacker(t, "text/plain", "https://evil.example", "cross-site", true)
	require.Equal(t, http.StatusUnsupportedMediaType, w.Code, w.Body.String())
	require.Empty(t, w.Header().Values("Set-Cookie"))
	require.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
}

func TestLoginCSRF_CrossSiteJSONFetchCannotSetRefreshCookie(t *testing.T) {
	w := postSessionAsAttacker(t, "application/json", "https://evil.example", "cross-site", false)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	require.Empty(t, w.Header().Values("Set-Cookie"))
}

// Control: the legitimate same-origin JSON path still reaches the handler, so
// the rejections above are attributable to the guard and not a broken harness.
func TestLoginCSRF_SameOriginJSONStillSetsRefreshCookie(t *testing.T) {
	w := postSessionAsAttacker(t, "application/json", loginCSRFAllowedOrigin, "same-origin", false)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Header().Get("Set-Cookie"), "refresh_token=fake-refresh-"+loginCSRFAttackerUID)
}

// Inverted from the complete-link MFA-skip exploit: a correct password for an
// MFA-enrolled victim must yield only an MFA challenge, never a session or a
// linked identity.
func TestCompleteLink_PasswordAloneNeverMintsSessionForMFAUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const victim = "22222222-2222-4222-8222-222222222222"
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	adapter := &fakeAuthAdapter{
		PrepareSSOLinkResult: &auth.SSOLinkChallenge{Token: "signed-link-challenge", Methods: []string{"totp"}},
	}
	h := oauth.NewHandler(oauth.HandlerDeps{Redis: rdb, AuthHandler: adapter})
	r := gin.New()
	r.POST("/api/v1/auth/sso/:provider/complete-link", h.CompleteLink)

	payload, err := json.Marshal(map[string]any{
		"provider": "google", "provider_user_id": "attacker-google-sub", "provider_email": "victim@example.test",
		"target_user_id": victim, "branch": "account_link", "created_at": time.Now().UTC().Format(time.RFC3339),
	})
	require.NoError(t, err)
	require.NoError(t, rdb.Set(context.Background(), "sso_token:tok-attacker", payload, 15*time.Minute).Err())

	body, err := json.Marshal(map[string]string{"sso_token": "tok-attacker", "password": "correct-password"}) //nolint:gosec // test-only credential fixture
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/sso/google/complete-link", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, true, resp["mfa_required"])
	require.Empty(t, resp["access_token"])
	require.Empty(t, w.Header().Values("Set-Cookie"))
	require.Zero(t, adapter.IssueAccessAndRefreshBoundCalls)
	require.Nil(t, adapter.LinkedIdentity)

	// The sso_token is single use even on the challenge path.
	_, err = rdb.Get(context.Background(), "sso_token:tok-attacker").Result()
	require.Error(t, err)
}
