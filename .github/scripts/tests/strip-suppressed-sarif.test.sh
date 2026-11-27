#!/usr/bin/env bash
#
# strip-suppressed-sarif.test.sh — hermetic self-test for strip-suppressed-sarif.sh.
# Needs only jq. A suppressed result is dropped, an unsuppressed one kept, and
# an unparseable file is left byte-identical.
set -u
ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
SCRIPT="$ROOT/.github/scripts/strip-suppressed-sarif.sh"
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
fail=0
check() { if [ "$2" = "$3" ]; then echo "ok   $1"; else echo "FAIL $1: want '$3' got '$2'"; fail=1; fi; }

cat > "$TMP/a.sarif" <<'JSON'
{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"Semgrep OSS"}},"results":[
 {"ruleId":"suppressed.rule","suppressions":[{"kind":"inSource"}]},
 {"ruleId":"live.rule"},
 {"ruleId":"live.empty","suppressions":[]}]},
 {"tool":{"driver":{"name":"Semgrep OSS"}}}]}
JSON
printf 'not json' > "$TMP/bad.sarif"
cp "$TMP/bad.sarif" "$TMP/bad.orig"

out=$(bash "$SCRIPT" "$TMP/a.sarif" "$TMP/bad.sarif" "$TMP/missing.sarif")
check "ids kept" "$(jq -c '[.runs[0].results[].ruleId]' "$TMP/a.sarif")" '["live.rule","live.empty"]'
check "run without results survives" "$(jq '.runs | length' "$TMP/a.sarif")" "2"
check "counts logged" "$(grep -c -- '3 -> 2' <<<"$out")" "1"
check "bad file warns" "$(grep -c '^::warning::' <<<"$out")" "1"
check "bad file untouched" "$(cmp -s "$TMP/bad.sarif" "$TMP/bad.orig" && echo same)" "same"
check "no temp left" "$(ls "$TMP" | grep -c filtered)" "0"
exit $fail
