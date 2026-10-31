---
description: mediasoup/Node.js media plane conventions
paths:
  - "services/media-plane/**/*.ts"
  - "services/media-plane/Dockerfile"
---

> **Imported from Concord-Voice-Alpha (@771eb3c) at the repo split.** References to `client/desktop/**`, `docs/runbooks/deploys.md`, `docs/reference/as-built-ledger.md`, `docs/superpowers/**`, managed (hosted) deploy workflows and Alpha's org rulesets point at Alpha, Desktop or the private ops repos. Use them for context; they are not files or CI in this repo.

# Media Plane Rules

## Architecture
- mediasoup-based WebRTC SFU on Node.js 26
- Stack: mediasoup, Socket.IO 4.x, Redis, NATS (versions: `services/media-plane/package.json` — do not duplicate them here)
- Key components: `RoomManager` (authoritative), `MediasoupService` (workers/routers only)

## Logical login connection ownership and protocol 4 (#2272)

- `lib/voiceAuthConnection.ts` installs a provisional exact connection-generation
  owner before registration awaits, including idle sockets. CP derives user/family
  from verified SID authority; handshake metadata cannot choose them. Keep original
  token/raw-body commitments through response loss and provisional close. A room
  generation has a separate immutable association to its exact parent connection;
  do not invent idle rooms or reuse a successor's identity for old cleanup.
- Fence promotion, produce, resume, consume and renewal synchronously before
  terminal native/remote awaits. Retain one terminal task per exact generation,
  native creation/close promises and late completions across disconnect/retry.
  `roomManager.ts` owns room work; `mediasoup.ts` owns native worker termination.
  Socket close, map absence, worker `died` or IPC `close` alone does not prove
  physical subprocess cessation. Idle owners need a zero-resource witness.
  Retire liabilities only after native/inflight work settles and exact signed
  room/connection release (or verified provisional-close receipt) succeeds.
- Admitted renewal retains user, family, credential epoch and exact connection/room
  generation, rechecks ownership after awaits and updates expiry last. Idle media
  renewal remains unsupported; expiry applies. Exact release stays available after
  terminalization/erasure and must not reacquire live account/family authority.
  CP attachment/release locks connection before binding/room and checks fresh
  associations after locking; preserve that order in callback changes.
- `lib/voiceAuthProtocol.ts` and the required enforcement subscription strictly
  decode bounded raw UTF-8 JSON before routing. Reject duplicate/escaped-equivalent
  keys, unknown fields and malformed new variants without legacy fallback. Verify
  domain-separated proof and exact boot/generation/request binding for commands,
  health and HTTP receipts; HTTP status or ACK alone cannot erase an obligation.
  One auth command selects one connection generation, never all local family/user
  sockets. Preserve existing strict JetStream moderation semantics.
- Protocol-4 transport health and productive authority are separate. Prepared
  capability works with admission closed. SQL failure, auth/epoch work aged at
  least 30 seconds from original operation creation, or a completed security round
  stale by 30 seconds withholds productive grants. Reuse the 10-second challenge
  and 30-second monotonic lease; retries cannot reset age. Quarantine may affect
  sibling families on the boot, but transport health, cleanup, release and
  reconciliation remain alive. SQL terminal commit and the 30-second admission
  fence promise no instantaneous or hard 30-second native packet stop.
- Keep the shared 64 CP exact-request cap, 100-target pages and 1000 retained local
  connection/room/release identities. Saturation/failure retains obligations and
  original owners. Protocol 3 cannot renew productive family authority. Activation
  requires the immutable paired-artifact/boot receipt; the first family-aware
  CP/media pair remains the rollback floor even with empty queues. Permanent boot
  retirement prevents reuse; it does not prove pending delivery complete.

Read the ABI
and protocol/control/lifecycle contract
before changing proofs, receipts, ownership or health. See [API contracts](../../../docs/api/README.md),
the as-built record and
cutover/rollback procedure.
Existing Redis auth fences still apply; #2271's separate budget and stronger
Redis-outage availability are not implemented by this contract.

## Voice lifecycle publisher clock (post-#2231)

All `voice.joined`, `voice.left`, `voice.room_empty`, and `voice.heartbeat`
events use the shared `NatsService.nextVoiceLifecycleTimestamp()` helper. It
publishes a strictly increasing RFC3339 timestamp at microsecond precision
within one media-plane process: take wall-clock microseconds when they advance,
otherwise increment the last published value by one microsecond. Do not replace
it with independent `new Date().toISOString()` calls; millisecond collisions can
collapse a leave/join into one control-plane lifecycle version.

This is deliberately not a distributed logical clock. Independent media-plane
processes remain unordered. Cross-process qualification belongs to the control
plane's exact lifecycle fences, PostgreSQL advisory locks and persisted
watermarks, post-commit mutation replay, and authoritative heartbeat repair.

## Participant identity is server-authoritative (post-#2109 / CV-CAN-017)

Voice participant display identity (`username` / `displayName` / `avatarUrl`) is
**server-authoritative**, not client-supplied. The control-plane resolves it from the
authenticated `user_id` and returns it on BOTH voice join-authorize responses
(`POST /channels/:id/voice/join` `AuthorizeJoin`, and the media-plane-facing
`POST /dm/conversations/:id/voice/authorize` `AuthorizeDMVoiceForMediaPlane`). The
media-plane parses it in `validateChannelAccess` (`middleware/auth.ts` →
`authIdentityPresent` + `authUsername`/`authDisplayName`/`authAvatarUrl`), and the pure
`resolveParticipantIdentity` picks it over the client-supplied `socket.handshake.auth`
values before it is stored on the `Participant` and broadcast (`user-joined`, the
participant list in the join response, and the `voice.joined` NATS roster). A member
therefore **cannot spoof its display identity to peers** (CWE-345).

- **The JWT still carries only `user_id`.** Identity comes from the control-plane lookup
  keyed on that authenticated id — never from JWT claims, never from `socket.handshake.auth`.
- **Handshake fallback is rollout-only.** `resolveParticipantIdentity` falls back to the
  `socket.handshake.auth` values ONLY when the control-plane did not supply identity
  (a pre-CV-CAN-017 control-plane). The discriminator is `authIdentityPresent` — field
  PRESENCE, set to `true` whenever the join-authorize response carried a string `username`
  field (even an empty one), `false` only when it is absent or non-string. It is NOT the
  non-emptiness of `authUsername`: an identity-aware control-plane that returns an EMPTY
  authoritative username fails CLOSED to the (empty) authoritative identity — `username`
  defaults to `''` with no handshake `displayName`/`avatarUrl` leak — rather than being
  mistaken for pre-CV-CAN-017 and re-opened to the spoofable handshake values. The
  discriminator reads the server-to-server response, which a patched in-room client cannot
  influence, so the fallback is **not attacker-triggerable**; it re-opens display spoofing
  only in a mixed-version window (new media-plane / old control-plane). In the single-stack
  deploy model (control-plane + media-plane rebuilt together) that window is effectively nil
  and drains once the control-plane rollout completes.
- `socket.handshake.auth` still carries the display fields onto `socket.data` at the
  connection layer (they seed the fallback and the pre-join connection log), but they are
  no longer trusted for the participant record or any peer/roster sink. `room_kind` remains
  a client-supplied routing hint that only selects which control-plane endpoint is consulted
  and grants no privilege.

### The session id must be enforcement-addressable (post-#3362)

`createAuthMiddleware` REFUSES a `user_id` claim that `isCanonicalEnforcementUUID` rejects,
before `socket.data.userId` is ever assigned. A participant is keyed in the room by that exact
string (`participants.set(userId, ...)`), while the mid-session enforcement rail refuses to act
on any id that same predicate rejects (`enforcePermissions.ts`). Admit a spelling that
predicate rejects and the participant is seated under a key **enforcement can never match** —
no kick, no mute and no permission revocation reaches them for the rest of the session. That is
an enforcement bypass, not a cosmetic mismatch.

**Refuse, do not normalize** — deliberately the opposite of the control plane, which
canonicalizes at `AuthRequired`. That is not this service's choke point: the media plane
verifies the same token itself and never passes through `AuthRequired`, so the invariant has to
be re-established here. Normalizing would need a UUID parser this service does not depend on,
and hand-rolling one in an auth path to re-derive what PostgreSQL and `google/uuid` already
agree on is how a subtle acceptance difference gets introduced. Reusing the **enforcement
rail's own predicate** is what makes the invariant provable rather than asserted: admission and
enforcement are one function, so they cannot drift apart. Do NOT replace it with a local regex,
a hand-rolled parser, or a new `uuid` dependency — a second copy of this rule is the defect.

Accepted asymmetry: a token the control plane would admit by normalizing is refused here. No
mint path emits such a spelling, so producing one requires the signing secret.

The refusal carries its **own** error message, not the `missing user_id` one directly above it.
Sharing that string let the pre-existing missing-claim test pass off this branch once the
presence check was deleted — proven by mutation, and the reason
`.claude/rules/domains/tests.md` § Vacuity calls a shared observable a shadowed assertion.

## Channel admission convergence (post-#3140)

Channel admission is also two-phase: A1 stores an exact-socket candidate only in
`Room.pendingChannelParticipants`; A2 reauthorizes after registration and before
`socket.join` or acknowledgement. A candidate MUST NOT replace an admitted same-user
session, seed the room crypto version, consume the participant cap, increment `e2eeEpoch`,
or emit lifecycle events. One synchronous promotion applies A2-refreshed identity,
entitlements, permissions, moderation state, and owner tier; checks capacity and crypto
compatibility; commits Socket.IO membership once; then replaces the admitted session and
publishes lifecycle state. Denial, credential-epoch ejection, adapter failure, or disconnect
removes only the exact pending socket, so a stale A1 candidate cannot evict a newer valid
session.

Channel A1 also commits an exact 30-second `voice_pending_admissions` reservation
for `(channel_id, user_id, admission_id, socket_id)` under authority locks before
responding. A2 reacquires those locks, rechecks authority, validates that exact
reservation, allocates a positive global authorization revision, and sends a
signed read-only prepare for the staged candidate. Prepare never mutates
`RoomManager`, sockets, epochs, or lifecycle state; the local media join performs
the sole promotion. `voice.joined` removes the exact reservation, and expired
reservations are swept independently from participant cleanup. `KeyedJoinFence`
serializes each room/user's A1→A2/promotion-or-rollback path; distributed room
ownership would require owner routing or distributed fencing.

## DM admission and moderation convergence (post-#2231)

DM admission is two-phase: authorize once (A1), register an exact-socket candidate, then
authorize again (A2) with the exact `call_id` returned by A1 before `socket.join` or
acknowledgement. A1 candidates live only in `Room.pendingDMParticipants`; they MUST NOT enter
the authoritative `Room.participants` map, set `callId` / `callStartedAt`, write participant
history, increment `e2eeEpoch`, emit `user-joined` / `user-left` / `room-empty`, make a room
active, appear in heartbeats, or affect DM tier caps and codec-floor calculations. An empty DM
room's media-frame crypto version is likewise seeded only by promotion, not by A1.

A2 must remain allowed and return the same non-empty call ID as A1. Apply its refreshed
server-authoritative identity, media entitlements, and `server_muted` / `server_deafened`
state to the candidate, perform the final `socket.connected` check, then enter one synchronous
promotion transaction for the exact `{roomId, userId, socketId, callId}` candidate with no
intervening `await`. After every admission check but before authoritative room mutation, that
transaction invokes the default in-memory Socket.IO adapter's synchronous `socket.join`; an
adapter failure therefore leaves the candidate provisional and silent. Promotion is the only
DM admission point: after the socket join succeeds it locks call metadata, seeds crypto version
when needed, records history, increments the epoch once, and emits one `user-joined`. A
same-user reconnect leaves the old admitted session authoritative until this exact promotion,
then replaces it without a transient leave, terminal event, or extra epoch. The post-promotion
Socket.IO `user-joined` payload carries authoritative optional `isDeafened` and `isTesting`
values; desktop merges only booleans that are present so legacy payloads and consume-race media
state remain intact. Idempotent replay for the already-promoted socket/call returns the current
snapshot without repeating lifecycle.

Denial, lease rotation, or disconnect rolls back only the exact pending socket. A fresh
provisional-only room closes locally without terminal lifecycle; an already-admitted call
continues through the normal terminal path. If cleanup defers to a queued same-room admission,
`KeyedJoinFence` retains the first deferred exact A1 call ID as a fallback when the last queued
cleanup has no current call ID, then clears it when the queue drains; it never promotes that ID
into authoritative `Room.callId` before A2. The durable `dm_voice_participants` row is still
committed later from `voice.joined`; this media-plane change adds no Redis commands. A committed
membership removal also publishes an unconditional disconnect. Its media-plane handler resolves
both the admitted socket and any exact provisional DM socket for the user, silently removes the
exact candidate first, then disconnects both, so an already-issued A2 response cannot promote it
and synchronous admitted cleanup can still terminalize a history-bearing call. If the candidate
was already the room's only live state, the enforcement-specific removal closes a never-admitted
room silently but emits normal terminal lifecycle when admitted history exists.
Successful DM heartbeats retry disconnects for media-reported users who are no longer
authoritative members; later hard-mute/deafen changes remain active-row-gated.

### Durable exact-session enforcement (post-#3140)

Before A2 promotion, the media plane registers an immutable exact-session record
with the control plane: `session_generation`, process-local `node_boot_id`, room
kind/ID, authenticated user, credential epoch, and Socket.IO ID. Registration is
service-hop authenticated and body-bound, and the control plane repeats the
credential and DM membership/block fences inside the registration transaction.
The local participant carries the same session generation from A1 through
promotion, so a stale target cannot evict a successor that reused a Socket.IO ID.

Credential-epoch and DM-block ejections are published to the exact
`voice.enforce.session.<node_boot_id>` subject with a signed immutable identity.
The target verifies every field, performs terminal RoomManager teardown before
`socket.disconnect()`, then performs an authenticated exact release and sends a
signed acknowledgement. A transient release failure after terminal local
teardown is retained in the media process's bounded exact-session retry queue
and retried after verified health recovery. Control-plane parent outboxes are
deleted only after a fresh registry scan proves that no matching session
generation remains; a generic or non-owner reply is never completion. Rows
have no TTL or lease expiry: a crash or restart can still leave an operator-
visible pending obligation until node/container recovery is human-verified.

The media process also has an expired-by-default authority lease. Every 10
seconds it sends a body-bound, HMAC-authenticated bootstrap to
`POST /api/v1/internal/voice/enforcement/health` (maximum body: 4 KiB). The
control plane publishes a challenge to the same exact
`voice.enforce.session.<node_boot_id>` NATS request subscriber; only a verified
challenge receipt renews the monotonic 30-second lease. Expiry closes local
rooms and disconnects sockets, but never releases registry rows or settles a
parent obligation on timer inference. `/readyz` reports protocol 3 only while
the lease, exact subscriber, durable command consumer and command owner are healthy.

### Durable voice command consumer (#2551 / #2624)

