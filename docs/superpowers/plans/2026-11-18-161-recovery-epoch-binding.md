# Recovery Token Epoch Binding Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bind recovery tokens to the credential epoch, refuse spent tokens on the device routes, shorten the token to 15 minutes, and let only a designated trusted device list or answer a device-recovery request.

**Architecture:** The token's purpose version becomes `recovery_v3` and its mint reads `users.credential_epoch` under the users row lock. The shared validator `ValidateRecoveryClaimsTx` compares that epoch under the same lock for every consumer. The device routes add a read-only spent-`jti` lookup, and `devicerecovery` matches the signed session's recorded `machine_id` against `trusted_recovery_devices`.

**Tech Stack:** Go, Gin, golang-jwt v5, go-redis v9, PostgreSQL, Redis. No new dependency.

**Spec:** `docs/superpowers/specs/2026-11-18-161-recovery-epoch-binding-design.md`

## Global Constraints

- Work on `feature/security-161-recovery-epoch-binding`. All Go paths below are under `services/control-plane/`.
- No migration, route, dependency, environment variable or client change.
- `auth.RecoveryTokenTTL = 15 * time.Minute` is the only lifetime constant for the token and its used-marker.
- `PurposeRecovery = "recovery_v3"`. Epoch comparison is strict equality on `COALESCE(credential_epoch, '')`; do not use `credepoch.MatchEpoch`.
- No Redis call while a SQL transaction or row lock is held.
- Every error is checked. A failed epoch, policy or spent-state read is 503, never 401 and never allow.
- Stale, unbound, expired and spent tokens share the existing generic 401 on the device routes. Never log a token, a `jti` or a `machine_id`.
- Integration tests need an isolated `DATABASE_URL` and `REDIS_URL`; the default `concord` database is forbidden. Follow "Test isolation" in `.claude/rules/domains/tests.md`. Never set a private `GOCACHE`, `GOMODCACHE` or `GOLANGCI_LINT_CACHE`.
- Commits are Conventional Commits with a bare type and no parenthesized scope.

## File map

| File | Responsibility in this change |
|---|---|
| `internal/auth/handlers.go` | `RecoveryTokenTTL`, `RecoveryClaims` fields, `MFAChecker` method, `CompleteRecovery`, used-marker key helper, spent read in `validateDeviceRecoveryToken` |
| `internal/mfa/challenge.go` | Purpose version, signing function, `GenerateChallengeToken` refusal |
| `internal/mfa/recovery.go` | `IssueRecoveryTokenTx`, locked policy-and-epoch read, epoch comparison in `ValidateRecoveryClaimsTx` |
| `internal/mfa/handlers.go` | `ValidateRecoveryToken` pre-check; `ListTrustedDevices` and designate log redaction |
| `internal/devicerecovery/store.go` | `ErrForbidden`, `lockTrustedResponder`, calls in `Respond` and `List` |
| `internal/sessions/handlers.go` | Drop `machine_id` from the session list |
| `docs/design/trusted-device-recovery.md`, `docs/adr/0046-account-authentication-and-recovery.md`, `docs/api/openapi.yaml`, `CHANGELOG.md` | Contract shipped with the code |

## Review Focus

1. **A never-rotated account** (`credential_epoch` NULL) recovers, and its token dies at the first rotation. Task 3: `TestNeverRotatedAccountRecoveryStillCompletes`.
2. **Ceremony, then reset with the same token** inside 15 minutes still succeeds, and the device routes refuse that token afterwards. Task 4: `TestDeviceCeremonyThenResetWithSameToken`.
3. **Redis is down** when a device route runs: 503, no request row, not a 401. Task 4: `TestDeviceRoutesFailClosedWhenSpentStateUnreadable`.
4. **A trusted device just rotated its refresh token** and still holds the older access token: it can still answer. Task 5: `TestTrustedResponderSurvivesRefreshRotation`.
5. **An ordinary session polls the request list**: it gets 200 with an empty list, not an error state. Task 5: `TestListRouteReturnsEmptyForUntrustedSession`.

---

### Task 1: Token contract (purpose, lifetime, epoch-stamped signing)

