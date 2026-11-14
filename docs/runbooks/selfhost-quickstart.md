# Self-host release guidance

Self-host releases are published on the [Self-Hosted releases page](https://github.com/Concord-Voice/Self-Hosted/releases). [v0.3.0](https://github.com/Concord-Voice/Self-Hosted/releases/tag/v0.3.0) is published with its bundle, manifest and `SHA256SUMS`, and each file carries a build attestation you can check with `gh attestation verify <file> --repo Concord-Voice/Self-Hosted --signer-workflow Concord-Voice/Self-Hosted/.github/workflows/release.yml --source-ref refs/tags/v0.3.0`. It has no supported install procedure yet: fresh-host acceptance ([#74](https://github.com/Concord-Voice/Self-Hosted/issues/74)) has not passed, so installation and an install link are unavailable. The `install-selfhost.sh` inside the v0.3.0 bundle is not a supported install path.

Once a release passes fresh-host acceptance, use the operator guide retained with that release's immutable tag and archive. No release, v0.3.0 included, qualifies yet, so the "selected verified release guide" sections below do not apply to any published release. This ordinary snapshot provides availability guidance; it does not announce a release or provide an installation procedure.

## Requirements

The selected verified release guide supplies its supported host, software, capacity and network requirements.

## Firewall and Docker publish boundary

The selected verified release guide supplies the host firewall, Docker publication, provider firewall and NAT requirements for each supported address family.

### Firewall (UFW)

Use the selected verified release guide for any UFW-specific guidance together with the Docker publication boundary.

## Verify your exposure

Follow the selected verified release guide for external exposure checks. Local health checks cannot establish public ingress safety.

## Object storage (bundled or BYO S3)

The selected verified release guide defines its supported storage choices and their lifecycle limits.

## Updating

The exact target release guide supplies its verification, backup, compatibility and update procedure.

### Trusted proxy gateway provenance

For a verified release carrying this source contract, the installer owns the
root `.env` schema and fresh file publication. It writes exact loopback
placeholders for `TRUSTED_PROXY_CIDRS` and `TRUSTED_PROXIES`; the deploy-lock
`up` path then measures the Docker services bridge and atomically replaces only
those two values with its gateway's canonical IPv4 `/32` or IPv6 `/128` host route before the
application rebuild. Existing installs with the known legacy broad defaults
are migrated there. A custom broad value, ambiguous network, or unverified
gateway stops the start. Direct `restart` requires an already exact, matching
running stack and otherwise directs the operator to `concord-selfhost up`.
IPv4-mapped IPv6 trust entries (`::ffff:`) are rejected by both services.

Both application services select `concordvoice-services` as their default
gateway through Compose `gw_priority: 1`; this requires Compose 2.33.1 or
newer. `down` can remove that network, so a later `up` measures its new gateway
instead of reusing the previous route. A stopped container cannot hold a Docker
network; when needed, the locked path starts only NATS to hold the measured
bridge before updating trust. A failed live endpoint check reports failure and
leaves exact under-trust in place. `concord-selfhost health` also refuses a
running gateway or application trust mismatch; `health --json` reports the
`proxy_provenance` check with `ok: false` (see [machine-readable
output](selfhost-machine-output.md)). Verify host-nginx attribution at the edge
for the selected release. Generic Cloudflare traffic remains attributed to
the edge until authenticated origin provenance is supplied by #80.

Pre-auth source controls use an IPv4 `/32` or native IPv6 `/64` identity across
nginx, control-plane Redis budgets, Admin Console lockout, and media admission.
`TRUSTED_PROXY_CIDRS` and `TRUSTED_PROXIES` must therefore be established before
those controls can safely count a forwarded address. In production the media
plane refuses startup if it has no valid exact trusted-proxy host route. A Redis
failure on `/api/v1/auth/*` returns 503 with `Retry-After: 1` rather than
admitting an uncounted credential attempt.

### WebSocket event-budget preparation

The fresh installer prepares `WS_EVENT_BUDGET_ENABLED=false` and
`WS_EVENT_BUDGET_SCALE_PERCENT=100` in its new root `.env`, so a fresh install
does not enforce budgets. The scale is a source default with **UNMEASURED**
thresholds. The production Compose layer requires
both keys and forwards them only to control-plane; the bundled and BYO S3
overlays inherit those mappings. This configuration delivery adds no
service-start action and does not authorize activation or announce a
budget-capable release.

Existing operators must prepare both keys through their established root `.env`
configuration process before adopting a verified release with the required-value
production model. Missing or explicitly
empty values fail interpolation for any selected command that renders that
model, not just startup. Through the self-host wrapper this appears as
`selfhost-storage: model resolution refused` on every verb, including `status`,
`logs`, `stop` and `down`, until both keys are present and nonempty. The installer remains fresh-only: do not rerun it to
replace an existing file or source dotenv as shell.

The control-plane in this source tree already parses both keys at process
start and enforces account budgets when enabled is `true`. With a key absent
from its environment it falls back to enabled and scale 100. The fresh installer
writes `false` and `100`, so enforcement stays off until an operator sets
`true`. Enabled accepts only lowercase `true` or `false`;
scale accepts canonical unsigned decimal 25..400, without spaces, signs, leading
zeros or fractions. Explicit empty is invalid, and scale is validated even when
disabled. An invalid nonempty value stops the control-plane at startup.
Select explicit `false` while activation prerequisites remain unmet;
verify the target release actually delivers that setting to the process. A key
in root `.env` alone does not prove container-environment delivery.

The existing selected-self-host helper clears inherited shell values for both
keys before Compose reads root `.env`. Ambient values cannot override or fill
in missing durable values on that path. Run the wrapper directly, as
`infrastructure/deploy/concord-selfhost <verb>`, not as `bash concord-selfhost`.
Its shebang starts Bash with `-p`, so shell functions and `BASH_ENV` exported
by the calling environment cannot replace the commands that clear those values.
Direct raw Compose retains its normal shell precedence. Base-only development has no new required budget mapping.
Compose checks presence only: it forwards nonempty values unchanged, including
explicit `false` and invalid nonempty grammar. The budget-capable runtime owns
semantic validation; a successful render proves neither valid runtime settings
nor delivery to a running process. Use the reviewed target-release procedure
to apply the prepared configuration and verify the actual process environment.

Activation requires the target release's configuration wiring, compatible
server/Admin metrics reader and storage catalog, compatible released clients
and validated admission floors, supported handling of existing connections,
and the reviewed workload and security evidence. Do not derive a client floor
from a repository package version. Record evidence for
the exact candidate using the [acceptance template](selfhost-public-acceptance.md).

For a budget-capable release's disable/rollback procedure, retain both keys and
a valid scale, and verify delivery of `WS_EVENT_BUDGET_ENABLED=false`. Disabling
preserves the original ingress, content and renewal guards. Retain Redis budget
debt and the compatible Admin reader and metrics storage catalog during binary
rollback; never flush or delete budget keys to restore allowance. Follow the
verified target release's procedure for applying configuration; this snapshot
supplies no restart or deployment authorization.

## TLS

The selected verified release guide supplies certificate provisioning and client trust requirements.
