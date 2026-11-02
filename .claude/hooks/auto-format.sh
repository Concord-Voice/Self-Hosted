#!/bin/bash
# auto-format.sh — PostToolUse hook for Edit|Write. Stack-aware formatter.
# Detects the formatter from the file extension + nearest config / installed
# tooling, and NO-OPS when none apply. Never blocks (always exit 0).
INPUT=$(cat)
FILE_PATH=$(printf '%s' "$INPUT" | jq -r '.tool_input.file_path // .tool_input.path // empty' 2>/dev/null)
[ -z "$FILE_PATH" ] && exit 0
[ ! -f "$FILE_PATH" ] && exit 0
REPO_ROOT="${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel 2>/dev/null)}"

find_prettier_dir() { # nearest ancestor (file dir → REPO_ROOT) with prettier config/dep
  local dir; dir="$(cd "$(dirname "$FILE_PATH")" 2>/dev/null && pwd)" || return 0
  while [ -n "$dir" ]; do
    if ls "$dir"/.prettierrc* >/dev/null 2>&1 || ls "$dir"/prettier.config.* >/dev/null 2>&1 \
       || { [ -f "$dir/package.json" ] && grep -q '"prettier"' "$dir/package.json" 2>/dev/null; }; then
      printf '%s\n' "$dir"; return 0
    fi
    [ -n "$REPO_ROOT" ] && [ "$dir" = "$REPO_ROOT" ] && break
    [ "$dir" = "/" ] && break
    dir="$(dirname "$dir")"
  done
}

case "$FILE_PATH" in
  *.ts|*.tsx|*.js|*.jsx|*.mjs|*.cjs|*.css|*.scss|*.json|*.md|*.yaml|*.yml|*.html)
    # Only the project's installed, lockfile-pinned prettier: npx would fetch the latest
    # release from the registry when none is installed.
    # Walk up from the config dir to REPO_ROOT so a hoisted workspace install is found.
    d="$(find_prettier_dir)"
    while [ -n "$d" ]; do
      [ -x "$d/node_modules/.bin/prettier" ] && { "$d/node_modules/.bin/prettier" --write "$FILE_PATH" 2>/dev/null; break; }
      [ -n "$REPO_ROOT" ] && [ "$d" != "$REPO_ROOT" ] && [ "$d" != "/" ] || break
      d="$(dirname "$d")"
    done ;;
esac
if [[ "$FILE_PATH" =~ \.go$ ]]; then
  command -v goimports >/dev/null 2>&1 && goimports -w "$FILE_PATH" 2>/dev/null
  if command -v gofumpt >/dev/null 2>&1; then gofumpt -w "$FILE_PATH" 2>/dev/null
  elif command -v gofmt >/dev/null 2>&1; then gofmt -w "$FILE_PATH" 2>/dev/null; fi
fi
if [[ "$FILE_PATH" =~ \.py$ ]]; then
  if command -v ruff >/dev/null 2>&1; then ruff format "$FILE_PATH" 2>/dev/null
  elif command -v black >/dev/null 2>&1; then black -q "$FILE_PATH" 2>/dev/null; fi
fi
[[ "$FILE_PATH" =~ \.rs$ ]] && command -v rustfmt >/dev/null 2>&1 && rustfmt "$FILE_PATH" 2>/dev/null
exit 0
