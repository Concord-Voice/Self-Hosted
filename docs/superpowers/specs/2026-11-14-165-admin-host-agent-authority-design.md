# Admin authority and host-agent write authorization — design

**Issue:** Concord-Voice/Security#165 · **Parent:** #151 · **Date:** 2026-11-14 · **Status:** ready for implementation planning

Line numbers and counts in the issue are leads. Implementers must find each site by symbol at the implementation head.

## 1. Intended outcome

An admin session is useful only while its admin remains active. Enrollment reset invalidates every control-plane handle that could survive the reset. Creating another admin requires a new user-verified WebAuthn assertion.

The root host agent independently authorizes every write. A process that can connect to the shared Unix socket cannot start, stop, restart, back up, update, or restore the instance without a fresh assertion for the exact action and arguments. The agent trusts only a root-owned authorizer file, consumes each ceremony once, checks the peer UID, and writes its own audit event before starting a job.

The remaining #165 findings close without taking ownership from Security#41 (admin session generation) or Security#60 (attestation provenance).

## 2. Problem statement

The current control-plane session middleware resolves an opaque Redis session and never reads the referenced admin row. Resetting enrollment removes credentials and marks the admin pending, but leaves sessions, enrollment ceremonies, and server step-up handles live. The create-admin route needs only that stale session. Separately, the host agent treats socket membership as sufficient authority: the control-plane container can invoke six root-running actions and two of them do not even require the control-plane's WebAuthn step-up. The low findings add a logout/session race, ignored WebAuthn clone warnings, unbounded JWKS refresh work, setup-listener slot exhaustion, and unbounded backup creation.

## 3. Scope and ownership

### In scope

- Per-request active-admin validation after session resolution.
- Reset cleanup for outstanding enrollment and control-plane step-up handles.
- Fresh, action-bound WebAuthn step-up for creating an admin.
- A persistent, non-secret notification of admin creation visible to every active admin.
- Agent-local WebAuthn begin/finish, root-owned authorizer trust, single-use action binding, peer-UID admission, and root-owned audit for all six writes.
- Clone-warning rejection at admin login, control-plane step-up, and agent step-up.
- Conditional admin-session sliding that cannot recreate a logged-out session.
- Bounded Cloudflare Access JWKS refresh and response parsing.
- Setup-listener write timeout and per-source connection cap.
- Backup step-up, hourly agent admission, 24-archive refusal, and conservative free-space admission.
- Self-Hosted docs and the reviewed Hosted mirror of shared admin/middleware code.

### Owned elsewhere

- Security#41 owns the per-admin session generation and reset-time generation bump. #165 depends on it and does not add a competing session index.
- Security#60 owns attestation-chain/FIDO metadata provenance. #165 rejects go-webauthn's runtime `CloneWarning`; it does not treat AAGUID as provenance or change enrollment attestation policy.
- The host-agent packages, setup listener, backup script, unit, and trust-store operations remain Self-Hosted only.

### Non-goals

- A second signer service, a new database table, a general admin RBAC model, or a new cryptography implementation.
- Automatically trusting every credential in `admin_webauthn_credentials`.
- Automatically deleting old backups.
- Protecting an operator from a fully malicious admin SPA that lies about the action being approved. The assertion is cryptographically bound to the action and arguments, but WebAuthn authenticators display RP identity, not Concord's semantic action text.
- Solving #60's attestation provenance gap.

## 4. Governing constraints

- `.claude/rules/domains/backend.md`: every error is checked; SQL is parameterized; security/audit failures fail closed; no secret or assertion material is logged.
- `.claude/rules/domains/admin-ui.md`: preserve the existing React/API boundary, accessible status presentation, and tested async states.
- `.claude/rules/domains/tests.md`: happy, error, race, and trust-boundary tests; isolated PostgreSQL/Redis only; at least 80% new-code coverage.
- `.claude/skills/_shared/context7-docs-discipline.md`: verify the installed go-webauthn contract before implementation. Current Context7 documentation confirms that `SessionData` must round-trip intact, `WithUserVerification(protocol.VerificationRequired)` makes UV mandatory, and `Authenticator.CloneWarning` is set when a nonzero counter does not advance.
- Agent trust files and audit files are root-owned regular files. Symlinks, group/other-writable parents, wrong owner/mode, duplicate credential IDs, unknown schema versions, and malformed encodings fail startup or disable writes.
- No migration is required. Existing `admin_audit_log` provides the durable admin-created notice source.
- Shared Go caches remain the defaults. Integration tests use isolated `DATABASE_URL` and `REDIS_URL`.

