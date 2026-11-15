package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// Hold successful reads so every request sees the same record before any writes.
type recoveryReadBarrier struct {
	key     string
	readers int32
	seen    atomic.Int32
	release chan struct{}
}

func (h *recoveryReadBarrier) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *recoveryReadBarrier) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if err := next(ctx, cmd); err != nil {
			return err
		}
		if cmd.Name() != "get" || cmd.Args()[1] != h.key {
			return nil
		}
		seen := h.seen.Add(1)
		if seen > h.readers {
			return nil
		}
		if seen == h.readers {
			close(h.release)
		}
		select {
		case <-h.release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (h *recoveryReadBarrier) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func TestRecoveryVerifyCodeConcurrentSingleUse(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "recoverywinner")
	seedRecoveryCode(t, ts, user.Email, testRecoveryCode, user.ID)
	ts.Redis.AddHook(&recoveryReadBarrier{
		key: recoveryCodeKeyPrefix + user.Email, readers: 2, release: make(chan struct{}),
	})

	statuses := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			response := ts.DoRequest(http.MethodPost, pathRecoveryVerifyCode, map[string]string{
				"email": user.Email, "code": testRecoveryCode,
			}, nil)
			statuses <- response.Code
		}()
	}
	got := []int{<-statuses, <-statuses}
	require.ElementsMatch(t, []int{http.StatusOK, http.StatusUnauthorized}, got)
	require.Zero(t, ts.Redis.Exists(context.Background(), recoveryCodeKeyPrefix+user.Email).Val())
}

func TestRecoveryVerifyCodeConcurrentWrongAttempts(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "recoveryattempts")
	seedRecoveryCode(t, ts, user.Email, testRecoveryCode, user.ID)
	key := recoveryCodeKeyPrefix + user.Email
	// Four concurrent wrong guesses, one short of the limit: every one must be
	// counted, and the record must keep its deadline. The fifth wrong guess
	// spends the last attempt and deletes the record.
	const concurrent = 4 // one short of the five-attempt limit
	ts.Redis.AddHook(&recoveryReadBarrier{key: key, readers: concurrent, release: make(chan struct{})})
	deadline, err := ts.Redis.PExpireTime(context.Background(), key).Result()
	require.NoError(t, err)

	statuses := make(chan int, concurrent)
	for i := 0; i < concurrent; i++ {
		go func() {
			response := ts.DoRequest(http.MethodPost, pathRecoveryVerifyCode, map[string]string{
				"email": user.Email, "code": "999999",
			}, nil)
			statuses <- response.Code
		}()
	}
	for i := 0; i < concurrent; i++ {
		require.Equal(t, http.StatusUnauthorized, <-statuses)
	}
	data, err := ts.Redis.Get(context.Background(), key).Bytes()
	require.NoError(t, err)
	var record struct {
		Attempts int `json:"attempts"`
	}
	require.NoError(t, json.Unmarshal(data, &record))
	require.Equal(t, concurrent, record.Attempts)
	remainingDeadline, err := ts.Redis.PExpireTime(context.Background(), key).Result()
	require.NoError(t, err)
	require.Equal(t, deadline, remainingDeadline)

	response := ts.DoRequest(http.MethodPost, pathRecoveryVerifyCode, map[string]string{
		"email": user.Email, "code": "999999",
	}, nil)
	require.Equal(t, http.StatusUnauthorized, response.Code)
	exists, err := ts.Redis.Exists(context.Background(), key).Result()
	require.NoError(t, err)
	require.Zero(t, exists, "the guess that spends the last attempt deletes the record")
}
