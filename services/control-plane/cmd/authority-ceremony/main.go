// Command authority-ceremony is the account-key authority's workstation tool
// (#3593, DoR §2.16 and §4): it signs a delegation with the two product-root
// primaries (LD-3, the 2-of-2 ceremony), a succession with the two recovery
// keys (T2), and an operator intent with an operator YubiKey (LD-4). The
// managed realm signs with YubiKeys on an operator workstation; a self-host
// install signs with software keys in files (--key-file, D-218). It also
// writes a realm's anchors file (make-anchors) and prints a trust file's
// current delegation for scripts (show-trust). It is never built into a
// server image (D-43).
package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

const usage = "usage: authority-ceremony sign-delegation --anchors <file> (--trust <file> | --first) --spec <file> " +
	"(--out <file> | --dry-run) [--slot <slot>] [--piv-tool <path> | --key-file <file>... [--yes]]\n" +
	"       authority-ceremony sign-succession --anchors <file> --trust <file> --spec <file> " +
	"(--out <file> | --dry-run) [--slot <slot>] [--piv-tool <path> | --key-file <file>... [--yes]]\n" +
	"       authority-ceremony add-trust --anchors <file> (--trust <file> | --first) --signed <file> --out <file>\n" +
	"       authority-ceremony operator-sign --intent <file> --key <hex point> [--slot <slot>] [--piv-tool <path>]\n" +
	"       authority-ceremony make-anchors --spec <file> --out <file>\n" +
	"       authority-ceremony show-trust --anchors <file> --trust <file>"

// errUsage is exit 2; every other error is exit 1.
var errUsage = errors.New("usage")

func main() {
	os.Exit(run(os.Args[1:], env{in: bufio.NewReader(os.Stdin), tty: os.Stdin, out: os.Stdout, errw: os.Stderr,
		now: time.Now}))
}

// env is the tool's I/O: in carries the confirmations, tty the PIV tool's PIN
// prompt (the same terminal in production).
type env struct {
	in   *bufio.Reader
	tty  io.Reader
	out  io.Writer
	errw io.Writer
	now  func() time.Time
}

// run dispatches a verb and returns the exit code: 0 done, 1 refused or
// failed, 2 usage.
func run(args []string, e env) int {
	verb, rest := "", []string(nil)
	if len(args) > 0 {
		verb, rest = args[0], args[1:]
	}
	var err error
	switch verb {
	case "sign-delegation":
		err = signDelegation(rest, e)
	case "sign-succession":
		err = signSuccession(rest, e)
	case "add-trust":
		err = addTrust(rest, e)
	case "operator-sign":
		err = operatorSign(rest, e)
	case "make-anchors":
		err = makeAnchors(rest, e)
	case "show-trust":
		err = showTrust(rest, e)
	default:
		err = errUsage
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage):
		_, _ = fmt.Fprintln(e.errw, usage) // stderr is the only place to report to
		return 2
	default:
		_, _ = fmt.Fprintln(e.errw, "refused:", err)
		return 1
	}
}

func newFlags(name string, e env) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(e.errw)
	return fs
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errUsage
	}
	return nil
}

