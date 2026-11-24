# Repository identity in trust gates — Design

Issueless ad-hoc run (weekly hygiene sweep, 2026-11-18). This is a companion to #340 (the
control-plane module rename). The project lead decided on a separate PR because these values
decide which repository a trust gate admits.

## Finding

Four trust decisions still name the pre-rename repository name. In this repository they
never match, so each one refuses everything:

- `infrastructure/deploy/tests/selfhost-acceptance.py` `_ci_runner_profile_valid`: the
  self-hosted acceptance runner admits a self-hosted runner only for that repository.
- `infrastructure/deploy/tests/selfhost-nginx-fixture.sh`: the disposable acceptance VM
  fixture refuses to run for any other repository.
- `services/control-plane/pkg/config/config.go`: the production default of
  `ATTESTATION_OIDC_SUBJECT_PREFIX` (and its doc comment). An attestation publish is accepted
  only from a GitHub OIDC token whose `sub` starts with this prefix.
- `.github/scripts/tests/sonar-required-gate.test.sh`: the `WORKFLOW_REPOSITORY` fixture value.

## Approach

Replace the pre-rename repository name with `Concord-Voice/Self-Hosted` in those gates and in
the tests and test support that model the same identity: `selfhost_acceptance_support.py`,
`test_selfhost_acceptance.py`, `config_test.go`, `oidc_test.go`, `publish_handler_test.go`,
`router_dm_icon_test.go` and `verify-sonar-caller-proof/main_test.go`. Matching stays exact:
no gate gains a prefix match, case folding or trimming.

Not changed: the Go module import path (#340 owns it), and
`selfhost_acceptance_support.py`'s guard list for `build-desktop.yml`, a workflow that does
not exist in this repository.

## Acceptance criteria

- Each gate admits `Concord-Voice/Self-Hosted` and refuses a look-alike
  (`Concord-Voice/Self-Hosted-fork`), a case variant, a padded value and any other repository.
  `test_runner_profile_closed_pairs_refuse_before_executor_calls` gains those refusal cases.
- `go vet` passes, and the DB-free config and OIDC tests pass. The DB-backed tests run in CI.
- No title, body or commit names the pre-rename repository.
