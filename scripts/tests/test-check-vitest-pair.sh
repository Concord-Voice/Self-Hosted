#!/usr/bin/env bash
# SC2016: patterns in single quotes are literal, not expansions.
# shellcheck disable=SC2016
#
# Regression test for scripts/ci/check-vitest-pair.sh (#270).
#
# The split-pair fixture must fail with a message that names the root, both
# versions, the copy that is behind and the fix. A matched pair must pass. Every
# copy counts, versions compare as semver, and anything the check cannot read
# must fail rather than pass. Fixtures are named *-lock.json so OSV and the
# dependency graph do not index them as real lockfiles.

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

# Writes $FIX/matched-lock.json with jq filter $2 applied to $WORK/$1/package-lock.json.
variant() { mkdir -p "$WORK/$1"; jq "$2" "$FIX/matched-lock.json" > "$WORK/$1/package-lock.json"; }
VITEST='.packages["node_modules/vitest"]'
PLUGIN='.packages["node_modules/@vitest/coverage-istanbul"]'

# The committed fixtures.
expect 'split pair fails and names root, newest version, lagging copy' 1 \
  "out of step in $FIX: the newest copy is 5\.1\.0.*" "$FIX/split-lock.json"
expect 'split pair names the copy that is behind' 1 '^  node_modules/vitest is 5\.0\.5\.' "$FIX/split-lock.json"
expect 'split pair message gives the fix' 1 \
  "in $FIX, run 'npm install --save-dev --ignore-scripts vitest@5\.1\.0'" "$FIX/split-lock.json"
expect 'matched pair passes' 0 '^ok: .* = 5\.1\.0$' "$FIX/matched-lock.json"
expect 'one split lockfile fails a multi-file run' 1 'vitest is 5\.0\.5' "$FIX/matched-lock.json" "$FIX/split-lock.json"

# Direction and ordering.
variant reverse "$VITEST.version = \"5.2.0\""
expect 'reverse split names the plugin as behind' 1 'node_modules/@vitest/coverage-istanbul is 5\.1\.0' "$WORK/reverse/package-lock.json"
variant numeric "$VITEST.version = \"5.10.0\" | $PLUGIN.version = \"5.9.0\""
expect 'versions compare numerically, not as text' 1 'newest copy is 5\.10\.0.*' "$WORK/numeric/package-lock.json"
variant pre "$VITEST.version = \"5.1.0-beta.1\""
expect 'a release is ahead of its prerelease' 1 'node_modules/vitest is 5\.1\.0-beta\.1\. Fix: .*vitest@5\.1\.0' "$WORK/pre/package-lock.json"
variant prenum "$VITEST.version = \"5.1.0-beta.10\" | $PLUGIN.version = \"5.1.0-beta.9\""
expect 'prerelease numbers compare as numbers' 1 'newest copy is 5\.1\.0-beta\.10;' "$WORK/prenum/package-lock.json"
variant nested ".packages[\"node_modules/foo/node_modules/vitest\"] = {version: \"5.0.5\"}"
expect 'a copy nested under a dependency names that dependency' 1 'update foo so it takes vitest@5\.1\.0, or add an "overrides" entry' "$WORK/nested/package-lock.json"
mkdir -p "$WORK/concat" && cat "$FIX/matched-lock.json" "$FIX/split-lock.json" > "$WORK/concat/package-lock.json"
expect 'a file with two JSON documents fails closed' 1 'expected one JSON document, got 2' "$WORK/concat/package-lock.json"

# Nested copies count, and get the workspace flag in the fix.
variant nested '.packages["packages/protocol/node_modules/vitest"] = {"version": "4.9.0", "dev": true}'
expect 'a nested workspace copy out of step fails' 1 \
  "packages/protocol/node_modules/vitest is 4\.9\.0\. Fix: .*vitest@5\.1\.0 -w packages/protocol'" "$WORK/nested/package-lock.json"
variant link '.packages["packages/protocol/node_modules/vitest"] = {"resolved": "../vitest", "link": true}'
expect 'a link entry is not a copy' 0 '^ok:' "$WORK/link/package-lock.json"

# Fail closed.
variant noplugin "del($PLUGIN)"
expect 'missing plugin fails' 1 'does not resolve both' "$WORK/noplugin/package-lock.json"
variant novitest "del($VITEST)"
expect 'missing vitest fails' 1 'does not resolve both' "$WORK/novitest/package-lock.json"
variant empty "$VITEST.version = \"\" | $PLUGIN.version = \"\""
expect 'empty versions fail' 1 'has a copy with no version' "$WORK/empty/package-lock.json"
variant junk "$VITEST.version = \"latest\" | $PLUGIN.version = \"5.1.0\""
expect 'a non-semver version fails and says why' 1 'cannot read .*not a semver version' "$WORK/junk/package-lock.json"
printf '{"lockfileVersion":3' > "$WORK/truncated.json"
expect 'truncated JSON fails and shows the jq error' 1 'cannot read .*truncated\.json: jq: ' "$WORK/truncated.json"
expect 'missing lockfile fails' 1 'does not exist' "$WORK/absent/package-lock.json"
mkdir -p "$WORK/nojq"; ln -s "$(command -v bash)" "$WORK/nojq/bash"
for tool in dirname head tail; do ln -s "$(command -v "$tool")" "$WORK/nojq/$tool"; done
out=$(PATH="$WORK/nojq" bash "$CHECK" "$FIX/matched-lock.json" 2>&1); got=$?
if [ "$got" -eq 1 ] && grep -q 'jq is not installed' <<<"$out"; then
  echo "ok:   no jq fails and says so"; pass=$((pass + 1))
else
  echo "FAIL: no jq: exit $got"; printf '    %s\n' "$out"; fail=$((fail + 1))
fi

echo "check-vitest-pair: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
