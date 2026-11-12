package main

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
)

func keyPEM(t *testing.T, k any) []byte {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func keyFile(t *testing.T, dir, name string, b []byte, mode os.FileMode) string {
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, b, mode))
	require.NoError(t, os.Chmod(p, mode))
	return p
}

func TestSignDelegationWithKeyFiles(t *testing.T) {
	r := authoritytest.NewRealm(t)
	f := newFixture(t)
	a := keyFile(t, f.dir, "a.pem", keyPEM(t, r.PrimaryA), 0o600)
	b := keyFile(t, f.dir, "b.pem", keyPEM(t, r.PrimaryB), 0o400)
	spec := f.file("spec.json", specJSON(t, r.Current.D))
	anchors := f.file("anchors", r.AnchorBytes)

	out := filepath.Join(f.dir, "yes.signed")
	code, stdout, stderr := runTool("", "sign-delegation", "--anchors", anchors, "--first", "--spec", spec,
		"--out", out, "--key-file", a, "--key-file", b, "--yes")
	require.Equal(t, 0, code, stderr)
	require.Contains(t, stdout, "software keys: 2 key(s) from 2 file(s)")
	require.Empty(t, f.calls(), "no PIV tool runs")
	signed, err := os.ReadFile(out) //nolint:gosec // G304: a test temp file
	require.NoError(t, err)
	tr := r.Trust(t)
	require.NoError(t, tr.AddSigned(signed))

	// Without --yes the typed word and digests are still asked for.
	code, _, stderr = runTool("", "sign-delegation", "--anchors", anchors, "--first", "--spec", spec,
		"--out", filepath.Join(f.dir, "typed.signed"), "--key-file", a, "--key-file", b)
	require.Equal(t, 1, code)
	require.NotEmpty(t, stderr)
	code, _, stderr = runTool(firstTwice(r.Current.Msg), "sign-delegation", "--anchors", anchors, "--first", "--spec", spec,
		"--out", filepath.Join(f.dir, "typed.signed"), "--key-file", a, "--key-file", b)
	require.Equal(t, 0, code, stderr)
}

func TestSignSuccessionWithKeyFiles(t *testing.T) {
	r := authoritytest.NewRealm(t)
	f := newFixture(t)
	out := filepath.Join(f.dir, "s1.signed")
	code, _, stderr := runTool("", "sign-succession", "--anchors", f.file("anchors", r.AnchorBytes),
		"--trust", f.file("trust", trustFile(r.Current.Signed)), "--spec", f.file("s.json", successionJSON(t, r, 0, newKeys(t, 4))),
		"--out", out, "--key-file", keyFile(t, f.dir, "ra.pem", keyPEM(t, r.RecoveryA), 0o600),
		"--key-file", keyFile(t, f.dir, "rb.pem", keyPEM(t, r.RecoveryB), 0o600), "--yes")
	require.Equal(t, 0, code, stderr)
	signed, err := os.ReadFile(out) //nolint:gosec // G304: a test temp file
	require.NoError(t, err)
	tr := r.Trust(t)
	require.NoError(t, tr.AddSigned(signed))
	require.Equal(t, uint64(1), tr.CurrentGen())

	// Without --yes each digest is still typed.
	ra := keyFile(t, f.dir, "ra2.pem", keyPEM(t, r.RecoveryA), 0o600)
	rb := keyFile(t, f.dir, "rb2.pem", keyPEM(t, r.RecoveryB), 0o600)
	specB := successionJSON(t, r, 0, newKeys(t, 4))
	args := []string{"sign-succession", "--anchors", f.file("anchors2", r.AnchorBytes),
		"--trust", f.file("trust2", trustFile(r.Current.Signed)), "--spec", f.file("s2.json", specB),
		"--out", filepath.Join(f.dir, "s2.signed"), "--key-file", ra, "--key-file", rb}
	code, _, _ = runTool("", args...)
	require.Equal(t, 1, code)
	msg, _, err := readSuccessionSpec(filepath.Join(f.dir, "s2.json"))
	require.NoError(t, err)
	code, _, stderr = runTool(twice(msg), args...)
	require.Equal(t, 0, code, stderr)

	// --yes still needs key files.
	code, _, stderr = runTool("", "sign-succession", "--anchors", "a", "--trust", "t", "--spec", "s", "--dry-run", "--yes")
	require.Equal(t, 2, code)
	require.Contains(t, stderr, "usage:")
}

func TestDryRunChecksKeyFiles(t *testing.T) {
	r := authoritytest.NewRealm(t)
	f := newFixture(t)
	a := keyFile(t, f.dir, "a.pem", keyPEM(t, r.PrimaryA), 0o600)
	b := keyFile(t, f.dir, "b.pem", keyPEM(t, r.PrimaryB), 0o600)
	base := []string{"sign-delegation", "--anchors", f.file("anchors", r.AnchorBytes), "--first",
		"--spec", f.file("spec.json", specJSON(t, r.Current.D)), "--dry-run", "--key-file", a}
	code, stdout, stderr := runTool("", append(append([]string{}, base...), "--key-file", b)...)
	require.Equal(t, 0, code, stderr)
	require.Contains(t, stdout, "software keys: 2 key(s) from 2 file(s)")
	require.Contains(t, stdout, "nothing was signed")

	code, _, stderr = runTool("", base...)
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "primary B: no --key-file holds this key")

	// The same file twice is one key.
	code, _, stderr = runTool("", append(append([]string{}, base...), "--key-file", a)...)
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "primary B: no --key-file holds this key")
}

