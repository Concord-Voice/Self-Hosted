package busmac

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// vectorsPath is shared with services/media-plane/tests/busMac.test.ts, which
// replays every row below against the TS module. Regenerate after an
// intentional change with:
//
//	BUSMAC_UPDATE_VECTORS=1 go test ./pkg/busmac/ -run TestVectorsMatchGenerator
const vectorsPath = "testdata/vectors.json"

// testRawPurpose stands in for the raw purposes Security#175 registers: the
// shipped table holds families only. busMac.test.ts registers the same string
// through its registry mock, so the raw vectors replay on both sides.
const testRawPurpose Family = "test>test/raw"

func TestMain(m *testing.M) {
	purposes[testRawPurpose] = kindRaw
	os.Exit(m.Run())
}

const b64Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// Fixture keys are one repeated byte so the secret scanners do not read the
// shared vector file as committed key material. Their strength is irrelevant:
// the vectors pin an encoding, not a secret.
var (
	keyA     = bytes.Repeat([]byte{0x11}, MinKeyBytes)
	keyB     = bytes.Repeat([]byte{0x22}, MinKeyBytes)
	keyWrong = bytes.Repeat([]byte{0x33}, MinKeyBytes)
	keyLong  = bytes.Repeat([]byte{0x44}, 2*MinKeyBytes)
	keyShort = bytes.Repeat([]byte{0x11}, MinKeyBytes-1)
	// 0xff encodes to '_' in base64url and '/' in standard base64.
	keyHigh = bytes.Repeat([]byte{0xff}, MinKeyBytes)
)

const (
	recvMs         int64 = 1_800_000_000_000
	maxSafeMs      int64 = 1<<53 - 1
	subJoined            = "voice.joined"
	subLeft              = "voice.left"
	subTier              = "voice.tier_violation"
	bodyASCII            = `{"channelId":"c1"}`
	bodyDecoy            = `{"channelId":"decoy"}`
	bodyNonASCII         = `{"n":"é漢"}`
	bodyEscapeText       = `{"s":"\ud800"}` // backslash text, not a surrogate
	lifecycle            = string(FamilyLifecycle)
	telemetry            = string(FamilyTelemetry)
	rawPurpose           = string(testRawPurpose)
)

func b64(b []byte) string                   { return base64.RawURLEncoding.EncodeToString(b) }
func entryOf(kid string, key []byte) string { return kid + ":" + b64(key) }

var (
	ringA     = entryOf("a", keyA)
	ringAB    = entryOf("a", keyA) + "," + entryOf("b", keyB)
	ringBA    = entryOf("b", keyB) + "," + entryOf("a", keyA)
	ringWrong = entryOf("a", keyWrong)
)

// refMAC is the spec §5.3 formula written out independently of the package,
// so the vectors do not certify the implementation with itself. The blank
// discards follow the hash.Hash contract ("never returns an error").
func refMAC(root []byte, purpose, kid string, lines ...string) []byte {
	sub := hmac.New(sha256.New, root)
	_, _ = sub.Write([]byte("concord-bus-mac/v1/" + purpose))
	m := hmac.New(sha256.New, sub.Sum(nil))
	_, _ = m.Write([]byte("concord-bus-mac/v1\n" + purpose + "\n" + kid + "\n" + strings.Join(lines, "\n")))
	return m.Sum(nil)
}

func familyLinesOf(subject string, ts int64, body []byte) []string {
	sum := sha256.Sum256(body)
	return []string{subject, strconv.FormatInt(ts, 10), hex.EncodeToString(sum[:])}
}

func refFamilyMAC(root []byte, family, kid, subject string, ts int64, body []byte) []byte {
	return refMAC(root, family, kid, familyLinesOf(subject, ts, body)...)
}

func jsonLit(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err) // json.Marshal of a string cannot fail
	}
	return string(b)
}

func object(members ...string) string { return "{" + strings.Join(members, ",") + "}" }

func envelopeText(kid string, ts int64, mac []byte, bodyLiteral string) string {
	return object(`"v":1`, `"kid":`+jsonLit(kid), `"ts":`+strconv.FormatInt(ts, 10),
		`"mac":"`+b64(mac)+`"`, `"body":`+bodyLiteral)
}

func signedEnvelope(root []byte, family, kid, subject string, ts int64, body string) string {
	return envelopeText(kid, ts, refFamilyMAC(root, family, kid, subject, ts, []byte(body)), jsonLit(body))
}

// --- Case tables. Each is replayed by the Go tests below AND, through the
// generated vector file, by the TS suite, so both refuse the same inputs.

type parseCase struct {
	Name      string `json:"name"`
	Spec      string `json:"spec"`
	ActiveKID string `json:"active_kid,omitempty"`
	Error     string `json:"error,omitempty"`
}

