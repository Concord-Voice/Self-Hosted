#!/usr/bin/env bash
#
# Self-test for lint-workflow-call-contract.sh.
#
# BOTH halves matter. The hazard half proves the guard can go red — the reason it
# exists is a startup_failure with zero jobs and no log, so a guard that cannot
# fire is worse than none. The safe half is equally load-bearing: this linter
# runs over every workflow in the repo, so a single false positive blocks all
# workflow changes. The inherit-from-workflow-level case is the specific shape
# that bit the first draft — pr-ci.yml's build and sonar jobs declare no
# permissions of their own and are perfectly valid.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
SCANNER="$REPO_ROOT/.github/scripts/lint-workflow-call-contract.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
case_number=0
CASE_ROOT=""

new_case() {
  case_number=$((case_number + 1))
  CASE_ROOT="$TMP/case-$case_number"
  mkdir -p "$CASE_ROOT/.github/workflows"
}

# write_callee <name> <workflow-level-perms-yaml-or-EMPTY> <job-perms-yaml-or-EMPTY>
write_callee() {
  local name="$1" toplevel="$2" jobperms="$3"
  {
    echo "name: $name"
    echo "on:"
    echo "  workflow_call:"
    [[ "$toplevel" != "EMPTY" ]] && { echo "permissions:"; echo "$toplevel"; }
    echo "jobs:"
    echo "  work:"
    [[ "$jobperms" != "EMPTY" ]] && { echo "    permissions:"; echo "$jobperms"; }
    echo "    runs-on: ubuntu-latest"
    echo "    steps: [{run: echo hi}]"
  } > "$CASE_ROOT/.github/workflows/$name.yml"
  # A mis-indented fixture parses as `permissions: null`, which silently means
  # "inherit" — the case then goes green (or red for the wrong reason) while
  # appearing to exercise a job-level grant. Assert the shape we meant to write.
  if [[ "$jobperms" != "EMPTY" ]]; then
    python3 - "$CASE_ROOT/.github/workflows/$name.yml" <<'CHECK'
import sys, yaml
doc = yaml.safe_load(open(sys.argv[1]))
perms = doc["jobs"]["work"].get("permissions")
if not isinstance(perms, dict) or not perms:
    raise SystemExit(f"FIXTURE BUG: {sys.argv[1]} job-level permissions parsed as {perms!r}")
CHECK
  fi
}

# write_caller <workflow-level-perms-yaml-or-EMPTY> <job-perms-yaml-or-EMPTY> <callee-name>
write_caller() {
  local toplevel="$1" jobperms="$2" callee="$3" uses="${4:-./.github/workflows/$3.yml}"
  {
    echo "name: caller"
    echo "on:"
    echo "  push:"
    echo "    branches: [main]"
    [[ "$toplevel" != "EMPTY" ]] && { echo "permissions:"; echo "$toplevel"; }
    echo "jobs:"
    echo "  call:"
    [[ "$jobperms" != "EMPTY" ]] && { echo "    permissions:"; echo "$jobperms"; }
    echo "    uses: $uses"
  } > "$CASE_ROOT/.github/workflows/caller.yml"
}

assert_exit() {
  local expected="$1" label="$2" output actual
  set +e
  output=$("$SCANNER" "$CASE_ROOT" 2>&1)
  actual=$?
  set -e
  if [[ "$actual" == "$expected" ]]; then
    echo "  PASS: $label"
    pass=$((pass + 1))
  else
    echo "  FAIL: $label (expected exit $expected, got $actual)" >&2
    printf '    %s\n' "$output" >&2
    fail=$((fail + 1))
  fi
}

echo "Hazards — the guard MUST go red:"

new_case
write_callee callee "  contents: read
  issues: read" EMPTY
write_caller "  contents: read" EMPTY callee
assert_exit 1 "caller misses a scope the callee needs (the live nightly bug)"

new_case
write_callee callee "  contents: read
  issues: read" EMPTY
fixture_git() {
  env -u GIT_INDEX_FILE -u GIT_DIR -u GIT_WORK_TREE git -C "$CASE_ROOT" "$@"
}
fixture_git init -q
fixture_git add .github/workflows/callee.yml
pinned="$(fixture_git -c user.name=Test -c user.email=test@example.invalid \
  commit-tree "$(fixture_git write-tree)" -m 'pinned fixture')"
write_callee callee "  contents: read" EMPTY
write_caller "  contents: read" EMPTY callee \
  "Concord-Voice/Self-Hosted/.github/workflows/callee.yml@$pinned"
assert_exit 1 "SHA-pinned call checks the committed contract, not the edited checkout"

new_case
write_callee callee "  contents: read" "      security-events: write"
write_caller "  contents: read" "      security-events: read" callee
assert_exit 1 "caller grants read where the callee needs write"

new_case
write_callee callee "  contents: read" "      checks: read"
write_caller "  contents: read
  checks: read" "      contents: read" callee
assert_exit 1 "job-level grant REPLACES workflow-level and drops a needed scope"

new_case
write_callee inner "  contents: read
  packages: read" EMPTY
{
  echo "name: middle"
  echo "on:"
  echo "  workflow_call:"
  echo "permissions:"
  echo "  contents: read"
  echo "jobs:"
  echo "  nested:"
  echo "    uses: ./.github/workflows/inner.yml"
} > "$CASE_ROOT/.github/workflows/middle.yml"
write_caller "  contents: read" EMPTY middle
assert_exit 1 "a nested callee's requirement is not covered"

