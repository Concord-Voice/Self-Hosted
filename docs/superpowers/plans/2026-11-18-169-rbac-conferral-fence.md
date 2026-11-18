# Security#169 RBAC Conferral Checks and Permission-Cache Fence Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close VULN-066, 067, 068, 053, 063 and 062 in the Self-Hosted control-plane: role
changes are judged at channel scope, copy paths are subset-checked, membership writes re-check
authority in the transaction, every permission-input writer bumps a generation after commit, and
role mutations work above 64 voice channels.

**Architecture:** One static conferral verdict shared by the in-transaction guard and the pooled
pre-check. One post-commit bump entry point built on the existing `bumpWithFallback`, with
step-up graces moved to MFA-only generations. One shared-advisory-lock helper for membership
writes. Capture chunked at 64 in both phases.

**Tech Stack:** Go control-plane, `lib/pq` v1.12.3, `go-redis/v9` v9.23.0, `gin` v1.12.0,
PostgreSQL, Redis. Integration tests use `internal/testhelpers/testdb` with real Postgres and
Redis (`DATABASE_URL`, `REDIS_URL`).

**Spec:** `docs/superpowers/specs/2026-11-18-169-rbac-conferral-fence-design.md`

All paths below are relative to `services/control-plane/` unless they start with `docs/`,
`.claude/` or `CHANGELOG.md`. Run every `go test` from `services/control-plane/`. Line anchors
come from HEAD `6ba00d24` and will move after the Task 1 rebase; Task 1 re-anchors them.

## Global Constraints

- No migration. No new table, outbox, dispatcher or environment variable.
- Cache value format stays `permValueVersion = "2"`.
- Every new 403 uses `errMsgCannotGrantPerms` ("Cannot grant permissions you do not have") with no detail, on removals too.
- A refusal returns before `PrepareCapture`, and before any DML, key rotation or candidate revocation.
- A failed authoritative (in-transaction) read returns 500 and never grants. A failed pre-check read returns nil and falls through.
- The advisory lock is the first statement of its transaction; lock order is advisory → writer barrier → users → domain rows.
- New reads of `member_roles` and override tables are server-qualified.
- Actor permissions are compared after the MFA mask; owners bypass, except the RS4 unenrolled-owner rule.
- `internal/rbac` imports nothing from presence packages.
- Chunk size `presenceCaptureMaxChannels = 64`; ceiling `presenceCaptureMaxChunks = 8`; channel cap `rbac.MaxChannelAuthorityChannels = 500`.
- Bumps run after commit, before `recheckVoice*`, on `context.WithoutCancel` with the existing 10 s deadline.
- Never upgrade a shared advisory lock to exclusive in the same transaction.
- Commits: Conventional Commits with bare `type:`; end each message with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Public repo: no references to other deployments' repos, paths, PRs or issues; Security repo references as `Security#N`.
- Test-first: every task's new test is run against the pre-change code and must fail before the implementation step.

## Review Focus

1. **A role with both ALLOW and DENY rows on the same channel.** Assign checks the ALLOW, removal checks the DENY; neither leaks into the other. Test added to Task 3.
2. **Override rows of a role that point at a channel on another server** (cross-server row). The server-qualified read ignores them; the verdict does not 500. Test added to Task 3.
3. **Exactly 64 and exactly 128 occupied voice channels.** Chunk boundaries: 1 and 2 chunks, 200 on every route, no off-by-one into an extra chunk. Test added to Task 14.
4. **Five invite-code collisions in a row.** Invite create returns 500 and inserts nothing; it never reports success without a row. Test added to Task 12.
5. **Redis unreachable for the whole request.** Reads fall through to the database, the bump fails, the bypass engages, and the next read on the same instance serves database truth. Test added to Task 10.

---

### Task 1: Rebase gate, verification list, and the ported exploit test

**Files:**
- Create: `internal/rbac/override_conferral_vuln066_test.go` (ported from the Security repo scan `scans/2026-11-11-self-hosted-vulnhunt/exploit_tests/override_conferral_vuln066_test.go`)
- Modify: `docs/superpowers/specs/2026-11-18-169-rbac-conferral-fence-design.md` ("Not verified" section: record each answer)

**Interfaces:**
- Consumes: nothing.
- Produces: `TestVULN066_AssignRoleConfersOverrideAllow` (asserts the exploit **succeeds**; Task 4 flips it). Re-anchored line numbers recorded in the spec.

- [ ] **Step 1:** Confirm Security#170 and Security#167 are merged on `origin/main`; `git fetch && git rebase origin/main`. Resolve conflicts in `internal/members`, `internal/invites` and `internal/channels/groups.go`, keeping the predecessors' code.
- [ ] **Step 2:** Answer each "Not verified" item by reading the code, and write the answer under that item in the spec: (a) every rbac override upsert/delete runs under `LockServerVisibilityCapture`; (b) whether `voice/temp_grants.go` grants can target a role; (c) `members/handlers.go` and `servers/handlers.go` never write `owner_id` outside ownership; (d) whether the `roles.delete` gate refuses an unenrolled owner, and whether UpdateRole's purpose is grace-eligible; (e) the MFA factor writer's bump site in `internal/mfa/handlers.go`; (f) the synced-move route from `docs/api/openapi.yaml`; (g) the add and unban shape Security#170 landed (route removed / system-principal only / RBAC-authorized); (h) account-erasure and server-delete lock order against `users FOR SHARE`. If (a) is false for any writer, stop and report: Task 3's stability argument fails.
- [ ] **Step 3:** Port the exploit test into package `rbac`, adapting fixtures to `testdb`. It creates a role with `permissions=0` and a channel ALLOW of a bit the actor lacks, has a `ManageRolesAssign` actor assign it, and asserts HTTP 200 and that the target now resolves the bit in that channel.
- [ ] **Step 4:** Run `go test -p 1 ./internal/rbac -run TestVULN066 -count=1 -v`. Expected: PASS (the exploit works on old code). Save the output for the PR notes.
- [ ] **Step 5:** Commit: `test: port the VULN-066 exploit test and record the Security#169 verification answers`.

