# Channel VIEW on Content Reads and WebSocket Fan-out Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A member denied a channel's VIEW gets 403 on its history, bulk history, pins and reactions, and receives none of its channel-scoped WebSocket events or typing frames (Concord-Voice/Security#167: VULN-085, VULN-091, VULN-074, VULN-081, VULN-059, VULN-132, VULN-133).

**Architecture:** One leaf package `internal/rbac/channelperm` owns the permission bits, the type-to-view-bit mapping and the single SQL definition of "viewer". REST reads test VIEW and READ_HISTORY on one resolved permission value. The hub gains three delivery primitives in a new file that resolve the audience at delivery time on the existing fenced lanes; deletes deliver to a viewer snapshot captured inside the deleting transaction. Typing relays gate the sender off the `Run` goroutine.

**Tech Stack:** Go, `database/sql` with `github.com/lib/pq` (`pq.Array` with `= ANY($1)` / `unnest`), gin, gorilla-style hub in `internal/websocket`, PostgreSQL integration tests via `testhelpers.SetupTestDB`.

**Spec:** `docs/superpowers/specs/2026-11-18-167-channel-view-fanout-design.md`

All paths below are relative to `services/control-plane/` unless they start with `docs/`, `.claude/` or `CHANGELOG.md` (repo root).

## Global Constraints

- No migration. No new event type. No payload shape change. Recipients only get subsets.
- Audiences only narrow: nobody receives a frame they do not receive today.
- No new permission cache. Every new fan-out decision reads fresh authority.
- No new database work on the hub `Run` goroutine.
- `Permission.Has` is any-bit with an Administrator bypass. Test bits one at a time; never pass a combined mask to `Has`. In SQL, "every bit of mask" is `(perms & mask) = mask`, never `!= 0`.
- The leaf package imports only the standard library and `github.com/lib/pq`; never `rbac`, `websocket`, `keyrotation`.
- Unknown channel type, resolver error, nil checker and timeout all deny, for owners and administrators too. Known types: `text`, `bulletin`, `voice`.
- No blank discards of errors in any line written or rewritten (`Scan`, `rows.Err`, `rows.Close`).
- Tests assert the outermost effect: socket frame, HTTP status, PostgreSQL row. Regression tests sit at the caller and are named by behaviour, like `internal/messages/send_message_view_gate_regression_test.go`.
- Public repo: no private repo names, paths, issue or PR numbers in code, tests, commits or docs. Commits use bare `type:` (no scope) and end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- New hub logic goes in new files; edits to `internal/websocket/hub.go` and `internal/channels/handlers.go` stay minimal.
- Quality gate: 0 new blocker or critical issues, 0 new high-confidence security hotspots, coverage on new code at least 80%.

## Review Focus

1. **A VIEW-only member (VIEW, no READ_HISTORY) loses live messages after a typing frame.** A typing denial must never be definitive in `applyBroadcastDeliveryResult`. Test: Task 7 `TestTypingReadersGate_ViewOnlyRecipientStaysSubscribed`.
2. **A kicked member still receives the `removed_user_id` exemption after the prune.** The exemption applies only to current server subscribers. Test: Task 4 `TestChannelEventToViewers_ExemptUserMustStillBeServerSubscriber`.
3. **Channel-only subscribers widen the audience.** The new primitives use server subscribers only. Test: Task 4 `TestChannelEventToViewers_ChannelOnlySubscriberNotAdded`.
4. **An owner on an unknown channel type is granted by an owner fast path.** Test: Task 2 `TestChannelPermParity_UnknownTypeDeniedForOwnerAndAdmin` and Task 3 `TestHistoryViewGate_UnknownTypeForbiddenForOwner`.
5. **A large server makes the list projection time out and silently drop every frame.** Bound the query with chunking and a timeout; on error nobody gets a frame and the drop is logged. Test: Task 4 `TestChannelListToViewers_ResolverErrorDeliversNothing`.

---

