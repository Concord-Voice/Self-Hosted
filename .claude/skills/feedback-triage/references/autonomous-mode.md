# Autonomous mode & scheduling

Read this before any mutating phase. It defines what each mode is allowed to do, the daily cloud-routine contract, and the safety envelope.

## Mode gate matrix

| Phase | auto (default) | dry-run | gated |
|---|---|---|---|
| 0 Resolve + envelope | run | run | run |
| 1 Catalog | run | run | run |
| 2 Cluster | run | run | run |
| 3 Consolidate (close dupes) | **apply** (within caps) | plan only | **digest → wait** |
| 4 Tracker `/issue-creation` | **apply** (Status=Triage) | plan only | **digest → wait** |
| 5 Public tracking note | apply | plan only | apply (additive) |
| 6 Reverse sync (close public) | **apply** (within caps) | plan only | **digest → wait** |
| 6.5 Known Issues regen | apply (additive) | plan only (print body) | apply (additive) |
| 7 Digest | post to tracker | present | present |

"digest → wait" (gated) means: collect the proposed destructive actions into the Phase 7 digest and stop; apply them on the next invocation only after explicit approval.

## Safety envelope (auto mode)

Bounds the blast radius of a bad clustering or a tracker hiccup. Defaults (override only with explicit approval):

- `MAX_CLOSURES_PER_RUN=15` — combined consolidation + fixed closures. Overflow → `deferred: closure cap` in the digest, applied across subsequent runs.
- `MAX_CLUSTER_SIZE=8` — a proposed dupe cluster larger than this is **never** auto-closed; it's a digest review item (a huge cluster usually means the clusterer over-merged).
- `MIN_CLUSTER_CONFIDENCE=high` — only same-root-cause / same-feature merges auto-apply.
- **Human-touched issues are untouchable** by auto-consolidation/close (any maintainer comment or `wontfix`/`duplicate`/`invalid`/`question` label).
- **No silent caps.** Every deferral is a line in the digest. A reader can always reconstruct what was withheld and why.

These caps bound autonomy; they do not gate it. A normal day's volume sits well under them.

## The daily cloud routine

The scheduled run is a Claude cloud routine (cron) that invokes this skill in auto mode. **Stand it up only after the skill is proven on real issues**, and **show the operator the exact routine config for sign-off before activation** — a recurring autonomous agent that mutates a public repo is outward-facing and gets explicit human authorization once, at creation.

Routine contract:

- **Cadence:** daily. Pick a quiet UTC hour (e.g. `0 8 * * *`). One run per day; runs are idempotent so a missed/retried run self-heals.
- **Command:** `/feedback-triage --mode=auto`.
- **Identity/secrets:** runs under an identity with `issues:write` on **both** repos and `project` scope for the tracker board. If that identity is the same `FEEDBACK_PAT` family used by the control-plane, keep its scope minimal (issues + project only).
- **Audit:** every run posts its Phase 7 digest to a pinned tracking issue in the **tracker** repo, so there's a durable daily log even when the run was a no-op.
- **Kill switch:** disabling the routine stops all mutation immediately; in-flight state is consistent because each issue's actions are individually guarded (see `idempotency.md`).

A starter `--mode=dry-run` window (first ~7 days) is strongly recommended before flipping the routine to `auto`: same logic, zero mutations, real digests to review.

## Why auto mode is safe enough to run unattended on a public repo

1. Every mutation is **reversible** (close, not delete) and **guarded** (fires only on a real transition).
2. The **caps** bound a worst-case bad run to ≤15 reversible closures, all logged.
3. **Human decisions are sacrosanct** — the routine never overrides a maintainer's label or comment.
4. The **digest** makes every run auditable after the fact; the kill switch makes it stoppable at any time.
5. Re-running is a **no-op** on an unchanged inbox, so a stuck/duplicated trigger can't compound damage.

**Honest caveat — enforcement is by convention, not by hook.** The envelope (caps, high-confidence-only, human-touched exclusion) is *agent-honored* discipline, not a wrapper script or pre-commit hook that structurally blocks an over-run. There is no executable guard in this skill — it is documentation an LLM run is asked to follow. That is acceptable here *only* because the three durable backstops are structural: every action is **reversible** (close, never delete), every run emits an **audit digest**, and the operator holds a **kill switch**. If a future change makes any action non-reversible, the envelope must become hook-enforced before that change ships.
