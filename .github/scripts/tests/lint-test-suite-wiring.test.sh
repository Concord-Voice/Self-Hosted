#!/usr/bin/env bash
#
# Self-test for lint-test-suite-wiring.sh.
#
# The load-bearing cases are the three shapes that actually occurred in pr-ci.yml
# during #3139 — a suite with no runner, a suite behind a filter that omits it,
# and a suite whose IMPLEMENTATION the filter omits. A lint for a defect it
# cannot reproduce is decoration, so each of the three is built as a fixture
# rather than asserted in prose.
#
# The vacuity case matters as much: an empty tests directory must exit 2, not 0.
# "Zero suites, zero problems" is exactly how a guard reports success having
# checked nothing.
#
# The four FAIL-OPEN cases below were found by bot review of the lint's own first
# revision, and each one made the lint certify a suite it had not verified:
# an echoed or commented-out filename counted as an invocation, a gate naming a
# filter output the job never declared was skipped rather than reported, and a
# pre-commit hook whose `files:` excluded the suite counted as a runner. A guard
# that passes on those is worse than no guard, because it also stops anyone
# looking.

set -euo pipefail

LINT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lint-test-suite-wiring.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
pass=0; fail=0

# mk_repo <filter-block> <run-block> [no-impl] [py] [precommit:<files-regex>] [ext:yaml]
#
# Builds a throwaway tree holding one suite, `demo.test.sh`, and (unless
# `no-impl`) its implementation `demo.sh`. `py` adds `demo.py`, the sibling a
# script keeps its logic in. `precommit:<re>` adds a pre-commit hook that
# invokes the suite, gated on that `files:` pattern.
#
# The directory comes from `mktemp -d`, NOT from an incrementing counter: this
# function is always called inside `$(...)`, so a counter increments in a
# SUBSHELL and never reaches the parent. Every fixture then shared `case-1`,
# each overwriting the last — the earlier revision did exactly that, and the
# suite still reported 9 passes because each case happened to overwrite the tree
# with the very shape it was about to assert.
mk_repo() {
  local filters="$1" run="$2" dir impl=1 py=0 pcfiles="" ext="yml" decoy=0 pctop="" pfif=""
  local on_block="on: [pull_request]"
  shift 2
  for opt in "$@"; do
    case "$opt" in
      no-impl) impl=0 ;;
      py) py=1 ;;
      precommit:*) pcfiles="${opt#precommit:}" ;;
      ext:*) ext="${opt#ext:}" ;;
      decoy) decoy=1 ;;
      on:*) on_block="${opt#on:}" ;;
      pctop:*) pctop="${opt#pctop:}" ;;
      pfif:*) pfif="${opt#pfif:}" ;;
      *) echo "mk_repo: unknown option: $opt" >&2; return 2 ;;
    esac
  done
  dir=$(mktemp -d "$TMP/case-XXXXXX")
  echo "$dir" >> "$TMP/builder-calls"
  mkdir -p "$dir/.github/workflows" "$dir/.github/scripts/tests"
  printf '#!/usr/bin/env bash\ntrue\n' > "$dir/.github/scripts/tests/demo.test.sh"
  [[ "$impl" == 1 ]] && printf '#!/usr/bin/env bash\ntrue\n' > "$dir/.github/scripts/demo.sh"
  [[ "$py" == 1 ]] && printf 'pass\n' > "$dir/.github/scripts/demo.py"
  # A DIFFERENT file whose basename collides with the suite.
  if [[ "$decoy" == 1 ]]; then
    mkdir -p "$dir/.github/other"
    printf '#!/usr/bin/env bash\ntrue\n' > "$dir/.github/other/demo.test.sh"
  fi
  # Fixtures are written with printf, not `cat <<YML`: this suite runs in a
  # pre-commit hook, and on macOS bash before 5.3 patch 16 a heredoc over 512
  # bytes can hang in its pipe write (see promote-unreleased-changelog.sh and
  # #3426). The size that counts is AFTER expansion, and several fixtures here
  # expand past 512. A double-quoted string expands exactly like an unquoted
  # heredoc body, so the bytes written are unchanged. Keep `"` out of them.
  local yml="name: ci
$on_block
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: dorny/paths-filter@v3
        id: filter
${pfif:+        if: $pfif}
        with:
          filters: |
            workflows:
$filters
      - name: run the suites
$run"
  printf '%s\n' "$yml" > "$dir/.github/workflows/ci.$ext"
  if [[ -n "$pcfiles" || -n "$pctop" ]]; then
    yml="${pctop:+files: '$pctop'}
repos:
  - repo: local
    hooks:
      - id: demo
        name: demo
        entry: bash .github/scripts/tests/demo.test.sh
        language: system
${pcfiles:+        files: '$pcfiles'}"
    printf '%s\n' "$yml" > "$dir/.pre-commit-config.yaml"
  fi
  printf '%s' "$dir"
}

# mk_joblevel <upstream-filter-paths-block>
#
# The other common valid layout: a `changes` job exposes a dorny output and the
# runner job gates on `needs.changes.outputs.workflows`. The runner STEP then has
# no `if:` at all, so a step-only reading of conditions calls it ungated and
# certifies it -- while omitting the suite from the upstream filter skips the
# whole job.
mk_joblevel() {
  local upstream="$1" dir
  dir=$(mktemp -d "$TMP/case-XXXXXX")
  echo "$dir" >> "$TMP/builder-calls"
  mkdir -p "$dir/.github/workflows" "$dir/.github/scripts/tests"
  printf '#!/usr/bin/env bash\ntrue\n' > "$dir/.github/scripts/tests/demo.test.sh"
  printf '#!/usr/bin/env bash\ntrue\n' > "$dir/.github/scripts/demo.sh"
  # printf, not a heredoc: see mk_repo.
  local yml="name: ci
on: [pull_request]
jobs:
  changes:
    runs-on: ubuntu-latest
    outputs:
      workflows: \${{ steps.filter.outputs.workflows }}
    steps:
      - uses: dorny/paths-filter@v3
        id: filter
        with:
          filters: |
            workflows:
$upstream
  lint:
    needs: changes
    if: needs.changes.outputs.workflows == 'true'
    runs-on: ubuntu-latest
    steps:
      - name: run the suites
        run: bash .github/scripts/tests/demo.test.sh"
  printf '%s\n' "$yml" > "$dir/.github/workflows/ci.yml"
  printf '%s' "$dir"
}

