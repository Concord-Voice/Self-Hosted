package main

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// The test binary doubles as the PIV tool: with FAKE_PIV_DIR set it signs its
// -i file as yubico-piv-tool would, with the key named by its call count
// (key0, key1, ...), and writes a DER signature to its -o file.
func TestMain(m *testing.M) {
	if dir := os.Getenv("FAKE_PIV_DIR"); dir != "" {
		os.Exit(fakePIV(dir, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakePIV(dir string, args []string) int {
	logf, err := os.OpenFile(filepath.Join(dir, "calls"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // G304: the fake tool's own temp directory
	if err != nil {
		return 9
	}
	_, _ = fmt.Fprintln(logf, strings.Join(args, " "))
	_ = logf.Close()
	calls, _ := os.ReadFile(filepath.Join(dir, "calls")) //nolint:gosec // G304: the fake tool's own temp directory
	n := bytes.Count(calls, []byte("\n")) - 1
	mode := os.Getenv("FAKE_PIV_MODE")
	if mode == "fail" {
		return 3
	}
	if path := os.Getenv("FAKE_PIV_EDIT"); path != "" { // replace the spec with FAKE_PIV_EDIT_FROM's bytes
		b, _ := os.ReadFile(os.Getenv("FAKE_PIV_EDIT_FROM")) //nolint:gosec // G304,G703: a test temp file named by the test
		_ = os.WriteFile(path, b, 0o600)                     //nolint:gosec // G703: a test temp file named by the test
	}
	var in, out string
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "-i":
			in = args[i+1]
		case "-o":
			out = args[i+1]
		}
	}
	der, err := os.ReadFile(filepath.Join(dir, "key"+strconv.Itoa(n))) //nolint:gosec // G304: the fake tool's own temp directory
	if err != nil {
		return 4
	}
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return 5
	}
	msg, err := os.ReadFile(in) //nolint:gosec // G304: the -i file the tool under test wrote
	if err != nil {
		return 6
	}
	h := sha512.Sum384(msg)
	sig, err := ecdsa.SignASN1(rand.Reader, k.(*ecdsa.PrivateKey), h[:])
	if err != nil {
		return 7
	}
	if mode == "ber" { // a long-form length where the short form fits
		sig = append([]byte{0x30, 0x81, sig[1]}, sig[2:]...)
	}
	if os.WriteFile(out, sig, 0o600) != nil { //nolint:gosec // G703: the -o file the tool under test named
		return 8
	}
	return 0
}

type fixture struct {
	t    *testing.T
	dir  string
	piv  string
	keys []*ecdsa.PrivateKey
}

// newFixture arms the fake PIV tool to sign with keys, in call order.
func newFixture(t *testing.T, keys ...*ecdsa.PrivateKey) *fixture {
	f := &fixture{t: t, dir: t.TempDir(), keys: keys}
	f.piv = filepath.Join(f.dir, "piv")
	require.NoError(t, os.Mkdir(f.piv, 0o700))
	for i, k := range keys {
		der, err := x509.MarshalPKCS8PrivateKey(k)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(f.piv, "key"+strconv.Itoa(i)), der, 0o600))
	}
	t.Setenv("FAKE_PIV_DIR", f.piv)
	return f
}

func (f *fixture) file(name string, b []byte) string {
	p := filepath.Join(f.dir, name)
	require.NoError(f.t, os.WriteFile(p, b, 0o600))
	return p
}

func (f *fixture) calls() []string {
	b, err := os.ReadFile(filepath.Join(f.piv, "calls"))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(f.t, err)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func (f *fixture) run(stdin string, args ...string) (int, string, string) {
	var out, errw bytes.Buffer
	args = append(args, "--piv-tool", os.Args[0])
	code := run(args, env{in: bufio.NewReader(strings.NewReader(stdin)), tty: strings.NewReader(""),
		out: &out, errw: &errw, now: time.Now})
	return code, out.String(), errw.String()
}

func specJSON(t *testing.T, d ac.Delegation) []byte {
	hexes := func(bs [][]byte) []string {
		out := make([]string, len(bs))
		for i, b := range bs {
			out[i] = hex.EncodeToString(b)
		}
		return out
	}
	b, err := json.Marshal(spec{RealmID: d.RealmID, Serial: d.Serial, MinValidSerial: d.MinValidSerial,
		ProductRootGen: d.ProductRootGen, RSKPublicKeys: hexes(d.RSKPublicKeys), CAKPublicKeys: hexes(d.CAKPublicKeys),
		OperatorPublicKeys: hexes(d.OperatorPublicKeys), AuthorityTLSKeys: hexes(d.AuthorityTLSKeys),
		CPTLSPins: hexes(d.CPTLSPins), NotBeforeMs: d.NotBeforeMs, NotAfterMs: d.NotAfterMs, Scope: d.Scope,
		AcceptedVersions: d.AcceptedVersions, TauMs: d.TauMs, DeltaMs: d.DeltaMs, GMs: d.GMs,
		CutoverAtMs: d.CutoverAtMs})
	require.NoError(t, err)
	return b
}

func prefix(msg []byte) string { return hex.EncodeToString(ac.Digest(msg))[:16] }

func twice(msg []byte) string { return prefix(msg) + "\n" + prefix(msg) + "\n" }

// firstTwice is the stdin of a --first ceremony: the word, then two digests.
func firstTwice(msg []byte) string { return "first\n" + twice(msg) }

func trustFile(items ...[]byte) []byte { return ac.TrustEnvelope{Items: items}.Encode() }

func TestSignDelegationFirst(t *testing.T) {
	r := authoritytest.NewRealm(t)
	f := newFixture(t, r.PrimaryA, r.PrimaryB)
	out := filepath.Join(f.dir, "d1.signed")
	code, stdout, stderr := f.run(firstTwice(r.Current.Msg), "sign-delegation", "--anchors", f.file("anchors", r.AnchorBytes),
		"--first", "--spec", f.file("spec.json", specJSON(t, r.Current.D)), "--out", out)
	require.Equal(t, 0, code, stderr)
	require.Contains(t, stdout, "trust: none (--first)")
	require.Contains(t, stdout, "PIV tool: "+os.Args[0])
	require.Equal(t, 2, strings.Count(stdout, "digest (SHA-384 of the delegation bytes):\n  "+hex.EncodeToString(ac.Digest(r.Current.Msg))), "the digest is shown before each touch")

	calls := f.calls()
	require.Len(t, calls, 2)
	for _, c := range calls {
		require.True(t, strings.HasPrefix(c, "-a verify-pin --sign -s 9c -H SHA384 -A ECCP384 -i "), c)
	}
	signed, err := os.ReadFile(out) //nolint:gosec // G304: a test temp file
	require.NoError(t, err)
	tr := r.Trust(t)
	require.NoError(t, tr.AddSigned(signed))
	require.Nil(t, tr.Halted())
}

func TestSignDelegationNextSerialAgainstTheTrust(t *testing.T) {
	r := authoritytest.NewRealm(t)
	first := r.Current
	next := r.NextDelegation(t)
	f := newFixture(t, r.PrimaryA, r.PrimaryB)
	code, stdout, stderr := f.run(twice(next.Msg), "sign-delegation", "--anchors", f.file("anchors", r.AnchorBytes),
		"--trust", f.file("trust", trustFile(first.Signed)), "--spec", f.file("spec.json", specJSON(t, next.D)),
		"--out", filepath.Join(f.dir, "d2"), "--slot", "9e")
	require.Equal(t, 0, code, stderr)
	require.Contains(t, stdout, "trust: high water generation 0 serial 1 min_valid_serial 1")
	require.Contains(t, f.calls()[0], "-s 9e")
}

func TestSignDelegationRefusesBeforeAnyTouch(t *testing.T) {
	r := authoritytest.NewRealm(t, func(d *ac.Delegation) { d.CutoverAtMs = r0(t) })
	first := r.Current
	backward := r.NextDelegation(t, func(d *ac.Delegation) { d.CutoverAtMs-- })
	stale := r.NextDelegation(t, func(d *ac.Delegation) {
		d.CutoverAtMs = first.D.CutoverAtMs
		d.NotAfterMs = d.NotBeforeMs + 1
	})
	future := r.NextDelegation(t, func(d *ac.Delegation) {
		d.CutoverAtMs = first.D.CutoverAtMs
		d.NotBeforeMs = uint64(time.Now().Add(time.Hour).UnixMilli())
		d.NotAfterMs = d.NotBeforeMs + 86_400_000
	})
	r.Current = first
	above := r.NextDelegation(t, func(d *ac.Delegation) { d.Serial, d.MinValidSerial = 5, 5 })
	below := r.NextDelegation(t, func(d *ac.Delegation) { d.Serial, d.MinValidSerial = 3, 3 })
	lowerMV := r.NextDelegation(t, func(d *ac.Delegation) { d.Serial, d.MinValidSerial = 6, 1 })
	gen1 := first.D
	gen1.ProductRootGen = 1
	cases := map[string]struct {
		d     ac.Delegation
		first bool
		trust [][]byte
		out   bool // --out already exists
		want  string
	}{
		"a serial below one issued":             {d: below.D, trust: [][]byte{first.Signed, above.Signed}, want: "serial 3 is below serial 5"},
		"a lower min_valid_serial":              {d: lowerMV.D, trust: [][]byte{first.Signed, above.Signed}, want: "min_valid_serial 1 is below 5"},
		"--first for a generation other than 0": {d: gen1, first: true, want: "--first signs only"},
		"an --out that exists":                  {d: serial2(first), out: true, want: "already exists"},
		"VP-1: a backward cutover":              {d: backward.D, want: "(VP-1)"},
		"a serial already issued":               {d: first.D, want: "already issued"},
		"a window that has ended":               {d: stale.D, want: "does not cover now"},
		"a window not yet begun (D-176)":        {d: future.D, want: "does not cover now"},
		"--first for a serial other than 1":     {d: backward.D, first: true, want: "--first signs only"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, r.PrimaryA, r.PrimaryB)
			out := filepath.Join(f.dir, "out")
			if c.out {
				f.file("out", []byte("an earlier ceremony"))
			}
			args := []string{"sign-delegation", "--anchors", f.file("anchors", r.AnchorBytes),
				"--spec", f.file("spec.json", specJSON(t, c.d)), "--out", out}
			if c.first {
				args = append(args, "--first")
			} else {
				if c.trust == nil {
					c.trust = [][]byte{first.Signed}
				}
				args = append(args, "--trust", f.file("trust", trustFile(c.trust...)))
			}
			code, _, stderr := f.run("first\n"+twice(c.d.Encode()), args...)
			require.Equal(t, 1, code)
			require.Contains(t, stderr, c.want)
			require.Empty(t, f.calls(), "nothing is signed")
			if c.out {
				b, err := os.ReadFile(out) //nolint:gosec // G304: a test temp file
				require.NoError(t, err)
				require.Equal(t, "an earlier ceremony", string(b), "an earlier output is never replaced")
			} else {
				require.NoFileExists(t, out)
			}
		})
	}
}

