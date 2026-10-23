#!/usr/bin/env bash
#
# lint-always-needs-comment.sh — enforce Invariant 8 of the desktop-build
# pipeline spec: every use of GHA `always()` (in `if:` keys or expressions)
# must be accompanied by a nearby inline comment explaining why it is
# load-bearing. Background: needs-cascade propagation of `always()` caused
# incidents #906 and #910 — codifying "always() always has a justification
# comment" prevents the next surprise.
#
# Usage:
#   lint-always-needs-comment.sh <file.yml-or-dir/> [--window N]
#   lint-always-needs-comment.sh --help
#
# Exits:
#   0 — every `always()` occurrence has an explanatory comment within the
#       look-back window (default N=10 lines, inclusive of the always()
#       line itself for inline comments)
#   1 — at least one `always()` occurrence lacks an adjacent comment with
#       a relevant keyword (always / load-bearing / cascade / skip)
#   2 — script error (invalid args, missing file, unreadable directory)
#
# Configuration:
#   --window N             override the default 10-line look-back distance
#   ALWAYS_COMMENT_WINDOW  env-var equivalent of --window (flag wins on
#                          conflict)
#
# Keyword regex: case-insensitive match of (always|load-bearing|cascade|skip)
# anywhere in a `#` comment line on or above the always() line within the
# window. Only YAML comment lines (those whose first non-whitespace char is
# `#`) and inline trailing comments on the always() line itself count.
#
# Reference: [internal]specs/2026-05-11-920-desktop-build-pipeline-design.md (Invariant 8)
#            [internal]plans/2026-05-11-920-desktop-build-pipeline.md §5.3

set -euo pipefail

usage() {
  awk 'NR==1{next} /^[^#]/{exit} {sub(/^# ?/,""); print}' "$0"
}

# ─── Argument parsing ─────────────────────────────────────────────────
TARGET=""
WINDOW="${ALWAYS_COMMENT_WINDOW:-10}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --window)
      WINDOW="${2:-}"
      shift 2
      ;;
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

# Validate WINDOW is a positive integer.
if ! [[ "$WINDOW" =~ ^[0-9]+$ ]] || [[ "$WINDOW" -lt 1 ]]; then
  echo "ERROR: --window must be a positive integer (got: $WINDOW)" >&2
  exit 2
fi

# Pre-flight: tools we depend on (grep/awk/sed are POSIX, no jq/yq needed
# since this is pure text-pattern scanning rather than YAML structure).
command -v awk  >/dev/null || { echo "ERROR: awk not found" >&2;  exit 2; }
command -v grep >/dev/null || { echo "ERROR: grep not found" >&2; exit 2; }

# ─── Collect files to scan ────────────────────────────────────────────
# `-not -type l` rejects symlinks so a malicious checkout cannot route
# the scanner to a file outside .github/workflows/ (e.g. ~/.aws/config).
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

