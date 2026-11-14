# Admin authority and host-agent write authorization implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Revalidate admin authority at use time and make the root host agent independently require a fresh, single-use WebAuthn authorization for every write.

**Architecture:** The control plane keeps console identity and durable admin state; Security#41 supplies session-generation revocation. The host agent owns a root-only credential store, WebAuthn ceremony, peer-UID gate, sign-counter persistence, and audit, so control-plane socket access alone cannot execute root actions. Existing libraries and audit/storage patterns are reused; no SQL migration or new dependency is added.

**Tech Stack:** Go 1.25, Gin, PostgreSQL, Redis/go-redis, go-webauthn, `golang.org/x/sync/singleflight`, React/Vite/Vitest, Bash deploy fixtures, systemd, Unix sockets.

**Spec:** `docs/superpowers/specs/2026-11-14-165-admin-host-agent-authority-design.md`

## Global constraints

- Start only after Security#41's session-generation contract is merged or rebased into this branch. Do not duplicate its generation/index.
- Security#60 owns attestation provenance. This plan only rejects `CloneWarning`.
- All root writes—`start`, `stop`, `restart`, `backup`, `update`, `restore`—require agent-local authorization.
- Agent trust comes only from the root-owned authorizer file. PostgreSQL credentials cannot grant root authority.
- Every authorization is UV-required, two minutes, single-use, and bound to exact normalized action arguments.
- No new dependency and no SQL migration.
- Use shared Go caches. Integration tests require isolated `DATABASE_URL` and `REDIS_URL`, never the shared `concord` database.
- Self-Hosted is the source. Mirror only shared admin/middleware changes to Hosted after source merge and run independent gates.

## Review focus

- A reset racing an in-flight session or ceremony must leave the old authority unusable; Tasks 2 and 3 own those race tests.
- A compromised socket peer must not change action arguments after the assertion; Task 6 executes only the request stored by Task 5.
- A trust-file rename/sign-counter failure must refuse the write before job creation; Tasks 4 and 6 test both.
- A known cached JWKS key must continue verifying while an unknown-key refresh is slow; Task 9 owns the concurrent test.
- Backup refusal must happen before dump, staging, deletion, or job creation; Task 11 checks the command transcript.

---

### Task 1: Baseline characterization and exploit regressions

**Files:**
- Create: `services/control-plane/internal/admin/security_165_regression_test.go`
- Create: `services/control-plane/internal/hostagent/security_165_regression_test.go`
- Modify: `services/control-plane/internal/admin/server_writes_test.go`
- Modify: `services/control-plane/cmd/concord-agent/main_test.go`
- Modify: `infrastructure/deploy/tests/test-selfhost-backup.sh`

**Interfaces:**
- Consumes: current vulnerable handlers and Security-repo VULN-137/139/142 evidence.
- Produces: retained red tests and a command/caller census; no production change.

- [ ] **Step 1: Record every production caller.** Census `AdminAuthRequired`, `SessionStore.Get`, enrollment/action challenge stores, every `FinishLoginWithBytes`, host-agent action dispatch, setup listener construction, JWKS refresh, and direct/agent backup entry points in the SDD ledger.
- [ ] **Step 2: Add the three exploit-shaped tests.** Name assertions for stale session after pending status, create-admin without step-up, and expected-UID socket request without agent authorization. Assert no handler/row/job mutation.
- [ ] **Step 3: Add low-finding red tests.** Pin logout-vs-slide recreation, clone warning, unknown-KID fetch flood, setup source monopolization, and second backup admission.
- [ ] **Step 4: Run the focused tests.** Expect the new tests to fail for their named security reason; unrelated baseline tests pass. Run the original Security overlays where their imports still match and record any acceptance mismatch rather than rewriting the evidence.
- [ ] **Step 5: Commit tests only.** Commit `test: reproduce admin and host-agent authority gaps`.

### Task 2: Session active-state validation and logout-safe slide

