# Registration Verification Subject Binding Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bind every accepted registration code to one canonical pending UUID and the locked email address that promotion marks verified.

**Architecture:** Canonicalize `pending_id` at each HTTP boundary and type all verification Redis key builders on `uuid.UUID`. Carry the accepted Redis record's recipient to `PendingRepo.Promote`, compare it with the row selected `FOR UPDATE`, and conditionally delete only the stale record that was accepted. Invalidate code material inside the row-locked change-email operation before its SQL mutation, and return resend's recipient from the same row lock that reserves the resend.

**Tech Stack:** Go, Gin, PostgreSQL, Redis/Lua, `google/uuid`, Testify, existing `stmthook` and test server helpers.

**Spec:** `docs/superpowers/specs/2026-11-14-160-registration-code-binding-design.md`

## Global Constraints

- Rebase on the merged Self-Hosted Security#157 source PR before editing; its attempt-key lifetime and resend reservation are prerequisites.
- Self-Hosted is the source. Do not edit Hosted's mirrored `services/**` or `docs/api/**` until the Self-Hosted PR is merged and scheduled for import.
- Use only the existing lower-case-plus-trim email normalization rule.
- Preserve the public registration request/response shapes and existing success statuses.
- Preserve #157's pending-lifetime `email_verify_attempts:<canonical-id>` key through resend, email change, stale-code mismatch and code expiry.
- Only successful promotion, explicit abandonment, or terminal pending cleanup may clear the attempt key.
- Use a pre-provisioned disposable `DATABASE_URL` and `REDIS_URL`; never test against shared `concord`.
- Use the shared default Go caches. Do not set `GOCACHE`, `GOMODCACHE`, or `GOLANGCI_LINT_CACHE`.
- No migration, dependency, feature flag, route, new email normalization policy, or unrelated registration redesign.
- Every task is reviewed before the next begins. Security review and red-team review do not replace each other.

## Review Focus

1. Upper-, lower- and mixed-case UUID text must reach one SQL row, code key, test-only key and attempt key.
2. A code accepted before a row lock must not promote an email committed by a racing change-email request.
3. Mismatch cleanup must not delete a newer code and must not reset #157's attempt budget.
4. A Redis invalidation error must leave the pending email and resend reservation unchanged.
5. No log or error body may disclose the full pending capability or the new row email.

## File map

| Unit | Files | Boundary |
|---|---|---|
| Canonical identity | `services/control-plane/internal/auth/verification.go`, `handlers.go`; auth key tests; `internal/testhelpers/testserver.go` | Raw client strings stop at HTTP/test-helper boundaries; verification keys accept `uuid.UUID` |
| Proof binding | `services/control-plane/internal/auth/handlers.go`, `pending.go`; auth integration tests | Accepted Redis recipient is compared inside `Promote`'s locked transaction |
| Mutation ordering | `services/control-plane/internal/auth/pending.go`, `handlers.go`; pending/handler integration tests | Change-email invalidates before SQL mutation; resend returns locked email |
| Capability logging | `services/control-plane/internal/auth/handlers.go`; focused log test | No full `pending_id` reaches logs |
| Contract/docs | `docs/api/openapi.yaml`, `docs/adr/0046-account-authentication-and-recovery.md`, `CHANGELOG.md` | Existing API shapes, clarified security semantics |
| Mirror | matching paths in `Concord-Voice/Hosted` | Exact source import after Self-Hosted merge, independent gate |

---

### Task 0: Rebase, census, and preserve the #157 baseline

**Files:** Read only.

- [ ] **Step 1: Rebase the isolated #160 worktree on the exact merged Self-Hosted #157 commit.** Record both SHAs in the task ledger. Stop if #157 is not merged; do not reimplement its changes in this branch.
- [ ] **Step 2: Census every production and test caller.** From `services/control-plane`, run:

  ```bash
  rg -n 'redisKey\(|verificationAttemptsKey\(|test_only:|FetchVerificationCode|attemptsGuard\(|sendInitialCode\(|clearVerificationCode|IncrementResend\(|UpdateEmail\(|Promote\(' internal pkg cmd --glob '*.go'
  ```

  Record each caller under the unit in the file map. Include direct test key construction.
