#!/bin/bash
# protect-env-files.sh — PreToolUse hook for Read|Edit|Write
# Blocks access to .env files and credential files that may contain secrets.

if ! command -v jq >/dev/null 2>&1; then
    echo "BLOCKED: jq is required for protect-env-files.sh but is not installed." >&2
    exit 2
fi

INPUT=$(cat)

if ! FILE_PATH=$(printf '%s' "$INPUT" | jq -er '.tool_input.file_path // .tool_input.path // empty' 2>/dev/null); then
    echo "BLOCKED: Failed to parse hook input in protect-env-files.sh. Failing closed." >&2
    exit 2
fi

[ -z "$FILE_PATH" ] && exit 0

# Block .env files (but allow .env.example)
if [[ "$FILE_PATH" =~ \.env$ ]] || [[ "$FILE_PATH" =~ \.env\.[^e] ]] || [[ "$FILE_PATH" =~ \.env\.local ]]; then
    echo "BLOCKED: Cannot access $FILE_PATH — likely contains secrets. Use .env.example as a reference instead." >&2
    exit 2
fi

# Block credential files
if [[ "$FILE_PATH" =~ credentials\.json ]] || [[ "$FILE_PATH" =~ \.pem$ ]] || [[ "$FILE_PATH" =~ \.key$ ]]; then
    echo "BLOCKED: Cannot access credential file: $FILE_PATH" >&2
    exit 2
fi

exit 0
