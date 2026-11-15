# Registration verification subject binding — design

**Issue:** Concord-Voice/Security#160

**Parent:** Concord-Voice/Security#151

**Date:** 2026-11-14

**Status:** ready for implementation after Security#157 lands in Self-Hosted

This design was checked against Self-Hosted `origin/main` at `b3b7f490` and the
in-progress Security#157 authentication changes. Line numbers are leads only;
implementation must locate symbols again after rebasing on the merged #157 head.

## 1. Intended outcome

A registration code can create an account only for the normalized email address
to which that exact code was sent. Every spelling of the same UUID uses the same
PostgreSQL row and Redis keys. A concurrent change-email or resend cannot turn a
code for address A into proof for address B, and the full pending-registration
capability never enters logs.

The fix keeps the existing pending-registration table, public request and
response shapes, resend limits, #157 attempt budget, and client flow. It adds no
migration, dependency, feature flag, or new endpoint.

## 2. Problem statement

The registration flow currently splits authority between PostgreSQL and Redis.
PostgreSQL chooses the email promoted with `email_verified = TRUE`, while Redis
holds the code hash and the email that received it. `attemptsGuard` checks only
the hash, then `Promote` later locks and reads the PostgreSQL row without
comparing its email to the verified Redis record. A concurrent email change can
therefore make a valid code for address A promote address B. Separately, the
PostgreSQL UUID input accepts upper-case spelling while Redis keys use the raw
client string, so changing the email with an upper-case ID can leave the old
lower-case code live. The register handler also logs the full `pending_id`, which
is a short-lived bearer capability.

## 3. Predicted impact map

### Surfaces touched

| Surface | Expected files | Responsibility |
|---|---|---|
| Registration handlers | `services/control-plane/internal/auth/handlers.go` | Parse UUIDs, carry verified email proof, order code invalidation, remove capability logging |
| Pending registration repository | `services/control-plane/internal/auth/pending.go` | Return the resend email under lock and compare the accepted email inside `Promote`'s transaction |
| Verification keys | `services/control-plane/internal/auth/verification.go` | Accept `uuid.UUID` and produce one canonical Redis key spelling |
| Test support | `services/control-plane/internal/testhelpers/testserver.go` | Canonical test-only verification-code key lookup |
| Focused tests | `internal/auth/*verification*_test.go`, `handlers_integration_test.go`, `pending_integration_test.go` | Case variants, forced race, cleanup ownership, resend snapshot, log redaction |
| API and design docs | `docs/api/openapi.yaml`, `docs/adr/0046-account-authentication-and-recovery.md`, `CHANGELOG.md` | Document address binding and canonical subject behavior |
| Hosted mirror | matching `services/control-plane/internal/auth`, test helper, docs and changelog in `Concord-Voice/Hosted` | Scheduled source mirror after the Self-Hosted PR merges |

No route is added or removed. No migration, Zustand store, CSS block, E2EE wire
format, or deployment configuration changes.

### Governing rules

- `.claude/rules/domains/backend.md`: parameterized SQL; handler dependency
  injection; check operation, commit, rollback and cleanup errors; never turn an
  unknown write outcome into a success claim.
- `.claude/rules/domains/tests.md`: exercise integration behavior against an
  isolated PostgreSQL database and Redis instance; retain focused failure-path
  assertions.
- `.claude/rules/labels.md`: this remains `risk: security`, `type: bug`,
  `area: control-plane`, and `domain: user-management` work.
- `.claude/rules/release-tracking-epics.md`: keep Security#160 linked to parent
  Security#151 and close it only after both owning and mirror work meet their
  independent gates.
- The workspace `AGENTS.md`: never use private Go caches and never run
  integration tests against the shared `concord` database.

### Phase-8 reviewers

The expected diff triggers `@red-team` through the security label and
`**/auth/**`, `@security-reviewer` always, `@code-reviewer` always,
`@scope-reviewer` always, `@docs-reviewer` through Markdown changes,
`@pr-review-toolkit:pr-test-analyzer` through Go test changes, and
`@pr-review-toolkit:silent-failure-hunter` for the Go diff. No migration,
frontend, IPC, RBAC, E2EE, socket, or deployment reviewer is expected from the
predicted file set unless implementation expands scope.

### Count-bearing surfaces

