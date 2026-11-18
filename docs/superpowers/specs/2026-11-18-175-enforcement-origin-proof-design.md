# Enforcement Origin Proof Design (Security#175)

Status: final design, reconciled after specialist review. Lands after Security#174. Control plane and media plane upgrade together.

## 1. Problem

The media plane applies moderation, permission and disconnect commands from NATS and treats bus position as proof of origin. Nine scan findings (VULN-148, 149, 150, 151, 153, 154, 155, 156, 212) follow from four gaps:

- No origin proof on any enforcement subject or on the durable `voice.commands.v1` record.
- The replay watermark advances from a sender-chosen `sequence` field.
- The owner record in KV bucket `VOICE_COMMAND_OWNER_V1` is accepted on shape alone.
- One undecodable record triggers the instance-wide teardown.

Security#174 delivers broker identities, mTLS and the full permission matrix. This change adds what a broker permission cannot give: a keyed MAC verified before a command is parsed, ordering bound to the broker-assigned sequence, an authenticated owner record, and a teardown that unauthenticated input cannot reach.

## 2. Goals and non-goals

**Goals**
- Every enforcement command, durable record and owner record carries an HMAC that the media plane verifies before it applies anything.
- Durable ordering comes from the broker's stream sequence, never from a payload field.
- An unauthenticated command record cannot cause a teardown or move the watermark.
- The upgrade loses no moderation.

**Non-goals**
- Broker identities and permissions. They ship in #174; this change only verifies them.
- Retiring the compatibility subjects. That is a later soak-gated change.
- Re-keying the request/reply acknowledgement rail (`voice.enforce.disconnect.ack`, `voice.enforce.credential_epoch.ack`, `voice.enforce.session.<boot>`). It keeps its existing proofs.
- A revision window for permission pushes.
- A legacy or unsigned mode.

## 3. Settled decisions (owner, not open)

- One HMAC-SHA256 scheme covers compatibility subjects, durable records and the owner record.
- The key root is the dedicated secret from #174, never `JWT_SECRET`. Each direction and kind has its own subkey. The keyring carries a key ID.
- Durable ordering binds to `info.streamSequence`. The gap check runs against the last disposed sequence.
- The owner-record MAC binds `prevRevision`. A record is valid only when `entry.revision == prevRevision + 1`.
- Compatibility subjects get a nonce replay guard on top of freshness. Durable records have no wall-clock window, because they are re-verified on replay.
- `voice.user_mute`, `voice.user_deafen` and `voice.enforce.deafen` are kept and signed.
- Bare `unmute` / `undeafen` are removed. A lift needs a signed, revisioned snapshot.
- VULN-151 is closed by the MAC alone. The revision fence stays monotonic with no window.
- A lost or garbage owner record stays fail-closed. An unauthenticated command record is termed and logged without teardown.

## 4. Constraints

| Constraint | Source |
|---|---|
| No JSON decode, schema check or room lookup runs on command bytes whose MAC has not verified. The owner record has one bounded exception (section 7.4). | New rule, added to `.claude/rules/domains/media-plane.md` |
| The stream and consumer contract is frozen: one pull durable, explicit ack, DeliverAll, MaxAckPending one, MaxMsgSize 4096, no purge or repair. | `media-plane.md`, "Durable voice command consumer" |
| Media startup never creates or repairs ownership resources. Every read and CAS checks resource births. Ack and term need current owner and consumer continuity. | same section |
| The strict envelope codec and `testdata/envelopes.json` stay authoritative for both suites. | same section |
| All compatibility handlers and the full required-subscription set stay. No required subscription may silently no-op. | `media-plane.md`, "Required subscription availability" |
| Logs carry fixed classifications only: no payloads, identifiers, subjects or raw errors. | `media-plane.md` |
| Constant-time comparison. A nil or short key never verifies. No blank error discards without the documented reason. | `.claude/rules/domains/backend.md` |
| Tests exercise the production consumer seam, prove the reject branch is reached, and use fixtures that can fail for one reason only. Real JetStream tests use the pinned broker resolver and never skip. | `.claude/rules/domains/tests.md`, `media-plane.md` |
| No migration. Outbox rows are columns and are signed at publish. | system review |

## 5. Approaches considered and reconciliation record

**Approaches.** Three carriers were compared: a detached MAC in a NATS header over raw bytes, proof fields in the JSON body, and a signed wrapper object. The header was chosen for the stream and the six subjects: the strict body codec, its fixtures and the control-plane readback stay unchanged, and the MAC covers bytes as received. The KV API takes no headers in either SDK, so the owner record carries its MAC in the value. For cutover, terming pre-upgrade records behind a protocol fence was chosen over a stream generation rollover (voice down until an operator completes it) and a grandfather floor (accepts unauthenticated input).

