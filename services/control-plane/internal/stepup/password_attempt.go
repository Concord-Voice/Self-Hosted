package stepup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
)

// A step-up password is one guess against the account's shared /auth/login
// lockout (CWE-307): a stolen session gets the same five guesses as the
// sign-in form and no more. The guess is split around the caller's
// transaction the way Budget is, because every lockout step is a Redis round
// trip and a Redis round trip inside an open transaction pins a pooled
// connection, and often a users-row lock, for the length of a network call:
//
//  1. ReservePasswordAttempt (or ReserveUserPasswordAttempt), BEFORE BeginTx,
//     counts the guess and refuses a locked account with LockedError (423),
//     even for the correct password.
//  2. VerifyPasswordFactor or PasswordAttempt.Check, wherever the hash is
//     read (usually under the transaction's row lock), compares the hash and
//     notes the outcome. No Redis.
//  3. Settle, AFTER the transaction ends, locks the account when a wrong
//     guess reached the threshold, or resets the count when the password
//     matched. It resets on a match even when a later factor (MFA) or the
//     write failed, as /auth/login clears before its MFA challenge: the
//     lockout guards the password, and an honest MFA typo must not count
//     toward locking the user out of sign-in.
//
// Only step 1 fails closed: a Redis fault there is a 500, never a pass and
// never a lock. Step 3 is best-effort and returns its error for the caller to
// log. It cannot let an extra guess through: step 1 already counted the
// guess, and the next reserve past the threshold locks at once, so a missed
// record costs no extra guess; a missed reset leaves the count high, which
// fails toward MORE limiting.

// ErrPasswordLockoutUnavailable marks the 500 a step-up answers when the
// shared lockout cannot count a guess. It is a different Cause from
// ErrPasswordVerification (a hash that cannot be checked), so a log tells a
// Redis fault from a corrupt hash. It wraps only the Redis error, which never
// carries the password or the hash.
var ErrPasswordLockoutUnavailable = errors.New("password lockout unavailable")

// ErrPasswordNotReserved marks the 500 for a supplied password that reached
// the hash check without a reservation. That is a wiring fault, and the
// guess is refused: a guess that was never counted must never be checked.
var ErrPasswordNotReserved = errors.New("password guess was not reserved against the lockout")

// errLockoutKeyMissing is the Cause when the account has no email to key its
// lockout by. users.email is NOT NULL, so this is defensive.
var errLockoutKeyMissing = errors.New("account has no email to key the password lockout")

// attemptSettleTimeout bounds Settle, which runs on a context
// detached from the request so a client that hangs up cannot skip them.
const attemptSettleTimeout = 5 * time.Second

// lockoutEmailSQL reads the email that keys the account's lockout. It is
// spelled apart from the handlers' own email reads so a test fake can answer
// each one separately.
const lockoutEmailSQL = `SELECT u.email FROM users AS u WHERE u.id = $1`

type attemptState int

const (
	attemptNone     attemptState = iota // no password was sent; nothing counted
	attemptReserved                     // counted, not yet checked
	attemptWrong                        // checked: wrong password
	attemptMatched                      // checked: correct password
)

// PasswordAttempt is one reserved step-up password guess. The zero value and
// a nil pointer are an unreserved attempt: Check refuses them and Settle does
// nothing. It is not safe for concurrent use; one request owns it.
type PasswordAttempt struct {
	rdb   *redis.Client
	email string
	state attemptState
}

// ReservePasswordAttempt counts password as one guess against the lockout
// keyed by email. Call it BEFORE BeginTx. An empty password guesses nothing:
// it is not counted, and the returned attempt is unreserved.
//
// It returns LockedError (423) while the account is locked and a 500 with
// Cause wrapping ErrPasswordLockoutUnavailable when the guess cannot be
// counted.
func ReservePasswordAttempt(ctx context.Context, rdb *redis.Client, email, password string) (*PasswordAttempt, *Error) {
	if password == "" {
		return &PasswordAttempt{}, nil
	}
	if email == "" {
		return nil, verificationFailed(errLockoutKeyMissing)
	}
	allowed, err := auth.ReserveLoginAttempt(ctx, rdb, email)
	if err != nil {
		return nil, verificationFailed(fmt.Errorf("%w: %w", ErrPasswordLockoutUnavailable, err))
	}
	if !allowed {
		return nil, LockedError()
	}
	return &PasswordAttempt{rdb: rdb, email: email, state: attemptReserved}, nil
}

