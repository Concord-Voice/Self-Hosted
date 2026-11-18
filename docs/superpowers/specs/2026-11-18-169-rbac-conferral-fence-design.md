# Security#169: RBAC conferral checks and permission-cache fence — Design

Issue: Concord-Voice/Security#169 (parent epic Concord-Voice/Security#152). Also closes
Concord-Voice/Security#97 (VULN-066) and Concord-Voice/Security#98 (VULN-067).
Branch: `feature/security-169-rbac-conferral-fence`. Scope: Self-Hosted control-plane.

Checked against worktree HEAD `6ba00d24`. Security#168, Security#170 and Security#167 have not
merged, so every line anchor in `members/`, `invites/`, `voice/` and `channels/` will move.
Implementation starts after Security#167 merges and the branch is rebased (task T0).

Bare paths are relative to `services/control-plane/internal/`. `backend.md` means
`.claude/rules/domains/backend.md`. Rule tags such as "#2721 F3" or "#2272 lock order" are the
names those rules already carry in `backend.md`; they are not references to new work.

## How this spec was produced

This run was unattended. The brainstorming questions were answered from four inputs, and the
answers are recorded here instead of in a conversation:

- the issue brief for Security#169;
- the Phase-1 blast-radius scout;
- the binding decisions D-169a to D-169e, which the project lead accepted on 2026-11-18
  ("Accept all");
- the Phase-2 design panel (architect, system engineer, architect reconciliation).

| Question the skill asks | Answer | Source |
|---|---|---|
| Why now, and for whom? | Six open findings from the 2026-11-11 scan, in the v1.0.0 milestone. Operators of self-hosted servers rely on role managers and moderators not being able to grant or keep more authority than they hold. | brief |
| What does success look like? | The brief's Definition of Done, unchanged (see "Intent"). | brief |
| Which approach? | Static per-channel conferral rule; existing generations plus MFA-only grace generations; in-transaction re-check under a shared advisory lock; both capture phases chunked. | panel, reconciled |
| One PR or several? | One PR, with separate `Closes` lines for Security#169, Security#97 and Security#98. | D-169b |
| Sequencing? | After Security#170 (add and unban shape) and Security#167 (channel-type `CASE`). | D-169d, scout |
| Migration? | None. | decisions file |
| Wire-shape change? | None. New 403s reuse the existing body. No client companion change. | scout |
| Size? | A project (L, about 28-36 engineering hours before review). Full design doc. | scout |

## Intent

A role grant or removal is checked against everything it changes for the holder, at server and
channel scope. Copy paths apply the same subset rule as direct override writes. Durable
membership writes re-check the actor's authority inside the write transaction. A cache fill
cannot republish revoked authority, and invalidation does not depend on the caller's
connection. Role mutations work on servers at the 500-channel cap.

The guards today judge a change by less than the resolver later grants. A role gives its
holders server bits, per-channel ALLOW overrides and per-channel DENY overrides, but the guard
counts only the server bits. That one gap produces VULN-066 (assign confers channel ALLOWs) and
VULN-067 (removal lifts channel DENYs). VULN-068 is the same gap on the category copy paths.
VULN-053 and VULN-063 are the cache trusting itself after a revocation. VULN-062 is a safety
bound that is smaller than the channel cap.

## Goals

- Close VULN-066, VULN-067, VULN-068, VULN-053, VULN-063 and VULN-062 on Self-Hosted.
- Bump the permission generation on every role, membership and override writer (D-169e). This
  is the core of Security#48 and parts of Security#47 and Security#50.
- Keep denial ahead of `PrepareCapture` on every route, so capture cost cannot leak voice
  occupancy.

## Non-goals

- Changing what Manage Channels means at channel scope. Security#115 owns that question. This
  PR pins today's override-route behaviour with a test and a doc line (panel conflict 2).
- A cache value format change. The format stays at version 2.
- A port to other deployments of the control-plane. That is a separate PR after this one merges.
- Any client change. Desktop and Mobile do not special-case these routes or this error body.

## Anti-goals (a failure even if the tests pass)

- A refusal that returns after `PrepareCapture` has run, or after any DML, key rotation or
  candidate revocation.
- A pre-check verdict that differs from the in-transaction verdict on the same inputs.
- A failed authoritative read that grants instead of refusing.
- A step-up grace that now expires after every role or override edit (repeated MFA prompts).
- Docs or comments that claim the membership re-check is totally ordered against every revoker.
- More statements under the per-server advisory lock than today.

