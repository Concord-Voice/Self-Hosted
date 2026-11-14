package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type recoveryCommitHook struct {
	beforeCommit func() error
	afterCommit  func() error
}

func (h recoveryCommitHook) DialHook(next redis.DialHook) redis.DialHook          { return next }
func (h recoveryCommitHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }
func (h recoveryCommitHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			if cmd.Name() == "multi" && h.beforeCommit != nil {
				if err := h.beforeCommit(); err != nil {
					return err
				}
			}
		}
		if err := next(ctx, cmds); err != nil {
			return err
		}
		for _, cmd := range cmds {
			if cmd.Name() == "exec" && h.afterCommit != nil {
				return h.afterCommit()
			}
		}
		return nil
	}
}

func TestConsumeRecoveryCodeRejectsStaleRecord(t *testing.T) {
	for _, change := range []string{"deleted", "replaced", "expired"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			mini := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
			t.Cleanup(func() { require.NoError(t, rdb.Close()) })
			h := &Handler{redis: rdb}
			key := recoveryRedisKey("snapshot@test.local")
			raw, err := json.Marshal(recoveryRecord{UserID: "test-user", CodeHash: hashRecoveryCode("123456")})
			require.NoError(t, err)
			require.NoError(t, rdb.Set(ctx, key, raw, time.Minute).Err())
			replacement, err := json.Marshal(recoveryRecord{UserID: "test-user", CodeHash: hashRecoveryCode("654321")})
			require.NoError(t, err)

			var once sync.Once
			var changeErr error
			rdb.AddHook(recoveryCommitHook{beforeCommit: func() error {
				once.Do(func() {
					switch change {
					case "deleted":
						changeErr = rdb.Del(ctx, key).Err()
					case "replaced":
						changeErr = rdb.Set(ctx, key, replacement, time.Minute).Err()
					case "expired":
						mini.FastForward(2 * time.Minute)
					}
				})
				return changeErr
			}})

			record, err := h.consumeRecoveryCode(ctx, key, "123456")
			require.ErrorIs(t, err, redis.Nil)
			require.Nil(t, record)
			if change == "replaced" {
				record, err = h.consumeRecoveryCode(ctx, key, "654321")
				require.NoError(t, err)
				require.Equal(t, "test-user", record.UserID)
			}
			require.False(t, mini.Exists(key))
		})
	}
}

func TestConsumeRecoveryCodeFailsClosed(t *testing.T) {
	for _, failure := range []string{"malformed record", "read failure", "commit failure", "canceled context"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			mini := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mini.Addr(), MaxRetries: -1})
			t.Cleanup(func() { require.NoError(t, rdb.Close()) })
			h := &Handler{redis: rdb}
			key := recoveryRedisKey("failure@test.local")
			raw, err := json.Marshal(recoveryRecord{UserID: "test-user", CodeHash: hashRecoveryCode("123456")})
			require.NoError(t, err)
			if failure == "malformed record" {
				raw = []byte("{")
			}
			require.NoError(t, rdb.Set(ctx, key, raw, time.Minute).Err())
			switch failure {
			case "read failure":
				mini.SetError("backend unavailable")
			case "commit failure":
				rdb.AddHook(recoveryCommitHook{beforeCommit: func() error { return errors.New("write refused") }})
			case "canceled context":
				cancel()
			}

			record, err := h.consumeRecoveryCode(ctx, key, "123456")
			require.Error(t, err)
			require.NotErrorIs(t, err, redis.Nil)
			require.Nil(t, record)
			mini.SetError("")
			remaining, err := mini.Get(key)
			require.NoError(t, err)
			require.Equal(t, string(raw), remaining)
		})
	}
}

func TestConsumeRecoveryCodeFailsClosedAfterCommit(t *testing.T) {
	ctx := context.Background()
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr(), MaxRetries: -1})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	h := &Handler{redis: rdb}
	key := recoveryRedisKey("post-commit@test.local")
	raw, err := json.Marshal(recoveryRecord{UserID: "test-user", CodeHash: hashRecoveryCode("123456")})
	require.NoError(t, err)
	require.NoError(t, rdb.Set(ctx, key, raw, time.Minute).Err())
	commitErr := errors.New("commit response lost")
	rdb.AddHook(recoveryCommitHook{afterCommit: func() error { return commitErr }})

	record, err := h.consumeRecoveryCode(ctx, key, "123456")
	require.ErrorIs(t, err, commitErr)
	require.Nil(t, record)
	require.False(t, mini.Exists(key))
}

func TestDeleteRecoveryRecordIfUnchangedPreservesReplacement(t *testing.T) {
	ctx := context.Background()
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	h := &Handler{redis: rdb}
	key := recoveryRedisKey("replace-after-send-failure@test.local")

	require.NoError(t, rdb.Set(ctx, key, "replacement", time.Minute).Err())
	require.NoError(t, h.deleteRecoveryRecordIfUnchanged(ctx, key, "installed"))
	stored, err := rdb.Get(ctx, key).Result()
	require.NoError(t, err)
	require.Equal(t, "replacement", stored)
}

func TestRecoveryFailureBudgetRoundsPTTLUp(t *testing.T) {
	ctx := context.Background()
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	h := &Handler{redis: rdb}
	key := recoveryFailureKeyPrefix + "retry-boundary@test.local"

	for range recoveryFailureLimit {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		require.True(t, h.allowRecoveryFailureAttempt(ctx, c, key))
	}
	mini.FastForward(500 * time.Millisecond)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	require.False(t, h.allowRecoveryFailureAttempt(ctx, c, key))
	require.Equal(t, http.StatusTooManyRequests, w.Code)
	require.Equal(t, "86400", w.Header().Get("Retry-After"))
}

type recoveryBudgetFaultHook struct {
	command string
	zero    bool
}

func (recoveryBudgetFaultHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h recoveryBudgetFaultHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() != h.command {
			return next(ctx, cmd)
		}
		if h.zero {
			cmd.(*redis.DurationCmd).SetVal(0)
			return nil
		}
		err := errors.New("forced recovery budget fault")
		cmd.SetErr(err)
		return err
	}
}
func (recoveryBudgetFaultHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func TestRecoveryFailureBudgetBackendFailureIs503(t *testing.T) {
	for _, tc := range []struct {
		name string
		hook recoveryBudgetFaultHook
	}{
		{name: "increment fault", hook: recoveryBudgetFaultHook{command: "incr"}},
		{name: "PTTL fault", hook: recoveryBudgetFaultHook{command: "pttl"}},
		{name: "nonpositive PTTL", hook: recoveryBudgetFaultHook{command: "pttl", zero: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			mini := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mini.Addr(), MaxRetries: -1})
			t.Cleanup(func() { require.NoError(t, rdb.Close()) })
			key := recoveryFailureKeyPrefix + "backend-fault@test.local"
			require.NoError(t, rdb.Set(ctx, key, "10", time.Minute).Err())
			rdb.AddHook(tc.hook)
			h := &Handler{redis: rdb, log: logger.New("test")}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			require.False(t, h.allowRecoveryFailureAttempt(ctx, c, key))
			require.Equal(t, http.StatusServiceUnavailable, w.Code)
			require.Contains(t, w.Body.String(), "step_up_budget_unavailable")
			require.Empty(t, w.Header().Get("Retry-After"))
		})
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	h := &Handler{}
	require.False(t, h.allowRecoveryFailureAttempt(context.Background(), c, recoveryFailureKeyPrefix+"no-client@test.local"))
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Contains(t, w.Body.String(), "step_up_budget_unavailable")
}
