# Security #172 Destructive-Action Gate Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every control-plane route that destroys existing message content passes a gate chosen by its effect, and a 1:1 DM participant can never cause the peer's messages to be deleted (Concord-Voice/Security#172: VULN-096, -097, -100, -084, -087, -101).

**Architecture:** Effect-equivalence tiers. DM expiration (destructive form) and group delete join DM purge's password-plus-MFA family through a helper extracted into `internal/stepup`; both edit routes join the own-rule delete soft-lock; the all-author purge counts the actor's rows through an author fence on the existing spec. Migration 000201 persists a 1:1 author scope (`apply_own` plus `expiration_backfill_author_id`) so resume can never widen.

**Tech Stack:** Go 1.27, gin v1.12.0, lib/pq v1.12.3, go-redis v9.23.0, PostgreSQL, Redis.

**Spec:** `docs/superpowers/specs/2026-11-18-172-destructive-step-up-design.md`

All paths below are relative to `services/control-plane/` unless they start with `docs/`, `.claude/`, `CHANGELOG.md` or `scripts/`.

## Global Constraints

- Purposes, exact strings: `dm.expiration_shorten`, `dm.group_delete` (hard; not own-rule; not grace-eligible); `messages.edit`, `dm.message_edit` (own-rule; grace-eligible).
- Bulk-destroy limiter key `ratelimit:user:<uid>:bulk_destroy`, limits from `resolvePurgeRateLimit` (default 5 per hour), shared by the two hard routes; DM purge's bucket untouched; Redis error = 503.
- Caps: request body 4 KiB (413 above) on DM expiration, group delete and DM purge; `current_password` at most 1 KiB in bytes and `mfa_code` at most 256 characters (400 above).
- `const EditSoftLockMinAge = GraceTTL` (10 min), age computed on the database clock from `created_at`.
- New 503 string, exact: `Editing messages is temporarily unavailable. Try again in a moment.` with `Retry-After: 30`.
- Route copy, exact: `Current password required to apply this timer to existing messages. If you are not asked for it, update Concord and try again.` and `Current password required to delete this group. If you are not asked for it, update Concord and try again.`
- A factor-less refused request spends no limiter unit, no password attempt and no MFA budget.
- No new env var, log field, metric or reason code. No copy names a threshold, count, duration, setting value or verified factor.
- `expiration.Request` keeps its closed schema; on the wire `retroactive` stays `"apply"`; `apply_own` is storage-only.
- Migration: no `RAISE`, no FK on the new column, constraints `NOT VALID` permanently, `DROP CONSTRAINT` without `IF EXISTS`. Confirm 000201 is still the next free number after rebasing (main is at 000197).
- Commits: Conventional Commits, bare `type:`; test commits touch only test files.
- Public repo: no references to the private mirror repository; backtick any `@name`.
- Integration tests use a disposable `DATABASE_URL` / `REDIS_URL`; never set private Go caches.

## Review Focus

1. **Account erased mid-backfill**: the author's rows cascade away; resume must converge and clear the marker, not 500 or stick on `backfill_pending`. Test in Task 3 (`TestApplyOwn_ResumeAfterAuthorErased`).
2. **A request body with `current_password` on a non-destructive expiration form** (a confused or newer client): must be ignored, not verified, not charged, not 400. Test in Task 4 (`TestUpdateExpiration_FactorsIgnoredOnNonDestructive`).
3. **Group delete with an empty body vs `{}` vs `null` vs two concatenated objects**: empty and `{}` mean no factor; `null` and trailing data are 400. Test in Task 5 (`TestDeleteGroup_BodyShapes`).
4. **Edit of an old message while a delete-earned grace is active in the same scope**: must pass without a second prompt; outside the scope must still count. Test in Task 7 (`TestChannelEdit_DeleteGraceCoversEdit`).
5. **Pre-fix binary reads an `apply_own` marker after a rollback without the down migration**: must fail closed (not found), never run all-author. Test in Task 3 (`TestRunBatch_UnknownModeFailsClosed` asserting the current code path treats an unrecognised mode as `ErrBackfillNotFound`).

---

### Task 1: Census guard and ported exploit tests (red)

