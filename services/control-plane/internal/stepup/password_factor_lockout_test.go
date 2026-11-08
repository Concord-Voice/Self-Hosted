package stepup

// A step-up password counts toward the shared per-email /auth/login lockout,
// so a stolen session cannot guess the password at a step-up route without
// the five-guess limit (CWE-307). The guess is split around the caller's
// transaction: reserve before it, check inside it, record and clear after it.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
)

const lockoutEmail = "stepup-lockout@example.test"

// reservedAttempt is a reserved guess on a private miniredis, for the tests
// that exercise only the hash check.
func reservedAttempt(t *testing.T) *PasswordAttempt {
	t.Helper()
	rdb, _ := newBudgetRedis(t)
	a, e := ReservePasswordAttempt(context.Background(), rdb, lockoutEmail, "any-guess")
	require.Nil(t, e)
	return a
}

func lockoutHash(t *testing.T, plaintext string) string {
	t.Helper()
	hash, err := auth.HashPassword(plaintext)
	require.NoError(t, err)
	return hash
}

// guess is one full step-up as a transactional route runs it: reserve, check,
// then settle after the transaction.
func guess(t *testing.T, rdb *redis.Client, subj Subject, password string) *Error {
	t.Helper()
	ctx := context.Background()
	a, e := ReservePasswordAttempt(ctx, rdb, lockoutEmail, password)
	if e != nil {
		return e
	}
	e = VerifyPasswordFactor(a, subj, password, testCopy)
	require.NoError(t, a.Settle(ctx))
	return e
}

// Mutant killed: checking the hash without the reservation (the sixth attempt
// with the correct password would pass).
func TestPasswordAttempt_FiveWrongPasswordsLockTheAccount(t *testing.T) {
	const plaintext = "correct-horse-battery"
	rdb, mr := newBudgetRedis(t)
	subj := Subject{PasswordHash: lockoutHash(t, plaintext)}

	for i := range 5 {
		e := guess(t, rdb, subj, "wrong-password")
		require.NotNil(t, e, "guess %d", i+1)
		require.Equal(t, http.StatusForbidden, e.Status, "guess %d", i+1)
	}
	require.True(t, mr.Exists("login_lockout:"+lockoutEmail), "the fifth wrong guess sets the shared lock")

	e := guess(t, rdb, subj, plaintext)
	require.NotNil(t, e, "a locked account is refused even for the correct password")
	require.Equal(t, http.StatusLocked, e.Status)
	require.Equal(t, "account_locked", e.Body["error_code"])
	require.Equal(t, ErrMsgStepUpLocked, e.Body["error"])
	require.Nil(t, e.Cause, "a lock is an outcome, not a fault")
}

// Mutant killed: dropping Settle still leaves the sixth reserve to lock,
// so the guess limit holds even when the post-transaction record is lost.
func TestPasswordAttempt_LostRecordStillLocksOnTheNextReserve(t *testing.T) {
	rdb, mr := newBudgetRedis(t)
	ctx := context.Background()
	subj := Subject{PasswordHash: lockoutHash(t, "correct-horse-battery")}

	for i := range 5 {
		a, e := ReservePasswordAttempt(ctx, rdb, lockoutEmail, "wrong-password")
		require.Nil(t, e, "guess %d", i+1)
		require.NotNil(t, VerifyPasswordFactor(a, subj, "wrong-password", testCopy))
	}
	require.False(t, mr.Exists("login_lockout:"+lockoutEmail))

	_, e := ReservePasswordAttempt(ctx, rdb, lockoutEmail, "correct-horse-battery")
	require.NotNil(t, e)
	require.Equal(t, http.StatusLocked, e.Status)
}

