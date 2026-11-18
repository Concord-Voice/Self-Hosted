# Channel VIEW on Content Reads and WebSocket Fan-out Design

Issue: Concord-Voice/Security#167. Branch: `feature/security-167-channel-view-fanout`. Base: `6ba00d24`.

## Brainstorming record (automated run)

This spec was written in an unattended run. The project lead's answers are the accepted decisions D-167a to D-167f ("Accept all", 2026-11-18) and the design panel's output. The questions the brainstorming process asks were answered from those inputs:

| Question | Answer | Source |
|---|---|---|
| Why, and what does success look like? | A member denied VIEW must not read a hidden channel's content or metadata over REST or WebSocket. Success is every finding below closed by a test that fails on the old code. | the issue |
| Which approach? | Shared predicate in a leaf package, per-recipient filter in the hub, delete-time viewer snapshot. | D-167f |
| Gate per event? | VIEW for metadata, VIEW and READ_HISTORY for content and typing recipients. | D-167a |
| `key_revocation` recipients? | Current viewers plus `removed_user_id`. | D-167b |
| `dm_typing` here or elsewhere? | Here. | D-167c |
| Absorb the overlapping broadcast and pending-request filters? | Yes. | D-167d |
| Unknown channel type in viewer SQL? | Fail closed. | D-167e |
| Migration? | None. | decisions file |
| Grant signal for a member who gains VIEW mid-session? | Not decided. Listed under Open decision. | not settled |

Size: a project (many packages, new hub primitives). The full written design follows. The builder check was done by reading the document as the builder against the code at `6ba00d24`; its answers are in "Risks and items to verify in planning".

## Intent

A server member who is denied a channel's view permission can still read that channel's history, pins and reactions over REST, and still receives its metadata, key and typing frames over WebSocket. The rule "who may see this channel" is written in six places with three different behaviours for an unknown channel type, and neither the read path nor the event producers use any of them.

This change gives the rule one home, applies it to every content read and every channel-scoped event, and makes the typing relays recheck the sender. Findings closed: VULN-085, VULN-091 (Medium, exploit-confirmed), VULN-074, VULN-081, VULN-059, VULN-132, VULN-133 (Low).

## Goals, non-goals, anti-goals

Goals

- A member denied VIEW gets 403 on history, bulk history, pins and reactions, and receives no frame and no list entry for that channel.
- One predicate and one SQL definition of "viewer", shared by `rbac`, `websocket`, `keyrotation`, `messages`, `media`, `channels`, `invites` and `users`.
- Typing frames are relayed only when the sender could send a message right now.
- Unknown channel type, resolver error, missing checker and timeout all deny, for owners and administrators too.

Non-goals

- No migration, no new event type, no payload shape change.
- Live content frames sent through `BroadcastToChannelAuthorized` stay VIEW-only.
- `role_*`, `member_*`, `server_*`, `ownership_*` and `channel_group_*` stay on the unfiltered server broadcast. REST group reads are not visibility-filtered and group payloads carry no channel ids, so the WebSocket reveals no more than REST.
- Existing `pending_key_requests` rows for hidden channels, and the key-delivery routes in `channels/handlers.go`, are not touched here.
- No signal to a member who gains VIEW mid-session (see Open decision).

Anti-goals

- Nobody may receive a frame they do not receive today. Audiences only narrow.
- No new permission cache. Every new fan-out decision reads fresh authority.
- No new database work on the hub `Run` goroutine.

## Accepted decisions and constraints

| Id | Decision |
|---|---|
| D-167a | Metadata events (created, updated, reordered, deleted) require VIEW. Content (history, bulk, pins, reactions, typing recipients) requires VIEW and READ_HISTORY. |
| D-167b | `key_revocation` goes to current viewers plus `removed_user_id`. |
| D-167c | The `dm_typing` fix lands here. |
| D-167d | The per-recipient broadcast filter and the invite-join pending-request insert filter are absorbed here. |
| D-167e | An unknown channel type fails closed in the viewer SQL. |
| D-167f | Shared predicate in a leaf package, per-recipient filter in the hub, viewer set captured inside the deleting transaction. |

Constraints the design must hold:

1. `Permission.Has` is any-bit with an Administrator bypass (`rbac/types.go:167`). Bits are tested one at a time. A combined mask is never passed to `Has`.
2. `rbac` imports `websocket` and `keyrotation`, so the shared package imports none of `rbac`, `websocket`, `keyrotation`.
3. Only `Run` reads or writes the subscription maps and `h.clients`.
4. The prune that follows `member_removed` runs before any later server fan-out (`hub.go:1420-1430`, `:5019-5027`). The `removed_user_id` exemption is a filter exemption among current server subscribers. It is not a user-scoped route.
5. A dropped push is acceptable when authority is unavailable: `key_revocations` is durable and pulled, metadata is refetched.
6. No blank discards of errors in any line this change writes or rewrites.
7. Tests assert the outermost effect: the frame on the socket, the HTTP status, the row in PostgreSQL.

## Approaches

1. **Chosen: resolve the audience in the hub at delivery time, on the existing ordered and fenced lanes; snapshot the audience for deletes.** Authorization sits next to delivery and reuses the existing "an authorization result overlapped by a revocation is retried" fence. Cost scales with connected subscribers, not members.
2. Resolve the audience in each producer at commit time and hand the hub a user set. Smaller hub change and total order for free, but the audience is unfenced between snapshot and delivery, and the invite-join list would cost members × channels on every join.
3. Keep a visible-channel set per client in the hub. No database per event, but it is a new authorization cache whose correctness depends on every authority writer triggering revalidation.

A fourth idea, one ordered lane per server for all of these events, was dropped: the hub's lanes, mutation gates and authorization epochs are all keyed by (server, channel) (`serverVoiceDeliveryKey`, `hub.go:2379`, `:2514`, `:2526`). Separating the queue key from the fence key means restructuring the most delicate code in the hub for an ordering property no client needs (see Ordering).

## Design

### 1. Leaf package `internal/rbac/channelperm`

Imports the standard library and `github.com/lib/pq` only.

Go predicate:

```go
const (
    Administrator, ViewVoiceChannels, ViewTextChannels,
    SendMessages, ReadMessageHistory int64 = ... // values moved from rbac/types.go
)
func ViewBit(channelType string) (bit int64, ok bool)   // text, bulletin -> text bit; voice -> voice bit; else ok=false
func Has(perms, bit int64) bool                          // one bit, Administrator bypass
func CanView(perms int64, channelType string) bool
func CanReadContent(perms int64, channelType string) bool // CanView AND Has(ReadMessageHistory)
```

`CanView` and `CanReadContent` return false for an unknown type before they look at `perms`, so an owner or administrator is denied too.

SQL, one definition with one known-type conjunct:

```go
type Queryer interface {
    QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}
// Candidates who hold the view bit and every bit of alsoRequired. alsoRequired == 0 means viewers.
func FilterUsers(ctx context.Context, q Queryer, serverID, channelID string, candidateUserIDs []string, alsoRequired int64) ([]string, error)
// Every server member who can VIEW the channel. For in-transaction delete snapshots.
func AllViewers(ctx context.Context, q Queryer, serverID, channelID string) ([]string, error)
// For each user, the subset of channelIDs that user can VIEW. One set-based query, chunked by the caller's bound.
func VisibleChannels(ctx context.Context, q Queryer, serverID string, userIDs, channelIDs []string) (map[string][]string, error)
```

Rules for the SQL:

- `c.type IN ('text','bulletin','voice')` is ANDed at the top level of the `WHERE`, outside the owner, administrator and override branches. Today the owner and administrator branches skip the type test (`rbac/channel_viewers.go:113-125`).
- "Every bit of the required mask" is an equality test against the mask, as `rbac/resolver.go:892-905` already does. It is never `!= 0`.
- A parity test runs every channel type, including an unknown one written directly to the table, through both the Go predicate and each SQL function and requires the same answer.

`rbac` changes:

- `rbac/types.go` aliases the constants from the leaf. `rbac.CanReadChannelContent(perms Permission, channelType string) bool` is a thin wrapper.
- `FilterVisibleUserIDsForChannelFresh`, `FilterVisibleUserIDsForChannelTx` and `CanDistributeChannelKeyTx` keep their signatures and call the leaf. New resolver methods `FilterReadableUserIDsForChannelFresh` and `VisibleChannelIDsForUsersFresh` expose the other two shapes.
- `visibleChannelIDs` (`rbac/resolver.go:808`) maps an unknown type to the text bit and its owner and administrator fast paths return every channel id. It gains the same known-type conjunct on all three paths, so list resolvers and per-channel gates cannot disagree.

Copies removed: `messages/handlers.go:535`, `media/handlers.go:2403`, `channels/handlers.go:1102` (`viewPermForType`, which maps unknown to text), `hub.go:3180` and the private constants at `hub.go:2186-2188`.

`channels.type` is `VARCHAR(20)` with no CHECK constraint (migration 000003), so the fail-closed branch is reachable only by a direct table write. It is defence in depth and changes nothing for rows the API can create.

### 2. REST reads

- `checkChannelAccess` (`messages/handlers.go:184`) selects `c.type` with `c.server_id`, calls `GetEffectivePermissions` once, and tests `rbac.CanReadChannelContent`. Its callers are history (`:281`), bulk (`:376`) and pins (`pinning.go:611`).
- `lookupMessageContext` (`reactions.go:179`) selects `c.type` and carries it on `messageContext`. `GetReactions` (`:594`) uses the same predicate.
- A view denial, a read-history denial and an unknown type all return the existing 403 body, so the response does not tell the caller which bit was missing.

### 3. Hub fan-out primitives

New file `internal/websocket/channel_event_fanout.go`. Edits to `hub.go` are limited to the request and result structs and the branch points named below.

```go
// One channel, VIEW resolved at delivery. exemptUserIDs are delivered without a permission check
// when, and only when, they are current server subscribers.
func (h *Hub) BroadcastChannelEventToViewers(ctx context.Context, serverID, channelID uuid.UUID, msg OutgoingMessage, exemptUserIDs ...uuid.UUID) bool

// A channel whose row is gone. viewerIDs was captured in the deleting transaction.
func (h *Hub) BroadcastChannelEventToSnapshot(ctx context.Context, serverID, channelID uuid.UUID, viewerIDs []uuid.UUID, msg OutgoingMessage) bool

type ChannelListEntry struct { ChannelID uuid.UUID; Value interface{} }
type ChannelListEvent struct {
    Type    string                 // "channels_reordered" or "key_needed"
    Data    map[string]interface{} // every field except the list
    ListKey string                 // "channels" or "channel_ids"
    Entries []ChannelListEntry     // order preserved
}
// One frame per recipient, entries limited to channels that recipient can VIEW. No entries, no frame.
func (h *Hub) BroadcastChannelListToViewers(ctx context.Context, serverID uuid.UUID, event ChannelListEvent) bool
```

All three return queue admission only, like `BroadcastToServerChannelViewAuthorizedContext`.

Behaviour all three share:

- They enter through `serverVoiceBroadcast`, so `Run` snapshots recipients and a worker does the database work. The result returns to `Run` for delivery.
- Recipients are server subscribers only. The existing view-authorized path also adds channel-only subscribers (`hub.go:5074-5087`); these entry points must not, or the audience would grow.
- At delivery `Run` drops any recipient that is no longer a server subscriber. If `applyServerBroadcastDeliveryResult` does not already do this, add it.
- No request built here may reach `allowAllChannelDelivery`. A nil checker, a nil or empty snapshot, or an empty projection delivers to nobody.
- Per-recipient payload bytes for list events are marshalled in the worker.

Per primitive:

| Primitive | Lane key | Worker step | Fence |
|---|---|---|---|
| ToViewers | (server, channel) | existing channel-row read and fresh viewer query (`FilterVisibleUserIDsForChannelFresh`); exempt users skip the query | existing mutation gate, authorization epoch and `authzState` comparison (`retryOnAuthorizationInvalidation`) |
| ToSnapshot | (server, channel) | none; a recipient is allowed when its user id is in the snapshot | none, by definition: the audience is historical |
| ListToViewers | (server, nil) | one `VisibleChannels` query for recipients × entry channel ids | `authzState` comparison only |

