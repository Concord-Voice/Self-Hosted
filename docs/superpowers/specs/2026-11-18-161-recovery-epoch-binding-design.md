# Recovery token epoch binding and responder restriction

**Findings:** VULN-018, VULN-203, VULN-019.
**Status:** Phase 2 design, ready for implementation.
**Plan:** `docs/superpowers/plans/2026-11-18-161-recovery-epoch-binding.md`

## 1. Problem

Final recovery authority is a bearer JWT that lives 25 hours and is tied to
nothing but its own `jti`.

- **VULN-018.** The token carries no credential epoch. A password change or a
  completed recovery does not invalidate earlier or sibling tokens, so a token
  minted before the owner remediates still resets the account.
- **VULN-203.** The device-request routes (create, poll, complete) accept the
  same token and never read the `recovery_token_used:<jti>` marker, so a token
  already spent on a reset still drives the trusted-device ceremony.
- **VULN-019.** Any live session of the account can list and answer a
  trusted-device recovery request. The trusted-device list is only a non-zero
  count at request creation.

## 2. Goals, non-goals, anti-goals

**Goals**

1. A recovery token is valid only while `users.credential_epoch` equals the
   epoch it was issued against. Every consumer enforces this under the users
   row lock: reset-password, reset-account, device create, poll and complete,
   and the (paused) social routes.
2. A token minted without the binding is rejected everywhere.
3. A token whose `jti` is spent is rejected by the device routes.
4. The token lives 15 minutes (ADR-0046 decision 7).
5. Only a session recorded on a designated trusted device can list or answer a
   device-recovery request.

**Non-goals**

- No migration, route, dependency, environment variable or client change.
- No recovery-device key, enrollment or protocol v3. Desktop issue 84 owns the
  cryptographic responder identity.
- No change to the v2 transcript, key custody, request lifetime or rate limits.
- No change to email-code lifetime. An unredeemed emailed code stays redeemable
  for its own 10 minutes; a token minted from it after a rotation carries the
  new epoch. Code and mailbox compromise are ADR-0046's subject, not token
  binding.
- No new security-event vocabulary.
- No cancellation of requests that were created before a rotation. Such a row
  can stay `pending` for at most 15 minutes, but its requester can no longer
  poll or complete, so no ciphertext is disclosed.

**Anti-goals**

- A second revocation scheme beside the credential epoch.
- Treating `X-Machine-Id` as an authenticator, or describing the responder
  check as a security boundary.
- A Redis call while a SQL row lock is held.

## 3. Current state (verified at `b23fe7d7`)

| Concern | Where | Fact |
|---|---|---|
| Mint | `internal/mfa/handlers.go` `(*Handler).GenerateRecoveryToken`, called from `internal/auth/handlers.go` `CompleteRecovery` | Called inside a transaction that already holds the users row `FOR NO KEY UPDATE`; stamps no epoch |
| Second mint | `internal/mfa/challenge.go` `GenerateRecoveryToken(userID, jwtSecret)` | Test callers only (`middleware`, `websocket`, `mfa` tests) |
| Shared validator | `internal/mfa/recovery.go` `ValidateRecoveryClaimsTx` | Reached by every consumer: both resets (`recoveryWork`), three device routes (`recoveryGuard`), both social routes and issuance (`beginRecoveryPolicyTx`) |
| Purpose | `PurposeRecovery = "recovery_v2"` | Was already bumped once to invalidate older tokens |
| Lifetime | `mfa.recoveryTTL` and `auth.recoveryTokenUsedTTL`, both 25 h | Two constants kept equal by a comment |
| Social recovery | `middleware.RecoveryCirclePaused` | Unconditional 503 on all five social routes; no flag or configuration |
| Epoch writers | `credepoch.Op.RotateTx` | Three callers: recovery reset (both routes), password change, account-key replacement |
| Email change | none | No primary-email change route exists. `backup_email` is not an address recovery can begin from |
| Responder | `devicerecovery.Respond`, `List` | `Respond` already runs `credepoch.GuardFamilyTx`, which reads the signed `loginfamily.AccessIdentity` (user, `sid`, epoch) from the request context. `List` checks nothing but ownership |
| Session row | `refresh_tokens.id` is the access token's `sid` | Rotation marks the predecessor `revoked_at` and inserts a successor with `predecessor_id`; the predecessor row is kept (purged 90 days after revocation). The successor's `machine_id` is the refresh request's `X-Machine-Id`, else the predecessor's |
| `machine_id` exposure | `GET /mfa/trusted-devices`, `GET /sessions` | Both return it to every session of the account |
| Clients | Desktop main, Mobile main (checked 2026-11-18) | Desktop reads only `user_id`, `jti` and `exp` from the recovery token and caps its deadline at `exp`. It treats a non-OK list poll as an error. Neither client reads `machine_id` from a response. Desktop's "designate this device" control is not wired, so no shipped client has a designated device |

