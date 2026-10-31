# ADR-0040: ICE-TCP ingress posture — `enableTcp` becomes a fail-closed env gate, default off

**Status:** Accepted (2026-09-04)
**Epic:** #2466 (EDGE-0) | **Issue:** #3105
**Spec:** `docs/superpowers/specs/2026-09-04-3105-ice-tcp-ingress-posture-design.md`

## Context

mediasoup's `WebRtcTransport` was configured with `enableTcp: true` on every transport
(`services/media-plane/src/config/index.ts`, prior comment: `// TCP fallback for restrictive
firewalls`). That "fallback" never existed in the deployed system: all three ingress surfaces
pass UDP only.

- `docker-compose.production.yml:551` publishes the RTC port range `/udp` only — no `/tcp`
  publish stanza exists for it.
- `infrastructure/deploy/provision-production.sh:975` opens the RTC range on `ufw` for `udp`
  only.
- `infrastructure/deploy/provision-production.sh:1030` adds the matching `DOCKER-USER`
  `-p udp --dport <range> -j RETURN` rule; the chain ends at `:1054` with
  `-i ${NET_IFACE} -j DROP`.

A TCP SYN into the RTC range therefore falls through every `RETURN` and hits that terminal
`DROP`. There is no `REJECT`, so no RST is sent back to the client — the connection attempt
times out rather than failing fast. mediasoup nonetheless advertised a TCP ICE candidate on
every transport, so every client burned a full ICE connectivity-check timeout probing a pair
that could never succeed.

**Lead with the security framing, because it is what makes this a hardening change rather than
a declined feature.** mediasoup binds a **real TCP listener per live transport today** — this is
not inert configuration. Verified in the installed mediasoup 3.26.0 worker source (block unchanged at 3.28.0, now `WebRtcTransport.cpp:157-193`):

- `worker/src/RTC/WebRtcTransport.cpp:154-187` — the `WebRtcTransport` constructor calls
  `new RTC::TcpServer(...)` per `listenInfo`, so a TCP server object is created for **every**
  transport that has `enableTcp: true` in its options, not lazily and not only for clients that
  request it.
- `worker/src/RTC/TcpServer.cpp:32-49` — each `TcpServer` instance calls
  `PortManager::BindTcp` exactly once, binding a real OS socket.

Those listeners bind `0.0.0.0` inside the container's network namespace and are unreachable
from outside **only because of the interface-scoped `DROP`** at the end of the `DOCKER-USER`
chain. That is an accident of the current bridge-networking topology, not a property of the
listener. Under #2750
(`network_mode: host`), those same per-transport TCP listeners would land directly on `ens4`,
on the provider private interface, and on any future WireGuard backplane — with `ufw` as the sole
filter. `ufw`'s RTC rule is UDP-only (same provisioner, same section), so under host networking
those listeners would be blocked **by omission**, not by decision: nobody would have chosen to
close them, the firewall rule set for the new topology just wouldn't happen to cover a protocol
nobody remembered was live. This ADR removes that listener set now, before the networking change
that would otherwise promote an accidental block into a real gap.

## Decision

`enableTcp` becomes a fail-closed environment gate, `MEDIASOUP_ENABLE_TCP`, defaulting to
`false`. The parse helper (`parseTcpEnabledEnv`, `services/media-plane/src/config/index.ts`) is:

```typescript
raw?.trim().toLowerCase() === 'true';
```

Only a value that normalizes to `true` — trimmed and case-folded — opens the capability. Unset, empty,
`'1'`, `'yes'`, `'on'`, and any other value all resolve to `false`. There is no unparseable-input
branch that fails open. When the gate is closed, mediasoup neither binds the per-transport TCP
listener nor advertises a TCP ICE candidate. A startup `WARN` fires when the gate is open, naming
the ingress obligation the process cannot itself verify — see § Residuals.

`MEDIASOUP_ENABLE_TCP` **is** declared as a literal `"false"` in each media-plane
`environment:` map (`docker-compose.yml`, `docker-compose.staging.yml`,
`docker-compose.production.yml`), and is deliberately absent from `deploy.env.example`,
`provision-secrets.yml`, and `services/control-plane/pkg/config/config.go` — the control plane
never reads it.

