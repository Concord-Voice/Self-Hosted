# Control-plane on-box blue-green with nginx active-colour flip — design

**Issue:** #253 · **Date:** 2026-11-11 · **Status:** approved for planning

`ctl` means `infrastructure/deploy/concord-ctl.sh`. Line numbers were read at `9e36545f` and are leads, not anchors: find each site by symbol before editing.

**Amended 2026-11-11 during execution:** green is a `compose run` one-off container of the `control-plane` service, not a second service in a new compose overlay. An overlay could not inherit the production hardening layer without restating the whole service, and both migration guards already see a one-off. Section 4.1 has the measurements. The four rulings in section 2 are unchanged.

This design came from a three-seat panel (architect philosophy, system-engineer approach, architect reconciliation). Nothing in it has been executed yet. Section 9 lists what is still unverified.

## 1. Intended outcome

A deploy that rebuilds only the control-plane, on a schema that is already current, stops producing 502s and unplanned WebSocket closes. It does this by starting a second control-plane beside the first, moving nginx to it, and moving back, with the old instance stopped the moment each move succeeds.

The rail ships **default-off**. With the switch unset, `rebuild`, `nginx-reload` and `wait-healthy` behave exactly as they do today.

## 2. Rulings (repo admin, 2026-11-11)

| # | Ruling | Effect on the design |
|---|---|---|
| R1 | **Option B.** Ship before WS state externalization with a bounded split. Rail is default-off. | The stop signal follows each successful reload at once. Activation is out of scope and gated on Concord-Voice/Desktop#68 and Concord-Voice/Desktop#64. |
| R2 | **Migration-gated flip.** | Overlap only on a schema proven current. Any colour started beside a serving colour refuses to migrate. A deploy that carries migrations, or any doubt, takes today's stop-first drained path. |
| R3 | **Round-trip colour model.** | Blue is home and keeps the service name `control-plane` on 8080. Green is transient. Two reconnect waves per overlapped deploy are accepted. |
| R4 | **Smallest design that works, one PR.** | No new state store. Reuse the existing lock, the `nginx-reload` transaction and the media rail's render helper. |

## 3. Why blue stays home

The deploy script assumes one control-plane in many places, and the flip is the easy part:

- Both migration guards find control planes by the exact Compose service label `control-plane` (`activity_history_refresh_daemon_ids`, and the own-set in `privacy_defaults_refuse_foreign_control_plane`). A colour under any other name is invisible to them, so the drained check would pass while that colour still serves.
- About 100 sites in `ctl` address the literal service `control-plane`. The media rail reads `services["control-plane"]`. `media-plane` has `depends_on: control-plane`.
- Five health probes hard-code `127.0.0.1:8080` outside the `CP_PROBE_BASE` seam.
- The admin-console health reader allowlists one container name.

Three options were weighed:

| Option | Verdict |
|---|---|
| Persistent alternating colours (whichever won the last deploy stays active) | Rejected. "Green is the long-lived active" becomes a steady state that every verb, probe and guard must handle, with no existing test net. |
| **Blue is home, green is transient (round trip)** | **Chosen.** Every new state exists only inside one locked function and its recovery. Cost: two reconnect waves and a longer deploy. |
| Symmetric rename to `control-plane-blue` / `control-plane-green` | Rejected. Both guards go blind and the default path cannot stay unchanged. |

## 4. Design

### 4.1 Naming, switch and guard visibility

- Home: service `control-plane`, container `concordvoice-control-plane`, host port 8080. Unchanged.
- **Green is a one-off container of the same service**, not a second service. The cutover starts it with
  `compose run --detach --no-deps --pull never --use-aliases --name concordvoice-control-plane-green -p 127.0.0.1:8081:8080 -e MIGRATIONS_MODE=verify -e OPS_METRICS_ENABLED=false control-plane`,
  the same mechanism `control_plane_cli` already uses for maintenance containers. There is no overlay file and no compose file is edited.
  - It inherits every compose layer of `control-plane` by construction (hardening, volumes, env, healthcheck, 45 s stop grace), including layers a deployment adds on top. Nothing has to be mirrored and nothing can drift.
  - Only the `-p` port is published; the service's own `8080:8080` publish is not applied to a one-off.
  - `OPS_METRICS_ENABLED=false` keeps green from opening or closing the single ops-metrics reader role that home owns.
  - Its restart policy is `no`. If green dies while it is the only serving colour, the next `rebuild` recovers. Documented, not engineered away.
