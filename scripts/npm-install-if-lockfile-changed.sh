#!/bin/bash
# npm-install-if-lockfile-changed.sh — auto-sync node_modules on pull/checkout.
#
# Runs `npm ci` in each specified directory whose package-lock.json
# differs between the previous HEAD and the current HEAD. Designed as a
# pre-commit post-merge / post-checkout hook so dependency updates are picked
# up the moment they land in the working tree. `npm ci` installs exactly what
# the lockfile pins, never rewrites it, and fails if package.json and the
# lockfile disagree.
#
# pre-commit stage env vars:
#   post-checkout: PRE_COMMIT_FROM_REF, PRE_COMMIT_TO_REF, PRE_COMMIT_CHECKOUT_TYPE
#                  (CHECKOUT_TYPE is "1" for branch switch, "0" for file
#                  checkout; absent outside pre-commit — defaults to "1")
#   post-merge:    PRE_COMMIT_IS_SQUASH_MERGE
#                  (no from/to refs — fall back to HEAD@{1} then ORIG_HEAD)
#
# Usage: npm-install-if-lockfile-changed.sh <dir> [<dir>...]

set -eu

# git exports GIT_DIR and GIT_INDEX_FILE to its hooks, and in a linked worktree
# GIT_DIR is .git/worktrees/<name>. npm fetches a git dependency with its own
# `git clone` and `git init` in a cache directory, and an inherited GIT_DIR
# points those at THIS repository instead: `git init` rewrites core.bare=true in
# the shared config, and the main checkout then reads as bare. The git calls
# below find the repository from the working directory, as githooks(5)
# recommends. check-hook-commands.sh clears the same variables for the same
# reason.
# shellcheck disable=SC2046 # the list is meant to word-split
unset $(git rev-parse --local-env-vars)

RED='\033[0;31m'
YELLOW='\033[1;33m'
GREEN='\033[0;32m'
NC='\033[0m'

if [ "$#" -eq 0 ]; then
  echo "Usage: $0 <dir> [<dir>...]" >&2
  exit 2
fi

# Skip file-only checkouts (e.g. `git checkout -- file`) — no branch move.
if [ "${PRE_COMMIT_CHECKOUT_TYPE:-1}" = "0" ]; then
  exit 0
fi

# Require npm early with a friendly message. The hook fires on every pull /
# merge / branch switch; a bare `npm: command not found` would be noisy and
# unhelpful for contributors without a local Node toolchain.
if ! command -v npm >/dev/null 2>&1; then
  echo -e "${YELLOW}⚠️  \`npm\` not on PATH — skipping auto-install hook.${NC}" >&2
  echo -e "   Install Node.js, then run \`npm ci\` manually when ready." >&2
  exit 0
fi

# Determine refs to diff. Prefer pre-commit-supplied refs (post-checkout case).
# Fall back to HEAD@{1} (previous HEAD from reflog — git always writes this on
# merge/checkout) before ORIG_HEAD, which is only set by `git merge` and can be
# stale or unset on fast-forward pulls with older git versions.
from_ref="${PRE_COMMIT_FROM_REF:-}"
to_ref="${PRE_COMMIT_TO_REF:-HEAD}"
if [ -z "$from_ref" ]; then
  if git rev-parse --verify --quiet 'HEAD@{1}' >/dev/null; then
    from_ref='HEAD@{1}'
  elif git rev-parse --verify --quiet ORIG_HEAD >/dev/null; then
    from_ref='ORIG_HEAD'
  else
    # First commit on a fresh clone — no prior HEAD to diff against.
    exit 0
  fi
fi

# Validate both refs resolve to commits. `rev-parse --verify` alone accepts
# the all-zero post-checkout sentinel, which is not an object Git can diff.
# Unresolvable refs bail quietly; the desktop pre-commit freshness check can
# catch a later TypeScript commit if its node_modules ends up stale.
if ! git rev-parse --verify --quiet "${from_ref}^{commit}" >/dev/null; then exit 0; fi
if ! git rev-parse --verify --quiet "${to_ref}^{commit}" >/dev/null; then exit 0; fi

# No movement — nothing to diff.
if [ "$(git rev-parse "$from_ref")" = "$(git rev-parse "$to_ref")" ]; then
  exit 0
fi

for dir in "$@"; do
  lockfile="$dir/package-lock.json"
  if [ ! -f "$lockfile" ]; then
    continue
  fi

  # Distinguish git-diff exit codes explicitly:
  #   0  = no change     → skip
  #   1  = differs       → install
  #   >1 = fatal error   → propagate. The `if !` idiom would have swallowed
  #                        these as "no change" under set -e, silently skipping
  #                        the install on corrupt repos / bad pathspec — the
  #                        exact scenario this hook is supposed to heal.
  set +e
  git diff --quiet "$from_ref" "$to_ref" -- "$lockfile"
  rc=$?
  set -e
  case $rc in
    0) continue ;;
    1) ;;  # lockfile differs — proceed to install
    *) echo -e "${RED}✗ git diff failed (rc=$rc) for $lockfile — aborting hook.${NC}" >&2
       exit "$rc" ;;
  esac

  echo -e "${YELLOW}📦 $lockfile changed — running \`npm ci\` in $dir...${NC}"
  if ! (cd "$dir" && npm ci --no-audit --no-fund); then
    echo -e "${RED}✗ npm ci failed in $dir.${NC}" >&2
    exit 1
  fi

  echo -e "${GREEN}✓ npm ci complete in $dir${NC}"
done
