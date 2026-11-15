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
# A keyword inside a longer word (hotfix, prefix) is not a closing reference.
[[ -z "$(parse_pr_closes 'Ships the hotfix #123 and a prefix #7')" ]] || fail "parse_pr_closes matched hotfix/prefix"
[[ "$(parse_pr_closes 'Closes: #12')" = "12" ]] || fail "parse_pr_closes missed Closes: #12"
[[ "$(parse_pr_refs 'Refs: #9')" = "9" ]] || fail "parse_pr_refs missed Refs: #9"
[[ "$(parse_pr_closes '(fixes #9)')" = "9" ]] || fail "parse_pr_closes missed a keyword after punctuation"

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

# A fence indented up to 3 spaces is a fence (GFM); mawk's `/^ ? ? ?/` stripped only one space.
body5=$'Title\n\n   ```\n   Closes #555\n   ```\n\nCloses #7'
out5="$(parse_pr_closes "$body5")"
echo "$out5" | grep -qx "7" || fail "parse_pr_closes should catch Closes #7 after an indented fence"
echo "$out5" | grep -qx "555" && fail "parse_pr_closes leaked an indented-fence Closes #555" || true

# A labels source that was named but is missing fails closed: no passthrough, exit 1.
( out="$(LABELS_FILE=/nonexistent/labels-$$.md bash -c 'source "'"$HERE"'/lib.sh"; printf "type: bug\n" | validate_against_active_labels' 2>/dev/null)" \
    && fail "missing named labels source must exit non-zero"
  [[ -z "$out" ]] || fail "missing named labels source must print nothing" )

# Same, when the source is named in repo-config and the reader helper is absent.
( tmpd="$(mktemp -d)"; mkdir -p "$tmpd/.claude"
  printf '%s\n' '{ "labels": { "source": "nope.md" } }' > "$tmpd/.claude/repo-config.json"
  REPO_ROOT="$tmpd" bash -c 'source "'"$HERE"'/lib.sh"; unset -f rc_get; printf "type: bug\n" | validate_against_active_labels' >/dev/null 2>&1 \
    && fail "named-but-missing source must fail closed without the rc_get helper"
  rm -rf "$tmpd" )

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

# pick_inheritance_source: first Closes wins, else first Refs, else empty.
[[ "$(pick_inheritance_source "10" "20")" = "10" ]] || fail "pick_inheritance_source should prefer Closes"
[[ "$(pick_inheritance_source "" "$(printf '77\n88')")" = "77" ]] || fail "pick_inheritance_source should fall back to the first Refs"
[[ -z "$(pick_inheritance_source "" "")" ]] || fail "pick_inheritance_source should be empty with no source"
staged=$'Stage 1 of 4.\n\nRefs #2001'
[[ "$(pick_inheritance_source "$(parse_pr_closes "$staged")" "$(parse_pr_refs "$staged")")" = "2001" ]] || fail "staged Refs-only PR should inherit from its Refs issue"

# risk: hotfix is a release control: never inherited, always surfaced with one exact command.
got="$(filter_inherited_labels '["type: bug","risk: hotfix","type: epic","routine: weekly"]' | paste -sd, -)"
[[ "$got" = "type: bug" ]] || fail "filter_inherited_labels kept a filtered label: got '$got'"
[[ "$(dropped_release_controls '["type: bug","risk: hotfix"]')" = "risk: hotfix" ]] || fail "dropped_release_controls should surface risk: hotfix"
warn="$(warn_dropped_release_controls '["risk: hotfix"]' 42 99 2>&1)"
[[ "$(printf '%s\n' "$warn" | grep -c "gh pr edit 99 --add-label 'risk: hotfix'")" = "1" ]] || fail "warn_dropped_release_controls should print one exact command"
printf '%s\n' "$warn" | grep -qE "add-label '(risk:|hotfix)'" && fail "warn_dropped_release_controls word-split the label" || true
[[ -z "$(warn_dropped_release_controls '["type: bug"]' 42 99 2>&1)" ]] || fail "warn_dropped_release_controls should be silent with no release control"

# The no-milestone WARN lists open titles rather than hardcoding one.
validate_pr_invariants --has-label --no-milestone --has-assignee --has-closes 2>&1 \
  | grep -q "milestones?state=open" || fail "no-milestone WARN should point at the open-milestones listing"

# Cloud routing: ID helpers emit sentinels; mutating helpers dispatch board-op with the decoded argv.
( export PROJECT_ID=PVT_test STATUS_FIELD_ID=F_status PRIORITY_FIELD_ID=F_prio CLAUDE_CODE_REMOTE=true
  tmpc="$(mktemp -d)"
  printf '%s\n' '#!/usr/bin/env bash' 'printf "%s\n" "$*" >> "$BOARD_OP_RECORD"' > "$tmpc/stub.sh"
  chmod +x "$tmpc/stub.sh"
  export BOARD_OP_RECORD="$tmpc/record" BOARD_OP_BIN="$tmpc/stub.sh"
  [[ "$(find_project_item_id 1234)" = "CLOUD_NOCREATE:1234" ]] || fail "find_project_item_id cloud sentinel"
  [[ "$(ensure_pr_on_project 42)" = "CLOUD:42" ]] || fail "ensure_pr_on_project cloud sentinel"
  [[ "$(get_issue_priority_option_id 777)" = "CLOUD_INHERIT:777" ]] || fail "get_issue_priority_option_id cloud sentinel"
  find_project_item_id 'x;y' >/dev/null 2>&1 && fail "cloud sentinel must reject a non-numeric issue"
  move_to_status "CLOUD:42" "opt_review" || fail "move_to_status cloud dispatch"
  set_priority "CLOUD_NOCREATE:1234" "CLOUD_INHERIT:777" || fail "set_priority cloud inherit dispatch"
  set_priority "CLOUD:42" "opt_p1" || fail "set_priority cloud dispatch"
  expected=$'42 --status opt_review\n1234 --no-create --inherit-priority-from 777\n42 --priority opt_p1'
  [[ "$(cat "$BOARD_OP_RECORD")" = "$expected" ]] || fail "board-op argv wrong: $(cat "$BOARD_OP_RECORD")"
  BOARD_OP_BIN="$tmpc/missing.sh" move_to_status "CLOUD:42" "opt_review" 2>/dev/null && fail "missing board-op must fail non-zero"
  # With no board configured, the read-only lookups stay empty even in the cloud.
  [[ -z "$(PROJECT_ID='' find_project_item_id 1234)" ]] || fail "no board: find_project_item_id should be empty in the cloud"
  rm -rf "$tmpc" )

echo "ALL PASS"
