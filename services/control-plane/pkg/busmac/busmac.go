// Package busmac authenticates frames on the internal NATS bus with a
// dedicated keyring, independently of the broker's own client authentication.
//
// # Construction
//
// A keyring holds one or more root keys, each named by a short key ID (kid).
// The first entry signs; any entry verifies, so a key rotates by prepending
// the new key on every verifier, then on the signer, then dropping the old one.
// Every MAC is computed under a per-purpose subkey:
//
//	subkey = HMAC-SHA256(root[kid], "concord-bus-mac/v1/" + purpose)
//	mac    = HMAC-SHA256(subkey, "concord-bus-mac/v1\n" + purpose + "\n" + kid + "\n" + join(lines, "\n"))
//
// A family MAC uses the lines [subject, ts, hex(sha256(body))], where subject
// is the NATS subject and ts is the decimal millisecond timestamp.
//
// # Invariant
//
// Two different (purpose, kid, lines) tuples never produce the same MAC input.
// The input is a newline-joined list, so this holds only while (1) no purpose,
// kid or line contains a newline, which every entry point refuses; (2) the
// line list is never empty, because joining no lines and joining one empty
// line give the same text; and (3) each purpose string has exactly one kind in
// the registry, because a raw purpose that equalled a family would let RawMAC
// compute that family's MAC from caller-chosen lines. NUL is refused beside
// newline so a C-string consumer can never truncate a field. Fields must be
// valid UTF-8 so the TS module, which hashes strings as UTF-8, cannot map two
// distinct inputs onto one byte sequence.
//
// What breaks it: accepting a newline anywhere, accepting an empty line list,
// registering one string under two kinds, or hashing a field in any encoding
// other than its UTF-8 bytes.
//
// # Failure posture
//
// Every operation fails closed. A nil or empty keyring, an unknown kid, a key
// shorter than MinKeyBytes, an unregistered purpose, a malformed field or MAC
// never verifies. Comparison is constant-time (hmac.Equal). Errors are fixed
// sentinels and never carry key material, a kid, a MAC or a subject.
//
// The `_, _ = mac.Write(...)` discards are the documented-safe case:
// hash.Hash.Write "never returns an error" per the stdlib contract.
package busmac

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// MinKeyBytes is the shortest root key ParseKeyring accepts.
	MinKeyBytes = 32
	// MaxPastSkew is how far before the receipt time a timestamp may sit.
	MaxPastSkew = 60 * time.Second
	// MaxFutureSkew is how far after the receipt time a timestamp may sit.
	MaxFutureSkew = 5 * time.Second
)

const (
	macVersion = "concord-bus-mac/v1"
	macSize    = sha256.Size
	maxKIDLen  = 16
	// maxTimestampMillis is 2^53-1, the largest integer the TS module can
	// represent exactly, so both sides accept the same timestamps.
	maxTimestampMillis = 1<<53 - 1
)

// Refusal sentinels. Each names one rule so a caller can classify a refusal
// without the error carrying any of the refused input.
var (
	ErrEmptyKeyring       = errors.New("busmac: empty keyring")
	ErrMalformedKeyring   = errors.New("busmac: malformed keyring")
	ErrShortKey           = errors.New("busmac: key shorter than minimum")
	ErrDuplicateKID       = errors.New("busmac: duplicate key id")
	ErrBadEnvelope        = errors.New("busmac: malformed envelope")
	ErrUnknownKID         = errors.New("busmac: unknown key id")
	ErrUnregisteredFamily = errors.New("busmac: unregistered family or purpose")
	ErrReservedPurpose    = errors.New("busmac: purpose is reserved for a family")
	ErrBadField           = errors.New("busmac: field refused")
	ErrSkew               = errors.New("busmac: timestamp outside window")
	ErrBadMAC             = errors.New("busmac: MAC mismatch")
)

type entry struct {
	kid string
	key []byte
}

// Keyring is an ordered set of root keys. Its zero value and a nil *Keyring
// are both empty: they sign nothing and verify nothing.
type Keyring struct {
	entries []entry
}

