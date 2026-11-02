---
name: create-migration
description: Create a numbered database migration pair (up + down SQL) following project conventions, then invoke migration-reviewer to verify symmetry and zero-downtime safety.
argument-hint: [migration_name]
disable-model-invocation: true
allowed-tools:
  - Write
  - Read
  - Bash(ls migrations/*)
  - Glob
---

Create a new database migration named **$0**.

See `.claude/rules/migrations.md` for project conventions. This skill is
the authoring counterpart to the `migration-reviewer` subagent — once the
files are written, invoke that subagent to verify the pair before the user
commits.

> Migrations are assumed to live under `migrations/` — the same directory
> named by `rc_get '.migrations.dir'` (default `migrations/`) that
> `migration-reviewer` reads. Adjust every `migrations/` path below to your
> layout if your repo keeps them elsewhere (e.g. `services/api/migrations/`).

## Steps

### 1. Determine the Next Version (DO NOT GUESS)

List existing migrations and compute the next sequential number:

```bash
ls migrations/*.up.sql | sort | tail -1
```

Parse the leading 6-digit number, increment by 1, and zero-pad to 6 digits.
Never assume a number based on memory or CLAUDE.md counts — those can be
stale. The filesystem is authoritative.

Verify no sibling file already exists at the chosen number. If one does,
STOP and report the collision rather than overwriting.

### 2. Validate the Migration Name

- `snake_case` only, no capitals, no dashes, no spaces
- Verb-first where it makes sense: `add_foo`, `drop_bar`, `rename_baz`,
  `backfill_qux`, `index_quux`
- Keep it under 50 characters so the final filename fits common terminal widths
- Reject names like `fix_stuff`, `update`, `wip` — they're uninformative

### 3. Draft the UP Migration

**File:** `migrations/NNNNNN_$0.up.sql`

Apply these rules (from `.claude/rules/migrations.md`):

**Data types**
- Timestamps: `TIMESTAMPTZ`, never bare `TIMESTAMP`
- Primary keys: `UUID` (preferred for user-facing entities) or `BIGSERIAL`
  (acceptable for internal high-volume tables). Never `SERIAL` (32-bit overflow).
- Money: `NUMERIC(p,s)`, never `FLOAT` / `DOUBLE PRECISION`
- Encrypted material: `BYTEA`. Hashed material: `TEXT`
- Audit columns on new tables:
  `created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()`
  `updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()`

**Zero-downtime safety**
- `CREATE INDEX CONCURRENTLY` on any table that may contain data (almost
  always — use non-concurrent only for brand-new empty tables in the same
  migration)
- `NOT NULL` on existing tables **must** provide a `DEFAULT`, or the target
  table must be guaranteed empty
- Foreign keys added to populated tables: use `ADD CONSTRAINT ... NOT VALID`
  now, then `VALIDATE CONSTRAINT` in a later migration
- Column type changes: prefer the two-step pattern (add new column,
  backfill, swap, drop old) over `ALTER COLUMN TYPE` on big tables

**Referential integrity**
- Every foreign key specifies `ON DELETE` behavior explicitly
  (`CASCADE`, `SET NULL`, or `RESTRICT` — never implicit `NO ACTION`)
- Unique constraints match what business logic actually requires
- Check constraints express real invariants

**Comments**
- Every table/column gets a `COMMENT ON TABLE` or `COMMENT ON COLUMN`
  explaining the business purpose. Future-you will thank present-you.

**Prohibited**
- No plaintext secrets, tokens, or credentials in SQL literals
- No `fmt.Sprintf`-style string construction — parameterization happens
  in Go code, but migrations themselves should be fully literal
- No `DROP TABLE` / `DROP COLUMN` without a confirmed data-migration plan

### 4. Draft the DOWN Migration

**File:** `migrations/NNNNNN_$0.down.sql`

**Walk through the up migration statement-by-statement and reverse each one:**

| Up statement | Down statement |
|---|---|
| `CREATE TABLE foo` | `DROP TABLE IF EXISTS foo` |
| `ALTER TABLE foo ADD COLUMN bar` | `ALTER TABLE foo DROP COLUMN IF EXISTS bar` |
| `CREATE INDEX CONCURRENTLY idx_foo` | `DROP INDEX CONCURRENTLY IF EXISTS idx_foo` |
| `ALTER TABLE foo ADD CONSTRAINT fk_bar` | `ALTER TABLE foo DROP CONSTRAINT IF EXISTS fk_bar` |

Rules:
- Use `IF EXISTS` on every drop to make the down migration idempotent
- Use `CASCADE` only when genuinely necessary and safe; default is omit
- If the up migration did a destructive data backfill (e.g., merged two
  columns), the down migration **cannot** perfectly reverse it. Add a
  `-- WARNING: rollback is destructive because ...` comment so reviewers
  see it explicitly
- Down migrations should handle partial-success scenarios — if the up
  migration created tables A, B, C and the down hits an error dropping B,
  C should still be attempted (use separate statements, not a transaction
  that aborts on first error)

### 5. Self-Verify the Pair

Before considering yourself done:
- [ ] Both files exist at the correct sequential number
- [ ] Filenames use `snake_case` and match the argument
- [ ] Every up statement has a reversing statement in down
- [ ] No bare `TIMESTAMP` (must be `TIMESTAMPTZ`)
- [ ] No `SERIAL` primary keys
- [ ] Indexes on populated tables use `CONCURRENTLY`
- [ ] `NOT NULL` additions have `DEFAULT` or target empty tables
- [ ] Foreign keys specify `ON DELETE` behavior
- [ ] No plaintext secrets
- [ ] Business-purpose comments on new tables/columns

### 6. Invoke migration-reviewer (MANDATORY)

After writing both files, invoke the `migration-reviewer` subagent in a
**separate Agent call** to review the pair independently:

> Launch Agent with subagent_type="migration-reviewer" and prompt:
> "Review the migration pair NNNNNN_$0.{up,down}.sql just created in
> migrations/. Focus on up/down symmetry,
> zero-downtime patterns, and security-sensitive table changes per
> .claude/rules/migrations.md. This is a fresh authoring; no prior
> review cycle."

Report the reviewer's findings to the user. If any CRITICAL or HIGH issues
surface, do not claim the migration is ready — fix and re-review.

### 7. Report Back

Present the user with:
- The two file paths
- A one-line summary of what the migration does (for the commit message)
- The migration-reviewer verdict
- Whether additional follow-up is needed (e.g., a separate
  `VALIDATE CONSTRAINT` migration later)