**Files:**
- Modify: `internal/auth/handlers.go` (recovery const block; `RecoveryClaims`)
- Modify: `internal/mfa/challenge.go`
- Test: `internal/mfa/challenge_test.go`, `internal/api/recovery_circle_pause_integration_test.go`
- Update callers: `internal/middleware/auth_tokenclass_test.go`, `internal/websocket/token_renewal_db_test.go`, `internal/mfa/recovery_test.go`

**Interfaces:**
- Produces: `auth.RecoveryTokenTTL`; `auth.RecoveryClaims{RecoveryFactor, UserID, JTI string; ExpiresAt time.Time; CredentialEpoch string; EpochBound bool}`; `mfa.GenerateRecoveryToken(userID, verifiedFactor string, jwtSecret JWTSecret, credEpoch CredEpoch) (string, string, error)`; `mfa.PurposeRecovery == "recovery_v3"`.

- [ ] **Step 1: Write the failing tests** in `internal/mfa/challenge_test.go`:
  - `TestGenerateRecoveryTokenStampsEpochPurposeAndTTL`: mint with factor `"email"` and epoch `"e1"`; assert `Purpose == "recovery_v3"`, `CredentialEpoch == "e1"`, `RecoveryFactor == "email"`, and `ExpiresAt - IssuedAt == 15*time.Minute`. A second mint with epoch `""` has an empty `CredentialEpoch`.
  - `TestGenerateRecoveryTokenRejectsUnknownFactor`: factor `"unverified"` returns an error and no token.
  - `TestGenerateChallengeTokenRefusesRecoveryPurpose`: `GenerateChallengeToken(u, PurposeRecovery, secret, "")` returns an error.
  - `TestLegacyRecoveryPurposeIsRejected`: sign `ChallengeClaims{Purpose: "recovery_v2", RecoveryFactor: "email"}` with the test secret; `ValidateChallengeToken(token, secret, PurposeRecovery)` returns an error.
- [ ] **Step 2: Write the failing pause guard** `TestRecoveryTokenTTLRequiresSocialPause` in `internal/api/recovery_circle_pause_integration_test.go`: assert `auth.RecoveryTokenTTL < 24*time.Hour`, then use the existing fixture and `assertPaused` on `POST /api/v1/auth/recovery/social-request` and `GET /api/v1/auth/recovery/social-request/{id}`. Its comment states that un-pausing needs a request-scoped credential first.
- [ ] **Step 3: Run red.** `go test ./internal/mfa -run 'TestGenerateRecoveryToken|TestGenerateChallengeTokenRefuses|TestLegacyRecoveryPurpose' -count=1`. Expected: compile failure on the new signature.
- [ ] **Step 4: Implement.** In `auth`: add `RecoveryTokenTTL`, delete `recoveryTokenUsedTTL` and use the new constant at its one use, add the two `RecoveryClaims` fields. In `mfa/challenge.go`: bump the purpose, delete `recoveryTTL`, change the signing function to the signature above (it validates the factor against `email`, `totp`, `webauthn`), make `GenerateChallengeToken` refuse `PurposeRecovery`, and replace the "carry no epoch" comment with one sentence: the token names the epoch it was issued against, and the reset it authorizes rotates that epoch, which revokes it and its siblings.
- [ ] **Step 5: Fix the test callers** listed above to the new signature (`"email"`, `CredEpoch("")`).
- [ ] **Step 6: Run green.** `go test ./internal/mfa ./internal/middleware ./internal/websocket -run 'Recovery|TokenClass|Renewal' -count=1`. Expected: PASS for this task's tests; `(*Handler).GenerateRecoveryToken` still compiles until Task 2.
- [ ] **Step 7: Commit.** `git commit -m "feat: stamp the credential epoch into 15-minute recovery tokens"`

### Task 2: Locked issuance and the authoritative epoch comparison

**Files:**
- Modify: `internal/mfa/recovery.go`, `internal/mfa/handlers.go` (remove `(*Handler).GenerateRecoveryToken`)
- Modify: `internal/auth/handlers.go` (`MFAChecker`, `CompleteRecovery`)
- Test: `internal/mfa/recovery_test.go`, `internal/auth/recovery_policy_integration_test.go`
- Update fakes: `internal/auth/security_event_test.go`, `mfa_challenge_errors_test.go`, `oauth_adapter_test.go`, `recovery_commit_ambiguity_internal_test.go`, `recovery_policy_errors_test.go`