## 5. Approaches considered

| Approach | Benefit | Cost / rejection |
|---|---|---|
| Read admin public keys from PostgreSQL in the agent | Smallest data-flow change | Rejected. The control-plane container writes that database, so a compromised control plane could add its own credential and grant itself root authority. Read-only access in the agent does not make the data trustworthy. |
| Agent verifies a token signed by the control plane | Simple agent verifier | Rejected. A key available to the compromised control plane recreates the confused deputy. A separate signer would be another privileged service outside this issue. |
| **Root-owned authorizer store plus agent-local WebAuthn ceremony** | The process that executes root actions owns the trust root, ceremony, replay fence, sign counter, and audit | Chosen. It adds explicit operator trust management and makes newly enrolled admins unable to run host writes until root approves their credential. |

Peer credentials remain defense in depth. UID admission alone cannot prove an admin approved a particular root action.

## 6. Reconciled decisions

### 6.1 Conflict: issue Option A vs compromised-database boundary

The issue suggested that the agent could read admin public keys read-only. The panel found that the control plane can still write those rows. The root-owned file wins: PostgreSQL remains the source for console authentication, while the agent authorizer file is the source for host-root authorization.

### 6.2 Conflict: control-plane step-up vs independent agent verification

The existing control plane consumes the assertion and sends plain action arguments to the agent. That cannot satisfy VULN-142. For host writes, the agent now begins and finishes the ceremony itself. The control plane is a transport and audit peer; it never receives a reusable authorization token and cannot substitute arguments after the assertion.

### 6.3 Conflict: session cleanup ownership

Security#41 already specifies a per-admin session generation. That design wins for sessions. #165 adds the hot-path `status == active` database check and handle cleanup. It neither scans `admin_session:*` nor adds another session index.

### 6.4 Conflict: AAGUID handling

Security#60 remains the provenance owner. #165 checks `CloneWarning` everywhere an admin assertion finishes and leaves AAGUID policy unchanged.

### 6.5 Backup retention

No automatic pruning. Deleting the wrong backup is materially worse than returning a clear refusal. At 24 matching archives, backup refuses and tells the operator to move or delete an archive deliberately.

## 7. Control-plane authority

### 7.1 Session middleware

`AdminAuthRequired` receives both `SessionStore` and `AdminRepo`. After `SessionStore.Get` resolves the session and before setting `admin_id`, it loads the admin by ID and requires `StatusActive`. Missing, pending, disabled, or database-error states return 401 and do not call the handler. A database error is logged with a fixed class and no identifier.

Security#41's generation comparison remains inside `SessionStore.Get`. The status read is still required because activation state is durable authority, and because a missed or delayed Redis revocation must not authorize a pending admin.

### 7.2 Logout-safe sliding

`SessionStore.Get` replaces its unconditional `SET` with one atomic Redis operation:

1. read and decode the current value;
2. reject an absent, changed, malformed, idle-expired, or absolute-expired record;
3. update `last_seen` only if the exact loaded value still exists;
4. preserve the TTL clamp to absolute expiry.

A Lua compare-and-set is preferred because it distinguishes logout/replacement from a storage fault. If logout deletes the key between load and slide, `Get` returns `ErrSessionInvalid` and never recreates it.

### 7.3 Reset handles

Enrollment and control-plane action ceremonies gain per-admin Redis index sets:

- `admin_enroll_challenge_index:<admin-id>`
- `admin_action_stepup_index:<admin-id>`

Store and index happen atomically, and the index expires no earlier than its longest member. Consume removes the handle and its set member atomically. Reset atomically deletes every indexed handle and the index before printing new enrollment material. A Redis failure prevents reset success and token output.

The host-agent authorizer is revoked first by the root reset wrapper. The authorizer directory is mounted read-only into the control-plane container; direct `control-plane admin reset-enrollment` refuses while the target admin remains in that root-owned projection. This makes the safe order enforceable for supported operations:

1. root agent removes the admin's authorizer entries;
2. Security#41 bumps the session generation;
3. #165 deletes enrollment/action handles;
4. credentials are deleted and status becomes pending;
5. a new enrollment token is minted and printed.

Any failure after steps 1–3 may cause a safe forced logout or loss of root-write access; it must not restore old authority.

### 7.4 Create-admin step-up and notice

Add a closed control-plane action `create_admin`:

- `POST /admin/api/v1/admins/stepup` begins a two-minute, UV-required assertion for the acting admin and stores it in the indexed action-step-up store.
- `POST /admin/api/v1/admins` requires `stepup.handle` and `stepup.assertion`, consumes the handle once, verifies exact acting admin and action, rejects `CloneWarning`, then hashes the new admin password and calls `CreatePending`.
- The route has a small fail-closed per-admin limiter in addition to the existing session gate.
- Failure before `CreatePending` creates no admin row. A token-mint failure after row creation must remove the pending row or return an explicit recoverable state; the implementation may not silently leave an undisclosed pending admin.
- The committed `EventBootstrap` audit emits the existing privileged-action security event.

`GET /admin/api/v1/security-notices` returns at most 20 sanitized admin-creation audit records from the last 24 hours. It contains actor, target username, fixed event kind, and timestamp; never tokens, passwords, credential IDs, assertions, IPs, or source references. The admin SPA polls it with the existing authenticated refresh cadence and renders an accessible security notice. This reuses the append-only audit table rather than adding notification storage.

## 8. Root-owned authorizer trust

### 8.1 File contract

The canonical file is `/etc/concord-voice/agent-authorizers/authorizers.json`, in a root-owned, non-group-writable directory. Schema 1 contains:

- RP ID and the exact allowed origins;
- a list of `admin_id`, credential ID, COSE public key, sign count, and an operator label;
- a generation and last-updated timestamp for diagnostics.

Public keys are not secrets, but integrity is critical. The file is root-owned mode 0644 under a root-owned mode 0755 directory; the nonroot control plane receives only a read-only mount for reset checks. The agent reads the file with no symlink traversal, validates ownership/mode/schema/uniqueness/bounds, and keeps writes disabled if validation fails. Updates use a same-directory temporary file, `fsync`, atomic rename, and directory `fsync`.

The control-plane container receives a read-only mount of the directory only so reset can prove the target is no longer trusted. It never writes or reloads the file.

### 8.2 Trust management

`concord-agent authorizer list|import|revoke` runs as host root and is the only writer:

- `import` accepts one bounded JSON export from `control-plane admin export-agent-authorizer --username <handle>`, prints the SHA-256 fingerprint, and requires an exact `--confirm-sha256` value before changing the file.
- Import is explicit and idempotent by credential ID. Enrolling or creating an admin does not auto-trust it for host writes.
- `revoke --admin-id` removes every credential for that admin. The supported reset wrapper invokes it before the database reset.
- Existing installations require an explicit one-time import. Until then, host-agent reads continue but all writes return `authorizer_unavailable`.

The root confirmation is an operational trust decision. It prevents a compromised container from silently adding a key; it does not claim independent attestation provenance.

## 9. Agent-local action authorization

### 9.1 Peer UID

The Unix listener records Linux `SO_PEERCRED` in `http.Server.ConnContext`. Requests without credentials or whose UID is not the configured control-plane UID (default 10006) are rejected before routing. The expected UID is resolved from a numeric, validated setting; production refuses UID 0 and broad/missing values.

Tests use a real Unix socket and subprocess credentials on Linux. Non-Linux unit tests exercise the policy through an injected peer-credential reader.

### 9.2 Ceremony

`POST /v1/authorizations` accepts a known action, validated arguments, and acting admin ID. The agent:

1. requires an active authorizer entry for that admin;
2. validates the action and arguments with the same closed parser used by job creation;
3. begins a UV-required WebAuthn login using the root-owned credentials;
4. stores the complete library `SessionData`, admin ID, normalized action request, and expiry in a bounded in-memory store;
5. returns an opaque 256-bit handle plus assertion options.

The store holds at most 64 live ceremonies for two minutes. At capacity it refuses new ceremonies. Restart invalidates all handles.

`POST /v1/actions/<action>` accepts only the handle and assertion. The agent removes the handle before verification, requires the path action to equal the stored action, and executes the stored normalized arguments. It verifies RP ID, allowed origin, allowed credential ID, UV, and expiry through go-webauthn; rejects `CloneWarning`; and atomically persists the advanced sign counter before starting the job. Any trust-store persistence failure refuses the write.

Every six action names require this authorization, including `start` and `backup`.

### 9.3 Agent audit