// serial2 is a valid serial-2 delegation after first, at first's cutover.
func serial2(first authoritytest.SignedDelegation) ac.Delegation {
	d := first.D
	d.Serial, d.MinValidSerial = 2, 2
	return d
}

// r0 is a cutover time in the past, so the fixture's window covers now.
func r0(t *testing.T) uint64 {
	t.Helper()
	return uint64(time.Now().Add(-time.Minute).UnixMilli())
}

func TestSignDelegationRefusesAtATouch(t *testing.T) {
	r := authoritytest.NewRealm(t)
	other, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	edited := r.Current.D
	edited.TauMs++
	cases := map[string]struct {
		keys    []*ecdsa.PrivateKey
		stdin   string
		noStdin bool
		mode    string
		edit    bool
		want    string
		calls   int
	}{
		"--first not typed":               {keys: []*ecdsa.PrivateKey{r.PrimaryA, r.PrimaryB}, stdin: "yes\n" + twice(r.Current.Msg), want: `"first" was not typed`},
		"a wrong confirmation":            {keys: []*ecdsa.PrivateKey{r.PrimaryA, r.PrimaryB}, stdin: "first\n0000000000000000\n", want: "does not match the digest"},
		"no confirmation":                 {keys: []*ecdsa.PrivateKey{r.PrimaryA, r.PrimaryB}, noStdin: true, want: "no confirmation"},
		"primary B in the reader for A":   {keys: []*ecdsa.PrivateKey{r.PrimaryB, r.PrimaryB}, want: "primary A: the signature does not verify", calls: 1},
		"another key in the reader for B": {keys: []*ecdsa.PrivateKey{r.PrimaryA, other}, want: "primary B: the signature does not verify", calls: 2},
		"a BER signature":                 {keys: []*ecdsa.PrivateKey{r.PrimaryA, r.PrimaryB}, mode: "ber", want: "not strict DER", calls: 1},
		"the PIV tool fails":              {keys: []*ecdsa.PrivateKey{r.PrimaryA, r.PrimaryB}, mode: "fail", want: "exit status 3", calls: 1},
		"the spec changes between touches": {keys: []*ecdsa.PrivateKey{r.PrimaryA, r.PrimaryB}, edit: true,
			want: "the spec changed after primary A signed", calls: 1},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, c.keys...)
			t.Setenv("FAKE_PIV_MODE", c.mode)
			specPath := f.file("spec.json", specJSON(t, r.Current.D))
			if c.edit {
				t.Setenv("FAKE_PIV_EDIT", specPath)
				t.Setenv("FAKE_PIV_EDIT_FROM", f.file("edited.json", specJSON(t, edited)))
			}
			stdin := c.stdin
			if stdin == "" && !c.noStdin {
				stdin = firstTwice(r.Current.Msg)
			}
			out := filepath.Join(f.dir, "out")
			code, _, stderr := f.run(stdin, "sign-delegation", "--anchors", f.file("anchors", r.AnchorBytes), "--first",
				"--spec", specPath, "--out", out)
			require.Equal(t, 1, code)
			require.Contains(t, stderr, c.want)
			require.Len(t, f.calls(), c.calls)
			require.NoFileExists(t, out)
		})
	}
}

