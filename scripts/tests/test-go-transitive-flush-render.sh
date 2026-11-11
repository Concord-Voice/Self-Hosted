#!/usr/bin/env bash
#
# Regression test for the "Render PR body" step of
# .github/workflows/go-transitive-flush.yml.
#
# Why this exists: that step is shell embedded in workflow YAML, so nothing ever
# executed it — CI runs the workflow only on the weekly cron, and a failure there
# manifests as "no PR was opened this week", which looks identical to "there was
# nothing to update". PR #2760 shipped a version that died under
# `set -euo pipefail` whenever `go mod tidy` removed modules without adding any
# (a grep matching nothing exits 1 → pipefail → set -e → step aborts before the
# body is written). Caught by CodeRabbit on that PR; this test locks it closed.
#
# The test EXTRACTS the real `run:` block from the workflow rather than
# reimplementing it, so it cannot drift away from what CI actually executes.

set -uo pipefail

# first_line: `| head -1` under `set -o pipefail` is a RACE, not a shorthand. head
# exits after the first line and closes the pipe; if the producer has not already
# finished, its next write takes SIGPIPE, the pipeline reports 141, and `set -e`
# kills the script silently with every later statement unreached. Whether it fires
# depends on producer buffering, so it passes on one machine and fails on another.
# first_line DRAINS its input, so there is nothing to race. Prose ABOUT the defect
# deliberately keeps saying `head -1`. Reference: #3105, commit 3f7144952 in
# infrastructure/deploy/tests/test-rtc-port-ingress-parity.sh.
first_line() {
  local l first=
  while IFS= read -r l || [[ -n "$l" ]]; do
    [[ -n "$first" ]] || { first=1; printf '%s\n' "$l"; }
  done
}

# Git exports GIT_DIR / GIT_INDEX_FILE / GIT_WORK_TREE (and friends) into the
# environment of every hook it runs. Left set, the throwaway fixture repos below
# would resolve to the PARENT repository instead of themselves, and every `git`
# call would fail with exit 128 — so this test passes when run by hand and dies
# under pre-commit. Scrub them; nothing here needs the surrounding repo, since
# REPO_ROOT is derived from BASH_SOURCE rather than from git.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY \
      GIT_COMMON_DIR GIT_PREFIX GIT_CONFIG_PARAMETERS 2>/dev/null || true

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORKFLOW="$REPO_ROOT/.github/workflows/go-transitive-flush.yml"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0

ok()   { echo "ok:   $1"; pass=$((pass + 1)); }
bad()  { echo "FAIL: $1"; [ -n "${2:-}" ] && printf '    %s\n' "${2//$'\n'/$'\n'    }"; fail=$((fail + 1)); }

# ── extract the step's shell verbatim ───────────────────────────────────────
# Deliberately awk rather than PyYAML. Every hook in .pre-commit-config.yaml is
# `language: system`, so `import yaml` here would be an UNDECLARED dependency
# that aborts the hook on any host without PyYAML — the repo pip-installs it
# only inside pr-ci.yml. Extracting still beats reimplementing: the whole point
# is that this test executes the same shell CI does, so it cannot drift.
{
  printf '#!/usr/bin/env bash\n'
  awk '
    /^      - name: Render PR body[[:space:]]*$/ { instep = 1; next }
    instep && /^        run: \|[[:space:]]*$/    { inrun = 1; next }
    inrun {
      if ($0 == "")            { print ""; next }        # blank line inside the block
      if ($0 ~ /^          /)  { sub(/^          /, ""); print; next }
      exit                                                # dedent => end of run:
    }
  ' "$WORKFLOW"
} > "$WORK/render.sh"

[ -s "$WORK/render.sh" ] || { echo "FAIL: could not extract the render step"; exit 1; }
grep -q 'pr-body.md' "$WORK/render.sh" \
  || { echo "FAIL: extraction produced no heredoc — the step's YAML shape changed"; exit 1; }
ok "extracted the 'Render PR body' step from the workflow (no third-party deps)"

bash -n "$WORK/render.sh" 2>"$WORK/syntax.err" \
  && ok "render step is syntactically valid bash" \
  || bad "render step has a bash syntax error" "$(cat "$WORK/syntax.err")"

