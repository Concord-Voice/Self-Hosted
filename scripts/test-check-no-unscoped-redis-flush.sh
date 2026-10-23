#!/usr/bin/env bash
# Tests for scripts/check-no-unscoped-redis-flush.sh — validates that the hook
# rejects unscoped Redis flushes and hand-rolled Redis clients in Go tests,
# while accepting redistest's own files, non-test Go files, and the legitimate
# miniredis / dead-address client idioms already in the tree.
#
# Refs #2680 — see [internal]specs/2026-08-16-2680-test-redis-isolation-design.md
#
# Style precedent: scripts/test-check-no-pg-tcp-probes.sh.
set -u

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
HOOK="$SCRIPT_DIR/check-no-unscoped-redis-flush.sh"

if [ ! -x "$HOOK" ]; then
  echo "FAIL: $HOOK not found or not executable"
  exit 1
fi

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

pass=0
fail=0

# run_case <name> <relative-path> <content> <expect_exit>
#   expect_exit=0 — must pass (no violation, or exempt)
#   expect_exit=1 — must fail (violation detected)
#
# The path is relative and preserved, because both the _test.go suffix check
# and the redistest allowlist are path-sensitive.
run_case() {
  local name="$1" relpath="$2" content="$3" expect_exit="$4"

  local f="$TMPDIR/$relpath"
  mkdir -p "$(dirname "$f")"
  printf '%s\n' "$content" > "$f"

  # A non-zero exit is the EXPECTED result for roughly half these cases, so
  # capture it explicitly rather than letting it be the command's status. This
  # form is correct under `set -e` as well as the `set -u` above — a future edit
  # adding -e for consistency with sibling scripts would otherwise abort the
  # suite on its first correctly-rejected case.
  local actual_exit=0
  ( cd "$TMPDIR" && "$HOOK" "$relpath" >/dev/null 2>&1 ) || actual_exit=$?

  if [ "$actual_exit" = "$expect_exit" ]; then
    echo "  ok: $name"
    pass=$((pass + 1))
  else
    echo "  FAIL: $name (expected exit $expect_exit, got $actual_exit)"
    fail=$((fail + 1))
  fi
}

echo "Rule 1 — unscoped flush:"
run_case "flushdb-method"   "internal/foo/a_test.go" 'require.NoError(t, rdb.FlushDB(ctx).Err())'          1
run_case "flushall-method"  "internal/foo/b_test.go" 'require.NoError(t, rdb.FlushAll(ctx).Err())'         1
run_case "flushdb-literal"  "internal/foo/c_test.go" 'client.Do(ctx, "FLUSHDB", "SYNC")'                   1
run_case "flushall-literal" "internal/foo/d_test.go" 'client.Do(ctx, "FLUSHALL")'                          1
run_case "reset-is-allowed" "internal/foo/e_test.go" 'require.NoError(t, redistest.Reset(ctx, rdb))'       0

echo "Rule 2 — hand-rolled client:"
run_case "parseurl"         "internal/foo/f_test.go" 'opts, err := redis.ParseURL(someURL)'                 1
run_case "client-helper-ok" "internal/foo/g_test.go" 'rdb := redistest.Client(t)'                           0

echo "Rule 3 — hand-rolled URL:"
run_case "getenv-redis-url" "internal/foo/h_test.go" 'url := os.Getenv("REDIS_URL")'                        1
run_case "url-helper-ok"    "internal/foo/i_test.go" 'url := redistest.URL(t)'                              0
run_case "other-getenv-ok"  "internal/foo/j_test.go" 'url := os.Getenv("DATABASE_URL")'                     0

echo "Exemptions:"
run_case "redistest-owns-flush" \
  "services/control-plane/internal/testhelpers/redistest/redistest.go" \
  'return c.Do(ctx, "FLUSHDB", "SYNC").Err()' 0
run_case "redistest-test-owns-all" \
  "services/control-plane/internal/testhelpers/redistest/redistest_test.go" \
  'opts, err := redis.ParseURL(x); _ = os.Getenv("REDIS_URL"); rdb.FlushDB(ctx)' 0
run_case "non-test-go-ignored" \
  "internal/foo/production.go" 'rdb.FlushDB(ctx)' 0
run_case "non-go-ignored" \
  "docs/notes.md" 'call FlushDB and redis.ParseURL here' 0

echo "Idioms already in the tree that must NOT be flagged:"
run_case "miniredis-client" "internal/foo/k_test.go" \
  'rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})' 0
run_case "dead-address-client" "internal/foo/l_test.go" \
  'down := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})' 0
