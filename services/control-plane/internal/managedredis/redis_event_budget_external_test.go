package managedredis_test

import (
	"errors"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/database"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedredis"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEventBudgetDedicatedClientUnsubscribesWithoutClosingSharedRuntime(t *testing.T) {
	mini := miniredis.RunT(t)
	mini.RequireUserAuth("cv_cp_12", "synthetic-password")
	runtime := managedredis.EventBudgetTestRuntime(t)
	require.Zero(t, managedredis.EventBudgetTestListenerCount(runtime))
	shared, err := database.NewManagedRedisClient(t.Context(), mini.Addr(), nil, runtime)
	require.NoError(t, err)
	t.Cleanup(func() {
		err := shared.Close()
		if !errors.Is(err, redis.ErrClosed) {
			assert.NoError(t, err)
		}
	})
	require.Equal(t, 1, managedredis.EventBudgetTestListenerCount(runtime))
	dedicated, err := database.NewManagedEventBudgetRedisClient(mini.Addr(), nil, runtime)
	require.NoError(t, err)
	t.Cleanup(func() {
		err := dedicated.Close()
		if !errors.Is(err, redis.ErrClosed) {
			assert.NoError(t, err)
		}
	})
	require.Equal(t, 1, managedredis.EventBudgetTestListenerCount(runtime), "constructor must neither dial nor subscribe")
	require.NoError(t, dedicated.Ping(t.Context()).Err())
	require.Equal(t, 2, managedredis.EventBudgetTestListenerCount(runtime))
	require.NoError(t, dedicated.Close())
	require.Equal(t, 1, managedredis.EventBudgetTestListenerCount(runtime), "owned connection Close must unsubscribe")
	require.NoError(t, shared.Set(t.Context(), "shared-survives-dedicated-unsubscribe", "value", 0).Err())
	require.NoError(t, runtime.Fresh(runtime.Now()))
	require.Equal(t, 1, managedredis.EventBudgetTestListenerCount(runtime))
	require.NoError(t, shared.Close())
	require.Zero(t, managedredis.EventBudgetTestListenerCount(runtime))
	require.Eventually(t, func() bool { return mini.CurrentConnectionCount() == 0 }, time.Second, time.Millisecond)
}