- [ ] **Step 3: Run the focused #157 baseline against isolated services.** From `services/control-plane` with explicit disposable URLs:

  ```bash
  go test -race ./internal/auth -run 'TestRegistrationAttempt|TestRegistrationAttempts|TestRecovery' -count=1 -p=1
  ```

  **Expected:** PASS. If it fails, classify the failure before editing #160.
- [ ] **Step 4: Run the current VULN-004/VULN-006 overlays or reconstruct them from `Concord-Voice/Security` `origin/main` into a temporary directory.** The component and forced E2E exploit expectations should demonstrate the vulnerable behavior on the baseline. An unavailable disposable service is `NOT-RUN-ENV`, not a fixed result.
- [ ] **Step 5: Record the predicted Phase-8 reviewers and count probe.** Expected reviewers are red-team, security, code, scope, docs, test analyzer and silent-failure hunter. Phase 4 must run `./scripts/update-claude-md-counts.sh`; route/package/migration counts are expected unchanged.

No commit for this read-only task.

---

### Task 1: Canonicalize the pending UUID and verification key namespace

**Files:**

- Modify: `services/control-plane/internal/auth/verification.go`
- Modify: `services/control-plane/internal/auth/handlers.go`
- Modify: `services/control-plane/internal/testhelpers/testserver.go`
- Modify: `services/control-plane/internal/auth/verification_unit_test.go`
- Modify: `services/control-plane/internal/auth/verification_test.go`
- Modify: `services/control-plane/internal/auth/verification_attempts_test.go`
- Modify as found by census: auth integration/coverage tests that construct verification keys directly

**Interfaces:** `redisKey(uuid.UUID)`, `verificationAttemptsKey(uuid.UUID)`, and `testVerificationCodeKey(uuid.UUID)` are the only key constructors. HTTP handlers parse once and pass `pendingID.String()` to SQL repository methods.

- [ ] **Step 1: Write failing table tests.** For one UUID, feed lower-, upper- and mixed-case strings through `uuid.Parse`; assert all typed key builders return exactly the same three keys. Add an invalid-ID case for each registration endpoint and assert existing `400` behavior. Update `FetchVerificationCode` tests so a case variant reads the canonical test-only key.
- [ ] **Step 2: Run the focused tests.**

  ```bash
  go test ./internal/auth ./internal/testhelpers -run 'Test.*(RedisKey|PendingID|VerificationCode)' -count=1 -p=1
  ```

  **Expected:** FAIL because key builders still accept raw strings and handler binding preserves the submitted spelling.
- [ ] **Step 3: Implement the minimal canonical boundary.** Change the three key helpers to accept `uuid.UUID`. Parse once in confirm, resend, change-email and abandon; use `parsed.String()` for repository calls. Parse the server-generated registration ID before its first code write and use the existing pending-row cleanup on the impossible malformed-ID error. Replace every production direct `test_only:` concatenation with `testVerificationCodeKey`.
- [ ] **Step 4: Update only callers broken by the type change.** Tests should parse fixture strings once. Keep `FetchVerificationCode(t, ts, pendingID string)` for its many callers, but parse and canonicalize inside it and fail the test on invalid input.
- [ ] **Step 5: Re-run the focused test, then the auth package.**

  ```bash
  go test ./internal/auth ./internal/testhelpers -run 'Test.*(RedisKey|PendingID|VerificationCode)' -count=1 -p=1
  go test -race ./internal/auth -count=1 -p=1
  ```

  **Expected:** PASS with isolated service URLs.
- [ ] **Step 6: Run `git diff --check`, review the caller census, and commit.**

  ```bash
  git add services/control-plane/internal/auth services/control-plane/internal/testhelpers/testserver.go
  git commit -m 'fix(auth): canonicalize pending registration ids'
  ```

---

### Task 2: Bind the accepted code recipient inside promotion

**Files:**

- Modify: `services/control-plane/internal/auth/handlers.go`
- Modify: `services/control-plane/internal/auth/pending.go`
- Modify: `services/control-plane/internal/auth/handlers_integration_test.go`
- Modify: `services/control-plane/internal/auth/pending_integration_test.go`
- Modify or create focused internal auth test for conditional Redis cleanup