The distinction matters and an earlier draft of this ADR got it wrong. The `NUM_WORKERS`
precedent (`docker-compose.production.yml:519-524`) is about not *interpolating* a value from
`.env` — writing `NUM_WORKERS: "3"` rather than `${NUM_WORKERS:-3}` keeps it out of
`/opt/concord/.env` and so carries none of the five env-var provisioning surfaces. It is **not**
about omitting the declaration: `NUM_WORKERS` is very much present in the environment map.
Omitting `MEDIASOUP_ENABLE_TCP` entirely made `process.env.MEDIASOUP_ENABLE_TCP` permanently
`undefined` inside every containerised deployment, so the documented opt-in silently did
nothing, the rollback path did not work, and the startup `WARN` could never fire. A literal
declaration reaches the container while still carrying none of the five surfaces; enabling the
gate is a deliberate compose edit, not an ambient environment variable. The parity test asserts
the declaration exists wherever the code gates on it.

## Alternatives, and what eliminated each

**Open the RTC range for TCP (the naive fix).** Eliminated on **benefit**, not cost — this
overrules an earlier cost/benefit-only framing. coturn already publishes and firewalls TCP TURN
relay: `3478/tcp` and `5349/tcp` are published (`docker-compose.yml:102-105`) and opened on both
firewall surfaces (`provision-production.sh:978`, `:980`), and the desktop client has consumed
server-minted `turn:<host>:3478?transport=tcp` and `turns:<host>:5349` credentials
(`services/control-plane/pkg/config/turn.go:63-66`) since #3104 / PR #3117. A network that
permits outbound TCP to the 40000–41999 RTC range is, in the overwhelming majority of cases,
strictly more permissive than one that permits outbound TCP to port 5349. So opening the RTC
range converts a working **relay** path into a **direct** one for users who can already reach
voice — it does not convert "no voice" into "voice". Its secondary costs remain real but are
not what eliminates it: a new stateful ~2,000-port ingress with no SYN-rate limiting anywhere in
`provision-production.sh`, conntrack growth the UDP-only posture never had, nine surfaces plus
nine stale doc lines to keep in sync, and pre-payment of the exact DNAT-rule doubling that
that #2750 was filed to avoid — on the current single deploy colour, before #2269's ingress contract
exists to govern a second one.

**A narrow TCP `portRange` via `listenInfos`.** Eliminated **structurally**, not on cost. The
per-transport TCP listener is created eagerly in the `WebRtcTransport` constructor for every
transport that has `enableTcp: true` — it is not allocated lazily for TCP-needing clients. A
narrow TCP port range is therefore not headroom; it is a hard ceiling on **total concurrent
transports**. TCP holds its own port bitmap, so it does not consume UDP capacity — but that
bitmap exhausts on its own count of live transports. Exhaustion throws
`"no more available ports"` (`PortManager.cpp:355-361`) from inside the `WebRtcTransport`
constructor, so `createWebRtcTransport` rejects and **every** join fails, not just the
UDP-blocked minority's. Set the TCP range equal in width to the UDP range and this option
collapses into "open the range" above.

**A bare `enableTcp: false` literal.** Dominated by the env-gate shape at roughly ten extra
lines of cost. A hardcoded `false` would strip the capability from every deployment, including
a self-hoster who has already opened a TCP range for their own topology. The env gate preserves
that option for anyone who has actually done the ingress work, while keeping every deployment
that hasn't fail-closed by default.

## Successor: `WebRtcServer`

The durable fix is mediasoup's `WebRtcServer` API: a fixed port pair per worker rather than a
per-transport listener pair. That yields roughly 2 ports per worker instead of one pair per
transport, a DNAT-rule reduction on the order of 2,000 rules (because the UDP side collapses to
the same shape), and it permanently removes the per-transport port-exhaustion failure mode this
ADR's "narrow range" alternative ran into. Under a `WebRtcServer`, mediasoup defaults **both**
`enableUdp` and `enableTcp` to `true` (`node/lib/Router.js:196-200`). A successor must explicitly
preserve this ADR's UDP/TCP policy; the API default does not authorize adding TCP listeners or
publications. Working ICE-TCP would require its own coordinated listener, publication and
firewall changes, at roughly one TCP port per worker rather than one per transport.