**Files:**
- Modify: `services/control-plane/internal/admin/middleware.go`
- Modify: `services/control-plane/internal/admin/session.go`
- Modify: `services/control-plane/internal/admin/routes.go`
- Modify: `services/control-plane/internal/admin/middleware_test.go`
- Modify: `services/control-plane/internal/admin/session_test.go`
- Modify: `services/control-plane/internal/admin/routes_test.go`

**Interfaces:**
- Consumes: Security#41 generation-aware `SessionStore.Get(ctx, sid)`; `AdminRepo.GetByID(ctx, adminID)`.
- Produces: `AdminAuthRequired(sessions *SessionStore, repo *AdminRepo) gin.HandlerFunc`; compare-and-slide session semantics.

- [ ] **Step 1: Extend red middleware cases.** Active proceeds; pending, disabled, missing, and repository error all abort before the handler. Assert no identifier in logs.
- [ ] **Step 2: Add the logout race test.** Pause between Redis read and slide, revoke the SID, release the slide, then assert `ErrSessionInvalid` and an absent Redis key.
- [ ] **Step 3: Implement exact compare-and-slide.** Add one package-local Lua script that checks the loaded bytes, writes the updated bytes with clamped TTL only on exact match, and reports missing/changed separately from Redis error.
- [ ] **Step 4: Wire the repository into middleware.** Load by session admin ID, require `StatusActive`, then set context. Update all constructors/tests.
- [ ] **Step 5: Verify and commit.** Run `go test -race ./internal/admin -run 'Test(AdminAuthRequired|Session)' -count=1`; commit `fix(admin): revalidate live admin sessions`.

### Task 3: Reset-time handle revocation and #41 integration

**Files:**
- Modify: `services/control-plane/internal/admin/enrollment.go`
- Modify: `services/control-plane/internal/admin/handlers.go`
- Modify: `services/control-plane/internal/admin/adminctl.go`
- Modify: `services/control-plane/internal/admin/adminctl_test.go`
- Modify: `services/control-plane/internal/admin/handlers_enroll_test.go`
- Modify: `services/control-plane/internal/admin/server_stepup_test.go`

**Interfaces:**
- Consumes: Security#41 `SessionStore.RevokeAdmin(ctx, adminID)` or its exact merged generation-bump name; Task 4's read-only authorizer projection.
- Produces: atomic `storeIndexedHandle`, `consumeIndexedHandle`, `revokeIndexedHandles` helpers for enrollment and control-plane action handles.

- [ ] **Step 1: Add handle lifetime/race tests.** Store multiple enrollment and action handles for two admins; reset one; assert only that admin's handles fail. Race consume with reset and prove no handle survives.
- [ ] **Step 2: Add reset-order tests.** A target still present in the root authorizer projection refuses before generation/credential/token mutation. Redis or generation failure produces no token output.
- [ ] **Step 3: Implement indexed handle scripts.** Atomically set/index, get-delete/deindex, and delete all indexed members. Bound handle count and TTL; malformed index members fail reset closed.
- [ ] **Step 4: Integrate the safe reset order.** Authorizer-absence check, Security#41 generation revocation, indexed-handle deletion, credential/status change, then token mint/output.
- [ ] **Step 5: Verify and commit.** Run focused admin race tests against isolated Redis/PostgreSQL; commit `fix(admin): revoke reset enrollment authority`.

### Task 4: Root-owned authorizer store and management commands

**Files:**
- Create: `services/control-plane/internal/hostagent/authorizers.go`
- Create: `services/control-plane/internal/hostagent/authorizers_test.go`
- Create: `services/control-plane/cmd/concord-agent/authorizer.go`
- Modify: `services/control-plane/cmd/concord-agent/main.go`
- Modify: `services/control-plane/cmd/concord-agent/main_test.go`
- Modify: `services/control-plane/internal/admin/adminctl.go`
- Modify: `services/control-plane/internal/admin/adminctl_test.go`
- Modify: `infrastructure/host-agent/concord-agent.service`
- Modify: `docker-compose.host-agent.yml`
- Modify: packaging/deploy manifest files that install the unit and create the directory.

