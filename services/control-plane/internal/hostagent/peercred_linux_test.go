//go:build linux

package hostagent

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUnixPeerCredentialsAndWrongUIDAdmission(t *testing.T) {
	require.Equal(t, http.StatusForbidden, unixPeerStatus(t, os.Getuid()+1))
	if os.Getuid() > 0 {
		require.Equal(t, http.StatusOK, unixPeerStatus(t, os.Getuid()))
	} else {
		require.Equal(t, http.StatusOK, unixSubprocessStatus(t, 65534))
		require.Equal(t, http.StatusForbidden, unixSubprocessStatus(t, 10006))
	}
}

// TestUnixPeerSubprocessHelper is invoked under a different kernel UID only by
// the parent test. Its HTTP response is printed to the parent test process.
func TestUnixPeerSubprocessHelper(t *testing.T) {
	path := os.Getenv("CV_AGENT_PEER_TEST_SOCKET")
	if path == "" {
		return
	}
	client := &http.Client{Transport: &http.Transport{DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
		return net.Dial("unix", path)
	}}}
	response, err := client.Get("http://unix/v1/status")
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	fmt.Print(response.StatusCode)
}

func unixSubprocessStatus(t *testing.T, expectedUID int) int {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "agent-peer-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "peer.sock")
	listener, err := net.Listen("unix", path)
	require.NoError(t, err)
	defer listener.Close()
	// The test process owns this temporary socket; allow the dropped-UID
	// subprocess to reach it so SO_PEERCRED, not filesystem mode, decides.
	require.NoError(t, os.Chmod(dir, 0o755))  //nolint:gosec // test-only peer gate fixture
	require.NoError(t, os.Chmod(path, 0o666)) //nolint:gosec // test-only peer gate fixture
	server := NewServer(&fakeRunner{out: statusJSON})
	require.NoError(t, server.SetExpectedPeerUID(expectedUID))
	httpServer := &http.Server{Handler: server, ConnContext: PeerConnContext, ReadHeaderTimeout: time.Second}
	go func() { _ = httpServer.Serve(listener) }()
	defer httpServer.Close()
	command := exec.Command(os.Args[0], "-test.run=^TestUnixPeerSubprocessHelper$") //nolint:gosec // fixed current test binary and fixed test selector
	command.Env = append(os.Environ(), "CV_AGENT_PEER_TEST_SOCKET="+path)
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	output, err := command.Output()
	require.NoError(t, err)
	// Go's test runner also writes a PASS line; the helper's status is first.
	require.GreaterOrEqual(t, len(output), 3)
	status, err := strconv.Atoi(string(output[:3]))
	require.NoError(t, err)
	return status
}

func unixPeerStatus(t *testing.T, expectedUID int) int {
	t.Helper()
	path := filepath.Join(t.TempDir(), "peer.sock")
	listener, err := net.Listen("unix", path)
	require.NoError(t, err)
	defer listener.Close()
	server := NewServer(&fakeRunner{out: statusJSON})
	require.NoError(t, server.SetExpectedPeerUID(expectedUID))
	httpServer := &http.Server{Handler: server, ConnContext: PeerConnContext, ReadHeaderTimeout: time.Second}
	go func() { _ = httpServer.Serve(listener) }()
	defer httpServer.Close()
	client := &http.Client{Transport: &http.Transport{DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
		return net.Dial("unix", path)
	}}}
	response, err := client.Get("http://unix/v1/status")
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	return response.StatusCode
}
