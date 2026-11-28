# Bound permanent message-ID reservation growth

## Problem

Migration 000206 reserves every new channel and DM message UUID permanently in
one ID-only table. An authenticated account can repeatedly create messages and
let them expire or be purged. Message cleanup cannot reclaim those reservations
without reopening an identifier, so ordinary message rate limits do not bound
the table's lifetime growth. This is the unresolved security finding on
Self-Hosted PR #403, which has already merged.

## Invariants

- A UUID used in either message table cannot be used again in either table,
  including after deletion, truncation, or account erasure.
- The reservation table remains ID-only. The quota may retain an active account
  UUID and a count, but no message ID, scope, content, or timestamp. Account
  deletion removes that account's counter; the global count never falls.
- An `INSERT ... ON CONFLICT (id)` retry that inserts no message spends no
  quota. A refused send leaves neither a new message nor a reservation.
- The online historical backfill remains resumable. It and temporary
  delete/truncate capture may add only identifiers from already existing
  messages; they do not spend the prospective issuance budget.
- The quota has a finite global ceiling and an independent per-account ceiling.
  Reaching either ceiling fails new message writes closed, across channel, DM,
  HTTP, WebSocket, and system-generated paths.

## Design

Add migration 000208, leaving the committed 000206 migration untouched. Create
64 fixed issuance budget rows with an aggregate ceiling, a separately stored
account-limit policy row, and a per-account counter with `ON DELETE CASCADE`. Initial
ceilings are **5,000,000 new reservations globally** (78,125 per shard) and
**250,000 per active account** after this
migration. The finite global headroom is a default, not a promise about host
disk capacity. Operators must compare it with free space and current reservation
size before promotion; raising a ceiling requires an explicit database change.

Attach one `AFTER INSERT FOR EACH ROW` quota trigger to each message table,
enabled `ALWAYS` like the reservation triggers. The existing `BEFORE INSERT`
trigger first reserves the UUID. The new trigger atomically increments the
account counter and then one server-selected random shard counter. When a
shard is full, it probes the other 63 rows so free headroom remains usable.
A missing policy row, reached ceiling, or account-counter failure raises a named
check violation. PostgreSQL rolls back both the message and its reservation
with the statement. `AFTER INSERT` does not run for an `ON CONFLICT` retry
that inserts no row. Deletes and truncates never return quota.

An initial singleton counter measured roughly 1,539 inserts/s versus 6,436
inserts/s without its quota trigger under an isolated eight-client, eight-account
microbenchmark. The shard design avoids that single write lock. The same
microbenchmark against the sharded migration measured roughly 3,437
inserts/s with no failed transactions. This local benchmark does not establish
production throughput; promotion still needs capacity and latency observation.

The cap converts unlimited disk growth into an explicit availability limit:
multiple abusive accounts can still spend the global headroom and stop new
sends until an operator adds capacity and raises the ceiling. This residual
denial risk is visible in quota usage; it is not represented as a permanent
solution to all message-flood abuse.

Quota errors should be distinguishable from transient database errors in
server logs and consistently reported to clients as storage capacity refusal,
without exposing whether the account or global ceiling was reached. Update
the public API documentation and operator runbook with the response and a
read-only utilization query. The service must not silently auto-raise a cap.
Channel and DM expiration-policy changes insert durable event messages in the
same transaction; at the ceiling they must roll back the policy and return the
same capacity code.

## Rollout and rollback

The migration is additive and does not scan historical messages or
reservations. Its counters start at zero. Thus the exact table bound after
installation is the finite pre-migration historical set plus at most the
global issuance ceiling. The backfill can add only IDs from that historical
set. Record the baseline count and free disk before deployment; monitor quota
usage during operation.

The down migration may remove the cap only from an unused installation. It
locks the message and quota tables, then refuses if either message table,
reservation table, or issuance counter has data. A populated installation
must retain the cap and use a compatible binary; deleting reservations or
resetting counters to force a downgrade is forbidden.

## Acceptance

| Criterion | Evidence |
|---|---|
| Bounded growth | Concurrent channel and DM inserts cannot exceed either ceiling; rejected writes leave no reservation. |
| No reuse | Existing cross-table, deleted-ID, and same-table retry cases still pass. |
| Backfill | Historical backfill and temporary delete/truncate capture preserve IDs without charging new issuance. |
| Erasure | Account deletion removes its per-account counter while leaving global usage and ID-only reservations. |
| Operations | Operator can read utilization and the service returns a stable capacity refusal at the ceiling. |
| Review | No new blocker or critical issue, no high-confidence security hotspot, and at least 80% coverage for new Go code. Migration and security specialists review the exact head. |
| Documentation | The reservation design, migration runbook, API response, and changelog reflect the behavior. |

This fix does not activate the dormant message-v1 writer or approve ADR-0051.
