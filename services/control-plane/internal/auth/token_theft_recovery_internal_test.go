package auth

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/models"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQueueTokenTheftRevocation_PersistsFallbackAfterEnqueueContextExpires(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	redisClient := setupAuthAttemptRedis(t)
	userID := dbtest.CreateUser(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()

	queuedInDB, err := queueTokenTheftRevocation(ctx, db, redisClient, userID.String())
	require.NoError(t, err)
	assert.False(t, queuedInDB)
	blocked, err := redisClient.Exists(context.Background(), middleware.TokenTheftBlockedKey(userID.String())).Result()
	require.NoError(t, err)
	assert.EqualValues(t, 1, blocked)
}

type tokenTheftRetryDisconnector struct{}

func (tokenTheftRetryDisconnector) DisconnectUser(uuid.UUID) {}

func TestHandleTokenTheft_RetriesWhenBothPersistenceStoresFail(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	redisClient := setupAuthAttemptRedis(t)
	userID := dbtest.CreateUser(t, db)
	_, err := db.Exec(
		`INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at) VALUES ($1, $2, $3, NOW() + INTERVAL '1 hour')`,
		uuid.New(), userID, HashRefreshToken("token-theft-retry-token"),
	)
	require.NoError(t, err)
	require.NoError(t, installTokenTheftRetryEnqueueFailure(db))
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS token_theft_retry_enqueue_failure ON token_theft_pending_revocations`)
		_, _ = db.Exec(`DROP FUNCTION IF EXISTS token_theft_retry_enqueue_failure()`)
	})

	const forceFailureKey = "token_theft_recovery_test:force_failure"
	require.NoError(t, redisClient.Set(context.Background(), forceFailureKey, "1", 0).Err())
	originalScript := persistTokenTheftRecoveryStateScript
	persistTokenTheftRecoveryStateScript = redis.NewScript(`
if redis.call("GET", "token_theft_recovery_test:force_failure") == "1" then
  return redis.error_reply("forced token theft recovery state failure")
end
redis.call("HSET", KEYS[1], ARGV[1], ARGV[2])
redis.call("SET", KEYS[2], ARGV[2])
return 1
`)
	t.Cleanup(func() { persistTokenTheftRecoveryStateScript = originalScript })

	h := &Handler{db: db, redis: redisClient, log: logger.New("test"), hub: tokenTheftRetryDisconnector{}}
	c, rec := newLoginContext()
	require.True(t, h.handleTokenTheft(c, models.RefreshToken{UserID: userID.String()}))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	_, err = db.Exec(`DROP TRIGGER IF EXISTS token_theft_retry_enqueue_failure ON token_theft_pending_revocations`)
	require.NoError(t, err)
	require.NoError(t, redisClient.Del(context.Background(), forceFailureKey).Err())

	assert.Eventually(t, func() bool {
		var active int
		err := db.QueryRow(`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, userID).Scan(&active)
		return err == nil && active == 0
	}, 5*time.Second, 25*time.Millisecond)
}

func installTokenTheftRetryEnqueueFailure(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE OR REPLACE FUNCTION token_theft_retry_enqueue_failure() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION 'token theft retry enqueue failure';
		END;
		$$ LANGUAGE plpgsql`); err != nil {
		return err
	}
	_, err := db.Exec(`
		CREATE TRIGGER token_theft_retry_enqueue_failure BEFORE INSERT ON token_theft_pending_revocations
		FOR EACH ROW EXECUTE FUNCTION token_theft_retry_enqueue_failure()`)
	return err
}