`VOICE_COMMANDS_V1` owns the exact `voice.commands.v1` subject. Its fixed contract
is file storage, one replica, Limits retention, DiscardNew, MaxAge zero,
MaxBytes 67108864, MaxMsgs 50000, MaxMsgSize 4096 and duplicate window 120 seconds.
Never substitute work-queue retention, TTL, stream purge or DiscardOld. One
stable pull durable, `voice-enforcement-v1`, uses explicit ack, DeliverAll on
initial creation, MaxAckPending one, AckWait 30 seconds and unlimited deliveries.
Keep only one outstanding pull request, with a two-second next deadline.

Both stream configurations keep default synchronous persistence, no compression,
placement, template owner or initial sequence offset, and disabled counter,
scheduled, atomic and batched publishing. Direct reads are enabled only on the
owner backing stream; TTL capabilities, delete-marker TTL, mirror-direct reads
and inherited consumer limits remain disabled. Refuse configuration drift
without repair. Go verifies modern SDK fields against the same resource birth
as its classic stream information before ownership mutation.

Real JetStream tests use the shared test-only NATS resolver: existing binary
override, executable PATH, then the official version/digest-pinned 2.15.1 image
through a uniquely owned stopped local Docker container. Keep a private locked
cache, atomic executable publication and bounded cleanup; each broker owns its
process, loopback port and storage. Never use workspace-specific tool paths or
silently skip these tests. Keep the protected required workflow's reviewed pin.

Keep the pinned Semgrep engine able to parse changed source and tests. Use
static type imports or typed locals when equivalent inline syntax is rejected,
and check scanner errors in both custom and community scans without relaxing
their enforcement.

The permanent file-backed KV bucket `VOICE_COMMAND_OWNER_V1` has one replica,
history one, no TTL and key `authoritative`. Migration 000167's immutable
`voice_command_transport` receipt binds generation, exact command/KV stream
Created identities and the original initializing revision. Created fingerprints
are canonical UTC RFC3339Nano text, not SQL-rounded event timestamps. Initial
provisioning performs NATS work outside SQL transactions, commits the receipt,
rereads its confirmed identity after ambiguous/concurrent commits, then releases
only the exact initializing revision in that committed receipt. Do not adopt a
recreated key's newer revision or rewrite a receipt to fit new resources.

Media startup never creates ownership resources. It accepts only a proof that
matches both current broker resource births. Every owner-store read and CAS
checks births, and CAS verifies them again after its write. Preserve the acquired
proof through renewals and recovery. Missing/changed stream, bucket or key after
provisioning is continuity loss, never permission to initialize an empty
replacement. Ordinary snapshot restore can change Created despite preserving
messages and revisions; it does not restore the frozen authority proof.

Only `released -> held` CAS admits a boot. A held foreign boot refuses regardless
of age. Renew serially every two seconds, bound broker calls to two seconds, and
measure five-second local validity from request start with a monotonic clock.
Do not introduce expiry-based takeover: a paused predecessor's workers may still
forward media. Admission, readiness, pull, effect boundaries and ack/term all
require current owner and consumer continuity. On uncertainty, stop these paths,
expire `VoiceEnforcementLease`, and use `VoiceEnforcementExpiryFence` to
terminalize/quarantine exact sessions. Same-boot recovery requires completed
teardown and its original proof; it cannot acquire another boot's held record.

Preserve `voice-enforcement-v1` on shutdown. Retain/await the one in-flight apply,
confirm native termination and complete exact registry releases, then close all
captured mediasoup workers and await actual subprocess closure BEFORE CAS
release. Use one absolute monotonic deadline for the remaining existing
35-second teardown budget, leaving time for the two-second release CAS; retain
the final ten-second HTTP-close reserve. Keep NATS and
health/request-reply/release retry services available through registry release
and worker exit proof, then close inter-service connections. Incomplete native
teardown or subprocess closure retains held ownership. Recreating a missing
durable requires local teardown and verified resource continuity first, then
DeliverAll replay of retained commands.

`control-plane voice-command-owner recover --expected-revision N
--expected-boot-id UUID --confirm-predecessor-stopped` is the bounded operator
recovery command. It changes only the exact held record to released, preserving
proof, after physical evidence establishes that the old container AND all its
mediasoup workers are stopped and cannot restart. The command has a ten-second
overall deadline. Missing resources or changed births refuse this command and
need complete operator generation reconciliation; no automatic proof rebinding
or empty-stream reset is supported. See the
transport runbook.

The strict shared envelope uses schema `concord.voice-enforcement`, version one,
canonical generation/command/session/boot UUIDs and decimal-string positive
BIGINT sequence/revision values. Parse with `BigInt`, never `Number`; enforce the
9223372036854775807 bound. Reject unknown/mixed fields and invalid kind/action
pairs. Match exact admitted/provisional generation, boot, socket and credential
epoch before effects; never resolve a later same-user session across an await.
A valid null-target saved snapshot or absent-member disconnect marker has no
local effect. Shared codec fixtures remain at
`services/control-plane/internal/voicecommand/testdata/envelopes.json` and both
service suites consume them.

`RoomManager.applyExactServerEnforcementSnapshot` separates desired revision and
flags from effect-completed revision. Restrictive desired flags synchronously
gate produce/resume. Equal desired revisions retry unfinished pauses; completion
advances only after all required audio pauses succeed and the same participant
and revision still own the result. Legacy complete snapshots use the same retry
boundary. Unmute/undeafen never automatically resumes media against self choices.

Ack with broker confirmation only after authoritative effects and server fanout
complete. Track the active command's mutation/fanout completion separately so a
fanout retry cannot replay a transient action after a later user resume. A
versioned core copy can join the active same-command promise or skip a completed
sequence; it cannot apply a future sequence ahead of durable delivery. Reserved
new-envelope fields never fall back to legacy decoding. Existing legacy full
snapshots already carry `version: 1`: version alone does not identify the new
schema. Preserve the legacy decoder for those messages.

An apply deadline is ten seconds, with inProgress at five seconds while healthy.
A timeout invalidates fetch/ack, quarantines media and retains the unresolved
promise; never run the successor concurrently. A transient failure stays
unacked. A malformed envelope emits only fixed `media_schema_rejected`
classification, then terms only after any required conservative teardown
succeeds. Never log payloads, identifiers or raw underlying errors.

With `ROOM_PLACEMENT_ENABLED=false`, keep `/readyz` bodies exact: 200
`{ready:true,voiceEnforcementProtocol:3}` or 503 `{ready:false}`. Extend the
voice-enforcement conditions with owner/consumer continuity without adding JSON
fields. With placement enabled, ADR-0042 D5 adds `colour`, `generation`,
`accepting_new_rooms`, `rooms`, and `creates_in_flight` to both bodies so the
cutover and reaper can verify the selected instance. Fixed
aggregate command counters may appear on local health with no ID dimensions;
the closed operations-metrics catalog and Admin API remain unchanged.

The #2623 transactional publisher sends accepted commands through the durable
subject. Keep all compatibility handlers; removing them requires a separate
reviewed change and is not necessary for durable delivery.

### Required subscription availability (#2611)

Initial NATS failure propagates. Before HTTP listen, register every core and
request/reply enforcement subscriber as required, confirm the complete nonempty
set with a bounded broker flush and live-handle checks, then initialize the
required durable consumer. No required subscription silently no-ops. The current
required set is compatibility mute/deafen/disconnect and user mute/deafen,
permission updates, disconnect/credential/exact-session acknowledgements, and
admission activation. Operations publishing is optional, not an admission gate.

Required handle closure invalidates availability independently of its handler.
Connection/registration epochs reject stale confirmation. NATS restores active
subscriptions on reconnect; reinstall only ended handles before confirming the
whole set. Use one recovery timer/attempt and a two-second confirmation deadline;
an unresolved raw flush must not accumulate on repeated retries.

Readiness, new Engine.IO/namespace connections and room admission fail immediately
when the required set is unavailable. The approved maximum existing-socket grace
is 30 seconds, measured from the first loss with a monotonic deadline. Repeated
loss does not extend it. Recovery may cancel only after complete confirmation;
late callbacks cannot bypass an elapsed deadline. Preserve #2624's earlier
immediate quarantine on durable-authority loss.

At the deadline, force-close every remaining socket even if native proof is
still pending. Capture/hold exact registry releases before clearing local release
references and closing transports; socket closure never proves native completion
or permits owner release. Recovery awaits successful terminalization before
renewing authority. Stop recovery timers/new installs at shutdown but retain live
exact request/reply handlers through registry release, native exit and owner
release. Logs contain fixed classifications, never NATS subjects or raw errors.

### Native enforcement termination proof (#2624)

Pinned mediasoup `Router.close`, transport close and `Worker.close` can return
before native forwarding stops. `.closed`, cleared maps, observer close and
`Worker.died` are NOT terminal completion evidence. `MediasoupService` retains
router-to-worker ownership independently of the removable room cache.
`captureNativeTermination` synchronously captures exact worker/router references
and all target transport IDs before any await or first close. An undefined
transport list requires whole-router absence; an empty list means the exact
target had no media resources.

`confirmNativeTermination` accepts a successful captured `worker.dump()` whose
`routerIds` omit the exact router, or a successful captured `router.dump()` whose
`transportIds` omit every captured transport. Verify dump identity. The pinned
Router dump remains usable after JavaScript router close while the worker IPC
channel is open. A dump rejection during concurrent router removal requires a
fresh successful worker dump explicitly proving absence. Arbitrary rejection,
timeout, IPC loss, nonempty targets or mismatched identity is unconfirmed.
Observed `subprocessClosed` / `subprocessclose` also proves absence.

`RoomManager` registers pending native-close targets before local cleanup clears
participant/resource/router maps, including leave, replacement and failed joins.
Retain them by exact room/user/socket/session identity until proof succeeds.
`awaitNativeTermination` gates helpers claiming terminal absence, durable
target-absent dispatch, signed exact-session ejection and ordinary/exact registry
release. Exact durable/signed callers pass captured `sessionGeneration` into
`leaveRoomIfSocketOwned`, including provisional targets, so same-socket successors
cannot satisfy or be removed by predecessor cleanup. A legacy room/user absence
may await retained closures but must never tear down a successor.

`awaitNativeTerminationForCredentialEpoch` awaits all retained proofs for the
superseded user/epoch before the signed epoch success reply. Retired proof records
are not live candidates for successor teardown. Every direct registry release
requires a `NativeSessionVerifier` and awaits it before authenticated HTTP;
`VoiceEnforcementSessionReleaseQueue` receives that verifier as its required
first argument and the optional saturation callback second. After-close release
requires the bound queue. No default verifier may infer completion from absent
local state. Exact signed success also requires confirmed registry release.

Register `setNativeTerminationFailureHandler` to synchronously invalidate owner
authority and expire admission lease; start the quarantine barrier without
awaiting recursive teardown. For quarantine, poison termination and recovery,
`RoomManager.closeAll` awaits `confirmAllRoutersClosed`: every live worker dump
must prove `routerIds` empty, not just cached routers. Any in-flight router
creation or untracked native router blocks recovery. Missing/closed IPC is not
absence unless actual subprocess closure was observed. Incomplete proof leaves
commands unacked/unterminated and the permanent owner held.

At worker creation, retain subprocess-close promises; resolve them only from
`subprocessClosed` / `subprocessclose`. `MediasoupService.close(deadlineMs)`
registers every remaining waiter before `Worker.close`, retains captured workers,
and waits for all actual subprocess closures under the shared remaining shutdown
deadline. Clear ownership records only after proof; do not add a timeout release
fallback, new grace period or environment variable.