func TestSignDelegationDryRunSignsNothing(t *testing.T) {
	r := authoritytest.NewRealm(t)
	f := newFixture(t)
	code, stdout, stderr := f.run("", "sign-delegation", "--anchors", f.file("anchors", r.AnchorBytes), "--first",
		"--spec", f.file("spec.json", specJSON(t, r.Current.D)), "--dry-run")
	require.Equal(t, 0, code, stderr)
	require.Contains(t, stdout, hex.EncodeToString(ac.Digest(r.Current.Msg)))
	require.Contains(t, stdout, "nothing was signed")
	require.Empty(t, f.calls())

	first := r.Current
	nxt := r.NextDelegation(t)
	code, stdout, stderr = f.run("", "sign-delegation", "--anchors", f.file("anchors", r.AnchorBytes),
		"--trust", f.file("trust", trustFile(first.Signed)), "--spec", f.file("spec2.json", specJSON(t, nxt.D)), "--dry-run")
	require.Equal(t, 0, code, stderr)
	require.Contains(t, stdout, "trust: high water generation 0 serial 1")
	require.Empty(t, f.calls())
}

func TestSignDelegationRefusesABadTrustOrAnchors(t *testing.T) {
	r := authoritytest.NewRealm(t, func(d *ac.Delegation) { d.CutoverAtMs = r0(t) })
	first := r.Current
	a := r.NextDelegation(t)
	b := r.NextDelegation(t, func(d *ac.Delegation) { d.CutoverAtMs-- }) // T4: a higher serial lowers the cutover
	nxt := r.NextDelegation(t)
	for name, c := range map[string]struct {
		anchors, trust []byte
		want           string
	}{
		"a halted trust":    {anchors: r.AnchorBytes, trust: trustFile(first.Signed, a.Signed, b.Signed), want: "trust: authoritycontract: trust halt"},
		"malformed anchors": {anchors: []byte("x"), trust: trustFile(first.Signed), want: "anchors:"},
		"a malformed trust": {anchors: r.AnchorBytes, trust: []byte("x"), want: "trust:"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			code, _, stderr := f.run("", "sign-delegation", "--anchors", f.file("anchors", c.anchors),
				"--trust", f.file("trust", c.trust), "--spec", f.file("spec.json", specJSON(t, nxt.D)), "--dry-run")
			require.Equal(t, 1, code)
			require.Contains(t, stderr, c.want)
		})
	}
}

