package main

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/hostagent"
)

func TestListenPermissions(t *testing.T) {
	dir, err := os.MkdirTemp("", "agent")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "run", "agent.sock")

	for i := range 2 { // second pass replaces the stale socket
		if i == 1 {
			// A token file left by a crash must not stop the agent starting.
			if err := os.WriteFile(filepath.Join(filepath.Dir(sock), setupTokenFile), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		ln, err := listen(sock, os.Getgid())
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		st, err := os.Stat(sock)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o660 || st.Mode()&os.ModeSocket == 0 {
			t.Fatalf("socket mode %v", st.Mode())
		}
		if dst, _ := os.Stat(filepath.Dir(sock)); dst.Mode().Perm() != 0o750 {
			t.Fatalf("dir mode %v", dst.Mode())
		}
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.Close()
		ln.(*net.UnixListener).SetUnlinkOnClose(false)
		_ = ln.Close()
	}

	if err := os.Remove(sock); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listen(sock, os.Getgid()); err == nil {
		t.Fatal("regular file at socket path was replaced")
	}

	// A shared directory (like /run) is refused before its mode is touched.
	_ = os.Remove(sock)
	_ = os.Remove(filepath.Join(filepath.Dir(sock), setupTokenFile))
	other := filepath.Join(filepath.Dir(sock), "other")
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(filepath.Dir(sock), 0o755) //nolint:gosec // checked below for being left alone
	if _, err := listen(sock, os.Getgid()); err == nil {
		t.Fatal("shared socket directory accepted")
	}
	if dst, _ := os.Stat(filepath.Dir(sock)); dst.Mode().Perm() != 0o755 {
		t.Fatalf("shared dir mode changed to %v", dst.Mode())
	}
}

func TestRunRefusesUntrustedCLI(t *testing.T) {
	t.Setenv("CONCORD_AGENT_CLI", "relative/concord-selfhost")
	if run() != 1 {
		t.Fatal("untrusted cli accepted")
	}
	t.Setenv("CONCORD_AGENT_CLI", "")
	if run() != 1 {
		t.Fatal("empty cli accepted")
	}
}

func TestExpectedControlPlaneUIDRejectsRootAndInvalidValues(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
	}{
		{value: "10006", want: 10006},
		{value: "", want: 10006},
		{value: "0"},
		{value: "-1"},
		{value: "10006 "},
		{value: "4294967296"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("CONCORD_AGENT_CONTROL_PLANE_UID", tc.value)
			got, err := expectedControlPlaneUID()
			if tc.want == 0 {
				if err == nil {
					t.Fatalf("accepted invalid UID %q: %d", tc.value, got)
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("UID %q: got %d, %v; want %d", tc.value, got, err, tc.want)
			}
		})
	}
}

func TestRunServesAndStopsOnSIGTERM(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(wd, "trusted-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	_ = os.Chmod(dir, 0o755) //nolint:gosec // the trust check expects a normal 0755 directory
	cli := filepath.Join(dir, "concord-selfhost")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\necho '{\"schema\":1,\"services\":[]}'\n"), 0o755); err != nil { //nolint:gosec // the fake CLI must be executable
		t.Fatal(err)
	}
	if _, err := hostagent.CheckTrusted(cli); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("checkout is not trusted here: %v", err)
		}
		t.Skipf("checkout is not trusted here: %v", err)
	}
	grp, err := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	if err != nil {
		t.Skip("current group has no name")
	}
	sockDir, err := os.MkdirTemp("", "agent")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "agent.sock")
	t.Setenv("CONCORD_AGENT_CLI", cli)
	t.Setenv("CONCORD_AGENT_GROUP", grp.Name)
	t.Setenv("CONCORD_AGENT_SOCKET", sock)
	uid := os.Getuid()
	if uid == 0 {
		uid = 10006
	}
	t.Setenv("CONCORD_AGENT_CONTROL_PLANE_UID", strconv.Itoa(uid))

	done := make(chan int, 1)
	go func() { done <- run() }()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	var resp *http.Response
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if resp, err = client.Get("http://agent/v1/status"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent never answered: %v", err)
		}
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	wantStatus := http.StatusForbidden
	if runtime.GOOS == "linux" && os.Getuid() != 0 {
		wantStatus = http.StatusOK
	}
	if resp.StatusCode != wantStatus || wantStatus == http.StatusOK && !strings.Contains(string(body), `"schema":1`) {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("agent did not stop on SIGTERM")
	}
}

