# Security #164 Low Authentication Gaps Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close Security #164's six owned low-severity authentication gaps while verifying, but not duplicating, Security #39's refresh-machine fix.

**Architecture:** Patch the existing ownership seams as three workstreams: WebAuthn, MFA storage/input, and registration/WebSocket admission. The MFA storage workstream is deliberately split into B1 compatible-reader and B2 migration/writer PRs. Each merged source PR is then imported to Hosted through a separate reviewed mirror PR. No new framework, route, dependency, or configuration variable is added.

**Tech Stack:** Go 1.27, Gin, PostgreSQL 16, Redis/go-redis, go-webauthn v0.18.2, `golang.org/x/crypto/argon2`, OpenAPI YAML.

**Spec:** `docs/superpowers/specs/2026-11-14-164-low-authn-gaps-design.md`

## Global Constraints

- Security #39 owns VULN-009. #164 records exact-head proof and makes no duplicate refresh-machine production edit.
- Self-Hosted is source for `services/**` and `docs/api/**`; no Hosted hand-port starts before the corresponding source PR merges.
- Every integration test uses an explicitly isolated `DATABASE_URL` and `REDIS_URL`; never use the shared `concord` database.
- Keep shared default Go caches. Do not set `GOCACHE`, `GOMODCACHE`, or `GOLANGCI_LINT_CACHE`.
- Preserve #35's conditional backup-code consumption and #36's Redis `GETDEL` ceremony consumption.
- Backup v2 is exactly Argon2id v19, `m=8192,t=1,p=1`, 16-byte salt, 32-byte digest. Reject any other stored v2 tuple before hashing.
- B1 reads exact bare/v1/v2 entries but writes only bare legacy SHA-256. B2 retains those reads and switches generation to v2. No migration or v2 writer may activate while any B0 replica can access the database.
- Registration limits are: body 64 KiB, decoded RSA public key 1 KiB, decoded wrapped private key 32 KiB, decoded KDF salt 64 bytes.
- WebAuthn credential maximum remains 10.
- `recovery-only` methods are a unique subset of `totp,webauthn,email,sms` with raw length ≤4. `email-sms/setup` is a unique non-empty subset of `email,sms` with raw length ≤2.
- JWT WebSocket attestation uses the shared HTTP validator only when `REQUIRE_CLIENT_ATTESTATION` is enabled. The ticket redemption rail is unchanged.
- Each PR updates `[Prerelease]` and the contract docs it changes.
- Before writing migration files, re-resolve the next free number. `000195` Self-Hosted and `000198` Hosted are inspected-head reservations only.

## File map and PR boundaries

| PR | Files owned | Boundary |
|---|---|---|
| A — WebAuthn | `internal/mfa/handlers.go`, focused MFA tests, OpenAPI/changelog if response text changes | Clone warning and write-time cap only |
| B1 — compatible reader | `internal/mfa/totp.go`, backup-code callers/tests, architecture/changelog | Read bare/v1/v2; writer remains bare; no migration |
| B2 — storage write/migration | same reader/writer surfaces, one migration pair, method handlers/tests, `docs/architecture.md`, OpenAPI, changelog | Retain bare/v1/v2 reads; write v2; prefix legacy rows; bounded method sets |
| C — registration/WS | `internal/auth/{handlers.go,pending.go}`, `internal/middleware/attestation.go`, `internal/websocket/handler.go`, `internal/api/router.go`, focused tests, architecture/OpenAPI/changelog | Bounded registration and JWT-only attestation |
| Mirror | Same imported source/docs per merged PR; repo-local migration filename | No independent redesign |

## Review Focus

- **CloneWarning is checked after sign-count persistence.** Expected: check immediately after library success and before every DB write or authority mint. Pinned in Task 1.
- **Backup hash parser accepts attacker-sized Argon parameters from a damaged DB row.** Expected: exact parameter/length checks precede `argon2.IDKey`. Pinned in Tasks 3A and 3B.
- **The migration or v2 writer reaches an old B0 replica.** Expected: B1 reader is deployed everywhere and zero B0 is proven before B2; Hosted never uses direct rolling B0-to-B2. Pinned in Tasks 3A, 3B, and 8.
- **Credential cap is still a begin-time check.** Expected: count and insert occur after the same users-row lock. Pinned by the concurrent test in Task 2.
- **A JWT WebSocket is attested twice or a ticket is newly attested.** Expected: shared validator runs only on JWT fallback; ticket regression stays unchanged. Pinned in Task 6.
- **Registration's 413 path still reaches password hashing.** Expected: body/field bounds and cheap conflicts precede the `HashPassword` AST call site, plus runtime 413 tests. Pinned in Task 5.

