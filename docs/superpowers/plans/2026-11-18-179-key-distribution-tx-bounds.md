# Key-Distribution Transaction Bounds Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop channel/DM key distribution from deadlocking the shared DB pool, stop redundant pending key-request rows, and page the pending-keys reader.

**Architecture:** Every per-target read inside a distribution transaction moves onto that transaction, behind a per-target SAVEPOINT. Each enqueue is preceded by an already-held check. The pending-keys reader is rewritten as one keyset-paged scan across channel and DM sources, with a bounded number of rows examined per request.

**Tech Stack:** Go, `database/sql`, lib/pq, PostgreSQL, gin, testify, `internal/testhelpers/testdb`.

**Spec:** `docs/superpowers/specs/2026-11-18-179-key-distribution-tx-bounds-design.md`

## Global Constraints

- No migration.
- No pool-wide `statement_timeout`. The pool runs migrations.
- In-tx timeouts: `lock_timeout = '5s'`, `statement_timeout = '15s'`, `idle_in_transaction_session_timeout = '15s'` via `SET LOCAL`. Pool DSN parameter: `idle_in_transaction_session_timeout=60000` (ms). nginx `/api/`: `proxy_read_timeout 60s`.
- Paging: default limit 100, max 200, scan cap 1,000 rows per request. Cursor = opaque: `created_at RFC3339Nano|source|id` (`source` ∈ {`channel`,`dm`}) sealed with AES-256-GCM under a per-process key, base64url-encoded. A full page always carries a cursor. A malformed, altered or pre-restart cursor, or a bad limit, returns 400.
- Authorization results must not change. The pool-backed `channelKeyAccess` stays for non-tx callers.
- PR text and comments cite VULN IDs and behavior only. This is a public repo.
- Test run: `cd services/control-plane && go test -race -p 1 ./internal/channels/... ./internal/database/...` (needs `DATABASE_URL` / `REDIS_URL` from the dev env; never private Go caches).

## Review Focus

1. A resolver error or timeout for one target must not poison the batch. Later targets still deliver (Task 1, `TestDistribution_TargetCheckErrorDoesNotPoisonBatch`).
2. Key reset followed by a stale distribution must keep the pending row until a fresh wrap lands (Task 3, `TestStaleSkip_AfterReset_KeepsPendingRow`).
3. A caller who can see no row in the first 1,200 must still reach a visible row at 1,201 within two pages (Task 4, `TestPendingKeys_ScanCapAdvancesCursor`).
4. A cursor from another user or a forged source value must not leak rows. Visibility applies regardless of the cursor (Task 4, `TestPendingKeys_ForgedCursorStillFiltered`).
5. Old clients sending no `limit`/`cursor` get a 200 with the first page and the same row shape (Task 4, `TestPendingKeys_NoParamsBackCompat`).

---

### Task 1: Transaction-scoped access check with savepoint (VULN-079)

**Files:**
- Modify: `services/control-plane/internal/channels/handlers.go` (`channelKeyAccess` ~2247, `distributionTargetAdmitted` ~2654)
- Test: `services/control-plane/internal/channels/keydist_tx_access_internal_test.go` (new, `package channels`)

**Interfaces:**
- Produces: `func (h *Handler) channelKeyAccessTx(ctx context.Context, tx *sql.Tx, channelID, userID string) (isMember, canView bool, err error)`. Same contract as `channelKeyAccess`: membership `SELECT` on `tx` first, `sql.ErrNoRows` returns `(false,false,nil)`, a nil resolver returns `(true,true,nil)`, otherwise `h.resolver.ResolveChannelPermissionsTx`.
- `distributionTargetAdmitted` keeps its signature and wraps the check in `SAVEPOINT keydist_target` / `RELEASE` / `ROLLBACK TO SAVEPOINT` on error, then runs the existing enrol path.

