#!/usr/bin/env bash
#
# lint-always-needs-comment.test.sh — Self-test for lint-always-needs-comment.sh.
#
# Covers the five non-negotiable fixtures from plan §5.3 plus a sanity
# check against the live `.github/workflows/build.yml` (its coverage
# `always()` uses are commented and must continue to pass).
#
# Pattern mirrors check-required-statuses.test.sh: assert_exit +
# assert_stderr_contains helpers, PASS/FAIL count at the end.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
SCRIPT="${REPO_ROOT}/.github/scripts/lint-always-needs-comment.sh"
FIXTURES="${SCRIPT_DIR}/fixtures/always-comment"

pass=0
fail=0

assert_exit() {
  local expected="$1"
  local label="$2"
  shift 2
  local actual
  set +e
  "$@" >/dev/null 2>&1
  actual=$?
  set -e
  if [[ "$actual" == "$expected" ]]; then
    echo "  ✓ $label (exit=$actual)"
    pass=$((pass + 1))
  else
    echo "  ✗ $label (expected exit=$expected, got exit=$actual)" >&2
    fail=$((fail + 1))
  fi
}

assert_stderr_contains() {
  local needle="$1"
  local label="$2"
  shift 2
  local err
  err=$(mktemp)
  set +e
  "$@" >/dev/null 2>"$err"
  set -e
  if grep -qF -- "$needle" "$err"; then
    echo "  ✓ $label (found: $needle)"
    pass=$((pass + 1))
  else
    echo "  ✗ $label (missing: $needle)" >&2
    echo "    stderr: $(cat "$err")" >&2
    fail=$((fail + 1))
  fi
  rm -f "$err"
}

# ── Sanity tests ──────────────────────────────────────────────────────
echo "Sanity tests:"
assert_exit 0 "--help exits 0" "$SCRIPT" --help
assert_exit 2 "missing target exits 2" "$SCRIPT"
assert_exit 2 "unknown flag exits 2" "$SCRIPT" --bogus
assert_exit 2 "nonexistent target exits 2" "$SCRIPT" /nonexistent/path.yml
assert_exit 2 "invalid window exits 2" "$SCRIPT" --window abc "${FIXTURES}/case-01-commented.yml"
assert_exit 2 "negative window exits 2" "$SCRIPT" --window 0 "${FIXTURES}/case-01-commented.yml"

# ── Required cases from plan §5.3 ─────────────────────────────────────
echo ""
echo "Required cases (plan §5.3):"

# Case 1: comment 3 lines above always() with `load-bearing` → exit 0
assert_exit 0 "case 1: commented (within window, keyword present)" \
  "$SCRIPT" "${FIXTURES}/case-01-commented.yml"

# Case 2: always() with no preceding comment → exit 1 + stderr names file + line
assert_exit 1 "case 2: no comment exits 1" \
  "$SCRIPT" "${FIXTURES}/case-02-no-comment.yml"

assert_stderr_contains "case-02-no-comment.yml:10" "case 2: stderr names file + line" \
  "$SCRIPT" "${FIXTURES}/case-02-no-comment.yml"

# Case 3: comment beyond default N=10 → exit 1
assert_exit 1 "case 3: comment too far exits 1" \
  "$SCRIPT" "${FIXTURES}/case-03-comment-too-far.yml"

# But: when window is widened to 20, case 3 should pass — proves the
# window flag actually controls the look-back distance.
assert_exit 0 "case 3 + --window 20: comment within widened window passes" \
  "$SCRIPT" --window 20 "${FIXTURES}/case-03-comment-too-far.yml"

# Env-var equivalent of --window also works.
assert_exit 0 "case 3 + ALWAYS_COMMENT_WINDOW=20 (env): passes" \
  env ALWAYS_COMMENT_WINDOW=20 "$SCRIPT" "${FIXTURES}/case-03-comment-too-far.yml"

# Case 4: comment present but no relevant keyword → exit 1, stderr names
# the keyword requirement
assert_exit 1 "case 4: wrong-keyword comment exits 1" \
  "$SCRIPT" "${FIXTURES}/case-04-wrong-keyword.yml"

assert_stderr_contains "always|load-bearing|cascade|skip" "case 4: stderr names keyword requirement" \
  "$SCRIPT" "${FIXTURES}/case-04-wrong-keyword.yml"

# Case 5: inline trailing comment on the same line as always() → exit 0
assert_exit 0 "case 5: inline trailing comment passes" \
  "$SCRIPT" "${FIXTURES}/case-05-inline-comment.yml"

# ── Live sanity: build.yml ────────────────────────────────────────────
echo ""
echo "Live sanity:"
LIVE_WORKFLOW="${REPO_ROOT}/.github/workflows/build.yml"
assert_exit 0 "live: build.yml has commented always() uses → exit 0" \
  "$SCRIPT" "$LIVE_WORKFLOW"

echo ""
echo "─────────────────────────────────────────"
echo "PASS: $pass  FAIL: $fail"
[[ "$fail" == "0" ]]
