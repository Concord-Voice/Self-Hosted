#!/bin/bash
# destructive-commands.sh — PreToolUse hook for Bash
# Blocks destructive commands. It checks force-push, hard reset, git clean,
# recursive rm and working-tree checkout/restore itself. It is a best-effort regex
# guard, not a parser: it covers common forms, not every shell construct, so keep
# the .claude/settings.json deny list too. Without perl it blocks every Bash command.
#
# Scoped bypass: the `commit-commands:clean_gone` skill needs to issue
# `git branch -D` to clean up [gone] branches. A per-invocation env-var prefix
# opts out of the block for `git branch -D` ONLY. See .claude/hooks/README.md
# "Scoped bypass channels" for the discipline.

set -o pipefail

# Fail closed: if jq is missing or the input isn't valid JSON, refuse to allow.
# This matches the discipline used by protect-sensitive.sh — a safety hook
# that errors-out should block, not allow, to avoid silent bypass classes.
if ! command -v jq >/dev/null 2>&1; then
    echo "BLOCKED: jq not available — destructive-commands hook cannot evaluate command" >&2
    exit 2
fi

INPUT=$(cat)
COMMAND=$(printf '%s' "$INPUT" | jq -er '.tool_input.command // ""' 2>/dev/null) || {
    echo "BLOCKED: malformed hook input — destructive-commands cannot parse tool_input.command" >&2
    exit 2
}

[ -z "$COMMAND" ] && exit 0

# Scoped bypass for `git branch -D` invoked through the env-var prefix.
# The detection is STRING-BASED: we inspect the command text for the literal
# prefix, then validate the remainder is exactly `git branch -D <branch>`
# with no shell metacharacters (no chaining, no command substitution).
# Env-based detection is unsafe — the hook process doesn't inherit the
# subprocess env, and an exported var would silently grant blanket bypass.
#
# Branch token is matched in three accepted shapes (in priority order):
#   1. Bare:           git branch -D feature/foo
#   2. Double-quoted:  git branch -D "feature/foo"      (clean_gone.md form)
#   3. Single-quoted:  git branch -D 'feature/foo'
# In ALL shapes the branch token must START with an alphanumeric character to
# prevent flag-injection patterns like `-r` being interpreted as git flags
# (defense-in-depth — `git branch -D` itself doesn't honor flags after `-D`
# but the strict-validation contract should hold uniformly).
BYPASS_PREFIX_RE='^[[:space:]]*env[[:space:]]+CLAUDE_HOOK_ALLOW_BRANCH_DELETE=1[[:space:]]+(.+)$'
# Three alternatives in the branch-token group, each anchored to alphanumeric
# start: bare, double-quoted, single-quoted.
BRANCH_DELETE_RE='^git[[:space:]]+branch[[:space:]]+-D[[:space:]]+([A-Za-z0-9][A-Za-z0-9._/-]*|"[A-Za-z0-9][A-Za-z0-9._/-]*"|'"'"'[A-Za-z0-9][A-Za-z0-9._/-]*'"'"')[[:space:]]*$'
if [[ "$COMMAND" =~ $BYPASS_PREFIX_RE ]]; then
    REMAINDER="${BASH_REMATCH[1]}"
    if [[ "$REMAINDER" =~ $BRANCH_DELETE_RE ]]; then
        echo "BYPASS: branch delete authorized (CLAUDE_HOOK_ALLOW_BRANCH_DELETE=1): $REMAINDER" >&2
        exit 0
    fi
    # Prefix present but remainder doesn't match strict pattern — fall through
    # to the standard block. The bypass is scoped to `git branch -D <branch>`
    # ONLY; chained commands, other destructive ops, or branch names with
    # shell metacharacters are not allowed.
fi

