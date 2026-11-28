# Reservation growth bound implementation plan

1. Add migration 000208 with bounded shard and active-account counters, named
   quota errors, `AFTER INSERT` triggers on both message tables, and a guarded
   down migration. Keep 000206 unchanged.
2. Add database-backed tests for exact ceiling, cross-table accounting,
   concurrency, rollback of rejected sends, idempotent retries, deletion,
   account erasure, backfill coexistence, and guarded down behavior. Use a
   dedicated scratch `DATABASE_URL`; never run against the shared `concord`
   database or private Go caches.
3. Trace HTTP and WebSocket send error handling. Map both quota constraints to
   one stable client capacity response and operator log signal. Add only the
   response tests needed to verify the two ingress paths.
4. Update `docs/design/message-id-reservation-baseline.md`, migration runbook,
   API contract, and `CHANGELOG.md` with default ceilings, read-only
   utilization query, refusal semantics, and rollback restriction.
5. Run focused isolated database and package tests, migration lint, change
   counts, and the repository quality gate. Compare concurrent send load with
   and without the prospective quota trigger; revise the design if a single
   write lock causes material contention. Record unavailable runtime coverage.
6. Open a focused PR referencing the unresolved #403 finding and Security
   #154. Run bot arrival, exact-head independent and specialist reviews,
   disposition gate, and CI. Stop at the Phase 9 merge-approval checkpoint.

The required native architect dispatch was unavailable on this run: the host
returned `403 Daybreak isn't available for this model` before any design work.
The developer requested this proposed fix in automated ad hoc mode; this plan
records the inline design decision and does not claim the native gate passed.
