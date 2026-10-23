#!/usr/bin/env bash
# Plain-shell test harness for supply-chain scanner scripts.
# No external dependencies (no bats) — consistent with the style used by
# check-telemetry-policy-version.sh and other existing shell tooling.

# Note: intentionally -eo not -euo — empty-array expansion (FAILED_TESTS[@]) is
# unbound under bash 3.2 (macOS), which does not support bash 5's empty-array fix.
set -eo pipefail

# Clean up the stderr-capture temp file on any exit (including Ctrl-C).
trap 'rm -f "/tmp/harness-stderr.$$"' EXIT

# Unset CI env vars that scan-supply-chain-iocs.sh's resolve_diff_base honors.
# If these leak in from a GitHub Actions runner (GITHUB_BASE_REF=main etc.),
# the scanner picks up the real CI repo's base instead of the fake test repo
# and subcheck 3's diff compares the wrong refs. Subshells spawned by command
# substitution inherit the harness env, so unsetting here propagates to every
# test's $("$SCANNER" ...) invocation.
unset GITHUB_BASE_REF GITHUB_HEAD_REF PRE_COMMIT_FROM_REF PRE_COMMIT_TO_REF
unset IOC_LIST_PATH IOC_ALLOWLIST_PATH RUNNER_ALLOWLIST_PATH

HARNESS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HARNESS_DIR/../../.." && pwd)"
FIXTURES="$HARNESS_DIR/fixtures"
SCANNER="$REPO_ROOT/scripts/security/scan-supply-chain-iocs.sh"
REFRESH_GUARD="$REPO_ROOT/scripts/security/check-ioc-refresh-pr.sh"
# shellcheck disable=SC2034  # used by runner-linter tests added in Task 6
RUNNER_LINTER="$REPO_ROOT/scripts/security/check-self-hosted-runners.sh"

PASS=0
FAIL=0
FAILED_TESTS=()

# Seed a fake repo root at a tmpdir and echo its path.
# The caller cds into it before invoking the scanner.
setup_test_repo() {
  local tmpdir
  tmpdir=$(mktemp -d)
  mkdir -p "$tmpdir"/client/desktop \
           "$tmpdir"/services/media-plane \
           "$tmpdir"/.github/tools/license-checker \
           "$tmpdir"/scripts/security/iocs
  cp "$FIXTURES/lockfile-clean.json" "$tmpdir/client/desktop/package-lock.json"
  cp "$FIXTURES/lockfile-clean.json" "$tmpdir/services/media-plane/package-lock.json"
  cp "$FIXTURES/lockfile-clean.json" "$tmpdir/.github/tools/license-checker/package-lock.json"
  cp "$FIXTURES/fake-ioc-list.txt" "$tmpdir/scripts/security/iocs/shai-hulud-2.0.txt"
  : > "$tmpdir/scripts/security/iocs/false-positive-allowlist.txt"
  # Initialize as a git repo so subcheck 3's diff logic has a base to compare.
  ( cd "$tmpdir" \
    && git init -q -b main \
    && git config user.email "test@example.com" \
    && git config user.name "Test Harness" \
    && git add -A \
    && git -c commit.gpgsign=false commit -q -m "initial fixture" )
  echo "$tmpdir"
}

# Run a named test function; print PASS/FAIL; track counts.
run_test() {
  local name="$1"
  cd "$REPO_ROOT"
  if "$name" 2>/tmp/harness-stderr.$$; then
    echo "  PASS: $name"
    PASS=$((PASS+1))
  else
    local rc=$?
    echo "  FAIL: $name (rc=$rc)"
    cat /tmp/harness-stderr.$$ >&2 || true
    FAIL=$((FAIL+1))
    FAILED_TESTS+=("$name")
  fi
  rm -f /tmp/harness-stderr.$$
}

# ── Test cases ─────────────────────────────────────────────────────────────
# Each test function:
#   - calls setup_test_repo (or sets up manually)
#   - cds into the fake root
#   - invokes the scanner
#   - returns 0 on pass, non-zero on fail

