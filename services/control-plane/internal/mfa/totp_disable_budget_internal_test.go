package mfa

// TOTPDisable charges the shared MFA budget only when its unlocked probe
// finds a TOTP row, makes no Redis call while the users row is locked, and
// refuses a TOTP row that appeared between the probe and the lock.
// RegenerateBackupCodes fails closed when only the budget is unavailable.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/backuphash"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/stepup"
)

// tdOnFirstCommandHook runs fn once, before the client's first Redis command.
type tdOnFirstCommandHook struct {
	once *sync.Once
	fn   func()
}

func (tdOnFirstCommandHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h tdOnFirstCommandHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		h.once.Do(h.fn)
		return next(ctx, cmd)
	}
}

func (h tdOnFirstCommandHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		h.once.Do(h.fn)
		return next(ctx, cmds)
	}
}

// A TOTP row committed after the probe found none was never charged, so the
// locked recheck refuses it for a retry: 409, no budget key, no Retry-After.
func TestTOTPDisable_EnrollmentAfterPreflightIs409Uncharged(t *testing.T) {
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	rdb, mr := iuLockoutRedis(t)
	userID := iuCreateUser(t, db, iuPassword)
	rdb.AddHook(tdOnFirstCommandHook{once: &sync.Once{}, fn: func() { iuEnrollTOTP(t, db, kr, userID) }})
	h := iuHandler(db, rdb, kr)

	c, w := iuGinContext(http.MethodPost, "/mfa/totp/disable", fmt.Sprintf(`{"password":%q,"code":%q}`, iuPassword, "123456"), userID, "")
	h.TOTPDisable(c)

	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Equal(t, "TOTP enrollment changed; retry", iuBody(t, w)["error"])
	assert.Empty(t, w.Header().Get("Retry-After"))
	assert.False(t, mr.Exists(h.stepUpBudget().Key(userID)), "the refused request was never charged")
	var enrolled bool
	require.NoError(t, db.QueryRow(`SELECT EXISTS(SELECT 1 FROM user_mfa_totp WHERE user_id = $1)`, userID).Scan(&enrolled))
	assert.True(t, enrolled, "nothing was removed")
}

// tdLockWatchHook records any Redis command issued while another session holds
// the user's row lock, probing it with FOR UPDATE NOWAIT.
type tdLockWatchHook struct {
	db     *sql.DB
	userID string
	mu     *sync.Mutex
	locked *[]string
}

func (tdLockWatchHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h tdLockWatchHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		h.observe(cmd.Name())
		return next(ctx, cmd)
	}
}

func (h tdLockWatchHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		h.observe("pipeline")
		return next(ctx, cmds)
	}
}

func (h tdLockWatchHook) observe(name string) {
	tx, err := h.db.Begin()
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()
	var one int
	err = tx.QueryRow(`SELECT 1 FROM users WHERE id = $1 FOR UPDATE NOWAIT`, h.userID).Scan(&one)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		h.mu.Lock()
		*h.locked = append(*h.locked, name)
		h.mu.Unlock()
	}
}

// The behavioural form of the ordering contract: a TOTP disable, refused or
// successful, issues no Redis command while its users-row lock is held.
func TestTOTPDisable_NoRedisWhileTheUsersRowIsLocked(t *testing.T) {
	for _, tc := range []struct {
		name  string
		right bool
	}{{"wrong code", false}, {"right code", true}} {
		t.Run(tc.name, func(t *testing.T) {
			db := iuNewTestDB(t)
			kr := iuKeyring(t)
			rdb, _ := iuLockoutRedis(t)
			userID := iuCreateUser(t, db, iuPassword)
			secret := iuEnrollTOTP(t, db, kr, userID)
			var locked []string
			rdb.AddHook(tdLockWatchHook{db: db, userID: userID, mu: &sync.Mutex{}, locked: &locked})
			h := iuHandler(db, rdb, kr)
			code := "000000"
			if tc.right {
				code = iuTOTPCode(t, secret)
			}

			c, w := iuGinContext(http.MethodPost, "/mfa/totp/disable", fmt.Sprintf(`{"password":%q,"code":%q}`, iuPassword, code), userID, "")
			h.TOTPDisable(c)

			if tc.right {
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			} else {
				require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			}
			assert.Empty(t, locked, "Redis commands issued under the users-row lock")
		})
	}
}