## Constraints

| Constraint | Source |
|---|---|
| Chunk the capture at 64 channels per chunk; denial still precedes `PrepareCapture` | D-169a |
| Removal subset-checks lifted DENY bits, also when removing another member's role; reverses the comment at `rbac/handlers.go:2194` | D-169c |
| Bump on every role, membership and override writer | D-169e |
| Generation invariant: tags read before compute, bump after commit, random generations, fallback deletes the generation key first | `rbac/cache.go:19-49`, `backend.md:487-513` |
| Pre-check verdict equals the in-transaction verdict; the pre-check stays non-authoritative and fails open on a read error | `rbac/role_guard.go:235-282`, `backend.md:684` |
| Advisory lock is the first statement of the transaction; #2272 lock order | `backend.md:412,415`, `rbac/authority_tx.go:70-77` |
| `internal/rbac` gains no presence imports | `rbac/presence_recheck.go:79-88` |
| No second outbox, table, dispatcher or environment variable | `rbac/authority_tx.go:137-140` |
| New reads of `member_roles` or override tables are server-qualified | `backend.md` (#2869) |
| Actor permissions are compared after the MFA mask; the unenrolled-owner rule (RS4) keeps parity | `rbac/role_guard.go:268-282`, `rbac/role_gates.go:222-241` |

## Third-party behaviour this design relies on (Context7, 2026-11-18)

Versions from `services/control-plane/go.mod`: `lib/pq` v1.12.3, `go-redis/v9` v9.23.0,
`gin` v1.12.0.

- **PostgreSQL advisory locks** (PostgreSQL docs §9.28.10): a shared advisory lock conflicts
  only with an exclusive lock on the same key; transaction-level locks release at transaction
  end. Queueing of a new shared request behind a waiting exclusive one is lock-manager behaviour
  and is not stated in those docs; T7 tests it.
- **`INSERT ... ON CONFLICT DO NOTHING RETURNING`** returns only rows actually inserted, so no
  row means a conflict (PostgreSQL `INSERT` docs). The invite-create retry loop relies on this.
- **`lib/pq` commit is ambiguous under cancellation.** `Commit` returns whatever the `COMMIT`
  round trip yields; a cancel that lands after the server commits can still surface an error
  (57014) although the commit is durable. This is why the ambiguous-commit paths must bump too,
  and why the bump runs on a detached context.
- **`go-redis` `TxPipelined`** wraps the queued commands in `MULTI/EXEC`, so the two MFA
  generation replacements in section 4.2 are atomic in one round trip.
- **`gin` `Context`** delegates `Done`/`Err` to `c.Request.Context()`, which is cancelled when
  the client disconnects. Any post-commit work on the request context can be skipped by a
  disconnect, which is VULN-063's second half; the bump uses `context.WithoutCancel` plus its
  own deadline (the existing `bumpWithFallback`).

## Approaches

The panel weighed these. The lead's "Accept all" covered D-169a to D-169e; the choices below
were made by the panel inside those decisions.

- **Conferral (VULN-066/067/068).**
  - *Static per-channel rule (chosen).* One reader and one verdict, shared by the guard and the
    pre-check. Compares channel ALLOWs (assign) and channel DENYs (removal, delete) with the
    actor's effective permissions in that channel. Costs one or two reads when the role has
    override rows; refuses conservatively in some moderation flows (lead call 1).
  - *Server-scope aggregate, as the brief's scope line says.* Smallest change, but a member
    restricted by a channel DENY still holds the bit at server scope, so VULN-067 stays open,
    and an actor denied VIEW in a private channel can still assign the role that ALLOWs it.
  - *Apply, re-resolve, roll back on excess.* Exact, but the pre-check cannot reproduce it, so
    denials would land after capture and reopen the occupancy signal.
- **Bump granularity (VULN-063, D-169e).**
  - *Existing user and server generations, plus MFA-only generations for graces (chosen).* No
    cache format change; graces behave as they do today.
  - *Existing generations alone.* Breaks graces: a post-commit server bump invalidates the grace
    the same request just earned, and every moderator's grace on the server.
  - *A third per-member generation.* Needs cache format 3, and server-wide writes still bump
    the server generation, so it does not fix graces.
- **Membership re-check (VULN-053).**
  - *In-transaction read under a shared per-server advisory lock (chosen).* Orders the write
    against every revoker that holds the exclusive lock.
  - *In-transaction read alone.* A revocation that commits between the read and the write
    still wins.
- **Chunking (VULN-062, D-169a).**
  - *Both phases (chosen).* At most 8 statements under the lock at the cap.
  - *Phase 1 only.* Meets D-169a literally but moves up to 500 statements under the lock.

## Panel conflicts and how each was settled

| # | Conflict | Winner | Why |
|---|---|---|---|
| 1 | Graces under the new bumps: reuse the existing generations, or give graces their own | Own generations | A grace is stamped with the generations read before the transaction (`stepup/grace.go`, `Read` near :189) and judged against them (`Covers`, near :160). Role-delete and kick/ban graces are server-scoped (`rbac/role_gates.go:105`, `members/moderation_gate.go:56`). Today only MFA events bump, so MFA-only generations keep step-up behaviour as it is. |
| 2 | Override routes: route the DENY-lowering case to Security#115 | Recorded as intended | `authorizeOverrideUpsertTx` judges Manage Channels and the ALLOW subset at server scope (`rbac/override_gates.go:136-146`). A channel-scope DENY-lift check there would stop a holder from unlocking a channel they locked for everyone. Pinned by a test and a doc line. |
| 3 | Ordering claim for membership writes | All revokers except the MFA enforcement toggle | The toggle takes `servers FOR NO KEY UPDATE`, not the exclusive server lock (`servers/mfa_enforcement.go:356`). |
| 4 | The add path (D-169d) | Converged | T0 reads what Security#170 landed; three outcomes in section 3. |
| 5 | Where the copy check lives | Only at the three new-child copy sites | The category-upsert cascade confers nothing new; a check there would refuse an actor because of another target's row. |
| 6 | Total bump failure | Local bypass | The brief requires the fence to fail closed when Redis is unavailable. Section 4.3. |
| 7 | Position-only writes and CreateServer | Bump (literal D-169e) | Harmless once graces are decoupled. |
| 8 | DeleteRole when the role has no holders | Static rule applies | One verdict keeps the pre-check and the guard in agreement. |

## Design

### 1. Conferral verdict (VULN-066, VULN-067)

One pure function and one reader, both in a new `rbac/role_conferral.go`, called by the in-transaction guard (`evaluateRoleGuard`) and by the pooled pre-check (`preCheckRoleMutation`).

- `readRoleConferralInputs(ctx, q, serverID, roleID)` reads the role's override rows on its own server: channel rows and category rows, each with `allow` and `deny`. The read is server-qualified (#2869) and takes no row lock.
- `(*Resolver).ResolveChannelPermissionsBatchTx(ctx, q, serverID, userID, channelIDs)` returns the actor's effective permissions for those channels. It is the batched form of `ResolveChannelPermissionsTx` and reuses `applyBatchedChannelOverrides`, generalised from `*sql.Tx` to a query interface so the pre-check can run it on the pool. The Administrator bypass reads the raw base and the MFA mask is applied once at the exit, in this one function. The pre-check must not re-implement either.
- `roleConferralVerdict(mode, roleBits, rows, actorServer, actorByChannel)` decides.

The rule, for a non-owner actor:

| Route | Must hold |
|---|---|
| Assign | role bits within the actor's server permissions (today's check); each channel row's ALLOW within the actor's effective permissions **in that channel**; each category row's ALLOW within the actor's server permissions |
| Unassign, DeleteRole | each channel row's DENY within the actor's effective permissions **in that channel**; each category row's DENY within the actor's server permissions |
| UpdateRole | unchanged |

