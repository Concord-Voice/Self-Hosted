# Self-Hosted Concord Voice Deployment: Design Dossier

**Status:** Design reference (pre-implementation) · **Date:** 2026-06-18
**Tracking epic:** #1615 — Self-hosted Concord Voice deployment (client + server + feature-gating)

**Current multi-service client requirements:** the
[Self-Hosted Services and Unified Client Connections Design of Record](service-connections-design-of-record.md)
preserves the 2026-10-08 client findings and defines independent services,
connection management, scoped preferences, admission, SSO, encrypted backup and unified presentation.
It describes the implementation target; this earlier dossier remains background
for installation and the original client slice.

> **This document is a design *reference*, not an ADR**. It consolidates the
> cross-cutting architecture shared by the epic's children, so they stay coherent
> during implementation. The implementing PRs author the **Architecture Decision
> Records** it anticipates: deployment-mode flag, capability-handshake contract,
> and self-hosted trust model. Those PRs pick ADR numbers late, per the
> `scripts/check-adr-numbers.sh` collision guard and the docs-ship-with-code rule
> in `CLAUDE.md`.
>
> Nothing here is a locked decision. Each child's `/dev-lifecycle` Phase 2 locks
> its own. Where this doc and a future ADR disagree, the ADR wins.

> **Shipped-status qualification (2026-07-13):** the gap inventory below is a
> historical pre-implementation snapshot. The basic installer, operator CLI,
> runtime server targeting, and capability handshake now exist. Current
> operator instructions live in `docs/runbooks/selfhost-quickstart.md`.

---

## 1. Scope

This epic delivers the **basic, working** self-hosted path. That path is a Linux installer, plus the
desktop-client changes for a non-`concordvoice.chat` server. A user can then
point the client at that server. The client stays fully functional in an isolated,
sandboxed self-hosted profile. It is the **foundation** that the full-scope
installer #210
extends, **not** a duplicate of it. That issue now covers air-gapped and offline
distribution only. The SaaS multi-host counterpart is
#1504, which
already names #210 as its self-hosted sibling.

