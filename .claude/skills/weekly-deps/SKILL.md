---
name: weekly-deps
description: "Triage the weekly batch of bot-authored dependency PRs (Dependabot plus github-actions updates) by risk and confidence, extract the embedded engine delta on bundled-engine bumps (e.g. Electron/Chromium, Node/V8), request Copilot reviews when Copilot is actually available, route higher-risk items through the repo's configured deep-review skill, and process safer bumps first to reduce lockfile conflicts. Active ecosystems are auto-detected from the live PR diffs (optionally pinned via `.claude/repo-config.json` `.ecosystems`); tier package→group data and the security-sensitive overlay come from `.weekly_deps.*` in the same config. Supports interactive triage/merge for one repository or, with --org, for every repository of an owner, plus --mode=prep and --mode=triage for autonomous, non-merge weekly routines."
argument-hint: [pr-number | all | --org[=<owner>] | --mode=prep | --mode=triage]
allowed-tools:
  - Bash(gh pr *)
  - Bash(gh api *)
  - Bash(gh run *)
  - Bash(gh issue *)
  - Bash(gh repo *)
  - Bash(gh search *)
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
- **`--org`** (or `--org=<owner>`) → discover open bot PRs in every repository of the owner and process them repository by repository (interactive). Read § Multi-repo mode before Phase 0.
- **`--mode=prep`** → autonomous prep run intended for a scheduled cron. Discovers, categorizes, runs community-signal lookup, requests Copilot reviews, sends `@dependabot rebase` on conflicts, writes a digest to the pinned issue, then exits. Does NOT merge anything. Does NOT schedule the triage run — triage has its own independent cron (see Schematic below). Read `references/autonomous-mode.md` before proceeding.
- **`--mode=triage`** → autonomous follow-up running on an independent cron. Picks up Copilot reviews that have landed since prep, auto-dismisses style nits, surfaces real findings into the same pinned-issue comment. Does NOT merge anything. Read `references/autonomous-mode.md` before proceeding.

> **Config-driven, ecosystem-auto-detecting.** This skill's engine (risk tiers A/B/C/D, confidence assessment, lockfile-contention ordering, the prep/triage cron flow) is generic. The repo-specific knowledge — which package ecosystems are in play, which packages map to which tier, which packages are security-sensitive, where the deep review goes, the cron schedule — is **data**, read from the OPTIONAL per-repo `.claude/repo-config.json` via `scripts/repo-config.sh` (`rc_get '<jq-filter>' [default]`, `rc_has '<jq-filter>'`). Active ecosystems are **auto-detected** from the live PR diffs (Phase 1.5); `.ecosystems` only pins/overrides that set. With an absent config (or empty `.weekly_deps.*`), the skill degrades safely: every bump defaults to **Tier C** (manual deep review), the security-sensitive list is the shared universal default, and Tier D yields nothing. See `.claude/repo-config.reference.md` for the full schema.

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

# Label → tier rules, bundled-engine additions, lockstep sets (all OPTIONAL, all default empty).
LABEL_RULES="$(rc_get '.weekly_deps.label_rules' '[]')"
BUNDLED_ENGINES_EXTRA="$(rc_get '.weekly_deps.bundled_engines' '[]')"
LOCKSTEP_SETS="$(rc_get '.weekly_deps.lockstep_sets' '[]')"
```

The security-sensitive package set and the local-test commands are loaded on demand in the phases that need them (Phase 3.5 and Phase 4.C respectively) — see those phases.

### Multi-repo mode (`--org`)

Without `--org`, the skill works on one repository: the one `gh repo view` resolves in the current directory. With `--org`, it works on every repository of one owner that has open bot PRs. The engine is the same; four things change.

**1. Discovery is owner-wide.** `gh pr list` reads one repository, so use search. The two authors need separate calls (same AND-not-OR reason as Phase 1):

```bash
ORG="${ORG:-$OWNER}"   # --org=<owner>, else the owner of the current repository
gh search prs --owner "$ORG" --author app/dependabot     --state open --limit 1000 --json repository,number,title,labels,isDraft,url
gh search prs --owner "$ORG" --author app/github-actions --state open --limit 1000 --json repository,number,title,labels,isDraft,url
```

Group the union by `.repository.nameWithOwner`. Search returns only the fields listed above; read everything else per PR in the later phases. A result count equal to `--limit` means the list is truncated: say so and do not report the batch as complete. PRs that a repository's own App-authenticated workflow opens are found per repository with the label query in Phase 1.

**2. Every command names its repository.** Run Phases 1.5 to 6 once per repository, with `OWNER_REPO`, `OWNER` and `REPO` set to that repository. Every `gh pr`, `gh issue` and `gh run` command in this skill and its references already carries `-R "$OWNER_REPO"`, and Phase 0 sets `OWNER_REPO` in single-repo mode too, so copy the commands as written. Set `OWNER`, `REPO` and `OWNER_REPO` together; the `gh api "repos/$OWNER/$REPO/…"` calls read the first two. A command without `-R` reads the repository of the current directory, which is the wrong one for all repositories but one.

**3. Config is loaded per repository, from its default branch.** The helper (`rc_get`, `rc_has`) is loaded once, as above, from the current repository. Each repository's config comes from the API, which serves the default branch, so a PR branch cannot supply it:

```bash
RC_FILE="$(mktemp)"   # rc_get reads $RC_FILE; one file per repository
CONFIG_STATE=ok       # reset for every repository; only the lines below may change it
if ! ERR="$(gh api "repos/$OWNER_REPO/contents/.claude/repo-config.json" -H 'Accept: application/vnd.github.raw' 2>&1 >"$RC_FILE")"; then
  case "$ERR" in
    *'HTTP 404'*) echo '{}' >"$RC_FILE" ;;                # no config: the safe defaults apply
    *) echo "CONFIG UNREADABLE for $OWNER_REPO: $ERR" >&2; echo '{}' >"$RC_FILE"; CONFIG_STATE=unreadable ;;
  esac
