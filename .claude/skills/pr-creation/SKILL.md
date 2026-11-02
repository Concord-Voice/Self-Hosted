---
name: pr-creation
description: Create a well-labeled, project-tracked, assignee-set PR. Lightweight mirror of /issue-creation focused on PR metadata — inherits labels from associated issue(s) referenced via Closes/Fixes/Resolves/Refs, syncs project board Status to "In review" (when a board is configured), auto-assigns the gh-authenticated user, validates pre-merge invariants. Dispatched automatically by /dev-lifecycle after `gh pr create`. Also use whenever a PR is created ad-hoc outside the /dev-lifecycle flow to apply correct metadata.
argument-hint: "[pr-number]"
allowed-tools:
  - Bash(gh pr *)
  - Bash(gh issue *)
  - Bash(gh api *)
  - Bash(gh repo *)
  - Read
  - Grep
---

# /pr-creation

Sync PR #$0 metadata with its underlying issue(s).

**Configuration (org schema):** this skill is config-driven via the OPTIONAL per-repo
`.claude/repo-config.json`, read through `scripts/repo-config.sh` (`rc_get '<jq-filter>' [default]`,
`rc_has '<jq-filter>'`). The keys it consumes: `.project*` (board id, status/priority field ids
and option ids), `.labels.namespaces`, and `.labels.source`. Owner/repo are auto-derived from `gh`.
Every project-board operation is gated on `rc_has '.project'` — when no board is configured, the skill
applies labels + milestone + assignee only and skips all board sync. `lib.sh` already sources the
config reader and defaults its env vars from it.

**Canonical reference (labels):** the labels source file (config `.labels.source`,
default `.claude/rules/labels.md`) — label inheritance is validated against its Active section
when the file exists. If the file is absent, validation degrades to passthrough.

**Catalog Dispatch:** when this repo defines a shared dispatchable-skills catalog
(`.claude/skills/_shared/dispatchable-skills.md`), this skill's catalog obligation is narrow — it
carries the docs-in-PR invariant (Phase 5 WARN below). It does NOT dispatch design-class skills:
the code is already written by PR-creation time, so design belongs upstream (`/issue-creation`,
`/dev-lifecycle`).

**Reviewer field is intentionally NOT modified.** Reviewer assignment is a manual decision.
A repo may add an automated-reviewer-assignment workflow later.

## Execution

All phases source the helpers in `.claude/skills/pr-creation/lib.sh` (which in turn sources
`scripts/repo-config.sh` for `rc_get`/`rc_has`):

```bash
REPO_ROOT=$(git rev-parse --show-toplevel)
source "${REPO_ROOT}/.claude/skills/pr-creation/lib.sh"
```

### Phase 1 — Resolve underlying issue(s)

Parse PR body for closing-keyword references:

```bash
PR_NUMBER=$0
PR_BODY=$(gh pr view "$PR_NUMBER" --json body --jq .body)
CLOSES=$(parse_pr_closes "$PR_BODY")
REFS=$(parse_pr_refs "$PR_BODY")
```

If `$CLOSES` is empty: emit a warning and skip Phase 2 (no labels to inherit) AND skip Phase 3 (no Status change). Proceed to Phase 4 (assignee + invariant validation still apply).

### Phase 2 — Inherit labels from issue(s)

Union of labels across all closing-referenced AND refs-referenced issues. Filter out `routine:*` and `type: epic`, then validate against the Active labels section in the configured labels source (`.labels.source`, default `.claude/rules/labels.md`). When that file is absent, `validate_against_active_labels` passes labels through unchanged.

