package sessions_test

// The session-revoke password counts toward the account's shared /auth/login
// lockout, so a stolen session cannot guess the password here without the
// five-guess limit (CWE-307).

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/sessions"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// resetUserRateLimits clears only userID's per-route limiter keys, so the
// route's limiter does not answer 429 before the lockout is reached.
func resetUserRateLimits(t *testing.T, ts *testhelpers.TestServer, userID string) {
	t.Helper()
	keys, err := ts.Redis.Keys(context.Background(), "ratelimit:*"+userID+"*").Result()
	require.NoError(t, err)
	if len(keys) > 0 {
		require.NoError(t, ts.Redis.Del(context.Background(), keys...).Err())
	}
}

// Mutant killed: verifying with auth.VerifyPassword directly (the sixth
// attempt with the correct password would revoke the sessions with 200).
func TestRevokeAll_FiveWrongPasswordsLockTheAccount(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "revokelock")
	before := activeSessionCount(t, ts, user.ID)

	revoke := func(password string) (int, map[string]interface{}) {
		resetUserRateLimits(t, ts, user.ID)
		w := ts.DoRequest(http.MethodPost, revokeAllPath, map[string]interface{}{
			"password": password, "include_current": true,
		}, testhelpers.AuthHeaders(user.AccessToken))
		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		return w.Code, body
	}

	for i := range 5 {
		code, _ := revoke("WrongPassword999!")
		require.Equal(t, http.StatusForbidden, code, "guess %d", i+1)
	}

	code, body := revoke(user.Password)
	assert.Equal(t, http.StatusLocked, code, "the lockout refuses the correct password too")
	assert.Equal(t, "account_locked", body["error_code"])
	assert.Equal(t, before, activeSessionCount(t, ts, user.ID), "no session was revoked")
}

// A Redis fault refuses the guess as a server error: never a lock, never a
// pass that would revoke the sessions.
func TestRevokeAll_RedisDownIs500(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "revokeredisdown")
	before := activeSessionCount(t, ts, user.ID)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	mr.Close()

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("user_id", user.ID) })
	router.POST(revokeAllPath, sessions.NewHandler(ts.DB, rdb, logger.New("test"), nil, nil).RevokeAllSessions)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, revokeAllPath,
		bytes.NewBufferString(`{"password":"`+user.Password+`","include_current":true}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "Failed to verify password", "the refusal comes from the password check")
	assert.Equal(t, before, activeSessionCount(t, ts, user.ID), "no session was revoked")
}

func activeSessionCount(t *testing.T, ts *testhelpers.TestServer, userID string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM refresh_tokens WHERE user_id=$1 AND revoked_at IS NULL`, userID).Scan(&n))
	return n
}

// PUT /sessions/revocation-mode goes through the same password check, so a
// locked account is refused there too, even for the correct password.
func TestUpdateRevocationMode_LockedAccountIs423(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "modelocked")
	var email string
	require.NoError(t, ts.DB.QueryRow(`SELECT email FROM users WHERE id = $1`, user.ID).Scan(&email))
	require.NoError(t, ts.Redis.Set(context.Background(), "login_lockout:"+email, "1", time.Hour).Err())

	w := ts.DoRequest(http.MethodPut, revocationModePath, map[string]interface{}{
		"mode": "simple", "password": user.Password,
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusLocked, w.Code, w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "account_locked", body["error_code"])
	var mode string
	require.NoError(t, ts.DB.QueryRow(`SELECT revocation_mode FROM users WHERE id = $1`, user.ID).Scan(&mode))
	assert.NotEqual(t, "simple", mode, "a locked request changes nothing")
}