- **Why this cannot fail open** (measured on Docker Compose v5.7.0 and pinned by a contract test):
  - a one-off carries the label `com.docker.compose.service=control-plane`, so the daemon-wide drained check (`activity_history_refresh_daemon_ids`) already counts it;
  - `compose ps --status running -q -- control-plane` lists it, so the foreign-project guard already treats it as this project's own;
  - `compose stop control-plane` stops home and leaves the one-off running; `compose up --no-deps control-plane` starts home beside it.
  - **Neither guard is edited.** The cutover also checks at run time that green's ID is in the `compose ps` list, and takes the drained path if it is not.
- **Switch:** `CONTROL_PLANE_DEPLOY_PROFILE` in the deploy env file, following the `MEDIA_DEPLOY_PROFILE` precedent. Values: `control-plane-single` (default) and `control-plane-bluegreen`. It only enables the rail; the compose command string is the same in both modes.
  - Honoured only in the production/staging branch of the compose-selection logic. Forced to single in the self-host branch and the other branches, silently, as the media switch is.
  - Exit 2 for an invalid value, and for `control-plane-bluegreen` combined with any of: `MEDIA_DEPLOY_PROFILE=media-bluegreen`, no `flock`, or no deploy state directory.
- **Marker.** A cutover-in-flight marker file (precedent: `REDIS_MANAGED_REBUILD_MARKER`) drives recovery. Marker present with the rail off refuses every locked verb with `rail-off-with-cutover-marker`. This is the single delta on the default path: one file-existence test, no docker call, no output change.

### 4.2 nginx selection and state

- **Render (rail on only).** Inside `render_nginx_config`, in place:
  - the exact line `upstream control_plane {` opens the block;
  - a line of the form `    server 127.0.0.1:<digits>;` is rewritten to the requested port;
  - any other `server` line inside the block marks the render bad; `}` closes the block.
  - Invariant: `owner_markers == 1 && scoped_replacements == 3 && cp_blocks == 1 && cp_servers == 1`, no bad line, block closed, port is 8080 or 8081.
  - With the rail off this code is skipped. `concordvoice.conf` is not edited and needs no marker comment, so the rail-off render is byte-identical to today's.
- **State** is the live rendered vhost. A new top-level function `control_plane_bluegreen_live_port` parses it the same way: a missing file means 8080, one canonical line with 8080 or 8081 gives that port, anything else returns 1. It is never called with the rail off.
- **Carry-forward.** With the rail on, an operator `nginx-reload` (argv unchanged) renders the live port, and refuses with `live-upstream-unparseable` if it cannot read one. Certificate and HSTS reloads therefore keep the active colour and need no Redis.
- **Flip.** Only the cutover requests a different port, through an internal form of `nginx-reload` carrying `--control-plane-upstream 8080|8081`. It is honoured only with a validated lock re-entry, the rail on, and the cutover marker present. Otherwise the caller gets today's usage error.
- `nginx -t`, the reload, the HSTS probe and the exact rollback are reused unchanged.

### 4.3 Internal callers

- The six vhost `proxy_pass` sites and the admin-console template share the upstream by name and follow the flip.
- `media-plane` keeps `CONTROL_PLANE_URL=http://control-plane:8080`. Green carries the network alias `control-plane` through `--use-aliases`. During overlap Docker DNS answers with both, so some calls reach a booting or stopping colour and are refused. Voice authorization fails closed. This is less than today's full outage and is accepted.
- The five fixed 8080 probes stay as they are. They are correct in every steady state, and the verbs that reach them refuse while a marker exists.
- `wait-healthy`: the single `CP_PROBE_BASE` assignment uses the live port when the rail is on, and exits 1 if it is unparseable.
- `activity_history_wait_control_plane` gains an optional **full URL** argument that defaults to today's literal. The cutover passes the `/readyz` URL of each colour. (A base URL would gate the flip on `/health`, which is liveness only.)

### 4.4 Port posture, managed Redis and managed egress

- **Self-host contract mode:** the switch is forced off, so no green container is ever created there. The port-posture gate reads the compose model, which never contains green, and is not edited.
- **Managed Redis:** an ordinary `rebuild` exits before the cutover hook in that mode, so single-container admission and its ACK slots are untouched. A test pins that no green call is made.
- **Managed egress:** in non-direct egress mode a new control-plane container needs the egress reconcile that `control_plane_cli` runs after it creates one. The cutover runs the same reconcile after starting green and after reaping it.

