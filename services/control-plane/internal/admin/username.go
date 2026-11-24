package admin

import "errors"

// ErrInvalidUsername marks an admin username that ValidateAdminUsername
// refuses.
var ErrInvalidUsername = errors.New("invalid username")

// MaxAdminUsernameLength bounds a new admin username, in bytes (each allowed
// character is one byte).
const MaxAdminUsernameLength = 64

// ValidateAdminUsername is the one rule for a new admin username, applied by
// CreateAdmin, adminctl bootstrap and the repository insert that both reach.
// A username is 1 to MaxAdminUsernameLength characters, each an ASCII letter,
// an ASCII digit, '.', '_' or '-'.
//
// The rule is an allowlist so that two usernames that look the same are the
// same bytes. Sign-in looks a username up by exact match, so a name that
// differs only in characters nobody can see (a zero-width, variation-selector
// or other default-ignorable character, a Hangul filler, a non-ASCII space,
// or another Unicode normalization form of the same text) or in a lookalike
// letter from another script would be a second admin that reads as the first
// in the audit log. The allowlist also keeps control characters out, which
// the audit writer drops from the actor (sanitizeAuditString).
//
// It gates new names only. Sign-in, reset-enrollment and unlock look an
// existing admin up by its stored name without this check, so an admin
// created before the rule keeps signing in.
func ValidateAdminUsername(username string) error {
	if username == "" || len(username) > MaxAdminUsernameLength {
		return ErrInvalidUsername
	}
	for i := 0; i < len(username); i++ {
		if !isAdminUsernameByte(username[i]) {
			return ErrInvalidUsername
		}
	}
	return nil
}

func isAdminUsernameByte(b byte) bool {
	switch {
	case 'a' <= b && b <= 'z', 'A' <= b && b <= 'Z', '0' <= b && b <= '9':
		return true
	case b == '.', b == '_', b == '-':
		return true
	}
	return false
}
