#!/usr/bin/env bash
# Contract tests for the shared CI scan applicability policy.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
HELPER="$REPO_ROOT/.github/scripts/ci-scan-relevance.mjs"
POLICY="$REPO_ROOT/.github/ci/scan-relevance.yml"
BUNDLE_SHA=ceb8a2b8f2d89434be7ff52d3de7ec3738c5cc9d
CACHE_ROOT="${CI_SCAN_RELEVANCE_CACHE:-${TMPDIR:-/tmp}/ci-scan-relevance-cache}"
BUNDLE="$CACHE_ROOT/dorny-paths-filter-$BUNDLE_SHA.js"
TASK_TMP="$(mktemp -d "${TMPDIR:-/tmp}/ci-scan-relevance.XXXXXX")"
trap 'rm -rf "$TASK_TMP"' EXIT

mkdir -p "$CACHE_ROOT"
if [[ ! -s "$BUNDLE" ]]; then
  curl --fail --location --silent --show-error \
    "https://raw.githubusercontent.com/dorny/paths-filter/$BUNDLE_SHA/dist/index.js" \
    --output "$BUNDLE.tmp"
  mv "$BUNDLE.tmp" "$BUNDLE"
fi
test -s "$BUNDLE"

node --input-type=module - "$HELPER" <<'NODE'
import assert from 'node:assert/strict';
import { pathToFileURL } from 'node:url';
const { resolveScanMode } = await import(pathToFileURL(process.argv[2]));
const now = 1_800_000_000_000;
const lease = 'required-relevant:123:2:1800000300';
assert.equal(resolveScanMode(lease, '123', '2', now), 'required-relevant');
for (const [value, id, attempt, time] of [
  ['required-relevant', '123', '2', now],
  [lease, '124', '2', now],
  [lease, '123', '3', now],
  [lease, '123', '2', now + 300_000],
  ['required-relevant:123:2:1800003601', '123', '2', now],
  ['required-relevant:0123:2:1800000300', '123', '2', now],
]) assert.equal(resolveScanMode(value, id, attempt, time), 'required-full');
assert.equal(resolveScanMode('required-full', '123', '2', now), 'required-full');
NODE

# Git's hook-local environment must never redirect fixture repo operations here.
GIT_LOCAL_NAMES="$(git -C "$REPO_ROOT" rev-parse --local-env-vars)"
for name in $GIT_LOCAL_NAMES; do unset "$name"; done
FIXTURE="$TASK_TMP/repo"
mkdir -p "$FIXTURE"
git -C "$FIXTURE" init --quiet
git -C "$FIXTURE" config user.name fixture
git -C "$FIXTURE" config user.email fixture@example.invalid
mkdir -p "$FIXTURE/docs" "$FIXTURE/services/control-plane/internal/example"
printf 'old doc\n' > "$FIXTURE/docs/relevance.md"
printf 'old source\n' > "$FIXTURE/services/control-plane/internal/example/deleted.go"
printf 'old source\n' > "$FIXTURE/services/control-plane/internal/example/renamed-out.go"
mkdir -p "$FIXTURE/.github/workflows" "$FIXTURE/.github/actions/obsolete"
printf 'name: retired\n' > "$FIXTURE/.github/workflows/removed-consumer.yaml"
printf 'name: retired\n' > "$FIXTURE/.github/actions/obsolete/action.yaml"
git -C "$FIXTURE" commit --allow-empty --quiet -m base
git -C "$FIXTURE" add .
git -C "$FIXTURE" commit --quiet -m baseline-files
BASE_SHA="$(git -C "$FIXTURE" rev-parse HEAD)"

# Exercise the actual pinned action bundle with GitHub's push event and GITHUB_OUTPUT.
# A trivial temporary map proves fixture/action plumbing independently of production.
TRIVIAL_MAP="$TASK_TMP/trivial-filter.yml"
printf 'docs:\n  - "docs/**"\n' > "$TRIVIAL_MAP"
mkdir -p "$FIXTURE/docs"
printf 'changed\n' > "$FIXTURE/docs/readme.md"
git -C "$FIXTURE" add .
git -C "$FIXTURE" commit --quiet -m docs
TRIVIAL_HEAD="$(git -C "$FIXTURE" rev-parse HEAD)"
EVENT="$TASK_TMP/push-event.json"
OUTPUT="$TASK_TMP/action-output"
printf '{"before":"%s","after":"%s","repository":{"default_branch":"main"}}\n' "$BASE_SHA" "$TRIVIAL_HEAD" > "$EVENT"
: > "$OUTPUT"
(cd "$FIXTURE" && GITHUB_EVENT_NAME=push GITHUB_EVENT_PATH="$EVENT" \
  GITHUB_WORKSPACE="$FIXTURE" GITHUB_REF=refs/heads/main GITHUB_OUTPUT="$OUTPUT" \
  INPUT_FILTERS="$TRIVIAL_MAP" INPUT_BASE="$BASE_SHA" INPUT_REF="$TRIVIAL_HEAD" \
  env 'INPUT_PREDICATE-QUANTIFIER=some-with-excludes' node "$BUNDLE") # pragma: allowlist secret; public matcher mode input, not a credential
grep -q '^true$' "$OUTPUT" || {
  echo "FAIL: pinned dorny bundle did not match fixture docs path" >&2
  cat "$OUTPUT" >&2
  exit 1
}

