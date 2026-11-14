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
# Usage: check-vitest-pair.sh [root ...]
#   Each root is a directory that holds a package-lock.json. With no argument it
#   checks the three npm roots: . client/admin services/media-plane
#
# Exit 0 = every root has a matched pair. Exit 1 = a root is out of step, or its
# lockfile is missing, unreadable, or does not resolve both packages (a guard
# that cannot see the pair must not pass).

set -uo pipefail

if [ "$#" -gt 0 ]; then
  roots=("$@")
else
  roots=(. client/admin services/media-plane)
fi

PLUGIN='@vitest/coverage-istanbul'
status=0

for root in "${roots[@]}"; do
  lock="$root/package-lock.json"
  if ! pair=$(jq -er --arg p "node_modules/$PLUGIN" \
      '[.packages["node_modules/vitest"].version // "missing", .packages[$p].version // "missing"] | @tsv' \
      "$lock" 2>/dev/null); then
    echo "::error file=$lock::vitest pair: cannot read $lock"
    status=1
    continue
  fi
  IFS=$'\t' read -r vitest plugin <<<"$pair"

  if [ "$vitest" = missing ] || [ "$plugin" = missing ]; then
    echo "::error file=$lock::vitest pair: $root does not resolve both halves (vitest $vitest, $PLUGIN $plugin)."
    status=1
    continue
  fi

  if [ "$vitest" = "$plugin" ]; then
    echo "ok: $root vitest $vitest = $PLUGIN $plugin"
    continue
  fi

  # The lower version is the half that is behind.
  lower=$(printf '%s\n%s\n' "$vitest" "$plugin" | sort -V | head -n1)
  if [ "$lower" = "$vitest" ]; then
    behind=vitest ahead="$plugin"
  else
    behind="$PLUGIN" ahead="$vitest"
  fi
  echo "::error file=$lock::vitest pair out of step in $root: vitest $vitest, $PLUGIN $plugin. $behind is behind."
  echo "  Fix: on this branch, run 'npm install --save-dev --ignore-scripts $behind@$ahead' in $root and commit package.json and package-lock.json."
  echo "  Or close this PR and open one that bumps vitest and $PLUGIN together."
  status=1
done

exit "$status"