### Task 1: Leaf predicate `channelperm`

**Files:**
- Create: `internal/rbac/channelperm/channelperm.go`
- Create: `internal/rbac/channelperm/channelperm_test.go`
- Modify: `internal/rbac/types.go` (constants become aliases of the leaf values; `Has` unchanged)

**Interfaces:**
- Produces:
  - `const Administrator, ViewVoiceChannels, ViewTextChannels, SendMessages, ReadMessageHistory int64` (values moved from `rbac/types.go`, byte-identical)
  - `func ViewBit(channelType string) (bit int64, ok bool)` — `text`, `bulletin` → `ViewTextChannels`; `voice` → `ViewVoiceChannels`; else `0,false`
  - `func Has(perms, bit int64) bool` — one bit, Administrator bypass
  - `func CanView(perms int64, channelType string) bool`
  - `func CanReadContent(perms int64, channelType string) bool`
  - `func IsKnownType(channelType string) bool`

- [ ] **Step 1: Write the failing tests** in `channelperm_test.go`:
  - `TestCanView_Table`: rows for `text`, `bulletin`, `voice`, `""`, `"stage"` × perms {0, view-text, view-voice, Administrator}. `text`+view-voice → false; `voice`+view-text → false; unknown + Administrator → false.
  - `TestCanReadContent_RequiresBothBits`: view-only → false; read-history-only → false; both → true; Administrator on `text` → true; Administrator on `"stage"` → false.
  - `TestHas_CombinedMaskTrap`: `CanReadContent(ViewTextChannels, "text") == false` and `CanReadContent(ReadMessageHistory, "text") == false` (guards against an OR-ed mask).
  - `TestConstantsMatchRBAC` in package `rbac` (`internal/rbac/channelperm_alias_test.go`): `int64(rbac.PermViewTextChannels) == channelperm.ViewTextChannels`, and likewise for the other four.
- [ ] **Step 2: Run** `go test ./internal/rbac/channelperm/ ./internal/rbac/ -run 'CanView|CanReadContent|CombinedMask|ConstantsMatch' -v`. Expected: FAIL (package does not exist).
- [ ] **Step 3: Implement** the leaf. `CanView`/`CanReadContent` return false on `!ok` before reading `perms`. In `rbac/types.go`, define the five constants as `Permission(channelperm.X)`; confirm `Permission`'s underlying type converts from `int64` (if it is `uint64`, keep the leaf type identical to `rbac.Permission`'s underlying type and adjust signatures accordingly; record the choice in the commit body).
- [ ] **Step 4: Run** the same command. Expected: PASS. Then `go build ./...`.
- [ ] **Step 5: Commit** `feat: add a shared channel visibility predicate`.

### Task 2: One SQL definition of "viewer"; collapse the copies

**Files:**
- Create: `internal/rbac/channelperm/viewers.go`
- Create: `internal/rbac/channelperm/viewers_integration_test.go`
- Modify: `internal/rbac/channel_viewers.go:100-140` (call the leaf; known-type conjunct wraps the whole `WHERE`)
- Modify: `internal/rbac/resolver.go:808` `visibleChannelIDs` (known-type conjunct on the owner, administrator and override paths)
- Modify: `internal/rbac/resolver.go` (add `FilterReadableUserIDsForChannelFresh`, `VisibleChannelIDsForUsersFresh`)
- Modify: `internal/rbac/handlers.go` or wherever `CanReadChannelContent` fits best (add the wrapper)
- Modify (remove copies): `internal/messages/handlers.go:534-543`, `internal/media/handlers.go:2403`, `internal/channels/handlers.go:1102` (`viewPermForType`), `internal/websocket/hub.go:3180` and the private bit constants at `hub.go:2186-2188` — each caller switches to `channelperm.ViewBit`/`CanView`
- Test: `internal/rbac/channel_viewers_unknown_type_integration_test.go`