// signDelegation is the 2-of-2 ceremony (LD-3, DoR §2.17 Renewal): it encodes
// the spec, refuses what the contract would not retain (PrecheckDelegation,
// VP-1 included), refuses an --out that exists, shows the trust's high-water
// mark, the PIV tool's resolved path, the digest and the decoded fields before
// each touch, re-reads the spec before the second touch and refuses bytes that
// differ, signs with primary A and then primary B, and admits the result into
// the trust before writing it. --first takes its own typed confirmation.
func signDelegation(args []string, e env) error {
	fs := newFlags("sign-delegation", e)
	anchorsPath := fs.String("anchors", "", "")
	trustPath := fs.String("trust", "", "")
	first := fs.Bool("first", false, "")
	specPath := fs.String("spec", "", "")
	outPath := fs.String("out", "", "")
	dryRun := fs.Bool("dry-run", false, "")
	sf := addSignerFlags(fs)
	if err := parse(fs, args); err != nil {
		return err
	}
	if *anchorsPath == "" || *specPath == "" || (*trustPath == "") == !*first || (*outPath == "") == !*dryRun || !sf.valid() {
		return errUsage
	}
	tr, err := loadTrust(*anchorsPath, *trustPath)
	if err != nil {
		return err
	}
	msg, d, err := readSpec(*specPath)
	if err != nil {
		return err
	}
	// --first stands in for a trust file only for the realm's first
	// delegation. Without the trust, the precheck cannot see a serial or a
	// cutover already issued.
	if *first && (d.ProductRootGen != 0 || d.Serial != 1 || d.MinValidSerial != 1) {
		return errors.New("--first signs only generation 0 serial 1; pass the realm's --trust for any other")
	}
	if err := tr.PrecheckDelegation(msg); err != nil {
		return err
	}
	// D-176: serve refuses a delegation whose window has not begun.
	if now := uint64(e.now().UnixMilli()); d.NotBeforeMs > now || d.NotAfterMs <= now {
		return fmt.Errorf("the window [%d, %d) does not cover now (%d)", d.NotBeforeMs, d.NotAfterMs, now)
	}
	if err := printHighWater(e.out, tr, *first); err != nil {
		return err
	}
	primaryA, primaryB := tr.CurrentPrimaries()
	keys := [2]named{{"primary A", primaryA}, {"primary B", primaryB}}
	if *dryRun {
		if err := showDelegation(e.out, msg, d); err != nil {
			return err
		}
		if err := sf.checkKeys(e, keys); err != nil {
			return err
		}
		_, err := fmt.Fprintln(e.out, "dry run: the precheck passed and nothing was signed")
		return err
	}
	if err := refuseExisting(*outPath); err != nil {
		return err
	}
	p, err := sf.signer(e, keys)
	if err != nil {
		return err
	}
	if *first && !*sf.yes {
		if err := confirmWord(e, "first", "This is the realm's first delegation and no trust file was checked. Type first to continue: "); err != nil {
			return err
		}
	}
	c := ceremony{e: e, p: p, yes: *sf.yes, tr: tr, msg: msg, kind: "delegation", out: *outPath,
		show:   func() error { return showDelegation(e.out, msg, d) },
		reread: func() ([]byte, error) { b, _, err := readSpec(*specPath); return b, err },
		keys:   keys}
	if err := c.run(); err != nil {
		return err
	}
	_, err = fmt.Fprintf(e.out, "wrote %s: generation %d serial %d, digest %x\n", *outPath, d.ProductRootGen, d.Serial, ac.Digest(msg))
	return err
}

// signSuccession is the recovery ceremony (T2, D-64): recovery A′ and B′ of
// the current generation sign the next generation's four keys. It refuses
// what T2 would ignore and a second succession from one generation, which
// halts the realm (PrecheckSuccession), and otherwise runs as signDelegation.
// The new generation's first delegation is a separate sign-delegation.
func signSuccession(args []string, e env) error {
	fs := newFlags("sign-succession", e)
	anchorsPath := fs.String("anchors", "", "")
	trustPath := fs.String("trust", "", "")
	specPath := fs.String("spec", "", "")
	outPath := fs.String("out", "", "")
	dryRun := fs.Bool("dry-run", false, "")
	sf := addSignerFlags(fs)
	if err := parse(fs, args); err != nil {
		return err
	}
	if *anchorsPath == "" || *trustPath == "" || *specPath == "" || (*outPath == "") == !*dryRun || !sf.valid() {
		return errUsage
	}
	tr, err := loadTrust(*anchorsPath, *trustPath)
	var halt *ac.HaltError
	if err != nil && !errors.As(err, &halt) {
		return err
	}
	msg, m, err := readSuccessionSpec(*specPath)
	if err != nil {
		return err
	}
	if err := tr.PrecheckSuccession(msg); err != nil {
		return err
	}
	if err := printHighWater(e.out, tr, false); err != nil {
		return err
	}
	show := func() error { return showSuccession(e.out, msg, m) }
	recA, recB := tr.CurrentRecoveries()
	keys := [2]named{{"recovery A′", recA}, {"recovery B′", recB}}
	if *dryRun {
		if err := show(); err != nil {
			return err
		}
		if err := sf.checkKeys(e, keys); err != nil {
			return err
		}
		_, err := fmt.Fprintln(e.out, "dry run: the precheck passed and nothing was signed")
		return err
	}
	if err := refuseExisting(*outPath); err != nil {
		return err
	}
	p, err := sf.signer(e, keys)
	if err != nil {
		return err
	}
	c := ceremony{e: e, p: p, yes: *sf.yes, tr: tr, msg: msg, kind: "succession", out: *outPath, show: show,
		reread: func() ([]byte, error) { b, _, err := readSuccessionSpec(*specPath); return b, err },
		keys:   keys}
	if err := c.run(); err != nil {
		return err
	}
	_, err = fmt.Fprintf(e.out, "wrote %s: generation %d to %d, digest %x\n", *outPath, m.PrevRootGen, m.NewRootGen, ac.Digest(msg))
	return err
}