**Reconciliation.** "Draft" is the pre-review design; "review" is the systems review.

| Topic | Positions | Final | Why |
|---|---|---|---|
| Carrier vs #174's own carrier | Draft: revisit if #174 carries its proof in the body. | Header carriage stands regardless. | The module supplies the MAC primitive; the carrier belongs to each surface. |
| Owner record and verify-before-parse | Neither named the conflict. | Bounded exception, stated in 7.4. | The MAC is inside the value, so a decode must come first. Every failure on that path has the same fail-closed outcome. |
| Reseal of an `initializing` record | Draft: reseal `released` or `initializing`. Review: never `initializing`. | Review wins. | The release path requires the entry at exactly the receipt's initializing revision (`verifiedVoiceCommandOwner`). A reseal moves the revision and voice could never start. |
| Reseal trigger | Draft: automatic at control-plane start. Review: a bounded command run by the updater. | Review wins. | A v1 record cannot be authenticated. The safety of sealing it rests on the media plane being stopped, which only the updater or operator can assert. Automatic sealing would bless a downgraded record at every start. |
| What counts as an uncertain disposal | Draft: an uncertain ack. Review: also every term, every unacked transient failure, the graceful-stop withhold. | Review wins, with one tightening. | `term()` returns nothing. Tightening: a successor that arrives while the media plane has deliberately not acked its predecessor means someone else acked it, and fails closed. |
| Order of proof and sequence checks | Both: sequence position first. | Proof verdict first (7.3). | Position-first lets an unauthenticated delivery reach the teardown path. Only an authenticated record can prove a gap. |
| Bind baseline | Draft: larger of ack floor and first sequence − 1. Review: first sequence only for a durable created in this bind. | Review wins. | For an existing durable, a difference means unconsumed records were purged. |
| Header bytes and `MaxMsgSize` | Draft: unconfirmed risk. Review: they count; the 4096 body bound is already unreachable. | Review wins; fixed in this change. | This change adds header bytes, so it owns the budget. |
| Architecture test | Draft: any enforcement subject literal. Review: the six exact strings. | Review wins. | A prefix match would flag the acknowledgement rail, which is out of scope. |
| Control-plane refusal of protocol-4 peers | Review: add it. | Not adopted. | The health request carries no protocol field. Both mixed pairs already fail closed at the media-plane lease check. Adding a field changes a closed wire shape for no extra coverage. |
| Reject classification | Issue: reuse `media_schema_rejected`. Review: new `media_proof_rejected`. | Review wins. | A forged command is an intrusion signal; a schema reject is a bug signal. |
| Bare restrictive `mute` / `deafen` | Review asked whether to remove them. | Kept, signed. | The decision removes lifts only. They can only restrict, and the kept user-mute subjects have the same shape. |
| Nonce guard eviction | Decision text says "nonce/LRU". | Bounded set that evicts by expiry only. | Evicting an unexpired nonce under pressure would reopen replay. |
| Record with a valid MAC that fails schema | Both: keep the conservative teardown. | Kept, pending owner confirmation (section 17). | Only the genuine control plane can reach it, and silently dropping a command that may be a revoke is fail-open. |
| Fixtures | Issue: extend `envelopes.json`. Both: sibling `proofs.json`. | `proofs.json`; `envelopes.json` untouched. | Follows from header carriage. |

## 6. Wire contract

All MACs are HMAC-SHA256 under a subkey derived per purpose from the keyring entry named by `kid`. The MAC input is a list of lines joined by `\n`. Line 1 is the purpose string. Every field is newline-free by grammar, and the signer refuses a field that contains a newline. Names marked provisional are fixed by task T0 against the merged #174 module.

**Proof header** (name provisional: `Concord-Proof`; it must not start with `Nats-`):
- Exactly one key and one value. A case variant of the name, or a second value, rejects.
- At most 128 bytes. Grammar is checked before any HMAC work.
- Durable: `v1.<kid>.<mac>`. Compatibility: `v1.<kid>.<ts>.<nonce>.<mac>`.
- `kid` is `[a-z0-9]{1,16}`; `ts` is 13-digit unix milliseconds (provisional unit); `nonce` is 22 characters of unpadded base64url (128 bits); `mac` is 64 lowercase hex characters.

