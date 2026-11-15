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
when the file exists. If no source is configured and the default file is absent, validation
degrades to passthrough; a configured source that is missing fails closed.

**Catalog Dispatch:** when this repo defines a shared dispatchable-skills catalog
(`.claude/skills/_shared/dispatchable-skills.md`), this skill's catalog obligation is narrow — it
carries the docs-in-PR invariant (Phase 5 WARN below). It does NOT dispatch design-class skills:
the code is already written by PR-creation time, so design belongs upstream (`/issue-creation`,
`/dev-lifecycle`).

**Reviewer field is intentionally NOT modified.** Reviewer assignment is a manual decision.
A repo may add an automated-reviewer-assignment workflow later.

## Execution

All phases load the helpers in `.claude/skills/pr-creation/lib.sh` and `scripts/repo-config.sh`
from the default branch, never from this checkout, because on a PR branch both are the PR's code:

```bash
RC_REF="refs/remotes/origin/$(gh repo view --json defaultBranchRef --jq '.defaultBranchRef.name' 2>/dev/null)"   # the API is the authority; origin/HEAD goes stale after a rename
[ "$RC_REF" != refs/remotes/origin/ ] || RC_REF="$(git symbolic-ref --quiet refs/remotes/origin/HEAD || echo refs/remotes/origin/main)"
case "$RC_REF" in refs/remotes/origin/HEAD) RC_REF= ;; refs/remotes/origin/?*) ;; *) RC_REF= ;; esac
RC_OID="$([ -n "$RC_REF" ] && git show-ref --verify --hash "$RC_REF" 2>/dev/null)"   # exact refname, no DWIM: a same-named branch or tag cannot stand in
case "$([ -n "$RC_OID" ] && git ls-tree "$RC_OID" -- scripts/repo-config.sh | cut -d' ' -f1)" in
  100644|100755) ;;
  *) echo "FATAL: ${RC_REF:-the default branch} is not fetched or has no regular scripts/repo-config.sh" >&2; return 1 2>/dev/null || exit 1 ;;
esac
RC_SRC="$(git show "$RC_OID:scripts/repo-config.sh")" || { echo "FATAL: could not read the default-branch helper" >&2; return 1 2>/dev/null || exit 1; }
eval "$RC_SRC"   # the commit that was checked; the checkout's copy is never sourced
unset RC_TRUSTED_STATE RC_TRUSTED_CFG   # nothing carries over from an earlier run in this shell
# Presence comes from the tree, not from a read: a blob that is listed but cannot be read is an
# error, never "absent". RC_TRUSTED_STATE: absent = no config on the default branch; present = read.
RC_ENT="$(git ls-tree "$RC_OID" -- .claude/repo-config.json)" || { echo "FATAL: could not list the default-branch tree" >&2; return 1 2>/dev/null || exit 1; }
case "${RC_ENT%% *}" in
  "") RC_TRUSTED_STATE=absent ;;
  100644|100755)
    RC_TRUSTED_CFG="$(git show "$RC_OID:.claude/repo-config.json")" && [ -n "$RC_TRUSTED_CFG" ] \
      || { echo "FATAL: could not read the default-branch .claude/repo-config.json" >&2; return 1 2>/dev/null || exit 1; }
    RC_TRUSTED_STATE=present ;;
  *) echo "FATAL: the default-branch .claude/repo-config.json is not a regular file" >&2; return 1 2>/dev/null || exit 1 ;;
esac
DEFAULT_BRANCH="${RC_REF#refs/remotes/origin/}"
case "$(git ls-tree "$RC_OID" -- .claude/skills/pr-creation/lib.sh | cut -d' ' -f1)" in
  100644|100755) eval "$(git show "$RC_OID:.claude/skills/pr-creation/lib.sh")" ;;   # rc_get is already defined, so lib.sh skips its own helper load
  *) echo "FATAL: $RC_REF has no regular .claude/skills/pr-creation/lib.sh" >&2; return 1 2>/dev/null || exit 1 ;;
esac
```

### Phase 1 — Resolve underlying issue(s)

Parse PR body for closing-keyword references:

```bash
PR_NUMBER=$0
PR_BODY=$(gh pr view "$PR_NUMBER" --json body --jq .body)
CLOSES=$(parse_pr_closes "$PR_BODY")
REFS=$(parse_pr_refs "$PR_BODY")
```

> Both parsers strip fenced **and inline** code spans before matching, so a
> reference written as `` `Refs #123` `` is invisible to them — no label,
> board, or milestone inheritance fires for it. This is deliberate (it stops
> example snippets in a Test Plan from being read as real references), so write
> every real `Closes`/`Refs` reference as plain text.

