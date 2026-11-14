# Security #162: Exact Trusted Proxy Provenance

**Status:** Final design for Security #162
**Scope:** Concord-Voice/Self-Hosted source change, followed by a separately
reviewed Concord-Voice/Hosted mirror and deployment qualification
**Dependency order:** #162 lands before #163; Security #80 remains the owner of
the outer Cloudflare/origin and published-port contract.

## 1. Problem

The control plane and media plane accept client-address headers from broad
private CIDRs while nginx appends the caller's existing
`X-Forwarded-For`. A caller that can reach either service from one of those
ranges can select the address used by bans, lockouts, audit/session records,
per-IP rate limits, and the media handshake ceiling. The default also treats
unrelated containers and LAN peers as authorities for client identity.

The repair is one provenance contract at the ingress seam:

1. nginx is the only process allowed to state the client address to an
   application service;
2. nginx overwrites, rather than extends, caller-provided forwarding headers;
3. each application trusts only the exact direct peer address it observes for
   nginx; and
4. production refuses broad, empty, malformed, or unmeasured trust entries.

Every existing `c.ClientIP()` and media admission consumer inherits that
answer. Per-consumer production rewrites would duplicate the policy and are
out of scope.

## 2. Goals

- A request cannot change its ban, lockout, limiter, audit, or session address
  by supplying `X-Forwarded-For` or `X-Real-IP`.
- The control plane and media plane accept forwarding headers only from exact
  host routes: IPv4 `/32` and IPv6 `/128`.
- Self-host fresh install and upgrade explicitly select the Docker bridge used
  for published ports, derive the gateway observed from host nginx, persist
  exact routes, and refuse ambiguous topology.
- The generic self-host nginx template does not treat a shared Cloudflare
  address range as proof that `CF-Connecting-IP` belongs to this deployment.
- Existing legitimate direct and proxied traffic continues to reach the same
  routes; only address attribution authority changes.
- Self-Hosted lands first. Hosted receives a reviewed mirror only after its
  backend-visible peers are measured independently.

## 3. Non-goals

- Do not change thresholds, windows, Redis keys, or ban semantics.
- Do not add a signed client-IP header, proxy secret, dependency, or second
  address parser.
- Do not change the order or behavior of #163's IPv6 `/64` aggregation and
  Redis fail-closed work.
- Do not claim that Cloudflare source CIDRs authenticate the deployment's own
  edge path. Security #80 owns the authenticated origin/published-port
  contract.
- Do not backfill historical session IPs.

## 4. Governing constraints

### 4.1 Trust boundary

Gin 1.12 documents that `SetTrustedProxies` accepts IP addresses or CIDRs and
that `ClientIP()` trusts forwarded headers only from those peers. The
production list therefore contains only the actual direct nginx peer. A
network range is not equivalent evidence.

Reference: Context7 `/gin-gonic/gin/v1.12.0`,
`Engine.SetTrustedProxies` and `Context.ClientIP`.

nginx documents that `$proxy_add_x_forwarded_for` includes the client-supplied
field and appends `$remote_addr`. That is precisely the authority mix this
change removes. Every application proxy location sets:

```nginx
proxy_set_header X-Real-IP       $remote_addr;
proxy_set_header X-Forwarded-For $remote_addr;
```

Reference:
<https://nginx.org/en/docs/http/ngx_http_proxy_module.html#proxy_set_header>.

### 4.2 Cloudflare identity

`set_real_ip_from` defines sources nginx trusts to replace
`$remote_addr` from a header. A public Cloudflare range proves only that the
TCP peer is in that shared range; it does not prove the request traversed this
deployment's authenticated Cloudflare path.

The generic Self-Hosted vhost therefore removes the unconditional
`set_real_ip_from ...`, `real_ip_header CF-Connecting-IP`, and
`real_ip_recursive on` block. A self-hosted deployment behind Cloudflare is
attributed to the Cloudflare edge until Security #80 supplies and tests an
authenticated origin path. This is a safe loss of address granularity, not a
reason to keep an unauthenticated identity claim.

Reference:
<https://nginx.org/en/docs/http/ngx_http_realip_module.html>.

### 4.3 Production configuration

For `ENVIRONMENT=production`:

- `TRUSTED_PROXY_CIDRS` is non-empty and each parsed prefix is exactly
  `/32` or `/128`;
- `TRUSTED_PROXIES` is non-empty and each entry is an exact IPv4 or IPv6
  address (bare address or an exact host prefix);
- invalid and broad entries are fatal even when mixed with valid entries;
- logs identify the variable and entry position, never echo an operator value;
- development and CI may retain existing CIDR support for fixtures and local
  topologies.

