#!/usr/bin/env bash
#
# check-vitest-pair.sh — fail when `vitest` and `@vitest/coverage-istanbul`
# resolve to different versions in an npm lockfile.
#
# Why: the coverage plugin peers on vitest at an exact version. A security
# advisory names one package, so Dependabot's security group can bump one half
# without the other. That PR cannot install and cannot merge, and until this
# check nothing said why (#270). CI may not push to a Dependabot branch, so this
# check fails the PR and tells the reader how to fix it. It reads files only and
# needs no token.
#
# Every copy counts, not only the hoisted one: when a bump crosses a range, npm
# nests the old version under a workspace (packages/x/node_modules/vitest), and
# that copy is out of step too. Versions compare as semver, so 5.1.0 is ahead
# of 5.1.0-beta.1.
#
# Usage: check-vitest-pair.sh [package-lock.json ...]
#   With no argument it checks the three npm roots' lockfiles:
#   package-lock.json, client/admin/package-lock.json and
#   services/media-plane/package-lock.json.
#
# Exit 0 = every lockfile has one version for both packages. Exit 1 = a lockfile
# is out of step, or is missing, unreadable, or does not resolve both packages
# (a guard that cannot see the pair must not pass).

set -uo pipefail

if [ "$#" -gt 0 ]; then
  locks=("$@")
else
  locks=(package-lock.json client/admin/package-lock.json services/media-plane/package-lock.json)
fi

if ! command -v jq >/dev/null 2>&1; then
  echo "::error::vitest pair: jq is not installed"
  exit 1
fi

# Prints a status word on the first line, then the lines to report.
# shellcheck disable=SC2016  # jq program, not shell expansions
PROGRAM='
def ver: (capture("^(?<a>[0-9]+)\\.(?<b>[0-9]+)\\.(?<c>[0-9]+)(-(?<p>[0-9A-Za-z.-]+))?$")
          // error("not a semver version: \(.)"))
  | [(.a | tonumber), (.b | tonumber), (.c | tonumber), (if .p then 0 else 1 end),
     ((.p // "") | split(".") | map(tonumber? // .))];
def show: map("\(.path) \(.v)") | join(", ");
(if length != 1 then error("expected one JSON document, got \(length)") else .[0] end)
| [ .packages | to_entries[]
  | select(.key | test("(^|/)node_modules/(vitest|@vitest/coverage-istanbul)$"))
  | select(.value.link != true)
  | {path: .key, pkg: (.key | sub("^.*node_modules/"; "")), v: (.value.version // "")} ] as $c
| if ($c | map(select(.pkg == "vitest")) | length) == 0
     or ($c | map(select(.pkg != "vitest")) | length) == 0 then
    "missing", "found: \(if $c == [] then "none" else ($c | show) end)"
  elif any($c[]; (.v | type) != "string" or .v == "") then
    "empty", ($c | show)
  elif ($c | map(.v) | unique | length) == 1 then
    "ok", $c[0].v
  else
    ($c | map(.v) | max_by(ver)) as $top
    | "split", $top, ($c[] | select(.v != $top) | "\(.pkg)\t\(.path)\t\(.v)")
  end
'

status=0
for lock in "${locks[@]}"; do
  root=$(dirname "$lock")
  if [ ! -f "$lock" ]; then
    echo "::error file=$lock::vitest pair: $lock does not exist"
    status=1
    continue
  fi
  if ! out=$(jq -r -s "$PROGRAM" "$lock" 2>&1); then
    echo "::error file=$lock::vitest pair: cannot read $lock: $out"
    status=1
    continue
  fi
  kind=$(head -n1 <<<"$out")
  rest=$(tail -n +2 <<<"$out")
  case "$kind" in
    ok)
      echo "ok: $root vitest = @vitest/coverage-istanbul = $rest"
      ;;
    missing)
      echo "::error file=$lock::vitest pair: $root does not resolve both vitest and @vitest/coverage-istanbul ($rest)."
      status=1
      ;;
    empty)
      echo "::error file=$lock::vitest pair: $root has a copy with no version ($rest)."
      status=1
      ;;
    split)
      top=$(head -n1 <<<"$rest")
      echo "::error file=$lock::vitest pair out of step in $root: the newest copy is $top; these copies are behind:"
      while IFS=$'\t' read -r pkg path v; do
        [ -n "$pkg" ] || continue
        # A hoisted copy is fixed at the root, a copy under a workspace
        # (packages/x/node_modules/...) in that workspace. A copy nested under
        # another package moves only when that package or an override does.
        case "$path" in
          node_modules/*/node_modules/*)
            parent=${path%/node_modules/*}
            echo "  $path is $v. Fix: in $root, update ${parent#node_modules/} so it takes $pkg@$top, or add an \"overrides\" entry for $pkg." ;;
          node_modules/*)
            echo "  $path is $v. Fix: in $root, run 'npm install --save-dev --ignore-scripts $pkg@$top'." ;;
          *)
            echo "  $path is $v. Fix: in $root, run 'npm install --save-dev --ignore-scripts $pkg@$top -w ${path%%/node_modules/*}'." ;;
        esac
      done < <(tail -n +2 <<<"$rest")
      echo "  Commit the changed package.json and package-lock.json files on this branch, or close this PR and open one that bumps vitest and @vitest/coverage-istanbul together."
      status=1
      ;;
    *)
      echo "::error file=$lock::vitest pair: unexpected result for $lock: $kind"
      status=1
      ;;
  esac
done

exit "$status"