type named struct {
	name  string
	point []byte
}

// ceremony is the two touches both signing verbs share: before each it shows
// the object and has the digest typed, it re-reads the spec between them and
// refuses bytes that changed, each signature is verified under its expected
// point, and the result is admitted into the trust before it is written.
type ceremony struct {
	e      env
	p      signer
	yes    bool // software keys only: no typed digest (signerFlags.valid)
	tr     *ac.Trust
	msg    []byte
	kind   string // the signed envelope's type
	out    string
	show   func() error
	reread func() ([]byte, error)
	keys   [2]named
}

func (c ceremony) run() error {
	var sigs [2][]byte
	for i, k := range c.keys {
		if i == 1 {
			again, err := c.reread()
			if err != nil {
				return err
			}
			if !bytes.Equal(again, c.msg) {
				return fmt.Errorf("the spec changed after %s signed; nothing was written", c.keys[0].name)
			}
		}
		if err := c.show(); err != nil {
			return err
		}
		if !c.yes {
			if err := confirm(c.e, c.msg, k.name); err != nil {
				return err
			}
		}
		sig, err := c.p.sign(c.msg, k.point)
		if err != nil {
			return fmt.Errorf("%s: %w", k.name, err)
		}
		sigs[i] = sig
	}
	signed := ac.Signed{Type: c.kind, Msg: c.msg, SigA: sigs[0], SigB: sigs[1]}.Encode()
	if signed == nil {
		return errors.New("the signed envelope does not encode")
	}
	if err := c.tr.AddSigned(signed); err != nil {
		return fmt.Errorf("the trust does not admit the signed %s: %w", c.kind, err)
	}
	return writeNew(c.out, signed)
}