## 4. Approaches

| Approach | Result | Why |
|---|---|---|
| **A. Bind to the credential epoch in the shared validator** | **Chosen** | One comparison under a lock every consumer already takes. Reuses the state that already revokes refresh tokens and login challenges. No migration |
| B. A per-user `recovery_generation` counter | Rejected | A migration and a second revocation system. Every credential writer would need a second increment that can drift from the epoch |
| C. Server-side recovery sessions with an opaque handle | Rejected | Explicit revocation, but it changes the token contract for every client and overlaps the protocol v3 design in Desktop issue 84 |

The choice changes only if a credential writer is found that must revoke
recovery authority and cannot rotate the epoch.

## 5. Decisions

### D1. Binding marker is the purpose version

`PurposeRecovery` becomes `"recovery_v3"`. A `recovery_v3` token is by
construction minted by code that stamps the epoch, so the purpose is the
explicit binding marker. No separate claim is added.

`users.credential_epoch` is NULL until the first rotation and the claim is
`cred_epoch,omitempty`, so an empty claim alone cannot distinguish "minted
before any rotation" from "minted by old code". The purpose version does.
A `recovery_v2` token fails purpose validation at parse time on every route.

In memory, `auth.RecoveryClaims` gains `CredentialEpoch string` and
`EpochBound bool`. Only `ValidateRecoveryToken` sets `EpochBound`. The locked
validator refuses claims without it, so a hand-built or zero-value
`RecoveryClaims` authorizes nothing.

### D2. Mint reads the epoch under the lock it is checked under

`MFAChecker.GenerateRecoveryToken(userID, verifiedFactor)` is replaced by
`IssueRecoveryTokenTx(ctx, tx, userID, verifiedFactor)`. It locks the users
row, evaluates current recovery policy, reads
`COALESCE(credential_epoch, '')` in the same statement, and mints. The caller
never handles the epoch, so it cannot be omitted or transposed with another
string. `CompleteRecovery` calls it in place of the policy check plus mint.

The package-level `mfa.GenerateRecoveryToken` becomes
`GenerateRecoveryToken(userID, verifiedFactor string, jwtSecret JWTSecret, credEpoch CredEpoch)`
and stays the single signing function. `GenerateChallengeToken` refuses
`PurposeRecovery`, as it already refuses `PurposeLogin`.

### D3. Two validation layers, one authority

- **Authority:** `ValidateRecoveryClaimsTx`, after the users lock, requires
  `claims.EpochBound` and `claims.CredentialEpoch == COALESCE(credential_epoch, '')`.
  The comparison is strict equality. `credepoch.MatchEpoch` is not used: it
  admits any claim while the stored epoch is NULL, which is wrong for a token
  that must name the state it was issued against.
- **Pre-check:** `ValidateRecoveryToken` makes the same comparison without a
  lock, folded into its existing policy read. A stale token is refused before
  any side effect: no used-marker reservation, no credential-fence begin, no
  password hashing, no request-body acceptance.

A mismatch returns `ErrRecoveryProofInvalid` from the locked validator and a
plain error from the pre-check. Both surface as the existing generic 401. A
failed read is `ErrRecoveryPolicyUnavailable` (503).

### D4. Lifetime is 15 minutes, held in one constant

`auth.RecoveryTokenTTL = 15 * time.Minute` replaces both `mfa.recoveryTTL` and
`auth.recoveryTokenUsedTTL`. The marker is written at or after issuance with
the same duration, so it always outlives the token.

Fifteen minutes is ADR-0046 decision 7. It equals the device-request lifetime,
which the server already caps at token expiry and the Desktop client already
honors. The ceremony and the final reset therefore share one 15-minute budget;
a user who runs out restarts from the email step.

Nothing live needs longer. Social recovery was the only 24-hour flow and is
unconditionally paused. A test pins that pause while the TTL is under 24 hours,
so un-pausing forces a request-scoped credential to be designed first.

### D5. Spent-`jti` check on device routes is a read outside SQL

`validateDeviceRecoveryToken` reads `EXISTS recovery_token_used:<jti>` after
token validation and before `devicerecovery.Create`, `Poll` or `Complete`.

- Present: the same 401 as an invalid token.
- Redis nil or erroring: 503, before any row is read or written.
- It never sets, deletes or extends the marker. The ceremony still does not
  spend the token, so "complete the ceremony, then reset with the same token"
  keeps working.

