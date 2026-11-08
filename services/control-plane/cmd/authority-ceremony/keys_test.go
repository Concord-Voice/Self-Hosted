package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
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
	require.Contains(t, stdout, "software keys: 2 file(s)")
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
}

func TestKeyFilesRefuse(t *testing.T) {
	r := authoritytest.NewRealm(t)
	p256, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
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
