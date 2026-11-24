# Rename the control-plane Go module path — Design

Issueless ad-hoc run (weekly hygiene sweep, 2026-11-18). It is a companion to PR #332:
that PR's review found this pre-existing problem in `services/control-plane/go.mod`.

## Finding

`services/control-plane/go.mod` still declares the module path the code had in a private
repository before the repo split. This repository is public. Every Go import, the
`go test` coverage paths and two tooling references carry that path.

## Approach

1. Rename the module to `github.com/Concord-Voice/Self-Hosted/services/control-plane`.
   Substitute the full old path with the new one in every tracked file (1,412 files: 1,409
   `.go` files, `go.mod`, the CodeQL log-injection query and the NOTICE test).
2. Fix the three references that use the bare repository prefix rather than the full
   module path:
   - `.github/workflows/license-check.yml`: both `go-licenses --ignore` values, so the
     first-party module is still excluded from the license check.
   - `scripts/sonar/prepare-protected-pr-analysis.py`: the coverage-path prefix that a
     Go coverage record may carry. The comment that tied it to the old path is rewritten.
   - `scripts/test-update-notice-md.sh` case 10: the negative assertion now greps for the
     new first-party path, which is the one its `go` shim emits. With the old path it
     could never match, so the case was vacuous.
3. **Out of scope, decided here:** mentions of the private repository that are not the
   module path. These are provenance notes ("Imported from … at the repo split"), links to
   that repository's issues and PRs in registers and comments, and the repository-identity
   values: `GITHUB_REPOSITORY` / OIDC-subject values in test fixtures, the
   self-hosted acceptance release-authority guards
   (`infrastructure/deploy/tests/selfhost-acceptance.py`, `selfhost-nginx-fixture.sh`) and
   the production default `ATTESTATION_OIDC_SUBJECT_PREFIX`
   (`services/control-plane/pkg/config/config.go`). These are not the module path and do not
   affect the build. Changing them changes which repository a trust decision admits, so they
   go to the project lead as a separate decision.

## Merge interaction

Every open PR that touches Go files conflicts with this one. Whichever lands second re-runs
the substitution (step 1) on its own new or changed files before merging.

## Acceptance criteria

- `git grep` finds the old module path nowhere.
- `gofmt -l` is empty; `go build ./...`, `go vet ./...` (default and
  `integration,managedredis_integration` tags) pass; `go mod tidy -diff` is empty;
  `govulncheck ./...` reports no vulnerabilities.
- CI passes, including the full control-plane test suite, the Go license check, the
  NOTICE drift guard and the Sonar coverage upload.