| Surface | Purpose (provisional strings) | MAC input lines after the purpose |
|---|---|---|
| `voice.commands.v1` record | `concord/bus/cp-to-mp/voice-command/v1` | `kid`, `VOICE_COMMANDS_V1`, `voice.commands.v1`, stream generation, decimal broker stream sequence, hex SHA-256 of the raw body |
| Six compatibility subjects | `concord/bus/cp-to-mp/<kind>/v1`, where kind is `enforce-mute`, `enforce-deafen`, `enforce-disconnect`, `enforce-permissions`, `user-mute` or `user-deafen` | `kid`, subject, `ts`, `nonce`, hex SHA-256 of the raw body |
| Owner record v2 | `concord/bus/cp-to-kv/owner-record/v2` or `concord/bus/mp-to-kv/owner-record/v2` | `kid`, bucket, key, `2`, generation, writer, phase, then boot ID, both birth timestamps, both proof timestamps and the initializing revision (each or empty), then `prevRevision` |

For a durable record the verifier supplies the generation from its own binding and the sequence from `message.info.streamSequence`. Nothing in the MAC input comes from the parsed body.

**Owner record v2** (JSON value, closed fields):

| Field | Type | Notes |
|---|---|---|
| `version` | int | Literal `2`. |
| `streamGeneration`, `phase`, `nodeBootId`, `birth` / `proof` | as today | Grammar unchanged. |
| `writer` | `control-plane` or `media-plane` | Selects the subkey. |
| `prevRevision` | decimal string | The revision this write replaced. `0` only for the first `Create`. |
| `kid`, `mac` | as above | |

A record is valid only when all of these hold: the entry is a PUT; `entry.revision == prevRevision + 1`; the MAC verifies under the subkey for `(writer, kid)`; and the writer/phase pair is allowed. The control plane writes `initializing` and `released`. The media plane writes `held` and `released`.

## 7. Media plane

### 7.1 Verified subscription seam

`subscribeVerified(subject, purpose, handler)` in `src/lib/nats.ts` replaces the decode-first loop for the six subjects and keeps their required-subscription registration. Its steps run in this order:

1. Header presence and grammar.
2. Key lookup, HMAC over the raw bytes, constant-time compare.
3. Freshness: `ts` within 6 s of now, and not before this boot's wall-clock start.
4. Nonce check and insert, with no `await` between them.
5. JSON decode, then the handler.

- **Replay guard.** A nonce is recorded only after its MAC verifies, so unauthenticated traffic cannot fill the cache. It is held until `ts + 6 s`. The cache holds at most 65,536 entries and evicts by expiry only. If it is full of unexpired entries, the message is dropped and enforcement availability is invalidated through the existing degraded rail. A revoke is never silently lost.
- **Verified type.** The handler receives a `VerifiedBusMessage` whose private-field brand only the seam can set. `handleNatsEnforcementCommand`, `handleEnforcePermissionsMessage` and `VoiceCommandDispatcher.handleCompatibility` check the brand at run time and refuse a plain object. No production export can mint one without a MAC check. Tests mint through the seam with a test key.
- `handleCompatibility` passes the verified raw bytes to the strict decoder instead of re-serialising the parsed object.

### 7.2 Compatibility handlers

- `applyServerEnforcementCommand` loses the bare remove branch and its `removeFn` / `removeAction` plumbing.
- A complete snapshot still lifts. The control plane's snapshot always carries `action` `mute` or `unmute`, so the allowed-action list is unchanged.
- A signed message with a lift action and no complete snapshot is ignored with `media_schema_rejected`.
- Bare `mute` / `deafen` and the two user-mute subjects still apply. They can only restrict.
- Permission pushes keep the existing monotonic fence.
- If `RoomManager.serverUnmuteUser` / `serverUndeafenUser` lose their last production caller, remove them and their tests in the same task.

### 7.3 Durable consumer

**Verdict first.** For every delivery, `processVoiceCommand` first checks that the subject and stream are the expected constants and `info.streamSequence` is a safe positive integer. It then verifies the proof over the raw bytes and headers. The result is *authentic* or *not authentic*. Only then is the sequence position judged.

**Tracker** (in `VoiceCommandRuntime`):
- `floor`: the highest sequence whose ack or term is confirmed.
- `pending`: the one delivered sequence whose disposal is not confirmed. It carries two flags. `authentic` is set if any delivery of it verified in this bind. `disposalSent` is set if the media plane itself sent an ack or term for it.
- The expected next sequence is `floor + 1` when nothing is pending. Otherwise it is `pending`, plus `pending + 1` only if `disposalSent` is set.

