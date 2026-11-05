package hostagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// ExecRunner runs the concord-selfhost CLI at a fixed absolute path.
type ExecRunner struct {
	CLI string
}

// Run checks the CLI is still trusted, then runs it with a fixed environment,
// no stdin, and stderr discarded. A cancelled context kills the whole process
// group, so `docker compose logs -f` children do not outlive the request.
func (e ExecRunner) Run(ctx context.Context, args []string, w io.Writer) error {
	if err := CheckTrusted(e.CLI); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, e.CLI, args...) //nolint:gosec // fixed CLI path checked above; args come from the server's allowlists
	cmd.Env = []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"LANG=C.UTF-8",
		"HOME=/root",
	}
	cmd.Dir = filepath.Dir(e.CLI)
	cmd.Stdout = w
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	return cmd.Run()
}

// CheckTrusted refuses a CLI that anyone but root (or the agent's own user)
// could change: the file, every file beside it (the CLI sources its
// neighbours), and every ancestor directory must be owned by root or the
// agent's user and not writable by group or others.
func CheckTrusted(cli string) error {
	if !filepath.IsAbs(cli) {
		return errors.New("cli path must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(cli)
	if err != nil {
		return err
	}
	st, err := os.Lstat(resolved)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Mode()&0o111 == 0 {
		return fmt.Errorf("%s is not an executable file", resolved)
	}
	dir := filepath.Dir(resolved)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Type().IsRegular() {
			if err := trustedEntry(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	for p := dir; ; p = filepath.Dir(p) {
		if err := trustedEntry(p); err != nil {
			return err
		}
		if p == "/" {
			return nil
		}
	}
}

func trustedEntry(p string) error {
	st, err := os.Lstat(p)
	if err != nil {
		return err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: no ownership info", p)
	}
	if uid := int(sys.Uid); uid != 0 && uid != os.Geteuid() {
		return fmt.Errorf("%s is owned by uid %d", p, uid)
	}
	if st.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by group or others", p)
	}
	return nil
}