**Interfaces:**
- Produces:
  - `hostagent.LoadAuthorizers(path string) (*AuthorizerStore, error)`
  - `(*AuthorizerStore).Credentials(adminID string) []webauthn.Credential`
  - `(*AuthorizerStore).AdvanceCounter(credentialID []byte, count uint32) error`
  - CLI `concord-agent authorizer list|import|revoke`
  - admin CLI `export-agent-authorizer --username <handle>`

- [ ] **Step 1: Write trust-file rejection tests.** Wrong owner/mode, symlink file/parent, unknown schema, duplicate credential ID, oversized list/field, malformed COSE key, and incomplete atomic write all fail.
- [ ] **Step 2: Write management tests.** Import requires exact fingerprint confirmation, is idempotent by credential ID, and never logs key material; revoke removes all entries for one admin; failed fsync/rename leaves the old file usable.
- [ ] **Step 3: Implement schema-1 load and atomic update.** Reuse standard library file primitives and go-webauthn credential types. Keep limits as constants; do not add a generic config framework.
- [ ] **Step 4: Add bounded admin export.** Output only schema, RP config, admin ID, credential ID/public key/sign counter/label, and fingerprint. No password, token, assertion, IP, or audit source.
- [ ] **Step 5: Wire host paths and read-only projection.** Create the root directory from packaging, pass its path to the agent, mount the directory read-only into control-plane, and preserve read-only behavior on appliance-managed hosts.
- [ ] **Step 6: Verify and commit.** Run hostagent/admin/package fixture tests; commit `feat(host-agent): add root-owned authorizer trust`.

### Task 5: Peer-credential gate and bounded agent ceremonies

**Files:**
- Create: `services/control-plane/internal/hostagent/peercred_linux.go`
- Create: `services/control-plane/internal/hostagent/peercred_other.go`
- Create: `services/control-plane/internal/hostagent/peercred_test.go`
- Create: `services/control-plane/internal/hostagent/authorization.go`
- Create: `services/control-plane/internal/hostagent/authorization_test.go`
- Modify: `services/control-plane/internal/hostagent/server.go`
- Modify: `services/control-plane/cmd/concord-agent/main.go`
- Modify: `services/control-plane/cmd/concord-agent/main_test.go`

**Interfaces:**
- Consumes: Task 4 `AuthorizerStore`; existing `commands(name, actionRequest)` parser.
- Produces:
  - `Peer{PID, UID, GID int}` in request context
  - `NewAuthorizationStore(authorizers, webauthn, now) *AuthorizationStore`
  - `Begin(adminID, action string, req actionRequest) (handle string, options *protocol.CredentialAssertion, error)`
  - `Consume(handle, action string, assertion []byte) (actionRequest, error)`

- [ ] **Step 1: Write real-socket peer tests.** Expected UID reaches reads; wrong UID and unavailable peer credentials receive a fixed denial before routing. Linux integration uses subprocess credentials; unit tests inject the reader.
- [ ] **Step 2: Write ceremony tests.** Unknown admin/action, invalid args, capacity 65, expiry, wrong action, replay, wrong origin/RP/credential, missing UV, clone warning, and counter-write failure all refuse.
- [ ] **Step 3: Add `ConnContext` peer capture.** Resolve expected UID once at startup, refuse root/invalid production configuration, and require exact UID on every request.
- [ ] **Step 4: Add the bounded in-memory store.** Keep complete `SessionData` unchanged, max 64, TTL two minutes, remove-before-verify, and execute stored normalized args.
- [ ] **Step 5: Verify and commit.** Run `go test -race ./internal/hostagent ./cmd/concord-agent -count=1`; commit `feat(host-agent): verify action-bound admin assertions`.

### Task 6: Agent-owned audit and write admission