The agent writes a bounded `privileged_action` security event for every authorization begin refusal, assertion refusal, replay, peer refusal, job admission, and job completion. The file is `/var/log/concord-security/host-agent/events.jsonl`, opened before writes are enabled and written by the root process. If the audit writer cannot open or cannot durably append the pre-job record, writes stay disabled or the request is refused. Events contain fixed action/outcome/reason, opaque admin ID, and job ID when created; never assertion bytes, challenges, credential IDs, tags, archive names, or request bodies.

The existing control-plane audit remains. Two audit streams are intentional because either trust domain may be compromised or unavailable.

## 10. Remaining low findings

### 10.1 Clone warning

After every successful `FinishLoginWithBytes` in admin login and control-plane create-admin step-up, check `cred.Authenticator.CloneWarning` before session mint, action mutation, or sign-count persistence. Reject uniformly and emit a denied security event. The agent applies the same rule before a root job starts.

### 10.2 JWKS unknown-key work

`accessVerifier` uses the already installed `golang.org/x/sync/singleflight`:

- network fetch and JSON parsing happen outside `mu`;
- one successful result replaces the key map under a short lock;
- `lastAttempt`, not only `fetchedAt`, enforces a one-second minimum refresh interval;
- unknown KIDs are negatively cached for one minute, capped at 256 entries;
- the response body is limited to 1 MiB and exactly one JSON document;
- a failed refresh never discards a still-valid cached key, and an unknown KID never blocks readers of known keys.

### 10.3 Setup listener

Retain the global 16-connection cap and add a listener wrapper allowing at most 4 concurrent TCP connections per canonical source IP. Set `WriteTimeout=30s` alongside the existing 5s header, 30s read, and 60s idle timeouts. Closing a connection always releases both counters.

### 10.4 Backup bounds

- The agent admits no more than one backup job per rolling hour. Refusals survive agent restart in a root-owned state file beside the write marker.
- `selfhost-backup.sh` refuses when the target already contains 24 matching regular archive files. It never follows symlinks and never auto-deletes.
- Before staging, it estimates `source_bytes = pg_database_size('concord') + bundled_minio_bytes` (MinIO term is zero for BYO S3) and requires filesystem available bytes to be at least `2 * source_bytes + 1 GiB`. Overflow, probe, or parse failure refuses.
- These checks run before `pg_dump`, container copy, or staging creation and are covered by shell fixtures.

## 11. API and compatibility

- Existing login, enrollment, read-only host-agent, job polling, and restore archive contracts remain.
- Server write UI still calls `/admin/api/v1/server/stepup`; the control plane now proxies the begin request, including normalized action arguments, to the agent.
- Final server-action requests still carry a `stepup` object, but its handle is agent-owned and the assertion is passed through. The agent uses stored arguments, so old clients that omit arguments at begin fail closed and must update with the backend.
- Create-admin callers must perform the new step-up. This is an intentional fail-closed API change.
- Shared admin/session/JWKS changes are mirrored to Hosted after Self-Hosted merges and pass a separate Hosted review/CI/merge gate. Host-agent, setup, backup, unit, and trust-store changes are not mirrored.

## 12. Rollout and recovery

1. Land Security#41 first or rebase #165 on its exact merged session-generation contract.
2. Ship agent binary, systemd/unit packaging, root trust commands, read-only authorizer mount, and control-plane/client changes together.
3. On upgrade, reads remain available and writes fail closed until a root operator imports at least one authorizer.
4. Exercise import/list/revoke and a non-destructive backup in staging.
5. Enable no compatibility flag: partial rollout is unsafe, so the control plane reports `authorizer_unavailable` until the new agent is ready.
6. Mirror shared files to Hosted only after source merge.

Rollback must preserve the root authorizer and audit files. Rolling back to an old agent reopens VULN-142 and is not an acceptable steady state; recover forward.

## 13. Test strategy

| Area | Required proof |
|---|---|
| VULN-137 | Original exploit fails; pending/disabled/missing admin session is 401; pre-reset enrollment/action handles fail; Security#41's two-session and generation race tests remain green. |
| VULN-139 | No/mismatched/replayed/clone-warning step-up creates no row; valid exact action creates one row, one token, one audit event, and one visible notice. |
| VULN-142 | Real socket wrong UID fails; expected UID without/mismatched/replayed authorization starts no job; valid exact assertion starts one job; both agent audit outcomes are present. |
| VULN-135/136 | Clone warning mints no session/action; concurrent logout wins over sliding Get and the key stays absent. |
| VULN-140 | Unknown-KID flood causes bounded fetches; slow fetch does not block known-key verification; oversized/multiple JSON fails without replacing cache. |
| VULN-141 | One source can hold four slots, not all 16; another source is served; write timeout releases the slot. |
| VULN-143 | Backup needs agent step-up; second within an hour, 25th archive, unsafe entry, insufficient/unknown space all refuse before command execution. |
| Trust store | Wrong owner/mode, symlink, duplicate ID, malformed schema, unconfirmed import, failed atomic write, reset-before-revoke all fail closed. |
| UI/docs | Security notice is accessible and sanitized; server action passes arguments at begin; runbooks match exact limits and recovery. |

