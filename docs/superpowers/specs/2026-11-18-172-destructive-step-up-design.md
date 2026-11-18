# Security #172 Destructive-Action Gate Design

Status: approved design (Phase 2, 2026-11-18). Issue: Concord-Voice/Security#172 (parent epic
Concord-Voice/Security#152). Code root: `services/control-plane`. Migration: `000201` (the
number the batch reserved; main is at `000197` and batch items 1 to 5 take `000198` to
`000200`, so confirm the next free number when the branch rebases). Lands after the
Concord-Voice/Security#173 change is on main, because both edit `internal/dm/handlers.go`.

## Intent and why

The control plane has two controls that protect message history from a hijacked or careless
session: the step-up gate (password plus MFA) on bulk-destructive routes, and the delete-rate
soft-lock on delete routes. Both are attached per route and per verb. A 2026-11-11 scan found
six ways to destroy existing message content that pass neither control, including one
(VULN-096) where a 1:1 DM participant causes the sweeper to hard-delete the **peer's**
messages. Data loss is permanent once the sweeper runs. The fix makes the gate follow the
effect (loss of existing content), so a cheaper verb is never a weaker door, and adds a
census so a future destructive writer cannot be added ungated.

### Goals

- Close VULN-096, VULN-097, VULN-100, VULN-084, VULN-087 and VULN-101.
- A 1:1 participant can never cause the peer's rows to be newly stamped, shortened, released
  or collected.
- Same effect, same cost: the two new bulk routes cost what DM purge costs.

### Non-goals

- Mutual consent for 1:1 retroactive apply (issue Option C).
- Moving DM purge onto a new seam, or a route-effect registry (epic #55 owns that).
- A new peer-visible system message.
- Remediating rows that a pre-fix 1:1 apply already stamped (see Boundaries).

### Anti-goals (a failure even if the code works)

- Frequent edits of recent messages lock anyone out.
- Any refusal, log field or copy reveals the actor's privacy setting, the soft-lock population,
  the threshold or which factor verified.
- Non-destructive expiration forms or under-threshold edits change bytes on the wire for
  released clients.
- A factor-less first attempt (the normal client path) spends a limiter unit, a password
  attempt or MFA budget.

## How this design was settled (unattended run)

This spec was written in automated mode. The project lead's decisions (D-172a to D-172e,
"Accept all", 2026-11-18) and the design panel's reconciled output answer every question the
brainstorming process would have asked. Recorded answers:

| Question | Answer | Source |
|---|---|---|
| Why now? | Release 1.0 security batch, position 6 of 6; permanent data-loss risk | issue, decisions |
| Which approach (issue Options A, B, C)? | A: shared gate plus author-scoped expiration | D-172a |
| May a 1:1 actor still apply retroactively? | Yes, own rows only, scope persisted for resume | D-172a |
| Does the privacy opt-out apply? | Yes, as DM purge honours it; unknown means required | D-172b |
| Is keep-pins to expire-pins destructive? | Yes, a retroactive apply | D-172c |
| How are edits counted? | Delete soft-lock counter, new edit purposes, only edits of messages older than the grace window | D-172d |
| What does "delete-grace window" mean in code? | `stepup.GraceTTL` (10 min) used as a message-age cutoff; no message-age grace exists in the code | panel (builder call) |
| Client rollout? | Desktop companion in the same window; Mobile calls none of the routes | D-172e, panel check |
| Peer's held pinned stamps when a 1:1 actor stops keeping pins? | Drop them (keeps I-HELD verbatim; the same result is reachable today by clear then set) | panel default |
| Historical peer stamps from pre-fix applies? | Not remediated | panel; open for the lead |
| Where are the VULN-096 exploit tests? | Concord-Voice/Security `scans/2026-11-11-self-hosted-vulnhunt/exploit_tests/` (`dm_expiration_peer_delete_vuln096_test.go`, `..._integration_test.go`, fake driver `vhfakedb_dm_vuln096_test.go`). They run through a `-overlay`; T0 ports them into `internal/dm` as ordinary test files. | located during this run |

## Third-party API notes (Context7, 2026-11-18)

The design reaches the libraries only through existing helpers. Facts the plan relies on:

- **lib/pq v1.12.3:** `pq.Array([]string)` binds a `text[]` that Postgres casts to `uuid[]`
  with `$n::uuid[]`; a nil slice binds SQL NULL, which is what the existing `OnlyIDs` arm
  depends on. `pq.As(err, pqerror.CheckViolation)` (SQLSTATE 23514) is the way the migration
  tests assert that CHECK (b) and CHECK (c) reject a row. A nil `*string` binds NULL, so
  `$8::uuid IS NOT NULL` is the correct guard for `FenceAuthor`.
