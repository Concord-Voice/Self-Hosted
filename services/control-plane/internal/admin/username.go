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

// ValidateAdminUsername is the one rule for a new admin username, applied by
// CreateAdmin, adminctl bootstrap and the repository insert that both reach.
// A username must be valid UTF-8, non-empty, made only of printable characters
// (unicode.IsPrint: letters, marks, numbers, punctuation, symbols and the
// ASCII space, so no control, format, bidi-override, zero-width or non-ASCII
// space character) and have no leading or trailing space.
//
// Sign-in looks a username up by exact match, so a name that differs only in
// characters nobody can see is a second admin that looks like the first. The
// audit writer also drops control characters from the actor
// (sanitizeAuditString), so such a name is recorded under a different name, or
// none.
func ValidateAdminUsername(username string) error {
	if username == "" || !utf8.ValidString(username) || strings.TrimSpace(username) != username {
		return ErrInvalidUsername
	}
	for _, r := range username {
		if !unicode.IsPrint(r) {
			return ErrInvalidUsername
		}
	}
	return nil
}
