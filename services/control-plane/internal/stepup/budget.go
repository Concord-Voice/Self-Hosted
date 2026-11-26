package stepup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// Budget bounds are const, never configuration: a configurable security bound
// is one more place it can be wrong ([internal]rules/backend.md).
const (
	BudgetLimit  = 5
	BudgetWindow = 15 * time.Minute
)

// MFASettingsBudgetPrefix keys the one budget shared by every MFA-settings
// route and the MFA-enforcement toggle's OFF confirmation. It is one budget
// because each of those surfaces verifies the same factor: a budget per route
// hands a stolen session five more guesses for every route added (PR #3464
// review finding 8, CWE-307). The spelling predates the toggle and is kept, so
// a deploy does not reset counters in flight.
const MFASettingsBudgetPrefix = "stepup:mfa_settings:"

// ErrMsgTooManyAttempts is the 429 body when the budget is exhausted. The body
// also carries "step_up_budget_exhausted": true, so a client can tell it from
// the route limiter's 429 without matching copy (#3455).
const ErrMsgTooManyAttempts = "Too many verification attempts"

// ErrMsgBudgetUnavailable is the 503 body when the budget cannot be
// evaluated. It is deliberately NOT the 429: telling a user "too many
// attempts" during a Redis outage misreports a server fault as their own
// doing and locks the form for a window that is not running. The body also
// carries "step_up_budget_unavailable": true, which tells it from a gate's
// other 503s (#3455).
const ErrMsgBudgetUnavailable = "Verification is temporarily unavailable. Try again in a few minutes."

var errBudgetUnwired = errors.New("step-up attempt budget has no Redis client")

// Budget is the fail-closed attempt budget every credential-bearing step-up
// charges before it opens a transaction. It is shared so the purge fence and
// the MFA-settings routes cannot drift apart again: they did once, and one of
// them answered a Redis outage with a 429.
//
// Charge it BEFORE BeginTx — a Redis round trip inside an open transaction
// pins a pooled connection for the length of a network call — and only when
// the request actually carries a credential: a credential-less request learns
// nothing beyond the fixed "step-up required" refusal every account gets, so
// charging it would let any bearer burn the owner's budget without guessing.
//
// Clear it only after a VERIFIED success has committed. AllowUserAction
// increments before the outcome is knowable and never decrements, so without
// the clear a legitimate user is eventually locked out by their own correct
// credentials; a verified credential whose transaction then rolled back
// changed nothing and still costs an attempt.
type Budget struct {
	rdb    *redis.Client
	prefix string
}

// NewBudget binds a budget to its Redis client and key prefix. A nil client
// is allowed and DENIES (503): a limiter that silently no-ops when unwired is
// the failure this type exists to prevent.
func NewBudget(rdb *redis.Client, prefix string) Budget {
	return Budget{rdb: rdb, prefix: prefix}
}

// Key is the one place a budget's Redis key is spelled, so the consume and
// the clear cannot disagree about it.
func (b Budget) Key(userID string) string { return b.prefix + userID }

// DangerousActionBudget is the budget every dangerous-action gate charges
// (#3454): the shared MFASettingsBudgetPrefix one, because a D1 gate verifies
// the same factor the MFA-settings routes and the enforcement toggle do, and a
// budget per route would hand a stolen session five more guesses per route.
func DangerousActionBudget(rdb *redis.Client) Budget {
	return NewBudget(rdb, MFASettingsBudgetPrefix)
}

// Charge is a dangerous-action gate's budget charge: one attempt when the
// request carries an MFA code, and nothing when it does not, since a request
// with no code guesses nothing. Only mfaCode counts: a D1 gate never accepts a
// password or a step_up_token, so neither is a guess against it.
//
// Run it BEFORE BeginTx (see Budget), and whatever the server's setting: the
// setting is read under the gate's lock, and deciding to charge from an
// unlocked read of it would fail open (#3454 C6). Its refusals are Consume's:
// the 429 flagged step_up_budget_exhausted, and the 503 flagged
// step_up_budget_unavailable with Cause set.
func Charge(ctx context.Context, b Budget, userID, mfaCode string) *Error {
	_, e := ChargeAttempt(ctx, b, userID, mfaCode)
	return e
}

// ChargeAttempt is Charge that also returns the charge, for RefundIfBusy.
// With no code it charges nothing and returns the zero Charged.
func ChargeAttempt(ctx context.Context, b Budget, userID, mfaCode string) (Charged, *Error) {
	if mfaCode == "" {
		return Charged{}, nil
	}
	return b.ConsumeAttempt(ctx, userID)
}

// Consume charges one attempt. It returns nil when the attempt is admitted,
// a 429 when the budget is exhausted, and a 503 — with Cause set, for the
// caller to log — when the budget cannot be evaluated at all.
func (b Budget) Consume(ctx context.Context, userID string) *Error {
	_, e := b.ConsumeAttempt(ctx, userID)
	return e
}

// ConsumeAttempt is Consume that also returns the admitted charge, which
// RefundIfBusy needs: a refund is bound to the window it was charged in.
func (b Budget) ConsumeAttempt(ctx context.Context, userID string) (Charged, *Error) {
	if b.rdb == nil {
		return Charged{}, budgetUnavailable(errBudgetUnwired)
	}
	count, charged, err := ChargeKey(ctx, b.rdb, b.Key(userID), BudgetWindow)
	if err != nil {
		return Charged{}, budgetUnavailable(fmt.Errorf("consume step-up attempt: %w", err))
	}
	if count > BudgetLimit {
		ttl, ttlErr := b.rdb.PTTL(ctx, b.Key(userID)).Result()
		if ttlErr != nil {
			return Charged{}, budgetUnavailable(fmt.Errorf("read step-up attempt PTTL: %w", ttlErr))
		}
		if ttl <= 0 {
			return Charged{}, budgetUnavailable(fmt.Errorf("read step-up attempt PTTL: non-positive PTTL %s", ttl))
		}
		return Charged{}, &Error{
			Status:            http.StatusTooManyRequests,
			Body:              gin.H{"error": ErrMsgTooManyAttempts, "step_up_budget_exhausted": true},
			retryAfterSeconds: int((ttl + time.Second - 1) / time.Second),
		}
	}
	return charged, nil
}