If `$CLOSES` is empty: emit a warning and skip Phase 3a (no issue Status to change). **Phase 3c still runs** — like Phase 4, it inherits onto the PR's own board item via `pick_inheritance_source`, so it falls back to `$REFS`. Skip Phase 2 only when `$REFS` is **also** empty — Phase 2 iterates `$CLOSES $REFS`, so a `Refs`-only PR still has labels to inherit. **Phase 3b still runs** (when a board is configured) — it places the PR itself on the board, which does not depend on a linked issue. Proceed to Phase 4 (assignee + invariant validation still apply; milestone inheritance uses the same `pick_inheritance_source` and self-skips only when both are empty).

### Phase 2 — Inherit labels from issue(s)

Union of labels across all closing-referenced AND refs-referenced issues. Filter out `routine:*`, `type: epic`, and `risk: hotfix`, then validate against the Active labels section in the configured labels source (`.labels.source`, default `.claude/rules/labels.md`). When no labels source is configured and the default file is absent, `validate_against_active_labels` passes labels through unchanged; a configured source (`LABELS_FILE` or `.labels.source`) that is missing fails closed (exit 1).

> **`risk: hotfix` is filtered for a different reason than the other two.** They are semantically wrong on a PR; this one is a *release control* in repos that define it — on a PR it cuts a release the moment it merges, bypassing the scheduled blocking-red gate, so it can ship on a red suite. On the issue it meant only "this problem is urgent", decided at triage. Inheriting would turn a weeks-old triage opinion into an ungated release nobody chose. The skill warns and prints the exact `gh pr edit` command instead, so a person decides at PR time.

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

    # `risk: hotfix` is deliberately NOT inherited — on a PR it is a release
    # control (in repos that define it) that cuts a release on merge and
    # bypasses the scheduled blocking-red gate. Surface it; never apply it automatically. The loop
    # lives in lib.sh (and is tested there) because label names contain a
    # space, and the obvious inline `for ... in $(...)` word-splits on it.
    warn_dropped_release_controls "$ISSUE_LABELS" "$issue_n" "$PR_NUMBER"
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

> **Cloud sessions behave differently here, and the difference is not visible at the call sites.** Projects v2 is GraphQL-only, and a cloud agent session's GitHub proxy serves only a pinned set of GraphQL operations — every board call 403s there regardless of the token. So when `CLAUDE_CODE_REMOTE=true`:
>
> - `find_project_item_id` / `ensure_pr_on_project` / `get_issue_priority_option_id` return a **sentinel string** (`CLOUD_NOCREATE:<n>` / `CLOUD:<n>` / `CLOUD_INHERIT:<n>`), not a Projects v2 node ID. Treat the value as opaque: pass it to `move_to_status` / `set_priority` and nothing else. Do not log it as an item ID, compare it, cache it, or interpolate it into a query.
> - `move_to_status` / `set_priority` do **not** mutate the board. They call a repo-provided `scripts/board-op.sh`, which dispatches the repo's board-sync workflow, and the runner performs the GraphQL. The board update is therefore **eventually consistent**, not synchronous with this phase — a success here means "dispatch accepted", not "board updated".
> - `find_project_item_id`'s lookup-only contract is preserved across the boundary by the `CLOUD_NOCREATE:` variant, which forwards `--no-create` so an issue deliberately kept off the board is still skipped rather than force-added.
> - The dispatch is a **no-op until the board-sync workflow is on the default branch** (`workflow_dispatch` only registers a workflow there) and it needs a board-scoped token secret on the runner. When the repo has no `scripts/board-op.sh`, or the dispatch fails, the call returns non-zero and the phase WARNs and continues — board Status is recoverable bookkeeping.
>
> Locally nothing changes; every step below describes the local path exactly.

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

PRs should carry the same Project Priority as their underlying issue. Read that issue's Priority option ID and apply it to the PR's project item.

Source selection is `pick_inheritance_source` — the SAME source Phase 4 uses for milestone, and for the same reason: this writes `$PR_ITEM_ID`, the **PR's own** board item, so a `Refs`-only staged PR needs it exactly as much as a `Closes` PR does. (Phase 3a stays `Closes`-only because it mutates the *issue's* Status, which is a different question.)

**Important:** `set_priority` calls `${PRIORITY_FIELD_ID:?}` — it aborts (non-zero) when the Priority field is not configured. Gate the call with an explicit pre-check so this branch is simply skipped for repos without a Priority field, rather than aborting the skill with an unhelpful `:?` message.

