package middleware

import (
	"bytes"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
)

func TestRequireCloudflareAccessDeniesAbsentAndAllowsValidToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	key := newRSAKey(t)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(jwksJSON(t, "k1", key))
	}))
	t.Cleanup(jwks.Close)

	verifier := newAccessVerifier(jwks.URL, "test-aud")
	router := gin.New()
	var called int32
	router.Use(RequireCloudflareAccess(verifier, slog.Default()))
	router.GET("/admin/x", func(c *gin.Context) {
		atomic.AddInt32(&called, 1)
		c.Status(http.StatusOK)
	})

	missing := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/x", nil)
	router.ServeHTTP(missing, req)
	require.Equal(t, http.StatusForbidden, missing.Code)
	require.Equal(t, int32(0), atomic.LoadInt32(&called))

	token := signJWT(t, key, "k1", map[string]any{
		"aud": "test-aud",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iss": jwks.URL,
	})
	allowed := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin/x", nil)
	req.Header.Set(accessHeader, token)
	router.ServeHTTP(allowed, req)
	require.Equal(t, http.StatusOK, allowed.Code)
	require.Equal(t, int32(1), atomic.LoadInt32(&called))
}

func TestAccessVerifierAllowsValidToken(t *testing.T) {
	key := newRSAKey(t)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(jwksJSON(t, "k1", key))
	}))
	t.Cleanup(jwks.Close)

	token := signJWT(t, key, "k1", map[string]any{
		"aud":   "test-aud",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"iat":   time.Now().Unix(),
		"iss":   jwks.URL,
		"email": "admin@example.test",
	})

	claims, err := newAccessVerifier(jwks.URL, "test-aud").Verify(token)
	require.NoError(t, err)
	require.Equal(t, "admin@example.test", claims.Email)
}

func TestAccessVerifierCachesJWKS(t *testing.T) {
	key := newRSAKey(t)
	hits := 0
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write(jwksJSON(t, "k1", key))
	}))
	t.Cleanup(jwks.Close)

	token := signJWT(t, key, "k1", map[string]any{
		"aud": "test-aud",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iss": jwks.URL,
	})
	verifier := newAccessVerifier(jwks.URL, "test-aud")

	_, err := verifier.Verify(token)
	require.NoError(t, err)
	_, err = verifier.Verify(token)
	require.NoError(t, err)
	require.Equal(t, 1, hits)
}

func TestAccessVerifierDebouncesUnknownKidRefresh(t *testing.T) {
	fixedNow := time.Unix(1_700_000_000, 0)
	oldNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	t.Cleanup(func() { nowFunc = oldNow })

	key := newRSAKey(t)
	hits := 0
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write(jwksJSON(t, "k1", key))
	}))
	t.Cleanup(jwks.Close)

	claims := map[string]any{"aud": "test-aud", "exp": fixedNow.Add(time.Hour).Unix(), "iss": jwks.URL}
	verifier := newAccessVerifier(jwks.URL, "test-aud")
	_, err := verifier.Verify(signJWT(t, key, "k1", claims))
	require.NoError(t, err)

	_, err = verifier.Verify(signJWT(t, key, "k2", claims))
	require.ErrorIs(t, err, errUnknownKID)
	require.Equal(t, 1, hits)

	nowFunc = func() time.Time { return fixedNow.Add(negativeKIDTTL + time.Millisecond) }
	_, err = verifier.Verify(signJWT(t, key, "k2", claims))
	require.ErrorIs(t, err, errUnknownKID)
	require.Equal(t, 2, hits)
}

func TestAccessVerifierKnownKeyRemainsAvailableDuringUnknownKIDRefresh(t *testing.T) {
	key := newRSAKey(t)
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseRefresh) }) }
	t.Cleanup(release)
	var hits int32
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&hits, 1) == 2 {
			close(refreshStarted)
			<-releaseRefresh
		}
		_, _ = w.Write(jwksJSON(t, "known", key))
	}))
	t.Cleanup(jwks.Close)

	claims := map[string]any{"aud": "test-aud", "exp": time.Now().Add(time.Hour).Unix(), "iss": jwks.URL}
	verifier := newAccessVerifier(jwks.URL, "test-aud")
	known := signJWT(t, key, "known", claims)
	require.NoError(t, verifyAccessToken(verifier, known))
	verifier.mu.Lock()
	verifier.lastAttempt = time.Now().Add(-jwksRefreshInterval)
	verifier.mu.Unlock()

	unknownDone := make(chan error, 1)
	unknown := signJWT(t, key, "unknown", claims)
	go func() { unknownDone <- verifyAccessToken(verifier, unknown) }()
	select {
	case <-refreshStarted:
	case <-time.After(time.Second):
		t.Fatal("unknown KID did not start a JWKS refresh")
	}

	knownDone := make(chan error, 1)
	go func() { knownDone <- verifyAccessToken(verifier, known) }()
	select {
	case err := <-knownDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("fresh known KID blocked on unknown KID refresh")
	}
	release()
	require.ErrorIs(t, <-unknownDone, errUnknownKID)
}

