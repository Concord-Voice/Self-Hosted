# Weekly hygiene: npm dependency risks — plan

Spec: `docs/superpowers/specs/2026-11-18-weekly-hygiene-npm-deps-design.md`.

1. Inventory each package with `npm ls` in the root, `client/admin`, `services/media-plane` and `.github/tools/license-checker` lockfiles. Read the advisory ranges from GHSA, OSV and NVD.
2. Root, admin and media `package.json`: bump `eslint` to `^10.15.0` and `typescript-eslint` to `^8.73.0`. Admin also bumps `@eslint-react/eslint-plugin` to `^5.30.4`. Delete the minimatch overrides.
3. Root: add `"@microsoft/eslint-formatter-sarif": { "eslint": "$eslint" }`.
4. All three: add `"uri-js": "npm:uri-js-replace@^1.0.3"`.
5. Run `npm install`. Then run `npm update undici` in root and admin, and `npm update qs` in media.
6. Run `override-range-lint.mjs --update-baseline` per directory. Re-run the lint with `--verify-installed-versions --fail-on-stale`.
7. Per workspace, run `npm run lint`, `typecheck`, `test` and `build`. Then generate SARIF with the root formatter.
8. Add a 2026-11-18 section to `docs/ci/minimatch-advisory-repair.md`.
9. Open the PR. Its body records picomatch as having no fix and lists the SonarCloud action Mark must take.
