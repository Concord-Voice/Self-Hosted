# Hidden-resource denial parity — Design

**Status:** Proposed (Phase 2 reconciled) · **Date:** 2026-11-27 · **Base:** `main` at 818a41f8
**Paths:** relative to `services/control-plane/internal/` unless stated.

## Summary

The issue named seven endpoints that serve channel-sensitive data. On `main`, six of them already decide access on the caller's effective channel permission, not on server membership alone. This change closes the three gaps that remain:

1. **Voice participants reveals hidden text channels.** `GET /channels/:id/voice/participants` answers `200 {"participants":[]}` for any text channel. Members hold the voice-view bit by default, so a member who passes the UUID of a hidden text channel learns it exists. A nonexistent ID answers 403.
2. **Attachment download reveals that a file still exists.** `GET /media/attachments/:file_id` answers 404 for a missing file and 403 for every denial. A caller whose access was revoked therefore learns that the file is still there.
3. **Three handlers return partial lists as complete.** `GetUnreadCounts`, `GetServerUnreadStatus` and `GetParticipants` skip a row that fails to scan and still answer 200. Desktop treats a per-server unread snapshot as complete, so a dropped row clears a real badge.

The change also adds byte-parity regression tests to the gates that are already correct, and documents every denial response in `docs/api/openapi.yaml`.

Message history, bulk history, pins and reactions still check only read-history, not the view bit. They belong to the sibling task and stay open until it lands.

## Decisions

| # | Question | Decision | Reason | Source |
|---|---|---|---|---|
| 1 | Scope | Ship only the residuals in this spec, using today's helpers, without waiting for the sibling | The sibling owns the shared predicate and the view-bit consolidation | Maintainer |
| 2 | Attachment denial response | Every download denial answers `404 {"error":"File not found"}`, byte-identical to a missing file | Removes the file-existence oracle | Maintainer |
| 3 | How participants rejects non-voice channels | Add `AND c.type = 'voice'` to the existing lookup's WHERE clause (`voice/handlers.go:664-668`) | A non-voice ID then takes the existing `sql.ErrNoRows` branch: same query, same work, same response as a nonexistent ID. Selecting the type and comparing it afterwards would add a branch that produces the same answer | System engineer, parent |
| 4 | Shape of the download check | One download-only function that writes no response. It runs the membership-and-type lookup, calls media's existing `channelViewPermission`, makes **one** `GetEffectivePermissions` call, and requires the view bit and `PermReadMessageHistory` on that bitfield. `rbac.ErrNotMember` maps to deny | Changes the approach's two `HasPermission` calls to one call. `HasPermission` is just `GetEffectivePermissions` plus `Has` (`rbac/resolver.go:122-131`), so the semantics are the same. This also leaves one error branch to test and narrows the work gap. The ErrNotMember mapping keeps `HasPermission`'s behaviour for a member removed mid-request (`resolver.go:125-127`) | Architect, refining the parent's position |
| 5 | Keeping missing and denied identical | One unexported writer, `writeAttachmentNotFound(c)`, used for the missing-file answer (`media/handlers.go:543`) and for every denial | Identical by construction, not by convention. This is the friend-request gate precedent in `backend.md`, where both refusals return through the same call | Architect |
| 6 | `checkReadHistoryPermission` | Delete it (`media/handlers.go:2519-2555`) | Its only caller (`:625`) is replaced | System engineer, parent |
| 7 | Upload | Upload keeps its 403s; `userHasChannelAccess` and `checkSendPermission` are unchanged | Upload names a channel the caller chose, and those helpers sit in the region the sibling will rewrite | System engineer |
| 8 | Work parity | Parity in status, body, headers and logging. The difference in work and timing is accepted and recorded (Residual R1) | `backend.md` § Auth & Security requires equal work, response and logging for a refusal designed to be indistinguishable. Equal work is not reachable here. A missing file has no channel to resolve. The resolver reads its cache first, so timing differs between two denied callers depending on cache state. Padding the missing-file path with dummy resolver calls would load every 404 and still not equalise. This is a recorded deviation for the maintainer's review | Parent, architect |
| 9 | Scan errors | Log, answer 500 with the endpoint's existing failure message, and return | A 200 with a missing row is a wrong answer, not a partial one | System engineer |
| 10 | Test seam for scan errors | Add `ArmRowValue` (about 30 lines) to `testhelpers/stmthook`, with tests at each calling handler | Real Postgres cannot produce these scan errors: every scanned column is NOT NULL or wrapped in COALESCE. `stmthook` already wraps every pooled query (`stmthook.go:276`), and `testhelpers.SetupTestServerWithRouterDB` already builds the router on a hooked pool (`channels/channel_group_guard_probe_test.go:28-29`). Moving the scan loop into a testable helper was rejected: `.claude/rules/domains/tests.md` says a helper-level test proves a branch exists, and only a test at the caller proves it is reached | Parent, architect |
| 11 | Mobile handling of 403 and 404 | Checked read-only during execution (plan Task 8) | — | Parent |
| 12 | Public-repo hygiene | No tracker numbers, vulnerability IDs or private repository names in code comments, tests or docs. Comments this change rewrites drop any such IDs | The repository is public | Parent |

