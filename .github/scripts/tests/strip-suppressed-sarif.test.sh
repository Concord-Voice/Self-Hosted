#!/usr/bin/env bash
#
# strip-suppressed-sarif.test.sh — hermetic self-test for the "Strip in-source-
# suppressed results from SARIF" step in semgrep.yml. Needs only jq. The step is
# inline (a script file would be PR-controlled in the protected run), so this
# extracts the block between its BEGIN/END markers and runs it under the shell
# options Actions uses. A suppressed result is dropped, an unsuppressed one
# kept, and an unparseable file is left byte-identical.
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

cd "$TMP" || exit 1
cat > semgrep-custom.sarif <<'JSON'
{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"Semgrep OSS"}},"results":[
 {"ruleId":"suppressed.rule","suppressions":[{"kind":"inSource"}],
  "locations":[{"physicalLocation":{"artifactLocation":{"uri":"a.go"},"region":{"startLine":7}}}]},
 {"ruleId":"live.rule"},
 {"ruleId":"live.empty","suppressions":[]}]},
 {"tool":{"driver":{"name":"Semgrep OSS"}}}]}
JSON
printf 'not json' > semgrep-community.sarif
cp semgrep-community.sarif bad.orig

out=$(bash -eo pipefail step.sh 2>/dev/null); rc=$?
check "step exits 0" "$rc" "0"
check "ids kept" "$(jq -c '[.runs[0].results[].ruleId]' semgrep-custom.sarif)" '["live.rule","live.empty"]'
check "run without results survives" "$(jq '.runs | length' semgrep-custom.sarif)" "2"
check "counts logged" "$(grep -c -- '3 -> 2' <<<"$out")" "1"
check "dropped result listed" "$(grep -c 'dropped suppressed: suppressed.rule a.go:7' <<<"$out")" "1"
check "bad file warns" "$(grep -c '^::warning::' <<<"$out")" "1"
check "bad file untouched" "$(cmp -s semgrep-community.sarif bad.orig && echo same)" "same"
check "no temp left" "$(ls | grep -c filtered)" "0"
rm -f semgrep-custom.sarif semgrep-community.sarif
check "missing files skipped" "$(bash -eo pipefail step.sh >/dev/null 2>&1; echo $?)" "0"
exit $fail