```bash
ALL_LABELS=""
for issue_n in $CLOSES $REFS; do
    # Explicit error handling on the fetch — $(...) doesn't propagate exit codes,
    # so a rate-limited or auth-failed gh would silently produce empty labels.
    if ! ISSUE_LABELS=$(gh issue view "$issue_n" --json labels --jq '[.labels[].name]' 2>&1); then
        echo "WARN: failed to fetch labels for #${issue_n}: ${ISSUE_LABELS}" >&2
        continue
    fi
    INHERITED=$(filter_inherited_labels "$ISSUE_LABELS")
    ALL_LABELS="${ALL_LABELS}${INHERITED}"$'\n'
done

# Dedupe + validate against the labels source Active section (no-op passthrough
# if no labels source file exists). Use printf '%s\n' (not echo) for consistency
# with the printf-not-echo discipline documented in lib.sh — see move_to_status comment.
VALIDATED=$(printf '%s\n' "$ALL_LABELS" | sort -u | grep -v '^$' | validate_against_active_labels)

if [ -n "$VALIDATED" ]; then
    LABEL_CSV=$(printf '%s\n' "$VALIDATED" | paste -sd, -)
    gh pr edit "$PR_NUMBER" --add-label "$LABEL_CSV"
fi
```

### Phase 3 — Sync project board Status (issues + the PR itself)

**This entire phase (3a/3b/3c) is gated on a configured project board.** If `.project` is
absent from `repo-config.json`, skip straight to Phase 4 — there is no board to sync.

```bash
if ! rc_has '.project'; then
    echo "INFO: no project board configured (.project absent); skipping Phase 3 board sync." >&2
    # ... proceed to Phase 4 ...
else
    # Resolve the "In review" status option id from config. Required for board sync.
    IN_REVIEW_OPTION_ID=$(rc_get '.project.fields.status.options.in_review')
    if [ -z "$IN_REVIEW_OPTION_ID" ]; then
        echo "WARN: .project is configured but .project.fields.status.options.in_review is unset; cannot sync Status. Skipping Phase 3 Status moves." >&2
    fi
fi
```

Everything in 3a/3b/3c below runs **only** inside the `rc_has '.project'` branch.

#### Phase 3a — Issue Status sync

For each `Closes` issue (NOT `Refs`), find its project board item and move to "In review":

```bash
for issue_n in $CLOSES; do
    # find_project_item_id distinguishes "not on board" (empty + exit 0)
    # from "API/network failure" (empty + exit 1 + stderr WARN). Capture both.
    if ! ITEM_ID=$(find_project_item_id "$issue_n"); then
        echo "WARN: find_project_item_id failed for #${issue_n}; skipping Status sync for this issue" >&2
        continue
    fi
    if [ -z "$ITEM_ID" ]; then
        echo "WARN: issue #${issue_n} is not on the project board; skipping Status sync" >&2
        continue
    fi
    # move_to_status now propagates GraphQL errors envelope as non-zero;
    # surface the per-issue failure but continue with subsequent issues
    # so a single bad item doesn't block the rest of the batch.
    if ! move_to_status "$ITEM_ID" "$IN_REVIEW_OPTION_ID"; then
        echo "WARN: failed to move issue #${issue_n} to In review (see previous error)" >&2
    fi
done
```

For `Refs` (non-closing) issues, do NOT change Status — those aren't being closed by this PR.

#### Phase 3b — PR Status sync

**ALL PRs are treated as in-review, even in draft mode** — so the PR itself is also added to the project board and moved to `Status: In review`. `ensure_pr_on_project` is idempotent: it adds the PR if not present, or returns the existing item ID.

```bash
if ! PR_ITEM_ID=$(ensure_pr_on_project "$PR_NUMBER"); then
    echo "WARN: failed to add PR #${PR_NUMBER} to project board; skipping PR Status + Priority sync" >&2
    PR_ITEM_ID=""
fi

if [ -n "$PR_ITEM_ID" ]; then
    if ! move_to_status "$PR_ITEM_ID" "$IN_REVIEW_OPTION_ID"; then
        echo "WARN: failed to move PR #${PR_NUMBER} to In review (see previous error)" >&2
    fi
fi
```

### Phase 3c — Inherit Project Priority onto the PR

PRs should carry the same Project Priority as their underlying issue. Read the first `Closes` issue's Priority option ID and apply it to the PR's project item.

