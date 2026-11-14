# IPv6 Aggregation and Fail-Closed Pre-Auth Budgets Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make every pre-auth source budget aggregate native IPv6 at /64 and make authentication gates reject requests that Redis cannot count.

**Architecture:** Security#162 first authenticates the observed client address. One Go normalization helper then owns every control/admin Redis key, a matching media helper owns the handshake key, and nginx uses its documented binary `/64` map for request and connection zones. Public auth routes and post-admission auth-failure recording share one fixed fail-closed 503 contract.

**Tech Stack:** Go 1.25, Gin, go-redis, Node 26, TypeScript, Socket.IO/Engine.IO, `ipaddr.js` 1.9.1, nginx, Vitest, testify.

**Spec:** `docs/superpowers/specs/2026-11-14-163-ipv6-aggregation-budgets-design.md`

## Global Constraints

- Rebase on the merged Self-Hosted Security#162 commit before runtime implementation; do not accept a bucket derived from a client-selected forwarding header.
- IPv4 remains /32; IPv4-mapped IPv6 maps to that same IPv4; native IPv6 maps to /64; invalid/empty input maps to the fixed `unattributed` identity.
- Keep existing thresholds, windows, Redis prefixes, media store cap, and callback contract.
- Do not add the optional /48 budget or a migration.
- Never log or return a source address, bucket, Redis key, or malformed proxy entry.
- Use the default shared Go caches. Integration tests require a pre-provisioned isolated `DATABASE_URL`; never run them against the shared `concord` database.
- Self-Hosted merges first. Hosted is a commit-based reviewed mirror with independent quality, review, CI, and merge approval.
- Current dependency references: Context7 `/websites/nginx_en` and `/whitequark/ipaddr.js`; nginx.org mailing-list `/64` example linked in the spec.

## Review Focus

- A scoped IPv6 address and the same address without a zone must share a /64; Task 1 pins zone removal.
- A bare `RecordAuthFailure` call can overwrite the backend 503 with a 401/403; Task 2's AST census rejects unguarded calls.
- An all-invalid `TRUSTED_PROXIES` list is non-empty text but zero valid rules; Task 3 proves required production mode still refuses startup.
- The nginx map must preserve four binary IPv4 bytes while truncating IPv6 to eight; Task 4 proves both in a real nginx process.
- The Hosted import may have different nginx/config context; Task 6 applies the source commit instead of manually rewriting shared code and reruns every gate.

## File map

| Unit | Files | Responsibility |
|---|---|---|
| Go source bucket | `internal/middleware/ip_bucket.go`, tests | Canonical /32-/64 identity and unattributed sentinel |
| Go consumers | `ratelimit.go`, `authban.go`, `admin/lockout.go`, tests | Compose/hide canonical source in every Redis key |
| Auth fail closure | `authban.go`, `ratelimit.go`, `api/router.go`, auth/MFA/stepup/users callers and tests | Stop the chain or denial response when counting is unavailable |
| Media admission | `admissionGate.ts`, `index.ts`, package manifests, tests | Canonical handshake bucket and production configuration refusal |
| nginx edge | `concordvoice.conf`, new runtime fixture, blue-green nginx fixture | Binary /64 keys for request and connection zones |
| Contracts/docs | OpenAPI, architecture, quickstart, env example, changelog | Operator and client-visible behavior |
| Mirror | Hosted corresponding files | Reviewed import and Hosted-specific integration only |

---

### Task 0: Rebase on trusted-hop prerequisite and record the baseline

**Files:**
- No production edits.
- Create during execution only: `.superpowers/sdd/2026-11-14-163-ipv6-aggregation-budgets/progress.md` (ignored scratch ledger).

**Interfaces:**
- Consumes: merged Security#162 Self-Hosted head and its exact client-IP regression commands.
- Produces: a recorded base SHA and source-IP caller census used by Tasks 1–4.

- [ ] **Step 1: Verify the prerequisite.** Confirm Security#162 is closed and its Self-Hosted PR is merged. Rebase the isolated #163 branch onto that merge commit. If #162 is not merged, stop runtime implementation while keeping this plan/spec branch reviewable.
- [ ] **Step 2: Run #162's focused regressions** for control-plane trusted proxy, media `resolveClientIp`, nginx overwrite, and production config. Record commands and terminal results in the scratch ledger.
- [ ] **Step 3: Census every production source key.** `rg -n 'ratelimit:ip:|auth_ban:ip:|auth_failures:ip:|admin_lockout:ip|binary_remote_addr|counters.get\(|counters.set\(' services infrastructure/deploy/nginx` and record each owner. The final census must include rate-limit fallback keys, auth ban/failure keys, all six admin lockout keys, three nginx request zones, two nginx connection zones, and the media handshake map.
- [ ] **Step 4: Run the original exploit before the fix.** Execute Security's `ipv6_rotation_vuln185_test.go` against this exact baseline and record its expected vulnerable result. Do not copy scan files into the PR.
- [ ] **Step 5: Commit nothing.** This task is evidence and dependency admission only.

