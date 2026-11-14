# Security #157 Account Attempt Budgets Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bound every code guess in Security #157 by a durable account or pending-registration budget without breaking transaction-scoped factor consumption.

**Architecture:** Reuse `stepup.Charge` and `DangerousActionBudget` at each missing MFA step-up caller, backed by a production caller census. Add a separate normalized-email recovery failure counter, preserve live recovery records with `SET NX`, and keep registration attempt and resend state across code/email rotation. Self-Hosted is the source; Hosted imports its shared server changes through a reviewed PR.

**Tech Stack:** Go, Gin v1.12.0, go-redis v9.23.0, PostgreSQL, Redis, existing `internal/stepup` and `internal/middleware` helpers.

**Spec:** `docs/superpowers/specs/2026-11-14-157-account-attempt-budgets-design.md`

## Global Constraints

- Work on `feature/security-157-account-attempt-budgets`; PR references must say `Concord-Voice/Security#157`, never bare `#157` (Self-Hosted #157 is unrelated).
- Preserve current request and successful response shapes. `RecoveryBegin` stays generic `200`.
- Redis budget admission happens before `BeginTx`; transactional factor consumption stays on the protected SQL transaction. A correct password does not clear the MFA budget.
- Exhaustion is `429` with `Retry-After`; budget dependency failure is `503`. Never log submitted codes or password/hash material.
- No dependency, migration, environment variable, configurable limit, or new UI.
- Every integration command below requires independently provisioned disposable `DATABASE_URL` and `REDIS_URL`. The test helpers' default `concord` database is forbidden. Never set private `GOCACHE`, `GOMODCACHE`, or `GOLANGCI_LINT_CACHE`.
- Hosted imports `services/**` and `docs/api/**` from Self-Hosted only after the Self-Hosted Phase 9 merge. The two repositories need separate quality gates, reviews, and merge approvals.

## File map

| File | Responsibility |
|---|---|
| `services/control-plane/internal/stepup/{budget.go,stepup.go}` | Existing MFA budget and 429/503 response contract |
| `services/control-plane/internal/auth/handlers.go` | Recovery begin/verify and registration code rotation |
| `services/control-plane/internal/auth/pending.go` | Row-locked resend policy and change-email mutation |
| `services/control-plane/internal/auth/verification.go` | Existing pending-ID attempt key and Lua reservation |
| `services/control-plane/internal/{sessions,mfa,users,ownership,dm}/*.go` | Current MFA verification call sites and post-commit clears |
| Matching `*_test.go` files | Red/green behavioral, concurrency, outage, and caller-census coverage |
| `docs/design/authenticated-step-up-budget.md`, `docs/adr/0046-account-authentication-and-recovery.md`, `docs/api/openapi.yaml`, `CHANGELOG.md` | User/operator contract shipped with the fix |

## Review Focus

1. An existing charged route submits one code: exactly one increment, never two. Task 5's cross-route table tests this.
2. A protected SQL write rolls back after a valid code: the code's transactional consumption rolls back but the budget charge stays. Task 5 tests this.
3. A recovery email send fails after another request installs a newer record: cleanup preserves the newer record. Task 3 tests compare-and-delete.
4. A pending registration lives longer than one code: its attempt key survives the original code TTL and still throttles after resend. Task 4 tests key expiry.
5. Redis fails between admission and a protected write: the write never starts and the caller reports `503`, not `403` or `429`. Tasks 2 and 5 test this.

---

### Task 1: Reproduce the reported boundaries

**Files:** Read only: `services/control-plane/internal/{auth,mfa,sessions,users,ownership,dm,stepup}/`; test targets in those packages.

**Interfaces:** No new interface. This task records current behavior for the four linked VULN reproductions and enumerates every production `VerifyCode`, `VerifyCodeTx`, `VerifyMFAFactor`, and `VerifyMFAFactorTx` caller before edits.

- [ ] **Step 1: Guard the database and Redis targets.** Verify that both environment variables point to isolated services; inspect their host/database names without printing credentials. Stop if either target is shared or uncertain. Run only unit tests when no isolated targets exist.
- [ ] **Step 2: Record the caller census.** Run:

```bash
rg -n 'VerifyCode(Tx)?\(|VerifyMFAFactor(Tx)?\(|stepup\.Charge\(' services/control-plane/internal --glob '!**/*_test.go'
```

Classify each result as login/non-step-up, already charged, or missing charge. Preserve this list in the PR verification notes.

- [ ] **Step 3: Run the existing focused tests and the four Security issue exploit overlays where the Security checkout provides them.** The issue names `recovery_bruteforce_vuln016_test.go`, `stepup_vuln015_test.go`, `mfa_code_budget_vuln024_test.go`, and `mfa_no_budget_vuln039_test.go`. Use each overlay's checked-in command; capture baseline failure/pass semantics before changing code. Never interpret a skipped PostgreSQL test as proof.

### Task 2: Make the existing budget's 429 retry contract exact

**Files:** Modify `services/control-plane/internal/stepup/budget.go`, `stepup.go`; test `services/control-plane/internal/stepup/grace_test.go`.

**Interfaces:** Preserve `Charge(ctx context.Context, b Budget, userID, mfaCode string) *Error` and `DangerousActionBudget(*redis.Client) Budget`. Extend `Error` with an unexported `retryAfterSeconds int` used only by `Write`.

- [ ] **Step 1: Write a failing budget test.** With `miniredis`, charge one user `BudgetLimit` times; assert the next `Error.Status == 429`, `Error.Write` emits a positive numeric `Retry-After`, and a closed Redis client yields `503` with no exhausted flag. Add a nil-client case.
- [ ] **Step 2: Run the red test.** From `services/control-plane`, run `go test ./internal/stepup -run 'Test.*Budget.*Retry|Test.*Charge' -count=1`; expect the missing header assertion to fail.
- [ ] **Step 3: Implement the narrow response field.** On a denied `Budget.Consume`, read the key TTL; convert positive TTL to whole seconds rounded up and set that value in `Error.Write` only for budget exhaustion. A TTL read error becomes the existing budget-unavailable `503`. Keep the existing body flags and fixed five-per-fifteen-minute bound.

```go
// In stepup.Error; never serialize this field into Body.
retryAfterSeconds int

// In Error.Write, before c.JSON:
if e.retryAfterSeconds > 0 {
    c.Header("Retry-After", strconv.Itoa(e.retryAfterSeconds))
}
```
- [ ] **Step 4: Run the same test green and commit.** Commit only these files with `fix: expose step-up budget retry delay`.

### Task 3: Make email recovery account-bounded and preserve a live code

**Files:** Modify `services/control-plane/internal/auth/handlers.go`; test `services/control-plane/internal/auth/recovery_integration_test.go` and `handlers_integration_2_test.go`.

**Interfaces:** Keep `RecoveryBegin(*gin.Context)`, `RecoveryVerifyCode(*gin.Context)`, `consumeRecoveryCode(ctx, redisKey, code)`, and the current request/response shape. Add fixed `recovery_fail:` key, limit 10, window 24 hours inside `auth`; reuse `middleware.AllowUserAction`.

- [ ] **Step 1: Write red tests.** A table sends wrong codes from different client IPs, calls begin between groups, and verifies that the eleventh account attempt is `429` with `Retry-After`; closed Redis returns `503`; a correct consumed code deletes the failure key. A concurrent begin test asserts one stored record and one email call. A send-failure race test replaces the record before cleanup and asserts the replacement survives.
- [ ] **Step 2: Run the red tests.** Use isolated `DATABASE_URL` and `REDIS_URL`; from `services/control-plane`, run `go test ./internal/auth -run 'TestRecovery(AccountBudget|BeginKeepsLiveRecord|BeginSendFailure)' -count=1`. Expect counter/reset/race assertions to fail on the baseline.
- [ ] **Step 3: Add budget admission before the WATCH consume.** Normalize with `strings.ToLower(strings.TrimSpace(email))`, call `middleware.AllowUserAction(ctx,h.redis,"recovery_fail:"+email,10,24*time.Hour)` before `consumeRecoveryCode`, map `allowed == false` to `429` and Redis error to `503`. Read `TTL` for `Retry-After`; fail closed if it cannot be read. Delete only that key after a correct code was consumed, logging a clear failure without exposing the code/email.

```go
subject := strings.ToLower(strings.TrimSpace(email))
budgetKey := "recovery_fail:" + subject
allowed, err := middleware.AllowUserAction(ctx, h.redis, budgetKey, 10, 24*time.Hour)
if err != nil { c.JSON(http.StatusServiceUnavailable, gin.H{"step_up_budget_unavailable": true}); return }
if !allowed {
    ttl, ttlErr := h.redis.TTL(ctx, budgetKey).Result()
    if ttlErr != nil { c.JSON(http.StatusServiceUnavailable, gin.H{"step_up_budget_unavailable": true}); return }
    seconds := max(1, int(math.Ceil(ttl.Seconds())))
    c.Header("Retry-After", strconv.Itoa(seconds))
    c.JSON(http.StatusTooManyRequests, gin.H{"step_up_budget_exhausted": true})
    return
}
record, err := h.consumeRecoveryCode(ctx, recoveryRedisKey(subject), code)
```
- [ ] **Step 4: Make begin a single-writer operation.** Replace `Set` with `SetNX(ctx,key,data,recoveryCodeTTL)`; send only when it returns true. On send failure, compare the current value with the exact `data` installed by the winner and delete only on equality, using one existing-style Lua compare-and-delete operation so the comparison and delete are atomic. A loser returns the same generic `200` without changing record or email count.

```go
installed, err := h.redis.SetNX(ctx, key, data, recoveryCodeTTL).Result()
if err != nil || !installed { c.JSON(http.StatusOK, successMsg); return }
// On SendRecoveryCode failure, execute the Lua below with key and exact data.
```

```lua
if redis.call("GET", KEYS[1]) == ARGV[1] then
    return redis.call("DEL", KEYS[1])
end
return 0
```
- [ ] **Step 5: Run green, concurrency, and exploit checks.** Run the targeted tests from Step 2 under `-race`, existing WATCH/single-use tests, and the VULN-016 overlay against the fixed code (its exploit success assertion should fail). Commit `fix: bound recovery code guesses per account`.

### Task 4: Preserve pending-registration attempt and resend state

**Files:** Modify `services/control-plane/internal/auth/{handlers.go,pending.go,verification.go}`; test `services/control-plane/internal/auth/handlers_integration_test.go` and focused pending repository tests.

**Interfaces:** Keep `verificationAttemptsKey(pendingID string)`, `sendInitialCode`, `PendingRepo.UpdateEmail(ctx,id,newEmail) error`, and the current request bodies. Make `UpdateEmail` enforce the existing `IncrementResend` cooldown/cap under its own `FOR UPDATE` lock. Retain `RevertResend` on send failure.

- [ ] **Step 1: Write red tests.** After wrong confirms, resend and change-email; assert the attempt counter never decreases, a second change-email within `ResendCooldown` returns the current cooldown response, and `MaxResends` applies across both routes. Advance the Redis clock past `VerifyCodeTTLNew` while the pending SQL row remains live; assert the attempt key still exists and denies another guess. Test concurrent resend/change-email against one pending row.
- [ ] **Step 2: Run the red tests.** Use isolated `DATABASE_URL`/`REDIS_URL`; run `go test ./internal/auth -run 'TestRegistration(AttemptsSurviveRotation|ChangeEmailSharesResendBudget|AttemptLifetime)' -count=1`.
- [ ] **Step 3: Split rotation and terminal cleanup.** Remove the attempt-key `Del` from `sendInitialCode`. Change resend/change-email to delete only `email_verify:<pendingID>` and `test_only:<pendingID>`; keep full cleanup for confirmed/abandoned pending rows. `ConfirmRegistration` already reads `pending.GetByID`; keep that result and pass `time.Until(pending.ExpiresAt)` through `attemptsGuard` to the existing Lua reservation's TTL argument. The Lua script sets the attempt key's TTL only when first created, so a resend must extend an existing attempt key to the pending row's later expiry without changing its count.

```lua
local attempts = redis.call("INCR", KEYS[2])
local attempts_ttl = redis.call("PTTL", KEYS[2])
if attempts_ttl < tonumber(ARGV[1]) then
    redis.call("PEXPIRE", KEYS[2], ARGV[1])
end
```
- [ ] **Step 4: Bind change-email to the existing row-locked resend policy.** Under `PendingRepo.UpdateEmail`'s existing `FOR UPDATE`, read `resend_count` and `last_resend_at`, reject `ErrResendsExhausted`/`ErrResendCooldown`, then update `email`, increment `resend_count`, and set `last_resend_at = NOW()` in one transaction. Preserve uniqueness and expiry checks. Map those errors in `ChangeRegistrationEmail` to the same `429` bodies as `ResendRegistrationCode`; on email-send failure call `RevertResend` and handle its error.

```sql
UPDATE pending_registrations
   SET email = $1, resend_count = resend_count + 1, last_resend_at = NOW()
 WHERE id = $2
```
- [ ] **Step 5: Run targeted tests and commit.** Run tests from Step 2 under `-race` plus existing registration confirm/resend tests. Commit `fix: retain registration verification budgets`.

### Task 5: Charge every MFA step-up caller exactly once

**Files:** Modify `services/control-plane/internal/{sessions/handlers.go,mfa/handlers.go,users/handlers.go,ownership/handlers.go,dm/purge.go}`; test their matching `*_test.go` files and `services/control-plane/internal/mfa/inline_purpose_consumers_test.go`.

**Interfaces:** Reuse `stepup.Charge(ctx, stepup.DangerousActionBudget(redis), userID, mfaCode)`, `(*stepup.Error).Write(c)`, existing `VerifyCode`/`VerifyCodeTx`, and each package's post-commit clear. Do not add a verifier wrapper or change test fake interfaces.

- [ ] **Step 1: Write red route tests.** For each listed route, submit five wrong MFA codes with the same user, then a sixth across a different route; assert one `stepup.MFASettingsBudgetPrefix+userID` counter, `429` with `Retry-After`, and no mutation. Close Redis and assert `503` before mutation. Assert a correct password alone leaves the MFA counter. For a tx route, force rollback after accepted MFA and assert its factor remains spendable while the budget charge remains.
- [ ] **Step 2: Run red tests.** With isolated services, run focused tests in `./internal/{sessions,mfa,users,ownership,dm}`; include the issue's VULN-015, VULN-024, and VULN-039 exploit overlays, recording their baseline behavior.
- [ ] **Step 3: Fix session factor policy.** In `authenticateForRevoke`, require password first; when `hasMFA` is true require `mfaCode` too. Charge that code before `VerifyCode`; propagate `stepup.Error` status/body. Keep the existing no-factor response and password lockout path. Apply to revoke, revoke-all, and revocation-mode through their shared helper.

```go
if hasMFA {
    if e := stepup.Charge(ctx, stepup.DangerousActionBudget(h.redis), uid, mfaCode); e != nil {
        e.Write(c)
        return true
    }
    valid, err := h.mfaVerifier.VerifyCode(ctx, uid, action.purpose, mfaCode)
    if err != nil { c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgMFAVerificationFailed}); return true }
    if !valid { c.JSON(http.StatusForbidden, gin.H{"error": errMsgInvalidMFACode}); return true }
}
```
- [ ] **Step 4: Fix MFA settings omissions.** In `TOTPDisable`, charge `req.Code` before `BeginTx` and clear only after confirmed commit. In `RegenerateBackupCodes`, charge `req.Code` before its hand-rolled `MatchCodeStep`; keep its TOTP-only requirement and replay-safe update, then clear after the successful database update. Do not double-charge `openMFASettingsTx` routes already calling `allowMFASettingsStepUp`.
- [ ] **Step 5: Fix users, ownership, and DM.** Add pretransaction charge in the public `ChangePassword`/`ReplaceMyKeys` paths before they call `verifyStepUpWithLockedUser`; map `429`/`503` through their existing outer response handling. Charge before `VerifyCode` in ownership transfer/reversal and before `VerifyMFAFactor` in `verifyPurgeStepUp`. Clear only after each protected write reports confirmed success; retain charge on rollback or ambiguous commit. Preserve existing password verification and authorization order.

```go
if e := stepup.Charge(ctx, stepup.DangerousActionBudget(h.redis), userID, mfaCode); e != nil {
    if e.Cause != nil { h.log.Error("MFA budget unavailable", "error", e.Cause) }
    e.Write(c)
    return
}
// Open the existing SQL transaction only after this check.
```
- [ ] **Step 6: Add a source-aware caller census.** Extend `inline_purpose_consumers_test.go` with an AST walk of non-test production Go files for calls to `VerifyCode`, `VerifyCodeTx`, `VerifyMFAFactor`, and `VerifyMFAFactorTx`. Record a fixed allowlist of login/non-step-up call sites with path and enclosing function; for every other call, assert the owning route's pretransaction `stepup.Charge` coverage in the table from Step 1. Reject a newly discovered caller until classified and tested.
- [ ] **Step 7: Run green and commit.** Run all focused route tests and exploit overlays against the fixed code; the exploit-success assertions must fail and retained regression assertions must pass. Commit `fix: share MFA step-up guess budget across routes`.

### Task 6: Ship contract docs and validate both repositories

**Files:** Modify `docs/design/authenticated-step-up-budget.md`, `docs/adr/0046-account-authentication-and-recovery.md`, `docs/api/openapi.yaml`, `CHANGELOG.md`; later Hosted mirror PR modifies only Hosted-owned docs/changelog plus concrete import conflicts.

**Interfaces:** No new runtime API. Document the new `429`/`503` response flags and `Retry-After` on every affected operation.

- [ ] **Step 1: Update docs with exact behavior.** Add the route census, shared MFA key and clear rules, recovery 10/24h account budget, ten-minute live-record rule, registration pending-lifetime budget, and Redis degradation policy. Add OpenAPI `429`/`503` entries only for operations whose behavior changed. Add one `## [Unreleased]` changelog entry in Self-Hosted.
- [ ] **Step 2: Run static and local gates.** From the repository root run `./scripts/update-claude-md-counts.sh` and the repository's `/verify-quality-gate` skill. Use isolated `DATABASE_URL`/`REDIS_URL` for every destructive Go integration test. Run `git diff --check`, inspect the changed-file list against Task 1's caller census, and verify new-code coverage ≥80%, zero new blocker/critical issues, and zero new high-confidence security hotspots. Do not call an environment-only skip a pass.
- [ ] **Step 3: Commit docs and gate repairs.** Commit `docs: record account verification budgets` and only concrete gate repairs. Preserve hooks and protected-path approval.
- [ ] **Step 4: Deliver and verify Self-Hosted.** Open a draft PR referencing `Concord-Voice/Security#157` without a bare close keyword, complete the required CI, bot arrival, specialist security and red-team reviews, exact-head disposition, ready step, and the per-PR Phase 9 merge approval. After merge, watch the exact merge commit's post-merge workflows to terminal state.
- [ ] **Step 5: Verify the Hosted mirror.** Wait for `.github/workflows/upstream-import.yml` to create the Hosted mirror PR; inspect its exact imported `services/**` and `docs/api/**` diffs, resolve concrete Hosted drift, add Hosted's `## [Unreleased]` line, and run Hosted's quality/security/review gates. Obtain separate Phase 9 approval before Hosted merge; verify its post-merge workflows. Do not claim Security #157 complete until both repo results and the four exploit regressions are recorded.

## Self-review record

- Spec coverage: MFA budget and session factor-AND (Task 5), recovery account budget and live record (Task 3), registration state (Task 4), retry/error contract (Task 2), docs, both repo gates and mirror (Task 6).
- Interface consistency: existing `stepup.Charge`, `DangerousActionBudget`, `VerifyCode`, and `VerifyCodeTx` remain unchanged; the only proposed new response field is inside `stepup.Error`.
- Scope: no new limiter framework, schema, dependency, code length, notification system, or client UI. Security #82 and #118 remain open until their separate acceptance criteria are reconciled with merged evidence.
