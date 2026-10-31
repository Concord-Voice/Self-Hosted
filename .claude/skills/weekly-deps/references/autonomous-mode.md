# Autonomous Mode

Read this file when `/weekly-deps` is invoked with `--mode=prep` or `--mode=triage`. Both are non-interactive paths intended for scheduled runs. The default invocation (`/weekly-deps` with no `--mode` flag) is interactive and follows the main SKILL.md phases unchanged.

The cron schedule and timezone label are config-driven — `.weekly_deps.cron.{prep,triage,tz_label}` in `.claude/repo-config.json` (defaults `0 13 * * 3` / `0 15 * * 3` / `ET`). Throughout this file the example times reflect those defaults; substitute the repo's configured values.

## Why a separate file

The interactive flow lives in SKILL.md because that's the primary use case — the developer runs it manually, gates each merge with a confirmation prompt. The autonomous flow has different rules (no merges, write to a pinned issue instead of stdout, two independent crons instead of a chained schedule) and would clutter the main phase descriptions if interleaved. Keeping it here means the interactive reader never needs to think about cron mechanics, and the routine-context agent reads exactly what it needs.

## The two modes

| Mode | When it fires | Purpose |
|---|---|---|
| `prep` | the configured prep cron (default `0 13 * * 3`) | Discover, categorize, community-signal lookup, Copilot request, rebase comments, post pinned-issue digest |
| `triage` | the configured triage cron (default `0 15 * * 3`), independent of prep | Re-discover, fetch Copilot reviews, auto-dismiss known findings, update the digest comment in place |

Neither mode merges any PR, creates any new PR, or modifies repo files. The autonomous routine's job is to *prepare* the batch for the developer's interactive `/weekly-deps` session — never to substitute for it.

## Operating-discipline non-negotiables in autonomous mode

Per the repo's operational discipline (and the org defaults):

- **Every merge gets approval.** Autonomous mode does NOT merge. Even HIGH-confidence Tier A patches stay open until the developer runs `/weekly-deps` interactively. The project's protect/destructive-command hook patterns establish that it chooses "block automation, force human review" over "trust the bot" — autonomous merges would invert that.
- **Verify before declaring fixed.** Every gh API call's exit code is checked. If a pinned-issue comment edit fails (network, rate limit, comment-not-found), the routine logs the failure and continues with the rest; it does not retry silently or claim success.
- **No new branches or PRs.** Autonomous mode never invokes `gh pr create`, `git checkout -b`, or any push. The tech-stack-doc version-refresh follow-up (Phase 6.2 of interactive mode) is *skipped entirely* in autonomous mode — that's a developer decision.
- **Idempotent state mutations only.** The autonomous mode mutates state in three ways, each idempotent: (1) requesting Copilot review (idempotent via Step 3.6.1 check in SKILL.md), (2) posting `@dependabot rebase` comments (idempotent — dependabot ignores duplicate requests within a short window, plus a 1h cooldown check in Step P5 below), (3) posting/editing the pinned-issue weekly digest comment (idempotent by week marker — Step P6 below MUST check for an existing `<!-- routine-week: $WEEK_TAG -->` comment before posting; if found, edit it in place rather than create a duplicate). No other state writes.

## Prep mode — flow

The prep mode runs Phases 0 → 3.6 of the main SKILL.md, then writes the digest, then exits. The triage follow-up runs on its own independent cron — prep does NOT schedule it (see Step P7 below).

### Step P1: Discover

Same as Phase 1 of SKILL.md (after Phase 0 loads config + identity). Two `gh pr list` calls (dependabot + github-actions bot), unioned. Detect active ecosystems per Phase 1.5.

### Step P2: Categorize + pre-flight

Run Phase 2, Phase 3.1, 3.2, 3.3 unchanged. Phase 3.4 (community signal lookup, per `community-signal-lookup.md`) runs in autonomous mode for **every** PR — there's no developer to opt out, so the conservative default is "always check."