**Out of scope here:** the offline or air-gapped image bundle (owned by #210) and
the web admin panel (owned by #1693). There is no per-server **runtime
licensing-enforcement service** — that technical component was cancelled in #210 on
2026-08-27. The *contractual* CVSL commercial per-server license is a separate thing
and is unaffected; it remains required. See the licensing-planes table below.

---

## 2. Current state — verified gaps

At the time of this design, Concord had a working single-operator, single-domain
SaaS deploy pipeline. It had **no path for a third party to stand up their own
instance**. The four rows below record the gaps verified then. They remain as
design history, and they are not current operator guidance.

| Surface | Gap (grounded) |
| --- | --- |
| **Deploy / ops** | The managed provisioning script is root-only, non-interactive, and assumes a real public domain with DNS already pointed + Let's Encrypt webroot reachability. Every service builds from source (Go control-plane, mediasoup native compile, custom postgres image). App code arrives **only** via GitHub-Actions rsync, because #848 retired the git-clone step. The deploy-applied `infrastructure/deploy/nginx/concordvoice.conf` **hardcodes `concordvoice.chat`** in every `server_name` / `ssl_certificate` path — a self-hoster gets a broken reverse proxy. |
| **Client server-targeting** | `client/desktop/src/renderer/config.ts` computes `API_BASE` as a build-time `const` with **no runtime setter**. The self-hosted UI exists as scaffolding, but it dead-ends. The `ConnectionSelector.tsx` → `ServerInput.tsx` pair validates a URL. Then `AuthFlow.handleServerConnect` (`AuthFlow.tsx:118-123`) only `console.debug`s and sets a placeholder `self-hosted-auth` step, with the inline comment *"Deferred: see #602 — requires self-hosted installer (#210)"*. Transient component state (`setServerUrl`) holds the entered URL. Nothing persists it, and nothing uses it to retarget the client. |
| **Cert / TLS** | No certificate pinning: every host, SaaS and self-hosted alike, is validated by Chromium's defaults (`docs/policies/api-tls-trust-model.md`). Chromium rejects a self-signed or private-CA self-hosted server, and there is no override surface. Removing the former `api.concordvoice.chat` pin did not change self-hosted behaviour — non-pinned hosts always deferred to Chromium. |
| **SSO + entitlements** | **Updated:** #662 added the single `INSTANCE_TYPE` capability seam and pre-auth `/server/capabilities`. #1294 added the entitlement resolver, its cache, and the client entitlement data layer. #1620 uses that existing `INSTANCE_TYPE=self-hosted` seam so the resolver returns the maximal current entitlement set for every user without adding per-feature branches. |

---

## 3. Target architecture — the shared skeleton

Five components the children must agree on. Implementation detail lives in each
child's own spec. This page is the contract between them.

### 3.1 Single `INSTANCE_TYPE` seam (#662)

Exactly **one** server-side notion of deployment mode (`self-hosted` vs `saas`):
`config.Config.InstanceType`, driven by the non-secret `INSTANCE_TYPE` env var.
Unknown or empty values normalize to `saas`, so misconfiguration never grants
self-hosted privileges by accident.

**Discipline:** SSO-suppression (#1619) and the entitlement grant (#1620) branch on
this **one** seam only — no scattered per-feature `if selfHosted` conditionals.
This mirrors the `resolveVideoPublisherCap` single-seam rule in
`.claude/rules/media-plane.md`.

### 3.2 Capability / version handshake contract (one descriptor, multiple riders)

The server advertises a versioned capability descriptor that the client
**clamps** to. When the self-hosted server is behind, the client degrades
gracefully: it hides or disables unsupported features, and never errors.
#662 owns this
work. That is the parentless capabilities-discovery issue, which absorbed #602,
and this epic adopted it as its capability child. The closest prior art to
**mirror, not overload** is the global, pre-auth, `no-store`
`GET /api/v1/client/config` (`clientconfig.Response`).

The descriptor carries, in one round-trip:

- `server.name`, `server.version`, and `server.instanceType` (`saas` or `self-hosted`).
- `auth.emailVerificationRequired`, `auth.mfaEnabled`, `auth.mfaMethods`, and `auth.oauthProviders`. An empty OAuth-provider array suppresses SSO for self-hosted.
- `features.voiceTiersSupported`, `features.e2eeEnforcedEverywhere`, `features.maxMembersPerServer`, and `features.entitlementMode`.
- Optional `features.activityHistorySupported`: `true` only when the server's
  Activity History gate is on, with an explicit replica count of one. Otherwise
  the server omits it.
- `policyVersion` for additive policy changes.

It is a **versioned, additively-evolvable** schema. New fields are `.optional()`.
Old clients ignore unknown fields, and new clients tolerate missing fields.
Validate at the client boundary with zod (the schema-at-boundary discipline in
`.claude/rules/frontend.md`). The Go
`Response` struct and its `clientConfigStore.ts` mirror are a **hand-maintained
contract**. Both sides land in the same PR.

The desktop treats valid `activityHistorySupported:true` as supported and a
schema-valid missing/false value as confirmed unsupported. A fetch failure, a non-2xx response, or a
malformed descriptor is an error rather than an unsupported verdict. Each
preserves the last confirmed support state. Capability support with
settings `available:false` is a separate operator/disclosure-unavailable state.

**Activity History disclosure and activation rider**. A self-hosted operator must
supply its own operator name and an absolute HTTPS privacy-policy URL. New opt-in
stays unavailable until it does. Missing or invalid disclosure does not prevent startup,
history reads, or deletion. The installer-owned repo-root `.env` is the sole
rollout-key source. `deploy.env.local` must not contain those keys. The installer
writes gate `false` and replica count `1`. `concord-selfhost up` refuses implicit
first activation. The explicit drained activation and rollback commands enforce
the single-replica contract.

### 3.3 Three cert trust modes (server) + client trust behavior

Server side (#1617,
invoked by the installer):

1. **Self-signed / local-CA** for local-only instances — generated by the installer.
2. **Let's Encrypt (ACME)** automation for domain holders. It reuses the certbot
   machinery and the already-`${DOMAIN}`-parameterized `copy-certs.sh`. It wires
   coturn's `COTURN_CERTS_DIR` and the nginx cert paths (`turns:5349`).
3. **Import-existing-cert** (operator supplies cert + key).

Client side (#1618):
Chromium rejects a self-signed instance by default today. The net-new client path
is an **explicit, opt-in, per-host trust delegation** for the entered self-hosted
origin **only**. It must **not** globally disable verification, so
`NODE_TLS_REJECT_UNAUTHORIZED` and `rejectUnauthorized:false` are forbidden. It
must also preserve the "never widen Chromium trust" invariant for the SaaS host
(there is no longer an `api.concordvoice.chat` SPKI pin to preserve). Standard ACME tooling only — no custom cert/crypto
(`.claude/rules/e2ee.md`).

### 3.4 Isolated client profile model

Today `pinUserDataPath.ts` pins userData to a **single** `ConcordVoice` dir. That
is an import-time side effect, and it must remain `main.ts`'s first import per
ADR-0020.
`token-meta.json`, `secure-token.dat`, `secure-e2ee.dat`, and `machine-id.json`
are single-tenant.

The net-new model (#1618)
layers a **per-server namespace** under the pinned root. That namespace is a
subdirectory keyed by the self-hosted origin. A self-hosted session then has its own token store,
E2EE keystore, and machine-id. This breaks neither the path-identity pinning
invariant nor the rule against re-deriving paths from the mutable `productName`. A fresh per-profile machine-id
correctly interacts with the server's `X-Machine-Id` token-theft detection (#89).

### 3.5 Candidate ADRs (authored in the implementing PRs)

1. **Deployment-mode flag** — the single self-hosted/SaaS seam, its env var, and the
   no-scattered-conditionals rule.
2. **Capability-handshake contract** — the `/server/capabilities` shape, clamp
   semantics, and the SSO/entitlement riders.
3. **Self-hosted trust model** — the three cert modes plus the client per-host
   trust delegation. Also why it does not widen Chromium trust for the SaaS host.

Each follows the ADR-number / migration-collision guard: **pick the number late**
and re-grep all surfaces (including any `.up.sql`) before committing.

---

## 4. Two licensing planes — keep them separate

The "self-hosted is not feature-gated" requirement lives on **one** of two distinct
planes. Conflating them is the trap.

| Plane | Owner | Self-hosted behavior |
| --- | --- | --- |
| **Per-user subscription** (premium feature gating) | #1294 entitlements + #1521 resolver | **Fully granted.** The resolver returns the complete entitlement set when deployment mode = self-hosted (#1620) — one branch, server-authoritative, the client never self-grants. |
| **Per-server license** (CVSL commercial license) | `docs/legal/commercial-license.md` — **contractual, not technically enforced** | Still required for for-profit / governmental self-hosting (Enterprise single-server or MSP & OEM Fleet). What was cancelled in #210 (2026-08-27) is the *runtime enforcement service* — no license keys, tokens, or validation ship. The obligation is honoured by agreement and audit. Separately, #2475 Ground Control is a self-hosted *subscription* for two network-side services, enforced SaaS-side at call time. |

A self-hosted deployment turns **off the per-user plane**; there is no per-server
**runtime licensing-enforcement** plane. The contractual per-server license is
unaffected — what does not exist is the machinery that would enforce it. The granted set derives from
`docs/design/entitlements/entitlement-matrix.md` (cite, do not duplicate).

---

## 5. Children & dependency sequencing

```
Epic #1615
├─ #1616  deploy script + concord-selfhost CLI      [sets INSTANCE_TYPE seam]        P3
├─ #1617  TLS/cert provisioning (self-signed/ACME)  [hardening]                      P4
├─ #1618  client self-hosted login + isolated profile + cert-trust                   P3
├─ #662   capability/version negotiation (ADOPTED)  [hardening; SSO+entitlement riders]
├─ #1619  SSO suppression on self-hosted            [blocked-by #1616 mode flag]      P3
└─ #1620  entitlements unlock-all on self-hosted    [resolver branch on INSTANCE_TYPE] P3
```

- **Minimal working path:** #1616 → #1618 → #1619 → #1620.
- **Hardening:** #1617 (Let's-Encrypt automation) + #662 (full capability negotiation).
- **Entitlement grant path:** #1521 landed the resolver package. #1620 adds the
  single self-hosted branch to that resolver seam.
- **Installer pre-flight traps to surface** (#1616): `MEDIA_PLANE_URL` must be a
  subdomain, or `config.go validate()` fatal-exits (#725). Coturn black-holes
  media if it advertises `127.0.0.1`, so behind-NAT hosts need an explicit
  `PUBLIC_IP`.

---

## 6. Security posture

- **Client-controlled server URL → privileged main-process code**
  (`.claude/rules/electron.md`, OWASP A10 SSRF
  and A01). Validate the self-hosted `apiBase` for HTTPS-or-localhost and URL
  shape. Extend the `isPermittedFrameUrl` allowlist deliberately, and never
  bypass it.
  Context isolation / sandbox / no-`nodeIntegration` / no-`@electron/remote` are hard
  constraints.
- **Trust-any-cert is a deliberate, scoped delegation** (OWASP A02 / A07). It is opt-in
  and per-host, and it never globally widens Chromium trust. Document the MITM
  exposure for local-only use. Let's Encrypt is the recommended default for any
  internet-reachable instance.
- **The entitlement grant is server-authoritative, never client-claimed** (OWASP
  A01). It keys on the server's `INSTANCE_TYPE` and resolves server-side. A forked
  client that claims "self-hosted" cannot grant itself features on a SaaS server.
- **SSO suppression's real control is server-side** (OWASP A01 / A07). The server
  omits the providers and rejects the routes. Client hiding is UX only. Advertise
  only the `Enabled` booleans (the `gifsEnabled` = `cfg.KlipyAPIKey != ""` precedent)
  — never client IDs / keys. Do **not** re-introduce `GOOGLE_CLIENT_SECRET` (#975).
- **Installer secrets bootstrapping** (`.claude/rules/deployment.md`
  Class-3 writer-conflict). The self-host installer owns a **separate** writer story
  for its `.env`. It must not re-create a server-side writer that conflicts with
  the org single-writer workflows.
- **Self-hosted clients are unsigned and non-attested by design.** The
  `REQUIRE_CLIENT_ATTESTATION` gate is SaaS-only. Never extend it onto
  self-hosted instances.

---

## 7. References

- **Epic & children:** #1615
  · #1616
  · #1617
  · #1618
  · #662
  · #1619
  · #1620
- **Siblings / dependencies:** #210 (air-gapped/offline distribution), #1504 (SaaS multi-host),
  #1294 / #1521 (entitlements), #971 (client-driven SSO), #850 (image-based deploy),
  #819 (deployment-guide docs), #602 (closed, folded into #662), #657 (closed epic),
  #89 (machine-id token-theft), #975 (Google PKCE), #725 (`MEDIA_PLANE_URL` guard).
- **Rule files:** `.claude/rules/` — `deployment.md`, `electron.md`, `backend.md`,
  `frontend.md`, `media-plane.md`, `e2ee.md`, `observability.md`, `migrations.md`,
  `tests.md`.
- **Prior-art design dossiers (pattern this doc follows):**
  `docs/design/saas-ha-multiregion/`, `docs/design/entitlements/`,
  `docs/design/media-egress/`.
- **External:** OWASP Top 10:2025 (A01 / A02 / A05 / A07 / A10). ACME (RFC 8555).
