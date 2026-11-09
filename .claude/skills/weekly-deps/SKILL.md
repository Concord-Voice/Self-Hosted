---
name: weekly-deps
description: "Triage the weekly batch of bot-authored dependency PRs (Dependabot plus github-actions updates) by risk and confidence, extract the embedded engine delta on bundled-engine bumps (e.g. Electron/Chromium, Node/V8), request Copilot reviews when Copilot is actually available, route higher-risk items through the repo's configured deep-review skill, and process safer bumps first to reduce lockfile conflicts. Active ecosystems are auto-detected from the live PR diffs (optionally pinned via `.claude/repo-config.json` `.ecosystems`); tier package→group data and the security-sensitive overlay come from `.weekly_deps.*` in the same config. Supports interactive triage/merge plus --mode=prep and --mode=triage for autonomous, non-merge weekly routines."
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
  - Bash(git fetch *)
  - Bash(git worktree *)
  - Bash(mktemp *)
  - Bash(rmdir *)
  - Bash(diff *)
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

> **Config-driven, ecosystem-auto-detecting.** This skill's engine (risk tiers A/B/C/D, confidence assessment, lockfile-contention ordering, the prep/triage cron flow) is generic. The repo-specific knowledge — which package ecosystems are in play, which packages map to which tier, which packages are security-sensitive, where the deep review goes, the cron schedule — is **data**, read from the OPTIONAL per-repo `.claude/repo-config.json` via `scripts/repo-config.sh` (`rc_get '<jq-filter>' [default]`, `rc_has '<jq-filter>'`). Active ecosystems are **auto-detected** from the live PR diffs (Phase 1.5); `.ecosystems` only pins/overrides that set. With an absent config (or empty `.weekly_deps.*`), the skill degrades safely: every bump defaults to **Tier C** (manual deep review), the security-sensitive list is the shared universal default, and Tier D yields nothing. See `config/repo-config.reference.md` for the full schema.

---

## Phase 0: LOAD CONFIG + IDENTITY

Before anything else, resolve repo identity and load the OPTIONAL per-repo config. Every later phase reads from these values; an absent file or absent key degrades gracefully (never errors).

```bash
# Repo identity (owner/repo), auto-derived from gh — no hardcoded org/repo.
OWNER_REPO=$(gh repo view --json nameWithOwner -q .nameWithOwner)
OWNER=$(echo "$OWNER_REPO" | cut -d/ -f1)
REPO=$(echo "$OWNER_REPO" | cut -d/ -f2)

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

Use the repo's feature/refactor lifecycle skill for issue-tied, single-author work. For dependency PRs, `/weekly-deps` is the orchestrator — but for Tier C PRs (Phase 4.0 / 4.C), it dispatches into the repo's configured deep-review skill (`.weekly_deps.deep_review_skill`, default `/enhanced-pr-review`) to reconcile any Copilot review Phase 3.6 obtained (Copilot is frequently unavailable — see the 3.6 precondition) with the specialist subagent suite that the deep-review skill dispatches directly. The split is: `weekly-deps` owns the discovery, categorization, confidence-based Copilot routing, lockfile-aware merge order, and tech-stack-doc follow-up; the deep-review skill owns the deep-review triage on individual Tier C PRs.

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

# Workflow-opened PRs authored by a GitHub App (OPTIONAL — only if the repo has such a
# routine). The author queries above miss them; query by the label the workflow applies.
gh pr list \
  --search 'label:"<routine-label>" is:open' \
  --json number,title,labels,headRefName,mergeable,mergeStateStatus,statusCheckRollup,files,reviews,reviewRequests,isDraft,createdAt,updatedAt \
  --limit 5
```

**Note on the search query:** the calls cannot be combined into `author:app/dependabot author:app/github-actions` — GitHub treats multiple `author:` qualifiers as AND, not OR, which returns empty. Always use separate calls and union the results.

The `reviews` and `reviewRequests` fields are load-bearing for re-entrant detection: if Copilot has already been requested (Phase 3.6) or already reviewed (Phase 4.0), the second invocation of the skill picks that up and routes accordingly.

The second `gh pr list` call (the `author:app/github-actions` query) also catches PRs opened by repo workflows via PR-creation actions (e.g. `peter-evans/create-pull-request`) using the default `GITHUB_TOKEN` — including any label-driven reclassification extension a repo adds (see "Choosing the right workflow" above). A workflow that authenticates as its own GitHub App instead (so PR CI runs on its PRs without a manual re-trigger) is **not** `app/github-actions`, so only the third, label-based call catches its PRs. Phase 2 applies any such repo-defined label rule.

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

### Special rule: bundled-engine PRs

Some dependencies are **wrappers around an engine whose version is not the package's
version**. The package bumps a minor; the engine underneath moves independently. Every
risk heuristic in this skill that reads the *package's* semver is blind to that by
construction — including the "patch or minor" shapes of Tier A and B, and the major-bump
override above.

**Members of the class (examples — extend for your stack):**

| Package / group | Embedded engine to extract |
|---|---|
| `electron` | **Chromium** and **Node.js** (both are named in the dependabot body) |
| WebRTC client/server libraries that bundle libwebrtc | bundled **libwebrtc** |
| Node runtime bumps (CI matrix, Docker base, `engines` floor) | **V8** |
| Database / cache / TURN container image tags | the server engine itself |

