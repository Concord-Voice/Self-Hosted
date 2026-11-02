---
name: issue-brief
description: Fetch GitHub issue context and format as a structured brief for brainstorming. Pulls title, body, all comments, labels, milestone, linked issues/PRs, and subissues into a single organized view.
argument-hint: [issue-number]
allowed-tools:
  - Bash(gh issue *)
  - Bash(gh api *)
  - Bash(gh pr *)
  - Bash(gh repo *)
  - Read
---

# Issue Brief

Generate a structured brief for issue #$0 to provide full context for brainstorming or implementation sessions.

---

## Step 1: Resolve repo identity

```bash
gh repo view --json nameWithOwner --jq '.nameWithOwner'   # e.g. owner/repo
```

The `gh api` calls below use gh's built-in `{owner}`/`{repo}` placeholders, which
it fills from the current repository — no manual variable wiring needed.

## Step 2: Fetch core issue data

```bash
gh issue view $0 --json number,title,body,state,labels,milestone,assignees,createdAt,updatedAt,comments
```

If the issue doesn't exist or is inaccessible, STOP and report the error.

## Step 3: Fetch linked issues and PRs

Use the timeline API to find cross-references:

```bash
gh api repos/{owner}/{repo}/issues/$0/timeline --paginate \
  --jq '[.[] | select(.event == "cross-referenced") | {
    number: .source.issue.number,
    title: .source.issue.title,
    state: .source.issue.state,
    type: (if .source.issue.pull_request then "pr" else "issue" end),
    merged: (if .source.issue.pull_request then .source.issue.pull_request.merged_at != null else false end)
  }] | unique_by(.number)'
```

If the timeline API fails (permissions, rate limit), fall back to grepping the issue body for `#NNN` references and fetching each:

```bash
gh issue view $0 --json body --jq '.body' | grep -oE '#[0-9]+' | sort -u
```

## Step 4: Identify subissues

Children are attached as native sub-issues (REST `/sub_issues`, per `.claude/rules/release-tracking-epics.md`). Fetch those first:

```bash
gh api "repos/{owner}/{repo}/issues/$0/sub_issues?per_page=100" --jq '.[] | "\(.number)\t\(.state)\t\(.title)"'
```

Only when that returns nothing, fall back to legacy checkbox references in the body (e.g., `- [ ] #123` or `- [x] #456`):

```bash
gh issue view $0 --json body --jq '.body' | grep -oE '\- \[[ x]\] #[0-9]+' | grep -oE '[0-9]+' | sort -u
```

For each subissue number, fetch its title and state.

## Step 5: Format the brief

Output the following structured format:

```
Issue Brief — #$0: [Title]
────────────────────────────────────────────────────────────
State: [open/closed] | Labels: [comma-separated]
Milestone: [name or "none"] | Assignees: [comma-separated or "unassigned"]
Created: [date] | Updated: [date]

## Description

[Full issue body, preserving markdown]

## Labels & Classification

- Phase: [phase label if present]
- Priority: [P1-P5 label with description]
- Type: [type label]
- Themes: [theme labels]

## Linked Issues

- #NNN [title] (STATE) — [inferred relationship]
- #NNN [title] (STATE)

## Linked PRs

- #NNN [title] (STATE — merged/open/closed)
- #NNN [title] (STATE)

## Subissues

- [ ] #NNN [title] (OPEN)
- [x] #NNN [title] (CLOSED)
(or "None" if no subissues found)

## Comments (N total, newest first)

### [author] — [date]
[comment body]

### [author] — [date]
[comment body]

## Context for Brainstorming

- **Dependencies:** [which linked issues must be done first — based on body references like "depends on #NNN" or "after #NNN"]
- **Already shipped:** [linked PRs that are merged]
- **Remaining work:** [open linked issues and subissues]
- **Suggested starting point:** [based on priority labels and what's already done]
────────────────────────────────────────────────────────────
```

## Inferring Relationships

When building the "Linked Issues" section, infer the relationship type from context:
- If the linked issue is referenced in the body as a dependency → "dependency"
- If a PR title or body contains `Closes #$0` or `Partial for #$0` → "implementing PR"
- If the linked issue shares a phase label → "related (same phase)"
- Otherwise → "related"

## What This Skill Does NOT Do

- Does not modify issues or PRs (read-only)
- Does not fetch full bodies of linked issues (only titles + state to avoid context explosion)
- Does not create branches, PRs, or start brainstorming automatically
- Does not fetch GitHub Projects board data (defer to future enhancement)
- Does not give time estimates or predictions

## Why This Skill Exists

Starting a new session on an existing issue requires re-reading the issue, all its comments, finding linked work, and understanding what's already been done. This takes 5-10 minutes of manual `gh` commands every time. `/issue-brief` does it in one command and formats the output for immediate consumption by `/superpowers:brainstorming`.
