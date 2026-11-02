package config

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const twoSpaStreams = "SPA_URL=https://stable.example.test/a/\nSPA_IPC_CONTRACT=11\nSPA_VERSION=stable-a\nSPA_PRERELEASE_URL=https://prerelease.example.test/a/\nSPA_PRERELEASE_IPC_CONTRACT=21\nSPA_PRERELEASE_VERSION=nightly-a\n"

func TestSpaSnapshotChannelSelection(t *testing.T) {
	path := writeTempEnvFile(t, twoSpaStreams)
	lsc := NewLiveSpaConfig(&Config{}, path, time.Hour)
	t.Cleanup(lsc.Stop)
	stable := SpaSnapshot{URL: "https://stable.example.test/a/", IPCContract: 11, Version: "stable-a"}
	nightly := SpaSnapshot{URL: "https://prerelease.example.test/a/", IPCContract: 21, Version: "nightly-a"}
	assert.Equal(t, "stable", string(SpaStable))
	assert.Equal(t, "prerelease", string(SpaPrerelease))
	assert.Equal(t, stable, lsc.Snapshot(SpaStable))
	assert.Equal(t, nightly, lsc.Snapshot(SpaPrerelease))
	for _, channel := range []SpaChannel{"", "unknown", "pilot", "PRERELEASE", " prerelease "} {
		assert.Equal(t, stable, lsc.Snapshot(channel), "channel %q defaults to Stable", channel)
	}
	assert.Equal(t, stable.URL, lsc.SpaURL())
	assert.Equal(t, stable.IPCContract, lsc.SpaIpcContract())
}

func TestSpaSnapshotAbsentStreamsAndLegacyVersion(t *testing.T) {
	for _, tc := range []struct {
		name, body      string
		stable, nightly SpaSnapshot
	}{
		{name: "both absent"},
		{name: "explicitly empty", body: "SPA_URL=\nSPA_IPC_CONTRACT=\nSPA_VERSION=\nSPA_PRERELEASE_URL=\nSPA_PRERELEASE_IPC_CONTRACT=\nSPA_PRERELEASE_VERSION=\n"},
		{name: "legacy versionless Stable", body: "SPA_URL=https://legacy.example.test/\nSPA_IPC_CONTRACT=7\n", stable: SpaSnapshot{URL: "https://legacy.example.test/", IPCContract: 7}},
		{name: "only Pre-Release", body: "SPA_PRERELEASE_URL=https://nightly.example.test/\nSPA_PRERELEASE_IPC_CONTRACT=8\nSPA_PRERELEASE_VERSION=nightly\n", nightly: SpaSnapshot{URL: "https://nightly.example.test/", IPCContract: 8, Version: "nightly"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempEnvFile(t, twoSpaStreams)
			lsc := NewLiveSpaConfig(&Config{}, path, time.Hour)
			t.Cleanup(lsc.Stop)
			require.NoError(t, os.WriteFile(path, []byte(tc.body), 0o600))
			lsc.reload()
			assert.Equal(t, tc.stable, lsc.Snapshot(SpaStable))
			assert.Equal(t, tc.nightly, lsc.Snapshot(SpaPrerelease))
		})
	}
}

func TestSpaSnapshotRejectsPartialOtherStreamAtomically(t *testing.T) {
	path := writeTempEnvFile(t, twoSpaStreams)
	lsc := NewLiveSpaConfig(&Config{}, path, time.Hour)
	t.Cleanup(lsc.Stop)
	stable, nightly := lsc.Snapshot(SpaStable), lsc.Snapshot(SpaPrerelease)
	for _, body := range []string{
		"SPA_URL=https://stable.example.test/b/\nSPA_IPC_CONTRACT=12\nSPA_VERSION=stable-b\nSPA_PRERELEASE_VERSION=orphan\n",
		"SPA_URL=https://stable.example.test/b/\nSPA_IPC_CONTRACT=12\nSPA_VERSION=stable-b\nSPA_PRERELEASE_URL=https://prerelease.example.test/b/\nSPA_PRERELEASE_IPC_CONTRACT=invalid\n",
		"SPA_URL=https://stable.example.test/b/\nSPA_PRERELEASE_URL=https://prerelease.example.test/b/\nSPA_PRERELEASE_IPC_CONTRACT=22\nSPA_PRERELEASE_VERSION=nightly-b\n",
	} {
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		lsc.reload()
		assert.Equal(t, stable, lsc.Snapshot(SpaStable))
		assert.Equal(t, nightly, lsc.Snapshot(SpaPrerelease))
	}
	require.NoError(t, os.Remove(path))
	lsc.reload()
	assert.Equal(t, stable, lsc.Snapshot(SpaStable))
	assert.Equal(t, nightly, lsc.Snapshot(SpaPrerelease))
}

