# Security#174 NATS Authentication and Voice Binding Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Authenticate every NATS client with its own nkey over mutual TLS, enforce the §5.2 permission matrix at the broker, sign every media-plane lifecycle and tier-violation event, and bind each control-plane lifecycle write to server-side state.

**Architecture:** A shared HMAC module (`pkg/busmac` in Go, `busMac.ts` in TS) signs a body-wrapper envelope that the control-plane ingress gate opens before any handler runs. The broker config carries three nkey users with allow-lists and a `verify: true` TLS block. Handlers take the decoded `nodeBootId` as origin and apply the claim and removal rules against a new per-row `media_boot_id` stamp (migration 000198).

**Tech Stack:** Go 1.x control plane (`nats.go` v1.55.0, `nkeys` v0.4.17, PostgreSQL), TypeScript media plane (`nats` ^2.29.3, vitest), `nats-server` 2.15.2 (pinned in `scripts/test-tools/nats-server.sh` and the Compose image), bash and Python provisioning under `infrastructure/deploy/`.

**Spec:** docs/superpowers/specs/2026-11-18-174-nats-auth-voice-binding-design.md

## Global Constraints

- Every lifecycle and tier-violation frame is signed; no anonymous, legacy or test-only insecure mode exists anywhere, including tests and dev.
- Client URLs are `tls://nats:4222`; the `nats://localhost:4222` defaults are removed from `pkg/config`, `cmd/ops-agent` and `src/config/index.ts`.
- Client TLS: `MinVersion: TLS13`, internal CA as the only root. Inbox prefixes: `_INBOX.cp`, `_INBOX.mp`, `_INBOX.ops`.
- Keyring format: `BUS_MAC_KEYRING="<kid>:<base64url, at least 32 bytes>[,<kid>:<key>...]"`, kid `^[a-z0-9]{1,16}$`, unique; first entry signs, any entry verifies.
- Envelope: exactly `{"v":1,"kid":"...","ts":<int ms>,"mac":"<base64url, no padding>","body":"<inner JSON text>"}`; unknown keys refused.
- `subkey = HMAC-SHA256(root[kid], "concord-bus-mac/v1/" + family)`; `mac = HMAC-SHA256(subkey, "concord-bus-mac/v1\n" + family + "\n" + kid + "\n" + subject + "\n" + ts + "\n" + hex(sha256(bodyBytes)))`.
- Skew: accept `receivedAt - 60 s <= ts <= receivedAt + 5 s`; `receivedAt` is NATS callback time, never a handler's `time.Now()`. No nonce cache.
- Families shipped here: `mp>cp/lifecycle`, `mp>cp/telemetry`. Signed subjects: `voice.joined`, `voice.left`, `voice.room_empty`, `voice.heartbeat`, `voice.tier_violation`. Unsigned: `voice.producer_*`, `ops.metrics.media.v1`.
- Bounds: lifecycle frame 128 KB (unchanged); tier-violation envelope 1024 bytes, inner 512 bytes.
- `voice.joined` meter: own per-room bucket, burst `2 * maxServerVoiceParticipantIDs`, refill `maxServerVoiceParticipantIDs` per 10 s. `voice.left`, `voice.room_empty`: not metered.
- Lease: row expired when `lifecycle_observed_at < receivedAt - presence.ActivityStateTTL` (90 s). Tombstone peer window: `ActivityStateTTL + 30 s`.
- "CONNECT" means `rbac.PermViewVoiceChannels` and `rbac.PermJoinVoice` from `Resolver.ResolveChannelPermissionsTx`, the check the join-authorization path uses (`internal/voice/handlers.go:485`).
- AST log guard over `nats.go` admits only `failure_class`, `action`, `is_dm`, `context`, `count`. Never log subject, raw error, kid, MAC or missing IDs.
- Exploit-test compile surface (spec §3) stays byte-stable: `pkg/nats.Connect(url string) (*Client, error)` with `nats.Nkey`/`nats.Secure` written in `pkg/nats/client.go`; `gateVoiceLifecycle`, `admitTierViolation`, `SetTierViolationRecorder`, `NewNATSSubscriber` (7 args), `handleJoined(data, receivedAt)`, `handleRoomEmpty(data, receivedAt)`, `parseVoiceJoinedAdmission`, `moveServerVoiceParticipant`, `mutateServerVoiceParticipant` (4 results), `persistDMRoomEmptySummary` `(bool, error)`, `lockVoiceParticipantUsersTx` `error`, `parseSortedVoiceParticipantIDs`, the listed fields and helpers. A struct-literal `NATSSubscriber{log, voiceRoomBudget}` sheds without panicking.
- Migration number: head is `000197` at `b23fe7d7` and still at `origin/main` `16aaa7d1`. Re-check before the migration commit; renumber if main moved.
- Commits: Conventional Commits, bare `type:`. This is a public repository: no private-repo names, paths or issue links in diffs, comments, commits or PR text.
- Workflow files: Task 7 edits `.github/workflows/unit-smoke.yml` and `.github/workflows/build.yml`. Pushing that commit needs a credential with `workflow` scope.
- Security#175 R1 (multi-key keyring for both planes and the `voice-command-owner` subcommands): Task 1 (`ParseKeyring`, `ActiveKID`), Task 10 (CP `main` and `voice-command-owner` parse it; MP config parses it).
- Security#175 R2 (raw MAC, no time window, caller-supplied purposes, constant-time, Go and TS, registry): Task 1 (`RawMAC`/`RawVerify`, `registry.go` table that #175 extends with its header name and nine purposes).
- Security#175 R3 (owner KV publish scoped to the exact subject for CP and MP): Task 7 matrix, Task 8 conformance `TestConformance_OwnerKVExactSubject`.
- Security#175 R4 (no other identity publishes to or subscribes on the MP pull inbox): Task 7 inbox prefixes, Task 8 `TestConformance_InboxIsolation`.
- Security#175 R5 (missing or short secret fatal on both planes and the subcommands): Task 1 (`ParseKeyring` refuses), Task 10 (CP boot, subcommand, MP config).
- Security#175 R6 (shared updater order): Task 9 exposes `concord-selfhost nats-credentials ensure` as its own step, so #175's stepwise updater calls it before "start the control plane".