### Task 2: Batched in-transaction channel permissions

**Files:**
- Modify: `internal/rbac/resolver.go` (`applyBatchedChannelOverrides` near :357; add the new method after `ResolveChannelPermissionsTx` near :265)
- Test: `internal/rbac/resolver_channel_batch_test.go` (create)

**Interfaces:**
- Consumes: `rowQuerier`, existing `resolveServerPermissions`, `MFAMask`.
- Produces:
  - `type permissionQueryer interface { QueryContext(context.Context, string, ...any) (*sql.Rows, error); QueryRowContext(context.Context, string, ...any) *sql.Row }` (satisfied by `*sql.Tx` and `*sql.DB`).
  - `func (r *Resolver) ResolveChannelPermissionsBatchTx(ctx context.Context, q permissionQueryer, serverID, userID string, channelIDs []string) (server Permission, byChannel map[string]Permission, err error)`. The Administrator bypass reads the raw base; the MFA mask is applied once at exit to `server` and to every `byChannel` value. Unknown or other-server channel IDs are absent from the map (not an error).

- [ ] **Step 1: Write the failing tests:** `TestResolveChannelPermissionsBatchTx_MatchesSingleChannel`: fixture with 3 channels (one with a user DENY of `PermSendMessages`, one with a role ALLOW of `PermManageMessages`, one plain); for each channel assert `byChannel[id] == ResolveChannelPermissionsTx(...)` on the same tx, and that `server` equals `ResolveServerPermissionsTx`. `TestResolveChannelPermissionsBatchTx_PoolAndTxAgree` (same call with `db` and with `tx` gives equal results). `TestResolveChannelPermissionsBatchTx_MFAMaskApplied` (unenrolled member on an enforcing server: `PermBan` absent from every value).
- [ ] **Step 2:** Run `go test -p 1 ./internal/rbac -run TestResolveChannelPermissionsBatchTx -count=1`. Expected: FAIL, undefined method.
- [ ] **Step 3:** Implement: generalise `applyBatchedChannelOverrides` from `*sql.Tx` to `permissionQueryer`; one server resolve plus one batched override query (`channel_id = ANY($1)` and `c.server_id = $2`).
- [ ] **Step 4:** Run the Step 2 command plus `go test -p 1 ./internal/rbac -run 'TestResolve|TestMFAMask' -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit: `refactor: add a batched channel-permission resolve usable on a tx or the pool`.

### Task 3: Conferral reader and verdict

**Files:**
- Create: `internal/rbac/role_conferral.go`
- Modify: `internal/rbac/role_gates.go` (`roleOverrideAllows` near :213 becomes a thin wrapper over the reader)
- Test: `internal/rbac/role_conferral_test.go` (create; unit table), `internal/rbac/role_conferral_reader_integration_test.go` (create)

**Interfaces:**
- Consumes: Task 2 `ResolveChannelPermissionsBatchTx`, `permissionQueryer`.
- Produces:
  - `type conferralMode int` with `conferAssign` and `conferRemove` (used for unassign and DeleteRole).
  - `type roleOverrideRow struct { ChannelID, CategoryID string; Allow, Deny Permission }` (exactly one ID set).
  - `func readRoleConferralInputs(ctx context.Context, q permissionQueryer, serverID, roleID string) ([]roleOverrideRow, error)`: server-qualified, no row locks.
  - `var errConferralExceedsActor = errors.New("role change confers permissions the actor lacks")`.
  - `func roleConferralVerdict(mode conferralMode, roleBits Permission, rows []roleOverrideRow, actorServer Permission, actorByChannel map[string]Permission) error`: returns `errConferralExceedsActor` or nil. Administrator in `actorServer` returns nil. A channel row whose channel is absent from `actorByChannel` is judged against 0 (refuses).
  - `func liftedDenyBits(rows []roleOverrideRow) Permission`: OR of every `Deny` (used for RS4 in Task 5).
  - `func (h *Handler) evaluateRoleConferral(ctx context.Context, q permissionQueryer, mode conferralMode, serverID, actorID, roleID string, roleBits Permission) error`: reader, batch resolve, verdict. Read errors are returned wrapped; callers decide 500 or fall-through.

- [ ] **Step 1: Write the failing unit table** `TestRoleConferralVerdict` with rows: assign, role bits within server → nil; assign, channel ALLOW `PermViewChannel` where the actor holds it at server but `actorByChannel[c]` lacks it → `errConferralExceedsActor`; assign, category ALLOW outside server → refuse; remove, channel DENY `PermSendMessages` where the actor lacks it in that channel → refuse; remove, actor holds it in that channel → nil; remove, category DENY within server → nil; Administrator actor → nil for all; no rows → today's server-bit verdict; **(Review Focus 1)** one channel with ALLOW `PermManageMessages` and DENY `PermSendMessages`, actor holds Manage but not Send there: assign → nil, remove → refuse.
- [ ] **Step 2: Write the failing reader tests:** `TestReadRoleConferralInputs_ServerQualified`: role with one channel row and one category row on its server, plus **(Review Focus 2)** a planted override row for the same role ID on a channel of another server; assert exactly the two own-server rows and no error. `TestRoleOverrideAllows_UnchangedResult`: the RS4 helper returns the same bits as before.
- [ ] **Step 3:** Run `go test -p 1 ./internal/rbac -run 'TestRoleConferral|TestReadRoleConferralInputs|TestRoleOverrideAllows' -count=1`. Expected: FAIL, undefined.
- [ ] **Step 4:** Implement `role_conferral.go`; rebuild `roleOverrideAllows` on `readRoleConferralInputs`.
- [ ] **Step 5:** Run the Step 3 command and `go test -p 1 ./internal/rbac -run TestRoleGates -count=1`. Expected: PASS.
- [ ] **Step 6:** Commit: `feat: add the static per-channel role conferral verdict`.

### Task 4: Assign checks the role's override ALLOWs (VULN-066)

**Files:**
- Modify: `internal/rbac/role_guard.go` (`evaluateRoleGuard` near :486-492; `preCheckRoleMutation` near :335-338; `mapGuardError` near :529)
- Modify: `internal/rbac/override_conferral_vuln066_test.go` (flip to assert 403)
- Test: `internal/rbac/role_conferral_assign_integration_test.go` (create)

**Interfaces:**
- Consumes: Task 3 `evaluateRoleConferral`, `errConferralExceedsActor`.
- Produces: `mapGuardError` maps `errConferralExceedsActor` → 403 `errMsgCannotGrantPerms`.

- [ ] **Step 1: Write the failing tests:** `TestAssignRole_ChannelAllowOutsideActor_403`; `TestAssignRole_ActorDeniedInThatChannel_403` (actor holds `PermViewChannel` at server, denied in a private channel; the role ALLOWs it there); `TestAssignRole_CategoryAllowOutsideActor_403`; `TestAssignRole_EnrolledOwner_200`; `TestAssignRole_UnenrolledOwnerRS4_EnrollmentRequired`; `TestAssignRole_InTxReadFailure_500` (break the override read through a test seam → 500, no `member_roles` row); `TestOverrideUpsert_CannotWriteAllowActorLacks_403` (spec criterion 2, regression on `override_gates.go:146`). Each 403 test also asserts the `member_roles` row count is unchanged. Change the ported exploit test to assert 403 and rename it `TestVULN066_AssignRoleCannotConferOverrideAllow`.
- [ ] **Step 2:** Run `go test -p 1 ./internal/rbac -run 'TestAssignRole_|TestVULN066|TestOverrideUpsert_CannotWrite' -count=1`. Expected: the new 403 tests and the flipped exploit test FAIL with 200; the owner, RS4 and override-upsert tests PASS.
- [ ] **Step 3:** In `evaluateRoleGuard`, for non-owner actors on assign, call `evaluateRoleConferral(ctx, tx, conferAssign, ...)` after the existing server-bit check; a read error returns a 500-class error. In `preCheckRoleMutation`, call it on the pool; a read error returns nil.
- [ ] **Step 4:** Run the Step 2 command and `go test -p 1 ./internal/rbac -run 'TestRoleGuard|TestRoleMutationDenial' -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit: `fix: check role override ALLOWs at channel scope when assigning a role`.

