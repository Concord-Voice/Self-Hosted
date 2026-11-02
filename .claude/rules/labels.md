---
description: Org-wide GitHub label taxonomy base — consumed by issue/PR skills and repo labelers
paths:
  - ".claude/**"
  - ".github/**"
---

# Label Taxonomy (org-wide)

Single source of truth for the GitHub label taxonomy across org repos. Each repo
overlays its own code-surface `area:` and app `domain:` values; the namespaces
and rules below are org-wide.

> **Priority is a project-board field, not a label.** MoSCoW: **P3=Must-have, P4=Should-have, P5=Could-have**; optional higher escalation tiers **P1=Critical, P2=Blocker** sit *above* the MoSCoW band. No `P*` labels exist.

## `type:` — mandatory, exactly one
- `type: bug` · `type: feature` · `type: enhancement` · `type: chore` · `type: epic`

## `area:` — mandatory, one or more (code-surface). Org-generic set:
- `area: ci-cd` · `area: docs` · `area: deps` · `area: ops` · `area: marketing` · `area: tooling`
- *(Per-repo overlay adds code-surface areas, e.g. a service/app: `api`, `desktop`, …)*

## `domain:` — additive (business/concern). Org-generic set:
- `area`/`domain` distinction: `domain:` is the concern, `area:` is the surface.
- `domain: authorization` · `domain: observability` · `domain: developer-experience` · `domain: performance` · `domain: testing-qa` · `domain: accessibility` · `domain: ux` · *(optional `domain: user-management`)*
- *(Per-repo overlay adds app domains specific to each repo's business surface.)*

## `release:` — mandatory exactly one for tracked work; values per-repo
- `release: future` — post-roadmap.
- *(Each repo defines its own `release: <stage|version>`. No codename labels — codename↔version mapping, if any, lives in that repo's CLAUDE.md.)*

## `risk:` — additive when relevant (org-generic, all kept)
- `risk: security` · `risk: privacy` · `risk: release-gate` · `risk: breaking-change` · `risk: data-loss`
- *(OWASP category goes in the issue body's `## Security Considerations`, not a label.)*

## `routine:` — additive, marks auto-generated issues (namespace kept; values per-repo)
## `workflow-validation:` — automation control, NOT triage (namespace kept; values per-repo; auto-applied by a labeler)

## Maintenance
A drift hook (`scripts/check-label-references.sh`, per-repo) greps that every
`` `<namespace>: <value>` `` referenced in skills/rules exists in this file. The
namespace set is `repo-config.json` `.labels.namespaces` (default:
`type area domain release risk routine workflow-validation`).
