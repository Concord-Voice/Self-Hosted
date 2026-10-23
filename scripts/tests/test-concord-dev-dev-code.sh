#!/usr/bin/env bash
# Regression test for concord-dev.sh `dev-code` verb.
#
# Verifies the verb extracts the latest dev-mode email verification code from
# the control-plane log (the code the email service logs when SMTP_HOST is
# empty). Each case runs in its own subshell that sources concord-dev.sh with a
# sandboxed LOG_DIR, honoring the script's source-vs-execute guard so main()
# never runs.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
TARGET="$PROJECT_ROOT/scripts/concord-dev.sh"

PASS=0
FAIL=0
pass() { echo "  ✓ $1"; PASS=$((PASS + 1)); }
fail() { echo "  ✗ $1"; FAIL=$((FAIL + 1)); }

# A representative log fixture: interleaved HTTP lines plus three DEV MODE codes
# to two recipients. Uses the real em-dash to mirror production log output.
FIXTURE='time=2026-06-15T03:00:00-04:00 level=INFO msg="HTTP Request" method=GET path=/health status=200
time=2026-06-15T03:01:00-04:00 level=INFO msg="DEV MODE — email verification code" to=alice@example.com code=111111
time=2026-06-15T03:02:00-04:00 level=INFO msg="HTTP Request" method=POST path=/api/v1/auth/register status=200
time=2026-06-15T03:03:00-04:00 level=INFO msg="DEV MODE — email verification code" to=bob@example.com code=222222
time=2026-06-15T03:04:00-04:00 level=INFO msg="DEV MODE — email verification code" to=alice@example.com code=333333'

# run_case <log-content|__NOLOG__> [extra setup before verb] -- <dev-code args...>
# Echoes "<exit-code>\n<stdout>". stderr is discarded.
run_case() {
  local content="$1"; shift
  local setup="$1"; shift   # extra shell run after source, before verb (e.g. "QUIET=1")
  local tmp; tmp="$(mktemp -d)"
  [[ "$content" != "__NOLOG__" ]] && printf '%s\n' "$content" > "$tmp/control-plane.log"
  local out rc
  # `${setup:-:}` keeps an empty setup from collapsing to a `; ;` syntax error.
  out="$(LOG_DIR="$tmp" bash -c 'source "$1"; '"${setup:-:}"'; verb_dev_code "${@:2}"' _ "$TARGET" "$@" 2>/dev/null)"
  rc=$?
  rm -rf "$tmp"
  printf '%s\n%s' "$rc" "$out"
}

# ── happy: no filter returns the LATEST code overall ──────────────────────
out="$(run_case "$FIXTURE" "" )"
rc="${out%%$'\n'*}"; body="${out#*$'\n'}"
if [[ "$rc" == 0 && "$body" == *"333333"* && "$body" != *"111111"* ]]; then
  pass "no filter → latest code (333333)"
else
  fail "no filter → latest code (333333) [rc=$rc body=$body]"
fi

# ── happy: email filter selects that recipient's code, not the global latest ─
out="$(run_case "$FIXTURE" "" bob@example.com)"
rc="${out%%$'\n'*}"; body="${out#*$'\n'}"
if [[ "$rc" == 0 && "$body" == *"222222"* && "$body" != *"333333"* ]]; then
  pass "email filter → that recipient's code (bob → 222222)"
else
  fail "email filter → bob's code (222222) [rc=$rc body=$body]"
fi

# ── happy: filter still picks the LATEST for a recipient with multiple codes ─
out="$(run_case "$FIXTURE" "" alice@example.com)"
rc="${out%%$'\n'*}"; body="${out#*$'\n'}"
if [[ "$rc" == 0 && "$body" == *"333333"* && "$body" != *"111111"* ]]; then
  pass "email filter → latest of recipient's codes (alice → 333333)"
else
  fail "email filter → alice's latest (333333) [rc=$rc body=$body]"
fi

# ── happy: --quiet emits ONLY the bare code (scriptable / REPL-friendly) ─────
out="$(run_case "$FIXTURE" "QUIET=1")"
rc="${out%%$'\n'*}"; body="${out#*$'\n'}"
if [[ "$rc" == 0 && "$body" == "333333" ]]; then
  pass "quiet mode → bare code only (333333)"
else
  fail "quiet mode → bare code only [rc=$rc body='$body']"
fi

# ── error: no log file → non-zero exit ──────────────────────────────────────
out="$(run_case "__NOLOG__" "")"
rc="${out%%$'\n'*}"
if [[ "$rc" == 1 ]]; then
  pass "missing log → exit 1"
else
  fail "missing log → exit 1 [rc=$rc]"
fi

# ── error: log present but no DEV MODE line → non-zero exit ──────────────────
out="$(run_case 'time=2026-06-15T03:00:00-04:00 level=INFO msg="HTTP Request" status=200' "")"
rc="${out%%$'\n'*}"
if [[ "$rc" == 1 ]]; then
  pass "log without a code → exit 1"
else
  fail "log without a code → exit 1 [rc=$rc]"
fi

# ── error: unknown option → exit 2 ──────────────────────────────────────────
out="$(run_case "$FIXTURE" "" --bogus)"
rc="${out%%$'\n'*}"
if [[ "$rc" == 2 ]]; then
  pass "unknown option → exit 2"
else
  fail "unknown option → exit 2 [rc=$rc]"
fi

# ── exact-match filter — a filter is NOT a loose substring of a longer address ─
# Locks the Gitar #1533 fix: the old `grep -F "to=$x"` also matched `to=$x.evil`.
# The `.evil` line is newer, so the old substring match would have returned it.
SUBSTR_FIXTURE='time=2026-06-15T03:00:00-04:00 level=INFO msg="DEV MODE — email verification code" to=alice@example.com code=111111
time=2026-06-15T03:05:00-04:00 level=INFO msg="DEV MODE — email verification code" to=alice@example.com.evil code=999999'
out="$(run_case "$SUBSTR_FIXTURE" "" alice@example.com)"
rc="${out%%$'\n'*}"; body="${out#*$'\n'}"
if [[ "$rc" == 0 && "$body" == *"111111"* && "$body" != *"999999"* ]]; then
  pass "filter is exact, not substring (alice@example.com ≠ alice@example.com.evil)"
else
  fail "exact-match filter [rc=$rc body=$body]"
fi

echo
echo "dev-code verb: $PASS passed, $FAIL failed"
[[ "$FAIL" -eq 0 ]]
