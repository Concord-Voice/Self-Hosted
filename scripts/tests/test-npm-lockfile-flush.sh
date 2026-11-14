#!/usr/bin/env bash
# SC2015: `cond && ok … || bad …` is safe here because ok() always returns 0.
# SC2016: backticks in single quotes are literal Markdown, not expansions.
# shellcheck disable=SC2015,SC2016
#
# Regression test for .github/workflows/npm-lockfile-flush.yml (#272).
#
# The workflow runs only on its weekly cron, so a broken step looks the same as
# "nothing to update". This test EXTRACTS the real `run:` blocks and executes
# them against a stub npm in throwaway repositories, so it cannot drift from
# what CI runs. It also locks the job split: no write credential shares a job
# with the package refresh, and the job that holds the App key runs no npm.
#
# awk, not PyYAML: this must run on a host without PyYAML.

set -uo pipefail

unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY \
      GIT_COMMON_DIR GIT_PREFIX GIT_CONFIG_PARAMETERS 2>/dev/null || true

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORKFLOW="$REPO_ROOT/.github/workflows/npm-lockfile-flush.yml"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0
ok()  { echo "ok:   $1"; pass=$((pass + 1)); }
bad() { echo "FAIL: $1"; [ -n "${2:-}" ] && printf '    %s\n' "${2//$'\n'/$'\n'    }"; fail=$((fail + 1)); }

# ── extract a step's shell verbatim ─────────────────────────────────────────
extract_step() { # $1 = step name, $2 = output file
  {
    printf '#!/usr/bin/env bash\n'
    awk -v want="      - name: $1" '
      $0 == want                                  { instep = 1; next }
      instep && /^        run: \|[[:space:]]*$/   { inrun = 1; next }
      inrun {
        if ($0 == "")           { print ""; next }
        if ($0 ~ /^          /) { sub(/^          /, ""); print; next }
        exit
      }
    ' "$WORKFLOW"
  } > "$2"
  [ "$(wc -l < "$2")" -gt 3 ]
}

extract_step 'Refresh the three lockfiles within existing ranges' "$WORK/refresh.sh" \
  && ok "extracted the refresh step" || { bad "could not extract the refresh step"; exit 1; }
extract_step 'Contention guard — defer if open Dependabot npm PRs touch these lockfiles' "$WORK/guard.sh" \
  && ok "extracted the guard step" || { bad "could not extract the guard step"; exit 1; }
extract_step 'Render PR body' "$WORK/render.sh" \
  && ok "extracted the render step" || { bad "could not extract the render step"; exit 1; }
for s in guard refresh render; do
  bash -n "$WORK/$s.sh" 2>"$WORK/syntax.err" && ok "$s step is valid bash" \
    || bad "$s step has a bash syntax error" "$(cat "$WORK/syntax.err")"
done

# ── harness: a repo with the three roots and a stub npm ─────────────────────
lock() { # $1 = name, $2 = version of node_modules/dep
  printf '{"name":"%s","lockfileVersion":3,"packages":{"":{"name":"%s"},"node_modules/dep":{"version":"%s"}}}\n' "$1" "$1" "$2"
}
make_repo() { # prints the repo path
  local dir="$WORK/repo-$RANDOM$RANDOM"
  mkdir -p "$dir/client/admin" "$dir/services/media-plane"
  for root in . client/admin services/media-plane; do
    printf '{"name":"r","devDependencies":{"dep":"^1.0.0"}}\n' > "$dir/$root/package.json"
    lock r 1.0.0 > "$dir/$root/package-lock.json"
  done
  ( cd "$dir" && git init -q . && git config user.email t@t.t && git config user.name t \
      && git add -A && git commit -qm base )
  echo "$dir"
}
# The stub records its arguments and the env it saw, then does $NPM_STUB_MODE.
make_stub() {
  mkdir -p "$WORK/bin"
  cat > "$WORK/bin/npm" <<'STUB'
#!/usr/bin/env bash
if [ "$1 $2" = "config ls" ]; then
  [ "${NPM_STUB_KNOWS_AGE:-1}" = 1 ] && echo 'min-release-age = null'
  [ -n "${npm_config_min_release_age:-}" ] && echo "min-release-age = $npm_config_min_release_age"
  exit 0
fi
[ "$1" = "--version" ] && { echo 0.0.0-stub; exit 0; }
echo "$PWD|$*|ignore=${npm_config_ignore_scripts:-}|age=${npm_config_min_release_age:-}" >> "$NPM_STUB_LOG"
case "$NPM_STUB_MODE" in
  bump)     [ "$PWD" = "$NPM_STUB_ROOT/client/admin" ] && \
              printf '{"name":"r","lockfileVersion":3,"packages":{"":{"name":"r"},"node_modules/dep":{"version":"1.2.0"}}}\n' > package-lock.json ;;
  manifest) [ "$PWD" = "$NPM_STUB_ROOT/services/media-plane" ] && \
              printf '{"name":"r","devDependencies":{"dep":"^1.2.0"}}\n' > package.json ;;
  stray)    [ "$PWD" = "$NPM_STUB_ROOT" ] && touch stray.txt ;;
  downgrade) [ "$PWD" = "$NPM_STUB_ROOT/services/media-plane" ] && \
              printf '{"name":"r","lockfileVersion":3,"packages":{"":{"name":"r"},"node_modules/dep":{"version":"0.9.0"}}}\n' > package-lock.json ;;
  none)     ;;
