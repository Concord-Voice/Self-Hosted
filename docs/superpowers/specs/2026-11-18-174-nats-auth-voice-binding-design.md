# Security#174 — Authenticate NATS and bind control-plane `voice.*` consumers to server state

**Status:** Final spec (architect Mode 3, post-panel reconciliation). Base: `main` `b23fe7d7`.
**Tracking:** Security#174 (epic Security#153). Shares the MAC module and the broker matrix with Security#175.
**Risk:** breaking change. Every install needs bus credentials before the stack starts. `concord-selfhost up` creates them.
**Owner confirmation:** the §11.1 deviations and migration 000198 were approved by the project lead on 2026-11-18.

## 1. Problem

The broker (`infrastructure/docker/nats/nats-cluster.conf`) has no `authorization` or `tls` block. The base `docker-compose.yml` publishes 4222 and 8222 on every interface. All three clients connect anonymously: the control plane (CP) and `cmd/ops-agent` through `pkg/nats/client.go`, and the media plane (MP) through `src/lib/nats.ts`.

The CP lifecycle consumers in `internal/voice/nats.go` treat bus position as proof of origin. One forged publish can:

- plant a participant row (VULN-109) or rewrite a roster (VULN-112);
- remove a participant (VULN-110) or clear a channel (VULN-209);
- write DM call history (VULN-111);
- raise a victim's tier-violation counter (VULN-113).

Two handler defects turn an event that changes nothing into a replica-wide WebSocket disconnect (VULN-108, VULN-208).

## 2. Goals and non-goals

**Goals**

1. Every bus client authenticates with its own nkey over mutual TLS. Anonymous and plaintext connections are refused.
2. Each identity can publish and subscribe only on its row of the matrix (§5.2), which includes the JetStream, KV and enforcement subjects Security#175 needs.
3. Every lifecycle and tier-violation event carries an origin MAC, verified in the ingress gate before any handler runs.
4. Each lifecycle write is bound to server-side state: admission, call lease, membership, or media boot.
5. Every refusal this change introduces is a determined no-op. None reaches `disconnectAllRichPresenceClients`.
6. An existing install upgrades with no manual credential step.

**Non-goals**