# Each case is a fresh commit from the same baseline; the action performs the diff.
reset_fixture() {
  git -C "$FIXTURE" reset --hard --quiet "$BASE_SHA"
  git -C "$FIXTURE" clean -fdq
}
run_policy_case() {
  local label="$1" expected="$2"; shift 2
  reset_fixture
  local path
  for path in "$@"; do
    mkdir -p "$FIXTURE/$(dirname "$path")"
    printf 'fixture\n' > "$FIXTURE/$path"
  done
  git -C "$FIXTURE" add -A
  git -C "$FIXTURE" commit --quiet -m "$label"
  run_current_diff_case "$label" "$expected"
}
run_current_diff_case() {
  local label="$1" expected="$2" head output
  head="$(git -C "$FIXTURE" rev-parse HEAD)"
  output="$TASK_TMP/$label.dorny-output"
  : > "$output"
  printf '{"before":"%s","after":"%s","repository":{"default_branch":"main"}}\n' "$BASE_SHA" "$head" > "$EVENT"
  (cd "$FIXTURE" && GITHUB_EVENT_NAME=push GITHUB_EVENT_PATH="$EVENT" \
    GITHUB_WORKSPACE="$FIXTURE" GITHUB_REF=refs/heads/main GITHUB_OUTPUT="$output" \
    INPUT_FILTERS="$POLICY" INPUT_BASE="$BASE_SHA" INPUT_REF="$head" \
    env 'INPUT_PREDICATE-QUANTIFIER=some-with-excludes' node "$BUNDLE") >/dev/null # pragma: allowlist secret; public matcher mode input, not a credential
  node --input-type=module - "$REPO_ROOT" "$output" "$TASK_TMP/$label.selection.json" "$BASE_SHA" "$head" "$expected" "$label" <<'NODE'
import assert from 'node:assert/strict';
import { readFile, writeFile } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';
import { resolve } from 'node:path';
const [root, outputPath, selectionPath, base, head, expectedText, label] = process.argv.slice(2);
const { composeSelection, ANALYSIS_KEYS, COMPONENT_KEYS } = await import(pathToFileURL(
  resolve(root, '.github/scripts/ci-scan-relevance.mjs')));
const lines = (await readFile(outputPath, 'utf8')).split(/\r?\n/); const filters = {};
for (let i = 0; i < lines.length; i++) {
  const match = lines[i].match(/^([a-z0-9_]+)<<(.+)$/);
  if (match && (lines[i + 1] === 'true' || lines[i + 1] === 'false'))
    filters[match[1]] = lines[i + 1];
}
const metadata = label === 'docs-schedule'
  ? { event_name: 'schedule', hotfix: false, head_sha: head }
  : { event_name: 'push', hotfix: label === 'docs-hotfix-push', before: base, after: head, history_available: true };
const result = composeSelection(filters, metadata, { mode: 'required-relevant', policySha: 'd'.repeat(40) });
assert.equal(result.known, true, 'matcher outputs compose as known');
await writeFile(selectionPath, `${JSON.stringify(result, null, 2)}\n`);
const expected = new Set(expectedText.split(',').filter(Boolean));
const scanners = ['codeql_actions','codeql_go','codeql_js','eslint','govulncheck','osv','semgrep_custom','semgrep_community','semgrep_canary','sonar'];
if (process.env.CI_SCAN_RELEVANCE_DEBUG) console.error(`${label} filters: ${JSON.stringify(filters)}`);
if (expectedText === 'all-scanners') {
  assert.deepEqual(Object.fromEntries(scanners.map(key => [key, result.selected[key]])),
    Object.fromEntries(scanners.map(key => [key, true])), `${label}: all scanner evidence paths run`);
  assert.equal(result.coverage_required.desktop, true, `${label}: full Sonar coverage runs`);
  assert.equal(result.coverage_required.control_plane, true, `${label}: full Sonar coverage runs`);
  assert.equal(result.coverage_required.media_plane, true, `${label}: full Sonar coverage runs`);
  assert.equal(result.coverage_required.admin, true, `${label}: full Sonar coverage runs`);
} else if (expectedText === 'broker-input') {
  // Shared broker checks below assert component routing and reachable suite registration.
} else if (expectedText === 'guard-input') {
  // These cases pin a particular guard input without overconstraining unrelated scanners.
} else if (expectedText === 'desktop-input') {
  assert.equal(filters.desktop, 'true', `${label}: desktop test input selects the build`);
  assert.equal(result.coverage_required.desktop, true, `${label}: desktop coverage is required`);
} else if (expectedText === 'advisory-only') {
  assert.equal(result.advisory.workflow_lint, true, `${label} reaches workflow-lint runner`);
} else if (expectedText === 'eslint-false') {
  assert.equal(result.selected.eslint, false, `${label} is ignored by the ESLint input matcher`);
} else if (expectedText === 'dev-counts-only') {
  // Assert the advisory family below; other scanners are intentionally orthogonal.
} else {
  assert.deepEqual(Object.fromEntries(scanners.map(key => [key, result.selected[key]])), Object.fromEntries(scanners.map(key => [key, expected.has(key)])), `${label}: exact scanner selection`);
}
if (label.startsWith('runner-docker-')) {
  assert.equal(filters.control_plane, 'true', `${label} reaches control-plane integration jobs`);
  if (label !== 'runner-docker-services') {
    assert.equal(filters.media_plane, 'true', `${label} reaches the warm Docker build`);
    assert.equal(filters.production_docker, 'true', `${label} reaches the cold Docker build`);
    assert.equal(filters.dev_stack, 'true', `${label} reaches dev-stack smoke`);
  }
}
if (label === 'historical-docs') {
  assert.equal(result.advisory.static_guards, false, 'docs-only PR does not select static guards');
}
if (label === 'apple-worker')
  assert.deepEqual(result.workers, ['apple-sso-bridge'], 'one worker change selects only its worker');
if (label === 'admin-lock')
  assert.deepEqual(result.npm_workspaces, ['client/admin'], 'admin lockfile selects only its license workspace');
if (label === 'admin-lock') assert.equal(result.license_go, false, 'npm manifest does not run Go attribution');
if (label === 'license-tool-manifest' || label === 'license-tool-lock') {
  assert.deepEqual(result.npm_workspaces, ['.', 'services/media-plane', 'client/admin'],
    `${label}: license tool inputs select every npm license workspace`);
  assert.equal(result.license_go, false, `${label}: tool dependency changes do not select Go attribution`);
}
if (label === 'notice')
  assert.deepEqual(result.npm_workspaces, ['.', 'services/media-plane', 'client/admin'],
    'NOTICE/policy changes select all npm workspaces in fixed order');
if (label === 'notice') assert.equal(result.license_go, true, 'NOTICE selects Go and npm attribution');
if (label === 'go-import-reachability')
  assert.equal(result.license_go, true, 'Go call-site changes select runtime reachability attribution');
if (label === 'go-only' || label === 'ts-only' || label === 'selfhost-docs') {
  assert.equal(filters.selfhost, 'false', `${label} uses shared SaaS checks without the extra packaging fixtures`);
  assert.equal(filters.selfhost_guard, 'false', `${label} does not select the advisory self-host guard`);
  assert.equal(result.advisory.selfhost, false, `${label} does not schedule extra self-host fixtures`);
}
if (label === 'selfhost-packaging') {
  assert.equal(filters.selfhost, 'true', 'public assembly changes select the unique self-host fixtures');
  assert.equal(filters.selfhost_guard, 'true', 'public assembly changes select the advisory self-host guard');
}
if (label === 'docs-ordinary-push')
  assert.equal(result.selected.osv, false, 'ordinary docs push skips OSV');
if (label === 'docs-hotfix-push' || label === 'docs-schedule')
  assert.equal(result.selected.osv, true, `${label} retains full OSV selection`);
if (label.startsWith('guard-')) {
  const family = label.startsWith('guard-dev-checks-desktop-')
    ? 'dev_checks'
    : label.startsWith('guard-secrets-contract-')
      ? 'secrets_contract'
      : label.slice('guard-'.length).replaceAll('-', '_');
  assert.equal(result.advisory[family], true, `${family} implementation input selects its own guard`);
  assert.equal(result.advisory.static_guards, true, `${family} selects the static-guards runner`);
  if (family === 'secrets_contract')
    assert.equal(filters.secrets_contract, 'true', `${label} matches the actual secrets contract policy`);
}
for (const [prefix, family] of [['feature-input-', 'feature_wiring'],
                                ['roster-input-', 'review_contracts'],
                                ['ioc-input-', 'native_ioc']]) {
  if (label.startsWith(prefix)) {
    assert.equal(result.advisory[family], true, `${label} selects ${family}`);
    assert.equal(result.advisory.static_guards, true, `${label} starts static guards`);
  }
}
if (label.startsWith('workflow-lint-source-'))
  assert.equal(result.advisory.workflow_lint, true, `${label} reaches workflow-lint runner`);
if (label === 'workflow-lint-only') {
  assert.equal(result.advisory.workflow_lint, true, 'workflow-lint-only input selects the lint runner');
  assert.equal(result.advisory.static_guards, false, 'workflow-lint selector alone does not start static guards');
}
if (label === 'go-toolchain-test-only')
  assert.equal(result.advisory.go_toolchain, true, 'guard self-test changes select its family');
if (label.startsWith('toolchain-input-'))
  assert.equal(result.advisory.go_toolchain, true, `${label} runs the Go toolchain guard`);
if (label === 'deployment-input-main-cd')
  assert.equal(result.advisory.deployment, true, 'main-cd change runs deployment wiring guard');
if (label === 'deployment-input-west-workflow') {
  assert.equal(result.advisory.deployment, true, 'West workflow change selects deployment guard');
  assert.equal(result.advisory.static_guards, true, 'West workflow change starts static guards');
}
if (label === 'deployment-input-fleet-helper') {
  assert.equal(result.advisory.deployment, true, 'fleet helper change selects deployment guard');
  assert.equal(result.advisory.static_guards, true, 'fleet helper change starts static guards');
}
if (label === 'deployment-input-provision-secrets') {
  assert.equal(result.advisory.deployment, true, 'provision-secrets change selects deployment guard');
  assert.equal(result.advisory.static_guards, true, 'provision-secrets change starts static guards');
}
if (label === 'catalog-input-dev-lifecycle')
  assert.equal(result.advisory.review_contracts, true, 'skill change runs catalog discipline guard');
if (label === 'spa-input-deploy-script')
  assert.equal(filters.spa, 'true', 'SPA deploy script runs SPA dry run');
NODE
}
run_transition_case() {
  local label="$1" expected="$2" from="$3" to="$4"
  reset_fixture
  git -C "$FIXTURE" mv "$from" "$to"
  git -C "$FIXTURE" commit --quiet -m "$label"
  run_current_diff_case "$label" "$expected"
}
run_delete_case() {
  local label="$1" expected="$2" path="$3"
  reset_fixture
  git -C "$FIXTURE" rm --quiet "$path"
  git -C "$FIXTURE" commit --quiet -m "$label"
  run_current_diff_case "$label" "$expected"
}
run_inventory_case() {
  local count="$1" path i
  reset_fixture
  for ((i = 1; i <= count; i++)); do
    path="$FIXTURE/docs/inventory/file-$i.md"
    mkdir -p "$(dirname "$path")"
    printf 'inventory %s\n' "$i" > "$path"
  done
  git -C "$FIXTURE" add -A
  git -C "$FIXTURE" commit --quiet -m "inventory-$count"
  run_current_diff_case "inventory-$count" semgrep_community
}

