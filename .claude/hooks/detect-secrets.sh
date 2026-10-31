#!/bin/bash
# detect-secrets.sh — UserPromptSubmit hook
# Detects potential secrets or credentials pasted into prompts.
# Blocks submission (exit 2) if high-confidence patterns are found.

if ! command -v jq >/dev/null 2>&1; then
    echo "BLOCKED: jq is required for detect-secrets.sh but is not installed." >&2
    exit 2
fi

INPUT=$(cat)

if ! PROMPT=$(printf '%s' "$INPUT" | jq -r '.prompt // ""'); then
    echo "BLOCKED: Failed to parse prompt payload in detect-secrets.sh. Failing closed." >&2
    exit 2
fi

[ -z "$PROMPT" ] && exit 0

# High-confidence secret patterns (specific formats, not just keywords)
# These match actual secret values, not prose about passwords
PATTERNS=(
    # AWS keys (AKIA followed by 16 alphanumeric chars)
    'AKIA[0-9A-Z]{16}'
    # GitHub tokens (ghp_, gho_, ghu_, ghs_, ghr_ followed by alphanumeric)
    'gh[pousr]_[A-Za-z0-9_]{36,}'
    # Generic API keys (long hex or base64 strings preceded by key/token/secret)
    '(api[_-]?key|api[_-]?secret|access[_-]?token|auth[_-]?token)["\s:=]+[A-Za-z0-9/+]{32,}'
    # JWT tokens (three base64url segments separated by dots)
    'eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}'
    # Private keys
    '-----BEGIN (RSA |EC |OPENSSH )?PRIVATE KEY-----'
    # Connection strings with embedded passwords.
    # The userinfo groups exclude ':', '/', '@' and whitespace so they cannot
    # span prose: grep is line-oriented, and an unanchored [^:]+ / [^@]+ would
    # let a bare "redis://host:6379" plus any later '@' on the same line match
    # (e.g. an @mention or an email address) — a false positive that blocked
    # ordinary prompts. The username group is '*' not '+' so the empty-userinfo
    # form "redis://:secret@host" is caught; '+' missed it entirely.
    '(postgres|mysql|redis|mongodb)://[^:/@[:space:]]*:[^@/[:space:]]+@'
)

for pattern in "${PATTERNS[@]}"; do
    # -e marks the operand as a PATTERN so a pattern that starts with '-'
    # (e.g. the '-----BEGIN … PRIVATE KEY-----' one) is not parsed as grep
    # options. printf '%s\n' (not echo) so a PROMPT starting with -n/-e is
    # passed through literally. Both guard the leading-dash foot-gun.
    if printf '%s\n' "$PROMPT" | grep -qE -e "$pattern"; then
        echo "BLOCKED: Your prompt appears to contain a secret or credential (matched pattern: ${pattern:0:30}...). Please remove sensitive data before submitting." >&2
        exit 2
    fi
done

exit 0
