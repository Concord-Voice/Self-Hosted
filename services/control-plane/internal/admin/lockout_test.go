package admin_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// uniqueLockoutIP returns a per-run-unique synthetic IP so concurrent runs and
// re-runs against the shared test Redis do not collide (the lockout keys are
// hashed, but the inputs must still be unique to self-isolate).
func uniqueLockoutIP(prefix string) string {
	seed := uniqueAdminUsername("ip")
	sum := sha256.Sum256([]byte(prefix + "-" + seed))
	return fmt.Sprintf("2001:db8:%02x%02x:%02x%02x::1", sum[0], sum[1], sum[2], sum[3])
}

func acctLockKey(username string) string  { return "admin_lockout:acct_lock:" + username }
func acctLevelKey(username string) string { return "admin_lockout:acct_level:" + username }
func ipLockKey(ip string) string          { return "admin_lockout:ip_lock:" + ipHashOf(ip) }
func ipLevelKey(ip string) string         { return "admin_lockout:ip_level:" + ipHashOf(ip) }

func ipHashOf(ip string) string { return ipKey(ip)[len("admin_lockout:ip:"):] }

func keyExists(t *testing.T, rdb *redis.Client, key string) bool {
	t.Helper()
	n, err := rdb.Exists(context.Background(), key).Result()
	require.NoError(t, err)
	return n == 1
}

// lockOut counts guesses until the lock engages: five are allowed, the sixth is
// the one that locks.
func lockOut(t *testing.T, lk *admin.Lockout, user, ip string) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		ok, err := lk.Reserve(ctx, user, ip)
		require.NoError(t, err)
		require.True(t, ok, "guess %d is inside the threshold", i+1)
	}
	ok, err := lk.Reserve(ctx, user, ip)
	require.NoError(t, err)
	require.False(t, ok, "the sixth guess must lock")
}

// forceLock counts guesses until the lock engages, for a test whose account
// already holds some counted guesses.
func forceLock(t *testing.T, lk *admin.Lockout, user, ip string) {
	t.Helper()
	for i := 0; i < 10; i++ {
		ok, err := lk.Reserve(context.Background(), user, ip)
		require.NoError(t, err)
		if !ok {
			return
		}
	}
	t.Fatal("the lock never engaged")
}

// assertWindow checks a lock key's remaining TTL is within a few seconds of the
// window the level calls for. Redis TTLs are wall-clock, so it cannot be exact.
func assertWindow(t *testing.T, rdb *redis.Client, key string, want time.Duration, msg string) {
	t.Helper()
	ttl, err := rdb.TTL(context.Background(), key).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, want-10*time.Second, msg)
	assert.LessOrEqual(t, ttl, want, msg)
}

func TestLockout_Reserve_AllowsFiveThenLocks(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	lk := admin.NewLockout(rdb, nil)
	user, ip := uniqueAdminUsername("acct"), uniqueLockoutIP("1.2.3")

	lockOut(t, lk, user, ip)

	locked, err := lk.IsLocked(context.Background(), user, ip)
	require.NoError(t, err)
	assert.True(t, locked)
	assertWindow(t, rdb, acctLockKey(user), time.Minute, "the first lock is the base window")
	assertWindow(t, rdb, acctLevelKey(user), time.Minute+time.Hour, "the level outlives its lock by an hour")
	assertWindow(t, rdb, ipLevelKey(ip), time.Minute+time.Hour, "the IP level outlives its lock by an hour")
}

func TestLockout_ReserveAggregatesNativeIPv6ByPrefix(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	lk := admin.NewLockout(rdb, nil)
	username := uniqueAdminUsername("ipv6")

	for suffix := 1; suffix <= 5; suffix++ {
		allowed, err := lk.Reserve(context.Background(), username, fmt.Sprintf("2001:db8:1:2::%x", suffix))
		require.NoError(t, err)
		require.True(t, allowed)
	}
	allowed, err := lk.Reserve(context.Background(), username, "2001:db8:1:2::ff")
	require.NoError(t, err)
	require.False(t, allowed)

	locked, err := lk.IsLocked(context.Background(), username, "2001:db8:1:2::100")
	require.NoError(t, err)
	require.True(t, locked)
}