**Record here, because it will bite whoever designs the successor: the published mediasoup v3
docs are wrong on this exact API family.** The public `WebRtcTransportOptions` reference states
that `enableUdp`/`enableTcp` "are only processed when a WebRtcServer is utilized." `Router.js`
lines 196–204 contradict that statement outright — the defaults it sets apply regardless.
Designing the `WebRtcServer` migration against the published docs alone would misinform the
default-value analysis; it requires reading `worker/src/RTC/WebRtcServer.cpp` end to end first.
That reading is a prerequisite for the successor design, not an implementation detail to fill in
later.

`WebRtcServer` is an ingress-contract change that changes the path serving 100% of users — a
different failure blast radius than this ADR's minority-degradation posture — and it is owned by
#2269. **This ADR files no
new issue for it.** It names #2269 as the successor's owner so the connection is discoverable
from here.

The R01 shared-listener candidate adds an
off-by-default runtime path and a private config-only last overlay. It preserves literal worker
counts and UDP-only publication policy. It is unactivated: ordinary controllers refuse shared
deployment/rollback pending the separate coordinated rollout and ingress acceptance gates.

## Residuals — named, not hidden

1. **The firewall width mismatch is recorded, not narrowed.** `ufw` and the `DOCKER-USER` chain
   both admit a wider range than the 2,000-port window the compose files actually publish. This
   ADR does not edit `provision-production.sh` to narrow that admission: the script runs only at
   host build time, and routine deploys go through `post-rsync.sh` / `concord-ctl.sh`, which never
   re-invoke it. Editing the provisioner would narrow nothing on a live host while creating a
   documented belief that it had been narrowed — worse than leaving the mismatch recorded and
   untouched. Live narrowing of the firewall surfaces to match the published range is owned by
   #2269.

2. **The metric that would reopen this decision is server-side and out of scope here.** The
   number that would justify revisiting Option A (open the range) or accelerating Option C
   (`WebRtcServer`) is the fleet's relay-vs-direct connection share — derived from mediasoup's
   `iceselectedtuplechange` event and `TransportTuple.protocol`. Capturing it needs a new
   closed-catalog operations metric key and migration 000127; that is not part of this change.
   This is named explicitly as the gate: if relay share climbs enough to matter, that is the
   trigger to revisit A or C, and nothing short of that measurement should.

3. **The startup `WARN` cannot verify a firewall.** When `MEDIASOUP_ENABLE_TCP` resolves `true`,
   `services/media-plane/src/index.ts` logs a `WARN` at startup naming the obligation to publish
   and firewall the TCP range. An operator who sets the variable without doing that work
   recreates the exact defect this ADR removes — the warning converts an invisible
   misconfiguration into a visible one in the logs, and it does nothing more than that. It is not
   a check, and it is not evidence the ports are actually reachable.

4. **coturn now carries every UDP-blocked session, and it is thinly provisioned.** coturn is
   capped at 256 MB / 1 CPU (`docker-compose.production.yml:145-149`). With the ICE-TCP fallback
   removed, every client on a UDP-blocking network relays through coturn rather than reaching a
   direct TCP pair. This is this decision's one real cost, and it is the residual that residual 2's
   metric exists to watch.

## Consequences

- Restrictive-network clients that previously would have attempted (and always failed) a direct
  TCP ICE pair now relay through TURN/TURNS (3478/tcp, 5349/tcp) without first burning an ICE
  connectivity-check timeout on a pair that could never succeed.
- No deployed environment loses any capability it was actually using: the TCP candidate was
  advertised but unreachable everywhere it was deployed.
- A self-hoster who has independently published and firewalled a TCP range for the RTC window can
  still opt in. **The opt-in is a compose edit, not a shell variable**: change
  `MEDIASOUP_ENABLE_TCP` to `"true"` in the media-plane `environment:` map of whichever compose
  file they deploy with, then recreate the container. Exporting the variable in a shell or
  writing it into `.env` does nothing — the map is a literal and does not interpolate, so the
  process would never see it. Publish the RTC range for TCP and allow it on both firewall
  surfaces **before** flipping it, or the original defect is recreated.
