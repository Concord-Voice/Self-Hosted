---
name: issue-creation
description: Create consistent, well-specified GitHub issues that are immediate `/dev-lifecycle` intakes. Enforces an 8-phase workflow — adjacency scan, scope assessment, pre-spec generation with research, label/milestone alignment, relationship linking, preview checkpoint, creation, and post-creation follow-through. Use whenever asking Claude to "create an issue", "file an issue", or open new work.
argument-hint: "[short topic or title sketch] [--force-standalone] [--parent=N]"
allowed-tools:
  - Bash(gh issue *)
  - Bash(gh pr *)
  - Bash(gh label *)
  - Bash(gh project *)
  - Bash(gh repo view *)
  - Bash(gh api *)
  - Bash(git log *)
  - Bash(git blame *)
  - Bash(git show *)
  - Bash(source *)
  - Read
  - Grep
  - Glob
  - Write
---

<CONFIG>
This skill is **config-driven and stack-neutral**. It reads an OPTIONAL
`.claude/repo-config.json` through the shared helper `scripts/repo-config.sh`
(`rc_get '<jq-filter>' [default]`, `rc_has '<jq-filter>'`). Source it once at the
start of any phase that needs config:

```bash
source "$(git rev-parse --show-toplevel)/scripts/repo-config.sh"   # path where the org tooling is installed/symlinked
```

Three rules make this skill work in ANY repo:

1. **Owner/repo are derived at runtime, never hardcoded:**
   `OWNER_REPO=$(gh repo view --json nameWithOwner -q .nameWithOwner)` (then `OWNER=${OWNER_REPO%%/*}`, `REPO=${OWNER_REPO#*/}`).
2. **Every project-board operation is gated on `rc_has '.project'`.** When `.project`
   is absent or null, the board step prints `no project board configured — skipping board fields`
   and continues — the issue is still created with labels + milestone. Project owner,
   number, board node-id, and field/option ids all come from `.project.*`.
3. **Labels, milestones, and domain rules are auto-discovered** from the live repo
   (`gh label list`, `gh api repos/$OWNER/$REPO/milestones`, `.claude/rules/*.md`).
   There are no hardcoded label values, release codenames, version strings, or field
   ids in this skill — it adapts to whatever taxonomy the target repo actually has.
</CONFIG>

<DISPATCH-GATE>
When a phase says "Run `/skill-name`" or "Dispatch X agent", you MUST invoke it via the Skill or Agent tool. Do NOT substitute with inline commands, manual replies, or partial implementations. The sub-skills/subagents own their execution; `/issue-creation` owns the sequencing. This is not optional.
</DISPATCH-GATE>

<CHECKPOINT-DISCIPLINE>
Five phases are **hard checkpoints** requiring explicit human approval before proceeding: 1 (adjacency-scan decision), 2 (scope assessment), 5 (relationship linking), 6 (preview), 7 (parent-absorbing path only; the universal creation gate is Phase 6). Never skip a checkpoint. "I'll just do this one thing" = stop and ask.
</CHECKPOINT-DISCIPLINE>

# Issue Creation

Create a `/dev-lifecycle`-ready GitHub issue for: **$0**

**Catalog Dispatch (mandatory):** this skill is one of the four ingesting skills bound by [`.claude/skills/_shared/dispatchable-skills.md`](../_shared/dispatchable-skills.md). Run its "## Dynamic skill scoping (mandatory)" algorithm at Phase 1 (UNDERSTAND) and Phase 3 (Pre-Spec). Dispatch of a matched skill is mandatory subject to the checkpoint discipline above; billed/cloud-cost skills require explicit preapproval.

Flags:
- `--force-standalone` — skip the adjacency scan and create as a standalone issue (use sparingly; you're overriding the system's main consolidation lever)
- `--parent=NNN` — declare up front that this is a child of issue #NNN; skip adjacency scan and go straight to scope + pre-spec generation

---

## Phase 1: Adjacency Scan ⚠️ CHECKPOINT

**Goal:** determine if this should be a new standalone issue, a sub-issue of an existing parent, or if it should spawn a new parent that absorbs adjacent existing issues.

### Step 1 — Resolve repo + taxonomy context

