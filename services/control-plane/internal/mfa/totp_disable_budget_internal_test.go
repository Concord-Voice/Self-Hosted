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
	"slices"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
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
