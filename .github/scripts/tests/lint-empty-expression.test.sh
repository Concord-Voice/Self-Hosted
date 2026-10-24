#!/usr/bin/env bash
#
# lint-empty-expression.test.sh — self-test for lint-empty-expression.sh.
#
# Covers spec §6.4 acceptance criteria:
#   1. Empty ${{ }} token inside a run: block is flagged.
#   2. ${{ }} token inside a bash comment within a run: block is flagged.
#   3. YAML-level comments mentioning the patterns are NOT flagged
#      (they fall outside the GHA expression evaluator's scan path).
#   4. Well-formed ${{ env.X }} tokens are NOT flagged.
#   5. Single-line run: form is scanned in-place (no in-block state).
#
# Mirrors lint-always-needs-comment.test.sh structure.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
SCRIPT="${REPO_ROOT}/.github/scripts/lint-empty-expression.sh"
FIXTURES="${SCRIPT_DIR}/fixtures/empty-expression"

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
assert_exit 2 "two targets exits 2" "$SCRIPT" "${FIXTURES}/case-01-bad-empty-token.yml" "${FIXTURES}/case-02-bad-token-in-comment.yml"

# ── Required cases (spec §6.4 acceptance) ─────────────────────────────
echo ""
echo "Required cases (spec §6.4):"

# Case 1: empty ${{ }} in run block → exit 1.
assert_exit 1 "case 1: empty \${{ }} in run block exits 1" \
  "$SCRIPT" "${FIXTURES}/case-01-bad-empty-token.yml"

assert_stderr_contains "case-01-bad-empty-token.yml:14:" "case 1: stderr names file + line of the empty token" \
  "$SCRIPT" "${FIXTURES}/case-01-bad-empty-token.yml"

assert_stderr_contains "empty" "case 1: stderr classifies the bug as 'empty'" \
  "$SCRIPT" "${FIXTURES}/case-01-bad-empty-token.yml"

# Case 2: ${{ ... }} in bash comment within run block → exit 1.
assert_exit 1 "case 2: \${{ }} in bash comment inside run exits 1" \
  "$SCRIPT" "${FIXTURES}/case-02-bad-token-in-comment.yml"

assert_stderr_contains "bash comment" "case 2: stderr classifies the bug as 'bash comment'" \
  "$SCRIPT" "${FIXTURES}/case-02-bad-token-in-comment.yml"

# Case 3: proper ${{ env.X }} in run block → exit 0.
assert_exit 0 "case 3: \${{ env.MY_VAR }} (proper) passes" \
  "$SCRIPT" "${FIXTURES}/case-03-good-proper-token.yml"

# Case 4: YAML-level comments mentioning the pattern → exit 0.
assert_exit 0 "case 4: YAML comments mentioning bug patterns pass" \
  "$SCRIPT" "${FIXTURES}/case-04-good-yaml-comment.yml"

# Case 5: single-line run: with well-formed token → exit 0.
assert_exit 0 "case 5: single-line run: with proper token passes" \
  "$SCRIPT" "${FIXTURES}/case-05-good-inline-run.yml"

# Case 6: single-line run: with a trailing bash comment containing ${{ ... }}
# → exit 1. Exercises the single-line scanner code path (script lines 173-184),
# which scans content in place without entering the in-block state machine.
assert_exit 1 "case 6: single-line run: with bash-comment containing expression token exits 1" \
  "$SCRIPT" "${FIXTURES}/case-06-bad-inline-comment.yml"

assert_stderr_contains "bash comment" "case 6: stderr classifies as bash-comment-in-single-line" \
  "$SCRIPT" "${FIXTURES}/case-06-bad-inline-comment.yml"

# Directory scan
echo ""
echo "Directory scan:"
assert_exit 1 "directory scan over fixtures exits 1 (bad cases dominate)" \
  "$SCRIPT" "${FIXTURES}"

# ── Live sanity ───────────────────────────────────────────────────────
echo ""
echo "Live sanity:"
LIVE_WORKFLOWS="${REPO_ROOT}/.github/workflows"
assert_exit 0 "live: .github/workflows/ is currently clean → exit 0" \
  "$SCRIPT" "$LIVE_WORKFLOWS"

echo ""
echo "─────────────────────────────────────────"
echo "PASS: $pass  FAIL: $fail"
[[ "$fail" == "0" ]]
