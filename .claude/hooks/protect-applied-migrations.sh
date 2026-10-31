#!/bin/bash
# protect-applied-migrations.sh — PreToolUse hook for Edit
#
# Blocks edits to existing migration files in the migrations directory (default
# migrations/; override via .claude/repo-config.json's .migrations.dir — same
# knob migration-reviewer reads). Migrations are immutable once written — the
# rule (.claude/rules/migrations.md) is that every up migration must have a
# corresponding down migration and you never modify a shipped migration in
# place; you add a new one instead.
#
# Write (for brand-new migration files) is intentionally NOT blocked — only
# Edit on existing files. Use the Write tool to create NNNNNN_*.up.sql and
# NNNNNN_*.down.sql with the next sequential number.
#
# Fails closed: any parsing failure blocks the tool invocation.

if ! command -v jq >/dev/null 2>&1 || ! command -v realpath >/dev/null 2>&1; then
    echo "BLOCKED: jq and realpath are required for protect-applied-migrations.sh but are not installed." >&2
    exit 2
fi

REPO_ROOT="${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel 2>/dev/null)}"
if [ -z "$REPO_ROOT" ] || [ ! -d "$REPO_ROOT" ]; then
    echo "BLOCKED: Failed to resolve the repository root. Failing closed." >&2
    exit 2
fi
# Read the config with jq rather than sourcing scripts/repo-config.sh: this hook runs on
# every Edit, and sourcing would execute whatever that checked-out file contains.
MIGRATIONS_REL=$(jq -r '.migrations.dir // empty' "$REPO_ROOT/.claude/repo-config.json" 2>/dev/null || true)
MIGRATIONS_REL="${MIGRATIONS_REL:-migrations/}"
# No migrations directory in this repo => nothing to protect. Without this,
# a repo that simply doesn't have one (or hasn't created it yet) would fail
# closed on the realpath below and block EVERY Edit, not just migrations.
[ -d "$REPO_ROOT/${MIGRATIONS_REL%/}" ] || exit 0
MIGRATIONS_DIR=$(realpath "$REPO_ROOT/${MIGRATIONS_REL%/}") || {
    echo "BLOCKED: Failed to resolve the migrations directory. Failing closed." >&2
    exit 2
}

INPUT=$(cat)

if ! printf '%s' "$INPUT" | jq -e '
    type == "object" and
    (.tool_input | type == "object") and
    ([.tool_input.file_path?, .tool_input.path?, .tool_input.command?]
        | map(select(. != null))
        | all(type == "string"))
' >/dev/null 2>&1; then
    echo "BLOCKED: Failed to parse hook input in protect-applied-migrations.sh. Failing closed." >&2
    exit 2
fi

FILE_PATHS=$(printf '%s' "$INPUT" | jq -r '
    [.tool_input.file_path?, .tool_input.path?]
    | map(select(. != null))
    | .[]
')
PATCH_COMMAND=$(printf '%s' "$INPUT" | jq -r '.tool_input.command // empty')

if [ -n "$PATCH_COMMAND" ]; then
    if [[ "$PATCH_COMMAND" != '*** Begin Patch'* || "$PATCH_COMMAND" != *'*** End Patch' ]]; then
        echo "BLOCKED: Failed to parse hook input in protect-applied-migrations.sh. Failing closed." >&2
        exit 2
    fi
    PATCH_PATHS=$(printf '%s\n' "$PATCH_COMMAND" | sed -nE \
        -e 's/^\*\*\* (Update|Delete) File: (.+)$/\2/p' \
        -e 's/^\*\*\* Move to: (.+)$/\1/p')
    FILE_PATHS="${FILE_PATHS}${FILE_PATHS:+$'\n'}${PATCH_PATHS}"
elif [ -z "$FILE_PATHS" ]; then
    echo "BLOCKED: Failed to parse hook input in protect-applied-migrations.sh. Failing closed." >&2
    exit 2
fi

[ -z "$FILE_PATHS" ] && exit 0

canonicalize_path() {
    local raw_path=$1 candidate parent base canonical_parent

    candidate=$raw_path
    if [[ "$candidate" != /* ]]; then
        candidate="$REPO_ROOT/$candidate"
    fi

    if [[ -e "$candidate" || -L "$candidate" ]]; then
        realpath "$candidate"
        return
    fi

    parent=$(dirname "$candidate")
    base=$(basename "$candidate")
    # GNU realpath permits a missing final component by default and preserves
    # caller-supplied casing on a case-insensitive filesystem. Entering the
    # parent requires it to exist; external /bin/pwd -P then reports its
    # physical path and canonical casing on both GNU/Linux and macOS (Bash's
    # pwd builtin preserves caller-supplied casing on case-insensitive macOS).
    canonical_parent=$(cd "$parent" && /bin/pwd -P) || return 1
    printf '%s/%s\n' "$canonical_parent" "$base"
}

while IFS= read -r FILE_PATH; do
    if ! CANONICAL_PATH=$(canonicalize_path "$FILE_PATH"); then
        echo "BLOCKED: Failed to resolve hook path '$FILE_PATH'. Failing closed." >&2
        exit 2
    fi

    if [[ "$(dirname "$CANONICAL_PATH")" == "$MIGRATIONS_DIR" && "$(basename "$CANONICAL_PATH")" =~ ^[0-9]{6}_.*\.(up|down)\.sql$ ]]; then
        cat >&2 <<EOF
BLOCKED: Cannot Edit an existing migration file:
    $FILE_PATH

Migrations are immutable once written (.claude/rules/migrations.md). To change
schema behavior, add a NEW migration with the next sequential number using the
Write tool, e.g.:
    ${MIGRATIONS_REL%/}/000058_fix_something.up.sql
    ${MIGRATIONS_REL%/}/000058_fix_something.down.sql

If you genuinely need to correct an unreleased migration, make the edit
directly via your editor (this hook guards automated editing tools).
EOF
        exit 2
    fi
done <<< "$FILE_PATHS"

exit 0