func TestSignDelegationSpecMustBeComplete(t *testing.T) {
	r := authoritytest.NewRealm(t)
	var m map[string]any
	require.NoError(t, json.Unmarshal(specJSON(t, r.Current.D), &m))
	for name, c := range map[string]struct {
		edit func(map[string]any)
		want string
	}{
		"a missing field":   {func(m map[string]any) { delete(m, "cutover_at_ms") }, "spec: cutover_at_ms is missing"},
		"a null field":      {func(m map[string]any) { m["cutover_at_ms"] = nil }, "spec: cutover_at_ms is null"},
		"an unknown field":  {func(m map[string]any) { m["cutover"] = 1 }, `spec: json: unknown field "cutover"`},
		"a non-hex key":     {func(m map[string]any) { m["rsk_public_keys"] = []string{"zz"} }, "spec: rsk_public_keys[0]"},
		"a point too short": {func(m map[string]any) { m["rsk_public_keys"] = []string{"04"} }, "spec: not a valid delegation"},
	} {
		t.Run(name, func(t *testing.T) {
			cp := map[string]any{}
			for k, v := range m {
				cp[k] = v
			}
			c.edit(cp)
			b, err := json.Marshal(cp)
			require.NoError(t, err)
			f := newFixture(t)
			code, _, stderr := f.run("", "sign-delegation", "--anchors", f.file("anchors", r.AnchorBytes), "--first",
				"--spec", f.file("spec.json", b), "--dry-run")
			require.Equal(t, 1, code)
			require.Contains(t, stderr, c.want)
		})
	}
}

func TestUsage(t *testing.T) {
	f := newFixture(t)
	for name, args := range map[string][]string{
		"no verb":                     {},
		"an unknown verb":             {"keygen"},
		"--trust and --first":         {"sign-delegation", "--anchors", "a", "--trust", "t", "--first", "--spec", "s", "--dry-run"},
		"neither --trust nor --first": {"sign-delegation", "--anchors", "a", "--spec", "s", "--dry-run"},
		"--out and --dry-run":         {"sign-delegation", "--anchors", "a", "--first", "--spec", "s", "--out", "o", "--dry-run"},
		"an extra argument":           {"operator-sign", "--intent", "i", "--key", "k", "extra"},
		"operator-sign without --key": {"operator-sign", "--intent", "i"},
	} {
		t.Run(name, func(t *testing.T) {
			code, _, stderr := f.run("", args...)
			require.Equal(t, 2, code)
			require.Contains(t, stderr, "usage:")
		})
	}
}

func intentB64(t *testing.T, r *authoritytest.Realm, expires time.Time) ([]byte, string) {
	b := ac.Intent{RealmID: r.ID, AccountID: uuid.NewString(), RequestID: uuid.NewString(), Op: ac.OpRevokeRoot,
		ExpiresAtMs: uint64(expires.UnixMilli()), ExpectedSeq: 3}.Encode()
	require.NotNil(t, b)
	return b, base64.StdEncoding.EncodeToString(b)
}