**Interfaces:**
- Consumes: Task 1.
- Produces:
  - `type Queryer interface { QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) }`
  - `func FilterUsers(ctx context.Context, q Queryer, serverID, channelID string, candidateUserIDs []string, alsoRequired int64) ([]string, error)`
  - `func AllViewers(ctx context.Context, q Queryer, serverID, channelID string) ([]string, error)`
  - `func VisibleChannels(ctx context.Context, q Queryer, serverID string, userIDs, channelIDs []string) (map[string][]string, error)`
  - `func rbac.CanReadChannelContent(perms Permission, channelType string) bool`
  - `func (r *Resolver) FilterReadableUserIDsForChannelFresh(ctx context.Context, serverID, channelID string, candidates []string) ([]string, error)`
  - `func (r *Resolver) VisibleChannelIDsForUsersFresh(ctx context.Context, serverID string, userIDs, channelIDs []string) (map[string][]string, error)`
  - Existing `FilterVisibleUserIDsForChannelFresh`, `FilterVisibleUserIDsForChannelTx`, `CanDistributeChannelKeyTx` keep their signatures.

- [ ] **Step 1: Write the failing integration tests** (`testhelpers.SetupTestDB`). Seed a server with owner, administrator, member; channels of type `text`, `bulletin`, `voice`, and `stage` written directly with `INSERT` (no CHECK constraint exists).
  - `TestChannelPermParity_AllTypesAllRoles`: for every (type, role, override ∈ {none, deny-view, deny-read-history}) the Go `CanView`/`CanReadContent` on `GetEffectivePermissions` equals membership in `FilterUsers(…,0)` / `FilterUsers(…,ReadMessageHistory)`, `AllViewers` and `VisibleChannels`.
  - `TestChannelPermParity_UnknownTypeDeniedForOwnerAndAdmin`: `stage` → owner and administrator absent from all three SQL shapes and from `visibleChannelIDs`.
  - `TestFilterUsers_RequiredMaskIsAllOf`: user with only READ_HISTORY and VIEW-deny is absent for `alsoRequired=ReadMessageHistory`.
  - `TestFilterVisibleUserIDsForChannelFresh_UnknownTypeFailsClosed` in `channel_viewers_unknown_type_integration_test.go` (caller-level regression for D-167e).
- [ ] **Step 2: Run** `go test ./internal/rbac/... -run 'ChannelPermParity|RequiredMaskIsAllOf|UnknownTypeFailsClosed' -v`. Expected: FAIL (undefined functions; on old `channel_viewers.go` the owner is returned for `stage`).
- [ ] **Step 3: Implement** the leaf SQL with `c.type IN ('text','bulletin','voice')` ANDed at top level, ids via `pq.Array(...)` and `= ANY($n::uuid[])`; check `rows.Err` and `rows.Close`. `VisibleChannels` is one set-based query; the caller chunks `userIDs` at 500. Point the rbac functions at the leaf; delete the five copies.
- [ ] **Step 4: Run** the Step 2 command, then `go test ./internal/rbac/... ./internal/messages/... ./internal/media/... ./internal/channels/... ./internal/websocket/...`. Expected: PASS, no behaviour change for known types.
- [ ] **Step 5: Commit** `refactor: give channel visibility one SQL definition and fail closed on unknown types`.

### Task 3: REST reads require VIEW and READ_HISTORY (VULN-085, VULN-091)

**Files:**
- Modify: `internal/messages/handlers.go:182-217` (`checkChannelAccess`: select `c.type`; one `GetEffectivePermissions`; `rbac.CanReadChannelContent`)
- Modify: `internal/messages/reactions.go:179-188` (`lookupMessageContext` selects `c.type`, `messageContext.channelType`) and `:594-604`
- Verify only: `internal/messages/pinning.go:611` (uses `checkChannelAccess`)
- Create: `internal/messages/history_view_gate_regression_test.go`
- Create: `internal/messages/reactions_view_gate_regression_test.go`
- Modify: `docs/api/openapi.yaml` (403 on `GET /channels/{id}/messages`, bulk at ~3227, pins at ~3567, `GET /messages/{id}/reactions` at ~10902)

