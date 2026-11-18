package busmac

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"unicode/utf8"
)

// envelopeMembers is the closed member set; each must appear exactly once.
var envelopeMembers = map[string]bool{"v": true, "kid": true, "ts": true, "mac": true, "body": true}

// integerLiteral is the only ts spelling accepted: no sign, fraction,
// exponent or leading zero. The TS module applies the same pattern to the raw
// member text, so `1800000000000.0` is refused on both sides.
var integerLiteral = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

type envelope struct {
	kid  string
	ts   int64
	mac  []byte
	body []byte
}

// decodeEnvelope parses the one JSON object a signed frame may be. It walks
// the top-level members itself, because json.Unmarshal into a struct keeps the
// LAST of two duplicate members: a frame carrying two bodies would then open
// to a body the sender never signed in some decoders and not in others. The
// TS module refuses the same inputs with its own member scan.
func decodeEnvelope(data []byte) (envelope, error) {
	bad := envelope{}
	// A raw non-UTF-8 byte would be replaced silently by the decoder, letting
	// two different frames open to one body. Refuse it instead.
	if !utf8.Valid(data) {
		return bad, ErrBadEnvelope
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return bad, ErrBadEnvelope
	}
	var env envelope
	seen := make(map[string]bool, len(envelopeMembers))
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return bad, ErrBadEnvelope
		}
		name, ok := tok.(string)
		if !ok || !envelopeMembers[name] || seen[name] {
			return bad, ErrBadEnvelope
		}
		seen[name] = true
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil || !env.set(name, raw) {
			return bad, ErrBadEnvelope
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return bad, ErrBadEnvelope
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return bad, ErrBadEnvelope
	}
	if len(seen) != len(envelopeMembers) {
		return bad, ErrBadEnvelope
	}
	return env, nil
}

// set validates one member's raw JSON text and stores its value.
func (e *envelope) set(name string, raw json.RawMessage) bool {
	switch name {
	case "v":
		return string(raw) == "1"
	case "ts":
		if !integerLiteral.Match(raw) {
			return false
		}
		ts, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil || ts > maxTimestampMillis {
			return false
		}
		e.ts = ts
		return true
	case "kid":
		kid, ok := jsonString(raw)
		e.kid = kid
		return ok
	case "mac":
		enc, ok := jsonString(raw)
		if !ok {
			return false
		}
		mac, ok := decodeCanonical(enc)
		if !ok || len(mac) != macSize {
			return false
		}
		e.mac = mac
		return true
	case "body":
		body, ok := jsonString(raw)
		e.body = []byte(body)
		return ok
	}
	return false
}

// jsonString decodes raw only when it is a JSON string: Unmarshal into a
// string leaves the target untouched for `null`, which must not read as "".
func jsonString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// encodeEnvelope writes the envelope exactly as JSON.stringify writes
// {v, kid, ts, mac, body}, so a Go sender and a TS sender produce identical
// bytes. encoding/json differs from JSON.stringify (it escapes <, >, & and
// U+2028/U+2029), which is why this does not use it.
func encodeEnvelope(kid string, ts int64, mac, body []byte) []byte {
	out := make([]byte, 0, len(body)+len(kid)+96)
	out = append(out, `{"v":1,"kid":`...)
	out = appendJSONString(out, kid)
	out = append(out, `,"ts":`...)
	out = strconv.AppendInt(out, ts, 10)
	out = append(out, `,"mac":`...)
	out = appendJSONString(out, base64.RawURLEncoding.EncodeToString(mac))
	out = append(out, `,"body":`...)
	out = appendJSONString(out, string(body))
	return append(out, '}')
}

// appendJSONString is ECMAScript QuoteJSONString over valid UTF-8: the seven
// short escapes, \u00xx (lowercase hex) for the remaining C0 controls, and
// every other byte verbatim.
func appendJSONString(out []byte, s string) []byte {
	const hexDigits = "0123456789abcdef"
	out = append(out, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"', '\\':
			out = append(out, '\\', c)
		case '\b':
			out = append(out, '\\', 'b')
		case '\f':
			out = append(out, '\\', 'f')
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		default:
			if c < 0x20 {
				out = append(out, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
			} else {
				out = append(out, c)
			}
		}
	}
	return append(out, '"')
}
