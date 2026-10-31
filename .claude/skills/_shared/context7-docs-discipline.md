# Context7 Docs Discipline (shared)

Single source of truth for the "fetch current dependency docs before engaging a
third-party dependency" rule. Referenced by [`/issue-creation`](../issue-creation/SKILL.md)
(Pre-Spec) and [`/dev-lifecycle`](../dev-lifecycle/SKILL.md) (PLAN + EXECUTE). Aligns
with the org-wide Context7 convention.

## The rule (MANDATORY)

**When you design or implement work that engages a third-party dependency, you MUST
fetch that dependency's current documentation from Context7 BEFORE writing the design
or the code. Do not rely on training data or a single in-repo call site. Do not
silently fall back to memory.**

This is not box-ticking. Any project that pins recent majors — e.g. if your project
pins something like Electron ^42, mediasoup 3.20, React 19, Zustand 5, or Gin 1.12 —
is pinning APIs that have moved past most training cutoffs. Stale recall on a
version-sensitive API is the exact failure this prevents.

## When it triggers (judgment criteria)

Trigger when the work will **call, configure, or upgrade** a third-party dependency's
API / CLI / config surface. The trigger is the **API surface you touch**, not whether
the issue text names the library:

- Affected `area: media-plane` → mediasoup, Socket.IO, NATS, ioredis — even if unnamed.
- Affected `area: desktop` → Electron, React, Zustand, Vite, electron-updater, WebCrypto.
- Affected `area: control-plane` → Gin, the Go stdlib `crypto/*`, pgx, go-redis.
- Any dependency **bump** (`area: deps`) → fetch the new version's migration/changelog.

**Skip only when** ALL hold: pure internal/business logic with no third-party API
surface; OR trivial language-stdlib use; OR you are literally renaming/moving existing
calls with zero behavior change. When unsure, fetch — it is cheap.

## How (Context7 MCP — repo-portable)

Context7 is a project-configured MCP server (`.mcp.json`), so it works for any agent on
this repo. The tools are usually **deferred** — load their schemas via `ToolSearch`
first, or the direct call fails with `InputValidationError`:

```
ToolSearch  query: "select:mcp__context7__resolve-library-id,mcp__context7__query-docs"
mcp__context7__resolve-library-id  <library-name>      # e.g. "mediasoup", "Electron"
mcp__context7__query-docs          <library-id> "<the specific feature/API question>"
```

- Resolve to the **pinned version** when the version list offers it (e.g.
  `/pmndrs/zustand/v5.0.12`) — match the project's `package.json` / `go.mod` pin.
- Use a **specific** query ("Consumer.setPreferredLayers spatial/temporal layers",
  not "mediasoup"). Specific queries return usable snippets.
- Never put secrets (keys, tokens) in a query.
- On a Context7 quota error, say so and proceed using the in-repo pattern + the
  dependency's pinned local types as the fallback basis — but state explicitly that
  docs were unavailable. (Where the operator has one configured, a machine-local
  `ctx7` CLI is an equivalent path if the MCP is down.)

Record what you fetched: add the resolved library-id + doc URL to the issue's
`## References` (issue-creation) or note it in the plan / task notes (dev-lifecycle).

## Rationalizations — all of these mean "fetch the docs anyway"

| Excuse (seen in baseline testing) | Reality |
|---|---|
| "The existing call site in the repo shows the API." | One in-repo call site can be version-locked to an older pattern. It shows ONE usage, not the current contract, bounds, or footguns. Confirm against current docs. |
| "It's a quick / one-liner / 10-minute job." | Size ≠ safety. The one-liner APIs (`flashFrame` per-platform behavior, `setPreferredLayers` signature) are exactly where stale memory bites. |
| "I'll fetch the docs later, before I commit." | "Later" is the deferral that never fires under pressure. Fetch BEFORE you design / write the dependency code. |
| "I already know this API." | The org rule is explicit: use Context7 *even when you think you know* — your training data may predate the pinned version. |
| "The library isn't named in the issue." | The trigger is the file surface / API you touch, not the issue's wording. |
| "Context7 tools aren't loaded." | Load them with one bulk `ToolSearch` (above). That is one call, not a reason to skip. |

## Red flags — STOP and fetch

- About to write code against a dependency API having read only an in-repo call site.
- Telling yourself the change is too small / too familiar to need docs.
- Planning to "verify the API later."
- Designing an approach around an API signature you recalled from memory.
