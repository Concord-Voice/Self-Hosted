# Weekly hygiene: npm dependency risks — design (ad-hoc, issueless)

Scope: the npm findings from the 2026-11-18 SonarCloud SCA scan of `Concord-Voice_Self-Hosted`, and Dependabot alerts 8 and 9. Go modules, source, Python and workflows are out of scope. Other batches of the same sweep own them.

## Findings and decisions

| Package | Found | Advisory range | Exposure | Decision |
|---|---|---|---|---|
| undici | 8.12.3 (root, admin; dev, via jsdom) | through 8.12.3 (6 CVEs) | dev only | Lockfile refresh to 8.12.4, inside jsdom's `^8.12.1` |
| qs | 6.16.1 (media; runtime, via express/body-parser) | stringify `allowEmptyArrays` key encoding | runtime | Lockfile refresh to 6.17.1. The fix shipped in 6.16.2 and 6.17.0 |
| minimatch | 10.2.6 (root, admin), 3.1.5 (root) | `>= 7.0.0, < 11.0.1`, no 10.x backport | dev only | Parent bumps: eslint `^10.15.0`, typescript-eslint `^8.73.0`, @eslint-react `^5.30.4`. These remove every minimatch override. 3.1.5 is outside the range and goes with the eslint 8 tree |
| eslint | 8.57.1 (root; nested under `@microsoft/eslint-formatter-sarif`) | `< 9.26.0`, RuleTester only | none at runtime: the formatter loads the host ESLint | Override the formatter's eslint to `$eslint`. This removes the tree |
| uri-js | 4.4.1 (all three; via ajv@6 under eslint) | through 4.4.1, abandoned upstream | dev only | Override to `npm:uri-js-replace@^1.0.3`. That fork has no mailto handler and no `pctDecChars`, and its `removeDotSegments` uses `[\s\S]`. ajv@6 calls only `parse`, `resolve` and `serialize` |
| picomatch | 4.0.7 (all four lockfiles; dev, via vite/vitest/tinyglobby) | `<= 4.0.7`, no patched release | dev only. All globs come from the repo's own config | No fix exists. Upstream fix PRs opened 2026-11-18 and are not merged. Record the reason. Mark accepts the finding in SonarCloud |

## Constraints

- Edit only `package.json` files, lockfiles, the override-range baselines that record the overrides, and `docs/ci/minimatch-advisory-repair.md`.
- Do not edit `scripts/npm-install-if-lockfile-changed.sh` or `npm-lockfile-flush.yml`.
- Every override that forces a declared range is recorded through `scripts/security/override-range-lint.mjs --update-baseline`.

## Acceptance criteria

- `npm ls` shows undici ≥ 8.12.4, qs ≥ 6.17.1, minimatch ≥ 11.0.1, no eslint 8, and uri-js resolved to uri-js-replace in every affected lockfile.
- The override-range lint passes with `--verify-installed-versions --fail-on-stale` for the root, admin and media-plane directories.
- Lint, typecheck, test and build pass for every affected workspace. Tests that need local binaries this Mac does not have may defer to CI.
- Dependabot alerts 8 and 9 close because of the fix after merge.