# ── harness: build a repo whose go.mod diff has a given shape, then render ───
# $1 = case name, $2 = base go.mod body, $3 = new go.mod body,
# $4..$6 = expected bumped/new/orphaned counts
render_case() {
  local name="$1" before="$2" after="$3" want_bump="$4" want_new="$5" want_orph="$6"
  local dir="$WORK/case-$RANDOM"
  mkdir -p "$dir/services/control-plane"
  ( cd "$dir" && git init -q . && git config user.email t@t.t && git config user.name t ) || true
  printf '%s' "$before" > "$dir/services/control-plane/go.mod"
  ( cd "$dir" && git add -A && git commit -qm base ) || true
  printf '%s' "$after" > "$dir/services/control-plane/go.mod"

  local out rc
  out=$( cd "$dir" && TRIGGER=schedule bash "$WORK/render.sh" 2>&1 ); rc=$?

  if [ "$rc" -ne 0 ]; then
    bad "$name — render step exited $rc (should always reach the body write)" "$out"
    return
  fi
  if [ ! -s /tmp/pr-body.md ]; then
    bad "$name — no PR body was written"
    return
  fi
  local body; body=$(cat /tmp/pr-body.md)
  local got
  got=$(printf '%s' "$body" | grep -oE '\*\*[0-9]+ module\(s\) bumped, [0-9]+ added, [0-9]+ orphaned module\(s\) removed\*\*' | first_line)
  local want="**${want_bump} module(s) bumped, ${want_new} added, ${want_orph} orphaned module(s) removed**"
  if [ "$got" = "$want" ]; then
    ok "$name — counts render as ${want_bump}/${want_new}/${want_orph}"
  else
    bad "$name — wrong counts" "want: $want"$'\n'"got:  ${got:-no count line found}"
  fi
}

BASE_TWO=$'module x\n\ngo 1.26.6\n\nrequire (\n\tgithub.com/foo/bar v1.0.0\n\tgithub.com/orphan/mod v2.0.0\n)\n'

# The founding regression: tidy removes an orphan and adds nothing, so the
# '+' grep matches zero lines. Pre-fix this exited 1 and no PR was ever opened.
render_case "orphan-removal only (the #2760 regression)" \
  "$BASE_TWO" \
  $'module x\n\ngo 1.26.6\n\nrequire (\n\tgithub.com/foo/bar v1.0.0\n)\n' \
  0 0 1

# Mirror case: a module is added and nothing removed, so the '-' grep is empty.
render_case "addition only" \
  $'module x\n\ngo 1.26.6\n\nrequire (\n\tgithub.com/foo/bar v1.0.0\n)\n' \
  $'module x\n\ngo 1.26.6\n\nrequire (\n\tgithub.com/foo/bar v1.0.0\n\tgithub.com/new/dep v1.2.3\n)\n' \
  0 1 0

# The ordinary weekly shape: paired version bumps, nothing added or orphaned.
render_case "paired version bumps" \
  "$BASE_TWO" \
  $'module x\n\ngo 1.26.6\n\nrequire (\n\tgithub.com/foo/bar v1.1.0\n\tgithub.com/orphan/mod v2.1.0\n)\n' \
  2 0 0

# Mixed: one bump, one removal, one addition.
render_case "mixed bump + orphan + addition" \
  "$BASE_TWO" \
  $'module x\n\ngo 1.26.6\n\nrequire (\n\tgithub.com/foo/bar v1.1.0\n\tgithub.com/new/dep v0.1.0\n)\n' \
  1 1 1

# SINGLE-LINE require form. go.mod supports `require <mod> <ver>` without a
# block, and gofmt leaves a lone requirement in that shape. Keying the parser on
# whitespace-after-the-diff-marker matched only the indented block entry, so a
# module graph in this form scored 0/0/0 — every count silently wrong rather
# than the step failing loudly.
render_case "single-line require directive" \
  $'module x\n\ngo 1.26.6\n\nrequire github.com/foo/bar v1.0.0\n' \
  $'module x\n\ngo 1.26.6\n\nrequire github.com/foo/bar v1.1.0\n' \
  1 0 0

# Non-requirement lines must never be counted as modules: `go`/`toolchain`
# directives and the `require (` / `)` block delimiters all move in real diffs.
render_case "directive churn is not counted as modules" \
  $'module x\n\ngo 1.26.5\n\nrequire (\n\tgithub.com/foo/bar v1.0.0\n)\n' \
  $'module x\n\ngo 1.26.6\n\ntoolchain go1.26.6\n\nrequire (\n\tgithub.com/foo/bar v1.0.0\n)\n' \
  0 0 0

# ── the body must NOT tell the reviewer to re-trigger CI ─────────────────────
# The PR is opened with an App installation token, so `pull_request` workflows
# start on their own. A leftover re-trigger instruction would send the reviewer
# to close and reopen a PR whose checks are already running.
for stale in "Close and reopen this PR" "GITHUB_TOKEN"; do
  if grep -qF -- "$stale" /tmp/pr-body.md; then
    bad "body still contains '$stale'"
  else
    ok "body does not contain '$stale'"
  fi