test_subcheck1_payload_at_root_fails() {
  local tmpdir; tmpdir=$(setup_test_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  # Plant the payload.
  : > bun_environment.js

  local output exit_code=0
  output=$("$SCANNER" --working-tree-only 2>&1) || exit_code=$?

  # Must exit 2 and print a FAIL line.
  [[ $exit_code -eq 2 ]] || { echo "expected exit 2, got $exit_code" >&2; return 1; }
  echo "$output" | grep -q "\[FAIL\] Shai-Hulud payload file:" \
    || { echo "expected FAIL line for payload file" >&2; return 1; }
}

test_subcheck1_payload_symlink_fails() {
  local tmpdir; tmpdir=$(setup_test_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  printf 'console.log("payload")\n' > payload.js
  ln -s payload.js bun_environment.js

  local output exit_code=0
  output=$("$SCANNER" --working-tree-only 2>&1) || exit_code=$?
  [[ $exit_code -eq 2 ]] || { echo "expected exit 2 for payload symlink, got $exit_code: $output" >&2; return 1; }
  echo "$output" | grep -q '\[FAIL\] Shai-Hulud payload file: ./bun_environment.js' \
    || { echo "expected symlink finding, got: $output" >&2; return 1; }
}

test_subcheck1_clean_tree_passes() {
  local tmpdir; tmpdir=$(setup_test_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  local output exit_code=0
  output=$("$SCANNER" --working-tree-only 2>&1) || exit_code=$?

  [[ $exit_code -eq 0 ]] || { echo "expected exit 0, got $exit_code: $output" >&2; return 1; }
  ! echo "$output" | grep -q "\[FAIL\]" \
    || { echo "unexpected FAIL line: $output" >&2; return 1; }
}

test_subcheck1_payload_in_test_fixtures_excluded() {
  local tmpdir; tmpdir=$(setup_test_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  # Plant inside the tests/ path — should be excluded.
  mkdir -p scripts/security/tests
  : > scripts/security/tests/bun_environment.js

  local output exit_code=0
  output=$("$SCANNER" --working-tree-only 2>&1) || exit_code=$?

  [[ $exit_code -eq 0 ]] || { echo "expected exit 0 (excluded), got $exit_code" >&2; return 1; }
}

test_subcheck1_payload_in_node_modules_default_excluded() {
  local tmpdir; tmpdir=$(setup_test_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  mkdir -p node_modules/some-pkg
  : > node_modules/some-pkg/bun_environment.js

  local output exit_code=0
  output=$("$SCANNER" --working-tree-only 2>&1) || exit_code=$?

  [[ $exit_code -eq 0 ]] || { echo "expected exit 0 (default excludes node_modules)" >&2; return 1; }
}

test_subcheck1_payload_in_node_modules_with_flag() {
  local tmpdir; tmpdir=$(setup_test_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  mkdir -p node_modules/poisoned-pkg
  : > node_modules/poisoned-pkg/bun_environment.js

  local output exit_code=0
  output=$("$SCANNER" --working-tree-only --include-node-modules 2>&1) || exit_code=$?

  [[ $exit_code -eq 2 ]] || { echo "expected exit 2 with --include-node-modules, got $exit_code" >&2; return 1; }
  echo "$output" | grep -q "\[FAIL\] Shai-Hulud payload file: ./node_modules/poisoned-pkg/bun_environment.js" \
    || { echo "expected FAIL line mentioning node_modules path" >&2; return 1; }
}

test_subcheck2_ioc_in_lockfile_fails() {
  local tmpdir; tmpdir=$(setup_test_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  cp "$FIXTURES/lockfile-with-ioc.json" client/desktop/package-lock.json

  local output exit_code=0
  output=$("$SCANNER" --lockfile-only 2>&1) || exit_code=$?

  [[ $exit_code -eq 3 ]] || { echo "expected exit 3, got $exit_code: $output" >&2; return 1; }
  echo "$output" | grep -q "\[FAIL\] IOC match: fixture-malicious-package" \
    || { echo "expected FAIL line for IOC match" >&2; return 1; }
}

test_subcheck2_clean_lockfiles_pass() {
  local tmpdir; tmpdir=$(setup_test_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  local output exit_code=0
  output=$("$SCANNER" --lockfile-only 2>&1) || exit_code=$?

  [[ $exit_code -eq 0 ]] || { echo "expected exit 0, got $exit_code: $output" >&2; return 1; }
}

test_subcheck2_ioc_in_license_tool_lockfile_fails() {
  local tmpdir; tmpdir=$(setup_test_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"
  cp "$FIXTURES/lockfile-with-ioc.json" .github/tools/license-checker/package-lock.json

  local output exit_code=0
  output=$("$SCANNER" --lockfile-only 2>&1) || exit_code=$?
  [[ $exit_code -eq 3 ]] || { echo "expected exit 3 for license tool IOC, got $exit_code: $output" >&2; return 1; }
  grep -q '\[FAIL\] IOC match: fixture-malicious-package' <<<"$output" \
    || { echo "expected IOC match in license tool lockfile, got: $output" >&2; return 1; }
}

test_subcheck2_nested_node_modules_ioc_detected() {
  # Regression test for #722 Copilot finding: IOC packages at nested
  # node_modules/<a>/node_modules/<b> keys were being mangled to
  # "a/node_modules/b" rather than "b", causing subcheck 2 to miss
  # transitive-dep poisonings. Verify the LAST `node_modules/` segment is
  # used as the package name.
  local tmpdir; tmpdir=$(setup_test_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  cp "$FIXTURES/lockfile-with-nested-ioc.json" client/desktop/package-lock.json

  local output exit_code=0
  output=$("$SCANNER" --lockfile-only 2>&1) || exit_code=$?

  [[ $exit_code -eq 3 ]] || { echo "expected exit 3 (nested IOC), got $exit_code: $output" >&2; return 1; }
  echo "$output" | grep -q "\[FAIL\] IOC match: fixture-malicious-package" \
    || { echo "expected FAIL line for nested fixture-malicious-package" >&2; return 1; }
}

test_subcheck2_malformed_lockfile_internal_error() {
  # Copilot finding: a lockfile that isn't valid JSON should produce a
  # structured [ERROR] message and exit 10, not a raw Python traceback.
  local tmpdir; tmpdir=$(setup_test_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  echo "{ this is not valid json <<< MERGE CONFLICT MARKER" \
    > client/desktop/package-lock.json

  local output exit_code=0
  output=$("$SCANNER" --lockfile-only 2>&1) || exit_code=$?

  [[ $exit_code -eq 10 ]] || { echo "expected exit 10 on malformed JSON, got $exit_code: $output" >&2; return 1; }
  echo "$output" | grep -q "\[ERROR\] unable to parse lockfile" \
    || { echo "expected structured ERROR line, got: $output" >&2; return 1; }
}

test_subcheck2_fp_allowlist_suppresses_match() {
  local tmpdir; tmpdir=$(setup_test_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  cp "$FIXTURES/lockfile-with-ioc.json" client/desktop/package-lock.json
  echo "fixture-malicious-package  # FP confirmed for test" \
    > scripts/security/iocs/false-positive-allowlist.txt

  local output exit_code=0
  output=$("$SCANNER" --lockfile-only 2>&1) || exit_code=$?

  [[ $exit_code -eq 0 ]] || { echo "expected exit 0 after FP allowlist, got $exit_code" >&2; return 1; }
  echo "$output" | grep -q "\[INFO\] FP-allowlisted: fixture-malicious-package" \
    || { echo "expected INFO FP-allowlisted line" >&2; return 1; }
}

test_subcheck2_fp_allowlist_without_rationale() {
  local tmpdir; tmpdir=$(setup_test_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  cp "$FIXTURES/lockfile-with-ioc.json" client/desktop/package-lock.json
  echo "fixture-malicious-package" \
    > scripts/security/iocs/false-positive-allowlist.txt

  local output exit_code=0
  output=$("$SCANNER" --lockfile-only 2>&1) || exit_code=$?

  [[ $exit_code -eq 0 ]] || { echo "expected exit 0 after FP allowlist, got $exit_code" >&2; return 1; }
  echo "$output" | grep -Fxq "[INFO] FP-allowlisted: fixture-malicious-package" \
    || { echo "expected INFO line without a rationale, got: $output" >&2; return 1; }
}

test_subcheck2_missing_ioc_file_internal_error() {
  local tmpdir; tmpdir=$(setup_test_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  rm scripts/security/iocs/shai-hulud-2.0.txt

  local output exit_code=0
  output=$("$SCANNER" --lockfile-only 2>&1) || exit_code=$?

  [[ $exit_code -eq 10 ]] || { echo "expected exit 10 on missing IOC file, got $exit_code" >&2; return 1; }
}

test_subcheck2_trusted_policy_overrides_pr_files() {
  local tmpdir; tmpdir=$(setup_test_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  cp "$FIXTURES/lockfile-with-ioc.json" client/desktop/package-lock.json
  printf '# PR deleted the IOC list\n' > scripts/security/iocs/shai-hulud-2.0.txt
  printf 'fixture-malicious-package\n' > scripts/security/iocs/false-positive-allowlist.txt
  printf 'fixture-malicious-package\n' > "$tmpdir/trusted-list.txt"
  : > "$tmpdir/trusted-allowlist.txt"
  # PR-root modules must not shadow stdlib imports in the trusted scan.
  printf 'raise SystemExit(0)\n' > json.py

  local output exit_code=0
  output=$(PYTHONSAFEPATH=1 PYTHONPATH='' \
    IOC_LIST_PATH="$tmpdir/trusted-list.txt" \
    IOC_ALLOWLIST_PATH="$tmpdir/trusted-allowlist.txt" \
    "$SCANNER" --lockfile-only 2>&1) || exit_code=$?

  [[ $exit_code -eq 3 ]] || { echo "expected trusted IOC match (3), got $exit_code: $output" >&2; return 1; }
  echo "$output" | grep -q '\[FAIL\] IOC match: fixture-malicious-package' \
    || { echo "expected trusted IOC match, got: $output" >&2; return 1; }
}

test_subcheck2_v1_lockfile_fails_closed() {
  local tmpdir; tmpdir=$(setup_test_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  printf '%s\n' '{"lockfileVersion":1,"dependencies":{"fixture-malicious-package":{"version":"1.0.0"}}}' \
    > client/desktop/package-lock.json
  local output exit_code=0
  output=$("$SCANNER" --lockfile-only 2>&1) || exit_code=$?
  [[ $exit_code -eq 10 ]] || { echo "expected unsupported v1 to fail closed (10), got $exit_code: $output" >&2; return 1; }
  echo "$output" | grep -q 'unsupported lockfile schema' \
    || { echo "expected schema error, got: $output" >&2; return 1; }
}

test_subcheck2_invalid_packages_fails_closed() {
  local tmpdir; tmpdir=$(setup_test_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  printf '%s\n' '{"lockfileVersion":3,"packages":[]}' \
    > client/desktop/package-lock.json
  local output exit_code=0
  output=$("$SCANNER" --lockfile-only 2>&1) || exit_code=$?
  [[ $exit_code -eq 10 ]] || { echo "expected invalid packages to fail closed (10), got $exit_code: $output" >&2; return 1; }
}

test_subcheck2_v2_lockfile_still_supported() {
  local tmpdir; tmpdir=$(setup_test_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  cp "$FIXTURES/lockfile-with-ioc.json" client/desktop/package-lock.json
  sed -i.bak 's/"lockfileVersion": 3/"lockfileVersion": 2/' client/desktop/package-lock.json
  rm -f client/desktop/package-lock.json.bak
  local output exit_code=0
  output=$("$SCANNER" --lockfile-only 2>&1) || exit_code=$?
  [[ $exit_code -eq 3 ]] || { echo "expected v2 IOC finding (3), got $exit_code: $output" >&2; return 1; }
}

test_subcheck2_npm_alias_real_name_matches_ioc() {
  local tmpdir; tmpdir=$(setup_test_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  cat > client/desktop/package-lock.json <<'JSON'
{"lockfileVersion":3,"packages":{"":{"name":"fixture-app","version":"1.0.0"},
"node_modules/safe-alias":{"name":"fixture-malicious-package","version":"1.0.0-poisoned"}}}
JSON
  local output exit_code=0
  output=$("$SCANNER" --lockfile-only 2>&1) || exit_code=$?
  [[ $exit_code -eq 3 ]] || { echo "expected alias real-name IOC finding (3), got $exit_code: $output" >&2; return 1; }
  echo "$output" | grep -q '\[FAIL\] IOC match: fixture-malicious-package@1.0.0-poisoned' \
    || { echo "expected actual package name and version, got: $output" >&2; return 1; }
}

test_subcheck3_new_preinstall_warns() {
  local tmpdir; tmpdir=$(setup_test_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  cp "$FIXTURES/lockfile-with-install.json" client/desktop/package-lock.json
  git add client/desktop/package-lock.json
  git -c commit.gpgsign=false commit -q -m "add sketchy-pkg"

  local output exit_code=0
  output=$("$SCANNER" --lockfile-only 2>&1) || exit_code=$?

  [[ $exit_code -eq 0 ]] || { echo "expected exit 0 (warn-only), got $exit_code: $output" >&2; return 1; }
  echo "$output" | grep -q "\[WARN\] New install script: sketchy-pkg" \
    || { echo "expected WARN line for new install script: $output" >&2; return 1; }
}

test_subcheck3_multiline_preinstall_warns() {
  # Regression test for #722: a preinstall script with an
  # embedded newline (valid in JSON) used to split the TSV output across
  # rows, letting the downstream `read` parser see only the first line
  # and the WARN never fire. Now extract_install_scripts uses json.dumps
  # on the command value to force a single-line literal.
  local tmpdir; tmpdir=$(setup_test_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  cp "$FIXTURES/lockfile-with-multiline-install.json" client/desktop/package-lock.json
  git add client/desktop/package-lock.json
  git -c commit.gpgsign=false commit -q -m "add multiline-pkg"

  local output exit_code=0
  output=$("$SCANNER" --lockfile-only 2>&1) || exit_code=$?

  [[ $exit_code -eq 0 ]] || { echo "expected exit 0 (warn-only), got $exit_code: $output" >&2; return 1; }
  echo "$output" | grep -q "\[WARN\] New install script: multiline-pkg" \
    || { echo "expected WARN line for multi-line install script: $output" >&2; return 1; }
  # The embedded newline must survive as a JSON-escaped \n (two chars:
  # backslash + n), not as a real newline that splits the WARN across rows.
  echo "$output" | grep -q 'multiline-pkg.*\\n' \
    || { echo "expected JSON-escaped \\n literal in WARN line: $output" >&2; return 1; }
  # Confirm the WARN is on a single line (no real newline inside the value).
  local warn_lines
  warn_lines=$(echo "$output" | grep -c "multiline-pkg" || true)
  [[ "$warn_lines" -eq 1 ]] \
    || { echo "expected exactly 1 WARN line for multiline-pkg, got $warn_lines: $output" >&2; return 1; }
}

test_subcheck3_existing_preinstall_not_flagged() {
  local tmpdir; tmpdir=$(setup_test_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  # Amend initial commit so the install-script lockfile is in HEAD~1,
  # then add an unrelated second commit so HEAD~1 is resolvable and
  # contains the install script. This exercises the diff-suppression
  # path (pre-existing script in base → no WARN), not the skip path.
  cp "$FIXTURES/lockfile-with-install.json" client/desktop/package-lock.json
  git add client/desktop/package-lock.json
  git -c commit.gpgsign=false commit --amend -q --no-edit

  echo "marker" > unrelated.txt
  git add unrelated.txt
  git -c commit.gpgsign=false commit -q -m "unrelated change"

  local output exit_code=0
  output=$("$SCANNER" --lockfile-only 2>&1) || exit_code=$?

  [[ $exit_code -eq 0 ]] || { echo "expected exit 0, got $exit_code" >&2; return 1; }
  ! echo "$output" | grep -q "\[WARN\] New install script:" \
    || { echo "unexpected WARN — script was already in base" >&2; return 1; }
  # Confirm we're actually testing diff-suppression (not the skip path).
  ! echo "$output" | grep -q "\[INFO\] Install-script diff skipped" \
    || { echo "hit skip path instead of diff-suppression" >&2; return 1; }
}

test_subcheck3_no_base_resolvable_skips_gracefully() {
  local tmpdir; tmpdir=$(setup_test_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  # Reset to a single commit with no parent (HEAD~1 unresolvable).
  rm -rf .git
  git init -q -b main
  git config user.email "t@t"; git config user.name "t"
  git add -A
  git -c commit.gpgsign=false commit -q -m "first"

  local output exit_code=0
  output=$("$SCANNER" --lockfile-only 2>&1) || exit_code=$?

  [[ $exit_code -eq 0 ]] || { echo "expected exit 0, got $exit_code" >&2; return 1; }
  echo "$output" | grep -q "\[INFO\] Install-script diff skipped" \
    || { echo "expected skip INFO line" >&2; return 1; }
}

setup_workflow_test_repo() {
  local fixture="$1"
  local tmpdir
  tmpdir=$(mktemp -d)
  mkdir -p "$tmpdir/.github/workflows" "$tmpdir/scripts/security"
  cp "$FIXTURES/workflows/$fixture" "$tmpdir/.github/workflows/fixture.yml"
  : > "$tmpdir/scripts/security/self-hosted-runner-allowlist.txt"
  echo "$tmpdir"
}

test_runner_linter_clean_passes() {
  local tmpdir; tmpdir=$(setup_workflow_test_repo "clean.yml")
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  local output exit_code=0
  output=$("$RUNNER_LINTER" 2>&1) || exit_code=$?
  [[ $exit_code -eq 0 ]] || { echo "expected exit 0, got $exit_code: $output" >&2; return 1; }
}

test_runner_linter_self_hosted_empty_allowlist_fails() {
  local tmpdir; tmpdir=$(setup_workflow_test_repo "self-hosted-scalar.yml")
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  local output exit_code=0
  output=$("$RUNNER_LINTER" 2>&1) || exit_code=$?
  [[ $exit_code -eq 1 ]] || { echo "expected exit 1, got $exit_code: $output" >&2; return 1; }
  echo "$output" | grep -q "\[FAIL\]" || { echo "expected FAIL line" >&2; return 1; }
}

test_runner_linter_array_all_allowlisted_passes() {
  local tmpdir; tmpdir=$(setup_workflow_test_repo "self-hosted-array.yml")
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  printf 'self-hosted\nlinux\nfips-validated\n' \
    > scripts/security/self-hosted-runner-allowlist.txt

  local output exit_code=0
  output=$("$RUNNER_LINTER" 2>&1) || exit_code=$?
  [[ $exit_code -eq 0 ]] || { echo "expected exit 0 with full allowlist, got $exit_code: $output" >&2; return 1; }
}

test_runner_linter_trusted_allowlist_overrides_pr_file() {
  local tmpdir; tmpdir=$(setup_workflow_test_repo "self-hosted-array.yml")
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  printf 'self-hosted\nlinux\nfips-validated\n' \
    > scripts/security/self-hosted-runner-allowlist.txt
  printf 'self-hosted\n' > "$tmpdir/trusted-runner-allowlist.txt"
  printf 'raise SystemExit(0)\n' > yaml.py
  printf 'raise SystemExit(0)\n' > json.py

  local output exit_code=0
  output=$(PYTHONSAFEPATH=1 PYTHONPATH='' \
    RUNNER_ALLOWLIST_PATH="$tmpdir/trusted-runner-allowlist.txt" \
    "$RUNNER_LINTER" 2>&1) || exit_code=$?
  [[ $exit_code -eq 1 ]] || { echo "expected trusted runner-policy failure (1), got $exit_code: $output" >&2; return 1; }
  echo "$output" | grep -q '\[FAIL\]' || { echo "expected FAIL line, got: $output" >&2; return 1; }
}

test_runner_linter_array_partial_allowlist_fails() {
  local tmpdir; tmpdir=$(setup_workflow_test_repo "self-hosted-array.yml")
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  printf 'self-hosted\nlinux\n' > scripts/security/self-hosted-runner-allowlist.txt
  # Missing "fips-validated".

  local output exit_code=0
  output=$("$RUNNER_LINTER" 2>&1) || exit_code=$?
  [[ $exit_code -eq 1 ]] || { echo "expected exit 1 on missing label, got $exit_code" >&2; return 1; }
}

test_runner_linter_expression_unparseable_warns() {
  local tmpdir; tmpdir=$(setup_workflow_test_repo "expression-runner.yml")
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  local output exit_code=0
  output=$("$RUNNER_LINTER" 2>&1) || exit_code=$?
  [[ $exit_code -eq 2 ]] || { echo "expected exit 2 on expression runner, got $exit_code" >&2; return 1; }
}

test_regenerate_produces_sorted_deduped_list() {
  local tmpdir
  tmpdir=$(mktemp -d)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"

  mkdir -p scripts/security/iocs
  # Keep CSV + SHA256 inside the test tmpdir so the RETURN trap cleans them up,
  # avoiding leaked /tmp artifacts and cross-test collisions on concurrent runs.
  local csv="$tmpdir/ioc-test.csv"
  local sha="$tmpdir/ioc-test.sha256"
  cp "$FIXTURES/fake-datadog-csv.csv" "$csv"
  if command -v sha256sum >/dev/null; then
    sha256sum "$csv" > "$sha"
  else
    shasum -a 256 "$csv" > "$sha"
  fi

  "$REPO_ROOT/scripts/security/regenerate-ioc-list.sh" "$csv" "$sha"

  # Output file must exist and contain sorted unique package names.
  [[ -f scripts/security/iocs/shai-hulud-2.0.txt ]] \
    || { echo "output file not created" >&2; return 1; }

  local names
  names=$(awk '/^[^#]/ && NF' scripts/security/iocs/shai-hulud-2.0.txt)
  local expected='@ctrl/tinycolor
angulartics2
chalk-template
fixture-malicious-package'
  [[ "$names" == "$expected" ]] \
    || { echo "expected sorted names, got: $names" >&2; return 1; }

  # Header must contain SHA256 and must not contain timestamp-only churn.
  grep -q "^# SHA256 of source CSV:" scripts/security/iocs/shai-hulud-2.0.txt \
    || { echo "missing SHA256 header" >&2; return 1; }
  ! grep -q "^# Vendored at:" scripts/security/iocs/shai-hulud-2.0.txt \
    || { echo "unexpected Vendored at header" >&2; return 1; }
}


setup_ioc_refresh_guard_repo() {
  local tmpdir
  tmpdir=$(mktemp -d)
  mkdir -p "$tmpdir/scripts/security/iocs" \
           "$tmpdir/client/desktop" \
           "$tmpdir/services/media-plane" \
           "$tmpdir/.github/tools/license-checker"
  cat > "$tmpdir/scripts/security/iocs/shai-hulud-2.0.txt" <<'LIST'
# Shai-Hulud 2.0 npm supply-chain IOC list
# Source:  https://github.com/DataDog/indicators-of-compromise/blob/1111111111111111111111111111111111111111/shai-hulud-2.0/shai-hulud-2.0.csv
# Source commit:  1111111111111111111111111111111111111111
# SHA256 of source CSV:  aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
# Refresh cadence: daily via .github/workflows/refresh-ioc-list.yml (automated PR)
# FP workflow: add the package to scripts/security/iocs/false-positive-allowlist.txt (CODEOWNERS-gated)

alpha-package
bravo-package
LIST
  : > "$tmpdir/scripts/security/iocs/false-positive-allowlist.txt"
  cat > "$tmpdir/client/desktop/package-lock.json" <<'JSON'
{"packages":{"":{"name":"desktop"},"node_modules/safe-package":{"version":"1.0.0"}}}
JSON
  cp "$tmpdir/client/desktop/package-lock.json" "$tmpdir/services/media-plane/package-lock.json"
  cp "$tmpdir/client/desktop/package-lock.json" "$tmpdir/.github/tools/license-checker/package-lock.json"
  ( cd "$tmpdir" \
    && git init -q -b main \
    && git config user.email "test@example.com" \
    && git config user.name "Test Harness" \
    && git add -A \
    && git -c commit.gpgsign=false commit -q -m "base ioc list" )
  echo "$tmpdir"
}

write_ioc_refresh_head() {
  local file="$1"
  shift
  cat > "$file" <<'HEADER'
# Shai-Hulud 2.0 npm supply-chain IOC list
# Source:  https://github.com/DataDog/indicators-of-compromise/blob/2222222222222222222222222222222222222222/shai-hulud-2.0/shai-hulud-2.0.csv
# Source commit:  2222222222222222222222222222222222222222
# SHA256 of source CSV:  bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
# Refresh cadence: daily via .github/workflows/refresh-ioc-list.yml (automated PR)
# FP workflow: add the package to scripts/security/iocs/false-positive-allowlist.txt (CODEOWNERS-gated)

HEADER
  printf '%s\n' "$@" >> "$file"
}

test_regenerate_ioc_list_is_deterministic_without_vendored_at() {
  local tmpdir
  tmpdir=$(mktemp -d)
  trap "rm -rf $tmpdir" RETURN
  local csv="$tmpdir/ioc.csv"
  local sha="$tmpdir/ioc.sha256"
  local out1="$tmpdir/out1.txt"
  local out2="$tmpdir/out2.txt"
  cp "$FIXTURES/fake-datadog-csv.csv" "$csv"
  if command -v sha256sum >/dev/null; then
    sha256sum "$csv" > "$sha"
  else
    shasum -a 256 "$csv" > "$sha"
  fi

  OUT="$out1" "$REPO_ROOT/scripts/security/regenerate-ioc-list.sh" "$csv" "$sha" 2222222222222222222222222222222222222222 >/dev/null
  OUT="$out2" "$REPO_ROOT/scripts/security/regenerate-ioc-list.sh" "$csv" "$sha" 2222222222222222222222222222222222222222 >/dev/null

  cmp -s "$out1" "$out2" || { echo "expected identical generator output" >&2; return 1; }
  ! grep -q '^# Vendored at:' "$out1" || { echo "date header must not be diff-relevant" >&2; return 1; }
  grep -q '^# Source commit:  2222222222222222222222222222222222222222$' "$out1" \
    || { echo "missing pinned source commit header" >&2; return 1; }
}

test_ioc_refresh_guard_clean_small_delta_passes() {
  local tmpdir; tmpdir=$(setup_ioc_refresh_guard_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"
  write_ioc_refresh_head scripts/security/iocs/shai-hulud-2.0.txt \
    alpha-package bravo-package charlie-package

  local output exit_code=0
  output=$("$REFRESH_GUARD" --base-ref HEAD --skip-provenance 2>&1) || exit_code=$?
  [[ $exit_code -eq 0 ]] || { echo "expected clean delta to pass, got $exit_code: $output" >&2; return 1; }
  echo "$output" | grep -q 'Guard result: pass' \
    || { echo "expected pass report, got: $output" >&2; return 1; }
}

test_ioc_refresh_guard_noop_body_fails() {
  local tmpdir; tmpdir=$(setup_ioc_refresh_guard_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"
  write_ioc_refresh_head scripts/security/iocs/shai-hulud-2.0.txt \
    alpha-package bravo-package

  local output exit_code=0
  output=$("$REFRESH_GUARD" --base-ref HEAD --skip-provenance 2>&1) || exit_code=$?
  [[ $exit_code -eq 1 ]] || { echo "expected no-op delta to fail with exit 1, got $exit_code: $output" >&2; return 1; }
  echo "$output" | grep -q 'package set is unchanged' \
    || { echo "expected no-op reason, got: $output" >&2; return 1; }
}

test_codeowners_protects_ioc_refresh_guard_script() {
  grep -Eq '^scripts/security/check-ioc-refresh-pr\.sh[[:space:]]+@Concord-Voice/security$' "$REPO_ROOT/CODEOWNERS" \
    || { echo "check-ioc-refresh-pr.sh must be CODEOWNERS-gated to @Concord-Voice/security" >&2; return 1; }
}

test_ioc_refresh_workflow_skips_noop_before_pr_creation() {
  local workflow="$REPO_ROOT/.github/workflows/refresh-ioc-list.yml"
  grep -q -- '--noop-exit-code 2' "$workflow" \
    || { echo "scheduled refresh must request distinct no-op exit code" >&2; return 1; }
  grep -q "steps.guard.outputs.exit_code != '2'" "$workflow" \
    || { echo "create-pull-request/escalation must skip no-op guard result" >&2; return 1; }
}

test_ioc_refresh_workflow_uses_valid_comment_queries() {
  local workflow="$REPO_ROOT/.github/workflows/refresh-ioc-list.yml"
  ! grep -q -- '--jq --arg' "$workflow" \
    || { echo "gh api --jq does not accept jq's --arg option" >&2; return 1; }

  local paginated_count
  paginated_count=$(grep -Fc 'gh api --paginate --slurp "repos/${GITHUB_REPOSITORY}/issues/${PR_NUMBER}/comments"' "$workflow" || true)
  [[ "$paginated_count" -eq 2 ]] \
    || { echo "expected both IOC guard comment lookups to use gh api --paginate --slurp" >&2; return 1; }

  local marker_filter_count
  marker_filter_count=$(grep -Fc 'contains("<!-- ioc-refresh-guard -->")' "$workflow" || true)
  [[ "$marker_filter_count" -eq 2 ]] \
    || { echo "expected both IOC guard comment lookups to use a literal marker jq filter" >&2; return 1; }
}

test_ioc_refresh_workflow_permissions_preserve_only_required_writes() {
  python3 - "$REPO_ROOT/.github/workflows/refresh-ioc-list.yml" <<'PY'
import sys
import yaml

workflow = yaml.load(open(sys.argv[1]), Loader=yaml.BaseLoader)
jobs = workflow['jobs']
failures = []
refresh = jobs['refresh']['permissions']
if refresh != {'contents': 'read'}:
    failures.append(f'refresh native token must be read-only, got {refresh}')
token = next(step for step in jobs['refresh']['steps'] if step.get('id') == 'app-token')
if token['with'].get('permission-contents') != 'write':
    failures.append('refresh App token must retain content writes')
if token['with'].get('permission-pull-requests') != 'write':
    failures.append('refresh App token must retain PR writes')
guard = jobs['guard']['permissions']
if guard != {'contents': 'read', 'pull-requests': 'write'}:
    failures.append(f'PR guard retains comment scope, got {guard}')
assert not failures, '; '.join(failures)
PY
}

test_ioc_refresh_guard_noop_can_use_custom_exit_code() {
  local tmpdir; tmpdir=$(setup_ioc_refresh_guard_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"
  write_ioc_refresh_head scripts/security/iocs/shai-hulud-2.0.txt \
    alpha-package bravo-package

  local output exit_code=0
  output=$("$REFRESH_GUARD" --base-ref HEAD --skip-provenance --noop-exit-code 2 2>&1) || exit_code=$?
  [[ $exit_code -eq 2 ]] || { echo "expected no-op delta to exit 2, got $exit_code: $output" >&2; return 1; }
  echo "$output" | grep -q 'package set is unchanged' \
    || { echo "expected no-op reason, got: $output" >&2; return 1; }
}

test_ioc_refresh_guard_removal_requires_review() {
  local tmpdir; tmpdir=$(setup_ioc_refresh_guard_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"
  write_ioc_refresh_head scripts/security/iocs/shai-hulud-2.0.txt \
    alpha-package

  local output exit_code=0
  output=$("$REFRESH_GUARD" --base-ref HEAD --skip-provenance 2>&1) || exit_code=$?
  [[ $exit_code -eq 1 ]] || { echo "expected removal to require review with exit 1, got $exit_code: $output" >&2; return 1; }
  echo "$output" | grep -q 'removals require security review' \
    || { echo "expected removal review reason, got: $output" >&2; return 1; }
}

test_ioc_refresh_guard_lockfile_collision_fails() {
  local tmpdir; tmpdir=$(setup_ioc_refresh_guard_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"
  write_ioc_refresh_head scripts/security/iocs/shai-hulud-2.0.txt \
    alpha-package bravo-package safe-package

  local output exit_code=0
  output=$("$REFRESH_GUARD" --base-ref HEAD --skip-provenance 2>&1) || exit_code=$?
  [[ $exit_code -eq 1 ]] || { echo "expected lockfile collision to fail with exit 1, got $exit_code: $output" >&2; return 1; }
  echo "$output" | grep -q 'safe-package.*package-lock.json' \
    || { echo "expected collision reason, got: $output" >&2; return 1; }
}

test_ioc_refresh_guard_license_tool_lockfile_collision_fails() {
  local tmpdir; tmpdir=$(setup_ioc_refresh_guard_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"
  cat > .github/tools/license-checker/package-lock.json <<'JSON'
{"packages":{"":{"name":"license-checker-tool"},"node_modules/fixture-malicious-package":{"version":"1.0.0"}}}
JSON
  write_ioc_refresh_head scripts/security/iocs/shai-hulud-2.0.txt \
    alpha-package bravo-package fixture-malicious-package

  local output exit_code=0
  output=$("$REFRESH_GUARD" --base-ref HEAD --skip-provenance 2>&1) || exit_code=$?
  [[ $exit_code -eq 1 ]] || { echo "expected tool lock collision exit 1, got $exit_code: $output" >&2; return 1; }
  grep -q 'fixture-malicious-package.*license-checker/package-lock.json' <<<"$output" \
    || { echo "expected license tool collision reason, got: $output" >&2; return 1; }
}

test_ioc_refresh_guard_oversized_delta_fails() {
  local tmpdir; tmpdir=$(setup_ioc_refresh_guard_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"
  local names=(alpha-package bravo-package)
  local i
  for i in $(seq 1 101); do
    names+=("pkg-$i")
  done
  write_ioc_refresh_head scripts/security/iocs/shai-hulud-2.0.txt "${names[@]}"

  local output exit_code=0
  output=$("$REFRESH_GUARD" --base-ref HEAD --skip-provenance --max-delta 100 2>&1) || exit_code=$?
  [[ $exit_code -eq 1 ]] || { echo "expected oversized delta to fail with exit 1, got $exit_code: $output" >&2; return 1; }
  echo "$output" | grep -q 'additions 101 exceeds max 100' \
    || { echo "expected oversized reason, got: $output" >&2; return 1; }
}

test_ioc_refresh_guard_unsorted_body_fails() {
  local tmpdir; tmpdir=$(setup_ioc_refresh_guard_repo)
  trap "rm -rf $tmpdir" RETURN
  cd "$tmpdir"
  write_ioc_refresh_head scripts/security/iocs/shai-hulud-2.0.txt \
    bravo-package alpha-package charlie-package

  local output exit_code=0
  output=$("$REFRESH_GUARD" --base-ref HEAD --skip-provenance 2>&1) || exit_code=$?
  [[ $exit_code -eq 1 ]] || { echo "expected unsorted body to fail with exit 1, got $exit_code: $output" >&2; return 1; }
  echo "$output" | grep -q 'body is not sorted' \
    || { echo "expected sortedness reason, got: $output" >&2; return 1; }
}

# ── Entry point ────────────────────────────────────────────────────────────
echo "Running supply-chain scanner tests..."
echo ""

# Auto-discover every function named test_*
# shellcheck disable=SC2046  # word splitting is intentional
for fn in $(declare -F | awk '/^declare -f test_/ {print $3}'); do
  run_test "$fn"
done

echo ""
if [[ $FAIL -eq 0 ]]; then
  echo "All $PASS tests passed."
  exit 0
else
  echo "$FAIL tests failed ($PASS passed):"
  printf '  - %s\n' "${FAILED_TESTS[@]}"
  exit 1
fi
