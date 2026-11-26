package mfa_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/devicerecovery"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func recoveryV2Fixture(t *testing.T, ts *testhelpers.TestServer, userID string) (devicerecovery.Row, devicerecovery.RespondBody, devicerecovery.RespondBody) {
	t.Helper()
	b, err := os.ReadFile("../../../../docs/design/trusted-recovery-v2-vectors.json")
	require.NoError(t, err)
	var v struct {
		Context devicerecovery.Context `json:"context"`
		Offer   devicerecovery.Offer   `json:"offer"`
		Payload string                 `json:"encrypted_payload"`
	}
	require.NoError(t, json.Unmarshal(b, &v))
	_, err = ts.DB.Exec(`INSERT INTO trusted_recovery_devices(user_id,device_name,machine_id) VALUES($1,'device',$2)`, userID, uuid.NewString())
	require.NoError(t, err)
	r, err := devicerecovery.Create(context.Background(), ts.DB, userID, "public-jti", time.Now().Add(time.Hour), devicerecovery.CreateBody{ProtocolVersion: 2, ServerOrigin: v.Context.ServerOrigin, AccountBinding: devicerecovery.AccountBinding(userID), RequesterNonce: v.Context.RequesterNonce, RequesterPublicKey: v.Context.RequesterPublicKey}, func(context.Context, *sql.Tx) error { return nil })
	require.NoError(t, err)
	_, hash, err := devicerecovery.Transcript(r.Context, v.Offer)
	require.NoError(t, err)
	offer := devicerecovery.RespondBody{Action: "offer", ProtocolVersion: 2, ResponderPublicKey: v.Offer.ResponderPublicKey, ResponderNonce: v.Offer.ResponderNonce, TranscriptHash: devicerecovery.Encode(hash)}
	approve := devicerecovery.RespondBody{Action: "approve", ProtocolVersion: 2, TranscriptHash: offer.TranscriptHash, EncryptedPayload: v.Payload}
	return r, offer, approve
}

// actionV2Map emits exactly the fields allowed for an action, so null/empty
// fields of the internal Go transport struct never sneak into wire fixtures.
func actionV2Map(b devicerecovery.RespondBody) map[string]any {
	r := map[string]any{"action": b.Action, "protocol_version": 2}
	if b.Action == "offer" {
		r["responder_public_key"] = b.ResponderPublicKey
		r["responder_nonce"] = b.ResponderNonce
		r["transcript_hash"] = b.TranscriptHash
	}
	if b.Action == "approve" {
		r["transcript_hash"] = b.TranscriptHash
		r["encrypted_payload"] = b.EncryptedPayload
	}
	return r
}

// TestRespondRouteReturns403ForUntrustedSession proves only a session on a
// designated trusted device can answer, and an untrusted one changes nothing.
func TestRespondRouteReturns403ForUntrustedSession(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "recoveryuntrusted")
	r, offer, approve := recoveryV2Fixture(t, ts, user.ID)
	route := urlRecoveryRequests + "/" + r.RequestID + respondSuffix
	untrusted := testhelpers.AuthHeaders(user.AccessToken)
	for _, body := range []map[string]any{actionV2Map(offer), actionV2Map(approve), {"action": "reject", "protocol_version": 2}} {
		w := ts.DoRequest("POST", route, body, untrusted)
		require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	}
	var status string
	require.NoError(t, ts.DB.QueryRow(`SELECT status FROM recovery_requests WHERE id=$1`, r.RequestID).Scan(&status))
	require.Equal(t, "pending", status)

	testhelpers.TrustSession(t, ts.DB, user.ID, user.AccessToken)
	w := ts.DoRequest("POST", route, actionV2Map(offer), untrusted)
	require.Equal(t, http.StatusOK, w.Code, "control: the same session answers once its machine is designated: "+w.Body.String())
}

// TestListRouteReturnsEmptyForUntrustedSession keeps the client poll healthy:
// an ordinary session gets 200 and an empty list, not an error state.
func TestListRouteReturnsEmptyForUntrustedSession(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "recoverylistuntrusted")
	recoveryV2Fixture(t, ts, user.ID)
	w := ts.DoRequest("GET", urlRecoveryRequests, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.JSONEq(t, `{"requests":[]}`, w.Body.String())
}
