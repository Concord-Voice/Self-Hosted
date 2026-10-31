---
name: dev-lifecycle
description: Orchestrate the full development lifecycle from issue understanding through merge-ready. Semi-automated with checkpoints at visibility boundaries. Re-entrant — detects current phase from repo/PR state. Supports parent issues with child tracking.
argument-hint: "[issue-number] [--approach=inline|subagent|hybrid] [--mode=overnight|weekly-deps|trusted|careful|hand-held]"
allowed-tools:
  - Bash(gh issue *)
  - Bash(gh pr *)
  - Bash(gh api *)
  - Bash(gh run *)
  - Bash(git branch *)
  - Bash(git log *)
  - Bash(git status *)
  - Read
  - Grep
  - Glob
---

<DISPATCH-GATE>
When a phase says "Run `/skill-name`", you MUST invoke it via the Skill tool. Do NOT substitute with inline commands, manual agents, or partial implementations. The sub-skill owns the execution; the orchestrator owns the sequencing. This is not optional.
</DISPATCH-GATE>

# Dev Lifecycle

> **Structure is generic; the repo-specific knobs are data.** The 9-phase flow, the mode-to-gate matrix, the dispatch gate, and the never-`--admin`-bypass discipline are fixed. Everything that varies per repo — the default branch, artifact directories, the bot roster waited on at Phase 7.5, the reviewer roster at Phase 8, the quality gate, parent-epic linkage, version-bump, and branch-protection prose — is read from the OPTIONAL per-repo `.claude/repo-config.json` via the shared helper `scripts/repo-config.sh` (`rc_get '<jq-filter>' [default]`, `rc_has '<jq-filter>'`). With an absent file, or any absent/`null` key, the affected step **degrades gracefully** — it is skipped, never errored. The bot roster in particular lives in `.bots[]` as the **single source of truth** shared with `/enhanced-pr-review`, so the two can never drift on which bots are online/offline or blocking. See `knowledge/repo-config.reference.md` for the full schema.
>
> Load the helper once at the top of any phase that reads config, and resolve the default branch up front (used throughout in place of a hard-coded `main`):
>
> ```bash
> source "$(git rev-parse --show-toplevel)/scripts/repo-config.sh"   # path where the org tooling is installed/symlinked
> DEFAULT_BRANCH="$(rc_get '.default_branch' "$(gh repo view --json defaultBranchRef --jq '.defaultBranchRef.name' 2>/dev/null || echo main)")"
> SPEC_DIR="$(rc_get '.docs.spec_dir' 'docs/superpowers/specs/')"
> PLAN_DIR="$(rc_get '.docs.plan_dir' 'docs/superpowers/plans/')"
> ```

Orchestrate the full development lifecycle for issue #$0.

---

## Choosing the right workflow

`/dev-lifecycle` is the right entry point for **features, refactors, and well-scoped changes** where the work is "build the thing the issue describes." For bug reports — where the work is "find why this broken behavior happens and fix it" — invoke `/reproduce-then-fix $0` instead. That skill enforces a failing-test-first ordering with a bounded retry budget, which is the wrong shape for feature work but the right shape for debugging. The bug-fix skill produces its own draft PR and then hands back to `/dev-lifecycle` from Phase 6 (CI MONITOR) onward.

Quick triage: if the issue describes an expected behavior that doesn't happen, or a wrong/error/unexpected outcome → `/reproduce-then-fix`. If it describes new capability or restructuring → `/dev-lifecycle`. When unsure, ask the developer.

---

## Step 1: Detect Mode — Single Issue or Parent

> These commands are for **mode detection only** — determining whether the issue has children. Do NOT use these results as a substitute for `/issue-brief` in Phase 1.

Fetch the issue and check for child issues:

```bash
gh issue view $0 --json body,title,state,labels --jq '{title: .title, state: .state, labels: [.labels[].name], body: .body}'
```

Check for children: native sub-issues first (REST `/sub_issues`, per `.claude/rules/release-tracking-epics.md`), then legacy checkbox references only when there are none:

```bash
CHILDREN=$(gh api "repos/{owner}/{repo}/issues/$0/sub_issues?per_page=100" --jq '.[].number')
[ -n "$CHILDREN" ] || CHILDREN=$(gh issue view $0 --json body --jq '.body' | grep -oE '\- \[[ x]\] #[0-9]+' | grep -oE '[0-9]+')
```

**If children found** → go to Parent Issue Mode (below).
**If no children** → go to Single Issue Mode (Phase Detection).

---

## Parent Issue Mode

When the issue has child issues, show a progress dashboard.

For each child issue number, fetch its state and look for associated branches/PRs:

```bash
# For each child:
gh issue view ${CHILD} --json number,title,state --jq '"\(.number) | \(.title) | \(.state)"'
gh pr list --search "head:feature/${CHILD}" --json number,title,state,isDraft,merged --jq '.[0]'
```

### Dashboard Format

```
/dev-lifecycle #$0 — Parent Issue (N children)
────────────────────────────────────────────────
| # | Child | Title | Phase | PR | Status |
|---|-------|-------|-------|-----|--------|

[For each child, determine phase using the same State Detection logic below]

Next: [first incomplete child] — [current phase description]
────────────────────────────────────────────────
```

Status values: `Not started`, `In progress (Phase N)`, `PR open`, `Merged`

After showing the dashboard, ask: "Which child to work on? [number or 'next']"

When a child is selected, drop into Single Issue Mode for that child. After the child's PR is merged, return to the parent dashboard.

---

## Single Issue Mode — Phase Detection

Detect the current phase by checking artifacts in order. The first matching condition determines the phase:

### Detection sequence

Run these checks in order. Use the Glob tool (not `ls`) for file discovery, and gate each step on the previous result.

**Step 1: Resolve repo identity**
```bash
REPO_FULL=$(gh repo view --json nameWithOwner --jq '.nameWithOwner')
OWNER=$(echo $REPO_FULL | cut -d/ -f1)
REPO=$(echo $REPO_FULL | cut -d/ -f2)
```

**Step 2: Does a feature branch exist?**
```bash
BRANCH=$(git branch --list "feature/$0-*" --format='%(refname:short)' | head -1)
# Also check remote if no local branch (fresh clone / pruned locals)
if [ -z "$BRANCH" ]; then
  REMOTE=$(git branch -r --list "origin/feature/$0-*" --format='%(refname:short)' | head -1)
  # Strip origin/ prefix for use in gh commands (gh pr list --head expects bare branch name)
  BRANCH="${REMOTE#origin/}"
fi
```
If `BRANCH` is empty → **Phase 1**. Stop detection here.

**Step 3: Does a spec exist?**
Use Glob to search for `${SPEC_DIR}*-$0-*` (where `SPEC_DIR` is `.docs.spec_dir`, default `docs/superpowers/specs/`). The naming convention includes the issue number (e.g., `2026-04-10-598-<short-topic>-design.md`), so matching by `*-$0-*` is deterministic.

If no match, also check for legacy specs (pre-convention) by reading headers of files in `${SPEC_DIR}` for an `Issue: #$0` reference. If neither matches → no spec exists.