**Files:**
- Create: `services/control-plane/internal/hostagent/audit.go`
- Create: `services/control-plane/internal/hostagent/audit_test.go`
- Modify: `services/control-plane/internal/securityevent/securityevent.go`
- Modify: `services/control-plane/internal/securityevent/securityevent_test.go`
- Modify: `services/control-plane/internal/hostagent/jobs.go`
- Modify: `services/control-plane/internal/hostagent/jobs_test.go`
- Modify: `services/control-plane/internal/hostagent/server.go`
- Modify: `services/control-plane/cmd/concord-agent/main.go`

**Interfaces:**
- Consumes: Task 5 `AuthorizationStore.Consume`; existing `jobs.start`.
- Produces: host-agent security-event service/path and `Audit.WriteAdmission(...) error`.

- [ ] **Step 1: Add audit failure tests.** Writer-open failure disables writes; pre-job append failure starts no job; denied, replayed, started, and completed outcomes write bounded fields.
- [ ] **Step 2: Add mutation-order tests.** Assert the sequence is parse → peer → consume/verify → counter persist → audit persist → marker/job start. Inject failure at each seam and assert no later mutation.
- [ ] **Step 3: Add the host-agent event identity.** Extend the existing security-event writer with a fixed `host-agent` service and root path; retain its framing/size/sanitization rules.
- [ ] **Step 4: Require authorization for all six actions.** Delete the boolean distinction that exempts start/backup. `handleWrite` accepts only handle/assertion and executes Task 5's stored args.
- [ ] **Step 5: Verify and commit.** Run hostagent/securityevent race tests; commit `fix(host-agent): gate every root write locally`.

### Task 7: Control-plane proxy and admin server-action UI

**Files:**
- Modify: `services/control-plane/internal/admin/server_writes.go`
- Modify: `services/control-plane/internal/admin/server_writes_test.go`
- Modify: `services/control-plane/internal/admin/server_stepup_test.go`
- Modify: `client/admin/src/server.ts`
- Modify: `client/admin/src/ServerActions.tsx`
- Modify: `client/admin/src/ServerActions.test.tsx`

**Interfaces:**
- Consumes: agent `POST /v1/authorizations` and authorized `POST /v1/actions/<action>`.
- Produces: unchanged browser endpoint paths with begin body `{action, tag?, archive?}` and final body `{stepup:{handle, assertion}}`.

- [ ] **Step 1: Update tests first.** Assert tag/archive are sent at begin, omitted from final, every action prompts for a security key, and agent begin/final failures preserve fixed user-facing errors.
- [ ] **Step 2: Replace local server-action verification with proxying.** Bound both agent response bodies, pass through only known fields/statuses, and retain control-plane requested/outcome audit.
- [ ] **Step 3: Update UI action flow.** Collect arguments before begin, invoke WebAuthn, submit handle/assertion, and keep existing job polling/accessibility behavior.
- [ ] **Step 4: Verify and commit.** Run focused Go tests and `npm test -- --run ServerActions`; commit `fix(admin): proxy host write authorization to agent`.

### Task 8: Create-admin step-up and persistent notices

**Files:**
- Create: `services/control-plane/internal/admin/action_stepup.go`
- Create: `services/control-plane/internal/admin/action_stepup_test.go`
- Modify: `services/control-plane/internal/admin/handlers.go`
- Modify: `services/control-plane/internal/admin/handlers_enroll_test.go`
- Modify: `services/control-plane/internal/admin/routes.go`
- Modify: `services/control-plane/internal/admin/routes_test.go`
- Modify: `services/control-plane/internal/admin/audit.go`
- Modify: `services/control-plane/internal/admin/audit_test.go`
- Modify admin client API/Portal files only as needed to render the notice.

**Interfaces:**
- Consumes: Task 3 indexed action-step-up store; existing WebAuthn service and audit table.
- Produces: `POST /admin/api/v1/admins/stepup`, protected `POST /admins` step-up request shape, `GET /security-notices`.