run_case "options-from-testserver" "internal/foo/m_test.go" \
  'rdb := redis.NewClient(ts.Redis.Options())' 0

echo "Bypasses found in the #2789 review — each verified against the real guard:"
run_case "flushdb-backtick"      "internal/foo/o_test.go" 'client.Do(ctx, `FLUSHDB`, "SYNC")'                    1
run_case "flushall-backtick"     "internal/foo/p_test.go" 'client.Do(ctx, `FLUSHALL`)'                           1
run_case "flushdb-async"         "internal/foo/q_test.go" 'require.NoError(t, rdb.FlushDBAsync(ctx).Err())'      1
run_case "flushall-async"        "internal/foo/r_test.go" 'require.NoError(t, rdb.FlushAllAsync(ctx).Err())'     1
run_case "flushdb-lowercase"     "internal/foo/s_test.go" 'client.Do(ctx, "flushdb")'                            1
run_case "flush-via-eval-lua"    "internal/foo/t_test.go" "rdb.Eval(ctx, \"redis.call('flushdb')\", nil)"        1
run_case "options-literal-db"    "internal/foo/u_test.go" 'c := redis.NewClient(&redis.Options{Addr: a, DB: 1})' 1
run_case "options-literal-pw"    "internal/foo/v_test.go" 'c := redis.NewClient(&redis.Options{Addr: a, Password: p})' 1
run_case "lookupenv-redis-url"   "internal/foo/w_test.go" 'url, ok := os.LookupEnv("REDIS_URL")'                 1

echo "The original defect site is in scope (testhelpers/*.go, not just _test.go):"
run_case "testhelpers-non-test-go" \
  "services/control-plane/internal/testhelpers/testredis.go" 'rdb.FlushDB(ctx)'                                 1

echo "Must still NOT be flagged — regression guard for the widened regexes:"
run_case "miniredis-still-ok"    "internal/foo/x_test.go" 'rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})' 0
run_case "deadaddr-still-ok"     "internal/foo/y_test.go" 'down := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})' 0
run_case "redistest-helper-ok"   "internal/foo/z_test.go" 'opts := redistest.Options(t)'                         0
run_case "lookupenv-other-ok"    "internal/foo/aa_test.go" 'v, ok := os.LookupEnv("DATABASE_URL")'               0

echo "Unreadable files must NOT be reported clean (#2789 review):"
# A violating file the guard cannot read used to exit 0 with no output — the
# guard asserting "clean" about something it never scanned. Root bypasses file
# permissions entirely, so the chmod cases only mean anything as a normal user.
if [ "$(id -u)" -eq 0 ]; then
  echo "  skip: running as root — chmod 000 is not enforced, cases would be vacuous"
else
  unreadable_case() { # <name> <relpath> <content> <expect_exit>
    local f="$TMPDIR/$2"
    mkdir -p "$(dirname "$f")"
    printf '%s\n' "$3" > "$f"
    chmod 000 "$f"
    local actual_exit=0
    ( cd "$TMPDIR" && "$HOOK" "$2" >/dev/null 2>&1 ) || actual_exit=$?
    chmod 644 "$f" # so the EXIT trap can clean up
    if [ "$actual_exit" = "$4" ]; then
      echo "  ok: $1"; pass=$((pass + 1))
    else
      echo "  FAIL: $1 (expected exit $4, got $actual_exit)"; fail=$((fail + 1))
    fi
  }
  unreadable_case "unreadable-with-violation" "internal/foo/ab_test.go" 'rdb.FlushDB(ctx)' 1
  unreadable_case "unreadable-without-violation" "internal/foo/ac_test.go" 'rdb := redistest.Client(t)' 1
fi

# Absent stays skippable — pre-commit legitimately passes deleted paths.
run_case "nonexistent-path-ok" "internal/foo/does_not_exist_test.go" '' 0
rm -f "$TMPDIR/internal/foo/does_not_exist_test.go"
actual=0; ( cd "$TMPDIR" && "$HOOK" "internal/foo/does_not_exist_test.go" >/dev/null 2>&1 ) || actual=$?
if [ "$actual" = "0" ]; then
  echo "  ok: deleted-path-skipped"; pass=$((pass + 1))
else
  echo "  FAIL: deleted-path-skipped (expected exit 0, got $actual)"; fail=$((fail + 1))
fi

echo "Multiple violations in one file still exit 1:"
run_case "several" "internal/foo/n_test.go" \
  'rdb.FlushDB(ctx)
opts, _ := redis.ParseURL(u)
_ = os.Getenv("REDIS_URL")' 1

echo
echo "passed: $pass, failed: $fail"
[ "$fail" -eq 0 ] || exit 1
