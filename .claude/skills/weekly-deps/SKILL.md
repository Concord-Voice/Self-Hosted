---
name: weekly-deps
description: "Triage the weekly batch of bot-authored dependency PRs (Dependabot plus github-actions updates) by risk and confidence, request missing Copilot reviews where needed, route higher-risk items through the repo's configured deep-review skill, and process safer bumps first to reduce lockfile conflicts. Active ecosystems are auto-detected from the live PR diffs (optionally pinned via `.claude/repo-config.json` `.ecosystems`); tier package→group data and the security-sensitive overlay come from `.weekly_deps.*` in the same config. Supports interactive triage/merge plus --mode=prep and --mode=triage for autonomous, non-merge weekly routines."
argument-hint: [pr-number | all | --mode=prep | --mode=triage]
allowed-tools:
  - Bash(gh pr *)
  - Bash(gh api *)
  - Bash(gh run *)
  - Bash(gh issue *)
  - Bash(gh repo *)
  - Bash(git log *)
  - Bash(git diff *)
  - Bash(git rev-parse *)
  - Bash(date *)
  - Bash(jq *)
  - Read
  - Grep
  - Glob
---

<DISPATCH-GATE>
When a phase says "Run `/skill-name`", you MUST invoke it via the Skill tool. Do NOT substitute with inline commands or partial implementations. Sub-skills own their execution; this skill owns the sequencing. This is not optional.
</DISPATCH-GATE>

# Weekly Dependency PRs

Triage and merge the weekly batch of bot-authored dependency PRs (dependabot + github-actions). Argument:

- **No arg** (or `all`) → discover and process all open bot PRs (interactive).
- **PR number** (`$0`) → run pre-flight + processing on just that one PR through its tier's workflow (interactive).
- **`--mode=prep`** → autonomous prep run intended for a scheduled cron. Discovers, categorizes, runs community-signal lookup, requests Copilot reviews, sends `@dependabot rebase` on conflicts, writes a digest to the pinned issue, then exits. Does NOT merge anything. Does NOT schedule the triage run — triage has its own independent cron (see Schematic below). Read `references/autonomous-mode.md` before proceeding.
- **`--mode=triage`** → autonomous follow-up running on an independent cron. Picks up Copilot reviews that have landed since prep, auto-dismisses style nits, surfaces real findings into the same pinned-issue comment. Does NOT merge anything. Read `references/autonomous-mode.md` before proceeding.

> **Config-driven, ecosystem-auto-detecting.** This skill's engine (risk tiers A/B/C/D, confidence assessment, lockfile-contention ordering, the prep/triage cron flow) is generic. The repo-specific knowledge — which package ecosystems are in play, which packages map to which tier, which packages are security-sensitive, where the deep review goes, the cron schedule — is **data**, read from the OPTIONAL per-repo `.claude/repo-config.json` via `scripts/repo-config.sh` (`rc_get '<jq-filter>' [default]`, `rc_has '<jq-filter>'`). Active ecosystems are **auto-detected** from the live PR diffs (Phase 1.5); `.ecosystems` only pins/overrides that set. With an absent config (or empty `.weekly_deps.*`), the skill degrades safely: every bump defaults to **Tier C** (manual deep review), the security-sensitive list is the shared universal default, and Tier D yields nothing. See `knowledge/repo-config.reference.md` for the full schema.

---

## Phase 0: LOAD CONFIG + IDENTITY

Before anything else, resolve repo identity and load the OPTIONAL per-repo config. Every later phase reads from these values; an absent file or absent key degrades gracefully (never errors).

```bash
# Repo identity (owner/repo), auto-derived from gh — no hardcoded org/repo.
OWNER_REPO=$(gh repo view --json nameWithOwner -q .nameWithOwner)
OWNER=$(echo "$OWNER_REPO" | cut -d/ -f1)
REPO=$(echo "$OWNER_REPO" | cut -d/ -f2)

# Config helper (path where the org tooling is installed/symlinked).
source "$(git rev-parse --show-toplevel)/scripts/repo-config.sh"

# Pinned ecosystem set (OPTIONAL). Empty → auto-detect from live PR diffs in Phase 1.5.
ECOSYSTEMS_PIN="$(rc_get '.ecosystems | join(" ")' '')"

# Tier package→group data (OPTIONAL). '{}' → every bump defaults to Tier C (manual review).
TIERS_JSON="$(rc_get '.weekly_deps.tiers' '{}')"

# Deep-review sub-skill for Tier C (OPTIONAL). Default /enhanced-pr-review; /review-pr is the final manual backstop.
DEEP_REVIEW_SKILL="$(rc_get '.weekly_deps.deep_review_skill' '/enhanced-pr-review')"

# Tier D security-data refresh sources (OPTIONAL). '[]' → Tier D yields nothing.
TIER_D_SOURCES="$(rc_get '.weekly_deps.tier_d_sources' '[]')"

# Pinned-issue digest title (OPTIONAL).
DIGEST_ISSUE_TITLE="$(rc_get '.weekly_deps.digest_issue_title' 'Weekly Dependency Triage — Routine Output')"
```

The security-sensitive package set and the local-test commands are loaded on demand in the phases that need them (Phase 3.5 and Phase 4.C respectively) — see those phases.

---

## Choosing the right workflow

`/weekly-deps` is the entry point for **batch-mergeable bot PRs** — dependabot bumps and any github-actions security-data refresh. These share three structural properties that distinguish them from human-authored PRs:

1. **No GitHub issue** behind them — they're not part of the milestone/phase tracker.
2. **Reproducible from upstream** — closing and re-running the bot regenerates an equivalent PR, so the cost of skipping or rejecting is low.
3. **Touch the same lockfiles** — multiple open PRs in the same directory contend for the same lockfile (`package-lock.json` / `go.sum` / etc.), so merge order matters even when each individual PR is mergeable in isolation.

Use the repo's feature/refactor lifecycle skill for issue-tied, single-author work. For dependency PRs, `/weekly-deps` is the orchestrator — but for Tier C PRs (Phase 4.0 / 4.C), it dispatches into the repo's configured deep-review skill (`.weekly_deps.deep_review_skill`, default `/enhanced-pr-review`) to reconcile the Copilot review that Phase 3.6 requested with the subagent table that the deep-review skill brings in via `/review-pr`. The split is: `weekly-deps` owns the discovery, categorization, confidence-based Copilot routing, lockfile-aware merge order, and tech-stack-doc follow-up; the deep-review skill owns the deep-review triage on individual Tier C PRs.