---

## Dependency gate: VULN-009 / Security #39

- [ ] **Step 1: Refresh Security #39 and its implementation PR.** Record issue state, PR URL, exact head SHA, review state, and CI state. If no reviewed fix exists, mark VULN-009 blocked on #39; do not implement it on #164.

- [ ] **Step 2: Verify the exact head.** Run #39's focused missing-`X-Machine-Id` regression against isolated services and confirm a bound refresh token is rejected with no successor row/cookie. Record the command and result in #164's PR/issue evidence.

- [ ] **Step 3: Preserve ownership.** Add no #164 production or test diff under the refresh machine-binding functions unless #39's own lifecycle explicitly assigns it here.

## PR A — WebAuthn

### Task 1: Reject go-webauthn CloneWarning on both assertion rails

**Files:**
- Modify: `services/control-plane/internal/mfa/handlers.go`
- Test: `services/control-plane/internal/mfa/security_event_test.go`
- Test: `services/control-plane/internal/mfa/silent_failure_regression_test.go`
- Test: `services/control-plane/internal/mfa/handlers_test.go`

**Interfaces:**
- Consumes: go-webauthn `credential.Authenticator.CloneWarning` and `SignCount`.
- Produces: no new exported API; both assertion paths return their existing generic failure and emit `ReasonChallengeInvalid` with `AuthWebAuthn`.

- [ ] **Step 1: Confirm the pinned dependency contract.** Resolve go-webauthn v0.18.2 in Context7 or upstream source. Record that `FinishLogin*` returns an updated credential and `CloneWarning` represents a non-zero counter regression/equality warning.

- [ ] **Step 2: Write failing login/step-up tests.** Construct a successful fake WebAuthn result with `CloneWarning=true`. Assert `verifyWebAuthnChallenge` returns false, emits the WebAuthn challenge-denied event, performs no `UPDATE user_mfa_webauthn`, and produces no successful challenge/session completion.

- [ ] **Step 3: Write the failing inline test.** Assert `WebAuthnVerifyInlineFinish` returns the existing forbidden verification body, performs no sign-count update, and inserts no `step_up_tokens` row.

- [ ] **Step 4: Pin valid zero/increasing counters.** Add cases where `CloneWarning=false` and `SignCount` is zero or increases; both retain current success and persistence.

- [ ] **Step 5: Run the red tests.** Run the exact new tests with `go test -race ./internal/mfa -run '<new test alternation>' -count=1`. Expected: clone cases currently authenticate or persist.

- [ ] **Step 6: Implement the two checks.** Immediately after each `FinishLogin*` success, branch on `credential.Authenticator.CloneWarning`. Reuse the current generic response/security-event vocabulary; write no credential details or counters to logs.

- [ ] **Step 7: Run focused and package tests.** Run the new tests, then `go test -race ./internal/mfa -count=1` against isolated services. Expected: PASS.

- [ ] **Step 8: Commit the task.** Stage only the handler and focused tests. Commit: `fix(mfa): reject cloned WebAuthn assertions`.

### Task 2: Re-enforce the ten-credential cap at finish

**Files:**
- Modify: `services/control-plane/internal/mfa/handlers.go`
- Test: `services/control-plane/internal/mfa/handlers_test.go`
- Test: `services/control-plane/internal/mfa/enrollment_upgrade_test.go`
- Modify: `CHANGELOG.md`

**Interfaces:**
- Consumes: `withMFAFactorWriteTx(ctx, userID, es, write)` and its users-row lock.
- Produces: package sentinel `errWebAuthnCredentialLimit`; handler maps it to the existing 409 response.

- [ ] **Step 1: Write the failing concurrent test.** Seed nine credentials, create two independent fresh registration ceremonies, release two finish goroutines from a barrier, and assert one 200, one 409, and a final count of exactly ten. Use real isolated PostgreSQL/Redis and no sleeps.

- [ ] **Step 2: Write failure tests.** A count-query error returns 500 and inserts nothing; ten existing credentials return 409 and do not change MFA flags.

- [ ] **Step 3: Run red.** Run `go test -race ./internal/mfa -run 'TestWebAuthnRegisterFinish.*(Concurrent|Limit|Count)' -count=1`. Expected: current code can leave eleven or lacks the expected 409.

