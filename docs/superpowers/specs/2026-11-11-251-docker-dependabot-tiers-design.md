# Split docker Dependabot config into blast-radius tiers — Design (Issue #251)

## Understanding

**Intended outcome.** Each container image tier has its own Dependabot block with a stated reason for being separate, and CI shows a red check when a tracked Dockerfile is in no block.

**What is wrong today.**

- `infrastructure/docker/buildtools/Dockerfile` is in no `docker` block. Its digest-pinned parent is never bumped by Dependabot.
- `scripts/security/audit-dependabot-coverage.sh` detects exactly this. On `main` it exits 1 with `UNCOVERED docker: infrastructure/docker/buildtools`. No workflow runs it; it is a local pre-commit hook only.
- The blocks carry no statement of why they are separate, and one comment still lists buildtools as a member of the runtime block.

**Was the buildtools drop deliberate?** No evidence that it was. The commit that removed the directory adapted a copied CI configuration to this repository and gives no reason for that line. The Dockerfile is still tracked, the audit still flags it, and a sibling comment still assumes it is covered. This design treats the drop as unintended.

**Success looks like.** The audit exits 0 on the branch. A change that removes the buildtools block turns the `workflow-lint` check red.

## Decision

Option X1 from the issue's Design Dossier: a fourth `docker` block, plus the existing audit as a CI step.

Rejected: a new partition guard that fails when two tiers share a block (X2), and one block per directory (X3). The failure that happened was lost coverage, and the existing audit catches that once it runs in CI.

## Design

### 1. `.github/dependabot.yml`

Four `docker` blocks with disjoint directories. MinIO stays excluded.

| Tier | Directories | Why it is separate |
|---|---|---|
| Runtime services | `/services/control-plane`, `/services/media-plane` | These images run in production. Gains the `risk: security` label. |
| Database | `/infrastructure/docker/postgres` | A base bump can be a data migration. Carries its own `ignore` for majors. |
| Build tooling | `/infrastructure/docker/buildtools` (new) | Its parent becomes the parent of the next published builder image, which every later runtime build inherits. |
| Deploy tests | `/infrastructure/deploy/tests` | Test-only image. Carries its own `ignore` for non-LTS Ubuntu. |

The new block copies the schedule, cooldown, commit prefix, labels, PR limit and `base-images` group of its siblings. Each block gets a one-paragraph tier comment. The stale postgres comment is corrected.

### 2. The audit as a CI step

The step goes in the `workflow-lint` job of `.github/workflows/pr-ci.yml`, not in `unit-smoke.yml` as the issue text says. Two reasons:

- `workflow-lint` already installs the hash-pinned PyYAML that the audit needs. `dev-checks` in `unit-smoke.yml` does not.
- The `dev_checks` relevance filter does not include `.github/dependabot.yml`, so a pull request that only edits that file would never start the job.

The `workflow_lint` filter in `.github/ci/scan-relevance.yml` gains four paths, so the job starts when coverage can change:

- `.github/dependabot.yml`
- `scripts/security/audit-dependabot-coverage.sh`
- `scripts/security/check-pin-exceptions.mjs` (the audit imports it)
- `**/*Dockerfile*` (the audit counts any tracked path that contains `Dockerfile`)

Known ceiling: a new npm, Go, pip or non-root compose root added with no other `workflow_lint` path in the same pull request does not start the job. Add those manifest globs if that case is ever missed.

`workflow-lint` is an advisory job: its failure shows as a red check and is not part of the required build result. That matches the issue's acceptance criterion (the pull request "fails it") and is no weaker than `unit-smoke`, which is advisory too. Which job should be merge-blocking for supply-chain guards is an open question that #252 settles; this step can move with that decision.

### 3. Docs

`docs/runbooks/refresh-buildtools-base.md` pre-flight step 2 tells the operator to refresh the parent pin by hand before every publish. With the new block, Dependabot proposes that bump weekly. The step becomes: confirm no Dependabot pull request for the buildtools parent is open and unmerged, then verify the pin is current as before. The manual re-resolve stays as the fallback.

`.github/SECURITY.md` names Dependabot alerts only and states no cadence. No change.

## Testing

- **The audit itself is the test.** Red on `main` (exit 1, output recorded in the PR), green on the branch.
- **Mutation.** Removing the new block makes the audit exit 1 again.
- **Filter wiring.** `.github/scripts/tests/ci-scan-relevance.test.sh` gains policy cases: a change to `.github/dependabot.yml`, to the audit script, and to a Dockerfile outside the paths listed today each select `workflow_lint`.
- **Existing suites stay green:** `scripts/security/test-audit-dependabot-coverage.sh`, the pin-exception check, `actionlint`.

After merge, the Dependabot page must show no configuration error. Merging any change to `.github/dependabot.yml` starts an immediate Dependabot run across every ecosystem, so a burst of pull requests is expected.

## Out of scope

- Validating what a bump changes. That is the base-image identity check in #252.
- Making the audit merge-blocking.
- A partition guard.

## Acceptance criteria

The issue's acceptance criteria apply, with one stated change: the audit runs in `pr-ci.yml` `workflow-lint`, not in `unit-smoke.yml`. This repository configures no quality gate in `.claude/repo-config.json`, so the gate is the existing pre-commit and PR check set.
