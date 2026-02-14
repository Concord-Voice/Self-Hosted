package auth

import (
	"fmt"
	"regexp"
	"strings"
)

// Username validation constants
const (
	MinUsernameLength = 3
	MaxUsernameLength = 32
)

// Username validation regex
// Allows: a-z, A-Z, 0-9, underscore, hyphen
// Must start with alphanumeric, no consecutive special chars
var (
	usernameRegex     = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*[a-zA-Z0-9]$|^[a-zA-Z0-9]$`)
	consecutiveSpecial = regexp.MustCompile(`[_-]{2,}`)
)

// Common offensive/reserved words (basic list - expand as needed)
// These are case-insensitive and checked for exact matches and variations
var blockedWords = []string{
	// System reserved
	"admin", "administrator", "root", "system", "moderator", "mod",
	"support", "help", "concord", "concordvoice", "official",
	"api", "www", "mail", "smtp", "ftp", "noreply",

	// Profanity (basic list - use a proper library for production)
	"fuck", "shit", "damn", "bitch", "ass", "bastard",
	"asshole", "dick", "pussy", "cock", "cunt",

	// Slurs and hate speech (basic - expand significantly)
	"nigger", "nigga", "faggot", "retard", "tranny",

	// Impersonation risks
	"staff", "employee", "team", "service", "bot",
}

// Leetspeak substitution map for detecting variations
var leetMap = map[rune]rune{
	'0': 'o',
	'1': 'i',
	'3': 'e',
	'4': 'a',
	'5': 's',
	'7': 't',
	'8': 'b',
	'@': 'a',
	'$': 's',
}

// ValidateUsername performs comprehensive username validation
func ValidateUsername(username string) error {
	// Length check
	if len(username) < MinUsernameLength {
		return fmt.Errorf("username must be at least %d characters", MinUsernameLength)
	}
	if len(username) > MaxUsernameLength {
		return fmt.Errorf("username must be at most %d characters", MaxUsernameLength)
	}

	// Format check (alphanumeric, _, -)
	if !usernameRegex.MatchString(username) {
		return fmt.Errorf("username must start and end with a letter or number, and can only contain letters, numbers, underscores, and hyphens")
	}

	// No consecutive special characters
	if consecutiveSpecial.MatchString(username) {
		return fmt.Errorf("username cannot contain consecutive underscores or hyphens")
	}

	// Check for blocked words
	if err := checkBlockedWords(username); err != nil {
		return err
	}

	return nil
}

// checkBlockedWords checks if username contains offensive or reserved words
func checkBlockedWords(username string) error {
	// Normalize username (lowercase)
	normalized := strings.ToLower(username)

	// Remove common separators to catch variations like "bad_word" or "bad-word"
	stripped := strings.ReplaceAll(strings.ReplaceAll(normalized, "_", ""), "-", "")

	// Convert leetspeak to normal characters
	deLeeted := deLeetSpeak(stripped)

	// Check exact match and contains
	for _, blocked := range blockedWords {
		blocked = strings.ToLower(blocked)

		// Check if normalized username matches or contains blocked word
		if normalized == blocked || stripped == blocked || deLeeted == blocked {
			return fmt.Errorf("username is not available")
		}

		// Check if it contains the blocked word (to catch things like "admin123")
		if strings.Contains(stripped, blocked) || strings.Contains(deLeeted, blocked) {
			return fmt.Errorf("username is not available")
		}
	}

	return nil
}

// deLeetSpeak converts leetspeak characters to their normal equivalents
func deLeetSpeak(s string) string {
	var result strings.Builder
	for _, r := range s {
		if replacement, ok := leetMap[r]; ok {
			result.WriteRune(replacement)
		} else {
			result.WriteRune(r)
		}
	}
	return result.String()
}

// NormalizeUsername converts username to lowercase for storage
// We store lowercase but preserve the user's preferred capitalization in display_name
func NormalizeUsername(username string) string {
	return strings.ToLower(username)
}

// FormatUsernameWithDomain formats username with the platform domain
func FormatUsernameWithDomain(username string) string {
	return fmt.Sprintf("%s@concordvoice.chat", NormalizeUsername(username))
}