// ParseKeyring parses "<kid>:<base64url key>[,<kid>:<base64url key>...]".
// A kid matches ^[a-z0-9]{1,16}$ and is unique; a key is canonical unpadded
// base64url of at least MinKeyBytes bytes. Nothing is trimmed: whitespace, a
// trailing comma or a line ending is refused, so the caller decides what an
// operator-edited value means.
func ParseKeyring(spec string) (*Keyring, error) {
	if spec == "" {
		return nil, ErrEmptyKeyring
	}
	parts := strings.Split(spec, ",")
	ring := &Keyring{entries: make([]entry, 0, len(parts))}
	for _, part := range parts {
		e, err := parseEntry(part)
		if err != nil {
			return nil, err
		}
		if ring.lookup(e.kid) != nil {
			return nil, ErrDuplicateKID
		}
		ring.entries = append(ring.entries, e)
	}
	return ring, nil
}

func parseEntry(part string) (entry, error) {
	kid, encoded, ok := strings.Cut(part, ":")
	if !ok || !validKID(kid) {
		return entry{}, ErrMalformedKeyring
	}
	key, ok := decodeCanonical(encoded)
	if !ok {
		return entry{}, ErrMalformedKeyring
	}
	if len(key) < MinKeyBytes {
		return entry{}, ErrShortKey
	}
	return entry{kid: kid, key: key}, nil
}