Go production and test files, API documentation, and tests change. The route and
package counts do not. Phase 4 must still run
`./scripts/update-claude-md-counts.sh` as a probe and commit only a real count
change.

## 4. Current flow and security invariants

The registration code is evidence for one tuple:

```
(canonical pending UUID, normalized recipient email, code generation)
```

PostgreSQL may decide the current pending row and Redis may store the code, but
promotion is the security sink. The sink must prove that the locked row still
names the recipient carried by the accepted Redis record before inserting the
verified user.

The following invariants are load-bearing:

1. Parse every client-supplied `pending_id` with `uuid.Parse` at the HTTP
   boundary. Use `parsed.String()` for every SQL argument and a typed
   `uuid.UUID` for every verification Redis key.
2. A successful code comparison yields an explicit proof containing the
   normalized recipient email, the exact raw Redis record, and the charged
   attempt count. A boolean is insufficient because it drops the subject.
3. `PendingRepo.Promote` compares the normalized expected email with the row
   selected `FOR UPDATE` before any insert. A mismatch creates no user, session,
   or key row.
4. A stale accepted record is removed only if it is still the current Redis
   record. Cleanup must not delete a newer code written by a racing resend or
   change-email request.
5. Stale-code cleanup removes code material and the test-only plaintext key but
   preserves #157's pending-lifetime attempt counter. Only successful promotion,
   explicit abandonment, or terminal pending-row cleanup clears that budget.
6. Change-email invalidates the current code after its row is locked and all
   rejection checks pass, but before the email update is executed. If Redis
   invalidation fails, the SQL transaction rolls back and the address is not
   changed.
7. Resend obtains the recipient from the same row-locked operation that reserves
   the resend count. It never sends to an email captured by an earlier unlocked
   `GetByID`.
8. Logs may name the event and non-capability fields. They never contain a full
   pending UUID.

## 5. Approaches considered

### A. Canonical UUID plus transaction-bound email comparison — chosen

Canonicalize the ID at every boundary, type the key builders on `uuid.UUID`,
return the Redis record's recipient from code verification, and pass it to
`Promote`. Compare against the locked row immediately before insertion. Add an
exact-record compare-and-delete for stale proof cleanup. Keep the code in Redis.

This is the smallest complete fix. It closes the deterministic case split and
the cross-store race at the sink without changing persistence or public APIs.
It composes with #157's pending-lifetime attempt budget.

### B. Move code state into PostgreSQL

Store the code hash, recipient, expiry and attempt count on the pending row so
confirmation and promotion occur in one database transaction. This removes the
two-store boundary by construction, but needs a migration and reimplements the
#157 Redis budget and TTL behavior. It also collides with nearby registration
work for no required user-visible benefit. Rejected for this issue.

### C. Include the email in the Redis key

Use `(pending_id, email)` as the Redis key. An email change makes the old key
unreachable, but the client does not supply the authoritative email at confirm,
resend still needs a locked snapshot, and promotion should still compare the
recipient. It adds key lifecycle complexity without replacing the sink check.
Rejected.

## 6. Detailed design

### 6.1 Canonical pending IDs

`ConfirmRegistration`, `ResendRegistrationCode`,
`ChangeRegistrationEmail`, and `AbandonRegistration` parse the bound string
with `uuid.Parse` once. Invalid input keeps the existing `400` behavior. Each
handler stores both the typed UUID and `pendingID.String()`; SQL receives only
the canonical string.

The internally generated ID returned by `InsertOrTakeover` is parsed before the
first verification-code write. Failure is an internal error and the pending row
is removed through the existing rollback path; a malformed server-generated ID
must never create a noncanonical Redis namespace.

The key functions become:

```go
func redisKey(pendingID uuid.UUID) string
func verificationAttemptsKey(pendingID uuid.UUID) string
func testVerificationCodeKey(pendingID uuid.UUID) string
```

All direct concatenation in auth code, auth tests, and
`testhelpers.FetchVerificationCode` is replaced with these canonical semantics.
The test helper may keep its string API for callers, but it must parse and use
`parsed.String()` before the Redis read.

### 6.2 Carry the proof subject

The attempt reservation Lua script continues to atomically read the code record
and charge #157's attempt counter. `reserveVerificationAttempt` returns the
parsed `verificationRecord`, its exact raw JSON bytes/string, and the charged
count. `attemptsGuard` returns a small internal proof value only after the
constant-time hash comparison succeeds:

```go
type registrationCodeProof struct {
    email    string
    raw      string
    attempts int
}
```

The email is normalized with the same lower-case and trim rule used when the
pending row and verification record are created. No new email canonicalization
policy is introduced.

### 6.3 Bind promotion to the accepted recipient

`PendingRepo.Promote` becomes:

```go
func (r *PendingRepo) Promote(
    ctx context.Context,
    id string,
    expectedEmail string,
) (string, error)
```

After selecting the pending row `FOR UPDATE` and checking expiry, it compares
`strings.ToLower(strings.TrimSpace(p.Email))` with the normalized
`expectedEmail`. On mismatch it returns a new sentinel such as
`ErrVerificationEmailChanged` before inserting into `users`, `user_keys`, or
`public_keys`.

The handler maps this sentinel to the existing generic wrong-code contract:
`401 {"code":"invalid_code","attempts_remaining":N}`. It does not reveal the
new email or whether an email change won the race. It then runs an atomic Redis
compare-and-delete: delete the code record and test-only plaintext only when the
current record exactly equals `proof.raw`. A newer record is left intact. Any
cleanup error is checked, logged without the ID/email/code, and returned as a
generic server failure; promotion already rolled back.

Successful promotion keeps the existing terminal cleanup, including the #157
attempt key. An expired or invalid code keeps existing responses and charge
behavior.

### 6.4 Change-email ordering

The existing row-locked `updateEmail` operation gains one narrowly scoped
pre-update callback. It runs only after expiry, resend-budget and uniqueness
checks pass, while the row lock is held, immediately before the SQL `UPDATE`.
The production handler supplies a callback that deletes current code material
for the canonical UUID while preserving the attempt key.

If invalidation fails, the transaction rolls back. If the subsequent SQL update
or commit fails, the code may have been removed while the old row remains; this
is a safe availability failure and the existing resend flow recovers it. A
cross-store transaction or durable outbox is not added for a ten-minute code.

The callback is internal to the auth package. The exported test/repository
surface retains a no-callback wrapper where existing direct tests need it.

### 6.5 Resend recipient snapshot

`IncrementResend` returns the row email together with the new count and
reservation timestamp. The email is selected under the same `FOR UPDATE` lock
and returned from that transaction. `ResendRegistrationCode` removes its
unlocked `GetByID` read and passes the returned email to `sendInitialCode`.

The promotion email check remains authoritative if a change-email transaction
commits after the resend reservation. No stale-address code can promote the new
address.

### 6.6 Capability logging

Remove the `pending_id` attribute from `Pending registration created`. Do not
replace it with a prefix: a prefix still leaks capability material and no
documented operator workflow needs correlation by this ID. A focused log-capture
test asserts that the complete returned ID is absent.

## 7. Error and compatibility contract

- Existing clients may continue to send upper-case UUID text; it is accepted and
  canonicalized. The API does not require clients to change stored values.
- Existing success payloads and status codes remain unchanged.
- A code that was correct for an earlier email now returns the same `401`
  contract as any invalid code. It consumes the already reserved attempt.
- Redis failure during required pre-update invalidation fails closed and changes
  no email. Redis failure during stale-record cleanup creates no account and
  returns a generic server error.
- No existing pending row or Redis record needs migration. Live records already
  created under the canonical server-issued lower-case UUID remain reachable.
  Noncanonical duplicate keys created by the vulnerable flow expire naturally;
  canonical request handling and the locked email comparison make them unusable.
- Hosted imports the source change only after the Self-Hosted PR merges. Its PR
  receives independent tests, reviews, CI and Phase-9 merge approval.

## 8. Test strategy

1. Unit table: lower-, upper- and mixed-case spellings parse to one UUID and all
   three verification key builders produce exactly one key set.
2. Component test: a matching code returns a proof containing the record email,
   exact raw record and charged attempt count.
3. Repository integration test: `Promote(id, expectedEmail)` refuses a different
   locked row email and inserts no user/key rows.
4. Forced interleaving: accept code A, commit change-email to B before
   `Promote`'s locked read, then assert `401`, no session/user, and no verified B.
5. Cleanup ownership: after accepting old record A, replace it with record B
   before mismatch cleanup; assert B remains and the #157 attempt key remains.
6. Canonical end-to-end test: change-email with upper-case ID, then confirm with
   the old code and lower-case ID; assert refusal and no verified user.
