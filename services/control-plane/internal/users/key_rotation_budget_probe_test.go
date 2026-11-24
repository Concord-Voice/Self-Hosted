package users_test

// The password change and the E2EE key replacement charge the shared MFA
// budget only when the account has an inline factor, and refuse a factor
// that appears under the lock after a probe that found none.

import (
	"context"
	"database/sql"
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
