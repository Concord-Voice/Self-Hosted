package mfa

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"runtime"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/backuphash"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/argon2"
)

func TestVerifyBackupCodeReadsBareV1AndV2(t *testing.T) {
	code := "ABCD1234"
	sha := sha256.Sum256([]byte(code))
	bare := hex.EncodeToString(sha[:])
	salt := []byte("0123456789abcdef")
	v2 := backuphash.V2Prefix + base64.RawURLEncoding.EncodeToString(salt) + "$" +
		base64.RawURLEncoding.EncodeToString(argon2.IDKey([]byte(code), salt, 2, 19456, 1, 32))

	for _, stored := range []string{bare, "v1$" + bare, v2} {
		idx, matched, err := VerifyBackupCode("  abcd1234  ", []string{stored}, []bool{false})
		require.NoError(t, err)
		require.True(t, matched, stored)
		require.Zero(t, idx)
		_, matched, err = VerifyBackupCode(code, []string{stored}, []bool{true})
		require.NoError(t, err)
		require.False(t, matched, "a used code must not match")
	}

	idx, matched, err := VerifyBackupCode(code, []string{"v1$" + bare, v2}, []bool{true, false})
	require.NoError(t, err)
	require.True(t, matched, "a v2 entry remains usable after a v1 entry is consumed")
	require.Equal(t, 1, idx)
}

func TestVerifyBackupCodeRejectsMalformedStoredHashesBeforeMatching(t *testing.T) {
	code := "ABCD1234"
	sha := sha256.Sum256([]byte(code))
	bare := hex.EncodeToString(sha[:])
	salt := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef"))
	digest := base64.RawURLEncoding.EncodeToString(make([]byte, 32))

	for name, bad := range map[string]string{
		"uppercase bare":   strings.ToUpper(bare),
		"uppercase v1":     "v1$" + strings.ToUpper(bare),
		"short v1":         "v1$xyz",
		"large parameters": "v2$argon2id$v=19$m=65536,t=3,p=4$" + salt + "$" + digest,
		"wrong version":    "v2$argon2id$v=20$m=19456,t=2,p=1$" + salt + "$" + digest,
		"short salt":       backuphash.V2Prefix + "AA$" + digest,
		"short digest":     backuphash.V2Prefix + salt + "$AA",
		"oversize digest":  backuphash.V2Prefix + salt + "$" + digest + "A",
		"unknown version":  "v3$unknown",
	} {
		t.Run(name, func(t *testing.T) {
			_, matched, err := VerifyBackupCode(code, []string{bare, bad}, []bool{false, false})
			require.Error(t, err, "all entries must validate before an earlier entry matches")
			require.False(t, matched)
		})
	}
}

// A row can hold every format at once during the B1/B2 overlap: bare codes a
// B1 replica generated, v1 codes the 000201 migration prefixed, and v2 codes a
// B2 replica generated. The verifier is the B1 reader (only its Argon2id call
// moved into backuphash.DeriveV2), so this matrix is also what a B1 binary
// answers after an application rollback from B2.
func TestVerifyBackupCodeMixedB1AndB2Row(t *testing.T) {
	b2Codes, b2Hashes, err := GenerateBackupCodes()
	require.NoError(t, err)
	legacy := func(code string) string {
		sum := sha256.Sum256([]byte(code))
		return hex.EncodeToString(sum[:])
	}
	codes := []string{"BARE0001", "PREFIX01", b2Codes[0]}
	hashes := []string{legacy(codes[0]), "v1$" + legacy(codes[1]), b2Hashes[0]}
	used := []bool{false, false, false}

	for want, code := range codes {
		idx, matched, err := VerifyBackupCode(strings.ToLower(code), hashes, used)
		require.NoError(t, err)
		require.True(t, matched, "format %d must verify", want)
		require.Equal(t, want, idx)
		used[idx] = true
		_, matched, err = VerifyBackupCode(code, hashes, used)
		require.NoError(t, err)
		require.False(t, matched, "format %d must verify only once", want)
	}

	_, matched, err := VerifyBackupCode(b2Codes[1], []string{b2Hashes[0]}, []bool{false})
	require.NoError(t, err)
	require.False(t, matched, "a v2 hash must not accept another code")
}

// Parse refuses every stored entry before any Argon2id runs, so a hostile
// parameter string costs no memory even when a valid v2 entry precedes it. A
// verifier that derived the valid entry first would allocate its 19 MiB.
func TestVerifyBackupCodeRefusesHostileParametersWithoutArgon2(t *testing.T) {
	codes, hashes, err := GenerateBackupCodes()
	require.NoError(t, err)
	hostile := "v2$argon2id$v=19$m=4194304,t=64,p=255$" +
		base64.RawURLEncoding.EncodeToString(make([]byte, 16)) + "$" +
		base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	stored := []string{hashes[0], hostile}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, matched, err := VerifyBackupCode(codes[1], stored, []bool{false, false})
	runtime.ReadMemStats(&after)

	require.Error(t, err)
	require.False(t, matched)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(1<<20),
		"a malformed entry must be refused before any Argon2id allocation")

	runtime.ReadMemStats(&before)
	_, matched, err = VerifyBackupCode(codes[1], stored[:1], []bool{false})
	runtime.ReadMemStats(&after)
	require.NoError(t, err)
	require.False(t, matched)
	require.GreaterOrEqual(t, after.TotalAlloc-before.TotalAlloc, uint64(19456<<10),
		"control: a well-formed v2 entry does run the bounded Argon2id")
}
