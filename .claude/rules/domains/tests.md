---
description: Test conventions for Go, TypeScript, and CI shell test files
paths:
  - "**/*_test.go"
  - "**/tests/**/*.test.ts"
  - "**/tests/**/*.test.tsx"
  - ".github/scripts/tests/*.test.sh"
  - "scripts/test-*.sh"
---

## Test the consumer, not the handshake

**A test that asserts a value is PASSED must be paired with one asserting it is OBEYED.**

Founding incident (PR #3145): `ScreenShareOptions.streamAudio` shipped to a green suite with
**zero readers**. The picker set it, the service forwarded it, and the capture path never
looked at it — so turning "Stream Audio" off still published the user's whole desktop mix to
the channel. Thirty-five picker tests passed throughout.

Every test written for that field asserted one of two things:

```ts
expect(onSelect).toHaveBeenCalledWith('screen:0', expect.objectContaining({ streamAudio: false }));
expect(svc.switchScreenSource).toHaveBeenCalledWith('screen:0', expect.objectContaining({ streamAudio: true }));
```

Both verify that our own code moved a value between our own functions. Neither can fail while
the behaviour is absent, because neither reaches the boundary where the value would be spent.
A suite shaped this way verifies a handshake and passes forever.

**The rule:** when a test asserts a value crosses an internal boundary, a sibling test must
assert the OUTERMOST observable effect that value is supposed to cause — the constraint handed
to `getUserMedia`, the row written to Postgres, the payload put on the socket, the producer
that does or does not exist. Assert against the seam the code does not own.

```ts
// The pairing that would have caught it — asserts what the OS was actually asked for.
await svc.captureScreenElectron('screen:0', dims, 30, /* wantAudio */ false);
expect(getUserMedia.mock.calls[0][0].audio).toBe(false);
```

**How to spot the gap in review:** a field with more write sites than read sites is the smell.
`grep` the identifier across `client/desktop/src/`; if every hit is an assignment, a prop-drill,
or a test, nothing consumes it. See
`client/desktop/tests/unit/services/voiceService.captureSeam.test.ts` for the shape. Paths in
this section are repository-relative — the example is desktop-renderer code, so a reviewer
running from the repository root needs the `client/desktop/` prefix to resolve any of it.

## The converse: a branch that is OBEYED must be proven REACHED

The section above pairs *a value is PASSED* with *a value is OBEYED*. This is its mirror,
and it is a different defect: a branch that obeys its input perfectly, reached by nobody.

Founding incident (#3198 PR 2, Phase-8 review): `screenAudioRefusalMessage` gained a
`platform` parameter to fix a real bug — a Linux user sharing a window was told to *"share a
whole screen to include sound"*, which on Linux is equally silent. The parameter landed. The
branch landed. Its tests landed and passed. **The sole production call site was never
updated**, so `platform` defaulted to `null` and the branch was dead outside the tests that
passed it by hand. The as-built entry (then in `CLAUDE.md`, now
`docs/reference/as-built-ledger.md`) and the commit message both recorded the defect as fixed, and
the value the call needed (`this.cachedPlatform`) was already read 34 lines above it.

Three layers of test surrounded that code, and every one was blind by construction:

| Test | Asserts | Why it cannot see a broken caller |
|---|---|---|
| `screenAudioCopy.test.ts` | the helper, called directly | it supplies the argument itself |
| `screenAudioVerdictAgreement.test.ts` | a table MIRRORING the verdict switch | a mirror of a switch cannot see that switch's body |
| `voiceService.screenAudioToggle.test.ts` | the real caller — but 3 of its 5 refusal cells | no row varied `platform` for a window target |

The third is the painful one: the right test already existed, at the right layer, and simply
did not vary the input whose absence was the defect.

**The rule:** when a pure helper gains a parameter, the regression test goes at the CALLER and
must VARY the new parameter there. A helper-level test proves a branch EXISTS; only a
caller-level test that varies the input proves the branch is REACHED.

**Take the cross-product over cells that reach the branch, not over all inputs.** Completing
the table above to all six `(sourceId, platform)` combinations is the wrong correction:
`('screen:0', 'darwin')` resolves to `'system-loopback'`, so audio is accepted and no refusal
message is produced at all. That row fails with `toMatch expects a string, but got object` —
a null `videoSlotError`, because nothing refused. Five cells reach the arm; the sixth belongs
to a different test. Enumerate the inputs, then drop the ones the branch does not serve, and
say in the table why each is absent.

**How to spot it in review:** a parameter with a default value whose only non-default callers
are tests. `grep` the function name across `src/`; if every production call site is shorter
than the signature, the new argument has no reader. The smell is the converse of the one
above — there, more write sites than read sites; here, more parameters than arguments.


# Test Rules

## General
- Test behavior, not implementation details
- Descriptive names: `TestHashCode_SameInput_ReturnsSameHash`
- Both positive and negative cases required
- Deterministic — no timing, random data, or external service dependencies
- No real credentials in test data (use `test-token-123` style values)

## Vacuity — a test that can pass for the wrong reason

**A fixture that can fail for more than one reason pins none of them.** When a case
asserts an outcome that two independent branches can both produce, disabling the
branch the case *names* changes nothing and the case still passes. It reads as
coverage and is not.

The tell is a fixture carrying more setup than the case needs. Build the narrow one:
remove whatever else could produce the outcome, so the branch under test is the only
thing that can.

**Exit status is the most common carrier**, because a diagnosis and a crash are both
non-zero. A guard that reports a problem exits 1; the same guard with that check
deleted raises an exception and also exits 1. Assert the message, not just the code —
`.github/scripts/tests/*.test.sh` use a `check_msg` helper for exactly this.

**A second carrier is a mock queue that outlives its test** — see § The `*Once`
queue outlives the test that queued it, under TypeScript/React Tests.

**A third is a pipe into `grep -q` under `pipefail`.** `grep -q` exits on its first
match, a writer that is still writing takes SIGPIPE (or EPIPE where the runner ignores
the signal), and `pipefail` turns that into a non-zero pipeline. A positive assertion
then fails on a MATCH; a negated one (`! printf … | grep -q X`) passes while `X` is
present. It is timing-dependent — measured at 2 in 18,000 runs on a 365-byte value —
so it surfaces as a flake. Use `grep -q X <<<"$OUT"`, or `grep -q X < <(cmd)` for a
command's output. Founding incident: `scripts/test-check-mirror-pairing.sh` in CI run
35924955957, after `check-mirror-pairing.sh` had already been fixed for the same shape.

**Prove it by mutation, not by inspection.** Revert the fix the case exists for and
confirm the case goes red. A surviving mutant means the assertion is not attached to
the code it names — reading the test cannot tell you this, and neither can a green run.

**A new early-exit branch can make an OLD test vacuous, so mutate what you did not
touch.** Adding a check that runs before an existing one gives every fixture a second
way to reach the same failure, and the older case silently stops pinning its own guard.
Mutating only the branches you added cannot see this: the new cases all go red on cue
while the shadowed one stays green.

Founding incident: PR
#3146 — three
instances of one shape in a single file. Cases asserting `exit 1` on a wiring lint
passed off an *implementation-path* branch while claiming to pin the *suite-path*
branch, because the fixture happened to contain both. Two were found by mutation
testing during authoring and fixed; the third shipped and was caught in review. A
fourth mutant survived on the crash-vs-diagnosis shape above. A fifth appeared the other
way round: a preamble gate added to the changelog promoter ran *before* its
duplicate-version guard, so the duplicate case — untouched, and green since long before
that PR — could no longer fail for its own reason, and deleting the guard it named left
every one of the 28 cases passing.

**The mirror shape — a new branch DOWNSTREAM of an existing one, sharing its
observable — hides until the old guard is deleted.** A refusal added *after* an
existing refusal is unreachable while that one stands, so it cannot shadow anything
today. It shadows the moment someone removes the earlier check: the input now falls
through to your branch, produces the same observable, and the old test stays green
while the guard it names is gone. That is precisely the change the old test exists to
catch.

The detection profile is the opposite of the upstream case and is why this survives
review. Mutating the branches you ADDED shows nothing — they behave correctly.
Mutating the branch you did NOT touch is the only thing that exposes it, and only when
the two branches produce distinguishable observables. **Copy-paste is the mechanism**:
adding a refusal beside an existing one makes reusing its message the natural move, and
that reuse IS the defect. Give every refusal its own observable, and only collapse two
when an indistinguishability requirement demands it — a deliberately-uniform refusal is
a decision to document, not a default.

Prove it with three arms, because two cannot tell you whether the vacuity was real:

| arm | what it establishes |
|---|---|
| control, unmutated | the test runs and is green on correct code |
| old guard deleted, **distinct** observable | the test now catches the deletion |
| old guard deleted, **shared** observable | the vacuity was real, not hypothetical |

Without the third arm you have fixed something you never demonstrated was broken.

Second occurrence of the class, PR
#3363 — a new
non-canonical-`user_id` refusal in the media-plane socket middleware reused the
`'Invalid token: missing user_id'` string of the presence check directly above it. Delete
the presence check and `rejects token missing user_id claim` passed off the new branch.
Measured at all three arms above; the third confirmed it.

## Go Tests
- stdlib `testing` + `stretchr/testify` (assert, require)
- Use `t.Run("descriptive case", ...)` for subtests
- `require` for preconditions (stops on failure), `assert` for checks (continues)
- Integration tests use real PostgreSQL + Redis via Docker in CI
- Race detection enabled: `go test -race`
- Run with `-p 1` for integration tests — see § Test isolation for what that flag is actually for
- **Never use a single-return type assertion on decoded JSON.**
  `x := body["k"].(map[string]interface{})` panics on a shape mismatch, and a panic aborts the
  whole package test binary — every other test in the package reports as failed and the
  assertion that actually tripped is buried in a stack trace. Use `testhelpers.JSONField` /
  `JSONElem` / `JSONAs`, or a plain two-return comma-ok. Note the helpers take `*testing.T` and
  not `require.TestingT`: that interface is `{Logf, Errorf, FailNow}` with no `Helper()`, and
  without `t.Helper()` every failure reports the helper's line instead of the caller's.
  Enforced by `scripts/check-no-single-return-json-assertion.sh` at pre-commit **and**
  whole-tree in `pr-ci.yml`'s `static-guards` job; `scripts/json-assertion-allowlist.txt` holds
  the shrinking set of not-yet-swept files and errors on a stale entry, which is what forces
  each sweep PR to delete its own entries. Founding sweep:
  #2811; helpers proven in
  PR #2804.
- **A negative WebSocket window needs a quiet channel and a live observer.** "Read for N ms and
  fail on frame X" proves nothing unless both hold. (1) Every OTHER producer of X is drained
  before the stimulus, by construction: a client gets `server_voice_counts` from the
  registration snapshot AND the subscribe catch-up, unordered against each other and against
  `connection_ready`, so draining "the first one" can drain the wrong one and the late one reads
  as the stimulus's output. (2) The observer survives the fail-closed path for the whole window:
  `DisconnectAllRichPresenceClients` closes EVERY local client, and a window that ends at EOF
  observed nothing. So require the read that ends the window to be a timeout (`errors.As` to
  `net.Error`, `Timeout()`), and fail on any other error instead of breaking out of the loop.
  Prove (2) by mutation: make the code under test emit X and confirm red. Then keep a positive
  control in the suite, a sibling test that drives the SAME fixture and the SAME reader with X
  allowed to arrive and requires it, so a reader that can no longer match X fails the control
  instead of silently passing the negative. Match every wire form of X, too: a roster removal
  arrives as a per-user `left` OR a channel-wide `room_empty`, and a reader that knows only one
  form lets the other through.
  Go counterpart of § Async assertions' "never wrap a negative assertion in `waitFor`".
  Founding incident: `TestHandleHeartbeat_AmbiguousCommitEmitsNoRosterOrCountRemoval` flaked on
  (1) in CI run 36530282352 and was vacuous on (2) underneath — three emitting mutants
  survived it 20/20. A `@red-team` pass on the first fix then found a `room_empty` emission
  the reader could not see.
- **A P-384 point in an account-authority fixture must be a real point on the curve.** The
  `authoritycontract` codec checks every 97-byte point position (`kP97`, `checkPoint` in
  `crypto.go`), so a fixture made of `0x04` plus filler bytes makes `Encode()` return nil. The
  test then fails for a reason that has nothing to do with its assertion, or a refusal it
  expects comes from the codec instead of the rule it names. Derive a fixture point as `b·G`
  through `crypto/ecdh` (`P384().NewPrivateKey` over a fixed scalar), or use
  `authoritytest.Point(key)`. It happened three times in #3587 PR-B1, in `trPoint`
  (`authoritycontract`), `pt` (`authority/policy`) and `p97` (`authority/store`).
- **Find files relative to the package directory, never with `runtime.Caller`.** `go test`
  starts each test binary in its package's source directory, so `os.ReadFile("handlers.go")`
  and `filepath.Join("..", "..", "migrations")` work everywhere. `runtime.Caller` returns a
  module-relative import path under `-trimpath`, which CI's `go test` uses, so a lookup built
  on it fails there with "no such file". A shared helper that other packages' tests call runs
  in the caller's directory instead: use `testdb.ModuleDir`, which walks up to `go.mod`, and
  resolve it at package init, before any test can `t.Chdir`. The reason for `-trimpath`:
  without it the absolute source directory is part of Go's build-cache key, so every worktree
  fills the shared cache with its own copy of each compiled package.

## Test isolation — Redis and PostgreSQL

Two backends, two different mechanisms, and neither is obvious from reading a test.

### Redis — one private logical database per process (#2680)

`internal/testhelpers/redistest` allocates a Redis logical DB index **once per
OS process**, by `INCR` on `concord:testredis:seq` in a reserved DB 0. Because
`go test` compiles one binary per package, per-process is simultaneously
per-package within a run and per-session across worktrees — one mechanism, both
collisions.

- **It is on by default and there is nothing to opt into.** Note the polarity,
  which is the reverse of what people assume: before #2680, leaving `REDIS_URL`
  unset was the *broken* configuration, because unset is what pinned DB 1 for
  everyone.
- **Get a client from `redistest`**, never by hand:
  `redistest.Client(t)`, or `redistest.Options(t)` when you need to mutate
  options before dialling. `redistest.URL(t)` and `redistest.DB(t)` are there
  when you need the raw values.
- **Never call `FlushDB`/`FlushAll` directly.** Use `redistest.Reset(ctx, c)`.
  It refuses when allocation failed (naming that cause, rather than telling you to
  call `Client` first — `Reset` forces allocation itself, so "unallocated" here
  always means "allocation failed"), and it refuses when the client's server
  address or DB index differs from the allocated one.
- **`REDIS_URL` must carry the `//`.** `redis://host:6379` works; the opaque form
  `redis:6379` does not. Both parse, and both name the same server — but an opaque
  URL puts the authority in `url.URL.Opaque` and leaves `Path` empty, and
  `URL.String()` emits `Opaque` *instead of* `Path`, so the per-process index
  cannot be written into it. Left unchecked this failed silently in the worst
  possible direction: allocation reported success, its probe PINGed fine (having
  selected DB 0), and every client in the process shared DB 0. `rewriteURL` now
  asserts the returned URL actually resolves to the index it was asked for, so
  this fails loudly at allocation instead.
- **DB 0 is reserved** for the ticket counter and the dev app's own data, and is
  never allocated to a test.
- `scripts/check-no-unscoped-redis-flush.sh` enforces the above. It runs at
  pre-commit **and** as two explicit steps in `pr-ci.yml`'s `static-guards` job
  (the guard whole-tree, plus its own self-test) — nothing invokes `pre-commit`
  in CI, so the hook alone would be invisible to a `--no-verify` commit, an
  uninstalled hook, or a fork PR. Thirteen packages had independently hand-rolled
  their own client before #2680, which is why this is enforcement and not a
  convention.

> **What `Reset` guarantees, and what it does not.** It is a **mis-wiring check,
> not a mutual-exclusion primitive.** Ownership is *computed*, never *registered*:
> allocation takes a ticket but writes no claim to the allocated database, so the
> guard can only prove "this client's server and index are the ones this process
> computed" — not "no other process computed the same index." Under a
> `CONCORD_TEST_REDIS_DB` pin, or once allocations exceed the pool and wrap,
> two processes legitimately match and each is permitted to flush the other.
> Detecting *that* needs live registration, which this package deliberately does
> not keep.

**Escape hatch:** `CONCORD_TEST_REDIS_DB=<n>` pins an index and skips allocation
entirely — no `INCR`, no ticket. It is read once per process inside the allocation
`sync.Once`, so it applies to every package binary that inherits the environment.
**It disables isolation rather than redirecting it, and the blast radius is wider
than "two pinned processes".** Skipping allocation also skips the ticket, so the
pinned index is never *reserved* — nothing records that it is in use. The pin
therefore collides with two different populations: another process sharing the
same pin, **and** any ordinary unpinned process the allocator later hands that
same index. In both cases the indices match by construction, so `Reset` permits
each to flush the other, and neither side can detect it — this is the same
computed-not-registered limitation as the blockquote above, reached by a
different route. Export it for one foreground command while debugging — never in
a shell profile, a `.env`, or a shell you then run a second `go test` from — and
if something else is running tests at the same time, give the pinned run its own
Redis instance. `n = 0` and any non-positive value are rejected.

**Pool size.** The pool is `databases - 1`, read live via `CONFIG GET` with a
fallback of 15, which is what keeps the index computation valid against whatever
a given server reports. There is deliberately **no wrap warning**: the ticket
counts *cumulative* allocations, not concurrent ones, and one `go test ./...`
allocates ~60 indices, so any such warning becomes permanently true after the
first ordinary run and then fires forever while conveying nothing. Collision
needs more processes *simultaneously live* than the pool holds, which `-p 1` and
ordinary runner concurrency keep well out of reach.

**Known fidelity gap:** `redistest.Client` matches the old test helper, not
production — it does **not** set `ContextTimeoutEnabled`, which
`internal/database/redis.go` sets on every production client.

### Redis — frozen clock and dead clients for counter tests (#3455)

Counter and window logic (`stepup.DeleteSoftLock`, the message and DM delete
handlers, the router composition test) runs on an in-process `miniredis`
instead of `redistest`, and each file says why: not isolation, since every test
process already owns a private logical database, but the frozen clock.
`router_klipy_ratelimit_test.go` records the exception.

- **Advance time with `mr.FastForward`, never `time.Sleep`.** TTLs move only
  when told to, so a test crosses a 30 s window or a 24 h window in
  microseconds. Pick offsets that stay off the edge (29.5 s, not 29 s, where the
  limiter's TTL reaches exactly 0 and the test would depend on miniredis's `<= 0`
  rule).
- **Build a dead client with `MaxRetries: -1, DialerRetries: 1`.** go-redis
  defaults `DialerRetries` to 5, so one call against an unreachable address costs
  about 1.7 s; these options cost about 230 µs. The same applies to a client whose
  miniredis was closed under it. Cap the dead shapes: three in a unit file (nil
  client, `127.0.0.1:1`, `mr.Close()`), and two per route in a handler file
  (population user gets the 503 and the row survives; non-population user gets
  200).
- **No unscoped flush.** A miniredis instance is private to its test and needs
  none. A test on the shared logical database uses `redistest.Reset`, and
  `scripts/check-no-unscoped-redis-flush.sh` still rejects a bare `FlushDB` or
  `FlushAll`.

### PostgreSQL — one shared database behind a global advisory lock

Unlike Redis, Postgres is **not** isolated per process. `testhelpers/testdb`
takes a single global `pg_advisory_lock(0x434f4e434f5244)` for every
`SetupTestDB` caller, refcounted per call, with a 120 s timeout. Every test
process on the machine queues on that one lock. This is tribal knowledge that was
previously visible only by running `psql -l`.

**Consequence for running two worktrees concurrently: don't, unless you give one
its own database.** Both default to the same `DATABASE_URL`, and there are two
independent hazards — only one of which migration parity touches.

1. **Truncation — parity does not help.** `TruncateAllTables` is one
   unconditional multi-table `TRUNCATE` over every application table, and the
   advisory lock is held only from `SetupTestDB` to its cleanup, not for the
   process lifetime. Two processes therefore interleave at *test* granularity,
   and one process's cleanup deletes rows the other is still using. Measured at
   **matching** migration versions: `pq: insert or update on table "servers"
   violates foreign key constraint "servers_owner_id_fkey" (23503)` — the owner
   row vanished between two statements of the same test.
   Migration 000194 puts `BEFORE TRUNCATE` guards on the login-family evidence
   tables, so a bare `TRUNCATE` of them fails with `restrict_violation`. Reset
   them with `testdb.ExecReplica(db, "TRUNCATE ...")` (replica mode, superuser
   test role); `TruncateAllTables` already does.
2. **Schema divergence — parity does help.** Each binary runs `m.Up()` under a
   per-binary `sync.Once`, so a worktree at a higher migration count applies
   migrations the other's code does not expect.

**The remedy for (1) is separation, not synchronization:** point the second
worktree at its own database (`DATABASE_URL=…/concord_test_wt2` — migrations run
automatically on first `SetupTestDB`), or run the two serially. Matching
migration counts addresses (2) only. Fixing this properly is tracked in
#2790.

**Concurrent transaction teardown:** unblock or cancel the loader as needed, then join
any competing transaction operation with a bounded wait. Check its explicit `Rollback`
before canceling that transaction's context: `database/sql` cancellation can trigger an
asynchronous rollback that races the driver's state and produces `driver: bad connection`.
If teardown fails, use bounded fallback cancellation and joining so no goroutine remains
unobserved. This ordering applies to PostgreSQL lock-observer fixtures as well as their
cleanup paths.

### What `-p 1` is actually for

It is **not** about coverage: `-coverprofile` merges across packages regardless
of `-p`. Since #2680 it is also **not** about Redis. Two reasons remain, and both
are still live:

1. The single global `pg_advisory_lock` above — N concurrent package binaries
   queue on it and the tail can exceed the 120 s timeout.
2. `internal/voice` and `internal/rbac` share the `voice.enforce.*` NATS subject
   space and cross-talk when run concurrently.

Measured after the Redis fix landed: seven packages run concurrently **without**
`-p 1` fail widely — including `TestMigration000072_CodeRedemptions`, a pure
Postgres test — and pass cleanly with it. Dropping the flag for wall clock is a
separate, measured change.

## TypeScript/React Tests
- Vitest + @testing-library/react
- **Always call `resetAllStores()` in `beforeEach`** — state leakage causes flaky tests
- Set up auth context: `useAuthStore.getState().setAccessToken('mock-token')`
- MSW for API mocking — NOT direct fetch mocks (a `vi.spyOn(globalThis, 'fetch')` that calls through only records the init; MSW still answers)
- Fixture factories for test data (`mockUser`, `mockServer`)
- Prefer `getByRole`, `getByText` over `getByTestId` (accessibility-first)

### The `*Once` queue outlives the test that queued it

`vi.clearAllMocks()` clears call history. It does **not** drain the queue that
`mockResolvedValueOnce` / `mockRejectedValueOnce` / `mockReturnValueOnce` /
`mockImplementationOnce` push onto. `clearMocks` is pinned `false` in
`vite.config.ts` — the flag breaks 31 import-time tests, which is a deliberate
pin and not a gap — so nothing drains those queues globally either.

A value a test queues and never consumes is therefore handed to the **next**
test's first call on that mock. The leaked value sits at the *head* of the
queue, ahead of whatever the consuming test queues for itself, so that test
reads a value it did not write and its own fixture is never touched. Change
that fixture to a wrong value and the test still passes. That is the vacuity.

**Queuing a value you never consume is usually the point, not a slip.** The
tests that leak are the ones proving a branch does *not* do something: an
optimistic send that skips the decrypt, a debounce that swallows the fetch, a
dev-mode short-circuit that never asks the OS. Each deliberately queues a value
to prove it is not read — and that value then leaks forward.

**The signature is: passes alone, fails in suite.** The worse case is the one
that passes both ways while pinning nothing, which no run of any colour reveals.

**The fix** is `mockReset()` in `beforeEach` — the only call that drains the
queue — plus an explicit default, because `mockReset()` also clears the
implementation:

```ts
beforeEach(() => {
  vi.clearAllMocks();
  mockDecryptForChannel.mockReset();
  mockDecryptForChannel.mockResolvedValue(null);
});
```

**`vi.fn(impl)` survives `mockReset()`; `vi.fn().mockReturnValue(x)` does not.**
`@vitest/spy` sets `resetToMockImplementation: true` only for an implementation
passed to `vi.fn()` itself, so `mockReset()` restores that one. A default
attached afterwards is wiped and has to be re-established in the `beforeEach`.
Declaring module-scope mocks as `vi.fn(() => x)` removes that second step.

**Do not look for these by reading.** Whether a queued value is consumed is a
runtime fact. `client/desktop/tests/onceQueueLeak.setup.ts` instruments it and
**runs on every test run** — every `*Once` variant reaches
`mock.mockImplementationOnce` by property lookup, so wrapping that one property
catches all of them, and wrapping the queued implementation itself makes
consumption observed rather than inferred. A queued implementation invoked
during a *different* test than the one that queued it fails that test, naming
the queue site, the queuing test and the consuming test. `ONCE_LEAK_GUARD_OFF=1`
disables it; there is no good reason to.

Note the two signals are different claims. "Undrained at end of test" is the
*precondition* and fires on correctly-fixed files too, so the guard does not
report it; only cross-test consumption is the defect.

**The guard can fail open, so it has a self-test.** Its patch works because
`@vitest/spy` dispatches every `*Once` through a *property*; if a future version
inlines that, the patch attaches to nothing and every run stays green with no
detector. `tests/unit/services/onceQueueLeakGuard.test.ts` pins that dispatch,
the `mockClear`-does-not-drain / `mockReset`-does asymmetry, and the
`vi.fn(impl)`-survives-reset rule, so a bump that invalidates any of them goes
red rather than quietly removing the protection.

**A green suite is not evidence either way, and this is the part to internalise.**
The suite was green BEFORE the drains landed, which constrains every crossing to
one of two cases: the consuming assertion never read the value, or the leaked
value happened to equal the intended one. There is no third case — a
value-dependent consumer handed something different would already have been red.
So a green run after the fix is what you get whether or not a vacuity exists. It
is a non-regression check. **What the drain actually earns is that each fixture
becomes live**: mutating it now turns the test red, where before the mutation was
a no-op. Only a post-fix mutation of the specific fixture settles the question.

Founding audit: 2026-09-17, after PR
#2364 hit it
in `ConversationList.test.tsx`. Of 137 files using a `*Once`, 101 also call
`clearAllMocks` and 5 had a later test consume the leaked value. (A sixth figure
— 15 files leaving a queue undrained at a boundary — came from a one-time
instrumented run and cannot be reproduced by the shipped guard, which reports
crossings only.) **Two** tests were proven vacuous by mutation, both in
`permissionManager.test.ts` and both fed by the same leaked `'denied'`: "falls
through to macOS check in packaged mode" asserted `'denied'` while its own
fixture said `'authorized'` and still passed, and "returns all permission types"
read `'denied'` for notifications while its fixture said `'authorized'` —
surviving only because it asserted `toHaveProperty` and never a value.

**The prior art was much larger than it looked, which is the real argument for
the rule.** `useMessageFetch.test.ts` carried a hand-rolled local drain naming
this exact mechanism, and `PrivacySecuritySection.test.tsx` carried **46** more
as a chained `mockApiFetch.mockReset().mockResolvedValueOnce(…)` in individual
tests — authors draining a queue their own `beforeEach` had filled, one test at a
time, for as long as the file has existed. The mechanism was understood in
practice and never written down, so every new test re-derived it or leaked.

### A new unconditional call in `main.ts` owes every mock of its module

A `vi.mock` factory that omits an export does not fail on import — it throws on
ACCESS, at the call site that reaches the missing name. When `main.ts` gains an
unconditional call in its `app.whenReady()` body, every suite that re-imports
`main.ts` with its own factory for that module (e.g. `tests/unit/main/main.test.ts`
and `tests/unit/main/quitLifecycle.deadlock.test.ts` for `audiocapHost`) needs the
new export, or the `whenReady` body throws at that line. What breaks depends on
WHERE the call sits: after `createWindow` it surfaces only as an unhandled rejection
beside an all-green count; before it, the window is never built and unrelated
assertions (zero `close` listeners) fail. Grep before adding the call:
`grep -rlE "vi\.mock\(['\"][./]*.*main/<module>['\"]" client/desktop/tests`.

Founding pair: `setAudiocapPortSink` (#3198 PR 3, green count plus unhandled error)
and `setAudiocapInterruptListener` (#3394 PR 2, five quit-lifecycle failures once
the call moved above `registerAudiocapIpc`).

### Async assertions — wait on the thing you are asserting about

**Gate on the DOM, not on the spy.** `waitFor(() => expect(spy).toHaveBeenCalled())`
resolves when the mock is *invoked*, which happens at the top of the async function
under test. Whatever its continuation renders after an `await` has not flushed yet,
and React schedules those updates through its Scheduler as a **macrotask** — under
this project's Vitest jsdom environment `scheduler@0.27.0` selects `setImmediate`,
falling back to `MessageChannel` only where `setImmediate` is absent — so under
full-suite load the flush can land after a synchronous assertion has already
sampled an empty DOM. In isolation it passes every time.
Wrap the DOM assertion itself:

```ts
// Wrong — proves the save started, not that it finished
await waitFor(() => expect(save).toHaveBeenCalledWith([id]));
expect(screen.getByRole('status')).toHaveTextContent('Removed exception for X.');

// Right
await waitFor(() =>
  expect(screen.getByRole('status')).toHaveTextContent('Removed exception for X.')
);
```

The tell is a **gate/assert mismatch**: `waitFor` on a spy or a store, followed by
a bare `expect` on the DOM. Note that a store assertion sitting right beside a
racing DOM one can pass reliably — a mock that mutates the store *synchronously*
is flushed inside `fireEvent`'s own `act()` wrapper. Only state written after an
`await` is exposed, so "the assertions around it are fine" is not evidence.

**A `findBy*` query gates only on EXISTENCE.** Anything a post-render effect does
*to* the node it returns — moving focus, scrolling it into view, setting a measured
attribute — lands in a later flush, so the query resolving is not a gate for it.
This is a distinct shape from the flush race above: the DOM *has* flushed and the
query legitimately matched; what has not run is the **effect**. Wrap those
assertions too, and re-query inside the callback rather than closing over the node
the `findBy*` returned:

```ts
// Wrong — findByRole waited for the alert to exist, not for focus to land on it
const alert = await screen.findByRole('alert');
expect(alert).toHaveFocus();

// Right
await screen.findByRole('alert');
await waitFor(() => expect(screen.getByRole('alert')).toHaveFocus());
```

The mechanism is structural, not incidental: RTL's `asyncWrapper` calls
`setReactActEnvironment(false)` for the duration of every `findBy*`/`waitFor`, so
there is no act-exit flush of passive effects. Resolution then depends on the
ordering between React's Scheduler macrotask and RTL's own `setTimeout(0)` — a
race with no guaranteed winner.

Second incident: PR
#2817 — a bare
`toHaveFocus()` after `findByRole` reddened `main`; focus is applied by a
`useEffect` keyed on the error state, and the identical pattern sat latent in a
sibling screen's test.

**Never wrap a negative assertion in `waitFor`.** It retries until its callback
stops throwing, so an emptiness or absence check passes on the *first* poll whether
or not the state ever settled — the check silently becomes vacuous. Keep
`toBeEmptyDOMElement()`, `not.toBeInTheDocument()`, and `not.toHaveBeenCalled()`
synchronous.

Leaving one bare is only *sound*, though, when something before it has already
gated on the state under test — one of: a synchronous handler that `fireEvent`
flushed inside its own `act()`; an enclosing `await act(...)` that drained the
continuation; or a preceding **positive** assertion that waited on the
post-`await` write. A bare emptiness check placed directly after
`await waitFor(() => expect(spy).toHaveBeenCalled())` is NOT sound: that gate
establishes nothing about the DOM, so the check proves nothing. Supply the
missing gate — do not reach for `waitFor` on the negative itself, and do not
"finish the job" by wrapping the remaining bare assertions in a file whose
positive ones were just fixed.

Founding incident: PR
#2806 — one
live-region assertion failed during a full-suite run while passing 19/19 in
isolation; four more sites in the same file carried the identical latent race.

### MSW 3 answers at the socket, not at `fetch`

Since msw 3 (`@mswjs/interceptors` 0.45), `setupServer` lets the real undici `fetch`
run and intercepts the HTTP request it writes. Four consequences:

- **A handler's `request` is rebuilt from the wire.** Its `signal`, `cache`,
  `credentials` and `mode` are defaults, not what the caller passed. Assert those
  where they are set, on the `RequestInit` the component handed to `fetch`:
  `vi.spyOn(globalThis, 'fetch')` after `server.listen()` calls through to MSW and
  records the init. That is a seam, not a fetch mock; MSW still answers.
  URL, method, headers and body are on the wire and stay assertable in the handler.
  A client abort does reach the handler's `signal` once the socket closes, but
  late, so `aborted === false` on it proves nothing; read the caller's.
- **A request reaches MSW several event-loop turns after `fetch()`.** `await act(…)`
  no longer covers a round-trip, and an element that renders before the answer
  (a default-state switch, a stop button, the first of two cards) is not a gate.
  Gate on the DOM change the answer causes. A test that ends with a request in
  flight hands it to the next test's handlers, so a "nothing happened" test needs
  an explicit settle point (a positive control in the same render, or awaiting
  the deduped in-flight promise, or waiting until the handler has answered as many
  requests as the `fetch` spy recorded). A "request was not sent" assertion
  counts at the caller seam (the `fetch` spy, or a spy on the API function),
  not only in the handler, which may not have run yet.
- **`HttpResponse.error()` rejects the real fetch.** The message is undici's
  `fetch failed`, not the renderer's `Failed to fetch`.
- **A real fetch under `vi.useFakeTimers()` schedules undici's socket timers on
  the fake clock.** A kept-alive socket then hangs the next request made on it
  once that clock is advanced, jumped or uninstalled, so `tests/setup.ts` turns
  connection reuse off (`pipelining: 0`), replacing the Agent in both undici
  dispatcher slots so Node 24 (undici 7) and Node 26 (undici 8) are covered. Keep it. `vi.getTimerCount()` still
  counts undici's timers, so a test about its own timers tracks its handles.

Founding migration: PR
#3809 — 114
failures in fourteen test files, fixed there and in the shared `tests/setup.ts`;
none a production defect.

### `server.listen` takes `onUnhandledFrame`; the old name is ignored without a sound

msw 3 renamed `onUnhandledRequest` to `onUnhandledFrame`, because the option now
covers any network frame, a WebSocket connection as well as a request. It accepts
`'bypass' | 'warn' | 'error'`, or a callback
`({ frame, defaults: { warn, error } }) => void | Promise<void>`, and defaults to
`'warn'`. The old name is not an error: test files are not type-checked by the
pre-commit or CI typecheck, so `server.listen({ onUnhandledRequest: 'error' })` compiled, ran
and let every unmocked request through. 106 files carried it until the rename.

- **Write `onUnhandledFrame: 'error'` in a new file.** An unmocked request then rejects
  with `fetch failed` (cause: `Cannot bypass a request when using the "error" strategy`),
  and the test fails at the call that has no handler. Fix that failure by adding the
  handler. `'warn'` and `'bypass'` are not the fix: a request they let through reaches the
  real network, where a dev server on `:8080` answers it, so the test passes on the
  developer machine and fails in CI.
- **`'bypass'` is a choice a file makes on purpose** (about 80 files still do), for a
  test that deliberately lets some traffic through. Keep it as written; do not copy it
  into a file that has not decided that.
- **The mistake is a one-line grep.** Anything this finds is inert, and it should print
  nothing:

  ```bash
  grep -rl onUnhandledRequest client/desktop/tests client/desktop/src
  ```

- **To prove a rename took effect**, add a request with no handler to one file and show
  it fails under the new name and passes under the old one. The request needs a real
  listener behind it (a `node:http` server on port `0`), because a connection refused
  fails under both names and proves nothing.

### StrictMode effect replay needs `<StrictMode>` as the topmost rendered element

`main.tsx` renders the app under `<React.StrictMode>`, so in the dev renderer React
replays every effect on mount — setup, cleanup, setup again — without touching the
DOM. A test that wants that replay must hand a bare `@testing-library/react` `render`
an element whose TOPMOST node is `<StrictMode>`:

```ts
import { render as bareRender } from '@testing-library/react';
bareRender(<StrictMode><BrowserRouter><ModalProvider><Page /></ModalProvider></BrowserRouter></StrictMode>);
```

`tests/test-utils`'s `render` puts its providers OUTSIDE the element you pass, and RTL's
`wrapper` option does the same, so a `<StrictMode>` nested inside either one replays
nothing: React's dev double-invoke pass treats the first placed fiber of a commit as a
unit and only replays it when StrictMode is that fiber or an ancestor of it. Components
still render twice either way, which is why a nested StrictMode looks like it works.

**The suite stays green while reproducing nothing.** Measured 2026-10-06 (#2365) with a
probe component: topmost StrictMode logged setup → cleanup → setup, nested StrictMode
logged setup only. The defect it hid was a cleanup that cleared a store on unmount and
therefore also cleared it during the replay (`SettingsPage.tsx`'s focus-request clear),
which `SettingsPage.focus-request-strict-mode.test.tsx` pins. `MessageList.test.tsx`'s
StrictMode case had already found the same trap and recorded it in a comment; this is
the second occurrence.

### A test under `client/` reads only what the public mirror publishes

`scripts/public-mirror/filter-manifest.txt` publishes `client/**` but only a short list of
other paths: a few `docs/` and `infrastructure/` files, one `scripts/` file, and no
`.github/workflows/*` (the mirror gets its own workflows from the overlay). The public CI
runs the mirrored tests, so a test that reads an unpublished file at collection fails with
ENOENT there and loses EVERY test in its file, while the private suite stays green. The
manifest already records the first occurrence (the #2022 parity test, fixed by allowing the
one file it read); the packaged-app wiring tests in `verify-packaged-app.test.ts` (#3821)
were the second.

Pick one: read a published path; add a file-level `allow` to the manifest for the single
file the test needs; or skip behind a marker only the mirror has (the overlay's
`public-ci-frontend.yml`) and pair the skip with a canary asserting it cannot apply where
the private workflows exist. `verify-packaged-app.test.ts` takes the third route because the
files it reads are the signing-pipeline workflows. To reproduce, run the test in a tree whose
`.github/workflows` holds only the overlay file: the unguarded version collects no tests.

## Schema-validated handler tests

WebSocket handlers in `client/desktop/src/renderer/hooks/messaging/useWebSocketMessages.ts` operate on payloads already narrowed by `WebSocketEventSchema` (zod 4 discriminated union, see `.claude/rules/frontend.md` § WebSocket payload validation). Consequence for the test suite:

- **DO NOT** test in-handler bail paths for fields the schema guarantees (e.g., "ignores message when `channel_id` missing"). After PR #1184 these branches are structurally unreachable — Istanbul will report them as uncovered forever, and they encode behavior that can never fire in production.
- **DO** test the schema-rejection boundary itself once, in the flat dedicated suite (`client/desktop/tests/unit/services/websocketService.dispatch.test.ts`), which exercises production `client/desktop/src/renderer/services/messaging/websocketService.ts`. Cover: malformed envelope, unknown event type, missing required field per schema. Assert `wireViolationCount` increments and the handler is NOT invoked.
- **DO** test the in-handler `.optional()` field branches that the schema legitimately permits (e.g., `VoiceStateUpdateSchema.username` may be undefined; a handler that supplies a fallback `'Unknown'` should have a test asserting the fallback fires).

When removing a defensive in-handler check that the schema now guarantees, leave a removal comment matching the pattern in `useWebSocketMessages.coverage.test.ts` so future readers don't reintroduce the check:

```ts
// Removed in #709: `<event> handler — edge cases` tested in-handler bail
// when <field> was missing. Now structurally guaranteed by zod at
// dispatch boundary; equivalent coverage in websocketService.dispatch.test.ts.
```

## Desktop release trust tests (#2274)

Changes to `build-desktop.yml`, `main-ci.yml`, the desktop release trust
checker, or its policy inputs MUST run both the live-tree check and its
hermetic mutation suite:

```bash
bash .github/scripts/lint-pr-release-trust.sh .
bash .github/scripts/tests/lint-pr-release-trust.test.sh
```

Changes to `infrastructure/github/secrets-hygiene.json`,
`secrets-hygiene.sh`, `gh-stub.sh`, or `secrets-hygiene.test.sh` MUST run:

```bash
bash infrastructure/github/secrets-hygiene.test.sh
```

Test secret-hygiene behavior only through this suite: it supplies the stateful
`gh` stub and synthetic values. Never substitute an authenticated GitHub CLI,
network call, or live secret/environment mutation for this harness.

## Coverage
- Coverage provider: Istanbul (not v8) — avoids OOM issues
- SonarQube enforces ≥ 80% on new code
- Every new source file needs a corresponding test file
- Every new function: at least one happy-path and one error-path test

## Drag-and-drop tests under jsdom (post-#2359)

jsdom implements neither `DragEvent` nor `DataTransfer`. Testing Library's
`createEvent` resolves `window[EventType] || window.Event`, so
`fireEvent.dragOver(el, { clientY })` silently constructs a plain `Event` and
**drops `clientY`**.

The failure mode is a **passing** test, which is why it survives review. A drop
handler then receives `undefined`, any midpoint comparison against `NaN` is
false, and the insertion index collapses to end-of-list — which is frequently
the same answer a genuine off-by-one produces. The test is green against both
the correct and the broken implementation.

This compounds with the other jsdom gap: `getBoundingClientRect` returns
zero-height rects, so injected geometry is required regardless.

When testing a drag surface:

- Build the event as a `MouseEvent` with the native drag type name (React
  dispatches on the native type, so `onDragOver` / `onDrop` still fire) and
  `Object.defineProperty` a fake `dataTransfer` onto it.
- Inject row geometry, computed from each row's **current** DOM index so the
  stub survives a reorder. Never read geometry from jsdom.
- Keep the index decision in a pure function and test it against synthetic
  rects. Assert no geometry through the DOM.
- Prove the suite discriminates: state what the naive mutant returns for each
  case, and confirm the ones that should go red actually do.