### Task 5: Removal and DeleteRole check lifted DENYs (VULN-067)

**Files:**
- Modify: `internal/rbac/role_guard.go` (`confersNothing` short-circuit near :454; comments at :70-79 and :466-468; `preCheckOwnerConferral` near :353)
- Modify: `internal/rbac/role_gates.go` (`unenrolledOwnerConferral` near :230 accepts lifted DENY bits)
- Modify: `internal/rbac/handlers.go` (UnassignRole pre-check near :2126 and guard call near :2190-2196 with its comment; DeleteRole guard call)
- Test: `internal/rbac/role_conferral_removal_integration_test.go` (create)

**Interfaces:**
- Consumes: Task 3 `evaluateRoleConferral(conferRemove)`, `liftedDenyBits`.
- Produces: unassign and delete no longer use `confersNothing`; if no caller remains, delete it.

- [ ] **Step 1: Write the failing tests:** `TestUnassignRole_SelfLiftsChannelDeny_403`; `TestUnassignRole_FromOtherMember_LiftsDenyActorLacks_403` (D-169c); `TestUnassignRole_ActorHoldsBitInChannel_200`; `TestDeleteRole_LiftsDenyActorLacks_403` (also with zero holders: still 403); `TestUnassignRole_UnenrolledOwner_DangerousDenyLift_EnrollmentRequired` (the lifted DENY includes `PermBan`); `TestUnassignRole_UnenrolledOwner_NonDangerousDenyLift_200`. Each refusal asserts the row is still present and no key rotation fired (use the rotation-count seam the `authority_tx_test.go` helpers already expose).
- [ ] **Step 2:** Run `go test -p 1 ./internal/rbac -run 'TestUnassignRole_|TestDeleteRole_Lifts' -count=1`. Expected: the 403 and enrollment tests FAIL with a success status.
- [ ] **Step 3:** Route unassign and DeleteRole through `evaluateRoleConferral(..., conferRemove, ...)` in both the guard and the pre-check; extend RS4 to `liftedDenyBits(rows) & DangerousPermissions`; apply the same term to DeleteRole only if Task 1 (d) found the `roles.delete` gate does not already refuse. Rewrite the three comments to describe the new rule.
- [ ] **Step 4:** Run the Step 2 command and `go test -p 1 ./internal/rbac -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit: `fix: subset-check lifted channel DENYs when removing or deleting a role`.

### Task 6: Copy paths subset-check copied ALLOWs (VULN-068)

**Files:**
- Modify: `internal/rbac/override_gates.go` (extract the predicate from :136-146)
- Modify: `internal/rbac/handlers.go` (permission-sync enable near :3495-3549 keeps the resolved permissions; `ReplaceCategoryOverridesForChannelsTx` near :3743 gains `actorPerms`)
- Modify: `internal/channels/handlers.go` (synced move near :1589; 403 mapping), `internal/channels/groups.go` (reorder near :1211; 403 mapping)
- Test: `internal/rbac/permission_sync_conferral_test.go` (create), `internal/channels/synced_copy_conferral_test.go` (create), `internal/rbac/override_route_deny_pin_test.go` (create)

**Interfaces:**
- Consumes: nothing new.
- Produces:
  - `var ErrOverrideCopyExceedsActor = errors.New("copied overrides confer permissions the actor lacks")`.
  - `func authorizeOverrideCopy(actorPerms, copiedAllow Permission) error`: `ErrOverrideCopyExceedsActor` or nil; Administrator passes.
  - `func ReplaceCategoryOverridesForChannelsTx(ctx context.Context, tx *sql.Tx, serverID, categoryID string, channelIDs []string, actorPerms Permission) error`: checks `BIT_OR(allow)` of the category rows before any DML. Keep the existing parameter list and append `actorPerms`. The unexported cascade used by category upserts is unchanged.

- [ ] **Step 1: Write the failing tests:** `TestPermissionSyncEnable_CopiedAllowOutsideActor_403` (asserts the channel override row count is unchanged, no rotation, zero `PrepareCapture` calls through the executor spy); `TestPermissionSyncEnable_Administrator_200`; `TestSyncedMove_CopiedAllowOutsideActor_403`; `TestReorderIntoSyncedCategory_CopiedAllowOutsideActor_403`; `TestCategoryUpsertCascade_NotChecked_200` (editing one category row is not refused because of another target's row). Spec criterion 13: `TestOverrideRoute_ManageChannelsHolderCanLowerDenyThatBindsThem_200`.
- [ ] **Step 2:** Run `go test -p 1 ./internal/rbac ./internal/channels -run 'TestPermissionSyncEnable_|TestSyncedMove_|TestReorderIntoSynced|TestCategoryUpsertCascade_NotChecked|TestOverrideRoute_ManageChannels' -count=1`. Expected: the three 403 tests FAIL with 200; the rest PASS.
- [ ] **Step 3:** Implement the predicate; call it from sync enable (in the transaction after `resolveManageChannelsTx`, plus a pooled pre-check before capture); add the `actorPerms` parameter and pass the actor's already-resolved server permissions from both `channels` callers; map `ErrOverrideCopyExceedsActor` → 403 `errMsgCannotGrantPerms` in `channels`.
- [ ] **Step 4:** Run the Step 2 command and `go test -p 1 ./internal/rbac ./internal/channels -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit: `fix: subset-check category ALLOWs copied onto a channel`.

