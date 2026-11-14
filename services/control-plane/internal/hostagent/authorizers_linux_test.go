//go:build linux

package hostagent

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// The production authorizer file is root-owned but projected read-only into
// the nonroot control-plane container. Exercise that access with real UIDs.
func TestAuthorizerNonrootProjection(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to drop a subprocess to the control-plane UID")
	}
	root, err := os.MkdirTemp("/tmp", "agent-authorizer-projection-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	path := writeAuthorizerDoc(t, root, testAuthorizerDoc(t))
	require.NoError(t, os.Chmod(root, 0o755))                                                //nolint:gosec // test-only read projection
	command := exec.Command(os.Args[0], "-test.run=^TestAuthorizerNonrootProjectionHelper$") //nolint:gosec // fixed test binary and selector
	command.Env = append(os.Environ(), "CV_AGENT_PROJECTION_TEST_PATH="+path)
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
}

func TestAuthorizerNonrootProjectionHelper(t *testing.T) {
	path := os.Getenv("CV_AGENT_PROJECTION_TEST_PATH")
	if path == "" {
		return
	}
	store, err := loadAuthorizersAt(path, 0, filepath.Dir(path))
	require.NoError(t, err)
	list, err := store.List()
	require.NoError(t, err)
	require.Len(t, list, 1)
	file, err := os.OpenFile(path, os.O_WRONLY, 0) //nolint:gosec // child test opens its parent-created read-only projection
	if err == nil {
		_ = file.Close()
	}
	require.ErrorIs(t, err, os.ErrPermission)
}
