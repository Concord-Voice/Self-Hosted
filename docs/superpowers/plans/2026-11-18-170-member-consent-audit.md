# Security #170 Member Consent, Durable Restrictions and Atomic Audit: Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close VULN-051, 052, 055, 056, 057 and 061 in the Self-Hosted control plane: remove direct member add, keep restrictions across exit and re-entry, write every in-scope audit row inside its action's transaction, validate `reason` and `duration_seconds`, and stop dead-invite lookups leaking server identity.

**Architecture:** One new primitive, `rbac.AuditWriter.LogTx`, writes the audit row in the caller's `*sql.Tx` and returns a receipt confirmed after commit. Restrictions use a tombstone table (`server_member_restrictions`, migration 000198): the two exit transactions capture live state immediately before `DELETE FROM server_members`, and the one admission insert (`invites.addMemberToServer`) restores and consumes it in the same statement. Four autocommit moderation routes move into transactions that take the I-ORDER lock prefix.

**Tech Stack:** Go, gin, `database/sql` with lib/pq, PostgreSQL 16, go-redis v9, golang-migrate SQL files, testify.

**Spec:** `docs/superpowers/specs/2026-11-18-170-member-consent-audit-design.md`

All paths below are relative to `services/control-plane/` unless they start with `docs/`, `.claude/` or `CHANGELOG.md`.

## Global Constraints

- Migration number is `000198` (latest on main is 000197). Paired `.up.sql` and `.down.sql`, `TIMESTAMPTZ`, explicit `ON DELETE`, squawk-clean.
- `reason` cap: `binding:"max=512"` (code points). Rejected characters: U+0000 to U+001F except U+0009, U+000A, U+000D.
- `duration_seconds` bounds: integer, `60 <= d <= 604800`, checked before any `time.Duration` conversion. 400 message unchanged.
- Dead invite body is exactly `{"valid":false}` with status 200. Unknown code stays 404, malformed stays 400.
- Audit `action` at most 50 characters, `target_type` at most 20 characters.
- New audit action strings: `member_server_muted`, `member_server_unmuted`, `member_server_deafened`, `member_server_undeafened`.
- On any `LogTx` or commit error the route answers its existing 500 body. No new error body.
- No `context.WithoutCancel` anywhere in the changed code. No `_ =` discarded errors. `tx.*` inside transactions; commit error checked.
- Lock order for new transactions: `rbac.LockServerVisibilityCapture`, `loginfamily.AcquireWriterBarrierTx`, `lockModerationUsersTx`, `servers` row `FOR SHARE`, then child rows.
- Integration tests use real PostgreSQL through `testhelpers.SetupTestServer`; run with `go test -p 1`.
- Commits: Conventional Commits with bare `type:`; end each message with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Self-Hosted is public: no references to other private repos in code, comments, commits or docs.

## Review Focus

1. **A reason made only of allowed whitespace (`"\n\n"`) or exactly 512 multi-byte runes.** Expect 200: the cap counts code points, not bytes, and whitespace controls are allowed. Test added in Task 3.
2. **A member who is restricted, leaves, and rejoins twice in a row (retry of the same invite join).** Expect the restriction restored once and the second join a no-op that consumes nothing. Test added in Task 7.
3. **A timeout set on a member whose server is deleted concurrently.** Expect 404 or success, never a deadlock or a 500 from an FK violation in the audit insert. Test added in Task 5.
4. **A moderator lifting a mute on a member who has left.** Expect 404 and the tombstone untouched, so the user is still muted on return. Test added in Task 7.
5. **The request client disconnecting during the timeout transaction.** Expect both the timeout and its audit row to be absent, never one without the other. Test added in Task 5.

---

### Task 1: `AuditWriter.LogTx`, `AuditReceipt` and deep NUL scrub

**Files:**
- Modify: `internal/rbac/audit.go` (`Log` at `:60`, `emitAuditCommitted` at `:86`, `auditMetadataForStorage` at `:171`)
- Create: `internal/testhelpers/audit_fault.go`
- Test: `internal/rbac/audit_tx_test.go` (new), `internal/rbac/audit_test.go`

