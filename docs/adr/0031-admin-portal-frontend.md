# ADR-0031: Bespoke same-origin Admin Portal frontend

**Status:** Accepted
**Date:** 2026-07-14
**Deciders:** @markdrogersjr (project lead)
**Issue:** #1691
**Design basis:** Admin Web Console UI design

## Context

The control plane already provides separate administrator identity, password plus
WebAuthn authentication, local sessions, and four closed aggregate-metrics read
routes. Operators need an interface for those routes without widening the
metrics contract, adding a second dashboard identity, or making identity-bearing
operations data available to a dashboard product.

## Decision

Ship `client/admin` as a bespoke React and TypeScript Admin Portal. The control
plane serves its built assets at the same origin under `/admin/`; hosted
deployments keep Cloudflare Access as the outer gate, and the existing local
admin session remains the authorization boundary for metrics.

The portal consumes only the existing fixed routes: `/admin/api/v1/health`,
`/metrics/current`, `/metrics/series`, and `/counters`. It renders only
validated catalog identifiers and finite scalar values. Session state comes from
the protected response codes, and presentation thresholds stay browser-local;
there is no status endpoint or threshold-write API.

The primary Docker build context remains `services/control-plane`. Compose and
Buildx supply `client/admin` as the named `admin_ui` context, and a Node build
stage copies generated assets into the final control-plane image. `dist` is not
committed and the runtime image contains neither Node nor npm. Portal JavaScript
does not persist fetched metric samples, counter baselines, node IDs, health
events, credentials, or session facts; only validated presentation preferences
may persist. Origin-side metrics storage remains governed by ADR-0030.

## Rejected alternatives

### Embedded Grafana, Beszel, or Netdata

Rejected. An embedded dashboard introduces another authentication boundary,
iframe and CSP complexity, an outbound target and SSRF defense, a separate visual
system, and a broader data surface than the fixed, closed aggregate catalog
(see `.claude/rules/opsmetrics.md` and
`services/control-plane/internal/opsmetrics/catalog.go` for the current key set).

### Repository-root control-plane Docker context

Rejected. It would make the deployment manifest cover the whole repository and
weaken the existing exact build-context invariant. The named `admin_ui` context
keeps the production control-plane context narrow.

### Committed frontend `dist` with `go:embed`

Rejected. Generated bundles would create source-review churn and make ordinary
Go builds depend on a prebuilt artifact. The image build is the deterministic
composition point.

### New session-status or threshold-write APIs

Rejected. Existing protected responses establish session state, while
browser-local presentation thresholds do not justify a database, authorization,
or audit mutation surface.

## Consequences

- The Admin Portal is deployable with the control plane while remaining usable
  with future self-hosted perimeter work.
- The UI has a fixed, reviewable operations-data surface rather than a general
  dashboard query interface.
- Frontend build, coverage, dependency audit, browser tests, and production
  image assembly are first-class CI responsibilities.

## Related

- ADR-0030: Aggregate operations metrics boundary
- Admin Web Console UI design