// A counted guess stays counted for an hour, so a patient attacker cannot stay
// under the threshold by pausing for a minute.
func TestLockout_Reserve_AttemptsStayCountedForAnHour(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	lk := admin.NewLockout(rdb, nil)
	user, ip := uniqueAdminUsername("acct"), uniqueLockoutIP("1.2.3")

	ok, err := lk.Reserve(context.Background(), user, ip)
	require.NoError(t, err)
	require.True(t, ok)

	assertWindow(t, rdb, acctKey(user), time.Hour, "account attempts window")
	assertWindow(t, rdb, ipKey(ip), time.Hour, "IP attempts window")
}

// The managed-Redis ACL only lets the control plane touch listed key patterns, so
// a lockout key kind missing from the manifest turns every admin sign-in into a
// 500 (the script gets NOPERM). Every prefix lockout.go writes must be listed.
func TestLockoutKeyPrefixesAreInTheManagedRedisACL(t *testing.T) {
	raw, err := os.ReadFile("../managedredis/acl-manifest.json")
	require.NoError(t, err)
	var manifest struct {
		Services map[string]struct {
			KeyPatterns []string `json:"keyPatterns"`
		} `json:"services"`
	}
	require.NoError(t, json.Unmarshal(raw, &manifest))
	patterns := manifest.Services["control-plane"].KeyPatterns

	for _, key := range []string{
		acctKey("x"), acctLockKey("x"), acctLevelKey("x"), ipKey("1.2.3.4"), ipLockKey("1.2.3.4"), ipLevelKey("1.2.3.4"),
		admin.ServerStepUpPrefix + strings.Repeat("cd", 32), // hex handle, as storeServerStepUp writes it
	} {
		listed := false
		for _, p := range patterns {
			if ok, _ := path.Match(p, key); ok {
				listed = true
				break
			}
		}
		assert.True(t, listed, "%q is not covered by a control-plane key pattern in acl-manifest.json", key)
	}
}

// A reserved guess that fails and reached the threshold locks at once, without
// waiting for another guess to arrive.
func TestLockout_RecordCheckedFailure_LocksAtThreshold(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	lk := admin.NewLockout(rdb, nil)
	user, ip := uniqueAdminUsername("acct"), uniqueLockoutIP("1.2.3")

	for i := 0; i < 4; i++ {
		ok, err := lk.Reserve(ctx, user, ip)
		require.NoError(t, err)
		require.True(t, ok)
		require.NoError(t, lk.RecordCheckedFailure(ctx, user, ip))
		locked, err := lk.IsLocked(ctx, user, ip)
		require.NoError(t, err)
		require.False(t, locked, "must not lock before the fifth failed guess (i=%d)", i)
	}
	ok, err := lk.Reserve(ctx, user, ip)
	require.NoError(t, err)
	require.True(t, ok, "the fifth guess is still checked")
	require.NoError(t, lk.RecordCheckedFailure(ctx, user, ip))

	locked, err := lk.IsLocked(ctx, user, ip)
	require.NoError(t, err)
	assert.True(t, locked, "the fifth failed guess must lock without a sixth")
	assert.Equal(t, int64(1), failCount(t, rdb, acctLevelKey(user)), "one lock, one level")
}

// RecordCheckedFailure must not count: calling it repeatedly on a count below the
// threshold never locks.
func TestLockout_RecordCheckedFailure_DoesNotCount(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	lk := admin.NewLockout(rdb, nil)
	user, ip := uniqueAdminUsername("acct"), uniqueLockoutIP("1.2.3")

	ok, err := lk.Reserve(ctx, user, ip)
	require.NoError(t, err)
	require.True(t, ok)
	for i := 0; i < 10; i++ {
		require.NoError(t, lk.RecordCheckedFailure(ctx, user, ip))
	}

	locked, err := lk.IsLocked(ctx, user, ip)
	require.NoError(t, err)
	assert.False(t, locked)
	assert.Equal(t, int64(1), failCount(t, rdb, acctKey(user)))
}

