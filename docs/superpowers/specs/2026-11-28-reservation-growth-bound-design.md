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
a singleton global budget with an issued count and adjustable ceiling, plus a
per-account counter with `ON DELETE CASCADE`. Initial ceilings are **5,000,000
new reservations globally** and **250,000 per active account** after this
migration. The finite global headroom is a default, not a promise about host
disk capacity. Operators must compare it with free space and current reservation
size before promotion; raising a ceiling requires an explicit database change.

Attach one `AFTER INSERT FOR EACH ROW` quota trigger to each message table,
enabled `ALWAYS` like the reservation triggers. The existing `BEFORE INSERT`
trigger first reserves the UUID. The new trigger atomically increments the
global counter under its row lock and then the account counter. A missing
singleton, reached ceiling, or account-counter failure raises a named
check violation. PostgreSQL rolls back both the message and its reservation
with the statement. `AFTER INSERT` does not run for an `ON CONFLICT` retry
that inserts no row. Deletes and truncates never return quota.

The global singleton serializes new message issuance. Measure this additional
write contention in an isolated database before promotion. If it is material,
replace the singleton with bounded shards in a separately reviewed change;
do not omit the global cap to improve throughput.

Quota errors should be distinguishable from transient database errors in
server logs and consistently reported to clients as storage capacity refusal,
without exposing whether the account or global ceiling was reached. Update
the public API documentation and operator runbook with the response and a
read-only utilization query. The service must not silently auto-raise a cap.

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