- [ ] **Step 4: Implement the transaction check.** Inside the existing `withMFAFactorWriteTx` closure, query `COUNT(*)` after the users-row lock. Return `errWebAuthnCredentialLimit` at ten; otherwise execute the existing insert. Map only that sentinel to the current 409 text.

- [ ] **Step 5: Verify.** Re-run focused tests and `go test -race ./internal/mfa -count=1`. Run `golangci-lint run ./internal/mfa/...` from `services/control-plane`. Expected: PASS / 0 issues.

- [ ] **Step 6: Docs and commit PR A.** Add one `[Prerelease]` bullet describing clone rejection and write-time cap. If response schemas are unchanged, leave OpenAPI unchanged and state why in the PR. Commit: `fix(mfa): enforce WebAuthn assertion and credential limits`.

- [ ] **Step 7: PR A gate.** Run diff check, relevant pre-commit hooks, security review, red-team PoC attempt, code/test/docs/scope review, PR CI, and CodeRabbit windows. Stop at Phase 9 for explicit merge approval.

## PR B1/B2 — MFA storage and bounded method input

### Task 3A: Land the compatible backup-code reader without changing writes

**Files:**
- Modify: `services/control-plane/internal/mfa/totp.go`
- Modify: `services/control-plane/internal/mfa/handlers.go`
- Test: `services/control-plane/internal/mfa/totp_test.go`
- Test: `services/control-plane/internal/mfa/handlers_unit_test.go`
- Test: `services/control-plane/internal/mfa/totp_step_replay_test.go`
- Modify: `docs/architecture.md`
- Modify: `CHANGELOG.md`

**Interfaces:**
- Keep: `GenerateBackupCodes() (codes []string, hashes []string, err error)`.
- Change: `VerifyBackupCode(code string, hashes []string, used []bool) (index int, matched bool, err error)`.
- Add package-private exact codec helpers; no generic password-hash wrapper.

- [ ] **Step 1: Write the B1 compatibility matrix.** Assert exact bare lowercase 64-hex, v1, and v2 fixtures verify; uppercase/short/long bare values and malformed versioned values fail closed. Correct codes match, wrong codes do not, and used indices are skipped.

- [ ] **Step 2: Write parser-abuse tests.** Malformed v1/v2, different algorithm/version, `m/t/p` changes, wrong salt/digest lengths, and very large numeric parameters return an error before calling Argon2. Keep the parser exact rather than accepting arbitrary PHC strings.

- [ ] **Step 3: Pin the B1 writer.** Assert `GenerateBackupCodes` still writes exact bare lowercase 64-hex SHA-256. This is a rollout invariant, not temporary test looseness.

- [ ] **Step 4: Write real consume-path compatibility tests.** Bare, v1, and v2 fixtures each verify once through the conditional consumption path; malformed storage authenticates none.

- [ ] **Step 5: Run red.** Run focused MFA tests. Expected: v1/v2 reads and verification-error plumbing fail while bare generation stays green.

- [ ] **Step 6: Implement the expand reader only.** Parse exact bare/v1/v2 forms. Use `argon2.IDKey` only for exact v2 with constants `8192,1,1,16,32`; normalize exactly as today and compare decoded digests with `subtle.ConstantTimeCompare`. Do not switch generation and do not add a migration.

- [ ] **Step 7: Thread verification errors.** Update every `VerifyBackupCode` caller. A malformed stored element is a dependency/storage failure and cannot authenticate. Preserve the existing conditional `backup_codes_used` update and result checks.

- [ ] **Step 8: Verify and document B1.** Run focused tests, `go test -race ./internal/mfa -count=1`, and scoped lint. Document that B1 changes reads only and must reach every replica before B2.

- [ ] **Step 9: Gate and commit B1.** Commit `fix(mfa): expand backup code hash reader`; complete the independent security/code/test/docs/scope and CI gates. Do not combine B1 with B2.

### Task 3B: After B1 adoption, migrate rows and enable v2 writes

**Additional files:**
- Create: `services/control-plane/migrations/000195_backup_code_hash_versions.up.sql` (re-resolve number)
- Create: `services/control-plane/migrations/000195_backup_code_hash_versions.down.sql` (re-resolve number)
- Modify: `services/control-plane/migrations/TEST_MIGRATIONS.md`

- [ ] **Step 1: Prove the deployment prerequisite.** For Hosted, record both blue/green pools and background workers on B1-or-newer with zero B0 replicas. For a one-shot Self-Hosted upgrade, record that all B0 processes are stopped and database-fenced. A source merge or green CI run is not this proof.