fi
jq -e 'type == "object"' "$RC_FILE" >/dev/null 2>&1 || { echo '{}' >"$RC_FILE"; CONFIG_STATE=unreadable; }
```

Then run the `rc_get` lines of Phase 0 again for this repository. Only a 404 means "no config". For any other failure, or for content that is not a JSON object, set `CONFIG_STATE=unreadable`: every PR of that repository is Tier C with confidence UNKNOWN, and the repository's table says the config could not be read. Never apply one repository's tier data to another repository.

**4. Tables, contention and follow-ups are per repository.** Show one categorization table per repository, with the repository name in its header, then one combined summary. Lockfile contention is counted per repository and manifest directory: two PRs contend only when the repository, the directory and the lockfile are all the same, so PRs in different repositories never wait for each other. A PR that changes several directories contends in each of them. Steps that need a working tree (the local tests of Phase 4.C step 4 and 4b, and Phases 6.2, 6.3 and 6.5) run only for a repository that has a local checkout. Without a checkout, record `NOT RUN (no local checkout)`. This is never a pass.

`--org` is interactive only. `--mode=prep` and `--mode=triage` stay per repository, because each repository has its own pinned digest issue and its own cron.

### Reading PR state when the token is limited

Some tokens cannot read every field this skill needs. A fine-grained token can lack access to the GraphQL `statusCheckRollup` field (then `gh pr list -R "$OWNER_REPO" --json statusCheckRollup` and `gh pr checks` both fail), can get `403` from the REST check-runs and commit-status endpoints, and can fail on `gh pr view --json reviewRequests` when a team is a requested reviewer. Each of these failures is silent if the command's output is used without a check. Use the two helpers below wherever a phase needs CI state or the reviewer list. In both, a read that fails gives `unknown`. It never gives `green`, `0` or "none".

**Shell state does not carry between tool calls.** Each Bash call an agent makes can start a fresh shell, so the Phase 0 variables, `rc_get` and these two functions are gone in the next call. Run the Phase 0 block, then this block, at the top of every Bash call that uses any of them, in the same call. A helper that is not defined prints nothing, and that is why neither helper ever uses empty output as an answer: `pr_ci` always prints a status word and a `source:` line, and `pr_requested` always prints at least one line. **Empty output from either helper means it did not run: treat it as `unknown`.**

```bash
# pr_ci <pr> → green | red | pending | missing | unknown, then the source that answered.
# Three sources, tried in order. A source that errors is skipped, never read as "no checks".
pr_ci() {
  local pr="$1" wf out sha
  wf="$(rc_get '.weekly_deps.ci_workflow' '')"   # empty → no presence check
  # 1. gh pr checks (GraphQL statusCheckRollup): every check on the head commit.
  if out="$(gh pr checks "$pr" -R "$OWNER_REPO" --json bucket,workflow 2>/dev/null)" && jq -e 'type == "array" and length > 0' <<<"$out" >/dev/null 2>&1; then
    jq -r --arg wf "$wf" 'if ($wf != "" and ([.[] | select(.workflow == $wf)] | length) == 0) then "missing"
      elif ([.[].bucket] | any(. == "fail" or . == "cancel")) then "red"
      elif ([.[].bucket] | all(. == "pass" or . == "skipping")) then "green" else "pending" end' <<<"$out"
    echo "source: pr-checks"; return 0
  fi
  sha="$(gh api "repos/$OWNER_REPO/pulls/$pr" --jq .head.sha 2>/dev/null)" || sha=""
  [ -n "$sha" ] || { echo unknown; echo "source: none (head SHA unreadable)"; return 0; }
  # 2. REST check-runs plus the combined commit status. Source 1 covers both kinds; here
  #    they are two endpoints, and both must be read: CI that reports through the status
  #    API (many non-Actions CI services) is invisible to check-runs.
  if out="$(gh api --paginate --slurp "repos/$OWNER_REPO/commits/$sha/check-runs?per_page=100" 2>/dev/null)"; then
    local st; st="$(gh api "repos/$OWNER_REPO/commits/$sha/status" --jq '"\(.state) \(.total_count)"' 2>/dev/null)" || st="unreadable"
    jq -r --arg wf "$wf" --arg st "$st" '[.[].check_runs[]] as $c
      | ($st | split(" ")) as [$state, $n]
      | if ($state == "failure" or $state == "error") then "red"
        elif ($c | any(.conclusion == "failure" or .conclusion == "timed_out" or .conclusion == "cancelled" or .conclusion == "action_required" or .conclusion == "startup_failure")) then "red"
        elif ($c | any(.status != "completed")) then "pending"
        elif ($state == "pending" and ($n // "0") != "0") then "pending"
        elif ($c | length) == 0 and ($n // "0") == "0" then (if $state == "unreadable" then "unknown" else "missing" end)
        elif $state == "unreadable" then "unknown"
        elif ($c | all(.conclusion == "success" or .conclusion == "skipped" or .conclusion == "neutral")) then (if $wf == "" then "green" else "green-unconfirmed" end)
        else "pending" end' <<<"$out"
    echo "source: check-runs+status"; return 0
  fi
  # 3. Actions workflow runs for the head SHA: works where 1 and 2 are refused, but it
  #    sees GitHub Actions only. Checks that other Apps report are invisible here.
  if out="$(gh api --paginate --slurp "repos/$OWNER_REPO/actions/runs?head_sha=$sha&per_page=100" 2>/dev/null)"; then
    jq -r --arg wf "$wf" '[.[].workflow_runs[]] as $r
      | if ($r | length) == 0 then "missing"
        elif ($wf != "" and ([$r[] | select(.name == $wf)] | length) == 0) then "missing"
        elif ($r | any(.conclusion == "failure" or .conclusion == "timed_out" or .conclusion == "cancelled" or .conclusion == "action_required" or .conclusion == "startup_failure")) then "red"
        elif ($r | any(.status != "completed")) then "pending"
        elif ($r | any(.conclusion == "success")) then "green" else "missing" end' <<<"$out"
    echo "source: actions-runs"; return 0
  fi
  echo unknown; echo "source: none (all three reads failed)"
}

# pr_requested <pr> → one requested reviewer per line (users, then teams as team:<slug>),
# or "none" when the list was read and is empty, then a last line "source: graphql" or
# "source: rest". When neither read worked it prints the single word "unknown".
# Empty output never means "none": it means the function did not run.
pr_requested() {
  local pr="$1" out
  if out="$(gh pr view "$pr" -R "$OWNER_REPO" --json reviewRequests --jq '.reviewRequests[] | (.login // ("team:" + (.slug // .name // "")))' 2>/dev/null)"; then
    printf '%s\n' "${out:-none}"; echo "source: graphql"; return 0
  fi
  if out="$(gh api "repos/$OWNER_REPO/pulls/$pr/requested_reviewers" --jq '(.users[].login), (.teams[] | "team:" + .slug)' 2>/dev/null)"; then
    printf '%s\n' "${out:-none}"; echo "source: rest"; return 0
  fi
  echo unknown
}
```

How to read the result of `pr_ci`:

- `source: rest` from `pr_requested` can miss a requested bot reviewer such as Copilot, because the REST endpoint does not handle bots the way GraphQL does. A REST result can show that Copilot **is** requested; it can never show that Copilot is **not** requested. Treat "Copilot absent, source: rest" as unknown.
- `green-unconfirmed` (source 2 with `weekly_deps.ci_workflow` set) means all check runs passed, but a check run does not carry its workflow name, so the required workflow is not confirmed. Confirm it with source 3 (a completed, successful run whose `name` equals the configured workflow). If that read fails, the result is `unknown`.
- `green` from `actions-runs` covers GitHub Actions only. Print the source beside the status (`green (actions-runs)`) so the reader knows that checks from other Apps were not seen. Keep that green only if `gh api "repos/$OWNER_REPO/pulls/$PR" --jq .mergeable_state` reads successfully as `clean`, `behind` or `has_hooks`. Any other value (`unstable`, `blocked`, `dirty`, `unknown`, `draft`), empty output, or a failed read means a check this source cannot see may be failing or pending: the result is `unknown`.
- Several runs can exist for one head SHA. A failed run stays in the list after a new run passes, so this source can say `red` for a PR that is now green. That error is in the safe direction. Do not filter failed runs out to get a green result.
- `unknown` is not `missing`. `missing` means a read worked and found no checks. `unknown` means no read worked. Both skip the PR, and both are reported with their own word.

The same rule applies to every other read in this skill: when a command exits non-zero or returns something that does not parse, the value is unknown. Do not replace it with an empty list or a zero.

---

## Choosing the right workflow

`/weekly-deps` is the entry point for **batch-mergeable bot PRs** — dependabot bumps and any github-actions security-data refresh. These share three structural properties that distinguish them from human-authored PRs:

1. **No GitHub issue** behind them — they're not part of the milestone/phase tracker.
2. **Reproducible from upstream** — closing and re-running the bot regenerates an equivalent PR, so the cost of skipping or rejecting is low.
3. **Touch the same lockfiles** — multiple open PRs in the same directory contend for the same lockfile (`package-lock.json` / `go.sum` / etc.), so merge order matters even when each individual PR is mergeable in isolation.

Use the repo's feature/refactor lifecycle skill for issue-tied, single-author work. For dependency PRs, `/weekly-deps` is the orchestrator — but for Tier C PRs (Phase 4.0 / 4.C), it dispatches into the repo's configured deep-review skill (`.weekly_deps.deep_review_skill`, default `/enhanced-pr-review`) to reconcile any Copilot review Phase 3.6 obtained (Copilot is frequently unavailable — see the 3.6 precondition) with the specialist subagent suite that the deep-review skill dispatches directly. The split is: `weekly-deps` owns the discovery, categorization, confidence-based Copilot routing, lockfile-aware merge order, and tech-stack-doc follow-up; the deep-review skill owns the deep-review triage on individual Tier C PRs.

Use `/review-pr` standalone only as a fallback when Phase 3.6's Copilot request was declined for a specific PR and that PR is still Tier C — without a Copilot review present, `/enhanced-pr-review` has less to reconcile and a direct `/review-pr` is more efficient.

> **Extension point — label-based reclassification.** A repo may file dependency PRs that warrant a non-default tier based on a label (for example, a routine that opens transitive-dependency-hygiene PRs and tags them). Reclassifying such PRs by label is a supported extension point: declare the rule as data in `.weekly_deps.label_rules` (Phase 2 § Label rules), which maps the label to a tier and, if conservative-bump heuristics apply, to a demotion condition. The shared skill ships no repo-specific label rules.

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
   • Phase 3.7 record coverage           • Phase 4.0 auto-dismiss
   • Phase 5 rebase conflicts            • Step T4a re-check coverage
   • post digest comment
                                         • edit digest comment in place
   • exit                                • exit
                                                  │
   later that day (when the dev gets in) ─────────▼
   the developer runs `/weekly-deps` interactively
   • reads the pinned-issue digest
   • processes Phase 4 merges with confirmations
```

The two crons are **independent** — the prep run does NOT schedule the triage. If prep fails, the triage cron still fires and aborts cleanly with "no prep comment for this week" (Step T2 of `references/autonomous-mode.md`). This is more robust than the chained-schedule alternative.

Both autonomous modes share Phases 1–3 (Phase 3.7 records coverage), plus Phase 5 (rebase) in `prep`, and Phase 4.0 (auto-dismiss only, no merges) and the Step T4a coverage re-check in `triage`. Phases 4.A–D and 6.2 (the merge and follow-up PR steps) are **never run in autonomous mode** — they require human confirmation per the operational discipline.

---

## Phase 1: DISCOVER

Enumerate open bot-authored PRs. Don't trust the user's pasted list — re-fetch live state.

```bash
# Dependabot PRs (all ecosystems dependabot manages for this repo)
gh pr list -R "$OWNER_REPO" \
  --search "author:app/dependabot is:open" \
  --json number,title,labels,headRefName,mergeable,mergeStateStatus,files,isDraft,createdAt,updatedAt \
  --limit 50

# Other bot-authored PRs (github-actions for security-data refreshes etc.)
gh pr list -R "$OWNER_REPO" \
  --search "author:app/github-actions is:open" \
  --json number,title,labels,headRefName,mergeable,mergeStateStatus,files,isDraft,createdAt,updatedAt \
  --limit 20

# Workflow-opened PRs authored by a GitHub App (OPTIONAL — only if the repo has such a
# routine). The author queries above miss them; query by the label the workflow applies.
gh pr list -R "$OWNER_REPO" \
  --search 'label:"<routine-label>" is:open' \
  --json number,title,labels,headRefName,mergeable,mergeStateStatus,files,isDraft,createdAt,updatedAt \
  --limit 5
```

**Note on the search query:** the calls cannot be combined into `author:app/dependabot author:app/github-actions` — GitHub treats multiple `author:` qualifiers as AND, not OR, which returns empty. Always use separate calls and union the results.

**These calls do not ask for `statusCheckRollup`, `reviews` or `reviewRequests`.** One PR that the token cannot read those fields for makes the whole list call fail, and then no PR is discovered. CI state and requested reviewers are read per PR with `pr_ci` and `pr_requested` (§ Reading PR state when the token is limited); reviews are read per PR in Phases 3.6 and 4. If a list call still fails, drop `mergeable,mergeStateStatus,files` too and read them per PR; do not continue with a partial list.

Re-entrant detection still depends on reviewers and reviews: if Copilot has already been requested (Phase 3.6) or has already reviewed (Phase 4.0), the second invocation of the skill picks that up from the per-PR reads and routes accordingly.

In `--org` mode the list comes from the owner-wide search (§ Multi-repo mode); run the label query below per repository only where a repository has such a routine.

The second `gh pr list` call (the `author:app/github-actions` query) also catches PRs opened by repo workflows via PR-creation actions (e.g. `peter-evans/create-pull-request`) using the default `GITHUB_TOKEN` — including any label-driven reclassification extension a repo adds (see "Choosing the right workflow" above). A workflow that authenticates as its own GitHub App instead (so PR CI runs on its PRs without a manual re-trigger) is **not** `app/github-actions`, so only the third, label-based call catches its PRs. Phase 2 applies any such repo-defined label rule.

If the user passed `$0` as a specific PR number, narrow to just that PR via `gh pr view $0 -R "$OWNER_REPO" --json ...` and skip directly to Phase 2 for that one PR.

If zero open bot PRs: report "No open bot-authored PRs. Nothing to triage." and exit.

---

## Phase 1.5: DETECT ACTIVE ECOSYSTEMS

Derive the active ecosystem set from the **live PR diffs**, not from a hardcoded list. For each discovered PR, list its changed files and map each manifest to an ecosystem; the union across all PRs is the active set for this run.

```bash
# For each PR number from Phase 1:
gh pr diff $PR -R "$OWNER_REPO" --name-only
```

Map each touched manifest path to an ecosystem (basename-based, directory-agnostic):

| Manifest file (basename) | Ecosystem |
|---|---|
| `package.json` / `package-lock.json` / `npm-shrinkwrap.json` / `yarn.lock` / `pnpm-lock.yaml` | `npm` |
| `go.mod` / `go.sum` | `gomod` (advisory ecosystem `go`) |
| `.github/workflows/*.yml` (action refs) | `github-actions` (advisory ecosystem `actions`) |
| `requirements*.txt` / `Pipfile*` / `pyproject.toml` / `poetry.lock` / `uv.lock` | `pip` |
| `Cargo.toml` / `Cargo.lock` | `cargo` |
| `Gemfile` / `Gemfile.lock` | `rubygems` (Dependabot name `bundler`) |
| `composer.json` / `composer.lock` | `composer` |
| `build.gradle` / `build.gradle.kts` / `settings.gradle*` / `gradle/libs.versions.toml` / `gradle/wrapper/gradle-wrapper.properties` | `gradle` (advisory ecosystem `maven`) |
| `Dockerfile*` / `*.dockerfile` / `docker-compose*.yml` / `compose*.yml` (image tags and digests) | `docker` (Dependabot names `docker` and `docker-compose`; no advisory ecosystem) |

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

The four tiers are a fixed *concept*; the package→group→tier *data* is per-repo and lives in `.weekly_deps.tiers` (loaded as `TIERS_JSON` in Phase 0). Each tier letter (`A`, `B`, `C`) maps rule names to rules. The rule name is a label for the table; the fields decide the match:

```json
{
  "A": {
    "lint and format": { "groups": ["lint"], "dependency_type": "development", "max_bump": "minor" },
    "type stubs only": { "groups": ["ui-framework"], "packages": ["@types/*"], "max_bump": "minor" },
    "dev singles":     { "ungrouped": true, "dependency_type": "development", "max_bump": "minor" }
  },
  "B": {
    "runtime libs":    { "groups": ["runtime"], "max_bump": "minor" },
    "ci actions":      { "ecosystems": ["github-actions"], "groups": ["actions"], "max_bump": "minor" }
  },
  "C": {
    "build and signing": { "groups": ["packaging"] }
  }
}
```

| Field | Type | A rule matches a PR only when |
|---|---|---|
| `groups` | string[] | the PR's Dependabot group name is in the list. Read the name from the title: `the <group> group` appears in both forms, `bump the <group> group … with N updates` and `bump <pkg> from X to Y … in the <group> group` (a group that had one update). A PR whose title has neither form has no group |
| `ungrouped` | bool | `true`: the PR bumps one package and has no Dependabot group. A one-update PR that names a group is grouped, not ungrouped |
| `packages` | glob[] | **every** bumped package matches a glob. One member outside the list means no match |
| `ecosystems` | string[] | the PR's detected ecosystem (Phase 1.5) is in the list |
| `directories` | glob[] | **every** manifest directory the PR changes matches a glob. A Dependabot group with `directories:` can change several (`… across 3 directories`) |
| `dependency_type` | `development` \| `production` | every bumped package is of that type in the manifest the PR changes (for npm, the changed line is under `devDependencies` for `development`, and under `dependencies` or `optionalDependencies` for `production`). If the type cannot be read from the diff, or the ecosystem has no such split, the rule does not match |
| `max_bump` | `patch` \| `minor` | no bumped package moves more than this. Default `minor`. A tier C rule ignores it. No value admits a major bump |

All fields that a rule gives must hold. A rule in tier A or B that gives none of `groups`, `ungrouped` and `packages` is ignored, because it would match every PR. An unknown field makes the rule not match; do not guess its meaning.

To classify a PR: test it against every rule. If rules of more than one tier match, the higher-risk tier wins (C, then B, then A). If no rule matches, the PR is Tier C. Then apply the overrides below, which no tier data can switch off. Show the matched rule name in the table, or `no rule` for the fall-through, so the developer can see why a PR is in its tier.

The group name and the package list are separate tests on purpose. A group can hold members of different risk: a rule with `groups` and `packages` together admits the group only while all its members are in the low-risk list, and the same group with one other member falls through to Tier C.

**With `.weekly_deps.tiers == {}` (the safe default), no package matches any group, so every bump falls through to Tier C (manual deep review).** This is intentional: a repo that hasn't declared its tier data gets conservative, never-fast-merged behavior rather than an unsafe fast-track.

**The major-bump override is universal and always applies, regardless of config:**

> **Any major-version bump (`X.Y.Z → X+1.0.0`) → Tier C**, regardless of which group the package belongs to. The dependabot group label / config group is a *starting* signal, not the final answer. Always check the version shape before final tier assignment.

For grouped PRs (`chore: bump the X group ...`), extract the individual package names and versions from the PR body bullet list (`gh pr view "$PR" -R "$OWNER_REPO" --json body --jq '.body'`). Match the tier rules **once, against the whole PR**: `groups`, `ungrouped`, `ecosystems` and `directories` are properties of the PR, and `packages`, `dependency_type` and `max_bump` are tested against every member as their rows say. Do not classify members one at a time against `groups`, because a single member is not a grouped PR and would match nothing. The per-member reads feed the member-level checks: any major bump in the group → Tier C, any bundled-engine member → Tier C, any security-sensitive member → LOW confidence.

How to read the bump shape of each member:

- `X.Y.Z` with `X ≥ 1`: a change in `X` is major, in `Y` minor, in `Z` patch.
- `0.Y.Z`: a change in `Y` is **major** and a change in `Z` is **minor**. Below 1.0, semver lets a minor break, so this shifts every 0.x change up one step. `0.0.Z` to `0.0.Z'` is major.
- A pre-release version (`-rc.1`, `-beta`) on either side is major.
- A GitHub Actions bump pinned by commit SHA is parseable only from the version Dependabot states for it (the `# vX.Y.Z` comment in the diff, or "from vA to vB" in the body), and only once Phase 3.3b confirms that the SHA resolves to that tag. Until then, or if 3.3b fails, the shape is unparseable.
- Anything else (a date tag, a branch name, a bare SHA with no stated version, a Docker tag that is not a version) is unparseable: do not count it as patch or minor. The PR is Tier C with confidence UNKNOWN.

### Label rules

`.weekly_deps.label_rules` (loaded as `LABEL_RULES`) reclassifies PRs by label. This is for PRs that a repo routine opens, such as a transitive-dependency refresh that moves many modules at once and so does not fit a per-package rule.

```json
[ { "label": "<routine-label>", "tier": "C", "demote_to": "B", "demote_max_packages": 4, "authors": ["<your-ci-app>[bot]"] } ]
```

A label rule applies only when the PR carries `label` **and** its author is a bot or GitHub App: the author login ends in `[bot]`, or is listed in the rule's optional `authors` array. Read the author per PR (`gh pr view "$PR" -R "$OWNER_REPO" --json author --jq .author.login`); if it cannot be read, the rule does not apply. A label is a weak signal on its own, because anyone with triage rights can add one to any PR, so a labelled PR from any other author is classified as if it had no label.

When a label rule applies, its tier takes part in the normal comparison: the PR's tier is the highest-risk tier among the label rule and every tier rule that matches. The "no rule matches → Tier C" fall-through does not apply, because the label rule is a match. `demote_to` lowers only the label rule's own tier, and only when all of these hold: the diff changes at most `demote_max_packages` packages or modules, no change is a major bump, and no changed package matches the security-sensitive list (Phase 3.5). So a matching tier C rule still wins over a demoted label rule, and no label rule moves a PR below the tier that an override gives it. If the PR body has a self-review checklist, say so in the table and read it before any subagent review.

### Lockstep sets: packages that must move together

Some packages pin another package at an **exact** version, usually through an exact `peerDependencies` entry. A test runner and its coverage or browser plugin are the common case: the plugin at version N accepts the runner at version N only. Dependabot can still open one single-package PR per member, for example when a security update covers one member, or when a grouped PR was split after a conflict.

Each of those PRs can be green alone, or can be red only because of the pair. If one is merged without the other, the default branch holds versions that the peer range refuses: a clean install fails or resolves a different tree than the lockfile, and the second PR must be rebuilt against a broken base.

Declare the sets in `.weekly_deps.lockstep_sets` (loaded as `LOCKSTEP_SETS`), one list of globs per set:

```json
[ ["vitest", "@vitest/*"] ]
```

The rule, for each manifest directory:

1. Collect the open PRs that bump a member of one set in that directory.
2. If one PR bumps every member that the manifest has, to matching versions, it is a normal PR. Classify it as usual.
3. If the bumps are split across PRs, or one PR bumps only some members, **do not merge any of them one at a time**. Mark each `HOLD (lockstep: <set>)`. The hold applies at every tier and every confidence, and Phase 4.A never batch-merges a held PR.
4. Resolve the hold in one of these ways, and let the developer choose: wait for the grouped PR that carries the whole set and close the split PRs in its favor; or comment `@dependabot recreate` on the grouped PR after the split ones are closed; or, if the repo has no group for the set, report that its Dependabot config needs one (and a second group with `applies-to: security-updates`, because a group without that key does not apply to security updates) and leave the PRs open.
5. A red check on a split PR whose log shows a peer-dependency resolution error (`ERESOLVE`, `peer … @"<exact version>"`) is this case. Do not ask for `@dependabot recreate` on that PR alone; it will be red again.

Without config, treat this as a suspected lockstep set and flag it for the developer: two single-package PRs in one directory whose packages share a scope or a name stem and move between the same two versions. A suspicion holds the PRs the same way until the developer says the packages are independent.

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
| Any `docker` ecosystem bump (base images in a `Dockerfile`, service images in a compose file) | the image contents: OS packages, language runtime, or server |
| Mobile framework packages that ship a JavaScript engine or native runtime | the bundled JS engine and the native toolchain versions the release requires |
| Anything the repo lists in `.weekly_deps.bundled_engines` (package globs or Dependabot group names) | as the repo documents |

A `docker` bump is in the class by its ecosystem, not by its name: the tag is a label for contents that the diff does not show, and a tag can move to new contents under the same name. A digest-only refresh under an unchanged tag is still an engine change.

**Step 1 — extract the engine delta at FULL PATCH PRECISION.** Dependabot often already
prints it; nothing in the routine asks anyone to read it. In the digest, report the engine
line, not just the package line:

```bash
gh pr view "$PR" -R "$OWNER_REPO" --json body --jq .body \
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

Membership is whatever the repo declares under `.weekly_deps.tiers.A` (typically dev-tooling, build-tooling, test frameworks, type stubs — patch/minor bumps). With no config, nothing lands here. A tier A rule should set `dependency_type: "development"` unless every package it can match is known to stay out of shipped artifacts; without it, a production package that joins the group later is fast-tracked with the rest.

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
- its bump shape cannot be parsed, OR
- a label rule puts it here and its demotion condition does not hold (§ Label rules), OR
- the repository's config could not be read (`--org` mode, `CONFIG_STATE=unreadable`), OR
- **no rule matches it**, which includes the case where no tier data is configured (the safe default routes everything here).

An ecosystem that the repo's tier data does not mention gets no default tier from this skill. Its PRs match no rule and are Tier C until the repo adds a rule for them.

For a wrapper such as Electron, Tier C asks **two separate questions, and the second is the one that has caused an outage.** (a) *API security* — check release notes for `contextIsolation`, `sandbox`, `webPreferences` default changes against the repo's rules docs. (b) *Bundled-engine behavior* — extract the engine delta at full patch precision and walk the platform-behavior register, per § Special rule: bundled-engine PRs. (a) alone has passed consecutive bumps while the engine broke behavior underneath.

Tier C PRs get the deepest review path: Phase 3.6 requests a Copilot review, then Phase 4.0 / Phase 4.C dispatch the configured deep-review skill (`.weekly_deps.deep_review_skill`, default `/enhanced-pr-review <PR>`) via the Skill tool to reconcile Copilot's findings with the repo's specialist subagents (`.review_agents`). The specialist suite is dispatched inside the deep-review skill, so dispatching it again here would duplicate work.

### Tier D — Security Data (verify upstream integrity)

PRs that refresh static security-relevant **data**, not application code (threat-feed pulls, indicator/signature/blocklist refreshes, and similar). The review question for Tier D is "does the data come from the same authoritative source it claimed to come from in the previous refresh?" — not "is this code correct?" Check the diff is data-only, the source URL hasn't changed, and CI passed.

Tier D membership is config-driven via `.weekly_deps.tier_d_sources` (loaded as `TIER_D_SOURCES` in Phase 0) — a list of source descriptors, each identifying a security-data refresh (e.g. by author + label + the expected data-file path glob). A PR is Tier D when it matches one of these descriptors. **With `.weekly_deps.tier_d_sources == []` (the default), Tier D yields nothing** — repos without a security-data refresh workflow simply never produce a Tier D PR.

### Display the categorization table

After classifying, present to the developer (example shape; rows reflect whatever the repo actually has):

```
/weekly-deps — <owner>/<repo> — N open bot PRs   (ecosystems: <detected/pinned set>)
──────────────────────────────────────────────────────────────────────────
Tier A (Fast — batch-mergeable):              N PRs
  #NN <group> (K updates)                       [<manifest dir>]    HIGH    rule: <rule name>
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

Held (do not merge one at a time):
  #NN, #NN  — HOLD (lockstep: <set>)             [<manifest dir>]

Lockfile contention (process order matters; per repository and directory):
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
pr_ci "$PR"   # § Reading PR state when the token is limited
```

Do not call `gh pr checks` directly here: with a token that cannot read `statusCheckRollup` it fails, and a failed call must not be read as "no failing checks". `pr_ci` tries the three sources in order and names the one that answered.

- `green` → continue to 3.2. Keep the source with the status.
- `green-unconfirmed` → confirm the required workflow as described under the helper; unconfirmed is `unknown`.
- `red` → skip; suggest `@dependabot recreate` only if the failure looks like a transient or stale-lockfile issue (network blip, snapshot mismatch), and never for a lockstep hold (Phase 2 § Lockstep sets). Otherwise close the PR with an explanatory comment.
- `pending` → skip for this run; the developer can re-invoke after CI settles.
- `missing` → a read worked and found no checks at all, or none from the workflow named in `weekly_deps.ci_workflow` when that key is set. Classify as UNKNOWN and skip. For a routine-labelled PR opened by a workflow that authenticates as a GitHub App (Phase 1), CI should start on its own; if it is still missing, check that workflow's App-token mint step before falling back to a close/reopen.
- `unknown` → no read worked. Classify as UNKNOWN and skip. Report it as a token or API problem, with the PR number, so that it is not mistaken for a PR without CI.

### 3.2 Mergeable status

```bash
gh pr view $PR -R "$OWNER_REPO" --json mergeable,mergeStateStatus --jq '{mergeable, mergeStateStatus}' \
  || gh api "repos/$OWNER_REPO/pulls/$PR" --jq '{mergeable, mergeable_state}'   # REST: true/false/null and clean/behind/dirty/blocked/unstable
```

If both reads fail, or `mergeable` is `null` or `UNKNOWN` (GitHub has not computed it yet), the mergeable status is unknown: skip the PR for this run. Do not treat it as ready.

- `mergeable=MERGEABLE` and `mergeStateStatus=CLEAN` → ready.
- `mergeStateStatus=BEHIND` → behind the default branch; the rebase will happen during merge or via `@dependabot rebase`.
- `mergeable=CONFLICTING` → blocked; queue for Phase 5 (Conflict).

### 3.3 No surprise diff

```bash
gh pr diff $PR -R "$OWNER_REPO" --name-only
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
| gradle | `build.gradle*`, `settings.gradle*`, `gradle/libs.versions.toml`, `gradle.lockfile`, and for a wrapper bump `gradle/wrapper/gradle-wrapper.properties` (a changed `gradle-wrapper.jar` or `gradlew` script is a surprise diff unless the PR is a wrapper bump, and then the jar still needs its checksum verified) |
| docker | only the `Dockerfile*` or compose file(s) in the dependabot directory, and in them only image references |
| security-data refresh (Tier D) | only the data-file path(s) declared in the matching `.weekly_deps.tier_d_sources` descriptor |

**Surprise diffs require manual review.** If a `chore: bump` PR also touches files outside its expected footprint (workflows, source, anything else), escalate to Tier C / LOW confidence regardless of original classification. The surprise-diff signal feeds directly into Phase 3.5 (sets LOW) and Phase 3.6 will then request Copilot review; Phase 4.0/4.C dispatches the deep-review skill for the actual triage. This routing is one of the few defenses against a compromised bot or a malicious-typosquat scenario where a dependency's install script tries to land arbitrary content — the supply-chain concern needs the full review path, not a fast-track.

### 3.3b Pinned-ref upstream resolution (github-actions ecosystem only)

Skip this for PRs of other ecosystems — it applies to `.github/workflows/**` action bumps.

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

1. **GitHub Advisory Database** — `gh api "/advisories?ecosystem=<eco>&published=><date>"` filtered server-side by ecosystem and date, then version-range-checked client-side via jq's `.vulnerabilities[].package.name` field. The `<eco>` is the advisory-DB name for the PR's detected ecosystem (npm→`npm`, gomod→`go`, github-actions→`actions`, pip→`pip`, cargo→`rust`, rubygems→`rubygems`, composer→`composer`, gradle→`maven`; docker has no advisory ecosystem, so signal 1 is not available for it and the PR relies on the bundled-engine rule). The `/advisories` endpoint has no `package=` query parameter; client-side filtering is required. See `references/community-signal-lookup.md` for the full query.
2. **Release-notes keyword scan** — greps the dependabot-supplied release notes in the PR body for unambiguous yank/advisory markers ONLY: `yanked`, `do not upgrade`, `critical bug`, `hotfix needed`, `reverted in v<N>`, plus `CVE-YYYY-NNNN` and `GHSA-` identifiers. Bare `BREAKING` / `MAJOR` / `regression` keywords are deliberately NOT scanned — they fire too often in normal release notes (see History section in `community-signal-lookup.md`).
3. **Package repo issue spike** (only for packages matching `security-sensitive-packages.md`) — counts recent issues in the package's GitHub repo mentioning the new version + crash/regression keywords. Uses a static known-mapping from package name to GitHub repo (curl is not in `allowed-tools`); skipped for packages outside the known-mapping. A repo can extend the known-mapping via `.weekly_deps.signal3_repo_map`.

The lookup produces one record per PR with a `verdict` field that Phase 3.5 (confidence) consumes. Failure modes (API rate limit, non-GitHub repo, missing release tag) degrade gracefully to `UNKNOWN` rather than `HIGH` — a missing signal must never elevate confidence.

For grouped PRs (`chore: bump the X group ...`), the lookup runs once per package in the group. A LOW verdict on ANY package elevates the whole PR to LOW.

### 3.5 Confidence assessment

Combine the signals from 3.1–3.4 plus the tier from Phase 2 plus the bumped-package identity into a single confidence label per PR. The label drives Phase 3.6 (whether to request Copilot review) and Phase 4 (whether to fast-merge or hold for triage).

Confidence is biased toward false-positives. The cost of an unnecessary Copilot review is low (a few minutes of bot time the user already pays for via subscription); the cost of a missed regression in a security-sensitive bump is potentially much higher.

| Label | Conditions (ALL must hold) |
|---|---|
| **HIGH** | Tier A, AND CI green, AND no surprise diff, AND no lockstep hold, AND no bumped package matches the security-sensitive list, AND community-signal verdict is not LOW, AND the bump shape is one of: (a) every bumped package moves by a patch; (b) a **grouped** PR matched by a tier A rule with `dependency_type: "development"`, where every member moves by a patch or a minor |
| **MEDIUM** | Tier A with green CI + clean footprint whose bump shape is not (a) or (b) above: a **single-package** minor bump, or a grouped minor bump matched by a rule without `dependency_type: "development"`. OR Tier B with green CI + no breaking-change keywords in PR body + no security-sensitive package match + no community-signal LOW verdict |
| **LOW** | ANY of: Tier C; CI red or pending; surprise diff; breaking-change keywords (`BREAKING`, `MAJOR`, `deprecate`, `remove`, `behavior change`, `default changed`) in PR body; any bumped package matches the security-sensitive list; community-signal lookup returned LOW verdict (active GHSA/CVE against the bumped-to version, or strong-negative release-note keyword match); a github-actions PR whose Phase 3.3b pinned-ref upstream resolution failed or could not be completed |
| **UNKNOWN** | Missing signal — CI state `missing` or `unknown` (Phase 3.1), unparseable bump shape, unreadable repo config, community-signal lookup failed, or any pre-flight check returned an error. Treat as LOW. |

**Why a grouped dev-only minor is HIGH and a single-package minor is MEDIUM.** The two rows used to overlap: HIGH said "patch bump (or grouped dev-only)" and MEDIUM said "Tier A minor bump", so a grouped dev-only minor bump satisfied both. The grouped case is HIGH because the repo declared the group and its dev-only boundary as data, and the rule verified the dependency type for every member from the diff: a breakage stays in the local toolchain and CI is the test for it. A single-package PR has no declared group behind it, so a minor bump gets the per-PR look of Phase 4.B. A grouped PR in which one member fails the dev-only check does not match the rule at all, so it cannot be HIGH by this path.

"CI green" in the HIGH row needs a result from `pr-checks` or `check-runs`. A `green (actions-runs)` result is green for MEDIUM at most, because that source cannot see checks from other Apps.

**To compute the security-sensitive match:** the effective list is the **shared universal default ∪ the repo's `.weekly_deps.security_sensitive_extra`**. Read `references/security-sensitive-packages.md` (the universal default — loaded on demand to keep SKILL.md lean), then union the repo's extras:

```bash
SECURITY_EXTRA="$(rc_get '.weekly_deps.security_sensitive_extra | join(" ")' '')"
```

Glob-match each bumped package name against the union. For grouped PRs (`chore: bump the X group ...`), extract individual package names from the PR body bullet list via `gh pr view $PR -R "$OWNER_REPO" --json body --jq '.body'`. A match on ANY package in the group elevates the whole PR to LOW.

Annotate each PR with its label and append to the categorization table from Phase 2 — the developer should see confidence alongside tier when deciding what to do.

### 3.6 Request Copilot review (gated on LOW / UNKNOWN)

> **⚠ PRECONDITION — Copilot Code Review may be deactivated on this repo (e.g. billing lapsed), and it is usually not a required reviewer.** In that state `gh pr edit --add-reviewer "@copilot"` **exits 0 and registers nothing**, and `POST /pulls/<n>/requested_reviewers` returns 404. The failure is silent, which is the dangerous part: without a check, this phase reports "Copilot review requested" for PRs that will never receive one, and Phase 3.5's confidence model then credits a signal that does not exist.
>
> **So verify, do not assume — in both directions.** Before running this phase, confirm the request actually took:
>
> ```bash
> gh pr edit "$PR" -R "$OWNER_REPO" --add-reviewer "@copilot" >/dev/null 2>&1
> REQ="$(pr_requested "$PR")"            # read once; two reads can disagree
> if [ -z "$REQ" ] || [ "$REQ" = unknown ]; then echo "probe: no result"
> elif grep -qiE '^(copilot|copilot-pull-request-reviewer(\[bot\])?)$' <<<"$REQ"; then echo "probe: copilot requested"
> elif grep -qx 'source: graphql' <<<"$REQ"; then echo "probe: copilot off"
> else echo "probe: no result"            # REST cannot prove a bot is absent
> fi
> ```
>
> Use `pr_requested` (§ Reading PR state when the token is limited), not `gh pr view --json reviewRequests` alone: that call fails when a team is a requested reviewer, and a failed call is not a count of zero. Read the list **once** into a variable and test that: two separate reads can disagree after a transient error.
>
> `probe: no result` means the probe proved nothing. Do not conclude that Copilot is off and do not report the review as requested; try the probe on another PR, and if no PR gives a result, treat Copilot's state as unknown for the run and take the direct specialist dispatch path for Tier C.
>
> `probe: copilot off` means Copilot is off.
>
> Equally, do not hardcode "Copilot is off" — it returns when billing is re-enabled. The probe above is the source of truth; this note is not.

Bot-authored PRs do NOT trigger an automatic Copilot review, because Copilot Code Review is gated on the PR author having the subscription — dependabot and github-actions don't. The result is that the weekly batch of dependabot PRs lands with zero review signal beyond CI, which is a real gap for Tier C and security-sensitive bumps.

This phase closes that gap — **when Copilot is available** — by explicitly requesting review on LOW / UNKNOWN PRs. The mechanism (a maintainer adding `Copilot` as a `review_requested` reviewer, which the Copilot Code Review bot honors) is the canonical path on GitHub.

**Note the wider reality even when it is on:** AI reviewers commonly decline bot-authored PRs (a seat-based reviewer such as Gitar may be seatless on them, CodeRabbit reports "Bot user detected", Codex can hit quota, Copilot goes unrequested). Dependency PRs frequently merge on CI plus this skill's own triage alone. That is expected, and it is the strongest argument for the deterministic checks in Phase 2 — a bundled-engine delta extraction runs whether or not any reviewer shows up.

**Step 3.6.1 — Idempotency check.** For each LOW / UNKNOWN PR, check whether Copilot is already requested or has already reviewed:

```bash
# Already requested? Read once; "unknown", empty output, or a REST-only list that lacks
# Copilot all count as "cannot rule out".
REQ="$(pr_requested "$PR")"

# Already reviewed?
gh pr view $PR -R "$OWNER_REPO" --json reviews --jq '[.reviews[] | select(.author.login == "copilot-pull-request-reviewer")] | length' \
  || gh api --paginate "repos/$OWNER_REPO/pulls/$PR/reviews" --jq '[.[] | select(.user.login | test("^copilot-pull-request-reviewer"))] | length'
```

If `REQ` is empty or `unknown`, or lacks Copilot with `source: rest`, or both review reads fail, do not send a new request for this PR in this run: a second request cannot be ruled out. List the PR as "Copilot state unknown".

If Copilot is in `REQ`, or the review count is > 0, skip this PR (Copilot is either still working on it or done; the next re-invocation will pick up the review in Phase 4.0).

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
  gh pr edit <PR> -R "$OWNER_REPO" --add-reviewer "@copilot"

Proceed? [yes — request all] [pick subset] [skip — review yourself]
```

**Step 3.6.3 — Dispatch.** On `yes`:

```bash
for PR in $LOW_PRS; do
  gh pr edit $PR -R "$OWNER_REPO" --add-reviewer "@copilot"
done
```

The CLI's `@copilot` literal is the canonical path per `gh pr edit --help` — it routes through GraphQL's `botIds` field internally, which the plain REST `requested_reviewers` POST does not support for bots. Do NOT substitute the bot's full login `copilot-pull-request-reviewer` in the standard reviewer-request API; that returns an error.

On `pick subset`: prompt the developer to specify which PRs to skip, then dispatch only the rest.

**Step 3.6.4 — Hand off and exit early.** Run Step 3.7.1 for the whole batch first, so the coverage lines exist even when this run exits here. Copilot Code Review typically takes a few minutes per PR (more for large diffs). Don't sit and poll inside this skill invocation — tell the developer:

```
Copilot review requested on N PRs.
Re-invoke /weekly-deps in ~5–10 min and I'll pick up the reviews in Phase 4.0.

In the meantime, HIGH-confidence PRs can proceed — would you like to process those now?
[yes — process HIGH now] [no — wait for full batch] [exit]
```

If `yes — process HIGH now`: skip directly to Phase 4 with only the HIGH-confidence subset. This is safe because HIGH PRs are by definition Tier A green-CI dev-only patches with no security-sensitive packages — they don't benefit from Copilot review and would just clog its queue.

If `no — wait`: print the resume command and exit.

If a Copilot review request fails (auth issue, repo billing, API hiccup): surface the error, name the affected PR, and offer fallbacks — (a) retry, (b) skip Copilot and dispatch the specialist subagents directly for that PR (per `_shared/pr-review-agent-dispatch.md`, unioned with the repo's `.claude/repo-config.json` `.review_agents`; the model cannot invoke the user-only review skill), or (c) defer to manual review by the developer in the GitHub UI.

### 3.7 Review coverage (every bot PR)

A bot-authored PR can merge with nobody having reviewed it: Copilot does not review a bot-authored PR unless a maintainer requests it, and an org admin exempt from the default-branch ruleset can merge with no review. This step makes that gap visible. It **informs only** — zero coverage never blocks a merge here, and this skill adds no status check. It prints a coverage line, and `/enhanced-pr-review` returns NOT-READY for a bot PR with zero coverage (its §6.0 precondition 10).

**Step 3.7.1 — Record coverage for every PR.** Run this for the whole batch from Phase 1, including HIGH PRs; Step 3.6.4 runs it before its early exit. It is read-only. The script ships with `/enhanced-pr-review`: `.claude/skills/enhanced-pr-review/` in a repo copy, or the core-lifecycle plugin's `skills/enhanced-pr-review/` in a plugin install. A repo copy is read from the default branch by full refname and run only when it is a regular file, never from the checkout, because a PR in the batch can edit the checkout's copy (the same rule as `/enhanced-pr-review` Phase 1 Step 6). In `--org` mode, run this from a checkout of the repository that ships the script, or set `COVERAGE_SCRIPT` to a plugin copy outside any working tree.

```bash
COV_REF="$(git symbolic-ref --quiet refs/remotes/origin/HEAD)"   # full refname, no fallback: unset → UNKNOWN (fix with `git remote set-head origin --auto`)
COV_PATH=.claude/skills/enhanced-pr-review/review-coverage.sh; COV_SH=
case "$COV_REF" in refs/remotes/origin/?*)
  case "$(git ls-tree "$COV_REF" -- "$COV_PATH" | cut -d' ' -f1)" in 100644|100755) COV_SH="$(git show "$COV_REF:$COV_PATH")" ;; esac ;;   # a regular file only: a symlink blob would run its target
esac
cov() { if [ -n "$COV_SH" ]; then bash -c "$COV_SH" review-coverage "$@"; else echo "UNKNOWN: no regular review-coverage.sh on $COV_REF" >&2; return 2; fi; }
[ -z "${COVERAGE_SCRIPT:-}" ] || cov() { "$COVERAGE_SCRIPT" "$@"; }   # plugin install: the plugin's copy, outside the working tree
for PR in $ALL_PRS; do   # ALL_PRS = every PR number from the Phase 1 queries, unioned (per repository in --org mode)
  rc=0; cov "$PR" "$OWNER_REPO" || rc=$?   # exit 0 covered · 1 zero · 2 UNKNOWN
  [ "$rc" -le 1 ] || echo "#$PR coverage UNKNOWN"   # the script's reason is on stderr; never drop the PR's line
done
```

Each run prints one line, which goes verbatim into the run output (the digest in autonomous mode, the Phase 6.1 summary in interactive mode):

```
#101 @1a2b3c4 covered: copilot-pull-request-reviewer[bot] (COMMENTED)
#104 @5d6e7f8 ZERO coverage; stale: copilot-pull-request-reviewer[bot]@9a8b7c6
#105 @0f1e2d3 covered: <login> (first-party /review-pr)
```

Coverage is bound to the **current head SHA**. It is a submitted review whose `commit_id` is that SHA, by someone other than the PR author who is a human repo insider (`author_association` OWNER or MEMBER; COLLABORATOR also covers read and triage access, so it does not count) or an allowlisted review bot (a bot needs the allowlist even with an insider association, and a login on the default branch's `.bots[]` roster counts as a bot even when GitHub types it `User`), or a first-party coverage record carrying that SHA (see Step 3.7.2). A `COMMENTED` review needs a body or a top-level inline comment, because GitHub files each thread reply as an empty `COMMENTED` review at the current head. The allowlist is Copilot plus `.review_coverage.reviewer_bots` from the default branch's repo config. A refusal body (Copilot's "wasn't able to review any files", or the roster's `decline_pattern` / `ack_pattern`) does not count, and neither does an edited record. A drive-by account's review does not count, and neither does a code-scanning bot's. A review on an older SHA is listed as `stale` and does not count. An `@dependabot rebase` (Phase 5) moves the head, so it resets coverage. A manually triggered review counts the same way: a re-requested Copilot review or a human review is recorded against its SHA by GitHub, and a first-party suite run by hand counts once its record names the head SHA and the reviewer. Exit 2 is UNKNOWN. Print the line as `#<n> coverage UNKNOWN` and never treat it as covered. If no regular script is found on the default branch (or at `$COVERAGE_SCRIPT`), every PR's line reads `coverage UNKNOWN`.

**Step 3.7.2 — Zero coverage on the head SHA: run and record the first-party suite.** "External reviewer" means any reviewer other than this routine. Give external reviewers their window first. When the first-party suite runs depends on the mode:

| Context | Zero coverage on the head SHA → |
|---|---|
| `--mode=prep` | Record only. Copilot is requested in this same run (Phase 3.6, autonomous Step P4), so the line goes in the digest and nothing else happens. |
| `--mode=triage` | Record only. Re-check the line (autonomous-mode Step T4a); the suite waits for the interactive pass (see **No unattended suite** below). |
| Interactive Phase 4 | Run the suite, then confirm with the developer before posting the record (one batch prompt, the same as Step 3.6.2), before the PR's merge prompt. |

To run it: for a Tier C PR in interactive mode, Phase 4.C's `$DEEP_REVIEW_SKILL` pass *is* the first-party review. Do not run the suite twice. `/enhanced-pr-review` records its own pass (its Phase 1 Step 6, `suite=/enhanced-pr-review`). If `$DEEP_REVIEW_SKILL` finished on the head SHA but left no record (the configured skill is not `/enhanced-pr-review`), post it here with `suite=$DEEP_REVIEW_SKILL`. If the developer declined the deep review, or declined its record, there is no pass to record: leave the PR at ZERO. In every other interactive case, run the `/review-pr` suite: the model cannot invoke the user-only `/review-pr` skill, so dispatch the specialist subagents directly (per `_shared/pr-review-agent-dispatch.md`, unioned with the repo's `.claude/repo-config.json` `.review_agents`), the same fallback as Phase 3.6. Pin the SHA first: capture `REVIEWED_SHA="$(gh pr view $PR -R "$OWNER_REPO" --json headRefOid -q .headRefOid)"` before dispatch and give every reviewer that SHA. Post a record only after every dispatched reviewer returned a result, and only if the head is still `$REVIEWED_SHA`. A suite that did not run leaves the PR at ZERO, and that is what gets reported.