### Step P3: Confidence assess

Run Phase 3.5 (the confidence-assessment step). The autonomous mode adds **one extra rule** to the LOW conditions:

- Community-signal lookup verdict is `LOW` (advisory affecting the bumped-to version, or strong-negative release-note keyword match)

This is already encoded in Phase 3.5's conditions in SKILL.md; just confirming it applies in autonomous mode too.

### Step P4: Request Copilot review (idempotent)

Run Phase 3.6 for every LOW / UNKNOWN PR. In autonomous mode there's no developer prompt — the routine just dispatches. The Step 3.6.1 idempotency check (in SKILL.md) is mandatory: PRs that already have Copilot requested or reviewed get skipped, no double-request.

```bash
for PR in $LOW_PRS; do
  ALREADY_REQUESTED=$(gh pr view $PR --json reviewRequests \
    --jq '[.reviewRequests[] | select(.login == "Copilot" or .login == "copilot-pull-request-reviewer")] | length')
  ALREADY_REVIEWED=$(gh pr view $PR --json reviews \
    --jq '[.reviews[] | select(.author.login == "copilot-pull-request-reviewer")] | length')
  if [ "$ALREADY_REQUESTED" = "0" ] && [ "$ALREADY_REVIEWED" = "0" ]; then
    gh pr edit $PR --add-reviewer "@copilot" && echo "Requested: #$PR" || echo "Failed: #$PR"
  else
    echo "Skipped (already requested or reviewed): #$PR"
  fi
done
```

Failures are logged in the digest under "Copilot review requests" but do not abort the run — other phases can proceed.

### Step P5: Rebase on conflicts (idempotent)

In autonomous mode, the routine sends `@dependabot rebase` to every CONFLICTING PR. The rebase is server-side and idempotent — dependabot rate-limits its own response, so repeated commands within a short window are no-ops.

```bash
CONFLICTING=$(gh pr list \
  --search "author:app/dependabot is:open" \
  --json number,mergeable \
  --jq '.[] | select(.mergeable == "CONFLICTING") | .number')

for PR in $CONFLICTING; do
  # Check if we already sent a rebase comment in the last hour (avoid spam if run multiple times)
  LAST_REBASE=$(gh api "/repos/$OWNER/$REPO/issues/$PR/comments" \
    --jq '[.[] | select(.body | test("@dependabot rebase"; "i")) | .created_at] | last' 2>/dev/null)
  HOUR_AGO=$(date -u -v-1H +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '1 hour ago' +%Y-%m-%dT%H:%M:%SZ)
  if [ -z "$LAST_REBASE" ] || [ "$LAST_REBASE" \< "$HOUR_AGO" ]; then
    gh pr comment $PR --body "@dependabot rebase"
    echo "Rebase requested: #$PR"
  else
    echo "Skipped (recent rebase): #$PR — last sent $LAST_REBASE"
  fi
done
```

The 1-hour cooldown prevents spam if the cron fires more often than expected (e.g., manually triggered twice).

### Step P6: Write digest to pinned issue

Per `pinned-issue-digest.md`, find the pinned issue by exact title (`.weekly_deps.digest_issue_title`, default `Weekly Dependency Triage — Routine Output`), then create a new comment with:

- Front-matter markers (`routine-week`, `routine-prep-run`)
- PR inventory table (tier, confidence, lockfile, CI)
- Community signal section (advisories, release-note flags)
- Copilot review requests (what was requested, what was skipped)
- Rebase commands sent
- Recommendation for the developer's interactive run

