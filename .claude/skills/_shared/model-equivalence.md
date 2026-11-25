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
| Mythos | None | *None* — Claude-only; no Codex profile is generated |
| Fable | Astra | `gpt-6-astra`* |
| Opus | Sol | `gpt-6.1-sol`* |
| Sonnet | Terra | `gpt-6.1-terra`* |
| Haiku | Luna | `gpt-6-luna`* |

`*` - Denotes that this is the formal API pin as of writing, but the actual pin may differ for a later/newer version. Unless a specific version is called out or pinned in a `SKILL.md` or `AGENT.md` file, always find the latest version of `gpt-<VERSION>-<MODEL>`. 

### Codex-only role overrides

After the general pin translation above, Codex applies role-specific assignments:

| Agent | Claude pin (unchanged) | Codex agent model |
|---|---|---|
| `red-team` | Opus | `gpt-6-sol` |

GPT-6 Sol is an explicitly pinned security baseline that replaces the phased-out `gpt-daybreak-blue-latest` assignment. It is not a new capability equivalence, and it is never passed as a dispatch-time override. The translator applies these assignments only while generating `.codex/agents/*.toml`; the `.claude/agents/*.md` source pins stay untouched.

Other Opus-pinned agents, including `security-reviewer`, follow the Opus row of the translation table above.

An agent pinned to a Claude-only model (e.g. Mythos) is never an override row, because it gets no Codex profile at all: the translator skips it, and must fail rather than fall back to a default model for it.

## Effort-level equivalence

Agent files pin `effort` in Claude's vocabulary. Non-Claude surfaces translate as below.

The Codex column supplies the literal `model_reasoning_effort` wire strings. Use the row matching the source agent’s `effort` field.

| Claude | OpenAI / Codex | Notes |
|---|---|---|
| *ultracode* | `ultra` | Ultracode is no longer an `effort` value, it is a session posture. Codex still features it as an effort level that is for above-max reasoning + automated multi-agent parallelization. As such, this should govern orchestration breadth, not one agent's depth. **Never emitted into an agent profile**. The skill `/dev-lifecycle` recommends `Ultracode` to be toggled *on* when `Dynamic` or `Subagent-Driven` mode are selected during initialization. |
| `max` | `max` | Codex now emits a `Max` effort setting just below `Ultra` |
| `xhigh` | `xhigh` | |
| `high` | `high` | |
| `medium` | `medium` | |
| `low` | `low` | UI calls this `Light` effort |

**Governance:** adding or changing a row requires Repo Admin approval — same governance as
dispatch-catalog entries and labels (file an issue titled `Catalog proposal: model-equivalence
<model-id>`). Non-Claude rows stay deliberately coarse: capability-class routing, never
speculative per-vendor prose.

## GitHub Copilot (including Copilot Fusion)

The installer generates one GitHub review profile. It preserves the source Opus class using the verified Copilot model ID documented in the tooling source’s `CAPABILITIES.md`. Two constraints govern Copilot model selection:

- **Fusion has no fixed model.** It picks a model per prompt, and the running conversation
  cannot read which one. Nothing may depend on the orchestrator's model.
- **A pinned custom agent runs outside the router.** Copilot runs a custom agent that pins a
  model on that model, not through Fusion. Generated Copilot agent profiles must carry a pin,
  so each specialist keeps the capability contract its source pin sets.

Do not write Copilot model strings into a profile until they are verified against Copilot's
own model list.
