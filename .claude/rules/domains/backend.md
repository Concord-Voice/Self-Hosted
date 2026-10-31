---
description: Go backend conventions for the control-plane service
paths:
  - "services/control-plane/**/*.go"
---

> **Imported from Concord-Voice-Alpha (@771eb3c) at the repo split.** References to `client/desktop/**`, `docs/runbooks/deploys.md`, `docs/reference/as-built-ledger.md`, `docs/superpowers/**`, managed (hosted) deploy workflows and Alpha's org rulesets point at Alpha, Desktop or the private ops repos. Use them for context; they are not files or CI in this repo.

# Backend Rules

## Architecture
- Handler-struct pattern with dependency injection (`NewHandler(db, redis, log)`)
- All handlers follow: `func (h *Handler) EndpointName(c *gin.Context)`
- REST routes + WebSocket hub on port 8080 (canonical route count in CLAUDE.md Key Counts)

## Client compatibility

- Preserve APIs, WebSocket events and payload fields consumed by current Stable, the previous released minor line or the active next-minor/major Pilot line. Pilot-only server behavior requires an explicit capability or feature flag enabled only for a negotiated supporting client; add a flag with its first feature, not a new framework. The repo admin/release owner closes Pilot when its base ships Stable. See the release compatibility policy.
- Keep `clientversion.Parse`, `Version` and `Compare` stable-only; configured `CLIENT_MIN_VERSION` and capability floors remain strict stable versions. Parse supplied client declarations with `ParseClient`, accepting only stable or validated `X.Y.0-pilot.N` and comparing Pilot's stable base for admission. Do not use this normalization for update selection or entitlement. Preserve full Pilot declarations on the wire and verify HTTP plus both WebSocket authentication paths when changing floors.

