package oauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/oauth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// newDualProviderInitiateRig wires Initiate with BOTH providers registered so
// the PKCE contract is pinned from one rig: both apple and google REQUIRE a
// client-supplied code_challenge (#974/#975).
func newDualProviderInitiateRig(t *testing.T) (*gin.Engine, *redis.Client, *recordingPutter) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)

	google, err := oauth.NewGoogleProvider(oauth.GoogleConfig{
		ClientID:    "test-client.apps.googleusercontent.com",
		RedirectURI: "http://127.0.0.1:0/oauth/callback",
	})
	require.NoError(t, err)
	apple, err := oauth.NewAppleProvider(oauth.AppleConfig{
		ClientID:   testAppleClientID,
		TeamID:     testAppleTeamID,
		KeyID:      testAppleKeyID,
		PrivateKey: generateP256PEM(t),
	})
	require.NoError(t, err)

	registry := oauth.NewRegistry()
	registry.Register(google)
	registry.Register(apple)

	putter := &recordingPutter{}
	h := oauth.NewHandler(oauth.HandlerDeps{Registry: registry, Redis: rdb, CFKV: putter})
	r := gin.New()
	r.GET("/api/v1/auth/sso/:provider", h.Initiate)
	return r, rdb, putter
}

const initiateRedirect = "http://127.0.0.1:65000/oauth/callback"

var testSSOClientBinding = strings.Repeat("A", 43)

func testSSOClientBindingDigest(t *testing.T) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(testSSOClientBinding)
	require.NoError(t, err)
	digest := sha256.Sum256(append([]byte("concord-sso-client-binding-v1\x00"), raw...))
	return digest[:]
}

func getInitiate(r *gin.Engine, provider, challenge string) *httptest.ResponseRecorder {
	return getInitiateWithBinding(r, provider, challenge, testSSOClientBinding)
}

