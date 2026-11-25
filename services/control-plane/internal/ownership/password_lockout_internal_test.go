package ownership

// verifyPassword's lockout branches that need a Redis client of their own: a
// fault must refuse as a 500 (never a pass, never a lock), and a lock must
// emit the lockout security event.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/backuphash"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/stepup"
	dbtest "github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
)

func lockoutTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/x", nil)
	return c, w
}

func TestVerifyPassword_RedisFaultIs500(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	userID := dbtest.CreateUser(t, db).String()
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	mini.Close()
	h := &Handler{db: db, log: logger.New("ownership-lockout"), redis: rdb}
	c, w := lockoutTestContext()

	err := h.verifyPassword(c, userID, "any-guess")

	require.Error(t, err, "an uncountable guess must never pass")
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.Contains(t, w.Body.String(), errMsgFailedVerifyPassword)
	_, marked := middleware.NightwatchVerdictFromContext(c)
	require.False(t, marked, "a Redis fault is not a lock")
}

func TestVerifyPassword_LockedEmitsTheLockoutEvent(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	uid := dbtest.CreateUser(t, db)
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	require.NoError(t, rdb.Set(context.Background(), "login_lockout:"+uid.String()+"@test.local", "1", time.Hour).Err())
	h := &Handler{db: db, log: logger.New("ownership-lockout"), redis: rdb}
	c, w := lockoutTestContext()

	require.Error(t, h.verifyPassword(c, uid.String(), "any-guess"))

	require.Equal(t, http.StatusLocked, w.Code)
	verdict, ok := middleware.NightwatchVerdictFromContext(c)
	require.True(t, ok, "a 423 marks the lockout verdict")
	require.Equal(t, securityevent.ReasonAccountLocked, verdict.Reason)
}

// busyOwnershipMFAVerifier answers busy while busy is set, then judges codes:
// acceptCode verifies, anything else is wrong.
type busyOwnershipMFAVerifier struct {
	busy       bool
	acceptCode string
}

func (*busyOwnershipMFAVerifier) IsEnabled(context.Context, string) bool { return true }
func (v *busyOwnershipMFAVerifier) VerifyCode(_ context.Context, _ string, _ stepup.Purpose, code string) (bool, error) {
	if v.busy {
		return false, fmt.Errorf("verify: %w", backuphash.ErrBusy)
	}
	return code == v.acceptCode, nil
}
func (*busyOwnershipMFAVerifier) GetEnabledMethods(context.Context, string) ([]string, error) {
	return []string{"totp"}, nil
}

// A busy backup-code check judged nothing, so verifyMFA answers 503 with
// Retry-After: 1 and refunds the shared budget: repeated busy answers never
// reach 429, while a wrong code still costs exactly one attempt.
func TestVerifyMFA_BusyVerifierRefundsSharedBudget(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	userID := dbtest.CreateUser(t, db).String()
	_, err := db.Exec(`INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, enabled, confirmed)
		VALUES ($1, '\x00'::bytea, '\x00'::bytea, TRUE, TRUE)`, userID)
	require.NoError(t, err)
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	verifier := &busyOwnershipMFAVerifier{busy: true, acceptCode: "123456"}
	h := &Handler{db: db, log: logger.New("ownership-busy"), redis: rdb, mfaVerifier: verifier}
	key := stepup.DangerousActionBudget(rdb).Key(userID)
	budget := func() int {
		n, err := rdb.Get(context.Background(), key).Int()
		if errors.Is(err, redis.Nil) {
			return 0
		}
		require.NoError(t, err)
		return n
	}

	for i := 0; i <= stepup.BudgetLimit; i++ {
		before := budget()
		c, w := lockoutTestContext()
		verified, err := h.verifyMFA(c, userID, stepup.PurposeOwnershipTransfer, "BACKUP01")
		require.Error(t, err)
		require.False(t, verified)
		require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
		require.Equal(t, "1", w.Header().Get("Retry-After"))
		require.JSONEq(t, `{"error":"Verification is busy. Try again.","verification_busy":true}`, w.Body.String())
		require.Equal(t, before, budget(), "a busy check must not cost an attempt")
	}

	verifier.busy = false
	c, w := lockoutTestContext()
	_, err = h.verifyMFA(c, userID, stepup.PurposeOwnershipTransfer, "000000")
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	require.Equal(t, 1, budget(), "a wrong code costs exactly one attempt")

	c, w = lockoutTestContext()
	verified, err := h.verifyMFA(c, userID, stepup.PurposeOwnershipTransfer, "123456")
	require.NoError(t, err, w.Body.String())
	require.True(t, verified)
}
