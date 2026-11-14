# Security #162 Exact Trusted Proxy Provenance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make nginx the only client-address authority and let the control and
media planes trust only the exact backend-visible nginx peer.

**Architecture:** nginx overwrites both forwarding headers with its observed
peer. Production Go and TypeScript configuration accepts only IPv4 `/32` and
IPv6 `/128` trusted peers. Compose explicitly selects the shared services
bridge as each application container's default gateway. Before application
start, the self-host lifecycle inspects and pins that network and atomically
persists its exact gateway route; after the canonical guarded rebuild it proves
the running endpoints match.

**Tech Stack:** nginx, Bash, Docker Compose, Go/Gin 1.12, Node.js 24
`net.BlockList`, TypeScript/Vitest.

**Spec:** `docs/superpowers/specs/2026-11-14-162-trusted-proxy-provenance-design.md`

## Global Constraints

- Self-Hosted source lands first; Hosted is a later reviewed mirror with its
  own measured peer addresses and gates.
- Production trust entries are exact host routes only: IPv4 `/32`, IPv6
  `/128`.
- Never echo configured proxy entries, observed addresses, headers, or
  environment-file content into application logs.
- No new dependency, secret, signed header, API route, database migration, or
  per-consumer client-IP implementation.
- Preserve current rate windows, ban keys, status codes, and thresholds.
- Use isolated PostgreSQL/Redis for handler integration tests; never the shared
  `concord` database. Use shared Go caches.
- #162 precedes #163. Do not fold IPv6 `/64` aggregation or Redis degradation
  policy into this branch.

## Review Focus

- A mixed exact+broad trust list must fail as a whole, never keep the exact
  entry and silently skip the broad one.
- IPv4-mapped IPv6 socket peers must match an exact IPv4 rule exactly as the
  existing admission gate does.
- A recreated Docker network must be remeasured before any application service
  starts.
- YAML network order is not a gateway-selection mechanism. Both application
  endpoints must report `concordvoice-services` at `GwPriority: 1` before the
  deployment reports success.
- Discovery and guarded rebuild are one deploy-lock transaction. Never create,
  recreate, stop, or prebuild an application service merely to discover its
  future gateway.