func getInitiateWithBinding(r *gin.Engine, provider, challenge, binding string) *httptest.ResponseRecorder {
	target := "/api/v1/auth/sso/" + provider + "?redirect_uri=" + url.QueryEscape(initiateRedirect)
	if challenge != "" {
		target += "&code_challenge=" + url.QueryEscape(challenge)
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if binding != "" {
		req.Header.Set("X-Concord-SSO-Client-Binding", binding)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestInitiateGlobalLimitRejectsBeforeStateAllocationAndSeparatesProviders(t *testing.T) {
	r, rdb, putter := newDualProviderInitiateRig(t)
	ctx := context.Background()
	invalid := getInitiateWithBinding(r, "google", strings.Repeat("a", 43), "")
	require.Equal(t, http.StatusBadRequest, invalid.Code)
	noBudget, err := rdb.Exists(ctx, "ratelimit:sso:initiate:google").Result()
	require.NoError(t, err)
	require.Zero(t, noBudget, "invalid client binding must not spend the provider budget")
	require.NoError(t, rdb.Set(ctx, "ratelimit:sso:initiate:apple", "600", time.Minute).Err())
	blocked := getInitiate(r, "apple", strings.Repeat("a", 43))
	require.Equal(t, http.StatusTooManyRequests, blocked.Code, blocked.Body.String())
	require.Contains(t, blocked.Body.String(), "rate_limited")
	keys, err := rdb.Keys(ctx, "sso_state:*").Result()
	require.NoError(t, err)
	require.Empty(t, keys)
	require.Empty(t, putter.calls)

	allowed := getInitiate(r, "google", strings.Repeat("a", 43))
	require.Equal(t, http.StatusOK, allowed.Code, allowed.Body.String())
	count, err := rdb.Get(ctx, "ratelimit:sso:initiate:google").Int64()
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
}

type rejectInitiateCounterHook struct{}

func (rejectInitiateCounterHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (rejectInitiateCounterHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (rejectInitiateCounterHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "incr" && len(cmd.Args()) > 1 {
			key, ok := cmd.Args()[1].(string)
			if ok && strings.HasPrefix(key, "ratelimit:sso:initiate:") {
				return errors.New("counter unavailable")
			}
		}
		return next(ctx, cmd)
	}
}

func TestInitiateGlobalLimitFailsClosedOnRedisError(t *testing.T) {
	r, rdb, putter := newDualProviderInitiateRig(t)
	rdb.AddHook(rejectInitiateCounterHook{})
	response := getInitiate(r, "apple", strings.Repeat("a", 43))
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.Contains(t, response.Body.String(), "redis_unavailable")
	keys, err := rdb.Keys(context.Background(), "sso_state:*").Result()
	require.NoError(t, err)
	require.Empty(t, keys)
	require.Empty(t, putter.calls)
}

func TestInitiate_ClientBindingRequiredAndStoredAsDigest(t *testing.T) {
	r, rdb, _ := newDualProviderInitiateRig(t)
	for _, provider := range []string{"apple", "google"} {
		t.Run(provider, func(t *testing.T) {
			for name, binding := range map[string]string{
				"missing": "", "short": strings.Repeat("A", 42),
				"long": strings.Repeat("A", 44), "padded": testSSOClientBinding + "=",
				"invalid": strings.Repeat("!", 43), "noncanonical": strings.Repeat("A", 42) + "B",
			} {
				t.Run(name, func(t *testing.T) {
					w := getInitiateWithBinding(r, provider, strings.Repeat("a", 43), binding)
					require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
					assert.Contains(t, w.Body.String(), "invalid_request")
				})
			}
			t.Run("duplicate", func(t *testing.T) {
				req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/sso/"+provider+"?redirect_uri="+url.QueryEscape(initiateRedirect)+"&code_challenge="+strings.Repeat("a", 43), nil)
				req.Header.Add("X-Concord-SSO-Client-Binding", testSSOClientBinding)
				req.Header.Add("X-Concord-SSO-Client-Binding", testSSOClientBinding)
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				require.Equal(t, http.StatusBadRequest, w.Code)
			})
			w := getInitiate(r, provider, strings.Repeat("a", 43))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var resp struct {
				State string `json:"state"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			raw, err := rdb.Get(context.Background(), "sso_state:"+resp.State).Bytes()
			require.NoError(t, err)
			assert.NotContains(t, string(raw), testSSOClientBinding)
			assert.NotContains(t, w.Body.String(), testSSOClientBinding)
			var rec struct {
				ClientBindingDigest []byte `json:"client_binding_digest"`
			}
			require.NoError(t, json.Unmarshal(raw, &rec))
			assert.Equal(t, testSSOClientBindingDigest(t), rec.ClientBindingDigest)
		})
	}
}

func TestInitiate_Apple_RequiresCodeChallenge(t *testing.T) {
	r, _, _ := newDualProviderInitiateRig(t)

	cases := map[string]string{
		"absent":      "",
		"too_short":   strings.Repeat("a", 42),
		"too_long":    strings.Repeat("a", 129),
		"bad_charset": strings.Repeat("a", 42) + "+",
	}
	for name, challenge := range cases {
		t.Run(name, func(t *testing.T) {
			w := getInitiate(r, "apple", challenge)
			require.Equal(t, http.StatusBadRequest, w.Code)
			assert.Contains(t, w.Body.String(), "code_challenge_required")
		})
	}
}

func TestInitiate_Apple_EmbedsClientChallenge_ReturnsNonce_EmptyVerifier(t *testing.T) {
	r, rdb, _ := newDualProviderInitiateRig(t)
	challenge := strings.Repeat("a", 43)

	w := getInitiate(r, "apple", challenge)
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		AuthURL string `json:"auth_url"`
		State   string `json:"state"`
		Nonce   string `json:"nonce"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.AuthURL)
	require.NotEmpty(t, resp.State)
	require.NotEmpty(t, resp.Nonce, "apple initiate must return the nonce for local id_token binding (#974)")

	parsed, err := url.Parse(resp.AuthURL)
	require.NoError(t, err)
	q := parsed.Query()
	assert.Equal(t, challenge, q.Get("code_challenge"),
		"authorize URL must embed the CLIENT-supplied challenge, not a server-generated one")
	assert.Equal(t, "S256", q.Get("code_challenge_method"))
	assert.Equal(t, "https://api.concordvoice.chat/auth/sso/apple/callback", q.Get("redirect_uri"),
		"apple auth_url must carry the Worker callback, not the loopback (#2306)")

	raw, err := rdb.Get(context.Background(), "sso_state:"+resp.State).Bytes()
	require.NoError(t, err)
	var rec struct {
		Provider     string `json:"provider"`
		State        string `json:"state"`
		Nonce        string `json:"nonce"`
		CodeVerifier string `json:"code_verifier"`
	}
	require.NoError(t, json.Unmarshal(raw, &rec))
	assert.Equal(t, "apple", rec.Provider)
	assert.Equal(t, resp.State, rec.State)
	assert.Equal(t, resp.Nonce, rec.Nonce, "record nonce must match the response nonce")
	assert.Empty(t, rec.CodeVerifier, "apple records must store NO verifier — the client owns it (#974)")
}

// TestInitiate_Google_RequiresCodeChallenge mirrors TestInitiate_Apple_RequiresCodeChallenge:
// after #975 google is also client-driven, so a missing/malformed challenge
// returns 400 code_challenge_required (identical to apple's gate).
func TestInitiate_Google_RequiresCodeChallenge(t *testing.T) {
	r, _, _ := newDualProviderInitiateRig(t)

	cases := map[string]string{
		"absent":      "",
		"too_short":   strings.Repeat("a", 42),
		"too_long":    strings.Repeat("a", 129),
		"bad_charset": strings.Repeat("a", 42) + "+",
	}
	for name, challenge := range cases {
		t.Run(name, func(t *testing.T) {
			w := getInitiate(r, "google", challenge)
			require.Equal(t, http.StatusBadRequest, w.Code)
			assert.Contains(t, w.Body.String(), "code_challenge_required")
		})
	}
}

// TestInitiate_Google_EmbedsClientChallenge_ReturnsNonce_EmptyVerifier mirrors
// TestInitiate_Apple_EmbedsClientChallenge_ReturnsNonce_EmptyVerifier: after
// #975 google returns nonce + embeds the CLIENT challenge in the auth URL and
// stores an empty CodeVerifier (the client owns the verifier).
func TestInitiate_Google_EmbedsClientChallenge_ReturnsNonce_EmptyVerifier(t *testing.T) {
	r, rdb, _ := newDualProviderInitiateRig(t)
	challenge := strings.Repeat("b", 43)

	w := getInitiate(r, "google", challenge)
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		AuthURL string `json:"auth_url"`
		State   string `json:"state"`
		Nonce   string `json:"nonce"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.AuthURL)
	require.NotEmpty(t, resp.State)
	require.NotEmpty(t, resp.Nonce, "google initiate must return nonce for desktop id_token binding (#975)")

	parsed, err := url.Parse(resp.AuthURL)
	require.NoError(t, err)
	q := parsed.Query()
	assert.Equal(t, challenge, q.Get("code_challenge"),
		"authorize URL must embed the CLIENT-supplied challenge, not a server-generated one")
	assert.Equal(t, "S256", q.Get("code_challenge_method"))

	raw, err := rdb.Get(context.Background(), "sso_state:"+resp.State).Bytes()
	require.NoError(t, err)
	var rec struct {
		Provider     string `json:"provider"`
		State        string `json:"state"`
		Nonce        string `json:"nonce"`
		CodeVerifier string `json:"code_verifier"`
	}
	require.NoError(t, json.Unmarshal(raw, &rec))
	assert.Equal(t, "google", rec.Provider)
	assert.Equal(t, resp.State, rec.State)
	assert.Equal(t, resp.Nonce, rec.Nonce, "record nonce must match the response nonce")
	assert.Empty(t, rec.CodeVerifier, "google records must store NO verifier — the client owns it (#975)")
}

// TestInitiate_Apple_AuthURLUsesWorkerCallback is the #2306 regression lock:
// Apple's Services ID accepts only the registered HTTPS Return URL, so the
// authorization URL must carry the Worker-bridge callback — never the desktop
// loopback. The loopback stays private relay metadata: retained in the Redis
// state record, port-only published to the bridge KV.
func TestInitiate_Apple_AuthURLUsesWorkerCallback(t *testing.T) {
	r, rdb, putter := newDualProviderInitiateRig(t)
	challenge := strings.Repeat("a", 43)

	w := getInitiate(r, "apple", challenge)
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		AuthURL string `json:"auth_url"`
		State   string `json:"state"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	parsed, err := url.Parse(resp.AuthURL)
	require.NoError(t, err)
	// Literal on purpose: asserting against the production constant would
	// tautologically pass if the constant drifted. This is the exact URL
	// registered on the Apple Services ID (apple-sso-worker-bridge runbook §3).
	assert.Equal(t, "https://api.concordvoice.chat/auth/sso/apple/callback",
		parsed.Query().Get("redirect_uri"),
		"apple authorization URL must use the registered Worker callback (#2306)")

	// The desktop loopback remains PRIVATE relay metadata: stored in the
	// state record for this attempt, never shown to Apple.
	raw, err := rdb.Get(context.Background(), "sso_state:"+resp.State).Bytes()
	require.NoError(t, err)
	var rec struct {
		RedirectURI string `json:"redirect_uri"`
	}
	require.NoError(t, json.Unmarshal(raw, &rec))
	assert.Equal(t, initiateRedirect, rec.RedirectURI,
		"redis state record must retain the validated desktop loopback")

	// The bridge KV received ONLY the loopback port, stateTTL-aligned.
	require.Len(t, putter.calls, 1)
	assert.Equal(t, resp.State, putter.calls[0].Key)
	assert.Equal(t, "65000", putter.calls[0].Value)
	assert.Equal(t, 600, putter.calls[0].TTL)
}