**Important:** `set_priority` calls `${PRIORITY_FIELD_ID:?}` — it aborts (non-zero) when the Priority field is not configured. Gate the call with an explicit pre-check so this branch is simply skipped for repos without a Priority field, rather than aborting the skill with an unhelpful `:?` message.

```bash
FIRST_CLOSES=$(printf '%s' "$CLOSES" | head -n1)
# Only attempt Priority inheritance when the repo config defines the Priority field.
# set_priority uses ${PRIORITY_FIELD_ID:?} internally — calling it without the
# field configured aborts the skill. The explicit pre-check keeps the skip path
# visible and avoids masking real failures with `if ! ...`.
if [ -n "$FIRST_CLOSES" ] && [ -n "$PR_ITEM_ID" ] \
        && rc_has '.project.fields.priority.id' \
        && [ -n "$(rc_get '.project.fields.priority.id')" ]; then
    if ! ISSUE_PRIORITY=$(get_issue_priority_option_id "$FIRST_CLOSES"); then
        echo "WARN: could not read Priority from issue #${FIRST_CLOSES}; skipping PR Priority sync" >&2
    elif [ -z "$ISSUE_PRIORITY" ]; then
        echo "INFO: issue #${FIRST_CLOSES} has no Priority set; nothing to inherit" >&2
    else
        if ! set_priority "$PR_ITEM_ID" "$ISSUE_PRIORITY"; then
            echo "WARN: failed to set Priority on PR #${PR_NUMBER} (see previous error)" >&2
        else
            echo "INFO: inherited Priority from issue #${FIRST_CLOSES} onto PR #${PR_NUMBER}" >&2
        fi
    fi
fi
```

### Phase 4 — Auto-assign the gh-authenticated user + inherit milestone

```bash
GITHUB_USER=$(get_authenticated_user)
if [ -n "$GITHUB_USER" ]; then
    gh pr edit "$PR_NUMBER" --add-assignee "$GITHUB_USER"
fi
# (Reviewer field intentionally NOT modified — see top-of-file note.)

# Inherit milestone from the first Closes issue (symmetric with /issue-creation,
# which SETS milestone explicitly). Without this, a PR auto-closes its issue at
# merge but the milestone stays on the issue — the release-tracking view loses
# the PR.
FIRST_CLOSES=$(printf '%s' "$CLOSES" | head -n1)
if [ -n "$FIRST_CLOSES" ]; then
    ISSUE_MILESTONE=$(gh issue view "$FIRST_CLOSES" --json milestone --jq '.milestone.title // empty' 2>/dev/null || true)
    PR_MILESTONE=$(gh pr view "$PR_NUMBER" --json milestone --jq '.milestone.title // empty' 2>/dev/null || true)
    if [ -n "$ISSUE_MILESTONE" ] && [ -z "$PR_MILESTONE" ]; then
        gh pr edit "$PR_NUMBER" --milestone "$ISSUE_MILESTONE" \
            && echo "INFO: inherited milestone '$ISSUE_MILESTONE' from issue #${FIRST_CLOSES}" >&2 \
            || echo "WARN: failed to set milestone '$ISSUE_MILESTONE' on PR" >&2
    fi
fi
```

### Phase 5 — Validate pre-merge invariants