// refuseExisting finds an --out clash before any touch; O_EXCL still guards
// the write.
func refuseExisting(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%s already exists; nothing was written", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// printHighWater prints what the precheck checked the spec against, so a stale
// trust file is visible before any touch.
func printHighWater(w io.Writer, tr *ac.Trust, first bool) error {
	if first {
		_, err := fmt.Fprintln(w, "trust: none (--first)")
		return err
	}
	hw := tr.HighWater()
	_, err := fmt.Fprintf(w, "trust: high water generation %d serial %d min_valid_serial %d\n", hw.Gen, hw.Serial, hw.MinValid)
	return err
}

// newPIVSigner resolves the PIV tool and prints its absolute path, so a
// binary found through PATH is the one the operator expects.
func newPIVSigner(tool, slot string, e env) (pivSigner, error) {
	path, err := exec.LookPath(tool)
	if err == nil {
		path, err = filepath.Abs(path)
	}
	if err != nil {
		return pivSigner{}, fmt.Errorf("the PIV tool: %w", err)
	}
	if _, err := fmt.Fprintf(e.out, "PIV tool: %s, slot %s\n", path, slot); err != nil {
		return pivSigner{}, err
	}
	return pivSigner{tool: path, slot: slot, stdin: e.tty, stderr: e.errw}, nil
}

// loadTrust is the realm's trust as clients build it: the anchors file and,
// unless this is the first delegation, the realm's trust file. A typed halt
// returns the populated trust alongside the error so recovery callers can
// apply their own checks. Every other admission error returns no trust.
func loadTrust(anchorsPath, trustPath string) (*ac.Trust, error) {
	b, err := os.ReadFile(anchorsPath) //nolint:gosec // G304: operator-supplied path
	if err != nil {
		return nil, err
	}
	a, err := ac.DecodeAnchors(b)
	if err != nil {
		return nil, fmt.Errorf("anchors: %w", err)
	}
	tr, err := ac.NewTrust(a)
	if err != nil {
		return nil, fmt.Errorf("anchors: %w", err)
	}
	if trustPath == "" {
		return tr, nil
	}
	envelope, err := os.ReadFile(trustPath) //nolint:gosec // G304: operator-supplied path
	if err != nil {
		return nil, err
	}
	if err := tr.AddTrust(envelope); err != nil {
		var halt *ac.HaltError
		if errors.As(err, &halt) {
			return tr, fmt.Errorf("trust: %w", err)
		}
		return nil, fmt.Errorf("trust: %w", err)
	}
	return tr, nil
}

// spec is the delegation as the custodians write it: the DoR's field names,
// points and pins in hex. Every field must be present and not null, so an
// omitted field never signs as zero.
type spec struct {
	RealmID            string   `json:"realm_id"`
	Serial             uint64   `json:"serial"`
	MinValidSerial     uint64   `json:"min_valid_serial"`
	ProductRootGen     uint64   `json:"product_root_gen"`
	RSKPublicKeys      []string `json:"rsk_public_keys"`
	CAKPublicKeys      []string `json:"cak_public_keys"`
	OperatorPublicKeys []string `json:"operator_public_keys"`
	AuthorityTLSKeys   []string `json:"authority_tls_keys"`
	CPTLSPins          []string `json:"cp_tls_pins"`
	NotBeforeMs        uint64   `json:"not_before_ms"`
	NotAfterMs         uint64   `json:"not_after_ms"`
	Scope              []string `json:"scope"`
	AcceptedVersions   []string `json:"accepted_versions"`
	TauMs              uint64   `json:"tau_ms"`
	DeltaMs            uint64   `json:"delta_ms"`
	GMs                uint64   `json:"g_ms"`
	CutoverAtMs        uint64   `json:"cutover_at_ms"`
}

var specFields = []string{"realm_id", "serial", "min_valid_serial", "product_root_gen", "rsk_public_keys",
	"cak_public_keys", "operator_public_keys", "authority_tls_keys", "cp_tls_pins", "not_before_ms", "not_after_ms",
	"scope", "accepted_versions", "tau_ms", "delta_ms", "g_ms", "cutover_at_ms"}

// readStrict decodes the JSON file at path into v: every field in fields
// must be present once, spelled exactly, and not null, and any other key is
// refused. encoding/json alone would match a key in any case and let a later
// duplicate win, so a spec could sign a value its reader never saw.
func readStrict(path string, fields []string, v any) error {
	b, err := os.ReadFile(path) //nolint:gosec // G304: operator-supplied path
	if err != nil {
		return err
	}
	present, err := topLevelKeys(b)
	if err != nil {
		return fmt.Errorf("spec: %w", err)
	}
	for k := range present {
		if !slices.Contains(fields, k) {
			return fmt.Errorf("spec: json: unknown field %q", k)
		}
	}
	for _, f := range fields {
		raw, ok := present[f]
		if !ok {
			return fmt.Errorf("spec: %s is missing", f)
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("spec: %s is null", f)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("spec: %w", err)
	}
	return nil
}

// topLevelKeys returns the object's keys and raw values, refusing a key
// given twice.
func topLevelKeys(b []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	out := map[string]json.RawMessage{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, _ := t.(string) // an object key is always a string
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("%s is given twice", k)
		}
		out[k] = raw
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after the object")
	}
	return out, nil
}

// readSpec returns the canonical delegation bytes the spec encodes.
func readSpec(path string) ([]byte, ac.Delegation, error) {
	var s spec
	if err := readStrict(path, specFields, &s); err != nil {
		return nil, ac.Delegation{}, err
	}
	var err error
	d := ac.Delegation{RealmID: s.RealmID, Serial: s.Serial, MinValidSerial: s.MinValidSerial,
		ProductRootGen: s.ProductRootGen, NotBeforeMs: s.NotBeforeMs, NotAfterMs: s.NotAfterMs,
		Scope: s.Scope, AcceptedVersions: s.AcceptedVersions, TauMs: s.TauMs, DeltaMs: s.DeltaMs, GMs: s.GMs,
		CutoverAtMs: s.CutoverAtMs}
	for _, f := range []struct {
		name string
		in   []string
		out  *[][]byte
	}{
		{"rsk_public_keys", s.RSKPublicKeys, &d.RSKPublicKeys},
		{"cak_public_keys", s.CAKPublicKeys, &d.CAKPublicKeys},
		{"operator_public_keys", s.OperatorPublicKeys, &d.OperatorPublicKeys},
		{"authority_tls_keys", s.AuthorityTLSKeys, &d.AuthorityTLSKeys},
		{"cp_tls_pins", s.CPTLSPins, &d.CPTLSPins},
	} {
		*f.out = make([][]byte, len(f.in))
		for i, h := range f.in {
			if (*f.out)[i], err = hex.DecodeString(h); err != nil {
				return nil, ac.Delegation{}, fmt.Errorf("spec: %s[%d]: %w", f.name, i, err)
			}
		}
	}
	msg := d.Encode()
	if msg == nil {
		return nil, ac.Delegation{}, errors.New("spec: not a valid delegation (a field is out of range or a point is not P-384)")
	}
	return msg, d, nil
}

// successionSpec is the succession as the custodians write it, points in hex.
type successionSpec struct {
	RealmID      string `json:"realm_id"`
	PrevRootGen  uint64 `json:"prev_root_gen"`
	NewRootGen   uint64 `json:"new_root_gen"`
	NewPrimaryA  string `json:"new_primary_a"`
	NewPrimaryB  string `json:"new_primary_b"`
	NewRecoveryA string `json:"new_recovery_a"`
	NewRecoveryB string `json:"new_recovery_b"`
}

var successionFields = []string{"realm_id", "prev_root_gen", "new_root_gen", "new_primary_a", "new_primary_b",
	"new_recovery_a", "new_recovery_b"}

// readSuccessionSpec returns the canonical succession bytes the spec encodes.
func readSuccessionSpec(path string) ([]byte, ac.Succession, error) {
	var s successionSpec
	if err := readStrict(path, successionFields, &s); err != nil {
		return nil, ac.Succession{}, err
	}
	m := ac.Succession{RealmID: s.RealmID, PrevRootGen: s.PrevRootGen, NewRootGen: s.NewRootGen}
	for _, f := range []struct {
		name, in string
		out      *[]byte
	}{
		{"new_primary_a", s.NewPrimaryA, &m.NewPrimaryA}, {"new_primary_b", s.NewPrimaryB, &m.NewPrimaryB},
		{"new_recovery_a", s.NewRecoveryA, &m.NewRecoveryA}, {"new_recovery_b", s.NewRecoveryB, &m.NewRecoveryB},
	} {
		var err error
		if *f.out, err = hex.DecodeString(f.in); err != nil {
			return nil, ac.Succession{}, fmt.Errorf("spec: %s: %w", f.name, err)
		}
	}
	msg := m.Encode()
	if msg == nil {
		return nil, ac.Succession{}, errors.New("spec: not a valid succession (a field is out of range or a point is not P-384)")
	}
	return msg, m, nil
}

func showSuccession(w io.Writer, msg []byte, m ac.Succession) error {
	p := &printer{w: w}
	p.f("\nsuccession: realm %s, generation %d to %d\n", m.RealmID, m.PrevRootGen, m.NewRootGen)
	p.f("  new primary A   %x\n  new primary B   %x\n", m.NewPrimaryA, m.NewPrimaryB)
	p.f("  new recovery A′ %x\n  new recovery B′ %x\n", m.NewRecoveryA, m.NewRecoveryB)
	p.f("digest (SHA-384 of the succession bytes):\n  %x\n", ac.Digest(msg))
	return p.err
}

func showDelegation(w io.Writer, msg []byte, d ac.Delegation) error {
	p := &printer{w: w}
	ts := func(ms uint64) string {
		return time.UnixMilli(int64(ms)).UTC().Format(time.RFC3339) //nolint:gosec // G115: the codec bounds every u by 2^53 − 1
	}
	p.f("\ndelegation: realm %s, generation %d, serial %d, min_valid_serial %d\n",
		d.RealmID, d.ProductRootGen, d.Serial, d.MinValidSerial)
	p.f("  window      %s to %s\n", ts(d.NotBeforeMs), ts(d.NotAfterMs))
	p.f("  tau %d ms, delta %d ms, g %d ms\n", d.TauMs, d.DeltaMs, d.GMs)
	if d.CutoverAtMs == 0 {
		p.f("  cutover     none\n")
	} else {
		p.f("  cutover     %s (%d)\n", ts(d.CutoverAtMs), d.CutoverAtMs)
	}
	p.f("  scope       %s\n  versions    %s\n", strings.Join(d.Scope, ", "), strings.Join(d.AcceptedVersions, ", "))
	for _, k := range []struct {
		name string
		keys [][]byte
	}{
		{"rsk", d.RSKPublicKeys}, {"cak", d.CAKPublicKeys}, {"operator", d.OperatorPublicKeys},
		{"authority tls", d.AuthorityTLSKeys}, {"cp tls pin", d.CPTLSPins},
	} {
		if len(k.keys) == 0 {
			p.f("  %-13s none\n", k.name)
		}
		for _, b := range k.keys {
			p.f("  %-13s %x\n", k.name, b)
		}
	}
	p.f("digest (SHA-384 of the delegation bytes):\n  %x\n", ac.Digest(msg))
	return p.err
}

// confirm makes the custodian type the digest's first 16 hex characters, so
// the touch follows a reading of the digest rather than a habit (LD-3: the
// digest display is the review step).
func confirm(e env, msg []byte, who string) error {
	want := hex.EncodeToString(ac.Digest(msg))[:16]
	if _, err := fmt.Fprintf(e.out, "Type the digest's first 16 characters to sign with %s, then touch the key: ", who); err != nil {
		return err
	}
	line, err := e.in.ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("no confirmation for %s: %w", who, err)
	}
	if strings.ToLower(strings.TrimSpace(line)) != want {
		return fmt.Errorf("the confirmation for %s does not match the digest; nothing was signed", who)
	}
	return nil
}

