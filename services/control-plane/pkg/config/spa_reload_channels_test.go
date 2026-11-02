package config

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These assertions use the legacy API so malformed reloads are behavior-red
// independently of the new Snapshot API's availability.
func TestSpaChannelReloadRejectsPartialCandidate(t *testing.T) {
	for _, candidate := range []struct{ name, body string }{
		{"url without contract", "SPA_URL=https://next.example.test/\n"},
		{"contract without url", "SPA_IPC_CONTRACT=2\n"},
		{"invalid integer", "SPA_URL=https://next.example.test/\nSPA_IPC_CONTRACT=oops\n"},
		{"negative contract", "SPA_URL=https://next.example.test/\nSPA_IPC_CONTRACT=-1\n"},
		{"zero contract", "SPA_URL=https://next.example.test/\nSPA_IPC_CONTRACT=0\n"},
		{"malformed url", "SPA_URL=://broken\nSPA_IPC_CONTRACT=2\n"},
		{"version alone", "SPA_VERSION=next\n"},
		{"partial other stream aborts entire candidate", "SPA_URL=https://next.example.test/\nSPA_IPC_CONTRACT=2\nSPA_VERSION=next\nSPA_PRERELEASE_URL=https://nightly.example.test/\n"},
	} {
		t.Run(candidate.name, func(t *testing.T) {
			path := writeTempEnvFile(t, "SPA_URL=https://previous.example.test/\nSPA_IPC_CONTRACT=1\nSPA_VERSION=previous\n")
			lsc := NewLiveSpaConfig(&Config{}, path, time.Hour)
			t.Cleanup(lsc.Stop)
			require.NoError(t, os.WriteFile(path, []byte(candidate.body), 0o600))
			lsc.reload()
			assert.Equal(t, "https://previous.example.test/", lsc.SpaURL())
			assert.Equal(t, 1, lsc.SpaIpcContract())
		})
	}
}
