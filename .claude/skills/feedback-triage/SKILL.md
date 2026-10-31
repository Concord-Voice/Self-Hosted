---
name: feedback-triage
description: Daily routine that triages a PUBLIC feedback repo — catalogs open user-submitted Bug Reports / Feature Requests, consolidates duplicates (oldest survives; newer ones closed and counted via a managed report-counter + 👍 seed), opens or updates the matching tracking issue in a private tracker repo via /issue-creation, back-annotates each public issue with its internal tracking ref and release window, maintains a public pinned "Known Issues" list users can check before filing, and closes public issues once the tracked fix ships. Use whenever asked to triage, consolidate, dedupe, or sync user feedback, to update the Known Issues list, to process the public feedback inbox, or to run the scheduled daily feedback pass — even when the user just says "go through the feedback", "dedupe the public issues", or "sync feedback to the tracker".
argument-hint: "[--mode=auto|dry-run|gated] [--public=owner/repo] [--tracker=owner/repo] [--limit=N]"
allowed-tools:
  - Bash(gh issue *)
  - Bash(gh search *)
  - Bash(gh api *)
  - Bash(gh label *)
  - Bash(gh repo *)
  - Bash(git log *)
  - Bash(date *)
  - Bash(jq *)
  - Read
  - Grep
  - Glob
  - Write
---

<DISPATCH-GATE>
When a phase says "Dispatch `/issue-creation`" or "Dispatch the Explore agent", you MUST invoke it via the Skill or Agent tool. Do NOT substitute an inline `gh issue create` for the tracker-side tracking issue — `/issue-creation` owns label/milestone/board/epic discipline; this skill owns the sequencing and the cross-repo wiring. This is not optional.
</DISPATCH-GATE>

<SAFETY-PREAMBLE>
This routine mutates a **PUBLIC, source-available repo** (closing user issues, posting public comments) and a **private tracker**. Five properties are non-negotiable and are the reason this skill exists rather than ad-hoc `gh` commands:

1. **Idempotent.** It runs every 24h. Re-running with no new feedback MUST be a no-op — never double-count a report, re-close a closed dupe, re-post a status comment, or create a second tracker issue. State lives in a per-issue **managed status comment** (`references/markers-and-templates.md`) that the routine re-reads first; every action fires only on a real state transition, cross-checked against durable GitHub state.
2. **Reversible-only.** Never delete. Closing is the only destructive action, and it is recoverable.
3. **Bounded.** The auto-mode safety envelope (Phase 0) caps blast radius. Exceeding a cap downgrades that batch to a digest entry instead of silently truncating.
4. **Structural fields only into shell.** Only structural values (issue number, the public-issue URL, label names) are ever interpolated into a `gh`/shell command. User-authored title/body reaches tools via `--json` / stdin / a file — never spliced onto a command line. This holds even when an autonomous run improvises an un-pseudocoded step (e.g. never build `gh search "<user title>"`).
5. **Write-surface scope.** The public-repo writes here are *issue/comment* mutations under the `FEEDBACK_PAT`-family `issues:write` scope — a distinct, sanctioned surface from any git-tree sole-writer invariant your repo records in a deployment rules file (which would govern `contents:write` on `main`). This routine never touches the git tree.
</SAFETY-PREAMBLE>

# Feedback Triage

Daily bidirectional sync between the public feedback repo and the private tracker repo, plus a public **Known Issues** list users can check before filing.

```
PUBLIC  example-org/example-repo              (user Bug Reports / Feature Requests land here)
   │  Phase 1 catalog → 2 cluster → 3 consolidate → 4 sync → 5 status-note → 6.5 regen Known Issues
   ▼
PRIVATE example-org/example-repo-private      (tracking issues, epics, board, releases)
   ▲
   │  Phase 6 reverse-sync: tracked fix shipped → close/annotate the public issue
```