func TestKeyFilesRefuse(t *testing.T) {
	r := authoritytest.NewRealm(t)
	p256, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	_, ed, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	good := keyPEM(t, r.PrimaryA)
	cases := map[string]struct {
		files [][]byte
		mode  os.FileMode
		want  string
	}{
		"a key file group can read": {files: [][]byte{good, keyPEM(t, r.PrimaryB)}, mode: 0o640, want: "group or others can access"},
		"a P-256 key":               {files: [][]byte{keyPEM(t, p256)}, mode: 0o600, want: "not a P-384 key"},
		"two PEM blocks":            {files: [][]byte{append(append([]byte{}, good...), good...)}, mode: 0o600, want: "not one PKCS#8 PEM private key"},
		"not PEM":                   {files: [][]byte{[]byte("hello")}, mode: 0o600, want: "not one PKCS#8 PEM private key"},
		"primary B missing":         {files: [][]byte{good}, mode: 0o600, want: "no --key-file holds this key"},
		"text before the PEM":       {files: [][]byte{append([]byte("x\n"), good...)}, mode: 0o600, want: "not one PKCS#8 PEM private key"},
		"text after the PEM":        {files: [][]byte{append(append([]byte{}, good...), "x"...)}, mode: 0o600, want: "not one PKCS#8 PEM private key"},
		"a PEM header":              {files: [][]byte{pemOf("PRIVATE KEY", map[string]string{"Proc-Type": "4,ENCRYPTED"}, []byte{1})}, mode: 0o600, want: "not one PKCS#8 PEM private key"},
		"a SEC1 key":                {files: [][]byte{sec1PEM(t, r.PrimaryA)}, mode: 0o600, want: "a SEC1 EC key, not PKCS#8"},
		"bad DER":                   {files: [][]byte{pemOf("PRIVATE KEY", nil, []byte("junk"))}, mode: 0o600, want: "not one PKCS#8 PEM private key"},
		"an Ed25519 key":            {files: [][]byte{keyPEM(t, ed)}, mode: 0o600, want: "not a P-384 key"},
		"too large":                 {files: [][]byte{append(append([]byte{}, good...), strings.Repeat("\n", maxKeyFile)...)}, mode: 0o600, want: "too large for a key file"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			args := []string{"sign-delegation", "--anchors", f.file("anchors", r.AnchorBytes), "--first",
				"--spec", f.file("spec.json", specJSON(t, r.Current.D)), "--out", filepath.Join(f.dir, "out"), "--yes"}
			for i, b := range c.files {
				args = append(args, "--key-file", keyFile(t, f.dir, "k"+string(rune('0'+i)), b, c.mode))
			}
			code, _, stderr := runTool("", args...)
			require.Equal(t, 1, code)
			require.Contains(t, stderr, c.want)
			require.NoFileExists(t, filepath.Join(f.dir, "out"))
		})
	}
}

func pemOf(typ string, h map[string]string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Headers: h, Bytes: der})
}

func sec1PEM(t *testing.T, k *ecdsa.PrivateKey) []byte {
	der, err := x509.MarshalECPrivateKey(k)
	require.NoError(t, err)
	return pemOf("EC PRIVATE KEY", nil, der)
}

func TestKeyFileTrailingNewlineIsAccepted(t *testing.T) {
	r := authoritytest.NewRealm(t)
	dir := t.TempDir()
	p := keyFile(t, dir, "a.pem", append(keyPEM(t, r.PrimaryA), "\n\n"...), 0o600)
	k, err := loadKeys([]string{p})
	require.NoError(t, err)
	require.Len(t, k, 1)
}

func TestKeyFilePathRefusals(t *testing.T) {
	r := authoritytest.NewRealm(t)
	dir := t.TempDir()
	target := keyFile(t, dir, "a.pem", keyPEM(t, r.PrimaryA), 0o600)

	_, err := loadKeys([]string{filepath.Join(dir, "missing.pem")})
	require.ErrorIs(t, err, os.ErrNotExist)

	link := filepath.Join(dir, "link.pem")
	require.NoError(t, os.Symlink(target, link))
	_, err = loadKeys([]string{link})
	require.Error(t, err, "a symlink is not followed")

	fifo := filepath.Join(dir, "fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	_, err = loadKeys([]string{fifo})
	require.ErrorContains(t, err, "not a regular file")

	require.ErrorContains(t, func() error { _, err := loadKeys([]string{dir}); return err }(), "not a regular file")

	orig := geteuid
	t.Cleanup(func() { geteuid = orig })
	geteuid = func() int { return orig() + 1 }
	_, err = loadKeys([]string{target})
	require.ErrorContains(t, err, "not owned by this user")
}

func TestKeyFileUsage(t *testing.T) {
	base := []string{"sign-delegation", "--anchors", "a", "--first", "--spec", "s", "--dry-run"}
	for name, extra := range map[string][]string{
		"--yes without --key-file":  {"--yes"},
		"--key-file and --piv-tool": {"--key-file", "k", "--piv-tool", "p"},
		"--key-file and --slot":     {"--key-file", "k", "--slot", "9e"},
	} {
		t.Run(name, func(t *testing.T) {
			code, _, stderr := runTool("", append(append([]string{}, base...), extra...)...)
			require.Equal(t, 2, code)
			require.Contains(t, stderr, "usage:")
		})
	}
}