if [[ ! -f "$POLICY" || ! -f "$HELPER" ]]; then
  echo "RED: scan relevance contract is absent (expected policy and composeSelection helper)"
  echo "  policy: $([[ -f "$POLICY" ]] && echo present || echo missing)"
  echo "  helper: $([[ -f "$HELPER" ]] && echo present || echo missing)"
  exit 1
fi

# Matcher cases use the real dorny action against exact Git diffs. Expected scanner
# keys are intentionally explicit so accidental broadening also fails.
run_policy_case runner-docker-lock advisory-only .github/ci/docker-engine-packages.lock
run_policy_case runner-docker-install advisory-only scripts/ci/install-docker-engine.sh
run_policy_case runner-docker-services advisory-only scripts/ci/start-control-plane-services.sh
run_policy_case historical-docs semgrep_community \
  docs/history/plans/2026-10-07-3587-pr-a1-contract-codec.md \
  docs/history/plans/2026-10-07-3587-pr-a2-contract-verification.md \
  docs/history/plans/2026-10-07-3587-pr0-migrationrunner.md \
  docs/history/reports/2026-10-07-2553-account-root-authority-vendors.md \
  docs/history/reports/2026-10-07-2553-key-custody-decision-brief.md \
  docs/history/reports/2026-10-07-2553-recovery-sheet-decisions.md \
  docs/history/reports/2026-10-07-2553-vendor-review.md \
  docs/history/specs/2026-10-07-2553-account-key-authority-design-of-record.md
