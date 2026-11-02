---
name: release-notes
description: Draft GitHub release notes for a version tag by walking git log since the previous tag, grouping commits by Conventional Commit type, and cross-referencing closed issues. Includes a "Docs that may need updating" section to surface stale documentation. Use when cutting vX.Y.Z or any interim tag.
argument-hint: "[version] [--since=<tag>] [--dry-run]"
allowed-tools:
  - Bash(git log *)
  - Bash(git tag *)
  - Bash(git show *)
  - Bash(gh release *)
  - Bash(gh issue view *)
  - Bash(gh pr view *)
  - Bash(gh repo view *)
  - Bash(find *)
  - Read
  - Grep
  - Glob
---

# Draft Release Notes

Generate release notes for a version tag. Args: $0 = the new version
(e.g. `vX.Y.Z`). Optional flags:

- `--since=<tag>` — explicit "from" tag; otherwise auto-detect the previous tag
- `--dry-run` — print to stdout only, do not create a draft release via `gh release create`

## Scope invariant — the range, never the cutting PR

**The unit of a release is `<previous-tag>..HEAD`, not the PR that carried the version
bump.** A version bump physically rides on one PR, so anything invoked from that PR's
context — this skill, and any changelog entry it seeds — will silently inherit that PR's
diff as its scope unless told otherwise. Every merge since the previous tag ships in this
release and belongs in the notes, including work from unrelated PRs, hotfixes, dependency
merges, and bot-authored commits.

Never derive release scope from `gh pr diff`, the current branch's diff, or the issues
closed by the cutting PR. Derive it from `git log <previous-tag>..HEAD` (Step 1), and state
the resolved range and commit count in the output so an undercounted release is visible
rather than merely plausible.

This failure is quiet by construction: a release that reports one change looks exactly like
a release that contained one change. Printing the range and the commit count is what makes
the two distinguishable.

## Execution

### Step 1: Determine the range
- If `--since` is provided, use it as the from-tag.
- Otherwise, run `git tag --sort=-v:refname | head -2` and pick the one
  that isn't $0 as the previous release.
- If no previous tag exists (first release), collect all commits since the repository's initial commit.
- Print the range: `<previous>..HEAD` (or `<previous>..<target-tag>`).

### Step 2: Collect commits in range
```bash
git log --pretty=format:'%h %s%n%b%n---' <previous>..HEAD
```
Parse each commit message. For each commit, extract:
- Short SHA
- Conventional Commit type (`feat`, `fix`, `refactor`, `docs`, `test`,
  `chore`, `perf`, `build`, `ci`)
- Subject line
- Any `Closes #NNN` or `Fixes #NNN` references (for cross-linking)
- `Co-Authored-By:` trailers (to acknowledge AI-assisted commits)

### Step 3: Bucket by type
Group commits into the following sections, in this order:

1. **✨ Features** (`feat:`) — new user-visible capability
2. **🐛 Bug Fixes** (`fix:`) — user-visible behavior corrections
3. **⚡ Performance** (`perf:`) — measurable speedups or resource reductions
4. **🔒 Security** — commits whose subject/body mentions "security",
   "auth", "crypto", "CVE", regardless of type
5. **📦 Infrastructure & Build** (`build:`, `ci:`, `chore:` touching infra/CI paths or `.github/`)
6. **📚 Documentation** (`docs:`)
7. **♻ Refactors** (`refactor:`)
8. **🧪 Tests** (`test:`)
9. **🏠 Chores** (remaining `chore:`)

Skip commits that are pure version-bumps, merge commits without substance,
and commits matching any patterns listed under `skip_commit_patterns` in
`.claude/repo-config.json` (configured automation commits for this repo).

### Step 4: Cross-reference issues
For any commit body with `Closes #NNN`, fetch the issue title via
`gh issue view NNN --json title,labels` and append as a trailing note:
`(closes #NNN — <title>)`.

### Step 5: Migrations summary (conditional)
Check whether the repo contains a database migrations directory:
```bash
find . -maxdepth 6 -type d -name "migrations" | grep -v node_modules | head -5
```
Only if one or more migrations directories exist AND new migration files
were added in the commit range, add a dedicated subsection:
```
### Database Migrations
- `<migration-filename>` — [one-line description from the file's leading comment]
```
Read each new migration file's leading comment block to extract the description.
Skip this entire section if no migrations directory is found or no new
migration files were added in the range.

