# Lifecycle Agent Dispatch — Phases 1–4

> **Single source of truth** for agent dispatch in the *pre-review* half of the
> lifecycle. Consumed by `/dev-lifecycle` Phases 1, 2, 3, and 4.
>
> Its Phase-8 counterpart is [`pr-review-agent-dispatch.md`](pr-review-agent-dispatch.md).
> Together they cover every agent-dispatching point in the lifecycle.
>
> **How dispatch works:** every entry is a subagent, dispatched via the **Agent
> tool** by the orchestrator (the `/dev-lifecycle` main loop). Subagents cannot
> dispatch subagents — an agent-to-agent edge in a design doc is realized either
> as a sequential orchestrator-mediated relay or as a **Workflow script**
> (`agent(prompt, {agentType})`), never as nested dispatch.

## Model pinning is deliberate

Every agent file pins `model:` and `effort:`. That pinning is a **cost and
capability contract**, not a default to be overridden at dispatch. Do NOT pass a
`model` override to `Agent`/`agent()` unless the task prompt explicitly says to
escalate (the documented case: a producer working an auth/crypto/RBAC path).
The orchestrator's own model is the dynamic piece; the fleet is fixed.

### `maxTurns` is the third pin — and it fails silently

Every agent file also pins `maxTurns`. Unlike `model`/`effort` it is a **hard
external kill**: the harness stops the agent the instant the cap is reached,
mid-flight, with no warning to the agent — the value appears nowhere in its
system prompt, so it cannot pace itself against it.

A subagent's **final assistant text block IS its return value.** So when the cap
lands right after a tool call, there is no final text and the orchestrator
receives *nothing* — or, worse, whatever stray opening line the agent happened to
emit on turn 1, which reads like a report opening rather than an error. The run
still reports success. Treat a subagent reply that is pure narration
("I'll start by examining the diff…") as a **truncation, not a clean result** —
re-dispatch or resume it, because the turn budget is per-invocation and a resume
gets a fresh one.

One turn is one assistant message however many tool calls it batches, so raise
`maxTurns` against *turns*, not tool calls — an agent batching two calls per turn
burns a 25-turn budget in 40 tool calls. When adding an agent, size this pin from
the workload deliberately, and give it headroom for the report-writing turn
itself. Each agent carries a `## Turn Budget` section stating the contract.

**Pre-extract large inputs rather than making the agent fetch them.** The single
most effective way to keep a review inside its budget is to hand it the material
instead of a command to obtain it. Measured on the same 5-file diff: a
`code-reviewer` told to run `gh pr diff` itself burned all 20 turns across 31
tool calls and returned nothing; re-dispatched against a pre-extracted diff file
with an explicit "read this one file, then stop and write" instruction, it
finished in 5 tool calls. Effort tier compounds this — an `xhigh` agent explores
harder per turn, so it exhausts a budget faster than a `high` agent on identical
work. Size the pin against the agent's effort, not just its role.

Founding incident: five runs across four agents (`architect` ×2,
`docs-reviewer`, `security-reviewer`, `code-reviewer`) terminated on exactly
their pinned cap and returned only narration; three were Phase-8 reviewers on
live PRs whose findings were silently discarded. A follow-up re-review confirmed
the sizing empirically: `security-reviewer` used 11/25 and `docs-reviewer` 10/25,
while `code-reviewer` — the only `xhigh` agent pinned below 30 — exhausted 20/20
on the same diff and was raised to 30.

---

## Phase 1: UNDERSTAND

| Condition | Agent | Returns |
|---|---|---|
| Always | `@architect` (Mode 1 — blast-radius scout) | Surfaces touched · governing rules files · **which Phase-8 reviewers will fire** · count-bearing surfaces |
| A knowledge gap blocks scoping — unfamiliar subsystem, unclear upstream contract, "why is it like this" | `@researcher` | Grounded answer with `file:line` / SHA / Context7 evidence + confidence |

`@architect`'s reviewer prediction is the load-bearing output: it front-loads
Phase-8 findings into Phase-2 planning. Feed it into the spec, not just the log.

---

## Phase 2: PLAN

Phase 2 is the **design panel**. Dispatch is fan-out-then-synthesize, and its
canonical implementation is the `design-panel` Workflow (see § Workflow
realization). Hand-dispatch only when a Workflow is unavailable.

| Condition (any match) | Agent | Stage |
|---|---|---|
| Always | `@architect` (Mode 2 — design philosophy) | 0 — produces the brief the panel builds against |
| Backend / media-plane / infra / schema / config surface in scope | `@system-engineer` | 1 (parallel) |
| Frontend surface in scope — `src/frontend/**`, `src/admin/**` (adjust to your layout), label `domain: ux`, or any user-visible change | `@ui-designer` **and** `@ux-designer` | 1 (parallel, both) |
| Stage-1 frontend agents ran | `@frontend-designer` | 2 — reconciles UI+UX into the handoff spec |
| Always | `@architect` (Mode 3 — reconciliation) | 3 — final spec from all stage-2 outputs |
| Any stage surfaces an unanswered external/internal question | `@researcher` | any (interrupt) |