The list lane has no per-channel mutation gate. That is deliberate: its payload is ids plus position, a revoked client drops the channel on `channel_access_revoked`, and a list entry for an unknown channel id is ignored by the desktop store (`channelStore.ts:581-590`).

View-authorized jobs take a slot from `purgeDeliverySlots` (capacity 8, `hub.go:2469-2478`). These events now share that budget with purge invalidations. A large purge can delay them; it cannot reorder them within a lane.

The hub reaches the two new query shapes through small optional interfaces beside `ChannelAudiencePermissionChecker`, implemented by the adapter in `api/router.go`. A checker that implements neither is treated as a failed authorization.

### 4. Producers

| Event | Producer | Change |
|---|---|---|
| `channel_created` | `channels/handlers.go:1056`, `:1064` | `ToViewers`, one call per channel. The voice channel and its linked text channel are filtered separately. |
| `channel_updated` | `channels/handlers.go:1714` | `ToViewers` |
| `channels_reordered` | `channels/groups.go:1227` | `ListToViewers`, `ListKey: "channels"` |
| `channel_deleted` | `channels/handlers.go:1852` | `ToSnapshot`. `deleteChannelTx` calls `channelperm.AllViewers` on the transaction after its `FOR UPDATE` and before the `DELETE`. A snapshot error fails the request. |
| `channel_deleted` | `keyrotation/rotator.go:249`, sent by `websocket/keyrotation_broadcast.go:50` | `ToSnapshot`. `deleteIncompleteChannelTx` takes the server id and snapshots each id it is about to delete. `Rotation` gains `DeletedChannelViewers map[string][]string`. |
| `channel_deleted` | `users/delete_account.go:911`, wired in `api/privacy_wiring.go:58-70` | `ToSnapshot`. `ChannelDeletedBroadcaster` becomes `func(serverID, channelID string, viewerIDs []string)`. |
| `key_revocation` | `websocket/keyrotation_broadcast.go:60-68` | `ToViewers` with `RemovedUserID` as the exempt user |
| `key_needed` | `invites/handlers.go:297` | `ListToViewers`, `ListKey: "channel_ids"` (see section 7) |

Snapshot failure in the two security-driven deletes (rotation, account erasure) must not block the delete. They take the snapshot under a savepoint, as `initialDistributorCanRotateTx` does for its check, and on failure roll back to it, log, and continue with no audience. The push is then dropped and clients converge on refetch.

`keyrotation` and `users` import the leaf directly. No function is injected, and `NewRotator`, `RecordKeyRevocationTx` and its three wrappers keep their signatures.

Producers pass a bounded context that is detached from request cancellation and log when admission returns false.

Members who lose VIEW through a role or override change already receive a user-scoped `channel_access_revoked` (`rbac/authority_tx.go:934-949`), and the desktop handler for it removes the channel, clears its messages and invalidates the cached key. They do not need `key_revocation`, so the exemption stays limited to `removed_user_id`.

### 5. Channel `typing`

Today `handleTyping` (`hub.go:4534`) checks the sender's subscription, and recipients get a cached single-bit VIEW check (`hub.go:2870`).

