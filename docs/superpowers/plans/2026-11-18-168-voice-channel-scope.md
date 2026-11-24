# Security #168 Voice Moderation Channel Scope Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Authorize every `/servers/:id/voice/:userId/*` actor before any voice-state read, decide session actions and moves at the channel they touch in the committing transaction, make the mover hold what a temporary grant confers, pin the DM soft-mute rule, and delete the dead `authorize-action` route.

**Architecture:** A narrow-only gate (spec Approach A). The server-scope bit stays necessary; channel overrides can only refuse. New authority code lives in `internal/voice/mod_authority.go`; handlers in `internal/voice/handlers.go` and `internal/voice/temp_grants.go` call it. The bodies of the persistent mute/deafen handlers are not touched.

**Tech Stack:** Go, Gin, `database/sql` + `lib/pq` (PostgreSQL), go-redis v9 (RBAC cache), testify; integration tests through `testhelpers.TestServer`.

**Spec:** `docs/superpowers/specs/2026-11-18-168-voice-channel-scope-design.md`

## Global Constraints

- All paths below are relative to `services/control-plane/` unless they start with `docs/`, `CHANGELOG.md` or `.claude/`.
- Monotone: nothing refused at `6ba00d24` may succeed after the change.
- No migration, no config, no new dependency, no change to `rbac/resolver.go`, `tempGrantAllow`, or the hierarchy exception (move is the only `requireHierarchy=false`).
- Do not edit the bodies of `ServerMute`, `ServerUnmute`, `ServerDeafen`, `ServerUndeafen`, `applyServerEnforcement`.
- Lock order unchanged: visibility advisory lock → users (credential guard) → per-user voice lifecycle lock.
- Write decisions use `ResolveServerPermissionsTx` / `ResolveChannelPermissionsTx` inside the committing transaction; cached `HasPermission` may only refuse early.
- Multi-bit checks use `hasAllPerms`, never `Permission.Has(A|B)` (any-of). Never call a permission check with `0`.
- Test `errors.Is(err, rbac.ErrChannelNotInServer)` before `rbac.ErrNotMember` on every channel resolve.
- Response bodies: 403 member = `{"error":"Not a member of this server"}`; 403 permission = `{"error":"Insufficient permissions"}`. Reuse the existing constants.
- Every Gin refusal path ends with `return` after writing the response.
- Public repository: no references to other private repositories, their paths, issue or PR numbers; no competitor names; backtick any `@agent` name; no finding identifiers in `CHANGELOG.md` or the PR title. Write Security issue references so they cannot autolink (for example `Concord-Voice/Security` issue 168).
- Commits: Conventional Commits, bare `type:` (no scope). Every commit ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Tests: assert the outermost effect (override row, outbox row, audit row); every refusal test has a positive control.
- Test command prefix (run from `services/control-plane`): `DATABASE_URL=... REDIS_URL=... go test -count=1`. Do not set private `GOCACHE`/`GOMODCACHE`.

## Review Focus

1. **Uppercase or brace-wrapped UUIDs in path params** on the voice routes: the preamble must treat them as the canonical ID (same 403/200) and self-move detection must compare canonical values. Test in Task 2 (`TestVoiceModPreamble_AlternateSpellingSelfMove`).
2. **Target with voice rows in two channels mid-move** (one hidden, one visible): the move and session actions must act on the visible one only, deterministically. Tests in Task 3 and Task 4.
3. **Channel deleted between the decision transaction and the grant transaction:** the grant closure must answer 403 permission, not 500 and not 403 member. Test in Task 3 (`TestServerMove_DestinationDeletedBeforeGrant`).
4. **Server owner and server Administrators with a channel DENY:** they must not be narrowed (resolver semantics), so moves and session actions still succeed. Tests in Task 3 and Task 4 (positive controls).
5. **Request body sent to a non-member with malformed JSON:** must still get 403 member, not 400, because the body is parsed after the preamble. Covered in Task 7's matrix (`malformed body` state).

---

### Task 1: Delete the `authorize-action` route

