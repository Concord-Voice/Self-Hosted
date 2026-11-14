// Package backuphash validates the stored backup-code formats shared by MFA
// verification and factor-status readers.
package backuphash

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

// V2Prefix identifies the bounded Argon2id backup-code format.
const V2Prefix = "v2$argon2id$v=19$m=8192,t=1,p=1$"

var errInvalidStoredHash = errors.New("invalid stored backup hash")

// Parse accepts only the legacy bare SHA-256 digest, its v1-prefixed form,
// and the exact bounded Argon2id v2 form. A non-nil salt identifies v2.
func Parse(stored string) (salt, digest []byte, err error) {
	if len(stored) == 64 {
		digest, err = hex.DecodeString(stored)
		if err == nil && hex.EncodeToString(digest) == stored {
			return nil, digest, nil
		}
	}
	if strings.HasPrefix(stored, "v1$") && len(stored) == 67 {
		digest, err = hex.DecodeString(stored[3:])
		if err == nil && hex.EncodeToString(digest) == stored[3:] {
			return nil, digest, nil
		}
	}
	// Bound the encoded value before decoding or splitting untrusted storage.
	if len(stored) == len(V2Prefix)+22+1+43 && strings.HasPrefix(stored, V2Prefix) {
		parts := strings.Split(stored[len(V2Prefix):], "$")
		if len(parts) == 2 {
			salt, err = base64.RawURLEncoding.DecodeString(parts[0])
			if err == nil && len(salt) == 16 && base64.RawURLEncoding.EncodeToString(salt) == parts[0] {
				digest, err = base64.RawURLEncoding.DecodeString(parts[1])
				if err == nil && len(digest) == 32 && base64.RawURLEncoding.EncodeToString(digest) == parts[1] {
					return salt, digest, nil
				}
			}
		}
	}
	return nil, nil, errInvalidStoredHash
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