**Step 1 — extract the engine delta at FULL PATCH PRECISION.** Dependabot often already
prints it; nothing in the routine asks anyone to read it. In the digest, report the engine
line, not just the package line:

```bash
gh pr view "$PR" --json body --jq .body \
  | grep -oiE 'Updated (Chromium|Node\.js) to [0-9][0-9.]*' | sort -u
```

Report as `electron 43.1.1 → 43.3.0  (Chromium 150.0.7871.114 → 150.0.7871.212)`. The
**second** parenthesis is the risk-bearing number. A wrapper minor can carry an engine
milestone crossing, and — the case that bites in practice — an engine *patch* roll can
carry a behavior regression.

**Step 2 — always Tier C.** Any engine digit change is Tier C regardless of the wrapper's
bump shape. Do not demote because the package bump "is only a minor."

**Step 3 — walk the platform-behavior register**, if the repo keeps one (a list, usually in
its frontend rules file, of timing/ordering/availability guarantees the app relies on —
see `_shared/context7-docs-discipline.md` § web-platform APIs). For each entry, ask whether
this engine delta could have changed it. The register exists because these contracts are
**not visible in any diff and not covered by any unit test** — only a real engine exhibits
them. If an entry looks exposed, say so in the digest and name the runtime probe that would
catch it. A repo with no register gets Steps 1–2 only; say so in the digest rather than
implying the walk happened.

> **Founding incident (a past outage).** The Electron package was already Tier C, and the
> tier was honored. It still missed, because the Tier C question was *"check release notes
> for `contextIsolation` / `sandbox` / `webPreferences`"* — an **Electron-API-security**
> checklist that never asks what the bundled Chromium changed. A minor Electron bump rolled
> Chromium by a patch. That patch stopped routing frames through an encoded transform
> attached after the media receiver went live, so unprocessed frames went straight to the
> decoder and a core media feature broke for every user.
>
> **No code reviewer could have caught this, and that is the point** — the diff is a
> version string, so the entire risk lives in behavior the patch does not contain. Do not
> respond to a miss in this class by asking reviewers to try harder. The controls that
> work are the three steps above, plus the runtime probes named in the register.
>
> **Honest limit:** the engine never documented the regression, so Step 1 alone would
> probably not have caught it either. What *would* have is reading the platform docs for
> the API at the call site, which guaranteed first-frame delivery only for the
> creation-time path. That control lives in `_shared/context7-docs-discipline.md`
> § web-platform APIs. Step 1 is what makes the engine delta *visible* so the register
> walk in Step 3 has something to act on.

### Tier A — Fast (batch-confirm, minimal review)

The "dev-environment-only, low-blast-radius" tier. Failures here would surface as developer-experience regressions, not production incidents — they live behind the `devDependencies` (or equivalent dev-only) boundary, so a breakage shows up locally during the repo's test/lint step, not in shipped artifacts, and CI catches it before merge. Batch these together for one confirmation prompt.

Membership is whatever the repo declares under `.weekly_deps.tiers.A` (typically dev-tooling, build-tooling, test frameworks, type stubs — patch/minor bumps). With no config, nothing lands here.

### Tier B — Standard (per-PR quick check)

Production dependencies receiving patch or minor bumps. Skim the release notes for breaking-change keywords, but don't budget hours of investigation. Membership is whatever the repo declares under `.weekly_deps.tiers.B`. With no config, nothing lands here.

**Bundled-engine packages are NOT Tier A or B at any bump shape.** Even if the repo's
tier data lists one under a "patch or minor" group, the bundled-engine special rule above
claims it for Tier C unconditionally. Do not let the two rules be resolved by reading
order — the Tier C claim wins, and tier data that contradicts it should be fixed.

### Tier C — Careful (deep review, possibly local test)

Framework-level changes, security-relevant production libraries, and **all major-version bumps**. These touch paths where a subtle behavior shift can cause a real incident, so they MUST get a thorough look. A PR lands in Tier C when ANY of:

- its package matches a group the repo declares under `.weekly_deps.tiers.C`, OR
- it is a bundled-engine package with any engine version change (§ Special rule: bundled-engine PRs), OR
- it is a major-version bump (`X.Y → X+1.0`) in any group (the universal override above), OR
- any bumped package matches the security-sensitive list (Phase 3.5), OR
- it has a surprise diff (Phase 3.3), OR
- **no tier data is configured** (the safe default routes everything here).

For a wrapper such as Electron, Tier C asks **two separate questions, and the second is the one that has caused an outage.** (a) *API security* — check release notes for `contextIsolation`, `sandbox`, `webPreferences` default changes against the repo's rules docs. (b) *Bundled-engine behavior* — extract the engine delta at full patch precision and walk the platform-behavior register, per § Special rule: bundled-engine PRs. (a) alone has passed consecutive bumps while the engine broke behavior underneath.

Tier C PRs get the deepest review path: Phase 3.6 requests a Copilot review, then Phase 4.0 / Phase 4.C dispatch the configured deep-review skill (`.weekly_deps.deep_review_skill`, default `/enhanced-pr-review <PR>`) via the Skill tool to reconcile Copilot's findings with the repo's specialist subagents (`.review_agents`). The specialist suite is dispatched inside the deep-review skill, so dispatching it again here would duplicate work.

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
  #NN electron <from> → <to>                    [<manifest dir>]    LOW (bundled engine)
       └─ engine: Chromium <from> → <to> · Node <from> → <to>
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