run_policy_case docs-ordinary-push semgrep_community docs/ordinary.md
run_policy_case docs-hotfix-push codeql_actions,codeql_go,codeql_js,eslint,govulncheck,osv,semgrep_custom,semgrep_community,semgrep_canary,sonar docs/hotfix.md
run_policy_case docs-schedule codeql_actions,codeql_go,codeql_js,eslint,govulncheck,osv,semgrep_custom,semgrep_community,semgrep_canary,sonar docs/scheduled.md
run_policy_case go-only codeql_go,govulncheck,osv,semgrep_custom,semgrep_community,sonar services/control-plane/internal/example/example.go
run_policy_case ts-only codeql_js,eslint,semgrep_custom,semgrep_community,sonar packages/protocol/src/renderer/example.ts
run_policy_case selfhost-docs semgrep_community docs/self-hosting/example.md
run_policy_case selfhost-packaging guard-input infrastructure/deploy/selfhost-storage.sh
run_policy_case ignored-package-tests eslint-false packages/protocol/tests/unit/example.test.ts
run_policy_case admin-tsconfig codeql_js,semgrep_community,sonar client/admin/tsconfig.json
run_policy_case actions-only codeql_actions,codeql_js,semgrep_community,semgrep_canary .github/workflows/example.yml
run_policy_case embedded-html codeql_go,codeql_js,govulncheck,semgrep_community,sonar services/control-plane/internal/email/templates/reset.html
run_policy_case admin-html codeql_js,semgrep_community,sonar client/admin/index.html
run_policy_case embedded-json codeql_js,semgrep_community,osv,sonar client/admin/package.json
run_policy_case embedded-yaml codeql_js,codeql_actions,semgrep_community,semgrep_canary .github/workflows/reusable.yml
run_policy_case desktop-manifest codeql_js,eslint,semgrep_community,sonar,osv package.json
run_policy_case admin-manifest codeql_js,semgrep_community,sonar,osv client/admin/package-lock.json
run_policy_case media-manifest codeql_js,semgrep_community,sonar,osv services/media-plane/package.json
run_policy_case control-plane-manifest codeql_go,govulncheck,osv,semgrep_community,sonar services/control-plane/go.sum
run_policy_case added-workspace codeql_js,semgrep_community,osv future-workspace/package.json
run_policy_case sonar-config semgrep_community,sonar sonar-project.properties
run_policy_case ignored-prototype '' prototypes/mobile-client/example.ts
run_policy_case sbom codeql_js,osv,semgrep_community docs/inventory.cdx.json
run_policy_case scanner-config codeql_js,semgrep_custom,semgrep_community,semgrep_canary .semgrep/rules/example.yml
run_policy_case admin-lock codeql_js,semgrep_community,sonar,osv client/admin/package-lock.json
tool_input_failures=0
run_policy_case license-tool-manifest codeql_js,osv,semgrep_community .github/tools/license-checker/package.json || tool_input_failures=1
run_policy_case license-tool-lock codeql_js,osv,semgrep_community .github/tools/license-checker/package-lock.json || tool_input_failures=1
[[ "$tool_input_failures" -eq 0 ]] || exit 1
run_policy_case notice semgrep_community NOTICE.md
run_policy_case go-import-reachability codeql_go,govulncheck,osv,semgrep_custom,semgrep_community,sonar services/control-plane/internal/example/example.go
run_policy_case go-toolchain-test-only semgrep_community scripts/test-check-go-toolchain.py
run_policy_case guard-workflow-lint codeql_actions,codeql_go,codeql_js,eslint,govulncheck,osv,semgrep_custom,semgrep_community,semgrep_canary,sonar \
  .github/workflows/ci-example.yml \
  .github/scripts/tests/ci-scan-relevance.test.sh \
  .github/scripts/ci-scan-relevance.mjs \
  .github/ci/scan-relevance.yml
run_policy_case workflow-lint-source-suite advisory-only .github/scripts/tests/ci-scan-relevance.test.sh
run_policy_case workflow-lint-source-helper advisory-only .github/scripts/ci-scan-relevance.mjs
run_policy_case workflow-lint-source-remote-builder advisory-only scripts/ci/require-remote-buildkit.py
run_policy_case workflow-lint-source-map advisory-only .github/ci/scan-relevance.yml
run_policy_case workflow-lint-only advisory-only .github/scripts/workflow-lint-only.txt
run_policy_case workflow-lint-legacy-changelog advisory-only CHANGELOG.md
run_policy_case guard-go-toolchain semgrep_community scripts/check-go-toolchain.py scripts/test-check-go-toolchain.py
run_policy_case toolchain-input-builder guard-input services/control-plane/Dockerfile
run_policy_case toolchain-input-ops-builder guard-input services/control-plane/Dockerfile.ops-agent
run_policy_case guard-openapi codeql_go,govulncheck,osv,semgrep_custom,semgrep_community,sonar scripts/api/check-openapi-coverage.sh services/control-plane/internal/stepup/openapi_contract_test.go
run_policy_case guard-deployment semgrep_community scripts/test-admin-ui-production-wiring.sh
run_policy_case guard-secrets-contract-workflow-consumer guard-input .github/workflows/provision-secrets.yml
run_policy_case guard-secrets-contract-composite-consumer guard-input .github/actions/release/action.yml
run_policy_case guard-secrets-contract-generic-workflow-yml guard-input .github/workflows/unlisted-consumer.yml
run_policy_case guard-secrets-contract-generic-workflow-yaml guard-input .github/workflows/unlisted-consumer.yaml
run_policy_case guard-secrets-contract-generic-composite guard-input .github/actions/unlisted/action.yml
run_delete_case guard-secrets-contract-deleted-workflow guard-input .github/workflows/removed-consumer.yaml
run_delete_case guard-secrets-contract-deleted-composite guard-input .github/actions/obsolete/action.yaml
run_policy_case guard-selfhost semgrep_community scripts/tests/test-install-selfhost.sh scripts/install-selfhost.sh
run_policy_case guard-feature-wiring semgrep_community scripts/check-feature-wiring.sh
run_policy_case feature-input-openapi guard-input docs/api/openapi.yaml
run_policy_case feature-input-compose guard-input docker-compose.production.yml
run_policy_case feature-input-primary-nginx guard-input infrastructure/deploy/nginx/concordvoice.conf
run_policy_case feature-input-admin-nginx guard-input infrastructure/deploy/nginx/admin-console.conf.template
run_policy_case guard-go-test-contracts semgrep_community scripts/check-no-unscoped-redis-flush.sh scripts/test-check-no-unscoped-redis-flush.sh
run_policy_case verifier-gate all-scanners .github/scripts/verify-pr-scan-results.py
run_policy_case wrapper-gate all-scanners .github/workflows/required-pr-build.yml
run_policy_case desktop-input-package desktop-input packages/e2ee-client/src/example.ts
run_policy_case guard-sonar-profile semgrep_community,sonar scripts/sonar/check-narrowed-profile.sh scripts/sonar/test-check-narrowed-profile.sh
run_policy_case guard-changelog semgrep_community .github/scripts/verify-changelog.sh .github/scripts/tests/verify-changelog.test.sh
run_policy_case guard-dev-checks semgrep_community scripts/concord-dev.sh scripts/tests/test-concord-dev-args.sh
run_delete_case delete-in-scope codeql_go,govulncheck,osv,semgrep_custom,semgrep_community,sonar services/control-plane/internal/example/deleted.go
run_transition_case rename-into-scope codeql_go,govulncheck,osv,semgrep_custom,semgrep_community,sonar docs/relevance.md services/control-plane/internal/example/renamed.go
run_transition_case rename-out-scope codeql_go,govulncheck,osv,semgrep_custom,semgrep_community,sonar services/control-plane/internal/example/renamed-out.go docs/renamed.md
run_inventory_case 301
run_inventory_case 3000
run_inventory_case 3001