## Per-item design

### 1. Voice participants (`voice/handlers.go:653-733`)

- Add `AND c.type = 'voice'` to the lookup's WHERE clause. A text, bulletin or unknown-type channel, visible or hidden, then answers through the existing ErrNoRows branch with `403 {"error": errMsgNotMember}`. That is byte-identical to a nonexistent ID and to a caller who is not a member.
- Keep the uncached voice-view check (683-692). `backend.md` requires participant reads to bypass the permission cache.
- Scan loop (714-725): on a scan error, log, answer `500 {"error": errMsgFetchParticipants}` and return.
- **Who sees a change:** Desktop requests participants only for voice channels (`ChannelList.tsx:517`), and treats any non-ok answer as a no-op. Desktop needs no change. Mobile is checked in Task 8.

### 2. Attachment download (`media/handlers.go:516-643`)

Add this method beside `userCanDownloadAttachment`:

`func (h *Handler) downloadChannelAccessAllowed(ctx context.Context, userID, channelID string) (bool, error)`

It writes no response and logs nothing. In order:

1. Run the same membership-and-type lookup as `userHasChannelAccess` (2403-2407). No row means `(false, nil)`.
2. If `h.resolver == nil`, return `(true, nil)` (allow on membership). Media tests depend on this mode.
3. `viewPerm, ok := channelViewPermission(channelType)`. If `!ok`, return `(false, nil)`, so an unknown type fails closed.
4. Call `h.resolver.GetEffectivePermissions(ctx, serverID, userID, channelID)`. If `errors.Is(err, rbac.ErrNotMember)`, return `(false, nil)`. Return any other error. Otherwise return `perms.Has(viewPerm) && perms.Has(rbac.PermReadMessageHistory), nil`.

`userCanDownloadAttachment` then writes every response itself:

- **Channel file:** call `downloadChannelAccessAllowed`.
- **DM file:** the existing `dmAttachmentDownloadAccessQuery`.
- **No scope** (unreachable under the tier-2 rule that a file has a channel or a DM, never both): deny.
- **Error:** `h.log.Error(...)` with a fixed message, then `500 {"error": errMsgFailedVerifyAccess}`.
- **Any denial:** `writeAttachmentNotFound(c)`, which is `c.JSON(http.StatusNotFound, gin.H{"error": errMsgFileNotFound})`. Line 543 calls the same writer.

**What a denial must look like:** no `X-File-*` headers, no `Cache-Control` from the success path, no ciphertext. No log line, the same as the missing-file path. The headers are already written only after access is granted (575-609). Keep it that way.

**Cleanups:**
- Delete `checkReadHistoryPermission`.
- Correct the comment at 620-623. It claims the check matches `messages.checkChannelAccess`, but that function has no view check.
- Correct the same claim in `media/handlers_test.go:1862-1867`.

**Dependency on the sibling:** this code calls media's `channelViewPermission`, which the sibling plans to remove. Whichever change lands second gets a compile error, so the conflict is loud, not silent. The fix is to call the sibling's shared predicate instead. Until then this is a temporary download-only copy of the view decision (Residual R2).

**`404 "File not found in storage"` (563) is not an oracle.** It is reachable only after access is granted.

### 3. Scan errors (`channels/handlers.go:2024-2031`, `2092-2103`; `voice/handlers.go:714-725`)

Replace each `continue` with a log line, a 500 using the endpoint's existing message (`errMsgFailedFetchUnreadCounts`, `errMsgFailedFetchServerUnread`, `errMsgFetchParticipants`) and `return`. Each 500 body is byte-identical to that endpoint's existing query-failure body. The `rows.Err()` checks stay as they are.

### 4. Regression locks (test only; these pass on `main`)

Existing correct gates get byte-parity assertions: GetChannel, MarkChannelRead, MarkServerRead skipping hidden channels, and server unread status leaving them out. Each lock is proven by deleting the gate it names (see the plan).

### 5. OpenAPI (`docs/api/openapi.yaml`)