# mk_joblevel_gate <job-if-expression> <upstream-filter-paths-block>
# As mk_joblevel, but the caller chooses the gate expression — for shapes the
# regex must recognise beyond a plain `== 'true'` comparison.
mk_joblevel_gate() {
  local gate="$1" upstream="$2" dir
  dir=$(mktemp -d "$TMP/case-XXXXXX")
  echo "$dir" >> "$TMP/builder-calls"
  mkdir -p "$dir/.github/workflows" "$dir/.github/scripts/tests"
  printf '#!/usr/bin/env bash\ntrue\n' > "$dir/.github/scripts/tests/demo.test.sh"
  printf '#!/usr/bin/env bash\ntrue\n' > "$dir/.github/scripts/demo.sh"
  # printf, not a heredoc: see mk_repo.
  local yml="name: ci
on: [pull_request]
jobs:
  changes:
    runs-on: ubuntu-latest
    outputs:
      workflows: \${{ steps.filter.outputs.workflows }}
    steps:
      - uses: dorny/paths-filter@v3
        id: filter
        with:
          filters: |
            workflows:
$upstream
  lint:
    needs: changes
    if: $gate
    runs-on: ubuntu-latest
    steps:
      - name: run the suites
        run: bash .github/scripts/tests/demo.test.sh"
  printf '%s\n' "$yml" > "$dir/.github/workflows/ci.yml"
  printf '%s' "$dir"
}

# mk_relevance_consumer <gate> [omit-path]
# Mirrors the special shared-selector consumer: workflow-lint is selected by the
# immutable advisory family map instead of a local paths-filter output. The
# suite uses an .mjs implementation, which must resolve through the shared map.
mk_relevance_consumer() {
  local gate="$1" omit="${2:-}" dir
  dir=$(mktemp -d "$TMP/case-XXXXXX")
  echo "$dir" >> "$TMP/builder-calls"
  mkdir -p "$dir/.github/workflows" "$dir/.github/scripts/tests" "$dir/.github/ci"
  printf '#!/usr/bin/env bash\ntrue\n' > "$dir/.github/scripts/tests/ci-scan-relevance.test.sh"
  printf 'export {}\n' > "$dir/.github/scripts/ci-scan-relevance.mjs"
  local suite="      - '.github/scripts/tests/ci-scan-relevance.test.sh'"
  local helper="      - '.github/scripts/ci-scan-relevance.mjs'"
  [[ "$omit" == suite ]] && suite=""
  [[ "$omit" == helper ]] && helper=""
  printf '%s\n' "workflow_lint:" \
    "  - '.github/ci/scan-relevance.yml'" \
    "  - '.github/scripts/ci-scan-relevance.mjs'" \
    "  - '.github/scripts/tests/ci-scan-relevance.test.sh'" > "$dir/.github/ci/scan-relevance.yml"
  [[ "$omit" == suite ]] && sed -i.bak '/ci-scan-relevance.test.sh/d' "$dir/.github/ci/scan-relevance.yml"
  [[ "$omit" == helper ]] && sed -i.bak '/ci-scan-relevance.mjs/d' "$dir/.github/ci/scan-relevance.yml"
  [[ "$omit" == map ]] && sed -i.bak '/scan-relevance.yml/d' "$dir/.github/ci/scan-relevance.yml"
  printf '%s\n' "name: ci" \
    "on: [pull_request]" \
    "jobs:" \
    "  workflow-lint:" \
    "    needs: [ioc, detect-changes]" \
    "    if: $gate" \
    "    runs-on: ubuntu-latest" \
    "    steps:" \
    "      - name: suite" \
    "        run: bash .github/scripts/tests/ci-scan-relevance.test.sh" \
    > "$dir/.github/workflows/pr-ci.yml"
  printf '%s' "$dir"
}

check() {  # <expected-exit> <label> <repo-dir>
  local want="$1" label="$2" dir="$3" out actual
  set +e
  out=$(bash "$LINT" "$dir" 2>&1)
  actual=$?
  set -e
  if [[ "$actual" == "$want" ]]; then
    echo "  PASS: $label"; pass=$((pass + 1))
  else
    echo "  FAIL: $label (expected exit $want, got $actual)" >&2
    printf '    %s\n' "$out" >&2
    fail=$((fail + 1))
  fi
}

check_msg() {  # <expected-exit> <grep-pattern> <label> <repo-dir>
  local want="$1" pat="$2" label="$3" dir="$4" out actual
  set +e
  out=$(bash "$LINT" "$dir" 2>&1)
  actual=$?
  set -e
  # Exit code ALONE cannot tell a diagnosis from a crash -- both are 1. Disabling
  # the undeclared-filter-name check makes the lint raise KeyError and exit 1,
  # which satisfied an exit-only assertion; mutation testing found that mutant
  # surviving for exactly that reason. For a lint whose whole value is what it
  # SAYS, the message is the assertion.
  if [[ "$actual" == "$want" ]] && grep -qi -- "$pat" < <(printf '%s\n' "$out"); then
    echo "  PASS: $label"; pass=$((pass + 1))
  else
    echo "  FAIL: $label (exit $actual, want $want; message must match: $pat)" >&2
    printf '    %s\n' "$out" >&2
    fail=$((fail + 1))
  fi
}

GATED='        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: bash .github/scripts/tests/demo.test.sh'
UNGATED='        run: bash .github/scripts/tests/demo.test.sh'
# Same invocation, but the `if:` names an output this job never declares.
TYPO_GATE='        if: steps.filter.outputs.workflow == '"'"'true'"'"'
        run: bash .github/scripts/tests/demo.test.sh'
NO_RUN='        run: echo "nothing invokes the suite"'
TYPO_STEP='        if: steps.filtre.outputs.workflows == '"'"'true'"'"'
        run: bash .github/scripts/tests/demo.test.sh'
INVERSE='        if: steps.filter.outputs.workflows == '"'"'false'"'"'
        run: bash .github/scripts/tests/demo.test.sh'
OTHER_SUITE='        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: bash .github/scripts/tests/not-demo.test.sh'
ECHOED='        run: echo bash .github/scripts/tests/demo.test.sh'
BOTH="              - '.github/scripts/tests/demo.test.sh'
              - '.github/scripts/demo.sh'"
BOTH_PY="$BOTH
              - '.github/scripts/demo.py'"
TEST_ONLY="              - '.github/scripts/tests/demo.test.sh'"
NEITHER="              - '.github/workflows/**'"

echo "Wired and reachable (exit 0):"
check 0 "ungated invocation needs no filter entry" "$(mk_repo "$NEITHER" "$UNGATED")"
check 0 "gated invocation with suite AND impl listed" "$(mk_repo "$BOTH" "$GATED")"
# A suite with no `X.sh` beside it must not be asked for an implementation entry
# that could never match anything — the empty-`impl_paths` branch, which no
# fixture reached while `no-impl` was documented but never passed by any caller.
check 0 "a suite with no implementation script needs only its own entry" \
  "$(mk_repo "$TEST_ONLY" "$GATED" no-impl)"
