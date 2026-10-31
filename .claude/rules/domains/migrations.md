---
description: Database migration conventions and safety rules
paths:
  - "services/control-plane/migrations/**/*.sql"
  - "services/control-plane/internal/authority/store/migrations/**/*.sql"
---

> **Imported from Concord-Voice-Alpha (@771eb3c) at the repo split.** References to `client/desktop/**`, `docs/runbooks/deploys.md`, `docs/reference/as-built-ledger.md`, `docs/superpowers/**`, managed (hosted) deploy workflows and Alpha's org rulesets point at Alpha, Desktop or the private ops repos. Use them for context; they are not files or CI in this repo.

# Migration Rules

## Naming & Structure
- Format: `NNNNNN_descriptive_name.up.sql` / `NNNNNN_descriptive_name.down.sql`
- Sequential numbering, zero-padded to 6 digits (see CLAUDE.md "Key Counts" section for current latest migration; do not duplicate the count here)
- Every up migration MUST have a corresponding down migration
- PostgreSQL 16 syntax

## Schema Safety
- No `DROP TABLE`/`DROP COLUMN` without confirmed data migration
- `ALTER TABLE` with `NOT NULL` must provide `DEFAULT` or target empty tables
- Use `CREATE INDEX CONCURRENTLY` for large tables
- Always `TIMESTAMPTZ`, never `TIMESTAMP`
- Foreign keys: appropriate `ON DELETE` behavior

## Transaction shape

golang-migrate itself opens no transaction. `migrationrunner.Run`, which the
control plane's `database.RunMigrations` calls (#3587 PR-0), builds the driver
with the default `postgres.Config{}`, which leaves multi-statement mode off,
so each migration file is sent to PostgreSQL as a single simple-query `Exec`
call carrying the whole file. PostgreSQL runs a multi-statement simple query
as one implicit transaction — that is where the "one file, one transaction"
behavior actually comes from, not from an explicit `BEGIN`/`COMMIT` the driver
adds. The practical consequence: a file containing more than one statement can
never use `CREATE INDEX CONCURRENTLY`, because `CONCURRENTLY` refuses to run
inside a transaction block. A file whose **only** statement is a
`CREATE INDEX CONCURRENTLY` is legal, because that single statement has
nothing to be implicitly wrapped with (000139).

## Activity History (migration 000087)

Migration 000087 adds these eight columns to `user_presence_settings`:
`presence_settings_version`, `presence_settings_operation_id`,
`activity_history_enabled`, `activity_history_retention_days`,
`activity_history_consent_version`, `activity_history_consent_copy_hash`,
`activity_history_consented_at`, and
`activity_history_reconsent_required`. It also creates the category-neutral
`presence_history` ledger and `presence_settings_pending_operations` quarantine.

Code that mutates this state must preserve the canonical lock order:
`users -> settings -> pending -> recipient exceptions -> history`. Do not add a
migration trigger or maintenance query that acquires these rows in a conflicting
order.

