# Key-distribution transaction bounds — Design

Date: 2026-11-18 · Findings: VULN-079 (Medium), VULN-080 (Medium), VULN-207 (Low)

## Intent

Channel-key and DM-key distribution must not be able to stall the whole control plane, and a caller must not be able to make the server store or re-read key-request rows it does not need. Every control-plane route shares one database pool, so a stall in key distribution is an outage for the whole service.

## Current state

- `distributeChannelKeyBatch` holds the channel row lock (`FOR UPDATE` in `prepareChannelKeyDistribution`) and loops over up to 1,000 targets. For each target, `distributionTargetAdmitted` calls `channelKeyAccess`. That function queries `h.db` and calls `ResolveEffectivePermissionsUncached`, which opens its own transaction on the pool. So each target needs a second pool connection while the first one holds a lock that other requests queue on. Enough concurrent requests exhaust the pool and deadlock it (VULN-079).
- On a stale `wrapped_key_versions` entry, `distributeOneChannelKey` enqueues a `pending_key_requests` self-heal row without checking whether the target already holds the current key. The version is chosen by the caller (VULN-080). The DM branch (`distributeOneDMKey`, `enqueueDMKeyRequest`) has the same pattern (VULN-207).
- Post-commit cleanup deletes pending rows only for `delivered` targets.
- `GetPendingKeyRequests` and `appendDMPendingRequests` have no `LIMIT`. `filterVisiblePendingRequests` runs a permission resolve for each row.
- Existing unique indexes already cap the pending tables at one row per (context, user). The problem is redundant rows and an unbounded read, not unbounded growth, so **no migration is needed**.

## Goals

1. No code path takes a second pool connection while a distribution transaction is open.
2. A pending row is written only when the target lacks the key at the current epoch. It is deleted after commit when the request is fulfilled or found redundant.
3. Both pending-key readers are paged, and the work per request is bounded.
4. A blocked or idle distribution transaction cannot hold a connection indefinitely.

## Non-goals and anti-goals

- No set-based rewrite of the distribution loop (Option B). That belongs with the planned transactional DM distribution service.
- The self-heal enqueue stays. Dropping it (Option C) brings back the concurrent key-recovery-reset race it fixes.
- No change to who may distribute or view keys. Authorization results must stay byte-for-byte the same; only the connection they run on changes.
- **Anti-goal:** a pool-wide `statement_timeout`. The same `*sql.DB` runs migrations (`cmd/server/main.go`) and the adminctl tools, so a pool-wide timeout would kill long migrations.

## Approaches

| | Approach | Trade-off | Verdict |
|---|---|---|---|
| A | Run every per-target check on the transaction; check already-held before each enqueue; clean up on duplicate; page both readers | Smallest change that removes each root cause | **Chosen** |
| B | Set-based batch distribution | Also removes the per-target round trips; larger rewrite that collides with the planned DM service | Rejected: scope |
| C | Drop the self-heal enqueue | Simplest; brings back the recovery-reset race | Rejected |

## Design

### 1. Transaction-scoped access check (VULN-079)

- Add `channelKeyAccessTx(ctx, tx *sql.Tx, channelID, userID)`. It runs the membership `SELECT` on `tx` and resolves permissions with the existing `Resolver.ResolveChannelPermissionsTx`. That function reads and publishes no cache entry and opens no nested transaction.
- `distributionTargetAdmitted` takes the transaction and calls the Tx variant. The pool-backed `channelKeyAccess` stays for non-transactional callers such as `GetChannelKeys`.
- **Equivalence check (required test):** for the same fixture, `ResolveChannelPermissionsTx` and `ResolveEffectivePermissionsUncached` (channel scope, which goes through `ResolveEffectivePermissionsForChannelsFresh`) must return the same mask. Cover owner, Administrator, a channel allow override, a channel deny override, MFA-masked, and a channel in another server. If they differ, the Tx variant is fixed to match. The mutation-guard callers already rely on the Tx variant, so it is the one to keep correct.

### 2. Timeouts (VULN-079, defense in depth)

- Inside both distribution transactions, right after `BeginTx`: `SET LOCAL lock_timeout = '5s'`, `SET LOCAL statement_timeout = '15s'`, `SET LOCAL idle_in_transaction_session_timeout = '15s'`. This follows the existing `SET LOCAL` precedent in `mfaenforce`.
- Pool level: `idle_in_transaction_session_timeout` only (60 s), as a DSN run-time parameter set in `database.New`. Migrations never sit idle inside a transaction, so this cannot affect them.
- nginx `/api/` location: `proxy_read_timeout 60s` in `infrastructure/deploy/nginx/concordvoice.conf`.
- A timeout makes the handler return 503 with the existing generic error body, never a partial commit.

### 3. Already-held guard and cleanup (VULN-080, VULN-207)

- Channel branch: before `enqueueChannelKeyRequest` on a stale skip, run `SELECT 1 FROM channel_keys WHERE channel_id=$1 AND user_id=$2 AND key_version=$current` on `tx`. If a row exists, return `distributionDuplicate` and do not enqueue. The error-enrol path in `distributionTargetAdmitted` gets the same check.
- DM branch: the same check against `dm_channel_keys` before `enqueueDMKeyRequest`, returning `dmRecipientAlreadyHeld`.
- Post-commit cleanup: `notifyChannelKeyDistribution` and `notifyDMKeyDistribution` delete pending rows for `distributionDuplicate` / `dmRecipientAlreadyHeld` targets as well as `delivered`.

