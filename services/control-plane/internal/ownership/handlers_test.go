package ownership_test

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTS(t *testing.T) *testhelpers.TestServer {
	t.Helper()
	return testhelpers.SetupTestServer(t)
}

// --- InitiateTransfer ---

func TestInitiateTransfer_Success(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "xferowner1")
	member := ts.CreateTestUser(t, "xfermember1")
	serverID := ts.CreateTestServer(t, owner.ID, "Transfer Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership", map[string]interface{}{
		"target_user_id": member.ID,
		"password":       testhelpers.TestPassword,
	}, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "pending", body["status"])
	assert.Equal(t, member.ID, body["to_user_id"])
	assert.NotEmpty(t, body["transfer_id"])
	assert.NotEmpty(t, body["expires_at"])
}

func TestInitiateTransfer_NotOwner(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "xferowner2")
	member := ts.CreateTestUser(t, "xfermember2")
	other := ts.CreateTestUser(t, "xferother2")
	serverID := ts.CreateTestServer(t, owner.ID, "Transfer Server 2")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	ts.AddMemberToServer(t, serverID, other.ID, "member")

	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership", map[string]interface{}{
		"target_user_id": other.ID,
		"password":       testhelpers.TestPassword,
	}, testhelpers.AuthHeaders(member.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestInitiateTransfer_WrongPassword(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "xferowner3")
	member := ts.CreateTestUser(t, "xfermember3")
	serverID := ts.CreateTestServer(t, owner.ID, "Transfer Server 3")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership", map[string]interface{}{
		"target_user_id": member.ID,
		"password":       "WrongPassword999!",
	}, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestInitiateTransfer_TargetNotMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "xferowner4")
	nonMember := ts.CreateTestUser(t, "xfernonmem4")
	serverID := ts.CreateTestServer(t, owner.ID, "Transfer Server 4")

	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership", map[string]interface{}{
		"target_user_id": nonMember.ID,
		"password":       testhelpers.TestPassword,
	}, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestInitiateTransfer_TargetIsSelf(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "xferowner5")
	serverID := ts.CreateTestServer(t, owner.ID, "Transfer Server 5")

	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership", map[string]interface{}{
		"target_user_id": owner.ID,
		"password":       testhelpers.TestPassword,
	}, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestInitiateTransfer_AlreadyPending(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "xferowner6")
	member1 := ts.CreateTestUser(t, "xfermem6a")
	member2 := ts.CreateTestUser(t, "xfermem6b")
	serverID := ts.CreateTestServer(t, owner.ID, "Transfer Server 6")
	ts.AddMemberToServer(t, serverID, member1.ID, "member")
	ts.AddMemberToServer(t, serverID, member2.ID, "member")

	// First transfer succeeds
	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership", map[string]interface{}{
		"target_user_id": member1.ID,
		"password":       testhelpers.TestPassword,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	// Second transfer should 409
	w = ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership", map[string]interface{}{
		"target_user_id": member2.ID,
		"password":       testhelpers.TestPassword,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestInitiateTransfer_Unauthenticated(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "xferowner7")
	member := ts.CreateTestUser(t, "xfermem7")
	serverID := ts.CreateTestServer(t, owner.ID, "Transfer Server 7")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership", map[string]interface{}{
		"target_user_id": member.ID,
		"password":       testhelpers.TestPassword,
	}, nil)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// --- GetTransferStatus ---

