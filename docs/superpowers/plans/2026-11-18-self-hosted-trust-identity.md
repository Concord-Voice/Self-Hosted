# Repository identity in trust gates — Plan

Spec: `docs/superpowers/specs/2026-11-18-self-hosted-trust-identity-design.md`

1. Branch `fix/self-hosted-trust-identity` from `origin/main`.
2. Replace the pre-rename repository name with `Concord-Voice/Self-Hosted` in the files the
   spec lists. Leave module import paths and the `build-desktop.yml` guard list unchanged.
3. Add look-alike, case and padding refusal cases to the runner-profile test.
4. Verify: call `_ci_runner_profile_valid` directly for admitted and refused values, run the
   nginx fixture's guard with each, run `go vet` and the DB-free tests.
5. Draft PR. Reviews: security-reviewer and red-team (trust-boundary change).