### Task 7: Step-up graces on MFA-only generations

**Files:**
- Modify: `internal/permgen/permgen.go`, `internal/permgen/permgen_test.go`
- Modify: `internal/stepup/grace.go` (`Read` :184, `Grant` :216, `GrantRead` :226, `seedGenerations` :286, `missingGenerationKeys` :320)
- Modify: `internal/rbac/cache.go` (`DropUserGeneration` :204 and `DropServerGeneration` :213 delete both keys), `internal/rbac/resolver.go` (rename `BumpUserPermissionGeneration` → `PublishUserMFAChange`, `BumpServerPermissionGeneration` → `PublishServerMFAChange`)
- Modify: `internal/mfa/handlers.go` (factor-change call site from Task 1 (e)), `internal/servers/mfa_enforcement.go` (near :437)
- Test: `internal/stepup/grace_mfa_generation_test.go` (create)

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `func MFAUserKey(userID string) string` → `"permgen:mu:" + CanonicalID(userID)`; `func MFAServerKey(serverID string) string` → `"permgen:ms:" + CanonicalID(serverID)`.
  - `func (c *PermissionCache) BumpUserWithMFA(ctx context.Context, userID string) error` and `BumpServerWithMFA(ctx context.Context, serverID string) error`: replace the permission and the MFA generation in one `TxPipelined` (`MULTI/EXEC`).
  - `func (r *Resolver) PublishUserMFAChange(ctx context.Context, userID string) error` and `PublishServerMFAChange(ctx context.Context, serverID string) error`, both through `bumpWithFallback`.

