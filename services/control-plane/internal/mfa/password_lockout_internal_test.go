package mfa

// The step-up password's lockout is split around each gated transaction:
// reserved before BeginTx, checked under the row lock, recorded after the
// transaction ends, and cleared only after a verified success committed. These
// drive the handlers on a private miniredis so a Redis fault or a lock can be
// set up without touching the shared test server.

import (
	"context"
	"database/sql"
	"net/http"
	"slices"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

const iuWrongPassword = "NotTheIuPassword9!" // pragma: allowlist secret

// iuLockoutRedis is a private miniredis and its client.
func iuLockoutRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, mr
}

// iuDownRedis is a client whose server is already gone.
func iuDownRedis(t *testing.T) *redis.Client {
	t.Helper()
	rdb, mr := iuLockoutRedis(t)
	mr.Close()
	return rdb
}

func iuLockoutKey(userID string) string { return "login_attempts:" + userID + "@iu-stepup.test" }

func iuLockKey(userID string) string { return "login_lockout:" + userID + "@iu-stepup.test" }

// iuFailLockoutModeHook fails only the lockout script calls in one mode
// ("checked" or "clear"), so the reserve and the budget still succeed and the
// post-transaction step is the one that faults.
type iuFailLockoutModeHook struct{ mode string }

func (iuFailLockoutModeHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (iuFailLockoutModeHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (f iuFailLockoutModeHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if slices.Contains(cmd.Args(), any(f.mode)) {
			return assert.AnError
		}
		return next(ctx, cmd)
	}
}

func iuLockoutEvents(h *Handler) *securityEventRecorder {
	rec := &securityEventRecorder{}
	h.SetSecurityEvents(rec)
	return rec
}

func iuHasReason(rec *securityEventRecorder, reason securityevent.ReasonCode) bool {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return slices.ContainsFunc(rec.events, func(e securityevent.Event) bool { return e.ReasonCode == reason })
}

func iuTOTPEnrolled(t *testing.T, db *sql.DB, userID string) bool {
	t.Helper()
	var enrolled bool
	require.NoError(t, db.QueryRow(`SELECT EXISTS(SELECT 1 FROM user_mfa_totp WHERE user_id = $1)`, userID).Scan(&enrolled))
	return enrolled
}

func TestTOTPDisable_RedisDownFailsClosedWith503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	userID := iuCreateUser(t, db, iuPassword)
	secret := iuEnrollTOTP(t, db, kr, userID)
	h := iuHandler(db, iuDownRedis(t), kr)
	c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/totp/disable",
		`{"password":"`+iuPassword+`","code":"`+iuTOTPCode(t, secret)+`"}`, userID, "")

	h.TOTPDisable(c)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Equal(t, stepup.ErrMsgBudgetUnavailable, iuBody(t, w)["error"])
	assert.Equal(t, true, iuBody(t, w)["step_up_budget_unavailable"])
	assert.True(t, iuTOTPEnrolled(t, db, userID), "an uncountable guess removes nothing")
}

func TestWebAuthnDelete_RedisDownIs500(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	userID := iuCreateUser(t, db, iuPassword)
	iuAddWebAuthnCredential(t, db, userID)
	credID := iuWebAuthnCredentialID(t, db, userID)
	h := iuHandler(db, iuDownRedis(t), kr)
	c, w := iuGinContext(http.MethodDelete, "/api/v1/mfa/webauthn/credentials/"+credID,
		`{"password":"`+iuPassword+`"}`, userID, "")
	c.Params = gin.Params{{Key: "id", Value: credID}}

	h.WebAuthnDeleteCredential(c)

	assert.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	assert.Equal(t, 1, iuCountRows(t, db, "user_mfa_webauthn", userID), "an uncountable guess removes nothing")
}

func TestTOTPDisable_LockedAccountIs423WithTheLockoutEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	userID := iuCreateUser(t, db, iuPassword)
	secret := iuEnrollTOTP(t, db, kr, userID)
	rdb, mr := iuLockoutRedis(t)
	require.NoError(t, mr.Set(iuLockKey(userID), "1"))
	h := iuHandler(db, rdb, kr)
	rec := iuLockoutEvents(h)
	c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/totp/disable",
		`{"password":"`+iuPassword+`","code":"`+iuTOTPCode(t, secret)+`"}`, userID, "")

	h.TOTPDisable(c)

	assert.Equal(t, http.StatusLocked, w.Code, w.Body.String())
	assert.Equal(t, "account_locked", iuBody(t, w)["error_code"])
	assert.True(t, iuHasReason(rec, securityevent.ReasonAccountLocked))
	assert.True(t, iuTOTPEnrolled(t, db, userID))
}