- **go-redis v9.23.0:** each pipelined command carries its own error; `redis.Nil` is a miss,
  not a failure. The new limiter goes through `middleware.AllowUserAction`, which already
  maps every non-`Nil` error to fail-closed; the tests drive that path with Redis stopped.
- **gin v1.12.0:** body caps use `http.MaxBytesReader` before decoding, as the existing
  expiration and purge handlers do; a DELETE body is read the same way and an empty body is
  allowed.

## Approaches

- **A. Effect-equivalence tiers (chosen).** Each new route joins the family of the existing
  route with the same effect. Cost: two verifier families remain, and the raw password reaches
  two more routes, as it already reaches DM purge.
- **B. One own-rule seam for all six.** Everything confirms in-transaction through
  `VerifyOwnRuleTx` and `current_password` is refused everywhere. Rejected: an MFA account
  would need only MFA for a bulk destroy that DM purge gates with password plus MFA, and the
  issue's acceptance wording asks for `current_password` / `mfa_code`.
- **C. Unify upward.** A new bulk seam, DM purge migrated onto it, and a declarative
  route-effect registry checked at boot. Rejected: it changes DM purge's wire contract and
  mint semantics, and adds a framework inside a security fix; that work belongs to epic #55.

## Goal

Every control-plane route that destroys existing message content (hard delete, retroactive
expiry, group deletion, content overwrite) passes a gate chosen by its **effect**, not by its
route or verb. A 1:1 DM participant can never cause the peer's messages to be deleted. The
own-rule decision is made per row author, not per request shape.

Findings closed: VULN-096, VULN-097, VULN-100, VULN-084, VULN-087, VULN-101.

## Binding decisions (project lead, 2026-11-18)

- **D-172a** A 1:1 retroactive apply is unilateral but touches only the actor's own rows. The
  scope is persisted for resume (migration 000201).
- **D-172b** The two new bulk gates honour `require_auth_before_purge` exactly as DM purge
  does: a missing row or a read error means step-up is required.
- **D-172c** Switching a 1:1 from keeping pins to expiring them is a retroactive apply, so the
  own-rows rule applies.
- **D-172d** Edits reuse the delete soft-lock counter under new edit purposes. Only an edit of
  a message older than the grace window counts.
- **D-172e** A Desktop companion ships in the same window. Three flows fail closed on older
  Desktop builds; that is accepted.

## Chosen approach in detail

Effect-equivalence tiers. Each new route joins the family of the existing route with the same
effect, so the cheaper verb is never the weaker door.

| Effect | Routes | Family | Factors |
|---|---|---|---|
| Bulk destroy | DM expiration (destructive form), group delete | hard, same as DM purge | `current_password` + `mfa_code`, verified before the transaction |
| Single-row overwrite | channel edit, DM edit | own-rule, same as single delete | `mfa_code` or `step_up_token`, verified in the write transaction |
| All-author purge covering own rows | channel purge, server purge | existing soft-lock | unchanged |

