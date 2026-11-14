# MFA-safe SSO linking and login-CSRF closure — design

**Issue:** Concord-Voice/Security#159 · **Date:** 2026-11-14 · **Status:** approved for planning

This is the Phase-2 reconciled design for VULN-032, VULN-031, VULN-034, VULN-029 and VULN-030, including the related VULN-007/VULN-020 login-CSRF surface. It is a semantic two-repository change: Self-Hosted is the reference implementation and Hosted receives the same security contract, adapted to any branch-local drift. Desktop is a release-order dependency, not optional follow-up work.

No new third-party API or dependency is introduced. The design uses the repository's current Gin, Redis, JWT, Postgres, `crypto/subtle`, and existing singleflight patterns, so there is no external dependency contract to refresh before planning. Symbol names and route shapes below were read from the current branch; implementers must locate symbols rather than rely on line numbers.

## 1. Problem statement

The account-link completion path proves a password and immediately creates an SSO identity plus a session, bypassing both effective `password_login_disabled` policy and MFA. Six unauthenticated JSON POST routes can establish a refresh cookie but do not reject browser-simple or cross-site requests, so an attacker can drive login CSRF. The SSO `state` record is not bound to the Desktop process that initiated it. Provider key caches retain withdrawn JWKS keys and can repeatedly fetch on arbitrary unknown `kid` values; SSO initiation is bounded only per IP; and E2EE key material is decoded without server-side encoded or decoded size ceilings. These defects compound: the public pre-auth edge can be abused before the final account/session transaction, and today no one closed authority check covers that whole path.

## 2. Constraints and invariants

| Constraint | Binding decision | Governing source |
|---|---|---|
| Closed login completion authority | An MFA challenge can complete only the signed, server-selected completion kind. Missing legacy kind means ordinary password login; unknown values fail closed. There is no client-supplied `mfa_completed` boolean. | `.claude/rules/domains/backend.md`; existing `internal/mfa` challenge contract |
| Final policy is serialized | The successful link/session transaction locks the user, then re-checks disabled state, effective password-login policy, credential epoch, and current MFA policy before it writes anything. | `.claude/rules/domains/backend.md` transaction/error invariants |
| Identity and session are indivisible | SSO identity, login family and refresh row commit in one transaction. A failure leaves none of them. Cookie/header/body are emitted only after commit. | `.claude/rules/domains/backend.md` transaction invariants |
| E2EE material is opaque and bounded | The server validates only encoding and size, never logs or interprets the wrapped private key. Existing public-key/PoP validation remains unchanged. | `.claude/rules/domains/backend.md`; E2EE review trigger |
| Native clients remain supported | Missing `Origin` and Fetch Metadata are allowed because Desktop main is not a browser. If either header is present, it is evaluated strictly. All six guarded routes still require JSON. | Existing Desktop main-process auth architecture |
| No open-ended compatibility bypass | Desktop support lands before mandatory server enforcement. There is no permanent `compat` mode or environment switch that accepts an unbound SSO state after cutover. | Security#159 state-binding acceptance |
| Mirrors are semantic, not blind copies | Self-Hosted and Hosted must satisfy identical tests and API behavior even where their handler/config files differ. | Org mirror/release practice |
| Real backing services in integration tests | Postgres/Redis integration coverage uses the repository harness and isolated test DB/Redis indices; shared Go caches remain untouched. | `.claude/rules/domains/tests.md` |

There is no migration. No existing token or Redis record is made more permissive. Old in-flight SSO states and old MFA challenges may fail after enforcement and must be restarted; their TTLs are short and this is safer than dual parsing.

## 3. Reconciliation decisions