# ─── Scan helper (one file, all always() occurrences) ─────────────────
# awk variables:
#   window  — look-back distance (inclusive of the always() line itself)
#   filename— file being scanned (for stderr reporting)
#
# Algorithm:
#   For each line, retain a sliding history of recent comment lines (only
#   YAML comment lines: leading whitespace + `#`). When a non-comment line
#   contains `always(`, check whether any retained comment line within
#   [current - window + 1, current] satisfies the keyword regex. The
#   always() line itself is included so inline trailing comments
#   (e.g. `if: ${{ always() }}  # always(): load-bearing`) count as proof.
scan_file() {
  local file="$1"
  local window="$2"

  awk -v window="$window" -v filename="$file" '
    BEGIN {
      # case-insensitive keyword match; awk handles only basic regex so
      # we encode the case fold by listing both cases per character class.
      # The `skip` keyword requires a `-` or `:` suffix (e.g. `skip:`,
      # `skip-reason:`, `skip-condition:`) so unrelated mentions like
      # "# skipped on forks" or "# skip empty matrix" do NOT count as
      # justification for an always() use. The plain word "skip" is too
      # broad — it appears in many CI contexts unrelated to always()
      # cascading behavior, and accepting it would let pre-existing
      # neighboring comments accidentally satisfy the lint.
      KEYWORD_RE = "([Aa][Ll][Ww][Aa][Yy][Ss])|([Ll][Oo][Aa][Dd]-[Bb][Ee][Aa][Rr][Ii][Nn][Gg])|([Cc][Aa][Ss][Cc][Aa][Dd][Ee])|([Ss][Kk][Ii][Pp][-:])"
      fail = 0
    }

    # Buffer comment-line numbers in a circular ring of size window.
    {
      raw = $0
      # Identify YAML comment lines: leading whitespace then `#`.
      is_comment = (raw ~ /^[[:space:]]*#/)
      # Identify inline trailing comments on this same line (anywhere a
      # `#` appears after non-whitespace, treating quoted strings as not
      # YAML-significant for this lint; in practice always() lines do
      # not contain `#` inside quoted values).
      inline_comment = ""
      hash_pos = index(raw, "#")
      if (hash_pos > 0 && !is_comment) {
        inline_comment = substr(raw, hash_pos)
      }

      # Track comment lines (full-line) with their text for keyword lookup.
      if (is_comment) {
        comment_text[NR] = raw
      }

      # Detect always() occurrence on a NON-comment line in a GHA
      # expression context. The expression contexts in GHA are:
      #   (a) an `if:` key value at line start (after optional
      #       whitespace and optional list dash), and
      #   (b) inside any `${{ ... }}` interpolation (which can appear in
      #       `if:`, `env:`, `run:`, etc.).
      # The `if:` clause is anchored to line-start so that a step
      # name like `name: "Run X if: always() works"` is NOT mis-classified
      # as an `if:` usage — the embedded `if:` is part of a string scalar,
      # not a YAML key. Without the anchor, the lint produced FPs that
      # encouraged comment-spam workarounds (developers adding no-op
      # comments to silence the linter), weakening the signal.
      # Limitations:
      #   - Multi-line YAML block scalars (`if: |` / `if: >-` continuations):
      #     an `always()` on a continuation line is treated as plain text.
      #     No current workflow uses this multi-line form for always().
      #   - YAML flow form `{ if: always() }` (single-line mapping) is
      #     not matched by the line-start anchor. No current workflow
      #     uses this form for control-flow expressions.
      in_expr_context = (raw ~ /^[[:space:]]*-?[[:space:]]*if:/ || raw ~ /\$\{\{/)
      if (!is_comment && raw ~ /always\(/ && in_expr_context) {
        # Inline-comment fast path: if the same line carries a trailing
        # `#` comment matching the keyword regex, accept it.
        accepted = 0
        if (inline_comment != "" && inline_comment ~ KEYWORD_RE) {
          accepted = 1
        }

        # Look back across the window for a YAML comment line with the
        # keyword. Window is inclusive of NR (so window=1 means inline
        # only); window=10 means lines [NR-9 .. NR].
        if (!accepted) {
          start = NR - window + 1
          if (start < 1) start = 1
          for (i = start; i <= NR; i++) {
            if (i in comment_text && comment_text[i] ~ KEYWORD_RE) {
              accepted = 1
              break
            }
          }
        }

        if (!accepted) {
          # Distinguish "no comment at all in window" from "comment(s)
          # present but none had the keyword" so the user gets a more
          # actionable error.
          any_comment = 0
          start = NR - window + 1
          if (start < 1) start = 1
          for (i = start; i <= NR; i++) {
            if (i in comment_text) { any_comment = 1; break }
          }
          if (any_comment) {
            printf "%s:%d: always() lacks justification comment with keyword (always|load-bearing|cascade|skip) within %d lines\n", filename, NR, window > "/dev/stderr"
          } else {
            printf "%s:%d: always() lacks any preceding comment within %d lines (need keyword: always|load-bearing|cascade|skip)\n", filename, NR, window > "/dev/stderr"
          }
          fail = 1
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
  if ! scan_file "$f" "$WINDOW"; then
    overall_fail=1
  fi
done

if [[ "$overall_fail" -ne 0 ]]; then
  echo "" >&2
  echo "ERROR: one or more always() uses lack a justification comment (Invariant 8). See lines above." >&2
  exit 1
fi

exit 0
