# Minimatch advisory repair in CI tooling

GHSA-g3hv-75wg-m249 affects minimatch versions from 7.0.0 up to, but not including, 11.0.1, and is first fixed in 11.0.1. The 3.x line is outside the affected range. The Self-Hosted OSV scan first found vulnerable versions in two development-only lockfiles: `services/media-plane/package-lock.json` and `.github/tools/license-checker/package-lock.json`. The media lockfile pins minimatch 11.0.8 for ESLint and TypeScript-ESLint. Its override crosses three consumers' declared 10.x ranges; the recorded tuples live in the media package's `.override-range-baseline.json` and must be removed when those consumers update their ranges.

The old CI license checker depends on `glob@7`, which reads minimatch's `_glob` matcher field. Forcing minimatch 11 under that old glob crashes the checker during a real production inventory. The locked tool therefore uses `license-checker-rseidelsohn@5.0.1` with tool-local minimatch 11.0.3 and glob 14.0.3 overrides. Glob 14.0.3 also clears GHSA-3g3m-2fjr-6pw4. These overrides cross seven declared dependency ranges, recorded in the tool's `.override-range-baseline.json`; the linter must continue to expose any new consumer or range change.

The root workspace now resolves minimatch 11.0.8 within the ranges declared by ESLint 10.15.0, `@eslint/config-array` 0.23.6, and `@typescript-eslint/typescript-estree` 8.73.0. The two temporary scoped minimatch overrides and their three baseline rows have been removed.

The root workspace keeps the scoped `@microsoft/eslint-formatter-sarif` override to `$eslint`. The formatter declares `eslint@^8.9.0` and loads the host ESLint through `require.main.require('eslint')`. The override removes the unused nested ESLint 8 tree, which previously brought in four minimatch 3.1.5 copies and `glob@7`. The root baseline records the formatter’s exact resolution at ESLint 10.15.0.

The admin and media workspaces each retain their minimatch override and three baseline rows.
Their lockfiles resolve ESLint 10.15.0 and TypeScript-ESLint 8.73.0. Both lockfiles resolve
`@typescript/native` to 7.0.2. Each lockfile contains four nested
`@typescript-eslint/typescript-estree` 8.73.0 entries. All eight entries declare the optional peer
`npm:typescript@>=7.1.0-dev.20261009.1 || >=7.1.0-beta <7.1.0-dev`. The locked compiler version
falls outside that range. Verify peer compatibility when a supported compiler is available.
Before retiring either minimatch override, verify that workspace's dependency tree with the
override-range check, a frozen npm install, and workspace lint.

In an isolated install, the replacement checker returned byte-identical media production CSV to the old checker and matched its positive and negative allowlist outcomes. The CI workflow keeps the same license allowlist and still runs the NOTICE drift guard over each selected workspace. `scripts/update-notice-md.py` uses the same replacement binary for its installed-dependency inventory. Changes to the allowlist, private-package exclusion, CSV parsing, or NOTICE comparison require separate review; this dependency repair does not authorize weakening them.

The isolated media lint, typecheck and build checks passed, as did npm audit for both repaired trees. The complete native media test suite, admin inventory, NOTICE checks and OSV scan remain CI gates for the branch. Do not remove the overrides or their baseline rows until upstream parent ranges admit patched versions and those gates pass without the overrides.

This change also touches the pre-commit configuration to keep dependency pin checks attached to the shared package manifests. Its eight remote hook revisions are public Git commit IDs, all unchanged from the previous commit. Detect-secrets reported seven of those 40-character IDs as hex entropy when the file was staged. `.secrets.baseline` records exactly those seven hash-and-line findings as false positives, without excluding the file or detector. The eighth revision already passed the scanner. The revision guard still rejects tags, malformed hashes, missing frozen-version comments and unrecognized suffixes; its regression test reads the configured pattern.
