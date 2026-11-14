package ownership_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOwnershipConfirmStepUpRejectsSessionOnly(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "confirm-session-only-owner")
	target := ts.CreateTestUser(t, "confirm-session-only-target")
	serverID := ts.CreateTestServer(t, owner.ID, "Confirm Session Only")
	ts.AddMemberToServer(t, serverID, target.ID, keyMember)

	w := ts.DoRequest(http.MethodPost, pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: target.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	// A session alone is not a fresh proof. The pending row remains intact.
	w = ts.DoRequest(http.MethodPost, pathServersPrefix+serverID+pathTransferOwnershipConfirm, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assertOwnershipTransferPending(t, ts, serverID, owner.ID)
}

func TestOwnershipConfirm_ExhaustedMFABudgetDoesNotReservePassword(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "confirm-budget-before-password-owner")
	target := ts.CreateTestUser(t, "confirm-budget-before-password-target")
	serverID := ts.CreateTestServer(t, owner.ID, "Confirm Budget Before Password")
	ts.AddMemberToServer(t, serverID, target.ID, keyMember)

	w := ts.DoRequest(http.MethodPost, pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: target.ID, keyPassword: testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	enableMFAForUser(t, ts, owner.ID)
	require.NoError(t, ts.Redis.Set(context.Background(), stepup.DangerousActionBudget(ts.Redis).Key(owner.ID), stepup.BudgetLimit, stepup.BudgetWindow).Err())

	for i := 0; i < stepup.BudgetLimit; i++ {
		keys, err := ts.Redis.Keys(context.Background(), "ratelimit:*"+owner.ID+"*").Result()
		require.NoError(t, err)
		if len(keys) > 0 {
			require.NoError(t, ts.Redis.Del(context.Background(), keys...).Err())
		}
		w = ts.DoRequest(http.MethodPost, pathServersPrefix+serverID+pathTransferOwnershipConfirm, map[string]interface{}{
			keyPassword: owner.Password, "mfa_code": "rejected-code",
		}, testhelpers.AuthHeaders(owner.AccessToken))
		require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
	}

	require.False(t, ts.Redis.Exists(context.Background(), "login_attempts:"+owner.Email).Val() > 0,
		"an MFA-budget denial must not reserve an unchecked password guess")
	login := ts.DoRequest(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": owner.Email, "password": owner.Password,
	}, nil)
	require.Equal(t, http.StatusOK, login.Code, login.Body.String())
	assertOwnershipTransferPending(t, ts, serverID, owner.ID)
}

func TestOwnershipConfirmStepUpRequiresConfirmationPurpose(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "confirm-purpose-owner")
	target := ts.CreateTestUser(t, "confirm-purpose-target")
	serverID := ts.CreateTestServer(t, owner.ID, "Confirm Purpose")
	ts.AddMemberToServer(t, serverID, target.ID, keyMember)
	enableMFAForUser(t, ts, owner.ID)

	initiationProof := seedMFAInlineToken(t, ts, owner.ID, stepup.PurposeOwnershipTransfer)
	w := ts.DoRequest(http.MethodPost, pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: target.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
		"mfa_code":      initiationProof,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	// This separate, unspent initiation-purpose proof must not authorize
	// confirmation, even though it belongs to the same session and actor.
	wrongPurposeProof := seedMFAInlineToken(t, ts, owner.ID, stepup.PurposeOwnershipTransfer)
	w = ts.DoRequest(http.MethodPost, pathServersPrefix+serverID+pathTransferOwnershipConfirm, map[string]interface{}{
		keyPassword: testhelpers.TestAuthPlaintext,
		"mfa_code":  wrongPurposeProof,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assertOwnershipTransferPending(t, ts, serverID, owner.ID)

	confirmationProof := seedMFAInlineToken(t, ts, owner.ID, stepup.PurposeOwnershipTransferConfirm)
	w = ts.DoRequest(http.MethodPost, pathServersPrefix+serverID+pathTransferOwnershipConfirm, map[string]interface{}{
		keyPassword: testhelpers.TestAuthPlaintext,
		"mfa_code":  confirmationProof,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func assertOwnershipTransferPending(t *testing.T, ts *testhelpers.TestServer, serverID, ownerID string) {
	t.Helper()
	var currentOwner, status string
	require.NoError(t, ts.DB.QueryRow(`SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&currentOwner))
	require.NoError(t, ts.DB.QueryRow(`SELECT status FROM ownership_transfers WHERE server_id = $1`, serverID).Scan(&status))
	assert.Equal(t, ownerID, currentOwner)
	assert.Equal(t, keyPending, status)
}
