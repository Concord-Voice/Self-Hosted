#!/bin/bash
# protect-env-files.sh — PreToolUse hook for Read|Edit|Write|Bash
# Blocks access to .env files and credential files that may contain secrets.
# Only .env.example is exempt: every other .env, .env.* or *.env file is treated as live.

if ! command -v jq >/dev/null 2>&1; then
    echo "BLOCKED: jq is required for protect-env-files.sh but is not installed." >&2
    exit 2
fi

INPUT=$(cat)

# Malformed JSON fails closed; a missing field does not (a Bash call has no file_path).
if ! FILE_PATH=$(printf '%s' "$INPUT" | jq -r '.tool_input.file_path // .tool_input.path // empty' 2>/dev/null); then
    echo "BLOCKED: Failed to parse hook input in protect-env-files.sh. Failing closed." >&2
    exit 2
fi

COMMAND=$(printf '%s' "$INPUT" | jq -r '.tool_input.command // empty' 2>/dev/null || true)

# Bash: block any word that names a live env file (cat .env, source app/.env.prod, ...),
# or an unquoted glob that could expand to one (.env*, .en?, .*). The command is read with
# glob characters inside quotes masked to _ and then the quotes deleted: a quoted glob
# ('.[]' in jq, '.*' in sed) never expands, while ".env"* and .en''v still read as .env* and
# .env. Braces split words, so {.env,x} is caught, and case is ignored. A word like
# process.env or config.env is left alone: code and prose name those far more often than a
# shell reads one.
if [[ -z "$FILE_PATH" ]] && [[ -n "$COMMAND" ]]; then
    SPLIT=" \t\n;&|()<>=\`{},"
    WORDS=$(printf '%s' "$COMMAND" \
        | perl -0pe 's/(\x27[^\x27]*\x27|"(?:[^"\\]|\\.)*")/(my $q = $1) =~ tr{*?[\x27"}{___}d; $q/ge' \
        | tr -s "$SPLIT" '\n')
    if printf '%s\n' "$WORDS" | grep -iE '(^|/)\.env(\.[^/]*)?$' | grep -viqE '(^|/)\.env\.example$' \
       || printf '%s\n' "$WORDS" | grep -iqE '(^|/)\.(e(nv?)?)?[*?[]'; then
        echo "BLOCKED: Bash command touches a .env file — likely contains secrets. Use .env.example as a reference instead." >&2
        exit 2
    fi
    exit 0
fi

[ -z "$FILE_PATH" ] && exit 0

# Block .env files (but allow .env.example)
BASE="${FILE_PATH##*/}"
if [[ "$BASE" =~ ^\.env(\..*)?$ || "$BASE" =~ .\.env$ ]] && [[ "$BASE" != ".env.example" ]]; then
    echo "BLOCKED: Cannot access $FILE_PATH — likely contains secrets. Use .env.example as a reference instead." >&2
    exit 2
fi

# Block credential files
if [[ "$FILE_PATH" =~ credentials\.json ]] || [[ "$FILE_PATH" =~ \.pem$ ]] || [[ "$FILE_PATH" =~ \.key$ ]]; then
    echo "BLOCKED: Cannot access credential file: $FILE_PATH" >&2
    exit 2
fi

exit 0