**Domain parents come AFTER `users`, never before (#2447).** `servers`,
`dm_conversations`, and any other domain parent are acquired *after* the `users`
row, extending the chain to
`users -> domain parent -> settings -> pending -> recipient exceptions -> history`
(extended again by migration 000111 — see that section for the current chain).
This is easy to get backwards — a domain handler naturally reaches for its own
parent row first — and getting it backwards builds a deadlock against every
presencehistory writer. Two existing call sites establish the order: `credepoch.GuardTx`
takes `SELECT ... FROM users ... FOR SHARE` and says in its own doc comment that
locking `users` first "respects the canonical 000087 lock order"
(`internal/credepoch/credepoch.go`), and `internal/dm/handlers.go` locks `users`
before `dm_conversations` and states why. The FK-induced `FOR KEY SHARE` that an
`INSERT INTO server_members` takes on `servers` is acquired at the mutation
write, i.e. at the END of the chain, so it does not conflict.

The operational downgrade guard is intentionally narrower than the normal
migration runner: it may perform only an exact clean-87 to clean-86 transition,
under the migration advisory lock, after enabled/history/pending zero assertions,
and must verify clean version 86. Do not generalize it into arbitrary down
migration support. Migration 000087 down is destructive because it drops the
history and pending tables plus all eight columns.

## Voice lifecycle watermarks (migrations 000093–000097)

The active Server Voice and Private Call participant watermark rollout is
intentionally split across five migration transactions. Migration 000093 adds
nullable columns and write defaults without checks, so old replicas can still
update pre-backfill rows. Migration 000094 backfills the original `joined_at`
values. Migration 000095 adds `NOT VALID` non-null guards only after the
backfill, under a short `ACCESS EXCLUSIVE` lock. Migration 000096 validates
those guards under `SHARE UPDATE EXCLUSIVE` in a separate transaction.
Migration 000097 first validates the guards for safe replay after its own down
migration, then uses them to make `SET NOT NULL` metadata-only and drops each
proof constraint in a separate `ALTER TABLE` command. Both validations precede
the first `ACCESS EXCLUSIVE` operation. Do not collapse these phases: every
migration file is one transaction,
so an `ACCESS EXCLUSIVE` lock would otherwise remain held through backfill or
validation scans. All phased up/down operations preserve the canonical
`voice_participants -> dm_voice_participants` lock-acquisition order. Migration
000093 column teardown and migration 000094 backfill rollback fail while either
ephemeral participant table is nonempty. Migration 000097 down may run with
active participants because it recreates `NOT VALID` non-null guards before
restoring nullable columns; it intentionally does not validate while holding
`ACCESS EXCLUSIVE`. Final column teardown may drop the DM column first after
both locks are already held.

## Rich Presence settings cleanup (migration 000098)

Migration 000098 creates `activity_settings_pending_cleanups`, a
privacy-critical marker keyed by settings owner. A policy-affecting settings
write must commit its versioned original before/after policy evidence and the
settings row in the same transaction. Cleanup failure must retain that exact
evidence so a same-value retry, another control-plane replica, or a later write
first resumes the committed cleanup instead of deriving a no-op from current
settings. Once external suppression succeeds, replace the bracket with a
versioned suppression receipt before finalizing the marker in a separate
transaction. A retry that finds that receipt must not replay suppression.
The inspection/external-suppression transaction must finish before receipt
work. Suppression, each of at most two exact receipt attempts, and finalization
must use separate detached bounded contexts and separate advisory-locked
transactions. This is replay-safe recovery, not crash-proof exactly-once
external suppression.
Before suppression resolution begins, reserve two full activity-cleanup
budgets inside the outer transaction deadline: one parent-bound child for
ordinary resolution/disconnect/delete work and one outer-budget remainder for
resolver-error global disconnect. If the reservation is unavailable, attempt
the global disconnect immediately, return an error, and retain evidence without
writing a receipt.

Policy writers serialize through `BeginAudienceOperation`'s canonical
user/settings row locks and must check the marker in the same transaction
before changing policy. Every later settings write, including Custom
Status-only writes, must resume older activity cleanup inside one sender gate
before Custom Status readiness. Each cleanup phase separately holds the
deterministic per-user PostgreSQL advisory transaction lock around marker
`FOR UPDATE`; no transaction spans phases. Thus a writer that raced the initial
resume still observes the committed marker after its row-lock wait and routes
back through advisory-serialized resume. Ambiguous receipt commits must be
verified or persisted in one more freshly locked transaction, and a successful
cleanup deletes only the matching `(user_id, operation_id)` receipt.
Do not store payload, audience, participant, or identity expansion in this table; the bounded recipient
resolver rebuilds affected recipients from the saved policy bracket and current
authoritative state. Missing Redis state is empty when the saved prior policy
proves ineligibility (Server Voice requires prior master On and tier not Off;
Private Call requires prior master On regardless of tier), or when the database
holds no evidence of a live or uncleared badge for that category — no
`voice_participants` / `dm_voice_participants` row, no
`presence_active_pending_plans` row, and for Server Voice no
`server_voice_terminal_outbox` row: nothing was published, so there is no
audience to clear. A pending plan or outbox row always fails closed, because a
clear is still owed. A sender with only a participant row is treated like a
stored badge: the prior audience is named from that row (the builder and
`visibleServerSettingsRecipients` for Server Voice, the stored path's own query
for Private Call) and exactly those viewers are cleared and reconnected; an
empty audience sends nothing. A stored invisible status is deliberately NOT a
reason to skip this: the hide handler clears nothing when the generation is
already gone, so the status cannot prove what viewers hold. An ambiguous scope
(rows in two rooms), a failed lookup, and all other resolver uncertainty still
fail closed and retain evidence. The participation clause narrows #2408, whose unconditional
prior-eligible fail-closed made every idle sender's second sharing change
disconnect every local client and leave a marker no retry could clear.

Account erasure must enter the same sender gate, finish any marker, exact-delete
both supported activity keys, and disconnect local Rich Presence clients before
deleting the user. The marker FK remains `ON DELETE RESTRICT` as a cross-replica
race backstop; unresolved privacy-repair state must never cascade away.

The 000098 down migration is intentionally guarded and destructive. It must
acquire `ACCESS EXCLUSIVE` on the marker table before testing emptiness and
dropped-table handling, so a concurrent insert cannot commit between the empty
check and `DROP TABLE`. Rollback must fail while any cleanup evidence remains.

## Active-category pending plans (migration 000111)

Migration 000111 creates `presence_active_pending_plans`, the durable
reconciliation rail for the ACTIVE Rich Presence categories (Server Voice,
Private Call). It is a **sibling** of 000087's
`presence_settings_pending_operations`, not a widening of it: that table is
`PRIMARY KEY (user_id)`, and a row in it suppresses the sender's whole Custom
Status reconnect snapshot for the row's lifetime, so a third operation kind
placed there would be both structurally serialized per user and
indistinguishable from a topology operation to 000087's reconciler.

**`ON DELETE RESTRICT`, and the contrast that decides it.** 000087 cascades and
000101 (`presence_offline_fences`) cascades; this table restricts, following
000098. The question is what a lost row fails toward. A 000101 fence is a
*denial* — losing it fails safe. A plan row is *evidence that a retraction is
owed*, and the stale generation it names lives in Redis, outside the FK graph:
cascading deletes the evidence while the stale visibility survives, which fails
OPEN. RESTRICT is therefore not belt-and-braces — it is what compels the erasure
path to carry an explicit drain step (a delete returning `category` inside the
erasure transaction, ahead of the `users` row removal, with one clear frame per
returned category emitted post-commit). Do not soften it to CASCADE to make an
erasure test pass; the RESTRICT is the test.

**`PRIMARY KEY (user_id, category)` and the collision ratchet.** A user can be in
Server Voice and a Private Call at once, as two independent generations, so a
`(user_id)` key would let one obligation absorb the other. Two producers can
still collide on one pair, so the insert ratchets rather than failing:
`ON CONFLICT (user_id, category) DO UPDATE` takes the newer `operation_id`,
forces `resolution = 'conservative'`, NULLs `scope_lifecycle_id`, keeps
`GREATEST` of the event watermarks and `LEAST` of the due times, and resets
`attempts` / `failure_class`. That is safe only because the conservative terminal
is a clear frame keyed on exactly `(sender, category)` — this table's own primary
key — and is generation-agnostic, so one frame retracts either colliding
generation. Resolution is monotone `exact -> conservative`, never the reverse.

**A delete that sends no clear must capture a plan.** The settings writer reads
the absence of participant rows, terminal outbox rows and plans as "no badge was
published" (#3444). So a path that deletes a `voice_participants` or
`dm_voice_participants` row without sending its own Rich Presence clear must
insert a conservative plan in the same transaction, or that absence becomes a
lie while a viewer still holds the badge. The Server Voice lease reaper and the
DM ring path's stale-row expiry do this (#3446), and record the plan even when
the user still has another participant row: a viewer who could see only the
deleted room must still lose the badge, and the cost -- the live badge cleared
until the next heartbeat republishes it -- is the privacy-safe direction.
`EventAt` is the delete time, so a generation whose lifecycle event is stamped
after the delete resolves as a successor; a join stamped before the delete but
committed after it is cleared and republished the same way. A deleter that runs
outside the reaper must lock the `users` row `FOR KEY SHARE` before the
participant row, because the plan's foreign key takes that lock anyway and
account erasure takes `users FOR UPDATE` first.

**An absent generation always owes a clear.** The resolver once acknowledged a
plan older than `ActivityStateTTL` with no delivery (B1), on the premise that
every viewer's copy had expired by then. The desktop client never expires a
badge on its own -- only a `rich_presence_clear` or a reconnect snapshot removes
one -- so a plan retried from quarantine or after downtime left the badge in
place. #3446 removed that arm: absent state resolves to a clear at any age.

**`_exact_evidence_check` is a falsifiability rule, not a null guard.**
`CHECK (resolution = 'conservative' OR scope_lifecycle_id IS NOT NULL)`: an exact
plan must name the generation it is aimed at, because after a destructive
mutation a recovered process cannot otherwise tell a stale call from a successor,
and a clear aimed at a successor kills a live call's presence. The database
enforces the resolver's fail-closed rule rather than trusting a call site to. The
`failure_class` CHECK exists for the adjacent reason: a free-text column there
would eventually hold a wrapped database error, and the log line printing it
would become a data leak.

**The FK holds `SHARE ROW EXCLUSIVE` on `users` while the table is created.** The
inline `REFERENCES users(id)` conflicts with `ROW EXCLUSIVE`, so it blocks every
`users` write while held. There is no validation scan — the referencing table is
empty — so the hold is sub-millisecond, but it waits behind any long-running
`users` transaction and queues every later `users` write behind itself. Deploy
the migration alone, ahead of the binary: see
`docs/runbooks/deploys.md`.

**The down migration is guarded and refuses to discard evidence.** It mirrors
000098: `to_regclass` guard, `LOCK TABLE ... IN ACCESS EXCLUSIVE MODE` **before**
the emptiness test (evidence can commit between a bare existence check and the
drop), `RAISE EXCEPTION` on any surviving row, then the drop. An operator rolling
back mid-outage must let the reconciler drain first.

**Lock order.** Plan capture sits after the domain parent and before the
destructive write, so the canonical chain extends to
`users -> domain parent -> settings -> pending -> ACTIVE PLANS -> recipient exceptions -> history`.
`internal/dm`'s `DeleteGroup` is the reference producer: sender gates before
`BeginTx`, then the `users` rows in database-sorted order, then
`dm_conversations`, then re-validation of the candidate set under that lock, then
the capture, then the deletes. It never locks an additional `users` row
mid-transaction — that reintroduces exactly the unordered acquisition this chain
exists to prevent.

## When to Partition Large Tables

Partition the `messages` table only when warning signs actually appear. Both premature partitioning and reaching for the wrong tool under pressure are anti-patterns — this section codifies the trigger criteria so the decision is data-driven, not speculative.

**Partition only when one of these holds:**
- Any single query p99 > 100ms on the `messages` table
- Any single channel exceeds 100k messages
- Total `messages` table > 10M rows AND index bloat is observed — a high `pg_stat_user_tables.n_dead_tup` ratio, or `pgstattuple` reporting low tuple density (`ANALYZE` alone only refreshes planner statistics; it does not surface bloat)

**Recommended approach:** PostgreSQL declarative partitioning by `hash(channel_id)`. Do **NOT** migrate to Cassandra/ScyllaDB at our scale — the Cassandra hot-partition problem a major comms platform reported is driven by per-channel read concurrency at very large scale (roughly 5–6 orders of magnitude beyond ours), not by aggregate row count. Declarative partitioning keeps the table on PostgreSQL 16 and within the existing migration tooling.

When partitioning by hash, mind two caveats:
- **The modulus is fixed at creation.** Changing the partition count later requires a new partitioned set plus a row migration — there is no online re-hash. Choose it deliberately up front.
- **Single-channel reads stay prunable (the hot path); cross-channel/global queries fan out** to every partition. That is the right trade-off for Concord's per-channel access pattern — don't expect global-scan speedups.

**Out of scope until triggered:** adding the trigger criteria here (criteria only, no code) is the v1.0.0-scoped work (§4.1). Actually partitioning the table is deferred until a trigger condition is met — earliest **v1.2.0** if the criteria cross during GA traffic (§4.3), otherwise a **v1.3.0** sketch candidate (§4.4). Do not add partitioning migrations speculatively.

Refs: release-tracking spec §4.1/§4.3/§4.4; competitive brief §3.1 — message storage & hot-partition mitigation at a major comms platform.

## Data Integrity
- Primary keys: prefer UUID or BIGSERIAL
- Unique constraints where business logic requires
- Default values must be sensible
- Audit columns: `created_at TIMESTAMPTZ DEFAULT NOW()`, `updated_at TIMESTAMPTZ DEFAULT NOW()`

## Server Voice observed lease (migrations 000132–000133)

Migration 000132 adds the PostgreSQL-assigned `lifecycle_observed_at` lease
timestamp and its ordered sweep index. Migration 000133 preserves that lease
on an exact `lifecycle_event_at` replay while inserts and distinct lifecycle
updates renew it. The migration runner wraps each file in a transaction, so
the plain index is intentional; do not use `CONCURRENTLY`.

**The trigger is not the only renewer (#3187).** The conditional upsert keeps a
row only when `participant.lifecycle_event_at <= EXCLUDED.lifecycle_event_at`.
A row stamped AHEAD of the database clock loses that comparison on every later
heartbeat, so the upsert is a no-op, the 000132/000133 trigger never fires, and
the lease expires under a participant still in the SFU.
`renewObservedLeaseForFutureStampedRows` in `internal/voice/nats.go` re-stamps
that class directly, bypassing the trigger. Three properties are load-bearing
and each is easy to undo:

- **The predicate is forward-stamp only, and that boundary is the Redis
  lifecycle watermark's.** A future stamp is the one refused-upsert case the
  watermark ACCEPTS — it is the newest stamp it has seen, by definition — so the
  gap is genuinely unadjudicated and belongs to this statement. Every other
  refused upsert is a heartbeat the watermark REJECTS as stale. Widening to
  "renew whenever the upsert was refused" (`lifecycle_event_at > eventAt`) is
  strictly wrong: once a newer heartbeat advances the row to t2, a publisher
  replaying an older payload at t1 satisfies t2 > t1 on every delivery, so a
  stale row whose terminal event was lost is renewed forever and #2907's
  room-empty reconciliation is defeated.
- **It must not name `lifecycle_event_at` in the `SET` list.** The trigger is
  `BEFORE INSERT OR UPDATE **OF lifecycle_event_at**`, so it fires on the
  column being *mentioned*, changed or not, and 000133's `ELSE` branch then
  writes `OLD.lifecycle_observed_at` straight back. That failure is
  silently-successful: `err == nil` and `RowsAffected == 1`. Only a test catches it.
- **It is bounded ABOVE**, by `maxVoiceLifecycleForwardSkew`. Every other
  eviction or move path is fenced by `lifecycle_event_at <= $3`, and
  `presence.IsValidActivitySourceTime` bounds a producer stamp only at roughly
  year 2255 — so the observed lease is the ONLY eviction path that acts on wall
  clock rather than event order. Renewing without a ceiling makes a far-future
  row permanently un-evictable instead of 90 seconds stale, and it is the
  ceiling that makes any poisoned state self-expiring.

Three residuals are accepted and documented at the function: a lost race with
the reconciler's DELETE (this UPDATE takes no lifecycle lock; the reverse
ordering is safe because that DELETE re-asserts the lease predicate), a replay
holding a still-future row for roughly the ceiling plus one lease window, and
`resolveRoom` preceding the call. Each is bounded by the ceiling and
self-expiring. Serialising the renewal under the lifecycle lock would close the
first but serialise every heartbeat against the reconciler — a concurrency
change, not a bug fix.

### The ingress clamp and migration 000134 (#3205)

`parseVoiceEventTime` now bounds every arriving producer stamp by the consuming
replica's NATS receipt time. **For every value written at ingress from #3205
onward, `lifecycle_event_at` on BOTH tables means "producer clock,
upper-bounded by the consuming replica's receipt clock"** — not "producer
clock". Nothing in the schema expresses that; this paragraph is the record. Do
not spend a migration on a `COMMENT ON COLUMN` for it.

**Two classes of row do not carry that meaning, and reading them as if they did
will mislead a later investigation.** Rows written by a pre-#3205 replica hold
an unbounded producer stamp — that is the whole reason 000134 exists, and a
straggler can still create one for the length of a rolling deploy. And the DM
rows 000134 itself repaired hold `CURRENT_TIMESTAMP` at *migration* time, which
is neither clock: it is a recovery value chosen because the original producer
stamp was unrecoverable and any value at or before now unwedges the row. Treat a
`dm_voice_participants.lifecycle_event_at` equal to a migration or post-drain
runbook timestamp as repaired, not as observed.

**The renewal above is demoted, not retired.** With the clamp in place no new
binary writes a stamp ahead of its own receipt clock, so
`renewObservedLeaseForFutureStampedRows` is near-dead in steady state. It remains
correct and necessary for exactly two classes: a straggler old replica during a
rolling deploy, and control-plane/database clock drift. **Do not delete it** on
the evidence that it renews nothing in a healthy fleet — that is the state it is
supposed to be in.

**The statement clock.** The renewal reads the clock three times in one statement
— once in `SET`, twice to bound the predicate — and PostgreSQL 16 documents
`clock_timestamp()` as returning the time at the moment of the call, so its value
can change *within* a single statement. It uses `statement_timestamp()` for that
reason. The divergence it removed was the statement's own execution time, which
is microseconds against a 180 s window, so this is a coherence fix rather than a
bug fix — but do not describe the old form as self-consistent, and do not revert
it for symmetry with 000133's trigger, which reads the clock once and is
unaffected.

**Migration 000134 is data repair, and the rail asymmetry is why it exists.** The
clamp bounds arriving stamps; it cannot reach rows a wrong-clocked producer
already wrote. A far-future `voice_participants` row self-heals — within the skew
ceiling the observed lease holds it, and beyond the ceiling the renewal declines,
the lease ages out, and #2907's reconciler reaps. `dm_voice_participants` has
neither the column nor a sweeper (`staleServerVoiceDiscoverySQL` reads only
`voice_participants`), so a far-future row there is refused by the
`lifecycle_event_at <= $3` delete fence, refused by the `<=` conditional upsert,
and reachable by nothing else — permanent. 000134 clamps those rows once, on
`CURRENT_TIMESTAMP` so the whole repair shares one instant, and is idempotent by
construction because the predicate is self-negating. Its down migration is a
deliberate no-op: the pre-clamp values are unrecoverable and restoring one would
re-wedge the room the repair unwedged.

**000134 does not close its own window.** Migrations apply at control-plane boot,
so the first new replica runs the repair while straggler old replicas may still
publish unclamped stamps. The same statement appears in
`docs/runbooks/deploys.md` as a post-drain
verification step; that is what closes it. A full-drain deploy gate was considered
and rejected as disproportionate for a P3 robustness fix — the honest position is
a bounded re-poisoning window plus an operator step, not a pretence that the
migration alone is sufficient.

## Cross-server role assignment and permission-bitfield sign (migration 000144)

Migration 000144 (#2869) closes the `member_roles` half of the same-server family
that 000082 opened for `channel_groups`. It adds `roles UNIQUE (id, server_id)`
and replaces 000035's existence-only `member_roles_role_id_fkey` with
`member_roles_role_server_fkey FOREIGN KEY (role_id, server_id) REFERENCES
roles (id, server_id) ON DELETE CASCADE`. `ON DELETE CASCADE` is carried over
unchanged rather than borrowed from 000082's `ON DELETE SET NULL (group_id)`:
that column-list form exists because `channels.group_id` is nullable, and
`member_roles.role_id` is `NOT NULL`, so deleting the role must still delete the
assignment row.

**Each pre-flight guard is decided by what the offending row IS, not by habit.**

- **Cross-server `member_roles` rows fail closed.** 000082's violating state was
  API-REACHABLE — three channel handlers could each produce a cross-server group
  binding — so rows were expected and cleanup was the only way to attach the FK.
  000144's is API-UNREACHABLE: all four production writers are server-scoped, and
  `internal/ownership` and the admin console hold no `member_roles` writer at all.
  A surviving row is evidence of an unaudited writer or direct database access,
  so the migration `RAISE`s with the count and the enumerating query (the 000110
  pattern) rather than deleting a privilege grant and destroying the trail.
- **Negative GRANTS fail closed** — `roles.permissions` and both override tables'
  `allow`. These ARE API-writable (see [`backend.md`](backend.md) § "The
  permission bitfields are pinned non-negative"), so the refusal is not an
  evidence argument: -1 grants `PermAdministrator` along with every other bit,
  and clearing bit 63 would keep that grant while making it look clean.
- **Negative DENIES are normalised** by `deny & 9223372036854775807`. A deny is
  applied as `perms &^= deny`, and every value it is subtracted from is
  non-negative, so bit 63 removes nothing and clearing it changes no outcome.

**Do not "fix" a failing deploy by adding a DELETE or an UPDATE to either
fail-closed guard**; the refusal is the feature, and the operator decides with
the rows in hand. A refused run rolls back whole but leaves version 144 recorded
as dirty, and the control-plane refuses to boot on a dirty version (the shared
`migrationrunner` refusal). Once the rows are dealt with, run
`migrate -command=force -force-version=143` (143, not 144: nothing of 000144
committed, so forcing 144 would skip it) and migrate again.

**The guards run under `LOCK TABLE … IN ACCESS EXCLUSIVE MODE` on all four tables, taken BEFORE the first guard.** Without it a writer could commit a violating row between a guard's clean read and the `ADD CONSTRAINT` that follows, and the migration would fail on Postgres's opaque constraint error instead of the guard's diagnosis — the 000098/000111 pattern. The lock is held to COMMIT, which is why splitting validation out (below) also means narrowing that `LOCK TABLE`.

**Five `CHECK (… >= 0)` constraints** cover `roles.permissions` and `allow` /
`deny` on both `channel_permission_overrides` and `category_permission_overrides`.
The category table is the enforcement source every synced channel is copied from,
so constraining only the channel table would turn a negative category value into
a failed sync rather than a refused write.

**The down migration restores the schema exactly** — verified by diffing
`pg_constraint` across up → down → up: the difference is precisely the seven added
constraints plus the FK swap, and the replay is byte-identical. It does NOT restore
the sign of a normalised deny: that bit carried no permission and its original
value is recorded nowhere. The UNIQUE must be dropped LAST — it is the FK's target.

**Not split into `NOT VALID` + `VALIDATE`.** The guards prove the tables hold no
violating row, so validation is a clean scan on a pre-GA-sized table. Split it if
that scan ever holds `ACCESS EXCLUSIVE` long enough to matter; the 000121/000122
pair is the shape. No new index either: the cascade filters on both FK columns and
`idx_member_roles_role(role_id)` already leads with `role_id`.

**Testing.** `TestMigration000144_UpGuardsAndDenyNormalisation` runs the down
migration to reconstruct the pre-000144 schema, seeds each offending state, and
asserts the up migration refuses (or normalises) it — so, unlike 000082, the
guards ARE runtime-exercised, and each was mutation-proven to fail its own case.
Every constraint refusal elsewhere is paired with a CONTROL that must succeed,
because a constraint rejecting everything would satisfy the refusals alone.

## DM clear-reap purge reason (migrations 000155–000156, #3462)

Migrations 000155 and 000156 are literal copies of the 000130/000131 shape
that admitted the `expiry` purge reason, this time widening
`message_purges_reason_check` to also admit `clear` — the reason the DM
clear-reap purge-engine terminal writes when it deletes a batch of messages
every current participant has already cleared. 000155 adds the widened
`NOT VALID` check and swaps it in for the old one; 000156 validates it.

**000155's down deliberately deviates from 000130's down in one place.** It
still refuses to run — `RAISE EXCEPTION` under `LOCK TABLE ... IN ACCESS
EXCLUSIVE MODE` — while any `reason = 'clear'` row survives, because that row
is guarded evidence of an already-committed, irreversible deletion, and a down
migration must never make evidence like that silently disappear (the 000130
pattern). But once the guard has proven no such row exists, 000155's down also
runs `VALIDATE CONSTRAINT` on the four-value check it restores, rather than
leaving it `NOT VALID` the way 000130's down does. At the schema version this
down returns to, that constraint **was already validated** (by 000131), so
skipping the extra validate would leave an up → down → up round trip with a
`pg_constraint` diff in `convalidated` — a state golang-migrate's own version
number cannot see and this repository's up/down parity tests are written to
catch. See the #3462 design's D7 for the full reasoning and
`docs/runbooks/deploys.md` § Rollback for
the operational consequence: once real `clear` evidence exists, rollback is
forward-fix only.

## Server MFA-enforcement toggle (migration 000157)

Migration 000157 (written as 000155; renumbered when #3462 took 000155–000156 first) adds `servers.enforce_mfa_dangerous_actions BOOLEAN NOT NULL
DEFAULT FALSE` (#3453). Metadata-only on PG16 (the 000039 precedent), no
index, no backfill — the flag is always read by primary key. See
[`backend.md`](backend.md) § "MFA-enforcement permission mask" for what the
column drives.

**The down is guarded, in the 000138 shape, not the 000144 shape.** It locks
`servers IN ACCESS EXCLUSIVE MODE`, then a `DO` block counts rows where the
flag is TRUE (inside an `information_schema` existence check, so it parses on
a re-run after the column is already gone) and `RAISE EXCEPTION`s naming that
count when it is nonzero, before `DROP COLUMN IF EXISTS` runs. Dropping the
column while any server enforces would silently turn enforcement off for that
server with no trace — the guard trades an operator interrupt for that
silent loss.

**Recovery after a refused down (L10): `force 157`, never `force 156`.**
golang-migrate stamps the down's *target* version (156) dirty before it runs
the file. The refused file rolls back in its own transaction, so the column
still exists and the schema is still at 157. Forcing 156 and running `migrate
down` again would execute **000156's own down**, which puts the
`message_purges_reason_check` it validated back to `NOT VALID` (#3462).
Confirm the column still exists, then `force 157`, then boot normally. If the down is genuinely
wanted, reset every enforcing row to FALSE first and run it again.

**A binary rollback needs no down and gets none of this guard's protection.**
No non-test code path does `SELECT *` on `servers`, so an old binary
tolerates the extra column fine — but it also does not know to mask
anything, so rolling back to a pre-#3453 binary **silently stops
enforcement** for every server that had turned it on, for as long as the
rollback lasts. See the deploy runbook's 000157 section for the operator-facing
version of both of these.

## TOTP last-used step (migration 000158)

Migration 000158 adds `user_mfa_totp.last_used_step BIGINT` — nullable, no
default, no backfill, no index. It is the last 30-second TOTP time step
(Unix time / 30) at which the user's code was accepted, and it makes a code
single-use (RFC 6238 §5.2): every TOTP acceptance writes the step through a
guarded `UPDATE … WHERE user_id = $1 AND (last_used_step IS NULL OR
last_used_step < $2)` and accepts only if that affected exactly one row. NULL
means "no step accepted yet", which is true of every pre-existing row, so no
backfill is needed. See [`backend.md`](backend.md) § "Policy P1" for which
writers advance it and why the advance rides the caller's store.

**`TOTPSetup`'s upsert resets it to NULL** together with the secret, so the
column never describes a secret other than the stored one. That is hygiene, not
a guard: every reader only reaches a row that is `enabled AND confirmed`, and
`TOTPVerifySetup` overwrites the value before a pending enrolment can become one.
The two guard readers are `acceptTOTPStep` and `RegenerateBackupCodes`. **A third
reader is presentation only (MFA picker PR 1):** `stepup.InlineMFAFactors` reads
the step to rank TOTP's recency for the advisory `default_method`. It never
writes the column and never decides acceptance. That is why the column is no
longer "only read by primary key inside the guarded UPDATE"; see
[`backend.md`](backend.md) § "Policy P1" for its step-precision band. `TOTPVerifySetup` writes it under
`enabled = FALSE` rather than the step guard — that predicate is its
compare-and-set against a concurrent double submit. Every other writer advances
it under the step guard, and none lowers it.

**The down is unguarded, deliberately, and drops the column outright.** Unlike
000157's flag, the value is meaningful only while a code is inside its ±1-step
window, so dropping it loses at most one replay of each code accepted in the
last ~90 s — never durable state. An old binary ignores the column, so the up is
safe to apply ahead of the code. **A binary rollback alone is not possible:** an
image built before 000158 refuses to boot against it, because golang-migrate's
`readUp` fails `versionExists` for a database version the image does not embed
(v4.20.1 `migrate.go:537`) and `RunMigrations` returns that error
(`internal/migrationrunner/runner.go:73`). Rolling back means running this
down first, which re-opens in-window replay for as long as the old image runs.

## Channel voice pending admissions (migrations 000160–000161)

Migration 000160 creates the bounded `voice_pending_admissions` handoff table
and expiry index. Migration 000161 adds the exact `admission_id` and `socket_id`
identity columns. Both foreign keys cascade because these rows are short-lived
handoff state, not audit evidence. The global `voice_authorization_revision_seq`
is owned by migration 000159; this pair must neither create nor drop it.

The 000160 index is intentionally non-concurrent because it is created on the
new empty table before any reservation writes. Migration 000161 only adds the
identity columns before writers use them; it does not create or drop the
000159-owned sequence. Apply both migrations before a binary writes
reservations; a binary rollback leaves the additional columns harmlessly
unused. Do not generalize this rationale to other migrations: the runner's
transaction behavior does not make `CREATE INDEX CONCURRENTLY` safe.

## Step-up tokens (migration 000162)

Migration 000162 creates `step_up_tokens` (#3455, PR #3509): single-use,
purpose-bound tokens for the password step-up mint
(`POST /api/v1/auth/step-up/password`, `factor = 'password'`) and for WebAuthn
inline verification (`factor = 'webauthn'`, which lived in Redis before). Only
`SHA-256(token)` is stored, as the `BYTEA` primary key; `credential_epoch` is
`users.credential_epoch` at mint (NULL = never rotated); `user_id` cascades on
account deletion. A consumer spends a token with ONE `DELETE … RETURNING`
inside its own transaction, so a rollback restores it — see
[`backend.md`](backend.md) § "Step-up tokens" for the match and its
invariants. The table is new and empty, so both indexes,
`(user_id, expires_at)` and `(expires_at)`, are plain `CREATE INDEX` in the
same file; do not generalize that to a populated table. (golang-migrate sends
each file as one implicit transaction, so `CONCURRENTLY` was not available
here anyway.) CHECKs pin what the writer guarantees: `token_hash` is 32 bytes,
`purpose` is non-empty, and `credential_epoch` is NULL or
`credepoch.NewEpoch`'s 32 lowercase hex (000151's format check), so a test or
tool that stores another epoch shape fails at the write. A token binds to the
user and the credential epoch, not to a session: only an epoch rotation
strands it.

**Deploy order: apply 000162 before the binary.** A binary ahead of the table
answers every own-rule password confirmation and every WebAuthn inline
confirmation with a 500; the table ahead of the binary is simply unused.

**The growth bound is enforced by the writer, not the schema.** Every mint, in
its own transaction under the user's `FOR NO KEY UPDATE` row lock, deletes that
user's expired rows and keeps at most **16 rows per user**, dropping the oldest
first — so after any mint a user holds at most 16 rows, all live. The hourly
`runCleanup` job (`stepup.SweepExpiredTokens`, `FOR UPDATE SKIP LOCKED`)
deletes expired rows for users who never mint again. The table therefore holds
at most 16 rows per user who minted since the last sweep, and every row expires
60 seconds after its mint. A new writer must keep both halves: mint through
`stepup.MintToken`, never a bare `INSERT`.

**The down drops the table unguarded.** Every row is at most 60 seconds from
expiry, so a rollback costs at most one re-prompt. A binary rollback without
the down leaves the table unused, and an old binary's Redis WebAuthn tokens and
the new binary's rows are invisible to each other, so a confirmation minted on
one side fails closed on the other and the user repeats it.

## Pin-aware message expiry (migration 000168)

Migration 000168 (#3459, epic #1351) adds a per-context "Include pinned
messages" switch and a parked stamp for kept pins: `channels` and
`dm_conversations` gain `expiration_include_pinned BOOLEAN NOT NULL DEFAULT TRUE`,
and `messages` and `dm_messages` gain `held_expires_at TIMESTAMPTZ NULL`. The
default TRUE means every timer that exists at ship keeps expiring pins; FALSE is
stored only by a writer that asks for it (a new timer with the field omitted
stores FALSE, an existing one keeps its value). A context **keeps pins ("K")**
when `expiration_window_seconds IS NOT NULL AND NOT expiration_include_pinned`.
Every reader reports the EFFECTIVE value E (`window IS NOT NULL AND include`),
never the raw column, because the raw column reads TRUE under a null window and a
client seeded from it would turn pinned expiry on by accident.

**C-SWEEP: keep is expressed only by the stamp's absence.** A kept pin carries
`expires_at NULL`, and its would-be stamp parks in `held_expires_at`, which no
deleting reader looks at. No keep predicate may ever enter the sweeper's
discovery (`channelGroups` / `dmGroups`), `selectExpiry` or `anyEligible`: the
boot preflight loops until discovery is empty, and a predicate that skips a row
the stamp still names would stall it forever. `held_expires_at` carries **no
index**, because nothing selects by it.

**I-HELD.** `held_expires_at IS NOT NULL` implies `pinned_at IS NOT NULL`,
`expires_at IS NULL`, and the parent is K. The CHECK enforces the first two; the
application state machine (`internal/expiration/pinned.go`) enforces the third by
reconciling a context's pinned rows in the same transaction as every policy
write, after the update and under the parent `FOR NO KEY UPDATE`. A pin moves a
stamp that has not passed into held, an unpin returns it
(`COALESCE(expires_at, held_expires_at)`), and a clear drops every held stamp
even under `leave_pending`, because a kept pin was never scheduled. Each
`pinned_at` writer embeds an exported SET fragment (`ChannelPinHoldAssignments`,
`ConversationPinHoldAssignments`, `UnpinReleaseAssignments`); a static census
test fails the build for a writer that omits one, and the CHECK makes a forgotten
unpin a loud 500 instead of a silent deferral.

**C-NORES: a stamp that has already passed is never moved or cleared.** Every
move predicate is `expires_at > C` (the accepted cutoff the policy update's
`clock_timestamp()` CTE returned, passed explicitly, never `now()`), or `>
statement_timestamp()` for a pin, never `>=`. So turning the switch off does not
rescue a due pin (the next sweep deletes it), and pinning a row whose stamp has
passed leaves it stamped (D-C). A pin reads `statement_timestamp()` once for
both `CASE` arms: two `clock_timestamp()` reads can straddle the stamp and trip
the CHECK (the 000134 precedent).

**Q-SHAPE.** Every pinned-only statement that names `held_expires_at` also carries
`pinned_at IS NOT NULL`, so it rides `idx_messages_pinned` /
`idx_dm_messages_pinned` and is bounded by the pin cap. That covers the reconcile
statements, the due collect, the clear-drop, and this migration's own evidence
copy and restore. The CHECK (held implies pinned) makes the predicate complete.
Never replace one with a scan of `messages` or `dm_messages`, least of all under
the down's ACCESS EXCLUSIVE lock. The retroactive `apply` backfill and its
eligibility probe embed ONE shared fragment (`applyEligible` /
`applyAssignments`) by compile-time concatenation; two copies that drift leave
`backfill_pending` set forever, and a static test asserts both SQL consts embed
the fragment.

**The CHECKs are `NOT VALID`, permanently.** `messages_held_expires_at_shape_check`
and `dm_messages_held_expires_at_shape_check` are added `NOT VALID` and **no later
migration validates them. Do not add one.** The column is new, so every existing
row has `held_expires_at NULL` and satisfies the CHECK by construction; `NOT
VALID` already enforces every new and updated row, which is all a CHECK on a new
column can be asked to do. A `VALIDATE CONSTRAINT` would be a sequential scan of
two of the hottest tables, at boot, inside the health budget, to prove a
tautology. (The design considered a follow-up validating migration and rejected it.)

**C-DDL: the migration is metadata-only.** Nullable columns with no default, a
constant `DEFAULT` on the two parents (stored as `attmissingval` on PostgreSQL 11+,
no rewrite), `NOT VALID` CHECKs, no index on the held column. A test compares
`relfilenode` before and after. The file takes ACCESS EXCLUSIVE on all four
tables, parents first, as its first statement; it runs at singleton boot with the
predecessor stopped.

**Rollback evidence and C-DOWN.** Dropping the columns alone would lose which
contexts keep pins and every held stamp, and the next up would relabel a
keep-pins context TRUE and leave every formerly kept pin at `expires_at NULL`
forever. So the down copies that state into two tables the up creates
(`IF NOT EXISTS`, never dropped, **no foreign key** — the 000164 pattern, and an FK
from the evidence to `messages` would put a cascade trigger on the hottest table):

- `expiration_keep_pins_evidence (context_kind, context_id, expiration_revision)`,
  one row per K context;
- `expiration_held_stamps_evidence (message_kind, message_id, held_expires_at)`,
  one row per held message, collected through the pinned index from the K parents.

The up restores from them, by primary key, and deletes the consumed rows. A
context is relabelled FALSE only if its `expiration_revision` is unchanged (a
moved revision means someone acted under the old binary's semantics, which had no
switch). A held stamp is restored only under the same rule: where the context's
revision moved, the old binary's action stands and the stamp is dropped (a clear
followed by a `new_only` set would otherwise resurrect a cancelled schedule, and
delete the message if the stamp is past). Where the revision is unchanged, the
stamp returns to `held_expires_at` when the message is still pinned, and goes back
into `expires_at` when it was unpinned while rolled back. That re-arms a deferral
whose condition (pinned AND kept) ended during the rollback, and a stamp already
past is then deleted by the boot preflight; this is intended, because the
retention promise governs once the deferral lapses. Pins stamped by the old binary
inside a K context are normalised into held (stamps not yet due only). **C-DOWN: a
down→up neither relabels, nor re-arms a kept pin, nor drops evidence.** The down
itself never re-arms a stamp: under the old binary a held pin sits at `expires_at
NULL` and is kept, the safe direction while rolled back. It is unguarded for the
reason 000164's is: the columns record an attribute of retention, not evidence of
a deletion, and the evidence tables preserve it.

**Rollback order is fixed.** Stop the control plane (a running new binary fails
every expiry batch against a dropped column, and on restart it re-applies the
up), run the 000168 down with `cmd/migrate` from the new tag's tree, then start
the old image. An image-only rollback is impossible: golang-migrate refuses a
database version the image has no file for. See
`deploys.md` § "Pin-aware expiry (migration
000168, #3459)".

**The expiry audit row changed with it.** See the `message_purges` entry under Key
Tables Reference: an expiry row is now written inside the batch transaction with
`include_pinned = E OR any pinned victim`.

## Account authority database (separate directory, PR-B1)

The account-key authority has its **own** PostgreSQL 16 database and its own migrations
directory, `services/control-plane/internal/authority/store/migrations/`. This file's
numbering, count and "next number" rules do not apply to it, and its rules do not apply to
`services/control-plane/migrations/`.

- **Its own numbering.** It starts at `000001_authority`. CLAUDE.md's Key Counts count the
  control-plane directory only; `./scripts/update-claude-md-counts.sh` does not read this one.
- **The schema exists before the first migration.** golang-migrate keeps `schema_migrations`
  in `current_schema()`, so schema `authority` (owner `authority_owner`) and the database
  `search_path = authority` are created by the operator's bootstrap, never by 000001 (B1-D2).
  The test form is `services/control-plane/scripts/authority-test-bootstrap.sql`.
- **The owner migrates; the app runs.** Migrations run as `authority_owner`; the service runs
  as `authority_app`. `meta` is updatable only column by column. A grant change is a schema
  change and ships as a new migration. `TestAppRoleGrants` pins the grants.
- **`account-authority migrate --realm <uuid>` applies them** from the binary's embedded
  copy (`store.MigrateEmbedded`, `iofs`), as `authority_owner` and under the serve lock, so it
  exits 1 with `reason=lock` while `serve` runs (D-171). It then inserts the `meta` singleton
  for that realm if none exists, and refuses a different realm with `reason=trust` (B2-D2).
  The new row keeps `replication_required = true`; a single node needs PR-B3's
  `replication-gate off`.
- **`committed_at` is the database's clock** on both event tables, forced by trigger (F1).
- **The down migration refuses while any authority row exists** (B1-D27: a recorded refusal
  leaves a head and a ledger row and no event). It exists for an empty test database. It
  locks all ten tables `IN ACCESS EXCLUSIVE MODE` before the check, as 000098 does, so a row
  committed between the check and its table's `DROP` is refused, not dropped.
- **Never-deleted rows survive the owner, TRUNCATE included.** Row triggers do not fire on
  `TRUNCATE`, so `meta`, `delegations`, `successions` and `revoked_keys` also refuse it with a
  `BEFORE TRUNCATE` statement trigger, `legacy_population` refuses it once sealed, and `meta`
  refuses `DELETE`. The test harness resets them as superuser under
  `SET LOCAL session_replication_role = replica`, which skips both kinds of trigger for that
  transaction only.
- **A dirty or skewed schema refuses to start** (D-209). `store.CheckSchema` returns
  `ErrSchemaDirty` or `ErrSchemaSkew`; there is no automatic repair.
- **Applied means immutable**, exactly as for the control plane:
  `.claude/hooks/protect-applied-migrations.sh` guards both directories.

Design of record §3.2.1–§3.2.2; plan `docs/superpowers/plans/2026-10-14-3587-pr-b1-commit-core.md`.

## Stripe billing (migrations 000178–000182)

Epic #2033 PR-1 (Refs #1305) claims 000178–000182; PR-5's seats migration
is 000188. Spec:
`docs/superpowers/specs/2026-10-15-2033-stripe-billing-design.md` §5.

| Migration | Change | Down |
|---|---|---|
| 000178 | `subscriptions.status` admits every Stripe status (adds `incomplete_expired`, `unpaid`, `paused`; the live set stays `active, trialing, past_due`). Adds `plan` (`supersonic`/`wing`), `billing_interval` (`month`/`year`), `founders_rate BOOLEAN NOT NULL DEFAULT FALSE`, `cancel_at`. `subscriptions_stripe_identity_check`: a `stripe` row has both Stripe ids and any other row has at most one. `subscriptions_stripe_period_check`: a `stripe` row has a `current_period_end`. | Guarded: refuses while any row is Stripe-sourced or in a new status |
| 000179 | Replaces `idx_subscriptions_user_active` with `idx_subscriptions_user_live_code` (UNIQUE, live code rows), `idx_subscriptions_stripe_subscription_id` (UNIQUE where set: the Reconcile upsert key, so `ON CONFLICT` repeats `WHERE stripe_subscription_id IS NOT NULL`), and the non-unique live-Stripe indexes `idx_subscriptions_user_live_stripe` and `idx_subscriptions_stripe_live_period_end`. Adds the full `idx_subscriptions_user_id`: every other `user_id` index is partial on a live status, so without it the `users` `ON DELETE CASCADE` and any history read scan the table, which Stripe rows make grow | Guarded: refuses while any user holds more than one live row |
| 000180 | `stripe_customers`: one Customer per user (PK) and one user per Customer (UNIQUE); cascades with the user | Guarded: refuses while any row exists |
| 000181 | `stripe_webhook_events`: dedupe only, no payload column, 30-day retention, `processed_at` index | Plain drop |
| 000182 | `billing_erasure_outbox`: pending Stripe Customer erasures; no `user_id`, no FK | Guarded: refuses while any row exists |

**Down guards: the 000117 shape, written schema-aware.** Each guarded down
checks `to_regclass(format('%I.<table>', current_schema()))`, takes
`LOCK TABLE … IN ACCESS EXCLUSIVE MODE`, runs its `EXISTS` test, and
`RAISE`s with `ERRCODE = 'restrict_violation'`; every statement inside the
`DO` block is qualified with `format('%I.…', current_schema())`, as in 000170.
Never write `public.` in a down (#2624). The lock comes **before** the test:
`migrationRequireGuardRunsUnderLock` (`internal/database/migration_000178_test.go`)
holds a blocking row in an open transaction, parks the down on its lock with
`testdb.WaitForRelationLockWaiter`, commits the row, and requires the refusal.
A down that tested first would miss the row, wait on the lock, then drop.

**Recovery after a refused down: force N, never N-1.** golang-migrate stamps
the down's *target* version dirty before it runs the file. The file's implicit
transaction rolls back, so the schema is still at N. Downs run 182, 181, 180,
179, 178; a multi-step down refused at N leaves N-1 dirty with the schema at N.

| Refused down | Recorded | Confirm, then force | Never |
|---|---|---|---|
| 000178 | 177 dirty | `subscriptions.plan` exists → **178** | 177: the next down runs 000177's guarded feedback-screenshot down |
| 000179 | 178 dirty | `idx_subscriptions_user_live_code` exists → **179** | 178: the next down runs 000178's down against a schema still at 179 |
| 000180 | 179 dirty | `stripe_customers` exists → **180** | 179 |
| 000182 | 181 dirty | `billing_erasure_outbox` exists → **182** | 181: 000181's down is a plain drop and silently discards the dedupe table |

000181 never refuses. Force with `./bin/migrate -command=force -force-version=N`
(the in-repo `cmd/migrate`) or `migrate -path migrations -database "$DATABASE_URL" force N`,
then boot (an `up` should report no change). In a development database whose
`stripe_customers` rows point at throwaway test-mode Customers, empty that table
(`TRUNCATE stripe_customers;` in `psql`) before running the 000180 down; never
do that in production, where a row is the only local record of which Customer
belongs to which user. There is no supported reset for 000178, 000179 or 000182:
their refusals mean billing has been used, and the rollback floor below applies.

**Indexes are plain `CREATE INDEX`.** golang-migrate sends 000179 as one
implicit transaction, where `CONCURRENTLY` cannot run (see "Transaction
shape"), and `subscriptions` holds only code grants before GA, so the
`ACCESS EXCLUSIVE` build is brief. 000181's index is on a new, empty table.
Do not generalize this to a populated table.

**Billing ops metrics (000183, PR-2, Closes #1306).** Admits
`billing_erasure_outbox_pending` (control-source gauge) and
`billing_drift_corrections_total` (control-source counter) to both
`ops_metric_*_metric_key_check` CHECKs (72 → 74 keys). It follows the 000142
metric-key protocol, not the guarded-down shape above: the down locks both
metric tables `ACCESS EXCLUSIVE`, deletes only those two series, and restores the
exact 000142 catalog, so it never refuses and needs no force-N recovery. Apply it
before code emits either key. Its test pins a literal 74-key list and asserts that
list equals `opsmetrics.Catalog()`, so a metric added to the Go catalog without a
migration fails it; 000142's test is frozen to its own literal 72.

**Wing seats (000188, PR-5, Refs #2470).** `subscription_seats`: one row per
invite, cascading from the parent `subscriptions` row and from both users
(an erased holder or payer takes the seat with them; the erasure tells the
survivor after the commit). CHECKs: payer ≠ holder; the six states; pending
has no `activated_at`; active has `activated_at` and no `ended_at`; terminal
has `ended_at` and `end_reason`. UNIQUE partial indexes: one open seat per
subscription and one active seat per holder. 000187 is #4097's, so this PR
merges after it. The down is the guarded shape above: it refuses while any
seat is pending or active. A refused 000188 down leaves **187 dirty**; confirm
`subscription_seats` exists, then force **188** (never 187, whose down would
run against a schema still at 188). **000193 (#4133 follow-up)** widens `subscription_seats_end_reason_check` to
admit `pair_ineligible` (an unfriend or block ended a pending invite; a block
ended an active seat). Its down refuses while any row carries that reason; a
refused down leaves **191 dirty** (no 000192 exists), so force **193**.
#4135 took 000194 rather than 000192, because golang-migrate never applies
a version below the one recorded.

**Rollback floor: PR-1, after the 000183 down.** An image built before 000178
cannot boot once 000178 is applied: `migrationrunner.Run` calls `m.Up()`, and
golang-migrate v4.20.1's `readUp` fails `versionExists` for a recorded version
the image does not embed (pinned by `TestRun_UpFailureIsReported`). The same
mechanism stops a PR-1 image (000178–000182) from booting once 000183 is
recorded. To roll the binary back from PR-2 to PR-1, first run the 000183 down
from the PR-2 source (`migrate … down 1`, back to 182; it never refuses and drops
only the two billing metric series), then deploy the PR-1 image. PR-1 ships the
per-source reader set beside 000179 (spec R37), so it reads every row PR-2
wrote. Going below PR-1 means running the 000178–000182 downs, which refuse once
a Stripe row, a Customer mapping or an erasure obligation exists.

**Shape rules writers keep.**
- A `stripe` row always carries `stripe_subscription_id`, `stripe_customer_id`
  and `current_period_end`. Test fixtures use `sub_test_<unique>`,
  `cus_test_<unique>` and a non-NULL period end.
- A user holds at most one live code row but any number of live Stripe rows. No
  reader may assume one live row per user.
- `stripe_webhook_events` and `billing_erasure_outbox` have no foreign key, so
  `truncateAllTablesOnce` lists them explicitly. `stripe_customers` is not
  listed: `CASCADE` from `users` clears it, as it clears the other FK children
  of `users`.

## Account-guard maturity (migration 000189, #3680)

Migration 000189 (C4 PR1; spec `docs/superpowers/specs/2026-10-23-3680-accountguard-core-design.md`
§6.1) ships **dark**: nothing reads any column or table it adds until PR3's runner.
It adds maturity, version and enrolment-provenance columns to `users` (password and
primary email, each on its own) and to `user_mfa_totp`, `user_mfa_webauthn`,
`user_sso_identities` and `user_recovery_keys`, plus five material triggers, the
global `accountguard_material_version_seq`, and the `accountguard_state` marker. The
`users` ALTERs run first, then the four factor tables, under the file's leading
`SET LOCAL lock_timeout = '3s'`. Every change to an existing table is catalog-only.

**Sentinels.** Legacy `1970-01-01Z` is the constant fast default, so existing rows
read mature and `enrolled` long ago with no rewrite (`TestMigration000189_UpDoesNotRewriteUsers`
compares `relfilenode`). Awaiting-notice `2999-01-01Z` is reserved for PR7b's stamp.
Both are finite because lib/pq does not scan ±infinity. The `isfinite` CHECKs, and
every other CHECK on a new column here, are `NOT VALID` permanently: add no
validating migration (the 000168 reasoning).

**The stamp function.** Every maturity default is `accountguard_maturity_stamp()`
(`now() + interval '72 hours'`), and PR7b re-bodies that one function. Never
`ALTER … SET DEFAULT` these columns again. `TestMigration000189_StampFunctionMatchesDeltaCP`
pins the interval at 72 h and checks each `pg_attrdef` default; PR3 rebinds it to
`decide.DeltaCP`.

**Enrolment provenance.** `enrolled_at` (legacy fast default, then `now()`) and
`enrol_class` (`smallint`, 1–3) record how a credential was enrolled. `enrol_class`
NULL means unknown, which PR2 reads at the rank floor, so it fails closed. On a
material change the trigger keys on `enrolled_at`. A writer that sets `enrolled_at`
(`IS DISTINCT FROM OLD`) keeps both columns as written, so a same-class re-enrolment
keeps its class. A writer that leaves `enrolled_at` alone gets `now()`, and its class
is nulled unless it also changed the class. Neither column is in any `UPDATE OF`
list, so a later statement that writes only them fires no trigger.
`TestMigration000189_WriterProvenanceSurvivesSameClassReenrolment` pins this for
all six slots.

**Provenance is written in pairs, and only by `internal/accountguard` (H3).** A
statement names both provenance columns of a slot or neither, in both directions:
any write that names `enrolled_at`, `password_enrolled_at` or `email_enrolled_at`
also names the matching `enrol_class`, `password_enrol_class` or
`email_enrol_class`, and any write that names a class also names its `*enrolled_at`.
The trigger cannot tell a class named with its old value from a class left out, so
a class left out of a material write that sets `enrolled_at` survives as stale
provenance. Non-test writes of any of the six columns live in `internal/accountguard`
alone (H3-b's `EnrolAtRegistrationTx` included). A source-scan test enforces both
directions per slot prefix (`""`, `password_`, `email_`) and the confinement.

**Maturity writers are confined the same way, now rather than in PR6.** The
provenance scan also flags any non-test literal outside `internal/accountguard` that
names `enrolled_at` or `enrol_class` under any prefix, or `matures_at`,
`password_matures_at` or `email_matures_at`. It checks `migrations/*.sql` too, where
only 000189 may name them, and forbids exported consts and vars in
`internal/accountguard` that hold such a literal, so a package-level name cannot hand
a column to another package. The scan reads source literals. It does not see SQL
assembled at runtime, and it does not replace PR6's guard triggers, which still land
as planned.

**The material trigger.** Each table has one trigger named `accountguard_material`:
`BEFORE INSERT OR UPDATE OF …` on `users` and the four factor tables (a column list
narrows only the `UPDATE` event, so every `INSERT`, `COPY` and upsert attempt fires
it). All five are `ENABLE ALWAYS`, so `session_replication_role = replica` cannot skip
them (red-team RT-3; the compose app role is a superuser, and
`internal/authority/store/serve.go` refuses `replica` for the same reason). `ALWAYS`
does not stop the table owner running `DISABLE TRIGGER`; that is the accepted
boundary. PR6's guard triggers must sort before that name. The per-table rules are in the up file's header (`users`: `password_hash` and
`email` independently; TOTP: `user_id`, `enabled` or `confirmed` false→true, a
changed secret; WebAuthn and SSO: identity and key columns; recovery key: the five
key columns). On a factor `INSERT` the trigger sets only the version; maturity and
provenance come from the column defaults or the writer, which H3-b's registration
stamp relies on. On a `users` `INSERT` it sets only the two versions. On a material change it moves the version, re-stamps maturity and
resets provenance. A writer-supplied `matures_at` in the same `UPDATE` is kept; a
direct write to a version column is discarded, on `INSERT` and `UPDATE` alike, so
versions move only through the trigger. "Material" means exactly "the trigger moved
a version on an `UPDATE`", which is #2627's record key.

**Every credential version comes from one global sequence (amendment A-16, red-team
F1 and RT-4).** `accountguard_material_version_seq` (`AS bigint START WITH 2 NO CYCLE`,
`USAGE` granted to `current_user` as 000159 does) supplies all six version slots:
`users.password_version`, `users.email_version` and the four factor tables'
`material_version`. It is drawn on every `INSERT`, discarding any writer-supplied
value (`users` included, both slots), and on every material `UPDATE`. A non-material
`UPDATE`, the `mfa-rekey` rewrap included, keeps `OLD`. The reason is identity reuse: `user_mfa_totp` and
`user_recovery_keys` are keyed by `user_id`, so with `INSERT` at the default 1 and a
bump of `OLD + 1`, a remove-then-re-enrol handed the new credential a
`(kind, id, version)` its predecessor held. A material `UPDATE` must draw from the
sequence too, never take `OLD + 1`: mixing the two collides, because `OLD + 1` can
equal the value the sequence next hands a re-inserted slot. START 2 keeps every
drawn value apart from the legacy fast default 1, which stays the column default.
`users` is on the sequence too: its first form used `OLD + 1`, which hands a
rolled-back write's version to the next material change (red-team RT-4), so it never
takes `OLD + 1`. The trigger names the sequence through `TG_TABLE_SCHEMA`, never
through `search_path`: a relation lookup searches `pg_temp` first, so a session's
`TEMP SEQUENCE` of the same name would otherwise choose the version. The sequence
`USAGE` grant assumes the runtime role is the role that runs the migration (as in
000159); a separate runtime role would fail every material write, password and email
changes included, until granted `USAGE`. `accountguard_is_rewrap()` and
`accountguard_maturity_stamp()` resolve through `search_path` by design (`pg_temp` is
never searched for functions); shadowing one needs arbitrary SQL as the table owner,
who can already `DISABLE TRIGGER`.

- **Versions are equality-only.** They have gaps (a rolled-back write, and the
  `BEFORE INSERT` that fires on an upsert's conflict path, each spend a value) and no
  order across slots. Never compare them with `<`, never derive a count from them.
- **No version, users or factor, leaves the server.** The counter is global, so a
  version would leak cross-account enrolment volume. No response body, notice, security
  event or user-facing change record carries one; guard and ceremony tokens are
  opaque to their holder (a server-side handle, or AEAD-sealed). Server logs may.
- **A down→up restarting the sequence at 2 is safe, so the down drops it
  unguarded.** The restart can re-issue a value a previous run drew; that is harmless
  only because nothing outside the database holds one. Every table that can hold a
  version reference (000190 onward) is
  dropped by an earlier down, and tokens and jobs exist only behind activation,
  which this down refuses. Contrast 000159's down, which refuses to drop a consumed
  `voice_authorization_revision_seq` because its references live outside the
  database. A future table or token that stores a factor version outside that
  envelope breaks this argument and must add the 000159 refusal.
`TestMigration000189_CredentialColumnsAreAllClassified` fails on any credential-table
column nobody classified, and `…MaterialClassificationMatchesTrigger` checks each
classification against the trigger.

**The rewrap flag.** `concord.credential_rewrap = 'mfa-rekey'` set transaction-local
(`set_config(…, true)`) makes the TOTP trigger treat a rekey as non-material, but only
when `key_version` moved and nothing besides `totp_secret_enc`, `totp_secret_nonce`,
`key_version` and `updated_at` changed (red-team RT-2: without the `key_version`
requirement, a second writer of the flag could swap a secret silently). Only
`internal/mfa/rekey.go` sets it, committing one row per transaction. The source-scan
test is case-insensitive (Postgres GUC names are) and flags the GUC name and the value
`'mfa-rekey'` in any other non-test source, reuse of the identifiers `rewrapFlag`,
`rewrapValue` and `resealRowTx` elsewhere in `internal/mfa`, and any mention in
`migrations/*.sql` outside 000189 and 000191. It reads source text, so it cannot see a GUC name
assembled at runtime.

**Never set `concord.credential_rewrap` at role, database or connection level**
(`ALTER ROLE … SET`, `ALTER DATABASE … SET`, or `options=` in `DATABASE_URL`). A
setting at those levels is not transaction-local and would make every flagged write look
like a rewrap. The trigger's `key_version` requirement narrows that exposure; it does
not remove the rule.

`mfa-rekey` pins `material_version` in its compare-and-set and reads it back with
`RETURNING`. If the trigger moved the version anyway (the exemption was not applied),
the row is rolled back and the run aborts with an error naming the user id and key
version, so the fallback is never silent. An aborted run still prints its partial
result, `Failed` rows included, then exits 1; a re-run converges. Every reader uses
`NULLIF(current_setting(name, true), '')`: on PostgreSQL 16, after a transaction-local
`set_config` commits, the same session reads `''`, not NULL
(`TestMigration000189_GUCReadsEmptyAfterLocalSetCommits`).

**`accountguard_state`.** A singleton (`singleton boolean … CHECK (singleton)`) with
one row inserted and `activated_at` NULL. PR3's runner is the first reader and must
treat a missing row as an error, never as "not activated"; PR7b sets `activated_at`.

**The down** refuses while `accountguard_state.activated_at` is set or while any
`users.email_contested_at` or factor `maturity_contested_at` is set. It does **not**
refuse on an immature credential: before activation nothing reads maturity, so a stamp
carries no state (spec §6.1, D-2; amendment A-14 in the #3662 design §14). It takes
`ACCESS EXCLUSIVE` on `users`, the four factor tables, then `accountguard_state`
before checking (000178 shape), and drops `accountguard_material_version_seq` last,
after every function that draws from it. The file opens with
`SET LOCAL lock_timeout = '3s'`, so an unbounded lock wait cannot queue all sign-in
traffic behind it; a timeout fails the file and rolls the down back whole. After a
refusal or a timeout, confirm `accountguard_state` exists, then `force 189`, never
`force 188`.

**E1: an older image refuses a newer schema.** golang-migrate's `readUp` fails
`versionExists` for a recorded version the image does not embed, and
`migrationrunner.Run` returns that as fatal, so a binary-only rollback past any
migration crash-loops instead of downgrading quietly. Pinned by
`TestRun_OlderSourceRefusesNewerSchema` and
`TestRun_ControlPlaneTreeWithoutNewestRefuses` (`internal/migrationrunner/runner_test.go`).

## Account-guard rewrap binding (migration 000191, #3680 follow-up)

Migration 000191 lands the red-team-critical F1 and F2 findings on #4129 without
editing 000189, which shipped.

**The flag's value is bound to its transaction (F2).** 000191 replaces
`accountguard_is_rewrap()` with `CREATE OR REPLACE` (same OID, so the five triggers
need no change). It now accepts only `'mfa-rekey:' || pg_current_xact_id()`, and
`internal/mfa/rekey.go` writes that value in the same change. A writer that leaves the
flag at session level (`set_config(…, false)`) no longer exempts a later transaction's
write, whether the value is the bare `'mfa-rekey'` or one bound to the setting
transaction. `pg_current_xact_id()` returns the top-level id inside a savepoint, and
every caller is a material trigger on a write, which already holds an id. The binding
stops a mistaken writer, not a session that runs arbitrary SQL: that session can predict
its next transaction id, and as the table owner it can already `DISABLE TRIGGER` (the
same accepted boundary as `search_path`, above). An `mfa-rekey` built before 000191
writes the bare value, so run it from the image that carries the schema
(`docs/runbooks/mfa-key-rotation.md`).

**The down fails closed on a missing activation row (F1).** 000189's down reads a
missing `accountguard_state` row as "not activated". Every rollback to before 000189
passes through 000191's down first. That down takes `SHARE` on `accountguard_state`
under `SET LOCAL lock_timeout = '3s'` and refuses (`restrict_violation`) while the
table exists and holds no row, then restores 000189's function body. It cannot see a row
deleted after it ran and before 000189's down; a `BEFORE DELETE OR TRUNCATE` refusal
on the table is deferred to PR7b. After a refusal, restore the row and `force 191`.
Pinned by `internal/database/migration_000191_test.go`.

## Migration linting (squawk, #4175)

SonarCloud has no PostgreSQL analyzer, so every `.sql` file is excluded from
it. [squawk](https://squawkhq.com) (`squawk-cli` 2.70.0) lints migrations
instead, in two places that share `.squawk.toml` at the repo root:

- the `squawk` pre-commit hook, on the `services/control-plane/migrations/*.up.sql`
  files being committed;
- the `migration-lint` job, on the up migrations the PR adds or changes against
  main. It is advisory in `pr-ci.yml` and required through its copy in
  `required-pr-build.yml` (`Protected migration lint`), which feeds
  `Required PR build result`. Change the two together.

Neither one lints the applied history. Those files are immutable and already
applied in prod, so they are baselined, not rewritten. The version is pinned in
the hook's `additional_dependencies` and in `.github/requirements-squawk.txt`
(hashed, for CI). Bump both together.

CI reads `.squawk.toml` and `.github/requirements-squawk.txt` from main, not
from the PR, so a PR cannot loosen the check it is judged by. A change to
either takes effect in CI only after it merges; until then the hook (which
reads the working tree) is the only place to see its effect.

What `.squawk.toml` sets, and why:

- `pg_version = "16"`, matching prod.
- `assume_in_transaction = true`, because of the transaction shape above. Each
  file runs as one implicit transaction, so squawk stops asking for
  `IF NOT EXISTS` on every statement and stops flagging indexes on tables
  created in the same file. A `CREATE INDEX` without `CONCURRENTLY` on an
  existing table is still flagged.
- `*.down.sql` is excluded: rollbacks drop what the up created by design.
- Five rules are excluded: `require-lock-timeout` and `require-statement-timeout`
  (no migration sets them, and adopting them is a convention change), plus
  `prefer-text-field`, `prefer-bigint-over-int` and `prefer-bigint-over-smallint`
  (column-type style, which also fires on brand-new tables). Changing this list
  is a team decision, not covered by the Sonar no-tuning rule.

A one-statement `CREATE INDEX CONCURRENTLY` file runs outside any transaction,
so squawk asks for `IF NOT EXISTS` there. Add it, but do not treat it as
recovery. A failed concurrent build leaves an INVALID index behind, and
`IF NOT EXISTS` skips that index and reports success, so a replay leaves an
index no query can use. Before replaying a failed concurrent-index migration,
find the leftover (`SELECT indexrelid::regclass FROM pg_index WHERE NOT
indisvalid`) and either drop it with `DROP INDEX CONCURRENTLY` or rebuild it with
`REINDEX INDEX CONCURRENTLY`
([PostgreSQL 16](https://www.postgresql.org/docs/16/sql-createindex.html#SQL-CREATEINDEX-CONCURRENTLY)).

When a warning is wrong for one statement, for example a validated
`ADD CONSTRAINT` on a table known to be small (000193), suppress it on that
statement only, with the reason beside it:

```sql
-- subscription_seats holds at most one open seat per subscription.
-- squawk-ignore constraint-missing-not-valid
ALTER TABLE subscription_seats ADD CONSTRAINT ...
```

Never add a rule to `excluded_rules` to quiet one migration.

## Down Migrations
- Must reverse ALL up migration changes
- Handle partial success scenarios
- Use `CASCADE` only when necessary and safe

## Security
- No plaintext secrets or credentials in SQL
- Sensitive columns use appropriate types (BYTEA for encrypted, TEXT for hashed)
- RBAC-related migrations require security review
- No string concatenation for data manipulation — use parameterized queries

## Key Tables Reference
- `users`, `servers`, `channels`, `messages` — core entities
- `dm_conversations`, `dm_messages`, `dm_message_reactions` - DM conversations/messages/reactions
- `channel_keys`, `dm_channel_keys` — E2EE key material with epoch tracking
- `refresh_tokens`, `sessions` — auth state
- `server_members`, `member_roles`, `role_permissions` — RBAC
- `user_presence_settings`, `presence_history`, `presence_settings_pending_operations` (migration 000087, #1235) — opt-in history settings/version marker, self-owned interval ledger, and durable pending-delivery quarantine; use the lock/downgrade rules above
- `activity_settings_pending_cleanups` (migration 000098, #2231) — one durable, versioned Rich Presence before/after policy bracket or completed-suppression receipt per user; use the advisory-lock and guarded-downgrade rules above
- `presence_active_pending_plans` (migration 000111, #2448) — durable per-subject reconciliation obligations for the two active Rich Presence categories: PK `(user_id, category)`, FK `ON DELETE RESTRICT`, subjects and generations only — never an audience, a conversation, or a social-graph edge. Use the ratchet, evidence-check and guarded-downgrade rules above
- `age_verification_records` (migration 000069, #1623) — identity-blind age claim, one row per user (`UNIQUE(user_id)`), booleans + a `jurisdiction_obligation` integer + client RSA-PSS signature metadata only. **No DOB / age value / jurisdiction name / IP / location column** — the schema is the privacy guarantee (ADR-0025). `users.disabled` / `disabled_reason` / `disabled_at` (same migration) are the terminal soft-disable set by the `valid_age=false` enforcement path; distinct from the legacy never-enforced `users.age_verified`.
- `message_purges` (migration 000090, #1352) — privacy-safe bulk-purge audit, mirrors the `account_deletions` pattern: `actor_id`/`target_user_id` FK `ON DELETE SET NULL`, counts + context + range only — **never message content or usernames**. `status` (`in_progress`→`completed`) makes a crashed batched purge traceable/resumable and keeps the schema async-ready. `include_pinned BOOLEAN NOT NULL DEFAULT TRUE` (migration 000164, #3458) records whether a purge deleted pinned messages. TRUE is the honest default, not a placeholder: every purge before 000164 deleted pins, and the writers that omit the column still do — historical rows (a clear-reap row written before 000164 included) and an older binary during a rolling deploy. The expiry writer was on this list until 000168 (#3459) and is not any more: `purge.deleteExpiryBatch` inserts its `reason = 'expiry'` row INSIDE the batch transaction, after locking the parent context `FOR SHARE` and then its victims, with a client-generated id and `include_pinned = E OR any pinned victim` (E is the parent's effective include-pinned value, read under that lock). FALSE therefore needs E false AND no pinned victim. The parent lock comes first so a keep-pins PATCH, which holds the parent `FOR NO KEY UPDATE` while it parks pins, is ordered wholly before or after the batch: locking victims first let a batch delete a pin the PATCH was about to park (#3459 Codex review). Parent then message is every writer's order, so it adds no lock edge. A due-before-keep or late-pinned victim records TRUE, as #3458 I5 requires, and an expiry in a keep-pins context that deletes only unpinned rows records FALSE. The old pool write that preceded the batch (and recorded TRUE) is gone, so a failure before COMMIT leaves neither a row nor a deletion and the result carries no `PurgeID`; a COMMIT error is settled by reading the known id back (`reconcileExpiryCommit`). Migration 000164's header statement that the expiry writer records TRUE is superseded by this rule; the applied file is not edited. A user-requested channel, server, DM, kick or ban purge writes the request's value, and the clear reap writes it explicitly (#3458 §18.2): FALSE for a batch bounded by the Clear watermark, which keeps every pin, TRUE for the zero-participant batch, which deletes them. The down is deliberately unguarded: unlike the 000098/000130/000155 guards it drops an attribute of retention, not evidence of a deletion, and a guard would refuse after the first default purge. It still keeps the evidence (#3552 review): an `AFTER INSERT` trigger records each purge that keeps pins in `message_purges_kept_pins` in that purge's own transaction, the down drops the trigger and the column without copying anything, and the next up restores FALSE from the table, so a down→up round trip no longer relabels a keep-purge TRUE. The down copies nothing on purpose: a copy under its ACCESS EXCLUSIVE lock would hold every reader and writer for a scan of the whole append-only table. The evidence table has no foreign key, because 000090's down drops `message_purges` without CASCADE.
- `dm_message_hidden_ranges` (migration 000090, #1352) — per-user persistent receiver-hide for DM/group purges: `[hidden_from, hidden_to)` rows, merged on insert. A range never hides a pinned message (#3458): `purge.HiddenRangeFilter` (`dmvisibility.HiddenRangeFilterForViewerExpr`) carries `AND pinned_at IS NULL`, so each hide stores one row and a pinned message is visible to everyone until it is unpinned. Unpinning a message inside a range hides it again. `hidden_count` counts only unpinned peer messages. Clear leaves pinned messages in place: the reap's below-watermark select and the sweeper's candidate query skip them, and the zero-participant select still deletes them because nobody can see them. Two consequences follow from every reader inheriting the filter: the conversation-list unread count and last-message preview include a pinned message under a Clear range, and an author can still edit or delete their own pinned message inside their own cleared range. Every `dm_messages` content read (scroll fetch, list preview, unread count, pins) must apply the `purge.HiddenRangeFilter` anti-join or the hide is silently defeated. `privacy_settings.require_auth_before_purge` (same migration) is the DM/group step-up toggle — reads fail CLOSED (missing row/error ⇒ step-up required), and flipping it OFF must itself require step-up (constraint on #1354).
- `media_files.blob_reaped_at` (migration 000090, #1352) — the purge reaper's confirmed-delete marker, and the only thing that retires a row from the straggler sweep. `deleted_at` cannot serve: it records that the METADATA row was soft-deleted, not that the object was removed. The sweep remains bounded to **`media_tier = 2`**; current Tier-1 profile media uses stable `profile_slot` rows and immutable generation-suffixed keys, while legacy exact `avatars/<userID>`/`banners/<userID>` rows are compatibility evidence handled by the Tier-1 obligation rail. Do NOT widen the sweep to Tier 1. Only `deleted_at IS NOT NULL` rows may carry the marker, and `purge.reapSweptBlob`'s live-key guard stays as defense-in-depth behind the tier bound.
- `refresh_tokens.predecessor_id` (migration 000100, #2428) — nullable lineage column set at rotation time to the revoked source token's id. **Two writers** stamp it: `rotateAndRespond` (the successor's `predecessor_id` = the revoked source id) and the grace-recovery re-mint (`predecessor_id` = the revoked successor's id, keeping the chain strictly linear — decision D1). Grace-period refresh recovery selects the exact live successor by `predecessor_id = <replayed id> AND revoked_at IS NULL AND expires_at > NOW()` (not a time window; the `expires_at` filter mirrors the normal-refresh expiry gate so an already-expired lineage fails closed to 401 — CWE-613), inside the users `FOR NO KEY UPDATE` transaction so it composes with the #2201 credential-epoch fence. Plain partial `CREATE INDEX ... WHERE predecessor_id IS NOT NULL` (this migration file carries more than one statement, and `CONCURRENTLY` cannot run inside a multi-statement file — see "Transaction shape" above — so no `CONCURRENTLY` here). No self-FK — a dangling id is harmless behind the `revoked_at IS NULL` filter, avoiding an `ON DELETE` dance with the 90-day revoked-token purge. No backfill: existing rows read NULL as "no recorded successor" → fail-closed 401 — a pre-migration token can still be grace-recovered if it rotates *after* the migration, because that rotation stamps the lineage edge on the new successor.
- `users.username` case-insensitive uniqueness — `users_username_lower_key` UNIQUE index on `LOWER(username)` (migration 000079, #1931). Usernames are stored lowercase regardless of registration path (password AND SSO normalize at the boundary); identity is case-insensitive everywhere (login / existence / search / uniqueness / friend-add all key on `LOWER(username)`). The original raw `users_username_key` is retained but redundant once rows are folded. The migration is transactional: a pre-existing duplicate-by-case pair aborts it (fail-closed `DO`-block `RAISE`); the case-fold `UPDATE` is irreversible (down only drops the index).
- `subscriptions` (migrations 000070, 000083, 000110, 000178, 000179; epic #2033) — one entitlement row per grant, `source` ∈ {`code`, `stripe`}. The status CHECK admits every Stripe status; live = `active, trialing, past_due`. At most one live code row per user (`idx_subscriptions_user_live_code`) beside any number of live Stripe rows; `stripe_subscription_id` is unique where set (the Reconcile upsert key). See "Stripe billing" above.
- `stripe_customers` (migration 000180, #2033) — user ↔ Stripe Customer, one each way; `last_checkout_at` feeds the drift worker's discovery pass; cascades with the user; guarded down.
- `stripe_webhook_events` (migration 000181, #2033) — webhook dedupe by `evt_` id; no payload column; 30-day retention by `processed_at`; plain down.
- `billing_erasure_outbox` (migration 000182, #2033) — pending Stripe Customer erasures, written with `ON CONFLICT (stripe_customer_id) DO NOTHING`; no user id and no FK, so an obligation outlives its user; guarded down.
- `accountguard_state` (migration 000189, #3680) — singleton activation marker for the account guard: one row, `activated_at` NULL until PR7b sets it; first reader is PR3's runner, which treats a missing row as an error. Guarded down; see "Account-guard maturity" above.