| Path | Line | Change |
|---|---|---|
| `GET /channels/{id}` | 2841 | Add 400 and 404 ("missing, not a member, or not viewable — identical"), plus 500 |
| `POST /channels/{id}/read` | 3791 | Add 400, 403 ("missing, not a member, or not viewable — identical") and 500 |
| `GET /channels/{id}/voice/participants` | 4503 | Widen 403 to "missing, not a member, not a voice channel, or not viewable — identical". Add 500 ("never a partial list") |
| `GET /media/attachments/{file_id}` | 9109 | Description (9117): access needs the channel's view bit plus read-history, or DM participation and visibility. Delete 403 (9200). Widen 404 (9206) to "unknown, deleted, not a Tier 2 attachment, or not accessible — identical" |
| `GET /servers/unread-status` | 13951 | Hidden and history-denied channels are left out; 500 is never a partial list |
| `POST /servers/{id}/read` | 16610 | 403 for a non-member or a missing server; hidden channels are skipped |
| `GET /servers/{id}/unread` | 17270 | Same as above, plus hidden channels are left out |

`.claude/rules/internal-placeholders.md` covers `docs/api`. The `[internal]` lines (3859, 3973, 4047, 4123, 4195, 4309, 4383) lie outside these blocks and must stay byte-for-byte unchanged.

## The 403-vs-404 rule

On any endpoint keyed by a resource UUID, a resource the caller may not see must produce exactly the answer that endpoint gives for a resource that does not exist: status, body and headers. Each endpoint keeps its existing missing-resource code:

| Endpoint | Missing-resource answer |
|---|---|
| Channel metadata | 404 |
| Mark channel read | 403 |
| Voice participants | 403 |
| Attachment download | 404 (changed from 403 for denials by this spec) |

No endpoint flips between 403 and 404 globally. A distinct "you can see it but may not do this" answer is allowed only where the resource's existence is not secret. This change adds no such answer.

**Who sees a change:**
- **Download denials** change from 403 to 404. Desktop treats any non-ok status the same way (`AttachmentDisplay.tsx:337`, `OverflowMarkdownAttachment.tsx:101`). Its 403 attestation handler ignores these bodies (`apiClient.ts:1080`).
- **Participants on a non-voice channel** changes from 200 to 403. Desktop never requests that.
- **Unread endpoints on a scan error** change from a partial 200 to 500. Desktop's handling of that 500 is unverified (Task 8).
- **Mobile:** unverified (Task 8).

## Out of scope (owned by the sibling)

- The view-bit gate on message history, bulk history, pins and reactions (`messages/handlers.go:184-218`, `messages/reactions.go:594-604`).
- Merging the copies of the type-to-view-bit mapping (`channels/handlers.go:1112`, `media/handlers.go:2451`, `messages/handlers.go:536`, `messages/purge.go:903-907`, the hub).
- Making unknown channel types fail closed in `viewPermForType` and in the resolver's SQL and owner/admin fast paths.
- The shared predicate package and the hub broadcast filter.

## Residuals (accepted and recorded)

| # | Residual | Why accepted |
|---|---|---|
| R1 | Work and timing differ. A missing file costs one query; a denied one costs metadata lookup, membership and one resolver call. The same pre-existing shape exists in GetChannel, MarkChannelRead, and participants on a view-denied voice channel | Decision 8 |
| R2 | A temporary download-only copy of the view decision | Removed when the sibling's shared predicate lands |
| R3 | The download check reads the permission cache first, so a revoked view can keep working until the cache TTL expires | Unchanged behaviour; `backend.md` does not list download among the reads that must bypass the cache |
| R4 | History, pins and reactions still leak metadata for hidden channels | Until the sibling lands |
| R5 | Mobile and Desktop handling of the unread 500 is unverified | Task 8 |

## Acceptance criteria

| Criterion | Target |
|---|---|
| New and updated tests in `stmthook`, `voice`, `channels` and `media` | All pass with `-race -p 1` |
| Mutation proofs listed in the plan | Each turns its test red; results recorded in the PR body |
| Coverage of new code (Sonar) | ≥ 80% |
| New blocker or critical issues | 0 |
| New high-confidence security hotspots | 0 |
| `docs/api/openapi.yaml` | Updated for all seven paths above |
| Docs to update | openapi ships in the same PR; no other docs change |
| Placeholder check from `internal-placeholders.md` | Prints nothing |
| Migrations, routes, stores, new Go packages | None |

## Files touched

- **Production:** `voice/handlers.go`; `channels/handlers.go` (scan loops only); `media/handlers.go`.
- **Test infrastructure:** `testhelpers/stmthook/stmthook.go` and `stmthook_test.go`.
- **Tests:** `voice/handlers_integration_2_test.go` and a new `voice/participants_scan_error_test.go`; a new `channels/unread_scan_error_test.go`, plus `channels/handlers_test.go` and `channels/server_unread_status_shape_test.go`; `media/handlers_test.go` and `media/handlers_integration_2_test.go`.
- **Docs:** `docs/api/openapi.yaml`, this spec, and the plan.

**Text overlap with the sibling:** none in `channels/handlers.go` (the sibling edits 1102-1185). In `media/handlers.go` the only overlap is the call to `channelViewPermission`. `messages/*` is untouched.