The issue's proposed “reject broader than `/24`” rule is superseded. A
`/24` still grants 254 unrelated peers authority to select client identity
and does not satisfy “the one proxy hop actually deployed.”

### 4.4 Self-host topology discovery

nginx is a host process, not a Compose service. It connects to
`127.0.0.1:8080` and `127.0.0.1:3000`; Docker publishes those ports from the
control-plane and media-plane bridge-network endpoints. The backend-visible
peer is therefore the gateway of the container's **default-gateway network**.
It is not an nginx-container address, loopback, or the host's public address.
Production evidence from #64 recorded `172.19.0.1`, the selected Docker bridge
gateway.

The current Compose model does not select that network explicitly. Both
services join `concordvoice-data` and `concordvoice-services`, in opposite YAML
orders, with equal implicit gateway priority. Compose documents that YAML
`priority` and attachment order do not choose the default gateway. Docker
Engine breaks an equal-priority tie by network name, but that fallback is not
the security contract this change should trust. With the repository's two
generated names sharing one project prefix, `concordvoice-data` sorts before
`concordvoice-services`; on the supported Engine floor the current published
ports therefore use the data-network gateway. The previous proposal to inspect
`concordvoice-services` without first making it the default gateway was
therefore incorrect.

The repaired model uses network long syntax and assigns
`concordvoice-services` `gw_priority: 1` for both application services;
`concordvoice-data` remains at the default `0`. Docker Compose 2.33.1 is the
first supported release with this field, so the self-host Compose floor rises
from 2.24.5 to 2.33.1. The existing Docker Engine floor, 29.8.2, already exceeds
the Engine 28/API 1.48 requirement. No network subnet or gateway is hard-coded.

References:

- <https://docs.docker.com/reference/compose-file/services/#gw_priority>
- <https://docs.docker.com/reference/api/engine/version-history/#v148-api-changes>
- <https://github.com/moby/moby/discussions/53256>
- <https://github.com/moby/moby/issues/48851>

The selected gateway is stable across ordinary container restarts while the
Compose network remains. It can change when `down` removes the network and a
later `up` recreates it, or when Docker's address pools change. It must be
derived before every start path rather than copied from another host or from a
previous installation.

Before a self-host `up` or the post-switch start in `update`:

1. refuse Docker Compose older than 2.33.1 before mutation;
2. enter the same deploy transaction lock that owns the subsequent guarded
   rebuild; discovery and rebuild are one transaction, not two wrapper calls;
3. render the full self-host model with safe exact loopback placeholders and
   require both application services to select `concordvoice-services` at
   `gw_priority: 1`, with every other attached network lower;
4. identify the one Compose-owned `concordvoice-services` bridge by its project
   and network labels. If it does not exist, first prove that the project has
   no containers, then use NATS as a stopped network materializer:
   `docker compose up --no-start --no-deps --pull missing nats`. This command
   neither needs the locally built application images nor starts NATS. Moby
   #48851 means the stopped container is not reported in network membership and
   does not pin the network; an empty `Containers` map is expected here;
5. inspect the resulting network and require the expected ownership labels,
   bridge driver, allowed running project-only membership, exactly one valid
   IPAM gateway, and record both its immutable ID and gateway. A missing network
   with any project container is inconsistent and refused rather than repaired;
6. if no already-running allowed project member pins that network, start only
   the pinned NATS dependency with
   `docker compose up -d --no-deps --no-build --pull missing --wait
   --wait-timeout 180 nats`. The self-host port-posture gate has already proved
   that NATS publishes no host port. Reinspect and require the same network ID,
   the same gateway, and running NATS membership. Existing running application
   or NATS membership skips this step, so upgrades do not recreate a service;
7. convert the inspected network gateway to an exact `/32` or `/128` route;
8. atomically replace only `TRUSTED_PROXY_CIDRS` and
   `TRUSTED_PROXIES` in the root `.env`, preserving all other bytes,
   ownership, and mode; and
9. export the same values for the current Compose invocation so container
   creation cannot read stale process state; then let the existing guarded
   rebuild remain the only application image builder and service starter.

After the guarded rebuild, but before reporting `Done`, inspect both running
application containers. Each must report the selected services-network
endpoint at `GwPriority: 1`, no attached endpoint at an equal or higher
priority, and a gateway equal to the pre-start network IPAM gateway and the
persisted host route. This two-stage proof is intentional: fresh installs do
not yet have control-plane or media-plane images, while creating or recreating
their stopped containers on an upgrade would interrupt the running services
before the canonical handoff. A post-start mismatch returns failure but leaves
the services running in fail-safe under-trust; it never widens the allowlist or
rolls back to the broad legacy value.