Why channel scope: a member restricted by a channel DENY normally still holds that bit at server scope, so a server-scope comparison always passes and VULN-067 stays open. Why category rows use server scope: the resolver never reads `category_permission_overrides`; those rows confer only through later copies.

The rule is static. It ignores the target's other roles, so the pre-check reaches the same verdict as the guard without simulating the write.

Owners keep their bypass. The unenrolled-owner rule (RS4) keeps parity: on an enforcing server, an owner with no inline factor who unassigns a role is refused with the enrollment-required response when the lifted DENY bits include a bit in `DangerousPermissions`. `preCheckOwnerConferral` gains the same term. T0 checks whether the `roles.delete` step-up gate already refuses that owner on DeleteRole; if it does not, the same term applies there.

Failure rules:
- A failed read in the transaction returns 500 and never grants.
- A failed read in the pre-check returns nil and falls through, as the rule in the `preCheckRoleMutation` comment requires (`rbac/role_guard.go:250-257`).
- Every new refusal returns before any DML, so no key rotation and no candidate revocation fires.
- A role with no override rows costs one extra read and gets today's verdict.

The existing ALLOW subset check on the override upsert routes (`rbac/override_gates.go:146`) already covers the brief's defense-in-depth item. It gets a regression test, not new code.

Comments this reverses: `rbac/role_guard.go:70-79` (the note telling readers not to add a subset check to unassign), `rbac/handlers.go:2194-2196`, and the stale writer list at `rbac/role_guard.go:466-468`.