func TestSpaSnapshotConcurrentReloadKeepsAuditTupleCoherent(t *testing.T) {
	path := writeTempEnvFile(t, twoSpaStreams)
	lsc := NewLiveSpaConfig(&Config{}, path, time.Hour)
	t.Cleanup(lsc.Stop)
	start := make(chan struct{})
	var readers sync.WaitGroup
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			<-start
			for range 2000 {
				stable := lsc.Snapshot(SpaStable)
				nightly := lsc.Snapshot(SpaPrerelease)
				assert.Contains(t, []SpaSnapshot{
					{URL: "https://stable.example.test/a/", IPCContract: 11, Version: "stable-a"},
					{URL: "https://stable.example.test/b/", IPCContract: 12, Version: "stable-b"},
				}, stable)
				assert.Contains(t, []SpaSnapshot{
					{URL: "https://prerelease.example.test/a/", IPCContract: 21, Version: "nightly-a"},
					{URL: "https://prerelease.example.test/b/", IPCContract: 22, Version: "nightly-b"},
				}, nightly)
			}
		}()
	}
	close(start)
	for i := range 100 {
		body := twoSpaStreams
		if i%2 == 0 {
			body = "SPA_URL=https://stable.example.test/b/\nSPA_IPC_CONTRACT=12\nSPA_VERSION=stable-b\nSPA_PRERELEASE_URL=https://prerelease.example.test/b/\nSPA_PRERELEASE_IPC_CONTRACT=22\nSPA_PRERELEASE_VERSION=nightly-b\n"
		}
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		lsc.reload()
	}
	readers.Wait()
}

func TestSpaSnapshotLoadEnvironmentPreservesInternalVersions(t *testing.T) {
	t.Setenv("ENVIRONMENT", "development")
	for key, value := range map[string]string{
		"SPA_URL": "https://stable.example.test/a/", "SPA_IPC_CONTRACT": "11", "SPA_VERSION": "stable-a",
		"SPA_PRERELEASE_URL": "https://prerelease.example.test/a/", "SPA_PRERELEASE_IPC_CONTRACT": "21", "SPA_PRERELEASE_VERSION": "nightly-a",
	} {
		t.Setenv(key, value)
	}
	for _, version := range []string{"stable-a", "stable-a-audit-corrected", ""} {
		t.Run(fmt.Sprintf("version=%s", version), func(t *testing.T) {
			t.Setenv("SPA_VERSION", version)
			cfg, err := Load()
			require.NoError(t, err)
			lsc := NewLiveSpaConfig(cfg, "", time.Hour)
			t.Cleanup(lsc.Stop)
			assert.Equal(t, SpaSnapshot{URL: "https://stable.example.test/a/", IPCContract: 11, Version: version}, lsc.Snapshot(SpaStable))
			assert.Equal(t, SpaSnapshot{URL: "https://prerelease.example.test/a/", IPCContract: 21, Version: "nightly-a"}, lsc.Snapshot(SpaPrerelease))
		})
	}
}

func TestSpaSnapshotAuditOnlyReloadIsRetained(t *testing.T) {
	path := writeTempEnvFile(t, "SPA_URL=https://stable.example.test/a/\nSPA_IPC_CONTRACT=11\nSPA_VERSION=first-audit\n")
	lsc := NewLiveSpaConfig(&Config{}, path, time.Hour)
	t.Cleanup(lsc.Stop)
	require.Equal(t, "first-audit", lsc.Snapshot(SpaStable).Version)
	require.NoError(t, os.WriteFile(path, []byte("SPA_URL=https://stable.example.test/a/\nSPA_IPC_CONTRACT=11\nSPA_VERSION=corrected-audit\n"), 0o600))
	lsc.reload()
	assert.Equal(t, SpaSnapshot{URL: "https://stable.example.test/a/", IPCContract: 11, Version: "corrected-audit"}, lsc.Snapshot(SpaStable))
}
