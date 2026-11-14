# Security #164 Low Authentication Gaps Design

**Issue:** `Concord-Voice/Security#164`
**Parent:** `Concord-Voice/Security#151`
**Owning source:** `Concord-Voice/Self-Hosted`
**Mirror:** `Concord-Voice/Hosted` after each source PR merges
**Status:** Final Phase 2 design

## Intent

Six low-severity authentication gaps currently let a prerequisite-bearing
attacker bypass a defense-in-depth control or spend disproportionate server
work. A seventh finding, refresh machine binding, belongs to Security #39 and
is a dependency verification here rather than duplicate implementation. This
design closes the remaining gaps at their existing seams: WebAuthn assertion
results, the transaction that stores credentials, backup-code encoding, the
two MFA method-list handlers, registration parsing, and the WebSocket JWT
fallback.

Success means each control does what its surrounding code already claims:
clone warnings deny authentication without advancing the counter; the ten-key
cap is enforced under the same user lock as insertion; new backup codes have
independent salts and a bounded slow hash while legacy codes remain usable;
method arrays are small closed sets; registration rejects oversized input
before Argon2id; and a JWT-authenticated WebSocket cannot skip enabled client
attestation. No new authentication product, factor, route, config flag, or
dependency is introduced.

## Scope and blast radius

| Finding | Source surfaces | Tests and docs |
|---|---|---|
| VULN-009 | No #164 production edit; verify Security #39 on `internal/auth` | #39 exact-head test evidence |
| VULN-021 | `internal/mfa/handlers.go`; existing security-event vocabulary only | MFA WebAuthn and security-event tests |
| VULN-026 | `internal/mfa/handlers.go`; existing `withMFAFactorWriteTx` | real PostgreSQL concurrent finish test |
| VULN-022 | `internal/mfa/totp.go`, backup-code callers, migrations `000195_*` | unit, integration, migration up/down tests; architecture docs |
| VULN-028 | `internal/mfa/handlers.go` | handler tests; OpenAPI constraints |
| VULN-002 | `internal/auth/handlers.go`, `pending.go` | registration boundary and work-order tests; OpenAPI limits |
| VULN-192 | `internal/middleware/attestation.go`, `internal/websocket/handler.go`, router wiring tests | JWT/ticket WebSocket tests; OpenAPI and architecture docs |

The Self-Hosted migration number is `000195` at the inspected `origin/main`
head (`b3b7f490`). Hosted currently ends at `000197`, so its reviewed mirror is
expected to use `000198`. Both numbers are reservations, not constants: each
implementer must re-resolve the next free number immediately before creating
the migration.

No Desktop code change is required. The existing Desktop registration producer
uses RSA-4096 SPKI, an AES-GCM wrapped PKCS#8 private key, and a password KDF
salt. The proposed limits are comfortably above those bounded objects and are
pinned by a representative contract fixture.

### User-visible compatibility

- Existing valid WebAuthn assertions, including authenticators whose counters
  remain zero, continue to work. The library sets `CloneWarning` only when a
  non-zero counter participates in a regression/equality warning.
- Existing SHA-256 backup-code rows continue to verify. New setup or
  regeneration replaces the entire set with v2 entries.
- Duplicate MFA methods become one method. Unknown methods remain a 400.
- Valid current Desktop registration payloads stay below every limit.
- When client attestation is enabled, legacy JWT WebSocket fallback now needs
  the same attestation headers as authenticated HTTP. Ticket connections remain
  unchanged because the ticket-mint request already passes the HTTP gate.

## Governing constraints

| Constraint | Source | Design consequence |
|---|---|---|
| Check every error; fail closed on auth dependencies; parameterized SQL | `.claude/rules/domains/backend.md` | malformed hashes, Redis failure, count read failure, and attestation failure cannot authenticate |
| Users row precedes MFA factor rows | `.claude/rules/domains/backend.md`, migration 000189 | finish-time cap runs inside `withMFAFactorWriteTx`, after its users-row lock |
| Up/down symmetry and guarded destructive rollback | `.claude/rules/migrations.md`, `.claude/rules/domains/migrations.md` | v2 rows make the backup-hash downgrade refuse rather than silently corrupt codes |
| Real integration DB, race-safe tests, ≥80% new-code coverage | `.claude/rules/domains/tests.md` | concurrency tests use isolated PostgreSQL with barriers, not sleeps or the shared `concord` DB |
| Current third-party contract must be verified | `.claude/skills/_shared/context7-docs-discipline.md` | implementation pins behavior against the repository's go-webauthn version; current docs confirm `CloneWarning` and returned `SignCount` semantics |
| Source owns `services/**`; Hosted imports reviewed source changes | org split contract | no hand-port into Hosted before the corresponding Self-Hosted PR merges |

