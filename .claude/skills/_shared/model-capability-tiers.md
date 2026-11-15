# Model-Capability Tiers Catalog

This file is the single source of truth for **model-capability tier profiles** consumed by
`/dev-lifecycle` (and any future orchestrator that calibrates execution behavior to the
running model). Canonical source: the dev-tooling Master (`skills/_shared/`); consumer repos
receive real-file copies via `scripts/sync-shared.sh` (same mechanism as
`dispatchable-skills.md`). Edit HERE, never in a consumer copy.

> **Design principle:** Mode = risk authority (what the human delegated).
> Tier = capability calibration (how well the model executes within that delegation).
> Tier adjusts recommendations and execution mechanics; it can NEVER move a gate.

> **Scope narrowed 2026-07-24 (agent pinning).** Tier used to carry two subagent axes —
> *dispatch granularity* and *subagent model selection*. Both are retired: every agent now
> pins its own `model` and `effort` in its agent file, and both dispatch tables forbid passing
> a `model` override at dispatch. **Tier now calibrates the ORCHESTRATOR only.** It answers
> "how big a bite does the main loop take between checkpoints, and how hard does it try before
> surfacing" — not "which model does the work," because the fleet already answers that.
> Consequently tier is a much smaller lever than it was: an unmapped model is a non-event,
> not a problem to chase.

## Detection procedure (fail-closed)

1. **Harness evidence only.** Tier resolution requires an explicit model ID string injected
   into the runner's system context by the harness (e.g., Claude Code's environment block
   names the exact model ID, such as `claude-fable-5[1m]`). Strip harness decorations
   (bracketed effort suffixes like `[1m]`) before matching.
2. **Model self-belief is never sufficient.** Models misreport their own identity. A runner
   that "believes" it is a given model but has no harness evidence resolves `generic`.
3. **No evidence → `generic` (fail-closed).** This is what makes mirrors consumed by other
   tools (e.g., `.github/skills/` for Copilot) safe: a runner that sees no model line
   self-classifies `generic` and gets the conservative profile.
4. **Present-but-unmapped ID → `generic` + WARN.** Emit one line so catalog staleness is
   visible rather than silent (mirrors the catalog-dispatch resilience rule):
   `WARN: unmapped model ID <id>; resolving generic — catalog may be stale`
