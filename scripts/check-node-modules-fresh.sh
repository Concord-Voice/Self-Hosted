#!/bin/bash
# check-node-modules-fresh.sh — verify node_modules is installed and current.
#
# Exits non-zero (with a friendly message) if the given directory's
# node_modules is missing, lacks the npm install marker, or is older than
# its package-lock.json. This is the pre-commit guard that surfaces "Run
# npm install" instead of letting tools like tsc, eslint, or prettier fail
# with confusing "Cannot find module" errors after a pull picks up
# dependency changes.
#
# Usage: check-node-modules-fresh.sh <dir> [<dir>...]

set -eu

RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m'

if [ "$#" -eq 0 ]; then
  echo "Usage: $0 <dir> [<dir>...]" >&2
  exit 2
fi

fail=0

for dir in "$@"; do
  # Guard: a mistyped dir arg must not silently pass the freshness check.
  # Distinguish "dir doesn't exist" (config error) from "no lockfile in dir"
  # (legitimate — not every project has npm deps).
  if [ ! -d "$dir" ]; then
    echo -e "${RED}✗ $dir does not exist.${NC}" >&2
    fail=1
    continue
  fi

  if [ ! -f "$dir/package-lock.json" ]; then
    continue
  fi

  if [ ! -d "$dir/node_modules" ]; then
    echo -e "${RED}✗ $dir/node_modules is missing.${NC}" >&2
    echo -e "  ${YELLOW}Run:${NC} (cd $dir && npm install)" >&2
    fail=1
    continue
  fi

  # npm (>=7) writes this marker on every install; its absence means either
  # an ancient npm or a manual/partial node_modules copy — in both cases
  # we cannot prove node_modules matches the lockfile, so treat as stale.
  marker="$dir/node_modules/.package-lock.json"
  if [ ! -f "$marker" ]; then
    echo -e "${RED}✗ $dir/node_modules/.package-lock.json marker missing — freshness cannot be verified.${NC}" >&2
    echo -e "  ${YELLOW}Run:${NC} (cd $dir && npm install)" >&2
    fail=1
    continue
  fi

  if [ "$dir/package-lock.json" -nt "$marker" ]; then
    echo -e "${RED}✗ $dir/node_modules is stale (package-lock.json is newer).${NC}" >&2
    echo -e "  ${YELLOW}Run:${NC} (cd $dir && npm install)" >&2
    fail=1
  fi
done

exit $fail
