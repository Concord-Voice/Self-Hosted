#!/usr/bin/env bash
#
# lint-gh-repo-flag.test.sh — self-test for lint-gh-repo-flag.sh.
#
# Covers spec §6.5 acceptance criteria:
#   1. Detects unguarded gh call in a no-checkout job (exit 1).
#   2. Accepts gh call with explicit --repo flag (exit 0).
#   3. Accepts gh call in a job whose own steps include actions/checkout
#      (exit 0).
#   4. Accepts gh call in a job whose `needs:` cascade includes a job
#      with actions/checkout (exit 0).
#   5. Skips gh tokens appearing inside bash comments (exit 0).
#   6. Detects unguarded gh call when the `needs:` cascade has no
#      checkout anywhere (exit 1) — proves the transitive walker
#      doesn't accept any-presence-of-needs as proof of context.
#
# Mirrors lint-always-needs-comment.test.sh structure.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
SCRIPT="${REPO_ROOT}/.github/scripts/lint-gh-repo-flag.sh"
FIXTURES="${SCRIPT_DIR}/fixtures/gh-repo-flag"

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

# ── Required cases (spec §6.5 acceptance) ─────────────────────────────
echo ""
echo "Required cases (spec §6.5):"

# Case 1: no checkout, no --repo → exit 1.
assert_exit 1 "case 1: gh call without checkout and without --repo exits 1" \
  "$SCRIPT" "${FIXTURES}/case-01-bad-no-checkout-no-repo.yml"

assert_stderr_contains "case-01-bad-no-checkout-no-repo.yml:" "case 1: stderr names the file" \
  "$SCRIPT" "${FIXTURES}/case-01-bad-no-checkout-no-repo.yml"

assert_stderr_contains "--repo" "case 1: stderr mentions the missing --repo flag" \
  "$SCRIPT" "${FIXTURES}/case-01-bad-no-checkout-no-repo.yml"

assert_stderr_contains "notify" "case 1: stderr names the offending job key" \
  "$SCRIPT" "${FIXTURES}/case-01-bad-no-checkout-no-repo.yml"

# Case 2: --repo present → exit 0.
assert_exit 0 "case 2: gh call with --repo flag passes" \
  "$SCRIPT" "${FIXTURES}/case-02-good-with-repo-flag.yml"

# Case 3: own checkout in same job → exit 0.
assert_exit 0 "case 3: gh call with own actions/checkout passes" \
  "$SCRIPT" "${FIXTURES}/case-03-good-own-checkout.yml"

# Case 4: needs-cascade with upstream checkout → exit 0.
assert_exit 0 "case 4: gh call in dependent job (needs: has checkout) passes" \
  "$SCRIPT" "${FIXTURES}/case-04-good-needs-cascade.yml"

# Case 5: gh in bash comment → exit 0.
assert_exit 0 "case 5: gh inside a bash comment is not a real call → passes" \
  "$SCRIPT" "${FIXTURES}/case-05-good-gh-in-bash-comment.yml"

# Case 6: transitive needs but no checkout anywhere upstream → exit 1.
assert_exit 1 "case 6: needs-cascade WITHOUT upstream checkout exits 1" \
  "$SCRIPT" "${FIXTURES}/case-06-bad-transitive-no-checkout.yml"

# Case 7: multi-line gh call (backslash continuation), --repo absent on every
# continuation line → exit 1. Confirms `join_continuations` correctly joins the
# multi-line command and the scanner flags absence of --repo on the joined form.
assert_exit 1 "case 7: multi-line gh call without --repo exits 1" \
  "$SCRIPT" "${FIXTURES}/case-07-bad-multiline-no-repo.yml"

# Case 8: a bash comment ending with `\` followed by a real gh call. Regression
# guard for the fix to join_continuations: bash does NOT honor `\` continuation
# on comment lines, so the gh call must NOT be swallowed into the comment. Pre-
# fix, this fixture exited 0 (silent-clean). Post-fix, the lint flags the missing
# --repo on the gh call as expected.
assert_exit 1 "case 8: gh call after backslashed comment exits 1 (regression guard for continuation fix)" \
  "$SCRIPT" "${FIXTURES}/case-08-bad-gh-after-comment-continuation.yml"

# Directory scan: mix of bad and good → exit 1.
echo ""
echo "Directory scan:"
assert_exit 1 "directory scan over fixtures exits 1 (mix)" \
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
