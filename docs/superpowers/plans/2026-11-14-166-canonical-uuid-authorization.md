# Security #166 Canonical UUID Authorization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make UUID spelling unable to change an authorization decision, key revocation, or emitted identity on the six confirmed Security #166 paths.

**Architecture:** Normalize IDs at the affected HTTP boundaries and again in shared resolver, hierarchy, and revocation code. Use canonical keys for the batch permission map and a same-statement creator guard for group-DM role changes. Keep string APIs and existing transaction ownership.

**Tech Stack:** Go, Gin v1.12.0, google/uuid v1.6.0, PostgreSQL.

**Spec:** `docs/superpowers/specs/2026-11-14-166-canonical-uuid-authorization-design.md`

## Global Constraints

- Preserve valid UUID spellings accepted today; malformed IDs fail before writes.
- Preserve permission precedence, transaction/lock order, status codes, and hidden-resource error privacy.
- Use the shared default Go caches. Use only an isolated `DATABASE_URL` for PostgreSQL tests.
- Use an isolated issue worktree and do not touch unrelated dirty checkouts.
- The six supplied scan exploit tests must stop reaching their attacker goals.

## Review Focus

- A batch containing the same UUID in two spellings must resolve once and return one canonical key.
- Malformed cached epoch keys are skipped so valid channels still receive revocations; malformed route and write-target IDs fail before writes.
- An uppercase still-visible role target must not enter the denied-key set.
- A creator's role must remain unchanged even if the Go preflight is bypassed or races.
- An accepted alternate spelling must not leak back into a response, audit row, cache key, or WebSocket payload.

## Tasks

### Task 1: Reproduction and test isolation

**Files:** Existing scan exploit tests through temporary Go overlays; new focused tests in the affected package suites.

- [x] Replay VULN-077, 064, 099, 195, 196, and 205 against `origin/main` at `7e66479b`; all six currently demonstrate attacker success.
- [ ] Provision an isolated PostgreSQL database or mark the real-DB cases unavailable; never use the shared `concord` database.
- [ ] Write the smallest failing behavior tests for resolver spelling equivalence, owner hierarchy, key candidate visibility, group creator demotion, and affected handler canonical output/refusal. Use the existing package test fixtures where possible.
- [ ] Run each targeted test against the unfixed branch. Require an assertion failure on the expected security property, plus a positive control showing the authorized path works. Commit the red tests alone: `test: reproduce Security #166 UUID authorization bypasses`.

### Task 2: Shared canonicalization and RBAC foundation

**Files:** `services/control-plane/internal/requestid/uuid.go`, `uuid_test.go`, `internal/rbac/resolver.go`, `resolver_test.go`.

**Interface:** `requestid.Canonical(raw string) (string, error)` and `requestid.Param(c *gin.Context, name string) (string, error)`. Batch resolver results use canonical UUID keys.

- [ ] Add the pure helper and tests for accepted spellings and malformed text.
- [ ] Canonicalize server/user/channel IDs before resolver cache/SQL/map use; deduplicate batch channel aliases by UUID value, and index its result by canonical key.
- [ ] Compare parsed UUID values in hierarchy checks, including the database owner ID; fail closed on parse errors.
- [ ] Run focused RBAC tests and the VULN-077 and VULN-205 replays. The exploit tests must fail on attacker-goal assertions, not test setup.

### Task 3: Key revocation identity

**Files:** `internal/rbac/authority_tx.go` and focused authority tests.

- [ ] Canonicalize candidate and visible user IDs for set membership and denied-user output inside the existing transaction.
- [ ] Run authority tests and VULN-064 replay; confirm the still-visible member's key is retained.

### Task 4: HTTP boundaries and guarded writes

**Files:** `internal/messages/{handlers.go,purge.go}`, `internal/channels/handlers.go`, `internal/rbac/handlers.go`, `internal/dm/group_handlers.go`, `internal/voice/handlers.go`, and matching focused tests.

- [ ] Normalize affected path/body IDs immediately after validation and carry the canonical value through authorization, writes, response, audit, and event emission.
- [ ] In the group-DM role `UPDATE`, refuse creator demotion in SQL and treat zero affected rows through the existing unavailable path.
- [ ] Run focused route tests and VULN-099, 195, and 196 replays. Check malformed IDs return 400 before writes and emitted IDs are canonical.

### Task 5: Documentation and guard

**Files:** `docs/api/openapi.yaml`, `docs/architecture.md`, `CHANGELOG.md`, and one focused source guard test if the existing test layout supports it.

- [ ] Document case-insensitive UUID input, canonical output, and malformed-ID 400s for affected routes; record the RBAC canonicalization rule.
- [ ] Add a small guard that catches a new comparison between a request-derived raw ID and a database UUID text in `internal/rbac`, or make that class impossible through a typed API if a smaller safe change emerges.
- [ ] Run changelog/count and quality-gate checks required by the repository. Commit the verified fix.

### Task 6: Lifecycle gates

**Files:** The reviewed control-plane and documentation changes.

- [ ] Rerun all six exploit overlays, focused route/permission tests, and quality checks on the final branch.
- [ ] Open a draft PR, monitor CI, run required red-team/security/RBAC/E2EE and code review, and keep merge behind Phase 9 approval.

## Self-review

The tasks cover each of the six findings, the accepted-input and emitted-output contracts, malformed input, and docs. No migration or new dependency is planned.