### 4.5 Pending-migration check and verify mode

- **Subcommand** `./main migrations status`, routed through the existing subcommand dispatch in `cmd/server/main.go` before config load.

  | Stdout (one line) | Exit |
  |---|---|
  | `current <n>` | 0 |
  | `pending <db> <latest>`, `ahead <db> <latest>`, `dirty <db>`, `uninitialised <latest>` | 3 |
  | none; a fixed stderr message, never the DSN or a driver error | 1 |
  | usage error | 64 |

  Total timeout 15 s.
- **The read path never builds the migrate driver**, because building it takes the advisory lock and can create the version table. It opens `BEGIN READ ONLY`, sets `statement_timeout` to 5 s, checks the version table with `to_regclass`, then reads `version, dirty`. The latest embedded version comes from the `file://migrations` source walked with `First`/`Next`.
- **Go surface:** `migrationrunner.ReadStatus(ctx, db, src) (Status, error)`, `Status.Current()`, `migrationrunner.Verify(ctx, db, sourceURL)`; thin wrappers `database.MigrationStatus` and `database.VerifyMigrations`.
- **`MIGRATIONS_MODE`**, parsed before the database is opened. Only green ever receives it, through `-e` on its `compose run`; home's configuration is never changed:
  - empty or `apply`: today's `RunMigrations`;
  - `verify`: startup fails unless the schema is current, and nothing is written;
  - any other value: startup fails before any database connection.
- **In `ctl`:** `control_plane_cli migrations status`, with `migrations` added to the condition that forces a fresh container of the built image. Overlap requires exit 0 **and** stdout matching `^current [0-9]+$`. Everything else takes the drained path.
- **Old images.** An image from before this change exits 64 and takes the drained path. A Go test pins "subcommand present implies verify mode is honoured", so a current status from an image is also proof that the image supports `verify`.

### 4.6 Cutover, lock, rollback and recovery

**Lock.** The deploy transaction lock that `rebuild` already holds.

**Nested flip.** The cutover re-invokes `ctl nginx-reload` while holding that lock. A new argv re-entry marker, beside the existing one, lets the child skip re-acquiring it. The child validates before dispatch:

1. its inherited fd 9 is the lock file;
2. a fresh non-blocking lock attempt on the path fails (someone holds it);
3. a non-blocking lock on fd 9 succeeds (the holder is this process tree);
4. the argv is exactly the flip form.

**Hook.** In `rebuild`, between the image build and the "draining the previous control-plane" step. It runs only when the rail is on and the scope is exactly `control-plane`. The existing five-line drained sequence stays in place, in order, as the fallback.

**Sequence.** `control_plane_bluegreen_cutover` returns "done", or "take the drained path, nothing was changed", or exits 1 with a named error.

1. Read the live port. Unparseable → drained path (today's `rebuild` never depended on the vhost). A live port of 8081 never reaches here; recovery handles it first.
2. Status check (4.5). Not provably current → drained path.
3. `privacy_defaults_refuse_foreign_control_plane`.
4. Resolve the built image ID. Write the marker atomically (mode 0600, no content). Either fails → drained path, nothing started.
5. Remove a leftover `concordvoice-control-plane-green` (nginx is on 8080, so it is not serving). Start green with the `compose run` form in 4.1.
   - Start failed: reap, clear the marker, `standby-not-ready`. Home keeps serving. A new image that cannot boot is caught here, before any user is moved.
   - Its ID is not in `compose ps --status running -q -- control-plane`: reap, clear the marker, drained path.
   - Its image ID differs from the built one: reap, clear the marker, `standby-image-mismatch`.
6. Gate on green's `/readyz`. On failure: reap green, clear the marker, `standby-not-ready`. No flip is attempted and home keeps serving.
7. Ignore HUP, INT and TERM. Flip to 8081. On success stop home with nothing in between, then restore the traps.
   - Flip failed: nginx restored itself; reap green, clear the marker, `flip-failed`.
   - Stop failed: `old-colour-stop-failed`, marker kept.
8. Repeat the status check. If it is still current, start home with the unchanged drained-path `up` line: on a schema proven current its startup migration run changes nothing. Gate on its `/readyz`. On a non-current status or any failure: `home-not-ready`; green keeps serving and the marker stays. Home's image is not checked (today's path does not check it either).
9. Ignore signals. Flip to 8080. On success stop green at once (`docker stop -t 45`) and remove it, restore the traps, clear the marker. On flip failure: `flip-failed`, marker kept. On a failed reap: `old-colour-stop-failed`, marker kept.
10. The rest of `rebuild` runs unchanged.

