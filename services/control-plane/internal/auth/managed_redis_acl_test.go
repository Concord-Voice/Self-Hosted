package auth_test

import (
	"encoding/json"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type aclManifest struct {
	Services map[string]struct {
		Commands    []string `json:"commands"`
		KeyPatterns []string `json:"keyPatterns"`
	} `json:"services"`
}

func readControlPlaneACL(t *testing.T) (map[string]bool, []string) {
	t.Helper()
	raw, err := os.ReadFile("../managedredis/acl-manifest.json")
	require.NoError(t, err)
	var m aclManifest
	require.NoError(t, json.Unmarshal(raw, &m))
	granted := map[string]bool{}
	for _, c := range m.Services["control-plane"].Commands {
		granted[c] = true
	}
	return granted, m.Services["control-plane"].KeyPatterns
}

// The managed-Redis ACL checks every command a Lua script calls, not only
// EVAL, so a script command the manifest does not grant fails with NOPERM
// under managed credentials. DECR, for one, is not granted: DECRBY is.
func TestAuthLuaRedisCommandsAreInTheManagedRedisACL(t *testing.T) {
	granted, _ := readControlPlaneACL(t)
	call := regexp.MustCompile(`redis\.call\(["']([A-Za-z]+)["']`)
	handlers, err := os.ReadFile("handlers.go")
	require.NoError(t, err)
	pending, err := os.ReadFile("pending.go")
	require.NoError(t, err)
	for file, src := range map[string][]byte{"handlers.go": handlers, "pending.go": pending} {
		for _, m := range call.FindAllStringSubmatch(string(src), -1) {
			cmd := strings.ToUpper(m[1])
			assert.True(t, granted[cmd], "%s (in %s) is not granted to the control-plane in acl-manifest.json", cmd, file)
		}
	}
}

// A key kind missing from the manifest gets NOPERM under managed credentials:
// recovery verify-code then answered 503 for every caller.
func TestAuthAttemptBudgetKeysAreInTheManagedRedisACL(t *testing.T) {
	_, patterns := readControlPlaneACL(t)
	for _, key := range []string{
		"recovery_fail:user@example.com",
		"email_verify_attempts:issued:user@example.com",
		"email_verify_attempts:3f6c0b8e-0000-4000-8000-000000000001",
	} {
		listed := false
		for _, p := range patterns {
			if ok, _ := path.Match(p, key); ok {
				listed = true
				break
			}
		}
		assert.True(t, listed, "%q is not covered by a control-plane key pattern in acl-manifest.json", key)
	}
}
