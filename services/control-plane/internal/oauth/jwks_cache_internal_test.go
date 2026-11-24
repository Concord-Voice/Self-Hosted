package oauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testJWKS(t *testing.T, key *rsa.PrivateKey, kid string) string {
	t.Helper()
	body, err := json.Marshal(jwksResponse{Keys: []jwksKey{{
		Kid: kid, Kty: "RSA", Alg: "RS256",
		N: base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		E: base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
	}}})
	require.NoError(t, err)
	return string(body)
}

func TestJWKSCacheWithdrawnKeyAndUnknownKidBound(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	for _, provider := range []string{"apple", "google"} {
		t.Run(provider, func(t *testing.T) {
			var body atomic.Value
			body.Store(testJWKS(t, key, "new"))
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				value, ok := body.Load().(string)
				if !ok {
					http.Error(w, "invalid JWKS fixture", http.StatusInternalServerError)
					return
				}
				_, _ = w.Write([]byte(value))
			}))
			t.Cleanup(server.Close)

			var lookup func(string) (*rsa.PublicKey, error)
			old := cachedKey{key: &key.PublicKey, expiresAt: time.Now().Add(time.Hour)}
			switch provider {
			case "apple":
				p := &AppleProvider{cfg: AppleConfig{JWKSEndpoint: server.URL}, httpClient: server.Client(), clock: time.Now, jwksCache: map[string]cachedKey{"old": old}}
				lookup = func(kid string) (*rsa.PublicKey, error) { return p.fetchKey(context.Background(), kid) }
			case "google":
				p := &GoogleProvider{cfg: GoogleConfig{JWKSEndpoint: server.URL}, httpClient: server.Client(), jwksCache: map[string]cachedKey{"old": old}}
				lookup = func(kid string) (*rsa.PublicKey, error) { return p.fetchKey(context.Background(), kid) }
			}

			_, err := lookup("new")
			require.NoError(t, err)
			_, err = lookup("old")
			require.Error(t, err, "successful replacement must withdraw an omitted kid")
			require.EqualValues(t, 1, requests.Load(), "unknown kids share one provider refresh opportunity")

			for i := 0; i < 10; i++ {
				_, err = lookup("unknown" + string(rune('a'+i)))
				require.Error(t, err)
			}
			require.EqualValues(t, 1, requests.Load(), "distinct attacker-chosen kids must not trigger more fetches")
		})
	}
}

func TestJWKSCacheFailureDoesNotReplaceValidSnapshot(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	for _, provider := range []string{"apple", "google"} {
		t.Run(provider, func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(testJWKS(t, key, "attacker")))
			}))
			t.Cleanup(server.Close)
			old := cachedKey{key: &key.PublicKey, expiresAt: time.Now().Add(time.Hour)}
			var lookup func(string) (*rsa.PublicKey, error)
			switch provider {
			case "apple":
				p := &AppleProvider{cfg: AppleConfig{JWKSEndpoint: server.URL}, httpClient: server.Client(), clock: time.Now, jwksCache: map[string]cachedKey{"old": old}}
				lookup = func(kid string) (*rsa.PublicKey, error) { return p.fetchKey(context.Background(), kid) }
			case "google":
				p := &GoogleProvider{cfg: GoogleConfig{JWKSEndpoint: server.URL}, httpClient: server.Client(), jwksCache: map[string]cachedKey{"old": old}}
				lookup = func(kid string) (*rsa.PublicKey, error) { return p.fetchKey(context.Background(), kid) }
			}
			_, err := lookup("attacker")
			require.Error(t, err, "non-200 response must not supply a key")
			_, err = lookup("old")
			require.NoError(t, err, "failed refresh must preserve unexpired snapshot")
			for i := 0; i < 5; i++ {
				_, err = lookup("missing")
				require.Error(t, err)
			}
			require.EqualValues(t, 1, requests.Load(), "failed refresh still spends the provider opportunity")
		})
	}
}

func TestJWKSCacheConcurrentUnknownKidsShareOneFetch(t *testing.T) {
	for _, provider := range []string{"apple", "google"} {
		t.Run(provider, func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				time.Sleep(10 * time.Millisecond)
				_, _ = w.Write([]byte(`{"keys":[]}`))
			}))
			t.Cleanup(server.Close)
			var lookup func(string) (*rsa.PublicKey, error)
			if provider == "apple" {
				p := &AppleProvider{cfg: AppleConfig{JWKSEndpoint: server.URL}, httpClient: server.Client(), clock: time.Now}
				lookup = func(kid string) (*rsa.PublicKey, error) { return p.fetchKey(context.Background(), kid) }
			} else {
				p := &GoogleProvider{cfg: GoogleConfig{JWKSEndpoint: server.URL}, httpClient: server.Client()}
				lookup = func(kid string) (*rsa.PublicKey, error) { return p.fetchKey(context.Background(), kid) }
			}
			var wg sync.WaitGroup
			for i := 0; i < 20; i++ {
				wg.Add(1)
				go func(kid string) {
					defer wg.Done()
					_, _ = lookup(kid)
				}(string(rune('a' + i)))
			}
			wg.Wait()
			require.EqualValues(t, 1, requests.Load())
		})
	}
}

