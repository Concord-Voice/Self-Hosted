package ownership_test

// A busy backup-code check on transfer confirmation judged nothing: the shared
// MFA budget gets its attempt back after the coordinator transaction ends, and
// the transfer stays pending.

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/backuphash"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
)

const confirmBusyBackupCode = "MFABUSY1"

func TestOwnershipConfirm_BusyBackupCheckRefundsTheCharge(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "confirm-busy-owner")
	serverID := startOwnershipTransfer(t, ts, owner, "confirm-busy")

	// A real TOTP factor, sealed under the test router's all-zero keyring, with
	// one v2 backup code so the verifier reaches backup-code hashing.
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "Concord Voice", AccountName: owner.Email})
	require.NoError(t, err)
	sealed, nonce, err := mfa.EncryptSecret([]byte(key.Secret()), make([]byte, 32))
	require.NoError(t, err)
	stored, err := backuphash.NewV2(context.Background(), confirmBusyBackupCode)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`UPDATE users SET mfa_enabled = TRUE, mfa_methods = ARRAY['totp']::TEXT[] WHERE id = $1`, owner.ID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed, backup_codes_hash, backup_codes_used)
		VALUES ($1, $2, $3, 1, TRUE, TRUE, $4, $5)`, owner.ID, sealed, nonce, pq.Array([]string{stored}), pq.Array([]bool{false}))
	require.NoError(t, err)

	shared := stepup.DangerousActionBudget(ts.Redis).Key(owner.ID)
	sharedCount := func() int {
		n, err := ts.Redis.Get(context.Background(), shared).Int()
		if err != nil {
			return 0
		}
		return n
	}
	confirm := func(code string) (int, string, string) {
		// The route's own rate limit is not the question here.
		keys, err := ts.Redis.Keys(context.Background(), "ratelimit:*"+owner.ID+"*").Result()
		require.NoError(t, err)
		if len(keys) > 0 {
			require.NoError(t, ts.Redis.Del(context.Background(), keys...).Err())
		}
		w := ts.DoRequest(http.MethodPost, pathServersPrefix+serverID+pathTransferOwnershipConfirm, map[string]interface{}{
			keyPassword: owner.Password, "mfa_code": code,
		}, testhelpers.AuthHeaders(owner.AccessToken))
		return w.Code, w.Header().Get("Retry-After"), w.Body.String()
	}

	release := backuphash.HoldAllSlots(50 * time.Millisecond)
	var once sync.Once
	releaseOnce := func() { once.Do(release) }
	t.Cleanup(releaseOnce)
	// One more than the budget: without the refund the last call answers 429.
	for range stepup.BudgetLimit + 1 {
		code, retryAfter, body := confirm(confirmBusyBackupCode)
		require.Equal(t, http.StatusServiceUnavailable, code, body)
		require.Equal(t, "1", retryAfter)
		require.JSONEq(t, `{"error":"Verification is busy. Try again.","verification_busy":true}`, body)
		require.Equal(t, 0, sharedCount(), "a busy check must not cost a shared attempt")
		assertOwnershipTransferPending(t, ts, serverID, owner.ID)
	}
	releaseOnce()

	code, _, body := confirm("00000000")
	require.Equal(t, http.StatusForbidden, code, body)
	require.Equal(t, 1, sharedCount(), "a wrong code costs exactly one shared attempt")
	assertOwnershipTransferPending(t, ts, serverID, owner.ID)
}