new_case
write_caller "  contents: read" EMPTY ghost
assert_exit 1 "caller uses a workflow that does not exist"

new_case
write_callee callee "  contents: read" EMPTY
write_caller EMPTY EMPTY callee
assert_exit 1 "neither job nor workflow declares permissions (grant unverifiable)"

echo ""
echo "Safe forms — the guard MUST stay green:"

new_case
write_callee callee "  contents: read
  pull-requests: read" EMPTY
write_caller "  contents: read
  pull-requests: write" EMPTY callee
assert_exit 0 "job inherits a covering workflow-level grant (the pr-ci.yml shape)"

new_case
write_callee callee "  contents: read
  checks: read" EMPTY
write_caller "  contents: read" "      contents: read
      checks: read" callee
assert_exit 0 "job-level grant is an exact match"

new_case
write_callee callee "  contents: read" EMPTY
write_caller "  contents: read" "      contents: write
      checks: read" callee
assert_exit 0 "job-level grant is a strict superset (write covers read)"

new_case
write_callee callee "  contents: read
  issues: read" EMPTY
write_caller "  contents: read" "      write-all" callee
assert_exit 0 "blanket write-all covers every scope"

new_case
write_callee callee "  contents: read" EMPTY
write_caller "  read-all" EMPTY callee
assert_exit 0 "blanket read-all covers a read need"

new_case
write_callee callee "  contents: read" "      contents: none"
write_caller "  contents: read" EMPTY callee
assert_exit 0 "callee job explicitly drops to none"

new_case
mkdir -p "$CASE_ROOT/.github/workflows"
{
  echo "name: remote-caller"
  echo "on:"
  echo "  push:"
  echo "    branches: [main]"
  echo "permissions:"
  echo "  contents: read"
  echo "jobs:"
  echo "  call:"
  echo "    uses: google/osv-scanner-action/.github/workflows/osv-scanner-reusable.yml@v1"
} > "$CASE_ROOT/.github/workflows/caller.yml"
assert_exit 0 "remote callee is skipped (contents not resolvable from the tree)"

new_case
write_callee callee "  contents: read" EMPTY
{
  echo "name: caller"
  echo "on:"
  echo "  push:"
  echo "    branches: [main]"
  echo "permissions:"
  echo "  contents: read"
  echo "jobs:"
  echo "  normal:"
  echo "    runs-on: ubuntu-latest"
  echo "    steps: [{run: echo not-a-caller}]"
} > "$CASE_ROOT/.github/workflows/caller.yml"
assert_exit 0 "a workflow with no uses: jobs is ignored"

# ── secrets half of the contract ─────────────────────────────────────────────
# A secret declared `required: false` may be legitimately omitted, so "caller
# does not pass X" alone is not a defect. DISAGREEMENT between sibling callers
# is. These cases pin that distinction in both directions.

write_secret_callee() {
  cat > "$CASE_ROOT/.github/workflows/callee.yml" <<'YAML'
name: callee
on:
  workflow_call:
    secrets:
      TOKEN:
        required: false
permissions:
  contents: read
jobs:
  work:
    runs-on: ubuntu-latest
    steps: [{run: echo hi}]
YAML
}

# <name> <secrets-block-or-NONE>
write_secret_caller() {
  local name="$1" secrets="$2"
  {
    echo "name: $name"
    echo "on:"
    echo "  push:"
    echo "    branches: [main]"
    echo "permissions:"
    echo "  contents: read"
    echo "jobs:"
    echo "  call:"
    echo "    uses: ./.github/workflows/callee.yml"
    # NOT `[[ cond ]] && echo` — as the last statement in a function that form
    # returns 1 when the condition is false, and `set -e` then kills the suite
    # mid-run with no output. It cost exactly that here.
    if [[ "$secrets" != "NONE" ]]; then echo "$secrets"; fi
  } > "$CASE_ROOT/.github/workflows/$name.yml"
}

echo ""
echo "Secrets — disagreement between sibling callers:"

new_case
write_secret_callee
write_secret_caller alpha "    secrets:
      TOKEN: \${{ secrets.TOKEN }}"  # pragma: allowlist secret
write_secret_caller beta NONE
assert_exit 1 "one caller passes the secret and another omits it (the live sonar bug)"

new_case
write_secret_callee
write_secret_caller alpha NONE
write_secret_caller beta NONE
assert_exit 0 "NO caller passes it — consistent, so not a disagreement"

new_case
write_secret_callee
write_secret_caller alpha "    secrets:
      TOKEN: \${{ secrets.TOKEN }}"  # pragma: allowlist secret
write_secret_caller beta "    secrets:
      TOKEN: \${{ secrets.TOKEN }}"  # pragma: allowlist secret
assert_exit 0 "every caller passes it"

new_case
write_secret_callee
write_secret_caller alpha "    secrets:
      TOKEN: \${{ secrets.TOKEN }}"  # pragma: allowlist secret
write_secret_caller beta "    secrets: inherit"
assert_exit 0 "secrets: inherit forwards everything and is not an omission"

echo ""
echo "The checked-in tree:"
set +e
out=$("$SCANNER" "$REPO_ROOT" 2>&1); rc=$?
set -e
if [[ "$rc" == "0" ]]; then
  echo "  PASS: real repository passes"
  pass=$((pass + 1))
else
  echo "  FAIL: real repository is red" >&2
  printf '    %s\n' "$out" >&2
  fail=$((fail + 1))
fi

echo ""
echo "─────────────────────────────────────────"
echo "PASS: $pass  FAIL: $fail"
[[ "$fail" == "0" ]]