**Interfaces:**
- Produces:
  - `func (a *AuditWriter) LogTx(ctx context.Context, tx *sql.Tx, serverID string, actorID *string, action, targetType string, targetID *string, metadata map[string]interface{}) (AuditReceipt, error)`
  - `type AuditReceipt struct{ /* unexported */ }` and `func (r AuditReceipt) Confirm(ctx context.Context)`; the zero value is inert.
  - `var ErrNilAuditWriter = errors.New("rbac: audit writer is nil")` and `var ErrNilAuditTx = errors.New("rbac: audit transaction is nil")`
  - `func testhelpers.FailAuditInserts(t *testing.T, db *sql.DB, action string)` installs a `BEFORE INSERT ON audit_log` trigger that raises when `NEW.action = action`, removed in `t.Cleanup` (pattern: `members/removal_failclosed_test.go:14`).

- [ ] **Step 1: Write the failing tests** in `internal/rbac/audit_tx_test.go`:
  - `TestLogTxRowCommitsWithTransaction`: begin tx, `LogTx`, commit; one `audit_log` row with the action.
  - `TestLogTxRowRollsBackWithTransaction`: `LogTx`, rollback; zero rows.
  - `TestLogTxNilWriterAndNilTx`: `(*AuditWriter)(nil).LogTx(...)` returns `ErrNilAuditWriter`; `w.LogTx(ctx, nil, ...)` returns `ErrNilAuditTx`.
  - `TestLogTxDoesNotEmitCommittedUntilConfirm`: with a recording security-event sink, no `ReasonAuditCommitted` after `LogTx`; exactly one after `Confirm`; none from a zero `AuditReceipt{}.Confirm`.
  - `TestLogTxStripsNULAtEveryDepth`: metadata `{"reason":"a\x00b","nested":{"k\x00":"v\x00"},"list":["x\x00"],"duration_seconds":600}` inserts into the real `jsonb` column; stored JSON equals `{"reason":"ab","nested":{"k":"v"},"list":["x"],"duration_seconds":600}`.
  - `TestLogTxFailedInsertAbortsTransaction`: with `FailAuditInserts(t, db, "x_fail")`, `LogTx` returns an error and `tx.Commit()` returns `pq.ErrInFailedTransaction`.
- [ ] **Step 2: Add to `internal/rbac/audit_test.go`** `TestAuditMetadataForStorageKeepsPermissionDecimalStrings`: a `Permission` value still serializes as a decimal string after the scrub.
- [ ] **Step 3: Run** `go test -p 1 ./internal/rbac/ -run 'LogTx|AuditMetadataForStorage' -v`. Expected: FAIL (`LogTx` undefined).
- [ ] **Step 4: Implement** `LogTx`, `AuditReceipt`, `Confirm` and the scrub. `Log` and `LogTx` share one marshal function; the scrub recurses through maps, slices and strings after a `json.Marshal` then `Decoder.UseNumber` round-trip, then applies the existing `Permission` conversion. `LogTx` emits `ReasonAuditWriteFailed` on insert error; `Confirm` calls the existing `emitAuditCommitted` and info log. Add the four new action strings to the `emitAuditCommitted` list. Add `FailAuditInserts` to `internal/testhelpers/audit_fault.go`.
- [ ] **Step 5: Run** the same command. Expected: PASS. Then `go test -p 1 ./internal/rbac/`. Expected: PASS.
- [ ] **Step 6: Commit** `feat: add a transactional audit writer with a post-commit receipt`.

### Task 2: Migration 000198 `server_member_restrictions`

**Files:**
- Create: `migrations/000198_create_server_member_restrictions.up.sql`, `migrations/000198_create_server_member_restrictions.down.sql`
- Test: `internal/migrationrunner/member_restrictions_schema_test.go` (new; pattern: `internal/migrationrunner/voice_transport_rollback_schema_test.go`)

**Interfaces:**
- Produces: table `server_member_restrictions(server_id UUID, user_id UUID, timed_out_until TIMESTAMPTZ NULL, server_muted BOOLEAN NOT NULL DEFAULT false, server_deafened BOOLEAN NOT NULL DEFAULT false, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`, PK `(server_id, user_id)`, both FKs `ON DELETE CASCADE`, index `idx_server_member_restrictions_user_id`, constraint `server_member_restrictions_deafen_implies_mute CHECK (NOT server_deafened OR server_muted)`.

