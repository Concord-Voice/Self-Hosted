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
# [internal]tests/test-rtc-port-ingress-parity.sh.
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
    bad "$name — wrong counts" "want: $want"$'\n'"got:  ${got:-<no count line found>}"
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

# ── the body must carry the CI re-trigger warning ────────────────────────────
# pr-ci.yml does not fire for GITHUB_TOKEN-authored PRs, so the body is the only
# place a reviewer learns the checklist is unsatisfiable until they act.
if grep -q "Close and reopen this PR" /tmp/pr-body.md; then
  ok "body carries the CI re-trigger instruction"
else
  bad "body is missing the CI re-trigger instruction"
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

echo "─────────────────────────────────"
echo "go-transitive-flush render test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
