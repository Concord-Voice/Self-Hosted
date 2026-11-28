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
   five-second DDL lock timeout. If the timeout is reached, resolve the
   conflicting transaction and inspect the migration state. Migration 000206
   can be marked dirty even if PostgreSQL rolled back its DDL. Verify the
   actual schema and the last successfully applied migration. Follow the
   [dirty migration recovery procedure](../../services/control-plane/migrations/README.md#dirty-migration-state)
   to restore the verified clean version before retrying 000206. Confirm the
   migration completed cleanly before starting the backfill.
2. During a normal maintenance window, run `message-id-reservation-backfill`
   with the intended database URL and a bounded `--batch-size` (default 1,000).
   No total runtime deadline is imposed by default, so a large table can finish
   its keyset scan; `--timeout` supplies an explicit total deadline when an
   operator needs one. A timed-out or cancelled command leaves the gate closed
   if it stops before the gate commits. A rerun in that state begins the
   keyset scan again. The command takes a
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