func TestAccessVerifierSingleflightRefreshAndBoundedNegativeKIDs(t *testing.T) {
	key := newRSAKey(t)
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseRefresh) }) }
	t.Cleanup(release)
	var hits int32
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&hits, 1) == 2 {
			close(refreshStarted)
			<-releaseRefresh
		}
		_, _ = w.Write(jwksJSON(t, "known", key))
	}))
	t.Cleanup(jwks.Close)

	claims := map[string]any{"aud": "test-aud", "exp": time.Now().Add(time.Hour).Unix(), "iss": jwks.URL}
	verifier := newAccessVerifier(jwks.URL, "test-aud")
	require.NoError(t, verifyAccessToken(verifier, signJWT(t, key, "known", claims)))
	verifier.mu.Lock()
	verifier.lastAttempt = time.Now().Add(-jwksRefreshInterval)
	verifier.mu.Unlock()

	const callers = 16
	tokens := make([]string, callers)
	for i := range tokens {
		tokens[i] = signJWT(t, key, fmt.Sprintf("unknown-%d", i), claims)
	}
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(token string) {
			defer wg.Done()
			errs <- verifyAccessToken(verifier, token)
		}(tokens[i])
	}
	select {
	case <-refreshStarted:
	case <-time.After(time.Second):
		t.Fatal("unknown KID burst did not start a JWKS refresh")
	}
	release()
	wg.Wait()
	close(errs)
	for err := range errs {
		require.ErrorIs(t, err, errUnknownKID)
	}
	require.Equal(t, int32(2), atomic.LoadInt32(&hits), "one JWKS refresh per unknown-KID burst")

	for i := 0; i < maxNegativeKIDs+20; i++ {
		verifier.cacheNegativeKID(fmt.Sprintf("negative-%d", i), time.Now())
	}
	verifier.mu.RLock()
	negativeCount := len(verifier.negativeKIDs)
	verifier.mu.RUnlock()
	require.LessOrEqual(t, negativeCount, maxNegativeKIDs)
}

func TestAccessVerifierRejectsOversizedAndTrailingJWKS(t *testing.T) {
	key := newRSAKey(t)
	claims := func(issuer string) map[string]any {
		return map[string]any{"aud": "test-aud", "exp": time.Now().Add(time.Hour).Unix(), "iss": issuer}
	}
	for name, body := range map[string]string{
		"oversized": string(make([]byte, maxJWKSBody+1)),
		"trailing":  string(jwksJSON(t, "known", key)) + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			t.Cleanup(jwks.Close)
			_, err := newAccessVerifier(jwks.URL, "test-aud").Verify(signJWT(t, key, "known", claims(jwks.URL)))
			require.ErrorIs(t, err, errUnknownKID)
		})
	}
}

func TestAccessVerifierFailedRefreshPreservesFreshKeys(t *testing.T) {
	key := newRSAKey(t)
	var failRefresh atomic.Bool
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if failRefresh.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(jwksJSON(t, "known", key))
	}))
	t.Cleanup(jwks.Close)

	claims := map[string]any{"aud": "test-aud", "exp": time.Now().Add(time.Hour).Unix(), "iss": jwks.URL}
	verifier := newAccessVerifier(jwks.URL, "test-aud")
	known := signJWT(t, key, "known", claims)
	require.NoError(t, verifyAccessToken(verifier, known))
	verifier.mu.Lock()
	verifier.lastAttempt = time.Now().Add(-jwksRefreshInterval)
	verifier.mu.Unlock()
	failRefresh.Store(true)
	require.ErrorIs(t, verifyAccessToken(verifier, signJWT(t, key, "unknown", claims)), errUnknownKID)
	require.NoError(t, verifyAccessToken(verifier, known))
}

func verifyAccessToken(verifier *accessVerifier, token string) error {
	_, err := verifier.Verify(token)
	return err
}