- [ ] **Step 1: Write the failing tests:**
  - `TestMemberRestrictionsSchema`: the columns, types, PK, index and CHECK exist; inserting `(deafened=true, muted=false)` fails with SQLSTATE 23514.
  - `TestMemberRestrictionsCascade`: deleting the user or the server removes the row.
  - `TestMemberRestrictionsDownRefusesLiveTombstone`: with a row `server_muted=true`, running the down file fails with SQLSTATE 23514 and the table still exists; with only an expired-timeout row, down succeeds; up then succeeds again (up, down, up).
- [ ] **Step 2: Run** `go test -p 1 ./internal/migrationrunner/ -run MemberRestrictions -v`. Expected: FAIL (relation does not exist).
- [ ] **Step 3: Write the up file** per spec section 2. **Write the down file** following `migrations/000197_add_admin_session_generation.down.sql`: `SET LOCAL lock_timeout = '3s'`, `LOCK TABLE server_member_restrictions IN ACCESS EXCLUSIVE MODE`, a `DO` block that raises `'cannot roll back 000198 while a departed member still has a live restriction'` with `ERRCODE = 'check_violation'` when any row has `server_muted OR server_deafened OR timed_out_until > NOW()`, then `DROP TABLE server_member_restrictions`. A leading comment says that a rollback discards departed members' restrictions.
- [ ] **Step 4: Run** the test command, then `squawk migrations/000198_*.sql`. Expected: PASS and no squawk findings.
- [ ] **Step 5: Commit** `feat: add the server member restrictions table`.

### Task 3: Shared reason validator and duration bound

**Files:**
- Create: `internal/members/moderation_input.go`
- Modify: `internal/members/handlers.go` (`TimeoutMemberRequest` at `:161-165`, `BanRequest` at `:1830-1840`)
- Test: `internal/members/moderation_input_test.go` (new)

**Interfaces:**
- Produces:
  - `func validateModerationReason(reason string) error` returns `errInvalidReason` for any rune in U+0000 to U+001F other than `\t`, `\n`, `\r`.
  - `var errInvalidReason = errors.New("reason contains invalid characters")`; the routes answer 400 `{"error":"reason contains invalid characters"}`.
  - `func timeoutDuration(seconds int64) (time.Duration, bool)` returns false unless `60 <= seconds <= 604800`, comparing integers before conversion.
  - Struct tags: `Reason string `json:"reason" binding:"max=512"`` on both request types.

- [ ] **Step 1: Write the failing tests:** `TestValidateModerationReason` table: `""`, `"ok"`, `"line1\nline2\r\n\tx"`, `"\n\n"` pass; `"a\x00b"`, `"\x01"`, `"\x1f"`, `"\x1b[31m"` fail. `TestTimeoutDuration`: 59, 604801, `math.MaxInt64`, `math.MaxInt64/1e9 + 1`, -1 return false; 60 and 604800 return `(60*time.Second, true)` and `(604800*time.Second, true)`. `TestReasonBindingCountsRunes`: binding a `TimeoutMemberRequest` with 512 copies of `"é"` passes; 513 fails with `validator.ValidationErrors`.
- [ ] **Step 2: Run** `go test -p 1 ./internal/members/ -run 'ValidateModerationReason|TimeoutDuration|ReasonBinding' -v`. Expected: FAIL (undefined).
- [ ] **Step 3: Implement** the two functions and the tags. Do not wire them into handlers yet (Task 5 and Task 6 do).
- [ ] **Step 4: Run** the command. Expected: PASS.
- [ ] **Step 5: Commit** `feat: add moderation reason and duration validation`.

### Task 4: Remove direct member add (VULN-051)

**Files:**
- Modify: `internal/api/router.go:2455-2459` (delete the `serverRoutes.POST(pathIDMembers, ...)` block)
- Modify: `internal/members/handlers.go` (delete `AddMember` at `:473`, `addMemberTx`, `respondAddMemberError`, `addMemberInsertQuery`, and the members copy of `hydrateJoinerPresence`)
- Modify: `internal/presencecapture/capture.go` (comment on `FamilyMemberAdd`: retained for enum position; no production caller)
- Delete: `internal/members/add_member_gate_test.go`, `internal/members/add_member_presence_test.go`, `internal/members/add_member_server_vanish_race_test.go`; remove `AddMember` cases from other `internal/members/*_test.go` files
- Test: `internal/members/direct_add_removed_vuln051_test.go` (new)