Rejected: one own-rule seam for all six (an MFA account would need less for the same bulk
effect than DM purge asks), and a unified seam plus route-effect registry (changes DM purge's
wire contract and adds a framework inside a security fix; that work belongs to the step-up
consolidation epic #55).

## Reconciliation ledger

Each row names a conflict between the design philosophy, the technical approach and the
Desktop handoff, the winner, and why.

| # | Conflict | Winner | Why |
|---|---|---|---|
| R1 | VULN-084: split an all-author spec into a self half and an others half, or add an author fence to the existing spec | **Author fence** (technical approach). The split is withdrawn. | Verified in `internal/messages/purge.go`: `guardChannelPurgeBatch` refuses a spec whose author differs from the guard's (`samePurgeAuthor`, :237), `recheckPurgeAuthorTx` re-derives the planned author, and `purgeSpecProvenance` (:476) turns a self spec into `PurgeExempt`. A split would force changes to D1 provenance and the authority recheck. |
| R2 | Who decides the author scope: the handler, or the service from the locked row | **The service** | It removes the one input a handler bug could widen. CHECK (c) turns any remaining scope bug into a 500, never a wider write. |
| R3 | When the client sends factors: up front from the local privacy setting, or only on a retry after a refusal | **Retry only, on all four gates** | Two facts decide it. A pre-fix server's closed schema answers 400 to factor keys on DM expiration. A pre-fix server refuses an unknown purpose at WebAuthn inline begin, so an up-front prompt would strand security-key users on group delete. Retry-only is also the existing D1 client contract (backend.md, "Client contract (C6)"). |
| R4 | Limiter position: before the step-up read, or after the missing-factor refusal | **After the missing-factor refusal, before any verification** | R3 makes the first send factor-less by design. Charging it would cost two of five hourly units per action. A factor-less request can neither destroy nor guess, so refusing it uncharged loses nothing. |
| R5 | Group delete limiter: swap the router middleware, or charge in the handler | **In the handler**, same helper and order as expiration | Follows from R4: router middleware cannot run after the missing-factor refusal. The existing `RateLimitByUser(5, 1m)` stays as the flood bound. |
| R6 | Limiter bucket: share DM purge's, or a dedicated key | **One dedicated key shared by the two new routes**; DM purge's bucket untouched | Sharing between the new routes stops alternation from doubling the rate. DM purge's key is derived from its route path and its middleware sits ahead of the handler; folding it in would change which purge requests are limited, outside the six findings. |
| R7 | Recheck under the lock: answer a step-up 403, a 409, or a new discriminator | **None reaches the client: the handler re-runs the gate once** | A transition that turns destructive under the lock skipped both the step-up and the limiter. A 403 alone would leave the limiter bypassed when the actor's rule is off, and the actor can drive that race against themselves. Re-running the gate with the classification forced closes both and needs no new wire field. |
| R8 | Purpose name for the DM expiration gate: `dm.expiration_apply` or `dm.expiration_shorten` | **`dm.expiration_shorten`** | Parallel with `channels.expiration_shorten`. "Apply" is wrong for the keep-to-expire pin transition, which is `new_only` on the wire. |
| R9 | Wire shape of the hard factors: two fields, or a third field for the inline WebAuthn token | **Two fields** | DM purge carries the inline token in `mfa_code`; `VerifyMFAFactor` claims it by purpose. |
| R10 | DM purge after the extraction: byte-identical bodies, or add the password cap | **Add the cap** | backend.md already caps `current_password` at 1 KiB in bytes because it bounds the Argon2id input. Bodies stay byte-identical for every in-cap input. |
| R11 | Edit gate presentation: inline strip opening the dangerous-action dialog, or the delete refusal modal | **Delete refusal modal, generalised**; the inline strip is the state after dismissal | The dangerous-action dialog is factor-only and cannot run the password leg an own-rule gate needs. |
| R12 | Peer notification: build a system line, or leave it out | **No new infrastructure** | The route already writes a durable `expiration_event` row in the same transaction and broadcasts it. Only the Desktop wording for that row in a 1:1 needs checking. |
| R13 | Success feedback and copy: toasts and new sentences, or the existing status region and vocabulary | **Existing status region and vocabulary** | An apply is an asynchronous backfill, so "deleted" would be false at that moment. No copy states a threshold, a count or a duration. |

## Design

### 1. Purposes (`internal/stepup/purpose.go`)

All four join `allPurposes`. One purpose per route.

| Purpose | Family | `ownRulePurposes` | `graceEligiblePurposes` |
|---|---|---|---|
| `dm.expiration_shorten` | hard | no | no (irreversible) |
| `dm.group_delete` | hard | no | no (irreversible) |
| `messages.edit` | own-rule | yes | yes |
| `dm.message_edit` | own-rule | yes | yes |

### 2. Step-up seam (`internal/stepup`)

Extracted from `internal/dm/purge.go`:

- `RequireAuthBeforePurge(ctx, q, userID)` reads the actor's setting. A missing row or any
  error means required.
- `VerifyBulkDestroy(ctx, db, rdb, mfaVerifier, userID, route HardRoute{Purpose, Copy}, password, mfaCode) (mfaVerified bool, *Error)`.
  It caps `current_password` at 1 KiB in bytes and `mfa_code` at 256 characters (400 above).
  It writes no gin response; the caller marks a 423.
- With both factors empty it returns the route's refusal and **spends nothing**: no password
  attempt recorded, no MFA budget charge. The bodies are the ones DM purge answers today:
  400 `NoFactors`, 403 with `password_required: true`, 403 with `mfa_required: true`.
- DM purge's `verifyPurgeStepUpWithMFA` becomes a wrapper with the same bodies. DM purge's
  request read gains the 4 KiB body cap.

Route copy (shown by older clients, so it must read on its own):

| Purpose | `CredentialRequired` |
|---|---|
| `dm.expiration_shorten` | Current password required to apply this timer to existing messages. If you are not asked for it, update Concord and try again. |
| `dm.group_delete` | Current password required to delete this group. If you are not asked for it, update Concord and try again. |

`NoFactors` for both follows DM purge's wording. `OwnRuleCopy` gains an edit copy that names
no setting. `ErrMsgEditGuardUnavailable` is a new 503 string: "Editing messages is temporarily
unavailable. Try again in a moment."

### 3. Bulk-destroy gate (one helper, both hard routes)

Run after the route's authorization refusals, in this order:

1. `required := RequireAuthBeforePurge(...)`.
2. `required` and both factors empty: the route's refusal from `VerifyBulkDestroy`. No limiter
   unit.
3. Charge the limiter through `middleware.AllowUserAction` with key
   `ratelimit:user:<uid>:bulk_destroy`, limits from `resolvePurgeRateLimit`. Over: 429. Any
   Redis error: 503.
4. `required`: `VerifyBulkDestroy`. Refusals: 403 invalid credential, 423 locked, 403
   `mfa_required`, 429 or 503 budget.
5. After the write commits and only if MFA verified: clear the MFA budget on
   `stepup.SettleContext`, best-effort.

`dm.Handler` gains `SetBulkDestroyRateLimit(limit, window)`. The boot guard exits if it is
unwired. No new environment variable.

### 4. Migration 000201 (`dm_conversations` only)

Up, in one file:

1. `LOCK TABLE dm_conversations IN ACCESS EXCLUSIVE MODE`.
2. Abandon in-flight 1:1 all-author markers: set `expiration_backfill_mode` and
   `expiration_backfill_cutoff` to NULL where `NOT is_group AND expiration_backfill_mode = 'apply'`.
   No `RAISE`. The revision is not bumped.
3. `ADD COLUMN expiration_backfill_author_id UUID NULL`. **No foreign key**: `SET NULL` would
   violate CHECK (b) and fail account erasure, and `CASCADE` would delete the conversation.
4. `DROP CONSTRAINT dm_conversations_expiration_backfill_mode_check` with no `IF EXISTS`, so a
   name drift fails in CI. Re-add as `CHECK (expiration_backfill_mode IN ('apply','apply_own','clear')) NOT VALID`.
5. Add, both `NOT VALID`:
   - (b) `dm_conversations_backfill_author_shape_check`:
     `(expiration_backfill_mode IS NOT DISTINCT FROM 'apply_own') = (expiration_backfill_author_id IS NOT NULL)`
   - (c) `dm_conversations_backfill_group_apply_check`:
     `is_group OR expiration_backfill_mode IS DISTINCT FROM 'apply'`
6. `COMMENT ON COLUMN`.

`NOT VALID` is permanent: step 2 and the all-NULL new column make every existing row satisfy
the checks, and `NOT VALID` enforces every later write. A migration test asserts zero
violating rows.

Down: lock; clear every `apply_own` marker (mode, cutoff, author), so an older binary can
never resume one as all-author; drop the three checks; re-add `IN ('apply','clear') NOT VALID`;
drop the column.

`apply_own` is storage-only. On the wire `retroactive` stays `"apply"`.

### 5. Author scope (`internal/expiration`)

- `StartConversationTransition(ctx, tx, id, actorID uuid.UUID, req)`. The conversation
  `lockSQL` also returns `is_group` and `expiration_backfill_author_id`. `start()` derives the
  scope from the **locked** `is_group`: a group writes `'apply'` with a NULL author; a 1:1
  writes `'apply_own'` with the actor.
- One new fragment beside `applyEligible`:
  `conversationApplyAuthor = ($5::uuid IS NULL OR user_id = $5::uuid)`. The conversation-scope
  `applyBatchSQL`, `applyEligibleSQL` and `reconcileApplySQL` embed both, which keeps R2
  (batch, probe and start reconcile select one shape).
- `runBatch`, `hasEligibleBackfillRow` and `executeBackfillBatch` gain an `apply_own` arm that
  binds the stored author. Plain `'apply'` on a conversation is honoured only when the locked
  `is_group` is true; otherwise `ErrBackfillNotFound`.
- `clearMarkerSQL` also clears the author and matches it with `IS NOT DISTINCT FROM`.
- Channel scope is unchanged. `policyColumns` becomes per-scope (`NULL::uuid` for channels).
- `resume` stays ungated: it rebuilds its binds only from the stored marker.

Pinned reconcile for a **1:1** (groups unchanged):

| Transition | Actor's pinned rows | Peer's pinned rows |
|---|---|---|
| clear | `dropHeldSQL`, all authors | same |
| set `new_only`, keeps pins | `holdPinnedSQL`, all authors (needed for I-KEEP) | same |
| set `new_only`, stops keeping pins | new `releaseHeldScopedSQL`: held stamp goes back to `expires_at` | same statement drops the held stamp |
| set apply (`apply_own`) | `reconcileApplySQL` with the actor bound | new `peerProtectSQL` (`user_id <> actor`): keeping pins, move stamps `> C` into held; not keeping, drop held |

`duePinnedSQL` gains `($4::uuid IS NULL OR user_id = $4)`, so `ExpireConversationPins` only
receives the actor's ids in a 1:1.

Net property: a 1:1 actor can lengthen the peer's retention (hold, drop) and can never newly
stamp a peer row, move a peer stamp earlier, or release or collect a peer row. I-HELD, I-KEEP
and C-NORES hold after every transition. A peer's pinned row that was already due stays
stamped and the sweeper deletes it as before; the actor neither causes nor accelerates that.

### 6. `PATCH /api/v1/dm/conversations/:id/expiration`

Body: a DM wrapper `dmExpirationRequest{expiration.Request; CurrentPassword, MFACode}`, shaped
like `channelExpirationRequest`. It strips the two keys and delegates to the closed-schema
decoder. The keys are accepted on any mode. Body cap 4 KiB (413 above; was 1 KiB).

Destructive predicate, one exported function used at both points:
`expiration.IsDestructive(req, previous Policy)` =
`mode == "set" && (retroactive == "apply" || (previous.KeepsPins() && include_pinned == true))`.

Order:

1. 400 bad id. 413, then 400 for body or `Validate`.
2. Only when the request could be destructive (`set` with `apply`, or explicit
   `include_pinned: true`): an unlocked pre-read of participant, role and policy. 404 for a
   non-participant or a missing conversation (identical bodies), 403 for a group non-admin,
   409 for a pending backfill, each with the bytes the transaction path writes today.
3. `IsDestructive(req, preRead)`: run the bulk-destroy gate (section 3) with
   `dm.expiration_shorten`. Otherwise skip it; any factor in the body is ignored, neither
   verified nor charged.
4. Transaction, unchanged order: credential epoch 401, authoritative 404/403, start.
5. **Recheck under the lock.** If `IsDestructive(req, transition.Previous)` holds and the gate
   did not run: roll back, run the gate, and repeat step 4 once. At most one repeat.
6. Existing 200/503 and post-commit steps.

`resume`, `clear` and non-destructive `set` forms keep `RateLimitByUser(10, 1m)`, their query
count and their bytes on the wire.

### 7. `DELETE /api/v1/dm/conversations/:id`

Body: optional `{current_password, mfa_code}` through a 4 KiB, exactly-one-object reader. Empty
means no factor. Anything else is 400.

Order: existing router limiter; 400 id; 400 body; existing role and group refusals; the
bulk-destroy gate with `dm.group_delete`; `deleteGroupData` unchanged (it revalidates the admin
under its locks); budget clear after commit.

### 8. Edits (`PATCH /api/v1/messages/:id`, `PATCH /api/v1/dm/conversations/:id/messages/:message_id`)

- Body embeds `stepup.Fields`: optional `mfa_code` / `step_up_token`; a `current_password` key
  is 400. Bodies from released clients are unchanged.
- Population: the "by its author" rows of the soft-lock table. Channel edit: the server rule
  when the server enforces MFA on dangerous actions, otherwise the own rule. DM edit: the own
  rule only.
- Counted only when `created_at < now() - EditSoftLockMinAge`, computed on the database clock
  in the existing unlocked author preflight. `const EditSoftLockMinAge = GraceTTL` (10 min); a
  test pins the equality.
- Same counter, keys and grace scope as deletes (`DMDeleteScope`, `ServerDeleteScope`, the day
  key, `DeleteGraceScope`). Edits and deletes share one budget; a delete-earned grace covers
  an edit in the same scope.
- Sequence mirrors the delete's nine steps, with `HitN` after ciphertext-shape validation. A
  young message, a member outside the population and an edit under the threshold cost no
  Redis call and change nothing.
- Over the threshold: the delete soft-lock's refusal shape, including `delete_rate_limited`
  and `Retry-After`. Redis failure: 503 `ErrMsgEditGuardUnavailable`, `Retry-After: 30`.
- Lock order. Channel: `confirmSoftLockTx` is the first statement of the update transaction
  (users, servers, factor write, then the existing locks). DM: after
  `dmblock.PrepareConversationTx`, the confirmation takes the `GuardFamilyTx` slot as
  `confirmDMDeleteTx` does. `confirmed` is reset at the top of every attempt.

### 9. All-author purge (VULN-084)

- `purge.DeleteSpec.FenceAuthor *string`. In `selectBatch` the id arm becomes
  `($5::uuid[] IS NULL OR id = ANY($5) OR ($8::uuid IS NOT NULL AND user_id IS DISTINCT FROM $8::uuid))`;
  `selectPinned` gets the same change. `nil` keeps today's semantics for `DeleteOne`, expiry
  and ban/kick.
- `gateSelfPurge` counts the specs where `Author == nil || *Author == actor`. `fence` sets
  `OnlyIDs = victims` and `FenceAuthor = &actor` on each.
- The all-author spec stays one D1 spec. `samePurgeAuthor`, `recheckPurgeAuthorTx` and
  `purgeSpecProvenance` are not changed. The request still confirms at most once.
- No wire change.

### 10. Census (root cause)

A static guard test enumerates every writer that destroys message content (`DELETE FROM
messages|dm_messages`, `SET content`, `SET expires_at`, conversation and server deletes) and
classifies each as gated-by, or exempt with a reason. An unclassified writer fails the build.
The writers in `internal/dmblock`, `internal/keyrotation` and `internal/servers` are not yet
classified; any that turns out ungated is fixed in this work, not recorded for later.

## Client contract (binds Desktop)

- **Send a factor only on a retry after the server asked for it.** Never up front, never from
  the local privacy setting. An older server never asks, so every flow keeps working against it.
- Hard routes take `current_password` and `mfa_code` only. Edits take `mfa_code` or
  `step_up_token`, never `current_password`.
- Route on flags, not copy: `password_required`, `mfa_required`, `delete_rate_limited`. The
  one exact string to mirror is `ErrMsgEditGuardUnavailable`.
- Any definite non-2xx from the expiration route means nothing changed. The existing 503 that
  carries a policy body, and transport failures, stay on the refresh path.

Desktop companion (separate repository, same release window):

- `stepUpPurpose.ts`: the four purposes. Edit purposes in both lists; hard purposes in
  `STEP_UP_PURPOSES` only. Add a test that pins the two lists, since none exists today.
- DM expiration editor: 1:1 copy as permanent text ("Apply to my existing messages"; the
  peer's earlier messages keep their current timer). Confirm sends factor-less; a refusal
  hands off to a thin `BulkDestroyStepUpDialog` (password leg always on). The client-side
  destructive classification selects copy only, never whether to send a factor.
- Group delete: remove the native `confirm()`. `DMThreadRemovalDialog` gains a `delete`
  action on its existing stage machine; the credentials stage opens on the refusal. No typed
  group name.
- Edits: `DeleteRefusalModal` generalised with an `action` prop. The draft stays in the
  mounted edit box; after dismissal an inline gate offers "Verify and save". The draft-reset
  effect must treat the gated and blocked states like a failed save.
- Purge modal: no visible change; it must not assume a soft-lock refusal carries
  `target_user_id`.
- The rendering of the `expiration_event` row for a 1:1 apply must say the timer was applied
  to the sender's own earlier messages.
- No new design tokens. Every scheme and theme pair is captured for the three new surfaces.

Mobile calls none of these routes (its API client issues no PATCH or DELETE), so it needs no
change.

## Boundaries

- No change to DM purge's limiter, order or wire contract beyond the input caps.
- No mutual-consent flow, no new message type, no route-effect registry.
- No new environment variable, log field, metric or reason code. Nothing logged reveals the
  setting, the population or which factor verified.
- `expiration.Request` keeps its closed schema.
- Rows a pre-fix 1:1 apply already stamped are **not remediated**. The policy row never
  recorded who applied it, the apply overwrote the peer's earlier stamps, and clearing stamps
  would lengthen retention an author may rely on. The migration only abandons in-flight
  markers.
- Behaviour changes to announce: group delete and the destructive expiration form share a
  5-per-hour fail-closed budget; an in-flight 1:1 retroactive apply stops at upgrade; released
  Desktop builds fail closed on the three flows.

## Work breakdown

| # | Task | Depends on | PR |
|---|---|---|---|
| T0 | Census guard test, red for the six. Port the two reported VULN-096 exploit tests. | none | 1 |
| T1 | `internal/stepup`: four purposes, closed-set tests, copy, bulk helpers extracted, DM purge rewired. | T0 | 1 |
| T2 | Migration 000201 and `internal/expiration` author scope, R2 and pin-matrix tests. | T0 | 1 |
| T3 | `internal/dm/expiration.go`: wrapper, classification, gate, recheck loop. | T1, T2 | 1 |
| T4 | Group delete gate; `SetBulkDestroyRateLimit` wiring and boot guard. | T1 | 1 |
| T5 | `internal/messages/purge.go` and purge engine: `FenceAuthor`. | T0 | 2 |
| T6 | Channel edit soft-lock. | T1 | 2 |
| T7 | DM edit soft-lock. | T1, Concord-Voice/Security#173 on main | 2 |
| T8 | Docs (below), split across both PRs; `docs/api/openapi.yaml` has one writer at a time. | all | 1, 2 |
| T9 | Desktop companion. | PR 1 on main | Desktop |

T1, T2 and T5 can run in parallel; so can T3, T4, T6 and T7 once their inputs exist.

## Acceptance criteria

### Exploit and regression tests (every VULN)

Each finding has at least one test that **fails on the pre-fix code and passes on the fixed
code**. The red run is recorded (test name and failure line) in the PR body before the fix
commit, and each test is checked against the reverted production hunk to prove it is not
vacuous.

| VULN | Test (fails on old code, passes on new) |
|---|---|
| VULN-096 | Ported `TestVuln096_*` (gin plus scripted driver) and the ported integration test: after a 1:1 apply by Bob, Alice's old row keeps `expires_at` NULL and no batch bind lacks the author. Plus the 1:1 mixed-author matrix and the R2 resume test. |
| VULN-097 | Ported `TestVuln097_NoStepUpOnBulkExpiration`: with `require_auth_before_purge = true` and no factors, the destructive form returns the step-up 403 and runs no write. Plus the step-up matrix and the recheck-loop test. |
| VULN-100 | Group delete with the rule on and no or wrong factors returns the step-up refusal and deletes nothing; the bulk-destroy limiter is charged once per verified attempt and fails closed when Redis is down. |
| VULN-084 | An all-author purge with no `target_user_id` that covers the actor's rows calls `HitN` with that count and trips the soft-lock at the threshold; a post-count actor message survives; other authors' rows are still deleted. |
| VULN-087 | The 16th edit of a message older than `EditSoftLockMinAge` inside the window returns 403 `delete_rate_limited`; a young-message edit succeeds with Redis down. |
| VULN-101 | Same as VULN-087 on the DM edit route, plus the lock-order and single-spend retry test. |

Functional criteria:

- A 1:1 participant who sets a retroactive policy, or stops keeping pins, cannot newly stamp,
  shorten, release or collect a row authored by the other participant. Hold and drop, which
  only lengthen retention, stay all-author.
- A resumed 1:1 backfill, by either participant, touches only the stored author's rows, and
  `backfill_pending` clears.
- With the actor's rule on, the destructive expiration form and group delete return the
  step-up refusal when the factors are missing or wrong, and run no write. With the rule off
  they run without factors.
- A factor-less refused request costs no limiter unit, no password attempt and no MFA budget.
  A Redis failure on the destructive form or on group delete is a 503; `new_only`, `clear`
  and `resume` stay reachable.
- A transition that becomes destructive under the lock never commits ungated or uncharged.
- An all-author purge confirms at most once per request.
- An edit of a younger message never counts; edits and deletes share one budget and one grace.
- Non-destructive expiration forms and under-threshold edits are byte-compatible for released
  clients.
- The census guard test classifies every content-destroying writer; it was red for the six
  before the fix.

### Suites

- Focused: `go test -count=1 ./internal/stepup/... ./internal/expiration/... ./internal/dm/... ./internal/messages/... ./internal/purge/... ./internal/api/... ./migrations/...`
  pass, including the integration tests with a disposable `DATABASE_URL` and `REDIS_URL`.
- Full: `go test ./...` and `go vet ./...` from `services/control-plane` pass, and the repo's
  lint and quality gate (`/verify-quality-gate`) is green on the PR.

### Quality gate

| Criterion | Threshold |
|---|---|
| New blocker or critical issues | 0 |
| New high-confidence security hotspots | 0 |
| Coverage on new code | at least 80% |
| Docs to update | every item in "Docs to update" ships in the PR that changes the behaviour |

### Docs to update

This is an acceptance criterion: the PR that changes a behaviour carries its doc change.

- `docs/api/openapi.yaml`: the four purposes in the enum; factor fields and refusals on DM
  expiration and group delete; the 4 KiB cap; step-up fields and the soft-lock refusal on both
  edit routes; the all-author purge soft-lock note. One writer at a time.
- `docs/architecture.md`: the `dm_conversations` entity (`expiration_backfill_author_id`, mode
  `apply_own`), the step-up summary line (bulk-destroy routes now share the purge budget), and
  the `expiration` package row (author-scoped 1:1 backfill).
- `CHANGELOG.md`: the fixes and the three behaviour changes listed under Boundaries.
- `docs/design/authenticated-step-up-budget.md`: the routes behind the destructive-action gate.
- `.claude/rules/domains/backend.md`: a new "gate by effect" rule with the census; the
  soft-lock section (routes covered, edit rows and the all-author purge row in the population
  table, `EditSoftLockMinAge`, the "ways around the soft-lock that remain" sentence
  rewritten); the step-up tokens section (two new own-rule purposes, the two new hard routes,
  the retry-only client contract).
- `.claude/rules/domains/migrations.md`: a 000201 section and a per-author note in the
  pin-aware section. The invariant header of `internal/expiration/pinned.go` changes with it.
- `CLAUDE.md` counts through `./scripts/update-claude-md-counts.sh`.
- Desktop: its own changelog and step-up documentation, in the companion PR.

## Proof

Integration, against disposable PostgreSQL and Redis:

- 1:1 mixed-author matrix: previous keep or not, new keep or not, apply / `new_only` / clear,
  with pinned, held and due rows. Every peer row's `expires_at` is NULL or unchanged; no peer
  id appears in `DuePinnedIDs`; I-HELD and I-KEEP hold after every transition.
- R2: a 1:1 `apply_own` over more than 5,000 mixed-author rows resumes to completion; resume
  converges after the author's account is erased; a group apply still covers every author.
- Migration up and down: an in-flight 1:1 marker is abandoned; the constraint names exist;
  (b) and (c) reject their violations; down clears `apply_own` markers; zero violating rows.
- Lock order: channel edit against concurrent member removal; DM edit against the participant
  prefix; a retried attempt never spends a factor twice.
- All-author purge counting, the post-count survivor, and unchanged ban/kick and `DeleteOne`.

Unit and handler:

- `IsDestructive` table (omitted, true, false against keep and not-keep).
- Wrapper decode: keys stripped, closed schema kept, null refused, caps.
- Step-up matrix: missing, wrong and correct password and MFA; rule off; missing settings row;
  read error.
- Gate order: a factor-less refusal leaves the limiter key, the attempt count and the MFA
  budget untouched; the factor-less body equals `VerifyBulkDestroy`'s empty-input body for
  each account shape.
- Recheck loop, driven by a hook between the pre-read and the transaction, with the rule on
  and with it off; the limiter is charged in both.
- Edits: 15 deletes plus one old edit trips the lock; a delete grace covers an edit; a
  `current_password` key is 400.
- Static: the three conversation statements embed both fragments; the handler classifies only
  through `IsDestructive`; the closed-set purpose tests; the OpenAPI contract test.
- Router: the edit route limiter still runs before `HitN`; the boot guard refuses an unwired
  bulk-destroy limit.

## Risks

| Risk | Mitigation |
|---|---|
| The two fragments drift and `backfill_pending` never clears | Static embed test and the 5,000-row integration test |
| Author-scoped reconcile breaks I-HELD or I-KEEP | Protective complement for peer rows; invariant assertions after every matrix cell |
| Scope widens on resume, rollback or an older binary | `apply_own`, CHECK (b) and (c), down migration clears scoped markers; an older binary reads `apply_own` as not found and fails closed |
| The recheck loop double-spends a factor | The gate runs at most once per request; a test pins one verification and one limiter unit |
| Edit confirmation breaks lock order | Confirmation as the first statement through the existing helper; lock-order tests |
| A released Desktop loses three flows | Same-window companion; route copy that reads on its own |
| An unclassified destructive writer remains | The census fails the build |

## Open for the lead

- Rows a pre-fix 1:1 apply already stamped on the peer's messages are not remediated (see
  Boundaries). This is a new call outside D-172a to D-172e.

## Not verified

- The exact `error` strings of the soft-lock refusal for the edit variant.
- Whether an empty-input call into the extracted password reservation touches Redis; the gate
  order test decides it.
- Whether old and new binaries ever serve concurrently. If they do, CHECK (c) makes an older
  binary's 1:1 apply a 500 and its resume of an `apply_own` marker a not-found; both fail closed.
- `expiration.EventFor`'s payload and the Desktop renderer for that row.
- `deleteGroupRowsTx` lock order (no statement is added inside that transaction).