---

### Task 1: Canonical Go source bucket and every Redis key consumer

**Files:**
- Create: `services/control-plane/internal/middleware/ip_bucket.go`
- Create: `services/control-plane/internal/middleware/ip_bucket_test.go`
- Modify: `services/control-plane/internal/middleware/ratelimit.go`
- Modify: `services/control-plane/internal/middleware/ratelimit_test.go`
- Modify: `services/control-plane/internal/middleware/authban.go`
- Modify: `services/control-plane/internal/middleware/authban_test.go`
- Modify: `services/control-plane/internal/admin/lockout.go`
- Modify: `services/control-plane/internal/admin/lockout_test.go`

**Interfaces:**
- Produces: `middleware.IPBucket(raw string) string` and `middleware.ClientIPBucket(c *gin.Context) string`.
- Contract: invalid/empty → `unattributed`; IPv4/mapped IPv4 → canonical IPv4; native IPv6 → masked `/64` address without zone or `/64` suffix.
- Consumed by: rate-limit IP/fallback keys, auth failure/ban/clear keys, and admin lockout hashing.

- [ ] **Step 1: Write failing `TestIPBucket` table cases.** Assert the exact table in spec §5, uppercase/compressed IPv6 canonicalization, two different zones on one address, 256 addresses inside `2001:db8:1:2::/64`, an adjacent `/64`, empty input, and malformed input. Assert `ClientIPBucket` uses the trusted `ClientIP()` result from a Gin request.
- [ ] **Step 2: Run the red test.** From `services/control-plane`: `go test ./internal/middleware -run '^TestIPBucket$' -count=1`. Expected: build failure because the helpers do not exist.
- [ ] **Step 3: Implement the two helpers.** Use only `net/netip`: `ParseAddr`, `WithZone("")`, `Unmap`, `Is4`, and `Prefix(64).Masked().Addr().String()`. Keep the sentinel a package constant used only as a key identity.
- [ ] **Step 4: Add failing consumer tests.** In middleware, send 256 distinct addresses in one /64 through `RateLimitByIP` and prove one key/counter; prove plain/mapped IPv4 share it. In authban, record across a /64 until one ban applies to another address in that /64. In admin, reserve guesses from rotating /128s in one /64 and prove the shared source axis locks; prove mapped/plain IPv4 share it. Tests assert no raw native /128 appears in Redis keys.
- [ ] **Step 5: Move normalization into key owners.** `rateLimitIPKey` calls `ClientIPBucket`; `RecordAuthFailure` and `ClearAuthFailures` bucket before their key; admin `Reserve`, `RecordCheckedFailure`, and `IsLocked` hash `middleware.IPBucket(ip)`. A stored `reservedIPHash` remains the already-hashed canonical bucket and `Reset` remains unchanged.
- [ ] **Step 6: Run focused green tests.** `go test -race ./internal/middleware ./internal/admin -run 'IPBucket|RateLimitByIP|AuthBan|Lockout' -count=1`. Expected: PASS.
- [ ] **Step 7: Run scoped lint and diff checks.** `golangci-lint run ./internal/middleware/... ./internal/admin/...`; `git diff --check`. Expected: no findings.
- [ ] **Step 8: Commit only Task 1.** `fix: aggregate source budgets by IPv6 prefix`.

---

### Task 2: Fail closed across public auth admission and failure recording

**Files:**
- Modify: `services/control-plane/internal/middleware/ratelimit.go`, `ratelimit_test.go`
- Modify: `services/control-plane/internal/middleware/authban.go`, `authban_test.go`
- Modify: `services/control-plane/internal/api/router.go`
- Create: `services/control-plane/internal/api/auth_ip_budget_wiring_test.go`
- Modify: `services/control-plane/internal/auth/handlers.go` and focused tests
- Modify: `services/control-plane/internal/mfa/handlers.go` and focused tests
- Modify: `services/control-plane/internal/stepup/password_token.go` and focused tests
- Modify: `services/control-plane/internal/users/handlers.go` and focused tests