The repository has `.claude/rules/**`, not `.codex/rules/**`; the imported rule
files are the applicable source of truth in this checkout.

## Approaches considered

### A. Patch each current seam, split into three review units — selected

Keep the existing handler and transaction ownership. Land three source PRs:

1. WebAuthn clone rejection and finish-time credential cap.
2. Versioned backup-code hashes and bounded MFA method sets.
3. Bounded registration and JWT WebSocket attestation.

Each PR carries its focused tests and docs. This follows the issue dossier,
keeps migration/storage review separate from WebSocket admission, and lets a
reviewer reject one control without holding unrelated fixes. The cost is three
source and three mirror gates.

### B. One six-finding PR

This reduces branch coordination and one-time CI overhead, but combines a
schema/data rewrite, WebAuthn semantics, unauthenticated resource bounds, and
WebSocket admission. The resulting review surface is too broad for security
work and raises the chance that a rollback or mirror conflict couples unrelated
controls.

### C. A generic request-contract and credential-storage framework

A shared generic bounded-set parser and versioned-secret framework could serve
future routes. It would create abstractions before a second concrete consumer
exists and would expand behavior outside the six named paths. This is rejected
under YAGNI; small package-local helpers are sufficient.

### D. Pepper-based backup hashes

HMAC-SHA-256 with a server pepper avoids repeated Argon2 work, but adds a new
secret lifecycle and deployment contract. A database compromise would still be
contained, but a secret-sync or rotation failure could lock users out. The
selected per-code salted Argon2id format adds no operator secret and satisfies
the issue's storage requirement.

## Final design

### 1. WebAuthn clone warnings deny before persistence

Both assertion consumers inspect
`credential.Authenticator.CloneWarning` immediately after successful
`FinishLogin` / `FinishLoginWithBytes` and before `sign_count`,
`last_used_at`, challenge completion, or inline-token minting.

When it is true:

- return the route's existing generic verification failure;
- do not write the returned `SignCount`;
- do not mint a session or step-up token;
- emit the existing closed-schema WebAuthn challenge-denied security event;
- log only a fixed failure class and user identifier already used by these
  handlers, never credential ID, assertion, public key, or counter values.

Conflict resolved: a dedicated `authenticator_clone_suspected` event reason
would improve categorization, but it expands the shared event schema and every
consumer. The existing `challenge_invalid` + `AuthWebAuthn` event is already a
security event and is the smallest complete contract. If operations later need
clone-specific alerting, that is a separate schema change.

The implementation must verify the exact pinned go-webauthn v0.18.2 behavior
before coding. Context7's available v0.17.4 contract and the inspected pinned
v0.18.2 source agree: validation calls `UpdateCounter`, returns the mutated
credential, and sets `CloneWarning` when the new counter is less than or equal
to the stored counter and either counter is non-zero. The relying party decides
whether to accept and persist that result, so rejection must precede the write.

### 2. The credential cap is checked under the insert lock

The begin-time count stays as an early UX rejection. It is not authoritative.
`WebAuthnRegisterFinish` already inserts through `withMFAFactorWriteTx`, which
opens a transaction and locks the user's row before invoking its write closure.
Inside that closure:

1. count the user's `user_mfa_webauthn` rows;
2. return a sentinel when the count is already 10;
3. otherwise insert the credential and sync MFA flags in the same transaction.

The handler maps the sentinel to the existing 409 limit response. All database
errors remain 500 and no credential or flag mutation commits. Because every
finish for one user takes the same users-row lock, two finishes starting at
nine credentials serialize and leave at most ten. No new table constraint or
lock order is needed.

### 3. Versioned, salted backup-code hashes

`backup_codes_hash TEXT[]` remains the storage column. Each element becomes a
closed versioned string:

