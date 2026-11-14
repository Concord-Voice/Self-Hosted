---
name: enhanced-pr-review
description: "Consolidate bot and project-side PR review feedback into one triage table. Collects every GitHub-app reviewer's comments/reviews via a type-aware `user.type==\"Bot\"` filter (display names and CI checks come from `.claude/repo-config.json` `.bots[]`), dispatches the specialist subagents the shared dispatch table and the repo's `.review_agents` declare, auto-triages known patterns, and surfaces only conflicts that need human judgment. Use when reconciling AI review feedback before merge, or when you need a consolidated merge-approval sign-off posted to the PR to satisfy a required-review rule. Also use when an autonomous autofix agent may have committed fixes to, replied to, or resolved reviewer-bot threads on the PR — Phase 1.7 re-verifies that unreviewed work before merge. Also use when review threads are closed but it is unclear what was decided on each one, when a bot posted a summary comment nobody answered, or when a finding was deferred to a follow-up issue that may never have been filed."
argument-hint: [pr-number]
allowed-tools:
  - Bash(gh pr *)
  - Bash(gh api *)
  - Bash(gh issue view *)
  - Bash(.claude/skills/enhanced-pr-review/check-review-dispositions.sh *)
  - Read
  - Grep
  - Glob
---

# Enhanced PR Review