The directly callable self-host `concord-ctl.sh restart` is not a migration
path: restart does not recreate containers and therefore cannot apply changed
environment values. Before restarting, it must require that the canonical
exact routes, both running containers' trust-variable values, and both live
endpoint network-ID, gateway, and priority proofs already agree. Otherwise it
refuses without mutation and directs the operator to `concord-selfhost up`.
The same endpoint proof runs after a permitted restart.

The resolver may migrate the known legacy defaults
`172.16.0.0/12,10.0.0.0/8`,
`172.16.0.0/12,10.0.0.0/8,192.168.0.0/16`, and
`172.19.0.0/16`. Any other broad value is treated as an operator choice and
refused with an instruction to replace it with measured exact routes. It is
never narrowed by guessing.

`install-selfhost.sh` remains the schema authority and fresh publisher for the
Self-Hosted root `.env`; it declares both trust keys together. The deploy-lock
helper is a sanctioned derived-value migrator inside that same Self-Hosted
writer chain, not an independent or reusable environment writer. It may add a
missing `TRUSTED_PROXIES` only for a recognized legacy installer file and may
replace only `TRUSTED_PROXY_CIDRS` and `TRUSTED_PROXIES` after bridge proof.
Duplicates, a missing control-plane key, an unknown broad value, malformed
dotenv, symlinks, concurrent inode replacement, or any request to change a
third key are refused. Every other byte plus file ownership and mode is
preserved. The installer, Compose consumers, service examples, and operator
docs must name both keys so the migration does not create a hidden schema.

If the Compose floor, render, stopped network materialization, running-holder
proof, network inspection, atomic rewrite, or final rendered-Compose
verification fails, no application service starts and an already running
application is not recreated. A newly started NATS holder may remain internal
and healthy for diagnosis; it has no self-host host port. `down` removes the
network as today; the next `up` measures the newly allocated gateway before
starting. Endpoint inspection and the through-nginx attribution probe run
before success is reported. Their failure leaves a newly started application
in fail-safe under-trust for operator diagnosis, returns nonzero, and is not
permission to widen the trusted range.

### 4.5 Writer/consumer parity

These are one contract and change together:

| Surface | Required state |
|---|---|
| Installer/root `.env` schema authority | Fresh-publishes both trust variables; no broad default prompt |
| Locked self-host trust migrator | Updates only the two installer-owned derived keys after bridge proof; no general dotenv mutation API |
| `concord-selfhost up/update` | Invokes the locked migrator and persists exact routes before application start |
| Direct self-host `concord-ctl restart` | Refuses unless canonical values and existing live endpoints already agree; never migrates |
| Base/production/self-host Compose | Selects `concordvoice-services` as both application services' default gateway; no production broad literal; consumes both exact variables |
| Control-plane config | Parses as today; production validation accepts host routes only |
| Gin router | Continues using the validated list through `SetTrustedProxies` |
| Media config | Production validation accepts exact peers only |
| Media admission gate | Continues matching the compiled list; untrusted peers use their socket peer |
| nginx user/API/media/admin locations | Both client-address headers overwrite with `$remote_addr` |
| Generic self-host Cloudflare block | Removed until authenticated origin provenance exists |
| Examples and operator docs | Exact-route semantics and migration behavior |

The callers of `c.ClientIP()` do not change. Tests at login, recovery,
MFA verify, step-up password, admin login, and key reveal prove that common
derivation reaches each security consumer.

## 5. Options considered

### Option A: exact peer trust plus edge overwrite — selected

Overwrite both headers at nginx, trust only measured host routes, reject broad
production configuration, and make the self-host lifecycle persist the
measured gateway. This closes direct-service and header-chain bypasses at their
shared sources.

### Option B: overwrite at nginx only

This is smaller, but direct access from any peer in the broad private ranges
can still state an arbitrary address. It does not close VULN-008, VULN-017, or
VULN-145.

### Option C: signed application header

An HMAC header would survive more complicated proxy chains, but it creates a
new secret, rotation path, parser, and failure mode. The deployed topology has
one application-facing proxy hop, so exact network provenance is sufficient.

## 6. Detailed behavior

### 6.1 Edge forwarding

All control-plane, media-plane, WebSocket, upload, invite, and admin proxy
locations set both address headers from `$remote_addr`. No location uses
`$proxy_add_x_forwarded_for`, passes `$http_x_forwarded_for`, or inherits a
different value.

The nginx fixture sends a request carrying forged values through the real
template to an echo upstream. The upstream receives one address in each header,
both equal to nginx's observed peer. A mutation back to
`$proxy_add_x_forwarded_for` must fail the fixture.

