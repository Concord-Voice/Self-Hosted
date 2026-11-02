#!/usr/bin/env bash
# Test harness for pr-creation/lib.sh generalization.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
fail() { echo "FAIL: $*" >&2; exit 1; }
# shellcheck disable=SC1090
source "$HERE/lib.sh"

# parse_pr_closes extracts each Closes/Fixes/Resolves number.
out="$(parse_pr_closes $'Title\n\nCloses #12\nFixes #34, Resolves #56')"
echo "$out" | grep -qx "12" || fail "parse_pr_closes missed 12"
echo "$out" | grep -qx "34" || fail "parse_pr_closes missed 34"
echo "$out" | grep -qx "56" || fail "parse_pr_closes missed 56"

# parse_pr_refs extracts Refs without closing.
parse_pr_refs 'Refs #99' | grep -qx "99" || fail "parse_pr_refs missed 99"

# No hardcoded project-ID default leaks: with PROJECT_ID unset, the project-id resolver must be empty, not PVT_….
unset PROJECT_ID STATUS_FIELD_ID PRIORITY_FIELD_ID OWNER REPO_NAME
resolved="$(printf '%s' "${PROJECT_ID:-}")"
[ -z "$resolved" ] || fail "PROJECT_ID should have no value when unset"
grep -qE 'PROJECT_ID:-.?PVT_' "$HERE/lib.sh" && fail "hardcoded PROJECT_ID default still present in lib.sh" || true

# Fence bypass: a 4-backtick outer fence wrapping a 3-backtick inner block must NOT leak inner Closes.
body4=$'Title\n\n````\n```\nCloses #777\n```\n````\n\nCloses #5'
out4="$(parse_pr_closes "$body4")"
echo "$out4" | grep -qx "5" || fail "parse_pr_closes should catch the real Closes #5"
echo "$out4" | grep -qx "777" && fail "parse_pr_closes leaked a fenced Closes #777" || true

# Normal 3-backtick fence still strips its contents.
body3=$'Title\n\n```\nCloses #888\n```\n\nFixes #6'
out3="$(parse_pr_closes "$body3")"
echo "$out3" | grep -qx "6" || fail "parse_pr_closes should catch Fixes #6"
echo "$out3" | grep -qx "888" && fail "parse_pr_closes leaked a 3-backtick fenced Closes #888" || true

# Empty-namespace config: validate_against_active_labels degrades to passthrough no-op (no error).
( tmpd="$(mktemp -d)"; mkdir -p "$tmpd/.claude"
  printf '%s\n' '{ "labels": { "namespaces": [] } }' > "$tmpd/.claude/repo-config.json"
  RC_FILE="$tmpd/.claude/repo-config.json" LABEL_NAMESPACES="" bash -c '
    source "'"$HERE"'/lib.sh"
    printf "type: bug\n" | validate_against_active_labels >/dev/null 2>&1' || fail "empty namespaces should no-op, not error"
  rm -rf "$tmpd" )

# The bundled taxonomy (namespace headings, no "## Active labels") still yields an allowlist.
# Source layout (plugins/*/skills/pr-creation) or installed layout (.claude/skills/pr-creation).
bundled="$HERE/../../../../docs/rules/labels.md"; [ -f "$bundled" ] || bundled="$HERE/../../rules/labels.md"
( out="$(LABELS_FILE="$bundled" LABEL_NAMESPACES="type area" bash -c '
    source "'"$HERE"'/lib.sh"
    printf "type: bug\ntype: nope\n" | validate_against_active_labels 2>/dev/null')" || fail "bundled labels.md should validate"
  [ "$out" = "type: bug" ] || fail "bundled labels.md allowlist wrong: got '$out'" )

# Fail-loud: a mutating helper with no project config aborts non-zero.
( unset PROJECT_ID STATUS_FIELD_ID
  RC_FILE=/nonexistent bash -c 'source "'"$HERE"'/lib.sh"; move_to_status 1 X 2>/dev/null' && fail "move_to_status should fail loud with no PROJECT_ID" || true )

echo "ALL PASS"
