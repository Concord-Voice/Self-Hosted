package mfaenforce_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/backuphash"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/mfaenforce"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
)

// TestRequire_BusyBackupCheckIsATypedRetryable503: with every backup-hash
// slot held, a v2 backup code reaches the real verifier and comes back as the
// typed busy refusal, which WriteError answers 503 with Retry-After: 1. The
// gate's caller refunds the charge off this type (stepup.RefundIfBusy). Once
// the slots free, the same code confirms.
func TestRequire_BusyBackupCheckIsATypedRetryable503(t *testing.T) {
	db := gateTestDB(t)
	verifier, ring := newVerifier(t, db)
	owner := createUser(t, db)
	enrollTOTP(t, db, ring, owner)
	server := createServer(t, db, owner, true)
	const code = "MFABUSY1"
	stored, err := backuphash.NewV2(context.Background(), code)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE user_mfa_totp SET backup_codes_hash = $1, backup_codes_used = $2 WHERE user_id = $3`,
		pq.Array([]string{stored}), pq.Array([]bool{false}), owner)
	require.NoError(t, err)
	in := mfaenforce.Confirm{Verifier: verifier, Purpose: stepup.PurposeMemberBan, Code: code}

	release := backuphash.HoldAllSlots(50 * time.Millisecond)
	out, e := requireOutcome(t, db, server, owner, in, true)
	release()
	require.Equal(t, mfaenforce.Unconfirmed, out)
	require.NotNil(t, e)
	require.True(t, stepup.IsBusy(e), "the refusal is typed busy, so the caller can refund")

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	mfaenforce.WriteError(c, logger.New("test"), e, nil)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Equal(t, "1", w.Header().Get("Retry-After"))
	require.JSONEq(t, `{"error":"Verification is busy. Try again.","verification_busy":true}`, w.Body.String())

	out, e = requireOutcome(t, db, server, owner, in, true)
	require.Nil(t, e)
	require.Equal(t, mfaenforce.Verified, out)
}
