#!/bin/bash
# protect-sensitive.sh — PreToolUse hook for Edit|Write. Blocks (exit 2) edits to
# security-critical files unless authorized via the per-branch marker file.
# Protected regexes come from .claude/repo-config.json (.hooks.protected_patterns[]);
# absent/empty => nothing blocked (the global deny-list still covers .env/.ssh).
set -o pipefail
if ! command -v jq >/dev/null 2>&1; then
  echo "BLOCKED: jq is required for protect-sensitive.sh but is not installed." >&2; exit 2
fi
INPUT=$(cat)
if ! FILE_PATH=$(printf '%s' "$INPUT" | jq -er '.tool_input.file_path // empty' 2>/dev/null); then
  echo "BLOCKED: Failed to parse hook input in protect-sensitive.sh. Failing closed." >&2; exit 2
fi
[ -z "$FILE_PATH" ] && exit 0
REPO_ROOT="${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel 2>/dev/null || echo "")}"
RC_FILE="${REPO_ROOT:+$REPO_ROOT/}.claude/repo-config.json"; [ -f "$RC_FILE" ] || RC_FILE=".claude/repo-config.json"

# Marker-file authorization (generic bypass engine; inert without a marker dir).
BRANCH=$(git branch --show-current 2>/dev/null || true)
if [ -n "$BRANCH" ]; then
  BRANCH_SAFE="${BRANCH//\//__}"
  if [[ "$BRANCH_SAFE" =~ ^[A-Za-z0-9._-]+$ ]]; then
    MARKER="${REPO_ROOT:+$REPO_ROOT/}.claude/state/hook-bypass/${BRANCH_SAFE}.txt"
    if [ -f "$MARKER" ] && [ ! -L "$MARKER" ] && [ "$(wc -c < "$MARKER" 2>/dev/null || echo 0)" -le 16384 ]; then
      REL_PATH="$FILE_PATH"
      [ -n "$REPO_ROOT" ] && [[ "$FILE_PATH" = "$REPO_ROOT"/* ]] && REL_PATH="${FILE_PATH#"$REPO_ROOT"/}"
      while IFS= read -r ap || [ -n "$ap" ]; do
        [[ "$ap" =~ ^[[:space:]]*# ]] && continue; [ -z "$ap" ] && continue
        [[ "$ap" == /* ]] && continue; [[ "$ap" =~ (^|/)\.\.(/|$) ]] && continue
        if [ "$REL_PATH" = "$ap" ]; then echo "ALLOWED: $FILE_PATH (marker ${BRANCH_SAFE}.txt, branch $BRANCH)" >&2; exit 0; fi
      done < "$MARKER"
    fi
  fi
fi
# Skip non-executable Claude metadata (names may contain security terms).
if [[ "$FILE_PATH" =~ /\.claude/(agents|skills|rules)/ ]] || [[ "$FILE_PATH" =~ ^\.claude/(agents|skills|rules)/ ]]; then exit 0; fi
# Per-repo protected patterns.
[ -f "$RC_FILE" ] || exit 0
while IFS= read -r pattern; do
  [ -z "$pattern" ] && continue
  [[ "$FILE_PATH" =~ $pattern ]]; rc=$?
  if [ "$rc" -eq 2 ]; then
    echo "BLOCKED: invalid regex in protected_patterns: $pattern. Failing closed." >&2; exit 2
  elif [ "$rc" -eq 0 ]; then
    echo "BLOCKED: Editing security-critical file: $FILE_PATH (matched: $pattern). Requires explicit user approval." >&2; exit 2
  fi
done < <(jq -r '.hooks.protected_patterns[]? // empty' "$RC_FILE" 2>/dev/null)
exit 0
