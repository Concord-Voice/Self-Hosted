package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha512"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"syscall"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// signer signs msg with the key whose point is want, and returns a P1363
// signature that verifies under want.
type signer interface {
	sign(msg, want []byte) ([]byte, error)
}

// signerFlags picks the signer: a YubiKey through the PIV tool (the managed
// realm, LD-3), or software keys in files (self-host, D-218), never both.
// --yes skips the typed word and digests and is allowed only with --key-file: a
// self-host installer signs unattended, and its keys are already on the
// host, so typing adds no second reader.
type signerFlags struct {
	fs         *flag.FlagSet
	slot, tool *string
	keys       []string
	yes        *bool
}

func addSignerFlags(fs *flag.FlagSet) *signerFlags {
	s := &signerFlags{fs: fs,
		slot: fs.String("slot", "9c", ""),
		tool: fs.String("piv-tool", "yubico-piv-tool", ""),
		yes:  fs.Bool("yes", false, "")}
	fs.Func("key-file", "", func(v string) error { s.keys = append(s.keys, v); return nil })
	return s
}

// valid reports whether the parsed flags name one signer.
func (s *signerFlags) valid() bool {
	piv := false
	s.fs.Visit(func(f *flag.Flag) { piv = piv || f.Name == "slot" || f.Name == "piv-tool" })
	if len(s.keys) == 0 {
		return !*s.yes
	}
	return !piv
}

// signer returns the PIV signer, or loads the key files and refuses a set
// that lacks one of want, before anything is signed.
func (s *signerFlags) signer(e env, want [2]named) (signer, error) {
	if len(s.keys) == 0 {
		return newPIVSigner(*s.tool, *s.slot, e)
	}
	k, err := loadKeys(s.keys)
	if err != nil {
		return nil, err
	}
	for _, w := range want {
		if _, ok := k[hex.EncodeToString(w.point)]; !ok {
			return nil, fmt.Errorf("%s: no --key-file holds this key", w.name)
		}
	}
	if _, err := fmt.Fprintf(e.out, "software keys: %d key(s) from %d file(s)\n", len(k), len(s.keys)); err != nil {
		return nil, err
	}
	return k, nil
}

// checkKeys is the dry run's part of signer: with key files, it proves they
// hold want. The PIV path has nothing to check without a touch.
func (s *signerFlags) checkKeys(e env, want [2]named) error {
	if len(s.keys) == 0 {
		return nil
	}
	_, err := s.signer(e, want)
	return err
}

// keySigner holds software P-384 keys by their uncompressed point (hex).
type keySigner map[string]*ecdsa.PrivateKey

// maxKeyFile bounds a key file read: a P-384 PKCS#8 PEM is about 300 bytes.
const maxKeyFile = 64 << 10

// geteuid is os.Geteuid, replaced in tests.
var geteuid = os.Geteuid

// loadKeys reads PKCS#8 PEM P-384 keys. Each path is opened once, without
// following a symlink, and must be a regular file that the caller owns and
// that its group and others cannot access. The file must hold one PEM block
// and nothing else but trailing whitespace.
func loadKeys(paths []string) (keySigner, error) {
	k := keySigner{}
	for _, p := range paths {
		b, err := readKeyFile(p)
		if err != nil {
			return nil, err
		}
		key, err := parseKey(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		pt, err := key.PublicKey.Bytes()
		if err != nil {
			return nil, err
		}
		k[hex.EncodeToString(pt)] = key
	}
	return k, nil
}

func readKeyFile(p string) ([]byte, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: an operator flag naming the operator's own key file
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", p)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != geteuid() {
		return nil, fmt.Errorf("%s: the key file is not owned by this user", p)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s: group or others can access the key file", p)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxKeyFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxKeyFile {
		return nil, fmt.Errorf("%s: too large for a key file", p)
	}
	return b, nil
}

func parseKey(b []byte) (*ecdsa.PrivateKey, error) {
	blk, rest := pem.Decode(b)
	if blk != nil && blk.Type == "EC PRIVATE KEY" {
		return nil, errors.New("a SEC1 EC key, not PKCS#8; convert it with openssl pkcs8 -topk8 -nocrypt")
	}
	if blk == nil || blk.Type != "PRIVATE KEY" || len(blk.Headers) != 0 ||
		!bytes.HasPrefix(b, []byte("-----BEGIN ")) || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("not one PKCS#8 PEM private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, errors.New("not one PKCS#8 PEM private key")
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P384() {
		return nil, errors.New("not a P-384 key")
	}
	return key, nil
}

func (k keySigner) sign(msg, want []byte) ([]byte, error) {
	key, ok := k[hex.EncodeToString(want)]
	if !ok {
		return nil, errors.New("no --key-file holds this key")
	}
	h := sha512.Sum384(msg)
	r, s, err := ecdsa.Sign(rand.Reader, key, h[:])
	if err != nil {
		return nil, err
	}
	sig := make([]byte, 96)
	r.FillBytes(sig[:48])
	s.FillBytes(sig[48:])
	if !ac.VerifyP384(want, msg, sig) {
		return nil, errors.New("the signature does not verify under the expected key")
	}
	return sig, nil
}