The Security-repo exploit overlays are acceptance evidence, not substitutes for retained regressions in Self-Hosted.

## 14. Acceptance criteria

| Criterion | Required result |
|---|---|
| Security findings | Every #165 exploit/regression listed above is red on the vulnerable baseline and green after the fix. |
| Sonar blocker/critical | **0 new blocker or critical issues.** |
| Security hotspots | **0 new high-confidence security hotspots.** |
| New-code coverage | **At least 80%.** |
| Quality gate | The repo-declared quality gate, lint, type-check, focused race tests, Go tests, admin client tests, deploy shell tests, and packaging tests pass. If no repo quality-gate command is configured, report that signal as UNVERIFIED rather than claiming pass. |
| Docs to update | `docs/runbooks/selfhost-host-agent.md`, `docs/runbooks/selfhost-backup-restore.md`, `docs/architecture.md`, `CHANGELOG.md`, and any API/client contract docs touched by implementation ship in the same PR. |
| Mirroring | Shared admin/middleware changes have a separately reviewed Hosted mirror; Self-Hosted-only files do not enter Hosted. |

## 15. Predicted Phase-8 routing

- `@red-team`: `risk: security`, auth/authorization content, and the privileged socket boundary.
- `@security-reviewer`: always, with special focus on root trust, replay, reset ordering, and audit failure.
- `@code-reviewer` and `@scope-reviewer`: always.
- `@pr-review-toolkit:silent-failure-hunter`: Go files.
- `@pr-review-toolkit:pr-test-analyzer`: Go, TypeScript, and shell test files.
- `@deployment-config-reviewer`: systemd, Docker Compose, environment consumers, and deploy scripts.
- `@frontend-reviewer`: admin React changes under the repo-specific frontend pattern.
- `@docs-reviewer`: Markdown changes.

No migration reviewer should fire because this design adds no SQL migration. The diff changes API routes, Go packages, test files, admin React, deployment config, and shell fixtures, so Phase 4 must run `./scripts/update-claude-md-counts.sh`.

## 16. Risks

| Risk | Likelihood | Mitigation |
|---|---:|---|
| Root trust import approves the wrong credential | Medium | Exact fingerprint confirmation, explicit one-time import, no automatic DB synchronization, audit, and runbook ceremony. |
| Reset revokes DB state but not agent trust | Medium | Root wrapper revokes first; read-only projection makes direct reset refuse while trust remains; fail closed on every intermediate error. |
| Control plane replays or changes action arguments | High without binding | Agent owns the single-use ceremony and executes the stored normalized request. |
| Sign-counter update fails after assertion | Low | Persist atomically before starting the job; failure refuses. |
| New agent and old control plane roll out separately | Medium | Writes remain unavailable until both support the ceremony; forward-only recovery. |
| Per-request DB status check adds latency/outage coupling | Medium | It is the durable authority check; failures intentionally return 401. Measure but do not cache away the invariant. |
| Backup estimate is conservative | Medium | Clear refusal with measured required/available bytes; operator may move backups or free space. |
| Admin SPA compromise mislabels a signed action | Residual | Action/args are cryptographically bound and audited; a trusted display or separate signer is future work. |

## 17. References

- Concord-Voice/Security#165 and parent #151.
- Security#41 (session generation) and #60 (attestation provenance).
- `.claude/rules/domains/backend.md`, `.claude/rules/domains/admin-ui.md`, `.claude/rules/domains/tests.md`.
- go-webauthn current documentation via Context7: `SessionData`, `WithUserVerification`, and `Authenticator.UpdateCounter` / `CloneWarning`.
- Current source: `internal/admin/{middleware,session,handlers,server_writes,adminctl}.go`, `internal/hostagent/{server,jobs}.go`, `cmd/concord-agent/main.go`, `internal/middleware/cfaccess_jwks.go`, and `infrastructure/deploy/selfhost-backup.sh`.
