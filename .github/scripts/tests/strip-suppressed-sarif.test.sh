#!/usr/bin/env bash
#
# strip-suppressed-sarif.test.sh — hermetic self-test for the "Strip in-source-
# suppressed results from SARIF" step in semgrep.yml. Needs only jq, git and awk.
# The step is inline (a script file would be PR-controlled in the protected run),
# so this extracts the block between its BEGIN/END markers and runs it under the
# shell options Actions uses, inside a throwaway git repo that stands in for the
# merge base (BASELINE_COMMIT) and the scanned merge SHA (HEAD).
#
# What it pins. A suppressed result is DROPPED only for an annotation the base
# already carried (the PR edits the annotated line, or leaves it alone). It is
# KEPT when the PR adds, upper-cases, re-spells or relocates the annotation, when
# the path is a symlink or unreadable at HEAD, when no annotation sits on the
# match, when the path is forged, and when there is no base. Unsuppressed results
# and an unparseable file are untouched, and no log line can start a workflow
# command.
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
printf 'a // nosemgrep: r.old\n'            > old.go    # base annotation, PR edits the line
printf 'k // nosemgrep: r.stay\n'           > stay.go   # base annotation, PR never touches it
printf 'b\n'                                > new.go
printf 'c\n'                                > up.go
printf 'd\n'                                > eq.go
printf 'p // nosemgrep: r.r\nq\nbad()\n'    > reloc.go  # annotation on line 1, match later moves
printf 'plain()\n'                          > plain.go  # no annotation anywhere
printf 'e\n'                                > binattr.go # PR adds an annotation AND marks the file binary
printf 'm // nosemgrep: r.m\nn\n'           > reloc2.go # annotation on line 1, moves to line 2 in ONE hunk
printf 'x\n'                                > link-target.go
ln -s nosemgrep-t.go stay-link.go                       # unchanged symlink whose target text reads like an annotation
git add . && g commit -q -m base
BASE=$(git rev-parse HEAD)
printf 'a2 // nosemgrep: r.old\n'           > old.go    # edited line keeps its annotation
printf 'b // nosemgrep: r.new\n'            > new.go    # annotation added
printf 'c // NOSEMGREP: r.up\n'             > up.go     # added in upper case
printf 'd // nosem=r.eq/x\n'                > eq.go     # added with `=` and a rule id of odd shape
printf 'p\nq\nbad() // nosemgrep: r.r\n'    > reloc.go  # annotation moved from line 1 to the match
printf 'e // nosemgrep: r.b\n'              > binattr.go
printf 'binattr.go binary\n'                > .gitattributes   # `git diff` now says "Binary files differ"
printf 'm\nn // nosemgrep: r.m\n'           > reloc2.go
ln -s link-target.go link.go                            # symlink: HEAD content is a path
git add . && g commit -q -m head

# result NAME FILE LINE: one suppressed result
r() { printf '{"ruleId":"%s","suppressions":[{"kind":"inSource"}],"locations":[{"physicalLocation":{"artifactLocation":{"uri":%s},"region":{"startLine":%s}}}]}' "$1" "$2" "$3"; }
mk_sarif() {
  {
    printf '{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"Semgrep OSS"}},"results":['
    r old.rule '"old.go"' 1; printf ','
    r stay.rule '"stay.go"' 1; printf ','
    r new.rule '"new.go"' 1; printf ','
    r up.rule '"up.go"' 1; printf ','
    r eq.rule '"eq.go"' 1; printf ','
    r reloc.rule '"reloc.go"' 3; printf ','
    r binattr.rule '"binattr.go"' 1; printf ','
    r reloc2.rule '"reloc2.go"' 2; printf ','
    r link.rule '"link.go"' 1; printf ','
    r stayl.rule '"stay-link.go"' 1; printf ','
    r plain.rule '"plain.go"' 1; printf ','
    r ghost.rule '"ghost.go"' 1; printf ','
    r forged.rule '"x.go\n::warning::forged"' 1; printf ','
    printf '{"ruleId":"live.rule"},{"ruleId":"live.empty","suppressions":[]}]},{"tool":{"driver":{"name":"Semgrep OSS"}}}]}\n'
  } > semgrep-custom.sarif
  printf 'not json' > semgrep-community.sarif
}
ids() { jq -c '[.runs[0].results[].ruleId]' semgrep-custom.sarif; }
warned() { grep -cF "::warning::nosemgrep added by this PR or not verifiable against the base, result kept: [\"$1\"" <<<"$out"; }

# --- run 1: base available ------------------------------------------------------
mk_sarif; cp semgrep-community.sarif bad.orig
out=$(BASELINE_COMMIT="$BASE" bash -eo pipefail "$TMP/step.sh" 2>/dev/null); rc=$?
check "step exits 0" "$rc" "0"
check "only base-carried annotations dropped" "$(ids)" \
  '["new.rule","up.rule","eq.rule","reloc.rule","binattr.rule","reloc2.rule","link.rule","stayl.rule","plain.rule","ghost.rule","forged.rule","live.rule","live.empty"]'
check "run without results survives" "$(jq '.runs | length' semgrep-custom.sarif)" "2"
check "counts logged" "$(grep -c -- '15 -> 13' <<<"$out")" "1"
check "edited annotated line dropped" "$(grep -cF 'dropped suppressed: ["old.rule","old.go",1]' <<<"$out")" "1"
check "untouched annotation dropped" "$(grep -cF 'dropped suppressed: ["stay.rule","stay.go",1]' <<<"$out")" "1"
for id in new up eq reloc binattr reloc2 link stayl plain ghost; do
  check "$id kept with a warning" "$(warned "$id.rule")" "1"
done
check "control chars in a path cannot forge a command" "$(grep -c '^::warning::forged' <<<"$out")" "0"
check "escaped path still listed" "$(grep -cF 'x.go\n::warning::forged' <<<"$out")" "1"
check "bad file warns" "$(grep -c '^::warning::SARIF suppression filter failed' <<<"$out")" "1"
check "bad file untouched" "$(cmp -s semgrep-community.sarif bad.orig && echo same)" "same"
check "no temp left" "$(ls | grep -c filtered)" "0"

# --- run 2: no base to compare with -> nothing is dropped -----------------------
mk_sarif
out=$(env -u BASELINE_COMMIT bash -eo pipefail "$TMP/step.sh" 2>/dev/null); rc=$?
check "no base: exits 0" "$rc" "0"
check "no base: every suppressed result kept" "$(jq '[.runs[0].results[]] | length' semgrep-custom.sarif)" "15"

# --- run 3: no SARIF files ------------------------------------------------------
rm -f semgrep-custom.sarif semgrep-community.sarif
check "missing files skipped" "$(BASELINE_COMMIT="$BASE" bash -eo pipefail "$TMP/step.sh" >/dev/null 2>&1; echo $?)" "0"
exit $fail
