# Enforcement Origin Proof Implementation Plan (Security#175)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every enforcement command, durable `voice.commands.v1` record and owner record carries an HMAC that the media plane verifies before it applies anything, durable ordering binds to the broker stream sequence, and unauthenticated input can no longer reach the instance-wide teardown.

**Architecture:** A detached HMAC-SHA256 proof travels in the `Concord-Proof` NATS header on the six compatibility subjects and on the durable stream, and inside the JSON value of a new owner record v2. The control plane signs through one leaf package (`internal/enforcementbus`) and the existing durable publisher. The media plane verifies in one seam (`subscribeVerified`), in a verdict-first durable consumer with a broker-sequence tracker, and in the owner store. A protocol bump from 4 to 5 makes mixed-version pairs fail closed at the lease.

**Tech Stack:** Go 1.x control plane (`nats.go` v1.55.0, `jetstream`), TypeScript media plane (`nats` `^2.29.3`, Vitest), pinned `nats-server` 2.15.2 through `scripts/test-tools/nats-server.sh`, Bash deploy scripts.

**Spec:** docs/superpowers/specs/2026-11-18-175-enforcement-origin-proof-design.md. Read it with this plan. It builds on Security#174 (docs/superpowers/specs/2026-11-18-174-nats-auth-voice-binding-design.md, §5.3 busmac module, §5.2 broker matrix). **Security#174 must be merged before Task 0 starts.**

## Global Constraints

- Every MAC is HMAC-SHA256 under a per-purpose subkey derived from the keyring entry named by `kid`, computed with #174's `RawMAC`/`RawVerify`. The MAC input is lines joined by `\n`: line 1 is the version prefix `concord-bus-mac/v1`, line 2 is the purpose string, line 3 is `kid`, then the surface's fields (spec §6 says "line 1 is the purpose"; #174's merged primitive puts the version prefix first, and this plan follows #174). The signer refuses a field that contains a newline or NUL.
- Proof header: exactly one key and one value. A case variant of the name, or a second value, rejects. At most 128 bytes. Grammar is checked before any HMAC work. The name must not start with `Nats-`.
- Durable proof value `v1.<kid>.<mac>`. Compatibility proof value `v1.<kid>.<ts>.<nonce>.<mac>`. `kid` is `[a-z0-9]{1,16}`. `ts` is 13-digit unix milliseconds. `nonce` is 22 characters of unpadded base64url (128 bits). `mac` is 64 lowercase hex characters.
- No JSON decode, schema check or room lookup runs on command bytes whose MAC has not verified. The owner record is the one bounded exception (spec 7.4).
- The stream and consumer contract is frozen: one pull durable, explicit ack, DeliverAll, MaxAckPending one, MaxMsgSize 4096, no purge or repair.
- Media startup never creates or repairs ownership resources. Every read and CAS checks resource births.
- `testdata/envelopes.json` is not changed. Proof vectors go in a sibling `testdata/proofs.json`.
- All compatibility handlers and the full required-subscription set stay. No required subscription may silently no-op.
- Logs carry fixed classifications only: no payloads, identifiers, subjects or raw errors.
- Constant-time comparison. A nil or short key never verifies. No blank error discard without the documented reason.
- Real JetStream tests use #174's authenticated helpers (Go `natstest.Start(t)`, TS `startAuthBroker()` in `services/media-plane/tests/helpers/natsAuthBroker.ts`) and never skip.
- TypeScript tests live under `services/media-plane/tests/**/*.test.ts`; Vitest collects nothing elsewhere.
- No migration. No legacy or unsigned mode. No new environment variable unless Task 0 finds #174's keyring has no verify-only slot.
- Durable publish budget: `MaxCommandHeaderBytes = 512`; body over 3584 bytes is refused at enqueue with `ErrInvalidCommand`; the decoder bound stays 4096 in both languages.
- Compatibility freshness: `ts` within 6 s of now and not before this boot's wall-clock start. Replay cache: at most 65,536 entries, evicts by expiry only.
- `voiceEnforcementProtocol` goes from 4 to 5.
- Commits use Conventional Commits with a bare `type:` (no scope). This repo is public: commits, comments and docs name no private repository.

## Review Focus