**Files:**
- Modify: `internal/voice/handlers.go:891-1009` (delete `AuthorizeVoiceAction` and any constant or helper only it used; let `golangci-lint`'s `unused` find them)
- Modify: `internal/api/router.go:2767-2770`
- Modify: `docs/api/openapi.yaml:3688` (delete the path and any schema only it referenced)
- Modify: `docs/api/README.md:109,128`
- Test: `internal/voice/handlers_test.go:47,383-~540`, `internal/voice/handlers_integration_2_test.go:224-~490` (delete the tests of the removed handler)
- Test: `internal/api/router_test.go` or the existing route-table test, whichever lists routes (add one assertion)

**Interfaces:**
- Produces: nothing new. Later tasks must not reference `AuthorizeVoiceAction`.

- [ ] **Step 1: Write the failing test** `TestRouter_AuthorizeVoiceActionRouteRemoved` in the existing route test file: POST `/api/v1/channels/<valid uuid>/voice/authorize-action` with valid auth → `assert.Equal(t, http.StatusNotFound, w.Code)`.
- [ ] **Step 2: Run it.** `go test -count=1 ./internal/api/ -run TestRouter_AuthorizeVoiceActionRouteRemoved` → FAIL (route answers non-404).
- [ ] **Step 3: Delete** the handler, route, OpenAPI path and the old tests in one change.
- [ ] **Step 4: Re-measure counts.** `python3 scripts/api/extract-routes.py` (from repo root); write the measured Voice count into README `:128` (row no longer lists `authorize-action`) and the totals at `:109`. Run `scripts/api/check-openapi-coverage.sh` → exit 0.
- [ ] **Step 5: Run.** `go test -count=1 ./internal/api/ ./internal/voice/` → PASS; `go vet ./...` → clean.
- [ ] **Step 6: Commit.** `git commit -m "refactor: remove the unused voice authorize-action route"` (router and spec in this same commit).

---

### Task 2: Authority core in `mod_authority.go`

**Files:**
- Create: `internal/voice/mod_authority.go`
- Modify: `internal/voice/handlers.go:1113` (`authorizeVoiceMod` becomes a wrapper); `voiceModContext` gains `self bool`
- Test: `internal/voice/mod_authority_test.go` (package `voice`, unit tests), `internal/voice/mod_authority_integration_test.go` (package `voice_test`)

**Interfaces:**
- Produces:
  - `type voiceModPreamble struct { perm rbac.Permission; requireHierarchy bool; allowSelf bool }`
  - `func (h *Handler) authorizeVoiceModWith(c *gin.Context, p voiceModPreamble) *voiceModContext` (nil means a response was written)
  - `func (h *Handler) authorizeVoiceMod(c *gin.Context, perm rbac.Permission) *voiceModContext` — unchanged signature; calls `authorizeVoiceModWith(c, voiceModPreamble{perm: perm, requireHierarchy: true})`
  - `func hasAllPerms(p, required rbac.Permission) bool` — `p&rbac.PermAdministrator != 0 || p&required == required`
  - `func resolveActorVoiceChannelTx(ctx context.Context, tx *sql.Tx, r *rbac.Resolver, serverID, actorID, channelID string) (perms rbac.Permission, present bool, err error)` — `present=false, err=nil` for `rbac.ErrChannelNotInServer` and for a channel whose type is not voice; other `rbac.ErrNotMember` returned as `errVoiceModActorNotMember`
  - `func findVoiceChannelsTx(ctx context.Context, tx *sql.Tx, serverID, targetID string) ([]string, error)` — replaces `findVoiceChannelTx` (`handlers.go:1176`), `ORDER BY vp.channel_id`, no row lock
  - `func authorizeMoveDestinationTx(ctx context.Context, tx *sql.Tx, r *rbac.Resolver, serverID, actorID, destID string, selfMove bool) (rbac.Permission, error)` — absent, or missing `VIEW_VOICE_CHANNELS|JOIN_VOICE`, or (non-self) missing Move → `errVoiceModPermission`
  - `func authorizeMoveSourcesTx(ctx context.Context, tx *sql.Tx, r *rbac.Resolver, serverID, actorID string, sources []string) (visible []string, err error)` — drops absent or not-VIEW channels; any visible channel without Move → `errVoiceModPermission`
  - `func filterAuthorizedSessionSpecsTx(ctx context.Context, tx *sql.Tx, r *rbac.Resolver, serverID, actorID string, perm rbac.Permission, specs []voicecommand.CommandSpec) ([]voicecommand.CommandSpec, error)` — per distinct `ChannelID`: absent or not VIEW → drop; VIEW without `perm` → `errVoiceModPermission`; empty `ChannelID` → non-sentinel error (500)
  - `func requireActorVoiceChannelPermTx(ctx context.Context, tx *sql.Tx, r *rbac.Resolver, serverID, actorID, channelID string, perm rbac.Permission) error` — absent, not VIEW, or without `perm` → `errVoiceModPermission`

- [ ] **Step 1: Write the failing unit tests** in `mod_authority_test.go`:
  - `TestHasAllPerms`: table — `(VIEW|JOIN, VIEW|JOIN)→true`, `(VIEW, VIEW|JOIN)→false`, `(Administrator, VIEW|JOIN|SPEAK)→true`, `(0, VIEW)→false`.
  - `TestResolveActorVoiceChannel_ChannelNotInServerBeforeNotMember`: a stub error `fmt.Errorf("x: %w", rbac.ErrChannelNotInServer)` mapped through the helper's error classifier returns `present=false, err=nil`; a bare `rbac.ErrNotMember` returns `errors.Is(err, errVoiceModActorNotMember)`. (Expose the classifier as `classifyVoiceChannelResolveErr(err error) (present bool, out error)` so it is unit-testable.)
- [ ] **Step 2: Write the failing integration tests** in `mod_authority_integration_test.go` (use `export_test.go` to expose the helpers to `voice_test`):
  - `TestFindVoiceChannelsTx_SortedAllRows`: target with rows in two channels → both IDs, ascending.
  - `TestAuthorizeMoveDestinationTx`: table over {missing channel, text channel, other server's channel, no VIEW, no JOIN, channel DENY Move} → `errors.Is(err, errVoiceModPermission)`; owner with channel DENY Move → nil; self with VIEW+JOIN and no Move bit → nil.
  - `TestAuthorizeMoveSourcesTx`: {hidden, visible+Move} → returns only the visible; {visible without Move} → `errVoiceModPermission`.
  - `TestFilterAuthorizedSessionSpecsTx`: hidden+allowed → only allowed specs; denied+allowed → `errVoiceModPermission`; spec with `ChannelID==""` → error that is not `errVoiceModPermission`.
  - `TestVoiceModPreamble_AlternateSpellingSelfMove` (Review Focus 1): self-move request with the actor's own ID upper-cased in `:userId` is treated as self (no Move bit needed) → handler gets past the preamble (assert via Task 3 later; here assert `authorizeVoiceModWith` returns `ctx.self==true` through a test route registered in the test).
- [ ] **Step 3: Run.** `go test -count=1 ./internal/voice/ -run 'TestHasAllPerms|TestResolveActorVoiceChannel|TestFindVoiceChannelsTx|TestAuthorizeMove|TestFilterAuthorizedSessionSpecsTx|TestVoiceModPreamble'` → FAIL (undefined).
- [ ] **Step 4: Implement** the Interfaces above. Preamble order: path IDs via `requestid.Param` (400) → `checkMembership` (403 member) → unless `allowSelf && actor==target`: `HasPermission(..., "", perm)` (403 permission) → if `requireHierarchy && !self`: `CheckHierarchy` (403). Update the one caller of `findVoiceChannelTx` to `findVoiceChannelsTx` and keep its current behaviour (take index 0) until Task 3 rewires it.
- [ ] **Step 5: Run** the Step 3 command → PASS; then `go test -count=1 ./internal/voice/` → PASS (persistent handlers unchanged).
- [ ] **Step 6: Commit.** `git commit -m "feat: add channel-scoped voice moderation authority helpers"`

---

### Task 3: `ServerMove` reorder, grant authority and move audit

**Files:**
- Modify: `internal/voice/handlers.go:1708-1990` (`ServerMove`, `prepareModeratedMove`, replace `auditMoveIfCrossesHierarchy` with `auditVoiceMove`)
- Modify: `internal/voice/mod_authority.go` (add `revalidateVoiceMoveAuthorityTx`)
- Modify: `internal/voice/temp_grants.go:127,134-160,210-225`
- Modify: `internal/voice/export_test.go:300` (test-only zero-authority helper)
- Test: `internal/voice/move_test.go`, `internal/voice/temp_grants_test.go`, `internal/voice/temp_grant_fence_test.go:57`, `internal/voice/temporary_grant_cleanup_fence_test.go:174`

**Interfaces:**
- Consumes: Task 2 helpers.
- Produces:
  - `func revalidateVoiceMoveAuthorityTx(ctx context.Context, tx *sql.Tx, r *rbac.Resolver, serverID, actorID, sourceID, destID string) error` — membership, server Move (no hierarchy), `authorizeMoveDestinationTx(...,false)`, `hasAllPerms(destPerms, tempGrantAllow)`, `authorizeMoveSourcesTx` on `{sourceID}`; source dropped → `errTemporaryGrantSourceChanged`.
  - `var errTemporaryGrantAuthorityRequired = errors.New(...)` in `temp_grants.go`, returned before `BeginTx` when `authorization.authority == nil`.
  - `guardTemporaryGrantAuthorization`: credential guard only when `guardCredential`; authority closure always.
  - `func (h *Handler) prepareModeratedMove(c *gin.Context, serverID, actorID, targetID, sourceChannelID, targetChannelID string) (granted, ok bool)`
  - `func (h *Handler) auditVoiceMove(ctx context.Context, serverID, actorID, targetID, fromChannelID, toChannelID string, crossed, temporaryGrant bool)` — writes `voice_member_moved` when `crossed || temporaryGrant`; metadata keys `from_channel_id`, `to_channel_id`, `hierarchy_crossed`, `temporary_grant` (always present).
  - Test-only (in `export_test.go`): `func GrantTemporaryChannelAccessForTest(...)` passing a named closure `allowAllTemporaryGrantAuthorityForTest`.

- [ ] **Step 1: Update existing tests to the new rule** (they must still pass after the change and must fail if a destination check is removed):
  - `TestServerMove_WithPermission_HiddenChannel_GrantsTemp` (`move_test.go:89`), `TestServerMove_JoinInheritedViewDenied_GrantsTemp` (`:121`), `TestServerMove_StalePermissionCacheStillGrantsAccess` (`:147`), `TestServerMove_HierarchyBypass_AuditsCrossing` (`:213`): add `ts.CreateChannelOverride(t, to, "role", moverRole, int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice|rbac.PermSpeak), 0)`. In `:89` change the audit expectation to `assert.Equal(t, 1, auditMoveCount(...))` and assert metadata `temporary_grant=true`, `hierarchy_crossed=false`.
  - `TestServerMove_CrossServerTarget` (`:257`), `TestServerMove_NonVoiceTarget` (`:276`): expect `403` and body `Insufficient permissions` (was 400).
  - `temp_grant_fence_test.go:57`, `temporary_grant_cleanup_fence_test.go:174`: pass the test-only closure instead of `temporaryGrantAuthorization{}`.
- [ ] **Step 2: Write the failing new tests** in `move_test.go` (keep `moveTempOverrideExists` and `hideVoiceChannel` names and signatures unchanged):
  - `TestServerMove_MoverLacksDestinationView_403NoGrant`, `TestServerMove_MoverLacksDestinationJoin_403NoGrant`, `TestServerMove_DestinationDenyMove_403NoGrant`, `TestServerMove_SourceDenyMove_403NoGrant`, `TestServerMove_GrantNeededMoverLacksSpeak_403NoGrant`: each → `403`, body `{"error":"Insufficient permissions"}`, `moveTempOverrideExists==false`, `auditMoveCount==0`.
  - `TestServerMove_MissingDestination_SameAsNoAccess`: random UUID destination → body byte-identical to the no-access case.
  - `TestServerMove_TargetInHiddenChannel_409LikeNotInVoice`: body byte-identical to `TestServerMove_TargetNotInVoice`'s.
  - `TestServerMove_TargetInTwoChannels_UsesVisible` (Review Focus 2): rows in hidden `a` and visible `b` → 200 and the `voice_move` signal `from == b`.
  - `TestServerMove_SelfMove_NonMember403`, `TestServerMove_SelfMove_NoDestinationAccess403` (no override row), existing `TestServerMove_SelfMove` stays 200 with no override and `auditMoveCount==0`.
  - `TestServerMove_HierarchyCrossNoGrant_Audited`: one audit row, `temporary_grant=false`, `hierarchy_crossed=true`.
  - `TestServerMove_OwnerWithChannelDeny_Succeeds` (Review Focus 4).
  - `TestServerMove_PermissionRevokedBeforeGrant_403NoGrant`: in `beforeTemporaryGrantForTest`, insert a user DENY of VIEW for the mover on `to` → 403, no override row.
  - `TestServerMove_DestinationDeletedBeforeGrant` (Review Focus 3): in `beforeTemporaryGrantForTest`, delete `to` → 403 permission (not 500, not member body), no override row.
  - `TestServerMove_StaleCachePermissive_RefusedInTx`: warm the cache with one allowed request, then remove `PermMoveMembers` from the mover's role by direct SQL (no cache invalidation) → 403 from the transaction, no override row.
- [ ] **Step 3: Write the failing grant tests** in `temp_grants_test.go`: `TestGrantTemporaryChannelAccess_NilAuthority_ErrorNoRow` (`errors.Is(err, errTemporaryGrantAuthorityRequired)`, no row); `TestGrantTemporaryChannelAccess_ClosureRunsWithoutCredentialGuard` (`guardCredential=false`, closure returns sentinel → that sentinel, no row).
- [ ] **Step 4: Run.** `go test -count=1 ./internal/voice/ -run 'TestServerMove|TestGrantTemporaryChannelAccess|Fence'` → new tests FAIL against current code.
- [ ] **Step 5: Implement** per spec § Move steps 1-11: `ServerMove` calls `authorizeVoiceModWith(c, voiceModPreamble{perm: rbac.PermMoveMembers, allowSelf: true})` before parsing the body; in the decision transaction run membership/server Move, then `authorizeMoveDestinationTx`, then `findVoiceChannelsTx` + `authorizeMoveSourcesTx` (self-move skips the source filter and Move), then compute `crossed` with `CheckHierarchyTx` (error → `true`); self-move never calls `prepareModeratedMove`; grant closure is `revalidateVoiceMoveAuthorityTx`; after success call `auditVoiceMove` unless self. Remove the production `grantTemporaryChannelAccess` wrapper.
- [ ] **Step 6: Run** the Step 4 command → PASS; `go test -count=1 ./internal/voice/` → PASS.
- [ ] **Step 7: Commit.** `git commit -m "fix: authorize voice moves at both channels before reading voice state"`

---

### Task 4: Channel step for session actions and temp-access revoke

**Files:**
- Modify: `internal/voice/handlers.go:1356` (`userLevelAction`), `:1548` (`ServerDisconnect`), `:1616-1700` (`RevokeTempAccess`, remove pool-side `isVoiceChannelInServer` at ~`:1639`)
- Modify: `internal/voice/temp_grants.go:616-629` (revoke authority closure adds `requireActorVoiceChannelPermTx(..., rbac.PermMoveMembers)`)
- Test: `internal/voice/handlers_test.go` (near `TestUserMute`, `:766`), `internal/voice/disconnect_test.go`, `internal/voice/temp_access_revoke_test.go`, new `internal/voice/persistent_mute_scope_test.go`

**Interfaces:**
- Consumes: `filterAuthorizedSessionSpecsTx`, `requireActorVoiceChannelPermTx` (Task 2).

- [ ] **Step 1: Write the failing tests** (use `seedOutboxVoiceSession`, `requireOutboxQueued`, `readServerOutboxCommand`, `requireNoServerOutboxCommands`):
  - `TestUserMute_ChannelDenyMute_403NoOutbox`, `TestUserDeafen_ChannelDenyDeafen_403NoOutbox`, `TestServerDisconnect_ChannelDenyMove_403NoOutbox`: user DENY of the bit on the target's channel → 403 permission, `requireNoServerOutboxCommands`.
  - `TestUserMute_HiddenChannel_NotInVoiceBody`, `TestServerDisconnect_HiddenChannel_NotInVoiceBody`: body byte-identical to the genuine not-in-voice response (400 / 409), no outbox row.
  - `TestServerDisconnect_HiddenPlusAllowed_QueuesVisibleOnly` (Review Focus 2): 202, outbox commands only for the visible channel.
  - `TestServerDisconnect_DeniedPlusAllowed_403NoOutbox`.
  - `TestServerDisconnect_AdminWithChannelDeny_Queued` (Review Focus 4).
  - `TestServerMute_ChannelDenyMute_StillApplies` in `persistent_mute_scope_test.go` (D-168c pin): channel DENY of Mute → `/mute` 200 and `server_members.server_muted = true`.
  - `TestRevokeTempAccess_ChannelDenyMove_403`, `TestRevokeTempAccess_NoView_403`, `TestRevokeTempAccess_TextChannel_403`, `TestRevokeTempAccess_OtherServerChannel_403`: 403 permission and the temp override row still exists. Update `TestRevokeTempAccess_CrossServerIDOR` (`:274`) to expect 403.
  - `TestUserMute_StaleCachePermissive_RefusedInTx`: same pattern as Task 3.
- [ ] **Step 2: Run.** `go test -count=1 ./internal/voice/ -run 'TestUserMute|TestUserDeafen|TestServerDisconnect|TestServerMute_ChannelDeny|TestRevokeTempAccess'` → new tests FAIL.
- [ ] **Step 3: Implement.** After `captureServerVoiceCommandSpecsTx` in `userLevelAction` and `ServerDisconnect`, call `filterAuthorizedSessionSpecsTx`; `errVoiceModPermission` → 403 permission; empty result → existing not-in-voice response. In `RevokeTempAccess` delete the pool check and extend the closure.
- [ ] **Step 4: Run** the Step 2 command → PASS; `go test -count=1 ./internal/voice/` → PASS.
- [ ] **Step 5: Commit.** `git commit -m "fix: honour channel denies on voice session moderation"`

---

### Task 5: DM soft-mute rule and canonical IDs

**Files:**
- Modify: `internal/dm/handlers.go:3973-4055` (`DMUserMute`)
- Modify: `docs/api/openapi.yaml:~6862` (DM user-mute description)
- Test: `internal/dm/handlers_test.go` (extend near `TestDMUserMute`, `:4279`)

- [ ] **Step 1: Write the failing tests:** `TestDMUserMute_SelfTargetAlternateSpelling_400` (actor's own ID upper-cased → 400 self-target, no outbox row). Pin tests (pass on old code, recorded as the VULN-106 pin): `TestDMUserMute_GroupNonAdminParticipant_Queued` (202 + outbox row), `TestDMUserMute_OneToOnePeer_Queued`, `TestDMUserMute_NonParticipant_403NoOutbox`.
- [ ] **Step 2: Run.** `go test -count=1 ./internal/dm/ -run TestDMUserMute` → the alternate-spelling test FAILS; pins PASS.
- [ ] **Step 3: Implement.** Read `id` and `userId` with `requestid.Param` (400 on error), use the canonical strings everywhere in the handler, compare self on canonical values. Handler comment states the rule verbatim: "Any participant of the conversation may soft-mute an in-call peer; no admin role is required; applies to 1:1 and group conversations." Same sentence in the OpenAPI description.
- [ ] **Step 4: Run** `go test -count=1 ./internal/dm/` → PASS.
- [ ] **Step 5: Commit.** `git commit -m "fix: canonicalise DM soft-mute IDs and pin the participant rule"`

---

### Task 6: Canonical IDs in `RemoveMember` and `authorizeDMGroupAdmin`

**Files:**
- Modify: `internal/dm/handlers.go:1726-1800` (`RemoveMember`), `:3866-3890` (`authorizeDMGroupAdmin`)
- Test: `internal/dm/remove_member_reconciliation_test.go` (or a new `internal/dm/path_id_canonical_test.go`)

- [ ] **Step 1: Write the failing tests:** `TestRemoveMember_CreatorAlternateSpelling_403` (admin sends the creator's ID upper-cased → 403 `Cannot remove the group creator`, participant row still present); `TestRemoveMember_SelfLeaveAlternateSpelling_TreatedAsSelf`; `TestDMGroupAdmin_AlternateSpelling_SameAsCanonical` (one route that uses `authorizeDMGroupAdmin`: same status and same rows for both spellings).
- [ ] **Step 2: Run** `go test -count=1 ./internal/dm/ -run 'AlternateSpelling'` → FAIL.
- [ ] **Step 3: Implement** with `requestid.Param` for both path params in both functions; canonical strings flow into every comparison and query.
- [ ] **Step 4: Run** `go test -count=1 ./internal/dm/` → PASS.
- [ ] **Step 5: Commit** (own commit). `git commit -m "fix: canonicalise DM group member path IDs"`

---

### Task 7: Preamble equivalence matrix and lock-held proof

**Files:**
- Create: `internal/voice/moderation_preamble_test.go`

- [ ] **Step 1: Write the tests:**
  - `TestVoiceModeration_NonMemberUniform403`: routes {mute, unmute, deafen, undeafen, user-mute, user-deafen, disconnect, move, temp-access} × states {target in voice, not in voice, text destination, missing destination, same channel, malformed body} → every response `403` with body exactly `{"error":"Not a member of this server"}` (compare raw bytes).
  - `TestVoiceModeration_MemberWithoutBitUniform403`: same matrix for a member without the bit → exactly `{"error":"Insufficient permissions"}`.
  - `TestVoiceModeration_AuthorizedActorSeesDistinctAnswers`: positive control — the owner gets the distinct 200/202/400/409 per state.
  - `TestVoiceModeration_RefusedCallersTakeNoVisibilityLock`: open `tx` on `ts.DB`, call `rbac.LockServerVisibilityCapture(ctx, tx, serverID)`; with it held, the non-member move and the member-without-bit move each complete within 2 s; an authorized move started in a goroutine does not complete within 500 ms and completes after `tx.Rollback()`.
- [ ] **Step 2: Run against Task 1 base** (`git stash` or a scratch checkout of `6ba00d24`): `go test -count=1 ./internal/voice/ -run TestVoiceModeration` → the non-member matrix and lock test FAIL. Record the output for A14.
- [ ] **Step 3: Run on the branch** → PASS.
- [ ] **Step 4: Commit.** `git commit -m "test: pin uniform voice moderation refusals before voice state"`

---

### Task 8: Docs

**Files:**
- Modify: `docs/adr/0023-temporary-sbac-and-move-hierarchy-exception.md`, `docs/api/openapi.yaml`, `docs/architecture.md` (new `#### Voice moderation authority` before `### Media Plane`, `:874`), `docs/api/README.md` (if Task 1 counts changed again), `CHANGELOG.md` (`[Prerelease]`: `### Security`, a new `### Removed`, totals line `:39`), `.claude/rules/domains/backend.md` (tracked regular file)

- [ ] **Step 1:** Write each change listed in spec § Docs to update and § Upgrade notes for operators. CHANGELOG describes behaviour only; no finding identifiers.
- [ ] **Step 2: Verify.** `scripts/api/check-openapi-coverage.sh` → exit 0; `grep -c "VULN-" CHANGELOG.md` → `0`; `grep -n "authorize-action" -r docs services` → no matches; OpenAPI lint used by CI (see `.github/workflows/pr-ci.yml`) → pass.
- [ ] **Step 3: Commit.** `git commit -m "docs: record channel-scoped voice moderation authority"`

---

### Task 9: Exploit reproduction, mutation proof and gates

**Files:** none committed (scratch copy only).

- [ ] **Step 1: Exploit tests.** Copy the branch to a scratch directory; copy in the two reproduction tests attached to the Security issue (`move_vuln103_test.go`, `servermove_occupancy_oracle_vuln102_test.go`). Run with PostgreSQL and Redis. Expected: VULN-102 test fails at probe 1 on `403 Not a member of this server` (a 500 is not acceptable); VULN-103 test fails at its `require.Equal(200)` with 403 and no override row. Never commit these files.
- [ ] **Step 2: Mutation proof.** For each guard — destination VIEW, destination JOIN, destination Move, source Move, SPEAK on grant, grant closure call, unconditional closure invocation, nil-authority check, source visibility filter, session channel step, revoke channel step, `ErrChannelNotInServer` ordering, DM canonical compare, RemoveMember canonical compare — remove it, run its owning test, record the failing test name, restore. Every guard must have at least one failing test.
- [ ] **Step 3: Red-green record (A14).** Run each new finding test on `6ba00d24` (fail, quote) and on the branch (pass). VULN-106 pins pass on both by design; record that.
- [ ] **Step 4: Suites.** `go test -count=1 ./internal/voice/... ./internal/dm/... ./internal/api/...` → PASS; `go test -count=1 ./...` in `services/control-plane` → PASS; `go vet ./...` and `golangci-lint run` → clean.
- [ ] **Step 5: Quality gate.** `/verify-quality-gate` → 0 new blocker/critical, 0 new high-confidence hotspots, ≥ 80% coverage on new code.
- [ ] **Step 6:** Put the Step 1-3 records in the PR description. No commit.