### 6.2 Control plane

`computeTrustedProxyCIDRs` and `parseTrustedProxyCIDRs` retain their
current signatures. A small helper validates the already parsed production
prefixes as host routes. `Config.validate()` returns a `FATAL:` error before
the router is built when any production entry is broad.

`configureTrustedProxies` remains the single Gin wiring point. It does not
set `TrustedPlatform` because that would bypass the proxy-source check.

### 6.3 Media plane

`compileTrustedProxies` keeps range support because development tests and
non-production deployments use it. Production config validation is the
enforcement seam: an empty, malformed, or non-host entry exits before Socket.IO
listens.

`resolveClientIp` continues to prefer valid `X-Real-IP`, then the last
valid `X-Forwarded-For` value, only when the socket peer is in the exact
compiled set. From any neighboring private address, both headers are ignored
and the socket peer is the admission key.

### 6.4 Consumers

The following outcomes are tested without adding per-handler address logic:

| Consumer | Forged header result |
|---|---|
| Login/auth ban | Failure is charged to observed client address |
| Recovery | Failure is charged to observed client address |
| MFA verify | Failure is charged to observed client address |
| Step-up password | Failure is charged to observed client address |
| Admin login/lockout | Failure is charged to observed client address |
| `POST /users/me/keys/reveal` | Failure is charged to observed client address |
| Media handshake ceiling | Neighboring RFC1918 peer is keyed by its socket address |

The two Security scan overlays for VULN-008 and VULN-017 must fail against the
fixed source, with retained positive tests proving correct attribution.

## 7. Rollout

1. Land Self-Hosted source and docs with no production deployment claim.
2. On every Self-Hosted deployment target, resolve and pin the service network
   under the deploy lock, start through the canonical guarded rebuild, record
   that both running endpoints select the inspected gateway without publishing
   addresses in PR logs, and run the through-nginx attribution probe before
   accepting the release.
3. Re-run after network recreation and during update/rollback drills.
4. Prepare a reviewed Hosted mirror. Measure the backend-visible control-plane
   and media-plane peers independently; do not copy Self-Hosted gateway values.
5. Deploy Hosted only after its own config, CI, and live canary prove the exact
   peer and end-client attribution.
6. Continue #80 for authenticated Cloudflare/origin provenance and #163 for
   IPv6 aggregation plus Redis failure posture.

Rollback restores the previous release and its backed-up `.env`. A rollback
may re-enable broad trust, so it is an emergency availability action and must
be followed by the same through-nginx spoof probe. Forward repair is preferred.

## 8. Security and failure analysis

| Risk | Likelihood | Mitigation |
|---|---:|---|
| Published ports use a different network from the inspected gateway | Medium | Pin both endpoints with `gw_priority: 1`; require Compose >=2.33.1; verify rendered priority before build and running endpoints before success |
| Network is recreated with a new gateway | Medium | Resolve before every `up`; persist and export current values |
| Compose accepts syntax but does not transmit gateway priority | Low with floors | Existing Engine >=29.8.2 plus Compose >=2.33.1; inspect running endpoints before reporting success; mismatch remains under-trusted |
| Bootstrap endpoint proof requires unavailable app images | Certain on fresh install | Materialize with stopped NATS, then start only internal NATS to pin the proved network; canonical rebuild remains the sole app builder/starter |
| Stopped seed is mistaken for network ownership/pinning | Certain on current Moby | Accept empty membership after materialization; require a running allowed member and stable network ID+gateway before publishing `.env` |
| Discovery recreates a serving app before guarded handoff | Medium without sequencing | Run preparation under the deploy lock; never call `create`/`up --no-start` for application services |
| Direct restart perpetuates legacy broad container env | Medium during upgrade | Pre-restart equality proof; refuse without mutation and direct the operator to the `up` migration path |
| Header overwrite missed in one location | Medium | Template census test rejects every append/pass-through form |
| Broad entry survives beside an exact one | Medium | Production validation rejects the whole list on any non-host prefix |
| Cloudflare users are bucketed by edge IP | Expected until #80 | Remove unauthenticated identity restoration; document the availability/granularity tradeoff |
| Exact trust breaks a custom second proxy | Low | The second proxy must be the backend-visible exact peer; measure it and configure its host route |
| Media gate becomes inactive from empty config | Low after fix | Production config exits before Socket.IO listens |
| Upgrade edits secrets in `.env` | Low, high impact | Atomic targeted replacement, same owner/mode, byte-preservation tests, backup before update |
| Hosted copies Self-Hosted topology | Medium | Separate mirror PR and mandatory per-target peer evidence |

