package mfa

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