**Step 4: Does a plan exist?**
Same approach as Step 3, searching `${PLAN_DIR}*-$0-*` (where `PLAN_DIR` is `.docs.plan_dir`, default `docs/superpowers/plans/`). Fall back to header scan for legacy plans without issue numbers.

**Step 5: Does a PR exist?**
Only run if BRANCH is non-empty:
```bash
PR_NUMBER=$(gh pr list --head "${BRANCH}" --json number --jq '.[0].number' 2>/dev/null)
PR_DRAFT=$(gh pr list --head "${BRANCH}" --json isDraft --jq '.[0].isDraft' 2>/dev/null)
```
If `PR_NUMBER` is empty → no PR exists.

**Step 6: CI status?**
Only run if PR_NUMBER is non-empty:
```bash
CI=$(gh pr checks ${PR_NUMBER} --json bucket --jq '[.[].bucket] | if all(. == "pass" or . == "skipping") then "green" elif any(. == "fail") then "red" else "pending" end' 2>/dev/null)
```

**Step 7: Bot review present?**
Only run if PR_NUMBER is non-empty and PR is not a draft. Detect whether ANY GitHub-app reviewer has posted, structurally via `user.type == "Bot"` (same filter `/enhanced-pr-review` uses — no hard-coded login list):
```bash
BOT_REVIEWS=$(gh api repos/${OWNER}/${REPO}/pulls/${PR_NUMBER}/reviews --jq '[.[] | select(.user.type == "Bot")] | length' 2>/dev/null)
```

### Phase mapping

| Condition | Phase |
|---|---|
| No branch | **Phase 1: UNDERSTAND** |
| Branch, no spec (`*-$0-*` in `SPEC_DIR`) | **Phase 2: PLAN** (brainstorming needed) |
| Branch + spec, no plan (`*-$0-*` in `PLAN_DIR`) | **Phase 2: PLAN** (writing-plans needed) |
| Branch + plan, no implementation commits | **Phase 3: EXECUTE** |
| Branch + plan + implementation commits, no PR | **Phase 4: VALIDATE** |
| Draft PR, CI not green | **Phase 6: CI MONITOR** |
| Draft PR, CI green | **Phase 7: READY** |
| PR ready (not draft), no bot review yet | **Phase 7.5: WAIT FOR BOT REVIEWS** |
| PR ready (not draft), bot-reviewed | **Phase 8: FINAL REVIEW** |
| PR ready, all bot review threads resolved (per the Phase 9 GraphQL `reviewThreads` check) | **Phase 9: MERGE-READY** |

To distinguish Phase 3 from Phase 4, check for implementation commits beyond spec/plan (excluding both artifact dirs on the default branch):

```bash
IMPL_COMMITS=$(git log "${DEFAULT_BRANCH}..${BRANCH}" --oneline -- ":!${SPEC_DIR}" ":!${PLAN_DIR}" | wc -l | tr -d ' ')
```

If `IMPL_COMMITS` > 0 → Phase 4. If `IMPL_COMMITS` = 0 → Phase 3.

Report detected phase:

```
/dev-lifecycle #$0 — Phase N: [PHASE NAME]
[1 ✅] [2 ✅] [3 ✅] [4 ○] [5 ○] [6 ○] [7 ○] [8 ○] [9 ○]

Detected state: [what was found]
Starting from Phase N.
```

---

## Catalog Dispatch (consulted at every phase)

`/issue-creation`, `/pr-creation`, `/dev-lifecycle`, and `/enhanced-pr-review` MUST consult [`.claude/skills/_shared/dispatchable-skills.md`](../_shared/dispatchable-skills.md) at each phase and run its "## Dynamic skill scoping (mandatory)" algorithm. Dispatch of a matched skill is mandatory; the Task Handling mode controls only the *timing of the gate*, not whether the skill runs.

### Dispatch algorithm

At each phase entry, AFTER the Mode-Aware Gate check passes:

1. **Load** the catalog file. Locate the section corresponding to the current phase (e.g., `## Phase 2: PLAN`).
2. **Always-dispatch entries:** invoke each via the `Skill` tool, in catalog order. Subject only to the mode authorization (e.g., a Careful-mode user at Phase 2 sees the prompt for each dispatch).
3. **Candidate entries:** for each, evaluate the entry's `**Trigger:**` heuristic against the context signature (issue body / PR body / labels / changed files).
   - If trigger matches AND mode authorizes auto-dispatch (per the matrix): invoke via the `Skill` tool. Dispatch is **mandatory** — a matched candidate is never silently skipped.
   - If trigger matches AND mode requires gating: surface "`<skill>` trigger matched — invoke? [yes / skip]". The skill remains on the required set; the gate controls timing, not whether it runs.
   - If trigger does NOT match: skip (no dispatch — not a withhold).
   - If a trigger matches but the skill is intentionally not dispatched (gate pending, billed-awaiting-approval): log it as `withheld: <skill> — <reason>`. A match is never silently dropped.
   - Emit the visibility line `Scoped skills (Phase N): [...]` so dispatch is auditable.
4. **Billed / cloud-cost entries** (`billed` tag): never auto-invoke in any mode — surface the `**Recommendation text:**` and require explicit user preapproval. `user-triggered` billed skills (e.g., `/code-review ultra`) Claude cannot launch at all.

### Trigger heuristic evaluation

The catalog's per-skill triggers are populated. Evaluate them as case-insensitive substring or regex matches against:

- Issue body (`gh issue view $0 --json body --jq .body`)
- Issue labels (`gh issue view $0 --json labels --jq '[.labels[].name]'`)
- PR body (when running at Phase 6+, after the PR exists)

For Phase 1 (UNDERSTAND), trigger evaluation runs against the just-fetched issue brief. For Phase 8 (FINAL REVIEW), trigger evaluation runs against the PR title/body + the diff summary.

### Dispatch resilience

If a catalog entry references a skill that does not exist in the current plugin inventory, log a WARN (one line: `WARN: catalog references unknown skill /<name>; skipping`) and continue. Do not abort the phase. The catalog is a single source of truth across plugin loads — entries may temporarily reference skills present in some loads but not others.

### Catalog-dispatch reference at each phase

Each phase below has a "**Catalog Dispatch:**" line indicating which catalog section to consult. Always-dispatch entries from that section are listed inline as a sanity-check; if the inline list drifts from the catalog, the catalog wins.

---

## Phase 1: UNDERSTAND (auto-run)

Run `/issue-brief $0` via the Skill tool. This gathers the complete issue context: title, body, all comments, labels, milestone, linked issues/PRs, subissues, and cross-references.

Do NOT substitute with inline `gh issue view` commands — `/issue-brief` is the mandatory path for gathering issue context.

Display the brief, then run the parent-epic linkage check below (if the repo enables it), then proceed to Phase 2.

**Catalog Dispatch:** consult `## Phase 1: UNDERSTAND` in [`dispatchable-skills.md`](../_shared/dispatchable-skills.md). Always-dispatch: `/issue-brief`. Candidates: `/engineering:architecture`, `/operations:vendor-review` — evaluate triggers against the issue body.

### Parent-epic linkage check (config-gated)