- Operator/account JWT mode, and any anonymous, legacy or test-only insecure mode (owner decisions).
- MAC, sequence binding and owner-record work on enforcement commands (Security#175).
- Enabling room placement on self-host.
- The fail-closed `disconnectAllRichPresenceClients` arms that remain reachable by load, including the DM `room_empty` arm behind the call-lease check (Security#57).

## 3. Binding decisions and constraints

**Owner decisions (2026-11-18, not reopened)**

- One static nkey user per service (control plane, media plane, ops-agent) with allow-lists. mTLS from an installer-generated internal CA, `verify: true`. The nkey is the only identity.
- A new dedicated MAC secret, not derived from `JWT_SECRET`: one Go and one TS HMAC-SHA256 module, per-direction and per-family subkeys, key ID.
- No legacy mode. Clients fail closed. A bare `compose up` without credentials stops with a clear error.
- CP and MP upgrade together. The full broker matrix lands in this PR.
- `voice.user_mute`, `voice.user_deafen` and `voice.enforce.deafen` are **kept and signed, not retired**. This PR grants them in the matrix (publish: CP only; subscribe: MP only). Their MAC check lands with the `cp>mp/enforce` family in Security#175 through the shared `voice.enforce.mute` handler path. Between the two merges they are broker-authenticated only.

**Rule files**

| Rule file | Invariant this change touches |
|---|---|
| `.claude/rules/domains/backend.md` | Admission-pair replay rule (unchanged). The text allowing a heartbeat insert with no admission is rewritten for the claim rule (§5.6). |
| `.claude/rules/domains/backend.md` | The time reference is NATS receipt time, never a handler's `time.Now()`. The MAC skew check and the lease test both use it. |
| `.claude/rules/domains/backend.md` | The AST log guard over `nats.go` admits only `failure_class`, `action`, `is_dm`, `context`, `count`. Shed and metric emission stays in `ingress_gate.go`. |
| `.claude/rules/domains/media-plane.md` | Initial NATS failure propagates. Logs carry fixed classifications, never subjects or raw errors. The owner-record no-repair rule stays. |
| `.claude/rules/domains/migrations.md`, `.claude/rules/migrations.md` | Migration 000198 (§5.5). |
| `.claude/rules/domains/tests.md` | Real JetStream tests use the shared pinned broker resolver. `internal/voice` and `internal/rbac` share `voice.enforce.*`. |

**Dependency contracts (Context7, NATS docs, read 2026-11-18)**

- `tls { verify: true }` requires a client certificate with the client-auth extended key usage.
- `allow_responses: true` equals `{ max: 1 }` with no time limit. It implicitly denies other publishes unless they are allowed explicitly, and the reply subject is not constrained by the publish allow-list.
- A `$VAR` reference resolves from the environment only when it is not quoted. An unresolved reference stops the server at startup. A value that begins with a digit may not resolve on some server versions. Every value injected here begins with `U` (public nkey) or `/` (path).
- `http: <host>:<port>` binds monitoring to one address. The `-m` flag sets the monitoring port.
- Not confirmed by the fetched pages: the `tls` key that pins a minimum protocol version on the pinned `nats:2.15.2` server. T4 confirms it with `nats-server -t`.

**Exploit-test compatibility.** The exploit tests are compiled into `internal/voice`, `pkg/nats` and `internal/websocket` with `go test -overlay`. A compile error in one file hides every result in that package, so these stay stable:

- `pkg/nats.Connect(url string) (*Client, error)`. The credential options (`nats.Nkey`, `nats.Secure`) are written in `pkg/nats/client.go` itself, because one test greps that file.
- `gateVoiceLifecycle(func(string, []byte)) func(string, []byte)`, `admitTierViolation([]byte)`, `SetTierViolationRecorder`, `NewNATSSubscriber` (7 arguments), `handleJoined(data, receivedAt)`, `handleRoomEmpty(data, receivedAt)`.
- `parseVoiceJoinedAdmission(voiceJoinedEvent)`, `moveServerVoiceParticipant(ctx, tx, channelID, senderID, eventAt, admission)`, `mutateServerVoiceParticipant(ctx, channelID, senderID, eventAt, admission)` (4 results), `persistDMRoomEmptySummary(ctx, event, conversationID, eventAt) (bool, error)`, `lockVoiceParticipantUsersTx(ctx, tx, ids, mode) error`, `parseSortedVoiceParticipantIDs`.
- Fields `log`, `db`, `redis`, `voiceRoomBudget`, `tierViolationUserBudget`, `tierViolationBudget`, `disconnectAllRichPresenceClientsHook`, and the in-repo test helpers `recordingEntitlementPusher`, `recordingSecurityEmitter`, `promptTier`.
- A `NATSSubscriber` built as a struct literal with only `log` and `voiceRoomBudget` must shed frames. It must not panic on a nil keyring or a nil new field.

## 4. Reconciliation record

| # | Conflict | Winner | Why |
|---|---|---|---|
| 1 | Key generation: Go helper (K1) or host `openssl` plus Python (K2) | System-engineer: K2 on installs | Verified: `up` is `require_security_log_boundary && guarded_ctl rebuild && authority_lifecycle up`, and `rebuild` builds the images. Provisioning must run before that. K1 would force a build/start split in `concord-ctl.sh`. Architect addition: dev and CI use a Go generator so they do not depend on the host `openssl` build. A cross-check test ties the two. |
| 2 | Meter `voice.room_empty` | System-engineer: not metered | The G2 note in `ingress_gate.go` records that a shed `room_empty` is terminal, and a DM `room_empty` carries the call summary. After origin proof and the VULN-208 fix a no-op costs one indexed read. This deviates from the issue text (§11.1). |
| 3 | Meter `voice.joined` | System-engineer, with a larger burst | Own per-room bucket, placed after the MAC and after the handled-subject check. Burst raised from 1x to 2x `maxServerVoiceParticipantIDs` because the same G2 note measured an honest mass rejoin exceeding a 1x-ceiling burst. |
| 4 | "Retired boot" as a boot record or a per-row lease | System-engineer on mechanism, architect on the window | Boot-fence retirement can lag without bound, so a per-row lease on `lifecycle_observed_at` is right. The proposed 30 s window is rejected: the MP heartbeat runs every 30 s (`services/media-plane/src/index.ts:2444`), so a live boot's rows would look expired between two heartbeats. The window is `presence.ActivityStateTTL` (90 s), the lease the stale sweep already uses. |
| 5 | Replacement for the no-op `room_empty` disconnect | System-engineer: tombstone | The DM arm's comment confirms the Hub is replica-local, so a peer-removed roster needs the reconnect. Architect addition: the tombstone records which replica wrote it, so a replica never treats its own removal as a peer's. |
| 6 | Boot ID in an envelope field or in the signed body | System-engineer: body | Handlers keep `(data, receivedAt)`, which the exploit tests call, and the MAC still covers the boot. |
| 7 | Matrix details | System-engineer | Traced call by call: MP KV reads use `STREAM.MSG.GET`; CP needs `CONSUMER.INFO` and `STREAM.PURGE`, no `CONSUMER.CREATE`, no `allow_responses`; no `dm.*` or `media.*` bus subjects exist. |
| 8 | Commit order | System-engineer, one change | Clients, broker config, Compose, test helper and CI broker land in one commit. Architect change: the MP starts sending `nodeBootId` in T2, not T6, so every commit between them stays green. |
| 9 | `media_boot_id` on `dm_voice_participants` | Architect: dropped | That table has no `lifecycle_observed_at`, so the lease rule cannot apply to it. DM `voice.left` and `room_empty` already run `dmCallEventMayMutateLiveState`. |
| 10 | Permanently `NOT VALID` CHECK | Architect: a normal validated CHECK | Every existing row is NULL and the table holds only live participants. `migrations.md` already has the precedent of not splitting when no violating row can exist. |
| 11 | Where "admission is mandatory" is enforced | Architect: server-join handler and the write sink | `parseVoiceJoinedAdmission` receives only the event and cannot tell a DM join from a server join. One exploit test calls `mutateServerVoiceParticipant` directly, so the rule must hold at the sink. |
| 12 | Unknown user in a heartbeat | Both, refined | `lockVoiceParticipantUsersTx` still returns an error (an exploit control test requires it). The error is typed and carries the missing IDs, and callers drop per user. |
| 13 | "CP decoders are strict" | Corrected | The CP lifecycle decoders use plain `json.Unmarshal`. The envelope and broker auth are what force the joint upgrade. |

Upheld from the design philosophy without dispute: body-wrapper MAC transport, certificate lifetimes, ops-agent on the Docker network only, migration 000198 over a session join, per-identity inbox prefixes, monitoring on container loopback.

## 5. Design

### 5.1 Identity and transport

- `nats-cluster.conf` gains `authorization { timeout: 2, users = [...] }` with three nkey users and `tls { cert_file, key_file, ca_file, verify: true, timeout: 2 }` plus a TLS 1.3 minimum (key confirmed in T4). No `no_auth_user`, no `accounts` block.
- Public nkeys and TLS paths are unquoted `$VAR` references. The committed file stays the single source of the matrix, and the conformance suite runs it verbatim.
- Monitoring becomes `http: "127.0.0.1:8222"`. `-m 8222` is removed from the base and dev commands. The healthcheck URL changes to `http://127.0.0.1:8222/healthz`, because `localhost` can resolve to `::1` in the Alpine image.
- The base Compose file publishes neither 4222 nor 8222. `docker-compose.dev.yml` publishes `127.0.0.1:4222:4222` only.
- ops-agent stays a Compose service on `concordvoice-services`. Its identity is publish-only on `ops.metrics.host.v1`. The Compose comment saying it receives no credentials is rewritten.
- Client URLs become `tls://nats:4222`.

**Go client.** `Connect(url)` calls `ConnectWithIdentity(url, IdentityControlPlane, CredentialsFromEnv())`. `cmd/ops-agent` uses `IdentityOpsAgent`. Before dialing it returns a configuration error when the scheme is not `tls`, when `NATS_NKEY_SEED`, `NATS_TLS_CA_FILE`, `NATS_TLS_CERT_FILE` or `NATS_TLS_KEY_FILE` is missing or unreadable, when the seed is not a user seed, or when certificate and key do not pair. It sets `nats.Nkey(pub, kp.Sign)`, `nats.Secure` with `MinVersion: TLS13` and the internal CA as the only root, and `nats.CustomInboxPrefix("_INBOX.cp")` or `"_INBOX.ops"`. `RetryOnFailedConnect(true)` stays. `nkeys` becomes a direct dependency. The `nats://localhost:4222` defaults are removed.

**TS client.** `connect()` gains `authenticator: nkeyAuthenticator(...)`, `tls: { caFile, certFile, keyFile }` and `inboxPrefix: '_INBOX.mp'`. Config validation rejects a non-`tls://` URL or any missing input. The error propagates as an initial NATS failure.

### 5.2 Permission matrix (shipped in `nats-cluster.conf`)

Anything not listed is denied. Entries marked † stay only if the conformance trace shows the client sends them.

| Identity | Publish | Subscribe | `allow_responses` |
|---|---|---|---|
| control-plane | `voice.enforce.>`, `voice.user_mute`, `voice.user_deafen`, `voice.admission.activate`, `voice.commands.v1`, `presence.erasure.cleared.v2`, `attestation.registry.updated`, `attestation.registry.revoked`, `$KV.VOICE_COMMAND_OWNER_V1.authoritative` | `voice.*`, `voice.tier_violation control-plane.tier-violation`, `presence.erasure.cleared.v2`, `ops.metrics.host.v1`, `ops.metrics.media.v1`, `attestation.registry.updated`, `attestation.registry.revoked`, `_INBOX.cp.>` | not set |
| control-plane, JetStream API | `$JS.API.STREAM.INFO.` and `STREAM.CREATE.` for `VOICE_COMMANDS_V1` and `KV_VOICE_COMMAND_OWNER_V1`; `STREAM.PURGE.VOICE_COMMANDS_V1`; `STREAM.MSG.GET.VOICE_COMMANDS_V1`; `DIRECT.GET.KV_VOICE_COMMAND_OWNER_V1` and `.>`; `CONSUMER.INFO.VOICE_COMMANDS_V1.voice-enforcement-v1`; † `STREAM.MSG.GET.KV_VOICE_COMMAND_OWNER_V1`; † `$JS.API.INFO` | | |
| media-plane | `voice.joined`, `voice.left`, `voice.room_empty`, `voice.heartbeat`, `voice.tier_violation`, `voice.producer_added`, `voice.producer_removed`, `ops.metrics.media.v1`, `$KV.VOICE_COMMAND_OWNER_V1.authoritative`, `$JS.ACK.VOICE_COMMANDS_V1.voice-enforcement-v1.>` | `voice.enforce.>`, `voice.user_mute`, `voice.user_deafen`, `voice.admission.activate`, `_INBOX.mp.>` | `true` |
| media-plane, JetStream API | `$JS.API.INFO`; `STREAM.INFO.` for both streams; `STREAM.MSG.GET.KV_VOICE_COMMAND_OWNER_V1`; `CONSUMER.INFO.`, `CONSUMER.MSG.NEXT.` and `CONSUMER.CREATE.` for `VOICE_COMMANDS_V1.voice-enforcement-v1` (create with filter `voice.commands.v1`); † `CONSUMER.DURABLE.CREATE.VOICE_COMMANDS_V1.voice-enforcement-v1` | | |
| ops-agent | `ops.metrics.host.v1` | deny `>` | not set |

Denied by omission: stream delete and update, consumer delete, any other stream, purge for the MP, lifecycle and tier-violation publishes for the CP, and every `voice.enforce.*`, `voice.commands.v1`, `voice.user_*`, `voice.admission.activate`, `presence.*` and `attestation.*` publish for the MP. Request/reply runs one way: the CP requests on `voice.enforce.session.<boot>`, `voice.enforce.disconnect.ack` and `voice.enforce.credential_epoch.ack`, and the MP answers through `allow_responses`.

### 5.3 Bus MAC module and envelope

**Modules.** Go: `services/control-plane/pkg/busmac`. TS: `services/media-plane/src/lib/busMac.ts`. Shared vectors: `services/control-plane/pkg/busmac/testdata/vectors.json`, read by both suites.

```go
type Family string // FamilyLifecycle = "mp>cp/lifecycle", FamilyTelemetry = "mp>cp/telemetry"
func ParseKeyring(spec string) (*Keyring, error)
func (k *Keyring) Sign(f Family, subject string, body []byte, now time.Time) ([]byte, error)
func (k *Keyring) Open(f Family, subject string, envelope []byte, receivedAt time.Time) ([]byte, error)
func (k *Keyring) MAC(f Family, subject string, tsMillis int64, body []byte) (kid string, mac []byte, err error)
func (k *Keyring) VerifyMAC(f Family, kid, subject string, tsMillis int64, body, mac []byte) bool
```

The TS module mirrors these. Security#175 registers its `cp>mp/*` and owner families in the same table and may use the detached `MAC`/`VerifyMAC` for headers.

**Keyring.** `BUS_MAC_KEYRING="<kid>:<base64url, at least 32 bytes>[,<kid>:<key>...]"`. A kid matches `^[a-z0-9]{1,16}$` and is unique. The first entry signs, any entry verifies.

**Envelope.** One JSON object with exactly `{"v":1,"kid":"...","ts":<int ms>,"mac":"<base64url, no padding>","body":"<inner JSON text>"}`. Unknown keys are refused.

**MAC.**

- `subkey = HMAC-SHA256(root[kid], "concord-bus-mac/v1/" + family)`.
- `mac = HMAC-SHA256(subkey, "concord-bus-mac/v1\n" + family + "\n" + kid + "\n" + subject + "\n" + ts + "\n" + hex(sha256(bodyBytes)))`.
- `bodyBytes` is the UTF-8 encoding of the `body` string after the envelope is decoded. The sender signs the output of `JSON.stringify(payload)`.
- `subject` is the NATS message subject, never a payload value.
- A newline or NUL in `kid`, `family` or `subject` is refused. Comparison is constant-time. A nil or empty keyring, an unknown kid, or an unregistered family never verifies.

**Skew.** Accept `receivedAt - 60 s <= ts <= receivedAt + 5 s`, where `receivedAt` is taken once at NATS callback time. There is no nonce cache: lifecycle watermarks and single-use admissions already fence replays.

**MP publish path.** New `publishSigned(subject, family, payload)` builds the body, signs and publishes with no `await` in between. `publish()` refuses the five signed subjects. `voice.producer_*` and `ops.metrics.media.v1` stay unsigned. Signing happens at publish time: `publish()` already drops frames while disconnected.

**Inner body.** `voice.joined`, `voice.left`, `voice.room_empty` and `voice.heartbeat` gain `"nodeBootId":"<uuid>"`, the MP's per-process `voiceEnforcementNodeBootId`.

### 5.4 Ingress gate

`SetBusMACKeyring(*busmac.Keyring)` injects the keyring. `Subscribe()` returns an error when it is nil, so the CP fails at boot.

Order in `gateVoiceLifecycle`:

1. Drop `voice.tier_violation` by subject (unchanged).
2. Size bound, 128 KB (unchanged).
3. Handled-subject check. An unsigned `voice.producer_*` frame sheds as `unhandled_subject` and is never an origin failure.
4. `Open(lifecycle, subject, data, receivedAt)`. A failure sheds as `origin_proof` through the existing interval reporter. No subject, raw error, kid or MAC is logged.
5. Decode `channelId` and `nodeBootId` from the inner body. Missing or nil sheds as `invalid_event`.
6. Meter per room. Heartbeat: unchanged. `voice.joined`: its own bucket, burst `2 * maxServerVoiceParticipantIDs`, refill `maxServerVoiceParticipantIDs` per 10 s. `voice.left` and `voice.room_empty`: not metered.
7. Dispatch the inner body.

The MAC check runs before the meter on purpose. A forged room key can no longer become resident in the budget table. The G2 comment is rewritten to say so.

`admitTierViolation`: envelope bound 1024 bytes, then `Open(telemetry, "voice.tier_violation", ...)`, then the existing 512-byte inner bound, parse, meter and record. A failed proof sheds as `origin_proof` and records nothing.

### 5.5 Data model: migration 000198 `add_voice_media_boot_binding`

| Change | Detail |
|---|---|
| `voice_participants.media_boot_id UUID` | Nullable, no default. NULL means "written before this change" and is never backfilled. Validated `CHECK (media_boot_id IS NULL OR media_boot_id <> '00000000-0000-0000-0000-000000000000')`. |
| New table `server_voice_room_terminals` | `channel_id UUID PRIMARY KEY REFERENCES channels(id) ON DELETE CASCADE`, `media_boot_id UUID NOT NULL`, `recorded_by UUID NOT NULL`, `removed_count INTEGER NOT NULL CHECK (removed_count > 0)`, `recorded_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()`. At most one row per channel (upsert). |

- Runs under `SET lock_timeout = '5s'`. Heartbeats write `voice_participants` constantly, so a failed attempt must retry, not queue writers.
- No new index. The down migration drops the table, the constraint and the column.
- `dm_voice_participants` is not changed.

### 5.6 Handler binding

**Origin.** Each lifecycle handler puts the decoded `nodeBootId` on the context under its own key. It is not folded into the placement context value, which is set only when placement is enabled. With no origin on the context, the write sink writes nothing and returns `applied=false`. All callers of the sink are lifecycle paths (verified).

**Lease.** A row is *expired* when `lifecycle_observed_at < receivedAt - presence.ActivityStateTTL` (90 s). The existing trigger renews that column only when `lifecycle_event_at` is written.

**Claim rule** (who may insert or refresh a server row, for an event from boot X):

| Event | Row | Result |
|---|---|---|
| `voice.joined` with a consumed admission | any | Upsert, stamp X. |
| `voice.joined` with no admission (server room) | any | Rejected in the server-join handler. No row. |
| Heartbeat names the user | stamped X | Refresh. |
| Heartbeat names the user | none, NULL, another boot, or expired | Resolve `server_members` and `CONNECT` in the same transaction. Pass: write and stamp X. Fail: leave untouched, `failure_class=claim_refused`. |

The membership row is locked `FOR SHARE` inside `moveServerVoiceParticipant`. `CONNECT` is resolved in `mutateServerVoiceParticipant` with the resolver the admission-create path uses. A nil resolver means no write. The foreign-row case is expressed in the upsert's `ON CONFLICT ... DO UPDATE ... WHERE` predicate, so an untouched row does not renew its lease.

**Removal rule** (`voice.left`, `voice.room_empty`, heartbeat omission, from boot X): a row is removed only when it is stamped X, NULL, or expired. A live row of another boot is left alone, logged once with `action=foreign_boot` and counted. Where placement is enabled its guard still runs first.

This fences events from a predecessor or peer boot, for example a restarted MP's late `room_empty` arriving after users rejoined. It is not a defence against a leaked MP credential plus MAC key, whose holder can name any boot.

**Per finding**

- **VULN-109.** `parseVoiceJoinedAdmission` keeps its behaviour. The server-join handler rejects a nil admission. The sink always consumes a supplied admission. The `priorAdmitted` replay rule is unchanged.
- **VULN-112.** The claim rule above.
- **VULN-110, VULN-209.** The removal rule above.
- **VULN-111.** `dmCallEventMayMutateLiveState` runs before `persistDMRoomEmptySummary`, under the existing `LockDMCallLifecycle`. `insertCompletedCallEvent` requires the caller and every listed participant to be `dm_participants` rows in its transaction. A mismatch writes nothing and logs `action=participant_mismatch`.
- **VULN-108.** `lockVoiceParticipantUsersTx` returns a typed error that wraps `errVoiceParticipantUserMissing` and carries the missing IDs (never logged). The joined handler drops the event. The heartbeat path drops only the missing users and applies the rest. `runRefreshMutation` and `activity_service.go` match it through an exported `presence.ErrActivityUserMissing`. Each logs `failure_class=user_missing` with a count.
- **VULN-208.** In `handleResolvedRoomEmpty`:
  - Rows removed: in the same transaction, after the deletes, upsert the tombstone with this replica's process ID in `recorded_by`. `databaseEmpty` means no row of any boot remains.
  - `databaseEmpty && !removedAny`: read for a tombstone on the channel with `recorded_by` different from this replica and `recorded_at` within `ActivityStateTTL + 30 s`. Found: a peer replica removed the rows, keep the disconnect. Not found: determined no-op, log `action=noop_room_empty`, skip the broadcast, no disconnect. Read error: fail closed with the existing disconnect.
  - The tombstone commits with the peer's deletes, and this replica can only observe "empty" after that commit, so the read cannot miss it.
- **VULN-113.** Closed by the broker identity plus the telemetry MAC.

**No new amplifier.** `user_missing`, `claim_refused`, `foreign_boot`, missing admission, `participant_mismatch`, `noop_room_empty` and `origin_proof` are all determined no-ops. None may fall into an `activityErr != nil` arm that disconnects. Each has a test asserting a hook count of 0.

### 5.7 Provisioning and rollout

**One PR, one synchronized stack.** Broker, CP and MP change together. Frames lost while Compose recreates the services converge through the 90 s stale sweep and the claim rule.

**Where it runs.** `selfhost-update.sh` runs under the previous release's shell and then calls the new tag's `concord-selfhost up`. The backfill therefore lives in the new `up`, before the build:

`up) require_security_log_boundary && nats_credentials_ensure && guarded_ctl rebuild && authority_lifecycle up`

`install-selfhost.sh` calls the same `ensure` after it writes `.env`.

**Generators.**

- Installs: `infrastructure/deploy/selfhost-nats-credentials.sh ensure` uses `openssl` (Ed25519 keys, P-256 CA and leaves) and a stdlib helper `selfhost-nats-keys.py` for nkey encoding. `ensure` and the installer probe `openssl genpkey -algorithm ed25519` and stop with a named error if it is unsupported.
- Dev and CI: a Go generator in `internal/testhelpers/natstest`, also runnable from `scripts/concord-dev.sh` and `scripts/ci/start-control-plane-services.sh`. Dev output goes to `.dev/nats/` (gitignored).
- A Go test checks that `nkeys.FromSeed` accepts every seed the install script emits and derives the same public key.

**State machine.** `ensure` runs under `flock`, stages each write in the same directory, renames it into place, and keeps `.env` at mode 0600.

| State | Action |
|---|---|
| Seed or `BUS_MAC_KEYRING` present | Never regenerated. Malformed: stop, naming the key. |
| Seed absent | Generate. |
| Public key absent or not matching its seed | Re-derive and rewrite. |
| No CA key, no CA certificate, no leaves | Create the CA (10 years). |
| CA key absent and a leaf needs issuing | Stop: "internal NATS CA key missing; see self-hosted-tls runbook". |
| CA key absent, all leaves valid | Warn and continue. |
| Leaf missing, not chaining to the CA, under 365 days left, or wrong EKU or SANs | Reissue (2 years) and force recreation of the consuming service in the same `up`. |

**Certificates.** Under `${NATS_CERTS_DIR}` (default `/opt/concord/certs/nats`): `ca/`, `server/`, `control-plane/`, `media-plane/`, `ops-agent/`. Server SANs: `nats`, `concordvoice-nats`, `localhost`, `127.0.0.1`. Client leaves carry only the client-auth EKU. `ca.key` is never mounted. Each container mounts read-only its own directory plus `ca.crt`. Key files are 0400, owned by the image's runtime UID: 10006 (control plane), 10007 (media plane), 0 (ops-agent); T5 resolves the broker's. `concord-selfhost health` warns under 60 days. CA rotation is a manual runbook step.

**Env writers.** In self-host contract mode the root `.env` is the only writer (the controller refuses `deploy.env.local` there). In managed contract mode both writers carry the values, and a new `nats_managed_writer_consistency_guard`, modelled on the Redis guard, refuses disagreement before Compose.

**Fail-closed layers, outermost first:** the `up` backfill; Compose `${VAR:?message}` guards with `bind.create_host_path: false`; the server's unresolved-variable refusal; client validation.

**Rollback.** The documented path is unchanged: check out the previous tag and restore the pre-update backup. The old Compose file and config ignore the new variables.

### 5.8 Config surface

Surfaces: (a) loaders, (b) writer emission, (c) `.env.example`, (d) Compose base and overlays, (e) managed-mode writer guard. New scripts are added to `infrastructure/deploy/deploy-manifest.txt`.

| Variable | Scope | Secret | Surfaces |
|---|---|---|---|
| `NATS_CONTROL_PLANE_NKEY_SEED`, `NATS_MEDIA_PLANE_NKEY_SEED`, `NATS_OPS_AGENT_NKEY_SEED` | host `.env` | yes | b, c (blank), d (mapped to the container's `NATS_NKEY_SEED`, `:?`), e |
| `NATS_CONTROL_PLANE_NKEY_PUBLIC`, `NATS_MEDIA_PLANE_NKEY_PUBLIC`, `NATS_OPS_AGENT_NKEY_PUBLIC` | host `.env` to broker | no | b, c, d (`:?`), e |
| `BUS_MAC_KEYRING` | host `.env` to CP and MP | yes | a, b, c (blank), d (`:?`), e |
| `NATS_CERTS_DIR` | host `.env` | no | b, c, d (bind sources), e |
| `NATS_URL` (`tls://nats:4222`) | container | no | a (scheme checked, default removed), c, d |
| `NATS_NKEY_SEED`, `NATS_TLS_CA_FILE`, `NATS_TLS_CERT_FILE`, `NATS_TLS_KEY_FILE` | CP, MP, ops-agent | seed yes | a, d (fixed paths under `/run/concord/nats/`) |
| `NATS_SERVER_TLS_CERT`, `NATS_SERVER_TLS_KEY`, `NATS_SERVER_TLS_CA` | broker | no | d (fixed paths under `/etc/nats/tls/`) |

Compose interpolates every service, including ones in an inactive profile, so all three identities are always provisioned.

## 6. Test plan

### 6.1 Exploit tests

Source: Security repository, `scans/2026-11-11-self-hosted-vulnhunt/exploit_tests/`. Regenerate the paths in `overlay_vuln107.json`, `overlay_vuln108_voice.json`, `overlay_vuln108_hub.json`, `overlay_vuln109.json`, `overlay_vuln111.json`, `overlay_vuln112.json`, `overlay_vuln165.json` and `overlay_vuln208.json` for the branch. Run with `go test -overlay`. Record the output per test function in the PR. Each failure must be an assertion, not a compile error or a panic.

| File | Function | Expected on the fix |
|---|---|---|
| `cp_voice_consumers_vuln107_test.go` | `ForgedLifecycleEventsReachTheHandler` | Fail: 0 of 3 frames admitted. |
| | `ForgedTierViolationEscalatesAgainstVictim` | Fail: no counter key. |
| | `BrokerAndClientHaveNoAuthentication` | Fail: config has `authorization` and `tls`. |
| `nats_no_auth_vuln165_test.go` | `BrokerConfigHasNoAuthOrTLS` | Fail. |
| | `ControlPlaneClientSendsNoCredentials` | Fail: `Connect` refuses `nats://` before dialing. |
| | `LiveAnonymousPeerReadsAndWritesBus` | Skips unless `VULN165_NATS_URL` is set. Run it against a broker started from the shipped config. Fail: anonymous connect refused. |
| `forged_unknown_user_disconnect_all_vuln108_test.go` | `ForgedJoinedUnknownUserDisconnectsWholeReplica` | Fail: gate sheds the frame. |
| | `Control_LockErrorIsTheTrigger` | Pass (control: an unknown user is still an error). |
| `forged_voice_joined_vuln109_test.go` | `ForgedVoiceJoinedPlantsParticipantRow` | Fail at the gate. |
| | `LifecycleWrapperAddsNoMembershipCheck` | Fail at the sink: no origin and no admission, so no row. |
| `forged_dm_room_empty_vuln111_test.go` | `ForgedDMRoomEmptyWritesCallHistory` | Fail at the gate. |
| `forged_voice_heartbeat_vuln112_test.go` | `ForgedHeartbeatAddsArbitraryParticipants` | Fail at the gate. |
| `forged_room_empty_disconnect_all_vuln208_test.go` | both functions | Fail: the shared helper asserts the gate admitted the frames. |
| `hub_disconnect_all_closes_sockets_vuln108_test.go` | `DisconnectAllClosesEveryRealSocket` | **Still passes** (§11.1). |

Because the gate sheds these frames first, the handler fixes are proven by the new tests in §6.2, which drive validly signed frames.

### 6.2 New tests

**`pkg/busmac` and `busMac.test.ts`:** shared vectors, including non-ASCII and escaped-surrogate bodies. Wrong key, family, subject or kid; stale and future `ts`; newline or NUL in a field; nil or empty keyring; duplicate kid; short key; unknown envelope key; `v` not 1.

**Clients:** each missing credential, a non-`tls` scheme and a non-user seed are refused before dialing, for all three identities. The inbox prefix is set per identity.

**Gate (`internal/voice/ingress_gate_origin_test.go`):**

- Unsigned, badly signed, wrong-family, wrong-subject and out-of-skew lifecycle frames reach no handler and shed as `origin_proof`.
- Unsigned `voice.producer_*` sheds as `unhandled_subject`.
- A struct-literal subscriber with a nil keyring sheds without panicking.
- `voice.joined` is metered per room: a full-room rejoin of `maxServerVoiceParticipantIDs` frames, twice in one refill interval, sheds nothing; the next frame sheds. `voice.left` and `voice.room_empty` are never shed by the meter.
- Tier violation: no proof records nothing (VULN-113). An envelope over 1024 bytes and an inner body over 512 bytes are refused.

**DB-backed handlers** (disposable `DATABASE_URL` and `REDIS_URL`, signed frames through the gate):

- Server join without an admission, and with a non-matching admission: no row.
- Heartbeat naming a non-member, and a member without `CONNECT`: no row, hook 0, the rest of the heartbeat applied.
- Heartbeat naming a user whose live row belongs to another boot: claimed only when the resolve passes; an untouched row keeps its `lifecycle_observed_at`.
- `voice.left`, `room_empty` and heartbeat omission from another boot against a live row: nothing removed (VULN-110, VULN-209). The same against a NULL row and an expired row: removed.
- A row refreshed 31 s ago is not removable by another boot.
- DM `room_empty` with an unknown call ID, and with stranger participants: no history, hook 0.
- Unknown-user joined and heartbeat: hook 0.
- No-op `room_empty`: no tombstone gives hook 0; this replica's own recent tombstone gives hook 0; a peer's recent tombstone gives hook 1; a read error gives hook 1.
- Two subscribers on one database: exactly one tombstone, and the loser reconnects its clients.
- Concurrent member removal against a heartbeat claim under `-race`: no deadlock.
- Migration 000198 up and down, following `TEST_MIGRATIONS.md`.

**Broker conformance** (pinned broker, shipped config verbatim, only port, store directory and monitoring port overridden). Go suite `internal/natsconformance` and TS suite `src/lib/__tests__/nats.auth.integration.test.ts`:

- Anonymous, plaintext, foreign-CA, wrong-EKU, wrong-nkey and TLS 1.2-only clients are refused.
- Every denied publish in §5.2, per identity. The CP cannot publish any lifecycle subject. The MP cannot publish `voice.enforce.*`, `voice.commands.v1`, `voice.user_mute` or `voice.user_deafen`; the CP can.
- ops-agent cannot subscribe. No identity can subscribe to another's inbox.
- The queue subscription works; a subscription under a different group is recorded.
- A full startup trace with zero permission violations: CP `Initialize`, publish, `VoiceCommandAckFloor`, purge; MP consumer create, `MSG.NEXT`, ack, owner update, request reply. The trace decides the † entries.

**MP (`nats.test.ts`):** lifecycle frames are enveloped and carry `nodeBootId`; `publish()` refuses the five signed subjects; config fails closed on each missing input.

**Provisioning (`infrastructure/deploy/tests/`):** a second run changes nothing byte for byte; every state-machine row; modes and ownership; the renewal threshold and the 60-day warning; a reissued leaf recreates its service; upgrade of a pre-change `.env` fixture; `docker compose config` without credentials prints the `:?` message; port-posture fixtures regenerated; the broker healthcheck passes with monitoring on `127.0.0.1`; the nkey cross-check.

**Shared helper:** `natstest.Start(t)` and a TS equivalent generate ephemeral nkeys and a CA, start the broker from `scripts/test-tools/nats-server.sh` with the shipped config, and set the client variables. Existing bus tests move to it (about 28 Go and 8 TS files, system-engineer count).

## 7. Acceptance criteria

| Gate | Threshold |
|---|---|
| New blocker or critical issues | 0 |
| New high-confidence security hotspots | 0 |
| Coverage on new code | at least 80% |

- [ ] An anonymous client cannot connect. No identity can publish or subscribe outside its row in §5.2. The conformance trace shows zero violations.
- [ ] All bus traffic uses mutual TLS with verified peers, TLS 1.3 minimum at the broker. The base Compose file publishes neither 4222 nor 8222. Monitoring binds container loopback.
- [ ] Every unsigned or badly signed lifecycle or tier-violation frame is shed before any handler runs.
- [ ] A server `voice.joined` without a matching pending admission creates no row. A heartbeat cannot add or claim a non-member, or a member without `CONNECT`.
- [ ] A DM `room_empty` with an unknown call ID, or naming non-participants, writes no history.
- [ ] A `voice.left`, `room_empty` or heartbeat omission removes no row that another boot stamped and that is still within its 90 s lease.
- [ ] An unknown-user joined or heartbeat, and a `room_empty` with nothing to remove and no peer tombstone, never call `disconnectAllRichPresenceClients`. No refusal added by this change does.
- [ ] The exploit tests produce the results in §6.1. VULN-110, VULN-113 and VULN-209 have regression tests.
- [ ] `voice.user_mute`, `voice.user_deafen` and `voice.enforce.deafen` are granted to the CP for publish and the MP for subscribe, and no document describes them as retired.
- [ ] An existing install upgrades through `concord-selfhost update` with no manual credential step. A bare `compose up` without credentials stops with a named error.
- [ ] Every client refuses to start when its seed, certificate, key, CA or keyring is missing, or its URL is not `tls://`.
- [ ] **Docs to update** (§8) ship in the same PR.
- [ ] `/verify-quality-gate` is green.

## 8. Docs to update

- `docs/architecture.md`: the three identities, the matrix, mTLS, the signed envelope, the boot binding.
- `docs/runbooks/self-hosted-tls.md`: the internal NATS CA, leaf lifetimes, renewal on `up`, the health warning, CA rotation, and the three-step MAC-key rotation (add second, promote, remove).
- `docs/runbooks/selfhost-quickstart.md` and `docs/runbooks/selfhost-release.md`: provisioning, the upgrade step, the `openssl` Ed25519 prerequisite, and that a short burst of `origin_proof` sheds after a long broker outage is expected.
- `docs/development.md`: local credentials in `.dev/nats/` and the dev `127.0.0.1` publish.
- `.env.example`, `services/control-plane/README.md`, `CHANGELOG.md` (breaking change).
- `.claude/rules/domains/backend.md`: the claim and removal rules, the 90 s lease, the tombstone, the envelope and receipt-time skew, the determined-no-op list.
- `.claude/rules/domains/media-plane.md`: lifecycle subjects are published only signed, the tier-violation bounds (1024 envelope, 512 inner), the three kept subjects, the inbox prefix.
- `.claude/rules/domains/tests.md`: the authenticated broker helper.
- `.claude/rules/domains/migrations.md`: migration 000198.
- The G2 comment block in `internal/voice/ingress_gate.go`: origin proof before the meter, joined metering, why `room_empty` and `left` stay unmetered.

## 9. Task breakdown (for `/superpowers:writing-plans`)

Each commit is green. Main lane: T2, T3, T4, T6 in that order. T1 and the start of T5 run in parallel with T2.

| # | Task | Main files | Depends on |
|---|---|---|---|
| T1 | `busmac` in Go and TS, shared vectors | `services/control-plane/pkg/busmac/`, `services/media-plane/src/lib/busMac.ts` | none |
| T2 | Handler binding, one serial lane: (a) VULN-108 typed error and per-user drop; (b) migration 000198, MP sends `nodeBootId`, CP event structs, origin context, claim and removal rules (VULN-109 sink, VULN-110, VULN-112, VULN-209); (c) server-join admission rejection (VULN-109); (d) tombstone and no-op arm (VULN-208); (e) VULN-111 | `internal/voice/nats.go`, `internal/presence/activity_service.go`, `internal/dm/call_events.go`, `migrations/000198_*`, lifecycle publish sites in `services/media-plane/src/lib/nats.ts` | none |
| T3 | Authenticated bus in **one commit**: both clients, broker config, Compose base and overlays, healthcheck, test helper and Go generator, CI and dev brokers, conformance suites, migration of existing bus tests | `pkg/nats/client.go`, `cmd/ops-agent/main.go`, `pkg/config`, `src/lib/nats.ts`, `src/config/index.ts`, `nats-cluster.conf`, `docker-compose*.yml`, `internal/testhelpers/natstest`, `internal/natsconformance`, `scripts/test-tools/`, `scripts/ci/start-control-plane-services.sh`, `scripts/concord-dev.sh` | T1 (keyring parser), T2 |
| T4 | Matrix trim from the trace, TLS minimum key, monitoring override for tests, queue-semantics result | `nats-cluster.conf`, conformance tests | T3 |
| T5 | Provisioning: script, Python encoder, `up` hook, installer call, recreate on reissue, managed-writer guard, health warning, manifest, fixtures, cross-check test | `infrastructure/deploy/*`, `infrastructure/deploy/tests/` | interface from §5.8; integrates after T3 |
| T6 | Envelope enforcement on both services together: `publishSigned`, gate and tier-violation verification, bounds, joined metering, G2 comment | `src/lib/nats.ts`, `internal/voice/ingress_gate.go`, `internal/voice/tier_violation_ingress_test.go` | T1, T2, T3 |
| T7 | Docs and rules (§8) | as listed | T2 to T6 |
| T8 | Exploit-test run with regenerated overlays and `VULN165_NATS_URL`; results recorded in the PR | none | all |

## 10. Risk register

| Risk | Likelihood | Mitigation |
|---|---|---|
| The matrix misses a JetStream subject and voice commands stall | High | Conformance trace on both services' real startup against the shipped config. |
| Test churn from authenticated brokers | High | One shared helper; T3 is atomic. |
| Overlap with #294 on Compose, installer, `concord-ctl.sh`, `.env.example` | High | Rebase order agreed by the orchestrator before Phase 3. |
| A new refusal falls into an existing disconnect-all arm | Medium | Typed sentinels; a hook-count-0 test per refusal. |
| Lock-order deadlock between a heartbeat claim and member removal | Medium | T2 reads the removal path's order first and documents it; concurrent test. A conflict recalls the architect. |
| Upgrade leaves an install without credentials | Medium | Backfill in `up`, Compose guards, upgrade test on a pre-change fixture. |
| A reissued leaf is on disk but the running service still holds the old one | Medium | Reissue forces recreation in the same `up`; test. |
| Healthcheck resolves `localhost` to `::1` and never passes | Medium | Healthcheck uses `127.0.0.1`; test. |
| Honest mass rejoin exceeds the joined budget | Low | 2x-ceiling burst, convergence through the heartbeat claim, test. |
| Clock difference above 5 s between MP and CP hosts sheds all lifecycle frames | Low on a single host | Both skew bounds are named constants with tests; the runbook names the symptom (`origin_proof`). |
| Frames queued in the client across a long broker outage fail the skew check | Low | Accepted loss; converges through the stale sweep and the claim rule; documented. |
| Predecessor-boot rows linger up to 90 s after an MP restart | Certain, accepted | Same as today's crash behaviour. A rejoin restamps immediately. |
| Hand-rolled nkey encoding is wrong | Low | Cross-check against `nkeys`; a wrong key fails closed. |
| Host `openssl` lacks Ed25519 | Low | Named prerequisite probe in the installer and in `ensure`. |

## 11. Confirmations and verifications

### 11.1 Owner confirmations (deviations from the issue text)

1. **`hub_disconnect_all_closes_sockets_vuln108_test.go` keeps passing.** It asserts only that `Hub.DisconnectAllRichPresenceClients` closes every socket, which is the fail-closed contract of the presence revocation fence. Making it fail would weaken that fence. Recommendation: amend the criterion to record it as amplifier evidence, while both trigger tests fail. Bounded teardown stays with Security#57.
2. **`voice.room_empty` is not metered.** The issue lists metering for it. A shed `room_empty` is terminal and a DM one loses call history. `voice.joined` is metered.

Consistent with the issue's intent, recorded for visibility:

3. A `room_empty` that removes nothing still reconnects clients when a peer replica has just removed the rows. The issue's own security notes keep fail-closed for unknown audience state.
4. Instance binding uses a per-row boot stamp, not the placement record, because placement is off on self-host.
5. Mandatory admission is enforced in the server-join handler and the write sink, not in `parseVoiceJoinedAdmission`.
6. On self-host there is one env writer. The two-writer rule applies in managed contract mode.

### 11.2 Task-level verifications

| Task | Verify |
|---|---|
| T2 | Lock order of the member-removal path. Whether the `CONNECT` resolver can read inside the lifecycle transaction. That DM `voice.left` returns before any mutation when the lease check fails. Per-row removals are broadcast when other boots' rows remain. |
| T4 | The TLS minimum-version key on `nats:2.15.2`. Whether `$VAR` resolves inside the nested `users` list. The † entries. Whether the tier-violation queue name is ever overridden outside tests. |
| T5 | The broker image's runtime UID. Whether `selfhost-tls.py` already requires a given `openssl` version. |
| `security-reviewer` | That the account-erasure presence rail covers a user deleted mid-lock, so the VULN-108 drop is a determined no-op. |

## 12. Review routing and count-bearing surfaces

- Reviewers: `security-reviewer`, `red-team` (mandatory), `code-reviewer`, `silent-failure-hunter`, `pr-test-analyzer`, `docs-reviewer`, `migration-reviewer`. Content triggers are re-evaluated against the real diff at Phase 8.
- Count-bearing: one migration, new Go packages (`pkg/busmac`, `internal/natsconformance`, `internal/testhelpers/natstest`), new test files. This repository has no count probe.