**Bounded split.** Each leg overlaps from a successful reload until the old colour exits: at most the 45 s stop grace.

**Recovery** has two parts, so that no container is ever started ahead of `activity_history_routine_guard` (the first-activation boundary).

*Pre-step*, in `rebuild`, immediately before that guard, rail on only. It triggers when the marker exists **or** the live port reads 8081 (so a lost marker cannot leave a deploy stuck). Otherwise it does nothing: one file test and one file read.

| Live port | Action |
|---|---|
| unparseable (marker present) | `recovery-ambiguous`; nothing changed |
| 8080 | Reap green, clear the marker, continue. The rebuild may then run a fresh cutover. |
| 8081 | The effective scope must be exactly `control-plane`, else `cutover-incomplete` and nothing changed. Write the marker if it is missing. Stop home if it is running. Continue with the marker in place: exactly one control-plane container (green) or none is left for the guard to count. |

*Resume*, at the hook, when the marker is present. It replaces steps 1-7:

- green answers `/readyz` and the status is current → steps 8-9 with the newly built image;
- otherwise → the foreign-project guard, reap green, the daemon-wide drained check, start home in the ordinary apply mode, gate 8080, flip to 8080, clear the marker. This is the drained path with a flip at the end.

Because the resume runs after the build, a bad image recorded by an interrupted run can never block the deploy that fixes it.

- While a marker exists, other locked verbs refuse with `cutover-incomplete`. Exceptions: operator `nginx-reload`; `stop`, which also reaps green; and `sudoers-sync`, which touches neither containers nor nginx and runs in a deploy pipeline before the `rebuild` that recovers.
- A SIGKILL between a flip and its stop leaves both colours serving until the next `rebuild`. This is documented, not engineered away.

**Errors.** Named errors are prefixed `control-plane-bluegreen:` and exit 1: `standby-image-mismatch`, `standby-not-ready`, `flip-failed`, `old-colour-stop-failed`, `home-not-ready`, `cutover-incomplete`, `recovery-ambiguous`, `live-upstream-unparseable`, `rail-off-with-cutover-marker`. Mode refusals are `rail-refused-<reason>` and exit 2.

## 5. Test strategy

There is no existing safety net: no test names `render_nginx_config`, `wait-healthy`, the `privacy_defaults_` guards or the media rail. So characterization comes first.

**T0 — characterization, first commit, test file only.** Follows the existing harness shape in `scripts/tests/test-concord-selfhost.sh` (real `ctl`, one rewritten lock declaration, PATH stubs). It pins:

- the docker/compose argv transcript of `rebuild control-plane` with the rail unset;
- the `wait-healthy` curl argv;
- a sha256 of the shipped vhost's render.

It must pass on the unmodified tree and on every later commit.

**Hermetic cases** (`infrastructure/deploy/tests/test-control-plane-bluegreen.sh`). Each names the mutation that turns it red, per `.claude/rules/domains/tests.md`.

| Area | Cases |
|---|---|
| Renderer | 0 or 2 blocks, 2 server lines, `backup`, non-loopback host, tab indentation: each rejects, removes the temp file and leaves the live file alone. Rail-on render at 8080 equals the T0 hash. |
| Reader | missing → 8080; 8081; 9999 and garbage refuse; render then read round-trips |
| Status table | `current 12` overlaps; bare `current`, extra lines, exit 3 / 64 / 1 and timeout go drained, with the five drained calls in order |
| Gate | The cutover curls `/readyz`, not `/health` |
| Order | On both legs the next mutating call after the reload is the old colour's stop; an INT during a slow reload still issues it |
| Failures | Every named error and the state it leaves |
| Recovery | One test per table row, including resume with a pending migration |
| Guards | Neither guard function differs from the T0 baseline; foreign project refused in both paths; with green listed by `compose ps` the foreign guard passes; green missing from `compose ps` → reaped, drained path |
| Re-entry | No fd 9; fd 9 on another file; lock not held; unrelated holder; genuine with wrong argv; genuine. Uses real `flock` and hard-fails in CI. |
| Modes | Self-host forced off; managed Redis makes no green call; non-direct egress runs the reconcile after green starts and after it is reaped; `media-bluegreen` exits 2; marker with rail off refuses |
| Compose contract | One case in the same file that calls the real `docker` (not the stub), against a real daemon and a throwaway two-file project: a `compose run` one-off carries the service label and `oneoff=True`; `compose ps --status running -q -- <svc>` lists it; `compose stop <svc>` leaves it running; `--use-aliases` gives it the service alias; only the `-p` port is published; a key set in the second file (`read_only`) is inherited. Skips without a daemon locally; hard-fails under CI. |

