# Weekly hygiene: npm dependency risks — plan

> **Update after #330 merged (2026-11-18):** #330 landed first with the same undici, qs and minimatch fixes. Its root minimatch override is scoped (`eslint@^10` and `@typescript-eslint/typescript-estree`) rather than global, and this PR keeps that design. After merging main, this PR adds the `@microsoft/eslint-formatter-sarif` `$eslint` override, which removes eslint 8.57.1, every minimatch 3.1.5 copy and `glob@7` (68 root lockfile entries). It also adds the corrected admin undici rationale and the runbook and CHANGELOG updates.

Spec: `docs/superpowers/specs/2026-11-18-weekly-hygiene-npm-deps-design.md`.

1. Inventory each package with `npm ls` in the root, `client/admin`, `services/media-plane` and `.github/tools/license-checker` lockfiles. Read the advisory ranges from GHSA, OSV and NVD.
2. Check each candidate release against the 7-day cooldown. eslint 10.15.0 and typescript-eslint 8.73.0 are too new, so minimatch uses overrides instead of parent bumps.
3. Root: add `"@microsoft/eslint-formatter-sarif": { "eslint": "$eslint" }` and `"minimatch": "^11.0.3"`. Admin: raise its minimatch override from `^10.2.6` to `^11.0.3` and update its `//overrides_security` rationale.
4. uri-js: no change. An alias override fails the lockfile registry guard; record the finding as no fix and no exposure.
5. Run `npm install --before=2026-11-11` in each tree. Then run `npm update undici` in root and admin, and `npm update qs` in media, with the same cutoff.
6. Run `override-range-lint.mjs --update-baseline` per directory. Re-run the lint with `--verify-installed-versions --fail-on-stale`.
7. Run the registry guard and the npm audit gate. Per workspace, run `npm run lint`, `typecheck`, `test` and `build`.
8. Add a 2026-11-18 section to `docs/ci/minimatch-advisory-repair.md`, and one CHANGELOG entry for the runtime qs fix and the tooling changes.
9. Open the PR. Its body records picomatch and uri-js as having no fix and names the SonarCloud action the maintainer must take.
