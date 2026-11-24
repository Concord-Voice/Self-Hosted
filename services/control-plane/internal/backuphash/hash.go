// Package backuphash validates the stored backup-code formats shared by MFA
// verification and factor-status readers.
package backuphash

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

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
// It shares DeriveV2's slots and returns ErrBusy when none frees in time.
func NewV2(ctx context.Context, code string) (string, error) {
	return newV2(ctx, code, rand.Reader)
}

func newV2(ctx context.Context, code string, random io.Reader) (string, error) {
	salt := make([]byte, v2SaltLen)
	if _, err := io.ReadFull(random, salt); err != nil {
		return "", fmt.Errorf("read backup-code salt: %w", err)
	}
	digest, err := DeriveV2(ctx, code, salt)
	if err != nil {
		return "", err
	}
	return V2Prefix + base64.RawURLEncoding.EncodeToString(salt) + "$" +
		base64.RawURLEncoding.EncodeToString(digest), nil
}

// MaxConcurrentDerivations caps Argon2id backup-code derivations across the
// process, writer and verifier alike. Each derivation holds 19 MiB, so four
// bound the peak near 76 MiB however many setup, regenerate or verify requests
// arrive at once; one TOTP setup's eight derivations still finish in well under
// a second.
const MaxConcurrentDerivations = 4

// maxDerivationWait bounds how long a caller whose context has no earlier
// deadline queues for a slot, so a flood is answered rather than parked.
const maxDerivationWait = 5 * time.Second

// ErrBusy reports that no derivation slot freed before the caller's context
// ended or maxDerivationWait passed. HTTP callers answer it with 503.
var ErrBusy = errors.New("backup-code hashing is busy")

var derivationSlots = make(chan struct{}, MaxConcurrentDerivations)

// deriveKey is the Argon2id call; tests swap it to observe the semaphore.
var deriveKey = func(code string, salt []byte) []byte {
	return argon2.IDKey([]byte(code), salt, v2Time, v2MemoryKiB, v2Threads, v2DigestLen)
}

// DeriveV2 computes the v2 digest of a normalized code under a parsed salt. It
// waits for one of MaxConcurrentDerivations slots and returns ErrBusy, without
// deriving, if the context ends first.
func DeriveV2(ctx context.Context, code string, salt []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBusy, err)
	}
	ctx, cancel := context.WithTimeout(ctx, maxDerivationWait)
	defer cancel()
	select {
	case derivationSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w", ErrBusy, ctx.Err())
	}
	defer func() { <-derivationSlots }()
	return deriveKey(code, salt), nil
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