**No unattended suite.** Neither autonomous mode runs the first-party suite. It reads Dependabot PR bodies and upstream release notes, which are untrusted text, and a reviewer's tool list cannot make it read-only: `git diff`, `git log` and `git show` all accept `--output=<file>`, and a hook runner executes repo code. With no human present to stop a prompt injection, the suite runs only in the interactive pass.

Record it on the PR from a body file (`gh pr comment $PR -R "$OWNER_REPO" --body-file <file>`), under the identity that ran it (`gh api user --jq .login`):

```markdown
<!-- cv-review-coverage: sha=<$REVIEWED_SHA> reviewer=<login> suite=/review-pr -->
**First-party review** at `<sha7>` by <login> via `/review-pr` (<agents that ran>): <N findings | clean>. Recorded by `/weekly-deps` because no external reviewer engaged on this SHA.
```

Post one record per reviewed SHA. First look for your own unedited `cv-review-coverage: sha=<$REVIEWED_SHA>` comment, filtering on `user.login` equal to the gh user, and post nothing if it exists. Never edit a record, yours or anyone else's: an edited record no longer counts. The marker shape is fixed and goes on a line of its own. `review-coverage.sh` counts it only when `reviewer=` equals the comment's author, and that author passes the same trust rule as a review: not the PR author, and a human insider or an allowlisted bot. Re-run Step 3.7.1 for the PR afterwards. The line must now read `covered`. If it does not, the record did not land, so report the PR as ZERO with the failure and never as covered. Findings from the suite go to the developer through the normal Phase 4 prompt, not into the record.

