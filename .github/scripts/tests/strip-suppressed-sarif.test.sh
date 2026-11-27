#!/usr/bin/env bash
#
# strip-suppressed-sarif.test.sh — hermetic self-test for the "Strip in-source-
# suppressed results from SARIF" step in semgrep.yml. Needs only jq and git. The
# step is inline (a script file would be PR-controlled in the protected run), so
# this extracts the block between its BEGIN/END markers and runs it under the
# shell options Actions uses, inside a throwaway git repo that stands in for the
# merge base (BASELINE_COMMIT) and the scanned merge SHA (HEAD).
#
# What it pins:
#   - a suppression the base already carried (PR edits the annotated line) is dropped;
#   - a `nosemgrep` the PR ADDS keeps its result, so a PR cannot silence itself;
#   - a path unreadable at HEAD, a forged multi-line path, and a missing base all
#     fail closed (result kept), and a path cannot forge a workflow command;
#   - unsuppressed results and an unparseable file are untouched.
set -u
ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
# STRIP_TEST_WORKFLOW points the test at a mutated copy to prove it goes red.
WORKFLOW="${STRIP_TEST_WORKFLOW:-$ROOT/.github/workflows/semgrep.yml}"
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
fail=0
check() { if [ "$2" = "$3" ]; then echo "ok   $1"; else echo "FAIL $1: want '$3' got '$2'"; fail=1; fi; }

awk '/# BEGIN strip-suppressed-sarif/{f=1;next} /# END strip-suppressed-sarif/{f=0} f' "$WORKFLOW" \
  | sed 's/^          //' > "$TMP/step.sh"
check "step extracted" "$([ -s "$TMP/step.sh" ] && echo yes)" "yes"

# --- a repo with a base commit and a head commit --------------------------------
REPO="$TMP/repo"; mkdir "$REPO"; cd "$REPO" || exit 1
git init -q .
g() { git -c user.name=t -c user.email=t@t.invalid -c commit.gpgsign=false "$@"; }
printf 'a // nosemgrep: r.old\n' > old.go
printf 'b\n' > new.go
git add . && g commit -q -m base
BASE=$(git rev-parse HEAD)
printf 'a2 // nosemgrep: r.old\n' > old.go          # edits the annotated line, annotation unchanged
printf 'b // nosemgrep: r.new\n' > new.go           # the PR adds an annotation
git add . && g commit -q -m head

mk_sarif() {
  cat > semgrep-custom.sarif <<'JSON'
{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"Semgrep OSS"}},"results":[
 {"ruleId":"old.rule","suppressions":[{"kind":"inSource"}],
  "locations":[{"physicalLocation":{"artifactLocation":{"uri":"old.go"},"region":{"startLine":1}}}]},
 {"ruleId":"new.rule","suppressions":[{"kind":"inSource"}],
  "locations":[{"physicalLocation":{"artifactLocation":{"uri":"new.go"},"region":{"startLine":1}}}]},
 {"ruleId":"ghost.rule","suppressions":[{"kind":"inSource"}],
  "locations":[{"physicalLocation":{"artifactLocation":{"uri":"ghost.go"},"region":{"startLine":1}}}]},
 {"ruleId":"forged.rule","suppressions":[{"kind":"inSource"}],
  "locations":[{"physicalLocation":{"artifactLocation":{"uri":"x.go\n::warning::forged"},"region":{"startLine":1}}}]},
 {"ruleId":"live.rule"},
 {"ruleId":"live.empty","suppressions":[]}]},
 {"tool":{"driver":{"name":"Semgrep OSS"}}}]}
JSON
  printf 'not json' > semgrep-community.sarif
}
ids() { jq -c '[.runs[0].results[].ruleId]' semgrep-custom.sarif; }

# --- run 1: base available ------------------------------------------------------
mk_sarif; cp semgrep-community.sarif bad.orig
out=$(BASELINE_COMMIT="$BASE" bash -eo pipefail "$TMP/step.sh" 2>/dev/null); rc=$?
check "step exits 0" "$rc" "0"
check "pre-existing annotation dropped, PR-added and unverifiable kept" "$(ids)" '["new.rule","ghost.rule","forged.rule","live.rule","live.empty"]'
check "run without results survives" "$(jq '.runs | length' semgrep-custom.sarif)" "2"
check "counts logged" "$(grep -c -- '6 -> 5' <<<"$out")" "1"
check "dropped result listed" "$(grep -cF 'dropped suppressed: ["old.rule","old.go",1]' <<<"$out")" "1"
check "PR-added annotation warned" "$(grep -cF '::warning::nosemgrep added by this PR or not verifiable against the base, result kept: ["new.rule","new.go",1]' <<<"$out")" "1"
check "control chars in a path cannot forge a command" "$(grep -c '^::warning::forged' <<<"$out")" "0"
check "escaped path still listed" "$(grep -cF 'x.go\n::warning::forged' <<<"$out")" "1"
check "bad file warns" "$(grep -c '^::warning::SARIF suppression filter failed' <<<"$out")" "1"
check "bad file untouched" "$(cmp -s semgrep-community.sarif bad.orig && echo same)" "same"
check "no temp left" "$(ls | grep -c filtered)" "0"

# --- run 2: no base to compare with -> nothing is dropped -----------------------
mk_sarif
out=$(env -u BASELINE_COMMIT bash -eo pipefail "$TMP/step.sh" 2>/dev/null); rc=$?
check "no base: exits 0" "$rc" "0"
check "no base: every suppressed result kept" "$(jq '[.runs[0].results[]] | length' semgrep-custom.sarif)" "6"

# --- run 3: no SARIF files ------------------------------------------------------
rm -f semgrep-custom.sarif semgrep-community.sarif
check "missing files skipped" "$(BASELINE_COMMIT="$BASE" bash -eo pipefail "$TMP/step.sh" >/dev/null 2>&1; echo $?)" "0"
exit $fail