func TestRequireCloudflareAccessFromConfigAllowsValidToken(t *testing.T) {
	key := newRSAKey(t)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(jwksJSON(t, "k1", key))
	}))
	t.Cleanup(jwks.Close)

	router := gin.New()
	router.Use(RequireCloudflareAccessFromConfig(&config.Config{CFAccessAUD: "test-aud", CFAccessTeamDomain: jwks.URL}, slog.Default()))
	router.GET("/admin/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/x", nil)
	req.Header.Set(accessHeader, signJWT(t, key, "k1", map[string]any{"aud": []string{"other", "test-aud"}, "exp": time.Now().Add(time.Hour).Unix(), "iss": jwks.URL}))
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestRSAPublicKeyFromJWKRejectsInvalidKeys(t *testing.T) {
	key := newRSAKey(t)
	n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())

	_, ok := rsaPublicKeyFromJWK("EC", "RS256", n, e)
	require.False(t, ok)
	_, ok = rsaPublicKeyFromJWK("RSA", "HS256", n, e)
	require.False(t, ok)
	_, ok = rsaPublicKeyFromJWK("RSA", "RS256", "%%%", e)
	require.False(t, ok)
	_, ok = rsaPublicKeyFromJWK("RSA", "RS256", n, "%%%")
	require.False(t, ok)
	_, ok = rsaPublicKeyFromJWK("RSA", "RS256", base64.RawURLEncoding.EncodeToString([]byte{}), e)
	require.False(t, ok)
}

func TestAccessVerifierRejectsInvalidTokens(t *testing.T) {
	fixedNow := time.Unix(1_700_000_000, 0)
	oldNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	t.Cleanup(func() { nowFunc = oldNow })

	key := newRSAKey(t)
	otherKey := newRSAKey(t)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(jwksJSON(t, "k1", key))
	}))
	t.Cleanup(jwks.Close)

	validClaims := map[string]any{
		"aud": "test-aud",
		"exp": fixedNow.Add(time.Hour).Unix(),
		"iss": jwks.URL,
	}

	tests := []struct {
		name  string
		token string
		want  error
	}{
		{"malformed", "not-a-jwt", errMalformed},
		{"bad_signature", signJWT(t, otherKey, "k1", validClaims), errBadSignature},
		{"missing_exp", signJWT(t, key, "k1", map[string]any{"aud": "test-aud", "iss": jwks.URL}), errExpired},
		{"unknown_kid", signJWT(t, key, "k2", validClaims), errUnknownKID},
		{"alg_none", unsignedJWT(t, "none", "k1", validClaims), errBadAlg},
		{"alg_hs256", hmacJWT(t, key.N.Bytes(), "k1", validClaims), errBadAlg},
		{"aud_mismatch", signJWT(t, key, "k1", map[string]any{"aud": "wrong-aud", "exp": fixedNow.Add(time.Hour).Unix(), "iss": jwks.URL}), errAudMismatch},
		{"expired", signJWT(t, key, "k1", map[string]any{"aud": "test-aud", "exp": fixedNow.Add(-2 * time.Minute).Unix(), "iss": jwks.URL}), errExpired},
		{"nbf_future", signJWT(t, key, "k1", map[string]any{"aud": "test-aud", "exp": fixedNow.Add(time.Hour).Unix(), "nbf": fixedNow.Add(2 * time.Minute).Unix(), "iss": jwks.URL}), errExpired},
		{"bad_issuer", signJWT(t, key, "k1", map[string]any{"aud": "test-aud", "exp": fixedNow.Add(time.Hour).Unix(), "iss": "https://other.cloudflareaccess.com"}), errBadIssuer},
	}

	verifier := newAccessVerifier(jwks.URL, "test-aud")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := verifier.Verify(tt.token)
			require.ErrorIs(t, err, tt.want)
		})
	}
}

func TestAccessVerifierRefetchesOnKidMiss(t *testing.T) {
	currentNow := time.Unix(1_700_000_000, 0)
	oldNow := nowFunc
	nowFunc = func() time.Time { return currentNow }
	t.Cleanup(func() { nowFunc = oldNow })

	key1 := newRSAKey(t)
	key2 := newRSAKey(t)
	hits := 0
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		if hits == 1 {
			_, _ = w.Write(jwksJSON(t, "k1", key1))
			return
		}
		_, _ = w.Write(jwksJSONMulti(t, map[string]*rsa.PrivateKey{"k1": key1, "k2": key2}))
	}))
	t.Cleanup(jwks.Close)

	verifier := newAccessVerifier(jwks.URL, "test-aud")
	_, err := verifier.Verify(signJWT(t, key1, "k1", map[string]any{"aud": "test-aud", "exp": currentNow.Add(time.Hour).Unix(), "iss": jwks.URL}))
	require.NoError(t, err)
	currentNow = currentNow.Add(time.Second + time.Millisecond)
	_, err = verifier.Verify(signJWT(t, key2, "k2", map[string]any{"aud": "test-aud", "exp": currentNow.Add(time.Hour).Unix(), "iss": jwks.URL}))
	require.NoError(t, err)
	require.Equal(t, 2, hits)
}

