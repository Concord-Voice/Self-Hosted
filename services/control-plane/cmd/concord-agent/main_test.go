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
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"schema":1`) {
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

func setupRoot(t *testing.T, installed bool) (cli, assets string) {
	t.Helper()
	root := t.TempDir()
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
	return filepath.Join(deploy, "concord-selfhost"), assets
}

func TestStartSetupSkipsAnInstalledHost(t *testing.T) {
	cli, assets := setupRoot(t, true)
	addr, done, err := startSetup(context.Background(), cli, "127.0.0.1:0", assets, time.Minute)
	if err != nil || addr != nil || done != nil {
		t.Fatalf("setup started on an installed host: %v %v", addr, err)
	}
}

func TestStartSetupServesTLSUntilTheWindowEnds(t *testing.T) {
	cli, assets := setupRoot(t, false)
	addr, done, err := startSetup(context.Background(), cli, "127.0.0.1:0", assets, 300*time.Millisecond)
	if err != nil || addr == nil {
		t.Fatalf("setup did not start: %v", err)
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
}

func TestStartSetupStopsWithTheAgent(t *testing.T) {
	cli, assets := setupRoot(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	_, done, err := startSetup(ctx, cli, "127.0.0.1:0", assets, time.Hour)
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
	cli, assets := setupRoot(t, false)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if _, _, err := startSetup(context.Background(), cli, ln.Addr().String(), assets, time.Minute); err == nil {
		t.Fatal("busy port not reported")
	}
}