**Interfaces:**
- Consumes: `rbac.CanReadChannelContent` (Task 2).

- [ ] **Step 1: Write the failing tests** (model on `send_message_view_gate_regression_test.go`, package `messages_test`):
  - `TestHistoryViewGate_ViewDenyForbidden`: override denies `PermViewTextChannels`, base role grants read-history; `GET /api/v1/channels/:id/messages`, `/messages/bulk`, `/pins` each return 403 with the existing forbidden body.
  - `TestHistoryViewGate_ReadHistoryDenyForbidden`: VIEW granted, read-history denied → 403 on all three.
  - `TestHistoryViewGate_UnknownTypeForbiddenForOwner`: channel type `stage` → 403 for member, administrator and owner.
  - `TestHistoryViewGate_ViewerAllowed`: both bits → 200.
  - `TestReactionsViewGate_ViewDenyForbidden`, `..._ReadHistoryDenyForbidden`, `..._ViewerAllowed` for `GET /api/v1/messages/:id/reactions`; the 403 body must not contain emoji or reactor ids.
  - `TestHistoryViewGate_ResolverErrorIs500`: a resolver stub returning an error → 500.
- [ ] **Step 2: Run** `go test ./internal/messages/ -run 'HistoryViewGate|ReactionsViewGate' -v`. Expected: FAIL (200 where 403 expected). Save this output for the PR (old-code failure evidence).
- [ ] **Step 3: Implement** the two handler changes and the OpenAPI 403 descriptions ("missing view permission or read-history permission on the channel").
- [ ] **Step 4: Run** the Step 2 command, then `go test ./internal/messages/...`. Expected: PASS.
- [ ] **Step 5: Commit** `fix: require channel view permission on history, pins and reaction reads`.

T1–T3 form a stand-alone cut that closes the two Medium findings.

### Task 4: Hub fan-out primitives

**Files:**
- Create: `internal/websocket/channel_event_fanout.go`
- Create: `internal/websocket/channel_event_fanout_test.go`
- Modify: `internal/websocket/hub.go` — only the request/result structs (`serverVoiceBroadcast`, `channelDeliveryRequest`, `channelDeliveryResult`) and the branch points in the worker (`hub.go:2850`) and in `enqueueServerBroadcastDeliveryResult` (`hub.go:3019`)
- Modify: `internal/api/router.go:142` area (the checker adapter implements the two new optional interfaces)

**Interfaces:**
- Consumes: Task 2 resolver methods.
- Produces:
  - `func (h *Hub) BroadcastChannelEventToViewers(ctx context.Context, serverID, channelID uuid.UUID, msg OutgoingMessage, exemptUserIDs ...uuid.UUID) bool`
  - `func (h *Hub) BroadcastChannelEventToSnapshot(ctx context.Context, serverID, channelID uuid.UUID, viewerIDs []uuid.UUID, msg OutgoingMessage) bool`
  - `type ChannelListEntry struct { ChannelID uuid.UUID; Value interface{} }`
  - `type ChannelListEvent struct { Type string; Data map[string]interface{}; ListKey string; Entries []ChannelListEntry }`
  - `func (h *Hub) BroadcastChannelListToViewers(ctx context.Context, serverID uuid.UUID, event ChannelListEvent) bool`
  - `type ChannelReadersChecker interface { FilterReadableUserIDsForChannelFresh(ctx context.Context, serverID, channelID string, candidates []string) ([]string, error) }`
  - `type ChannelListVisibilityChecker interface { VisibleChannelIDsForUsersFresh(ctx context.Context, serverID string, userIDs, channelIDs []string) (map[string][]string, error) }`

