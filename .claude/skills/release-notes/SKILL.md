---
name: release-notes
description: Draft GitHub release notes for a version tag by walking git log since the previous tag, grouping commits by Conventional Commit type, and cross-referencing closed issues. Includes a "Docs that may need updating" section to surface stale documentation. Use when cutting vX.Y.Z or any interim tag.
argument-hint: "[version] [--since=<tag>] [--dry-run]"
allowed-tools:
  - Bash(git log *)
  - Bash(git tag *)
  - Bash(git rev-parse *)
  - Bash(source *)
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
(e.g. `vX.Y.Z`, or whatever release-tag scheme this repo uses — check
`git tag -l | head` rather than assuming a bare `v*` prefix). Optional flags:

- `--since=<tag>` — explicit "from" tag; otherwise auto-detect the previous tag
- `--dry-run` — print to stdout only, do not create a draft release via `gh release create`

## Scope invariant — the range, never the cutting PR

**The unit of a release is `<previous-tag>..HEAD`, not the PR that carried the version
bump.** A version bump physically rides on one PR, so anything invoked from that PR's
context — this skill, any changelog entry it seeds, and any brand-voice pass over that entry — will
silently inherit that PR's
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
- Otherwise, list this repo's release tags and pick the one that isn't $0
  as the previous release. The prefix comes from `.release.tag_prefix`
  (default `v`), so a repo with other tags (for example per-component
  prefixes) only sees its own release line:
  ```bash
  # Read the key from the default branch's config as data (no helper to source, and a
  # PR branch's copy is never read); absent config or key → v.
  RC_REF="$(git symbolic-ref --quiet refs/remotes/origin/HEAD || echo refs/remotes/origin/main)"
  TAG_PREFIX="$(git show "$RC_REF:.claude/repo-config.json" 2>/dev/null | jq -r '.release.tag_prefix // empty' 2>/dev/null)"
  TAG_PREFIX="${TAG_PREFIX:-v}"
  TAGS="$(git tag -l "${TAG_PREFIX}*" --sort=-v:refname)"
  # A repo with bare tags (1.2.3) and no .release.tag_prefix would otherwise
  # find nothing and treat this as a first release.
  [ -n "$TAGS" ] || { echo "WARN: no ${TAG_PREFIX}* tags; using all tags" >&2; TAGS="$(git tag -l --sort=-v:refname)"; }
  printf '%s\n' "$TAGS" | head -2
  ```
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

### Step 3.5: The three artifacts

One range can produce up to three artifacts with different jobs. Skip the two
`CHANGELOG.md` rows when this repo uses GitHub Releases as its only changelog.
Truncation belongs to exactly one of them — do not carry a rule across the boundaries:

| Artifact | Detail | Voice | Audience |
|---|---|---|---|
| Draft GitHub release body | every non-skipped commit | mechanical, `git log`-derived | developers, archival |
| `CHANGELOG.md` — `###` category bullets | **complete, in good detail** | `/ste-writing` **flavored** mode | public archival record |
| `CHANGELOG.md` — version preamble | 1–3 sentences, summarized | project brand voice | in-app "what's new" view, if any |

**The changelog is the record, not the notification.** It gets everything that shipped,
described well enough to be genuinely useful a year later — never capped, never
truncated. Its bullets carry a bold-lead outcome plus the detail that makes the outcome
legible (what broke, what a user saw, what now happens instead). One bullet per
*outcome*, so an epic's children or several commits closing one issue collapse into one
well-written bullet keyed on the issue and linking the primary PR — that is
consolidation, not omission, and every merged PR in the range must be traceable to some
bullet. Internal-only work (tests, chores, refactors, tooling, docs) still belongs in
the record; group it under `### Changed` in plain terms rather than dropping it, unless
it is genuinely invisible outside the repo.

Voice for those bullets is `/ste-writing` in **flavored** mode, not strict — approachable
plain technical English, consistent terminology, one idea per sentence, no marketing
adjectives and no AI-slop hedging. Strict mode is for procedures and safety text and
would make the changelog read like a manual.

**The preamble is the notification.** Each `## [x.y.z]` section opens with a short
brand-voice paragraph *above the first `### ` heading*. If the app renders changelog
text in-client (for example a post-update view that splits each section on its first
`### ` and shows only what comes before it), that zone is all a user sees. Write it
last, from the finished detail below it: two or three sentences naming what actually
changed for a person using the app. It is a summary, so it legitimately omits — no
tail-collapse bullet, no "and N more"; the detail directly beneath it is the complete
account, and the in-app view can link out to the full notes.

A zone holding only a blockquote counts as absent — a `> versions 0.2.4–0.2.6 never
reached you` note is an aside about the release, not a summary of it. Pair it with real
prose when you want both.

Truncation matters most here for a reason easy to miss: an in-app view typically
concatenates **every version the user skipped**, so someone updating across nine
releases sees nine preambles at once. Nine short paragraphs read fine; nine full bodies
are a wall of text.

Report the split in the skill's output: `CHANGELOG seed: 87 commits → 31 detail bullets
across 4 categories + 1 preamble`. A reviewer can then see whether the range was
genuinely covered.

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
{ "glob": "<glob-or-path-prefix>", "docs": ["<doc-path>", ...] }
```
For each entry where a file matching `glob` was changed in the range but
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
**CHANGELOG seed:** NN commits → MM detail bullets across P categories + 1 preamble  (omit when there is no `CHANGELOG.md`)

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
- Does not write changelog entries automatically. If this repo uses GitHub
  Releases as the changelog, the draft release is the whole output. If it keeps
  a checked-in `CHANGELOG.md` (for example one a CI job requires a matching
  `## [x.y.z]` section in on every version bump), this skill's categorized
  output — **covering the whole `<previous-tag>..HEAD` range, not the cutting
  PR** — is the recommended seed for that entry. Per Step 3.5 the entry has two
  zones with two voices: the `###` category bullets are the complete record in
  `/ste-writing` **flavored** mode, and the preamble above them is a 1–3
  sentence summary in the project's brand voice (apply its brand-voice
  guidelines or enforcement skill if one is installed) — user-benefit-first
  declaratives, named mechanisms, emoji-free, the product's own name. Neither
  zone may re-narrow scope back to the cutting PR. If the file ships to users
  (e.g. bundled into the app for a post-update view), entries must not
  reference private-tree paths (`.claude/`, internal docs trees, `CLAUDE.md`)
- Does not update `CLAUDE.md` counts
- Does not infer semantic version bumps (the user picks the tag)

## Why This Skill Exists

Release notes are one of those tasks where the first 80% is a boring
mechanical summary that a machine should do, and the last 20% is
judgment — which changes matter, which deserve a callout, which docs
are now stale. This skill does the 80% deterministically and flags the
20% explicitly so a human can make the judgment call. The "Docs that
may need updating" section is the most valuable part: it's the only
automated signal the project currently has for documentation drift.