**Interfaces:** `attemptsGuard` returns `registrationCodeProof{email, raw, attempts}` on success. `PendingRepo.Promote(ctx, canonicalID, expectedEmail)` compares normalized emails after `FOR UPDATE`. `deleteVerificationMaterialIfCurrent` atomically deletes the code and test-only keys only when the code record still equals `proof.raw`.

- [ ] **Step 1: Write the repository red test.** Insert a pending row for B, call `Promote(id, A)`, and assert the new `ErrVerificationEmailChanged`, no `users`, `user_keys` or `public_keys` row, and the pending row remains.
- [ ] **Step 2: Write the forced-interleaving red test.** Use the existing `stmthook` seam: accept code A, commit the real email change to B immediately before `Promote`'s `SELECT ... FOR UPDATE`, then assert confirm returns the generic `401 invalid_code`, no session/user is minted, and B is not marked verified. Assert the response does not contain A or B.
- [ ] **Step 3: Write cleanup ownership red tests.** After accepting record A, replace the Redis record/test-only value with B before mismatch cleanup. Assert B remains. In the no-replacement case assert A is removed. In both cases assert the #157 attempt key and its TTL remain.
- [ ] **Step 4: Run the focused tests.**

  ```bash
  go test ./internal/auth -run 'Test.*(Promote.*Email|ChangeEmailRace|VerificationMaterialIfCurrent)' -count=1 -p=1
  ```

  **Expected:** FAIL: `Promote` has no expected-email argument and verification returns only a boolean.
- [ ] **Step 5: Implement the proof value and locked comparison.** Return the parsed email, exact raw record and charged attempt count after constant-time code comparison. Normalize the expected email with the existing lower-case/trim rule. Compare inside `Promote` after the locked row read and expiry check, before any insert.
- [ ] **Step 6: Add exact-record cleanup.** Use one small Lua compare-and-delete over the code and test-only keys. Preserve the attempt key. Check and log cleanup errors without the pending ID, email, code or raw record. Map the mismatch to the existing `401 invalid_code` body with remaining attempts.
- [ ] **Step 7: Run focused and full auth race tests.**

  ```bash
  go test -race ./internal/auth -run 'Test.*(Promote.*Email|ChangeEmailRace|VerificationMaterialIfCurrent)' -count=1 -p=1
  go test -race ./internal/auth -count=1 -p=1
  ```

  **Expected:** PASS against isolated services.
- [ ] **Step 8: Review transaction outcomes and commit.** Confirm every mismatch exits before all three inserts and no error path claims success.

  ```bash
  git add services/control-plane/internal/auth
  git commit -m 'fix(auth): bind registration codes to recipient email'
  ```

---

### Task 3: Order change-email invalidation and resend recipient selection

**Files:**

- Modify: `services/control-plane/internal/auth/pending.go`
- Modify: `services/control-plane/internal/auth/handlers.go`
- Modify: `services/control-plane/internal/auth/pending_integration_test.go`
- Modify: `services/control-plane/internal/auth/handlers_integration_test.go`

**Interfaces:** the package-internal row-locked email update accepts one pre-update invalidation callback; production supplies code-material deletion. `IncrementResend` returns `(count, reservedAt, email, error)` from its locked transaction.

- [ ] **Step 1: Write the change-email red test.** Force the invalidation callback to fail. Assert the pending email, resend count and timestamp remain unchanged. Add a successful ordering probe that records invalidation before the SQL update executes.
- [ ] **Step 2: Write the resend red test.** Arrange a row email change around the resend lock. Assert the returned recipient is the email selected by the transaction that reserved the resend, and the handler's email service receives that exact address. Assert there is no earlier unlocked `GetByID` result in the path.
- [ ] **Step 3: Run the focused tests.**

  ```bash
  go test ./internal/auth -run 'Test.*(UpdateEmail.*Invalidat|IncrementResend.*Email|Resend.*LockedEmail)' -count=1 -p=1
  ```

  **Expected:** FAIL because invalidation currently follows the committed email update and resend returns no email.