- [ ] **Step 2: Write B2 codec tests.** Assert generated entries use exact v2, all eight salts are distinct and 16 bytes, digests are 32 bytes, and the B2 reader continues accepting bare/v1/v2.

- [ ] **Step 3: Write mixed-version tests.** Model B1 and B2 readers around the migration: B1 reads migrated v1, B1 reads newly generated v2, B2 reads a late bare row, and application rollback from B2 to B1 reads all persisted forms. Assert the B1 writer never creates v2.

- [ ] **Step 4: Write migration tests.** Up prefixes every bare 64-hex array element without changing order or `backup_codes_used` and is safe to rerun. Down strips v1 under a table lock only when no v2 exists and raises when any v2 exists. Update all repository fixtures that insert backup hashes after migrations to use a reader-supported form.

- [ ] **Step 5: Implement B2.** Retain the B1 reader unchanged, switch only generation to v2, and add the migration pair. Preserve the conditional consume mutation and verification-error behavior.

- [ ] **Step 6: Verify and document.** Run focused/race MFA tests and migration tests. Document the B1/B2 order, Hosted zero-B0 gate, Self-Hosted full-stop alternative, and why application rollback uses B1 without running the destructive down migration.

- [ ] **Step 7: Commit the B2 storage portion.** Commit code/tests (`fix(mfa): salt backup code hashes`) and migration/docs as required. Task 4 lands in the same B2 PR; run the combined B2 gate only after both tasks are complete.

### Task 4: Normalize method arrays before loops and writes

**Files:**
- Modify: `services/control-plane/internal/mfa/handlers.go`
- Test: `services/control-plane/internal/mfa/handlers_test.go`
- Test: `services/control-plane/internal/mfa/settings_stepup_internal_test.go`
- Modify: `docs/api/openapi.yaml`
- Modify: `CHANGELOG.md`

**Interfaces:**
- Add: `func normalizeMethodSet(methods []string, max int, allowed map[string]struct{}) ([]string, error)`.
- `SetRecoveryOnly` consumes the normalized slice before enabled-factor filtering.
- `EmailSmsSetup` consumes and returns the normalized slice before Redis generation.

- [ ] **Step 1: Write table tests.** Cover empty, known unique, small duplicate, unknown, raw length max/+1, and order preservation for both route-specific allowed sets.

- [ ] **Step 2: Write the 1,000-entry route regression.** Assert 400, zero Redis setup keys, and zero email/code-generation calls. Add a small duplicate case that generates exactly one email code.

- [ ] **Step 3: Run red.** Run the new handler tests. Expected: current code loops/generates per duplicate or accepts oversized input.

- [ ] **Step 4: Implement one package-local normalizer.** Reject raw over-limit first, reject unknown values, and return first-seen unique values. Do not add reflection, generics, config, or a reusable validation package.

- [ ] **Step 5: Update OpenAPI.** Set `maxItems`, `uniqueItems: true`, and the exact enums for both request arrays. Document stable 400 behavior.

- [ ] **Step 6: Verify and commit.** Run focused tests, full MFA race tests, scoped lint, OpenAPI contract test, YAML parse, and diff check. Commit: `fix(mfa): bound authentication method sets`.

- [ ] **Step 7: PR B2 gate.** Run migration-reviewer, security/red-team/code/test/docs/scope review, quality gate, PR CI, and bot windows for Tasks 3B and 4 together. Stop at Phase 9 for explicit merge approval.

## PR C — Registration and WebSocket JWT attestation

### Task 5: Bound registration before allocation and password hashing

**Files:**
- Modify: `services/control-plane/internal/auth/handlers.go`
- Modify: `services/control-plane/internal/auth/pending.go`
- Test: `services/control-plane/internal/auth/handlers_integration_2_test.go`
- Test: `services/control-plane/internal/auth/handlers_coverage_test.go`
- Create or modify: `services/control-plane/internal/auth/registration_bounds_test.go`
- Modify: `docs/api/openapi.yaml`
- Modify: `CHANGELOG.md`

**Interfaces:**
- Add: `const maxRegisterRequestBytes = 64 * 1024` and exact decoded-field constants from Global Constraints.
- Add: `func bindRegisterRequest(c *gin.Context, req *RegisterRequest) error` with sentinel `errRegisterBodyTooLarge`.
- Add: `func decodeBoundedBase64(value, label string, maxDecoded int) ([]byte, error)`.
- Generalize the existing registered-user uniqueness query to accept the minimal `QueryRowContext` interface, so the handler precheck and transaction use the same root logic.