**Interfaces:**
- Produces: `RateLimitByIPFailClosed(redis, requests, window) gin.HandlerFunc` using the existing core limiter.
- Changes: `RecordAuthFailure(c *gin.Context, redisClient *redis.Client, cfg AuthBanConfig) bool`; true means the failure was counted and the caller may write its ordinary denial, false means a fixed 503 was written and the caller must return.
- Removes: caller-supplied raw IP and separate `MarkAuthFailureOutcome` calls.

- [ ] **Step 1: Write failing middleware tests.** Change the existing Redis-down and nil-client auth-ban tests to assert 503, `Retry-After: 1`, Nightwatch degraded reason, and handler not called. Add the same assertions for `RateLimitByIPFailClosed` on INCR, TTL, and EXPIRE failures. Add `RecordAuthFailure` cases for recorded, ban-created, nil Redis, INCR failure, EXPIRE failure, and pipeline failure; backend failures return false and write the fixed 503.
- [ ] **Step 2: Run red middleware tests.** `go test ./internal/middleware -run 'AuthBan|RateLimitByIPFailClosed|RecordAuthFailure' -count=1`. Expected: old fail-open assertions or missing symbols fail.
- [ ] **Step 3: Reuse the existing limiter core.** Add the thin fail-closed constructor. In the shared backend-error writer set `Retry-After: 1` before the existing fixed 503. Make `AuthBanCheck` call the same path for nil Redis and TTL errors. Refactor `RecordAuthFailure` to own `c.Request.Context()`, `ClientIPBucket(c)`, Nightwatch outcome, and backend 503; retain one private outcome helper only if tests need to isolate Redis state transitions.
- [ ] **Step 4: Write the route wiring test.** Parse `router.go` and assert every `authRoutes` per-IP middleware call is `RateLimitByIPFailClosed`; assert no `RateLimitByIP(` remains inside that group. Mount `/api/v1/auth/login` with the production middleware over a closed Redis server and a handler that flips a boolean; assert 503, `Retry-After: 1`, and false.
- [ ] **Step 5: Write caller regressions first.** For each production package calling `RecordAuthFailure`, force its Redis write to fail after the request is admitted and assert the final response remains 503, never 401/403, and no success/session/key side effect occurs. Add an AST census test listing every production call and rejecting a call whose boolean result is a bare expression or is not used to return before an ordinary denial.
- [ ] **Step 6: Update all callers.** Replace the raw context/IP call plus `MarkAuthFailureOutcome` with `if !middleware.RecordAuthFailure(c, h.redis, cfg) { return }` (or `return false` in boolean helpers). Preserve domain security-event ordering without logging the source.
- [ ] **Step 7: Switch every `/api/v1/auth/*` IP limiter** in `router.go` to the new fail-closed constructor. Keep thresholds, windows, middleware order, test-mode relaxation, and route handlers unchanged.
- [ ] **Step 8: Run focused suites.** With an isolated database where required: `go test -race ./internal/middleware ./internal/api ./internal/auth ./internal/mfa ./internal/stepup ./internal/users -run 'AuthBan|IPBudget|RecordAuthFailure|BackendUnavailable|RedisDown' -count=1`. Expected: PASS.
- [ ] **Step 9: Run lint and check.** `golangci-lint run ./internal/middleware/... ./internal/api/... ./internal/auth/... ./internal/mfa/... ./internal/stepup/... ./internal/users/...`; `git diff --check`.
- [ ] **Step 10: Commit only Task 2.** `fix: fail closed when auth source budgets are unavailable`.

---

### Task 3: Aggregate media handshakes and refuse missing production configuration

**Files:**
- Modify: `services/media-plane/package.json`
- Modify: `services/media-plane/package-lock.json`
- Modify: `services/media-plane/src/lib/admissionGate.ts`
- Modify: `services/media-plane/src/index.ts`
- Modify: `services/media-plane/tests/admissionGate.test.ts`
- Modify: `services/media-plane/.env.example`

**Interfaces:**
- Produces: `bucketClientIp(address: string): string` beside the gate and `AdmissionGateOptions.required?: boolean`.
- Consumes: `ipaddr.js` 1.9.1 `process`, `toByteArray`, and `fromByteArray`, documented through Context7 `/whitequark/ipaddr.js`.
- Runtime: `index.ts` passes `required: config.environment === 'production'`.

