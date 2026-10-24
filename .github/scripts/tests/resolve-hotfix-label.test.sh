#!/usr/bin/env bash
#
# resolve-hotfix-label.test.sh — self-test for resolve-hotfix-label.sh.
#
# Hermetic: a PATH-prepended mock `gh` whose behaviour is driven by env vars,
# mirroring the mock-gh pattern in infrastructure/deploy/tests/test-deploy-spa.sh.
# No network, no sleeping (HOTFIX_RETRY_DELAY=0 throughout).
#
# The case that matters is "association empty then resolves" — the #3013 race.
# Everything else exists so the fix cannot be mistaken for simply deleting the
# association path.

# WHAT A MOCK CANNOT PROVE. The stub answers in the shape the script expects,
# so it validates logic, not the real API's response shape or jq filters. That
# half was verified by hand against live GitHub on 2026-08-29, at the three
# branches that matter — reproduce with an authenticated `gh`:
#
#   export GITHUB_REPOSITORY=Concord-Voice/Self-Hosted
#   M=$(gh api repos/$GITHUB_REPOSITORY/pulls/3013 --jq .merge_commit_sha)
#   # 1. the founding incident, whose old-code answer was silence:
#   SHA=$M COMMIT_SUBJECT="$(git log -1 --format=%s $M)" \
#     bash .github/scripts/resolve-hotfix-label.sh
#     # -> hotfix=true resolution=subject pr_number=3013
#   # 2. association fallback, subject reference removed:
#   SHA=$M COMMIT_SUBJECT="no reference" bash .github/scripts/resolve-hotfix-label.sh
#     # -> hotfix=true resolution=association pr_number=3013
#   # 3. a merged NON-hotfix PR must say no-label, never undetermined:
#   M2=$(gh api repos/$GITHUB_REPOSITORY/pulls/3014 --jq .merge_commit_sha)
#   SHA=$M2 COMMIT_SUBJECT="$(git log -1 --format=%s $M2)" \
#     bash .github/scripts/resolve-hotfix-label.sh
#     # -> hotfix=false resolution=no-label pr_number=3014
#
# Deliberately NOT automated here. A live check would bind this suite to #3013
# and #3014 keeping their present labels forever, so relabelling an old PR
# would fail a test about code that never changed. The suite stays hermetic;
# this block is the procedure, run by hand when the API contract is in doubt.
#
# Still unproven by either layer: that `github.token` carrying exactly
# `contents|pull-requests|issues: read` authorizes these three endpoints. The
# local run used a broader token. That is a permissions claim, and only a real
# push to main can settle it — which is why the reporting step distinguishes
# `api-error` from `undetermined` rather than assuming success.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
SCRIPT="${REPO_ROOT}/.github/scripts/resolve-hotfix-label.sh"

pass=0
fail=0
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

STUB_DIR="$WORK/bin"
mkdir -p "$STUB_DIR"

# ── Mock gh ───────────────────────────────────────────────────────────────────
# MOCK_PR_SHAS        "n:sha,n:sha" — what pulls/{n} reports as merge_commit_sha.
#                     An unlisted n exits 1, i.e. the PR read fails.
# MOCK_API_FAIL       non-empty makes every pulls/{n} read fail (403 shape)
# MOCK_LABELS         what issues/{n}/labels returns
# MOCK_LABELS_FAIL    non-empty makes the labels read fail
# MOCK_ASSOC_PR       the PR number the association eventually returns
# MOCK_ASSOC_EMPTY_N  how many association calls return empty first (the race)
cat > "$STUB_DIR/gh" <<'STUB'
#!/usr/bin/env bash
url="$*"
case "$url" in
  *"/commits/"*"/pulls"*)
    n=$(cat "$MOCK_COUNTER" 2>/dev/null || echo 0)
    echo $(( n + 1 )) > "$MOCK_COUNTER"
    if (( n < ${MOCK_ASSOC_EMPTY_N:-0} )); then printf '\n'; else printf '%s\n' "${MOCK_ASSOC_PR:-}"; fi ;;
  *"/pulls/"*)
    [[ -z "${MOCK_API_FAIL:-}" ]] || { echo "HTTP 403: Resource not accessible by integration" >&2; exit 1; }
    want="${url##*/pulls/}"; want="${want%% *}"
    found=""
    IFS=',' read -ra pairs <<< "${MOCK_PR_SHAS:-}"
    for pair in "${pairs[@]}"; do
      [[ "${pair%%:*}" == "$want" ]] && found="${pair#*:}"
    done
    [[ -n "$found" ]] || { echo "HTTP 404" >&2; exit 1; }
    printf '%s\n' "$found" ;;
  *"/labels"*)
    [[ -z "${MOCK_LABELS_FAIL:-}" ]] || { echo "HTTP 403: Resource not accessible by integration" >&2; exit 1; }
    printf '%s\n' "${MOCK_LABELS:-}" ;;
  *) exit 1 ;;