check 0 "a pre-commit hook whose files: matches is a valid runner" \
  "$(mk_repo "$NEITHER" "$NO_RUN" 'precommit:^\.github/scripts/')"

echo ""
echo "Shared relevance selector:"

RELEVANCE_GATE='${{ !cancelled() && needs.ioc.result == '\''success'\'' && (needs.detect-changes.result != '\''success'\'' || needs.detect-changes.outputs.scan-known != '\''true'\'' || fromJSON(needs.detect-changes.outputs.scan-selection).advisory.workflow_lint) }}'
check 0 "shared workflow-lint selector resolves test, helper, and map" \
  "$(mk_relevance_consumer "$RELEVANCE_GATE")"
RELEVANCE_GATE_HOTFIX='${{ !cancelled() && needs.ioc.result == '\''success'\'' && (needs.detect-changes.result != '\''success'\'' || needs.detect-changes.outputs.scan-known != '\''true'\'' || fromJSON(needs.detect-changes.outputs.scan-selection).hotfix || fromJSON(needs.detect-changes.outputs.scan-selection).advisory.workflow_lint) }}'
check 0 "Hotfix full-scan disjunct preserves the shared workflow-lint selector" \
  "$(mk_relevance_consumer "$RELEVANCE_GATE_HOTFIX")"
check_msg 1 "matches no pattern in that filter" \
  "shared selector rejects a map missing the suite" \
  "$(mk_relevance_consumer "$RELEVANCE_GATE" suite)"
check_msg 1 "omits required source" \
  "shared selector rejects a map missing the helper" \
  "$(mk_relevance_consumer "$RELEVANCE_GATE" helper)"
check_msg 1 "omits required source" \
  "shared selector rejects a policy that does not match its own path" \
  "$(mk_relevance_consumer "$RELEVANCE_GATE" map)"
check_msg 1 "fail-closed detector fallback" \
  "shared selector requires unknown-detector fallback" \
  "$(mk_relevance_consumer '${{ !cancelled() && needs.ioc.result == '\''success'\'' && (fromJSON(needs.detect-changes.outputs.scan-selection).advisory.workflow_lint) }}')"
check_msg 1 "SKIPPED" \
  "shared selector rejects an inverted advisory gate" \
  "$(mk_relevance_consumer '${{ needs.detect-changes.result != '\''success'\'' || needs.detect-changes.outputs.scan-known != '\''true'\'' || !fromJSON(needs.detect-changes.outputs.scan-selection).advisory.workflow_lint }}')"
check_msg 1 "ci-scan-relevance.test.sh:" \
  "shared selector rejects an unrecognized advisory family" \
  "$(mk_relevance_consumer '${{ needs.detect-changes.result != '\''success'\'' || needs.detect-changes.outputs.scan-known != '\''true'\'' || fromJSON(needs.detect-changes.outputs.scan-selection).advisory.workflow_lintty }}')"
check_msg 1 "not the supported workflow-lint selector source" \
  "shared selector rejects an unrecognized producer output" \
  "$(mk_relevance_consumer '${{ needs.detect-changes.result != '\''success'\'' || needs.detect-changes.outputs.scan-known != '\''true'\'' || fromJSON(needs.detect-changes.outputs.other-selection).advisory.workflow_lint }}')"

echo ""
echo "The three shapes that actually occurred (exit 1):"

# Shape 2 of #3139: two suites existed with no runner anywhere and had therefore
# never executed, in CI or locally, despite passing when run by hand.
check_msg 1 "NO RUNNER" "a suite with NO runner is caught" "$(mk_repo "$BOTH" "$NO_RUN")"

# Shape 1: invoked behind a filter that does not list it, so the job fast-exits
# before the step on exactly the PRs that change the suite.
# `no-impl` is load-bearing: with `demo.sh` present the impl-path branch
# produces exit 1 on its own, so this case passed whether or not the
# suite-path check ran. Same vacuity as the pre-commit pair below.
check_msg 1 "tests/demo.test.sh matches no pattern" \
  "gated invocation whose suite is absent from the filter" \
  "$(mk_repo "$NEITHER" "$GATED" no-impl)"

# Shape 3: the suite was listed but its implementation was not — a change to the
# script under test alone would not run its own tests.
check_msg 1 "its implementation" \
  "gated invocation whose IMPLEMENTATION is absent from the filter" \
  "$(mk_repo "$TEST_ONLY" "$GATED")"

# A script that keeps its logic in a snake_case .py beside it
# (promote_unreleased_changelog.py) has TWO files under test. Listing only the
# .sh lets a PR that changes only the .py skip the suite, in CI and pre-commit.
check_msg 1 "demo.py matches no pattern" \
  "gated invocation whose sibling .py is absent from the filter" \
  "$(mk_repo "$BOTH" "$GATED" py)"
check 0 "gated invocation with suite, .sh AND sibling .py listed" \
  "$(mk_repo "$BOTH_PY" "$GATED" py)"
check_msg 1 "does not match .github/scripts/demo.py" \
  "a pre-commit files: that reaches the .sh but not its sibling .py" \
  "$(mk_repo "$NEITHER" "$NO_RUN" py 'precommit:^\.github/scripts/(tests/demo\.test|demo)\.sh$')"
# The sibling is named in snake_case: my-demo.test.sh tests my-demo.sh AND
# my_demo.py. A lint that looked for my-demo.py would find nothing and pass,
# which the dash-free `demo` fixtures above cannot tell apart.
d=$(mk_repo "$BOTH" "$GATED")
mv "$d/.github/scripts/tests/demo.test.sh" "$d/.github/scripts/tests/my-demo.test.sh"
mv "$d/.github/scripts/demo.sh" "$d/.github/scripts/my-demo.sh"
printf 'pass\n' > "$d/.github/scripts/my_demo.py"
sed -i.bak 's#/demo\.#/my-demo.#g' "$d/.github/workflows/ci.yml" && rm -f "$d/.github/workflows/ci.yml.bak"
check_msg 1 "my_demo.py matches no pattern" \
  "a dashed suite's sibling is looked up in snake_case" "$d"

echo ""
echo "Fail-open shapes the lint's own first revision passed (exit 1):"

# Step outputs are JOB-LOCAL. An `if:` naming an output the job never declares
# is permanently false, so the step never runs — and the first revision treated
# an unrecognised gate as "not filter-gated, nothing to check".
check_msg 1 "declares no filter named" \
  "a gate naming a filter output the job never declares" \
  "$(mk_repo "$BOTH" "$TYPO_GATE")"