### Step 6: "Docs that may need updating" (core feature)
This is what makes this skill more than a `git log` wrapper. For each
source area touched, check whether related docs were also modified in
the same range.

**Config-driven mapping (preferred):** Check for `.claude/repo-config.json`.
If it exists and contains a `docs_map.sources` array, use those entries to
drive the check. Each entry has the shape:
```json
{ "source": "<glob-or-path-prefix>", "docs": ["<doc-path>", ...] }
```
For each entry where a file matching `source` was changed in the range but
none of the listed `docs` were also changed, flag it.

**Generic heuristic fallback (when no `docs_map` config is present):**
For each source file changed in the range:
- If the file is under a `src/`, `lib/`, or `internal/` directory and
  no `.md` file in the same or a sibling `docs/` directory was also
  touched in the range, flag it as potentially stale.
- If files under `.claude/` or `.mcp.json` changed and `CLAUDE.md` was
  not updated, flag it.

For each source area touched in the range where the corresponding doc
was NOT also touched, add a line to a "Docs that may need updating"
section. This is advisory, not a block — doc drift is silent and this
section surfaces it before release.

Example output:
```
### 📋 Docs that may need updating
- `src/auth/` had changes but `docs/architecture.md` was not updated
- `src/api/` touched but no adjacent docs updated — confirm API docs are current
- `.claude/` config changed but `CLAUDE.md` was not updated
```

### Step 7: AI authorship acknowledgement
Count commits with `Co-Authored-By:` trailers and add a footer:
```
---
_This release includes N commits with AI assistance (configured AI model)._
```
If the repo has an AI governance policy document configured under
`ai_policy_doc` in `.claude/repo-config.json`, or if a file matching
`docs/policies/ai-*.md` exists in the repo, append a link to it:
```
_Full AI governance policy: <policy-path>_
```
Otherwise, omit the policy link.

### Step 8: Determine default branch
```bash
gh repo view --json defaultBranchRef --jq '.defaultBranchRef.name'
```
Use the result as the `--target` value in the release command.

### Step 9: Output
If `--dry-run`, print the draft to stdout and stop.

Otherwise, create a GitHub draft release via:
```bash
gh release create $0 --draft --title "$0" --notes-file <tempfile> --target <default-branch>
```

ALWAYS create as `--draft` — the team reviews and publishes manually.
Publishing a release is a consequential action that requires human approval.

## Output Format (Template)

```markdown
## $0 — <date>

**From:** <previous-tag>  |  **Commits:** NN  |  **Contributors:** MM

<optional one-paragraph summary if there's a clear theme to the release>

### ✨ Features
- [SHA] feat: … (closes #NNN — Issue Title)

### 🐛 Bug Fixes
- [SHA] fix: …

### 🔒 Security
- [SHA] fix: … (CVE- if applicable)

### 📦 Infrastructure & Build
- [SHA] …

### 📚 Documentation
- [SHA] …

### ♻ Refactors
- [SHA] …

### 🧪 Tests
- [SHA] …

### 🏠 Chores
- [SHA] …

### Database Migrations
- `migration-name` — description
(omit section entirely if no migrations were added in the range)

### 📋 Docs that may need updating
- ...

---
_This release includes N commits with AI assistance (configured AI model)._
```
(use the model name from the active AI session; fall back to a generic 'AI assistance' if unknown)

## What This Skill Does NOT Do

- Does not publish the release (always `--draft`)
- Does not write changelog entries to a `CHANGELOG.md` — this repo uses
  GitHub Releases as the changelog, not a checked-in file
- Does not infer semantic version bumps (the user picks the tag)

## Why This Skill Exists

Release notes are one of those tasks where the first 80% is a boring
mechanical summary that a machine should do, and the last 20% is
judgment — which changes matter, which deserve a callout, which docs
are now stale. This skill does the 80% deterministically and flags the
20% explicitly so a human can make the judgment call. The "Docs that
may need updating" section is the most valuable part: it's the only
automated signal the project currently has for documentation drift.