- [ ] **Step 1: Write body boundary tests.** An exact-limit JSON body reaches ordinary validation; +1 returns 413. Add trailing-value and malformed JSON cases returning 400.

- [ ] **Step 2: Write field boundary tests.** Each base64 field accepts its decoded maximum and rejects +1 before decode. Keep RSA-4096 parsing as the stricter public-key semantic check.

- [ ] **Step 3: Pin current Desktop compatibility.** Use an RSA-4096 SPKI plus an AES-GCM-sized wrapped PKCS#8 fixture and current salt shape; assert registration reaches pending creation.

- [ ] **Step 4: Pin cheap-work ordering.** Add an AST test over `Register`/its preparation helpers asserting bounded bind, cheap validation/key checks, and registered-user conflict lookup appear before `HashPassword`. Runtime 413 and confirmed-conflict cases assert no pending row or other side effect.

- [ ] **Step 5: Run red.** Run `go test -race ./internal/auth -run 'TestRegister.*(Body|Key|Cheap|Desktop|Conflict)' -count=1`. Expected: current code lacks 413/decoded caps and hashes before conflict lookup.

- [ ] **Step 6: Implement bounded binding.** Wrap before first read, decode one JSON value plus EOF, invoke existing Gin validation, tolerate unknown fields, and distinguish `*http.MaxBytesError` as 413.

- [ ] **Step 7: Implement bounded key decoding and precheck.** Use base64 `DecodedLen` before allocation, retain exact RSA-4096 validation, run the shared users uniqueness query before `HashPassword`, and repeat it inside `InsertOrTakeover` to close the race.

- [ ] **Step 8: Update OpenAPI and verify.** Document 64 KiB request cap, decoded field caps, and 413. Run focused/full auth race tests, scoped lint, OpenAPI contract test, YAML parse, and diff check.

- [ ] **Step 9: Commit.** Commit: `fix(auth): bound registration before password hashing`.

### Task 6: Apply the shared attestation validator to JWT WebSockets

**Files:**
- Modify: `services/control-plane/internal/middleware/attestation.go`
- Modify: `services/control-plane/internal/websocket/handler.go`
- Modify: `services/control-plane/internal/api/router.go`
- Test: `services/control-plane/internal/middleware/attestation_test.go`
- Test: `services/control-plane/internal/websocket/handler_test.go`
- Test: `services/control-plane/internal/api/router_*_test.go` (use the existing wiring guard file)
- Modify: `docs/api/openapi.yaml`
- Modify: `docs/architecture.md`
- Modify: `CHANGELOG.md`

**Interfaces:**
- Extract: `func ValidateAttestation(c *gin.Context, rdb *redis.Client, log *logger.Logger) bool` from the enabled branch of `RequireAttestation`.
- Extend `websocket.NewHandler` with `requireAttestation bool` and `log *logger.Logger`; store both on the handler.
- `authenticateViaJWT` sets verified `user_id` plus `JWTClaimsContextKey` carrying the server-verified `sid`, calls `ValidateAttestation` only when enabled, and returns a generic auth error if it fails. Do not add a JWT/header session-binding comparison here; Security #44 owns authoritative namespace binding.

- [ ] **Step 1: Write middleware parity tests.** Calling the extracted function directly and through `RequireAttestation(true,...)` must produce the same success, 403, and Redis-failure 503 outcomes.

- [ ] **Step 2: Write JWT handshake regressions.** With attestation enabled: valid JWT plus no token is rejected before upgrade; invalid token is rejected; Redis failure returns 503; valid bound attestation reaches upgrade/admission.

- [ ] **Step 3: Write ticket regression.** With attestation enabled, a valid already-minted WebSocket ticket follows the current ticket path without a second attestation lookup. Assert a Redis attestation fault injected only for validation cannot break ticket redemption.

- [ ] **Step 4: Write router/constructor wiring test.** Assert `cfg.RequireClientAttestation` and the project logger reach `websocket.NewHandler`, while `/ws` remains outside `authRequired` and `authenticateViaTicket` remains first in the classifier.

- [ ] **Step 5: Run red.** Run focused middleware/websocket/api tests. Expected: JWT no-attestation currently reaches WebSocket auth.

