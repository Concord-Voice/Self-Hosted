package clientconfig_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/clientconfig"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Exercise the actual public handler without the unrelated database fixture.
func TestGetConfigChannelDiscovery(t *testing.T) {
	for _, prereleaseAvailable := range []bool{false, true} {
		for _, live := range []bool{false, true} {
			name := "static"
			if live {
				name = "live"
			}
			if prereleaseAvailable {
				name += "/two-streams"
			} else {
				name += "/stable-only"
			}
			t.Run(name, func(t *testing.T) {
				t.Setenv("ENVIRONMENT", "development")
				t.Setenv("SPA_CONFIG_FILE", "")
				t.Setenv("SPA_URL", "https://stable.example.test/release/")
				t.Setenv("SPA_IPC_CONTRACT", "11")
				t.Setenv("SPA_VERSION", "released-audit-identity")
				t.Setenv("SPA_PRERELEASE_URL", "")
				t.Setenv("SPA_PRERELEASE_IPC_CONTRACT", "")
				t.Setenv("SPA_PRERELEASE_VERSION", "")
				if prereleaseAvailable {
					t.Setenv("SPA_PRERELEASE_URL", "https://prerelease.example.test/nightly/")
					t.Setenv("SPA_PRERELEASE_IPC_CONTRACT", "22")
					t.Setenv("SPA_PRERELEASE_VERSION", "nightly-audit-identity")
				}
				cfg, err := config.Load()
				require.NoError(t, err)
				var lsc *config.LiveSpaConfig
				if live {
					lsc = config.NewLiveSpaConfig(cfg, "", time.Hour)
					t.Cleanup(lsc.Stop)
				}
				h := clientconfig.NewHandler(cfg, lsc, nil)
				for _, query := range []string{"", "stable", "prerelease", "pilot", "unknown", "Stable", "PRERELEASE", " prerelease", "prerelease ", "../../prerelease"} {
					t.Run("channel="+query, func(t *testing.T) {
						w := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(w)
						target := "/api/v1/client/config"
						if query != "" {
							target += "?channel=" + url.QueryEscape(query)
						}
						c.Request = httptest.NewRequest(http.MethodGet, target, nil)
						h.GetConfig(c)
						require.Equal(t, http.StatusOK, w.Code)
						assert.Contains(t, w.Header().Get("Cache-Control"), "no-store")
						var body map[string]json.RawMessage
						require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
						var response clientconfig.Response
						require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
						wantURL, wantIPC := "https://stable.example.test/release/", 11
						if query == "prerelease" {
							wantURL, wantIPC = "", 0
							if prereleaseAvailable {
								wantURL, wantIPC = "https://prerelease.example.test/nightly/", 22
							}
						}
						assert.Equal(t, wantURL, response.SpaURL)
						assert.Equal(t, wantIPC, response.SpaIpcContract)
						wantKeys := []string{"minVersion", "featureFlags", "mediaPlaneUrl", "turn"}
						if wantURL != "" {
							wantKeys = append(wantKeys, "spaUrl", "spaIpcContract")
						}
						keys := make([]string, 0, len(body))
						for key := range body {
							keys = append(keys, key)
						}
						assert.ElementsMatch(t, wantKeys, keys, "audit identity and query text must not expand the public response")
					})
				}
			})
		}
	}
}

