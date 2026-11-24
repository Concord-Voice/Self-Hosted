package admin

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrInvalidUsername marks an admin username that ValidateAdminUsername
// refuses.
var ErrInvalidUsername = errors.New("invalid username")

// MaxAdminUsernameLength is the longest admin username, in characters.
const MaxAdminUsernameLength = 64

// ValidateAdminUsername is the one rule for a new admin username, applied by
// CreateAdmin, adminctl bootstrap and the repository insert that both reach.
// A username must be valid UTF-8, non-empty, made only of printable characters
// (unicode.IsPrint: letters, marks, numbers, punctuation, symbols and the
// ASCII space, so no control, format, bidi-override, zero-width or non-ASCII
// space character), have no leading or trailing space and be at most
// MaxAdminUsernameLength characters, so a refused attempt cannot store an
// oversized actor in the audit log.
//
// Sign-in looks a username up by exact match, so a name that differs only in
// characters nobody can see is a second admin that looks like the first. The
// audit writer also drops control characters from the actor
// (sanitizeAuditString), so such a name is recorded under a different name, or
// none.
func ValidateAdminUsername(username string) error {
	if username == "" || !utf8.ValidString(username) || strings.TrimSpace(username) != username ||
		utf8.RuneCountInString(username) > MaxAdminUsernameLength {
		return ErrInvalidUsername
	}
	for _, r := range username {
		if !unicode.IsPrint(r) {
			return ErrInvalidUsername
		}
	}
	return nil
}
