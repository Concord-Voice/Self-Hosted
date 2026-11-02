#!/bin/bash
# issue-creation-nudge.sh — UserPromptSubmit hook
# Detects intent to create a GitHub issue and suggests /issue-creation
# for consistent, dev-lifecycle-ready intake.
# NON-BLOCKING: always exits 0; prints reminder to stderr only.

INPUT=$(cat)

command -v jq >/dev/null 2>&1 || exit 0

PROMPT=$(echo "$INPUT" | jq -r '.prompt // .user_prompt // empty' 2>/dev/null || printf '')
[ -z "$PROMPT" ] && exit 0

# Lowercase for matching (case-insensitive)
PROMPT_LC=$(echo "$PROMPT" | tr '[:upper:]' '[:lower:]')

# Already invoking /issue-creation? Skip to avoid echo.
if [[ "$PROMPT_LC" =~ /issue-creation ]]; then
    exit 0
fi

# Intent patterns — order matters, first match wins
# Deliberately narrow: only trigger on clear "create a new issue" intent,
# not on mentions like "this issue exists" or "look at issue #123".
INTENT_DETECTED=0

if [[ "$PROMPT_LC" =~ (create|file|open|make|add|draft|start|write).{0,20}(new |a )?(github )?issue ]]; then
    INTENT_DETECTED=1
elif [[ "$PROMPT_LC" =~ (new|another)[[:space:]]+(github[[:space:]]+)?issue ]]; then
    INTENT_DETECTED=1
elif [[ "$PROMPT_LC" =~ issue[[:space:]]+for[[:space:]]+(this|that|the) ]]; then
    INTENT_DETECTED=1
elif [[ "$PROMPT_LC" =~ (track|capture)[[:space:]]+(this|that|it)[[:space:]]+(as|in)[[:space:]]+(an?[[:space:]]+)?issue ]]; then
    INTENT_DETECTED=1
fi

if [ "$INTENT_DETECTED" -eq 1 ]; then
    cat >&2 <<'EOF'
Reminder: Detected intent to create a GitHub issue. Use `/issue-creation [topic]` for consistent, `/dev-lifecycle`-ready intake.

The skill will:
  1. Scan existing issues for duplicates / parent-epic fits / adjacent clusters
  2. Assess scope (single issue vs parent + children)
  3. Generate a pre-spec with Context7 docs, `.claude/rules/` citations, and 2–3 implementation options
  4. Apply canonical labels + milestone + priority
  5. Embed dependencies, related issues, and recent PRs
  6. Preview before creating

Skip nudge by running `/issue-creation` directly, or override with `gh issue create` if you're doing scripted creation (e.g. from `/dev-lifecycle`).
EOF
fi

exit 0