// confirmWord makes the operator type word after prompt.
func confirmWord(e env, word, prompt string) error {
	if _, err := fmt.Fprint(e.out, prompt); err != nil {
		return err
	}
	line, err := e.in.ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("no confirmation for %q: %w", word, err)
	}
	if strings.ToLower(strings.TrimSpace(line)) != word {
		return fmt.Errorf("%q was not typed; nothing was signed", word)
	}
	return nil
}

// operatorSign is the operator CLI verb (LD-4, D-231): it decodes the intent
// the console shows, displays it itself rather than trusting the console's
// rendering, and signs sig(operator, intent_digest) with the operator's PIV
// key. The signature, base64, is pasted back into the console.
func operatorSign(args []string, e env) error {
	fs := newFlags("operator-sign", e)
	intentPath := fs.String("intent", "", "")
	keyHex := fs.String("key", "", "")
	slot := fs.String("slot", "9c", "")
	tool := fs.String("piv-tool", "yubico-piv-tool", "")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *intentPath == "" || *keyHex == "" {
		return errUsage
	}
	key, err := hex.DecodeString(*keyHex)
	if err == nil {
		_, err = ac.ParsePoint(key)
	}
	if err != nil {
		return fmt.Errorf("--key is not a P-384 point: %w", err)
	}
	raw, err := os.ReadFile(*intentPath) //nolint:gosec // G304: operator-supplied path
	if err != nil {
		return err
	}
	intentBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return fmt.Errorf("intent: not base64: %w", err)
	}
	in, err := ac.DecodeIntent(intentBytes)
	if err != nil {
		return fmt.Errorf("intent: %w", err)
	}
	if now := uint64(e.now().UnixMilli()); in.ExpiresAtMs <= now {
		return fmt.Errorf("the intent expired at %d (now %d)", in.ExpiresAtMs, now)
	}
	digest := ac.Digest(intentBytes)
	p := &printer{w: e.out}
	p.f("\nintent: op %s, account %s\n  realm       %s\n  request     %s\n  expires     %s\n",
		in.Op, in.AccountID, in.RealmID, in.RequestID,
		time.UnixMilli(int64(in.ExpiresAtMs)).UTC().Format(time.RFC3339)) //nolint:gosec // G115: bounded by the codec
	p.f("  expected seq %d, recovery gen %d\n", in.ExpectedSeq, in.ExpectedRecoveryGen)
	for _, b := range []struct {
		name string
		v    []byte
	}{
		{"expected head digest", in.ExpectedHeadDigest}, {"predecessor root digest", in.PredecessorRootDigest},
		{"candidate root", in.CandidateRoot}, {"ek binding digest", in.EKBindingDigest},
		{"legacy ek digest", in.LegacyEKDigest}, {"device intent digest", in.DeviceIntentDigest},
		{"recovery bind digest", in.RecoveryBindDigest},
	} {
		if len(b.v) > 0 {
			p.f("  %-23s %x\n", b.name, b.v)
		}
	}
	p.f("intent digest:\n  %x\n", digest)
	if p.err != nil {
		return p.err
	}
	msg := ac.Sig{Role: ac.RoleOperator, Digest: digest}.Encode()
	p2, err := newPIVSigner(*tool, *slot, e)
	if err != nil {
		return err
	}
	if err := confirm(e, intentBytes, "the operator key"); err != nil {
		return err
	}
	sig, err := p2.sign(msg, key)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(e.out, "signature (paste into the console):\n%s\n", base64.StdEncoding.EncodeToString(sig))
	return err
}

