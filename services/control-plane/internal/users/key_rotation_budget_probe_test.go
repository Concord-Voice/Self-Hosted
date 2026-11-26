package users_test

// The password change and the E2EE key replacement charge the shared MFA
// budget only when the account has an inline factor, and refuse a factor
// that appears under the lock after a probe that found none.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/users"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
)

// kpHandler wires a users handler as router.go does, over fence.
func kpHandler(t *testing.T, ts *testhelpers.TestServer, fence *credepoch.Fence) *users.Handler {
	t.Helper()
	h := users.NewHandler(ts.DB, logger.NewWithWriter(io.Discard), nil, krMFAHandler(t, ts.DB, ts.Redis), nil, fence, nil)
	h.SetRedis(ts.Redis)
	h.SetPasswordVerifier(testhelpers.HashVerifier{DB: ts.DB})
	service := presencehistory.NewService(ts.DB, presencehistory.DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(immediatePresenceDelivery{}))
	h.SetPresenceHistory(service)
	return h
}

var kpRoutes = []struct {
	name string
	call func(*testing.T, *users.Handler, testhelpers.TestUser, string) *httptest.ResponseRecorder
}{
	{"password change", krChangePassword},
	{"key reset", krReplaceKeys},
}

// A request carrying a code for an account with no inline factor guesses
// nothing, so it must neither spend nor be refused by the budget.
func TestKeyRotationNoMFACodeDoesNotTouchTheBudget(t *testing.T) {
	for _, route := range kpRoutes {
		t.Run(route.name, func(t *testing.T) {
			ts := setupTS(t)
			user := ts.CreateTestUser(t, "kp_nomfa_"+route.name[:3])
			h := kpHandler(t, ts, testCredFence(t, ts.DB))
			key := stepup.DangerousActionBudget(ts.Redis).Key(user.ID)
			require.NoError(t, ts.Redis.Set(context.Background(), key, stepup.BudgetLimit, 0).Err())

			w := route.call(t, h, user, "123456")
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			n, err := ts.Redis.Get(context.Background(), key).Int()
			require.NoError(t, err)
			assert.Equal(t, stepup.BudgetLimit, n, "a no-MFA request must not charge the budget")
		})
	}
}

// kpEnrollOnFenceHook enrolls a WebAuthn factor the first time the credential
// fence touches Redis: after the pre-transaction budget probe found no factor,
// and before the transaction takes the users-row lock.
type kpEnrollOnFenceHook struct {
	once   *sync.Once
	enroll func()
}

func (kpEnrollOnFenceHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h kpEnrollOnFenceHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		h.once.Do(h.enroll)
		return next(ctx, cmd)
	}
}

func (h kpEnrollOnFenceHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		h.once.Do(h.enroll)
		return next(ctx, cmds)
	}
}

func kpEnrollingFence(t *testing.T, db *sql.DB, userID string) *credepoch.Fence {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	rdb.AddHook(kpEnrollOnFenceHook{once: &sync.Once{}, enroll: func() {
		_, err := db.Exec(`INSERT INTO user_mfa_webauthn (id, user_id, credential_id, credential_name, credential_type, public_key, sign_count, created_at)
			VALUES (gen_random_uuid(), $1, $2, 'Key', 'hardware', '\x00', 0, NOW())`, userID, []byte("kp-cred-"+userID))
		if err == nil {
			_, err = db.Exec(`UPDATE users SET mfa_enabled = true, mfa_methods = ARRAY['webauthn'] WHERE id = $1`, userID)
		}
		assert.NoError(t, err)
	}})
	return credepoch.New(db, rdb, logger.NewWithWriter(io.Discard))
}

// A factor committed after the uncharged probe must be refused for a retry,
// not judged for free.
func TestKeyRotationFactorEnrolledAfterProbeIsRefused(t *testing.T) {
	for _, route := range kpRoutes {
		t.Run(route.name, func(t *testing.T) {
			ts := setupTS(t)
			user := ts.CreateTestUser(t, "kp_race_"+route.name[:3])
			h := kpHandler(t, ts, kpEnrollingFence(t, ts.DB, user.ID))
			hashBefore := krPasswordHash(t, ts.DB, user.ID)

			w := route.call(t, h, user, "000000")
			require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), "MFA enrollment changed")
			_, err := ts.Redis.Get(context.Background(), stepup.DangerousActionBudget(ts.Redis).Key(user.ID)).Result()
			assert.ErrorIs(t, err, redis.Nil, "the refused request was never charged")
			assert.Equal(t, hashBefore, krPasswordHash(t, ts.DB, user.ID))
		})
	}
}

// kpEnrollBackupCodeTOTP gives the user a confirmed TOTP factor sealed under
// krMFAHandler's keyring, with busyBackupCode as its one backup code, so the
// real verifier reaches backup-code hashing inside the users-row lock.
func kpEnrollBackupCodeTOTP(t *testing.T, db *sql.DB, userID string) {
	t.Helper()
	key, err := hex.DecodeString(krKeyringHex)
	require.NoError(t, err)
	sealed, nonce, err := mfa.EncryptSecret(bytes.Repeat([]byte("A"), 32), key)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed)
		VALUES ($1, $2, $3, 1, TRUE, TRUE)`, userID, sealed, nonce)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE users SET mfa_enabled = TRUE, mfa_methods = ARRAY['totp']::TEXT[] WHERE id = $1`, userID)
	require.NoError(t, err)
	setBackupCode(t, db, userID, busyBackupCode)
}

// A busy backup-code check under the users-row lock judged nothing: the
// password change and the key reset answer 503 with Retry-After: 1, change
// nothing, and refund the shared budget after the transaction has ended, so
// repeated busy answers never reach 429. A wrong code then costs exactly one
// attempt, and the right code completes the change.
func TestKeyRotationBusyBackupCheckRefundsTheBudget(t *testing.T) {
	for _, route := range kpRoutes {
		t.Run(route.name, func(t *testing.T) {
			ts := setupTS(t)
			user := ts.CreateTestUser(t, "kp_busy_"+route.name[:3])
			kpEnrollBackupCodeTOTP(t, ts.DB, user.ID)
			h := kpHandler(t, ts, testCredFence(t, ts.DB))
			key := stepup.DangerousActionBudget(ts.Redis).Key(user.ID)
			hashBefore := krPasswordHash(t, ts.DB, user.ID)

			release := holdBackupHashSlots(t)
			for i := 0; i <= stepup.BudgetLimit; i++ {
				before := redisCount(t, ts.Redis, key)
				requireVerificationBusy(t, route.call(t, h, user, busyBackupCode))
				require.Equal(t, before, redisCount(t, ts.Redis, key), "a busy check must not cost an attempt")
				require.Equal(t, hashBefore, krPasswordHash(t, ts.DB, user.ID), "a busy check changes nothing")
			}
			release()

			w := route.call(t, h, user, "00000000")
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			require.Equal(t, 1, redisCount(t, ts.Redis, key), "a wrong code costs exactly one attempt")

			w = route.call(t, h, user, busyBackupCode)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		})
	}
}
