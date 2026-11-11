# Base-Image Identity Check Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A static check fails when any `FROM` or `COPY --from=` reference in a tracked Dockerfile differs from the identity declared in `.github/base-images.json`.

**Architecture:** One standard-library Python script parses every tracked Dockerfile and compares each stage with a manifest entry. A hermetic shell suite drives it through fixtures. The check runs in pre-commit and as an advisory CI step.

**Tech Stack:** Python 3 standard library, bash test harness, pre-commit, GitHub Actions YAML.

**Spec:** `docs/superpowers/specs/2026-11-11-252-base-image-identity-check-design.md`

## Global Constraints

- Python standard library only. No Docker daemon, no network.
- CLI: `check-base-images.py [--root DIR] [--manifest PATH] [FILE...]`. Exit `0` pass, `1` violations, `2` could not complete.
- One output line per violation: `<file>:<line>: <rule>: <message>`.
- Rule names, exactly: `unlisted-dockerfile`, `stale-entry`, `stage-mismatch`, `variable-reference`, `missing-tag`, `missing-digest`, `identity-mismatch`, `tag-mismatch`, `external-copy-source`, `syntax-directive`, `escape-directive`.
- A manifest `tag` pattern must start with `^` and end with `$`; otherwise exit `2`.
- Fixture files are not named `Dockerfile*`.
- This is a public repository. No comment or doc may cite an issue or PR number from another repository.
- Commit subjects use a bare Conventional Commits type with no parenthesized scope.
- Never stage `.claude/state/`.

## Review Focus

- **A real `FROM` hidden from the parser** (continuation, heredoc, `# escape=`). Expected: the parser never sees fewer instructions than Docker. Pinned by the continuation and escape fixtures in Task 1; heredoc behavior is fail-closed by design and gets a fixture.
- **A registry given with a port, or `localhost`.** Expected: treated as a registry, not as a Docker Hub repository. Pinned by a normalization fixture in Task 1.
- **`FROM` flags** (`--platform=$BUILDPLATFORM`). Expected: the flag is not read as the image. Pinned by a pass fixture in Task 1.
- **A manifest that is valid JSON but the wrong shape** (missing `stages`, a stage with both `scratch` and `registry`). Expected: exit `2` with a message that names the entry. Pinned by a fixture in Task 1.
- **`COPY --from=0`** (numeric stage index). Expected: allowed when the index is an earlier stage. Pinned by a pass fixture in Task 1.

---

### Task 1: Checker, fixtures and suite

**Files:**
- Create: `.github/scripts/check-base-images.py`
- Create: `.github/scripts/tests/check-base-images.test.sh`
- Create: `.github/scripts/tests/fixtures/base-images/<case>/` (one directory per case: `manifest.json` plus one or more `*.dockerfile` files)
- Create: `.github/base-images.json`

**Interfaces:**
- Consumes: nothing.
- Produces: the CLI and exit codes in Global Constraints; the suite at `.github/scripts/tests/check-base-images.test.sh`, runnable with `bash` from the repository root, exit 0 on success. Tasks 2 and 3 use both.

- [ ] **Step 1: Write the suite and fixtures first**

The suite runs the checker once per fixture as
`python3 .github/scripts/check-base-images.py --root <case-dir> --manifest <case-dir>/manifest.json <files...>`
and asserts the exit code and, for failing cases, that the output contains the expected rule name. It ends with a `N passed, M failed` line and exits non-zero when `M > 0`. Follow the style of `.github/scripts/tests/trivy-assert-os.test.sh`.

Must-fail cases (expected exit 1 and rule): digest-only `FROM` with no tag (`missing-tag`); registry not in the manifest (`identity-mismatch`); missing digest (`missing-digest`); Alpine-to-Debian swap under one image name (`tag-mismatch`); `ARG`-templated reference (`variable-reference`); `COPY --from=` an external image (`external-copy-source`); `RUN --mount=type=bind,from=` an external image (`external-copy-source`); a file absent from the manifest (`unlisted-dockerfile`); a manifest entry with no file (`stale-entry`); a changed `# syntax=` directive (`syntax-directive`); an undeclared `# syntax=` directive (`syntax-directive`); an `# escape=` directive (`escape-directive`); an extra stage (`stage-mismatch`); a wrong alias (`stage-mismatch`); a `FROM`-looking line inside a heredoc body (`stage-mismatch`).

Must-fail with exit 2: an unanchored tag pattern; a manifest with the wrong shape; a manifest that is not JSON.

Must-pass cases (exit 0): a digest-only bump; a same-flavor patch bump; `FROM scratch`; `--platform=$BUILDPLATFORM`; a declared build context used by `COPY --from=`; `COPY --from=0`; a `FROM` split over a continuation line; a registry with a port; lower-case instructions.

One more case runs the checker with no `FILE` arguments against the real repository root and expects exit 0.

- [ ] **Step 2: Run the suite to verify it fails**

Run: `bash .github/scripts/tests/check-base-images.test.sh`
Expected: non-zero exit; every case fails because the script does not exist.

- [ ] **Step 3: Write `.github/scripts/check-base-images.py`**

Implement the CLI, the normalization rule and the rule table from the spec. Parsing choices are in the spec section "Checker" and are binding.

