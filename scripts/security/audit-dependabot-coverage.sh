#!/usr/bin/env bash
#
# Audit .github/dependabot.yml coverage against the tree, per ecosystem.
#
# This exists because the audit it replaces was a shell snippet inside
# [internal]rules/deployment.md, and a snippet in prose is not a control: it
# accumulated FIVE defects in two PRs, every one of them found by a reviewer
# rather than by running it, and every one of them the same shape — a claim of
# complete coverage whose mechanism could only ever have seen part of the tree.
#
#   1. bare `go.mod` / `requirements*.txt` pathspecs, anchored at the repo root,
#      silently dropping services/control-plane and .github (Codex, #3284)
#   2. three of the five watched ecosystems enumerated at all (#3284)
#   3. nested helper manifests reported as uncovered update roots (#3284)
#   4. a `package-lock.json` test that drops a dependency-bearing LOCKLESS root
#      — right only by coincidence (Codex #3284, fixed #3292)
#   5. `&&` on jq's exit code, so an UNREADABLE manifest was indistinguishable
#      from a manifest that declares nothing (CodeRabbit, #3325)
#
# Each of those is now a case in test-audit-dependabot-coverage.sh.
#
# Exit codes are the contract:
#   0  every ecosystem's tree set equals what dependabot.yml watches
#   1  drift — an uncovered update root, or a watched directory not in the tree
#   2  the audit could not complete, so NO conclusion may be drawn from it
#
# Usage: audit-dependabot-coverage.sh [--root <dir>]
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LINT="$SCRIPT_DIR/check-pin-exceptions.mjs"
ROOT=""

while [ $# -gt 0 ]; do
  case "$1" in
    --root) ROOT="${2:-}"; shift 2 || true ;;
    -h|--help) sed -n '3,30p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) printf 'unknown argument: %s\n' "$1" >&2; exit 2 ;;
  esac
done

if [ -z "$ROOT" ]; then
  ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || {
    printf 'AUDIT INCOMPLETE: not a git repository and no --root given\n' >&2
    exit 2
  }
fi
DB="$ROOT/.github/dependabot.yml"

# Fail closed on an absent config rather than comparing against an empty set:
# every tree root would read as uncovered, or — worse, if the tree enumeration
# also came back empty — the audit would report clean coverage of nothing.
if [ ! -f "$DB" ]; then
  printf 'AUDIT INCOMPLETE: dependabot config not found at %s\n' "$DB" >&2
  exit 2
fi

# NOTE: there is no separate syntax-check step here, and its absence is the fix
# rather than a gap. The declared-directory parser parses with a REAL YAML parser
# and then validates the SHAPE it reads, so both happen on the path that cannot be
# skipped. The earlier version syntax-checked separately and CONTINUED when the
# validator was unavailable, which let a run reach AUDIT CLEAN without establishing
# anything at all (CodeRabbit, PR #3325).
#
# What that buys, stated exactly, because the first version of this note claimed
# more: YAML syntax, plus the fields these controls read — `updates:` is a
# sequence of mappings, each names a `package-ecosystem` and at least one of
# `directory`/`directories`, and an `ignore:` is a sequence of mappings. It is NOT
# Dependabot's own schema validator. An unknown ecosystem name or a malformed
# `schedule:` passes here and may still be rejected by Dependabot. The earlier
# note said parsing established "that Dependabot can load the file", and the gap
# was a fail-OPEN: `ignore: not-a-list` is valid YAML, so the ignore rule vanished
# and the pin lint reported every obligation accounted for (CodeRabbit again).

status=0     # 0 clean · 1 drift · 2 incomplete. Only ever ratchets upward.
raise() { [ "$status" -ge "$1" ] || status="$1"; }

TMPD="$(mktemp -d)"
trap 'rm -rf "$TMPD"' EXIT

ECOSYSTEMS="npm gomod pip docker docker-compose github-actions"