- [ ] **Step 1: Add create-admin denial tests.** Missing, wrong-admin, wrong-action, expired, replayed, non-UV, clone-warning, and limiter-store-failure cases create no row and no token.
- [ ] **Step 2: Add token-mint compensation test.** Force mint failure after insert and assert no undisclosed pending admin remains.
- [ ] **Step 3: Add notice tests.** Only sanitized committed admin-created events from 24 hours are returned, max 20; all active sessions see them; tokens and source data never appear.
- [ ] **Step 4: Implement the closed action and route limiter.** Reuse the package WebAuthn user builder and indexed store. Verify before password hashing/insert.
- [ ] **Step 5: Render the accessible notice.** Poll through the existing authenticated client cadence; announce a new notice without stealing focus.
- [ ] **Step 6: Verify and commit.** Run focused admin and client tests; commit `fix(admin): require step-up to create administrators`.

### Task 9: Clone-warning coverage and JWKS bounds

**Files:**
- Modify: `services/control-plane/internal/admin/handlers.go`
- Modify: `services/control-plane/internal/admin/handlers_auth_test.go`
- Modify: `services/control-plane/internal/middleware/cfaccess_jwks.go`
- Modify: `services/control-plane/internal/middleware/cfaccess_jwks_test.go`

**Interfaces:**
- Consumes: installed go-webauthn credential result; installed `singleflight.Group`.
- Produces: uniform clone-warning rejection and bounded `accessVerifier.lookupKey`.

- [ ] **Step 1: Add login clone-warning test.** No session, sign-count update, or success audit occurs; denial is uniform.
- [ ] **Step 2: Add JWKS concurrency/bounds tests.** Known key remains fast during slow unknown fetch; one fetch per burst; one-minute negative cache max 256; 1 MiB+1 and trailing JSON fail; failed refresh preserves good keys.
- [ ] **Step 3: Implement the smallest clone check.** Check immediately after finish and before every success mutation.
- [ ] **Step 4: Refactor refresh around singleflight.** Fetch/decode outside the mutex, record last attempt, then swap the validated nonempty map under lock. Keep cache semantics explicit.
- [ ] **Step 5: Verify and commit.** Run `go test -race ./internal/admin ./internal/middleware -count=1`; commit `fix(security): reject cloned admin keys and bound jwks refresh`.

### Task 10: Setup-listener connection fairness

**Files:**
- Create: `services/control-plane/cmd/concord-agent/per_source_listener.go`
- Create: `services/control-plane/cmd/concord-agent/per_source_listener_test.go`
- Modify: `services/control-plane/cmd/concord-agent/main.go`
- Modify: `services/control-plane/cmd/concord-agent/main_test.go`

**Interfaces:**
- Produces: `limitPerSource(inner net.Listener, max int) net.Listener`, max 4 per canonical source; setup `WriteTimeout=30*time.Second`.

- [ ] **Step 1: Add two-source test.** Four held connections from source A are allowed, the fifth refuses/releases promptly, and source B still completes a request under global 16.
- [ ] **Step 2: Add timeout/close tests.** A blocked response and TLS/header failure release the per-source slot exactly once.
- [ ] **Step 3: Implement the listener wrapper.** Canonicalize `net.TCPAddr.IP`; wrap `net.Conn.Close` with `sync.Once`; leave non-TCP setup listeners unsupported/fail closed.
- [ ] **Step 4: Wire timeout and listener order.** Per-source cap inside the existing global cap, then TLS. Keep existing setup window/shutdown behavior.
- [ ] **Step 5: Verify and commit.** Run cmd/concord-agent tests under race; commit `fix(host-agent): bound setup connections per source`.

### Task 11: Backup rate, retention, and space admission

**Files:**
- Modify: `services/control-plane/internal/hostagent/jobs.go`
- Modify: `services/control-plane/internal/hostagent/jobs_test.go`
- Modify: `infrastructure/deploy/selfhost-backup.sh`
- Modify: `infrastructure/deploy/tests/test-selfhost-backup.sh`

