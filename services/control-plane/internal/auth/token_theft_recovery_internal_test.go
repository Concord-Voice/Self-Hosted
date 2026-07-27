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

func TestRecoverPendingTokenTheftRevocations_MarksRecoveryGeneration(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	redisClient := setupAuthAttemptRedis(t)
	require.NoError(t, redisClient.Del(t.Context(), middleware.TokenTheftRecoveryGenerationKey()).Err())

	require.NoError(t, RecoverPendingTokenTheftRevocations(t.Context(), db, redisClient, logger.New("test")))
	state, err := redisClient.Get(t.Context(), middleware.TokenTheftRecoveryGenerationKey()).Result()
	require.NoError(t, err)
	assert.Equal(t, "1", state)
}

func TestProcessTokenTheftRevocation_NoPendingRecordIsNoop(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	redisClient := setupAuthAttemptRedis(t)

	require.NoError(t, ProcessTokenTheftRevocation(t.Context(), db, redisClient, uuid.NewString()))
}

func TestProcessTokenTheftRevocation_CompletesLegacyPendingRecord(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	redisClient := setupAuthAttemptRedis(t)
	userID := dbtest.CreateUser(t, db)
	_, err := db.Exec(
		`INSERT INTO token_theft_pending_revocations (user_id) VALUES ($1)`, userID,
	)
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at) VALUES ($1, $2, $3, NOW() + INTERVAL '1 hour')`,
		uuid.New(), userID, HashRefreshToken("legacy-pending-token"),
	)
	require.NoError(t, err)

	require.NoError(t, ProcessTokenTheftRevocation(t.Context(), db, redisClient, userID.String()))

	var active, pending int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, userID,
	).Scan(&active))
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM token_theft_pending_revocations WHERE user_id = $1`, userID,
	).Scan(&pending))
	assert.Zero(t, active)
	assert.Zero(t, pending)
	blocked, err := redisClient.Exists(t.Context(), middleware.TokenTheftBlockedKey(userID.String())).Result()
	require.NoError(t, err)
	assert.Zero(t, blocked)
}

func TestFinalizeTokenTheftRevocation_PreservesIncompleteRecord(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	redisClient := setupAuthAttemptRedis(t)
	userID := dbtest.CreateUser(t, db)
	operationID := uuid.NewString()
	_, err := db.Exec(
		`INSERT INTO token_theft_pending_revocations (user_id, operation_id) VALUES ($1, $2)`, userID, operationID,
	)
	require.NoError(t, err)

	require.NoError(t, finalizeTokenTheftRevocation(t.Context(), db, redisClient, userID.String(), sql.NullString{String: operationID, Valid: true}))

	var pending int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM token_theft_pending_revocations WHERE user_id = $1`, userID,
	).Scan(&pending))
	assert.Equal(t, 1, pending)
}

func TestRecoverPendingTokenTheftRevocations_PromotesRedisFallbacks(t *testing.T) {
	tests := []struct {
		name  string
		stage func(context.Context, *redis.Client, string) error
	}{
		{
			name: "operation-scoped fallback",
			stage: func(ctx context.Context, redisClient *redis.Client, userID string) error {
				operationID := uuid.NewString()
				if err := redisClient.HSet(ctx, tokenTheftFallbackHashKey, userID, operationID).Err(); err != nil {
					return err
				}
				return redisClient.Set(ctx, middleware.TokenTheftBlockedKey(userID), operationID, 0).Err()
			},
		},
		{
			name: "legacy fallback",
			stage: func(ctx context.Context, redisClient *redis.Client, userID string) error {
				if err := redisClient.SAdd(ctx, tokenTheftLegacyFallbackSetKey, userID).Err(); err != nil {
					return err
				}
				return redisClient.Set(ctx, middleware.TokenTheftBlockedKey(userID), tokenTheftLegacyStateValue, 0).Err()
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, cleanup := dbtest.SetupTestDB(t)
			t.Cleanup(cleanup)
			redisClient := setupAuthAttemptRedis(t)
			userID := dbtest.CreateUser(t, db)
			_, err := db.Exec(
				`INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at) VALUES ($1, $2, $3, NOW() + INTERVAL '1 hour')`,
				uuid.New(), userID, HashRefreshToken("fallback-token-"+test.name),
			)
			require.NoError(t, err)
			require.NoError(t, test.stage(t.Context(), redisClient, userID.String()))

			require.NoError(t, RecoverPendingTokenTheftRevocations(t.Context(), db, redisClient, logger.New("test")))

			var active, pending int
			require.NoError(t, db.QueryRow(
				`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, userID,
			).Scan(&active))
			require.NoError(t, db.QueryRow(
				`SELECT COUNT(*) FROM token_theft_pending_revocations WHERE user_id = $1`, userID,
			).Scan(&pending))
			assert.Zero(t, active)
			assert.Zero(t, pending)
			blocked, err := redisClient.Exists(t.Context(), middleware.TokenTheftBlockedKey(userID.String())).Result()
			require.NoError(t, err)
			assert.Zero(t, blocked)
		})
	}
}

func TestRecoverPendingTokenTheftRevocations_WithoutRedisKeepsPendingRecord(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := dbtest.CreateUser(t, db)
	require.NoError(t, EnqueueTokenTheftRevocation(t.Context(), db, userID.String()))

	err := RecoverPendingTokenTheftRevocations(t.Context(), db, nil, logger.New("test"))
	require.ErrorContains(t, err, "token theft block store unavailable")

	var pending int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM token_theft_pending_revocations WHERE user_id = $1`, userID,
	).Scan(&pending))
	assert.Equal(t, 1, pending)
}

func TestRecoverPendingTokenTheftRevocations_KeepsFallbackWhenPromotionFails(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	redisClient := setupAuthAttemptRedis(t)
	userID := dbtest.CreateUser(t, db)
	operationID := uuid.NewString()
	require.NoError(t, redisClient.HSet(t.Context(), tokenTheftFallbackHashKey, userID.String(), operationID).Err())
	require.NoError(t, redisClient.Set(t.Context(), middleware.TokenTheftBlockedKey(userID.String()), operationID, 0).Err())
	require.NoError(t, installTokenTheftRetryEnqueueFailure(db))
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS token_theft_retry_enqueue_failure ON token_theft_pending_revocations`)
		_, _ = db.Exec(`DROP FUNCTION IF EXISTS token_theft_retry_enqueue_failure()`)
	})

	err := RecoverPendingTokenTheftRevocations(t.Context(), db, redisClient, logger.New("test"))
	require.ErrorContains(t, err, "promote token theft fallback")

	storedOperationID, err := redisClient.HGet(t.Context(), tokenTheftFallbackHashKey, userID.String()).Result()
	require.NoError(t, err)
	assert.Equal(t, operationID, storedOperationID)
}

func TestRetryTokenTheftRecoveryOnce_JoinsQueueAndProcessingFailures(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := dbtest.CreateUser(t, db)
	h := &Handler{db: db}

	err := h.retryTokenTheftRecoveryOnce(t.Context(), userID.String())
	require.Error(t, err)
	assert.ErrorContains(t, err, "persist token theft recovery state")
	assert.ErrorContains(t, err, "revoke refresh tokens after token theft retry")
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