done

# A bare issue reference in a PR body autolinks to THIS repository's issue of
# that number, whatever the author meant. The body is generated, so it must
# carry none.
if grep -qE '(^|[^[:alnum:]&/])#[0-9]+' /tmp/pr-body.md; then
  bad "body contains a bare issue reference" "$(grep -nE '(^|[^[:alnum:]&/])#[0-9]+' /tmp/pr-body.md)"
else
  ok "body contains no bare issue reference"
fi

# ── the body must render a REAL stamp, not the hardcoded cron literals ───────
# The guard used to be `grep -qE 'on Tuesday|at 06:00 UTC'`, which was wrong in a
# way that only showed on one day a week: the cron IS Tuesday 06:00 UTC, so a
# CORRECT scheduled run renders "on Tuesday <date> at 06:00 UTC" and matched both
# halves of its own forbidden pattern. The test therefore failed every Tuesday and
# passed the other six days — and because `.pre-commit-config.yaml` is in this
# hook's `files:` filter, it blocked unrelated Tuesday commits.
#
# What actually distinguishes a rendered stamp from the #2760 hardcode is the
# presence of a real ISO date. Assert that instead of banning a weekday name.
if grep -qE '[0-9]{4}-[0-9]{2}-[0-9]{2}' /tmp/pr-body.md; then
  ok "body renders a real generation stamp (ISO date present)"
else
  bad "body has no real date — the generation stamp is hardcoded again"
fi