The key prefix moves to one helper shared with the reset path.

The read is deliberately not inside `recoveryGuard`. This repository keeps
Redis out of SQL transactions, and the epoch comparison in the guard is the
linearization point. If a reset reserves the marker after the read, it either
commits first, so the guard sees the new epoch and refuses, or it commits
after the device operation, which is then equivalent to the operation having
happened first. The marker check adds coverage for a reset in flight and for
an ambiguous commit, where the marker is kept and the epoch is uncertain.

### D6. Responder must be on a designated trusted device

`devicerecovery` gains `lockTrustedResponder(ctx, tx, userID)`:

```sql
SELECT d.id FROM trusted_recovery_devices d
JOIN refresh_tokens r ON r.user_id = d.user_id AND r.machine_id = d.machine_id
WHERE r.id = $1 AND r.user_id = $2
FOR SHARE OF d
```

`$1` is the `sid` of the signed access identity in the request context, the
identity `GuardFamilyTx` already validates. `Respond` and `List` keep their
signatures; no caller passes a session id.

- **`Respond`:** called after `lockUser` and `GuardFamilyTx`, before
  `lockRequest`. No row returns the new `ErrForbidden` (403). Because it runs
  before the request is looked up, an untrusted session learns nothing about
  whether a request exists and cannot trigger an expiry transition.
- **`List`:** called after `lockUser`. No row returns an empty list with 200,
  because the Desktop poll treats any non-OK status as a failure.
- **No `revoked_at` predicate.** An access token minted before a refresh
  rotation carries the predecessor's id, and that row is revoked but kept.
  Filtering on `revoked_at IS NULL` would lock a trusted device out for up to
  the access-token lifetime after every rotation. Session liveness is the
  family guard's job.
- **`FOR SHARE OF d`** serializes against removal of the designation: the
  response either commits before the delete or sees no row.
- A NULL `machine_id`, a missing session row and a missing identity all fail
  closed. A session created without `X-Machine-Id` becomes eligible once a
  refresh records one.

Lock order becomes `users`, then the designation row, then `recovery_requests`.
The trusted-device writers take only the designation row, so no cycle exists.

### D7. The responder check is hardening, not a boundary

`machine_id` is asserted by the client at sign-in and again at refresh. A
session that learns a designated device's identifier and can pass the
machine-change refresh gate, or sign in again, can present it. The check stops
an ordinary other session from answering; it does not authenticate a device.
The design document and ADR say so in these words. The boundary is the
device-held key in Desktop issue 84.

### D8. `machine_id` leaves read responses

Because D6 gives the identifier a gating role, it is removed from:

- `GET /mfa/trusted-devices` (each device object),
- `GET /sessions` (each session object), and
- the "Trusted device designated" log line.

Redacting only the trusted-device list would change nothing, because the
session list returns the same value for every session of the account.
`POST /mfa/trusted-devices` still echoes the caller's own header value.

### D9. Credential changes that revoke recovery tokens

A token dies when the epoch rotates: a completed reset-password or
reset-account, a password change, or an account-key replacement. MFA factor
changes do not rotate the epoch; the existing policy re-check in the same
validator covers them.

There is no primary-email change path. ADR-0046 records the rule that any
future one must rotate the epoch in its own transaction.

### D10. Rollout

Deploying invalidates every outstanding `recovery_v2` token; affected users
re-enter an email code. During a blue-green overlap each side refuses the
other's tokens, which is fail-closed in both directions. Rolling back restores
the old acceptance rules, so a rollback target must be treated as vulnerable.

## 6. Response contract

| Condition | Route | HTTP |
|---|---|---:|
| Stale epoch, unbound, expired or malformed token | all recovery-token routes | 401 (existing generic body) |
| Spent `jti` | device create, poll, complete | 401 (same body) |
| Spent `jti` | reset-password, reset-account | 401 (existing "already used" body) |
| Policy, epoch or spent state cannot be read | all | 503 |
| Session not on a trusted device | `POST /mfa/recovery-requests/{id}/respond` | 403 |
| Session not on a trusted device | `GET /mfa/recovery-requests` | 200, `{"requests": []}` |

## 7. Exploit-test inversion

Each exploit asserts the vulnerable behavior. After the fix that assertion
must fail, and an in-repository regression asserts the opposite.