// addTrust writes a new trust file: the old one's items plus one signed
// delegation or succession, in the §2.6 order. The item must be new to the
// trust and admitted without a halt, and the result must load as a trust on
// its own. The old file is never replaced.
func addTrust(args []string, e env) error {
	fs := newFlags("add-trust", e)
	anchors := fs.String("anchors", "", "anchors file")
	trustPath := fs.String("trust", "", "the realm's current trust file")
	first := fs.Bool("first", false, "the realm has no trust file yet")
	signedPath := fs.String("signed", "", "a signed delegation or succession")
	out := fs.String("out", "", "the new trust file")
	if err := parse(fs, args); err != nil || *anchors == "" || *signedPath == "" || *out == "" ||
		(*trustPath == "") == !*first {
		return errUsage
	}
	if err := refuseExisting(*out); err != nil {
		return err
	}
	tr, err := loadTrust(*anchors, *trustPath)
	var halt *ac.HaltError
	if err != nil && !errors.As(err, &halt) {
		return err
	}
	var items [][]byte
	if *trustPath != "" {
		b, err := os.ReadFile(*trustPath) //nolint:gosec // G304: operator-supplied path
		if err != nil {
			return err
		}
		te, err := ac.DecodeTrustEnvelope(b)
		if err != nil {
			return fmt.Errorf("trust: %w", err)
		}
		items = te.Items
	}
	item, err := os.ReadFile(*signedPath) //nolint:gosec // G304: operator-supplied path
	if err != nil {
		return err
	}
	if err := tr.AddSigned(item); err != nil {
		return fmt.Errorf("signed: %w; nothing was written", err)
	}
	if hw := tr.HighWater(); *first && hw != (ac.HighWater{Gen: 0, Serial: 1, MinValid: 1}) {
		return errors.New("--first takes only the realm's first delegation, generation 0 serial 1; nothing was written")
	}
	// ponytail: try each position; the order is strict, so at most one is valid.
	for i := 0; i <= len(items); i++ {
		next := ac.TrustEnvelope{Items: append(append(append([][]byte{}, items[:i]...), item), items[i:]...)}.Encode()
		if _, err := ac.CheckTrustEnvelope(next); err != nil {
			continue
		}
		if err := loadTrustBytes(*anchors, next); err != nil {
			return fmt.Errorf("the new trust does not load: %w; nothing was written", err)
		}
		if err := writeNew(*out, next); err != nil {
			return err
		}
		hw := tr.HighWater()
		p := &printer{w: e.out}
		p.f("wrote %s: %d items, high water generation %d serial %d min_valid_serial %d\n",
			*out, len(items)+1, hw.Gen, hw.Serial, hw.MinValid)
		return p.err
	}
	return errors.New("the signed item has no place in the trust order (a duplicate?); nothing was written")
}

