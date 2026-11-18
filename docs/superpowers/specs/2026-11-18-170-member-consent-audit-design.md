# Security #170: Member Consent, Durable Restrictions and Atomic Audit (Design)

- Issue: Concord-Voice/Security#170 (parent epic Concord-Voice/Security#152)
- Target: `services/control-plane`, branch `feature/security-170-member-consent-audit`
- Line anchors: HEAD `6ba00d24` (spot-checked again when this spec was written)
- Batch position: 2 of 6 (Security#168, then #170, #167, #169, #173, #172). Migration number: 000198.

## How this design was settled

This spec was written in automated mode. A design panel produced a recommended direction and a reconciled spec. The project lead accepted decisions D-170a to D-170f on 2026-11-18 ("Accept all"). Where the brainstorming process would ask the developer a question, the answer comes from those decisions or from the panel, and is recorded below as an answer or as an architect call. Calls that the decisions do not cover are listed in "Open calls" and are not settled by the lead.

| Question the process asks | Answer | Source |
|---|---|---|
| Why now? | The 2026-11-11 vulnerability scan found six open defects. VULN-051 (High) lets an attacker manufacture a shared-server relationship and pass DM, friend and presence privacy gates. This blocks release 1.0. | Issue, scout |
| What does success look like? | Membership exists only because the target joined by invite. A timeout, server mute or server deafen survives any exit and re-entry until a moderator lifts it. An action and its audit row commit together or not at all. Invite lookup reveals nothing for a dead invite. | Issue Definition of Done |
| Anti-goals | A fix that blocks a legitimate leave. A fix that needs a client release. Two live copies of restriction state that can drift. An audit write that caller text can make fail. | Issue Option B critique, D-170b, panel |
| Direct add: consent invitation or removal? | Remove it. No consent-invite feature. No first-party client or Admin Console code calls it. | D-170a |
| Dead invite response? | Exactly `200 {"valid":false}`. Unknown codes stay 404. | D-170b |
| Which reason characters are rejected? | NUL and C0 controls except tab, line feed and carriage return; cap 512. NUL is also stripped in `auditMetadataForStorage`. | D-170c |
| Does a kick or unban clear a stored restriction? | No. Only a moderator lift clears one. | D-170d |
| Audit failure posture? | Ban, kick and timeout roll back and answer 500. | D-170e |
| Related audit gaps? | Fold in ownership writes, voice move audit, and server mute/deafen audit rows. | D-170f |

## Goal

Server membership exists only because the target joined through an invite. A timeout, server mute or server deafen survives any exit and re-entry until a moderator lifts it. A moderation action and its audit row commit together or not at all, and no caller-controlled text can make the audit insert fail. `duration_seconds` is validated before arithmetic. Invite lookup returns server identity only for a usable invite.

## Problem

- **Membership is not consent (VULN-051, High).** `server_members` rows are a trust signal for DM privacy (`dm/handlers.go`), friend eligibility (`friends/privacy.go`) and the presence audience (`presence/audience.go`). `POST /servers/:id/members` (`AddMember`, `members/handlers.go:473`) lets an actor with invite permission create one for any user.
- **Restrictions die with the row (VULN-052, Medium).** `timed_out_until`, `server_muted` and `server_deafened` live on the membership row. Self-leave or kick (`:1373`) and ban (`:2104`) delete the row. Rejoin (`invites/handlers.go:180-185`) inserts column defaults.
- **Audit is detached from the action (VULN-055 Low, VULN-056 Medium).** The audit row is written after commit, on the request context, and its error is discarded or only logged. A NUL in `reason`, a client disconnect or a database error yields a committed action with no audit row.
- **Duration wraps (VULN-057, Low).** `time.Duration(req.DurationSeconds) * time.Second` is computed before the bound check, and the audit records the raw operand.
- **Invite lookup leaks (VULN-061, Low).** `GetInviteInfo` (`invites/handlers.go:690`) returns server id, name, icon, banner and member count for revoked, expired and exhausted invites.

## Constraints

| Constraint | Source |
|---|---|
| Remove direct add; no consent-invite feature | D-170a |
| Dead invite answers exactly `200 {"valid":false}`; unknown code stays 404 because Desktop `AddFriendModal.checkIfServerInvite` returns `res.ok` | D-170b; `.claude/rules/domains/backend.md` client compatibility |
| `binding:"max=512"` counts runes (validator uses `utf8.RuneCountInString`) | D-170c; Context7 check |
| No removal clears a stored restriction | D-170d |
| Ban, kick and timeout fail closed on audit failure | D-170e |
| A dangerous-action gate refusal writes nothing, including no audit row | `backend.md` dangerous-action gates (I-TX) |
| In `WithGatedTx` closures, presence capture precedes `DELETE FROM server_members`; `presencehook.Complete` owns the commit | comments at `members/handlers.go:1311-1323` |
| Lock order: visibility advisory lock, writer barrier, users, servers, child rows | `backend.md` I-ORDER; `members/handlers.go:1395-1420` |
| No blank-discarded errors; `tx.*` inside transactions; commit error checked | `backend.md` error handling, database |
| Migration: paired down file, `TIMESTAMPTZ`, explicit `ON DELETE`, one file is one transaction, squawk-clean | `.claude/rules/migrations.md`, `.claude/rules/domains/migrations.md` |
| Integration tests use real PostgreSQL behind the shared test-DB advisory lock; tests must not pass or fail for the wrong reason | `.claude/rules/domains/tests.md` |
| No client change | D-170a, D-170b |

## Approaches

Restriction persistence was the only axis with real freedom. All three options use the same table, `server_member_restrictions(server_id, user_id, timed_out_until, server_muted, server_deafened)`.

| Option | How it works | Trade-off |
|---|---|---|
| 1. Tombstone, in Go (chosen) | The membership row stays the only authority while the user is a member. The two exit transactions capture live restriction state in the statement before the delete. The one re-entry insert restores and consumes it in the same statement. | Fewest touch points (two exits, one entry), all in transactions that already hold the needed locks. No backfill and no drift. A future exit path that skips capture loses state, so guard tests pin every insert and delete literal. |
| 2. Dual-write (the issue's scope text) | The six writers write the row and the table; re-entry copies the table to the row. | Two live copies can drift, and a missed lift re-imposes a restriction. Needs a backfill. Puts more edits in `voice/handlers.go`, which Security#168 rewrites first. |
| 3. Database triggers | A delete trigger captures, an insert trigger restores. | Covers every path including raw SQL. Behaviour is invisible in Go and in fake-database tests. Cascade deletes of a server or user would fire capture against a parent being deleted, which is unverified. |

Rejected outright: soft-deleting membership rows. Thirty-seven files read `server_members` as "is a member", and one missed filter recreates VULN-051.

**Pick: Option 1**, by the panel and confirmed by a census at HEAD. Non-test Go has exactly two `DELETE FROM server_members` sites (`members/handlers.go:1373`, `:2104`). After direct add is removed, the only re-entry insert is `invites/handlers.go:182`. `servers/handlers.go:417` is the owner row of a new server and cannot have a tombstone. No migration defines a trigger on `server_members`. This deviates from the issue's scope wording ("writers write it") but meets its Definition of Done and contradicts no binding decision. It is listed as open call 1.

What would change the pick: a third exit or entry site appears (move to Option 3), or the lead wants moderators to view or lift restrictions of departed users (move to Option 2).

Audit has one shape under D-170e: `AuditWriter.LogTx` inside each action's transaction. Detaching contexts with `context.WithoutCancel` (issue Option C) was rejected because it leaves the audit row outside the action's transaction.

## Design

### 1. Direct add is removed (VULN-051)

Delete the route (`api/router.go:2455-2459`), `AddMember`, `addMemberTx`, `respondAddMemberError`, `addMemberInsertQuery`, and the members copy of `hydrateJoinerPresence` (its only caller is `AddMember`). Delete their tests (`add_member_gate_test.go`, `add_member_presence_test.go`, `add_member_server_vanish_race_test.go`, and the `AddMember` cases in other files) and the OpenAPI operation.

`presencecapture.FamilyMemberAdd` and its registry entry stay, with a comment that no production caller remains. The enum is positional and append-only (`presencecapture/capture.go:45-57`), so deleting the constant would renumber live families.

The router does not set `HandleMethodNotAllowed`, so the removed route answers the router's 404. A test pins that POST returns no 2xx and writes no `server_members` row, and that the DM and friend-request privacy gates still refuse the attacker.

`invites.JoinServer` is then the only admission path.

### 2. Restriction tombstones (VULN-052)

**Migration `000198_create_server_member_restrictions`.**

- Columns: `server_id UUID NOT NULL REFERENCES servers(id) ON DELETE CASCADE`, `user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE`, `timed_out_until TIMESTAMPTZ NULL`, `server_muted BOOLEAN NOT NULL DEFAULT false`, `server_deafened BOOLEAN NOT NULL DEFAULT false`, `created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()`.
- `PRIMARY KEY (server_id, user_id)`, an index on `user_id` for the cascade, and `CHECK (NOT server_deafened OR server_muted)`.
- No reason, actor or `updated_at` column. The audit log holds who and why.
- Down: `SET LOCAL lock_timeout = '3s'`, `LOCK TABLE server_member_restrictions IN ACCESS EXCLUSIVE MODE`, then `RAISE EXCEPTION ... USING ERRCODE = 'check_violation'` while any row has `server_muted OR server_deafened OR timed_out_until > NOW()`, then `DROP TABLE`. The file comment says a rollback discards departed members' restrictions. This follows `000197_add_admin_session_generation.down.sql`.
- No backfill: a current member's state is on the row, which stays the authority.

**Invariants.**

- R1. While a membership row exists it is the only authority. No reader or lift endpoint consults the table.
- R2. Capture runs in the same transaction as `DELETE FROM server_members`, as the statement immediately before it, at both exit sites. In the kick/leave closure it goes first in the existing `queries` slice, so it follows presence capture, and a failure abandons the presence plan.
- R3. Capture writes only live state. An expired timeout is stored as NULL. A row with nothing live writes no tombstone. The mute flag is stored as `server_muted OR server_deafened`, so the CHECK can never fail an exit.
- R4. Capture is a no-op when no membership row exists. A ban of a non-member leaves an existing tombstone untouched (D-170d).
- R5. Restore happens inside the statement that inserts the membership row, and consumes the tombstone only when no membership row exists. A retry that hits `ON CONFLICT DO NOTHING` consumes nothing.
- R6. Lift endpoints keep answering 404 for a non-member.

**Reference statements** (the plan pins the final text; a real-PostgreSQL test proves each).

Capture, parameters `(server_id, user_id)`:

```sql
INSERT INTO server_member_restrictions (server_id, user_id, timed_out_until, server_muted, server_deafened)
SELECT sm.server_id, sm.user_id,
       CASE WHEN sm.timed_out_until > NOW() THEN sm.timed_out_until END,
       sm.server_muted OR sm.server_deafened, sm.server_deafened
FROM server_members sm
WHERE sm.server_id = $1 AND sm.user_id = $2
  AND (sm.timed_out_until > NOW() OR sm.server_muted OR sm.server_deafened)
ON CONFLICT (server_id, user_id) DO UPDATE
SET timed_out_until = EXCLUDED.timed_out_until,
    server_muted = EXCLUDED.server_muted,
    server_deafened = EXCLUDED.server_deafened
```

Restore, replacing the first statement of `addMemberToServer`:

```sql
WITH restored AS (
    DELETE FROM server_member_restrictions
    WHERE server_id = $1 AND user_id = $2
      AND NOT EXISTS (SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)
    RETURNING timed_out_until, server_muted, server_deafened
)
INSERT INTO server_members (server_id, user_id, role, joined_at, timed_out_until, server_muted, server_deafened)
SELECT $1, $2, 'member', NOW(),
       (SELECT CASE WHEN timed_out_until > NOW() THEN timed_out_until END FROM restored),
       COALESCE((SELECT server_muted FROM restored), false),
       COALESCE((SELECT server_deafened FROM restored), false)
ON CONFLICT (server_id, user_id) DO NOTHING
```

**Go shape.**

- `addMemberToServer(tx txQuerier, serverID, userID, inviteID string) error` and `checkBanAndMembership` keep their signatures, and the restore stays one `tx.Exec`.
- `JoinServer` reads the new row's `timed_out_until` in the transaction after the insert. After commit, when it is in the future, it broadcasts the same message type and payload that `members.broadcastTimeout` sends. A test pins that the payloads match.
- Concurrent joins by the same user are serialized by `rbac.LockAuthorityPrincipalsTx` (`invites/handlers.go:465`); exits by the membership row lock.

### 3. Audit inside the action's transaction (VULN-055, VULN-056)

**Primitive, in `rbac/audit.go`.**

```go
func (a *AuditWriter) LogTx(ctx context.Context, tx *sql.Tx, serverID string, actorID *string,
    action, targetType string, targetID *string, metadata map[string]interface{}) (AuditReceipt, error)

func (r AuditReceipt) Confirm(ctx context.Context) // zero value is inert
```

- A1. `LogTx` takes `*sql.Tx`, so passing the pool is a compile error.
- A2. `LogTx` shares the marshal and scrub path with `Log`. It never emits `ReasonAuditCommitted`. `Confirm` emits it and writes the info log, and callers call it only after a confirmed commit. A failed insert emits `ReasonAuditWriteFailed` at once.
- A3. A nil writer or nil transaction returns an error. No in-scope path keeps an `if h.audit != nil` skip. Router wiring fails at startup when the members, voice or ownership handler has no writer.
- A4. `LogTx` is the last statement before the commit owner runs (`tx.Commit` or `presencehook.Complete`). Every refusal point precedes it, so a refused request writes nothing (I-TX). On error inside a `WithGatedTx` closure the presence plan is abandoned.
- A5. On any `LogTx` error the route answers its existing 500 body. No new error body.
- A6. `auditMetadataForStorage` removes U+0000 from every string at every depth, map keys included. Permission values stay decimal strings and `duration_seconds` stays a number. Round-tripping through `encoding/json` with `UseNumber` is the suggested way to reach typed values.
- A7. No `context.WithoutCancel`. A cancelled request rolls back the action and the audit row together. The detached write at `ownership/handlers.go:1551-1553` goes away.

Third-party behaviour this design relies on (fetched with Context7 on 2026-11-18):

- lib/pq (`/lib/pq`): after a statement error the transaction is in the failed state; `Commit` then rolls back and returns `pq.ErrInFailedTransaction`. A failed `LogTx` therefore cannot be caught and continued; the caller returns the error. If the context was cancelled before `Commit`, `Commit` returns `driver.ErrBadConn` and nothing commits. An I/O error after `COMMIT` was sent is ambiguous: the action and the audit row are then both present or both absent, so the receipt is not confirmed and the route answers 500.
- go-redis v9 (`/redis/go-redis`): a command on a cancelled context fails with `context.Canceled` before any bytes are sent. Post-commit publishes in this change must not reuse a request context that may already be cancelled. The WebSocket hub path used by `broadcastTimeout` takes no request context, so the restored-timeout broadcast reuses it.
- gin (`/gin-gonic/gin`): `ShouldBindJSON` runs `binding` tags through go-playground/validator and returns `validator.ValidationErrors`; the route maps that to its existing 400.

**Sites.**

| Action | Audit action | Today | Change |
|---|---|---|---|
| Timeout set | `member_timed_out` | Autocommit update, discarded audit error | New transaction (section 4) |
| Timeout clear | `member_timeout_removed` | Autocommit, warn only | New transaction |
| Role label | `member_updated` | Autocommit, warn only | New transaction |
| Unban | `member_unbanned` | Autocommit, warn only | New transaction |
| Kick, leave | `member_removed`, `member_left` | After commit (`auditMemberRemoval`) | In the `execRemovalFenceTx` closure before `Complete`; receipt carried in `memberAuthorityFence` |
| Ban | `member_banned` | After commit | In `execBanFenceTx` after `writeBanTx`, for member and non-member bans |
| Server mute, deafen, undeafen | `member_server_muted`, `member_server_deafened`, `member_server_undeafened` | None | In `applyServerEnforcement` (`voice/handlers.go:1301`) after the outbox enqueue |
| Server unmute | `member_server_unmuted` | None | In `ServerUnmute` after the outbox enqueue |
| Voice move | `voice_member_moved` | Best effort after commit (`auditMoveIfCrossesHierarchy`) | In the grant transaction; for a no-grant move, in the decision transaction before the signal. Which moves are audited is whatever Security#168 leaves. |
| Ownership initiate, cancel, reverse, execute | four existing strings | After commit (`:264`, `:428`, `:584`, `:1553`) | Inside each action's existing transaction |

The four new action strings join the `emitAuditCommitted` list. A test pins every in-scope action string at 50 characters or fewer and every target type at 20 or fewer (`audit_log` column widths, migration 000035).

An audit failure in ownership execute leaves the transfer pending and retryable. It must not mark it failed or cancelled.

`Log` stays for `rbac/handlers.go` only (open call 3). A guard test pins that no other package calls it.

### 4. Lock order for the four new transactions

Timeout set, timeout clear, role label and unban each run in one transaction that takes these locks in this order before the first child-row statement:

1. `rbac.LockServerVisibilityCapture`
2. `loginfamily.AcquireWriterBarrierTx`
3. `lockModerationUsersTx(actor, target)` (sorted, `FOR NO KEY UPDATE`)
4. the `servers` row lock, at least `FOR SHARE`
5. the action's write; zero rows keeps today's 404 and writes no audit row
6. `LogTx`
7. commit, checked

This is I-ORDER and the order `prepareRemovalFenceTx` uses, without its credential guard. The audit insert's foreign-key locks (`KEY SHARE` on `servers` and `users`) land on rows the transaction already holds, which avoids a deadlock against `lockRemovalServerTx` (`servers ... FOR UPDATE`). One shared helper in `members` owns the prefix.

Each route keeps its authorization preflight and response bodies byte for byte. #170 adds no in-transaction re-authorization, credential guard or MFA gate to these routes.

A commit error answers 500 and does not confirm the receipt.

### 5. Reason and duration (VULN-056, VULN-057)

- `TimeoutMemberRequest.Reason` and `BanRequest.Reason` gain `binding:"max=512"` (512 code points). `bindOptionalBody` calls `ShouldBindJSON` and tolerates only `io.EOF`, so binding tags run.
- One shared validator in `members` rejects U+0000 to U+001F except tab, line feed and carriage return, with a fixed 400 message. Nothing else is rejected.
- Timeout validates right after bind and before any database access. Ban validates after `authorizeBan`, so an unauthorized caller still gets 403.
- `duration_seconds` is range-checked as an integer against 60 and 604800 before any conversion. The 400 message is unchanged. The audit metadata records the applied value as a JSON number.

### 6. Invite lookup (VULN-061)

`GetInviteInfo` answers exactly `200 {"valid":false}` when the invite is revoked, expired or exhausted. The 400 for a malformed code and the 404 for an unknown code are unchanged. Lookup, public preview and join share one validity predicate. The plan first confirms that `validateInvite` (`:112`), `publicInvitePreview` and the inline expression in `GetInviteInfo` agree today, and stops if they do not.

Desktop `JoinServerModal.tsx` and `useInvitePreview.ts` already branch on `valid` alone, so no client change is needed. An authenticated caller can still tell an unknown code from a dead one; that leaks no server identity and is pinned by a test as intended (open call 5).

## Non-goals

- No consent-invite feature, no targeted invitations, no block check (D-170a).
- No change to who may time out, mute, deafen, move, ban or kick. Security#168 and #169 own authorization changes.
- No visibility or lift of a departed member's restriction.
- No change to `rbac/handlers.go` audit writes in this PR.
- No client change.

## Left to the builder

- Exact Go names for the lock-prefix helper, the reason validator and the guard tests.
- Whether the capture statement is a package constant shared by both exit sites (recommended, so the guard test compares identical literals).
- Test fixture churn from handlers built with a nil audit writer: fix the fixtures, never restore the skip.

## Decomposition

| # | Task | Files | Depends on | Lane |
|---|---|---|---|---|
| T0 | Rebase after Security#168 merges; re-anchor `voice/handlers.go` and `router.go` | none | Security#168 | gate for T8 |
| T1 | `LogTx`, `AuditReceipt`, deep NUL scrub, length pin | `rbac/audit.go` | none | parallel |
| T2 | Migration 000198 up and guarded down, migration tests | `migrations/`, `migrationrunner` test | none | parallel |
| T3 | Shared reason validator | new file in `members` | none | parallel |
| T4 | Remove direct add and its tests | `members/handlers.go`, `api/router.go` | none | A, first |
| T5 | Lock-prefix helper; timeout set and clear, role label, unban; integer duration bounds | `members/handlers.go` | T1, T3, T4 | A |
| T6 | Kick/leave and ban: capture and `LogTx` | `members/handlers.go` | T1, T2, T5 | A |
| T7 | Restore in `addMemberToServer`, timeout broadcast, lookup gate, shared predicate | `invites/handlers.go` | T2 | B |
| T8 | Mute, unmute, deafen, undeafen audit rows; move audit | `voice/handlers.go` | T0, T1 | C, last |
| T9 | Ownership audit writes into their transactions | `ownership/handlers.go` | T1 | D |
| T10 | Startup wiring check for the audit writer | `api/router.go` | T1 | after A to D |
| T11 | Ported real-PG regressions for the five scan tests; guard tests | tests | T4 to T9 | after |
| T12 | Docs | see "Docs to update" | shapes fixed | finalize last |

Guard tests in T11:
- Every non-test `DELETE FROM server_members` literal is immediately preceded by the capture literal, and the capture literals are identical.
- The only non-test `INSERT INTO server_members` literals are the restore statement and the owner insert at server creation.
- No package other than `rbac` calls `AuditWriter.Log`.
- A kick racing a timeout on the same member completes without a deadlock.

## Acceptance criteria

Every VULN in the issue has an exploit or regression test that fails on the old code and passes on the new code. Each test below names the VULN it proves.

| # | Criterion | VULN | Proof |
|---|---|---|---|
| 1 | `POST /servers/:id/members` is not routed and writes no membership row; shared-server privacy gates still refuse the attacker | 051 | Real-PG integration test (fails on old code: 201 and a row) |
| 2 | A member who leaves, is kicked, or is banned then unbanned while timed out, muted or deafened, and rejoins by invite, is still restricted; an expired timeout is not restored | 052 | Real-PG test per exit path and per restriction |
| 3 | A row with `server_deafened` and not `server_muted` can still leave, and returns with both flags | 052 | Real-PG test |
| 4 | A timeout or ban whose `reason` holds NUL or another rejected control character answers 400 and writes nothing; 512 code points pass and 513 fail | 056 | Handler tests |
| 5 | A forced audit-insert failure rolls back each action in the section 3 table and answers 500 | 055, 056 | One real-PG test per site, asserting the action row is unchanged |
| 6 | Cancelling the request context never leaves an action without its audit row | 055 | Real-PG test per new transaction |
| 7 | A gate refusal or a zero-row write leaves no audit row | 055 | Extends the dangerous-action gate tests |
| 8 | Metadata holding NUL at any depth is stored without it and inserts into a real `jsonb` column | 056 | Real-PG test |
| 9 | `duration_seconds` of 59, 604801 and an overflowing value answer 400; 60 and 604800 pass; the audit row records the applied value | 057 | Boundary tests |
| 10 | `GET /invites/:code` for a revoked, expired or exhausted code returns a body equal to `{"valid":false}`; unknown stays 404 | 061 | Handler tests asserting the whole body |
| 11 | Server mute, unmute, deafen, undeafen and every audited move write one audit row in the action's transaction | D-170f | Real-PG tests |
| 12 | The five scan exploit tests, run unmodified against the fixed tree, fail; the failing assertion for each is recorded | all | Table below |
| 13 | Migration 000198 passes the migration rules, squawk, and an up, down, up cycle; the down refuses while a live tombstone exists | 052 | Migration tests |
| 14 | The focused package suites (`internal/members`, `internal/invites`, `internal/rbac`, `internal/voice`, `internal/ownership`, `internal/api`, `internal/migrationrunner`) pass, and the full control-plane suite (`go test -p 1 ./...` with PostgreSQL and Redis) passes | all | Test run output |
| 15 | Quality gate: 0 new blocker or critical issues, 0 new high-confidence security hotspots, at least 80% coverage on new code | all | `/verify-quality-gate` |
| 16 | Docs to update: every file in the next section is changed in this PR where behaviour changes (`docs/api/openapi.yaml`, `docs/architecture.md` and `CHANGELOG.md` at minimum) | all | Docs review |

Expected result of each scan test (`Concord-Voice/Security` repo, `scans/2026-11-11-self-hosted-vulnhunt/exploit_tests/`):

| Test | Expected failure on the fixed tree |
|---|---|
| `forced_member_add_vuln051_integration_test.go` | The add no longer answers 201 |
| `forced_member_add_vuln051_static_test.go` | `func AddMember not found` |
| `rejoin_vuln052_test.go` | The join insert now mentions `timed_out_until` |
| `selfleave_vuln052_test.go` | `execRemovalFenceTx references timed_out_until`. Its first half (a restricted member may leave) still holds by design. |
| `timeoutnul_vuln056_test.go` | The NUL subtest gets 400, not 200. The clean-reason subtest may fail only because the fake database cannot serve the new lock statements; record that separately. |

Three of them need the scan's `vhFakeDB` overlay, whose JSON points at another worktree and must be re-pathed for the run.

## Docs to update

- `docs/api/openapi.yaml`: remove the `post` operation under `/servers/{id}/members`; document both response shapes for `/invites/{code}`; the `reason` limit and character rule and the integer bounds of `duration_seconds` on the timeout route and the ban route.
- `docs/architecture.md`: the entity diagram, the table list, and the audit sentence (atomic audit).
- `services/control-plane/migrations/README.md`: the range heading and a row for 000198.
- `CHANGELOG.md`: Security entries for VULN-051, 052, 055, 056, 057 and 061; a Removed entry for `POST /servers/:id/members`.
- `.claude/rules/domains/backend.md`: the `LogTx` contract (A1 to A7), the lock prefix, and invariants R1 to R6 with their guard tests.
- `.claude/rules/domains/migrations.md`: migration 000198 and the Key Tables Reference.
- This spec and the plan under `docs/superpowers/plans/`.

## Risks

| Risk | Likelihood / impact | Mitigation |
|---|---|---|
| A new transaction deadlocks against kick, ban, ownership or server deletion | Low / high | Section 4 prefix; kick-versus-timeout race test |
| An audit-table fault now blocks moderation, a self-leave and ownership execute | Low / high | D-170e direction; scrub and length pins remove input-caused failures |
| A future exit or entry path skips capture or restore | Low / high | Two literal guard tests |
| Security#168 reshapes the move path | High / medium | T0 gate; T8 planned last against merged code |
| Handlers built with a nil audit writer now fail closed in tests | High / low | Fix the fixtures |
| Fake-database tests cannot serve the new lock statements | Medium / low | New behaviour is proven on real PostgreSQL |

## Open calls (not ruled on by the lead)

"Accept all" covered D-170a to D-170f. These are architect calls taken in this spec; the lead can overturn any of them.

1. Exit-only tombstones in place of the issue's dual-write wording.
2. Fail-closed audit beyond ban, kick and timeout: timeout clear, role label, unban, self-leave, the four server mute/deafen actions, voice move, and the four ownership writes.
3. The twelve `logAudit` call sites in `rbac/handlers.go` keep the after-commit pattern here and are routed to Security#169, which opens those transactions next and can use `LogTx` from this PR. The #169 decision record needs that added.
4. A guarded down migration.
5. An unknown invite code (404) stays distinguishable from a dead one (200).
6. Group DMs appear to have the same reset class (`dm_participants` carries `server_muted` and `server_deafened`; a participant row is deleted and re-inserted with defaults). Not traced end to end; outside #170 and needs a home (Security#173 is nearest).
7. D-169d speaks of "#170's new add path". There is none; the VULN-053 re-check belongs on `invites.JoinServer`.
8. This PR resolves VULN-051 for Security#58 and makes Security#51 moot. When they close is the lead's call.
9. Desktop reason inputs have no `maxLength`; a 513-character reason now gets 400. A Desktop cap is optional polish.

## Unverified

- Lock strengths inside the ownership transactions and server deletion. Section 4 is safe by construction against the removal and ban transactions.
- Which ownership transaction is execute, and how the executor retries.
- Whether the `member_joined` payload already carries `timed_out_until`.
- Whether a census test requires every presence family to have a call site (affects retiring `FamilyMemberAdd` in place).
- How a Desktop audit viewer renders the four new action strings. Mobile callers were not checked.
- `docs/ci/quality-gate-definition.md` does not exist in this repo; criterion 15 thresholds come from the architect contract.
