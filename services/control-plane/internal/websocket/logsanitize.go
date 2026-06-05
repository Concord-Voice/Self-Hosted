package websocket

import "strings"

// sanitizeLogValue strips control characters from a user-derived string before it
// is written to a log sink, preventing CWE-117 log forging (CRLF injection of
// fabricated log lines). `\n` and `\r` are removed via strings.ReplaceAll — the
// form the CodeQL go/log-injection sanitizer model recognizes — and any remaining
// C0 control characters plus DEL are dropped for defense-in-depth.
//
// Apply it to wire-derived string values logged via the stdlib logger. Values
// that are uuid.UUID (whose String() is structurally hex-only) or that are
// already escaped via the %q verb do not need it. See #1365 and
// [internal]rules/observability.md ("Logging Discipline").
func sanitizeLogValue(s string) string {
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, "\r", "")
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}