esac
STUB
chmod +x "$STUB_DIR/gh"
export PATH="$STUB_DIR:$PATH"

run() { # run <subject> -> sets OUT
  local out gho rc
  gho="$WORK/out.$RANDOM"
  MOCK_COUNTER="$WORK/counter.$RANDOM"; export MOCK_COUNTER
  : > "$MOCK_COUNTER"
  SHA="deadbeefdeadbeefdeadbeefdeadbeefdeadbeef" \
  GITHUB_REPOSITORY="Concord-Voice/Self-Hosted" \
  COMMIT_SUBJECT="$1" \
  HOTFIX_RETRY_DELAY=0 \
  HOTFIX_RETRY_ATTEMPTS="${ATTEMPTS:-5}" \
  GITHUB_OUTPUT="$gho" \
    bash "$SCRIPT" >/dev/null 2>&1 && rc=0 || rc=$?
  # A discarded exit status would let the resolver write correct outputs, exit
  # nonzero, and still pass here — while main-ci's step fails and the release
  # never dispatches. The script exits 0 on every resolution path by design
  # (fail-closed is expressed in the OUTPUTS, not the status), so a nonzero
  # here is always a defect.
  if (( rc != 0 )); then
    echo "  ✗ resolver exited ${rc}; main-ci would fail this step" >&2
    fail=$(( fail + 1 ))
  fi
  OUT=$(cat "$gho" 2>/dev/null | tr '\n' ' ')
  rm -f "$gho" "$MOCK_COUNTER"
}

check() { # check <label> <needle>
  if grep -qF -- "$2" <<< "$OUT"; then
    echo "  ✓ $1"; pass=$(( pass + 1 ))
  else
    echo "  ✗ $1 (missing: $2)" >&2; echo "    got: $OUT" >&2; fail=$(( fail + 1 ))
  fi
}

SHA_REAL="deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
OTHER_SHA="feedfacefeedfacefeedfacefeedfacefeedface"

echo "Case 1: subject names the PR, hotfix label present"
MOCK_PR_SHAS="3013:$SHA_REAL" MOCK_LABELS="type: chore,risk: hotfix" \
  run "chore: propose desktop release v0.2.42 (#3013)"
check "hotfix=true"            "hotfix=true"
check "resolution=subject"     "resolution=subject"
check "resolved_via=subject"   "resolved_via=subject"
check "pr_number=3013"         "pr_number=3013"

echo ""
echo "Case 2: subject path, label absent → no-label (NOT undetermined)"
MOCK_PR_SHAS="3020:$SHA_REAL" MOCK_LABELS="type: chore,area: ci-cd" \
  run "chore: something ordinary (#3020)"
check "hotfix=false"           "hotfix=false"
check "resolution=no-label"    "resolution=no-label"
# THE POINT OF resolved_via: `resolution` says no-label on nearly every merge,
# which is where the subject-vs-association distinction used to vanish.
check "no-label keeps the path" "resolved_via=subject"

echo ""
echo "Case 3: THE RACE — association empty twice, then resolves"
# The #3013 shape: no parsable subject, association not yet written. A single
# attempt returns false here; the retry is what saves it.
MOCK_ASSOC_EMPTY_N=2 MOCK_ASSOC_PR=3013 MOCK_PR_SHAS="3013:$SHA_REAL" \
  MOCK_LABELS="risk: hotfix" run "hand edited squash title with no reference"
