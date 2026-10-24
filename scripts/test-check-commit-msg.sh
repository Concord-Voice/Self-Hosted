#!/bin/bash
# Test fixture for scripts/check-commit-msg.sh
# Run manually: ./scripts/test-check-commit-msg.sh
# Exits non-zero if any case fails.

set -euo pipefail
HOOK="$(dirname "$0")/check-commit-msg.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

PASSES=0
FAILS=0
FAIL_DETAILS=()

run() {
  local label="$1"
  local msg="$2"
  local expect="$3"  # PASS or FAIL
  echo "$msg" > "$TMP/m"
  if "$HOOK" "$TMP/m" >/dev/null 2>&1; then
    actual=PASS
  else
    actual=FAIL
  fi
  if [ "$actual" = "$expect" ]; then
    PASSES=$((PASSES + 1))
    printf '  \033[32m✓\033[0m %s (%s expected, got %s)\n' "$label" "$expect" "$actual"
  else
    FAILS=$((FAILS + 1))
    printf '  \033[31m✗\033[0m %s (%s expected, got %s) — message: %q\n' "$label" "$expect" "$actual" "$msg"
    FAIL_DETAILS+=("$label")
  fi
}

echo "=== Allowed Conventional Commits types (bare prefix) ==="
run "feat:" "feat: add username validation" PASS
run "fix:" "fix: resolve race condition" PASS
run "refactor:" "refactor: extract token helper" PASS
run "docs:" "docs: update README" PASS
run "test:" "test: add integration test" PASS
run "chore:" "chore: bump dependency" PASS
run "style:" "style: format files" PASS
run "perf:" "perf: cache token result" PASS
run "ci:" "ci: tweak workflow trigger" PASS
run "revert:" "revert: revert PR #999" PASS

echo ""
echo "=== Breaking-change suffix (!) ==="
run "feat!:" "feat!: drop deprecated API" PASS
run "fix!:" "fix!: change default config to breaking value" PASS
run "ci!:" "ci!: switch to required check (breaking)" PASS

echo ""
echo "=== Special-case auto-generated messages ==="
run "Merge branch" "Merge branch 'main' into feature/foo" PASS
run "Merge tag" "Merge tag 'v1.0.0'" PASS
run 'Revert "..."' 'Revert "feat: add username validation"' PASS
run 'Revert ""' 'Revert ""' PASS

echo ""
echo "=== Multi-line — only subject line is checked ==="
run "valid subject + body" "$(printf 'feat: add login\n\nMore details in the body.\nThird line.')" PASS
run "invalid subject + body" "$(printf 'random: foo\n\nA body that has feat: in it should not save the commit.')" FAIL

echo ""
echo "=== Invalid forms ==="
run "unknown type" "random: not a real type" FAIL
run "parenthesized scope" "feat(scope): paren-form rejected per project memory" FAIL
run "no type at all" "no type at all" FAIL
run "uppercase type" "FEAT: uppercase" FAIL
run "empty subject" "" FAIL
run "type with no description" "feat:" FAIL
run "type with whitespace-only description" "feat:   " FAIL
run "type missing colon" "feat add login" FAIL
run "trailing-only colon-space" "feat: " FAIL
run "Merge-shaped imposter without space" "Merge:fake" FAIL
run "Revert-shaped imposter without quotes" "Revert: fake without-quotes" FAIL

echo ""
echo "─────────────────────────────────────────────"
printf 'Passed: \033[32m%d\033[0m   Failed: \033[31m%d\033[0m\n' "$PASSES" "$FAILS"
if [ "$FAILS" -gt 0 ]; then
  echo ""
  echo "Failed cases:"
  printf '  - %s\n' "${FAIL_DETAILS[@]}"
  exit 1
fi
exit 0
