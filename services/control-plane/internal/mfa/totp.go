// Package mfa implements Multi-Factor Authentication (TOTP + WebAuthn) for Concord.
package mfa

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"strings"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/backuphash"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/argon2"
)

const (
	totpIssuer  = "ConcordVoice"
	totpDigits  = otp.DigitsSix
	totpPeriod  = 30
	totpAlgo    = otp.AlgorithmSHA1
	totpSkew    = 1 // Allow previous and next period for clock drift
	backupCount = 8
	backupLen   = 8
)

// backupAlphabet is [A-Z0-9] for human-readable backup codes.
const backupAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// GenerateSecret creates a new TOTP secret for the given email.
// Returns the otp.Key which contains the secret and otpauth:// URL.
func GenerateSecret(email string) (*otp.Key, error) {
	return totp.Generate(totp.GenerateOpts{
		Issuer:      totpIssuer,
		AccountName: email,
		Period:      totpPeriod,
		Digits:      totpDigits,
		Algorithm:   totpAlgo,
	})
}

// MatchCodeStep checks a TOTP code against a secret with a 1-step skew window
// and reports which 30-second time step (Unix time / totpPeriod) it matched.
// A match is not an acceptance: the caller must record the step through a
// guarded write that admits only a step later than the last one accepted
// (user_mfa_totp.last_used_step), which is what makes a code single-use
// (RFC 6238 §5.2).
func MatchCodeStep(secret, code string) (step int64, ok bool) {
	return matchCodeStepAt(secret, code, time.Now())
}

// matchCodeStepAt is MatchCodeStep at a given instant. pquerna/otp's
// totp.ValidateCustom returns only a bool, so the window is enumerated here.
// Every candidate is generated and compared in constant time whether or not an
// earlier one matched, and a code that matches two candidates (a six-digit
// collision inside the window) reports the later one, so the recorded step can
// only burn more of the window, never less. Each candidate is
// generated at the start of its own step, so the counter the library derives
// is exactly the step reported.
func matchCodeStepAt(secret, code string, now time.Time) (step int64, ok bool) {
	// hotp.ValidateCustom, which the previous matcher went through, trimmed
	// surrounding whitespace; keep accepting what it accepted.
	submitted := []byte(strings.TrimSpace(code))
	opts := totp.ValidateOpts{Period: totpPeriod, Digits: totpDigits, Algorithm: totpAlgo}
	current := now.Unix() / totpPeriod
	for candidate := current - totpSkew; candidate <= current+totpSkew; candidate++ {
		want, err := totp.GenerateCodeCustom(secret, time.Unix(candidate*totpPeriod, 0), opts)
		if err != nil {
			return 0, false
		}
		if subtle.ConstantTimeCompare([]byte(want), submitted) == 1 {
			step, ok = candidate, true
		}
	}
	return step, ok
}

// EncryptSecret encrypts a TOTP secret using AES-256-GCM.
// Returns ciphertext and nonce. The encKey must be exactly 32 bytes.
func EncryptSecret(plaintext, encKey []byte) (ciphertext, nonce []byte, err error) {
	// aes.NewCipher also accepts 16- and 24-byte keys, which would silently
	// downgrade to AES-128/192.
	if len(encKey) != 32 {
		return nil, nil, fmt.Errorf("key must be 32 bytes, got %d", len(encKey))
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, nil, fmt.Errorf("create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("create GCM: %w", err)
	}

	nonce = make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("generate nonce: %w", err)
	}

	ciphertext = gcm.Seal(nil, nonce, plaintext, nil)
	return ciphertext, nonce, nil
}

// DecryptSecret decrypts an AES-256-GCM encrypted TOTP secret.
func DecryptSecret(ciphertext, nonce, encKey []byte) ([]byte, error) {
	if len(encKey) != 32 {
		return nil, fmt.Errorf("key must be 32 bytes, got %d", len(encKey))
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}

	// gcm.Open panics, rather than erroring, on a wrong-length nonce, and the
	// nonce comes from a stored row, so corrupt or hand-written data must fail here.
	if len(nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("decrypt: invalid nonce length %d, want %d", len(nonce), gcm.NonceSize())
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}

	return plaintext, nil
}

// ParseEncryptionKey decodes a hex-encoded 32-byte AES key.
func ParseEncryptionKey(hexKey string) ([]byte, error) {
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("decode hex key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("key must be 32 bytes, got %d", len(key))
	}
	return key, nil
}

// GenerateBackupCodes creates 8 random backup codes.
// Returns the plaintext codes (to show the user once) and legacy SHA-256 hashes.
// The writer changes only after every serving replica can read both formats.
func GenerateBackupCodes() (codes []string, hashes []string, err error) {
	codes = make([]string, backupCount)
	hashes = make([]string, backupCount)
	alphabetLen := big.NewInt(int64(len(backupAlphabet)))

	for i := range backupCount {
		var sb strings.Builder
		for range backupLen {
			idx, randErr := rand.Int(rand.Reader, alphabetLen)
			if randErr != nil {
				return nil, nil, fmt.Errorf("generate random: %w", randErr)
			}
			sb.WriteByte(backupAlphabet[idx.Int64()])
		}
		codes[i] = sb.String()
		hash := sha256.Sum256([]byte(codes[i]))
		hashes[i] = hex.EncodeToString(hash[:])
	}

	return codes, hashes, nil
}

// VerifyBackupCode checks a code against stored hashes using timing-safe comparison.
// Returns the index of the matched code, whether it matched, and a storage error.
// The caller marks a match used through its conditional database update.
func VerifyBackupCode(code string, hashes []string, used []bool) (index int, matched bool, err error) {
	type parsedHash struct{ salt, digest []byte }
	parsed := make([]parsedHash, len(hashes))
	for i, stored := range hashes {
		parsed[i].salt, parsed[i].digest, err = backuphash.Parse(stored)
		if err != nil {
			return -1, false, err
		}
	}
	normalized := strings.TrimSpace(code)
	if len(normalized) != backupLen {
		return -1, false, nil
	}
	normalized = strings.ToUpper(normalized)
	codeHash := sha256.Sum256([]byte(normalized))

	for i := 0; i < len(parsed) && i < len(used); i++ {
		// A code with no used flag is treated as used: the arrays are written
		// together, so a short one is a damaged row, and the caller indexes
		// used[i] to mark the match.
		if used[i] {
			continue
		}
		stored := parsed[i]
		candidate := codeHash[:]
		if stored.salt != nil {
			candidate = argon2.IDKey([]byte(normalized), stored.salt, 1, 8192, 1, 32)
		}
		if subtle.ConstantTimeCompare(candidate, stored.digest) == 1 {
			return i, true, nil
		}
	}
	return -1, false, nil
}