// The record runs after the rollback, and the clear only after the commit.
func TestTOTPDisable_RecordsAfterTheTxAndClearsAfterCommit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	userID := iuCreateUser(t, db, iuPassword)
	secret := iuEnrollTOTP(t, db, kr, userID)
	rdb, mr := iuLockoutRedis(t)
	h := iuHandler(db, rdb, kr)

	c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/totp/disable",
		`{"password":"`+iuWrongPassword+`","code":"`+iuTOTPCode(t, secret)+`"}`, userID, "")
	h.TOTPDisable(c)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	require.True(t, mr.Exists(iuLockoutKey(userID)), "the wrong guess is counted")

	c, w = iuGinContext(http.MethodPost, "/api/v1/mfa/totp/disable",
		`{"password":"`+iuPassword+`","code":"`+iuTOTPCode(t, secret)+`"}`, userID, "")
	h.TOTPDisable(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.False(t, mr.Exists(iuLockoutKey(userID)), "a committed, verified disable resets the count")
}

// A correct password resets the count even when the MFA leg then fails, as
// /auth/login does: an honest MFA typo must not count toward the sign-in
// lockout. The MFA leg stays limited by the step-up budget.
func TestTOTPDisable_WrongCodeAfterTheRightPasswordClearsTheCount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	userID := iuCreateUser(t, db, iuPassword)
	iuEnrollTOTP(t, db, kr, userID)
	rdb, mr := iuLockoutRedis(t)
	// Earlier wrong guesses, so the reset is visible, not just never created.
	require.NoError(t, mr.Set(iuLockoutKey(userID), "3"))
	h := iuHandler(db, rdb, kr)

	c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/totp/disable",
		`{"password":"`+iuPassword+`","code":"000000"}`, userID, "")
	h.TOTPDisable(c)

	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.False(t, mr.Exists(iuLockoutKey(userID)), "the matched password resets the lockout count")
	assert.False(t, mr.Exists(iuLockKey(userID)))
}

func TestSetBackupEmail_LockedAccountIs423(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	userID := iuCreateUser(t, db, iuPassword)
	rdb, mr := iuLockoutRedis(t)
	require.NoError(t, mr.Set(iuLockKey(userID), "1"))
	h := iuHandler(db, rdb, kr)
	rec := iuLockoutEvents(h)
	c, w := iuGinContext(http.MethodPut, "/api/v1/mfa/backup-email",
		`{"email":"backup@iu-stepup.test","password":"`+iuPassword+`"}`, userID, "")

	h.SetBackupEmail(c)

	assert.Equal(t, http.StatusLocked, w.Code, w.Body.String())
	assert.Equal(t, "account_locked", iuBody(t, w)["error_code"])
	assert.True(t, iuHasReason(rec, securityevent.ReasonAccountLocked))
	var backup sql.NullString
	require.NoError(t, db.QueryRow(`SELECT backup_email FROM users WHERE id = $1`, userID).Scan(&backup))
	assert.False(t, backup.Valid, "a locked step-up writes nothing")
}

// Five wrong guesses through the in-transaction gate lock the account: the
// record that runs after each rollback is what sets the lock.
func TestSetBackupEmail_FiveWrongPasswordsLockAfterTheTx(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	userID := iuCreateUser(t, db, iuPassword)
	rdb, mr := iuLockoutRedis(t)
	h := iuHandler(db, rdb, kr)

	for i := range 5 {
		mr.Del(mfaSettingsStepUpKey(userID))
		c, w := iuGinContext(http.MethodPut, "/api/v1/mfa/backup-email",
			`{"email":"backup@iu-stepup.test","password":"`+iuWrongPassword+`"}`, userID, "")
		h.SetBackupEmail(c)
		require.Equal(t, http.StatusForbidden, w.Code, "guess %d: %s", i+1, w.Body.String())
	}
	assert.True(t, mr.Exists(iuLockKey(userID)), "the fifth wrong guess sets the shared lock")
}

func TestSetBackupEmail_SuccessClearsTheCountAfterCommit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	userID := iuCreateUser(t, db, iuPassword)
	rdb, mr := iuLockoutRedis(t)
	h := iuHandler(db, rdb, kr)

	c, w := iuGinContext(http.MethodPut, "/api/v1/mfa/backup-email",
		`{"email":"backup@iu-stepup.test","password":"`+iuPassword+`"}`, userID, "")
	h.SetBackupEmail(c)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.False(t, mr.Exists(iuLockoutKey(userID)), "a committed, verified change resets the count")
}