**`@ui-designer` and `@ux-designer` are dispatched as a pair or not at all.**
The reconciliation step exists to resolve their tension; running one alone
produces an unbalanced spec with nothing to reconcile.

---

## Phase 3: EXECUTE

Dispatch is per plan-task, decided by the `/dev-lifecycle` D1–D6 criteria. This
table answers *which agent* once D1–D5 have answered *whether to dispatch*.
Above the D6 breadth threshold (>~15 units or >~10 files), author a Workflow that
fans these agents out instead of hand-dispatching a handful.

**Rows are evaluated MOST-SPECIFIC FIRST, and the first match wins.** The paths
overlap by construction — a migration lives under `src/backend/**` (adjust to your
layout), and a Go test lives beside the code it tests — so without an explicit precedence the table
would contradict its own ownership rules below. Read it top to bottom and stop at the
first match.

| # | Task touches | Agent |
|---|---|---|
| 1 | Test authoring or coverage remediation, in ANY surface (`**/*_test.go`, `**/*.test.ts(x)`, `**/tests/**`) | `@test-writer` |
| 2 | `src/backend/migrations/**` (adjust to your layout; authoring a migration) | `@database-migration` |
| 3 | `src/frontend/**`, `src/admin/**` (adjust to your layout) | `@frontend-developer` |
| 4 | `src/backend/**`, `src/media/**` (adjust to your layout), `infrastructure/**`, `.github/workflows/**` | `@backend-developer` |
| 5 | Documentation that must ship with this change | `@docs-writer` |
| 6 | A design question the plan does not answer | `@architect` (Mode 4 — gap resolution) |
| 7 | An unknown that blocks implementation | `@researcher` |

Rows 1 and 2 sit above the developer rows deliberately: they are the two ownership
rules that a generic path match would otherwise silently override.

**Test-authoring tasks go to `@test-writer`, never to a developer agent in the
same dispatch.** Separating them is what keeps a test task from "fixing"
production code to make a test pass — a recorded failure mode.

---

## Phase 4: VALIDATE

| Condition | Agent |
|---|---|
| Verification fails and the cause is not obvious | `@backend-developer` or `@frontend-developer` (by surface) — systematic debugging, then fix |
| Coverage below the ≥80% new-code gate | `@test-writer` |
| Issue carries `risk: security`, OR the diff touches auth / crypto / E2EE / RBAC / IPC / an outbound-request path | **`@red-team`** — adversarial pass with executable PoCs, *before* the PR exists |
| `./scripts/update-claude-md-counts.sh` dry-run is non-empty, or a doc named in the acceptance criteria is unstaged | `@docs-writer` |

`@red-team` at Phase 4 is the deliberate shift-left: proving an exploit before
the PR is cheaper than proving it during review. It runs in an isolated
worktree (`isolation: worktree`), so its PoC tests never touch the shared tree.
It does **not** replace its Phase-8 run — see the PR dispatch table.

---

## Workflow realization

Subagents cannot dispatch subagents. The Phase-2 hierarchy is therefore encoded
as a Workflow script, which the orchestrator invokes:

```js
phase('Design')
const stage1 = await parallel([
  () => agent(brief, { agentType: 'system-engineer', phase: 'Design' }),
  () => agent(brief, { agentType: 'ui-designer',     phase: 'Design' }),
  () => agent(brief, { agentType: 'ux-designer',     phase: 'Design' }),
])
phase('Reconcile')
const fe = await agent(reconcilePrompt(stage1), { agentType: 'frontend-designer' })
return agent(synthesisPrompt(stage1, fe), { agentType: 'architect' })
```

The repo-local implementation lives at `.claude/workflows/design-panel.js` and
is invoked by name. Each `agent()` call resolves that agent file's pinned model,
so one panel run has a deterministic, budgetable cost.

---

## Dispatch resilience

If an entry references an agent not present in the current inventory, log one
line — `WARN: dispatch table references unknown agent @<name>; skipping` — and
continue. Do not abort the phase. This mirrors the catalog-dispatch resilience
rule in [`dispatchable-skills.md`](dispatchable-skills.md).

## Visibility

Every dispatch emits an auditable line, matching the `Scoped skills (Phase N)`
convention:

```
Dispatched agents (Phase 2): @architect(philosophy) → [@system-engineer, @ui-designer, @ux-designer] → @frontend-designer → @architect(reconcile)
Dispatched agents (Phase 3): Task 4 → @backend-developer; Task 5 → @test-writer
```
