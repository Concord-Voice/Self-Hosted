# Hidden-resource denial parity — Implementation plan

**Spec:** `docs/superpowers/specs/2026-11-27-hidden-channel-oracles-design.md`
**Base:** `main` at 818a41f8. Paths are under `services/control-plane/internal/` unless stated; run commands from `services/control-plane`.

**Applies to every task:**
- No tracker numbers, vulnerability IDs or private names in code, comments, tests or commits.
- Commits use a bare `type:` prefix.
- New tests use `testhelpers.JSONField`, `JSONElem` or `JSONAs`, never a single-return type assertion on decoded JSON.
- Hidden-channel fixtures deny only the view bit and keep read-history granted (`rbac/types.go:79`). Otherwise the history gate also produces the denial and the test pins nothing.
- `CreateChannelOverride` (`testhelpers/testserver.go:691`) does not clear the RBAC cache. Create overrides before the member's first request, or clear the server's cache explicitly.

### Task 0 — Branch (parent)

- Create a `fix/` branch from `origin/main`. Do not use a `feature/<N>-` name.
- Confirm `git log HEAD..origin/main` is empty.

### Task 1 — `stmthook.ArmRowValue` (test-writer)

**Files:** `testhelpers/stmthook/stmthook.go`, `testhelpers/stmthook/stmthook_test.go`

**Steps:**
1. Add `func (h *Hook) ArmRowValue(fragment string, column int, value driver.Value)`. It arms only the **next** query whose text contains `fragment`.
2. Add `func (h *Hook) RowValueHits() int`. It returns the number of rows rewritten since the last arm.
3. In `hookConn.QueryContext` (276), wrap the returned `driver.Rows` for an armed match. The wrapper's `Next(dest)` calls the inner `Next`, then sets `dest[column] = value` and counts a hit. The arm is cleared once it has matched.
4. This state is separate from `Arm` and `ArmCommit`; neither resets it.

**Tests and the mutation that must turn each red:**
- `TestArmRowValue_RewritesOnlyTheNextMatchingQuery`: a matching query scans the injected value; a second matching query and a non-matching query scan the real values. Mutation: stop `Next` assigning `dest[column]`.
- `TestArmRowValue_CountsRewrittenRows`: a three-row query reports 3 hits. Mutation: count once per query instead of once per row.

**Verify:** `go test -race -p 1 ./internal/testhelpers/stmthook/...`

**Depends on:** Task 0.

### Task 2 — Failing tests: participants and scan errors (test-writer)

**Files:** `voice/handlers_integration_2_test.go`, new `voice/participants_scan_error_test.go`, new `channels/unread_scan_error_test.go`

**Tests:**
- `TestGetParticipants_NonVoiceChannelMatchesNonexistent`
  - Cells, requested by a server member: a visible text channel; a text channel hidden by a default-role override denying `PermViewTextChannels`.
  - Each cell asserts 403 and a body byte-equal to that member's response for `uuid.New()`.
  - Positive control: a voice channel answers 200.
  - Red on `main` (200 `[]`). Mutation after Task 3: remove `AND c.type = 'voice'`.
- `TestGetParticipants_ScanErrorAnswers500`
  - Build with `stmthook.Open` and `SetupTestServerWithRouterDB`. Insert one participant.
  - `ArmRowValue` on a unique substring of the participants query, with column 4 (`is_muted`) set to `"not-a-bool"`.
  - Assert 500, `RowValueHits() >= 1`, and a body byte-equal to the body from `hook.Arm(<same fragment>, nil, stmthook.ErrInjected)` (the query-failure body).
  - Control: an arm whose fragment matches nothing gives 200 with one participant.
  - Mutation after Task 3: restore `continue`.
- `TestGetUnreadCounts_ScanErrorAnswers500`
  - Fragment `COUNT(m.id)::int AS unread_count`, column 1 set to `"x"`.
  - Every readable channel yields a row, so no messages are needed.
  - Same assertions and control as above. Mutation: restore `continue` (2028).
- `TestGetServerUnreadStatus_ScanErrorAnswers500`
  - The member joins, then the owner posts. The query INNER JOINs messages, so without an unread message there is no row and the test is vacuous.
  - Fragment: a unique substring of the server-status query; column 2 set to `"x"`.
  - Same assertions and control. Mutation: restore `continue` (2096).

Before arming, confirm each fragment matches only the handler's statement and none of the resolver's.

**Verify** (expect red before Task 3):
- `go test -race -p 1 -run 'TestGetParticipants_' ./internal/voice/...`
- `go test -race -p 1 -run 'ScanErrorAnswers500' ./internal/channels/...`