// A record or clear that faults after the transaction is logged, not answered:
// the guess was already counted when it was reserved.
func TestSetBackupEmail_PostTxLockoutFaultsAreBestEffort(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := iuNewTestDB(t)
	kr := iuKeyring(t)

	for _, tc := range []struct {
		mode, password string
		want           int
	}{
		{mode: "checked", password: iuWrongPassword, want: http.StatusForbidden},
		{mode: "clear", password: iuPassword, want: http.StatusOK},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			userID := iuCreateUser(t, db, iuPassword)
			rdb, mr := iuLockoutRedis(t)
			rdb.AddHook(iuFailLockoutModeHook{mode: tc.mode})
			h := iuHandler(db, rdb, kr)
			c, w := iuGinContext(http.MethodPut, "/api/v1/mfa/backup-email",
				`{"email":"backup@iu-stepup.test","password":"`+tc.password+`"}`, userID, "")

			h.SetBackupEmail(c)

			assert.Equal(t, tc.want, w.Code, w.Body.String())
			assert.True(t, mr.Exists(iuLockoutKey(userID)), "the reserved guess stays counted")
		})
	}
}

func TestReserveStepUpPassword_DeletedAccountIs401(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := iuNewTestDB(t)
	rdb, _ := iuLockoutRedis(t)
	h := iuHandler(db, rdb, iuKeyring(t))
	c, w := iuGinContext(http.MethodPost, "/x", "", "", "")

	_, ok := h.reserveStepUpPassword(c, "00000000-0000-0000-0000-000000000000", iuPassword)

	assert.False(t, ok)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestReserveStage(t *testing.T) {
	assert.Equal(t, stageAccountLocked, reserveStage(stepup.LockedError()))
	assert.Equal(t, stageInternal, reserveStage(&stepup.Error{Status: http.StatusInternalServerError}))
	assert.Equal(t, stageSubjectGone, reserveStage(&stepup.Error{Status: http.StatusUnauthorized}))
}

// Both stageInternal arms of the shared decision table: a supplied password
// that was never reserved (it is refused, never checked) and an MFA leg that
// faults.
func TestVerifyStepUpFactors_StageInternal(t *testing.T) {
	copyText := stepup.Copy{NoFactors: "no factors", CredentialRequired: "credential required"} //nolint:gosec // G101: refusal copy

	e, stage := verifyStepUpFactors(nil, stepup.Subject{PasswordHash: "stored-hash"},
		mfaStepUpCredentials{Password: iuPassword}, copyText, func() *stepup.Error {
			t.Fatal("the MFA leg must not run after a refused password")
			return nil
		})
	require.NotNil(t, e)
	assert.Equal(t, stageInternal, stage)
	assert.ErrorIs(t, e, stepup.ErrPasswordNotReserved)

	e, stage = verifyStepUpFactors(nil, stepup.Subject{MFAEnabled: true},
		mfaStepUpCredentials{MFACode: "123456"}, copyText, func() *stepup.Error {
			return &stepup.Error{Status: http.StatusInternalServerError, Cause: assert.AnError}
		})
	require.NotNil(t, e)
	assert.Equal(t, stageInternal, stage)
}

// The pool-side gate records a wrong guess at once and clears only after
// every factor passed.
func TestRequirePasswordAndMFA_RecordsAndClears(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	userID := iuCreateUser(t, db, iuPassword)
	rdb, mr := iuLockoutRedis(t)
	h := iuHandler(db, rdb, kr)

	for i := range 5 {
		mr.Del(mfaSettingsStepUpKey(userID))
		c, w := iuGinContext(http.MethodPost, "/x", "", userID, "")
		_, ok := h.requirePasswordAndMFA(c, userID, mfaStepUpCredentials{Password: iuWrongPassword}, totpSetupStepUp)
		require.False(t, ok)
		require.Equal(t, http.StatusForbidden, w.Code, "guess %d", i+1)
	}
	require.True(t, mr.Exists(iuLockKey(userID)))

	other := iuCreateUser(t, db, iuPassword)
	c, _ := iuGinContext(http.MethodPost, "/x", "", other, "")
	_, ok := h.requirePasswordAndMFA(c, other, mfaStepUpCredentials{Password: iuWrongPassword}, totpSetupStepUp)
	require.False(t, ok)
	require.True(t, mr.Exists(iuLockoutKey(other)))
	c, _ = iuGinContext(http.MethodPost, "/x", "", other, "")
	_, ok = h.requirePasswordAndMFA(c, other, mfaStepUpCredentials{Password: iuPassword}, totpSetupStepUp)
	require.True(t, ok)
	assert.False(t, mr.Exists(iuLockoutKey(other)), "a verified step-up resets the count")
}