### 2. Copy paths (VULN-068)

One predicate, `authorizeOverrideCopy(actorPerms, copiedAllow)`, extracted from `rbac/override_gates.go:146`: Administrator passes; otherwise `copiedAllow &^ actorPerms` must be empty. `actorPerms` is the actor's **server** permissions, the same scope the direct override write uses.

Applied at the three sites that copy category rows onto a channel that did not carry them:
- permission-sync enable (`rbac/handlers.go:3500`, where the resolved permissions are discarded today);
- synced channel move (`channels/handlers.go:1589`);
- reorder into a synced category (`channels/groups.go:1211`).

The exported `rbac.ReplaceCategoryOverridesForChannelsTx` gains a required `actorPerms` argument and checks before any DML, so the two `channels` callers cannot omit it. The unexported cascade used by category upserts stays unchecked (conflict 5). A pooled pre-check with the same predicate keeps denial ahead of capture. `channels` maps the refusal to 403 with the existing body.

### 3. Membership-write re-check (VULN-053)

New `rbac.WithServerAuthorizedWriteTx(ctx, db, resolver, req, write)`. It owns the whole transaction:

1. `BEGIN` at READ COMMITTED.
2. `pg_advisory_xact_lock_shared` on the server key, as the first statement (new `LockServerVisibilityCaptureShared`).
3. `credepoch.GuardFamilyTx` (writer barrier, then `users FOR SHARE`).
4. `ResolveServerPermissionsTx` and the permission test.
5. The caller's DML, plus Security#170's audit insert.
6. `COMMIT`.

A shared lock conflicts only with the exclusive form on the same key, so it orders the write against every writer that holds the exclusive server lock: role assign, unassign, update, delete and reorder, kick and ban (`members/handlers.go:1398,1962`), and ownership transfer (`ownership/handlers.go:1120,1182`). MFA factor removal is ordered through the users row. Two membership writes do not block each other.