func parseCases() []parseCase {
	okCase := func(name, spec, kid string) parseCase { return parseCase{Name: name, Spec: spec, ActiveKID: kid} }
	bad := func(name, spec, code string) parseCase { return parseCase{Name: name, Spec: spec, Error: code} }
	encA := b64(keyA)
	last := strings.IndexByte(b64Alphabet, encA[len(encA)-1])
	nonCanonical := encA[:len(encA)-1] + string(b64Alphabet[last^1]) // same bytes to a lenient decoder
	return []parseCase{
		okCase("single", ringA, "a"),
		okCase("two_entries_first_signs", ringAB, "a"),
		okCase("reordered_first_signs", ringBA, "b"),
		okCase("kid_16_chars", entryOf("abcdefghij012345", keyA), "abcdefghij012345"),
		okCase("key_64_bytes", entryOf("k1", keyLong), "k1"),
		okCase("key_high_bytes", entryOf("h", keyHigh), "h"),
		bad("empty", "", "empty_keyring"),
		bad("duplicate_kid", ringA+","+entryOf("a", keyB), "duplicate_kid"),
		bad("short_key_31_bytes", entryOf("a", keyShort), "short_key"),
		bad("empty_key", "a:", "short_key"),
		bad("uppercase_kid", entryOf("A1", keyA), "malformed_keyring"),
		bad("kid_17_chars", entryOf("abcdefghij0123456", keyA), "malformed_keyring"),
		bad("empty_kid", ":"+encA, "malformed_keyring"),
		bad("trailing_comma", ringA+",", "malformed_keyring"),
		bad("leading_comma", ","+ringA, "malformed_keyring"),
		bad("padding", ringA+"=", "malformed_keyring"),
		bad("standard_alphabet", "a:"+strings.ReplaceAll(b64(keyHigh), "_", "/"), "malformed_keyring"),
		bad("leading_space", " "+ringA, "malformed_keyring"),
		bad("trailing_space", ringA+" ", "malformed_keyring"),
		bad("trailing_newline", ringA+"\n", "malformed_keyring"),
		bad("trailing_crlf", ringA+"\r\n", "malformed_keyring"),
		bad("embedded_newline", "a:"+encA[:8]+"\n"+encA[8:], "malformed_keyring"),
		bad("no_colon", "a", "malformed_keyring"),
		bad("two_colons", ringA+":x", "malformed_keyring"),
		bad("non_canonical_key", "a:"+nonCanonical, "malformed_keyring"),
	}
}

type macCase struct {
	Name     string `json:"name"`
	Keyring  string `json:"keyring"`
	Family   string `json:"family"`
	Subject  string `json:"subject"`
	TsMillis int64  `json:"ts_ms"`
	Body     string `json:"body"`
	KID      string `json:"kid"`
	MACHex   string `json:"mac_hex"`
}

func macCases() []macCase {
	mk := func(name, ring string, root []byte, kid, family, subject string, ts int64, body string) macCase {
		return macCase{Name: name, Keyring: ring, Family: family, Subject: subject, TsMillis: ts, Body: body,
			KID: kid, MACHex: hex.EncodeToString(refFamilyMAC(root, family, kid, subject, ts, []byte(body)))}
	}
	return []macCase{
		mk("lifecycle_ascii", ringA, keyA, "a", lifecycle, subJoined, recvMs, bodyASCII),
		mk("lifecycle_non_ascii", ringA, keyA, "a", lifecycle, subLeft, recvMs, bodyNonASCII),
		mk("lifecycle_escape_text", ringA, keyA, "a", lifecycle, subJoined, recvMs, bodyEscapeText),
		mk("telemetry", ringA, keyA, "a", telemetry, subTier, recvMs, `{"tier":"x"}`),
		mk("empty_body_ts_zero", ringA, keyA, "a", lifecycle, subJoined, 0, ""),
		mk("max_safe_ts", ringA, keyA, "a", lifecycle, subJoined, maxSafeMs, bodyASCII),
		mk("first_entry_signs", ringBA, keyB, "b", lifecycle, subJoined, recvMs, bodyASCII),
	}
}

type rawCase struct {
	Name    string   `json:"name"`
	Keyring string   `json:"keyring"`
	Purpose string   `json:"purpose"`
	Lines   []string `json:"lines"`
	KID     string   `json:"kid"`
	MACHex  string   `json:"mac_hex"`
}

func rawCases() []rawCase {
	mk := func(name string, lines ...string) rawCase {
		return rawCase{Name: name, Keyring: ringA, Purpose: rawPurpose, Lines: lines, KID: "a",
			MACHex: hex.EncodeToString(refMAC(keyA, rawPurpose, "a", lines...))}
	}
	return []rawCase{
		mk("one_empty_line", ""),
		mk("three_lines", "x", "y", "z"),
		mk("non_ascii_lines", "é漢", "2"),
	}
}