// chargeScript counts one attempt on KEYS[1], starts the window when the key
// has no expiry (a new key, or one whose earlier expiry write was lost), and
// returns the count with the window's ID, held in KEYS[2] with the counter's
// TTL. A count of 1 starts a new window, so it takes the new ID ARGV[2]: no
// earlier charge on this counter is still outstanding, since an unrefunded
// charge keeps the count above zero. Any other count keeps the stored ID, or
// sets ARGV[2] if a counter written before IDs existed has none.
var chargeScript = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
redis.call('PEXPIRE', KEYS[1], ARGV[1], 'NX')
local ttl = redis.call('PTTL', KEYS[1])
if n == 1 then
	redis.call('SET', KEYS[2], ARGV[2], 'PX', ttl)
else
	redis.call('SET', KEYS[2], ARGV[2], 'PX', ttl, 'NX')
end
return {n, redis.call('GET', KEYS[2])}
`)

// windowKey holds the ID of the window a counter key is counting.
func windowKey(key string) string { return key + ":window" }

// ChargeKey counts one attempt on key in a window that starts at the first
// attempt, and returns the count and the charge. Counting and reading the
// window ID in one script means the charge always names the window it was
// counted in, so its refund cannot reach a later window.
func ChargeKey(ctx context.Context, rdb *redis.Client, key string, window time.Duration) (int64, Charged, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return 0, Charged{}, fmt.Errorf("charge %q: window id: %w", key, err)
	}
	res, err := chargeScript.Run(ctx, rdb, []string{key, windowKey(key)}, window.Milliseconds(), hex.EncodeToString(nonce)).Slice()
	if err != nil {
		return 0, Charged{}, err
	}
	var count int64
	var id string
	if len(res) == 2 {
		count, _ = res[0].(int64)
		id, _ = res[1].(string)
	}
	if count <= 0 || id == "" {
		return 0, Charged{}, fmt.Errorf("charge %q: unexpected reply %v", key, res)
	}
	return count, Charged{rdb: rdb, key: key, window: id}, nil
}

// Charged is one admitted attempt and the window it was counted in. The zero
// value charged nothing and refunds nothing.
type Charged struct {
	rdb    *redis.Client
	key    string
	window string // the window's ID (windowKey)
}

// Made reports whether this is a real charge rather than the zero value.
func (ch Charged) Made() bool { return ch.rdb != nil }

// Clear resets the budget after a verified, committed success. It is
// best-effort by design: a failure leaves the counter high, which fails
// toward MORE limiting, never less. The error is returned for the caller to
// log; an unwired client has nothing to clear.
func (b Budget) Clear(ctx context.Context, userID string) error {
	if b.rdb == nil {
		return nil
	}
	if err := b.rdb.Del(ctx, b.Key(userID)).Err(); err != nil {
		return fmt.Errorf("clear step-up attempts: %w", err)
	}
	return nil
}

func budgetUnavailable(cause error) *Error {
	return &Error{
		Status: http.StatusServiceUnavailable,
		Body:   gin.H{"error": ErrMsgBudgetUnavailable, "step_up_budget_unavailable": true},
		Cause:  cause,
	}
}

// refundScript returns one attempt only to the window it was charged in
// (ARGV[1], that window's ID). A request that waited out its window must not
// take back a charge another request made in the next one: that would let a
// request straddling each boundary keep every window short of its limit. It
// never creates the key or takes it below zero, and DECRBY keeps the expiry.
var refundScript = redis.NewScript(`
if redis.call('GET', KEYS[2]) ~= ARGV[1] then return 0 end
local n = tonumber(redis.call('GET', KEYS[1]) or '0')
if n > 0 then return redis.call('DECRBY', KEYS[1], 1) end
return 0
`)

// Refund returns this charge when its code was never judged: on a busy check
// (RefundIfBusy) or on an MFA sign-in refused past the limit (the mfa
// package's reserveVerifyAttempt). A wrong code, or any other failure, keeps
// its charge.
// It runs on SettleContext so a client that hangs up after a busy answer still
// gets the attempt back. A charge whose window has ended refunds nothing.
func (ch Charged) Refund(ctx context.Context) error {
	if ch.rdb == nil {
		return nil
	}
	sctx, cancel := SettleContext(ctx)
	defer cancel()
	if err := refundScript.Run(sctx, ch.rdb, []string{ch.key, windowKey(ch.key)}, ch.window).Err(); err != nil {
		return fmt.Errorf("refund step-up attempt: %w", err)
	}
	return nil
}

// RefundIfBusy refunds each charge when err is a busy backup-code check
// (IsBusy) and does nothing otherwise. Call it with every charge the request
// made, once the verification result is known.
func RefundIfBusy(ctx context.Context, err error, charges ...Charged) error {
	if !IsBusy(err) {
		return nil
	}
	var errs []error
	for _, ch := range charges {
		errs = append(errs, ch.Refund(ctx))
	}
	return errors.Join(errs...)
}