### 4. Paged reader (VULN-080, VULN-207)

- `GET /api/v1/e2ee/pending-keys?limit=&cursor=`. Default limit 100, maximum 200. The response becomes `{pending_requests: [...], next_cursor: "<opaque>" | null}`. The field is additive, so old clients keep working and read only the first page.
- One keyset cursor across both sources: a `UNION ALL` of channel and DM requests, ordered by `(created_at, id)`. The cursor is opaque: `created_at|source|id` sealed with AES-256-GCM under a key the process draws at start, sent as unpadded base64url (at most 138 characters). A capped scan's cursor can name a row the caller cannot see, so the client can neither read a cursor nor forge one. The server rejects with 400 a cursor that does not open or is malformed. A restart draws a new key, so cursors in flight then get 400 and the client starts again from the first page; the single control-plane replica makes that the only case.
- Visibility is applied before `LIMIT`, so a page is never filled with rows the caller cannot act on and later rows cannot starve. The per-row resolver call is replaced by one batched resolve per distinct server. Builder's call: the SQL or batched-resolver form, whichever keeps the results identical to today's `filterVisiblePendingRequests`, proved by a test.
- `docs/api/openapi.yaml` documents `limit`, `cursor` and `next_cursor`.

### 5. Client companion (Desktop)

`e2eeService.ts` follows `next_cursor` until it is null, capped at 20 pages per sweep. The mock in `tests/mocks/handlers.ts` gains a paged response. This is its own Desktop PR, and it must merge after the server change.

### 6. Control-plane mirror

The same control-plane change ships to the mirrored deployment repository in its own PR. Its nginx file differs, so it is edited by hand.

## Constraints

- PR text and code comments cite VULN IDs and behavior only.
- Integration tests run against Postgres through `DATABASE_URL`.

## Tests (acceptance)

1. Port the scan's pool-deadlock exploit test as an integration test. Use a pool with `MaxOpenConns=4` and 8 concurrent 1,000-target distributions. Every request completes or times out, and an unrelated query keeps getting a connection.
2. Port the pending-flood exploit test. A stale version for a target that already holds the current key writes no row, on both the channel and DM branches.
3. Cleanup: duplicate and already-held targets have their pending rows deleted after commit.
4. Pagination: page size, cursor round-trip, stable order across mixed channel and DM rows, malformed cursor returns 400, and a no-starvation case (first 200 rows invisible to the caller, row 201 visible, so it appears on page 1).
5. Resolver equivalence table, as in section 1.
6. Update the existing `TestGetPendingKeyRequests_*` and the integration tests that assert the old response shape.

## Docs

- `docs/api/openapi.yaml`: pending-keys pagination.
- `.claude/rules/domains/backend.md`: "no pool query inside an open transaction" also applies to helpers called from a transaction.
- Deploy docs: the new pool and nginx timeouts.

## Decisions from the builder check

1. **Errors inside the transaction keep skip-and-continue.** Each target's access check runs inside `SAVEPOINT target_check`. On an error (including a lock or statement timeout), the handler does `ROLLBACK TO SAVEPOINT`, then the existing skip-and-enrol path runs, and the batch continues. Without the savepoint, one resolver error would poison the transaction (25P02) and fail every later target.
2. **"Current epoch" means the batch `keyVersion` after the server's epoch guard has accepted it.** It never means a caller-chosen `wrapped_key_versions` value. The plan adds an assertion test that the epoch guard runs before any target is processed. DM: the same, using the DM branch's epoch-validated version.
3. **The resolver stays unchanged.** `channelKeyAccessTx` does the membership `SELECT` on `tx` first, so a missing channel or a non-member still returns `isMember=false` before the resolver is called. The nil-resolver fallback is kept. If the equivalence test finds a real difference in the permission mask, work stops and the difference is reported. Shared resolver code is not changed inside this PR.
4. **Ordering and cursor.** Rows are merged into one order, `(created_at, source, id)`, where `source` is `channel` or `dm`. The cursor carries all three. Desktop treats the list as a set (to be confirmed in the companion PR), so the order change is safe.
5. **Bounded scan.** One request examines at most 1,000 candidate rows. If the page is not full by then, `next_cursor` points at the last row examined. A cursor that moves past rows the caller cannot see keeps both "bounded work" and "no starvation". A full page always carries `next_cursor`, without looking at what follows it, so whether the cursor is set never depends on rows the caller cannot see; the sweep then ends on an empty page with a null cursor. Revised test 4: row 1,201 is visible after the first 1,200 invisible rows, and it is reached within two requests, each under the scan cap.
6. **Cleanup on duplicate is safe.** A row at the epoch-validated `keyVersion` can only exist if it was written after the target's latest key reset: `ReplaceMyKeys` and `RecoveryResetAccount` purge wrapped keys, and they serialize against distribution through the existing lock. So the pending request is redundant. A test covers reset, then a stale distribution: the row is kept until a fresh wrap lands.
7. **No connection pooler.** Nothing in the deploy config uses PgBouncer, so the startup parameter is safe. The deploy doc says to move the parameter to `ALTER ROLE ... SET` if a pooler is ever added.
8. **Mirror.** The mirror's `internal/channels` and `postgres.go` match apart from line offsets, comments and one unrelated validation block, so the same patch applies with hand-merging.

## Left to the builder

The exact timeout values may change if a test shows a legitimate 1,000-target distribution needs longer. Record the measured duration in the PR. Also the form of the visibility check (section 4).