```bash
source "$(git rev-parse --show-toplevel)/scripts/repo-config.sh"
OWNER_REPO=$(gh repo view --json nameWithOwner -q .nameWithOwner)
OWNER=${OWNER_REPO%%/*}; REPO=${OWNER_REPO#*/}
gh label list --limit 300 --json name,description > /tmp/issue-creation-labels.json
```

Load the labels so the skill knows what themes/systems this repo actually uses — this discovered list is the authoritative label taxonomy for all later phases (do not assume any fixed set). Also glob `.claude/rules/*.md` to identify which domain rules might apply based on keywords in $0.

### Step 2 — Keyword extraction

From the user's topic ($0), extract:
- **Primary nouns** (e.g., "pin messages", "SSO", "key rotation")
- **Action verbs** (add, fix, refactor, audit, document)
- **Surface hints** — map to the repo's own surfaces (e.g. frontend, backend, CI, docs, infra), using the `area:`/`domain:` labels discovered in Step 1 as the vocabulary

### Step 3 — Dispatch adjacency scan

Invoke the **Explore** subagent via the Agent tool with a tightly-scoped prompt:

```
You are scanning open GitHub issues for adjacency to a proposed new issue.

Proposed topic: "$0"
Extracted keywords: [nouns, verbs, surface hints from step 2]

Run these searches in parallel:
  gh issue list --state open --search "KEYWORD1 KEYWORD2" --limit 30 --json number,title,labels,milestone
  gh issue list --state open --label "domain: DOMAINGUESS" --limit 30 --json number,title,labels,milestone
  gh issue list --state open --label "type: epic" --limit 50 --json number,title,labels,milestone,body

For each open epic returned: read its body and determine if the proposed topic fits as a sub-issue under its scope.

Report in ≤400 words:

## Duplicate candidates
[Issues whose title/body overlaps strongly with the proposed topic. Include #N and one-line reason.]

## Parent-epic candidates (for sub-issue creation)
[Open type-epic-labeled issues where the proposed topic is within their stated scope. Include #N, parent scope in one line, and why the topic fits.]

## Adjacent non-epic issues (for new-parent clustering)
[Open issues NOT labeled type-epic that are scope-adjacent — same domain, same area, similar problem space. If ≥3 such issues exist, this is a signal to propose a new parent epic that absorbs them along with the new issue.]

## No adjacency
[If nothing relevant, say so.]
```

### Step 4 — Decision table

Based on the Explore agent's report, select the path:

| Finding | Path |
|---|---|
| Strong duplicate exists | **STOP.** Redirect user to existing issue — consider adding a comment to it, or expanding its scope instead. |
| Strong parent-epic fit | **Sub-issue path.** Go to Phase 2 with `parent=NNN`. |
| 3+ adjacent non-epic issues | **New-parent-with-absorption path.** Go to Phase 2 with cluster-proposal mode. |
| 1–2 adjacent issues | **Standalone + cross-reference path.** Go to Phase 2 as standalone; Phase 5 will add `## Related`. |
| No adjacency | **Standalone path.** Go to Phase 2 as standalone. |

### Step 5 — Checkpoint

Present the decision to the user:

```
Adjacency scan complete.

Recommendation: [DUPLICATE|SUB-ISSUE|NEW-PARENT+ABSORB|STANDALONE]

Rationale: [one paragraph]

[If sub-issue or new-parent:] Proposed structure:
  Parent: #NNN "Title" (existing)  OR  [NEW PARENT: "proposed title"]
  This issue (new): "working title"
  [If absorb:] Absorbing into new parent: #AAA, #BBB, #CCC

Proceed? [yes / revise / cancel]
```

**Do not continue without explicit approval.**

---

## Phase 2: Scope Assessment ⚠️ CHECKPOINT

**Goal:** decide whether the proposed scope should be a single issue or split into parent+children.

### Heuristics for "too wide"

A single issue is "too wide" (and should become parent+children) if ANY of these are true:
- Crosses ≥2 distinct code surfaces / services (more than one `area:` would genuinely apply)
- Requires ≥2 database or schema migrations
- Spans ≥2 release boundaries (would carry more than one `release:` value)
- Natural decomposition into ≥3 independent landable PRs
- Body would exceed ~300 lines with dense AC
- Mixes a large foundational change with unrelated polish/follow-up work in one issue

