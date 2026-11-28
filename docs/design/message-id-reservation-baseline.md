# Message-ID Reservation Baseline

Migration 000206 is a dormant prerequisite for a future `message-v1` writer. It
does not select an E2EE format, change a route, enable a writer, or change
client-visible responses.

## Reservation contract

`message_id_reservations` is one permanent global UUID namespace shared by
`messages` and `dm_messages`. It deliberately stores only `message_id`: no
sender, scope, content fingerprint, timestamp, or message metadata is retained.
Insert triggers reserve every new ID atomically, ID-update triggers reject an
identifier change, and truncate capture prevents an administrative truncation
from reopening an ID. The temporary delete-capture triggers preserve a legacy
ID if deletion races the online baseline scan.

The database rejection is one generic unique-violation shape. A future writer
must keep its externally visible conflict response uniform for a mismatched
retry, a deleted or retired ID, a cross-scope ID, and a legacy ID; it must not
turn reservation state into an oracle.

## Operator procedure

1. Apply migration 000206. It performs DDL only; it intentionally does not
   scan existing message tables under an exclusive migration lock. It has a
   five-second DDL lock timeout: resolve the conflicting transaction and rerun
   the migration if the timeout is reached.
2. During a normal maintenance window, run `message-id-reservation-backfill`
   with the intended database URL and a bounded `--batch-size` (default 1,000).
   No total runtime deadline is imposed by default, so a large table can finish
   its keyset scan; `--timeout` supplies an explicit total deadline when an
   operator needs one. A timed-out or cancelled command leaves the gate closed
   and may be rerun, but begins its keyset scan again. The command takes a
   dedicated advisory lock, first rejects any pre-existing cross-table UUID
   collision, then keyset-scans each table in bounded batches.
3. The command rechecks the cross-table collision condition and both
   table-to-reservation anti-joins, then commits
   `message_id_reservation_rollout.backfill_complete = true`. It removes the
   temporary delete-capture triggers in a separate cleanup transaction. Trigger
   removal uses a transaction-local five-second lock timeout, so it fails rather
   than queueing an exclusive lock behind ordinary message traffic. A cleanup
   failure leaves the verified gate committed and the temporary triggers in
   place; rerunning the command revalidates the baseline and retries cleanup
   without rescanning either message table.

The operation never deletes rows to resolve a collision or make an anti-join
pass. Investigate and reconcile a failed database manually; do not set the gate
by hand.

## Future activation gate

A later message-v1 writer must fail closed unless the singleton rollout row
exists and `backfill_complete` is true. It must use the one global namespace for
both channel and DM writes and preserve the uniform conflict rule above. This
PR intentionally ships neither that writer nor a client capability gate.

The down migration is intentionally restrictive: it takes locks before its
checks and refuses once any message, reservation, or verified baseline exists.
It never drops retained reservation state just to make a downgrade succeed.

## Prospective issuance quota (migration 000208)

Migration 000208 bounds the growth identified after the baseline merged. It
does not remove or rewrite any reservation. It counts actual new channel and
DM message inserts after installation with a global ceiling of 5,000,000 and
an active-account ceiling of 250,000. The global count is permanent; account
counters are removed on account deletion. Both ceilings can be raised by an
operator only after checking free space. The initial limits are operational
headroom, not a claim that every self-hosted disk can hold that many rows.

Historical backfill and temporary delete/truncate capture do not spend this
prospective budget. Their possible new rows are bounded by the finite set of
pre-migration message IDs. The total reservation count can therefore exceed
the prospective ceiling; inspect the actual baseline before promotion.

An `AFTER INSERT` trigger charges only a message row that was really inserted.
A same-ID `ON CONFLICT` retry does not spend the budget. At either ceiling, a
new channel or DM send fails and its transaction rolls back the attempted
reservation. HTTP `POST /messages` returns 507 with
`code: message_storage_capacity`; WebSocket channel and DM sends emit an
`error` frame with the same code. No response distinguishes the account and
global ceilings. Deleting messages never returns quota.

Before deployment, record current size and disk headroom:

```sql
SELECT count(*) AS reserved_ids,
       pg_total_relation_size('message_id_reservations') AS reservation_bytes
FROM message_id_reservations;
```

Monitor the prospective budget with this read-only query:

```sql
SELECT issued, max_issued, max_issued - issued AS remaining,
       max_per_account
FROM message_id_reservation_quota WHERE singleton = TRUE;
```

For the active accounts nearest their individual ceiling:

```sql
SELECT usage.user_id, usage.issued,
       quota.max_per_account - usage.issued AS remaining
FROM message_id_reservation_account_usage AS usage
CROSS JOIN message_id_reservation_quota AS quota
WHERE quota.singleton = TRUE
ORDER BY usage.issued DESC
LIMIT 20;
```

If the host has capacity for more reservations, the operator may raise a
ceiling explicitly, for example:

```sql
UPDATE message_id_reservation_quota
SET max_issued = 6000000
WHERE singleton = TRUE AND max_issued < 6000000;
```

No automatic increase or counter reset occurs. The down migration refuses on
a populated installation; retain this migration and use a compatible binary
instead of dropping history or undoing a security bound.
