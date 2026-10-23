#!/usr/bin/env bash
# Validate the merged-main provenance required before an ordinary main push
# may skip the standalone OSV scan. Unknown or ambiguous metadata stays unknown.
set -euo pipefail

emit() {
  if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
    printf '%s=%s\n' "$1" "$2" >> "$GITHUB_OUTPUT"
  else
    printf '%s=%s\n' "$1" "$2"
  fi
}

emit valid false
emit hotfix false

repo=${GITHUB_REPOSITORY:-}
before=${BEFORE_SHA:-}
after=${AFTER_SHA:-}
via=${RESOLVED_VIA:-}
resolution=${HOTFIX_RESOLUTION:-}
hotfix=${HOTFIX:-}
pr_number=${PR_NUMBER:-}

[[ "$repo" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || exit 0
[[ "$before" =~ ^[0-9a-f]{40}$ && "$after" =~ ^[0-9a-f]{40}$ ]] || exit 0
[[ "$before" != 0000000000000000000000000000000000000000 &&
   "$after" != 0000000000000000000000000000000000000000 ]] || exit 0
[[ "$pr_number" =~ ^[1-9][0-9]*$ ]] || exit 0
[[ "$via" == subject || "$via" == association ]] || exit 0
[[ "$resolution" == subject || "$resolution" == association ||
   "$resolution" == no-label ]] || exit 0
[[ "$hotfix" == true || "$hotfix" == false ]] || exit 0
if [[ "$resolution" == no-label && "$hotfix" != false ]] ||
   [[ "$resolution" != no-label && "$hotfix" != true ]]; then
  exit 0
fi

tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT
if ! gh api "repos/$repo/pulls/$pr_number" > "$tmp_dir/pr.json" 2>/dev/null; then
  exit 0
fi
if ! jq -e --arg sha "$after" --argjson number "$pr_number" '
  .number == $number and
  .state == "closed" and
  .merged == true and
  (.merged_at | type == "string" and length > 0) and
  .base.ref == "main" and
  .merge_commit_sha == $sha
' "$tmp_dir/pr.json" >/dev/null; then
  exit 0
fi

if [[ "$via" == association ]]; then
  if ! gh api "repos/$repo/commits/$after/pulls" --paginate --jq '.[]' \
      > "$tmp_dir/associations.jsonl" 2>/dev/null; then
    exit 0
  fi
  # The shared hotfix resolver intentionally reads the first association for
  # its release contract. OSV is stricter: association fallback is skippable
  # only when the endpoint proves exactly one PR and it is the verified merge.
  if ! jq -se --arg sha "$after" --argjson number "$pr_number" '
    length == 1 and
    .[0].number == $number and
    .[0].state == "closed" and
    .[0].merged == true and
    (.[0].merged_at | type == "string" and length > 0) and
    .[0].base.ref == "main" and
    .[0].merge_commit_sha == $sha
  ' "$tmp_dir/associations.jsonl" >/dev/null; then
    exit 0
  fi
fi

emit valid true
emit hotfix "$hotfix"
emit before "$before"
emit after "$after"
emit pr_number "$pr_number"
emit resolved_via "$via"
