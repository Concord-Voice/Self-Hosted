package auth_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/devicerecovery"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Regression suite for VULN-019: answering a trusted-device recovery request
// requires a session that belongs to a designated trusted device, not merely
// any live session of the account.

const respondRoutePrefix = "/api/v1/mfa/recovery-requests/"

// trustedDeviceSession returns an access token for a second session of user
// whose refresh row carries the machine id of a designated trusted device.
func trustedDeviceSession(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser) string {
	t.Helper()
	machineID := uuid.NewString()
	_, err := ts.DB.Exec(`INSERT INTO trusted_recovery_devices(user_id,device_name,machine_id) VALUES($1,'trusted',$2)`, user.ID, machineID)
	require.NoError(t, err)
	token := ts.RenewedAccessToken(t, user.ID)
	sid := testhelpers.AccessIdentityForToken(t, token).SID
	_, err = ts.DB.Exec(`INSERT INTO refresh_tokens(id,user_id,token_hash,expires_at,machine_id) VALUES($1,$2,$3,$4,$5)`,
		sid, user.ID, uuid.NewString(), time.Now().Add(time.Hour), machineID)
	require.NoError(t, err)
	return token
}

type pendingDeviceRequest struct {
	devicerecovery.Context
	Status string `json:"status"`
}

func openPendingRequest(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser) pendingDeviceRequest {
	t.Helper()
	create := ts.DoRequest(http.MethodPost, pathDeviceReqCreate, deviceCreateV2(t, user.ID, recoveryTokenFor(t, ts, user)), nil)
	require.Equal(t, http.StatusOK, create.Code, create.Body.String())
	var pending pendingDeviceRequest
	testhelpers.ParseJSON(t, create, &pending)
	require.Equal(t, "pending", pending.Status)
	return pending
}

func requestStatus(t *testing.T, ts *testhelpers.TestServer, id string) string {
	t.Helper()
	var status string
	require.NoError(t, ts.DB.QueryRow(`SELECT status FROM recovery_requests WHERE id = $1`, id).Scan(&status))
	return status
}

func TestUntrustedSessionCannotAnswerDeviceRecoveryRequest(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "untrustedresp")
	trusted := trustedDeviceSession(t, ts, user)
	pending := openPendingRequest(t, ts, user)
	_, offer, payload := deviceFixture(t)
	_, h, err := devicerecovery.Transcript(pending.Context, offer)
	require.NoError(t, err)
	digest := devicerecovery.Encode(h)
	route := respondRoutePrefix + pending.RequestID + "/respond"
	offerBody := map[string]any{"action": "offer", "protocol_version": 2, "responder_public_key": offer.ResponderPublicKey, "responder_nonce": offer.ResponderNonce, "transcript_hash": digest}

	// user.AccessToken's session has no designated device behind it.
	for _, body := range []map[string]any{
		offerBody,
		{"action": "approve", "protocol_version": 2, "transcript_hash": digest, "encrypted_payload": payload},
		{"action": "reject", "protocol_version": 2},
	} {
		w := ts.DoRequest(http.MethodPost, route, body, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusForbidden, w.Code, "action %v: %s", body["action"], w.Body.String())
	}
	require.Equal(t, "pending", requestStatus(t, ts, pending.RequestID), "an untrusted session must not move the request")

	// Control: the designated device's session still completes the exchange.
	w := ts.DoRequest(http.MethodPost, route, offerBody, testhelpers.AuthHeaders(trusted))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w = ts.DoRequest(http.MethodPost, route, map[string]any{"action": "approve", "protocol_version": 2, "transcript_hash": digest, "encrypted_payload": payload}, testhelpers.AuthHeaders(trusted))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestUntrustedSessionSeesNoDeviceRecoveryRequests(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "untrustedlist")
	trusted := trustedDeviceSession(t, ts, user)
	pending := openPendingRequest(t, ts, user)

	w := ts.DoRequest(http.MethodGet, "/api/v1/mfa/recovery-requests", nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.JSONEq(t, `{"requests":[]}`, w.Body.String())

	w = ts.DoRequest(http.MethodGet, "/api/v1/mfa/recovery-requests", nil, testhelpers.AuthHeaders(trusted))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), pending.RequestID)
}

func TestRemovedTrustedDeviceCannotAnswerDeviceRecoveryRequest(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "removedresp")
	trusted := trustedDeviceSession(t, ts, user)
	pending := openPendingRequest(t, ts, user)
	_, err := ts.DB.Exec(`DELETE FROM trusted_recovery_devices WHERE user_id = $1`, user.ID)
	require.NoError(t, err)
	w := ts.DoRequest(http.MethodPost, respondRoutePrefix+pending.RequestID+"/respond", map[string]any{"action": "reject", "protocol_version": 2}, testhelpers.AuthHeaders(trusted))
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	require.Equal(t, "pending", requestStatus(t, ts, pending.RequestID))
}
