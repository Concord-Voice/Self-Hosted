# Authenticated step-up budget

Design contract for Hosted issue 2271, within Hosted epic 696.

**Implementation status:** only the opaque actor adapter in
`services/control-plane/internal/stepup/budget_actor.go` is implemented here.
It has no production caller and performs no accounting. The PostgreSQL budget,
consumer migration, retirement integration and lifecycle controls below are
planned requirements, not active enforcement or deployment evidence. Existing
Redis budgets and authentication defenses remain unchanged.

## Actor provenance

`BudgetActorFromContext(*gin.Context) (BudgetActor, *Error)` is the sole HTTP
constructor. `BudgetActor` retains two private fields: the complete
`loginfamily.AccessIdentity` and the expected family UUID. It has no exported
fields, setters, identity getters, serialization methods or transaction capability.
Actor and receipt identities must remain off the wire and out of telemetry.

The constructor defensively handles nil context, then uses only the existing
`middleware.VerifiedAccessFromContext`. Successful `AuthRequired` admission owns
that private carrier. Its accessor checks consistency with admitted claims and
the exact user, SID, credential epoch, signed access expiry and nonzero family.
An empty credential epoch remains valid legacy state. The adapter copies the
whole identity and returned grant's family by value; it does not reconstruct
authority from `user_id`, JWT strings, headers, cookies or the public
`loginfamily.AccessIdentityFromContext` carrier.

Missing or inconsistent provenance returns a zero actor and the existing generic
500 error body `{"error":"Verification failed"}`. The constructor does not write
or abort the response, log values, emit telemetry, query SQL/Redis, recheck live
authority or consume proof. Earlier middleware 401, disabled-user 403 and auth
dependency 503 remain middleware outcomes.

An actor is a snapshot, not evidence that its authority remains live. Future
reservation must reject a zero actor, reacquire the canonical full access/family
guard, and compare `LiveFamily.Binding()` with the expected user/family/epoch
before charging. Mismatch refuses; it never redirects the charge to another
family. Proof requires the access token itself to remain unexpired; refresh-SID
guard semantics are not a substitute. No permissive reservation stub is provided.

## Planned allowance and reservation

One PostgreSQL row belongs to each server-derived logical login family. It permits
five charged receipts in a fixed fifteen-minute database-clock window. Route,
factor, SID and refresh changes do not reset the allowance. A genuine fresh login
has its distinct family; this is not an account-wide bound across families. No
new user-wide or IP secondary proof budget is introduced.

Reserve once before the first expensive or consuming proof operation, including
password hashing, factor validation, token consumption and enrollment finish. The
standalone READ COMMITTED transaction must:

1. Apply bounded local timeouts and preserve any required outer lock prefix.
2. Acquire the canonical writer barrier before users or families; refuse a
   stopped or incompatible step-up lifecycle before budget or factor work.
3. Lock the complete sorted users set once at its strongest required mode;
   standalone reservation uses the actor with `UserShare`.
4. Guard the full access identity with `FamilyShare` and verify its binding.
5. Insert a missing budget row and use a separate locked SELECT with a fresh
   statement snapshot. Sample database time after acquiring that lock, validate
   stored invariants, reset only an expired window, and append a fresh receipt
   only below the limit of five.
6. Commit and release the connection before proof. Failed or unknown reservation
   commit performs zero proof and is not blindly retried with another receipt.

Reserve/refund deadlines are two seconds, lock timeout one second and statement
timeout two seconds; a shorter request remainder wins. No hashing, Redis or remote
work occurs under budget locks. Do not open an accounting connection inside a
domain transaction. Barrier/capability failures are not permission to reset markers,
reuse constructors, roll back savepoints, add late users or upgrade locks.

Every protected effect reacquires its own canonical authority under the actual
transaction owner, preserving visibility, private-voice and presence prefixes.
No protected-effect transaction locks the budget row. Existing explicit bounded
authority exceptions remain owned by their original code.

## Planned settlement and retention

Full required proof and a definite full protected-effect commit permit removing
only that request's original receipt. Partial or ignored factors, grace-only work,
rollback, cancellation and uncertain commit retain the charge. Success cannot
clear another receipt or reset/extend an unexpired window, even when empty.
Failed settlement leaves the committed effect and charge intact.

The separate `step_up_attempt_budgets` relation has primary key
`(user_id,family_id)` and restrictive foreign key
`(family_id,user_id) REFERENCES login_families(family_id,user_id) ON DELETE RESTRICT`.
It retains a nonnull window UUID, TIMESTAMPTZ start/expiry and unique nonnull UUID
receipts of cardinality 0..5. It contains no route, target or factor dimensions.
Malformed stored state is unavailable. No migration number is allocated here.

Settlement uses a private accounting transaction, locking the exact retained
family at `FOR NO KEY UPDATE`, then its budget. It does not reacquire productive
authority or require the actor to remain live: a committed password change or
family terminalization may settle its own receipt. It checks the original window
and unexpired database deadline. Missing rows/receipt or a different/expired
window are inert; impossible ownership is an integrity failure. This path grants
no SQL authority capability and cannot mutate family identity.

Extend canonical `loginfamily.SweepRetiredEvidence` in place for retirement.
An unexpired budget window, even empty, blocks the family and the entire sealed
auth-operation cohort. Under the existing complete GC locks and fresh eligibility,
delete expired budget rows before dependent families in family-to-budget order,
committing cleanup together. Do not weaken SID/delivery retention, partially shrink
an ineligible cohort, swallow restrictive-FK errors or add a step-up cleanup timer.
Keep the erased-principal maintenance path and existing cohort/page bounds.

## Planned consumers and authority composition

The implementation inventory must follow real routed authority and effect owners,
including wrappers; package names alone do not classify authenticated work.