**Interfaces:**
- Produces: `invites.JoinServer` is the only admission path. No exported symbol is added.

- [ ] **Step 1: Write the failing test** `TestDirectMemberAddIsNotRouted`: owner A creates a server; victim V sets `dm_privacy_level` to server-members-only and friend requests to mutual servers. A posts `{"user_id": V}` to `/api/v1/servers/{id}/members`. Assert status is not 2xx, `SELECT count(*) FROM server_members WHERE server_id=$1 AND user_id=$2` is 0, A's DM create to V is refused, and A's friend request to V is refused.
- [ ] **Step 2: Run** `go test -p 1 ./internal/members/ -run TestDirectMemberAddIsNotRouted -v`. Expected: FAIL (201, one row).
- [ ] **Step 3: Delete** the route, functions and tests listed above. If `presencecapture` has a census test that requires a call site per family, mark `FamilyMemberAdd` exempt there with the same comment.
- [ ] **Step 4: Run** the test, then `go build ./... && go vet ./internal/members/ ./internal/api/ ./internal/presencecapture/` and `go test -p 1 ./internal/members/ ./internal/presencecapture/`. Expected: PASS.
- [ ] **Step 5: Commit** `fix: remove direct server member add`.

### Task 5: Lock prefix and the four autocommit routes in transactions

**Files:**
- Create: `internal/members/moderation_tx.go`
- Modify: `internal/members/handlers.go` (`UpdateMember` `:878`, `TimeoutMember` `:1039`, `RemoveTimeout` `:1106`, `UnbanMember` `:2305`)
- Test: `internal/members/moderation_tx_integration_test.go` (new), `internal/members/dangerous_action_gate_test.go`

