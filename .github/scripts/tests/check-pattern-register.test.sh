#!/usr/bin/env bash
#
# check-pattern-register.test.sh — Self-test for check-pattern-register.sh.
#
# Covers the four diff cases from plan §3.3 (added if:, removed if:, modified
# if:, no if: change) plus multi-file aggregation. Pattern mirrors
# lint-always-needs-comment.test.sh: assert_exit + assert_stderr_contains
# helpers, PASS/FAIL count at the end.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
SCRIPT="${REPO_ROOT}/.github/scripts/check-pattern-register.sh"
# shellcheck disable=SC2034  # Used in Task 2+ fixture-based tests; declared in
# Task 1's harness so subsequent tasks extend tests, not the harness skeleton.
FIXTURES="${SCRIPT_DIR}/fixtures/pattern-register"

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

# ── Diff-case helper: stage a fixture transition and run the script ────
# Usage: run_diff_case <before-fixture> <after-fixture> <label>
run_diff_case() {
  local before="$1"
  local after="$2"
  local label="$3"
  local sandbox
  sandbox=$(mktemp -d)
  cd "$sandbox"
  git init --quiet
  git config user.email test@example.com
  git config user.name "Test"
  mkdir -p .github/workflows
  cp "$before" .github/workflows/sample.yml
  git add .github/workflows/sample.yml
  git commit --quiet -m "init"
  cp "$after" .github/workflows/sample.yml
  git add .github/workflows/sample.yml

  assert_stderr_contains "pattern register drift check" "$label: warning fires" "$SCRIPT"
  assert_exit 0 "$label: exits 0 (warn-only)" "$SCRIPT"

  cd "$REPO_ROOT"
  rm -rf "$sandbox"
}

# ── Sanity tests ──────────────────────────────────────────────────────
echo "Sanity tests:"
assert_exit 0 "--help exits 0" "$SCRIPT" --help
assert_exit 2 "unknown flag exits 2" "$SCRIPT" --bogus

# ── Discovery tests ────────────────────────────────────────────────────
echo ""
echo "Discovery tests:"

# Trap-based cleanup so a failing test doesn't orphan /tmp sandboxes.
SANDBOX=""
SANDBOX2=""
MULTI_SANDBOX=""
cleanup_sandboxes() {
  [[ -n "$SANDBOX" ]] && rm -rf "$SANDBOX"
  [[ -n "$SANDBOX2" ]] && rm -rf "$SANDBOX2"
  [[ -n "$MULTI_SANDBOX" ]] && rm -rf "$MULTI_SANDBOX"
  return 0
}
trap cleanup_sandboxes EXIT

# Set up a sandbox git repo for staged-diff testing
SANDBOX=$(mktemp -d)
cd "$SANDBOX"
git init --quiet
git config user.email test@example.com
git config user.name "Test"
git commit --allow-empty --quiet -m "init"

# Case: no .github/workflows/ files modified — script exits 0 (no warning)
echo "test-content" > some-file.txt
git add some-file.txt
assert_exit 0 "no workflow change → exits 0" "$SCRIPT"

cd "$REPO_ROOT"
rm -rf "$SANDBOX"

# Case: a .github/workflows/ file modified with an if: clause added
SANDBOX2=$(mktemp -d)
cd "$SANDBOX2"
git init --quiet
git config user.email test@example.com
git config user.name "Test"
mkdir -p .github/workflows
cp "${FIXTURES}/case-04-no-if-change.yml" .github/workflows/sample.yml
git add .github/workflows/sample.yml
git commit --quiet -m "init"

# Now add an if: clause to the workflow
cat >> .github/workflows/sample.yml <<'WORKFLOW_EOF'
  test:
    runs-on: ubuntu-latest
    if: github.event_name == 'push'
    steps:
      - run: echo "test"
WORKFLOW_EOF
git add .github/workflows/sample.yml

assert_stderr_contains "pattern register drift check" "added if: → warning" "$SCRIPT"
assert_exit 0 "added if: → still exits 0 (warn-only)" "$SCRIPT"

cd "$REPO_ROOT"
rm -rf "$SANDBOX2"

# ── Diff-case coverage (added / removed / modified if:) ────────────────
echo ""
echo "Diff-case tests:"

run_diff_case \
  "${FIXTURES}/case-01-added.before.yml" \
  "${FIXTURES}/case-01-added.yml" \
  "case-01 added if:"

run_diff_case \
  "${FIXTURES}/case-02-removed.before.yml" \
  "${FIXTURES}/case-02-removed.yml" \
  "case-02 removed if:"

run_diff_case \
  "${FIXTURES}/case-03-modified.before.yml" \
  "${FIXTURES}/case-03-modified.yml" \
  "case-03 modified if:"

# ── Multi-file aggregation ─────────────────────────────────────────────
echo ""
echo "Multi-file aggregation:"

MULTI_SANDBOX=$(mktemp -d)

cd "$MULTI_SANDBOX"
git init --quiet
git config user.email test@example.com
git config user.name "Test"
mkdir -p .github/workflows
cp "${FIXTURES}/case-01-added.before.yml" .github/workflows/wf1.yml
cp "${FIXTURES}/case-02-removed.before.yml" .github/workflows/wf2.yml
git add .github/workflows/
git commit --quiet -m "init"

# Apply changes to BOTH workflows in one staged commit
cp "${FIXTURES}/case-01-added.yml" .github/workflows/wf1.yml
cp "${FIXTURES}/case-02-removed.yml" .github/workflows/wf2.yml
git add .github/workflows/

assert_stderr_contains "2 workflow(s) modify" "multi: counts both files" "$SCRIPT"
assert_stderr_contains "wf1.yml" "multi: names wf1" "$SCRIPT"
assert_stderr_contains "wf2.yml" "multi: names wf2" "$SCRIPT"
assert_exit 0 "multi: exits 0" "$SCRIPT"

cd "$REPO_ROOT"
rm -rf "$MULTI_SANDBOX"
MULTI_SANDBOX=""

# ── Summary ───────────────────────────────────────────────────────────
echo ""
echo "Results: $pass passed, $fail failed"
if [[ "$fail" -gt 0 ]]; then
  exit 1
fi
