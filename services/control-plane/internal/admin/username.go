package admin

import (
	"errors"
	"strings"
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
// lowercase ASCII letter (a-z), an ASCII digit, '.', '_' or '-'.
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
	for i := 0; i < len(username); i++ {
		if !isAdminUsernameByte(username[i]) {
			return ErrInvalidUsername
		}
	}
	return nil
}

func isAdminUsernameByte(b byte) bool {
	switch {
	case 'a' <= b && b <= 'z', '0' <= b && b <= '9':
		return true
	case b == '.', b == '_', b == '-':
		return true
	}
	return false
}

// skeletonFolds maps each character that reads like another to one
// representative: 1, i, l and | to l; 0 and o to o; '.', '_' and '-' to one
// separator.
var skeletonFolds = strings.NewReplacer(
	"1", "l", "i", "l", "|", "l",
	"0", "o",
	".", "-", "_", "-",
)

// skeletonPairs folds the letter pairs that read as one letter: rn as m, vv as
// w, cl as d. It runs after skeletonFolds, so c1 and ci fold to d as well.
var skeletonPairs = strings.NewReplacer("rn", "m", "vv", "w", "cl", "d")

// UsernameSkeleton is the confusable skeleton of an admin username: the
// lowercased name with skeletonFolds and then skeletonPairs applied. Two names
// with one skeleton can be misread as each other, so a new admin username is
// refused when its skeleton equals an existing admin's (alice, Alice, aIice
// and a1ice share one; so do ops0 and opso, corn and com, first.last and
// first_last). It is computed for legacy names too, whatever they hold.
func UsernameSkeleton(username string) string {
	return skeletonPairs.Replace(skeletonFolds.Replace(strings.ToLower(username)))
}
