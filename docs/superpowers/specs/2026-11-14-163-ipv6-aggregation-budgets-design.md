# IPv6 aggregation and fail-closed pre-auth budgets — design

**Issue:** Concord-Voice/Security#163 · **Parent:** #151 · **Date:** 2026-11-14 · **Status:** approved for planning

This design was read against Self-Hosted `b3b7f490`. Line numbers are leads only; implementation must locate symbols again after rebasing on Security#162.

## 1. Intended outcome

Every source-address budget uses one canonical identity: an IPv4 address is its /32, an IPv4-mapped IPv6 spelling is the same IPv4 /32, and a native IPv6 address is its masked /64. This applies to control-plane rate limits and auth bans, admin lockouts, nginx request and connection limits, and the media-plane pre-upgrade handshake ceiling. Public authentication requests stop with a fixed 503 and `Retry-After` if Redis cannot enforce the source controls. A media-plane process configured for production refuses to start if no valid trusted-proxy rule exists, and a trusted proxy that omits the client headers is counted under the observed proxy peer instead of being admitted outside the gate.

This work does not make a forwarded address trustworthy. Security#162 must land first and establish the one real proxy hop; #163 aggregates and counts only the address that authenticated derivation supplies.

## 2. Blast radius and review routing

### Surfaces

| Surface | Likely files |
|---|---|
| Control-plane source identity and Redis gates | `internal/middleware/ratelimit.go`, `authban.go`, their tests, `internal/api/router.go`, route wiring tests |
| Auth-failure producers | `internal/auth/handlers.go`, `internal/mfa/handlers.go`, `internal/stepup/password_token.go`, `internal/users/handlers.go`, focused tests |
| Admin lockout | `internal/admin/lockout.go`, `handlers.go`, `routes.go`, lockout and route tests |
| Media-plane handshake | `services/media-plane/src/lib/admissionGate.ts`, `src/index.ts`, `tests/admissionGate.test.ts`, `package.json`, lockfile, `.env.example` |
| Edge limits | `infrastructure/deploy/nginx/concordvoice.conf`, a real-nginx fixture under `infrastructure/deploy/tests/` |
| Contracts and operator docs | `docs/api/openapi.yaml`, `docs/architecture.md`, `docs/runbooks/selfhost-quickstart.md`, `CHANGELOG.md` |
| Hosted mirror | The corresponding imported service, nginx, tests, and docs after the Self-Hosted source PR merges |

### Governing rules

- `.claude/rules/domains/backend.md`: errors are checked; security middleware may not fail open; fixed responses and Nightwatch outcomes must not contain source addresses.
- `.claude/rules/domains/media-plane.md`: pre-upgrade admission remains ahead of socket allocation and its in-memory store stays bounded.
- `.claude/rules/domains/tests.md`: security tests name the mutant they kill, cover failure paths, and use existing fixtures.
- `.claude/skills/_shared/context7-docs-discipline.md`: current nginx and `ipaddr.js` contracts were checked before this design.
- Security#162: client-IP authenticity precedes aggregation. A /64 derived from an attacker-selected header is still attacker-selected.

### Phase-8 reviewers

- `@red-team`: the `risk: security` label and auth/limiter content trigger it.
- `@security-reviewer`, `@scope-reviewer`, and `@code-reviewer`: always.
- `@pr-review-toolkit:silent-failure-hunter`: Go files change.
- `@pr-review-toolkit:pr-test-analyzer`: Go and TypeScript tests change.
- `@docs-reviewer`: Markdown changes.
- The repository has no configured deployment or Socket.IO specialist pattern. The orchestrator should still dispatch the available deployment-config and Socket.IO reviewers because nginx, Compose-provided environment, and the media-plane handshake are changed.

### Count-bearing surfaces

API route wiring, Go packages, and test files change, so Phase 4 runs `./scripts/update-claude-md-counts.sh` as a probe. No migration, Zustand store, or CSS theme block changes.

## 3. Decisions and rejected alternatives

### 3.1 One source bucket, no optional /48 tier