- [ ] **Step 1: Write failing tests**
  - `TestChannelKeyAccessTx_MatchesPoolVariant`: a table over owner, Administrator role, channel allow override, channel deny override, MFA-masked member, non-member, and a channel in another server. For each row, assert `channelKeyAccessTx` (inside a tx) == `channelKeyAccess` for `(isMember, canView)`.
  - `TestDistribution_NoPoolConnWhileTxOpen`: set `db.SetMaxOpenConns(1)`, open the distribution tx, call `distributionTargetAdmitted` with a 2s ctx deadline, and assert it returns before the deadline with `admitted` set. Today it blocks on a second connection.
  - `TestDistribution_TargetCheckErrorDoesNotPoisonBatch`: inside a tx, force the target-1 check to fail (pass a malformed-but-canonicalizable channel whose server lookup errors, or inject a resolver stub returning an error), then run target 2's check and an `INSERT` on the same tx. Assert that both succeed and that target 1 is enrolled in `pending_key_requests`.
- [ ] **Step 2: Run, expect FAIL** (`channelKeyAccessTx` is undefined; the 1-conn test times out).
- [ ] **Step 3: Implement `channelKeyAccessTx` and the savepoint wrapping.** If the equivalence table shows a mask difference, STOP and report it. Do not edit `internal/rbac/resolver.go`.
- [ ] **Step 4: Run, expect PASS.** Mutation check: swap `channelKeyAccessTx` back to `channelKeyAccess` in `distributionTargetAdmitted`; the 1-conn test must go red.
- [ ] **Step 5: Commit** `fix: run key-distribution access checks on the distribution transaction`

### Task 2: Timeouts (VULN-079 defense in depth)

**Files:**
- Modify: `handlers.go` at both distribution `BeginTx` sites (~2992 channel, ~4396 DM); `services/control-plane/internal/database/postgres.go` (`New`); `infrastructure/deploy/nginx/concordvoice.conf` (`/api/` block ~341-353)
- Test: `keydist_tx_access_internal_test.go`, `internal/database/postgres_test.go` (new or existing)

**Interfaces:**
- Produces: `func setDistributionTimeouts(ctx context.Context, tx *sql.Tx) error`, which issues the three `SET LOCAL`s from Global Constraints. It is called as the first statement after each distribution `BeginTx`; a failure rolls back and returns 500.
- Produces: `func withPoolSessionParams(databaseURL string) (string, error)`, which adds `idle_in_transaction_session_timeout=60000` to URL-form and key=value DSNs and leaves an explicit caller value unchanged.

- [ ] **Step 1: Write failing tests**
  - `TestSetDistributionTimeouts_AppliesInTxOnly`: after the call, `SHOW lock_timeout` == `5s` in the tx. After commit, a new query on the pool shows the default.
  - `TestWithPoolSessionParams`: URL form, key=value form, existing param kept, `postgres://` with query string.
  - `TestDistribution_LockTimeoutReturns503`: hold `FOR UPDATE` on the channel row from another conn and run the distribution handler. Assert 503 within ~6s and that no `channel_keys` row was written.
- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement.** Map `55P03`/`57014` to 503 with the handler's existing generic body. Add `proxy_read_timeout 60s;` to the nginx `/api/` location.
- [ ] **Step 4: Run, expect PASS.**
- [ ] **Step 5: Commit** `fix: bound key-distribution transactions with local timeouts`

### Task 3: Already-held guard and cleanup (VULN-080, VULN-207)

