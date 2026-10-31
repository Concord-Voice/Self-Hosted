---
description: Security and implementation invariants for the Admin Portal
paths:
  - "client/admin/**"
---

> **Imported from Concord-Voice-Alpha (@771eb3c) at the repo split.** References to `client/desktop/**`, `docs/runbooks/deploys.md`, `docs/reference/as-built-ledger.md`, `docs/superpowers/**`, managed (hosted) deploy workflows and Alpha's org rulesets point at Alpha, Desktop or the private ops repos. Use them for context; they are not files or CI in this repo.

# Admin Portal Rules

## Closed data boundary

- Render only fixed, frontend-owned labels, allowlisted identifiers, and finite
  scalar values returned by the closed admin API contract.
- Validate every response before React receives it. Reject unknown identifiers,
  duplicate keys, non-finite scalars, malformed timestamps, oversized arrays,
  and unexpected rendering-bearing fields with a generic error.
- Do not add arbitrary metric keys, query parameters, time ranges, node IDs,
  labels, dimensions, or user-provided text to the rendering path.

## Credentials and persistence

- Never log or persist passwords, enrollment tokens, WebAuthn options or
  responses, session material, node IDs, metric samples, counter baselines, or
  health events.
- Keep credentials and metrics in memory. Only the six approved threshold pairs
  and a fixed font preference may use validated localStorage entries.
- Use the existing same-origin auth, enrollment, logout, and fixed metrics
  endpoints. Do not add a session-status endpoint or a threshold-write API.

## Fetching and accessibility

- Poll only the visible workspace: health/current metrics every 15 seconds,
  counters every 30 seconds, and the selected series every 5 minutes. Pause
  while the document is hidden, abort superseded requests, and allow only one
  in-flight request per resource.
- Use semantic controls first. Every status needs text plus a glyph; charts need
  a text summary and semantic table alternative; focus must remain visible;
  dialogs return focus to their trigger; and reduced-motion preferences must be
  honored.
- Preserve keyboard operation and a non-overlapping 320px layout. Do not use
  color as the only status signal.

## Required checks

```bash
cd client/admin
npm run format:check
npm run lint
npm run typecheck
npm run test:coverage
npm run build
npm run test:e2e
```

Keep Istanbul statements, branches, functions, and lines at or above 80 percent.