Use one shared rule: IPv4 /32 and native IPv6 /64. A second /48 counter is rejected for this issue. It doubles Redis work, needs a new threshold policy, and is not required to close any listed finding. If observed abuse later rotates across assigned /64s, that is a separate measured change.

The control plane exposes:

```go
func IPBucket(raw string) string
func ClientIPBucket(c *gin.Context) string
```

`IPBucket` parses with `net/netip`, clears an IPv6 zone, calls `Unmap`, preserves canonical IPv4, and returns the masked native IPv6 /64 address. Invalid or empty input returns the fixed sentinel `unattributed`; it never returns attacker text. `ClientIPBucket` applies it to `c.ClientIP()`. The sentinel deliberately makes unattributable requests share a bounded bucket rather than bypass the control.

Every Redis key builder owns normalization. Callers may continue passing a raw observed address to `RecordAuthFailure`, `ClearAuthFailures`, and the admin lockout; those implementations bucket before composing or hashing the key. This prevents a newly added caller from silently restoring /128 behavior. Existing IPv4 keys retain their current suffix, avoiding a gratuitous reset of live IPv4 budgets during rollout.

### 3.2 Fail closed at both auth middleware gates

Every route under `/api/v1/auth/*` uses the fail-closed IP limiter and the parent `AuthBanCheck` becomes fail closed for nil Redis, TTL errors, and timeouts. Both use the existing shared fixed 503 path, add `Retry-After: 1`, abort the Gin chain, and emit `ReasonRateLimitBackendUnavailable`. No handler runs after either admission failure.

`RecordAuthFailure` can still encounter an outage after admission. Its `AuthFailureBackendUnavailable` outcome must cause each producer to stop its ordinary 401/403 response and return the same fixed 503. The caller census is closed and tested; a wrong credential is never answered as an ordinary rejection when its source failure could not be counted. Clearing a counter after successful authentication remains best effort because retaining a stale counter fails toward less guessing, not more.

Admin pre-auth routes retain their outer limiter's present availability posture because their independent account-plus-source `Lockout` is already fail closed. Both layers still use the canonical source bucket. This avoids adding a second outage dependency without weakening the required admin guess bound.

### 3.3 Use the documented binary nginx map

The issue dossier proposed textual first-four-hextet matching. That loses to nginx's documented binary form because compressed IPv6 text has multiple spellings. Define one HTTP-level map:

```nginx
map $binary_remote_addr $source_addr_bucket {
    "~^(\C{8})" $1;
    default      $binary_remote_addr;
}
```

The regex captures the first eight bytes of a 16-byte IPv6 address. A four-byte IPv4 value does not match and remains intact. Use `$source_addr_bucket` for all three `limit_req_zone` keys and both `limit_conn_zone` keys. The real-IP module has already produced `$remote_addr`; #162 owns whether that address is authentic.

This follows the official nginx `map`/`$binary_remote_addr` contracts and Maxim Dounin's nginx.org `/64` example. The implementation must prove the exact config with a real nginx process; a string assertion alone does not prove binary capture behavior.

### 3.4 Media-plane gate has no uncounted production path

Add a small `bucketClientIp(string): string` beside `resolveClientIp`. Use a direct `ipaddr.js` dependency at the lockfile's existing 1.9.1 version instead of writing a second IPv6 parser. `ipaddr.process` canonicalizes IPv4-mapped IPv6; native IPv6 bytes 8–15 are zeroed and reconstructed with `fromByteArray`. No new runtime package is downloaded because the version is already present transitively, but declaring it directly makes the import stable.

`createAdmissionGate` gains `required?: boolean`. `index.ts` passes `required: config.environment === 'production'`. When required, zero valid trusted-proxy rules throw during startup; development/test retains today's one-shot warning and inert gate. A trusted peer with no valid forwarded client address resolves to its observed peer and is counted. After #162, only the actual nginx hop can enter that branch, and the shipped nginx always overwrites the client headers. An untrusted peer remains attributed to itself. A genuinely peerless request is rejected before counting because it has no attributable source.