- [ ] **Step 4: Implement the internal pre-update callback.** Invoke it only after row lock, expiry, budget and uniqueness checks succeed and immediately before the `UPDATE`. Return its error so the SQL transaction rolls back. The production callback deletes code and test-only material for the canonical ID while preserving attempts. Keep an exported repository wrapper with no callback only where existing direct tests require it.
- [ ] **Step 5: Return email from `IncrementResend`.** Select it under the row lock and return it with the reservation. Remove the handler's unlocked `GetByID` lookup and use the returned email for `sendInitialCode`.
- [ ] **Step 6: Re-run focused, auth race and lint checks.**

  ```bash
  go test -race ./internal/auth -run 'Test.*(UpdateEmail.*Invalidat|IncrementResend.*Email|Resend.*LockedEmail)' -count=1 -p=1
  go test -race ./internal/auth -count=1 -p=1
  golangci-lint run ./internal/auth/... ./internal/testhelpers/...
  ```

  **Expected:** PASS and zero lint issues.
- [ ] **Step 7: Commit.**

  ```bash
  git add services/control-plane/internal/auth
  git commit -m 'fix(auth): serialize registration email code rotation'
  ```

---

### Task 4: Remove the pending capability from logs

**Files:**

- Modify: `services/control-plane/internal/auth/handlers.go`
- Modify or create: focused auth log-capture test

- [ ] **Step 1: Write the red log test.** Capture a successful register event and assert the returned full `pending_id` is absent from every field and rendered message. Keep an assertion that the fixed event name remains.
- [ ] **Step 2: Run it.**

  ```bash
  go test ./internal/auth -run 'Test.*Registration.*Log' -count=1 -p=1
  ```

  **Expected:** FAIL because `Pending registration created` currently includes `pending_id`.
- [ ] **Step 3: Remove only the capability field.** Do not add a prefix, hash, correlation layer or new telemetry.
- [ ] **Step 4: Re-run the focused test and `rg` the package's production log calls for pending IDs.**

  ```bash
  go test ./internal/auth -run 'Test.*Registration.*Log' -count=1 -p=1
  rg -n -U 'log\.(Info|Warn|Error|Debug)\([^\n]*(pending|Pending)|"pending_id"' internal/auth --glob '*.go'
  ```

  **Expected:** test PASS; no production log includes a full pending ID.
- [ ] **Step 5: Commit.**

  ```bash
  git add services/control-plane/internal/auth
  git commit -m 'fix(auth): stop logging pending registration ids'
  ```

---

### Task 5: Retain exploit regressions and update the source contract

**Files:**

- Modify: `services/control-plane/internal/auth/handlers_integration_test.go`
- Modify: `services/control-plane/internal/auth/verification_attempts_test.go`
- Modify: `services/control-plane/internal/auth/pending_integration_test.go`
- Modify: `docs/api/openapi.yaml`
- Modify: `docs/adr/0046-account-authentication-and-recovery.md`
- Modify: `CHANGELOG.md`

- [ ] **Step 1: Port the VULN-004 and VULN-006 exploit logic into retained positive regressions.** The tests assert the secure outcome: no verified B, one canonical key set, no newer-code deletion, and no attempt-key reset.
- [ ] **Step 2: Run adapted temporary copies of the original exploit tests.** Adjust only compile-time calls for typed UUID/proof signatures outside the repository. Confirm the vulnerable assertions fail because the exploit is refused. Do not count a compile error, service outage or timeout as proof.
- [ ] **Step 3: Update OpenAPI descriptions, not schemas.** State that confirm binds the code to the current normalized pending email, case variants share one canonical subject, and a stale correct code receives the existing `401 invalid_code` contract. Correct the change-email description if it still says it resets the resend counter after #157.
- [ ] **Step 4: Update ADR 0046.** Add the canonical tuple and locked promotion check, #157 attempt-key preservation, code rotation ordering and source/mirror rule.
- [ ] **Step 5: Add one `## [Unreleased]` changelog line.** Describe the user-visible security correction without naming exploit details or internal key names.
- [ ] **Step 6: Run docs and contract checks.** Locate the repository's existing OpenAPI contract test and YAML parser command; run both. Run `git diff --check`.
- [ ] **Step 7: Commit.**

  ```bash
  git add services/control-plane/internal/auth docs/api/openapi.yaml docs/adr/0046-account-authentication-and-recovery.md CHANGELOG.md
  git commit -m 'test: retain registration code binding regressions'
  ```