**Real nginx** (`test-control-plane-bluegreen-nginx.sh`, with stub upstream `control_plane_bluegreen_upstream.py`):

- The upstream block comes from the real renderer; the WebSocket location is extracted from the shipped vhost.
- Two stub upstreams on 8080 and 8081 copy Go `Shutdown` semantics.
- Assertions: zero non-200 on `/health` from fresh and keep-alive clients across render, reload and old-stub SIGTERM; a WebSocket opened before the reload still echoes after it; a new WebSocket lands on the new stub.
- Preflights that both ports are free and hard-fails under CI if nginx is missing.

**Go:**

- `status_test.go` against real Postgres: uninitialised, current, pending, ahead, dirty, two rows; the catalog is unchanged afterwards.
- `Verify` errors on every non-current case.
- Subcommand routing and exit codes; mode parsing; verify never calls apply.

**CI reach.** The deploy-test step globs `infrastructure/deploy/tests/test-*.sh`.

**Not covered here.** No real control plane runs under Docker end to end in CI. That, voice surviving a deploy, and drained clients reconnecting on time are activation evidence for a deployment.

## 6. Files

**Protected paths (approved per-path by the repo admin, 2026-11-11):**

1. Edit `infrastructure/deploy/concord-ctl.sh`
2. Create `infrastructure/deploy/tests/test-control-plane-bluegreen.sh`
3. Create `infrastructure/deploy/tests/test-control-plane-bluegreen-nginx.sh`
4. Create `infrastructure/deploy/tests/control_plane_bluegreen_upstream.py`

**Protected, conditional (approved; touch only if the condition holds, and say so in the PR):**

5. `.github/workflows/pr-ci.yml`: only if the CI runner lacks nginx.

`CODEOWNERS` and `infrastructure/deploy/deploy-manifest.txt` were also approved conditionally. Neither is needed now: there is no overlay file.

**Not protected:**

- Edit `services/control-plane/cmd/server/main.go`; create `cmd/server/main_migrations_test.go`
- Create `services/control-plane/internal/migrationrunner/status.go` and `status_test.go`; edit the package comment in `runner.go`
- Edit `services/control-plane/internal/database/postgres.go`
- Edit `docs/architecture.md`, `docs/development.md`, `CHANGELOG.md`
- Edit `.claude/rules/domains/migrations.md`

**Untouched:** every compose file (none is added either), both nginx sources, `selfhost-port-posture.py`, `concord-selfhost`, `voice-boot-containment.py`, `.pre-commit-config.yaml`, `.env.example`, `docker_reader.go`.

`.env.example` is left alone on purpose: the switch lives in the deploy env file, and `MIGRATIONS_MODE` reaches a container only through the cutover's `compose run -e`, so no operator-settable `.env` surface is added.

## 7. Acceptance criteria

| # | Criterion | Proof |
|---|---|---|
| 1 | Rail unset: `rebuild`, `nginx-reload` and `wait-healthy` behave as today; existing deploy tests pass unmodified | T0 transcript and hash |
| 2 | Two colours coexist on distinct loopback ports with no name or port collision | Compose contract test; happy-path transcript |
| 3 | Rail on: the active port is rendered; an ambiguous source is rejected before activation; `nginx -t` and rollback are preserved | Renderer cases |
| 4 | Zero non-200 on `/health` across a full flip | Real-nginx test |
| 5 | A WebSocket opened before the flip survives the reload | Real-nginx test |
| 6 | `wait-healthy` gates on the live colour and fails when it is not ready; the combined unscoped requirement is preserved | Hermetic |
| 7 | Bounded split: stop follows reload at once, an interrupt cannot separate them, overlap is at most 45 s | Order and signal cases |
| 8 | Standby fails `/readyz`: no flip, home serves, green reaped, exit 1 `standby-not-ready` | Failure case |
| 9 | Pending or undeterminable migrations take the drained path with both guards in order; verify mode on a non-current schema fails and writes nothing | Status table; Go tests |
| 10 | A foreign-project control plane is refused in both paths | Guard cases |
| 11 | Every recovery row converges or refuses as specified | Recovery cases |
| 12 | `/readyz` and drain-before-close are re-verified, not rebuilt | `main_drainsettle_test.go` unchanged; gate case |
| 13 | Happy and error paths, each with a named mutation | Review against `.claude/rules/domains/tests.md` |
| 14 | shellcheck-clean; Go lint green | CI |
| 15 | Docs ship in the same PR (list below) | Review |

