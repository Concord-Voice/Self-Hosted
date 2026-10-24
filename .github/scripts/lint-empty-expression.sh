#!/usr/bin/env bash
#
# lint-empty-expression.sh — flag empty `${{ }}` tokens and GHA-style
# `${{ ... }}` tokens hidden inside bash `#` comments within `run:` blocks.
# Root cause of #876: GitHub Actions' expression parser scans ALL `run:`
# content for `${{ ... }}` substrings — including those inside bash
# comments. An empty `${{ }}` token there fails dispatch with the cryptic
# "An expression was expected (...)" error at a misleading position.
# Pattern P9 from spec:
# [internal]specs/2026-05-14-921-workflow-validation-sandbox-design.md §5.2 / §6.4
#
# Scope: ONLY content inside `run:` blocks. Outside run: blocks the GHA
# expression evaluator does not scan bash comments, so a stray `${{ ... }}`
# in a YAML comment (e.g. `# uses runs-on: ${{ matrix.os }}` documentation
# in build.yml:334) is harmless and must not false-positive. The
# scope-restriction is intentional and matches the spec wording.
#
# Usage:
#   lint-empty-expression.sh <file.yml-or-dir/>
#   lint-empty-expression.sh --help
#
# Exits:
#   0 — no empty `${{ }}` and no `${{ ... }}` in bash comments inside run blocks
#   1 — at least one bug pattern detected
#   2 — script error (invalid args, missing file, etc.)

set -euo pipefail

usage() {
  awk 'NR==1{next} /^[^#]/{exit} {sub(/^# ?/,""); print}' "$0"
}

# ─── Argument parsing ─────────────────────────────────────────────────
TARGET=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --help|-h)
      usage
      exit 0
      ;;
    -*)
      echo "ERROR: unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
    *)
      if [[ -n "$TARGET" ]]; then
        echo "ERROR: only one target path may be passed (got '$TARGET' and '$1')" >&2
        exit 2
      fi
      TARGET="$1"
      shift
      ;;
  esac
done

if [[ -z "$TARGET" ]]; then
  echo "ERROR: a target file or directory is required" >&2
  usage >&2
  exit 2
fi

if [[ ! -e "$TARGET" ]]; then
  echo "ERROR: target does not exist: $TARGET" >&2
  exit 2
fi

command -v awk >/dev/null || { echo "ERROR: awk not found" >&2; exit 2; }

# ─── Collect files to scan ────────────────────────────────────────────
# Symlinks rejected (same defense as sibling lints).
FILES=()
if [[ -d "$TARGET" ]]; then
  while IFS= read -r f; do
    FILES+=("$f")
  done < <(find "$TARGET" -type f -not -type l \( -name '*.yml' -o -name '*.yaml' \) | sort)
else
  FILES+=("$TARGET")
fi

if [[ "${#FILES[@]}" -eq 0 ]]; then
  echo "ERROR: no YAML files found under $TARGET" >&2
  exit 2
fi

