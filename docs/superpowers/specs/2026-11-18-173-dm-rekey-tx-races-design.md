# Security #173: DM re-key authority, rotation budget, friend and personal-thread races, DM voice lock order

- **Issue:** Concord-Voice/Security#173 (parent epic Concord-Voice/Security#152)
- **Repository:** Concord-Voice/Self-Hosted, control plane
- **Branch:** `feature/security-173-dm-rekey-tx-races`
- **Base:** `6ba00d24` (line numbers below are from this commit)
- **Status:** design approved in automated mode. The project lead accepted every
  Phase-1 recommendation on 2026-11-18 ("Accept all"), and the design panel
  reconciled the result. Where this document records an answer to a design
  question, the source is the decision record (D-173a to D-173d) or the panel.

## 1. Intent

Six control-plane flows decide authority, a budget or uniqueness outside the
transaction that commits the write, or take locks in an inconsistent order. The
2026-11-11 scan confirmed all six, and all six are still open at the base commit.

| VULN | Severity | Defect |
|---|---|---|
| VULN-082 | Medium | `POST /api/v1/e2ee/keys/:context_id` admits a group successor claim in `admitDMSuccessorClaimTx` (`internal/channels/handlers.go:4650`) with no creator/admin rule and no budget charge. The rule lives only in `dmRotationAuthority` on the rotate-key route, on the pool. |
| VULN-083 | Low | The rotate-key budget is a Redis read before the transaction (`dm_rotation.go:165`) and a spend after commit (`:181`). It fails open on a Redis error (`:223`). |
| VULN-094 | Low | `GetOrCreatePersonalThread` (`internal/dm/handlers.go:3250`) checks on the pool and inserts in a separate transaction. Parallel requests make two personal threads. |
| VULN-118 | Low | `SendRequest` (`internal/friends/handlers.go:417`) checks the pair with a pre-read. The schema has only a directional unique constraint, so mirrored rows can exist. |
| VULN-119 | Low | Decline deletes by id only (`friends/handlers.go:652`). A decline that races a block can erase the block row. |
| VULN-104 | Low | DM voice join (`dm/handlers.go:2997`) and abort (`:5413`) take row locks before the process-local lifecycle mutex. The other seven call sites take the mutex first. Postgres cannot see the cycle, so it resolves only by lock timeout. |

**Why.** The root cause is that rules and budgets live in route handlers, and
uniqueness lives in pre-reads. Every write in these flows must re-check its
authority and budget in the transaction that commits it, the schema must
enforce pair and personal-thread uniqueness, and DM voice must have one
documented lock order.

**Success.** Every VULN has a regression or exploit test that fails on the base
commit and passes on the new code, and no existing behaviour that the decisions
keep changes.

## 2. Goals, non-goals, anti-goals

**Goals**

- One authority rule for group re-keys, checked under the conversation lock on
  both routes.
- One budget, counted in Postgres inside the locked transaction, that fails
  closed.
- Schema-enforced uniqueness for friend pairs and personal threads, with a
  silent dedupe of existing rows.
- One lock order for DM voice: lifecycle mutex, then `BEGIN`, then row locks.

**Non-goals**

- No Desktop or Mobile change (D-173a).
- No new route and no entitlement or tier change.
- No reconnect re-announce of rotation cues (the cue table makes it possible
  later; it is not in scope).
- No redesign of the striped mutex into an advisory lock.
- Sending messages into a demoted personal thread is not tested beyond listing.

**Anti-goals (a failure even if the tests pass)**

- A migration that stops a self-hosted instance from booting (a RAISE, or an
  UPDATE of a blocked friendship that trips the 000153 reconciliation trigger).
- A removed member who keeps reading new messages until an admin comes online.
- A refused rotation that spends the budget.
- A ledger row that commits ahead of the successor wraps (the 2026-09-17
  lockout class).
- A new existence oracle: a non-participant must still get 404 before any new
  403 or 429.
- A change to the `SendRequest` timing discipline documented in its comments.

## 3. Constraints