- [ ] **Step 1: Write the failing tests:** `TestKeys` gains the two MFA keys; `TestGrace_SurvivesServerPermissionBump` (grant a server-scoped grace, call `cache.BumpServer`, assert `Covers` is still true); `TestGrace_ExpiresOnMFAUserChange` and `TestGrace_ExpiresOnMFAServerChange` (call the two publish methods, assert `Covers` is false); `TestGrace_GrantRefreshesMFAGenerationTTL` (TTL of `MFAUserKey` reset to `permgen.TTL` after `Grant`); `TestGrace_LegacyStampReadsAbsent` (a grace stamped with `permgen:u/s` values does not cover).
- [ ] **Step 2:** Run `go test -p 1 ./internal/permgen ./internal/stepup -run 'TestKeys|TestGrace_' -count=1`. Expected: FAIL (undefined keys; the server-bump test fails because graces use `permgen:s`).
- [ ] **Step 3:** Implement the keys; switch `GraceStore` to them with `EXPIRE` on grant; add the pipelined double bumps; rename the resolver methods and update both MFA writers; the fallback deletes both generation keys before any entry.
- [ ] **Step 4:** Run `go test -p 1 ./internal/permgen ./internal/stepup ./internal/mfa ./internal/servers ./internal/rbac -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit: `feat: stamp step-up graces with MFA-only generations`.

### Task 8: One post-commit bump entry point

**Files:**
- Create: `internal/rbac/fence.go`
- Modify: `internal/rbac/authority_tx.go` (`CompleteChannelAuthorityMutationWithRotations` near :906, the other `CompleteChannelAuthorityMutation*` and `FailClosedChannelAuthorityMutation`: add a required `PermissionInputChange` argument and call the bump before `presenceExecute`)
- Test: `internal/rbac/fence_test.go` (create)

**Interfaces:**
- Consumes: existing `bumpWithFallback`, `PermissionCache.BumpServer` and `BumpUser`.
- Produces:
  - `type PermissionInputChange struct { ServerID string; Server bool; UserIDs []string }`.
  - `func (r *Resolver) PublishPermissionInputChange(ctx context.Context, ch PermissionInputChange) error`: detaches with `context.WithoutCancel` plus a 10 s deadline; bumps the server when `Server` is true, else each user; returns the joined error after the fallback.

- [ ] **Step 1: Write the failing tests:** `TestPublishPermissionInputChange_ServerBumpMissesOldEntry` (plant an entry with current tags, publish, assert `Get` misses); `TestPublishPermissionInputChange_UserBump`; `TestPublishPermissionInputChange_CancelledContextStillBumps` (pass an already-cancelled context; the generation changed); `TestCompleteChannelAuthorityMutation_BumpsBeforePresenceExecute` (order spy).
- [ ] **Step 2:** Run `go test -p 1 ./internal/rbac -run 'TestPublishPermissionInputChange|TestCompleteChannelAuthorityMutation_Bumps' -count=1`. Expected: FAIL, undefined.
- [ ] **Step 3:** Implement `fence.go`; add the required argument and update every current caller to pass the scope from the spec's bump table (section 4.1).
- [ ] **Step 4:** Run `go build ./... && go test -p 1 ./internal/rbac ./internal/channels ./internal/voice -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit: `feat: add a post-commit permission generation bump entry point`.

### Task 9: Convert every writer and remove delete-based invalidation (VULN-063)

**Files:**
- Modify: `internal/rbac/handlers.go` (CreateRole, UpdateRole, DeleteRole, ReorderRoles, AssignRole, UnassignRole, channel and category override upsert/delete, sync enable/disable; replace the invalidations near :970, :1131, :1455, :1768, :2048, :2158)
- Modify: `internal/rbac/cache.go` (delete `Invalidate` and `InvalidateChannel`; make `InvalidateUser` and `InvalidateServer` unexported and fallback-only; update the header comment)
- Modify: `internal/rbac/resolver.go` (delete `InvalidateChannel` and `InvalidateUser` near :668-690)
- Modify: `internal/members/handlers.go` (kick, ban, leave, any add path), `internal/invites/handlers.go` (join), `internal/ownership/handlers.go` (transfer and reversal), `internal/servers/handlers.go` (CreateServer, DeleteServer), `internal/channels/handlers.go` and `internal/channels/groups.go` (copy callers, :686, :1638), `internal/voice/temp_grants.go` (grantee, or server per Task 1 (b))
- Test: `internal/rbac/writer_registry_test.go` (create), `internal/rbac/fence_race_integration_test.go` (create)

**Interfaces:**
- Consumes: Task 8 `PublishPermissionInputChange`.
- Produces: `var permissionInputWriterRegistry = map[string]string{"<pkg>/<file>:<func>": "server" | "user" | "exempt: <reason>"}` in the test file.

- [ ] **Step 1: Write the failing registry test** `TestPermissionInputWriterRegistry`: walk non-test `.go` files under `internal/`, find SQL string literals containing `INSERT INTO`, `UPDATE` or `DELETE FROM` on `member_roles`, `roles`, `server_members`, `channel_permission_overrides`, `category_permission_overrides`, or `UPDATE servers SET owner_id`; key each by enclosing function; fail on any site absent from the registry, and on any registry entry whose function no longer contains such a site. Exemptions exactly as the spec's table (mute, deafen, `timed_out_until`, `server_bans`, `server_invites`).
- [ ] **Step 2: Write the failing race tests** in `fence_race_integration_test.go`, one per family (role update, role delete, unassign, channel override, category override, kick, ban, ownership transfer, voice temp-grant revoke): use `SetAfterComputeForTest` to pause a read after compute, run the revoking request to completion, resume, then assert the next `HasPermission` returns false. Add `TestUnassignRole_ClientDisconnect_StillRevokes`: cancel the request context from `SetAuthorityCommitForTest` after commit; assert the generation changed and the revoked bit is not served.
- [ ] **Step 3:** Run `go test -p 1 ./internal/rbac -run 'TestPermissionInputWriterRegistry|TestFenceRace|TestUnassignRole_ClientDisconnect' -count=1`. Expected: the race tests FAIL (republished entry served); the registry test FAILS listing unconverted sites.
- [ ] **Step 4:** Convert each writer to `PublishPermissionInputChange` after commit (ambiguous-commit paths included), using the spec's scope table; delete the delete-based API; fill the registry.
- [ ] **Step 5:** Run `go build ./... && go test -p 1 ./internal/rbac ./internal/members ./internal/invites ./internal/ownership ./internal/servers ./internal/channels ./internal/voice -count=1`. Expected: PASS.
- [ ] **Step 6:** Commit: `fix: bump the permission generation on every permission-input writer`.