```bash
WEEK_TAG=$(date -u +%G-W%V)
NOW=$(date -u +%Y-%m-%dT%H:%M:%SZ)

# Build digest body (see pinned-issue-digest.md for full template)
BODY=$(cat <<EOF
<!-- routine-week: $WEEK_TAG -->
<!-- routine-prep-run: $NOW -->
<!-- routine-triage-run: -->

## Weekly Dependency Triage — $WEEK_TAG ($(date -u +%a\ %F))

...

EOF
)

# Idempotency: a week-tagged comment may already exist (re-fire, retry, or a previous
# partial failure). Look up the current week's marker BEFORE deciding POST vs PATCH.
EXISTING_ID=$(gh api "/repos/$OWNER/$REPO/issues/$ISSUE_NUM/comments?per_page=100" \
  | jq -r --arg m "<!-- routine-week: $WEEK_TAG -->" \
  '[.[] | select(.body | contains($m))] | first | .id // empty')

if [ -n "$EXISTING_ID" ]; then
  gh api -X PATCH "/repos/$OWNER/$REPO/issues/comments/$EXISTING_ID" --field body="$BODY"
  echo "Updated existing prep comment $EXISTING_ID for week $WEEK_TAG"
else
  gh api -X POST "/repos/$OWNER/$REPO/issues/$ISSUE_NUM/comments" --field body="$BODY"
  echo "Created new prep comment for week $WEEK_TAG"
fi
```

If the comment post or edit fails: log the failure, write the digest content to `/tmp/weekly-deps-digest-$WEEK_TAG.md` as a fallback, exit non-zero so the cron's failure-notification fires. The idempotency check above prevents duplicate comments if the prep run fires twice in the same ISO week (e.g., a manual re-run after the regular cron).

### Step P7: SKIP — triage runs on its own independent cron

The triage follow-up is **not** scheduled by the prep run. It has its own independent recurring cron (the configured triage cron, default `0 15 * * 3`) registered alongside the prep cron at initial setup.

Why two independent crons instead of prep → triage chaining:

- **Remote autonomous sessions cannot create new schedules** — schedule-creation tools are not part of the in-session tool surface for a routine run. A chained design that assumed schedule-creation tools would be available at routine time would not work; the triage cron must be independently armed at setup.
- **More robust failure semantics.** If the prep run fails or doesn't fire, the triage cron still runs at its scheduled time. It detects "no prep comment for this week" in Step T2 and aborts cleanly — no orphan resources, no missed weeks.
- **Simpler operational model.** Two routines, each independently inspectable, pauseable, and editable. The chained model would have made the triage existence implicit on prep's success.

The prep run does NOT do anything to schedule the triage — that cron is independently armed at setup time and fires regardless of prep's outcome.

### Step P8: Exit cleanly

No post-merge cleanup, no copy-paste blocks, no new PRs. The routine logs a final status line ("Prep run for week $WEEK_TAG complete — see issue #$ISSUE_NUM comment for digest") and exits.

## Triage mode — flow

The triage mode runs a couple of hours after prep (the gap between the two configured crons). Copilot reviews should be in by then.

### Step T1: Re-discover

Same as Phase 1 of SKILL.md. The set of open PRs may have shifted:

