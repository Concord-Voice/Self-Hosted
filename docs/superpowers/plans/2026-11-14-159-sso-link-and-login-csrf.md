# MFA-safe SSO link and login-CSRF closure Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close Security#159 across Self-Hosted and Hosted without stranding supported Desktop SSO users.

**Architecture:** Self-Hosted defines the server contract; Hosted ports the same behavior and tests around its branch-local drift. A signed MFA completion kind and single-use pending link defer identity and session writes to one locked transaction. Desktop main binds each provider state to a per-flow secret before mandatory server enforcement; one auth-group middleware guards exactly six cookie-establishing public POST routes.

**Tech Stack:** Go, Gin, Postgres, Redis, JWT, existing singleflight; Electron/TypeScript, Vitest.

**Spec:** `docs/superpowers/specs/2026-11-14-159-sso-link-and-login-csrf-design.md` (read before execution); [Security#159](https://github.com/Concord-Voice/Security/issues/159).

## Global constraints

- No migration, new dependency, permanent compatibility switch, new IPC channel, new Zustand store, or renderer-held SSO binding secret.
- SSO binding: 32 random bytes, unpadded base64url (43 ASCII), header `X-Concord-SSO-Client-Binding`; Redis holds only `SHA-256("concord-sso-client-binding-v1\x00" || raw)`. Mandatory after the Desktop release floor; never drop this acceptance criterion.
- MFA: signed `completion_kind` is absent/`login` or `sso_link`; unknown values fail before completion. Pending `mfa_sso_link:<jti>` is `SET NX EX` for five minutes and atomically consumed after verified MFA. No client `mfa_completed` flag.
- Final link transaction locks user authority and rechecks disabled state, effective password policy, credential epoch, and current login MFA before inserting identity, login family, and refresh row. Commit precedes cookie, header, or body.
- CSRF guard is group-mounted after `AuthBanCheck`, uses `mime.ParseMediaType` and Gin full route template, and covers only the six routes listed in Task 6. Absent Origin/Fetch Metadata remains valid for native JSON clients.
- JWKS: successful HTTP 200 valid nonempty snapshots replace; every key lookup checks expiry; at most one provider-wide unknown-`kid` refresh opportunity per 60 seconds, including failed fetches.
- Keep per-IP initiate limit `10/15m`; add provider-wide fail-closed `600/min` before state/CFKV allocation. Fixed encoded/decoded E2EE maxima are `16384/12288` wrapped-key and `1024/768` salt.
- Integration tests use real isolated `DATABASE_URL` per worktree and the `REDIS_URL` backed `redistest` allocator. Never point at shared `concord`, use raw Redis `FlushDB`/`FlushAll`, or set private `GOCACHE`, `GOMODCACHE`, or `GOLANGCI_LINT_CACHE`. If shared cache access fails, report the sandbox error.
- Server commands run from the current Self-Hosted root or the Hosted root when Task 10 explicitly says so. Desktop commands use the absolute checked-out Desktop path; never run Self-Hosted and Hosted Postgres suites concurrently against one database. Test fixtures must supply valid JSON and binding headers when testing a later gate.

## File map and ordering

| Deliverable | Create / modify | Depends on |
|---|---|---|
| Server characterization | Existing `services/control-plane/internal/{oauth,auth,mfa,api,admin}/*_test.go`; scan exploit tests as source evidence | First |
| Desktop binding | `Desktop/desktop/src/main/oauth/{apple/appleFlow,google/googleFlow}.ts` and paired unit tests | Characterization |
| Desktop MFA continuation | `Desktop/desktop/src/{main/ipc/sso.ts,main/ipcContract.ts,shared/sso.ts,preload/preload.ts,renderer/services/system/ssoService.ts,renderer/components/Auth/SSOAccountLinkConfirm.tsx,renderer/stores/auth/mfaChallengeStore.ts}` and paired tests | Characterization; independent of binding implementation |
| Server MFA/link | Self-Hosted `services/control-plane/internal/{mfa/challenge.go,mfa/handlers.go,auth/oauth_adapter.go,oauth/handler.go}` and owning tests; audited MFA writer file/test only if needed | Characterization |
| Server state binding | Self-Hosted `services/control-plane/internal/{oauth/handler.go,oauth/apple_session.go}` and Apple/Google initiate/session integration tests | Desktop header contract |
| CSRF and exemptions | Create Self-Hosted `services/control-plane/internal/middleware/login_csrf{,_test}.go`; modify `api/router.go`, auth/API/admin tests | Characterization |
| JWKS and limits | Self-Hosted `oauth/{apple,google,handler}.go`, `auth/handlers.go`, owning tests | Characterization |
| Docs and gate | Self-Hosted `docs/adr/0046-account-authentication-and-recovery.md`, `docs/api/openapi.yaml`, `docs/development.md`, `CHANGELOG.md`; `docs/api/README.md` only if schema needs prose | Server behavior stable |
| Hosted mirror | Corresponding Hosted paths and tests; branch-specific policy test | Self-Hosted contract stable |
| Release | Desktop companion release, supported shell floor, staged Apple/Google proof, then server enforcement | All code/tests/docs pass |

No deletion is planned. `Desktop/` denotes the sibling repository root, not a directory inside Self-Hosted. Existing exploit files are evidence to invert in owning package tests; keep them if the scan harness supports regression execution.

## Review focus

1. A missing or malformed binding at session exchange must consume state and return the same opaque error as a mismatch: Task 5 tests it.
2. A factor enrollment/policy change while a link waits for the user lock must leave zero rows: Task 4 tests it and audits all factor writers.
3. A failed unknown-`kid` fetch must still spend the 60-second refresh opportunity: Task 7 tests it for both providers.
4. A native JSON request with no Origin or Fetch Metadata must pass the guard: Task 6 tests every guarded route.
5. A stale pending link after otherwise valid MFA must not fall through to ordinary login: Task 3 tests the dispatch and Task 4 tests zero rows.

---

### Task 0: Capture exploit behavior and route census

**Files:** Test/modify `services/control-plane/internal/oauth/handler_integration_test.go`, `services/control-plane/internal/api/oauth_wiring_test.go`, `services/control-plane/internal/auth/oauth_adapter_test.go`; inspect Security-repository `scans/2026-11-11-self-hosted-vulnhunt/exploit_tests/{sso_link_mfa_skip_vuln032_test.go,login_csrf_vuln031_test.go}` and all public `SetRefreshCookie` callers. Mirror baseline tests into Hosted only after locating its corresponding files.

**Interfaces:** Records the current HTTP shapes, route templates, writer census and DB/Redis fixtures consumed by later tasks. No production interface.

- [ ] Add two focused tests proving the old VULN-032 token/cookie/link-on-password behavior and VULN-031 simple-content cross-site acceptance on the unmodified branch. Name the exact mutation that will invert each test; assert the real rows, cookie and status, not only a mock call.
- [ ] Run `cd services/control-plane && go test -p 1 ./internal/oauth ./internal/api ./internal/auth -run 'Test.*(VULN032|VULN031|SSOCookieRouteInventory)' -count=1`; expect the characterization assertions to pass before fixes. Preserve the existing scan PoCs separately.
- [ ] Census `SetRefreshCookie` in `auth/handlers.go`, `oauth/handler.go`, `mfa/handlers.go`; pin the six guarded routes, the refresh/logout exemptions, and separate admin cookie writers in an API/admin inventory test. Assert route method plus Gin template, not literal URLs.
- [ ] Commit the red/characterization tests separately in the implementation branch; their protected expectations become the regression assertions in Tasks 4 and 6.

### Task 1: Keep Apple and Google binding secret in Desktop main

**Files:** Modify `Desktop/desktop/src/main/oauth/apple/appleFlow.ts`, `Desktop/desktop/src/main/oauth/google/googleFlow.ts`; test `Desktop/desktop/tests/unit/main/oauth/apple/appleFlow.test.ts`, `Desktop/desktop/tests/unit/main/oauth/google/googleFlow.test.ts`.

**Interfaces:** Each flow creates one 32-byte random secret per attempt, encodes it as unpadded base64url, and passes exactly the same header on initiate and session through its existing `controlPlaneFetch`. No signature or IPC shape changes.

- [ ] Write provider tests asserting 43 canonical characters, same header on the two requests, distinct values on two attempts, and absence from URL/query/body/result/log/IPC sentinel captures; cancel, timeout, error and supersession clear the flow-held value.
- [ ] Run `cd /Users/markd/.codex/worktrees/security-151-desktop/Desktop/desktop && npm test -- tests/unit/main/oauth/apple/appleFlow.test.ts tests/unit/main/oauth/google/googleFlow.test.ts`; expect the new header/custody assertions to fail.
- [ ] Add the minimum main-process secret lifecycle to both existing flows; keep PKCE, loopback, provider exchange and Concord session exchange in main.
- [ ] Rerun the same Vitest command and `cd /Users/markd/.codex/worktrees/security-151-desktop/Desktop/desktop && npm run typecheck`; expect pass, then commit only this provider-flow slice.

### Task 2: Carry complete-link MFA through the existing Desktop bridge

**Files:** Modify `Desktop/desktop/src/main/ipc/sso.ts`, `desktop/src/main/ipcContract.ts`, `desktop/src/shared/sso.ts`, `desktop/src/preload/preload.ts`, `desktop/src/renderer/services/system/ssoService.ts`, `desktop/src/renderer/components/Auth/SSOAccountLinkConfirm.tsx`, `Desktop/CHANGELOG.md`; edit `desktop/src/renderer/stores/auth/mfaChallengeStore.ts` only if its existing `sso_login` setter needs the link context. Test `Desktop/desktop/tests/unit/main/ipc/sso.test.ts`, `tests/unit/main/ipcContract.test.ts`, `tests/unit/renderer/services/ssoService.test.ts`, `tests/unit/components/Auth/SSOAccountLinkConfirm.test.tsx`, `tests/unit/stores/mfaChallengeStore.test.ts` if touched.

**Interfaces:** Widen `SSOCompletionResult` in `shared/sso.ts` with the existing public `kind: 'mfa_challenge'` fields and `credentialOwner`; `sso:completeLink` returns tokens/challenge/error; `sso:completeMFA` remains the only proof submission and refresh-token storage bridge. Set `IPC_CONTRACT_VERSION=34`; keep `SPA_MIN_CONTRACT=19` until remote SPA reliance is actually released.

- [ ] Write tests: complete-link challenge returns no refresh credential, moves pending owner from link to MFA, and a successful `sso:completeMFA` stores the refresh token under that owner. Test owner/provider/token mismatch and an error path; modal uses purpose `sso_login` without a new submission path.
- [ ] Run `cd /Users/markd/.codex/worktrees/security-151-desktop/Desktop/desktop && npm test -- tests/unit/main/ipc/sso.test.ts tests/unit/main/ipcContract.test.ts tests/unit/renderer/services/ssoService.test.ts tests/unit/components/Auth/SSOAccountLinkConfirm.test.tsx tests/unit/stores/mfaChallengeStore.test.ts`; expect new branch assertions to fail.
- [ ] Implement the union and main/preload/service/component dispatch using the existing SSO sign-in MFA path; preserve main-only refresh custody and existing IPC channel names. Bump contract to 34 and update its comment/history.
- [ ] Rerun the focused Vitest command, `cd /Users/markd/.codex/worktrees/security-151-desktop/Desktop/desktop && npm run typecheck`, and `cd /Users/markd/.codex/worktrees/security-151-desktop/Desktop/desktop && npm run build:main`; expect pass. Add an Unreleased Desktop changelog entry and run `BASE_SHA="$(git merge-base HEAD origin/main)" GITHUB_BASE_REF=main bash .github/scripts/verify-changelog.sh` from the Desktop root. Commit the companion slice.

### Task 3: Close the signed MFA completion dispatcher

**Files:** Modify `services/control-plane/internal/mfa/challenge.go`, `services/control-plane/internal/mfa/handlers.go`; test `services/control-plane/internal/mfa/challenge_test.go`, `services/control-plane/internal/mfa/handlers_test.go`, `services/control-plane/internal/mfa/login_fail_closed_test.go`; update the existing auth MFA-checker interface/fixture methods in `auth/handlers.go` and its tests only as required by the new issuer.

**Interfaces:** Add `ChallengeClaims.CompletionKind` (`completion_kind,omitempty`) with closed values `login` and `sso_link`. Keep `PurposeLogin`. Extend the existing `LoginCompleter` consumer interface with a dedicated SSO-link completion callback receiving server-validated signed claims; `auth.Handler` is the production implementation. The verifier alone dispatches after one-time claim and factor verification.

- [ ] Write tests: absent kind and `login` complete the old login path; unknown kind rejects before factor/completer work; `sso_link` reaches only the new callback after successful proof. Invalid/replayed factor and challenge never call either completer.
- [ ] Run `cd services/control-plane && go test ./internal/mfa ./internal/auth -run 'Test.*(CompletionKind|SSOLinkDispatch|LoginFailClosed)' -count=1`; expect the new assertions to fail.
- [ ] Add the signed field and explicit closed switch in `Verify`/`completeVerifyPurpose`; do not accept kind from the verify JSON request. Preserve existing challenge claims, remember-me, WebAuthn and one-time claim behavior.
- [ ] Rerun the focused command and commit this dispatcher slice. A reviewer must confirm that no `sso_link` branch can invoke ordinary `CompleteLogin`.

### Task 4: Defer link and mint atomically under final policy

**Files:** Modify `services/control-plane/internal/oauth/handler.go`, `services/control-plane/internal/auth/oauth_adapter.go`, and the existing auth/MFA checker interface seam in `auth/handlers.go`; test `oauth/handler_integration_test.go`, `auth/oauth_adapter_test.go`, `mfa/handlers_test.go`. Audit MFA factor enrollment/disable writers in `mfa/handlers.go`; edit their owning file/test only if they do not serialize with the user authority lock.

**Interfaces:** Keep `OAuth.AuthAdapter` in `oauth/handler.go` as the consumer interface; add an SSO-link-specific challenge issuer accepting target user ID, verified epoch and `auth.SSOIdentityLink`. Add a dedicated auth SSO-link completer called only after Task 3's verified callback. The existing `IssueAccessAndRefreshBound` no-proof entry refuses an MFA-enrolled target with `ErrMFARequired`; the privileged completion uses an internal, non-boolean proof path. Pending state is `mfa_sso_link:<jti>` with target ID, epoch and provider identity only.

- [ ] Write integration tests for effective password disable before challenge (Hosted stored `true` refuses; Self-Hosted's deployment override remains explicit), MFA challenge response with zero token/cookie/header and zero identity/family/refresh rows, no-MFA direct completion, and direct bound mint returning `ErrMFARequired` for enrolled MFA.
- [ ] Run `cd services/control-plane && go test -p 1 ./internal/oauth ./internal/auth ./internal/mfa -run 'Test.*(SSOLink|BoundMintMFA|PasswordLoginDisabled)' -count=1`; expect the new protected assertions to fail.
- [ ] Implement initial `CompleteLink`: consume SSO token, verify password with current lockout semantics, check effective policy, capture epoch, query login-eligible factors fail closed, then issue a five-minute signed `sso_link` challenge plus `SET NX EX` pending record. On Redis/WebAuthn transient write failure, invalidate created state and return dependency failure. No identity/session/cookie before MFA.
- [ ] Implement the final adapter transaction: acquire the same user authority lock, re-read disabled/policy/epoch/MFA, reject stale/changed state, then insert identity, family and refresh in one transaction. Map duplicate/lock/dependency errors without a cookie; emit cookie/header/body only after commit. Audit and, if needed, lock MFA enrollment/disable writers on the same row.
- [ ] Add tests for expired/missing/malformed/replayed pending JTI, signed/pending user and epoch mismatch, wrong factor, credential reset, MFA enrollment race, concurrent completions, and forced identity/family/refresh/commit failures. For every failure assert zero partial rows and no cookie; preserve correct `GETDEL` single winner.
- [ ] Rerun the focused Go command and `cd services/control-plane && go test -race -p 1 ./internal/oauth ./internal/auth ./internal/mfa -run 'Test.*(SSOLink|BoundMintMFA)' -count=1`; expect pass. Commit the policy/transaction slice.

### Task 5: Bind provider state to the initiating Desktop process

**Files:** Modify `services/control-plane/internal/oauth/handler.go`, `services/control-plane/internal/oauth/apple_session.go`; test `oauth/apple_initiate_integration_test.go`, `oauth/apple_session_integration_test.go`, `oauth/google_session_integration_test.go`, `oauth/handler_internal_test.go`.

**Interfaces:** `ssoStateRecord` gains only a 32-byte digest field. `Initiate` requires the canonical binding header and stores the domain-separated SHA-256 digest; session exchange atomically consumes state and compares recomputed digests with `subtle.ConstantTimeCompare` before provider token validation or callback classification. Both provider session paths share this check.

- [ ] Write Apple and Google tests for exact header success; absent, padded/noncanonical, short, long and malformed values; different session secret yields opaque `401 invalid_state`, consumes state, and performs zero provider/user/session work. Inspect Redis JSON, bodies, logs and errors for absence of a sentinel raw secret.
- [ ] Run `cd services/control-plane && go test -p 1 ./internal/oauth -run 'Test.*(ClientBinding|Initiate|Session)' -count=1`; expect the binding tests to fail.
- [ ] Implement canonical base64url validation and digest in `handler.go`; enforce exchange in the existing common session path in `apple_session.go`, before other work. Keep the raw value out of URL, Redis, body and logs. Do not add unbound-state compatibility parsing.
- [ ] Rerun the focused command and commit the binding slice. Re-run both Desktop provider tests against the same header contract.

### Task 6: Guard six public cookie routes; prove two exemptions and admin inventory

**Files:** Create `services/control-plane/internal/middleware/login_csrf.go`, `login_csrf_test.go`; modify `services/control-plane/internal/api/router.go`; test `api/oauth_wiring_test.go`, `auth/handlers_integration_test.go`, `admin/routes_test.go` or `admin/handlers_auth_test.go`.

**Interfaces:** Group middleware uses `c.Request.Method` plus `c.FullPath()` and only matches these six templates: `POST /api/v1/auth/register/confirm`, `POST /api/v1/auth/login`, `POST /api/v1/auth/mfa/verify`, `POST /api/v1/auth/sso/:provider/session`, `POST /api/v1/auth/sso/:provider/complete-link`, `POST /api/v1/auth/sso/:provider/complete-registration`. For matches: missing/malformed/non-JSON media type -> `415 unsupported_media_type`; cross-site Fetch Metadata -> `403 cross_site_request`; present non-exact configured Origin -> `403 invalid_origin`.

- [ ] Write a table over every six-route template: reject missing, malformed, text/plain, form and multipart media types; reject case-insensitive cross-site; reject foreign, `null`, multi-value, prefix/suffix and wrong-port origins; accept exact allowed Origin with JSON parameters and origin-less native JSON. Supply valid fixtures so the handler is reached on positive rows.
- [ ] Run `cd services/control-plane && go test ./internal/middleware ./internal/api -run 'Test.*(LoginCSRF|SSOCookieRouteInventory)' -count=1`; expect the guard tests to fail.
- [ ] Implement the classifier and parser in the new middleware file; mount once after `AuthBanCheck` in `api/router.go`, preserving CORS preflight ownership and existing route rate limits.
- [ ] Pin `SetRefreshCookie` set/clear name/path, HttpOnly, Lax and TLS/proxy Secure attributes. Pin refresh cookie versus explicit `X-Refresh-Token`, logout cookie versus bearer/header/session sources, and absence of form/query credentials. Assert a modeled cross-site POST carries no Lax cookie. If any exemption invariant fails, classify that route before shipping.
- [ ] Pin admin classification separately: password route is challenge-only; WebAuthn sets `__Host-cv_admin_sid` with Secure, HttpOnly, Path `/`, SameSite=Strict; logout clears; other admin writes remain under existing auth/router controls. Inventory every public cookie writer so a future writer fails until classified or justified.
- [ ] Rerun `cd services/control-plane && go test -p 1 ./internal/middleware ./internal/api ./internal/auth ./internal/admin -run 'Test.*(LoginCSRF|Cookie|RouteInventory|Refresh|Logout)' -count=1`; expect pass. Invert VULN-031/007/020 regressions, then commit the guard/inventory slice.

### Task 7: Replace JWKS snapshots and bound unknown-kid fetches

**Files:** Modify `services/control-plane/internal/oauth/apple.go`, `google.go`; test `oauth/apple_test.go`, `google_test.go`, `apple_session_integration_test.go`, `google_session_integration_test.go`.

**Interfaces:** Preserve each provider's `fetchKey(ctx,kid)` surface. A valid HTTP 200 nonempty JWKS replaces the whole map; provider-wide last-attempt timestamp is recorded before network I/O; existing singleflight collapses concurrent misses; no per-kid negative map.

- [ ] Write fake-clock/fake-provider tests for withdrawn key rejection after replacement, expired cached key rejection after all refresh paths, non-200/invalid/empty response preserving only unexpired snapshot, and many distinct/concurrent unknown kids causing at most one fetch in 60 seconds even on failure; assert a new opportunity after clock advance.
- [ ] Run `cd services/control-plane && go test ./internal/oauth -run 'Test.*(JWKS|UnknownKid|KeyRotation)' -count=1`; expect new assertions to fail.
- [ ] Implement the same snapshot/attempt contract separately in Apple and Google around their current cache mutex and singleflight. Keep parse and HTTP errors fail closed.
- [ ] Rerun focused tests plus `cd services/control-plane && go test -race ./internal/oauth -run 'Test.*(JWKS|UnknownKid)' -count=1`; expect pass. Commit both providers together.

### Task 8: Bound initiation allocation and E2EE key material

**Files:** Modify `services/control-plane/internal/oauth/handler.go`, `services/control-plane/internal/auth/handlers.go`, and `services/control-plane/internal/api/router.go` only if existing route limiter composition requires it; test `oauth/apple_initiate_integration_test.go`, `oauth/handler_integration_test.go`, `auth/handlers_integration_test.go` (or existing `handlers_integration_2_test.go`).

**Interfaces:** After provider/request/binding validation and before `randomString`/Redis state/Apple CFKV, consume `ratelimit:sso:initiate:<provider>` budget of 600 accepted per minute; Redis error is `503 redis_unavailable`, exhaustion `429 rate_limited`. Use one shared encoded/decoded validation contract for password registration and SSO fields: wrapped `16384/12288`, salt `1024/768`.

- [ ] Write Redis-backed tests proving existing per-IP cap and provider-global cap independently, no state or Apple CFKV write on exhaustion/backend error, and provider isolation. Run `cd services/control-plane && go test -p 1 ./internal/oauth ./internal/api -run 'Test.*(InitiateGlobalLimit|InitiateRate)' -count=1`; expect new assertions to fail.
- [ ] Add the fixed provider budget with existing Redis rate-limit primitives or the smallest atomic equivalent; preserve `10/15m/IP`. Rerun the rate command; expect pass.
- [ ] Write password and SSO registration table tests: otherwise valid exact encoded/decoded limit accepted; encoded max+1 and valid decoded max+1 return 400 `invalid_key_material`; malformed base64 still fails; response/log never contains key sentinel. Run `cd services/control-plane && go test -p 1 ./internal/auth ./internal/oauth -run 'Test.*KeyMaterialLimit' -count=1`; expect new boundary assertions to fail.
- [ ] Put shared constants/validation in existing auth code and call them from both registration readers; validate encoded size before decode, decoded size immediately after, leaving public-key/PoP checks intact. Rerun boundary tests; expect pass. Commit limits slice.

### Task 9: Self-Hosted integration, docs and quality gate

**Files:** Modify `docs/adr/0046-account-authentication-and-recovery.md`, `docs/api/openapi.yaml`, `docs/development.md`, `CHANGELOG.md`; modify `docs/api/README.md` only when schema needs explanatory prose. Any generated count-doc delta from `scripts/update-claude-md-counts.sh` must be reviewed.

**Interfaces:** Document mandatory binding header on both SSO routes, complete-link challenge response and statuses, six-route media/Origin errors, key `maxLength`, rollout/restart behavior, and JWKS availability tradeoff.

- [ ] Update docs and `## [Unreleased]` with user-visible behavior; verify that old in-flight states/challenges restart and older Desktop builds cannot initiate SSO after enforcement are explicit. Run `BASE_SHA="$(git merge-base HEAD origin/main)" GITHUB_BASE_REF=main bash .github/scripts/verify-changelog.sh` from the Self-Hosted root.
- [ ] Run `./scripts/api/check-openapi-coverage.sh` and the repository's OpenAPI drift checks. Run `./scripts/update-claude-md-counts.sh` because routes, Go packages and tests changed; inspect any generated delta before including it.
- [ ] With isolated `DATABASE_URL` and allocator-backed `REDIS_URL`, run `cd services/control-plane && go test -p 1 ./internal/oauth ./internal/auth ./internal/mfa ./internal/api ./internal/middleware ./internal/admin -count=1`, then `cd services/control-plane && go test -race -p 1 ./internal/oauth ./internal/auth ./internal/mfa ./internal/api ./internal/middleware ./internal/admin -count=1`. Run the two original exploit files in their supported scan harness with expectations inverted; record each outcome separately.
- [ ] Run the configured `.claude/repo-config.json` `/verify-quality-gate` checks, relevant pre-commit hooks, and required whole-repository CI. Record exact-head lint/type/test, new-code coverage >=80%, zero new blocker/critical findings and zero new high-confidence hotspots; local green results do not substitute for server reports.
- [ ] Commit Self-Hosted docs and any tested count delta with the implementation. Do not claim deployment or live protection from source/CI proof.

### Task 10: Port the semantic contract to Hosted

**Files:** Create Hosted `services/control-plane/internal/middleware/login_csrf{,_test}.go`; modify corresponding Hosted `services/control-plane/internal/{api/router.go,oauth/handler.go,oauth/apple_session.go,oauth/apple.go,oauth/google.go,auth/oauth_adapter.go,auth/handlers.go,mfa/challenge.go,mfa/handlers.go}` and owning tests. Modify Hosted `docs/adr/0046-account-authentication-and-recovery.md`, `docs/api/openapi.yaml`, `docs/development.md`, `CHANGELOG.md` and `docs/api/README.md` only if needed.

**Interfaces:** Match Self-Hosted HTTP status/body/header/cookie, signed completion kind, state digest, Redis keys/TTLs, JWKS/rate/key constants and six-route census. Preserve Hosted's stored `password_login_disabled=true` enforcement and branch-local config/handler differences.

- [ ] Diff named Hosted files and tests against the reviewed Self-Hosted contract immediately before porting. Copy test intent and route census, then run focused Hosted Go tests red first against its own isolated `DATABASE_URL` and `REDIS_URL`.
- [ ] Port one behavior slice at a time in Task 3→8 order; after each slice run its same `cd services/control-plane && go test -p 1 ... -run ... -count=1` command in Hosted. Add an explicit Hosted stored-disabled test and repeat Apple/Google binding and Desktop continuation probes.
- [ ] Run the full Task 9 focused/race suites, original exploit regressions, OpenAPI drift, `./scripts/update-claude-md-counts.sh`, changelog guard, pre-commit and configured quality gate in Hosted. Review drift instead of copying whole Self-Hosted files. Record separate Hosted exact-head coverage >=80%, zero new blocker/critical issues and zero new high-confidence hotspots.
- [ ] Commit Hosted code, tests, docs and reviewed generated count delta as its own PR. Cross-check the eighteen design acceptance criteria against Self-Hosted, Hosted and Desktop artifacts; a missing mirror proof keeps enforcement blocked.

### Task 11: Release floor, staged proof and enforcement

**Files:** Desktop release metadata and supported-client-floor configuration only where the release system actually owns them; no permanent server bypass. `SPA_MIN_CONTRACT` changes from 19 to 34 only if the remote SPA starts depending on the widened IPC result.

**Interfaces:** Supported Desktop build contains contract 34, Apple/Google binding and link-MFA continuation before mandatory server binding is deployed.

- [ ] Release Desktop companion with Task 1 and 2 evidence; verify installed build and supported minimum through the real release-admission mechanism. Do not treat `IPC_CONTRACT_VERSION` or `SPA_MIN_CONTRACT` source values alone as proof of the supported shell floor.
- [ ] Exercise both providers against Self-Hosted and Hosted staging: same-secret initiate/session succeeds; mismatch consumes state; complete-link MFA remains tokenless until successful factor; exact-origin and origin-less native JSON work. Capture build, server SHA and test environment.
- [ ] Only after the floor is observable, deploy Self-Hosted and Hosted enforcement in the approved order. Observe outcome-class counts only; never log state, binding, JTI/challenge, provider token, email or key material.
- [ ] Rollback is an application rollback for a demonstrated outage, with the Desktop companion retained; document that rollback restores exposure. No schema rollback exists; Redis records expire after their TTL.

## Risk register and open gates

| Risk / decision | Mitigation / gate |
|---|---|
| Old Desktop shell cannot produce binding | Companion build and supported shell floor precede enforcement; no compatibility bypass |
| MFA policy race or partial session | Audit every factor writer; lock final authority; force each insert/commit failure and concurrency in real Postgres tests |
| Missing Redis pending state after valid factor | Fail closed with no identity/session; user restarts; aligned five-minute TTL and atomic consume |
| Cross-site guard breaks native client | JSON still required, absent Origin/Fetch Metadata accepted; Desktop and staged provider tests |
| Refresh/logout exemption drifts | Executable cookie, credential-source and route inventory assumptions; reclassify if any fails |
| JWKS rotation availability | Fixed 60-second unknown-kid bound, fake-clock tests, outcome-only telemetry; no expired or withdrawn key acceptance |
| 600/min/provider too low for real burst | Measure initiation traffic before deploy; revise constant only with measured evidence and security review |
| Hosted divergence | Port semantics/test IDs, not files; prove stored-disabled Hosted behavior independently |

**Resolve before enforcement:** identify the real supported Desktop release-floor control; confirm all MFA enrollment/disable writers serialize on the user lock; measure provider initiation volume; inspect the final reflective cookie-route census. These are verification gates, not reasons to omit state binding or defer the server fix.

**Skipped scope:** browser SSO cookie support, new MFA UI/factors, broad admin/authenticated CSRF expansion, configurable ceilings, background JWKS refresh, migrations and unrelated auth refactors.