**Files:**
- Create: `internal/dm/vuln096_exploit_test.go` (ported from Concord-Voice/Security `scans/2026-11-11-self-hosted-vulnhunt/exploit_tests/dm_expiration_peer_delete_vuln096_test.go`, with its scripted driver `vhfakedb_dm_vuln096_test.go` merged in as `internal/dm/vuln096_fakedb_test.go`)
- Create: `internal/dm/vuln096_exploit_integration_test.go` (ported from `dm_expiration_peer_delete_vuln096_integration_test.go`)
- Create: `internal/api/destructive_writer_census_test.go`

**Interfaces:**
- Produces: `destructiveWriterCensus` table (`map[string]censusEntry{GatedBy string; ExemptReason string}`) keyed `"<file>:<func>"`, which later tasks update when they gate a writer.

- [ ] **Step 1: Port the two exploit tests.** Keep the module import path of this repo; drop the `-overlay` instructions; keep the names `TestVuln096_PeerRowsStamped`, `TestVuln097_NoStepUpOnBulkExpiration` and the integration test name; invert nothing yet (they assert the fixed behaviour: peer row `expires_at` stays NULL; the step-up read happens and the response is 403 `password_required`).
- [ ] **Step 2: Write `TestDestructiveWriterCensus`.** It walks `internal/**/*.go` (non-test), finds every statement matching `DELETE FROM (messages|dm_messages|dm_conversations|servers)`, `UPDATE (messages|dm_messages) SET (content|ciphertext|expires_at)`, and fails for any `"<file>:<func>"` not in the table. Classify every current writer, including those in `internal/dmblock`, `internal/keyrotation` and `internal/servers`. Mark the six findings' writers with `GatedBy: ""` and a `TODO(#172)` guard that makes the test fail while any entry has neither field set.
- [ ] **Step 3: Run and record red.**
  Run: `go test -count=1 -run 'TestVuln096|TestVuln097|TestDestructiveWriterCensus' ./internal/dm/ ./internal/api/`
  Expected: FAIL for all three (peer row stamped; no step-up query; six unclassified writers). Paste the failure lines into the PR body. If a writer in `dmblock`, `keyrotation` or `servers` is ungated content destruction, add a task here before continuing (spec: fixed in this work).
- [ ] **Step 4: Commit.** `git add internal/dm/vuln096_*_test.go internal/api/destructive_writer_census_test.go && git commit -m "test: add Security #172 exploit and destructive-writer census tests"`

### Task 2: Step-up purposes and the bulk-destroy seam

**Files:**
- Modify: `internal/stepup/purpose.go` (constants, `allPurposes`, `ownRulePurposes`, `graceEligiblePurposes`)
- Modify: `internal/stepup/ownrule.go` (`OwnRuleCopy` edit copy, `ErrMsgEditGuardUnavailable`)
- Create: `internal/stepup/bulk_destroy.go`
- Modify: `internal/stepup/grace.go` (add `EditSoftLockMinAge`)
- Modify: `internal/dm/purge.go:58-110,308-405` (rewire onto the seam, 4 KiB body cap)
- Test: `internal/stepup/purpose_test.go`, `internal/stepup/bulk_destroy_test.go`, `internal/stepup/openapi_contract_test.go`, `internal/dm/purge_stepup_test.go`

**Interfaces:**
- Produces:
  - `PurposeDMExpirationShorten`, `PurposeDMGroupDelete`, `PurposeMessageEdit`, `PurposeDMMessageEdit Purpose`
  - `type HardRoute struct { Purpose Purpose; CredentialRequired string; NoFactors string }`
  - `func RequireAuthBeforePurge(ctx context.Context, q Querier, userID string) bool`
  - `func VerifyBulkDestroy(ctx context.Context, db *sql.DB, rdb redis.UniversalClient, mfa MFAVerifier, userID string, route HardRoute, password, mfaCode string) (mfaVerified bool, e *Error)` (use the existing types `verifyPurgeStepUpWithMFA` already passes)
  - `func BulkDestroyRefusalNoFactors(route HardRoute, ...) *Error` is not separate: `VerifyBulkDestroy` with both inputs empty returns the refusal and spends nothing.
  - `const EditSoftLockMinAge = GraceTTL`
  - `const ErrMsgEditGuardUnavailable = "Editing messages is temporarily unavailable. Try again in a moment."`

