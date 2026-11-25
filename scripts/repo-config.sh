#!/bin/sh
# repo-config.sh — sourced helper to read a repo's .claude/repo-config.json.
# Usage (source it, then):
#   rc_get '<jq-filter>' [default]   -> prints the value, or default if file/key absent/null/malformed/missing-jq;
#                                       null/absent yield the default; false/0/"" are returned verbatim.
#   rc_has '<jq-filter>'             -> exit 0 iff the key exists and is non-null
# Resolution: $RC_FILE if set, else <git-toplevel|PWD>/.claude/repo-config.json.
# Never errors the caller: missing jq, missing file, malformed JSON all yield the default.

RC_FILE="${RC_FILE:-$( { git rev-parse --show-toplevel 2>/dev/null || printf '%s' "$PWD"; }; )/.claude/repo-config.json}"

rc_get() (
  filter="$1"
  default="${2:-}"
  command -v jq >/dev/null 2>&1 || { printf '%s' "$default"; return 0; }
  [ -f "$RC_FILE" ] || { printf '%s' "$default"; return 0; }
  v="$(jq -ers "select(length == 1) | .[0] | ($filter) | select(. != null) | tostring" "$RC_FILE" 2>/dev/null)" || { printf '%s' "$default"; return 0; }
  printf '%s' "$v"
)

rc_has() (
  command -v jq >/dev/null 2>&1 || return 1
  [ -f "$RC_FILE" ] || return 1
  filter="$1"
  jq -es "length == 1 and (.[0] | ($filter) != null)" "$RC_FILE" >/dev/null 2>&1
)