| Constraint | Source |
|---|---|
| Group successor claim: creator or admin, or any current-epoch holder while a persisted cue is pending. 1:1 is unchanged. The rotate-key route is always manual and never accepts a cue. | D-173a |
| Budget counted from `dm_key_revocations` inside the locked transaction, no Redis in the budget path. Cued rotations are not charged. | D-173b. `revoked_at TIMESTAMPTZ NOT NULL DEFAULT NOW()` exists (migration 000041), so no schema change. |
| Only a commit writes a ledger row, so a DB count never charges a refusal. | D-173b |
| The ledger row commits with the successor wraps. The cue is not a `dm_key_revocations` row. | `recordDMSuccessorRevocationTx` (`handlers.go:4581`) |
| Silent dedupe, no RAISE. Personal duplicates: keep the oldest, set the rest to `is_personal = FALSE`, delete nothing. Friend pairs: blocked beats accepted beats pending. | D-173c |
| The friend-pair fold is delete-only. Triggers from 000146 and 000153 fire on INSERT or UPDATE only; an UPDATE of a blocked row without reconciliation evidence raises `check_violation` at commit. | `000146:139`, `000153:76-116` |
| One migration file is one transaction: no `CREATE INDEX CONCURRENTLY`. Down migrations drop the index and state that the fold is irreversible. | `.claude/rules/migrations.md`, `.claude/rules/domains/migrations.md` |
| `SendRequest` keeps its order: the eligibility read is unconditional and runs before the pair check; the race loser answers through `friendshipConflictResponse`. No presence capture on pending inserts. | `friends/handlers.go:437-470` comments |
| Lock order users, then conversation parent, then children is unchanged. New reads run after the conversation is locked. | `.claude/rules/domains/backend.md` |
| Three PRs in this repository, landed in migration-number order. | D-173d |
| Migration numbers: this issue takes 000199, 000200 and 000201. Concord-Voice/Security#172 moves to 000202. | Panel R1 (the persisted cue is a third piece of durable state) |

## 4. Approaches

The decisions fixed the behaviour. The open question was how the rotation cue
is produced, plus the VULN-104 lock direction and the budget's charged set.

**Cue source**

| Option | How | Trade-off |
|---|---|---|
| 1. Cue table, explicit writer helper (**picked**) | Table `dm_rotation_cues`; a leaf package with `Record`, `Pending`, `Consume`; the three group topology writers call `Record` in the transaction that changes membership. | Precise reasons in the ledger; sits where each writer already reads the epoch for its WebSocket cue. A future writer can forget the call; a static allow-list test guards it. |
| 2. Trigger on `dm_participants` | AFTER INSERT OR DELETE upserts a cue for groups with a current epoch. | No writer can skip it, but the reason is coarse (left vs removed vs blocked), cascade ordering is subtle, and the logic is hidden from Go readers. |
| 3. Derived cue, no state | Infer "rotation pending" from wraps vs participants under the lock. | No migration, but it is not the persisted cue D-173a names, it rests on incidental data, and it over-matches ordinary pending re-wraps. |

Why 1: only three group topology writers exist (add member near
`dm/handlers.go:1644`, `removeMemberRows` at `:2172`, the block reconciler in
`internal/dmblock/dmblock.go` near `:1184-1242`), each already emits the
WebSocket cue, and none is unable to call a helper. Option 3's predicate is used
once, as the migration backfill.

**VULN-104 direction**