# Execute the metadata step from pr-ci.yml itself with a fixture-backed `gh`
# response. Feed its emitted hotfix value into the real composer so the label
# classification and full-selection behavior are tested as one path.
python3 - "$REPO_ROOT" "$TASK_TMP/metadata-step.sh" <<'PY'
import sys
from pathlib import Path
import yaml
root, output = Path(sys.argv[1]), Path(sys.argv[2])
workflow = yaml.load((root / '.github/workflows/pr-ci.yml').read_text(), Loader=yaml.BaseLoader)
job = workflow['jobs']['detect-changes']
step = next(step for step in job['steps'] if step.get('id') == 'metadata')
output.write_text(step['run'])
PY
mkdir -p "$TASK_TMP/bin"
mkdir -p "$TASK_TMP/runner"
cat > "$TASK_TMP/bin/gh" <<'SH'
#!/bin/sh
cat "$PR_FIXTURE"
SH
chmod +x "$TASK_TMP/bin/gh"
for label in 'risk: hotfix' 'risk: security'; do
  fixture="$TASK_TMP/pr-${label//:/-}.json"
  output="$TASK_TMP/pr-${label//:/-}.output"
  printf '{"base":{"ref":"main","sha":"%040d"},"head":{"sha":"%040d"},"merge_commit_sha":"%040d","changed_files":1,"labels":[{"name":"%s"}]}' \
    1 2 3 "$label" > "$fixture"
  : > "$output"
  PATH="$TASK_TMP/bin:$PATH" PR_FIXTURE="$fixture" PR_NUMBER=123 GITHUB_OUTPUT="$output" \
    GITHUB_REPOSITORY=Concord-Voice/Self-Hosted RUNNER_TEMP="$TASK_TMP/runner" \
    GH_TOKEN=test-token-123 \
    bash "$TASK_TMP/metadata-step.sh"
done
CI_SCAN_SELECTION_DIR="$TASK_TMP" node --input-type=module - "$REPO_ROOT" "$TASK_TMP/pr-risk- hotfix.output" "$TASK_TMP/pr-risk- security.output" <<'NODE'
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';
import { resolve } from 'node:path';
const root = process.argv[2];
const { composeSelection, ANALYSIS_KEYS } = await import(pathToFileURL(
  resolve(root, '.github/scripts/ci-scan-relevance.mjs')));
const retained = ['services', 'spa', 'selfhost', 'desktop', 'control_plane', 'media_plane',
  'media_plane_tests', 'admin', 'production_docker', 'dev_stack', 'native', 'go_lint'];
const matcherLines = (await readFile(`${process.env.CI_SCAN_SELECTION_DIR}/historical-docs.dorny-output`, 'utf8')).split(/\r?\n/);
const filterNames = matcherLines.flatMap((line, i) => {
  const match = line.match(/^([a-z0-9_]+)<<.+$/);
  return match && (matcherLines[i + 1] === 'true' || matcherLines[i + 1] === 'false')
    ? [match[1]] : [];
});
const requiredFilterKeys = ['go_toolchain','openapi','spa_env','deployment','selfhost_guard','feature_wiring','workflow_lint',
  'adr_numbers','secrets_contract','dispatch_trust','go_test_contracts','review_contracts',
  'tooling_mirrors','graphify_contracts','native_ioc','sonar_profile','changelog','dev_checks',
  'worker_apple','worker_invite','license_desktop','license_media','license_admin','license_go'];
for (const key of requiredFilterKeys)
  assert.ok(filterNames.includes(key), `complete matcher filter set includes ${key}`);
const requiredOutputs = [...new Set([...filterNames, ...requiredFilterKeys, 'all_changed', ...retained])];
const outputs = Object.fromEntries(requiredOutputs.map(key => [key, 'false']));
const metadata = async path => {
  const lines = (await readFile(path, 'utf8')).split(/\r?\n/);
  const values = Object.fromEntries(lines.filter(line => line.includes('=')).map(line => {
    const at = line.indexOf('='); return [line.slice(0, at), line.slice(at + 1)];
  }));
  return { event_name: 'pull_request', hotfix: values.hotfix === 'true',
    head_sha: values.head_sha, base_sha: values.base_sha, merge_sha: values.merge_sha,
    changed_files: Number(values.changed_files) };
};
const opts = { mode: 'required-relevant', policySha: 'd'.repeat(40) };
const hotfix = composeSelection(outputs, await metadata(process.argv[3]), opts);
assert.equal(hotfix.known, true);
assert.equal(hotfix.hotfix, true, 'validated Hotfix label is carried to auxiliary consumers');
for (const key of ANALYSIS_KEYS.slice(0, 10)) {
  const nativeCodeQL = ['codeql_actions', 'codeql_go', 'codeql_js'].includes(key);
  assert.equal(hotfix.selected[key], !nativeCodeQL,
    `risk: hotfix PR ${nativeCodeQL ? 'leaves native ownership outside' : 'selects'} ${key}`);
}
const ordinary = composeSelection(outputs, await metadata(process.argv[4]), opts);
assert.equal(ordinary.known, true);
assert.equal(ordinary.hotfix, false, 'ordinary PR does not request auxiliary full-scan behavior');
for (const key of ANALYSIS_KEYS.slice(0, 10)) assert.equal(ordinary.selected[key], false, `ordinary label does not select ${key}`);
const unavailablePush = composeSelection(outputs, { event_name: 'push', hotfix: false,
  before: 'b'.repeat(40), after: 'c'.repeat(40), history_available: false }, opts);
