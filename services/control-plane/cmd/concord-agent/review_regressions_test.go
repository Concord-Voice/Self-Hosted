package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/hostagent"
)

func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "agent")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	run := filepath.Join(dir, "run")
	if err := os.MkdirAll(run, 0o750); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestListenSweepsStaleWriteMarkerAndAdmissionTemp(t *testing.T) {
	run := socketDir(t)
	stale := []string{writeMarkerFile, backupAdmissionTempPrefix + "1234567890"}
	for _, name := range stale {
		if err := os.WriteFile(filepath.Join(run, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sock := filepath.Join(run, "agent.sock")
	ln, err := listen(sock, os.Getgid())
	if err != nil {
		t.Fatalf("stale crash leftovers stopped the agent starting: %v", err)
	}
	_ = ln.Close()
	for _, name := range stale {
		if _, err := os.Lstat(filepath.Join(run, name)); !os.IsNotExist(err) {
			t.Fatalf("%s was not swept: %v", name, err)
		}
	}
}

func TestListenRefusesLookalikeLeftovers(t *testing.T) {
	for name, build := range map[string]func(string) error{
		"non-numeric temp suffix": func(run string) error {
			return os.WriteFile(filepath.Join(run, backupAdmissionTempPrefix+"x1"), nil, 0o600)
		},
		"bare temp prefix": func(run string) error {
			return os.WriteFile(filepath.Join(run, backupAdmissionTempPrefix), nil, 0o600)
		},
		"marker directory": func(run string) error {
			return os.Mkdir(filepath.Join(run, writeMarkerFile), 0o700)
		},
		"marker symlink": func(run string) error {
			target := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(target, nil, 0o600); err != nil {
				return err
			}
			return os.Symlink(target, filepath.Join(run, writeMarkerFile))
		},
	} {
		t.Run(name, func(t *testing.T) {
			run := socketDir(t)
			if err := build(run); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadDir(run)
			if err != nil {
				t.Fatal(err)
			}
			if ln, err := listen(filepath.Join(run, "agent.sock"), os.Getgid()); err == nil {
				_ = ln.Close()
				t.Fatal("unexpected socket-directory entry accepted")
			}
			after, err := os.ReadDir(run)
			if err != nil || len(after) != len(before) {
				t.Fatalf("refused entry was removed: %v %v", after, err)
			}
		})
	}
}

// postOverSocket serves agent on a real unix socket so the kernel supplies the
// peer; it returns the status and body of POST target.
func postOverSocket(t *testing.T, agent *hostagent.Server, target string) (int, string) {
	t.Helper()
	sock := filepath.Join(socketDir(t), "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: agent, ConnContext: hostagent.PeerConnContext, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	resp, err := client.Post("http://agent"+target, "application/json", strings.NewReader(`{"handle":"h","assertion":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestConfigureWriteAccessNeedsBothAuditAndTrust(t *testing.T) {
	peerVisible := runtime.GOOS == "linux" && os.Getuid() != 0
	for _, tc := range []struct {
		audit, trusted bool
		enabled        bool
		status         int
		reason         string
	}{
		{true, false, false, http.StatusServiceUnavailable, "writes_disabled"},
		{false, true, false, http.StatusServiceUnavailable, "writes_disabled"},
		{false, false, false, http.StatusServiceUnavailable, "writes_disabled"},
		// Positive control: the write route is reached and refuses for want of
		// an authorization store, not because writes are off.
		{true, true, true, http.StatusServiceUnavailable, "authorizer_unavailable"},
	} {
		agent := hostagent.NewServer(hostagent.ExecRunner{})
		if peerVisible {
			if err := agent.SetExpectedPeerUID(os.Getuid()); err != nil {
				t.Fatal(err)
			}
		}
		if err := configureWriteAccess(agent, "/x/concord-selfhost", "/a/b/c/concord-selfhost", filepath.Join(t.TempDir(), "agent.sock"), tc.audit, tc.trusted, ""); err != nil {
			t.Fatal(err)
		}
		if agent.WritesEnabled() != tc.enabled || agent.WritesDisabled() == tc.enabled {
			t.Fatalf("audit=%t trusted=%t: writes enabled=%t disabled=%t", tc.audit, tc.trusted, agent.WritesEnabled(), agent.WritesDisabled())
		}
		if !peerVisible {
			continue // SO_PEERCRED is Linux-only; the route check needs a kernel peer
		}
		status, body := postOverSocket(t, agent, "/v1/actions/start")
		if status != tc.status || !strings.Contains(body, tc.reason) {
			t.Fatalf("audit=%t trusted=%t: got %d %s, want %d %s", tc.audit, tc.trusted, status, body, tc.status, tc.reason)
		}
	}
}

func TestCanonicalTCPSourceKeysIPv6ByPrefix(t *testing.T) {
	key := func(ip string) string {
		t.Helper()
		got, ok := canonicalTCPSource(&net.TCPAddr{IP: net.ParseIP(ip)})
		if !ok {
			t.Fatalf("%s refused", ip)
		}
		return got
	}
	if key("2001:db8:1:2::1") != key("2001:db8:1:2:ffff:ffff:ffff:fffe") {
		t.Fatal("addresses in one /64 got separate per-source budgets")
	}
	if key("2001:db8:1:2::1") == key("2001:db8:1:3::1") {
		t.Fatal("different /64 prefixes share a budget")
	}
	if got := key("2001:db8:1:2:aaaa::1"); got != "2001:db8:1:2::/64" {
		t.Fatalf("IPv6 key %q", got)
	}
	for _, ip := range []string{"192.0.2.1", "::ffff:192.0.2.1"} {
		if got := key(ip); got != "192.0.2.1" {
			t.Fatalf("IPv4 key for %s = %q", ip, got)
		}
	}
}

func TestLimitPerSourceDoubleCloseReleasesOneSlot(t *testing.T) {
	raw := newQueuedListener()
	limited := limitPerSource(raw, 4)
	t.Cleanup(func() { _ = limited.Close() })
	held := make([]net.Conn, 0, 4)
	for range 4 {
		client := queueTCPConn(t, raw, net.ParseIP("192.0.2.1"))
		t.Cleanup(func() { _ = client.Close() })
		conn, err := limited.Accept()
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, conn)
	}
	t.Cleanup(func() {
		for _, c := range held {
			_ = c.Close()
		}
	})
	_ = held[0].Close()
	_ = held[0].Close() // a second close must not release a second slot

	client := queueTCPConn(t, raw, net.ParseIP("192.0.2.1"))
	t.Cleanup(func() { _ = client.Close() })
	conn, err := limited.Accept()
	if err != nil {
		t.Fatalf("released slot not reusable: %v", err)
	}
	held = append(held, conn)

	refused := queueTCPConn(t, raw, net.ParseIP("192.0.2.1"))
	t.Cleanup(func() { _ = refused.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := limited.Accept(); err == nil {
			accepted <- c
		}
	}()
	_ = refused.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := refused.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
		t.Fatalf("over-limit connection was not closed: %v", err)
	}
	select {
	case c := <-accepted:
		_ = c.Close()
		t.Fatal("double close released two slots: a fifth concurrent connection was admitted")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestStartSetupLimitsConnectionsPerSource(t *testing.T) {
	if setupMaxConnsPerSource != 4 {
		t.Fatalf("setup per-source limit %d, want 4", setupMaxConnsPerSource)
	}
	cli, assets, tokenFile := setupRoot(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	addr, _, err := startSetup(ctx, cli, "127.0.0.1:0", assets, tokenFile, time.Minute)
	if err != nil || addr == nil {
		t.Fatalf("setup did not start: %v", err)
	}
	conf := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} //nolint:gosec // self-signed test server
	for i := range setupMaxConnsPerSource {
		c, err := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", addr.String(), conf)
		if err != nil {
			t.Fatalf("connection %d refused under the limit: %v", i+1, err)
		}
		t.Cleanup(func() { _ = c.Close() })
	}
	raw, err := net.DialTimeout("tcp", addr.String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	_ = raw.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := raw.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("fifth connection from one source was not closed by the setup listener: %v", err)
	}
}
