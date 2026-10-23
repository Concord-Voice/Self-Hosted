#!/usr/bin/env bash
#
# lint-boolean-coercion.sh — flag `workflow_dispatch` input comparisons of the
# form `inputs.X == 'true'` or `inputs.X != 'true'`. Root cause of #911 → #912
# and #947: GitHub Actions' expression evaluator silently coerces boolean inputs
# (`type: boolean`) to numeric 1/0 (NOT string "true"/"false"), so a comparison
# against the string `'true'` always evaluates false. The correct forms are:
#   - `format('{0}', inputs.X) == 'true'` — type-agnostic, works for both
#     boolean and string-typed inputs.
#   - `inputs.X == true` — no quotes; only when the input is declared
#     `type: boolean`.
# Pattern P2/P8 from spec:
# [internal]specs/2026-05-14-921-workflow-validation-sandbox-design.md §5.2 / §6.3
#
# Usage:
#   lint-boolean-coercion.sh <file.yml-or-dir/>
#   lint-boolean-coercion.sh --help
#
# Exits:
#   0 — no boolean-coercion patterns found
#   1 — at least one `inputs.X == 'true'` or `inputs.X != 'true'` pattern
#       found outside of a YAML comment (the bug pattern)
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
# Symlinks rejected as defense against malicious checkouts routing the
# scanner outside .github/workflows/ — same pattern as the sibling lints.
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
# awk variables:
#   filename — file being scanned (for stderr reporting)
#
# Algorithm:
#   For each line, skip YAML comments (lines whose first non-whitespace
#   char is `#`) so prose mentioning the bug pattern in commentary does
#   not false-positive. Match `inputs.<name> == 'true'` or `inputs.<name>
#   != 'true'` where `<name>` is `[a-zA-Z_][a-zA-Z0-9_-]*` (covers both
#   snake_case from the original #911 incident — `inputs.force_deploy` —
#   and kebab-case occasionally used). Emit file:line plus the suggested
#   fix wording so developers don't have to look up the correct form.
scan_file() {
  local file="$1"

  awk -v filename="$file" '
    # BAD_RE matches the bug pattern. The lookahead-free form means we
    # accept whatever whitespace surrounds the operator. The name class
    # is [a-zA-Z_][a-zA-Z0-9_-]* to match real GHA input-name conventions.
    BEGIN {
      # The string-literal `true` may be single-quoted OR double-quoted in
      # GHA YAML — the expression evaluator treats both as the same string
      # value, so both exhibit the silent-coercion bug against `type: boolean`
      # inputs. The alternation matches either quote style. The octal
      # escape \047 represents the single-quote character (ASCII 39); using
      # the octal form keeps the bash outer-single-quote integrity intact
      # without resorting to the close-escape-open dance.
      BAD_RE = "inputs\\.[a-zA-Z_][a-zA-Z0-9_-]*[[:space:]]*(==|!=)[[:space:]]*(\"true\"|\047true\047)"
      fail = 0
    }

    # Skip YAML comment lines — `^[[:space:]]*#` is the canonical YAML
    # comment-start signature. A line whose first non-whitespace char is
    # `#` is parsed as a comment by yaml.safe_load and never seen by the
    # GHA expression evaluator at dispatch time. Prose in such comments
    # discussing the bug pattern (e.g. `# Empirical: run had
    # `inputs.force == ''true''` evaluate FALSE`) must not trigger the lint.
    /^[[:space:]]*#/ { next }

    {
      if (match($0, BAD_RE)) {
        # Extract the matched substring for the error message.
        matched = substr($0, RSTART, RLENGTH)
        printf "%s:%d: boolean-coercion bug: %s\n", filename, NR, matched > "/dev/stderr"
        printf "%s:%d:   suggested fix: format('\''{0}'\'', inputs.X) == '\''true'\'' (type-agnostic) OR inputs.X == true (no quotes; requires type: boolean)\n", filename, NR > "/dev/stderr"
        fail = 1
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
  echo "ERROR: workflow_dispatch boolean comparisons against the string 'true' silently fail at runtime." >&2
  echo "       Use format('{0}', inputs.X) == 'true' (type-agnostic) or inputs.X == true (no quotes)." >&2
  exit 1
fi

exit 0