Use `/review-pr` standalone only as a fallback when Phase 3.6's Copilot request was declined for a specific PR and that PR is still Tier C — without a Copilot review present, `/enhanced-pr-review` has less to reconcile and a direct `/review-pr` is more efficient.

> **Extension point — label-based reclassification.** A repo may file dependency PRs that warrant a non-default tier based on a label (for example, a routine that opens transitive-dependency-hygiene PRs and tags them). Reclassifying such PRs by label is a supported extension point: add a rule in Phase 2 that maps the label to a tier (and, if conservative-bump heuristics apply, a demotion condition). The shared skill ships no repo-specific label rules.

---

## Autonomous mode (weekly routine)

When invoked as `/weekly-deps --mode=prep` (recurring prep cron) or `/weekly-deps --mode=triage` (independent recurring triage cron), the skill runs non-interactively and writes its digest to a pinned GitHub issue instead of stdout. **Read `references/autonomous-mode.md` before proceeding in either autonomous mode** — it documents the prep flow, the triage flow, the independent-cron design (prep does NOT schedule triage), and the operating-discipline non-negotiables (no autonomous merges, no new PRs, idempotent state mutations only including the week-marker idempotency check on digest comments).

The cron schedule and timezone label are config-driven (`.weekly_deps.cron.{prep,triage,tz_label}`, defaults `0 13 * * 3` / `0 15 * * 3` / `ET`). Load them when reporting the schematic:

```bash
CRON_PREP="$(rc_get '.weekly_deps.cron.prep' '0 13 * * 3')"
CRON_TRIAGE="$(rc_get '.weekly_deps.cron.triage' '0 15 * * 3')"
TZ_LABEL="$(rc_get '.weekly_deps.cron.tz_label' 'ET')"
```

The digest output channel (a single pinned issue, one comment per ISO week, edited in place by the triage run) is documented in `references/pinned-issue-digest.md`. One-time setup (creating + pinning the issue) is a maintainer's responsibility — the routine does not auto-create the issue, and aborts with a clear error if it can't find one.

Schematically (times shown for the default schedule; substitute the configured cron):

```
prep cron (e.g. 0 13 * * 3) ──┐    triage cron (e.g. 0 15 * * 3) ──┐
                              ▼                                    ▼
   /weekly-deps --mode=prep              /weekly-deps --mode=triage
   • discover, categorize                • re-discover
   • Phase 3.4 community signal          • find prep-run comment
   • Phase 3.6 request Copilot           • fetch Copilot reviews
   • Phase 5 rebase conflicts            • Phase 4.0 auto-dismiss
   • post digest comment                 • edit digest comment in place
   • exit                                • exit
                                                  │
   later that day (when the dev gets in) ─────────▼
   the developer runs `/weekly-deps` interactively
   • reads the pinned-issue digest
   • processes Phase 4 merges with confirmations
```

The two crons are **independent** — the prep run does NOT schedule the triage. If prep fails, the triage cron still fires and aborts cleanly with "no prep comment for this week" (Step T2 of `references/autonomous-mode.md`). This is more robust than the chained-schedule alternative.

Both autonomous modes share Phases 1–3, plus Phase 5 (rebase) in `prep` and Phase 4.0 (auto-dismiss only, no merges) in `triage`. Phases 4.A–D and 6.2 (the merge and follow-up PR steps) are **never run in autonomous mode** — they require human confirmation per the operational discipline.

---

## Phase 1: DISCOVER

Enumerate open bot-authored PRs. Don't trust the user's pasted list — re-fetch live state.

```bash
# Dependabot PRs (all ecosystems dependabot manages for this repo)
gh pr list \
  --search "author:app/dependabot is:open" \
  --json number,title,labels,headRefName,mergeable,mergeStateStatus,statusCheckRollup,files,reviews,reviewRequests,isDraft,createdAt,updatedAt \
  --limit 50

# Other bot-authored PRs (github-actions for security-data refreshes etc.)
gh pr list \
  --search "author:app/github-actions is:open" \
  --json number,title,labels,headRefName,mergeable,mergeStateStatus,statusCheckRollup,files,reviews,reviewRequests,isDraft,createdAt,updatedAt \
  --limit 20
```

**Note on the search query:** the two calls cannot be combined into `author:app/dependabot author:app/github-actions` — GitHub treats multiple `author:` qualifiers as AND, not OR, which returns empty. Always use two separate calls and union the results.

The `reviews` and `reviewRequests` fields are load-bearing for re-entrant detection: if Copilot has already been requested (Phase 3.6) or already reviewed (Phase 4.0), the second invocation of the skill picks that up and routes accordingly.

The second `gh pr list` call (the `author:app/github-actions` query) also catches PRs opened by repo workflows via PR-creation actions (e.g. `peter-evans/create-pull-request`) — including any label-driven reclassification extension a repo adds (see "Choosing the right workflow" above). Phase 2 applies any such repo-defined label rule.

If the user passed `$0` as a specific PR number, narrow to just that PR via `gh pr view $0 --json ...` and skip directly to Phase 2 for that one PR.

If zero open bot PRs: report "No open bot-authored PRs. Nothing to triage." and exit.

---

## Phase 1.5: DETECT ACTIVE ECOSYSTEMS

Derive the active ecosystem set from the **live PR diffs**, not from a hardcoded list. For each discovered PR, list its changed files and map each manifest to an ecosystem; the union across all PRs is the active set for this run.

```bash
# For each PR number from Phase 1:
gh pr diff $PR --name-only
```

Map each touched manifest path to an ecosystem (basename-based, directory-agnostic):

| Manifest file (basename) | Ecosystem |
|---|---|
| `package.json` / `package-lock.json` / `npm-shrinkwrap.json` / `yarn.lock` / `pnpm-lock.yaml` | `npm` |
| `go.mod` / `go.sum` | `gomod` (advisory ecosystem `go`) |
| `.github/workflows/*.yml` (action refs) | `github-actions` (advisory ecosystem `actions`) |
| `requirements*.txt` / `Pipfile*` / `pyproject.toml` / `poetry.lock` | `pip` |
| `Cargo.toml` / `Cargo.lock` | `cargo` |
| `Gemfile` / `Gemfile.lock` | `rubygems` |
| `composer.json` / `composer.lock` | `composer` |

