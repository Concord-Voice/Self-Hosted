package users_test

// The purge-fence step-up password counts toward the account's shared
// /auth/login lockout, so a stolen session cannot guess the password here
// without the five-guess limit (CWE-307).

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// resetStepUpLimits clears userID's limiter and step-up budget keys, so
// neither answers 429 before the lockout is reached.
func resetStepUpLimits(t *testing.T, ts *testhelpers.TestServer, userID string) {
	t.Helper()
	ctx := context.Background()
	for _, pattern := range []string{"ratelimit:*" + userID + "*", "stepup:*" + userID + "*"} {
		keys, err := ts.Redis.Keys(ctx, pattern).Result()
		require.NoError(t, err)
		if len(keys) > 0 {
			require.NoError(t, ts.Redis.Del(ctx, keys...).Err())
		}
	}
}

// Mutant killed: verifying with auth.VerifyPassword directly (the sixth
// attempt with the correct password would lower the fence with 200).
func TestUpdatePrivacy_FiveWrongPasswordsLockTheAccount(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "purgefencelock")
	materializePrivacyRow(t, ts, user)
	unmatchedValue := "NotTheRightValue123!" // bound, not inline — gosec G101

	for i := range 5 {
		resetStepUpLimits(t, ts, user.ID)
		w := patchPrivacy(t, ts, user.AccessToken, map[string]interface{}{
			keyRequireAuthBeforePurge: false,
			keyCurrentPassword:        unmatchedValue,
		})
		require.Equal(t, http.StatusForbidden, w.Code, "guess %d: %s", i+1, w.Body.String())
	}

	resetStepUpLimits(t, ts, user.ID)
	w := patchPrivacy(t, ts, user.AccessToken, map[string]interface{}{
		keyRequireAuthBeforePurge: false,
		keyCurrentPassword:        testhelpers.TestAuthPlaintext,
	})
	require.Equal(t, http.StatusLocked, w.Code, "the lockout refuses the correct password too: %s", w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	require.Equal(t, "account_locked", body["error_code"])
	require.True(t, storedPurgeFence(t, ts, user.ID), "a locked account must leave the fence up")
}

// The wrong guess is recorded after the transaction ends, and only a verified
// disable that committed resets the count.
func TestUpdatePrivacy_VerifiedDisableClearsTheCountAfterCommit(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "purgefenceclear")
	materializePrivacyRow(t, ts, user)
	unmatchedValue := "NotTheRightValue123!" // bound, not inline — gosec G101
	var email string
	require.NoError(t, ts.DB.QueryRow(`SELECT email FROM users WHERE id = $1`, user.ID).Scan(&email))
	ctx := context.Background()

	w := patchPrivacy(t, ts, user.AccessToken, map[string]interface{}{
		keyRequireAuthBeforePurge: false,
		keyCurrentPassword:        unmatchedValue,
	})
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	n, err := ts.Redis.Exists(ctx, "login_attempts:"+email).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "the wrong guess is counted")

	resetStepUpLimits(t, ts, user.ID)
	w = patchPrivacy(t, ts, user.AccessToken, map[string]interface{}{
		keyRequireAuthBeforePurge: false,
		keyCurrentPassword:        testhelpers.TestAuthPlaintext,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	n, err = ts.Redis.Exists(ctx, "login_attempts:"+email).Result()
	require.NoError(t, err)
	require.Zero(t, n, "a committed, verified disable resets the count")
}