# Substring matching cannot tell a mention from an invocation.
check_msg 1 "NO RUNNER" \
  "an echoed filename is not an invocation" \
  "$(mk_repo "$BOTH" '        run: echo "demo.test.sh is disabled for now"')"
check_msg 1 "NO RUNNER" \
  "a commented-out invocation is not an invocation" \
  "$(mk_repo "$BOTH" '        run: |
          # bash .github/scripts/tests/demo.test.sh
          echo skipped')"

# A pre-commit hook is gated by `files:` exactly as a workflow step is gated by
# a path filter, and recording the mention as an ungated runner ignored that.
check_msg 1 "the hook exists but never fires" \
  "a pre-commit hook whose files: excludes the suite is not a runner" \
  "$(mk_repo "$NEITHER" "$NO_RUN" 'precommit:^services/')"
# The variant WITHOUT an implementation script is the one that actually pins the
# suite-path branch. With `demo.sh` present, the impl-path check alone produces
# exit 1, so the case above passes whether or not the suite-path check runs —
# mutation-testing the hook fix showed it surviving for exactly that reason.
check_msg 1 "the hook exists but never fires" \
  "a pre-commit files: exclusion is caught with no impl script to fall back on" \
  "$(mk_repo "$NEITHER" "$NO_RUN" no-impl 'precommit:^services/')"
# The mirror of the case above: a hook reaching the SUITE but not the script the
# suite tests. Editing demo.sh alone would then run nothing. Isolating this needs
# its own fixture for the same reason — with a `files:` that misses both paths,
# the suite-path problem fires and the impl-path check could be dead code.
check_msg 1 "editing the script under test would not run" \
  "a pre-commit hook that reaches the suite but not its implementation" \
  "$(mk_repo "$NEITHER" "$NO_RUN" 'precommit:^\.github/scripts/tests/')"

echo ""
echo "Fail-open shapes found on the FIXED lint, cycle 5 (exit 1):"

# `bash` sits immediately before the path, so a preceding-token check called it
# the runner -- but the command is `echo` and `bash` is data being printed.
check_msg 1 "NO RUNNER" \
  "\`echo bash <suite>\` is not an invocation — echo is the command" \
  "$(mk_repo "$BOTH" "$ECHOED")"

# endswith("demo.test.sh") is also true of "not-demo.test.sh", so invoking one
# suite certified a different one. Nothing here invokes demo.test.sh at all.
check_msg 1 "NO RUNNER" \
  "invoking a similarly-named path does not certify this suite" \
  "$(mk_repo "$BOTH" "$OTHER_SUITE")"

# Distinct from the output-name typo already covered: the output NAME is right
# and only the step ID is wrong, so a name-only lookup found the right table.
check_msg 1 "no paths-filter step with id" \
  "a gate naming a step id that does not exist in the job" \
  "$(mk_repo "$BOTH" "$TYPO_STEP")"

# The inverse gate is the sharpest of these: every path is listed, so a
# coverage-only check is satisfied, yet the step is skipped on exactly the
# changes that require it.
check_msg 1 "is SKIPPED on exactly the changes" \
  "a gate tested as == 'false' skips on precisely the changed paths" \
  "$(mk_repo "$BOTH" "$INVERSE")"

# Job-level gating: the step carries no `if:` at all, so reading step conditions
# alone called it ungated.
check_msg 1 "matches no pattern in that filter" \
  "a job-level needs.<job>.outputs gate whose filter omits the suite" \
  "$(mk_joblevel "              - '.github/workflows/**'")"

check 0 "a job-level gate whose filter DOES cover both paths" \
  "$(mk_joblevel "              - '.github/scripts/tests/demo.test.sh'
              - '.github/scripts/demo.sh'")"

echo ""
echo "Fail-open shapes found on the FIXED lint, cycle 6 (exit 1):"

# A here-document BODY is data fed to a command, not commands the shell runs.
# `pr-ci.yml` embeds whole python programs this way, so a suite path merely
# PRINTED by one counted as a runner — the echo confusion, one quoting level down.
check_msg 1 "NO RUNNER" \
  "a suite path inside a heredoc BODY is not an invocation" \
  "$(mk_repo "$BOTH" '        run: |
          cat <<'"'"'EOF'"'"'
          bash .github/scripts/tests/demo.test.sh
          EOF')"

# ...and the opening line must still be read, or stripping the body would also
# blind the lint to a genuine invocation that happens to open a heredoc.
check 0 "a real invocation on a heredoc-opening line still counts" \
  "$(mk_repo "$BOTH" '        run: |
          bash .github/scripts/tests/demo.test.sh <<'"'"'EOF'"'"'
          some input
          EOF')"

echo ""
echo "Gate and runner shapes found on the FIXED lint, cycle 6b (exit 1):"

# `bash -n` READS the script without executing it, so the assertions never run.
# Distinct from the echoed-command case: bash really is the command here.
check_msg 1 "NO RUNNER" \
  "\`bash -n <suite>\` parses without executing, so it is not a runner" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: bash -n .github/scripts/tests/demo.test.sh')"

# Bundled short flags: `-en` carries -n just as `-n` does.
check_msg 1 "NO RUNNER" \
  "a bundled no-exec flag (\`bash -en\`) is caught too" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: bash -en .github/scripts/tests/demo.test.sh')"

# ...and an ordinary flag must NOT be mistaken for no-exec, or every hardened
# invocation in the repo would read as a non-runner.
# `./`-prefixed paths are normalised in is_suite_path. Nothing exercised it, so
# deleting the normalisation left the suite green — which is exactly how a stray
# mutation of those two lines sat unnoticed in the working tree during review.
check 0 "a \`./\`-prefixed invocation is still a runner" \
  "$(mk_repo "$NEITHER" '        run: bash ./.github/scripts/tests/demo.test.sh')"

check 0 "\`bash -euo pipefail <suite>\` is still a runner" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: bash -euo pipefail .github/scripts/tests/demo.test.sh')"

# fromJSON converts the dorny STRING to a real boolean, so it genuinely gates —
# the bare-reference branch discarded it and certified the suite unconditionally.
check_msg 1 "matches no pattern in that filter" \
  "a fromJSON-wrapped gate is a real gate, not a bare reference" \
  "$(mk_joblevel_gate "fromJSON(needs.changes.outputs.workflows)" \
     "              - '.github/workflows/**'")"

check 0 "a fromJSON gate whose filter DOES cover both paths" \
  "$(mk_joblevel_gate "fromJSON(needs.changes.outputs.workflows)" \
     "              - '.github/scripts/tests/demo.test.sh'
              - '.github/scripts/demo.sh'")"