**Depends on:** Task 1.

### Task 3 — Participants predicate and scan branches (backend-developer)

**Files:** `voice/handlers.go` (664-668, 714-725); `channels/handlers.go` (2024-2031, 2092-2103)

**Steps:**
1. Add `AND c.type = 'voice'` to the lookup's WHERE clause.
2. Replace each of the three `continue`s with a fixed log line (no IDs interpolated), `c.JSON(http.StatusInternalServerError, gin.H{"error": <existing message>})` and `return`.
3. Touch no other lines in `channels/handlers.go`. Lines 1102-1185 belong to the sibling.

**Verify:** the Task 2 commands now pass; `golangci-lint run ./internal/voice/... ./internal/channels/...`

**Depends on:** Task 2.

### Task 4 — Failing tests: attachment download (test-writer)

**Files:** `media/handlers_test.go`, `media/handlers_integration_2_test.go`

**New tests:**
- `TestDownloadAttachment_DenialsMatchNotFound` (table)
  - Each cell asserts: 404; a body byte-equal to the same caller's response for a random missing file ID; the same `Content-Type`; no `X-File-*` header; body is not the ciphertext.

  | Cell | Harness and fixture | Gate mutation that must turn it red (after Task 5) |
  |---|---|---|
  | Non-member | Nil resolver (membership is the only gate there) | Drop the `server_members` join from the lookup |
  | View denied, history granted | Role carrying `PermReadMessageHistory` only | Skip the view-bit check |
  | History denied, view granted | Role carrying `PermViewTextChannels` only | Skip the history check |
  | Unknown channel type | Insert `type = 'forum'` directly (`channels.type` has no CHECK constraint, migration 000003:15); role grants text and voice view plus history | Map unknown types to the text bit |
  | DM non-participant | — | Force `hasAccess` to true |
  | DM participant, file hidden by a Clear range | — | Force `hasAccess` to true |

  - Controls: the owner gets 200 with the ciphertext; a DM participant gets 200.
  - Shared mutation: answer denials with `403 errMsgAccessDenied`. Every cell must go red.
- `TestUserCanDownloadAttachment_NoScopeAnswersNotFound` (in-package): both scope pointers nil gives a 404 body. Mutation: restore the 403.
- `TestDownloadChannelAccessAllowed_LookupError_500`: the handler on a closed DB, as in `TestCheckSendPermission_ChannelLookupError_500` (1344). Call `userCanDownloadAttachment` directly; expect `500 errMsgFailedVerifyAccess`. Mutation: map the error to a denial (404).
- `TestDownloadChannelAccessAllowed_ResolverError_500`: uses `mediaBrokenResolver` (1369). Expect 500. Same mutation.
- `TestDownloadAttachment_MemberRemovedMidCheckAnswersNotFound`
  - Build the handler and resolver on `stmthook.Open`.
  - `hook.Arm([<the decision lookup's fragment>, <the resolver's first server_members read>], <delete the member's server_members row on ts.db>, nil)`.
  - Expect 404, and `hook.Report()` seen equal to 2.
  - Mutation: remove the `ErrNotMember` mapping. The answer becomes 500.
- `TestUploadAttachment_ViewDeniedStill403`: upload to a view-denied channel through the caller of `validateChannelAttachment` (2171) still answers `403 {"error":"Access denied"}`. Mutation: route the upload denial through `writeAttachmentNotFound`.

**Updates to existing tests (403 becomes 404 with body parity; rename `…403` to `…NotFound`):**
- `media/handlers_integration_2_test.go:451`
- `media/handlers_test.go:1538`, `:1647`, `:1815`, `:1897`
- The `TestDownloadAttachmentDMVisibility` subtests at `:1661`, `:1682`, `:1710`, `:1714`, `:1735`, `:1756`, `:1777`
- Rebuild `TestDownloadAttachment_Resolver_NoViewPermission403` (1787) with read-history granted. Today its member has no permissions at all, so the history gate also denies and the test does not pin the view gate.

**Verify** (expect red before Task 5): `go test -race -p 1 -run 'Download|UserCanDownload|UploadAttachment_ViewDenied' ./internal/media/...`

**Depends on:** Task 1.

### Task 5 — Download decision and the single 404 writer (backend-developer)

**Files:** `media/handlers.go`

**Steps:**
1. Add `writeAttachmentNotFound(c *gin.Context)` and use it at 543.
2. Add `downloadChannelAccessAllowed(ctx context.Context, userID, channelID string) (bool, error)` exactly as in spec §2. It writes nothing and logs nothing.
3. Rewrite `userCanDownloadAttachment` (617-642) so that:
   - any denial calls `writeAttachmentNotFound`;
   - any error logs a fixed message and answers `500 errMsgFailedVerifyAccess`.