The union of mapped ecosystems is `DETECTED_ECOSYSTEMS`. **If `.ecosystems` is set in repo-config** (`ECOSYSTEMS_PIN` from Phase 0 is non-empty), use it as the authoritative set instead (a pin/override — useful for repos that want to scope the routine to a subset, or assert the expected set for sanity-checking). Otherwise use the detected set.

The detected ecosystem of each PR drives:
- the **expected file footprint** check in Phase 3.3 (a PR's diff should touch only its ecosystem's manifests in one directory),
- the **advisory-DB ecosystem mapping** in Phase 3.4 (`community-signal-lookup.md`),
- the **per-ecosystem local-test defaults** in Phase 4.C.

If a PR touches manifests for an ecosystem outside the detected/pinned set (shouldn't happen for a normal bot bump), treat it as a surprise diff (Phase 3.3) and escalate to Tier C / LOW.

---

## Phase 2: CATEGORIZE

Bucket each PR into one of four risk tiers using the rules below, then present the tier table to the developer.

### The tier concept (generic) + the data (config)

The four tiers are a fixed *concept*; the package→group→tier *data* is per-repo and lives in `.weekly_deps.tiers` (loaded as `TIERS_JSON` in Phase 0). Each entry maps a tier letter to named groups, and each group lists the packages (glob patterns) and the bump shape that qualifies. To classify a PR's bumped package(s): glob-match each package name against the groups in `TIERS_JSON`; the matched group's tier is the starting tier.

**With `.weekly_deps.tiers == {}` (the safe default), no package matches any group, so every bump falls through to Tier C (manual deep review).** This is intentional: a repo that hasn't declared its tier data gets conservative, never-fast-merged behavior rather than an unsafe fast-track.

**The major-bump override is universal and always applies, regardless of config:**

> **Any major-version bump (`X.Y.Z → X+1.0.0`) → Tier C**, regardless of which group the package belongs to. The dependabot group label / config group is a *starting* signal, not the final answer. Always check the version shape before final tier assignment.

For grouped PRs (`chore: bump the X group ...`), extract the individual package names from the PR body bullet list (`gh pr view $PR --json body --jq '.body'`) and classify each; the **highest-risk** member determines the PR's tier (any Tier C member → whole PR Tier C; any major bump in the group → Tier C).

### Tier A — Fast (batch-confirm, minimal review)

The "dev-environment-only, low-blast-radius" tier. Failures here would surface as developer-experience regressions, not production incidents — they live behind the `devDependencies` (or equivalent dev-only) boundary, so a breakage shows up locally during the repo's test/lint step, not in shipped artifacts, and CI catches it before merge. Batch these together for one confirmation prompt.

Membership is whatever the repo declares under `.weekly_deps.tiers.A` (typically dev-tooling, build-tooling, test frameworks, type stubs — patch/minor bumps). With no config, nothing lands here.

### Tier B — Standard (per-PR quick check)

Production dependencies receiving patch or minor bumps. Skim the release notes for breaking-change keywords, but don't budget hours of investigation. Membership is whatever the repo declares under `.weekly_deps.tiers.B`. With no config, nothing lands here.

### Tier C — Careful (deep review, possibly local test)

Framework-level changes, security-relevant production libraries, and **all major-version bumps**. These touch paths where a subtle behavior shift can cause a real incident, so they MUST get a thorough look. A PR lands in Tier C when ANY of:

- its package matches a group the repo declares under `.weekly_deps.tiers.C`, OR
- it is a major-version bump (`X.Y → X+1.0`) in any group (the universal override above), OR
- any bumped package matches the security-sensitive list (Phase 3.5), OR
- it has a surprise diff (Phase 3.3), OR
- **no tier data is configured** (the safe default routes everything here).

Tier C PRs get the deepest review path: Phase 3.6 requests a Copilot review, then Phase 4.0 / Phase 4.C dispatch the configured deep-review skill (`.weekly_deps.deep_review_skill`, default `/enhanced-pr-review <PR>`) via the Skill tool to reconcile Copilot's findings with the repo's subagent table. `/review-pr` is used inside `/enhanced-pr-review`, so a direct call there would duplicate work.

### Tier D — Security Data (verify upstream integrity)

PRs that refresh static security-relevant **data**, not application code (threat-feed pulls, indicator/signature/blocklist refreshes, and similar). The review question for Tier D is "does the data come from the same authoritative source it claimed to come from in the previous refresh?" — not "is this code correct?" Check the diff is data-only, the source URL hasn't changed, and CI passed.

Tier D membership is config-driven via `.weekly_deps.tier_d_sources` (loaded as `TIER_D_SOURCES` in Phase 0) — a list of source descriptors, each identifying a security-data refresh (e.g. by author + label + the expected data-file path glob). A PR is Tier D when it matches one of these descriptors. **With `.weekly_deps.tier_d_sources == []` (the default), Tier D yields nothing** — repos without a security-data refresh workflow simply never produce a Tier D PR.

### Display the categorization table

After classifying, present to the developer (example shape; rows reflect whatever the repo actually has):

```
/weekly-deps — N open bot PRs   (ecosystems: <detected/pinned set>)
──────────────────────────────────────────────────────────────────────────
Tier A (Fast — batch-mergeable):              N PRs
  #NN <group> (K updates)                       [<manifest dir>]    HIGH
  ...

Tier B (Standard — per-PR check):             N PRs
  #NN <pkg> <from> → <to>                       [<manifest dir>]    LOW (security-sensitive match)
  ...

Tier C (Careful — deep review):               N PRs
  #NN <pkg> <from> → <to>                       [<manifest dir>]    LOW (MAJOR bump)
  ...

Tier D (Security data — integrity):           N PRs
  #NN <security-data refresh>                                       n/a (data-only)

Copilot review queue (Phase 3.6 will request):
  #NN, #NN, ...  — all LOW

Lockfile contention (process order matters):
  <manifest dir>/<lockfile>:   K PRs  — sequential merge w/ rebase
  ...

Proposed processing order:
  1. Tier A (HIGH only) → Phase 4.A batch-merge now
  2. Request Copilot on LOW PRs → exit, re-invoke in ~5–10 min
  3. (after re-invoke) Tier A (MEDIUM), Tier B, Tier D, Tier C
─────────────────────────────────────────────────────────────────────────
```

A single-package bump in the "patch" shape that would otherwise be Tier A or B is **promoted to Tier C by a major version bump** — illustrating the universal override. **Always check the version shape (`X.Y.Z → X+1.0.0` = major) before final tier assignment.**

---

## Phase 3: PRE-FLIGHT

For every PR you plan to merge, verify three things. If any fails, skip that PR and surface it at the end as "needs developer attention."

### 3.1 CI status

```bash
gh pr checks $PR --json bucket --jq '[.[].bucket] | if all(. == "pass" or . == "skipping") then "green" elif any(. == "fail") then "red" else "pending" end'
```

- `green` → continue to 3.2.
- `red` → skip; suggest `@dependabot recreate` only if the failure looks like a transient or stale-lockfile issue (network blip, snapshot mismatch). Otherwise close the PR with an explanatory comment.
- `pending` → skip for this run; the developer can re-invoke after CI settles.

### 3.2 Mergeable status

```bash
gh pr view $PR --json mergeable,mergeStateStatus --jq '{mergeable, mergeStateStatus}'
```

- `mergeable=MERGEABLE` and `mergeStateStatus=CLEAN` → ready.
- `mergeStateStatus=BEHIND` → behind the default branch; the rebase will happen during merge or via `@dependabot rebase`.
- `mergeable=CONFLICTING` → blocked; queue for Phase 5 (Conflict).

### 3.3 No surprise diff

```bash
gh pr diff $PR --name-only
```

Expected file footprint per PR type — derived from the PR's detected ecosystem (Phase 1.5), confined to a single manifest directory:

| PR ecosystem | Expected files only |
|---|---|
| npm | `package.json` + the lockfile (`package-lock.json` / `yarn.lock` / `pnpm-lock.yaml`) in the dependabot directory |
| gomod | `go.mod`, `go.sum` in the dependabot directory |
| github-actions | `.github/workflows/*.yml` (the actions being bumped) |
| pip | `requirements*.txt` / `Pipfile*` / `pyproject.toml` + the matching lock, in the dependabot directory |
| cargo | `Cargo.toml`, `Cargo.lock` in the dependabot directory |
| rubygems | `Gemfile`, `Gemfile.lock` in the dependabot directory |
| composer | `composer.json`, `composer.lock` in the dependabot directory |
| security-data refresh (Tier D) | only the data-file path(s) declared in the matching `.weekly_deps.tier_d_sources` descriptor |

**Surprise diffs require manual review.** If a `chore: bump` PR also touches files outside its expected footprint (workflows, source, anything else), escalate to Tier C / LOW confidence regardless of original classification. The surprise-diff signal feeds directly into Phase 3.5 (sets LOW) and Phase 3.6 will then request Copilot review; Phase 4.0/4.C dispatches the deep-review skill for the actual triage. This routing is one of the few defenses against a compromised bot or a malicious-typosquat scenario where a dependency's install script tries to land arbitrary content — the supply-chain concern needs the full review path, not a fast-track.

### 3.4 Community signal lookup

Before computing confidence, check whether the upstream community has surfaced known issues with any bumped package — security advisories, regression reports, yanked versions. The ~24h window between when dependabot opens a PR and when this skill processes it is typically enough for malicious publishes or critical regressions to get caught and reported. Looking up that signal cheaply hardens the routine against supply-chain incidents (malicious publish, accidental destructive release, upstream-maintainer-pulled packages).

Read `references/community-signal-lookup.md` for the full mechanics. Summary of what it checks per bumped package:

1. **GitHub Advisory Database** — `gh api "/advisories?ecosystem=<eco>&published=><date>"` filtered server-side by ecosystem and date, then version-range-checked client-side via jq's `.vulnerabilities[].package.name` field. The `<eco>` is the advisory-DB name for the PR's detected ecosystem (npm→`npm`, gomod→`go`, github-actions→`actions`, pip→`pip`, cargo→`rust`, rubygems→`rubygems`, composer→`composer`). The `/advisories` endpoint has no `package=` query parameter; client-side filtering is required. See `references/community-signal-lookup.md` for the full query.
2. **Release-notes keyword scan** — greps the dependabot-supplied release notes in the PR body for unambiguous yank/advisory markers ONLY: `yanked`, `do not upgrade`, `critical bug`, `hotfix needed`, `reverted in v<N>`, plus `CVE-YYYY-NNNN` and `GHSA-` identifiers. Bare `BREAKING` / `MAJOR` / `regression` keywords are deliberately NOT scanned — they fire too often in normal release notes (see History section in `community-signal-lookup.md`).
3. **Package repo issue spike** (only for packages matching `security-sensitive-packages.md`) — counts recent issues in the package's GitHub repo mentioning the new version + crash/regression keywords. Uses a static known-mapping from package name to GitHub repo (curl is not in `allowed-tools`); skipped for packages outside the known-mapping. A repo can extend the known-mapping via `.weekly_deps.signal3_repo_map`.

The lookup produces one record per PR with a `verdict` field that Phase 3.5 (confidence) consumes. Failure modes (API rate limit, non-GitHub repo, missing release tag) degrade gracefully to `UNKNOWN` rather than `HIGH` — a missing signal must never elevate confidence.

For grouped PRs (`chore: bump the X group ...`), the lookup runs once per package in the group. A LOW verdict on ANY package elevates the whole PR to LOW.

### 3.5 Confidence assessment

Combine the signals from 3.1–3.4 plus the tier from Phase 2 plus the bumped-package identity into a single confidence label per PR. The label drives Phase 3.6 (whether to request Copilot review) and Phase 4 (whether to fast-merge or hold for triage).

Confidence is biased toward false-positives. The cost of an unnecessary Copilot review is low (a few minutes of bot time the user already pays for via subscription); the cost of a missed regression in a security-sensitive bump is potentially much higher.

| Label | Conditions (ALL must hold) |
|---|---|
| **HIGH** | Tier A, AND CI green, AND no surprise diff, AND patch bump (or grouped dev-only), AND no bumped package matches the security-sensitive list, AND community-signal verdict is not LOW |
| **MEDIUM** | Tier A minor bump with green CI + clean footprint, OR Tier B with green CI + no breaking-change keywords in PR body + no security-sensitive package match + no community-signal LOW verdict |
| **LOW** | ANY of: Tier C; CI red or pending; surprise diff; breaking-change keywords (`BREAKING`, `MAJOR`, `deprecate`, `remove`, `behavior change`, `default changed`) in PR body; any bumped package matches the security-sensitive list; community-signal lookup returned LOW verdict (active GHSA/CVE against the bumped-to version, or strong-negative release-note keyword match) |
| **UNKNOWN** | Missing signal — no `statusCheckRollup`, unparseable bump shape, community-signal lookup failed, or any pre-flight check returned an error. Treat as LOW. |

**To compute the security-sensitive match:** the effective list is the **shared universal default ∪ the repo's `.weekly_deps.security_sensitive_extra`**. Read `references/security-sensitive-packages.md` (the universal default — loaded on demand to keep SKILL.md lean), then union the repo's extras:

```bash
SECURITY_EXTRA="$(rc_get '.weekly_deps.security_sensitive_extra | join(" ")' '')"
```

Glob-match each bumped package name against the union. For grouped PRs (`chore: bump the X group ...`), extract individual package names from the PR body bullet list via `gh pr view $PR --json body --jq '.body'`. A match on ANY package in the group elevates the whole PR to LOW.

Annotate each PR with its label and append to the categorization table from Phase 2 — the developer should see confidence alongside tier when deciding what to do.

### 3.6 Request Copilot review (gated on LOW / UNKNOWN)

Bot-authored PRs do NOT trigger an automatic Copilot review, because Copilot Code Review is gated on the PR author having the subscription — dependabot and github-actions don't. The result is that the weekly batch of dependabot PRs lands with zero review signal beyond CI, which is a real gap for Tier C and security-sensitive bumps.

This phase closes that gap by explicitly requesting Copilot review on LOW / UNKNOWN PRs. The mechanism (a maintainer adding `Copilot` as a `review_requested` reviewer, which the Copilot Code Review bot honors) is the canonical path on GitHub.

**Step 3.6.1 — Idempotency check.** For each LOW / UNKNOWN PR, check whether Copilot is already requested or has already reviewed:

```bash
# Already requested?
gh pr view $PR --json reviewRequests --jq '[.reviewRequests[] | select((.login // "") == "Copilot" or (.login // "") == "copilot-pull-request-reviewer")] | length'

# Already reviewed?
gh pr view $PR --json reviews --jq '[.reviews[] | select(.author.login == "copilot-pull-request-reviewer")] | length'
```

If either count is > 0, skip this PR (Copilot is either still working on it or done; the next re-invocation will pick up the review in Phase 4.0).

**Step 3.6.2 — Batch confirmation.** Show the developer the list of PRs that need a fresh Copilot review request, grouped by tier with the reason each one was flagged:

```
Phase 3.6 — Request Copilot review on N LOW-confidence PRs?

Tier C (always-LOW):
  #NN <pkg> <from> → <to>                     [MAJOR bump / framework / security boundary]
  ...

CI failed / pending:
  #NN <group>                                 [CI red — Copilot may surface the cause]

Security-sensitive package match:
  #NN <pkg> <from> → <to>                     [matches security-sensitive list]

Each will get @copilot added as a reviewer via:
  gh pr edit <PR> --add-reviewer "@copilot"

Proceed? [yes — request all] [pick subset] [skip — review yourself]
```

**Step 3.6.3 — Dispatch.** On `yes`:

```bash
for PR in $LOW_PRS; do
  gh pr edit $PR --add-reviewer "@copilot"
done
```

The CLI's `@copilot` literal is the canonical path per `gh pr edit --help` — it routes through GraphQL's `botIds` field internally, which the plain REST `requested_reviewers` POST does not support for bots. Do NOT substitute the bot's full login `copilot-pull-request-reviewer` in the standard reviewer-request API; that returns an error.

On `pick subset`: prompt the developer to specify which PRs to skip, then dispatch only the rest.

**Step 3.6.4 — Hand off and exit early.** Copilot Code Review typically takes a few minutes per PR (more for large diffs). Don't sit and poll inside this skill invocation — tell the developer:

```
Copilot review requested on N PRs.
Re-invoke /weekly-deps in ~5–10 min and I'll pick up the reviews in Phase 4.0.

In the meantime, HIGH-confidence PRs can proceed — would you like to process those now?
[yes — process HIGH now] [no — wait for full batch] [exit]
```

If `yes — process HIGH now`: skip directly to Phase 4 with only the HIGH-confidence subset. This is safe because HIGH PRs are by definition Tier A green-CI dev-only patches with no security-sensitive packages — they don't benefit from Copilot review and would just clog its queue.

If `no — wait`: print the resume command and exit.

If a Copilot review request fails (auth issue, repo billing, API hiccup): surface the error, name the affected PR, and offer fallbacks — (a) retry, (b) skip Copilot and dispatch `/review-pr` instead for that PR (subagent-based review using the repo's existing agent table), or (c) defer to manual review by the developer in the GitHub UI.

---

## Phase 4: PROCESS

Work through tiers in the proposed order. Within each tier, walk PRs sequentially **within each lockfile group**, but across lockfile groups you can interleave (different directories don't contend).

**Re-entrant detection.** Before processing any PR, check whether it has a Copilot review present (set in Phase 3.6 of a previous invocation, now completed):

```bash
gh pr view $PR --json reviews --jq '[.reviews[] | select(.author.login == "copilot-pull-request-reviewer")] | map({state, submittedAt, body: (.body // "" | .[0:200])})'
```

If a Copilot review exists → route through Phase 4.0 (Copilot review triage) before per-tier processing. If no review yet but Copilot is in `reviewRequests`, surface the still-pending state to the developer and skip this PR for this run.

### 4.0 — Copilot review triage (when re-entrant detects a review)

For each PR with a Copilot review present, triage the findings before deciding on the merge. The shape of triage differs by tier:

**Tier A / B → inline lighter triage.** Fetch the review summary and inline comments:

```bash
gh pr view $PR --json reviews --jq '.reviews[] | select(.author.login == "copilot-pull-request-reviewer") | {state, body}'
gh api "repos/$OWNER/$REPO/pulls/$PR/comments" --jq '.[] | select(.user.login | test("copilot"; "i")) | {path, line, body: (.body | .[0:300])}'
```

Apply the auto-dismiss rules below and surface only findings that pass:

| Finding pattern | Action |
|---|---|
| Style nit, formatting, naming preference | Auto-dismiss |
| "Consider adding a test for ..." on a dependency bump | Auto-dismiss (deps don't ship new tests for upstream changes) |
| "Consider adding documentation ..." on a dep bump | Auto-dismiss |
| Type-safety finding on dev-only (e.g. `devDependencies`) patch bump | Auto-dismiss if CI green; surface if CI red |
| Specific behavioral change called out ("X.method now returns Y instead of Z") | **Surface** |
| Deprecation warning that affects shipped/production code | **Surface** |
| Breaking-change call-out matching the PR's bump shape | **Surface** |
| Anything mentioning manifest modifications outside the bumped package | **Surface** (supply-chain signal) |

Present a compact summary to the developer:

```
#$PR — <title>
Copilot review: <APPROVED | COMMENTED | CHANGES_REQUESTED>
Inline findings: N total
  Auto-dismissed (N): style/nit/test-coverage
  Surfacing (N):
    - <file>:<line> — <finding summary>

Proceed? [merge / fix in-PR / hold]
```

**Tier C → full deep-review dispatch.** For major version bumps and framework changes, the lighter inline triage is insufficient. Invoke the repo's configured deep-review skill via the Skill tool — `$DEEP_REVIEW_SKILL <PR>` (from Phase 0; default `/enhanced-pr-review <PR>`). That skill is designed precisely for this — it fetches all bot-authored review comments via author-type filter, runs `/review-pr` internally to add subagent coverage (the repo's declared specialists), auto-triages known patterns, and presents a unified triage table.

If `.weekly_deps.deep_review_skill` is unset AND `/enhanced-pr-review` is not available in the repo, fall back to `/review-pr <PR>`; if no review skill exists at all, hand the PR to the developer for manual review.

Do NOT substitute inline gh commands or a single dispatched agent for the deep-review skill here — the DISPATCH-GATE at the top of this skill applies. The deep-review skill owns the execution; `weekly-deps` owns the sequencing.

After the deep-review skill returns its triage table, prompt the developer for the merge decision per the existing Phase 4.C step 5 format.

**Tier D → skip Copilot triage entirely.** Tier D PRs (security-data refreshes) are data-only and Copilot review adds no useful signal. If Copilot was somehow requested on a Tier D PR (e.g., a security-sensitive package match misfired), surface the review for the developer's awareness but proceed with the existing Phase 4.D data-integrity check as the primary signal.

### 4.A — Tier A processing (batch-confirm, HIGH confidence only)

Process Tier A PRs that pre-flight labeled HIGH (Phase 3.5). A Tier A PR that landed at MEDIUM (e.g., a minor bump in a type-stubs package) or LOW (e.g., a security-sensitive package match) does NOT batch-merge here — it routes through Phase 4.B or 4.0 instead.

Show the developer the HIGH-confidence Tier A list with CI status and mergeable status, then prompt:

```
Tier A (HIGH) — N PRs, all green CI, all mergeable, no security-sensitive matches.

Squash-merge all in sequence?
[yes — merge all] [review each] [skip tier A this run]
```

If `yes — merge all`:

1. **Pick the first PR per directory** to merge first (one per manifest directory). These cannot conflict with each other.
2. For each:
   ```bash
   gh pr merge $PR --squash --delete-branch
   # Worktree quirk: --delete-branch may exit non-zero on local cleanup;
   # server-side merge already succeeded. Verify with:
   gh pr view $PR --json state --jq .state
   # Expect: "MERGED"
   ```
3. **Do not trust the exit code** of `gh pr merge` in worktree context — always verify with `gh pr view`.
4. After each merge, comment `@dependabot rebase` on the next PR in that same directory's queue (the lockfile changed; subsequent PRs need a refresh):
   ```bash
   gh pr comment $NEXT_PR --body "@dependabot rebase"
   ```
5. Wait briefly for the rebase + CI re-run before merging the next in that directory's queue. In practice the developer should run `/weekly-deps` again after ~10–20 minutes to pick up the rebased PRs.

If `review each`: drop into Tier B's per-PR flow for each Tier A PR.

### 4.B — Tier B processing (per-PR quick check)

For each Tier B PR in turn:

1. Show the PR title, the bump shape (e.g., `8.18.0 → 8.20.0`, minor), and the changelog excerpt:
   ```bash
   gh pr view $PR --json body --jq '.body' | head -80
   ```
2. Skim for breaking-change keywords: `BREAKING`, `MAJOR`, `deprecate`, `remove`, `behavior change`, `default changed`.
3. If clean: prompt the developer:
   ```
   #$PR — <title>
   Bump: <from> → <to> (minor/patch)
   Changelog: <summary>
   No breaking-change keywords found.

   Merge? [yes / view full diff / view full release notes / skip]
   ```
4. On `yes`, run the merge command from 4.A step 2 and verify via `gh pr view`.
5. On `view full diff`: `gh pr diff $PR | head -200` and re-prompt.

### 4.C — Tier C processing (deep review)

Tier C deserves the most care. For each Tier C PR:

1. **Read release notes thoroughly.** Scan the upstream project's release notes / CHANGELOG for the new version. Grep for breaking-change markers relevant to the package class — generic ones include `BREAKING CHANGE`, `removed`, `deprecated`, `default changed`, plus any security-boundary keywords specific to the package (a repo may track these per-package family in its rules docs).
2. **Cross-reference any repo rules docs** that govern the surface the package defends (whatever per-surface rules docs the repo keeps, if any). Any default that changed in the new version is a potential rules-doc update.
3. **Invoke the configured deep-review skill via the Skill tool** — `$DEEP_REVIEW_SKILL <PR>` (default `/enhanced-pr-review <PR>`). This is the canonical path for Tier C: it fetches the Copilot review that Phase 3.6 requested, runs `/review-pr` internally to add subagent coverage (the repo's declared specialists), and produces a unified triage table that the developer can act on in one pass. Do NOT substitute a direct `/review-pr` invocation that bypasses the Copilot reconciliation — the existing Copilot review is load-bearing and ignoring it would re-do work the bot already finished.

   If Phase 3.6 was skipped for this PR (the developer chose "skip — review yourself"), fall back to `/review-pr $PR` directly. `/enhanced-pr-review` still works without a Copilot review present but adds less value than a direct subagent dispatch in that case.
4. **Run local tests if the bump touches buildable/runnable code.** Use the repo's configured per-directory commands, read from `.weekly_deps.local_test_cmds` (a map of manifest-directory → command list). When unset for a directory, fall back to a sensible per-ecosystem default for that PR's detected ecosystem:

   ```bash
   # Per-directory override (preferred). $DIR is the PR's manifest directory and comes
   # from the PR, so pass it as data (--arg), never into the jq program text.
   LOCAL_CMDS="$(jq -r --arg d "$DIR" '.weekly_deps.local_test_cmds[$d] // [] | join("\n")' "$RC_FILE" 2>/dev/null)"
   ```

   | Ecosystem | Default local-test commands (run from the manifest dir) |
   |---|---|
   | npm | `npm test`, `npm run lint`, `npm run build` (skip any script the repo doesn't define) |
   | gomod | `go test -race ./...`, `go vet ./...` |
   | github-actions | (no local test; rely on CI re-run) |
   | pip | `pytest` (or the repo's test runner) |
   | cargo | `cargo test`, `cargo clippy` |
   | rubygems | `bundle exec rspec` (or the repo's test task) |
   | composer | `composer test` (or the repo's test script) |

5. **Present findings** to the developer with a clear recommendation:
   ```
   #$PR — <title>
   Major/framework bump: <from> → <to>
   Release-note highlights: <summary, breaking-change items>
   Copilot review: <APPROVED | CHANGES_REQUESTED | COMMENTED> — N inline findings
   Deep-review triage:
     Auto-dismissed: N findings
     Real concerns: N findings (see triage table)
     Conflicts (Copilot vs subagent): N (need your decision)
   Local test result: <pass/fail>
   Rules-doc impact: <none | requires update to a rules doc>

   Recommendation: [merge / merge after fixing X / hold for now / close]
   ```
6. Wait for explicit developer decision. Do not auto-merge Tier C.

### 4.D — Tier D processing (security data integrity)

For each Tier D PR:

1. **Verify the diff is data-only.** `gh pr diff $PR --name-only` should show only the data file(s) declared in the matching `.weekly_deps.tier_d_sources` descriptor, no code.
2. **Inspect the data delta.** `gh pr diff $PR | head -200` — look for the additions/removals. A normal refresh adds new entries and may remove expired ones; an abnormal refresh would remove a large fraction of entries or add entries with unusual structure.
3. **Verify the upstream source.** The bot's PR body should reference the same upstream feed URL as the previous refresh. If the source URL has changed without an explanatory commit, flag for developer review.
4. Prompt:
   ```
   #$PR — <title>
   Data delta: +N entries, -M entries
   Source: <upstream URL>
   Diff scope: data-only (no code changes)

   Merge? [yes / view full delta / skip]
   ```
5. On `yes`, merge with `gh pr merge $PR --squash --delete-branch` and verify.

---

## Phase 5: CONFLICT RESOLUTION

After a round of merges (or in `--mode=prep` autonomous runs, where any PR is already CONFLICTING at routine time), some PRs in the same lockfile directories will need rebasing. Dependabot listens for `@dependabot rebase` and will refresh the PR against the new default branch.

**Interactive mode:** prompt the developer before sending each rebase command. Some developers prefer to inspect the conflict before rebasing.

**Autonomous mode (`--mode=prep`):** send `@dependabot rebase` to every CONFLICTING PR without prompting. The command is idempotent server-side, and `references/autonomous-mode.md` adds a 1-hour cooldown check (don't re-send if a rebase comment was already posted in the last hour) to handle accidental double-fires of the cron.

For each conflicting PR:

```bash
gh pr comment $PR --body "@dependabot rebase"
```

The rebase typically takes a few minutes plus a full CI run. Don't sit and poll — tell the developer:

```
Conflict resolution in progress:
  #NN — @dependabot rebase requested
  #NN — @dependabot rebase requested

Re-invoke /weekly-deps in ~15–20 min to pick up the rebased PRs.
```

If a PR repeatedly conflicts even after rebase (e.g., the bumped package keeps shifting), use `@dependabot recreate` to start fresh. If the package is genuinely incompatible with the current default branch (real merge conflict, not a lockfile artifact), surface to developer for manual resolution or close with explanation.

---

## Phase 6: SUMMARY + POST-MERGE

After processing completes, report results and surface follow-ups.

### 6.1 Merge summary

```
/weekly-deps — Run summary
─────────────────────────────────────────────────────────────
Merged this run:           N PRs
  Tier A (HIGH):  N (#NN, #NN, ...)
  Tier B:         N (#NN, #NN, ...)
  Tier C:         N (#NN <pkg> — Copilot approved, local test passed)
  Tier D:         N (#NN security-data refresh)

Awaiting Copilot review:   N PRs
  #NN — @copilot requested, no review yet (re-invoke in ~5 min)

Awaiting rebase:           N PRs
  #NN — @dependabot rebase requested

Held / closed:             N PRs
  None

CI failed (skipped):       N PRs
  #NN <group> — Copilot review surfaced root cause (see comment thread)

Rules-doc updates needed: <list>
─────────────────────────────────────────────────────────────
```

### 6.2 Tech-stack-doc version refresh (optional)

If the repo records tech-stack versions in a doc (e.g. a "Tech Stack" table in its top-level guide) and provides a regeneration script, and any merged PR bumped one of those versions, the recorded version is now stale.

When the repo ships such a script, run it and offer to commit the update on a fresh branch (the script's dry-run-by-default / `--apply`-to-write contract is the repo's; this skill just invokes it). Skip this step entirely if the repo has no such script or doesn't record stack versions in a doc.

If the script updates the doc, create a follow-up PR:
- Branch name: a non-`feature/`-prefixed name (post-merge bookkeeping), e.g. `chore/stack-version-refresh-<date>`
- Title: `chore: refresh tech-stack versions after weekly dep bumps`
- Body: list the bumped versions that triggered the update

Prompt the developer before pushing — this is a fresh branch + PR creation, which is in the "actions affecting shared state" bucket and needs explicit confirmation.

### 6.3 Rules-doc impact

If any Tier C merge changed a behavior covered by a repo rules doc (if the repo keeps any), the rules doc may need an update. Surface this explicitly with file path and the specific section that may be stale; do not silently update the rules doc — the developer should review and decide.

### 6.4 Post-merge cleanup handoff

The dependabot branches are deleted server-side by `--delete-branch`. Per operational discipline, do NOT attempt local cleanup commands in this agent session — they may be blocked or self-destructive in worktree context. Print a copy-paste block for the developer's primary terminal:

```
✅ Weekly dep PRs processed. Local cleanup (run in your terminal):

  git checkout <default-branch>
  git pull
  # Prune any local dependabot/* branches you may have checked out:
  git branch | grep '^[^*]*dependabot/' | xargs -n1 git branch -D 2>/dev/null
  # If you used a worktree for this batch, also run:
  #   git worktree remove .claude/worktrees/<worktree-name>
```

Substitute the real default-branch name and `<worktree-name>` value before printing — never leave the placeholders.

---

## What This Skill Does NOT Do

- Does **not** enable GitHub's auto-merge feature — every merge is explicit and verified.
- Does **not** push branches or open PRs of its own except the optional tech-stack-doc version refresh follow-up (Phase 6.2), which is gated on developer confirmation and only when the repo ships such a script.
- Does **not** modify a repo's rules docs based on dependency bumps — flags for developer review only.
- Does **not** merge Tier C PRs without dispatching the configured deep-review skill (or `/review-pr` as fallback if Copilot review was explicitly skipped) first.
- Does **not** request Copilot review on HIGH-confidence PRs — that's noise for both the bot's queue and the developer's inbox. Copilot is reserved for LOW/UNKNOWN signal.
- Does **not** request Copilot review on PRs that already have one in `reviews` or `reviewRequests` — Step 3.6.1 idempotency check prevents double-requesting.
- Does **not** poll inside the same invocation while waiting for Copilot to finish a review — the developer re-invokes after ~5–10 minutes, the same re-entrant cadence as `@dependabot rebase`.
- Does **not** use the bare `requested_reviewers` REST endpoint with `reviewers: ["copilot-pull-request-reviewer"]` — that returns an error because Copilot is a bot, not a user. Always use `gh pr edit --add-reviewer "@copilot"` or the GraphQL `botIds` path.
- Does **not** auto-dismiss Copilot findings that mention specific behavioral changes, deprecations affecting shipped code, breaking-change call-outs, or supply-chain anomalies — those always surface to the developer regardless of tier.
- Does **not** suppress or tune static-analysis or quality-gate rules to force a dependency PR's check to pass — if a bump degrades coverage or trips a rule, the fix is in the repo's test code, not the rule config.
- Does **not** trust `gh pr merge`'s exit code in worktree context — always verifies with `gh pr view`.
- Does **not** process all PRs in a single round when lockfile contention is present — that's the entire reason `@dependabot rebase` exists.
- Does **not** retry the same PR after `@dependabot rebase` in the same invocation — the rebase + CI cycle takes too long. The developer re-invokes `/weekly-deps` after ~15–20 minutes.
- Does **not** budget hours of investigation for Tier C unless the developer asks. The recommendation in Phase 4.C may be "hold for now" if the bump deserves a dedicated session.

**Autonomous mode specifically does NOT:**

- Merge any PR. The prep run + triage run produce *information* (a digest in the pinned issue) and *idempotent preparation* (Copilot review requests, `@dependabot rebase` comments). Every merge still requires the developer's interactive `/weekly-deps` session.
- Create any new PRs, branches, or commits. The tech-stack-doc version refresh (Phase 6.2 of interactive mode) is skipped entirely in autonomous mode — that's the developer's call to make.
- Run subagent dispatches (`/enhanced-pr-review`, `/review-pr`) on Tier C PRs. The triage run only does the lighter inline auto-dismiss + finding-surface; the heavyweight subagent reconciliation stays interactive.
- Modify a repo's rules docs or `references/security-sensitive-packages.md` based on what it sees. Updates to those files always require a developer-driven PR.
- Auto-create or auto-pin the routine's output issue. The digest issue (title from `.weekly_deps.digest_issue_title`) is a one-time manual setup (see `references/pinned-issue-digest.md`); the routine aborts cleanly if it doesn't exist.
- Retry a failed routine step in the same invocation. If `gh api` fails to post the digest comment, the prep run logs to `/tmp/weekly-deps-digest-<week>.md` and exits non-zero so the cron's failure notification fires — the developer investigates and re-invokes manually.

## Why This Skill Exists

Every week, dependabot opens a batch of dependency PRs and the github-actions bot occasionally adds a few more for security-data refreshes. Without a skill, the developer:

1. Manually skims each title trying to remember which groups are "fine to merge fast" and which need real review,
2. **Lacks any external review signal beyond CI on those PRs** — Copilot Code Review is gated on the PR author having the subscription, and dependabot doesn't, so bot-authored PRs land with zero AI review unless someone explicitly asks for it,
3. Discovers mid-batch that the second PR in a directory is conflicting because they merged the first one,
4. Forgets to refresh any tech-stack-version doc after framework bumps, and
5. Spends far longer than necessary on what should be a quick triage.

`/weekly-deps` automates the routine (Tier A batches, lockfile-aware ordering, rebase comments, optional version-doc refresh) while preserving human control at every decision that affects shared state (merging Tier C, opening the follow-up PR, pruning local branches). The risk knowledge is **data** — the repo declares its tiers, its security-sensitive packages, its security-data sources in `.claude/repo-config.json` — so the same engine works across repos with different stacks; a repo that declares nothing gets the safe all-Tier-C default.

Critically, it closes the bot-PR review blind spot: confidence-tiered Copilot requests (Phase 3.6) mean LOW/UNKNOWN PRs get the same review treatment a human-authored PR would, while HIGH-confidence dev-tooling patches don't burn Copilot's queue. The re-entrant Phase 4.0 then reconciles Copilot's findings — inline for Tier A/B (auto-dismiss style nits, surface behavioral changes) or via the full deep-review dispatch for Tier C, which also brings in the repo's subagent table. The end result is that a major framework bump or a validation-library major bump gets the same level of review scrutiny as a human-driven PR in the standard lifecycle flow.

The autonomous-mode addition (prep + triage crons) lets the same workflow run unattended *up to but not including* the actual merge decisions. By the time the developer arrives, the pinned issue holds a fully-populated digest: PR inventory by tier/confidence, community-signal flags from the adoption window (advisories filed, yank/hotfix markers spotted, issue spikes on package repos), Copilot reviews already requested and likely already completed, and lockfile-conflicting PRs already rebased. The developer's interactive `/weekly-deps` session can then skip Phases 1–3 entirely and jump directly to merge decisions — turning a manual workflow into a focused-decision session, without compromising any of the operational-discipline guarantees that "every merge gets explicit approval."
