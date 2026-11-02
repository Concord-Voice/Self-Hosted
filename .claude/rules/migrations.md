---
description: Database migration conventions and safety rules
paths:
  - "migrations/**/*.sql"
---

# Migration Rules

Generic PostgreSQL migration discipline. Nothing here is project-specific — add
your own schema's invariants to your repo's copy rather than to this file.

## Naming & Structure
- Format: `NNNNNN_descriptive_name.up.sql` / `NNNNNN_descriptive_name.down.sql`
- Sequential numbering, zero-padded to 6 digits (if your `CLAUDE.md` tracks a "latest migration" count, cite it there rather than duplicating it here)
- Every up migration MUST have a corresponding down migration
- One migration file is one transaction under most runners (e.g. golang-migrate wraps each file in a tx) — this is why `CREATE INDEX CONCURRENTLY` and other non-transactional statements need their own file or an explicitly non-transactional runner mode

## Schema Safety
- No `DROP TABLE`/`DROP COLUMN` without confirmed data migration
- `ALTER TABLE` with `NOT NULL` must provide `DEFAULT` or target empty tables
- Use `CREATE INDEX CONCURRENTLY` for large tables
- Always `TIMESTAMPTZ`, never `TIMESTAMP`
- Foreign keys: appropriate `ON DELETE` behavior

### Phasing a non-null rollout on a live table

Adding a `NOT NULL` column to a hot table is a multi-migration sequence, not one
file. The lock class of each step is what forces the split:

1. Add the column **nullable** with a write-time default and no constraint, so
   replicas still running the old code can update pre-backfill rows.
2. Backfill existing rows in its own transaction.
3. Add a `NOT VALID` check constraint — only after the backfill — under a short
   `ACCESS EXCLUSIVE` lock.
4. `VALIDATE CONSTRAINT` in a separate transaction; validation takes only
   `SHARE UPDATE EXCLUSIVE`, so it does not block writes during the table scan.
5. Use the validated constraint to make `SET NOT NULL` metadata-only, then drop
   the proof constraint.

Do not collapse these phases. Because each file is one transaction, a combined
migration holds `ACCESS EXCLUSIVE` across the backfill or the validation scan —
which is the outage. Keep every validation before the first `ACCESS EXCLUSIVE`
operation, and hold multi-table locks in one canonical order across every up
and down migration so concurrent migrations cannot deadlock.

## Data Integrity
- Primary keys: prefer UUID or BIGSERIAL
- Unique constraints where business logic requires
- Default values must be sensible
- Audit columns: `created_at TIMESTAMPTZ DEFAULT NOW()`, `updated_at TIMESTAMPTZ DEFAULT NOW()`
- A case-insensitive uniqueness rule needs a functional unique index (e.g. on `LOWER(col)`) plus normalization at **every** write boundary — an index alone does not make identity case-insensitive on the read paths
- A migration that folds existing data (case-fold, dedupe, merge) is irreversible: its down migration can only drop the index, not restore the original values. Say so in the file, and fail the up migration closed (a `DO` block that `RAISE`s) if pre-existing rows would collide

## Down Migrations
- Must reverse ALL up migration changes
- Handle partial success scenarios
- Use `CASCADE` only when necessary and safe
- A guarded destructive down migration must take the lock **before** it tests for emptiness, so a concurrent insert cannot commit between the check and the `DROP`. Rollback must fail while any un-migrated state remains
- Keep guarded downgrades narrow: an exact one-version transition under the migration advisory lock, with explicit pre-assertions, is safe; generalizing it into arbitrary down-migration support is not

## Security
- No plaintext secrets or credentials in SQL
- Sensitive columns use appropriate types (BYTEA for encrypted, TEXT for hashed)
- RBAC-related migrations require security review
- No string concatenation for data manipulation — use parameterized queries
- Prefer schemas that cannot hold the sensitive value at all over schemas that hold it and rely on access control — an absent column is a stronger privacy guarantee than a protected one
- Audit/receipt tables record counts, context, and ranges only — never message content, usernames, or other payload. Actor/target FKs use `ON DELETE SET NULL` so the receipt survives account deletion

## When to Partition a Large Table

Partition a high-volume table (typically the append-only event/message table)
only when warning signs actually appear. Both premature partitioning and
reaching for the wrong tool under pressure are anti-patterns — this section
codifies the trigger criteria so the decision is data-driven, not speculative.

**Partition only when one of these holds:**
- Any single query p99 > 100ms on that table
- Any single partition-key value (e.g. one channel, one tenant) exceeds 100k rows
- Total table > 10M rows AND index bloat is observed — a high `pg_stat_user_tables.n_dead_tup` ratio, or `pgstattuple` reporting low tuple density (`ANALYZE` alone only refreshes planner statistics; it does not surface bloat)

**Recommended approach:** PostgreSQL declarative partitioning by
`hash(<partition key>)`. Reaching for a different datastore is usually the wrong
response to these signals — declarative partitioning keeps the table on
PostgreSQL and inside your existing migration tooling.

When partitioning by hash, mind two caveats:
- **The modulus is fixed at creation.** Changing the partition count later requires a new partitioned set plus a row migration — there is no online re-hash. Choose it deliberately up front.
- **Single-key reads stay prunable (the hot path); cross-key/global queries fan out** to every partition. That is the right trade-off when your access pattern is per-key — don't expect global-scan speedups.

Adding the criteria above is not a licence to act on them: do not add
partitioning migrations speculatively, before a trigger condition is met.
