---
name: phase-status
description: Show a burndown summary for this repository's release by querying GitHub issues via the gh CLI and MCP server. Surfaces open feature epics first, then groups open issues by priority (project board field, P1-P99) and milestone. Use at the start of weekly planning or when triaging Beta readiness.
argument-hint: "[release: <X.Y.Z> | release: beta | release: future] [--all] [--stale-days=14]"
allowed-tools:
  - Bash(gh issue list *)
  - Bash(gh issue view *)
  - Bash(gh label list *)
  - Bash(gh api *)
  - Read
  - mcp__github__list_issues
  - mcp__github__search_issues
---

# Phase Status

Summarise progress on this repository's release. Args: $0

**Label taxonomy (per `.claude/rules/labels.md` — installed there by `install.sh claude`):**
Release-tracking labels are exclusively `release: <stage-or-version>` (e.g., `release: beta`, `release: 1.0`, `release: 1.1`) — the label prefix and the priority-field taxonomy (`P1`-`P99`) are read from `rc_get '.board.release_label_prefix'` (default `release:`) and `rc_get '.board.priority_labels'` (default `P1..P5`) when `.claude/repo-config.json` sets them; otherwise assume this default scheme. If your repo migrated off an older phase-based tagging scheme, its historical issues should already carry the current `release: *` labels.

Valid argument forms (`release:` below is the DEFAULT prefix — substitute
`$RELEASE_PREFIX` from Step 0 wherever this repo configures a different one):
- `<prefix> <X.Y.Z>` — e.g., `release: 1.0`, `release: 1.1`, `release: 1.2`, `release: 1.3`
- `<prefix> beta` — current pre-GA bucket
- `<prefix> future` — post-roadmap, no specific version

If no argument is provided, default to `<prefix> beta` (the current in-flight release per `CLAUDE.md`).

Flags:
- `--all` — include closed issues in the count (default: open only)
- `--stale-days=N` — consider issues "stale" if not updated in N days (default: 14)

## Execution

### Step 0: Resolve the board knobs

Read the release-label prefix and the priority-label taxonomy from
`.claude/repo-config.json` once, up front; every later step uses these values
rather than the literals. With an absent file or absent keys both fall back to
the documented defaults, so this step never errors.

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
RELEASE_PREFIX="$(rc_get '.board.release_label_prefix' 'release:')"
PRIORITY_LABELS="$(rc_get '.board.priority_labels' '["P1","P2","P3","P4","P5"]')"   # JSON array, highest priority first
```

### Step 1: Validate the release argument
If $0 is empty, use `$RELEASE_PREFIX beta`. Accept any of the valid argument forms listed above, with `$RELEASE_PREFIX` in place of the literal `release:`. If $0 is not a recognized form, STOP and list the valid options.

### Step 2: Fetch open issues in the phase
```bash
gh issue list \
  --label "$0" \
  --state open \
  --limit 200 \
  --json number,title,labels,milestone,updatedAt,author,assignees
```

(or the equivalent `mcp__github__list_issues` / `mcp__github__search_issues`
call, whichever is faster this session)

### Step 3: Group by priority
When a board is configured (`rc_has '.project'`), priority is the board's Priority
field, not a label (see `.claude/rules/labels.md`). Parsing the label list for a
priority on such a repo finds nothing and silently buckets every issue as
`Unprioritised`, so read the board. Address it by node id (`rc_get '.project.id'`):
that works whether the board is user- or org-owned, whereas `--owner @me` cannot
reach an org-owned board. Reading the field needs the `project` token scope; on
`INSUFFICIENT_SCOPES`, surface `gh auth refresh -s project` rather than falling
back to labels. Bucket by the option names, in the order
`.project.fields.priority.options` declares:

```bash
PROJECT_ID="$(rc_get '.project.id')"
REPO="$(gh repo view --json nameWithOwner --jq .nameWithOwner)"