func TestOperatorSign(t *testing.T) {
	r := authoritytest.NewRealm(t)
	f := newFixture(t, r.Operator)
	intent, b64 := intentB64(t, r, time.Now().Add(time.Minute))
	code, stdout, stderr := f.run(prefix(intent)+"\n", "operator-sign", "--intent", f.file("intent", []byte(b64+"\n")),
		"--key", hex.EncodeToString(authoritytest.Point(r.Operator)))
	require.Equal(t, 0, code, stderr)
	require.Contains(t, stdout, "op revoke_root")
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	sig, err := base64.StdEncoding.DecodeString(lines[len(lines)-1])
	require.NoError(t, err)
	msg := ac.Sig{Role: ac.RoleOperator, Digest: ac.Digest(intent)}.Encode()
	require.True(t, ac.VerifyP384(authoritytest.Point(r.Operator), msg, sig), "sig(operator, intent_digest)")
}

func TestOperatorSignRefuses(t *testing.T) {
	r := authoritytest.NewRealm(t)
	live, liveB64 := intentB64(t, r, time.Now().Add(time.Minute))
	_, expiredB64 := intentB64(t, r, time.Now().Add(-time.Minute))
	opKey := hex.EncodeToString(authoritytest.Point(r.Operator))
	cases := map[string]struct{ intent, key, stdin, want string }{
		"an expired intent":      {intent: expiredB64, key: opKey, want: "expired"},
		"not base64":             {intent: "!!", key: opKey, want: "not base64"},
		"not an intent":          {intent: base64.StdEncoding.EncodeToString([]byte("x")), key: opKey, want: "intent:"},
		"a key that is no point": {intent: liveB64, key: "04ab", want: "not a P-384 point"},
		"another operator key": {intent: liveB64, key: hex.EncodeToString(authoritytest.Point(r.CAK)),
			stdin: prefix(live) + "\n", want: "does not verify"},
		"a wrong confirmation": {intent: liveB64, key: opKey, stdin: "nope\n", want: "does not match"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, r.Operator)
			code, _, stderr := f.run(c.stdin, "operator-sign", "--intent", f.file("intent", []byte(c.intent)), "--key", c.key)
			require.Equal(t, 1, code)
			require.Contains(t, stderr, c.want)
		})
	}
}

func TestDERToP1363(t *testing.T) {
	k, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	msg := []byte("m")
	h := sha512.Sum384(msg)
	der, err := ecdsa.SignASN1(rand.Reader, k, h[:])
	require.NoError(t, err)
	sig, err := derToP1363(der)
	require.NoError(t, err)
	require.Len(t, sig, 96)
	require.True(t, ac.VerifyP384(authoritytest.Point(k), msg, sig))

	n := elliptic.P384().Params().N
	seq := func(r, s []byte) []byte {
		body := append(append([]byte{0x02, byte(len(r))}, r...), append([]byte{0x02, byte(len(s))}, s...)...) //nolint:gosec // G115: the test's short integers fit a byte
		return append([]byte{0x30, byte(len(body))}, body...)                                                 //nolint:gosec // G115: the test's short integers fit a byte
	}
	one := []byte{1}
	for name, b := range map[string][]byte{
		"empty":                    {},
		"a trailing byte":          append(append([]byte{}, der...), 0),
		"a long-form short length": append([]byte{0x30, 0x81, der[1]}, der[2:]...),
		"a SET":                    append([]byte{0x31}, der[1:]...),
		"a zero-padded integer":    seq([]byte{0, 1}, one),
		"r = 0":                    seq([]byte{0}, one),
		"a negative r":             seq([]byte{0xff}, one),
		"s = n":                    seq(one, append([]byte{0}, n.Bytes()...)),
		"r above n":                seq(append([]byte{0}, new(big.Int).Add(n, big.NewInt(1)).Bytes()...), one),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := derToP1363(b)
			require.ErrorIs(t, err, errStrictDER)
		})
	}
	small, err := derToP1363(seq(one, one))
	require.NoError(t, err, "short integers are left-padded to 48 bytes")
	require.Equal(t, byte(1), small[47])
	require.Equal(t, byte(1), small[95])
}

// D-43: the ceremony tool never ships in a server artifact.
func TestNoServerImageBuildsTheCeremonyTool(t *testing.T) {
	files, err := filepath.Glob("../../Dockerfile*")
	require.NoError(t, err)
	require.NotEmpty(t, files)
	for _, p := range files {
		b, err := os.ReadFile(p) //nolint:gosec // G304: a Dockerfile in this module
		require.NoError(t, err)
		require.NotContains(t, string(b), "authority-ceremony", p)
		require.NotContains(t, string(b), "./cmd/...", p)
	}
}

func newKeys(t *testing.T, n int) []*ecdsa.PrivateKey {
	ks := make([]*ecdsa.PrivateKey, n)
	for i := range ks {
		k, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		require.NoError(t, err)
		ks[i] = k
	}
	return ks
}

