package dm_test

// The purge step-up password counts toward the account's shared /auth/login
// lockout, so a stolen session cannot guess the password here without the
// five-guess limit (CWE-307).

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// resetAttemptLimits clears userID's limiter and step-up budget keys, so
// neither answers 429 before the lockout is reached.
func resetAttemptLimits(t *testing.T, ts *testhelpers.TestServer, userID string) {
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
// attempt with the correct password would purge with 200).
func TestPurgeConversation_FiveWrongPasswordsLockTheAccount(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "su_lock_alice")
	bob := ts.CreateTestUser(t, "su_lock_bob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)
	insertDMMsg(t, ts, convID, alice.ID, "keep-me")

	purge := func(password string) (int, map[string]interface{}) {
		resetAttemptLimits(t, ts, alice.ID)
		w := ts.DoRequest(http.MethodDelete, purgeConvPath(convID),
			map[string]any{"range": "all", "current_password": password},
			testhelpers.AuthHeaders(alice.AccessToken))
		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		return w.Code, body
	}

	for i := range 5 {
		code, _ := purge("wrong-password-123")
		require.Equal(t, http.StatusForbidden, code, "guess %d", i+1)
	}

	code, body := purge(alice.Password)
	assert.Equal(t, http.StatusLocked, code, "the lockout refuses the correct password too")
	assert.Equal(t, "account_locked", body["error_code"])
	assert.Equal(t, 1, countDMMessages(t, ts, convID), "a locked account purges nothing")
}