If the topic is clearly a single landable change (bug fix, one-surface feature, doc update), treat as single issue.

### Step 1 — Propose structure

For single-issue path, print:
```
Scope verdict: single issue (no children needed)
Estimated landable PR: one
```

For split path, print:
```
Scope verdict: parent + N children
Proposed children:
  1. "Child title" — [type: <type>, area: <area>, release: <release>, Priority=<P?> (board, if configured)]
  2. "Child title" — [type: <type>, area: <area>, release: <release>, Priority=<P?> (board, if configured)]
  3. ...
Each child gets its own pre-spec in Phase 3.
```

### Step 2 — Checkpoint

```
Accept scope? [yes / merge children / split further / single issue after all]
```

---

## Phase 3: Pre-Spec Generation (auto-run)

**Goal:** produce the complete issue body — matching `/dev-lifecycle`'s intake bar.

For each issue to be created (parent + any children), generate the body using the canonical template below. Run Steps 1–4 sequentially; the output of each feeds the next.

### Step 1 — Gather documentation references

If a documentation-lookup MCP (e.g. Context7) is available, use it to fetch current docs for any external library/framework mentioned in the topic — resolve the library id, then query the relevant feature. If no such MCP is available, fall back to the library's official docs URL. This is best-effort: skip it for changes with no external-dependency surface.

Glob `.claude/rules/*.md` and match the discovered rule files against the topic's keywords/surfaces to identify applicable domain rules. Read each matching rule and treat it as a hard constraint — in particular, any rule whose subject matter is directly touched by the change (e.g. a crypto/key rule when key material is involved, a migrations rule when SQL schema changes) is MANDATORY to cite. Do not assume specific rule filenames; use whatever `*.md` files the repo actually ships under `.claude/rules/`.

Build a `references` list: external doc URLs + internal rule file paths + any OWASP / RFC / NIST standard that applies.

### Step 2 — Design Dossier via catalog-scoped design-class dispatch (architect fallback)

Run the catalog's "## Dynamic skill scoping (mandatory)" algorithm for Phase 2/Pre-Spec against the proposed issue's signature (labels, predicted file surfaces, title/body keywords). Dispatch every matched **`design-class`** skill (`/engineering:architecture`, `/engineering:system-design`, `/frontend-design:frontend-design`, `/engineering:testing-strategy`, etc.) via the `Skill` tool and capture each one's artifact (ADR link, service-boundary sketch, component notes) into the issue body's `## Design Dossier` section. Emit the visibility line: `Scoped skills (Pre-Spec): [...]`.

**Fallback when no design-class skill matches** (trivial bug/chore): dispatch the **`@architect`** agent for a lightweight options pass so small issues still get an options section, and record `Scoped skills: none (design-class) — architect fallback`. This replaces the retired `planner` agent. Architect prompt:

```
Design 2–3 implementation options for this proposed issue:

Topic: "$0"
Scope verdict: [single | parent-with-children — list]
References gathered: [from Step 1]

For each option, produce:
- A short name ("Approach A: Direct integration")
- How it would be built in 2–3 sentences
- Risks / tradeoffs (security, performance, complexity, migration pain)
- Rough estimate (S/M/L)

Recommend the default option with one-line rationale.
Target: 300–500 words.
```

Capture the returned options — they'll appear in the body's "Implementation Options" section.

### Step 3 — Security considerations pass

If any of these apply, run the security pass (cite the matching `.claude/rules/*.md` file discovered in Step 1, if the repo ships one, plus the standard):
- Authentication, authorization, session — OWASP A07
- Crypto / key material — OWASP A02, NIST SP 800-57
- User input surfaces — OWASP A03 (injection), A05 (XSS)
- Network/HTTP — OWASP A02 (misconfig); secure-headers hardening if a web surface
- File/resource access — OWASP A01 (broken access control)
- Dependencies — OWASP A06 (vulnerable components)

Embed at least the security concerns that **apply**; skip ones that don't.

### Step 4 — Build body from canonical template

