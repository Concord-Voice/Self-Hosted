#!/bin/bash
set -euo pipefail

# Commit message format checker for pre-commit framework
#
# Accepted forms (matched against the SUBJECT LINE only — first line of file):
#   <type>: <description>            — bare Conventional Commits prefix
#   <type>!: <description>           — breaking-change suffix per spec
#   Merge ...                        — git's merge-commit auto-format
#   Revert "..."                     — git revert --no-edit auto-format
# Types: feat, fix, refactor, docs, test, chore, style, perf, ci, revert
#
# Description must contain at least one non-whitespace character.

COMMIT_MSG_FILE="${1:?Missing commit message file argument}"
# Read only the subject line; multi-line bodies must not match accidentally.
SUBJECT="$(head -n 1 "$COMMIT_MSG_FILE")"

# Allow auto-generated merge / revert commits with their default messages.
if echo "$SUBJECT" | grep -qE '^(Merge |Revert ")'; then
  exit 0
fi

# Conventional Commits subject: type[!]: <non-whitespace + rest>
if ! echo "$SUBJECT" | grep -qE '^(feat|fix|refactor|docs|test|chore|style|perf|ci|revert)!?: [^[:space:]].*$'; then
  echo "❌ Invalid commit message format!"
  echo ""
  echo "Format: <type>[!]: <description>"
  echo "Types: feat, fix, refactor, docs, test, chore, style, perf, ci, revert"
  echo "Example: feat: add username validation"
  echo "Example: feat!: drop deprecated API (breaking change)"
  echo ""
  echo "Special cases (auto-generated, also accepted):"
  echo "  Merge ...        — git merge default"
  echo "  Revert \"...\"   — git revert --no-edit default"
  exit 1
fi

exit 0