func TestAccessVerifierFailsClosedWhenJWKSUnavailableOnEmptyCache(t *testing.T) {
	key := newRSAKey(t)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(jwks.Close)

	token := signJWT(t, key, "k1", map[string]any{"aud": "test-aud", "exp": time.Now().Add(time.Hour).Unix(), "iss": jwks.URL})
	_, err := newAccessVerifier(jwks.URL, "test-aud").Verify(token)
	require.ErrorIs(t, err, errUnknownKID)
}

func TestAccessVerifierRejectsStaleCachedKeyWhenRefreshFails(t *testing.T) {
	fixedNow := time.Unix(1_700_000_000, 0)
	oldNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	t.Cleanup(func() { nowFunc = oldNow })

	key := newRSAKey(t)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(jwksJSON(t, "k1", key))
	}))

	token := signJWT(t, key, "k1", map[string]any{
		"aud": "test-aud",
		"exp": fixedNow.Add(3 * jwksTTL).Unix(),
		"iss": jwks.URL,
	})
	verifier := newAccessVerifier(jwks.URL, "test-aud")
	_, err := verifier.Verify(token)
	require.NoError(t, err)
	jwks.Close()

	nowFunc = func() time.Time { return fixedNow.Add(jwksTTL + time.Second) }
	_, err = verifier.Verify(token)
	require.ErrorIs(t, err, errUnknownKID)
}

func TestRequireCloudflareAccessLogsPIISafeReason(t *testing.T) {
	key := newRSAKey(t)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(jwksJSON(t, "k1", key))
	}))
	t.Cleanup(jwks.Close)

	token := signJWT(t, key, "k1", map[string]any{
		"aud":   "wrong-aud",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"iss":   jwks.URL,
		"email": "admin@example.test",
	})
	var logs bytes.Buffer
	router := gin.New()
	router.Use(RequireCloudflareAccess(newAccessVerifier(jwks.URL, "test-aud"), slog.New(slog.NewTextHandler(&logs, nil))))
	router.GET("/admin/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/x", nil)
	req.Header.Set(accessHeader, token)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	out := logs.String()
	require.Contains(t, out, "cloudflare_access_denied")
	require.NotContains(t, out, token)
	require.NotContains(t, out, "admin@example.test")
}

func TestCloudflareAccessFailureNormalizesUnknownVerifierErrors(t *testing.T) {
	require.Equal(t, "cloudflare_access_denied", cloudflareAccessFailure(errors.New("unexpected verifier detail")))
}

func newRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return key
}

func jwksJSON(t *testing.T, kid string, key *rsa.PrivateKey) []byte {
	t.Helper()
	return jwksJSONMulti(t, map[string]*rsa.PrivateKey{kid: key})
}

func jwksJSONMulti(t *testing.T, keys map[string]*rsa.PrivateKey) []byte {
	t.Helper()
	jwks := make([]map[string]string, 0, len(keys))
	for kid, key := range keys {
		jwks = append(jwks, map[string]string{
			"kid": kid,
			"kty": "RSA",
			"alg": "RS256",
			"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		})
	}
	b, err := json.Marshal(map[string]any{"keys": jwks})
	require.NoError(t, err)
	return b
}

func signJWT(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	h := jsonSegment(t, map[string]string{"alg": "RS256", "kid": kid})
	p := jsonSegment(t, claims)
	signed := h + "." + p
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	require.NoError(t, err)
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func unsignedJWT(t *testing.T, alg, kid string, claims map[string]any) string {
	t.Helper()
	return jsonSegment(t, map[string]string{"alg": alg, "kid": kid}) + "." + jsonSegment(t, claims) + "."
}

func hmacJWT(t *testing.T, secret []byte, kid string, claims map[string]any) string {
	t.Helper()
	signed := jsonSegment(t, map[string]string{"alg": "HS256", "kid": kid}) + "." + jsonSegment(t, claims)
	mac := hmac.New(sha256.New, secret)
	_, err := mac.Write([]byte(signed))
	require.NoError(t, err)
	return signed + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func jsonSegment(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(b)
}