| Conflict | Winner | Reason |
|---|---|---|
| Treat password proof as enough to link vs enforce the target account's full login policy | **Full policy wins.** | Linking creates a login identity and a live session. It cannot be a weaker login path than `/auth/login`. |
| Link the identity before MFA and mint later vs defer the whole mutation | **Defer identity and session together.** | A half-linked account after failed/abandoned MFA is both surprising and security-sensitive. The existing bound transaction is the right atomic boundary. |
| Reuse a generic login MFA challenge vs invent a second MFA verification endpoint | **Reuse `/auth/mfa/verify`, add a signed closed completion kind.** | This preserves the existing modal and proof verification while preventing a challenge from being replayed into the wrong completer. |
| Bind SSO state only to provider `state`/PKCE vs bind it to the initiating Concord client | **Client binding wins.** | The issue explicitly requires it. PKCE binds the provider code exchange, not the Concord session exchange to the initiating Desktop process. |
| Put the binding secret in a query/body/response vs keep it in Desktop main memory | **Main-memory secret in a request header wins.** | Query and response values are more likely to enter URL, proxy, renderer, and access logs. |
| Roll out server enforcement with Desktop in the same instant vs preserve old clients indefinitely | **Desktop companion first, then mandatory server enforcement.** | An old shell cannot manufacture the new secret. A permanent server bypass would leave the issue unresolved. |
| Add CSRF middleware to every auth POST vs target only cookie-establishing public routes | **Group-mounted middleware with an explicit six-route classifier.** | Broad guarding risks breaking recovery/native flows; per-route decorators are easy to omit. One group guard plus an inventory test keeps the scope closed and auditable. |
| Guard refresh/logout too vs exempt them | **Exempt only with invariant tests.** | Refresh/logout use the Lax refresh cookie or explicit native token paths. The exemption is acceptable only while cookie attributes and credential-extraction behavior are pinned. |
| Merge new JWKS keys into cache vs replace the provider snapshot | **Full replacement wins.** | A successful provider snapshot is authoritative; merging makes key withdrawal impossible. |
| Fetch once for every unknown `kid` vs bound refresh per provider | **One provider-wide refresh opportunity per interval, collapsed concurrently.** | Attacker-controlled `kid` cardinality must not control provider traffic. |
| Add configurable security ceilings vs fixed conservative bounds | **Fixed bounds.** | These are abuse ceilings, not product tuning knobs. Matching the existing Desktop envelope avoids needless configuration and compatibility surface. |

## 4. Design

### 4.1 SSO-link completion is a closed two-stage login

#### Challenge contract

Extend the signed login challenge claims with `completion_kind`, a closed enum:

- absent or `login`: the existing password-login completion path;
- `sso_link`: the account-link continuation described here;
- any other value: reject before factor verification or completion.

Keep the existing login challenge purpose so factor verification, one-time challenge claiming, WebAuthn options and the Desktop MFA modal remain shared. `completion_kind` is signed by the server and is never accepted from a completion request. The issuer for `sso_link` also stores a single-use Redis record keyed by the challenge JTI, for example `mfa_sso_link:<jti>`, with the same five-minute TTL as the challenge. Its payload contains only:

- target user ID;
- credential epoch observed after password verification;
- provider, provider subject, provider email, and relay-email flag.

It contains no password and no bearer/session token. Store it with `SET NX EX`; if the write fails, invalidate/discard any already-created challenge/WebAuthn transient state and return a dependency error, never a usable challenge.

#### Initial `complete-link`

`POST /api/v1/auth/sso/:provider/complete-link` continues to consume the one-time `sso_token` and verify the target account password under existing lockout behavior. It then:

1. Reads the stored `password_login_disabled` preference and evaluates `auth.EffectivePasswordLoginDisabled(instanceType, stored)`. An effective disable returns `403 account_uses_sso`; no MFA challenge, identity, session, or cookie is produced. Self-Hosted retains its existing deployment-policy override; Hosted enforces the stored preference.
2. Captures the verified credential epoch.
3. Queries current login-eligible MFA enrollment using the existing fail-closed adapter.
4. If no login-eligible factor exists, invokes the bound final transaction. That transaction must re-check that MFA is still absent under the locked authority read; an enrollment race returns `mfa_required` and writes nothing.
5. If MFA is enabled, creates the signed `sso_link` challenge and Redis pending record, then returns the standard MFA challenge shape. The response contains no access token, refresh token, session ID header, or `Set-Cookie`, and no SSO identity exists yet.

The challenge response is `200`, matching existing login/SSO MFA behavior:

```json
{
  "mfa_required": true,
  "challenge_token": "<signed JWT>",
  "methods": ["totp", "webauthn"],
  "recovery_only_methods": [],
  "default_method": "totp",
  "webauthn_options": {}
}
```

Optional fields follow today's omission rules. Recovery-only factors do not become a new login factor.

#### MFA continuation

`POST /api/v1/auth/mfa/verify` keeps its request shape. After the existing verifier successfully claims and verifies the challenge:

1. It dispatches by the signed `completion_kind`.
2. For `sso_link`, a dedicated completer atomically consumes `mfa_sso_link:<jti>` with `GETDEL` (or the repository's equivalent atomic primitive).
3. It constant-time/strictly validates the pending user ID and credential epoch against signed claims and rejects missing, expired, mismatched, or malformed state.
4. It enters the final bound transaction. The transaction locks the target user authority row and re-checks account disabled state, effective `password_login_disabled`, credential epoch, and current MFA policy. The verified challenge is represented by an internal proof type constructed only by the verifier, not by an exported boolean.
5. In that same transaction it inserts the SSO identity, login family, and refresh row. Commit precedes cookie/header/body emission.

The existing `IssueAccessAndRefreshBound` surface should become or delegate to a policy-aware bound primitive. A direct caller with an MFA-enrolled target and no verifier-issued proof gets `ErrMFARequired`. Factor enrollment/disable writers must be audited for the same user authority lock; any writer that can change the final MFA answer without serializing on it must be brought under that lock as part of this change.

Challenge/pending-link state is spent on a completed verification attempt even when the final transaction discovers changed policy. The user restarts the link flow; no partially authorized state is retained.

### 4.2 SSO state is bound to the initiating client

Desktop main owns a 256-bit one-flow secret:

1. Before calling SSO Initiate, main generates 32 cryptographically random bytes and encodes them as unpadded base64url (exactly 43 ASCII characters).
2. Main sends the raw value only in `X-Concord-SSO-Client-Binding` on both `GET /api/v1/auth/sso/:provider` and `POST /api/v1/auth/sso/:provider/session`.
3. The server rejects missing, non-canonical, or non-32-byte values. Initiate computes `SHA-256("concord-sso-client-binding-v1\x00" || raw)` and stores only the digest in `ssoStateRecord`.
4. Session exchange atomically consumes the state record, hashes the request header the same way, and compares the 32-byte digests with `subtle.ConstantTimeCompare` before ID-token validation or callback classification. Missing/malformed/mismatch uses the same opaque `401 invalid_state` response and consumes the state.
5. Main destroys the raw value when the flow succeeds, fails, times out, or is cancelled.

The raw binding never appears in an authorization URL, query string, Redis, JSON response/body, log field, telemetry, renderer state, preload/API argument, or IPC result. Tests inspect response bodies, structured log captures and IPC payloads for the sentinel secret.

The Desktop Apple and Google flows already keep PKCE, loopback, provider token exchange and Concord session exchange in main. That custody remains. A future browser client may instead use a host-only, Secure, HttpOnly, SameSite pre-auth cookie whose digest is stored in the state record; it must not relax Desktop's header binding. Browser-cookie support is not part of this issue.

### 4.3 Desktop MFA and binding companion

The Desktop companion lands before mandatory server enforcement:

- Apple and Google main-process flow objects generate and retain the client-binding secret and attach the header to Initiate and Session. Nothing crosses preload/IPC.
- `sso:completeLink` accepts the existing request but its return type becomes a discriminated union: completed session, MFA challenge, or normalized error. This additive result-shape change bumps `IPC_CONTRACT_VERSION` from 33 to 34.
- On an MFA challenge, main moves the credential-owner context from pending link to pending MFA and returns only the public challenge fields. No refresh credential has been stored.
- The renderer service and `SSOAccountLinkConfirm` route that branch into the existing global MFA challenge store/modal with purpose `sso_login`, provider and owner. `sso:completeMFA` remains the only completion bridge; it submits `/auth/mfa/verify` from main and stores the returned refresh token under the correct owner. Do not fork a second MFA UI.
- Preload exposes the widened result type but no new channel and no secret-bearing argument.

`SPA_MIN_CONTRACT` does not move merely because main gains the capability. It moves to 34 only in the coordinated release that makes the remote SPA depend on the widened result. Independently, server enforcement must not be enabled until the supported Desktop minimum is a build containing contract 34 and both provider bindings.

### 4.4 Login-CSRF guard for exactly six public routes

Add one middleware to the `/api/v1/auth` group, after `AuthBanCheck`. The middleware classifies by HTTP method plus Gin's matched full route template and enforces only this closed set:

1. `POST /api/v1/auth/register/confirm`
2. `POST /api/v1/auth/login`
3. `POST /api/v1/auth/mfa/verify`
4. `POST /api/v1/auth/sso/:provider/session`
5. `POST /api/v1/auth/sso/:provider/complete-link`
6. `POST /api/v1/auth/sso/:provider/complete-registration`

For a classified request:

- Parse `Content-Type` with `mime.ParseMediaType`; require media type exactly `application/json`. Parameters such as `charset=utf-8` are allowed. Missing, malformed, `text/plain`, form-urlencoded, and multipart values return `415 unsupported_media_type` before the handler.
- If `Sec-Fetch-Site` is present and equals `cross-site` case-insensitively, return `403 cross_site_request`. Absence remains valid for native clients.
- If `Origin` is present, require one exact configured allowed origin, including scheme, host and port. `null`, multiple/comma-joined values, suffix/prefix matches and foreign origins return `403 invalid_origin`. Absence remains valid for native clients.

The existing CORS layer still owns preflight. The guard never reflects an origin and never infers trust from `Host`, `Referer`, `Sec-Fetch-Mode`, or an access token. Same-origin JSON and origin-less Desktop-main JSON continue to work.

An inventory test enumerates public handlers that call `auth.SetRefreshCookie` and proves that each corresponding POST route is either in the six-route classifier or one of the two named exemptions below. A new cookie-establishing route fails this test until explicitly classified or justified.

#### Refresh/logout exemption

`POST /api/v1/auth/refresh` and `POST /api/v1/auth/logout` are not in the classifier only after tests pin both code and browser-cookie assumptions:

- `SetRefreshCookie` and cookie clearing use the same name/path and keep `HttpOnly`, `Secure` under the current TLS/proxy contract, and `SameSite=Lax`;
- refresh prefers the ambient cookie only under the existing extraction rules, while native header-token paths remain explicit non-browser credentials;
- logout's cookie-authorized branch and explicit bearer/header/session paths remain distinguishable;
- a browser cross-site POST cannot supply the Lax cookie in the tested model;
- neither endpoint starts accepting form fields or query credentials.

If any of those invariants changes, the exemption test fails and the route must be reconsidered. This issue does not claim that the guard protects these two routes.

#### Admin inventory

The admin surface is separate and is not covered by the public-auth classifier:

- `POST /admin/api/v1/auth/password` returns a WebAuthn challenge and establishes no session cookie;
- `POST /admin/api/v1/auth/webauthn` establishes `__Host-cv_admin_sid`, whose code/tests pin Secure, HttpOnly, Path `/`, SameSite=Strict and the `__Host-` contract;
- `POST /admin/api/v1/auth/logout` clears that cookie;
- enrollment and gated admin writes are under the existing admin router/auth controls.

Add an inventory assertion documenting those classifications. No admin route is silently treated as protected by this middleware; broader admin Origin/Fetch-Metadata hardening, if desired, is a separate issue.

### 4.5 JWKS cache replacement and bounded unknown-`kid` refresh

Apple and Google use the same contract:

- A successful HTTP 200 with a valid JWKS builds a fresh immutable `kid -> key` map and atomically replaces that provider's cache snapshot. It never merges with the old map. A `kid` withdrawn from the successful snapshot is immediately rejected.
- A non-200, invalid, empty, or otherwise unusable response does not replace a still-valid cache. It also does not make expired cached keys usable.
- Every lookup checks snapshot expiry after any refresh path; the post-refresh lookup cannot bypass TTL.
- An unknown `kid` may trigger at most one provider refresh opportunity per fixed 60-second interval, independent of the attacker-controlled `kid` value. Concurrent misses collapse through the repository's existing singleflight pattern.
- Record the provider refresh attempt before network I/O so failures and timeouts are bounded too. After a successful refresh that still omits the `kid`, reject. A legitimate newly rotated key may be rejected until the interval opens; this bounded availability tradeoff is deliberate.

No per-`kid` map, unbounded negative cache, background loop, or new dependency is added.

### 4.6 Aggregate SSO-initiation bound

Keep the existing `10 / 15 minutes / IP` route limit. In `Initiate`, after provider and request validation but before generating state or writing Redis/CFKV, add a fail-closed provider-wide Redis budget:

- key namespace is provider-specific, for example `ratelimit:sso:initiate:<provider>`;
- ceiling is 600 accepted initiations per provider per minute per deployment;
- exhaustion returns `429 rate_limited` and creates no state/KV entry;
- Redis failure returns `503 redis_unavailable` and creates no state/KV entry.

The hard ceiling bounds one provider to 10 state allocations/second and both configured providers to 1,200/minute while preserving the tighter per-IP limit. It is a security ceiling, not an environment variable.

### 4.7 Encoded and decoded E2EE key-material limits

Define shared server constants and validation used by password registration and SSO completion so the two paths cannot drift:

| Field | Maximum encoded JSON string | Maximum decoded bytes |
|---|---:|---:|
| wrapped private key | 16,384 ASCII characters | 12,288 bytes |
| key salt | 1,024 ASCII characters | 768 bytes |

These match the existing Desktop trust-boundary envelope and are comfortably above current legitimate payloads. Validate encoded length before base64 decoding and decoded length immediately afterward. Continue current canonical/base64 and public-key proof validation. Any over-limit field returns `400 invalid_key_material`; do not log the value. OpenAPI declares the encoded `maxLength` values.

Boundary coverage includes exactly-at-limit (when otherwise valid), encoded max+1, and a valid encoding whose decoded value is max+1. The bounds apply to both password registration and SSO registration/link payload readers where the field is accepted.

## 5. API and status contract

### `POST /api/v1/auth/sso/:provider/complete-link`

| Status | `error_code` / body | Meaning |
|---:|---|---|
| 200 | existing session response | No login-eligible MFA at the locked final check; identity and session committed atomically |
| 200 | `mfa_required: true` challenge shape | MFA continuation required; no identity, token, session header or cookie |
| 400 | `invalid_request` | Malformed JSON/request |
| 401 | existing invalid/expired SSO token code | One-time SSO proof invalid |
| 403 | `account_uses_sso` | Effective password login is disabled |
| 423 | `account_locked` | Existing password lockout behavior |
| 429 | existing route rate-limit body | Per-IP limit |
| 500/503 | stable existing dependency/internal code | Challenge or state persistence failed; no mutation |

### `POST /api/v1/auth/mfa/verify` for `sso_link`

| Status | Meaning |
|---:|---|
| 200 | Factor verified; identity plus session committed; existing session body/header/cookie returned |
| 400 | Existing malformed proof semantics |
| 401 | Invalid/expired/spent challenge, missing pending link, stale epoch, unknown completion kind, or mismatched signed/pending authority |
| 403 | Final effective policy now refuses the link (`account_uses_sso` where disclosing it matches the initial path) |
| 409/423 | Existing duplicate-identity or account-lock semantics where applicable |
| 500/503 | Final transaction/dependency failed; no identity/session committed |

### SSO state-boundary routes

`GET /api/v1/auth/sso/:provider` and `POST /api/v1/auth/sso/:provider/session` require `X-Concord-SSO-Client-Binding` after the coordinated enforcement release. Missing, malformed or mismatched session-exchange binding is the same opaque `401 invalid_state`. Initiate rejects malformed/missing binding as `400 invalid_request`; neither response includes the binding. The existing successful response shapes do not gain a binding field.

### CSRF guard

All six classified routes add `415 unsupported_media_type`, `403 cross_site_request`, and `403 invalid_origin` as described in section 4.4. Handler-specific statuses remain unchanged after the guard passes.

## 6. Test strategy

Tests must first demonstrate each old exploit against the unmodified behavior, then invert to the protected result. A test description names the mutation that would make it fail.

### Control plane: SSO link and MFA

- MFA-enrolled target: `complete-link` returns the challenge shape with no `Set-Cookie`, session header, tokens, identity, login family, or refresh row; correct MFA produces all three rows atomically and one cookie.
- Wrong/expired/replayed factor, expired/missing/replayed Redis JTI, signed/pending user or epoch mismatch, and unknown `completion_kind` all fail closed.
- A normal legacy login challenge with no kind still completes ordinary login; `sso_link` can never dispatch to the ordinary login completer.
- Effective `password_login_disabled` refuses before challenge creation in Hosted policy, while Self-Hosted's existing policy override remains covered.
- A credential-epoch change after password proof and an MFA enrollment/policy change before final commit each leave no identity/session. A direct bound mint for an MFA-enrolled user without internal proof returns `ErrMFARequired`.
- Force identity insert, family insert, refresh insert and commit failures separately; each leaves zero partial rows and no response cookie.
- Concurrent final attempts yield one consumed challenge/pending record and at most one identity/session.

### Control plane: client binding

- Both providers: exact 32-byte binding succeeds; absent, padded/non-canonical, short, long and malformed bindings fail.
- Session exchange with a different secret fails constant-time comparison, consumes state and performs no provider/user/session work.
- Redis stores only the 32-byte digest; the raw sentinel is absent from state JSON, responses, captured logs and errors.
- Initiate/session tests cover both Apple and Google. Apple CFKV state-to-port handling occurs only after the aggregate budget and carries no binding secret.

### Control plane: login CSRF and route inventory

For every classified route, table-drive:

- `text/plain`, `application/x-www-form-urlencoded`, `multipart/form-data`, missing and malformed Content-Type -> 415;
- `Sec-Fetch-Site: cross-site` -> 403;
- foreign, `null`, prefix/suffix lookalike, wrong-port and multi-value Origin -> 403;
- configured exact Origin + JSON -> reaches the handler;
- no Origin/Fetch Metadata + JSON -> reaches the handler for native clients.

The route inventory pins exactly six guarded public routes, the refresh/logout exemptions, all public `SetRefreshCookie` call sites, and the separate admin classification. Cookie tests pin set/clear attributes and refresh/logout credential sources.

### Control plane: JWKS, rate limits and key material

- A second successful JWKS omitting an old `kid` makes that key fail immediately; failed refresh preserves only an unexpired prior snapshot; expired snapshots are never served.
- Many distinct and concurrent unknown `kid` values cause at most one provider fetch per 60-second interval, including when the fetch fails. Advance a fake clock rather than sleeping.
- Per-IP and provider-global SSO-initiation limits are independently exercised. Global rejection and Redis error create no SSO state or Apple CFKV record.
- Password and SSO registration use the same key-size table; encoded and decoded boundaries, invalid base64 and redacted errors are covered.

### Desktop

- Apple and Google main flows generate a fresh 32-byte secret per attempt, send the same value on initiate/session, never send it over IPC/preload/renderer, and destroy it on every terminal path.
- `sso:completeLink` distinguishes session, MFA and error bodies. MFA stores no refresh credential; the existing `sso:completeMFA` stores the successful credential under the preserved owner/provider.
- `SSOAccountLinkConfirm` opens the canonical MFA modal and does not duplicate submit logic.
- Contract tests pin `IPC_CONTRACT_VERSION = 34`; SPA minimum behavior is tested at the release that raises it.

### Quality and execution

- Run focused Go unit/integration suites with real isolated Postgres/Redis per `.claude/rules/domains/tests.md`; do not set private Go caches.
- Run Desktop Vitest suites for main IPC/provider flows, preload/service and account-link/MFA UI.
- Run OpenAPI coverage, pre-commit, lint/race checks required by each repository, and both repositories' exploit regressions.
- Because this changes API routes/middleware, Go packages and test files, Phase 4 runs `./scripts/update-claude-md-counts.sh` in Self-Hosted and Hosted and records any resulting documentation delta. There is no migration, new Zustand store, or CSS theme block.

## 7. Exact file map

The implementation plan may split tests, but it must not expand production scope without an architect amendment.

### Self-Hosted reference

**Create**

- `services/control-plane/internal/middleware/login_csrf.go`
- `services/control-plane/internal/middleware/login_csrf_test.go`

**Edit — control plane**

- `services/control-plane/internal/api/router.go` — group-mounted guard, six-route classifier input, existing route limits
- `services/control-plane/internal/oauth/handler.go` — state digest, initiate global budget, link challenge/pending state, key bounds
- `services/control-plane/internal/oauth/apple_session.go` — binding verification at session exchange
- `services/control-plane/internal/oauth/apple.go` — full-replace JWKS cache and provider-wide miss refresh bound
- `services/control-plane/internal/oauth/google.go` — same JWKS contract
- `services/control-plane/internal/auth/oauth_adapter.go` — policy-aware locked atomic identity/session mint and SSO-link challenge issuer
- `services/control-plane/internal/auth/handlers.go` — shared registration key-material bounds and cookie invariant coverage seams
- `services/control-plane/internal/mfa/challenge.go` — signed closed completion kind
- `services/control-plane/internal/mfa/handlers.go` — verifier dispatch to dedicated SSO-link completer

**Edit/create tests in the existing owning packages**

- `services/control-plane/internal/oauth/handler_integration_test.go`
- `services/control-plane/internal/oauth/apple_initiate_integration_test.go`
- `services/control-plane/internal/oauth/apple_session_integration_test.go`
- `services/control-plane/internal/oauth/google_session_integration_test.go`
- `services/control-plane/internal/oauth/apple_test.go`
- `services/control-plane/internal/oauth/google_test.go`
- `services/control-plane/internal/auth/oauth_adapter_test.go`
- `services/control-plane/internal/auth/handlers_integration_test.go`
- `services/control-plane/internal/mfa/challenge_test.go`
- `services/control-plane/internal/mfa/handlers_test.go`
- `services/control-plane/internal/mfa/login_fail_closed_test.go`
- `services/control-plane/internal/admin/routes_test.go` and/or `handlers_auth_test.go` — admin inventory and cookie contract only; no public middleware wiring

If the MFA factor-writer audit finds an unserialized policy writer, edit only its existing owning file/test to take the same authority lock and call that out in the PR. Do not pre-emptively refactor all MFA enrollment.

**Docs/API**

- `docs/adr/0046-account-authentication-and-recovery.md` — extend the existing auth ADR with the closed completion authority, client binding, CSRF scope, JWKS/cache tradeoff and rollout
- `docs/api/openapi.yaml` — headers, MFA branch, error statuses and key `maxLength`
- `docs/api/README.md` if the new contract needs prose beyond generated schema
- `docs/development.md` — focused test commands and Desktop-first rollout
- `CHANGELOG.md` — user/security-visible behavior under Prerelease

### Hosted semantic mirror

Apply the same create/edit list under Hosted's corresponding paths:

- `services/control-plane/internal/{api,oauth,auth,mfa,middleware,admin}/...`
- `docs/adr/0046-account-authentication-and-recovery.md`
- `docs/api/openapi.yaml` (and `docs/api/README.md` only if needed)
- `docs/development.md`
- `CHANGELOG.md`

`oauth/handler.go`, `auth/handlers.go`, router and config surfaces have branch-local drift. Port the contract and tests; do not copy whole files. Hosted additionally proves that stored `password_login_disabled=true` is effective.

### Desktop companion

**Edit**

- `desktop/src/main/oauth/apple/appleFlow.ts`
- `desktop/src/main/oauth/google/googleFlow.ts`
- `desktop/src/main/ipc/sso.ts`
- `desktop/src/main/ipcContract.ts`
- `desktop/src/shared/sso.ts`
- `desktop/src/preload/preload.ts`
- `desktop/src/renderer/services/system/ssoService.ts`
- `desktop/src/renderer/components/Auth/SSOAccountLinkConfirm.tsx`
- `desktop/src/renderer/stores/auth/mfaChallengeStore.ts` only to reuse the existing `sso_login` continuation; no new store

**Edit tests**

- `desktop/tests/unit/main/ipc/sso.test.ts`
- `desktop/tests/unit/main/oauth/apple/appleFlow.test.ts`
- `desktop/tests/unit/main/oauth/google/googleFlow.test.ts`
- `desktop/tests/unit/main/ipcContract.test.ts`
- `desktop/tests/unit/renderer/services/ssoService.test.ts`
- `desktop/tests/unit/components/Auth/SSOAccountLinkConfirm.test.tsx`
- existing MFA modal/store tests where the link continuation is asserted

No renderer network call, new IPC channel, migration, CSS/theme change, or new dependency is in scope.

## 8. Rollout and rollback

There is no durable compatibility flag.

1. **Desktop companion release:** ship contract 34 with both provider-binding headers and complete-link MFA continuation. Server still runs the old compatible contract during this release step. Verify both providers against Self-Hosted and Hosted staging.
2. **Client floor:** advance the supported Desktop minimum/release admission to the companion build. If the remote SPA starts depending on the widened result, raise `SPA_MIN_CONTRACT` to 34 in that coordinated release; otherwise leave it 19.
3. **Self-Hosted and Hosted enforcement:** land the semantic mirror with mandatory binding, closed MFA completion, CSRF guard, JWKS/rate/key limits, docs and exploit tests. Deploy Hosted only after the client floor is observable. Self-Hosted release notes state that in-flight SSO attempts restart and older Desktop builds cannot start SSO.
4. **Observe:** metrics/logs may count only outcome classes (missing/malformed/mismatch binding, CSRF rejection class, provider refresh attempted/suppressed, global limit hit). They must not include state, binding, challenge, JTI, provider token, key material or email.

Rollback of the server enforcement release is an application rollback, not a runtime bypass. Rolling back restores vulnerability, so use it only for a demonstrated outage and keep the Desktop companion in place. Database rollback is unnecessary because there is no schema change. Redis records expire naturally and are versioned/namespaced so an older process ignores them.

## 9. Decomposition

| Task | Work | Ordering / parallelism |
|---|---|---|
| T0 | Exploit-characterization tests and route/cookie inventory in both server repos | First serial gate |
| T1 | Desktop Apple/Google client binding and custody tests | Parallel after T0; must ship before T8 |
| T2 | Desktop complete-link MFA union, main custody, preload/service/UI continuation, contract 34 | Parallel with T1; must ship before T8 |
| T3 | Closed MFA completion kind, pending JTI state and policy-aware atomic mint | Parallel with T4–T6 after T0 |
| T4 | Six-route CSRF guard plus refresh/logout/admin invariant tests | Parallel with T3, T5, T6 |
| T5 | JWKS replace/expiry/unknown-`kid` refresh bound | Parallel per provider, shared contract first |
| T6 | Initiation aggregate bound and key-material ceilings | Parallel with T3–T5 |
| T7 | Self-Hosted integration, OpenAPI, ADR 0046, changelog and quality/count probes | After T3–T6 |
| T8 | Hosted semantic port and Hosted-only password-disabled coverage | After Self-Hosted contract stabilizes; can prepare tests earlier |
| T9 | Client-floor release, staging verification, then mandatory server deploy | Strictly serial after T1/T2/T7/T8 |

## 10. Acceptance criteria

| # | Criterion | Required proof |
|---|---|---|
| 1 | MFA-enrolled `complete-link` returns a challenge and no identity, access/refresh token, session header or cookie; the identity/session appear only after successful MFA. | Self-Hosted + Hosted integration tests; Desktop continuation tests |
| 2 | Effective `password_login_disabled` refuses link completion; Self-Hosted's existing override and Hosted enforcement are each explicit. | Policy table tests in both repos |
| 3 | A direct/bound mint for an MFA-enrolled user without verifier-issued proof fails, including an enrollment race after password proof. | Atomic adapter integration tests |
| 4 | Final disabled/password/MFA/epoch policy read is under the user lock; identity, family and refresh are one transaction with rollback on every forced failure. | Real-Postgres transaction tests and review |
| 5 | Signed completion kind is closed; legacy login remains compatible; unknown kind and missing/mismatched pending JTI fail closed. | Challenge + verifier tests |
| 6 | SSO state is bound to a 256-bit initiating-client secret for Apple and Google; only a digest reaches Redis; comparison is constant-time; the secret appears in no response/log/IPC. | Provider integration and Desktop custody tests |
| 7 | Desktop main/preload/service/UI companion ships before enforcement and supports link-MFA through the existing modal and `sso:completeMFA`; contract is 34. | Desktop test suite and release evidence |
| 8 | Exactly six public cookie-establishing POST routes reject non-JSON/browser-simple types, cross-site Fetch Metadata and foreign/null Origin, while exact-origin JSON and origin-less native JSON work. | Middleware matrix + live route inventory |
| 9 | Refresh/logout stay exempt only while cookie attributes and credential-source invariants pass; admin cookie routes are inventoried separately. | Cookie/extraction/admin inventory tests |
| 10 | A successful JWKS refresh fully replaces the provider snapshot; withdrawn/expired keys fail. Distinct/concurrent unknown kids cause at most one attempt/provider/60s, including failures. | Apple + Google fake-clock tests |
| 11 | Existing per-IP initiation limit remains and a provider-global 600/minute fail-closed bound prevents state/CFKV allocation after exhaustion. | Redis-backed rate-limit tests |
| 12 | Password registration and SSO use the shared encoded/decoded key-material bounds; over-limit data returns 400 without logging material. | Boundary/redaction tests + OpenAPI schema |
| 13 | Every original exploit test is inverted and the focused, race/lint, OpenAPI, pre-commit and repository-required suites pass in Self-Hosted, Hosted and Desktop as applicable. | CI links and local command transcript |
| 14 | **0 new blocker/critical issues.** | Quality-gate report in each server repository and Desktop |
| 15 | **0 new high-confidence security hotspots.** | Quality-gate/security scan report in each affected repository |
| 16 | **At least 80% new-code coverage.** | Quality-gate coverage report in each affected repository |
| 17 | Docs ship with the code: ADR 0046, OpenAPI, development/release notes and Hosted mirror are updated in their owning PRs. | Diff review; OpenAPI drift gate |
| 18 | Phase-4 count probe runs in Self-Hosted and Hosted because API routes/middleware, Go packages and tests changed. | `./scripts/update-claude-md-counts.sh` transcript and any generated doc delta |

## 11. Risk register

| Risk | Likelihood | Mitigation |
|---|---|---|
| Server enforcement strands old Desktop builds | High without ordering | Companion first, contract 34, supported-client floor, staged provider proof before mandatory deploy |
| MFA policy changes between proof and commit | Medium | Same locked final authority read; audit factor writers for the lock; no boolean proof |
| Challenge succeeds but Redis pending state is gone | Low/expected at TTL | Fail closed with no identity/session; user restarts; align TTLs and consume atomically |
| Redis write succeeds but challenge response is lost | Low | Both are short-lived, single-use and contain no password; natural expiry; no cleanup job |
| Origin policy breaks Desktop/native flows | Medium if absence is rejected | JSON is mandatory, but absent Origin/Fetch Metadata is intentionally accepted and tested |
| Refresh/logout exemption becomes stale | Medium over time | Route/cookie inventory test makes code/cookie assumptions executable; reconsider on failure |
| JWKS bound delays a legitimate emergency rotation | Low | 60-second ceiling; successful scheduled/ordinary refresh still replaces snapshot; never serve withdrawn/expired keys |
| Provider-global initiation ceiling is too low for a burst | Low at 600/min/provider | Keep per-IP tighter, add outcome-only metric, change the constant only with measured evidence and security review |
| Fixed key bounds reject an unusual legacy key | Low | Bounds match current Desktop envelope and far exceed legitimate material; stage boundary telemetry without values |
| Hosted drift causes a partial mirror | Medium | Port tests and semantics, not whole files; cross-repo acceptance checklist and same exploit IDs |
| Secret leaks through diagnostics | Medium without custody tests | Header only, main memory only, digest in Redis, redaction assertions over logs/responses/IPC |

## 12. Phase-8 reviewers and out of scope

Expected review coverage: security reviewer and red team (auth/session/Redis/JWT boundaries), E2EE reviewer (wrapped-key material), frontend reviewer (renderer/store/service), code reviewer, scope reviewer, docs reviewer, and test writer. Route changes also require the repository's auth/RBAC route-inventory review even though no permission model changes.

Out of scope:

- redesigning SSO registration, provider consent, PKCE or provider token verification beyond the explicit binding/cache changes;
- adding a browser SSO client or pre-auth cookie implementation;
- expanding the CSRF guard to every authenticated write or admin route;
- changing refresh-token storage/cookie policy, except executable invariants needed to justify exemption;
- new MFA factors or a second MFA UI;
- configurable rate/key ceilings, background JWKS refresh, schema migrations, or broad MFA refactoring.

## 13. Verification still required during implementation

- Confirm every MFA factor-enrollment/disable writer serializes with the chosen user authority lock; this design requires remediation only where the audit proves a gap.
- Confirm the release system's exact supported-Desktop floor mechanism before T9; `SPA_MIN_CONTRACT` alone is not proof that an old shell cannot call SSO.
- Confirm the Hosted drift at each named file immediately before porting and preserve Hosted-only policy behavior.
- Measure provider-initiation traffic before deploy; 600/min/provider is deliberately conservative but has not been validated against production telemetry.
- Run the reflective cookie-route inventory against the final diff so no new or branch-only `SetRefreshCookie` call site is omitted.