**Interfaces:**
- Consumes: `rbac.AuditWriter.LogTx`, `AuditReceipt.Confirm` (Task 1); `validateModerationReason`, `timeoutDuration` (Task 3).
- Produces: `func (h *Handler) beginModerationTx(ctx context.Context, serverID, actorID, targetUserID string) (*sql.Tx, error)` takes, in order, `rbac.LockServerVisibilityCapture`, `loginfamily.AcquireWriterBarrierTx`, `lockModerationUsersTx(ctx, tx, actorID, targetUserID)`, `SELECT 1 FROM servers WHERE id = $1 FOR SHARE` (no row returns `sql.ErrNoRows`, mapped to the route's existing 404). The caller then does its write, `LogTx`, `tx.Commit()`, and `receipt.Confirm` only when commit returned nil.

- [ ] **Step 1: Write the failing tests** in `moderation_tx_integration_test.go`, one subtest per route (`timeout`, `remove_timeout`, `update_member`, `unban`):
  - `TestModerationAuditFailureRollsBack`: `testhelpers.FailAuditInserts` for the route's action; request answers 500; the row state (`timed_out_until`, member role label, `server_bans` row) is unchanged.
  - `TestModerationCancelledContextLeavesBothOrNeither`: run the route with a request context cancelled by a statement hook right after the action's write; assert either both the action and the audit row exist or neither does.
  - `TestModerationZeroRowWriteWritesNoAudit`: target not a member (or not banned for unban): existing 404, zero audit rows.
  - `TestTimeoutDurationBoundaries`: 59, 604801 and `9223372036854775807` answer 400 with the unchanged message and write nothing; 60 and 604800 answer 200; the audit row's `metadata->'duration_seconds'` is the JSON number applied.
  - `TestTimeoutReasonRejected`: reasons `"a\x00b"` and 513 runes answer 400 before any database write (no audit row, no `timed_out_until`).
  - `TestTimeoutRacingServerDeletion` (Review Focus 3): run timeout and server delete concurrently 20 times; every timeout answers 200 or 404; no 500, no deadlock (`40P01`).
- [ ] **Step 2: Extend** `dangerous_action_gate_test.go`: for each gated case touched here, assert zero `audit_log` rows after a refusal.
- [ ] **Step 3: Run** `go test -p 1 ./internal/members/ -run 'Moderation|TimeoutDuration|TimeoutReason|TimeoutRacing|DangerousAction' -v`. Expected: FAIL (audit failure does not roll back; 2xx on NUL).
- [ ] **Step 4: Implement** `beginModerationTx` and rewrite the four routes on it. Keep each route's authorization preflight and response bodies byte for byte. Timeout: validate reason and duration right after bind, before any database access. Replace every `_ = h.audit.Log(...)` and warn-only `h.audit.Log(...)` in these routes with `LogTx` before commit. Remove the `if h.audit != nil` skip in these routes.
- [ ] **Step 5: Run** the command, then `go test -p 1 ./internal/members/`. Expected: PASS. Fix fixtures that build handlers with a nil audit writer; never restore the skip.
- [ ] **Step 6: Commit** `fix: write moderation audit rows inside the action transaction`.

### Task 6: Kick, leave and ban: capture restrictions and `LogTx`

**Files:**
- Create: `internal/members/restrictions.go`
- Modify: `internal/members/handlers.go` (`execRemovalFenceTx` `:1349`, delete literal `:1373`; `execBanFenceTx` `:1890`, delete `:2104`; `BanMember` `:2205`; `auditMemberRemoval`; `memberAuthorityFence`)
- Test: `internal/members/restrictions_capture_integration_test.go` (new), `internal/members/removal_failclosed_test.go`

**Interfaces:**
- Consumes: Task 1, Task 2 table, Task 3 validator.
- Produces: `const captureRestrictionsQuery` holding the spec section 2 capture statement verbatim, with parameters `($1 server_id, $2 user_id)`. `memberAuthorityFence` gains `auditReceipt rbac.AuditReceipt`; callers call `fence.auditReceipt.Confirm(ctx)` after the fence reports commit.

- [ ] **Step 1: Write the failing tests:**
  - `TestExitCapturesLiveRestrictions`: for each exit (`self_leave`, `kick`, `ban`) times each restriction (`timeout_live`, `muted`, `deafened_and_muted`): after the exit, one `server_member_restrictions` row with the live values.
  - `TestExitDeafenedWithoutMuteStillLeaves` (spec criterion 3): row `server_deafened=true, server_muted=false`; leave answers 200; tombstone has both flags true.
  - `TestExitWithExpiredTimeoutWritesNoTombstone`: `timed_out_until` in the past and no flags: no row.
  - `TestBanOfNonMemberKeepsTombstone`: existing tombstone, ban the departed user: tombstone unchanged (D-170d).
  - `TestRemovalAuditFailureRollsBack`: `FailAuditInserts` for `member_removed`, `member_left`, `member_banned`: 500; membership row still present; no tombstone; no ban row.
  - `TestBanReasonRejected`: authorized ban with `"a\x00b"` answers 400 and writes nothing; unauthorized ban with the same reason still answers 403.
- [ ] **Step 2: Run** `go test -p 1 ./internal/members/ -run 'Exit|BanOfNonMember|RemovalAudit|BanReason' -v`. Expected: FAIL (relation empty; audit failure leaves the removal committed).
- [ ] **Step 3: Implement.** In `execRemovalFenceTx`, put `captureRestrictionsQuery` first in the existing `queries` slice, immediately before `DELETE FROM server_members`. In `execBanFenceTx`, execute it immediately before the delete at `:2104`. Call `LogTx` inside each closure as the last statement before `presencehook.Complete`; on error, abandon the presence plan and return. Validate ban reason after `authorizeBan`. Delete the post-commit audit calls.
- [ ] **Step 4: Run** the command, then `go test -p 1 ./internal/members/`. Expected: PASS.
- [ ] **Step 5: Commit** `fix: keep member restrictions and audit rows across removal`.

### Task 7: Restore on join, restored-timeout broadcast, and invite lookup gate

**Files:**
- Modify: `internal/invites/handlers.go` (`validateInvite` `:112`, public preview validity, `addMemberToServer` `:180`, `JoinServer`, `GetInviteInfo` `:690`)
- Test: `internal/invites/restrictions_restore_integration_test.go` (new), `internal/invites/invite_info_validity_test.go` (new), `internal/invites/handlers_test.go`

**Interfaces:**
- Consumes: Task 2 table; Task 6 capture produces the tombstones these tests rely on (tests seed rows directly so the lane does not wait on Task 6).
- Produces: `func inviteUsable(invite models.ServerInvite, now time.Time) bool`, used by `validateInvite`, the public preview and `GetInviteInfo`. `addMemberToServer` keeps `func addMemberToServer(tx txQuerier, serverID, userID, inviteID string) error`, with its first statement replaced by the spec's restore statement in one `tx.Exec`.

- [ ] **Step 1: Confirm the predicates agree.** Read `validateInvite`, the public-preview validity check and the inline expression in `GetInviteInfo`. If they disagree on any of revoked, expired or exhausted, stop and report; do not choose.
- [ ] **Step 2: Write the failing tests:**
  - `TestJoinRestoresTombstone`: seed a tombstone (live timeout, muted, deafened); join by invite; the new `server_members` row carries the values; the tombstone is gone.
  - `TestJoinDropsExpiredTimeout`: tombstone `timed_out_until` in the past plus `server_muted`; join; row has NULL timeout and `server_muted=true`.
  - `TestJoinRetryConsumesNothing` (Review Focus 2): already a member with a tombstone present; join again; `ON CONFLICT` no-op; tombstone still present.
  - `TestLiftOnDepartedMemberIs404` (Review Focus 4): tombstone with `server_muted`; unmute and remove-timeout routes answer 404; tombstone unchanged; rejoin restores the mute.
  - `TestJoinBroadcastsRestoredTimeout`: hub recorder sees one message with the same type and payload shape `members.broadcastTimeout` sends.
  - In `invite_info_validity_test.go`, `TestGetInviteInfoDeadInviteBody`: revoked, expired and exhausted codes each answer 200 with body exactly `{"valid":false}` (compare the whole body); unknown code 404; malformed 400; usable code unchanged.
- [ ] **Step 3: Run** `go test -p 1 ./internal/invites/ -run 'Join|LiftOnDeparted|GetInviteInfoDead' -v`. Expected: FAIL.
- [ ] **Step 4: Implement** `inviteUsable`, the restore statement, the in-transaction read of `timed_out_until` after the insert, the post-commit broadcast through the hub (no request context), and the lookup gate.
- [ ] **Step 5: Run** the command, then `go test -p 1 ./internal/invites/`. Expected: PASS.
- [ ] **Step 6: Commit** `fix: restore member restrictions on rejoin and hide dead invites`.

### Task 8: Ownership audit writes inside their transactions

**Files:**
- Modify: `internal/ownership/handlers.go` (`Log` calls at `:264`, `:428`, `:584`, `:1553`; detached context near `:1551`)
- Test: `internal/ownership/audit_tx_integration_test.go` (new)

**Interfaces:**
- Consumes: Task 1.

- [ ] **Step 1: Identify** the transaction each action already opens (initiate near `:672-738`, cancel near `:356-411`, reverse at `:949`, execute near `:1163-1223`) and how the executor retries a pending transfer. Record both in the task's commit message.
- [ ] **Step 2: Write the failing tests:** `TestOwnershipAuditFailureRollsBack` with one subtest per action (`ownership_transfer_initiated`, `ownership_transfer_cancelled`, `ownership_transfer_reversed`, `ownership_transferred`) using `FailAuditInserts`: 500 (or the executor's error), the transfer state is unchanged, and for execute the transfer stays pending and a later run with the fault removed completes it with one audit row.
- [ ] **Step 3: Run** `go test -p 1 ./internal/ownership/ -run OwnershipAuditFailure -v`. Expected: FAIL.
- [ ] **Step 4: Implement:** move each `Log` into its transaction as `LogTx` before commit, confirm after commit, delete the `context.WithoutCancel` use.
- [ ] **Step 5: Run** the command, then `go test -p 1 ./internal/ownership/`. Expected: PASS.
- [ ] **Step 6: Commit** `fix: write ownership audit rows inside their transactions`.

### Task 9: Voice mute, deafen and move audit (after Security#168 merges)

**Files:**
- Modify: `internal/voice/handlers.go` (`applyServerEnforcement` `:1301`, `ServerUnmute` `:1424`, move path calling `auditMoveIfCrossesHierarchy` at `:1794`, `:1817`, `:1964`)
- Test: `internal/voice/server_enforcement_audit_integration_test.go` (new)

**Interfaces:**
- Consumes: Task 1.

- [ ] **Step 1: Gate.** Confirm Security#168 has merged to main. Rebase the branch on main and re-read the anchors above; they will have moved. If #168 has not merged, stop this task.
- [ ] **Step 2: Write the failing tests:** `TestServerEnforcementWritesAudit`: mute, unmute, deafen and undeafen each write exactly one audit row with `member_server_muted`, `member_server_unmuted`, `member_server_deafened`, `member_server_undeafened`; `FailAuditInserts` for each makes the route 500 and leaves the `server_members` flags and the outbox unchanged. `TestVoiceMoveAuditInTransaction`: every move the merged code audits writes one `voice_member_moved` row, and an audit fault leaves the grant (or decision) uncommitted and sends no move signal.
- [ ] **Step 3: Run** `go test -p 1 ./internal/voice/ -run 'ServerEnforcementWritesAudit|VoiceMoveAudit' -v`. Expected: FAIL.
- [ ] **Step 4: Implement:** `LogTx` after the outbox enqueue in `applyServerEnforcement` and `ServerUnmute`; for moves, `LogTx` in the grant transaction, or for a no-grant move in the decision transaction before the signal. Remove the best-effort helper.
- [ ] **Step 5: Run** the command, then `go test -p 1 ./internal/voice/`. Expected: PASS.
- [ ] **Step 6: Commit** `fix: audit server mute, deafen and voice moves atomically`.

### Task 10: Fail startup without an audit writer; pin `Log` callers

**Files:**
- Modify: `internal/api/router.go` (members, voice and ownership handler construction)
- Test: `internal/api/audit_writer_wiring_test.go` (new), `internal/rbac/audit_callers_guard_test.go` (new)

- [ ] **Step 1: Write the failing tests:** `TestRouterRefusesNilAuditWriter`: building the router with a nil audit writer returns an error (or panics, matching existing wiring tests such as `internal/api/admin_wiring_test.go`) naming the handler. `TestOnlyRBACCallsAuditLog`: parse every non-test `.go` file under `internal/` with `go/parser`; a selector call `.Log(` on an `*rbac.AuditWriter` value (match by the `audit` field and `h.audit.Log` / `a.audit.Log` patterns) appears only in package `rbac`. `TestAuditActionAndTargetLengths`: every audit action string literal passed to `LogTx` or `Log` is at most 50 characters, every target type at most 20.
- [ ] **Step 2: Run** `go test -p 1 ./internal/api/ ./internal/rbac/ -run 'NilAuditWriter|OnlyRBACCallsAuditLog|AuditActionAndTargetLengths' -v`. Expected: FAIL for the wiring test.
- [ ] **Step 3: Implement** the startup check.
- [ ] **Step 4: Run** the command. Expected: PASS.
- [ ] **Step 5: Commit** `fix: require an audit writer for moderation handlers`.

### Task 11: Restriction literal guards, race test and scan regression ports

**Files:**
- Test: `internal/members/restriction_literals_guard_test.go` (new), `internal/members/kick_timeout_race_integration_test.go` (new), `internal/members/scan_regressions_vuln052_test.go` (new), `internal/members/scan_regressions_vuln056_test.go` (new)

- [ ] **Step 1: Write the guard tests:** `TestEveryMemberDeleteIsPrecededByCapture`: scan non-test `.go` files under `internal/`; every string literal containing `DELETE FROM server_members` is the next query after a literal equal to `captureRestrictionsQuery` (or the slice element before it); there are exactly two. `TestOnlyKnownMemberInserts`: the only non-test literals containing `INSERT INTO server_members` are the restore statement in `internal/invites/handlers.go` and the owner insert in `internal/servers/handlers.go`.
- [ ] **Step 2: Write** `TestKickRacingTimeoutDoesNotDeadlock`: 50 iterations of a kick and a timeout on the same member in parallel; every request answers 200, 404 or the existing conflict status; none answers 500; no `40P01` in logs.
- [ ] **Step 3: Port the scan tests as real-PostgreSQL regressions:** `TestRejoinKeepsTimeoutVULN052` and `TestSelfLeaveKeepsServerMuteVULN052` (leave, rejoin by invite, still restricted); `TestTimeoutNULReasonVULN056` (NUL answers 400, nothing written). VULN-051 is covered by Task 4's test.
- [ ] **Step 4: Run** `go test -p 1 ./internal/members/ -run 'EveryMemberDelete|OnlyKnownMemberInserts|KickRacingTimeout|VULN05' -v`. Expected: PASS on this branch. Then prove each port fails on old code: `git stash` is not usable across these commits, so check out `origin/main` in a scratch worktree, copy the three port files in, run them, and record the failures. Expected: each fails on its restriction or status assertion, not on compilation (adapt only setup calls if a helper is new).
- [ ] **Step 5: Run the five original scan tests** from `Concord-Voice/Security` `scans/2026-11-11-self-hosted-vulnhunt/exploit_tests/` against this branch, unmodified, re-pathing the `vhFakeDB` overlay JSON to this worktree. Expected: each fails with the assertion in the spec's table. Record result and reason per test in the PR body.
- [ ] **Step 6: Commit** `test: pin member restriction paths and port the scan regressions`.

### Task 12: Docs

**Files:**
- Modify: `docs/api/openapi.yaml` (`/servers/{id}/members` post near line 15563; `/invites/{code}` near line 8336; timeout route near line 15904; ban route)
- Modify: `docs/architecture.md` (entity diagram near lines 359-408; table list near line 698; audit sentence near line 1920)
- Modify: `services/control-plane/migrations/README.md` (range heading near line 85; row after line 307)
- Modify: `CHANGELOG.md`, `.claude/rules/domains/backend.md`, `.claude/rules/domains/migrations.md`

- [ ] **Step 1: Edit** OpenAPI: remove the `post` operation; add the `200 {"valid":false}` response schema for dead invites alongside the existing 200, 400 and 404; add `maxLength: 512` and the character rule to both `reason` fields; add `minimum: 60`, `maximum: 604800`, `type: integer` to `duration_seconds`.
- [ ] **Step 2: Edit** `docs/architecture.md`: add `server_member_restrictions` to the entity diagram and the table list; state that moderation audit rows commit in the action's transaction. Re-read the table list first; the line anchor is unverified.
- [ ] **Step 3: Edit** the migrations README, `CHANGELOG.md` (Security entries for VULN-051, 052, 055, 056, 057, 061; Removed: `POST /servers/:id/members`), `backend.md` (a section with `LogTx` rules A1 to A7, the lock prefix, R1 to R6 and the guard test names), and `migrations.md` (000198 and the Key Tables Reference).
- [ ] **Step 4: Validate:** `npx @redocly/cli@latest lint docs/api/openapi.yaml` (or the repo's configured OpenAPI lint) and the repo's markdown lint. Expected: no new errors.
- [ ] **Step 5: Commit** `docs: document member restrictions, atomic audit and invite lookup`.

### Task 13: Full verification

- [ ] **Step 1: Run** focused suites: `go test -p 1 ./internal/members/ ./internal/invites/ ./internal/rbac/ ./internal/voice/ ./internal/ownership/ ./internal/api/ ./internal/migrationrunner/ ./internal/presencecapture/`. Expected: PASS.
- [ ] **Step 2: Run** the full suite: `make test` (equivalent to `go test -p 1 -v ./...`) with PostgreSQL and Redis up. Expected: PASS.
- [ ] **Step 3: Run** `golangci-lint run ./...` and `/verify-quality-gate`. Expected: no new findings; at least 80% coverage on new code.
- [ ] **Step 4: Check** `git grep -n "WithoutCancel" -- services/control-plane/internal/members services/control-plane/internal/ownership services/control-plane/internal/voice services/control-plane/internal/invites` returns nothing, and `git grep -n "_ = h.audit" -- services/control-plane/internal` returns nothing.
- [ ] **Step 5: Record** the outputs for the PR body. No commit.