func successionJSON(t *testing.T, r *authoritytest.Realm, prev uint64, next []*ecdsa.PrivateKey) []byte {
	pt := func(k *ecdsa.PrivateKey) string { return hex.EncodeToString(authoritytest.Point(k)) }
	b, err := json.Marshal(successionSpec{RealmID: r.ID, PrevRootGen: prev, NewRootGen: prev + 1,
		NewPrimaryA: pt(next[0]), NewPrimaryB: pt(next[1]), NewRecoveryA: pt(next[2]), NewRecoveryB: pt(next[3])})
	require.NoError(t, err)
	return b
}

func TestSignSuccession(t *testing.T) {
	r := authoritytest.NewRealm(t)
	next := newKeys(t, 4)
	f := newFixture(t, r.RecoveryA, r.RecoveryB)
	specPath := f.file("succession.json", successionJSON(t, r, 0, next))
	msg, _, err := readSuccessionSpec(specPath)
	require.NoError(t, err)
	out := filepath.Join(f.dir, "s1.signed")
	code, stdout, stderr := f.run(twice(msg), "sign-succession", "--anchors", f.file("anchors", r.AnchorBytes),
		"--trust", f.file("trust", trustFile(r.Current.Signed)), "--spec", specPath, "--out", out)
	require.Equal(t, 0, code, stderr)
	require.Equal(t, 2, strings.Count(stdout, "digest (SHA-384 of the succession bytes):\n  "+hex.EncodeToString(ac.Digest(msg))))
	require.Contains(t, stdout, "recovery A′")
	signed, err := os.ReadFile(out) //nolint:gosec // G304: a test temp file
	require.NoError(t, err)
	tr := r.Trust(t)
	require.NoError(t, tr.AddSigned(signed))
	require.Equal(t, uint64(1), tr.CurrentGen())
	a, b := tr.CurrentPrimaries()
	require.Equal(t, authoritytest.Point(next[0]), a)
	require.Equal(t, authoritytest.Point(next[1]), b)
}

func TestSignSuccessionRefuses(t *testing.T) {
	r := authoritytest.NewRealm(t)
	next := newKeys(t, 4)
	reused := []*ecdsa.PrivateKey{r.PrimaryA, next[1], next[2], next[3]}
	cases := map[string]struct {
		spec  []byte
		keys  []*ecdsa.PrivateKey
		dry   bool
		want  string
		calls int
	}{
		"an anchor key reused (T2)": {spec: successionJSON(t, r, 0, reused), want: "distinct and new to the realm"},
		"a generation not current":  {spec: successionJSON(t, r, 1, next), want: "not the current generation 0"},
		"a missing field":           {spec: []byte(`{"realm_id":"x"}`), want: "spec: prev_root_gen is missing"},
		"primary A in the reader for A′": {spec: successionJSON(t, r, 0, next), keys: []*ecdsa.PrivateKey{r.PrimaryA},
			want: "recovery A′: the signature does not verify", calls: 1},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, c.keys...)
			specPath := f.file("succession.json", c.spec)
			msg, _, _ := readSuccessionSpec(specPath)
			out := filepath.Join(f.dir, "out")
			code, _, stderr := f.run(twice(msg), "sign-succession", "--anchors", f.file("anchors", r.AnchorBytes),
				"--trust", f.file("trust", trustFile(r.Current.Signed)), "--spec", specPath, "--out", out)
			require.Equal(t, 1, code)
			require.Contains(t, stderr, c.want)
			require.Len(t, f.calls(), c.calls)
			require.NoFileExists(t, out)
		})
	}
	f := newFixture(t)
	code, stdout, stderr := f.run("", "sign-succession", "--anchors", f.file("anchors", r.AnchorBytes),
		"--trust", f.file("trust", trustFile(r.Current.Signed)), "--spec", f.file("s.json", successionJSON(t, r, 0, next)), "--dry-run")
	require.Equal(t, 0, code, stderr)
	require.Contains(t, stdout, "nothing was signed")
	require.Empty(t, f.calls())
	code, _, stderr = f.run("", "sign-succession", "--anchors", "a", "--spec", "s", "--dry-run")
	require.Equal(t, 2, code, "--trust is required: a succession has no --first")
	require.Contains(t, stderr, "usage:")
}

// runTool runs a verb that touches no PIV tool.
func runTool(stdin string, args ...string) (int, string, string) {
	var out, errw bytes.Buffer
	code := run(args, env{in: bufio.NewReader(strings.NewReader(stdin)), tty: strings.NewReader(""),
		out: &out, errw: &errw, now: time.Now})
	return code, out.String(), errw.String()
}

func signedSuccession(t *testing.T, r *authoritytest.Realm, next []*ecdsa.PrivateKey) []byte {
	msg := ac.Succession{RealmID: r.ID, PrevRootGen: 0, NewRootGen: 1, NewPrimaryA: authoritytest.Point(next[0]),
		NewPrimaryB: authoritytest.Point(next[1]), NewRecoveryA: authoritytest.Point(next[2]),
		NewRecoveryB: authoritytest.Point(next[3])}.Encode()
	require.NotNil(t, msg)
	return ac.Signed{Type: "succession", Msg: msg, SigA: authoritytest.SignRaw(t, r.RecoveryA, msg),
		SigB: authoritytest.SignRaw(t, r.RecoveryB, msg)}.Encode()
}