**Interfaces:**
- Produces: one backup admission per rolling hour persisted beside the write marker; script constants max 24 archives, 1 GiB reserve, `2*source+reserve` requirement.

- [ ] **Step 1: Add agent rate tests.** First accepted, concurrent/second refused, restart still refuses, failed job still spends admission, 60-minute boundary allows.
- [ ] **Step 2: Add shell admission fixtures.** 24 vs 25 archive files, symlink/nonregular entries, BYO vs bundled source estimates, overflow/probe failure, insufficient/exact free bytes. Assert no `pg_dump`, Docker copy, staging, or delete on refusal.
- [ ] **Step 3: Implement persistent agent admission.** Use a root-owned timestamp file with no symlink following and atomic update before job creation.
- [ ] **Step 4: Implement shell checks before staging.** Query PostgreSQL size and bundled volume size with existing trusted tools; parse decimal integers; use overflow-safe arithmetic; count only exact regular archive names; never prune.
- [ ] **Step 5: Verify and commit.** Run hostagent race tests and `bash infrastructure/deploy/tests/test-selfhost-backup.sh`; commit `fix(selfhost): bound backup creation`.

### Task 12: Documentation, full validation, and mirror handoff

**Files:**
- Modify: `docs/runbooks/selfhost-host-agent.md`
- Modify: `docs/runbooks/selfhost-backup-restore.md`
- Modify: `docs/architecture.md`
- Modify: `CHANGELOG.md`
- Modify API/client contract documentation touched by Tasks 7–8.

**Interfaces:**
- Consumes: exact implemented paths, limits, errors, rollout ordering, and Security#41/#60 ownership.
- Produces: operator trust-import/reset/recovery runbook and Hosted mirror manifest.

- [ ] **Step 1: Document the trust boundary.** Exact authorizer path/modes, import fingerprint, explicit trust after enrollment, reset order, fail-closed errors, two audit streams, and residual malicious-UI risk.
- [ ] **Step 2: Document backup limits.** One/hour, 24 archives, no pruning, free-space formula, and operator recovery.
- [ ] **Step 3: Update architecture/changelog/API.** Active-status validation, session generation dependency, create-admin step-up/notices, agent-local ceremony, peer UID, JWKS/setup bounds.
- [ ] **Step 4: Run focused and package tests.** Admin, middleware, hostagent, concord-agent race suites; admin client tests; backup/deb/deploy fixtures; original exploit overlays.
- [ ] **Step 5: Run repo gates.** `golangci-lint`, TypeScript lint/type-check, `git diff --check`, `./scripts/update-claude-md-counts.sh`, and the repo quality gate. Record the quality gate as UNVERIFIED if the trusted repo config declares none.
- [ ] **Step 6: Run required reviews.** Red team with executable PoCs, security, code, silent-failure, test quality, deployment config, frontend, docs, and scope review. Resolve every blocking finding and rerun affected tests.
- [ ] **Step 7: Prepare the Hosted mirror manifest.** List only shared `internal/admin` and `internal/middleware` commits/files; exclude hostagent, cmd/concord-agent, Self-Hosted deploy/unit/client differences unless independently present. Open and gate the mirror only after Self-Hosted source merge.
- [ ] **Step 8: Commit docs.** Commit `docs: describe admin and host-agent authorization`.

## Self-review result

- Spec coverage: every #165 finding is assigned; #41 and #60 remain explicit dependencies rather than duplicate implementations.
- Type consistency: Task 4 authorizer interfaces feed Task 5, Task 5 feeds Task 6, and Task 7 preserves browser paths while changing the agent trust owner.
- Review-focus tests appear in their owning tasks.
- No migration, second signer, generic notification framework, automatic backup pruning, or new dependency was added.
- The plan intentionally splits trust store, ceremony, write admission, UI proxy, and low findings because each can be reviewed/rejected independently.
