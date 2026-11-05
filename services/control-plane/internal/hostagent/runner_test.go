package hostagent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// trustedDir makes a 0755 directory under the package dir; /tmp is
// world-writable and would fail the ancestor check.
func trustedDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckTrusted(filepath.Join(wd, "runner.go")); err != nil && !strings.Contains(err.Error(), "not an executable") {
		t.Skipf("checkout itself is not trusted here: %v", err)
	}
	dir, err := os.MkdirTemp(wd, "trusted-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // the trust check expects a normal 0755 directory
		t.Fatal(err)
	}
	return dir
}

func writeCLI(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "concord-selfhost")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil { //nolint:gosec // the fake CLI must be executable
		t.Fatal(err)
	}
	return p
}

func TestCheckTrusted(t *testing.T) {
	dir := trustedDir(t)
	cli := writeCLI(t, dir, "true")
	if err := CheckTrusted(cli); err != nil {
		t.Fatalf("trusted cli refused: %v", err)
	}
	if err := CheckTrusted("concord-selfhost"); err == nil {
		t.Fatal("relative path accepted")
	}

	neighbour := filepath.Join(dir, "selfhost-storage.sh")
	if err := os.WriteFile(neighbour, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(neighbour, 0o666); err != nil { //nolint:gosec // deliberately unsafe input
		t.Fatal(err)
	}
	if err := CheckTrusted(cli); err == nil {
		t.Fatal("world-writable neighbour accepted")
	}
	_ = os.Remove(neighbour)

	if err := os.Chmod(dir, 0o775); err != nil { //nolint:gosec // deliberately unsafe input
		t.Fatal(err)
	}
	if err := CheckTrusted(cli); err == nil {
		t.Fatal("group-writable directory accepted")
	}
	_ = os.Chmod(dir, 0o755) //nolint:gosec // restore the normal directory mode

	if err := os.Chmod(cli, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckTrusted(cli); err == nil {
		t.Fatal("non-executable cli accepted")
	}
}

func TestExecRunner(t *testing.T) {
	dir := trustedDir(t)
	cli := writeCLI(t, dir, `printf '%s|' "$@"; env; echo err >&2`)
	t.Setenv("CONCORD_SECRET", "leak")
	var out bytes.Buffer
	if err := (ExecRunner{CLI: cli}).Run(context.Background(), []string{"status", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.HasPrefix(got, "status|--json|") || strings.Contains(got, "err") || strings.Contains(got, "CONCORD_SECRET") {
		t.Fatalf("output %q", got)
	}

	slow := writeCLI(t, dir, "sleep 30 & wait")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := (ExecRunner{CLI: slow}).Run(ctx, nil, &out); err == nil {
		t.Fatal("cancelled run returned nil")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("process group not killed on cancel")
	}
}
