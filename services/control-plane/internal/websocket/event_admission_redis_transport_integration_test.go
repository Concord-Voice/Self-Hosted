package websocket

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/database"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Preparation is outside the measured account decision: preload the exact
// production script and establish the connection before backend ownership.
// Neither fixture changes production retry, timeout or RESP protocol settings.
func budgetTransportFixture(t *testing.T) (*redis.Client, *budgetTransportProxy, *redis.Client) {
	t.Helper()
	inspector := budgetRealRedis(t)
	require.NoError(t, redis.NewScript(eventAdmissionLua).Load(t.Context(), inspector).Err())
	proxy := newBudgetTransportProxy(t, redistest.URL(t))
	client, err := database.NewEventBudgetRedisClient(proxy.URL())
	require.NoError(t, err)
	t.Cleanup(func() {
		err := client.Close()
		if !errors.Is(err, redis.ErrClosed) {
			assert.NoError(t, err)
		}
	})
	require.NoError(t, client.Ping(t.Context()).Err())
	require.Zero(t, proxy.ScriptCalls(), "connection setup must not run the budget script")
	return inspector, proxy, client
}
func budgetTransportIdentity(t *testing.T, inspector *redis.Client, class EventClass) (uuid.UUID, string) {
	t.Helper()
	user := uuid.NewSHA1(uuid.NameSpaceOID, []byte(t.Name()))
	key := "ratelimit:user:ws:v1:{" + user.String() + "}:" + class.String()
	require.NoError(t, inspector.Del(t.Context(), key).Err())
	t.Cleanup(func() { assert.NoError(t, inspector.Del(context.Background(), key).Err()) })
	return user, key
}
func budgetTransportTimes(t *testing.T, inspector *redis.Client, key string) (int64, int64) {
	t.Helper()
	fields, err := inspector.HGetAll(t.Context(), key).Result()
	require.NoError(t, err)
	require.Len(t, fields, 3)
	require.Equal(t, "1", fields["v"])
	last, err := strconv.ParseInt(fields["last_us"], 10, 64)
	require.NoError(t, err)
	tat, err := strconv.ParseInt(fields["tat_us"], 10, 64)
	require.NoError(t, err)
	require.Equal(t, strconv.FormatInt(last, 10), fields["last_us"])
	require.Equal(t, strconv.FormatInt(tat, 10), fields["tat_us"])
	return last, tat
}
func budgetTransportNoReplayAfterClose(t *testing.T, proxy *budgetTransportProxy, want int) {
	t.Helper()
	require.Equal(t, want, proxy.ScriptCalls())
	// Observe the still-listening fixture after the application/client Close has
	// settled. This is a bounded wire observation, not a library recovery join.
	timer := time.NewTimer(50 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	require.Equal(t, want, proxy.ScriptCalls(), "closed application must not replay a lost decision")
	require.NoError(t, proxy.Close())
	require.Equal(t, want, proxy.ScriptCalls(), "fixture teardown must not hide another script command")
}
func budgetTransportPass(t *testing.T) {
	t.Helper()
	if !t.Failed() {
		t.Log("WS_EVENT_BUDGET_TRANSPORT PASS")
	}
}
func TestRedisAccountBudgetTransportIntegrationLostReplyPreservesOneDebit(t *testing.T) {
	inspector, proxy, client := budgetTransportFixture(t)
	t.Log("WS_EVENT_BUDGET_TRANSPORT START")
	user, key := budgetTransportIdentity(t, inspector, EventClassProfile)
	backend, err := NewRedisAccountBudgetBackend(EventBudgetConfig{true, 100}, client)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, backend.Close()) })
	captured := proxy.ArmDrop()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Millisecond)
	defer cancel()
	decision, err := backend.Decide(ctx, user, EventClassProfile, 5000000, 8)
	require.ErrorIs(t, err, ErrAccountBudgetUnavailable)
	assert.Equal(t, RedisBudgetDecision{}, decision)
	admissionAwait(t, captured)
	require.Equal(t, 1, proxy.ScriptCalls(), "one preloaded EVALSHA must cross the actual socket")
	last, tat := budgetTransportTimes(t, inspector, key)
	assert.Equal(t, int64(5000000), tat-last, "a complete server debit must survive loss before client receipt")
	require.NoError(t, backend.Close())
	require.NoError(t, backend.Close())
	require.NoError(t, inspector.Ping(t.Context()).Err(), "independent inspector must survive owned-client Close")
	later, err := backend.Decide(t.Context(), user, EventClassProfile, 5000000, 8)
	assert.ErrorIs(t, err, ErrAccountBudgetUnavailable)
	assert.Equal(t, RedisBudgetDecision{}, later)
	budgetTransportNoReplayAfterClose(t, proxy, 1)
	_, afterTat := budgetTransportTimes(t, inspector, key)
	assert.Equal(t, tat, afterTat, "no hidden retry may add another interval")
	budgetTransportPass(t)
}
