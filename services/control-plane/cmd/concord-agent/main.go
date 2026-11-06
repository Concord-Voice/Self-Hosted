// Command concord-agent is the Concord Host Agent (#186). It runs as root under
// systemd and serves a fixed set of read-only concord-selfhost actions on a
// unix socket that only root and the concord-agent group can open. On a host
// with no .env it also serves the first-run setup wizard over HTTPS (#187).
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/net/netutil"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/hostagent"
)

const (
	defaultSocket = "/run/concord-agent/agent.sock"
	defaultGroup  = "concord-agent"
	// The setup wizard (#187) listens on every interface: the operator opens
	// it from another machine on the LAN.
	defaultSetupAddr = ":9443"
	defaultWizardDir = "/usr/share/concord-agent/wizard"
	// setupTokenFile sits beside the socket, in the agent's own 0750
	// directory, and is itself 0600 root: the journal is readable by the adm
	// and systemd-journal groups, so the token never goes there.
	setupTokenFile = "setup-token"
	// setupMaxConns bounds the TLS connections a LAN client can hold open.
	setupMaxConns = 16
)

func main() {
	os.Exit(run())
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func run() int {
	cli := os.Getenv("CONCORD_AGENT_CLI")
	resolved, err := hostagent.CheckTrusted(cli)
	if err != nil {
		log.Printf("concord-agent: startup failed: untrusted cli: %v", err)
		return 1
	}
	grp, err := user.LookupGroup(envOr("CONCORD_AGENT_GROUP", defaultGroup))
	if err != nil {
		log.Printf("concord-agent: startup failed: group: %v", err)
		return 1
	}
	gid, err := strconv.Atoi(grp.Gid)
	if err != nil {
		log.Print("concord-agent: startup failed: group id")
		return 1
	}
	sock := envOr("CONCORD_AGENT_SOCKET", defaultSocket)
	ln, err := listen(sock, gid)
	if err != nil {
		log.Printf("concord-agent: startup failed: socket: %v", err)
		return 1
	}

	srv := &http.Server{
		Handler:           hostagent.NewServer(hostagent.ExecRunner{CLI: cli}),
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    8 << 10,
		IdleTimeout:       60 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	// Setup mode is an extra listener; the socket serves either way, so a
	// setup failure is logged and the agent carries on.
	if _, _, err := startSetup(ctx, resolved, envOr("CONCORD_AGENT_SETUP_ADDR", defaultSetupAddr),
		envOr("CONCORD_AGENT_WIZARD_DIR", defaultWizardDir), filepath.Join(filepath.Dir(sock), setupTokenFile),
		hostagent.SetupWindow); err != nil {
		log.Printf("concord-agent: setup mode unavailable: %v", err)
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx) // best-effort: the agent is exiting
	}()
	log.Print("concord-agent: listening")
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("concord-agent: serve: %v", err)
		return 1
	}
	return 0
}

// startSetup serves the first-run wizard over HTTPS when the install root
// (three levels above the CLI) has no .env. It returns a nil address when the
// host is already installed. The listener closes after a successful install,
// after window, or when ctx ends; done is closed once it has. The wizard
// address with its token goes to tokenFile, which is removed on close.
func startSetup(ctx context.Context, cli, addr, assets, tokenFile string, window time.Duration) (net.Addr, <-chan struct{}, error) {
	_ = os.Remove(tokenFile) // a token from an earlier run is dead
	deploy := filepath.Dir(cli)
	envPath := filepath.Join(filepath.Dir(filepath.Dir(deploy)), ".env")
	if !hostagent.Needed(envPath) {
		return nil, nil, nil
	}
	// The page reads the token, so only root may change what it is built from.
	assets, err := hostagent.CheckTrustedTree(assets)
	if err != nil {
		return nil, nil, fmt.Errorf("untrusted wizard directory: %w", err)
	}
	installer := hostagent.ExecRunner{CLI: filepath.Join(deploy, "install-selfhost.sh"), Combined: true}
	setup, token, err := hostagent.NewSetup(installer, envPath, assets)
	if err != nil {
		return nil, nil, err
	}
	cfg, fingerprint, err := hostagent.SelfSignedTLS(time.Now())
	if err != nil {
		return nil, nil, err
	}
	tcp, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	port := 0
	if a, ok := tcp.Addr().(*net.TCPAddr); ok {
		port = a.Port
	}
	if err := writeTokenFile(tokenFile, fmt.Sprintf("https://<this-host>:%d/#token=%s\n", port, token)); err != nil {
		_ = tcp.Close()
		return nil, nil, err
	}
	ln := tls.NewListener(netutil.LimitListener(tcp, setupMaxConns), cfg)
	srv := &http.Server{
		Handler:           setup,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		MaxHeaderBytes:    8 << 10,
		IdleTimeout:       60 * time.Second,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("concord-agent: setup: serve: %v", err)
		}
	}()
	go func() {
		timer := time.NewTimer(window)
		defer timer.Stop()
		select {
		case <-setup.Done():
		case <-timer.C:
			log.Print("concord-agent: setup window ended; restart the agent for a new token")
		case <-ctx.Done():
		}
		_ = os.Remove(tokenFile) // best-effort: the token is dead either way
		// An install already running may finish: it writes .env atomically.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 11*time.Minute)
		defer cancel()
		_ = srv.Shutdown(sctx) // best-effort: Serve returns either way
	}()
	log.Printf("concord-agent: setup mode on port %d; the wizard address with its one-time token is in %s (root only)", port, tokenFile)
	log.Printf("concord-agent: setup certificate SHA-256 fingerprint %s; setup closes in %s", fingerprint, window)
	return ln.Addr(), done, nil
}

// writeTokenFile creates path afresh as 0600, refusing anything already there.
func writeTokenFile(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // fixed path beside the socket
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// listen opens the socket with no window in which it is world-accessible:
// the directory is 0750 root:group (owner is the agent, root under systemd), the umask makes the socket 0600, and it is
// then handed to the group as 0660.
func listen(path string, gid int) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	// The directory is chowned and chmodded below, so it must be one the
	// agent owns outright: refuse a shared directory such as /run.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(path) && e.Name() != setupTokenFile {
			return nil, fmt.Errorf("socket directory %s is not empty; use a dedicated directory", dir)
		}
	}
	if err := os.Chown(dir, os.Geteuid(), gid); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o750); err != nil { //nolint:gosec // a directory needs its search bit for the group
		return nil, err
	}
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("socket path exists and is not a socket")
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	if err := os.Chown(path, os.Geteuid(), gid); err != nil {
		_ = ln.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil { //nolint:gosec // the concord-agent group must be able to connect
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}