- A stopped container does not pin a Docker network (Moby #48851). An empty
  membership map after `up --no-start` is expected; exact trust is not
  published until a running allowed member pins the same inspected network.
- Direct self-host `restart` is verification-only. It refuses legacy or
  mismatched canonical/container values and directs the operator to `up`;
  restart never performs migration.
- An `.env` rewrite failure must preserve the old file byte-for-byte and
  start no service.
- Removing generic Cloudflare real-IP restoration must leave direct-origin
  client attribution correct and document edge-IP attribution for
  Cloudflare-proxied self-host traffic.

---

### Task 1: Freeze the trust-boundary census and exploit baseline

**Files:**
- Modify: `services/control-plane/internal/api/trusted_proxy_test.go`
- Modify: `services/media-plane/tests/admissionGate.test.ts`
- Create: `infrastructure/deploy/tests/test-selfhost-trusted-proxy.sh`

**Interfaces:**
- Produces: retained negative controls for edge overwrite, exact Gin trust,
  and exact media trust. Later tasks turn them green.

- [ ] **Step 1: Record the complete production census.** Use `rg` to list
  every `proxy_set_header X-Real-IP`,
  `proxy_set_header X-Forwarded-For`, `TRUSTED_PROXY_CIDRS`,
  `TRUSTED_PROXIES`, `SetTrustedProxies`, and
  `resolveClientIp` writer/consumer. Put the expected file list in the new
  shell test so a new deployment writer fails review rather than drifting.
- [ ] **Step 2: Add failing Gin tests.**
  `TestClientIP_ExactProxyHonorsOverwrittenAddress` uses
  `172.19.0.1/32`; `TestClientIP_PrivateNeighborCannotSupplyAddress`
  uses peer `172.19.0.2` with both forged headers and expects the peer.
- [ ] **Step 3: Add the failing media regression.**
  `ignores both forwarded headers from a neighbouring RFC1918 peer` trusts
  only `172.19.0.1`, sends from `172.19.0.2`, and asserts the peer is the
  admission key. Keep the mapped-IPv6 case.
- [ ] **Step 4: Add the failing nginx/template assertions.** The fixture
  requires every application proxy location to set both headers to
  `$remote_addr`; rejects `$proxy_add_x_forwarded_for`,
  `$http_x_forwarded_for`, and unconditional
  `real_ip_header CF-Connecting-IP`; runs the existing real nginx fixture
  when nginx is available.
- [ ] **Step 5: Run the targeted tests.** The new exact-neighbor unit tests may
  already pass; the nginx census must fail on the append and Cloudflare block.
  Record the VULN-008 and VULN-017 overlay baseline against this exact head.
- [ ] **Step 6: Commit.** `test: pin exact proxy provenance before fixing client IP trust`

---

### Task 2: Make nginx overwrite caller address claims

**Files:**
- Modify: `infrastructure/deploy/nginx/concordvoice.conf`
- Modify: `infrastructure/deploy/nginx/admin-console.conf.template`
- Modify: `infrastructure/deploy/tests/test-selfhost-trusted-proxy.sh`
- Modify when required by fixture selection:
  `infrastructure/deploy/tests/selfhost-nginx-fixture.sh`

**Interfaces:**
- Produces: every upstream receives one `X-Real-IP` and one
  `X-Forwarded-For`, each equal to nginx `$remote_addr`.

- [ ] **Step 1: Replace every application proxy location's XFF value.** Change
  only `$proxy_add_x_forwarded_for` to `$remote_addr`; retain existing
  WebSocket, Host, scheme, timeout, and body-size behavior.
- [ ] **Step 2: Remove the generic Cloudflare real-IP block.** Remove the
  shared `set_real_ip_from` list, `real_ip_header`, and
  `real_ip_recursive` from the generic Self-Hosted vhost. Do not add an
  unauthenticated replacement.
- [ ] **Step 3: Extend the fixture to an echo upstream.** Send forged incoming
  XFF and X-Real-IP through HTTP and WebSocket locations. Assert the upstream
  sees one address in each header, both equal to nginx's observed peer.
- [ ] **Step 4: Verify.** Run
  `bash infrastructure/deploy/tests/test-selfhost-trusted-proxy.sh` and the
  existing nginx/blue-green fixture suites selected by the changed template;
  `nginx -t` must pass. Prove non-vacuity once by restoring one append form
  under an overlay and seeing the new test fail.
- [ ] **Step 5: Commit.** `fix: overwrite client IP headers at the self-host edge`

---

### Task 3: Reject broad control-plane proxy authority in production

**Files:**
- Modify: `services/control-plane/pkg/config/config.go`
- Modify: `services/control-plane/pkg/config/config_test.go`
- Modify: `services/control-plane/internal/api/trusted_proxy_test.go`

**Interfaces:**
- Produces:
  `func trustedProxyCIDRsAreHostRoutes(cidrs []string) bool`.
- Consumes: the existing validated
  `Config.TrustedProxyCIDRs []string`; no second environment parser.

- [ ] **Step 1: Write the production matrix.** Reject empty, malformed, IPv4
  prefixes `/0`, `/8`, `/16`, `/24`, `/31`; IPv6 `/0`,
  `/64`, `/127`; and mixed exact+broad lists. Accept bare/exact IPv4 and
  IPv6 host routes. Confirm development retains its existing CIDR behavior.
- [ ] **Step 2: Run the focused config tests and confirm broad values pass
  before the fix.**
- [ ] **Step 3: Implement the smallest guard.** Parse each already validated
  CIDR with the standard library, compare prefix bits to address bits, and add
  one `Config.validate()` production guard. Remove the obsolete broad-list
  warning and Cloudflare-CIDR warning; neither represents the new contract.
- [ ] **Step 4: Run.**
  `go test ./pkg/config ./internal/api -run 'TrustedProxy|ClientIP' -count=1`
  and the same packages under `-race`. Run scoped golangci-lint.
- [ ] **Step 5: Commit.** `fix: require exact trusted proxy peers in production`

---

### Task 4: Enforce the same exact-peer contract in the media plane

**Files:**
- Modify: `services/media-plane/src/lib/admissionGate.ts`
- Modify: `services/media-plane/src/config/index.ts`
- Modify: `services/media-plane/tests/admissionGate.test.ts`
- Modify: `services/media-plane/tests/config.test.ts`

**Interfaces:**
- Produces:
  `parseTrustedProxyEntry(entry: string): { address: string; prefix: number; family: 'ipv4' | 'ipv6' } | null`.
- `compileTrustedProxies` consumes that parser; production config uses the
  returned prefix to require `32` or `128`.

- [ ] **Step 1: Add config tests.** Production rejects empty, malformed,
  broad, and mixed exact+broad `TRUSTED_PROXIES`; accepts bare addresses and
  exact host-prefix spellings. Errors name only the 1-based entry position.
  Development retains ranges.
- [ ] **Step 2: Extract the existing parser without changing matcher
  behavior.** `compileTrustedProxies` still builds one `BlockList`, retains
  mapped-IPv6 matching, and never logs values.
- [ ] **Step 3: Add the production startup guard.** It runs before Socket.IO
  listens and exits with the repository's existing `FATAL:` convention.
  Remove comments that call broad Docker ranges the production contract.
- [ ] **Step 4: Run.**
  `npm test -- --run tests/admissionGate.test.ts tests/config.test.ts`,
  `npm run typecheck`, and `npm run lint` from
  `services/media-plane`.
- [ ] **Step 5: Commit.** `fix: trust exact nginx peers for media admission`

---

### Task 5: Select, discover, and persist the self-host published-port gateway

**Files:**
- Modify: `infrastructure/deploy/concord-selfhost`
- Modify: `infrastructure/deploy/concord-ctl.sh`
- Modify: `infrastructure/deploy/install-selfhost.sh`
- Modify: `docker-compose.yml`
- Modify: `docker-compose.production.yml`
- Modify: `docker-compose.selfhost.yml` only if the rendered model needs an
  explicit variable override
- Modify: `scripts/tests/test-install-selfhost.sh`
- Modify: `scripts/tests/test-concord-selfhost.sh`
- Modify: `infrastructure/deploy/tests/test-selfhost-port-posture.sh` only
  for the new exact-route rendered invariant
- Modify: `infrastructure/deploy/tests/test_selfhost_acceptance.py`
- Modify: `infrastructure/deploy/tests/selfhost-acceptance.py`

**Interfaces:**
- Produces shell globals
  `SELFHOST_TRUSTED_PROXY_CIDRS` and
  `SELFHOST_TRUSTED_PROXIES`.
- Produces
  `selfhost_prepare_trusted_proxy_provenance`, called from the self-host
  `rebuild` path after the deploy transaction lock is acquired and before the
  first Compose mutation that can build, recreate, or start an application
  service.
- `install-selfhost.sh` remains the root `.env` schema authority and fresh
  publisher. The helper is its private, deploy-lock migration seam: it writes
  only `TRUSTED_PROXY_CIDRS` and `TRUSTED_PROXIES`, then exports both for the
  current process. Do not expose a generic dotenv setter.

- [ ] **Step 1: Write lifecycle failures first.** Cover Compose 2.33.0 refusal
  and 2.33.1 admission, fresh network, known legacy broad values, network
  recreation with a new gateway, exact IPv4/IPv6 gateways,
  missing/multiple/foreign-labeled networks, non-bridge driver, service
  membership mismatch, absent/equal/wrong `GwPriority`, endpoint/network
  gateway mismatch, invalid gateway, inspect failure, atomic replacement
  failure, and an unknown custom broad value. Add a fresh-install case with no
  application images, empty network membership after stopped materialization,
  network ID/gateway change between materialization and holder start, and an
  upgrade case with running application containers. Before canonical handoff,
  assert there is no app build, create, recreate, stop, or start. Every
  pre-application error asserts no application start/rebuild.
- [ ] **Step 2: Write file-safety tests.** The successful rewrite changes only
  the two values, preserves all other bytes and secret lines, owner, mode, and
  final newline. A recognized legacy installer file may gain the missing media
  key; a missing control-plane key, duplicate, unknown broad value, third-key
  mutation request, symlink, or inode race is refused. Failure leaves the
  original byte-identical and removes its staging file.
- [ ] **Step 3: Make gateway selection explicit and raise the floor.** Convert
  both application services to network long syntax with
  `concordvoice-services: { gw_priority: 1 }` and every other endpoint at the
  default `0`. Raise all installer, controller, acceptance-runner, fixture,
  comment, and operator-doc Compose floors from 2.24.5 to 2.33.1. Do not use
  YAML order or `priority`; Docker documents that neither selects the default
  gateway. The Docker Engine floor remains 29.8.2.
- [ ] **Step 4: Change Compose consumers.** Production media and control
  services consume required `TRUSTED_PROXIES` and
  `TRUSTED_PROXY_CIDRS`; remove every broad production literal. Safe exact
  loopback placeholders are used only for the pre-discovery render.
- [ ] **Step 5: Implement pre-start network preparation under the deploy
  lock.** Run from the guarded `rebuild` transaction rather than as a separate
  `concord-selfhost` wrapper mutation. Reuse ownership/inspection patterns from
  `selfhost_live_port_check` and the stage-network gate. Render and assert the
  gateway-priority model. When the owned services network is absent, prove the
  project has no containers and run only
  `docker compose up --no-start --no-deps --pull missing nats` to materialize
  it. Accept empty membership after that command; stopped containers do not
  pin networks. Require the network's Compose labels, bridge driver, allowed
  running project-only members, and one valid IPAM gateway, then record its ID
  and gateway. If no running allowed member already pins it, start only NATS
  with `up -d --no-deps --no-build --pull missing --wait --wait-timeout 180`,
  and require the same ID+gateway plus running membership afterward. Never
  create or prebuild control-plane/media-plane here. Convert the gateway to an
  exact route using the existing Python standard-library fixture style; do not
  add a parser dependency.
- [ ] **Step 6: Implement fresh-install and upgrade migration.** The installer
  remains the schema authority, no longer asks for or emits a broad default,
  and fresh-publishes both variables. The resolver is only the locked
  derived-value migration seam and recognizes only the spec's legacy default
  strings; a different broad value gets a fixed refusal message. `update`'s
  existing backup remains before the tag switch; the new tag's `up` performs
  migration before services start.
- [ ] **Step 7: Preserve the canonical build/start and prove live endpoints.**
  After atomic persistence, export the exact values in the current controller
  process and continue through the existing guarded rebuild; it remains the
  only application image builder and starter. Before `Done`, inspect both
  running application endpoints and require services-network `GwPriority: 1`,
  no equal/higher competitor, and gateway equality with the pre-start IPAM and
  persisted route. On mismatch return nonzero but leave services running in
  fail-safe under-trust; never restore or widen the legacy CIDR. Then send
  forged headers through host nginx and require edge-observed attribution
  before accepting the target. Run installer, lifecycle, port-posture,
  storage-mode, acceptance, and publication-manifest fixtures selected by the
  changed writers.
- [ ] **Step 8: Guard direct self-host restart.** Before restart mutates any
  container, require canonical exact trust values, both live application
  container environment values, and both endpoint network-ID/gateway/priority
  proofs to agree. On any legacy or mismatched state, refuse and direct the
  operator to `concord-selfhost up`. Re-run endpoint proof after a permitted
  restart. Parse inspected environment internally and never log its values.
- [ ] **Step 9: Commit.** `fix: pin self-host proxy trust to the published-port bridge`

---

### Task 6: Prove security consumers inherit the corrected address

**Files:**
- Modify the smallest existing focused integration-test files under:
  `services/control-plane/internal/auth/`,
  `services/control-plane/internal/mfa/`,
  `services/control-plane/internal/stepup/`,
  `services/control-plane/internal/admin/`, and
  `services/control-plane/internal/users/`
- Prefer one table-driven test helper in an existing shared test file; do not
  add production wrappers.

**Interfaces:**
- Consumes: Gin `ClientIP()` configured with one exact trusted peer.
- Produces: proof that login, recovery, MFA verify, step-up password, admin
  login, and key reveal charge the observed client address.

- [ ] **Step 1: Build the table.** For each named route, seed independent
  victim and observed-client Redis keys, send the request from the exact nginx
  peer with nginx's single observed-client header plus a caller-forged
  historical chain, and assert only the observed-client key changes.
- [ ] **Step 2: Include no-mutation assertions.** The request may fail for its
  ordinary invalid credential, but it must not charge the victim key or
  mutate unrelated account state.
- [ ] **Step 3: Run against isolated PostgreSQL/Redis.** Run each focused
  package normally and under `-race`. Re-run VULN-008 and VULN-017 overlays;
  both must fail for the fixed reason, not because setup or a prerequisite
  fails.
- [ ] **Step 4: Commit.** `test: prove auth controls use the edge-observed address`

---

### Task 7: Documentation, mirror handoff, and exact-head validation

**Files:**
- Modify: `CHANGELOG.md`
- Modify: `docs/architecture.md`
- Modify: `docs/runbooks/selfhost-quickstart.md`
- Modify: `docs/runbooks/selfhost-machine-output.md` only if output changes
- Modify: `services/control-plane/.env.example`
- Modify: `services/control-plane/README.md`
- Modify: `services/media-plane/.env.example`
- Modify: `services/media-plane/README.md`
- Modify comments next to all Compose writers/consumers

**Interfaces:**
- Produces: operator migration instructions and a topology-neutral Hosted
  mirror checklist.

- [ ] **Step 1: Document the contract.** Explain exact host routes, lifecycle
  discovery, known-legacy migration, unknown-broad refusal, network recreation,
  the Compose 2.33.1 floor, explicit default-gateway selection, writer
  ownership, and rollback. State that the installer owns the Self-Hosted env
  schema and fresh publication while the deploy-lock helper migrates only its
  two derived trust keys. Keep both keys in service examples. State that generic
  Cloudflare self-host traffic is attributed to the edge until #80 supplies
  authenticated origin provenance.
- [ ] **Step 2: Write the Hosted handoff.** List every mirrored source/config
  file, require independent control/media peer measurement, and prohibit
  copying the Self-Hosted gateway. Keep merge, deployment, and live canary
  evidence separate.
- [ ] **Step 3: Run drift probes.** `rg` must find no production broad trust
  literal and no application proxy location using an append/pass-through
  header. A writer census must show only installer fresh publication and the
  locked two-key migrator for root `.env`, and every installer/Compose/example
  schema surface must carry both variables. Run
  `./scripts/update-claude-md-counts.sh` if present; otherwise record that the
  extracted repository has no local count artifact.
- [ ] **Step 4: Run the complete selected test matrix.** Go tests/race/lint,
  media tests/typecheck/lint, nginx fixture, installer/lifecycle/deployment
  fixtures, `git diff --check`, and the repository quality gate. Record
  exact-head results and any configured gate that is unavailable.
- [ ] **Step 5: Dispatch Phase-4/8 security reviewers.** Include red-team,
  socket-io, deployment-config, security, code, test, docs, and scope review.
  Resolve every concrete finding and repeat required security cycles.
- [ ] **Step 6: Commit.** `docs: define exact client IP provenance and rollout`
- [ ] **Step 7: Open the Self-Hosted PR.** Include the VULN overlay evidence,
  Cloudflare #80 residual, exact topology proof, quality criteria, and
  independent Hosted mirror requirement. Do not merge without the lifecycle's
  Phase-9 approval.

## Self-review

- Spec coverage: edge, control, media, lifecycle migration, six consumers,
  original overlays, docs, Hosted mirror, #80 residual, and quality criteria
  each map to a task.
- Type consistency: the media parser is the single parser for compile and
  production validation; the shell resolver exports the two names Compose
  consumes.
- Proportion: the plan names interfaces and assertions without transcribing
  their bodies.
- Excluded work: no threshold changes, signed header, API route, migration,
  historical backfill, #163 aggregation, or #80 edge implementation.