- `v1$<64 lowercase hex characters>` — the existing unsalted SHA-256 value;
- `v2$argon2id$v=19$m=8192,t=1,p=1$<base64url salt>$<base64url digest>` — a
  16-byte random salt and 32-byte digest for one normalized code.

The v2 parameters are deliberately bounded and exact. A single submitted code
may be tested against up to eight independent salts, so password-grade 64 MiB
parameters would multiply into an unauthenticated CPU/memory denial. The codes
already have a fail-closed per-account attempt budget from Security #157. Eight
8 MiB, one-pass computations retain a meaningful database-only cracking cost
without multiplying one valid request into half a gigabyte of memory. The
parser rejects any different algorithm, version, parameter tuple, salt length,
digest length, or malformed encoding before calling Argon2.

Generation writes only v2. Verification accepts both exact formats, uses
constant-time digest comparison, skips used indices, and returns an error for a
malformed stored element. A storage error fails the verifier closed. The
existing atomic consumption mutation remains the sole owner of marking a code
used; #164 does not redesign Security #35.

Migration `000195` rewrites every bare 64-hex legacy element to `v1$...` in
place while preserving array order and `backup_codes_used`. It does not hash or
log plaintext because none exists. Its down migration strips `v1$` only after
holding the table lock and proving that no v2 element exists; if any v2 code
has been generated, downgrade aborts. Silently discarding v2 codes would lock
users out and is forbidden.

### 4. MFA method inputs become bounded closed sets

One package-local normalizer accepts a raw slice, an allowed closed set, and
the route's maximum unique count. It rejects an overlong raw array before any
Redis write or loop, rejects unknown values, and returns first-seen unique
values.

- `PUT /mfa/recovery-only`: raw length at most 4; allowed values are `totp`,
  `webauthn`, `email`, `sms`; the existing enabled-factor filter still decides
  which valid methods may be stored.
- `POST /mfa/email-sms/setup`: raw length at most 2; allowed values are `email`,
  `sms`; at least one unique value is required.

Thus a 1,000-entry duplicate input returns 400 before code generation, while a
small duplicate input produces one code and one stored key. The response uses
the normalized slice. No generic validation framework is added.

### 5. Registration is bounded before expensive work

`POST /auth/register` applies a 64 KiB `http.MaxBytesReader` before its first
body read and maps `*http.MaxBytesError` to 413. Its bounded binder requires one
JSON value plus EOF and runs the existing Gin struct validation. Unknown fields
remain tolerated for released-client compatibility.

Before any base64 allocation, decoded-length checks enforce:

| Field | Decoded maximum | Reason |
|---|---:|---|
| `public_key` | 1 KiB | RSA-4096 SPKI is far smaller; X.509 parsing still enforces RSA-4096 |
| `wrapped_private_key` | 32 KiB | Desktop AES-GCM-wrapped RSA-4096 PKCS#8 is a few KiB |
| `key_derivation_salt` | 64 bytes | current Desktop salt is fixed and much smaller |

The order is fixed:

1. bounded JSON parse and tag validation;
2. age, username, password length/strength, normalized email syntax, and key
   field encoded/decoded-length checks;
3. base64 decode and RSA-4096 public-key parse;
4. cheap registered email/username conflict query;
5. `HashPassword`;
6. the existing transaction-bound pending takeover/collision logic.

The cheap conflict query reuses the same users-table uniqueness helper as
`InsertOrTakeover`; the transaction repeats it to close races. Registration
already returns distinct email/username conflicts, so moving this known oracle
earlier does not add a new enumeration class. No DNS/MX lookup is introduced.

Tests inject a password-hash function or use an existing test hook to prove
413 and cheap conflict paths execute zero Argon2id calls. The production
default remains the existing `HashPassword`; no configurable KDF is added.

### 6. JWT WebSocket fallback uses the HTTP attestation validator

Attestation validation logic is extracted from `RequireAttestation` into one
middleware-package function used by both the HTTP wrapper and WebSocket JWT
authentication. It retains the current token lookup, constant-time comparison,
revocation check, user binding, and 403/503 behavior.