| Position | Authentic | Not authentic |
|---|---|---|
| Expected | Strict decode, apply, `ackAck`. A confirmed ack sets `floor`. An unconfirmed ack leaves it pending with `disposalSent`. A transient apply failure leaves it pending without `disposalSent`. | Term under current continuity, mark pending with `disposalSent`, stay ready. No parse, no quarantine, no teardown. Exception: if this sequence already verified authentic in this bind, drop it without terming. |
| At or below `floor` | Idempotent `already_complete` ack. No effect. | Term. No tracker change. |
| Beyond the expected set | Authority loss: the existing invalidate path (fail closed). | Term. No tracker change, no teardown. A real gap surfaces at the next authentic record. |

- An authentic record that fails the strict decode, or whose body `sequence` differs from the broker sequence, keeps today's conservative teardown-then-term with `media_schema_rejected`. Only a control-plane defect can reach it.
- An authentic delivery supersedes an earlier unauthenticated verdict for the same expected sequence.
- `applyDurable` takes the verified record and the broker sequence. `completedSequence` advances from the broker sequence only.

**Bind baseline.**
- Existing durable: `floor` is the consumer's ack floor. If one delivery is outstanding, it becomes `pending` with `disposalSent` set and must equal `floor + 1`. Otherwise the last delivered sequence must equal `floor`.
- Durable created in this bind: `floor` is the stream's first sequence − 1.
- Any inconsistency is `startup_unavailable`.
- Real-broker tests pin what the broker reports after a term and after a retirement purge. Where the broker's behaviour and this rule differ, fail closed.

### 7.4 Owner store

- `read()` keeps its birth checks, then: a 1024-byte size cap, strict v2 decode, MAC verification, the writer/phase check, `revision == prevRevision + 1`, and the proof-to-birth match. Any failure is the existing fail-closed path.
- **Exception to verify-before-parse.** The MAC is inside the value, so the bounded strict decode runs first. No decoded field changes state or selects behaviour before the MAC verifies. A detached prefix in the value was considered and not adopted: every failure here has the same outcome, and a non-JSON value would change every reader and tool.
- A v1 record is refused like any other invalid record. It gets its own fixed log classification, so the runbook can name the upgrade failure signature.
- `compareAndSet` signs as `media-plane` with `prevRevision` set to the replaced revision. It fails closed unless the returned revision is exactly that + 1 (today it only requires a larger one).

### 7.5 Failure classes

No row logs a payload, identifier, subject or raw error.

