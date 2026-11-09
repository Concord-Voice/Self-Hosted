package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
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
	return prefix + "-" + uniqueAdminUsername("ip")
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

// Reset drops attempts and lock on both axes but keeps the level.
func TestLockout_ResetClearsAttemptsAndLockButKeepsLevel(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	lk := admin.NewLockout(rdb, nil)
	user, ip := uniqueAdminUsername("acct"), uniqueLockoutIP("7.7.7")

	lockOut(t, lk, user, ip)
	ok, err := lk.Reserve(ctx, uniqueAdminUsername("other"), uniqueLockoutIP("6.6.6"))
	require.NoError(t, err)
	require.True(t, ok)

	require.NoError(t, lk.Reset(ctx, user, ip))

	locked, err := lk.IsLocked(ctx, user, ip)
	require.NoError(t, err)
	assert.False(t, locked, "Reset must lift the lock")
	for _, k := range []string{acctKey(user), ipKey(ip)} {
		assert.False(t, keyExists(t, rdb, k), "Reset must clear the attempts: %s", k)
	}
	assert.Equal(t, int64(1), failCount(t, rdb, acctLevelKey(user)), "the account level survives a Reset")
	assert.Equal(t, int64(1), failCount(t, rdb, ipLevelKey(ip)), "the IP level survives a Reset")

	// A re-attack straight after a Reset escalates from the kept level.
	lockOut(t, lk, user, ip)
	assertWindow(t, rdb, acctLockKey(user), 2*time.Minute, "the next lock is level 2")
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
