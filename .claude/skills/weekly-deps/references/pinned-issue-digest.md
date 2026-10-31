# Pinned-Issue Digest Contract

Read this file when Step P6 of `weekly-deps` autonomous mode (per `references/autonomous-mode.md`) needs to write or update the routine's output channel — the single GitHub issue that holds the routine's weekly digest. This file defines the issue's title, comment format, version-marker scheme, and how the prep and triage runs coordinate on the same comment.

## Why a pinned issue (and not a chat channel or a repo file)

The issue lives in the same place as the PRs the routine is triaging — one click from a digest to a PR, no platform switching. It survives chat-channel retention. It doesn't require a commit (and therefore a CI run, a PR, an approval) like a repo-tracked markdown file would. And it's accessible to anyone with repo read access, so future contributors can see how the routine has been triaging without an additional permissions setup.

The trade-off is GitHub issue notifications can be noisy. Subscribe to the issue specifically (not the whole repo) and the inbox-load stays manageable — one notification per weekly run.

## Issue identity

**Title:** the value of `.weekly_deps.digest_issue_title` in `.claude/repo-config.json` (default: `Weekly Dependency Triage — Routine Output`).

The title is matched exactly so the routine can find the issue by title-search without ambiguity. Don't rename it once set; if you need to retire the issue, close it and create a new one with the same title.

**One-time setup (manual, by a maintainer):**

```bash
TITLE="$(rc_get '.weekly_deps.digest_issue_title' 'Weekly Dependency Triage — Routine Output')"

# Create the issue
gh issue create \
  --title "$TITLE" \
  --label "routine,dependencies" \
  --body "Pinned tracking issue for the \`/weekly-deps\` routine.

Each prep run posts a new comment with the week's PR inventory, community signals,
and Copilot review requests. The triage run (a couple of hours later) edits that comment
in place to add Copilot reconciliation results. The two crons are independent — prep does NOT schedule triage.

Format: each comment is tagged with a stable week marker (e.g. \`<!-- routine-week: 2026-W19 -->\`)
so the triage run can find and edit its sibling prep comment.

Run \`/weekly-deps\` interactively to process the week's batch using the routine's prep work."
```

Then pin the issue manually via the GitHub UI (Issues → ⋯ → Pin issue). `gh` does not support pin via CLI directly; the GraphQL `pinIssue` mutation works if you prefer scripting it.

**Finding the issue from the routine:**

```bash
TITLE="$(rc_get '.weekly_deps.digest_issue_title' 'Weekly Dependency Triage — Routine Output')"
ISSUE_NUM=$(gh issue list --state open \
  --search "in:title \"$TITLE\"" \
  --json number --jq '.[0].number')
if [ -z "$ISSUE_NUM" ]; then
  echo "FATAL: routine output issue not found. Create it per references/pinned-issue-digest.md."
  exit 1
fi
```

If the issue is not found, the routine should NOT auto-create it — that's an explicit one-time setup decision for a maintainer.

## Comment format

Each weekly run produces one comment per ISO week. The prep run creates the comment; the triage run edits the same comment a couple of hours later.

**Front matter (mandatory):**

```markdown
<!-- routine-week: 2026-W19 -->
<!-- routine-prep-run: 2026-05-13T13:00:00Z -->
<!-- routine-triage-run: <empty until triage runs> -->
```

The `routine-week` marker uses ISO week format (`YYYY-Www`) and is the key the triage run uses to find its sibling prep comment.