The **Known Issues** list (Phase 6.5) is a single **pinned issue** in the public repo whose body the routine regenerates each run from the same per-issue marker state. The public *file tree* is mirror-locked (sole-writer App; see `references/known-issues.md`), so a pinned **issue** — not a `KNOWN_ISSUES.md` file — is the writable, automation-native surface. Every consolidation and tracking comment links the reporter to it ("we're already aware — it's on our Known Issues list").

## Modes

| Mode | Flag | Mutations | When |
|---|---|---|---|
| **auto** (default) | `--mode=auto` | Applies everything autonomously, within the Phase 0 safety envelope. | The scheduled daily cloud routine. |
| **dry-run** | `--mode=dry-run` | **None.** Produces the full plan + digest only. | First week (build trust); any time you want a preview. |
| **gated** | `--mode=gated` | Additive/non-destructive auto; batches closures + `/issue-creation` calls into the digest and waits for approval. | Interactive human runs. |

Resolve mode from `$ARGUMENTS` (default `auto`). **Always read [`references/autonomous-mode.md`](references/autonomous-mode.md) before any mutating phase** — it defines per-mode gate behavior, the cloud-routine contract, and how `/issue-creation`'s human checkpoints are reconciled in an unattended run.

---

## Phase 0 — Resolve context + safety envelope (read-only)

```bash
PUBLIC=${public:-example-org/example-repo}         # source-available feedback repo (FEEDBACK_REPO)
TRACKER=${tracker:-example-org/example-repo-private}  # private tracker
TODAY=$(date -u +%Y-%m-%d)
```

1. **Auth + scope check.** Confirm `gh auth status` has `issues:write` on both repos and `project` scope (tracker board mutations via `/issue-creation`). On `INSUFFICIENT_SCOPES`, surface `gh auth refresh -s project,read:project` and stop.
2. **Load the safety envelope** (auto mode). Defaults — override only with explicit approval:
   - `MAX_CLOSURES_PER_RUN=15` (consolidation closes + fixed-issue closes, combined)
   - `MAX_CLUSTER_SIZE=8` (a proposed dupe cluster larger than this is flagged for human review, never auto-closed)
   - `MIN_CLUSTER_CONFIDENCE=high` (only same-root-cause / same-feature matches auto-consolidate; "same area" is NOT enough)
   - When a cap would be exceeded, that batch is recorded in the digest as `deferred: cap exceeded (<which>)` — **logged, never silently dropped** (no-silent-caps discipline).
3. **Resolve the Known Issues issue.** Locate the pinned Known Issues issue by sentinel (read-only). In a mutating mode (auto / gated-apply), create + pin it if absent — first run only — so Phases 3 / 5 / 6.5 can link and maintain it; in dry-run, just note its absence (the link would resolve once created). See [`references/known-issues.md`](references/known-issues.md).
4. Print the run header: mode, repos, caps, Known Issues issue #, `TODAY`.

---

## Phase 1 — Catalog the public inbox (read-only)

Pull **open** issues plus issues **closed in the last 2 days** (to reconcile in-flight reverse-sync). The GitHub author is the same feedback bot on every issue — **never key on author**; the real reporter is the non-reversible per-reporter correlation token in the body (`**Reported by:** report \`<token>\`` for bugs / `**Requested by:** report \`<token>\`` for features).

```bash
gh issue list -R "$PUBLIC" --state open  --limit 200 \
  --json number,title,body,labels,createdAt,updatedAt,comments,reactionGroups,author > /tmp/ft-open.json
gh issue list -R "$PUBLIC" --state closed --limit 60 --search "closed:>=$(date -u -v-2d +%Y-%m-%d 2>/dev/null || date -u -d '2 days ago' +%Y-%m-%d)" \
  --json number,title,body,labels,closedAt > /tmp/ft-closed-recent.json
```

For each open issue derive its **state record** (see [`references/idempotency.md`](references/idempotency.md) for the full re-derivation rules):

