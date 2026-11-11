# Open go-transitive-flush PRs with an App installation token — Design (Issue #250)

## Understanding

**Intended outcome.** The weekly Go transitive-flush workflow opens its pull request, and that pull request runs the full PR check set with no manual step.

**Why it does not today.** `.github/workflows/go-transitive-flush.yml` passes `github.token` to `peter-evans/create-pull-request`. This repository does not allow GitHub Actions to create pull requests, so the step fails. Both scheduled runs so far (2026-11-03, 2026-11-10) failed at "Open PR if changed". Allowing Actions to create pull requests would not fix it: a pull request authored by `GITHUB_TOKEN` starts no `pull_request` workflows.

**Success looks like.** A dispatch from `main` with no Dependabot Go pull request open produces an App-authored pull request on which `pr-ci.yml` and `required-pr-build.yml` run. The job that fetches Go modules references no App credential.

**Constraint.** This is a public repository. The workflow keeps its `schedule` and `workflow_dispatch` triggers, so a fork never reaches the App key.

## Decision

Option A from the issue's Design Dossier: reuse the App that `refresh-ioc-list.yml` already uses. The token is minted in a second job that runs no Go tooling.

Rejected:

- **Dedicated App in a protected environment.** Smaller blast radius, but needs organization-admin provisioning before any code can land. Revisit if the shared key gains more consumers.
- **One job, with the token minted first or late.** A job receives every secret it references when the job starts, whatever the step order. The App private key would be on the runner while `go get -u` downloads and resolves third-party modules. The first draft of this design minted late in one job; the branch review found the gap.

## Design

All changes are in one workflow, which now has two jobs.

1. **Job `refresh`** keeps the contention guard, the module refresh and the body render. It references no App credential. Its `GITHUB_TOKEN` has `contents: read` and `pull-requests: write`; the second is for the guard's deferral comment. When `go.mod` or `go.sum` changed, it uploads those two files and the rendered body as a one-day artifact and sets the job output `has_changes`.
2. **Job `open-pr`** has `needs: refresh` and runs only when `has_changes` is `true`. It checks out the repository, copies the two module files out of the artifact, mints the App token with the same pinned `actions/create-github-app-token` SHA and the same `PUBLIC_MIRROR_APP_CLIENT_ID` / `PUBLIC_MIRROR_APP_PRIVATE_KEY` inputs as `refresh-ioc-list.yml`, and hands the token to `create-pull-request`. It runs no Go command. Its `GITHUB_TOKEN` has `contents: read` only.
3. **`create-pull-request`** takes `token: ${{ steps.app-token.outputs.token }}`. The pinned `author` and `committer` identities stay as they are.
4. **Rendered PR body** loses the "CI does not run on this PR until you re-trigger it" section. The `Founding issue` line goes too: its number belongs to another repository and would autolink to the wrong issue here.
5. **Header comment** is rewritten to describe App authorship and the two-job boundary. Issue numbers from another repository are removed.

Job separation is the security property: the job that fetches modules cannot reach the App key, and the job that holds the key treats the artifact as data. Deferred runs and no-change runs never start `open-pr`, so they mint nothing.

`refresh` still holds a `GITHUB_TOKEN` that can comment on pull requests while modules are fetched. That is accepted: it cannot write repository contents.

**Amended in review.** The residual above is removed. The contention guard moved to its own job, `guard`, which holds the `pull-requests: write` scope and runs no Go tooling. `refresh` now has `contents: read` only and references no secret, and `actions/setup-go` runs with `cache: false`. The workflow has three jobs: `guard`, `refresh`, `open-pr`.

## Testing

`scripts/tests/test-go-transitive-flush-render.sh` already extracts and runs the real "Render PR body" block. Two changes:

- The assertion that the body carries "Close and reopen this PR" flips to assert its absence, and the body must carry no bare issue reference.
- New static assertions on the workflow file: `refresh` references no App credential; `open-pr` is gated on `has_changes` and runs no Go tooling; there is exactly one mint step and one key reference, both in `open-pr`; only the PR step consumes the token; no job grants `contents: write`.

End-to-end proof is two manual dispatches, recorded on the issue: one with no Dependabot Go pull request open (opens an App-authored pull request with PR checks running), one with such a pull request open (posts one deferral comment, opens nothing). The first needs a moment when no Dependabot Go pull request is open; at the time of writing #245 is open.

## Docs

- `.claude/rules/domains/backend.md` § Dependency Hygiene: replace the "CI re-trigger required" bullet, and correct the "Bot-PR CI re-trigger pattern" paragraph, which still lists this workflow as `GITHUB_TOKEN`-authored.
- `.claude/skills/weekly-deps/SKILL.md`: the discovery query `author:app/github-actions` will no longer find the flush pull request. Add a query on the `routine: go-transitive-hygiene` label.

## Out of scope

- The repository label `routine: go-transitive-hygiene` does not exist yet. It is created with `gh label create` when this lands; it is repository state, not a file in this change.
- A weekly npm flush, which will copy this token step, is separate work.

## Acceptance criteria

The issue's acceptance criteria apply unchanged. This repository configures no quality gate in `.claude/repo-config.json`, so the gate is the existing pre-commit and PR check set.