**Body template** (example rows use neutral manifest directories — substitute the repo's actual dirs and PR numbers):

```markdown
<!-- routine-week: 2026-W19 -->
<!-- routine-prep-run: 2026-05-13T13:00:00Z -->
<!-- routine-triage-run: -->

## Weekly Dependency Triage — 2026-W19 (Wed 2026-05-13)

| Run | Status | Time |
|---|---|---|
| Prep | ✅ Complete | <prep cron time> |
| Triage | ⏳ Pending | <triage cron time> |

---

### 📊 PR inventory — N open bot PRs

| Tier | Conf | PR | Title | Lockfile | CI |
|---|---|---|---|---|---|
| A | HIGH | #101 | typescript-eslint 8.59.2 → 8.59.3 | <dir-1> | 🟢 |
| A | MEDIUM | #102 | type-stubs 25.6.0 → 25.7.0 (minor) | <dir-1> | 🟢 |
| A | LOW | #103 | dev-tooling group (CI red) | <dir-1> | 🔴 |
| B | LOW | #104 | ajv 8.18.0 → 8.20.0 | <dir-1> | 🟢 |
| C | LOW | #105 | <framework> X.Y.Z → X+1.0.0 (MAJOR) | <dir-2> | 🟢 |
| D | n/a | #106 | security-data refresh | (data) | ⚪ |
| ... | | | | | |

**Lockfile contention:** <dir-1> 4 PRs, <dir-2> 1 PR, .github/workflows 1 PR.

### 🔍 Community signal — high-attention items

| PR | Package | Signal | Severity |
|---|---|---|---|
| (none in this week's batch) | | | |

OR

| PR | Package | Signal | Severity |
|---|---|---|---|
| #104 | ajv | GHSA-xxxx-yyyy-zzzz published 2026-05-12 (affects ≥ 8.20.0) | HIGH 🔴 |
| #104 | ajv | release notes mention "critical bug" | MEDIUM 🟡 |
| #105 | <framework> | 3 new issues upstream mentioning vX+1 crashes | MEDIUM 🟡 |

### 🤖 Copilot review requests

Requested this run:
- #103, #104, #105

Already requested in a prior week (skipped, idempotent):
- (none)

Already reviewed (skipped, will be triaged in the triage run):
- (none)

### 🔄 Rebase commands sent

Sent `@dependabot rebase` to:
- (none — no CONFLICTING PRs detected)

OR

- #102 (was CONFLICTING after #103 merge)

### 🎯 Recommendation for interactive `/weekly-deps`

Once Copilot reviews land (typically within ~10 min of the prep run; the triage run runs a couple of hours later), run `/weekly-deps` interactively to process:

- **HIGH (auto-mergeable):** K PRs — Phase 4.A batch confirm
- **MEDIUM (per-PR check):** K PRs — Phase 4.B
- **LOW (after Copilot triage):** K PRs — Phase 4.0 → 4.B/4.C
- **D (data-only):** K PRs — Phase 4.D

Estimated time: a short focused-decision session.

---

*The triage run will edit this comment in place when it completes.*
```

**After triage run, the comment is EDITED (not replaced):**

The triage run finds the existing comment by `<!-- routine-week: 2026-WNN -->` marker, then updates:

1. The `routine-triage-run` front-matter timestamp
2. The status table (Triage → ✅ Complete)
3. Adds a new section **"### 🤖 Copilot reconciliation"** after the "Copilot review requests" section:

```markdown
### 🤖 Copilot reconciliation (triage run)

| PR | Copilot state | Inline findings | Auto-dismissed | Surfaced |
|---|---|---|---|---|
| #105 | APPROVED | 0 | — | — |
| #107 | COMMENTED | 2 | 2 (style nits) | 0 |
| #103 | CHANGES_REQUESTED | 1 | 0 | 1 (root cause of CI red — see comment thread) |
| #104 | COMMENTED | 3 | 2 | 1 (behavior change — see thread) |

**Surfaced findings (need human review):**

- **#103** — Copilot flags a lint-rule rename that affects a handful of files. Auto-fixable; surfaced for developer decision.
- **#104** — Copilot notes that `ajv@8.20.0` changes default behavior for `$ref` resolution. A couple of validation test cases may need updating.
```

The "Surfaced findings" list is the **only** thing the developer needs to read carefully. Everything else is bookkeeping.

## Editing logic — how the triage run finds its comment

```bash
# Fetch all comments on the issue
ALL_COMMENTS=$(gh api "/repos/$OWNER/$REPO/issues/$ISSUE_NUM/comments" --paginate)

# Find this week's comment by marker
WEEK_TAG=$(date -u +%G-W%V)
COMMENT_ID=$(echo "$ALL_COMMENTS" | jq -r --arg w "$WEEK_TAG" '
  .[] | select(.body | contains("<!-- routine-week: " + $w + " -->")) | .id
' | head -1)

if [ -z "$COMMENT_ID" ]; then
  echo "FATAL: triage run could not find prep-run comment for week $WEEK_TAG"
  echo "       Possible causes: prep run failed to post; pinned issue has been renamed."
  exit 1
fi

# Build new body (combining prep content + triage additions)
NEW_BODY=$(...)

# Edit in place
gh api -X PATCH "/repos/$OWNER/$REPO/issues/comments/$COMMENT_ID" \
  --field body="$NEW_BODY"
```

The prep-run body must be preserved in the edited version — the triage run *adds* the Copilot-reconciliation section, doesn't replace the whole comment.

## Length-budget pragmatics

GitHub issue-comment max length is 65,536 characters. A typical weekly digest is 2–4 KB. If a single week's digest exceeds 50 KB (e.g., a release pulled in 80+ dependency PRs):

- Truncate the per-PR inventory table to the top 30 most-noteworthy rows
- Move the rest to a child issue linked from the digest
- Note in the comment: "Inventory truncated — see #NNNNN for full list"

This is a far-edge case; in practice the routine produces a few KB per week.

## When the routine output channel changes

If a maintainer wants to migrate from a pinned issue to a chat channel, a Discussion, or a different issue:

1. Close the old issue with a comment pointing at the new channel
2. Update `references/autonomous-mode.md` with the new write-path
3. Update the one-time-setup section here

Don't try to support multiple channels simultaneously from the routine — pick one canonical output and stick with it. The routine's job is to produce signal in one well-known place, not to mirror everywhere.