```markdown
## Problem

[1–3 paragraphs. What's broken, missing, or needed. Include concrete user-facing or developer-facing impact.]

## Goal / Definition of Done

[1 paragraph. What "shipped" looks like in observable terms.]

## Scope

- [ ] [Concrete task 1]
- [ ] [Concrete task 2]
- [ ] ...

[If parent-with-children, replace Scope with:]
## Sub-Issues

- [ ] #XXX — [Child title 1]
- [ ] #YYY — [Child title 2]

## Acceptance Criteria

- [ ] [Testable outcome 1]
- [ ] [Testable outcome 2]
- [ ] Tests cover happy-path and error-path per this repo's testing conventions (the relevant `.claude/rules/*.md`, if any)
- [ ] **Docs to update:** [list every doc this change will make stale — `docs/**`, `.claude/rules/**`, `CLAUDE.md`, READMEs, ADRs, runbooks. These ship IN the implementing PR, never a follow-up (per the docs-in-PR discipline). Write "none" only if the change touches no documented behavior.]
- [ ] **Quality gate:** meets this repo's test/quality conventions, if any (e.g. a coverage threshold and a clean static-analysis/security-scan run as defined by the repo's quality-gate doc)

## Design Dossier

[From matched `design-class` skills (per § Dynamic skill scoping): ADR links, system-design sketches, frontend-design notes. If no design-class skill matched, this degrades to 2–3 implementation options from the `@architect` agent, recommended default marked. `/dev-lifecycle` Phase 2 reads this section as the design basis.]

## Security Considerations

[Only fill if applicable. Reference `.claude/rules/*.md` and relevant OWASP categories.]

## Testing Strategy

- Unit tests: [what to cover]
- Integration tests: [what to cover — using this repo's integration-test setup, if any]
- E2E tests: [end-to-end coverage if user-visible, using the repo's E2E harness if it has one]
- Manual verification: [if any]

## Dependencies

- **#NNN** — [blocking — why it must land first]
- **#NNN** — [related — not blocking but concurrent consideration]

## Related

- **#NNN** — [cross-reference]
- PR #XXX — [recent work that informs this issue]

## Parent Epic (if sub-issue)

- **#PPP** — [parent title]

## References

- Internal: `.claude/rules/XXX.md`
- External: [library docs, OWASP, RFC, NIST]
- Prior art: [similar issues/PRs in this repo]

## Notes

Created via `/issue-creation` on YYYY-MM-DD.
```

Write the body to `/tmp/issue-creation-<slug>.md` for each issue to be created.

---

## Phase 4: Labels + Milestone + Priority + Project Fields (auto-run)

**Goal:** apply consistent metadata so the issue is immediately filterable and `/dev-lifecycle`-ready. This covers BOTH issue-level metadata (labels, milestone) AND Project v2 field metadata (Priority, Status, Size). The two are separate — setting a label does NOT propagate to the project field.

### Step 1 — Infer labels

**Source of truth:** the repo's own label taxonomy, discovered live via `gh label list`
in Phase 1 Step 1 (`/tmp/issue-creation-labels.json`). If the repo configures a
canonical labels doc under `labels.source` in `.claude/repo-config.json` (default
`.claude/rules/labels.md`), read it too for namespace semantics:

```bash
LABELS_DOC=$(rc_get '.labels.source' '.claude/rules/labels.md'); [ -f "$LABELS_DOC" ] && Read "$LABELS_DOC"
```

Apply labels per the decision trees below, but **only ever apply labels that actually exist** in the discovered
list — if a namespace below has no matching labels in this repo, skip it.

**Label namespaces** come from `repo-config.json` `.labels.namespaces`
(default the documented set: `["type","area","domain","release","risk","routine","workflow-validation"]`):

```bash
NAMESPACES=$(rc_get '.labels.namespaces | join(" ")' "type area domain release risk routine workflow-validation")
```

**Mandatory namespaces** (apply exactly one of each *when the repo defines that namespace*):

| Namespace | Cardinality | When to apply |
|---|---|---|
| `type:` | exactly one | issue lifecycle classification |
| `area:` | one or more | code-surface dimension (where the change lives) |
| `release:` | exactly one | release-tracking (only if the repo uses `release:` labels) |

**Additive namespaces** (apply only when relevant *and* the namespace exists in this repo):

| Namespace | Cardinality | When to apply |
|---|---|---|
| `domain:` | additive | business / concern dimension (what the change is about) |
| `risk:` | additive | when a security / privacy / breaking-change / data-loss / release-gate concern is present |
| `routine:` | additive | only for auto-generated issues (daily security review, weekly deps, etc.) |

**Decision tree for `type:` (mandatory, exactly one)**
- New user-facing capability → `type: feature`
- Broken behavior → `type: bug`
- Improvement to existing feature → `type: enhancement`
- Tooling, refactor, docs, deps, CI → `type: chore`
- Parent with children (use sub-issues, not bare epic label) → `type: epic`

**Decision tree for `area:` (mandatory, one or more — code-surface dimension)**
Map the change to the repo's `area:` labels, discovered via `gh label list`. Pick the label(s) whose name/description best match where the change physically lives (the affected service, package, or directory). Common patterns to match against the discovered list: a CI/release-pipeline area for `.github/workflows` changes, a docs area for the `docs/` tree, a deps area for dependency bumps, a tooling area for `.claude/` work. Never invent an `area:` value that isn't in the discovered list — if none fits, report the gap (see "Does not create labels" below).

**Decision tree for `release:` (mandatory if the repo uses `release:`, exactly one — release-tracking)**
Derive the available `release:` values from the live label list (`gh label list`), not from any fixed roadmap. Match the change to the release line it belongs to; use the repo's "no specific version / post-roadmap" value (e.g. `release: future`) if one exists and the work isn't tied to a planned version. If the repo defines no `release:` labels at all, skip this namespace.

**Decision tree for `domain:` (additive — business / concern dimension)**
Map the change to the repo's `domain:` labels, discovered via `gh label list`. Match on what the change is *about* (its concern) rather than where it lives. Apply zero or more as relevant; only use values present in the discovered list.

**Decision tree for `risk:` (additive — only when relevant)**
- Any security surface touched → `risk: security`
- Privacy-specific concerns (consent flows, PII scrubbing, GDPR) → `risk: privacy`
- Blocks the milestone tag → `risk: release-gate`
- API / contract break → `risk: breaking-change`
- Potential for data loss in operation or migration → `risk: data-loss`

**Note: Priority is a project board field, NOT a label** (when a board is configured). If `rc_has '.project'` and the board defines a Priority field, prompt the developer for the priority and set it via the Step 4 mutation. Do NOT apply any `P:*` priority label — priority lives on the board, not in the label taxonomy. When no project board is configured, skip priority entirely.

**Note: Status is also a project board field**, not a label — set via the Step 4 mutation (only when a board is configured).

### Step 2 — Infer milestone

Discover the repo's open milestones at runtime and map the chosen `release:` value (or, if the repo has no `release:` labels, the change's target version) onto the best-matching milestone:

```bash
gh api "repos/$OWNER/$REPO/milestones?state=open" --jq '.[].title'
```

Pick the milestone whose title corresponds to the release line the issue belongs to. If the work has no specific target version (e.g. a `release: future`-style value) or the repo defines no milestones, attach no milestone. Never assume a fixed milestone naming scheme — match against the titles the repo actually returns.

### Step 3 — Validate issue-level metadata

Before Phase 5, double-check:
- No conflicting `type:` labels (e.g., `bug` + `chore`)
- No conflicting `release:` labels (pick one), if the repo uses that namespace
- Milestone aligns with the chosen release line, where a milestone applies
- Priority is planned — only when a board is configured (`rc_has '.project'`)
- For parent epics, the `type: epic` label is present AND the body has a tasklist

### Step 4 — Project v2 field plan (config-gated)

**This entire step is gated on a configured project board.** Run:

```bash
if ! rc_has '.project'; then
  echo "no project board configured — skipping board fields"
  # → no Priority/Status/Size plan; the issue still gets labels + milestone in Phases 1–7.
fi
```

When `.project` is absent or null, **skip this step entirely** and proceed to Phase 5 — the issue is created with labels + milestone only. Everything below runs **only** inside the `rc_has '.project'` branch.

When a board IS configured, read all board identifiers from `.claude/repo-config.json` — never hardcode them:

```bash
PROJ_ID=$(rc_get '.project.id')                    # PVT_… board node id
PROJ_NUMBER=$(rc_get '.project.number')            # for `gh project item-add`
PROJ_OWNER=$(rc_get '.project.owner' "$OWNER")     # falls back to the gh-authed owner
PRIORITY_FIELD=$(rc_get '.project.fields.priority.id')
STATUS_FIELD=$(rc_get '.project.fields.status.id')
SIZE_FIELD=$(rc_get '.project.fields.size.id')
# Option ids are looked up by name from the config, e.g.:
#   PRIORITY_OPT=$(rc_get '.project.fields.priority.options.P1')
#   STATUS_OPT=$(rc_get '.project.fields.status.options.in_review')
#   SIZE_OPT=$(rc_get '.project.fields.size.options.M')
```

The board has custom single-select fields **independent from labels**. Which fields and options exist is entirely repo-defined (read `.project.fields.*` from config); the rows below describe the *roles* a board typically configures, not a required schema:

| Project Field | Source / role |
|---------------|---------------|
| **Priority** | `.project.fields.priority` — set from a developer prompt (urgency → the option names the repo defines, e.g. Critical→`P1`, Blocker→`P2`, …). Only set if the field is configured. |
| **Status** | `.project.fields.status` — a new issue defaults to the repo's "backlog" option; promote to "ready" only if the spec is fully intake-ready. |
| **Size** | `.project.fields.size` — optional T-shirt size if the implementation-options analysis produced a clear estimate. |
| **Milestone** | (built-in) auto-syncs from the issue milestone — no separate action needed. |
| **Parent issue** | (built-in) auto-linked from parent's tasklist checkbox `- [ ] #NNN`; explicit API call as fallback. |
| **Sub-issues progress** | (built-in) auto-calculated from child completion — no action. |

**Single-select field mutation template** (works for Priority, Status, or Size — substitute the field id and option id resolved from config):

```bash
gh api graphql -f query="mutation {
  updateProjectV2ItemFieldValue(input: {
    projectId: \"$PROJ_ID\",
    itemId: \"<PROJECT_ITEM_ID>\",
    fieldId: \"$PRIORITY_FIELD\",
    value: { singleSelectOptionId: \"$PRIORITY_OPT\" }
  }) { projectV2Item { id } }
}"
```

> Field and option ids live in `.claude/repo-config.json` under `.project.fields.*`. If the board schema changes, refresh them there — re-discover with `gh api graphql -f query="{ user(login: \"$PROJ_OWNER\") { projectV2(number: $PROJ_NUMBER) { fields(first:30) { nodes { ... on ProjectV2SingleSelectField { id name options { id name } } } } } } }"` (use `organization(login:)` instead of `user(login:)` for an org-owned board) and write the ids back into the config.

---

## Phase 5: Relationship Scan ⚠️ CHECKPOINT

**Goal:** identify and embed every relevant cross-reference before creation.

### Step 1 — Dependency detection

Search for issues the new one depends on:
- Issues referenced in the topic or scope (`#NNN` mentions by user)
- Issues in the same theme with `epic` label (likely parents)
- Issues blocked by this one — search for issues whose body mentions the new topic's keywords

```bash
gh issue list --state open --search "KEYWORD" --limit 30 --json number,title,state
```

### Step 2 — Recent PR context

Look at recent merged PRs (last 30 days) on the same surface:

```bash
git log --since="30 days ago" --oneline -- <relevant-paths>
gh pr list --state merged --limit 20 --json number,title,mergedAt,files
```

Identify PRs that:
- Shipped related functionality (cite as "building on PR #NNN")
- Introduced the current gap this issue addresses (cite as "gap identified after PR #NNN")

### Step 3 — Rule file citations

For each applicable rule discovered by globbing `.claude/rules/*.md` (Phase 3 Step 1), cite it in `## References` using its relative path. Cite only the rule files this repo actually ships.

### Step 4 — Related issue embedding

Update the body's `## Related`, `## Dependencies`, and `## Parent Epic` sections with full citations.

### Step 5 — Checkpoint

```
Relationship scan complete. Found:
- N dependencies (blocks: #...)
- M related issues (cross-references: #...)
- K relevant PRs (#...)
- Rule files applicable: [list]

Review the embedded references? [yes / trust-it / revise]
```

---

## Phase 6: Preview ⚠️ CHECKPOINT

**Goal:** show the complete issue (title + body + labels + milestone) before any mutation.

Print:

```
────────────────────────────────────────
PREVIEW — about to create

Title: <full title>
Milestone: <name>
Labels: <comma-separated>

Body:
<full body from /tmp/issue-creation-<slug>.md>
────────────────────────────────────────

If this is a parent-with-children, the children will be created AFTER
the parent exists so their bodies can reference the parent number.

Create? [yes / edit title / edit body / change labels / cancel]
```

**Never create without explicit "yes" here.**

---

## Phase 7: Creation ⚠️ CHECKPOINT for parent-absorbing path

### Step 1 — Create the issue

```bash
gh issue create \
  --title "<title>" \
  --body-file /tmp/issue-creation-<slug>.md \
  --label "<comma-separated labels>" \
  --milestone "<milestone>"
```

Capture the returned issue URL as `NEW_URL` and parse the issue number as `NEW` (e.g. `NEW=${NEW_URL##*/}`).

### Step 2 — Verify project auto-add + populate fields (config-gated)

**Skip this whole step unless a board is configured.** Structured guard:

```bash
if rc_has '.project'; then
  source "$(git rev-parse --show-toplevel)/scripts/repo-config.sh"
  OWNER_REPO=$(gh repo view --json nameWithOwner -q .nameWithOwner); OWNER="${OWNER_REPO%%/*}"; REPO="${OWNER_REPO##*/}"

  PROJ_ID=$(rc_get '.project.id')
  PROJ_NUMBER=$(rc_get '.project.number')
  PROJ_OWNER=$(rc_get '.project.owner' "$OWNER")
  PRIORITY_FIELD=$(rc_get '.project.fields.priority.id')

  # Verify presence + fetch project item ID (owner/repo derived at runtime)
  ITEM_ID=$(gh api graphql -f query="{ repository(owner: \"$OWNER\", name: \"$REPO\") { issue(number: $NEW) { projectItems(first: 3) { nodes { id project { id } } } } } }" \
    --jq ".data.repository.issue.projectItems.nodes[] | select(.project.id == \"$PROJ_ID\") | .id" | head -1)

  # If empty, add manually (the board's auto-add workflow may be off):
  if [ -z "$ITEM_ID" ] || [ "$ITEM_ID" = "null" ]; then
    ITEM_ID=$(gh project item-add "$PROJ_NUMBER" --owner "$PROJ_OWNER" --url "$NEW_URL" --format json | jq -r .id)
  fi

  # Set Priority (mandatory when a board IS configured — resolve the option id from config by name):
  #   PRIORITY_OPT=$(rc_get ".project.fields.priority.options.$CHOSEN_PRIORITY")
  gh api graphql -f query="mutation { updateProjectV2ItemFieldValue(input: { projectId: \"$PROJ_ID\", itemId: \"$ITEM_ID\", fieldId: \"$PRIORITY_FIELD\", value: { singleSelectOptionId: \"$PRIORITY_OPT\" } }) { projectV2Item { id } } }"

  # Optional: Set Size (.project.fields.size) if an estimate was produced in Phase 3
  # Optional: Promote Status (.project.fields.status) from the repo's backlog option → ready if intake-ready
