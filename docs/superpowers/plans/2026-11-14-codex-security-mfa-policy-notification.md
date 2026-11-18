# MFA policy notification port plan

1. Port Hosted #4424's recipient query and per-user bounded send to Self-Hosted's matching handler. Keep the generation bump and wire payload intact.
2. Port its tests for owner and Administrator delivery, ordinary-member exclusion, no-op and commit-error behavior, and fail-closed recipient lookup.
3. Update the backend rule, OpenAPI contract, and prerelease changelog.
4. Run focused checks with an isolated `DATABASE_URL`, then publish a draft PR for CI and reviewer reconciliation. Do not use the shared database.