func validKID(kid string) bool {
	if kid == "" || len(kid) > maxKIDLen {
		return false
	}
	for i := 0; i < len(kid); i++ {
		c := kid[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// decodeCanonical accepts only the one unpadded base64url spelling of a byte
// string. The stdlib decoder skips CR and LF and tolerates non-zero trailing
// bits, so the re-encode comparison is what makes the encoding unique.
func decodeCanonical(s string) ([]byte, bool) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || base64.RawURLEncoding.EncodeToString(b) != s {
		return nil, false
	}
	return b, true
}

// ActiveKID returns the kid that signs, or "" for an empty keyring.
func (k *Keyring) ActiveKID() string {
	if signer, ok := k.active(); ok {
		return signer.kid
	}
	return ""
}

func (k *Keyring) active() (entry, bool) {
	if k == nil || len(k.entries) == 0 || len(k.entries[0].key) < MinKeyBytes {
		return entry{}, false
	}
	return k.entries[0], true
}

// lookup returns the root key for kid, or nil. A key shorter than MinKeyBytes
// is treated as absent so no construction path can make a short key verify.
func (k *Keyring) lookup(kid string) []byte {
	if k == nil {
		return nil
	}
	for _, e := range k.entries {
		if e.kid == kid && len(e.key) >= MinKeyBytes {
			return e.key
		}
	}
	return nil
}

// validField reports whether s may appear in the MAC input: valid UTF-8 with
// no newline and no NUL.
func validField(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsAny(s, "\n\x00")
}

func checkPurpose(p Family, want purposeKind) error {
	if !validField(string(p)) {
		return ErrBadField
	}
	switch purposes[p] {
	case want:
		return nil
	case kindFamily:
		return ErrReservedPurpose
	default:
		return ErrUnregisteredFamily
	}
}

func checkLines(lines []string) error {
	if len(lines) == 0 {
		return ErrBadField
	}
	for _, line := range lines {
		if !validField(line) {
			return ErrBadField
		}
	}
	return nil
}

func familyLines(subject string, tsMillis int64, body []byte) ([]string, error) {
	if subject == "" || !validField(subject) || tsMillis < 0 || tsMillis > maxTimestampMillis {
		return nil, ErrBadField
	}
	sum := sha256.Sum256(body)
	return []string{subject, strconv.FormatInt(tsMillis, 10), hex.EncodeToString(sum[:])}, nil
}

func compute(root []byte, purpose Family, kid string, lines []string) []byte {
	sub := hmac.New(sha256.New, root)
	_, _ = sub.Write([]byte(macVersion + "/" + string(purpose)))
	mac := hmac.New(sha256.New, sub.Sum(nil))
	_, _ = mac.Write([]byte(macVersion + "\n" + string(purpose) + "\n" + kid + "\n" + strings.Join(lines, "\n")))
	return mac.Sum(nil)
}

// verify is the one comparison every verifier reaches, after its own field
// checks have passed.
func (k *Keyring) verify(purpose Family, kid string, lines []string, mac []byte) bool {
	if !validField(kid) || len(mac) != macSize {
		return false
	}
	root := k.lookup(kid)
	if root == nil {
		return false
	}
	return hmac.Equal(compute(root, purpose, kid, lines), mac)
}

// RawMAC computes a MAC over caller-supplied lines under a registered raw
// purpose, with no timestamp and no time window. A family's string is
// reserved and refused with ErrReservedPurpose.
func (k *Keyring) RawMAC(purpose Family, lines []string) (kid string, mac []byte, err error) {
	signer, ok := k.active()
	if !ok {
		return "", nil, ErrEmptyKeyring
	}
	if err := checkPurpose(purpose, kindRaw); err != nil {
		return "", nil, err
	}
	if err := checkLines(lines); err != nil {
		return "", nil, err
	}
	return signer.kid, compute(signer.key, purpose, signer.kid, lines), nil
}

// RawVerify reports whether mac authenticates lines under a registered raw
// purpose and kid. It is false for a family's string, as for every refusal.
func (k *Keyring) RawVerify(purpose Family, kid string, lines []string, mac []byte) bool {
	if checkPurpose(purpose, kindRaw) != nil || checkLines(lines) != nil {
		return false
	}
	return k.verify(purpose, kid, lines, mac)
}

// MAC computes the detached family MAC over subject, tsMillis and body.
func (k *Keyring) MAC(f Family, subject string, tsMillis int64, body []byte) (kid string, mac []byte, err error) {
	signer, ok := k.active()
	if !ok {
		return "", nil, ErrEmptyKeyring
	}
	if err := checkPurpose(f, kindFamily); err != nil {
		return "", nil, err
	}
	lines, err := familyLines(subject, tsMillis, body)
	if err != nil {
		return "", nil, err
	}
	return signer.kid, compute(signer.key, f, signer.kid, lines), nil
}

// VerifyMAC reports whether mac authenticates a detached family MAC. It
// applies no time window; Open is the entry point that does.
func (k *Keyring) VerifyMAC(f Family, kid, subject string, tsMillis int64, body, mac []byte) bool {
	if checkPurpose(f, kindFamily) != nil {
		return false
	}
	lines, err := familyLines(subject, tsMillis, body)
	if err != nil {
		return false
	}
	return k.verify(f, kid, lines, mac)
}

// Sign wraps body in a signed envelope for subject, stamped with now. The
// body must be valid UTF-8, because the envelope carries it as a JSON string
// and the receiver hashes what that string decodes to.
func (k *Keyring) Sign(f Family, subject string, body []byte, now time.Time) ([]byte, error) {
	ts := now.UnixMilli()
	kid, mac, err := k.MAC(f, subject, ts, body)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(body) {
		return nil, ErrBadField
	}
	return encodeEnvelope(kid, ts, mac, body), nil
}

// Open verifies an envelope received on subject at receivedAt and returns the
// body it authenticates. receivedAt must be the NATS callback time, taken once
// per message, never a handler's own clock reading.
//
// Refusals are checked in a fixed order, shared with the TS module: keyring,
// family, subject, envelope shape, kid, skew, then the MAC.
func (k *Keyring) Open(f Family, subject string, envelope []byte, receivedAt time.Time) ([]byte, error) {
	if _, ok := k.active(); !ok {
		return nil, ErrEmptyKeyring
	}
	if err := checkPurpose(f, kindFamily); err != nil {
		return nil, err
	}
	if subject == "" || !validField(subject) {
		return nil, ErrBadField
	}
	env, err := decodeEnvelope(envelope)
	if err != nil {
		return nil, err
	}
	if !validField(env.kid) {
		return nil, ErrBadField
	}
	if k.lookup(env.kid) == nil {
		return nil, ErrUnknownKID
	}
	recv := receivedAt.UnixMilli()
	if env.ts < recv-MaxPastSkew.Milliseconds() || env.ts > recv+MaxFutureSkew.Milliseconds() {
		return nil, ErrSkew
	}
	if !k.VerifyMAC(f, env.kid, subject, env.ts, env.body, env.mac) {
		return nil, ErrBadMAC
	}
	return env.body, nil
}