type signCase struct {
	Name      string `json:"name"`
	Keyring   string `json:"keyring"`
	Family    string `json:"family"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	NowMillis int64  `json:"now_ms"`
	KID       string `json:"kid"`
	MACHex    string `json:"mac_hex"`
	Envelope  string `json:"envelope"`
}

// signCases pins the exact envelope bytes. Go produces them here; the TS suite
// must reproduce them with JSON.stringify, an independent encoder, and the Go
// suite re-reads them with encoding/json.
func signCases(t *testing.T) []signCase {
	t.Helper()
	mk := func(name, ring string, root []byte, kid, family, subject, body string) signCase {
		r, err := ParseKeyring(ring)
		require.NoError(t, err)
		env, err := r.Sign(Family(family), subject, []byte(body), time.UnixMilli(recvMs))
		require.NoError(t, err, name)
		return signCase{Name: name, Keyring: ring, Family: family, Subject: subject, Body: body, NowMillis: recvMs,
			KID: kid, MACHex: hex.EncodeToString(refFamilyMAC(root, family, kid, subject, recvMs, []byte(body))),
			Envelope: string(env)}
	}
	return []signCase{
		mk("ascii", ringA, keyA, "a", lifecycle, subJoined, bodyASCII),
		mk("non_ascii", ringA, keyA, "a", lifecycle, subJoined, bodyNonASCII),
		mk("escape_text", ringA, keyA, "a", lifecycle, subJoined, bodyEscapeText),
		mk("escapes_and_specials", ringA, keyA, "a", lifecycle, subJoined,
			"\"\\/\b\f\n\r\t\x00\x01\x1f\x7f<>&\u2028\u2029é"),
		mk("empty_body", ringA, keyA, "a", lifecycle, subJoined, ""),
		mk("telemetry", ringA, keyA, "a", telemetry, subTier, `{"tier":"x"}`),
		mk("first_entry_signs", ringBA, keyB, "b", lifecycle, subJoined, bodyASCII),
	}
}

type openCase struct {
	Name             string `json:"name"`
	Group            string `json:"group"`
	Keyring          string `json:"keyring"`
	Family           string `json:"family"`
	Subject          string `json:"subject"`
	Envelope         string `json:"envelope"`
	ReceivedAtMillis int64  `json:"received_at_ms"`
	BodyHex          string `json:"body_hex,omitempty"`
	Error            string `json:"error,omitempty"`
}

func openCases() []openCase {
	goodMAC := refFamilyMAC(keyA, lifecycle, "a", subJoined, recvMs, []byte(bodyASCII))
	good := signedEnvelope(keyA, lifecycle, "a", subJoined, recvMs, bodyASCII)
	at := func(ts int64) string { return signedEnvelope(keyA, lifecycle, "a", subJoined, ts, bodyASCII) }
	// Both decoders turn the lone-surrogate escape into U+FFFD, EF BF BD.
	loneDecoded := "{\"s\":\"\uFFFD\"}"
	lone := envelopeText("a", recvMs, refFamilyMAC(keyA, lifecycle, "a", subJoined, recvMs, []byte(loneDecoded)),
		`"{\"s\":\"\ud800\"}"`)
	encMAC := b64(goodMAC)
	last := strings.IndexByte(b64Alphabet, encMAC[len(encMAC)-1])
	nonCanonicalMAC := encMAC[:len(encMAC)-1] + string(b64Alphabet[last^1])

	mV, mKid, mTs := `"v":1`, `"kid":"a"`, `"ts":`+strconv.FormatInt(recvMs, 10)
	mMac, mBody := `"mac":"`+encMAC+`"`, `"body":`+jsonLit(bodyASCII)
	decoyBody := `"body":` + jsonLit(bodyDecoy)
	zeroMac := `"mac":"` + b64(make([]byte, sha256.Size)) + `"`

	accept := func(name, ring, family, subject, envelope, body string) openCase {
		return openCase{Name: name, Group: "accept", Keyring: ring, Family: family, Subject: subject,
			Envelope: envelope, ReceivedAtMillis: recvMs, BodyHex: hex.EncodeToString([]byte(body))}
	}
	refuse := func(name, ring, family, subject, envelope, code string) openCase {
		return openCase{Name: name, Group: "refuse", Keyring: ring, Family: family, Subject: subject,
			Envelope: envelope, ReceivedAtMillis: recvMs, Error: code}
	}
	strict := func(name, envelope string) openCase {
		return openCase{Name: name, Group: "strict", Keyring: ringA, Family: lifecycle, Subject: subJoined,
			Envelope: envelope, ReceivedAtMillis: recvMs, Error: "bad_envelope"}
	}

	return []openCase{
		accept("ascii", ringA, lifecycle, subJoined, good, bodyASCII),
		accept("non_ascii", ringA, lifecycle, subJoined,
			signedEnvelope(keyA, lifecycle, "a", subJoined, recvMs, bodyNonASCII), bodyNonASCII),
		accept("escape_text", ringA, lifecycle, subJoined,
			signedEnvelope(keyA, lifecycle, "a", subJoined, recvMs, bodyEscapeText), bodyEscapeText),
		accept("lone_surrogate_escape", ringA, lifecycle, subJoined, lone, loneDecoded),
		accept("past_edge_60000ms", ringA, lifecycle, subJoined, at(recvMs-60_000), bodyASCII),
		accept("future_edge_5000ms", ringA, lifecycle, subJoined, at(recvMs+5_000), bodyASCII),
		accept("second_entry_verifies", ringAB, lifecycle, subJoined,
			signedEnvelope(keyB, lifecycle, "b", subJoined, recvMs, bodyASCII), bodyASCII),
		accept("reordered_ring_verifies", ringBA, lifecycle, subJoined, good, bodyASCII),
		accept("telemetry", ringA, telemetry, subTier,
			signedEnvelope(keyA, telemetry, "a", subTier, recvMs, `{"tier":"x"}`), `{"tier":"x"}`),
		accept("reordered_members_and_whitespace", ringA, lifecycle, subJoined,
			" {\n\t\"body\" : "+jsonLit(bodyASCII)+" , \"mac\":\""+encMAC+"\",\"ts\":"+
				strconv.FormatInt(recvMs, 10)+",\"kid\":\"a\",\"v\":1}\r\n", bodyASCII),

		refuse("wrong_key", ringWrong, lifecycle, subJoined, good, "bad_mac"),
		refuse("wrong_family", ringA, telemetry, subJoined, good, "bad_mac"),
		refuse("wrong_subject", ringA, lifecycle, subLeft, good, "bad_mac"),
		refuse("unknown_kid", ringA, lifecycle, subJoined,
			signedEnvelope(keyA, lifecycle, "z", subJoined, recvMs, bodyASCII), "unknown_kid"),
		refuse("kid_relabelled", ringAB, lifecycle, subJoined,
			envelopeText("b", recvMs, goodMAC, jsonLit(bodyASCII)), "bad_mac"),
		refuse("body_altered", ringA, lifecycle, subJoined,
			envelopeText("a", recvMs, goodMAC, jsonLit(bodyDecoy)), "bad_mac"),
		refuse("ts_altered", ringA, lifecycle, subJoined,
			envelopeText("a", recvMs-1, goodMAC, jsonLit(bodyASCII)), "bad_mac"),
		refuse("stale_60001ms", ringA, lifecycle, subJoined, at(recvMs-60_001), "skew"),
		refuse("future_5001ms", ringA, lifecycle, subJoined, at(recvMs+5_001), "skew"),
		refuse("max_safe_ts", ringA, lifecycle, subJoined, at(maxSafeMs), "skew"),
		refuse("kid_lone_surrogate_is_unknown", ringA, lifecycle, subJoined,
			object(mV, `"kid":"\ud800"`, mTs, mMac, mBody), "unknown_kid"),
		refuse("empty_kid_is_unknown", ringA, lifecycle, subJoined,
			object(mV, `"kid":""`, mTs, mMac, mBody), "unknown_kid"),

		strict("unknown_member", object(mV, mKid, mTs, mMac, mBody, `"x":1`)),
		strict("v_2", object(`"v":2`, mKid, mTs, mMac, mBody)),
		strict("v_string", object(`"v":"1"`, mKid, mTs, mMac, mBody)),
		strict("v_float", object(`"v":1.0`, mKid, mTs, mMac, mBody)),
		strict("v_missing", object(mKid, mTs, mMac, mBody)),
		strict("v_duplicate", object(mV, mV, mKid, mTs, mMac, mBody)),
		strict("ts_fraction", object(mV, mKid, `"ts":1.5`, mMac, mBody)),
		strict("ts_integral_float", object(mV, mKid, `"ts":1800000000000.0`, mMac, mBody)),
		strict("ts_exponent", object(mV, mKid, `"ts":1.8e12`, mMac, mBody)),
		strict("ts_negative", object(mV, mKid, `"ts":-1`, mMac, mBody)),
		strict("ts_string", object(mV, mKid, `"ts":"1800000000000"`, mMac, mBody)),
		strict("ts_unsafe", object(mV, mKid, `"ts":9007199254740992`, mMac, mBody)),
		strict("missing_mac", object(mV, mKid, mTs, mBody)),
		strict("missing_body", object(mV, mKid, mTs, mMac)),
		strict("duplicate_body_last_signed", object(mV, mKid, mTs, mMac, decoyBody, mBody)),
		strict("duplicate_mac_last_valid", object(mV, mKid, mTs, zeroMac, mMac, mBody)),
		strict("duplicate_body_escaped_name", object(mV, mKid, mTs, mMac, decoyBody, `"b`+`\`+`u006fdy":`+jsonLit(bodyASCII))),
		strict("kid_null", object(mV, `"kid":null`, mTs, mMac, mBody)),
		strict("kid_number", object(mV, `"kid":1`, mTs, mMac, mBody)),
		strict("body_object", object(mV, mKid, mTs, mMac, `"body":{"channelId":"c1"}`)),
		strict("body_null", object(mV, mKid, mTs, mMac, `"body":null`)),
		strict("mac_padded", object(mV, mKid, mTs, `"mac":"`+encMAC+`="`, mBody)),
		strict("mac_non_canonical", object(mV, mKid, mTs, `"mac":"`+nonCanonicalMAC+`"`, mBody)),
		strict("mac_standard_alphabet", object(mV, mKid, mTs, `"mac":"+`+encMAC[1:]+`"`, mBody)),
		strict("mac_16_bytes", object(mV, mKid, mTs, `"mac":"`+b64(goodMAC[:16])+`"`, mBody)),
		strict("not_an_object", "["+good+"]"),
		strict("null", "null"),
		strict("empty_object", "{}"),
		strict("truncated", good[:len(good)-1]),
		strict("trailing_value", good+" {}"),
		strict("empty_text", ""),
	}
}

type fieldCase struct {
	Name             string   `json:"name"`
	Group            string   `json:"group"`
	Op               string   `json:"op"`
	Keyring          string   `json:"keyring"`
	Family           string   `json:"family"`
	KID              string   `json:"kid"`
	Subject          string   `json:"subject"`
	Lines            []string `json:"lines"`
	TsMillis         int64    `json:"ts_ms"`
	Body             string   `json:"body"`
	Envelope         string   `json:"envelope"`
	ReceivedAtMillis int64    `json:"received_at_ms"`
	MACHex           string   `json:"mac_hex"`
	// Error is the refusal code for mac, raw_mac, sign and open ("" means the
	// call succeeds). verify_mac and raw_verify rows must always return false.
	Error string `json:"error"`
}

// fieldCases gives each verify row the MAC a check-free verifier would
// accept, so a "false" there is the named rule's doing and nothing else's.
func fieldCases() []fieldCase {
	row := func(group, name, op string, edits ...func(*fieldCase)) fieldCase {
		c := fieldCase{Group: group, Name: name, Op: op, Keyring: ringA, Family: lifecycle, KID: "a",
			Subject: subJoined, Lines: []string{"x"}, TsMillis: recvMs, Body: bodyASCII, ReceivedAtMillis: recvMs}
		if op == "raw_mac" || op == "raw_verify" {
			c.Family = rawPurpose
		}
		for _, edit := range edits {
			edit(&c)
		}
		switch c.Op {
		case "verify_mac":
			c.MACHex = hex.EncodeToString(refFamilyMAC(keyA, c.Family, c.KID, c.Subject, c.TsMillis, []byte(c.Body)))
		case "raw_verify":
			c.MACHex = hex.EncodeToString(refMAC(keyA, c.Family, c.KID, c.Lines...))
		case "open":
			c.Envelope = signedEnvelope(keyA, c.Family, c.KID, c.Subject, recvMs, c.Body)
		}
		return c
	}
	code := func(s string) func(*fieldCase) { return func(c *fieldCase) { c.Error = s } }
	family := func(f string) func(*fieldCase) { return func(c *fieldCase) { c.Family = f } }
	subject := func(s string) func(*fieldCase) { return func(c *fieldCase) { c.Subject = s } }
	kid := func(k string) func(*fieldCase) { return func(c *fieldCase) { c.KID = k } }
	lines := func(l ...string) func(*fieldCase) { return func(c *fieldCase) { c.Lines = l } }
	ts := func(v int64) func(*fieldCase) { return func(c *fieldCase) { c.TsMillis = v } }
	// The exact lines a family MAC covers: a raw call over them under a family
	// purpose would forge, or accept, that family's MAC.
	forged := lines(familyLinesOf(subJoined, recvMs, []byte(bodyASCII))...)

	return []fieldCase{
		row("field", "kid_newline_open", "open", kid("a\n"), code("bad_field")),
		row("field", "kid_nul_open", "open", kid("a\x00"), code("bad_field")),
		row("field", "kid_newline_verify_mac", "verify_mac", kid("a\n")),
		row("field", "kid_nul_raw_verify", "raw_verify", kid("a\x00")),
		row("field", "family_newline_mac", "mac", family(lifecycle+"\n"), code("bad_field")),
		row("field", "family_nul_raw_mac", "raw_mac", family(rawPurpose+"\x00"), code("bad_field")),
		row("field", "family_newline_sign", "sign", family(lifecycle+"\n"), code("bad_field")),
		row("field", "family_nul_open", "open", family(lifecycle+"\x00"), code("bad_field")),
		row("field", "family_newline_verify_mac", "verify_mac", family(lifecycle+"\n")),
		row("field", "subject_newline_mac", "mac", subject(subJoined+"\n"), code("bad_field")),
		row("field", "subject_nul_sign", "sign", subject(subJoined+"\x00"), code("bad_field")),
		row("field", "subject_newline_open", "open", subject(subJoined+"\n"), code("bad_field")),
		row("field", "subject_empty_mac", "mac", subject(""), code("bad_field")),
		row("field", "subject_newline_verify_mac", "verify_mac", subject(subJoined+"\n1")),
		row("field", "subject_nul_verify_mac", "verify_mac", subject(subJoined+"\x00")),
		row("field", "line_newline_raw_mac", "raw_mac", lines("a\nb"), code("bad_field")),
		row("field", "line_nul_raw_mac", "raw_mac", lines("a", "b\x00"), code("bad_field")),
		row("field", "no_lines_raw_mac", "raw_mac", lines(), code("bad_field")),
		row("field", "line_newline_raw_verify", "raw_verify", lines("a\nb")),
		row("field", "line_nul_raw_verify", "raw_verify", lines("a", "\x00")),
		row("field", "no_lines_raw_verify", "raw_verify", lines()),
		row("field", "ts_negative_mac", "mac", ts(-1), code("bad_field")),
		row("field", "ts_unsafe_mac", "mac", ts(maxSafeMs+1), code("bad_field")),
		row("field", "ts_unsafe_verify_mac", "verify_mac", ts(maxSafeMs+1)),
		row("field", "raw_baseline_succeeds", "raw_mac"),

		row("unregistered", "unregistered_sign", "sign", family("x"), code("unregistered_family")),
		row("unregistered", "unregistered_mac", "mac", family("x"), code("unregistered_family")),
		row("unregistered", "unregistered_raw_mac", "raw_mac", family(""), code("unregistered_family")),
		row("unregistered", "unregistered_open", "open", family("x"), code("unregistered_family")),
		row("unregistered", "unregistered_verify_mac", "verify_mac", family("x")),
		row("unregistered", "unregistered_raw_verify", "raw_verify", family("mp>cp/other")),
		row("unregistered", "raw_purpose_is_not_a_family_mac", "mac", family(rawPurpose), code("unregistered_family")),
		row("unregistered", "raw_purpose_is_not_a_family_sign", "sign", family(rawPurpose), code("unregistered_family")),
		row("unregistered", "raw_purpose_is_not_a_family_open", "open", family(rawPurpose), code("unregistered_family")),
		row("unregistered", "raw_purpose_is_not_a_family_verify", "verify_mac", family(rawPurpose)),

		row("reserved", "lifecycle_raw_mac", "raw_mac", family(lifecycle), forged, code("reserved_purpose")),
		row("reserved", "telemetry_raw_mac", "raw_mac", family(telemetry), forged, code("reserved_purpose")),
		row("reserved", "lifecycle_raw_verify", "raw_verify", family(lifecycle), forged),
		row("reserved", "telemetry_raw_verify", "raw_verify", family(telemetry), forged),
	}
}

type vectorFile struct {
	Comment        string      `json:"_comment"`
	Families       []string    `json:"families"`
	TestRawPurpose string      `json:"test_raw_purpose"`
	ParseKeyring   []parseCase `json:"parse_keyring"`
	MAC            []macCase   `json:"mac"`
	Raw            []rawCase   `json:"raw"`
	Sign           []signCase  `json:"sign"`
	Open           []openCase  `json:"open"`
	Fields         []fieldCase `json:"fields"`
}

// registeredFamilies lists the shipped families, excluding testRawPurpose.
func registeredFamilies() []string {
	names := make([]string, 0, len(purposes))
	for f, kind := range purposes {
		if kind == kindFamily {
			names = append(names, string(f))
		}
	}
	sort.Strings(names)
	return names
}

func generateVectors(t *testing.T) []byte {
	t.Helper()
	file := vectorFile{
		Comment: "Shared vectors for services/control-plane/pkg/busmac (Go) and " +
			"services/media-plane/src/lib/busMac.ts (TS). Generated by TestVectorsMatchGenerator; " +
			"regenerate with BUSMAC_UPDATE_VECTORS=1. Keys are repeated-byte test fixtures. " +
			"test_raw_purpose is registered by both test suites only.",
		Families:       registeredFamilies(),
		TestRawPurpose: rawPurpose,
		ParseKeyring:   parseCases(),
		MAC:            macCases(),
		Raw:            rawCases(),
		Sign:           signCases(t),
		Open:           openCases(),
		Fields:         fieldCases(),
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	require.NoError(t, enc.Encode(file))
	return buf.Bytes()
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	raw, err := os.ReadFile(vectorsPath)
	require.NoError(t, err)
	var file vectorFile
	require.NoError(t, json.Unmarshal(raw, &file))
	require.NotEmpty(t, file.Open)
	return file
}

// --- Runners shared by the vector replay and the named table tests.

var errorCodes = map[string]error{
	"empty_keyring":       ErrEmptyKeyring,
	"malformed_keyring":   ErrMalformedKeyring,
	"short_key":           ErrShortKey,
	"duplicate_kid":       ErrDuplicateKID,
	"bad_envelope":        ErrBadEnvelope,
	"unknown_kid":         ErrUnknownKID,
	"unregistered_family": ErrUnregisteredFamily,
	"reserved_purpose":    ErrReservedPurpose,
	"bad_field":           ErrBadField,
	"skew":                ErrSkew,
	"bad_mac":             ErrBadMAC,
}

// codeOf names the sentinel err matches, so a refusal is asserted by the rule
// that produced it rather than by "some error".
func codeOf(err error) string {
	if err == nil {
		return ""
	}
	for code, sentinel := range errorCodes {
		if errors.Is(err, sentinel) {
			return code
		}
	}
	return "unexpected:" + err.Error()
}

func mustRing(t *testing.T, spec string) *Keyring {
	t.Helper()
	ring, err := ParseKeyring(spec)
	require.NoError(t, err)
	return ring
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	require.NoError(t, err)
	return b
}

func runParseCase(t *testing.T, c parseCase) {
	ring, err := ParseKeyring(c.Spec)
	assert.Equal(t, c.Error, codeOf(err))
	if c.Error != "" {
		assert.Nil(t, ring)
		return
	}
	require.NotNil(t, ring)
	assert.Equal(t, c.ActiveKID, ring.ActiveKID())
}

func runOpenCase(t *testing.T, c openCase) {
	ring := mustRing(t, c.Keyring)
	body, err := ring.Open(Family(c.Family), c.Subject, []byte(c.Envelope), time.UnixMilli(c.ReceivedAtMillis))
	assert.Equal(t, c.Error, codeOf(err))
	if c.Error != "" {
		assert.Nil(t, body)
		return
	}
	assert.Equal(t, c.BodyHex, hex.EncodeToString(body))
}

func runFieldCase(t *testing.T, c fieldCase) {
	ring := mustRing(t, c.Keyring)
	f := Family(c.Family)
	switch c.Op {
	case "mac":
		_, mac, err := ring.MAC(f, c.Subject, c.TsMillis, []byte(c.Body))
		assert.Equal(t, c.Error, codeOf(err))
		assert.Nil(t, mac)
	case "raw_mac":
		kid, mac, err := ring.RawMAC(f, c.Lines)
		assert.Equal(t, c.Error, codeOf(err))
		if c.Error == "" {
			assert.Equal(t, "a", kid)
			assert.Equal(t, refMAC(keyA, c.Family, "a", c.Lines...), mac)
			return
		}
		assert.Nil(t, mac)
	case "sign":
		env, err := ring.Sign(f, c.Subject, []byte(c.Body), time.UnixMilli(c.TsMillis))
		assert.Equal(t, c.Error, codeOf(err))
		assert.Nil(t, env)
	case "open":
		body, err := ring.Open(f, c.Subject, []byte(c.Envelope), time.UnixMilli(c.ReceivedAtMillis))
		assert.Equal(t, c.Error, codeOf(err))
		assert.Nil(t, body)
	case "verify_mac":
		assert.False(t, ring.VerifyMAC(f, c.KID, c.Subject, c.TsMillis, []byte(c.Body), mustHex(t, c.MACHex)))
	case "raw_verify":
		assert.False(t, ring.RawVerify(f, c.KID, c.Lines, mustHex(t, c.MACHex)))
	default:
		t.Fatalf("unknown op %q", c.Op)
	}
}

// --- Tests.

func TestVectors(t *testing.T) {
	file := loadVectors(t)

	assert.Equal(t, registeredFamilies(), file.Families)
	assert.Equal(t, rawPurpose, file.TestRawPurpose)
	for _, c := range file.ParseKeyring {
		t.Run("parse/"+c.Name, func(t *testing.T) { runParseCase(t, c) })
	}
	for _, c := range file.MAC {
		t.Run("mac/"+c.Name, func(t *testing.T) {
			ring := mustRing(t, c.Keyring)
			kid, mac, err := ring.MAC(Family(c.Family), c.Subject, c.TsMillis, []byte(c.Body))
			require.NoError(t, err)
			assert.Equal(t, c.KID, kid)
			assert.Equal(t, c.MACHex, hex.EncodeToString(mac))
			assert.True(t, ring.VerifyMAC(Family(c.Family), c.KID, c.Subject, c.TsMillis, []byte(c.Body), mac))
		})
	}
	for _, c := range file.Raw {
		t.Run("raw/"+c.Name, func(t *testing.T) {
			ring := mustRing(t, c.Keyring)
			kid, mac, err := ring.RawMAC(Family(c.Purpose), c.Lines)
			require.NoError(t, err)
			assert.Equal(t, c.KID, kid)
			assert.Equal(t, c.MACHex, hex.EncodeToString(mac))
			assert.True(t, ring.RawVerify(Family(c.Purpose), c.KID, c.Lines, mac))
		})
	}
	for _, c := range file.Sign {
		t.Run("sign/"+c.Name, func(t *testing.T) {
			ring := mustRing(t, c.Keyring)
			env, err := ring.Sign(Family(c.Family), c.Subject, []byte(c.Body), time.UnixMilli(c.NowMillis))
			require.NoError(t, err)
			assert.Equal(t, c.Envelope, string(env))

			// Independent decode: the envelope is plain JSON carrying the
			// reference MAC, not merely bytes this package round-trips.
			var decoded struct {
				V    int    `json:"v"`
				KID  string `json:"kid"`
				Ts   int64  `json:"ts"`
				MAC  string `json:"mac"`
				Body string `json:"body"`
			}
			require.NoError(t, json.Unmarshal(env, &decoded))
			assert.Equal(t, 1, decoded.V)
			assert.Equal(t, c.KID, decoded.KID)
			assert.Equal(t, c.NowMillis, decoded.Ts)
			assert.Equal(t, b64(mustHex(t, c.MACHex)), decoded.MAC)
			assert.Equal(t, c.Body, decoded.Body)

			body, err := ring.Open(Family(c.Family), c.Subject, env, time.UnixMilli(c.NowMillis))
			require.NoError(t, err)
			assert.Equal(t, c.Body, string(body))
		})
	}
	for _, c := range file.Open {
		t.Run("open/"+c.Name, func(t *testing.T) { runOpenCase(t, c) })
	}
	for _, c := range file.Fields {
		t.Run("field/"+c.Name, func(t *testing.T) { runFieldCase(t, c) })
	}
}

func TestVectorsMatchGenerator(t *testing.T) {
	generated := generateVectors(t)
	if os.Getenv("BUSMAC_UPDATE_VECTORS") == "1" {
		require.NoError(t, os.MkdirAll(filepath.Dir(vectorsPath), 0o750))
		require.NoError(t, os.WriteFile(vectorsPath, generated, 0o600))
		return
	}
	committed, err := os.ReadFile(vectorsPath)
	require.NoError(t, err)
	require.Equal(t, string(generated), string(committed),
		"testdata/vectors.json is stale; regenerate with BUSMAC_UPDATE_VECTORS=1")
}

// TestOpenRefuses covers key, family, subject, kid and the skew window, with
// the accept rows (both window edges included) as positive controls.
func TestOpenRefuses(t *testing.T) {
	for _, c := range openCases() {
		if c.Group != "strict" {
			t.Run(c.Group+"/"+c.Name, func(t *testing.T) { runOpenCase(t, c) })
		}
	}
}

func TestFieldsRefuseNewlineAndNUL(t *testing.T) {
	for _, c := range fieldCases() {
		if c.Group == "field" {
			t.Run(c.Name, func(t *testing.T) { runFieldCase(t, c) })
		}
	}
}

func TestUnregisteredFamily(t *testing.T) {
	for _, c := range fieldCases() {
		if c.Group == "unregistered" {
			t.Run(c.Name, func(t *testing.T) { runFieldCase(t, c) })
		}
	}
	assert.True(t, Registered(FamilyLifecycle))
	assert.True(t, Registered(FamilyTelemetry))
	assert.False(t, Registered(Family("x")))
}

// A registered family owns its purpose string. RawMAC and RawVerify refuse it,
// so no raw caller can mint or accept that family's MAC by passing the
// family's own lines; the family path over the same lines still works.
func TestRawMACRefusesFamilyPurposes(t *testing.T) {
	for _, c := range fieldCases() {
		if c.Group == "reserved" {
			t.Run(c.Name, func(t *testing.T) { runFieldCase(t, c) })
		}
	}
	ring := mustRing(t, ringA)
	mac := refFamilyMAC(keyA, lifecycle, "a", subJoined, recvMs, []byte(bodyASCII))
	assert.True(t, ring.VerifyMAC(FamilyLifecycle, "a", subJoined, recvMs, []byte(bodyASCII), mac), "positive control")
}

func TestParseKeyringRefuses(t *testing.T) {
	for _, c := range parseCases() {
		t.Run(c.Name, func(t *testing.T) { runParseCase(t, c) })
	}
}

func TestEnvelopeStrict(t *testing.T) {
	for _, c := range openCases() {
		if c.Group == "strict" {
			t.Run(c.Name, func(t *testing.T) { runOpenCase(t, c) })
		}
	}
}

// A raw byte that is not UTF-8 is refused outright rather than silently
// replaced, so two different wire frames can never open to one body. The TS
// side receives a decoded string and cannot express this input.
func TestEnvelopeRefusesInvalidUTF8(t *testing.T) {
	good := signedEnvelope(keyA, lifecycle, "a", subJoined, recvMs, bodyASCII)
	bad := strings.Replace(good, `c1`, "c\xff", 1)

	_, err := mustRing(t, ringA).Open(FamilyLifecycle, subJoined, []byte(bad), time.UnixMilli(recvMs))
	assert.ErrorIs(t, err, ErrBadEnvelope)
}

func TestAnyKeyringEntryVerifies(t *testing.T) {
	signer := mustRing(t, ringAB)
	verifier := mustRing(t, ringBA)
	assert.Equal(t, "a", signer.ActiveKID())
	assert.Equal(t, "b", verifier.ActiveKID())

	env, err := signer.Sign(FamilyLifecycle, subJoined, []byte(bodyASCII), time.UnixMilli(recvMs))
	require.NoError(t, err)
	body, err := verifier.Open(FamilyLifecycle, subJoined, env, time.UnixMilli(recvMs))
	require.NoError(t, err)
	assert.Equal(t, bodyASCII, string(body))

	// A ring without the signing kid refuses it as unknown, not as a bad MAC.
	_, err = mustRing(t, entryOf("b", keyB)).Open(FamilyLifecycle, subJoined, env, time.UnixMilli(recvMs))
	assert.ErrorIs(t, err, ErrUnknownKID)
}

func TestNilAndEmptyKeyringNeverVerify(t *testing.T) {
	mac := refFamilyMAC(keyA, lifecycle, "a", subJoined, recvMs, []byte(bodyASCII))
	raw := refMAC(keyA, rawPurpose, "a", "x")
	env := []byte(signedEnvelope(keyA, lifecycle, "a", subJoined, recvMs, bodyASCII))

	for name, ring := range map[string]*Keyring{"nil": nil, "zero value": {}} {
		t.Run(name, func(t *testing.T) {
			assert.False(t, ring.VerifyMAC(FamilyLifecycle, "a", subJoined, recvMs, []byte(bodyASCII), mac))
			assert.False(t, ring.RawVerify(testRawPurpose, "a", []string{"x"}, raw))
			assert.Empty(t, ring.ActiveKID())

			_, err := ring.Open(FamilyLifecycle, subJoined, env, time.UnixMilli(recvMs))
			assert.ErrorIs(t, err, ErrEmptyKeyring)
			_, err = ring.Sign(FamilyLifecycle, subJoined, []byte(bodyASCII), time.UnixMilli(recvMs))
			assert.ErrorIs(t, err, ErrEmptyKeyring)
			_, _, err = ring.MAC(FamilyLifecycle, subJoined, recvMs, []byte(bodyASCII))
			assert.ErrorIs(t, err, ErrEmptyKeyring)
			_, _, err = ring.RawMAC(testRawPurpose, []string{"x"})
			assert.ErrorIs(t, err, ErrEmptyKeyring)
		})
	}

	ring, err := ParseKeyring("")
	assert.ErrorIs(t, err, ErrEmptyKeyring)
	assert.Nil(t, ring)
}

// The comparison is over the whole MAC: a prefix, an extension or one flipped
// bit never verifies, and the untouched MAC does (the positive control).
func TestVerifyMACComparesTheWholeMAC(t *testing.T) {
	ring := mustRing(t, ringA)
	mac := refFamilyMAC(keyA, lifecycle, "a", subJoined, recvMs, []byte(bodyASCII))
	verify := func(m []byte) bool {
		return ring.VerifyMAC(FamilyLifecycle, "a", subJoined, recvMs, []byte(bodyASCII), m)
	}
	flipped := append([]byte(nil), mac...)
	flipped[len(flipped)-1] ^= 1

	assert.True(t, verify(mac))
	assert.False(t, verify(mac[:16]), "prefix")
	assert.False(t, verify(append(append([]byte(nil), mac...), 0)), "extended")
	assert.False(t, verify(flipped), "flipped bit")
	assert.False(t, verify(nil), "empty")
}

func TestMACIsTheRawConstructionOverFamilyLines(t *testing.T) {
	ring := mustRing(t, ringA)
	body := []byte(bodyNonASCII)

	kid, mac, err := ring.MAC(FamilyLifecycle, subJoined, recvMs, body)
	require.NoError(t, err)
	assert.Equal(t, "a", kid)
	assert.Equal(t, refMAC(keyA, lifecycle, "a", familyLinesOf(subJoined, recvMs, body)...), mac)
}

func TestSignRefusesInvalidUTF8Body(t *testing.T) {
	ring := mustRing(t, ringA)
	_, err := ring.Sign(FamilyLifecycle, subJoined, []byte("{\"s\":\"\xff\"}"), time.UnixMilli(recvMs))
	assert.ErrorIs(t, err, ErrBadField)
}

// Keyring refusals surface in boot logs, so they must never echo the key.
func TestKeyringErrorsCarryNoKeyMaterial(t *testing.T) {
	for _, spec := range []string{entryOf("a", keyShort), ringA + "," + entryOf("a", keyB), ringA + " "} {
		_, err := ParseKeyring(spec)
		require.Error(t, err)
		for _, key := range [][]byte{keyShort, keyA, keyB} {
			assert.NotContains(t, err.Error(), b64(key)[:8])
		}
	}
}