func TestAddTrust(t *testing.T) {
	r := authoritytest.NewRealm(t)
	f := newFixture(t)
	anchors := f.file("anchors", r.AnchorBytes)
	add := func(stdin string, args ...string) (int, string, string) {
		return runTool(stdin, append([]string{"add-trust", "--anchors", anchors}, args...)...)
	}
	t1 := filepath.Join(f.dir, "t1")
	code, stdout, stderr := add("", "--first", "--signed", f.file("d1", r.Current.Signed), "--out", t1)
	require.Equal(t, 0, code, stderr)
	require.Contains(t, stdout, "1 items, high water generation 0 serial 1")

	next := newKeys(t, 4)
	s1 := signedSuccession(t, r, next)
	t2 := filepath.Join(f.dir, "t2")
	code, _, stderr = add("", "--trust", t1, "--signed", f.file("s1", s1), "--out", t2)
	require.Equal(t, 0, code, stderr)

	d := r.Current.D
	d.ProductRootGen = 1
	msg := d.Encode()
	g1 := ac.Signed{Type: "delegation", Msg: msg, SigA: authoritytest.SignRaw(t, next[0], msg),
		SigB: authoritytest.SignRaw(t, next[1], msg)}.Encode()
	t3 := filepath.Join(f.dir, "t3")
	code, stdout, stderr = add("", "--trust", t2, "--signed", f.file("g1", g1), "--out", t3)
	require.Equal(t, 0, code, stderr)
	require.Contains(t, stdout, "high water generation 1 serial 1")

	b, err := os.ReadFile(t3) //nolint:gosec // G304: a test temp file
	require.NoError(t, err)
	te, err := ac.CheckTrustEnvelope(b)
	require.NoError(t, err)
	require.Equal(t, [][]byte{s1, r.Current.Signed, g1}, te.Items, "successions first, then delegations by (gen, serial)")
	tr := r.Trust(t)
	require.NoError(t, tr.AddTrust(b))
	require.Equal(t, uint64(1), tr.CurrentGen())
}

func TestAddTrustRefuses(t *testing.T) {
	r := authoritytest.NewRealm(t)
	d2 := r.NextDelegation(t)
	f := newFixture(t)
	anchors := f.file("anchors", r.AnchorBytes)
	trust := f.file("trust", trustFile(r.Current.Signed))
	stranger := newKeys(t, 2)
	forged := ac.Signed{Type: "delegation", Msg: d2.Msg, SigA: authoritytest.SignRaw(t, stranger[0], d2.Msg),
		SigB: authoritytest.SignRaw(t, stranger[1], d2.Msg)}.Encode()
	cases := map[string]struct {
		args []string
		want string
	}{
		"an item the trust already holds": {args: []string{"--trust", trust, "--signed", f.file("d1", r.Current.Signed)},
			want: "nothing was written"},
		"--first with a later delegation": {args: []string{"--first", "--signed", f.file("d2", d2.Signed)},
			want: "--first takes only the realm's first delegation"},
		"signatures not from the anchors": {args: []string{"--trust", trust, "--signed", f.file("forged", forged)},
			want: "nothing was written"},
		"an --out that exists": {args: []string{"--trust", trust, "--signed", f.file("d2b", d2.Signed), "--out", trust},
			want: "already exists"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(f.dir, "out")
			args := append([]string{"add-trust", "--anchors", anchors}, c.args...)
			if !strings.Contains(strings.Join(c.args, " "), "--out") {
				args = append(args, "--out", out)
			}
			code, _, stderr := runTool("", args...)
			require.Equal(t, 1, code)
			require.Contains(t, stderr, c.want)
			require.NoFileExists(t, out)
		})
	}
	code, _, _ := runTool("", "add-trust", "--anchors", anchors, "--trust", trust, "--first", "--signed", "x", "--out", "y")
	require.Equal(t, 2, code, "--trust and --first are exclusive")
}

func anchorsJSON(t *testing.T, id string, keys []*ecdsa.PrivateKey) []byte {
	b, err := json.Marshal(map[string]string{"realm_id": id,
		"primary_a": hex.EncodeToString(authoritytest.Point(keys[0])), "primary_b": hex.EncodeToString(authoritytest.Point(keys[1])),
		"recovery_a": hex.EncodeToString(authoritytest.Point(keys[2])), "recovery_b": hex.EncodeToString(authoritytest.Point(keys[3]))})
	require.NoError(t, err)
	return b
}

