# Weekly hygiene: OpenSSF Scorecard alerts — Design (ad-hoc, issueless)

Scope: the 11 open Scorecard code-scanning alerts on 2026-11-18 (base `65646fc8`).

## Pinning alerts (PinnedDependencies)

| Alert | Location | Cause | Fix |
|---|---|---|---|
| #464, #465 | `ioc-scanning.yml` | `pip install --user pyyaml` (no version, no hash) | Use the established line: `--require-hashes --only-binary=:all: -r .github/requirements-pyyaml.txt` |
| #466 | `semgrep.yml` | `pip install "semgrep==X"` (version, no hash) | New hash-locked `.github/requirements-semgrep.txt` (semgrep + full transitive closure, `--generate-hashes`), installed with `--require-hashes`. A step fails when the file's semgrep pin and `SEMGREP_VERSION` disagree. Dependabot's existing `pip` entry on `/.github` covers the new file. Cache key uses `hashFiles` of the file |
| #463 | `scripts/npm-install-if-lockfile-changed.sh` | `npm install` | `npm ci`: lock-exact and never rewrites the lockfile, so the mutation-warning block becomes dead and is removed |
| #477, #478 | `scripts/concord-dev.sh` | `npm install` on an existing tree | `npm ci` behind a lock-hash stamp in `node_modules/`: skip when the stamp matches `package-lock.json`, otherwise `npm ci`. Warm `up` stays zero-cost; the media-plane two-pass (`--ignore-scripts` + `npm rebuild`) is kept |
| #498 | `npm-lockfile-flush.yml` | `npm update --package-lock-only` | No code fix exists: the job's purpose is to re-resolve lockfiles, which cannot be hash-pinned. It runs `--package-lock-only --ignore-scripts --min-release-age=7` and its output ships as a reviewed PR. Dismiss `won't fix` with that reason |

## Repo-level alerts

| Alert | Live state | Disposition |
|---|---|---|
| #468 Maintained | Repo created 2026-09-06 (73 days) | Dismiss `won't fix`: clears by itself after 2026-12-05 |
| #469 CII-Best-Practices | No badge | Dismiss `won't fix`; registering is an external account action — decision for Mark |
| #467 Code-Review | 1/29 changesets approved; single human maintainer, PRs are bot-reviewed and admin-merged | Dismiss `won't fix`; a second human reviewer is a staffing decision — reported to Mark |
| #444 Branch-Protection | Default ruleset: OrganizationAdmin bypass `exempt`, 1 required approval | Settings change (remove admin bypass, 2 approvals) would block the sole maintainer. Escalate to Mark; not dismissed here |

## Acceptance

- actionlint and `workflow-validate.yml` checks pass; `scripts/test-npm-install-if-lockfile-changed.sh` and the `concord-dev` tests pass.
- `pip install --require-hashes -r .github/requirements-semgrep.txt` succeeds on Linux x86_64 Python 3.12.
- No repo settings, rulesets or branch protection changed.