**Docs to update:**

- `docs/architecture.md`: the colour model and the switch; how to identify the active colour (the `upstream control_plane` block of the live vhost, the marker file, and whether a `concordvoice-control-plane-green` container exists); how to force back (rerun `rebuild control-plane`; `stop` then `rebuild control-plane` as the last resort); the migration gate and the named errors; activation gates and preconditions; env-table rows for `MIGRATIONS_MODE` and `CONTROL_PLANE_DEPLOY_PROFILE`.
- `docs/development.md`: the deploy-verb section.
- `CHANGELOG.md`.
- The comment block above the stop-first sequence in `ctl rebuild`.
- `.claude/rules/domains/migrations.md`: one sentence saying status and verify must never build the migrate driver.

## 8. Decomposition

| # | Task | Ordering |
|---|---|---|
| T0 | Characterization tests | Serial gate; lands first |
| T1 | Go: status, verify mode, subcommand, tests | Parallel |
| T2 | Compose one-off contract test (test only) | Parallel |
| T3 | Renderer, live-port reader, flip argument, lock re-entry | Parallel with T1 and T2; serial with T4 and T5 (same file) |
| T4 | Switch, refusals, marker | After T3 |
| T5 | Cutover, recovery, rebuild hook, `wait-healthy` | Needs T1's exit-code contract, T2, T3, T4 |
| T6 | Real-nginx test | Once T3's contract is fixed |
| T7 | Docs and the `ctl` comment block | After T5 |

## 9. Risks and unverified points

| Risk | Likelihood | Mitigation |
|---|---|---|
| The recovery flow was written at reconciliation and was not reviewed by the panel | Medium | Security and code review of 4.6; one test per recovery row |
| Presence residual: a user with one device on the old colour who connects another device to the new colour before the old colour's shutdown write can end up shown offline and fenced. It fails closed. | Low per user, twice per deploy | Confirm in backend review. If confirmed, file a follow-up and add it as a third activation gate. Not fixed in this PR. |
| Another Compose version treats one-offs differently (`ps` omits them, or `stop` stops them) | Low | Contract test in CI; run-time check that green is in `compose ps`; if `stop` also took green, the sequence still converges to home at step 8 |
| Green has restart policy `no` | Low: green is the sole server only between home's stop and its restart | The next `rebuild` recovers; documented |
| Lock re-entry weakens serialization | Low | Four-part validation; six tests; security review |
| Both voice lifecycle subscribers apply every event during overlap | Unknown | Bounded by the 45 s split; backend review |
| CI runner lacks nginx, or 8080/8081 are busy | Unknown | Hard-fail; conditional `pr-ci.yml` edit |

**Not verified yet (each is a first step of the task that depends on it):**

- Which `ctl` wrapper the `compose run` for green must go through (follow `control_plane_cli`).
- That `control_plane_cli` stdout is clean in non-direct egress mode.
- `media-plane` behaviour on brief control-plane call failures.
- That the host `nginx.conf` leaves `worker_shutdown_timeout` unset. Neither repo vhost file sets it.
- That building the golang-migrate Postgres driver takes the advisory lock and creates the version table in the pinned version.
- `/auth/ws-ticket` allows 10 per user per minute; two waves cost 2 tickets per device.

## 10. Out of scope

- Activating the rail in any deployment, and that deployment's own pipeline, health actor and runbook.
- The `session_drain` frame and close 4001 (Concord-Voice/Desktop#64); reconnect refetch (Concord-Voice/Desktop#68); WS state externalization (#79).
- The media plane, horizontal scaling, cross-host work, and stateful containers.
- A per-migration "overlap-safe" marker.
