#!/usr/bin/env bash
#
# Regression test for scripts/ci/check-vitest-pair.sh (#270).
#
# The split-pair fixture must fail with a message that names the root, both
# versions, the half that is behind and the fix. A matched pair must pass. A
# lockfile that does not resolve the pair must fail, not pass vacuously.

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK="$HERE/../ci/check-vitest-pair.sh"
FIX="$HERE/fixtures/vitest-pair"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0

expect() {
  local name="$1" want="$2" pat="$3"; shift 3
  local out got
  out=$(bash "$CHECK" "$@" 2>&1); got=$?
  if [ "$got" -ne "$want" ]; then
    echo "FAIL: $name: expected exit $want, got $got"; printf '    %s\n' "$out"; fail=$((fail + 1)); return
  fi
  if [ -n "$pat" ] && ! grep -qE -- "$pat" <<<"$out"; then
    echo "FAIL: $name: output missing /$pat/"; printf '    %s\n' "$out"; fail=$((fail + 1)); return
  fi
  echo "ok:   $name"; pass=$((pass + 1))
}

expect 'split pair fails and names root, versions, lagging half' 1 \
  "out of step in $FIX/split: vitest 5\.0\.5, @vitest/coverage-istanbul 5\.1\.0\. vitest is behind" "$FIX/split"
expect 'split pair message gives the fix' 1 \
  "npm install --save-dev --ignore-scripts vitest@5\.1\.0' in $FIX/split" "$FIX/split"
expect 'matched pair passes' 0 'ok: .* vitest 5\.1\.0 = @vitest/coverage-istanbul 5\.1\.0' "$FIX/matched"
expect 'one split root fails a multi-root run' 1 'vitest is behind' "$FIX/matched" "$FIX/split"

# The reverse split: the plugin is the half that is behind.
mkdir -p "$WORK/reverse"
jq '.packages["node_modules/vitest"].version = "5.2.0"' "$FIX/matched/package-lock.json" > "$WORK/reverse/package-lock.json"
expect 'reverse split names the plugin as behind' 1 '@vitest/coverage-istanbul is behind' "$WORK/reverse"

mkdir -p "$WORK/nopair"
jq 'del(.packages["node_modules/@vitest/coverage-istanbul"])' "$FIX/matched/package-lock.json" > "$WORK/nopair/package-lock.json"
expect 'missing half fails closed' 1 'does not resolve both halves' "$WORK/nopair"
expect 'missing lockfile fails closed' 1 'cannot read' "$WORK/absent"

echo "check-vitest-pair: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