- [ ] **Step 4: Write `.github/base-images.json` for the nine tracked Dockerfiles**

One entry per file from `git ls-files -- '*Dockerfile*'`. Tag patterns: a floating tag is matched exactly; a patch-pinned tag admits any patch of the same minor and flavor. `services/control-plane/Dockerfile` declares `build_contexts: ["admin_ui"]`. `infrastructure/docker/buildtools/Dockerfile` declares `syntax: "docker/dockerfile:1.7"`.

- [ ] **Step 5: Run the suite to verify it passes**

Run: `bash .github/scripts/tests/check-base-images.test.sh`
Expected: exit 0, `0 failed`.

- [ ] **Step 6: Mutation check**

For each rule, disable it in the script, run the suite, confirm that rule's case fails, and restore. Record the result per rule in the task report.

- [ ] **Step 7: Commit**

```bash
git add .github/scripts/check-base-images.py .github/scripts/tests/check-base-images.test.sh .github/scripts/tests/fixtures/base-images .github/base-images.json
git commit -m "feat: add the base-image identity check"
```

### Task 2: Wiring

**Files:**
- Modify: `.pre-commit-config.yaml`
- Modify: `.github/workflows/pr-ci.yml` (job `workflow-lint`)
- Modify: `.github/ci/scan-relevance.yml` (filter `workflow_lint`)
- Modify: `.github/scripts/tests/ci-scan-relevance.test.sh`
- Modify: the repository's CODEOWNERS file

**Interfaces:**
- Consumes: the CLI and suite path from Task 1.
- Produces: nothing.

- [ ] **Step 1: Write the failing relevance case**

Add `run_policy_case workflow-lint-source-base-images advisory-only .github/base-images.json` next to the other `workflow-lint-source-*` cases.

- [ ] **Step 2: Run it to verify it fails**

Run: `bash .github/scripts/tests/ci-scan-relevance.test.sh` (needs PyYAML on `python3`)
Expected: `workflow-lint-source-base-images reaches workflow-lint runner` assertion.

- [ ] **Step 3: Wire it**

- `scan-relevance.yml`, filter `workflow_lint`: append `.github/base-images.json` at the end of the list.
- `pr-ci.yml`, job `workflow-lint`: a step `Base images match their declared identity` running `python3 .github/scripts/check-base-images.py`, and `bash .github/scripts/tests/check-base-images.test.sh` added to the "Self-test" step.
- `.pre-commit-config.yaml`: hook `check-base-images` (`language: system`, `pass_filenames: false`, files: any path containing `Dockerfile`, the manifest, the script) and hook `test-check-base-images` (files: the script, the suite, the fixtures).
- CODEOWNERS: add `/.github/base-images.json` with the same owner as the `.github` entries.

- [ ] **Step 4: Verify**

Run: `bash .github/scripts/tests/ci-scan-relevance.test.sh`, `bash .github/scripts/lint-test-suite-wiring.sh`, `actionlint -config-file=.actionlint.yaml`
Expected: all exit 0.

- [ ] **Step 5: Commit**

```bash
git add .pre-commit-config.yaml .github/workflows/pr-ci.yml .github/ci/scan-relevance.yml .github/scripts/tests/ci-scan-relevance.test.sh <CODEOWNERS path>
git commit -m "ci: run the base-image identity check in pre-commit and workflow-lint"
```

### Task 3: Docs

**Files:**
- Modify: `docs/development.md`, `docs/runbooks/refresh-buildtools-base.md`, `docs/runbooks/refresh-minio-image.md`, `.github/CONTRIBUTING.md`

**Interfaces:**
- Consumes: the manifest path and the CLI from Task 1.
- Produces: nothing.

- [ ] **Step 1: Write the docs**

Each file gains a short passage: a base-image change edits the Dockerfile and its entry in `.github/base-images.json` in the same pull request; `python3 .github/scripts/check-base-images.py` checks it locally; a digest-only bump needs no manifest edit. `docs/development.md` carries the full explanation; the others link to it.

- [ ] **Step 2: Lint and commit**

Run: `pre-commit run --files <the four files>`
Expected: all hooks pass.

```bash
git add docs/development.md docs/runbooks/refresh-buildtools-base.md docs/runbooks/refresh-minio-image.md .github/CONTRIBUTING.md
git commit -m "docs: say how to change a base image"
```

## After the tasks

**As built.** This plan is the record of what was planned. Where it differs from the code, the spec and the code are right:

- The checker has 13 rule names. `heredoc` and `unsupported-character` were added after this plan; a heredoc case reports `heredoc` as well as any stage rule.
- The pre-commit hooks of Task 2 are not in the pull request. It wires the `workflow-lint` CI job only.
- The spec section "Amended in the final review" lists the later changes to the parsing rules, the checked file set and the fixtures.

- The protected job in `required-pr-build.yml` is not in this plan. It is added here or in a follow-up once the maintainer decides; the spec section "Where it runs" fixes its shape.
- Committing `.pre-commit-config.yaml` triggers hooks that fail on macOS for environment reasons. That commit needs the maintainer's go-ahead to skip those hooks, or a Linux session.