**This check runs ONLY when the repo opts in** — i.e. `rc_has '.epic_linkage'` is true AND `.epic_linkage.enabled` is true. Repos that don't track release work as version-epics skip this step entirely (no WARN, no prompt). Read the knobs:

```bash
rc_has '.epic_linkage' && [ "$(rc_get '.epic_linkage.enabled' false)" = "true" ] || echo "epic-linkage disabled — skip"
EPIC_TITLE_PATTERN="$(rc_get '.epic_linkage.epic_title_pattern' 'Epic: v{version} release')"   # {version} is filled from the milestone
EPIC_LABEL="$(rc_get '.epic_linkage.epic_label' 'type: epic')"
```

When enabled, identify which version-epic the issue should be attached to:

1. Read the issue's milestone. The version-epic title is `EPIC_TITLE_PATTERN` with `{version}` substituted from the milestone (e.g., pattern `Epic: v{version} release` + milestone `1.2.3` → `Epic: v1.2.3 release`).
2. Search for the open epic: `gh issue list --label "$EPIC_LABEL" --search "<rendered-title>"`.
3. If a matching epic exists AND the current issue is not already attached: surface this to the user before starting work, and propose attaching via REST `/sub_issues` (never `- [ ] #N` tasklist).
4. If NO matching epic exists for the target milestone: **WARN** and offer to file the epic. Do NOT hard-block. The user may either (a) file the epic first via `/issue-creation` then continue, or (b) confirm "no parent" and proceed (e.g., during a rollout transition before all in-flight milestones have their epics filed, or for pure-chore work outside any release).

This surfaces the "child filed before parent" failure mode without blocking forward progress. The WARN posture (vs STOP) supports the rollout period: when this discipline first lands, not all active milestones have their version epics yet, and hard-blocking would create an immediate ops blocker.

## Phase 1.5: AUTONOMY CONTRACT ⚠️ CHECKPOINT (interactive intake)

Phase 1.5 runs IMMEDIATELY after Phase 1 (UNDERSTAND) completes and the feature branch is created at the start of Phase 2, BEFORE Phase 2's spec/plan work starts. It asks two questions that determine how the rest of the lifecycle behaves.

### Skipping Phase 1.5 via flags

Both flags from the `argument-hint` are optional:

```bash
/dev-lifecycle <issue-number> --approach=<inline|subagent|hybrid> --mode=<overnight|weekly-deps|trusted|careful|hand-held>
```

- If **both** flags are passed, Phase 1.5 is skipped entirely and the selected approach+mode apply.
- If only **one** flag is passed, the other question is asked interactively.
- If **neither** flag is passed (the default), both questions are asked.

### Question 1: Implementation Approach

| Option | When to recommend |
|---|---|
| `Inline` | Sequential work with shallow context. Bug fix, single-file change, simple refactor. Lower token cost. |
| `Subagent Driven` | Parallelizable work, multiple independent tasks, deep context per task. Higher token cost, better isolation. |
| `Hybrid` (recommended default for nontrivial scope) | Mix: subagents for parallelizable / context-heavy work, inline for sequential follow-ups. |

Default recommendation logic:
- Issue has ≥3 sub-tasks (checkbox list or sub-issue tree) → **Hybrid**
- Issue is a single task or simple refactor → **Inline**
- Issue has 4+ orthogonal sub-issues OR explicit parallelism requested → **Subagent Driven**

### Question 2: Task Handling Mode

| Mode | Description | Gates |
|---|---|---|
| `Hand-Held` | Approvals for ALL phases (except Phase 1 UNDERSTAND, which is read-only). | 1.5, 2, 3, 4, 5, 6, 7, 8, 9 |
| `Careful` | Plan/spec building requires dev sign-off; gates at draft PR push; phase-by-phase approval through PR cycle. | 2, 5, 6, 7, 8, 9 |
| `Trusted` | Dev on-the-loop; start-to-draft auto, gate at PR push; draft-to-ready auto, gate at merge. | 5, 9 |
| `Weekly Deps` | Dependabot/github-actions PRs; auto end-to-end unless flagged. | 9 + flag-conditional 5, 7, 8 |
| `Overnight` | Small issues; autonomous to Phase 8 ready state; gate only at merge. | 9 |

### Mode-to-gate matrix (canonical)

| Phase | Hand-Held | Careful | Trusted | Weekly Deps | Overnight |
|---|---|---|---|---|---|
| 1 UNDERSTAND | Auto | Auto | Auto | Auto | Auto |
| 2 PLAN | **Gate** | **Gate** | Auto | Auto | Auto |
| 3 EXECUTE | **Gate** | Auto | Auto | Auto | Auto |
| 4 VALIDATE | **Gate** | Auto | Auto | Auto | Auto |
| 5 COMMIT+DRAFT PR | **Gate** | **Gate** | **Gate** | Auto* | Auto |
| 6 CI MONITOR | **Gate** | **Gate** | Auto | Auto | Auto |
| 7 READY | **Gate** | **Gate** | Auto | Auto* | Auto |
| 7.5 WAIT FOR BOT REVIEWS | Auto† | Auto† | Auto† | Auto† | Auto† |
| 8 FINAL REVIEW | **Gate** | **Gate** | Auto | Auto* | Auto |
| 9 MERGE-READY | **Gate** | **Gate** | **Gate** | **Gate** | **Gate** |

`Auto*` = autonomous unless content-flagged (e.g., a Tier C dependabot PR with surface-area beyond expected footprint triggers a flag).

`Auto†` = autonomous wait; not a gate, but Phase 7.5 STOPs and flags to the developer if any expected bot review is missing after the timeout (10 min default). The STOP-on-anomaly condition is mode-independent — see Phase 7.5 for details.

**Phase 9 always gates.** Merge always requires explicit user approval regardless of selected mode. This is the universal floor.

### Mode recommendation heuristics