- [ ] **Step 1: Declare the existing transitive runtime package directly.** Add exact `ipaddr.js: "1.9.1"` to media-plane dependencies and update the lockfile with the repository's pinned npm. Add a type declaration only if the locked package lacks usable TypeScript types; do not add a wrapper dependency.
- [ ] **Step 2: Write failing bucket tests.** Exact IPv4/mapped/native IPv6 cases from spec §5; 256 addresses inside one /64 produce one string; adjacent /64 differs. Assert malformed input throws only in direct helper tests—the gate calls it only after `resolveClientIp` validation.
- [ ] **Step 3: Write failing gate tests.** Rotating /128s consume one 60-request budget and request 61 is denied; mapped/plain IPv4 share it; trusted peer without headers is counted under the peer and eventually denied; peerless request is denied; `required: true` throws for empty, blank, all-invalid, and only-`/0` rules; `required: false` keeps the one-shot inert warning. Preserve all existing untrusted-peer and 10,000-entry cap cases.
- [ ] **Step 4: Run red tests.** From `services/media-plane`: `npm test -- --run tests/admissionGate.test.ts`. Expected: missing helper/options and old fail-open cases fail.
- [ ] **Step 5: Implement the minimum media change.** `process` mapped IPv4; for IPv6 zero bytes 8–15 and reconstruct/canonicalize. Key the map on the bucket. Change trusted-peer missing/invalid-header fallback from null to observed peer. Reject only a genuinely absent peer. After proxy compilation, throw a fixed no-address error when `required` and size is zero; retain the inert branch otherwise. Pass the production boolean in `index.ts` and update stale comments/env example.
- [ ] **Step 6: Run verification.** `npm test -- --run tests/admissionGate.test.ts`; `npm run typecheck`; `npm run lint`; `npm run build`. Expected: PASS/no findings.
- [ ] **Step 7: Audit dependencies.** `npm audit --omit=dev`; no new vulnerability may be introduced by making the already-locked package direct.
- [ ] **Step 8: Commit only Task 3.** `fix: aggregate and close media handshake admission`.

---

### Task 4: Aggregate nginx request and connection limits with runtime proof

**Files:**
- Modify: `infrastructure/deploy/nginx/concordvoice.conf`
- Create: `infrastructure/deploy/tests/test-nginx-ip-bucket.sh`
- Modify: `infrastructure/deploy/tests/test-control-plane-bluegreen-nginx.sh` if it copies standalone zone declarations
- Modify: the deploy-test workflow only if the existing job does not run the new fixture

**Interfaces:**
- Produces: nginx variable `$source_addr_bucket` from `$binary_remote_addr` via `"~^(\C{8})"`.
- Consumed by: zones `api`, `auth`, `ws_handshake`, `addr`, and `turn_addr`.

- [ ] **Step 1: Write a failing static contract test.** Assert one map exists, all five zones key on `$source_addr_bucket`, and no `limit_req_zone`/`limit_conn_zone` remains keyed directly on `$binary_remote_addr`. Assert the blue-green fixture uses the same map instead of carrying a stale test-only zone.
- [ ] **Step 2: Write the real-nginx fixture.** Use the existing isolated fixture style and disposable ports. Trust only loopback for a test-only real-IP header so requests can synthesize source addresses without host network mutation. Configure one tiny request zone from the production map. Prove three addresses inside `2001:db8:1:2::/64` share one budget and the over-budget response fires; two adjacent IPv4 addresses each receive their own first request; mapped/plain IPv4 share a budget. Include a negative control using `$binary_remote_addr` directly that permits the rotating IPv6 request, proving the oracle detects the defect.
- [ ] **Step 3: Run red.** `bash infrastructure/deploy/tests/test-nginx-ip-bucket.sh`. Expected: static or runtime shared-/64 assertion fails on the current config.
- [ ] **Step 4: Add the documented map and replace all five zone keys.** Do not change rates, zone sizes, bursts, status codes, or server blocks.
- [ ] **Step 5: Run nginx verification.** New fixture PASS; existing `bash infrastructure/deploy/tests/test-control-plane-bluegreen-nginx.sh` PASS or its documented local skip; the CI job must execute both with nginx present. Run the repository's deploy/config tests selected by the changed-file relevance manifest.
- [ ] **Step 6: Run shell/static checks.** `shellcheck infrastructure/deploy/tests/test-nginx-ip-bucket.sh`; `git diff --check`.
- [ ] **Step 7: Commit only Task 4.** `fix: aggregate nginx source limits by IPv6 prefix`.

---

### Task 5: API contract, operator docs, exploit proof, and Self-Hosted gate

