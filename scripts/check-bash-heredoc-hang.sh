#!/usr/bin/env bash
#
# check-bash-heredoc-hang.sh — refuse to run the git hooks under a bash that can
# hang forever writing a heredoc (#3426).
#
# macOS bash 5.1.0 through 5.3.15 writes a heredoc into a pipe with one blocking
# write before exec'ing its reader, and a macOS pipe can stay at 512 bytes under
# load, so any heredoc over 512 bytes (after expansion, whatever command reads
# it) can hang. Fixed upstream in bash 5.3 patch 16. The full mechanism is in
# .pre-commit-config.yaml above this hook, which also explains why it must run
# first with fail_fast. The shebang resolves bash from PATH, the same bash the
# other hooks get. This file must contain no heredoc.
#
# Usage:
#   check-bash-heredoc-hang.sh                       check the bash running it
#   check-bash-heredoc-hang.sh --check OS MAJ MIN PATCH
#                                                    decide for given values (tests)
#
# Exits:
#   0 — this bash is not affected
#   1 — this bash can hang on a heredoc; upgrade it
#   2 — usage error

set -euo pipefail

# affected <os> <major> <minor> <patch> — true for darwin bash 5.1.0–5.3.15.
affected() {
  [[ "$1" == darwin* ]] || return 1
  (( $2 == 5 && $3 >= 1 )) || return 1
  (( $3 < 3 || ($3 == 3 && $4 < 16) ))
}

if [[ "${1:-}" == "--check" ]]; then
  if [[ $# -ne 5 ]]; then
    echo "usage: $0 --check OS MAJOR MINOR PATCH" >&2
    exit 2
  fi
  # No leading zeros: (( )) would read `09` as invalid octal and fall through
  # to "not affected".
  for n in "$3" "$4" "$5"; do
    [[ "$n" =~ ^(0|[1-9][0-9]*)$ ]] || { echo "ERROR: not a version number: $n" >&2; exit 2; }
  done
  os="$2" major="$3" minor="$4" patch="$5" shell_desc="bash $3.$4.$5"
elif [[ $# -eq 0 ]]; then
  # uname, not $OSTYPE: bash takes an exported OSTYPE from the environment.
  case "$(uname -s)" in Darwin) os=darwin ;; *) os=other ;; esac
  major="${BASH_VERSINFO[0]}" minor="${BASH_VERSINFO[1]}" patch="${BASH_VERSINFO[2]}"
  shell_desc="$BASH ($BASH_VERSION)"
else
  echo "usage: $0 [--check OS MAJOR MINOR PATCH]" >&2
  exit 2
fi

if affected "$os" "$major" "$minor" "$patch"; then
  printf '%s\n' \
    "ERROR: $shell_desc on macOS can hang forever writing a heredoc over 512" \
    "bytes (fixed upstream in bash 5.3 patch 16). Hooks in this repository feed" \
    "heredocs that size, so the hooks are refused rather than risk a silent hang." \
    "Upgrade the bash first on your PATH, then retry:" \
    "  brew upgrade bash" \
    "Details: internal development reference omitted" >&2
  exit 1
fi