assert.equal(unavailablePush.known, false, 'failed push provenance stays unknown for full-consumer fallback');
const unknownMode = composeSelection(outputs, await metadata(process.argv[4]), { ...opts, mode: 'unset' });
assert.equal(unknownMode.known, false, 'unrecognized policy mode stays unknown');

const complete = { ...outputs };
const base = { event_name: 'pull_request', hotfix: false, head_sha: 'a'.repeat(40),
  base_sha: 'b'.repeat(40), merge_sha: 'c'.repeat(40), changed_files: 8 };
for (const key of requiredOutputs) {
  const { [key]: _removed, ...missing } = complete;
  assert.equal(composeSelection(missing, base, opts).known, false, `missing ${key} is unknown`);
  for (const malformed of ['', 'yes', true]) {
    assert.equal(composeSelection({ ...complete, [key]: malformed }, base, opts).known, false,
      `malformed ${key} is unknown`);
  }
}

const suites = ['desktop', 'control_plane', 'media_plane', 'admin'];
const falseCoverage = Object.fromEntries(suites.map(key => [key, false]));
assert.deepEqual(composeSelection(complete, base, opts).coverage_required, falseCoverage);
for (const suite of suites) {
  const expected = { ...falseCoverage, [suite]: true };
  assert.deepEqual(composeSelection({ ...complete, [`sonar_${suite}`]: 'true' }, base, opts).coverage_required,
    expected, `Sonar ${suite} requires only its matching producer`);
  assert.deepEqual(composeSelection({ ...complete, [suite]: 'true' }, base, opts).coverage_required,
    expected, `legacy ${suite} producer selection requires matching coverage`);
}
assert.deepEqual(composeSelection({ ...complete, sonar_force_all: 'true' }, base, opts).coverage_required,
  Object.fromEntries(suites.map(key => [key, true])), 'force-all requires all four exact producers');
console.log('PASS: actual PR label metadata, retained component validation, and exact coverage requirements');
NODE

CI_SCAN_SELECTION_DIR="$TASK_TMP" node --input-type=module - "$REPO_ROOT" "$BUNDLE" <<'NODE'
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';
import { resolve } from 'node:path';

const root = process.argv[2];
const bundle = process.argv[3];
await (await import('node:fs/promises')).stat(bundle);
const { composeSelection, ANALYSIS_KEYS } = await import(pathToFileURL(
  resolve(root, '.github/scripts/ci-scan-relevance.mjs')));
const retained = ['services', 'spa', 'selfhost', 'desktop', 'control_plane', 'media_plane',
  'media_plane_tests', 'admin', 'production_docker', 'dev_stack', 'native', 'go_lint'];
const selectionDir = process.env.CI_SCAN_SELECTION_DIR;
const matcherLines = (await readFile(`${selectionDir}/historical-docs.dorny-output`, 'utf8')).split(/\r?\n/);
const filterNames = matcherLines.flatMap((line, i) => {
  const match = line.match(/^([a-z0-9_]+)<<.+$/);
  return match && (matcherLines[i + 1] === 'true' || matcherLines[i + 1] === 'false')
    ? [match[1]] : [];
});
const outputs = Object.fromEntries(filterNames.map(key => [key, 'false']));
const meta = { event_name: 'pull_request', hotfix: false, head_sha: 'a'.repeat(40),
  base_sha: 'b'.repeat(40), merge_sha: 'c'.repeat(40), changed_files: 8 };
const policySha = 'd'.repeat(40);
const selected = (filters, m = meta, mode = 'required-relevant') =>
  composeSelection(filters, m, { mode, policySha });
const pushMeta = { event_name: 'push', hotfix: false, before: 'a'.repeat(40),
  after: 'b'.repeat(40), history_available: true };

// Historical repro: docs-only PR #3691 must keep text/community and secret checks,
// while language-specific and project-root tool scans stay out.
const docs3691 = selected({ ...outputs, semgrep_community: 'true' });
assert.equal(docs3691.known, true, '#3691 metadata is known');
assert.deepEqual(docs3691.selected, Object.fromEntries(ANALYSIS_KEYS.map(key =>
  [key, key === 'semgrep_community'])), '#3691 selects only community text scan');
assert.deepEqual(JSON.parse(docs3691.codeql_matrix), { include: [] });

for (const [name, key] of [['Go-only', 'codeql_go'], ['TS-only', 'codeql_js'],
  ['Actions-only', 'codeql_actions']]) {
  const value = selected({ ...outputs, [key]: 'true' }, pushMeta);
  assert.deepEqual(value.selected, Object.fromEntries(ANALYSIS_KEYS.map(candidate =>
    [candidate, candidate === key])), `${name} selects only ${key}`);
  assert.equal(value.known, true, `${name} is known`);
}

// Every analysis-affecting embedded resource/config input is represented by its
// dorny output; removals and both rename endpoints are supplied by the matcher.
for (const key of ['codeql_go', 'codeql_js', 'sonar_desktop', 'sonar_control_plane',
  'sonar_media_plane', 'sonar_admin', 'sonar_force_all', 'osv', 'semgrep_community']) {
  const value = selected({ ...outputs, [key]: 'true' });
  assert.equal(value.known, true, `${key} inventory result is known`);
  assert.ok(value.selected[key] || value.coverage_required, `${key} result composes`);
}
assert.deepEqual(selected(outputs).coverage_required,
  { desktop: false, control_plane: false, media_plane: false, admin: false },
  'unselected suites require no coverage');
