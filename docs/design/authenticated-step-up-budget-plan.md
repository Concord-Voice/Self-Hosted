# Authenticated step-up budget implementation plan

Plan for Hosted issue 2271 within Hosted epic 696, governed by the
[design contract](authenticated-step-up-budget.md).

The opaque actor adapter and private SQL accounting core are implemented as dormant
groundwork. They introduce no production caller, numbered production migration,
active budget admission, factor call, retirement change, controller change or
activation. Existing middleware S1 provenance is reused; it is not rebuilt. The
SQL fixture is prospective-schema evidence only. Source availability is not
validation, release or delivery evidence.

## Bounded units and owners

| Unit | Canonical source and scope | Required evidence |
|---|---|---|
| Actor adapter | New `internal/stepup/budget_actor.go` and focused test file | Actual `AuthRequired` provenance, exact value copies, generic refusal and no response side effects |
| Consumer/lifetime inventory | Source-driven sink and routed-owner census | Independently scan proof, token, enrollment and budget sinks; identify first proof, effect commit, continuation lifetime and retry ownership |
| Dormant SQL core | New private step-up accounting engine and disposable schema fixtures; no production callers | Exact five-of-six concurrency, fixed DB window, own-receipt settlement, canonical actor reacquisition and real commit ambiguity |
| Schema and retention integration | Separately allocated paired migration and existing loginfamily retirement owner | Real migration up/down evidence, restrictive FK and complete cohort retention before production rows |
| Terminal composition | Canonical loginfamily access-guarded target capture | Complete actor/target union, immutable UUID order, final actor check and unchanged selector semantics |
| Consumer integration | Authenticated mint and the contract's complete consumer inventory | Reserve before proof, no duplicate charges, definite-effect settlement and ordinary-auth isolation |
| Proof lifecycle | Existing rollout singleton/current-image CLI and all strict readers/controllers | Complete process-generation inventory, whole-work drain, immutable receipt, one-way DB-clock cutover and rollback floor |
| Delivery | Public API/design records, actual Hosted canonical-consumption lane and response consumers | Exact source provenance, client compatibility and controlled activation evidence |

Paths are relative to `services/control-plane/` unless otherwise specified.
No migration number is reserved by this plan. Recheck canonical main and pending
delivery allocations when schema work begins. Reconcile current overlapping
key-reveal, auth, router and lifecycle owners before those later units. Hosted
issue 4201 remains outside this adapter's scope; no repair retry or controller
write follows from it.

## SQL core interface and commit-boundary evidence

