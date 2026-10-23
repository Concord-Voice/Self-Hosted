#!/usr/bin/env bash
# check-env-file.sh — lints an env file for values that would fail
# when sourced by bash. Catches the class of bugs where an unquoted
# value contains shell metacharacters — bash would interpret these as
# I/O redirection, command separation, command substitution, variable
# expansion, pipelines, logical operators, or escape sequences instead
# of literal text.
#
# Rejected metachars (unquoted): <, >, ;, space, backtick, $, |, &, \
# Rejected patterns:
#   - Quoted value followed by more content (e.g. KEY="foo" "bar")
#   - Opened quote that does not close on the same line
#   - Lines that don't match KEY=VALUE form
#
# Exit 0 if the file is clean. Exit 1 (with diagnostic output) on any
# line that would break when sourced.
#
# Usage: ./check-env-file.sh path/to/.env [path/to/another.env ...]
#
# Limitations:
#   - Does not recognise escaped quotes ("foo\"bar") — treats them as
#     mis-quoted. Escaped quotes in env files are rare; use single
#     quotes for literal strings containing double quotes and vice
#     versa. Documented, not a bug.
#   - Does not accept the `\'` form that ${VAR@Q} produces when the
#     value is exactly one single-quote character. ${VAR@Q} emits the
#     compact `\'` form rather than wrapping it (e.g., `'\\''`). This
#     edge case requires a value of exactly one apostrophe — an
#     unrealistic shape for any production secret. Hand-edited files
#     using this form should switch to `''\''''` (the multi-segment
#     form for a single-char value).
#   - set -u only (no -e, no pipefail) — the control flow is explicit
#     accumulate-and-continue across multiple files; -e would abort
#     on the first file error instead of reporting all of them.
#
# Precedent: sibling check-*.sh scripts in this directory follow the
# same exit-code + diagnostic pattern. Invoked from pre-commit and
# optionally from provisioning flows.
#
# Refs #725 (Track 4 + post-review hardening).
set -u

if [ "$#" -lt 1 ]; then
  echo "usage: $0 path/to/.env [more.env ...]" >&2
  exit 2
fi

overall_exit=0