**Interfaces:**
- Consumes: Task 1's signing function and `RecoveryClaims` fields.
- Produces: `MFAChecker.IssueRecoveryTokenTx(ctx context.Context, tx *sql.Tx, userID, verifiedFactor string) (token string, jti string, err error)`, replacing `GenerateRecoveryToken(userID, verifiedFactor string)`. `ValidateRecoveryClaimsTx` keeps its signature.

- [ ] **Step 1: Write the failing tests** in `internal/mfa/recovery_test.go`. Every scripted `recoveryPolicyQuery` answer gains a sixth value, the epoch string.
  - `TestIssueRecoveryTokenTxStampsLockedEpoch`: scripted lock then policy row with epoch `"e1"`; the token's `cred_epoch` is `"e1"` and `statements[0]` contains `FOR NO KEY UPDATE`. With a NULL/empty epoch the claim is empty.
  - `TestIssueRecoveryTokenTxRefusesUnsatisfiedPolicy`: reserved `{totp}` with factor `"email"` returns `auth.ErrRecoveryProofInvalid` and an empty token; an unknown factor returns the same; a failed policy read returns `auth.ErrRecoveryPolicyUnavailable`.
  - `TestRecoveryClaimsTxRejectsRotatedEpoch`, a table over (claim, stored, `EpochBound`): `("", "", true)` and `("e1", "e1", true)` pass; `("", "e1", true)`, `("e1", "e2", true)`, `("e1", "", true)` and `("e1", "e1", false)` return `auth.ErrRecoveryProofInvalid`.
  - Extend `TestRecoveryClaimsTransactionUsesLockedPolicyWithoutRedis` so its claims carry `EpochBound: true`.
- [ ] **Step 2: Write the failing integration test** `TestRecoveryIssuanceBindsCurrentEpoch` in `internal/auth/recovery_policy_integration_test.go`: set the user's `credential_epoch` to a known value, complete verify-code, decode the returned token with `mfa.ValidateChallengeToken(..., mfa.PurposeRecovery)`, and assert `CredentialEpoch` equals the stored value.
- [ ] **Step 3: Run red.** `go test ./internal/mfa -run 'TestIssueRecoveryTokenTx|TestRecoveryClaimsTx' -count=1`. Expected: undefined `IssueRecoveryTokenTx`.
- [ ] **Step 4: Implement in `internal/mfa/recovery.go`.** Add an unexported `lockRecoveryPolicyTx(ctx, tx, userID) (methods []string, credEpoch string, err error)` that takes the existing users lock and extends the existing locked policy `SELECT` with `COALESCE(credential_epoch, '')`. `IssueRecoveryTokenTx` validates the factor, calls it, checks the factor against `methods`, and signs with the read epoch. `ValidateRecoveryClaimsTx` calls it and additionally requires `claims.EpochBound` and equal epochs. Lock and read failures stay wrapped in `auth.ErrRecoveryPolicyUnavailable`.
- [ ] **Step 5: Implement in `internal/auth/handlers.go`.** Replace the interface method and its comment. `CompleteRecovery` begins a `READ COMMITTED` transaction, calls `IssueRecoveryTokenTx`, and on error rolls back and answers through `writeRecoveryPolicyError`, falling back to the existing 500 body. The rest of the function is unchanged.
- [ ] **Step 6: Update the five fakes** to the new method.
- [ ] **Step 7: Run green.** `go test -race ./internal/mfa ./internal/auth -run 'Recovery' -count=1`. Expected: PASS, except tests that still build validator claims without `EpochBound`; fix those fixtures here.
- [ ] **Step 8: Commit.** `git commit -m "feat: bind recovery token validation to the credential epoch"`

### Task 3: Pre-check, route matrix and the exploit regression

**Files:**
- Modify: `internal/mfa/handlers.go` (`ValidateRecoveryToken`), `internal/mfa/recovery.go` (unlocked policy read returns the epoch)
- Create: `internal/auth/recovery_epoch_binding_integration_test.go`
- Test: `internal/mfa/recovery_test.go`

**Interfaces:**
- Consumes: Task 2.
- Produces: `ValidateRecoveryToken` returns claims with `CredentialEpoch` set and `EpochBound == true`, and refuses a token whose epoch differs from the stored one.

