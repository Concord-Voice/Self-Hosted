# Weekly CI flake fixture repairs

Date: 2026-11-09
Status: authorized ad-hoc repair; source owner: `Concord-Voice/Self-Hosted`

## Evidence and scope

The weekly report confirmed these fail-then-pass runs on identical tested code:

| Test | Failed then passed | Observed failure | Causal fixture issue |
| --- | --- | --- | --- |
| `TestCollectorDropsFailedWriteIntervalAndContinuesRetention` | Hosted [42039710519/job 127287442450](https://github.com/Concord-Voice/Hosted/actions/runs/42039710519/job/127287442450) -> [attempt 2/job 127298294712](https://github.com/Concord-Voice/Hosted/actions/runs/42039710519/job/127298294712) | second `write` missing | buffered `prune` precedes `busy=false` |
| `TestThePeerCheckSpendsTheRequestDeadline` | Hosted [42059340730/job 127357207022](https://github.com/Concord-Voice/Hosted/actions/runs/42059340730/job/127357207022) -> [attempt 2/job 127369402069](https://github.com/Concord-Voice/Hosted/actions/runs/42059340730/job/127369402069) | expected 503/note missing | real 30 ms + 30 ms timing raced a 50 ms deadline |
| `TestRestoreBeginEndsOnTheReplicationCheck` | Hosted [42185653861/job 127785491107](https://github.com/Concord-Voice/Hosted/actions/runs/42185653861/job/127785491107) -> [attempt 2/job 127795340247](https://github.com/Concord-Voice/Hosted/actions/runs/42185653861/job/127795340247) | serve lock appeared leaked | one-second fixture deadline equals the lock-watch interval; cause remains unknown |
| `TestDeviceTransactionsPassTheReleaseGate` | Self-Hosted [42295328210/job 128159205999](https://github.com/Concord-Voice/Self-Hosted/actions/runs/42295328210/job/128159205999) -> [attempt 2/job 128165291663](https://github.com/Concord-Voice/Self-Hosted/actions/runs/42295328210/job/128165291663) | `store`, not `replication`, after 215 ms | a 200 ms whole-transaction fixture budget can expire before the release gate |

The report verified source/tested SHA, workflow revision, and that passing attempts executed the named tests. It does not prove a production defect. Change only the four existing test files; production deadline, lock, release-gate, workflow, and dependency behavior stay unchanged. `services/` is imported into Hosted after the Self-Hosted owner merges.

## Design

| Fixture | Repair | Oracle retained |
| --- | --- | --- |
| Collector | Wait for `!collector.busy.Load()` after all first-interval events and before tick two. | No-overlap behavior, failed-write dropping, and retention continue. |
| HTTP deadline | Use `testing/synctest` virtual time for the existing 30 ms peer + 30 ms handler against the 50 ms request deadline. | The deadline begins on request arrival, spans peer and route, and produces exactly one non-client 503. A fresh route deadline is a negative control. |
| Restore | Cancel only inside the existing replication probe rather than via a one-second timeout. | Probe entry, committed metadata, immediate lock reacquisition, and a successful confirmed retry. If immediate reacquisition fails, stop for production diagnosis; never poll/retry it. |
| Device | Cancel only inside the replication probe rather than with a 200 ms transaction budget. | Probe entry and the exact 503 replication envelope, with no signed outcome bytes. |

Authority integration tests use the supplied isolated `AUTHORITY_TEST_DATABASE_URL` for `concord_authority_test`, `-p 1`, and shared Go caches. They never default to a developer database. Historical logs are evidence only and no secrets or personal data enter fixtures.

## Acceptance

- Focused `-race -count=1` commands in the plan pass, including serialized authority-store coverage.
- No production diff; every test synchronizes on causal state or virtual time rather than an enlarged wall-clock margin.
- The restore immediate-lock assertion remains load-bearing.
- `scripts/update-claude-md-counts.sh` and the split's quality-gate definition are absent. This is a coverage gap, not a passed probe.
