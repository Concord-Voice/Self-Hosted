package main

import (
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
	"os"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// signer signs msg with the key whose point is want, and returns a P1363
// signature that verifies under want.
type signer interface {
	sign(msg, want []byte) ([]byte, error)
}

// signerFlags picks the signer: a YubiKey through the PIV tool (the managed
// realm, LD-3), or software keys in files (self-host, D-218), never both.
// --yes skips the typed digest and is allowed only with --key-file: a
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

func (s *signerFlags) signer(e env) (signer, error) {
	if len(s.keys) == 0 {
		return newPIVSigner(*s.tool, *s.slot, e)
	}
	k, err := loadKeys(s.keys)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(e.out, "software keys: %d file(s)\n", len(s.keys)); err != nil {
		return nil, err
	}
	return k, nil
}

// keySigner holds software P-384 keys by their uncompressed point (hex).
type keySigner map[string]*ecdsa.PrivateKey

// loadKeys reads PKCS#8 PEM P-384 keys. A file that its group or others can
// read, or that holds anything else, is refused.
func loadKeys(paths []string) (keySigner, error) {
	k := keySigner{}
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("%s: group or others can access the key file", p)
		}
		b, err := os.ReadFile(p) //nolint:gosec // G304: an operator flag naming the operator's own key file
		if err != nil {
			return nil, err
		}
		blk, rest := pem.Decode(b)
		if blk == nil || blk.Type != "PRIVATE KEY" || len(rest) != 0 {
			return nil, fmt.Errorf("%s: not one PKCS#8 PEM private key", p)
		}
		parsed, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: not one PKCS#8 PEM private key", p)
		}
		key, ok := parsed.(*ecdsa.PrivateKey)
		if !ok || key.Curve != elliptic.P384() {
			return nil, fmt.Errorf("%s: not a P-384 key", p)
		}
		pt, err := key.PublicKey.Bytes()
		if err != nil {
			return nil, err
		}
		k[hex.EncodeToString(pt)] = key
	}
	return k, nil
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