- [ ] **Step 1: Write the failing unit tests** in `internal/mfa/recovery_test.go`:
  - `TestValidateRecoveryTokenPrechecksEpoch`: stored `"e2"`, token `"e1"` returns an error that is not `auth.ErrRecoveryPolicyUnavailable` and nil claims; matching epochs return claims with `EpochBound == true`; a failed read returns `auth.ErrRecoveryPolicyUnavailable`.
  - `TestVuln018RecoveryTokenDoesNotSurviveCredentialRotation`: issue a token at epoch `"e1"`, script the stored epoch as `"e2"`, assert both `ValidateRecoveryToken` and `ValidateRecoveryClaimsTx` refuse it.
- [ ] **Step 2: Write the failing integration tests** in the new file, against the real router:
  - `TestRecoveryTokenRejectedAfterCredentialRotation`: for each rotation in {the real password-change route, a completed reset-password with a sibling token, the rotation helper in `internal/testhelpers/stale_boundary.go`} and each route in {reset-password, reset-account, device create, device poll, device complete}: expect 401. For the two reset routes also assert `recovery_token_used:<jti>` does not exist afterwards and the stored password hash is unchanged.
  - `TestSiblingRecoveryTokenDiesWithCompletedRecovery`: mint tokens A and B; A resets the password (200); B is 401 on all five routes.
  - `TestNeverRotatedAccountRecoveryStillCompletes`: a user with NULL `credential_epoch` gets a token with no `cred_epoch`, reset-password returns 200, and `credential_epoch` is then non-NULL.
  - Extend `TestRecoveryPolicyLegacyAndIncompleteTokensCannotReachRecoveryConsumers` with a `recovery_v2` token: 401 on all five routes.
- [ ] **Step 3: Run red.** `go test -race ./internal/auth -run 'TestRecoveryTokenRejectedAfter|TestSiblingRecoveryToken|TestNeverRotated' -count=1`. Expected: the reset rows fail on the used-marker assertion (the marker is reserved before the locked check refuses).
- [ ] **Step 4: Implement.** The unlocked policy read used by `ValidateRecoveryToken` also returns `COALESCE(credential_epoch, '')` (one query, no added round trip; `GetRecoveryMethods` keeps its signature and drops the value). `ValidateRecoveryToken` compares, then returns claims with both new fields set.
- [ ] **Step 5: Run green**, then `go test -race ./internal/mfa ./internal/auth -count=1`. Expected: PASS.
- [ ] **Step 6: Run the original exploit** where the original `recovery_vuln018_test.go` and its overlay are available. Expected: FAIL. Record the output for the PR. If the overlay is unavailable, record that and cite Step 1's regression.
- [ ] **Step 7: Commit.** `git commit -m "test: prove stale recovery tokens fail on every consumer"`

### Task 4: Spent-`jti` read on the device routes

**Files:**
- Modify: `internal/auth/handlers.go` (`validateAndConsumeRecoveryToken`, `validateDeviceRecoveryToken`)
- Test: `internal/auth/device_recovery_v2_test.go`

**Interfaces:**
- Produces (unexported, `auth`): `recoveryTokenUsedKey(jti string) string` and `(h *Handler) recoveryTokenSpent(ctx context.Context, jti string) (bool, error)`.

- [ ] **Step 1: Write the failing tests:**
  - `TestDeviceRoutesRejectSpentRecoveryToken`: set `recovery_token_used:<jti>` with a known TTL; create, poll and complete each return 401 with the generic body; no `recovery_requests` row exists for the user; the key still exists and its TTL has not grown.
  - `TestDeviceRoutesFailClosedWhenSpentStateUnreadable`: with a failing Redis hook on that key prefix, each route returns 503 and no row is inserted.
  - `TestDeviceCeremonyThenResetWithSameToken`: run the existing vectors-based ceremony to `complete`, then reset-password with the same token returns 200, then a device poll with it returns 401. Rename and extend `TestDeviceRecoveryV2ActualRoutesCompleteAndResetTokenSurvives` for this.
