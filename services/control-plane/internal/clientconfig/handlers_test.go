package clientconfig_test

import (
	"net/http"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTS(t *testing.T) *testhelpers.TestServer {
	t.Helper()
	return testhelpers.SetupTestServer(t)
}

func TestGetConfigReturnsExpectedShape(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", "/api/v1/client/config", nil, nil)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)

	// Verify all expected top-level keys exist
	require.Contains(t, resp, "minVersion")
	require.Contains(t, resp, "featureFlags")
	require.Contains(t, resp, "mediaPlaneUrl")
	require.Contains(t, resp, "turn")
	require.NotContains(t, resp, "telemetryPolicyVersion", "field removed from /client/config response")

	// Feature flags should be a nested object with voice/video/e2ee
	flags, ok := resp["featureFlags"].(map[string]interface{})
	require.True(t, ok, "featureFlags should be an object")
	assert.Contains(t, flags, "voice")
	assert.Contains(t, flags, "video")
	assert.Contains(t, flags, "e2ee")

	// TURN should be a nested object (fields may be omitted when empty)
	_, ok = resp["turn"].(map[string]interface{})
	require.True(t, ok, "turn should be an object")
}

func TestGetConfigReflectsConfigValues(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", "/api/v1/client/config", nil, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)

	// Default test config: no min version, features enabled by default in config.Load()
	// but test config uses zero-value Config{} so features default to false
	assert.Equal(t, "", resp["minVersion"])

	flags := resp["featureFlags"].(map[string]interface{})
	assert.Equal(t, false, flags["voice"])
	assert.Equal(t, false, flags["video"])
	assert.Equal(t, false, flags["e2ee"])

	// Default media plane URL is empty in test config
	assert.Equal(t, "", resp["mediaPlaneUrl"])

	// SPA fields omitted when zero-valued (omitempty)
	assert.Nil(t, resp["spaUrl"])
	assert.Nil(t, resp["spaIpcContract"])
}

func TestGetConfigNoAuthRequired(t *testing.T) {
	ts := setupTS(t)

	// Endpoint should work without any auth headers
	w := ts.DoRequest("GET", "/api/v1/client/config", nil, nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestGetConfigEmptyTURNFieldsOmitted(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", "/api/v1/client/config", nil, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)

	// With empty TURN host/realm (test config default), the turn object
	// should exist but contain no host/realm keys (omitempty on both fields)
	turn, ok := resp["turn"].(map[string]interface{})
	require.True(t, ok, "turn should be an object")
	assert.NotContains(t, turn, "host", "empty host should be omitted")
	assert.NotContains(t, turn, "realm", "empty realm should be omitted")
}

func TestGetConfigSpaFieldsOmittedWhenZero(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", "/api/v1/client/config", nil, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)

	// spaUrl and spaIpcContract have omitempty — absent when zero-valued
	assert.Nil(t, resp["spaUrl"], "empty spaUrl should be omitted")
	assert.Nil(t, resp["spaIpcContract"], "zero spaIpcContract should be omitted")
}

func TestGetConfigContentTypeJSON(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", "/api/v1/client/config", nil, nil)
	require.Equal(t, http.StatusOK, w.Code)

	ct := w.Header().Get("Content-Type")
	assert.Contains(t, ct, "application/json")
}

func TestGetConfigFeatureFlagsIndependent(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", "/api/v1/client/config", nil, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)

	flags := resp["featureFlags"].(map[string]interface{})

	// Test config has all flags at zero value (false) — verify each is
	// independently false rather than missing
	assert.Equal(t, false, flags["voice"], "voice should be explicitly false")
	assert.Equal(t, false, flags["video"], "video should be explicitly false")
	assert.Equal(t, false, flags["e2ee"], "e2ee should be explicitly false")
	assert.Equal(t, false, flags["gifsEnabled"], "gifsEnabled should be explicitly false when no KLIPY credential is configured")

	// Verify exactly 4 keys — no extra fields leaking. CRITICALLY: the
	// raw KLIPY app key must NEVER appear here.
	assert.Len(t, flags, 4, "featureFlags should have exactly 4 keys")
	_, hasKey := flags["klipyApiKey"]
	assert.False(t, hasKey, "klipyApiKey must NEVER be present in /client/config — it would leak the server-side KLIPY app key into the renderer bundle")
}
