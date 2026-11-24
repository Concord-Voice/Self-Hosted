package admin_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/admin"
)

func TestValidateAdminUsername_AcceptsTheAllowlist(t *testing.T) {
	for _, ok := range []string{
		"a", "alice", "ops-admin_2", "first.last", "0", "-", "_.-",
		strings.Repeat("x", admin.MaxAdminUsernameLength),
	} {
		assert.NoError(t, admin.ValidateAdminUsername(ok), "%q", ok)
	}
}

func TestValidateAdminUsername_RefusesUppercaseControlSpaceAndLength(t *testing.T) {
	for _, bad := range []string{
		"", "evil\x01", "\x01\x02", "tab\there", "line\nbreak", "del\x7f", "c1\u0085",
		"two words", " leading", "trailing ", " nbsp", "bad\xffutf8", "nul\x00",
		"slash/name", "at@name", "per%cent", "amp&name", "q?name", "hash#name", "plus+name",
		"Alice", "aIice", "ALICE", "opsO", "pipe|name",
		strings.Repeat("x", admin.MaxAdminUsernameLength+1),
	} {
		assert.ErrorIs(t, admin.ValidateAdminUsername(bad), admin.ErrInvalidUsername, "%q", bad)
	}
}

// Every character here is invisible or default-ignorable, so "admin" followed
// by it reads as "admin" in the audit log. unicode.IsPrint accepts most of
// them, which is how such a name became a second "admin".
func TestValidateAdminUsername_RefusesInvisibleAndIgnorable(t *testing.T) {
	for _, r := range []rune{
		0x034F,                 // COMBINING GRAPHEME JOINER
		0xFE00, 0xFE0E, 0xFE0F, // variation selectors
		0xE0100, 0xE01EF, // variation selectors supplement
		0x3164,         // HANGUL FILLER
		0x115F, 0x1160, // HANGUL CHOSEONG / JUNGSEONG FILLER
		0xFFA0,                 // HALFWIDTH HANGUL FILLER
		0x2800,                 // BRAILLE PATTERN BLANK
		0x180B, 0x180C, 0x180D, // MONGOLIAN FREE VARIATION SELECTORS
		0x180E,         // MONGOLIAN VOWEL SEPARATOR
		0x17B4, 0x17B5, // KHMER VOWEL INHERENT AQ / AA
		0x200B, 0x200C, 0x200D, 0x2060, 0xFEFF, // zero-width characters
		0x00AD,                 // SOFT HYPHEN
		0x202E, 0x2066, 0x061C, // bidi controls
		0xE0001, 0xE0041, // tag characters
		0x3000, 0x2007, // non-ASCII spaces
	} {
		name := "admin" + string(r)
		assert.ErrorIs(t, admin.ValidateAdminUsername(name), admin.ErrInvalidUsername, "admin+U+%04X", r)
	}
}

// NFC and NFD spell one name two ways, and mixed-script lookalikes read as
// the ASCII name: none is a second admin.
func TestValidateAdminUsername_RefusesNormalizationFormsAndLookalikes(t *testing.T) {
	for _, bad := range []string{
		"Zoë",        // NFC
		"Zoë",       // NFD of the same name
		"аdmin",      // Cyrillic a
		"admіn",      // Cyrillic i
		"ａdmin",      // fullwidth a
		"admin․name", // one dot leader
		"名前",
	} {
		assert.ErrorIs(t, admin.ValidateAdminUsername(bad), admin.ErrInvalidUsername, "%q", bad)
	}
}