When the developer is undecided, suggest a mode based on issue characteristics. These are seeded suggestions; the developer can override. The map below is a **minimal generic default** keyed off conventional label namespaces (`type:`, `area:`, `domain:`, `risk:`) and authorship; a repo extends it by reading its own labels at runtime (the live label set from `gh label list` / the issue's labels), not by hard-coding values here.

| Issue characteristic | Recommended mode |
|---|---|
| Bug fix with clear repro path (use `/reproduce-then-fix` instead — but if entered via `/dev-lifecycle`) | `Careful` |
| Bot author (Dependabot, github-actions, or similar) | `Weekly Deps` |

> **Retired 2026-07-24 — the security-label mode recommendation.** A row here previously pinned `risk: security` / `risk: privacy` / security-sensitive `domain:` issues to the most-gated mode. It is removed. It was **suggestion-only** — the developer confirmed the mode at Phase 1.5 either way — so in practice it warned about a choice already made deliberately and was routinely overridden. A nudge that is always dismissed is not a safety control; it trains people to click through prompts, which is worse than no prompt. **Nothing about how security work is reviewed changes:** the specialist reviewer suite still dispatches on the same labels and file globs, Phase 7.5 still waits on required reviewers, and Phase 9 still gates every merge. The row governed *how often the developer was interrupted*, never *what got reviewed*.
| Typo / doc-only fix / lint cleanup (label `type: chore` combined with a docs `area:` label) | `Trusted` or `Overnight` |
| Multi-day epic intake (parent issue with children) | `Careful` for parent; per-child mode selection on child intake |
| Frontend polish without server changes (a frontend `area:` label only, combined with `type: enhancement`) | `Trusted` |
| `.claude/` tooling change (a tooling `area:` label) | `Careful` |

### Intake prompt

After Phase 1's parent-epic linkage check completes and the feature branch exists, present:

```
Phase 1.5: AUTONOMY CONTRACT
[1 ✅] [1.5 ◀] [2 ○] [3 ○] [4 ○] [5 ○] [6 ○] [7 ○] [8 ○] [9 ○]

Issue #<N> characteristics: <labels-summary>
Suggested approach: <Inline | Subagent Driven | Hybrid>
Suggested mode: <Hand-Held | Careful | Trusted | Weekly Deps | Overnight>

Implementation Approach? [inline / subagent / hybrid]
Task Handling Mode? [hand-held / careful / trusted / weekly-deps / overnight]
```

Use the `AskUserQuestion` tool with two questions (one per dimension). Persist the chosen approach + mode in the session — they're consulted at every subsequent phase.

### Recorded mode persists across re-invocations of `/dev-lifecycle`

When the developer re-invokes `/dev-lifecycle <N>` mid-lifecycle (e.g., after CI completes), Phase Detection runs first. If a detected phase is > 1, look for a `.claude/state/dev-lifecycle-mode/<sanitized-branch>.txt` file containing the previously-selected approach + mode. If present, **always re-display the loaded mode + approach and require explicit confirmation before honoring it** — do not silently re-apply. If absent (first re-entry or stale state), re-ask Phase 1.5 questions and persist.

**Re-entry confirmation prompt (mandatory):**

```
Loaded prior /dev-lifecycle state for this branch:
  approach=<value>
  mode=<value>

Continue in this mode? [yes / re-ask / abort]
```

`yes` proceeds with the loaded values. `re-ask` triggers full Phase 1.5 re-prompt (existing file is overwritten on confirmation of new values). `abort` exits without progressing.

**Why mandatory:** the state file is in the worktree (not gitignored only because `.claude/state/` falls under the umbrella `.claude/*` rule with no carve-out). A planted file (e.g., from a compromised dev tool, a malicious commit, or even a stale file from an abandoned-then-rewritten branch) containing `mode=overnight` would silently elevate authorization from a re-asked Careful default to Overnight (no gate until Phase 9), bypassing 7 of 8 phase gates. The re-confirmation prompt adds a single keystroke to the legitimate flow and a hard stop to silent escalation.

Mode-state file format (one `key=value` per line):

```
approach=hybrid
mode=careful
```

The state file is best-effort persistence; if it's missing or corrupt, fall through to interactive re-ask. The re-confirmation prompt above is independent of corruption — even a syntactically valid file requires confirmation.

---

## Phase 2: PLAN (auto-run)

**Mode-Aware Gate (per Phase 1.5):** Gated in `Hand-Held` and `Careful` modes. In `Trusted`, `Weekly Deps`, and `Overnight` modes, this phase auto-proceeds without prompt. If gated, present:

> "Spec and plan generation will run next. Proceed? [yes / pause / abort]"

**Catalog Dispatch:** consult `## Phase 2: PLAN` in [`dispatchable-skills.md`](../_shared/dispatchable-skills.md). Always-dispatch: `/superpowers:brainstorming` (if no spec exists), `/superpowers:writing-plans` (if spec exists). Candidates: `/product-management:write-spec`, `/engineering:system-design`, `/product-management:product-brainstorming`, `/frontend-design:frontend-design`, `/design:design-handoff`, `/design:ux-copy`, `/design:accessibility-review`, `/engineering:testing-strategy`, `/engineering:tech-debt`.

**Step 1 — Create or switch to feature branch (unconditional, before anything else):**

```bash
# Check if a matching local branch already exists
BRANCH=$(git branch --list "feature/$0-*" --format='%(refname:short)' | head -1)

if [ -n "$BRANCH" ]; then
  # Re-entry: switch to existing local branch
  git checkout "$BRANCH"
else
  # Check for a matching remote branch (e.g., fresh clone or pruned locals)
  REMOTE_BRANCH=$(git branch -r --list "origin/feature/$0-*" --format='%(refname:short)' | head -1)

  if [ -n "$REMOTE_BRANCH" ]; then
    # Re-entry: create local tracking branch from existing remote
    git checkout --track "$REMOTE_BRANCH"
  else
    # Create new branch. Topic is 2-4 significant words from issue title, lowercased, hyphenated.
    git checkout -b feature/$0-<topic>
  fi
fi
```

This ensures ALL subsequent commits (spec, plan, implementation) land on the feature branch, not the default branch.

**Step 2 — Spec and plan generation:**

If no spec exists (`${SPEC_DIR}*-$0-*`):
0. **Honor the Design Dossier.** If the issue brief (`/issue-brief`) contains a `## Design Dossier` section, feed it to brainstorming as the design basis — do NOT re-run the design-class skills it already captured. If there is NO dossier (issue filed outside `/issue-creation`), run the catalog's "## Dynamic skill scoping (mandatory)" algorithm now and dispatch matched `design-class` skills *before* brainstorming; brainstorming then synthesizes their output.
1. Invoke `/superpowers:brainstorming` via the Skill tool with the issue brief (and any dossier / design-class artifacts) as context.
2. **(Only when the repo enables a quality gate — `.quality_gate.enabled` is true.)** After brainstorming produces a spec, verify it includes a **mandatory acceptance criteria** section aligned with the repo's quality-gate definition (`.quality_gate.doc_path`, when set). If missing, add it before proceeding, filling thresholds from config:

   ```bash
   [ "$(rc_get '.quality_gate.enabled' false)" = "true" ] || { echo "quality gate disabled — using acceptance criteria from verification-before-completion only"; }
   COVERAGE_MIN="$(rc_get '.quality_gate.coverage_min' 80)"
   BLOCKERS_MAX="$(rc_get '.quality_gate.blockers_max' 0)"
   HOTSPOTS_MAX="$(rc_get '.quality_gate.hotspots_max' 0)"
   QG_DOC="$(rc_get '.quality_gate.doc_path')"   # optional; the authoritative gate definition when set
   ```

   | Gate Condition | Threshold |
   |---|---|
   | New blocker/critical issues | ≤ `BLOCKERS_MAX` |
   | New high-confidence security hotspots | ≤ `HOTSPOTS_MAX` |
   | New code coverage | ≥ `COVERAGE_MIN`% |

   When `.quality_gate.enabled` is false or absent, skip this acceptance-criteria injection entirely.
3. Invoke `/superpowers:writing-plans` via the Skill tool.

If spec exists but no plan (`${PLAN_DIR}*-$0-*`):
1. Invoke `/superpowers:writing-plans` via the Skill tool.

If both exist, skip to Phase 3.

**Naming convention:** Spec files use `YYYY-MM-DD-NNN-<short-topic>-design.md` in `SPEC_DIR`. Plan files use `YYYY-MM-DD-NNN-<short-topic>.md` in `PLAN_DIR` (matching the repo's existing pattern — no `-plan` suffix). NNN is the issue number, enabling deterministic detection via `*-$0-*` glob.

## Phase 3: EXECUTE (auto-run)

**Mode-Aware Gate (per Phase 1.5):** Gated in `Hand-Held` mode only. All other modes auto-proceed. If gated, present:

> "Execute the plan now? [yes / pause / abort]"

**Catalog Dispatch:** consult `## Phase 3: EXECUTE` in [`dispatchable-skills.md`](../_shared/dispatchable-skills.md). Always-dispatch: `/superpowers:subagent-driven-development` OR `/superpowers:executing-plans` depending on Implementation Approach selected at Phase 1.5. Candidates: `/superpowers:test-driven-development`, `/simplify`, `/frontend-design:frontend-design`.

Offer execution choice:

```
Phase 3: EXECUTE
Plan ready at: [plan path]

Execution approach:
1. Subagent-driven (recommended) — fresh agent per task, review between tasks
2. Inline — execute in this session with checkpoints

Which approach? [1/2]
```

Invoke the chosen skill. Hooks fire during execution.

## Phase 4: VALIDATE (auto-run)

**Mode-Aware Gate (per Phase 1.5):** Gated in `Hand-Held` mode only. All other modes auto-proceed. If gated, present:

> "Run verification and quality-gate checks? [yes / skip / abort]"

**Catalog Dispatch:** consult `## Phase 4: VALIDATE` in [`dispatchable-skills.md`](../_shared/dispatchable-skills.md). Always-dispatch: `/superpowers:verification-before-completion` (plus `/verify-quality-gate` only when the repo ships a quality gate — see below). Candidates: `/engineering:debug`, `/superpowers:systematic-debugging`, `/engineering:testing-strategy`.

```
Phase 4: VALIDATE
Running verification and quality gate checks...
```

Run `/superpowers:verification-before-completion` (always). Then, **only when `.quality_gate.enabled` is true AND a `/verify-quality-gate` skill exists in the current inventory**, dispatch `/verify-quality-gate` to predict the repo's code-quality gate locally before push. If the gate is disabled or the skill is absent, the verification-before-completion pass above is sufficient — do not abort (catalog dispatch resilience). The `/engineering:testing-strategy` cross-dispatch keys off the quality gate's coverage-miss signal when present.

**Docs-in-PR reminder:** confirm every doc named in the issue's "Docs to update" acceptance criterion is staged in this branch, and that no behavior changed by this PR has left a doc stale. Docs ship with the code that changed them — never a follow-up. The hard gate is `/enhanced-pr-review` §2.5.0 at Phase 8; this is the front-loaded reminder.

If failures: report and suggest fixes. Loop back to Phase 3 if code changes needed.
If clean: proceed to Phase 5.

## Phase 5: COMMIT + DRAFT PR ⚠️ CHECKPOINT

**Mode-Aware Gate (per Phase 1.5):** Gated in `Hand-Held`, `Careful`, `Trusted` modes. In `Weekly Deps`, gated only if a flag was raised in Phase 3 or 4 (otherwise auto). In `Overnight`, auto-proceeds without prompt.

```
Phase 5: COMMIT + DRAFT PR
[1 ✅] [2 ✅] [3 ✅] [4 ✅] [5 ◀] [6 ○] [7 ○] [8 ○] [9 ○]

All local checks passed. Ready to commit and create a draft PR?
This will push your branch and create a draft PR on GitHub.
[yes / no / I'll handle it manually]
```

If yes: invoke `/commit-push-pr --draft` (or guide through git add, commit, push, gh pr create).

After the PR exists (regardless of which path created it), invoke `/pr-creation <PR>` via the Skill tool to sync PR metadata with the underlying issue. This is preapproved per the [`dispatchable-skills.md`](../_shared/dispatchable-skills.md) catalog — no extra gate even in Careful mode, since `/pr-creation` is metadata-only.

```bash
# After gh pr create returns the PR number, capture it and invoke /pr-creation
PR_NUMBER=$(gh pr list --head "$BRANCH" --json number --jq '.[0].number')
# Then: invoke /pr-creation via the Skill tool with PR_NUMBER as $0
```

Verify `/pr-creation` reports clean (Phase 5 of `/pr-creation` is advisory only; WARN messages are non-blocking). Proceed to Phase 6.

If manual: print the commands and wait.

## Phase 6: CI MONITOR (auto-run)

**Mode-Aware Gate (per Phase 1.5):** Gated in `Hand-Held` and `Careful` modes (developer wants to see CI result before next action). In `Trusted`, `Weekly Deps`, `Overnight` — auto-proceed once CI is green or auto-remediate if failing per existing logic.

**Catalog Dispatch:** consult `## Phase 6: CI MONITOR` in [`dispatchable-skills.md`](../_shared/dispatchable-skills.md). Always-dispatch: `/ci-status`. Candidates (on CI failure): `/engineering:debug`, `/superpowers:systematic-debugging`, `/reproduce-then-fix`.

Run `/ci-status <PR>` via the Skill tool.

- If pending: "CI still running. Run `/ci-status <PR>` again, or `/loop 2m /ci-status <PR>` to auto-poll."
- If failing: show remediation suggestions. After fixes, loop back to Phase 5 (commit fixes).
- **Version-bump recommendation (config-gated).** Only when `.version_bump.enabled` is true: if the repo's version-source check recommends a bump, present the recommendation to the developer and wait for explicit approval before bumping. Read the knobs first:
  ```bash
  [ "$(rc_get '.version_bump.enabled' false)" = "true" ] || echo "version-bump disabled — skip"
  SOURCE_CHECK="$(rc_get '.version_bump.source_check')"   # name of the CI check that signals a needed bump
  BUMP_FILE="$(rc_get '.version_bump.bump_file')"          # the version manifest to edit
  ```
  When the check named by `SOURCE_CHECK` recommends a bump, surface it and, on approval, bump the version in `BUMP_FILE`. When `.version_bump.enabled` is false or absent, this bullet does not apply.
- If green: proceed to Phase 7.

## Phase 7: READY FOR REVIEW ⚠️ CHECKPOINT

**Mode-Aware Gate (per Phase 1.5):** Gated in `Hand-Held` and `Careful` modes. In `Trusted` and `Overnight`, auto-marks ready. In `Weekly Deps`, gated only if a flag was raised earlier.

```
Phase 7: READY FOR REVIEW
[1 ✅] [2 ✅] [3 ✅] [4 ✅] [5 ✅] [6 ✅] [7 ◀] [8 ○] [9 ○]

CI is green. Mark PR #<N> as ready for review?
This makes the PR visible to reviewers.
[yes / no / not yet]
```

If yes:
```bash
gh pr ready <PR>
```

Then inform: "PR marked ready. Bot reviews may take a few minutes to start. Phase 7.5 will hold until expected bots have arrived. Run `/dev-lifecycle $0` again to resume."

## Phase 7.5: WAIT FOR BOT REVIEWS (auto-run, STOP-on-anomaly)

**Mode-Aware Gate (per Phase 1.5):** Auto in all modes — this phase waits, it doesn't mutate state. The STOP-on-anomaly condition (no expected bot arrived within timeout) is mode-independent and forces a flag to the developer regardless of selected mode.

After Phase 7 marks the PR ready, expected bot reviews START (they do NOT run on draft PRs). Phase 7.5 holds the lifecycle until all expected bots have either (a) posted their review, or (b) timed out with a flag.

**Why this phase exists.** Dispatching `/enhanced-pr-review` (Phase 8) before bot reviews arrive risks producing a "no findings — proceed to merge" reconciliation table that masks late-arriving real findings. Phase 7.5 is the structural fence between "PR is ready" and "all expected reviewers have spoken."

### Step 7.5.1 — Load the expected-bot set from config (single source of truth)

The expected bots are **data**, read from `.claude/repo-config.json` `.bots[]` — the SAME roster `/enhanced-pr-review` consumes. This is the single source of truth: there is no separate hand-maintained table here to drift from it, which structurally eliminates the online/offline mismatch that arises when two skills each keep their own bot list. Load the roster:

```bash
BOTS_JSON="$(rc_get '.bots' '[]')"   # JSON array; '[]' when unset → no bots expected, Phase 7.5 is a no-op wait
```

If `.bots == []` (or the file is absent), there is no bot to wait on — proceed straight to Phase 8.

Each `.bots[]` entry carries the fields this phase needs:
- `.name` — display name for messages.
- `.ci_check` — the bot's CI check-run name (may be unset for reviewers that post no CI check, e.g. an inline-comment-only reviewer).
- `.login` / `.match` / `.marker` — how to recognize the bot's conversation comment (exact login + body `marker` for shared-login posters like `github-actions[bot]`; a login-substring `match` for dedicated-login reviewers).
- `.offline` (default `false`) — a bot known to be down; **skip it** from the expected set (no STOP for an offline bot).
- `.when` — gating for whether this bot is expected on THIS PR: `always` (every PR), `active` (expected whenever the bot is online — i.e. only suppressed by `.offline:true`), or `shippable` (only when shippable code changed — see below).

When a bot entry omits `.when`, treat it as `always` (i.e. expected on every PR unless `.offline:true`).

Build the **expected set** = every `.bots[]` entry with `.offline != true` AND its `.when` condition satisfied for this PR.

**Determining "shippable code changed"** (for `.when == "shippable"` bots): check `gh pr diff <PR> --name-only` against the repo's shippable globs:

```bash
SHIPPABLE_GLOBS="$(rc_get '.release_signal.shippable_globs' '[]')"   # e.g. ["services/","src/"]
```

If none of the changed files match any glob, a `shippable`-gated bot is expected to SKIP (its CI job carries an internal filter) — treat skip as "review complete," not "still waiting." With no `shippable_globs` configured, treat `shippable` bots as not-expected on this PR.

### Step 7.5.2 — Poll for arrival

For each bot in the expected set, check two signals (drive both from its `.bots[]` fields — no literal bot names in the commands):

```bash
# CI check status for this bot (only if the entry has a .ci_check)
gh pr checks <PR> --json name,state,bucket | jq --arg c "<entry.ci_check>" '.[] | select(.name == $c) | {state, bucket}'

# Conversation comment from this bot: match a dedicated login by substring, OR a shared
# login (e.g. github-actions[bot]) disambiguated by the entry's body marker.
gh api repos/<owner>/<repo>/issues/<PR>/comments \
  --arg login "<entry.login>" --arg match "<entry.match>" --arg marker "<entry.marker>" \
  --jq '[ .[] | select(
       ($match != "" and (.user.login | ascii_downcase | contains($match | ascii_downcase)))
       or ($login != "" and .user.login == $login and $marker != "" and (.body | contains($marker)))
     ) ]'
```

A bot's review has **ARRIVED** when EITHER:
- Its CI check is in a terminal state (`success` / `failure` / `neutral` / `skipping`), OR
- Its conversation comment is posted

A bot is **IN-PROGRESS** when:
- Its CI check is `pending` / `in_progress` / `queued`

A bot is **MISSING** when:
- It has a `.ci_check` that does not exist in the check list (or has no `.ci_check`) AND no comment from the bot has been posted

### Step 7.5.3 — Decision

| State | Action |
|---|---|
| All expected bots ARRIVED | Proceed to Phase 8 |
| Any expected bot IN-PROGRESS | Wait + re-poll (30s interval recommended; or use `/loop 2m /ci-status <PR>` for hands-off polling) |
| After timeout (10 min default since `gh pr ready`), any expected bot still MISSING | **STOP and flag** |

**STOP message format:**

```
⚠ Phase 7.5: Bot review missing — cannot proceed to Phase 8

Expected reviewer `<bot.name>` has not started review on PR #<N>:
  - No CI check `<bot.ci_check>` found in the check list
  - No conversation comment from `<bot.login or bot.match>` posted

Possible causes:
  1. Bot not configured for this repo / org
  2. Bot is temporarily offline (set `"offline": true` on its `.bots[]` entry)
  3. Bot's filter excluded this PR's file set

Resolve by one of:
  - Investigate bot configuration; re-trigger if misconfigured
  - Mark the bot offline in `.claude/repo-config.json` `.bots[]` (`"offline": true`) if newly down
  - Skip this wait with explicit developer authorization: "skip the wait, proceed to Phase 8"
```

**Per-PR override:** the developer may explicitly authorize skipping (e.g., "skip the wait, just merge"). Honor the override per-invocation; do not normalize as default future behavior. Per-PR overrides do NOT modify the roster — toggling a bot's `.offline` is a deliberate, committed config change.

### Step 7.5.4 — Marking a bot offline (one place, no sync)

When a bot goes offline (e.g., a reviewer's plan downgrade), set `"offline": true` on its single `.bots[]` entry in `.claude/repo-config.json`. Because that array is the single source of truth shared with `/enhanced-pr-review`, this one edit updates both skills at once — there is no second table to keep in sync, and therefore no drift-induced false-positive STOP. Bringing the bot back online is the inverse edit (`"offline": false` or remove the field).

## Phase 8: FINAL REVIEW (auto-run + checkpoint)

**Prerequisite:** Phase 7.5 must have completed with all expected bots ARRIVED. If Phase 7.5 reports a STOP-on-anomaly, this phase MUST NOT run until either the bot arrives or the developer explicitly authorizes skipping the wait.

**Mode-Aware Gate (per Phase 1.5):** Review-dispatch via `/enhanced-pr-review` always runs (it's read-only triage). The "Apply fixes and reply to bot review comments?" sub-checkpoint is gated in `Hand-Held` and `Careful` modes; auto-apply in `Trusted` / `Overnight`. In `Weekly Deps`, gated only if flagged.

**Catalog Dispatch:** consult `## Phase 8: FINAL REVIEW` in [`dispatchable-skills.md`](../_shared/dispatchable-skills.md). Always-dispatch: `/enhanced-pr-review` (which itself sub-dispatches `/review-pr`). Candidates: `/superpowers:receiving-code-review`, `/design:design-critique`, `/design:accessibility-review`, `/simplify`. Recommend-only / billed: `/code-review ultra <PR#>` (chat recommendation if Phase 8 surfaces 5+ subagent findings; billed + user-triggered, never auto-invoked; deprecated alias `/ultrareview`).

Run `/enhanced-pr-review <PR>` via the Skill tool. That skill internally runs `/review-pr`, which dispatches the specialist subagents the repo declares in `.claude/repo-config.json` `.review_agents` (`.base` always-run agents plus `.patterns[]` file-pattern → agent matches); with no config it falls back to `/review-pr`'s default base set. The enumerated agent roster is therefore the repo's concern, not this skill's. Do NOT substitute with a single agent or manual review.

Display the triage table. Then checkpoint:

```
Reconciliation complete. Recommended actions:
- Fix: N findings
- Dismiss: N findings
- Conflicts: N (need your decision)

Apply fixes and reply to bot review comments? [yes / review each / skip]
```

**Phase 8 does not end when the fixes land. It ends when every finding has a recorded decision.**

Applying fixes and deciding findings are different acts, and only the first leaves a trace by default. A fix commit records *that* code changed; nothing in it says which reviewer comment it answers, and a correctly-dismissed finding leaves no artifact at all. The result is a PR whose conversations are closed but whose decisions are unrecoverable — the reviewer cannot tell whether their finding was fixed, rejected on merit, or simply lost.

So Phase 8's exit condition is the **disposition ledger** defined in [`/enhanced-pr-review` Post-Report](../enhanced-pr-review/SKILL.md): every in-scope conversation carries one reply with the reasoning plus a `cv-disposition` marker (`accepted` / `rejected` / `deferred` / `undecided`), and every triage row carries the `Thread` handle tying it to the conversation it answers. Bot reviewers post a summary comment and then one comment per finding, so both surfaces need answering — the per-finding threads through their markers, the summary comments through the Phase 3.5 reconciliation post.

Three things follow, none optional:

1. **`/enhanced-pr-review` posts its Phase 3.5 reconciliation post to the PR** — one comment, edited in place on re-runs, reconciling every reviewer's findings, every subagent's findings, and the decision on each. It posts whether or not the PR is merge-ready; a stalled review needs the record more than a clean one does.
2. **A `deferred` decision is not complete until its follow-up issue exists.** The gate calls `gh issue view` and fails closed, and rejects a PR number in an issue's place. If the issue was never filed the finding is not deferred — it is unaddressed, and routes back to Fix-in-PR.
3. **Verify mechanically, not by assertion:**

   ```bash
   .claude/skills/enhanced-pr-review/check-review-dispositions.sh "<PR>"
   ```

   Exit 0 → the ledger is complete. Exit 1 → the blocking list names each gap. Exit 2 → the check could not run; treat as UNKNOWN and re-run, never as clean.


**Review loop discipline:** each cycle closes its own findings — a finding surfaced by a fix commit gets a disposition like any other, because the ledger covers the review as it stands at HEAD. After applying reconciliation fixes:

1. Make a fix commit (or a batch of related fixes), push, and let CI re-run (loop back to Phase 6).
2. If a bot triggers a new review on the fix commit, review the **new** findings only. Do NOT re-run `/enhanced-pr-review` from the top.
3. **Soft guideline for noise prevention:** for cosmetic / low-severity / single-bot polish findings, aim for ≤ 2 fix-commit cycles per bot before surfacing remaining items to the developer.
4. **Hard rule for merge-blocking findings:** when a bot review causes `mergeStateStatus: BLOCKED` via branch protection — i.e. any `.bots[]` entry with `"blocking": true` whose latest verdict is `CHANGES_REQUESTED` / failing-gate / `BLOCKING`, or a security BLOCK finding — keep iterating until the block resolves properly. The 2-commit soft guideline does NOT apply. See [`/enhanced-pr-review` Phase 5 "Branch protection discipline"](../enhanced-pr-review/SKILL.md) for the full discipline.
5. **NEVER use `gh pr merge --admin` to circumvent a legitimate bot block.** Branch protection from a bot declared `"blocking": true` is intentionally provisioned; admin override is for emergencies and explicit per-PR developer authorization, not a workaround for "I don't want to make another fix commit."

## Phase 9: MERGE-READY ⚠️ CHECKPOINT

**Mode-Aware Gate (per Phase 1.5):** ALWAYS gated regardless of mode. Merge requires explicit user approval as a universal floor. The "I'll handle it" option remains for developers who want to merge manually.

**Catalog Dispatch:** consult `## Phase 9: MERGE-READY` in [`dispatchable-skills.md`](../_shared/dispatchable-skills.md). The merge command itself (`gh pr merge --squash`) is the always-dispatch primary. Candidates (when a version tag will be cut): `/release-notes`, `/engineering:deploy-checklist`.

### Step 9.1 — Pre-merge disposition gate (programmatic, non-negotiable)

Before presenting the merge prompt, run the gate. It is a script, not a judgment call:

```bash
.claude/skills/enhanced-pr-review/check-review-dispositions.sh "$0"
```

Branch on the exit code — and on nothing else:

- **Exit 0** → every in-scope conversation carries a decision, every deferral names an issue that exists, and the Phase 3.5 reconciliation post accounts for every reviewer that spoke. Proceed to Step 9.1.5.
- **Exit 1** → the blocking list is printed, one line per gap. Threads carrying a disposition but never marked resolved are batch-resolvable per [`/enhanced-pr-review` Step 4.3](../enhanced-pr-review/SKILL.md) (which defers to the same script); everything else needs a decision recorded. Do NOT proceed to Step 9.2.
- **Exit 2** → the check could not be made. This is **UNKNOWN, not clean** — a rate-limited or auth-failed lookup returns no violations, and reading that as a pass is the exact failure the fail-closed exit exists to prevent. Re-run before merging.

Do not re-run the full reconciliation — Phases 1–3 completed in Phase 8.

This gate exists because the human "All reviews addressed" prompt is too easy to nod through. Threads a reviewer bot closed with no human reply look identical, at the merge prompt, to threads that were answered carefully — and the difference is exactly what the ledger records. The gate makes it visible without blocking the well-handled ones.

### Step 9.1.5 — Consolidated merge-approval gate (required-review rule)

If the protected-branch ruleset requires an approving review (and possibly code-owner review on CODEOWNERS paths, with stale approvals dismissed on push), this step satisfies it. When the repo declares `.branch_protection.doc_path` (`rc_get '.branch_protection.doc_path'`), consult that doc for the authoritative current ruleset state rather than restating field values — the ruleset is operator-tunable and the committed snapshot can drift from the live rule. When no such doc is configured, read the active rule directly (`gh api repos/${OWNER}/${REPO}/rulesets`, `gh pr view $0 --json mergeStateStatus`). Either way, after Step 9.1 confirms threads resolved and CI is green at HEAD, invoke [`/enhanced-pr-review` Phase 6](../enhanced-pr-review/SKILL.md) directly (do NOT re-run Phases 1–3 — they completed in Phase 8). Phase 6:

- runs the §6.0 honesty gate — see [`/enhanced-pr-review` §6.0](../enhanced-pr-review/SKILL.md) for the authoritative precondition list (no `BLOCK` rows; no `"blocking": true` bot in a Changes-Suggested/Blocked/failing-gate state; CI green-not-pending; release-classification clear/satisfied/deferred when a release signal is configured; zero unresolved threads — a `[bot]`-resolved thread counts only with a Phase 1.7 `Verified-correct` disposition; every `Fix` finding landed; **no UNVERIFIED autofix-agent action remaining**; HEAD is the commit being approved);
- on any failure → emits the §6.4 **NOT-READY** summary; **STOP — do not proceed to Step 9.2**. The developer clears the blocker and re-invokes `/dev-lifecycle $0`;
- on pass → emits the standard consolidated-approval artifact and determines the merge mechanism per §6.1: a real `gh pr review --approve` when the PR author ≠ the gh user (the approval mechanically satisfies the rule, including code-owner review when the gh user owns the touched paths); OR a consolidated comment + §6.3 **documented bypass** when the gh user authored the PR and no eligible non-author reviewer exists (e.g., sole code owner on a CODEOWNERS-path PR). On pass, proceed to Step 9.2 (the merge checkpoint).

Because the approval must post AFTER the final push (when the ruleset dismisses stale approvals on push), this is genuinely the last step before merge — any later fix commit dismisses the approval and re-runs this gate.

### Step 9.2 — Merge checkpoint

```
Phase 9: MERGE-READY
[1 ✅] [2 ✅] [3 ✅] [4 ✅] [5 ✅] [6 ✅] [7 ✅] [8 ✅] [9 ◀]

All reviews addressed (verified: N bot threads resolved, 0 blocking). PR #<N> is ready to merge.
[squash-merge / rebase-merge / I'll handle it]
```

If squash-merge: `gh pr merge <PR> --squash --subject "<title> (#<PR>)"` with a body summarizing the changes.
If manual: print the command and exit.

### Agent-side state-file cleanup (best-effort)

After confirming the merge succeeded, the agent cleans up this branch's per-branch state files. The **mode-state file** (this skill's Phase 1.5 persistence) is always cleaned up — this skill owns it. The **hook-bypass marker file** is cleaned up **only when the repo provisions sensitive-path protection** — i.e. `rc_has '.hooks.protected_patterns'` is true; that marker is the bypass token a `protect-sensitive`-style hook honors, and repos without protected patterns never write one (so there is nothing to clean).

```bash
BRANCH_SAFE="${BRANCH//\//__}"

# Defense-in-depth: refuse cleanup if BRANCH_SAFE contains anything that
# isn't a valid filesystem character. Git's refname grammar already blocks
# path-traversal sequences (`..`, `~`, `^`, `:`, leading `-`, etc.), and
# the `/` → `__` substitution above neutralizes path separators, but this
# assertion future-proofs against a refactor that sources BRANCH from a
# less-trusted input (env var, PR API field, etc.).
#
# Regex class: [A-Za-z0-9_.-] = alphanumerics + underscore + dot + hyphen.
# Underscore must be allowed because BRANCH_SAFE contains `__` from the
# `/` → `__` substitution applied to every feature/<N>-<topic> branch.
if [[ ! "$BRANCH_SAFE" =~ ^[A-Za-z0-9_.-]+$ ]]; then
    echo "WARN: refusing /dev-lifecycle state cleanup on suspicious branch name: $BRANCH_SAFE" >&2
    return 0  # skip cleanup; do not propagate as merge failure
fi

# Mode-state file: always cleaned up (owned by this skill).
MODE_STATE=".claude/state/dev-lifecycle-mode/${BRANCH_SAFE}.txt"

# Hook-bypass marker: only relevant when the repo provisions protected patterns.
if rc_has '.hooks.protected_patterns'; then
    MARKER=".claude/state/hook-bypass/${BRANCH_SAFE}.txt"
    rm -f "$MARKER" 2>/dev/null || true   # best-effort; absent file is not an error
fi
rm -f "$MODE_STATE" 2>/dev/null || true   # best-effort; absent file is not an error
```

If either file existed and was removed, log to chat: `Cleaned up mode state: $MODE_STATE` (and `Cleaned up hook-bypass marker: $MARKER` when applicable).

### Post-merge cleanup handoff (mandatory)

After the merge succeeds, **do NOT attempt local cleanup commands** in this session — they are session-bound operations for the developer's primary terminal, not the agent's worktree. Per-command rationale: `git branch -D` is hook-blocked by the destructive-commands hook and will fail; `git checkout ${DEFAULT_BRANCH} && git pull` would change the worktree's HEAD instead of the developer's primary checkout (technically allowed, but useless to the developer); `git worktree remove` would self-delete the working directory mid-session. Retrying any of these wastes turns and produces no useful state. Print a copy-paste block for the developer instead (substitute `${DEFAULT_BRANCH}` with the resolved default branch):

```
✅ PR #<N> merged. Local cleanup (run in your terminal):

  git checkout <default-branch>
  git pull
  git branch -D feature/$0-<topic>
  # If you used a worktree for this issue, also run:
  #   git worktree remove .claude/worktrees/<worktree-name>
  # The agent has already attempted to remove any /dev-lifecycle state files
  # for this branch. If they persist (e.g., the agent's worktree was different),
  # clean them up manually:
  #   rm -f .claude/state/dev-lifecycle-mode/feature__$0-<topic>.txt
  #   # (only if your repo provisions protected patterns:)
  #   rm -f .claude/state/hook-bypass/feature__$0-<topic>.txt

Linked issue #$0 auto-closes only if the PR body contains "Closes #$0". Verify and, if still open, close manually:
  gh issue view $0 --json state --jq '.state'
  # If state is OPEN: gh issue close $0 --comment 'Closed by #<N>'
```

Substitute the real `<default-branch>`, `<topic>`, `<worktree-name>`, and `<N>` values into the block before printing — never leave placeholder angle-brackets in the output the developer is meant to copy.

After merge, if this was a child of a parent issue, return to the parent dashboard once the developer confirms cleanup is complete.

---

## What This Skill Does NOT Do

- Never skips checkpoints at Phases 5, 7, 8, and 9
- Never pushes, merges, or marks ready without explicit developer confirmation
- Never handles multiple PRs per single child issue (one branch, one PR per child)
- Never executes children in parallel (sequential, one at a time)
- Phase detection is read from live repo/GitHub data; the only persisted state is the Phase 1.5 mode-state file (`.claude/state/dev-lifecycle-mode/`), cleaned up at merge
- Does not give time estimates or deadlines
- Never runs inline `gh issue view` as a substitute for `/issue-brief`
- Never dispatches a single agent as a substitute for `/review-pr` or `/enhanced-pr-review`
- Never commits spec/plan files without first being on a feature branch
- Never runs more than 2 fix commits in a single bot review cycle (soft guideline; does not apply to merge-blocking findings)
- Never attempts post-merge local cleanup (`git branch -D`, `git checkout <default-branch> && git pull`, `git worktree remove`) — blocked by the destructive-commands hook in agent context. Always handed off to the developer as a copy-paste block (Phase 9, "Post-merge cleanup handoff")

## Why This Skill Exists

The development lifecycle has 9 distinct phases, each invoking different skills with different tools. Without orchestration, the developer manually remembers which phase they're in, which command to run next, and which checkpoints need confirmation. `/dev-lifecycle` automates the routine transitions while preserving human control at every decision point that affects shared state (pushing, marking ready, merging).
