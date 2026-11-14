# Security #158: Fresh step-up for destructive account and session actions

**Issue:** [Concord-Voice/Security#158](https://github.com/Concord-Voice/Security/issues/158)

**Date:** 2026-11-14

**Status:** approved for planning

**Canonical source:** Concord-Voice/Self-Hosted

This design is the Phase-2 reconciliation of the architect, system-engineer,
UI/UX/frontend, and live-repository findings. It closes VULN-038, VULN-027,
VULN-036, VULN-035, VULN-011, VULN-013, VULN-014, and VULN-049 without adding a
schema migration, NATS contract, environment variable, or second step-up
framework.

## 1. Intended outcome

A bearer token, a password alone on an MFA account, or a step-up completed by a
different login family is insufficient to erase an account, remove an
authenticator, weaken SSO authentication, unlink an SSO identity, confirm an
ownership transfer, or revoke another session.

Every proof-bearing action uses the existing `internal/stepup` machinery. The
password and, when enrolled, the second factor are checked against the actor
state locked by the same transaction that owns the protected mutation. MFA-code
attempts are charged before that transaction through Security #157's shared
budget. A successful proof clears its charge only after the protected effect has
definitely committed. Rollback and commit uncertainty retain the charge.

Account erasure also protects other principals' data: the user must explicitly
acknowledge the exact current set of owned servers that still contain other
members. An unverified email adds a one-use emailed confirmation, but that link
never replaces password/MFA and never deletes the account by itself.

## 2. Rulings and resolved conflicts

| Conflict | Ruling | Consequence |
|---|---|---|
| Defer the account-erasure UI to Desktop #661 | **Rejected.** Live `Concord-Voice/Desktop#661` does not exist, and `Concord-Voice/Hosted#661` is a closed, unrelated telemetry issue. The comment in `desktop/src/renderer/api/privacy.ts` is stale. | The Desktop erasure danger-zone flow is required #158 scope. Backend hardening may land first, but #158 cannot close until the companion ships. |
| Let the emailed link authorize erasure for an unverified address | **Rejected.** An unverified address is not an authenticator. | The link confirms intent only. Final erasure still requires a new password/MFA proof inside the deletion transaction. It does not mark the email verified. |
| Verify step-up in middleware or a preflight transaction | **Rejected.** | The route reserves external attempt state before `BeginTx`; the mutation owner verifies and consumes proof using its already-locked actor in the effect transaction. |
| Call Redis while holding SQL locks | **Rejected.** | Security #157's `stepup.Charge` call, password-attempt reservation, email-confirmation claim, and free-revoke reservation all happen before the SQL transaction. |
| Add an opaque new budget abstraction in #158 | **Rejected.** | Reuse #157's chosen call-site `stepup.Charge` model and caller census. Do not double-charge routes #157 already covers. |
| Give every route a reusable “dangerous action” purpose | **Rejected.** | Add one closed purpose per consumer route. The new purposes are never grace eligible. |
| Implement the free-revoke race in both #37 and #158 | **Rejected.** Security #37 remains the implementation owner for VULN-013. | #158 depends on the one #37 atomic reservation and carries the cross-finding acceptance test. If delivered in the same train, the code lands once and both issues reference it. |
| Invent an ownership-transfer UI because none was located | **Rejected.** | Harden the server and add the typed Desktop purpose/API contract. A census test records that there is currently no renderer mutation callsite; a future surface must use the contract. |

## 3. Scope, ownership, and dependencies

### 3.1 Repositories

- **Self-Hosted** owns canonical control-plane code, OpenAPI, ADR/design updates,
  tests, and the implementation changelog.
- **Desktop** owns user prompts, the account-erasure consequence review, the
  custom-protocol email-confirmation handoff, and API adapters.
- **Hosted** receives `services/**` and `docs/api/**` only through the scheduled,
  reviewed upstream mirror. Hosted-specific changelog or non-imported docs are
  updated in its mirror PR.

The Self-Hosted admin console is not the user client and receives no erasure UI.
Mobile has no confirmed callsite in this issue and is not a substitute for the
required Desktop companion.

### 3.2 Required predecessor contracts

**Security #157 — account attempt budgets.** #158 consumes #157's chosen
contract:

```go
stepup.Charge(ctx, stepup.DangerousActionBudget(redis), userID, mfaCode)
```

The outer request owner charges a non-empty submitted MFA code once, before
`BeginTx`. A missing code is not charged. Exhaustion is `429`; unavailable Redis
is `503`. A definite successful protected effect clears the budget after commit.
Wrong proof, rollback, cancellation, and unknown commit retain it. Direct
`VerifyCode`/`VerifyCodeTx` callers remain covered by #157's production census.

**Security #37 — atomic free revoke.** #37 owns the Lua reservation that replaces
the check-then-act free-revoke counter. #158 must not add a second counter or a
parallel policy. Its session acceptance suite runs against #37's merged
primitive.

**Security #118 — overlap only.** #118 partially overlaps factor removal, SSO
unlink/trust, and session revocation. #158 is the concrete vulnerability owner.
After #158 merges, #118 is rescaled against its remaining acceptance criteria;
the same route must not acquire a second proof gate.

## 4. Common step-up contract

### 4.1 Closed purposes

Add the following values to `internal/stepup.Purpose`, `allPurposes`, the
OpenAPI enum, the closed-set tests, and the Desktop mirror. None belongs to
`GraceEligiblePurposes` or the password-token `OwnRulePurposes` set.

| Route | Purpose |
|---|---|
| `POST /api/v1/privacy/erase-account` | `privacy.account_erase` |
| `DELETE /api/v1/mfa/webauthn/credentials/:id` | `mfa_settings.webauthn_delete` |
| `POST /api/v1/mfa/totp/disable` | existing `mfa_settings.totp_disable` |
| `DELETE /api/v1/users/me/sso-identities/:provider` | `account.sso_unlink` |
| `PATCH /api/v1/users/me/security` when `trust_sso_security` changes `false -> true` | `account.sso_trust_enable` |
| `POST /api/v1/servers/:id/transfer-ownership/confirm` | `ownership.transfer_confirm` |

One inline token is bound to one user, credential epoch, JTI, and purpose. It is
one-use. A token for a neighbouring route is indistinguishable from invalid
proof and is not consumed by the wrong route.

### 4.2 Request fields

Routes retain their existing field spellings and add the shared step-up fields
at the top level:

```json
{
  "password": "current account password",
  "mfa_code": "TOTP, backup code, or purpose-bound WebAuthn inline token"
}
```

`mfa_code` is the existing polymorphic second-factor field: the MFA verifier
distinguishes a typed TOTP/backup code from the 43-character purpose-bound
WebAuthn inline token. `step_up_token` is the existing password-token field for
own-rule routes and is **not** an alternative second factor for these actions;
new #158 consumers reject it. Route-specific request structs reuse
`stepup.Fields.Input`'s size/shape rules without reading a Gin body twice.
`TOTPDisable` preserves its existing `code` field instead of silently renaming
it. `PATCH /users/me/security` preserves `current_passphrase` instead of adding
a second password spelling.

All proof-bearing bodies use the existing body cap and strict JSON shape. Raw
passwords, factor codes, inline tokens, email-confirmation tokens, and Redis key
suffixes must never enter logs, metrics, security events, or error text.

### 4.3 Response contract

| Result | HTTP | Required wire contract |
|---|---:|---|
| Required proof absent | 403 | `{"error_code":"step_up_required"}` plus the existing safe method hints |
| Password/factor invalid | 403 | Existing generic invalid-proof response; do not reveal which leg failed |
| Password lockout | 423 | Existing `stepup.LockedError` response |
| Shared MFA budget exhausted | 429 | `step_up_budget_exhausted: true` and `Retry-After` from #157 |
| Shared MFA budget unavailable | 503 | `step_up_budget_unavailable: true`; no invalid-code claim |
| Actor/session/epoch stale | 401 | Existing authentication-required response |
| Protected state changed | 409 | Route-specific stable `error_code`; no partial mutation |
| Definite commit | Existing success status | Post-commit effects may run; budget may clear |
| Commit outcome unknown | Existing fail-closed 5xx | Do not retry proof/effect automatically and do not clear budget |

### 4.4 Transaction pattern

Every route follows this order:

1. Parse and bound the request.
2. Capture the authenticated `loginfamily.AccessIdentity` and expected
   credential epoch from middleware.
3. Reserve the password guess with `stepup.ReserveUserPasswordAttempt` when a
   password is supplied.
4. Charge a submitted MFA code once through #157's
   `stepup.DangerousActionBudget`.
5. Perform any route-specific Redis claim needed before SQL.
6. Open the route owner's transaction and take its existing canonical locks.
7. Recheck actor family, session liveness, account state, and credential epoch.
8. Verify the reserved password against the locked subject.
9. Re-read locked factor enrollment; when MFA is enrolled, verify and consume
   the exact-purpose second-factor proof inside this transaction.
10. Revalidate route-specific authority and perform the mutation.
11. Commit.
12. Settle the password attempt. Clear the shared MFA budget only after a
    definite successful protected effect. Run notifications, socket fencing,
    authenticator cleanup, and other effects after commit.

No helper may call `LockSubjectTx` a second time after the mutation owner has
already locked the actor. Helpers accept the transaction and the locked
`stepup.Subject`/authority value they need.

## 5. Account erasure

### 5.1 Request and responses

`POST /api/v1/privacy/erase-account` changes from an effectively empty body to:

```json
{
  "password": "required",
  "mfa_code": "required when locked state has MFA; accepts the purpose-bound WebAuthn inline token",
  "confirmed_owned_server_ids": [
    "canonical-lowercase-uuid"
  ],
  "email_confirmation": {
    "request_id": "uuid",
    "token": "43-character base64url secret"
  }
}
```

`confirmed_owned_server_ids` is required and may be empty. Reject malformed
UUIDs, duplicates, non-canonical encodings, and unsorted input with `400
invalid_request`; the Desktop sends the canonical lexicographically sorted
form. `email_confirmation` is omitted for verified-email accounts and for the
first unverified-email preparation request.

| Outcome | HTTP | Body |
|---|---:|---|
| Account erased | 204 | Empty |
| Unverified email; confirmation issued | 202 | `{"status":"email_confirmation_required","request_id":"<uuid>","expires_in":900}` |
| Protected owned-server set differs | 409 | `{"error_code":"owned_server_confirmation_stale","required_owned_server_ids":["..."]}` |
| Email confirmation missing, expired, replayed, wrong-session, wrong-epoch, or wrong-set | 409 | `{"error_code":"email_confirmation_stale"}` |
| Proof failures | 401/403/423/429/503 | Common contract in §4.3 |

The `409` server-ID list is authenticated data about servers the caller owns.
It is sorted and contains IDs only; the Desktop resolves display names from its
current server inventory.

The legacy `clientId` field remains ignored during the compatibility window but
never contributes authority. The obsolete Desktop API signature is replaced;
new code must not send it.

### 5.2 Protected owned-server set

The protected set is exactly every server currently owned by the actor that has
at least one other member. A server with no other members requires no explicit
acknowledgement. The user may remove a server from the protected set only by
transferring ownership or removing the other members before retrying.

Inside `deleteAccountTx`, after the existing actor/user authority is locked:

1. Lock the actor's owned-server rows in canonical server UUID order.
2. Compute the protected set from locked server/member state.
3. Canonicalize the request set and exact-compare it.
4. Missing, extra, duplicate, or changed IDs return
   `ErrErasureOwnedServerSetDrifted`; the transaction deletes nothing.

The comparison is not a non-locking preflight. A UI preview may be stale; the
transactional exact comparison is decisive.

### 5.3 Lock ordering and proof placement

Preserve the current erasure prefix:

1. private-voice scopes for the actor and bounded survivor candidates;
2. login-family writer barrier;
3. complete users set in canonical UUID order;
4. `guardErasureActorTx` on the already-locked actor family;
5. actor-owned server rows in canonical UUID order;
6. proof verification against the already-locked actor;
7. protected-set comparison;
8. existing channel/server cleanup, billing erasure capture, seat capture, user
   delete, and privacy-safe audit insert.

The AccountService, not the Gin handler, owns the proof and exact-set checks.
The handler cannot call the old proofless `DeleteAccount(ctx, userID)` entry
point. Replace it with a closed request type that requires the authenticated
actor snapshot, reserved password attempt, step-up input, purpose, and
confirmation set.

Post-commit family close, presence/activity cleanup, seat notifications, and
other obligations retain their existing confirmed/unknown-commit semantics.

### 5.4 Unverified-email confirmation protocol

An unverified address is an intent channel, not a factor. The protocol is:

1. The Desktop submits the normal erasure body without `email_confirmation`.
2. The server verifies fresh `privacy.account_erase` proof and the current
   protected-server set. It does not delete.
3. After that proof transaction definitely commits, generate a 32-byte random
   secret and request UUID. Store only the SHA-256 digest in Redis at
   `account_erase_email:<request-id>` for exactly 15 minutes. The bounded record
   contains user ID, initiating family ID, credential epoch, canonical-set
   digest, expiry, and state `pending`.
4. Email a custom-protocol link:
   `concord://account-erasure/confirm/<request-id>/<base64url-secret>`.
   The raw secret is present only in the email and transient Desktop memory.
5. Return the `202` body above. The response never contains the secret.
6. Desktop main validates the closed link shape, UUID, and 43-character
   base64url secret, then delivers it over a dedicated main-to-renderer IPC
   channel. It is not persisted in storage, Zustand, analytics, crash reports,
   clipboard, or logs.
7. The renderer shows “Email confirmed; review and erase” but performs no
   automatic mutation. The user submits a new fresh password/MFA proof with the
   exact `email_confirmation` object.
8. Before SQL, a Redis Lua claim constant-time compares the token digest and all
   request bindings, then changes `pending -> claimed:<claim-id>`. Missing,
   expired, mismatched, or already-used state is `409 email_confirmation_stale`.
9. A definite SQL rollback may release only that claim back to `pending` while
   the original TTL remains. A definite erasure commit deletes the record. An
   unknown commit leaves it claimed until expiry and never retries erasure.

Issuing the email and final erasure are separate protected effects, so an
unverified-email account performs two fresh step-ups. A successfully issued
pending confirmation may clear that request's #157 budget only after both the
Redis record and email handoff succeed. The final request charges independently
and clears only after definite erasure commit.

The link does not change `email_verified`, cannot be used from another login
family, and cannot be reused after a credential-epoch change or server-set
change. Old Desktop builds that cannot receive the closed deep link fail safely
at `202`; they cannot fall back to bearer-only erasure.

## 6. Factor removal

Create one transactional policy helper, conceptually:

```go
func requireFactorRemovalStepUpTx(
    ctx context.Context,
    tx *sql.Tx,
    userID string,
    subject stepup.Subject,
    attempt *stepup.PasswordAttempt,
    password string,
    mfaCode string,
    purpose stepup.Purpose,
) *stepup.Error
```

It does not acquire locks and does not call Redis. It verifies the reserved
password against `subject`, reads current enrollment in the caller's
transaction, and requires a purpose-bound second factor when the locked account
has MFA. TOTP and backup-code verification/consumption use the transactional
verifier. WebAuthn uses a one-use inline token minted for the exact route.

### 6.1 TOTP disable

Keep the existing `password` and `code` request fields and
`mfa_settings.totp_disable` purpose. #157 charges `code` once before `BeginTx`.
Under the existing users-row `FOR NO KEY UPDATE` lock, verify password and the
second factor, delete TOTP, enforce the last-inline-factor policy, synchronize
MFA flags, and commit. A proof or flag-sync failure rolls back the delete.

### 6.2 WebAuthn credential delete

Extend the DELETE body:

```json
{
  "password": "required",
  "mfa_code": "TOTP, backup code, or WebAuthn inline token for mfa_settings.webauthn_delete"
}
```

Reserve/charge before the transaction. Lock the users row first, verify the
proof, delete only the named credential owned by the actor, enforce the
last-inline-factor policy, synchronize flags, and commit. The authenticator
cleanup signal and permission invalidation remain post-commit. A credential may
prove its own removal, but the one-use token cannot remove a second credential.

Password-only factor removal is allowed only when the locked account state has
no enrolled MFA factor that requires a second leg. Enrollment or removal racing
the request is resolved by the state read under the actor lock.

## 7. SSO security mutations

### 7.1 Unlink identity

`DELETE /api/v1/users/me/sso-identities/:provider` accepts:

```json
{
  "password": "required",
  "mfa_code": "required when MFA is enrolled; accepts the purpose-bound WebAuthn inline token"
}
```

Use purpose `account.sso_unlink`. In one transaction, lock the actor, recheck
epoch/account state, verify proof, count current login methods, preserve the
existing `would_lock_out` refusal, and delete the provider identity. Proof
failure and lockout refusal mutate nothing.

### 7.2 Enable SSO trust

`PATCH /api/v1/users/me/security` keeps `current_passphrase` and adds
`mfa_code`. Only the transition
`trust_sso_security: false -> true` uses purpose
`account.sso_trust_enable` and requires password plus MFA when enrolled.
Disabling the flag strengthens authentication and remains ungated. A request
that changes multiple security fields validates every applicable invariant in
one actor-locked transaction; no field may commit on partial proof.

## 8. Logout and session revocation

### 8.1 Own-family logout

For `POST /api/v1/auth/logout`, the authenticated bearer family is the only
family a bearer-authorized request may terminate. If `X-Session-ID` resolves to
a different family, return `403 {"error_code":"invalid_logout_target"}` and do
none of the following: revoke rows, clear cookies, blacklist the bearer, fence
sockets, remove attestation state, or emit a success event.

Ending another session remains exclusively
`DELETE /api/v1/sessions/:id`, whose policy can require fresh step-up.

### 8.2 Atomic free-revoke allowance

Replace `countRevokesInWindow` followed by `recordRevoke` with #37's single Lua
reservation. The authority key is:

```text
session_revokes:<user-id>:<authenticating-family-id>
```

The sorted set contains unique reservation IDs scored by server time. One script
prunes entries older than 24 hours, returns `proof_required` when the configured
allowance is full, or inserts the caller's reservation and applies the bounded
TTL. The actor family comes from verified access identity, never a request
header. Missing family provenance or Redis failure requires step-up.

A definite SQL success keeps the reservation. A definite non-commit removes
only the request's reservation. Unknown commit keeps it. Concurrent retries and
duplicate target session IDs cannot grant more free effects than the allowance.

### 8.3 Per-family simple-mode window

Change the key to:

```text
session_revoke_auth:<user-id>:<authenticating-family-id>
```

The same verified family must earn and consume the window. Session B cannot use
session A's grant. The target session ID does not choose the key.

## 9. Ownership-transfer confirmation

`POST /api/v1/servers/:id/transfer-ownership/confirm` accepts:

```json
{
  "password": "required",
  "mfa_code": "required when MFA is enrolled; accepts the purpose-bound WebAuthn inline token"
}
```

Purpose `ownership.transfer_confirm` is not grace eligible. The handler may do
shape validation, but the existing ownership coordinator transaction is the
decisive owner. Under its existing sorted server/user lock prefix it must:

1. lock and revalidate the pending transfer;
2. prove the authenticated confirming actor is the current actor allowed to
   confirm;
3. recheck the actor family, credential epoch, and account state;
4. verify password plus MFA when enrolled against the already-locked actor;
5. recheck source/target membership and current ownership;
6. perform transfer, key cleanup, and pending-row terminalization atomically.

The initiation-time step-up is not confirmation authority. A non-locking
`requireServerOwner` preflight may remain only as an early refusal; it cannot
authorize the transaction. No pending-transfer column or migration is needed.

## 10. Desktop companion

The Desktop change is required for issue completion and ships before or in the
same release train as server enforcement.

### 10.1 Existing security surfaces

- `SecuritySection.tsx`: `handleRevokeWebAuthnKey` sends the collected MFA code
  or inline token instead of discarding it. Enabling `trust_sso_security` uses
  `SessionStepUpDialog`; local state changes only after server success.
- `LinkedAccountsList.tsx`: Unlink opens the step-up dialog before the DELETE.
  Preserve `would_lock_out` copy and refetch identities only after success.
- Reuse `submitMfaStepUp` and the existing request-context fence. Do not add a
  Zustand store for transient proof.

### 10.2 Account-erasure danger zone

Add an account-erasure section to the existing Privacy settings surface. It:

1. explains that erasure is irreversible and may remove owned servers and DM
   conversations affecting other people;
2. lists every currently known multi-member server the user owns;
3. leaves every acknowledgement unchecked by default and provides transfer
   guidance;
4. derives the canonical sorted ID set from individually checked rows;
5. collects password/MFA through the existing step-up dialog;
6. handles `202` by waiting for the email deep link without retaining the
   password or MFA proof;
7. receives the request ID/token through the dedicated deep-link IPC, shows the
   confirmation state, and requires a second explicit Erase action with a fresh
   proof;
8. handles `409 owned_server_confirmation_stale` by refreshing inventory and
   clearing every acknowledgement;
9. clears credentials, account-scoped stores, pending deep links, and exits the
   authenticated surface only after `204`.

The raw email token lives only in component memory. Cancel, account switch,
logout, renderer reload, reset, or expiry drops it.

### 10.3 Closed deep-link transport

Extend the existing `concord:` parser with exactly:

```text
concord://account-erasure/confirm/<uuid>/<43-char-base64url>
```

Reject extra segments, query/fragment data, noncanonical UUIDs, wrong token
length/alphabet, and every other host/path combination. Queue at most the
existing bounded deep-link capacity. Deliver through a dedicated
`deeplink:account-erasure-confirmation` main-to-preload-to-renderer channel with
the existing trusted-frame and session-generation fences. Never route the
secret through generic invite/friend-code handlers.

Update the IPC contract version and tests. Logs record only the fixed outcome
class (`accepted`, `invalid_shape`, `stale_session`), never the URL, request ID,
or token.

### 10.4 Failure states and accessibility

- `403`: keep the dialog open with generic verification text.
- `409`: refresh authoritative state; do not preserve stale destructive
  acknowledgements.
- `429`: disable submit until `Retry-After` elapses.
- `503`: preserve nonsensitive form state and offer retry; clear passwords,
  codes, and tokens according to existing secret-input behavior.
- Success: refresh server-authoritative state and restore focus, except erasure,
  which ends the session.

Use native buttons, visible focus, keyboard-operable cancel/escape, focus return,
`role="alert"` for refusals, and text rather than color alone. Reuse existing
danger, modal, disabled, and focus tokens; add no 32-theme token matrix.

### 10.5 Ownership-transfer compatibility

Add `ownership.transfer_confirm` to the typed purpose union and any typed API
adapter. A repository census must prove whether a renderer mutation callsite
exists. If none exists, record zero callsites and add no speculative component;
the hardened API remains authoritative for every caller.

## 11. Files and decomposition

Line numbers are leads, not anchors. Locate symbols before editing.

### 11.1 Self-Hosted canonical changes

| Unit | Likely files | Responsibility |
|---|---|---|
| Closed purposes and common proof | `internal/stepup/purpose.go`, purpose/OpenAPI/census tests | Add exact purposes; keep them out of grace/own-rule sets; integrate #157 budget classification. |
| Erasure HTTP/email handoff | `internal/privacy/handler.go`, focused confirmation helper/tests | Bind the new body, issue/claim the Redis email confirmation, map stable responses. |
| Erasure transaction | `internal/users/delete_account.go`, focused erasure tests | Closed request type, locked proof, owned-server exact set, unchanged post-commit obligations. |
| Factor removal | `internal/mfa/handlers.go` or a focused `factor_removal_stepup.go`, tests | Shared transactional helper for TOTP and WebAuthn removal. |
| SSO security | `internal/users/handlers.go`, tests | Transactional unlink and trust-enable proof. |
| Own-family logout | `internal/auth/handlers.go`, E2E tests | Reject cross-family header targets without side effects. |
| Session revoke | `internal/sessions/handlers.go`, #37 helper/tests | Atomic per-family allowance and per-family simple window. |
| Ownership | `internal/ownership/handlers.go`, tests | Fresh confirmation proof in coordinator transaction. |
| API/docs | `docs/api/openapi.yaml`, ADR-0046, `docs/design/authenticated-step-up-budget.md`, `CHANGELOG.md` | Wire contract, policy/caller census, release note. |

### 11.2 Desktop companion changes

| Unit | Likely files | Responsibility |
|---|---|---|
| Purposes/API | `desktop/src/renderer/components/Auth/stepUpPurpose.ts`, `desktop/src/renderer/api/privacy.ts` | Exact enums and typed request/response unions. |
| Existing settings | `SecuritySection.tsx`, `LinkedAccountsList.tsx`, focused tests | Send proof and preserve authoritative UI behavior. |
| Erasure UI | `PrivacySection.tsx` plus focused component/styles/tests | Consequence review, per-server acknowledgement, email-wait/finalize states. |
| Deep link | `desktop/src/main/deepLink.ts`, main/preload wiring, `ipcContract.ts`, tests | Closed parsing and transient delivery. |
| Release docs | Desktop changelog | User-visible security hardening. |

### 11.3 Work ordering

| Task | Ordering |
|---|---|
| A. Merge or import #157 budget contract; merge #37 atomic reservation | Serial prerequisites for their consumers |
| B. Closed purposes, OpenAPI enum, Desktop purpose map | First shared contract |
| C. Factor removal and SSO mutations | Parallel after B |
| D. Logout and session revoke | Parallel after A |
| E. Ownership confirmation | Parallel after A/B |
| F. Erasure transactional core and email-confirmation state | Serial within erasure; parallel with C-E |
| G. Desktop existing settings and erasure/deep-link companion | Starts after B/API contract; must finish before issue closure |
| H. Docs, caller census, exploit regressions, quality gates | After implementation surfaces stabilize |
| I. Hosted mirror | Only after canonical Self-Hosted merge and green gate |

## 12. Test strategy

### 12.1 Required exploit regressions

Retain inverted regressions for:

- `erase_account_no_stepup_vuln038_test.go`;
- `webauthn_delete_vuln027_test.go`;
- `logout_xsession_vuln011_test.go`;
- `logout_xsession_e2e_vuln011_test.go` against a disposable
  `DATABASE_URL`/`REDIS_URL`.

Never run destructive integration tests against the shared `concord` database.

### 12.2 Backend matrices

Each proof-bearing route covers: no proof, password only with MFA enrolled,
wrong password, wrong code, valid TOTP, valid backup code, valid WebAuthn inline
token, wrong-purpose token, replayed token, exhausted budget, Redis unavailable,
epoch change while waiting, rollback, definite commit, and unknown commit.

Additional suites:

- **Erasure:** exact/empty/missing/extra/duplicate/unsorted server sets; member or
  ownership drift; transfer racing delete; no rows deleted on every refusal;
  existing private-voice/login-family lock-order tests remain green.
- **Email confirmation:** verified-email bypass; initial unverified `202`;
  token digest only in Redis; wrong family/epoch/set/token; expiry; replay;
  known rollback release; unknown commit retained; link never marks email
  verified and never deletes by itself.
- **Factor removal:** factor enrolled between preflight and lock; factor removed
  concurrently; proof by the credential being removed; last-inline-factor
  refusal; sync failure rollback; post-commit cleanup only.
- **SSO:** `would_lock_out`; trust enable versus disable; multi-field PATCH
  atomicity; identity uniqueness and state drift.
- **Logout:** foreign `X-Session-ID` leaves refresh row, cookie, blacklist,
  sockets, attestation, and event stream unchanged.
- **Free revoke:** `N` parallel requests admit at most the configured free
  allowance; known failure releases exact reservation; unknown commit retains;
  Redis outage requires proof.
- **Simple mode:** a grant earned by family A is unusable by family B.
- **Ownership:** initiation proof cannot confirm; actor/owner/member/epoch
  changes under lock refuse without partial transfer.
- **Census:** every production closed-purpose consumer reaches the shared helper;
  every direct MFA verifier call is charged or explicitly exempt under #157;
  Desktop has no unclassified dangerous mutation callsite.

### 12.3 Desktop matrices

- Existing WebAuthn, SSO unlink, and trust flows send exact purpose/proof fields
  and make no optimistic state change.
- Erasure acknowledgements start unchecked, clear on `409`, and cannot submit a
  stale set.
- `202` clears the first proof and enters email-wait state; the deep link arms
  but never automatically submits the final request.
- Deep-link parser rejects every near miss and never logs/persists secrets.
- Account switch, logout, reset, renderer reload, and expiry discard pending
  erasure material.
- `403`, `409`, `429`, `503`, focus return, keyboard operation, and alert
  semantics are covered.

## 13. Acceptance mapping

| Security #158 criterion | Required proof |
|---|---|
| Bearer-only erasure is rejected | `403 step_up_required`; no user/server/DM/session/audit mutation |
| Multi-member owned servers are protected | Exact locked set; unconfirmed or stale set returns `409` and deletes nothing |
| Unverified-email erasure is not bearer-only | One-use family/epoch/set-bound email confirmation plus a second fresh transactional step-up |
| WebAuthn delete rejects password only on MFA accounts | Transaction test and inverted VULN-027 exploit |
| TOTP and WebAuthn removal share one policy | Caller census and both route matrices |
| SSO unlink and trust enable require step-up | Missing/stale proof tests; no partial state |
| Logout cannot end another family | Component and disposable-Postgres E2E exploit regressions |
| Free revoke is atomic | Parallel test admits at most the configured allowance; #37 implementation referenced once |
| Simple-mode grant is per family | Family A/B isolation test |
| Transfer confirmation needs fresh proof | Coordinator transaction tests; initiation proof rejected |
| Desktop sends every current required proof | Component/API tests plus zero-callsite ownership census |
| Docs ship with code | ADR-0046, authenticated-step-up budget design, OpenAPI, and changelogs updated in the owning PRs |
| Quality gate | 0 new blocker/critical issues; 0 new high-confidence security hotspots; at least 80% new-code coverage |

All listed issue exploit tests must fail against the fixed code. Self-Hosted and
Hosted each pass their own lint, type-check, test, security, and review gates;
one repository's result is not evidence for the other.

### Docs to update

- `docs/design/authenticated-step-up-budget.md`: add every #158 consumer to the
  production caller census, its purpose, charge owner, transaction owner, and
  success-only clear point.
- `docs/adr/0046-account-authentication-and-recovery.md`: record factor-removal,
  SSO-unlink/trust, ownership-confirmation, and erasure proof policy, including
  the unverified-email rule.
- `docs/api/openapi.yaml`: exact request fields, closed-purpose enum, `202`,
  `403`, `409`, `423`, `429`, and `503` responses, and size/format bounds.
- Self-Hosted and Desktop `CHANGELOG.md`; Hosted's changelog in the reviewed
  mirror PR because it is not imported from Self-Hosted.
- The stale Desktop `privacy.ts` #661 ownership comment: replace it with the
  shipped component/API owner or remove it.

These docs ship with the code they describe; a follow-up documentation PR does
not satisfy #158.

## 14. Rollout and mirror order

1. Merge Security #157's budget call-site coverage and Security #37's one atomic
   free-revoke reservation, or rebase #158 onto the exact merged contracts.
2. Land Self-Hosted closed purposes, backend behavior, regression tests,
   OpenAPI, ADR/design updates, and changelog.
3. Land Desktop support for the new purposes, existing Settings actions,
   erasure danger zone, and closed email-confirmation deep link.
4. Release ordering prefers Desktop first, then server enforcement. If the
   server reaches a user first, old clients fail closed on dangerous actions;
   there is no legacy bearer-only fallback.
5. After the canonical Self-Hosted merge is green, allow the scheduled upstream
   mirror to import `services/**` and `docs/api/**` into Hosted through a
   reviewed PR. Resolve only mirror conflicts; do not hand-copy a competing
   implementation.
6. Add Hosted-only changelog/non-imported docs, run Hosted's independent quality
   gate and security reviews, then merge.
7. Observe `step_up_required`, 409 drift, 429 exhaustion, 503 dependency,
   email-confirmation issuance/expiry, cross-family logout refusal, and
   free-revoke reservation outcomes by fixed low-cardinality class only.

No migration, deployment configuration, feature flag, or NATS sequencing is
required. This is a fail-closed API change, so rollback must use compatible
forward code; it must not restore bearer-only erasure or factor removal.

## 15. Risks and review focus

| Risk | Likelihood | Mitigation |
|---|---|---|
| A handler verifies proof before the transaction and the locked state changes afterward | Medium | Transaction-owner helpers accept already-locked subject/authority; race tests for every route |
| #157 and #158 both charge the same code | Medium during parallel development | Production caller census; one outer request owner; rebase on #157 before implementation review |
| Erasure set is computed before locks or ignores newly joined members | Medium | Locked exact recomputation and `409`; acknowledgements clear in Desktop |
| Email link becomes a bearer-only delete or verifies the address | High impact | Link only supplies a bound intent secret; second fresh step-up in deletion tx; explicit tests for no delete/no email verification |
| Deep-link secret leaks through argv or logging | Medium | Closed parser, fixed outcome logs, no URL logging, transient IPC/component memory, reset fences |
| Free-revoke reservation is released after an unknown SQL commit | High impact | Release only on definite non-commit; mutation test for the unknown arm |
| Wrong login family earns or spends simple-mode/free authority | Medium | Keys derive only from verified access identity; family A/B tests |
| Factor-removal helper relocks the user and inverts lock order | Medium | Pass locked subject into helper; no nested `LockSubjectTx`; lock-order tests |
| Ownership confirmation trusts initiation-time proof | Medium | New non-grace purpose checked in coordinator transaction |
| Backend lands without usable Desktop erasure UI | High product impact | Desktop companion is an acceptance gate; stale #661 comment removed/updated |
| Hosted diverges through manual porting | Medium | Self-Hosted first, scheduled reviewed mirror, independent Hosted gate |

Phase-8 reviewers must focus on direct-verifier census coverage, transaction
lock ordering, commit-ambiguity settlement, deep-link secret handling, Desktop
request-context fences, RBAC/ownership rechecks, and scope overlap with #37,
#118, and #157. Security and red-team review are mandatory; renderer changes
also require the frontend specialist review.