for (const n of [300, 301, 3000]) {
  assert.equal(selected(outputs, { ...meta, changed_files: n }).known, true,
    `${n} files stays within the documented cap`);
}
for (const n of [3001, -1, 1.5])
  assert.equal(selected(outputs, { ...meta, changed_files: n }).known, false,
    `${n} changed_files is unknown`);
assert.equal(selected({}, meta).known, false, 'missing filter output is unknown');
assert.equal(selected({ ...outputs, codeql_go: 'yes' }).known, false,
  'malformed boolean output is unknown');
assert.equal(selected(outputs, meta, 'future-mode').known, false, 'invalid mode is unknown');
assert.equal(selected(outputs, { ...meta, head_sha: 'bad' }).known, false,
  'invalid PR SHA is unknown');

const legacy = selected(outputs, meta, 'legacy');
assert.equal(legacy.known, true, 'legacy metadata remains accepted');
for (const key of ['eslint', 'govulncheck', 'osv', 'semgrep_custom', 'semgrep_community', 'semgrep_canary', 'sonar'])
  assert.equal(legacy.selected[key], true, `legacy defaults to full ${key}`);
for (const key of ['codeql_actions', 'codeql_go', 'codeql_js'])
  assert.equal(legacy.selected[key], false, `PR legacy mode leaves native CodeQL ownership to GitHub: ${key}`);
for (const mode of ['legacy', 'required-full', 'required-relevant']) {
  const prSelection = selected({ ...outputs, codeql_go: 'true' }, meta, mode);
  for (const key of ['codeql_actions', 'codeql_go', 'codeql_js'])
    assert.equal(prSelection.selected[key], false, `${mode} PR never claims protected ${key}`);
}
for (const mode of ['legacy', 'required-full']) {
  const pushSelection = selected(outputs, pushMeta, mode);
  for (const key of ['codeql_actions', 'codeql_go', 'codeql_js'])
    assert.equal(pushSelection.selected[key], true, `${mode} push keeps ${key}`);
}

const suites = ['desktop', 'control_plane', 'media_plane', 'admin'];
const allCoverage = Object.fromEntries(suites.map(key => [key, true]));
const noCoverage = Object.fromEntries(suites.map(key => [key, false]));
assert.deepEqual(selected(outputs, meta, 'legacy').coverage_required, noCoverage,
  'ordinary legacy calls preserve selected component coverage');
assert.deepEqual(selected(outputs, meta, 'required-full').coverage_required, allCoverage,
  'protected full mode requires all coverage producers');
assert.deepEqual(selected(outputs, { ...meta, hotfix: true }).coverage_required, allCoverage,
  'Hotfix selection forces coverage producers');
assert.deepEqual(selected({ ...outputs, sonar_force_all: 'true' }, meta).coverage_required, allCoverage,
  'global Sonar config selection forces all four producers');
for (const suite of suites) {
  const expected = { ...noCoverage, [suite]: true };
  assert.deepEqual(selected({ ...outputs, [suite]: 'true' }, meta, 'legacy').coverage_required,
    expected, `legacy raw ${suite} selection preserves its own coverage flag`);
}

const goMatrix = JSON.parse(selected({ ...outputs, codeql_go: 'true' }, pushMeta).codeql_matrix).include;
assert.deepEqual(goMatrix, [{ language: 'go', runner: 'ubuntu-24.04', 'build-mode': 'manual' }],
  'selected CodeQL matrix preserves the existing Go leg values');
const docsMatrix = JSON.parse(selected({ ...outputs, semgrep_community: 'true' }, meta).codeql_matrix);
assert.deepEqual(docsMatrix, { include: [] }, 'docs-only scans allocate no CodeQL leg');

const fs = await import('node:fs');
for (const [filename, key] of [['go-only', 'codeql_go'], ['historical-docs', 'semgrep_community'],
  ['sonar-config', 'sonar_force_all']]) {
  const emitted = JSON.parse(fs.readFileSync(`${selectionDir}/${filename}.selection.json`, 'utf8'));
  assert.equal(emitted.known, true, `${filename} selection is known`);
  assert.equal(emitted.selected[key], true, `${filename} emitted selection contains ${key}`);
}
const emittedSonar = JSON.parse(fs.readFileSync(`${selectionDir}/sonar-config.selection.json`, 'utf8'));
assert.deepEqual(emittedSonar.coverage_required, allCoverage, 'emitted Sonar config requires all four suites');
console.log(`PASS: ${ANALYSIS_KEYS.length} analysis keys; historical, boundary, error, and legacy contracts`);
NODE

# Ensure the policy names the same immutable action tested above.
grep -qF "$BUNDLE_SHA" "$REPO_ROOT/.github/workflows/pr-ci.yml"


# Regression for #2624: the shared native broker is executed by both workspaces.
run_policy_case broker-shared-vector broker-input services/control-plane/internal/voicecommand/testdata/envelopes.json
run_policy_case broker-unrelated-doc broker-input docs/nats-resolver-unrelated.md
run_policy_case broker-resolver broker-input scripts/test-tools/nats-server.sh
run_policy_case broker-contract-suite broker-input scripts/test-nats-server-bootstrap.sh
broker_routing_status=0
node --input-type=module - "$TASK_TMP" <<'NODE' || broker_routing_status=$?
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
const dir = process.argv[2];
const keys = ['control_plane', 'media_plane_tests'];
const actual = async label => {
  const lines = (await readFile(`${dir}/${label}.dorny-output`, 'utf8')).split(/\r?\n/);
  const filters = {};
  for (let i = 0; i < lines.length; i++) {
    const match = lines[i].match(/^([a-z0-9_]+)<<(.+)$/);
    if (match && ['true', 'false'].includes(lines[i + 1])) filters[match[1]] = lines[i + 1];
  }
  assert.equal(filters.all_changed, 'true', `${label}: actual changed path reaches the matcher`);
  for (const key of keys) assert.ok(['true', 'false'].includes(filters[key]), `${label}: matcher emits ${key}`);
  return filters;
};
const shared = await actual('broker-shared-vector');
const unrelated = await actual('broker-unrelated-doc');
const resolver = await actual('broker-resolver');
await test('shared voice-command fixture selects both workspaces (positive control)', () => {
  assert.deepEqual(keys.map(key => shared[key]), ['true', 'true']);
});
await test('unrelated documentation selects neither broker consumer (negative control)', () => {
  assert.deepEqual(keys.map(key => unrelated[key]), ['false', 'false']);
});
for (const key of keys) await test(`native NATS resolver change selects ${key} (#2624)`, () => {
  assert.equal(resolver[key], 'true', `shared NATS resolver must select ${key}`);
});
const selected = JSON.parse(await readFile(`${dir}/broker-resolver.selection.json`, 'utf8'));
for (const key of ['control_plane', 'media_plane']) await test(`native NATS resolver change requires ${key} coverage (#2624)`, () => {
  assert.equal(selected.coverage_required[key], true, `shared NATS resolver must require ${key} coverage`);
});
NODE

