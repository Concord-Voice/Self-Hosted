#!/usr/bin/env bash
#
# lint-boolean-coercion.test.sh — self-test for lint-boolean-coercion.sh.
#
# Covers spec §6.3 acceptance criteria:
#   1. Both `==` and `!=` variants of `inputs.X 'true'` patterns are
#      detected.
#   2. The two correct forms (`format('{0}', ...)`, `inputs.X == true`)
#      are NOT flagged.
#   3. YAML comments mentioning the bug pattern are NOT flagged
#      (mirrors the live-tree main-cd.yml:129 occurrence).
#   4. Sanity tests cover the canonical exit-code contract from sibling
#      lints (0 clean / 1 drift / 2 script error).
#
# Mirrors lint-always-needs-comment.test.sh: assert_exit + assert_stderr
# helpers, PASS/FAIL count at end, live-tree sanity as final step.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
SCRIPT="${REPO_ROOT}/.github/scripts/lint-boolean-coercion.sh"
FIXTURES="${SCRIPT_DIR}/fixtures/boolean-coercion"

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
assert_exit 2 "two targets exits 2" "$SCRIPT" "${FIXTURES}/case-01-bad-eq.yml" "${FIXTURES}/case-02-bad-neq.yml"

# ── Required cases (spec §6.3 acceptance) ─────────────────────────────
echo ""
echo "Required cases (spec §6.3):"

# Case 1: inputs.X == 'true' → exit 1, stderr names the file:line + matched substring.
assert_exit 1 "case 1: == 'true' exits 1" \
  "$SCRIPT" "${FIXTURES}/case-01-bad-eq.yml"

assert_stderr_contains "case-01-bad-eq.yml:15:" "case 1: stderr names file + line (line 15)" \
  "$SCRIPT" "${FIXTURES}/case-01-bad-eq.yml"

assert_stderr_contains "inputs.force_deploy == 'true'" "case 1: stderr quotes the matched pattern" \
  "$SCRIPT" "${FIXTURES}/case-01-bad-eq.yml"

assert_stderr_contains "suggested fix:" "case 1: stderr includes suggested-fix wording" \
  "$SCRIPT" "${FIXTURES}/case-01-bad-eq.yml"

# Case 2: inputs.X != 'true' → exit 1.
assert_exit 1 "case 2: != 'true' exits 1" \
  "$SCRIPT" "${FIXTURES}/case-02-bad-neq.yml"

assert_stderr_contains "inputs.skip_tests != 'true'" "case 2: stderr quotes the != variant" \
  "$SCRIPT" "${FIXTURES}/case-02-bad-neq.yml"

# Case 3: format('{0}', ...) == 'true' → exit 0 (correct form).
assert_exit 0 "case 3: format('{0}', inputs.X) == 'true' passes" \
  "$SCRIPT" "${FIXTURES}/case-03-good-format.yml"

# Case 4: inputs.X == true (no quotes) → exit 0 (correct form for type: boolean).
assert_exit 0 "case 4: inputs.X == true (no quotes) passes" \
  "$SCRIPT" "${FIXTURES}/case-04-good-bool.yml"

# Case 5: YAML comment quoting the bug pattern → exit 0 (must not FP on prose).
assert_exit 0 "case 5: YAML comment mentioning bug pattern passes" \
  "$SCRIPT" "${FIXTURES}/case-05-good-yaml-comment.yml"

# Case 6: double-quoted `"true"` variant — same bug class as case 1, different
# quote style. GHA YAML accepts both single- and double-quoted string literals;
# the expression evaluator treats them identically. The lint MUST flag this form.
assert_exit 1 "case 6: == \"true\" (double-quoted) exits 1" \
  "$SCRIPT" "${FIXTURES}/case-06-bad-eq-doublequote.yml"

assert_stderr_contains 'inputs.force_deploy == "true"' "case 6: stderr quotes the double-quoted variant" \
  "$SCRIPT" "${FIXTURES}/case-06-bad-eq-doublequote.yml"

# Directory scan: when the fixtures directory contains both bad and good cases,
# the lint should exit 1 (at least one bad). This proves the multi-file aggregator
# works.
echo ""
echo "Directory scan:"
assert_exit 1 "directory scan over fixtures dir exits 1 (mix of bad + good)" \
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