func TestGetConfigConcurrentLiveReloadReturnsWholeGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spa.env")
	generations := []string{
		"SPA_URL=https://stable.example.test/a/\nSPA_IPC_CONTRACT=11\nSPA_VERSION=a\nSPA_PRERELEASE_URL=https://prerelease.example.test/a/\nSPA_PRERELEASE_IPC_CONTRACT=21\nSPA_PRERELEASE_VERSION=a\n",
		"SPA_URL=https://stable.example.test/b/\nSPA_IPC_CONTRACT=12\nSPA_VERSION=b\nSPA_PRERELEASE_URL=https://prerelease.example.test/b/\nSPA_PRERELEASE_IPC_CONTRACT=22\nSPA_PRERELEASE_VERSION=b\n",
	}
	require.NoError(t, os.WriteFile(path, []byte(generations[0]), 0o600))
	cfg := &config.Config{}
	lsc := config.NewLiveSpaConfig(cfg, path, time.Microsecond)
	t.Cleanup(lsc.Stop)
	h := clientconfig.NewHandler(cfg, lsc, nil)
	// Acknowledge both generations at the HTTP boundary before stress-reading.
	// The fixed request count below must not assume how quickly the watcher runs.
	for _, generation := range []int{1, 0} {
		require.NoError(t, os.WriteFile(path+".next", []byte(generations[generation]), 0o600))
		require.NoError(t, os.Rename(path+".next", path))
		require.Eventually(t, func() bool {
			for _, channel := range []string{"stable", "prerelease"} {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/client/config?channel="+channel, nil)
				h.GetConfig(c)
				var body clientconfig.Response
				if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil {
					return false
				}
				wantURL, wantIPC := "https://"+channel+".example.test/a/", 11
				if channel == "prerelease" {
					wantIPC = 21
				}
				if generation == 1 {
					wantURL = "https://" + channel + ".example.test/b/"
					wantIPC++
				}
				if body.SpaURL != wantURL || body.SpaIpcContract != wantIPC {
					return false
				}
			}
			return true
		}, 5*time.Second, time.Millisecond, "both channels must expose the replacement generation")
	}
	done := make(chan struct{})
	writerResult := make(chan error, 1)
	go func() {
		for i := 0; ; i++ {
			select {
			case <-done:
				writerResult <- nil
				return
			default:
			}
			// Atomic replacement keeps this test about reader coherence, not partial writes.
			if err := os.WriteFile(path+".next", []byte(generations[i%2]), 0o600); err != nil {
				writerResult <- err
				return
			}
			if err := os.Rename(path+".next", path); err != nil {
				writerResult <- err
				return
			}
		}
	}()
	defer func() { close(done); require.NoError(t, <-writerResult) }()
	for range 2000 {
		for _, channel := range []string{"stable", "prerelease"} {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/client/config?channel="+channel, nil)
			h.GetConfig(c)
			require.Equal(t, http.StatusOK, w.Code)
			var body clientconfig.Response
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			pairs := map[string]int{"https://stable.example.test/a/": 11, "https://stable.example.test/b/": 12}
			if channel == "prerelease" {
				pairs = map[string]int{"https://prerelease.example.test/a/": 21, "https://prerelease.example.test/b/": 22}
			}
			ipc, ok := pairs[body.SpaURL]
			if !assert.True(t, ok, "unexpected URL for %s: %s", channel, body.SpaURL) {
				return
			}
			if !assert.Equal(t, ipc, body.SpaIpcContract, "mixed generation response") {
				return
			}
		}
	}
}

func TestGetConfigChannelInvalidStaticTuple(t *testing.T) {
	valid := config.SpaSnapshot{URL: "https://valid.example.test/", IPCContract: 7, Version: "build-1"}
	for _, tc := range []struct {
		name  string
		tuple config.SpaSnapshot
		valid bool
	}{
		{"valid control", valid, true},
		{"zero contract", config.SpaSnapshot{URL: valid.URL}, false},
		{"malformed url", config.SpaSnapshot{URL: "://broken", IPCContract: 7}, false},
		{"invalid audit version", config.SpaSnapshot{URL: valid.URL, IPCContract: 7, Version: "bad version"}, false},
	} {
		for _, channel := range []string{"stable", "prerelease"} {
			t.Run(channel+"/"+tc.name, func(t *testing.T) {
				cfg := &config.Config{
					SpaURL: valid.URL, SpaIpcContract: valid.IPCContract, SpaVersion: valid.Version,
					SpaPrereleaseURL: valid.URL, SpaPrereleaseIpcContract: valid.IPCContract, SpaPrereleaseVersion: valid.Version,
				}
				if channel == "stable" {
					cfg.SpaURL, cfg.SpaIpcContract, cfg.SpaVersion = tc.tuple.URL, tc.tuple.IPCContract, tc.tuple.Version
				} else {
					cfg.SpaPrereleaseURL, cfg.SpaPrereleaseIpcContract, cfg.SpaPrereleaseVersion = tc.tuple.URL, tc.tuple.IPCContract, tc.tuple.Version
				}
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/client/config?channel="+channel, nil)
				clientconfig.NewHandler(cfg, nil, nil).GetConfig(c)
				require.Equal(t, http.StatusOK, w.Code)
				var body map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
				if tc.valid {
					assert.JSONEq(t, `"https://valid.example.test/"`, string(body["spaUrl"]))
					assert.JSONEq(t, `7`, string(body["spaIpcContract"]))
				} else {
					assert.NotContains(t, body, "spaUrl")
					assert.NotContains(t, body, "spaIpcContract")
				}
			})
		}
	}
}
