package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Lockout policy constants (#1688 §7, reworked in #221). A password guess is
// counted BEFORE its password is checked. The sixth guess in an attempts window
// locks the axis; each lock lasts twice as long as the one before it, from
// lockoutBaseWindow up to lockoutMaxWindow. The level behind that escalation
// lives in its own key and outlives the lock, so waiting out a lock does not
// buy a fresh first-level lock.
const (
	// lockoutThreshold is the number of guesses whose password is checked before
	// a lock engages. The next guess is refused unchecked.
	lockoutThreshold = 5
	// lockoutBaseWindow is the length of the first lock.
	lockoutBaseWindow = time.Minute
	// lockoutMaxWindow caps the escalation so a determined attacker cannot hold an
	// operator out for longer than an operator can wait.
	lockoutMaxWindow = 60 * time.Minute
	// lockoutAttemptsWindow is how long a guess stays counted. It must be long
	// enough that a patient attacker cannot stay under the threshold by waiting.
	lockoutAttemptsWindow = time.Hour
	// lockoutLevelMemory is how long the level outlives the lock it escalated: a
	// level key lives for the lock window plus this.
	lockoutLevelMemory = time.Hour
	// lockoutMaxLevel is the first level whose window reaches lockoutMaxWindow.
	lockoutMaxLevel = 7
)

// Lockout key prefixes. Each axis has three keys: attempts (the running count),
// lock (exists while the axis is locked) and level (how many times it has
// locked). The account keys carry the operator handle directly (not PII — admin
// handles are not emails); the IP keys carry a SHA-256 hash of the IP so the RAW
// IP is NEVER stored at rest (#1688 §7).
// The kind sits before the colon ("acct_lock:", not "acct:lock:") so a handle
// can never collide with another kind's key. The managed-Redis ACL manifest
// lists every prefix.
const (
	lockoutAcctPrefix      = "admin_lockout:acct:"
	lockoutAcctLockPrefix  = "admin_lockout:acct_lock:"
	lockoutAcctLevelPrefix = "admin_lockout:acct_level:"
	lockoutIPPrefix        = "admin_lockout:ip:"
	lockoutIPLockPrefix    = "admin_lockout:ip_lock:"
	lockoutIPLevelPrefix   = "admin_lockout:ip_level:"
)

// The lockoutScript modes.
const (
	lockoutModeReserve = "reserve"
	lockoutModeChecked = "checked"
	lockoutModeClear   = "clear"
)

// ErrLockoutUnavailable is returned when no Redis client is wired. A guess that
// cannot be counted is refused, never allowed.
var ErrLockoutUnavailable = errors.New("admin lockout unavailable")

// lockoutScript counts and locks both axes in one atomic Redis step, so a burst
// of concurrent guesses cannot all pass the lock check before any is counted.
// KEYS: account attempts, lock, level; then the same three for the IP.
// ARGV[1] is the mode, ARGV[2] the threshold, ARGV[3] the attempts window in
// seconds, ARGV[4] how long the level outlives its lock in seconds, and the rest
// the lock window of level 1, 2, ... in seconds (the last repeats).
//
//	reserve - count a guess BEFORE its password is checked. Returns 1 if the
//	          check may run, 0 if either axis is locked or this guess went over
//	          the threshold. A locked axis is not counted again.
//	checked - a reserved guess failed; lock any axis that reached the threshold.
//	          The guess was already counted, so this does not count it again.
//	clear   - the sign-in or enrollment completed; drop the account's attempts and
//	          lock, and refund the one guess this sign-in reserved on the IP that
//	          reserved it (never below zero). The levels stay, so a re-attack after a sign-in
//	          escalates at once. The IP lock and the rest of the IP count stay
//	          too: they record what came from that source, and one admin proving
//	          their own identity says nothing about those guesses (a holder of one
//	          valid account must not be able to lift a spray lock or reset a spray
//	          budget).
//
// Counting before the check has one cost: six guesses sent at the same moment
// lock the axis even if they are all correct, because the sixth is refused
// before any success clears the count. A real operator does not do this.
var lockoutScript = redis.NewScript(`
local mode = ARGV[1]
local limit = tonumber(ARGV[2])
local memory = tonumber(ARGV[4])
local windows = #ARGV - 4
local axes = {{KEYS[1], KEYS[2], KEYS[3]}, {KEYS[4], KEYS[5], KEYS[6]}}

local function lock(attempts, lockKey, levelKey)
  local level = redis.call('INCR', levelKey)
  local window = tonumber(ARGV[4 + math.min(level, windows)])
  redis.call('SET', lockKey, '1', 'EX', window)
  redis.call('EXPIRE', levelKey, window + memory)
  redis.call('DEL', attempts)
end

if mode == 'clear' then
  redis.call('DEL', KEYS[1], KEYS[2])
  if tonumber(redis.call('GET', KEYS[4]) or '0') > 0 then redis.call('DECRBY', KEYS[4], 1) end
  return 1
end

if mode == 'reserve' then
  if redis.call('EXISTS', KEYS[2]) == 1 or redis.call('EXISTS', KEYS[5]) == 1 then return 0 end
  local allowed = 1
  for _, k in ipairs(axes) do
    local n = redis.call('INCR', k[1])
    redis.call('EXPIRE', k[1], ARGV[3])
    if n > limit then
      lock(k[1], k[2], k[3])
      allowed = 0
    end
  end
  return allowed
end

for _, k in ipairs(axes) do
  if redis.call('EXISTS', k[2]) == 0 and tonumber(redis.call('GET', k[1]) or '0') >= limit then
    lock(k[1], k[2], k[3])
  end
end
return 1
`)

// Lockout enforces per-account AND per-IP exponential backoff in Redis. The two
// axes are independent: account lockout stops credential-stuffing one handle;
// IP lockout stops spraying many handles from one source. A guess is refused if
// EITHER axis is locked. Every window is a Redis TTL, so the now field is not
// read; it stays so NewLockout keeps its signature.
type Lockout struct {
	redis *redis.Client
	now   func() time.Time
}

// NewLockout wires a Lockout against Redis with an injectable clock. A nil clock
// defaults to time.Now (production).
func NewLockout(rdb *redis.Client, now func() time.Time) *Lockout {
	if now == nil {
		now = time.Now
	}
	return &Lockout{redis: rdb, now: now}
}

// hashIP returns the hex SHA-256 of an IP string. The raw IP is never stored —
// only this hash forms the Redis key suffix.
func hashIP(ip string) string {
	sum := sha256.Sum256([]byte(ip))
	return hex.EncodeToString(sum[:])
}

// windowForLevel is the lock window for the n-th consecutive lock of an axis:
// 2^(level-1) * lockoutBaseWindow, capped at lockoutMaxWindow. Level 0 and below
// have no lock.
func windowForLevel(level int64) time.Duration {
	if level < 1 {
		return 0
	}
	// 2^6 min = 64 min already exceeds the 60 min cap, so exp >= 6 saturates; the
	// explicit bound also prevents an overflowing shift on a huge level.
	exp := level - 1
	if exp >= 6 {
		return lockoutMaxWindow
	}
	window := time.Duration(int64(1)<<uint(exp)) * lockoutBaseWindow
	if window > lockoutMaxWindow {
		return lockoutMaxWindow
	}
	return window
}

// run executes lockoutScript for one mode. It takes the IP's hash, not the IP:
// Reset must name the IP that RESERVED the guess, which a later request cannot
// supply as a raw address without the raw IP being stored between the two steps.
func (l *Lockout) run(ctx context.Context, mode, username, ipHash string) (int64, error) {
	if l == nil || l.redis == nil {
		return 0, ErrLockoutUnavailable
	}
	args := make([]interface{}, 0, 4+lockoutMaxLevel)
	args = append(args, mode, lockoutThreshold,
		int64(lockoutAttemptsWindow/time.Second), int64(lockoutLevelMemory/time.Second))
	for level := int64(1); level <= lockoutMaxLevel; level++ {
		args = append(args, int64(windowForLevel(level)/time.Second))
	}
	return lockoutScript.Run(ctx, l.redis, []string{
		lockoutAcctPrefix + username, lockoutAcctLockPrefix + username, lockoutAcctLevelPrefix + username,
		lockoutIPPrefix + ipHash, lockoutIPLockPrefix + ipHash, lockoutIPLevelPrefix + ipHash,
	}, args...).Int64()
}

// Reserve counts a password guess on both axes before its password is checked,
// in one atomic step with the lock check. It returns false when the account or
// the IP is locked, or when this guess is the one that locks it. On an error the
// caller must refuse the guess as a server error — never as a lock, and never as
// a success — because an uncounted guess would reopen unlimited guessing.
func (l *Lockout) Reserve(ctx context.Context, username, ip string) (bool, error) {
	allowed, err := l.run(ctx, lockoutModeReserve, username, hashIP(ip))
	if err != nil {
		return false, fmt.Errorf("reserve admin login attempt: %w", err)
	}
	return allowed == 1, nil
}

// RecordCheckedFailure locks any axis whose reserved guess reached the threshold.
// The guess was already counted by Reserve, so this does not count it again.
func (l *Lockout) RecordCheckedFailure(ctx context.Context, username, ip string) error {
	if _, err := l.run(ctx, lockoutModeChecked, username, hashIP(ip)); err != nil {
		return fmt.Errorf("record admin login failure: %w", err)
	}
	return nil
}

// IsLocked reports whether the account OR the IP is currently locked. It is a
// pure read: it counts nothing, so a step that only re-checks (WebAuthnLogin)
// can never lock the axis it is checking.
func (l *Lockout) IsLocked(ctx context.Context, username, ip string) (bool, error) {
	if l == nil || l.redis == nil {
		return false, ErrLockoutUnavailable
	}
	n, err := l.redis.Exists(ctx, lockoutAcctLockPrefix+username, lockoutIPLockPrefix+hashIP(ip)).Result()
	if err != nil {
		return false, fmt.Errorf("check admin lock: %w", err)
	}
	return n > 0, nil
}

// Reset is called when a sign-in or enrollment completes. It drops the account's
// attempts and lock (the caller just proved the password and the hardware key)
// and refunds the one guess that sign-in reserved on the IP, but never lifts an
// IP lock or clears the rest of the IP count. It keeps both levels: a re-attack
// straight after a sign-in escalates from where it left off.
//
// reservedIPHash is hashIP of the address the PASSWORD step ran from, which the
// handler stored in the challenge. It is deliberately not the address of the
// request that completes the sign-in: the handle is a bearer value, so the two
// can differ, and refunding the completing request's address would let a holder
// of one valid account lower the count of a source that never reserved the guess.
// An empty hash (a challenge stored before this field existed) refunds nothing.
func (l *Lockout) Reset(ctx context.Context, username, reservedIPHash string) error {
	if _, err := l.run(ctx, lockoutModeClear, username, reservedIPHash); err != nil {
		return fmt.Errorf("reset lockout: %w", err)
	}
	return nil
}

// Unlock is the operator's way out (adminctl unlock): it clears the account's
// attempts, lock AND level, so the next sign-in starts from a clean first level.
// The IP axis is not touched; an IP lock expires within lockoutMaxWindow.
func (l *Lockout) Unlock(ctx context.Context, username string) error {
	if l == nil || l.redis == nil {
		return ErrLockoutUnavailable
	}
	if err := l.redis.Del(ctx, lockoutAcctPrefix+username, lockoutAcctLockPrefix+username, lockoutAcctLevelPrefix+username).Err(); err != nil {
		return fmt.Errorf("unlock admin account: %w", err)
	}
	return nil
}
