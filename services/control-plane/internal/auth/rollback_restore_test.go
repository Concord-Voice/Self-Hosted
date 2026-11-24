package auth

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

const (
	rollbackOldRaw = `{"code_hash":"old","email":"a@example.test"}`
	rollbackNewRaw = `{"code_hash":"new","email":"a@example.test"}`
)

// A resend or change-email whose transaction does not commit must leave the
// user the code they were already mailed, and must give the charge back: no
// code was delivered.
func TestAbandonUncommittedCodeRestoresPreviousCodeAndRefunds(t *testing.T) {
	ctx := context.Background()
	client := newRollbackRedis(t)
	h := &Handler{redis: client, log: logger.NewWithWriter(io.Discard)}
	pendingID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440010")

	require.NoError(t, client.Set(ctx, redisKey(pendingID), rollbackOldRaw, 5*time.Minute).Err())
	snap, err := h.snapshotVerificationCode(ctx, pendingID)
	require.NoError(t, err)
	require.Equal(t, rollbackOldRaw, snap.raw)

	// prepareInitialCode overwrote the record, then the commit failed.
	require.NoError(t, client.Set(ctx, redisKey(pendingID), rollbackNewRaw, 10*time.Minute).Err())

	refunds := 0
	h.abandonUncommittedCode(ctx, "resend", pendingID, rollbackNewRaw, snap, func() { refunds++ })

	require.Equal(t, rollbackOldRaw, client.Get(ctx, redisKey(pendingID)).Val())
	ttl := client.PTTL(ctx, redisKey(pendingID)).Val()
	require.Greater(t, ttl, time.Duration(0))
	require.LessOrEqual(t, ttl, 5*time.Minute)
	require.Equal(t, 1, refunds)
}

func TestAbandonUncommittedCodeWithNoPreviousCodeRemovesTheNewOne(t *testing.T) {
	ctx := context.Background()
	client := newRollbackRedis(t)
	h := &Handler{redis: client, log: logger.NewWithWriter(io.Discard)}
	pendingID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440011")

	snap, err := h.snapshotVerificationCode(ctx, pendingID)
	require.NoError(t, err)
	require.Empty(t, snap.raw)
	require.NoError(t, client.Set(ctx, redisKey(pendingID), rollbackNewRaw, 10*time.Minute).Err())

	refunds := 0
	h.abandonUncommittedCode(ctx, "change-email", pendingID, rollbackNewRaw, snap, func() { refunds++ })

	require.Zero(t, client.Exists(ctx, redisKey(pendingID)).Val())
	require.Equal(t, 1, refunds)
}

// A code written after the failed attempt (a concurrent resend that won the
// lock) must survive the compensation.
func TestAbandonUncommittedCodeLeavesANewerCodeAlone(t *testing.T) {
	ctx := context.Background()
	client := newRollbackRedis(t)
	h := &Handler{redis: client, log: logger.NewWithWriter(io.Discard)}
	pendingID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440012")
	newer := `{"code_hash":"newer","email":"a@example.test"}`

	require.NoError(t, client.Set(ctx, redisKey(pendingID), rollbackOldRaw, 5*time.Minute).Err())
	snap, err := h.snapshotVerificationCode(ctx, pendingID)
	require.NoError(t, err)
	require.NoError(t, client.Set(ctx, redisKey(pendingID), newer, 10*time.Minute).Err())

	h.abandonUncommittedCode(ctx, "resend", pendingID, rollbackNewRaw, snap, func() {})

	require.Equal(t, newer, client.Get(ctx, redisKey(pendingID)).Val())
}

// Preparation failed before it stored anything (an expiring pending row): the
// previous code was never touched and only the charge is returned.
func TestAbandonUncommittedCodeBeforeAnyWriteKeepsPreviousCode(t *testing.T) {
	ctx := context.Background()
	client := newRollbackRedis(t)
	h := &Handler{redis: client, log: logger.NewWithWriter(io.Discard)}
	pendingID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440013")

	require.NoError(t, client.Set(ctx, redisKey(pendingID), rollbackOldRaw, 5*time.Minute).Err())
	snap, err := h.snapshotVerificationCode(ctx, pendingID)
	require.NoError(t, err)

	refunds := 0
	h.abandonUncommittedCode(ctx, "resend", pendingID, "", snap, func() { refunds++ })

	require.Equal(t, rollbackOldRaw, client.Get(ctx, redisKey(pendingID)).Val())
	require.Equal(t, 1, refunds)
}

// If the compensation cannot reach Redis the unsent code may still be live, so
// the charge stays.
func TestAbandonUncommittedCodeKeepsChargeWhenRestoreFails(t *testing.T) {
	ctx := context.Background()
	client := newRollbackRedis(t)
	h := &Handler{redis: client, log: logger.NewWithWriter(io.Discard)}
	pendingID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440014")
	require.NoError(t, client.Close())

	refunds := 0
	h.abandonUncommittedCode(ctx, "resend", pendingID, rollbackNewRaw, verificationSnapshot{}, func() { refunds++ })

	require.Zero(t, refunds)
}

// A client that disconnects mid-request cancels its context; the compensation
// must still run.
func TestAbandonUncommittedCodeSurvivesRequestCancellation(t *testing.T) {
	client := newRollbackRedis(t)
	h := &Handler{redis: client, log: logger.NewWithWriter(io.Discard)}
	pendingID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440015")
	bg := context.Background()

	require.NoError(t, client.Set(bg, redisKey(pendingID), rollbackOldRaw, 5*time.Minute).Err())
	snap, err := h.snapshotVerificationCode(bg, pendingID)
	require.NoError(t, err)
	require.NoError(t, client.Set(bg, redisKey(pendingID), rollbackNewRaw, 10*time.Minute).Err())

	ctx, cancel := context.WithCancel(bg)
	cancel()
	refunds := 0
	h.abandonUncommittedCode(ctx, "resend", pendingID, rollbackNewRaw, snap, func() { refunds++ })

	require.Equal(t, rollbackOldRaw, client.Get(bg, redisKey(pendingID)).Val())
	require.Equal(t, 1, refunds)
}

// newRollbackRedis is an in-process Redis: these tests exercise one Lua script
// and plain commands, so they need no shared server.
func newRollbackRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return client
}
