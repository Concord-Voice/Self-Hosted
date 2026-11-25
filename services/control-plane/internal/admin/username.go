package admin

import (
	"errors"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// ErrInvalidUsername marks an admin username that ValidateAdminUsername
// refuses.
var ErrInvalidUsername = errors.New("invalid username")

// ErrConfusableUsername marks a new admin username whose confusable skeleton
// (UsernameSkeleton) equals that of an existing admin.
var ErrConfusableUsername = errors.New("username looks like an existing admin username")

// MaxAdminUsernameLength bounds a new admin username, in bytes (each allowed
// character is one byte).
const MaxAdminUsernameLength = 64

// ValidateAdminUsername is the character rule for a new admin username,
// applied by CreateAdmin, adminctl bootstrap and the repository insert that
// both reach. A username is 1 to MaxAdminUsernameLength characters, each a
// lowercase ASCII letter (a-z), an ASCII digit, '.', '_' or '-', and its first
// and last characters are a letter or a digit. So no new name begins with '-'
// and reads as an option to adminctl, and none ends in a separator that the
// confusable skeleton would trim.
//
// The rule is an allowlist, so a new name holds no character that is
// invisible, default-ignorable, a control character, another Unicode
// normalization form of the same text, a letter from another script, or an
// uppercase letter. It does not by itself stop two names that look alike in
// ASCII (alice and a1ice): the repository insert also refuses a new name
// whose UsernameSkeleton matches an existing admin's.
//
// It gates new names only. Sign-in, reset-enrollment and unlock look an
// existing admin up by its stored name without this check, so an admin
// created before the rule, whatever its characters or case, keeps signing in.
func ValidateAdminUsername(username string) error {
	if username == "" || len(username) > MaxAdminUsernameLength {
		return ErrInvalidUsername
	}
	if !isAdminUsernameEdgeByte(username[0]) || !isAdminUsernameEdgeByte(username[len(username)-1]) {
		return ErrInvalidUsername
	}
	for i := 0; i < len(username); i++ {
		if !isAdminUsernameByte(username[i]) {
			return ErrInvalidUsername
		}
	}
	return nil
}

// isAdminUsernameEdgeByte reports whether b may begin or end a new admin
// username: a lowercase ASCII letter or an ASCII digit.
func isAdminUsernameEdgeByte(b byte) bool {
	return 'a' <= b && b <= 'z' || '0' <= b && b <= '9'
}

func isAdminUsernameByte(b byte) bool {
	switch {
	case isAdminUsernameEdgeByte(b):
		return true
	case b == '.', b == '_', b == '-':
		return true
	}
	return false
}

// skeletonSeparator is the one separator every '.', '_', '-', dash and
// whitespace character folds to.
const skeletonSeparator = '-'

// skeletonHomoglyphs maps the letters drawn like a Latin letter to that
// letter, and the symbols drawn like a separator to the separator. It holds
// both cases, because it runs before NFKD normalization and before and after
// case folding: Greek capital nu reads as N while its lowercase reads as v,
// Cyrillic capital ve reads as B while its lowercase does not read as b, NFKD
// turns the Greek lunate sigma symbol (U+03F2, drawn like c) into a final
// sigma, and case folding turns its capital (U+03F9) into it.
//
// ponytail: a small curated table of the visually identical letters. The
// upgrade path is the Unicode TR39 confusables data (skeleton()), which also
// covers other scripts and symbols.
var skeletonHomoglyphs = map[rune]rune{
	// Cyrillic lowercase: a ie ka o er es u ha, straight u,
	// byelorussian-ukrainian i, je, dze, komi de, shha, palochka, qa, we.
	'\u0430': 'a', '\u0435': 'e', '\u043a': 'k', '\u043e': 'o', '\u0440': 'p', '\u0441': 'c', '\u0443': 'y',
	'\u0445': 'x', '\u04af': 'y', '\u0456': 'i', '\u0458': 'j', '\u0455': 's', '\u0501': 'd', '\u04bb': 'h',
	'\u04cf': 'l', '\u051b': 'q', '\u051d': 'w',
	// Cyrillic uppercase: a ve ie ka em en o er es te u ha,
	// byelorussian-ukrainian i, je, dze, palochka, qa, we.
	'\u0410': 'a', '\u0412': 'b', '\u0415': 'e', '\u041a': 'k', '\u041c': 'm', '\u041d': 'h', '\u041e': 'o',
	'\u0420': 'p', '\u0421': 'c', '\u0422': 't', '\u0423': 'y', '\u0425': 'x', '\u0406': 'i', '\u0408': 'j',
	'\u0405': 's', '\u04c0': 'l', '\u051a': 'q', '\u051c': 'w',
	// Greek lowercase: omicron alpha nu tau iota kappa rho upsilon, lunate
	// sigma symbol.
	'\u03bf': 'o', '\u03b1': 'a', '\u03bd': 'v', '\u03c4': 't', '\u03b9': 'i', '\u03ba': 'k', '\u03c1': 'p',
	'\u03c5': 'u', '\u03f2': 'c',
	// Greek uppercase: alpha beta epsilon zeta eta iota kappa mu nu omicron
	// rho tau upsilon chi, lunate sigma symbol.
	'\u0391': 'a', '\u0392': 'b', '\u0395': 'e', '\u0396': 'z', '\u0397': 'h', '\u0399': 'i', '\u039a': 'k',
	'\u039c': 'm', '\u039d': 'n', '\u039f': 'o', '\u03a1': 'p', '\u03a4': 't', '\u03a5': 'y', '\u03a7': 'x',
	'\u03f9': 'c',
	// Latin: dotless i, alpha, script g, dental click.
	'\u0131': 'i', '\u0251': 'a', '\u0261': 'g', '\u01c0': 'l',
	// Symbols drawn like a separator: minus sign, middle dot.
	'\u2212': skeletonSeparator, '\u00b7': skeletonSeparator,
}

// skeletonRune is one step of UsernameSkeleton: it drops a combining mark
// (Mn), a format or control character (Cf, Cc) and a default-ignorable code
// point, maps whitespace and dash punctuation to the separator, and maps a
// homoglyph to its Latin letter.
func skeletonRune(r rune) rune {
	switch {
	case unicode.IsSpace(r), unicode.In(r, unicode.Zs, unicode.Pd):
		return skeletonSeparator
	case unicode.In(r, unicode.Mn, unicode.Cf, unicode.Cc,
		unicode.Other_Default_Ignorable_Code_Point, unicode.Variation_Selector):
		return -1
	}
	if latin, ok := skeletonHomoglyphs[r]; ok {
		return latin
	}
	return r
}

// skeletonFolds maps each character that reads like another to one
// representative: 1, i, l and | to l; 0 and o to o; '.' and '_' to the
// separator.
var skeletonFolds = strings.NewReplacer(
	"1", "l", "i", "l", "|", "l",
	"0", "o",
	".", "-", "_", "-",
)

// skeletonPairs expands each letter that reads as a pair into that pair: m to
// rn, w to vv, d to cl. Expanding, not shortening, keeps the rule confluent:
// no output holds an m, w or d, so wvhank and vwhank both reach vvvhank. It
// runs after skeletonFolds, so c1 and ci read as cl too.
var skeletonPairs = strings.NewReplacer("m", "rn", "w", "vv", "d", "cl")

// UsernameSkeleton is the confusable skeleton of an admin username. Two names
// with one skeleton can be misread as each other, so a new admin username is
// refused when its skeleton equals an existing admin's. It is computed for
// every existing name, including a legacy one the current rule would refuse,
// in these steps:
//
//  1. NFKD normalization and Unicode case folding, so Zoë and zoe, José and
//     jose, Alice and alice, and a fullwidth letter and its ASCII form match.
//  2. Combining marks, format and control characters and default-ignorable
//     code points are dropped, so bob followed by a zero-width space is bob.
//  3. Whitespace, dash punctuation, '.', '_', the minus sign and the middle
//     dot become one separator, runs of separators collapse to one and
//     separators at either end are trimmed, so Ops Team, ops-team and
//     ops__team match, and so do dave and dave.
//  4. The letters of skeletonHomoglyphs (Cyrillic, Greek and Latin letters
//     drawn like another Latin letter) become that letter, so alice spelled
//     with a Cyrillic a (U+0430) or a dotless i (U+0131) reads as alice. The
//     table is applied before NFKD as well as after it, because NFKD turns
//     some of its letters into letters that look different.
//  5. 1, i, l and | fold to l and 0 and o to o (alice, aIice and a1ice match;
//     so do ops0 and opso).
//  6. m expands to rn, w to vv and d to cl (corn and com match, and so do kim
//     and klm).
//
// Lookalikes outside these steps, such as letters of other scripts or
// letters missing from the table, are not detected.
func UsernameSkeleton(username string) string {
	s := strings.Map(skeletonRune, username)
	s = strings.Map(skeletonRune, norm.NFKD.String(s))
	s = strings.Map(skeletonRune, norm.NFKD.String(cases.Fold().String(s)))
	s = collapseSeparators(skeletonFolds.Replace(s))
	return skeletonPairs.Replace(s)
}

// collapseSeparators collapses each run of separators to one and trims them
// at both ends.
func collapseSeparators(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prev := rune(skeletonSeparator)
	for _, r := range s {
		if r == skeletonSeparator && prev == skeletonSeparator {
			continue
		}
		b.WriteRune(r)
		prev = r
	}
	return strings.TrimSuffix(b.String(), string(skeletonSeparator))
}
