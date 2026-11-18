# Minimatch advisory repair in CI tooling

GHSA-g3hv-75wg-m249 affects every minimatch version below 11.0.1. The Self-Hosted OSV scan found vulnerable versions in two development-only lockfiles: `services/media-plane/package-lock.json` and `.github/tools/license-checker/package-lock.json`. The media package now resolves minimatch 11.0.3 for ESLint and TypeScript-ESLint. Its override crosses three consumers' declared 10.x ranges; the recorded tuples live in the media package's `.override-range-baseline.json` and must be removed when those consumers update their ranges.

The old CI license checker depends on `glob@7`, which reads minimatch's `_glob` matcher field. Forcing minimatch 11 under that old glob crashes the checker during a real production inventory. The locked tool therefore uses `license-checker-rseidelsohn@5.0.1` with tool-local minimatch 11.0.3 and glob 14.0.3 overrides. Glob 14.0.3 also clears GHSA-3g3m-2fjr-6pw4. These overrides cross seven declared dependency ranges, recorded in the tool's `.override-range-baseline.json`; the linter must continue to expose any new consumer or range change.

In an isolated install, the replacement checker returned byte-identical media production CSV to the old checker and matched its positive and negative allowlist outcomes. The CI workflow keeps the same license allowlist and still runs the NOTICE drift guard over each selected workspace. `scripts/update-notice-md.py` uses the same replacement binary for its installed-dependency inventory. Changes to the allowlist, private-package exclusion, CSV parsing, or NOTICE comparison require separate review; this dependency repair does not authorize weakening them.

The isolated media lint, typecheck and build checks passed, as did npm audit for both repaired trees. The complete native media test suite, admin inventory, NOTICE checks and OSV scan remain CI gates for the branch. Do not remove the overrides or their baseline rows until upstream parent ranges admit patched versions and those gates pass without the overrides.

This change also touches the pre-commit configuration to keep dependency pin checks attached to the shared package manifests. Its eight remote hook revisions are public Git commit IDs, all unchanged from the previous commit. Detect-secrets reported seven of those 40-character IDs as hex entropy when the file was staged. `.secrets.baseline` records exactly those seven hash-and-line findings as false positives, without excluding the file or detector. The eighth revision already passed the scanner. The revision guard still rejects tags, malformed hashes, missing frozen-version comments and unrecognized suffixes; its regression test reads the configured pattern.

## 2026-11-18: root and admin lockfiles, parent bumps replace the overrides

Dependabot alerts 8 (`client/admin/package-lock.json`) and 9 (`package-lock.json`) reported the same advisory against minimatch 10.2.6. The advisory range is `>= 7.0.0, < 11.0.1`, and no 10.x backport exists, so the fix is 11.x. Minimatch 3.1.5 is outside the advisory range. It came only from the eslint 8.57.1 tree under `@microsoft/eslint-formatter-sarif`, and that tree is now removed (see below).

Parent bumps now admit patched minimatch, so the minimatch overrides are no longer necessary:

- `eslint@^10.15.0` and `@eslint/config-array@0.23.6` declare `minimatch@^11.0.5`.
- `typescript-eslint@^8.73.0` (through `@typescript-eslint/typescript-estree`) declares `minimatch@^11.0.1`.
- `@eslint-react/eslint-plugin@^5.30.4` (admin only) moves its `@typescript-eslint/*` copy to 8.73.0.

The root, admin and media packages take those bumps. The admin `minimatch: ^10.2.6` override and the media `minimatch: ^11.0.3` override are deleted, and so are the three media baseline rows that recorded them. All three trees resolve minimatch 11.0.9 without an override. `@microsoft/eslint-formatter-sarif@3.1.0` declares `eslint@^8.9.0` but loads ESLint with `require.main.require('eslint')`, so its nested eslint 8 copy was never executed. The root override `"@microsoft/eslint-formatter-sarif": { "eslint": "$eslint" }` removes that copy and its 69 packages. The override is recorded in the root `.override-range-baseline.json`. The SARIF output was checked against the root lint target.

Do not re-add a minimatch override to clear a later alert unless a parent range blocks a patched version. Prefer a parent bump.