```bash
PRIORITY_SRC=$(pick_inheritance_source "$CLOSES" "$REFS")
# Only attempt Priority inheritance when the repo config defines the Priority field.
# set_priority uses ${PRIORITY_FIELD_ID:?} internally — calling it without the
# field configured aborts the skill. The explicit pre-check keeps the skip path
# visible and avoids masking real failures with `if ! ...`.
if [ -n "$PRIORITY_SRC" ] && [ -n "$PR_ITEM_ID" ] \
        && rc_has '.project.fields.priority.id' \
        && [ -n "$(rc_get '.project.fields.priority.id')" ]; then
    if ! ISSUE_PRIORITY=$(get_issue_priority_option_id "$PRIORITY_SRC"); then
        echo "WARN: could not read Priority from issue #${PRIORITY_SRC}; skipping PR Priority sync" >&2
    elif [ -z "$ISSUE_PRIORITY" ]; then
        echo "INFO: issue #${PRIORITY_SRC} has no Priority set; nothing to inherit" >&2
    else
        if ! set_priority "$PR_ITEM_ID" "$ISSUE_PRIORITY"; then
            echo "WARN: failed to set Priority on PR #${PR_NUMBER} (see previous error)" >&2
        else
            echo "INFO: inherited Priority from issue #${PRIORITY_SRC} onto PR #${PR_NUMBER}" >&2
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

# Inherit milestone from the first Closes issue, falling back to the first Refs
# issue (symmetric with /issue-creation, which SETS milestone explicitly).
# Without this, a PR auto-closes its issue at merge but the milestone stays on
# the issue — the release-tracking view loses the PR. The Refs fallback matches
# Phase 2's `$CLOSES $REFS` label iteration: a staged multi-PR issue is
# referenced with `Refs #N` on purpose, and those PRs need the milestone too.
MILESTONE_SRC=$(pick_inheritance_source "$CLOSES" "$REFS")
if [ -n "$MILESTONE_SRC" ]; then
    ISSUE_MILESTONE=$(gh issue view "$MILESTONE_SRC" --json milestone --jq '.milestone.title // empty' 2>/dev/null || true)
    PR_MILESTONE=$(gh pr view "$PR_NUMBER" --json milestone --jq '.milestone.title // empty' 2>/dev/null || true)
    if [ -n "$ISSUE_MILESTONE" ] && [ -z "$PR_MILESTONE" ]; then
        gh pr edit "$PR_NUMBER" --milestone "$ISSUE_MILESTONE" \
            && echo "INFO: inherited milestone '$ISSUE_MILESTONE' from issue #${MILESTONE_SRC}" >&2 \
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
- Milestone IS auto-applied at Phase 4, from the first `Closes` issue or (failing that) the first `Refs` issue — but only when the PR has no milestone already, so a deliberate override is never clobbered. Use `gh pr edit --milestone` to override.
- This skill is idempotent: re-running on the same PR re-applies the same metadata.

## What this skill does NOT do

- Does not create PRs (use `gh pr create` or `/commit-push-pr` first).
- Does not modify PR title or body.
- Does not assign reviewers.
- Does not change issue labels, milestone, or body.
- Does not bypass any merge gates — Phase 5 is advisory only (logs WARNs, exits 0).
- **Dispatches no agents — deliberately.** This skill is metadata mechanics (labels, board status, priority, assignee, milestone, invariant checks); every step is a deterministic `gh` call with a correct answer, so there is nothing for an agent to judge. It is the one orchestrator with no row in [`_shared/lifecycle-agent-dispatch.md`](../_shared/lifecycle-agent-dispatch.md), and that absence is a decision, not an oversight — do not add one to be symmetric with the other orchestrators.
- Does not summon bot reviewers. That is [`/dev-lifecycle`](../dev-lifecycle/SKILL.md) **Phase 7.2**, which runs after the ready flip — bots do not review drafts, so summoning at PR-creation time would be a no-op.

## Tests

Lib tests live at `skills/pr-creation/test-lib.sh`. Run with:

```bash
bash skills/pr-creation/test-lib.sh
```

Hermetic: covers the pure parsers (`parse_pr_closes`, `parse_pr_refs`), `pick_inheritance_source`,
the `risk: hotfix` filter and its warning, and the cloud-routing block (via a `BOARD_OP_BIN` stub
that records argv), and asserts no hard-coded project-board defaults leak into `lib.sh`
(config-driven generalization guard). The suite prints `ALL PASS` or the first failure.