broker_registration_status=0
python3 - "$REPO_ROOT" "$TASK_TMP" <<'PY' || broker_registration_status=$?
# Reuse the repository's supported command/enforcement recognisers. A filename
# in prose, echo, an uncalled function, or a heredoc is not a registration edge.
import ast
import json
import re
import shlex
import sys
from pathlib import Path
import yaml

root, evidence = Path(sys.argv[1]), Path(sys.argv[2])
library_path = root / '.github/scripts/lint_test_suite_wiring.py'
module = ast.parse(library_path.read_text(), filename=str(library_path))
cut = next(i for i, node in enumerate(module.body) if isinstance(node, ast.Assign)
           and any(isinstance(target, ast.Name) and target.id == 'suites' for target in node.targets))
library = {}
exec(compile(ast.Module(body=module.body[:cut], type_ignores=[]), str(library_path), 'exec'), library)

scripts = {str(path.relative_to(root)): path.read_text()
           for pattern in ('.github/scripts/*.sh', '.github/scripts/tests/*.test.sh', 'scripts/*.sh')
           for path in root.glob(pattern)}
workflow = yaml.safe_load((root / '.github/workflows/pr-ci.yml').read_text())
pc = yaml.safe_load((root / '.pre-commit-config.yaml').read_text())


def invokes(body, path, shell=None):
    library['SUITE_DIR'] = str(Path(path).parent)
    return library['invokes'](body, Path(path).name) and not library['swallows_failure'](body, Path(path).name, shell)


def reaches(body, target, shell=None, seen=None):
    if invokes(body, target, shell):
        return True
    seen = set() if seen is None else seen
    for path, source in scripts.items():
        if path not in seen and path in body and invokes(body, path, shell):
            seen.add(path)
            if reaches(source, target, 'bash', seen):
                return True
    return False


def selected(condition, selection):
    condition = str(condition or '')
    if library['statically_false'](condition) or library['event_conjunct_excludes_pr'](condition):
        return False
    families = re.findall(r'\.advisory\.([a-z_]+)', condition)
    return not families or any(selection['advisory'].get(family, False) for family in families)


def registered(target, changed, selection):
    # These ordinary PR quality jobs execute checked-out head scripts. This does
    # not claim that an edited reusable workflow ran under the protected pin.
    for name in ('workflow-lint', 'static-guards'):
        job = workflow['jobs'][name]
        if job.get('continue-on-error') or not selected(job.get('if'), selection):
            continue
        for step in job['steps']:
            if not step.get('continue-on-error') and selected(step.get('if'), selection):
                if reaches(step.get('run', ''), target, library['resolve_shell'](workflow, job, step)):
                    return True
    for repo in pc['repos']:
        for hook in repo['hooks']:
            entry = str(hook.get('entry', ''))
            # pre-commit execs argv directly; shell operators in entry are data.
            if any(word in {';', '&&', '||', '|', '&'} for word in shlex.split(entry)):
                continue
            stages = hook.get('stages', pc.get('default_stages', ['pre-commit']))
            if not any(stage in stages for stage in ('commit', 'pre-commit')):
                continue
            if any(hook.get(key) for key in ('types', 'types_or', 'exclude_types')):
                continue  # This focused check certifies only unrestricted shell hooks.
            includes = [str(node['files']) for node in (pc, hook) if node.get('files')]
            excludes = [str(node['exclude']) for node in (pc, hook) if node.get('exclude')]
            if all(re.search(pattern, changed) for pattern in includes) and not any(re.search(pattern, changed) for pattern in excludes):
                if reaches(entry, target, 'bash'):
                    return True
    return False


suite = 'scripts/test-nats-server-bootstrap.sh'
assert (root / suite).is_file(), 'contract-suite fixture is a real repository file'
assert (root / 'scripts/test-tools/nats-server.sh').is_file(), 'resolver fixture is a real repository file'
assert (root / 'services/control-plane/internal/voicecommand/testdata/envelopes.json').is_file(), 'shared-vector control is a real repository file'
assert invokes(f'bash {suite}', suite), 'supported shell invocation is recognised (positive control)'
for mention in (f'echo {suite}', f'bash -n {suite}', f'# bash {suite}', f"cat <<'TEXT'\nbash {suite}\nTEXT"):
    assert not invokes(mention, suite), f'mere mention cannot register the contract suite: {mention}'
control = json.loads((evidence / 'workflow-lint-source-suite.selection.json').read_text())
assert registered('.github/scripts/tests/ci-scan-relevance.test.sh', '.github/scripts/tests/ci-scan-relevance.test.sh', control), 'actual CI selector suite is registered (positive control)'
print('PASS: actual quality-check registration and invocation-vs-mention controls')
failures = []
for label, changed in (('broker-resolver', 'scripts/test-tools/nats-server.sh'), ('broker-contract-suite', suite)):
    selection = json.loads((evidence / f'{label}.selection.json').read_text())
    assert selection['known'] is True, 'matching relevance is based on validated actual matcher output'
    if not registered(suite, changed, selection):
        failures.append(f'hermetic NATS acquisition/cache/cleanup contract suite must be registered for {changed}')
for failure in failures:
    print(f'RED: {failure}')
assert not failures, '; '.join(failures)
PY
[[ "$broker_routing_status" -eq 0 && "$broker_registration_status" -eq 0 ]]
