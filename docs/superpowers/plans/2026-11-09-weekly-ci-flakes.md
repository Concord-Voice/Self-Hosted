# Weekly CI Flake Fixture Repairs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace four confirmed CI timing hazards with causal, test-only synchronization while preserving every account-authority and operations-metrics assertion.

**Architecture:** All changes stay in existing Go test fixtures. Collector work waits for the actual idle state, the HTTP fake exposes its request contexts, and authority-store cases cancel only from inside the replication probe so the tests cannot pass before the release gate. Production code and constants remain unchanged.

**Tech Stack:** Go `testing`, `stretchr/testify`, `context`, PostgreSQL authority integration harness.

**Spec:** `docs/superpowers/specs/2026-11-09-weekly-ci-flakes-design.md`

## Global Constraints

- Modify only the four test files named below. Do not change production source, deadlines, release behavior, dependencies, workflows, or configuration.
- Work from Self-Hosted; `services/` flows to Hosted through the upstream import.
- Preserve all oracles listed in the spec. A barrier or probe call is an added precondition, not a replacement assertion.
- For authority-store tests, set `CV_FLAKE_AUTHORITY_URL` to the isolated superuser URL naming exactly `concord_authority_test`; pass it as `AUTHORITY_TEST_DATABASE_URL`.
- Run authority integration commands serially with `-p 1`. Do not use `DATABASE_URL` or a shared developer database.
- Use shared Go caches; do not set `GOCACHE`, `GOMODCACHE`, or `GOLANGCI_LINT_CACHE`.
- Run each focused race command once with `-count=1`; historical fail-then-pass evidence supplies repetition evidence. Do not substitute stress loops for causal proof.
- If restore still fails immediate lock reacquisition, stop the fixture task and return it for production diagnosis.
- No issue is required for this ad-hoc run. Root alone commits and publishes after review.

## Review Focus

- A buffered `prune` event arriving before `busy=false`: Task 1 pins the idle precondition before the next tick.
- A peer and route receiving different absolute deadlines: Task 2 compares both context deadlines directly.
- A fake route returning before cancellation and making the 503 assertion vacuous: Task 2 blocks the route on `ctx.Done()` and requires one note.
- Restore cancellation occurring before the local commit/release gate: Task 3 requires replication-probe entry and committed metadata before accepting the result.
- Device submission expiring before the release gate: Task 4 requires probe entry and the complete replication envelope.

---

### Task 1: Collector idle synchronization

**Files:**
- Modify: `services/control-plane/internal/opsmetrics/collector_test.go:392-414`
- Test: `services/control-plane/internal/opsmetrics/collector_test.go`

**Interfaces:**
- Consumes: `Collector.busy atomic.Bool`, the existing `waitCollectorEvents`, and the established `require.Eventually(!collector.busy.Load())` pattern at lines 246-248.
- Produces: a deterministic second-tick precondition; no new production interface.

- [ ] **Step 1: Demonstrate the old event/idle mismatch**

Temporarily hold the test store after it emits `prune` but before `Maintain` returns. Confirm the old test can observe all three events while `collector.busy.Load()` is still true and its immediate second tick is suppressed. Do not retain the forcing barrier.

- [ ] **Step 2: Add the causal idle gate**

Capture the `collector` returned by `newCollectorUnderTest`, then after the first `waitCollectorEvents` require `!collector.busy.Load()` with the same one-second bound and one-millisecond poll interval already used by the sibling test. Leave the second-tick and batch-content assertions unchanged.

- [ ] **Step 3: Run the focused race test**

Run:

```bash
cd services/control-plane
go test -race -count=1 ./internal/opsmetrics -run '^TestCollectorDropsFailedWriteIntervalAndContinuesRetention$'
```

Expected: PASS; two batches exist and the second omits the previously consumed host/media values.

### Task 2: Shared authority HTTP deadline

**Files:**
- Modify: `services/control-plane/internal/authority/server/http_test.go:32-105,479-495`
- Test: `services/control-plane/internal/authority/server/http_test.go`

**Interfaces:**
- Consumes: the fake authority backend and request context created once by `Server.Handler`.
- Produces: optional fake hooks receiving `context.Context`, plus captured peer/route deadlines used only by this test.

- [ ] **Step 1: Make the fake context-aware without changing default behavior**

Change `fake.hit` to accept the method context and add optional test hooks for `hit` and `Revoked`. Every fake authority method must pass its received context to `hit`; when hooks are nil, retain the current sleep/result behavior exactly.

- [ ] **Step 2: Replace elapsed margins with context events**

In `TestThePeerCheckSpendsTheRequestDeadline`, have the peer hook capture its context deadline and return normally. Have the route hook capture its deadline and block on `ctx.Done()`. For both GET and POST cases, require that both deadlines exist and are equal, the response is 503, and `f.noted == 1`. Remove the 30 ms sleeps; the configured deadline remains a bounded watchdog, not the proof.

- [ ] **Step 3: Run a deadline-identity negative control**

Temporarily make the route hook report a distinct deadline and confirm the equality assertion fails. Restore the real shared-context path before continuing.

- [ ] **Step 4: Run the focused race test**

Run:

```bash
cd services/control-plane
go test -race -count=1 ./internal/authority/server -run '^TestThePeerCheckSpendsTheRequestDeadline$'
```

Expected: PASS for both GET and POST; each observes one absolute deadline and one late-result note.

### Task 3: Restore release-gate cancellation and lock proof

**Files:**
- Modify: `services/control-plane/internal/authority/store/owner_restore_integration_test.go:889-926`
- Test: `services/control-plane/internal/authority/store/owner_restore_integration_test.go`

