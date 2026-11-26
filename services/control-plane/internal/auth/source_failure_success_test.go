package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The source failure counter is shared by every client in one IPv4 /32 or IPv6
// /64. A successful sign-in proves one account, not the source, so it must not
// reset that counter: otherwise anyone with an account in the bucket can fail
// N-1 times, sign in, and repeat without ever reaching the cumulative ban. The
// counter decays with its own window instead. Per-account state still clears.
const (
	sourceBudgetPeer = "192.0.2.1" // httptest.NewRequest's RemoteAddr
	sourceFailureKey = "auth_failures:ip:" + sourceBudgetPeer
	sourceBanKey     = "auth_ban:ip:" + sourceBudgetPeer
)

func seedSourceFailures(t *testing.T, ts *testhelpers.TestServer) int {
	t.Helper()
	cfg := middleware.DefaultAuthBanConfig()
	seeded := cfg.Threshold - 1
	require.NoError(t, ts.Redis.Set(context.Background(), sourceFailureKey, seeded, cfg.Window).Err())
	return seeded
}

func requireSourceFailures(t *testing.T, ts *testhelpers.TestServer, want int) {
	t.Helper()
	got, err := ts.Redis.Get(context.Background(), sourceFailureKey).Int()
	require.NoError(t, err, "a successful authentication must not delete the shared source failure counter")
	assert.Equal(t, want, got)
	ttl, err := ts.Redis.TTL(context.Background(), sourceFailureKey).Result()
	require.NoError(t, err)
	assert.Positive(t, ttl, "the counter must keep decaying with its original window")
}

func TestSuccessfulLoginKeepsTheSharedSourceFailureCount(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "srckeeplogin")
	ctx := context.Background()
	seeded := seedSourceFailures(t, ts)
	accountKey := "login_attempts:" + user.Email
	require.NoError(t, ts.Redis.Set(ctx, accountKey, "2", time.Hour).Err())

	w := ts.DoRequest(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": user.Email, "password": user.Password,
	}, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	requireSourceFailures(t, ts, seeded)
	_, err := ts.Redis.Get(ctx, accountKey).Result()
	assert.True(t, errors.Is(err, redis.Nil), "the signed-in account's own attempt counter still clears")

	// The reset-and-repeat cycle is closed: the next failure from the same
	// source reaches the threshold and bans it, even for the valid account.
	w = ts.DoRequest(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": user.Email, "password": "WrongPassword123!", // pragma: allowlist secret
	}, nil)
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	banned, err := ts.Redis.Exists(ctx, sourceBanKey).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), banned, "the threshold failure must ban the source")

	w = ts.DoRequest(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": user.Email, "password": user.Password,
	}, nil)
	assert.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
}

func TestRecoveryResetKeepsTheSharedSourceFailureCount(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "srckeepreset")
	seeded := seedSourceFailures(t, ts)

	seedRecoveryCode(t, ts, user.Email, "999999", user.ID)
	w := ts.DoRequest(http.MethodPost, pathRecoveryVerifyCode, map[string]interface{}{
		"email": user.Email, "code": "999999",
	}, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var verifyBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &verifyBody)
	recoveryToken, ok := verifyBody["recovery_token"].(string)
	require.True(t, ok)

	pub, wrappedKey, salt := testhelpers.E2EETestKeys()
	w = ts.DoRequest(http.MethodPost, pathRecoveryResetPwd, map[string]interface{}{
		"recovery_token":      recoveryToken,
		"new_password":        testNewPassword,
		"wrapped_private_key": wrappedKey,
		"key_derivation_salt": salt,
		"key_derivation_alg":  testKeyDerivationAlg,
		"public_key":          pub,
	}, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	requireSourceFailures(t, ts, seeded)
}

// A refused recovery key counts against the client's source bucket, the same
// /64 that every address in it shares, so rotating within the /64 gains nothing.
const recoveryKeySourceBucket = "auth_failures:ip:2001:db8:163:1::"

// postRecoveryKeyFrom sends a random recovery key from peer. JoinHostPort
// brackets an IPv6 peer; the test server trusts no proxy, so RemoteAddr is the
// client address.
func postRecoveryKeyFrom(t *testing.T, ts *testhelpers.TestServer, peer string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"token": randomRecoveryKey(t)})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, pathRecoveryVerifyCode, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = net.JoinHostPort(peer, "40000")
	w := httptest.NewRecorder()
	ts.Router.ServeHTTP(w, req)
	return w
}

func TestRefusedRecoveryKeyCountsAgainstTheSourceBucket(t *testing.T) {
	ts := setupTS(t)
	for _, peer := range []string{"2001:db8:163:1::10", "2001:db8:163:1:ffff::20"} {
		w := postRecoveryKeyFrom(t, ts, peer)
		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	}
	got, err := ts.Redis.Get(context.Background(), recoveryKeySourceBucket).Int()
	require.NoError(t, err, "a refused recovery key must count against the source bucket")
	assert.Equal(t, 2, got)
}

// failSourceFailureIncr fails only INCR on the source failure counter, so the
// ban check and IP limiter in front of the handler still pass.
type failSourceFailureIncr struct{}

var errForcedSourceFailureIncr = errors.New("forced source failure incr fault")

func failsSourceFailureIncr(cmd redis.Cmder) bool {
	args := cmd.Args()
	return cmd.Name() == "incr" && len(args) > 1 && strings.HasPrefix(fmt.Sprint(args[1]), "auth_failures:ip:")
}

func (failSourceFailureIncr) DialHook(next redis.DialHook) redis.DialHook { return next }

func (failSourceFailureIncr) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if failsSourceFailureIncr(cmd) {
			return errForcedSourceFailureIncr
		}
		return next(ctx, cmd)
	}
}

func (failSourceFailureIncr) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			if failsSourceFailureIncr(cmd) {
				for _, c := range cmds {
					c.SetErr(errForcedSourceFailureIncr)
				}
				return errForcedSourceFailureIncr
			}
		}
		return next(ctx, cmds)
	}
}

func TestRefusedRecoveryKeyFailsClosedWhenTheSourceCounterIsDown(t *testing.T) {
	ts := setupTS(t)
	ts.Redis.AddHook(failSourceFailureIncr{})

	w := postRecoveryKeyFrom(t, ts, "2001:db8:163:1::10")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.JSONEq(t, `{"error":"service unavailable","message":"rate-limit backend unavailable; please retry shortly"}`, w.Body.String())
	assert.Contains(t, []string{"2", "3", "4"}, w.Header().Get("Retry-After"), "the auth source-budget 503 jitters its Retry-After")
}
