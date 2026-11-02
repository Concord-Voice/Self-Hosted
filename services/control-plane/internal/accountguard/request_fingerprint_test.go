package accountguard

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
	"github.com/stretchr/testify/require"
)

func TestRequestOriginExactBinaryTranscript(t *testing.T) {
	// Literal count/kind/TEXT length/version pin byte order and ID boundaries.
	suffix, err := hex.DecodeString("000000010600000001610000000000000007")
	require.NoError(t, err)
	transcript := append([]byte("concord/accountguard/origin/v1\x00"), suffix...)
	want := sha256.Sum256(transcript)
	got, err := originFingerprint([]decide.CredRef{{Kind: decide.CredTOTP, ID: "a", Version: 7}})
	require.NoError(t, err)
	require.Equal(t, want, got)
	for _, refs := range [][]decide.CredRef{
		{{Kind: decide.CredUnknown, ID: "a", Version: 1}}, {{Kind: decide.CredTOTP, ID: "", Version: 1}},
		{{Kind: decide.CredTOTP, ID: "a", Version: 0}}, {{Kind: decide.CredTOTP, ID: "a", Version: -1}},
		{{Kind: decide.CredTOTP, ID: "b", Version: 1}, {Kind: decide.CredTOTP, ID: "a", Version: 1}},
		{{Kind: decide.CredTOTP, ID: "a", Version: 1}, {Kind: decide.CredTOTP, ID: "a", Version: 1}},
		{{Kind: decide.CredTOTP, ID: string([]byte{0xff}), Version: 1}},
	} {
		_, err = originFingerprint(refs)
		require.Error(t, err)
	}
	changed, err := originFingerprint([]decide.CredRef{{Kind: decide.CredTOTP, ID: "a", Version: 8}})
	require.NoError(t, err)
	require.NotEqual(t, want, changed)
}
func TestRequestFingerprintExactTranscriptAndAddressSeparation(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	// Address change=2, one backup-address=3, TEXT(a), version7, reset=false.
	core, err := hex.DecodeString("0200000001030000000161000000000000000700")
	require.NoError(t, err)
	address, err := hex.DecodeString("00000003614062") // TEXT(a@b)
	require.NoError(t, err)
	coarse := sha256.Sum256(append([]byte("concord/accountguard/request-coarse/v1\x00"), core...))
	mac := hmac.New(sha256.New, key)
	_, err = mac.Write(append(append([]byte("concord/accountguard/request-keyed/v1\x00"), core...), address...))
	require.NoError(t, err)
	input := requestFingerprintInput{kind: decide.ReqAddressChange, targets: []decide.CredRef{{Kind: decide.CredBackupAddress, ID: "a", Version: 7}}, pendingAddress: "a@b"}
	got, err := requestFingerprints(input, key, 7)
	require.NoError(t, err)
	require.Equal(t, coarse[:], got.Coarse)
	require.Equal(t, mac.Sum(nil), got.Keyed)
	require.Equal(t, int16(7), got.KeyID)
	otherGeneration, err := requestFingerprints(input, key, 8)
	require.NoError(t, err)
	require.Equal(t, got.Keyed, otherGeneration.Keyed)
	require.Equal(t, got.Coarse, otherGeneration.Coarse)
	input.pendingAddress = "another@b"
	otherAddress, err := requestFingerprints(input, key, 7)
	require.NoError(t, err)
	require.Equal(t, got.Coarse, otherAddress.Coarse)
	require.NotEqual(t, got.Keyed, otherAddress.Keyed)
	got.Keyed[0] ^= 0xff
	got.Coarse[0] ^= 0xff
	require.Equal(t, coarse[:], otherGeneration.Coarse)
	require.Equal(t, mac.Sum(nil), otherGeneration.Keyed)
	for _, bad := range []requestFingerprintInput{
		{}, {kind: decide.ReqRecoveryKey}, {kind: decide.ReqBreakGlass, pendingAddress: "a@b"},
		{kind: decide.ReqCredentialRemove, targets: []decide.CredRef{{Kind: decide.CredPassword, ID: "a", Version: 1}}},
		{kind: decide.ReqAddressChange, targets: input.targets, pendingAddress: " A@b "},
		{kind: decide.ReqAddressChange, targets: input.targets, pendingAddress: ""},
		{kind: decide.ReqAddressChange, targets: input.targets, pendingAddress: "a@b", resetFactors: true},
	} {
		_, err = requestFingerprints(bad, key, 7)
		require.Error(t, err)
	}
	for _, length := range []int{0, 1, 31, 33} {
		_, err = requestFingerprints(input, make([]byte, length), 7)
		require.Error(t, err)
	}
	for _, id := range []int16{0, -1} {
		_, err = requestFingerprints(input, key, id)
		require.Error(t, err)
	}
	plain, err := requestFingerprints(requestFingerprintInput{kind: decide.ReqBreakGlass}, key, 7)
	require.NoError(t, err)
	reset, err := requestFingerprints(requestFingerprintInput{kind: decide.ReqBreakGlass, resetFactors: true}, key, 7)
	require.NoError(t, err)
	require.NotEqual(t, plain.Keyed, reset.Keyed)
	require.NotEqual(t, plain.Coarse, reset.Coarse)
	a, err := originFingerprint([]decide.CredRef{{Kind: decide.CredTOTP, ID: "a", Version: 1}, {Kind: decide.CredTOTP, ID: "bc", Version: 1}})
	require.NoError(t, err)
	b, err := originFingerprint([]decide.CredRef{{Kind: decide.CredTOTP, ID: "ab", Version: 1}, {Kind: decide.CredTOTP, ID: "c", Version: 1}})
	require.NoError(t, err)
	require.NotEqual(t, a, b)
}
