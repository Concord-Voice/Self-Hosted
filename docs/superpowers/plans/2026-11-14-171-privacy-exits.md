# Security #171 Privacy Exit Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans task by task. Checkboxes track progress.

**Goal:** Close six privacy exits across presence, profiles, ownership, and friendship notifications in this repository.

**Architecture:** One Hub visible-online predicate feeds the two current raw-presence callers. Other exits enforce their existing audience or prior-relationship rule at their write/read boundary.

**Spec:** `docs/superpowers/specs/2026-11-14-171-privacy-exits-design.md`

## Constraints

- Use an isolated worktree and isolated `DATABASE_URL` / `REDIS_URL`; use shared default Go caches.
- No migration or new dependency. Preserve transaction order and current client contracts where the privacy rule does not require a change.
- #166 must merge and pass postmerge before #171 merges; rebase shared docs and rerun exact-head gates.
- Test authoring is a separate `@test-writer` dispatch; Phase 4 and Phase 8 both require `@red-team`.

## Tasks

### 1. Baseline and test proof

- [ ] Run the five supplied exploit tests on unfixed `origin/main` with isolated services and record attacker-goal success.
- [ ] Add small RED tests for server count, last-seen, group ring, profile, transfer event, and stranger block; include positive controls.

### 2. Presence exits

**Files:** `internal/websocket/hub.go`, `servers/handlers.go`, `members/handlers.go`, `dm/handlers.go`, matching tests.

- [ ] Extract one visible-online snapshot/MGET rule and expose filtered accessors.
- [ ] Migrate the two production raw-presence callers; audit remaining callers.
- [ ] Filter last-seen hydration and capture hidden status before final disconnect.
- [ ] Use one filtered group-ring recipient slice for invitations, status, and response IDs.

### 3. Audience and notification exits

**Files:** `internal/users/handlers.go`, `ownership/handlers.go`, `friends/handlers.go`, matching tests.

- [ ] Apply search audience and symmetric block exclusion to profile reads with uniform 404.
- [ ] Send transfer initiation only to both parties.
- [ ] Capture prior accepted-friendship status inside the block transaction and condition target notification on it.

### 4. Validation and docs

- [ ] Run focused package tests, race checks where practical, exploit replays, lint/vet, and quality gate against isolated services.
- [ ] Update OpenAPI, architecture, and changelog for changed privacy semantics.
- [ ] Dispatch pre-PR red team; fix concrete findings. Commit and push a draft PR.

### 5. Lifecycle gates

- [ ] Rebase after #166 merges and Phase 10 is green; rerun exact-head CI and quality gates.
- [ ] Mark ready, wait for required online bots, run the full Phase-8 specialist suite and disposition gate.
- [ ] Obtain separate explicit Phase-9 merge approval for each PR; run Phase 10 before the next batch merge.