// A locked axis refuses before counting, so a hammering client cannot push the
// level or the count up while it is locked.
func TestLockout_LockedAxisIsNotCountedAgain(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	lk := admin.NewLockout(rdb, nil)
	user, ip := uniqueAdminUsername("acct"), uniqueLockoutIP("1.2.3")
	lockOut(t, lk, user, ip)

	for i := 0; i < 20; i++ {
		ok, err := lk.Reserve(ctx, user, ip)
		require.NoError(t, err)
		require.False(t, ok)
	}

	assert.Equal(t, int64(1), failCount(t, rdb, acctLevelKey(user)), "hammering a held lock must not escalate it")
	assert.Zero(t, failCount(t, rdb, acctKey(user)), "a refused guess is not counted")
}

// The level lives in its own key, so waiting out a lock does not buy a fresh
// first-level lock: each lock lasts twice as long as the one before, up to 60 min.
func TestLockout_EscalatesAcrossExpiry(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	user, ip := uniqueAdminUsername("acct"), uniqueLockoutIP("1.2.4")
	lk := admin.NewLockout(rdb, nil)

	want := []int{1, 2, 4, 8, 16, 32, 60, 60, 60}
	for i, minutes := range want {
		lockOut(t, lk, user, ip+string(rune('a'+i)))
		assertWindow(t, rdb, acctLockKey(user), time.Duration(minutes)*time.Minute, "lock window")
		// Expiry: the lock key goes, the level key stays.
		require.NoError(t, rdb.Del(ctx, acctLockKey(user)).Err())
		require.True(t, keyExists(t, rdb, acctLevelKey(user)), "the level must outlive the lock")
	}
}

func TestLockout_PerAccountIndependentOfPerIP(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	lk := admin.NewLockout(rdb, nil)

	userA := uniqueAdminUsername("acctA")
	userB := uniqueAdminUsername("acctB")
	ipShared := uniqueLockoutIP("9.9.9")
	ipOther := uniqueLockoutIP("8.8.8")

	lockOut(t, lk, userA, ipShared) // locks acct:userA AND ip:ipShared

	lockedA, err := lk.IsLocked(ctx, userA, ipOther)
	require.NoError(t, err)
	assert.True(t, lockedA, "userA's account lock holds regardless of IP")

	// userB from the SHARED ip is locked too — the per-IP lock holds even though
	// userB's own account has no failures. (per-IP defends against spraying.)
	lockedBShared, err := lk.IsLocked(ctx, userB, ipShared)
	require.NoError(t, err)
	assert.True(t, lockedBShared, "the per-IP lock blocks a second account from the same IP")
	allowed, err := lk.Reserve(ctx, userB, ipShared)
	require.NoError(t, err)
	assert.False(t, allowed, "and a guess from that IP is refused, not counted")
	assert.Zero(t, failCount(t, rdb, acctKey(userB)))

	// userB from a DIFFERENT ip is NOT locked: the two axes are independent.
	lockedBOther, err := lk.IsLocked(ctx, userB, ipOther)
	require.NoError(t, err)
	assert.False(t, lockedBOther, "an unrelated account+IP must not be locked")
}

// Reset drops the account's attempts and lock, refunds the one guess the sign-in
// reserved on the IP, keeps both levels, and never lifts an IP lock or clears the
// rest of an IP count: one admin proving their own identity says nothing about
// the guesses that came from the source.
func TestLockout_ResetClearsAccountAxisKeepsLevelsAndIPState(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	lk := admin.NewLockout(rdb, nil)
	user, ip := uniqueAdminUsername("acct"), uniqueLockoutIP("7.7.7")

	lockOut(t, lk, user, ip)
	require.NoError(t, lk.Reset(ctx, user, ipHashOf(ip)))

	assert.False(t, keyExists(t, rdb, acctLockKey(user)), "Reset must lift the account lock")
	assert.False(t, keyExists(t, rdb, acctKey(user)), "Reset must clear the account attempts")
	assert.True(t, keyExists(t, rdb, ipLockKey(ip)), "Reset must NOT lift an IP lock")
	locked, err := lk.IsLocked(ctx, uniqueAdminUsername("someone-else"), ip)
	require.NoError(t, err)
	assert.True(t, locked, "the IP stays locked for every other account")
	assert.False(t, keyExists(t, rdb, ipKey(ip)), "a refund on a locked IP must not recreate its deleted count")
	assert.Equal(t, int64(1), failCount(t, rdb, acctLevelKey(user)), "the account level survives a Reset")
	assert.Equal(t, int64(1), failCount(t, rdb, ipLevelKey(ip)), "the IP level survives a Reset")

	// A re-attack straight after a Reset escalates from the kept level.
	lockOut(t, lk, user, uniqueLockoutIP("5.5.4"))
	assertWindow(t, rdb, acctLockKey(user), 2*time.Minute, "the next lock is level 2")
}