**The `└─ engine:` line is mandatory for every bundled-engine PR** (§ Special rule
above), at full patch precision — it is the number the risk actually attaches to, and
printing it is what makes an otherwise-invisible milestone or patch roll reviewable.
Follow it with the register-walk verdict (when the repo keeps a register), so a Tier C
engine bump reads:

```
  #NN electron 43.1.1 → 43.3.0                  [<manifest dir>]    LOW (bundled engine)
       └─ engine: Chromium 150.0.7871.114 → 150.0.7871.212 · Node 24.18.1
       └─ register: <entry> — probe armed (<probe name>)
                    <entry> — unaffected · <entry> — unaffected
```

A register entry with **no armed probe** is itself the finding: report it rather than
passing the PR.

A single-package bump in the "patch" shape that would otherwise be Tier A or B is **promoted to Tier C by a major version bump** — illustrating the universal override. **Always check the version shape (`X.Y.Z → X+1.0.0` = major) before final tier assignment** — the dependabot group label is a starting signal, not the final answer.

---

## Phase 3: PRE-FLIGHT

For every PR you plan to merge, verify three things. If any fails, skip that PR and surface it at the end as "needs developer attention."

### 3.1 CI status

```bash
CI_WF="$(rc_get '.weekly_deps.ci_workflow' '')"   # empty → no presence check
gh pr checks "$PR" --json bucket,workflow | jq -r --arg wf "$CI_WF" '. as $checks | if ($wf != "" and ([$checks[] | select(.workflow == $wf)] | length) == 0) then "missing" elif ($checks | length) == 0 then "missing" elif ([$checks[].bucket] | all(. == "pass" or . == "skipping")) then "green" elif ([$checks[].bucket] | any(. == "fail")) then "red" else "pending" end'
```

- `green` → continue to 3.2.
- `red` → skip; suggest `@dependabot recreate` only if the failure looks like a transient or stale-lockfile issue (network blip, snapshot mismatch). Otherwise close the PR with an explanatory comment.
- `pending` → skip for this run; the developer can re-invoke after CI settles.
- `missing` → no checks at all, or none from the workflow named in `weekly_deps.ci_workflow` when that key is set. Classify as UNKNOWN and skip. For a routine-labelled PR opened by a workflow that authenticates as a GitHub App (Phase 1), CI should start on its own; if it is still missing, check that workflow's App-token mint step before falling back to a close/reopen.

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

### 3.3b Pinned-ref upstream resolution (github-actions ecosystem only)

Skip this for npm and gomod PRs — it applies to `.github/workflows/**` action bumps.

**Why this lives here and not in CI.** A CI gate of the shape "this action's runtime is digest-pinned" asserts *internal consistency*: that `with.version` fullmatches the version named in the `uses:` line's trailing `# vX.Y.Z` comment, plus a sha256. That is a real invariant and it fails closed — it is the kind of gate that catches a half-updated pin dependabot produces.

What it structurally **cannot** see is whether the comment names the real upstream tag, or whether the digest belongs to that tag. **A pin that is wrong but self-consistent passes the gate silently** — a SHA lifted from a different tag, or a digest for `latest`, satisfies every regex.

Closing that in CI was considered and rejected: it would put a live GitHub API + registry round-trip on a merge-blocking gate, trading a real availability failure mode for a hypothetical integrity one. Triage is the right home — here a network failure degrades the PR to UNKNOWN, which is already the correct handling, instead of blocking a merge.

**Check 1 — the action SHA resolves to the commented tag.**

```bash
# For each `uses: <owner>/<repo>@<40-hex>  # vX.Y.Z` in the diff — ACTION_REPO is that
# upstream <owner>/<repo>, NOT this repo's $OWNER/$REPO from Phase 0:
gh api "repos/$ACTION_REPO/git/ref/tags/v$VERSION" --jq '.object.sha, .object.type'
```

Must equal the pinned SHA. If `.object.type` is `tag` (annotated), dereference it before comparing. Also confirm the commit is on mainline rather than an unmerged side branch:

```bash
# Resolve the upstream default branch — do NOT hardcode `main`.
# docker/setup-qemu-action is on `master`, and it is a common member of these batches, so a
# hardcoded `main` 404s on a real member of the set, not a hypothetical one.
DEFAULT_BRANCH=$(gh api "repos/$ACTION_REPO" --jq .default_branch)
gh api "repos/$ACTION_REPO/compare/$DEFAULT_BRANCH...$PINNED_SHA" --jq '.status, .ahead_by'
# expect: "behind" or "identical", ahead_by 0
```

**Check 2 — the image digest resolves to that same tag** (only when the action takes a co-located digest-bearing input):

```bash
docker buildx imagetools inspect "$IMAGE:$VERSION"   # read the top-level Digest
```

**Use `buildx imagetools`, not `docker manifest inspect -v`.** They return *different digests* for the same tag — `imagetools` gives the multi-platform manifest-list digest, which is the kind these pins use; `manifest inspect -v` reports a per-platform descriptor. Both are well-formed sha256 values, so picking the wrong one produces a plausible pin that fails the gate. Verify the method first by resolving the **outgoing** version and confirming it reproduces the pin already in the file — if it does not, the method is wrong, not the pin.

**Check 3 — bind the digest to the commit** (strongest, when the publisher sets OCI labels):

```bash
# `.Image` resolves index -> per-platform manifest -> config blob for you,
# returning a map keyed by platform, each carrying that platform's config:
docker buildx imagetools inspect "$IMAGE:$VERSION" --format '{{json .Image}}'