# ─── Scan helper ──────────────────────────────────────────────────────
# State machine: track whether the current line is inside a YAML `run:`
# block. A multi-line `run: |` (or `run: >`, with optional chomping
# indicator) starts the block; subsequent lines indented MORE than the
# `run:` key's column are part of the block. When indent drops back to
# or below the run: key's column, the block ends. Single-line `run: <txt>`
# (content on same line) is scanned in-place without entering block state.
#
# Inside a run block we flag two patterns:
#   1. Empty `${{ }}` — including `${{   }}` (whitespace-only)
#   2. `${{ ... }}` inside a bash `#` comment — line-leading `#` (after
#      any whitespace) OR `#` preceded by whitespace (inline trailing
#      bash comment). Quoted-string `#` inside `echo "literal # ${{ ..."
#      is not bash-syntax-distinguished by regex; accepting that small
#      false-positive risk is consistent with the spec's "regex-scan"
#      framing. None of today's workflows exercise that pattern.
scan_file() {
  local file="$1"

  awk -v filename="$file" '
    BEGIN {
      in_run = 0
      run_indent = -1
      fail = 0
    }

    {
      raw = $0
      is_blank = (raw ~ /^[[:space:]]*$/)
      is_yaml_comment = (raw ~ /^[[:space:]]*#/)

      # Compute leading-whitespace count (column of first non-space char,
      # 0-indexed). For blank lines, leave run-block state unchanged.
      if (!is_blank) {
        match(raw, /[^[:space:]]/)
        leading = RSTART - 1
      } else {
        leading = -2  # neutral sentinel
      }

      # Exit run-block if indent drops back to or below the run: key.
      # A YAML comment at or below the run-key indent is OUTSIDE the run
      # block (the `#` belongs to YAML, not bash) — exiting before scan
      # avoids false-positives on doc-style comments like
      # `# uses runs-on: ${{ matrix.os }}` in build.yml:334.
      if (in_run && !is_blank && leading <= run_indent) {
        in_run = 0
        run_indent = -1
      }

      # Inside a run block: scan for both bug patterns.
      if (in_run && !is_blank) {
        # Pattern 1: empty ${{ }} (with optional whitespace inside).
        if (raw ~ /\$\{\{[[:space:]]*\}\}/) {
          printf "%s:%d: empty ${{ }} token in run: block (GHA parser fails dispatch with \"An expression was expected\")\n", filename, NR > "/dev/stderr"
          fail = 1
        }
        # Pattern 2: ${{ ... }} inside a bash comment.
        # The bash-comment marker is `#` at start of line (post-whitespace)
        # OR `#` preceded by whitespace mid-line. The regex (^|[[:space:]])#
        # is anchored to avoid matching `#` inside identifiers like `foo#bar`.
        if (raw ~ /(^|[[:space:]])#[^\n]*\$\{\{/) {
          printf "%s:%d: ${{ }} token inside bash comment (GHA parser still scans it; will fail dispatch)\n", filename, NR > "/dev/stderr"
          fail = 1
        }
      }

      # Detect entry into a run: block on this line (after any exit above).
      # Skip YAML comments to avoid mis-classifying a commented-out run:
      # line as an active block.
      if (!is_yaml_comment) {
        # Multi-line form: run: with optional block-scalar indicator (|, >,
        # |+, |-, |1, |2, |-1, etc.) and nothing else through end-of-line.
        if (raw ~ /^[[:space:]]*-?[[:space:]]*run:[[:space:]]*([|>][+\-]?[0-9]*)?[[:space:]]*$/) {
          # Indent = column of first non-whitespace char (0-indexed).
          # For a list-item step like `      - run: |`, the `-` is the
          # first non-whitespace char; YAML considers the key sibling-
          # comparison at that column, so this is correct for detecting
          # sibling-step exit.
          match(raw, /[^[:space:]]/)
          run_indent = RSTART - 1
          in_run = 1
        }
        # Single-line form: `run: <content>` where <content> is not a
        # block-scalar indicator. Scan the content portion in place; do
        # NOT enter in_run state.
        else if (match(raw, /^[[:space:]]*-?[[:space:]]*run:[[:space:]]+/)) {
          content = substr(raw, RSTART + RLENGTH)
          if (content ~ /\$\{\{[[:space:]]*\}\}/) {
            printf "%s:%d: empty ${{ }} token in single-line run: (GHA parser fails dispatch)\n", filename, NR > "/dev/stderr"
            fail = 1
          }
          if (content ~ /(^|[[:space:]])#[^\n]*\$\{\{/) {
            printf "%s:%d: ${{ }} token inside bash comment of single-line run:\n", filename, NR > "/dev/stderr"
            fail = 1
          }
        }
      }
    }

    END {
      exit fail
    }
  ' "$file"
}

# ─── Drive the scan, aggregating exit status ──────────────────────────
overall_fail=0
for f in "${FILES[@]}"; do
  if ! scan_file "$f"; then
    overall_fail=1
  fi
done

if [[ "$overall_fail" -ne 0 ]]; then
  echo "" >&2
  echo "ERROR: empty \${{ }} tokens or \${{ ... }} inside bash comments in run: blocks." >&2
  echo "       GHA's expression parser scans ALL run: content; the bad tokens fail dispatch with cryptic" >&2
  echo "       \"An expression was expected\" errors. Either complete the expression or remove the token." >&2
  exit 1
fi

exit 0