- [ ] **Step 6: Extract the existing validator without changing behavior.** The HTTP middleware's enabled branch delegates to the exported function. Preserve constant-time token comparison, revocation, user binding, fixed error bodies, and Redis fail-closed behavior.

- [ ] **Step 7: Wire JWT only.** Pass the existing flag/logger into the WebSocket handler. After access-token validation, set `user_id` and `JWTClaimsContextKey` with the verified `sid`, then call the shared validator. Do not call it in `authenticateViaTicket`, do not move `/ws` under `authRequired`, and do not preempt Security #44's namespace-binding change.

- [ ] **Step 8: Docs and verification.** Document the JWT header requirement and ticket exemption in OpenAPI/architecture. Run focused tests, `go test -race ./internal/middleware ./internal/websocket ./internal/api -count=1`, scoped lint, OpenAPI contract/YAML checks, and diff check.

- [ ] **Step 9: Commit.** Commit: `fix(websocket): enforce attestation on JWT fallback`.

- [ ] **Step 10: PR C gate.** Run security/red-team/e2ee/code/test/docs/scope review, quality gate, PR CI, and bot windows. Rebase on #44 if it lands; preserve its authoritative-sid namespace. Stop at Phase 9 for explicit merge approval.

## Task 7: Source-wide acceptance after all four PRs

- [ ] **Step 1: Run the six finding regressions plus #39 proof on the exact source heads.** Report each finding independently; an unrelated 403 does not prove its intended control.

- [ ] **Step 2: Run package and repository gates.** At minimum: affected package race tests, scoped golangci-lint, migration up/down suite, OpenAPI contract/YAML parse, changelog guard, `git diff --check`, and configured pre-commit hooks.

- [ ] **Step 3: Run the count probe when present.** `./scripts/update-claude-md-counts.sh`; this inspected checkout lacks the script/root Key Counts file, so record `NOT CONFIGURED` rather than PASS if still absent.

- [ ] **Step 4: Confirm formal gate evidence.** Each PR must show 0 new blocker/critical issues, 0 new high-confidence security hotspots, and ≥80% new-code coverage, plus its repository-required checks.

## Task 8: Reviewed Hosted mirrors and rollout gates

**Files:** Import each merged Self-Hosted PR's shared `services/control-plane/**`, `docs/api/openapi.yaml`, and applicable architecture/changelog text. Rename the migration pair to the next free Hosted number; do not change its semantics.

**Interfaces:** Source commit is the mirror input. Hosted CI/review/Phase 9 is independent evidence.

- [ ] **Step 1: Start only from the merged Self-Hosted commit.** Compare shared-file checksums/diffs and identify intentional repository-local differences before applying.

- [ ] **Step 2: Re-resolve migration number.** At the inspected Hosted head the candidate is `000198`; use the actual next free number at mirror time and update references/tests.

- [ ] **Step 3: Mirror and deploy B1 first.** The B1 mirror contains no migration and no v2 writer. Complete its independent Phase 9 approval, deploy it through both blue/green pools and all workers, and record deployment inventory showing zero B0 before B2 migration admission.

- [ ] **Step 4: Prove overlap compatibility.** In an isolated Hosted blue-green rehearsal, keep one B1 pool while introducing B2. Before/after the prefix migration, prove both pools verify bare/v1/v2 fixtures and the B1 pool can consume a code generated by B2.

- [ ] **Step 5: Admit B2 only after the gate.** Run the Hosted migration, then roll out the v2 writer. Abort if any B0 replica reappears. A B2 application rollback targets B1 and leaves the database up; do not run down after v2 exists.

- [ ] **Step 6: Run Hosted's independent focused tests, hooks, lint, migration checks, OpenAPI contract, PR CI, security/specialist reviews, and bot windows.** Never use the shared `concord` database; source green status is not mirror proof.

- [ ] **Step 7: Stop each mirror PR at Phase 9.** Obtain separate explicit merge approval. Deployment inventory and overlap proof are separate operational gates; do not infer them from merge.

## Self-review result

- Spec coverage: all seven issue findings are assigned; VULN-009 is an explicit dependency gate and six findings have implementation tasks.
- Type consistency: backup verifier, method normalizer, registration binder, and attestation validator have one signature each across tasks.
- Review focus: all five listed failure classes have a named regression in their owning task.
- Proportion: the plan specifies interfaces, tests, commands, and gates without transcribing production bodies.
- Out of scope preserved: no #35/#36/#39/#44/#53 redesign, no Desktop change, no new dependency/config/route.