# `bash -c <string>` runs the STRING; the next argument becomes $0, so a suite
# path there is a name, not a script. Verified against real bash: the command
# above prints only `skipped`.
check_msg 1 "NO RUNNER" \
  "\`bash -c '<cmd>' <suite>\` runs the string, not the suite" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: bash -c '"'"'echo skipped'"'"' .github/scripts/tests/demo.test.sh')"

# The over-fire guard for the char test: `-c` and `-n` disqualify, ordinary
# flags must not. `-euo` contains neither, and this is what stops the rule from
# declaring every hardened invocation in the repo a non-runner.
check 0 "a flag containing neither n nor c leaves the invocation intact" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: bash -euo pipefail .github/scripts/tests/demo.test.sh')"

echo ""
echo "Reachability shapes found on the FIXED lint, cycle 8 (exit 1):"

# Basename comparison collides in BOTH directions. The earlier fix stopped
# `not-demo.test.sh` certifying `demo.test.sh`; a different file with the SAME
# basename still did. Only the whole repo-relative path separates them.
check_msg 1 "NO RUNNER" \
  "a same-basename file in another directory does not certify the suite" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: bash .github/other/demo.test.sh' decoy)"

# A workflow whose own trigger paths exclude the suite is never STARTED for a
# change to it, so the job gate never gets a say.
#
# NOTE the plain single quotes below. These `on:` blocks are DOUBLE-quoted shell
# strings, where `'` is already literal — the `'"'"'` escape idiom belongs inside
# SINGLE-quoted strings and here it injects real double-quote characters, so the
# glob became `- '".github/**"'` and matched nothing. The exclusion case still
# exited 1 (a glob matching nothing excludes everything) and so passed for the
# wrong reason; only the admit case exposed it.
check_msg 1 "do not start a PULL-REQUEST run" \
  "a workflow whose on: paths exclude the suite cannot run it" \
  "$(mk_repo "$BOTH" '        run: bash .github/scripts/tests/demo.test.sh' \
     "on:on:
  pull_request:
    paths:
      - 'src/**'")"

check 0 "a workflow whose on: paths DO admit the suite is fine" \
  "$(mk_repo "$BOTH" '        run: bash .github/scripts/tests/demo.test.sh' \
     "on:on:
  pull_request:
    paths:
      - '.github/**'")"

# `if: false` parses as the YAML BOOLEAN false, and `str(v or "")` collapsed it
# to the empty string — so the one condition that matters most read as ungated.
check_msg 1 "statically false" \
  "a step gated \`if: false\` is not reachable" \
  "$(mk_repo "$BOTH" '        if: false
        run: bash .github/scripts/tests/demo.test.sh')"

check_msg 1 "statically false" \
  "a valid gate ANDed with false is not reachable" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"' && false
        run: bash .github/scripts/tests/demo.test.sh')"

echo ""
echo "Shapes found on the FIXED lint, cycle 9 (exit 1):"

# The inverse-gate bug re-entered through the branch added to fix it: the
# fromJSON regex consumed only the wrapper, so a trailing `== false` was unseen.
check_msg 1 "SKIPPED on exactly the changes" \
  "a fromJSON gate compared against false is not positive" \
  "$(mk_repo "$BOTH" '        if: fromJSON(steps.filter.outputs.workflows) == false
        run: bash .github/scripts/tests/demo.test.sh')"

# The ACCEPT side of ordered negation: a `!` that excludes something else must
# leave the suite covered. Without this, "any negation ⇒ uncovered" survives.
check 0 "a ! negation that does not name the suite leaves it covered" \
  "$(mk_repo "$BOTH" "$GATED" "on:on:
  pull_request:
    paths:
      - '.github/**'
      - '!.github/workflows/**'")"

# GitHub and dorny apply path patterns IN ORDER, so a later `!pattern` overrides
# an earlier match. Returning on the first positive match ignored the negation.
check_msg 1 "do not start a PULL-REQUEST run" \
  "an ordered ! negation in on: paths excludes the suite" \
  "$(mk_repo "$BOTH" '        run: bash .github/scripts/tests/demo.test.sh' \
     "on:on:
  pull_request:
    paths:
      - '.github/**'
      - '!.github/scripts/tests/**'")"

# pre-commit applies the CONFIG-level files/exclude to every hook.
check_msg 1 "never fires for the suite" \
  "a top-level pre-commit files: disables an otherwise ungated hook" \
  "$(mk_repo "$NEITHER" "$NO_RUN" no-impl 'pctop:^docs/')"

# `&&` short-circuits: the right-hand command after a false never runs.
check_msg 1 "NO RUNNER" \
  "\`false && bash <suite>\` is unreachable, not an invocation" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: false && bash .github/scripts/tests/demo.test.sh')"

# A suite whose red is discarded is as silent as one that never runs — the same
# failure this lint exists for, one step further along.
check_msg 1 "cannot fail the build" \
  "a continue-on-error step does not enforce its suite" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        continue-on-error: true
        run: bash .github/scripts/tests/demo.test.sh')"

check_msg 1 "cannot fail the build" \
  "an invocation ending in \`|| true\` does not enforce its suite" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: bash .github/scripts/tests/demo.test.sh || true')"

# The over-fire controls: `&&` that is NOT short-circuited, and a `||` fallback
# that is not a failure-swallow, must both still count.
check 0 "a chained \`cd x && bash <suite>\` is still a runner" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: echo start && bash .github/scripts/tests/demo.test.sh')"

echo ""
echo "Shapes found on the FIXED lint, cycle 10 (exit 1):"

# `bash -s` reads commands from STDIN; the path is only a positional parameter.
# Verified against bash 5.2 directly: the suite prints nothing.
check_msg 1 "NO RUNNER" \
  "\`bash -s <suite>\` reads stdin and never executes the suite" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: bash -s .github/scripts/tests/demo.test.sh </dev/null')"

# `||` short-circuits when the LEFT side SUCCEEDS — the mirror of `false &&`.
check_msg 1 "NO RUNNER" \
  "\`true || bash <suite>\` never reaches the suite" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: true || bash .github/scripts/tests/demo.test.sh')"

# Enumerating SUCCESSFUL fallbacks is unbounded, so the rule fails closed on any
# handler that does not provably propagate.
check_msg 1 "cannot fail the build" \
  "\`|| echo ignored\` converts the suite red into a green step" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: bash .github/scripts/tests/demo.test.sh || echo ignored')"

# The over-fire control: a handler that DOES propagate must stay a valid runner,
# or the fail-closed default would reject correct wiring.
check 0 "\`|| exit 1\` propagates, so the invocation still enforces" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: bash .github/scripts/tests/demo.test.sh || exit 1')"

# `exit` and `return` propagate only the status they CARRY. Matching the bare
# keyword accepted `|| exit 0`, which is a swallow wearing the shape of the fix.
check_msg 1 "cannot fail the build" \
  "\`|| exit 0\` names a propagator but discards the status" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: bash .github/scripts/tests/demo.test.sh || exit 0')"

