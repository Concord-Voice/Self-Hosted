---
name: enhanced-pr-review
description: "Consolidate bot and project-side PR review feedback into one triage table. Collects every GitHub-app reviewer's comments/reviews via a type-aware `user.type==\"Bot\"` filter (display names and CI checks come from `.claude/repo-config.json` `.bots[]`), runs /review-pr to dispatch the specialist subagents the repo declares in `.review_agents`, auto-triages known patterns, and surfaces only conflicts that need human judgment. Use when reconciling AI review feedback before merge, or when you need a consolidated merge-approval sign-off posted to the PR to satisfy a required-review rule. Also use when an autonomous autofix agent may have committed fixes to, replied to, or resolved reviewer-bot threads on the PR — Phase 1.7 re-verifies that unreviewed work before merge."
argument-hint: [pr-number]
allowed-tools:
  - Bash(gh pr *)
  - Bash(gh api *)
  - Read
  - Grep
  - Glob
---

# Enhanced PR Review

> **Bot roster is config-driven.** This skill discovers reviewers structurally via the `user.type == "Bot"` filter (Phase 1), not from a hard-coded list — so any GitHub-app reviewer is picked up automatically with the fallback display name `Bot: {login}`. Pretty display names, CI-check names, and PR-body markers come from the OPTIONAL per-repo `.claude/repo-config.json` `.bots[]` array (read via `scripts/repo-config.sh` `rc_get`/`rc_has`). To register a bot for a nicer display name, add an entry to `.bots[]` — see the "Adding support for a new bot" section at the end. With `.bots == []` the generic fallback still works for every reviewer.
>
> **Three posting locations — the skill fetches all three.** Bot reviewers can post to ANY of three GitHub endpoints, and which one they use is reviewer-specific (no universal rule). `/pulls/{n}/comments` carries inline review comments (line-by-line); `/pulls/{n}/reviews` carries PR review summaries with a state (a bot's overall judgment); `/issues/{n}/comments` carries top-level conversation comments (verdict-only reviewers, Quality-Gate posters, and any `github-actions[bot]` workflow posting). PRs are issues under the hood — that's why the conversation endpoint is `/issues/...`. The skill polls all three in Phase 1 to avoid missing a reviewer (or a recommendation like a version bump) that only posts to the conversation endpoint.
>
> **Autonomous autofix agents are a DIFFERENT class from reviewers — see Phase 1.7.** Some platforms offer an autofix PR assistant (for example Blacksmith **Codesmith**'s "Autofix for your PRs") that, once a PR is `ready_for_review`, automatically pushes commits to fix failing tests, reviewer-bot comments, and red CI — **without any of this skill's discipline** (no `.claude/rules/` cross-check, no causality-radius test, no false-positive gate). By the time `/enhanced-pr-review` runs, such an agent may have already committed "fixes," posted replies, **resolved review threads**, and caused a reviewer bot to flip its verdict. **A commit, reply, or thread-resolution from an autofix agent is unreviewed work, not a verified resolution** — every green it produces is suspect (a resolved thread, a re-approved verdict, and a green CI check can all be *manufactured*, e.g. by resolving a thread it never fixed or deleting the test that was failing). Phase 1.7 detects an autofix footprint **behaviorally** — the one thing no reviewer bot does is author commits or resolve threads, so detection needs no config; its thread-resolution and reply signals are identity-independent, while commit detection is identity-heuristic (see Phase 1.7.1's limitation note) — re-verifies each action against the *original* finding, and routes every unverified action back through the normal Fix path. It NEVER auto-reverts; corrections are Fix rows the developer confirms. If no autofix agent has acted, Phase 1.7 detects no footprint and is a zero-cost no-op.

Reconcile all AI-authored code review feedback with project-specific subagent reviews for PR #$0. The skill fetches findings from every GitHub-app reviewer, not just one — a login-substring filter is the wrong abstraction because new reviewers keep being added and their login names aren't predictable. Identity-to-display-name mapping is data (`repo-config.json` `.bots[]`), not code baked into the skill.

---

## Phase 1 — Gather

**Step 1** — Resolve repo identity and load the bot roster:

```bash
gh repo view --json nameWithOwner --jq '.nameWithOwner'
```

Store as `REPO` (e.g., `your-org/your-repo`).

Load the OPTIONAL per-repo bot roster into a shell variable for the three fetches below. `.bots[]` (each `{match?, login?, marker?, name, ...}`) supplies display names, the shared-login markers, and CI-check names; an absent file or `.bots == []` yields `[]`, in which case every reviewer falls back to `Bot: {login}`:

```bash
source "$(git rev-parse --show-toplevel)/scripts/repo-config.sh"   # path where the org tooling is installed/symlinked
BOTS_JSON="$(rc_get '.bots' '[]')"   # JSON array; '[]' when unset → generic fallback naming
```

All three fetches pass `$BOTS_JSON` to jq via `--argjson bots "$BOTS_JSON"` and resolve a display name with one shared helper. The helper finds the FIRST `.bots[]` entry that matches the comment's author/body, else falls back to `Bot: {login}`:

```jq
# $bots is the repo-config .bots array. $login is .user.login; $body is the comment body
# (pass "" for endpoints without a body marker, i.e. the reviews endpoint).
def botname($login; $body):
  ( [ $bots[]
      | . as $b
      | ($b.match  // "") as $m      # login substring (dedicated-login reviewer)
      | ($b.login  // "") as $bl     # exact login (shared-login poster, e.g. github-actions[bot])
      | ($b.marker // "") as $mk     # body marker that disambiguates a shared login
      | select(
          ( $m  != "" and ($login | ascii_downcase | contains($m | ascii_downcase)) )
          or
          ( $bl == $login and $mk != "" and ($body | contains($mk)) )
        )
      | $b.name ]
    | first ) // ("Bot: " + $login);
```

A few properties of this resolution:
- A roster entry with only `.match` (a login substring) names a dedicated-login reviewer; an entry with `.login` + `.marker` names one of several posters that share a login (the classic case: many workflows post under `github-actions[bot]`, disambiguated by the `<!-- marker -->` HTML comment each workflow places for body-update idempotency).
- Fields are bound to vars (`$b`, `$m`, `$bl`, `$mk`) **before** the predicate so the inner `contains`/`ascii_downcase` pipes don't rebind `.` away from the roster entry. The marker test uses `contains` (literal substring) rather than `test` (regex) — markers are HTML comments and must not be interpreted as regex.
- Entries are evaluated in roster order; put the more specific (`.login`+`.marker`) entries before any broad `.match` that could also catch the shared login.

**Step 2** — Fetch bot-authored inline comments:

```bash
gh api repos/${REPO}/pulls/$0/comments \
  --argjson bots "$BOTS_JSON" \
  --jq '
    def botname($login; $body): ( [ $bots[] | . as $b | ($b.match // "") as $m | ($b.login // "") as $bl | ($b.marker // "") as $mk | select( ($m != "" and ($login | ascii_downcase | contains($m | ascii_downcase))) or ($bl == $login and $mk != "" and ($body | contains($mk))) ) | $b.name ] | first ) // ("Bot: " + $login);
    [ .[]
      | select(.user.type == "Bot")
      | { id: .id,
          author: .user.login,
          bot: botname(.user.login; (.body // "")),
          path: .path,
          line: (.line // .original_line),
          body: .body }
    ]'
```

Why this filter:
- `user.type == "Bot"` catches every GitHub app — any reviewer (Copilot, a SAST poster, Graphite, Qodo, CodeRabbit, etc.) without enumerating login strings. `user.type` alone is the right fence here — don't also gate on `author_association` (`NONE`, `MEMBER`, etc.), because a bot that an org has added as a collaborator (e.g., some Dependabot configurations) will have a non-`NONE` association, and we still want its findings. Adding the association gate would reintroduce the exact false-negative failure mode this skill is trying to prevent.
- **Verdict-only reviewers typically return ZERO rows at this endpoint** — a reviewer that posts only a conversation-level verdict (Step 4) and no inline comments contributes nothing here. The `botname` helper handles that uniformly: a roster entry with no inline comments simply never matches at this endpoint.
- The `bot:` field gives a human-readable display name for the triage table, resolved from `repo-config.json` `.bots[]` (Step 1). The same `botname` helper is reused across all three fetch endpoints (Steps 2, 3, 4) — adding a bot is a config edit (`.bots[]`), not three code edits. The body-marker disambiguation only fires where a body is passed (Steps 2 and 4); Step 3 passes `""` because the reviews endpoint groups by login, not by per-comment marker.

If no bot comments are found, note `"No bot inline comments for PR #$0"` and proceed — `/review-pr` may still produce subagent-only findings that need reporting.

**Step 3** — Fetch bot review summaries:

```bash
gh api repos/${REPO}/pulls/$0/reviews \
  --argjson bots "$BOTS_JSON" \
  --jq '
    def botname($login; $body): ( [ $bots[] | . as $b | ($b.match // "") as $m | ($b.login // "") as $bl | ($b.marker // "") as $mk | select( ($m != "" and ($login | ascii_downcase | contains($m | ascii_downcase))) or ($bl == $login and $mk != "" and ($body | contains($mk))) ) | $b.name ] | first ) // ("Bot: " + $login);
    [ .[]
      | select(.user.type == "Bot")
      | { author: .user.login,
          bot: botname(.user.login; ""),
          state: .state,
          body: .body }
    ]'
```

Note: review-endpoint logins may differ from comment-endpoint logins for the same bot (a bot may post inline comments under one login and review summaries under a `…[bot]` variant). A roster `.match` substring that is common to both login variants resolves both — which is why the "Adding support for a new bot" section recommends a substring `.match` over an exact login. A reviewer can also post a FORMAL PR review at this endpoint with a `state` of `COMMENTED`, `DISMISSED` (stale-dismissal on push), or `APPROVED` — and an `APPROVED` from a bot with `Pull requests: write` counts toward the required-review count. Its substantive prose verdict may ALSO appear as a conversation comment (Step 4).

**Step 4** — Fetch bot-authored TOP-LEVEL CONVERSATION comments:

```bash
gh api repos/${REPO}/issues/$0/comments \
  --argjson bots "$BOTS_JSON" \
  --jq '
    def botname($login; $body): ( [ $bots[] | . as $b | ($b.match // "") as $m | ($b.login // "") as $bl | ($b.marker // "") as $mk | select( ($m != "" and ($login | ascii_downcase | contains($m | ascii_downcase))) or ($bl == $login and $mk != "" and ($body | contains($mk))) ) | $b.name ] | first ) // ("Bot: " + $login);
    [ .[]
      | select(.user.type == "Bot")
      | { author: .user.login,
          bot: botname(.user.login; (.body // "")),
          created_at: .created_at,
          body: .body }
    ]'
```

Why a third fetch — this is where verdict-only reviewers and CI-poster workflows live. GitHub treats PRs as issues under the hood, and the `/issues/{n}/comments` endpoint surfaces top-level CONVERSATION comments on the PR (the running thread visible in the PR's "Conversation" tab). Loud-but-easy-to-miss signals live here — for example a release/version-bump classifier, or a static-analysis summary, posted under a shared CI login and disambiguated only by a body marker.

**Why marker-based detection** — a shared CI login (commonly `github-actions[bot]`: every `gh api` call from any workflow posts under it) cannot be disambiguated by login alone. The body's leading `<!-- marker -->` HTML comment is a deliberate convention each workflow places for body-update idempotency (so the workflow can edit-in-place rather than spam-comment). The `botname` helper inspects that marker via the roster's `.login`+`.marker` entries. **Disambiguating a new shared-login poster is a config edit** — add a `{login, marker, name}` entry to `.bots[]`; no skill change is needed.

**Verdict-only shape:** some reviewers emit ONE comment per PR carrying an overall judgment — e.g. a `<details>` block with an `✅ Approved` / `⚠ Changes Suggested` / `❌ Blocked` summary and a paragraph of reasoning — and no line-by-line findings. In the triage table (Phase 3), such a row has no `File:Line` value; the body cell carries the summary. This is structurally different from a line-by-line reviewer, and the triage table must handle both shapes.

**Reviewers that also run as a CI check** — some bots additionally surface a CI check whose status (`success` / `failure` / `neutral`) is a parallel signal already captured by `/ci-status` and `/dev-lifecycle` Phase 6 (CI MONITOR). The check name, when set, lives in the roster entry's `.ci_check`. This skill's reconciliation focuses on the comment-body content; the check status is the orchestrator's concern.

**Release-signal poster — special prominence in reconciliation (config-gated).** When the repo declares a `.release_signal` (see Phase 2.6), a release/version-bump classifier comment is treated specially: a recommendation to bump the version that is needed-but-absent is a **blocking-merge concern** with a top-of-table row in Phase 3 (severity: BLOCK), not a routine bot-only finding. Repos without `.release_signal` skip this entirely. See Phase 2.6.

**Step 5** — Run `/review-pr $0` to get all project-side findings. `/review-pr` dispatches the specialist subagents the repo declares in `.claude/repo-config.json` `.review_agents` (`.base` always-run agents plus `.patterns[]` file-pattern → agent matches); with no config it falls back to its default base set. The enumerated agent list is therefore the repo's concern, not this skill's.

> **Drift / config-schema review is delegated.** Reviewing config-drift or config-schema changes is the job of whatever reviewer the repo wires up for those paths in `.review_agents` (`.patterns[]`); this skill does not own that check, and any review fixtures for it live alongside that agent, not here. This skill ships no test fixtures of its own — its surface is the reconciliation/gate logic above, exercised against live PRs.

### Phase 1.5 — Catalog dispatch for Phase-8 candidate skills

After the always-dispatch agent suite returns (via `/review-pr`), consult the [`dispatchable-skills.md` catalog](../_shared/dispatchable-skills.md) `## Phase 8: FINAL REVIEW` section for additional candidate skills.

Run the catalog's "## Dynamic skill scoping (mandatory)" algorithm for `## Phase 8: FINAL REVIEW` against the PR title + body + changed-files signature. Dispatch is mandatory for matches; emit the `Scoped skills (Phase 8): [...]` visibility line. For each candidate entry (e.g., `/superpowers:receiving-code-review`, `/design:design-critique`, `/design:accessibility-review`, `/simplify`), evaluate its `**Trigger:**` heuristic against the PR title + body + changed files list. If the trigger matches:

- In `Hand-Held` and `Careful` modes (mode persisted from `/dev-lifecycle` Phase 1.5 via `.claude/state/dev-lifecycle-mode/<sanitized-branch>.txt`): surface as "`<skill>` trigger matched — invoke? [yes / skip]" and wait for user input.
- In `Trusted` / `Overnight` / `Weekly Deps` modes: invoke via the `Skill` tool, append findings to the reconciliation table in Phase 2.

The `/code-review ultra` recommend-only / billed entry surfaces as a chat suggestion at the end of Phase 3 (Report) if Phase 2's reconciliation table has 5+ findings: "Consider running `/code-review ultra <PR#>` for a comprehensive cloud-based multi-agent review." It is billed + user-triggered — Claude recommends, never launches it (deprecated alias `/ultrareview`).

If the mode-state file is missing (e.g., `/enhanced-pr-review` invoked directly outside of `/dev-lifecycle`), default to `Careful` mode behavior (gate on every candidate invocation).

**Defensive posture (defense-in-depth):** when `/enhanced-pr-review` is invoked DIRECTLY by the user (not via `/dev-lifecycle` Phase 8 dispatch in the same session), treat the mode-state file as advisory only and default to `Careful` regardless of the file's contents. This closes the gap where a planted mode-state file could escalate authorization on direct re-invocation. Only honor the mode-state file when `/enhanced-pr-review` is invoked as part of an active `/dev-lifecycle` flow that has already confirmed the mode via Phase 1.5's re-entry confirmation prompt.

---

## Phase 1.7 — Autonomous-agent (autofix) footprint reconciliation

**Core principle: an autonomous autofix agent's commit, reply, or thread-resolution is an UNREVIEWED change, not a verified resolution. Distrust by default; re-verify each against the original finding.**

This phase runs after Phase 1 (it needs the gathered bot findings) and before Phase 2 (its outputs feed the triage). It exists because an autofix agent (see the header callout) may have already acted on the PR by the time this skill runs, and its actions look green while being unverified. Phase 1.7 formalizes, and scopes to the agent's commits, the re-verification a diligent reviewer does by hand (read the code at the finding, diff against the claim) — and adds the one step a hand-reviewer can miss: reconstructing a finding the agent *erased* by re-approving itself. It is config-free and behavior-based, so it works on any repo with any autofix agent, and is a no-op where none has acted.

### 1.7.1 — Detect the footprint (behavior-based, identity-agnostic)

Three behavioral signals mark a PR as autofix-touched — autofix **commits**, bot **thread-resolutions**, and bot **replies**. **Reviewer bots exhibit none of them** — reviewers open threads and post verdicts; they never author commits or resolve threads. So the signature distinguishes an autofix agent from a reviewer bot without a login list (the same philosophy as Phase 1's `user.type == "Bot"` fence). No `.bots[]` entry or config is required to detect an autofix agent; a roster entry, if present, only prettifies its display name. Note the scope of that guarantee: it means the signals won't *false-positive on a reviewer bot*, not that every autofix commit trips them. So the commit signal (A) is detected **two ways** — by commit identity **and** by commit-message marker — and is backstopped by the identity-independent thread signals (B and C, resolutions and replies); together they make the phase robust against manufactured green.

**Signal A — autofix commits** on the PR branch. The commit's *identity* may be a bot or a human, so detect it two ways.

**A1 · by identity** — a bot author or committer:

```bash
gh api repos/${REPO}/pulls/$0/commits \
  --jq '[ .[]
    | select((.author.type == "Bot") or (.committer.type == "Bot"))
    | select(((.author.login // .committer.login) // "") | test("dependabot|renovate"; "i") | not)
    | { sha: .sha, author: (.author.login // .committer.login), committer: (.committer.login // null), msg: (.commit.message | split("\n")[0]) } ]'
```

Any row is a candidate autofix-agent commit. Match on **either** `author.type` **or** `committer.type` being `Bot`: some agents push commits authored under a developer's git identity (a user PAT, or the human kept as author) while the bot is only the *committer*, so an author-only filter would miss them. `dependabot` / `renovate` are excluded — a bot-authored commit on a *deps* PR is that PR's purpose, not an autofix action.

Matching the committer also picks up benign CI bots (e.g. `github-actions[bot]` committing a version bump, changelog, generated code, or format pass). That over-capture is intentional and **not** fixed with a login allowlist: `github-actions[bot]` is ambiguous — autofix agents commonly run *as* GitHub Actions, so excluding that login would reintroduce a miss. Instead these benign commits are filtered out **behaviorally** at the disposition stage: a footprint commit that maps to no reviewer finding and implicates no thread is dispositioned `No-finding` (§1.7.2–1.7.3), an informational Note that never routes to Fix and never blocks the merge gate. So Signal A errs toward inclusion here and lets §1.7.2 separate real autofix actions from benign bot commits.

**A2 · by commit-message marker** — identity-independent, catching the fully-human-identity case where A1 sees no `Bot` type at all (both author *and* committer are a developer, e.g. a human-triggered assistant such as Codesmith whose commits carry the triggering user as both). An autofix commit usually *names the reviewer finding it closes*:

```bash
gh api repos/${REPO}/pulls/$0/commits \
  --jq '[ .[]
    | (.commit.message | split("\n")[0]) as $subj
    | select($subj | test("auto[- ]?fix|(address|resolv)[a-z]*[[:space:]].*(finding|review|comment|reviewer|bot)"; "i"))
    | { sha: .sha, author: (.author.login // .committer.login), subject: $subj } ]'
```

Union A2's rows with A1's and dedupe by SHA. This arm is deliberately heuristic — it can match a developer's own "fix: address review comment" — but a false match only routes an already-HEAD, in-radius commit through re-verification (no auto-revert; a `Verified-correct` disposition keeps it), so erring toward inclusion is safe. A commit that carries **neither** a bot identity **nor** a finding-naming marker still slips past Signal A entirely; **Signals B and C** below are identity-independent and catch the manufactured-green actions that matter most (a resolved-but-unfixed thread, a reply that dismisses a true positive) no matter how the commit was authored.

**Signal B — bot-resolved review threads** (this is the Phase 4 hole, surfaced early). Reuse Phase 4 Step 4.1's `reviewThreads` GraphQL query but read `resolvedBy { login }`: a thread with `isResolved: true` whose `resolvedBy.login` ends in `[bot]` was resolved by an automated actor, not a developer. A human resolving a thread is normal; reviewer bots don't resolve; so a bot resolver ≈ the autofix agent.

**Signal C — bot replies inside another bot's thread.** In the `reviewThreads` comments, a thread whose FIRST comment author is a reviewer bot but which carries a LATER comment by a *different* bot is an autofix reply.

**If all three signals are empty → no autofix footprint was *detected*. Skip the rest of Phase 1.7 and proceed to Phase 2 unchanged.** A PR no autofix agent touched incurs zero new work and zero new rows. Emit the one-line visibility note `Autofix footprint: none detected` and move on. Read this as detection-limited, not a guarantee: an agent that commits under a fully human identity (no message marker) and neither resolves a thread nor replies is invisible to all three signals. That residual case carries no manufactured-green risk (nothing was resolved or dismissed), and the agent's code still passes through the normal Phase 5 specialist review of HEAD — Phase 1.7's special finding-reconstruction only adds value once a footprint is visible.

If a bot login recurs across signals, capture it as the autofix agent and derive a display name — the `.bots[]` `name` (via the Phase 1 `botname` helper) if the agent is registered, else `Autofix: <login>`.

### 1.7.2 — Reconstruct the original finding for each action

For each footprint action, recover **what it claimed to address** — because a reviewer bot may have SUPERSEDED its original finding after re-reviewing the autofix commit (a verdict flip from `CHANGES_REQUESTED` to `APPROVED`), leaving no open finding to check. Sources, in priority order:

1. **The review thread's FIRST comment** — the reviewer bot's original finding. Survives even when the reviewer's top-level verdict flips. Most reliable.
2. **The reviewer bot's EARLIER review** at `pulls/$0/reviews` — walk the FULL history, not just the latest state; a superseded `CHANGES_REQUESTED` / `COMMENTED` is the original ask.
3. **The autofix commit message** — it usually names the target ("fix: address <reviewer> finding on <file>").
4. **Pre-autofix code** — `gh pr diff` scoped to `base..<first-autofix-commit>^` shows what the code looked like before the agent touched it.

**Do NOT conclude "there is no open finding, so nothing to verify" prematurely.** The finding may have been closed *by the action under review* — reconstruct first, walking the FULL review history (sources 1–2 recover a finding the agent erased by re-approving itself).

**Only after all four sources are exhausted** may a commit be declared finding-less — and only if it also implicates no thread. A commit that matched Signal A (identity or message marker) but is tied to **no** Signal B bot-resolved thread, **no** Signal C bot reply, and yields **no** reconstructable finding addressed no bot finding at all. It is a benign non-autofix commit: a CI actor committing a version bump, changelog, generated code, or format pass (the common `github-actions[bot]` case), or a human commit that merely tripped the A2 message heuristic. Disposition it **`No-finding`** (§1.7.3) — an informational Note, never a blocker. Because this conclusion is gated on *no thread resolution, no reply, and no recoverable finding across the full history*, it cannot mask a manufactured green (an erased finding is still recovered by sources 1–2). A commit tied to a resolved thread, a reply, or any reconstructable finding is NEVER `No-finding`; it stays in the normal UNVERIFIED flow.

### 1.7.3 — Re-review each action against its finding (reuse the specialist suite — no new agent)

Default disposition = **UNVERIFIED** for any action tied to a finding, a bot-resolved thread, or a bot reply. (A commit-only footprint that §1.7.2 shows addresses no finding and implicates no thread is dispositioned **`No-finding`**, not UNVERIFIED — see the table below.) Re-verification reuses the EXISTING specialist reviewers the repo declares in `.review_agents` (the same suite `/review-pr` runs in Step 5) — scope the matching reviewers to the **autofix commit range** with the explicit question:

> *"Does this change correctly and completely close `<reconstructed original finding>`, with no regression, silent failure, deleted/weakened test, or unrelated change?"*

There is **no dedicated autofix-reviewer persona** — the specialists already judge diffs; Phase 1.7 only *scopes* them to the autofix commits and *pairs* them with the original finding. Assign each action exactly ONE disposition:

| Disposition | Meaning | Routes to |
|---|---|---|
| **Verified-correct** | The change fully closes the original finding with no new defect; specialist re-review confirms against the diff. | **Note** row — keep the change. This is the ONLY disposition that lets a bot-resolved thread count as resolved (Phase 4) or clear the Phase 6 gate. |
| **Incorrect-fix** | The change is present but does NOT close the finding, or is incomplete. | **Fix in this PR** — the finding is still live. |
| **False-dismissal** | The agent resolved/replied-away a TRUE-POSITIVE finding with no real fix. | **Fix in this PR** — reopen the finding. |
| **Regression / silent-failure** | The change closes the stated finding but introduces a NEW defect (swallowed error, behavior change). | **Fix in this PR** — for the new defect. |
| **Revert-needed** | The change is harmful or unrelated to any finding — e.g. skipped/deleted a test, reverted legitimate code, weakened CI to green it. | **Fix in this PR** — revert the autofix change. |
| **No-finding** | The commit matched Signal A/A2 but addressed no reviewer finding and implicates no thread — a benign CI/version-bump/format commit, or an A2 heuristic false-positive. Reached ONLY after §1.7.2 is fully exhausted (no thread, no reply, no recoverable finding). | **Note** row — informational, not an autofix action. Does NOT route to Fix and does NOT block Phase 6 precondition 9. |

Every disposition other than `Verified-correct` and `No-finding` becomes a Fix row that flows through the **existing Post-Report fix path** (which applies fixes with developer confirmation). `Verified-correct` keeps the change and `No-finding` is a benign non-autofix commit — both are Note rows with nothing to fix. Per the skill's invariant, **Phase 1.7 never autonomously reverts or rewrites** the agent's commits — the correction/revert is a Fix row the developer confirms, exactly like any other Fix. A `Revert-needed` / `Incorrect-fix` / `False-dismissal` on a security or otherwise **BLOCK-severity** finding is a BLOCK row (top of the Phase 3 table).

Autofix-commit changes are HEAD — they are in-radius by construction (radius-test Q1 = YES) and therefore **non-deferrable** under §2.5.0(ii). "Correcting the autofix agent's commit is out of scope" is never a valid defer.

### 1.7.4 — Rationalizations that are STRUCTURALLY INVALID

Same posture as §2.5.5: if the reasoning matches any of these, it routes to re-verify / Fix regardless of how reasonable the framing sounds.

| Rationalization | Reality |
|---|---|
| "The autofix agent already fixed it — the thread is resolved." | A resolution is a *claim*, and the resolver is the thing under review. Re-verify against the diff. Reject. |
| "A reviewer bot re-reviewed the autofix commit and approved." | A reviewer bot re-approves shallow/cosmetic changes. Its verdict is a third-party signal, not proof the finding closed. Read the diff. Reject. |
| "CI is green, so the fix worked." | CI green can be *manufactured* — the agent skips or deletes the failing test. Green CI over a deleted test proves nothing. Verify the test still exists and still asserts what it did. Reject. |
| "Reverting/correcting the autofix commit is out of scope for this PR." | The autofix commit IS this PR's HEAD — in-radius (Q1), non-deferrable (§2.5.0(ii)). Reject. |
| "There's no open finding, so there's nothing to re-check." | The finding may have been closed by the action under review. Reconstruct it (§1.7.2) first. Reject. |

### 1.7.5 — Emit into the triage

Footprint actions become Phase 3 rows: **Source** = the autofix agent's display name plus the original reviewer in parentheses (e.g. `Autofix: <login> (autofix) + <reviewer> (finding)`); **Category** = `Autofix-handled`; **Triage** = `UNVERIFIED → <disposition>`. `Verified-correct` → Action `Note`; every other disposition → Action `Fix` (BLOCK if the underlying finding is security/blocking-severity). A **`No-finding`** row is the exception: it pairs with no reviewer (Source is just the committing bot login, e.g. `CI: github-actions[bot]`), its Triage is `No-finding` (not `UNVERIFIED → …`), and its Action is `Note` — it is a benign non-autofix commit, not counted against the Phase 6 gate. These rows carry into Phase 4 (thread-resolution) and Phase 6 (honesty gate), which refuse to count an unverified autofix action as resolved.

---

## Phase 2 — Reconcile

Categorize each finding into one of four buckets:

### Agreed
A bot AND a project agent flagged the same file and issue area. Strongest signal. Also applies when TWO different bots flagged the same issue (e.g., two code-reviewing bots both flagging a missing null-check, or a reviewer + a Quality-Gate poster both calling out a complexity threshold) — multi-bot agreement is itself a high-signal event worth calling out in the Triage column. **Action: Fix.**

### Bot-only
Only one or more bots flagged it, no project agent did. Apply hybrid triage:

1. Read the `.claude/rules/` file matching the finding's domain, if present. If the finding contradicts a documented project rule, mark as `Auto: FP (.claude/rules/{file})`.
2. Check if the pattern is documented as intentional in `CLAUDE.md` or project conventions. If so, mark as `Auto: FP (documented pattern)`.
3. If neither applies, read the actual code at the referenced file:line. Evaluate whether the suggestion is technically correct for this codebase. Mark as `Auto: valid` with brief reasoning, or `Needs review` if ambiguous.

### Subagent-only
Only a project agent flagged it. These are project-specific checks (RBAC coverage, migration symmetry, docs drift, etc.) that bots don't know about. Trusted. **Action: Fix.**

### Conflict
A bot and a project agent both flagged the same file area but recommend different fixes or have contradictory assessments. Present both sides with context. **Action: Developer decision.**

### Deferral
Deferral is NOT a parallel category to Agreed / Bot-only / Subagent-only / Conflict. It is a route OUT of the default action (Fix in this PR), gated by Phase 2.5 below. The default action for every finding is **Fix in this PR**. See Phase 2.5 for the gate.

---

## Phase 2.5 — Deferral Gate

The default action for every finding is **Fix in this PR**. Defer is the exception, gated by the mechanisms in this section. A finding may ONLY be deferred if its rationale passes ALL of: §2.5.1 closed allowlist, §2.5.2 causality-radius test, §2.5.3 show-the-fix-first, §2.5.4 format-level gate. Anti-patterns enumerated in §2.5.5 are categorically rejected.

### 2.5.0 — Non-deferrable findings

Two classes of finding are **categorically non-deferrable** — they route to Fix-in-PR regardless of the §2.5.1 categories and the §2.5.2 radius test. The only escape is the §2.5.6 lockstep valve.

**(i) Documentation findings the PR makes stale.** A finding is a *documentation finding* if its fix is an edit to a doc surface (`*.md`, `docs/**`, `.claude/rules/**`, `CLAUDE.md`, `README*`, ADRs, runbooks, or doc comments tracked by `docs-reviewer`) AND the doc describes behavior or structure this PR changes. The §2.5.2 radius test does NOT apply — it is code-structural (imports, packages, directories) and blind to a Markdown file that *describes* code without importing it. Replace it with one semantic test: **"Does this PR make this doc stale?"** If yes → **Fix in this PR**, regardless of the doc's filesystem distance from the diff. Preexisting doc debt in an *untouched* doc file follows the normal gate; in a *touched* doc file it is non-deferrable via rule (ii).

**(ii) No-rot rule — findings in files the PR modifies.** Any finding whose file appears in `gh pr diff $0 --name-only` (radius-test **Q1 = YES**) is **non-deferrable, whether or not this PR introduced it.** Touching a file makes its preexisting issues yours to fix — a chore/bug-style fix that stops rot from lingering. "Preexisting" / "out of scope" / "follow-up issue" are not defer reasons here. Bounded to files actually in the diff.

### 2.5.1 — Closed allowlist of defer categories

A finding can ONLY be deferred if its rationale matches one of these categories EXACTLY:

- **A. SCHEMA_MIGRATION_REQUIRED** — fix requires a DB migration not in the PR's migration set, AND the migration is sequenced behind another in-flight migration. (Valid in OR out of radius.)
- **B. EXTERNAL_BLOCKER** — fix depends on a third-party API change, vendor response, or upstream library version that isn't yet available. (Valid in OR out of radius.)
- **C. EXPLICITLY_OUT_OF_SCOPE_AND_OUT_OF_RADIUS** — BOTH conditions required: (a) the linked issue body explicitly names this finding as "out of scope" or "follow-up" (verbatim phrase must be quoted in the defer rationale), AND (b) the finding is outside the PR's causality radius per the 5-question test in §2.5.2. (NOT valid for in-radius findings.)
- **D. CROSS_PR_COORDINATION** — fixing this requires changes coordinated across 2+ open PRs from different authors that haven't merged yet. (Valid in OR out of radius.)

Any rationale that doesn't fit A–D verbatim → the gate REJECTS the defer and routes the finding to "Fix in this PR." If a genuinely-new category is needed, propose it in a separate skill update PR — not via in-the-moment rationalization on this PR.

### 2.5.2 — Causality-radius test

Before any defer is considered, you MUST answer all 5 yes/no questions in writing in the report. The finding is INSIDE the causality radius if ANY answer is YES; OUTSIDE only if ALL answers are NO.

```
1. Is the finding's file in `gh pr diff $0 --name-only`?
2. Does the finding's file import / require / use any file from #1?
3. Is the finding's file imported by / required by / used by any file from #1?
4. Is the finding's file in the same package / directory as any file from #1?
5. Does the PR add or modify a symbol that's referenced in the finding's surrounding code (within ±20 lines or in the same function/struct/component)?
```

Implementation guidance:
- Q1 — use `gh pr diff $0 --name-only` and check exact path match.
- Q2/Q3 — for Go, grep imports in the finding's file vs. the diff files. For TypeScript, grep `import .* from` / `require(`. For Python, grep `from .* import` / `import`.
- Q4 — match by directory prefix; "same package" means same nearest containing module (Go package, npm workspace, Python module).
- Q5 — grep the finding's file for symbols added/modified in the diff (function names, struct names, type names, exported constants).

In-radius findings cannot be deferred via category C. Out-of-radius findings can be deferred via any of A–D.

### 2.5.3 — Show-the-fix-first requirement

Before any finding is presented as a defer candidate, you MUST write out what the fix would look like in this PR as a concrete 1–2 sentence description in the Notes column of the triage row:

```
Proposed fix in this PR: <concrete description of the edit that would close the finding, e.g.,
"Replace the bare TIMESTAMP column type with TIMESTAMPTZ at migration 000062 line 14
 and update the down migration's reverse statement on line 27.">
```

Without that text the defer marker is malformed and §2.5.4 fails the row. This requirement inverts the reasoning frame: consider the fix first, defer as the exception. Often, articulating the fix reveals it's trivial and there was never a real defer case.

### 2.5.4 — Format-level gate (mechanical)

Defer-row format:

```
Defer (category: A|B|C|D, radius: in|out, issue: #NNN, fix-size: <N lines>)
```

All four fields are mandatory. Mechanical rejection rules:

- `radius: in, category: C` → category C is disallowed in-radius. Reject with: `ERROR: invalid defer — category C requires radius: out`.
- Missing any of `category` / `radius` / `issue` / `fix-size` → Reject with: `ERROR: malformed defer row — line N, missing <field>`.
- `issue: #` with no integer → Reject with: `ERROR: defer requires existing tracked issue number. Opening a new issue solely to satisfy this gate is not a valid defer reason; the issue must pre-exist with milestone assignment.`
- `fix-size:` containing non-numeric text → Reject with: `ERROR: fix-size must be a numeric line count.`

This is mechanical validation, not reasoning-dependent. The format is enforceable by a regex.

### 2.5.5 — Defer arguments that are STRUCTURALLY INVALID

These rationalizations look reasonable but are categorically rejected. If the defer reasoning matches any of these patterns verbatim or paraphrastically, the gate REJECTS the defer and routes the finding to Fix-in-PR.

1. **"This is just ever so slightly out of scope"** — scope is the causality radius (§2.5.2), not the original intent. Slightness is not a defer reason. Reject.
2. **"We didn't touch this file"** — Q1 of the radius test, evaluated in isolation. The reviewer flagged the finding precisely because of Q2–Q5. The fact that the file is not in `pr diff --name-only` is what triggered the adjacency check, not a defense against it. Reject.
3. **"The reviewer's finding is technically correct but unrelated to this PR's goal"** — if the finding is in-radius, "unrelated to original goal" is intent, not causality. Reject. If out-of-radius AND the issue body explicitly says "out of scope," appeal to category C instead.
4. **"We can address this in a follow-up issue"** — opening a follow-up issue is a defer MECHANISM, not a defer JUSTIFICATION. The justification must appeal to A, B, C, or D. Reject.
5. **"Fixing this would require touching files outside the current diff"** — that's exactly the causality-radius point. If those adjacent files are in the radius (Q2/Q3/Q4 yes), touching them is in scope. Reject.
6. **"The doc update can be a follow-up"** — documentation that describes behavior this PR changes is in scope by construction; the radius test does not apply to it (see §2.5.0(i)). Merging the code without the doc leaves `main` self-inconsistent. Reject.
7. **"This issue predates our PR — it's preexisting, not something we introduced"** — touching the file makes the finding in-radius (Q1) and yours to fix under the no-rot rule (§2.5.0(ii)). Age is not a defer reason. Reject.

### 2.5.6 — Lockstep valve

The ONLY escape for a *disproportionately large or risky* preexisting fix surfaced by the no-rot rule (§2.5.0(ii)) is a **lockstep companion PR**: opened immediately, reviewed, and landing in lockstep with this PR (before it, or together), gated on **explicit developer approval in writing.** This is categorically NOT a deferral — there is no someday-issue; the fix is tracked as live code in review, not backlog. Requirements:

1. Explicit developer sign-off naming the finding(s) and the companion PR.
2. The companion PR exists before this PR merges.
3. "Disproportionate" is a reviewer judgment (e.g., the preexisting fix is a large refactor or carries independent risk warranting its own review surface), confirmed by the developer.

Valve-row format (in the triage table):

```
Lockstep-companion (companion-PR: #NNN, approver: <dev>, reason: disproportionate-preexisting)
```

`companion-PR: #` with no integer, or a missing approver, → reject as malformed (same mechanical posture as §2.5.4).

---

## Phase 2.6 — Release-signal gate (config-gated)

**This entire phase runs ONLY when the repo declares a release signal** — i.e. `rc_has '.release_signal'` is true. Most repos don't have a release-classification workflow; for them this phase is **skipped in full** (and §6.0 precondition 5 below is dropped). Run the gate first:

```bash
if ! rc_has '.release_signal'; then
  : # no release signal configured — skip Phase 2.6 entirely; release classification is not a gate for this repo
else
  RELEASE_MARKER="$(rc_get '.release_signal.marker')"            # PR-body marker of the classifier's comment
  VERSION_MANIFEST="$(rc_get '.release_signal.version_manifest')" # file whose version the classifier checks (may be unset)
  # … run §2.6.1–§2.6.4 below.
fi
```

When the phase IS active: a release-signal comment is NOT a bot-only finding to be auto-triaged; it is a **PR-level release-engineering signal** with its own category. The gate makes the classifier's version-bump recommendation un-missable in the reconciliation table — that recommendation is otherwise easy to lose among many conversation comments.

### 2.6.1 — Parse the release-signal comment

The comment is identified in Phase 1 Step 4 by its `.release_signal.marker` (the `RELEASE_MARKER` above) under a shared CI login. A typical body contains a table with load-bearing rows like:

| Check | Result |
|-------|--------|
| Version bump needed | **Yes** / No |
| Version bump found | **Yes** — `<version>` / **No** — current version is `<version>` |

Plus a `> **Recommendation:**` line at the bottom naming the action (e.g., "Bump the version in `<version_manifest>` before merge", where `<version_manifest>` is `.release_signal.version_manifest`). Read the actual comment body for the exact row labels and recommendation wording — they are the classifier workflow's concern, not this skill's.

### 2.6.2 — Categorize the recommendation

Apply this decision tree:

| Version bump needed | Version bump found | Category | Triage-table severity | Action |
|---|---|---|---|---|
| No | (any) | `Release-signal: ALL CLEAR` | INFO | None — note in summary |
| Yes | Yes | `Release-signal: SATISFIED` | INFO | None — note in summary |
| Yes | **No** | `Release-signal: BLOCKING` | **BLOCK** | Bump the version before merge OR defer with explicit developer approval |

The BLOCKING case is the one that gets missed. It MUST appear as a top-row in the Phase 3 triage table (row ordering: BLOCK rows first, per Phase 3 layout rules).

### 2.6.3 — Interaction with `/dev-lifecycle`

[`/dev-lifecycle`](../dev-lifecycle/SKILL.md) handles the orchestrator-side version-bump approval (it reads the same `.release_signal` config and waits for explicit developer approval before bumping). This skill's role is to make the recommendation un-missable in the reconciliation table. The bump itself is best bundled into the review-loop fix-commit batch — alongside the other reconciliation fixes — so it piggybacks on a CI cycle that is already going to happen rather than triggering an extra run. The Release-signal BLOCK row surfaces the requirement; the actual bump commit lands when Phase 3 Post-Report fixes are applied.

### 2.6.4 — Deferring a BLOCK recommendation

A `Release-signal: BLOCKING` row can ONLY be deferred if BOTH conditions hold:
1. The developer explicitly approves the defer in writing on the PR (not just an in-chat ack)
2. The defer reason matches one of: the PR will be closed/abandoned; the version bump is being handled in a sibling PR that will land first; or the release-signal check itself is misconfigured for this PR shape (rare — investigate the classifier rather than ignore it).

There is no `Defer (category: ...)` row format for the release signal — the §2.5 deferral gate categories (A/B/C/D) are about code-finding defers, not release-engineering signals.

---

## Phase 3 — Report

Output the triage table. The **Source(s)** column names each reviewer explicitly — use the `bot` field from Phase 1 (the display name resolved from `.bots[]`, e.g. "Copilot", "SonarCloud", or a `Bot: graphite-app[bot]` fallback) rather than a generic "bot" label. Multi-bot agreement should be listed with a `+` separator.

**Two finding shapes coexist in the table:**
- **Line-level findings** (inline review comments, subagent findings) populate `File:Line` with a specific path:line; the Finding column holds the line-level description.
- **PR-level verdicts / recommendations** (a verdict-only reviewer's overall judgment, a Quality-Gate result, a release-signal version-bump recommendation, a review summary) leave `File:Line` blank or marked `—`; the Finding column carries the overall verdict or recommendation. These rows describe the PR as a whole.

**The `Thread` column is REQUIRED on every row.** It carries the GraphQL thread ID (`PRRT_…`, from Step 4.1's `id` field) for a line-level finding, or the top-level comment's `id` for a PR-level verdict; `—` only for a subagent finding with no GitHub conversation behind it. This is what makes a decision *addressable* — without it the table records that N findings were fixed but never which conversation each fix answers, so nothing downstream can check that the finding a commit claims to close is the finding whose thread got resolved.

**Row ordering: BLOCK rows first.** Any row with a `BLOCK` severity (a release-signal recommending a version bump while the bump is absent — only when `.release_signal` is configured — or any subagent finding flagged blocking-merge) sorts to the top of the table regardless of category. The developer sees blockers first; everything else is below.

```
Enhanced PR Review Report — PR #$0
──────────────────────────────────────────────────────────────────────────
| # | Finding | File:Line | Thread | Source(s) | Category | Triage | Action |
|---|---------|-----------|--------|-----------|----------|--------|--------|
| 1 | ⚠ BLOCK: <subagent blocking-merge finding, or release-signal version-bump if .release_signal is set> | — / path:line | `PRRT_…` | <reviewer> | PR-level / Subagent-only | — | **Resolve before merge** |
| 2 | ⚠ BLOCK: autofix "fix" does not close the finding — <original finding> still live at HEAD | src/api/users.go:142 | `PRRT_…` | Autofix: <login> (autofix) + <reviewer> (finding) | Autofix-handled | UNVERIFIED → Incorrect-fix | **Fix** |
| 3 | Verdict-only reviewer: ✅ Approved — "No issues found." (stale if it re-approved an unverified autofix HEAD) | — | `PRRT_…` | Bot: <login> | PR-level verdict | — | Note (no action needed) |
| 4 | Quality Gate passed (0 new issues) | — | `PRRT_…` | <reviewer> | PR-level verdict | — | Note (no action needed) |
| 5 | Missing nil-check before deref | src/api/users.go:142 | `PRRT_…` | @code-reviewer + <bot> | Agreed | — | Fix |
| 6 | Authorization scope check missing | src/api/dm.go:88 | `PRRT_…` | @code-reviewer | Subagent-only | — | Fix |
| 7 | Incomplete effect dependency list | src/ui/components/X.tsx:34 | `PRRT_…` | <bot> | Bot-only | Auto: FP (.claude/rules/{file}) | Dismiss |
| 8 | Approach disagreement on retry policy | src/net/index.ts:201 | `PRRT_…` | <bot> + @code-reviewer | Conflict | — | Developer decision |

Summary: BLOCK rows: N | Autofix-handled: N (N verified, N unverified→fix) | PR-level verdicts: N | Agreed: N | Bot-only: N (N valid, N FP) | Subagent-only: N | Conflicts: N
Bots that posted: <list each bot from the `bot` field with its contribution, e.g. "Copilot (N findings), SonarCloud (1 verdict)">
Autofix footprint: <agent display name> — N commits, N thread-resolutions (N verified, N unverified→fix) | or "none"
──────────────────────────────────────────────────────────────────────────

Recommended next steps:
1. Fix all Agreed + valid Bot-only + Subagent-only findings
2. Review Conflicts and decide
3. Carry every row to a disposition recorded in its thread (Post-Report)
4. Post/update the Phase 3.5 reconciliation post, then run `.claude/skills/enhanced-pr-review/check-review-dispositions.sh`
```

If NO bots posted findings (the filter returned an empty array in Phase 1), the report should explicitly call that out — an empty bot-findings section is itself a signal (bots either agree the PR is clean, or they haven't reviewed yet).

---

## Post-Report — Interactive Phase (the disposition ledger)

Every row is carried to a **disposition** — a recorded decision written into the conversation the finding came from. The ledger is that set of decisions. It is complete when `.claude/skills/enhanced-pr-review/check-review-dispositions.sh` exits 0, and Phase 4 will not pass until it does.

### The four dispositions (closed set)

| Disposition | Means | `evidence:` field |
|---|---|---|
| `accepted` | The finding is valid and the fix has landed. | the commit SHA that landed it (7–40 hex) |
| `rejected` | Declined — false positive, intentional behavior, or the finding stopped existing (duplicate, code removed). | the rule/convention path the rejection rests on, or the row it duplicates |
| `deferred` | Tracked as follow-up work. | `#NNN` — **the issue must already exist**; the gate calls `gh issue view` and fails closed if it does not, and rejects a PR number in an issue's place |
| `undecided` | Genuinely open and known to be — it needs someone else's call, or a design decision first. | what it waits on |

`undecided` **blocks the gate**, exactly like a thread with no marker. It is not an escape hatch. What it buys is the difference between *"we know this is open and here is what it waits on"* and *"nobody decided and nobody noticed"* — two states that look identical in a silent thread. Recording an open question truthfully is always available, which is what makes mis-filing it as `rejected` or `deferred` to clear the gate indefensible rather than merely discouraged.

Nothing else is a disposition. "Acknowledged", a 👍, and a bot's own thread-resolve are not decisions on record.

`deferred` records the *outcome*; it does not justify one. The justification still passes §2.5.1's allowlist and the §2.5.2 radius test. This table adds only that the promised issue must be *verifiable* at merge time — a follow-up that was never filed is an accepted finding wearing a deferral's clothes.

### The reply shape

Each in-scope thread gets one reply with two parts, in this order:

1. **The reasoning, in prose** — what you concluded and what you checked. This is the part a human reads six months from now.
2. **One marker line** — `<!-- cv-disposition: <verb> evidence: <value> -->`

```markdown
Fixed in `e5c34d0`. Good catch — it also caught a second-order effect of an earlier
change in this PR, so the regression test now pins both.

<!-- cv-disposition: accepted evidence: e5c34d0 -->
```

The marker is an HTML comment, invisible in the rendered PR — the reviewer sees only your prose. It exists so the gate can verify a decision was made without trying to parse whether prose constitutes one.

```bash
gh api repos/${REPO}/pulls/$0/comments/{comment_id}/replies \
  -X POST -F body=@reply.md
```

Stage the body to a file: marker text contains `<`/`>` and shell-quoting a multi-line body with backticks is where this breaks.

**Write the marker yourself.** The gate reads markers only from non-bot authors with an `OWNER`/`MEMBER`/`COLLABORATOR` association, so a marker a bot posts — or a drive-by account — does not count.

**For findings marked Fix:** apply the fix, then disposition the thread `accepted` with the SHA that landed it. A fix that landed without its marker leaves the ledger incomplete even though the code is correct.

**For Conflicts:** wait for the developer decision, then disposition with whichever verb it selected.

---

## Phase 3.5 — The reconciliation post (REQUIRED when any reviewer spoke)

This skill posts its own comment to the PR: the single artifact reconciling every review signal — each bot's findings, each specialist subagent's findings, and the decision on each — into one record that lives with the PR rather than in terminal scrollback.

**It posts regardless of merge-readiness.** A review whose outcome is "three deferred, one still under discussion" needs this record more than a clean one does. That is why it is here and not folded into Phase 6: Phase 6's artifact is gated behind the §6.0 honesty gate and only ever appears on a PR that is *ready*, so a stalled PR would carry no record of what was decided at all.

**Why the summary comments make this mandatory.** Bots post a summary comment enumerating their findings and then one inline comment per finding. Top-level conversation comments have no thread and no Resolve affordance — GitHub offers nowhere to record a decision against them — so no per-thread gate can ever reach that surface. PRs are routinely observed with zero review threads and several top-level bot comments, where a thread-only gate reports "all threads resolved" over a surface it never read.

### 3.5.1 — Shape

The first line is the marker; the gate keys on it and on the reviewer names in the body.

```markdown
<!-- cv-review-reconciliation -->
## 🔍 Review Reconciliation — PR #<n>

**Reviewed at HEAD:** `<sha>` · **Reviewer:** @<gh_user> · via `/enhanced-pr-review`

### Reviewers consulted
| Reviewer | Surface | Findings | Answered in |
|---|---|---|---|
| <bot display name> | N inline + 1 summary | N | threads |
| <verdict-only bot> | Quality Gate | 0 new issues | this post |
| @code-reviewer | subagent | N | rows N |

### Decisions
| # | Finding | Source(s) | Thread | Decision | Evidence |
|---|---|---|---|---|---|
| 1 | <finding> | <reviewer> | `PRRT_…` | ✅ Accepted | fixed in `<sha>` |
| 2 | <finding> | <reviewer> | `PRRT_…` | ❌ Rejected | <rule path or measurement> |
| 3 | <finding> | @code-reviewer | `PRRT_…` | ⏭ Deferred | #NNN — category A, out-of-radius |

**Ledger:** N findings · N accepted · N rejected · N deferred · 0 undecided
```

Reviewer names must match the roster: either the literal login, or the `name` your `.claude/repo-config.json` `.bots[]` entry declares for it. Registering a bot for a nicer display name is the same config edit that gives it a display name in the Phase 3 table — the gate reads the same roster, so the two cannot drift.

**Required vs. example.** Only two elements are required, and they are the two the gate checks: the `<!-- cv-review-reconciliation -->` marker, and a **Reviewers consulted** list naming every reviewer that spoke. The **Decisions** table below is an *example* of the house format — useful, conventional, and not machine-verified. **What the gate verifies, and what is convention.** The check confirms exactly three things: that exactly one post exists, that a repo member (not a bot) authored it, and that every reviewer which spoke on any of the three surfaces is named in it. It does **not** parse the Decisions table, cross-check `Thread` handles, or count rows. The table shape above is the house format a reader relies on; the reviewer-coverage clause is the part with teeth. Do not read "REQUIRED" as "machine-verified in full".

### 3.5.2 — Post once, then edit in place

```bash
EXISTING=$(gh api "repos/${REPO}/issues/$0/comments" \
  --jq '[.[] | select(.body | test("<!--\\s*cv-review-reconciliation\\s*-->")) | .id] | first // empty')

if [ -n "$EXISTING" ]; then
  gh api "repos/${REPO}/issues/comments/${EXISTING}" -X PATCH -F body=@reconciliation.md
else
  gh pr comment "$0" --body-file reconciliation.md
fi
```

The gate requires **exactly one**. Re-running after a fix commit updates the existing comment; it never adds a second, so the PR carries one current record rather than a pile of superseded ones.

### 3.5.3 — Verify

```bash
.claude/skills/enhanced-pr-review/check-review-dispositions.sh "$0"
```

Exit 0 → proceed. Exit 1 → the blocking list is printed. Exit 2 → the check could not be made; treat as UNKNOWN, never as clean.

**What a green run proves, and what it does not.** It proves the decisions are recorded, addressable and auditable. It does **not** prove a human made them: `gh` authenticates as the repo owner, so an agent's reply is authored by a `User` and clears the non-bot filter exactly as the developer's would. The gate cannot distinguish them and does not try. What changes is that the reasoning is written down in one reviewable place instead of terminal scrollback. Never cite a green run as evidence that a person reviewed anything.

---

## Phase 4 — Pre-merge thread resolution check

This phase runs **after** all fix commits land and CI is green. It gates `/dev-lifecycle` Phase 9 (MERGE-READY) on one invariant: **every in-scope review conversation carries a recorded human decision** — a Phase 3.5 disposition marker — and the reconciliation post accounts for every reviewer that spoke.

Run the check; it is the gate, and the rest of this phase is how to read and clear its output:

```bash
.claude/skills/enhanced-pr-review/check-review-dispositions.sh "$0"
```

Exit 0 → proceed. Exit 1 → the blocking list is printed; clear it. Exit 2 → the lookup failed; treat as UNKNOWN, never as clean.

**Why the invariant is "carries a decision", not "was resolved by a human".** The obvious alternative — *a thread resolved by a `[bot]` actor is blocking unless an autofix re-review cleared it* — misfires in both directions when measured. Reviewer bots resolve their own threads as routine housekeeping once a developer has replied, so that rule blocks a large majority of well-handled threads; a gate that blocks the common good case gets overridden as routine, and an overridden gate stops catching the threads that received no reply at all. Keying on the presence of a human disposition clears the housekeeping and catches the silence. Phase 1.7 is unaffected: an autofix agent's *code change* still needs its `Verified-correct` disposition under §6.0 precondition 9.

### Step 4.1 — Query unresolved review threads

GitHub's REST `pulls/{n}/comments` endpoint does not surface thread-resolution state. Use the GraphQL API:

```bash
OWNER_REPO=$(gh repo view --json nameWithOwner --jq '.nameWithOwner')
OWNER=$(echo $OWNER_REPO | cut -d/ -f1)
REPO=$(echo $OWNER_REPO | cut -d/ -f2)

gh api graphql -f query="
{
  repository(owner: \"${OWNER}\", name: \"${REPO}\") {
    pullRequest(number: $0) {
      reviewThreads(first: 100) {
        nodes {
          id
          isResolved
          resolvedBy {
            login
          }
          path
          line
          comments(first: 100) {
            nodes {
              author {
                __typename
                login
              }
              body
            }
          }
        }
      }
    }
  }
}" --jq '[
  .data.repository.pullRequest.reviewThreads.nodes[]
  | select(.isResolved == false or .comments.nodes[0].author.__typename == "Bot")
  | {
      thread_id: .id,
      path: .path,
      line: .line,
      is_resolved: .isResolved,
      resolved_by: (.resolvedBy.login // null),
      resolved_by_bot: ((.resolvedBy.login // "") | test("\\[bot\\]$")),
      bot: .comments.nodes[0].author.login,
      first_comment: (.comments.nodes[0].body | .[0:140]),
      has_dev_reply: ([.comments.nodes[] | select(.author.__typename != "Bot")] | length > 0),
      has_disposition: ([.comments.nodes[]
                         | select(.author.__typename != "Bot")
                         | select(.body | test("<!--\\s*cv-disposition:\\s*[a-z]+"))] | length > 0)
    }
]'
```

The filter keeps a thread if it is unresolved **OR** was resolved by a `[bot]` actor. The second clause closes the manufactured-green hole: without it, `select(.isResolved == false)` dropped an autofix-agent-resolved thread entirely, so a finding the agent closed without fixing never reached the merge gate. `resolved_by_bot: true` marks those threads for reconciliation against their Phase 1.7 disposition.

The bot-detection filter uses GraphQL's `author.__typename == "Bot"` rather than a login-substring match. This is the GraphQL-side equivalent of Phase 1 Step 2's `user.type == "Bot"` REST filter — type-aware rather than name-aware, so any future GitHub-app reviewer (Graphite, Qodo, CodeRabbit, or any new bot) is picked up automatically without enumerating logins. The two-step filter (bot-authored first comment, then any non-bot reply) catches the realistic case where a developer replied inline but never clicked the GitHub UI's "Resolve conversation" button. Those threads can be auto-resolved.

The `comments(first: 100)` window covers virtually all real-world thread depths (most bot threads are 1–3 comments; a window of 100 makes the `has_dev_reply` false-negative case essentially impossible without GitHub API misuse). If a thread genuinely exceeds 100 comments, manual inspection is the right escalation.

### Step 4.2 — Categorize threads

Categorize on **the disposition**, then on the resolve flag — in that order.

| Condition | Action |
|---|---|
| `has_disposition: true` AND `is_resolved: true` | **Clear.** A decision is on record and the thread is closed. The resolver's identity does not matter. |
| `has_disposition: true` AND `is_resolved: false` | **Auto-resolvable** — decision recorded, only the flag missing. Batch-resolve. |
| `has_disposition: false` | **Blocking** — no decision on record, whatever the flag says and whoever set it. A reply without a marker, a thumbs-up, and a bot's self-resolve are all this row. |
| disposition is `deferred` | **Blocking until the issue is verified** to exist (`gh issue view`) and to be an issue rather than a PR. |
| **Additionally:** a Phase 1.7 footprint action not `Verified-correct` | **Blocking** — independent of this table; §6.0 precondition 9 owns it. A `cv-disposition` marker does not discharge it. |

**A marker is only a marker when an authorized human wrote it.** The check reads markers from non-bot authors with an `OWNER`/`MEMBER`/`COLLABORATOR` association only — so a bot echoing the format, or a drive-by account, leaves the thread reading as undecided.

### Step 4.3 — Report and gate

**Defer to the script's exit code.** Step 4.1's query is shown above so a reader can see what the gate reads; it is not a second, hand-run procedure with its own criteria. Everything below branches on the exit code and nothing else.

- **Exit 0** → print `✅ All review conversations carry a decision. Safe to proceed to merge.`
- **Exit 1** → the blocking list is printed, one line per gap. Clear them:
  - *no disposition* → reply with reasoning plus a `cv-disposition` marker (Post-Report).
  - *replied but no marker* → append the marker; the reasoning is already there.
  - *unfiled deferral* → file the issue, or fix in-PR. A promise is not a deferral.
  - *reconciliation gaps* → update the Phase 3.5 post in place; never add a second.
  - *autofix action still UNVERIFIED* → land the Phase 1.7 Fix. A `cv-disposition` marker does NOT discharge §6.0 precondition 9.
- **Exit 2** → UNKNOWN. Re-run; never read it as clean.

Threads that carry a disposition but were never marked resolved on GitHub are the one auto-resolvable class. Offer batch-resolve:

```bash
gh api graphql -f query='mutation($id: ID!) { resolveReviewThread(input: {threadId: $id}) { thread { isResolved } } }' -F id="$THREAD_ID"
```

Resolve only threads whose disposition is recorded — resolving one without a decision is precisely the silent close this gate exists to catch.

---

## Phase 5 — Branch protection discipline (when bot reviews block merge)

When a bot review (a code-reviewing bot's `CHANGES_REQUESTED`, a Quality-Gate failure, a release-signal `BLOCKING` when `.release_signal` is configured, a security-reviewer BLOCK severity, etc.) causes the PR's `mergeStateStatus` to transition to `BLOCKED`, the block is intentional infrastructure. It is not noise to bypass.

When a repo provisions a reviewing bot to block merges on critical and high-risk findings (declare it `"blocking": true` in `.bots[]`), that block status is a deliberate gate — treat it as load-bearing.

### Step 5.1 — Do NOT reach for `--admin` first

**NEVER use `gh pr merge --admin`** to circumvent a legitimate bot CHANGES_REQUESTED state. It is NOT a default escape hatch. It exists for genuine emergencies (security incident response, broken-CI recovery) and explicit per-PR developer authorization. It is NOT a workaround for "I've already made N fix commits and don't want another."

### Step 5.2 — Investigate, don't bypass

Read the bot's MOST RECENT review carefully. Distinguish three cases:

| Case | Default action |
|---|---|
| Real bug, in-radius | Fix it. Make another commit. Push. Wait for re-review. The 2-commit soft guideline (see Step 5.3) does NOT apply to merge-blocking findings. |
| False positive, can demonstrate empirically | Reply with evidence (test output, demonstration, doc citation, code excerpt). Request dismissal from developer. |
| Out-of-scope (passes §2.5 deferral gate) — EXCLUDES documentation findings (§2.5.0(i)) and any finding in a touched file (§2.5.0(ii)), which are non-deferrable | File a tracked follow-up issue. Request defer + dismissal with explicit developer approval. |
| Unclear / ambiguous | Surface to developer. Do NOT default to bypass. |

### Step 5.3 — The 2-commit "soft guideline" — what it is and isn't

`/dev-lifecycle` Phase 8 historically said "Maximum: 2 fix commits per review cycle." That is a soft guideline for **noise prevention** — preventing ping-pong on low-severity polish loops, not a hard rule that justifies bypassing legitimate findings.

| Finding shape | 2-commit guideline applies? |
|---|---|
| Cosmetic / single-bot polish (e.g., comment-style nits, naming preference) | YES — surface remaining items to developer after 2 cycles |
| Merge-blocking via branch protection (CHANGES_REQUESTED, BLOCKING) | **NO** — keep iterating until block resolves properly |
| Critical / high-severity / security-relevant | **NO** — keep iterating until resolved properly |
| In-radius and structurally correct | **NO** — fix it |

A 3rd, 4th, or Nth fix commit on a legitimate merge-blocking finding is exactly the right behavior. Do not surface "I've hit the 2-commit limit" as a reason to bypass.

### Step 5.4 — When `--admin` IS appropriate

Three cases only:

1. **Genuine emergency** — production incident, broken CI infrastructure blocking all merges, security response requiring immediate action. Document the emergency in the PR or follow-up.
2. **Explicit per-PR developer authorization** — the developer has reviewed the situation, understands the bypassed finding, and explicitly authorizes for THIS specific merge. Per-PR only; not a license for future bypasses.
3. **Verified bot misconfiguration** — the bot is producing systematic false positives across the repo (not just one PR), is being investigated separately, and the developer has authorized bypass while the bot is reconfigured.

For any other case, the block must be resolved properly.

### Step 5.5 — Illustration of the principle

A common failure mode: a blocking reviewer's `CHANGES_REQUESTED` is cleared by the first fix commit, but the bot then flags two new issues on that fix commit — say one false positive plus one real typo. With a soft "max 2 fix commits per cycle" rule in play, the temptation is to reach for `gh pr merge --admin` rather than push a 3rd commit. That is the wrong call: a bot provisioned to block critical and high-risk findings is doing exactly its job, and an arbitrary commit-count rule does not justify bypassing branch protection on a high-risk finding.

The correct response: investigate each new finding, demonstrate the false positive with empirical evidence (reply on the thread), fix the legitimate typo, push the 3rd fix commit, and wait for re-review. The 2-commit limit does not apply to merge-blocking findings.

---

## Phase 6 — Consolidated Merge Approval

This phase runs **last** — after Phase 4 reports all bot threads resolved, after every Phase 3 `Fix` decision has landed as a commit, and after CI is re-green on the resulting HEAD. It produces the single **consolidated merge-approval artifact** that records — in the PR itself — that a human reviewer ran `/enhanced-pr-review` and consolidated every signal (specialist subagents, every bot from `.bots[]`, CI, the release signal when configured, and any other reviewer) into one verdict.

That artifact serves the repo's **required-review rule**. When the repo declares `.branch_protection.doc_path`, the authoritative current state of the protected-branch ruleset (approval count, code-owner review, thread-resolution, stale-dismissal) lives in that doc — consult it (`BP_DOC="$(rc_get '.branch_protection.doc_path')"`) rather than restating ruleset field values inline, since the ruleset is operator-tunable and the live state can drift from committed snapshots. When no such doc is configured, read the active rule directly with `gh api repos/${REPO}/rulesets` / `gh pr view $0 --json mergeStateStatus`. Either way, branch on the active rule. Depending on PR authorship the artifact is EITHER a real GitHub approving review OR a documented sign-off comment — see §6.1.

**This phase NEVER merges.** It emits the approval artifact and prints the merge command; the merge itself stays with the developer / [`/dev-lifecycle`](../dev-lifecycle/SKILL.md) Phase 9 (MERGE-READY), the universal-floor gate. Phase 6 also never runs `gh pr merge` (admin or otherwise).

### 6.0 — The honesty gate (preconditions)

The approval artifact may be emitted ONLY if ALL of the following hold. If ANY fails, emit the **NOT-READY** summary (§6.4) naming the blocker(s) and STOP. There is no pressure — time, exhaustion, sunk-cost, "I already read it myself", a chat thumbs-up — that lowers this bar. A consolidated approval is a factual claim about the PR's state; emitting one while a blocker is live is a false statement in a durable, auditable record.

1. **No open `BLOCK`-severity rows** in the Phase 3 table (security-reviewer BLOCK, release-signal BLOCKING per §2.6 when `.release_signal` is configured, any subagent blocking-merge finding).
2. **No blocking bot's most-recent verdict is `⚠ Changes Suggested` / `❌ Blocked` / `CHANGES_REQUESTED`** — for every bot marked `"blocking": true` in `.bots[]`, its latest verdict must be `✅ Approved` / passing, OR the finding was fixed AND the bot re-reviewed the fix commit (re-fetch Phase 1 Step 4; do not trust a stale verdict).
3. **Every blocking Quality-Gate / SAST check = passed.** A `PENDING` / queued / not-yet-reported check is **NOT** a pass. Wait for the result; never approve ahead of a gate that has not reported.
4. **All required CI checks green** — `gh pr checks $0` shows zero `fail` and zero `pending` in the required set. **Pending ≠ green.** Report the actual state, never the assumed-imminent one.
5. **(Only when `.release_signal` is configured)** the release signal = ALL CLEAR or SATISFIED, or an explicitly-approved defer per §2.6.4. When `.release_signal` is absent, this precondition does not exist — skip it.
6. **The disposition ledger is complete** — the check exits 0 at the current HEAD. **Exit 2 fails this precondition** — an unverifiable ledger is not a clean one. Consume Phase 4 Step 4.1's result at the current HEAD, and re-run Step 4.1 now if it isn't fresh. (When Phase 6 is invoked directly rather than via `/dev-lifecycle` Step 9.1.5, there is no prior Phase 4 run to consume — run Step 4.1 here.)
7. **Every `Fix`-marked finding is actually resolved** — verify by reading `gh pr diff $0` / the fix-commit SHAs, not by asserting it. A finding you intended to fix but didn't push is still open.
8. **HEAD is the commit being approved** — no unpushed local fixes. The artifact references the current HEAD SHA. If the active ruleset dismisses stale reviews on push (the common configuration), **any later push dismisses the approval** — so Phase 6 is genuinely the last step before merge. If a fix still needs to land, you are not in Phase 6 yet.
9. **No UNVERIFIED autonomous-agent (autofix) action remains** (Phase 1.7). Every footprint action carries a disposition; every non-`Verified-correct`, non-`No-finding` disposition has a landed Fix commit (verify by diff per precondition 7). A thread resolved by a `[bot]` actor counts toward precondition 6 **only** with a `Verified-correct` disposition — never on the `isResolved` flag alone. A manufactured green — the agent resolved a thread it did not fix, a reviewer bot re-approved a cosmetic change, or a failing test was deleted/skipped to green CI — does NOT satisfy this gate. If any footprint action is still `UNVERIFIED`, or any bot-resolved thread lacks a `Verified-correct` disposition, this precondition FAILS. A **`No-finding`** disposition (a benign non-autofix commit that §1.7.2 tied to no finding and no thread) is informational and does NOT block — it is not an UNVERIFIED action, so it never fails this gate. When Phase 1.7 reported `Autofix footprint: none detected`, this precondition is trivially satisfied.

**Faithful reporting (non-negotiable):** the artifact reports the *actual* state of each signal. Never report a pending check as passed, a true-positive finding as a false-positive, or an unaddressed finding as resolved. If a signal cannot be verified, say so in the artifact — do not infer green.

### 6.1 — Determine the merge mechanism (authorship × code-owner)

Resolve identities and the CODEOWNERS surface:

```bash
PR_AUTHOR=$(gh pr view $0 --json author --jq '.author.login')
GH_USER=$(gh api user --jq '.login')
gh pr diff $0 --name-only        # cross-check changed paths against the repo CODEOWNERS file
```

Whether a PR needs **code-owner** review (vs. a plain approving review) depends on the active ruleset and whether any changed path matches the repo `CODEOWNERS` — consult `.branch_protection.doc_path` (`rc_get '.branch_protection.doc_path'`) for the current rule when set, else read the live ruleset directly, rather than assuming. Then:

| Case | Condition | Artifact (§6.2) | Merge path |
|---|---|---|---|
| **Formal approval** | `PR_AUTHOR != GH_USER` (e.g. teammate or `dependabot`) | `gh pr review $0 --approve --body-file <file>` | The approving review **mechanically satisfies** the required-review rule — including code-owner review when `GH_USER` is the code owner for the touched paths. Normal squash-merge. |
| **Self, no code-owner path** | `PR_AUTHOR == GH_USER`; no code-owner review required for the touched paths | Consolidated **comment** (`gh pr comment`) | Self-approval is impossible (see below). If a bot is provisioned to auto-approve qualifying PRs, that approval can satisfy the required-review count → merge with a plain `gh pr merge --squash`, no bypass. Otherwise another write-access human's approving review; if neither, documented admin-bypass per §6.3. |
| **Self, code-owner path** | `PR_AUTHOR == GH_USER` **and** code-owner review is required for a touched path | Consolidated **comment** + documented bypass authorization (§6.3) | If the code-owner team has no eligible member other than the author, **admin-bypass is the only mechanical path** — the required code-owner reviewer *is* the author. Otherwise another code-owner approves. |

**NEVER run `gh pr review $0 --approve` on a PR you authored.** GitHub rejects it (`422 Can not approve your own pull request`). For self-authored PRs the artifact is a comment, not a review — and the comment does not by itself satisfy the ruleset (a comment is not an approving review); it is the audit record that accompanies the §6.3 merge path.

### 6.2 — Standard artifact format

Use this exact shape every time, so the approval record is uniform and greppable. Stage the body to a temp file and post via `--body-file` (avoids shell-quoting fragility with the table). The header line names the reviewer and the skill — this is the "a human ran `/enhanced-pr-review` and signed off" record.

```markdown
## ✅ Consolidated Merge Review — PR #<n>

**Reviewed at HEAD:** `<sha>` · **Reviewer:** @<gh_user> · via `/enhanced-pr-review`

| Signal | Result | Notes |
|---|---|---|
| Specialist subagents (`/review-pr`) | ✅ clean / ⚠ N | <agents that ran; finding summary> |
| <one row per bot from `.bots[]` that posted — use its `bot` display name> | ✅ / ⚠ / ❌ / N comments | <verdict or finding count> |
| CI checks | ✅ N/N required green | |
| Release signal *(only if `.release_signal` configured)* | ALL CLEAR / SATISFIED / deferred (#NNN) | |
| Bot review threads | ✅ 0 unresolved | |
| Disposition ledger | ✅ N/N decided | N accepted · N rejected · N deferred · 0 undecided |

**Decisions:** recorded in the Phase 3.5 reconciliation post (link it) — N findings, 0 undecided. Not restated here: that post is edited in place, and a second copy would be a snapshot from a different moment that starts disagreeing with it the first time a decision changes.

**Verdict: APPROVED FOR MERGE.** Every merge-gate signal is green and every finding is resolved or properly dispositioned.

**Merge path:** <the §6.1 case that applies, with the exact command from §6.5>
```

For a formal approval the same body is passed to `gh pr review $0 --approve --body-file <file>`; for a comment it is passed to `gh pr comment $0 --body-file <file>`.

### 6.3 — Documented admin-bypass (self-authored PRs with no eligible reviewer)

For a self-authored PR where no eligible non-author reviewer exists, the required approving review cannot be obtained — a **structural property**, not a bot flake. Two shapes hit this: **(a)** the PR touches a CODEOWNERS path whose owning team has only the author as a member, so code-owner review can't be satisfied; **(b)** the PR needs an approving review but no other write-access human is available. GitHub forbids self-approval (the `422` above) and bots/Actions can't self-approve either, so the only mechanical path is a documented org-admin bypass. This is the in-PR realization of [Phase 5 Step 5.4 case 2](#step-54--when---admin-is-appropriate) ("explicit per-PR developer authorization"), and it is gated:

1. **The §6.0 honesty gate passed.** A bypass is NEVER used to skip a live blocker — that is exactly the Phase 5 Step 5.1 prohibition. The bypass exists only to cross the *structural* "no eligible reviewer" gap on an otherwise-clean PR.
2. **The consolidated comment (§6.2) is posted FIRST**, as the audit record, before any merge.
3. **The developer explicitly authorizes the bypass in writing** for THIS PR. An in-chat "yep" is not the record — the authorization is captured in the comment (e.g., a `> Bypass authorized by @<dev>: structural — sole code owner is the author; review consolidated above.` line).
4. **Phase 6 prints the merge command; the developer runs it** (Phase 9). Phase 6 does not merge.

The "no eligible reviewer" shape only arises when the active ruleset actually requires code-owner (or N-person) review; with no such requirement, the **Self, no code-owner path** row applies and no bypass is needed.

**Do NOT propose weakening the ruleset or CODEOWNERS to unblock a single PR** (e.g., exempting a path from code-owner review, dropping the approval count, removing a path from CODEOWNERS). Ad-hoc control-weakening to merge one PR is prohibited — it degrades the gate for every future PR to save one bypass. The **durable** fix for single-member-team friction is to add a second member to the code-owner team (when one is declared, `rc_get '.branch_protection.code_owner_team'` names it) so a genuine second-person review becomes possible; surface THAT as the structural recommendation (once, not per-PR), never a per-PR carve-out.

### 6.4 — NOT-READY output (honesty gate failed)

When any §6.0 precondition fails, emit this instead of an approval — and post NOTHING that asserts approval:

```markdown
## ⛔ Consolidated Merge Review — PR #<n> — NOT READY

**Reviewed at HEAD:** `<sha>`

Blockers preventing approval:
- <failed precondition> — <specific blocker> — <action to clear>
- ...

No approval posted. After the blockers clear, re-run the merge-approval gate — `/dev-lifecycle <n>` if you entered via the lifecycle, or `/enhanced-pr-review <n>` Phase 6 directly.
(Reminder: a new push dismisses any prior approval — land all fixes before the approval step.)
```

You MAY post this as a comment (it is a truthful status, not an approval), or surface it in chat only — developer's preference. Never convert a NOT-READY into an approval by softening the language.

### 6.5 — Handoff to merge (Phase 9)

Phase 6 ends by printing the exact merge command for the §6.1 case and handing off. It does not run it.

```bash
# Case "Formal approval" (teammate / dependabot PR), after the approving review lands:
gh pr merge $0 --squash

# Case "Self, no code-owner path" — after a provisioned bot's auto-approval
# OR another write-access human's approving review lands:
gh pr merge $0 --squash
# ...or, if neither is available, documented bypass (§6.3 satisfied):
gh pr merge $0 --squash --admin

# Case "Self, code-owner path" — documented bypass (§6.3 satisfied), developer runs:
gh pr merge $0 --squash --admin
```

`/dev-lifecycle` Phase 9 consumes the Phase 6 verdict as its merge gate: a `NOT READY` (§6.4) is a hard stop; an `APPROVED FOR MERGE` artifact plus (for the self-authored code-owner case) the recorded §6.3 authorization is the green light. Per the "Post-merge cleanup handoff" discipline, the actual `git branch -D` / worktree cleanup is printed for the developer's terminal, never run in-session.

---

## What This Skill Does NOT Do

- Does not auto-fix without developer confirmation
- Does not dismiss findings without developer approval
- Does not modify code during the report phase — only generates the triage table and drafts replies
- Does not wait for bots to finish reviewing (assumes review is already complete — bots typically post within 1–3 min of `ready_for_review`)
- Does not replace `/review-pr` — it calls `/review-pr` and adds bot reconciliation on top
- Does not distinguish between a bot's `NEUTRAL` check-run state and its inline comments: a bot can post a `NEUTRAL` check (the summary declining to judge the PR) while still posting individual BUG_PREDICTION-style inline comments. Always check both the `reviews` endpoint AND the `comments` endpoint.
- Does not let a `deferred` decision stand on a promise — the named issue must exist at merge time and be an issue, not a PR
- Does not treat a disposition marker as discharging Phase 1.7 — the two gates ask different questions and neither answers the other
- Does not auto-resolve threads that lack a recorded disposition — a reply alone is not a decision; the `cv-disposition` marker is. Silent auto-resolve would erase reviewer signal nobody decided on
- Does not trust an autonomous autofix agent's commits, thread replies, or thread resolutions as verified. Phase 1.7 re-reviews each action against the original finding — reconstructing the finding if the agent erased it by re-approving itself — and routes every non-`Verified-correct` disposition to the Fix path. It does NOT autonomously revert or rewrite the agent's commits; corrections and reverts are Fix rows the developer confirms (preserving the no-auto-fix invariant above)
- Does not count a bot-resolved review thread toward the Phase 4 resolution gate or the Phase 6 honesty gate on the strength of the `isResolved` flag alone — only a Phase 1.7 `Verified-correct` disposition (or a human resolver) clears it. A green CI check, a reviewer bot's re-approval, and a bot-set resolved flag can all be manufactured by the autofix agent and are re-verified, not trusted
- Does not defer findings unless the rationale matches one of the closed allowlist categories (A–D in §2.5.1) AND passes the format-level gate (§2.5.4) AND has explicit developer approval after the categorical match is established
- Does not accept the rationalizations enumerated in §2.5.5 ("STRUCTURALLY INVALID" arguments) — those route to Fix-in-PR regardless of how compelling the framing appears
- Does not perform the merge — Phase 6 emits the approval artifact and prints the merge command; the merge stays with the developer / `/dev-lifecycle` Phase 9. The skill never runs `gh pr merge` (admin or otherwise).
- Does not self-approve — for a PR the gh user authored, the Phase 6 artifact is a comment, never `gh pr review --approve` (GitHub rejects self-approval with `422`). The comment is the audit record for a §6.3 documented bypass, not a substitute for an approving review.
- Does not emit an approval while any §6.0 blocker is live — a pending (≠ passed) check, an unresolved thread, a `BLOCK` row, or an unaddressed `Fix` finding routes to the NOT-READY output (§6.4), regardless of time/exhaustion/authority pressure. Faithful reporting of each signal's actual state is non-negotiable.
- Does not weaken the ruleset or CODEOWNERS to unblock a PR — single-member-team review friction is solved by adding a member to the code-owner team (`.branch_protection.code_owner_team`), never by a per-PR control carve-out (§6.3).

---

## Adding support for a new bot

When a new AI reviewer is added to the repo, the `user.type == "Bot"` filter picks it up automatically with the fallback display name `"Bot: {login}"` — **no skill change is ever required for the skill to surface a new bot's findings.** Registering a bot is a CONFIG edit (the `botname` helper is generic; the roster is data), and to give it a pretty display name you add an entry to `.claude/repo-config.json` `.bots[]`:

1. Find the bot's login at each endpoint it posts to:
   ```bash
   gh api repos/${REPO}/pulls/{pr}/comments --jq '.[] | select(.user.type == "Bot") | .user.login' | sort -u
   gh api repos/${REPO}/issues/{pr}/comments --jq '.[] | select(.user.type == "Bot") | .user.login' | sort -u
   ```
2. Add a `.bots[]` entry (schema in `knowledge/repo-config.reference.md`):
   - **Dedicated-login reviewer** — set `.match` to a stable login substring (case-insensitive). Prefer a substring over an exact login so a bot that posts inline comments under one login and review summaries under a `…[bot]` variant resolves with one entry:
     ```json
     { "match": "graphite", "name": "Graphite", "login": "graphite-app[bot]" }
     ```
   - **Shared-login poster** (e.g. a CI workflow posting under `github-actions[bot]`) — set `.login` to the exact shared login and `.marker` to the HTML-comment marker the workflow places in its body:
     ```json
     { "login": "github-actions[bot]", "marker": "<!-- coverage-delta -->", "name": "Coverage Delta" }
     ```
     Order shared-login `.marker` entries before any broad `.match` that could also catch the same login.
   - Optionally add `.ci_check` (the bot's check-run name), `.blocking` (whether its negative verdict is a merge gate, consumed by §6.0 precondition 2), and the `.offline`/`.when` fields (consumed by `/dev-lifecycle` wait-for-bots), per the schema.
3. Commit the config change in the consuming repo as `chore: register <name> in repo-config bots roster`. The skill itself is unchanged.