### Task 10: Local bypass after a total bump failure

**Files:**
- Create: `internal/rbac/fence_bypass.go`
- Modify: `internal/rbac/fence.go` (mark on total failure), `internal/rbac/resolver.go` (`HasPermission` :122, `GetEffectivePermissions` :138 and `cacheSet` :107 consult the bypass)
- Test: `internal/rbac/fence_bypass_test.go` (create)

**Interfaces:**
- Consumes: Task 8.
- Produces: `type cacheBypass struct` with `markServer(id string, until time.Time)`, `markUser(id string, until time.Time)` and `active(serverID, userID string, now time.Time) bool`; capacity 4096 scopes; overflow sets a global bypass until the latest expiry. Duration: the permission-cache TTL plus `30*time.Second`. ERROR log field `failure_class=permission_generation_bump_failed`.

- [ ] **Step 1: Write the failing tests:** `TestCacheBypass_ServesDatabaseTruthAfterBumpFailure` (the bump client points at a closed port; afterwards a stale entry in the real Redis is not served); `TestCacheBypass_ExpiresAfterTTLPlus30s` (injected clock); `TestCacheBypass_OverflowBypassesAll` (4097 scopes); `TestCacheBypass_LogsFailureClass` (log capture). **Review Focus 5:** `TestRedisDownForWholeRequest_FallsThroughAndBypasses`: Redis unreachable for reads and for the bump; the revoking request returns 200; the next read on that resolver returns the database value.
- [ ] **Step 2:** Run `go test -p 1 ./internal/rbac -run 'TestCacheBypass|TestRedisDownForWholeRequest' -count=1`. Expected: FAIL, undefined.
- [ ] **Step 3:** Implement the bypass with a mutex-guarded map.
- [ ] **Step 4:** Run the Step 2 command and `go test -p 1 ./internal/rbac -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit: `fix: serve database truth locally after a total generation bump failure`.

### Task 11: Shared-lock authorized write helper

**Files:**
- Create: `internal/rbac/authorized_write_tx.go`
- Modify: `internal/rbac/authority_tx.go` (add `LockServerVisibilityCaptureShared` next to `LockServerVisibilityCapture` at :77)
- Test: `internal/rbac/authorized_write_tx_test.go` (create)

**Interfaces:**
- Consumes: `ServerVisibilityCaptureAdvisoryKey`, `credepoch.GuardFamilyTx`, `ResolveServerPermissionsTx`.
- Produces:
  - `func LockServerVisibilityCaptureShared(ctx context.Context, tx *sql.Tx, serverID string) error` (`SELECT pg_advisory_xact_lock_shared($1)`).
  - `type AuthorizedWriteRequest struct { ServerID, ActorID string; Perm Permission }`.
  - `var ErrAuthorizedWriteDenied = errors.New("actor no longer holds the permission")`.
  - `func WithServerAuthorizedWriteTx(ctx context.Context, db *sql.DB, resolver *Resolver, req AuthorizedWriteRequest, write func(ctx context.Context, tx *sql.Tx) error) error`: READ COMMITTED; shared lock first; `GuardFamilyTx`; resolve and `Has`; then `write`; commit. Denied → `ErrAuthorizedWriteDenied`; `ErrNotMember` passes through; a read error → wrapped 500-class error.

- [ ] **Step 1: Write the failing tests:** `TestWithServerAuthorizedWriteTx_DeniedAfterDemotionCommit`; `TestWithServerAuthorizedWriteTx_WaitsForExclusiveHolder` (hold `LockServerVisibilityCapture` in another tx; `testdb.WaitForAdvisoryLockWaiter`; commit a demotion; assert the helper returns `ErrAuthorizedWriteDenied` and `write` never ran); `TestWithServerAuthorizedWriteTx_ConcurrentSharedDoNotBlock` (two helpers in parallel both complete while `testdb.AdvisoryLockWaiterExists` stays false); `TestWithServerAuthorizedWriteTx_LockIsFirstStatement` (statement-order seam); `TestSharedLockCallers_Allowlist` (only `authorized_write_tx.go` calls `LockServerVisibilityCaptureShared`, and that file never calls `LockServerVisibilityCapture`).
- [ ] **Step 2:** Run `go test -p 1 ./internal/rbac -run 'TestWithServerAuthorizedWriteTx|TestSharedLockCallers' -count=1`. Expected: FAIL, undefined.
- [ ] **Step 3:** Implement the helper and the shared-lock function.
- [ ] **Step 4:** Run the Step 2 command. Expected: PASS.
- [ ] **Step 5:** Commit: `feat: add a shared-lock in-transaction authorization helper for membership writes`.

### Task 12: Unban, invite create and the add path re-check (VULN-053)

**Files:**
- Modify: `internal/members/handlers.go` (`UnbanMember`, as Security#170 left it), `internal/invites/handlers.go` (create near :64-76 and :224-228)
- Test: `internal/members/unban_recheck_test.go` (create), `internal/invites/create_recheck_test.go` (create), `internal/members/add_path_pin_test.go` (create)

**Interfaces:**
- Consumes: Task 11 `WithServerAuthorizedWriteTx`, `ErrAuthorizedWriteDenied`.
- Produces: nothing new.

- [ ] **Step 1: Write the failing tests:** `TestUnban_RefusedAfterDemotionWithFreshCacheEntry` (demote the moderator, plant a v2 entry with current generations granting `PermBan`, call unban → the same status and body as today's cached denial; the ban row is still present); the same after a kick and after a ban of the moderator. `TestInviteCreate_RefusedAfterDemotionWithFreshCacheEntry`. `TestInviteCreate_CodeCollisionRetries` (seed collisions for the first 2 generated codes through a code-generator seam → 201). **Review Focus 4:** `TestInviteCreate_FiveCollisions_500NoRow`. `TestInviteCreate_DBOutage_500` (today's loop swallows it). Add-path pin per Task 1 (g): route removed → `TestAddMemberRoute_Absent` (404 or 405); system-principal only → `TestAddMember_PlantedFullCacheEntry_NonSystemRefused`; RBAC-authorized → `TestAddMember_RefusedAfterDemotionWithFreshCacheEntry`.
- [ ] **Step 2:** Run `go test -p 1 ./internal/members ./internal/invites -run 'TestUnban_Refused|TestInviteCreate_|TestAddMember' -count=1`. Expected: the refusal, five-collision and outage tests FAIL; the pin may already pass if the route is gone (record that).
- [ ] **Step 3:** Wrap unban and invite create in `WithServerAuthorizedWriteTx`, keeping the cached `HasPermission` early denial; move the invite insert inside as `INSERT ... ON CONFLICT DO NOTHING RETURNING created_at`, retry up to 5 times on no row, stop on any other error, and keep the `IsServerFKViolation` mapping; route any RBAC-authorized add path through the helper.
- [ ] **Step 4:** Run `go test -p 1 ./internal/members ./internal/invites -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit: `fix: re-check membership-write authority inside the write transaction`.