func TestJWKSCacheRefreshIntervalAndExpiry(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	for _, provider := range []string{"apple", "google"} {
		t.Run(provider, func(t *testing.T) {
			var body atomic.Value
			body.Store(testJWKS(t, key, "old"))
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				value, ok := body.Load().(string)
				if !ok {
					http.Error(w, "invalid JWKS fixture", http.StatusInternalServerError)
					return
				}
				_, _ = w.Write([]byte(value))
			}))
			t.Cleanup(server.Close)
			var lookup func(string) (*rsa.PublicKey, error)
			var advance, expire func()
			if provider == "apple" {
				now := time.Now()
				p := &AppleProvider{cfg: AppleConfig{JWKSEndpoint: server.URL}, httpClient: server.Client(), clock: func() time.Time { return now }}
				lookup = func(kid string) (*rsa.PublicKey, error) { return p.fetchKey(context.Background(), kid) }
				advance = func() { now = now.Add(61 * time.Second) }
				expire = func() { p.jwksCache["new"] = cachedKey{key: &key.PublicKey, expiresAt: now.Add(-time.Second)} }
			} else {
				p := &GoogleProvider{cfg: GoogleConfig{JWKSEndpoint: server.URL}, httpClient: server.Client()}
				lookup = func(kid string) (*rsa.PublicKey, error) { return p.fetchKey(context.Background(), kid) }
				advance = func() { p.jwksLastAttempt = time.Now().Add(-61 * time.Second) }
				expire = func() { p.jwksCache["new"] = cachedKey{key: &key.PublicKey, expiresAt: time.Now().Add(-time.Second)} }
			}
			_, err := lookup("old")
			require.NoError(t, err)
			body.Store(testJWKS(t, key, "new"))
			_, err = lookup("new")
			require.Error(t, err, "rotation is delayed during the bounded miss interval")
			require.EqualValues(t, 1, requests.Load())
			advance()
			_, err = lookup("new")
			require.NoError(t, err, "new signing key must work after interval reopens")
			require.EqualValues(t, 2, requests.Load())
			_, err = lookup("old")
			require.Error(t, err, "old key is withdrawn by the new snapshot")
			expire()
			_, err = lookup("new")
			require.Error(t, err, "expired key cannot be returned from post-refresh lookup")
		})
	}
}

func TestJWKSCacheInvalidResponsePreservesSnapshot(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	for _, provider := range []string{"apple", "google"} {
		for _, body := range []string{`not-json`, `{"keys":[]}`} {
			t.Run(provider+"/"+body, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_, _ = w.Write([]byte(body))
				}))
				t.Cleanup(server.Close)
				old := cachedKey{key: &key.PublicKey, expiresAt: time.Now().Add(time.Hour)}
				var lookup func(string) (*rsa.PublicKey, error)
				if provider == "apple" {
					p := &AppleProvider{cfg: AppleConfig{JWKSEndpoint: server.URL}, httpClient: server.Client(), clock: time.Now, jwksCache: map[string]cachedKey{"old": old}}
					lookup = func(kid string) (*rsa.PublicKey, error) { return p.fetchKey(context.Background(), kid) }
				} else {
					p := &GoogleProvider{cfg: GoogleConfig{JWKSEndpoint: server.URL}, httpClient: server.Client(), jwksCache: map[string]cachedKey{"old": old}}
					lookup = func(kid string) (*rsa.PublicKey, error) { return p.fetchKey(context.Background(), kid) }
				}
				_, err := lookup("new")
				require.Error(t, err)
				_, err = lookup("old")
				require.NoError(t, err)
			})
		}
	}
}

func TestJWKSCacheCanceledCallerCannotCancelSharedRefresh(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	body := testJWKS(t, key, "rotated")
	for _, provider := range []string{"apple", "google"} {
		t.Run(provider, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				close(started)
				<-release
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(server.Close)

			var lookup func(context.Context, string) (*rsa.PublicKey, error)
			if provider == "apple" {
				p := &AppleProvider{cfg: AppleConfig{JWKSEndpoint: server.URL}, httpClient: server.Client(), clock: time.Now}
				lookup = p.fetchKey
			} else {
				p := &GoogleProvider{cfg: GoogleConfig{JWKSEndpoint: server.URL}, httpClient: server.Client()}
				lookup = p.fetchKey
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			firstResult := make(chan error, 1)
			go func() { _, err := lookup(ctx, "rotated"); firstResult <- err }()
			<-started
			cancel()
			select {
			case err := <-firstResult:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(time.Second):
				t.Fatal("canceled caller did not stop waiting")
			}
			secondResult := make(chan error, 1)
			go func() { _, err := lookup(context.Background(), "rotated"); secondResult <- err }()
			close(release)
			select {
			case err := <-secondResult:
				require.NoError(t, err, "caller cancellation must not consume the refresh without caching the rotated key")
			case <-time.After(time.Second):
				t.Fatal("shared refresh did not finish")
			}
			require.EqualValues(t, 1, requests.Load())
		})
	}
}
