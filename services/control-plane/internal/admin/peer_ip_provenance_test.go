package admin_test

// Client-IP provenance regression for the admin password login (VULN-008
// family). Admin login keys its per-IP lockout on c.ClientIP(), which the
// production router (api.configureTrustedProxies) resolves from forwarding
// headers only for an exactly configured trusted proxy. This engine applies
// the same production shape — one exact trusted proxy host route — so:
//
//   - an untrusted peer that claims another address is counted as itself;
//   - the trusted proxy's forwarded client is counted as that client, which
//     proves the assertion reads the key the handler actually wrote.
//
// Mutant killed: a handler that reads a forwarding header directly, or keys
// on anything but c.ClientIP().

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

const (
	adminTrustedProxyIP = "10.20.30.40"
	adminUntrustedPeer  = "203.0.113.77"
	adminForwardedIP    = "198.51.100.88"
)

// adminIPLockKey is the documented admin lockout key: the raw IP never
// appears, only its hex SHA-256.
func adminIPLockKey(ip string) string {
	sum := sha256.Sum256([]byte(ip))
	return "admin_lockout:ip:" + hex.EncodeToString(sum[:])
}

func adminIPCounted(t *testing.T, rdb *redis.Client, ip string) bool {
	t.Helper()
	n, err := rdb.Exists(context.Background(), adminIPLockKey(ip)).Result()
	require.NoError(t, err)
	return n > 0
}

func postAdminLoginVia(engine *gin.Engine, peer, forwarded, username string) int {
	b, _ := json.Marshal(map[string]string{"username": username, "password": "wrong-password"})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/v1/auth/password", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", forwarded)
	req.Header.Set("X-Real-IP", forwarded)
	req.RemoteAddr = peer + ":1234"
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec.Code
}

func TestAdminPasswordLogin_SpoofedForwardingKeysPeer(t *testing.T) {
	_, rdb, engine := setupEnrollLockout(t)
	require.NoError(t, engine.SetTrustedProxies([]string{adminTrustedProxyIP + "/32"}))

	code := postAdminLoginVia(engine, adminUntrustedPeer, testhelpers.SpoofedClientIP, uniqueAdminUsername("peerip"))
	require.Equal(t, http.StatusUnauthorized, code)

	assert.True(t, adminIPCounted(t, rdb, adminUntrustedPeer), "the guess must be counted against the real peer")
	assert.False(t, adminIPCounted(t, rdb, testhelpers.SpoofedClientIP), "a spoofed forwarding header must not choose the lockout key")
}

func TestAdminPasswordLogin_TrustedProxyForwardedClientIsKeyed(t *testing.T) {
	_, rdb, engine := setupEnrollLockout(t)
	require.NoError(t, engine.SetTrustedProxies([]string{adminTrustedProxyIP + "/32"}))

	code := postAdminLoginVia(engine, adminTrustedProxyIP, adminForwardedIP, uniqueAdminUsername("peeripfwd"))
	require.Equal(t, http.StatusUnauthorized, code)

	assert.True(t, adminIPCounted(t, rdb, adminForwardedIP), "the trusted proxy's client must be the lockout key")
	assert.False(t, adminIPCounted(t, rdb, adminTrustedProxyIP), "the proxy itself must not absorb every client's guesses")
}