## Review Focus

- A maximal honest frame (a heartbeat with `maxServerVoiceParticipantIDs` users; a 512-byte tier-violation inner body full of quote characters) must still fit the 128 KB and 1024-byte envelope bounds after JSON-string escaping. Test in Task 10.
- A broker restart while CP and MP run must re-authenticate both on reconnect (nkey signature and TLS handshake every time), and lifecycle traffic resumes. Test in Task 8.
- An operator-edited `.env` (CRLF line ends, quoted values, trailing whitespace, a trailing comma in `BUS_MAC_KEYRING`) must never cause a seed or keyring to be regenerated; a malformed value stops with the key's name. Test in Task 9.
- An `ensure` killed between staging and rename must leave the old file intact, and the next run converges. Test in Task 9.
- An envelope with a duplicate key (two `body` or two `mac` fields) is refused identically by Go and TS, so the two decoders never disagree on which body was signed. Test in Task 1.

---

### Task 1: Bus MAC module (Go and TS) with shared vectors

`Shape: construction`

**Files:**
- Create: `services/control-plane/pkg/busmac/busmac.go`, `services/control-plane/pkg/busmac/registry.go`, `services/control-plane/pkg/busmac/envelope.go`
- Create: `services/control-plane/pkg/busmac/busmac_test.go`, `services/control-plane/pkg/busmac/testdata/vectors.json`
- Create: `services/media-plane/src/lib/busMac.ts`, `services/media-plane/tests/busMac.test.ts`

