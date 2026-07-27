package auth_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartTokenTheftRecoveryWorker_CompletesStartupRecoveryBeforeReturning(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "token-theft-recovery")
	_, err := ts.DB.Exec(
		`INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at) VALUES ($1, $2, $3, NOW() + INTERVAL '1 hour')`,
		uuid.New(), user.ID, auth.HashRefreshToken("startup-recovery-session"),
	)
	require.NoError(t, err)
	require.NoError(t, auth.EnqueueTokenTheftRevocation(context.Background(), ts.DB, user.ID))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	auth.StartTokenTheftRecoveryWorker(ctx, ts.DB, ts.Redis, logger.New("test"), time.Hour)

	var pending, active int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM token_theft_pending_revocations WHERE user_id = $1`, user.ID,
	).Scan(&pending))
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, user.ID,
	).Scan(&active))
	assert.Zero(t, pending)
	assert.Zero(t, active)

	var revokedAt sql.NullTime
	require.NoError(t, ts.DB.QueryRow(
		`SELECT revoked_at FROM refresh_tokens WHERE user_id = $1`, user.ID,
	).Scan(&revokedAt))
	require.True(t, revokedAt.Valid)
}

func TestProcessTokenTheftRevocation_SkipsCompletedRecord(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "token-theft-no-stale-replay")
	ctx := context.Background()
	_, err := ts.DB.Exec(
		`INSERT INTO token_theft_pending_revocations (user_id, completed_at) VALUES ($1, NOW())`, user.ID,
	)
	require.NoError(t, err)

	_, err = ts.DB.Exec(
		`INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at) VALUES ($1, $2, $3, NOW() + INTERVAL '1 hour')`,
		uuid.New(), user.ID, auth.HashRefreshToken("post-recovery-session"),
	)
	require.NoError(t, err)

	// A retry that only needs Redis cleanup must not revoke this legitimately
	// minted post-recovery session.
	require.NoError(t, auth.ProcessTokenTheftRevocation(ctx, ts.DB, ts.Redis, user.ID))
	var active int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, user.ID,
	).Scan(&active))
	assert.Equal(t, 1, active)
	var pending int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM token_theft_pending_revocations WHERE user_id = $1`, user.ID,
	).Scan(&pending))
	assert.Zero(t, pending)
}

func TestStartTokenTheftRecoveryWorker_RetriesRecoveryFailures(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "token-theft-recovery-failure")
	_, err := ts.DB.Exec(
		`INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at) VALUES ($1, $2, $3, NOW() + INTERVAL '1 hour')`,
		uuid.New(), user.ID, auth.HashRefreshToken("failed-startup-recovery-session"),
	)
	require.NoError(t, err)
	require.NoError(t, auth.EnqueueTokenTheftRevocation(context.Background(), ts.DB, user.ID))

	deadRedis := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 10 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { require.NoError(t, deadRedis.Close()) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	auth.StartTokenTheftRecoveryWorker(ctx, ts.DB, deadRedis, logger.New("test"), time.Hour)

	var pending int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM token_theft_pending_revocations WHERE user_id = $1`, user.ID,
	).Scan(&pending))
	assert.Equal(t, 1, pending)
}
