---
description: Skill-authoring discipline for .claude/skills
paths:
  - ".claude/skills/**"
---

# Skill-Authoring Rules

Path-scoped discipline for `.claude/skills/**`. The model-dispatch rule below should be
enforced by a script wired into your repo's pre-commit config; the rest is review-enforced.

## Never dispatch a non-model-invocable target from the model

An orchestrator skill MUST NOT instruct the model to invoke (`Run` / `Invoke` / `Dispatch` /
`Always-dispatch` / "via the Skill tool") a target the model cannot invoke:

- A **local skill with `disable-model-invocation: true`** (e.g. `review-pr`, the scaffolders,
  `create-migration`, `release-checklist`). The harness rejects model invocation with
  `Skill <name> cannot be used with Skill tool due to disable-model-invocation`, so the
  DISPATCH-GATE step becomes **unhonorable** — the model is forced to skip a mandatory step
  or substitute a manual/single-agent review, both contract violations. **Caught by the lint.**
- A **plugin slash command** (`/<namespace>:<command>`, e.g. `pr-review-toolkit:review-pr`).
  Plugin commands are user-invocation-only; there is no `SlashCommand` tool. **NOT caught by
  the lint** — a namespaced target is indistinguishable by name from a model-invocable plugin
  *skill* (`/superpowers:brainstorming`, `/engineering:architecture`) without enumerating the
  plugin cache, so a lint check would false-positive on every legitimate `/superpowers:*`
  dispatch. Enforced by review instead.

A human typing `/name` can still invoke a disabled skill; the prohibition is only on the MODEL
dispatching it.

## Resolving a needed dispatch — guard strength tracks payload nature

When an orchestrator genuinely needs a disabled skill's work, pick by the skill's payload:

| Payload | Fix | Why |
|---|---|---|
| **Agent-dispatch fan-out** (e.g. `review-pr`) | Extract the agent table to a `_shared/` reference; orchestrators dispatch the agents **directly via the Agent tool**; the skill keeps `disable-model-invocation: true` (user-facing only). | Agent/Task dispatch bypasses the skill boundary, so the hard guard is preserved *structurally* — no model-invocable expensive fan-out exists to fire unprompted. |
| **Bash-runner / other** (e.g. `verify-quality-gate`) | Make the skill model-invocable (remove the flag); add a "Model-invocation authorization" prose block naming the allowed callers. | No agent-dispatch loophole exists; a prose guard is proportionate when unprompted invocation is cheap / read-only. |
| **Caller-restricted invocation** ("only from skills X/Y/Z") | Not possible. | Claude Code has no caller-based invocation gating; `disable-model-invocation` is binary and context-blind. Use one of the rows above. |

Reference implementation: `_shared/pr-review-agent-dispatch.md` (the extracted fan-out) +
`/enhanced-pr-review` Phase 1 Step 5 (direct dispatch) for the agent case;
`verify-quality-gate` for the bash case.

## Catalog entries for disabled skills

The dispatch catalog `_shared/dispatchable-skills.md` lists candidate skills that orchestrators
auto-dispatch when a trigger matches. A `disable-model-invocation` skill MUST NOT be a plain
auto-dispatch candidate — the orchestrator's "dispatch every match" algorithm would
`Skill`-invoke it and the harness would reject. Instead, mark its `### ` entry header
**`user-invoked only`** and write its *Mode interaction* as "surface `/<skill>` as a developer
suggestion; never model-dispatched, in any mode." The catalog's "Dynamic skill scoping"
algorithm honors this marker as the *sole* exception to "dispatch every match." Current
user-invoked-only entries: `/scaffold-component`, `/scaffold-endpoint`, `/create-migration`,
`/release-checklist`. The lint enforces the marker (see below).

## Naming convention: slash = invoke, bare name = name

The lint keys on the **slash form** (`/review-pr`). Use it ONLY for an actual invocation. When
merely *naming* a disabled skill in prose, drop the slash:

- ✅ "the `review-pr` skill", "the fan-out `review-pr` packages", "`Skill(review-pr)`" — naming
- ❌ "Run `/review-pr`", "dispatch `/review-pr`", "fall back to `/review-pr`" — invocation of a
  disabled skill (the lint fails these)

This lets an explanatory mention of a disabled skill coexist on a line with a dispatch verb
without tripping the co-occurrence heuristic — and it reads better (the slash connotes a
command). For a genuine user-facing command suggestion where the slash IS correct (e.g. a
"Run `/verify-quality-gate` to check locally" row shown to the human), add an inline
`<!-- dispatch-lint:ok — <reason> -->` marker on that line to exempt it.

## How the lint works (`scripts/check-skill-dispatch-targets.sh`)

1. Builds the DISABLED set from every `.claude/skills/*/SKILL.md` frontmatter
   (`disable-model-invocation: true`).
2. Fails any line carrying a dispatch verb (`run`/`invoke`/`dispatch` stems, or "via the Skill
   tool") **and** a `/<name>` where `<name>` is in the DISABLED set.
3. Skips fenced code blocks, HTML comments, and lines with a `<!-- dispatch-lint:ok -->` marker.
4. **Catalog check** — in `_shared/dispatchable-skills.md`, fails any `### ` candidate-entry header naming a disabled skill that lacks the `user-invoked` marker (see "Catalog entries for disabled skills" above).
5. Does not attempt plugin-command detection (see the false-positive note above).

Regression test: `scripts/test-check-skill-dispatch-targets.sh`.

## Background

Founding incident: an orchestrator skill instructed the model to dispatch another skill
that had `disable-model-invocation: true` set. The harness rejected the model-invocation
with `Skill <name> cannot be used with Skill tool due to disable-model-invocation`, and the
failure only surfaced mid-run — well after the calling skill had already committed to the
dispatch. This rule exists to catch that class of bug at commit time instead.
