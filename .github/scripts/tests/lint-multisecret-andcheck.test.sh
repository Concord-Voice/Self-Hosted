#!/usr/bin/env bash
#
# lint-multisecret-andcheck.test.sh — Self-test for lint-multisecret-andcheck.sh.
#
# Mirrors the assert-helper pattern from check-required-statuses.test.sh.
# Each fixture under tests/fixtures/multisecret/ exercises one branch of
# the AND-check completeness rule. A final sanity test runs the linter
# against the live .github/workflows/ directory and expects exit 0
# (validates today's repo passes).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
SCRIPT="${REPO_ROOT}/.github/scripts/lint-multisecret-andcheck.sh"
FIXTURES="${SCRIPT_DIR}/fixtures/multisecret"
LIVE_WORKFLOWS="${REPO_ROOT}/.github/workflows"

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
assert_exit 2 "unknown flag exits 2" "$SCRIPT" --bogus foo.yml
assert_exit 2 "nonexistent file exits 2" "$SCRIPT" /nonexistent/file.yml
assert_exit 2 "nonexistent --groups exits 2" "$SCRIPT" \
  --groups /nonexistent/groups.json "${FIXTURES}/case-01-group-complete.yml"

# ── Plan §5.2 test cases ─────────────────────────────────────────────
echo ""
echo "Plan §5.2 cases:"

assert_exit 0 "case 1: complete apple-p12 AND-check" \
  "$SCRIPT" "${FIXTURES}/case-01-group-complete.yml"

assert_exit 1 "case 2: missing APPLE_CERTIFICATE_PASSWORD → exit 1" \
  "$SCRIPT" "${FIXTURES}/case-02-missing-password.yml"

assert_stderr_contains "apple-p12" "case 2: stderr names the group" \
  "$SCRIPT" "${FIXTURES}/case-02-missing-password.yml"

assert_stderr_contains "APPLE_CERTIFICATE_PASSWORD" "case 2: stderr names the missing secret" \
  "$SCRIPT" "${FIXTURES}/case-02-missing-password.yml"

assert_exit 0 "case 3: no secret reference → exit 0" \
  "$SCRIPT" "${FIXTURES}/case-03-no-secret-ref.yml"

assert_exit 0 "case 4: no if clauses → exit 0" \
  "$SCRIPT" "${FIXTURES}/case-04-no-if-clauses.yml"

assert_exit 1 "case 5: bash incomplete (missing companion) → exit 1" \
  "$SCRIPT" "${FIXTURES}/case-05-bash-incomplete.yml"

assert_stderr_contains "APPLE_CERTIFICATE_PASSWORD" "case 5: stderr names the missing secret" \
  "$SCRIPT" "${FIXTURES}/case-05-bash-incomplete.yml"

# Case 6: malformed YAML → exit 2 (script error), NOT exit 1.
# Regression guard: an earlier revision treated YAML parse failures as
# policy violations (exit 1), which conflicted with the contract
# documented in the script header. Mirrors §5.1's case-05 behavior.
assert_exit 2 "case 6: malformed YAML → exit 2 (script error)" \
  "$SCRIPT" "${FIXTURES}/case-06-malformed.yml"

assert_stderr_contains "YAML parse error" "case 6: stderr names YAML parse error" \
  "$SCRIPT" "${FIXTURES}/case-06-malformed.yml"

# ── Live workflow sanity check ───────────────────────────────────────
echo ""
echo "Live workflow sanity:"
assert_exit 0 "live workflows lint clean (exit 0)" \
  "$SCRIPT" "$LIVE_WORKFLOWS"

# ── Directory mode ───────────────────────────────────────────────────
echo ""
echo "Directory mode:"
TMP=$(mktemp -d)
trap "rm -rf '$TMP'" EXIT
cp "${FIXTURES}/case-01-group-complete.yml" "$TMP/"
cp "${FIXTURES}/case-03-no-secret-ref.yml" "$TMP/"
assert_exit 0 "directory with only-clean fixtures → exit 0" "$SCRIPT" "$TMP"

cp "${FIXTURES}/case-02-missing-password.yml" "$TMP/"
assert_exit 1 "directory with one violating fixture → exit 1" "$SCRIPT" "$TMP"

echo ""
echo "─────────────────────────────────────────"
echo "PASS: $pass  FAIL: $fail"
[[ "$fail" == "0" ]]
