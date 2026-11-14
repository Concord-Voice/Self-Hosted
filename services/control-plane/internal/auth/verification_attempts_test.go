package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// setupAuthAttemptRedis returns a client on this process's own Redis logical
// database, allocated by redistest (#2680). The scoped reset replaces a bare
// FLUSHDB that could reach whatever database the client happened to hold.
func setupAuthAttemptRedis(t *testing.T) *redis.Client {
	t.Helper()

	client := redistest.Client(t)
	require.NoError(t, client.ConfigSet(context.Background(), "appendonly", "yes").Err())
	require.NoError(t, redistest.Reset(context.Background(), client))

	return client
}

func TestAttemptsGuardCountsStaleConcurrentInvalidAttemptsAtomically(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	client := setupAuthAttemptRedis(t)
	h := &Handler{redis: client}
	pendingID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	record := verificationRecord{
		CodeHash: hashCode("123456"),
		Email:    "race@example.com",
		Attempts: 0,
	}
	raw, err := json.Marshal(record)
	require.NoError(t, err)
	require.NoError(t, client.Set(ctx, redisKey(pendingID), raw, VerifyCodeTTLNew).Err())

	type attemptResult struct {
		ok     bool
		status int
	}
	totalAttempts := MaxCodeAttempts + 4
	results := make(chan attemptResult, totalAttempts)
	var wg sync.WaitGroup
	for i := 0; i < totalAttempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)

			proof := h.attemptsGuard(ctx, c, pendingID, "000000") //nolint:gosec // test value, not a credential
			results <- attemptResult{ok: proof != nil, status: w.Code}
		}()
	}
	wg.Wait()
	close(results)

	unauthorized := 0
	tooMany := 0
	for result := range results {
		require.False(t, result.ok)
		switch result.status {
		case http.StatusUnauthorized:
			unauthorized++
		case http.StatusTooManyRequests:
			tooMany++
		default:
			require.Failf(t, "unexpected status", "status=%d", result.status)
		}
	}
	require.Equal(t, MaxCodeAttempts, unauthorized)
	require.Equal(t, totalAttempts-MaxCodeAttempts, tooMany)

	attempts, err := client.Get(ctx, verificationAttemptsKey(pendingID)).Int()
	require.NoError(t, err)
	require.GreaterOrEqual(t, attempts, MaxCodeAttempts)
}

// regression for #2083
func TestAttemptsGuardReturnsTooManyWhenCodeAlreadyExhausted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	client := setupAuthAttemptRedis(t)
	h := &Handler{redis: client}
	pendingID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")
	record := verificationRecord{
		CodeHash: hashCode("123456"),
		Email:    "exhausted@example.com",
		Attempts: 0,
	}
	raw, err := json.Marshal(record)
	require.NoError(t, err)
	require.NoError(t, client.Set(ctx, redisKey(pendingID), raw, VerifyCodeTTLNew).Err())

	h.exhaustVerificationCode(ctx, pendingID)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	proof := h.attemptsGuard(ctx, c, pendingID, "000000") //nolint:gosec // test value, not a credential
	require.Nil(t, proof)
	require.Equal(t, http.StatusTooManyRequests, w.Code)

	var body map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, "too_many_attempts", body["code"])
}

func TestAttemptsGuardRearmsMissingAttemptTTL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	client := setupAuthAttemptRedis(t)
	h := &Handler{redis: client}
	pendingID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440001")
	record := verificationRecord{
		CodeHash: hashCode("123456"),
		Email:    "ttl@example.com",
		Attempts: 0,
	}
	raw, err := json.Marshal(record)
	require.NoError(t, err)
	require.NoError(t, client.Set(ctx, redisKey(pendingID), raw, VerifyCodeTTLNew).Err())
	require.NoError(t, client.Set(ctx, verificationAttemptsKey(pendingID), "1", 0).Err())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	proof := h.attemptsGuard(ctx, c, pendingID, "000000") //nolint:gosec // test value, not a credential
	require.Nil(t, proof)
	require.Equal(t, http.StatusUnauthorized, w.Code)

	ttl := client.TTL(ctx, verificationAttemptsKey(pendingID)).Val()
	require.Positive(t, ttl)
}

func TestDeleteVerificationMaterialIfCurrentPreservesReplacement(t *testing.T) {
	ctx := context.Background()
	client := setupAuthAttemptRedis(t)
	h := &Handler{redis: client}
	pendingID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440003")

	oldRaw := `{"code_hash":"old","email":"a@example.test"}`
	newRaw := `{"code_hash":"new","email":"b@example.test"}`
	require.NoError(t, client.Set(ctx, redisKey(pendingID), newRaw, VerifyCodeTTLNew).Err())
	require.NoError(t, client.Set(ctx, testVerificationCodeKey(pendingID), "654321", VerifyCodeTTLNew).Err())
	require.NoError(t, client.Set(ctx, verificationAttemptsKey(pendingID), "2", VerifyCodeTTLNew).Err())

	require.NoError(t, h.deleteVerificationMaterialIfCurrent(ctx, pendingID, oldRaw))
	stored, err := client.Get(ctx, redisKey(pendingID)).Result()
	require.NoError(t, err)
	require.Equal(t, newRaw, stored)
	require.Equal(t, "654321", client.Get(ctx, testVerificationCodeKey(pendingID)).Val())
	require.Equal(t, "2", client.Get(ctx, verificationAttemptsKey(pendingID)).Val())
}
