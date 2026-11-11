# Docker Dependabot Tiers Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every tracked Dockerfile except MinIO is watched by a Dependabot block of its own tier, and CI shows a red check when one is not.

**Architecture:** One new `docker` block and tier comments in `.github/dependabot.yml`. The existing coverage audit becomes a step in the `workflow-lint` job, and that job's relevance filter gains the paths that can change coverage.

**Tech Stack:** Dependabot v2 config, GitHub Actions YAML, bash audit script (needs Node and PyYAML), the dorny-based relevance test harness.

**Spec:** `docs/superpowers/specs/2026-11-11-251-docker-dependabot-tiers-design.md`

## Global Constraints

- Exactly four `docker` blocks with disjoint directories: runtime (`/services/control-plane`, `/services/media-plane`), `/infrastructure/docker/postgres`, `/infrastructure/docker/buildtools`, `/infrastructure/deploy/tests`. MinIO stays excluded.
- The new block copies its siblings: weekly, `tuesday`, `05:00`, `Etc/UTC`, commit prefix `chore`, cooldown `default-days: 7`, labels `area: deps` and `area: ci-cd`, `open-pull-requests-limit: 5`, group `base-images` with pattern `*`.
- The runtime block gains the label `risk: security`. No other block changes its labels.
- The audit step uses the PyYAML that `workflow-lint` already installs. No new install step.
- This is a public repository. No comment or doc added here may cite an issue or PR number from another repository.
- Commit subjects use a bare Conventional Commits type with no parenthesized scope.
- Never stage `.claude/state/`.
- Local runs of the audit need PyYAML: use `pre-commit run dependabot-coverage-audit --all-files`, which provisions it.

## Review Focus

- **A pull request that edits only `.github/dependabot.yml`.** Expected: `workflow-lint` starts. Pinned by the relevance policy case in Task 2.
- **A new Dockerfile in a directory no filter names today.** Expected: `workflow-lint` starts. Pinned by the relevance policy case in Task 2.
- **The buildtools block is removed again.** Expected: the audit exits 1. Pinned by the mutation step in Task 1.
- **Dependabot rejects the config** (overlapping directories, unknown key). Expected: no configuration error on the Dependabot page. No local test can prove this; it is checked after merge.
- **The audit cannot parse the config in CI** (PyYAML missing). Expected: exit 2 and a red step, never a silent pass. This is the script's existing behavior; confirm the step sits after the install step.

---

### Task 1: Fourth docker block and tier comments

**Files:**
- Modify: `.github/dependabot.yml` (the "Container base images" section)
- Modify: `docs/runbooks/refresh-buildtools-base.md` (pre-flight step 2)
- Test: `scripts/security/audit-dependabot-coverage.sh` (existing, unchanged)

**Interfaces:**
- Consumes: nothing.
- Produces: nothing Task 2 depends on.

- [ ] **Step 1: Record the failing audit**

Run: `pre-commit run dependabot-coverage-audit --all-files`
Expected: `Failed`, with `UNCOVERED docker: infrastructure/docker/buildtools` and `AUDIT DRIFT`. Save the output for the PR body.

- [ ] **Step 2: Edit `.github/dependabot.yml`**

- Add a `docker` block for `directory: "/infrastructure/docker/buildtools"` after the postgres block, with the sibling settings from Global Constraints.
- Add `risk: security` to the runtime block's labels.
- Put one tier comment above each of the four blocks, saying what the tier is and why it gets its own merge decision (see the table in the spec).
- Correct the postgres comment that lists buildtools as a member of the runtime block.

- [ ] **Step 3: Run the audit to verify it passes**

Run: `pre-commit run dependabot-coverage-audit --all-files`
Expected: `Passed`.

- [ ] **Step 4: Prove the audit catches the regression**

Temporarily delete the new block, run the audit, confirm `UNCOVERED docker: infrastructure/docker/buildtools`, restore the block, re-run to green.

- [ ] **Step 5: Update the runbook**

In `docs/runbooks/refresh-buildtools-base.md`, pre-flight step 2: Dependabot now proposes the parent bump weekly. The step becomes "confirm no Dependabot pull request for the buildtools parent is open and unmerged, then verify the pin is current". Keep the re-resolve procedure as the fallback and keep the warning about dispatching on a stale digest.

- [ ] **Step 6: Lint and commit**

Run: `pre-commit run --files .github/dependabot.yml docs/runbooks/refresh-buildtools-base.md`
Expected: all hooks pass.

```bash
git add .github/dependabot.yml docs/runbooks/refresh-buildtools-base.md
git commit -m "chore: give the buildtools image its own Dependabot tier"
```

### Task 2: Run the audit in CI

**Files:**
- Modify: `.github/workflows/pr-ci.yml` (job `workflow-lint`)
- Modify: `.github/ci/scan-relevance.yml` (filter `workflow_lint`)
- Test: `.github/scripts/tests/ci-scan-relevance.test.sh`

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces: nothing.

- [ ] **Step 1: Write the failing policy cases**

In `.github/scripts/tests/ci-scan-relevance.test.sh`, next to the existing `workflow-lint-source-*` cases, add three. A label that starts with `workflow-lint-source-` already asserts `advisory.workflow_lint === true`.

```bash
run_policy_case workflow-lint-source-dependabot <expected> .github/dependabot.yml
run_policy_case workflow-lint-source-coverage-audit <expected> scripts/security/audit-dependabot-coverage.sh
run_policy_case workflow-lint-source-new-dockerfile <expected> tools/example/Dockerfile.example
```

`<expected>` is the analysis-key set the harness reports for that path. Start with `advisory-only`; if the harness reports other analysis keys for a path, use the set it reports and check each key is explained by an existing filter.

- [ ] **Step 2: Run the test to verify it fails**

Run: `bash .github/scripts/tests/ci-scan-relevance.test.sh`
Expected: non-zero exit on the first new case, with the `reaches workflow-lint runner` assertion.

- [ ] **Step 3: Extend the filter and add the step**

- `.github/ci/scan-relevance.yml`, filter `workflow_lint`: add `.github/dependabot.yml`, `scripts/security/audit-dependabot-coverage.sh`, `scripts/security/check-pin-exceptions.mjs`, `**/Dockerfile*`.
- `.github/workflows/pr-ci.yml`, job `workflow-lint`: add a step after "Install python3 + PyYAML (lint dependencies)", named `Dependabot watches every update root`, running `bash scripts/security/audit-dependabot-coverage.sh`. Its comment says what a failure means and that the job is advisory.

- [ ] **Step 4: Run the test to verify it passes**

Run: `bash .github/scripts/tests/ci-scan-relevance.test.sh`
Expected: exit 0.

- [ ] **Step 5: Lint and commit**

Run: `pre-commit run --files .github/workflows/pr-ci.yml .github/ci/scan-relevance.yml .github/scripts/tests/ci-scan-relevance.test.sh`
Expected: all hooks pass, including `actionlint`.

```bash
git add .github/workflows/pr-ci.yml .github/ci/scan-relevance.yml .github/scripts/tests/ci-scan-relevance.test.sh
git commit -m "ci: run the Dependabot coverage audit in workflow-lint"
```

## After the tasks

- In the PR, record the red audit output from `main` and the green output from the branch.
- After merge: open the repository's Dependabot page, confirm no configuration error, use "Check for updates", and record what opens on the issue.
