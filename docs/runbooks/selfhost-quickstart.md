# Self-host release guidance

Self-host installation is available only through a verified published release. Consult the [public Releases index](https://github.com/Concord-Voice/Concord-Voice/releases) for published assets. If no verified self-host release is listed, installation and an install link are unavailable.

Use the operator guide retained with the selected verified immutable release tag and archive. This ordinary snapshot provides availability guidance; it does not announce a release or provide an installation procedure.

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

### WebSocket event-budget preparation

The fresh installer prepares `WS_EVENT_BUDGET_ENABLED=true` and
`WS_EVENT_BUDGET_SCALE_PERCENT=100` in its new root `.env`. These are source
defaults with **UNMEASURED** thresholds. The production Compose layer requires
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
writes `true` and `100`. Enabled accepts only lowercase `true` or `false`;
scale accepts canonical unsigned decimal 25..400, without spaces, signs, leading
zeros or fractions. Explicit empty is invalid, and scale is validated even when
disabled. An invalid nonempty value stops the control-plane at startup.
Select explicit `false` while activation prerequisites remain unmet;
verify the target release actually delivers that setting to the process. A key
in root `.env` alone does not prove container-environment delivery.

The existing selected-self-host helper clears inherited shell values for both
keys before Compose reads root `.env`. Ambient values cannot override or fill
in missing durable values on that path. Direct raw Compose retains its normal
shell precedence. Base-only development has no new required budget mapping.
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