Hard rules:
- **Never upgrade from shared to exclusive in the same transaction.** Two upgrading holders deadlock. The callback may only run DML.
- The lock order stays advisory, barrier, users, domain rows (#2272).
- The cached `HasPermission` stays in front as a cheap early denial with today's bodies. An in-transaction denial is answered exactly as the cached denial is today.
- A failed re-check read returns 500. `ErrNotMember` returns 403.

Callers:
- **Unban**: wraps whatever transaction Security#170 builds around `UnbanMember`.
- **Invite create**: the insert moves into the transaction as `INSERT ... ON CONFLICT DO NOTHING RETURNING created_at`. No row returned means a code collision: pick a new code and retry in the same transaction, at most 5 times. Any other error stops the loop. Today's loop swallows every error, including outages.
- **Add (D-169d)**, by what Security#170 landed:
  - route removed: a test pins that `POST /servers/:id/members` no longer exists;
  - route limited to system principals: a test pins that a planted cache entry granting every bit does not admit a non-system caller;
  - any RBAC-authorized add path: it goes through this helper.

### 4. Generation fence (VULN-063, D-169e)

#### 4.1 One post-commit entry point

`(*Resolver).PublishPermissionInputChange(ctx, PermissionInputChange{ServerID, Server bool, UserIDs []string})`, built on `bumpWithFallback` (`rbac/resolver.go:714`), which already detaches from the request, retries once, and falls back to deleting the generation key first. That closes both halves of VULN-063: the republish race and the cancelled context.

- It runs after commit and before the response, and before `recheckVoice*` and any WebSocket announcement of the change.
- `CompleteChannelAuthorityMutation*` and `FailClosedChannelAuthorityMutation` take a `PermissionInputChange` as a required argument. Ambiguous-commit paths bump too; a bump is idempotent.
- `PermissionCache.Invalidate`, `PermissionCache.InvalidateChannel`, `Resolver.InvalidateChannel` and `Resolver.InvalidateUser` are deleted, so delete-based invalidation no longer compiles. The SCAN deletes survive only inside the fallback.
- The cache value format stays at version 2. Old and new binaries share it.

| Writer | Bump |
|---|---|
| CreateRole, UpdateRole, DeleteRole, ReorderRoles | server |
| AssignRole, UnassignRole | target user |
| Channel override upsert and delete | server when the target is a role; that user when the target is a user |
| Category override upsert, delete and cascade; sync enable and disable; the two `channels` copy callers; `channels/groups.go:686`; `channels/handlers.go:1638` | server |
| Kick, ban, leave, invite join, any add path Security#170 leaves | target or joining user |
| Ownership transfer and reversal | server |
| CreateServer, DeleteServer | server |
| Voice temporary grants (`voice/temp_grants.go`) | grantee user; server if a grant can target a role (T0 check) |
| Mute, deafen and `timed_out_until` writes on `server_members`; `server_bans`; `server_invites` | exempt: the resolver reads only whether a `server_members` row exists |

A writer-registry test lists every DML site on `member_roles`, `roles`, `server_members`, both override tables and `servers.owner_id` in non-test code. Each site needs a bump scope or a recorded exemption from this table. A new writer cannot land without that decision.

#### 4.2 Graces move to MFA-only generations

- `permgen` gains `MFAUserKey` (`permgen:mu:{user}`) and `MFAServerKey` (`permgen:ms:{server}`): random 64-bit values with `permgen.TTL`, like the existing keys.
- `stepup.GraceStore` (`Read`, `Grant`, `GrantRead`, `seedGenerations`) stamps and compares the MFA keys instead of `permgen:u` and `permgen:s`. `Grant` refreshes the TTL of the generations it stamps with `EXPIRE`, because no cache publish renews these keys.
- The two MFA writers (`mfa/handlers.go:271`, `servers/mfa_enforcement.go:437`) keep invalidating the permission cache and now also replace the MFA generation. Both keys are replaced in one `MULTI/EXEC`. The fallback deletes both generation keys before any entry. The resolver methods they call are renamed `PublishUserMFAChange` and `PublishServerMFAChange` so the two kinds of bump cannot be confused at a call site.
- Graces already in Redis read as absent after the upgrade, so each user is prompted once. That is the safe direction.
- Result: a grace expires on an MFA event and on nothing else, which is today's behaviour.

#### 4.3 Local bypass when a bump fails completely

If the bump, its retry and the fallback all fail, the write has already committed and the request still returns 200. Before returning, the resolver marks that server or user as bypassed in process memory for the cache TTL (5 minutes) plus 30 seconds. While a scope is bypassed, cache reads for it count as misses and fills for it are skipped, so this instance serves only database truth.

- The map is bounded at 4096 scopes. On overflow the whole cache is bypassed until the latest expiry.
- The failure is logged at ERROR with `failure_class=permission_generation_bump_failed`.
- This closes the residual on a single-instance deployment, which is the Self-Hosted shape. A multi-instance deployment would still let other instances serve the stale entry until the TTL; that is out of scope here.

### 5. Chunked capture (VULN-062, D-169a)

All changes are in `voicepresence/executor.go`, plus one batched query in `rbac/channel_viewers.go`. `internal/rbac` still imports nothing from presence.

- `presenceCaptureMaxChannels = 64` keeps its value and now means the chunk size.
- `presenceCaptureMaxChunks = 8`, derived from the exported `rbac.MaxChannelAuthorityChannels` (500). Beyond 8 chunks the capture still fails closed with `ErrCaptureChannelLimit`. The 500-channel cap makes that unreachable outside a unit test.
- Phase 1 (pool): one `activeSenders` query per chunk using `channel_id = ANY($1)`, where today there is one per voice channel. One shared `memberLoader`.
- Phase 2 (under the exclusive lock): one batched visibility query per chunk through a new `FilterVisibleUserIDsForChannelsTx`. The single-channel function becomes a one-element call of it, so one copy of the SQL predicate exists. At the cap that is at most 8 statements under the lock, where today's limit is 64.
- All chunks run in one transaction, under one lock, before the write. Atomicity is unchanged.
- The batched SQL must carry Security#167's fail-closed channel-type `CASE` (D-167e).

Both phases are chunked because chunking phase 1 alone would move up to 500 statements under the lock.

## Invariants that must hold

| Invariant | Source |
|---|---|
| Tags are read before compute; bumps run after commit; generations are random; the fallback deletes the generation key first | `rbac/cache.go:19-49`, `backend.md:487-513` |
| The pre-check verdict equals the in-transaction verdict, MFA mask included; the pre-check is never authoritative and fails open on a read error | `rbac/role_guard.go:235-282` |
| The advisory lock is the transaction's first statement; lock order follows #2272 | `rbac/authority_tx.go:70-77` |
| New reads of `member_roles` or overrides are server-qualified | `backend.md` (#2869) |
| A failed authoritative read refuses with 500 and never grants | brief |
| A refusal returns before any DML, so no key rotation or candidate revocation fires | `rbac/authority_tx.go:551-600` |
| No second outbox, table or dispatcher; no new environment variable | `rbac/authority_tx.go:137-140` |
| No wire-shape change; every new 403 uses `errMsgCannotGrantPerms` with no detail | scout |

## API changes

| Route | Change |
|---|---|
| `POST /servers/{id}/members/{user_id}/roles` | New 403 for a channel or category ALLOW outside the actor's permissions. Above 64 voice channels: 500 becomes 200. |
| `DELETE /servers/{id}/members/{user_id}/roles/{role_id}` | New 403 for a lifted DENY outside the actor's permissions, on self-removal and on removal from another member. New enrollment-required response for the unenrolled-owner case. 500 becomes 200. |
| `DELETE /servers/{id}/roles/{role_id}` | Same DENY-lift 403. 500 becomes 200. |
| `PATCH /servers/{id}/roles/{role_id}` | Only 500 becomes 200. |
| `PUT /channels/{id}/permission-sync`, synced channel move, `/servers/{id}/channels/reorder` into a synced category | New 403 when the copied ALLOW bits are outside the actor's server permissions. |
| `DELETE /servers/{id}/bans/{user_id}`, `POST /servers/{id}/invites` | Bodies unchanged. They can now return the existing credential-epoch error. |

The 403 body reads "Cannot grant permissions you do not have" on a removal too. It stays uniform so that the response does not reveal which term refused.

## Rollout

- Self-Hosted restarts in place, so there is no mixed-version window.
- Old and new binaries share the version-2 cache. Old readers honour generations, so new bumps
  invalidate for them too. Old binaries take only the exclusive advisory lock, on the same key,
  so they conflict correctly with the new shared lock.
- Graces stamped before the upgrade read as absent afterwards, so each user is prompted for
  step-up once. That is the safe direction.
- Depends on Security#170 (unban and add shape) and Security#167 (channel-type `CASE`). Rebase
  after Security#167 merges.

## Tasks

| # | Task | Depends on | Parallel |
|---|---|---|---|
| T0 | Rebase after Security#167. Re-anchor. Run the "Not verified" list. Port `override_conferral_vuln066_test.go` from the Security repo scan as a failing test. | predecessors merged | gate |
| T1 | `rbac/role_conferral.go`: reader, verdict, `ResolveChannelPermissionsBatchTx` | T0 | chain A root |
| T2 | Wire T1 into assign, unassign, delete and both pre-checks; RS4 parity; rewrite the reversed comments | T1 | chain A, serial |
| T3 | Copy predicate, required `actorPerms` argument, 403 mapping in `channels` | T0 | parallel |
| T4 | Grace generations in `permgen`, `stepup`, `mfa`, `servers` | T0 | lands before T5's writer conversion |
| T5 | `PublishPermissionInputChange`; required argument on the completion helpers; delete the invalidation API; convert every writer; writer-registry, race and context-cancel tests | T4 | shares `rbac/handlers.go` with T2 and T3 |
| T6 | Local bypass on total bump failure | T5 | serial |
| T7 | `WithServerAuthorizedWriteTx`, shared lock, unban, invite create, add-path pin | T0 | parallel |
| T8 | Chunked capture, batched visibility, F3 test redesign, 65-channel fixtures | T0 | parallel |
| T9 | Docs (see "Docs to update") | T1-T8 | last |

Task numbers here are the spec's; the plan renumbers into executable steps.

## Acceptance criteria

Overall gates:

- **Every VULN in the issue has an exploit or regression test that fails on the old code and
  passes on the new code.** VULN-066, 067, 068, 053, 063 and 062 each map to at least one row
  below. Each test is run once against the pre-fix code and its failure output is recorded
  before the fix lands (test-first). The ported `override_conferral_vuln066_test.go` fails
  against the fixed code, as the issue requires (it asserts the exploit succeeds).
- **Focused and full affected package suites pass**: the focused test for each task, then the
  full suites of every changed package (`rbac`, `voicepresence`, `members`, `invites`,
  `channels`, `stepup`, `permgen`, `mfa`, `servers`, `ownership`, `voice`), then
  `go test ./...` for the control-plane, with real Postgres and Redis.
- **`docs/api/openapi.yaml`, `docs/architecture.md` and `CHANGELOG.md` are updated** where
  behaviour changes (see "Docs to update").

| # | Criterion | VULN | Proof |
|---|---|---|---|
| 1 | An actor cannot assign a role whose override ALLOWs exceed the actor's permissions, including when the actor holds the bit at server scope but is denied it in that channel (403) | 066 | integration tests; ported exploit test |
| 2 | An actor cannot write ALLOW bits they lack onto a role through the override upsert routes | 066 | regression test on the existing check |
| 3 | An actor restricted by a role's channel DENY cannot remove that role from themselves, from another member, or delete it (403); an actor who holds the bit in that channel can (200) | 067 | integration tests |
| 4 | Sync enable, synced move and synced reorder refuse (403) when copied ALLOW bits exceed the actor's permissions, with row counts unchanged, no rotation and no capture; an Administrator passes | 068 | integration tests |
| 5 | After a ban, kick or demotion commits, the former moderator's unban and invite-create calls are refused even with a freshly planted cache entry carrying current generations; the add path is pinned per section 3 | 053 | integration tests |
| 6 | An unban that starts while a demotion holds the exclusive lock waits, then is refused; two concurrent membership writes do not wait on each other | 053 | `testdb/lockwaiters.go` tests |
| 7 | A revoked grant is not served after the revoking request returns, for every writer family, including when the client disconnects mid-request | 063 | `afterCompute` race tests; context-cancel tests |
| 8 | After a total bump failure the instance serves database truth for that scope and logs `failure_class=permission_generation_bump_failed` | 063 | broken-Redis test |
| 9 | A grace earned on a gated action still covers the next one after a role or override write; an MFA factor change or enforcement toggle still expires it | 063 (regression) | step-up tests |
| 10 | Owner role mutations return 200 on a server with 65 occupied voice channels, on all four routes; at most `ceil(n/64)` visibility statements run under the lock; 513 channels returns `ErrCaptureChannelLimit` | 062 | integration and unit tests; `rbac/role_guard_denial_regression_test.go:380-410` updated |
| 11 | Every denial class (hierarchy, server bits, channel ALLOW, DENY lift, delete, RS4, copy) makes zero `PrepareCapture` calls; the pre-check and the guard agree on a shared fixture table | 066/067/068/062 | spy test replacing the bound-based proof |
| 12 | Every DML site on a permission-input table has a bump scope or a recorded exemption | 063 | writer-registry test |
| 13 | The override routes still let a Manage Channels holder lower a DENY that binds them (recorded as intended) | — | pinning test |
| 14 | Quality gate: 0 new blocker or critical issues, 0 new high-confidence security hotspots, at least 80% coverage on new code; `/verify-quality-gate` passes | — | `.claude/repo-config.json` `.quality_gate`; `.claude/rules/domains/tests.md:763` |
| 15 | **Docs to update** are all updated in the same PR | — | docs review |

## Docs to update

All in the same PR (criterion 15):

- `docs/architecture.md`, Authentication & Authorization: the conferral rule (channel scope,
  category rows at server scope, removal and delete), the generation fence and its writer
  table, grace generations, the shared advisory lock and its stated ordering gap (the MFA
  enforcement toggle), the chunked capture bound.
- `docs/api/openapi.yaml`: the new 403 responses on assign, unassign, delete-role,
  permission-sync, synced move and reorder; the enrollment-required response on unassign; the
  credential-epoch error on unban and invite create.
- `CHANGELOG.md`: an `Unreleased` security entry naming the six VULNs and the behaviour
  changes (new 403s, 500 becomes 200 above 64 voice channels, one step-up re-prompt after
  upgrade).
- `.claude/rules/domains/backend.md`: the #2350 conferral bullet, the #2721 guard bullet, the
  generation-cache residual at 487-513 (closed), the capture bound at 684; recheck the
  hand-kept counts in that file.
- Code comments the change contradicts: the `rbac/cache.go` header; `rbac/role_guard.go:70-79`;
  `rbac/role_guard.go:466-468`; `rbac/handlers.go:2194-2196`; `rbac/authority_tx.go:165-185`;
  the `permgen` package comment.

## Risks

| Risk | Likelihood | Mitigation |
|---|---|---|
| The static DENY rule refuses a legitimate moderation flow (lead call 1) | Medium | Pinning test; owners and Administrators unaffected; operators can grant the bit in that channel |
| Pre-check and guard drift, reopening the occupancy signal | Medium | One verdict function, one resolver function, parity table test |
| Rebase conflicts with Security#170, #167, #168 | High | T0 gate |
| A server bump on every role-targeted override write flushes that server's cache | Medium | Existing route rate limits |
| A writer is missed | Medium | Writer-registry test; the deleted API fails the build |
| Shared lock misuse (upgrade, or a path reaching the exclusive lock) | Low | Helper owns the transaction; allowlist test |
| Invite create aborts its transaction on a code collision | Medium | `ON CONFLICT DO NOTHING RETURNING` |
| A 403 hints that a role has overrides on channels the actor cannot see | Low | Uniform body, no detail |

## Reviewers expected

`security-reviewer`, `red-team` (label `risk: security`), `rbac-reviewer` (files under
`rbac/`), `code-reviewer`, `test-reviewer`, `docs-reviewer`. `e2ee-reviewer` may fire on
content tokens in `authority_tx.go`. No migration reviewer.

## Calls that need the lead

"Accept all" on 2026-11-18 covered D-169a to D-169e. These five arose later, in the panel. The
spec is written to the recommended answer for each; each is isolated so a different answer
changes one task.

1. **Channel-scope rule and its cost.** A role manager cannot assign a role that grants access
   to a channel they cannot use, and cannot remove a restricting role in a channel where they
   are restricted on that same bit. Example: a "Muted" role denies Send everywhere, including an
   announcements channel where the default role is also denied Send; a non-Administrator
   moderator then cannot remove "Muted" from anyone. Recommended: accept. Alternative: ignore a
   lifted DENY bit that the default role also denies in that channel (sound only if every member
   always holds the default role, not verified). Affects T1.
2. **Graces on MFA-only generations (section 4.2).** Touches step-up code outside the issue's
   file list. Recommended: yes. Alternative: accept an MFA prompt after every role or override
   edit. Affects T4.
3. **Override routes recorded as intended (conflict 2).** Needs an explicit yes under the
   no-deferrals policy, because nothing is fixed there: the behaviour is pinned and documented.
4. **Local bypass on total bump failure (section 4.3).** Included under the no-deferrals
   policy. Striking it removes T6 only.
5. **MFA enforcement toggle against unban and invite create.** Ordered by read-after-commit
   only. Recommended: accept. Alternative: add `servers FOR SHARE` in the helper (lock order
   against the toggle is safe: `mfaenforce.LockGateTx` locks users, then servers). Affects T7.

## Left to the builder

- Exact Go names of unexported helpers, file splits inside a package, and test file names,
  provided the exported names in this spec stay.
- The shape of the writer-registry test (AST walk or a grep over non-test files), provided it
  fails on an unlisted DML site.
- Fixture builders for the 65-channel and private-channel scenarios.
- Log field names other than `failure_class`.

## Not verified (T0 checks each before the dependent task starts)

- Every rbac override writer holds the exclusive server lock. Section 1's stability argument
  depends on it.
- Whether a voice temporary grant can target a role.
- That `members/handlers.go:936` and `servers/handlers.go:749` never write `owner_id`.
- Whether the `roles.delete` gate already refuses an unenrolled owner, and whether UpdateRole's
  step-up purpose is grace-eligible.
- `mfa/handlers.go:271` as the MFA factor writer's bump site.
- The exact route for a synced channel move.
- The shape Security#170 lands for add and unban.
- The bodies of Security#97 and Security#98, and the exploit test, which is not in the
  Self-Hosted tree.
- That a new shared advisory request queues behind a waiting exclusive one.
- The account-erasure and server-delete lock orders against the helper's `users FOR SHARE`.