esac
exit 0
STUB
  chmod +x "$WORK/bin/npm"
}
make_stub

run_refresh() { # $1 = mode, [$2 = knows-age 0/1]; sets OUT, RC, REPO, OUTPUTS
  REPO=$(make_repo)
  : > "$WORK/npm.log"; : > "$WORK/gh_output"
  OUT=$(cd "$REPO" && PATH="$WORK/bin:$PATH" NPM_STUB_MODE="$1" NPM_STUB_KNOWS_AGE="${2:-1}" \
        NPM_STUB_ROOT="$REPO" NPM_STUB_LOG="$WORK/npm.log" GITHUB_OUTPUT="$WORK/gh_output" \
        npm_config_ignore_scripts=true npm_config_min_release_age=7 \
        bash "$WORK/refresh.sh" 2>&1); RC=$?
  OUTPUTS=$(cat "$WORK/gh_output")
}

# ── refresh step behaviour ──────────────────────────────────────────────────
run_refresh bump
if [ "$RC" -eq 0 ] && [ "$OUTPUTS" = "has_changes=true" ]; then
  ok "a lockfile-only change reports has_changes=true"
else
  bad "a lockfile-only change should pass with has_changes=true" "rc=$RC outputs=$OUTPUTS"$'\n'"$OUT"
fi
if [ "$(grep -c '|update --package-lock-only --ignore-scripts --min-release-age=7 --no-audit --no-fund|ignore=true|age=7$' "$WORK/npm.log")" -eq 3 ] \
   && grep -q "^$REPO|" "$WORK/npm.log" && grep -q "^$REPO/client/admin|" "$WORK/npm.log" \
   && grep -q "^$REPO/services/media-plane|" "$WORK/npm.log"; then
  ok "npm update runs once per root, lockfile-only, scripts off, 7-day window"
else
  bad "npm update was not run as required in each of the three roots" "$(cat "$WORK/npm.log")"
fi

run_refresh none
[ "$RC" -eq 0 ] && [ "$OUTPUTS" = "has_changes=false" ] \
  && ok "no change reports has_changes=false (no PR)" \
  || bad "no change should pass with has_changes=false" "rc=$RC outputs=$OUTPUTS"$'\n'"$OUT"

run_refresh manifest
[ "$RC" -ne 0 ] && [ -z "$OUTPUTS" ] && grep -q 'services/media-plane/package.json' <<<"$OUT" \
  && ok "a package.json change fails the run and names the file" \
  || bad "a package.json change must fail the run" "rc=$RC outputs=$OUTPUTS"$'\n'"$OUT"

run_refresh stray
[ "$RC" -ne 0 ] && grep -q 'stray.txt' <<<"$OUT" \
  && ok "any other new file fails the run" \
  || bad "an untracked file must fail the run" "rc=$RC"$'\n'"$OUT"

run_refresh downgrade
[ "$RC" -ne 0 ] && [ -z "$OUTPUTS" ] && grep -q 'services/media-plane/package-lock.json: node_modules/dep 1.0.0 -> 0.9.0' <<<"$OUT" \
  && ok "a downgrade fails the run and names the lockfile, package and versions" \
  || bad "a downgrade must fail the run" "rc=$RC outputs=$OUTPUTS"$'\n'"$OUT"

run_refresh bump 0
[ "$RC" -ne 0 ] && [ ! -s "$WORK/npm.log" ] && grep -q 'no min-release-age' <<<"$OUT" \
  && ok "an npm without min-release-age fails before any update, even with the env set" \
  || bad "an npm without min-release-age must fail before updating" "rc=$RC"$'\n'"$OUT"

# ── guard step: which open PRs defer the run ────────────────────────────────
# The stub gh applies the step's own --jq filter to fixture PR JSON, so the
# filter itself is what is tested.
cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
case "$1" in
  pr)
    if [ "$2" = list ]; then
      while [ $# -gt 0 ]; do [ "$1" = --jq ] && { jq -r "$2" "$GH_STUB_PRS"; exit; }; shift; done
      exit 1
    fi
    [ "$2" = comment ] && echo "comment $3" >> "$GH_STUB_LOG" ;;
  api) echo 0 ;;