`authenticateViaJWT` first validates the access token, then places the verified
user ID and server-verified `sid` in the same Gin context keys used by auth
middleware (`user_id` and `JWTClaimsContextKey`), then invokes that shared
validator when `RequireClientAttestation` is enabled. This does not itself
replace the current client-selected attestation namespace; Security #44 owns
that change. A failure aborts the HTTP handshake before upgrade or client
allocation. The handler receives the existing config boolean through
constructor wiring; no environment variable is added.

The ticket path does not run a second attestation lookup. Tickets can only be
minted through `/auth/ws-ticket`, which is already inside `authRequired` after
`RequireAttestation`; they are short-lived and single-use. Rechecking at ticket
redemption would add a Redis dependency and could break clients whose
attestation token expires after ticket mint. Tests pin both facts: enabled JWT
without attestation is rejected, and a valid ticket retains its current path.

Conflict resolved: moving `/ws` under `authRequired` appears simpler, but that
middleware expects the bearer before the handler's ticket/JWT classifier and
would either reject tickets or apply the wrong proof twice. The shared validator
inside only the JWT branch preserves both rails.

Security #44 will later replace caller-selected attestation namespaces with
the authoritative JWT `sid`. The shared validator is the integration seam;
#164 must not create a second key derivation or freeze today's header keying in
WebSocket code.

## PR and rollout order

1. Security #39 lands and proves VULN-009. #164 records that exact-head result;
   it does not duplicate the fix.
2. **PR A:** VULN-021 + VULN-026 (WebAuthn).
3. **PR B:** VULN-022 + VULN-028 (MFA storage/input). It may land after A and
   must coordinate with Security #35 if that issue edits the same verifier.
4. **PR C:** VULN-002 + VULN-192 (registration/WS). It coordinates with
   Security #44 at the shared attestation validator.
5. After each Self-Hosted source PR merges, create the scheduled reviewed Hosted
   mirror PR from the merged source commit. Re-resolve migration numbering and
   run Hosted's own hooks, CI, reviews, and Phase 9 approval.

No deployment or live acceptance is implied by source merge. Client attestation
is default-off in Self-Hosted; its enabled-mode handshake must still be tested.

## Error and privacy contract

- Clone warnings and invalid backup codes expose only the current generic
  verification response.
- No backup code, hash, salt, WebAuthn credential ID, assertion, public key,
  attestation token, or registration key blob enters logs.
- Malformed stored backup hashes fail closed and log a fixed failure class.
- Oversized registration returns 413 before key decoding, conflict lookup, or
  Argon2id. Other invalid request bodies retain 400.
- Credential-cap races return one success and one 409 at nine existing keys.
- Attestation registry failure returns 503 and cannot upgrade the WebSocket.

## Testing strategy

Each production branch starts with a failing regression. Use the repository's
isolated PostgreSQL/Redis test harness; set `DATABASE_URL` and `REDIS_URL` to
task-specific disposable services, never the shared `concord` database. Shared
Go caches remain at their defaults.

Required proof:

- CloneWarning true in login and inline assertion flows: generic denial, no
  sign-count write, no session/token, denied WebAuthn event.
- CloneWarning false with zero and increasing counters: existing success.
- Two barrier-synchronized finishes from nine credentials: final count 10.
- v1 fixture verifies and consumes once; v2 entries have distinct salts and
  verify; malformed/oversized parameter strings fail without Argon allocation;
  regeneration makes old v1 codes unusable.
- Migration up marks v1 without changing order/used flags; guarded down works
  before v2 and refuses after v2.
- 1,000 duplicate methods returns 400 and produces no Redis code; small
  duplicates normalize to one.
- 64 KiB boundary and +1; oversized individual decoded blobs; representative
  Desktop payload; 413/conflict paths observe zero password hashes.
- Attestation enabled: JWT missing/invalid/Redis-failure cases deny before
  upgrade; valid JWT attestation succeeds; ticket path is unchanged.
- VULN-009 verification cites Security #39's exact tested head.

## Phase 8 reviewers predicted