4. Delete `checkReadHistoryPermission` (2519-2555).
5. Correct the comment at 620-623, dropping any tracker IDs.
6. Leave `userHasChannelAccess`, `checkSendPermission` and `channelViewPermission` untouched.

**Verify:** the Task 4 command passes; `go build ./...`; `golangci-lint run ./internal/media/...`

**Depends on:** Task 4.

### Task 6 — Regression locks and the mutation record (test-writer)

**Files:** `channels/handlers_test.go`, `channels/server_unread_status_shape_test.go`, `media/handlers_test.go` (comment at 1862-1867)

**Regression locks (these pass on `main`):**
- **Extend** `TestGetChannel_HiddenChannelDeniedToMember` (331): the denied body is byte-equal to the same member's body for a nonexistent UUID. Mutation: delete the view gate (1175-1185).
- **Extend** `TestMarkChannelRead_ViewDeniedMemberBlocked` (577): body parity with the nonexistent-UUID 403. Mutation: delete the view gate (2150-2160).
- **New** `TestMarkServerRead_SkipsHiddenChannel`: one visible and one hidden channel; 200; a read-state row exists for the visible channel only. Mutation: pass every channel ID in place of `visibleIDs` (2222).
- **Extend** `TestServerUnreadStatusExcludesNonVisibleChannel` (29), without duplicating it. Add a cell where only the hidden channel has unread messages: the server must be missing from `server_ids`. Add a history-denied cell. Mutation: pass every channel ID in place of `readableIDs` (2081).

**Mutation record:** run every mutation named in Tasks 1, 2, 4 and 6 against the finished code. Revert each one afterwards. Record the pairs of mutation and red test for the PR body.

**Verify:** `go test -race -p 1 ./internal/voice/... ./internal/channels/... ./internal/media/... ./internal/testhelpers/stmthook/...`

**Depends on:** Tasks 3 and 5.

### Task 7 — OpenAPI (docs-writer)

**Files:** `docs/api/openapi.yaml` (repository root)

**Steps:** apply the spec §5 table to the seven paths. Do not edit any line containing `[internal]`. Commit, then run the check from `.claude/rules/internal-placeholders.md`. It must print nothing:

```bash
git fetch -q origin main
git diff --no-color --no-ext-diff --no-textconv --no-relative -U0 origin/main...HEAD -- \
    ':(top)client/admin' ':(top)docs/api' ':(top)infrastructure/deploy' ':(top)infrastructure/docker' ':(top)services' \
  | awk '/^diff --git /{h=0} /^@@/{h=1; next} h && /^[-+]/' | grep -F '[internal]'
```

If an OpenAPI or route-parity check exists (for example `scripts/api/test-extract-routes.sh`), run it. Check first whether it covers response codes.

**Depends on:** Tasks 3 and 5, so the docs match the code.

### Task 8 — Client compatibility check (parent, read-only)

**Mobile:** find the call sites for attachment download, channel participants, `/channels/:id/read` and the unread endpoints. Confirm that:
- a 404 on download is handled like a 403;
- a 403 on participants is a no-op;
- a 500 from an unread endpoint does not clear badges.

**Desktop:** confirm the handling of a 500 from `/servers/unread-status` and `/servers/:id/unread`.

Record the findings in the PR. If any client misreads the change, stop and return to the architect.

**Depends on:** Task 7.

### Task 9 — Final verification and hand-offs (parent)

- `go test -race -p 1 ./internal/voice/... ./internal/channels/... ./internal/media/... ./internal/testhelpers/stmthook/...`
- `go test -p 1 -coverprofile=cover.out ./internal/voice/... ./internal/channels/... ./internal/media/...` then `go tool cover -func=cover.out | grep -E 'GetParticipants|GetUnreadCounts|GetServerUnreadStatus|userCanDownloadAttachment|downloadChannelAccessAllowed|writeAttachmentNotFound'`. New lines must reach ≥ 80%.
- `golangci-lint run ./...`
- Run the repository's JSON-assertion guard if present (`scripts/check-no-single-return-json-assertion.sh`).
- Re-run the Task 7 placeholder check.
- Leave the sibling task a note on the download-only view decision in `media/handlers.go` and its compile-time dependency on `channelViewPermission`.
- **Reviewers (trusted fleet review table):** `security-reviewer` at Phase 4 and again on the PR head; `code-reviewer`; `docs-reviewer`; `scope-reviewer`. No migration or frontend reviewer applies.

**Depends on:** Tasks 6, 7 and 8.