## Error Handling
- Every error MUST be checked. `errcheck` (configured in `services/control-plane/.golangci.yml`, the repo's only golangci-lint config) flags **unassigned** error returns — but with default settings it honors explicit `_` blank-discards, so `_ = h.db.Exec(...)` and `_, _ = h.db.Exec(...)` patterns pass `errcheck` while still violating this rule. Such silent discards are caught by review (`@silent-failure-hunter` agent + human reviewers), not by the linter. Founding incident pair: #1142 (silently-broken FoF privacy) and #1154 (remaining discards in DM key-distribution).
- Wrap errors with context: `fmt.Errorf("operation: %w", err)`
- Sentinel errors as package-level variables (`ErrNotMember`)
- Return early on error — avoid deep nesting
- Check both operation error AND commit/rollback for transactions
- Voice outbox methods keep `BeginTx` and the immediate checked deferred
  `Rollback` together in the method that owns the transaction. Share validation
  or SQL statements instead of returning a live transaction from a helper, so
  cleanup ownership remains visible to Sonar. Join rollback errors with the
  method's return error and tolerate `sql.ErrTxDone` after completion.

## Database
- Always parameterized queries: `$1, $2, ...` — never `fmt.Sprintf` for SQL
- Use `tx.QueryRow`/`tx.Exec` inside transactions (not `db.QueryRow`)
- Always `defer rows.Close()` after `db.Query()`
- Check `rows.Err()` after iteration loops
- Verify `argIdx` increments correctly in dynamic UPDATE builders
- The control plane's boot-time migrations run through
  `internal/migrationrunner.Run(db, sourceURL)`, which refuses a dirty recorded
  version without touching it (#3587 PR-0). The account authority is the
  second caller: `account-authority migrate` reaches the same refusal through
  `RunSource(db, source.Driver)`, fed by `store.MigrateEmbedded`'s `iofs`
  source, so its migrations ship inside the binary (#3587 PR-B2). `Run` opens
  its `file://` source and delegates to `RunSource`. `cmd/migrate`
  deliberately builds its own migrate instance, because it is the repair tool
  and must reach a dirty database. The runner keeps its version table and
  advisory-lock key in the connection's current schema, so each caller needs
  its own database or schema. Do not add another boot-time runner with its own
  refusal logic.

## Server Voice lifecycle lease

**Channel voice-join rate limits preserve ten logical admissions per user per
minute.** Direct requests and requests with an invalid media proof use the
existing `RateLimitByUser` bucket of 10 per minute. HMAC-verified media-plane
authorization hops use an isolated 20-per-minute per-user bucket because one
logical admission performs both A1 and A2. Keep these buckets separate: sharing
the service-hop calls with the direct bucket would halve the intended logical
admission budget, while allowing more than 20 service-hop calls would permit
more than ten paired admissions.

**Channel A1/A2 authorization is fenced by a committed pending reservation.**
A1 takes the visibility, credential, server, channel, and
member authority locks, resolves current authority, then upserts and commits an
exact 30-second `voice_pending_admissions(channel_id, user_id, admission_id,
socket_id)` row in that same transaction before responding. A2 reacquires and
rechecks that authority, validates the exact reservation, allocates a positive
global authorization revision, and sends a
signed, read-only NATS prepare that validates the exact connected staged
candidate. Prepare mutates no `RoomManager`, socket, epoch, or lifecycle state;
the control plane commits before returning. The local media-plane join then
performs the sole exact promotion and membership/lifecycle mutation. RBAC
rechecks and timeout disconnects union unexpired reservations with
`voice_participants`; `voice.joined` removes the exact reservation in its
participant transaction, while the existing active-presence pre-plan rail
removes a bounded oldest batch of expired reservations independently of
participant cleanup. Later complete server/DM moderation snapshots carry a
positive global authorization revision; the media plane applies only a strictly
newer snapshot than the active or staged session state, with equal duplicates
inert. Promotion preserves a stored snapshot when it is at least as new as the
A2 revision, preserving newer successors. The retained reservation plus the `voice.joined` timeout backstop
closes landing races. Never treat A2 or prepare as promotion.

`voice_participants.lifecycle_event_at` is producer ordering evidence. The
PostgreSQL-assigned `lifecycle_observed_at` is lease evidence for reconciliation.
Eligibility starts at 90 seconds. The existing active-presence ticker runs
nominally every five seconds, and each pre-commit cleanup phase is bounded to 10
seconds; this is not a `<=95s` completion SLA. `PrepareCapture` runs before the
composite transaction, which locks visibility capture, the shared Server Voice
lifecycle lock, then the channel row used by key-epoch rotation. Locked
requalification protects both terminal ingress and expiry cleanup: a fresh
lifecycle mutation wins, a temporary grant inside its 60-second grace is left
alone, and a grant appearing after preflight causes a fail-closed retry.

The transaction conditionally deletes the participant and eligible temporary
override together, computes the successor epoch before purging the departing
user's wrapped channel key and pending key request, and records the durable
`key_revocations` row before one commit. Cleanup errors are logged with a fixed
failure class and retried on a later ticker pass; they must not block active-plan
processing or startup. Only a confirmed commit may trigger commit-derived effects (the successor-epoch announcement, roster-left/count notifications, and cleanup completion). A rollback or other proven no-commit outcome runs neither class. An ambiguous commit may still run fail-closed effects (cache invalidation, authorization revalidation, disconnect, and presence abandon), but must not announce an unconfirmed epoch or leave event.
After a confirmed participant deletion, the stale-participant reconciler gives
the completion and roster-left call chain one separate shared, detached
10-second post-commit context; presence execution is queued to its independently
bounded executor. An
ambiguous commit never announces the local epoch and runs only safe fail-closed
cache invalidation, subscription revalidation, disconnect, and presence-abandon
effects. A rollback leaves all durable state for retry.

An expired lease must not call `ClearServerVoice`, clear Redis activity, or
introduce a fleet disconnect. Committed deletion may deliver the normal Server
Voice leave and count updates. Voice join authorization, channel-key access,
participant reads, and WebSocket subscribe/prune authorization must bypass stale
permission-cache reads and resolve effective permissions from the database.
`key_revocations` is durable authorization state; a missed push is an availability
failure recovered by the pull/read path. Future at-least-once JetStream delivery
belongs to #2635;
do not add a second delivery rail.

Temporary grant creation, terminal cleanup, and permanent user-override supersession
share the visibility → per-user lifecycle lock prefix. An ambiguous grant commit is
compensated only while the returned short-lived PostgreSQL `xmin` fence identifies
the same `move_granted` tuple; `granted_at` remains grace-period evidence. Failed
immediate compensation force-disconnects and the existing five-second cleanup
callback retries past-grace orphans. A permanent supersession that removes effective `VIEW` atomically
records the key revocation and purges wrapped/pending key material; one that retains
`VIEW` clears temporary metadata without a gratuitous rotation. Any ambiguous
supersession still invalidates and rechecks permissions, including JOIN-only loss.
Generic override deletion and category sync must atomically return `409` when their
rewrite would remove a temporary move row; the dedicated revoke or permanent-upsert
supersession paths are the only allowed removers. Category parent/sync-flag and child
changes belong in the same visibility-locked transaction so conflict never leaves
configuration and enforcement divergent. Category upsert/delete preflight the exact
synced child set, re-read it under that lock, retry the entire capture once on a
mismatch, then return retryable `409` on a second mismatch; their protected-row scan,
child DML, cache invalidation, voice recheck, and ambiguous-commit recovery all use
that locked exact set rather than a mutable category subquery. Every writer that can
change that set — sync enable/disable, a channel `group_id` move, group deletion, and
bulk reordering — takes the same visibility lock. The protected predicate is exactly
`target_type='user' AND is_temporary AND temporary_reason='move_granted'`; temporary
role rows and other temporary reasons remain ordinary override data.

WebSocket channel subscribe, typing, and subscriber pruning use fresh database
authorization reads. DM subscribe and typing recheck current membership, and DM
message persistence locks the conversation and sender participant row before insert;
a removed participant cannot continue sending or persisting through a stale
subscription. Authorization/read failures fail closed; message writes retain the
credential-epoch fence.

Ordinary channel-override ambiguous commits treat the durable outcome as unknown:
invalidate the affected cache, recheck voice/subscriber authorization, and abandon
captured presence fail closed. Never assume rollback or announce an unconfirmed
authority or epoch.

The lease has TWO renewers, not one (#3187). The 000132/000133 trigger covers
rows whose upsert is APPLIED; `renewObservedLeaseForFutureStampedRows` covers the
one refused-upsert case the Redis lifecycle watermark cannot adjudicate — a row
stamped ahead of the database clock, which the watermark accepts as newest while
the upsert still refuses it. Every other refused upsert is a heartbeat the
watermark rejects as stale, and renewing those would be an end-run around the
staleness fence, not a fix. It runs in `handleServerHeartbeat` immediately after
the channel id is validated — that parse is the only step ahead of it, and an
unparseable channel has no rows to renew — and before every later fallible
stage, because each of those (nil activity bridge, pre-mutation read, stale
reconciliation exhausting the shared 10 s budget) can return before it and leave
a live participant unrenewed. See
[`migrations.md`](migrations.md) § Server Voice observed lease for the three
properties that keep it correct.

Its failure logs carry a cause class and nothing else. That is not an oversight
to be improved: `richPresenceLifecycleLogFindings` (an AST walk over `nats.go`,
in `nats_rich_presence_log_guard_test.go`) admits only `failure_class`,
`action`, `is_dm`, `context` and `count` on any `NATSSubscriber` log call, and
`count` only as an int literal or a `len()` call. A `channel_id`, a participant
id, or a row count read back from the database are all rejected by that guard.
Adding one is a privacy regression, not added observability.

**`clock_skew` — the ingress clamp's class (#3205).** `parseVoiceEventTime` bounds
every arriving producer stamp by the consuming replica's NATS receipt time, and
reports through `voiceSkewShed` when the pre-clamp difference exceeded
`maxVoiceLifecycleForwardSkew`. Four properties are load-bearing:

- **The clamp is zero-tolerance; only the REPORT is threshold-gated.** A sub-bound
  clamp is NTP jitter, and reporting it turns every voice event on a healthy fleet
  into a log line. The threshold can therefore cost visibility, never correctness.
- **The reference is NATS RECEIPT time, never a handler's own `time.Now()`.**
  `receivedAt` is stamped in `voiceLifecycleDispatcher.enqueue`, which runs on the
  NATS callback goroutine. Clamping at handler drain instead turns dispatcher
  lateness into a stamp *advance*, and advancing is the one direction that lets
  `DELETE … lifecycle_event_at <= $3` remove a live row — reintroducing the
  CWE-863 breach PR #3201 was closed for. This is not hypothetical at the
  topology we actually run: a SINGLE replica's own dispatcher backlog produces
  the divergence, which is what `TestVoiceLifecycleDispatcherStampsReceiptTimeAtEnqueue`
  pins. Do not justify it by fan-out — there is indeed no queue group on the
  voice wildcard subscribe, but `validateControlPlaneReplicaCount`
  (`pkg/config/config.go:646`) refuses an explicit `CONTROL_PLANE_REPLICA_COUNT != 1`,
  so "every replica applies every event" describes the topology #2757 would
  introduce, not the one in production.
- **Validate, then clamp.** Parse, then `presence.IsValidActivitySourceTime`, then
  clamp. The reverse order makes the year-2255 representability ceiling
  unreachable and silently vacuates the test that guards it.
- **The branch stays in `nats.go`; the emission lives in `ingress_gate.go`.** Same
  reason as `voiceDropShed`: the AST guard walks `nats.go` only and would reject
  an accumulated count. Moving the branch out of `nats.go` escapes the guard
  entirely rather than satisfying it.
- **The clamped stamp is TRUNCATED to microsecond precision, and that is
  correctness rather than tidiness.** `lifecycle_event_at` is `timestamptz` —
  integer microseconds — and PostgreSQL **rounds** a finer value on write, so a
  nanosecond-granular stamp reads back up to 500 ns **later** than the Go value
  that wrote it. Seven sites then compare that round-trip against the original
  *in Go* (`participant.lifecycleEventAt.After(eventAt)`) and conclude the row is
  newer than the event that stamped it. **The two comparison engines disagree:**
  the `DELETE … lifecycle_event_at <= $3` fence compares inside PostgreSQL, where
  the parameter rounds exactly as the column did, so it always matched — the
  defect is only ever in the Go-side checks. Do **not** "fix" it by truncating
  the DELETE's parameter: that makes the stored value strictly greater than the
  predicate and manufactures the permanent orphan it appears to prevent.
  **TRUNCATE, never round** — rounding advances a stamp, and advancing is the one
  direction that lets the fence remove a live row. Every producer stamp is
  already microsecond-aligned, minted from a monotonic microsecond counter
  formatted with a six-digit fraction (`nextVoiceLifecycleTimestamp`,
  `services/media-plane/src/lib/nats.ts`), so the invariant held **implicitly**
  until the clamp began substituting a nanosecond-granular `time.Now()` — the
  clamp armed this, it did not inherit it. The live failure was on the **DM**
  rail, where it is permanent: the first participant's terminal commits and
  `advancePrivateVoiceParticipantRows` stamps the *surviving* participant's row
  to `eventAt`; the second participant's terminal reads it back, rejects on the
  Go-side `After()`, and `clearDMRoomEmptyParticipant` reports `applied=false`
  with a **nil error**, so nothing logs. `dm_voice_participants` has neither
  `lifecycle_observed_at` nor a sweeper, so the row never leaves. It fires iff
  the nanosecond remainder rounds up (≥ 500 ns) — ~50 % per event on a
  nanosecond-clock host and **0 % on macOS**, whose `time.Now()` is
  microsecond-granular, which is why it was CI-only and looked like a flake.

`maxVoiceLifecycleForwardSkew` stays a `const` and gets no environment variable —
an operator cannot act on the number, and a misconfiguration would silence the
only signal that a media host's clock is wrong (same reasoning as
`presenceAudienceConcurrency`, #1654).

**The `voice.tier_violation` door and the entitlement violation recorder (#1308).**
The media plane reports each mic its audio tier gate refused on `voice.tier_violation`
(`{userId, reason, timestamp}`). `admitTierViolation` takes it on its OWN subscription in
the queue group `control-plane.tier-violation`, so ONE replica counts each report: core NATS
fans a plain subscription to every replica, and the counter window is fleet-shared, so
counting the fan-out would make the threshold ceil(5/N). The `voice.*` wildcard still
receives the subject on every replica; `gateVoiceLifecycle` DROPS that copy by subject -- no
parse, no shed, it is not refused, only delivered twice -- ahead of G-1, and every other
subject continues through G-1/G1/G0/G2 unchanged, so an unknown one still reaches
`unhandled_subject`. The door's order is the property: a 512-byte
bound before any parse (`oversized_payload`; its regression test, like G-1's, feeds a
MALFORMED oversized document), then `malformed_payload`, then `invalid_user` (the id must
be a CANONICAL uuid, not merely parseable, and not the nil uuid), then **`invalid_reason`** -- a new class: the
reason is not one of the media plane's two audio reasons (`attachment_over_tier` is refused
here: only the authenticated upload handlers count it, so a forged report cannot pass as
upload evidence) -- then the per-user budget and only after it the global one
(`ingress_budget`; per-user first, so one noisy user cannot spend the global tokens, and
global at all because distinct forged ids each arrive with a full per-user burst). The
GLOBAL budget is the forge bound; the per-user one is fairness for honest traffic, and a
forger can evict a chosen user's key by rotating ids (2 x 1024) or spend it directly (8
per 30 s), so it bounds nothing against a forger beyond what the global budget does.
Spending a user's budget cannot hide that user's flag: the forged reports are counted for the
same user, so the window escalates anyway; only the shed genuine reports' WARN lines are
lost. Sheds report through `reportShed` with their own
`tierViolationShedState`, aggregated, never per message. A shed report is a DROP; nothing
on this path disconnects anyone.
`entitlements.ViolationRecorder` is the one sink for both counted doors (this one and the
attachment tier-cap 413s on both upload paths in `internal/media` -- on the single-shot route
only the declared-size refusal, never the body-size overflow, whose limit includes the
multipart overhead allowance and so trips on extra form fields with a file inside the cap). A count means REFUSED,
never "proven hostile": an honest client with a stale set just after a downgrade, or one
refused while the tier lookup has degraded to free, is counted too, and the threshold is what
absorbs it. It counts on `entviol:<canonical uuid>`
with INCR + EXPIRE NX in one MULTI/EXEC -- a FIXED one-hour window, escalation on exactly
the fifth count (`==`, never `>=`), fleet-wide once per window -- and takes a `uuid.UUID`
so the key is canonical by type (Claim SPELLING above). Its escalation pushes the enforced
entitlement set through the existing `entitlements_changed` path and emits one
`security_control`/`denied`/`authorization_denied` event; it holds an `EntitlementPusher`,
which has no `DisconnectUser`, so the threshold structurally cannot disconnect -- forging
the trigger would otherwise kick a chosen user. `Record` never blocks or fails its caller:
it hands the count to one of 64 background slots and returns. The count and the escalation
each get their own 2-second deadline; the resync push itself is not bounded by it (the hub's
user broadcast blocks until the hub loop takes it), which the slot cap bounds instead. The
escalation is at most once: an INCR applied with its reply lost at the fifth count skips that
window's escalation, deliberately (a marker key's reply can be lost the same way). Three
recorder classes join the vocabulary: **`violation_counter_unavailable`** (Redis absent or
erroring; the raw error is never logged, it can echo the key and so the user id),
**`violation_record_saturated`** (every slot busy -- only reachable when Redis is slow -- so
the record is dropped rather than queued) and **`violation_resync_unresolved`** (the tier
could not be read, so the push is skipped: the escalation reads it through
`Cache.GetTierChecked`, which returns an error where `GetTier`/`ResolveTier` fail closed to
free -- right for enforcing one request, wrong as a set the client then holds; a database
error or a spent deadline both land here, and the WARN line and the event still fire). `Record` also logs `invalid_reason` for an out-of-set value from a caller -- a
programming error, unreachable from the wire. Threshold, window and both budgets are `const`,
never configuration.

## Logical login family authority (#2272)

`internal/loginfamily` owns SQL authority for migration 000194. Keep these
invariants when changing authentication, minting, session revocation or voice:

- Resolve the verified signed SID through immutable `login_family_sids` to its
  server-owned `login_families` user/credential-epoch binding and exact signed
  access expiry. A refresh row, device metadata or client-supplied family is not
  authority. Fresh credential/OAuth login creates a family; normal and grace
  refresh inherit it. Refresh-row revocation/cleanup does not terminalize it.
  Persist prepared access-token expiry before commit; signing must not extend it.
- Ordinary `middleware.AuthRequired`, control WebSocket/ticket establishment and
  renewal check live family authority. Their `AccessGrant` is a snapshot, not a
  final transaction capability. Productive/writer transactions acquire the
  rollout barrier at their early seam, then the complete sorted users set at its
  strongest required mode, then sorted families before downstream authority or
  mutation. Preserve existing outer prefixes and explicit theft/factor exceptions;
  do not add users or upgrade their locks after family acquisition. Channel A2
  commits initial SQL, prepares outside SQL, then repeats family and resource
  guards in fresh final SQL. Helpers never commit their caller's transaction.
- Revoke-one/logout terminalizes the selected SID's whole owned family.
  All/all-other capture a frozen frontier under the user fence; all-other excludes
  the complete validated current family. A delayed `user` operation is never a
  wildcard over later logins. Preserve explicit refresh → explicit SID → cookie
  selection and bearer fallback only with no selected target; invalid explicit
  input refuses. Commit terminal authority before blacklist work and blacklist
  only a bearer belonging to that family.
- Terminalization and durable targets commit together. Terminal families cannot
  revive; retries retain original owner/reason/frontier/age. Epoch-wide paths
  capture before presence/forced-clear children and reuse the actual epoch parent,
  without requiring its superseded epoch to remain current. A committed mutation
  is distinct from delivered closure: reconciliation requires exact signed
  receipts and a fresh owner-bound empty scan. Unknown commit is not success.
- Control-socket closure is synchronous, with reconciliation as the backstop.
  After a definite (or possibly-durable) commit, revoke-one/all, logout,
  recovery reset, erasure, age disablement, password change and E2EE key
  replacement call `loginfamily.CloseCommittedFamilies` with the
  result's owners, which fences each terminal family via
  `Hub.DisconnectFamilyContext` before the response (bounded, detached from
  client cancellation; it reads committed rows only). A failed close logs a
  fixed string and never fails the committed request; the sealed owner's
  reconciler retries it. Theft calls `Hub.DisconnectUser` on every branch,
  including deferred/Redis-fallback ones with no terminal family yet. Do not
  make the 5s reconciler the only closure path.
- Preserve Redis blacklist, theft/disablement and credential fences. Required
  authority unavailable is fail-closed (documented 503); Redis can stop
  `AuthRequired` before a handler and SQL absence cannot clear Redis-only state.
  After authority is established, failed old accounting alone need not prevent
  durable terminalization. No SQL auth bypass or full Redis-outage containment
  follows; #2271's separate step-up budget/availability work is not implemented here.

Source seams: `internal/loginfamily/{authority,targets,revocation}.go`,
`internal/middleware/auth.go`, `internal/websocket/handler.go` and
`internal/api/voice_auth_*.go`. Read the lock contract
and authority/schema contract
before changing transaction order. See [API semantics](../../../docs/api/README.md#public-authentication-flow),
the as-built record and
cutover/rollback procedure.

## Auth & Security

**Password-login fallback integrity.** `users.PatchSecurity` and
`users.DeleteSSOIdentity` serialize fallback decisions on the same users row.
Use explicit `sql.LevelReadCommitted`, acquire the row lock in a standalone
statement, then count child identities in a separate statement after acquisition.
A count in the locking statement can use a snapshot taken before a lock wait.
Keep the count and mutation in that transaction; check rollback and commit errors.
PATCH uses `FOR NO KEY UPDATE` for its non-key users write; unlink retains
`FOR UPDATE`. Evaluate the stored password preference through deployment policy.
After a successful final self-hosted identity deletion, clear a stored true
password-disable flag in the same transaction before commit. Partial unlink,
not-linked and definite precommit rollback outcomes preserve the stored preference.
A commit error has an uncertain durable outcome; return non-success without
automatic replay.

- JWT access tokens: 15min TTL, refresh tokens: 30d rolling HttpOnly cookies
- WebSocket: ticket-based auth (30s single-use)
- **Bearer-auth parity (#2352):** HTTP bearer auth and the JWT WebSocket fallback share live disabled-account validation; a disabled-account lookup error fails closed. HTTP keeps email verification as route-level policy via `RequireVerifiedEmail`, while the JWT WebSocket fallback rejects an explicit boolean `email_verified: false`. A missing or non-boolean legacy claim remains verified for backward compatibility. Ticket authentication is unchanged.
- **Control-socket in-band token renewal:** a control socket closes with 1008 at its authority deadline, the `exp` of the token that opened it. An `auth_renew` frame carrying a rotated access token can move that deadline (`websocket/token_renewal.go`). Invariants: (1) the deadline is never shortened and never revived: `extendDeadline` refuses an `exp` that is not strictly later, a deadline that has already passed, and a socket whose expiry timer teardown stopped. (2) One validator: the handshake's JWT fallback and renewal both call `validateAccessToken`, so they cannot drift; renewal additionally requires the socket's user and credential epoch. (3) Lineage, not equality: a rotated `sid` is accepted only when it is the socket's direct successor via `refresh_tokens.predecessor_id` on an unrevoked row; an empty `SessionID` fails closed. (4) On a rotated `sid`, `Hub.Run` rebinds `SessionID` over `rebindSession` first; every accepted token, same-sid included, then has its `sid` re-checked for revocation, closing the socket if revoked or unverifiable (the #2414 ordering). **The deadline moves LAST, after that check** — extending first let a stalled `Run` keep an extended deadline alive for an unverified session (red-team PoC, PR review). Both `Run` waits select on the renewal `ctx`, never only `Hub.done`: a rebind `Run` never took refuses with nothing changed, one it took but did not apply closes the socket. Sids are compared and rebound in canonical uuid form. Only `Run` writes `SessionID` after registration. (5) `readPump` handles the frame after the ingress budget and never forwards it to the hub; the ack goes through `enqueueOutbound`, never a raw `c.Send <-`, and carries only `ok`. (6) The token is never logged; refusals log a fixed reason class. Renewal is throttled to one attempt per socket per 10 s (the throttle logs once per window, not per frame) and bounded by a 3 s context, which holds for Redis only because the production client sets `ContextTimeoutEnabled`. Any failure keeps the old deadline.
- **Token-class binding (#2899):** bearer authentication is bound to the ACCESS-token class, not merely to a valid signature. `router.go` hands one `cfg.JWTSecret` to both `mfa.NewHandler` and `middleware.AuthRequired`, and `mfa.GenerateChallengeToken` signs the login / suspicious-refresh / MFA-upgrade / recovery families with that same secret — each carrying `user_id` — so a signature check alone let an `mfa_challenge_token`, handed to a caller holding ONLY the password by `POST /api/v1/auth/login`, authenticate as the victim for its 5-minute TTL: a complete MFA bypass. A recovery token did the same for 25 hours, and this is not a hypothetical transport — the client already puts recovery tokens in the `Authorization: Bearer` slot for the unauthenticated recovery endpoints (`validateRecoveryTokenParam`). `middleware.IsAccessTokenClass(issuer, hasPurpose)` is the rule and it exists in exactly one place: it requires `iss == middleware.AccessTokenIssuer` and rejects a `purpose` claim by KEY PRESENCE, not by string value (`claims["purpose"].(string)` yields `""` for any non-string, so a numeric / null / object purpose bypassed the first version of this guard). The three CONTROL-PLANE readers delegate to it: `AuthRequired`, the WS-JWT fallback (`authenticateViaJWT`), and the typed `auth.ValidateAccessToken` behind `Logout` (a challenge token unmarshals happily into `Claims`, so `blacklistAccessToken` accepted one and let its holder revoke the victim's sessions). `auth.GenerateAccessToken` mints from that same constant, so minter and readers cannot drift. **Neither surrounding fence catches this class**, which is why it survived several auth hardening passes: `blacklist:<jti>` keys are never written for challenge JTIs (those live under `mfa_challenge:<jti>:remember_me` / `mfa_webauthn_session:<jti>`), and #2418 stamps the user's CURRENT epoch into a login challenge, so the #2201 fence passes by construction. **There is a FOURTH reader, and it does NOT enforce the same boundary** — do not read the "three readers" above as "everywhere". `services/media-plane/src/middleware/auth.ts` verifies the same `JWT_SECRET` and sets `socket.data.userId` from the token, but checks `issuer: 'concordvoice-control-plane'` ONLY, with no `purpose` check, and hard-codes the issuer string in another language where no Go constant can reach it. That is safe today precisely because the issuer partition holds — but it is exactly the "future control-plane-issuer token carrying a purpose" case the `purpose` clause exists to catch on the Go side, so if such a token is ever minted the media-plane is the unprotected surface. Its long-standing issuer requirement is also what proves the claim was already load-bearing in production, which is why the control-plane check carries **no rollout window**. Two consequences for tests: fixtures must mint through the REAL `mfa` generators (a hand-rolled lookalike JWT drifts from the production token shape and goes on passing while the hole reopens), and any WS fixture that omits `iss` now stops at this boundary instead of reaching the epoch-fence / disabled-user / Redis-outage guard it means to assert on — correct the fixture, never relax the assertion. `jwt.WithIssuer` (golang-jwt v5) would enforce the issuer half at parse time and is worth revisiting if a fourth bearer surface appears; it is not used today because splitting one security fence across a parser option plus a separate `purpose` predicate is harder to audit than one named helper called identically at every reader. **The kernel shares the RULE, not the EXTRACTION, and the two readers measurably disagree on claim-key CASE** (`@red-team`, PR #2900): the untyped path does an exact `claims["purpose"]` map lookup, while the typed path decodes into a struct where `encoding/json` falls back to case-INSENSITIVE field matching when no exact tag match exists. So `{"Purpose":"login"}` is invisible to the map path and fills `Claims.Purpose`, and `{"Iss":…}` is invisible to it but fills `RegisteredClaims.Issuer`. Extracting `IsAccessTokenClass` did NOT close this — the divergence lives in the extraction. It is NOT exploitable and is accepted rather than fixed: no mint emits a capitalised key, so presenting one requires signing it, and anyone holding `JWT_SECRET` can mint an ordinary access token instead. Closing it means the typed reader must stop trusting struct decoding for those two claims (decode to `map[string]json.RawMessage`, derive both from exact keys) — real complexity for an unreachable case. Pinned by `TestReaderExtractionDivergesOnClaimKeyCase` so it is rediscovered deliberately. The reciprocal half — what stops an ACCESS token being redeemed as a CHALLENGE — is `mfa.ChallengeTokenIssuer`, named in the same change for the same reason: it was a bare literal at both the mint and the read. Distinct per-family signing keys remain the stronger end state and are deliberately NOT in scope here — that is a keyring/rotation project, not a fix for this defect.
- **Claim SPELLING is attacker-chosen, exactly as claim ENCODING is (#3362).** One claim over from #2899, same file, same class. `AuthRequired` canonicalizes `user_id` through `uuid.Parse` + `.String()` before any gate runs, so `c.Get("user_id")` is a canonical UUID **by construction** and every downstream consumer inherits that for free. The defect is structural: the request identity crosses two stores with different equality rules. Every SQL predicate compares it as a PostgreSQL `uuid`, which resolves `{braced}`, UPPERCASE and dash-less spellings to ONE value; `middleware.UserDisabledKey` and `credepoch.Key` instead CONCATENATE the raw string into a Redis key. Measured before the fix: all three variants were admitted **200** where the canonical spelling was refused — the disabled denylist has **no DB fallback**, so a disabled account was simply never seen, and a `blocked:` epoch marker was missed while `authoritativeEpoch` fell through to `dbEpoch`, whose SQL *does* canonicalize, matched, and admitted. **When one identifier keys two stores, canonicalize at the single admission choke point, never at each gate** — the two key builders deliberately do NOT re-canonicalize, because a boundary expressed twice is one edit from being enforced differently by surface. **But "the choke point covers everything" is a claim about REACHABILITY, and #3362's first draft got it wrong by enumerating the wrong thing.** It grepped the key BUILDERS' call sites; `credepoch.Begin` builds `Key(userID)` *internally*, so every caller of the FENCE is a key-builder call site that grep never shows — and two of them are the UNAUTHENTICATED recovery reset routes, which `AuthRequired` structurally cannot reach. **Enumerate callers of the functions that build the key, not call sites of the key builder**, and treat any unauthenticated rail as owing the invariant itself. **`uuid.Parse` is a PARSER, not a canonicality check** — and it is looser than it looks: its 38-byte arm slices `s[1:]` and never validates the delimiters, so `"!"+canonical+"?"` and `" "+canonical+" "` both parse. Its accepted set is also *not* a superset of PostgreSQL's — PG takes a hyphen after any group of four digits (`1111-1111-2222-…`), a fourth live variant of this same bypass, which `uuid.Parse` refuses outright. So a site that parses and then uses the RAW string has validated nothing; that is precisely what `users/handlers.go` `ChangePassword` did while MINTING a continuation token pair, re-minting a non-canonical spelling as another rather than normalizing it away. The WebSocket door (`websocket/handler.go` `parseUserIDFromClaims`, gates keyed on `userID.String()`) was already correct; the HTTP middleware was the outlier, which is the second time these two doors have diverged on claim handling and the first time the WS side was the stronger one.
- Use `crypto/subtle.ConstantTimeCompare` for token/hash comparisons
- **A refusal designed to be indistinguishable must be equalised in WORK, not just in wording (#1240).** The friend-request privacy gate is the reference: `internal/friends/privacy.go` puts the shared-server `EXISTS` in the `CASE` **condition** rather than an arm, and repeats the enum comparison in **both** arms, so all three `allow_friend_requests_from` values perform identical database work for a fixed (requester, target) pair. Rewriting it with `AND`/`OR` reopens the channel — Postgres does not define operand evaluation order and may short-circuit. In `SendRequest` the eligibility read **replaces** the bare existence probe instead of being added after the friendship check, so a privacy-blocked rejection costs exactly what a user-blocked one costs; both then return through the *same* `friendshipConflictResponse("blocked")` call, making the 403 byte-identical by construction rather than by convention. Any new control of this shape needs all three properties — equal work, equal response, equal telemetry (see `.claude/rules/observability.md` (in Concord-Voice-Alpha) principle 7) — and each is individually easy to undo with a well-meaning simplification.
- Password changes MUST revoke all refresh tokens atomically
- **A stored password remains a valid step-up factor even when `password_login_disabled` is set (#2765 — decided, not inherited).** No step-up path consults that flag and none should: the flag narrows the **unauthenticated** door where the deployment offers SSO — credential stuffing and brute force against `/login`, which is why `internal/auth/handlers.go` deliberately skips the lockout counter for it — while step-up runs behind an already-authenticated session and proves knowledge, not identity. Different threat model, correctly different answer. Two independent reasons make this structural rather than a default to be revisited casually. (1) **On an SSO account the password IS the E2EE root secret.** `SSOPassphraseSetup.tsx` derives an Argon2id wrap-key from that passphrase and wraps the RSA-4096 private key with it, and `oauth.CompleteRegistration` strength-validates it on the same request that carries the wrapped key material — a factor that still unwraps every message the user can read cannot coherently be inert for step-up. (2) **Honouring the flag creates a self-lockout.** `users.PatchSecurity` explicitly re-enables password login using the passphrase and nothing else — there is no MFA branch; a successful final self-hosted `users.DeleteSSOIdentity` also clears the flag atomically with deletion — so an SSO-only account could never re-enable password login. `internal/oauth/handler.go` states the intended model: the hash remains a valid step-up credential and becomes usable for native login when the user enables it or self-hosted deployment policy requires it — dormant at the SaaS login door, never revoked. A future change that wants a genuinely revoked password must revoke the **credential** (rotate `credential_epoch`, clear the factor), not overload this flag, and must give `PatchSecurity` a second factor first or it ships the lockout. Applies uniformly to every password-verification helper in the control-plane, on or off the `internal/stepup` seam.
- **Per-user credential-epoch fence (#2201):** `users.credential_epoch` (nullable; NULL = never rotated) is the durable source of truth for bulk access-token revocation; `internal/credepoch.Fence` owns every semantic (Redis `cred_epoch:<id>` states `active:<epoch>` / `none` / `blocked:<opID>`, DB read-through on miss, DB fallback on Redis transport error, fail closed only when both stores fail). Every mint path embeds `cred_epoch` + `sid` claims (primary and password-verified continuation mints also embed `auth_time`; refresh and grace rotation never do, so a renewed session cannot read the wrapped identity key); `AuthRequired`, WS-ticket redemption, and the WS-JWT fallback enforce identical semantics (media-plane parity rides the bearer-carrying voice authorize hops). The four destructive flows (ChangePassword / ReplaceMyKeys / RecoveryResetPassword / RecoveryResetAccount) rotate the epoch inside their transaction under the user-row lock, publish `blocked:` before and `active:` after a DEFINITE commit, DEL after a DEFINITE rollback, and retain `blocked:` after an AMBIGUOUS commit (its 5m TTL + read-through IS the reconciliation — do not add a bespoke reconciler). A failed `active:` publish DELs the cache key instead of leaving a pre-rotation value: a stale `active:<oldEpoch>` would otherwise keep admitting the just-revoked token (access reads are cache-first; `GuardTx` only fences writes) until the 15m cache TTL, so the DEL forces the next request to read through to the DB's new epoch. An unrecognized cache value is repaired with `Set` (not `SetNX`, which no-ops while the bad value occupies the key). Key-material-coupled writes (encrypted blobs, presence-override ciphertext, wrapped-key creation/distribution, HTTP **and WebSocket** message send/edit, tier-2 attachments) recheck via `credepoch.GuardTx` (users-row `FOR SHARE`) inside their write transaction — never remove a guard because "middleware already checked": the guard is what closes the admitted-before/commits-after race. The authenticated credential epoch is captured at WS upgrade onto `Client.CredEpoch` and stamped onto every inbound frame so `persistMessage`/`persistDMMessage` fence the write inside the persist transaction; a WS ticket binds to the authenticated `sid` claim (never a client-supplied `X-Session-ID`) so `DisconnectSession` can evict the ticket socket. Refresh-token rotation (`rotateAndRespond`) runs in one transaction that locks the user row `FOR NO KEY UPDATE` and revokes the source token only `WHERE revoked_at IS NULL RETURNING`, so a concurrent reset that already revoked it refuses to mint a post-reset session (the same lock the reset holds serializes the two). Authenticated flows return a continuation token pair, minted by `IssueTokenPair` under a fresh `FOR NO KEY UPDATE` lock and bound (via `MatchEpoch`) to the epoch that flow committed — a concurrent second destructive flow that advanced the epoch in the post-commit window skips the pair (`ErrContinuationEpochAdvanced`) rather than hand back a session it meant to terminate; recovery flows terminate every session. `Op.Commit` retries its owner-scoped CAS once on a Redis transport error so a fast reconnect still clears a stale pre-rotation cache value; a durable Redis outage spanning the whole flow (both `Begin` and `Commit` unreachable) cannot be repaired synchronously and leaves a bounded `≤cacheTTL` (15m) fail-open residual — the DB stays authoritative and every sensitive write is still `GuardTx`-fenced. The WS persist guard distinguishes a proven `ErrEpochMismatch`/`ErrBlocked` (auth-shaped rejection) from a transient users-row read failure (logged, retryable save error).
- **Issuance binding (#2418 / #2414):** every session mint is additionally bound to the epoch it was *authorized* under, not merely the epoch current at mint time. `CompleteLogin` takes an `expectedEpoch` and runs in ONE transaction holding the users row `FOR NO KEY UPDATE` — the same lock every destructive reset holds — refusing via `MatchEpoch` when the durable epoch advanced past it; the same-device revoke and the refresh INSERT moved inside that transaction, and the `user_keys` read stays BEFORE the mint so a keyless user cannot leave a committed session behind a 500 (locked by `TestLoginMissingE2EEKeysDoesNotCreateSession`). `Login` reads the epoch alongside the password verification and threads it to both the direct path and the MFA challenge; `lookupUserForLogin` returns it as a **separate value, never a `models.User` field**, because that struct has a `PublicUser()` client-facing serializer and a revocation tag must not sit one careless edit from the wire. The MFA login challenge carries its issuance epoch in `ChallengeClaims.CredentialEpoch` (`cred_epoch`, omitempty), stamped at all three issuance sites (direct login, suspicious-machine-ID refresh, SSO — each failing closed on a read error rather than substituting `""`) and consumed at `completeVerifyPurpose`; a **claimless** challenge is REJECTED for a rotated user (fail closed — exposure is one 5-minute challenge TTL; a deliberate deviation from #2418's stated AC). Recovery and MFA-upgrade challenges are deliberately unstamped because `completeVerifyPurpose` routes only `PurposeLogin` to a mint. **The SSO mint is bound the same way (#2450):** `IssueAccessAndRefresh` (`oauth_adapter.go`) reads `email_verified`/`disabled`/`credential_epoch` and INSERTs the refresh row in ONE transaction under `FOR NO KEY UPDATE`. There is no `expectedEpoch` to compare against — SSO carries no separately-authorized epoch — so **the lock, not a comparison, is the mechanism**: it forces a total order with the reset, so either the reset commits first (a genuinely post-reset sign-in, acceptable) or the mint holds the row and the reset sweeps what it inserted. #2418 shipped a comment on `generateTokenPair` naming SSO as a flow that must not mint unfenced, but left `IssueAccessAndRefresh` doing exactly that: an unfenced epoch read and a separate autocommit INSERT with a Redis round trip between them, so a reset's revoke-all sweep ran before the row existed and `rotateAndRespond` could rotate the survivor into a valid post-reset session (CWE-367 → CWE-613). Exposure was every existing-user SSO mint — the trust-SSO branch, the non-MFA fallthrough, and password-verified account linking (`CompleteLink`); `CompleteRegistration` mints for a brand-new user with no pre-reset credential (the same argument that makes `generateTokenPair` safe), and the SSO **MFA** branch already redeemed through `CompleteLogin`. **One scope limit remains — do not restate this as "the lock alone forces a total order":** standalone bulk refresh-token revocation uses `auth.RevokeAllRefreshTokens`, which takes the users-row `FOR NO KEY UPDATE` lock before sweeping sessions; destructive flows already holding that lock retain their own atomic sweeps (#2457, #2460); the provider-assertion `Callback` path carries no separately-authorized epoch, while `CompleteLink` compares its password-verified epoch under this lock (#2458). The challenge epoch is stamped via the distinct `mfa.CredEpoch` type, and the signing key via `mfa.JWTSecret`, so a `jwtSecret`/`credEpoch` transposition is a compile error rather than a silent signing-key disclosure into a client-visible claim; the typing stops at `GenerateLoginChallenge`, whose `credEpoch string` parameter is separated from `userID` only by an intervening `bool`. Relatedly, `entCache.GetTier` is resolved BEFORE `BeginTx` on both mint paths — it is a Redis GET with a DB read-through, and calling it under the users-row lock both acquires a second pool connection while holding one and extends the hold on a lock every destructive reset needs. The epoch claim must NOT be validated in a `Claims.Validate()` method — golang-jwt v5 auto-invokes that during `ParseWithClaims`, where the durable epoch is unavailable. There is **no** `refresh_tokens.credential_epoch` column: both refresh-row read paths already filter `revoked_at IS NULL` and resets bulk-revoke, so a stored per-row epoch is redundant under correct locking and would fail *open* (NULL admits) for the very mistake it would exist to catch — revisit only if a future path reads refresh rows for minting *without* that filter. **The grace-period refresh path (`handleGracePeriodRefresh`) is LIVE as of #2428** — see [`migrations.md`](migrations.md) § `refresh_tokens.predecessor_id` for its contract. At the time #2418 shipped it was genuinely dead code: its successor lookup carried an uncast `$2` placeholder whose type PostgreSQL could not infer, and the error was swallowed as a benign 401, so a grace mint could not execute and #2418 deferred it rather than activate a weak `(user_id, ±2s)` time-window heuristic inside a security change. #2428 (PR #2429) did the deferred work — the lookup is now an exact `predecessor_id = $1 AND revoked_at IS NULL AND expires_at > NOW()` lineage query, the swallow is replaced by a `sql.ErrNoRows`→401 / real-error→500 split, and the successor lookup, conditional revocation, and replacement refresh-row insertion run inside the users `FOR NO KEY UPDATE` transaction. **The #2418 grace concern is now addressed by exact lineage and that shared lock, which serializes the grace re-mint against every destructive reset.** On the WebSocket side the post-registration `fence.Check` in `handler.go` closes the register-vs-disconnect-sweep ordering race; **`hub.register` must stay unbuffered**, or the send no longer blocks until `Run` consumes it and that ordering argument collapses.
- **CompleteLink epoch binding (#2458):** `oauth.Handler.CompleteLink` must pass the epoch returned by `VerifyPassword` to `IssueAccessAndRefreshBound`; that transaction locks the user row, runs `MatchEpoch`, and inserts both the SSO identity and refresh session atomically. A mismatch rolls back both writes. Provider-assertion and new-user flows remain unbound because neither has an earlier password proof.
- **The session that enrolls MFA is exempt from its own pre-MFA challenge.** `checkPreMFASessionLock` makes every refresh session older than `users.mfa_enabled_at` prove the new factor once. The enrolling session proved it minutes earlier, so a FIRST activation writes the same single-use, 30 s upgrade-bypass key a verified challenge writes (`auth.MFAUpgradeBypassKey`), for the caller's `sid`, and the client refreshes at once to spend it. Three conditions, each mutation-proven in `internal/mfa/enrollment_upgrade_test.go`: (1) **first activation only**, read by `mfaNeverEnabled` BEFORE the enable writes the stamp — a session older than an EXISTING stamp keeps its challenge even when it adds a factor of its own, otherwise a password-only session could escape the lock by enrolling a second key; (2) **the proving session is the activating session** — TOTP proves at `verify-setup` and activates at `confirm-setup`, which takes no code, so `verify-setup` records the `sid` and `confirm-setup` grants only on a match, while WebAuthn and Email/SMS each prove and activate in one request (a WebAuthn ceremony begun in one session and finished in another grants the finisher, which is the session holding the new key); (3) **a token without a `sid` grants nothing**. Every failure — an unreadable flag, a missing setup record, a Redis error, an access token rotated between the two TOTP steps — lands on the challenge, which is the pre-change behaviour. A TOTP re-setup deletes the setup record, so a session that verified the replaced secret cannot match. The flags write on the TOTP and WebAuthn enable paths is **fatal**, as it already was for Email/SMS: login and this lock read those flags, so a 200 over an unset `mfa_enabled_at` would tell the user MFA is on while neither is enforced. The factor write and the flags write commit in ONE transaction (`withMFAFactorWriteTx`, B1), so a failed flags write rolls the factor back with it: a WebAuthn credential needs no compensating delete, and a TOTP confirm stays unconfirmed, so the retry is an ordinary first confirm and grants as one. The inline-factor read runs inside that transaction and is fatal. An unreadable email/SMS state is NOT fatal: the degraded write carries the row's own email/SMS entries forward, so it can never count a factor that is on as absent and never takes the disable branch that would clear `mfa_enabled_at`. A row confirmed before B1 whose flags write was lost is healed by `confirm-setup`, which answers 200 and grants unless the flags already list TOTP; only a completed activation is a 409. A TOTP re-setup that cannot delete the setup record stops with a 500, and `verify-setup` stops when its lockout cannot be read, because checking codes without it would lift the attempt limit.
- **An MFA read that decides a challenge fails closed, and the flags write takes a row lock.** `Verify` reads everything a dependency failure could stop on — the challenge's reuse marker, the user's lockout, the remember-me state, the recovery-only methods — before it judges a code, because judging spends a backup code or a WebAuthn ceremony; any of those failing answers 500 with a dependency event and spends nothing. The WebAuthn keys are read before the ceremony is taken for the same reason. It then **counts the attempt before judging** and refuses anything past the limit with 429. The count is `INCR` followed by `EXPIRE NX`, so the expiry is set only when the count has none: the window runs from the first attempt and nothing extends it, since a refused attempt renewing it would let one request every few minutes hold the account's MFA locked for as long as the requests kept coming. NX also gives a count whose `EXPIRE` failed an expiry on the next attempt. Counting first is what makes the limit hold under concurrency: each request gets its own number, so however many guesses arrive at once only the first five have a code checked. Counting after the check let every request that read a count below the limit try a code. A count or window that cannot be written answers 500 without judging, so a right code and a wrong one get the same answer. A failed lockout write after the fifth wrong code also answers 500, and the count still refuses every further attempt until its window ends. TOTP setup counts a wrong code after judging it (`recordCodeFailure`), since the secret being confirmed is the caller's own, and any count, window or lockout write it cannot make answers 500. A WebAuthn assertion whose sign-count write fails does not verify, at sign-in or inline. A factor the user marked recovery-only is refused at sign-in exactly as a wrong code is, so the answer never confirms the code. Backup codes belong to TOTP, so a recovery-only TOTP refuses its backup codes too, and a restricted TOTP or backup code is refused without being judged: judging would spend the backup code or the TOTP step. The list governs sign-in only; step-up never reads it. The restriction lapses when it would leave no sign-in method (`effectiveRecoveryOnly`, applied by `GetLoginMethods` and `Verify` alike): an account with MFA and nothing to offer at sign-in would otherwise sign in on the password alone. SMS does not count as a sign-in method, because `Verify` cannot answer it, and `SetRecoveryOnly` refuses a restriction that would take away the last sign-in method, leaving only SMS (`signInFactor` feeds both checks). An account with no sign-in method to begin with (SMS only, development and test) can still clear or set its list, since no choice strands it (`strandsSignIn`). Every challenge `Verify` and `SendEmailMFACode` accept is a sign-in challenge. `IsEnabled` counts only the factors a step-up can verify inline (TOTP, WebAuthn) and answers `true` on a read error, because every caller uses it to decide whether to demand a factor. The email login code claims its one-per-challenge slot with `SETNX` before sending, so an unreachable limit or two concurrent requests cannot send twice, and the claim is released if the code cannot be stored or sent. **A code goes out by email only when the email service actually delivers** (`!IsDevMode()`), or when the environment is exactly `development` or `test`: `email.Service` never delivers when SMTP is unset, so a service that exists is not one that delivers. Without SMTP it logs codes only when `ENVIRONMENT` is exactly `development` or `test` (an unset `ENVIRONMENT` means `production`, so a deploy that forgets it meets the production guards instead of logging codes; local dev names it in `.env` and `concord-dev.sh`; `CONCORD_ENV=test`, whose `test_only:` codes and relaxed auth limits are test backdoors, is refused at load under any `ENVIRONMENT` but `development` or `test`), and everywhere else every send (sign-up, account recovery, ownership-transfer reversal token) fails with `ErrDeliveryNotConfigured` without logging the secret: staging runs without SMTP, and a logged recovery code resets the account. SMS, which has no provider and reaches the user only as `dev_codes` in the setup response, can be enrolled only under that same allowlist. Status reads (`GetStatus`, `EmailSmsStatus`, `GetBackupEmail`) answer 500 rather than a default that reports a factor off, and `GetStatus` re-derives the flags whenever they disagree with any of the four factors. The flags write (`withMFAFactorWriteTx`) locks the users row `FOR NO KEY UPDATE` before any read, so two enrollments take turns and the later one reads every factor change committed before it; READ COMMITTED is pinned, since under REPEATABLE READ the reads would see the snapshot taken when the lock was requested. Its email/SMS read happens before the transaction opens, so a stalled Redis never holds that lock. Undoing a partial write — the WebAuthn withdraw, the email/SMS withdraw, releasing the email send slot — runs on a context detached from the request, since a client that disconnects cancels exactly the requests whose cleanup matters. Because it outlives the request, an undo removes only what its own request wrote (`releaseOwnedScript`, compare-and-delete): the email/SMS enable keys hold a per-request value, and a setup code is compared before it is deleted. Otherwise a timed-out attempt's undo could switch off a method its retry had just reported on, or delete the code a newer setup had just sent. An email/SMS activation that fails part-way turns back off only the methods it turned on (`SET … GET` reports which were already on), then re-syncs the flags so they match whatever the failure left. Disable paths delete the factor and write the flags in one transaction and record the factor-disabled event only after it commits, so a failed disable changes nothing and reports nothing. `WebAuthnDeleteCredential` leaves out `remaining_credential_ids` when it cannot list them: the client hands that list to the authenticator as every key still accepted, so an empty one would hide every key the user still has. Malformed ids on the MFA routes that take one answer 404, like any other miss. **A login challenge offers `webauthn` only together with `webauthn_options`** (`auth.webAuthnLoginOffer`, shared by password login, the refresh challenges and SSO): a key offered without options strands the user — the client picks it and nothing happens — so when the ceremony cannot begin, `webauthn` is dropped from the offered methods, and a challenge nothing can answer is a retryable 503 at password login and an error on the other two paths. A login challenge that is not sent has its remember-me key (`auth.MFAChallengeRememberMeKey`) discarded rather than left to expire. The challenge-required security event is recorded only after the offer succeeds, on every path, so the telemetry never reports a challenge that was not sent.
- Account erasure (`POST /api/v1/privacy/erase-account`) inherits the atomic-token invariant structurally: `refresh_tokens.user_id` has `ON DELETE CASCADE`, so deletion inside the erasure transaction removes refresh tokens atomically (stronger than the `ChangePassword` soft-revoke — no residue). The handler must also blacklist the current JWT access token using its `jti` for the token's remaining TTL and set `user_disabled:<id>` for `auth.AccessTokenTTL` so other already-issued access tokens for the erased user are rejected before returning success. See `docs/superpowers/specs/2026-04-20-669-erase-account-wiring-design.md` §7.
- **RBAC role-mutation escalation guard (#2350):** every actor-selected path that confers a role's permission bitfield — `CreateRole`, `UpdateRole`, and `AssignRole` (the latter two via the shared `authorizeRoleMutationTx`, which superseded `checkPermissionEscalation` in #2721) — MUST apply the permission-subset check (`Permission(rolePerms)&^actorPerms != 0` → 403). `UpdateRole` and `AssignRole` additionally apply a position-hierarchy check; **`CreateRole` applies its own** (#2359) — `resolveNewRolePosition`, called inside the write transaction #2721 shaped for it, gives a non-owner a slot strictly below their own highest role and returns `errHierarchyDenied` when none exists (their highest role is at position 0 or 1). It **shifts** rather than clamps: a clamp to `actorMaxPosition-1` always returns that one value, since the actor's roles are a subset of the server's and `MAX(position)+1` is never the smaller operand, so every role an actor created collided on one position; and hunting for a free slot below the actor does not help, because `applyRolePositions` repacks densely as `len-i-1` on every reorder. That makes `CreateRole` a **multi-row `roles.position` writer**, safe only because it and `applyRolePositions` both take `LockServerVisibilityCapture` first (#2861) and are therefore totally ordered. Before #2721 this shift was an escalation primitive — it let an actor raise their own ceiling between a guard's two unsynchronized reads; `roleGuardQuery` closed that by making the ceiling structurally unable to be newer than the target. Where both apply, **neither substitutes for the other**: position bounds *which roles* an actor may touch, the subset check bounds *which bits* they may confer — but **only the server-level `roles.permissions` bitfield**. A role also carries `channel_permission_overrides` rows (`target_type='role'`) whose ALLOW bits the resolver ORs in, so a role with `permissions = 0` — a subset of every actor — can still confer permissions inside a channel. The guard's subset check does not inspect them; that gap is proven and tracked in #2723 (RS4, #3454, does count them, but only for an unenrolled owner of an enforcing server), and a lower-positioned role can legitimately carry a bit the actor does not hold — exactly the CWE-269 escalation #2350 closed. Do not remove the subset check as "redundant with the hierarchy check." **The actor's permissions are resolved cache-bypassing**, and since #2721 that means `ResolveServerPermissionsTx` on the write transaction rather than `ResolveEffectivePermissionsFresh` on the pool. Every one of these paths writes DURABLE authority that outlives the 5-minute cache entry authorizing it, and a pre-mutation compute can repopulate that entry after a demotion invalidated it — so a cached read lets a just-demoted actor mint a permanent grant. Do NOT revert to `GetEffectivePermissions` for symmetry with the ephemeral `HasPermission` checks; their staleness expires, this one does not. **An earlier revision of this entry scoped that sentence to "both callers" while the bullet above named three handlers**, and `CreateRole` was in fact resolving cache-first off `GetEffectivePermissions` — the asymmetry was unremarked, not deliberate, and #2721 closed it. `ResolveServerPermissionsTx` mirrors `ResolveEffectivePermissionsUncached`, never `...Fresh`: `Fresh` calls `cache.Set`, and publishing from a transaction that then rolls back seeds Redis with a state that never committed. **Owner bypass is per-handler, not uniform:** `UpdateRole` and `AssignRole` bypass on `servers.owner_id` (identity, never `PermAdministrator`); `CreateRole` has **no** owner bypass, so an owner is bounded there by their own effective permissions. Because `OwnerPermissions` (`internal/rbac/types.go`) excludes bit 62, an owner cannot CREATE a role carrying `PermAdministrator`. **That does not keep bit 62 out of a server, and an earlier revision of this entry said it did:** `UpdateRole`'s owner bypass skips the subset check entirely and `buildRoleUpdateClauses` writes `*req.Permissions` unmasked, so an owner can set bit 62 on an existing role — or any other value, which is how a negative `roles.permissions` is API-writable (#2869). Owners MAY delegate `PermAdministrator` — decided 2026-09-23 — so `UpdateRole`'s path is intended and `CreateRole`'s refusal is the defect, tracked as #3407 (the owner gains exactly bit 62 on create, not a bypass). Do not cite `CreateRole`'s missing bypass as containment. Note the guard tests the LITERAL bitfield: `computeRolePermissions` is a plain `BIT_OR` and never expands bit 62 into a full mask, so a holder of bit 62 would pass the subset test. `PermAdministrator` is also **not** exempted in the role handlers while `UpsertChannelOverride`/`UpsertCategoryOverride` do exempt it; that asymmetry over-blocks and therefore fails closed — do NOT "harmonize" by adding a bypass to the role handlers, which would weaken the guard to buy symmetry. `UnassignRole` is position-only, and the rationale once given for that — "removal confers no bits" — is **true only at server scope**, where effective permissions are a `BIT_OR` over the member's roles. At CHANNEL scope the resolver SUBTRACTS role-attached DENY overrides (`applyChannelOverrides`: `finalPerms &^= roleDeny`), so removing a deny-bearing role is a monotonic WIDENING: any member holding `PermManageRolesAssign` can lift a moderation restriction placed on themselves. Proven with a passing PoC — see #2724. Do not repeat the "subtraction cannot escalate" reasoning; it does not survive the channel-scope model. Denial auditing is deliberately uniform-or-nothing across all guards — instrumenting one alone would make the audit trail misleading by implying the others have no denials (#2679).
- **The role-mutation guards read every operand in ONE statement inside the write transaction (#2721).** `authorizeRoleMutationTx` (`internal/rbac/role_guard.go`) replaced the pre-transaction guard helpers for **four** handlers — `UpdateRole`, `DeleteRole`, `AssignRole`, `UnassignRole`. **`CreateRole` does NOT route through that guard** and never has: it has no pre-existing target row to re-read or lock, so it runs its own in-transaction, cache-free escalation check (`ResolveServerPermissionsTx` plus a direct subset comparison) inside a transaction that takes the same advisory lock. Five handlers changed; four share the guard. Do not restate this as "all five handlers use `authorizeRoleMutationTx`" — that claim was in an earlier revision of this bullet and is false; it also contradicts this bullet's own later sentence that `CreateRole` adds no row lock, which is true precisely BECAUSE it never calls the guard that takes one. The defect was not staleness but **incoherence**: the target role's `position`, the actor's ceiling `COALESCE(MAX(r.position),0)`, and `servers.owner_id` were three or four separate autocommit `QueryRow` calls, so under READ COMMITTED each took its own snapshot and `rolePosition >= actorMaxPosition` compared operands from different points in time. Raise the ceiling between the two reads and `5 >= 6` is false — the guard opens. PoC-proven 4/4 during #2850's adversarial pass: `DeleteRole` destroyed a peer `PermAdministrator` role, `UpdateRole` zeroed a peer Administrator's bitfield. **The ceiling half cannot be fixed with a lock and never could:** it is an aggregate, and PostgreSQL rejects a locking clause at a query level carrying aggregation (`FOR SHARE is not allowed with aggregate functions`), so the issue's original "apply the same `FOR SHARE` treatment to the position read" has no row-lock form. Snapshot coherence is the mechanism there, and it is sufficient. The permissions half **does** take a lock — `FOR SHARE OF r` on the target `roles` row, the same shape and the same reasoning as `recipientKeyFresh` in `e2ee.md` § "Distribution side must not re-create purged recipient rows". It is table-qualified deliberately: `LockRows` sits above the index scan while the aggregating `SubPlan`s sit below it, so the one-row scope is a plan-shape property rather than a syntax accident. **`servers.owner_id` is read UNLOCKED inside that same statement, and that is load-bearing** — it is what keeps a `roles ↔ servers` lock edge from existing in either direction. Do NOT "harden" it to `FOR SHARE`. **EPQ ceiling-monotonicity is the security property, and it is not cheaply re-derivable.** If the target row was updated-and-committed between the statement's snapshot and the lock acquisition, `LockRows` runs `EvalPlanQual`: it re-reads the **locked relation's** tuple at the newest version while the subqueries stay pinned to the original snapshot. So the target position may be newer than the ceiling, but **the ceiling can never be newer than the target** — exactly the direction the escalation exploited. Do not "fix" the asymmetry by folding the ceiling into the locked relation. **Deadlock argument, by lock-set identity rather than a global invariant:** `UpdateRole`/`DeleteRole` add zero edges, because their write already locks that same row more strongly (`UPDATE` → `FOR NO KEY UPDATE`, `DELETE` → `FOR UPDATE`) and taking a weaker lock earlier does not grow the lock set; `AssignRole` raises the FK's `FOR KEY SHARE` to `FOR SHARE` on the same row, and `UnassignRole`'s child `DELETE` takes no parent lock at all, so for both the guard is a genuinely new acquisition — but each waits on at most ONE row, and the only writer they can wait on (`applyRolePositions`) takes no advisory lock and therefore can never wait on them, so the wait is one-directional and no cycle is constructible; `CreateRole` adds no row lock and merely joins the advisory total order it was previously outside. **Explicitly rejected variant, recorded so a future edit is visibly a rule violation:** dropping the aggregate and locking the actor's N role rows with a non-aggregate `SELECT … FOR SHARE`. That locks N rows in unspecified order while `applyRolePositions` locks in client-supplied order, making `T1 holds A wants B / T2 holds B wants A` directly constructible. A guard takes **at most one** row lock, always the target `roles` row, always after the advisory lock, never on `servers` or `member_roles`. The guard sits at the top of the `write` closure — **after** `capturePresenceVisibility`, never hoisted above it, because capture must remain the exact pre-write audience (#2445) — and a guard denial returns through that closure as an error, which `withAuthorityCapture` rolls back **without** calling `presenceAbandon`; that is correct, since `Abandon` disconnects the captured audience and must not fire when nothing was written, so sentinels map to 403 at the call site and must never be logged as the class-2 500. Each closure runs `SET LOCAL lock_timeout = '3s'` as its first statement (after the advisory lock, so the "advisory lock is the transaction's first statement" invariant is untouched): the guard is the first place in this family that can block on a row lock **while holding the per-server advisory lock**, and the pool sets no `lock_timeout`/`statement_timeout`/`idle_in_transaction_session_timeout`, so a stuck `applyRolePositions` would otherwise pin every role mutation on the server. `55P03` → 500, generic body, `failure_class: "guard_lock_timeout"` — identical on the wire, distinguishable in logs, mirroring `ErrPresenceCaptureLimited`. **The residual this bullet used to name is now CLOSED, and the deadlock argument that rested on it has changed premise — both by #2861 (#2851), which landed first.** The residual was `applyRolePositions` **lowering the actor's own ceiling** between the guard's snapshot and the commit: defender-side, one transaction, not attacker-steerable. `applyRolePositions` now takes `LockServerVisibilityCapture` as its own first statement, so it joins the per-server advisory total order and can no longer commit inside that window at all. **Do not reinstate the "one member of this family whose write takes no advisory lock" phrasing** — every member takes it now. The consequence for the deadlock argument is that an earlier revision of this bullet reasoned "`applyRolePositions` takes no advisory lock and therefore can never wait on us, so the wait is one-directional and no cycle is constructible." That premise is FALSE as of #2861; the conclusion survives and strengthens, because two transactions that both acquire the same per-server advisory key first are totally ordered and cannot interleave. The guard's own AT-MOST-ONE-row-lock rule stands on its own merits regardless. **Still never write "the TOCTOU is closed" unqualified** — the accepted residuals below (the three-statement in-transaction resolve, and the transaction-scoped `lock_timeout`) are unaffected by #2861 and remain open by choice. **#2851 is a hard PREREQUISITE for #2850, not a follow-up:** #2850 makes `CreateRole` shift every role at or above the actor, turning it into a multi-row `roles.position` writer, at which point the one-directional-wait argument above does not survive the move from one row to N and a production `40P01` class becomes constructible. Merge order is #2721 → #2839 → #2851 → #2850. **A cheap, NON-AUTHORITATIVE hierarchy denial runs on the pool BEFORE the transaction opens, and it is load-bearing for reasons that are not about performance.** Moving the guard inside the write closure put three things ahead of every denial that used to sit behind it, and an adversarial pass on the fix confirmed all three: `preparePresenceCapture` (O(#senders), and above `presenceCaptureMaxChannels` it fails closed with `ErrPresenceCaptureLimited`, so the SAME unauthorized request returned 403 below the bound and 500 above it — **disclosing aggregate voice occupancy, the exact signal `authority_tx.go` says it exists to contain**); `pg_advisory_xact_lock(server)`, which an actor authorized for nothing on the target role could force the server to take; and the guard's own row-lock wait, held WHILE holding that advisory lock, so one concurrent `roles` writer became a ~3 s server-wide stall of every role mutation including the owner's. Note the `lock_timeout` does NOT bound the advisory acquisition — it is the first statement *inside* the closure, hence strictly after it. The pre-check uses the same statement **minus the locking clause**, derived from one shared constant so the two cannot drift, and it must never carry one: on the pooled connection no `SET LOCAL lock_timeout` applies, so a `FOR SHARE` there could block indefinitely. **A pre-check error returns nil and falls through to the transaction** — it can only ever save work, never grant it; do NOT "harden" it into a denial-on-error, which would let a transient pool blip 403 a legitimate mutation. `CreateRole` deliberately has no pre-check: it takes no presence capture, so the occupancy oracle cannot arise there, and reaching it already requires `PermManageRoles`. **The guard also ENFORCES `is_managed` / `is_default`, it does not merely read them.** The first cut scanned both flags into `roleGuardResult` and every call site discarded the result — leaving the *position* re-derived under `FOR SHARE` while the *flag* verdict silently stayed authoritative in the pre-check, one new `is_managed` writer away from re-opening this very straddle. `rejectRoleFlags` applies each handler's own policy (`AssignRole` rejects neither, matching its prior behaviour) and returns the typed `roleFlagDenied`, which carries the message because the 403 bodies differ per handler and are part of the wire contract. **The pre-check denies on BOTH halves — hierarchy AND escalation — and the escalation half costs nothing.** An earlier revision denied only on hierarchy, which left F3 open for the escalation path: a `PermManageRoles` holder whose request was an escalation denial still entered `withAuthorityCapture` and still got 403-below-bound / 500-above-bound. Gitar caught it. The fix is one extra scalar subquery in the guard statement — `COALESCE(BIT_OR(...), 0)` over the actor's roles — which for a NON-OWNER is byte-identical to what `resolveServerPermissions` returns, so the pre-check reaches an escalation verdict with **zero additional round-trips**. It is NOT valid for an owner (`OwnerPermissions` there), so every consumer short-circuits on `IsOwner` first; `CreateRole` has no pre-check at all, which is what preserves its deliberate absence of an owner bypass. **Three residuals are accepted rather than closed, and each is accepted for a stated reason — do not silently "fix" one without re-reading why.** (1) The in-transaction actor resolve is still three statements in a READ COMMITTED transaction, so `servers.owner_id` is read TWICE and the straddle is narrowed rather than eliminated; every divergence direction lands fail-closed or on committed truth, and a genuine single-snapshot guarantee needs `REPEATABLE READ` with real serialization-failure handling, not a comment. (2) `SET LOCAL lock_timeout` is TRANSACTION-scoped, so the post-guard write can also wait — `AssignRole`'s FK on `server_members`, `UnassignRole`'s `member_roles` row, `CreateRole`'s FK `FOR KEY SHARE` on `servers` (which waits on, and is never waited on by, a server-delete `FOR UPDATE`). That is strictly better than the previous unbounded wait, and `isGuardLockTimeout` is now checked in `mapGuardError` as well as inside the guard so those 55P03s keep the `guard_lock_timeout` failure class instead of falling to the generic 500. (3) The escalation 403 now sequences AFTER request-shape 4xx in `UpdateRole`/`CreateRole` (an escalating request with a malformed colour returns 400 where it returned 403) — less information disclosed, not more, and accepted as such. `roles.is_managed` / `is_default` remain nullable and are scanned into plain `bool`, which fails CLOSED on a NULL (`Scan` errors → 500) rather than silently reading `false`. Design: `2026-08-20-2721-rbac-role-mutation-toctou-design.md`. **RS4 (#3454) narrows the owner bypass in this guard for `AssignRole` on an enforcing server: see § "Dangerous-action gates (#3454)".** The three role gates (`CreateRole`, `UpdateRole`, `DeleteRole`) run after this guard's verdicts and before any write; `CreateRole` holds `servers FOR SHARE` from its gate, so a server delete queues behind it, and its position shift moved after the gate.
- **Every `member_roles` reader that turns a `role_id` into authority is server-qualified, and migration 000144 makes the alternative unrepresentable (#2869).** Migration 000035 declared `member_roles.role_id UUID NOT NULL REFERENCES roles(id)` beside a separate composite FK to `server_members(server_id, user_id)`, and tied `roles.server_id` to nothing. A row naming THIS server and THAT server's role was therefore schema-legal, and every hierarchy and permission aggregate joined on `role_id` alone — so such a row imported a foreign role's `position` and `permission` bits into the victim server. **The asymmetry was the tell:** `reorderGuardQuery` and `resolveNewRolePosition` already carried `AND r.server_id = ...` while `roleGuardSelect`'s own `actor_max_position` and `actor_base_permissions` did not, which marks the omission as an oversight rather than a decision — the guard that blocks cross-server assignment was evaluating the actor through the hole it guards. Proven twice by independent `@red-team` passes: during #2850 (`bits=4611686018912075266` against a server-qualified `484687362`, **bit 62 set**) and again during #3350's review by a different route. Migration 000144 adds `roles UNIQUE (id, server_id)` and replaces the existence-only FK with `member_roles FOREIGN KEY (role_id, server_id) REFERENCES roles (id, server_id) ON DELETE CASCADE` — the shape 000082 used for `channel_groups`. **The rule covers TWO shapes of reader, not one.** A join to `roles` carries `AND r.server_id = mr.server_id`; and a reader that never joins `roles` at all — the `user_roles` CTEs and `IN (SELECT role_id …)` lists that match `channel_permission_overrides.target_id` — must join it anyway to get that predicate, because `mr.server_id` names the MEMBERSHIP's server, not the role's. A third `@red-team` pass during #2869's own review found exactly that: every join qualified, and a foreign-role channel override still honoured through the `user_roles` CTE, leaking a hidden channel through both `GetVisibleChannelIDs` and `GetAllVisibleChannelIDs`. Do not state a count of qualified sites — it was written as twelve, then thirteen, and was wrong both times; `cross_server_row_test.go` plants a cross-server row past the FK and asserts every resolver entry point ignores it, which is the check a count was standing in for. **Do NOT delete a predicate as redundant-given-the-FK:** it is what keeps an aggregate correct for a row reaching the table by a route the FK does not cover — a replica running ahead of the migration, a restore from a pre-000144 dump, or direct database access — and it costs one comparison on a row the join already fetched. **Two write-side and read-side consequences ride the same PR.** The override upserts (`UpsertChannelOverride` / `UpsertCategoryOverride`) insert through `INSERT … SELECT … WHERE target_type <> 'role' OR EXISTS (role in this server)`, so a `target_type='role'` override naming another server's role is refused with a 400 (`Override target role does not belong to this server`) and writes nothing — `channel_permission_overrides.target_id` carries no FK at all, so this predicate is the only thing standing between a foreign role id and a stored override. Both category→channel copy cascades (`syncCategoryOverridesToChannels`, `copyCategoryOverridesToChannel`) share one statement (`copyCategoryOverridesSQL`) carrying the same filter, because a foreign-role category row written BEFORE the guard existed would otherwise be re-copied into every synced channel on the next sync. Such rows are inert — every reader matches role targets against the server's own roles — and are deliberately left in place rather than swept by a migration. And the member list (`ListMembers`) now returns 500 when the member-roles read fails, instead of returning every member with `roles: []`: an empty list reads as "this member holds no roles" to a client that builds assign/unassign from it, so a silent degrade there is a wrong answer, not a partial one. **The channel-scoped resolver entry points refuse a channel of another server.** `HasPermission`, `GetEffectivePermissions`, `ResolveEffectivePermissionsFresh`/`…Uncached`, `ResolveEffectivePermissionsForChannelsFresh` and `ResolveChannelPermissionsTx` take `serverID` and `channelID` separately — base permissions from the first, overrides from the second — and the owner/administrator short-circuit used to return the full base set for a channel of ANY server (proven: `HasPermission(serverA, ownerOfA, channelOfB, …)` returned true). `requireChannelsInServer` now runs before that short-circuit and fails with `ErrChannelNotInServer`, which wraps `ErrNotMember`, so every existing caller denies it without a change. Every production caller derives the server from the channel row today; the guard exists so a future caller that takes the two from separate request fields cannot turn that into a cross-server grant. The migration's own guard and cleanup policy is in [`migrations.md`](migrations.md) § 'Cross-server role assignment and permission-bitfield sign'.
- **The permission bitfields are pinned non-negative — all five columns (#2869).** `roles.permissions` and `allow`/`deny` on both `channel_permission_overrides` and `category_permission_overrides` are `BIGINT`. Bit 62 is `PermAdministrator`; bit 63, the sign bit, is not a permission at all, and Go (`int64`), SQL (`BIGINT`) and the client (`BigInt`, whose bitwise operators sign-extend) read it differently. Be precise about the hazard, because the loose version — "a negative value reads as administrator" — is false: bit 63 alone grants nothing. The dangerous value is -1 and its kin, which are negative because they set EVERY bit: they carry bit 62, and one in the actor's `BIT_OR` makes `Permission(req) &^ actorPerms` zero for every request, opening the escalation subset check. The CHECKs refuse the sign bit and with it -1, the natural "all permissions" sentinel; they do **not** keep bit 62 out, since `1 << 62` is positive. **Negative values are API-writable, by three routes:** the server owner via `UpdateRole` (owner bypass, unmasked write — see the #2350 entry above), a `PermAdministrator` holder via `UpsertChannelOverride`/`UpsertCategoryOverride` (both exempt administrators from the allow subset check), and any `PermManageChannels` holder via `deny`, which is never subset-checked. All four write handlers — `CreateRole`, `UpdateRole`, and both override upserts — therefore reject a negative bitfield with a 400 before the CHECK can turn it into a 500. Constrain all five columns or none: hardening one copy would leave precisely the asymmetry the bullet above exists to remove. **A permission bitfield crosses JSON as a decimal string, both directions, on every RBAC endpoint (#3406):** `,string` on every `int64` bitfield field, `strconv.FormatInt` at every `gin.H` exit, and `GetAuditLog` re-emits the bitfield keys registered in `auditBitfieldKeys` as exact decimal strings (stored JSONB is never rewritten). A JSON number is refused with 400. **A leading `+` is refused by `internal/rbac/bitfield_wire.go`, not by `encoding/json`:** Go 1.27 runs the v1 API on the v2 engine, whose `,string` integer parse is `strconv.ParseInt`, so `"+64"` decodes to 64 where Go 1.26 refused it, and no GODEBUG or `go` directive restores the old answer. Each request struct carrying a bitfield has an `UnmarshalJSON` that first decodes a shadow whose bitfield members are `bitfieldSignProbe`. `encoding/json` calls the probe once for EVERY occurrence of the member, after its own member matching (case folding, escapes), so a signed occurrence is refused even when a later duplicate is the value the real decode keeps. Do not reduce the shadow to a `json.RawMessage`: that sees only the last occurrence, and `"+64"` followed by `null` then stores 64, because `null` leaves an `int64` unchanged. `UpdateRoleRequest` also clears `Permissions` when the last occurrence is any spelling of the string `"null"`: Go 1.27 clears a `,string` pointer only for the literal token, so an escaped spelling left a present 0 that `UpdateRole` would write, where Go 1.26 left the role unchanged. A new bitfield request field gets the same wrapper, and a new `*int64` bitfield gets the same reset. A new bitfield on the wire takes this form, and a new bitfield-bearing audit action registers its keys. **`,string` still decodes `"-1"`**, so the non-negative rule above keeps its `binding:"gte=0"`; the tag changes the wire form and nothing else. New audit rows store every `Permission`-typed metadata value as a decimal string at write time (`auditMetadataForStorage`), whatever its key, so an audit writer records a bitfield as `Permission(x)` and the registry only has to cover rows written before #3406. The rule keys on the type, not on `int64`: `AuditWriter` also serves non-permission events, and a member timeout's `duration_seconds` stays a JSON number. **An owner cannot write bit 62 through an override:** `OwnerPermissions` excludes it and the override subset check exempts only a holder of `PermAdministrator`, so a test needing a value above 2^53 on an override acts as a member holding Administrator through a role (`setupAdministratorAndTarget`), not as the owner.
- **RBAC/SBAC authority writes carry a pre-mutation presence capture (#2445).** Every permission write that can change channel visibility runs through `rbac.withAuthorityCapture` (`internal/rbac/authority_tx.go`): `PrepareCapture` → `BeginTx` → `pg_advisory_xact_lock` → `CaptureVisibility` → the authority write → `Commit`, then a post-commit `Execute`. **Capture strictly precedes the write** because the pre-mutation authorized audience is destroyed by the write itself — post-hoc recomputation cannot reconstruct it, so a capture failure must BLOCK the write (rollback, 500, nothing changed, nothing disclosed). **That rule holds only where the handler still owns its write.** `syncCategoryOverridesToChannels` runs AFTER its caller committed the parent `category_permission_overrides` row, and the child `channel_permission_overrides` rows it writes ARE the enforcement (the resolver never reads the category table) — so blocking there leaves the parent applied and every child stale behind an HTTP 200, meaning a tightened category override silently no-ops while a loosened one still applies. It therefore drops the plan and keeps the writes, degrading presence to the ≤90 s TTL (the pre-#2445 baseline for that path). `deleteCategoryOverrideWithCapture` and `copyCategoryOverridesToChannel` own both their writes inside the transaction, so they block normally. **The capture never vetoes a permission write.** Relatedly, `presenceCaptureMaxChannels` bounds VOICE fan-out and `Executor.activeSenders` filters `c.type = 'voice'`, so cascades must pass the voice subset for capture while still rewriting every synced child — feeding it the unfiltered list let 65 synced text channels, with no presence work at all, trip the bound. The capture is **two-phase on purpose**: `pg_advisory_xact_lock` is held until COMMIT, so anything called between the lock and the write runs under the lock regardless of which connection it uses. `PrepareCapture` runs pre-transaction and resolves each sender's candidate half (settings, tier, friends/FoF, membership, base presence); `CaptureVisibility` runs in-transaction after the lock and does one visibility filter per affected channel. That split is what keeps lock-hold time O(#affected channels) instead of O(#senders) — a channel admits up to 1000 participants, so the collapsed form would hold a lock serializing every RBAC mutation on the server for ~1000 sequential round trips. It is sound only because the two halves depend on disjoint state: no hooked RBAC write mutates the settings/tier/friends/membership inputs phase 1 reads. Collapsing the phases re-opens the defect. The advisory key is derived ONLY by `rbac.ServerVisibilityCaptureAdvisoryKey` (`sha256("rbac_visibility_capture\x00" + serverID)[:8]`, big-endian into signed int64); the domain string is load-bearing — it must stay disjoint from the `internal/voice` lifecycle and settings-cleanup key spaces so no new lock edge exists against those transaction families, and the lock stays the transaction's first statement and its only advisory key. **No direction branch may exist:** assigning a role can NARROW visibility via role-targeted `deny` bits and unassigning can WIDEN it, so every hooked mutation captures and refreshes unconditionally and the clear set is always `captured − fresh`; "widening path" and "narrowing path" must not become code concepts. Two writes are deliberately NOT hooked and must stay that way — `UPDATE channels SET sync_permissions` (visibility-inert: `filterVisibleUserIDsForChannel` in `internal/rbac/channel_viewers.go` reads neither that column nor `category_permission_overrides`) and `invalidateSyncedChannelCaches` (performs no write, and iterates unsynced channels too). **BOTH category-override cascades are hooked** — the upsert cascade (`syncCategoryOverridesToChannels`) and the delete cascade (`deleteCategoryOverrideWithCapture`, reached from `DeleteCategoryOverride`); the delete half was missed in the original implementation, and its bare `_, _ = h.db.Exec` also let a failed cascade return 200. **Wiring hazard:** `newTempGrantManager` is called at three sites (`internal/voice/nats.go`, `internal/voice/handlers.go`, `internal/voice/temp_grant_sweep.go`), each owning an independent manager, so all three owners take `SetPresenceRecheck` — a fourth construction site that is not wired silently keeps pre-#2445 behavior for its trigger. The RBAC handler's own wiring IS boot-guarded: `requirePresenceRecheckWired` (`internal/api/router.go`) fatal-exits when the activity service exists and `rbacHandler.HasPresenceRecheck()` is false. It must interrogate the HANDLER — a nil check on the executor value is a tautology, since `voicepresence.NewExecutor` never returns nil — and its scope stops at that handler, which is exactly why the three temp-grant owners above remain a silent hazard. Delivery is post-commit and **not durable** by design; durability is deferred to the shared enforcement outbox (#2635), which wraps this exact structure — do NOT add a second outbox, table, stream, dispatcher, or consumer family ahead of it. `internal/rbac` declares the `PresenceRecheck` interface at the consumer (the `VoiceEnforcer` precedent) and gains zero presence imports; that keeps the implementation swappable for #2635 and keeps the `internal/presence` test binary compiling. Phase 1 shares ONE `presence.ServerMemberLoader` per `PrepareCapture` call (#2681): `server_members` is read once and replayed to every sender, with the sender exclusion applied in memory by `membersExcluding` rather than by the old in-SQL `user_id <> $2` predicate — that predicate was what made the read per-sender and unshareable. The loader lives entirely inside phase 1 and is discarded when `PrepareCapture` returns; it must NEVER move onto the `Executor` or be reused across captures, which would compute an audience from PRE-mutation membership — the reconstruction error this bullet exists to prevent. It carries its own `DBTX` and its own server binding and consults NEITHER of the caller's, so `captureServerMembers` asserts the binding and fails closed with `ErrCaptureLoaderServerMismatch` rather than silently resolving a foreign server's members; an in-transaction caller must build the loader over that same `*sql.Tx`, or the member read escapes the transaction's snapshot. `internal/graphpresence` deliberately stays on the nil-loader path because its loop varies `serverID` rather than `senderID`. Carry into #2447: sharing the read is sound only while no hooked write mutates `server_members`, and that family hooks `RemoveMember` / `execBanTx`, which delete from it. Design: `2026-07-30-2445-server-voice-presence-rbac-reconciliation-design.md`.
- **Graph-destroying writes capture presence INSIDE the transaction (#2446).** Friendship acceptance, friend-code claim, friend removal, blocking, and the `dm_friends_of_friends` toggle run `presencehook.WithGatedTx` (which acquires the sender gates and THEN opens the transaction) → `presencehook.Capture` → the write → `presencehook.Complete` (which COMMITS), through `presencecapture.GraphPresenceCapture` implemented by `internal/graphpresence`. No handler opens its own `db.BeginTx` any more; there are **NINE** production `presencehook.WithGatedTx` call sites — four in `internal/friends`, three in `internal/members`, one in `internal/invites`, one in `internal/users`. **This read FIVE (friends + users only) until #2854 corrected it**, having gone stale when #2447 added the members and invites sites; issue #2854 inherited the stale count and reasoned from it. Verify with `grep -rn "presencehook.WithGatedTx(" internal/ --include="*.go" | grep -v _test.go` rather than trusting this sentence. The consequence was not cosmetic: every one of those sites carried a gate-before-no-op-check ordering defect, so a fix scoped to the named handlers would have left five unfixed. **#2854 stage C closed it at the four sites that could reach it** — `AddMember` and `execBanTx` in `internal/members`, `RemoveFriend` and `BlockUser` in `internal/friends` — with a pre-transaction PROBE. `ClaimFriendCode` and the `internal/users` privacy PATCH already nil the capture for the same reason and needed no change; the rest gate unconditionally by design, because their principal is the actor themself and holding your own stripe during your own request is not stranger contention. **The probe contract, and it is narrow on purpose:** a single boolean read on the POOLED handle via `QueryRowContext`, carrying NO locking clause (on the pool no `SET LOCAL lock_timeout` applies, so `FOR SHARE` there could block indefinitely), placed AFTER every existing authorization check so it cannot become an existence oracle, and NON-AUTHORITATIVE — the in-transaction read stays and stays the authority, and **its value must never be substituted for that read**. A probe ERROR fails OPEN, falling through to the gated path, because an unreadable probe proves nothing. A probe/in-transaction DISAGREEMENT on a path that still writes fails CLOSED with `presencehook.ErrProbeStale`. `presencehook.AcceptedEdgeExists` is the one definition of the accepted-edge predicate, shared as a FUNCTION rather than a constant so the argument binding is shared too; `graphpresence.acceptedEdgeExists` delegates to it and that backwards-reading import direction is deliberate and acyclic. Three test guards hold this in place, and the AST one is the only mechanism that can catch its defect at all: substituting the probe's verdict for the in-transaction read leaves every behavioural test green, because the response and the durable state are byte-identical and only the held locks differ. **The bridge covers Server Voice only** — there is no `CategoryPrivateCall` path in it. **PR 1 was entirely read-only:** it wired NO durable topology rail, wrote NO markers, and took NO row lock. **PR 2 (#2446) wires the rail and it is LIVE** — `SetTopologyRail` runs at construction in `internal/api/router.go` and the boot guards refuse to start without it, so the capture now writes durable Custom Status markers and takes `FOR UPDATE` row locks on `user_presence_settings` and `presence_settings_pending_operations` inside `BeginTopologyBatch`. **The #2445 two-phase split MUST NOT be copied here.** That split is sound only because no hooked RBAC write mutates the settings/tier/friends/membership inputs its phase 1 reads; this family's write mutates exactly those, so the graph read is the state the write destroys and must run under the same transaction. The contract enforces that by construction — every capture-**reading** method takes `*sql.Tx`; `WithGatedTx` is the one pre-transaction method and it reads nothing, it only acquires the sender gates and opens the very transaction the capture then runs in. `Subject` carries no audience either, so a caller cannot hand in a pre-computed one. **Do NOT write down the general form "the visibility filter is untouched by these writes."** It is true for friendship writes and FALSE for membership writes (#2447): `FilterVisibleUserIDsForChannelTx` JOINs `server_members` and `member_roles`, which `RemoveMember` and `execBanTx` delete from. Stated generally it licenses hoisting the filter back out of the transaction and reintroduces the #2445 defect in the opposite direction, and no test catches that unless it specifically races it. **What makes the capture correct is not a lock — it is that the clear set is `captured − fresh`.** READ COMMITTED suffices: a concurrent add leaves the new friend out of `captured` and inside `fresh` (never cleared); a concurrent remove clears idempotently alongside that transaction's own clear. Over-clearing is safe here and this shape cannot under-clear. **`Complete` owns the COMMIT and no handler may call `tx.Commit()`** — `presencehistory.CompleteTopologyBatch` commits the caller's transaction itself, so delegating the commit now is what lets the durable Custom Status leg and #2448 swap in at ONE construction site instead of rewriting every hook. `presencehook.Capture`/`Complete` no-op and commit respectively when the capture is unwired, so a replica without the hook behaves exactly as before and degrades to the pre-existing ≤90 s TTL. **An UNDETERMINED base presence is not an empty audience (#2770).** `presence.SenderPresenceResolver` carries TWO methods and the split is load-bearing. `RichPresenceEmissionPermitted` absorbs its own transport errors into a fail-closed `false` — returning an error to the delivery-policy and settings-cleanup callers would route through `failClosedGeneration` into a global Rich Presence disconnect (#2444), so keep those on the bool form. `RichPresenceEmissionState` returns `(bool, error)` and is for the PRE-MUTATION capture path ONLY, reached through the separate `CaptureServerVoiceCandidatesStrict` entry point. **The lenient `CaptureServerVoiceCandidates` keeps absorbing an undetermined presence into an empty set, and that is an RBAC-availability guarantee, not a default:** `voicepresence.Executor.PrepareCapture` calls it before an authority write and `rbac.withAuthorityCapture` turns a `PrepareCapture` error into a 500 with the write blocked, so propagating a transient Redis fault there puts Redis availability in front of CreateRole / UpdateRole / AssignRole / RemoveMember / ban. Same fault, opposite correct answer, because the two callers ask different questions — do NOT collapse them back into one entry point. The strict form exists because `CaptureServerVoiceCandidates` used to collapse a Redis transport error and a failed `presence_offline_fences` read into the same `(empty, nil)` a legitimate suppression returns (a `redis.Nil` is itself a determined suppression and always was), so a transient fault during a removal/block/FoF-toggle dropped the leg with `Degraded()` false and no error, the call site's declared `FailPosture` never ran, and a viewer who had just lost authorization kept the sender's activity until the presence TTL expired (CWE-284). Collapsing the two is correct when the question is "deliver now?" — both answers are no and no write is gated on it — and wrong when the question is "what did this audience look like before the write?". Do NOT wire the State form into a delivery path, and do NOT widen the NIL-resolver case with it: a nil resolver still means `(empty, nil)`, because that is a wiring bug with its own control and other callers depend on the established behaviour. **A leg's captured set is a CANDIDATE superset** — `graphpresence` applies no channel-visibility filter and imports `internal/rbac` not at all. It is the sender's SERVER MEMBERS, intersected with friends ∪ FoF at the default `TierFriends` and taken whole at `TierServers` (`presence.serverVoiceCandidates`), so it is always a SUBSET of the server's membership; "superset" refers only to the missing visibility filter. Do NOT restate it as "friends ∪ FoF ∪ server peers" — that writes a union where the code intersects, and implies a non-member friend is captured when such a viewer is never in the set at all (PR #2770 review). That is safe for delivery, because `ActivityService` unions `recheckViewers` into the clear set and a superset only over-clears; it is NOT safe to hand to a disconnect, which is what the two rules below exist to bound. **Two rules bound the disconnect, both load-bearing (PR #2738 review, five passing `@red-team` PoCs).** (1) **A revoking family with a counterpart reconciles nothing unless an ACCEPTED edge exists** — `CaptureInTx` gates on it, because the presence audience derives from `status = 'accepted'` rows only, so absent→blocked, pending→blocked and blocked→blocked leave every audience byte-identical. Without the gate the counterpart is a raw path parameter: `POST /friends/<stranger>/block` closed every device a stranger had, repeatably, **on the successful commit path**, and dragged their whole Server Voice audience into the plan; the gate also removes the O(audience) work that made `DELETE /friends/<stranger>` a latency oracle for whether a stranger was in voice. (2) **`Abandon` is a no-op for causes that PROVE no commit** (`presencecapture.CauseProvesNoCommit`): every hooked site abandons and returns, leaving its deferred rollback to discard the write, so `CauseWriteFailed`/`CauseRowsAffected` changed nothing and must disconnect nobody — otherwise aborting the HTTP request mid-flight converts one call into a teardown of every user in the captured set. The cause vocabulary is the named type `presencecapture.Cause`, not a bare string — it is branched on AND logged as a fixed `failure_class` enum, so an untyped parameter invited a caller to pass an interpolated DB error into that field (same reasoning as the `mfa.CredEpoch` typing above). `CauseCommitUnresolved` stays fail-closed; the post-commit terminals in `dispatch` are unaffected, being the only place a committed mutation can have left stale state. **Unknown state fails closed; proven no-change must not.** Note `CanRevokeVisibility()` gates only the peripheral `viewers` seed — it does NOT bound `Abandon`. Regressions live in `internal/friends/presence_disconnect_regression_test.go` (inverted exploit proofs: if they fail, the class is back). The peripheral leg covers the **FoF ripple, not the direct edge** — the focal refresh already clears the counterpart exactly, but the counterpart also loses every sender that was FoF-visible only through the mutated edge, and those are never focal. It is seeded for the principal set rather than computed, and **only for mutations that can revoke**: removal, block and the FoF toggle seed it; **accept does not**, because an accepted edge is purely additive. **That is NOT the direction branch forbidden for the #2445 family** — there no static property tells you whether a role write widens or narrows; here it does, the predicate asks "can this revoke?", and it is answered exhaustively at the enum so a new family must state its own answer. `FamilyFriendsOfFriendsToggle` answers true for BOTH directions on purpose. **The disconnect is a full session teardown, not a presence refresh** — `Hub.DisconnectRichPresenceClients` closes every local device for each user — which is why gating it matters. Handlers must NOT branch on `HasWork()` or `Degraded()`; those are for counters. **`FailConservativeDegrade` is declared per call site and used ONLY by `BlockUser`, and it needs a SAVEPOINT to work at all.** Every capture read runs on the caller's `*sql.Tx`, and a failed statement poisons a PostgreSQL transaction (`25P02`), so without one the degrade branch handed back a plan into a transaction that could no longer commit and `BlockUser` 500'd anyway — inert for its entire intended failure mode. `beginSavepoint` opens `SAVEPOINT concord_graph_presence_capture` for that posture only (fail-closed sites block the write by design and skip the round trip), and the degrade path rolls back to it before returning, restoring a usable transaction. There are now TWO savepoints through that one helper — `concord_graph_presence_gate` and `concord_graph_presence_capture` — so do not reason about "the" savepoint. If the rollback itself fails the capture fails CLOSED, because committing a write with no reconciliation is the worse outcome. **Do not "simplify" this by moving the reads to another connection** — they would read a different snapshot and break the in-transaction invariant the whole family rests on. **Forward constraint for the durable leg, now LIVE (PR 2 / #2446):** a failure in the marker-writing stage blocks the write regardless of posture. The grounds are NOT that the markers cannot be rolled back inside the caller's transaction — they can, via the capture savepoint, and believing otherwise pushes the next implementer toward the opposite fix. The grounds are that a degrade rolls the markers back **silently**, stranding a plan whose batch references markers that no longer exist, after which `verifyTopologyMarker` finds nothing current and skips delivery **with no error at all**. That is why the marker write sits between TWO savepoints — the gate savepoint is released before it, and the capture savepoint is opened after it — so a C1 degrade cannot reach the markers (invariant TB-1). Bounds (`maxFocalSenders`, `maxCapturedViewers`) are `const`, never configuration; exceeding the focal bound fails CLOSED regardless of posture. `internal/presencecapture` is a zero-internal-dependency LEAF so five consumer packages gain no presence imports and no test binary can cycle; `internal/presencehook` holds the shared handler plumbing (that split is auditability, not cycle avoidance — `pkg/logger` has no internal deps). `requireGraphPresenceCaptureWired` interrogates the HANDLERS, never the constructed reconciler — `graphpresence.New` never returns nil, so a check on that value is a tautology that still boots with a `SetGraphPresenceCapture` line deleted. **That guard asks ONE question — "was this wired at construction" — and #2854 stage A is the record of what happens when you try to make its seventh predicate answer a different one.** `HasErasureClearPublisher` reads `nats != nil`, and a NATS outage used to reach `log.Fatal` through it, taking **auth and health** down and crash-looping self-hosted and dev deployments. The first fix weakened the predicate to a "was `SetNATS` called" flag. **That was built, reviewed, and REVERTED** — three reviewers found three separate defects: (1) it boots on a **misconfiguration** as readily as on an outage, which is fail-insecure (CWE-636) on a deterministic deploy defect; (2) a nil client silently disables `PermissionEnforcer.dispatch` (`internal/voice/permission_enforcer.go`), so `recheckOne` never runs and its deliberate fail-closed arm never fires — a banned, timed-out or permission-revoked member keeps talking in a live voice room (CWE-613), a surface the guard had been keeping structurally unreachable; and (3) it removes the only observer of `s.nats`'s assignment, after which deleting `s.nats = c` kills the erasure clear on **every** replica with every test still green. **The real defect was never in the guard.** `pkg/nats.Connect` omitted `nats.RetryOnFailedConnect(true)`, and `MaxReconnects`/`ReconnectWait` govern reconnection only AFTER a successful first dial — so a bus down at boot returned an error, left the client nil, and NOTHING retried (`bindRouter` runs once). The nil was PERMANENT for the process lifetime; the old `log.Fatal` under a `Restart=always` supervisor was accidentally supplying the retry, which is why removing it made the degradation permanent rather than transient. With the option set, an outage yields a **reconnecting** client and never reaches this predicate, so a nil arriving here is a deleted wiring line or an unparseable `NATS_URL` — both correctly fatal. **Do not weaken the guard to fix a bus problem; fix the connection.** Relatedly, the boot warning must NOT log the raw connect error: `nats.Connect` returns a `*url.Error` that formats the raw URL, so a `nats://<user>:<pass>@host` misconfiguration writes the credential into the log (CWE-532) — it logs `failure_class: "nats_config"` instead, and must not enumerate affected features (#2875). **Both presence dispatch workers are closed at shutdown, ordered BEFORE `hub.Shutdown()`** — `NewRouter` returns a closer that calls `graphpresence.Reconciler.Close` and `voicepresence.Executor.Close`, and `shutdownControlPlane` runs it between the background-worker wait and the hub. Nothing called either `Close` before PR #2738, so a graceful shutdown left queued plans undrained and their fail-closed abandons never ran; the #2445 executor had the identical gap, which is why one closer covers both. The ordering is load-bearing and locked by `TestShutdownControlPlaneWaitsForHTTPDrain`: the drain's abandons disconnect THROUGH the hub, so a hub closed first would silently discard exactly the disconnects the drain exists to deliver. **A further class joins the `failure_class` vocabulary (#2854 stage C): **`probe_stale`** — a pre-transaction probe that the authoritative in-transaction read contradicted on a path that would otherwise write without a capture. It is a 503 with `Retry-After: 1`, nothing is written, and it carries NO `presencecapture.Cause` and must never reach `Abandon`: no plan exists on that path, so asserting one would put a `DisconnectRichPresenceClients` call on a path that proved nothing happened. It must never be handled by re-entering `WithGatedTx` to promote the request — acquiring a gate while the failed transaction still holds row locks rebuilds the channel-vs-row cycle no deadlock detector can break. Do NOT collapse it into `presence_operation_pending`, which means a durable marker is held and is false here. Two ingress-gate classes join the `failure_class` vocabulary (#2854 B1):** `ingress_budget` (a message shed by a NATS ingress token bucket) and `replay` (a message shed by the erasure-clear dedup window), `ingress_budget` emitted from both doors and `replay` from the erasure door. Only the VOICE door is a decorator (`gateVoiceLifecycle`); the erasure classes are emitted inline from `handlePresenceErasureCleared`, and that asymmetry is load-bearing — a decorator can be un-wired at its registration site without any test noticing, which is why the voice door carries a wiring pin and the erasure door needs none. **The same change also moved the pre-existing `malformed_payload` and `invalid_user` arms from a per-message `log.Error` to the aggregated reporter**, which is both a severity change (ERROR → WARN) and a message-string change on an existing observable: anything keyed on the old `Presence erasure clear rejected` message or on ERROR-level erasure rejections goes quiet. **Neither may be logged per message** — a gate that logs once per rejection IS an amplification primitive, handing a forged flood one log write per message and recreating in the log exactly the amplification the gate removed from the socket; both report aggregated counters on an interval instead (same reasoning as `Hub.ClearErasedSenderCustomText`). The probe's fail-open arm splits its own class three ways -- `lookup_deadline`, `lookup_unavailable`, `lookup` -- because `verified=false` disables dedup for that message, so this line is the only signal that verification stopped working, and a deadline is a different page from an unavailable pool. Written as three statements with LITERAL values, never a classifier call: `nats_rich_presence_log_guard_test` walks this file's AST and requires a literal, and widening that guard to admit a helper would defeat the closed vocabulary it enforces. The raw driver error is never logged -- it can echo a parameter value on a privacy path. A further class, `unhandled_subject`, marks a `voice.*` message whose subject the subscriber has no case for — those are rejected at the subscription rather than copied, queued, dispatched and dropped. A fifth, `oversized_payload`, refuses a payload on LENGTH **before any parse**: `json.Unmarshal` ran on the raw NATS payload ahead of every gate, and the transport's per-message ceiling bounds ONE message rather than aggregate parser work, so an attacker publishing maximum-size documents bought megabytes of scanning per second on the subscription's callback goroutine — the same goroutine whose latency produces the library-level `slow_consumer` drops (CWE-400). The 128KB bound is DERIVED, not picked: the largest honest payload is a heartbeat carrying `maxServerVoiceParticipantIDs` (1000) UUID strings at ~40 bytes each, so ~40KB plus envelope. **The ordering is the security property, so its regression test feeds a MALFORMED oversized document** — a valid one yields `oversized_payload` whether the length check runs before or after the parse, and therefore proves refusal but not sequence; only a malformed one separates the two (`oversized_payload` vs `invalid_event`). **The per-room voice budget meters `voice.heartbeat` ONLY**, and the restriction is load-bearing: shedding a heartbeat is a delay (idempotent, self-repairing, already coalesced), while shedding a `room_empty` is TERMINAL — the room is already destroyed and no successor message exists. Metering every subject with a burst derived from a participant CEILING was the error. **That sentence used to add that `voice_participants` "has exactly one INSERT site with removal-only reconciliation"; #2868 corrected it.** There is one INSERT *statement* — in `moveServerVoiceParticipant`, running `ON CONFLICT (channel_id, user_id) DO UPDATE` — but TWO paths reach it: `handleServerVoiceJoined` and the 30 s heartbeat's `refreshServerHeartbeatParticipant`, both through `applyServerHeartbeatParticipant` → `upsertServerVoiceParticipant`; `bulkRefreshPrivateVoiceParticipants` upserts `dm_voice_participants` the same way. Heartbeat reconciliation is therefore BIDIRECTIONAL — `reconcileServerHeartbeatParticipants` removes the rows the media list omits, the refresh adds the ones it names — so a lost `joined` converges on the room's next tick. **The heartbeat-only restriction is UNAFFECTED by that correction**: it stands on the measured starvation (486 genuine `voice.left` offered in 1.2 s under producer churn, ZERO admitted) and on `room_empty`'s terminality, and `joined`'s convergence touches neither. Do not read the correction as licence to meter the one-shot subjects. **Three further classes name an event the DISPATCHER dropped rather than one a gate shed (#2868):** `dispatch_drop_convergent` (heartbeat, joined or left — the room's next 30 s heartbeat reconciles it in both directions, so the loss is a delay), `dispatch_drop_terminal` (`room_empty` — after it the media plane drops the room from `getActiveRoomIds()`, so there is no successor message for that room), and `dispatch_drop_unresolvable` (the room key took `voiceLifecycleDispatchRoomKey`'s SHA-256 arm, which #2871's G1 leaves reachable only from tests). All three DROP; none disconnects anything. They are emitted from `voiceDropShed` in `internal/voice/ingress_gate.go`, not from `nats.go`, because that file's AST log guard admits only a literal for `failure_class` and only an int literal or `len()` for a count — the three names stay string literals in `voiceLifecycleDropClassName`. A convergent-only report logs at WARN; a report carrying a terminal or unresolvable count logs at ERROR. **`resource_limit` is RETIRED — do NOT reuse it for any of these.** It was the voice-lifecycle overflow arm's class and meant "we tore down every Rich Presence client on this replica"; #2868 removed that teardown, so the value now has **no producer at all** in package `voice` (pinned by `TestRetiredResourceLimitHasNoProducer`). Do not restate it as a live meaning and do not recycle the name onto a shed or a drop: every alert keyed on it has already gone silent, and re-pointing a silent alert at a new meaning is worse than leaving it dead. `pkg/nats`'s async error handler emits THREE further classes, and the split matters because the library's callback fires for far more than backpressure: `slow_consumer` (the client library shed a message before any handler saw it), `subscription_fatal` (a permissions or authorization failure — the subscription is GONE and every message on that subject is lost until the credential is fixed), and `async_error` (anything else). Do not collapse them: an earlier revision stamped `slow_consumer` on all of them, so a dead subscription presented as consumer lag and an operator would tune queue depth while a door was silently closed. **This gets worse after #2857** — on an authenticated bus a permissions violation is the expected fault, not an impossible one — distinct from the two above because those are our deliberate, bounded sheds and this one is a signal that a callback is not keeping up. Design: `2026-08-13-2446-presence-friendship-fof-reconciliation-design.md`.

- **Membership and account-lifecycle writes ride the same rail, and the two directions have OPPOSITE conservative defaults (#2447).** Direct add, invite join, kick, self-leave, ban, server delete and account erasure all reconcile presence, but *conservative* means something different in each direction and getting it backwards is the bug. On a **revocation** (kick, ban, self-leave, erasure) conservative means **over-clear**: an uncleared viewer is a disclosure, an unnecessary disconnect is merely a reconnect — so those paths capture the pre-mutation audience inside the transaction, fail CLOSED on a capture error, and register families with `CanRevokeVisibility: true`. On an **addition** (direct add, invite join) conservative means **withhold**: hydration has no minuend, so a missed hydrate shows the joiner *less* than they are entitled to and self-corrects, whereas failing the request would drive duplicate-join retries. Those paths hydrate through `presence.ActivitySnapshotService.Snapshot` **outside** the `presencecapture` contract, fail OPEN after the commit, and MUST register `CanRevokeVisibility: false`. **A disconnect is never a safe default on an additive path** — `true` there seeds `plan.viewers` and tears down every device of the user who just joined. Two further traps: there is **no separate Leave handler** (self-leave is `RemoveMember` with `isSelfRemoval`, so hooking "leave" means hooking removal), and the account-erasure path is **already inside** `presenceHistory.WithSender`, so it must capture through an `...AlreadyGated` entry point — reaching `presencehook.WithGatedTx` there hashes to the same buffered-1 sender-gate stripe and self-deadlocks with no timeout and no detector. The `#2681` memoized `server_members` loader must be built over the same `*sql.Tx` as the write and discarded with it: #2447 is the family that mutates that table, so a loader shared across captures or hoisted onto the `Executor` would serve a pre-mutation answer to a post-mutation reader. **The opposite-conservative-defaults principle extends to #2854 stage C's probe.** On an ADDITIVE path a stale probe may short-circuit to a retryable 409/404, because the worst outcome is a wrong answer the caller retries away and nothing was written. On a REVOKING path that still performs its write it may NOT: it must abort with `presencehook.ErrProbeStale` rather than proceed, because proceeding applies a revocation with no capture and leaves the counterpart holding a Custom Status that carries no TTL (CWE-284). Same probe, opposite correct answer, for the same reason the two directions differ everywhere else in this family.
- **The presence-audience revocation fence is LEVEL-triggered, and coverage is a property of REACHING the choke point (#2992).** `Hub.presenceAuthzEpoch` became `presenceAuthzState`, one `atomic.Uint64` packing `epoch<<32 | openCount`. The epoch alone is EDGE-triggered and structurally cannot see a revocation that was already open when the audience worker sampled — by then that bump is folded into the value the worker stored, which is the COMMON case, because a gated revoking transaction routinely outlasts a three-round-trip audience query holding no snapshot. A revoking transaction therefore brackets itself **before `BeginTx`** via `Hub.BeginAudienceRevocation()`, whose `sync.Once`-wrapped closer lowers the count, and a worker reading a non-zero `openCount` knows a revocation was in flight *when it looked*. A result is unproven iff `openCount != 0` in the sampled state OR the epoch half moved since — **neither the commit time nor the dispatch delay appears in the proof**, which is what makes the two arms exhaustive given that the epoch is bumped at bracket OPEN. The bracket must PRECEDE `BeginTx`: one taken afterwards cannot order a query that started in between. It is acquired at `graphpresence.runInTx`, which gated sites reach *through* `presencehook.WithGatedTx` — but a site that nils its capture takes that helper's unwired branch and never arrives, which `FamilyBlock` and `FamilyMemberBan` deliberately do, so **do not write "all nine gated sites are covered by construction"**. `deleteAccount` owns its transaction and is hand-wired as `defer s.audienceFence.BeginAudienceRevocation()()`; the method guards its own nil receiver, so that call site needs no branch. `DeleteServer` is different: `activepresence.Rail.WithGatedRevocationTx` acquires the sender gates, opens the audience fence, and only then begins the transaction, keeping the fence open through commit and `CompleteAlreadyGated`. Its C2 Custom Status compensation (`reconcileServerDeleteAudience`) deliberately runs after that rail-owned bracket closes and must run whenever `outcome.committed` is true, including the 503 delivery-incomplete path; do not return on the acknowledgement error before it runs. Family scoping comes from the boot-guarded `presencecapture.PolicyFor` and **fails CLOSED on a lookup error**: an unregistered family is exactly the case where a write is not known to be additive. The decrement is `Add(^uint64(0))`, -1 on the FULL 64-bit word, so running it at `openCount == 0` borrows out of the epoch half — rewinding the epoch AND setting the count to `0xFFFFFFFF`, a permanent hub-wide presence blackout; `sync.Once` is what makes that unreachable and nothing may decrement outside that closer. **No migration and no config** — the fence is process state, and therefore blind across replicas, which is why `validateControlPlaneReplicaCount` refusing any count but 1 is load-bearing for it. One `failure_class` joins the vocabulary: **`presence_authz_fence_held`**, emitted log-only by `watchPresenceAuthzLevel` after four consecutive raised 30 s ticks (which proves ≥90 s of continuous hold, not 120 s — N observations bound the N-1 gaps between them). That watchdog is an OBSERVER and must never write the fence: a reset would be a fail-open timer on an authorization control. It also will NOT catch a sustained revoking-write loop, whose count drops momentarily between transactions. Finally, **`broadcastPresenceToAllSync` consults NEITHER arm**, and that is a stated exclusion rather than an oversight — it is the shutdown-only synchronous path, its fan-out is process-local with no NATS leg, and every recipient is `closeOutbound()`-ed a few lines below.
- **Step-up policy is owned by `internal/stepup` (#2765).** Password + MFA step-up had grown FIVE independent implementations that disagreed with each other, and consolidating them is the point — **not** a NULL-`password_hash` bug. **`users.password_hash` is `TEXT NOT NULL` (migration 000001, never relaxed), and SSO registration stores a real Argon2id hash alongside a `password_login_disabled` flag that is set, so an account with no stored hash is not reachable today.** An earlier revision of this entry (and of the #2765 design) asserted that four copies returned a raw 500 to "passwordless SSO accounts"; that premise is FALSE and was corrected in-PR after audit — do not restore it, and do not cite `COALESCE(password_hash, '')` as evidence a NULL occurs. The no-password-factor branch and that COALESCE are deliberate **defence-in-depth** for a state the schema forbids: they cost one comparison, they give a defined answer to a future schema change or an auth method with no password, and `internal/users`' `TestUsersPasswordHashCannotBeNull` fails loudly if the constraint is ever dropped so the branch gets revisited rather than silently becoming live. **The real defects consolidation fixes are divergence ones** — the five sites returned different statuses for the same condition, and only `internal/dm`'s offered an actionable 400 when no password factor was available instead of a bare rejection. **Do not grow another copy.** **SIX** password-verification helpers remain off the seam, each doing its own `SELECT password_hash … Scan` — census taken against this branch, so trust the count rather than re-deriving it from memory:

| Site | Helper |
|---|---|
| `internal/users/handlers.go:1198` | `verifyCurrentPassword` |
| `internal/users/handlers.go:1278` | `verifyStepUpWithLockedUser` |
| `internal/users/handlers.go:2759` | `verifyCurrentPassphrase` |
| `internal/ownership/handlers.go:1581` | `verifyPassword` (password only — `verifyMFA` at :1609 moved onto the seam, see below) |
| `internal/mfa/handlers.go:383` | `verifyUserPassword` (behind `RegenerateBackupCodes` only — `requirePasswordAndMFA` moved onto the seam) |
| `internal/sessions/handlers.go:208` | `verifyUserPassword` (behind `authenticateForRevoke`, which still accepts password OR code and offers flag-based `methods` from `users.mfa_methods`) |

An earlier revision of this entry said "three", naming only rows 2, 4 and 5 — a **half-count**, which is worse than no list at all because it reads as complete and would send an author away believing the sweep was finished. Migrating any of them is a consistency win, **not** a bug fix: none of them can fail on the NULL that the retracted premise imagined, so there is no defect to repair and no urgency. Weigh each on its own blast radius — row 2 in particular is the authoritative reauthentication checkpoint for password change and key replacement. Note also that **no step-up path consults `password_login_disabled`**, so a password remains a valid step-up factor for an account that cannot use it to log in (`internal/auth/handlers.go` gates login on it). That is pre-existing across all five sites and deliberate-by-default — the actor chose that password at SSO registration — but it is an unexamined policy edge worth a decision before #2565 / #2562 / #2567 build on this seam.

**Policy P1 is now enforced everywhere on the seam (#2765 Phase-8 fix batch).** "Has MFA" for step-up means TOTP (`enabled AND confirmed`) or WebAuthn (any credential), read from the factor tables, never `users.mfa_methods` — and that reading now governs every step-up-gated route: the MFA-settings in-tx gate, the nine former `requirePasswordAndMFA` routes, the purge-fence toggle, DM Clear/purge, and ownership transfer. `internal/mfa`'s off-seam `verifyUserPassword` (the census row above) is now called by `RegenerateBackupCodes` only — `requirePasswordAndMFA` was rewired onto `stepup.LoadSubject` + `stepup.VerifyPasswordFactor` + `stepup.VerifyMFAFactor`, so the package's nine pool-side routes (`TOTPSetup`, `WebAuthnRegisterBegin`, `SetRecoveryOnly`, `SetRecoveryHardened`, `EmailSmsSetup`, `DesignateTrustedDevice`, `RemoveTrustedDevice`, `UpsertRecoveryCircle`, `DeleteRecoveryCircle`) now answer with the same `internal/stepup` bodies as the four in-transaction routes (`password_required` / `mfa_required`+`methods` / `Invalid password` / `Invalid MFA code`) instead of their old byte-flat "Incorrect password, or MFA code required/invalid", and gain the shared attempt budget. `internal/ownership/handlers.go`'s `verifyMFA` (:1609) now reads `stepup.InlineMFAMethods` directly, once, and fails closed with a 500 on a read error — it does not route through `requirePasswordAndMFA` (ownership transfer has its own password verifier, `verifyPassword` at :1581, which stays off-seam), but it no longer derives "has MFA" from `users.mfa_methods` either.

**`stepup.LockSubjectTx`** is the common prefix every in-transaction gate now shares (`mfa`'s `openMFASettingsTx`, `users.gatePurgeFenceDisable`, `dm.loadClearStepUpSubject`): lock the `users` row (`FOR SHARE` or `FOR NO KEY UPDATE`, following the writes), `credepoch.MatchEpoch` against the token epoch, then read the P1 predicate in a SEPARATE statement so a factor committed while the request waited on the lock is still seen. A missing `users` row (a deleted account holding a still-valid JWT) is 401 `"Session no longer valid"`, not the pool-side `LoadSubject`'s 500.

**`stepup.Budget`** is the shared 5-per-15-min fail-closed attempt budget every credential-bearing step-up now charges — the MFA-settings gate, the nine legacy routes, the purge-fence toggle each construct one over the same `middleware.AllowUserAction` primitive with their own key prefix. DM Clear and DM purge do **not** charge it: their routes are bounded instead by the fail-closed purge rate limit (`RateLimitByUserFailClosed`, sized by `resolvePurgeRateLimit`), which counts every request whether or not it carries a credential. It answers 429 `"Too many verification attempts"` when spent and **503** `"Verification is temporarily unavailable. Try again in a few minutes."` when it cannot be evaluated at all (a Redis outage) — deliberately NOT a 429 in that case, since reporting a server fault as "too many attempts" misattributes it to the caller and locks the form for a window that never started. It is charged only when the request carries a credential (`creds.sent()`), for the same reason as before: a credential-less probe learns nothing beyond the fixed refusal every account gets. It is cleared only after a VERIFIED, COMMITTED success. A verifier-side failure (a database read error, an MFA backend error) is now a uniform 500 `"Verification failed"` on every seam route, replacing the nine legacy routes' old 403.

**D1 invariant.** `TOTPDisable` and `WebAuthnDeleteCredential` refuse (409, `{"error": "Turn off email and text-message codes before removing your last authenticator app or security key.", "inline_factor_required": true}`) when the delete would leave email/SMS as the account's only remaining second factor — checked inside the transaction via `refuseLastInlineFactorTx`, after the delete but before commit, so the rollback undoes the delete. Neither route is charged against the shared attempt budget: both call `stepup.LockSubjectTx` directly rather than going through `openMFASettingsTx`/`allowMFASettingsStepUp`. Both still answer a wrong password or code with the seam's exact 403 bodies, `stepup.ErrMsgInvalidPassword` and `stepup.ErrMsgInvalidMFACode`, although they verify off the seam: the desktop's `classifyStepUpRefusal` matches those strings byte-for-byte, and the routes' own older wording ("Incorrect password", "Invalid MFA code. Make sure…") fell through to a generic banner, so the refused field was never marked or refocused. **Accepted residual:** the email/SMS half of that check reads `es`, captured via `readEmailSmsForSync` BEFORE `BeginTx`, so a concurrent email/SMS activation landing between that read and the in-tx check is not seen — D1 can race a concurrent activation. **Accepted residual, unrelated seam:** `internal/sessions`' `authenticateForRevoke` (census row above) is untouched by this work — it still accepts password OR code and offers flag-based `methods` from `users.mfa_methods`, off the P1 seam. That `methods` list (the `auth_required` refusal's) is `users.mfa_methods`, and no desktop code reads it: the #7 route adapter (`stepUpRouteAdapters.ts`) maps `auth_required` to `mfaRequired` with `methods: null` and the step-up requirements read decides what is offered.

**Policy P1 — the MFA leg is required only for a factor the server can verify inline.** `inlineMFAMethods`/`InlineMFAMethods` derives the set from the factor tables themselves (`user_mfa_totp` — enabled AND confirmed — and `user_mfa_webauthn`), never from `users.mfa_methods`. Email and SMS carry no inline verifier — `verifyCodeMatchedMethod` accepts only a TOTP code, a WebAuthn inline token minted for the caller's own purpose, or a backup code (which itself needs TOTP) — so requiring one for an email/SMS-only account would lock it out of its own settings, and `mfa_methods` can be stale in both directions relative to the confirmed-and-enabled state the factor tables hold. **A caller with an enclosing transaction MUST use `VerifyMFAFactorTx`, not `VerifyMFAFactor`:** verification WRITES, so verifying on the pool while a transaction is open lets a rollback burn a single-use factor while changing nothing else. Two writes do this, not one: backup-code redemption marks the code used, and **a TOTP match advances `user_mfa_totp.last_used_step`** (migration 000158). That column is the last 30-second time step (Unix time / 30) at which the user's TOTP code was accepted; `acceptTOTPStep` records a match through `UPDATE … WHERE user_id = $1 AND (last_used_step IS NULL OR last_used_step < $2)` and accepts only on exactly one affected row, so a code verifies at most once across every route (RFC 6238 §5.2) and, of concurrent submissions of one code, exactly one wins. A refused advance answers exactly as a wrong code — no log line, counter or security-event reason tells them apart (`observability.md` principle 7) — and is never retried as a backup code. The advance runs on the **same store the caller verified on**, so inside a transaction a rollback un-burns the step with everything else; on the pool path (`VerifyCode` on `h.db`, reached through `stepup.VerifyMFAFactor` and by `internal/ownership`/`internal/sessions`, plus the login challenge, which calls `verifyCodeMatchedMethod` on `h.db` directly) it commits at once, so an action that fails after verification has still spent that step, exactly as a backup code already was. `TOTPVerifySetup` and `RegenerateBackupCodes` check a code with `MatchCodeStep` directly rather than through `verifyCodeMatchedMethod`, and each folds the step into its own UPDATE: verify-setup under `enabled = FALSE AND totp_secret_nonce = <the nonce it read>` (a lost double-submit answers the existing "already verified" 409; a `TOTPSetup` re-enrolment that replaced the secret between the read and the write answers "TOTP setup was restarted" 409; neither returns backup codes, and without the nonce the re-enrolled row, also not enabled, would be enabled by a code checked against the previous secret), regenerate under the step guard (a replay answers the existing "Invalid TOTP code" 403). `TOTPSetup`'s upsert resets the step to NULL with the secret. `internal/dm` correctly uses the non-tx form because it has no enclosing transaction — the asymmetry is deliberate, not drift. The `stepup` package declares its MFA dependency at the consumer (the `rbac.PresenceRecheck` precedent); its INTERNAL imports are `internal/auth`, `internal/credepoch`, `internal/middleware` (the `Budget` limiter) and `internal/securityevent` (the password-token mint), so it adds no internal import edge onto `internal/mfa` — and because it imports `internal/auth`, `internal/auth` can never import it: the login, refresh and SSO challenges reach `stepup.DefaultMethodReader` through `auth.Handler.SetDefaultMethodReader`, wired in `router.go` — it does also import `gin`, because `Error` carries a `gin.H` body; note `mfa.Verifier` has only three methods and does **not** satisfy the four-method union, which is why narrower per-function interfaces exist — do not collapse them. `Error.Cause` is for the CALLER to log and `Write` never serializes it: populated on a 500, **nil on every 4xx** (a rejected credential is an outcome, not a fault, and logging one hints at which accounts are being probed), and never carrying the password-verification error, because an argon2 decode failure can embed the malformed hash and a password hash may reach no log sink in any form (see `observability.md` Core principle #1) — that path carries the fixed `ErrPasswordVerification` sentinel instead.

**The P1 text lives in two shared fragments, and one users-anchored reader serves presentation (MFA picker PR 1).** `p1TOTPActive` and `p1WebAuthnAny` in `stepup/subject.go` are concatenated into both `inlineMFAMethodsSQL` and `inlineMFAFactorsSQL` — compile-time `const` concatenation, never `Sprintf`. `TestInlineMFAMethodsSQL_RenderedUnchanged` pins `inlineMFAMethodsSQL` byte-for-byte, because rbac's dangerous-permission mask and every gate read it; change a fragment and that test, not a review, is what notices. `loadSubjectSQL` keeps its own copy keyed on `u.id`, and `TestP1Equivalence` holds all four readers (`InlineMFAFactors`, `InlineMFAMethods`, `LoadSubject`, `LockSubjectTx`) to one answer across every fixture. **The readers differ on a missing user on purpose:** `inlineMFAMethodsSQL` has no `FROM` and answers any uuid with `(false, false)` (authorization-critical, unchanged), while `InlineMFAFactors` is anchored on `FROM users u WHERE u.id = $1`, so a deleted account is `subjectGone()` — the GET would otherwise answer a live token for a deleted user with an empty 200. `InlineMFAFactors` backs `GET /mfa/step-up` (`stepup.RequirementsHandler`, `no-store`, 200/401/500 only, never 404) and the challenges' advisory `default_method`; it decides nothing. Its `backup_code_available` pairs `backup_codes_hash` and `backup_codes_used` with two-array `unnest` (`h IS NOT NULL AND used = FALSE`) so it agrees with `mfa.VerifyBackupCode` on a damaged row, and its TOTP recency is `last_used_step × 30`, which is precise to the verifier's ±1-period skew band — the accepted precision, not a bug. Never log the chosen factor, the last-use times or backup availability (`observability.md` principle 7).

**A WebAuthn inline token is bound to ONE consumer route (`stepup.Purpose`, #3453 RS11).** `POST /mfa/webauthn/verify-inline/begin` takes a strict `{"purpose": "<stepup.Purpose>"}` body (1 KiB, one object, a known value, else a fixed 400); the purpose is stored WITH the ceremony session and finish reads it from there, never from its own body; since #3509 the token is a `webauthn` row in `step_up_tokens` (§ "Step-up tokens" below), and the ceremony session stays in Redis at `mfa_inline_purpose_session:<uid>`. **The session prefix must stay one a pre-purpose binary never builds**: that binary finishes `mfa_inline_session:<uid>`, so on a shared prefix an old replica in a rolling deploy finishes a new ceremony into an unbound token (pinned by `inline_purpose_mixed_fleet_test.go`). A token minted by a replica from before #3509 lives in Redis and a new replica never reads it, nor the reverse: during that rollout a WebAuthn confirmation can fail closed once and the user repeats the ceremony. Every verify entry point — `VerifyCode`/`VerifyCodeTx`, `stepup.VerifyMFAFactor`/`Tx`, `mfaenforce.ConfirmTx`/`RequireConfirmationTx` — takes the caller's own purpose and spends only a row with that purpose, so a token minted for another action matches nothing: refused exactly like an invalid code and NOT consumed. The rules: **one constant per route, never per family** (a shared purpose makes a token minted for one route spendable on the other — `#3454` added one per dangerous action), and **a shared verification helper takes the calling route's purpose as a parameter** — `users.verifyStepUpWithLockedUser` serves both the password change (`account.password_change`) and the E2EE key reset (`account.e2ee_key_reset`), and when it hard-coded the first, a token minted to change the password reset the E2EE identity. The consumer tables in `internal/mfa` drive each purpose at its verify entry point and cannot see a route passing another route's purpose; that is pinned at the handler (`internal/users/key_reset_inline_purpose_test.go`). Also: add it to `allPurposes` in `internal/stepup/purpose.go` and to the OpenAPI enum, and mirror it in the renderer's `stepUpPurpose.ts` if a prompt uses it. An invalid purpose passed to `VerifyCode`/`VerifyCodeTx` is a 5xx wiring error, never a silent TOTP-only downgrade. **The login MFA `Verify` never accepts an inline token** (`noInlinePurpose`); do not invent a login purpose. No log line, counter or event may carry a refused token's purpose or distinguish a mismatch from an absent token (`observability.md` principle 7).
- **Disabling a security control costs at least what using it costs (#2765).** `require_auth_before_purge = false` is gated by the same step-up the purge demands; setting it **`true`**, and every other field on `PATCH /users/me/privacy`, stays unauthenticated. Tightening a control must never be harder than loosening it. Three properties are load-bearing and must survive any refactor: (1) the check runs **inside** the write transaction, taking the `users` row **`FOR SHARE`** as the transaction's first statement — that lock conflicts with the `FOR NO KEY UPDATE` every destructive reset holds, so a request authorized by credentials superseded while it waited blocks, re-reads, and fails; verifying before `BeginTx` reopens exactly that window. Do **not** upgrade it to `FOR NO KEY UPDATE` — this handler never writes `users`. (2) It precedes the row-ensuring `INSERT … ON CONFLICT DO NOTHING`, so a rejection leaves **no** `privacy_settings` row behind; verifying after it would let a 403 flip the fail-closed "no row means true" default. (3) It gates on the **requested value** being `false`, never on a computed OFF *transition* — a prior-value read is ambiguous on a lazily-created row, which is precisely where it would fail open. The corollary is a client contract: `require_auth_before_purge` must never ride in a bulk privacy save. The same read carries `credential_epoch` for `credepoch.MatchEpoch` (**not** `GuardTx`, which would issue its own `FOR SHARE` on a row already locked): the step-up proves knowledge of a password, the fence proves the session was not revoked, and those are different facts. Credential fields live in a **separate struct** from the column fields, so `buildPrivacyClauses` — which turns every non-nil field into a SQL `SET` clause — is structurally incapable of seeing one; adding a credential to the wrong struct is a compile error, not a review catch. The route limiter stays fail-**open** (it protects twelve innocuous fields); the credential branch carries the shared fail-closed `stepup.Budget` (5 per 15 min, 429 when spent, **503** — not 429 — when the budget itself cannot be evaluated), the same type the MFA-settings gate uses. **Lock strength follows the writes, not the route:** the MFA-settings gate takes `FOR NO KEY UPDATE` for `EmailSmsDisable` and `SetBackupEmail`, which write `users` in the same transaction, and `FOR SHARE` for `StoreRecoveryKey` and `DeleteRecoveryKey`, which do not — taking `FOR SHARE` and then separately `UPDATE users` in one transaction is a lock upgrade, and two concurrent same-user requests on that shape deadlock rather than serialize.
- **`ReorderRoles` serializes its authorization with its position write (#2851).** `applyRolePositions` was the one role-authority writer in `internal/rbac/handlers.go` taking neither `withAuthorityCapture` nor `LockServerVisibilityCapture`, while its guards read on `h.db` in autocommit. Two defects followed from that single structural fact: a PoC-proven CWE-367 straddle in which an actor at position 5 demoted a role promoted to 19 after the reads had already passed, and a live `40P01` deadlock, because the write loop iterated the CLIENT-SUPPLIED role order and two concurrent reorders therefore took row locks in opposing sequences — no attacker and no elevated privilege required, just two admins reordering at once. `LockServerVisibilityCapture` is now the transaction's FIRST statement, the transaction pins `sql.LevelReadCommitted` explicitly, and `evaluateReorderGuards` re-runs authoritatively inside it, reading owner, actor ceiling, position violations and managed/default flags in ONE statement so they share a snapshot — the *incoherence* half of #2721, not merely staleness. **The isolation pin is load-bearing:** the lock-then-read ordering is only meaningful under READ COMMITTED, where the guard SELECT takes its snapshot after the lock is granted; under REPEATABLE READ the snapshot is registered at the transaction's first statement — the lock itself — and the straddle silently reopens with every test still green. **The write is ONE batched `UPDATE … FROM unnest(...) WITH ORDINALITY`, not a loop:** the advisory lock is held to COMMIT, so a per-role loop held a server-wide lock across a client-controlled number of round trips; a single statement bounds the hold regardless of list length, and `RowsAffected` must equal `len(role_ids)` or the whole reorder rolls back. `role_ids` is `max=500,unique,dive,uuid` — element type closes CodeQL `go/log-injection` at the boundary (an unvalidated element reached Postgres, and the resulting `22P02` carried the attacker's string into the log), while `max` and `unique` are load-bearing for the lock hold and for the exactness of the `RowsAffected` check respectively. **The default role is ALWAYS the lowest role and can never be promoted — by anyone, the server owner included.** Every member holds `@all`, so its position sets the floor of `COALESCE(MAX(r.position), 0)` for the entire membership; promoting it in one reorder would raise every member's hierarchy ceiling at once. It is created `is_default = TRUE, is_managed = TRUE` (`internal/servers/handlers.go`), so the managed-role refusal deliberately exempts default roles — refusing every managed role would reject any full-list reorder, which is exactly the payload the hierarchy UI sends — and the stricter last-position rule governs it instead. **Scope of the advisory order, stated precisely because it is narrower than it looks:** the lock totally orders writes to `roles.position`, and `applyRolePositions` is the only such writer, so add the lock with any second one. It does **not** order the guard's other two operands — `member_roles` (member add/ban/kick in `internal/members`) and `servers.owner_id` (`internal/ownership`) write outside this key, so a ban or an ownership transfer committing mid-transaction is accepted at one transaction's width of staleness. **The pre-transaction call of the guard is a fast path only:** non-authoritative, FAILS OPEN so a transient pool fault can never deny a legitimate request, and carrying no locking clause — forced rather than chosen, since the actor ceiling is an aggregate and PostgreSQL rejects a locking clause at a query level carrying aggregation (`FOR SHARE is not allowed with aggregate functions`). Do not make it authoritative, do not deny on its error, and do not give it a locking clause.
- **The CLIENT-VERSION gate does not apply to the media-plane's server-to-server hop; the ATTESTATION gate does (2026-09-02 outage).** `RequireClientVersion` and `RequireAttestation` both sit on the `authRequired` group, so both cover the two endpoints the media plane re-validates against — `POST /channels/:id/voice/join` and `POST /dm/conversations/:id/voice/authorize`. Setting `CLIENT_MIN_VERSION` at production scope made `clientversion.Parse("")` fail for a hop that sends no version header, and **every voice join — server channel and DM alike — 403'd**; the media plane then collapsed that into the same `Not authorized to access this channel` string an RBAC denial produces, so neither the user nor an operator could tell the two apart. `middleware.MediaPlaneServiceHop` marks an authenticated hop via `pkg/mediaproof`, and `RequireClientVersion` skips a marked request. **The exemption stops there, and the asymmetry is the point.** A version header is client-asserted on the direct path anyway, so exempting it grants an attacker nothing they did not already have; attestation is the one control a tampered client cannot satisfy. **Do not extend the exemption to `RequireAttestation`** — an adversarial pass with an executable PoC proved the premise that would justify it false: the media plane admits SFU sockets on the JWT alone (`createAuthMiddleware` verifies signature, issuer and `user_id`, nothing more) and server-channel `AuthorizeJoin` writes NO reservation, so nothing forces the user's own gated call to have happened first. Same user, same revoked build: 403 `ATTESTATION_REVOKED` on the direct call, **200 through the hop**, with ADR-0010's D13 kill-switch never consulted. The DM path is safe only because `AuthorizeVoiceJoin` mints a call lease that `/voice/authorize` cannot create for itself (pinned by `handlers_test.go`'s "media authorization must not create a call lease without /voice/join") — that lease, not the HMAC, is what supplies the ordering there. Making voice work with attestation ENABLED means relaying the client's `X-Attestation-Token` / `X-Machine-Id` from the handshake, with `X-Session-ID` taken from the bearer's own verified `sid`; that relay shipped in #4206 (`attestationRelayHeaders` + `bearerAttestationHeaders`, UUID-only, bearer hops only; `attestation_relay_test.go` pins the gate order). **It is also not the credential relay an earlier revision of this rule called it** — `loadAndAuthenticateToken` resolves the record from the `(session, machine)` headers on the request being validated and compares it to the user authenticated by that same request's bearer token, which the media plane already relays, so a relayed triple satisfies every binding while a cross-user relay still 403s (both proven). Four properties are load-bearing. (1) The proof binds method, **full request URI** (not just the path — the query string is part of the request being authorized) and a SHA-256 of the bearer token, with a `mediaproof.MaxClockSkew` replay window. (2) A present-but-INVALID proof is **not** an error — the request earns no exemption and proceeds through the gate like any other, because rejecting outright would turn a clock-skew blip or a mid-deploy secret rotation into a second outage — but it is **logged, sampled**: with no gate enabled a broken proof returns 200, so an unlogged failure would let the mechanism die silently and re-arm the outage for the next operator who sets `CLIENT_MIN_VERSION`. (3) An unconfigured `JWT_SECRET` yields a nil key, and `mediaproof.Sign` and `Verify` both treat nil as never-valid, so a misconfigured deployment cannot turn the exemption into a wildcard. (4) The marker is honoured only on the two allowlisted route patterns, and only when `AuthRequired` has already put a `user_id` on the context — the middleware enforces its own registration contract rather than documenting it, and `router_client_version_wiring_test.go` pins both the ordering and the attestation non-exemption against `router.go`'s source. **`pkg/mediaproof` is shared, but each caller supplies its OWN version and purpose strings** — the DM endpoint proof (`concord/dm-voice-media-authorization/v1`, which binds the conversation and call ID and gates the media handoff itself) and the service-hop proof (`concord/media-plane-service-hop/v1`) derive different keys and are not replayable as one another. The package prepends the version and timestamp itself so a caller cannot skew-check one timestamp and sign another, and refuses to sign or verify any field containing a newline — `c.Request.URL.Path` is percent-decoded, so `%0A` yields a real newline and the "no caller can supply one" argument an earlier revision made was false.
- No `unsafe` package, no `fmt.Sprintf` for SQL

## MFA-enforcement permission mask (#3453)

`servers.enforce_mfa_dangerous_actions` (migration 000157) lets an owner or a
raw-bit Administrator withhold the dangerous permission bits from any member
with no inline MFA factor. Full design: the #3453 spec.
Do not restate its invariants here beyond what a future change needs to not
break them.

**The mask (`rbac.MFAMask`/`Apply`, `types.go`).** `DangerousPermissions`
(`0x0200419B`: manage server/roles/channels/crypto-rotation, kick, ban, manage
all messages, manage dev resources) and `ConcretePermissions` (every named
`Perm*` except Administrator, `0x3FFFFFFF`) are `const`, never derived from the
`PermissionNames` map at init — a `var` map can't fail a build when a bit is
forgotten, a pin test can. `Apply` first **EXPANDS** bit 62 into
`ConcretePermissions` and only then strips `DangerousPermissions`, because
`Permission.Has` short-circuits on `PermAdministrator` alone: clearing bit 62
without expanding it first would drop an unenrolled Administrator's *entire*
permission set, not just the dangerous slice. I5: Administrator identity —
whether to run that expansion at all, and the owner/admin bypass inside the
resolver and the role guard — is always decided on the **raw** bitfield, never
the masked one; masking is a read-side transform of a result, not an input to
any decision. **Single-exit rule:** every public resolver entry point
(`computeEffectivePermissions`, `ResolveEffectivePermissionsUncached`,
`ResolveServerPermissionsTx`, `ResolveChannelPermissionsTx`,
`ResolveEffectivePermissionsForChannelsFresh` per channel) computes the raw
value through every internal branch — including the owner short-circuit and
the channel-override Administrator bypass — and calls `mask.Apply` exactly
once, at its own `return`. Applying it earlier would let a stripped-Administrator
fall through into SBAC override evaluation, where a DENY that bit 62 is meant
to ignore would start to bind. `visibleChannelIDs`, `allVisibleChannelIDs` and
`filterVisibleUserIDsForChannel` are **never** masked (I4) — visibility,
presence audience and voice enforcement are unrelated to this control and a
flip must cause zero voice rechecks, presence captures or disconnects.

**Generation-tagged cache (`rbac/cache.go`).** Every `perm:*` value now carries
two generation tags — `permgen:u:{userID}` and `permgen:s:{serverID}` — read in
the same `MGET` as the value, and a value is served only when both tags are
present and match. Generations are **64 random bits from `crypto/rand`, never a
counter**: a counter that gets evicted and re-seeded can restart where it
started and match a stale value again, where a random replacement cannot. A
compute is tagged with the generations read **before** it started (`Get`
returns them on a miss; `ResolveEffectivePermissionsFresh`, which never calls
`Get`, reads them itself first via `Generations`), and every bump runs **after**
its writer's commit — that ordering is what makes every read/write interleaving
end in either a miss or a correct value (see the cache.go package comment for
the full proof). When a tag is absent, `Set` seeds it with `SET NX` and writes
**no value** ("seed-and-skip") rather than publishing a value tied to a
generation that didn't exist yet. A bump that fails twice falls back to
`DropUserGeneration`/`DropServerGeneration`, which delete the **generation key
first**, then the entries. Deleting the generation is what invalidates: a cache
read with it absent misses and reseeds, and a step-up grace read with it absent
is not covered. A fallback that deleted only the entries left in place the
generation every grace is stamped against, so a grace taken before an MFA
factor change, or before an enforcement off-and-on, kept covering for its window
(Codex review of #3454). Do not "simplify" the fallback back to
`InvalidateUser`/`InvalidateServer`. **Residual, stated so it isn't rediscovered
as a bug:** only the MFA hooks and the toggle bump generations today. Role,
membership and override writers still `Invalidate`/`InvalidateServer` by
deleting entries outright, so a compute already in flight when one of those
commits can still republish the value it just invalidated, for up to the
5-minute TTL — the same residual that predates #3453. Moving those writers onto
bumps is a separate change.

**Every key is built from a CANONICAL id, never the request's spelling.**
`permgen.CanonicalID` (the `internal/permgen` leaf, #3454; formerly `rbac`'s
`cacheID`) folds every uuid spelling PostgreSQL resolves to
the same row (either case, braces, hyphens after any group of four digits, or
none) onto the canonical lowercase 8-4-4-4-12 form. It strips `-`, `{` and `}`
wherever they appear, so it also folds spellings PostgreSQL rejects. Those
requests fail at the database anyway, so the wider fold is harmless. Any
string that does not reduce to exactly 32 hex digits is used as given.
Every key builder (`cacheKey`, `permgen.UserKey`, `permgen.ServerKey`)
and every invalidation pattern goes through it. The generation keys are
spelled ONLY in `permgen`, because two packages read them: `rbac` writes and
bumps them, and `stepup`'s grace store (#3454 D-2) reads them to stamp and
judge a grace. `rbac` imports `stepup`, so `stepup` cannot import `rbac`; a
second spelling in `stepup` would let the two drift, and a grace would then
survive every bump it exists to observe. **The failure it closes was
live:** the Phase-4 `@red-team` pass primed `perm:<UPPERCASE-ID>:<uid>`
through `/servers/<UPPERCASE-ID>/permissions` before a flip. That entry was
tagged with `permgen:s:<UPPERCASE-ID>`, which the canonical bump never
reaches, so an unenrolled Administrator renamed an enforcing server through
the same spelling. The canonical-only `Invalidate*` patterns had the same
hole for role and override revocations before #3453.
`TestPermissionCache_EveryUUIDSpellingIsOneEntry` and
`TestMFAEnforcement_FlipReachesEveryIDSpelling` pin it. The lesson is the
#3362 one, one layer over: when an identifier keys two stores with different
equality rules, the string-keyed store must adopt the database's equality.
Do not "optimise" `CanonicalID` away as redundant with route validation, because
no route canonicalizes `:id`.

**`internal/mfaenforce` (leaf package; imports only `stepup` + stdlib + `pq` +
`gin`).** `LockGateTx(ctx, tx, serverID, actorID, userLock, serverLock,
tokenEpoch)` runs `stepup.LockSubjectTx` first (locks `users`, fences the
credential epoch, reads the P1 subject), then locks `servers` at the caller's
chosen `ServerLock` and reads `owner_id`, the flag, and
`current_setting('transaction_isolation')` **in the same statement** — a REPEATABLE
READ transaction is refused there at zero extra round trips, because a P1
verdict read under anything but READ COMMITTED can be stale by the time the
gate uses it. **`ServerLock` must be at least as strong as whatever lock the
transaction takes on that row later** — asking for less turns the later
statement into a lock upgrade, and two concurrent requests both holding the
weaker lock deadlock trying to upgrade. `FOR KEY SHARE` is deliberately not
offered: it doesn't conflict with the toggle's own `FOR NO KEY UPDATE`, so a
gate holding it could read a flag that then changes under it. **Global lock
order stays advisory → `users` → `servers` → children**, unchanged by this
package; `LockGateTx` takes `users` then `servers` itself so a caller can't
interleave them. **Hazard handed to #3454, closed by its A-8 placement rule (keep `GuardTx`, gate after it, never upgrade the users lock):** a family that
takes the actor's `users` row at `FOR KEY SHARE` (an FK check can do this
implicitly) and only gates *after* its `servers` lock performs a `KEY SHARE →
SHARE` upgrade on `users` while holding `servers` — that can cycle against
`DeleteServer`'s `users FOR NO KEY UPDATE` → `servers FOR UPDATE` order,
because `FOR NO KEY UPDATE` is compatible with `KEY SHARE` and gets granted
first. Gate before taking the `servers` lock, not after.

**`internal/authoritycontract` (leaf package; stdlib + `github.com/google/uuid` only).** The account-key authority's frozen protocol-v1 contract (#2553 design of record §2, §3.1). It must never import `database/sql` — `authorityrelay` imports it — nor `internal/age`, whose closure pulls gin, go-redis, lib/pq and nats; `imports_test.go` pins both with `go list`. Every layout is a table of `field{name, kind, pointer}` rows decoded by one strict EN5 path; never add a hand-written parser or a `json.Marshal` call. Nested-message positions are opaque `b≤N` at the codec layer: decode the inner message where it is used. `testdata/vectors.json` is written only by `scripts/authority-vectors/gen.mjs` (private), and CI verifies it and never regenerates it.

**Amended `LockSubjectTx` placement contract** (`stepup/subject.go`): the
transaction MUST be READ COMMITTED; the lock MUST precede any statement whose
correctness depends on its verdict and any domain-parent row lock (`servers`,
`channels`, `dm_conversations`) — the one exception being a family that
already holds the actor's `users` row at the requested strength or stronger,
where re-taking it adds no edge (`rbac.withAuthorityCapture` is that case);
advisory locks and `SET LOCAL lock_timeout` may precede it freely.

**MFA hooks (`internal/mfa`).** Four sites bump the user's permission
generation, and none bumps before its commit or on an error that precedes it:
`withMFAFactorWriteTx` (covers TOTP re-enroll/confirm, WebAuthn register,
flag resyncs), `TOTPVerifySetup`'s autocommit enable `UPDATE`, `TOTPDisable`,
`WebAuthnDeleteCredential`. All four call the one helper,
`h.invalidatePermissionState`, so gain and loss log an identical line (I7) and
run detached via `context.WithoutCancel`. **The three transaction sites
(`withMFAFactorWriteTx`, `TOTPDisable`, `WebAuthnDeleteCredential`) bump after
`tx.Commit()` returns *whatever it returns*, not only on a clean commit** (plan
correction C-3) — a `Commit()` error doesn't prove a rollback, since the server
can commit and lose the client's acknowledgement, and skipping the bump on that
ambiguity would leave a removed factor's dangerous bits cached until the
5-minute TTL. An extra bump on a true rollback costs one cache miss; that
asymmetry is why the rule tightens toward bumping rather than away from it.
`TestInvalidatePermissionState_CommitErrorStillBumps` forces the `Commit()`
failure with a deferred constraint trigger. A static test in `internal/mfa`
(`p1_writer_pin_test.go`) parses every non-test `.go` file for a write to
`user_mfa_totp.enabled`/`.confirmed` or an insert/delete on either factor
table and attributes each to the hooked function that owns it — a write inside
a `withMFAFactorWriteTx` closure belongs to that function, and a write in an
unhooked helper belongs to its callers — then asserts that set is exactly these
four and that each calls the hook. A future factor writer that skips the
wrapper fails the build instead of leaving stale dangerous bits cached
silently. The router wires
`mfaHandler.SetPermissionInvalidator(rbacResolver)` and
`requirePermissionInvalidatorWired` fatal-exits at boot if it wasn't called —
interrogating the **handler**, never a nil check on the resolver value (which
is never nil), because only the handler can see a deleted setter call.
`internal/servers` carries the equivalent `SetMFAVerifier`/`HasMFAVerifier`
pair and boot guard for the toggle's own OFF confirmation.

**Permission-change announcements (#3456).** Two WebSocket events tell the
desktop to re-read what it shows. Neither carries a permission value: the server
stays authoritative on every request, so a lost event costs a stale display until
the next read and nothing else.

- **`server_permissions_changed`**, `{"type":"server_permissions_changed","data":{"server_id":"<uuid>"}}`,
  goes to every subscriber of the server. `refreshMFAEnforcementViews`
  (`internal/servers/mfa_enforcement.go`) replaces `bumpMFAEnforcementGeneration` at
  both of its call sites: after a clean commit that changed the value, and after
  a `Commit()` error whenever the `UPDATE` ran (the lost-ack rule above). So a
  changed PUT sends one event, a lost ack sends one, and a no-op or a refusal
  sends none. The id is re-rendered from its parsed form, so the wire always
  carries the canonical lowercase spelling. A nil hub is skipped; only tests build
  the handler without one.
- **`permissions_changed`**, exactly `{"type":"permissions_changed","data":{}}`,
  goes to the actor's own connected clients. `mfa.PermissionChangeNotifier` is
  declared at the consumer, in `internal/mfa`, beside `PermissionInvalidator`;
  `internal/api` adapts the hub to it (`permission_change_notifier.go`), so `mfa`
  never imports `websocket`. `invalidatePermissionState` calls it after the bump
  attempt, so each of the four hook sites sends one event, and so does any other
  path that reaches `withMFAFactorWriteTx` (the `GET /mfa/status` flag resync, the
  email/SMS verify). It is identical for a gained and a lost factor. `data` is a
  non-nil empty map: `OutgoingMessage.Data` has no `omitempty`, so a nil map would
  go out as `"data":null`, which `PermissionsChangedSchema` rejects.
- **The bump runs BEFORE the event, in both emitters, and the event follows
  whatever the bump returned.** A client that re-read on the event before the bump
  could be served, or could recompute and cache, the value the bump is about to
  retire. A failed bump has already retried and fallen back inside the resolver,
  so the re-read is still the best the client can do.
- **Both use the Context broadcast form** (`BroadcastToServerContext`,
  `BroadcastToUserContext`) under a 1 s `context.WithTimeout(context.WithoutCancel(ctx))`.
  Both run on the request goroutine after the commit, so an unbounded send into a
  saturated hub queue would hang a request whose change has already committed.
  `WithoutCancel` keeps a client that hangs up from cancelling the send. One
  second, not rbac's ten: rbac's events are revocations, and these only ask for a
  re-read.
- **A failure logs exactly one line**, `"Failed to announce a permission change"`
  with `failure_class=perm_change_broadcast` and no identifier, no error text and
  no direction, byte-identical from both emitters (I7,
  `observability.md` principle 7). A user id, a server id or
  a gain/loss word there would turn the line into an enrollment oracle.
- **Boot guard.** `requirePermissionInvalidatorWired` has a second leg:
  `HasPermissionChangeNotifier` fatal-exits at boot if `SetPermissionChangeNotifier`
  was never called, interrogating the **handler** for the reason given above. An
  unwired notifier is silent otherwise: the desktop never learns to re-read.
- **Single-replica hub assumption.** `BroadcastTo*Context` is an in-process channel
  send. It reaches only the clients connected to the replica that served the
  request, and there is no NATS or Redis relay for it. That is correct while the
  control plane is a singleton. A second replica needs a relay for both events
  first, or members connected to the other replica keep a stale display until
  their next read.
- **F12: the uniform event reveals THAT a flip happened.** Every subscriber of the
  server receives `server_permissions_changed` for a flip, including members who
  may not know the setting exists. The frame names no direction and no enrollment
  and is identical for ON and OFF, but it does tell a connected member that
  something changed. That was accepted (L7). The untouched `updated_at` (step 7)
  is therefore not proof that a flip is invisible to members; do not "fix" the
  difference in either direction.

**`ListServers` and the role-guard pre-check also carry the mask, and the
pre-check MUST (#2721 F3).** `ListServers` adds the flag to its `SELECT` and,
**only if some row enforces**, makes one `rbac.MaskFor` call and applies it to
each enforcing row in Go — never mirrored as SQL arithmetic. A P1 read error
there masks as if unenrolled (`failure_class=list_servers_mfa_state_unavailable`)
rather than failing the whole list. `role_guard.go`'s `roleGuardSelect` reads
`enforce_mfa_dangerous_actions` alongside `owner_id`, in the same **unlocked**
subselect shape, because both are `servers`-table facts the guard *reads* to
decide, not rows it ever writes — locking either would manufacture a
`roles`↔`servers` lock edge that doesn't need to exist. The pre-check consumer
applies `MaskFor` then `Apply` to `ActorBasePermissions` for a non-owner before
the subset comparison. This is load-bearing, not belt-and-braces: without it,
an unenrolled non-owner's pre-check verdict disagrees with the in-transaction
verdict `ResolveServerPermissionsTx` produces (which *is* masked), so a
dangerous conferral passes the pre-check and is refused only after
`PrepareCapture` — which answers 403 below `presenceCaptureMaxChannels` and 500
above it, disclosing aggregate voice occupancy: the #2721 F3 oracle, reopened.
O-1 (`TestRoleMutationDenial_MFAMaskAppliesInPreCheck`) pins it. An error on the pre-check's `MaskFor` call falls
through to the transaction, following the pre-check's existing non-authoritative
rule — it may only ever save work, never grant it.

**The endpoints (`internal/servers/mfa_enforcement.go`).** `GET`/`PUT
/api/v1/servers/:id/mfa-enforcement` is the **only** place the flag reaches the
wire (design L1) — `models.Server`, every server response and `server_updated`
never carry it, so no producer that reuses those shapes can leak it to a
member who may not read it. `GetMFAEnforcement` authorizes on raw bit 62
(`RawRolePermissions`), never the masked resolver — an unenrolled
Administrator must still be able to read the setting that masks them. `PUT`'s
step order is load-bearing end to end: (1) the fail-closed `stepup.Budget`
charge, only for an OFF that carries a code, **before** `BeginTx` — a
rollback must never refund a spent attempt. It is the MFA-settings routes'
budget (`stepup.MFASettingsBudgetPrefix`, key `stepup:mfa_settings:<uid>`), not
one of its own: both verify the same factor, and a budget per route hands a
stolen owner or Administrator session five more guesses per window for every
route added (PR #3464 review finding 8, CWE-307); (2) `BeginTx(ReadCommitted)` +
`SET LOCAL lock_timeout='3s'`; (3) `LockGateTx(users FOR SHARE, servers FOR NO
KEY UPDATE)` — this transaction never writes `users`, and the server lock
already matches the `UPDATE` that follows, so no lock is ever upgraded; (4)
**authorization runs before any MFA-derived answer** — owner or raw bit 62,
else the generic `RequirePermission`-identical 403, so an unauthorized caller
learns nothing about the setting's state (I7); (5) the gate runs on the
**requested** value, never a computed transition (L6) — `enabled=true` needs
`Subject.MFAEnabled` or refuses `mfa_enrollment_required`, `enabled=false`
always calls `mfaenforce.ConfirmTx`, **even when the setting is already OFF** —
a same-value request becomes a no-op only *after* passing this gate, and a
verified OFF-while-off clears the attempt budget exactly like any other
verified, committed OFF, because it *was* one; (6) only past that gate does an
unchanged value skip the `UPDATE`; (7) the `UPDATE` deliberately leaves
`updated_at` alone — every member reads it through `ListServers`, so touching
it would announce that a flip happened to people who may not know the setting
exists. Whenever the `UPDATE` ran, `refreshMFAEnforcementViews` (the bump, then
the `server_permissions_changed` announcement; see "Permission-change
announcements") runs after `Commit()` returns *whatever it returns* — the same C-3 rule as the MFA
hooks below, because a lost commit acknowledgement on an ON that applied would
otherwise serve every member's unmasked value for the cache TTL (Phase-4
`@red-team` finding F2, pinned by
`TestMFAEnforcement_LostCommitAckStillRefreshesPermissions`). Only after a
clean commit that changed the value is the Nightwatch success event sent, with
`AuthMethod=session` in **both** directions — the credential that authorized
the *toggle flip* is the caller's session, never the MFA factor that satisfied
the gate, so the reason code (`…Enabled`/`…Disabled`) is safe to expose (it
implies the actor is enrolled, not who they are) while `AuthMethod` names no
factor. **Every refusal on this route — including a 401 credential-epoch
mismatch from `LockGateTx`/`LockSubjectTx` — surfaces to Nightwatch as the same
one `RouteServerMFAEnforcement` fallback-denied `privileged_route_denied`
event**, never a distinguishable reason per refusal kind (I7).

**Five `failure_class` values — #3453 shipped four (spec §9 as originally written
was short one) and #3456 added `perm_change_broadcast`, described under
"Permission-change announcements".** `mfa_gate_lock` (an `IsLockConflict` 503: a 40P01 should never
occur under the stated lock order, while a 55P03 means a row lock was held
past the 3 s `lock_timeout`), `perm_generation_bump` (a bump/fallback that
still failed), `list_servers_mfa_state_unavailable` (a `ListServers` P1 read
error), and **`mfa_enforcement_internal`**, logged by the endpoints on any 500
from the toggle route, with the stage named in the wrapped cause, plus
**`perm_change_broadcast`** (#3456: a permission-change event the hub could not take
in time). None of the
five, and nothing else on this control, may log `enrolled`, `enforcing`,
`masked`, gain/loss direction, the method list, or any part of `mfa_code` (I7,
`observability.md` principle 7).

**Local curl recipe** (replace `$TOKEN`/`$SERVER_ID`; `jq` optional):

```bash
# Read the setting (owner or raw-bit Administrator only)
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/v1/servers/$SERVER_ID/mfa-enforcement" | jq

# Turn it ON (needs the caller's own MFA already enrolled)
curl -s -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"enabled": true}' \
  "http://localhost:8080/api/v1/servers/$SERVER_ID/mfa-enforcement" | jq

# Turn it OFF (always needs an inline confirmation: a TOTP code, a backup code, or a
# WebAuthn inline token minted for THIS route's purpose)
curl -s -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"enabled": false, "mfa_code": "123456"}' \
  "http://localhost:8080/api/v1/servers/$SERVER_ID/mfa-enforcement" | jq

# The WebAuthn form: begin names the purpose, the browser (navigator.credentials.get)
# answers the options, and finish returns an mfa_token usable ONLY on the toggle's OFF.
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"purpose": "servers.mfa_enforcement_disable"}' \
  "http://localhost:8080/api/v1/mfa/webauthn/verify-inline/begin" | jq
```

## Dangerous-action gates (#3454)

On a server with `enforce_mfa_dangerous_actions` on, each of 15 routes asks the actor for an inline MFA confirmation inside the route's own transaction; on a server without it, a request carrying no `mfa_code` is byte-identical to before. Full design and every ruling: the #3454 spec (its "Amendments after #3455 merged" A-1 to A-12 win over older text). The primitives are `internal/mfaenforce/dangerous.go` and `internal/stepup/grace.go`; each family's wiring is its package's `dangerous_gates.go` (`role_gates.go` in `rbac`, `moderation_gate.go` in `members`, `softlock.go`/`purge.go` in `messages`).

**The sequence every gated route follows.** (1) Before `BeginTx`: `stepup.Charge` (the budget, only when `mfa_code` is non-empty, whatever the server's setting, C6) and one pipelined `GraceStore.Read`. These are the ONLY Redis calls the gate makes: none runs inside a gate transaction, because a Redis round trip would pin a pooled connection while `users` and `servers` are held (A-2, A-9). (2) In the transaction: `LockGateTx`, then the route's OWN permission verdict (I7: nothing MFA-derived before it), then prior-state reads, then `mfaenforce.Require(ctx, tx, gate, Confirm{...}, fires)`, then the write. (3) After COMMIT: `mfaenforce.Settle`, on `stepup.SettleContext`. `Require` returns `Unconfirmed` (not enforcing, or the predicate did not fire), `GraceCovered` (a valid pre-read grace, no verifier call), or `Verified`; only `Verified` clears the budget and grants a grace, so a grace never slides. A code sent beside a valid grace is ignored and its charge stands.

**Invariants a future edit must not break.**
- **I-TX.** The flag check and the write share one READ COMMITTED transaction, check before write. A refusal writes nothing: no row, no object, no audit row.
- **I-ORDER.** advisory, then `users`, then `servers`, then children. `LockGateTx` is the first row lock, except in a `withAuthorityCapture` family, which already holds the actor `FOR NO KEY UPDATE` and `servers FOR UPDATE` (the gate re-takes both at the same strength, A-7). **Never request a users lock stronger than one the transaction already holds on the actor**: two concurrent holders deadlock on a `SHARE` to `NO KEY UPDATE` upgrade (A-8). `ServerLock` must be at least as strong as any later lock on the row.
- **Keep every existing `credepoch.GuardTx` and its #3508 classification; `LockGateTx` goes after it** (A-8). Its own epoch and missing-row arms are unreachable backstops there. A `ErrServerNotFound` maps to the route's existing vanished-server answer, which #3508 made route-specific.
- **I-RETRY.** In a retry wrapper the gate goes inside `write`. Since A-1 every factor is spent on the consumer's transaction, so a rolled-back attempt restores it and a retry re-verifies; a breach costs one extra verification, never a lost token. The forced-retry tests are regression pins. The budget is charged once per request, outside the loop.
- **I-CHANNEL-SCOPE.** A channel-keyed route resolves `server_id` with an unlocked read, calls `LockGateTx`, then locks or writes the channel with `AND server_id = $2`.
- **I-BOUND applies to NEW gate transactions only** (A-12.2). `mfaenforce.WithGateTx` sets `lock_timeout = '3s'`, and the uploads add `idle_in_transaction_session_timeout = '10s'` and a 5 s PutObject context. It serves only UpdateServer and the two uploads. Purge admission, ban, kick, DeleteChannel, expiration and both override upserts set no `lock_timeout` of their own, and CreateRole, UpdateRole and DeleteRole keep the role guard's `applyGuardLockTimeout` (3 s).
- **I-BODY.** Refusal bodies come only from the seam (`stepup.Error`): `mfa_required` with `methods`, `Invalid MFA code`, `mfa_enrollment_required`, plus the budget 429 (`step_up_budget_exhausted`), the budget 503 (`step_up_budget_unavailable`) and the lock 503 (`lock_conflict`, `Retry-After: 1`). `mfaenforce.WriteError` writes them; D1 never answers `step_up_unavailable`, an own-rule flag.
- **`mfaenforce.IsGateError` answers any 40P01 or 55P03 in a gated route's transaction with the 503 `lock_conflict` body** (A-12.3), including the route's own statements and a conflict PostgreSQL reports at COMMIT (a refused COMMIT is a rollback, not an ambiguous outcome; `WithGateTx` wraps the commit error, the channel expiration route checks `IsLockConflict` before its ambiguous-commit 503, and CreateRole marks it `errRoleCommitLock`): a deadlock that was a 500 is a retryable 503 now. The moderation routes decide per request, because their classifier serves gated and ungated transactions alike: a ban or a kick that purges answers any lock conflict this way (`writeGatedLockConflict`), while a plain kick keeps its 500 byte-identical (I-UNGATED). The role-row guard's own lock timeout keeps its 500, because `rbac` marks only the gate's lock (`errRoleGateLock`). Routes that keep their own 500 call `IsGateError` first and write their own body for the rest.
- **I-OBS.** No log, counter or error class discriminates enrolled, enforcing, confirmed or grace-covered (principle 7 of `observability.md`). Fixed `failure_class` values only: `mfa_gate_lock`, `mfa_gate_failed`, `mfa_gate_budget_clear_failed`, and the grace store's. No migration, no metric.
- **I-ID.** The owner and a raw Administrator are gated too. The owner bypasses permission checks, never the gate.

**The grace window (D-2, A-9, A-10).** A confirmed, committed action grants `stepup:grace:<uid>:<sid>:<serverID>:<bit>` for 10 minutes, stamped with the credential epoch, the actor's and the server's permission generations (`permgen`) and a strength. It is read BEFORE the transaction and judged under the locks: every stamp must match, the purpose must be `graceEligible`, and the strength must satisfy the rule (the server rule accepts only `mfa`). A Redis error or a malformed value is no grace, so the actor is prompted. **Always fresh, never grace-eligible:** `servers.delete`, `servers.mfa_enforcement_disable`, `roles.create`, `roles.update` and both override upserts (their prior-state predicate always demands a factor when it fires). `Grant` seeds an absent permission generation with `SET NX` and grants ONLY when its own `SET NX` created every key it needed; a stamp is never an absent value. A key that already exists means something wrote that generation after the read (an MFA change or an enforcement flip bumping it after the confirmation committed, or the cache seeding it), and `Grant` then grants nothing. Never re-read and stamp the stored value: that folds a post-confirmation bump into the grace (Codex review of #3454). Never seed in `Read` either: `Read` runs on every gated request, and a toggle-off request with no code must write nothing to Redis (`TestPurgeD1Gate_ToggleOffByteIdentical`). A settle that grants TWO graces (the composed purge's soft-lock and D1 graces) passes the second read through `AdoptGenerations(first stamped read)`, because the first grant's `SET NX` created the shared generation and the second's would otherwise refuse.

**Wire contract (A-6).** `mfa_code` is a plain `binding:"max=256"` JSON field on UpdateServer, CreateRole, UpdateRole, both override upserts, ban, kick and the expiration PATCH (on the channel-only wrapper `channelExpirationRequest`, so the DM route keeps its closed schema), and a multipart field on the two uploads. DeleteChannel, DeleteRole and DeleteServer read an OPTIONAL body through `stepup.ReadOptionalStepUp`: empty means no code, and `null` or anything but one object is a 400. The purge routes already bind `stepup.Fields`. `step_up_token` and `current_password` are never read by a D1 gate. **Client contract (C6, binds the desktop):** send `mfa_code` only on a retry after a 403 `mfa_required`; a request that carries one is charged to the budget even on a server that does not enforce.

**Step-up site census (15 routes, one purpose each; the two purge purposes are #3455's, reused).** "Fires" is the gate's predicate; every route also needs the route's own permission first.

| Route | Purpose | Fires | Transaction |
|---|---|---|---|
| `PATCH /servers/:id` | `servers.update` | always | new, `WithGateTx` |
| `DELETE /servers/:id` | `servers.delete` | always, fresh | existing main transaction, actor added to the sorted candidate set |
| `POST /media/upload/server-icon`, `…/server-banner` | `media.server_icon_upload`, `media.server_banner_upload` | always | new, `WithGateTx` held across PutObject, `HoldsExternalWrite` |
| `POST /servers/:id/bans/:user_id` | `members.ban` | always | existing ban transaction; the gate replaces the owner `FOR UPDATE` |
| `DELETE /servers/:id/members/:user_id` | `members.kick_purge` | only when `purge_messages` and not self | existing removal transaction |
| `DELETE /channels/:id` | `channels.delete` | always | inside `deleteChannelTx`, after `GuardTx` |
| `PATCH /channels/:id/expiration` | `channels.expiration_shorten` | the new window is finite and (no prior window, or shorter, or `retroactive: apply`), or `include_pinned` turns on (prior effective false, read under the channel lock, #3459 D-B) | existing, after `GuardTx` |
| `DELETE /channels/:id/messages`, `DELETE /servers/:id/messages` | `messages.channel_purge`, `messages.server_purge` | the plan has a spec whose author is not the actor | the purge engine's `Plan.Admit`, composed with the self-purge soft-lock |
| `POST /servers/:id/roles` | `roles.create` | `new & D != 0`, fresh | existing, after `GuardTx` and `applyGuardLockTimeout` |
| `PATCH /servers/:id/roles/:role_id` | `roles.update` | `(new &^ prior) & D != 0`, only when `permissions` is sent, fresh | inside `write` |
| `DELETE /servers/:id/roles/:role_id` | `roles.delete` | always | inside `write` |
| `PUT /channels/:id/overrides`, `PUT /categories/:id/overrides` | `overrides.channel_upsert`, `overrides.category_upsert` | `(new_allow &^ prior_allow) & D != 0`; the category upsert also when its sync copy would newly give any target a D bit on any synced child (A-12.17), fresh | inside the authority-capture callback (channel) or the wrapper's `write` (category) |

**Pinned expiry on the expiration route (#3459 D-B).** Turning `include_pinned` on is gated like a shortening, because it makes every kept pin deletable and deletes the ones already due before the PATCH answers. `expirationGateFires` is `shortensRetention(...) || enablesPinnedExpiry(locked.includePinned, request.IncludePinned)`: `include_pinned: true` over a prior EFFECTIVE value of false (E = window set AND pins expire, read by `lockChannelExpirationScopeTx` in the same `FOR NO KEY UPDATE` statement that reads the prior window, so a toggle-off landing between an earlier read and the lock cannot turn a gated false-to-true into an ungated true-to-true). Turning it off, omitting it, or re-affirming true never fires. The purpose stays `channels.expiration_shorten`, there is no new refusal reason (so no #3456 lockstep), and `expirationGateMayFire` (`request.WindowSeconds != nil || request.IncludePinned != nil`) decides whether the pre-transaction grace read is worth a Redis call; `Validate` already requires a window on every `set`, so the second operand adds no request today and exists so a later relaxation cannot skip the read. **The DM route stays ungated** (no D1 gate exists for DMs). The deletion of due pins runs after the gate settles and the broadcast, synchronously on a detached context bounded by 10 s and by what is left of the route's 12 s response budget (`expiration.ContextExpirer`, `expiration.ResponseBudget`, shared with the backfill resume so the PATCH still answers inside the server's 15 s `WriteTimeout`); see the `message_purges` entry in [`migrations.md`](migrations.md) for the audit row it writes. **Two deliberate non-gates (developer decisions, 2026-10-09):** turning pins on stays grace-eligible under `channels.expiration_shorten` (a recent MFA confirmation in the same session covers it), and UNPIN is not gated at all: under a keep-pins policy, unpinning a kept pin whose held stamp has passed makes it due, and any member holding `PermPinMessages` (every member, by default) can do that to another author's pin. Both were raised by the #3459 Phase-4 red-team and accepted; do not "fix" either without a new decision.

`D` is `PermAdministrator | PermManageDevResources | PermManageCryptoRotation` (`rbac/role_gates.go`). Removing a deny and AssignRole stay ungated (D6); the category-sync override copy stays ungated when a channel is created in or moved into a synced category (C11), but a category upsert whose copy would newly confer a D bit on a synced child asks for the factor itself (A-12.17, developer decision 2026-10-01). The prior override row is read `FOR NO KEY UPDATE`, not `FOR UPDATE` (A-12.6), and only when the new allow carries a bit in D; a missing row is prior 0. Both upserts answer RS5 at server scope, because their ManageChannels denials are server-scoped, and the category upsert's forced-retry test pins I-RETRY.

**Purge (A-3, A-12.4).** The D1 confirmation is part of `purge.Plan.Admit`, composed with the self-purge soft-lock's admission (see § "Delete-rate soft-lock"): one lock, one decision, at most one verification, at most one budget charge. The admission's authority refusal answers the route's 403; a batch-time refusal after admission with 0 deleted keeps its 500 (`purge_authority_race_test.go`), because the purge has started and left a recovery audit row. `Plan.Guard` rechecks `enforce_mfa_dangerous_actions FOR SHARE` per batch and refuses only a spec whose provenance is `unconfirmed`; a self spec is `exempt`. Ban and kick hand their own provenance to `purgeServerCore` through `PurgeUserServerMessages`.

**RS4 (owner conferral).** On an enforcing server an owner with no inline factor who assigns a role is held to `MFAMask{Enforcing: true}.Apply(OwnerPermissions)` instead of bypassing the subset check; an enrolled owner keeps the bypass. **It refuses any bit missing from that masked set, not only D** (`maskedOwnerConferral`, `rbac/role_gates.go`), in both `evaluateRoleGuard` and the pooled pre-check, which must agree or F3's occupancy oracle reopens for the owner. **What it tests is the whole conferral, not `roles.permissions` alone:** the role's bitfield OR every ALLOW bit its same-server channel and category overrides carry (`roleOverrideAllowsQuery`). A `permissions = 0` role with a dangerous override passed the bitfield-only check, and the resolver ORs a channel override into its holder's permissions while the category sync copies a category override onto synced children with no gate (C11). That was a Codex security finding on this PR; `TestRoleGate_RS4CountsOverrideAllows` and `TestRoleGate_RS4OverridePreCheckAndGuardEachRefuse` pin both copies. The override read takes no lock, because writing a role override needs `ManageChannels`, which the mask withholds from the very actor RS4 holds. The non-owner subset check still reads the bitfield only; that is #2723, unchanged here. AssignRole's own gate is not added (D6).

**RS5 (`rbac.EnrollmentDenial`, A-11).** `EnrollmentDenial(ctx, q, serverID, channelID, actorID, requiredBit)` decides whether a denial the caller ALREADY made is one an inline factor would lift: it returns `mfa_enrollment_required` only when the server enforces, the actor has no inline factor, the channel is in the server, and the actor is the owner or holds the bit raw (Administrator counts). Otherwise it returns nil and the route writes its existing 403. A bit the mask never withholds returns nil before any statement; the shape is constant per scope (3 statements at server scope, 5 at channel scope). **Pass the denial's own scope** (A-12.1): empty at a server-scoped denial, the channel at a channel-scoped one. Call it only after the denial, on the querier the denial came from. CreateRole's escalation comparison is never the masked denial and carries none; its stale-cache residual is the pre-existing 5-minute `HasPermission` TTL. `GET /servers/:id/permissions` also reports `mfa_restricted` (server scope only, `omitempty`, about the caller alone).

**Wiring and capability.** `requireDangerousActionGatesWired` (extracted from `NewRouter`) interrogates the channels, members, rbac and media HANDLERS (`HasMFAVerifier`, `HasRedis`; media only when it exists) and fatal-exits on a gap; servers and messages keep their own guards. `features.mfaEnforcedDangerousActions` in `GET /server/capabilities` is set from the SAME predicate (`dangerousActionGateGap`), is not `omitempty`, and defaults false: a client must not let an owner turn enforcement ON against a server that cannot gate (C5, X16).

**Residual (C7), stated so it is not rediscovered.** A Redis outage is a total D1 outage on enforcing servers for any request that carries a code: the budget is fail-closed (503 `step_up_budget_unavailable`), because skipping it would allow unbounded guessing of a 6-digit TOTP on the most dangerous routes. Moderation, a ban during a raid included, is unavailable on enforcing servers while Redis is down. A request with no code on an enforcing server is still the plain 403 `mfa_required`. The grace read fails to "no grace". Also open: role, membership and override writers still invalidate the permission cache by deletion, so a grace and a cached permission can outlive a revocation by up to their TTLs (pre-existing).

## Delete-rate soft-lock (#3455)

Message deletes past a burst threshold need a step-up confirmation or a wait.
Full design and the panel's contradiction ledger: the #3455 spec
(its "Developer decisions" block, D-1/D-2/D-3, supersedes the panel text). Routes
covered: `DELETE /api/v1/messages/:id`, `DELETE /api/v1/dm/conversations/:id/messages/:message_id`,
and the channel and server self-purge (`DELETE /channels/:id/messages`,
`DELETE /servers/:id/messages`). The DM purge is unchanged: it already
requires password plus MFA whenever the own rule applies, so it is not counted
twice. A ban or kick purge (`purgeServerCore`) never reaches the soft-lock.

**What it bounds, verbatim.** The burst tier bounds bursts that straddle the
route limiter's fixed 60 s window, where a burst can reach 20. A paced script
is bounded at 100 unconfirmed deletes per user per 24 h; the route limiter
(`RateLimitByUser(10, 1m)`, unchanged) remains the sustained-rate bound below
that. Two more properties: it fails closed for population users while Redis
refuses writes (the limiter fails open then), and it is defense in depth if
the limiter is ever loosened. Do not describe it as slow-purge protection.
A verified confirmation `Reset`s the day key as well as the burst key, so the
day tier bounds unconfirmed deletes per confirmation, not per calendar 24 h:
every further hundred costs a real factor, spent against the shared step-up
budget. That is the design (D-2), not a gap.

**The counter (`stepup/softlock.go`).** `DeleteSoftLockThreshold` 15 per
`DeleteSoftLockWindow` 30 s, per user per scope, plus
`DeleteSoftLockDayThreshold` 100 per `DeleteSoftLockDayWindow` 24 h per user
across ALL scopes. All four are `const`, never configuration. `HitN(ctx, uid,
scope, n)` is one `TxPipelined` MULTI covering both tiers (`INCRBY n`,
`EXPIRE … NX`, `PTTL` on each key); `Hit` is `HitN(…, 1)`. `Over` means
`burst > 15 || day > 100`, and `RetryAfter` is the larger `ceil(PTTL)` among the
tiers that are over (minimum 1 s; a `PTTL` at or below zero falls back to that
tier's own window). `Reset` is one `DEL` of the scope's burst key and the user's
day key. Keys are spelled only in `Key`/`DayKey`: `stepup:delete_softlock:<uid>:dm`,
`…:<uid>:server:<sid>` and `…:<uid>:day`; `ServerDeleteScope` takes a
`uuid.UUID`, so a raw `c.Param` is a compile error. The counter lives in
`stepup` because `mfaenforce`'s import allowlist forbids go-redis.

- **`EXPIRE NX`, never plain `EXPIRE`.** NX sets a TTL only on a key with none, so
  a refused hit never extends the window and a TTL-less key is repaired on the
  next hit. Replacing it with `Expire` must fail the non-extension test (H6).
- **Redis 7.0 floor.** `EXPIRE … NX` needs Redis 7. On Redis 6 the EXEC aborts
  and `HitN` answers the 503, never `Over=false`. Reasoned from Redis semantics,
  not measured. `docker-compose.yml` pins an exact Redis 8.10 release, and MFA
  login verify already needs 7. The floor matters for self-hosters on an external Redis.
- **A Redis failure is a 503, never a pass.** A nil client, any pipeline error and
  any per-command error return `ErrMsgDeleteGuardUnavailable` with `Cause`
  set. A counter that no-ops when Redis is down is the fail-open this exists to
  prevent.
- **Atomicity is what removes the check-then-act gap.** N concurrent requests at
  burst count 15 receive the distinct values 16…15+N.

**Population (D8, D-1): the strictest applicable rule wins.** Read before the
transaction, from statements the handler already runs, and consulted only after
the permission or author 403 (invariant I7). A member outside every rule is
never counted, never refused by the soft-lock, and costs no Redis call.

| Delete | Server rule applies when | Own rule applies when | Governs when both |
|---|---|---|---|
| Channel message, by its author | the server enforces MFA on dangerous actions | the author's `require_auth_before_purge` (a missing row is TRUE) | server |
| Channel message, by a moderator (not the author) | the server enforces | never | server |
| DM message (author only) | never | the author's `require_auth_before_purge` | own |
| Channel or server self-purge (resolved author is the actor) | the server enforces | the actor's setting | server |

The server rule confirms with inline MFA only, and an unenrolled actor gets
`mfa_enrollment_required` with `Retry-After`. The own rule alone confirms through
`stepup.VerifyOwnRuleTx`, the seam DM Clear uses (`dm/visibility.go`
`verifyClearStepUp` is now a wrapper over it): the inline MFA code when the
account has an inline factor, otherwise a password step-up token (§ "Step-up
tokens" below — the password itself never reaches these routes), and a
password token never substitutes for a missing code on an MFA account. Read shapes: the channel
route joins `servers` in its row read (`deleteTargetQuery`) and reads the actor's
setting in the same statement, unlocked, the `role_guard` precedent (the
authoritative re-read is under `LockGateTx`); the DM route adds
`COALESCE((SELECT require_auth_before_purge …), TRUE)` to its author read; the
self-purge does both plus a bounded `COUNT` in one round trip
(`selfPurgeSoftLockQuery`). Moderators on an enforcing server are in the
population, and a moderator who clears 16 or more messages in 30 s is prompted.
That is the threshold working, not a bug.

**Sequence.** (1) validate the id, (2) `stepup.ReadOptionalStepUp`, (3) row read,
(4) permission or author check, (5) outside the population: the ordinary path,
which carries #3142's credential-epoch fence, (6) `HitN`, and on error the 503,
(7) not over: the ordinary path, and any factor the body carried is **ignored**,
neither verified nor charged, (8) over with no factor: run the delete with the
confirmation hook, which refuses, (9) over with a factor: `Budget.Consume` **before
`BeginTx`**, then the confirmation in the delete's own transaction, then
**after commit and only if a factor verified**, `Reset` and `Budget.Clear`. Both
are best-effort with a Warn on failure, since a failed `Reset` only prompts
sooner, and both run on `stepup.SettleContext`: detached from the request, so a
client that hangs up just after its delete commits cannot cancel them, and
bounded by `stepup.SettleTimeout` (2 s), so a stalled Redis cannot hold them. Count before the limiter is impossible by construction: the route
limiter is middleware ahead of the handler, so a limiter 429 never reaches
`HitN`. `router_message_delete_softlock_test.go` drives the real router to pin
that order. Handler-seam tests mount the handlers without the limiter, the only
way to reach a 16th delete there.

**One confirmation helper per route (D-3), now composed (#3454 A-3, A-10).** The helper is `admitDeleteTx` in `messages/softlock.go` (channel delete through `confirmSoftLockTx`, and both purges through `selfPurgeAdmit`, as `purge.Plan.Admit`) and `confirmDMMessageDelete` in `dm/message_delete_softlock.go`. Do not fan the decision out. `admitDeleteTx` must be its transaction's first statement: it takes `LockGateTx(users FOR SHARE, servers FOR SHARE)` once (`lockSoftLockGateTx`), so the order is `users` then `servers` then the factor write then the caller's own locks. It chooses its rule from the flag as re-read under the `servers` lock, never from the unlocked read: turned ON in flight, the server rule governs and a password no longer passes; turned OFF in flight, the own rule governs when it applies, and only when no rule applies any more does the action proceed unverified (then no `Reset`, no `Budget.Clear`, and any budget charge stands). **On a purge it is also where the D1 gate confirms.** `E` is the flag under the lock, `D` means the plan has a spec whose author is not the actor: `E` and (`D` or over or recheck) is the server rule, once; `!E` and over is the own rule; otherwise unverified. It never calls `ConfirmTx` twice (`acceptTOTPStep` refuses a replayed TOTP step, and a token spends once). Before any confirmation it runs the plan's own batch guard over each D1 spec, **only when a rule applies** (A-12.4), so an actor who lost authority gets that guard's refusal (the route's 403) and never `mfa_required`. When `D` holds the refusal is D1's plain seam body, with no `delete_rate_limited` and no `Retry-After` (waiting lifts nothing); a plan with no D1 spec keeps #3455's decorated refusal. The budget is charged at most once per request: the soft-lock charges over the threshold when a factor is present, and D1's `Charge` only when `D` holds, `mfa_code` is non-empty and the soft-lock did not already charge.

**Grace (D-2, A-9, A-10).** Grace is honoured in more than the two helpers: it is honoured in `confirmServerRuleTx` (both callers, `confirmSoftLockTx`'s enforcing arm and `recheckSoftLockTx`), in `verifyOwnRuleTx` (own rule, channel routes), and in `confirmDMMessageDelete` (own rule only, after `LockSubjectTx`, before `VerifyOwnRuleTx`). Keys: the D1 key above, and `stepup:grace:<uid>:<sid>:<scope>:delete` with `<scope>` spelled by `stepup.SoftLockScope` (`dm` or `server:<sid>`), both spelled only in `stepup`. A purge both rules apply to needs BOTH graces to cover (`deleteAdmission.graceCovers`); each covers only its own scope. Each helper reports one of three outcomes: **verified** (resets the counters, clears the budget, grants grace), **grace-covered** (resets the counters, neither clears nor grants, so the window never slides) and **unverified** (settles nothing). Settlement runs on `SettleContext` in `settleSelfPurge` (after an admission that committed), the channel delete's settlement and `settleDMDeleteConfirmation`. The grace read runs only on a path that already calls Redis (over the threshold, or a D1 route), so the outside-population recheck reads none and a self-purge whose server turned enforcement on in flight is always prompted. `confirmed` is reset at the top of every attempt because `DeleteOne` retries on `ErrMembershipChanged`; the retried attempt re-verifies after the rollback, because TOTP steps, backup codes and step-up tokens (WebAuthn and password alike, #3509) are all spent on the transaction that rolled back. The invariant to keep: over the threshold, a delete commits only if a factor verified in the same transaction, a grace the governing rule accepts (judged under that transaction's locks) covered it, or no rule applies under the locks it took.

**Self-purge (`messages/purge.go`).** `gateSelfPurge` counts only delete specs
whose resolved author is the actor. It runs one `COUNT … LIMIT 101` (`selfPurgeCountCap`,
one past the day tier, since a larger count cannot change a verdict) and
`HitN(count)` before any batch. Any self-purge of more than 15 messages therefore
trips the burst tier. The count reads the ids it counts, and when no confirmation
governs the purge the actor's own specs carry them as `purge.DeleteSpec.OnlyIDs`,
so the batches delete only counted messages: the count and the batches are
separate reads, and a message sent between them would otherwise be deleted
uncounted. A zero count fences to nothing. Over the threshold, the confirmation
runs ONCE, as `purge.Plan.Admit`: the engine runs it first in the transaction
that writes the purge's `in_progress` audit row, because batches are separate
transactions and a TOTP code verifies once. A refusal rolls that admission back,
so nothing is purged and nothing is audited, and a factor is spent only by a
purge that was admitted: committed on its own, a confirmation spent the factor
even when the audit write then failed. Every error `Run` returns from admission
wraps `purge.ErrNotAdmitted`, with one exception: a failed admission COMMIT is
reconciled first (`reconcileAdmission`), by a 5 s read detached with
`context.WithoutCancel` of the audit row by the id the admission generated —
present means admitted and the batches run, absent means `ErrNotAdmitted`, and
a failed read is `purge.ErrAdmissionUnknown`, which is deliberately not
`ErrNotAdmitted`: the handlers answer their ordinary 500 and settle nothing, and
no batch runs. A COMMIT still being applied when that read runs reads as absent,
which refuses a purge whose audit row then commits — nothing is deleted. The
budget is cleared once an admission that
verified a factor committed, and `Reset` runs only after a purge that succeeded
and only when a confirmation ran.

**Outside the population, a self-purge still has an admission.** Its fence and
confirmation both hang off the unlocked population read, and the per-batch guard
never reads `enforce_mfa_dangerous_actions`, so an owner turning enforcement on
after that read would let the actor delete their whole matching history
uncounted and unconfirmed. `gateSelfPurge` therefore returns a `recheck`
confirmation for that case, and `selfPurgeAdmit` runs it as `recheckSoftLockTx`:
the same users → servers locks as `confirmSoftLockTx` (`lockSoftLockGateTx`).
Still not enforcing, it admits the purge unverified, as before; turned on, the
server rule governs (`confirmServerRuleTx`), so a request without a verified
inline factor gets the soft-lock refusal and nothing is purged or audited. It
does not re-read the own rule, which nobody but the actor can turn on. `runServerPurge` does not log a failed
admission on this route: `PurgeServer` answers it, and its `server_id` line
must not carry a soft-lock refusal (C7). `PurgeServer` composes `authorizeServerPurge` and `runServerPurge` itself
because the soft-lock sits between them, which `purgeServerCore` (ban/kick)
must never do. Purge bodies bind through `purgeRequest`, which carries the same
two fields and caps as the reader below, and `readPurgeBody` reads them the way
that reader does: the whole body, at most 4 KiB, as exactly one JSON object.
`ShouldBindJSON` stops at the end of the first value, so it never saw a second
one or anything past the cap.

**Body and purposes.** `stepup.ReadOptionalStepUp(c)` returns `stepup.Input{MFACode,
CurrentPassword}`. An empty or whitespace-only body means no step-up; oversize
(4 KiB), non-object JSON (`null` included), trailing data, a wrong type, an
`mfa_code` over 256 characters or a `current_password` over 1 KiB (counted in
bytes, because it bounds the Argon2id input) are `400 Invalid request body`.
Unknown fields are ignored and no `Content-Type` is required. #3454 reuses it.
The channel and server purge routes bind a larger struct through
`bindPurgeRequest`, and cap the whole body at the same 4 KiB with
`http.MaxBytesReader` before binding; it had been read unbounded until the
#3455 red-team pass.
Four purposes joined `allPurposes`: `messages.delete`, `dm.message_delete`,
`messages.channel_purge`, `messages.server_purge`, one per route so a WebAuthn
token minted for one is spendable on no other. (`dm.purge` and `dm.clear`
already existed.) The desktop mirrors them in `STEP_UP_PURPOSES`
(`components/Auth/stepUpPurpose.ts`), and `stepUpPurpose.parity.test.ts` reads
`stepup/purpose.go` and fails on a TS purpose Go does not have; `build.yml`'s
`desktop` paths-filter lists that Go file so a Go-only rename re-runs the desktop suite.

**Error classification, in this order** (`respondSoftLockError`,
`writeDMMessageDeleteError`). (1) `mfaenforce.IsLockConflict` → `mfaenforce.WriteBusy`
(503, `Retry-After: 1`, `lock_conflict: true`), **first**, because a 55P03 can
arrive inside a 500 `*stepup.Error`'s `Cause`. (2) `errors.As(*stepup.Error)`:
a 403 is copied (never mutated) and gains `delete_rate_limited: true` plus
`Retry-After: ceil(PTTL)`; every other status is written as is; a `Cause` is
logged first. The five soft-lock 403s are `mfa_required` + `methods`,
`Invalid MFA code`, `password_required`, `Invalid password` and
`mfa_enrollment_required`. (3) on the DM route, #3141's statuses: a
credential-epoch mismatch or block → 401 `Authentication required` through
`respondGuardTxError`; `dmblock.ErrUnavailable`/`ErrMembershipChanged` → 403
`dm_unavailable`; `errDMPurgeNotParticipant` → 403 not a participant;
`errDMPurgeScopeChanged` → 403 `You can only delete your own messages`. On the
channel delete the ordinary path's refusals never reach the classifier: #3142's
in-transaction guard (`authorizeMessageDeleteTx`) writes its own 401, 403 or
404 and returns `errMessageDeleteGuardRejected`. (4) `mfaenforce.ErrServerNotFound`
or `sql.ErrNoRows` → 404. (5) else 500.
The machine-readable flags are: `step_up_budget_exhausted` (budget 429),
`step_up_budget_unavailable` (budget 503), `lock_conflict` (lock-conflict 503),
`delete_rate_limited` (the 403s). The soft-lock-unavailable 503 has **no**
flag: `ErrMsgDeleteGuardUnavailable` with `Retry-After: 30`. Clients route by
flag or by the exact string, never by status alone (X11/X17).

**Logging (C7, a hard rule).** Only a population member can reach the lines that
only the soft-lock path reaches, so none of them carries `user_id`, a scope,
`server_id`, the enforcement flag, MFA methods, the governing rule or which rule
put the actor in the population. Each carries a fixed `failure_class`
(`delete_softlock_unavailable`, `delete_softlock_budget_unavailable`,
`delete_softlock_confirm_failed`, `delete_softlock_reset_failed`,
`delete_softlock_budget_clear_failed`, `delete_lock_conflict`) and the error.
The existing `Message deleted` Info line is unchanged and fires on every path.
**No metric, no Nightwatch verdict and no new `ReasonCode` on any soft-lock
path (C6, C12):** verdicts are last-write-wins, and marking only population
requests would split the security stream by a privacy setting (see
`observability.md` principle 7). The signal for write
refusal is the route limiter's own `rate_limit_backend_unavailable` verdict.

**Boot guard.** `NewRouter` wires `messagesHandler.SetMFAVerifier`/`SetRedis`;
`dm.Handler` takes both through its constructor. `requireMessageDeleteGuardWired`
(extracted from `NewRouter`, which sits at the S3776 ceiling) interrogates the
**handlers** through `HasMFAVerifier`/`HasRedis` and fatal-exits on any gap, for
the reason `requirePermissionInvalidatorWired` gives. It matters because an
unwired handler fails closed silently: every over-threshold delete a 500, every
population delete a 503. #3454 reuses it.

**Lock order after #3141/#3142 (the merge that re-homed this feature).** Both
delete transactions keep `main`'s authority rechecks and add the confirmation
at a fixed place. Channel delete: `confirmSoftLockTx` first when over the
threshold (users FOR SHARE → servers FOR SHARE on the server arm → the factor
write), then `authorizeMessageDeleteTx` — `lockChannelMessageMutationTx`
(`credepoch.GuardTx`, which re-takes the users row already held and adds no
edge; channels FOR SHARE; server_members FOR SHARE; in-transaction permissions;
the message FOR UPDATE), the author, and `PermManageAllMessages` for someone
else's message. The ordinary path is that guard alone; the credential-epoch
fence it carries is #3142's, so there is no separate normal-path fence. DM
delete: `dmblock.PrepareConversationTx` first (every participant plus the
actor, sorted, FOR SHARE, then the conversation FOR SHARE), then the fence in
the slot #3141 gives `credepoch.GuardTx` — `LockSubjectTx` + `VerifyOwnRuleTx`
on the confirmation path, which is the placement contract's exception because
the prefix already holds the actor's users row FOR SHARE — then the
participant FOR SHARE and the message FOR UPDATE. Never move the DM fence
ahead of the prefix: locking the actor alone before a sorted multi-user prefix
is the out-of-order edge `dmblock` exists to remove. Because only the prefix
returns `ErrMembershipChanged`, a retried DM attempt never reached the verifier.

**I7 after #3141.** `main` removed the DM route's pre-transaction author read;
this route keeps one unlocked statement (author plus
`require_auth_before_purge`) because the soft-lock counts before the
transaction opens, and a non-author must be refused before the count. The
transaction's recheck stays authoritative.

**Residuals, so they are not rediscovered.** A member who can delete can infer
the enforcement flag, because they see the prompt (H3; mitigated by ordering,
neutral copy and no discriminating telemetry). A missing `privacy_settings` row
reads as ON, so nearly every author deleting their own DM or channel message
is in the population, and while Redis refuses writes (MISCONF, a timeout) those
deletes are 503s (R2, see
`docs/runbooks/deploys.md`). A successful `Reset` discards concurrent in-flight
`INCR`s (RS4; bounded by in-flight requests, a just-verified factor and the
route limiter). `allkeys-lru` eviction resets a counter in the fail-open
direction (RS5); `Budget`, the MFA verify counter and `RateLimitByUser` share
the exposure. A flag flip between the pre-read and the transaction costs at most
one uncounted delete or one extra prompt. Ways around the soft-lock that remain:
edit-to-empty (bounded by the 10/min edit limiter) and `DeleteGroup` (one
action, not a rate). The counted unit is an authorized attempt, so a 404 or 500
inside the transaction still counts.

## Step-up tokens (#3509, migration 000162)

The own-rule routes — DM Clear, the channel and DM message deletes and the
channel and server self-purges, every route `stepup.VerifyOwnRuleTx` guards —
**never receive the account password.** A password-only account proves it at
`POST /api/v1/auth/step-up/password` (`stepup.PasswordTokenHandler`, mounted on
the AUTHENTICATED group under the `/auth` path, beside `/auth/ws-ticket`), which
returns `{step_up_token, expires_in: 60}`; the route takes `step_up_token`. The
three own-rule body readers share `stepup.Fields`: a body that carries
`current_password` **as a key, any value, null included** is a 400 whatever the
account or soft-lock position, so a stale client fails loudly instead of
looping, and `mfa_code` and `step_up_token` are each capped at 256 characters.
The hard and settings flows (DM purge, MFA settings, the purge fence, password
change, account deletion, OAuth consent, the MFA-enforcement toggle) still take
`current_password` and are untouched.
**No #3454 dangerous-action purpose is an own-rule purpose (A-5).** The 13 purposes it added (`channels.delete`, `servers.update`, `media.server_icon_upload`, `media.server_banner_upload`, `members.ban`, `members.kick_purge`, `roles.delete`, `roles.create`, `roles.update`, `channels.expiration_shorten`, `servers.delete`, `overrides.channel_upsert`, `overrides.category_upsert`) are in `allPurposes` but NOT in `ownRulePurposes`, so the password mint refuses every one of them before it looks at the password and no password token can be minted for a D1 route. A D1 gate reads only `mfa_code`; `step_up_token` serves the soft-lock's own rule. A D1 purge reuses `messages.channel_purge` and `messages.server_purge`, because it is the same route and request as the self-purge and one verification must not run under two purposes.

**The mint's order is the contract** — each refusal comes before the next step:
body (400) → `purpose` must be `Purpose.OwnRule()` (400 *before the password is
examined*) → `LoadSubject` P1: an account with inline MFA is a 403
`{error, mfa_required, mfa_methods}` *before verification*, so an MFA account
can never confirm these routes with a password → the password, through
`auth.Handler.VerifyPassword`, which shares `/login`'s email-keyed lockout (423
`error_code: account_locked`, CompleteLink's shape) → `MintToken`. A wrong
password is a 403 `Invalid password` (the seam's byte-exact copy) and also feeds
`middleware.RecordAuthFailure`, the per-IP ban; the route carries `/login`'s
`AuthBanCheck` and `RateLimitByIP(authFlowTestRateLimit(10), 15m)`. **It must
never become a cheaper password oracle than login**, which is why none of
those may be dropped. A verifier fault is logged with a fixed `failure_class`
and never its error, which can embed hash text; no line carries the password.
Its route guards (`stepUpPasswordRouteGuards`) are `AuthBanCheck` plus
**fail-closed** per-IP and per-user limits, 10 per 15 minutes each: a Redis
outage refuses the mint rather than leaving a live session free to drive
Argon2 guesses. An account with no usable password factor gets the route's own
NoFactors 400 (`stepup.OwnRuleCopy`) before verification. It emits /login's
event shapes on `securityevent.RouteStepUpPassword` — invalid_credentials on a
wrong password, account_locked on the 423, authentication_succeeded on a mint —
and the route's Nightwatch fallback is /login's, without an `auth_method`, so
a wrong password the handler failed to record is told apart from one it did.
**Deploy order: apply migration 000162 before the binary that mints**; a binary
ahead of the table answers every own-rule password confirmation and every
WebAuthn inline confirmation with a 500.
`auth.ErrInvalidCredentials` is the sentinel the mint tells a wrong password
from a fault by.

**The store (`stepup/token.go`).** `step_up_tokens` holds both kinds, told
apart by `factor` (`password` | `webauthn`); only `SHA-256(token)` is stored.
`MintToken` runs its own transaction: users row `FOR NO KEY UPDATE` (serializes
one user's mints, which is what makes the cap exact), the session epoch fenced
with `credepoch.MatchEpoch` (401 on a stale session), then delete the user's
expired rows, insert with the epoch read under the lock, and cap the user's
rows at `MaxLiveTokensPerUser` (16), oldest first. `SpendToken` is ONE
statement on the caller's querier — `DELETE … WHERE token_hash, user_id,
purpose, factor match AND expires_at > now() AND credential_epoch IS NOT
DISTINCT FROM (SELECT credential_epoch FROM users WHERE id = $2) RETURNING 1`.
The invariants and what breaks them: **a rollback restores the token** only
while the spend runs on the consumer's transaction (`VerifyCodeTx` passes its
`tx`; spending on the pool is the GETDEL defect this replaced); **a token for
another purpose or factor is refused and NOT consumed** only while both are
columns of the match (#3467); **a password change or credential revocation
strands every older token** only while the epoch comparison stays `IS NOT
DISTINCT FROM` (with `=`, a never-rotated account's NULL could never match).
**A token binds to the user and the credential epoch, not to a session**:
only an epoch rotation (password change, key reset, recovery) strands it, and
revoking one session or all of them without a rotation leaves an outstanding
token spendable by the user's other session for the rest of its 60 s. **The
rollback guarantee is the consumer's**, not the store's: it holds for every
consumer that spends through `VerifyCodeTx` or the own-rule seam on its own
transaction, while the sessions and ownership routes still verify WebAuthn
through `VerifyCode`, on the pool, so their token is spent before their own
work and a failure after it does not restore it. A self-purge spends in the
purge engine's admission transaction (`purge.Plan.Admit`), which commits with
the `in_progress` audit row before any batch runs: a batch that fails later
leaves the token spent and the purge resumable. **Every time** the store
stamps or compares is `clock_timestamp()`, never `now()` (the transaction's
start): with `now()`, a consumer that waited on a lock spent an expired token,
and a mint that waited behind other mints stamped its row older than theirs,
so the cap evicted the fresh row it was about to return.
Lock order is users → `step_up_tokens` for every writer; the hourly
`runCleanup` sweep (`SweepExpiredTokens`) uses `FOR UPDATE SKIP LOCKED`, so it
never waits on a request. It deletes at most 1000 rows per statement, the
longest-expired first, each statement its own commit, until a batch comes up
short or its context ends, and reports what it drained even on an error: a
backlog that outruns the context loses at most the batch in flight, never what
earlier batches deleted. `runCleanup` gives it its own 10 s inside the shared
30 s, so it cannot starve the tasks after it. Both own-rule NoFactors 400s —
the seam's password arm and the mint's — come from `noFactorsError` and carry
`step_up_unavailable: true` beside the unchanged copy, which is how a client
tells an account with nothing to confirm with from a malformed body's 400
(`StepUpUnavailableError` in the OpenAPI spec). The own-rule password arm answers a missing token
with the route's `password_required` copy and a token that matched nothing
with 403 `ErrMsgStepUpTokenInvalid` ("Your confirmation expired. Enter your
password again.") plus `password_required` and `step_up_token_invalid`. A
token in the body counts as a factor for the step-up budget, as a code does.

## Outbound-request SSRF egress guard

Any handler that issues an HTTP request to a host derived from — or reachable via redirect from — user input MUST guard egress, not just validate the initial URL. The KLIPY media proxy (`services/control-plane/internal/klipy/handlers.go`) is the reference implementation (#1361):

- **Dial-time IP denylist (`net.Dialer.Control`)** on a cloned transport — `Control` fires post-DNS, pre-connect, on the resolved IP, so it defends redirect-SSRF **and** DNS-rebinding with no TOCTOU window. Deny via a pure `netip.Addr` predicate (`Unmap()` first, then reject loopback / private / ULA / link-local / all multicast / unspecified / CGNAT `100.64.0.0/10` / deprecated IPv6 site-local `fec0::/10`). Keep the predicate a package function behind a test seam — `httptest` binds `127.0.0.1`, which the guard denies.
- **Per-hop host re-validation (`http.Client.CheckRedirect`)** re-applies the scheme + host allowlist on every redirect; the initial URL check covers only hop 0.
- Stdlib only (`net`, `net/netip`) — no third-party SSRF library (no-new-deps policy).

## Account authority HTTP server (`net/http`, not Gin)

`internal/authority/server` serves the account-key authority with stdlib `net/http`, not
Gin (DoR D-32): HTTP/1.1 only, with mutual TLS from `internal/authority/tlspin`. The Gin
conventions below do not apply there. Its own rules:

- **Routes are rows.** Each endpoint is one `route{}` in `routes()`: a Go 1.22 `ServeMux`
  method pattern, its log `op`, its deadline class (8 s for transactions, 4 s for reads) and
  its handler. Body caps live in the handler (`s.body(limit, …)`). No router library.
  `DisableGeneralOptionsHandler` is set, so `OPTIONS *` reaches the handler like any request.
- **Only the wrapper writes.** A handler returns a complete `store.Outcome` and never touches
  the `ResponseWriter`, so a panic, a late result or a deadline cannot leave a partial body.
  A panic or a late result becomes 503 `store`, except a late 503 `replication`, which passes
  through as a fresh envelope. A status outside 200–599 is also 503 `store`. A body that
  misses its deadline drops the connection unanswered. `http.ErrAbortHandler` is re-raised.
- **The peer check runs before the mux, on every request, uncached** (D-207). A revoked pin
  gets no reply and a closed connection. Do not move the check into a route or cache it. A
  request no route takes, `HEAD` included, is answered 404/405 `malformed` after the check.
- **Client authentication is the listener's, not `VerifyConnection`'s** (E1). The TLS config
  checks shape only (one certificate, P-384, exact EKU); the listener checks the pin and the
  uncached `revoked_keys` read after the handshake has proved possession, before the
  authenticated slot is taken. Do not put a database read back into `VerifyConnection`.
- **Never `InsecureSkipVerify`** under `internal/authority*` or `authorityclient`. `tlspin`'s
  grep test enforces it (AC14).
- **Logs** carry only DoR §3.6's fields: `op`, `outcome`, `code`, `request_id`, `latency_ms`,
  `count` on the aggregated `authority.tls` line, `complete` (0/1) on a completed
  `authority.purge` run, and `reason`, `step` (a closed `server.Step*` set) and `error` on the
  exit line. A fail-closed branch that would otherwise be silent logs its edge, never once per
  poll: `authority.role` (`code` `leader`/`schema`/`trust`), `authority.clock`, `authority.disk`.
  A recovered panic is one fixed `authority.panic` line whose `op` is the route or `tls`, never
  the value or a stack. A line cut short by serve's own stop is not logged. The exit line's error text passes through
  `exitText` (`cmd/account-authority/main.go`), the one filter that keeps paths, addresses,
  SQL and row values out; no other line carries an error's text. Never log a URL path (it can
  name an account), an account ID, a body, a key or a certificate.

Design of record §3.3; plan `docs/superpowers/plans/2026-10-15-3587-pr-b2-transport-binary-tick.md`.

## Gin Conventions
- `c.ShouldBindJSON(&req)` (returns error), NOT `c.BindJSON` (auto-aborts)
- For Gin JSON request bodies that require both a byte cap and strict full-document enforcement,
  merely wrapping a single-value decoder with `http.MaxBytesReader` is insufficient: the decoder
  may stop after its first valid JSON value without reading the oversized remainder. Those JSON
  handlers MUST apply `MaxBytesReader`, call
  `ShouldBindBodyWithJSON` so the complete bounded body is consumed, retrieve the cached `[]byte`
  from `gin.BodyBytesKey`, and require `json.Valid(body)`. Accept exactly one JSON document with
  optional trailing whitespace. Preserve error ordering: `*http.MaxBytesError` is 413 first;
  other bind errors, a second document, or non-whitespace trailing content are 400; semantic
  cardinality checks run only after strict body validation.
- **One raw-body exception: the Stripe webhook** (`internal/billing/webhook.go`). It wraps the body in
  `http.MaxBytesReader` (256 KiB, `webhookMaxBody`) and reads it with `io.ReadAll`, because Stripe's
  `Stripe-Signature` HMAC covers the exact bytes, and a bind-then-re-marshal would change them. An
  over-cap body is 413 before any verification. Nothing else may read a body this way: every other JSON
  handler follows the strict-body rule above.
- User ID: `c.GetString("user_id")` from auth middleware
- Always return after sending a response

## Linting
- golangci-lint v2: errcheck, gosec, staticcheck, govet, revive
- If golangci-lint would flag it, fix it before committing

## Dependency Hygiene

The transitive Go module graph in `services/control-plane` is refreshed by a weekly automated workflow:

- **Workflow:** `.github/workflows/go-transitive-flush.yml`
- **Cadence:** Tuesday 06:00 UTC. Dependabot's gomod ecosystem fires the same day (`day: tuesday` in `.github/dependabot.yml`, no `time:` pinned); timing-independence is structural via the contention guard, NOT the 06:00 UTC slot choice.
- **Operation:** `go get -u ./... && go mod tidy` in `services/control-plane/`
- **Output:** PR labeled `routine: go-transitive-hygiene`, opened only if go.mod/go.sum changed
- **CI re-trigger required (post-#1228 review):** the auto-generated PR is created by `GITHUB_TOKEN`. Per `peter-evans/create-pull-request`'s [documented limitation](https://github.com/peter-evans/create-pull-request/blob/main/docs/concepts-guidelines.md#triggering-further-workflow-runs), `pull_request` workflows do NOT auto-trigger on push for these PRs — so the transitive-flush PR appears in `/weekly-deps` with no CI run. Reviewers MUST push an empty commit (`git commit --allow-empty -m "chore: trigger ci"`) or close+reopen the PR before merging. Migrating to a PAT or GitHub App to fix this is a possible follow-up if the manual re-trigger becomes painful in practice.
- **Triage:** Processed in the following Wednesday's `/weekly-deps` review window (Phase 1 DISCOVER picks it up alongside Dependabot PRs). Default tier classification is **Tier C** (deep review); reclassified to Tier B if the diff is <5 modules AND contains no major-version crossings AND contains no security-related deps in the bump set.
- **Contention:** Workflow defers and exits success-skip if open Dependabot Go PRs exist against `services/control-plane/`. Deferral comments are added to those PRs with a dated marker.
- **Manual invocation:** `gh workflow run go-transitive-flush.yml` for ad-hoc CVE-driven flush between scheduled runs.
- **Triage ownership:** Same as Dependabot Go PRs — weekly review by repo admin via `/weekly-deps`. No explicit assignee per the convention that bot-authored PRs are routed through labels, not assignments.

**Bot-PR CI re-trigger pattern (documented once, #1436):** Workflows that open PRs via `peter-evans/create-pull-request` with `GITHUB_TOKEN` still inherit the anti-recursion limitation — `pull_request` workflows do not auto-trigger on those PRs; reviewers push an empty commit or close+reopen before merging. `go-transitive-flush.yml` remains in this category. `refresh-ioc-list.yml` was removed from it by #1490: it mints the Concord CI Bot App token, so refresh PR pushes are App-authored and PR CI starts naturally.

**Supply-chain IOC cadence (#1436/#1490):** `refresh-ioc-list.yml` runs DAILY (09:00 UTC, was Tuesday-weekly) — threat-intel freshness is an active-defense posture decoupled from the weekly-deps window. The workflow is data-deterministic, opens no PR for unchanged CSV data, and requests auto-merge only after the IOC refresh guard verifies provenance, diff size, sorting/dedupe, and lockfile-clean additions. Guard failures ping `@Concord-Voice/security` and remain human-reviewed.

Founding incident: #1212 (2026-05-27 audit found 164 indirect Go modules with available updates in `services/control-plane` accumulating between direct-dep bumps).