5. **Re-detect on every invocation; never persist.** Tier is held in session memory only —
   never written to `.claude/state/dev-lifecycle-mode/` or anywhere else. A planted `tier=`
   line in any state file is ignored because no code path reads one (extends the
   planted-file defense from `/dev-lifecycle` Phase 1.5's re-confirmation rationale).

## Model-ID → profile mapping table

Prefix match against the decoration-stripped model ID, except the four GPT-6 rows and the
`gemini-3.8-flash` row: those require equality with the entire stripped ID. First matching
row wins.

| Model-ID evidence (match) | Profile | Notes |
|---|---|---|
| `claude-mythos-5` | `fable-5` | Prefix match (covers Mythos 5.x, e.g. `claude-mythos-5-1`); frontier model, nearest profile |
| `claude-fable-5` | `fable-5` | |
| `claude-opus-5` | `opus-5` | |
| `claude-sonnet-5` | `sonnet-5` | |
| `claude-opus-4-8` | `opus-5` | nearest profile |
| `claude-opus-4-7` | `opus-5` | nearest profile |
| `claude-opus-4-6` | `opus-5` | nearest profile |
| `claude-haiku-` | `generic` | explicit; Haiku-class must not orchestrate a lifecycle run |
| `gpt-6-astra` | `fable-5` | Exact match; GPT-6 frontier role fit, provisional, not a benchmark equivalence |
| `gpt-6.1-sol` | `sonnet-5` | Exact match; GPT-6.1 daily coding workhorse role fit |
| `gpt-6-sol` | `opus-5` | Exact match; GPT-6 coding workhorse and cyber baseline role fit, provisional |
| `gpt-6-luna` | `sonnet-5` | Exact match; GPT-6 fast, affordable role fit, provisional |
| `gpt-5-6-sol` / `gpt-5.6-sol` | `fable-5` | cross-vendor equivalence — see table below |
| `gpt-5-6-terra` / `gpt-5.6-terra` | `opus-5` | |
| `gpt-5-6-luna` / `gpt-5.6-luna` | `sonnet-5` | |
| `gpt-5-5` / `gpt-5.5` | `generic` | Haiku-class equivalent; must not orchestrate |
| `gemini-3.8-flash` | `sonnet-5` | Exact match; flash-class worker model, so it must not inherit the pro-variant row below |
| `gemini-3` | `opus-5` | nearest profile; provisional, pro variants |
| *anything else / no evidence* | `generic` | + WARN when an ID was present but unmapped |

For example, `gpt-6-sol` resolves `opus-5`, while `gpt-6-solar` and
`gpt-6-astra-preview-unsafe` resolve `generic` + WARN. Do not infer a tier from
the shared start of an unknown GPT-6 ID.

### Cross-vendor equivalence (Claude ↔ OpenAI / Codex)

Consumer repos are read by more than one runner (Claude Code, Codex, Copilot). This table
serves **three distinct questions**, and they do not always give the same answer — read the
column you actually need:

- **Capability equivalence** — an operational role fit inferred from the model catalog,
  not a measured cross-vendor benchmark. Prose only; never emit this value into a config file.
- **Codex agent model** — the literal value a translator writes as `model` in a
  `.codex/agents/*.toml` profile. This column is authoritative for translation.
- **Tier profile** — the internal profile used to resolve orchestration capability.

| Claude pin | Capability equivalence | Codex agent model (authoritative for translation) | Tier profile |
|---|---|---|---|
| Mythos 5.1 | None in the GPT-6 catalog | *None* — Claude-only; no Codex profile is generated | `fable-5` |
| Fable 5 | GPT-6 **Astra** | `gpt-6-astra` | `fable-5` |
| Opus 5 | GPT-6 **Astra** | `gpt-6-astra` | `opus-5` |
| Sonnet 5 | GPT-6.1 **Sol** | `gpt-6.1-sol` | `sonnet-5` |
| Haiku 4.5 | GPT-5.5 | `gpt-6-luna` — **upgrade** | `generic` |

The GPT-6 rows in the Model-ID table classify a **running orchestrator** when the harness
supplies its exact model ID. A bare `GPT-6` family label remains `generic`. The GPT-5.6
rows remain for older orchestrator sessions; generated agent profiles now use the GPT-6
translation above. The source Claude pins and Codex-only role overrides remain in force.

**Haiku is the row where the columns diverge, deliberately.** GPT-5.5 is the honest capability
equivalence, but it is not a member of the GPT-6 agent-profile family, so there is no Codex
agent model at Haiku's class to translate *into* — a `haiku`-pinned agent upgrades to
`gpt-6-luna` rather than falling back to whatever a translator's default happens to be.

**The upgrade cannot leak into tier resolution, because an agent's pinned model is not a tier
input at all.** Per the 2026-07-24 scope narrowing above, tier calibrates the **orchestrator
only** — the model the harness reports for the running main loop. A dispatched subagent's
pinned model, translated or otherwise, never feeds the detection procedure, so translating
`haiku` → `gpt-6-luna` cannot promote anything out of `generic`. The Tier-profile column
above describes what a **session running as that model** resolves to, not what a subagent
pinned to it becomes.

Do **not** add provenance plumbing to carry "this was originally a haiku pin" into tier
resolution. The detection procedure is deliberately harness-evidence-only, and the axis that
would have needed such plumbing (subagent model selection) was retired in the same 2026-07-24
narrowing. An earlier revision of this section instructed readers to "never derive the tier
from the translated model," which was unimplementable as written — the detector has only the
runtime model ID and no way to comply. The instruction was removed rather than satisfied.

#### Codex-only role overrides

After the general pin translation above, Codex applies role-specific assignments:

| Agent | Claude pin (unchanged) | Codex agent model |
|---|---|---|
| `red-team` | Opus | `gpt-6-sol` |

GPT-6 Sol is an explicitly pinned security baseline that replaces the phased-out `gpt-daybreak-blue-latest` assignment. It is not a new capability equivalence or orchestrator tier, so it does not change the Model-ID → profile rows above and is never passed as a dispatch-time override. The translator applies these assignments only while generating `.codex/agents/*.toml`; the `.claude/agents/*.md` source pins stay untouched.

The cyber baseline covers only the adversarial role. Other Opus-pinned agents, including `security-reviewer` and any repo-local domain reviewers, are not in it: no override has been decided for them, so they follow the default Opus mapping (`gpt-6-astra`).

An agent pinned to a Claude-only model (e.g. Mythos) is never an override row, because it gets no Codex profile at all: the translator skips it, and must fail rather than fall back to a default model for it.

### Effort-level equivalence

Agent files pin `effort` in Claude's vocabulary. Non-Claude surfaces translate as below.

This is a **uniform one-rung down-shift, not a ceiling clamp**: a Codex rung reasons harder
than the Claude rung of the same name, so mapping the names onto each other would run every
mirrored agent hotter than its pin intends. Every row shifts, not just rows that would exceed
a ceiling. Values in the Codex column are the literal `model_reasoning_effort` wire strings.

| Claude | OpenAI / Codex | Notes |
|---|---|---|
| *ultracode* | `ultra` | Not an `effort` value — a session posture (above-max reasoning + automated multi-agent parallelization). Governs orchestration breadth, not one agent's depth. **Never emitted into an agent profile**, and the model catalog does not offer it for `gpt-6-luna` at all |
| `max` | `xhigh` | Codex `max` is deliberately skipped; ultracode owns the rung above `xhigh` |
| `xhigh` | `high` | |
| `high` | `medium` | |
| `medium` | `low` | |
| `low` | `low` | Collapses with `medium`. `low` is the Codex floor — there is **no** rung beneath it |

**There is no Codex "Light".** An earlier revision of this table mapped `low` → Light; that
value appears in no model's `supported_reasoning_levels` and is not accepted in
`config.toml`. The floor is `low`. The verified Codex ladder is
`low < medium < high < xhigh < max < ultra`. In the GPT-6 family, `ultra` is available on
`gpt-6-astra` and `gpt-6-sol`, but not `gpt-6-luna`.

**Governance:** adding or changing a row requires Repo Admin approval — same governance as
dispatch-catalog entries and labels (file an issue titled `Catalog proposal: tier-mapping
<model-id>`). Non-Claude rows stay deliberately coarse: capability-class routing, never
speculative per-vendor prose.

## Behavioral profiles — orchestrator calibration only

Parameter values are a starting calibration, tunable through normal PR review of this file.
Axes are interface (changing them needs a design-doc addendum); values are calibration.

| Axis | `fable-5` | `opus-5` | `sonnet-5` | `generic` |
|---|---|---|---|---|
| 1. Default approach (nontrivial scope) | Dynamic | Dynamic | Dynamic | legacy heuristics (Inline / Subagent Driven / plan-time static split) |
| 1a. Default approach (trivial single-task) | Inline | Inline | Inline | Inline |
| 2. Dynamic batch sizing (plan tasks executed inline between verification points) | 3–4 | 2–3 | 1–2 | n/a — Dynamic degrades to plan-time static split |
| 3. Confidence posture within already-authorized auto phases | decide-and-log | decide-and-log for routine; surface novel | surface-and-ask on novel/ambiguous | surface-and-ask |
| 4. Retry / fix-loop budget (pre-commit local remediation cycles per failure before surfacing)¹ | 3 | 2 | 2 | 1 |
| 5. Mode-suggestion notch (deterministic; applied per `/dev-lifecycle` Phase 1.5 carve-outs/clamp) | +1 | 0 | 0 | −1 |

¹ Distinct scope from the review-loop ≤2-fix-commits-per-cycle guideline, which governs
*pushed commits* per bot-review cycle and is unchanged and tier-invariant. The retry budget
governs *pre-commit local* remediation attempts.

### Retired axes (do not reintroduce)

- **Subagent dispatch granularity** — superseded by named agents. *Which* agent takes a task is
  answered by `lifecycle-agent-dispatch.md`, not by the orchestrator's tier.
- **Subagent model selection** — superseded by per-agent `model` pinning. The pin IS the cost
  and capability contract; a tier that could override it would reintroduce exactly the
  unpredictability the pinning exists to remove. **No orchestrator, at any tier, may pass a
  `model` override at dispatch.**

The one surviving rule from the old axis 6: **a `generic`-tier orchestrator must not run a
lifecycle beyond trivial scope** — Haiku-class runners have hallucinated completion reports.
That is a floor on the orchestrator, not a statement about subagents.

## Invariants tiers may not alter

No tier, in any mode, may alter:

1. Any cell of the `/dev-lifecycle` mode-to-gate matrix (autonomy framework Component D).
2. The Phase 9 universal merge floor (always gated).
3. Phase 7.5 STOP-on-anomaly for **required** reviewers (mode- and tier-independent).
4. Billed-skill preapproval (`billed` / `user-triggered` semantics in `dispatchable-skills.md`).
5. The DISPATCH-GATE (sub-skills invoked via the Skill tool, never substituted).
6. Catalog mandatory-dispatch (a matched candidate is never silently skipped).
7. **Per-agent `model` / `effort` pinning** (added 2026-07-24).

A tier profile that appears to authorize crossing any of these is a catalog bug — the
invariant wins, and the discrepancy should be fixed in this file via normal PR review.

**Invariant 7 vs. escalation (clarified 2026-08-26).** `lifecycle-agent-dispatch.md`
defines criteria (E1-E4) under which a repo-local escalation agent — a higher-pinned
agent that owns both producer surfaces — takes a task instead of a lower-pinned
producer. That is **agent selection**, which the dispatch tables have always owned,
and it does not touch invariant 7: the escalation agent runs on its own pin, and the producer it
replaced still runs on its own pin whenever it *is* dispatched. No `model` override
is passed at any point.

Escalation is also **not a tier input, in either direction**. Tier calibrates the
orchestrator only (see the 2026-07-24 narrowing above); it neither gates nor relaxes
E1-E4, and a `generic`-tier orchestrator evaluates them identically to a `fable-5`
one. The one tier-adjacent interaction lives in the dispatch table, not here: under
`Automated End-to-End` a *borderline* E1/E3 call resolves toward escalation — and
that is keyed on the **mode**, which is risk authority, not on the tier.
