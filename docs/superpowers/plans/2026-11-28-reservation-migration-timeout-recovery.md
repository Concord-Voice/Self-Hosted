# Recover the migration marker after a reservation DDL timeout implementation plan

1. Confirm the affected source contract on the current default branch.
2. Document checking migration state, verifying the last applied schema, and using the existing clean-version recovery procedure. Keep the backfill timeout and post-gate trigger cleanup separate.
3. Validate the relative runbook link and compare the documented sequence with the prior disposable migration fixture.
4. Run the applicable independent reviews and required CI. Request merge approval for the reviewed head.