// anchorsSpec is the realm id and the four product-root points, in hex.
type anchorsSpec struct {
	RealmID   string `json:"realm_id"`
	PrimaryA  string `json:"primary_a"`
	PrimaryB  string `json:"primary_b"`
	RecoveryA string `json:"recovery_a"`
	RecoveryB string `json:"recovery_b"`
}

var anchorsFields = []string{"realm_id", "primary_a", "primary_b", "recovery_a", "recovery_b"}

// makeAnchors writes the realm's anchors file (T1): four pairwise-distinct
// P-384 points. It prints the file's SHA-384, which the operator records
// out of band: the anchors are trusted by provenance, not by a signature.
func makeAnchors(args []string, e env) error {
	fs := newFlags("make-anchors", e)
	specPath := fs.String("spec", "", "")
	out := fs.String("out", "", "")
	if err := parse(fs, args); err != nil || *specPath == "" || *out == "" {
		return errUsage
	}
	if err := refuseExisting(*out); err != nil {
		return err
	}
	var s anchorsSpec
	if err := readStrict(*specPath, anchorsFields, &s); err != nil {
		return err
	}
	a := ac.Anchors{RealmID: s.RealmID}
	for _, f := range []struct {
		name, in string
		out      *[]byte
	}{
		{"primary_a", s.PrimaryA, &a.PrimaryA}, {"primary_b", s.PrimaryB, &a.PrimaryB},
		{"recovery_a", s.RecoveryA, &a.RecoveryA}, {"recovery_b", s.RecoveryB, &a.RecoveryB},
	} {
		var err error
		if *f.out, err = hex.DecodeString(f.in); err != nil {
			return fmt.Errorf("spec: %s: %w", f.name, err)
		}
	}
	if _, err := ac.NewTrust(a); err != nil {
		return fmt.Errorf("spec: %w (the realm id must be a lower-case canonical UUID and the four points distinct P-384 points)", err)
	}
	b := a.Encode()
	if err := writeNew(*out, b); err != nil {
		return err
	}
	p := &printer{w: e.out}
	p.f("wrote %s\nrealm_id=%s\nprimary_a=%s\nprimary_b=%s\nrecovery_a=%s\nrecovery_b=%s\nsha384=%s\n", *out,
		a.RealmID, s.PrimaryA, s.PrimaryB, s.RecoveryA, s.RecoveryB, hex.EncodeToString(ac.Digest(b)))
	return p.err
}

