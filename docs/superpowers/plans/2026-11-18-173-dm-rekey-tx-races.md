# Security #173 DM Re-key, Budget, Friend and Lock-Order Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move DM re-key authority and the rotation budget into the locked write transaction, enforce friend-pair and personal-thread uniqueness in the schema, and give DM voice one lock order (VULN-082, 083, 094, 118, 119, 104).

**Architecture:** Three PRs on one branch family, landed A then B then C. PR-A adds two dedupe-plus-unique-index migrations and makes the inserts the arbiter. PR-B adds a `dm_rotation_cues` table, a leaf `dmrotationcue` package, three cue writers and an admission gate (authority + DB-counted budget) inside `distributeDMKeysTx`. PR-C moves the lifecycle mutex ahead of `BEGIN` at join and abort.

**Tech Stack:** Go 1.27.4, gin v1.12.0, lib/pq v1.12.3, go-redis v9.23.0, Postgres, golang-migrate, testify, `internal/testhelpers` real-Postgres harness.

**Spec:** `docs/superpowers/specs/2026-11-18-173-dm-rekey-tx-races-design.md`

## Global Constraints

- All paths below are relative to `services/control-plane/` unless they start with `docs/`, `.claude/` or `CHANGELOG.md` (repository root).
- Migration numbers: 000199 (friend pairs), 000200 (personal thread), 000201 (rotation cues). Each `.up.sql` is one transaction: no `CONCURRENTLY`, no RAISE in the folds.
- Friend-pair fold is DELETE-only. Never UPDATE a `friendships` row in a migration.
- Personal fold sets `is_personal = FALSE` only. It never deletes and never sets `hidden_at`.
- Plain unique indexes carry `-- squawk-ignore require-concurrent-index-creation` with a one-line reason.
- Budget: `dmRotateLimit = 10` per 24 h per conversation, counted from `dm_key_revocations.revoked_at`; unknown reasons count as charged; no Redis in the budget path.
- Cue reasons (exact strings): `member_added`, `member_removed`, `member_left`, `user_blocked`, `upgrade_backfill`.
- Authority refusal message: existing `errMsgRotationGroupAdminOnly` = "Only the group's creator or an admin can rotate its key".
- Error order on a successor claim: 404 non-participant, 409 stale, 403 holder, 403 authority, 400 incomplete, 429 budget.
- Decline 404 text: "Friend request not found".
- Lock order: lifecycle mutex, then `BEGIN`, then users, conversation parent, children.
- Tests that touch Postgres or Redis run with a disposable `DATABASE_URL` / `REDIS_URL`. Never set a private `GOCACHE`/`GOMODCACHE`.
- Commits: Conventional Commits, bare `type:`; end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Public repository: no references to private repositories, their paths or numbers; backtick any `@agent` name.
- Each test listed as a VULN regression must be shown failing on the base commit `6ba00d24` before the fix (record the failing output in the task's commit body or PR notes).

## Review Focus

1. **Block arrives between decline's pre-read and its delete.** The block row must survive and decline answers 404. Pinned in Task 4 (`TestDecline_RacingBlockKeepsBlockRow`).
2. **Friend-code claim loses the insert race.** `use_count` must stay unchanged and the caller sees the conflict answer, not 500. Pinned in Task 3 (`TestFriendCodeClaim_ConflictDoesNotSpendUse`).
3. **Personal thread exists but its response fetch fails.** Must return 500, never create a second thread. Pinned in Task 5 (`TestPersonalThread_FetchFailureDoesNotCreateSecond`).
4. **Cue present but the claim arrives on rotate-key.** A non-admin is still refused, and the cue survives. Pinned in Task 10 (`TestRotateDMKey_IgnoresCueForNonAdmin`).
5. **Redis down during a rotation.** The budget still enforces 429 at the limit and a refused claim writes no ledger row. Pinned in Task 11 (`TestDMRotateBudget_RedisOutageStillEnforced`, `TestDMRotateBudget_RefusalDoesNotCharge`).

---

## PR-A: friends and personal thread

### Task 1: Migration 000199, friend-pair fold and unique index

**Files:**
- Create: `migrations/000199_friendships_pair_unique.up.sql`
- Create: `migrations/000199_friendships_pair_unique.down.sql`
- Test: `internal/database/migration_000199_test.go`
- Modify: `migrations/README.md` (header range, new row)

**Interfaces:**
- Produces: unique index `idx_friendships_pair_unique` on `(LEAST(requester_id, addressee_id), GREATEST(requester_id, addressee_id))`, the `ON CONFLICT` arbiter for Task 3.

- [ ] **Step 1: Write the failing tests** in `migration_000199_test.go` using `migrationReadFile`, `migrationRollback` and `migrationSchemaSnapshot` (pattern of `migration_000179_test.go`). Each test drops the new index inside a rolled-back tx, seeds rows, runs the up SQL, then asserts:
  - `TestMigration000199_BlockedBeatsAccepted`: A→B blocked + B→A accepted ⇒ one row, status `blocked`, `requester_id = A`.
  - `TestMigration000199_BothBlockedKeepsOlder`: two blocked mirrored rows ⇒ the row with the earlier `created_at` survives, its direction unchanged.
  - `TestMigration000199_AcceptedBeatsPending`: accepted + pending ⇒ accepted survives.
  - `TestMigration000199_CommitsWithBlockedRows`: run up SQL in a real committed tx with blocked mirrored rows; `tx.Commit()` returns nil (deferred 000153 trigger does not fire). Clean up the seeded users afterwards.
  - `TestMigration000199_IndexRefusesMirror`: after up, inserting B→A when A→B exists fails with SQLSTATE `23505` (`pq.As(err, pqerror.UniqueViolation) != nil`).
  - `TestMigration000199_DownUpRoundTrip`: down drops `idx_friendships_pair_unique`; up recreates it as `CREATE UNIQUE INDEX`.
- [ ] **Step 2: Run** `go test ./internal/database -run TestMigration000199 -v`. Expected: FAIL (file not found).
- [ ] **Step 3: Write the migration.** `LOCK TABLE public.friendships IN SHARE ROW EXCLUSIVE MODE;` then a `DELETE ... USING (SELECT id, ROW_NUMBER() OVER (PARTITION BY LEAST(...), GREATEST(...) ORDER BY CASE status WHEN 'blocked' THEN 0 WHEN 'accepted' THEN 1 ELSE 2 END, created_at, id) rn ...) WHERE rn > 1`, then the index with the squawk-ignore comment. Down: `DROP INDEX IF EXISTS idx_friendships_pair_unique;` plus a comment that the fold is irreversible.
- [ ] **Step 4: Run** the Step 2 command and `squawk` on the file per `.claude/rules/domains/migrations.md`. Expected: PASS, no squawk error.
- [ ] **Step 5: Commit** `feat: enforce one friendship row per user pair`.

### Task 2: Migration 000200, personal-thread demote and partial unique index

**Files:**
- Create: `migrations/000200_dm_conversations_one_personal.up.sql`
- Create: `migrations/000200_dm_conversations_one_personal.down.sql`
- Test: `internal/database/migration_000200_test.go`
- Modify: `migrations/README.md`

**Interfaces:**
- Produces: partial unique index `idx_dm_conversations_one_personal ON dm_conversations (created_by) WHERE is_personal`, the arbiter for Task 5.

- [ ] **Step 1: Write the failing tests:**
  - `TestMigration000200_KeepsOldestPersonal`: three personal rows for one user ⇒ exactly one `is_personal = TRUE`, the earliest `created_at`; the other two still exist with `is_personal = FALSE` and `hidden_at IS NULL`; row count unchanged.
  - `TestMigration000200_IndexRefusesSecondPersonal`: second personal insert for the same `created_by` fails with `23505`; a non-personal insert succeeds.
  - `TestMigration000200_DownUpRoundTrip`.
- [ ] **Step 2: Run** `go test ./internal/database -run TestMigration000200 -v`. Expected: FAIL.
- [ ] **Step 3: Write the migration** (lock `dm_conversations IN SHARE ROW EXCLUSIVE MODE`, `UPDATE ... SET is_personal = FALSE` for `ROW_NUMBER() OVER (PARTITION BY created_by ORDER BY created_at, id) > 1` among personal rows, then the index). Confirm `dm_conversations.created_at` exists first (spec §10); stop and report if not.
- [ ] **Step 4: Run** Step 2 command and squawk. Expected: PASS.
- [ ] **Step 5: Commit** `feat: enforce one personal thread per user`.

### Task 3: Friend inserts use the pair index as arbiter

**Files:**
- Modify: `internal/friends/handlers.go` (`SendRequest` insert near `:481`; `executeFriendCodeClaim` insert near `:1503`; `executeBlockTxWithAudience` insert near `:877`)
- Test: `internal/friends/friend_pair_race_test.go`

**Interfaces:**
- Consumes: Task 1 index; existing `friendshipConflictResponse(status string) (int, string)`; existing `presencehook.ErrProbeStale`.

- [ ] **Step 1: Write the failing tests** (real Postgres through the handler, as in `handlers_test.go`):
  - `TestSendRequest_ParallelMirroredRequestsOneRow`: 20 goroutine pairs, A→B and B→A at once ⇒ `SELECT COUNT(*)` for the pair = 1; every response is 201 or the status `friendshipConflictResponse` gives for the stored row; no 500.
  - `TestSendRequest_ConflictSendsNoNotification`: the loser triggers zero notification publishes (use the existing notifier fake).
  - `TestFriendCodeClaim_ConflictDoesNotSpendUse`: a pending B→A exists; A claims B's code; `use_count` unchanged and the response matches `friendshipConflictResponse("pending")`.
  - `TestBlockInsert_ConflictReturnsProbeStale`: drive `executeBlockTxWithAudience` with no prior row in the capture but a row inserted before the write ⇒ returns `presencehook.ErrProbeStale`; the inserted row is unchanged.
- [ ] **Step 2: Run** `go test ./internal/friends -run 'TestSendRequest_Parallel|TestSendRequest_ConflictSends|TestFriendCodeClaim_Conflict|TestBlockInsert_Conflict' -race -v` on base. Expected: FAIL (two rows, or 500 on unique violation after Task 1).
- [ ] **Step 3: Implement.** Each insert becomes `INSERT ... ON CONFLICT DO NOTHING RETURNING id, created_at` (no conflict target: the pair index is the only applicable arbiter besides the directional constraint). `sql.ErrNoRows` from `Scan` = lost race. `SendRequest`: re-read the pair, answer via `friendshipConflictResponse`, or 409 pending if gone; the eligibility read stays first and unconditional. Claim: abandon the capture plan, return a `claimRejection` from `friendshipConflictResponse`, skip the `use_count` update. Block: return `presencehook.ErrProbeStale`.
- [ ] **Step 4: Run** Step 2 command, then `go test ./internal/friends -race`. Expected: PASS, including the existing timing tests in `friend_request_privacy_test.go`.
- [ ] **Step 5: Commit** `fix: make the friendship insert arbitrate pair races`.

### Task 4: Decline repeats its authorising predicate

**Files:**
- Modify: `internal/friends/handlers.go:652`
- Test: `internal/friends/decline_predicate_test.go`

- [ ] **Step 1: Write the failing tests:**
  - `TestDecline_NonPendingRowIs404`: row is `accepted`, caller is addressee ⇒ 404 "Friend request not found", row intact.
  - `TestDecline_NotAddresseeIs404`: caller is the requester ⇒ 404 (or the existing pre-read 403/404, whichever is earlier today; assert row intact).
  - `TestDecline_RacingBlockKeepsBlockRow`: use a test hook between the pre-read and the delete (or seed the row as `pending`, start decline, flip it to `blocked` in another tx before the delete) ⇒ 404, row is `blocked`.
- [ ] **Step 2: Run** `go test ./internal/friends -run TestDecline_ -v`. Expected: `TestDecline_RacingBlockKeepsBlockRow` FAIL on base (row deleted).
- [ ] **Step 3: Implement** `DELETE FROM friendships WHERE id = $1 AND addressee_id = $2 AND status = 'pending'`; `RowsAffected() == 0` ⇒ 404 "Friend request not found".
- [ ] **Step 4: Run** Step 2 command. Expected: PASS.
- [ ] **Step 5: Commit** `fix: decline deletes only a pending request addressed to the caller`.

### Task 5: Personal thread insert-or-get

**Files:**
- Modify: `internal/dm/handlers.go` (`GetOrCreatePersonalThread`, `:3250-3306`)
- Test: `internal/dm/personal_thread_race_test.go`

**Interfaces:**
- Consumes: Task 2 index.

- [ ] **Step 1: Write the failing tests:**
  - `TestPersonalThread_ParallelCreatesOneID`: 20 parallel `POST /api/v1/dm/conversations/personal` for one user ⇒ one distinct id; exactly one 201, the rest 200; `COUNT(*) WHERE created_by = u AND is_personal` = 1.
  - `TestPersonalThread_FetchFailureDoesNotCreateSecond`: an existing personal thread whose response fetch returns nil (inject via the handler's existing fetch seam, or delete its participant row) ⇒ 500, still one personal row.
  - `TestPersonalThread_DemotedThreadStillLists`: seed a demoted duplicate with a message ⇒ the owner's conversation list includes it and its messages list.
- [ ] **Step 2: Run** `go test ./internal/dm -run TestPersonalThread_ -race -v`. Expected: FAIL on base.
- [ ] **Step 3: Implement.** Lookup keyed on `created_by = $1 AND is_personal`. Create tx: `INSERT ... ON CONFLICT DO NOTHING RETURNING id`; on `sql.ErrNoRows`, `SELECT id ... WHERE created_by = $1 AND is_personal`; participant insert `ON CONFLICT DO NOTHING` (confirm the `dm_participants` key first, spec §10). Nil fetch ⇒ 500. 201 only for the inserter.
- [ ] **Step 4: Run** Step 2 command and `go test ./internal/dm -run Personal`. Expected: PASS, existing `TestGetOrCreatePersonalThread_*` still pass.
- [ ] **Step 5: Commit** `fix: return the existing personal thread on a create race`.

### Task 6: PR-A docs

**Files:**
- Modify: `docs/api/openapi.yaml` (friend request 409 conflict shapes; personal thread 200 vs 201; decline 404)
- Modify: `.claude/rules/domains/migrations.md` (Key Tables for `friendships` and `dm_conversations` indexes; the no-RAISE silent-fold exception for 000199/000200 and why)
- Modify: `CHANGELOG.md` (Security: VULN-094, VULN-118, VULN-119)

- [ ] **Step 1:** Edit the three files.
- [ ] **Step 2: Run** `npx @redocly/cli lint docs/api/openapi.yaml` (or the repo's configured openapi lint). Expected: no new errors.
- [ ] **Step 3: Commit** `docs: document friend and personal-thread conflict responses`.

---

## PR-B: re-key authority, cue and budget

### Task 7: Migration 000201, rotation cue table and backfill

**Files:**
- Create: `migrations/000201_dm_rotation_cues.up.sql`
- Create: `migrations/000201_dm_rotation_cues.down.sql`
- Test: `internal/database/migration_000201_test.go`
- Modify: `internal/dm/group_handlers.go:59-68` (`groupDeleteStatements`: add `DELETE FROM dm_rotation_cues WHERE conversation_id = $1` before `dm_key_revocations`)
- Modify: `internal/dmblock/dmblock.go:1319-1327` (same statement in the empty-conversation list)
- Modify: `internal/testhelpers/testdb/testdb.go` (truncate list near 323)
- Modify: `migrations/README.md`

**Interfaces:**
- Produces: table `dm_rotation_cues(conversation_id UUID PK FK ON DELETE CASCADE, cued_epoch INTEGER NOT NULL CHECK (cued_epoch > 0), reason TEXT NOT NULL, cued_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`.

- [ ] **Step 1: Write the failing tests:**
  - `TestMigration000201_BackfillsInFlightRemoval`: group with wraps at `MAX(key_version)=3` for A, B, C and participants A, B ⇒ one cue, `cued_epoch = 3`, reason `upgrade_backfill`.
  - `TestMigration000201_NoBackfillWhenSetsMatch`: holders = participants ⇒ no cue.
  - `TestMigration000201_NoBackfillFor1to1`.
  - `TestMigration000201_CascadeOnConversationDelete`.
  - `TestMigration000201_DownUpRoundTrip`.
  - Extend `internal/dm/group_delete_reconciliation_test.go` with `TestGroupDelete_RemovesRotationCue`.
- [ ] **Step 2: Run** `go test ./internal/database -run TestMigration000201 -v`. Expected: FAIL.
- [ ] **Step 3: Write the migration and the three list edits.** Backfill: `INSERT INTO dm_rotation_cues SELECT ... FROM dm_conversations WHERE is_group AND` holder set at `MAX(key_version)` (from `dm_channel_keys`) differs from the `dm_participants` set (symmetric difference via `EXCEPT` both ways).
- [ ] **Step 4: Run** Step 2 command and `go test ./internal/dm -run TestGroupDelete`. Expected: PASS.
- [ ] **Step 5: Commit** `feat: add the persisted DM rotation cue table`.

### Task 8: `internal/dmrotationcue` leaf package

**Files:**
- Create: `internal/dmrotationcue/cue.go`
- Test: `internal/dmrotationcue/cue_test.go`

**Interfaces:**
- Produces:
  - `const ReasonMemberAdded = "member_added"`, `ReasonMemberRemoved = "member_removed"`, `ReasonMemberLeft = "member_left"`, `ReasonUserBlocked = "user_blocked"`, `ReasonUpgradeBackfill = "upgrade_backfill"`
  - `var ErrUnknownReason = errors.New("dmrotationcue: unknown reason")`
  - `func Record(ctx context.Context, tx *sql.Tx, conversationID string, epoch int, reason string) error`
  - `func Pending(ctx context.Context, tx *sql.Tx, conversationID string, epoch int) (reason string, pending bool, err error)`
  - `func Consume(ctx context.Context, tx *sql.Tx, conversationID string, throughEpoch int) error`
  - `func Uncharged() []string` (returns a fresh copy of all five reasons)
- Imports limited to `context`, `database/sql`, `errors`, `fmt`.

- [ ] **Step 1: Write the failing tests:** `TestRecord_NoOpForEpochZero`; `TestRecord_RejectsUnknownReason` (`errors.Is(err, ErrUnknownReason)`, no row); `TestRecord_KeepsGreaterEpoch` (record 5 then 3 ⇒ `cued_epoch = 5`, reason of the 5 write); `TestPending_MatchesExactEpoch` (cue at 5: `Pending(5)` true, `Pending(4)` false); `TestConsume_DeletesThroughEpoch`; `TestUncharged_ReturnsCopy` (mutating the result does not change the next call); `TestPackageImports` (parse `cue.go` with `go/parser`, assert the import set).
- [ ] **Step 2: Run** `go test ./internal/dmrotationcue -v`. Expected: FAIL (package missing).
- [ ] **Step 3: Implement.** `Record` upsert: `INSERT ... ON CONFLICT (conversation_id) DO UPDATE SET cued_epoch = EXCLUDED.cued_epoch, reason = EXCLUDED.reason, cued_at = NOW() WHERE dm_rotation_cues.cued_epoch < EXCLUDED.cued_epoch`.
- [ ] **Step 4: Run** Step 2 command. Expected: PASS.
- [ ] **Step 5: Commit** `feat: add the dmrotationcue helper package`.

### Task 9: Cue writers in the three topology paths

**Files:**
- Modify: `internal/dm/handlers.go` (add-member tx, epoch read near `:1655`; `removeMemberRows` `:2172`, epoch near `:2196`)
- Modify: `internal/dmblock/dmblock.go` (reconciler near `:1184-1242`)
- Test: `internal/dm/rotation_cue_writers_test.go`, `internal/dmblock/rotation_cue_writer_test.go`, `internal/dm/participant_writers_allowlist_test.go`

**Interfaces:**
- Consumes: Task 8 `Record` and reason constants.

- [ ] **Step 1: Write the failing tests:**
  - `TestAddMember_RecordsCue`: group at epoch 2, admin adds C ⇒ cue `(2, member_added)`.
  - `TestRemoveMember_RecordsRemovedCue` / `TestLeaveGroup_RecordsLeftCue` ⇒ reasons `member_removed` / `member_left`.
  - `TestAddMember_NoCueBeforeFirstEpoch`: epoch 0 ⇒ no row.
  - `TestDMBlock_RecordsBlockedCue` ⇒ `user_blocked`.
  - `TestParticipantWriters_AllowList`: walk `internal/` Go sources (non-test), find string literals matching `(?i)(INSERT INTO|DELETE FROM)\s+dm_participants`, assert each enclosing `file:func` is in an explicit allow-list map where each entry is marked either "records cue" or a one-line reason it does not need one (1:1 creation, personal thread, whole-conversation delete).
- [ ] **Step 2: Run** `go test ./internal/dm ./internal/dmblock -run 'Cue|AllowList' -v`. Expected: FAIL.
- [ ] **Step 3: Implement** a `dmrotationcue.Record` call in each writer's tx after the membership write, using the epoch already read for the WebSocket cue. Before reading the epoch, confirm the parent row lock mode (spec §10); if the remove/leave or block path holds no row lock on `dm_conversations`, add `SELECT 1 FROM dm_conversations WHERE id = $1 FOR SHARE` after the users lock.
- [ ] **Step 4: Run** Step 2 command plus `go test ./internal/dm ./internal/dmblock`. Expected: PASS.
- [ ] **Step 5: Commit** `feat: persist a rotation cue on every group topology change`.

### Task 10: Authority gate in the shared transaction

**Files:**
- Modify: `internal/channels/handlers.go` (`dmDistributionBatch` `:4435` add `manualRotation bool`; `admitDMSuccessorClaimTx` `:4650`; `recordDMSuccessorRevocationTx` `:4581` reason; `keyDistributionErrorResponse` `:2794` map the new error)
- Modify: `internal/channels/dm_rotation.go` (`rotateDMKeyTx` `:261` sets `manualRotation: true`; `dmRotationAuthority` delegates to the predicate)
- Create: `internal/channels/dm_rekey_authority.go`
- Test: `internal/channels/dm_successor_claim_vuln082_test.go` (regression port), `internal/channels/dm_rekey_authority_test.go`

**Interfaces:**
- Consumes: Task 8 `Pending`, `Consume`.
- Produces:
  - `var errDMRotationNotAuthorized = errors.New("dm rotation: group re-key requires creator or admin")` mapped to 403 `errMsgRotationGroupAdminOnly`.
  - `type dmClaimAdmission struct { cued bool; cueReason string; selfHeal bool }`
  - `func admitDMRekeyAuthorityTx(ctx context.Context, tx *sql.Tx, conversationID, actorID string, previousVersion int, manualRotation bool) (dmClaimAdmission, error)` (called from `admitDMSuccessorClaimTx` after the holder check, before completeness)
  - `func dmRekeyRoleAllowed(isGroup bool, createdBy, actorID, role string) bool` (shared by the pool pre-check and the tx gate)

- [ ] **Step 1: Write the failing tests.** Port the scan's exploit test as `TestVULN082_GroupNonAdminUnifiedRouteRefused` (real `distributeDMKeys`, miniredis): non-admin holder posts a full successor batch for epoch N+1 on `/e2ee/keys/:context_id` ⇒ 403 `errMsgRotationGroupAdminOnly`, no new `dm_channel_keys` rows at N+1, no ledger row. Authority matrix in `dm_rekey_authority_test.go` (table-driven, both routes):

  | case | route | cue | expect |
  |---|---|---|---|
  | group non-admin | unified | none | 403 |
  | group non-admin | unified | at N | 200, ledger reason = cue reason, cue consumed |
  | group non-admin | unified | at N-1 | 403 |
  | group non-admin | unified | none, ledger row at N, no successor (AC-1) | 200, no new ledger row |
  | group admin | unified | none | 200 |
  | group creator | unified | none | 200 |
  | 1:1 participant | unified | none | 200 |
  | group non-admin | rotate-key | at N | 403, cue still present (`TestRotateDMKey_IgnoresCueForNonAdmin`) |
  | group admin | rotate-key | at N | 200, cue consumed |
  | non-participant | either | any | 404 before 403 |
  | stale epoch, non-admin | unified | none | 409 (not 403) |
  | initial epoch (`previousVersion == 0`), non-admin | unified | none | unchanged from today |

  Plus `TestLostCueRace_NonAdminRetryRefused`: two members claim the cued epoch concurrently; one wins 200, the loser gets 409, and its retry at N+1 gets 403.
- [ ] **Step 2: Run** `go test ./internal/channels -run 'VULN082|RekeyAuthority|IgnoresCue|LostCueRace' -race -v` on base. Expected: `TestVULN082_*` and the non-admin-uncued row FAIL (200 today).
- [ ] **Step 3: Implement** the gate under the existing conversation `FOR UPDATE` lock: read `dc.is_group, dc.created_by, dp.role`; apply `dmRekeyRoleAllowed`; otherwise allow only when `!manualRotation && (cue pending at previousVersion || ledger row exists for previousVersion with no successor)`. Set the ledger reason to `cueReason` when cued. Call `dmrotationcue.Consume(previousVersion)` after the wraps and ledger row are written, on every committed successor claim.
- [ ] **Step 4: Run** Step 2 command, then `go test ./internal/channels`. Expected: PASS; existing `dm_rotation_integration_test.go` and `dm_key_distribution_race_test.go` still pass.
- [ ] **Step 5: Commit** `fix: enforce group re-key authority inside the key distribution transaction`.

### Task 11: DB-counted rotation budget on both routes

**Files:**
- Modify: `internal/channels/dm_rekey_authority.go` (budget check)
- Modify: `internal/channels/handlers.go` (`distributeDMKeys` `:4385` and unified handler: map budget error before `keyDistributionErrorResponse`)
- Modify: `internal/channels/dm_rotation.go` (delete `dmRotateBudgetExhausted`, `consumeDMRotateBudget`, the `ratelimit:dm_rotate:` key use, the pre-tx read at `:165` and post-commit spend at `:181`; rewrite the comment at `:156-163`)
- Test: `internal/channels/dm_rotate_budget_test.go`

**Interfaces:**
- Produces:
  - `type dmRotateBudgetError struct { RetryAfter time.Duration }` with `Error() string`
  - `func checkDMRotateBudgetTx(ctx context.Context, tx *sql.Tx, conversationID string) error` (returns `*dmRotateBudgetError` at `count >= dmRotateLimit`)
  - Handlers: `var be *dmRotateBudgetError; if errors.As(err, &be) { middleware.RespondRateLimited(c, be.RetryAfter, dmRotateLimit); return }`

- [ ] **Step 1: Write the failing tests** (seed `dm_key_revocations` with explicit `revoked_at`):
  - `TestDMRotateBudget_TenChargedRowsGive429_Unified` and `_RotateKey`: 10 rows with reason `manual_rotation`/`successor_claim` in the last 24 h ⇒ admin claim gets 429, `Retry-After` header equals seconds until the oldest row + 24 h (±2 s), no new wraps or ledger row.
  - `TestDMRotateBudget_RowsOlderThan24hIgnored`.
  - `TestDMRotateBudget_CuedReasonsNotCounted`: 10 rows with reason `member_added` ⇒ admin claim 200.
  - `TestDMRotateBudget_UnknownReasonCharged`: 10 rows with reason `legacy_x` ⇒ 429.
  - `TestDMRotateBudget_CuedClaimExempt`: 10 charged rows + cue at N ⇒ non-admin claim 200.
  - `TestDMRotateBudget_ParallelCannotExceed`: 9 charged rows, 8 concurrent admin claims at successive epochs ⇒ total charged rows in window ≤ 10.
  - `TestDMRotateBudget_RedisOutageStillEnforced`: close miniredis, 10 charged rows ⇒ 429 (not 200).
  - `TestDMRotateBudget_RefusalDoesNotCharge`: a 403/400/409 refusal leaves the ledger count unchanged.
- [ ] **Step 2: Run** `go test ./internal/channels -run TestDMRotateBudget_ -race -v` on base. Expected: FAIL (unified route never charges; Redis outage fails open).
- [ ] **Step 3: Implement** `checkDMRotateBudgetTx`: `SELECT COUNT(*), MIN(revoked_at) FROM dm_key_revocations WHERE conversation_id = $1 AND revoked_at > NOW() - interval '24 hours' AND reason <> ALL($2)` with `pq.Array(dmrotationcue.Uncharged())`. Call it in the gate after completeness, skipped when `admission.cued || admission.selfHeal`. Remove the Redis budget code; keep the unified route's attempt limiter.
- [ ] **Step 4: Run** Step 2 command, then `go test ./internal/channels -race` and `golangci-lint run ./internal/channels/...`. Expected: PASS, no unused-symbol lint.
- [ ] **Step 5: Commit** `fix: count the DM rotation budget in the locked transaction`.

### Task 12: PR-B docs

**Files:**
- Modify: `docs/api/openapi.yaml` (`/e2ee/keys/{context_id}`: 403 group re-key authority, 429 with `Retry-After`; rotate-key 429 now DB-counted)
- Modify: `.claude/rules/domains/backend.md` (new section "DM rotation authority, cue and budget": rule, cue lifecycle, reason list, charged set, error order, the participant-writer allow-list)
- Modify: `.claude/rules/domains/migrations.md` (Key Tables: `dm_rotation_cues`)
- Modify: `CHANGELOG.md` (Security: VULN-082, VULN-083)

- [ ] **Step 1:** Edit the files.
- [ ] **Step 2: Run** the openapi lint from Task 6. Expected: no new errors.
- [ ] **Step 3: Commit** `docs: document DM rotation authority, cue and budget`.

---

## PR-C: DM voice lock order

### Task 13: Lifecycle mutex before BEGIN at join and abort

**Files:**
- Modify: `internal/dm/handlers.go` (`authorizeVoiceJoinTx` `:2953`, mutex at `:2997`; `AbortDMVoiceMediaAuthorization` `:5372`, mutex at `:5413`)
- Test: `internal/dm/voice_lock_order_test.go`, `internal/dm/voice_lock_order_static_test.go`

**Interfaces:**
- Consumes: existing `LockDMCallLifecycle(convID uuid.UUID) func()`, `handlePrivateVoiceJoined` in `internal/voice/nats.go`.

- [ ] **Step 1: Write the failing tests:**
  - `TestVoiceJoin_LifecycleHandlerDoesNotDeadlock`: set `lock_timeout = '3s'` on the test pool; goroutine 1 runs join to the point it holds row locks (test hook after begin), goroutine 2 runs `handlePrivateVoiceJoined` for the same conversation; assert via `pg_locks` / a channel that goroutine 1 held its locks while goroutine 2 waited on the mutex (window proven), then both finish with no `55P03` error within 10 s.
  - Same shape for abort: `TestVoiceAbort_LifecycleHandlerDoesNotDeadlock`.
  - `TestLockDMCallLifecycle_PrecedesBegin` (static): parse every non-test Go file under `internal/` with `go/ast`; for each function calling `LockDMCallLifecycle`, assert no call whose name starts with `begin`/`Begin` or is `BeginTx` appears before it in source order; assert exactly nine call sites.
- [ ] **Step 2: Run** `go test ./internal/dm -run 'LifecycleHandlerDoesNotDeadlock|PrecedesBegin' -race -v` on base. Expected: FAIL (lock timeout; static guard names the two sites).
- [ ] **Step 3: Implement.** Join: take the mutex before `beginDMVoiceMembershipAuthorityTx`; on begin error unlock and return; register `defer unlockLifecycle()` after the rollback/retract defer so retraction still runs after unlock. Abort: take it after `loadDMVoiceAuthorizeIdentity`, before `beginDMTopologyEffectWithCredential`; release on begin error. Confirm no callee under the moved region calls `LockDMCallLifecycle` (non-reentrant).
- [ ] **Step 4: Run** Step 2 command, then `go test ./internal/dm ./internal/voice -race`. Expected: PASS.
- [ ] **Step 5: Commit** `fix: take the DM call lifecycle mutex before row locks`.

### Task 14: PR-C docs and final verification

**Files:**
- Modify: `internal/dm/voicering.go` (`LockDMCallLifecycle` comment: the order and the nine sites)
- Modify: `.claude/rules/domains/backend.md` (new "DM voice lock order" section)
- Modify: `docs/architecture.md` (near line 329)
- Modify: `CHANGELOG.md` (Security: VULN-104)

- [ ] **Step 1:** Edit the files.
- [ ] **Step 2: Run** the full suite: `cd services/control-plane && go test ./... -race` with disposable `DATABASE_URL`/`REDIS_URL`, then `/verify-quality-gate`. Expected: all green.
- [ ] **Step 3: Commit** `docs: document the DM voice lock order`.
