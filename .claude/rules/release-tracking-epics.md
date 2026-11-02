# Release-Tracking Epic Rules

Path-scoped discipline for version-epic issues. This file is the load-bearing summary of the org-wide release tracking method.

## Discipline Summary

- **One epic per semver bump.** Title: `Epic: vX.Y.Z release`. One per minor or major version. No constellation-level tracking epics — codenames, if any, map 1:1 to versions in the repo CLAUDE.md, not via labels.
- **Labels:** `type: epic` plus `release: <stage-or-version>` (e.g., `release: 1.0`, `release: 1.1`). No codename labels — the codename-to-version mapping lives exclusively in the repo's CLAUDE.md.
- **Milestones:** existing per-version GitHub milestones remain canonical (`v1.0.0`, `v1.1.0`, etc.). Epics ALSO live on those milestones.

## Sub-issue Attachment Is REST-API Only

Children are attached via the REST `/sub_issues` endpoint, NEVER via `- [ ] #N` tasklist markdown.

**Two non-obvious gotchas (verified against the GitHub REST API in a live epic rollout):**

1. **The `sub_issue_id` body parameter wants the issue's internal numeric ID, not the user-visible issue number.** The parent is identified by issue number in the URL path; the child is identified by internal ID in the request body. Look up the child's internal ID via `gh api repos/<owner>/<repo>/issues/<number> --jq '.id'`.
2. **Use `-F` (uppercase) for typed integer parameters,** not `-f` (lowercase string). With `-f sub_issue_id=N`, gh sends `"N"` (string) and the API returns HTTP 422 "is not of type integer". With `-F sub_issue_id=N`, gh sends `N` (integer) and the API accepts it.

The correct incantation:

```bash
# Look up the child's internal id first:
CHILD_ID=$(gh api repos/<owner>/<repo>/issues/<child-issue-number> --jq '.id')

# Then attach:
gh api repos/<owner>/<repo>/issues/<epic-issue-number>/sub_issues \
  -X POST -F sub_issue_id="$CHILD_ID"
```

Bulk attachment loop pattern (when migrating many children to a parent epic):

```bash
for n in <child-issue-numbers>; do
  child_id=$(gh api repos/<owner>/<repo>/issues/$n --jq '.id')
  gh api repos/<owner>/<repo>/issues/<epic-#>/sub_issues \
    -X POST -F sub_issue_id="$child_id"
done
```

**Note on the tree invariant:** sub-issue relationships are strict trees. An issue can have only one parent. If a child is already attached to another parent, the POST returns HTTP 422 "Sub issue may only have one parent". Bulk-attaching a flat list to a new epic when those issues already have parents will partially fail by design — only the top-level parents attach, and their existing children remain reachable transitively. This is correct behavior, not a bug.

Per memory entry "Tasklist auto-link unreliable," GitHub's `- [ ] #N` heuristic frequently fails on programmatic body edits. The REST endpoint is deterministic. Do NOT mix the two patterns on the same epic.

## Lifecycle States

```
Planning → In Progress → Released → Retrospected → Closed
```

- **Planning:** epic exists, scope volatile.
- **In Progress:** children are being actively worked.
- **Released:** version tag is published, release notes filed.
- **Retrospected:** retrospective comment posted on the epic (sections: "What shipped" / "What slipped" / "What we learned").
- **Closed:** terminal state, reached after retro comment.

## Project Board Discipline Rule

The repo's project board (if it auto-closes on Status=Done) auto-closes issues when moved to `Status: Done`. Version epics ARE on the board, but:

- **Never move a version epic to `Status: Done` until the retrospective comment is posted.**
- The Done move IS the natural close action — but only after Retrospected is reached.
- Speculative Done moves before retro are destructive and forbidden.

## Branch-Name Caveat

GitHub auto-detects closing-issue references from branch names matching `feature/<N>-*` (per memory entry "Branch-name auto-link triggers project-board auto-close"). When working on or referencing a version epic:

- Use `release/1.X` or `chore/v1.X-*` branch names — NOT `feature/<epic-number>-*`.
- A `feature/<epic-number>-*` branch would auto-close the epic the wrong way (via project-board automation, bypassing the intentional migration comment).

## Verification Signals

The org equivalent verification checks (if present):

- One open `type: epic` per active version.
- All children attached via REST `/sub_issues` (not markdown tasklists).
- Closed epics have a retrospective comment tagged with `## Retrospective`.
- Codename (if version has one) — verify by cross-referencing the epic's `release: *` label against the CLAUDE.md codename-to-version table. (Codename labels are not used; the table is the only source of truth.)
- No orphan children of closed epics.

## Cross-references

- Repo `CLAUDE.md` "RELEASE ROADMAP" — narrative ground truth for version and codename mapping
- `.claude/skills/dev-lifecycle/SKILL.md` Phase 1 — intake check for parent-epic linkage (if present in repo)