### Task 13: Batched visibility query

**Files:**
- Modify: `internal/rbac/channel_viewers.go` (`FilterVisibleUserIDsForChannelTx` :27 and `filterVisibleUserIDsForChannel` :48)
- Test: `internal/rbac/channel_viewers_batch_test.go` (create)

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type ChannelCandidates struct { ChannelID string; UserIDs []string }`.
  - `func (r *Resolver) FilterVisibleUserIDsForChannelsTx(ctx context.Context, tx *sql.Tx, serverID string, in []ChannelCandidates) (map[string][]string, error)`: one statement; carries Security#167's fail-closed channel-type `CASE`. The single-channel function becomes a one-element call of it.

- [ ] **Step 1: Write the failing tests:** `TestFilterVisibleUserIDsForChannelsTx_EqualsUnionOfSingle`: seeded random fixtures (fixed seed, 20 channels mixing text, voice and private, with role and user overrides, 30 members); assert the batched map equals the per-channel results of the current function, sorted. `TestFilterVisibleUserIDsForChannelsTx_UnknownChannelTypeFailsClosed`.
- [ ] **Step 2:** Run `go test -p 1 ./internal/rbac -run TestFilterVisibleUserIDsForChannelsTx -count=1`. Expected: FAIL, undefined.
- [ ] **Step 3:** Implement with `unnest` of the channel and candidate arrays joined to the existing predicate.
- [ ] **Step 4:** Run the Step 2 command and `go test -p 1 ./internal/rbac -run 'Visib|Viewer' -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit: `refactor: batch the channel visibility filter`.

### Task 14: Chunked capture and the F3 test redesign (VULN-062)

**Files:**
- Modify: `internal/voicepresence/executor.go` (`prepareCapture` :139 with the bound near :158; `activeSenders` :342; `CaptureVisibility` :200; `captureChannelVisibility` :227) and the `ChannelVisibilityFilterer` interface in `internal/voicepresence` (gains `FilterVisibleUserIDsForChannelsTx`)
- Modify: `internal/rbac/authority_tx.go` (export `maxChannelAuthorityChannels` near :442 as `MaxChannelAuthorityChannels`; rewrite the capture-bound note at :165-185)
- Modify: `internal/rbac/role_guard_denial_regression_test.go` (replace the 500-past-the-bound proof near :380-410)
- Test: `internal/voicepresence/executor_chunk_test.go` (create), `internal/rbac/role_guard_parity_test.go` (create)

**Interfaces:**
- Consumes: Task 13 `FilterVisibleUserIDsForChannelsTx`, `ChannelCandidates`.
- Produces: `presenceCaptureMaxChunks = (rbac.MaxChannelAuthorityChannels + presenceCaptureMaxChannels - 1) / presenceCaptureMaxChannels` (8), with a unit test pinning the value.

