package main

import (
	"bytes"
	"crypto/elliptic"
	"encoding/asn1"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// errStrictDER is any ECDSA signature that is not exactly the minimal DER
// SEQUENCE { INTEGER r, INTEGER s } with 1 ≤ r, s < n.
var errStrictDER = errors.New("the PIV tool's signature is not strict DER")

// derToP1363 converts a strict-DER ECDSA P-384 signature to the contract's
// P1363 form, r‖s with each half FillBytes(48) (DoR §2.4). asn1.Unmarshal
// alone accepts BER-ish inputs it can still read, so the parse must also
// re-encode to the same bytes: that refuses a non-minimal length or integer,
// a negative integer and anything trailing.
func derToP1363(der []byte) ([]byte, error) {
	var sig struct{ R, S *big.Int }
	rest, err := asn1.Unmarshal(der, &sig)
	if err != nil || len(rest) != 0 {
		return nil, errStrictDER
	}
	if canon, err := asn1.Marshal(sig); err != nil || !bytes.Equal(canon, der) {
		return nil, errStrictDER
	}
	n := elliptic.P384().Params().N
	if sig.R.Sign() <= 0 || sig.S.Sign() <= 0 || sig.R.Cmp(n) >= 0 || sig.S.Cmp(n) >= 0 {
		return nil, errStrictDER
	}
	out := make([]byte, 96)
	sig.R.FillBytes(out[:48])
	sig.S.FillBytes(out[48:])
	return out, nil
}

// pivSigner is the PIV seam (DoR §4 #3593): it shells out to yubico-piv-tool
// on the operator's workstation, so the tool carries no PIV or PC/SC module.
// yubico-piv-tool hashes its input itself (-H SHA384), so the seam hands it the
// message, not a digest: a crypto.Signer would receive a digest the tool would
// hash a second time.
//
// The tool reads the PIN from the terminal and the YubiKey waits for a touch
// (PIN and touch policy ALWAYS, D-231). Every signature is verified against
// the point the caller expects before it is used, so the wrong key in the
// reader, the wrong slot or a tool that misbehaves is refused, never written.
type pivSigner struct {
	tool   string    // yubico-piv-tool, or a test stand-in
	slot   string    // PIV slot, 9c by default
	stdin  io.Reader // the terminal, for the PIN prompt
	stderr io.Writer
}

func (p pivSigner) sign(msg, wantPoint []byte) ([]byte, error) {
	dir, err := os.MkdirTemp("", "authority-ceremony-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }() // a leftover holds only the public message and signature
	in, out := filepath.Join(dir, "msg"), filepath.Join(dir, "sig")
	if err := os.WriteFile(in, msg, 0o600); err != nil {
		return nil, err
	}
	//nolint:gosec // G204: the tool path is an operator flag on the operator's own workstation, and no argument comes from the message
	cmd := exec.Command(p.tool, "-a", "verify-pin", "--sign", "-s", p.slot, "-H", "SHA384", "-A", "ECCP384",
		"-i", in, "-o", out)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = p.stdin, p.stderr, p.stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(p.tool), err)
	}
	der, err := os.ReadFile(out) //nolint:gosec // G304: a file in the directory made above
	if err != nil {
		return nil, err
	}
	sig, err := derToP1363(der)
	if err != nil {
		return nil, err
	}
	if !ac.VerifyP384(wantPoint, msg, sig) {
		return nil, errors.New("the signature does not verify under the expected key: wrong YubiKey or slot?")
	}
	return sig, nil
}
