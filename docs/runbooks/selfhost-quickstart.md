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
defaults with **UNMEASURED** thresholds. This preparation adds no Compose
mapping or service-start action and does not authorize activation or announce
a budget-capable release.

Before adopting a verified release that requires these keys, existing operators
must prepare them through their established root `.env` configuration process.
The installer remains fresh-only: do not rerun it to replace an existing file.
In a budget-capable release, enabled accepts only lowercase `true` or `false`;
scale accepts canonical unsigned decimal 25..400, without spaces, signs, leading
zeros or fractions. Explicit empty is invalid, and scale is validated even when
disabled. Select explicit `false` while activation prerequisites remain unmet;
verify the target release actually delivers that setting to the process. A key
in root `.env` alone does not prove container-environment delivery.

Activation requires the target release's configuration wiring, compatible
server/Admin metrics reader and storage catalog, compatible released clients
and validated admission floors, and the reviewed workload evidence. Do not
derive a client floor from a repository package version. Record evidence for
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