- Rollback is **not** symmetric with enabling, and saying it was is what made this bullet wrong.
  Setting the literal back to `"false"` rolls back the GATE only. The TCP publication and both
  firewall permits added on the way in stay behind — unused ingress, which is precisely the state
  the AC-3 parity test's disabled branch rejects, so a gate-only rollback leaves the repository's
  own blocking check failing. A full rollback SETS THE LITERAL BACK TO `"false"` — it does not remove
  it — and removes the other two: the TCP publication from the compose map, and the TCP permits
  from both firewall surfaces. **Removing the literal would be wrong** on both counts: it breaks the
  per-compose declaration invariant the AC-3 parity test asserts, and it recreates the
  unreachable-opt-in defect this ADR exists to correct, since the container reads `process.env` and
  a variable no compose file declares is permanently `undefined`. Neither direction requires
  re-provisioning the host, but the firewall permits do have to come back out.

## Addendum (2026-09-05) — the parity test's scope boundary, per #3122

Five review findings on PR #3119 sat on one boundary: the AC-3 parity test decides whether
a firewall permit *exists* and is *written correctly*, not whether it is **in force**. They
were dispositioned `deferred` and #3122
asked for a recorded decision. This is it.

**Decision: the asymmetric middle, implemented as structural refusal rather than evaluator
simulation.** The disabled branch keeps no effectiveness model at all; the enabled branch
asserts the structure the provisioner actually ships and refuses anything more complex.
Refusing on structural complexity is strictly coarser than deciding it, and coarseness costs
only false rejections. Rejected: narrowing and declaring effectiveness out of scope (it leaves
a model in the file that is wrong in the forbidden direction), and a runtime probe in a
privileged container (correct, and machinery this repo has no precedent for, in service of a
branch no deployment reaches).

**Where each finding landed.** All five are silent only in the *enabled* branch. In the
disabled branch — the one that runs today — four were already loud, because that branch is
over-inclusive about effectiveness: a permit present in the text but not in force still counts
as an opening and still fails. The exception was the interface-scoped `DROP` read as a chain
boundary, and it was silent there for one reason only: `seen_drop` was an effectiveness model
living inside an extractor whose whole job is over-inclusion. It was deleted, not improved.

**A correction to the rollback contract above.** That bullet says the disabled branch rejects
"any firewall permit overlapping the RTC window". It rejects any *recognised* one. Review
turned up three live permits the extractors silently dropped — a `ufw` comment naming the udp
twin, an `=`-attached long option, and an `-4` family selector — each reachable by an ordinary
edit, and each producing a clean pass over an open port. All three are fixed, and the class
they belong to is now bounded by a classifier: a permit yielding no port range must match one
of four provably-safe shapes or name a pinned exception, otherwise it fails loudly.

A second review round found that bound was **narrower than this ADR claimed**, and seven more
live permits greened the suite. The classifier waved every permit carrying a numeric port
through to the range extractor on the strength of a comment nothing checked — and that
extractor is a *rejecter*, so a rule it dropped for carrying `-o docker0`, `-p 6` or `--syn`
landed in neither set, which in the disabled branch reads as no opening. Recognition was also
anchored to spellings rather than commands: a `sudo` prefix, a backslash continuation, and the
`ufw route allow` form — the documented spelling for forwarded traffic — were each invisible.
The classifier now takes the *set difference* against what the extractor actually accepted, so
the bound holds for permits with a port as well as without; `ufw` classifies every verb outside
a small provably-inert allowlist; and rule languages the test cannot read are refused rather
than parsed. The honest statement of the residual is **recognition, not classification** — see
the note below on the provisioner running to completion, which is the same class of limit.

**Residual, stated rather than hidden.** The file assumes the provisioner runs to completion.
A `set -e` abort before the firewall section would leave a structurally top-level permit
unapplied. No bash text scanner can decide that soundly, and the file already rested on this
assumption before #3122; it is now written down at the check that depends on it.