# ── static: no write credential shares a job with the module refresh ────────
# A job receives every secret it references, and its GITHUB_TOKEN, when the job
# STARTS, whatever the step order and whatever `if:` a step carries. Minting
# late inside one job would therefore still put the App private key on the
# runner while `go get -u` downloads and resolves third-party modules. The lock
# is structural, in three jobs:
#   guard    the only GITHUB_TOKEN write scope (pull-requests), no Go tooling
#   refresh  fetches modules; read-only token, no secret of any name
#   open-pr  holds the App key; runs no Go tooling
job_block() { # $1 = job id; prints that job's lines with comment lines dropped
  awk -v j="  $1:" '
    $0 == j                  { p = 1; next }
    p && /^  [^ #]/          { exit }
    p && !/^[[:space:]]*#/   { print }
  ' "$WORKFLOW"
}
code_lines() { grep -v '^[[:space:]]*#' "$WORKFLOW"; }
code_count() { code_lines | grep -cF -- "$1" || true; }
GO_TOOLING='setup-go|(^|[^[:alnum:]_-])go [a-z]+'

GUARD_JOB=$(job_block guard)
REFRESH_JOB=$(job_block refresh)
OPEN_JOB=$(job_block open-pr)

# A fourth job would be outside every per-job assertion below.
job_ids=$(awk '/^jobs:/ { p = 1; next } p && /^  [^ #]/ { sub(/:.*/, ""); sub(/^  /, ""); printf "%s ", $0 }' "$WORKFLOW")
if [ "$job_ids" = "guard refresh open-pr " ]; then
  ok "the workflow has exactly the jobs guard, refresh and open-pr"
else
  bad "the job set changed; every job needs a credential assertion here" "jobs: $job_ids"
fi

# `secrets` with no dot also catches `secrets: inherit` and `toJSON(secrets)`.
if grep -qF 'go get -u' <<<"$REFRESH_JOB" \
   && ! grep -qE 'secrets|github\.token|PUBLIC_MIRROR_APP_|create-github-app-token|create-pull-request|write' <<<"$REFRESH_JOB"; then
  ok "the refresh job fetches modules and holds no secret and no write scope"
else
  bad "the refresh job is missing, or references a secret, a token or a write scope"
fi

if grep -qE '^    needs: guard$' <<<"$REFRESH_JOB" \
   && grep -qxF "    if: needs.guard.outputs.deferred != 'true'" <<<"$REFRESH_JOB" \
   && [ -n "$GUARD_JOB" ] \
   && ! grep -qE "$GO_TOOLING|PUBLIC_MIRROR_APP_|create-github-app-token" <<<"$GUARD_JOB"; then
  ok "the guard job runs first, with no Go tooling and no App credential"
else
  bad "the guard job is missing, is not a gate on refresh, or does more than query pull requests"
fi

# Whole-line match on the `if:`, so `… == 'true' || always()` does not pass.
# shellcheck disable=SC2016  # literal GitHub expression, not a shell expansion
if grep -qF 'has_changes: ${{ steps.refresh.outputs.has_changes }}' <<<"$REFRESH_JOB" \
   && grep -qF 'echo "has_changes=true" >> "$GITHUB_OUTPUT"' <<<"$REFRESH_JOB" \
   && grep -qE '^    needs: refresh$' <<<"$OPEN_JOB" \
   && grep -qxF "    if: needs.refresh.outputs.has_changes == 'true'" <<<"$OPEN_JOB"; then
  ok "the open-pr job runs only when the refresh job reports a change"
else
  bad "the open-pr job is missing, or is not gated on the refresh job's has_changes output"
fi

if [ -n "$OPEN_JOB" ] && ! grep -qE "$GO_TOOLING" <<<"$OPEN_JOB"; then
  ok "the open-pr job runs no Go tooling"
else
  bad "the job that holds the App key must not run Go tooling"
fi

# The payload crosses the job boundary as an artifact: same name and directory
# on both sides, module files copied (never executed), body read in place.
# shellcheck disable=SC2016  # literal GitHub expressions, not shell expansions
if [ "$(grep -cxF '          name: go-transitive-flush' <<<"$REFRESH_JOB")" -eq 1 ] \
   && [ "$(grep -cxF '          name: go-transitive-flush' <<<"$OPEN_JOB")" -eq 1 ] \
   && [ "$(grep -cxF '          path: ${{ runner.temp }}/flush' <<<"$REFRESH_JOB")" -eq 1 ] \
   && [ "$(grep -cxF '          path: ${{ runner.temp }}/flush' <<<"$OPEN_JOB")" -eq 1 ] \
   && grep -qF 'cp "$RUNNER_TEMP/flush/go.mod" "$RUNNER_TEMP/flush/go.sum" services/control-plane/' <<<"$OPEN_JOB" \
   && grep -qxF '          body-path: ${{ runner.temp }}/flush/pr-body.md' <<<"$OPEN_JOB"; then
  ok "the payload artifact is uploaded and consumed under one name and path"
else
  bad "the artifact name, its path, the module-file copy or body-path no longer agree between the jobs"
fi

mints=$(code_count 'actions/create-github-app-token@')
keys=$(code_count 'PUBLIC_MIRROR_APP_PRIVATE_KEY')
if [ "$mints" -eq 1 ] && [ "$keys" -eq 1 ] \
   && grep -qF 'PUBLIC_MIRROR_APP_PRIVATE_KEY' <<<"$OPEN_JOB"; then
  ok "one mint step and one key reference, both in the open-pr job"
else
  bad "expected exactly one mint step and one key reference, in the open-pr job" "mints=$mints keys=$keys"
fi

# `app-token` appears three times in code: the mint action's name, the step id
# and the one consumer. Counting the id (not `steps.app-token.outputs`) also
# covers `steps['app-token']`. The consumer must be the `token:` input of the
# create-pull-request step itself.
# shellcheck disable=SC2016  # literal GitHub expression, not a shell expansion
if grep -A2 -F 'uses: peter-evans/create-pull-request@' <<<"$OPEN_JOB" \
     | grep -qxF '          token: ${{ steps.app-token.outputs.token }}' \
   && [ "$(code_count 'app-token')" -eq 3 ] \
   && ! grep -qE 'github\.token|GITHUB_TOKEN' <<<"$OPEN_JOB"; then
  ok "the PR is opened with the App token, and only the PR step consumes it"
else
  bad "create-pull-request must take the App token, and nothing else may consume it"
fi

# Every way to give GITHUB_TOKEN a write scope: a `scope: write` line (quoted or
# not), `write-all`, or any inline value on a `permissions:` line. The mint
# step's `permission-*: write` inputs scope the App token and are excluded.
writes=$(code_lines \
  | grep -E "write-all|^[[:space:]]*permissions:[[:space:]]*[^[:space:]]|^[[:space:]]+[a-z-]+:[[:space:]]*[\"']?write" \
  | grep -vE '^[[:space:]]+permission-[a-z-]+: write$' || true)
if [ "$writes" = "      pull-requests: write" ] \
   && grep -qxF '      pull-requests: write' <<<"$GUARD_JOB"; then
  ok "the only GITHUB_TOKEN write scope is pull-requests, in the guard job"
else
  bad "GITHUB_TOKEN may hold one write scope only: pull-requests, in the guard job" "$writes"
fi

echo "─────────────────────────────────"
echo "go-transitive-flush render test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
