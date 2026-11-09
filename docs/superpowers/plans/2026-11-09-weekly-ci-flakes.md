# Weekly CI flake fixture repairs: plan

See [the design](../specs/2026-11-09-weekly-ci-flakes-design.md). Scope is four existing Go tests and these two documents; no issue or production change is required.

1. In `internal/opsmetrics/collector_test.go`, retain all event and batch assertions, then wait for the collector's idle flag before the next tick.
2. In `internal/authority/server/http_test.go`, use `testing/synctest` to make the 30 ms peer check plus 30 ms route deterministic against the existing 50 ms request deadline. Keep GET and POST coverage, 503, and one non-client note. Confirm a deliberately fresh route deadline fails the test before restoring the real path.
3. In `internal/authority/store/owner_restore_integration_test.go`, replace its one-second timeout with `cancelAtReplicationProbe`. Keep probe-entry, committed metadata, immediate lock reacquisition, and confirmed retry checks. Treat a repeat lock failure as a production-diagnosis stop.
4. In `internal/authority/store/device_integration_test.go`, replace the 200 ms budget with probe-entry cancellation. Require probe entry and the complete 503 replication error envelope.

Run once, without stress retries:

```bash
cd services/control-plane
go test -race -count=1 ./internal/opsmetrics ./internal/authority/server -run 'TestCollectorDropsFailedWriteIntervalAndContinuesRetention|TestThePeerCheckSpendsTheRequestDeadline'
AUTHORITY_TEST_DATABASE_URL="$CV_FLAKE_AUTHORITY_URL" go test -race -p 1 -count=1 ./internal/authority/store -run '^(TestRestoreBeginEndsOnTheReplicationCheck|TestDeviceTransactionsPassTheReleaseGate)$'
git diff --check
```

`CV_FLAKE_AUTHORITY_URL` must name the task-provisioned `concord_authority_test` database. Do not set private Go caches or use `DATABASE_URL`. The count refresh script is absent in this split; record that gap. Phase-8 review needs security, code, scope, Go test-analysis, silent-failure, and documentation coverage.