## Socket.IO
- Every client-invokable Socket.IO event is registered through `withRateLimit(socket, event, handler)` (`services/media-plane/src/lib/rateLimit.ts`, #2032) — the wrapper IS the registration (it calls `socket.on` internally), so a handler cannot exist without a declared budget. Budgets live in `EVENT_BUDGETS`, an exhaustive `Record<RoomEventName, RateBudget>` (`disconnect` is a lifecycle event and is excluded), so adding a union member without a budget is a compile error; a type-erased lookup miss degrades to the RESTRICTIVE `FALLBACK_BUDGET`, never to unlimited. On overflow the wrapper acks `{ error: 'rate_limited' }` — or emits `error` when the call supplied no ack — and **never invokes the handler**, so nothing partial is created. Token buckets live on `socket.data` and are freed with the socket; refill is lazy (computed on read), so there is no registry to prune and no timer to clear. Do NOT move the buckets to a module-level map, and do NOT register a handler with a bare `socket.on` — `tests/rateLimitRegistry.test.ts` fails on both. Budgets bound how FAST a socket may act; they do not bound accumulation — the per-participant structural caps below do that, and the two compose.
- Attestation relay (#4206): the handshake's `attestation_token` / `machine_id` are relayed as `X-Attestation-Token` / `X-Machine-Id` ONLY on hops that carry the user's bearer (`attestationRelayHeaders` in `middleware/auth.ts`; `voiceAuthConnection.request` adds them only `if (token)`), never on a service-signed hop. `X-Session-ID` is NEVER taken from the client: `bearerAttestationHeaders` sets it from the verified `sid` of the bearer sent on that hop, so a client cannot point the lookup at another of its sessions. Values are UUID-only. `refresh-attestation` is a budgeted, no-ack event that replaces `handshake.auth.attestation_token` (`null` deletes it); it never touches an identity field. The control plane, not the media plane, decides whether the triple belongs to the bearer (CV-CAN-013).
- Per-participant structural bounds (#2032) are a different axis from the #1542 per-room tier caps, which cannot express them (a 1000-participant room legitimately carries 1000 live mics). At most `MAX_RECV_TRANSPORTS_PER_PARTICIPANT` (4) live receive transports (VULN-009), and at most one live `mic`, one live `screen-audio`, and one live `screen` producer (`PARTICIPANT_PRODUCER_LIMITS`). On the send side, a participant holds at most one live send transport: a second `create-transport(send)` is refused while the first is open (D1, #2153) rather than replacing it in place. `camera` is REPLACE-IN-PLACE: a second camera produce supersedes the participant's older cameras once the replacement is fully registered (`supersedeOlderCameraProducers`), so the steady state is one camera per participant. `MAX_PARTICIPANT_CAMERA_PRODUCERS` (2) remains as the backstop that bounds the transient device-switch overlap and holds the ceiling under a concurrent burst, where several produces clear the synchronous reservation gate before any reaches the post-await eviction. A per-participant camera bound was deliberately absent at first, on the reasoning that the #1542 per-room cap already bounded it; an adversarial pass disproved that — the per-room cap is a SHARED resource counted with no per-owner term, so one participant could take every free-tier camera slot and deny camera to the rest of the room. The eviction routes through `closeProducer`, never a bare `producer.close()`, so it emits `producer-removed` and the room is told the superseded track is gone. It is invoked by the `produce` HANDLER in `index.ts`, not from inside `RoomManager.produce` — and that split is load-bearing, not incidental. `closeProducer` puts `producer-closed` on the wire immediately, while the replacement reaches the room as `new-producer` only after `produce()` returns (nothing subscribes to the internal `producer-added` event), so evicting inside `produce` sends the removal BEFORE the addition and every remote observes the publisher at zero cameras for a round trip. The announcement cannot move earlier instead: the handler auto-pauses audio for server-muted participants between `produce()` and the announcement, and announcing first would let a remote briefly consume a server-muted user. `tests/producerSupersession.test.ts` scans the real source to enforce both halves — every `roomManager.produce(` call site is paired with a supersede call, and the supersede call sits after the `new-producer` emit. Note this forecloses a genuine multi-camera client — supporting one means changing the eviction rule, not raising the number. `consume` is idempotent per `(participant, producerId, recvTransportId)` (VULN-010); the three-part key is required because a PiP window legitimately consumes one producer on a *second* recv transport of the same socket, and its transport component must come from the RESOLVED transport, never the caller's optional `transportId`. `RoomManager` owns every one of these checks: the committed count is always DERIVED from the authoritative map, so only the one-await-wide pending delta (`pendingRecvTransports`, `pendingProducerSources`, `pendingConsumeKeys`) has a release path, and check-and-reserve is synchronous — the #1539 TOCTOU discipline. The recv-transport check prunes already-closed transports in the same pass: an ICE/DTLS teardown emits no `routerclose`, so its map entry would otherwise hold the cap for the rest of the session.
- Connection admission (#2032) bounds what per-socket budgets structurally cannot — `socket.data` exists only AFTER a socket is established, so buckets keyed there cannot limit establishment. `allowRequest` (`lib/admissionGate.ts`) caps handshakes per source IP at `HANDSHAKE_BURST` (60) per `HANDSHAKE_WINDOW_MS` (10 s) over alternating fixed windows, in a store bounded by `MAX_TRACKED_IPS` (10 000) with oldest-insertion eviction. The client IP is resolved from the trusted proxy's `X-Real-IP` (falling back to the LAST `X-Forwarded-For` hop) and only when the immediate peer matches `TRUSTED_PROXIES` — nginx fronts this service, so `remoteAddress` is the proxy's address for every client, and an untrusted peer presenting the header is spoofing. It fails OPEN when no address can be resolved, and the gate is INERT when no trusted-proxy entry parses: with nothing trusted every client attributes to the nginx peer, and one shared bucket would deny voice deployment-wide on the first reconnect storm. `maxHttpBufferSize` is set explicitly to 256 KB (exceeding it CLOSES THE SESSION rather than rejecting one message, so it is sized ~58x over the largest inbound frame measured against production on 2026-08-19 — `update-rtp-capabilities` at 4,505 B; note `join-room` carries NO `rtpCapabilities`, it sends `undefined` and the client emits them after `device.load`, so join-room itself measures 94 B), `connectTimeout` is 10 s (the window a client holds a socket with zero authentication), and `perMessageDeflate` is left at its `false` default DELIBERATELY — compression on attacker-controlled inbound payloads is a decompression-amplification vector. Do not enable it for "performance".
- Validate all incoming event data types and structure
- Events should be idempotent (e.g., join room when already in room = no-op)
- Consider abuse scenarios for new event handlers
- `close-recv-transport` accepts only a non-empty `{ transportId }`; room/user identity comes exclusively from authenticated socket state. Resolve the ID only in that participant's `recvTransports` map, never through the send transport or another participant. Owned closes delete the map entry before `close()`; retries, unknown IDs, foreign IDs, and send-transport IDs receive the same idempotent success acknowledgement so the event cannot enumerate resources. Mediasoup's existing consumer `transportclose` handlers remain authoritative for consumer, layer-demand, and last-N cleanup.
- `join-room.mediaFrameCryptoVersion` is the media E2EE frame-format admission boundary. Reject missing or unaccepted values before authoritative participant storage, reconnect cleanup, or `e2eeEpoch` mutation. This is a compatibility gate for AES-256 media frames, not proof that a modified client honestly encrypted. **v5 is current:** AV1 retains per-OBU payload encryption, H.264 uses Annex-B/NAL-aware encrypted-suffix framing, and VP9/VP8/Opus use whole-frame v5 (detail in `.claude/rules/e2ee.md` § Media frame crypto v5). The accepted rollout set is exactly `{3, 4, 5}`. An empty channel room seeds from its first accepted joiner; an empty DM room seeds only when an A2-approved candidate is promoted. Every later authoritative join must equal that occupied room's version. Either mismatch direction throws the same typed `CryptoVersionMismatchError` (`code: 'crypto_version_mismatch'`, `{roomVersion, joinVersion}`), which `index.ts` returns as an actionable typed acknowledgement for the client's direction-neutral compatibility banner. Never ratchet an occupied room to a joining client's version: doing so leaves existing peers on mutually undecryptable formats. The active room's version changes only after the room empties/closes and is seeded again.
- `request-keyframe` is receiver-triggered video recovery after E2EE decrypt catch-up. Payload is `{ senderUserId }`; the handler validates the requester is in the room, the sender is in the room, and only calls `consumer.requestKeyFrame()` on requester-side video consumers attached to that sender's active `camera` / `screen` producers. It is idempotent and cooldown-limited **per (requester, sender) pair** (`KEYFRAME_REQUEST_COOLDOWN_MS`, currently 5s), the cooldown map living on the REQUESTER's `Participant` (keyed by `senderUserId`) so cleanup rides the participant lifecycle. It was previously one room-level map keyed by the sender alone, which gave every viewer of a sender a single shared 5s slot: one viewer could hold it and deny keyframe recovery to all the others (#2032 / VULN-011). Do NOT fix that by re-keying the room-level map instead — its prune is `delete(participant.userId)`, and a pair key would silently turn that prune into a no-op and leak the map. This semantic cooldown composes with — and is tighter than — the general per-socket `request-keyframe` budget.
- `update-test-status` (#1163 device-testing indicator) is rate-limited per participant with a sliding window (`TESTING_STATUS_WINDOW_MS` / `TESTING_STATUS_MAX_CHANGES` in `setTestingStatus.ts`, #2030): only STATE-CHANGING calls count (same-state calls are an idempotent no-op), and beyond the in-window budget the handler acks `rate_limited` without mutating state or broadcasting `participant-testing-changed`. This is the #1163 spec §7.5 mitigation, composed ABOVE the general per-socket limiter (#2032): the coarse `update-test-status` budget is a ceiling on arrival rate, this window is the authoritative semantic limit. Limiter timestamps live on the `Participant` so cleanup rides the participant lifecycle — do NOT move them to a module-level map.
- `set-preferred-layers` is receiver-triggered layer demand for owned camera or screen video consumers. Payload must be parsed and bounded at the Socket.IO boundary: non-empty owned `consumerId`, layer values 0–2, boolean visibility/focus/pressure flags, finite non-negative CSS dimensions, finite positive `devicePixelRatio`, and role in `thumbnail|grid|focus`. `RoomManager` must then verify ownership, video kind, and server-set `appData.source`. Camera demand is entitlement-clamped and stored for the room camera gate; non-SVC screen demand is stored for the per-sharer screen gate; SVC screen demand is validated and applied immediately but never stored as gate state. Call `consumer.setPreferredLayers()` only when mediasoup reports `consumer.type` as `simulcast` or `svc`.
- `resume-consumer` refuses to resume an **audio** consumer while the participant is `serverDeafened` (moderation deafen, #2162). `serverDeafenUser` pauses all audio consumers; without this audio-only guard a client `resume-consumer` — e.g. the per-stream screenshare **unmute** — would re-forward audio and bypass moderation. **Video** consumers still resume (deafen silences incoming audio, it does not blind the viewer). Mirrors the `lastNPausedConsumers` resume-guard shape and is regression-locked by the "moderation (serverDeafened) guard (#2162)" suite in `roomManager.test.ts`. Per-stream screenshare **mute reuses the existing `pause-consumer`/`resume-consumer` primitives** (no new socket event); the desktop renderer's `ConsumerPauseCoordinator` `'stream-mute'` reason drives the server pause and **re-applies** the viewer's intent whenever a `screen-audio` consumer is recreated (reconnect / stream restart), keyed on the sharer's userId. `screen-audio` consumers are never in `lastNPausedConsumers`, so per-stream mute never fights the audio last-N cap.

- **Media-rate policer wire additions (#2153), all additive — no IPC contract change.**
  `producer-paused` / `producer-resumed` gain `kind: 'audio'|'video'` and `source` at **all three**
  existing emit sites; the policer's own pause emits to peers only (`except(ownerSocketId)`).
  `media-policy-notice` (new, server → owner socket only) is `{producerId, kind, source,
  action:'paused'}` with a closed `action` enum — the renderer rejects unknown values. `force-disconnect`
  gains an optional `retryAfterSec`, set only when `reason === 'media_policy'`, computed as
  `ceil((cooldownUntil - now) / 1000)`. Three acks change: `join-room` and `produce` return
  `{error:'Media policy cooldown', code:'media_policy_cooldown', retryAfterSec}` while the caller is
  in cooldown, and `resume-producer` returns `{error:'media_policy_paused', message:'Paused by media
  policy'}` while the target producer is latched — this last ack is not authoritative for the UI
  (F6: the client's own `resume-producer` emit carries no ack callback today), so the client-side
  lock is the actual enforcement-visible mechanism; the ack exists for callers that do listen.

## Socket.IO Origin Gate Parity (post-#830, codified by #894)

The Socket.IO `cors.origin` callback lives at `services/media-plane/src/lib/originGate.ts` (extracted from `index.ts` for testability) and MUST maintain semantic parity with the control-plane CORS middleware at `services/control-plane/internal/middleware/cors.go:17`:

- `!origin` (no Origin header — curl, native test harnesses) → ALLOW. Browser clients always carry an Origin.
- `'null'` (sandboxed iframes, `data:` URLs) → REJECT. Never reflect to a credentialed Socket.IO connection. The reject is case-insensitive and whitespace-tolerant (`'NULL'`, `' null '`, etc. all reject) to defend against non-browser-client casing variants.
- `'file://'` (legacy Electron pre-#830) → REJECT. After #830 the bundled desktop renderer runs at `app://concord`, which must be in `config.allowedOrigins` — NOT exempt-allowed at the gate.
- Origin in `config.allowedOrigins` (or wildcard `'*'`) → ALLOW. Exact-match semantics, mirroring `cors.go:32`.
- `spa-cache://concord` (signed LKG SPA cache renderer, #1880) → ALLOW *only when in `config.allowedOrigins`*, exactly like `app://concord`. The cached renderer connects to media-plane for voice with `Origin: spa-cache://concord`, so it is allowlisted in the media-plane production `ALLOWED_ORIGINS` literal (`docker-compose.production.yml`) alongside `app://concord` and the Pages origin — added by #1889 before cache activation. NOT exempt-allowed at the gate.
- Everything else → REJECT.

**Production guard (codified by #894 reconcile):** the config loader at `services/media-plane/src/config/index.ts` fatal-exits at startup if `ALLOWED_ORIGINS` contains `'*'` while `NODE_ENV=production`. Socket.IO is configured with `credentials: true`; a wildcard credentialed origin is CWE-942 cross-origin hijack. The runtime gate keeps the `'*'` branch for parity (dev/staging operators may legitimately set it); the prod guard is the foot-gun-prevention layer.

**Worker-count guard (#2178):** the same config loader fatal-exits when `NUM_WORKERS` is not an integer in `[1, MAX_MEDIASOUP_WORKERS]` (`parseWorkerCountEnv`; the ceiling is `32`). Both bounds are load-bearing and they fail differently. A zero or unparseable count builds an EMPTY worker pool, `init()` still resolves, `/health` answers 200 — and then worker placement throws a typed `NoWorkersAvailableError` (`lib/workerSelection.ts`, #3149) which the join handler returns as an ordinary join-error ack via `emitJoinError`. That last hop is verified by inspection, not by test: `src/index.ts` is in `vitest.config.ts`'s coverage `exclude`, so no test reaches the socket-ack seam. The typed error's `code` never crosses the wire — `emitJoinError` special-cases only `CryptoVersionMismatchError` — so do not describe it as a client-visible code. The process **does not crash**: it serves `/health` 200 indefinitely while failing every voice join, so nothing restarts it. An oversized count is the likelier typo, and absent the ceiling it would fork more subprocesses than the container cgroup can hold — `init()` would never resolve, `/health` would never answer, and a scoped deploy's `--wait --wait-timeout 180` would fail with the old container already gone. Both descriptions are of what the guard PREVENTS: since #2178 neither value reaches the worker loop, because the loader exits first. `NUM_WORKERS` is a **per-file compose literal**, never an interpolation: `"4"` in `docker-compose.yml` (which declares no `cpus` limit, so it matches the code default) and `"3"` in `docker-compose.production.yml`, sized to that file's `deploy.resources.limits.cpus: '3'`. A mediasoup worker is a single-threaded C++ subprocess pinned to one core, so N > cpus oversubscribes the CFS quota and surfaces as RTP jitter rather than as a failure. Nothing enforces agreement between the two — the compose comment is the coupling; change them together.

**Do NOT re-introduce** an unconditional allow for `'null'` or `'file://'` — these were the pre-#830 mental model that #894 retired. If a future Electron change needs a new privileged scheme, register it (see `app://concord` in `.claude/rules/electron.md` (in Desktop)) and add it to `ALLOWED_ORIGINS`, do not carve a gate exemption.

## mediasoup Patterns
- One router per room via `MediasoupService.getOrCreateRouter()`
- Routers are placed on the **least-loaded worker by live consumer count**, not round-robin
  (#3149). `RoomManager` supplies per-room counts through a provider installed at wiring time;
  `MediasoupService` owns the room→worker assignment and sums per worker. Do NOT add a shadow
  tally — the count is enumerated from authoritative room state at selection time.
- **Ties break on the fewest assigned routers, NOT the lowest index**, and that is load-bearing
  rather than a refinement. A room reports 0 consumers until a second participant joins AND
  completes a client-driven `consume` — seconds. Under a lowest-index tie-break every room
  created in that fill window took worker 0 and stayed there for life, making a burst strictly
  worse than the round-robin it replaced. Breaking on room count restores round-robin's one
  virtue — correctness under zero information — while load still dominates whenever it is known.
  Do not "simplify" this back to a lowest-index tie-break.
- The surviving residual is **same-tick creation only**: `this.routers.set` runs after
  `await worker.createRouter(...)`, so two rooms selecting in one tick both see zero routers and
  co-locate. Genuinely one `await` wide, self-correcting, and pinned by a test that fails if
  placement is ever serialized or given a reservation.
- One send transport plus multiple receive transports per participant (main window + PiP); receive transports are keyed by ID and closed only through owner-scoped `RoomManager` operations
- `RoomManager` handles produce/consume/connectTransport/closeRecvTransport — do NOT recreate these on MediasoupService
- Always clean up transports and consumers on participant leave
- **Media-policy latch (#2153).** `policed` lives on the `ProducerEntry` and is set exactly once,
  **synchronously, before** `await pause()` — never after, so a resume racing the pause cannot slip
  through the gap between latching and the await settling. Nothing ever sets it back to `false`; it
  is removed only together with the entry, on both close paths (`closeProducer` and
  `transportclose`). **The server never resumes a policed producer on its own** — only the owning
  client's own subsequent action (a local close, a rejoin) can end the paused state, and even the
  client's own `resume-producer` is refused for both audio and video while the latch holds.
- **Identity fence.** A verdict computed against a captured `{participant, entry, producer}` triple
  is applied only if that triple is still live and unchanged at apply time
  (`isPolicedRefCurrent`: `rooms.get(roomId)?.participants.get(userId) === participant &&
  participant.producers.get(producerId) === entry && !entry.producer.closed`, and the latch adds
  `!entry.policed`) — otherwise the verdict is dropped as a no-op. This is the
  same one-await-wide TOCTOU discipline used elsewhere in this file (per-participant camera
  eviction, recv-transport reservation): a participant or producer that changed identity during the
  policer's own `await` must never receive a verdict computed against its predecessor.
- **Bounded reads.** mediasoup's `Channel.js` (3.26 through 3.28) has no request timeout, so every
  `getStats()` call the policer issues races `POLICER_READ_TIMEOUT_MS` and is skipped — reported
  `failed` for that tick, never reissued — if a previous read of the same object is still
  outstanding. Without the outstanding-read guard a wedged worker would let pending channel
  requests pile up unbounded. A client close reuses that producer's outstanding bounded tick
  result for its final sample; it never dispatches a second worker read or drops a successful
  result from both accounting paths.
  A failed read is a no-op for its whole group that tick: no baseline
  moves, no debt drains, no strike. A later successful sample resumes accounting. Three
  consecutive failed ticks mark degraded health and force-disconnect the exact admitted
  send transport whose stats failed and still has a live producer. A listen-only session
  has no forwarded upload to meter and stays connected. The owner receives
  `stats_unavailable`, with no abuse cooldown; the client leaves the call. Streaks are
  keyed to the send-transport ID, so a rejoin starts at zero rather than inheriting a
  predecessor's failures. A worker-wide stats IPC outage may therefore drop every
  active sender. At a healthy 5-second tick cadence with a 1-second read timeout,
  detection takes about 15–16 seconds from onset in the worst phase; skipped ticks, event-loop
  stalls, or a slow teardown extend wall time. This is a bound in completed failed samples,
  not per-packet ingress shaping.

## Per-room media limits (post-#1539, tier-aware post-#1542)

### Server Voice participant admission (#2231)

Channel rooms have one hard, non-tier-tunable participant ceiling:
`MAX_SERVER_VOICE_PARTICIPANTS = 1000` in `roomManager.ts`. The 1,000th unique
participant is admitted; a 1,001st new participant is rejected. An already
present user may reconnect at capacity so replacing a stale socket does not
evict a valid session. This guard applies only to channel rooms; Private Call
lifecycle work retains its separate 255-participant bound.

Keep the channel count check, reconnect classification, and
`room.participants.set` in the same synchronous section after room lookup. In
particular, do not insert an `await` between the new-user capacity check and the
participant insertion: concurrent boundary joins must not both claim the final
slot. The limit check also stays before existing-session cleanup so a rejected
join cannot evict a valid peer.

The control-plane heartbeat bridge uses the same exact 1,000 constant, guarded
against drift by `TestServerVoiceParticipantLimitMatchesMediaAdmission`. It
rejects an incoming media set or persisted database read above 1,000. When both
inputs are within that ceiling, the bounded reconciliation envelope may inspect
2,000 distinct stale/current IDs. Stale rows are cleared before replacements
and the remaining/current union must fit 1,000, so the callback's own
replacement work cannot create an over-cap persisted set from bounded inputs. A
deadline can leave an underfilled set, but the next heartbeat resumes safely.
Each retry orders missing current rows first, then persisted rows from oldest
`lifecycle_event_at` to newest with a UUID tie-break. Successful inserts and
refreshes therefore move behind untouched work without cursor state. Work stays
sequential through 255 participants; larger sets use exactly five workers that
claim consecutive indices from one shared atomic queue, keeping claimed work on
the priority prefix instead of splitting it into stride partitions. Workers
and temporary-grant cleanup stop on callback cancellation.

Durable stale-clear/add lifecycle work and its WebSocket fanout run before the
best-effort permission sweep. Heartbeat participant-delta and room-empty fanout
uses `BroadcastToServerContext`, so a saturated queue stops on callback
cancellation or Hub shutdown. Any undelivered committed delta forces a
conservative Rich Presence reconnect instead of leaving local clients stale.
Permission sweeps remain detached from the lifecycle deadline: the latest
pending exact set coalesces per channel and a global keyed FIFO prevents a
requeued channel from jumping ahead of already pending channels. The separate
Rich Presence reconnect snapshot limit remains 512 candidates; the room ceiling
does not make all 1,000 participants visible in one snapshot.

Roll out the control-plane's 1,000-participant heartbeat support before the
media-plane admission cap, or deploy both from one synchronized stack. The
join-time cap is not retroactive. Before completing media-plane rollout, verify
that neither live room membership nor `voice_participants` contains a channel
above 1,000. Drain participants until both sets are within the limit, or
manually reconcile legacy rows; do not rely on a process restart alone, because
the bounded control-plane reader rejects a 1,001st persisted row rather than
deleting it. Those heartbeats were already outside the former 255-participant
control-plane bound; the rollout must not depend on them becoming newly accepted.

Camera and screenshare producers are capped **per room** at the `produce` boundary
(`roomManager.ts`), and since #1542 both caps are **tier-aware** (ADR-0029):

- **Cap-tier resolution** is `resolveRoomCapTier(room)` — a pure, synchronous helper.
  **Channel rooms follow the SERVER's Mach tier** (`room.ownerTier`, from the
  `room_owner_tier` field on the channel join-authorize response). Since 2026-07-10
  (ADR-0029 amendment) the control-plane fills that field from the server SUBSCRIPTION
  axis — `entitlements.RoomCapTierForServer(h.serverTier(serverID))` — NOT the channel
  owner's personal plan: the server subscription provisions the channel's egress, so
  **neither a premium *member* NOR a premium *owner* on a free server raises a
  large/public channel's cap** (the abuse vector the asymmetry closes; Groundspeed →
  free, any Mach/selfhost → premium, collapsed to the SAME binary `'free'|'premium'`
  wire — the media-plane parse at `auth.ts` and this helper are unchanged). The
  `room_owner_tier` field name is retained for wire stability though it no longer keys
  off the owner. **DM rooms follow the MAX tier among present participants**
  (`Participant.tier`, the #1300 server-authoritative per-user datum — no new wire
  field; for DM rooms it arrives via the `media_entitlements` object on
  `POST /dm/conversations/:id/voice/authorize`, added to that endpoint by the #1542
  review reconciliation). Anything unknown/absent/empty fails closed to `'free'`. Both
  inputs are server-authoritative join-authorize values — never `socket.handshake.auth`.
- **Camera:** `resolveVideoPublisherCap(room)` = free `config.freeVideoPublisherCap`
  (default **8**, env `FREE_VIDEO_PUBLISHER_CAP`) / premium `PREMIUM_VIDEO_PUBLISHER_CAP`
  (**25**), clamped `[1, ABSOLUTE_VIDEO_PUBLISHER_CEILING = 25]`.
- **Screenshare:** `resolveScreenProducerCap(room)` = free `config.freeScreenProducerCap`
  (default **8**, env `FREE_SCREEN_PRODUCER_CAP`) / premium `PREMIUM_SCREEN_PRODUCER_CAP`
  (**16**), clamped `[1, ABSOLUTE_SCREEN_PRODUCER_CEILING = 16]`. This replaces the flat
  pre-#1542 `SCREEN_PRODUCER_CAP = 5` constant. Raised from the #1542 free 1 / premium 3
  for competitive parity (2026-07): major comms platforms cap stream *quality*, not concurrency —
  every voice-channel participant can Go Live simultaneously — so a 1-stream free room
  was a visible product gap (founder decision; see the ADR-0029 addendum). Free 8
  matches the free camera cap; premium 16 = 2× free.
- Both resolvers clamp **both** ends: the upper bound stops a tier value raising the
  global ceiling; the lower bound (1) stops a 0/negative tier value — which bypasses
  config's `parsePositiveIntEnv` — from disabling the cap.
- **Produce-time evaluation, grandfather-on-downgrade.** The cap is read inside the
  synchronous reservation at each produce, so membership churn (premium joins/leaves a
  DM; server subscription change refreshed on the next channel join) applies to NEW produces
  only. Existing producers are NEVER retroactively paused/closed on a downgrade — the
  transient over-cap state drains as producers close, bounded by group-DM size and the
  absolute ceiling. Do NOT add a force-pause path. **Accepted transient degradation:**
  a fail-closed `'free'` resolve during ANY member's channel join overwrites
  `room.ownerTier` free-ward until a later successful resolve — degradation can only
  ever LOWER caps, never escalate, and existing producers grandfather through it.

This is a `risk: security` capacity/DoS guardrail, so it is engineered to actually hold
against the adversary it bounds (a single misbehaving free client):

- **TOCTOU-safe (concurrency).** The count check + a synchronous slot **reservation**
  (`room.pendingProducerCounts`) run with no intervening `await`, then the reservation is
  released once the producer is recorded in `participant.producers` (or on `produce()`
  failure). Without the reservation, N concurrent `produce` calls would all pass the count
  check against a stale pre-`await` count and overrun the cap. **Do NOT** revert to a bare
  `countProducersBySource(...) >= cap` check across the `await` — that is the exact bypass
  the reservation closes (regression-locked by the concurrent-burst test in
  `roomManager.test.ts`).
- **Source is validated, not trusted.** The client-declared `appData.source` selects which
  cap applies, so `produce()` enforces `kind === 'video' ⇒ source ∈ {camera, screen}` and
  `kind === 'audio' ⇒ source ∈ {mic, screen-audio}`. A video stream mislabeled `'mic'`
  cannot dodge the camera cap. (mediasoup independently enforces `kind` ↔ actual media, so
  video media → `kind: 'video'` → a capped video source.)

With both in place the server-side reject of the (cap+1)th camera `produce` genuinely
cannot be bypassed by the client — a producer only exists if the SFU creates it, and the
cap holds under concurrency and mislabeling.

**Tier-seam (#1294 — realized by #1542):** the per-room seam is now live:
`resolveVideoPublisherCap` / `resolveScreenProducerCap` read `resolveRoomCapTier(room)` and
return the paid value for premium rooms (camera paid = the ceiling, 25 — raising paid above
25 means touching `ABSOLUTE_VIDEO_PUBLISHER_CEILING`, not just the resolver). Do NOT hardcode
a per-tier branch elsewhere; the resolvers remain the single seam. Rationale:
`docs/design/media-egress/cost-scale-model.md` §7 (P0 capacity guardrail) + ADR-0029.

**Consumer priority (#1542):** at consume-time the SFU sets `consumer.setPriority()` from
the *producer's* source via `consumerPriorityForSource` — **audio (`mic`, 255) >
screenshare (`screen`/`screen-audio`, 128) > webcam (`camera`, 1)** — so under BWE
congestion the transport sheds webcam before screenshare before audio. It is a BWE hint at
the transport layer (E2EE-safe, payload-independent) and non-fatal: a `setPriority` worker
error logs PII-safe and the consume proceeds.

## Profile-aware room codec floor

`RoomManager.computeCodecFloor` intersects only primary video codecs from participants with usable
video RTP capabilities; RTX, RED, ULPFEC, and other auxiliary/unsupported codecs never enter the
floor. Audio-only or unusable video claims do not constrain other participants. It returns `null`
until at least two participants advertise video, or when they have no common codec. Per-recipient
`canConsume` still prevents delivery to a participant that cannot decode a producer.
The camera layering gate uses active camera producer codecs, not receive claims, to choose its
producer-count threshold. Any VP8/H.264/unknown codec or multiple-encoding camera producer uses
the simulcast threshold; AV1/VP9 single-encoding producers use the SVC threshold. Before a camera
producer exists, an empty or disjoint `null` floor is treated conservatively as simulcast once
two participants have submitted capabilities; fewer retain the unconstrained SVC default.
With zero camera producers, stale consumer demands cannot enable the gate while producer-close
notifications are still pending; the existing OFF debounce bridges a normal replacement gap.

- AV1 and VP8 use lowercase base MIME keys. VP9 uses `video/vp9:0|2`; a missing VP9 `profile-id`
  means Profile 0. Profile 0 never satisfies Profile 2.
- H.264 admits Concord's `packetization-mode=1`, `level-asymmetry-allowed=1` profiles and normalizes
  RFC 6184 Constrained Baseline, Main, and High classes to the router's canonical profile-level-id.
  Different levels in one class intersect; High and Main do not.
- A completely profileless legacy H.264 capability may emit base `video/h264` only when every
  capable participant has that same base key. It is not a wildcard for a participant advertising a
  specific profile. For legacy floor broadcasts, clients treat bare VP9 as Profile 0 and bare
  H.264 as matching only a bare H.264 target; neither is a wildcard for a specific profile.
- `cameraLayeringCodecKind` treats both qualified `video/vp9:*` and legacy bare `video/vp9` as SVC.
- The **HEVC family is hardware-gated and never enters a floor** per
  [ADR-0044](../../../docs/adr/0044-codec-admission-policy.md). There is no `video/H265` entry in the
  router `mediaCodecs` list and no mediasoup worker codec class for it, so it cannot be intersected
  today. When #3241 adds one behind a router flag, HEVC is admitted only where both ends report
  hardware — it is a capability-gated tier, not a floor candidate, and it has no software fallback.
  **Concretely: `canonicalVideoCodecKey` must not gain an H265 branch until a hardware-provenance
  signal exists and is checked at that branch.** `RtpCapabilities` carries no hardware/software
  distinction for ANY codec — the floor already cannot tell AV1-HW from AV1-SW — so adding H265 the
  way AV1, VP8, VP9 and H.264 were added would silently admit software-only peers into the floor.
  #3241 lands the provenance signal in the same change that adds the branch, never after.

## Camera layer-selection gate (post-#1545)

`RoomManager` owns receiver camera layer demand state (`room.cameraLayerDemands`) and the room-level `cameraLayeringGateEnabled` flag. Clients can request preferred receive layers, but the SFU is authoritative for whether layered camera publishing is enabled:

- Store demand by consumer ID after validating ownership/video/camera source and applying the receiver's entitlement cap. Use physical pixels (`cssWidth`/`cssHeight` × `devicePixelRatio`) when deriving the maximum useful spatial layer so HiDPI clients and server clamping agree.
- Recompute `camera-layering-gate` from stored demand, active camera producer count, codec family (`svc` vs. simulcast fallback), and hysteresis. SVC can enable when visible demand benefits from lower layers; simulcast fallback additionally requires enough camera producers to justify multi-encode cost.
- **Camera gate-OFF debounce (reproduce-gap guard).** Gate-ON is immediate. When live demand would turn an enabled room camera gate OFF, defer the decision for `CAMERA_GATE_OFF_DEBOUNCE_MS` (1500ms), then recompute from current producers and demands. Any ON recompute cancels the pending OFF. The timer belongs to its room and is cleared when that room closes; never emit OFF from stale demand captured before the debounce.
- Demand cleanup is part of the consumer/producer/participant lifecycle. Closing a consumer or losing the associated camera producer must remove its demand and recompute the gate.
- **The join-room ack carries `cameraSpatialCap` — the cap the participant was ADMITTED under (#3279).** The desktop predicts, client-side, how many IGNIS pressure steps remain, and a prediction made against a different cap than the one `validateAndClampLayerDemand` clamps to spends a step moving the forwarded layer to where it already was. Every client-side derivation of this number disagreed somewhere: a live subscription-store read is wrong after a mid-call upgrade (`Participant.tier` is written once at join and `tierchange.go` deliberately leaves the SFU session alone); a store snapshot taken at join is wrong on a recovery rejoin, where `recoveryReset()` preserves the previous subscription by design (#2199); and even the renderer's own REST `media_entitlements` is wrong when a tier changes between that call and the media plane's **separate** `validateChannelAccess` re-authorization at join-room. Sending the admitted value is what ends the sequence — it is resolved by the party that clamps against it, at the moment it clamps. `cameraSpatialCapForParticipant` is module-level precisely so the advertised value and `maxCameraSpatialLayerForParticipant` are one expression rather than two copies of a rule; a test asserts the ack value equals the layer the clamp actually applies, at 1080p so the premium half cannot agree for a reason unrelated to entitlement. The field is **optional**: a participant that cannot be resolved omits it, and the client keeps its own fallback chain (ack → REST `media_entitlements` → subscription store, in `resolveSessionCameraCap`) rather than being handed a guess. Do NOT reintroduce a client-side derivation as the primary source.
- Do not let a failed or invalid mediasoup `setPreferredLayers()` call prevent gate recomputation for simple single-layer consumers. The API is only valid for simulcast/SVC consumers; single-layer consumers still provide useful demand for deciding when layered production should turn on.

**Client layering-mode gates (#1921, corrected by #2242) — SFU unchanged.** The desktop client's
`supportSvc`/`supportSimulcast` toggles gate only the encoding plan after codec selection:
AV1/VP9 use SVC when `supportSvc` is on, H264/VP8 use simulcast when `supportSimulcast` and the
server gate allow it, and otherwise the selected codec stays single-stream. The toggles never
filter or replace codecs. This is **producer-side only**: it changes what `rtpParameters` advertise, and
the SFU does not choose the producer codec family — `consumer.type` (`svc`/`simulcast`/`simple`)
is still derived from what the producer published, the `camera-layering-gate` above is still
SFU-authoritative, and there is **no new wire field, join-handshake change, or
`media_entitlements` field**. **Screenshare SVC stays client-published and encode-gate-ungated when enabled**
(SVC is one stream, cost-neutral); the SFU still validates and applies receiver preferred-layer
thinning but excludes SVC screen demand from persistent screen-gate state.
**Screenshare _simulcast_ is now server-gated (post-#1924)** — H264/VP8 3-layer simulcast screen is
authorized by the SFU's `screen-layering-gate`, mirroring the camera gate above. See the
"Screen layer-selection gate (post-#1924)" section below; SVC remains outside the producer
encode gate, while receiver preferred-layer thinning still runs through the SFU.

## Screen layer-selection gate (post-#1924)

`RoomManager` owns receiver **screen** layer demand state (`room.screenLayerDemands`) and a
**per-sharer** gate map (`room.screenLayeringGateBySharer`, keyed by the screen producer's owner
userId) — a server-authoritative gate that authorizes the client's H264/VP8 3-layer **simulcast**
screenshare, mirroring the camera gate (#1545) but with a different threshold model. The
`risk: security` concern is capacity/DoS: unbounded 3× simulcast screen egress from every sharer.
The gate holds against a misbehaving client.

- **Per-SHARER, not room-global (#1924 review fix B).** The gate is keyed by the screen's owner
  (`sharerUserId` = `consumer.appData.producerUserId`, server-set), so a producer simulcasts only
  when its OWN viewers are heterogeneous — two viewers watching *different* screens no longer flip a
  room-wide gate and force *every* publisher into simulcast. `recomputeScreenLayeringGate(room,
  sharerUserId)` pre-filters `screenLayerDemands` to that sharer and emits **targeted to the
  sharer's socket** (`io.to(targetSocketId)`), never a room broadcast. Keyed per SHARER (not per
  producerId) so it is **stable across the sharer's reproduce**, which mints a new producerId. This
  also subsumes the late-joiner case: a sharer's gate is driven by its own viewers, so there is no
  room-wide state a joiner can miss (the earlier per-join gate-state emit was removed).
- **Demand-driven, NO producer-count arm.** The pure `computeScreenLayeringGate(input)`
  (`screenLayerGovernor.ts`, sharer-agnostic — the caller pre-filters by `sharerUserId`) is simpler
  than `computeCameraLayeringGate`: it drops the SVC short-circuit (SVC screen is client-published &
  ungated, above) and the simulcast producer-floor. Screen's simulcast benefit is *one shared screen
  watched by many at heterogeneous sizes*, so the gate keys on **viewer-size heterogeneity**: it
  enables when ≥2 **distinct viewers** (deduped by owning `userId`, NOT raw consumer entries) of
  *that sharer's stream* have visible demand and ≥1 wants a sub-top layer (`maxUsefulSpatialLayer <
  2` or `pressureStepDown`), with the same **2-on / 1-off** distinct-viewer hysteresis as camera.
  **A single client cannot trip it** — the stored demand carries `userId` and the gate counts
  distinct `userId`s, so one client consuming a victim's screen multiple times still contributes
  only 1 toward the ≥2 threshold (`consume()` does not dedup per `(userId, producerId)`, so counting
  raw entries was the bypass). AV1/VP9 screen never simulcasts — it stays SVC (`castingKindForCodec`),
  so this gate governs H264/VP8 only, and AV1+simulcast is structurally unreachable.
- **Produce-time enforcement — the gate is HARD (server-authoritative vs a patched client).** The
  gate is not merely a signal the client may honor: `produce()` runs `validateScreenVideoSource`
  synchronously (before the cap reservation and the `await sendTransport.produce()`) and **REJECTS**
  a `source:'screen'` video producer whose `rtpParameters.encodings.length > 1` (simulcast) unless the
  sharer's gate is enabled (`screenLayeringGateBySharer.get(userId) === true`) — `'Screen simulcast
  not authorized (screen-layering-gate disabled)'`. A single-encode screen is always allowed; a
  patched client publishing 3× simulcast with the gate off is rejected **before the producer exists**
  (TOCTOU-safe). It also rejects a second concurrent `screen` producer per participant
  (`'Participant already has an active screen producer'`). This is what makes "holds against a
  misbehaving client" literally true for screen (stronger than the camera gate, which is advisory) —
  do NOT weaken it back to a signal-only gate. (#1924 review hardening.)
- **Gate state excludes SVC screen demand (#2204).** `setPreferredLayers` validates AV1/VP9
  (`type === 'svc'`) screen demand and immediately applies it through
  `consumer.setPreferredLayers` for per-layer thinning, but does **not store** that demand in
  `room.screenLayerDemands`; SVC therefore cannot participate in either an immediate gate
  recompute or the delayed gate-OFF re-check. H264/VP8 `simple` / `simulcast` demand still enters
  the map and recomputes the gate normally.
- **Screen demand is NOT camera-entitlement-clamped.** `validateAndClampLayerDemand` is source-aware:
  camera keeps `maxCameraSpatialLayerForParticipant` (free → layer 1), but a `screen` demand uses the
  full simulcast spatial range (layer 2) — a free viewer on a full-stage simulcast screen is not
  wrongly clamped to half-res (screen resolution entitlement is the separate #2163 concern).
- **Gate-OFF debounce (reproduce-flap guard).** A gate flip triggers the sharer's `fastReproduceScreen`,
  which momentarily drops ALL that sharer's viewers' demands (their consumers close, then re-consume
  the reproduced producer via the client re-tune path). Gate-ON is immediate; gate-OFF is **debounced**
  by `SCREEN_GATE_OFF_DEBOUNCE_MS` (1500ms) per sharer and re-checked at fire time, and any ON recompute
  cancels a pending OFF — so the reproduce gap does not flap the gate (which would trigger a spurious
  re-reproduce). Timers live on `room.screenGateOffTimers` and are cleared on `leaveRoom` /
  `closeRoom` (`cleanupSharerScreenGate`).
- **Client re-consume across reproduce (#1924 review fix A).** Because a screen reproduce replaces the
  producer (new producerId) and the desktop screen model is opt-in, a manually-tuned-in remote viewer
  would otherwise LOSE the share on every reproduce. The client (`voiceService.ts`) stashes a
  short-lived (3s) per-sharer "reproduce-pending" marker on `producer-closed` (only if it was tuned
  in, capturing dominance) and, on the sharer's next `new-producer` announce, auto-re-tunes-in to the
  new producer **regardless of `autoTuneInScreenShares`**, restoring dominance. The 3s window bounds
  it so a genuine stop-then-restart is not force-re-consumed. This also fixes the pre-existing
  codec-floor reproduce drop.
- **Reuse `set-preferred-layers`, route by source.** There is no new socket event. The existing
  `set-preferred-layers` handler routes to `setPreferredLayers`, which runs the shared
  `validateAndClampLayerDemand` prefix (parse → ownership → video-kind → source-aware cap → clamp)
  then dispatches by the server-set consumer source and server-derived type: `camera` →
  `cameraLayerDemands` + `recomputeCameraLayeringGate`; non-SVC `screen` → `screenLayerDemands` +
  `recomputeScreenLayeringGate`; SVC `screen` → apply preferred layers without persistent gate
  demand; any other source rejects. The demand parse/clamp primitives are the source-agnostic
  `cameraLayerGovernor` exports (name retained; a neutral rename is a future chore).
- **Three anti-waste disciplines.** (1) **Default-low + ramp-on-evidence:** a newly consumed
  *simulcast* screen consumer is seeded at `setPreferredLayers({spatialLayer:0, temporalLayer:0})`
  (SVC screen keeps mediasoup defaults) — quality is earned from demand, never given away by
  default; the seed is non-fatal + PII-safe-logged, like `setPriority`. (2) **Smallest-sufficient-
  layer** forwarding via the shared `layerForRender` pixel-bucketing. (3) **Gated encode with
  hysteresis** — the client only publishes simulcast when `screen-layering-gate` flips
  `screenLayeringEnabled`, and the edge-triggered emit + 2-on/1-off band prevents re-encode flap.
- **Demand cleanup is lifecycle-bound.** `screenLayerDemands` is cleared at all four camera-parallel
  sites — consumer `transportclose`, consumer `producerclose`, explicit `closeConsumer`, and
  participant `leaveRoom` — each recomputing the gate. Orthogonal to the #1542 screen producer cap
  (cap = concurrency; gate = quality).
- **E2EE:** H.264 uses one NAL-aware AEAD per access unit and VP8 uses whole-frame v5; each
  simulcast frame gets a fresh random IV, so both remain safe at any authorized layer count. AV1
  screen never simulcasts (see e2ee rules). Never bind RID/SSRC into an IV or H.264 AAD.

## Per-room audio last-N (post-#1544)

Audio forwarding is capped **per room** at the top-N active speakers (free default
**8**, env `FREE_AUDIO_LAST_N`; absolute ceiling **16** = `ABSOLUTE_AUDIO_LAST_N_CEILING`,
a code constant). The cap is `resolveAudioLastN(room)` — same clamp-both-ends discipline
and same `#1294` tier-seam as `resolveVideoPublisherCap` (free now; paid 16 deferred to #2288). The
mediasoup `AudioLevelObserver` is sized to N; a pure `ActiveSpeakerSet` (`activeSpeakerSet.ts`,
N-slot + ~2.5 s leave-hysteresis, `AUDIO_LAST_N_HOLD_MS`) owns the top-N decision.

This is a `risk: security` capacity/DoS guardrail (a single large free room can otherwise
demand an un-servable mesh), so it is engineered to **actually hold against a misbehaving
client**:

- **Server-authoritative enforcement (the unbypassable boundary).** The SFU pauses each
  subscriber's non-top-N **mic** audio consumers server-side, and `resumeConsumer`
  **refuses** to resume any consumer in `room.lastNPausedConsumers`. A client emitting
  `resume-consumer` for a last-N-paused consumer cannot re-forward it — that is the cap
  bypass the guard closes (regression-locked by the "refuses to resume … (client cannot
  bypass the cap)" test in `roomManager.test.ts`). **Do NOT** make `resumeConsumer`
  unconditional or remove the guard.
- **TOCTOU-safe.** `lastNPausedConsumers` is the synchronous source of truth — mutated
  (add before pause / delete before resume) **before** the async `consumer.pause()/resume()`,
  so overlapping 300 ms observer ticks cannot open a window where the guard's view is stale.
- **Composes with server-deafen (moderation not bypassed).** Last-N's resume path skips a
  subscriber whose `serverDeafened` is set — otherwise a deafened user would receive audio
  when their speaker enters the top-N (moderation bypass). Regression-locked by the
  "does NOT resume a SERVER-DEAFENED subscriber …" tests. **Do NOT** drop the
  `if (participant.serverDeafened) continue` guard in **either** `setAudioForwarding`
  (over-cap resume path) **or** `drainLastNPaused` (the #1742 small-room/shrink resume path).
- **Mic-only; producer never paused.** Only `mic` producers are last-N-managed
  (`micProducerIds`); `screen-audio` is audio-kind but **never** last-N-paused. The
  **producer is never paused** (active-speaker detection must stay live). Cleanup of
  `lastNPausedConsumers` runs on **every** teardown: the consume `transportclose`/`producerclose`
  handlers AND the explicit `closeConsumer` path (the latter does not fire those handlers).
- **Small-room no-op (post-#1742) — the cap only ENGAGES above N.** When
  `room.micProducerIds.size <= resolveAudioLastN(room)` (every call smaller than the cap — the
  overwhelming-majority case), last-N is a structural **no-op**: `applyLastNDelta` pauses
  nothing and instead **drains** `lastNPausedConsumers` (resume-on-shrink via `drainLastNPaused`,
  respecting the `serverDeafened` skip), and the consume-time seed does **not** start a fresh
  mic consumer paused (it only seeds when `micProducerIds.size > resolveAudioLastN(room)`).
  Resume-on-shrink has **two drivers**: the primary, event-driven one is `removeMicProducer`
  (the convergence point for every producer-close/leave flow), which calls the guarded drain the
  instant a leaving publisher drops the room to ≤ N — so recovery does not wait on the next
  `AudioLevelObserver` tick (which may not fire if the room has gone quiet). `applyLastNDelta`'s
  drain is the observer-tick backstop. **Do NOT** remove the guarded `drainLastNPaused` call from
  `removeMicProducer`.
  Applying the cap to a room that already fits under N is pure regression: the evict-on-silence
  pause/resume churn loses the leading frames of each new utterance during the resume gap → Opus
  PLC crackle (the user-reported symptom; founding incident #1742, regression introduced by the
  last-N rollout PR #1632). The guard keys on the server-authoritative `micProducerIds.size`,
  never client data, so it adds no trust surface and does **not** weaken the over-cap enforcement
  above — those invariants hold unchanged when the room genuinely exceeds N. Regression-locked by
  the `audio last-N small-room no-op (#1742)` suite in `roomManager.test.ts` (zero consumer
  pauses across a speak/silence cycle when ≤ N; resume-on-shrink; over-cap still seeds and still
  caps). **Do NOT** remove the `micProducerIds.size <= resolveAudioLastN(room)` short-circuit in
  `applyLastNDelta`, or the matching `> resolveAudioLastN(room)` guard on the consume-time seed.

**Desktop UI deferred:** a multi-speaker "currently audible" indicator is a follow-up; the
core feature is correct server-only (the client needs no change — the guard is authoritative).
Design: `docs/superpowers/specs/2026-06-18-1544-audio-last-n-design.md`.

## Per-user tier media gating (post-#1300)

The per-USER A/V quality lever (distinct from the per-ROOM caps above) is plumbed through a
**server-authoritative** `media_entitlements` object the control-plane adds to BOTH
join-authorize responses (`POST /channels/:id/voice/join` and `POST
/dm/conversations/:id/voice/join`). Shape (control-plane half, #1300):

```jsonc
"media_entitlements": {
  "tier": "free",                                                  // string
  "allowed_audio_tiers": ["minimum","low","moderate","standard"],  // []string
  "min_ptime_ms": 20,                                              // int
  "max_manual_bitrate_bps": 5000000,                               // int, send transport
  "camera_max_bitrate_bps": 2500000,                               // int, observed camera video
  "screen_max_bitrate_bps": 5000000                                 // int, observed screen video
}
```

- **Tier is never client-supplied.** The control-plane resolves it from the authenticated
  `user_id` via the shared entitlement cache (`entCache.GetTier`, #1296) and emits the caps
  straight from `entitlements.MediaFor(tier)` → `entitlements.For(tier)` (Go source of truth,
  `internal/entitlements/entitlements.go`). `For("")` fails closed to the free set, so a
  tier-resolution failure degrades to the free floor and never grants premium. A patched
  client cannot raise its own caps — the value is computed server-side. These are per-USER
  caps (the joining user's own transport/producers); the room-scoped caps
  (`MaxWebcamPublishers`/`MaxScreensharePublishers`) landed with #1542 and are intentionally
  NOT carried here — the channel room cap tier rides the separate `room_owner_tier`
  join-authorize field and DM rooms derive it from `Participant.tier` (see § "Per-room media
  limits" + ADR-0029); `MaxAudioLastN` remains a future seam. Tier is resolved once, at
  join — mid-session subscription changes ride membership churn for the room caps
  (produce-time re-resolution, ADR-0029) while per-user re-validate/`voice.enforce.disconnect`
  is future work. The join-room ack exposes the admitted camera/screen bitrate caps,
  allowed audio tiers, and minimum ptime after A2 re-authorization. The desktop
  pins these for the media session. Its active audio ceiling is the lesser of
  the admitted cap and the greater of the current personal cap and an admitted
  fixed channel audio standard; video uses the lesser of admitted and current
  personal caps. An upgrade cannot raise sender quality past the SFU's admitted
  snapshot, and a downgrade lowers stock-client senders immediately while
  preserving a channel's authorized audio standard. **A patched client can
  still use the old server-side premium caps after a downgrade until it rejoins or
  a future server entitlement re-authorization disconnects it.** Do not describe
  mid-call downgrades as fully server-enforced.
- **What the SFU enforces — and its honest limits (media-plane half, landed).**
  Browser encoder settings and REMB steer **cooperative/stock clients**. The
  sampled observed-rate policer can stop forwarding from a patched client after its video
  window or audio debt trips,
  subject to the interval, overhead credit, and source-label limits below. Do
  not describe the advisory or sampled enforcement as a per-packet guarantee.
  - **Bitrate advisory (`transport.setMaxIncomingBitrate(maxManualBitrateBps)`)** applied in
    `roomManager.ts` `createTransport` to the participant's **producing (`send`) transport** (the
    `recv` transport keeps the global default). It is a **BWE / congestion-control advisory**
    (REMB / transport-cc), **NOT a hard RTP policer**: a stock browser/Electron stack honors it
    and throttles its encoder, but a client that ignores congestion-control feedback can send
    above it and the SFU forwards what arrives. Bounds **cooperative** cost, not an adversarial
    sender. The separate sampled media policer pauses sustained violators after
    it observes them. A `setMaxIncomingBitrate` worker error on a SEND transport
    now closes that transport and refuses its creation; a receive-transport error
    remains best-effort.
  - **Send-transport structural bound (D1, #2153).** `createTransport` refuses a second
    `create-transport(send)` while a participant's existing send transport is open, rather than
    replacing it. Before this, a second create-transport(send) replaced the first in place, which
    let a client reset the policer's aggregate debt and drop unmetered audio bytes on each swap.
    The stock client creates exactly one send transport per session, so no stock flow is affected.
  - **Keyframe-request pacing (D3, #2153).** Video producers are created with mediasoup's
    `keyFrameRequestDelay` ≈ 1000 ms, bounding how often viewers' PLI (keyframe) requests force a
    sender to emit keyframes. Without this bound, a viewer flooding PLI requests could push a
    stock sender's encoder over the aggregate rate limit through keyframe volume alone and get it
    paused.
  - **Audio tier/ptime gate (`enforceAudioTierGate`)** is a **produce-time tripwire on
    client-DECLARED values**. For the joining user's own **mic** producer it REJECTS (throws,
    so the producer is never created — TOCTOU-safe, before the `await produce()`) when:
    - the effective opus `ptime` is absent (`ptime_undeclared`) or below `minPtimeMs`
      (`ptime_below_tier_minimum`). Effective ptime is the codec-level `ptime`, else the
      transport-level `rtpParameters.ptime`; it is **NOT** `minptime` — that is only the
      encoder's lower bound and stock Chromium/Electron emit `minptime=10` while packetizing at
      20 ms, so using it would falsely reject every stock free mic;
    - no bitrate is declared (`bitrate_undeclared`), or a declared bitrate exceeds the ceiling
      for the highest entry in `allowedAudioTiers` (`opus_bitrate_above_tier_ceiling`;
      `AUDIO_TIER_OPUS_BITRATE_CEILING_BPS`, mirroring the canonical client map
      `AUDIO_QUALITY_TIERS` in `client/desktop/src/renderer/stores/voice/audioQualityTiers.ts`
      (re-exported by `voiceStore.ts`) — free `standard` ceiling **96 000 bps**). The two sources
      are the highest `encodings[].maxBitrate` and the fmtp `maxaveragebitrate`: either one
      satisfies "declared", and when both are present both are checked. A value that is neither a
      finite number nor a numeric string counts as absent.

    A mic with no opus codec skips the gate. It cannot become a producer: `produce()` first
    refuses an audio producer unless every codec's mimeType is `audio/*` (`codecsAreAudio`,
    `media_schema_rejected`, "Invalid codec for producer kind"), opus is the router's only audio
    codec (`mediaCodecs` in `src/config/index.ts`), and mediasoup refuses an audio codec the
    router lacks. The `audio/*` check is load-bearing: mediasoup does not compare a producer's
    `kind` with its codecs, so before it a `kind: 'audio'` mic carrying `video/VP8` (alone, or
    ahead of an opus entry) became a live producer that skipped this gate and sent video down
    the Speak-only mic path (red-team PoC, #3858; pinned against a real worker by
    `tests/roomManager.codecKind.realWorker.test.ts`). Adding a second audio codec to the router
    means adding its own checks here. The check is SYMMETRIC (#1308): every codec on a video
    producer must be `video/*` (`codecsAreVideo`, same reason code and message), because without
    it a member with Video but no Speak could publish `kind: 'video'`, `source: 'camera'`
    carrying `audio/opus` -- mic audio past the Speak check and this gate. `video/rtx` is a
    `video/*` codec, so a stock RTX-bearing camera passes. Pinned against the same real worker.

    The stock client sends `ptime` and `encodings[].maxBitrate` on every tier; mediasoup-client
    passes both through to the `produce` event. It never sends `maxaveragebitrate`
    (mediasoup-client writes it into the SDP answer only), which is why `maxBitrate` is the
    primary source. The gate used to admit on absence on the premise that a stock client omits
    these; that premise was wrong (#3858), so absence is now refused. The four `reason` strings
    are a fixed, closed set so that #1308 can count them: keep them short and stable.

    **Every refusal is reported (#1308).** `rejectAudioTier` emits a `tier-violation` room
    event before it throws, and the NATS bridge publishes it as `voice.tier_violation` (see
    § NATS & Redis). The four reasons fold onto the control plane's two counted ones through
    `AUDIO_TIER_VIOLATION_REASON`: `ptime_undeclared` and `ptime_below_tier_minimum` become
    `audio_ptime_below_tier`; `bitrate_undeclared` and `opus_bitrate_above_tier_ceiling` become
    `audio_bitrate_over_tier`. Absence folds onto the matching reason because the stock client
    declares both on every tier. The report cannot change the refusal: `emitEvent` contains
    handler failures and the throw follows unconditionally.

    Honest limit: every value is a declaration and does not bind the encoder. A client that
    declares in-tier values and sends more is not stopped by this gate; the observed-rate
    policer (#2153) is what bounds bitrate. `enforceAudioTierGate` itself still only runs for
    `source === 'mic'`, so a mic relabelled as `screen-audio` still escapes THIS gate — but it
    no longer escapes enforcement: the media-rate policer below (#2153) meters every audio
    producer's observed RTP bytes regardless of its declared `source`, so a relabelled mic is
    metered in the `screen-audio` slot against the same audio limit a mic gets. The reject path
    logs a **PII-safe** violation (`{ userId, kind, source, reason }` ONLY — never the
    rtpParameters payload / codec params / media, per `observability.md` #4).
  - **Video resolution/fps are NOT enforced by the SFU** — pixel dims/fps are not reliably in
    signaling; they stay **client-enforced (#1301) with bitrate advisory and observed-rate
    policing as backstops**. Do NOT claim the SFU enforces pixel dimensions/fps.
    Post-#2163 the **client** enforces a **tiered pixel-rate** res/fps model for the screen-share
    (stream) axis (free `StreamMaxPixelRate = 62_208_000` px/s → 1080p30 and 720p60 admit, 1080p60
    rejected) at three client seams (launch-reset clamp, `voiceService.produceScreen` boundary,
    resolution-tiered Settings fps options); the **SFU stance on resolution/fps is unchanged** —
    still no pixel-dim/fps enforcement, and that stays client-enforced (residual 5 below). #2153
    does not touch this axis; it bounds wire *rate*, not resolution or fps.
  - **Hard enforcement against a non-cooperative client** (the #1539/#1544 "actually hold against a
    misbehaving client" bar) **has landed as the media-rate policer (#2153)**, documented in
    full below. What #1300 delivered *hard* was the **unforgeable tier resolution** that #2153's
    enforcement keys on; #2153 adds the server-side measurement that narrows what CV-CAN-018 and
    CV-CAN-020 left open (see `docs/reference/security-findings-2026-07-06-codex-deep-scan.md` —
    both are narrowed, not closed, by this design).

### Media-rate policer (#2153)

Design: `docs/superpowers/specs/2026-09-23-2153-media-rate-policer-design.md`.

Server-side measurement of what actually arrives, narrowing the gap the bitrate cap (advisory,
CV-CAN-018) and the audio tier gate (client-declared fmtp, CV-CAN-020) both left open. It stops
forwarding — never ingress — for a producer whose sampled rate exceeds a limit derived live from
the server-authoritative `Participant` caps (never client-declared). Video violations close the
current video producers and block new video during a video-only cooldown; audio still uses the
pause/second-strike voice policy. `src/lib/mediaPolicer.ts` is pure (no mediasoup import, no
input type carries `rtpParameters` or fmtp) and computes verdicts
from plain numbers RoomManager reads off mediasoup's cumulative RTP counters; `src/lib/mediaPolicerTick.ts`
applies them (latch, pause, evict, emit, log).

**Honest residuals (state these verbatim-precise; never claim a hard guarantee beyond them):**

1. The audio policer bounds the **sustained average RTP-layer rate**. For free audio that is 2.25× the
   96 kbps ceiling: 216 kbps at the RTP layer, measured after SRTP decryption — it excludes the
   IP/UDP headers and the SRTP auth tag, which add roughly 15 kbps more on the actual wire for an
   audio slot at 50 pps (T0 evidence: `docs/superpowers/reports/2026-09-23-2153-media-policer-t0.md`).
   It stops hifi/studio-class abuse. It does not stop a marginal overshoot.
2. **It stops forwarding, not ingress.** A paused audio producer still receives ingress inside
   the worker. A closed video producer no longer has a producer SSRC mapping, so further packets
   for it are discarded after transport and SRTP handling, before producer forwarding. Closing a
   send transport during a voice eviction rejects those packets earlier, but also ends that user's
   audio. Packets still arrive at the network layer until the sender stops; dropping them before
   they reach the worker is a separate edge-control concern.
3. A send transport can no longer be replaced in place (D1): `createTransport` refuses a second
   `create-transport(send)` while a participant's existing send transport is open. The unmetered
   tail on a `transportclose` is therefore only leave/rejoin churn, bounded by `join-room` at 0.5/s
   and the ICE/DTLS setup cost — not a swap a client can trigger at will.
4. Audio and video cooldowns and strikes do not survive a media-plane deploy or a healthwatch
   restart. The design assumes a single node (see the single-node paragraph below).
5. Video has separate observed-rate buckets for the camera and screen source, in addition to
   the send-transport aggregate. Each bucket sums every producer and simulcast SSRC in that
   declared source and survives client close/re-produce with a bounded final sample. The camera
   and screen plan caps come from `MediaFor` through both join authorizations. The stock client
   enforces each plan cap (Free camera 2.5 Mbps, screen 5 Mbps); the server uses the highest
   authorized video cap for either declared slot (Free 5 Mbps), bounded by the single-source
   maximum. The source label is client-declared: a client can submit camera pictures as a screen
   stream, and mediasoup cannot prove which physical capture device supplied encrypted video.
   Resolution and fps remain client-enforced.
6. On an audio pause-failure fallback (a worker error), the producer is closed, and the owner's
   client closes its matching local capture too (F6), but the owner still gets no explanation.
7. The video policy closes video producers, so its violations do not leave paused video tiles.
   Other pause paths and older media-plane versions can still leave the PiP window on a frozen
   frame or a late joiner on a black tile: the consume response carries no `producerPaused`.
   The popped-out PiP controls window does not show the audio policy owner notice strip;
   `voiceService.toggleMute` still guards the microphone there. See `.claude/rules/frontend.md` (in Desktop).
8. Bundled renderers get no explanation until the next desktop release. A join refused during a
   cooldown on the automatic reconnect path (`resumeAfterReconnect`) tears down through
   `emergencyCleanup()` with no dialog; only `handleJoinFailure` opens it.
9. **Aggregate-trip attribution is by producer-level bytes.** The aggregate check reads the
   transport's own counters, which include ingress no producer counter sees (padding-only packets,
   unsolicited RTX), but a trip is attributed to the producer with the most producer-level bytes. So
   the producer that gets latched may not be the source of the excess, and the excess itself keeps
   arriving. A video-attributed aggregate trip closes current video and arms its cooldown; an
   audio-attributed or unattributed trip retains the audio strike and eventual voice eviction.
   Its envelope includes the maximum simultaneous
   camera, screen, mic, and screen-audio source rates plus bounded video RTP/E2EE overhead,
   then the existing 1.5× aggregate tolerance for retransmissions and worker accounting.
   `max_manual_bitrate_bps` is the highest *single-source* ceiling for REMB, not a combined
   media quota. The free aggregate threshold is 16,687,392 RTP/RTX bps with two 5 Mbps video
   guards. The source buckets enforce those guards; the aggregate is a coarse guard for
   unattributed ingress, not a user-facing bitrate promise.
10. **A send transport whose `setMaxIncomingBitrate` call fails is now refused and closed.**
    The advisory still cannot make a modified sender obey REMB, so observed-rate buckets
    remain necessary. Video buckets charge observed bytes against the source-agnostic server
    video guard after crediting observed RTP headers (28 bytes per packet, capped by a
    guard-derived packet rate using a 512-byte minimum credited payload) and at most
    240 encrypted-frame envelopes of 38 bytes
    per second. This is explicit protocol overhead, not a 10% entitlement uplift. The
    5-second stats interval measures against a 5% margin after this bounded overhead credit.
    Two consecutive violating samples (10 seconds) or three violating samples in a trailing
    60-second window (15 seconds total) close all of that user's current video producers and
    start a video-only cooldown of 5, 10, 30, then 60 minutes on distinct later violations.
    Each new violation moves the 24-hour escalation window forward; a full 24 hours without a
    violation resets the next cooldown to 5 minutes. Escalation state is in process memory.
    A gross sample at twice the admitted rate can trip immediately. Screen audio paired with
    a disabled screen share is closed too; a microphone remains available unless it separately
    violates audio policy. The worker has no per-packet limiter at this API seam, so a large
    one-sample burst can reach it before the sampled response. These margins have not yet been
    calibrated in a loaded room; no finite absolute ingress-byte bound follows.
11. **Sustained stats read failure closes the exact session.** After three consecutive failed
    completed ticks, the policer force-disconnects the participant only if the captured failed
    send-transport ID still matches the live session. It retries a failed teardown on later
    failed ticks. The disconnect has no media-abuse cooldown and a fresh join can try again;
    if stats IPC remains unavailable, that join will be disconnected after its own three
    failed ticks. A worker-wide IPC backlog can disconnect legitimate calls. The ordinary
    rate policer still does not shape individual ingress packets between samples.

**Reset-detection residual.** Baselines are keyed per SSRC, so a new SSRC is always counted from zero. A
stream recreated on the SAME SSRC is detected only while its new counter is still below the old
baseline; if it has already climbed past by the next read, that one interval's delta is computed
against the stale baseline and under-counts by it, exactly once. This is lenient
(under-counts, never over-counts) and bounded to a single interval; it can never mask a sustained
overshoot for longer than one `POLICER_INTERVAL_MS`.

**Single-node assumption.** `MediaPolicyLedger` (audio and video strikes and cooldowns) lives entirely in process
memory. It is lost on every media-plane recreate — a deploy or a healthwatch restart clears every
cooldown and every strike, and a restart also evicts every room (the process going down closes every
transport). `docker-compose.failover.yml` has no second media plane, so there is no cross-node
consistency question today. If a second node is ever added, the ledger must move to a store that
**fails closed** — Redis does not (the media plane is designed to keep serving voice when Redis is
down), so it is not a substitute without a different failure posture.

**Boy-scout: four existing media-plane env vars reach no container.** `FREE_VIDEO_PUBLISHER_CAP`,
`FREE_SCREEN_PRODUCER_CAP`, `FREE_AUDIO_LAST_N` and `AUDIO_LAST_N_HOLD_MS` are parsed in
`services/media-plane/src/config/index.ts` with hardcoded fallback defaults, but none of the four
appears in any compose file's media-plane `environment:` map (`docker-compose.yml`,
`docker-compose.production.yml`, `docker-compose.staging.yml`) — verified by inspection of all
three. Every deployment therefore runs on the parser's built-in defaults regardless of what an
operator might set in `.env`; this was true before #2153 and #2153 did not change it. #2153's own
policer constants deliberately do **not** repeat this mistake — they carry zero env vars by design
(R6 in the spec's reconciliation record): every constant is a named literal in `mediaPolicer.ts`,
changed only by a PR.
- The media-plane parses `media_entitlements` from the join-authorize response it already
  fetches: `validateChannelAccess` (`middleware/auth.ts`) reads it into `ChannelAccessResult`
  (`userTier`/`allowedAudioTiers`/`minPtimeMs`/`maxManualBitrateBps`) and the join-room handler
  threads it through `RoomManager.joinRoom` onto the `Participant`. The tier does NOT ride
  `socket.handshake.auth` — like display identity (§ Participant identity is server-authoritative,
  CV-CAN-017, above) it arrives only on the server-to-server join-authorize response.
  Fail-closed on a missing/malformed field to the local `FREE_MEDIA_ENTITLEMENT` constant — the
  ONLY place Node mirrors Go entitlement values, justified as the missing-field floor (free is
  the floor, so drift can only make Node STRICTER, never escalate). Each field floors
  independently, AND an **atomic-free clamp** ties the caps to the tier: only the explicit
  `premium` tier may carry premium values, so a `free`/unknown tier cannot carry a premium cap
  from a cross-field-inconsistent response (a control-plane bug or downgrade race). The narrow
  #179 exception is `channel_audio_uplift: true`, emitted only by the control-plane after it has
  bounded a fixed channel audio standard by the server tier; in that case, and only that case,
  `allowedAudioTiers` and `minPtimeMs` may widen for a free member. `maxManualBitrateBps` always
  stays clamped to the per-user free floor unless `tier === "premium"`. Cross-references the
  #1294 tier-seam documented above.

Design: `docs/superpowers/specs/2026-06-21-1300-media-tier-gating-design.md`.

### Per-channel audio standard — channel-aware `media_entitlements` (post-#179)

The control-plane voice join-authorize (`AuthorizeJoin`, `internal/voice/handlers.go`)
computes `media_entitlements` via `entitlements.MediaForChannel(userTier, serverTier,
channelTier)` (`internal/entitlements/audio_tiers.go`) — NOT the bare `MediaFor(userTier)`.
This is the per-channel audio **standard** mechanism (matrix §2.3 / §6):

- **Channel has an admin-set tier (`audio_quality_tier != null`)** → the standard is granted
  to **every** member, regardless of their own subscription, by *widening* the returned
  `allowed_audio_tiers` up to the channel tier AND lowering `min_ptime_ms` to that tier's frame
  size (so a free member forced to a 10 ms tier — high/hifi/studio — is not rejected by the
  ptime arm of `enforceAudioTierGate`). It is **bounded by the server's binary audio ceiling**
  (binary over the ADR-0028 ladder): Groundspeed → `standard` (≤96 kbps), any Mach level
  (`mach1`/`mach2`/`mach3`) or `selfhost` → `studio` (≤510 kbps), via
  `ServerAudioCeilingTier(ResolveServerTier(serverID))`. A channel tier above the ceiling is
  clamped down (defensive — `UpdateChannel` already rejects above-ceiling writes).
- **Personal (`audio_quality_tier == null`)** → `MediaForChannel` returns `MediaFor(userTier)`
  unchanged. Audio is **NOT** server-uplifted in Personal mode (a free user cannot
  independently exceed 96 kbps; matrix §2.3). An unknown channel tier fails closed to Personal.

**The media-plane remains the enforcement point, with a narrow parser exception.**
`enforceAudioTierGate` still enforces from the joining user's `media_entitlements`; #179 feeds it
a channel-aware entitlement plus `channel_audio_uplift: true`. That flag is required for a
non-premium member to carry channel-standard `allowedAudioTiers` / `minPtimeMs`; without it, the
atomic-free clamp floors those fields to free as under #1300. `ResolveServerTier` is the
inert #1521 seam (every server → Groundspeed today; real Mach values arrive with #1556 — ladder-shaped
`mach1`/`mach2`/`mach3` + `selfhost` per ADR-0028), so this logic
is a structural no-op at Groundspeed and live the instant Mach ships — and `UpdateChannel`'s
server-ceiling guard means an admin cannot set a channel above `standard` on a free server
(closing the pre-#179 bug where a Studio channel rejected every free member's mic).
`MaxManualBitrateBps` (≥5 Mbps) already exceeds the 510 kbps audio ceiling, so the uplift never
touches it and the media-plane still clamps it to the per-user free floor unless the user tier is
premium. Design: `docs/superpowers/specs/2026-06-28-179-per-channel-audio-standard-design.md`.

## SRTP Is Structurally Mandatory

mediasoup transports use DTLS-SRTP by default and the media-plane never instantiates a non-encrypted RTP transport. There is no per-room encryption flag and no runtime branch on encryption — every voice room is encrypted at the transport layer.

**Architectural rule:** do NOT re-introduce a runtime branch on any per-room, per-channel, or per-message encryption-status field (e.g., `room.isEncrypted`, `channel.isEncrypted`, `message.is_encrypted`, `access.isEncrypted`). Under the E2EE-everywhere posture (#201), encryption is a structural invariant, not a runtime property to be tested; every channel is encrypted by construction, and a conditional on encryption status is dead code at best and an opt-out path at worst. If you find yourself writing `if (room.isEncrypted)` or any similar gate, you are wrong — the answer is unconditional.

**Reference:** The field and its conditional gates were removed by the E2EE-everywhere epic — see #201 for the architectural rationale, #1024 (frontend) + #1027 (media-plane) for file-by-file changes.

## Docker Build Context Invariant (post-#1083 / #1097)

Any file referenced by `services/media-plane/package.json` that must be present at `npm ci` time MUST be copied into the Docker layer **before** `RUN npm ci` in `services/media-plane/Dockerfile`. This applies independently to **every** Docker stage that runs `npm ci` (currently only the builder stage — the production stage uses `COPY --from=builder` per the runtime-stage discipline below). Categories of references that require this:

- **`file:` override targets** (e.g., `"overrides": { "node-domexception": "file:./stubs/node-domexception" }`). The stub directory must exist in the build context, and must persist in the final runtime image because npm creates a relative symlink (`node_modules/<pkg>` → `../<override-target>`) that is resolved at runtime by consuming packages, not just at install time. *(Only category in active use as of #1097.)*
- **Local workspace dependencies** (e.g., `"dep": "file:../other-workspace"`). *(Preventative — no current use sites.)*
- **Patch files** for `patch-package` (typically `patches/<package>.patch`). *(Preventative — no current use sites.)*

The recurring failure mode is a cache-cold Docker build where `npm ci` runs in a layer that has only seen `COPY package*.json ./`, with the override / workspace / patch target invisible at install time. mediasoup's postinstall script then fails with `ERR_MODULE_NOT_FOUND`.

The local CI signal (`npm test` in the `media-plane` job of `.github/workflows/build.yml`) does **not** catch this — it installs against the workspace filesystem where all paths are present. Catching this class of bug structurally requires a cache-cold `--no-cache` Docker build (the `production-compose-docker` job in `build.yml`, which builds the full production-active compose stack — consolidated from the former media-plane-only `media-plane-docker` job and the retired `cache-cold-defense.yml` workflow by #1439).

**Founding incident:** PR #1083 (commit `29beb18`) added an `overrides` block for `node-domexception` to silence the upstream deprecation warning, but didn't update the Dockerfile to copy `stubs/` before `npm ci`. The local `npm ci` test verified by the author passed because the stub was on disk in the dev workspace; the production deploy of SHA `0764ff33` failed with `ERR_MODULE_NOT_FOUND` from mediasoup's postinstall. Resolved by PR #1097.

**Runtime stage discipline (post-#1098):** the production stage in `services/media-plane/Dockerfile` MUST NOT install compilation toolchain packages (`python3`, `py3-pip`, `make`, `g++`, `linux-headers`, etc.). These are needed only at install time for mediasoup's `postinstall` native worker compile, which happens in the builder stage. The production stage uses `COPY --from=builder /app/node_modules ./node_modules` to bring the pre-compiled `mediasoup-worker` ELF + pruned production deps (after `npm prune --omit=dev`) + the `node-domexception` override symlink. The runtime image carries only what `node:26-alpine` ships by default, minus its bundled npm/npx/corepack (removed: unused at runtime, and their vendored deps fail `container-image-scan.yml`), plus the compiled artifacts.

**BusyBox `wget` is load-bearing in the runtime image (post-#2178).** The invariant is no longer "the runtime stage carries only what `node:26-alpine` ships plus the compiled artifacts" in a purely subtractive sense — one thing the base image ships is now a hard dependency. The media-plane compose healthcheck probes `/health` with `wget --spider`, and no `apk` line in the Dockerfile installs it, so a future base-image bump that dropped `wget` would make the healthcheck fail silently and every SCOPED media deploy would then fail `--wait` — a deploy-time failure with no PR-time signal. Do not remove the assertion below, and do not switch the runtime base image without re-verifying that `wget` is present.

**Verification of record for the media runtime pin (2026-10-07).** Against the exact digest `node:26-alpine@sha256:0b36e8c136b94cd4fcf02188228e76c31ad5872eef3fec8cbd2eee500cfd9e80` pinned by the final stage of `services/media-plane/Dockerfile`, the Node 26.10.0 / Alpine 3.24.2 image was verified on **both** `linux/amd64` and `linux/arm64`: BusyBox provides `/usr/bin/wget`, its `--help` lists `--spider`, and the media-plane healthcheck command run verbatim inside each image — `wget --spider -q http://127.0.0.1:3000/health`, against a local listener on that port — exits 0. That is the command at [`docker-compose.production.yml:550`](../../../docker-compose.production.yml) and [`docker-compose.yml:406`](../../../docker-compose.yml). The runtime index children are `sha256:b341ca66519d9a1c25d4e41f254ffb6fe403fc0f9054c62b863f0660dcc1c199` (amd64) and `sha256:f78d6d4ca1b1463e66837575f9ac0035aaad30d9497dc9f3bc611b544be2ab15` (arm64/v8). Re-run this at the next runtime-base move rather than trusting this line; it records one pin, not a property of the `node:*-alpine` family.

**Enforced by:** the `production-compose-docker` CI job (`.github/workflows/build.yml`) runs `docker run --rm concord-media-plane sh -c '! command -v g++ && ! command -v python3 && ! command -v make'` against the cache-cold compose build, runs `docker run --rm concord-media-plane sh -c 'command -v wget'` to assert the healthcheck probe binary is present, and runs a worker-spawn smoke (`docker run --rm concord-media-plane node -e "require('mediasoup').createWorker()..."`) to verify the runtime image is genuinely functional, not just toolchain-free. The `concord-media-plane` image name is deterministic because the compose build step pins the project name via `-p concord`.

**Founding incident (no-toolchain rule):** the production-stage toolchain was present since the media-plane service was introduced. Identified during `/reconcile-copilot` on PR #1097 (finding #7, `@security-reviewer` LOW); resolved by issue #1098.

**Full-stack consolidation (post-#1439, supersedes the post-#1248 two-surface structure):** the `production-compose-docker` job in `build.yml` IS the compose-layer defense — it builds the full production-active 4-Dockerfile stack (postgres + control-plane + media-plane + ops-agent) via `docker compose --profile services build --no-cache` at PR-time, path-gated on `services/**`, `infrastructure/docker/**`, both compose files, and `build.yml` itself. The former two-surface split (media-plane-only PR-time job + push-to-main `cache-cold-defense.yml`) validated media-plane twice while full-stack signal arrived only post-merge; PR-time full-stack became affordable once Blacksmith bare-metal collapsed the cache-cold cost (ADR-0016 measured −83%). See ADR-0006 §"Tier 2" (incl. the #1439 addendum) for the authoritative tier framework and investigation report 2026-05-28-1124 §5.B for the original two-surface design rationale.

## Custom base image discipline (post-#1166)

The `services/media-plane/Dockerfile` builder stage uses `ghcr.io/concord-voice/node-buildtools:<node-tag>-<DATE>@sha256:<digest>` as its FROM base, pre-baking the `python3 py3-pip make g++ linux-headers` toolchain consumed by mediasoup's native compile. The Dockerfile and publish workflow source live at `infrastructure/docker/buildtools/Dockerfile` and `.github/workflows/publish-buildtools-base.yml` respectively. (Historical: published as `ghcr.io/markdrogersjr/node-buildtools` from PR #1166 through PR #1348's namespace migration; both namespaces remain readable, publisher target moved to the org in #1348.)

**Refresh procedure:** see `docs/runbooks/refresh-buildtools-base.md`. Quarterly cadence OR alpine/python3/g++/linux-headers CVE event; 7-day SLO for critical CVEs.

**Owner:** repo admin (currently @markdrogersjr).

**SHA-pin requirement:** the builder stage MUST reference the GHCR image by manifest digest (`@sha256:...`), not by floating tag. The date-stamped tag (`<node-tag>-<DATE>`) is for human readability only; the digest is the authoritative supply-chain handle.

**`<node-tag>` is DERIVED, never hardcoded (post-#3172).** The publish workflow's `Compute date tag` step reads it from the `FROM node:<tag>` line in `infrastructure/docker/buildtools/Dockerfile` and refuses to publish under an empty prefix. It was the literal string `24-alpine` until dependabot bumped that parent to `node:26-alpine` on 2026-09-08, at which point every subsequent refresh would have stamped a Node 26 image with a tag naming Node 24. **Nothing would have broken** — consumers pin by digest and the tag is decorative at build time — which is exactly why the drift was invisible and survived. The class of defect is an operator reading a truthful-looking tag; deriving the prefix closes it at the source rather than by remembering to update a second place. Aligns with `.claude/rules/electron.md` (in Desktop) §"Supply-chain hardening".

**Runtime base pin (post-#1922):** the final-stage `node:26-alpine` base is independently pinned to its top-level multi-platform digest. Re-resolve it during every buildtools refresh and update it when the reviewed upstream digest changes; a rebuild alone cannot advance a digest-pinned runtime. Node/Alpine CVE responses MUST verify and update this runtime pin in the same consumer PR as the builder pin. Confirm the publisher used that upstream digest, verify amd64 and arm64 coverage, and build/smoke the full final image before merge. A floating runtime tag is prohibited because a deploy rebuild may otherwise reuse a cached pre-security base after the builder pin has moved. The general upstream-image rule in `.claude/rules/electron.md` (in Desktop) now requires the same tag-plus-digest pin for every deploy-path base; what stays media-plane-specific is the lockstep with the builder pin above. The paired procedure is canonical in the buildtools refresh runbook.

**CI age-guard:** the buildtools age-check step in the `dev-checks` job in `.github/workflows/unit-smoke.yml` (moved from build.yml by #1475; consolidated into `dev-checks` by #2065) emits a `::warning::` annotation when the pinned tag's date is >100 days old. The staleness annotation is informational only — it does NOT fail the build — but an *unparseable* tag is a hard `exit 1`, deliberately, so a tag-format change cannot turn the guard into a silent no-op. Its parser keys on the tag's trailing `YYYY-MM-DD` and is agnostic to the leading `<node-tag>`, so a Node-major bump moves the prefix without tripping it. Operator should respond by dispatching the publish workflow per the runbook. The 100-day threshold gives a 10-day grace past the 90-day quarterly cadence.

**Publish workflow:** `.github/workflows/publish-buildtools-base.yml` is `workflow_dispatch` only (multi-arch amd64+arm64). The workflow smoke-tests the published image and surfaces the manifest digest in the workflow summary for capture into consumer PR bodies.

**Founding incident:** #1166 — Phase 2 implementation child of CI speedup epic #1109, per investigation report `docs/superpowers/reports/2026-05-22-1111-media-plane-docker-investigation.md` §5.

## Production Guards
- JWT secret must have a production guard (fatal exit if default in production)
- New config with dev defaults must follow the same pattern

## NATS & Redis
- NATS messages must be structured and versioned
- Redis operations handle connection failures gracefully (degrade, don't crash)
- Clean up Redis entries on disconnect/crash

### Voice enforcement subjects (control-plane → media-plane)

The control-plane publishes voice-enforcement commands the media-plane consumes:

- `voice.enforce.mute` / `voice.enforce.deafen` — server-mute/deafen a participant (#488).
- `voice.enforce.permissions` — **mid-session permission push** (CV-CAN-007 review P1). Payload `{channelId, userId, permissions}` where `permissions` is the decimal-string effective bitfield (same wire format as the join-authorize `permissions` field, parsed by the same strict fail-closed parser exported from `middleware/auth.ts`). Published by `voice.PermissionEnforcer` (`services/control-plane/internal/voice/permission_enforcer.go`), which the RBAC mutation handlers invoke — right after their `PermissionCache` invalidation, so the re-resolve computes post-mutation state — via the narrow `rbac.VoiceEnforcer` interface (role edit/delete → server scope; role assign/unassign → user scope; channel/category override changes → channel scope). A recheck that resolves `rbac.ErrNotMember` publishes `voice.enforce.disconnect` instead (membership loss ⇒ eviction, ADR-0023 rail); any other resolution failure also disconnects fail closed unless the enforcer context was canceled during shutdown. The consumer (`services/media-plane/src/lib/enforcePermissions.ts` → `handlePermissionsUpdate`) replaces `Participant.permissions` via `RoomManager.updateParticipantPermissions` (a structural no-op for DM rooms — never bolt a permission model onto a room that has none), closes now-forbidden producers via `RoomManager.closeForbiddenProducers` (each close rides the normal `producer-removed` → `producer-closed` fan-out), and emits `permissions-changed` to the affected peer so its UI can stop local capture. Grants propagate too — the snapshot is replaced, not intersected, so a newly granted member publishes without rejoining. Four hardening invariants (adversarial-review confirmed, do NOT remove): (1) the enforcer serializes resolve+publish under `publishMu` so publish order equals resolve order — without it a recheck that resolved before a later mutation could publish after it, re-raising a just-revoked bitfield (single-instance guarantee; multi-instance needs a cross-instance sequence); (2) the resolve is `ResolveEffectivePermissionsFresh` (cache-READ-bypassing) — a concurrently in-flight pre-mutation compute can repopulate the cache after the mutation's invalidation, and publishing that stale entry would be sticky fail-open; (3) `produce()` re-checks `publishPermitted` AFTER the mediasoup await registers the producer — a push landing mid-await cannot see an unregistered producer, and the client controls produce() timing (TOCTOU); (4) the `voice.joined` bridge re-pushes via `RecheckParticipant` after recording presence, closing the join-vs-mutation sweep race. Ownership transfer/reversal (the largest single permission delta — owner short-circuit) also rechecks both parties via the same enforcer.
- **`media_policy` is not one of these subjects.** The #2153 media-rate policer's
  repeat-offender eviction is decided and enforced **entirely locally** to the media plane (a
  latch, then a close plus in-process `handleForceDisconnect`, then a userId-keyed TTL cooldown) —
  it publishes no NATS subject and the control plane is not involved in the decision. Its call into
  `handleForceDisconnect` passes `{reason:'media_policy', retryAfterSec}`, a **local, in-process**
  call, not a NATS-triggered one. The NATS-triggered force-disconnect below keeps its existing
  `{reason:'access_revoked'}` payload unchanged — `media_policy` never crosses the NATS boundary.
- `voice.enforce.disconnect` — **force-disconnect** a participant from a room (#487 P3). Payload `{channelId, userId, callId?}`. The consumer (`services/media-plane/src/lib/forceDisconnect.ts` → `handleForceDisconnect`) resolves both admitted and provisional DM socket IDs for the user, removes the exact provisional candidate, runs exact admitted teardown for the captured admitted socket, and then force-closes each captured live socket. Do NOT recreate transport teardown or provisional lifecycle: RoomManager remains authoritative, admitted removal auto-publishes `voice.left`, and candidate removal emits no `user-left`. Its enforcement-specific seam closes a newly provisional-only room silently, but emits the normal terminal `room-empty` when admitted history exists and the old admitted socket already left. This dual lookup is required for committed DM membership removal during A1→A2; disconnecting only the admitted reconnect socket would leave its pending successor promotable by an already-issued A2 response. The provisional-removal-first and admitted-teardown-before-force-close ordering is required because Socket.IO disconnect cleanup can run synchronously: force-closing first would erase exact ownership and suppress terminal closure or telemetry. Other disconnect sources include temporary-SBAC revocation, rejected users reported by DM heartbeats, and mutation-triggered permission-resolution failures. See [ADR-0023](../../../docs/adr/0023-temporary-sbac-and-move-hierarchy-exception.md) and the convergence point `revokeTemporaryChannelAccess` in `services/control-plane/internal/voice/temp_grants.go`.

  A present `callId` must be a canonical UUID and limits this command to the matching DM call: compare admitted `Room.callId` and the provisional candidate's stored call ID before teardown. A stale call's command is a no-op. An absent `callId` retains broad membership and permission revocation. Deploy this consumer before any control-plane publisher adds `callId`; an older media consumer discards the unknown field and would evict a successor call.
- `voice.enforce.disconnect.ack` and `voice.enforce.credential_epoch.ack` — authenticated request/reply variants used by durable reconciliation. Requests and acknowledgements are versioned and carry domain-separated HMAC proofs; invalid, stale, or replayed messages fail closed without teardown. Credential-epoch enforcement targets only sessions whose JWT-carried epoch exactly matches the superseded epoch, so a newer login is never disconnected by an older obligation. The control plane may remove durable evidence only after verifying the signed success reply.

### Tier-violation subject (media-plane → control-plane, #1308)

`voice.tier_violation` reports one mic the audio tier gate refused. Payload `{userId, reason,
timestamp}` and nothing else: `reason` is one of `audio_ptime_below_tier` or
`audio_bitrate_over_tier`, never the gate's own four strings, and the room, the rtpParameters
and every media detail stay here. The control plane bounds this payload at 512 bytes BEFORE it
parses it, so a new field means raising that bound in the same change. It is not a lifecycle
event: the control plane takes it on its own queue-group subscription, so one replica counts
each report, and drops the copy its `voice.*` wildcard also receives. Its door (per-user and
global budgets) leads to the entitlement violation recorder, which escalates on the fifth report in a fixed hour with a resync push and
one security event, never a disconnect. Like every `voice.*` publish it is unsigned on an
unauthenticated bus (#2857); the forge ceiling is that resync and event once per user-window.

### DM call completion subject (media-plane → control-plane)

The media plane publishes `voice.room_empty` after the last admitted participant leaves a DM room. Its terminal snapshot includes `{channelId, callId, ringId?, callerUserId, participantUserIds, startedAt, timestamp}`. An A1-only candidate is never a participant for this purpose: a provisional-only room has no call history, emits no heartbeat, and closes without `voice.room_empty`. The first successful A2 promotion locks `callId`, optional `ringId`, and `callerUserId` for the room lifetime; later joins MUST NOT replace them, participant history MUST survive individual leaves, and a terminal call ID MUST be briefly tombstoned so a delayed join cannot recreate the room. Ring-backed joins use the control-plane-authorized ring ID; a direct renderer `/voice/join` reserves the control-plane-generated call ID before room creation. The media-plane `/voice/authorize` hop only resolves that lease and marks the handoff admitted after verifying its short-lived, domain-separated HMAC proof; it never creates or refreshes the lease, and an ordinary member request MUST NOT be able to set the admission marker. DM authorization/admission/rollback is serialized per room and socket-bound: one socket may have only one in-flight or admitted voice room, both proof-bearing control-plane hops are deadline-bounded, and disconnect before/during admission rolls back only a provisional participant still owned by that socket. An empty unadmitted room calls HMAC-bound `DELETE /voice/authorize` to tombstone only the exact short, ringless reservation and is discarded without publishing a completed call; once participant admission has recorded call history, exact rollback follows the normal terminal lifecycle instead. `voice.joined` and non-empty `voice.heartbeat` renew the exact shared Redis call lease; `voice.room_empty` and exact empty heartbeats tombstone and clear only that exact lease. Control-plane cleanup holds the Redis conversation guard through deadline-bounded participant deletion. The control-plane uses `callId` as the completed call-event message ID, so duplicate terminal delivery is idempotent and a later rich snapshot upgrades the same fallback row. Do not reconstruct authoritative completed-call identity from live `dm_voice_participants`: those rows may already be deleted before `voice.room_empty` is consumed.

The control-plane may renew that lease for `voice.joined` or a non-empty
`voice.heartbeat` only after each accepted sender is proven to remain a current
`dm_participants` member under a transaction-held membership lock. A rejected
join creates or refreshes no lease, lifecycle hash, participant row, activity,
or roster frame. An all-nonmember heartbeat may reconcile exact current ghost
rows only against an already-existing matching lease; it must never create or
extend a lease, and a stale heartbeat must not erase a newer legitimate
generation.

Private Call activity is participant-set state, not a collection of independent
sender writes. An accepted join, leave, or heartbeat must atomically claim one
exact token/version/active lifecycle batch for every affected current or
removed sender, then advance `lifecycle_event_at` on every remaining affected
row under deterministic conversation/sender advisory locks. Reject the whole
batch when any Redis generation or PostgreSQL row is newer. This applies across
conversation moves too: advance both the old remaining set and the new set, and
never overwrite a peer whose old-scope lifecycle already belongs to a successor.
Timestamp qualification precedes token/membership classification because a
90-second Redis fence may expire while the PostgreSQL watermark remains.

`voice.joined`, `voice.left`, `voice.room_empty`, and `voice.heartbeat` MUST share one control-plane `voice.*` wildcard ingress and then enter the bounded keyed dispatcher. The dispatcher preserves cross-subject order per `channelId`, processes independent rooms concurrently, and may coalesce only adjacent pending heartbeats for the same room; it must never coalesce across a joined/left/room-empty boundary. Ingress never waits on a saturated queue or its recovery: the event is dropped, recovery logging plus a conservative disconnect of all local Rich Presence clients is coalesced onto a dedicated worker, and unrelated rooms continue. Shutdown discards pending events and joins only bounded in-flight handlers and recovery. Their DM WebSocket deltas MUST likewise share `BroadcastToDMParticipants` so the Hub cannot reorder room-scoped delivery. Splitting a room across lanes can let a stale join or heartbeat overtake `room_empty` and resurrect a completed call's roster.