---

## Phase 4: PROCESS

Work through tiers in the proposed order. Within each tier, walk PRs sequentially **within each lockfile group**, but across lockfile groups you can interleave (different directories don't contend).

**Lockstep holds come first.** A PR marked `HOLD (lockstep: …)` in Phase 2 is not processed by 4.A, 4.B or 4.C as a single PR, whatever its tier and confidence. Present the set as one decision (Phase 2 § Lockstep sets, step 4).

**Re-entrant detection.** Before processing any PR, check whether it has a Copilot review present (set in Phase 3.6 of a previous invocation, now completed):

```bash
gh pr view $PR -R "$OWNER_REPO" --json reviews --jq '[.reviews[] | select(.author.login == "copilot-pull-request-reviewer")] | map({state, submittedAt, body: (.body // "" | .[0:200])})'
```

If a Copilot review exists → route through Phase 4.0 (Copilot review triage) before per-tier processing. If no review yet but Copilot is in the output of `pr_requested "$PR"`, surface the still-pending state to the developer and skip this PR for this run.

**Coverage before every merge prompt.** Re-run Step 3.7.1 for the PR immediately before its merge prompt, because a rebase since prep changes the head SHA. On zero coverage, run and record the first-party suite (Step 3.7.2), then show the coverage line in the prompt. The line informs the decision; it does not gate it.

### 4.0 — Copilot review triage (when re-entrant detects a review)

For each PR with a Copilot review present, triage the findings before deciding on the merge. The shape of triage differs by tier:

**Tier A / B → inline lighter triage.** Fetch the review summary and inline comments:

```bash
gh pr view $PR -R "$OWNER_REPO" --json reviews --jq '.reviews[] | select(.author.login == "copilot-pull-request-reviewer") | {state, body}'
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

1. **Pick the first PR per directory** to merge first (one per manifest directory, and in `--org` mode one per repository and directory). These cannot conflict with each other. Leave out every PR with a lockstep hold.
2. For each:
   ```bash
   gh pr merge $PR -R "$OWNER_REPO" --squash --delete-branch
   # Worktree quirk: --delete-branch may exit non-zero on local cleanup;
   # server-side merge already succeeded. Verify with:
   gh pr view $PR -R "$OWNER_REPO" --json state --jq .state
   # Expect: "MERGED"
   ```
3. **Do not trust the exit code** of `gh pr merge` in worktree context — always verify with `gh pr view`.
4. After each merge, comment `@dependabot rebase` on the next PR in that same directory's queue (the lockfile changed; subsequent PRs need a refresh):
   ```bash
   gh pr comment $NEXT_PR -R "$OWNER_REPO" --body "@dependabot rebase"
   ```
5. Wait briefly for the rebase + CI re-run before merging the next in that directory's queue. In practice the developer should run `/weekly-deps` again after ~10–20 minutes to pick up the rebased PRs.

If `review each`: drop into Tier B's per-PR flow for each Tier A PR.

### 4.B — Tier B processing (per-PR quick check)

For each Tier B PR in turn:

1. Show the PR title, the bump shape (e.g., `8.18.0 → 8.20.0`, minor), and the changelog excerpt:
   ```bash
   gh pr view $PR -R "$OWNER_REPO" --json body --jq '.body' | head -80
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
5. On `view full diff`: `gh pr diff $PR -R "$OWNER_REPO" | head -200` and re-prompt.

### 4.C — Tier C processing (deep review)

Tier C deserves the most care. For each Tier C PR:

1. **Read release notes thoroughly — the package's AND the engine's.** Scan the upstream project's release notes / CHANGELOG for the new version. Grep for breaking-change markers relevant to the package class — generic ones include `BREAKING CHANGE`, `removed`, `deprecated`, `default changed`, plus any security-boundary keywords specific to the package (for Electron: `contextIsolation`, `sandbox`, `nodeIntegration`, `webPreferences`; a repo may track these per-package family in its rules docs). **For any bundled-engine package, this step is not complete until the engine delta is extracted at full patch precision and reported** (§ Special rule: bundled-engine PRs). Scanning only the wrapper's notes is exactly the gap behind the founding incident there — the engine roll was printed in the dependabot body and nothing asked anyone to read it.
2. **Cross-reference any repo rules docs** that govern the surface the package defends (whatever per-surface rules docs the repo keeps, if any). Any default that changed in the new version is a potential rules-doc update.

   **2b. Walk the platform-behavior register** (if the repo keeps one) on every bundled-engine bump. For each entry, ask whether this engine delta could have changed it, and name the runtime probe that would catch it if so. A register entry with no armed probe is the finding — say so rather than passing the PR.
3. **Invoke the configured deep-review skill via the Skill tool** — `$DEEP_REVIEW_SKILL <PR>` (default `/enhanced-pr-review <PR>`). This is the canonical path for Tier C: it fetches the Copilot review that Phase 3.6 requested, dispatches the repo's declared specialist subagents directly to add coverage, and produces a unified triage table that the developer can act on in one pass. Do NOT substitute a direct specialist-suite dispatch that bypasses the Copilot reconciliation — the existing Copilot review is load-bearing and ignoring it would re-do work the bot already finished.

   If Phase 3.6 was skipped for this PR — the developer chose "skip — review yourself", **or the 3.6 precondition probe found Copilot deactivated** — fall back to dispatching the specialist suite directly (per `_shared/pr-review-agent-dispatch.md`). `/enhanced-pr-review` still works without a Copilot review present but adds less value than a direct specialist dispatch in that case.

   **If the review produces a fix to apply, check the escalation criteria (E1–E4) before choosing who applies it** — see `_shared/lifecycle-agent-dispatch.md` § Escalation. A bundled-engine bump that turns out to have moved an auth, crypto, or key-handling behavior is **E2 on its face**; a fix in this class often spans app code, a worker or helper, and the rules docs. Never pass a `model` override; select the agent.
4. **Run tests for buildable/runnable bumps only in a verified ephemeral sandbox.**
   The PR's dependency lifecycle scripts, test scripts and configuration are untrusted
   code. A detached Git worktree changes the checkout, not the privileges of that code.
   Never install dependencies, execute tests or launch a PR-head app on the developer host.

   The sandbox must be separate from the host user and repository, contain an isolated
   copy of the exact PR-head SHA with no host Git directory, and have no host credentials,
   home directory, Git credential helpers, SSH or other agent sockets, cloud metadata,
   privileged daemon socket or writable host mounts. Enforce filesystem and network
   isolation. Deny network by default; if packages must be fetched, use a controlled
   read-only package proxy and integrity-checked artifacts without publishing credentials.
   Use disposable storage and resource limits. Verify these properties before executing
   any PR-controlled install, build, test or probe; a worktree or ordinary container alone
   does not establish them. Destroy the sandbox after collecting results, treat its output
   as untrusted, and recheck the PR head before using the result. Do not place the
   PR-controlled `$DIR` outside the sandbox checkout.

   If no environment can establish these properties, report **NOT RUN** and why; do not
   improvise an unsandboxed run. Hold the affected gate; an independently trusted CI result
   on the exact PR head may provide separately identified evidence only after verifying its
   isolation, permissions, secrets and trusted workflow provenance. A missing local test is
   not a passing test.

   Use the repo's configured per-directory commands, read from `.weekly_deps.local_test_cmds` (a map of manifest-directory → command list). When unset for a directory, fall back to a sensible per-ecosystem default for that PR's detected ecosystem:

   ```bash
   # Per-directory override (preferred). $DIR is the PR's manifest directory and comes
   # from the PR, so pass it as data (--arg), never into the jq program text.
   if [ "$RC_TRUSTED_STATE" = absent ]; then
     LOCAL_CMDS=""
   else
     LOCAL_CMDS="$(printf '%s' "$RC_TRUSTED_CFG" | jq -r --arg d "$DIR" '.weekly_deps.local_test_cmds[$d] // [] | join("\n")')" \
       || { echo "FATAL: could not parse trusted local test commands" >&2; exit 1; }
   fi
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
   | gradle | `./gradlew test` (or the repo's check task), with the wrapper from the PR head |
   | docker | (no local test; build the image if the repo has a build target, then rely on CI and step 4b) |

   In `--org` mode this step needs a local checkout of that repository. If there is none, record `Local test result: NOT RUN (no local checkout)` in step 5. Do not run the commands in a different repository's checkout, and do not record a pass.

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
   suite. Run it against the exact PR head inside the verified sandbox described
   in Step 4, using the repo's own dev launcher with enough clients to exercise the
   contract. If a suitable sandbox is unavailable or the probe cannot run inside it,
   report **NOT RUN**; do not launch PR-head code on the host. Three things must be
   true before a reading means anything:

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
   Local test result: <pass/fail/NOT RUN, with reason>
   Engine probes: <n/a — not a bundled-engine bump | clean after real run | BROKEN: … | PARTIAL: … | NOT RUN>
   Rules-doc impact: <none | requires update to a rules doc>

   Recommendation: [merge / merge after fixing X / hold for now / close]
   ```
6. Wait for explicit developer decision. Do not auto-merge Tier C.

### 4.D — Tier D processing (security data integrity)

For each Tier D PR:

1. **Verify the diff is data-only.** `gh pr diff $PR -R "$OWNER_REPO" --name-only` should show only the data file(s) declared in the matching `.weekly_deps.tier_d_sources` descriptor, no code.
2. **Inspect the data delta.** `gh pr diff $PR -R "$OWNER_REPO" | head -200` — look for the additions/removals. A normal refresh adds new entries and may remove expired ones; an abnormal refresh would remove a large fraction of entries or add entries with unusual structure.
3. **Verify the upstream source.** The bot's PR body should reference the same upstream feed URL as the previous refresh. If the source URL has changed without an explanatory commit, flag for developer review.
4. Prompt:
   ```
   #$PR — <title>
   Data delta: +N entries, -M entries
   Source: <upstream URL>
   Diff scope: data-only (no code changes)

   Merge? [yes / view full delta / skip]
   ```
5. On `yes`, merge with `gh pr merge $PR -R "$OWNER_REPO" --squash --delete-branch` and verify.

---

## Phase 5: CONFLICT RESOLUTION

After a round of merges (or in `--mode=prep` autonomous runs, where any PR is already CONFLICTING at routine time), some PRs in the same lockfile directories will need rebasing. Dependabot listens for `@dependabot rebase` and will refresh the PR against the new default branch.

**Interactive mode:** prompt the developer before sending each rebase command. Some developers prefer to inspect the conflict before rebasing.

**Autonomous mode (`--mode=prep`):** send `@dependabot rebase` to every CONFLICTING PR without prompting. The command is idempotent server-side, and `references/autonomous-mode.md` adds a 1-hour cooldown check (don't re-send if a rebase comment was already posted in the last hour) to handle accidental double-fires of the cron.

For each conflicting PR:

```bash
gh pr comment $PR -R "$OWNER_REPO" --body "@dependabot rebase"
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
  Tier C:         N (#NN <pkg> — reviewed, local test: PASS | FAIL | NOT RUN)
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

Review coverage (head SHA, one line per bot PR — Phase 3.7):
  #NN @<sha7> covered: <reviewer> (<state>)
  #NN @<sha7> ZERO coverage; stale: <reviewer>@<sha7>

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
workspace it covers first (`npm ci` in each), then run it. Both installation and
regeneration execute dependency or project code: use the verified ephemeral sandbox
and credential, filesystem and network restrictions from Phase 4.C. A merged dependency
is not automatically safe. If that isolation is unavailable, report regeneration
**NOT RUN** and hold the follow-up rather than running it on the host. Expect the same
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
- Does **not** request Copilot review on PRs that already have one requested or submitted, or whose reviewer list cannot be read — Step 3.6.1 idempotency check prevents double-requesting.
- Does **not** poll inside the same invocation while waiting for Copilot to finish a review — the developer re-invokes after ~5–10 minutes, the same re-entrant cadence as `@dependabot rebase`.
- Does **not** use the bare `requested_reviewers` REST endpoint with `reviewers: ["copilot-pull-request-reviewer"]` — that returns an error because Copilot is a bot, not a user. Always use `gh pr edit --add-reviewer "@copilot"` or the GraphQL `botIds` path.
- Does **not** auto-dismiss Copilot findings that mention specific behavioral changes, deprecations affecting shipped code, breaking-change call-outs, or supply-chain anomalies — those always surface to the developer regardless of tier.
- Does **not** suppress or tune static-analysis or quality-gate rules to force a dependency PR's check to pass — if a bump degrades coverage or trips a rule, the fix is in the repo's test code, not the rule config.
- Does **not** trust `gh pr merge`'s exit code in worktree context — always verifies with `gh pr view`.
- Does **not** merge one member of a lockstep set while another member's bump is in a different open PR (Phase 2 § Lockstep sets).
- Does **not** read a failed or refused API call as an empty result — a CI state, a reviewer list or a repo config that cannot be read is unknown, and unknown is never green.
- Does **not** apply one repository's tier data to another repository in `--org` mode, and does not ship default tier data: a repo that declares none gets Tier C for every bump.
- Does **not** process all PRs in a single round when lockfile contention is present — that's the entire reason `@dependabot rebase` exists.
- Does **not** retry the same PR after `@dependabot rebase` in the same invocation — the rebase + CI cycle takes too long. The developer re-invokes `/weekly-deps` after ~15–20 minutes.
- Does **not** add a required status check, or block a merge, for zero review coverage. Coverage is a line per PR (Phase 3.7) and a NOT-READY from `/enhanced-pr-review`. It does not count a review on an older SHA.
- Does **not** budget hours of investigation for Tier C unless the developer asks. The recommendation in Phase 4.C may be "hold for now" if the bump deserves a dedicated session.

**Autonomous mode specifically does NOT:**

- Merge any PR. The prep run + triage run produce *information* (a digest in the pinned issue) and *idempotent preparation* (Copilot review requests, `@dependabot rebase` comments). Every merge still requires the developer's interactive `/weekly-deps` session.
- Create any new PRs, branches, or commits. The tech-stack-doc version refresh (Phase 6.2) and the attribution-file regeneration (Phase 6.3) of interactive mode are skipped entirely in autonomous mode — those are the developer's call to make.
- Run the deep-review reconciliation (`$DEEP_REVIEW_SKILL`, default `/enhanced-pr-review`) on Tier C PRs. The triage run only does the lighter inline auto-dismiss + finding-surface; the heavyweight reconciliation stays interactive. Neither does it run the Phase 3.7.2 first-party coverage suite: it prints coverage lines and nothing else (see **No unattended suite**).
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