**Interfaces:**
- Consumes: existing package helper `cancelAtReplicationProbe` from `scenarios_integration_test.go`, `OwnerConfig.Probe`, and `holdsServeLock`.
- Produces: cancellation only after `RestoreBegin` enters `finish`; no production interface.

- [ ] **Step 1: Replace the one-second timer with probe-entry cancellation**

Create `ctx, cancel := context.WithCancel(context.Background())`, install `cancelAtReplicationProbe{cancel: cancel}` in `OwnerConfig`, and call `RestoreBegin` with that context. Require a positive probe-call count before accepting `ErrNotReplicated`.

- [ ] **Step 2: Preserve the full restore oracle**

Keep the committed metadata checks, immediate `holdsServeLock` refusal check, confirmed-probe retry, increased probe-call check, and unchanged restore-point comparison. Do not add retry/polling around lock reacquisition.

- [ ] **Step 3: Run the focused isolated race test**

Run, with `CV_FLAKE_AUTHORITY_URL` supplied by the orchestrator:

```bash
cd services/control-plane
AUTHORITY_TEST_DATABASE_URL="$CV_FLAKE_AUTHORITY_URL" go test -race -p 1 -count=1 ./internal/authority/store -run '^TestRestoreBeginEndsOnTheReplicationCheck$'
```

Expected: PASS with probe entry, committed epoch 1 metadata, immediate free lock, and convergent retry. If the lock assertion fails, stop and report a possible production defect in `stopped`/`ServeLock.Release`.

### Task 4: Device transaction release-gate synchronization

**Files:**
- Modify: `services/control-plane/internal/authority/store/device_integration_test.go:309-317`
- Test: `services/control-plane/internal/authority/store/device_integration_test.go`

**Interfaces:**
- Consumes: existing package helper `cancelAtReplicationProbe`, `Store.probe`, and `Store.SubmitDevice(context.Context, []byte) Outcome`.
- Produces: a release-gate test independent of transaction duration; no production interface.

- [ ] **Step 1: Cancel at replication-probe entry**

Remove the 200 ms `txDL` override. Create a cancellable parent context, assign `e.s.probe = &cancelAtReplicationProbe{cancel: cancel}`, and call `SubmitDevice` directly with that context.

- [ ] **Step 2: Assert the named branch and complete outcome**

Require at least one probe call. Require `HTTP == 503`, `Kind == KindError`, `Code == string(ac.ErrorReplication)`, and `Body` byte-equal to `ac.ErrorEnvelope{Code: string(ac.ErrorReplication)}.Encode()` so a pre-gate store error or signed response cannot pass.

- [ ] **Step 3: Run a pre-probe cancellation control**

Temporarily cancel before `SubmitDevice` and confirm the positive probe-call assertion fails. Restore probe-entry cancellation before continuing.

- [ ] **Step 4: Run the focused isolated race test**

Run:

```bash
cd services/control-plane
AUTHORITY_TEST_DATABASE_URL="$CV_FLAKE_AUTHORITY_URL" go test -race -p 1 -count=1 ./internal/authority/store -run '^TestDeviceTransactionsPassTheReleaseGate$'
```

Expected: PASS with at least one probe call and the exact replication error envelope.

### Task 5: Integrated verification and review handoff

**Files:**
- Verify: all four modified test files
- Verify: `docs/superpowers/specs/2026-11-09-weekly-ci-flakes-design.md`
- Verify: `docs/superpowers/plans/2026-11-09-weekly-ci-flakes.md`

**Interfaces:**
- Consumes: Tasks 1-4.
- Produces: one reviewable, test-only patch for root to commit and publish.

- [ ] **Step 1: Run the two non-database focused cases together**

```bash
cd services/control-plane
go test -race -count=1 ./internal/opsmetrics ./internal/authority/server -run 'TestCollectorDropsFailedWriteIntervalAndContinuesRetention|TestThePeerCheckSpendsTheRequestDeadline'
```

Expected: PASS in both packages.

- [ ] **Step 2: Run the two authority-store cases together, serialized**

```bash
cd services/control-plane
AUTHORITY_TEST_DATABASE_URL="$CV_FLAKE_AUTHORITY_URL" go test -race -p 1 -count=1 ./internal/authority/store -run '^(TestRestoreBeginEndsOnTheReplicationCheck|TestDeviceTransactionsPassTheReleaseGate)$'
```

Expected: PASS; no skip, database-name error, store result, or lock-reacquire failure.

- [ ] **Step 3: Inspect scope and formatting**

Run:

```bash
git diff --check
git status --short
git diff -- services/control-plane/internal/opsmetrics/collector_test.go services/control-plane/internal/authority/server/http_test.go services/control-plane/internal/authority/store/owner_restore_integration_test.go services/control-plane/internal/authority/store/device_integration_test.go docs/superpowers/specs/2026-11-09-weekly-ci-flakes-design.md docs/superpowers/plans/2026-11-09-weekly-ci-flakes.md
```

Expected: only the four test files and two lifecycle documents; no production diff.

- [ ] **Step 4: Record count-tooling and quality-gate evidence**

`scripts/update-claude-md-counts.sh` and `docs/ci/quality-gate-definition.md` are absent in this repository split. Record that gap; do not claim the count probe passed. Require 0 new blocker/critical issues, 0 new high-confidence security hotspots, and at least 80% new-code coverage where measured.

- [ ] **Step 5: Hand off for Phase-8 review**

Request `security-reviewer`, `code-reviewer`, `scope-reviewer`, the Go test analyzer, and silent-failure review. Add docs review because these Markdown artifacts change. Root owns commits, publication, downstream Hosted import, and final CI evidence.
