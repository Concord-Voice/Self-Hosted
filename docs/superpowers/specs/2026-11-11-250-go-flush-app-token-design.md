# Open go-transitive-flush PRs with an App installation token — Design (Issue #250)

## Understanding

**Intended outcome.** The weekly Go transitive-flush workflow opens its pull request, and that pull request runs the full PR check set with no manual step.

**Why it does not today.** `.github/workflows/go-transitive-flush.yml` passes `github.token` to `peter-evans/create-pull-request`. This repository does not allow GitHub Actions to create pull requests, so the step fails. Both scheduled runs so far (2026-11-03, 2026-11-10) failed at "Open PR if changed". Allowing Actions to create pull requests would not fix it: a pull request authored by `GITHUB_TOKEN` starts no `pull_request` workflows.

**Success looks like.** A dispatch from `main` with no Dependabot Go pull request open produces an App-authored pull request on which `pr-ci.yml` and `required-pr-build.yml` run. No write-capable token exists in the job while Go modules are fetched.

**Constraint.** This is a public repository. The workflow keeps its `schedule` and `workflow_dispatch` triggers, so a fork never reaches the App key.

## Decision

Option A from the issue's Design Dossier: reuse the App that `refresh-ioc-list.yml` already uses, and mint its token late.

Rejected:

- **Dedicated App in a protected environment.** Smaller blast radius, but needs organization-admin provisioning before any code can land. Revisit if the shared key gains more consumers.
- **Mint first, one identity for the whole job.** Simpler, but a write token would be live while `go get -u` downloads and resolves third-party modules, and on runs that defer or find no change.

## Design

All changes are in one job of one workflow.

1. **New step "Mint flush App token"** (`id: app-token`), placed after "Render PR body" and before "Open PR if changed", with the same `if:` as those two steps. It uses the same pinned `actions/create-github-app-token` SHA and the same `PUBLIC_MIRROR_APP_CLIENT_ID` / `PUBLIC_MIRROR_APP_PRIVATE_KEY` inputs as `refresh-ioc-list.yml`, with `permission-contents: write` and `permission-pull-requests: write`.
2. **"Open PR if changed"** takes `token: ${{ steps.app-token.outputs.token }}`. The pinned `author` and `committer` identities stay as they are.
3. **Job permissions** drop from `contents: write` + `pull-requests: write` to `contents: read` + `pull-requests: write`. `GITHUB_TOKEN` is now used only by the contention guard, which lists pull requests and posts a deferral comment.
4. **Rendered PR body** loses the "CI does not run on this PR until you re-trigger it" section. The `Founding issue: #1212` line goes too: that number belongs to another repository and would autolink to the wrong issue here.
5. **Header comment** is rewritten to describe App authorship and the late-mint ordering. Issue numbers from another repository (`#1228`, `#847`) are removed.

Ordering is the security property: the mint step runs only after the module refresh has finished and only when a pull request will be opened. Deferred runs and no-change runs never mint a token.

## Testing

`scripts/tests/test-go-transitive-flush-render.sh` already extracts and runs the real "Render PR body" block. Two changes:

- The assertion that the body carries "Close and reopen this PR" flips to assert its absence.
- A new static assertion on the workflow file: the mint step appears after the "Refresh transitive Go module graph" step, and "Open PR if changed" takes its token from `steps.app-token`. This is the regression lock for the ordering property.

End-to-end proof is two manual dispatches, recorded in the PR: one with no Dependabot Go pull request open (opens an App-authored pull request with PR checks running), one with such a pull request open (posts one deferral comment, opens nothing). The first needs a moment when no Dependabot Go pull request is open; at the time of writing #245 is open.

## Docs

- `.claude/rules/domains/backend.md` § Dependency Hygiene: replace the "CI re-trigger required" bullet, and correct the "Bot-PR CI re-trigger pattern" paragraph, which still lists this workflow as `GITHUB_TOKEN`-authored.
- `.claude/skills/weekly-deps/SKILL.md`: the discovery query `author:app/github-actions` will no longer find the flush pull request. Add a query on the `routine: go-transitive-hygiene` label.

## Out of scope

- The repository label `routine: go-transitive-hygiene` does not exist yet. It is created with `gh label create` when this lands; it is repository state, not a file in this change.
- A weekly npm flush, which will copy this token step, is separate work.

## Acceptance criteria

The issue's acceptance criteria apply unchanged. This repository configures no quality gate in `.claude/repo-config.json`, so the gate is the existing pre-commit and PR check set.