# Directories each dependabot block watches, parsed by the SAME module the
# pin-exception lint uses. A second parser here would be a second thing to keep
# correct, and this repo has already shipped a fix to one copy of a pair while
# leaving the other wrong (#3284, twice).
#
# ONE node invocation for all six ecosystems, not one each: this runs as a
# pre-commit hook on every manifest change, and on macOS the dominant cost is
# process spawn, not work.
parse_declared() {
  node -e "
    import('$LINT').then((m) => {
      const fs = require('node:fs');
      const yaml = fs.readFileSync('$DB', 'utf8');
      for (const eco of '$ECOSYSTEMS'.split(' ')) {
        for (const d of m.ecosystemDirectories(yaml, eco)) {
          fs.writeFileSync('$TMPD/' + eco + '.declared', (d === '' ? '.' : d) + '\n', { flag: 'a' });
        }
        fs.appendFileSync('$TMPD/' + eco + '.declared', '');
      }
    }).catch((e) => { console.error(String(e)); process.exit(1); });
  "
}

# `git ls-files` is the filter, deliberately, and not a find+exclusion list: an
# untracked scratch manifest is not deployable surface and must not become a
# coverage gap. EVERY pattern needs its leading `*` — a git pathspec without one
# is anchored at the repository root, which is defect 1 above.
#
# `git -C` changes the DIRECTORY, not the repository. Git exports GIT_DIR and
# GIT_INDEX_FILE to every hook it runs, and those win over cwd — so under a
# pre-commit hook this enumerated the OUTER repository's index while $ROOT
# pointed somewhere else entirely, and every path it returned was then unreadable
# relative to $ROOT. The audit fails closed on that (exit 2, which is how it was
# caught) but it was auditing the wrong tree. Clear the inherited environment so
# $ROOT means what it says.
# Writes the file list to $1 and returns non-zero if git itself failed. The
# earlier form substituted straight into `done < <(tracked ...)`, where a failing
# `git ls-files` yields an empty list and a SUCCESSFUL loop — so a broken index or
# an unreadable repository produced "0 tree roots", which compares as clean drift
# or as every watched directory being stale. Same shape as the jq defect: an error
# read as an answer (CodeRabbit, PR #3325).
tracked_to() {
  local out="$1"; shift
  if ! env -u GIT_DIR -u GIT_WORK_TREE -u GIT_INDEX_FILE \
           -u GIT_OBJECT_DIRECTORY -u GIT_ALTERNATE_OBJECT_DIRECTORIES \
           git -C "$ROOT" ls-files "$@" >"$out" 2>/dev/null; then
    printf 'AUDIT ERROR: git ls-files failed in %s for: %s\n' "$ROOT" "$*" >&2
    raise 2
    return 1
  fi
}

# Map a file to the directory a dependabot block would name. Root is `.`.
dirof() { local d; d="$(dirname "$1")"; [ "$d" = "." ] && printf '.\n' || printf '%s\n' "$d"; }

# ---------------------------------------------------------------------------
# Tree enumeration, one function per ecosystem. Each prints directory units.
# ---------------------------------------------------------------------------

# npm: an update root is a manifest that DECLARES dependencies. Discriminate on
# the dependency sections, never on a neighbouring package-lock.json — a
# dependency-bearing lockless manifest is a real update root (npm resolves from
# the manifest alone) and the lockfile test drops it silently. Defect 4.
tree_npm() {
  local f ls="$TMPD/npm.ls"
  tracked_to "$ls" '*package.json' || return 1
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    jq -e '(.dependencies//{}) + (.devDependencies//{}) + (.optionalDependencies//{})
           | length > 0' "$ROOT/$f" >/dev/null 2>&1
    # The `case` on jq's exit code is the fail-closed part — do not collapse it
    # back to `&&`. jq -e returns 1 for a valid manifest whose predicate is
    # false, 5 for malformed JSON and 2 for an unreadable file (measured). `&&`
    # treats all three alike, so a real update root with a broken manifest is
    # omitted exactly as a helper package is. Defect 5.
    case $? in
      0) dirof "$f" ;;
      1) ;;
      *) printf 'AUDIT ERROR: cannot read %s\n' "$f" >&2; raise 2 ;;
    esac
  done <"$ls"
}