func TestMakeAnchors(t *testing.T) {
	f := newFixture(t)
	keys := newKeys(t, 4)
	id := uuid.NewString()
	out := filepath.Join(f.dir, "anchors")
	code, stdout, stderr := runTool("", "make-anchors", "--spec", f.file("spec", anchorsJSON(t, id, keys)), "--out", out)
	require.Equal(t, 0, code, stderr)
	b, err := os.ReadFile(out) //nolint:gosec // G304: a test temp file
	require.NoError(t, err)
	require.Contains(t, stdout, "sha384="+hex.EncodeToString(ac.Digest(b)))
	require.Contains(t, stdout, "primary_a="+hex.EncodeToString(authoritytest.Point(keys[0])))
	a, err := ac.DecodeAnchors(b)
	require.NoError(t, err)
	require.Equal(t, ac.Anchors{RealmID: id, PrimaryA: authoritytest.Point(keys[0]), PrimaryB: authoritytest.Point(keys[1]),
		RecoveryA: authoritytest.Point(keys[2]), RecoveryB: authoritytest.Point(keys[3])}, a)
}

func TestMakeAnchorsRefuses(t *testing.T) {
	f := newFixture(t)
	keys := newKeys(t, 4)
	id := uuid.NewString()
	var extra map[string]string
	require.NoError(t, json.Unmarshal(anchorsJSON(t, id, keys), &extra))
	extra["serial"] = "1"
	withExtra, err := json.Marshal(extra)
	require.NoError(t, err)
	delete(extra, "serial")
	delete(extra, "recovery_b")
	missing, err := json.Marshal(extra)
	require.NoError(t, err)
	cases := map[string]struct {
		spec []byte
		want string
	}{
		"a repeated key":      {anchorsJSON(t, id, []*ecdsa.PrivateKey{keys[0], keys[1], keys[2], keys[0]}), "distinct"},
		"a realm id not UUID": {anchorsJSON(t, "realm", keys), "UUID"},
		"a missing field":     {missing, "recovery_b is missing"},
		"an unknown field":    {withExtra, "unknown field"},
		"bad hex":             {[]byte(strings.Replace(string(anchorsJSON(t, id, keys)), `"primary_a":"`, `"primary_a":"zz`, 1)), "primary_a"},
		"a key in another case": {[]byte(strings.Replace(string(anchorsJSON(t, id, keys)), `{`, `{"PRIMARY_A":"00",`, 1)),
			`unknown field "PRIMARY_A"`},
		"a key given twice": {[]byte(strings.Replace(string(anchorsJSON(t, id, keys)), `{`, `{"primary_a":"00",`, 1)),
			"primary_a is given twice"},
		"trailing data": {append(anchorsJSON(t, id, keys), []byte(`{}`)...), "trailing data"},
		"not an object": {[]byte(`[]`), "not a JSON object"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(f.dir, "out")
			code, _, stderr := runTool("", "make-anchors", "--spec", f.file("spec", c.spec), "--out", out)
			require.Equal(t, 1, code)
			require.Contains(t, stderr, c.want)
			require.NoFileExists(t, out)
		})
	}
	existing := f.file("existing", []byte("x"))
	code, _, stderr := runTool("", "make-anchors", "--spec", f.file("spec", anchorsJSON(t, id, keys)), "--out", existing)
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "already exists")
	code, _, _ = runTool("", "make-anchors", "--spec", "s")
	require.Equal(t, 2, code, "--out is required")
}

func TestShowTrust(t *testing.T) {
	r := authoritytest.NewRealm(t)
	d1 := r.Current
	d2 := r.NextDelegation(t, func(d *ac.Delegation) { d.NotBeforeMs++; d.NotAfterMs += 86_400_000 })
	require.NotEqual(t, d1.D.NotAfterMs, d2.D.NotAfterMs, "precondition: the window identifies the delegation")
	f := newFixture(t)
	anchors := f.file("anchors", r.AnchorBytes)
	code, stdout, stderr := runTool("", "show-trust", "--anchors", anchors,
		"--trust", f.file("trust", trustFile(d1.Signed, d2.Signed)))
	require.Equal(t, 0, code, stderr)
	require.Equal(t, fmt.Sprintf("realm_id=%s\ngeneration=0\nserial=%d\nmin_valid_serial=%d\nnot_before_ms=%d\nnot_after_ms=%d\n",
		r.ID, d2.D.Serial, d2.D.MinValidSerial, d2.D.NotBeforeMs, d2.D.NotAfterMs), stdout)

	code, _, _ = runTool("", "show-trust", "--anchors", anchors, "--trust", f.file("empty", trustFile()))
	require.Equal(t, 1, code, "an empty trust")
	succ := signedSuccession(t, r, newKeys(t, 4))
	code, _, stderr = runTool("", "show-trust", "--anchors", anchors, "--trust", f.file("succ", trustFile(succ, d1.Signed)))
	require.Equal(t, 1, code, "a succession with no delegation of the new generation is halted")
	require.Contains(t, stderr, "no valid delegation for the current generation", "the halt says why")
	other := authoritytest.NewRealm(t)
	code, _, _ = runTool("", "show-trust", "--anchors", f.file("other", other.AnchorBytes),
		"--trust", f.file("trust1", trustFile(d1.Signed)))
	require.Equal(t, 1, code, "another realm's anchors")
	code, _, _ = runTool("", "show-trust", "--anchors", anchors)
	require.Equal(t, 2, code, "--trust is required")
}
