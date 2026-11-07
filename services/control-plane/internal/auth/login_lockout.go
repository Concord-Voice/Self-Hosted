package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// This file is the one implementation of the per-email login lockout. The
// *Handler methods in handlers.go wrap it for /auth/login and CompleteLink.
// The authenticated password step-up routes in other packages use it too, so
// a stolen session gets the same five guesses as the sign-in form and no more
// (CWE-307): the routes that hold no transaction (internal/sessions,
// internal/ownership, backup-code regeneration in internal/mfa) call
// VerifyPasswordWithLockout, and the routes that check the password under a
// row lock go through internal/stepup's PasswordAttempt, which calls
// ReserveLoginAttempt before BeginTx and RecordCheckedFailure and
// ClearLoginAttempts after the transaction ends.

// The loginAttemptScript modes.
const (
	loginAttemptReserve = "reserve"
	loginAttemptChecked = "checked"
	loginAttemptFail    = "fail"
	loginAttemptClear   = "clear"
)

// ErrLockoutUnavailable is returned when no Redis client is wired. A guess
// that cannot be counted is refused, never allowed.
var ErrLockoutUnavailable = errors.New("login lockout unavailable")

// ErrPasswordHashCheck replaces the error VerifyPassword returns for a hash
// it cannot decode. That error can embed the hash it failed to parse, so it
// never leaves VerifyPasswordWithLockout; this fixed sentinel is safe to log.
var ErrPasswordHashCheck = errors.New("password hash check failed")

func runLoginAttemptScript(ctx context.Context, rdb *redis.Client, email, mode string) (int64, error) {
	if rdb == nil {
		return 0, ErrLockoutUnavailable
	}
	args := make([]interface{}, 0, 3+len(loginLockoutDurations))
	args = append(args, mode, loginLockoutThreshold, int64(time.Hour/time.Second))
	for _, d := range loginLockoutDurations {
		args = append(args, int64(d/time.Second))
	}
	return loginAttemptScript.Run(ctx, rdb, []string{
		fmt.Sprintf("login_attempts:%s", email),
		fmt.Sprintf(redisKeyLoginLockout, email),
		fmt.Sprintf("login_lockout_count:%s", email),
	}, args...).Int64()
}

// ReserveLoginAttempt counts a password guess before it is checked, in one
// atomic step with the lockout check. It returns false when the account is
// locked. On an error the caller must refuse the guess as a server error,
// never as a lock and never as a success.
func ReserveLoginAttempt(ctx context.Context, rdb *redis.Client, email string) (bool, error) {
	ok, err := runLoginAttemptScript(ctx, rdb, email, loginAttemptReserve)
	if err != nil {
		return false, err
	}
	return ok == 1, nil
}

// RecordCheckedFailure locks the account when a reserved guess that failed
// reached the threshold. The guess was already counted by ReserveLoginAttempt,
// so this does not count it again.
func RecordCheckedFailure(ctx context.Context, rdb *redis.Client, email string) error {
	_, err := runLoginAttemptScript(ctx, rdb, email, loginAttemptChecked)
	return err
}

// ClearLoginAttempts resets the counters after a successful guess. It leaves
// a lock that a concurrent burst set in place.
func ClearLoginAttempts(ctx context.Context, rdb *redis.Client, email string) error {
	_, err := runLoginAttemptScript(ctx, rdb, email, loginAttemptClear)
	return err
}

// VerifyPasswordWithLockout checks password against hash under the per-email
// lockout /auth/login uses: it reserves the guess before Argon2id runs, records
// a wrong password, and clears the count on success. It returns:
//
//   - (true, nil) for the correct password;
//   - (false, nil) for a wrong password, after counting it;
//   - (false, ErrAccountLocked) while the account is locked, before any hash
//     work and even for the correct password;
//   - (false, err) for any other fault: a Redis error (wrapped) or
//     ErrPasswordHashCheck. The caller answers it as a server error.
//
// Any Redis error refuses, including one after the hash check: the guess
// cannot be shown to have been counted or cleared. No returned error carries
// the password or the hash, so a caller may log it.
func VerifyPasswordWithLockout(ctx context.Context, rdb *redis.Client, email, password, hash string) (bool, error) {
	allowed, err := ReserveLoginAttempt(ctx, rdb, email)
	if err != nil {
		return false, fmt.Errorf("reserve login attempt: %w", err)
	}
	if !allowed {
		return false, ErrAccountLocked
	}

	match, err := VerifyPassword(password, hash)
	if err != nil {
		// The decode error is dropped: it can embed the hash.
		return false, ErrPasswordHashCheck
	}
	if !match {
		if err := RecordCheckedFailure(ctx, rdb, email); err != nil {
			return false, fmt.Errorf("record failed login attempt: %w", err)
		}
		return false, nil
	}
	if err := ClearLoginAttempts(ctx, rdb, email); err != nil {
		return false, fmt.Errorf("clear login attempts: %w", err)
	}
	return true, nil
}
