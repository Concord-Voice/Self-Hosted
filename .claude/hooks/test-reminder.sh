#!/bin/bash
# test-reminder.sh — PostToolUse hook for Edit|Write. Non-blocking (always exit 0).
# Reads .hooks.test_reminders[] and .hooks.test_paths from .claude/repo-config.json.
INPUT=$(cat)
FILE_PATH=$(printf '%s' "$INPUT" | jq -r '.tool_input.file_path // .tool_input.path // empty' 2>/dev/null)
[ -z "$FILE_PATH" ] && exit 0
command -v jq >/dev/null 2>&1 || exit 0
REPO_ROOT="${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel 2>/dev/null || echo "")}"
RC_FILE="${REPO_ROOT:+$REPO_ROOT/}.claude/repo-config.json"; [ -f "$RC_FILE" ] || RC_FILE=".claude/repo-config.json"
[ -f "$RC_FILE" ] || exit 0

# 1. Domain reviewer reminders.
while IFS=$'\t' read -r regex message stop; do
  [ -z "$regex" ] && continue
  if [[ "$FILE_PATH" =~ $regex ]]; then printf '%s\n' "$message" >&2; [ "$stop" = "true" ] && exit 0; fi
done < <(jq -r '.hooks.test_reminders[]? | (.regex + "\t" + .message + "\t" + (.stop|tostring))' "$RC_FILE" 2>/dev/null)

# 2. Generic coverage reminder.
ext="${FILE_PATH##*.}"
TEST_SKIP_RE=$(jq -r '.hooks.test_paths.test_skip_re // "(_test\\.go|\\.test\\.(ts|tsx)|\\.spec\\.(ts|tsx))$"' "$RC_FILE" 2>/dev/null)
[[ "$FILE_PATH" =~ $TEST_SKIP_RE ]] && exit 0
in_cfg() { local e; for e in $(jq -r ".hooks.test_paths.$1[]? // empty" "$RC_FILE" 2>/dev/null); do [ "$e" = "$2" ] && return 0; done; return 1; }
in_cfg skip_ext "$ext" && exit 0
in_cfg source_ext "$ext" || exit 0
DOCS_REMINDER=$(jq -r '.hooks.test_paths.docs_reminder // ""' "$RC_FILE" 2>/dev/null)
[ -n "$DOCS_REMINDER" ] && [[ ! "$FILE_PATH" =~ /tests/ ]] && printf '%s\n' "$DOCS_REMINDER" >&2
if [ "$ext" = "go" ]; then TEST_FILE="${FILE_PATH%.go}_test.go"
else
  TEST_FILE="$FILE_PATH"
  while IFS=$'\t' read -r src dst; do
    [ -z "$src" ] && continue
    if [[ "$FILE_PATH" == *"$src"* ]]; then rel="${FILE_PATH#*"$src"}"; TEST_FILE="${dst}${rel}"; break; fi
  # @tsv is safe here: src/dst are POSIX path prefixes (no backslashes to double).
  done < <(jq -r '.hooks.test_paths.remaps[]? | [.src, .dst] | @tsv' "$RC_FILE" 2>/dev/null)
  TEST_FILE="${TEST_FILE%.*}.test.${ext}"
fi
printf 'Reminder: Ensure test coverage exists for %s. Expected test: %s\n' "$(basename "$FILE_PATH")" "$TEST_FILE" >&2
exit 0