// showTrust prints a trust file's state as key=value lines for scripts:
// the high-water mark and the current delegation's window. It does not read
// the clock: a caller compares not_before_ms and not_after_ms with the time
// itself. A trust that does not load, or is halted, is refused with the halt.
func showTrust(args []string, e env) error {
	fs := newFlags("show-trust", e)
	anchors := fs.String("anchors", "", "")
	trustPath := fs.String("trust", "", "")
	if err := parse(fs, args); err != nil || *anchors == "" || *trustPath == "" {
		return errUsage
	}
	tr, err := loadTrust(*anchors, *trustPath)
	if err != nil {
		return err
	}
	d, ok := tr.Current()
	if !ok {
		return fmt.Errorf("trust: %w", tr.Halted())
	}
	hw := tr.HighWater()
	p := &printer{w: e.out}
	p.f("realm_id=%s\ngeneration=%d\nserial=%d\nmin_valid_serial=%d\nnot_before_ms=%d\nnot_after_ms=%d\n",
		d.RealmID, hw.Gen, hw.Serial, hw.MinValid, d.NotBeforeMs, d.NotAfterMs)
	return p.err
}

// loadTrustBytes checks that a trust envelope loads under the anchors with no halt.
func loadTrustBytes(anchorsPath string, envelope []byte) error {
	tr, err := loadTrust(anchorsPath, "")
	if err != nil {
		return err
	}
	return tr.AddTrust(envelope)
}

// writeNew writes b to a new file: it never replaces an earlier ceremony's
// output.
func writeNew(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644) //nolint:gosec // operator-supplied path; the signed delegation is public
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}

// printer writes to w until the first error and keeps it, so a display of
// many lines checks one error.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) f(format string, a ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format, a...)
	}
}