- [ ] **Step 1: Write the failing hub tests** (two real test clients subscribed to the server, a stub checker):
  - `TestChannelEventToViewers_OnlyViewerReceives`
  - `TestChannelEventToViewers_ChannelOnlySubscriberNotAdded`
  - `TestChannelEventToViewers_ExemptUserReceivesWithoutView`
  - `TestChannelEventToViewers_ExemptUserMustStillBeServerSubscriber` (pruned between snapshot and delivery → no frame)
  - `TestChannelEventToViewers_NilCheckerDeliversNothing`
  - `TestChannelEventToSnapshot_OnlySnapshotMembersReceive` and `..._EmptySnapshotDeliversNothing`
  - `TestChannelListToViewers_PerRecipientProjection` (payload `channels` order preserved, only visible ids)
  - `TestChannelListToViewers_EmptyProjectionSendsNoFrame`
  - `TestChannelListToViewers_ResolverErrorDeliversNothing`
  - `TestChannelEventFanout_PerChannelOrder` (update, update, delete on one channel arrive in order) and `TestChannelListToViewers_PerServerOrder`
- [ ] **Step 2: Run** `go test ./internal/websocket/ -run 'ChannelEventToViewers|ChannelEventToSnapshot|ChannelListToViewers|ChannelEventFanout' -race -v`. Expected: FAIL (undefined).
- [ ] **Step 3: Implement.** Enter through `serverVoiceBroadcast`; recipients are server subscribers only; lanes per the spec table (ToViewers and ToSnapshot on (server, channel); list on (server, `uuid.Nil`)); ToViewers sets `retryOnAuthorizationInvalidation`; none may reach `allowAllChannelDelivery`. Add a server-subscription re-check at enqueue if `enqueueServerBroadcastDeliveryLocked` lacks one. List payloads are marshalled per recipient in the worker.
- [ ] **Step 4: Run** the Step 2 command, then `go test ./internal/websocket/... -race`. Expected: PASS.
- [ ] **Step 5: Commit** `feat: add view-filtered channel event fan-out to the hub`.

### Task 5: Producers for created, updated, reordered and key_revocation (VULN-074, VULN-081)

**Files:**
- Modify: `internal/channels/handlers.go:1056`, `:1064` (`channel_created`, voice and linked text separately), `:1714` (`channel_updated`)
- Modify: `internal/channels/groups.go:1227` (`channels_reordered` → `BroadcastChannelListToViewers`, `ListKey: "channels"`)
- Modify: `internal/websocket/keyrotation_broadcast.go:24-68` (`key_revocation` → `BroadcastChannelEventToViewers` with `RemovedUserID` exempt)
- Create: `internal/channels/channel_metadata_view_gate_regression_test.go`
- Modify: `internal/websocket/keyrotation_broadcast_test.go`

**Interfaces:**
- Consumes: Task 4 primitives. Producers pass `context.WithTimeout(context.WithoutCancel(c.Request.Context()), channelAuthCtxTimeout)` and log when admission returns false.