The gate's 10,000-entry bound, 60-per-10-second threshold, callback contract, and PII-safe diagnostics remain unchanged.

### 3.5 Source first, mirror second

Self-Hosted is canonical for shared service code and its public nginx deployment. Merge and pass its exact-head gates first. Then import the reviewed commit into Hosted, resolve only Hosted-specific nginx/config/documentation differences, and run Hosted's independent quality, security, review, CI, and merge-approval gates. A green source PR is not proof that the mirror is green or deployed.

## 4. Ordering and boundaries

1. Security#162 merges in Self-Hosted and its Hosted mirror. Rebase #163 and rerun the #162 trust regressions.
2. Land the Go bucket helper and all key consumers with unit/integration tests.
3. Make auth admission and post-admission failure recording fail closed.
4. Land media-plane aggregation and production startup refusal.
5. Land nginx aggregation with a real runtime fixture.
6. Update API, architecture, runbook, changelog, and environment comments.
7. Complete Self-Hosted review and merge approval.
8. Create the scheduled Hosted mirror and repeat its gates.

Tasks 2–5 touch independent packages after the helper contract is fixed and can be implemented in parallel worktrees. Their final integration and documentation are serialized. No migration or feature flag is required.

## 5. Detailed behavior

### Control-plane bucket cases

| Input | Key identity |
|---|---|
| `192.0.2.7` | `192.0.2.7` |
| `::ffff:192.0.2.7` | `192.0.2.7` |
| `2001:db8:1:2::1` | `2001:db8:1:2::` |
| `2001:db8:1:2:ffff::99` | `2001:db8:1:2::` |
| scoped IPv6 | same /64 with the zone removed |
| empty or malformed | `unattributed` |

The shared identity reaches `ratelimit:ip:*`, `auth_ban:ip:*`, `auth_failures:ip:*`, and all `admin_lockout:ip*` hashed suffixes. Account axes and user-keyed rate limits are unchanged.

### Fixed backend-unavailable response

- Status: 503.
- Header: `Retry-After: 1`.
- Body: the existing fixed `service unavailable` / `rate-limit backend unavailable; please retry shortly` JSON.
- Handler side effects: none.
- Logs/events: fixed failure class and Nightwatch degraded outcome; never the address or Redis key.

### Media-plane configuration

- Production plus absent, blank, all-invalid, or only `/0` `TRUSTED_PROXIES`: startup refusal.
- Non-production with the same input: one warning, inert gate, preserving local development.
- Trusted proxy plus valid header: bucket the client.
- Trusted proxy plus absent/invalid headers: bucket the observed proxy peer.
- Untrusted peer: ignore forwarded headers and bucket the peer.
- No peer: reject; do not call the ordinary allow path.

## 6. Tests and falsifiability

### Go

- Table-test `IPBucket` for IPv4, mapped IPv4, compressed/native IPv6, 256 addresses in one /64, zones, empty, and malformed input.
- Prove the 256 IPv6 addresses share one `RateLimitByIP` counter and one auth-ban counter; the next request receives 429 with the expected TTL.
- Prove mapped and plain IPv4 share the same counter and admin lockout axis.
- Change the existing auth-ban Redis-down and nil-client tests from pass-through to 503, `Retry-After: 1`, and handler-not-called.
- Route-level login test with a failing Redis client: 503 and no handler side effect.
- One census test pins every route in the `/auth` group to the fail-closed IP limiter.
- Each `RecordAuthFailure` producer has a backend-unavailable regression showing it does not overwrite 503 with 401/403.

### Media plane

- 256 native IPv6 addresses in one /64 consume one handshake counter; the 61st request is denied.
- Plain/mapped IPv4 share a counter.
- Trusted peer without headers is counted under its peer and eventually denied.
- `required: true` throws on absent and all-invalid rules; `required: false` preserves the local inert mode.
- Peerless request is denied; forwarded headers from untrusted peers remain ignored; the store cap remains 10,000.

### nginx

