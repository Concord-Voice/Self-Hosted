# Tracker-side sync contract

How Phase 4 turns a surviving public feedback issue into a tracked item in the private `example-repo-private` repo, via `/issue-creation`.

## Dispatch contract

Dispatch `/issue-creation` (it is model-invocable — no `disable-model-invocation`) with a **tracking-request** built from the survivor:

- **Topic / title sketch:** the user's title with the `[Bug Report]`/`[Feature Request]` prefix stripped (e.g. `Reacting to a message with an emoji doesn't work`).
- **Body seed** handed to `/issue-creation`'s Pre-Spec:
  - The public report's `## Description` (verbatim, fenced).
  - `Demand: <reports> user report(s)` — the prioritization signal.
  - `Refs https://github.com/example-org/example-repo/issues/<N>` — the back-reference (full URL, **not** a bare `#N`, and **not** `Closes` — cross-repo `Closes` is wrong here and a bare number could auto-link the wrong issue).
- **Pre-answered metadata** (see mappings below) so `/issue-creation` doesn't have to guess.

`/issue-creation` then runs its own adjacency scan — so a feedback item that matches an existing tracker issue or epic is attached/cross-referenced rather than duplicated. That dedup-against-the-tracker is exactly why we route through it instead of `gh issue create`.

## Reconciling `/issue-creation`'s human checkpoints in auto mode

`/issue-creation` has five hard human checkpoints (adjacency decision, scope, relationships, preview, creation). In an **unattended auto run there is no human**, so the orchestrating model answers each checkpoint from this fixed, conservative policy (and records the answer in the digest for audit):

| Checkpoint | Auto-mode answer |
|---|---|
| Adjacency (Phase 1) | If a **strong duplicate** exists in the tracker → do NOT create; cross-reference it and record its number as `alpha`. If a **strong parent-epic** fit → create as sub-issue. Otherwise → standalone. |
| Scope (Phase 2) | Always **single issue** for a feedback item (one report = one landable change). Never auto-spawn an epic from feedback; if it looks epic-sized, create the issue and flag `needs-scoping` in the digest. |
| Relationships (Phase 5) | Accept the auto-detected links. |
| Preview (Phase 6) | Auto-accept. |
| Creation (Phase 7) | Create at board **Status = Triage** (or Backlog), **never Ready** — a human vets scope before work starts. |

In **gated** / **interactive** modes, do NOT pre-answer — dispatch the real `/issue-creation` and let the human hit each checkpoint (high-fidelity path). The pre-answer policy is an auto-mode-only accommodation, and the low board Status is its safety valve.

**Milestone (`/issue-creation` Step 2's deadline prompt) — auto-mode answer: leave it UNSET.** Step 2 sets the milestone (the complete-by *deadline*), a distinct axis from the `release:` label supplied above. In an unattended run there is no developer to answer that prompt, and feedback items land at **Triage/Backlog** precisely so a human vets them — that human assigns the deadline milestone then. Do NOT auto-apply Step 2's next-boundary default in auto mode: it would stamp an unvetted `v1.0.0`-class deadline on an item nobody has scoped. `release:` is still supplied (shipping stream); milestone is deliberately deferred to human triage.

## kind → label mapping

| Public kind | Tracker `type:` | Typical `area:` (refine from body) | Typical `domain:` |
|---|---|---|---|
| `[Bug Report]` | `type: bug` | infer from body: composer/chat → `area: desktop`; voice/video → `area: media-plane`; API/auth → `area: control-plane` | e.g. `domain: chat-systems`, `domain: ux` |
| `[Feature Request]` | `type: feature` | usually `area: desktop` | from the feature **Category** field (below) |

Feature-request **Category** values (from the in-app panel) map to a `domain:` hint:

| Category | `domain:` hint |
|---|---|
| New Feature | (none — infer from body) |
| Improvement to Existing Feature | (infer) |
| UI/UX Change | `domain: ux` |
| Performance | `domain: performance` |
| Other | (infer) |

`release:` defaults to `release: beta` (the active window) unless the body clearly describes post-1.0 scope. Let `/issue-creation` finalize labels against `.claude/rules/labels.md` — these are hints, not overrides.

## Demand → Priority hint

The report count is the whole point of consolidation — pass it through as a Priority suggestion (`/issue-creation` sets the board Priority field):

| `reports` | Suggested board Priority |
|---|---|
| ≥ 8 | P2 |
| 4–7 | P3 |
| 2–3 | P3 (note the count) |
| 1 | P4 |

This is a suggestion; a security/data-loss bug overrides upward regardless of count.

## Release-window derivation (for the public tracking note)

Resolve the **stored marker `release` token** (the idempotency comparator), in priority order:

1. The tracker issue's **`release:` label** → `release: beta`→`v0.2.0-Beta`, `release: 1.0`→`v1.0.0`, `release: 1.1/1.2/1.3`→`v1.1.0/…`, `release: future`→`future`.
2. Else → `future`.

The public window answers *"when does my fix **ship**?"* — a shipping-stream question — so it derives from the `release:` label (the stream the work rides), **not** the milestone. Under the corrected model (see `.claude/rules/labels.md` § Namespace: `release:` — "The two axes"; that file is installed by `install.sh claude`) the milestone is a *deadline* (the complete-**by** version), usually one version LATER than the ship stream; deriving the window from it would tell a user their fix lands around, say, `v1.0` when it actually ships in the next Beta update. The `release:` label is mandatory-present on every tracked issue (`/issue-creation` enforces exactly one), so there is deliberately **no milestone fallback** — an unlabeled issue resolves to `future` (an honest "unknown"), never a deadline dressed up as a ship date.

> **Stored token ≠ displayed string.** The values above are the *stored* `release` token (a stable comparator). What the public reader actually sees is the token passed through the **Public release-window display** normalization in [`markers-and-templates.md`](markers-and-templates.md) — e.g. the stored `v0.2.0-Beta` renders as **`an upcoming Beta update`**, never the raw token (which reads like an overdue patch, since this project ships rolling Beta updates such as v0.2.1). Store the token; display the normalized window.

For a **shipped** fix (Phase 6 `fixed_in`), prefer a concrete desktop release tag (`desktop-vX.Y.Z`) merged at/after the tracker issue closed; else `a recent release`. Do NOT fall back to the milestone version — it is the complete-by deadline, not the version the fix shipped in. (If your project maps release codenames to versions, that table lives in your own `CLAUDE.md` — but public copy uses the **version**, never the codename.)

## Catalog note

`feedback-triage` is an **orchestrator** of `/issue-creation`, like `/weekly-deps`. It is not itself a `_shared/dispatchable-skills.md` catalog candidate (nothing dispatches *it*), so it needs no catalog entry. If a future change makes `/dev-lifecycle` want to invoke it, add a catalog entry then (Repo-Admin-approved, per that file's rules).