7. Change-email Redis failure: force invalidation failure; assert the SQL email
   and resend state do not change.
8. Resend snapshot: force a concurrent email update around the row lock and
   assert the mail target equals the email returned by the winning locked state.
9. Log capture: register successfully and assert no log event contains the full
   pending ID.
10. Run adapted temporary copies of the VULN-004 and VULN-006 exploit overlays
    when typed helper signatures require compile-only adjustments. The exploit
    assertions must fail for the fixed behavior; a raw compile error is not
    acceptance evidence. Retain positive in-repo regressions.

Integration tests use only a pre-provisioned disposable `DATABASE_URL` and
`REDIS_URL`. They never use shared `concord`, and Go uses the workspace default
caches.

## 9. Acceptance criteria

| Criterion | Required evidence |
|---|---|
| A code sent to A cannot verify B in any change-email, resend and confirm interleaving | Deterministic forced-interleaving integration test |
| UUID case variants address the same SQL row, code record, test-only code and attempt counter | Typed-key unit table plus end-to-end upper-case regression |
| A stale accepted code cannot delete a newer code | Exact-record compare-and-delete race test |
| Change-email cannot commit when current code invalidation fails | Redis-failure transaction test |
| Resend uses the email read under its row lock | Repository/handler concurrency test |
| No full pending ID appears in logs | Captured-log regression and static diff review |
| #157 attempt budgets survive stale-code cleanup and code rotation | Redis key assertions in focused tests |
| Existing client request and response contracts remain compatible | OpenAPI contract test and unchanged schemas/statuses except clarified descriptions |
| Original exploit logic no longer succeeds | Adapted overlay evidence plus retained positive regressions |
| 0 new blocker/critical issues | Exact-head quality and review reports |
| 0 new high-confidence security hotspots | Exact-head security scan/review report |
| At least 80% new-code coverage | Exact-head quality-gate coverage report |
| Docs ship with code | Update ADR 0046, OpenAPI registration descriptions and changelog in the source PR; mirror them with the Hosted import |
| Both repositories pass their own lifecycle gates | Separate Self-Hosted and Hosted PR evidence and separate Phase-9 approval |

## 10. Dependencies and rollout

Security#157 is a hard source dependency because it changes the registration
attempt-key lifetime, code-material cleanup, resend reservation, and the same
handler tests. Rebase this implementation after #157 merges; do not copy its
work piecemeal.

Self-Hosted is the source of truth. Land and validate its PR first. The scheduled
Hosted mirror then imports the exact source commit, resolves repository drift
without redesigning the invariant, and runs its own quality/review/merge gates.
Security#160 closes only after both are complete.

Related Self-Hosted #116 (email-optional registration) and #110 (MFA at sign-up)
touch adjacent paths. Their future work must preserve the tuple binding in
section 4; neither is required to implement this fix.

## 11. Risks and mitigations

| Risk | Likelihood | Mitigation |
|---|---:|---|
| A newer code is deleted by stale mismatch cleanup | Medium without fencing | Exact raw-record compare-and-delete; race regression |
| Email changes but code invalidation failed | Medium without fail-closed ordering | Invalidate under the locked update before SQL mutation; rollback on Redis error |
| #157 attempt budget is accidentally reset | Medium | Code-only cleanup helpers; explicit attempt-key assertions |
| A handler or test helper keeps raw string key construction | Medium | Typed UUID key builders, caller census, case table |
| Error response leaks that an email changed | Low | Map mismatch to existing generic `invalid_code` response |
| Commit failure removes a still-valid code | Low | Safe failure; resend recovers; no verified state is claimed |
| Self-Hosted and Hosted drift | Medium | Source-first reviewed mirror with independent exact-head gates |
| Adjacent registration work overwrites the invariant | Medium | Dependency notes in ADR/spec and specialist review triggers |

## 12. Explicit non-goals

- Moving verification codes or attempt counters to PostgreSQL.
- Changing email normalization semantics beyond existing lower-case and trim.
- Redesigning registration, adding MFA-at-sign-up, or implementing optional
  email registration.
- Making cross-store code delivery perfectly available under deliberately
  concurrent resend/change-email requests. The security sink remains correct;
  ordinary retry recovers availability.
- Adding new correlation identifiers, telemetry fields, feature flags, or
  cleanup jobs.