tree_gomod() {
  local f ls="$TMPD/gomod.ls"
  tracked_to "$ls" '*go.mod' || return 1
  while IFS= read -r f; do [ -n "$f" ] && dirof "$f"; done <"$ls"
}
tree_pip() {
  local f ls="$TMPD/pip.ls"
  tracked_to "$ls" '*requirements*.txt' || return 1
  while IFS= read -r f; do [ -n "$f" ] && dirof "$f"; done <"$ls"
}

# docker: the unit is the DIRECTORY, so one directory holding two Dockerfiles is
# one unit. The `:(exclude)*/artifacts/**` is load-bearing: the agent-tooling
# tree carries recorded measurement inputs whose base image must NOT be bumped.
# The pathspec is prefix-free on purpose — sync-codex-tooling.py rewrites the
# agent-directory prefix wholesale when it mirrors the rule file that documents
# this, and an exclusion spelling that directory out is rewritten to a sibling
# that does not exist, leaving the exclusion inert (shipped once; #3284).
tree_docker() {
  local f ls="$TMPD/docker.ls"
  tracked_to "$ls" '*Dockerfile*' ':(exclude)*/artifacts/**' || return 1
  while IFS= read -r f; do [ -n "$f" ] && dirof "$f"; done <"$ls"
}

# docker-compose: a unit is a DIRECTORY holding a tracked Compose file that
# digest-pins an image. The updater scans every compose file in the directory a
# block names, so the directory is the unit, as for docker. Only digest pins
# count, because a pin nobody refreshes is the failure this leg exists to catch:
# a floating tag, `latest` or a local build tag (concordvoice-postgres) gives
# Dependabot nothing to move. WHICH images a block updates is its `allow:` list;
# this audit checks directories, nothing finer.
tree_docker_compose() {
  local f ls="$TMPD/docker-compose.ls"
  tracked_to "$ls" '*compose*.yml' '*compose*.yaml' ':(exclude)*/artifacts/**' || return 1
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    grep -Eq '^[[:space:]]*image:[^#]*@sha256:[0-9a-f]{64}' "$ROOT/$f"
    # Same fail-closed `case` as tree_npm: grep -q returns 1 for "no pin here"
    # and 2 for a file it cannot read, and only the first may drop the file.
    case $? in
      0) dirof "$f" ;;
      1) ;;
      *) printf 'AUDIT ERROR: cannot read %s\n' "$f" >&2; raise 2 ;;
    esac
  done <"$ls"
}

# github-actions: also directory-unit, and the two units are NOT interchangeable.
# `directory: "/"` covers .github/workflows; a composite action's own SHA-pinned
# `uses:` live in its action.yml and are invisible to that block, which is why
# .github/actions/track-deployment must be named explicitly (#3284 audit).
tree_github_actions() {
  local f wf="$TMPD/gha-workflows.ls" ac="$TMPD/gha-actions.ls"
  tracked_to "$wf" '.github/workflows/*.y*ml' || return 1
  [ -s "$wf" ] && printf '.\n'
  tracked_to "$ac" '.github/actions/**/action.y*ml' '.github/actions/*/action.y*ml' || return 1
  while IFS= read -r f; do [ -n "$f" ] && dirof "$f"; done <"$ac"
}

# ---------------------------------------------------------------------------
# Deliberate exclusions: a tree unit that is knowingly unwatched. Each one is
# subtracted from the tree set AND printed, because an exclusion nobody can see
# is indistinguishable from a gap nobody noticed.
# ---------------------------------------------------------------------------
excluded_docker="infrastructure/docker/minio" # #1220 fixed-upstream-source discipline

# Multi-line on purpose: Semgrep's bash parser fails on the one-line form, and
# the protected scan treats any partial parse of a changed file as a failure.
exclusions_for() {
  case "$1" in
    docker) printf '%s\n' "$excluded_docker" ;;
    *) : ;;
  esac
}