No addresses, headers, or secret-bearing environment content are added to
application logs.

## 9. Test strategy

- Go config unit tests: production rejects `10.0.0.0/8`,
  `172.19.0.0/16`, IPv6 prefixes shorter than `/128`, and mixed lists;
  production accepts exact IPv4/IPv6 host routes; non-production retains CIDR
  support.
- Gin tests: exact peer honors nginx's single address; a neighboring private
  peer with forged XFF/X-Real-IP is ignored.
- Control-plane integration table: each listed auth/admin/key consumer charges
  the observed address and leaves the victim key untouched.
- Media config tests: production empty/broad/malformed lists exit; exact peers
  start.
- Media resolver/admission tests: non-nginx RFC1918 peer cannot rotate keys by
  changing either header.
- Self-host lifecycle fixtures: Compose 2.33.0 refusal/2.33.1 admission,
  rendered gateway priority, fresh stopped-NATS materialization with empty
  membership, running NATS holder and stable network ID, no recreation of a
  running app before guarded handoff, post-start endpoint proof, restart
  refusal for legacy/mismatched live config, legacy migration, network
  recreation, ambiguous/foreign network refusal, atomic rewrite failure,
  ownership/mode preservation, and update/rollback.
- Real nginx fixture: forged incoming headers are overwritten for HTTP and
  WebSocket locations; `nginx -t` passes.
- Original VULN-008 and VULN-017 overlays fail.
- Full scoped Go race tests, media Vitest/typecheck/lint, deployment fixtures,
  and repository quality gate run before PR creation.

Tests use isolated databases and Redis endpoints where handlers require them.
No test targets the shared `concord` database.

## 10. Acceptance criteria

| Criterion | Proof |
|---|---|
| No caller-supplied forwarding entry changes any listed IP-keyed control | Root-cause proxy/Gin/media tests plus consumer integration table |
| Production trusts exact peers only | Go and TypeScript negative/positive config matrices |
| nginx never appends client XFF | Template census and real-nginx echo fixture |
| Fresh self-host installs derive exact routes without requiring app images | Version-floor, rendered model, stopped-NATS materialization, running-holder continuity, post-start container inspection, and lifecycle fixtures |
| Known broad legacy installs migrate before start | Update fixture with atomic `.env` proof |
| Root `.env` writer ownership remains unambiguous | Installer fresh-publishes both schema keys; locked migrator changes only those keys; writer-census and byte-preservation fixtures |
| Unknown broad custom values are not guessed narrower | Refusal fixture and operator message |
| Media neighbor spoof is blocked | Admission test from non-nginx RFC1918 peer |
| VULN-008 and VULN-017 exploit tests fail | Security overlay results recorded on the exact PR head |
| Cloudflare identity is not accepted without authenticated provenance | Generic block absent; #80 recorded as residual owner |
| Hosted mirror is topology-qualified independently | Separate PR, exact-peer evidence, independent CI and deployment canary |
| Docs ship with code | Changelog, architecture, service env docs, and self-host runbooks updated |
| 0 new blocker/critical issues | Exact-head SAST/review evidence |
| 0 new high-confidence security hotspots | Exact-head security review and scanner evidence |
| At least 80% new-code coverage | Exact-head quality-gate evidence |

## 11. Review routing and count-bearing surfaces

Expected Phase-8 reviewers:

- `security-reviewer`, `code-reviewer`, and advisory
  `scope-reviewer`: always.
- `red-team`: `risk: security` and auth-ban consumers.
- `socket-io-reviewer`: `services/media-plane/**`.
- `deployment-config-reviewer`: control-plane config, deployment scripts, and
  Compose files.
- `pr-test-analyzer`: Go, TypeScript, and deployment test files.
- `docs-reviewer`: Markdown changes.
- `silent-failure-hunter`: Go service changes.

No migration, API route, Zustand store, CSS theme, or E2EE key-material change
is expected. Go packages and test files change, so Phase 4 must run
`./scripts/update-claude-md-counts.sh` (or record that this extracted repo has
no local count file) and review any resulting diff rather than hand-editing
counts.

## 12. Docs to update

- `CHANGELOG.md`
- `docs/architecture.md`
- `docs/runbooks/selfhost-quickstart.md`
- `docs/runbooks/selfhost-machine-output.md` if the resolver adds a named
  health/result token
- `services/control-plane/.env.example`
- `services/control-plane/README.md`
- `services/media-plane/.env.example`
- `services/media-plane/README.md`
- comments next to every Compose writer/consumer

The docs must distinguish source correctness, Self-Hosted release proof,
Hosted mirror status, and live deployment proof.