// setupRoot builds an install root under the package directory: /tmp is
// world-writable and would fail the wizard directory's trust check.
func setupRoot(t *testing.T, installed bool) (cli, assets, tokenFile string) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hostagent.CheckTrustedTree(wd); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("checkout itself is not trusted here: %v", err)
		}
		t.Skipf("checkout itself is not trusted here: %v", err)
	}
	root, err := os.MkdirTemp(wd, "setup-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	deploy := filepath.Join(root, "infrastructure", "deploy")
	assets = filepath.Join(root, "wizard")
	for _, d := range []string{deploy, assets} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(assets, "index.html"), []byte("wizard"), 0o600); err != nil {
		t.Fatal(err)
	}
	if installed {
		if err := os.WriteFile(filepath.Join(root, ".env"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(deploy, "concord-selfhost"), assets, filepath.Join(t.TempDir(), setupTokenFile)
}

func TestStartSetupSkipsAnInstalledHost(t *testing.T) {
	cli, assets, tokenFile := setupRoot(t, true)
	if err := os.WriteFile(tokenFile, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	addr, done, err := startSetup(context.Background(), cli, "127.0.0.1:0", assets, tokenFile, time.Minute)
	if err != nil || addr != nil || done != nil {
		t.Fatalf("setup started on an installed host: %v %v", addr, err)
	}
	if _, err := os.Lstat(tokenFile); !os.IsNotExist(err) {
		t.Fatalf("stale token file kept: %v", err)
	}
}

func TestStartSetupServesTLSUntilTheWindowEnds(t *testing.T) {
	cli, assets, tokenFile := setupRoot(t, false)
	addr, done, err := startSetup(context.Background(), cli, "127.0.0.1:0", assets, tokenFile, 300*time.Millisecond)
	if err != nil || addr == nil {
		t.Fatalf("setup did not start: %v", err)
	}
	st, err := os.Stat(tokenFile)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", st, err)
	}
	line, _ := os.ReadFile(tokenFile) //nolint:gosec // path from the test
	_, port, _ := net.SplitHostPort(addr.String())
	if !strings.HasPrefix(string(line), "https://<this-host>:"+port+"/#token=") || len(line) != len("https://<this-host>:"+port+"/#token=")+65 {
		t.Fatalf("token file content %q", line)
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // self-signed test server
	}}
	resp, err := client.Get("https://" + addr.String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "wizard" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("setup listener outlived its window")
	}
	if _, err := net.DialTimeout("tcp", addr.String(), time.Second); err == nil {
		t.Fatal("setup port still open after the window")
	}
	if _, err := os.Lstat(tokenFile); !os.IsNotExist(err) {
		t.Fatalf("token file outlived the window: %v", err)
	}
}

func TestStartSetupStopsWithTheAgent(t *testing.T) {
	cli, assets, tokenFile := setupRoot(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	_, done, err := startSetup(ctx, cli, "127.0.0.1:0", assets, tokenFile, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("setup listener outlived the agent")
	}
}

func TestStartSetupReportsABusyPort(t *testing.T) {
	cli, assets, tokenFile := setupRoot(t, false)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if _, _, err := startSetup(context.Background(), cli, ln.Addr().String(), assets, tokenFile, time.Minute); err == nil {
		t.Fatal("busy port not reported")
	}
}

func TestStartSetupRefusesAWritableWizardDir(t *testing.T) {
	cli, assets, tokenFile := setupRoot(t, false)
	if err := os.Chmod(filepath.Join(assets, "index.html"), 0o666); err != nil { //nolint:gosec // the point of the test
		t.Fatal(err)
	}
	if _, _, err := startSetup(context.Background(), cli, "127.0.0.1:0", assets, tokenFile, time.Minute); err == nil ||
		!strings.Contains(err.Error(), "untrusted wizard directory") {
		t.Fatalf("writable wizard served: %v", err)
	}
	if _, err := os.Lstat(tokenFile); !os.IsNotExist(err) {
		t.Fatalf("token written for a refused setup: %v", err)
	}
}