**Files:**
- Modify: `handlers.go` (`distributeOneChannelKey` ~2724, the enrol path in `distributionTargetAdmitted`, `notifyChannelKeyDistribution` ~2948/2953, `distributeOneDMKey` ~4320, `notifyDMKeyDistribution` ~4798/4808 and its caller's delivered list)
- Test: `keydist_already_held_internal_test.go` (new)

**Interfaces:**
- Produces: `func channelKeyHeldTx(ctx context.Context, tx *sql.Tx, channelID, userID string, keyVersion int) (bool, error)` and `func dmKeyHeldTx(ctx context.Context, tx *sql.Tx, conversationID, userID string, keyVersion int) (bool, error)`. `keyVersion` is the batch version after the epoch guard, never a `wrapped_key_versions` value.
- Stale skip: if held, return `distributionDuplicate` / `dmRecipientAlreadyHeld` and do not enqueue.
- Cleanup: the post-commit `DELETE` covers delivered and duplicate/already-held targets. The `channelDistributionTally` gains a `duplicate []string` field. The DM notify receives `delivered` plus already-held as a separate `cleanup []string` argument, so the notification fan-out is unchanged.

- [ ] **Step 1: Write failing tests**
  - `TestStaleSkip_TargetHoldsCurrentKey_NoPendingRow` (channel) and the `_DM` twin: insert a `channel_keys` row at v, distribute v with a stale `wrapped_key_versions` for that target, and assert that `pending_key_requests` has 0 rows for it and the outcome is duplicate.
  - `TestStaleSkip_TargetLacksKey_EnqueuesPendingRow`: the positive control for the existing self-heal.
  - `TestDuplicate_PendingRowDeletedAfterCommit`: a pre-existing pending row plus a duplicate outcome leaves the row gone after commit.
  - `TestStaleSkip_AfterReset_KeepsPendingRow`: `ReplaceMyKeys`-equivalent purge (delete the target's channel_keys row and bump the public key), then a stale distribution. The row is enqueued and survives.
  - `TestEpochGuardPrecedesTargets`: a distribution with a non-current `keyVersion` returns the epoch error, and no target helper ran (no pending rows, no key rows).
- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run, expect PASS.** Mutation: remove the held check, and the first test goes red.
- [ ] **Step 5: Commit** `fix: skip redundant pending key requests when the target already holds the key`

### Task 4: Paged pending-keys reader (VULN-080, VULN-207)

**Files:**
- Modify: `handlers.go` (`GetPendingKeyRequests` ~3204, `filterVisiblePendingRequests` ~3288, `appendDMPendingRequests` ~3313)
- Create: `services/control-plane/internal/channels/pending_keys_page.go`: cursor codec and page query
- Test: `pending_keys_page_internal_test.go` (new); update `handlers_test.go` `TestGetPendingKeyRequests_*`, `handlers_integration_2_test.go`, `coverage_create_revalidation_test.go`, `dm_rotation_integration_test.go` where they assert the old shape
- Modify: `docs/api/openapi.yaml` (~7161-7184)

**Interfaces:**
- Produces: `type pendingCursor struct{ CreatedAt time.Time; Source string; ID string }`, `func decodePendingCursor(s string) (pendingCursor, error)`, `func (c pendingCursor) encode() string`.
- Produces: `func (h *Handler) pendingKeyRequestsPage(ctx context.Context, callerID string, after *pendingCursor, limit int) (rows []pendingKeyRequest, next *pendingCursor, err error)`. It reads ordered `(created_at, source, id)` candidates from a `UNION ALL` of both sources in batches. It applies today's visibility rule, using a batched resolve per distinct server and memoized per `(channel, requester)` for channel rows (DM rows keep their participant rule), and stops when the page is full or 1,000 candidates have been examined. `next` is the last examined candidate when more rows may exist, and nil otherwise.
- Response: `{"pending_requests": [...], "next_cursor": "<string>" | null}`. Row JSON is unchanged.

- [ ] **Step 1: Write failing tests**
  - `TestPendingCursor_RoundTrip`, `TestPendingCursor_RejectsMalformed` (bad base64, missing field, unknown source, bad time) → 400 at the handler.
  - `TestPendingKeys_PageSizeAndCursor`: 250 visible rows, `limit=100`, three pages of 100/100/50, and the last `next_cursor` is null.
  - `TestPendingKeys_MixedSourcesStableOrder`: interleaved channel and DM rows with equal `created_at` come out in `(created_at, source, id)` order with no duplicates across pages.
  - `TestPendingKeys_ScanCapAdvancesCursor`: 1,200 invisible rows then 1 visible. Page 1 returns 0 rows with a non-null cursor; page 2 returns the visible row.
  - `TestPendingKeys_ForgedCursorStillFiltered`: a cursor pointing before another user's hidden rows returns none of them.
  - `TestPendingKeys_VisibilityMatchesLegacy`: the same fixture through the old `filterVisiblePendingRequests` result (captured before the change) and the new page gives the same set.
  - `TestPendingKeys_NoParamsBackCompat`, `TestPendingKeys_LimitBounds` (0, 201, non-numeric → 400; absent → 100).
- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement.** Order `UNION ALL` on `created_at, source, id` with a `WHERE (created_at, source, id) > ($cursor...)` keyset predicate and `LIMIT` per batch (batch = 200). Bound the loop by the scan cap.
- [ ] **Step 4: Run, expect PASS.** Update the legacy tests to the new shape.
- [ ] **Step 5: Update `openapi.yaml`** (`limit`, `cursor` query params; `next_cursor` response field; 400).
- [ ] **Step 6: Commit** `fix: page the pending key-request reader with a bounded scan`

### Task 5: Port the exploit tests as regressions

**Files:**
- Create: `services/control-plane/internal/channels/keydist_pool_deadlock_regression_test.go`, `keydist_pending_flood_regression_test.go`
- Source material (read-only, never copy the folder into the repo): the scan's `exploit_tests/key_distribution_pool_deadlock_vuln079_test.go` and `pending_key_flood_vuln080_test.go`.

- [ ] **Step 1: Port both with the assertion inverted.** Deadlock: `MaxOpenConns=4`, 8 concurrent 1,000-target distributions, and an unrelated `SELECT 1` on the pool completes within 2s while they run; every distribution returns 200 or 503 within 20s. Flood: N stale submissions for already-held targets write 0 rows on both branches.
- [ ] **Step 2: Mutation proof.** Revert Task 1's call-site change and the deadlock regression goes red; revert Task 3's guard and the flood regression goes red. Record both in the PR.
- [ ] **Step 3: Commit** `test: regression tests for key-distribution pool deadlock and pending-row flood`

### Task 6: Docs and rules

**Files:**
- Modify: `.claude/rules/domains/backend.md` (Database section): "A helper called while a transaction is open takes the `*sql.Tx` and issues every read on it; a pool read inside an open transaction can exhaust the pool."
- Modify: the deploy doc that lists DB/nginx settings (find it with `grep -rl proxy_read_timeout docs infrastructure`, and if none exists add a section to `docs/development.md`): the new pool parameter, the in-tx timeouts, and "if a connection pooler is added, move the pool parameter to `ALTER ROLE … SET`".

- [ ] **Step 1: Edit both.** **Step 2: Commit** `docs: record transaction-scoped helpers and key-distribution timeouts`

---

## Companion: managed deployment mirror PR

Apply Tasks 1-6 to the mirrored control plane. `handlers.go`, `postgres.go` and `resolver.go` are equivalent apart from line offsets, comments and one unrelated validation block, so cherry-pick and hand-merge. The nginx file differs: add `proxy_read_timeout 60s;` to its own `/api/` location by hand. Use its rule files `.claude/rules/backend.md` / `e2ee.md`. Run its `scripts/update-claude-md-counts.sh` at Phase 4. Do not add a migration.

## Companion: Desktop PR (merge after both server PRs deploy)

`desktop/src/renderer/services/e2ee/e2eeService.ts` (~1164-1194): follow `next_cursor` until it is null, at most 20 pages per sweep, treating rows as a set. `tests/mocks/handlers.ts` (~189): a paged handler. Tests: three-page drain, a cap at 20 pages, a server without `next_cursor` (old server) still works with a single page.