# Over-fire control for the SAME rule, and the reason it is not "a nonzero
# literal": `|| exit $rc` is a live idiom in .pre-commit-config.yaml, so a rule
# demanding a literal would red the real tree. Only `0` is proof of zero.
check 0 "\`|| exit \$rc\` propagates an unknown status, so it still enforces" \
  "$(mk_repo "$NEITHER" '        run: bash .github/scripts/tests/demo.test.sh || exit \$rc')"

echo ""
echo "Enforcement — the status never reaches the shell:"

# Bash suppresses errexit for a command used as an `if` condition, and an `if`
# whose branch does not run still exits 0. The suite runs and its red vanishes.
check_msg 1 "cannot fail the build" \
  "a suite used as an \`if\` condition with no propagating branch" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: if bash .github/scripts/tests/demo.test.sh; then echo passed; fi')"

# Over-fire control: the same conditional shape IS enforcing when the branch
# propagates, which is the idiomatic way to add a message to a failure.
check 0 "an \`if !\` whose branch exits nonzero still enforces" \
  "$(mk_repo "$NEITHER" '        run: if ! bash .github/scripts/tests/demo.test.sh; then exit 1; fi')"

# A backgrounded invocation returns the async list status, which is 0 whatever
# the suite eventually does.
check_msg 1 "cannot fail the build" \
  "a backgrounded suite invocation cannot fail the step" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: bash .github/scripts/tests/demo.test.sh &')"

# Over-fire control: `&&` must not be read as a background operator.
check 0 "\`&& echo ok\` is not a backgrounded invocation" \
  "$(mk_repo "$NEITHER" '        run: bash .github/scripts/tests/demo.test.sh && echo ok')"

echo ""
echo "Triggers — only a pull-request event gates a pull request:"

