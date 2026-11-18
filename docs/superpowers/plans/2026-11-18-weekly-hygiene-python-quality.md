# Weekly hygiene: Python code-quality findings — Plan

Spec: `docs/superpowers/specs/2026-11-18-weekly-hygiene-python-quality-design.md`.

## Tasks

1. Baseline: run every Python test that covers a changed file on clean `origin/main`, and record the pass state and test counts.
2. Fix (Workflow, D6): one fixer agent per file, 16 files, file-disjoint, in parallel. Each fixer gets the finding list for its file and the per-rule fix shape from the spec.
3. Verify (Workflow, adversarial): one verifier per coupled file group. It reads the diff, runs that group's tests, and confirms each changed test still exercises the same behaviour.
4. `ste-lint.py`: delete the unused `per100` local in Agentic-Dev-Tooling (companion PR), then apply the same change to the synced copy here.
5. Re-run the full baseline set and compare the counts.
6. Record any rejections in the CodeQL register and dismiss them through the code-quality API, if it supports dismissal.
7. Commit, open the draft PRs, and take them through Phases 5 to 8. Stop at the Phase 9 merge gate.

## Test commands

The tests are scripts, not a pytest suite. CI runs each one directly with `python3`. The full list is in the PR body.