| Option | Change | Trade-off |
|---|---|---|
| L1. Mutex, then DB, everywhere (**picked**) | Move the mutex ahead of `BEGIN` at join and abort (2 sites). | Longer critical section on join; stripe head-of-line blocking remains. |
| L2. DB, then mutex, everywhere | Invert 7 sites, 4 in `internal/voice/nats.go`. | Large and risky; ring has no transaction. |
| L3. Advisory lock | Replace the mutex. | Redesign territory (Self-Hosted#91 ADR); oversized for a Low finding. |

Why L1: exactly nine call sites exist and seven are already mutex-first. None of
the functions that move under the mutex calls `LockDMCallLifecycle` (the mutex is
not reentrant).

**Budget charged set:** a reason list on the existing `reason` column (picked),
not a new `charged` column. No consumer reads the ledger reason; every reader
checks existence or `successor_epoch`.

## 5. Design

### 5.1 PR-A: friends and personal thread (VULN-118, VULN-119, VULN-094)

**Migration 000199** (`friendships`):

1. `LOCK TABLE public.friendships IN SHARE ROW EXCLUSIVE MODE`.
2. Delete-only fold. Per `(LEAST(requester_id, addressee_id), GREATEST(...))`
   pair, rank rows by status (blocked, then accepted, then anything else), then
   `created_at`, then `id`. Keep rank 1, delete the rest. Never UPDATE a row.
3. `CREATE UNIQUE INDEX idx_friendships_pair_unique ON friendships
   (LEAST(requester_id, addressee_id), GREATEST(requester_id, addressee_id))`
   with `-- squawk-ignore require-concurrent-index-creation` and a reason.
4. Down: drop the index; a comment states the fold is irreversible.

"The blocker's direction is kept" (D-173c), as resolved by the panel (R2):
`friendships` has no blocker column, and the block path updates existing rows in
place, so the fold never rewrites a blocked row and the survivor keeps its
stored direction. If both mirrored rows are blocked, the older survives
(`created_at`, then `id`). No unblock route exists; the only production deletes
are decline and remove-friend (accepted only).

**Migration 000200** (`dm_conversations`): per `created_by`, keep the oldest
`is_personal` row (`created_at`, then `id`) and set the rest to
`is_personal = FALSE`. Then `CREATE UNIQUE INDEX idx_dm_conversations_one_personal
ON dm_conversations (created_by) WHERE is_personal` with the squawk exemption.
Down drops the index. Demoted rows are not hidden: the retirement sweeper deletes
hidden, empty non-personal conversations (`dm/retirement_sweeper.go:19-25`), and
hiding would turn "nothing is deleted" into a later delete (R9).

The rule file says a fold must RAISE on collision; D-173c overrides it for these
two migrations, because the fold removes every collision before the index is
built and `CREATE UNIQUE INDEX` remains the fail-closed backstop (R3). The
exception is recorded in `.claude/rules/domains/migrations.md`.

**Handlers**

- `SendRequest`: order unchanged. The insert becomes
  `INSERT ... ON CONFLICT DO NOTHING RETURNING id, created_at`. On no row,
  re-read the pair and answer through `friendshipConflictResponse`; if the row
  is gone, answer 409 pending. No notification on that path. No wrapping
  transaction: the insert is the arbiter (R4).
- `executeFriendCodeClaim` (`:1470`, insert near `:1503`): same insert. On
  conflict, abandon the capture plan, return a `claimRejection` built from
  `friendshipConflictResponse`, and do not increment `use_count`.
- `executeBlockTxWithAudience` (`:820`, insert near `:877`):
  `ON CONFLICT DO NOTHING`; zero rows returns `presencehook.ErrProbeStale`
  (the existing retryable, fail-closed terminal). Not an upsert: presence
  capture runs before the write, and an upsert onto a row committed after
  capture could revoke an audience the capture never saw (R5).
- Decline (`:652`): `DELETE FROM friendships WHERE id = $1 AND addressee_id = $2
  AND status = 'pending'`; zero rows returns 404 "Friend request not found".
- `GetOrCreatePersonalThread`: look up by `created_by = caller AND is_personal`.
  In the create transaction, insert with `ON CONFLICT DO NOTHING RETURNING id`;
  on no row, select the existing id; insert the participant with
  `ON CONFLICT DO NOTHING`. A nil `fetchConversationResponse` returns 500 and
  never falls through to a second create. 201 for the inserter, 200 otherwise.

### 5.2 PR-B: re-key authority, cue and budget (VULN-082, VULN-083)

**Migration 000201:**

```sql
CREATE TABLE dm_rotation_cues (
  conversation_id UUID PRIMARY KEY REFERENCES dm_conversations(id) ON DELETE CASCADE,
  cued_epoch      INTEGER NOT NULL CHECK (cued_epoch > 0),
  reason          TEXT NOT NULL,
  cued_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

Backfill (R8): insert a cue with reason `upgrade_backfill` at `MAX(key_version)`
for each group whose holder set at that epoch differs from its participant set.
Remove and leave keep the leaver's wraps, so this finds in-flight removals. A
block removal deletes the wraps, so an in-flight block cue at upgrade time is
missed (accepted; it clears at the next admin rotation). A blanket backfill was
rejected: it would hand every member of every group one free re-key.

Down drops the table. The table joins the explicit child-delete lists
(`groupDeleteStatements` in `dm/group_handlers.go:59-68`, `dmblock.go:1319-1327`)
and the truncate list in `internal/testhelpers/testdb/testdb.go` (near 323).

**New leaf package `internal/dmrotationcue`** (imports `context`,
`database/sql`, `fmt` only, so `dm`, `dmblock` and `channels` can all import it):

- Reasons: `member_added`, `member_removed`, `member_left`, `user_blocked`,
  `upgrade_backfill`.
- `Record(ctx, tx, conversationID string, epoch int, reason string) error`:
  no-op when `epoch <= 0`; rejects unknown reasons; upsert keeps the greater
  epoch.
- `Pending(ctx, tx, conversationID string, epoch int) (reason string, pending bool, err error)`:
  true when a row has `cued_epoch = epoch`.
- `Consume(ctx, tx, conversationID string, throughEpoch int) error`: deletes rows
  with `cued_epoch <= throughEpoch`.
- `Uncharged() []string`: the reason list the budget query excludes.

**Cue writers**, each in the transaction that changes membership, using the
epoch it already reads, after a row lock on the conversation parent: add member
(epoch read near `dm/handlers.go:1655`), `removeMemberRows` (`:2172`; removed vs
left), and the dmblock reconciler (reason `user_blocked`).

**Admission**, in the shared body under the conversation `FOR UPDATE` lock. Add
`manualRotation bool` to `dmDistributionBatch` (`handlers.go:4435`), set only by
`rotateDMKeyTx` (`dm_rotation.go:261`). For a successor claim with
`previousVersion > 0`:

1. Stale epoch: 409 (existing). A race loser never sees a new error.
2. Not a holder: 403 (existing).
3. **Authority.** Allowed if the conversation is 1:1, or the actor is creator or
   admin. Otherwise allowed only when `!manualRotation` and either a cue is
   pending at `previousVersion` or AC-1 applies. Else a typed error mapped to
   403 `errMsgRotationGroupAdminOnly`.
4. Incomplete batch: 400 (existing).
5. **Budget**, skipped when the claim is cued (`!manualRotation` and a cue is
   pending, any role) or AC-1 applies. Count ledger rows for the conversation
   with `revoked_at > NOW() - interval '24 hours'` and `reason` not in
   `Uncharged()`. At `dmRotateLimit` (10) or more, a typed budget error carrying
   Retry-After (from the oldest counted row + 24 h) maps to 429 through
   `middleware.RespondRateLimited`. Unknown reasons count as charged.
6. **On commit:** the ledger reason is the cue reason when cued, otherwise
   today's value. `Consume(previousVersion)` runs on every committed successor
   claim from either route.

**AC-1 (panel architect call, recorded as accepted under "Accept all"):** a
non-admin may also finish a rotation when a ledger row already exists for the
current epoch with no successor (the client's revoked-epoch self-heal). That
epoch is already unusable, the claim writes no new ledger row (`ON CONFLICT DO
NOTHING`), and no production path creates the state. Without it, legacy-stuck
groups wait for an admin. It can be vetoed by deleting one clause.

**Supporting changes**

- One authority predicate serves the pool pre-check in `RotateDMKey` and the
  in-transaction gate; `dmRotationAuthority` delegates to it.
- Both handlers check the budget error before `keyDistributionErrorResponse`
  (`handlers.go:2794`), because that mapper cannot set headers.
- Delete `dmRotateBudgetExhausted`, `consumeDMRotateBudget` and the
  `ratelimit:dm_rotate:` key. The unified route's Redis attempt limiter stays
  (it limits attempts; it is not the budget).
- The initial epoch (`previousVersion == 0`) is untouched.
- A non-participant still gets 404 before any new 403 or 429.

**Bounding cue minting:** `AddMember` is admin-only, a leaver cannot claim, and
a block removes a real member. One row per conversation means N membership
changes before a claim buy one uncharged re-key.

### 5.3 PR-C: DM voice lock order (VULN-104)

- `authorizeVoiceJoinTx` (`dm/handlers.go:2953`): take the mutex before
  `beginDMVoiceMembershipAuthorityTx`. If begin fails, unlock and return.
  Register the unlock defer after the rollback/retract defer, so retraction
  still runs outside the mutex as today.
- `AbortDMVoiceMediaAuthorization` (`:5372`): take the mutex after
  `loadDMVoiceAuthorizeIdentity` and before `beginDMTopologyEffectWithCredential`.
  Release on the begin-error return.
- Documented order: lifecycle mutex, then `BEGIN`, then users, conversation
  parent, children. Stripes stay.

## 6. Tests

- **PR-A:** migration tests (pattern of `internal/database/migration_000179_test.go`)
  seeding mirrored rows (blocked/accepted, blocked/blocked, accepted/pending) and
  duplicate personal threads; parallel mirrored requests leave one row; parallel
  personal creates return one id; decline racing a block leaves the block row;
  decline of a non-pending or non-addressed row is 404 with the row intact; a
  claim conflict leaves `use_count` unchanged; a demoted thread's messages still
  list for its owner.
- **PR-B:** a port of the scan's `dm_successor_claim_vuln082_test.go` as a
  regression (the exploit succeeds on the base commit; the regression asserts it
  is refused), plus the authority matrix (group non-admin, admin, creator, 1:1;
  unified and rotate routes; cued, uncued, AC-1); cue lifecycle (written by each
  topology change, consumed by any committed claim, never honoured on
  rotate-key); budget (seed `revoked_at` directly; 10 charged rows give 429 on
  both routes; cued claims exempt; parallel rotations cannot exceed the limit; a
  Redis outage changes nothing); lost race (the loser's retry gets 403 as
  non-admin, one charged row as admin); a static test enumerating
  `dm_participants` insert and delete sites against an allow-list.
- **PR-C:** a two-goroutine test proving the join held its row locks while
  `handlePrivateVoiceJoined` waited on the mutex, then completed without a lock
  timeout (on the base commit it hits the timeout); a static guard that each
  `LockDMCallLifecycle` call precedes any begin in its function.

## 7. Acceptance criteria

| Criterion | Target |
|---|---|
| Every VULN in the issue (082, 083, 094, 118, 119, 104) | has an exploit or regression test that fails on the base commit and passes on the new code |
| Focused package suites (`internal/channels`, `internal/dm`, `internal/dmblock`, `internal/friends`, `internal/database`, `internal/dmrotationcue`, `internal/voice`) | pass with a disposable `DATABASE_URL` / `REDIS_URL` |
| Full affected control-plane suite (`go test ./...` under `services/control-plane`) | passes |
| `/verify-quality-gate` (lint, vet, tests) | green |
| New blocker or critical issues / new high-confidence hotspots | 0 / 0 |
| New-code coverage | at least 80% |
| Group non-admin successor claim on `/e2ee/keys/:context_id`, uncued | 403 |
| Same claim with a pending cue | 200; ledger reason is the cue reason; not charged |
| Creator or admin; 1:1 participant | 200 |
| Non-admin on rotate-key, cue present or not | 403 |
| 11th charged re-key in 24 h, either route, including under concurrency | 429 with Retry-After; a Redis outage changes nothing |
| Parallel personal-thread creates | one id |
| Parallel mirrored friend requests | one row per pair |
| Decline of a non-pending or non-addressed row | 404, row intact |
| Two-goroutine lock-order test | completes without lock timeout, window proven |
| Migrations on an instance with duplicates, including blocked mirrored rows | boot with no error |
| **Docs to update** | `docs/api/openapi.yaml`, `docs/architecture.md` and `CHANGELOG.md` are updated wherever behaviour changes, and every item in section 8 ships in the PR that changes it |

## 8. Docs to update

- **PR-A:** `docs/api/openapi.yaml` (friend request and personal-thread conflict
  responses, decline 404); `services/control-plane/migrations/README.md` (header
  range and rows); `.claude/rules/domains/migrations.md` (Key Tables, the no-RAISE
  fold exception); `CHANGELOG.md`.
- **PR-B:** `docs/api/openapi.yaml` (403 and 429 on `/e2ee/keys/{context_id}`,
  DB-counted 429 on rotate-key); a new "DM rotation authority, cue and budget"
  section in `.claude/rules/domains/backend.md`; migrations README and Key Tables
  entry for `dm_rotation_cues`; the stale budget comment at
  `channels/dm_rotation.go:156-163`; `CHANGELOG.md`. The PR body narrows the
  overlapping part of Concord-Voice/Security#46.
- **PR-C:** the `LockDMCallLifecycle` comment in `dm/voicering.go`; a new DM voice
  lock-order section in `.claude/rules/domains/backend.md`; `docs/architecture.md`
  (near line 329); `CHANGELOG.md`.

## 9. Risks

| Risk | Mitigation |
|---|---|
| A later migration number lands first and an instance on main skips the earlier one | Land A, then B, then C. If B is ready first, renumber in landing order. |
| An admin who loses a cue race retries once at the next epoch; that redundant re-key is charged | Covered by the lost-race test; accepted. |
| A future topology writer omits the cue | Static allow-list test over `dm_participants` writers. |
| The pair fold trips the reconciliation trigger | Delete-only fold; migration test with blocked mirrored rows. |
| squawk rejects the plain unique indexes | Sanctioned ignore comment with a reason. |
| A demoted one-person conversation misbehaves in list paths | List test in PR-A. |
| Drift from batch predecessors (Concord-Voice/Security#167 broadcast helpers, Concord-Voice/Security#172 group delete list) | Rebase B on landed main; B owns the delete-list entry. |
| Existing `successor_claim` rows count as charged for 24 h after deploy | Documented; self-clearing. |

## 10. Left to the builder

The panel did not verify these. The builder settles them while implementing;
a result that contradicts this design stops the task and is reported.

- The parent-lock mode held by the remove/leave and block paths where the epoch
  is read (add-member holds `FOR SHARE OF dc, dp`). If it is weaker than a row
  lock, add `FOR SHARE` on the parent before reading the epoch.
- Whether each `status = 'blocked'` reader ignores direction.
- That golang-migrate applies numbers in order from the stored version.
- Whether squawk flags a plain unique index under `.squawk.toml`.
- The primary key of `dm_participants` (for the `ON CONFLICT` target) and that
  `dm_conversations.created_at` exists.
- The `is_personal` branch in `internal/websocket/hub.go` (near 5713) for demoted
  threads.
- Lifecycle mutex use more than one call deep, or outside `dm` and `voice`.
- Exact names of new typed errors, test files and helper functions beyond those
  this document fixes.

## 11. Calls made in automated mode

- Migration numbers 000199-000201 for this issue, 000202 for Concord-Voice/Security#172 (R1).
- R2 reading of "the blocker's direction is kept".
- Block insert stays `DO NOTHING` + `ErrProbeStale`, not an upsert (R5).
- AC-1 included.
- Demoted personal threads are not hidden (R9).
- Third-party behaviour checked against current docs (2026-11-18): Postgres
  `INSERT ... ON CONFLICT DO NOTHING RETURNING` returns only inserted rows;
  `FOR UPDATE` conflicts with `FOR NO KEY UPDATE`; lib/pq v1.12.3 exposes
  `pq.As(err, codes...)` for SQLSTATE checks; gin v1.12.0 `c.Header` sets
  response headers before the body is written.