# Read org.opencontainers.image.revision from any platform's .config.Labels.
#
# Do NOT reach for `--raw` here. On a multi-platform tag it returns the INDEX,
# whose only keys are manifests/mediaType/schemaVersion — it contains no config
# blob, so the label is unreachable and the command appears to return nothing
# useful. Index- and manifest-level ANNOTATIONS are a different field from
# config LABELS; never read one as the other.
```

When `image.revision` equals the pinned action SHA, the digest, the registry tag, and the action ref all bind to one upstream commit — the tag can be repointed later without affecting the pin. Absent labels are common and are not a finding.

**Lockstep sweep — a `with:` version input is only a lockstep partner when it names the SAME artifact as the `uses:` ref.** Getting this backwards causes a wrong "fix":

| Pair | Lockstep? |
|---|---|
| `trufflesecurity/trufflehog@<sha> # vX` + `with.version: 'X@sha256:…'` | **Yes** — same artifact (the action runs that image), gate-enforced |
| `oras-project/setup-oras@<sha> # v2.0.1` + `with.version: 1.3.3` | **No** — `1.3.3` is the ORAS *CLI* the action installs, an independent rail. Leave it alone |

**A tool may have several independent version rails, and divergence between them is not drift.** A scanner can legitimately run at different versions on purpose in one repo — e.g. a dependabot-managed, gate-enforced workflow pin; a tag pin in `.pre-commit-config.yaml` kept by documented convention; and a release tarball verified against that release's checksums in another workflow. Only the first is under a lockstep gate. Do not "harmonize" them.

**Provenance note.** For these images the digest is the *only* integrity binding — it guarantees the bytes are the ones reviewed, not who built them. No cosign signature or OCI attestation is published for `ghcr.io/trufflesecurity/trufflehog` (the `.sig` tag 404s and `/referrers/` returns `MANIFEST_UNKNOWN`), despite `sigstore/cosign-installer` appearing in the upstream release workflow. Never describe this rail as signature-verified, and do not add a `cosign verify` step expecting it to pass.

**Verdict:** any check that fails, or cannot be completed, sets the PR to **LOW** per Phase 3.5. A failed lookup is UNKNOWN, never HIGH — a missing signal must not elevate confidence.

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
| **LOW** | ANY of: Tier C; CI red or pending; surprise diff; breaking-change keywords (`BREAKING`, `MAJOR`, `deprecate`, `remove`, `behavior change`, `default changed`) in PR body; any bumped package matches the security-sensitive list; community-signal lookup returned LOW verdict (active GHSA/CVE against the bumped-to version, or strong-negative release-note keyword match); a github-actions PR whose Phase 3.3b pinned-ref upstream resolution failed or could not be completed |
| **UNKNOWN** | Missing signal — no `statusCheckRollup`, unparseable bump shape, community-signal lookup failed, or any pre-flight check returned an error. Treat as LOW. |

**To compute the security-sensitive match:** the effective list is the **shared universal default ∪ the repo's `.weekly_deps.security_sensitive_extra`**. Read `references/security-sensitive-packages.md` (the universal default — loaded on demand to keep SKILL.md lean), then union the repo's extras:

```bash
SECURITY_EXTRA="$(rc_get '.weekly_deps.security_sensitive_extra | join(" ")' '')"
```

Glob-match each bumped package name against the union. For grouped PRs (`chore: bump the X group ...`), extract individual package names from the PR body bullet list via `gh pr view $PR --json body --jq '.body'`. A match on ANY package in the group elevates the whole PR to LOW.

Annotate each PR with its label and append to the categorization table from Phase 2 — the developer should see confidence alongside tier when deciding what to do.

### 3.6 Request Copilot review (gated on LOW / UNKNOWN)

> **⚠ PRECONDITION — Copilot Code Review may be deactivated on this repo (e.g. billing lapsed), and it is usually not a required reviewer.** In that state `gh pr edit --add-reviewer "@copilot"` **exits 0 and registers nothing**, and `POST /pulls/<n>/requested_reviewers` returns 404. The failure is silent, which is the dangerous part: without a check, this phase reports "Copilot review requested" for PRs that will never receive one, and Phase 3.5's confidence model then credits a signal that does not exist.
>
> **So verify, do not assume — in both directions.** Before running this phase, confirm the request actually took:
>
> ```bash
> gh pr edit "$PR" --add-reviewer "@copilot" >/dev/null 2>&1
> gh pr view "$PR" --json reviewRequests \
>   --jq '[.reviewRequests[] | select((.login // "") == "Copilot" or (.login // "") == "copilot-pull-request-reviewer")] | length'
> ```
>
> `0` means Copilot is off. Skip the phase for the whole run, log one line — `INFO: Copilot Code Review unavailable; skipping 3.6 and treating its signal as absent` — and **do not** escalate, retry, or surface it as a finding: its absence is a settled decision, not an incident. Then carry that into Phase 4: a Tier C PR that got no Copilot review takes the **direct specialist dispatch** path described in 4.C step 3, not the reconcile-Copilot path.
>
> Equally, do not hardcode "Copilot is off" — it returns when billing is re-enabled. The probe above is the source of truth; this note is not.