```bash
PR_META=$(gh pr view "$PR_NUMBER" --json labels,milestone,assignees,body)
# Use printf '%s' not echo — the PR body contains newlines which gh returns
# as `\n` escapes inside the JSON string; xpg_echo would expand them back to
# literal newlines and corrupt the JSON before jq sees it. Same footgun as
# move_to_status (see lib.sh).
HAS_LABEL=$(printf '%s' "$PR_META" | jq -r '.labels | length > 0' | sed 's/true/--has-label/;s/false/--no-label/')
HAS_MILESTONE=$(printf '%s' "$PR_META" | jq -r '.milestone != null' | sed 's/true/--has-milestone/;s/false/--no-milestone/')
HAS_ASSIGNEE=$(printf '%s' "$PR_META" | jq -r '.assignees | length > 0' | sed 's/true/--has-assignee/;s/false/--no-assignee/')
if [ -n "$CLOSES" ]; then HAS_CLOSES="--has-closes"; else HAS_CLOSES="--no-closes"; fi

validate_pr_invariants "$HAS_LABEL" "$HAS_MILESTONE" "$HAS_ASSIGNEE" "$HAS_CLOSES"

# Docs-in-PR drift WARN (non-blocking, advisory). If the PR changes code but
# touches no docs, remind the author to update docs IN THIS PR rather than a
# follow-up. Heuristic only. This WARNs only — Phase 5 is advisory (exits 0).
CHANGED=$(gh pr diff "$PR_NUMBER" --name-only 2>/dev/null || true)
CODE_CHANGED=$(printf '%s\n' "$CHANGED" | grep -vE '\.(md)$|^docs/|^\.claude/rules/' || true)
if [ -n "$CODE_CHANGED" ]; then
    DOC_TOUCHED=$(printf '%s\n' "$CHANGED" | grep -E '\.(md)$|^docs/|^\.claude/rules/|^CLAUDE\.md$' || true)
    if [ -z "$DOC_TOUCHED" ]; then
        echo "WARN: PR changes code but updates no docs. Verify no documentation was made stale; docs that describe changed behavior should ship in THIS PR, not a follow-up." >&2
    fi
fi
```

## Project board IDs (config-driven)

All board ids live in `.claude/repo-config.json` under `.project` — there are no hard-coded ids
in this skill. Read them (only when `rc_has '.project'`):

```bash
PROJECT_ID=$(rc_get '.project.id')                                  # PVT_… board node id
STATUS_FIELD_ID=$(rc_get '.project.fields.status.id')               # Status single-select field
PRIORITY_FIELD_ID=$(rc_get '.project.fields.priority.id')           # Priority single-select field
IN_REVIEW_OPTION_ID=$(rc_get '.project.fields.status.options.in_review')
# Priority option ids are keyed under .project.fields.priority.options.* as needed.
```

If the project schema changes, refresh the ids in `repo-config.json` — re-discover with:

```bash
PROJECT_ID=$(rc_get '.project.id')
gh api graphql -f query="{ node(id: \"$PROJECT_ID\") { ... on ProjectV2 { fields(first: 30) { nodes { ... on ProjectV2SingleSelectField { id name options { id name } } } } } } }"
```

and write the ids back into the config.

## Notes

- Reviewer assignment is OUT of scope — reviewer assignment is a manual decision.
- The Closes issue's milestone IS auto-inherited from the first `Closes` issue in Phase 4 (if the PR has no milestone yet). Use `gh pr edit --milestone` to override.
- This skill is idempotent: re-running on the same PR re-applies the same metadata.

## What this skill does NOT do

- Does not create PRs (use `gh pr create` or `/commit-push-pr` first).
- Does not modify PR title or body.
- Does not assign reviewers.
- Does not change issue labels, milestone, or body.
- **Dispatches no agents — deliberately.** This skill is metadata mechanics (labels, board status, priority, assignee, milestone, invariant checks); every step is a deterministic `gh` call with a correct answer, so there is nothing for an agent to judge. Its absence from the agent dispatch tables is a decision, not an oversight.
- Does not summon bot reviewers — that is `/dev-lifecycle` Phase 7.2, after the ready flip (bots do not review drafts).
- Does not bypass any merge gates — Phase 5 is advisory only (logs WARNs, exits 0).

## Tests

Lib tests live at `skills/pr-creation/test-lib.sh`. Run with:

```bash
bash skills/pr-creation/test-lib.sh
```

Covers the pure parsers (`parse_pr_closes`, `parse_pr_refs`) and asserts no hard-coded
project-board defaults leak into `lib.sh` (config-driven generalization guard).