Use exactly the private store, reservation, failure symbols and table named in the
[design contract](authenticated-step-up-budget.md#next-dormant-sql-core-contract).
Keep only the borrowed DB on the store. Test fixtures may create the prospective
table and its pure receipt-array predicate on an explicitly disposable database;
they must not create an unallocated numbered production migration. This is schema
fixture evidence only. Retirement, controller, proof-gate, existing Redis guards
and production caller changes remain separate units.

Commit uncertainty needs no production injection seam. Use a test-only per-fixture
`driver.Connector` around `pq.NewConnector`, opened with `sql.OpenDB` on the explicit
disposable URL. Forward real connection/context/BeginTx/query/exec behavior and the
optional interfaces needed by the actual fixture. Wrap the returned real driver
transaction: call its Commit exactly once, and only after a nil result consume a
one-shot atomic latch and return a fixed synthetic lost-result error. Never use or
wrap `driver.ErrBadConn`, which can trigger database/sql retries. No global driver
registration, fake transaction, mutable production hook or environment failpoint.

Set up actors/schema and observe results on an ordinary fixture DB handle. Arm only
the separate operation pool after setup, count BeginTx/Commit/injection attempts,
and bound every operation. For reserve ambiguity, prove the real charge committed
but the core returned zero receipt/unavailable and made no second attempt. For
refund ambiguity, prove only the eligible receipt was removed and the core returned
settlement-unknown. A local proof sentinel is a harness observation, not a claim of
production route coverage. If a test rolls back before returning a Commit error,
fixture knowledge of noncommit must not weaken the core's conservative uncertainty
classification. Test known pre-commit rollback separately.

Independent test authorship and the initial missing-API RED preceded the core's
implementation release. The source implements the frozen private interface; actual
isolated integration/race/lint results remain separate evidence and are not implied
by this design record. No numbered migration is released by the implementation.

## Actor API and acceptance

```go
type BudgetActor struct {
    identity loginfamily.AccessIdentity
    expectedFamilyID uuid.UUID
}

func BudgetActorFromContext(c *gin.Context) (BudgetActor, *Error)
```

The constructor checks nil context, calls `middleware.VerifiedAccessFromContext`,
and copies the whole returned identity and grant's family. Failure returns exactly
a zero actor and generic 500 `{"error":"Verification failed"}` using the existing
step-up error constant. It neither writes/aborts the response nor logs. No raw
context reconstruction, live-authority recheck, new exported carrier setter,
getter, reservation stub, SQL/Redis work or mutable authority capability is allowed.

Tests belong in the stepup package so private copies can be inspected. Use a
local Gin/httptest POST `/api/v1/auth/step-up/password` harness through real
`AuthRequired`, and `RequireVerifiedEmail` when exercising that policy. The test
endpoint only constructs/checks the actor and responds 204 or writes the returned
error. This is an adapter integration harness, not the full production router,
password verifier or token mint.

Create authority using existing public loginfamily APIs on an explicitly isolated
disposable test database: writer barrier, complete user lock, `ForUser`, family
creation, SID binding and definite commit. Retain the independently created
family UUID as the expected value. Use exact UTC whole-second access expiry and
an access-class JWT with the actual issuer, canonical user, bound SID, matching
epoch, exact expiry, fresh JTI and verified-email claim. Test both empty legacy
and valid nonempty credential epochs.

Use an isolated Redis/miniredis fixture for ordinary auth operations, with existing
dependency pins. No production Redis, rollout receipt or proof-gate mutation is
needed. A signed JWT without a persisted bound SID is not a positive control.
Request-context identity helpers alone cannot manufacture the private S1 carrier.

| Vector | Required observation |
|---|---|
| Genuine live middleware admission | 204; exact user, SID, epoch, expiry and independently created nonzero family copied |
| Value-copy isolation | Mutating local actor/accessor copies cannot alter the private carrier or a later constructor result |
| Nil/empty context, raw user, plausible raw claims or request identity only | Zero actor, generic 500, no response write/abort |
| Post-admission user/claims/SID/epoch/expiry/type tampering | Same zero-actor generic refusal; untouched control through the same route succeeds |
| Expired token, unknown/missing SID, terminal family or epoch mismatch | Existing middleware 401 before the adapter endpoint |
| Disabled user | Existing 403/account-disabled outcome before endpoint |
| Redis/auth-fence dependency unavailable | Existing auth 503 before endpoint, no budget flag |
| Invalid/missing bearer | Existing 401 and zero endpoint invocations |

Use endpoint call sentinels to distinguish middleware denial from adapter refusal.
Terminalization after a successful constructor is deliberately a future reservation
guard's responsibility; this value copy cannot prove that race closed. Do not
export a carrier setter merely to forge middleware-private grant corruption.

Required mutation checks replace S1 with raw context, omit individual identity or
family fields, or return a nonzero actor on failure. The positive and negative
controls must detect those changes. Run focused actor tests with isolated fixtures,
race checks, applicable stepup/middleware regressions and repository lint/hooks.
Record the normal test-file inventory/count probe in the implementation handoff;
do not expand this new-file unit into unrelated generated-source changes.

## Later acceptance and sequencing

First freeze the actual source-derived consumer inventory, including password and
MFA checks, raw TOTP matching, WebAuthn assertion/registration finish, token mint/
spend, existing budget consume/charge/clear and wrapper chains. Independently map
each sink to routed authority and its final effect owner. New sinks must fail the
census, rather than comparing two copies of a manually maintained list. Measure
actual predecessor ceremony/control lifetimes before choosing any cutover wait
beyond its fifteen-minute minimum.

Schema and engine work must prove five of six concurrent attempts share one family
allowance across routes, factors, refreshes and two instances. Exercise pool size
one, post-lock database time and unknown reservation commit with zero proof work.
Prove fixed unexpired windows, no whole-key success reset, own-receipt-only removal,
terminal-actor settlement and no refund after partial/grace/rollback/uncertain work.
Keep live empty windows and whole sealed cohorts retained; verify erased-user
maintenance, restrictive FK behavior and paired down refusal.

Review actor-before/after-target UUID order, paged target contention and complete
strongest lock sets. Recheck family/epoch/access expiry at each final effect owner.
Preserve independent logout, selected/preserved target errors, single-use token
rollback and single-attempt credential rotation. Key reveal must release no material
after final guard/read/commit uncertainty. Review migrations and lock ordering
before wiring callers.

Consumer tests must prove both directions of ordinary-auth evidence isolation:
authenticated attempt/success cannot spend/clear login or OAuth email/IP failure
evidence, while ordinary flows keep their defenses. Preserve independent route
and ceremony limits, factor equivalence, purpose/epoch bindings, nonce/CAS,
SID-scoped grace and simple/free-revoke policy. Replace existing behavior only as
part of coordinated activation; never temporarily remove a current guard.

Lifecycle evidence must hold a verifier after its reservation has committed, then
hold the actual effect owner and a tracked detached continuation. Each must prevent
cutover start even when the SQL barrier is free. Verify exact stopped/serving
generation inventories, closed status-v2 and receipt parsers, old-version refusal,
and no restart of incompatible writers. At 14:59 activation refuses; at 15:00 it
may proceed only if every remaining compatibility/drain requirement holds, using
database time. Unknown stopped-process DB work refuses. Preserve the original
family activation receipt and never copy or clear ordinary login evidence.

Require independent correctness/security review, no new blocker/critical findings
or high-confidence security hotspots, and at least 80% new-code coverage. Report
actual focused/race/lint/CI results and operational cost measurements separately;
no throughput, latency or full-outage availability result is implied by this plan.

Canonical Self-Hosted delivery to Hosted and actual supported-consumer compatibility
must be demonstrated before activation. Deployment remains separately governed.
Accounting-only Redis degradation after authenticated authority is distinct from
an unavailable `AuthRequired` veto. The latter's bounded 503 is not availability:
**A7b remains unfulfilled and blocks full issue closure.** This unit references
Hosted issue 2271; it does not close it or authorize rollout.