esac
STUB
chmod +x "$WORK/bin/gh"
run_guard() { # $1 = fixture PR JSON; sets OUT, RC, OUTPUTS
  printf '%s' "$1" > "$WORK/prs.json"; : > "$WORK/gh.log"; : > "$WORK/gh_output"
  OUT=$(PATH="$WORK/bin:$PATH" GH_STUB_PRS="$WORK/prs.json" GH_STUB_LOG="$WORK/gh.log" \
        GITHUB_OUTPUT="$WORK/gh_output" GITHUB_SERVER_URL=https://github.invalid \
        GITHUB_REPOSITORY=o/r GITHUB_RUN_ID=1 bash "$WORK/guard.sh" 2>&1); RC=$?
  OUTPUTS=$(cat "$WORK/gh_output")
}
DEP='"author":{"login":"app/dependabot"},"isCrossRepository":false'
run_guard "[{\"number\":1,\"headRefName\":\"dependabot/npm_and_yarn/vite-1\",$DEP},
  {\"number\":2,\"headRefName\":\"dependabot/npm_and_yarn/dot-github/tools/license-checker/x-1\",$DEP},
  {\"number\":3,\"headRefName\":\"dependabot/npm_and_yarn/evil\",\"author\":{\"login\":\"app/dependabot\"},\"isCrossRepository\":true},
  {\"number\":4,\"headRefName\":\"dependabot/npm_and_yarn/evil\",\"author\":{\"login\":\"mallory\"},\"isCrossRepository\":false},
  {\"number\":5,\"headRefName\":\"dependabot/go_modules/x\",$DEP}]"
[ "$RC" -eq 0 ] && [ "$OUTPUTS" = "deferred=true" ] && [ "$(cat "$WORK/gh.log")" = "comment 1" ] \
  && ok "the guard defers on a same-repo Dependabot npm PR and comments on that PR only" \
  || bad "the guard must count only PR 1 (not license-checker, fork, other author or Go)" "rc=$RC outputs=$OUTPUTS log=$(cat "$WORK/gh.log")"$'\n'"$OUT"
run_guard "[{\"number\":3,\"headRefName\":\"dependabot/npm_and_yarn/evil\",\"author\":{\"login\":\"mallory\"},\"isCrossRepository\":true}]"
[ "$RC" -eq 0 ] && [ "$OUTPUTS" = "deferred=false" ] && [ ! -s "$WORK/gh.log" ] \
  && ok "a spoofed dependabot/... branch does not defer the run" \
  || bad "a spoofed branch must not defer the run" "rc=$RC outputs=$OUTPUTS"$'\n'"$OUT"

# ── render step: counts per lockfile ────────────────────────────────────────
REPO=$(make_repo)
lock r 1.2.0 > "$REPO/client/admin/package-lock.json"
printf '{"name":"r","lockfileVersion":3,"packages":{"":{"name":"r"},"node_modules/dep":{"version":"1.0.1"},"node_modules/new":{"version":"2.0.0"}}}\n' \
  > "$REPO/services/media-plane/package-lock.json"
rm -f /tmp/pr-body.md
OUT=$(cd "$REPO" && TRIGGER=schedule bash "$WORK/render.sh" 2>&1); RC=$?
BODY=$(cat /tmp/pr-body.md 2>/dev/null || true)
if [ "$RC" -eq 0 ] && grep -qxF '| `package-lock.json` | 0 |' <<<"$BODY" \
   && grep -qxF '| `client/admin/package-lock.json` | 1 |' <<<"$BODY" \
   && grep -qxF '| `services/media-plane/package-lock.json` | 2 |' <<<"$BODY" \
   && grep -qF 'trigger: `schedule`' <<<"$BODY"; then
  ok "the PR body counts changed entries per lockfile"
else
  bad "the PR body table is wrong" "rc=$RC"$'\n'"$OUT"$'\n'"$BODY"
fi

# ── static: the job split is a credential boundary ──────────────────────────
job_block() { # $1 = job id; that job's lines, comment lines dropped
  awk -v j="  $1:" '
    $0 == j                { p = 1; next }
    p && /^  [^ #]/        { exit }
    p && !/^[[:space:]]*#/ { print }
  ' "$WORKFLOW"
}
GUARD_JOB=$(job_block guard)
REFRESH_JOB=$(job_block refresh)
OPEN_JOB=$(job_block open-pr)
# npm or npx in command position (line start, or after && | ; or a subshell
# paren), so prose such as a PR title that says "npm lockfiles" does not count.
NPM_TOOLING='setup-node|(^[[:space:]]*|&&[[:space:]]*|[|;(][[:space:]]*)(npm|npx) '

job_ids=$(awk '/^jobs:/ { p = 1; next } p && /^  [^ #]/ { sub(/:.*/, ""); sub(/^  /, ""); printf "%s ", $0 }' "$WORKFLOW")
[ "$job_ids" = "guard refresh open-pr " ] \
  && ok "the workflow has exactly the jobs guard, refresh and open-pr" \
  || bad "the job set changed; every job needs a credential assertion here" "jobs: $job_ids"

grep -qF 'npm update' <<<"$REFRESH_JOB" \
  && ! grep -qE 'secrets|github\.token|PUBLIC_MIRROR_APP_|create-github-app-token|create-pull-request|write' <<<"$REFRESH_JOB" \
  && ok "the refresh job resolves packages and holds no secret and no write scope" \
  || bad "the refresh job is missing, or references a secret, a token or a write scope"

grep -qE '^    needs: guard$' <<<"$REFRESH_JOB" \
  && grep -qxF "    if: needs.guard.outputs.deferred != 'true'" <<<"$REFRESH_JOB" \
  && [ -n "$GUARD_JOB" ] && ! grep -qE "$NPM_TOOLING|PUBLIC_MIRROR_APP_|create-github-app-token" <<<"$GUARD_JOB" \
  && ok "the guard job gates refresh and runs no npm and no App credential" \
  || bad "the guard job is missing, does not gate refresh, or does more than query pull requests"

# shellcheck disable=SC2016  # literal GitHub expression, not a shell expansion
grep -qF 'has_changes: ${{ steps.refresh.outputs.has_changes }}' <<<"$REFRESH_JOB" \
  && grep -qE '^    needs: refresh$' <<<"$OPEN_JOB" \
  && grep -qxF "    if: needs.refresh.outputs.has_changes == 'true'" <<<"$OPEN_JOB" \
  && ok "the open-pr job runs only when the refresh job reports a change" \
  || bad "the open-pr job is not gated on the refresh job's has_changes output"

[ -n "$OPEN_JOB" ] && ! grep -qE "$NPM_TOOLING" <<<"$OPEN_JOB" \
  && ok "the open-pr job runs no npm" \
  || bad "the job that holds the App key must not run npm"

grep -qF -- '--author app/dependabot' <<<"$GUARD_JOB" \
  && grep -qF '.author.login == "app/dependabot" and .isCrossRepository == false' <<<"$GUARD_JOB" \
  && ok "the contention guard counts only Dependabot's own same-repository PRs" \
  || bad "the contention guard must ignore PRs not opened by Dependabot from this repository"

for job in guard refresh open-pr; do
  if job_block "$job" | grep -qE '^    permissions:$' && job_block "$job" | grep -qxF '      contents: read'; then
    ok "the $job job declares contents: read"
  else
    bad "the $job job must declare its own permissions with contents: read"
  fi
done
[ "$(job_block refresh | grep -cE '^      [a-z-]+: (read|write)$')" -eq 1 ] && [ "$(job_block open-pr | grep -cE '^      [a-z-]+: (read|write)$')" -eq 1 ] \
  && ok "refresh and open-pr hold no GITHUB_TOKEN scope beyond contents: read" \
  || bad "refresh and open-pr must hold only contents: read"

# The PR may carry the three lockfiles and nothing else.
add_paths=$(awk '/^          add-paths: \|/ { p = 1; next } p && /^            / { sub(/^ +/, ""); print; next } p { exit }' <<<"$OPEN_JOB" | sort | tr '\n' ' ')
[ "$add_paths" = "client/admin/package-lock.json package-lock.json services/media-plane/package-lock.json " ] \
  && ok "add-paths is exactly the three lockfiles" \
  || bad "add-paths must list exactly the three lockfiles" "got: $add_paths"

# shellcheck disable=SC2016  # literal GitHub expressions, not shell expansions
[ "$(grep -cxF '          name: npm-lockfile-flush' <<<"$REFRESH_JOB")" -eq 1 ] \
  && [ "$(grep -cxF '          name: npm-lockfile-flush' <<<"$OPEN_JOB")" -eq 1 ] \
  && [ "$(grep -cxF '          path: ${{ runner.temp }}/flush' <<<"$REFRESH_JOB")" -eq 1 ] \
  && [ "$(grep -cxF '          path: ${{ runner.temp }}/flush' <<<"$OPEN_JOB")" -eq 1 ] \
  && grep -qxF '          body-path: ${{ runner.temp }}/flush/pr-body.md' <<<"$OPEN_JOB" \
  && ok "the payload artifact is uploaded and consumed under one name and path" \
  || bad "the artifact name, its path or body-path no longer agree between the jobs"

echo "─────────────────────────────────"
echo "npm-lockfile-flush test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
