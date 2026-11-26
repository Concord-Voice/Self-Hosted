package testhelpers

import (
	"context"
	"net/http"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// Client-IP provenance fixtures (VULN-008 family). Every per-IP auth control —
// the auth-failure counter, the auth ban, the per-IP rate limits — keys on
// gin's c.ClientIP(). The router honours forwarding headers only from an
// exactly configured trusted proxy, so a request from any other peer must be
// keyed by its TCP peer address, whatever headers it sends.
const (
	// TestPeerIP is the RemoteAddr host httptest.NewRequest assigns, which is
	// the real TCP peer of every TestServer.DoRequest call. The TestServer
	// config trusts no proxy, so this peer is untrusted.
	TestPeerIP = "192.0.2.1"
	// SpoofedClientIP is the address an untrusted peer claims in its
	// forwarding headers. No Redis key may ever contain it.
	SpoofedClientIP = "198.51.100.23"
)

// WithSpoofedForwarding returns a copy of h (or a new header set when h is
// nil) that claims SpoofedClientIP in every forwarding header gin reads.
func WithSpoofedForwarding(h http.Header) http.Header {
	out := http.Header{}
	if h != nil {
		out = h.Clone()
	}
	out.Set("X-Forwarded-For", SpoofedClientIP)
	out.Set("X-Real-IP", SpoofedClientIP)
	return out
}

// RequireNoRedisKeyMentions fails the test when any Redis key contains ip.
func RequireNoRedisKeyMentions(t *testing.T, rdb *redis.Client, ip string) {
	t.Helper()
	keys, err := rdb.Keys(context.Background(), "*"+ip+"*").Result()
	require.NoError(t, err)
	require.Empty(t, keys, "no Redis key may be derived from a spoofed forwarding header")
}

// RequireAuthFailureKeyedToPeer asserts that RecordAuthFailure counted the
// failure against peerIP (as a counter or, at the threshold, a ban) and that
// no key was derived from SpoofedClientIP.
func RequireAuthFailureKeyedToPeer(t *testing.T, rdb *redis.Client, peerIP string) {
	t.Helper()
	n, err := rdb.Exists(context.Background(), "auth_failures:ip:"+peerIP, "auth_ban:ip:"+peerIP).Result()
	require.NoError(t, err)
	require.Positive(t, n, "the auth failure must be recorded against the real peer %s", peerIP)
	RequireNoRedisKeyMentions(t, rdb, SpoofedClientIP)
}