else
  echo "no project board configured — skipping board fields"
fi
```

### Step 3 — Parent/child wiring (if applicable)

**Sub-issue path — parent tasklist update:**

```bash
# Fetch parent body
gh issue view PARENT --json body --jq '.body' > /tmp/parent-body.md
# Append " - [ ] #NEW — Title" to the "## Sub-Issues" section (or create section)
# Write back
gh issue edit PARENT --body-file /tmp/parent-body.md
```

**Verify native parent relationship was created by the tasklist edit:**

```bash
REPO_FULL=$(gh repo view --json nameWithOwner --jq '.nameWithOwner')
OWNER=${REPO_FULL%%/*}
REPO=${REPO_FULL#*/}

ACTUAL=$(gh api graphql -f query="{ repository(owner: \"$OWNER\", name: \"$REPO\") { issue(number: $NEW) { parent { number } } } }" --jq '.data.repository.issue.parent.number // "NONE"')

if [ "$ACTUAL" != "$PARENT" ]; then
  # Fallback: force native sub-issue relationship via REST API
  # (GitHub auto-detects tasklists but occasionally misses; this ensures linkage)
  SUB_ID=$(gh api repos/$OWNER/$REPO/issues/$NEW --jq .id)
  gh api --method POST repos/$OWNER/$REPO/issues/$PARENT/sub_issues -F sub_issue_id=$SUB_ID