- **kind** — `bug` (`[Bug Report]` / `type: bug`) or `feature` (`[Feature Request]` / `type: feature`).
- **reporter_id** — the dedup/uniqueness key; parsed from the `report \`<token>\`` line — now the per-user HMAC correlation token (stable per user; no longer a raw UUID).
- **managed marker** — parse the routine's status comment if present (`reports`, `consolidated`, `alpha`, `state`, `release`, `fixed_in`, `last_synced`). Absent ⇒ `state: untriaged`.
- **human-touched?** — any non-routine maintainer comment, or a `wontfix`/`duplicate`/`invalid`/`question` label. Human-touched issues are **reported in the catalog but excluded from auto-consolidation and auto-close** — a human already exercised judgment.
- **Routine artifacts excluded.** The routine's own pinned **Known Issues** issue (its body starts with the `<!-- feedback-triage:known-issues v1 -->` sentinel) is NOT user feedback — skip it entirely in catalog / cluster / consolidate. See [`references/known-issues.md`](references/known-issues.md).

Emit the catalog table: `# | kind | age | reporter | status | reports | alpha | flags`.

---

## Phase 2 — Cluster duplicates (read-only analysis)

**Goal:** group issues that are the *same report* (same bug / same requested feature), so the oldest survives and the rest fold in as demand signal.