1. **Skew boundary in both directions.** A genuine compatibility command whose `ts` is 6000 ms ahead of or behind the media plane's clock applies; 6001 ms rejects. Test added to Task 4 (`acceptsTsAtSixSecondBoundaryBothWays`).
2. **Key rotation mid-flight.** A record or command signed under a verify-only key still verifies on all three surfaces, and the media plane's next owner renewal after promotion writes the active `kid`. Tests added to Task 4 (`verifiesUnderVerifyOnlyKid`), Task 5 (`durableVerifiesUnderVerifyOnlyKid`) and Task 6 (`renewalAfterPromotionResignsWithActiveKid`).
3. **Multibyte UTF-8 at the publish budget.** A 3584-byte body with multibyte characters signs in Go and verifies in TypeScript over the raw bytes. Vector added to Task 0 (`proofs.json` case `durable-multibyte-max-body`), storage test added to Task 2.
4. **Header key case across languages on the compatibility path.** A header that Go publishes arrives in `nats.js` with exactly the key `Concord-Proof` (spec pins case only for the durable path). Real-broker test added to Task 1 (`TestEnforcementBusHeaderCaseSurvivesBroker` with a `nats.js`-equivalent exact-key read through the Go SDK's raw header map).
5. **Media-plane restart with an applied but unconfirmed ack.** After restart, the redelivered authentic record is acked as idempotent with no second effect and no gap. Test added to Task 5 (`restartRedeliveryAfterUnconfirmedAckIsNotAGap`).

---

## File Structure

| File | Responsibility | Task |
|---|---|---|
| `services/control-plane/pkg/busmac/proof.go` (new) | Proof header name, nine purposes, header grammar, MAC field builders | 0 |
| `services/control-plane/pkg/busmac/proof_test.go`, `proof_fixture_test.go` (new) | Golden vectors; generates and drift-checks `proofs.json` | 0 |
| `services/control-plane/internal/voicecommand/testdata/proofs.json` (new) | Shared vectors for both suites | 0 |
| `services/media-plane/src/lib/busProof.ts` (new) | TypeScript mirror of `proof.go`; durable verdict | 0, 5 |
| `services/control-plane/internal/securityevent/securityevent.go`, `services/media-plane/src/lib/securityEvent.ts` | `media_proof_rejected` reason | 0 |
| `services/control-plane/internal/enforcementbus/` (new) | Six subject constants, signed compatibility publisher, architecture test | 1 |
| `services/control-plane/pkg/nats/client.go` | `PublishRaw` | 1 |
| `services/control-plane/internal/voice/{nats,permission_enforcer,temp_grants}.go`, `internal/dm/handlers.go` | Move six publish sites onto the publisher | 1 |
| `services/control-plane/pkg/nats/voice_command_publisher.go`, `internal/voicecommand/{contract,outbox}.go` | Durable proof header, size budget | 2 |
| `services/control-plane/pkg/nats/voice_commands.go`, `voice_command_owner_v2.go` (new) | Owner v2 codec, sign, verify, writers, readers, reseal | 3 |
| `services/control-plane/cmd/server/voice_command_owner_recovery.go` | `reseal` and `inspect` output | 3 |
| `services/media-plane/src/lib/verifiedBusMessage.ts`, `nonceReplayGuard.ts` (new) | Branded verified message, replay guard | 4 |
| `services/media-plane/src/lib/nats.ts` | `subscribeVerified`, verdict-first consumer, owner store v2 | 4, 5, 6 |
| `services/media-plane/src/lib/{enforcementCommand,enforcePermissions,voiceCommandDispatcher,serverEnforcement}.ts`, `src/index.ts`, `src/lib/roomManager.ts` | Branded handlers, bare-lift removal | 4 |
| `services/media-plane/src/lib/voiceCommandSequenceTracker.ts` (new) | Floor/pending tracker and bind baseline | 5 |
| `services/media-plane/src/lib/voiceCommandOwner.ts` | Owner v2 decode, verify, sign | 6 |
| `services/media-plane/src/lib/{voiceAuthProtocol,voiceEnforcementLease}.ts`, `services/control-plane/internal/api/{voice_auth_boots,voice_auth_connections,voice_enforcement_sessions}.go`, `internal/api/testdata/auth_connection_protocol.json` | Protocol 5 | 7 |
| `infrastructure/deploy/selfhost-voice-owner.sh` (new), `infrastructure/deploy/concord-selfhost`, `deploy-manifest.txt` | Upgrade order | 7 |
| `services/control-plane/internal/natsconformance/enforcement_proof_test.go` (new) | Criterion 9 against the shipped config | 8 |
| docs and rules (spec §14) | Docs | 9 |

Order: Task 0 first and alone. Tasks 1, 2, 3 and 4 may run in parallel. Task 5 after 4, Task 6 after 5 (same file). Task 7 after 1–6. Tasks 8 and 9 after 7, in parallel.

---

### Task 0: Align with the merged #174 module; proof codec and shared vectors

**Shape:** construction

**Files:**
- Modify: `services/control-plane/pkg/busmac/` (the #174 module: family registry, keyring)
- Create: `services/control-plane/pkg/busmac/proof.go`, `proof_test.go`, `proof_fixture_test.go`
- Create: `services/control-plane/internal/voicecommand/testdata/proofs.json`
- Modify: `services/media-plane/src/lib/busMac.ts` (the #174 module)
- Create: `services/media-plane/src/lib/busProof.ts`, `services/media-plane/tests/busProof.test.ts`
- Modify: `services/control-plane/internal/securityevent/securityevent.go` (constant near line 166, `reasons` set near line 780), `services/control-plane/internal/securityevent/securityevent_test.go`
- Modify: `services/media-plane/src/lib/securityEvent.ts` (reason union near line 44 and its runtime list), `services/media-plane/tests/securityEvent.test.ts`
- Modify: `.secrets.baseline` if `proofs.json` trips detect-secrets

**Interfaces:**
- Consumes (#174 plan Task 1): `busmac.ParseKeyring(spec string) (*Keyring, error)`, `func (k *Keyring) ActiveKID() string`, `type Family string`, the `registry.go` purpose table, `func (k *Keyring) RawMAC(purpose Family, lines []string) (kid string, mac []byte, err error)` and `func (k *Keyring) RawVerify(purpose Family, kid string, lines []string, mac []byte) bool` (input `"concord-bus-mac/v1\n" + purpose + "\n" + kid + "\n" + strings.Join(lines, "\n")` under the purpose subkey, no time window), `BUS_MAC_KEYRING` "first entry signs, any entry verifies". TS (`busMac.ts`): `parseKeyring`, `activeKid()`, `rawMac(purpose, lines)`, `rawVerify(purpose, kid, lines, mac)`.
- Produces (Go, `pkg/busmac`; names are provisional until Step 1 fixes them against the merged code, and every later task uses whatever Step 1 records):
  - `const ProofHeader = "Concord-Proof"`
  - Families: `FamilyVoiceCommand = "concord/bus/cp-to-mp/voice-command/v1"`; `FamilyEnforceMute`, `FamilyEnforceDeafen`, `FamilyEnforceDisconnect`, `FamilyEnforcePermissions`, `FamilyUserMute`, `FamilyUserDeafen` = `"concord/bus/cp-to-mp/<kind>/v1"` with kind `enforce-mute`, `enforce-deafen`, `enforce-disconnect`, `enforce-permissions`, `user-mute`, `user-deafen`; `FamilyOwnerControlPlane = "concord/bus/cp-to-kv/owner-record/v2"`, `FamilyOwnerMediaPlane = "concord/bus/mp-to-kv/owner-record/v2"`.
  - No new keyring methods: every surface calls `RawMAC(family, fields)` / `RawVerify(family, kid, fields, mac)` with the field lists below (the primitive adds the version prefix, purpose and `kid` lines).
  - `func FormatDurableProof(kid string, mac []byte) (string, error)`; `func ParseDurableProof(v string) (kid string, mac []byte, err error)`
  - `func FormatCompatProof(kid string, tsMillis int64, nonce string, mac []byte) (string, error)`; `type CompatProof struct{ KID string; TSMillis int64; Nonce string; MAC []byte }`; `func ParseCompatProof(v string) (CompatProof, error)`
  - `func DurableFields(generation string, sequence uint64, body []byte) []string` → `VOICE_COMMANDS_V1`, `voice.commands.v1`, generation, decimal sequence, hex SHA-256 of body.
  - `func CompatFields(subject string, tsMillis int64, nonce string, body []byte) []string` → subject, decimal ts, nonce, hex SHA-256 of body.
  - `type OwnerFields struct{ Bucket, Key, Generation, Writer, Phase, NodeBootID, BirthStreamCreatedAt, BirthOwnerStoreCreatedAt, ProofStreamCreatedAt, ProofOwnerStoreCreatedAt, InitializingRevision string; PrevRevision uint64 }`; `func (o OwnerFields) Lines() []string` → bucket, key, `2`, generation, writer, phase, boot ID, birth stream, birth owner store, proof stream, proof owner store, initializing revision (each `""` when absent), decimal `PrevRevision`.
- Produces (TS, `src/lib/busProof.ts`, same names in camelCase): `PROOF_HEADER`, the nine family constants, `parseDurableProof`, `parseCompatProof`, `durableFields`, `compatFields`, `ownerLines`, and `readProofHeader(headers: MsgHdrs | undefined): string | null` (null unless exactly one key equal to `PROOF_HEADER` byte-for-byte with exactly one value of at most 128 bytes). Signing and verifying use #174's `keyring.rawMac` / `keyring.rawVerify` / `keyring.activeKid()`.
- Produces: `securityevent.ReasonMediaProofRejected ReasonCode = "media_proof_rejected"`; TS reason `'media_proof_rejected'`.

- [ ] **Step 1: Record the merged #174 names**

Run: `git -C <worktree> log --oneline -1 origin/main -- services/control-plane/pkg/busmac services/media-plane/src/lib/busMac.ts` and read both modules.
Expected: both exist with `RawMAC`/`RawVerify`/`ActiveKID` (TS `rawMac`/`rawVerify`/`activeKid`) and the `registry.go` table. Write a short table at the top of `proof.go`'s package comment mapping each provisional name in this plan to the merged name (registry entry function, TS export names, the config field that carries `BUS_MAC_KEYRING` in `services/media-plane/src/config/index.ts`, the Go and TS broker helpers). Confirm the merged MAC input puts `concord-bus-mac/v1` on line 1 and the purpose on line 2; if it differs, follow the merged code and say so in the table. If the merged keyring has no "first signs, rest verify" ordering, add a verify-only slot on every config surface #174 touched and note it here.

- [ ] **Step 2: Write the failing Go golden tests in `proof_test.go`**

```go
func TestProofFamiliesRegistered(t *testing.T)              // each of the 9 families: RawMAC(f, DurableFields/CompatFields/OwnerFields.Lines()) then RawVerify == true (fails today: unregistered families never verify)
func TestProofWrongFamilyFails(t *testing.T)                 // sign FamilyEnforceMute, verify FamilyEnforceDeafen == false
func TestProofMACInputLineOrder(t *testing.T)                // recompute HMAC by hand over "concord-bus-mac/v1\n"+purpose+"\n"+kid+"\n"+fields and compare with RawMAC output
func TestProofVerifyOnlyKIDVerifies(t *testing.T)            // keyring "new:<k2>,old:<k1>": MAC made under "old"-first keyring verifies; ActiveKID()=="new"
func TestDurableFieldsShape(t *testing.T)                    // DurableFields("g", 7, b) == {"VOICE_COMMANDS_V1","voice.commands.v1","g","7",hex(sha256(b))}
func TestCompatFieldsShape(t *testing.T)                     // CompatFields(subject, ts, nonce, b) == {subject, decimal ts, nonce, hex(sha256(b))}
func TestParseCompatProofGrammar(t *testing.T)               // table: valid; kid 17 chars; kid uppercase; ts 12 digits; nonce 21/23 chars; nonce with '='; mac uppercase hex; mac 63 chars; 129-byte value; "v2." prefix — all but first return error
func TestParseDurableProofGrammar(t *testing.T)              // same shape for v1.<kid>.<mac>
func TestOwnerFieldsLinesOrder(t *testing.T)                 // Lines() == []string{bucket,key,"2",gen,writer,phase,boot,bs,bo,ps,po,ir,"7"}
```

- [ ] **Step 3: Run them to verify they fail**

Run: `cd services/control-plane && go test ./pkg/busmac/ -run 'TestProof|TestParse|TestOwnerFields' -v`
Expected: FAIL (undefined: `FamilyEnforceMute`, `ParseCompatProof`, `OwnerFields`).

- [ ] **Step 4: Implement `proof.go` and register the families**

Add the nine families and `ProofHeader` to #174's `registry.go` table. Parsers check length (≤128) and grammar with anchored regular expressions before decoding hex.

- [ ] **Step 5: Run them to verify they pass**

Run: `cd services/control-plane && go test ./pkg/busmac/ -v`
Expected: PASS, including #174's existing vector tests.

- [ ] **Step 6: Write the fixture generator and drift test in `proof_fixture_test.go`**

`TestProofsFixtureMatches` builds the vector list from a fixed test keyring and compares it byte-for-byte with `../../internal/voicecommand/testdata/proofs.json`; with `BUSMAC_WRITE_PROOFS=1` it writes the file instead. Each entry: `name`, `family`, `keyring`, `fields`, `kid`, `mac` (hex), `header` (formatted value or null), `verdict` (`valid` or `invalid`). Cases: one valid per family; `durable-multibyte-max-body` (3584-byte body containing multibyte UTF-8); owner v2 `initializing` (control-plane), `held` (media-plane), `released` by each writer; and one `invalid` per family with a single mutated field.

- [ ] **Step 7: Generate the fixture and run the drift test**

Run: `cd services/control-plane && BUSMAC_WRITE_PROOFS=1 go test ./pkg/busmac/ -run TestProofsFixtureMatches && go test ./pkg/busmac/ -run TestProofsFixtureMatches -v`
Expected: second run PASS.

- [ ] **Step 8: Write the failing TS tests in `tests/busProof.test.ts`**

Load `../../control-plane/internal/voicecommand/testdata/proofs.json` the same way `tests/voiceCommand.test.ts` loads `envelopes.json`. For every entry: `keyring.rawVerify(family, kid, fields, hex(mac)) === (verdict === 'valid')`; for every entry with a `header`, the parser accepts it. Plus `readProofHeader` cases: no headers → null; two values → null; key `concord-proof` → null; both `Concord-Proof` and `concord-proof` → null; 129-byte value → null.

- [ ] **Step 9: Run to verify fail, implement `busProof.ts`, run to verify pass**

Run: `cd services/media-plane && npx vitest run tests/busProof.test.ts`
Expected: FAIL before the module exists; PASS after.

- [ ] **Step 10: Add `media_proof_rejected` to both enums with tests**

Go: `TestReasonMediaProofRejectedAccepted` in `securityevent_test.go` asserts the closed set accepts `"media_proof_rejected"`. TS: the equivalent case in `tests/securityEvent.test.ts`.
Run: `cd services/control-plane && go test ./internal/securityevent/ -v` and `cd services/media-plane && npx vitest run tests/securityEvent.test.ts`
Expected: FAIL, then PASS after adding the constant to the `reasons` set and the TS union and list.

- [ ] **Step 11: Secrets check**

Run: `pre-commit run detect-secrets --files services/control-plane/internal/voicecommand/testdata/proofs.json`
Expected: Passed. If it flags the test keys, add them to `.secrets.baseline` (`git add .secrets.baseline` and re-run until it passes).

- [ ] **Step 12: Commit**

```bash
git add services/control-plane/pkg/busmac services/control-plane/internal/voicecommand/testdata/proofs.json services/control-plane/internal/securityevent services/media-plane/src/lib/busProof.ts services/media-plane/src/lib/busMac.ts services/media-plane/src/lib/securityEvent.ts services/media-plane/tests/busProof.test.ts services/media-plane/tests/securityEvent.test.ts .secrets.baseline
git commit -m "feat: add enforcement proof codec and shared proof vectors"
```

---

### Task 1: Signed compatibility publisher and the six publish sites

**Shape:** construction

**Files:**
- Create: `services/control-plane/internal/enforcementbus/publisher.go`, `publisher_test.go`, `architecture_test.go`, `broker_test.go`
- Modify: `services/control-plane/pkg/nats/client.go:96-104` (add `PublishRaw` next to `Publish`)
- Modify: `services/control-plane/internal/voice/nats.go` (`publishEnforcementFlags` at 9107, disconnect sites at 7831 and 9093, constants at 9063-9065)
- Modify: `services/control-plane/internal/voice/permission_enforcer.go` (interface at 40, constant at 27, publishes at 626 and 651, constructor at 101)
- Modify: `services/control-plane/internal/voice/temp_grants.go:776`, `temp_grant_sweep.go:43`
- Modify: `services/control-plane/internal/dm/handlers.go` (constant at 85, publish at 3918)
- Modify: the construction sites of `NATSSubscriber`, `PermissionEnforcer`, `tempGrantManager`/`TempGrantSweeper` and `dm.Handler` (find with `git grep -n 'NewNATSSubscriber(\|NewPermissionEnforcer(\|NewTempGrantSweeper(\|dm.NewHandler(' services/control-plane -- ':!*_test.go'`)
- Test: caller tests in `internal/voice/permission_enforcer_test.go`, `internal/voice/temp_grants_test.go`, `internal/dm/handlers_test.go`, and a new `internal/voice/enforcement_publish_test.go`

**Interfaces:**
- Consumes: Task 0 families, `CompatFields`, `FormatCompatProof`, `Keyring.RawMAC`.
- Produces:
  - `func (c *Client) PublishRaw(subject string, header map[string][]string, data []byte) error` in `pkg/nats` (sends `nats.Msg{Header: nats.Header(header)}`; never canonicalizes keys).
  - `package enforcementbus`: `type Kind string` with `KindEnforceMute`, `KindEnforceDeafen`, `KindEnforceDisconnect`, `KindEnforcePermissions`, `KindUserMute`, `KindUserDeafen`; exported subject constants `SubjectEnforceMute = "voice.enforce.mute"`, `SubjectEnforceDeafen = "voice.enforce.deafen"`, `SubjectEnforceDisconnect = "voice.enforce.disconnect"`, `SubjectEnforcePermissions = "voice.enforce.permissions"`, `SubjectUserMute = "voice.user_mute"`, `SubjectUserDeafen = "voice.user_deafen"`.
  - `type RawPublisher interface{ PublishRaw(subject string, header map[string][]string, data []byte) error }`
  - `func New(raw RawPublisher, keyring *busmac.Keyring) (*Publisher, error)` (error on nil keyring or no active key); test-only option `WithClock(func() time.Time)` and `WithNonceSource(io.Reader)`.
  - `func (p *Publisher) Publish(kind Kind, payload any) error` — marshals once, signs those exact bytes with a fresh `ts` and 16-byte nonce, publishes with header `{ProofHeader: {value}}` built as a map literal. A nil `*Publisher` returns `ErrPublisherUnavailable`.
  - `var ErrPublisherUnavailable = errors.New("enforcement publisher unavailable")`
  - `NATSSubscriber.SetEnforcementPublisher(*enforcementbus.Publisher)` (a setter, so the 7-argument `NewNATSSubscriber` the #174 exploit tests compile against is unchanged); `voiceEnforcementPublisher` becomes `interface{ Publish(kind enforcementbus.Kind, payload any) error }`; `NewPermissionEnforcer` and `NewTempGrantSweeper` take that interface in place of `*natsclient.Client` for enforcement; `dm.HandlerDeps` gains `Enforcement *enforcementbus.Publisher`.

- [ ] **Step 1: Write the failing publisher tests in `publisher_test.go`**

```go
func TestPublishHeaderExactKeyAndGrammar(t *testing.T)   // fake RawPublisher captures header: len==1, key=="Concord-Proof", one value, ParseCompatProof ok
func TestPublishMACBindsSubjectTSNonceBytes(t *testing.T) // RawVerify(FamilyEnforceMute, kid, CompatFields("voice.enforce.mute", ts, nonce, data), mac)==true; flip one of subject/ts/nonce/byte → false (four sub-tests)
func TestPublishFreshNoncePerCall(t *testing.T)           // two publishes → different nonce
func TestPublishKindSelectsFamilyAndSubject(t *testing.T) // table over the six kinds
func TestNewRefusesNilKeyring(t *testing.T)
func TestNilPublisherReturnsUnavailable(t *testing.T)
```

- [ ] **Step 2: Run to verify fail**

Run: `cd services/control-plane && go test ./internal/enforcementbus/ -v`
Expected: FAIL (package has no non-test files).

- [ ] **Step 3: Implement `PublishRaw` and `enforcementbus.Publisher`**

Imports only `pkg/nats` (for nothing but the interface shape, if needed), `pkg/busmac` and the standard library, so `internal/dm` and `internal/voice` can both import it.

- [ ] **Step 4: Run to verify pass**

Run: `cd services/control-plane && go test ./internal/enforcementbus/ -v`
Expected: PASS.

- [ ] **Step 5: Write the failing architecture test**

`TestEnforcementSubjectLiteralsOnlyInEnforcementbus` parses every non-`_test.go` file under `services/control-plane` with `go/parser` and fails on any `*ast.BasicLit` string equal to one of the six exact subject strings outside `internal/enforcementbus`. It does not match prefixes (the `voice.enforce.*.ack` and `voice.enforce.session.` rail stays legal).
Run: `cd services/control-plane && go test ./internal/enforcementbus/ -run TestEnforcementSubjectLiteralsOnlyInEnforcementbus -v`
Expected: FAIL naming `internal/voice/nats.go`, `internal/voice/permission_enforcer.go`, `internal/dm/handlers.go`.

- [ ] **Step 6: Write the failing caller-level tests, one per site**

In `internal/voice/enforcement_publish_test.go`: `TestPublishEnforcementFlagsUsesSignedPublisher`, `TestJoinDisconnectUsesSignedPublisher` (site 7831), `TestRevokeDisconnectUsesSignedPublisher` (site 9093). In `permission_enforcer_test.go`: `TestRecheckPublishesPermissionsSigned` and `TestRecheckDisconnectSigned` (assert the fake receives `KindEnforcePermissions` / `KindEnforceDisconnect` while `publishMu` is held: the fake tries `TryLock` and expects false). In `temp_grants_test.go`: `TestTempGrantRevokeDisconnectSigned`. In `internal/dm/handlers_test.go`: `TestDMEnforcementPublishSigned`. Each also asserts a nil publisher yields no raw `Publish` on the six subjects.
Run: `cd services/control-plane && go test ./internal/voice/ ./internal/dm/ -run 'Signed' -v`
Expected: FAIL (fields and setters undefined).

- [ ] **Step 7: Move the six sites onto the publisher and delete the subject constants from `voice` and `dm`**

Signing in `permission_enforcer.go` stays inside `publishMu`. Wire the publisher where each type is constructed, built from the `*natsclient.Client` and the keyring the process already parses for #174.

- [ ] **Step 8: Run the package tests**

Run: `cd services/control-plane && go test -p 1 ./internal/enforcementbus/ ./internal/voice/ ./internal/dm/ ./pkg/nats/`
Expected: PASS, including the architecture test. (`internal/voice` and `internal/dm` DB-backed tests need the usual `DATABASE_URL`/`REDIS_URL`.)

- [ ] **Step 9: Write and run the real-broker header-case test (Review Focus 4)**

`TestEnforcementBusHeaderCaseSurvivesBroker` in `broker_test.go`: start the authenticated broker with #174's `natstest.Start(t)`, publish through `Publisher` on `voice.enforce.mute`, receive with a raw subscription and assert `len(msg.Header) == 1` and `msg.Header["Concord-Proof"]` has one value (map index, not `Get`).
Run: `cd services/control-plane && go test ./internal/enforcementbus/ -run TestEnforcementBusHeaderCaseSurvivesBroker -v`
Expected: PASS, not SKIP.

- [ ] **Step 10: Commit**

```bash
git add services/control-plane/internal/enforcementbus services/control-plane/pkg/nats/client.go services/control-plane/internal/voice services/control-plane/internal/dm services/control-plane/cmd/server
git commit -m "feat: sign compatibility enforcement publishes"
```

---

### Task 2: Durable proof header and publish size budget

**Shape:** construction

**Files:**
- Modify: `services/control-plane/internal/voicecommand/contract.go:18-26` (constants)
- Modify: `services/control-plane/internal/voicecommand/outbox.go:46` (`EnqueueTx`)
- Modify: `services/control-plane/pkg/nats/voice_command_publisher.go:160-215` (`PublishVoiceCommandAfter`, `freezeVoiceCommandForPublication`)
- Test: `services/control-plane/internal/voicecommand/outbox_test.go`, `services/control-plane/pkg/nats/voice_command_publisher_test.go`

**Interfaces:**
- Consumes: Task 0 `FamilyVoiceCommand`, `DurableFields`, `FormatDurableProof`, `ParseDurableProof`, `Keyring.RawMAC`.
- Produces: `voicecommand.MaxCommandPublishBodyBytes = 3584`, `voicecommand.MaxCommandHeaderBytes = 512` (`MaxCommandBytes` stays 4096); `func (c *Client) SetVoiceCommandKeyring(k *busmac.Keyring)` on `pkg/nats.Client` (shared with Task 3; whichever task lands first adds it, the other reuses it). A durable publish with a nil keyring returns an error before contacting the broker.

- [ ] **Step 1: Write the failing tests**

```go
// outbox_test.go
func TestEnqueueTxAccepts3584ByteBody(t *testing.T)
func TestEnqueueTxRefuses3585ByteBody(t *testing.T)   // errors.Is(err, voicecommand.ErrInvalidCommand); no row written
// voice_command_publisher_test.go (real broker via natstest)
func TestDurableProofBindsStreamGenerationSequence(t *testing.T) // read back the stored message; header has exactly key "Concord-Proof"; RawVerify(FamilyVoiceCommand, kid, DurableFields(gen, seq, data), mac)==true; same check with seq+1 or another generation == false
func TestDurableProofSignsFrozenBytes(t *testing.T)   // the MAC's body hash equals sha256 of the bytes freeze returned (spy on freeze output), not a re-marshal
func TestDurableMaxBodyMaxHeaderStored(t *testing.T)  // 3584-byte multibyte body (Review Focus 3) with a 16-char kid is stored under MaxMsgSize 4096
func TestDurablePublishRefusesNilKeyring(t *testing.T)
func TestDurableReadbackAcceptsUnsignedRetainedRecord(t *testing.T) // readback stays structural
```

- [ ] **Step 2: Run to verify fail**

Run: `cd services/control-plane && go test -p 1 ./internal/voicecommand/ ./pkg/nats/ -run 'EnqueueTx.*Body|Durable' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

`EnqueueTx` and `freezeVoiceCommandForPublication` refuse a body over `MaxCommandPublishBodyBytes` with `ErrInvalidCommand`. `PublishVoiceCommandAfter` signs the bytes `freezeVoiceCommandForPublication` returned with the receipt's stream generation and the claim's sequence, and adds the header as a map literal alongside the existing `ExpectLastSequence(sequence − 1)` option.

- [ ] **Step 4: Run to verify pass**

Run: `cd services/control-plane && go test -p 1 ./internal/voicecommand/ ./pkg/nats/ ./cmd/server/`
Expected: PASS, no SKIP on the broker tests.

- [ ] **Step 5: Commit**

```bash
git add services/control-plane/internal/voicecommand services/control-plane/pkg/nats
git commit -m "feat: sign durable voice commands and bound publish size"
```

---

### Task 3: Owner record v2 in the control plane, `reseal`, `recover`, `inspect`

**Shape:** construction

**Files:**
- Create: `services/control-plane/pkg/nats/voice_command_owner_v2.go`, `voice_command_owner_v2_test.go`
- Modify: `services/control-plane/pkg/nats/voice_commands.go` (record struct at 50; `initializingVoiceCommandOwner` 156; `InspectVoiceCommandOwner` 301; `verifiedVoiceCommandOwner` 319; `decodeVoiceCommandOwner` 434; `ReleaseInitialOwner` 497; `RecoverVoiceCommandOwner` 536; `NewVoiceCommandClient` 626)
- Modify: `services/control-plane/cmd/server/voice_command_owner_recovery.go` (dispatch at 26, inspect print at 52, `openVoiceCommandOwnerStore` at 133)
- Test: `services/control-plane/cmd/server/voice_command_owner_recovery_test.go`, `services/control-plane/pkg/nats/voice_commands_test.go`

**Interfaces:**
- Consumes: Task 0 `OwnerFields`, owner families, `RawMAC`, `RawVerify`, `ActiveKID`; `SetVoiceCommandKeyring` (Task 2).
- Produces:
  - `voiceCommandOwnerRecord` gains `Writer string` (`control-plane`|`media-plane`), `PrevRevision string` (decimal), `KID string`, `MAC string` (hex); `Version` 2.
  - `func decodeVoiceCommandOwnerV2(data []byte, generation uuid.UUID) (voiceCommandOwnerRecord, error)` (closed fields, 1024-byte cap); the existing decoder is renamed `decodeVoiceCommandOwnerV1`.
  - `func signVoiceCommandOwner(k *busmac.Keyring, r voiceCommandOwnerRecord, prevRevision uint64) (voiceCommandOwnerRecord, error)`
  - `func verifyVoiceCommandOwnerV2(k *busmac.Keyring, r voiceCommandOwnerRecord, entryRevision uint64) error` — MAC under `(writer, kid)`, `entryRevision == prevRevision + 1`, writer/phase allowed (control-plane: `initializing`, `released`; media-plane: `held`, `released`).
  - `func (c *Client) ResealVoiceCommandOwner(ctx context.Context, expectedRevision uint64, receipt voicecommand.TransportReceipt) (wrote bool, err error)`
  - `VoiceCommandOwnerStatus` gains `Version int`, `KID string`, `Sealed bool` (v2 and `KID == ActiveKID()`).
  - CLI: `voice-command-owner reseal --expected-revision N --confirm-media-plane-stopped`, budget `voiceCommandOwnerResealBudget = 10 * time.Second`. Inspect line: `phase=%s revision=%d node_boot_id=%s version=%d kid=%s sealed=%t receipt=verified`.
  - The subcommands parse `BUS_MAC_KEYRING` with `busmac.ParseKeyring` in `openVoiceCommandOwnerStore` and exit 1 with a fixed message when it is missing or invalid.

- [ ] **Step 1: Write the failing codec and verify tests in `voice_command_owner_v2_test.go`**

```go
func TestOwnerV2RoundTrip(t *testing.T)                 // sign then decode then verify at prev+1
func TestOwnerV2RevisionRule(t *testing.T)              // verify at prev+2 and at prev → error
func TestOwnerV2WriterPhaseMatrix(t *testing.T)         // table of 6 writer×phase pairs; only the 4 allowed pass
func TestOwnerV2WrongWriterSubkey(t *testing.T)         // signed as media-plane, writer field says control-plane → error
func TestOwnerV2UnknownFieldRefused(t *testing.T)
func TestOwnerV2OverSizeRefused(t *testing.T)           // 1025 bytes
func TestOwnerV2MatchesProofsFixture(t *testing.T)      // owner vectors from proofs.json verify
```

- [ ] **Step 2: Run to verify fail**

Run: `cd services/control-plane && go test ./pkg/nats/ -run TestOwnerV2 -v`
Expected: FAIL (undefined).

- [ ] **Step 3: Implement the codec, sign and verify**

- [ ] **Step 4: Run to verify pass**

Run: `cd services/control-plane && go test ./pkg/nats/ -run TestOwnerV2 -v`
Expected: PASS.

- [ ] **Step 5: Write the failing writer/reader tests (real broker, in `voice_commands_test.go`)**

```go
func TestInitializeWritesV2InitializingPrevZero(t *testing.T)
func TestReleaseInitialOwnerAcceptsV1AndV2Initializing(t *testing.T) // both at the receipt's initializing revision → v2 released, writer control-plane
func TestRecoverWritesV2FromV1AndV2Held(t *testing.T)
func TestVerifiedOwnerRefusesV1(t *testing.T)                     // readiness: intact v1 released → the same not-ready error the initializing phase gives, not continuity loss
func TestKVUpdateYieldsPrevPlusOne(t *testing.T)                  // real broker pins revision continuity
func TestWriteToOtherOwnerKeyBreaksContinuity(t *testing.T)
```

- [ ] **Step 6: Run to verify fail, implement the readers and writers, run to verify pass**

`verifiedVoiceCommandOwner` and publication readiness accept v2 only. v1 is accepted only by `ReleaseInitialOwner`, `RecoverVoiceCommandOwner`, `ResealVoiceCommandOwner` and `InspectVoiceCommandOwner`.
Run: `cd services/control-plane && go test -p 1 ./pkg/nats/ -v`
Expected: FAIL before, PASS after.

- [ ] **Step 7: Write the failing `reseal` and `inspect` tests in `voice_command_owner_recovery_test.go`**

```go
func TestResealV1ReleasedWritesV2(t *testing.T)
func TestResealAlreadySealedIsSuccessNoWrite(t *testing.T)     // revision unchanged
func TestResealV2UnderVerifyOnlyKeyResignsActive(t *testing.T) // rotation path; MAC checked first
func TestResealRefuses(t *testing.T)                           // sub-tests: held, initializing, garbage, missing, birth mismatch, initializing-revision mismatch, wrong --expected-revision
func TestResealRequiresConfirmFlag(t *testing.T)               // exit 64 without --confirm-media-plane-stopped
func TestResealDeadlineIsTenSeconds(t *testing.T)
func TestInspectPrintsVersionAndKID(t *testing.T)              // v1 → version=1 kid= sealed=false; v2 active → version=2 kid=<active> sealed=true
func TestOwnerSubcommandsRequireKeyring(t *testing.T)          // BUS_MAC_KEYRING unset → exit 1
```

- [ ] **Step 8: Run to verify fail, implement `reseal` and the inspect output, run to verify pass**

Run: `cd services/control-plane && go test -p 1 ./cmd/server/ -run 'Reseal|Inspect|OwnerSubcommands|Recover' -v`
Expected: FAIL before, PASS after.

- [ ] **Step 9: Commit**

```bash
git add services/control-plane/pkg/nats services/control-plane/cmd/server
git commit -m "feat: add signed owner record v2 and reseal command"
```

---

### Task 4: Verified subscription seam, replay guard, branded handlers, bare-lift removal

**Shape:** defect-repair (VULN-148, 149, 150, 151)

**Files:**
- Create: `services/media-plane/src/lib/verifiedBusMessage.ts`, `services/media-plane/src/lib/nonceReplayGuard.ts`
- Modify: `services/media-plane/src/lib/nats.ts` (add `subscribeVerified` beside `subscribe` at 1015)
- Modify: `services/media-plane/src/lib/enforcementCommand.ts:25`, `src/lib/enforcePermissions.ts:302`, `src/lib/voiceCommandDispatcher.ts:99`
- Modify: `services/media-plane/src/index.ts` (move `applyServerEnforcementCommand` at 280-370 into `src/lib/serverEnforcement.ts` as an export; `subscribeVoiceCompatibility` at 1394; handlers at 1455-1570; permissions subscription at 1679)
- Modify: `services/media-plane/src/lib/roomManager.ts:4942-4990` (remove `serverUnmuteUser`, `serverUndeafenUser` if no production caller remains) and their cases in `tests/roomManager.test.ts`
- Test: `services/media-plane/tests/verifiedBusMessage.test.ts` (new), `tests/nonceReplayGuard.test.ts` (new), `tests/enforcementCommand.test.ts`, `tests/enforcePermissions.test.ts`, `tests/voiceCommandCompatibility.test.ts`, `tests/serverEnforcement.test.ts`

**Interfaces:**
- Consumes: Task 0 `readProofHeader`, `parseCompatProof`, `compatFields`, families, keyring `rawVerify`.
- Produces:
  - `class VerifiedBusMessage { readonly subject: string; readonly raw: Uint8Array; readonly data: Record<string, unknown>; static is(value: unknown): value is VerifiedBusMessage }` with a `#brand` private field. The constructor is guarded by a module-private token, so only `verifyCompatibilityMessage` in the same module can construct one.
  - `function verifyCompatibilityMessage(input: { subject: string; family: BusFamily; data: Uint8Array; headers: MsgHdrs | undefined }, ctx: { keyring: BusMacKeyring; replay: NonceReplayGuard; nowMs: number; bootStartedAtMs: number }): { ok: true; message: VerifiedBusMessage } | { ok: false; reason: 'proof' | 'replay_full' | 'schema' }` — steps in order: header grammar; key lookup and HMAC, constant-time; `|nowMs − ts| ≤ 6000` and `ts ≥ bootStartedAtMs`; nonce check-and-insert with no `await`; JSON decode to a plain object.
  - `class NonceReplayGuard { constructor(capacity = 65_536); checkAndInsert(nonce: string, expiresAtMs: number, nowMs: number): 'fresh' | 'replayed' | 'full' }` — prunes expired entries first; never evicts an unexpired one.
  - `NatsService.subscribeVerified(subject: string, family: BusFamily, handler: (message: VerifiedBusMessage) => void | Promise<void>): void` — always `required: true`; `proof` → `media_proof_rejected` and drop; `replay_full` → `media_proof_rejected`, drop and `this.degraded()`; `schema` → `media_schema_rejected`.
  - `handleNatsEnforcementCommand(message: VerifiedBusMessage, allowedActions, handle, emit)`, `handleEnforcePermissionsMessage(roomManager, io, message: VerifiedBusMessage, emit)`, `VoiceCommandDispatcher.handleCompatibility(message: VerifiedBusMessage)` — each refuses a value where `!VerifiedBusMessage.is(value)` with `media_proof_rejected` and no effect. `handleCompatibility` passes `message.raw` to `decodeVoiceCommand`.
  - `applyServerEnforcementCommand` (now exported from `serverEnforcement.ts`) loses `removeAction`/`removeFn`; a lift action without a complete snapshot emits `media_schema_rejected` and does nothing.

- [ ] **Step 1: Write the failing seam tests in `tests/verifiedBusMessage.test.ts`**

Each case mutates one input from a valid message signed with a test keyring:
```ts
it('rejects a missing header')
it('rejects a duplicate header value')
it('rejects a case-variant header key')
it('rejects bad grammar')
it('rejects an unknown kid')
it('rejects a bad mac')
it('rejects a stale ts')                              // nowMs − ts = 6001
it('acceptsTsAtSixSecondBoundaryBothWays')           // ±6000 accepted, ±6001 rejected (Review Focus 1)
it('rejects ts before boot start')
it('rejects a replayed nonce')
it('rejects a cross-subject replay')                 // valid mute proof presented on voice.enforce.deafen
it('never decodes invalid JSON that has no proof')   // spy on JSON.parse / TextDecoder: zero calls
it('verifiesUnderVerifyOnlyKid')                     // Review Focus 2
it('reports replay_full when the guard is full of unexpired nonces')
```
And `tests/nonceReplayGuard.test.ts`: fresh/replayed; expiry frees capacity; full with all unexpired returns `'full'` and keeps every entry.

- [ ] **Step 2: Run to verify fail**

Run: `cd services/media-plane && npx vitest run tests/verifiedBusMessage.test.ts tests/nonceReplayGuard.test.ts`
Expected: FAIL (modules missing).

- [ ] **Step 3: Implement `verifiedBusMessage.ts`, `nonceReplayGuard.ts` and `subscribeVerified`**

- [ ] **Step 4: Run to verify pass**

Run: `cd services/media-plane && npx vitest run tests/verifiedBusMessage.test.ts tests/nonceReplayGuard.test.ts`
Expected: PASS.

- [ ] **Step 5: Write the failing handler tests**

- `tests/enforcementCommand.test.ts`: `refuses a plain object` (handle not called, `media_proof_rejected` emitted).
- `tests/enforcePermissions.test.ts`: `refuses a plain object`; `drops a forged int64-max revision then applies a genuine revoke` (forged one is unsigned through `subscribeVerified` with a fake `nc`; genuine one signed).
- `tests/voiceCommandCompatibility.test.ts`: `refuses a plain object`; `decodes the verified raw bytes, not a re-serialisation` (raw bytes with key order differing from `JSON.stringify(data)` decode as the raw).
- `tests/serverEnforcement.test.ts`: `a signed bare unmute is ignored with media_schema_rejected`; `a signed bare undeafen is ignored`; `a signed complete snapshot lifts`; `a signed bare mute still applies`.
- A wiring test (add to `tests/natsRequiredSubscriptions.test.ts`): with a fake connection, an unsigned message on each of the six subjects reaches no handler, and all six are registered as required.

- [ ] **Step 6: Run to verify fail**

Run: `cd services/media-plane && npx vitest run tests/enforcementCommand.test.ts tests/enforcePermissions.test.ts tests/voiceCommandCompatibility.test.ts tests/serverEnforcement.test.ts tests/natsRequiredSubscriptions.test.ts`
Expected: FAIL.

- [ ] **Step 7: Brand the three handlers, move `applyServerEnforcementCommand` and remove the bare-lift branch, switch the six subscriptions in `index.ts` to `subscribeVerified`**

`subscribeVoiceCompatibility(subject, family, handler)` keys `legacyVoiceCommands` by the `VerifiedBusMessage`. Remove `serverUnmuteUser`/`serverUndeafenUser` and their `roomManager.test.ts` cases if `git grep -n 'serverUnmuteUser\|serverUndeafenUser' services/media-plane/src` shows no caller.

- [ ] **Step 8: Run the media-plane suite**

Run: `cd services/media-plane && npm run typecheck && npx vitest run`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add services/media-plane/src services/media-plane/tests
git commit -m "fix: verify enforcement command proofs before parsing"
```

---

### Task 5: Durable verdict, delivery table, sequence tracker, bind baseline

**Shape:** defect-repair (VULN-153, 154, 155)

**Files:**
- Create: `services/media-plane/src/lib/voiceCommandSequenceTracker.ts`, `services/media-plane/tests/voiceCommandSequenceTracker.test.ts`
- Modify: `services/media-plane/src/lib/busProof.ts` (durable verdict)
- Modify: `services/media-plane/src/lib/nats.ts` (`VoiceCommandRuntime` at 253, `startVoiceCommandConsumer` at 386, `processVoiceCommand` at 478)
- Modify: `services/media-plane/src/lib/voiceCommandDispatcher.ts:57-145` (`applyDurable`, `completedSequence`)
- Test: `services/media-plane/tests/voiceCommandConsumer.test.ts` (real broker), `tests/voiceCommandDispatcher.test.ts`, `tests/voiceCommandStartup.test.ts`

**Interfaces:**
- Consumes: Task 0 `durableFields`, `parseDurableProof`, `readProofHeader`, `FAMILY_VOICE_COMMAND`; Task 4 nothing beyond shared files.
- Produces:
  - `function verifyDurableRecord(input: { subject: string; stream: string; streamSequence: number; data: Uint8Array; headers: MsgHdrs | undefined }, generation: string, keyring: BusMacKeyring): boolean` in `busProof.ts` (subject and stream must equal the constants; `streamSequence` a safe positive integer; MAC over raw bytes; no decode).
  - `type SequencePosition = 'expected' | 'at_or_below_floor' | 'beyond'`
  - `class VoiceCommandSequenceTracker { constructor(baseline: { floor: bigint; pending?: bigint }); position(seq: bigint): SequencePosition; markAuthentic(seq: bigint): void; verifiedAuthentic(seq: bigint): boolean; markDisposalSent(seq: bigint): void; confirm(seq: bigint): void; keepPending(seq: bigint): void }` — expected set is `floor + 1` when nothing is pending, else `pending`, plus `pending + 1` only when `disposalSent`. A `pending` from the baseline starts with `disposalSent` set.
  - `function voiceCommandBindBaseline(input: { createdInThisBind: boolean; ackFloor: bigint; firstSequence: bigint; lastDelivered: bigint; outstanding: number }): { floor: bigint; pending?: bigint }` — throws `startup_unavailable` on any inconsistency (spec 7.3 bind rules).
  - `VoiceCommandDispatcher.applyDurable(command: VoiceCommand, brokerSequence: bigint): Promise<DispatchResult>`; `completedSequence` advances from `brokerSequence` only.

- [ ] **Step 1: Write the failing tracker and baseline tests**

One test per cell of the spec 7.3 table, named by cell (e.g. `expected/authentic/confirmed ack sets floor`, `expected/not-authentic terms and marks disposalSent`, `expected/not-authentic after an authentic verdict drops without term`, `floor/authentic acks already_complete`, `beyond/authentic invalidates`, `beyond/not-authentic terms without tracker change`), plus: `term then redelivery of the same sequence is not a gap`; `successor while predecessor deliberately unacked is beyond`; baseline cases `fresh stream`, `existing durable with purged prefix → floor is ack floor`, `outstanding delivery must equal floor + 1`, `last delivered must equal floor when none outstanding`, `inconsistent → startup_unavailable`.
Run: `cd services/media-plane && npx vitest run tests/voiceCommandSequenceTracker.test.ts`
Expected: FAIL.

- [ ] **Step 2: Implement the tracker and baseline; run to verify pass**

Run: `cd services/media-plane && npx vitest run tests/voiceCommandSequenceTracker.test.ts`
Expected: PASS.

- [ ] **Step 3: Write the failing consumer tests (real broker via `startAuthBroker()`, `tests/voiceCommandConsumer.test.ts`)**

Through the production `processVoiceCommand` seam:
```ts
it('an unsigned one-byte record is termed with no teardown and no readiness change')
it('an unsigned well-formed record with sequence 2^53-1 is termed and does not move the watermark; a later genuine command applies')
it('a forged record beyond the expected set is termed with no teardown')
it('an authentic malformed record keeps teardown-then-term with media_schema_rejected')
it('an authentic record whose body sequence differs from the broker sequence keeps teardown-then-term')
it('an authentic record beyond the expected set invalidates with sequence_gap')
it('a sequence that verified authentic is never termed')
it('durableVerifiesUnderVerifyOnlyKid')                       // Review Focus 2
it('restartRedeliveryAfterUnconfirmedAckIsNotAGap')          // Review Focus 5: apply, drop the ack reply, restart runtime, redelivery acks with no second effect
it('pins the broker ack floor after a term and after a retirement purge')
it('headers survive pull delivery and DeliverAll replay')
```
Run: `cd services/media-plane && npx vitest run tests/voiceCommandConsumer.test.ts tests/voiceCommandStartup.test.ts`
Expected: FAIL.

- [ ] **Step 4: Implement verdict-first `processVoiceCommand`, the tracker in `VoiceCommandRuntime`, the bind baseline in `startVoiceCommandConsumer`, and `applyDurable(command, brokerSequence)`**

The proof verdict comes before any position judgment and before `decodeVoiceCommand`. Only an authentic record reaches the existing quarantine/teardown branch. If the real broker's reported ack floor disagrees with the baseline rule, fail closed (`startup_unavailable`) and record the observed behaviour in a comment next to `voiceCommandBindBaseline`.

- [ ] **Step 5: Run to verify pass**

Run: `cd services/media-plane && npm run typecheck && npx vitest run tests/voiceCommandConsumer.test.ts tests/voiceCommandStartup.test.ts tests/voiceCommandDispatcher.test.ts tests/voiceCommandTerminalization.test.ts`
Expected: PASS, broker tests not skipped.

- [ ] **Step 6: Commit**

```bash
git add services/media-plane/src services/media-plane/tests
git commit -m "fix: bind durable command order to the broker sequence"
```

---

### Task 6: Owner store v2 read and CAS in the media plane

**Shape:** defect-repair (VULN-156)

**Files:**
- Modify: `services/media-plane/src/lib/voiceCommandOwner.ts:120` (`decodeVoiceCommandOwnerRecord`), record type
- Modify: `services/media-plane/src/lib/nats.ts:208-250` (`NatsVoiceCommandOwnerStore.read`, `compareAndSet`)
- Test: `services/media-plane/tests/voiceCommandOwner.test.ts`, `tests/voiceCommandStartup.test.ts`

**Interfaces:**
- Consumes: Task 0 `ownerLines`, owner families, keyring.
- Produces:
  - `function decodeVoiceCommandOwnerRecordV2(raw: Uint8Array, expectedGeneration: string): VoiceCommandOwnerRecordV2` (1024-byte cap, closed fields; decoded fields select nothing before the MAC check).
  - `function verifyVoiceCommandOwnerRecord(record: VoiceCommandOwnerRecordV2, entryRevision: number, keyring: BusMacKeyring): boolean`
  - `function signVoiceCommandOwnerRecord(record: Omit<VoiceCommandOwnerRecordV2, 'kid' | 'mac' | 'writer' | 'prevRevision'>, prevRevision: number, keyring: BusMacKeyring): VoiceCommandOwnerRecordV2` (writer `media-plane`).
  - A v1 value logs the fixed classification `voice_command_owner_v1_record` and then takes the existing fail-closed path.
  - `compareAndSet` fails closed unless the returned revision is exactly `revision + 1`.

- [ ] **Step 1: Write the failing tests**

```ts
it('rejects a record with no mac')
it('rejects a v1 record with the v1 classification')
it('rejects a replayed older valid record')            // valid at revision 3 presented at entry revision 5
it('rejects a wrong writer for the phase')             // media-plane writer with phase initializing
it('rejects an oversize value before decode')
it('a non-consecutive CAS result fails closed')        // fake kv.update returns revision + 2
it('a garbage record fails closed with teardown preserved')
it('renewalAfterPromotionResignsWithActiveKid')        // Review Focus 2
it('accepts the owner vectors from proofs.json')
```
Run: `cd services/media-plane && npx vitest run tests/voiceCommandOwner.test.ts`
Expected: FAIL.

- [ ] **Step 2: Implement the v2 codec and the store changes**

`read()` keeps its birth checks, then size cap, strict v2 decode, MAC, writer/phase, `revision == prevRevision + 1`, proof-to-birth match. `compareAndSet` signs with `prevRevision = revision`.

- [ ] **Step 3: Run to verify pass**

Run: `cd services/media-plane && npm run typecheck && npx vitest run tests/voiceCommandOwner.test.ts tests/voiceCommandStartup.test.ts tests/voiceCommandPromotionRace.test.ts`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add services/media-plane/src services/media-plane/tests
git commit -m "fix: authenticate the voice command owner record"
```

---

### Task 7: Protocol 5, upgrade order, cutover integration test

**Shape:** construction

**Files:**
- Modify: `services/media-plane/src/lib/voiceAuthProtocol.ts` (`voiceEnforcementProtocol: 4` at 96, 139, 156), `services/media-plane/src/lib/voiceEnforcementLease.ts:152,190`
- Modify: `services/control-plane/internal/api/voice_auth_boots.go:64`, `voice_auth_connections.go:286`, `voice_enforcement_sessions.go:339`
- Modify: `services/control-plane/internal/api/testdata/auth_connection_protocol.json` (every `"voiceEnforcementProtocol": 4`)
- Create: `infrastructure/deploy/selfhost-voice-owner.sh`
- Modify: `infrastructure/deploy/concord-selfhost` (`up)` at 435), `infrastructure/deploy/deploy-manifest.txt`
- Test: `services/media-plane/tests/voiceEnforcementLease.test.ts`, `tests/voiceAuthProtocol.test.ts`, the Go tests that load `auth_connection_protocol.json`, `scripts/tests/test-concord-selfhost.sh`, `infrastructure/deploy/tests/test-selfhost-update.sh`

**Interfaces:**
- Consumes: Task 3 CLI (`inspect` line format, `reseal`), Tasks 1–6 behaviour.
- Produces: `voiceEnforcementProtocol` literal `5` in the three result types (Go and TS) and the lease check. `_voice_owner_cutover()` in `selfhost-voice-owner.sh`, called from the new tag's `concord-selfhost up`.
- Unchanged by decision: the health command/ack `protocol: 4` (`Health4`, `HealthAck4`, `voiceauthprotocol/codec.go:98`, `voiceAuthProtocol.ts:76,197,573`) and the login-family rollout receipt `Protocol: 4` (`cmd/server/voice_enforcement_rollout.go:370,424`). They belong to the acknowledgement rail and to a persisted rollout receipt, which the spec keeps out of scope; the lease fence is carried by the results.

- [ ] **Step 1: Write the failing protocol tests**

TS: `voiceEnforcementLease.test.ts` `refuses a result advertising protocol 4`; `accepts protocol 5`. `voiceAuthProtocol.test.ts`: the three result codecs round-trip `voiceEnforcementProtocol: 5` and reject 4. Go: update the fixture cases to 5 and add one case per result type with 4 expected to be refused by the TS decoder (the fixture is shared).
Run: `cd services/media-plane && npx vitest run tests/voiceEnforcementLease.test.ts tests/voiceAuthProtocol.test.ts` and `cd services/control-plane && go test ./internal/api/ -run 'Protocol|AuthConnection' -v`
Expected: FAIL.

- [ ] **Step 2: Bump the literals and constructors to 5; run to verify pass**

Then check `/readyz`: `git grep -n 'voiceEnforcementProtocol\|protocol' services/media-plane/src/index.ts | sed -n '/readyz/,+20p'`. If the body carries the number, bump it and its test; if it does not, the only `/readyz` reference is the rule-file text Task 9 fixes.
Expected: PASS on both commands from Step 1.

- [ ] **Step 3: Write the failing upgrade-order tests in `scripts/tests/test-concord-selfhost.sh`**

Stub `compose`, `guarded_ctl` and the CLI the way the existing `_authority_lifecycle` stubs work (line 287). Cases, each asserting the recorded call order:
- `sealed`: stop media-plane, stop control-plane, `concord-selfhost nats-credentials ensure` (the #174 standalone step), start control-plane, inspect, start media-plane.
- `credentials ensure fails`: stops before the control plane starts; neither plane is started.
- `v1 released` (`version=1 phase=released`): as above with `reseal --expected-revision <N> --confirm-media-plane-stopped` between inspect and media-plane start.
- `initializing`: continue without reseal.
- `held`: stops with a message naming the runbook `recover` step; media-plane is never started.
- any other output or a failed inspect: stops; media-plane is never started.
Also extend `infrastructure/deploy/tests/test-selfhost-update.sh` with one case asserting `update` still calls the new tag's `up` (the order lives there, because `selfhost-update.sh` runs under the previous release's shell).

- [ ] **Step 4: Run to verify fail**

Run: `bash scripts/tests/test-concord-selfhost.sh && bash infrastructure/deploy/tests/test-selfhost-update.sh`
Expected: FAIL on the new cases.

- [ ] **Step 5: Implement `_voice_owner_cutover` and wire it into `up`**

Media-plane must not start in any step before the inspect branch returns continue. The `nats-credentials ensure` step runs after both planes stop and before the control plane starts; when `up` already ran `nats_credentials_ensure` (#174's `up` line), the second call is the idempotent no-op #174 tests for. Run the subcommands inside the control-plane container the same way `docs/runbooks/selfhost-release.md` runs `voice-command-owner recover`. Add the new script to `deploy-manifest.txt`.

- [ ] **Step 6: Run to verify pass, plus the deploy suite**

Run: `bash scripts/tests/test-concord-selfhost.sh && bash infrastructure/deploy/tests/test-selfhost-update.sh && bash scripts/tests/run-selfhost-deploy-tests.sh`
Expected: PASS.

- [ ] **Step 7: Write and run the cutover integration test**

`TestCutoverMixedPairsFailClosed` in `services/control-plane/cmd/server/voice_command_transport_test.go`: a protocol-4 result is refused by the TS decoder fixture and a protocol-4 media plane's lease result is refused by the Go side's fixture expectation (both pairs end with no lease). `TestCutoverV1OwnerResealThenStart`: real broker; write a v1 `released` record, run `reseal`, then the v2 reader accepts it.
Run: `cd services/control-plane && go test -p 1 ./cmd/server/ -run Cutover -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add services/media-plane services/control-plane infrastructure/deploy scripts/tests
git commit -m "feat: bump voice enforcement protocol to 5 and order the owner cutover"
```

---

### Task 8: Shipped-config broker test and exploit run

**Shape:** construction

**Files:**
- Create: `services/control-plane/internal/natsconformance/enforcement_proof_test.go` (#174's conformance package)
- Test only; no production change unless a result below is wrong.

**Interfaces:**
- Consumes: #174 `natstest.Start(t)` with the shipped `nats-cluster.conf`; Tasks 1–7.

- [ ] **Step 1: Write the criterion 9 test**

```go
func TestOnlyControlPlanePublishesEnforcementAndCommands(t *testing.T) // six subjects + voice.commands.v1: CP allowed; MP and ops-agent refused
func TestOnlyMediaPlanePullsAndAcksDurable(t *testing.T)               // CONSUMER.MSG.NEXT and $JS.ACK on voice-enforcement-v1: MP only
func TestOnlyServiceIdentitiesWriteOwnerKey(t *testing.T)              // $KV.VOICE_COMMAND_OWNER_V1.authoritative: CP and MP only; other key refused
func TestNoIdentityReachesMediaPlanePullInbox(t *testing.T)            // publish and subscribe on _INBOX.mp.> from CP and ops-agent refused
func TestMediaPlaneStartupAndRenewalNeedNoExtraPermission(t *testing.T) // full MP startup + two renewals, zero permission violations
func TestConsumerTheftStepsRefused(t *testing.T)                       // repeats the exact steps of test_vuln_212 against the shipped config
```

- [ ] **Step 2: Run it**

Run: `cd services/control-plane && go test ./internal/natsconformance/ -run 'Enforcement|Durable|OwnerKey|PullInbox|Startup|ConsumerTheft' -v`
Expected: PASS, no SKIP.

- [ ] **Step 3: Run the nine scan exploit tests and record results**

Run each file listed in spec 13.5 per the scan's own instructions. Expected per file: as in spec 13.5 (`test_vuln_212` passes and is covered by Step 2 and Task 5). A file that still passes otherwise is a defect: fix it in the owning task's files before merge. Record the per-file result in the PR body.

- [ ] **Step 4: Commit**

```bash
git add services/control-plane/internal/natsconformance/enforcement_proof_test.go
git commit -m "test: pin enforcement proof broker permissions"
```

---

### Task 9: Docs, rules, changelog

**Shape:** construction

**Files:**
- Modify: `docs/architecture.md`, `.claude/rules/domains/media-plane.md`, `.claude/rules/domains/backend.md`, `docs/runbooks/selfhost-release.md`, `docs/api/README.md`, `CHANGELOG.md`

- [ ] **Step 1: Update each document with the content listed in spec §14**

`media-plane.md` also gains the verify-before-parse rule (with the owner-record exception) and the `/readyz` protocol text set to 5. `backend.md` states that enforcement publishes go only through `internal/enforcementbus` and the 3584/512 budget. The runbook covers: both planes upgrade together; the `up` order from Task 7; `reseal`; the `held` case; the bare `compose up` failure signature (`voice_command_owner_v1_record`); key rotation and its drop precondition. `CHANGELOG.md` records a breaking internal protocol change.

- [ ] **Step 2: Verify**

Run: `pre-commit run --files docs/architecture.md .claude/rules/domains/media-plane.md .claude/rules/domains/backend.md docs/runbooks/selfhost-release.md docs/api/README.md CHANGELOG.md`
Expected: Passed. Then `git grep -n 'unmute\|undeafen' -- .claude/rules docs/architecture.md` shows no text describing bare lifts as supported.

- [ ] **Step 3: Commit**

```bash
git add docs .claude/rules CHANGELOG.md
git commit -m "docs: document enforcement origin proof and owner v2 cutover"
```

---

## Spec-to-code notes for executors

- **Health protocol field.** Spec §5 says the health request carries no protocol field. The code has one (`Health4.Protocol`, `HealthAck4.protocol`, both `4`). This plan keeps it at 4 (Task 7 "Unchanged by decision"). The spec's conclusion (no control-plane refusal of protocol-4 peers) still holds because the lease checks the result's `voiceEnforcementProtocol`.
- **Updater location.** Spec §9.3 names `selfhost-update.sh`, but that script runs under the previous release's shell and calls the new tag's `concord-selfhost up` (as #174 §5.7 also found). The order therefore lives in `up` (Task 7).
- **`applyServerEnforcementCommand`** lives in `src/index.ts`, not a lib file. Task 4 moves it to `src/lib/serverEnforcement.ts` so it can be tested.
- **MAC line order.** Spec §6 puts the purpose on line 1. #174's merged `RawMAC` puts `concord-bus-mac/v1` on line 1, the purpose on line 2 and `kid` on line 3. This plan follows #174; Task 9 states the merged order in `docs/architecture.md`.
- **Exploit-test compile surface (#174 §3).** `NewNATSSubscriber` keeps its 7 arguments; the publisher arrives by setter.