// Mutant killed: a Settle that clears on a wrong guess, or that never clears
// after a correct one (an honest MFA typo after the correct
// password must not keep counting toward the sign-in lockout).
func TestPasswordAttempt_SettleRecordsWrongAndClearsMatched(t *testing.T) {
	const plaintext = "correct-horse-battery"
	rdb, mr := newBudgetRedis(t)
	ctx := context.Background()
	subj := Subject{PasswordHash: lockoutHash(t, plaintext)}

	wrong, e := ReservePasswordAttempt(ctx, rdb, lockoutEmail, "wrong-password")
	require.Nil(t, e)
	require.NotNil(t, VerifyPasswordFactor(wrong, subj, "wrong-password", testCopy))
	require.NoError(t, wrong.Settle(ctx))
	require.True(t, mr.Exists("login_attempts:"+lockoutEmail), "a wrong guess never clears the count")

	right, e := ReservePasswordAttempt(ctx, rdb, lockoutEmail, plaintext)
	require.Nil(t, e)
	require.True(t, mr.Exists("login_attempts:"+lockoutEmail), "a reserve alone clears nothing")
	require.Nil(t, VerifyPasswordFactor(right, subj, plaintext, testCopy))
	require.True(t, right.Verified())
	require.NoError(t, right.Settle(ctx))
	require.False(t, mr.Exists("login_attempts:"+lockoutEmail), "a correct password resets the counter")
}

// Mutant killed: a Settle that acts on a reserved but unchecked guess.
func TestPasswordAttempt_SettleBeforeCheckDoesNothing(t *testing.T) {
	rdb, mr := newBudgetRedis(t)
	ctx := context.Background()
	a, e := ReservePasswordAttempt(ctx, rdb, lockoutEmail, "guess")
	require.Nil(t, e)
	require.NoError(t, a.Settle(ctx))
	require.True(t, mr.Exists("login_attempts:"+lockoutEmail))
	require.False(t, mr.Exists("login_lockout:"+lockoutEmail))
}

func TestPasswordAttempt_EmptyPasswordCountsNothing(t *testing.T) {
	rdb, mr := newBudgetRedis(t)
	a, e := ReservePasswordAttempt(context.Background(), rdb, lockoutEmail, "")

	require.Nil(t, e)
	require.False(t, a.Verified())
	require.False(t, mr.Exists("login_attempts:"+lockoutEmail), "a request with no password guesses nothing")
	require.NoError(t, a.Settle(context.Background()))
	var nilAttempt *PasswordAttempt
	require.NoError(t, nilAttempt.Settle(context.Background()))
}

// Finding 4: a Redis fault must not look like a corrupt hash in the logs.
func TestPasswordAttempt_RedisDownIsA500WithItsOwnCause(t *testing.T) {
	rdb, mr := newBudgetRedis(t)
	mr.Close()

	a, e := ReservePasswordAttempt(context.Background(), rdb, lockoutEmail, "correct-horse-battery")

	require.Nil(t, a)
	require.NotNil(t, e, "an uncountable guess must never pass")
	require.Equal(t, http.StatusInternalServerError, e.Status)
	require.Equal(t, ErrMsgVerificationFailed, e.Body["error"])
	require.ErrorIs(t, e, ErrPasswordLockoutUnavailable)
	require.NotErrorIs(t, e, ErrPasswordVerification, "a Redis fault is not a hash fault")
	require.NotContains(t, e.Cause.Error(), "correct-horse-battery")
}

func TestPasswordAttempt_NilRedisIsA500(t *testing.T) {
	_, e := ReservePasswordAttempt(context.Background(), nil, lockoutEmail, "guess")

	require.NotNil(t, e)
	require.Equal(t, http.StatusInternalServerError, e.Status)
	require.ErrorIs(t, e, ErrPasswordLockoutUnavailable)
	require.ErrorIs(t, e, auth.ErrLockoutUnavailable)
}

func TestPasswordAttempt_MissingEmailIsA500(t *testing.T) {
	rdb, _ := newBudgetRedis(t)
	_, e := ReservePasswordAttempt(context.Background(), rdb, "", "guess")

	require.NotNil(t, e)
	require.Equal(t, http.StatusInternalServerError, e.Status)
	require.ErrorIs(t, e, errLockoutKeyMissing)
}