// A completed sign-in refunds exactly the guess it reserved: three spray guesses
// from the same IP stay counted, so signing in cannot reset a spray budget, and a
// refund never takes the count below zero.
func TestLockout_Reset_RefundsOnlyItsOwnGuessOnTheIP(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	lk := admin.NewLockout(rdb, nil)
	ip := uniqueLockoutIP("8.8.4")
	for i := 0; i < 3; i++ {
		ok, err := lk.Reserve(ctx, uniqueAdminUsername("spray"), ip)
		require.NoError(t, err)
		require.True(t, ok)
	}
	user := uniqueAdminUsername("acct")
	ok, err := lk.Reserve(ctx, user, ip) // the sign-in's own password-step guess
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(4), failCount(t, rdb, ipKey(ip)))

	require.NoError(t, lk.Reset(ctx, user, ipHashOf(ip)))
	assert.Equal(t, int64(3), failCount(t, rdb, ipKey(ip)), "only the sign-in's own guess is refunded")

	for i := 0; i < 5; i++ { // more resets than guesses: the count floors at zero
		require.NoError(t, lk.Reset(ctx, user, ipHashOf(ip)))
	}
	assert.Equal(t, int64(0), failCount(t, rdb, ipKey(ip)), "a refund never goes below zero")
}

// The refund goes to the address that reserved the guess, whatever address the
// completing request came from: Reset takes the reserving address's hash, so a
// sign-in cannot lower the count of a source that never reserved its guess.
func TestLockout_Reset_RefundsTheReservingIPOnly(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	lk := admin.NewLockout(rdb, nil)
	reserving, spray := uniqueLockoutIP("8.8.5"), uniqueLockoutIP("8.8.6")
	user := uniqueAdminUsername("acct")
	for i := 0; i < 4; i++ {
		ok, err := lk.Reserve(ctx, uniqueAdminUsername("spray"), spray)
		require.NoError(t, err)
		require.True(t, ok)
	}
	ok, err := lk.Reserve(ctx, user, reserving)
	require.NoError(t, err)
	require.True(t, ok)

	require.NoError(t, lk.Reset(ctx, user, ipHashOf(reserving)))

	assert.Equal(t, int64(0), failCount(t, rdb, ipKey(reserving)), "the reserving address gets its guess back")
	assert.Equal(t, int64(4), failCount(t, rdb, ipKey(spray)), "an address that reserved nothing is untouched")

	require.NoError(t, lk.Reset(ctx, user, "")) // a challenge stored before the field existed
	assert.Equal(t, int64(4), failCount(t, rdb, ipKey(spray)), "an empty hash refunds nothing")
}

// Unlock is the operator's way out: it clears the account's lock, attempts and
// level, and leaves the IP axis alone.
func TestLockout_UnlockClearsAccountAxisOnly(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	lk := admin.NewLockout(rdb, nil)
	user, ip := uniqueAdminUsername("acct"), uniqueLockoutIP("5.5.5")
	lockOut(t, lk, user, ip)

	require.NoError(t, lk.Unlock(ctx, user))

	for _, k := range []string{acctKey(user), acctLockKey(user), acctLevelKey(user)} {
		assert.False(t, keyExists(t, rdb, k), "Unlock must clear %s", k)
	}
	// lockOut ends with the account's attempts already deleted by the lock itself,
	// so the loop above cannot prove Unlock clears them: count three guesses with no
	// lock and clear those.
	counted := uniqueAdminUsername("counted")
	for i := 0; i < 3; i++ {
		ok, err := lk.Reserve(ctx, counted, uniqueLockoutIP("5.5.6"))
		require.NoError(t, err)
		require.True(t, ok)
	}
	require.Equal(t, int64(3), failCount(t, rdb, acctKey(counted)))
	require.NoError(t, lk.Unlock(ctx, counted))
	assert.False(t, keyExists(t, rdb, acctKey(counted)), "Unlock must clear the running attempt count")
	for _, k := range []string{ipLockKey(ip), ipLevelKey(ip)} {
		assert.True(t, keyExists(t, rdb, k), "Unlock must leave the IP axis alone: %s", k)
	}
	// From another IP the account is usable and starts again at level 1.
	other := uniqueLockoutIP("4.4.4")
	lockOut(t, lk, user, other)
	assertWindow(t, rdb, acctLockKey(user), time.Minute, "after Unlock the next lock is level 1 again")
}