check "hotfix=true after retry"   "hotfix=true"
check "resolution=association"    "resolution=association"
check "resolved_via=association"  "resolved_via=association"

echo ""
echo "Case 4: association never resolves → undetermined, not no-label"
MOCK_ASSOC_EMPTY_N=99 MOCK_ASSOC_PR=3013 MOCK_PR_SHAS="3013:$SHA_REAL" \
  MOCK_LABELS="risk: hotfix" run "hand edited squash title with no reference"
check "hotfix=false"              "hotfix=false"
check "resolution=undetermined"   "resolution=undetermined"
check "nothing found -> none"     "resolved_via=none"

echo ""
echo "Case 5: subject cites a PR whose merge_commit_sha does not match"
# Guards the dangerous direction: a stale or hand-typed (#N) must not have its
# labels read, or an unrelated PR could dispatch an ungated release.
MOCK_PR_SHAS="1:$OTHER_SHA,4242:$SHA_REAL" MOCK_ASSOC_EMPTY_N=0 MOCK_ASSOC_PR=4242 \
  MOCK_LABELS="type: chore" run "fix: title citing the wrong number (#1)"
check "falls back to association" "resolution=no-label"
check "uses the verified PR"      "pr_number=4242"
# Fell back to the association, so the path must say so even though the
# outcome is the same no-label an operator sees on an ordinary merge.
check "no-label via association"  "resolved_via=association"

echo ""
echo "Case 6: labels read fails → api-error (a NAMED failure, not silence)"
MOCK_PR_SHAS="3013:$SHA_REAL" MOCK_LABELS_FAIL=1 \
  run "chore: release (#3013)"
check "hotfix=false"              "hotfix=false"
check "resolution=api-error"      "resolution=api-error"
# The PR WAS located before the failure; the path that located it still reports.
# "the API is down" and "we never found the PR" are different incidents.
check "api-error keeps the path"  "resolved_via=subject"
check "and the PR it found"       "pr_number=3013"

echo ""
echo "Case 7: 'risk: hotfix-candidate' must not match 'risk: hotfix'"
MOCK_PR_SHAS="3013:$SHA_REAL" MOCK_LABELS="risk: hotfix-candidate" \
  run "chore: release (#3013)"
check "hotfix=false"              "hotfix=false"
check "resolution=no-label"       "resolution=no-label"

echo ""
echo "Case 8: trailing reference wins over one cited mid-title"
MOCK_PR_SHAS="3013:$SHA_REAL" MOCK_LABELS="risk: hotfix" \
  run "fix: follow-up to (#999) that broke the gate (#3013)"
check "pr_number=3013"            "pr_number=3013"

echo ""
echo "Case 9: API unreachable → api-error, NOT undetermined"
# The permissions hypothesis. `contents: read` alone cannot read PR metadata,
# and the original code's `2>/dev/null || echo ""` made a 403 look exactly like
# "no PR yet" — which is why the #3013 cause could not be proven from the log.
MOCK_API_FAIL=1 MOCK_ASSOC_PR=3013 MOCK_LABELS="risk: hotfix" \
  run "chore: release (#3013)"
check "hotfix=false"              "hotfix=false"
check "resolution=api-error"      "resolution=api-error"
check "never located -> none"     "resolved_via=none"

echo ""
echo "Case 10: association returns a PR that did not produce this commit"
# The association lists PRs CONTAINING the commit, not the one that produced it.
# An open PR carrying the commit on its branch must not have its labels read.
MOCK_ASSOC_EMPTY_N=0 MOCK_ASSOC_PR=777 MOCK_PR_SHAS="777:$OTHER_SHA" \
  MOCK_LABELS="risk: hotfix" run "no reference in this title"
check "hotfix=false"              "hotfix=false"
check "unverified PR rejected"    "resolution=undetermined"

echo ""
echo "Case 11: --help exits 0"
if bash "$SCRIPT" --help >/dev/null 2>&1; then
  echo "  ✓ --help exits 0"; pass=$(( pass + 1 ))
else
  echo "  ✗ --help exits 0" >&2; fail=$(( fail + 1 ))
fi