- [ ] **Step 2: Run red.** `go test -race ./internal/auth -run 'TestDeviceRoutes|TestDeviceCeremonyThenReset' -count=1`. Expected: 200 where 401 or 503 is required.
- [ ] **Step 3: Implement.** `recoveryTokenUsedKey` replaces the inline format string in the reset path. `recoveryTokenSpent` returns an error for a nil client and for any Redis error. `validateDeviceRecoveryToken` calls it after the claims check: error answers through `writeRecoveryPolicyError(c, ErrRecoveryPolicyUnavailable)`, spent answers `devicerecovery.ErrUnauthorized`. Replace the function's comment with the linearization argument from spec D5 in two sentences.
- [ ] **Step 4: Run green**, then `go test -race ./internal/auth ./internal/api -run 'DeviceRecovery|Device' -count=1`. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "fix: refuse spent recovery tokens on device recovery routes"`

### Task 5: Restrict responders to designated trusted devices

**Files:**
- Modify: `internal/devicerecovery/store.go`
- Test: `internal/devicerecovery/store_test.go`, `internal/mfa/device_recovery_v2_test.go`, `internal/auth/device_recovery_v2_test.go`

**Interfaces:**
- Produces: `devicerecovery.ErrForbidden = &APIError{http.StatusForbidden, ...}`; unexported `lockTrustedResponder(ctx context.Context, tx *sql.Tx, userID string) error`. `Respond` and `List` keep their signatures.

- [ ] **Step 1: Add the fixture helper** `trustSession(t, db, userID string, sid uuid.UUID) string` to each affected test package: upsert a `refresh_tokens` row with `id = sid` and a fresh `machine_id`, insert the matching `trusted_recovery_devices` row, return the machine id. Check the row's required columns against migration `000002`. Test identities come from access-only family fixtures, so most have no `refresh_tokens` row yet.
- [ ] **Step 2: Write the failing store tests:**
  - `TestRespondRequiresTrustedDevice`: an untrusted session gets `ErrForbidden` for `offer`, `approve` and `reject`, and for a request id that does not exist; the request row is byte-for-byte unchanged.
  - `TestListHidesRequestsFromUntrustedSession`: returns an empty slice and nil error; the pending row is not transitioned.
  - `TestTrustedResponderSurvivesRefreshRotation`: predecessor row revoked with machine M and successor live with M, both `sid`s can respond; a successor with a different machine and a row with NULL `machine_id` get `ErrForbidden`.
  - `TestRemovedTrustedDeviceCannotRespond`: delete the designation, then `ErrForbidden`.
  - `TestTrustedDeviceOfAnotherAccountCannotRespond`: the same machine id designated for user B does not let B's session answer A's request (existing `ErrNotFound`), and does not make A's undesignated session trusted.
- [ ] **Step 3: Write the failing route tests** in `internal/mfa/device_recovery_v2_test.go`: `TestRespondRouteReturns403ForUntrustedSession`; `TestListRouteReturnsEmptyForUntrustedSession` (200 and body exactly `{"requests":[]}`); keep the vectors-based happy path, now through `trustSession`.
- [ ] **Step 4: Run red.** `go test -race ./internal/devicerecovery ./internal/mfa -run 'Trusted|Untrusted|RespondRequires|ListHides' -count=1`. Expected: untrusted sessions succeed.
- [ ] **Step 5: Implement** `lockTrustedResponder` with the query in spec D6, reading the identity with `loginfamily.AccessIdentityFromContext`. A missing identity, a user mismatch, a nil `sid` and `sql.ErrNoRows` all return `ErrForbidden`; any other error is wrapped and surfaces as 500. Do not add a `revoked_at` predicate. Call it in `Respond` after `GuardFamilyTx` and before `lockRequest`; in `List` after `lockUser`, returning `[]any{}` on `ErrForbidden`. Update the lock-order comment on `Row`.
- [ ] **Step 6: Update existing responder tests** in the three test files to call `trustSession`, and correct the store-test comment that says the requester has no credential epoch.
- [ ] **Step 7: Run green**, then `go test -race ./internal/devicerecovery ./internal/mfa ./internal/auth -count=1`. Expected: PASS.
- [ ] **Step 8: Commit.** `git commit -m "fix: require a designated trusted device to answer recovery requests"`

### Task 6: Remove `machine_id` from read responses

**Files:**
- Modify: `internal/mfa/handlers.go` (`ListTrustedDevices`, the designate log line), `internal/sessions/handlers.go` (session list)
- Test: the existing trusted-device and session-list handler tests in `internal/mfa` and `internal/sessions`

- [ ] **Step 1: Precondition.** On the latest released Desktop tag, search for readers of `machine_id` from the trusted-device or session responses. None exist on Desktop or Mobile main. If a released reader exists, stop and report; do not ship this task.
- [ ] **Step 2: Write the failing tests** `TestListTrustedDevicesOmitsMachineID` and `TestSessionListOmitsMachineID`: create a session and a designation with a known machine id; assert the response body does not contain `machine_id` or the value, and that every other existing field is still present.
- [ ] **Step 3: Run red.** `go test -race ./internal/mfa ./internal/sessions -run 'OmitsMachineID' -count=1`. Expected: the field is present.
- [ ] **Step 4: Implement.** Drop the column from both list queries and the field from both response objects. Remove `machine_id` from the "Trusted device designated" log call. Leave the `POST /mfa/trusted-devices` response unchanged.
- [ ] **Step 5: Run green**, then both packages in full. Expected: PASS.
- [ ] **Step 6: Commit.** `git commit -m "fix: stop returning machine identifiers in device and session lists"`

### Task 7: Documentation

**Files:**
- Modify: `docs/design/trusted-device-recovery.md`, `docs/adr/0046-account-authentication-and-recovery.md`, `docs/api/openapi.yaml`, `CHANGELOG.md`

- [ ] **Step 1: `docs/design/trusted-device-recovery.md`.** Replace the paragraph that says requester tokens have no credential epoch with the epoch binding, the 15-minute lifetime shared by ceremony and reset, and the spent-token rule. In the API table, the two `/mfa/recovery-requests` rows require a session on a designated trusted device, with 403 and empty-list behavior. Add the lock order and the sentence from spec D7 that the check is hardening and not device authentication. Add 403 and 503 to the status list.
- [ ] **Step 2: `docs/adr/0046-account-authentication-and-recovery.md`.** Add a dated implementation note: the epoch-bound 15-minute token of decision 7 is implemented as `recovery_v3`; the responder restriction is interim hardening ahead of the recovery device; any future primary-email change must rotate the credential epoch in its own transaction.
- [ ] **Step 3: `docs/api/openapi.yaml`.** `verify-code`: purpose `recovery_v3`, 15 minutes, epoch-bound. Device routes: 401 covers spent and stale tokens; add 503. `/mfa/recovery-requests/{id}/respond`: add 403. `/mfa/recovery-requests`: note the empty list. Remove `machine_id` from the `TrustedDevice` list item and the `Session` schema; leave it on the designate response.
- [ ] **Step 4: `CHANGELOG.md`.** One line under `### Security` in `[Prerelease]`, tagged `**Recovery:**` and marked `**BREAKING**`: tokens last 15 minutes and stop working after a password change or completed recovery; outstanding tokens are invalid after upgrade; only a designated trusted device can answer a device recovery request; lists no longer return machine identifiers.
- [ ] **Step 5: Verify.** Run the repository's OpenAPI and Markdown checks used by pre-commit. Expected: PASS.
- [ ] **Step 6: Commit.** `git commit -m "docs: record recovery token binding and responder restriction"`

### Task 8: Gate

- [ ] **Step 1:** From `services/control-plane`, `go build ./... && go vet ./internal/...`. Expected: clean.
- [ ] **Step 2:** `go test -race ./internal/auth ./internal/mfa ./internal/devicerecovery ./internal/sessions ./internal/api ./internal/middleware ./internal/websocket -count=1` against isolated services. Expected: PASS with no skipped PostgreSQL test counted as proof.
- [ ] **Step 3:** `golangci-lint run ./internal/...`. Expected: 0 issues.
- [ ] **Step 4:** Produce a coverage profile for the changed packages and confirm at least 80% on new code.
- [ ] **Step 5:** Confirm each row of the spec's acceptance table has its named evidence, and list the exploit result from Task 3 Step 6.

## Ordering

Tasks 1 to 4 are sequential. Tasks 5 and 6 do not depend on 1 to 4 and can run in parallel with them only in separate worktrees, because Tasks 2, 3 and 6 all edit `internal/mfa/handlers.go`. Task 7 follows the code. Task 8 is last.