Bot-authored PRs do NOT trigger an automatic Copilot review, because Copilot Code Review is gated on the PR author having the subscription — dependabot and github-actions don't. The result is that the weekly batch of dependabot PRs lands with zero review signal beyond CI, which is a real gap for Tier C and security-sensitive bumps.

This phase closes that gap — **when Copilot is available** — by explicitly requesting review on LOW / UNKNOWN PRs. The mechanism (a maintainer adding `Copilot` as a `review_requested` reviewer, which the Copilot Code Review bot honors) is the canonical path on GitHub.

**Note the wider reality even when it is on:** AI reviewers commonly decline bot-authored PRs (a seat-based reviewer such as Gitar may be seatless on them, CodeRabbit reports "Bot user detected", Codex can hit quota, Copilot goes unrequested). Dependency PRs frequently merge on CI plus this skill's own triage alone. That is expected, and it is the strongest argument for the deterministic checks in Phase 2 — a bundled-engine delta extraction runs whether or not any reviewer shows up.

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

If a Copilot review request fails (auth issue, repo billing, API hiccup): surface the error, name the affected PR, and offer fallbacks — (a) retry, (b) skip Copilot and dispatch the specialist subagents directly for that PR (per `_shared/pr-review-agent-dispatch.md`, unioned with the repo's `.claude/repo-config.json` `.review_agents`; the model cannot invoke the user-only review skill), or (c) defer to manual review by the developer in the GitHub UI.

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

**Tier C → full deep-review dispatch.** For major version bumps and framework changes, the lighter inline triage is insufficient. Invoke the repo's configured deep-review skill via the Skill tool — `$DEEP_REVIEW_SKILL <PR>` (from Phase 0; default `/enhanced-pr-review <PR>`). That skill is designed precisely for this — it fetches all bot-authored review comments via author-type filter, dispatches the repo's declared specialist subagents (`.review_agents`) directly to add coverage, auto-triages known patterns, and presents a unified triage table.

If `.weekly_deps.deep_review_skill` is unset AND `/enhanced-pr-review` is not available in the repo, fall back to dispatching the specialist suite directly (per `_shared/pr-review-agent-dispatch.md`); if no reviewer agents exist at all, hand the PR to the developer for manual review (they can run `/review-pr <PR>` themselves — it is user-invoked only). <!-- dispatch-lint:ok — a user-facing suggestion, not a model dispatch -->

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

1. **Read release notes thoroughly — the package's AND the engine's.** Scan the upstream project's release notes / CHANGELOG for the new version. Grep for breaking-change markers relevant to the package class — generic ones include `BREAKING CHANGE`, `removed`, `deprecated`, `default changed`, plus any security-boundary keywords specific to the package (for Electron: `contextIsolation`, `sandbox`, `nodeIntegration`, `webPreferences`; a repo may track these per-package family in its rules docs). **For any bundled-engine package, this step is not complete until the engine delta is extracted at full patch precision and reported** (§ Special rule: bundled-engine PRs). Scanning only the wrapper's notes is exactly the gap behind the founding incident there — the engine roll was printed in the dependabot body and nothing asked anyone to read it.
2. **Cross-reference any repo rules docs** that govern the surface the package defends (whatever per-surface rules docs the repo keeps, if any). Any default that changed in the new version is a potential rules-doc update.

   **2b. Walk the platform-behavior register** (if the repo keeps one) on every bundled-engine bump. For each entry, ask whether this engine delta could have changed it, and name the runtime probe that would catch it if so. A register entry with no armed probe is the finding — say so rather than passing the PR.
3. **Invoke the configured deep-review skill via the Skill tool** — `$DEEP_REVIEW_SKILL <PR>` (default `/enhanced-pr-review <PR>`). This is the canonical path for Tier C: it fetches the Copilot review that Phase 3.6 requested, dispatches the repo's declared specialist subagents directly to add coverage, and produces a unified triage table that the developer can act on in one pass. Do NOT substitute a direct specialist-suite dispatch that bypasses the Copilot reconciliation — the existing Copilot review is load-bearing and ignoring it would re-do work the bot already finished.

   If Phase 3.6 was skipped for this PR — the developer chose "skip — review yourself", **or the 3.6 precondition probe found Copilot deactivated** — fall back to dispatching the specialist suite directly (per `_shared/pr-review-agent-dispatch.md`). `/enhanced-pr-review` still works without a Copilot review present but adds less value than a direct specialist dispatch in that case.

   **If the review produces a fix to apply, check the escalation criteria (E1–E4) before choosing who applies it** — see `_shared/lifecycle-agent-dispatch.md` § Escalation. A bundled-engine bump that turns out to have moved an auth, crypto, or key-handling behavior is **E2 on its face**; a fix in this class often spans app code, a worker or helper, and the rules docs. Never pass a `model` override; select the agent.
4. **Run local tests if the bump touches buildable/runnable code.** Run them at the PR's head, never in the current checkout: that tree still has the old lockfile, so a pass there proves nothing about the bump. Use a throwaway worktree:

   ```bash
   # Detached at the fetched head, outside the repo (so the PR's files never land in
   # an un-ignored path of this checkout), named by its SHA so a re-run after a
   # force-push neither collides with a stale branch nor reuses an old worktree.
   git fetch origin "pull/$PR/head"
   ROOT=$(git rev-parse --show-toplevel)
   TMP=$(mktemp -d)
   WT="$TMP/weekly-deps-pr-$PR-$(git rev-parse --short FETCH_HEAD)"
   git worktree add --detach "$WT" FETCH_HEAD
   cd "$WT/$DIR"   # then install deps and run the commands below
   ```

   After the tests, drop the worktree by absolute path (each agent command may run in a fresh shell, so substitute the printed values if `$ROOT`, `$WT` and `$TMP` are gone): `cd "$ROOT" && git worktree remove --force "$WT" && rmdir "$TMP"`.

   Use the repo's configured per-directory commands, read from `.weekly_deps.local_test_cmds` (a map of manifest-directory → command list). When unset for a directory, fall back to a sensible per-ecosystem default for that PR's detected ecosystem:

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

   **4b. For a bundled-engine bump, RUN the register's probes against the new
   engine.** Step 2b establishes which contracts are exposed and which probe
   covers each. That verifies the instrumentation *exists*; it does not verify
   the instrumentation *observed anything*. "Probe armed" is a property of the
   codebase, not evidence about this bump, and a walk that ends there reports
   "exposed, covered" when the truthful statement is "exposed, no evidence in
   either direction" (§ Special rule: bundled-engine PRs). Skip 4b only when the
   repo keeps no register — and say so in Step 5.

   **This step's gate is its own, not step 4's.** It applies to any bundled-engine
   bump, including one the tier table classified A or B — a database or TURN image
   tag bump moves an engine even though it never touches buildable code.

   These contracts only exist in a real engine, so the run is the app, not a
   suite. Run it from the PR-head worktree above (probes run against the current
   checkout measure the OLD engine), using the repo's own dev launcher with enough
   clients to exercise the contract. Three things must be true before a reading
   means anything:

   1. **You are running the bumped engine — the one THIS bump moved.** Check the
      engine named in Step 1's delta, at full patch precision, not whichever is
      easiest to read. Without this the step passes against the engine you already
      had.
      - Electron/Chromium → `process.versions.chrome` in the renderer console
      - Node runtime → `process.versions.node` in the main-process console
      - a bundled native library → the version as *installed*, not as declared
        (e.g. `npm ls <pkg>`)
      - a container image tag → the digest actually running,
        `docker inspect --format '{{.Image}}' <container>`, since a tag can be
        re-pointed under you
   2. **The log level shows the verdict lines.** If the probes log at debug
      level, a default console filter (Chromium DevTools hides Verbose by
      default) *guarantees* you see nothing, and rule 3 is what stops that from
      being read as a pass.
   3. **The pass is the PRESENCE of a verdict line, never the absence of a
      failure.** Silence is also produced by a healthy session before the probe
      fires, by a retry branch that logs nothing, by too little traffic for the
      probe's minimum sample, and by the handler never running at all. Exactly
      one of those is a pass. Where a healthy run emits nothing by design, get a
      **positive witness** first (e.g. a counter the production code path stamps
      on every observation, read back non-zero) — a witness that builds its own
      instance of the detector proves the module works, not that the shipped
      path is running.

   For each row Step 2b marked exposed, map the result onto exactly one verdict,
   following that register entry's "what proves it" column:

   ```text
   <register entry> → <how to exercise it, and for how long>
     PASS     <the positive verdict line or witness>
     BROKEN   <the failure line>
     NOT RUN  no verdict line at all (or a probe-side error, not an engine
              change). Never record this as clean.
   a contract that cannot be induced on a healthy machine (e.g. "a failed
     secure-storage write never clears a valid in-memory session")
     PASS     the failure was induced and the contract held
     BROKEN   the failure was induced and the contract broke
     NOT RUN  no failure could be induced on this machine. This is the honest
              answer, and it is NOT a pass.
   a row proved by inspection rather than observation
     PASS     the code path is intact; say that inspection is what you did
     BROKEN   it is missing
     NOT RUN  you did not look
   ```

   **Three row verdicts, and nothing else: PASS, BROKEN, NOT RUN.** The aggregate
   reported in Step 5 and in the Phase 6.1 summary is derived from them — it is not
   a fourth vocabulary:

   ```text
   n/a — not a bundled-engine bump   no row applied
   clean after real run              every exposed row PASS
   BROKEN: <row>, …                  any row BROKEN, whatever the others did
   NOT RUN                           no row reached a verdict
   PARTIAL: <row>=<verdict>, …       rows disagree and none is BROKEN
   ```

   Attach the specific probe lines to the PR — the lines, scrubbed, not a console
   dump: a verbose console can carry user and session identifiers. A bump whose
   probes have not run is not ready to recommend; say `NOT RUN` in Step 5 rather
   than reporting the walk alone, and derive the aggregate from the table above
   rather than choosing a word that feels right.

   **A probe row's recurring cost must be stated, not hidden.** A row that needs,
   say, ≥ 90 s of a two-client session runs on *every* future bundled-engine bump.
   That cost can be the point — it is the control a past miss lacked — but a row
   that quietly multiplies the probe's wall-clock cost without saying so repeats
   the same silent-drift failure one layer up. Record each row's pinned baseline
   (the engine versions it was last verified against) so the probe has something
   to compare to.

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
   Engine probes: <n/a — not a bundled-engine bump | clean after real run | BROKEN: … | PARTIAL: … | NOT RUN>
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
  Tier C:         N (#NN <pkg> — reviewed, local test passed)
                     engine probes: clean after real run | BROKEN: … | PARTIAL: … |
                                    NOT RUN | n/a — not a bundled-engine bump
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

### 6.3 Third-party attribution regeneration (optional)

If the repo keeps a legal attribution file (e.g. `NOTICE.md`, listing every
third-party component it distributes) and ships a script to regenerate it, that
file goes stale on **every** production dependency bump. Refreshed only by hand,
it drifts silently: many modules move between generations, and a direct
production dependency can go unattributed entirely. Skip this step if the repo
has no such file or script.

Run it whenever any merged PR touched a **production** dependency of a shipped
artifact. Dev-only bumps (`@types/*`, eslint, vitest, tsx) do not affect it — the
attribution file records the production bundles only. If the script reads
installed packages rather than lockfiles (license-checker style), install every
workspace it covers first (`npm ci` in each), then run it. Expect the same
dry-run-by-default / `--apply`-to-write contract as 6.2, but follow the script's
own docs.

**If it exits non-zero over a missing licence, stop and read the message.** The
commonest cause is a module linked into a shipped binary with no licence recorded
in the attribution file; others are a licence outside the compatibility allowlist,
or a field the table format cannot hold. Package tooling (e.g. `go list`) often
does not report a licence, and inventing one would assert a licence
determination, which is an audit action rather than post-merge bookkeeping.
Determine the licence from the module's own `LICENSE` file, add the row by hand,
and record the decision where the repo tracks licence decisions — do not let a
dependency-bump PR assert it in passing.

**Also check the licence column, not just the versions.** A bump only changes
attribution if upstream relicensed. Confirm by diffing the licence file across
the two versions rather than trusting the recorded string:

```bash
diff "$(go env GOMODCACHE)/<module>@<old>/LICENSE" \
     "$(go env GOMODCACHE)/<module>@<new>/LICENSE"
```

If it differs, that is exactly what the attribution file exists to record and it
needs a human decision — surface it rather than regenerating over it.

If the script writes changes, create a follow-up PR:
- Branch name: e.g. `chore/notice-refresh-<date>` (no `feature/` prefix — post-merge bookkeeping)
- Title: e.g. `docs: regenerate NOTICE.md after weekly dep bumps`
- Body: the added/removed components and the version bumps, plus an explicit
  statement that no licence changed (or which did)

Prompt the developer before pushing, same gate as 6.2.

> **Regenerate on the same npm version CI uses, and expect warnings you cannot
> reproduce otherwise.** With range-valued `overrides`, whether `npm ci` honours
> the lockfile pin or re-resolves the range can depend on the npm version: two
> npm minors on the same commit can install different patch versions of one
> package. If a CI drift guard emits a warning you cannot reproduce locally, this
> is why — match the workflow's Node version rather than regenerating against a
> different npm and pushing a file CI will disagree with.

> **A CI drift guard does not do this for you, by design.** A well-shaped guard
> FAILS only on an attribution defect — a shipped component with no row, a row for
> something no longer shipped, a changed licence, or a count that disagrees with
> its own table — and emits ordinary version drift as a warning, because blocking
> it would turn every routine Dependabot bump red for no compliance gain. Closing
> that warning is this step's job.

### 6.4 Rules-doc impact

If any Tier C merge changed a behavior covered by a repo rules doc (if the repo keeps any), the rules doc may need an update. Surface this explicitly with file path and the specific section that may be stale; do not silently update the rules doc — the developer should review and decide.

### 6.5 Post-merge cleanup handoff

The dependabot branches are deleted server-side by `--delete-branch`. Per operational discipline, do NOT attempt local cleanup commands in this agent session — they may be blocked or self-destructive in worktree context. Print a copy-paste block for the developer's primary terminal:

```
✅ Weekly dep PRs processed. Local cleanup — run in your PRIMARY checkout,
   NOT the agent's worktree:

  git -C "<primary-checkout>" worktree remove "<agent-worktree-path>" &&
  cd "<primary-checkout>" &&
    git fetch origin <default-branch> &&
    git merge --ff-only origin/<default-branch> &&
    # Prune any local dependabot/* branches you may have checked out:
    git branch | grep '^[^*]*dependabot/' | xargs -n1 git branch -D 2>/dev/null
  # If a test worktree was left behind (a run that stopped early), also run:
  #   git worktree list    # then: git worktree remove --force <its path>
```

Substitute the real primary-checkout path, the agent worktree path and the default-branch name before printing — never leave the placeholders.

**Do NOT emit a `git checkout <default-branch>` line, and do name the primary checkout explicitly.** The developer routinely runs this block in a terminal already sitting in the agent's worktree, and from there that command does not merely misfire — it *fails*, because git allows a branch in exactly one worktree at a time:

```
fatal: 'main' is already used by worktree at '<primary-checkout>'
```

Everything after it is then skipped, so the developer gets no update, no prune and no worktree removal out of a block that looked like it ran. The line is unnecessary anyway: the primary checkout is already on the default branch — that is *why* the worktree cannot check it out.

**Keep the `&&` chain — the chain was never the bug.** An earlier revision of this section read as though `&&` were at fault because the steps after it were skipped. They were skipped correctly; the defect was a first command that *always* failed, not the operator that honoured it. With a `cd` in front, which succeeds whenever the path is right, the chain is what makes the block **fail closed**: a mistyped path leaves the shell where it was, and unchained the following update and branch-prune would run against whatever repository that happens to be — frequently the agent's own worktree. (A `#` comment between chained lines parses fine in both bash and zsh; it does not break the continuation.)

**Update the default branch with `git fetch origin <default-branch> && git merge --ff-only origin/<default-branch>`, never `git pull`.** The primary checkout often carries other sessions' uncommitted files, and with `pull.rebase=true` (a common setting) `git pull` refuses on any unstaged change, even when the incoming commits touch none of those files. A fast-forward `merge` refuses only when an incoming change overlaps a dirty file. Never stash or commit those files to make the update pass. The `/dev-lifecycle` merge-phase cleanup block has the same rule and the full reasoning.

**Order matters: the worktree removal comes BEFORE the branch prune, not after.** Git refuses to delete a branch still checked out in a linked worktree (`Cannot delete branch 'x' checked out at '<path>'`), so a block that prunes first and removes second can silently leave branches behind. `git -C` keeps the removal runnable from any directory, including from inside the worktree being removed.

**Emit that line EXECUTABLE and first when this run used a worktree, and omit it entirely when it did not.** Both alternatives are wrong: *commented out as a conditional*, where a skipped line lets the prune fail on exactly the case the comment warned about; and *executable unconditionally*, where `git worktree remove` on a path that is not a worktree exits non-zero (`fatal: '<path>' is not a working tree`) and the `&&` aborts the update and the prune, making the whole block a no-op. You generate this block per run and already know which case applies — decide it at print time, as you already do for the worktree path. (The trailing `git worktree list` hint is different: it is for a Phase 4.C test worktree a stopped run may have left behind, which this run cannot know about, so it stays a comment after the chain.)

**Keep the double quotes around every substituted path.** A checkout path may legitimately contain a space (`~/Dev/My Project` is an ordinary repository name), and unquoted it word-splits or globs, so `cd` can land in the wrong directory or fail. The two cases differ and the difference is the whole point of the chain: if `cd` **fails**, `&&` stops everything after it, which is the safe outcome; if it **succeeds in the wrong directory**, the update and the prune run there. That second case is the exact failure this section exists to prevent, so do not reintroduce it one line up. Substituting a path into a literal shell command is text interpolation, which makes the quoting the generator's job, not the developer's.

**What those quotes do and do not cover — do not describe them as an escaping contract.** They stop word-splitting and globbing, which is the whole of the realistic failure here. They do NOT neutralise `$`, a backtick, a backslash or an embedded double quote, every one of which stays live inside double quotes. That gap is accepted rather than closed: the value is the developer's own checkout path, on their own machine, pasted into their own shell, so a path crafted to exploit it is a developer attacking themselves. The rule changes the moment the source changes — if a path ever arrives from anywhere untrusted, quoting is not sufficient and it needs real escaping or an outright rejection.

Suggest `--force` only if the removal refuses over build output the agent installed (`node_modules` from an attribution or lockfile regeneration); gitignored files do not block removal but untracked ones do, so read what it names before forcing.

**Print the `git status` caution with it.** Several concurrent agent sessions often run against the same primary checkout, so the update there runs on top of another session's uncommitted work — it is a shared directory, not a clean one by default. If the fast-forward refuses because an incoming change overlaps one of those files, leave them alone and rerun the update once that session has committed.

---

## What This Skill Does NOT Do

- Does **not** enable GitHub's auto-merge feature — every merge is explicit and verified.
- Does **not** push branches or open PRs of its own except the optional tech-stack-doc version refresh follow-up (Phase 6.2) and the optional attribution-file regeneration (Phase 6.3), both gated on developer confirmation and only when the repo ships the relevant script.
- Does **not** modify a repo's rules docs based on dependency bumps — flags for developer review only.
- Does **not** merge Tier C PRs without dispatching the configured deep-review skill (or a direct specialist-suite dispatch as fallback if Copilot review was explicitly skipped or unavailable) first.
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
- Create any new PRs, branches, or commits. The tech-stack-doc version refresh (Phase 6.2) and the attribution-file regeneration (Phase 6.3) of interactive mode are skipped entirely in autonomous mode — those are the developer's call to make.
- Run subagent dispatches (the deep-review skill, or the direct specialist-suite dispatch) on Tier C PRs. The triage run only does the lighter inline auto-dismiss + finding-surface; the heavyweight subagent reconciliation stays interactive.
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