# An unrestricted `push:` masked a `pull_request.paths` that excluded the suite:
# the workflow ran, but only AFTER the merge it should have gated.
check_msg 1 "PULL-REQUEST run" \
  "an unrestricted push does not excuse a pull_request path exclusion" \
  "$(mk_repo "$BOTH" "$GATED" 'on:on:
  push:
  pull_request:
    paths:
      - src/**')"

# The same defect with nothing to mask it: a push-only workflow never gates a PR.
check_msg 1 "PULL-REQUEST run" \
  "a push-only workflow gates nothing on a pull request" \
  "$(mk_repo "$BOTH" "$GATED" 'on:on:
  push:')"

# Over-fire control: a pull_request path filter that DOES admit the suite passes,
# and the coexisting push trigger is irrelevant either way.
check 0 "a pull_request filter covering the suite admits it" \
  "$(mk_repo "$BOTH" "$GATED" 'on:on:
  push:
  pull_request:
    paths:
      - .github/**')"

# `on: push` and `on: [push]` are the same workflow as the mapping form with an
# empty body. Returning early on "carries no path filter" answered the wrong
# question once the rule became "which EVENT is this".
check_msg 1 "do not start a PULL-REQUEST run" \
  "a scalar \`on: push\` is still push-only" \
  "$(mk_repo "$BOTH" "$GATED" 'on:on: push')"

check_msg 1 "do not start a PULL-REQUEST run" \
  "a list \`on: [push]\` is still push-only" \
  "$(mk_repo "$BOTH" "$GATED" 'on:on: [push]')"

# Over-fire control: the list form carrying a PR event must still admit.
check 0 "a list naming pull_request admits the suite" \
  "$(mk_repo "$BOTH" "$GATED" 'on:on: [push, pull_request]')"

echo ""
echo "Enforcement — the status is inverted or reparented:"

# `!` inverts the status: an exit-7 suite becomes a passing command. It is a
# keyword invokes() steps over, so this is purely an enforcement question.
check_msg 1 "cannot fail the build" \
  "\`! bash <suite>\` inverts the suite red into a green step" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: |
          ! bash .github/scripts/tests/demo.test.sh')"

# A backslash continuation makes the next physical line ARGUMENTS to the command
# above it, so `echo \` + `bash <suite>` never executes the suite. The per-line
# loop promoted it to a command of its own.
check_msg 1 "NO RUNNER" \
  "a continued \`echo\` swallows the suite as an argument" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: |
          echo \\
          bash .github/scripts/tests/demo.test.sh')"

# Over-fire control: a continuation is how a LONG invocation is written, and
# joining must leave it a runner.
check 0 "a continued \`bash\` invocation is still a runner" \
  "$(mk_repo "$NEITHER" '        run: |
          bash \\
            .github/scripts/tests/demo.test.sh')"

echo ""
echo "Invocation — the script-file operand:"

# `bash [option]... script-file [arg]...`: everything after the script file is a
# POSITIONAL PARAMETER, so this runs the wrapper and hands it the suite as "$1".
check_msg 1 "NO RUNNER" \
  "a suite passed as an argument to another script is not invoked" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: bash .github/scripts/wrapper.sh .github/scripts/tests/demo.test.sh')"

# A DEFINITION stores the command; nothing runs until something calls it.
check_msg 1 "NO RUNNER" \
  "a suite inside a function nothing calls is not invoked" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: |
          disabled() { bash .github/scripts/tests/demo.test.sh; }
          echo defined but never called')"

# Over-fire control: the same body IS reached once the function is called, and
# reachability is a fixpoint so a call from another called function counts too.
check 0 "a suite inside a called function is invoked" \
  "$(mk_repo "$NEITHER" '        run: |
          inner() { bash .github/scripts/tests/demo.test.sh; }
          outer() { inner; }
          outer')"

echo ""
echo "Reachability — the filter PRODUCER must run too:"

# A paths-filter step behind `if: false` publishes no outputs, so every consumer
# gated on `steps.filter.outputs.*` is skipped. Recording its declared filters
# made the consumer's gate look satisfiable.
check_msg 1 "publishes no outputs" \
  "a paths-filter producer behind \`if: false\` skips every consumer" \
  "$(mk_repo "$BOTH" "$GATED" 'pfif:false')"

echo ""
echo "Reachability — a conjunct outside the filter reference:"

# `gates_in()` sees only the filter reference, so the rest of the condition was
# dropped and a step that runs only after merge read as a pull-request gate.
check_msg 1 "non-pull-request event" \
  "an event conjunct pinning the step to push gates no pull request" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"' && github.event_name == '"'"'push'"'"'
        run: bash .github/scripts/tests/demo.test.sh')"

# Over-fire control: the same shape naming a PR event is a correct gate.
check 0 "an event conjunct naming pull_request is still a valid gate" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"' && github.event_name == '"'"'pull_request'"'"'
        run: bash .github/scripts/tests/demo.test.sh')"

echo ""
echo "Enforcement — the shell options that make a bare invocation count:"

# The default step shell is `bash -eo pipefail`; `set +e` removes the errexit
# that makes a bare invocation enforcing at all.
check_msg 1 "cannot fail the build" \
  "\`set +e\` before the invocation discards the suite status" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: |
          set +e
          bash .github/scripts/tests/demo.test.sh
          echo ignored')"

# The DISABLE side must accept the same option grammar as the enable side. It
# did not: `\b` fails on `+eu`, so `set +eu` matched neither recogniser and a
# suite whose errexit was off was certified. Bundled forms, both polarities.
check_msg 1 "cannot fail the build" \
  "bundled \`set +eu\` disables errexit just as \`set +e\` does" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: |
          set +eu
          bash .github/scripts/tests/demo.test.sh
          echo ignored')"

# Over-fire control: `set +x` changes tracing, not errexit, and must NOT disable.
check 0 "\`set +x\` does not disable errexit" \
  "$(mk_repo "$NEITHER" '        run: |
          set +x
          bash .github/scripts/tests/demo.test.sh')"

# Over-fire control: the LAST `set` wins, so re-enabling errexit restores it.
check 0 "\`set -e\` after a \`set +e\` restores enforcement" \
  "$(mk_repo "$NEITHER" '        run: |
          set +e
          echo something unrelated
          set -e
          bash .github/scripts/tests/demo.test.sh')"

# `shell: sh` is `sh -e {0}` with no pipefail, so a pipeline reports only its
# LAST command and the suite red never leaves the pipe.
check_msg 1 "cannot fail the build" \
  "a pipeline under \`shell: sh\` loses the suite status" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        shell: sh
        run: bash .github/scripts/tests/demo.test.sh | cat')"

# Over-fire control: the DEFAULT shell carries `-o pipefail`, so the same
# pipeline is enforcing there.
check 0 "the same pipeline under the default pipefail shell still enforces" \
  "$(mk_repo "$NEITHER" '        run: bash .github/scripts/tests/demo.test.sh | cat')"

echo ""
echo "Triggers — an activity type that never sees the code:"

# `types: [closed]` starts the workflow only once the PR is no longer under
# review, so it gates nothing. The trigger has no path filter, which is why the
# earlier check waved it through.
check_msg 1 "do not start a PULL-REQUEST run" \
  "\`pull_request: types: [closed]\` gates no code change" \
  "$(mk_repo "$BOTH" "$GATED" 'on:on:
  pull_request:
    types: [closed]')"

# Over-fire control: the default-ish set still admits.
check 0 "\`types: [opened, synchronize]\` still admits the suite" \
  "$(mk_repo "$BOTH" "$GATED" 'on:on:
  pull_request:
    types: [opened, synchronize]')"

echo ""
echo "Enforcement — the shell, the exit, and the AND-list:"

# A custom template such as `shell: bash {0}` starts bash WITHOUT `-e`, so a
# bare invocation is not enforcing. Unrecognised shells fail closed.
check_msg 1 "cannot fail the build" \
  "a custom \`shell:\` template is not known to carry errexit" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        shell: bash {0}
        run: |
          bash .github/scripts/tests/demo.test.sh
          echo ignored')"

# Over-fire control: an explicit `set -e` in the body restores it.
check 0 "an explicit \`set -e\` rescues a custom shell template" \
  "$(mk_repo "$NEITHER" '        shell: bash {0}
        run: |
          set -e
          bash .github/scripts/tests/demo.test.sh')"

# Nothing after an unconditional `exit` runs.
check_msg 1 "NO RUNNER" \
  "a line after an unconditional \`exit\` never executes" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: |
          echo done
          exit 0
          bash .github/scripts/tests/demo.test.sh')"

# Over-fire control: a CONDITIONAL exit does not end the block.
check 0 "a conditional \`|| exit 1\` does not kill the rest of the block" \
  "$(mk_repo "$NEITHER" '        run: |
          echo checking || exit 1
          bash .github/scripts/tests/demo.test.sh')"

# `--version` prints and exits without running a script, exactly like `-n`.
check_msg 1 "NO RUNNER" \
  "\`bash --version <suite>\` prints and exits without running it" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: bash --version .github/scripts/tests/demo.test.sh')"

# errexit exempts the LEFT of an AND-list, so a failing suite only skips the
# right side and the block carries on. Verified against bash: alone the step
# exits 7, with any command after it the step exits 0 -- which is why the
# existing `&& echo ok` case (the whole block) stays a valid runner.
check_msg 1 "cannot fail the build" \
  "a suite on the left of \`&&\` with a command after it" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: |
          bash .github/scripts/tests/demo.test.sh && echo passed
          echo later')"

echo ""
echo "The last four fail-open holes:"

# dorny emits the non-empty strings 'true'/'false', both truthy -- but a MISSING
# output is the empty string, which is false. Dropping bare references conflated
# "always true" with "always false".
check_msg 1 "declares no filter named" \
  "a bare reference to an UNDECLARED output is always false" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.typo
        run: bash .github/scripts/tests/demo.test.sh')"

# Over-fire control, and the case that makes `bare` load-bearing: dorny sets a
# DECLARED output to the string 'false' when nothing matched, which is still
# truthy, so the step runs whatever the filter lists. The filter here lists
# NEITHER path on purpose -- pattern-matching a bare gate would red a step
# GitHub always runs.
check 0 "a bare reference to a declared output ignores the filter patterns" \
  "$(mk_repo "$NEITHER" '        if: steps.filter.outputs.workflows
        run: bash .github/scripts/tests/demo.test.sh')"

# An `exit 1` inside `if false; then ... fi` is present and unreachable, and the
# compound returns 0. A handler holding any conditional is not proof.
check_msg 1 "cannot fail the build" \
  "a conditional handler does not prove propagation" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: bash .github/scripts/tests/demo.test.sh || if false; then exit 1; fi')"

# `set +o pipefail` under the DEFAULT bash shell -- the shell type was treated as
# the whole answer, so the twin of the errexit scan was missing.
check_msg 1 "cannot fail the build" \
  "\`set +o pipefail\` disables it even under the default shell" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: |
          set +o pipefail
          bash .github/scripts/tests/demo.test.sh | cat')"

# Over-fire control: restoring it re-enables enforcement.
check 0 "\`set -o pipefail\` after disabling it restores enforcement" \
  "$(mk_repo "$NEITHER" '        run: |
          set +o pipefail
          set -o pipefail
          bash .github/scripts/tests/demo.test.sh | cat')"

# A producer pinned to a non-PR event publishes nothing during a PR run, so every
# consumer of its outputs is skipped -- the same check the CONSUMER already had.
check_msg 1 "publishes no outputs" \
  "a paths-filter producer pinned to push skips every consumer" \
  "$(mk_repo "$BOTH" "$GATED" "pfif:github.event_name == 'push'")"

echo ""
echo "State read at the WRONG point, and defaults read from the WRONG level:"

# pipefail must be read AT the invocation. Scanning the whole block let a later
# re-enable cover a suite that ran while the option was off.
check_msg 1 "cannot fail the build" \
  "pipefail disabled AT the invocation is not rescued by a later re-enable" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: |
          set +o pipefail
          bash .github/scripts/tests/demo.test.sh | cat
          set -o pipefail')"

# The shell fallback chain is step, then job default, then WORKFLOW default. The
# last one was missing, so a root `bash {0}` resolved to None and read as safe.
# Passed through the on-block, which mk_repo inserts verbatim.
check_msg 1 "cannot fail the build" \
  "a workflow-level \`defaults.run.shell\` is honoured" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"'
        run: |
          bash .github/scripts/tests/demo.test.sh
          echo ignored' 'on:on: [pull_request]
defaults:
  run:
    shell: bash {0}')"

echo ""
echo "Triggers — the proposed revision, and every revision:"

# `pull_request_target` runs at the BASE commit, so a suite riding it tests the
# OLD copy of itself. This repo documents that in .github/workflows/labeler.yml.
check_msg 1 "do not start a PULL-REQUEST run" \
  "\`pull_request_target\` runs base code, so it does not test the change" \
  "$(mk_repo "$BOTH" "$GATED" 'on:on: [pull_request_target]')"

# `types: [opened]` runs for the PR's first revision and no later push.
check_msg 1 "do not start a PULL-REQUEST run" \
  "\`types: [opened]\` misses every later push" \
  "$(mk_repo "$BOTH" "$GATED" 'on:on:
  pull_request:
    types: [opened]')"

# ... and synchronize-only misses the revision the PR opened with.
check_msg 1 "do not start a PULL-REQUEST run" \
  "\`types: [synchronize]\` misses the opening revision" \
  "$(mk_repo "$BOTH" "$GATED" 'on:on:
  pull_request:
    types: [synchronize]')"

echo ""
echo "Conditions — a disjunction can exclude pull requests too:"

# Every branch decidable and every branch non-PR: the step never runs on a PR.
check_msg 1 "non-pull-request event" \
  "a disjunction whose every branch is a non-PR event excludes PRs" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"' && (github.event_name == '"'"'push'"'"' || github.event_name == '"'"'schedule'"'"')
        run: bash .github/scripts/tests/demo.test.sh')"

# Over-fire control: one UNDECIDABLE branch and the disjunction may still admit.
check 0 "a disjunction with an undecidable branch stays admissible" \
  "$(mk_repo "$BOTH" '        if: steps.filter.outputs.workflows == '"'"'true'"'"' && (github.event_name == '"'"'push'"'"' || inputs.force)
        run: bash .github/scripts/tests/demo.test.sh')"

echo ""
echo "Workflow file extensions:"

# GitHub Actions accepts .yaml as readily as .yml, and .pre-commit-config.yaml
# already gates on `\.ya?ml$`. A .yaml workflow was invisible to the scan, so a
# suite invoked only from one reported NO RUNNER.
check 0 "a .yaml workflow is scanned for runners" \
  "$(mk_repo "$NEITHER" "$UNGATED" ext:yaml)"

echo ""
echo "Vacuity and interface:"

# A guard that reports success over zero inputs has verified nothing.
d="$TMP/empty"; mkdir -p "$d/.github/scripts/tests" "$d/.github/workflows"
check_msg 2 "refusing to pass vacuously" "an empty tests directory exits 2, not 0" "$d"

d="$TMP/absent"; mkdir -p "$d"
check_msg 2 "tests" "a tree with no tests directory exits 2" "$d"

set +e
HELP=$(bash "$LINT" --help 2>&1); hrc=$?
set -e
if [[ "$hrc" == "0" ]]; then echo "  PASS: --help exits 0"; pass=$((pass + 1))
else echo "  FAIL: --help exits 0 (got $hrc)" >&2; fail=$((fail + 1)); fi

# Asserting only the EXIT CODE let `usage() { :; }` survive: --help is the only
# delivery vehicle for the limits document, and an empty one still exits 0.
# These pin the load-bearing structure, not prose that will be reworded.
for want in "INVOKED by something" "Fail-CLOSED gaps" "Fail-OPEN gaps"             "EVIDENCE FIRST" "pull_request_target" "Exits:"; do
  if grep -qF -- "$want" < <(printf '%s\n' "$HELP"); then
    echo "  PASS: --help documents \"$want\""; pass=$((pass + 1))
  else
    echo "  FAIL: --help is missing \"$want\" — the limits section is the whole point of it" >&2
    fail=$((fail + 1))
  fi
done

# ...and it must not spill the implementation below the header block.
if grep -qF -- "set -euo pipefail" < <(printf '%s\n' "$HELP"); then
  echo "  FAIL: --help leaked past the header into the script body" >&2; fail=$((fail + 1))
else
  echo "  PASS: --help stops at the header"; pass=$((pass + 1))
fi

# Every fixture must have gone to its OWN directory. With the counter bug each
# call returned the same path, so this is the assertion that fails if anyone
# reintroduces it.
#
# Compared against the number of BUILDER CALLS rather than a hand-written total:
# the property is "no two fixtures share a tree", which a literal only expresses
# by coincidence, and which then goes stale the moment a case is added. It did,
# on the very next commit.
calls=$(wc -l < "$TMP/builder-calls" | tr -d ' ')
built=$(find "$TMP" -maxdepth 1 -type d -name 'case-*' | wc -l | tr -d ' ')
if [[ "$built" == "$calls" ]]; then
  echo "  PASS: each fixture built its own tree ($built trees / $calls calls)"; pass=$((pass + 1))
else
  echo "  FAIL: $calls fixture builds produced only $built distinct trees" >&2; fail=$((fail + 1))
fi

# The guard must satisfy its own rule against the real repository — the one
# assertion here that reads live state rather than a fixture.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
set +e
bash "$LINT" "$REPO_ROOT" >/dev/null 2>&1; rrc=$?
set -e
if [[ "$rrc" == "0" ]]; then echo "  PASS: the live repository satisfies the rule"; pass=$((pass + 1))
else echo "  FAIL: the live repository violates the rule (exit $rrc)" >&2; fail=$((fail + 1)); fi

echo ""
echo "─────────────────────────────────────────"
echo "PASS: $pass  FAIL: $fail"
[[ "$fail" == "0" ]]
