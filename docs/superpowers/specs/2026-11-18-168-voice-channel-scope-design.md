# Security #168 Voice Moderation Channel-Scope Design

Status: final spec (design panel reconciled, 2026-11-18). Base: `6ba00d24`.
Scope: `services/control-plane` only. No migration, no config, no client protocol change.
Findings covered: VULN-102, VULN-103, VULN-197, VULN-106 (Security #168), plus the channel-scope and dead-route parts of Security #144.

## Intent

Voice moderation authority is decided at server scope only, and the move handler decides it after it has already answered questions about voice state. Four consequences:

- A Move Members holder can write a temporary `VIEW|CONNECT|SPEAK` override on a channel they cannot access themselves, and the write leaves no audit row when the target is lower-ranked.
- A verified account that is not a member of the server gets distinct 400/409 answers from the move route and takes the exclusive per-server visibility lock while doing so. A self-move has no membership check at all.
- A channel DENY of Mute, Deafen or Move Members is ignored by voice session actions.
- DM soft-mute has no stated authorization rule.

After this change every `/servers/:id/voice/:userId/*` route authorizes the actor before it reads voice state, session actions and moves are additionally decided at the channel they touch, the mover must hold what a temporary grant confers, and that decision is repeated in the transaction that writes the grant.

## Recorded decisions (binding, 2026-11-18)

| ID | Decision |
|---|---|
| D-168a | DM soft-mute: any conversation participant may soft-mute an in-call peer. Record as intended and pin with a test. No client change. |
| D-168b | Fold in the channel-scope check and the dead `authorize-action` route deletion from Security #144, then close #144. |
| D-168c | Channel scope applies to session actions only (user-mute, user-deafen, disconnect, move). Persistent server mute/deafen stay at server scope. |
| D-168d | Move Members is checked at the source and the destination channel, in addition to the mover's VIEW and JOIN on the destination. |
| D-168e | Audit every non-self move that writes a temporary grant. |

## Brainstorming record (automated run)

This spec was produced in an unattended run. The project lead's answers are the recorded decisions above ("Accept all", 2026-11-18) and the design panel's reconciliation. Where the brainstorming process asks a question, the answer and its source are:

| Question | Answer | Source |
|---|---|---|
| Why now, and what does success look like? | Four open findings on the voice moderation surface; success is that each has a test that fails on `6ba00d24` and passes on the branch, with nothing refused today newly allowed | issue; C1 |
| Which approach? | A, narrow-only gate | panel; issue Option A |
| Fold in Security #144? | Yes: narrowing and route deletion; conferral moves to Security #142 | D-168b; R15 |
| Channel scope for persistent mute/deafen? | No | D-168c |
| DM soft-mute rule? | Any participant may soft-mute an in-call peer | D-168a |
| Audit scope? | Every non-self move that writes a grant, plus hierarchy crossings (existing) | D-168e |
| Calls beyond the decisions (list below) | Adopted by the panel; separable at approval | panel |
| Size | A project: full design. Builder check done by reading the document as the builder (no subagent in this run) | process |

Builder check outcome: no question needs the lead's answer to build. Minor items settled as calls: the exploit-test harness runs in a scratch copy (never committed); route counts are re-measured with the script, not edited by hand; client-side menu gating is out of scope.

## Goals, non-goals, anti-goals

Goals
- One shared preamble; a caller who is not a member, or lacks the server-scope bit, gets one uniform 403 before any voice-state read and before any transaction or lock.
- Every verdict that gates a write or a signal is recomputed in the committing transaction with the uncached transaction resolvers.
- A refused request writes nothing and signals nothing.

Non-goals
- Channel overrides do not confer moderation authority. A channel ALLOW of bits 18-20 stays inert; the server-scope bit stays necessary. Conferral depends on the override read mask tracked in Security #142 under Security epic #115.
- No fail-closed audit. The move audit stays best-effort here; the next change in the batch (Security #170) converts it.
- No edits to the bodies of `ServerMute`, `ServerUnmute`, `ServerDeafen`, `ServerUndeafen` or `applyServerEnforcement`.
- No change to `rbac/resolver.go`, to `tempGrantAllow`, or to the hierarchy exception (move remains the only `requireHierarchy=false`).

Anti-goals
- Nothing refused today may succeed after this change (monotone).
- No response may depend on activity in a channel the actor cannot see.
- No silent partial success: an action is applied to every session the actor can see, or refused.

## Constraints

| # | Constraint | Source |
|---|---|---|
| C1 | Monotone: server-scope bit stays necessary; overrides only narrow | Security epic #115 ordering rule; ADR-0023 |
| C2 | Authorize, then reveal. Non-members and members without the bit are refused on the pool, before `BeginTx`, and never take the visibility lock | this issue |
| C3 | Decide where you write: `ResolveServerPermissionsTx` / `ResolveChannelPermissionsTx` (`rbac/resolver.go:248,265`) in the committing transaction. The cached `HasPermission` preflight may only refuse early | `.claude/rules/domains/backend.md` |
| C4 | Delegation: the mover holds every bit the grant confers, checked in the grant transaction | this issue; ADR-0023 Decision 1 |
| C5 | Channels come from server-side voice state, never from the request, except the move destination and the temp-access `channel_id`, both validated in-server and in-transaction | Security #144 |
| C6 | Lock order unchanged: visibility advisory lock, then users (credential guard), then per-user voice lifecycle lock | `.claude/rules/domains/backend.md` |
| C7 | Public repository hygiene in the diff, commits and PR text | repo policy |
| C8 | Tests are proven by mutation and assert the outermost effect (override row, outbox row, audit row) | `.claude/rules/domains/tests.md` |

## Approaches

| | A. Narrow-only gate (chosen) | B. Channel-authoritative | C. Router middleware plus per-handler patches |
|---|---|---|---|
| Rule | server bit AND channel-resolved bit | channel-resolved bit alone | membership middleware, ad-hoc handler patches |
| Before the override read mask exists | safe, adds refusals only | makes the override writer part of the moderation trust base | safe |
| Latent override rows for bits 18-20 | ALLOW inert, DENY starts to bite | ALLOW goes live on every instance at upgrade | as A |
| Leaves | conferral for Security #142 | — | member-without-bit oracle, no in-transaction guarantee |

A is chosen: it is what the issue describes, satisfies C1 without waiting for Security #142, and keeps "authorize first" literal. The panel did not dispute the direction. It would change to B only once the read mask is on main.

## Reconciliation

Each row names a conflict between the design philosophy, the technical approach, the issue text or the recorded decisions, and the ruling.

| # | Conflict | Winner | Why |
|---|---|---|---|
| R1 | Session actions: "keep only the authorized channels" (philosophy) vs "hidden counts as absent, visible-but-denied refuses the whole request" (technical) | Technical | Keeping a subset returns 202 while skipping a channel the actor can see. That is a silent partial success. |
| R2 | One `revalidateVoiceMoveAuthorityTx(source, dest)` for both transactions (philosophy) vs staged helpers (technical) | Technical | The source is not known until the destination has been authorized; a single call would either read voice state first or resolve twice. The single composed check remains, as the grant closure. |
| R3 | Mandatory grant authority: "fail when `guardCredential` is set and authority is nil" (philosophy) vs "nil authority fails at grant entry regardless" (technical) | Technical, extended | Verified at `voice/temp_grants.go:213`: when `guardCredential` is false the function returns before it reaches the closure. The technical fix is necessary but not sufficient: a non-nil closure is also skipped in that case. The grant path must run the closure unconditionally. |
| R4 | "Reuse `errVoiceModPermission`" (philosophy) vs explicit mapping order (technical) | Technical | `rbac.ErrChannelNotInServer` wraps `rbac.ErrNotMember` (`rbac/resolver.go:27`). Without testing it first, a missing channel answers "Not a member of this server" and the collapsed answer leaks again. |
| R5 | Temp-access: add Move at the channel (philosophy) vs also remove the pool-side `isVoiceChannelInServer` check (technical) | Technical | The pool check (`voice/handlers.go:1639`) runs outside the transaction and gives a 400-versus-proceed signal that confirms hidden channel IDs. |
| R6 | Route-removal compatibility: "older released clients unverified" (philosophy) vs verified (technical) | Technical, extended | No commit in the history of the desktop client or the mobile client has ever contained `authorize-action`; no reference in this repository outside the handler, its route, its tests and its docs. |
| R7 | Issue text "mirror the channel check in the `authorizeVoiceMod` preflight" vs C3 | C3 | The channel is server-side voice state. Reading it on the pool would be a second, unlocked read of a fact the transaction reads under the visibility lock. The preflight stays server-scope and refuse-only. |
| R8 | Issue text "require the bit on the target's current channel for mute and deafen" vs D-168c | D-168c | Binding. Persistent mute/deafen ignore a channel DENY on purpose; a test pins that. Acceptance criterion 3 is reworded accordingly. |
| R9 | Closure signature carries `grantPath bool` and `targetID` (technical) | Dropped | The closure only runs on the grant path and checks facts about the actor. A parameter that is always true invites a caller to pass false. |
| R10 | Proof that a refused caller takes no lock: in-package fake SQL driver (technical) vs integration test | Integration test | Hold the server visibility lock in a second transaction; the refused request must still complete, and an authorized control must block until release. Outermost effect, real PostgreSQL, no new test infrastructure in this package. |
| R11 | Other DM handlers that read path IDs as raw text: same PR or separate | Same PR, own commit | Three-line defect of the same class in the same file: `RemoveMember` and `authorizeDMGroupAdmin`. Must land before the next change in the batch touches the file. |
| R12 | A channel override that carries the Administrator bit satisfies the new channel checks | Accepted, recorded | The new checks use the resolver's own semantics, the same ones `AuthorizeJoin` uses, so "can access" means one thing. No new authority arises because the server-scope bit stays necessary. A local raw-bit test would fork semantics and refuse movers who can in fact join. The class closes at the resolver with Security #142; ADR-0023 records it. |
| R13 | Finding identifiers in public text | Precedent | The changelog carries none; describe behaviour. The PR title and changelog follow suit. |
| R14 | "`verify-openapi-coverage` CI job" (technical) | Corrected | That job was folded into a combined guard job. The check itself is `scripts/api/check-openapi-coverage.sh`; router and spec must change in the same commit. |
| R15 | D-168b "close Security #144" vs that issue's first criterion (an override ALLOW confers the bit) | D-168b, with a note | The narrowing half and the route deletion ship here. The conferral half cannot ship under C1 and moves to Security #142. The closing note must also say that D-168c supersedes the earlier epic decision to check persistent mute/deafen at the target's channel. |

## Design

New code lives in a new file, `internal/voice/mod_authority.go`. Line numbers below are at `6ba00d24`.

### Authority rule

For actor A, moderation bit P, server S, channel C:

- Server scope (all routes): A is a member of S, and P is in A's server permissions. Hierarchy applies everywhere except move.
- Channel scope (user-mute, user-deafen, disconnect, move, temp-access revoke): additionally A holds `VIEW_VOICE_CHANNELS` on C and P on C, resolved with `ResolveChannelPermissionsTx`.
- "Holds" uses resolver semantics. The owner and server Administrators are not narrowed by overrides (`rbac/resolver.go:282`).

Two helpers make the bit tests safe:

- `hasAllPerms(p, required rbac.Permission) bool` returns `p&rbac.PermAdministrator != 0 || p&required == required`. `Permission.Has(A|B)` is an any-of test (`rbac/types.go:167`) and must not be used for a multi-bit requirement.
- `resolveActorVoiceChannelTx(ctx, tx, r, serverID, actorID, channelID) (perms rbac.Permission, present bool, err error)` tests `errors.Is(err, rbac.ErrChannelNotInServer)` before anything else and reports `present=false`. Any other `rbac.ErrNotMember` keeps today's mapping to the member 403. Callers map absence per the tables below.

### Shared preamble (pool, before `BeginTx`)

```go
type voiceModPreamble struct {
	perm             rbac.Permission
	requireHierarchy bool
	allowSelf        bool // move only
}
func (h *Handler) authorizeVoiceModWith(c *gin.Context, p voiceModPreamble) *voiceModContext
```

`voiceModContext` gains `self bool`. Order: path IDs (400) → membership (403 "Not a member of this server") → server-scope bit (403 "Insufficient permissions") → hierarchy (403).

- `authorizeVoiceMod(c, perm)` stays, as a one-line wrapper with `requireHierarchy: true`, so the persistent handlers do not change.
- When `allowSelf` is set and actor equals target, the permission and hierarchy steps are skipped. They are never called with a zero permission: `Permission.Has(0)` is false.
- Request-derived 400s (malformed IDs, malformed body, self-target on user-mute/user-deafen) are not voice state and may precede or follow the preamble as they do today, except in move, where the body is now parsed after the preamble.

### Move: `POST /servers/:id/voice/:userId/move`

Checked in this order. Every refusal writes nothing and signals nothing.

| # | Step | Failure |
|---|---|---|
| 1 | Preamble with `{PermMoveMembers, requireHierarchy: false, allowSelf: true}` | 400 / 403 member / 403 permission |
| 2 | `parseMoveRequest` | 400 `Invalid target_channel_id` |
| 3 | Transaction: visibility lock, `GuardFamilyTx` | existing 401/500 mapping |
| 4 | In transaction: membership; server-scope Move unless self | 403 member / 403 permission |
| 5 | Destination: is a voice channel of this server; actor holds VIEW and JOIN there; Move there unless self | 403 permission for all of these, including a missing, non-voice or foreign channel (today 400) |
| 6 | Source: `findVoiceChannelsTx` (sorted). Unless self, drop channels where the actor lacks VIEW. None left | 409 `Target user is not in a voice channel` |
| 7 | Unless self: Move on every remaining source channel | 403 permission |
| 8 | Destination is among the sources | 400 `user is already in the target channel` |
| 9 | Unless self: target's access to the destination (existing). If a grant is needed, the actor's destination permissions must satisfy `hasAllPerms(destPerms, tempGrantAllow)` | 403 permission |
| 10 | Grant transaction re-runs the composed check as its authority closure | 403 member / 403 permission; 409 source changed or no longer visible; 409 permanent-override block (existing) |
| 11 | Success | 200 `{"moved":true}` |

- `from` for the signal and for `expectedSourceChannelID` is the first remaining source channel.
- A self-move needs membership and the actor's own VIEW and JOIN on the destination. It needs no Move bit, crosses no hierarchy, and by construction never reaches the grant path; the handler must not call `prepareModeratedMove` for it.
- The destination is resolved once for the actor; steps 5 and 9 read the same result.

Helpers:

```go
func findVoiceChannelsTx(ctx context.Context, tx *sql.Tx, serverID, targetID string) ([]string, error) // ORDER BY vp.channel_id, no row lock
func authorizeMoveDestinationTx(ctx context.Context, tx *sql.Tx, r *rbac.Resolver, serverID, actorID, destID string, selfMove bool) (rbac.Permission, error)
func authorizeMoveSourcesTx(ctx context.Context, tx *sql.Tx, r *rbac.Resolver, serverID, actorID string, sources []string) (visible []string, err error)
func revalidateVoiceMoveAuthorityTx(ctx context.Context, tx *sql.Tx, r *rbac.Resolver, serverID, actorID, sourceID, destID string) error
```

`revalidateVoiceMoveAuthorityTx` composes membership, server-scope Move, the destination stage, `hasAllPerms(destPerms, tempGrantAllow)`, and the source stage on `{sourceID}`. A source that is no longer visible maps to `errTemporaryGrantSourceChanged` (409); a denied Move maps to `errVoiceModPermission`. It is the only authority closure passed to the grant.

### Session actions: user-mute, user-deafen, disconnect

Preamble and `guardVoiceModEffectTx` are unchanged. After `captureServerVoiceCommandSpecsTx`:

```go
func filterAuthorizedSessionSpecsTx(ctx context.Context, tx *sql.Tx, r *rbac.Resolver, serverID, actorID string, perm rbac.Permission, specs []voicecommand.CommandSpec) ([]voicecommand.CommandSpec, error)
```

For each distinct `spec.ChannelID`: channel absent or actor lacks VIEW → drop those specs; actor sees the channel but lacks `perm` there → refuse the whole request with 403 permission. A spec without a channel ID is an internal error (500), never a pass. If no specs remain, return the existing not-in-voice answer (400 for user-mute and user-deafen, 409 for disconnect), byte-identical to the genuine case.

### Temp-access revoke: `DELETE /servers/:id/voice/:userId/temp-access`

Remove the pool-side `isVoiceChannelInServer` check. The existing authority closure additionally calls

```go
func requireActorVoiceChannelPermTx(ctx context.Context, tx *sql.Tx, r *rbac.Resolver, serverID, actorID, channelID string, perm rbac.Permission) error
```

which requires: voice channel of this server, actor VIEW, actor Move. Every failure is 403 permission (today a foreign or non-voice channel is 400). `200 {"revoked":false}` for a no-op is unchanged. System cleanup keeps passing no authority.

### Persistent mute, unmute, deafen, undeafen

Unchanged by decision. A channel DENY of the bit does not affect them. A test pins this so a later change cannot alter it silently.

### Temporary grant authority (`voice/temp_grants.go`)

- New sentinel `errTemporaryGrantAuthorityRequired`. `grantTemporaryChannelAccessWithCredential` returns it before `BeginTx` when `authorization.authority == nil`.
- `guardTemporaryGrantAuthorization` (grant only; revoke has its own inline copy) keeps the credential guard conditional on `guardCredential` and runs the authority closure unconditionally.
- The zero-authorization wrapper `grantTemporaryChannelAccess` (`:127`) leaves production code. `export_test.go` supplies a named test-only closure, and the two tests that pass an empty `temporaryGrantAuthorization{}` to the grant (`temp_grant_fence_test.go:57`, `temporary_grant_cleanup_fence_test.go:174`) are updated to pass it.
- The closure stays between the credential guard and the lifecycle lock. `verifyTemporaryGrantSource` then confirms, under the lifecycle lock, that the target is still in the source the closure authorized.

### Audit

One seam replaces `auditMoveIfCrossesHierarchy`:

```go
func (h *Handler) auditVoiceMove(ctx context.Context, serverID, actorID, targetID, fromChannelID, toChannelID string, crossed, temporaryGrant bool)
```

- Writes one `voice_member_moved` row when `crossed || temporaryGrant`; never for a self-move. Metadata: `from_channel_id`, `to_channel_id`, `hierarchy_crossed`, `temporary_grant` (both booleans, both always present).
- `crossed` is computed in the decision transaction with `CheckHierarchyTx`; an error counts as crossed.
- `prepareModeratedMove` returns `(granted, ok bool)`.
- Best-effort, as today. All inputs exist before commit, so the next change in the batch can move the write into the committing transaction.

### DM soft-mute and DM path IDs (`dm/handlers.go`)

- `DMUserMute`: the rule is "any participant of the conversation may soft-mute an in-call peer; no admin role; 1:1 and group". State it in the handler comment and in the OpenAPI description. Read `id` and `userId` with `requestid.Param`, carry the canonical strings through the queries and the command specs, and compare self-target on canonical values. Status codes are unchanged.
- `RemoveMember` and `authorizeDMGroupAdmin` read the same two path parameters as raw text. Apply the same canonical-ID rule, in a separate commit. An alternate spelling of an ID must be treated exactly like the canonical one in every comparison those handlers make.

### Route removal

Delete `AuthorizeVoiceAction` (`voice/handlers.go:891` to the end of the function, plus anything only it used), its route (`api/router.go:2767-2770`), its OpenAPI path (`docs/api/openapi.yaml:3688`, plus schemas only it referenced), and its tests in `voice/handlers_test.go` and `voice/handlers_integration_2_test.go`. Router and spec change in one commit.

### Concurrency

- The new reads are plain SELECTs taken while the visibility lock is held. `requireChannelsInServer` takes no row lock (`rbac/resolver.go:31-53`).
- Permission reads are stable inside the transaction because RBAC writers serialize on the same advisory lock. Voice participant rows are not under that lock, which is why the source is re-verified under the lifecycle lock and never trusted from the decision transaction.
- A stale permissive cache reaches the transaction and is refused there with the same body. A stale restrictive cache refuses early, which C3 allows.
- The exclusive per-server lock is held for about two more resolves. Callers refused by the preamble no longer take it.

## Calls beyond the recorded decisions

These go further than D-168a to D-168e. The panel adopted the first four. Each is separable and may be struck at spec approval.

1. Visibility before verdict: a moderator who cannot see the target's channel gets the not-in-voice answer, not a 403, and cannot act there.
2. Collapsed destination answers: missing, non-voice, foreign and inaccessible destinations all return the same 403 (several were 400).
3. SPEAK on the grant path: the mover must hold all of `tempGrantAllow` when a grant will be written.
4. Temp-access revoke requires VIEW and Move at the named channel.
5. Canonical IDs in `RemoveMember` and `authorizeDMGroupAdmin` (R11).

## Decomposition

| Task | Content | Depends on | Parallel |
|---|---|---|---|
| T1 | Delete `AuthorizeVoiceAction`, route, OpenAPI path, tests; README row and counts | — | first; touches `voice/handlers.go` |
| T2 | `mod_authority.go`: preamble, `hasAllPerms`, channel resolve with error mapping, stage helpers; unit tests for the mapping order and the multi-bit helper | T1 | no |
| T3 | `ServerMove` reorder, self-move, grant closure, mandatory grant authority, audit seam; `move_test.go` | T2 | no (same file as T4) |
| T4 | Channel step in `userLevelAction`, `ServerDisconnect`, `RevokeTempAccess`; DENY tests; persistent-mute pin test | T2 | after T3 |
| T5 | `DMUserMute` rule, canonical IDs, pinning tests | — | yes |
| T6 | Canonical IDs in `RemoveMember` and `authorizeDMGroupAdmin`, with regression tests; own commit | — | yes, after T5 (same file) |
| T7 | `moderation_preamble_test.go`: response-equivalence matrix and the lock-held test | T3, T4 | no |
| T8 | Docs (see below) | T1-T6 | yes |
| T9 | Reproduction tests from the security issue run in a scratch copy; mutation proof; quality gate | all | no |

## Verification

Integration tests run against real PostgreSQL and Redis. Each refusal test asserts the absence of the outermost effect; each has a positive control.

Move (`move_test.go`)
- `TestServerMove_WithPermission_HiddenChannel_GrantsTemp`: give the mover's role an ALLOW of VIEW, JOIN and SPEAK on the destination; expect 200, the grant row, and one audit row with `temporary_grant=true`, `hierarchy_crossed=false`.
- Mover without VIEW, without JOIN, with a channel DENY of Move at the destination, with a channel DENY of Move at the source, and with a grant needed but SPEAK denied: 403 permission, no override row, no audit row.
- Target in a channel the mover cannot see: 409 with a body byte-identical to the not-in-voice case.
- Missing destination, text channel, and a channel of another server: the same 403 as "no access".
- Self-move: non-member 403 member; destination without access 403 permission; valid move 200 with no override row and no audit row.
- Hierarchy-crossing move with no grant: one audit row with `temporary_grant=false`, `hierarchy_crossed=true`.
- In-transaction refusal: inside `beforeTemporaryGrantForTest`, insert a DENY of VIEW for the mover on the destination; expect 403 and no override row.
- Nil grant authority returns `errTemporaryGrantAuthorityRequired` and writes no row; a non-nil closure runs even when `guardCredential` is false.

Session actions and temp-access
- Channel DENY of Mute → user-mute 403, no outbox row; likewise Deafen with user-deafen and Move with disconnect.
- A moderator who cannot see the target's channel gets the not-in-voice body and no outbox row.
- Target with sessions in a visible-and-allowed channel and a hidden channel: 202 and outbox rows for the visible channel only. Visible-and-denied plus visible-and-allowed: 403 and no outbox row.
- Pin for D-168c: with a channel DENY of Mute, persistent mute still succeeds and `server_members.server_muted` is set.
- Temp-access: channel DENY of Move, a channel without VIEW, a text channel, and a channel of another server each give 403 permission and delete nothing.

Preamble (`moderation_preamble_test.go`)
- A non-member against all nine routes (mute, unmute, deafen, undeafen, user-mute, user-deafen, disconnect, move, temp-access) in each state (target in voice, not in voice, text destination, missing destination, same channel, malformed body) gets a byte-identical 403 member. A member without the bit gets a byte-identical 403 permission. A positive control shows an authorized actor does get the distinct 200/400/409.
- Lock-held test: a second transaction holds `rbac.LockServerVisibilityCapture` for the server; the non-member request and the member-without-bit request complete while it is held; an authorized move does not complete until it is released.

DM
- A non-admin participant of a group DM, and a 1:1 peer, soft-mute an in-call peer: 202 and an outbox row. A non-participant: 403 and no outbox row. An alternate spelling of the caller's own ID: 400 self-target.
- `RemoveMember` and the group-admin gate: an alternate spelling of a target ID gives the same status, the same rows and the same emitted IDs as the canonical one.

Mutation proof (C8)
- Remove each new guard in turn; the matching test must fail: destination VIEW, destination JOIN, destination Move, source Move, SPEAK, the closure call, the unconditional closure invocation, the nil-authority check, the visibility filter, the session channel step, the revoke channel step, the error-mapping order.
- The old in-transaction guards now sit behind the pool preflight. Exercise them with a stale permissive cache: warm the cache with one request, remove the permission by direct SQL without invalidation, and expect 403 from the transaction.

Reproduction tests
- The two reproduction tests attached to the security issue are copied into a scratch copy of the branch, never committed. The non-member test must fail at its first probe on `403 Not a member of this server`, not on a 500. The move test must fail at its `200` assertion with a 403 and no override row. They call `moveTempOverrideExists` and `hideVoiceChannel`; keep those helper names and signatures.

## Acceptance criteria

| # | Criterion | Proof |
|---|---|---|
| A1 | A Move Members holder without VIEW or JOIN on the destination gets 403 and no `channel_permission_overrides` row is written, including when the permission is removed between the decision and the grant | `move_test.go` refusal and in-transaction cases |
| A2 | A non-member gets the same 403 body from every `/servers/:id/voice/:userId/*` route whatever the voice state, and takes no visibility lock | `moderation_preamble_test.go` |
| A3 | A moderator whose bit is denied on the target's channel gets 403 from user-mute, user-deafen, disconnect and move. Persistent mute and deafen are unaffected | DENY tests and the D-168c pin |
| A4 | Move Members is required at source and destination; a grant requires the mover to hold all of `tempGrantAllow` | `move_test.go` |
| A5 | Every non-self move that writes a temporary grant, or crosses the hierarchy, writes one audit row with both flags | `move_test.go` |
| A6 | DM soft-mute behaves as D-168a and is pinned; the three DM handlers treat alternate ID spellings identically | `dm/handlers_test.go` |
| A7 | `POST /channels/:id/voice/authorize-action` is gone from router, spec, docs and tests; `scripts/api/check-openapi-coverage.sh` passes | CI |
| A8 | Both reproduction tests fail against the branch for the stated reason, run with PostgreSQL and Redis | T9 record |
| A9 | Every new guard has a test that fails when it is removed | mutation record |
| A10 | Quality gate: 0 new blocker or critical issues | `/verify-quality-gate` |
| A11 | Quality gate: 0 new high-confidence security hotspots | `/verify-quality-gate` |
| A12 | Quality gate: at least 80% coverage on new code | `/verify-quality-gate` |
| A13 | Docs to update ship in this PR (list below): `docs/api/openapi.yaml`, `docs/architecture.md` and `CHANGELOG.md` are updated wherever behaviour changes | docs review |
| A14 | Every finding in the issue (VULN-102, VULN-103, VULN-197, VULN-106) has at least one exploit or regression test in the committed suite that fails on `6ba00d24` and passes on the branch. VULN-106 is a pinning test: it records intended behaviour and fails if the rule changes | red-green record in the PR: each test run on the base commit (fail, quoted) and on the branch (pass) |
| A15 | Focused suites pass: `go test ./internal/voice/... ./internal/dm/... ./internal/api/...` with `DATABASE_URL` and `REDIS_URL` set | command output |
| A16 | The full affected module suite passes: `go test ./...` in `services/control-plane`, plus `go vet` and the repo lint | command output |

## Docs to update

- `docs/adr/0023-temporary-sbac-and-move-hierarchy-exception.md`: new decision "the mover holds what the move confers, at channel scope" (destination VIEW, JOIN, SPEAK-on-grant; Move at source and destination; in-transaction re-check). Amend Decision 2: a self-move now needs membership and destination access. Amend its mitigations: audit also on a temporary grant. Record R12 and the server-scope rule for persistent mute/deafen.
- `docs/api/openapi.yaml`: remove the `authorize-action` path; move and temp-access no longer answer 400 for a non-voice or foreign channel; 403 on user-mute, user-deafen, disconnect and temp-access; the DM soft-mute rule.
- `docs/architecture.md`: a new `#### Voice moderation authority` subsection at the end of the Control Plane section (before `### Media Plane`): preamble before voice state, channel scope for session actions and move, server scope for persistent mute/deafen, mover holds what a grant confers, grant always runs its authority closure.
- `docs/api/README.md`: Voice row (27 operations today) and the totals paragraph, re-measured with `scripts/api/extract-routes.py`.
- `CHANGELOG.md` `[Prerelease]`: a Security entry (behaviour only, no finding identifiers), a new Removed entry for the route, and the totals line.
- `.claude/rules/domains/backend.md`: a short voice-moderation-authority rule (authorize before reveal; decide at the channel in the committing transaction; `hasAllPerms` for multi-bit checks; `ErrChannelNotInServer` before `ErrNotMember`; a grant always runs its authority closure).
- Handler comments for `ServerMove`, `DMUserMute` and the grant function.

## Upgrade notes for operators

- Channel DENY rows for Mute, Deafen or Move Members that had no effect on voice session actions now take effect.
- Moving someone now needs the mover's own access to the destination, and Move Members on both channels. Organizer roles that relied on moving people into channels they cannot enter must be given access.
- A moderator can no longer act on a participant in a channel the moderator cannot see.
- Several 400 answers become 403. The desktop client handles non-2xx answers from these routes generically; no client change is required.

## Risks

| Risk | Likelihood | Mitigation |
|---|---|---|
| Security #144 closed with its conferral criterion silently dropped | Certain without a note | Closing note per R15 |
| New early 403s make existing tests vacuous | High | Mutation proof, stale-cache cases, positive controls |
| A target with sessions in two channels | Low | Every channel the effect touches is authorized; source order is deterministic |
| Latent DENY rows start to bite on upgrade | Medium | Changelog and upgrade notes; no pre-deploy count is possible on self-hosted instances |
| Grant audit is best-effort until the next change lands | Medium, short | Single seam with inputs available before commit |
| Rebase conflict with the next change in the batch | Medium | New file; persistent handler bodies untouched |
| Menu items for session actions return 403 in denied channels | Medium, cosmetic | Server enforces; client gating is optional polish |

## Left to the builder

- Internal names other than the signatures above.
- Whether the decision-transaction guard for move is a new function or a generalisation of `guardVoiceModEffectTx`, provided existing callers keep their behaviour.
- How the target's own resolve error on the destination is mapped once the actor is authorized.

## Dependency references

- PostgreSQL READ COMMITTED gives each statement a fresh snapshot; the design relies on the advisory lock, not on snapshot isolation, for stable permission reads. Checked against current PostgreSQL documentation during the technical review.
- `lib/pq` (Context7 `/lib/pq`, fetched 2026-11-18): `Commit` returns the result of the `COMMIT` statement; a context cancelled during the transaction poisons the connection and surfaces as `driver.ErrBadConn`, never as `ctx.Err()`. A refused request must therefore roll back explicitly and never rely on the context to undo a write. `pq.As(err, codes...)` is the current way to test SQLSTATE codes if a test needs it.
- Gin (Context7 `/gin-gonic/gin`): `c.Param` returns the raw path segment with no normalisation, which is why canonical IDs must come from `requestid.Param`. `AbortWithStatusJSON` does not return from the handler; every refusal path needs an explicit `return`.
- go-redis v9 (Context7 `/redis/go-redis`): the stale-permissive-cache tests change permissions by direct SQL and must not call the cache invalidation path; `Del` returns an `*IntCmd` whose `Err()` must be checked where a test clears a key itself.
- No new dependency. No third-party API contract changes.
