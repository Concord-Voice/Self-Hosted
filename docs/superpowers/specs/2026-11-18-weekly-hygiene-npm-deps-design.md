# Weekly hygiene: npm dependency risks — design (ad-hoc, issueless)

Scope: the npm findings from the 2026-11-18 SonarCloud SCA scan of `Concord-Voice_Self-Hosted`, and Dependabot alerts 8 and 9. Go modules, source, Python and workflows are out of scope. Other batches of the same sweep own them.

## Findings and decisions

| Package | Found | Advisory range | Exposure | Decision |
|---|---|---|---|---|
| undici | 8.12.3 (root, admin; dev, via jsdom) | through 8.12.3 (6 CVEs) | dev only | Lockfile refresh to 8.12.4 (published 2026-11-05), inside jsdom's `^8.12.1` |
| qs | 6.16.1 (media; runtime, via express/body-parser) | stringify `allowEmptyArrays` key encoding | runtime package, but no media-plane route parses with qs | Lockfile refresh to 6.17.1 (published 2026-10-28) |
| minimatch | 10.2.6 (root, admin), 3.1.5 (root) | `>= 7.0.0, < 11.0.1`, no 10.x backport | dev only | Parent releases that declare minimatch 11 are inside the 7-day release-age cooldown. Use a scoped override `minimatch: ^11.0.3`, resolved to 11.0.7 (published 2026-11-10), with the forced consumers recorded in each baseline. 3.1.5 is outside the range and goes with the eslint 8 tree |
| eslint | 8.57.1 (root; nested under `@microsoft/eslint-formatter-sarif`) | `< 9.26.0`, RuleTester only | none at runtime: the formatter loads the host ESLint | Override the formatter's eslint to `$eslint`. This removes the tree |
| uri-js | 4.4.1 (all three; via ajv@6 under eslint) | through 4.4.1, abandoned upstream with no release | dev only. ajv@6 resolves the `$id` and `$ref` of ESLint rule schemas, which come from installed packages, not from user input | No fix exists. An `npm:` alias override to a fork was tried and withdrawn: `scripts/ci/check-lockfile-registries.py` accepts an alias only when a dependency edge declares it, which is a deliberate guard against lockfile package swaps. Accept in SonarCloud |
| picomatch | 4.0.7 (all four lockfiles; dev, via vite/vitest/tinyglobby) | `<= 4.0.7`, no patched release | dev only. All globs come from the repo's own config | No fix exists. The CI audit acceptance landed separately in #331. Accept in SonarCloud |

## Constraints

- Edit only `package.json` files, lockfiles, the override-range baselines that record the overrides, `docs/ci/minimatch-advisory-repair.md`, and one `CHANGELOG.md` entry.
- Do not edit `scripts/npm-install-if-lockfile-changed.sh` or `npm-lockfile-flush.yml`.
- Respect the 7-day release-age cooldown from `.github/dependabot.yml`: no new or changed lockfile entry may be younger than seven days.
- Every override that forces a declared range is recorded through `scripts/security/override-range-lint.mjs --update-baseline`.

## Acceptance criteria

- `npm ls` shows undici ≥ 8.12.4, qs ≥ 6.17.1, minimatch ≥ 11.0.1 everywhere, and no eslint 8, in every affected lockfile.
- Every changed lockfile entry was published on or before 2026-11-11.
- `python3 scripts/ci/check-lockfile-registries.py` and `scripts/security/npm-audit-gate.sh` pass.
- The override-range lint passes with `--verify-installed-versions --fail-on-stale` for the root, admin and media-plane directories.
- Lint, typecheck, test and build pass for every affected workspace. Tests that need native binaries the local environment lacks may defer to CI.
- Dependabot alerts 8 and 9 close because of the fix after merge.