// tdFailKeyHook fails every command that names key, leaving the rest of the
// client, the password lockout included, working.
type tdFailKeyHook struct{ key string }

func (tdFailKeyHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h tdFailKeyHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if slices.Contains(cmd.Args(), any(h.key)) {
			cmd.SetErr(assert.AnError)
			return assert.AnError
		}
		return next(ctx, cmd)
	}
}

func (tdFailKeyHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// With the password lockout working and only the budget key failing, a backup
// code regeneration is the budget's 503, not a 500 from the lockout.
func TestRegenerateBackupCodes_BudgetDownIs503(t *testing.T) {
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	rdb, _ := iuLockoutRedis(t)
	userID := iuCreateUser(t, db, iuPassword)
	secret := iuEnrollTOTP(t, db, kr, userID)
	h := iuHandler(db, rdb, kr)
	rdb.AddHook(tdFailKeyHook{key: h.stepUpBudget().Key(userID)})

	c, w := iuGinContext(http.MethodPost, "/mfa/backup-codes/regenerate", fmt.Sprintf(`{"password":%q,"code":%q}`, iuPassword, iuTOTPCode(t, secret)), userID, "")
	h.RegenerateBackupCodes(c)

	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Equal(t, stepup.ErrMsgBudgetUnavailable, iuBody(t, w)["error"])
	assert.Equal(t, true, iuBody(t, w)["step_up_budget_unavailable"])
}

// ── A busy backup-code check costs no attempt ──────────────────────────────
//
// Each case fills every backuphash slot, sends a code whose check must derive
// a v2 hash, and asserts a 503 with Retry-After: 1 and the charge refunded.
// A wrong code still costs one attempt, and the same request succeeds once
// the slots free.

// busyBackupUser enrolls TOTP with a real set of v2 backup codes.
func busyBackupUser(t *testing.T, db *sql.DB, kr *Keyring) (userID, secret string, codes []string) {
	t.Helper()
	userID = iuCreateUser(t, db, iuPassword)
	secret = iuEnrollTOTP(t, db, kr, userID)
	codes, hashes, err := GenerateBackupCodes(context.Background())
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE user_mfa_totp SET backup_codes_hash = $1, backup_codes_used = $2 WHERE user_id = $3`,
		pq.Array(hashes), pq.Array(make([]bool, len(hashes))), userID)
	require.NoError(t, err)
	return userID, secret, codes
}

// holdBackupHashSlots fills every slot until release or the end of the test.
func holdBackupHashSlots(t *testing.T) (release func()) {
	t.Helper()
	free := backuphash.HoldAllSlots(50 * time.Millisecond)
	var once sync.Once
	release = func() { once.Do(free) }
	t.Cleanup(release)
	return release
}

func requireBusy(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	require.Equal(t, "1", w.Header().Get("Retry-After"))
}

func requireCharge(t *testing.T, mr *miniredis.Miniredis, key, want string) {
	t.Helper()
	got, err := mr.Get(key)
	require.NoError(t, err, "the charge key must exist")
	require.Equal(t, want, got)
}

func TestBusyBackupCheck_TOTPDisableRefundsTheCharge(t *testing.T) {
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	rdb, mr := iuLockoutRedis(t)
	h := iuHandler(db, rdb, kr)
	userID, _, codes := busyBackupUser(t, db, kr)
	var locked []string
	rdb.AddHook(tdLockWatchHook{db: db, userID: userID, mu: &sync.Mutex{}, locked: &locked})
	t.Cleanup(func() { assert.Empty(t, locked, "Redis commands issued under the users-row lock") })
	disable := func(code string) *httptest.ResponseRecorder {
		c, w := iuGinContext(http.MethodPost, "/mfa/totp/disable", fmt.Sprintf(`{"password":%q,"code":%q}`, iuPassword, code), userID, "")
		h.TOTPDisable(c)
		return w
	}

	release := holdBackupHashSlots(t)
	for range stepup.BudgetLimit + 1 {
		w := disable(codes[0])
		requireBusy(t, w)
		assert.Equal(t, true, iuBody(t, w)["verification_busy"])
		requireCharge(t, mr, h.stepUpBudget().Key(userID), "0")
	}
	release()

	require.Equal(t, http.StatusForbidden, disable("WRONGCOD").Code)
	requireCharge(t, mr, h.stepUpBudget().Key(userID), "1")
	w := disable(codes[0])
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestBusyBackupCheck_RegenerateRefundsTheCharge(t *testing.T) {
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	rdb, mr := iuLockoutRedis(t)
	h := iuHandler(db, rdb, kr)
	userID, secret, _ := busyBackupUser(t, db, kr)
	regenerate := func(code string) *httptest.ResponseRecorder {
		c, w := iuGinContext(http.MethodPost, "/mfa/backup-codes/regenerate", fmt.Sprintf(`{"password":%q,"code":%q}`, iuPassword, code), userID, "")
		h.RegenerateBackupCodes(c)
		return w
	}

	release := holdBackupHashSlots(t)
	requireBusy(t, regenerate(iuTOTPCode(t, secret)))
	requireCharge(t, mr, h.stepUpBudget().Key(userID), "0")
	release()

	require.Equal(t, http.StatusForbidden, regenerate("000000").Code)
	requireCharge(t, mr, h.stepUpBudget().Key(userID), "1")
	w := regenerate(iuTOTPCode(t, secret))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestBusyBackupCheck_SettingsGatesRefundTheCharge(t *testing.T) {
	for _, route := range []struct {
		name string
		call func(h *Handler, c *gin.Context)
		body string
	}{
		{"pool gate (recovery hardened)", (*Handler).SetRecoveryHardened, `{"enabled":true,"password":%q,"mfa_code":%q}`},
		{"transaction gate (backup email)", (*Handler).SetBackupEmail, `{"email":"backup@iu-stepup.test","password":%q,"mfa_code":%q}`},
	} {
		t.Run(route.name, func(t *testing.T) {
			db := iuNewTestDB(t)
			kr := iuKeyring(t)
			rdb, mr := iuLockoutRedis(t)
			h := iuHandler(db, rdb, kr)
			userID, _, codes := busyBackupUser(t, db, kr)
			var locked []string
			rdb.AddHook(tdLockWatchHook{db: db, userID: userID, mu: &sync.Mutex{}, locked: &locked})
			t.Cleanup(func() { assert.Empty(t, locked, "Redis commands issued under the users-row lock") })
			send := func(code string) *httptest.ResponseRecorder {
				c, w := iuGinContext(http.MethodPost, "/mfa/settings", fmt.Sprintf(route.body, iuPassword, code), userID, "")
				route.call(h, c)
				return w
			}

			release := holdBackupHashSlots(t)
			for range stepup.BudgetLimit + 1 {
				w := send(codes[0])
				requireBusy(t, w)
				assert.Equal(t, true, iuBody(t, w)["verification_busy"])
				requireCharge(t, mr, h.stepUpBudget().Key(userID), "0")
			}
			release()

			require.Equal(t, http.StatusForbidden, send("WRONGCOD").Code)
			requireCharge(t, mr, h.stepUpBudget().Key(userID), "1")
			w := send(codes[0])
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		})
	}
}

func TestBusyBackupCheck_SignInRefundsTheAttempt(t *testing.T) {
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	rdb, mr := iuLockoutRedis(t)
	h := iuHandler(db, rdb, kr)
	completer := &recordingLoginCompleter{}
	h.SetLoginCompleter(completer)
	userID, _, codes := busyBackupUser(t, db, kr)
	token, _, err := h.GenerateLoginChallenge(context.Background(), userID, false, "", securityevent.AuthPassword)
	require.NoError(t, err)
	attemptsKey := fmt.Sprintf(redisKeyMFAVerifyAttempts, userID)
	verify := func(code string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/verify",
			strings.NewReader(`{"mfa_challenge_token":"`+token+`","method":"backup_code","code":"`+code+`"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		h.Verify(c)
		return w
	}

	release := holdBackupHashSlots(t)
	for range failedAttemptLimit + 1 {
		requireBusy(t, verify(codes[0]))
		requireCharge(t, mr, attemptsKey, "0")
	}
	release()

	require.Equal(t, http.StatusForbidden, verify("WRONGCOD").Code)
	requireCharge(t, mr, attemptsKey, "1")
	verify(codes[0])
	require.True(t, completer.called, "the right code signs in once the slots free")
}

// A busy sign-in can wait past the end of the window it was counted in. Its
// refund must not take back a wrong code counted in the next window: if it
// did, one busy request straddling each boundary would keep every window
// below the fifth wrong code, and the lockout would never arm.
func TestBusyBackupCheck_SignInRefundAfterTheWindowStillLetsTheLockoutArm(t *testing.T) {
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	rdb, mr := iuLockoutRedis(t)
	h := iuHandler(db, rdb, kr)
	h.SetLoginCompleter(&recordingLoginCompleter{})
	userID, _, codes := busyBackupUser(t, db, kr)
	token, _, err := h.GenerateLoginChallenge(context.Background(), userID, false, "", securityevent.AuthPassword)
	require.NoError(t, err)
	attemptsKey := fmt.Sprintf(redisKeyMFAVerifyAttempts, userID)
	lockoutKey := fmt.Sprintf(redisKeyMFAVerifyLockout, userID)
	verify := func(method, code string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/verify",
			strings.NewReader(`{"mfa_challenge_token":"`+token+`","method":"`+method+`","code":"`+code+`"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		h.Verify(c)
		return w
	}

	// The busy request waits up to two seconds for a slot: long enough for
	// the window to end and five wrong codes to arrive in the next one.
	free := backuphash.HoldAllSlots(2 * time.Second)
	t.Cleanup(free)
	busy := make(chan *httptest.ResponseRecorder, 1)
	go func() { busy <- verify("backup_code", codes[0]) }()
	require.Eventually(t, func() bool { got, _ := mr.Get(attemptsKey); return got == "1" },
		time.Second, time.Millisecond, "the busy request is counted")

	mr.FastForward(verifyAttemptWindow + time.Second)
	for range failedAttemptLimit - 1 {
		require.Equal(t, http.StatusForbidden, verify("totp", "ZZZZZZ").Code)
	}
	requireBusy(t, <-busy) // its refund runs now, in the next window
	requireCharge(t, mr, attemptsKey, "4")

	require.Equal(t, http.StatusForbidden, verify("totp", "ZZZZZZ").Code)
	require.True(t, mr.Exists(lockoutKey), "the fifth wrong code in the window arms the lockout")
}

// A busy fifth attempt is refunded after a sixth was refused at the limit. The
// refused sixth judged nothing, so it gives its charge back and arms no
// lockout. After the busy refund the count is 4, so the next code is judged as
// the fifth, and a wrong one arms the lockout.
func TestBusyBackupCheck_SignInRefundAfterRefusalStillArmsTheLockout(t *testing.T) {
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	rdb, mr := iuLockoutRedis(t)
	h := iuHandler(db, rdb, kr)
	h.SetLoginCompleter(&recordingLoginCompleter{})
	userID, _, codes := busyBackupUser(t, db, kr)
	token, _, err := h.GenerateLoginChallenge(context.Background(), userID, false, "", securityevent.AuthPassword)
	require.NoError(t, err)
	attemptsKey := fmt.Sprintf(redisKeyMFAVerifyAttempts, userID)
	lockoutKey := fmt.Sprintf(redisKeyMFAVerifyLockout, userID)
	verify := func(method, code string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/verify",
			strings.NewReader(`{"mfa_challenge_token":"`+token+`","method":"`+method+`","code":"`+code+`"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		h.Verify(c)
		return w
	}

	for range failedAttemptLimit - 1 {
		require.Equal(t, http.StatusForbidden, verify("totp", "ZZZZZZ").Code)
	}
	free := backuphash.HoldAllSlots(2 * time.Second)
	t.Cleanup(free)
	busy := make(chan *httptest.ResponseRecorder, 1)
	go func() { busy <- verify("backup_code", codes[0]) }()
	require.Eventually(t, func() bool { got, _ := mr.Get(attemptsKey); return got == "5" },
		time.Second, time.Millisecond, "the busy request holds the fifth slot")

	require.Equal(t, http.StatusTooManyRequests, verify("totp", "ZZZZZZ").Code)
	requireCharge(t, mr, attemptsKey, "5") // the refused sixth gave its charge back
	require.False(t, mr.Exists(lockoutKey), "an attempt refused unjudged arms no lockout")
	requireBusy(t, <-busy)
	requireCharge(t, mr, attemptsKey, "4")
	require.False(t, mr.Exists(lockoutKey), "four wrong codes do not lock the account out")

	require.Equal(t, http.StatusForbidden, verify("totp", "ZZZZZZ").Code, "the next code is judged as the fifth")
	require.True(t, mr.Exists(lockoutKey), "the fifth wrong code arms the lockout")

	mr.FastForward(verifyAttemptWindow + time.Second)
	require.True(t, mr.Exists(lockoutKey), "the lockout outlives the attempt window")
	require.NotEqual(t, http.StatusForbidden, verify("totp", "ZZZZZZ").Code,
		"no further code is judged while locked out")
}

// Six concurrent sign-ins with the RIGHT backup code while every slot is held:
// five are busy and one is refused at the limit. None judged a code, so all
// six charges come back, no lockout is armed, and the right code then signs
// in. Arming the lockout on the refused one locked out an honest user who
// never sent a wrong code.
func TestBusyBackupCheck_ConcurrentRightCodesNeverLockOut(t *testing.T) {
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	rdb, mr := iuLockoutRedis(t)
	h := iuHandler(db, rdb, kr)
	completer := &recordingLoginCompleter{}
	h.SetLoginCompleter(completer)
	userID, _, codes := busyBackupUser(t, db, kr)
	token, _, err := h.GenerateLoginChallenge(context.Background(), userID, false, "", securityevent.AuthPassword)
	require.NoError(t, err)
	attemptsKey := fmt.Sprintf(redisKeyMFAVerifyAttempts, userID)
	lockoutKey := fmt.Sprintf(redisKeyMFAVerifyLockout, userID)
	verify := func(code string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/verify",
			strings.NewReader(`{"mfa_challenge_token":"`+token+`","method":"backup_code","code":"`+code+`"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		h.Verify(c)
		return w
	}

	// The busy requests wait for a slot, so all six are counted at once.
	release := backuphash.HoldAllSlots(time.Second)
	var once sync.Once
	free := func() { once.Do(release) }
	t.Cleanup(free)
	const n = failedAttemptLimit + 1
	statuses := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = verify(codes[0]).Code
		}()
	}
	require.Eventually(t, func() bool { got, _ := mr.Get(attemptsKey); return got == "5" },
		time.Second, time.Millisecond, "five busy requests hold every slot")
	wg.Wait()

	busy, refused := 0, 0
	for _, s := range statuses {
		switch s {
		case http.StatusServiceUnavailable:
			busy++
		case http.StatusTooManyRequests:
			refused++
		}
	}
	require.Equal(t, failedAttemptLimit, busy, "%v", statuses)
	require.Equal(t, 1, refused, "%v", statuses)
	requireCharge(t, mr, attemptsKey, "0")
	require.False(t, mr.Exists(lockoutKey), "no wrong code was sent, so nothing may lock the account out")
	require.False(t, completer.called)

	free()
	require.NotEqual(t, http.StatusTooManyRequests, verify(codes[0]).Code)
	require.True(t, completer.called, "the right code signs in once the slots free")
}

// WebAuthn key removal verifies its code through requireFactorRemovalStepUpTx.
// A busy backup-code check there refunds the charge after the transaction
// ends and removes nothing.
func TestBusyBackupCheck_WebAuthnDeleteRefundsTheCharge(t *testing.T) {
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	rdb, mr := iuLockoutRedis(t)
	h := iuHandler(db, rdb, kr)
	userID, _, codes := busyBackupUser(t, db, kr)
	iuAddWebAuthnCredential(t, db, userID)
	credentialID := iuWebAuthnCredentialID(t, db, userID)
	var locked []string
	rdb.AddHook(tdLockWatchHook{db: db, userID: userID, mu: &sync.Mutex{}, locked: &locked})
	t.Cleanup(func() { assert.Empty(t, locked, "Redis commands issued under the users-row lock") })
	remove := func(code string) *httptest.ResponseRecorder {
		c, w := iuGinContext(http.MethodDelete, "/api/v1/mfa/webauthn/credentials/"+credentialID,
			fmt.Sprintf(`{"password":%q,"mfa_code":%q}`, iuPassword, code), userID, "")
		c.Params = gin.Params{{Key: "id", Value: credentialID}}
		h.WebAuthnDeleteCredential(c)
		return w
	}

	release := holdBackupHashSlots(t)
	for range stepup.BudgetLimit + 1 {
		w := remove(codes[0])
		requireBusy(t, w)
		assert.Equal(t, true, iuBody(t, w)["verification_busy"])
		requireCharge(t, mr, h.stepUpBudget().Key(userID), "0")
	}
	release()
	assert.Equal(t, 1, iuCountRows(t, db, "user_mfa_webauthn", userID), "a busy check removes nothing")

	require.Equal(t, http.StatusForbidden, remove("WRONGCOD").Code)
	requireCharge(t, mr, h.stepUpBudget().Key(userID), "1")
}