- [ ] **Step 1: Write the failing tests:**
  - `TestChannelCreatedViewGate_HiddenChannelNotSentToNonViewer` (role with no view on the new channel's category override receives no `channel_created`; viewer does)
  - `TestChannelCreatedViewGate_LinkedTextFilteredSeparately`
  - `TestChannelUpdatedViewGate_HiddenChannelNotSentToNonViewer`
  - `TestChannelsReorderedViewGate_ProjectedPerRecipient`
  - `TestKeyRevocationViewGate_NonViewerExcluded`, `TestKeyRevocationViewGate_RemovedUserStillReceives`, `TestKeyRevocationViewGate_PrunedExMemberReceivesNothing`
- [ ] **Step 2: Run** `go test ./internal/channels/ ./internal/websocket/ -run 'ViewGate' -v`. Expected: FAIL (non-viewer receives the frame). Save output.
- [ ] **Step 3: Implement** the producer switches.
- [ ] **Step 4: Run** the Step 2 command, then `go test ./internal/channels/... ./internal/websocket/... ./internal/keyrotation/...`. Expected: PASS.
- [ ] **Step 5: Commit** `fix: send channel metadata and key revocation events only to channel viewers`.

### Task 6: channel_deleted from a delete-time snapshot (three producers)

**Files:**
- Modify: `internal/channels/handlers.go:1881-1915` (`deleteChannelTx`: `channelperm.AllViewers` on `tx` after the `FOR UPDATE` at `:1892`, before the `DELETE`; return the snapshot; snapshot error fails the request) and `:1852` (`BroadcastChannelEventToSnapshot`)
- Modify: `internal/keyrotation/rotator.go:25` (`Rotation` gains `DeletedChannelViewers map[string][]string`), `:249` (`deleteIncompleteChannelTx` takes `serverID`, snapshots under a savepoint; failure → roll back to savepoint, log, empty audience)
- Modify: `internal/websocket/keyrotation_broadcast.go:50-58` (`channel_deleted` → `BroadcastChannelEventToSnapshot`)
- Modify: `internal/users/delete_account.go:55`, `:911` (`ChannelDeletedBroadcaster func(serverID, channelID string, viewerIDs []string)`; snapshot under savepoint) and `internal/api/privacy_wiring.go:58-70`
- Create: `internal/channels/channel_deleted_snapshot_regression_test.go`
- Modify: `internal/keyrotation/rotator_test.go`, `internal/websocket/keyrotation_broadcast_test.go`
- Create: `internal/users/delete_account_channel_deleted_audience_test.go`

**Interfaces:**
- Consumes: `channelperm.AllViewers` (Task 2), `BroadcastChannelEventToSnapshot` (Task 4).
- Produces: `keyrotation.Rotation.DeletedChannelViewers`; `users.ChannelDeletedBroadcaster` new signature.

- [ ] **Step 1: Write the failing tests:**
  - `TestChannelDeletedSnapshot_ReachesPreDeleteViewersOnly` (user delete route)
  - `TestChannelDeletedSnapshot_SnapshotErrorFailsDelete` (500, channel row still present)
  - `TestRotationDeleteIncomplete_SnapshotsViewers` and `..._SnapshotErrorStillDeletes`
  - `TestKeyRotationChannelDeleted_OnlySnapshotReceives`
  - `TestAccountErasureChannelDeleted_OnlySnapshotReceives`
  - `TestChannelDeletedSnapshot_UpdateThenDeleteInOrder`
- [ ] **Step 2: Run** `go test ./internal/channels/ ./internal/keyrotation/ ./internal/websocket/ ./internal/users/ -run 'Snapshot|ChannelDeleted' -v`. Expected: FAIL. Save output.
- [ ] **Step 3: Implement.** Use `SAVEPOINT channel_viewer_snapshot` / `ROLLBACK TO SAVEPOINT` in the two security-driven deletes, as `initialDistributorCanRotateTx` does. `NewRotator`, `RecordKeyRevocationTx` and its three wrappers keep their signatures.
- [ ] **Step 4: Run** the Step 2 command, then the full suites of those four packages plus `./internal/api/...`. Expected: PASS.
- [ ] **Step 5: Commit** `fix: send channel_deleted only to members who could see the channel`.

### Task 7: Channel typing gates (VULN-132)

**Files:**
- Modify: `internal/websocket/hub.go:2194` (`rejectActiveMemberTimeout`) and `:2255` (`authorizeChannelPermissions`): split each into a goroutine-safe decision function and a `Run`-side reporter that calls `h.sendError`
- Create: `internal/websocket/typing_gate.go` (sender gate, readers query call)
- Modify: `internal/websocket/hub.go:4534` (`handleTyping` sets the sender gate and recipient mask on the request), the `BroadcastMessage`/`channelDeliveryRequest` structs, and the worker at `dispatchChannelDelivery` (`hub.go:2362`)
- Create: `internal/websocket/typing_view_gate_regression_test.go`

**Interfaces:**
- Produces:
  - `func (h *Hub) decideActiveMemberTimeout(ctx context.Context, serverID, userID uuid.UUID) (timedOut bool, until time.Time, err error)`
  - `func (h *Hub) decideChannelPermissions(ctx context.Context, serverID, channelID, userID uuid.UUID, channelType string, required ...int64) (bool, error)` — each bit tested separately
  - `BroadcastMessage.SenderGateUserID uuid.UUID`, `BroadcastMessage.RecipientAlsoRequired int64`

- [ ] **Step 1: Write the failing tests:**
  - `TestTypingSenderGate_TimedOutSenderNoFrame`
  - `TestTypingSenderGate_NoSendPermissionNoFrame`
  - `TestTypingSenderGate_NoViewNoFrame`
  - `TestTypingSenderGate_StopFrameGatedToo` (`is_typing:false`)
  - `TestTypingReadersGate_ViewOnlyRecipientStaysSubscribed` (no typing frame, still subscribed, receives the next message frame)
  - `TestTypingSenderGate_ResolverErrorNoFrameNoUnsubscribe`
  - `TestSendPath_UsesSameDecisionFunctions` (existing send tests still pass; timeout SQL appears once — assert via `grep -c` in the step 4 command below)
- [ ] **Step 2: Run** `go test ./internal/websocket/ -run 'TypingSenderGate|TypingReadersGate' -race -v`. Expected: FAIL. Save output.
- [ ] **Step 3: Implement.** Sender gate runs in the delivery goroutine before recipient work; readers via one `FilterReadableUserIDsForChannelFresh` call per frame; every denial on this path is non-definitive and never touches subscriptions. `handleTyping` adds no `Run`-side query.
- [ ] **Step 4: Run** the Step 2 command, then `go test ./internal/websocket/... -race`. Expected: PASS.
- [ ] **Step 5: Commit** `fix: recheck sender authority before relaying channel typing`.

### Task 8: dm_typing sender gate (VULN-133)

**Files:**
- Modify: `internal/dmblock/dmblock.go` (add `func CanSendNow(ctx context.Context, db *sql.DB, conversationID, userID string) error` — current participant plus `guardBlockedConversationTx` in a read-only transaction; returns `ErrUnavailable` on denial)
- Create: `internal/dmblock/can_send_now_test.go`
- Create: `internal/websocket/dm_typing_gate.go` (fixed slot pool, size 8; `channelAuthCtxTimeout`)
- Modify: `internal/websocket/hub.go:6499` (`handleDMTyping` offloads to the gate; offers to `dmBroadcast` without blocking)
- Create: `internal/websocket/dm_typing_gate_regression_test.go`

**Interfaces:**
- Produces: `dmblock.CanSendNow` as above.

- [ ] **Step 1: Write the failing tests:**
  - `TestCanSendNow_RemovedParticipantDenied`, `..._BlockedPairDenied`, `..._PendingReconciliationDenied`, `..._ParticipantAllowed`
  - `TestDMTypingGate_RemovedParticipantNoFrame`, `TestDMTypingGate_BlockedConversationNoFrame`, `TestDMTypingGate_NoFreeSlotDropsFrame`, `TestDMTypingGate_ParticipantFrameDelivered`
- [ ] **Step 2: Run** `go test ./internal/dmblock/ ./internal/websocket/ -run 'CanSendNow|DMTypingGate' -race -v`. Expected: FAIL. Save output.
- [ ] **Step 3: Implement** using the existing guard (no copied SQL).
- [ ] **Step 4: Run** the Step 2 command, then `go test ./internal/dmblock/... ./internal/websocket/... -race`. Expected: PASS.
- [ ] **Step 5: Commit** `fix: recheck DM participation and block state before relaying dm_typing`.

### Task 9: Invite join filtered by joiner visibility (VULN-059)

Precondition: the earlier batch change to `internal/invites/handlers.go` is on `origin/main`. Run `git fetch origin && git rebase origin/main` first and re-read `createPendingKeyRequests`.

**Files:**
- Modify: `internal/invites/handlers.go:245`, `:266-304` (`createPendingKeyRequests`)
- Create: `internal/invites/join_key_needed_view_gate_regression_test.go`

**Interfaces:**
- Consumes: `channelperm.VisibleChannels` (Task 2), `BroadcastChannelListToViewers` (Task 4).

- [ ] **Step 1: Write the failing tests:**
  - `TestJoinPendingKeyRequests_OnlyJoinerVisibleChannels` (rows in `pending_key_requests` only for channels the joiner can VIEW)
  - `TestJoinKeyNeeded_ProjectedPerRecipient` (`channel_ids` per recipient; recipient with none gets no frame)
  - `TestJoinPendingKeyRequests_ScanErrorIsReported` (no blank discard: a closed DB makes the function return/log an error, no frame)
- [ ] **Step 2: Run** `go test ./internal/invites/ -run 'JoinPendingKeyRequests|JoinKeyNeeded' -v`. Expected: FAIL. Save output.
- [ ] **Step 3: Implement** in one transaction; check `Scan`, `rows.Err`, `rows.Close`; remove the discards at `:245`, `:283`, `:287`.
- [ ] **Step 4: Run** the Step 2 command, then `go test ./internal/invites/...`. Expected: PASS.
- [ ] **Step 5: Commit** `fix: limit invite-join key requests to channels the joiner can see`.

### Task 10: Drift guard, docs, exploit re-run, full verification

**Files:**
- Create: `internal/websocket/server_broadcast_allowlist_test.go`
- Modify: `docs/architecture.md:338` (WebSocket hub paragraph)
- Modify: `.claude/rules/domains/backend.md` (new section "Channel visibility: one predicate, filtered fan-out")
- Modify: `CHANGELOG.md` (`[Prerelease]` → `### Security`, two entries; update the totals line)

- [ ] **Step 1: Write the failing test** `TestServerBroadcastAllowList`: parse every non-test `.go` file under `internal/` with `go/parser`; for each call to `BroadcastToServer`, `BroadcastToServerContext`, `BroadcastToServerAndPrune`, require the `Type` to be a string literal on the closed allow-list or the enclosing function to be in `{broadcastChannelGroup, <member_removed variable site>}`; fail if any of `channel_created`, `channel_updated`, `channel_deleted`, `channels_reordered`, `key_revocation`, `key_needed` appears. Include `TestServerBroadcastAllowList_DetectsViolation` running the checker on an in-memory source snippet.
- [ ] **Step 2: Run** `go test ./internal/websocket/ -run ServerBroadcastAllowList -v` with a temporary violating call to confirm it fails, then remove the call. Expected: FAIL then PASS.
- [ ] **Step 3: Write the docs:** architecture paragraph (filtered events, per-recipient projections, delete-time snapshot, typing gates, ordering guarantee, the open grant-signal behaviour); backend rule (one predicate in `channelperm`; no channel-scoped event on unfiltered `BroadcastToServer`; bits tested one at a time; fail closed); CHANGELOG entries in user terms.
- [ ] **Step 4: Re-run the two exploit tests** for VULN-085 and VULN-091 from the Security repository against this branch. Expected: each fails because the read returns 403, not because setup fails. Record the output in the PR body.
- [ ] **Step 5: Old-code evidence:** check out `6ba00d24` in a scratch worktree, copy each new regression test file in, run it, and confirm it fails; record one line per VULN in the PR body.
- [ ] **Step 6: Full verification:** `cd services/control-plane && go vet ./... && go test -race -p 1 ./internal/rbac/... ./internal/messages/... ./internal/channels/... ./internal/invites/... ./internal/websocket/... ./internal/keyrotation/... ./internal/users/... ./internal/dmblock/... ./internal/media/... ./internal/api/...`, then `make test`. Expected: all PASS. Then run `/verify-quality-gate`.
- [ ] **Step 7: Commit** `docs: record the channel visibility rule and filtered fan-out`.