// Mutant killed: checking a supplied password that was never reserved.
func TestVerifyPasswordFactor_UnreservedPasswordIsRefused(t *testing.T) {
	subj := Subject{PasswordHash: lockoutHash(t, "correct-horse-battery")}

	for name, a := range map[string]*PasswordAttempt{"nil": nil, "empty-password reservation": {}} {
		e := VerifyPasswordFactor(a, subj, "correct-horse-battery", testCopy)
		require.NotNil(t, e, name)
		require.Equal(t, http.StatusInternalServerError, e.Status, name)
		require.ErrorIs(t, e, ErrPasswordNotReserved, name)
	}
}

func TestPasswordAttempt_SettleErrorsAreReturned(t *testing.T) {
	const plaintext = "correct-horse-battery"
	subj := Subject{PasswordHash: lockoutHash(t, plaintext)}
	ctx := context.Background()

	rdb, mr := newBudgetRedis(t)
	wrong, e := ReservePasswordAttempt(ctx, rdb, lockoutEmail, "wrong-password")
	require.Nil(t, e)
	right, e := ReservePasswordAttempt(ctx, rdb, lockoutEmail, plaintext)
	require.Nil(t, e)
	require.NotNil(t, VerifyPasswordFactor(wrong, subj, "wrong-password", testCopy))
	require.Nil(t, VerifyPasswordFactor(right, subj, plaintext, testCopy))
	mr.Close()

	require.Error(t, wrong.Settle(ctx))
	require.Error(t, right.Settle(ctx))
}

func TestReserveUserPasswordAttempt(t *testing.T) {
	db := subjectTestDB(t)
	ctx := context.Background()

	t.Run("reads the email that keys the lockout", func(t *testing.T) {
		rdb, mr := newBudgetRedis(t)
		userID := subjectTestUser(t, db)

		a, e := ReserveUserPasswordAttempt(ctx, db, rdb, userID, "guess")

		require.Nil(t, e)
		require.NotNil(t, a)
		require.True(t, mr.Exists("login_attempts:"+userID+"@stepup-subject.test"))
	})
	t.Run("empty password reads nothing", func(t *testing.T) {
		a, e := ReserveUserPasswordAttempt(ctx, nil, nil, uuid.New().String(), "")
		require.Nil(t, e)
		require.False(t, a.Verified())
	})
	t.Run("deleted account is the 401", func(t *testing.T) {
		rdb, _ := newBudgetRedis(t)
		_, e := ReserveUserPasswordAttempt(ctx, db, rdb, uuid.New().String(), "guess")
		require.NotNil(t, e)
		require.Equal(t, http.StatusUnauthorized, e.Status)
	})
	t.Run("failed read is a 500", func(t *testing.T) {
		rdb, _ := newBudgetRedis(t)
		_, e := ReserveUserPasswordAttempt(ctx, db, rdb, "not-a-uuid", "guess")
		require.NotNil(t, e)
		require.Equal(t, http.StatusInternalServerError, e.Status)
		require.NotErrorIs(t, e, ErrPasswordLockoutUnavailable)
	})
}

func TestMarkAccountLocked_SetsTheLockoutVerdict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	MarkAccountLocked(c)

	verdict, ok := middleware.NightwatchVerdictFromContext(c)
	require.True(t, ok)
	require.Equal(t, securityevent.ReasonAccountLocked, verdict.Reason)
	require.Equal(t, securityevent.EventSecurityControl, verdict.EventType)
	require.Equal(t, securityevent.OutcomeDenied, verdict.Outcome)
	require.Equal(t, securityevent.AuthPassword, verdict.AuthMethod)
}

func TestLockedError_ReturnsAFreshValue(t *testing.T) {
	first := LockedError()
	first.Body["error"] = "mutated by a caller"

	require.Equal(t, ErrMsgStepUpLocked, LockedError().Body["error"])
}