- Static config test asserts every request and connection zone uses `$source_addr_bucket` and no zone remains on `$binary_remote_addr` directly.
- Runtime fixture starts real nginx on disposable ports and proves two native IPv6 loopback addresses in one /64 share the configured request budget while distinct IPv4 addresses remain distinct. If the host cannot bind the IPv6 fixture locally, it reports a local skip but the CI environment must run it; CI may not silently skip.
- `nginx -t` covers the shipped configuration and the blue-green fixture copies the same map.

### Scan evidence

Run Security's `ipv6_rotation_vuln185_test.go` against the fixed branch and retain its result. The fix is accepted when the old exploit expectation fails and the retained positive regressions above pass.

## 7. Rollout and rollback

Deploy #162 before #163. The new keys intentionally merge active IPv6 /128 counters into a fresh /64 key; there is no safe Redis migration because the old keys cannot be enumerated without expanding the deployment's key access and work. The first request starts the new window. IPv4 keys retain their names.

The production media-plane startup refusal makes bad `TRUSTED_PROXIES` visible during readiness instead of after unbounded admission. The compose files already set the variable; docs make it a required operator contract. Rollback must keep #162's trust fix and should revert all #163 aggregation consumers together. Do not roll back only one layer and claim the finding closed.

## 8. Risks

| Risk | Likelihood | Mitigation |
|---|---:|---|
| Shared ISP /64 causes collateral throttling | Medium | Keep existing thresholds; observe rejection metrics; add no /48 until measured evidence exists |
| #163 lands before authentic source derivation | High impact | Hard dependency and rebase gate on #162 plus its regressions |
| A missed raw-IP key builder restores /128 behavior | Medium | Production caller census and AST/text key-prefix guard |
| Fail-closed Redis outage affects auth availability | Expected during outage | Fixed short retry contract, readiness/alerts, document the degradation policy |
| Trusted nginx omits headers and consumes a shared proxy bucket | Low after #162 | Shipped nginx overwrites headers; runtime config test; visible reject metrics |
| nginx binary regex behaves differently on packaged nginx | Low | Runtime fixture using the CI/deployment nginx package |
| Hosted import drifts | Medium | Commit-based reviewed mirror after source merge and independent exact-head gates |

## 9. Acceptance criteria

| Criterion | Required evidence |
|---|---|
| Every named pre-auth counter uses IPv4 /32 or IPv6 /64 | Caller census plus Go, media, and nginx regressions |
| Unattributable/unconfigured production paths are rejected or counted | Media startup, peerless, missing-header tests; Go sentinel tests |
| `/api/v1/auth/*` Redis failures fail closed | Route-level 503, `Retry-After`, handler-not-called test |
| Listed scan exploit no longer reproduces | Retained Security exploit-test result at exact head |
| 0 new blocker/critical issues | Phase-8 review and scanner evidence |
| 0 new high-confidence security hotspots | Exact-head security scan evidence |
| At least 80% new-code coverage | Self-Hosted and Hosted quality-gate reports |
| Docs ship with code | OpenAPI, architecture, quickstart, env comments, changelog reviewed in each owning repo |
| Source and mirror both pass | Independent Self-Hosted and Hosted CI/review/merge approvals |

## 10. References

- Current nginx docs via Context7 `/websites/nginx_en`: `$binary_remote_addr` is 4 bytes for IPv4 and 16 for IPv6; `map` supports regex captures; `limit_req_zone` accepts a variable key.
- nginx.org mailing list, Maxim Dounin, “limit_req_zone for IPv6 subnets” (2021-04-04): the exact `~^(\C{8})` binary `/64` map.
- Current `ipaddr.js` docs via Context7 `/whitequark/ipaddr.js`: `process(string)` canonicalizes IPv4-mapped IPv6 and `fromByteArray(number[])` reconstructs address objects.
- Security#162 and Security#163 live issue contracts, read 2026-11-14.

## 11. Explicit non-goals

- No optional /48 counter, adaptive thresholds, distributed media-plane counter, migration, or new Redis keyspace policy.
- No change to account-keyed lockouts, user-keyed limits, handshake threshold, or store size.
- No redesign of Cloudflare provenance or trusted-hop discovery; #162 and #80 own those boundaries.
