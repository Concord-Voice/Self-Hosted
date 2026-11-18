// Package backuphash validates the stored backup-code formats shared by MFA
// verification and factor-status readers.
package backuphash

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/argon2"
)

// V2Prefix identifies the bounded Argon2id backup-code format.
const V2Prefix = "v2$argon2id$v=19$m=19456,t=2,p=1$"

// The v2 tuple. V2Prefix spells the same values, and Parse accepts no other,
// so the writer and the verifier cannot drift apart.
const (
	v2Time      = 2
	v2MemoryKiB = 19456
	v2Threads   = 1
	v2SaltLen   = 16
	v2DigestLen = 32
)

var errInvalidStoredHash = errors.New("invalid stored backup hash")

// Parse accepts only the legacy bare SHA-256 digest, its v1-prefixed form,
// and the exact bounded Argon2id v2 form. A non-nil salt identifies v2.
func Parse(stored string) (salt, digest []byte, err error) {
	if len(stored) == 64 {
		return parseSHA256(stored)
	}
	if strings.HasPrefix(stored, "v1$") && len(stored) == 67 {
		return parseSHA256(stored[3:])
	}
	// Bound the encoded value before decoding or splitting untrusted storage.
	if len(stored) != len(V2Prefix)+22+1+43 || !strings.HasPrefix(stored, V2Prefix) {
		return nil, nil, errInvalidStoredHash
	}
	parts := strings.Split(stored[len(V2Prefix):], "$")
	if len(parts) != 2 {
		return nil, nil, errInvalidStoredHash
	}
	salt, err = base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(salt) != v2SaltLen || base64.RawURLEncoding.EncodeToString(salt) != parts[0] {
		return nil, nil, errInvalidStoredHash
	}
	digest, err = base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(digest) != v2DigestLen || base64.RawURLEncoding.EncodeToString(digest) != parts[1] {
		return nil, nil, errInvalidStoredHash
	}
	return salt, digest, nil
}

// NewV2 hashes one normalized backup code with a fresh random 16-byte salt.
func NewV2(code string) (string, error) {
	return newV2(code, rand.Reader)
}

func newV2(code string, random io.Reader) (string, error) {
	salt := make([]byte, v2SaltLen)
	if _, err := io.ReadFull(random, salt); err != nil {
		return "", fmt.Errorf("read backup-code salt: %w", err)
	}
	return V2Prefix + base64.RawURLEncoding.EncodeToString(salt) + "$" +
		base64.RawURLEncoding.EncodeToString(DeriveV2(code, salt)), nil
}

// DeriveV2 computes the v2 digest of a normalized code under a parsed salt.
func DeriveV2(code string, salt []byte) []byte {
	return argon2.IDKey([]byte(code), salt, v2Time, v2MemoryKiB, v2Threads, v2DigestLen)
}

func parseSHA256(encoded string) ([]byte, []byte, error) {
	digest, err := hex.DecodeString(encoded)
	if err != nil || hex.EncodeToString(digest) != encoded {
		return nil, nil, errInvalidStoredHash
	}
	return nil, digest, nil
}

// CountAvailable validates every stored entry before counting unspent codes.
// A hash without a corresponding used flag is unavailable to the verifier.
func CountAvailable(hashes []string, used []bool) (int, error) {
	for _, stored := range hashes {
		if _, _, err := Parse(stored); err != nil {
			return 0, err
		}
	}
	count := 0
	for i := range hashes {
		if i < len(used) && !used[i] {
			count++
		}
	}
	return count, nil
}