**Interfaces:**
- Consumes: none.
- Produces (Go):
  - `type Family string`; `const FamilyLifecycle Family = "mp>cp/lifecycle"`, `FamilyTelemetry Family = "mp>cp/telemetry"`.
  - `func Registered(f Family) bool` reading the single table in `registry.go` (Security#175 adds its purposes and header-name constant there).
  - `const MinKeyBytes = 32`, `MaxPastSkew = 60 * time.Second`, `MaxFutureSkew = 5 * time.Second`.
  - Errors: `ErrEmptyKeyring`, `ErrMalformedKeyring`, `ErrShortKey`, `ErrDuplicateKID`, `ErrBadEnvelope`, `ErrUnknownKID`, `ErrUnregisteredFamily`, `ErrBadField`, `ErrSkew`, `ErrBadMAC`.
  - `func ParseKeyring(spec string) (*Keyring, error)`; `func (k *Keyring) ActiveKID() string`.
  - `func (k *Keyring) RawMAC(purpose Family, lines []string) (kid string, mac []byte, err error)` and `func (k *Keyring) RawVerify(purpose Family, kid string, lines []string, mac []byte) bool`: no time window; input `"concord-bus-mac/v1\n" + purpose + "\n" + kid + "\n" + strings.Join(lines, "\n")` under the purpose subkey.
  - `func (k *Keyring) MAC(f Family, subject string, tsMillis int64, body []byte) (kid string, mac []byte, err error)` = `RawMAC(f, [subject, strconv(ts), hex(sha256(body))])`; `VerifyMAC(f, kid, subject, tsMillis, body, mac) bool`.
  - `func (k *Keyring) Sign(f Family, subject string, body []byte, now time.Time) ([]byte, error)` and `Open(f Family, subject string, envelope []byte, receivedAt time.Time) ([]byte, error)`.
- Produces (TS, `busMac.ts`): `FAMILY_LIFECYCLE`, `FAMILY_TELEMETRY`, `parseKeyring(spec: string): BusMacKeyring`, methods `activeKid()`, `rawMac(purpose, lines)`, `rawVerify(purpose, kid, lines, mac)`, `mac(family, subject, tsMillis, body: Buffer)`, `verifyMac(...)`, `sign(family, subject, body: string, nowMs: number): string`, `open(family, subject, envelope: string, receivedAtMs: number): string`. Errors are `BusMacError` with a `code` matching the Go names in snake case.

- [ ] **Step 1: Write the failing Go tests** in `busmac_test.go`:

```go
func TestVectors(t *testing.T)                       // every vectors.json case: MAC hex equal, Open verdict equal
func TestVectorsMatchGenerator(t *testing.T)         // regenerates in memory; BUSMAC_UPDATE_VECTORS=1 rewrites the file, else require.Equal
func TestOpenRefuses(t *testing.T)                   // table: wrong key, family, subject, kid; ts = receivedAt-60001ms and +5001ms -> ErrSkew; ts at -60000ms and +5000ms accepted
func TestFieldsRefuseNewlineAndNUL(t *testing.T)     // "\n" or "\x00" in kid, family, subject, any RawMAC line -> ErrBadField
func TestNilAndEmptyKeyringNeverVerify(t *testing.T) // (*Keyring)(nil).VerifyMAC == false; ParseKeyring("") -> ErrEmptyKeyring
func TestParseKeyringRefuses(t *testing.T)           // duplicate kid, 31-byte key, kid "A1", kid of 17 chars, trailing comma, padding '=' in key
func TestEnvelopeStrict(t *testing.T)                // unknown key, v:2, ts:1.5, missing mac, duplicate "body", duplicate "mac" -> ErrBadEnvelope
func TestUnregisteredFamily(t *testing.T)            // Family("x") sign -> ErrUnregisteredFamily, verify false
func TestAnyKeyringEntryVerifies(t *testing.T)       // sign with "a,b", open with "b,a" succeeds; ActiveKID()=="a"
```

`vectors.json` cases include an ASCII body, a non-ASCII body (`{"n":"é漢"}`), a body containing the literal escape text `\ud800`, and an envelope whose `body` JSON string holds a raw lone-surrogate escape (both sides decode it to U+FFFD bytes `EF BF BD`).

- [ ] **Step 2: Run to verify failure**

Run: `cd services/control-plane && go test ./pkg/busmac/ -v`
Expected: FAIL, package does not compile (`undefined: ParseKeyring`).

- [ ] **Step 3: Implement the Go module.** `hmac.Equal` for comparison. Envelope decode walks `json.Decoder.Token()` over the top-level object to refuse duplicate and unknown keys, then decodes values; `ts` must be a JSON integer (`json.Number` with no `.`/`e`). Base64url is `base64.RawURLEncoding` for both keys and MAC.

- [ ] **Step 4: Run Go tests**

Run: `cd services/control-plane && BUSMAC_UPDATE_VECTORS=1 go test ./pkg/busmac/ -run TestVectorsMatchGenerator && go test ./pkg/busmac/ -v`
Expected: PASS.

- [ ] **Step 5: Write the failing TS tests** in `tests/busMac.test.ts`: `it('matches every shared vector')` loading `../../control-plane/pkg/busmac/testdata/vectors.json`, plus one `it` per row of the Go `TestOpenRefuses`, `TestFieldsRefuseNewlineAndNUL`, `TestParseKeyringRefuses`, `TestEnvelopeStrict` and `TestAnyKeyringEntryVerifies` tables with the same inputs and expected `code`.

- [ ] **Step 6: Run to verify failure**

Run: `cd services/media-plane && npx vitest run tests/busMac.test.ts`
Expected: FAIL, cannot resolve `../src/lib/busMac.js`.

- [ ] **Step 7: Implement `busMac.ts`** with `node:crypto` `createHmac` and `timingSafeEqual` (length-checked first). Duplicate-key detection uses a reviver-free scan: parse with `JSON.parse`, then re-check key count with a minimal top-level tokenizer, so Go and TS refuse the same inputs.

- [ ] **Step 8: Run TS tests, typecheck, lint**

Run: `cd services/media-plane && npx vitest run tests/busMac.test.ts && npm run typecheck && npm run lint`
Expected: PASS, no errors.

- [ ] **Step 9: Commit**

```bash
git add services/control-plane/pkg/busmac services/media-plane/src/lib/busMac.ts services/media-plane/tests/busMac.test.ts
git commit -m "feat: add the bus MAC module with shared vectors"
```

---

### Task 2: Typed missing-user error (VULN-108)

`Shape: defect-repair`

**Files:**
- Modify: `services/control-plane/internal/voice/nats.go` (`lockVoiceParticipantUsersTx` at :884, joined and heartbeat callers)
- Modify: `services/control-plane/internal/presence/activity_service.go`, `services/control-plane/internal/presence/activity_types.go`
- Test: `services/control-plane/internal/voice/nats_user_missing_test.go`

**Interfaces:**
- Produces: `presence.ErrActivityUserMissing` (exported sentinel); in `internal/voice`: `var errVoiceParticipantUserMissing = presence.ErrActivityUserMissing` and `type voiceParticipantUsersMissingError struct{ missing []uuid.UUID }` with `Unwrap() error` returning the sentinel and `Missing() []uuid.UUID`. `lockVoiceParticipantUsersTx` signature unchanged.

- [ ] **Step 1: Write failing DB-backed tests**

```go
func TestJoinedUnknownUserDropsWithoutDisconnect(t *testing.T)        // handleJoined for a deleted user: no row, hook count 0
func TestHeartbeatUnknownUserDropsOnlyThatUser(t *testing.T)          // heartbeat {known, unknown}: known row refreshed, unknown absent, hook 0
func TestHeartbeatAllUsersUnknownIsNoop(t *testing.T)                 // every user missing: nothing applied, hook 0
func TestLockVoiceParticipantUsersMissingIsTyped(t *testing.T)        // errors.Is(err, errVoiceParticipantUserMissing); Missing() == [unknown]
func TestRefreshMutationMatchesActivityUserMissing(t *testing.T)      // runRefreshMutation maps it to failure_class=user_missing, hook 0
```

Hook count is read from `disconnectAllRichPresenceClientsHook`.

- [ ] **Step 2: Run to verify failure**

Run: `cd services/control-plane && DATABASE_URL=$TEST_DATABASE_URL REDIS_URL=$TEST_REDIS_URL go test -p 1 ./internal/voice/ -run 'UnknownUser|UsersMissing|ActivityUserMissing' -v`
Expected: FAIL, the joined and heartbeat cases record a hook count of 1.

- [ ] **Step 3: Implement.** The joined handler drops the event; the heartbeat path removes `Missing()` IDs from its set and retries the lock once with the rest; each logs `failure_class=user_missing` with `count`. Before editing, record the member-removal path's lock order in a comment above `lockVoiceParticipantUsersTx` (spec §11.2).

- [ ] **Step 4: Run tests, including the existing log guard**

Run: `cd services/control-plane && DATABASE_URL=$TEST_DATABASE_URL REDIS_URL=$TEST_REDIS_URL go test -p 1 ./internal/voice/ ./internal/presence/ -v`
Expected: PASS, including `nats_rich_presence_log_guard_test.go`.

- [ ] **Step 5: Commit**

```bash
git add services/control-plane/internal/voice services/control-plane/internal/presence
git commit -m "fix: drop unknown voice users without a replica-wide disconnect"
```

---

### Task 3: Boot binding, migration 000198, claim and removal rules (VULN-110, VULN-112, VULN-209, VULN-109 sink)

`Shape: defect-repair`

**Files:**
- Create: `services/control-plane/migrations/000198_add_voice_media_boot_binding.up.sql`, `.down.sql`
- Create: `services/control-plane/internal/database/migration_000198_test.go`
- Modify: `services/control-plane/internal/voice/nats.go` (event structs, `mutateServerVoiceParticipant` :3783, `moveServerVoiceParticipant` :3843, `NewNATSSubscriber` :4054, left/heartbeat removal paths)
- Modify: `services/media-plane/src/lib/nats.ts` (lifecycle publish sites :920-970)
- Test: `services/control-plane/internal/voice/nats_boot_binding_test.go`, `services/media-plane/tests/nats.test.ts`

**Interfaces:**
- Consumes: Task 2 typed error.
- Produces:
  - Event structs `voiceJoinedEvent`, `voiceLeftEvent`, `voiceRoomEmptyEvent`, `voiceHeartbeatEvent` gain `NodeBootID uuid.UUID \`json:"nodeBootId"\``.
  - `type lifecycleOriginKey struct{}`; `func withLifecycleOrigin(ctx context.Context, boot uuid.UUID) context.Context`; `func lifecycleOrigin(ctx context.Context) (uuid.UUID, bool)`.
  - New `NATSSubscriber` fields: `resolver *rbac.Resolver` (set in `NewNATSSubscriber` from its existing `resolver` argument) and `replicaID uuid.UUID` (`uuid.New()` in `NewNATSSubscriber`; zero in a struct literal). Arity unchanged.
  - `func serverVoiceRowRemovable(stamp uuid.NullUUID, observedAt, receivedAt time.Time, origin uuid.UUID) bool`.
  - MP: lifecycle payloads carry `nodeBootId: voiceEnforcementNodeBootId` (imported from `./voiceEnforcementIdentity.js`).

- [ ] **Step 1: Re-check the migration head**

Run: `git fetch origin && git ls-tree --name-only origin/main services/control-plane/migrations/ | grep -E '/0001[0-9]{3}_' | tail -1`
Expected: `.../000197_add_admin_session_generation.up.sql`. Any higher number: renumber this migration and its test.

- [ ] **Step 2: Write the failing migration test** (`TestMigration000198_UpDownUpRoundTrip`, `TestMigration000198_RefusesNilUUIDStamp`, `TestMigration000198_TerminalCascadesWithChannel`, `TestMigration000198_RemovedCountPositive`), following `migration_000194_test.go`.

- [ ] **Step 3: Write the migration.** `SET LOCAL lock_timeout = '5s';` (repo form of the spec's `SET lock_timeout`); add `voice_participants.media_boot_id UUID` with the validated CHECK `voice_participants_media_boot_id_not_nil`; create `server_voice_room_terminals` exactly as spec §5.5; no index. Down drops the table, the constraint, the column.

- [ ] **Step 4: Run the migration tests**

Run: `cd services/control-plane && DATABASE_URL=$TEST_DATABASE_URL go test -p 1 ./internal/database/ -run Migration000198 -v`
Expected: PASS.

- [ ] **Step 5: Write failing handler tests** in `nats_boot_binding_test.go` (calling handlers directly with `receivedAt`; the gate is not yet enforcing):

```go
func TestHeartbeatClaimsForeignRowOnlyWhenResolvePasses(t *testing.T) // non-member: no row; member without PermJoinVoice: no row; member with CONNECT: stamped X
func TestHeartbeatForeignRowUntouchedKeepsObservedAt(t *testing.T)     // refused claim: lifecycle_observed_at unchanged
func TestHeartbeatNilResolverWritesNothing(t *testing.T)
func TestRemovalFromOtherBootLeavesLiveRow(t *testing.T)               // voice.left, room_empty, heartbeat omission vs a row stamped Y observed 31 s ago: kept, action=foreign_boot, hook 0
func TestRemovalOfNullAndExpiredRows(t *testing.T)                     // NULL stamp, and observed 91 s before receivedAt: removed
func TestSinkWithoutOriginWritesNothing(t *testing.T)                  // mutateServerVoiceParticipant with bare ctx: applied=false
func TestSinkWithoutAdmissionWritesNothing(t *testing.T)               // origin set, admission nil, server channel: no row
func TestConcurrentMemberRemovalAndClaim(t *testing.T)                 // run with -race; both finish within 10 s
```

MP: in `tests/nats.test.ts`, `it('lifecycle payloads carry nodeBootId')` for joined, left, room_empty, heartbeat.

- [ ] **Step 6: Run to verify failure**

Run: `cd services/control-plane && DATABASE_URL=$TEST_DATABASE_URL REDIS_URL=$TEST_REDIS_URL go test -p 1 -race ./internal/voice/ -run 'Claim|Removal|SinkWithout|ConcurrentMember' -v` and `cd services/media-plane && npx vitest run tests/nats.test.ts`
Expected: FAIL on the new cases.

- [ ] **Step 7: Implement.** Each lifecycle handler calls `withLifecycleOrigin` with the decoded boot (separate from the placement context value). `moveServerVoiceParticipant` locks the `server_members` row `FOR SHARE`; `mutateServerVoiceParticipant` resolves CONNECT through `s.resolver.ResolveChannelPermissionsTx` inside the same transaction. The foreign-row case is the upsert's `ON CONFLICT ... DO UPDATE ... WHERE` predicate. Removal paths filter with `serverVoiceRowRemovable`; the placement guard still runs first. Rewrite the `.claude/rules/domains/backend.md` sentence that allows a heartbeat insert without admission.

- [ ] **Step 8: Run the voice suite and MP tests**

Run: `cd services/control-plane && DATABASE_URL=$TEST_DATABASE_URL REDIS_URL=$TEST_REDIS_URL go test -p 1 -race ./internal/voice/ ./internal/database/ && cd ../media-plane && npx vitest run tests/nats.test.ts`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add services/control-plane/migrations/000198_* services/control-plane/internal services/media-plane/src/lib/nats.ts services/media-plane/tests/nats.test.ts .claude/rules/domains/backend.md
git commit -m "fix: bind server voice writes to the media boot that owns them"
```

---

### Task 4: Mandatory admission on server joins (VULN-109)

`Shape: defect-repair`

**Files:**
- Modify: `services/control-plane/internal/voice/nats.go` (server branch of `handleJoined` :6149)
- Test: `services/control-plane/internal/voice/nats_join_admission_test.go`

**Interfaces:**
- Consumes: Task 3 origin context. `parseVoiceJoinedAdmission` keeps its behaviour.

- [ ] **Step 1: Write failing tests:** `TestServerJoinWithoutAdmissionCreatesNoRow`, `TestServerJoinWithMismatchedAdmissionCreatesNoRow`, `TestServerJoinConsumesSuppliedAdmission` (second replay of the same admission writes nothing; the `priorAdmitted` rule holds), `TestDMJoinWithoutAdmissionUnchanged`. Each asserts hook count 0.
- [ ] **Step 2: Run to verify failure**

Run: `cd services/control-plane && DATABASE_URL=$TEST_DATABASE_URL REDIS_URL=$TEST_REDIS_URL go test -p 1 ./internal/voice/ -run 'ServerJoin|DMJoinWithoutAdmission' -v`
Expected: FAIL on the first test (a row is planted).
- [ ] **Step 3: Implement:** the server-join branch rejects a nil admission before the sink, logging `failure_class=admission_missing`.
- [ ] **Step 4: Run tests**

Run: same command as Step 2.
Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "fix: refuse server voice joins that carry no admission"`

---

### Task 5: Room-empty tombstone and determined no-op (VULN-208)

`Shape: defect-repair`

**Files:**
- Modify: `services/control-plane/internal/voice/nats.go` (`handleResolvedRoomEmpty` :7519)
- Test: `services/control-plane/internal/voice/nats_room_empty_tombstone_test.go`

**Interfaces:**
- Consumes: Task 3 `replicaID`, `server_voice_room_terminals`.
- Produces: `const roomEmptyTombstoneWindow = presence.ActivityStateTTL + 30*time.Second`; `func (s *NATSSubscriber) peerTombstoneRecent(ctx context.Context, channelID uuid.UUID, receivedAt time.Time) (bool, error)`.

- [ ] **Step 1: Write failing tests**

```go
func TestRoomEmptyRemovingRowsWritesTombstone(t *testing.T)    // one row, removed_count matches, recorded_by == s.replicaID
func TestNoopRoomEmptyWithoutTombstone(t *testing.T)           // hook 0, action=noop_room_empty, no broadcast
func TestNoopRoomEmptyOwnTombstone(t *testing.T)               // hook 0
func TestNoopRoomEmptyPeerTombstone(t *testing.T)              // recorded_at within window: hook 1
func TestNoopRoomEmptyPeerTombstoneExpired(t *testing.T)       // recorded_at older than window: hook 0
func TestNoopRoomEmptyTombstoneReadError(t *testing.T)         // canceled ctx on read: hook 1
func TestTwoSubscribersOneTombstone(t *testing.T)              // two NATSSubscriber on one DB: one tombstone row; the loser's hook == 1
func TestRoomEmptyBroadcastsPerRowWhenOtherBootRowsRemain(t *testing.T)
```

- [ ] **Step 2: Run to verify failure**

Run: `cd services/control-plane && DATABASE_URL=$TEST_DATABASE_URL REDIS_URL=$TEST_REDIS_URL go test -p 1 ./internal/voice/ -run 'RoomEmpty|TwoSubscribers' -v`
Expected: FAIL (no tombstone; no-op disconnects).
- [ ] **Step 3: Implement** per spec §5.6 VULN-208: tombstone upsert in the delete transaction; `databaseEmpty && !removedAny` reads `peerTombstoneRecent`; read error keeps the existing disconnect.
- [ ] **Step 4: Run tests**

Run: same command as Step 2.
Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "fix: skip the reconnect for a room_empty that removed nothing"`

---

### Task 6: DM room_empty call-history binding (VULN-111)

`Shape: defect-repair`

**Files:**
- Modify: `services/control-plane/internal/voice/nats.go` (`persistDMRoomEmptySummary` :6899 call site), `services/control-plane/internal/dm/call_events.go` (`insertCompletedCallEvent` :244)
- Test: `services/control-plane/internal/voice/nats_dm_room_empty_binding_test.go`, `services/control-plane/internal/dm/call_events_participants_test.go`

**Interfaces:**
- Produces: `dm.ErrCallParticipantMismatch`. `insertCompletedCallEvent` keeps its `*sql.DB` parameter (it opens its own transaction; the spec's "in its transaction" means that one) and checks `dm_participants` for the caller and every listed participant inside it.

- [ ] **Step 1: Write failing tests:** `TestDMRoomEmptyUnknownCallWritesNoHistory`, `TestDMRoomEmptyStrangerParticipantWritesNoHistory` (action `participant_mismatch`, hook 0), `TestDMRoomEmptyLeaseCheckRunsBeforePersist`, `TestDMLeftReturnsBeforeMutationWhenLeaseFails`, `TestInsertCompletedCallEventRefusesNonParticipant`.
- [ ] **Step 2: Run to verify failure**

Run: `cd services/control-plane && DATABASE_URL=$TEST_DATABASE_URL REDIS_URL=$TEST_REDIS_URL go test -p 1 ./internal/voice/ ./internal/dm/ -run 'DMRoomEmpty|DMLeft|NonParticipant' -v`
Expected: FAIL (history row written).
- [ ] **Step 3: Implement:** move `dmCallEventMayMutateLiveState` ahead of `persistDMRoomEmptySummary` under `LockDMCallLifecycle`; add the participant check.
- [ ] **Step 4: Run tests**

Run: same command as Step 2.
Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "fix: write DM call history only for live calls and real participants"`

---

### Task 7: Authenticated bus, one atomic commit

`Shape: construction`

**Files:**
- Modify: `services/control-plane/pkg/nats/client.go`, `services/control-plane/cmd/ops-agent/main.go`, `services/control-plane/pkg/config/config.go`, `services/control-plane/go.mod` (`nkeys` direct)
- Modify: `services/media-plane/src/lib/nats.ts` (`connect()` :809), `services/media-plane/src/config/index.ts` (:560)
- Modify: `infrastructure/docker/nats/nats-cluster.conf`, `docker-compose.yml`, `docker-compose.dev.yml`, `docker-compose.selfhost.yml`, `docker-compose.production.yml`, `.env.example`, `.gitignore` (`.dev/nats/`; only `.dev-data/` is ignored today)
- Create: `services/control-plane/internal/testhelpers/natstest/natstest.go`, `natstest_test.go`, `cmd/natstest-gen/main.go` (under `internal/testhelpers/natstest/`)
- Create: `services/control-plane/internal/natsconformance/conformance_test.go`, `services/media-plane/tests/helpers/natsAuthBroker.ts`, `services/media-plane/tests/natsAuth.integration.test.ts` (the spec's `src/lib/__tests__/` path is outside vitest's `tests/**/*.test.ts` include)
- Modify: `scripts/ci/start-control-plane-services.sh` (drops the unpinned `nats:2-alpine` container), `scripts/concord-dev.sh`, `scripts/test-tools/nats-server.sh` (config argument), `scripts/test-nats-server-bootstrap.sh`
- Modify (workflows): `.github/workflows/unit-smoke.yml` (run the dev generator after `cp .env.example .env`, before `validate-config` and `compose up`), `.github/workflows/build.yml` (add `infrastructure/docker/nats/**` and `docker-compose*.yml` to the `control-plane` and `media-plane-tests` path filters)
- Modify: the ~30 Go and ~2 TS bus test files that dial a broker (`grep -rln 'NATS_URL\|nats-server.sh' services/control-plane --include='*_test.go'`; `services/media-plane/tests/helpers/natsServerBinary.ts` users)

**Interfaces:**
- Consumes: Task 1 `ParseKeyring` (for config validation only).
- Produces:
  - `pkg/nats`: `type Identity string` with `IdentityControlPlane`, `IdentityMediaPlane`, `IdentityOpsAgent`; `type Credentials struct{ Seed, CAFile, CertFile, KeyFile string }`; `func CredentialsFromEnv() Credentials` (reads `NATS_NKEY_SEED`, `NATS_TLS_CA_FILE`, `NATS_TLS_CERT_FILE`, `NATS_TLS_KEY_FILE`); `func ConnectWithIdentity(url string, id Identity, creds Credentials) (*Client, error)`; `var ErrNATSConfig = errors.New("nats configuration invalid")`. `Connect(url)` = `ConnectWithIdentity(url, IdentityControlPlane, CredentialsFromEnv())`.
  - `natstest`: `type Broker struct{ URL string; Creds map[pkgnats.Identity]pkgnats.Credentials; MACKeyring string; Dir string }`; `func Start(t testing.TB) *Broker` (sets the client env vars via `t.Setenv`); `func Generate(dir string) (Env, error)` used by the generator command; `type Env map[string]string`.
  - TS: `startAuthBroker(): Promise<{ url, env, stop }>` in `tests/helpers/natsAuthBroker.ts`.
  - Broker conf variables: `$NATS_CONTROL_PLANE_NKEY_PUBLIC`, `$NATS_MEDIA_PLANE_NKEY_PUBLIC`, `$NATS_OPS_AGENT_NKEY_PUBLIC`, `$NATS_SERVER_TLS_CERT`, `$NATS_SERVER_TLS_KEY`, `$NATS_SERVER_TLS_CA`, all unquoted.

- [ ] **Step 1: Write failing client tests** in `pkg/nats/client_test.go`, for all three identities: `TestConnectRefusesPlainScheme`, `TestConnectRefusesEachMissingCredential` (table over the four inputs, plus unreadable file), `TestConnectRefusesNonUserSeed` (an account seed), `TestConnectRefusesMismatchedCertAndKey`, `TestInboxPrefixPerIdentity`. Each asserts `errors.Is(err, ErrNATSConfig)` and that no TCP dial happened (listener on a free port accepts 0 connections). MP: in `tests/config.test.ts`, one case per missing input and for a `nats://` URL.

- [ ] **Step 2: Write failing conformance tests** (Go `internal/natsconformance`, TS `natsAuth.integration.test.ts`) against `natstest.Start` with the shipped config verbatim (only port, store dir, monitoring port overridden): `TestConformance_RefusesAnonymousPlaintextForeignCAWrongEKUWrongNkeyTLS12`, `TestConformance_DeniedPublishes` (table: every §5.2 denial per identity), `TestConformance_CPPublishesKeptSubjects` (`voice.user_mute`, `voice.user_deafen`, `voice.enforce.deafen`), `TestConformance_OpsAgentCannotSubscribe`, `TestConformance_InboxIsolation` (CP and ops cannot subscribe to or publish on `_INBOX.mp.>`; MP cannot subscribe to `_INBOX.cp.>`), `TestConformance_OwnerKVExactSubject` (CP and MP may publish `$KV.VOICE_COMMAND_OWNER_V1.authoritative`; `$KV.VOICE_COMMAND_OWNER_V1.other` denied), `TestConformance_QueueSubscription`, `TestConformance_ReconnectAfterBrokerRestart` (Review Focus: both clients re-authenticate and a publish after restart arrives).

- [ ] **Step 3: Run to verify failure**

Run: `cd services/control-plane && go test ./pkg/nats/ ./internal/natsconformance/ -v` and `cd services/media-plane && npx vitest run tests/config.test.ts tests/natsAuth.integration.test.ts`
Expected: FAIL (`undefined: ConnectWithIdentity`, `natstest` missing).

- [ ] **Step 4: Implement clients, config, broker conf, Compose, helpers.** Go: `nats.Nkey(pub, kp.Sign)` and `nats.Secure(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: ca, Certificates: ...})` written in `client.go`; `nats.CustomInboxPrefix`; `RetryOnFailedConnect(true)` kept. TS: `nkeyAuthenticator(seed)`, `tls: { caFile, certFile, keyFile }`, `inboxPrefix: '_INBOX.mp'`; confirm with Context7 that nats.js 2.29 passes `minVersion` through to `tls.connect`, else enforce TLS 1.3 at the broker only. Broker conf: spec §5.1 and §5.2 matrix in full (the † entries kept until Task 8), `http: "127.0.0.1:8222"`. Compose: remove `-m 8222` and both base `ports:` (the self-host `!reset []` stays harmless); healthcheck `http://127.0.0.1:8222/healthz`; dev overlay publishes `127.0.0.1:4222:4222` only; every new variable uses `${VAR:?<named message>}`; bind mounts use `bind.create_host_path: false`; container paths `/run/concord/nats/` (clients) and `/etc/nats/tls/` (broker); rewrite the ops-agent "no credentials" comment. `pkg/config`: `NATSUrl` default removed, scheme must be `tls`. Dev and CI call the `natstest-gen` command, writing to `.dev/nats/`.

- [ ] **Step 5: Move existing bus tests to `natstest.Start` / `startAuthBroker`** and run both full suites.

Run: `cd services/control-plane && DATABASE_URL=$TEST_DATABASE_URL REDIS_URL=$TEST_REDIS_URL go test -p 1 ./... ` and `cd services/media-plane && npm test && npm run typecheck && npm run lint`
Expected: PASS, no test skips because a broker was absent.

- [ ] **Step 6: Verify Compose and scripts**

Run: `docker compose -f docker-compose.yml config >/dev/null` with an empty env (expect the `:?` message naming `NATS_CONTROL_PLANE_NKEY_SEED`), then `./scripts/concord-dev.sh validate-config` after the generator, and `bash scripts/test-nats-server-bootstrap.sh`
Expected: named error, then exit 0, then PASS.

- [ ] **Step 7: Commit (one commit; push with a `workflow`-scoped credential)**

```bash
git add -A services infrastructure/docker/nats docker-compose*.yml .env.example .gitignore scripts .github/workflows/unit-smoke.yml .github/workflows/build.yml
git commit -m "feat: authenticate every NATS client with nkeys over mutual TLS"
```

---

### Task 8: Matrix trim from the trace, TLS minimum, queue result

`Shape: construction`

**Files:**
- Modify: `infrastructure/docker/nats/nats-cluster.conf`, `services/control-plane/internal/natsconformance/conformance_test.go`, `services/media-plane/tests/natsAuth.integration.test.ts`

**Interfaces:**
- Consumes: Task 7 `natstest.Start`, conformance suites.

- [ ] **Step 1: Write the failing trace test** `TestConformance_StartupTraceZeroViolations`: CP `Initialize`, publish, `VoiceCommandAckFloor`, purge; MP consumer create, `MSG.NEXT`, ack, owner update, request reply; asserts the broker log has zero `Permissions Violation` lines and records each † subject's use.
- [ ] **Step 2: Run it**

Run: `cd services/control-plane && go test ./internal/natsconformance/ -run StartupTrace -v`
Expected: PASS or FAIL; either way the log names the used † subjects.
- [ ] **Step 3: Edit the conf:** drop each unused † entry; add the TLS 1.3 minimum key confirmed by `nats-server -t -c <conf>` on 2.15.2 (if no such key exists, record that in the conf comment and rely on the client `MinVersion` plus the TLS 1.2-only refusal test); confirm `$VAR` resolves inside `users` (a startup with a bogus public key must fail). Record whether the tier-violation queue name is overridden outside tests.
- [ ] **Step 4: Run both conformance suites**

Run: `cd services/control-plane && go test ./internal/natsconformance/ -v && cd ../media-plane && npx vitest run tests/natsAuth.integration.test.ts`
Expected: PASS, zero violations.
- [ ] **Step 5: Commit** `git commit -m "feat: trim the NATS permission matrix to the traced startup"`

---

### Task 9: Install-time provisioning

`Shape: construction`

**Files:**
- Create: `infrastructure/deploy/selfhost-nats-credentials.sh`, `infrastructure/deploy/selfhost-nats-keys.py`
- Create: `infrastructure/deploy/tests/test-selfhost-nats-credentials.sh` (picked up by the `pr-ci.yml` `test-selfhost-*.sh` glob; add it to `scripts/tests/run-selfhost-deploy-tests.sh` SUITES)
- Create: `services/control-plane/internal/testhelpers/natstest/install_crosscheck_test.go`
- Modify: `infrastructure/deploy/concord-selfhost` (`up)` at :435, new `nats-credentials` subcommand, `health` warning), `infrastructure/deploy/install-selfhost.sh`, `infrastructure/deploy/concord-ctl.sh` (`nats_managed_writer_consistency_guard` beside `redis_managed_writer_consistency_guard` :130, called where :6589 calls the Redis guard), `infrastructure/deploy/deploy-manifest.txt`, `infrastructure/deploy/tests/fixtures/selfhost-port-posture/*`, `scripts/tests/test-concord-selfhost.sh`, `scripts/tests/test-install-selfhost.sh`

**Interfaces:**
- Consumes: Task 7 variable names and container paths.
- Produces: `selfhost-nats-credentials.sh ensure [--env FILE] [--certs-dir DIR]` exit 0 on success, non-zero with one named error line; on a reissued leaf it prints `recreate: <service>` lines that `concord-selfhost` passes to the rebuild. `concord-selfhost nats-credentials ensure`. `up)` becomes `require_security_log_boundary && nats_credentials_ensure && guarded_ctl rebuild && authority_lifecycle up`.

- [ ] **Step 1: Write failing shell tests** in `test-selfhost-nats-credentials.sh`, one function per state-machine row of spec §5.7, plus: idempotent second run (byte-for-byte `.env` and cert tree), modes 0600 `.env` and 0400 keys with owners 10006/10007/0 and the broker UID resolved from the image, renewal under 365 days, 60-day `health` warning, `recreate:` on reissue, pre-change `.env` fixture upgrade, missing Ed25519 support named error (stub `openssl`), `flock` serialisation of two runs, and the Review Focus cases: CRLF / quoted / trailing-whitespace / trailing-comma `.env` values (no regeneration, malformed keyring named), and a run killed between stage and rename (old file intact, rerun converges). Go: `TestInstallSeedsMatchNkeys` runs the script in a temp dir and checks `nkeys.FromSeed` derives the written public key.
- [ ] **Step 2: Run to verify failure**

Run: `bash infrastructure/deploy/tests/test-selfhost-nats-credentials.sh` and `cd services/control-plane && go test ./internal/testhelpers/natstest/ -run InstallSeeds -v`
Expected: FAIL (script missing).
- [ ] **Step 3: Implement** the script per §5.7 (openssl Ed25519 and P-256, CA 10 years, leaves 2 years, server SANs `nats`, `concordvoice-nats`, `localhost`, `127.0.0.1`, client leaves client-auth EKU only, `ca.key` never mounted) and the Python nkey encoder (stdlib only: base32, CRC16). Check whether `selfhost-tls.py` already pins an `openssl` version.
- [ ] **Step 4: Run all provisioning tests**

Run: `bash infrastructure/deploy/tests/test-selfhost-nats-credentials.sh && bash infrastructure/deploy/tests/test-selfhost-port-posture.sh && bash infrastructure/deploy/tests/test-selfhost-update.sh && bash scripts/tests/test-concord-selfhost.sh && bash scripts/tests/test-install-selfhost.sh`
Expected: PASS (on macOS use `bash scripts/tests/run-selfhost-deploy-tests.sh`, which runs them on Linux).
- [ ] **Step 5: Commit** `git commit -m "feat: provision NATS credentials on install and every up"`

---

### Task 10: Envelope enforcement on both services (VULN-107, VULN-113)

`Shape: defect-repair`

**Files:**
- Modify: `services/media-plane/src/lib/nats.ts` (`publish()` :881, lifecycle and tier-violation sites), `services/media-plane/src/config/index.ts`, `services/media-plane/src/index.ts`
- Modify: `services/control-plane/internal/voice/ingress_gate.go` (`gateVoiceLifecycle` :531, `admitTierViolation` :643, `newVoiceRoomBudget` :177, G2 comment), `services/control-plane/internal/voice/nats.go` (`Subscribe` :5292), `services/control-plane/cmd/server/main.go`, `services/control-plane/cmd/server/voice_command_owner_recovery.go`, `services/control-plane/pkg/config/config.go`
- Test: `services/control-plane/internal/voice/ingress_gate_origin_test.go`, `services/control-plane/internal/voice/tier_violation_ingress_test.go`, `services/media-plane/tests/nats.test.ts`, `services/media-plane/tests/config.test.ts`

**Interfaces:**
- Consumes: Task 1 `Keyring.Open/Sign`, Task 3 `NodeBootID`, Task 7 authenticated broker.
- Produces: `func (s *NATSSubscriber) SetBusMACKeyring(k *busmac.Keyring)`; `Subscribe()` returns `errBusMACKeyringMissing` when nil. `config.Config.BusMACKeyring string` (`BUS_MAC_KEYRING`); CP `main` and both `voice-command-owner` subcommands exit non-zero when `busmac.ParseKeyring` fails. MP: `config.busMacKeyring: BusMacKeyring` (parse failure throws at load); `publishSigned(subject: SignedSubject, family, payload): boolean`; `publish()` returns false for the five signed subjects. `const tierViolationEnvelopeMaxBytes = 1024`, `voiceJoinedBurstMultiplier = 2`, `voiceJoinedRefillInterval = 10 * time.Second`.

- [ ] **Step 1: Write failing gate tests** in `ingress_gate_origin_test.go`: unsigned, badly signed, wrong-family, wrong-subject, out-of-skew lifecycle frames reach no handler and shed `origin_proof`; unsigned `voice.producer_added` sheds `unhandled_subject`; `NATSSubscriber{log: l, voiceRoomBudget: newVoiceRoomBudget()}` sheds without panic; missing `nodeBootId` sheds `invalid_event`; full-room rejoin of `maxServerVoiceParticipantIDs` frames twice in one interval sheds nothing and the next frame sheds; `voice.left` and `voice.room_empty` never shed by the meter; `Subscribe()` with nil keyring errors. Tier violation: no proof records nothing; envelope 1025 bytes and inner 513 bytes refused. Review Focus: a signed heartbeat naming `maxServerVoiceParticipantIDs` users fits under 128 KB; a 512-byte quote-heavy inner tier-violation body fits under 1024. MP: lifecycle frames are enveloped and verify with the shared keyring; `publish('voice.joined', …)` returns false; config throws on missing or short `BUS_MAC_KEYRING`. CP config: production `Load` refuses an empty `BUS_MAC_KEYRING`; `voice-command-owner inspect` exits non-zero on a short key.
- [ ] **Step 2: Run to verify failure**

Run: `cd services/control-plane && DATABASE_URL=$TEST_DATABASE_URL REDIS_URL=$TEST_REDIS_URL go test -p 1 ./internal/voice/ ./pkg/config/ ./cmd/server/ -run 'Origin|TierViolation|BusMAC|VoiceCommandOwner' -v` and `cd services/media-plane && npx vitest run tests/nats.test.ts tests/config.test.ts`
Expected: FAIL.
- [ ] **Step 3: Implement** the gate order of spec §5.4 exactly (MAC before meter), `publishSigned` with no `await` between sign and publish, and rewrite the G2 comment block (origin proof before the meter, joined metering, why `left` and `room_empty` stay unmetered).
- [ ] **Step 4: Run every touched suite, then the exploit compile check**

Run: the Step 2 commands, then `cd services/control-plane && go vet ./internal/voice/ ./pkg/nats/ ./internal/websocket/`
Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "fix: verify the origin MAC on every media-plane lifecycle frame"`

---

### Task 11: Docs and rules

`Shape: construction`

**Files:**
- Modify: every file in spec §8 (`docs/architecture.md`, `docs/runbooks/self-hosted-tls.md`, `docs/runbooks/selfhost-quickstart.md`, `docs/runbooks/selfhost-release.md`, `docs/development.md`, `.env.example`, `services/control-plane/README.md`, `CHANGELOG.md`, `.claude/rules/domains/backend.md`, `.claude/rules/domains/media-plane.md`, `.claude/rules/domains/tests.md`, `.claude/rules/domains/migrations.md`)

- [ ] **Step 1: Write each §8 item.** The runbook gives the three-step MAC-key rotation (add second, promote, remove) and names `origin_proof` as the clock-skew and long-outage symptom. No document calls `voice.user_mute`, `voice.user_deafen` or `voice.enforce.deafen` retired.
- [ ] **Step 2: Verify**

Run: `grep -rn -i 'retired' docs .claude/rules | grep -E 'user_mute|user_deafen|enforce.deafen'` and `pre-commit run --files <changed docs>`
Expected: no grep output; hooks pass.
- [ ] **Step 3: Commit** `git commit -m "docs: document NATS identities, the signed envelope and boot binding"`

---

### Task 12: Exploit-test run and PR record

`Shape: defect-repair`

**Files:** none in this repository (overlays come from the Security repository, `scans/2026-11-11-self-hosted-vulnhunt/exploit_tests/`).

- [ ] **Step 1: Regenerate the eight overlay JSON paths** for the branch checkout.
- [ ] **Step 2: Run each overlay**

Run: `cd services/control-plane && DATABASE_URL=$TEST_DATABASE_URL REDIS_URL=$TEST_REDIS_URL go test -overlay <overlay>.json ./internal/voice/ ./pkg/nats/ ./internal/websocket/ -run '<function>' -v`; for `LiveAnonymousPeerReadsAndWritesBus` set `VULN165_NATS_URL` to a broker from `natstest-gen` with the shipped config.
Expected: the per-function results of spec §6.1; every failure is an assertion, never a compile error or panic; `DisconnectAllClosesEveryRealSocket` passes.
- [ ] **Step 3: Record** the per-function output in the PR body, and run `/verify-quality-gate`.

Expected: gate green.

---