# ---------------------------------------------------------------------------
audit_one() {
  local eco="$1" treefn="$2" n_excl=0
  local t="$TMPD/$eco.tree" d="$TMPD/$eco.declared" x="$TMPD/$eco.excl" raw="$TMPD/$eco.raw"

  # A plain redirect, NOT a pipeline: `"$treefn" | sort` would run the tree
  # function in a subshell, where its `raise 2` mutates a copy of $status and
  # the fail-closed signal is lost on return. That is defect 5 one layer up.
  local before="$status"
  "$treefn" >"$raw"
  # Whether THIS ecosystem hit a read error, not merely whether the run did. The
  # summary line used to read "npm 5 tree · 5 watched — OK" while one npm manifest
  # was unreadable: the overall AUDIT INCOMPLETE was correct, but the per-ecosystem
  # line asserted a clean set comparison the audit had not performed.
  local incomplete=0
  [ "$status" = "$before" ] || incomplete=1
  sed '/^$/d' "$raw" | sort -u >"$t"

  [ -f "$d" ] || : >"$d"
  sort -u "$d" -o "$d"
  exclusions_for "$eco" | sed '/^$/d' | sort -u >"$x"

  if [ -s "$x" ]; then
    n_excl="$(comm -12 "$t" "$x" | wc -l | tr -d ' ')"
    comm -23 "$t" "$x" >"$t.keep" && mv "$t.keep" "$t"
  fi

  local uncovered stale
  uncovered="$(comm -23 "$t" "$d")"
  stale="$(comm -13 "$t" "$d")"

  printf '%-15s %2d tree · %2d watched' "$eco" "$(wc -l <"$t" | tr -d ' ')" "$(wc -l <"$d" | tr -d ' ')"
  [ "$n_excl" -gt 0 ] && printf ' · %d deliberate exclusion(s)' "$n_excl"
  if [ "$incomplete" -eq 1 ]; then
    printf ' — INCOMPLETE (a source could not be read; this set is not the tree)\n'
    return
  fi
  if [ -z "$uncovered" ] && [ -z "$stale" ]; then
    printf ' — OK\n'
  else
    printf ' — DRIFT\n'
    # One printf per entry. `printf 'fmt' "$eco" $list` recycles the format
    # string, so with two or more entries the SECOND line takes a directory as
    # its ecosystem name and the last one prints an empty value — a drift report
    # that misnames the thing that drifted.
    local d
    for d in $uncovered; do printf '  UNCOVERED %s: %s\n' "$eco" "$d"; done
    for d in $stale; do printf '  STALE     %s: %s (watched, absent from the tree)\n' "$eco" "$d"; done
    raise 1
  fi
}

# Parse first: a config this cannot read is an incomplete audit, not an empty
# watch list, and an empty watch list would report every root as uncovered.
if ! parse_declared; then
  printf 'AUDIT INCOMPLETE: could not parse update blocks from %s\n' "$DB" >&2
  exit 2
fi

# Dependabot's `directories:` accepts GLOBS (`/packages/*`). The parser stores the
# pattern literally, so a set comparison would call the pattern uncovered and every
# directory it actually matches stale — drift reported over a config with none.
# Refuse to compare rather than expand: an expander that disagrees with
# Dependabot's own matching would be a new way to be confidently wrong, and this
# repository's config uses no globs today (CodeRabbit, PR #3325).
if grep -lq '[*?[]' "$TMPD"/*.declared 2>/dev/null; then
  grep -h '[*?[]' "$TMPD"/*.declared 2>/dev/null | sed 's/^/  glob directory: /' >&2
  printf 'AUDIT INCOMPLETE: %s declares a directory GLOB, which this audit compares literally rather than expanding.\n' "$DB" >&2
  exit 2
fi

printf 'dependabot coverage audit · %s\n' "$ROOT"
audit_one npm            tree_npm
audit_one gomod          tree_gomod
audit_one pip            tree_pip
audit_one docker         tree_docker
audit_one docker-compose tree_docker_compose
audit_one github-actions tree_github_actions
printf 'deliberate exclusion · docker · %s (#1220 fixed-upstream-source discipline)\n' "$excluded_docker"

case "$status" in
  0) printf 'AUDIT CLEAN: every watched ecosystem matches the tree.\n' ;;
  1) printf 'AUDIT DRIFT: see UNCOVERED/STALE above.\n' >&2 ;;
  2) printf 'AUDIT INCOMPLETE: the audit could not read everything it enumerates; do not treat its output as complete coverage.\n' >&2 ;;
esac
exit "$status"
