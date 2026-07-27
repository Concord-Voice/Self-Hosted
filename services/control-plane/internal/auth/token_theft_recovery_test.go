package auth_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/require"
)

func TestStartTokenTheftRecoveryWorker_ReplaysPendingRevocation(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "token-theft-recovery")
	require.NoError(t, auth.EnqueueTokenTheftRevocation(context.Background(), ts.DB, user.ID))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	auth.StartTokenTheftRecoveryWorker(ctx, ts.DB, ts.Redis, logger.New("test"), time.Hour)

	require.Eventually(t, func() bool {
		var pending, active int
		if err := ts.DB.QueryRow(
			`SELECT COUNT(*) FROM token_theft_pending_revocations WHERE user_id = $1`, user.ID,
		).Scan(&pending); err != nil {
			return false
		}
		if err := ts.DB.QueryRow(
			`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, user.ID,
		).Scan(&active); err != nil {
			return false
		}
		return pending == 0 && active == 0
	}, 2*time.Second, 20*time.Millisecond)

	var revokedAt sql.NullTime
	require.NoError(t, ts.DB.QueryRow(
		`SELECT revoked_at FROM refresh_tokens WHERE user_id = $1`, user.ID,
	).Scan(&revokedAt))
	require.True(t, revokedAt.Valid)
}
