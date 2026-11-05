// Command concord-agent is the Concord Host Agent (#186). It runs as root under
// systemd and serves a fixed set of read-only concord-selfhost actions on a
// unix socket that only root and the concord-agent group can open.
package main

import (
	"context"
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

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/hostagent"
)

const (
	defaultSocket = "/run/concord-agent/agent.sock"
	defaultGroup  = "concord-agent"
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
	if _, err := hostagent.CheckTrusted(cli); err != nil {
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
	ln, err := listen(envOr("CONCORD_AGENT_SOCKET", defaultSocket), gid)
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
		if e.Name() != filepath.Base(path) {
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