func TestGetTransferStatus_Pending(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "statusowner1")
	member := ts.CreateTestUser(t, "statusmem1")
	serverID := ts.CreateTestServer(t, owner.ID, "Status Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// Initiate transfer
	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership", map[string]interface{}{
		"target_user_id": member.ID,
		"password":       testhelpers.TestPassword,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	// Owner sees full details
	w = ts.DoRequest("GET", "/api/v1/servers/"+serverID+"/transfer-ownership", nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	transfer := body["transfer"].(map[string]interface{})
	assert.Equal(t, "pending", transfer["status"])
	assert.Equal(t, member.ID, transfer["to_user_id"])
}

func TestGetTransferStatus_MemberSeesLimitedInfo(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "statusowner2")
	target := ts.CreateTestUser(t, "statusmem2")
	bystander := ts.CreateTestUser(t, "statusby2")
	serverID := ts.CreateTestServer(t, owner.ID, "Status Server 2")
	ts.AddMemberToServer(t, serverID, target.ID, "member")
	ts.AddMemberToServer(t, serverID, bystander.ID, "member")

	// Initiate transfer
	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership", map[string]interface{}{
		"target_user_id": target.ID,
		"password":       testhelpers.TestPassword,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	// Bystander should not see to_user_id
	w = ts.DoRequest("GET", "/api/v1/servers/"+serverID+"/transfer-ownership", nil, testhelpers.AuthHeaders(bystander.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	transfer := body["transfer"].(map[string]interface{})
	assert.Equal(t, "pending", transfer["status"])
	assert.Nil(t, transfer["to_user_id"])
}

func TestGetTransferStatus_NoPending(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "statusowner3")
	serverID := ts.CreateTestServer(t, owner.ID, "Status Server 3")

	w := ts.DoRequest("GET", "/api/v1/servers/"+serverID+"/transfer-ownership", nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Nil(t, body["transfer"])
}

func TestGetTransferStatus_NotMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "statusowner4")
	outsider := ts.CreateTestUser(t, "statusout4")
	serverID := ts.CreateTestServer(t, owner.ID, "Status Server 4")

	w := ts.DoRequest("GET", "/api/v1/servers/"+serverID+"/transfer-ownership", nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// --- CancelTransfer ---

func TestCancelTransfer_Success(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "cancelowner1")
	member := ts.CreateTestUser(t, "cancelmem1")
	serverID := ts.CreateTestServer(t, owner.ID, "Cancel Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// Initiate
	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership", map[string]interface{}{
		"target_user_id": member.ID,
		"password":       testhelpers.TestPassword,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	// Cancel
	w = ts.DoRequest("DELETE", "/api/v1/servers/"+serverID+"/transfer-ownership", nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify no pending transfer
	w = ts.DoRequest("GET", "/api/v1/servers/"+serverID+"/transfer-ownership", nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Nil(t, body["transfer"])
}

func TestCancelTransfer_NotOwner(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "cancelowner2")
	member := ts.CreateTestUser(t, "cancelmem2")
	serverID := ts.CreateTestServer(t, owner.ID, "Cancel Server 2")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// Initiate
	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership", map[string]interface{}{
		"target_user_id": member.ID,
		"password":       testhelpers.TestPassword,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	// Member tries to cancel
	w = ts.DoRequest("DELETE", "/api/v1/servers/"+serverID+"/transfer-ownership", nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestCancelTransfer_NoPending(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "cancelowner3")
	serverID := ts.CreateTestServer(t, owner.ID, "Cancel Server 3")

	w := ts.DoRequest("DELETE", "/api/v1/servers/"+serverID+"/transfer-ownership", nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// --- ConfirmTransfer ---

func TestConfirmTransfer_Success(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "confirmowner1")
	member := ts.CreateTestUser(t, "confirmmem1")
	serverID := ts.CreateTestServer(t, owner.ID, "Confirm Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// Initiate
	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership", map[string]interface{}{
		"target_user_id": member.ID,
		"password":       testhelpers.TestPassword,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	// Confirm
	w = ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership/confirm", nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, member.ID, body["new_owner_id"])

	// Verify owner_id changed in DB
	var newOwnerID string
	err := ts.DB.QueryRow(`SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&newOwnerID)
	require.NoError(t, err)
	assert.Equal(t, member.ID, newOwnerID)

	// Verify legacy roles swapped
	var oldOwnerRole, newOwnerRole string
	err = ts.DB.QueryRow(`SELECT role FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, owner.ID).Scan(&oldOwnerRole)
	require.NoError(t, err)
	assert.Equal(t, "member", oldOwnerRole)

	err = ts.DB.QueryRow(`SELECT role FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, member.ID).Scan(&newOwnerRole)
	require.NoError(t, err)
	assert.Equal(t, "owner", newOwnerRole)
}

func TestConfirmTransfer_NotOwner(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "confirmowner2")
	member := ts.CreateTestUser(t, "confirmmem2")
	serverID := ts.CreateTestServer(t, owner.ID, "Confirm Server 2")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// Initiate
	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership", map[string]interface{}{
		"target_user_id": member.ID,
		"password":       testhelpers.TestPassword,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	// Member tries to confirm
	w = ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership/confirm", nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestConfirmTransfer_NoPending(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "confirmowner3")
	serverID := ts.CreateTestServer(t, owner.ID, "Confirm Server 3")

	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership/confirm", nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// --- ConfirmTransfer: target left during pending window ---

func TestConfirmTransfer_TargetLeftServer(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "confirmowner4")
	member := ts.CreateTestUser(t, "confirmmem4")
	serverID := ts.CreateTestServer(t, owner.ID, "Confirm Server 4")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// Initiate
	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership", map[string]interface{}{
		"target_user_id": member.ID,
		"password":       testhelpers.TestPassword,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	// Remove the target member directly (simulating they left)
	_, err := ts.DB.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, member.ID)
	require.NoError(t, err)

	// Confirm should fail because target is no longer a member (409 Conflict)
	w = ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership/confirm", nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code)

	// Verify owner_id did NOT change (rollback)
	var currentOwnerID string
	err = ts.DB.QueryRow(`SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&currentOwnerID)
	require.NoError(t, err)
	assert.Equal(t, owner.ID, currentOwnerID)
}

// --- ReverseTransfer ---

func TestReverseTransfer_Success(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "reverseowner1")
	member := ts.CreateTestUser(t, "reversemem1")
	serverID := ts.CreateTestServer(t, owner.ID, "Reverse Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// Initiate + confirm
	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership", map[string]interface{}{
		"target_user_id": member.ID,
		"password":       testhelpers.TestPassword,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	w = ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership/confirm", nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Get the reversal token from DB
	var reversalToken string
	err := ts.DB.QueryRow(`SELECT reversal_token FROM ownership_transfers WHERE server_id = $1 AND status = 'completed'`, serverID).Scan(&reversalToken)
	require.NoError(t, err)

	// Reverse
	w = ts.DoRequest("POST", "/api/v1/ownership/reverse/"+reversalToken, map[string]interface{}{
		"password": testhelpers.TestPassword,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify owner_id reverted
	var revertedOwnerID string
	err = ts.DB.QueryRow(`SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&revertedOwnerID)
	require.NoError(t, err)
	assert.Equal(t, owner.ID, revertedOwnerID)
}

func TestReverseTransfer_ExpiredWindow(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "reverseowner2")
	member := ts.CreateTestUser(t, "reversemem2")
	serverID := ts.CreateTestServer(t, owner.ID, "Reverse Server 2")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// Initiate + confirm
	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership", map[string]interface{}{
		"target_user_id": member.ID,
		"password":       testhelpers.TestPassword,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	w = ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership/confirm", nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Manually set completed_at to 25 hours ago to simulate expired window
	_, err := ts.DB.Exec(
		`UPDATE ownership_transfers SET completed_at = NOW() - INTERVAL '25 hours' WHERE server_id = $1 AND status = 'completed'`,
		serverID,
	)
	require.NoError(t, err)

	var reversalToken string
	err = ts.DB.QueryRow(`SELECT reversal_token FROM ownership_transfers WHERE server_id = $1 AND status = 'completed'`, serverID).Scan(&reversalToken)
	require.NoError(t, err)

	// Reverse should fail
	w = ts.DoRequest("POST", "/api/v1/ownership/reverse/"+reversalToken, map[string]interface{}{
		"password": testhelpers.TestPassword,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusGone, w.Code)
}

func TestReverseTransfer_InvalidToken(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "reverseowner3")
	_ = ts.CreateTestServer(t, owner.ID, "Reverse Server 3")

	fakeToken := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	w := ts.DoRequest("POST", "/api/v1/ownership/reverse/"+fakeToken, map[string]interface{}{
		"password": testhelpers.TestPassword,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestReverseTransfer_WrongUser(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "reverseowner4")
	member := ts.CreateTestUser(t, "reversemem4")
	serverID := ts.CreateTestServer(t, owner.ID, "Reverse Server 4")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// Initiate + confirm
	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership", map[string]interface{}{
		"target_user_id": member.ID,
		"password":       testhelpers.TestPassword,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	w = ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership/confirm", nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var reversalToken string
	err := ts.DB.QueryRow(`SELECT reversal_token FROM ownership_transfers WHERE server_id = $1 AND status = 'completed'`, serverID).Scan(&reversalToken)
	require.NoError(t, err)

	// New owner (member) tries to reverse — should be forbidden
	w = ts.DoRequest("POST", "/api/v1/ownership/reverse/"+reversalToken, map[string]interface{}{
		"password": testhelpers.TestPassword,
	}, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// --- Hidden Owner Role in ListMembers ---

func TestListMembers_OwnerRoleMasked(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "maskowner1")
	member := ts.CreateTestUser(t, "maskmem1")
	serverID := ts.CreateTestServer(t, owner.ID, "Mask Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// Member views the member list — should not see "owner" role
	w := ts.DoRequest("GET", "/api/v1/servers/"+serverID+"/members", nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	members := body["members"].([]interface{})

	for _, m := range members {
		mem := m.(map[string]interface{})
		if mem["user_id"] == owner.ID {
			// Owner's role should NOT be "owner" when viewed by a non-owner
			assert.NotEqual(t, "owner", mem["role"], "Owner's role should be masked for non-owner viewers")
		}
	}
}

func TestListMembers_OwnerSeesOwnRole(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "maskowner2")
	member := ts.CreateTestUser(t, "maskmem2")
	serverID := ts.CreateTestServer(t, owner.ID, "Mask Server 2")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// Owner views the member list — should still see "owner" for themselves
	w := ts.DoRequest("GET", "/api/v1/servers/"+serverID+"/members", nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	members := body["members"].([]interface{})

	var ownerRole string
	for _, m := range members {
		mem := m.(map[string]interface{})
		if mem["user_id"] == owner.ID {
			ownerRole = mem["role"].(string)
		}
	}
	assert.Equal(t, "owner", ownerRole, "Owner should see their own 'owner' role")
}

// --- Auto-complete expired transfers ---

func TestAutoCompleteExpiredTransfer(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "autoowner1")
	member := ts.CreateTestUser(t, "automem1")
	serverID := ts.CreateTestServer(t, owner.ID, "Auto Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// Initiate a transfer
	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership", map[string]interface{}{
		"target_user_id": member.ID,
		"password":       testhelpers.TestPassword,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	// Manually set expires_at to the past (simulating 24h elapsed)
	_, err := ts.DB.Exec(
		`UPDATE ownership_transfers SET expires_at = NOW() - INTERVAL '1 hour' WHERE server_id = $1 AND status = 'pending'`,
		serverID,
	)
	require.NoError(t, err)

	// Directly execute the completion logic (simulating the cleanup job)
	completeExpiredTransfers(t, ts.DB, serverID, owner.ID, member.ID)

	// Verify owner_id changed
	var newOwnerID string
	err = ts.DB.QueryRow(`SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&newOwnerID)
	require.NoError(t, err)
	assert.Equal(t, member.ID, newOwnerID)

	// Verify transfer status
	var status string
	err = ts.DB.QueryRow(`SELECT status FROM ownership_transfers WHERE server_id = $1 ORDER BY requested_at DESC LIMIT 1`, serverID).Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, "completed", status)
}

// completeExpiredTransfers mimics the cleanup job logic for testing.
func completeExpiredTransfers(t *testing.T, db *sql.DB, _, _, _ string) {
	t.Helper()

	rows, err := db.QueryContext(context.Background(), `
		SELECT id, server_id, from_user_id, to_user_id
		FROM ownership_transfers
		WHERE status = 'pending' AND expires_at <= NOW()
	`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var xferID, srvID, fromUID, toUID string
		require.NoError(t, rows.Scan(&xferID, &srvID, &fromUID, &toUID))

		tx, err := db.BeginTx(context.Background(), nil)
		require.NoError(t, err)

		_, err = tx.Exec(`UPDATE ownership_transfers SET status = 'completed', completed_at = NOW() WHERE id = $1 AND status = 'pending'`, xferID)
		require.NoError(t, err)

		_, err = tx.Exec(`UPDATE servers SET owner_id = $1 WHERE id = $2`, toUID, srvID)
		require.NoError(t, err)

		_, err = tx.Exec(`UPDATE server_members SET role = 'member' WHERE server_id = $1 AND user_id = $2`, srvID, fromUID)
		require.NoError(t, err)

		_, err = tx.Exec(`UPDATE server_members SET role = 'owner' WHERE server_id = $1 AND user_id = $2`, srvID, toUID)
		require.NoError(t, err)

		require.NoError(t, tx.Commit())
	}
	require.NoError(t, rows.Err())
}

// --- Full lifecycle: initiate → cancel → re-initiate ---

func TestTransferLifecycle_CancelAndReinitiate(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "lcowner1")
	member := ts.CreateTestUser(t, "lcmem1")
	serverID := ts.CreateTestServer(t, owner.ID, "Lifecycle Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// Initiate
	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership", map[string]interface{}{
		"target_user_id": member.ID,
		"password":       testhelpers.TestPassword,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	// Cancel
	w = ts.DoRequest("DELETE", "/api/v1/servers/"+serverID+"/transfer-ownership", nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Re-initiate should succeed (no pending transfer)
	w = ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership", map[string]interface{}{
		"target_user_id": member.ID,
		"password":       testhelpers.TestPassword,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)
}

// --- Audit log ---

func TestTransferAuditLogged(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "auditowner1")
	member := ts.CreateTestUser(t, "auditmem1")
	serverID := ts.CreateTestServer(t, owner.ID, "Audit Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// Initiate
	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership", map[string]interface{}{
		"target_user_id": member.ID,
		"password":       testhelpers.TestPassword,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	// Check audit_log
	var count int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE server_id = $1 AND action = 'ownership_transfer_initiated'`,
		serverID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

// --- Permission cache invalidation ---

func TestTransferInvalidatesPermissionCache(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "cacheowner1")
	member := ts.CreateTestUser(t, "cachemem1")
	serverID := ts.CreateTestServer(t, owner.ID, "Cache Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	ctx := context.Background()

	// Seed permission cache for both users
	ownerCacheKey := "perm:" + serverID + ":" + owner.ID
	memberCacheKey := "perm:" + serverID + ":" + member.ID
	require.NoError(t, ts.Redis.Set(ctx, ownerCacheKey, "12345", 5*time.Minute).Err())
	require.NoError(t, ts.Redis.Set(ctx, memberCacheKey, "12345", 5*time.Minute).Err())

	// Initiate + confirm
	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership", map[string]interface{}{
		"target_user_id": member.ID,
		"password":       testhelpers.TestPassword,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	w = ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/transfer-ownership/confirm", nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Verify cache keys were invalidated
	_, err := ts.Redis.Get(ctx, ownerCacheKey).Result()
	assert.Error(t, err, "Owner's permission cache should be invalidated")
	_, err = ts.Redis.Get(ctx, memberCacheKey).Result()
	assert.Error(t, err, "Member's permission cache should be invalidated")
}