---

### Task 6: Self-Hosted validation, adversarial review, and PR lifecycle

**Files:** No planned source edits; review findings may require focused fixes.

- [ ] **Step 1: Run the configured quality-gate skill.** If the trusted default-branch repo config has no enabled gate, record `UNVERIFIED` and run the repository's lint, vet, test and coverage commands manually. Do not claim the skill passed when it skipped.
- [ ] **Step 2: Run focused security validation against isolated services.** At minimum:

  ```bash
  go test -race ./internal/auth -count=1 -p=1
  go test -race ./internal/testhelpers -count=1 -p=1
  golangci-lint run ./internal/auth/... ./internal/testhelpers/...
  git diff --check origin/main...HEAD
  ```

- [ ] **Step 3: Run `./scripts/update-claude-md-counts.sh` as a probe.** Expected: no route, package or migration count change. Commit only a real derived count change.
- [ ] **Step 4: Run pre-PR red-team and security review separately.** Required attacks: upper/mixed UUID aliases; change-email before/after code comparison; resend/change-email overwrite orders; stale mismatch cleanup deleting a successor; Redis error during locked update; capability leakage through logs/errors.
- [ ] **Step 5: Resolve every concrete finding with one focused regression and rerun only the affected gate plus the full auth race suite.** Stop after the required review-cycle cap unless a new P0/P1 finding appears.
- [ ] **Step 6: Create the Self-Hosted PR linked to Security#160 and parent Security#151.** State the #157 dependency, exact exploit evidence, isolated database/Redis targets, quality-gate status, docs changed, and explicit no-migration/no-route result.
- [ ] **Step 7: Monitor exact-head CI, required reviews, bot arrival/disposition and CodeRabbit windows.** Do not merge or use an admin bypass without the lifecycle's separate Phase-9 user approval.

---

### Task 7: Import and independently validate the Hosted mirror

**Prerequisite:** the Self-Hosted source PR is merged. Do not begin from an unmerged feature branch.

**Files:** matching imported auth code/tests/test helper plus `docs/api/openapi.yaml`, ADR/changelog where the Hosted mirror owns them.

- [ ] **Step 1: Run the repository's scheduled/reviewed source import process for the exact merged Self-Hosted commit.** Do not manually redesign the invariant in Hosted. Record source and destination SHAs.
- [ ] **Step 2: Resolve only real repository drift.** Re-run the caller census because Hosted line numbers and adjacent auth code may differ. Preserve the canonical UUID, locked email comparison, exact-record cleanup and attempt-budget invariants.
- [ ] **Step 3: Run the same retained exploit regressions, auth race suite, lint, OpenAPI/docs checks and count probe against isolated Hosted services.** A green Self-Hosted run is not Hosted evidence.
- [ ] **Step 4: Run Hosted security, code, docs, scope and test-quality reviews at the exact head.** Dispatch additional reviewers only if the actual diff triggers them.
- [ ] **Step 5: Create and monitor the Hosted mirror PR through CI and review disposition.** Link the exact Self-Hosted source PR/commit and Security#160.
- [ ] **Step 6: Stop at Hosted Phase 9 for separate user merge approval.** Close Security#160 only after both source and mirror PRs are merged and their acceptance evidence is recorded.

## Completion evidence

The implementation is complete only when the final handoff contains:

- exact Self-Hosted and Hosted commit/PR heads;
- deterministic VULN-004 race and VULN-006 case-variant secure outcomes;
- proof that successor code and #157 attempt state survive stale cleanup;
- isolated PostgreSQL/Redis test targets and terminal test/lint results;
- quality-gate status stated as PASS or UNVERIFIED with the manual substitute;
- exact-head security/red-team/reviewer dispositions;
- docs and changelog evidence;
- separate Phase-9 merge approvals and final merged state for both repositories.
