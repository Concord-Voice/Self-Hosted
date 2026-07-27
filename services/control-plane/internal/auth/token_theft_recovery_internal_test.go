package auth

import (
	"context"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
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