Dispatch the **Explore** agent for the clustering pass (it reads bodies in parallel and won't dump them back). Prompt skeleton:

```
You are clustering open user-feedback issues in <PUBLIC> by whether they describe
the SAME underlying bug or the SAME requested feature. High precision over recall:
only group issues a maintainer would confidently merge. "Same surface/area" is NOT
a match (two different bugs in the message composer are two clusters).

Input: the catalog JSON (number, title, body, kind).  For each proposed cluster return:
  - members: [issue numbers], survivor = oldest createdAt
  - confidence: high | medium | low   (only `high` is eligible for auto-consolidation)
  - one-line rationale
  - kind (bug|feature) — never merge a bug with a feature
Also return singletons (no duplicate) explicitly. ≤500 words.
```

Build the **consolidation plan**: per high-confidence cluster, survivor = oldest; dupes = the rest; `merged_reports = Σ (each member's existing marker reports, default 1)`. Clusters above `MAX_CLUSTER_SIZE`, or below `high` confidence, become digest review items — not auto-actions.

---

## Phase 3 — Apply consolidation on the public repo (mode-gated)

Per eligible cluster (auto applies; dry-run prints; gated → digest). Order: update survivor first, then close dupes, so a mid-run crash leaves the survivor already counted. Full templates in [`references/markers-and-templates.md`](references/markers-and-templates.md).

**On the survivor:**
1. Upsert the **managed status comment**: set `reports = merged_reports`, append dupes to `consolidated`, refresh the visible "👥 Reported by N users" line. Locate-by-sentinel, then PATCH that comment by its integer id (see `references/markers-and-templates.md` § Locate + parse) — never a second comment, and never `gh issue comment --edit-last` (it has no sentinel awareness and would clobber a later consolidation/fixed comment).
2. Ensure exactly **one bot 👍** on the issue (`gh api .../reactions` is idempotent per account — adding again is a safe no-op; check first to avoid noise).

**On each dupe (skip if already closed with the consolidation sentinel):**
3. Post the consolidation comment (template in `references/markers-and-templates.md`) — it thanks the reporter, points to the survivor `#<survivor>`, AND links the **Known Issues** list so they see we're already aware and tracking it.
4. Add the `duplicate` label (exists in the repo; do **not** create labels).
5. Close as **not planned** (`gh issue close --reason "not planned"`).

Respect the closure cap; overflow → digest.

---

## Phase 4 — Sync survivors into the tracker repo (mode-gated)

For each **surviving, non-human-touched** issue that represents real work and whose marker has no `alpha:` ref:

1. **Idempotency guard (belt-and-suspenders).** Before creating, search the tracker for an existing back-reference (covers a prior run that created the issue but failed to write the marker). GitHub search **tokenizes** a raw URL (splitting on `/` and `.`), so a bare-URL query matches noise — any tracker issue mentioning the bare number `$N` would falsely hit. Quote the URL as a **phrase** AND post-filter on literal containment (the search is best-effort; the filter is authoritative):
   ```bash
   PUB_URL="https://github.com/$PUBLIC/issues/$N"
   gh search issues --repo "$TRACKER" "\"$PUB_URL\"" --json number,body \
     --jq "[.[] | select(.body | contains(\"$PUB_URL\")) | .number]"
   ```
   A **confirmed** hit (body literally contains `$PUB_URL`) ⇒ reuse it; skip creation; jump to recording the ref. No confirmed hit ⇒ create. The marker's `alpha:` field stays the primary record; this search is only the crash-recovery fallback.
2. **Dispatch `/issue-creation`** on the tracker with a tracking-request payload (topic, kind→labels, the public body, `reports=N` as the demand signal, and the public URL as a `Refs` back-reference). The dispatch contract — including how `/issue-creation`'s human checkpoints are answered in auto mode, the label/domain/release mapping, and the demand-signal→Priority mapping — lives in [`references/alpha-sync.md`](references/alpha-sync.md). Created tracking issues land at board **Status = Triage/Backlog**, never `Ready`: a human still vets scope before work starts.
3. Capture the new (or reused) `example-repo-private#NNN` and its **`release:` label** (the shipping stream) → the **release window** (`v0.2.0-Beta`, `v1.0.0`, … or `a future release`). The window derives from the ship stream, NOT the milestone (a complete-by deadline). Derivation in [`references/alpha-sync.md`](references/alpha-sync.md).

---

## Phase 5 — Back-annotate the public issue with tracking status (mode-gated)

Update the survivor's **managed status comment** so a public reader sees it's tracked. Per your disclosure choice, include the tracker ref/link (renders as plain text to anonymous viewers since the tracker is private; links for maintainers):

```
📌 Tracked internally as example-repo-private#NNN — tracked for an upcoming Beta update.
```

…or `tracked for a future release.` when no version is known. The public-facing window string is the marker `release` token passed through the **Public release-window display** normalization in [`references/markers-and-templates.md`](references/markers-and-templates.md) — never the raw `v0.2.0-Beta` token, which reads as an overdue patch (this project ships rolling Beta updates). The managed status comment also carries the **Known Issues** link, so a reader lands on the full list. Write `state: tracked`, `alpha`, `release` (the **stored token**, e.g. `v0.2.0-Beta`, NOT the display string — it is the idempotency comparator), `last_synced=TODAY` into the marker. **Skip entirely if the marker already records the same `alpha` + `release`** — no-op on unchanged state.

---

## Phase 6 — Reverse sync: shipped fix → close/annotate public (mode-gated)

For every public issue whose marker carries an `alpha:` ref, read the tracker issue's current state and post **only on a transition** (full state machine in [`references/idempotency.md`](references/idempotency.md)):

| Tracker state | Public action | Marker → |
|---|---|---|
| Closed + completed (merged fix PR / a shipped release build containing the fix) | Comment `✅ Fixed in <version> (shipped <date>) — thanks for reporting!` and **close** the public issue. | `state: fixed`, `fixed_in: <version>` |
| In progress / open fix PR | Update note: `🔧 Fix in progress, targeting <version>.` | `state: in-progress` |
| Still open, untouched | No public post (avoid noise). | unchanged |
| Closed as **not planned** / wontfix | Comment `We've decided not to pursue this for now — rationale: <tracker reason>.` and close. | `state: declined` |

Version derivation is case-aware (the milestone — a complete-by deadline — is never the target in either case):
- **Shipped fix** (`fixed_in`, row 1): prefer a concrete desktop release tag (`desktop-vX.Y.Z`) that includes the fix; else `a recent release`.
- **In-progress target** (`targeting <version>`, row 2): the fix hasn't shipped, so derive the target from the tracker issue's `release:` **shipping stream** via the release-window normalization in [`references/markers-and-templates.md`](references/markers-and-templates.md) (e.g. `release: beta` → "an upcoming Beta update") — NOT a desktop tag (none exists yet) and NOT `a recent release` (past-looking).

Respect the combined closure cap.

---

## Phase 6.5 — Regenerate the public Known Issues list (mode-gated)

Maintain ONE pinned **Known Issues** issue in `$PUBLIC` — the user-facing list of what's been reported and where it stands. Its body is **regenerated wholesale** each run from the per-issue marker state collected in Phases 1–6, so it's idempotent by construction (same state ⇒ same body ⇒ no edit). Full templates + create/pin/find mechanics in [`references/known-issues.md`](references/known-issues.md).

1. **Locate or create.** Find the pinned issue by the `<!-- feedback-triage:known-issues v1 -->` sentinel. If absent, create it (label `documentation`, pin via the GraphQL `pinIssue` mutation) — first run only.
2. **Build the body** from the surviving (non-dupe, non-human-closed) catalog issues, grouped by `state`:
   - 🔧 **In progress** (`state: in-progress`) — fix underway, with target version.
   - 📋 **Tracked** (`state: tracked`, plus surviving `untriaged`) — acknowledged, with release window.
   - ✅ **Recently fixed** (`state: fixed`, last ~30 days) — shipped, with `fixed_in` version.
   Within each group, sort by **report count desc** (demand-ranked). Each row: short title + `👥 N` + the release/fixed version + a link to the public survivor `#N`. Header tells users to check here before filing and to 👍 the linked issue to add their vote. **Counts + public issue link + inert tracker ref only** — no internal reporter IDs, no private tracker body content.
3. **Upsert.** Diff the freshly-built body against the current pinned-issue body; **PATCH the issue body only if it changed**, and ensure it stays pinned. Unchanged ⇒ no-op.

Mode-gated: **auto** applies; **dry-run** prints the would-be body; **gated** shows it in the digest. Editing one routine-owned issue is additive and fully reversible, so it is **not** bounded by the closure cap.

---

## Phase 7 — Digest + audit

Tally: cataloged, clusters consolidated (+reports rolled up), dupes closed, tracker issues created/reused, public issues marked tracked / in-progress / fixed-closed, **Known Issues list updated (yes/no + entry count by group)**, and every `deferred:` cap/low-confidence item.

- **auto:** post the digest as a comment on a pinned tracking issue in the **tracker** (audit log), and print the summary.
- **dry-run / gated:** the digest **is** the artifact — present it and (gated) wait for approval to apply the deferred mutations.
- Digest > ~400 lines → write to `docs/superpowers/reports/<date>-feedback-triage.md` and link it (per CLAUDE.md long-artifact discipline).

---

## What this skill does NOT do

- Does not create labels (public repo has a fixed minimal set; uses only `duplicate`/`type: *`).
- Does not delete anything — closures only, and only reversible ones.
- Does not auto-consolidate or auto-close **human-touched** issues — a maintainer's judgment wins.
- Does not key on the GitHub author (one bot authors them all) — the body's internal reporter ID is the identity.
- Does not fabricate demand: the bot's 👍 is a single seed; the real tally is the managed counter, and organic user 👍s are read, never forged.
- Does not bypass `/issue-creation` for the tracker side — that skill owns tracker discipline.
- Does not exceed the Phase 0 caps silently — overflow is logged in the digest.
- Does not write a `KNOWN_ISSUES.md` file to the public repo — the file tree is mirror-locked (sole-writer App); the Known Issues list is a routine-maintained **pinned issue** (`references/known-issues.md`).
- Does not let its own pinned Known Issues issue re-enter the feedback pipeline — it's sentinel-excluded from catalog / cluster / consolidate (Phase 1).

## Why this skill exists

User feedback lands in a public repo as an unbounded stream of `[Bug Report]`/`[Feature Request]` issues with no dedup, no demand-counting, and no link to the work that fixes them. Left alone it becomes noise: the same bug reported ten times reads as ten problems, and reporters never learn their issue was tracked or shipped. This routine turns that stream into a deduplicated, demand-ranked, bidirectionally-linked queue — and closes the loop back to the reporter when the fix ships.