- [ ] **Step 1: Write the failing unit tests** in `executor_chunk_test.go`: `TestChunkChannels_Partition` (0, 1, 64, 65, 500 channels → 0, 1, 1, 2, 8 chunks); `TestPresenceCaptureMaxChunks_Is8`; `TestPrepareCapture_513Channels_ErrCaptureChannelLimit`; `TestPrepareCapture_OneSenderQueryPerChunk` (query-counting fake: 130 channels → 3 sender queries).
- [ ] **Step 2: Write the failing integration tests** in `role_guard_denial_regression_test.go`: `TestOwnerRoleMutations_65OccupiedVoiceChannels_200` over assign, unassign, delete and update; **Review Focus 3:** the same at exactly 64 and 128 occupied channels; `TestCaptureVisibility_StatementsUnderLock_AtMostCeil` (counting filterer: 65 → 2, 128 → 2, 500 → 8). In `role_guard_parity_test.go`: `TestRoleMutationDenial_ZeroPrepareCaptureCalls` (capture spy; denial classes hierarchy, server bits, channel ALLOW, DENY lift, delete, RS4 and copy each assert 0 calls) and `TestRoleMutation_PreCheckAndGuardAgree` (one fixture table run through `preCheckRoleMutation` on the pool and `evaluateRoleGuard` in a tx; the verdicts are equal).
- [ ] **Step 3:** Run `go test -p 1 ./internal/voicepresence ./internal/rbac -run 'TestChunkChannels|TestPresenceCaptureMaxChunks|TestPrepareCapture_|TestOwnerRoleMutations_|TestCaptureVisibility_Statements|TestRoleMutationDenial_ZeroPrepare|TestRoleMutation_PreCheckAndGuardAgree' -count=1`. Expected: the chunk, 64/65/128-channel and statement-count tests FAIL (500 or undefined); the zero-call and parity tests PASS (they lock in Tasks 4-6).
- [ ] **Step 4:** Implement both-phase chunking in one transaction with one shared `memberLoader`; remove the old bound-based assertion.
- [ ] **Step 5:** Run `go test -p 1 ./internal/voicepresence ./internal/rbac ./internal/members ./internal/channels -count=1`. Expected: PASS.
- [ ] **Step 6:** Commit: `fix: chunk the voice-presence capture so role mutations work past 64 voice channels`.

### Task 15: Docs

**Files:**
- Modify: `docs/architecture.md` (Authentication & Authorization), `docs/api/openapi.yaml`, `CHANGELOG.md` (`[Prerelease]`, `### Security`), `.claude/rules/domains/backend.md` (the #2350 conferral bullet, the #2721 guard bullet, the generation-cache residual at 487-513, the capture bound at 684, the hand-kept counts), the `internal/permgen/permgen.go` package comment
- Test: `internal/stepup/openapi_contract_test.go` and any other openapi contract test (run; modify only if it enumerates responses)

- [ ] **Step 1:** Run `go test -p 1 ./internal/stepup -run OpenAPI -count=1` and record the baseline (PASS).
- [ ] **Step 2:** Update `openapi.yaml`: 403 `errMsgCannotGrantPerms` on `POST /servers/{id}/members/{user_id}/roles`, `DELETE /servers/{id}/members/{user_id}/roles/{role_id}`, `DELETE /servers/{id}/roles/{role_id}`, `PUT /channels/{id}/permission-sync`, the synced-move route from Task 1 (f) and `/servers/{id}/channels/reorder`; the enrollment-required response on unassign; the credential-epoch error on `DELETE /servers/{id}/bans/{user_id}` and `POST /servers/{id}/invites`.
- [ ] **Step 3:** Update `architecture.md` with every item in the spec's "Docs to update". State that the shared lock orders against every exclusive-lock revoker and MFA factor removal but **not** against the MFA enforcement toggle.
- [ ] **Step 4:** Add the `CHANGELOG.md` line naming VULN-066, 067, 068, 053, 063 and 062, the new 403s, 500 → 200 above 64 voice channels, and the one-time step-up re-prompt after upgrade.
- [ ] **Step 5:** Update the `backend.md` bullets and counts. Confirm no comment the spec lists still contradicts the code: `grep -n 'confersNothing\|deny-subtraction exception\|InvalidateChannel' internal/rbac/*.go` shows no stale prose.
- [ ] **Step 6:** Run `go test -p 1 ./internal/stepup -run OpenAPI -count=1` and the OpenAPI lint configured in `.claude/repo-config.json`, if any. Expected: PASS.
- [ ] **Step 7:** Commit: `docs: document the Security#169 conferral rule, generation fence and capture chunking`.

### Task 16: Full verification

**Files:** none changed.

- [ ] **Step 1:** Run `go vet ./... && go build ./...`. Expected: no output.
- [ ] **Step 2:** Run every affected package suite: `go test -p 1 ./internal/rbac ./internal/voicepresence ./internal/members ./internal/invites ./internal/channels ./internal/stepup ./internal/permgen ./internal/mfa ./internal/servers ./internal/ownership ./internal/voice -count=1`. Expected: `ok` for every package.
- [ ] **Step 3:** Run `go test -p 1 ./... -count=1`. Expected: all `ok`.
- [ ] **Step 4:** Confirm the test-first record: for each VULN the PR notes hold the failing output from that task's "verify it fails" step (VULN-066 Task 4, 067 Task 5, 068 Task 6, 053 Task 12, 063 Task 9, 062 Task 14).
- [ ] **Step 5:** Run `/verify-quality-gate`. Expected: PASS (0 new blocker or critical issues, 0 new high-confidence hotspots, at least 80% new-code coverage).
- [ ] **Step 6:** Open the draft PR with three separate lines: `Closes Concord-Voice/Security#169`, `Closes Concord-Voice/Security#97`, `Closes Concord-Voice/Security#98`.