| Input | Outcome | Classification |
|---|---|---|
| Compatibility message failing any of steps 1 to 4 | Dropped before decode. No state change. | `media_proof_rejected` |
| Compatibility message, verified, schema reject or bare lift | Dropped | `media_schema_rejected` |
| Replay cache full | Message dropped and availability invalidated | `media_proof_rejected` |
| Durable record, not authentic | Per the table in 7.3. Never a teardown. | `media_proof_rejected` |
| Durable record, authentic, malformed | Conservative teardown, then term | `media_schema_rejected` |
| Durable record, authentic, beyond the expected set | Invalidate (`sequence_gap`) | — |
| Owner record invalid in any way | Existing fail-closed path | fixed owner classification |
| Control-plane signer has no active key | Publish refused; production start is fatal (#174 guard) | — |

## 8. Control plane

### 8.1 Signed compatibility publisher

A new leaf package (suggested `internal/enforcementbus`) owns the six subject constants. It marshals once, signs those exact bytes with a fresh `ts` and nonce, and publishes with the header. It imports only `pkg/nats` and the MAC module, so `internal/dm` and `internal/voice` can both use it without a cycle. `pkg/nats.Client` gains a raw message publish.

Six raw `Publish` sites move onto it:
- `internal/voice/nats.go`: `publishEnforcementFlags` and two disconnect sites.
- `internal/voice/permission_enforcer.go`: permissions and disconnect. Signing stays inside `publishMu`.
- `internal/voice/temp_grants.go`.
- `internal/dm/handlers.go`.

An architecture test fails on any of the six exact subject strings in non-test Go outside the package.

**The three kept subjects** have no control-plane call site in this repo. The package exposes their kinds, the golden vectors cover them, and the media plane refuses them unsigned. No call site is added for a message that no code path sends.

### 8.2 Durable publish

- `PublishVoiceCommandAfter` signs the bytes returned by `freezeVoiceCommandForPublication`, never a re-marshal, with the claim's sequence. The publisher already enforces `ExpectLastSequence(sequence − 1)` and `ack.Sequence == sequence`.
- The header is built as a map literal so its key keeps exact case.
- Readback stays structural and does not require a proof, because unsigned records remain in the retained suffix.
- **Size budget.** `MaxCommandHeaderBytes = 512`. `EnqueueTx` and `freezeVoiceCommandForPublication` refuse a body over 3584 bytes with `ErrInvalidCommand`. The decoder bound stays 4096 in both languages, so `envelopes.json` boundary cases do not move. The refusal belongs at enqueue: `voiceCommandPublisherError` does not map a broker size refusal to a terminal class, so it would be retried at the outbox head and block all later moderation.

### 8.3 Owner record writers and readers

| Path | Reads | Writes |
|---|---|---|
| `initializingVoiceCommandOwner` | — | v2 `initializing` by `Create`, `prevRevision` 0 |
| `ReleaseInitialOwner` | v1 or v2 `initializing` at exactly the receipt's initializing revision | v2 `released` |
| `RecoverVoiceCommandOwner` (existing operator command) | v1 or v2 `held`, exact revision and boot ID | v2 `released` |
| `reseal` (new operator command) | v1 `released`, or v2 `released` under a verify-only key (MAC checked first) | v2 `released` under the active key |
| `verifiedVoiceCommandOwner`, publication readiness | v2 only, MAC and revision rule checked | — |

- `control-plane voice-command-owner reseal --expected-revision N --confirm-media-plane-stopped` has a ten-second deadline.
- It refuses `held`, `initializing`, garbage and missing records, any birth or initializing-revision mismatch with the SQL receipt, and a wrong revision.
- A record already sealed under the active key is a success with no write.
- Sealing an unauthenticated v1 `released` record is safe only because the media plane is stopped: a false `released` matters only while a predecessor is still forwarding media.
- For publication readiness, an intact v1 record is "not ready, retry" (like `initializing`), not continuity loss.
- `inspect` prints the record version and key ID alongside phase and revision.
- v1 is accepted only at the three conversion call sites and `inspect`.

## 9. Cutover and rollout

1. **Security#174 merges first.**
2. **Protocol fence.** `voiceEnforcementProtocol` goes from 4 to 5:
   - TypeScript: `voiceAuthProtocol.ts` literal types, `voiceEnforcementLease.ts`.
   - Go: three result constructors in `internal/api`.
   - Shared fixture: `internal/api/testdata/auth_connection_protocol.json`.
   - The `/readyz` body.

   A new media plane refuses an old control plane's result, and an old media plane refuses the new one. Both pairs end with no lease and no admission. Without the bump, a new media plane beside an old control plane would term every record and report ready.
3. **Updater order** in `infrastructure/deploy/selfhost-update.sh` (shared with #174):
   1. Stop the media plane gracefully, so it releases.
   2. Stop the control plane.
   3. Start the control plane.
   4. Run `inspect` and branch:
      - sealed: continue;
      - v1 `released`: run `reseal`;
      - `initializing`: the control plane's own start releases it;
      - `held`: stop and point to the runbook's `recover` step;
      - anything else: stop.
   5. Start the media plane.

   A bare `compose up` leaves a v1 record. The media plane refuses it with the fixed classification and the runbook covers the step.
4. **Stream records.** No rewrite. Unsigned pre-upgrade records are termed. No moderation is lost:
   - every effectful record names a `target.nodeBootId`, and the dispatcher yields no effect for a different boot;
   - the upgrade restarts the media plane, and rejoin authorization reads current state and re-pushes snapshots.

   Outbox rows enqueued before the upgrade are signed when published.
5. **Key rotation.**
   1. Add the new key as verify-only on both planes.
   2. Promote it. A held owner record re-signs on the next two-second renewal; a released one is resealed.
   3. Drop the old key only after the media plane has stayed ready since promotion and the control-plane outbox is drained. A record still unconsumed when its key is dropped would be termed.
6. **Config.** No new environment variable if #174's keyring has an active key and a verify-only slot. Otherwise T0 adds the slot on every config surface #174 touched.

## 10. Limits

- A MAC proves who wrote a record, not who delivered it. Deleting stream records, draining the consumer, stalling the publisher's sequence fence with a foreign record, and injecting deliveries into the media plane's pull inbox are all stopped by broker permissions, not by this change.
- After a foreign record enters the stream, the media plane stays ready and terms it, but the control plane's expected-sequence fence stops further durable publishes until an operator reconciles. Snapshots keep flowing on the compatibility subjects.
- A drain that happens while the media plane is down is invisible to the tracker at bind.
- The key is symmetric. Per-direction subkeys stop reflection; they do not stop a compromised plane minting the other's MACs. On a single host that plane already controls its own enforcement.

## 11. Requirements on Security#174

1. A keyring with an active key ID and at least one verify-only slot, available to both planes and to the `voice-command-owner` subcommands.
2. A MAC primitive with no time window, with caller-supplied purpose strings and constant-time verification, in Go and TypeScript. Its registry holds the header name and the nine purposes.
3. Owner bucket publish permission scoped to the exact subject `$KV.VOICE_COMMAND_OWNER_V1.authoritative` for the two identities. A write to any other key breaks revision continuity.
4. No other identity may publish to, or subscribe on, the media plane's pull inbox subjects.
5. A missing or short secret is fatal at production start on both planes and in the subcommands.
6. The updater order in section 9 is shared.

This design adds no broker permission need for either identity.

## 12. Acceptance criteria

| # | Criterion | Evidence |
|---|---|---|
| 1 | A forged or unsigned message on any of the six compatibility subjects is dropped before any JSON decode. | Seam tests with a decoder spy |
| 2 | A forged permission grant cannot raise permissions or pin the revision; a genuine revoke applies afterwards. | `enforcePermissions` tests |
| 3 | A forged durable record with any `sequence` is termed and does not move the watermark; a later genuine command applies. | Consumer tests |
| 4 | An unauthenticated command record, well-formed or garbage, at any sequence position, causes no teardown and no readiness change. | Consumer tests |
| 5 | A forged, v1 or replayed owner record is never accepted as `held`. A lost or garbage owner record still fails closed. | Owner tests |
| 6 | An authentic record beyond the expected set fails closed. A redelivery after an unconfirmed term or ack is not a gap. A successor arriving while its authentic predecessor is deliberately unacked fails closed. A sequence that verified authentic is never termed. | Consumer tests |
| 7 | Bare `unmute` / `undeafen` have no effect; a signed complete snapshot lifts. | Handler tests |
| 8 | `voice.user_mute`, `voice.user_deafen` and `voice.enforce.deafen` apply only with a valid MAC from the production signer. | Cross-language test |
| 9 | With the shipped broker config, only the control-plane identity can publish the enforcement subjects and `voice.commands.v1`. Only the media-plane identity can pull and ack `voice-enforcement-v1`. Only those two can write the owner key. No other identity can reach the media plane's pull inbox. The media-plane identity can do all that startup and renewal need. | Real-broker test |
| 10 | A body over the publish budget is refused at enqueue and never blocks the outbox head. A maximum body with maximum headers is stored. | Go tests, real broker |
| 11 | Both mixed-version pairs fail closed at the lease. | Lease and result tests |
| 12 | Each v1 conversion path writes v2 only for its allowed phase. `reseal` refuses every other case. The media plane never accepts v1. | Owner and command tests |
| 13 | The scan exploit tests behave as in section 13.4. | Exploit run |
| 14 | 0 new blocker or critical issues. | Quality gate |
| 15 | 0 new high-confidence security hotspots. | Quality gate |
| 16 | At least 80% coverage on new code. | Quality gate |
| 17 | `/verify-quality-gate` is green on the PR. | CI |
| 18 | Every document in section 14 is updated in the same PR. | Docs review |

## 13. Tests

Each negative case mutates one input so it can fail for one reason only. Each new branch has a test at the production caller.

### 13.1 Shared
- `testdata/proofs.json` beside `envelopes.json`: key, purpose, inputs, expected MAC and verdict for all nine purposes, plus owner v2 vectors. A Go test generates it and fails on drift; both suites load it.
- `envelopes.json` cases are unchanged.

### 13.2 Control plane (Go)
- **Golden vectors:** sign and verify per purpose; wrong purpose, wrong `kid`, nil key and a newline-bearing field all fail.
- **Compatibility publisher:** header present with exact key bytes; the MAC binds subject, `ts`, nonce and exact bytes. A caller-level test at each of the six sites proves the publish goes through it.
- **Architecture test** on the six exact strings.
- **Durable publisher:** the MAC binds stream, generation and sequence; 3584 bytes accepted and 3585 refused at enqueue.
- **Owner v2:** codec, MAC, `prevRevision`, the writer/phase matrix. Each reader refuses v1 except the three conversion sites.
- **`reseal`:** happy path, idempotent success, rotation path, and refusal of `held`, `initializing`, garbage, missing, receipt mismatch and wrong revision.
- **`recover`** writes v2.
- **Protocol 5** in every result type.
- **Reason code:** `media_proof_rejected` is accepted by the closed enum.

### 13.3 Media plane (TypeScript)
- **Seam:**
  - Rejected: missing header, duplicate header, case-variant header, bad grammar, unknown `kid`, bad MAC, stale `ts`, `ts` before boot, replayed nonce, cross-subject replay.
  - Invalid JSON with no proof never reaches the decoder (spy).
  - Cache saturation invalidates availability.
  - A plain object passed to each of the three handlers is refused.
- **Compatibility:**
  - Unsigned messages on all six subjects are dropped.
  - A signed snapshot applies; a signed bare lift is ignored.
  - A forged int64-max revision is dropped and a genuine revoke applies.
- **Durable:** every cell of the 7.3 table, plus:
  - an unsigned one-byte record is termed with no teardown;
  - a term followed by redelivery of the same sequence is not a gap;
  - the bind baseline for a fresh stream, a purged prefix, an outstanding delivery, and after a term.
- **Owner:**
  - Rejected: no MAC; v1; a replayed older valid record; a wrong writer for the phase.
  - A non-consecutive CAS result fails closed.
  - A garbage record fails closed with teardown preserved.
- **Lease:** a result advertising protocol 4 is refused.

### 13.4 Real broker (pinned resolver, never skipped)
- Headers survive pull delivery, DeliverAll replay and control-plane readback.
- A maximum body with maximum-length header values is stored.
- KV update yields the previous revision + 1; a write to another key breaks continuity.
- The ack floor after a term and after a retirement purge.
- Criterion 9 against the shipped config, including the exact steps of the consumer-theft exploit.

### 13.5 Scan exploit tests

Task T8 runs all nine and records the result per file. A file that still passes, other than as noted below, is a defect to fix before merge.

| Test file | Expected after the fix |
|---|---|
| `test_vuln_148_enforce_mute_deafen` | Fails: the handler refuses an unverified object, so neither the forged snapshot nor the bare action reaches the room. |
| `test_vuln_149_user_mute_deafen` | Fails: same seam. |
| `test_vuln_150_enforce_disconnect` | Fails: forged disconnect refused; repeat blocked. |
| `test_vuln_151_enforce_permissions` | Fails: no grant and no pinned revision. |
| `test_vuln_153_max_sequence_watermark` | Fails: forged record termed; the watermark follows the broker sequence. |
| `test_vuln_154_malformed_record_teardown` | Fails on the malformed-record case. The owner-rewrite case still tears down, by decision; under the broker config only the two service identities can reach it. |
| `test_vuln_155_durable_no_origin_proof` | Fails on the exploit. Its pin that the envelope has no signature field still holds under header carriage. |
| `test_vuln_156_owner_record_spoof` | Fails: the forged record is not read as `held`. The fail-closed reaction remains, by decision. |
| `test_vuln_212_durable_consumer_theft` | Cannot fail from this change. Its exploit block starts its own broker with no config and runs no media-plane code, and its source pins are lines this design leaves alone. The evidence is criterion 9, which repeats its steps against the shipped config, and criterion 6. |

## 14. Docs to update

- `docs/architecture.md`: command stream and owner store (proof header, owner v2, sequence binding), enforcement subject proof, pointer to the #174 permission matrix.
- `.claude/rules/domains/media-plane.md`:
  - verify-before-parse for bus input, with the owner record exception;
  - amended malformed-envelope sentence;
  - owner v2 and `prevRevision`;
  - the delivery table and the rule that a term is unconfirmed;
  - signed compatibility subjects and removal of bare lifts;
  - the protocol number (the `/readyz` text still says 3).
- `.claude/rules/domains/backend.md`: enforcement publishes go only through the signed publisher; the publish size budget.
- `docs/runbooks/selfhost-release.md`: both planes upgrade together, updater order, `reseal`, the `held` case, the bare `compose up` failure signature, key rotation and its drop precondition.
- `docs/api/README.md`: internal bus contract note.
- `CHANGELOG.md`: breaking internal protocol change.

## 15. Task breakdown (for the implementation plan)

| Task | Scope | Main files | Depends on | Parallel |
|---|---|---|---|---|
| T0 | Reconcile with the merged #174 module. Proof codec in Go and TypeScript (header grammar, MAC input builders, owner line list). `proofs.json` generator and both loaders. `media_proof_rejected` in both enums. Secrets baseline if the fixture trips it. | MAC module, `internal/securityevent/securityevent.go`, `src/lib/securityEvent.ts`, `testdata/proofs.json` | #174 merged | No |
| T1 | Signed compatibility publisher, six sites, architecture test | `internal/enforcementbus/`, `pkg/nats/client.go`, `internal/voice/{nats,permission_enforcer,temp_grants}.go`, `internal/dm/handlers.go` | T0 | Yes |
| T2 | Durable proof header, size budget, real-broker storage tests | `pkg/nats/voice_command_publisher.go`, `internal/voicecommand/contract.go`, enqueue path | T0 | Yes |
| T3 | Owner v2 codec, writers, readers, `reseal`, `recover`, `inspect` | `pkg/nats/voice_commands.go`, `cmd/server/voice_command_owner_recovery.go` | T0 | Yes |
| T4 | Verified seam, replay guard, handlers, bare-lift removal | `src/lib/nats.ts`, `src/lib/enforcementCommand.ts`, `src/lib/enforcePermissions.ts`, `src/lib/voiceCommandDispatcher.ts`, `src/index.ts` | T0 | Yes with T1 to T3 |
| T5 | Durable verdict, delivery table, tracker, bind baseline, broker-sequence watermark | `src/lib/nats.ts`, `src/lib/voiceCommandDispatcher.ts` | T4 (same files) | No |
| T6 | Owner store v2 read and CAS | `src/lib/nats.ts`, `src/lib/voiceCommandOwner.ts` | T5 (same file) | No |
| T7 | Protocol 4 to 5, fixture, every `/readyz` body consumer, updater order, cutover integration test | `voiceAuthProtocol.ts`, `voiceEnforcementLease.ts`, `internal/api/*`, `selfhost-update.sh` | T1 to T6 | No |
| T8 | Shipped-config broker test; exploit run | tests | T7, #174 | Yes with T9 |
| T9 | Docs, rules, changelog | section 14 | T1 to T6 | Yes with T8 |

## 16. Risks

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| #174's module differs from the assumed interface | Medium | Rework of T0, then T1 and T4 | T0 is first and serial; section 11 is fed to #174 before it merges |
| Upgraded media plane meets a v1 owner record | Certain on a bare `compose up` | Voice down until resealed | Updater order; fixed failure classification; runbook |
| Owner rejects the `reseal` command | Unknown | Upgrade needs operator generation reconciliation | Decision 1 in section 17 |
| Tracker rule disagrees with what the pinned broker reports | Medium | False gap and a self-inflicted outage, or a missed gap | Real-broker tests pin the ack floor after term and purge before T5 merges |
| A forged delivery makes the media plane term a genuine command | Low (needs inbox access) | Lost moderation | Never term a sequence that verified authentic; inbox isolation in #174; criterion 9 |
| Replay cache saturation | Low | Availability invalidated | Inserts only after the MAC; bound far above design load |
| A republish or tool drops the header | Low | Records termed | Fails safe; the rule file states the header is part of the record |
| A foreign write to another key in the owner bucket | Low | Fail-closed outage | Exact-subject permission from #174 |
| Merge-order conflict in `src/index.ts` with parallel work | Medium | Rebase | Different functions; rebase on whichever lands first |

## 17. Owner decisions (approved 2026-11-18)

1. **Approve the `reseal` operator command.** It is a new control-plane writer for the owner record. It is bounded like `recover`: exact revision, receipt-matched births, `released` only, media plane confirmed stopped. The fallback is operator generation reconciliation at upgrade, with voice down until it completes.
2. **Confirm that a record with a valid MAC that fails schema keeps the conservative teardown.** The alternative reading of the amended criterion terms it without teardown. That is a one-line change, but it silently drops a command that may be a revoke.
3. **`test_vuln_212`.** Accept criterion 9 as its evidence, or have the scan test take a broker config so it can fail.

## 18. Dependency references

- **nats.js** (pinned `^2.29.3`): `JsMsg` exposes `data`, `headers`, `info`, `term(reason?): void` and `ackAck(): Promise<boolean>`. A term gives no confirmation. The delivery-count field was renamed between v2 and v3; do not design against the name.
- **nats.go** (v1.55.0): headers are set on `nats.Msg.Header` and sent with `PublishMsg`. `KeyValue.Update(ctx, key, value, revision)` and `Create` take no headers. `Update` fails if the revision changed.
- **nats-server** (pinned 2.15.1): header bytes count toward a stream's `max_msg_size`, by the review's reading of the server source. The docs fetched for this pass did not restate it; T2's real-broker test settles it.
- **Not confirmed from docs:** what consumer info reports for the ack floor after a term and after a purge; header key case preservation in nats.go. T5 and T1 pin them.