echo ""
echo "Case 12: missing SHA exits 1"
if GITHUB_REPOSITORY=x bash "$SCRIPT" >/dev/null 2>&1; then
  echo "  ✗ missing SHA should exit 1" >&2; fail=$(( fail + 1 ))
else
  echo "  ✓ missing SHA exits 1"; pass=$(( pass + 1 ))
fi

echo ""
echo "Workflow: the changes job can actually read PR metadata"
MAIN_CI="${REPO_ROOT}/.github/workflows/main-ci.yml"
if grep -q 'pull-requests: read' < <(awk '/^  changes:/{f=1;next} f&&/^  [a-z-]+:$/{f=0} f' "$MAIN_CI"); then
  echo "  ✓ changes job declares pull-requests: read"; pass=$(( pass + 1 ))
else
  echo "  ✗ changes job cannot read PR metadata — every resolution 403s" >&2; fail=$(( fail + 1 ))
fi
if grep -q 'contents: read' < <(awk '/^  changes:/{f=1;next} f&&/^  [a-z-]+:$/{f=0} f' "$MAIN_CI"); then
  echo "  ✓ changes job restates contents: read (job-level REPLACES workflow-level)"; pass=$(( pass + 1 ))
else
  echo "  ✗ job-level permissions dropped contents: read" >&2; fail=$(( fail + 1 ))
fi

echo ""
echo "Workflow: main-ci.yml consumes the script, not an inline lookup"
MAIN_CI="${REPO_ROOT}/.github/workflows/main-ci.yml"
if grep -q 'resolve-hotfix-label.sh' "$MAIN_CI"; then
  echo "  ✓ main-ci.yml calls resolve-hotfix-label.sh"; pass=$(( pass + 1 ))
else
  echo "  ✗ main-ci.yml does not call resolve-hotfix-label.sh" >&2; fail=$(( fail + 1 ))
fi
# The bare association lookup is what raced. Its return would silently
# reintroduce the bug, so pin its absence (comment lines excluded — three
# assertions in the preceding branch matched the prose explaining a removal).
if awk '/^[[:space:]]*#/ { next } /commits\/\$\{SHA\}\/pulls/ { found=1 } END { exit !found }' "$MAIN_CI"; then
  echo "  ✗ main-ci.yml still performs the racing association lookup inline" >&2; fail=$(( fail + 1 ))
else
  echo "  ✓ no inline association lookup remains in main-ci.yml"; pass=$(( pass + 1 ))
fi

echo ""
echo "Workflow: the reporting step consumes resolved_via, and not as \$PATH"
# The natural name for this variable is PATH, which would overwrite the runner's
# PATH for the step and break every command in it — including the shell's own
# lookups. Pin the safe name AND the absence of the dangerous one.
if grep -qE '^\s+RESOLVED_VIA: \$\{\{ steps\.hotfix\.outputs\.resolved_via \}\}' "$MAIN_CI"; then
  echo "  ✓ RESOLVED_VIA is wired to the resolved_via output"; pass=$(( pass + 1 ))
else
  echo "  ✗ reporting step does not consume resolved_via" >&2; fail=$(( fail + 1 ))
fi
if awk '/^[[:space:]]*#/ { next } /^[[:space:]]+PATH:/ { found=1 } END { exit !found }' "$MAIN_CI"; then
  echo "  ✗ a step declares PATH: as an env var — it would clobber the runner PATH" >&2; fail=$(( fail + 1 ))
else
  echo "  ✓ no step overwrites PATH"; pass=$(( pass + 1 ))
fi
# api-error used to fall through to the catch-all arm, announcing "risk: hotfix
# present ... release path runs on this merge" on a run that never read a label
# and never dispatched. A wrong log line is worse than a missing one.
if awk '/^[[:space:]]*#/ { next } /^[[:space:]]+api-error\)/ { found=1 } END { exit !found }' "$MAIN_CI"; then
  echo "  ✓ api-error has its own case arm"; pass=$(( pass + 1 ))
else
  echo "  ✗ api-error falls through to the hotfix-present arm" >&2; fail=$(( fail + 1 ))
fi

echo ""
echo "─────────────────────────────────────────"
echo "PASS: $pass  FAIL: $fail"
[[ "$fail" == "0" ]]
