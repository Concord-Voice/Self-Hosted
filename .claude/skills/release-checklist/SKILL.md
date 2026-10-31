---
name: release-checklist
description: Run release verification checks for a new version
argument-hint: [version]
disable-model-invocation: true
allowed-tools:
  - Bash(git *)
  - Bash(gh *)
  - Bash(npm *)
  - Bash(go *)
  - Read
  - Grep
  - Glob
---

Release checklist for version **$0**.

> Paths below use `frontend/` and `backend/` as literal stand-ins for this
> repo's actual frontend and backend directories (e.g. `apps/desktop/` and
> `services/api/`) — adjust every occurrence to your actual layout
> before running these commands.

## 1. Version Consistency

- [ ] Check current version: `cat frontend/package.json | jq .version`
- [ ] Verify tag doesn't exist: `git tag -l "v$0"`
- [ ] Desktop version matches: `frontend/package.json`

## 2. CI Status

- [ ] All GitHub Actions passing on main: `gh run list --branch main -L 5`
- [ ] SonarQube Quality Gate passing
- [ ] No open release-gating issues for this milestone: `gh issue list --milestone "v$0" --label "risk: release-gate"`

## 3. Test Suite

- [ ] Go tests pass: `cd backend/ && go test ./... -race`
- [ ] Frontend tests pass: `cd frontend/ && npx vitest run`
- [ ] Coverage >= 80% on new code (SonarQube enforced)

## 4. Security Checks

- [ ] No critical Dependabot alerts: `gh api repos/{owner}/{repo}/dependabot/alerts --jq '[.[] | select(.state=="open" and .security_advisory.severity=="critical")] | length'`
- [ ] Pre-commit hooks pass on HEAD: `pre-commit run --all-files`
- [ ] No secrets in codebase: `gitleaks detect --source .`

## 5. Documentation

- [ ] CHANGELOG.md updated for v$0
- [ ] CLAUDE.md counts current (migrations, stores, routes)
- [ ] API docs reflect any new endpoints

## 6. Build Verification

- [ ] Desktop build succeeds locally: `cd frontend/ && npm run build`
- [ ] Go build succeeds: `cd backend/ && go build ./...`

## 7. Release Steps

Once all checks pass:
1. Bump version in `frontend/package.json` to `$0`
2. Commit: `git commit -m "chore: bump version to $0"`
3. Push to main (`main-ci.yml` calls `build-desktop.yml` via `workflow_call` after scans pass)
4. Monitor build: `gh run list --branch main -L 1 --workflow main-ci.yml` — push:main release
   runs appear under `main-ci.yml`; only `workflow_dispatch` runs appear under
   `build-desktop.yml`
5. Verify release artifacts on GitHub Releases page

## 8. Post-Release

- [ ] Close milestone: `gh api repos/{owner}/{repo}/milestones/{id} -X PATCH -f state=closed`
- [ ] Verify auto-updater delivers to existing clients
- [ ] Smoke test: login, messaging, voice call on fresh install