> **Bot roster is config-driven.** This skill discovers reviewers structurally via the `user.type == "Bot"` filter (Phase 1), not from a hard-coded list — so any GitHub-app reviewer is picked up automatically with the fallback display name `Bot: {login}`. Pretty display names, CI-check names, and PR-body markers come from the OPTIONAL per-repo `.claude/repo-config.json` `.bots[]` array (read as data with `jq`, never by sourcing a script from the checkout). To register a bot for a nicer display name, add an entry to `.bots[]` — see the "Adding support for a new bot" section at the end. With `.bots == []` the generic fallback still works for every reviewer.
>
> **Three posting locations — the skill fetches all three.** Bot reviewers can post to ANY of three GitHub endpoints, and which one they use is reviewer-specific (no universal rule). `/pulls/{n}/comments` carries inline review comments (line-by-line); `/pulls/{n}/reviews` carries PR review summaries with a state (a bot's overall judgment); `/issues/{n}/comments` carries top-level conversation comments (verdict-only reviewers, Quality-Gate posters, and any `github-actions[bot]` workflow posting). PRs are issues under the hood — that's why the conversation endpoint is `/issues/...`. The skill polls all three in Phase 1 to avoid missing a reviewer (or a recommendation like a version bump) that only posts to the conversation endpoint.
>
> **Required vs optional.** Which reviewers gate the merge is roster data: a `.bots[]` entry with `"blocking": true` is a gatekeeper (`/dev-lifecycle` Phase 7.5 waits for it and STOPs if it is missing), `"offline": true` takes a bot out of the expected set, and `.when` scopes when it is expected. Every other AI reviewer is **optional**: never waited on, but fully reconciled here if it posts. Optional governs the WAIT, not the READ. A bot that posts a quota/usage-limit notice instead of a review is **EXHAUSTED for that PR** — drop it from the expected set for the remainder of the PR (it does not change the roster's `offline` flag). This skill reconciles whatever findings actually exist; classification never suppresses a finding.
>
> **Reviewer behaviours worth knowing** (observed on real PRs; re-check against your own reviewers, since bots change):
> - **Verdict-only reviewers** (e.g. Gitar) post the substantive verdict as ONE top-level conversation comment — a `<details>` block with an overall verdict plus a paragraph — and no line-by-line comments. Such a bot may ALSO submit a formal PR review (`COMMENTED`, `DISMISSED` on stale-dismissal, or `APPROVED` when its own auto-approve criteria match), and a bot `APPROVE` counts toward the required-review count. A matching CI check, if any, is a parallel signal; the comment carries the review.
> - **Severity-inflated, incremental reviewers** (e.g. CodeRabbit): re-rate every finding against `.claude/rules/` and the Phase-2.5 gate rather than accepting the bot's badge; it files findings over several passes, so an early "clean" pass is not terminal — re-check before the merge gate; it may auto-push a commit to the PR branch (`git fetch` and rebase before your next push); and its `CHANGES_REQUESTED` can be **sticky** — resolving threads does not clear `reviewDecision`, only a full re-review after a pushed fix does. Check thread state before reading a lingering `CHANGES_REQUESTED` as an unaddressed finding.
> - **A reviewer can be silently absent.** For example, when Copilot code review is disabled (billing, policy), `gh pr edit --add-reviewer "@copilot"` exits 0 and registers nothing, while `POST /pulls/<n>/requested_reviewers` returns 404. Mark such a bot `"offline": true` rather than escalating its absence, and do not hardcode the state — the probe in [`/weekly-deps` §3.6](../weekly-deps/SKILL.md) is the source of truth. When active, Copilot posts inline comments (login `Copilot`) AND a review summary (login `copilot-pull-request-reviewer[bot]`); the type-aware filter handles both.
> - **Codex** (`chatgpt-codex-connector[bot]`) posts inline comments (P1/P2-badged) and a formal `COMMENTED` review for its suggestions pass, AND a top-level conversation comment — a clean "didn't find any major issues" pass may land ONLY at `/issues/{n}/comments`, with no review-endpoint signal at all. No CI check. Summonable with an `@codex` conversation comment; latency varies from minutes (summoned) to many hours (unsummoned). It re-reviews every push and can surface new findings on a previously-clean PR. Its summary comment's per-review-type rows drive the Codex-conditional reviewers in the dispatch table.
> - **Quality-gate and CI posters** (e.g. SonarCloud, a release classifier, Semgrep SAST) post conversation comments — under a dedicated login, or under the shared `github-actions[bot]` login disambiguated by a body marker. Not reviewers in the prose-finding sense, but their verdicts are load-bearing signals; a release-classifier recommendation is the single most-missed signal in PR triage (see Phase 2.6).
> - **Subagent suite** — dispatched directly via the Agent tool per [`_shared/pr-review-agent-dispatch.md`](../_shared/pr-review-agent-dispatch.md) plus the repo's `.review_agents`: security-reviewer always (or the repo's `.review_agents.mandatory` floor, read from the merge base); red-team by the risk tiers (mandatory unless Codex's SECURITY review covered the head and no risk signal fired), plus the repo's `.red_team.critical_agent` at T3 when one is set; code-reviewer mandatory unless Codex's CODE review covered the head, and optional when it did — both rules are in § Codex-conditional reviewers of the dispatch table; docs-reviewer and scope-reviewer on their table triggers; plus file-pattern specialists (migration-reviewer, frontend-reviewer, and any repo-local domain reviewers). Each agent pins its own `model`/`effort` as a cost contract — **never pass a `model` override at dispatch.**
>   - **`@red-team` is not another reviewer.** It constructs executable proof-of-concept exploits in an isolated worktree rather than checking a list. Its findings arrive with a PoC and a validated fix; a `@red-team` claim WITHOUT a passing PoC is a hypothesis and is triaged as `Defer-with-issue`, never as a `Fix` row. Its tier and the inputs behind it go in the dispatch line, for example `red-team: T2, mandatory (Codex Security Review not covered; RS2)`.
>   - **`@architect` (Mode 4)** may be dispatched from Phase 2.5 when a finding implies the *design* is wrong rather than the code — its verdict decides whether the finding is an in-PR fix or a spec amendment.
>
> **Autonomous autofix agents are a DIFFERENT class from reviewers — see Phase 1.7.** Some platforms offer an autofix PR assistant (for example Blacksmith **Codesmith**'s "Autofix for your PRs") that, once a PR is `ready_for_review`, automatically pushes commits to fix failing tests, reviewer-bot comments, and red CI — **without any of this skill's discipline** (no `.claude/rules/` cross-check, no causality-radius test, no false-positive gate). By the time `/enhanced-pr-review` runs, such an agent may have already committed "fixes," posted replies, **resolved review threads**, and caused a reviewer bot to flip its verdict. **A commit, reply, or thread-resolution from an autofix agent is unreviewed work, not a verified resolution** — every green it produces is suspect (a resolved thread, a re-approved verdict, and a green CI check can all be *manufactured*, e.g. by resolving a thread it never fixed or deleting the test that was failing). Phase 1.7 detects an autofix footprint **behaviorally** — the one thing no reviewer bot does is author commits or resolve threads, so detection needs no config; its thread-resolution and reply signals are identity-independent, while commit detection is identity-heuristic (see Phase 1.7.1's limitation note) — re-verifies each action against the *original* finding, and routes every unverified action back through the normal Fix path. It NEVER auto-reverts; corrections are Fix rows the developer confirms. If no autofix agent has acted, Phase 1.7 detects no footprint and is a zero-cost no-op.

Reconcile all AI-authored code review feedback with project-specific subagent reviews for PR #$0. The skill fetches findings from every GitHub-app reviewer, not just one — a login-substring filter is the wrong abstraction because new reviewers keep being added and their login names aren't predictable. Identity-to-display-name mapping is data (`repo-config.json` `.bots[]`), not code baked into the skill.

---

## Phase 1 — Gather

**Step 1** — Resolve repo identity and load the bot roster:

```bash
gh repo view --json nameWithOwner --jq '.nameWithOwner'
```

Store as `REPO` (e.g., `{owner}/{repo}`).

Load the OPTIONAL per-repo bot roster into a shell variable for the three fetches below. `.bots[]` (each `{match?, login?, marker?, name, ...}`) supplies display names, the shared-login markers, and CI-check names; an absent file or `.bots == []` yields `[]`, in which case every reviewer falls back to `Bot: {login}`:

```bash
# Read the roster as data with jq. Never `source scripts/repo-config.sh` here: on a
# contributor's PR branch that file is the contributor's code, and sourcing it runs it.
BOTS_JSON="$(jq -c '.bots // [] | if type == "array" then . else [] end' \
  "$(git rev-parse --show-toplevel)/.claude/repo-config.json" 2>/dev/null || true)"
[ -n "$BOTS_JSON" ] || BOTS_JSON='[]'   # absent or unreadable → generic fallback naming
```

All three fetches page through the endpoint with `gh api --paginate`, pipe the pages into a separate `jq -s --argjson bots "$BOTS_JSON"` (`gh api --jq` takes no `--argjson`), and resolve a display name with one shared helper. The helper finds the FIRST `.bots[]` entry that matches the comment's author/body, else falls back to `Bot: {login}`:

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
gh api --paginate repos/${REPO}/pulls/$0/comments \
  | jq -s --argjson bots "$BOTS_JSON" '
    def botname($login; $body): ( [ $bots[] | . as $b | ($b.match // "") as $m | ($b.login // "") as $bl | ($b.marker // "") as $mk | select( ($m != "" and ($login | ascii_downcase | contains($m | ascii_downcase))) or ($bl == $login and $mk != "" and ($body | contains($mk))) ) | $b.name ] | first ) // ("Bot: " + $login);
    [ (add // [])[]
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

If no bot comments are found, note `"No bot inline comments for PR #$0"` and proceed — the Step 5 specialist suite may still produce subagent-only findings that need reporting.

**Step 3** — Fetch bot review summaries:

```bash
gh api --paginate repos/${REPO}/pulls/$0/reviews \
  | jq -s --argjson bots "$BOTS_JSON" '
    def botname($login; $body): ( [ $bots[] | . as $b | ($b.match // "") as $m | ($b.login // "") as $bl | ($b.marker // "") as $mk | select( ($m != "" and ($login | ascii_downcase | contains($m | ascii_downcase))) or ($bl == $login and $mk != "" and ($body | contains($mk))) ) | $b.name ] | first ) // ("Bot: " + $login);
    [ (add // [])[]
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
gh api --paginate repos/${REPO}/issues/$0/comments \
  | jq -s --argjson bots "$BOTS_JSON" '
    def botname($login; $body): ( [ $bots[] | . as $b | ($b.match // "") as $m | ($b.login // "") as $bl | ($b.marker // "") as $mk | select( ($m != "" and ($login | ascii_downcase | contains($m | ascii_downcase))) or ($bl == $login and $mk != "" and ($body | contains($mk))) ) | $b.name ] | first ) // ("Bot: " + $login);
    [ (add // [])[]
      | select(.user.type == "Bot")
      | { id: .id,
          author: .user.login,
          bot: botname(.user.login; (.body // "")),
          created_at: .created_at,
          body: .body }
    ]'
```

`id: .id` is load-bearing, not decoration: Phase 3 REQUIRES a `Thread` value on every row and specifies the top-level comment's id for PR-level verdicts. Without it selected here, that column has no producing code anywhere in the skill.

Why a third fetch — this is where verdict-only reviewers and CI-poster workflows live. GitHub treats PRs as issues under the hood, and the `/issues/{n}/comments` endpoint surfaces top-level CONVERSATION comments on the PR (the running thread visible in the PR's "Conversation" tab). Loud-but-easy-to-miss signals live here — for example a release/version-bump classifier, or a static-analysis summary, posted under a shared CI login and disambiguated only by a body marker.

**Why marker-based detection** — a shared CI login (commonly `github-actions[bot]`: every `gh api` call from any workflow posts under it) cannot be disambiguated by login alone. The body's leading `<!-- marker -->` HTML comment is a deliberate convention each workflow places for body-update idempotency (so the workflow can edit-in-place rather than spam-comment). The `botname` helper inspects that marker via the roster's `.login`+`.marker` entries. **Disambiguating a new shared-login poster is a config edit** — add a `{login, marker, name}` entry to `.bots[]`; no skill change is needed.

**Verdict-only shape:** some reviewers emit ONE comment per PR carrying an overall judgment — e.g. a `<details>` block with an `✅ Approved` / `⚠ Changes Suggested` / `❌ Blocked` summary and a paragraph of reasoning — and no line-by-line findings. In the triage table (Phase 3), such a row has no `File:Line` value; the body cell carries the summary. This is structurally different from a line-by-line reviewer, and the triage table must handle both shapes.

**Reviewers that also run as a CI check** — some bots additionally surface a CI check whose status (`success` / `failure` / `neutral`) is a parallel signal already captured by `/ci-status` and `/dev-lifecycle` Phase 6 (CI MONITOR). The check name, when set, lives in the roster entry's `.ci_check`. This skill's reconciliation focuses on the comment-body content; the check status is the orchestrator's concern.

**But a summoned reviewer may post TWO comments, and the first is not a review.** Some bots echo the summon within seconds and append a one-line reply ("On it — running a review now."); the verdict follows separately, sometimes most of an hour later. **Do not open a triage row for the acknowledgement** — it carries no finding, no verdict, and nothing to dispose of, so a row for it becomes a conversation the Phase-4 disposition gate then demands a decision on. Recognize it by that bot's roster `ack_pattern` (`.claude/repo-config.json` `.bots[]`), the same field `/dev-lifecycle` Step 7.5.2 uses to keep an ack from satisfying its arrival gate. Gate the pattern on the body carrying no verdict and being acknowledgement-shaped (a leading quoted summon plus a short unquoted reply), so a verdict that merely quotes the ack wording is still a verdict and gets its rows. An ack's presence *without* a verdict is a real signal, just not a review one: the review started and did not finish, which is Phase 7.5's business rather than this skill's — if you reach Phase 1 and find only an ack, the wait was released early and Phase 7.5 should be re-entered before reconciling.

**Some bots ack by marker rather than prose, and edit the verdict in place.** CodeRabbit's ack is an HTML-comment invocation marker under an auto-generated-reply header — a separate comment from its walkthrough — and its *verdict* is that walkthrough **edited in place**, keeping a `created_at` that can predate the summon. Sort or filter its comments by `created_at` and you select the ack, which is how a completed review gets reported as silence. Test every candidate comment and take any verdict match; never take `last`.

**Release-signal poster — special prominence in reconciliation (config-gated).** When the repo declares a `.release_signal` (see Phase 2.6), a release/version-bump classifier comment is treated specially: a recommendation to bump the version that is needed-but-absent is a **blocking-merge concern** with a top-of-table row in Phase 3 (severity: BLOCK), not a routine bot-only finding. Repos without `.release_signal` skip this entirely. See Phase 2.6.

**Step 5** — Dispatch the project-side specialist-agent suite **directly via the Agent tool**, per the single source of truth [`_shared/pr-review-agent-dispatch.md`](../_shared/pr-review-agent-dispatch.md) (security-reviewer always (or the repo's `.review_agents.mandatory` floor, read from the merge base with the default branch); red-team by the Codex-conditional risk tiers, plus the repo's `.red_team.critical_agent` at T3 when one is set; code-reviewer mandatory when Codex's code review has not covered the head and optional when it has; docs-reviewer and scope-reviewer on their triggers; plus file-pattern specialists; the IPC row is the model-invocable `/ipc-channel-audit` skill), unioned with the specialist subagents the repo declares in `.claude/repo-config.json` `.review_agents` (`.base` always-run agents plus `.patterns[]` matches). Follow the `review-pr` skill's dispatch procedure for the `.review_agents` part — its mandatory-base union and its exclusion of any agent whose definition this PR modifies — rather than re-deriving it, and take the always-run floor from its trust guard's resolved `MANDATORY_BASE` (read from the merge base with the default branch), never from the checkout's repo-config. The floor sets only the always-on rows; the Codex-conditional and risk-tiered rules above still decide everything else. Dispatching the agents directly here inherits the authorization of this `/enhanced-pr-review` invocation (reached via `/dev-lifecycle` Phase 8 or invoked directly). **Do NOT substitute with a single agent or manual review.** (Why direct dispatch, not `Skill(review-pr)`: the `review-pr` skill is `disable-model-invocation: true` — a user-facing convenience the model cannot invoke; agent dispatch bypasses the skill boundary. See `.claude/rules/skill-authoring.md`, installed there by `install.sh claude`.)

> **Drift / config-schema review is delegated.** Reviewing config-drift or config-schema changes is the job of whatever reviewer the repo wires up for those paths in `.review_agents` (`.patterns[]`); this skill does not own that check, and any review fixtures for it live alongside that agent, not here. This skill ships no test fixtures of its own beyond the disposition gate's — its surface is the reconciliation/gate logic above, exercised against live PRs.

### Phase 1.5 — Catalog dispatch for Phase-8 candidate skills

After the always-dispatch agent suite returns (per Step 5's direct dispatch), consult the [`dispatchable-skills.md` catalog](../_shared/dispatchable-skills.md) `## Phase 8: FINAL REVIEW` section for additional candidate skills.

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

**Signal B — bot-resolved review threads** (this is the Phase 4 hole, surfaced early). Reuse Phase 4 Step 4.1's `reviewThreads` GraphQL query but read `resolvedBy { login }`: a thread with `isResolved: true` whose `resolvedBy.login` ends in `[bot]` was resolved by an automated actor, not a developer.

**A bot resolver is NOT a reliable autofix-agent signal.** Reviewer bots resolve their own threads routinely — on one measured PR a reviewer bot resolved 13 of 17 threads, 11 of them after the developer had replied. Treat a `[bot]` resolver as *one weak input* to be corroborated against Signals A and C (authored commits, dismissal replies), never as identification on its own. Whether the thread carries a decision is Phase 4's question and is answered by the marker, not by the resolver's identity.

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

Default disposition = **UNVERIFIED** for any action tied to a finding, a bot-resolved thread, or a bot reply. (A commit-only footprint that §1.7.2 shows addresses no finding and implicates no thread is dispositioned **`No-finding`**, not UNVERIFIED — see the table below.) Re-verification reuses the EXISTING specialist reviewers from Step 5 ([`_shared/pr-review-agent-dispatch.md`](../_shared/pr-review-agent-dispatch.md) plus the repo's `.review_agents`) — scope the matching reviewers (security-reviewer, red-team, code-reviewer, any domain specialist, etc.) to the **autofix commit range** with the explicit question:

> *"Does this change correctly and completely close `<reconstructed original finding>`, with no regression, silent failure, deleted/weakened test, or unrelated change?"*

There is **no dedicated autofix-reviewer persona** — the specialists already judge diffs; Phase 1.7 only *scopes* them to the autofix commits and *pairs* them with the original finding. Assign each action exactly ONE disposition:

| Disposition | Meaning | Routes to |
|---|---|---|
| **Verified-correct** | The change fully closes the original finding with no new defect; specialist re-review confirms against the diff. | **Note** row — keep the change. This is the ONLY disposition that clears the autofix action itself under §6.0 precondition 9. It is a *separate* question from whether the thread carries a decision (Phase 4's marker) — the change being correct and the decision being recorded are two facts, and neither implies the other. |
| **Incorrect-fix** | The change is present but does NOT close the finding (e.g. a `safeConcat()` helper that is still string concatenation) or is incomplete. | **Fix in this PR** — the finding is still live. |
| **False-dismissal** | The agent resolved/replied-away a TRUE-POSITIVE finding with no real fix. Judge the dismissal's claims against the CURRENT HEAD diff — autofix agents can evaluate against stale commits (one dismissed a valid finding by describing the pre-push diff). | **Fix in this PR** — reopen the finding. |
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

> **Named case — metric-count drift (Key Counts drift).** When the repo sets `.docs.counts_script` in `.claude/repo-config.json`, a numeric or metadata count that this PR makes stale (for example a "Key Counts" block in `CLAUDE.md`) is a documentation finding under (i): it is **non-deferrable and refreshed in THIS PR**. The count is metadata, and the PR may touch no `.md` file (so `docs-reviewer` is never dispatched), so raise the finding by probing: run the script with no arguments (dry-run), resolved the same way as `/dev-lifecycle` Phase 4: the keys come from the default-branch tip, and the script must be a tracked regular file that matches the tip, as must every path in `.docs.counts_script_deps[]` and the script's directory (the repo files it sources or calls; it must call no others), because a PR must not choose the code its reviewer runs. A non-empty diff IS the finding; remediate with its `--apply` mode. `/dev-lifecycle` Phase 4 front-loads this probe; this section is the merge-time backstop.

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
# Load the helper from the default branch, never this checkout (on a PR branch it is the PR's code).
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

The comment is identified in Phase 1 Step 4 by its `.release_signal.marker` (the `RELEASE_MARKER` above) under a shared CI login. Its exact shape is the classifier workflow's concern, not this skill's — read the actual body for the row labels and recommendation wording. Two shapes are common:

- **Bump-oriented** — rows like `Version bump needed: Yes/No` and `Version bump found: Yes — <version> / No — current version is <version>`, plus a recommendation line naming the action (e.g. "Bump the version in `<version_manifest>` before merge", where `<version_manifest>` is `.release_signal.version_manifest`).
- **Note-oriented** — a per-axis table whose last column (`Bump?` or similar) asks for something per axis, e.g. `⚠️ ADD A NOTE` under the changelog's unreleased section, or `⚠️ BUMP to <n+1>` for an API/IPC contract version. Such a classifier may carry no recommendation line at all, which is why §2.6.2 keys on the asking cell rather than on prose.

**Different axes can ask for different things and must not be collapsed.** A contract-version axis genuinely wants a bump *in this PR*. A client-version axis that asks for a release note wants a changelog entry — a version bump is a separate decision about whether this PR finishes something worth shipping, and a PR that makes it carries the changelog promotion in the same diff. So `ADD A NOTE` never means "also bump".

### 2.6.2 — Categorize the recommendation

Apply this decision tree:

| Classifier asks for (on the axis it gates) | Category | Triage-table severity | Action |
|---|---|---|---|
| Nothing — not needed (any form) | `Release-signal: ALL CLEAR` | INFO | None — note in summary |
| Something the PR already supplies (bump found, note present) | `Release-signal: SATISFIED` | INFO | None — note in summary |
| A bump or note that is **absent** | `Release-signal: BLOCKING` | **BLOCK** | Supply exactly what the classifier asks for before merge (the bump, or the changelog note — never "bump instead" when it asks for a note, since a changelog gate may reject a hand-written version section), OR defer with explicit developer approval per §2.6.4 |

Read each axis separately and keep its own semantics: a contract-version `BUMP` is a genuine request to edit that contract file. Any row the classifier marks as a hard failure (e.g. `⛔`) is BLOCK. A *recommended bump level* row, where present, is advisory — the developer decides when to cut; mention the level in the summary, and surface a "this PR bumps X; criteria say Y" mismatch as LOW with the basis, never BLOCK on it.

The BLOCKING case is the one that gets missed. It MUST appear as a top-row in the Phase 3 triage table (row ordering: BLOCK rows first, per Phase 3 layout rules).

### 2.6.3 — Interaction with `/dev-lifecycle`

[`/dev-lifecycle`](../dev-lifecycle/SKILL.md) handles the orchestrator-side version-bump approval (it reads the same `.release_signal` config and waits for explicit developer approval before bumping). This skill's role is to make whichever recommendation fired un-missable in the reconciliation table. A changelog-note request is ordinary in-PR work — write the entry and push; it is not a checkpoint. A contract-version bump moves a compatibility boundary and does gate on developer approval.

What routinely rides the review-loop fix-commit batch is the changelog entry, not a version bump — almost no PR bumps. A bump is a decision about whether this PR finishes something worth shipping, not a step in a review. When that decision is yes it *may* ride the batch, alongside the other reconciliation fixes, so it piggybacks on a CI cycle that is already going to happen; if the repo's changelog gate requires the bump to arrive with the promotion that produced its version section, both land together. The Release-signal BLOCK row surfaces the requirement; the actual commit lands when Phase 3 Post-Report fixes are applied.

### 2.6.4 — Deferring a BLOCK recommendation

A `Release-signal: BLOCKING` row can ONLY be deferred if BOTH conditions hold:
1. The developer explicitly approves the defer in writing on the PR (not just an in-chat ack)
2. The defer reason matches one of: the PR will be closed/abandoned; the version bump is being handled in a sibling PR that will land first (valid only for a bump — a changelog note describes *this* PR's change, so no sibling can supply it); the change is genuinely not user-observable, in which case the correct artifact is the repo's changelog-exemption mechanism (e.g. a `Changelog-Exempt: <reason>` commit trailer) rather than a defer at all; or the release-signal check itself is misconfigured for this PR shape (rare — investigate the classifier rather than ignore it).

There is no `Defer (category: ...)` row format for the release signal — the §2.5 deferral gate categories (A/B/C/D) are about code-finding defers, not release-engineering signals.

---

## Phase 3 — Report

Output the triage table. The **Source(s)** column names each reviewer explicitly — use the `bot` field from Phase 1 (the display name resolved from `.bots[]`, e.g. "Copilot", "SonarCloud", or a `Bot: graphite-app[bot]` fallback) rather than a generic "bot" label. Multi-bot agreement should be listed with a `+` separator.

**Two finding shapes coexist in the table:**
- **Line-level findings** (inline review comments, subagent findings) populate `File:Line` with a specific path:line; the Finding column holds the line-level description.
- **PR-level verdicts / recommendations** (a verdict-only reviewer's overall judgment, a Quality-Gate result, a release-signal version-bump recommendation, a review summary) leave `File:Line` blank or marked `—`; the Finding column carries the overall verdict or recommendation. These rows describe the PR as a whole.

**The `Thread` column is REQUIRED on every row.** It carries the GraphQL thread ID (`PRRT_…`, from the `id` field in Phase 4 Step 4.1) for a line-level finding, or the top-level comment's `id` for a PR-level verdict; `—` only for a subagent-only finding that has no GitHub conversation behind it. This column is what makes a decision *addressable*: without it the table records that N findings were fixed but never which conversation each fix answers, so nothing downstream can verify that the finding a commit claims to close is the finding whose thread got resolved. Phase 3.5 and the Phase 4 gate both key on it.

**Row ordering: BLOCK rows first.** Any row with a `BLOCK` severity (a release-signal recommending a version bump or note while it is absent — only when `.release_signal` is configured — or any subagent finding flagged blocking-merge) sorts to the top of the table regardless of category. The developer sees blockers first; everything else is below.

```
Enhanced PR Review Report — PR #$0
──────────────────────────────────────────────────────────────────────────
| # | Finding | File:Line | Thread | Source(s) | Category | Triage | Action |
|---|---------|-----------|--------|-----------|----------|--------|--------|
| 1 | ⚠ BLOCK: <subagent blocking-merge finding, or release-signal version-bump if .release_signal is set> | — / path:line | `PRRT_…` | <reviewer> | PR-level / Subagent-only | — | **Resolve before merge** |
| 2 | ⚠ BLOCK: autofix "fix" does not close the finding — <original finding> still live at HEAD | src/api/users.go:142 | `PRRT_…` | Autofix: <login> (autofix) + <reviewer> (finding) | Autofix-handled | UNVERIFIED → Incorrect-fix | **Fix** |
| 3 | ⚠ BLOCK: Regression test deleted to green CI — restore it | src/api/users_search_test.go | `PRRT_…` | @code-reviewer + Autofix: <login> (autofix) | Autofix-handled | UNVERIFIED → Revert-needed | **Fix (revert)** |
| 4 | Verdict-only reviewer: ✅ Approved — "No issues found." (stale if it re-approved an unverified autofix HEAD) | — | `IC_…` | Bot: <login> | PR-level verdict | — | Note (contradicted by rows 2–3 if so) |
| 5 | Quality Gate passed (0 new issues) | — | `IC_…` | <reviewer> | PR-level verdict | — | Note (no action needed) |
| 6 | Missing nil-check before deref | src/api/servers.go:210 | `PRRT_…` | @code-reviewer + <bot> | Agreed | — | Fix |
| 7 | Authorization scope check missing | src/api/dm.go:88 | — | @code-reviewer | Subagent-only | — | Fix |
| 8 | Incomplete effect dependency list | src/ui/components/X.tsx:34 | `PRRT_…` | <bot> | Bot-only | Auto: FP (.claude/rules/{file}) | Dismiss |
| 9 | Approach disagreement on retry policy | src/net/index.ts:201 | `PRRT_…` | <bot> + @code-reviewer | Conflict | — | Developer decision |

Summary: BLOCK rows: N | Autofix-handled: N (N verified, N unverified→fix) | PR-level verdicts: N | Agreed: N | Bot-only: N (N valid, N FP) | Subagent-only: N | Conflicts: N
Bots that posted: <list each bot from the `bot` field with its contribution, e.g. "Copilot (N findings), SonarCloud (1 verdict)"; name an expected bot that is offline or exhausted as such>
Autofix footprint: <agent display name> — N commits, N thread-resolutions (N verified, N unverified→fix) | or "none"
──────────────────────────────────────────────────────────────────────────

Recommended next steps:
1. Fix all Agreed + valid Bot-only + Subagent-only findings
2. Review Conflicts and decide
3. Carry every row to a disposition and record it in its thread (Post-Report)
4. Post/update the Phase 3.5 reconciliation post, then run the disposition gate from the default branch (§3.5.3)
```

If NO bots posted findings (the filter returned an empty array in Phase 1), the report should explicitly call that out — an empty bot-findings section is itself a signal (bots either agree the PR is clean, or they haven't reviewed yet).

---

## Post-Report — Interactive Phase (the disposition ledger)

After the developer reviews the table, every row is carried to a **disposition** — a recorded decision, written into the conversation the finding came from. The ledger is the set of those dispositions. It is complete when the disposition gate (§3.5.3, run from the default branch) exits 0, and Phase 4 will not pass until it does.

### The four dispositions (closed set)

| Disposition | Means | `evidence:` field |
|---|---|---|
| `accepted` | The finding is valid and the fix has landed. | the commit SHA that landed it (7–40 hex) |
| `rejected` | The finding is declined — false positive, intentional behavior, or the finding stopped existing (duplicate, code removed). | the `.claude/rules/*` path or convention the rejection rests on, or the reason (`duplicate of row 1`) |
| `deferred` | Tracked as follow-up work. | `#NNN` — **the issue must already exist**; the gate calls `gh issue view` and fails closed if it does not, and rejects a PR number in an issue's place |
| `undecided` | The question is genuinely open and you know it — it needs someone else's call, or a design decision first. | what it waits on (`@<dev> — retry policy`) |

A finding that stopped existing — a duplicate, or code that was deleted — is `rejected` with the reason as its evidence (`rejected evidence: duplicate of row 1`). A separate `superseded` verb was tried and removed: nothing branched on it, so it was vocabulary wearing a disposition's ceremony.

`undecided` **blocks the gate**, exactly like a thread with no marker at all. It is not an escape hatch and it will not let a PR merge. What it buys is the difference between *"we know this is open and here is what it waits on"* and *"nobody decided and nobody noticed"* — two states that look identical in a silent thread. Recording an open question truthfully is always available, which is what makes mis-filing it as `rejected` or `deferred` to clear the gate indefensible rather than merely discouraged.

Nothing else is a disposition. "Acknowledged", "will look at it", a 👍 reaction, and a bot's own thread-resolve are not decisions on record.

`deferred` records the *outcome*; it does not by itself justify one. The justification still passes §2.5.1's closed allowlist and the §2.5.2 radius test, and §2.5.4 already requires the issue to pre-exist rather than be opened to satisfy a gate. This table adds only that the promised issue must be *verifiable* at merge time — a follow-up that was never filed is an accepted finding wearing a deferral's clothes.

**What §2.5.4's pre-existence rule is aimed at.** It targets an issue minted to *manufacture a justification* — filing something so a defer rationale has a number to point at. It does not forbid repairing a deferral you genuinely intended and forgot to file: filing it properly (adjacency scan, labels, milestone — not a stub), then saying in the thread and the reconciliation row that it was filed late, is the honest repair and is preferable to forcing an unrelated change into a long-green PR at the merge gate. Disclose the late filing; never let clean sequencing be implied by silence.

### The reply shape

Each in-scope thread gets one reply with two parts, in this order:

1. **The reasoning, in prose** — what you concluded and what you checked to conclude it. This is the part a human reads six months from now.
2. **One marker line** — `<!-- cv-disposition: <verb> evidence: <value> -->`

```markdown
Fixed in `e5c34d0`. Good catch — it also caught a second-order effect of an earlier
change in this PR, so the regression test now pins both.

<!-- cv-disposition: accepted evidence: e5c34d0 -->
```

```markdown
Declining this one — I believe it is a false positive, and here is the check I ran. The guideline
is "No `fmt.Sprintf` for SQL", and its target is value interpolation. This call interpolates a
column *identifier* drawn from a compile-time constant set, never from request data.

<!-- cv-disposition: rejected evidence: .claude/rules/<domain>.md -->
```

The marker is an HTML comment, so it is invisible in the rendered PR — the reviewer sees only your prose. It exists so the gate can verify a decision was made without trying to parse whether prose constitutes one.

Post the reply to the thread:

```bash
gh api repos/${REPO}/pulls/$0/comments/{comment_id}/replies \
  -X POST -F body=@reply.md
```

The `/replies` endpoint works for any bot's comment regardless of which GitHub app posted it — the thread-reply contract is endpoint-level, not author-specific. Stage the body to a file: marker text contains `<`/`>` and shell-quoting a multi-line body with backticks is where this breaks.

**Write the marker yourself.** The gate reads markers only from non-bot authors whose `authorAssociation` is `OWNER` or `MEMBER` — so a marker a bot posts, or one from a read-only collaborator or drive-by account, does not count. `COLLABORATOR` is deliberately excluded: GitHub returns it for read and triage access, so it is a *relationship*, not an authorization level.

### Applying the fixes

**For findings marked Fix:** apply the fix or invoke `@docs-writer` for documentation fixes, then disposition the thread `accepted` with the SHA that landed it. The SHA is what ties the row to the commit; a fix that landed without its marker leaves the ledger incomplete even though the code is correct.

**For Conflicts:** wait for the developer decision, then disposition with whichever verb the decision selected.

---

## Phase 3.5 — The reconciliation post (REQUIRED when any reviewer spoke, one per PR)

This skill posts its own comment to the PR. It is the single artifact that reconciles **every** review signal — each bot's findings, each specialist subagent's findings, and the decision on each — into one record that lives with the PR rather than in terminal scrollback.

**It posts regardless of merge-readiness.** A review whose outcome is "three findings deferred and one still under discussion" needs this record more than a clean one does, not less. This is why the reconciliation post is here and not folded into Phase 6: Phase 6's artifact is gated behind the §6.0 honesty gate and only ever appears on a PR that is *ready*, so a PR that stalls would carry no record of what was decided at all.

**Why the summary comments make this mandatory rather than nice-to-have.** Bots post a master comment enumerating their findings and then one inline comment per finding. Top-level conversation comments have no thread and no Resolve affordance — GitHub offers nowhere to record a decision against them — so no per-thread gate can ever reach that surface. PRs are routinely observed with **zero** review threads and several top-level bot comments, where a thread-only gate reports "all threads resolved" over a review surface it never read. The reconciliation post is where those bots get answered.

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
| <verdict-only bot> | 1 summary verdict | N | threads + row N |
| <quality-gate bot> | Quality Gate | 0 new issues | this post |
| <release-signal bot> | version-bump recommendation | 1 | this post — fixed in `<sha>` |
| red-team | subagent | 1 | row 3 |
| code-reviewer | subagent | 0 | — |

### Decisions
| # | Finding | Source(s) | Thread | Decision | Evidence |
|---|---|---|---|---|---|
| 1 | <finding> | <reviewer> | `PRRT_…` | ✅ Accepted | fixed in `<sha>` |
| 2 | <finding> | <reviewer> | `PRRT_…` | ❌ Rejected | <rule path or measurement> |
| 3 | <finding> | red-team | `PRRT_…` | ✅ Accepted | fixed in `<sha>` |
| 4 | <finding> | code-reviewer | `PRRT_…` | ⏭ Deferred | #NNN — category A, out-of-radius |
| 5 | Duplicate of row 1 | <reviewer> | `PRRT_…` | ❌ Rejected | duplicate of row 1 |

**Ledger:** N findings · N accepted · N rejected · N deferred · 0 undecided
```

**Reviewer names come from `.claude/repo-config.json` `.bots[]`, not from the script.** Names must match the roster: either the literal login, or the `name` your `.bots[]` entry declares for it — registering a bot for a nicer display name is the same config edit that gives it a display name in the Phase 3 table, so the two cannot drift. The gate resolves a *persona* per comment — marker first, so two workflows sharing the `github-actions[bot]` login (e.g. `<!-- semgrep-results -->` and `<!-- classify-release -->`) are required separately rather than one satisfying the other. The script holds no alias list. The roster is read from the base branch (`origin/HEAD`), never the working tree — this gate judges a PR, so a working-tree roster would let the branch under review declare the aliases that clear its own review. **That is correct and it has a cost worth naming: the gate structurally cannot catch a roster the PR itself breaks.** It judges against the base branch's good copy, passes, merges — and every SUBSEQUENT PR then fails at exit 2. A pre-commit check that runs this same gate against the WORKING-TREE roster closes that, and is safe there precisely because a pre-commit hook is not the thing deciding whether the PR's reviews are covered.

**Agent names are written bare in this post, never `@`-prefixed** (`red-team`, not `@red-team`): GitHub turns an `@`-prefixed word into a mention of whoever owns that login. The same holds for every disposition reply, review body and issue this skill writes. See § Agent names in GitHub posts in the dispatch table.

Every reviewer that posted anything gets a row in **Reviewers consulted**, including summary-only bots with no findings — that row is what records they were read rather than skipped. Every triage-table row gets a row in **Decisions**, carrying the same `Thread` handle so a reader can walk from the decision to the conversation it answers.

**Required vs. example — what the gate verifies, and what is convention.** Be precise about this, because the two are not the same size. `check-review-dispositions.sh` verifies exactly three things about this post: that exactly one exists, that a repo member (not a bot) authored it, and that every reviewer which spoke on any of the three surfaces — review threads, top-level comments, and PR reviews — is named in it by login or by a roster display-name alias. It does **not** parse the Decisions table, cross-check `Thread` handles, or count rows. The table shape above is the house format a reader relies on; the reviewer-coverage clause is the part with teeth. Do not read "REQUIRED" as "machine-verified in full".

The `Decision` values are the same four verbs as the thread markers. A row still under discussion is `undecided` and keeps the ledger open; Phase 4 will not pass while the count is non-zero.

### 3.5.2 — Post once, then edit in place

The gate requires **exactly one** reconciliation post. Re-running the skill after a fix commit updates the existing comment; it never adds a second, so the PR carries one current record rather than a pile of superseded ones.

```bash
# --paginate + jq -s: a long PR spreads comments over several pages, and a first-page-only
# lookup misses the existing post and creates a second one, which the gate then blocks.
EXISTING=$(gh api --paginate "repos/${REPO}/issues/$0/comments" \
  | jq -rs '[(add // [])[] | select(.body | test("<!--\\s*cv-review-reconciliation\\s*-->")) | .id] | first // empty')

if [ -n "$EXISTING" ]; then
  gh api "repos/${REPO}/issues/comments/${EXISTING}" -X PATCH -F body=@reconciliation.md
else
  gh pr comment "$0" --body-file reconciliation.md
fi
```

Stage the body to a file — the marker and the tables contain characters that make inline `-f body=` quoting fragile.

### 3.5.3 — Verify

```bash
GATE_REF="$(git symbolic-ref --quiet refs/remotes/origin/HEAD || echo refs/remotes/origin/main)"   # full refname: a branch or tag named origin/main cannot shadow it
case "$GATE_REF" in refs/remotes/origin/?*) GATE_SH="$(git show "$GATE_REF:.claude/skills/enhanced-pr-review/check-review-dispositions.sh")" ;; *) GATE_SH= ;; esac
if [ -n "$GATE_SH" ]; then bash -c "$GATE_SH" check-review-dispositions "$0"; else echo "UNKNOWN: no gate script on $GATE_REF" >&2; (exit 3); fi
```

Exit 0 means every in-scope thread carries a decision, every deferral names an issue that exists, and the reconciliation post accounts for every reviewer that spoke. Exit 1 → the blocking list is printed. Exit 2 means the check could not be made — treat that as UNKNOWN, never as clean. Any other exit (3 when no gate script could be read from the default branch, or a crash) → UNKNOWN, never clean.

**The gate runs from the default branch, never from this checkout.** A PR can edit the checkout's copy, for example by adding `exit 0` at the top, and a gate the PR controls verifies nothing. So every invocation reads the script from `refs/remotes/origin/HEAD` with `git show` and runs that copy. The full refname matters: a short `origin/main` resolves to a local branch or a fetched tag of that name before the remote-tracking ref, and a PR can supply either, the same trust rule the gate already applies to the roster. A PR that changes the gate is therefore judged by the previous gate, and its own version takes effect after it merges.

**What a green run proves, and what it does not.** It proves the decisions are recorded, addressable, and auditable. It does **not** prove a human made them: `gh` authenticates as the repo owner, so an agent's reply is authored by a `User` and clears the non-bot filter exactly as the developer's would. The gate cannot distinguish them and does not try. What changes is that the reasoning is now written down in one reviewable place instead of living in a terminal scrollback — a developer can read the reconciliation post and audit five decisions in one pass. Human judgment enters at the Phase 8 mode gate (`On-the-Loop` pauses on the triage outcome), not at this check. Never cite a green run as evidence that a person reviewed anything.

---

## Phase 4 — Pre-merge thread resolution check

This phase runs **after** all fix commits land and CI is green. It gates `/dev-lifecycle` Phase 9 (MERGE-READY) on one invariant: **every in-scope review conversation carries a recorded human decision** — a Phase 3.5 disposition marker — and the reconciliation post accounts for every reviewer that spoke.

Run the check; it is the gate, and the rest of this phase is how to read and clear its output:

```bash
GATE_REF="$(git symbolic-ref --quiet refs/remotes/origin/HEAD || echo refs/remotes/origin/main)"   # full refname: a branch or tag named origin/main cannot shadow it
case "$GATE_REF" in refs/remotes/origin/?*) GATE_SH="$(git show "$GATE_REF:.claude/skills/enhanced-pr-review/check-review-dispositions.sh")" ;; *) GATE_SH= ;; esac
if [ -n "$GATE_SH" ]; then bash -c "$GATE_SH" check-review-dispositions "$0"; else echo "UNKNOWN: no gate script on $GATE_REF" >&2; (exit 3); fi
```

Exit 0 → proceed. Exit 1 → the blocking list is printed; clear it. Exit 2 → the lookup failed; treat as UNKNOWN and re-run, never as clean. Any other exit (3 when no gate script could be read from the default branch, or a crash) → UNKNOWN, never clean.

**Why the invariant is "carries a decision", not "was resolved by a human".** The obvious alternative — *a thread resolved by a `[bot]` actor is blocking unless an autofix re-review cleared it* — misfires in both directions when measured. Reviewer bots resolve their own threads as routine housekeeping once a developer has replied (on one measured PR, a reviewer bot resolved 13 of 17 threads, 11 of them after substantial developer replies), so that rule blocks a large majority of well-handled threads; a gate that blocks the common good case gets overridden as routine, and an overridden gate stops catching the real hole — on that same PR, two threads that received no human reply at all and were closed by the bot with no decision on record. Keying on the presence of a human disposition clears the housekeeping and catches the silence. Phase 1.7 is unaffected: an autofix agent's *code change* still needs its `Verified-correct` disposition under §6.0 precondition 9. What changed is that a reviewer bot tidying up its own thread is no longer conflated with an autofix agent manufacturing a green.

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
      bot: .comments.nodes[0].author.login,
      first_comment: (.comments.nodes[0].body | .[0:140]),
      has_dev_reply: ([.comments.nodes[] | select(.author.__typename != "Bot")] | length > 0),
      has_disposition: ([.comments.nodes[]
                         | select(.author.__typename != "Bot")
                         | select(.body | test("<!--\\s*cv-disposition:\\s*[a-z]+"))] | length > 0)
    }
]'
```

**Scope: a thread is in scope if it is unresolved OR bot-authored** — matching `check-review-dispositions.sh`, which is the authority. Both clauses are load-bearing and neither subsumes the other:

- *unresolved* catches a human-authored thread nobody answered.
- *bot-authored* catches a bot finding that was resolved — by anyone, human or bot — without a decision being recorded. Keying the second clause on the **resolver** instead (`resolvedBy … test("\\[bot\\]$")`) misses the case where a developer clicks Resolve on a bot thread and moves on without replying, which is the single easiest way to close a finding without deciding it.

A human-authored thread a human already resolved is out of scope: two people talked and one of them closed it, which is self-evidently a decision.

The bot-detection filter uses GraphQL's `author.__typename == "Bot"` rather than a login-substring match. This is the GraphQL-side equivalent of Phase 1 Step 2's `user.type == "Bot"` REST filter — type-aware rather than name-aware, so any future GitHub-app reviewer (Graphite, Qodo, CodeRabbit, or any new bot) is picked up automatically without enumerating logins. `has_dev_reply` and `has_disposition` are deliberately separate fields. A thread with a reply but no marker is the most common near-miss — the developer engaged, the decision exists in their head and often in the prose, but nothing machine-checkable records *which* of the four outcomes was chosen. `has_dev_reply: true, has_disposition: false` is a blocking row that needs a marker appended, not a new argument; it is usually one comment away from clear.

The `comments(first: 100)` window covers virtually all real-world thread depths (most bot threads are 1–3 comments; a window of 100 makes the `has_dev_reply` false-negative case essentially impossible without GitHub API misuse). If a thread genuinely exceeds 100 comments, manual inspection is the right escalation.

### Step 4.2 — Categorize threads

Categorize on **the disposition**, then on the resolve flag — in that order.

| Condition | Action |
|---|---|
| `has_disposition: true` AND `is_resolved: true` | **Clear.** A decision is on record and the thread is closed. The resolver's identity does not matter — a reviewer bot tidying its own thread after a developer answered it is normal housekeeping. |
| `has_disposition: true` AND `is_resolved: false` | **Auto-resolvable** — the decision is recorded, only the resolve flag is missing. Surface for batch-resolve. |
| `has_disposition: false` | **Blocking** — no decision on record, whatever the resolve flag says and whoever set it. This is the whole gate. A reply without a marker, a 👍, and a bot's self-resolve are all this row. |
| Any row where the disposition is `deferred` | **Blocking until the issue is verified** to exist (`gh issue view`) and to be an issue rather than a PR. §2.5.4 already requires it to pre-exist; this confirms it actually does. |
| **Additionally:** a Phase 1.7 footprint action whose disposition ≠ `Verified-correct` | **Blocking** — independent of this table. An autofix agent's *code change* needs re-review on its own merits; §6.0 precondition 9 owns that. A `cv-disposition` marker on the thread does not discharge it. |

**A marker is only a marker when an authorized human wrote it.** The check reads markers from non-bot authors with an `OWNER`/`MEMBER` association only, so a bot quoting a reply back into the thread, an autofix agent emitting the format itself, or a drive-by account leaves the thread reading as undecided. That is deliberate: the artifact being verified is a *human decision*, and an agent that can mint its own dispositions verifies nothing.

### Step 4.3 — Report and gate

**Defer to the script's exit code.** Step 4.1's query is shown above so a reader can see what the gate reads; it is not a second, hand-run procedure with its own criteria. Everything below branches on the exit code and nothing else. **A bot-resolved row is not automatically safe and not automatically blocking** — the marker decides, per Step 4.2.

- **Exit 0** → print `✅ All review conversations carry a decision. Safe to proceed to merge.`
- **Exit 1** → the blocking list is printed, one line per gap. **Refuse to greenlight merge** and clear them:
  - *no disposition* → reply with reasoning plus a `cv-disposition` marker (Post-Report).
  - *replied but no marker* → append the marker; the reasoning is already there.
  - *unfiled deferral* → file the issue, or fix in-PR. A promise is not a deferral.
  - *reconciliation gaps* → update the Phase 3.5 post in place; never add a second.
  - *autofix action still UNVERIFIED* → land the Phase 1.7 Fix. A `cv-disposition` marker does NOT discharge §6.0 precondition 9.

  Then run `/enhanced-pr-review $0` again after the fixes land, or escalate to the developer to handle out-of-band.
- **Exit 2** → UNKNOWN. Re-run; never read it as clean.

`/dev-lifecycle` Phase 9 treats the Step 4.3 exit status as a hard gate — do not proceed to `gh pr merge` if Phase 4 reports blocking threads.

Threads that carry a disposition but were never marked resolved on GitHub are the one auto-resolvable class. Offer batch-resolve:

```bash
# Pipe the Step 4.1 output (saved to a variable or file) through jq to extract thread IDs:
RESOLVABLE_IDS=$(<Step 4.1 jq output> | jq -r '.[] | select(.has_disposition == true and .is_resolved == false) | .thread_id')

# Per thread (resolveReviewThread takes a thread ID, not a number):
echo "$RESOLVABLE_IDS" | while read -r THREAD_ID; do
  [ -z "$THREAD_ID" ] && continue
  gh api graphql -f query='mutation($id: ID!) { resolveReviewThread(input: {threadId: $id}) { thread { isResolved } } }' -F id="${THREAD_ID}"
done
```

The `thread_id` field in the Step 4.1 output (the top-level `id` selector on `reviewThreads.nodes`) is the GraphQL ID required by `resolveReviewThread.threadId` — it is NOT the integer thread number visible in PR URLs. Resolve only threads whose disposition is recorded — resolving one without a decision is precisely the silent close this gate exists to catch.

---

## Phase 5 — Branch protection discipline (when bot reviews block merge)

When a bot review (a code-reviewing bot's `CHANGES_REQUESTED`, a Quality-Gate failure, a release-signal `BLOCKING` when `.release_signal` is configured, a subagent BLOCK severity, etc.) causes the PR's `mergeStateStatus` to transition to `BLOCKED`, the block is intentional infrastructure. It is not noise to bypass.

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

1. **No open `BLOCK`-severity rows** in the Phase 3 table (a subagent BLOCK such as a security-reviewer BLOCK or a PROVEN red-team finding, release-signal BLOCKING per §2.6 when `.release_signal` is configured, any other subagent blocking-merge finding).
2. **No blocking bot's most-recent verdict is `⚠ Changes Suggested` / `❌ Blocked` / `CHANGES_REQUESTED`** — for every bot marked `"blocking": true` in `.bots[]`, its latest verdict must be `✅ Approved` / passing, OR the finding was fixed AND the bot re-reviewed the fix commit (re-fetch Phase 1 Step 4; do not trust a stale verdict).
3. **Every blocking Quality-Gate / SAST check = passed.** A `PENDING` / queued / not-yet-reported check is **NOT** a pass. Wait for the result; never approve ahead of a gate that has not reported.
4. **All required CI checks green** — `gh pr checks $0` shows zero `fail` and zero `pending` in the required set. **Pending ≠ green.** Report the actual state, never the assumed-imminent one.
5. **(Only when `.release_signal` is configured)** the release signal = ALL CLEAR or SATISFIED, or an explicitly-approved defer per §2.6.4. When `.release_signal` is absent, this precondition does not exist — skip it.
6. **The disposition ledger is complete** — the disposition gate, run from the default branch per §3.5.3, exits 0 at the current HEAD: every in-scope conversation carries a decision, every deferral names an issue that exists, and exactly one Phase 3.5 reconciliation post accounts for every reviewer that spoke. Re-run it here if the Phase 4 result isn't fresh; when Phase 6 is invoked directly rather than via `/dev-lifecycle` Step 9.1.5 there is no prior run to consume, so run it. **Exit 2 fails this precondition** — an unverifiable ledger is not a clean one.
7. **Every `Fix`-marked finding is actually resolved** — verify by reading `gh pr diff $0` / the fix-commit SHAs, not by asserting it. A finding you intended to fix but didn't push is still open.
8. **HEAD is the commit being approved** — no unpushed local fixes. The artifact references the current HEAD SHA. If the active ruleset dismisses stale reviews on push (the common configuration), **any later push dismisses the approval** — so Phase 6 is genuinely the last step before merge. If a fix still needs to land, you are not in Phase 6 yet.
9. **No UNVERIFIED autonomous-agent (autofix) action remains** (Phase 1.7). Every footprint action carries a disposition; every non-`Verified-correct`, non-`No-finding` disposition has a landed Fix commit (verify by diff per precondition 7). This precondition is about the autofix agent's **change**, and precondition 6 is about the **decision record**; they are independent and both must hold. A `cv-disposition` marker on a thread does not discharge this one — an agent's unreviewed commit needs re-review on its merits no matter how well the thread was answered. A manufactured green — the agent resolved a thread it did not fix, a reviewer bot re-approved a cosmetic change, or a failing test was deleted/skipped to green CI — does NOT satisfy this gate. If any footprint action is still `UNVERIFIED`, this precondition FAILS. A **`No-finding`** disposition (a benign non-autofix commit that §1.7.2 tied to no finding and no thread) is informational and does NOT block. When Phase 1.7 reported `Autofix footprint: none detected`, this precondition is trivially satisfied.

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
| Specialist subagents (per shared dispatch table + `.review_agents`) | ✅ clean / ⚠ N | <agents that ran; finding summary> |
| <one row per bot from `.bots[]` that posted — use its `bot` display name> | ✅ / ⚠ / ❌ / N comments | <verdict or finding count> |
| CI checks | ✅ N/N required green | |
| Release signal *(only if `.release_signal` configured)* | ALL CLEAR / SATISFIED / deferred (#NNN) | |
| Bot review threads | ✅ 0 unresolved | |
| Disposition ledger | ✅ N/N decided | N accepted · N rejected · N deferred · 0 undecided |

**Decisions:** recorded in the [review reconciliation post](<permalink to the Phase 3.5 comment>) — N findings, 0 undecided.

The per-finding decisions are NOT restated here. They live in exactly one place, the Phase 3.5 post, which is edited in place as the review progresses; a second copy in this artifact would be a snapshot taken at a different moment and would start disagreeing with the ledger the first time a decision changed.

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
- Does not replace the specialist-agent suite — it dispatches that suite (per the shared dispatch table and the `review-pr` skill's `.review_agents` procedure) and adds bot reconciliation on top
- Does not distinguish between a bot's `NEUTRAL` check-run state and its inline comments: a bot can post a `NEUTRAL` check (the summary declining to judge the PR) while still posting individual BUG_PREDICTION-style inline comments. Always check both the `reviews` endpoint AND the `comments` endpoint.
- Does not auto-resolve threads that lack a recorded disposition — silent auto-resolve would erase reviewer signal nobody decided on. A reply alone is not a disposition; the `cv-disposition` marker is
- Does not trust an autonomous autofix agent's commits, thread replies, or thread resolutions as verified. Phase 1.7 re-reviews each action against the original finding — reconstructing the finding if the agent erased it by re-approving itself — and routes every non-`Verified-correct` disposition to the Fix path. It does NOT autonomously revert or rewrite the agent's commits; corrections and reverts are Fix rows the developer confirms (preserving the no-auto-fix invariant above)
- Does not count any review thread toward the Phase 4 or Phase 6 gate on the strength of the `isResolved` flag — the flag records that a conversation was closed, never what was decided. A **human-authored `cv-disposition` marker** is what clears a thread, and it clears it regardless of who set the flag. Two consequences, both deliberate: a reviewer bot self-resolving a thread the developer answered is fine, and a *human* clicking Resolve without replying is not. A green CI check, a re-approval, and a resolved flag can all be manufactured; a marker written by a non-bot member cannot be manufactured by a bot
- Does not treat a disposition marker as discharging Phase 1.7. An autofix agent's code change still needs its own `Verified-correct` re-review under §6.0 precondition 9 — the two gates ask different questions ("was a decision recorded?" vs "was the unreviewed change actually correct?") and neither answers the other
- Does not let a `deferred` decision stand on a promise — the named issue must exist at merge time (`gh issue view`) and be an issue, not a PR, or the finding routes back to Fix-in-PR
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
2. Add a `.bots[]` entry (schema in `config/repo-config.reference.md`):
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