- PRs the developer merged manually between prep and triage → no longer in the list, ignore
- New PRs opened between prep and triage → include them, but skip the Copilot triage step for them (they didn't go through prep)

### Step T2: Find the prep-run comment

Per `pinned-issue-digest.md`, locate the pinned issue, then find the comment for this week by `<!-- routine-week: $WEEK_TAG -->` marker. If not found, abort — there's no prep run to triage against.

### Step T3: Fetch Copilot reviews

For each PR that was in the LOW/UNKNOWN bucket from prep:

```bash
COPILOT_REVIEW=$(gh pr view $PR --json reviews --jq \
  '.reviews[] | select(.author.login == "copilot-pull-request-reviewer") | {state, body, submittedAt}')

COPILOT_COMMENTS=$(gh api "/repos/$OWNER/$REPO/pulls/$PR/comments" --jq \
  '[.[] | select(.user.login | test("copilot"; "i")) | {path, line, body: (.body | .[0:300])}]')
```

If the review is still pending (no review object yet, only a `reviewRequests` entry), mark the PR as "awaiting Copilot" in the digest and don't run auto-dismiss. The developer can re-trigger triage manually later if needed.

### Step T4: Auto-dismiss and surface

Per Phase 4.0 of SKILL.md, apply the auto-dismiss rules:

- Style / nit / formatting → dismiss
- "Consider adding test" on a dependency bump → dismiss
- Test-coverage finding on a dev-only (e.g. `devDependencies`) patch → dismiss
- Behavioral change / deprecation affecting shipped code / breaking-change call-out / supply-chain anomaly → surface

For each PR, count `(dismissed, surfaced)`. For Tier C PRs, do NOT auto-process via the deep-review skill in autonomous mode — that's a subagent dispatch which should run interactively. Just summarize Copilot's review state and inline-finding counts; surface to the digest for the developer to act on in the interactive run.

### Step T5: Update the digest comment

Per `pinned-issue-digest.md`, edit the prep-run comment in place:

1. Update front-matter `routine-triage-run` timestamp
2. Update the status table (Triage → ✅ Complete)
3. Add the "🤖 Copilot reconciliation" section with the auto-dismiss/surface table

Keep all prep-run content intact. The triage run is additive, not replacing.

### Step T6: Exit

No follow-up schedule. The next prep run is the next scheduled cron.

## Failure handling matrix

| Failure | Prep behavior | Triage behavior |
|---|---|---|
| Pinned issue not found | Abort with non-zero exit; cron notifies | Abort; can't update non-existent prep comment |
| Copilot request fails (one PR) | Log in digest, continue with others | n/a |
| Dependabot rebase fails | Log in digest, continue | n/a |
| Community-signal API rate limit | Label affected PRs as UNKNOWN, continue | n/a |
| Triage cron didn't fire (e.g., platform outage) | n/a — prep doesn't schedule triage | The developer re-invokes `/weekly-deps --mode=triage` manually via the routine UI's "Run now" |
| Prep-run comment not found | n/a | Abort; possible cause: prep didn't run, or comment was edited externally |
| Copilot review still pending on N PRs | n/a | Flag "awaiting" in digest, skip auto-dismiss for those |
| Digest edit fails | n/a | Write to `/tmp/weekly-deps-triage-$WEEK_TAG.md`, exit non-zero |

In all failure cases, the routine never proceeds to a merge action, never creates a new PR, never modifies the repo's code. The worst-case outcome of a failed routine run is "the developer doesn't have the weekly digest; they run `/weekly-deps` interactively from scratch" — exactly the pre-routine baseline.

## Validating before scheduling

Before the cron is registered, the routine should be smoke-tested manually:

```bash
# Smoke test prep mode (will mutate state — make sure you're ok with that)
/weekly-deps --mode=prep

# Smoke test triage mode (after prep + a few min for Copilot to start)
/weekly-deps --mode=triage
```

Verify:

- Pinned issue gets a new comment with the week's digest
- Copilot reviewer was added to LOW/UNKNOWN PRs
- `@dependabot rebase` was sent to CONFLICTING PRs (if any)
- Triage run found and edited the prep comment (single comment, not two)
- The "Surfaced findings" section in the triage update is actionable

Only after a clean smoke test should the recurring prep and triage routines be enabled. Both are registered as **independent** crons (prep on `.weekly_deps.cron.prep`, triage on `.weekly_deps.cron.triage`).

## Removing the routine

If the routine needs to be paused or removed, disable or delete both scheduled crons (`weekly-deps-prep` and `weekly-deps-triage`) in the routines UI. (Find your trigger IDs at the routines URL.)

Because the two crons are **independent**, you must pause both — pausing only the prep routine still leaves triage firing on its schedule. Triage detects "no prep comment for this week" via Step T2 and aborts cleanly when prep is paused, so it's a no-op, but it still produces a log line on each fire.

The pinned issue can stay open as a passive history of past weekly digests — closing it would lose context.
