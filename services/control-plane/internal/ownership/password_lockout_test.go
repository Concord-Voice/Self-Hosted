package ownership_test

// The ownership-transfer password counts toward the account's shared
// /auth/login lockout, so a stolen session cannot guess the owner's password
// here without the five-guess limit (CWE-307).

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// Mutant killed: verifying with auth.VerifyPassword directly (the sixth
// attempt with the correct password would succeed with 201).
func TestInitiateTransfer_FiveWrongPasswordsLockTheAccount(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "xferlockowner")
	member := ts.CreateTestUser(t, "xferlockmember")
	serverID := ts.CreateTestServer(t, owner.ID, "Transfer Lockout Server")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	transfer := func(password string) (int, map[string]interface{}) {
		// The route's per-user limiter (3/min) would answer 429 before the
		// lockout is reached; reset only this user's limiter keys.
		keys, err := ts.Redis.Keys(context.Background(), "ratelimit:*"+owner.ID+"*").Result()
		require.NoError(t, err)
		if len(keys) > 0 {
			require.NoError(t, ts.Redis.Del(context.Background(), keys...).Err())
		}
		w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
			keyTargetUserID: member.ID,
			keyPassword:     password,
		}, testhelpers.AuthHeaders(owner.AccessToken))
		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		return w.Code, body
	}

	for i := range 5 {
		code, _ := transfer("WrongPassword999!")
		require.Equal(t, http.StatusForbidden, code, "guess %d", i+1)
	}

	code, body := transfer(testhelpers.TestAuthPlaintext)
	assert.Equal(t, http.StatusLocked, code, "the lockout refuses the correct password too")
	assert.Equal(t, "account_locked", body["error_code"])

	login := ts.DoRequest(http.MethodPost, "/api/v1/auth/login",
		map[string]string{"email": owner.Email, "password": owner.Password}, nil)
	assert.Equal(t, http.StatusUnauthorized, login.Code, "the same lock holds at /auth/login")
}

// lockAccount engages userID's shared /auth/login lock directly, under the
// email the lockout is keyed by.
func lockAccount(t *testing.T, ts *testhelpers.TestServer, userID string) {
	t.Helper()
	var email string
	require.NoError(t, ts.DB.QueryRow(`SELECT email FROM users WHERE id = $1`, userID).Scan(&email))
	require.NoError(t, ts.Redis.Set(context.Background(), "login_lockout:"+email, "1", time.Hour).Err())
}

// Mutant killed: the reverse route verifying without the lockout (a locked
// owner with the correct password would reverse with 200).
func TestReverseTransfer_LockedAccountIs423(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "reverselockowner")
	member := ts.CreateTestUser(t, "reverselockmem")
	serverID := ts.CreateTestServer(t, owner.ID, "Reverse Lockout Server")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: member.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	w = ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnershipConfirm, map[string]interface{}{keyPassword: testhelpers.TestAuthPlaintext}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var reversalToken string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT reversal_token FROM ownership_transfers WHERE server_id = $1 AND status = 'completed'`, serverID,
	).Scan(&reversalToken))

	lockAccount(t, ts, owner.ID)
	w = ts.DoRequest("POST", pathOwnershipReverse+reversalToken, map[string]interface{}{
		keyPassword: testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusLocked, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "account_locked", body["error_code"])

	var ownerID string
	require.NoError(t, ts.DB.QueryRow(`SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&ownerID))
	assert.Equal(t, member.ID, ownerID, "a locked reverse changes nothing")
}
