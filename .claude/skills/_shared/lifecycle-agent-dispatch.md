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
capability contract**, not a default to be overridden at dispatch. **Do NOT pass
a `model` override to `Agent`/`agent()` — there is no longer any exception.** The
orchestrator's own model is the dynamic piece; the fleet is fixed.

Cross-vendor mirrors preserve those source pins and translate them through
[`model-capability-tiers.md`](model-capability-tiers.md). Codex then applies only the cataloged
role override: `@red-team` uses `gpt-6-sol`. This is profile generation, not a
dispatch-time model choice.

The former exception ("a producer working an auth/crypto/RBAC path may be
escalated when the task prompt says so") is **retired**. It was the last surviving
path by which an orchestrator could choose a subagent's model. Where a repo needs
an escalation path, it selects a **different agent**, not a different model for
the same agent — see § Escalation (E1-E4) below.

That distinction matters against the tier catalog's invariant 7
(`per-agent model / effort pinning`): choosing an escalation agent over
`@backend-developer` is *agent selection*, which the dispatch tables have always
owned. It is not a model override, and it does not need — or get — a tier's
permission.

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

## Escalation (E1-E4)

Escalation applies only to a repo whose fleet is split — producer agents pinned
below the design agents (e.g. Sonnet producers, Opus designers) — **and** that
defines a repo-local escalation agent: a higher-pinned agent that owns both the
backend and frontend producer surfaces. A repo names it in `.claude/repo-config.json`
as `.agents.escalation` (read it with `rc_get '.agents.escalation' ''`): a bare
agent name, no `@`, resolving to `.claude/agents/<name>.md`. If that file does not
exist, treat the key as unset and say so in the dispatch line. The producers shipped here pin Opus and no
escalation agent ships, so with the key unset these criteria select no agent: when
one fires, record it and surface the task to the developer instead (an E4 bounce is
the common case). Where a repo does define one, it is the escalation target
wherever a producer agent is dispatched — Phase 3 (execute), Phase 4
(validate/remediate), Phase 6 (CI-failure remediation), and Phase 8 (review-fix
application). One agent, one criteria set, four dispatch points.

**Evaluate E1-E4 AFTER the phase table has named a producer, never instead of
it.** The table answers *which surface*; these answer *which tier of that
surface*. A task that resolves to `@test-writer`, `@database-migration`,
`@docs-writer`, `@architect`, or `@researcher` is not eligible — escalation
substitutes only for `@backend-developer` / `@frontend-developer`, or for a
producer named by a repo row (`.agents.producer_rows`) that is not one of those
five.

| # | Criterion | Escalate when |
|---|---|---|
| **E1** | **Novel logic** | The task derives behavior with no in-repo precedent: a new algorithm, state machine, wire protocol, locking or concurrency scheme, or a new invariant. Following an existing pattern is not E1 — however unfamiliar the file is to you |
| **E2** | **Security-sensitive surface** | The task edits auth, crypto / key handling, RBAC or permission logic, an epoch / fence / credential invariant, or IPC sender validation. This is the successor to the retired model-override carve-out, and it is now a *dispatch* rule rather than a prompt-dependent one |
| **E3** | **Cross-surface single contract** | One logical change must land coherently across frontend **and** backend (or ≥3 services), such that splitting it would require two agents to agree on a contract neither owns |
| **E4** | **Producer bounce** | A producer agent has returned twice on this same task without resolving it. Deterministic — no judgement required. Pass what was already tried into the escalated prompt |

### The floor — what never escalates

These are the readings that would let E1-E4 creep until the lower-pinned producers are never used:

- **Volume and tedium never escalate.** A 40-file mechanical rename is wide, not
  hard. Breadth is **D6** — author a Workflow. Reading E3 as "many files" is the
  most likely misuse of this table.
- **Unfamiliarity never escalates.** "I have not seen this subsystem" is a
  `@researcher` dispatch or a Phase-1 `@architect` scout, not an escalation.
- **A `risk: security` label alone never escalates.** E2 is about what the *diff
  touches*, not what the issue is filed under. A label-only match is what
  `@red-team` is for.
- **Knowledge cutoff is not a criterion.** It would be, except the MANDATORY
  Context7 docs discipline already fetches current docs for every third-party API
  before code is written. The gap is closed by process; adding a model-choice rule
  for it would be redundant and would rot the moment a pin changes.

### Tie-break

Under `Automated End-to-End` a **borderline** E1 or E3 call resolves *toward*
escalation — there is no human checkpoint between the task and the merge gate, so
the cheaper failure is the one you pay for in tokens. This adjusts a coin-flip; it
does not lower the bar, and it never fires for E2 or E4 (both are already
determinate).

### Visibility

An escalation is recorded in the Phase's dispatch line with the criterion that
fired and the agent it replaced, so a creeping escalation rate is visible rather
than inferred:

```
Task 3: subagent(@<escalation-agent>) — E2 (RBAC middleware) [escalated from @backend-developer]
Task 7: surfaced to developer — E4 (2nd bounce) [no escalation agent; from @frontend-developer]
```

An escalation agent should return an **escalation warrant** as the first line of
its report — its own judgement of whether the escalation was necessary. Carry that
verdict into the phase log. It is the only feedback signal these thresholds have.

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
| Backend / realtime / infra / schema / config / platform surface in scope | `@system-engineer` | 1 (parallel) |
| Frontend surface in scope — `src/frontend/**`, `src/admin/**` (adjust to your layout), label `domain: ux`, or any user-visible change | `@ui-designer` **and** `@ux-designer` | 1 (parallel, both) |
| Stage-1 frontend agents ran | `@frontend-designer` | 2 — reconciles UI+UX into the handoff spec |
| Always | `@architect` (Mode 3 — reconciliation) | 3 — final spec from all stage-2 outputs |
| Any stage surfaces an unanswered external/internal question | `@researcher` | any (interrupt) |

**Repo design seats.** A repo adds stage-1 seats without editing this file through
`rc_get '.agents.design_rows' '[]'` (hand-dispatch only: the `design-panel` Workflow has fixed seats and does not read this key yet, so a repo that relies on these rows dispatches Phase 2 by hand and says so in the dispatch line): an array of `{"agent": "<name>", "match": [<globs>], "with": "<agent>"}`.
A row fires when any `match` glob is in the plan's scope, or when the agent it names
in `with` is dispatched (so `{"agent": "platform-engineer", "with": "system-engineer"}`
always pairs the two). Fired seats run in parallel at stage 1 and feed the same
reconciliation. `agent` and `with` follow the `.agents.producer_rows` name rule
(`\A[a-z0-9][a-z0-9-]*\z`), and a row that is malformed, has neither `match` nor
`with`, or names an absent agent file is skipped and named in the dispatch line.
List every fired seat in the dispatch line.

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
| 2 | Authoring a migration: any path under `rc_get '.migrations.dir' 'migrations/'` | `@database-migration` |
| 3 | `src/frontend/**`, `src/admin/**` (adjust to your layout) | `@frontend-developer` |
| 4 | `src/backend/**`, `src/media/**` (adjust to your layout), `infrastructure/**`, `.github/workflows/**` | `@backend-developer` |
| 5 | Documentation that must ship with this change | `@docs-writer` |
| 6 | A design question the plan does not answer | `@architect` (Mode 4 — gap resolution) |
| 7 | An unknown that blocks implementation | `@researcher` |

**Repo rows sit between rows 2 and 3.** Read the repo's own rows with
`rc_get '.agents.producer_rows' '[]'`: an array of `{"match": [<globs>], "agent": "<name>"}`.
After rows 1 and 2 (test authoring and migrations keep their owners), check the repo
rows top to bottom and dispatch the first whose globs match the task's paths; only when
none matches do rows 3 onward apply. `rc_get` returns the array as JSON text; parse
it with `jq`. A value that is not an array is ignored and named in the dispatch
line. `agent` must match `\A[a-z0-9][a-z0-9-]*\z` (jq `test`; `^…$` also accepts a
trailing newline) and resolves to `.claude/agents/<name>.md`; `match`
globs use the same `**` dialect as the rows above. A row that is malformed (missing
or non-array `match`, missing `agent`, or an `agent` that fails the pattern) or whose
agent file is absent is skipped and named in the dispatch line, never silently
dropped. This is how a repo routes a surface to its own producer (for example CI and
infrastructure to a dedicated DevOps agent) without editing this shared file.

**A path no row names is a gap, not a free choice.** If the repo has a surface no
producer fits (e.g. a native C++ addon), add a `.agents.producer_rows` entry for it
(or a row here, upstream) above the broader rows it would otherwise fall into — the reader stops at the first match, so a later
widened glob would silently capture it — rather than leaving the task to be routed
by escalation criterion instead of table match.

Rows 1 and 2 sit above the developer rows deliberately: they are the two ownership
rules that a generic path match would otherwise silently override.

**Test-authoring tasks go to `@test-writer`, never to a developer agent in the
same dispatch.** Separating them is what keeps a test task from "fixing"
production code to make a test pass — a recorded failure mode.

**Then check E1-E4.** When row 3, row 4 or a repo row matched, evaluate the escalation
criteria above; if one fires, handle it per § Escalation. Rows 1, 2, 5, 6 and 7
are not eligible for escalation, and neither is a repo row whose agent is one of
the ineligible agents § Escalation lists. A task whose
*shape* is defect repair rather than construction routes to `/reproduce-then-fix`
before any of this — that is a skill dispatch, decided in `/dev-lifecycle` Phase
3, and the agent selection here happens inside it.

---

## Phase 4: VALIDATE

| Condition | Agent |
|---|---|
| Verification fails and the cause is not obvious | `@backend-developer` or `@frontend-developer` (by surface), or the repo-row producer that owns the surface — systematic debugging, then fix |
| The same verification failure survives a second producer return, or the failing surface is an E2 surface | Escalate per § Escalation — E4 (bounce) or E2. Pass both prior attempts into the prompt |
| Coverage below the repo's new-code gate (`.quality_gate.coverage_min`) | `@test-writer` |
| Issue carries `risk: security`, OR the diff touches auth / crypto / E2EE / RBAC / IPC / an outbound-request path | **`@red-team`** — adversarial pass with executable PoCs, *before* the PR exists |
| The repo's count-drift check (if it has one) reports drift, or a doc named in the acceptance criteria is unstaged | `@docs-writer` |

`@red-team` at Phase 4 is the deliberate shift-left: proving an exploit before
the PR is cheaper than proving it during review. It runs in an isolated
worktree (`isolation: worktree`), which redirects its working directory but does
**not** sandbox it — per the agent's own definition (`red-team.md`), an absolute path
in a `Write` or `Bash` still reaches the rest of the filesystem. Check the shared tree
for stray PoC artifacts before committing; a past PR swept one in and had to remove it.
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

**A seat that was requested and returned nothing is not a seat that was not
requested.** `parallel()` resolves a failed thunk to `null`, so the reconciler must
distinguish "not dispatched — no such surface in scope" from "dispatched and
crashed", and treat the second as UNREVIEWED. Reading a crashed seat as an absent
one is the exact failure that seat exists to prevent, and it has been a live defect
in a panel script before.

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

The bracketed panel lists **the seats actually dispatched**, not a fixed roster.
`@ui-designer`/`@ux-designer` are a pair-or-nothing, so a run that did not request
them must not list them — and a run that DID request them must. An audit line that
under-reports a dispatched seat is worse than no line: it states on the record that
a surface went unreviewed when it did not, and the reader has no way to tell which is
true.