gh api graphql --paginate \
  -f query='query($p:ID!,$endCursor:String){ node(id:$p){ ... on ProjectV2 {
    items(first:100, after:$endCursor){
      pageInfo{ hasNextPage endCursor }
      nodes{
        priority: fieldValueByName(name:"Priority"){ ... on ProjectV2ItemFieldSingleSelectValue { name } }
        content{ ... on Issue { number repository { nameWithOwner } } } } } } } }' \
  -f p="$PROJECT_ID" \
  --jq ".data.node.items.nodes[]
        | select(.content.number != null
                 and .content.repository.nameWithOwner == \"$REPO\")
        | [.content.number, (.priority.name // \"\")] | @tsv"
```

A board can hold several repositories' issues, so the filter keeps only this repo's
items before joining on issue number.

Join that number → priority map onto the Step 2 issue list. `Unprioritised` means a
missing **board item** or a board item with no Priority set — not a missing label.
Report the board-absent count separately: Projects v2 auto-add can silently skip
items, so an issue that is not on the board is a tracking gap rather than a
deprioritised one.

Without a board, fall back
to labels: parse the label list on each issue for the labels in `$PRIORITY_LABELS` (Step 0;
default `P1`, `P2`, `P3`, `P4`, `P5`), and bucket in that array's order — it is
declared highest-priority-first. A repo that declares `P1`-`P99`, or a different
naming scheme entirely, groups by whatever it declared; do NOT assume five
buckets. Issues carrying none of those labels go into `Unprioritised`.

### Step 4: Identify stale issues
An issue is "stale" if `updatedAt` is more than `--stale-days` (default 14)
in the past. Count them separately and list them at the bottom of the report.

### Step 5: Group by milestone
Bucket issues by their `milestone.title`. Issues with no milestone are a
red flag during planning — call them out.

### Step 5.5: Surface open epics

Epics are first-class tracking artifacts. Per `.claude/rules/release-tracking-epics.md`
(installed by `install.sh claude`; a repo may skip it and set `.epic_linkage.version_epics: false`) the convention is **one release epic per semver
bump** — titled `Epic: vX.Y.Z release`, carrying `type: epic` plus the release label,
and sitting on that version's milestone — with **feature** epics alongside them. A
repo can override the title pattern and label via `.claude/repo-config.json`
(`.epic_linkage.epic_title_pattern`, default `Epic: v{version} release`, and
`.epic_linkage.epic_label`, default `type: epic`); read them in Step 0 if the repo
sets them.

Surface both kinds. A release stream with no open epic, and an epic whose children
are all closed but which is still open, are both planning signals worth reporting —
not errors this skill should fix (it is read-only).

**A repo can opt out of version epics.** Some repos track each version with its
milestone and `release:` label alone, and keep epics for features. Such a repo sets
`.epic_linkage.version_epics` to `false`. Then surface feature epics only, and do
NOT flag a release stream that has no version epic, or suggest filing one:

```bash
VERSION_EPICS="$(rc_get '.epic_linkage.version_epics' 'true')"   # "false" = feature epics only
```

Fetch the open epics for the stream under review:

```bash
EPIC_LABEL="$(rc_get '.epic_linkage.epic_label' 'type: epic')"
gh issue list --label "$EPIC_LABEL" --state open --json number,title,labels,milestone \
  --jq '.[] | {n: .number, title, milestone: .milestone.title, labels: [.labels[].name]}'
```

For each open epic:

1. **Title and number.**
2. **Open children** — sub-issues still open, via the REST `/sub_issues` API:
   ```bash
   gh api repos/{owner}/{repo}/issues/<epic-#>/sub_issues --jq '.[].number'
   ```
3. **Stale children** — sub-issues last-updated > 14 days ago (smell signal).
4. **Health signals:**
   - All sub-issues attached via REST (not markdown tasklists) — sanity-check
     by grepping the epic body for `- [ ] #` patterns; those indicate the
     unreliable tasklist heuristic instead of the deterministic REST endpoint.
   - **Fully-complete epic still open** — if every child is closed, flag it for
     closure.

If your repo maps release codenames to versions, that table lives in your own
`CLAUDE.md` — codenames are cosmetic and should never appear in user-facing
copy, only the version.

### Step 6: Print the report

```
Phase Status — $0
────────────────────────────────────────────────────────────────
  Open epics
    Epic: <release or feature name>  #1236
      open children: NN    stale (>14d): MM
      health: ✓ all REST-attached

  Total open: NN
  Stale (>14d):  MM
  Unmilestoned:  KK  ← usually a red flag

  By Priority
    P1 (critical) :  N  #123, #124, #125
    P2 (high)     :  N  ...
    P3 (medium)   :  N  ...
    P4 (low)      :  N  ...
    P5 (nice)     :  N  ...
    (none)        :  N  ...   (of which K not on the board)

  By Milestone
    v1.0.0        : N   (NN% of stream)
    v1.1.0        : N
    (none)        : N   ← triage needed

  Stale issues (>14 days without update)
    #456  "Something" (updated 32d ago, assignee: foo)
    #789  "Another thing" (updated 21d ago, unassigned)
────────────────────────────────────────────────────────────────
Next action: [concrete recommendation based on the above]
```

The `By Priority` rows above show the DEFAULT `P1`-`P5` scheme. Emit one row per
bucket from Step 3, in that order: the board's `.project.fields.priority.options`
when a board is configured, otherwise `$PRIORITY_LABELS` (Step 0). Add the `(none)`
row for `Unprioritised` — never a fixed five.

## Recommendations

Based on the counts, suggest one of:
- **"Triage N unmilestoned issues"** if the unmilestoned count > 5
- **"Close or update N stale issues"** if the stale count > (open count ÷ 4)
- **"Focus on P1/P2 — M items blocking milestone X"** if there are high-priority open issues with a soon-approaching milestone
- **"On track — N issues remaining, none blocking"** if none of the above fire

## What This Skill Does NOT Do

- Does not modify or close any issues (read-only)
- Does not predict completion dates (per CLAUDE.md: "Avoid giving time estimates")
- Does not cross-phase — run it once per phase you care about

## Why This Skill Exists

Release-readiness conversations otherwise require manually running 3–5 `gh
issue list` commands and eyeballing the output. This collapses that into one
command answering "where are we, what's stuck, what needs triage" for a single
release stream.