| Finding | Exploit assertion today | After the fix | Retained regression |
|---|---|---|---|
| VULN-018 | `TestVuln018_RecoveryTokenSurvivesCredentialRotation`: a token is still accepted after the epoch rotates | Validation returns `ErrRecoveryProofInvalid`; the exploit test fails | `TestVuln018RecoveryTokenDoesNotSurviveCredentialRotation` (`internal/mfa`), `TestRecoveryTokenRejectedAfterCredentialRotation` and `TestSiblingRecoveryTokenDiesWithCompletedRecovery` (`internal/auth`) |
| VULN-203 (epoch) | Static: device routes accept a token with no epoch | `recovery_v2` fails purpose validation; a rotated epoch fails the guard | `TestLegacyRecoveryPurposeIsRejected` (`internal/mfa`), the device-route rows of `TestRecoveryTokenRejectedAfterCredentialRotation` |
| VULN-203 (spent) | Static: device routes accept a spent `jti` | 401 before any request row is read or written | `TestDeviceRoutesRejectSpentRecoveryToken`, `TestDeviceRoutesFailClosedWhenSpentStateUnreadable` (`internal/auth`) |
| VULN-019 | Static: any session answers and lists | 403 on respond, empty list | `TestRespondRequiresTrustedDevice`, `TestListHidesRequestsFromUntrustedSession` (`internal/devicerecovery`), route tests in `internal/mfa` |

The VULN-018 exploit test uses a machine-specific overlay path and a fake SQL
driver, so it is re-authored in the repository, not copied. Where the original
test and its overlay are available, run it once and record that it fails.

## 8. Acceptance criteria

| # | Criterion | Evidence |
|---|---|---|
| 1 | 0 new blocker or critical issues | Phase 8 dispositions at the PR head |
| 2 | 0 new high-confidence security hotspots | Scanner and security review at the PR head |
| 3 | At least 80% coverage on new code | Coverage report for the changed Go packages |
| 4 | After a password change or a completed recovery, every earlier token is refused by reset-password, reset-account and all three device routes | `TestRecoveryTokenRejectedAfterCredentialRotation`, `TestSiblingRecoveryTokenDiesWithCompletedRecovery` |
| 5 | A refused stale token leaves no used-marker and changes no account state | Same tests assert the Redis key and the password hash |
| 6 | An unbound (`recovery_v2`) token is refused everywhere | `TestLegacyRecoveryPurposeIsRejected` and the extended legacy-token integration test |
| 7 | A never-rotated account still recovers | `TestNeverRotatedAccountRecoveryStillCompletes` |
| 8 | A spent `jti` is refused by the device routes; unreadable spent state is 503 | Task 4 tests |
| 9 | A token older than 15 minutes is refused | `TestGenerateRecoveryTokenStampsEpochPurposeAndTTL` plus an expired-token route test |
| 10 | An untrusted session gets 403 from respond and an empty list | Task 5 tests |
| 11 | A designated device completes the v2 transcript, including after a refresh rotation | `TestTrustedResponderSurvivesRefreshRotation`, the vectors-based happy path |
| 12 | `machine_id` is absent from the trusted-device list and the session list | Task 6 tests |
| 13 | The listed exploit test fails against the fixed code | Recorded overlay run, or the re-authored regression where the overlay is unavailable |
| 14 | **Docs to update** ship in the same PR: `docs/design/trusted-device-recovery.md`, `docs/adr/0046-account-authentication-and-recovery.md`, `docs/api/openapi.yaml`, `CHANGELOG.md` | Diff |
| 15 | Lint, vet and the package test suites pass | Task 8 output |

## 9. Risks

| Risk | Likelihood | Mitigation |
|---|---|---|
| A never-rotated account is locked out by an empty-epoch comparison | Medium | Strict equality on `COALESCE(..., '')`; criterion 7 |
| A legitimate ceremony runs past the shared 15 minutes | Medium | Client already caps at `exp` and restarts cleanly; the constant is one line if the lead prefers longer |
| The spent read is mistaken for the authority and moved under the lock | Low | D5 states the linearization argument; a code comment repeats it |
| A trusted device is refused after refresh rotation | Medium before D6, low after | No `revoked_at` predicate; criterion 11 |
| The responder check is read as device authentication | Medium | D7 wording in the design document, ADR and changelog |
| Removing `machine_id` breaks a released client | Low | No reader on Desktop or Mobile main; Task 6 re-checks the latest released Desktop tag and stops if one exists |
| Open pull requests edit the same constant block, `MFAChecker` and the same documents | High | Land order is the orchestrator's call; the interface change is one replaced line, and the constant block loses one line |
| Test fixtures use sessions with no `refresh_tokens` row | Certain | Task 5 adds a `trustSession` fixture helper and updates every existing responder test |

## 10. Left to the implementer

- The shape of the shared policy-read helper in `internal/mfa`.
- Exact error strings, within the response contract above.
- Whether the route matrix is one table test or several.
