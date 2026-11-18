# Model Equivalence Catalog

Translation tables for generating a non-Claude runner's agent profiles from this repo's
Claude-native agent pins. Canonical source: Agentic-Dev-Tooling
`plugins/core-lifecycle/skills/_shared/`. Edit it there, never in a consumer copy.

**This file never classifies the running orchestrator.** `/dev-lifecycle` has no model gate
(see its § No model gate): it never reads a model ID, and its defaults are the same on every
harness. These tables apply only when a translator writes agent profiles. Every agent keeps
its source `model` and `effort` pins, and no orchestrator passes a `model` override at
dispatch.

## Cross-vendor equivalence (Claude ↔ OpenAI / Codex)

Consumer repos are read by more than one runner (Claude Code, Codex, Copilot). This table
serves **two distinct questions**, and they do not always give the same answer — read the
column you actually need:

- **Capability equivalence** — an operational role fit inferred from the model catalog,
  not a measured cross-vendor benchmark. Prose only; never emit this value into a config file.
- **Codex agent model** — the literal value a translator writes as `model` in a
  `.codex/agents/*.toml` profile. This column is authoritative for translation.

| Claude pin | Capability equivalence | Codex agent model (authoritative for translation) |
|---|---|---|
| Mythos 5.1 | None in the GPT-6 catalog | *None* — Claude-only; no Codex profile is generated |
| Fable 5 | GPT-6 **Astra** | `gpt-6-astra` |
| Opus 5 | GPT-6 **Astra** | `gpt-6-astra` |
| Sonnet 5 | GPT-6.1 **Sol** | `gpt-6.1-sol` |
| Haiku 4.5 | GPT-5.5 | `gpt-6-luna` — **upgrade** |

**Haiku is the row where the columns diverge, deliberately.** GPT-5.5 is the honest capability
equivalence, but it is not a member of the GPT-6 agent-profile family, so there is no Codex
agent model at Haiku's class to translate *into* — a `haiku`-pinned agent upgrades to
`gpt-6-luna` rather than falling back to whatever a translator's default happens to be.

### Codex-only role overrides

After the general pin translation above, Codex applies role-specific assignments:

| Agent | Claude pin (unchanged) | Codex agent model |
|---|---|---|
| `red-team` | Opus | `gpt-6-sol` |

GPT-6 Sol is an explicitly pinned security baseline that replaces the phased-out `gpt-daybreak-blue-latest` assignment. It is not a new capability equivalence, and it is never passed as a dispatch-time override. The translator applies these assignments only while generating `.codex/agents/*.toml`; the `.claude/agents/*.md` source pins stay untouched.

The cyber baseline covers only the adversarial role. Other Opus-pinned agents, including `security-reviewer` and any repo-local domain reviewers, are not in it: no override has been decided for them, so they follow the default Opus mapping (`gpt-6-astra`).

An agent pinned to a Claude-only model (e.g. Mythos) is never an override row, because it gets no Codex profile at all: the translator skips it, and must fail rather than fall back to a default model for it.

## Effort-level equivalence

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
dispatch-catalog entries and labels (file an issue titled `Catalog proposal: model-equivalence
<model-id>`). Non-Claude rows stay deliberately coarse: capability-class routing, never
speculative per-vendor prose.

## GitHub Copilot (including Copilot Fusion)

There is no Copilot column yet: Agentic-Dev-Tooling does not generate Copilot custom agents (see
its `CAPABILITIES.md`; consumer repositories receive this file without it). Two facts constrain
the column when it is added:

- **Fusion has no fixed model.** It picks a model per prompt, and the running conversation
  cannot read which one. Nothing may depend on the orchestrator's model.
- **A pinned custom agent runs outside the router.** Copilot runs a custom agent that pins a
  model on that model, not through Fusion. Generated Copilot agent profiles must carry a pin,
  so each specialist keeps the capability contract its source pin sets.

Do not write Copilot model strings into a profile until they are verified against Copilot's
own model list.
