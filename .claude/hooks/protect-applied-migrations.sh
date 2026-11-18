#!/bin/bash
# protect-applied-migrations.sh — PreToolUse hook for Edit|Write
#
# Blocks edits to existing migration files in the migrations directory (default
# migrations/; override via .claude/repo-config.json's .migrations.dir — same
# knob migration-reviewer reads). Migrations are immutable once written — the
# rule (.claude/rules/migrations.md) is that every up migration must have a
# corresponding down migration and you never modify a shipped migration in
# place; you add a new one instead.
#
# Write is allowed only for a migration file that does not exist yet, so a new
# NNNNNN_*.up.sql / NNNNNN_*.down.sql can be created; Write onto an existing one
# is an overwrite and is blocked like Edit.
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

TAB=$'\t'

fail_parse() {
    echo "BLOCKED: Failed to parse hook input in protect-applied-migrations.sh. Failing closed." >&2
    exit 2
}

INPUT=$(cat)

# Path fields must be strings without control characters: a newline would split
# one path into several harmless-looking entries below.
printf '%s' "$INPUT" | jq -e '
    type == "object" and
    (.tool_input | type == "object") and
    ([.tool_input.file_path?, .tool_input.path?, .tool_input.command?]
        | map(select(. != null))
        | all(type == "string")) and
    ([.tool_input.file_path?, .tool_input.path?]
        | map(select(. != null))
        | all(explode | any(. < 32 or . == 127) | not)) and
    ((.cwd? // "") | type == "string")
' >/dev/null 2>&1 || fail_parse

TOOL_NAME=$(printf '%s' "$INPUT" | jq -r '.tool_name // empty')
PATCH_COMMAND=$(printf '%s' "$INPUT" | jq -r '.tool_input.command // empty')
# The editing tool resolves a relative path against the session cwd, not the
# project root, so resolve it the same way.
HOOK_CWD=$(printf '%s' "$INPUT" | jq -r '.cwd // empty')
REL_BASE="${HOOK_CWD:-$REPO_ROOT}"
if [ ! -d "$REL_BASE" ]; then
    echo "BLOCKED: Failed to resolve the session working directory. Failing closed." >&2
    exit 2
fi

# One "mode<TAB>path" entry per path. A "create" may make a file that does not
# exist yet; an "edit" must never reach an existing migration.
if [[ "$TOOL_NAME" = "Write" ]]; then FIELD_MODE=create; else FIELD_MODE=edit; fi
ENTRIES=$(printf '%s' "$INPUT" | jq -r --arg m "$FIELD_MODE" '
    [.tool_input.file_path?, .tool_input.path?]
    | map(select(. != null))
    | .[]
    | $m + "\t" + .
')

if [ -n "$PATCH_COMMAND" ]; then
    if [[ "$PATCH_COMMAND" != '*** Begin Patch'* || "$PATCH_COMMAND" != *'*** End Patch' ]]; then
        fail_parse
    fi
    # apply_patch trims hunk header lines, so match markers after leading blanks
    # and regardless of case. Add File overwrites an existing file: check it
    # like Write.
    PATCH_ENTRIES=$(printf '%s\n' "$PATCH_COMMAND" | awk '
        { t = $0; sub(/^[ \t]+/, "", t); l = tolower(t) }
        index(l, "*** add file: ") == 1    { print "create\t" substr(t, 15); next }
        index(l, "*** update file: ") == 1 { print "edit\t" substr(t, 18); next }
        index(l, "*** delete file: ") == 1 { print "edit\t" substr(t, 18); next }
        index(l, "*** move to: ") == 1     { print "edit\t" substr(t, 14); next }
    ')
    ENTRIES="${ENTRIES}${ENTRIES:+$'\n'}${PATCH_ENTRIES}"
elif [ -z "$ENTRIES" ]; then
    fail_parse
fi

[ -z "$ENTRIES" ] && exit 0

canonicalize_path() {
    local raw_path=$1 mode=$2 candidate parent base canonical_parent next_parent segment

    # Refuse spellings the editing tool resolves differently from this hook:
    # it trims surrounding whitespace and expands a leading ~ to $HOME, and a
    # control character has no place in a real path.
    [[ -n "$raw_path" ]] || return 1
    [[ "$raw_path" != *[[:cntrl:]]* ]] || return 1
    [[ "$raw_path" != [[:space:]]* && "$raw_path" != *[[:space:]] ]] || return 1
    [[ "$raw_path" != "~"* ]] || return 1

    candidate=$raw_path
    if [[ "$candidate" != /* ]]; then
        candidate="$REL_BASE/$candidate"
    fi

    if [[ -e "$candidate" || -L "$candidate" ]]; then
        realpath "$candidate"
        return
    fi

    parent=$(dirname "$candidate")
    base=$(basename "$candidate")
    # A missing final `.` or `..` names a directory, never a migration file; refuse it too.
    [[ "$base" != "." && "$base" != ".." ]] || return 1
    # Only a create can need missing directories (Write makes them). An edit
    # whose parent is missing cannot be resolved, so it fails closed below.
    if [[ "$mode" = "create" ]]; then
        # Refuse ambiguous traversal and broken symlinks in the missing suffix;
        # otherwise a later mkdir could make a seemingly new path reach an
        # existing migration through `..` or a symlink.
        while [[ ! -d "$parent" ]]; do
            [[ -e "$parent" || -L "$parent" ]] && return 1
            next_parent=$(dirname "$parent")
            [[ "$next_parent" != "$parent" ]] || return 1
            # An [[ ]] test, not a case pattern: Semgrep 1.121's bash parser rejects `.|..)`.
            segment=$(basename "$parent")
            [[ "$segment" != "." && "$segment" != ".." ]] || return 1
            base="$segment/$base"
            parent=$next_parent
        done
    fi
    # GNU realpath permits a missing final component by default and preserves
    # caller-supplied casing on a case-insensitive filesystem. Entering the
    # parent requires it to exist; external /bin/pwd -P then reports its
    # physical path and canonical casing on both GNU/Linux and macOS (Bash's
    # pwd builtin preserves caller-supplied casing on case-insensitive macOS).
    canonical_parent=$(cd "$parent" && /bin/pwd -P) || return 1
    printf '%s/%s\n' "$canonical_parent" "$base"
}

# Same file (device + inode) as a shipped migration, whatever the spelling:
# covers MIGRATIONS/ vs migrations/ and other case-folded names.
is_existing_migration() {
    local m
    for m in "$MIGRATIONS_DIR"/[0-9][0-9][0-9][0-9][0-9][0-9]_*.up.sql "$MIGRATIONS_DIR"/[0-9][0-9][0-9][0-9][0-9][0-9]_*.down.sql; do
        [[ -e "$m" && "$1" -ef "$m" ]] && return 0
    done
    return 1
}

block() {
    cat >&2 <<EOF
BLOCKED: Cannot ${TOOL_NAME:-Edit} an existing migration file:
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
}

while IFS= read -r ENTRY; do
    MODE=${ENTRY%%"$TAB"*}
    FILE_PATH=${ENTRY#*"$TAB"}
    if ! CANONICAL_PATH=$(canonicalize_path "$FILE_PATH" "$MODE"); then
        echo "BLOCKED: Failed to resolve hook path '$FILE_PATH'. Failing closed." >&2
        exit 2
    fi

    # A create of a brand-new file is how new migrations are added.
    if [[ "$MODE" = "create" ]] && [[ ! -e "$CANONICAL_PATH" ]] && [[ ! -L "$CANONICAL_PATH" ]]; then
        continue
    fi
    if [[ -e "$CANONICAL_PATH" ]] && is_existing_migration "$CANONICAL_PATH"; then
        block
    fi
    if [[ "$(dirname "$CANONICAL_PATH")" == "$MIGRATIONS_DIR" && "$(basename "$CANONICAL_PATH")" =~ ^[0-9]{6}_.*\.(up|down)\.sql$ ]]; then
        block
    fi
done <<< "$ENTRIES"

exit 0
