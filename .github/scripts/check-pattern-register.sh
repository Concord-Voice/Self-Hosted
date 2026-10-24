#!/usr/bin/env bash
#
# check-pattern-register.sh — warn-only drift check for #921 G7 pattern register.
#
# When invoked by the pre-commit hook on a commit that touches
# .github/workflows/*.yml, scans the staged diff for any added/removed/modified
# `if:` clauses and prints a non-blocking warning prompting the developer to
# consult [internal]workflow-failure-patterns.md before pushing.
#
# Usage:
#   check-pattern-register.sh         # discover modified workflows via git
#   check-pattern-register.sh --help
#
# Exits:
#   0 — always, except on script error
#   2 — script error (invalid args, not in a git repo)
#
# Reference: [internal]specs/2026-05-14-997-pattern-register-design.md

set -euo pipefail

usage() {
  awk 'NR==1{next} /^[^#]/{exit} {sub(/^# ?/,""); print}' "$0"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --help|-h)
      usage
      exit 0
      ;;
    *)
      echo "Unknown arg: $1" >&2
      exit 2
      ;;
  esac
done

# ─── Discover modified workflow files ─────────────────────────────────
# pre-commit invokes us with pass_filenames: false, so we discover the
# staged workflow files ourselves. Empty result → silent exit 0.
MODIFIED_WORKFLOWS=$(git diff --cached --name-only --diff-filter=ACMR \
  -- '.github/workflows/*.yml' '.github/workflows/*.yaml' 2>/dev/null || true)

if [[ -z "$MODIFIED_WORKFLOWS" ]]; then
  exit 0
fi

# ─── Scan each file for if: changes ───────────────────────────────────
# Line marker regex: `^[+-]\s*if:\s` matches added (+) or removed (-)
# `if:` keys in unified-diff output. We use --unified=0 to avoid context
# lines (which would otherwise show as unchanged ` if: ...`).
# Note: bash 3.2 (macOS default) has no associative arrays; we use a
# parallel AFFECTED_LINES array indexed in lock-step with AFFECTED_FILES.
AFFECTED_FILES=()
AFFECTED_LINES=()

while IFS= read -r file; do
  [[ -z "$file" ]] && continue
  diff=$(git diff --cached --unified=0 -- "$file" 2>/dev/null || true)
  if_changes=$(echo "$diff" | grep -E '^[+-][[:space:]]*if:[[:space:]]' || true)
  if [[ -n "$if_changes" ]]; then
    AFFECTED_FILES+=("$file")
    # Extract +/-N line markers from @@ hunks paired with the if: changes.
    # We surface the new-side line numbers from the diff's @@ headers.
    # Line markers are extracted from the @@ hunk header's new-side start
    # position. We don't increment within a hunk: with --unified=0, each
    # `if:` change typically lands in its own hunk, so the simplification
    # is fine in practice. If multiple `if:` lines DO appear in one hunk
    # (rare edge case for tightly-grouped conditional rewrites), they
    # share the hunk-start line number — acceptable for a warn-only nudge.
    line_marks=$(echo "$diff" | awk '
      /^@@/ {
        # @@ -a,b +c,d @@   →   capture +c (new-side start)
        match($0, /\+[0-9]+/);
        new_line = substr($0, RSTART+1, RLENGTH-1);
        next;
      }
      /^[+-][[:space:]]*if:[[:space:]]/ {
        printf "%s%d, ", substr($0,1,1), new_line;
      }
    ' | sed 's/, $//')
    AFFECTED_LINES+=("$line_marks")
  fi
done <<< "$MODIFIED_WORKFLOWS"

if [[ ${#AFFECTED_FILES[@]} -eq 0 ]]; then
  exit 0
fi

# ─── Print warn-only block to stderr ──────────────────────────────────
{
  echo ""
  echo "[warn] workflow-failure pattern register drift check"
  echo ""
  echo "${#AFFECTED_FILES[@]} workflow(s) modify \`if:\` clauses without an inline reference"
  echo "to the register. Before pushing, consult [internal]workflow-failure-patterns.md"
  echo "to check that your changes don't reintroduce a registered pattern (P1–P15)."
  echo ""
  echo "Affected files:"
  local_idx=0
  for f in "${AFFECTED_FILES[@]}"; do
    echo "  $f — \`if:\` clauses changed at ${AFFECTED_LINES[$local_idx]}"
    local_idx=$((local_idx + 1))
  done
  echo ""
  echo "This warning is non-blocking; the commit will proceed."
} >&2

exit 0
