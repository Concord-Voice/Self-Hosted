# Recover the migration marker after a reservation DDL timeout

A disposable PostgreSQL check found that migration 206 can roll back its DDL and still leave a dirty migration marker. The older retry instruction leaves the rollout blocked.

Document checking migration state, verifying the last applied schema, and using the existing clean-version recovery procedure. Keep the backfill timeout and post-gate trigger cleanup separate.

## Acceptance criteria

Validate the relative runbook link and compare the documented sequence with the prior disposable migration fixture.