| Reviewer | Trigger |
|---|---|
| `@security-reviewer` | always |
| `@code-reviewer` | always |
| `@scope-reviewer` | always, advisory |
| `@red-team` | `risk: security`, `internal/auth/**` |
| `@migration-reviewer` | `services/control-plane/migrations/*.sql` in PR B |
| `@pr-review-toolkit:silent-failure-hunter` | Go service files |
| `@pr-review-toolkit:pr-test-analyzer` | `*_test.go` changes |
| `@docs-reviewer` | Markdown changes |
| `@e2ee-reviewer` | PR C content contains registration `wrappingKey`/key-material lifecycle semantics, even though filenames do not match a crypto glob |

RBAC and Socket.IO reviewers do not fire from the predicted source diff. The
WebSocket change is the Go control plane, not `services/media-plane/**`.

## Count-bearing surfaces

- Migration files: yes (PR B, one up/down pair).
- API routes: behavior and OpenAPI contracts change, but no route is added or
  removed.
- Test files: yes in all three PRs.
- Go packages: existing packages only; package count unchanged.
- Zustand stores and CSS theme blocks: no.

Phase 4 must run `./scripts/update-claude-md-counts.sh` when present. This
Self-Hosted checkout does not currently contain that script or a root
`CLAUDE.md` Key Counts block, so absence is recorded rather than treated as a
successful probe.

## Documentation to update

- `CHANGELOG.md` under `[Prerelease]`, in each PR.
- `docs/api/openapi.yaml`: registration body/field limits, method uniqueness and
  item caps, WebSocket JWT attestation requirement, stable 413/400/403/503
  responses.
- `docs/architecture.md`: versioned backup-code storage and JWT-vs-ticket
  WebSocket attestation boundary.
- The migration's comments and `TEST_MIGRATIONS.md` fixture coverage.

Docs ship in the PR that changes their contract; no follow-up docs PR.

## Acceptance criteria

| Criterion | Required evidence |
|---|---|
| VULN-009 remains owned by #39 | #39 exact-head regression result; no duplicate #164 production edit |
| Clone warning denied | login and inline tests show no counter write or authority mint |
| Write-time cap | real PostgreSQL concurrent finish test leaves at most ten |
| Salted backup storage | distinct v2 salts; v1 compatibility; guarded migration and regeneration proof |
| Bounded method sets | 1,000-entry case stops before Redis; duplicate small set creates one code |
| Bounded registration | +1 body returns 413 and zero hashes; each decoded field bound; Desktop fixture succeeds |
| WS attestation parity | enabled JWT no-attestation denial and valid-attestation success; ticket regression green |
| Docs | OpenAPI, architecture, changelog, and migration test docs ship with code |
| New blocker/critical issues | **0** |
| New high-confidence security hotspots | **0** |
| New-code coverage | **≥80%** |
| Source quality gate | lint, race-focused tests, migration checks, PR CI, and required ruleset checks pass on exact head |
| Mirror | each Hosted mirror passes independent lint/tests/CI/review and receives separate Phase 9 approval |

## Non-goals

- Implementing VULN-009 outside Security #39.
- Solving #35 backup-code claim atomicity, #36 ceremony consumption, #44
  attestation namespace ownership, or #53 WebSocket connection capacity.
- Adding a pepper, secret rotation, generic validation framework, new MFA
  method, new route, new dependency, or Desktop UI.
- Claiming deployment or live attestation acceptance from source tests.

## Risk register

| Risk | Likelihood | Mitigation |
|---|---:|---|
| Backup verification becomes a resource amplifier | Medium | exact low Argon2 tuple, eight-code ceiling, #157 account budget, malformed-parameter precheck |
| Downgrade destroys v2 codes | High impact | table lock + refuse down while any v2 entry exists |
| Finish cap races at nine | High without lock | count and insert after the existing users-row lock in one transaction |
| Zero-counter authenticators are falsely rejected | Low | branch only on library `CloneWarning`; zero/increasing counter regression tests |
| Registration cap breaks Desktop | Low | limits derived from producer format and representative client fixture |
| Attestation is checked twice or tickets break | Medium | validate only JWT fallback; ticket regression test and route-wiring test |
| #44 changes the namespace contract mid-branch | Medium | one shared validator seam; rebase and rerun both HTTP/WS tests |
| Source/Hosted migration numbers diverge | Certain | re-resolve per repo; mirror content, not numeric filename |
| Overlap with #35/#36 overwrites atomicity fixes | Medium | rebase before each PR; retain GETDEL and conditional backup consumption tests |