**Files:**
- Modify: `docs/api/openapi.yaml`
- Modify: `docs/architecture.md`
- Modify: `docs/runbooks/selfhost-quickstart.md`
- Modify: `CHANGELOG.md`
- Modify: any generated count file only if `./scripts/update-claude-md-counts.sh` reports a required change

**Interfaces:**
- Consumes: Tasks 1–4 exact behavior.
- Produces: public 503/`Retry-After` contract, trusted-proxy requirement, rate-limit keying/degradation documentation, and retained exploit evidence.

- [ ] **Step 1: Update OpenAPI.** Define/reuse one fixed auth rate-limit-backend-unavailable response with 503, `Retry-After`, and the exact JSON body; reference it from every affected `/auth` operation. Preserve existing 429 responses.
- [ ] **Step 2: Update architecture and quickstart.** Document /32-/64 keying, the `unattributed` bucket, auth Redis fail-closed behavior, the #162-before-#163 trust order, required production `TRUSTED_PROXIES`, the media missing-header fallback, and no /48 tier. Do not state that source merge proves Hosted or live deployment.
- [ ] **Step 3: Add one Unreleased changelog entry** covering IPv6 aggregation and fail-closed auth/media behavior.
- [ ] **Step 4: Validate docs.** Parse OpenAPI with the repository contract test and run pre-commit on the changed docs. `git diff --check` must pass.
- [ ] **Step 5: Rerun Security's original exploit** `ipv6_rotation_vuln185_test.go` against the exact head. Record that the old exploit expectation fails and the retained positive regressions pass; do not describe an unrun overlay as fixed.
- [ ] **Step 6: Run the Phase-4 count probe.** `./scripts/update-claude-md-counts.sh`; inspect and commit only real count drift.
- [ ] **Step 7: Run the configured quality gate.** If `.claude/repo-config.json` has no executable quality checks, record the gate as unconfigured and run the concrete Go lint/tests, media lint/typecheck/tests/build, nginx fixture, docs checks, and ≥80% new-code coverage report. Do not label an unconfigured skill PASS.
- [ ] **Step 8: Dispatch security validation.** Red team the rotating-/128, malformed/missing-source, Redis-failure, and shared-proxy-DoS paths; run security, code, test-quality, docs, scope, deployment-config, and Socket.IO reviews. Resolve every concrete blocker/major finding and repeat the relevant tests.
- [ ] **Step 9: Commit docs.** `docs: document prefix-based pre-auth budgets`.
- [ ] **Step 10: Create the Self-Hosted PR and wait for exact-head gates.** Include Security#163 linkage, #162 prerequisite SHA, scan evidence, local commands, quality-gate configuration status, and mirror sequencing. Wait for configured bots and CodeRabbit windows. Phase 9 still requires the user's explicit merge approval for this PR.

---

### Task 6: Reviewed Hosted mirror

**Files:**
- Apply the merged Self-Hosted commit(s) to the corresponding Hosted imported paths.
- Resolve only Hosted-specific nginx/config/docs/test differences found during import.

**Interfaces:**
- Consumes: merged Self-Hosted #163 commits and import mechanism.
- Produces: Hosted mirror PR with the same runtime contract and independent evidence.

- [ ] **Step 1: Start from current Hosted default branch after Self-Hosted merge.** Use an isolated Hosted worktree. Run the repository's import/parity tool with the exact source commit; do not manually re-create shared service edits.
- [ ] **Step 2: Resolve Hosted-specific surfaces.** Re-run the source-key census against Hosted nginx, media config, and control-plane routes. Add only missing Hosted fixtures/docs required to prove the same contract.
- [ ] **Step 3: Run the full mirror verification.** Repeat Go, media, nginx, OpenAPI, exploit, coverage, count-probe, security, and specialist review evidence on the Hosted head.
- [ ] **Step 4: Create the Hosted mirror PR.** Link Security#163 and the merged source PR/SHA. State exact import deviations, if any. Wait for independent CI, review threads, bot windows, and CodeRabbit.
- [ ] **Step 5: Stop at Phase 9.** Ask for explicit merge approval for the Hosted PR separately. Source approval or merge never authorizes the mirror merge.

## Self-review result

- Spec coverage: every finding and acceptance criterion maps to Tasks 1–6; #162 authenticity is an explicit Task-0 gate.
- Type consistency: Go and TypeScript interfaces are defined once above and used consistently in later tasks.
- Review focus: each of the five listed failure modes has a named test owner.
- Scope: no migration, /48 policy, distributed counter, threshold tuning, or unrelated proxy redesign.
- Proportion: the plan names decisions, signatures, tests, commands, and commits without transcribing implementation bodies.