- [ ] **Step 1: Write failing tests.**
  - `TestPurposes_ClosedSet`, `TestOwnRulePurposes_ClosedSet`, `TestGraceEligiblePurposes_ClosedSet`: extend expected lists with the four strings in their families.
  - `TestEditSoftLockMinAge_EqualsGraceTTL`: `EditSoftLockMinAge == GraceTTL`.
  - `TestOwnRuleCopy_Edit`: copy for both edit purposes is non-empty and contains none of `5`, `15`, `minute`, `setting`, `MFA`, `password`.
  - `TestRequireAuthBeforePurge`: row true → true; row false → false; no row → true; query error → true.
  - `TestVerifyBulkDestroy_EmptyInputsSpendNothing`: for an MFA account and a password-only account, both empty → the route's 403 (`password_required`) or 400 `NoFactors`, and afterwards the password-attempt key, the MFA budget key and `ratelimit:user:<uid>:bulk_destroy` are absent in Redis.
  - `TestVerifyBulkDestroy_Matrix`: wrong password → 403 invalid; locked → 423 status returned for the caller; right password without MFA on an MFA account → 403 `mfa_required`; both right → `mfaVerified == true`, nil error.
  - `TestVerifyBulkDestroy_Caps`: 1025-byte password → 400; 257-character code → 400.
  - `TestDMPurge_BodiesUnchanged` in `purge_stepup_test.go`: for each existing purge refusal, the response body is byte-identical to the pre-change golden; a 4097-byte body → 413.
  - Contract: `openapi_contract_test.go` expects the four purposes in the OpenAPI enum (fails until Task 9's OpenAPI edit; land that hunk here).
- [ ] **Step 2: Run.** `go test -count=1 ./internal/stepup/ ./internal/dm/ -run 'Purpose|EditSoftLockMinAge|OwnRuleCopy|RequireAuthBeforePurge|VerifyBulkDestroy|DMPurge_BodiesUnchanged'` → FAIL (undefined symbols).
- [ ] **Step 3: Implement** the constants, `HardRoute` values for the two purposes with the exact copy from Global Constraints, `RequireAuthBeforePurge` and `VerifyBulkDestroy` moved from `dm/purge.go:314-405` (no gin writes; return the status for 423), the caps, and `docs/api/openapi.yaml` purpose enum. `dm.(*Handler).verifyPurgeStepUpWithMFA` becomes a wrapper that writes today's bodies.
- [ ] **Step 4: Run** the Step 2 command plus `go test -count=1 ./internal/stepup/... ./internal/dm/...` → PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat: add the destructive-action purposes and bulk-destroy step-up seam"`

### Task 3: Migration 000201 and author-scoped expiration

**Files:**
- Create: `migrations/000201_dm_expiration_author_scope.up.sql`, `migrations/000201_dm_expiration_author_scope.down.sql`
- Modify: `internal/expiration/service.go` (`lockSQL`, `policyColumns`, `applyBatchSQL`, `applyEligibleSQL`, `reconcileApplySQL`, `clearMarkerSQL`, `start`, `runBatch`, `hasEligibleBackfillRow`, `executeBackfillBatch`, `StartConversationTransition`)
- Modify: `internal/expiration/pinned.go` (header invariants, `duePinnedSQL`, new `releaseHeldScopedSQL`, new `peerProtectSQL`, 1:1 arms of the reconcile)
- Modify: `internal/dm/expiration.go` (call-site signature only)
- Test: `internal/expiration/author_scope_migration_integration_test.go`, `internal/expiration/author_scope_integration_test.go`, `internal/expiration/author_scope_static_test.go`

**Interfaces:**
- Consumes: none.
- Produces:
  - `func (s *Service) StartConversationTransition(ctx context.Context, tx *sql.Tx, conversationID string, actorID uuid.UUID, request Request) (Transition, error)`
  - `func IsDestructive(req Request, previous Policy) bool` = `req.Mode == "set" && (req.Retroactive == "apply" || (previous.KeepsPins() && req.IncludePinned != nil && *req.IncludePinned))` (match the field types in `Request`)
  - `func (s *Service) ReadConversationPolicy(ctx context.Context, conversationID string) (Policy, error)` (unlocked)
  - SQL fragment `conversationApplyAuthor = "($5::uuid IS NULL OR user_id = $5::uuid)"`

- [ ] **Step 1: Write the migration test** `TestMigration000201`: after up, a seeded 1:1 row with mode `apply` has mode and cutoff NULL; a group `apply` row is unchanged; constraint names `dm_conversations_expiration_backfill_mode_check`, `dm_conversations_backfill_author_shape_check`, `dm_conversations_backfill_group_apply_check` exist; inserting 1:1 `apply` fails with `pq.As(err, pqerror.CheckViolation) != nil`; `apply_own` with NULL author fails likewise; a count of violating rows is 0; after down, `apply_own` rows have mode, cutoff and author NULL and the column is gone.
- [ ] **Step 2: Write the behaviour tests** (`author_scope_integration_test.go`):
  - `TestOneToOne_MixedAuthorMatrix`: table over previous {keep, not keep} × new {keep, not keep} × {apply, new_only, clear}, with plain, pinned, held and due rows from both authors. After each transition and each resume to completion: every peer row's `expires_at` is NULL or unchanged-or-later; no peer id in `DuePinnedIDs`; I-HELD and I-KEEP assertions from `pin_aware_migration_integration_test.go` hold.
  - `TestApplyOwn_ResumeLargeBacklog`: 5,100 mixed-author rows; resume to completion; `backfill_pending` false; only actor rows stamped.
  - `TestApplyOwn_ResumeAfterAuthorErased`: delete the actor's user row mid-backfill; resume converges and clears the marker.
  - `TestGroupApply_AllAuthors`: a group apply still stamps every author.
  - `TestRunBatch_UnknownModeFailsClosed`: plain `apply` marker on a 1:1 (constraint bypassed via `NOT VALID`-safe direct insert in the test's own transaction with the check dropped) → `ErrBackfillNotFound`.
- [ ] **Step 3: Write** `TestConversationApplyStatementsEmbedFragments` (static): the three conversation statements contain both `applyEligible` and `conversationApplyAuthor`; and `TestIsDestructive` table (omitted, true, false include_pinned × keep / not keep × apply / new_only / clear).
- [ ] **Step 4: Run.** `go test -count=1 ./internal/expiration/ -run 'Migration000201|OneToOne|ApplyOwn|GroupApply|RunBatch_Unknown|EmbedFragments|IsDestructive'` → FAIL.
- [ ] **Step 5: Implement** the migration (steps in spec section 4, verbatim order) and the service changes in spec section 5. Scope comes from the locked `is_group`; the pinned reconcile uses the 1:1 table in spec section 5.
- [ ] **Step 6: Run** Step 4 plus `go test -count=1 ./internal/expiration/... ./internal/dm/...` and the ported `TestVuln096_*` → PASS (VULN-096 tests green).
- [ ] **Step 7: Commit** (tests first in their own commit, then `feat: scope 1:1 retroactive expiration to the actor's rows`).

### Task 4: DM expiration route gate

**Files:**
- Modify: `internal/dm/expiration.go:20-190` (wrapper, cap, pre-read, gate, recheck loop)
- Create: `internal/dm/bulk_destroy_gate.go` (the shared gate used by Tasks 4 and 5)
- Modify: `internal/dm/handlers.go:125` (`Handler` field) and `internal/dm/bulk_destroy_gate.go` (`SetBulkDestroyRateLimit(limit int, window time.Duration)`)
- Modify: `internal/api/router.go:225,734` (wire the limit; boot guard)
- Test: `internal/dm/expiration_stepup_test.go`, `internal/dm/bulk_destroy_gate_test.go`, `internal/api/router_bulk_destroy_test.go`

**Interfaces:**
- Consumes: `stepup.RequireAuthBeforePurge`, `stepup.VerifyBulkDestroy`, `stepup.PurposeDMExpirationShorten`, `expiration.IsDestructive`, `expiration.ReadConversationPolicy`, `StartConversationTransition(..., actorID, ...)`.
- Produces: `func (h *Handler) runBulkDestroyGate(ctx context.Context, c *gin.Context, userID string, route stepup.HardRoute, password, mfaCode string) (gate bulkDestroyPass, ok bool)` where `bulkDestroyPass{MFAVerified bool}`; `func (h *Handler) settleBulkDestroy(ctx context.Context, userID string, p bulkDestroyPass)`; `type dmExpirationRequest struct { expiration.Request; CurrentPassword string; MFACode string }`.

- [ ] **Step 1: Write failing tests.**
  - `TestBulkDestroyGate_Order`: rule on, no factors → refusal, limiter key absent; rule on, factors → limiter charged exactly once, then verified; limiter over → 429 before verification; Redis down → 503.
  - `TestUpdateExpiration_StepUpMatrix`: destructive form with missing, wrong, right factors; rule off runs without factors; missing settings row and read error mean required; no write on any refusal (row count of stamped rows unchanged).
  - `TestUpdateExpiration_FactorsIgnoredOnNonDestructive`: `new_only` and `clear` with `current_password` set → 200, no password read, no limiter key.
  - `TestUpdateExpiration_NonDestructiveBytesUnchanged`: `resume`, `clear`, plain `new_only` responses and SQL query count match pre-change goldens.
  - `TestUpdateExpiration_WrapperDecode`: keys stripped, unknown key 400, `null` 400, 4097-byte body 413, caps 400.
  - `TestUpdateExpiration_RecheckUnderLock`: a test hook between pre-read and `BeginTx` flips the stored policy to keep pins; an `include_pinned: true` request that pre-read as non-destructive rolls back, runs the gate once (rule on: refusal; rule off: limiter charged once) and commits only after the gate.
  - `TestUpdateExpiration_RedisDownKeepsNonDestructive`: Redis stopped → destructive 503; `new_only`, `clear`, `resume` 200.
  - `TestRouter_BulkDestroyLimitWired`: boot guard exits when `SetBulkDestroyRateLimit` is not called.
- [ ] **Step 2: Run.** `go test -count=1 ./internal/dm/ ./internal/api/ -run 'BulkDestroy|UpdateExpiration_'` → FAIL; also `TestVuln097_NoStepUpOnBulkExpiration` still FAIL.
- [ ] **Step 3: Implement** spec section 3 and section 6 in that order. Limiter via `middleware.AllowUserAction` with the Global key. Recheck repeats the transaction at most once.
- [ ] **Step 4: Run** Step 2 plus `go test -count=1 ./internal/dm/... ./internal/api/...` → PASS, including `TestVuln097_*`.
- [ ] **Step 5: Commit** `feat: gate destructive DM expiration behind the bulk-destroy step-up`.

### Task 5: Group delete gate

**Files:**
- Modify: `internal/dm/group_handlers.go:369-424`
- Test: `internal/dm/group_delete_stepup_test.go`

**Interfaces:**
- Consumes: `runBulkDestroyGate`, `settleBulkDestroy`, `stepup.PurposeDMGroupDelete`.

- [ ] **Step 1: Write failing tests.** `TestDeleteGroup_StepUpMatrix` (rule on: missing/wrong → refusal and every message, member and conversation row still present; right → deleted, MFA budget cleared after commit; rule off → deleted without factors); `TestDeleteGroup_BodyShapes` (empty and `{}` → no factor; `null`, trailing object, 4097 bytes → 400/413); `TestDeleteGroup_SharesBulkDestroyBucket` (an expiration destructive charge and a group delete charge consume the same key; the sixth in the window is 429); `TestDeleteGroup_RefusalOrder` (non-admin 403 precedes any settings read).
- [ ] **Step 2: Run.** `go test -count=1 ./internal/dm/ -run DeleteGroup_` → FAIL.
- [ ] **Step 3: Implement** spec section 7. The router's `RateLimitByUser(redis, 5, 1*time.Minute)` at `internal/api/router.go` (the `dmRoutes.DELETE("/:id"` block) stays.
- [ ] **Step 4: Run** Step 2 plus `go test -count=1 ./internal/dm/...` → PASS. Update the census entry for `group_handlers.go:deleteGroupData`.
- [ ] **Step 5: Commit** `feat: gate group deletion behind the bulk-destroy step-up`.

PR-1 boundary: Tasks 1 to 5 plus Task 9's PR-1 docs.

### Task 6: All-author purge counts the actor's rows (VULN-084)

**Files:**
- Modify: `internal/purge/engine.go:52-62,800-845` (`DeleteSpec.FenceAuthor`, `selectBatch`, `selectPinned`)
- Modify: `internal/messages/purge.go:1151-1206` (`fence`, `isSelfSpec` callers, `gateSelfPurge`)
- Test: `internal/purge/engine_fence_author_test.go`, `internal/messages/purge_all_author_softlock_test.go`

**Interfaces:**
- Produces: `DeleteSpec.FenceAuthor *string` (nil = today's semantics). Arm: `($5::uuid[] IS NULL OR id = ANY($5) OR ($8::uuid IS NOT NULL AND user_id IS DISTINCT FROM $8::uuid))`; `selectPinned` gets the same with its own placeholder numbers.

- [ ] **Step 1: Write failing tests.** `TestSelectBatch_FenceAuthor` (with fence: actor rows limited to `OnlyIDs`, others unrestricted; nil fence unchanged for `DeleteOne`, expiry, ban/kick); `TestAllAuthorPurge_CountsActorRows` (actor with 20 own rows in an all-author channel purge → `HitN(20)`, trips at threshold with `delete_rate_limited`); `TestAllAuthorPurge_PostCountActorMessageSurvives`; `TestAllAuthorPurge_OthersStillDeleted`; `TestAllAuthorPurge_ConfirmsOnce` (E and over → one verification); same for the server route.
- [ ] **Step 2: Run.** `go test -count=1 ./internal/purge/ ./internal/messages/ -run 'FenceAuthor|AllAuthorPurge'` → FAIL.
- [ ] **Step 3: Implement** spec section 9. Do not change `samePurgeAuthor`, `recheckPurgeAuthorTx` or `purgeSpecProvenance`.
- [ ] **Step 4: Run** Step 2 plus `go test -count=1 ./internal/purge/... ./internal/messages/...` → PASS.
- [ ] **Step 5: Commit** `fix: count an all-author purge against the actor's own soft-lock`.

### Task 7: Channel edit soft-lock (VULN-087)

**Files:**
- Modify: `internal/messages/handlers.go:944-1130` (`authorizeMessageUpdate` returns the age flag; request embeds `stepup.Fields`; `updateMessageCiphertext` confirms first)
- Modify: `internal/messages/softlock.go` (edit purpose selection; 503 copy for edits)
- Test: `internal/messages/softlock_edit_test.go`, `internal/messages/softlock_edit_lockorder_integration_test.go`

**Interfaces:**
- Consumes: `stepup.PurposeMessageEdit`, `stepup.EditSoftLockMinAge`, `stepup.ErrMsgEditGuardUnavailable`, existing `chargeSoftLock`, `confirmSoftLockTx`, `settleSoftLock`.

- [ ] **Step 1: Write failing tests.** `TestChannelEdit_YoungMessageNeverCounts` (Redis stopped → 200); `TestChannelEdit_OldMessageTrips` (the edit after the threshold inside the window → 403 `delete_rate_limited` with `Retry-After`); `TestChannelEdit_SharedBudgetWithDeletes` (deletes up to threshold minus one plus one old edit → trips); `TestChannelEdit_DeleteGraceCoversEdit` (same scope passes; other scope counts); `TestChannelEdit_CurrentPasswordKey400`; `TestChannelEdit_RedisDown503` (exact `ErrMsgEditGuardUnavailable`, `Retry-After: 30`); `TestChannelEdit_ReleasedClientBytes` (under-threshold edit response matches golden); `TestChannelEdit_LockOrder` (integration, against concurrent member removal: no deadlock, refusal or success, never both); `TestChannelEdit_RouteLimiterBeforeHitN`.
- [ ] **Step 2: Run.** `go test -count=1 ./internal/messages/ -run ChannelEdit_` → FAIL.
- [ ] **Step 3: Implement** spec section 8 for the channel route.
- [ ] **Step 4: Run** Step 2 plus `go test -count=1 ./internal/messages/...` → PASS.
- [ ] **Step 5: Commit** `fix: count channel edits of older messages against the soft-lock`.

### Task 8: DM edit soft-lock (VULN-101)

Precondition: Concord-Voice/Security#173 is on main and this branch is rebased onto it.

**Files:**
- Modify: `internal/dm/handlers.go:3560-3722` (`authorizeDMMessageUpdate` age flag; `updateDMMessageRequest` embeds `stepup.Fields`; `updateDMMessageCiphertextAttempt` confirmation in the `GuardFamilyTx` slot; reset `confirmed` per attempt)
- Modify: `internal/dm/message_delete_softlock.go` (edit purpose and copy)
- Test: `internal/dm/message_edit_softlock_test.go`, `internal/dm/message_edit_softlock_integration_test.go`

**Interfaces:**
- Consumes: `stepup.PurposeDMMessageEdit`, `stepup.EditSoftLockMinAge`, `stepup.ErrMsgEditGuardUnavailable`, `confirmDMDeleteTx` pattern.

- [ ] **Step 1: Write failing tests** mirroring Task 7 with `DMEdit_` names, plus `TestDMEdit_RetrySpendsFactorOnce` (force `ErrMembershipChanged` on the first attempt; one verification, one `HitN`).
- [ ] **Step 2: Run.** `go test -count=1 ./internal/dm/ -run DMEdit_` → FAIL.
- [ ] **Step 3: Implement** spec section 8 for the DM route.
- [ ] **Step 4: Run** Step 2, then the census: `go test -count=1 ./internal/api/ -run TestDestructiveWriterCensus` → PASS (all six classified).
- [ ] **Step 5: Commit** `fix: count DM edits of older messages against the soft-lock`.

PR-2 boundary: Tasks 6 to 8 plus Task 9's PR-2 docs.

### Task 9: Docs and full verification (split across both PRs)

**Files:**
- Modify: `docs/api/openapi.yaml`, `docs/architecture.md`, `CHANGELOG.md`, `docs/design/authenticated-step-up-budget.md`, `.claude/rules/domains/backend.md`, `.claude/rules/domains/migrations.md`, `CLAUDE.md` (via `./scripts/update-claude-md-counts.sh`)

- [ ] **Step 1 (PR-1):** OpenAPI factor fields, refusals and 4 KiB cap on DM expiration and group delete; `docs/architecture.md` `dm_conversations` entity (`expiration_backfill_author_id`, mode `apply, apply_own or clear`), step-up line 57 (bulk-destroy routes), `expiration` row; `CHANGELOG.md` with the three behaviour changes from spec Boundaries; step-up budget design doc route list; backend.md "gate by effect" rule and step-up tokens section; migrations.md 000201 section and pin-aware note.
- [ ] **Step 2 (PR-2):** OpenAPI edit-route step-up fields and soft-lock refusal, all-author purge note; backend.md soft-lock section (population rows, `EditSoftLockMinAge`, residual sentence rewritten); `CHANGELOG.md` entries.
- [ ] **Step 3: Verify each PR.**
  Run: `go test -count=1 ./internal/stepup/... ./internal/expiration/... ./internal/dm/... ./internal/messages/... ./internal/purge/... ./internal/api/... ./internal/database/...` with disposable `DATABASE_URL` / `REDIS_URL`, then `go vet ./... && go test ./...`, then `./scripts/update-claude-md-counts.sh` and `/verify-quality-gate`.
  Expected: all PASS; `openapi_contract_test` PASS; gate green; coverage on new code at least 80%.
- [ ] **Step 4: Commit** `docs: document the Security #172 destructive-action gate`.

### Task 10: Desktop companion handoff (separate repository)

Not executed in this repository. After PR-1 is on main, open a Desktop plan from spec section "Client contract (binds Desktop)": four purposes in `stepUpPurpose.ts` with a new pin test; retry-only factor sends; `BulkDestroyStepUpDialog`; `DMThreadRemovalDialog` `delete` action replacing `confirm()`; generalised `DeleteRefusalModal` for edits with the draft-reset fix; captures in every scheme and theme pair. Mobile needs no change (its API client issues no PATCH or DELETE on these routes).
