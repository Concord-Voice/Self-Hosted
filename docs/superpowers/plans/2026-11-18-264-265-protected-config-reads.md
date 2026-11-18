# Protected config reads — Plan (Issues #264, #265)

Spec: `docs/superpowers/specs/2026-11-18-264-265-protected-config-reads-design.md`.

1. `check-base-images.py`: pass the manifest path to `tracked_files`, check its record (tracked,
   mode `100644`/`100755`) before `load_manifest`, and open the manifest with `O_NOFOLLOW`.
2. `check-base-images.test.sh`: add four exit-2 cases (symlink tracked, untracked, index-only
   symlink, symlink with a FILE list). Confirm they fail against the old checker.
3. `required-pr-build.yml` and `pr-ci.yml` `migration-lint`: add the merge-commit assertion step,
   and read config by fetching `pull_request.base.sha`.
4. `test_sonar_ioc_conditions.py`: add `MigrationLintRevisionTests`. It runs the real step
   scripts in scratch repositories (merge, single-parent, wrong head, empty SHA; base read vs
   `HEAD^1`) and pins the job shape. Confirm it fails against the old workflows.
5. Run `check-base-images.test.sh`, `sonar-required-gate.test.sh`, the wiring lint and pre-commit.
