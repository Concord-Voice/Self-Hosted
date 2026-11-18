package channels

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// VULN-080 / VULN-207: the pending-keys cursor codec.

func TestPendingCursor_RoundTrip(t *testing.T) {
	// Postgres keeps microseconds, and a non-UTC offset must survive too.
	at := time.Date(2026, 11, 18, 9, 30, 15, 123456000, time.FixedZone("x", -5*3600))
	for _, source := range []string{pendingSourceChannel, pendingSourceDM} {
		in := pendingCursor{CreatedAt: at, Source: source, ID: uuid.NewString()}
		encoded := in.encode()
		assert.LessOrEqual(t, len(encoded), pendingCursorMaxLen)
		assert.NotContains(t, encoded, "=", "the cursor is unpadded base64url")

		out, err := decodePendingCursor(encoded)
		require.NoError(t, err)
		assert.True(t, out.CreatedAt.Equal(in.CreatedAt), "created_at round-trips: %s vs %s", out.CreatedAt, in.CreatedAt)
		assert.Equal(t, in.Source, out.Source)
		assert.Equal(t, in.ID, out.ID)
	}
}

func TestPendingCursor_DecodeRejectsMalformed(t *testing.T) {
	id := uuid.NewString()
	raw := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for name, cursor := range map[string]string{
		"empty":          "",
		"bad base64":     "!!!",
		"padded":         base64.URLEncoding.EncodeToString([]byte("2026-01-01T00:00:00Z|channel|" + id)), // 65 bytes, so "="
		"missing field":  raw("2026-01-01T00:00:00Z|channel"),
		"extra field":    raw("2026-01-01T00:00:00Z|channel|" + id + "|x"),
		"unknown source": raw("2026-01-01T00:00:00Z|Channel|" + id),
		"bad time":       raw("2026-13-01T00:00:00Z|channel|" + id),
		"bad id":         raw("2026-01-01T00:00:00Z|channel|" + id[:35]),
		"overlong":       raw("2026-01-01T00:00:00Z|channel|" + id + string(make([]byte, 200))),
	} {
		_, err := decodePendingCursor(cursor)
		assert.ErrorIs(t, err, errInvalidPendingCursor, name)
	}
	_, err := decodePendingCursor(raw("2026-01-01T00:00:00Z|channel|" + id))
	assert.NoError(t, err, "positive control: a well-formed cursor decodes")
}