# Work-discarding and history-rewriting git/rm operations, best effort: a regex
# over the command text, not a parser. Git patterns only match in command
# position (see CMD_POS below), so a commit message
# that mentions one is not blocked. Quoted strings are dropped before matching for
# the same reason. --force-with-lease (with or without --force-if-includes) and
# index-only `git restore --staged` stay allowed.
# A quoted script handed to sh/bash/zsh -c or eval is executed, so it is scanned too.
# Quoted spans are read left to right, so the first quote opens the span and an
# apostrophe inside double quotes is inert. A quoted single word ("." or 'src') is an
# argument, so it is kept. A double-quoted span holding $( or a backtick is executed
# by the shell, so its text stays visible. Any other quoted span is prose and becomes
# the placeholder word `_`, so a quoted option value such as -C "/tmp/a b" stays one word.
SCAN=$(printf '%s' "$COMMAND" | perl -0777 -pe 's/"((?:[^"\\]|\\.)*)"|\x27([^\x27]*)\x27/my $d = defined $1; my $t = $d ? $1 : $2; ($d && $t =~ m{\$\(|`}) ? " $t " : ($t =~ m{^\S*$} ? $t : " _ ")/ge') || {
    echo "BLOCKED: perl not available or failed — destructive-commands hook cannot evaluate command" >&2
    exit 2
}
# Quote-aware, so an escaped quote inside the script (bash -c "echo \"a\" && rm -rf x") does not end it.
INNER=$(printf '%s' "$COMMAND" | perl -0777 -ne 'while (/(?:\b(?:sh|bash|zsh)\s+-[a-zA-Z]*c|\beval)\s+(?:"((?:[^"\\]|\\.)*)"|\x27([^\x27]*)\x27)/g) { my $t = defined $1 ? $1 : $2; $t =~ s/\\(.)/$1/g if defined $1; print "$t\n" }') || {
    echo "BLOCKED: perl failed — destructive-commands hook cannot evaluate command" >&2
    exit 2
}
[ -n "$INNER" ] && SCAN="$SCAN; $(printf '%s' "$INNER" | tr '\n' ';')"
# Command position: start, after ; & | ( { ! or a backtick, then any VAR=value
# assignments and wrapper words (then/do/else, sudo, env, time, nohup, command,
# exec, xargs and their flags) that run the next word as the command.
CMD_POS='(^|[;&|({!`])[[:space:]]*(([A-Za-z_][A-Za-z0-9_]*=[^[:space:]]*|then|do|else|sudo|env|time|nohup|command|exec|xargs|-[a-zA-Z-]+)[[:space:]]+)*'
# An absolute or relative path to git (/usr/bin/git) counts as git. Global options
# such as -C <path>, --work-tree <path>, -c <name>=<value> or --no-pager may sit
# before the subcommand.
GIT='([^[:space:];&|]*/)?git(\s+(-[Cc]\s+[^[:space:];&|]+|--(git-dir|work-tree|namespace)\s+[^[:space:];&|]+|--[a-z][a-z-]*(=[^[:space:];&|]*)?))*'
DESTRUCTIVE_RE="${CMD_POS}${GIT}\s+push\b[^;&|]*(\s--force(\s|$)|\s-[a-zA-Z]*f[a-zA-Z]*(\s|$)|\s\+[^[:space:]]+)"
DESTRUCTIVE_RE+="|${CMD_POS}${GIT}\s+reset\b[^;&|]*\s--hard"
DESTRUCTIVE_RE+="|${CMD_POS}${GIT}\s+clean\b[^;&|]*\s(-[a-zA-Z]*f|--force)"
DESTRUCTIVE_RE+="|${CMD_POS}${GIT}\s+checkout\b[^;&|]*\s\.{1,2}/?(\s|$)"
RESTORE_RE="${CMD_POS}${GIT}\s+restore\b[^;&|]*\s\.{1,2}/?(\s|$)"
DESTRUCTIVE_RE+="|(^|[;&|([:space:]])rm(\s+[^;&|]*)?\s(-[a-zA-Z]*[rR][a-zA-Z]*|--recursive)(\s|$)"
restore_hits_worktree() { # --staged alone only touches the index; judged per command segment
    local seg
    while IFS= read -r seg; do
        echo "$seg" | grep -qE "$RESTORE_RE" || continue
        echo "$seg" | grep -qE '\s(--staged|-S)(\s|$)' || return 0
        echo "$seg" | grep -qE '\s(--worktree(=[^[:space:]]*)?|-[a-zA-Z]*W[a-zA-Z]*)(\s|$)' && return 0
    done < <(printf '%s\n' "$SCAN" | tr ';&|' '\n')
    return 1
}
if echo "$SCAN" | grep -qE "$DESTRUCTIVE_RE" || restore_hits_worktree; then
    echo "BLOCKED: Potentially destructive command detected: $COMMAND" >&2
    exit 2
fi

if echo "$COMMAND" | grep -qE '(DROP\s+(TABLE|DATABASE|INDEX)|TRUNCATE\s|DELETE\s+FROM|git\s+branch\s+-D|git\s+stash\s+drop|git\s+reflog\s+expire|pkill|killall)'; then
    echo "BLOCKED: Potentially destructive command detected: $COMMAND" >&2
    # If the command matches the bare `git branch -D` pattern, hint at the
    # scoped-bypass channel for users running bulk cleanup intentionally.
    if echo "$COMMAND" | grep -qE 'git\s+branch\s+-D'; then
        echo "HINT: For bulk cleanup of [gone] branches, use commit-commands:clean_gone" >&2
        echo "      or prepend 'env CLAUDE_HOOK_ALLOW_BRANCH_DELETE=1 ' per .claude/hooks/README.md" >&2
    fi
    exit 2
fi

exit 0