| Surface | Required future treatment |
|---|---|
| Authenticated password-token mint | Reserve before password verification; settle after definite token persistence. Preserve purpose/P1, no-store and 60-second token semantics. |
| Password/security changes, E2EE replacement, purge-fence OFF, key reveal | One receipt across composed proof; settle at the actual final effect owner. |
| Selected/all/other revokes and revocation-mode changes | Charge proof-bearing operations while preserving simple/free-revoke and independent own-family logout rules. |
| Ownership transfer/reversal | Password and MFA share one request receipt. |
| MFA settings and enrollment | Include TOTP setup verification, email/SMS verification and WebAuthn registration finish before consuming ceremony state or verifying. Preserve TOTP-only backup regeneration. |
| Own-rule, deletion, purge and dangerous-action gates | Outer request owns one receipt; shared verification/token helpers are sinks, not extra reservation owners. |
| Ordinary login/OAuth, registration, recovery and independent admin auth | Remain separate with existing defenses. |

When the new budget becomes active, authenticated proof attempts and successes
must neither consume nor clear ordinary login/OAuth email/IP failure evidence.
Authenticated mint needs a separate narrow password verifier; ordinary login/OAuth
keep their shared verifier, lockouts, feeds and clears. Preserve existing read-only
vetoes and independent route/resource guards; use a distinct namespace for
authenticated IP/user resource limits. Current guards are not removed by this
adapter unit or before their replacement is enforced.

Key reveal must preserve its 403 `invalid_current_password` contract and input
cap when reconciled with its current owner. After reservation and proof, a short
canonical guarded transaction reads wrapped keys under current actor authority;
release material only after definite commit. That confirmed disclosure read is
the protected effect for own-receipt settlement. Failed guard/read or uncertain
commit retains charge and releases no keys. Recent-primary-auth key reads without
new password proof are not charged merely for reading.

Proof-bearing terminal operations need canonical access-guarded target capture:
`ResolveTargetSIDForAccessTx`, `CaptureUserTargetsForAccessTx` and
`CaptureEpochTargetsForAccessTx`. Resolve actor and target membership under the
complete users locks, then lock the deduplicated actor-plus-target family union
in immutable UUID order at `FamilyNoKeyUpdate` from the outset. Revalidate actor
SID, expiry, epoch, family and live state under that complete lock set. Return
only target families; actor-only membership is not a terminal target. Preserve
paged SQL frontier ownership, existing selector errors and one terminalization.
No preliminary actor SHARE followed by an upgrade or appended actor lock is valid.
Epoch rotation remains single-attempt, including failure/commit ambiguity.

## Planned lifecycle and response compatibility

Extend the existing `login_family_rollout` singleton with separate step-up start,
activation and immutable cutover-receipt fields. Preserve the original family
activation timestamp and version-1/protocol-4 receipt exactly. The additional state
is one-way: inactive to paused, then active only after at least fifteen full
database-clock minutes from recorded start. Mixed states and history replacement
refuse. A longer predecessor proof lifetime extends the wait.

The current-image lifecycle owner must stop new proof admissions across every
writer and drain the entire proof, protected-effect and tracked-continuation
lifetime before recording start. A free SQL barrier, handler return or empty pool
does not prove drain. Exclude incompatible/old generations from restart; stopped
processes require conclusive completion/termination of their database work.
Unknown work never becomes zero by timeout. Ordinary login/OAuth and independent
own-family logout continue under their existing policy on compatible processes.

Use the closed `concord.step-up-budget-cutover` version-1 receipt with canonical
`cutoverId`, `familyActivationId`, artifact/inventory SHA256 digests and exact
disjoint stopped/serving writer inventories. Process identities are canonical
boot-generation UUIDs, not reusable PIDs. Stop/drain observations must be bound to
the trusted current-image inventory. This is controller evidence, not cryptographic
host-stop attestation. Retain the accepted receipt unchanged across retries.

Upgrade the existing closed rollout status schema to version 2, retaining every
old field's meaning and adding `stepUpBudget`: state, cutover ID, start/activation
timestamps, proof-admission mode and outstanding proof/effect counts. SQL-only or
nonrunning observations report unknown admission/null counts. Coordinate every
strict reader and controller; old versions and missing/unknown observations cannot
authorize start or activation. No shell SQL writer, environment bypass or new
configuration knob is authorized. Rollback after start requires compatible forward
code; down migration must preserve the pause fence and retained evidence.

Future exhaustion remains 429 with `step_up_budget_exhausted:true` and
`Too many verification attempts`, plus Retry-After equal to ceiling remaining DB
seconds clamped to 1..900. Budget dependency or unknown reservation commit uses
503 with `step_up_budget_unavailable:true` and Retry-After 5. Auth-authority
uncertainty remains the canonical auth 503 without the budget flag; existing
identity/expiry/epoch/terminal 401 and disabled-user 403 retain priority. Remove
ordinary email-lockout 423 only from authenticated mint when separation is active.
Existing target refusal, preservation, count and recovery envelopes remain intact.

## Delivery limits

Self-Hosted owns canonical source; Hosted delivery needs a verified canonical
image-consumption lane or tracked source port with commit provenance. Actual
Desktop/Mobile response consumers need compatibility evidence before activation.
Passing adapter tests establishes neither consumer adoption nor budget enforcement.

**A7b remains unfulfilled:** SQL accounting may survive accounting-only Redis
degradation after authentication authority exists. It cannot bypass `AuthRequired`
blacklist/theft/epoch vetoes during a full outage. A bounded auth 503 proves refusal,
not containment availability. This groundwork references Hosted issue 2271; it
does not close it. No rollout, deployment or repair of Hosted issue 4201 is included.
See the [implementation and evidence plan](authenticated-step-up-budget-plan.md).