// ReserveUserPasswordAttempt is ReservePasswordAttempt for userID, reading
// the email that keys the lockout through q. Call it BEFORE BeginTx, with the
// pool. An empty password reads and counts nothing. A deleted account is the
// 401 LoadSubject answers; a failed read is a 500.
//
// The email is read outside the caller's transaction. An email change that
// commits in between moves no guess past the lockout: this guess is still
// counted, recorded and cleared under one key.
func ReserveUserPasswordAttempt(
	ctx context.Context, q RowQuerier, rdb *redis.Client, userID, password string,
) (*PasswordAttempt, *Error) {
	if password == "" {
		return &PasswordAttempt{}, nil
	}
	var email string
	err := q.QueryRowContext(ctx, lockoutEmailSQL, userID).Scan(&email)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, subjectGone()
	}
	if err != nil {
		return nil, verificationFailed(fmt.Errorf("read lockout email: %w", err))
	}
	return ReservePasswordAttempt(ctx, rdb, email, password)
}

// Check compares password with hash for a reserved attempt and notes the
// outcome for Settle. It makes no Redis call, so it may run
// under a transaction's lock.
//
// It returns ErrPasswordNotReserved for an unreserved attempt and
// ErrPasswordVerification for a hash it cannot check; neither carries the
// password or the hash, so a caller may log it.
func (a *PasswordAttempt) Check(password, hash string) (bool, error) {
	if a == nil || a.state == attemptNone {
		return false, ErrPasswordNotReserved
	}
	match, err := auth.VerifyPassword(password, hash)
	if err != nil {
		// The decode error is dropped: it can embed the hash.
		return false, ErrPasswordVerification
	}
	if match {
		a.state = attemptMatched
	} else {
		a.state = attemptWrong
	}
	return match, nil
}

// Verified reports whether Check found the correct password.
func (a *PasswordAttempt) Verified() bool { return a != nil && a.state == attemptMatched }

// Settle records a checked guess against the lockout: a wrong password counts
// toward the lock and a correct one resets the count (see the package steps).
// Call it once, AFTER the caller's transaction ends. It does nothing for an
// attempt that was never checked. The error is for the caller to log.
func (a *PasswordAttempt) Settle(ctx context.Context) error {
	if a == nil || (a.state != attemptWrong && a.state != attemptMatched) {
		return nil
	}
	post, cancel := context.WithTimeout(context.WithoutCancel(ctx), attemptSettleTimeout)
	defer cancel()
	if a.state == attemptWrong {
		if err := auth.RecordCheckedFailure(post, a.rdb, a.email); err != nil {
			return fmt.Errorf("record failed step-up password: %w", err)
		}
		return nil
	}
	if err := auth.ClearLoginAttempts(post, a.rdb, a.email); err != nil {
		return fmt.Errorf("clear step-up password attempts: %w", err)
	}
	return nil
}

// MarkAccountLocked records the lockout security event for a step-up 423 on
// a handler that has no security-event emitter of its own: the router's
// Nightwatch observer emits the verdict with the route template. The fields
// are the ones internal/sessions and internal/mfa emit directly.
func MarkAccountLocked(c *gin.Context) {
	middleware.MarkNightwatchVerdict(c, middleware.NightwatchVerdict{
		EventType: securityevent.EventSecurityControl, Outcome: securityevent.OutcomeDenied,
		Severity: securityevent.SeverityMedium, Reason: securityevent.ReasonAccountLocked,
		AuthMethod: securityevent.AuthPassword,
	})
}