- `BroadcastMessage` and `channelDeliveryRequest` gain a sender gate (the sender's user id) and a recipient "also required" mask. `handleTyping` sets both. No other caller does.
- The sender gate runs in the delivery goroutine that already checks recipients, before any recipient work. The sender must be a member, not timed out, and hold VIEW and SEND, read uncached.
- The send path's helpers `rejectActiveMemberTimeout` (`hub.go:2194`) and `authorizeChannelPermissions` (`hub.go:2255`) call `h.sendError`, which reads `Run`-owned state, so they cannot be called from a worker. Split each into a goroutine-safe decision function and a `Run`-side reporter. The send path and the typing gate call the same decision functions. The membership and timeout SQL exists once.
- Recipients are resolved with one fresh readers query per frame (`FilterUsers` with `ReadMessageHistory`), not one resolve per recipient.
- Every denial on this path is silent and non-definitive. `applyBroadcastDeliveryResult` unsubscribes a recipient on a definitive denial. A member with VIEW but without READ_HISTORY legitimately stays subscribed for live messages, so a typing denial must never be definitive, and a sender-gate denial must not touch any recipient's subscription.
- `is_typing: false` frames are gated the same way as `true`.

`handleTyping` keeps its one existing channel-context read on `Run` and adds none.

### 6. `dm_typing`

Recipients are already limited to current participants (`handleDMBroadcast`, `hub.go:6566`). The missing part is the sender.

- Add a read-only function to `dmblock` that answers "may this user send to this conversation right now": the user is a current participant, and the conversation passes `guardBlockedConversationTx`, the guard the send path runs through `PrepareConversationTx` (`hub.go:5799`). It takes no row locks. If the guard cannot run lock-free as written, factor its predicate into one shared query used by both.
- `handleDMTyping` (`hub.go:6499`) keeps its subscription check on `Run`, then evaluates the gate off `Run` in a goroutine drawn from a fixed-size slot pool, with `channelAuthCtxTimeout`. No free slot, a denial, an error or a timeout drops the frame. On success the frame is offered to `dmBroadcast` without blocking.

Because the gate is the send-path guard, later changes to DM block rules reach typing without a second edit.

### 7. Invite join

`createPendingKeyRequests` (`invites/handlers.go:266`) is rewritten:

- In one transaction, read the joiner's visible channels (`VisibleChannels` for the single user) and insert `pending_key_requests` rows for those ids only.
- Send `key_needed` through `ListToViewers` with the inserted ids, so each recipient's `channel_ids` holds only channels that recipient can VIEW.
- Check `Scan`, `rows.Err` and `rows.Close`. The discards at `:283` and `:287` go, and so does the adjacent one at `:245`, which sits in the same diff context.

### 8. Drift guard and rule

- A source-scan test parses the non-test Go files under `internal/` and finds every call to `BroadcastToServer`, `BroadcastToServerContext` and `BroadcastToServerAndPrune`. Each call's event type must be a string literal on a closed allow-list, or the call site must be on a short named list of dynamic sites (`broadcastChannelGroup`, the `member_removed` variable). The six channel-scoped types (`channel_created`, `channel_updated`, `channel_deleted`, `channels_reordered`, `key_revocation`, `key_needed`) are not on it.
- `.claude/rules/domains/backend.md` gains a section "Channel visibility: one predicate, filtered fan-out". This is the second fix of this class after the send-path gate.

## Ordering and failure behaviour

Today every one of these events shares one queue. After this change:

- Events for one channel keep their order: created, updated, `key_revocation`, deleted. The delete rides the same (server, channel) lane as the events before it.
- List events for one server keep their order relative to each other.
- A list event and a single-channel event are not ordered relative to each other, and neither are events for two different channels.

The two cases that last point opens:

| Case | Effect | Why it is acceptable |
|---|---|---|
| `channels_reordered` overtakes `channel_created` for the same new channel | the client ignores the unknown entry and then adds the channel with its creation-time position | needs a create and a reorder inside one authorization round trip; corrected by the next channel fetch |
| the linked text channel's `channel_created` overtakes the voice channel's | the text channel is known before its voice channel | the desktop store resolves the link by lookup (`getLinkedTextChannel`), not by arrival order |

Failure behaviour: a resolver error, a timeout, a missing checker or an unknown type delivers nothing for that event. `ToViewers` retries through the existing bounded retry; the others do not retry.

## Reconciliation record

No specialist technical approach was supplied for this issue and the frontend panel was not run, so there was no panel position to reconcile. The questions the design philosophy left open were settled by reading the code. Where the philosophy was wrong, the evidence wins:

| Philosophy position | Outcome | Evidence |
|---|---|---|
| One ordered lane per server for all metadata and key events | Dropped. Per-channel lanes plus one list lane. | Lanes, gates and epochs are keyed by (server, channel). |
| Fall back to producer-resolved audiences if a server lane is not possible | Not adopted | Unfenced audience; join-path cost of members × channels. |
| Leaf package is standard library only; viewer SQL placement left open | The SQL lives in the leaf, which imports `lib/pq` | `keyrotation` imports neither `rbac` nor `websocket`, and `users` does not import `rbac`; injection would widen four exported `keyrotation` signatures. |
| Typing sender gate calls the send path's functions | Kept, with a required split | The helpers call `h.sendError` and cannot run off `Run`. |
| Typing recipients checked per recipient | One set-based readers query per frame | The existing check is cached and single-bit; an uncached two-bit check per recipient per frame is a database amplifier. |
| `dm_typing` needs participant-resolved recipients | Already true; only the sender gate is new | `hub.go:6566`. |
| Treat a rotation's denied users as removed principals unless the client clears the key on `channel_access_revoked` | Not needed | The desktop handler removes the channel and invalidates the key. |
| `channel_group_*` in scope if REST group reads are filtered | Out of scope | They are not filtered and carry no channel ids. |
| Run the count-update script and cite the quality-gate definition file | Neither file exists in this repository | The gate thresholds below are stated directly. |

Found during reconciliation, not in the philosophy: the view-authorized path's channel-only recipients; the definitive-denial unsubscribe trap for typing; the shared `purgeDeliverySlots` budget; the absence of a CHECK constraint on `channels.type`; `visibleChannelIDs` mapping unknown to text.

## Acceptance criteria

Quality gate (mandatory):

| Gate | Threshold |
|---|---|
| New blocker or critical issues | 0 |
| New high-confidence security hotspots | 0 |
| Coverage on new code | at least 80% |

Behaviour:

| # | Criterion | Asserted at |
|---|---|---|
| 1 | A member with a view-deny override gets 403 from history, bulk history, pins and reactions. So does a member with VIEW and a read-history deny. | HTTP status |
| 2 | An unknown channel type returns 403 on the four routes for a member, an administrator and the owner. | HTTP status |
| 3 | The two scan exploit tests for VULN-085 and VULN-091 fail because the read is denied, not because setup fails. | exploit test output |
| 4 | With two server subscribers, one without VIEW, the one without receives no `channel_created`, `channel_updated`, `channel_deleted` or `key_revocation` for that channel. The other receives each one. | socket frames |
| 5 | `channel_deleted` reaches the members who could VIEW the channel when it was deleted, from all three producers, and nobody else. | socket frames |
| 6 | `channels_reordered` and `key_needed` reach each recipient with only that recipient's visible channels. A recipient with none receives no frame. | socket frames |
| 7 | A subscriber named in `removed_user_id` who has lost VIEW still receives `key_revocation`. A kicked member who was pruned receives nothing. | socket frames |
| 8 | Invite join inserts `pending_key_requests` rows only for channels the joiner can VIEW. | rows in PostgreSQL |
| 9 | A timed-out member, a member without SEND and a member without VIEW each produce no `typing` frame. A subscriber with VIEW but without READ_HISTORY receives no `typing` frame, stays subscribed, and receives the next message frame. | socket frames |
| 10 | A removed participant and a participant in a blocked conversation each produce no `dm_typing` frame. | socket frames |
| 11 | Two updates to one channel arrive in order, and an update followed by a delete arrives in that order. Two reorders of one server arrive in order. | socket frames |
| 12 | A resolver error or timeout on any new path delivers nothing and returns 500 on the REST routes. | socket frames, HTTP status |
| 13 | The Go predicate and each SQL function agree for text, bulletin, voice and an unknown type, for member, administrator and owner. A combined mask grants on neither bit alone. | unit and integration tests |
| 14 | The allow-list test fails when a channel-scoped event type is sent through an unfiltered server broadcast. | test output |
| 15 | Docs to update (below) are changed in the same pull request. | diff |
| 16 | Every finding in the issue (VULN-085, VULN-091, VULN-074, VULN-081, VULN-059, VULN-132, VULN-133) has at least one exploit or regression test that fails on the code at `6ba00d24` and passes on the branch. The failing run on the old code is recorded in the pull request. | test output on both trees |
| 17 | The focused tests for each changed package pass, and the full suites of every affected package (`rbac`, `rbac/channelperm`, `messages`, `channels`, `invites`, `websocket`, `keyrotation`, `users`, `dmblock`, `media`, `api`) pass with `-race`, including their PostgreSQL integration tests. | test output |
| 18 | `docs/api/openapi.yaml`, `docs/architecture.md` and `CHANGELOG.md` are updated wherever behaviour changes. | diff |

Regression tests sit at the caller, vary channel type and permission, and are named by behaviour, as `send_message_view_gate_regression_test.go` is.

## Docs to update

- `docs/api/openapi.yaml`: 403 on `GET /channels/{id}/messages`, `/channels/{id}/messages/bulk`, `/channels/{id}/pins` and `/messages/{id}/reactions`, stating that it covers a missing view bit as well as missing read-history.
- `docs/architecture.md`, WebSocket hub paragraph (line 338): which events are view-filtered, which are projected per recipient, which use a delete-time snapshot, the typing gates, and the ordering guarantee.
- `.claude/rules/domains/backend.md`: the new rule section.
- `CHANGELOG.md`, `[Prerelease]` section: a `### Security` entry for the REST read gate and one for the filtered WebSocket fan-out and typing gates, in user terms, with the totals line updated.
- This spec, and the plan under `docs/superpowers/plans/`.

## Task breakdown

| Task | Content | Depends on | Parallel |
|---|---|---|---|
| T0 | Leaf package, `rbac` aliases and wrappers, known-type conjunct in all viewer SQL, removal of the five copies, parity tests | none | first |
| T1 | REST reads and their regressions, OpenAPI | T0 | with T2 |
| T2 | Hub primitives in the new file, recipient and delivery-time rules, hub tests | T0 | with T1 |
| T3 | Producers: created, updated, reordered, `key_revocation`; three delete producers with snapshots | T2 | with T5 |
| T4 | `typing`: split of the send-path helpers, sender gate, readers query | T2 | after T2, same hub area as T3 |
| T5 | `dm_typing`: `dmblock` read-only gate, bounded off-`Run` evaluation | T0 | with T3 |
| T6 | Invite join: filtered insert, projected `key_needed`, error handling | T2, and the predecessor change to `invites/handlers.go` being on main | after rebase |
| T7 | Allow-list test, architecture doc, backend rule | T3 to T6 | last |
| T8 | Re-run the two exploit tests and record the result | T1 | any time after T1 |

One pull request. Commits are ordered so that T0 and T1 stand alone: they close the two exploit-confirmed findings and can be cut as a separate pull request if review volume requires it.

## Risks and items to verify in planning

| Item | Why it matters | Status |
|---|---|---|
| `applyServerBroadcastDeliveryResult` re-checks server subscription at delivery | a prune between recipient snapshot and delivery must win | partly read: it checks the mutation gate, the `presenceAuthzState` epoch and the per-key authz epoch (`hub.go:3009-3060`); a subscription re-check at enqueue is not confirmed. Add it if absent, test either way |
| What bumps `presenceAuthzState` | it is the only fence on the list lane and on exempt recipients | not read |
| `guardBlockedConversationTx` takes no locks | the typing gate must not contend with DM sends | verified: one plain `SELECT EXISTS` (`dmblock/dmblock.go:670`), no `FOR UPDATE`. It takes `*sql.Tx`, so the read-only gate runs it in a short read-only transaction |
| Desktop `removeChannel` invalidates the channel key | basis for limiting the exemption to `removed_user_id` | stated by the handler's comment; body not read |
| Receiver-side expiry of typing indicators | a stop frame is dropped when the sender loses SEND mid-typing | not found |
| Mobile handling of subset lists | schema exists; no handler was found | not verified |
| Channel create and update validation limits `type` to known values | confirms fail-closed has no production effect | not verified |
| `rbac.Permission` converts cleanly to the leaf's `int64` constants | aliasing in `rbac/types.go` | not verified |
| Final shape of `invites/handlers.go` after the predecessor change | T6 edits the same function | predecessor not on main at `6ba00d24` |
| Race between the joiner's visible-channel read and the insert | a concurrent override change can leave one pending row for a newly hidden channel | bounded by the denial path's purge of pending requests; key delivery authorization is outside this change |

## Open decision

No event tells a member that they have gained VIEW on a channel. Today a hidden channel created during a session reaches every client through the unfiltered `channel_created`, which is the leak this change closes. After it, a member granted VIEW mid-session sees the channel on the next channel fetch, as already happens for channels created before their session began.

A correct grant signal needs the set of users who became viewers, which is the before/after difference the authority capture computes for denials only. Recommendation: add it with the next authority-capture change in this batch rather than here, and document the current behaviour in the architecture doc now. The project lead decides.