for file in "$@"; do
  if [ ! -f "$file" ]; then
    echo "error: $file does not exist or is not a regular file" >&2
    overall_exit=1
    continue
  fi

  # Defense against mid-read I/O failure: verify the file is wholly
  # readable up-front. `while read` would silently truncate on a
  # permission flip or I/O error mid-scan without this guard.
  if ! cat "$file" >/dev/null 2>&1; then
    echo "error: $file exists but could not be read (permission or I/O failure)" >&2
    overall_exit=1
    continue
  fi

  # Read line-by-line, preserving line numbers
  line_num=0
  file_errors=0
  while IFS= read -r line || [ -n "$line" ]; do
    line_num=$((line_num + 1))

    # Skip comments and blank lines
    case "$line" in
      ''|'#'*) continue ;;
    esac

    # Must match KEY=... form (uppercase/underscore/digit identifier)
    case "$line" in
      [A-Z_]*=*) : ;;
      *)
        echo "$file:$line_num: not a valid KEY=VALUE line: $line" >&2
        file_errors=$((file_errors + 1))
        continue
        ;;
    esac

    value="${line#*=}"

    # Accept balanced quoted values (single or double) — the quoted
    # region must be the entire value with nothing on either side.
    case "$value" in
      '"'*'"')
        # Starts and ends with double-quote. Verify there is no unescaped
        # double-quote in the interior — that would indicate something
        # like KEY="foo" "bar" (two quoted segments with trailing content).
        interior="${value%\"}"
        interior="${interior#\"}"
        case "$interior" in
          *'"'*)
            echo "$file:$line_num: value has content outside the quoted region — e.g. KEY=\"foo\" \"bar\": $line" >&2
            file_errors=$((file_errors + 1))
            continue
            ;;
        esac
        continue
        ;;
      "'"*"'")
        interior="${value%\'}"
        interior="${interior#\'}"
        case "$interior" in
          *"'"*)
            # #841 — value contains interior single quotes. Validate as
            # ${VAR@Q} multi-segment output:
            #   open '  →  literal-chars  →  '\''  →  literal-chars  →  close '
            # Walking state machine: AT_START → INSIDE → AFTER_CLOSE →
            # IN_ESCAPE → IN_ESCAPE2 → INSIDE → ... → AFTER_CLOSE (final).
            mseg_state="AT_START"
            mseg_i=0
            mseg_len=${#value}
            mseg_valid=true
            while [ $mseg_i -lt $mseg_len ]; do
              mseg_c="${value:$mseg_i:1}"
              case "$mseg_state" in
                AT_START)
                  [ "$mseg_c" = "'" ] || { mseg_valid=false; break; }
                  mseg_state="INSIDE"
                  ;;
                INSIDE)
                  if [ "$mseg_c" = "'" ]; then
                    mseg_state="AFTER_CLOSE"
                  fi
                  ;;
                AFTER_CLOSE)
                  if [ "$mseg_c" = "\\" ]; then
                    mseg_state="IN_ESCAPE"
                  else
                    mseg_valid=false
                    break
                  fi
                  ;;
                IN_ESCAPE)
                  [ "$mseg_c" = "'" ] || { mseg_valid=false; break; }
                  mseg_state="IN_ESCAPE2"
                  ;;
                IN_ESCAPE2)
                  [ "$mseg_c" = "'" ] || { mseg_valid=false; break; }
                  mseg_state="INSIDE"
                  ;;
              esac
              mseg_i=$((mseg_i + 1))
            done
            if [ "$mseg_valid" = "false" ] || [ "$mseg_state" != "AFTER_CLOSE" ]; then
              echo "$file:$line_num: malformed single-quote escaping (expected balanced '...' or @Q multi-segment '...'\\'\\''...'): $line" >&2
              file_errors=$((file_errors + 1))
              continue
            fi
            ;;
        esac
        continue
        ;;
      \$\'*\')
        # #841 — ANSI-C quoted form `$'...'`. ${VAR@Q} produces this
        # when the value contains control characters (newline, tab,
        # NUL-prefix). Interior is bash escape sequences (\n, \t, \xNN,
        # \uNNNN) plus literal chars; bare ' inside is invalid.
        ansic_interior="${value#\$\'}"
        ansic_interior="${ansic_interior%\'}"
        ansic_i=0
        ansic_len=${#ansic_interior}
        ansic_valid=true
        while [ $ansic_i -lt $ansic_len ]; do
          ansic_c="${ansic_interior:$ansic_i:1}"
          if [ "$ansic_c" = "\\" ]; then
            # Trailing backslash with no following char means the closing '
            # would be eaten by the escape — bash would reject this. Linter
            # must match bash's strictness.
            if [ $((ansic_i + 1)) -ge $ansic_len ]; then
              ansic_valid=false
              break
            fi
            ansic_i=$((ansic_i + 2))   # skip escape sequence
          elif [ "$ansic_c" = "'" ]; then
            ansic_valid=false
            break
          else
            ansic_i=$((ansic_i + 1))
          fi
        done
        if [ "$ansic_valid" = "false" ]; then
          echo "$file:$line_num: unescaped single-quote inside \$'...' value: $line" >&2
          file_errors=$((file_errors + 1))
          continue
        fi
        continue
        ;;
      '"'*|"'"*)
        echo "$file:$line_num: value opens a quote but does not close on the same line: $line" >&2
        file_errors=$((file_errors + 1))
        continue
        ;;
    esac

    # Unquoted value — reject unsafe shell metacharacters. Each is a
    # real-world failure mode:
    #   space    — bash splits the value into tokens
    #   < >      — I/O redirection (the original SMTP_FROM line-15 bug)
    #   ;        — command separator
    #   `        — legacy command substitution
    #   $        — variable expansion OR $( ) command substitution
    #   |        — pipeline
    #   &        — background execution or AND/OR chaining via &&/||
    #   \        — escape sequence that changes the next character
    case "$value" in
      *' '*|*'<'*|*'>'*|*';'*|*'`'*|*'$'*|*'|'*|*'&'*|*'\'*)
        echo "$file:$line_num: unquoted value contains shell metacharacter (one of: space, <, >, ;, \`, \$, |, &, \\): $line" >&2
        echo "  fix: wrap the value in double quotes — KEY=\"...\"  (use single quotes if the value itself contains \$ that should stay literal)" >&2
        file_errors=$((file_errors + 1))
        ;;
    esac
  done < "$file"

  if [ "$file_errors" -gt 0 ]; then
    echo "$file: $file_errors error(s)" >&2
    overall_exit=1
  fi
done

exit "$overall_exit"