fi
```

Always run this verification — the tasklist auto-detection has edge cases around issue numbers inside descriptive text, retroactive body edits, and issues that previously had a different parent.

### Step 4 — New-parent-with-absorption path ⚠️ EXTRA CHECKPOINT

This is the only path that modifies existing issues' bodies — bigger blast radius.

```
About to absorb existing issues #AAA, #BBB, #CCC into new parent #PARENT.
This will:
  1. Create the new parent issue with tasklist of {#AAA, #BBB, #CCC, + new issue}
  2. Post a comment on each of #AAA/#BBB/#CCC noting the new parent (non-destructive)
  3. NOT modify #AAA/#BBB/#CCC bodies (audit pattern — parent owns the tasklist, children stay readable standalone)
  4. Run Phase 7 Step 2 (board fields, if a project is configured) for the new parent AND verify Step 3 (native parent link) for each absorbed child

Proceed? [yes / only-create-parent-skip-absorption / cancel]
```

On approval:
1. Create parent (as standalone).
2. For each absorbed issue, `gh issue comment N --body "Now tracked under #PARENT per issue-creation on YYYY-MM-DD."`
3. Create the new issue (if separate from parent).
4. Run Step 2 for all created issues.
5. Run Step 3 verification for each child.

---

## Phase 8: Post-Creation (auto-run)

Print summary:

```
✅ Issue created: #NNN
   URL: https://github.com/...

Next steps:
  - Begin work:      /dev-lifecycle NNN
  - Re-read brief:   /issue-brief NNN
  - Check adjacency: gh issue view NNN
```

If the issue lacks acceptance-criteria specificity, note it. If a board is configured and the chosen priority is debatable, flag it too: "Flag: priority may be too high / too low — reassess after 1 week of triage."

---

## What This Skill Does NOT Do

- Does not start implementation work (that's `/dev-lifecycle`)
- Does not skip the adjacency-scan checkpoint, even when user says "just make it"
- Does not modify `.claude/rules/` or `CLAUDE.md`
- Does not close existing issues on its own (explicit audit job)
- Does not apply `release-gate` or `security` labels speculatively — only when scope genuinely demands them
- Does not create labels — if a needed label doesn't exist, it reports the gap and asks the user to confirm creation via `gh label create` before proceeding
- Does not bypass Phase 6 preview for any reason
- Does not do `gh issue create` with a missing `type:` or missing mandatory namespace labels that the repo defines; when a board is configured, also not with a missing milestone (where one applies) or missing priority
- **Does not leave a configured project's Priority field UNSET** — when `rc_has '.project'` and the board has a Priority field, setting it is mandatory (Phase 7 Step 2). Labels alone are insufficient; the project field is a separate data plane that board views (and your repo's status skill, if present) read from. When no board is configured, there is no Priority field to set.
- **Does not assume tasklist edits propagate to native sub-issue links** — the verification-and-fallback in Phase 7 Step 3 is mandatory.

## Why This Skill Exists

Ad-hoc issue creation tends to accumulate label/milestone conflicts, orphan issues (no milestone), under-specified issues that can't pass `/dev-lifecycle` intake, and avoidable duplicates. The root cause is issue creation without systematic consolidation, scope, or metadata discipline. `/issue-creation` enforces that discipline at the moment of creation — cheaper than auditing after the fact.

## Companion Hook

If the repo installs the companion `.claude/hooks/issue-creation-nudge.sh` hook, it watches for user prompts containing intent keywords ("create an issue", "file an issue", "open an issue", "make a new issue", "add an issue") and prints a non-blocking reminder to use `/issue-creation`. The hook does NOT block raw `gh issue create` calls — tooling (e.g., `/dev-lifecycle` spawning child issues) can still use them directly. The hook is optional; this skill works the same whether or not it is present.
