package channels

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// VULN-080 / VULN-207: the pending-keys cursor codec.

func newTestPendingCursorCodec(t *testing.T) pendingCursorCodec {
	t.Helper()
	codec, err := newPendingCursorCodec()
	require.NoError(t, err)
	return codec
}

func TestPendingCursor_RoundTrip(t *testing.T) {
	codec := newTestPendingCursorCodec(t)
	// Postgres keeps microseconds, and a non-UTC offset must survive too.
	at := time.Date(2026, 11, 18, 9, 30, 15, 123456000, time.FixedZone("x", -5*3600))
	for _, source := range []string{pendingSourceChannel, pendingSourceDM} {
		in := pendingCursor{CreatedAt: at, Source: source, ID: uuid.NewString()}
		sealed, err := codec.seal(in)
		require.NoError(t, err)
		assert.LessOrEqual(t, len(sealed), pendingCursorMaxLen)
		assert.NotContains(t, sealed, "=", "the cursor is unpadded base64url")
		again, err := codec.seal(in)
		require.NoError(t, err)
		assert.NotEqual(t, sealed, again, "each seal draws a fresh nonce")

		out, err := codec.open(sealed)
		require.NoError(t, err)
		assert.True(t, out.CreatedAt.Equal(in.CreatedAt), "created_at round-trips: %s vs %s", out.CreatedAt, in.CreatedAt)
		assert.Equal(t, in.Source, out.Source)
		assert.Equal(t, in.ID, out.ID)
	}
}

func TestPendingCursor_DecodeRejectsMalformed(t *testing.T) {
	codec := newTestPendingCursorCodec(t)
	id := uuid.NewString()
	seal := func(raw string) string {
		s, err := codec.sealText(raw)
		require.NoError(t, err)
		return s
	}
	// 31 + 36 bytes of text plus 28 of nonce and tag is 95, which pads.
	wellFormed := "2026-01-01T00:00:00.5Z|channel|" + id
	valid := seal(wellFormed)
	sealed, err := base64.RawURLEncoding.DecodeString(valid)
	require.NoError(t, err)
	flip := func(i int) string {
		b := bytes.Clone(sealed)
		b[i] ^= 0x01
		return base64.RawURLEncoding.EncodeToString(b)
	}
	foreign, err := newTestPendingCursorCodec(t).sealText(wellFormed)
	require.NoError(t, err)
	padded := base64.URLEncoding.EncodeToString(sealed)
	require.Contains(t, padded, "=")
	overlong := seal(wellFormed + strings.Repeat("0", 200))
	require.Greater(t, len(overlong), pendingCursorMaxLen)

	for name, cursor := range map[string]string{
		"empty":                      "",
		"bad base64":                 "!!!",
		"unsealed well-formed text":  base64.RawURLEncoding.EncodeToString([]byte(wellFormed)),
		"padded":                     padded,
		"tampered nonce":             flip(0),
		"tampered ciphertext":        flip(12),
		"tampered tag":               flip(len(sealed) - 1),
		"truncated":                  base64.RawURLEncoding.EncodeToString(sealed[:len(sealed)-1]),
		"shorter than nonce and tag": base64.RawURLEncoding.EncodeToString(sealed[:27]),
		"sealed under another key":   foreign,
		"overlong":                   overlong,
		"sealed, missing field":      seal("2026-01-01T00:00:00Z|channel"),
		"sealed, extra field":        seal(wellFormed + "|x"),
		"sealed, unknown source":     seal("2026-01-01T00:00:00Z|Channel|" + id),
		"sealed, bad time":           seal("2026-13-01T00:00:00Z|channel|" + id),
		"sealed, bad id":             seal("2026-01-01T00:00:00Z|channel|" + id[:35]),
	} {
		_, err := codec.open(cursor)
		assert.ErrorIs(t, err, errInvalidPendingCursor, name)
	}
	_, err = codec.open(valid)
	assert.NoError(t, err, "positive control: a cursor this codec sealed opens")
}

// A codec whose key could not be made fails closed both ways.
func TestPendingCursor_NoKeyFailsClosed(t *testing.T) {
	valid, err := newTestPendingCursorCodec(t).seal(pendingCursor{CreatedAt: time.Now(), Source: pendingSourceDM, ID: uuid.NewString()})
	require.NoError(t, err)

	var noKey pendingCursorCodec
	_, err = noKey.seal(pendingCursor{CreatedAt: time.Now(), Source: pendingSourceDM, ID: uuid.NewString()})
	assert.ErrorIs(t, err, errPendingCursorUnavailable)
	_, err = noKey.open(valid)
	assert.ErrorIs(t, err, errInvalidPendingCursor)
}