func TestLockout_RawIPNeverStored(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	lk := admin.NewLockout(rdb, nil)
	ip := "203.0.113.42"
	lockOut(t, lk, uniqueAdminUsername("acct"), ip)

	keys, err := rdb.Keys(ctx, "admin_lockout:ip*").Result()
	require.NoError(t, err)
	require.Len(t, keys, 2, "after a lock only the IP lock and level keys remain")
	for _, k := range keys {
		assert.NotContains(t, k, ip, "raw IP must never appear in a Redis key (it is SHA-256 hashed)")
	}
}

func TestLockout_NilClient_FailsClosed(t *testing.T) {
	lk := admin.NewLockout(nil, nil)
	ctx := context.Background()

	ok, err := lk.Reserve(ctx, "user", "1.2.3.4")
	assert.False(t, ok)
	assert.True(t, errors.Is(err, admin.ErrLockoutUnavailable))
	_, err = lk.IsLocked(ctx, "user", "1.2.3.4")
	assert.True(t, errors.Is(err, admin.ErrLockoutUnavailable))
	assert.True(t, errors.Is(lk.Unlock(ctx, "user"), admin.ErrLockoutUnavailable))
	assert.Error(t, lk.RecordCheckedFailure(ctx, "user", "1.2.3.4"))
	assert.Error(t, lk.Reset(ctx, "user", "1.2.3.4"))
}

func TestWindowForLevel(t *testing.T) {
	cases := map[int64]time.Duration{
		-1: 0, 0: 0,
		1: time.Minute, 2: 2 * time.Minute, 3: 4 * time.Minute, 4: 8 * time.Minute,
		5: 16 * time.Minute, 6: 32 * time.Minute, 7: 60 * time.Minute, 8: 60 * time.Minute,
		1000: 60 * time.Minute, 1 << 62: 60 * time.Minute,
	}
	for level, want := range cases {
		assert.Equal(t, want, admin.WindowForLevelForTest(level), "level %d", level)
	}
}

// The managed-Redis ACL checks the commands a Lua script calls, not just EVALSHA,
// so a command the script uses that the manifest does not grant fails every admin
// sign-in with NOPERM. Every redis.call in lockout.go (and the Go-side Exists and
// Del) must be in the control-plane command list. DECR, for one, is not: the
// manifest grants DECRBY.
func TestLockoutRedisCommandsAreInTheManagedRedisACL(t *testing.T) {
	raw, err := os.ReadFile("../managedredis/acl-manifest.json")
	require.NoError(t, err)
	var manifest struct {
		Services map[string]struct {
			Commands []string `json:"commands"`
		} `json:"services"`
	}
	require.NoError(t, json.Unmarshal(raw, &manifest))
	granted := map[string]bool{}
	for _, c := range manifest.Services["control-plane"].Commands {
		granted[c] = true
	}

	src, err := os.ReadFile("lockout.go")
	require.NoError(t, err)
	used := map[string]bool{"EVAL": true, "EVALSHA": true, "EXISTS": true, "DEL": true}
	matches := regexp.MustCompile(`redis\.call\('([A-Za-z]+)'`).FindAllStringSubmatch(string(src), -1)
	require.NotEmpty(t, matches, "the script's redis.call sites were not found")
	for _, m := range matches {
		used[strings.ToUpper(m[1])] = true
	}
	for cmd := range used {
		assert.True(t, granted[cmd], "%s is used by the lockout but is not granted to the control-plane in acl-manifest.json", cmd)
	}
}
