# Control-plane on-box blue-green with nginx active-colour flip — design

**Issue:** #253 · **Date:** 2026-11-11 · **Status:** approved for planning

`ctl` means `infrastructure/deploy/concord-ctl.sh`. Line numbers were read at `9e36545f` and are leads, not anchors: find each site by symbol before editing.

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
- Standby: service `control-plane-green`, container `concordvoice-control-plane-green`, published on `127.0.0.1:8081` to container port 8080. Defined only in a new overlay, `docker-compose.control-plane-bluegreen.yml`, behind a Compose profile so nothing starts it by default.
- **Switch:** `CONTROL_PLANE_DEPLOY_PROFILE` in the deploy env file, following the `MEDIA_DEPLOY_PROFILE` precedent. Values: `control-plane-single` (default) and `control-plane-bluegreen`.
  - Honoured only in the production/staging branch of the compose-selection logic. Forced to single in the self-host branch and the other branches, silently, as the media switch is.
  - Exit 2 for an invalid value, and for `control-plane-bluegreen` combined with any of: `MEDIA_DEPLOY_PROFILE=media-bluegreen`, no `flock`, no deploy state directory, or a missing overlay file.
  - When on, the overlay is appended to the compose command. When off, the compose command string is identical to today's.
- **Guards with the rail on:**
  - `activity_history_refresh_daemon_ids` makes a second `docker ps` filtered on the green service label and appends its IDs. If either call fails it returns 1.
  - The own-set in `privacy_defaults_refuse_foreign_control_plane` names both services.
  - The drained check therefore refuses while either colour runs, and a foreign project's control plane is still refused.
- **Guards with the rail off:** untouched. The one hole (rail switched off while a green container still serves) is closed by a cutover-in-flight marker file, following the `REDIS_MANAGED_REBUILD_MARKER` precedent. Marker present with the rail off refuses every locked verb. This is the single delta on the default path: one file-existence test, no docker call, no output change.
- `rebuild control-plane-green` is already rejected by the scope allowlist. A test pins it.

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
- `media-plane` keeps `CONTROL_PLANE_URL=http://control-plane:8080`. Green carries the network alias `control-plane`. During overlap Docker DNS answers with both, so some calls reach a booting or stopping colour and are refused. Voice authorization fails closed. This is less than today's full outage and is accepted.
- The five fixed 8080 probes stay as they are. They are correct in every steady state, and the verbs that reach them refuse while a marker exists.
- `wait-healthy`: the single `CP_PROBE_BASE` assignment uses the live port when the rail is on, and exits 1 if it is unparseable.
- `activity_history_wait_control_plane` gains an optional **full URL** argument that defaults to today's literal. The cutover passes the `/readyz` URL of each colour. (A base URL would gate the flip on `/health`, which is liveness only.)

### 4.4 Port posture and managed Redis

- **Self-host contract mode:** the switch is forced off. `selfhost-port-posture.py` is not edited: it rejects any remapped port and has no branch for the green name. A test pins that a self-host render containing green is refused.
- **Managed mode:** the posture gate does not run. The cutover's model validator enforces exactly one green port: loopback, 8081 to 8080, tcp.
- **Managed Redis:** an ordinary `rebuild` exits before the cutover hook in that mode, so single-container admission and its ACK slots are untouched. A test pins that no green call is made.
- **Deployment-owned compose layers.** A deployment that layers extra compose files changing `control-plane` must mirror them onto `control-plane-green`. Until it does, the validator keeps every deploy on the drained path and prints a warning naming the differing keys, never their values.

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
- **`MIGRATIONS_MODE`**, parsed before the database is opened:
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

**Sequence.**

1. Validate the model from the rendered compose config. Green must equal home except `container_name`, `ports`, the network alias, `MIGRATIONS_MODE=verify`, `OPS_METRICS_ENABLED=false`, `profiles`, no `build`, and `pull_policy`. Failure → drained path.
2. Status check (4.5). Not provably current → drained path.
3. `privacy_defaults_refuse_foreign_control_plane`.
4. Record the built image ID. Write the marker atomically (mode 0600, no content).
5. Reap any stale green. Start green with `--no-build --no-deps --wait --wait-timeout 180`. If its image ID differs: reap, clear the marker, `standby-image-mismatch`.
6. Gate on green's `/readyz`. On failure: reap green, clear the marker, `standby-not-ready`. No flip is attempted and home keeps serving.
7. Ignore HUP, INT and TERM. Flip to 8081. On success stop home with nothing in between, then restore the traps.
   - Flip failed: nginx restored itself; reap green, clear the marker, `flip-failed`.
   - Stop failed: `old-colour-stop-failed`, marker kept.
8. Start home with `MIGRATIONS_MODE=verify`, check its image, gate on its `/readyz`. On failure: `home-not-ready`; green keeps serving and the marker stays.
9. Ignore signals. Flip to 8080. On success stop and remove green at once, restore the traps, clear the marker. On flip failure: `flip-failed`, marker kept.
10. The rest of `rebuild` runs unchanged.

**Bounded split.** Each leg overlaps from a successful reload until the old colour exits: at most the 45 s stop grace.

**Recovery.** Runs when `rebuild` is dispatched with a marker present, after the lock and before `activity_history_routine_guard` (which would otherwise count two containers). Recovery always builds first and then converges to home, so a bad image recorded by an interrupted run can never block the deploy that fixes it.

| Live port | Green | Action |
|---|---|---|
| unparseable | any | `recovery-ambiguous`; nothing changed |
| 8080 | any | Reap green, clear the marker, continue |
| 8081 | `/readyz` OK | Scope must be exactly `control-plane`, else `cutover-incomplete`. Stop home if running, then build. If status is current: steps 8-9 with the new image. Otherwise: guard, stop and remove green, drained check, start home in apply mode, gate 8080, flip to 8080, clear the marker. |
| 8081 | not ready or absent | Reap green, run the drained sequence for home on the current tag, gate 8080, flip to 8080, clear the marker, continue |

- While a marker exists, other locked verbs refuse with `cutover-incomplete`. Exceptions: operator `nginx-reload`, and `stop`, which also reaps green.
- A SIGKILL between a flip and its stop leaves both colours serving until the next `rebuild`. This is documented, not engineered away.

**Home's mode after an overlap.** Home keeps `MIGRATIONS_MODE=verify` in its container config until its next recreate. That is safe: it is the same image on the same schema, and the next `rebuild` recreates it. The docs warn against a hand-run `docker compose up`.

**Errors.** Named errors are prefixed `control-plane-bluegreen:` and exit 1: `standby-image-mismatch`, `standby-not-ready`, `flip-failed`, `old-colour-stop-failed`, `home-not-ready`, `cutover-incomplete`, `recovery-ambiguous`, `live-upstream-unparseable`. Mode refusals are `rail-refused-<reason>` and exit 2.

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
| Guards | Foreign project refused in both paths; green counts as own; rail-off argv unchanged |
| Re-entry | No fd 9; fd 9 on another file; lock not held; unrelated holder; genuine with wrong argv; genuine. Uses real `flock` and hard-fails in CI. |
| Modes | Self-host forced off; posture rejects a rendered green; managed Redis makes no green call; `media-bluegreen` exits 2; marker with rail off refuses |
| Validator | Dropped env key, `0.0.0.0`, or a second port fails; output never contains env values (sentinel secret) |
| Compose contract | Real `docker compose config` parity of base, production and overlay; proof that naming `control-plane-green` works for `up`, `stop`, `rm` and `ps` with only the default profile active |

**Real nginx** (`test-control-plane-bluegreen-nginx.sh`, with stub upstream `control_plane_bluegreen_upstream.py`):

- The upstream block comes from the real renderer; the WebSocket location is extracted from the shipped vhost.
- Two stub upstreams on 8080 and 8081 copy Go `Shutdown` semantics.
- Assertions: zero non-200 on `/health` from fresh and keep-alive clients across render, reload and old-stub SIGTERM; a WebSocket opened before the reload still echoes after it; a new WebSocket lands on the new stub.
- Preflights that both ports are free and hard-fails under CI if nginx is missing.

**Go:**

- `status_test.go` against real Postgres: uninitialised, current, pending, ahead, dirty, two rows; the catalog is unchanged afterwards.
- `Verify` errors on every non-current case.
- Subcommand routing and exit codes; mode parsing; verify never calls apply.

**CI reach.** The deploy-test step globs `infrastructure/deploy/tests/test-*.sh`, and its path filter includes `docker-compose*.yml`, so the parity test also runs on compose-only PRs.

**Not covered here.** No real control plane runs under Docker end to end in CI. That, voice surviving a deploy, and drained clients reconnecting on time are activation evidence for a deployment.

## 6. Files

**Protected paths (approved per-path by the repo admin, 2026-11-11):**

1. Edit `infrastructure/deploy/concord-ctl.sh`
2. Create `infrastructure/deploy/tests/test-control-plane-bluegreen.sh`
3. Create `infrastructure/deploy/tests/test-control-plane-bluegreen-nginx.sh`
4. Create `infrastructure/deploy/tests/control_plane_bluegreen_upstream.py`

**Protected, conditional (approved; touch only if the condition holds, and say so in the PR):**

5. `CODEOWNERS`: one line giving the overlay the same ownership as `docker-compose.production.yml`.
6. `infrastructure/deploy/deploy-manifest.txt`: only if a packaging test needs the overlay listed.
7. `.github/workflows/pr-ci.yml`: only if the CI runner lacks nginx.

**Not protected:**

- Create `docker-compose.control-plane-bluegreen.yml`
- Edit `services/control-plane/cmd/server/main.go` and `main_subcommand_test.go`
- Create `services/control-plane/internal/migrationrunner/status.go` and `status_test.go`; edit the package comment in `runner.go`
- Edit `services/control-plane/internal/database/postgres.go`
- Edit `docs/architecture.md`, `docs/development.md`, `CHANGELOG.md`
- Edit `.claude/rules/domains/migrations.md`

**Untouched:** all six existing compose files, both nginx sources, `selfhost-port-posture.py`, `concord-selfhost`, `voice-boot-containment.py`, `.pre-commit-config.yaml`, `.env.example`, `docker_reader.go`.

`.env.example` is left alone on purpose: the switch lives in the deploy env file, and `MIGRATIONS_MODE` reaches a container only through the overlay, so no operator-settable `.env` surface is added.

## 7. Acceptance criteria

| # | Criterion | Proof |
|---|---|---|
| 1 | Rail unset: `rebuild`, `nginx-reload` and `wait-healthy` behave as today; existing deploy tests pass unmodified | T0 transcript and hash |
| 2 | Two colours coexist on distinct loopback ports with no name or port collision | Compose parity test; validator |
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

- `docs/architecture.md`: the colour model and the switch; how to identify the active colour (the `upstream control_plane` block of the live vhost, and the marker file); how to force back (rerun `rebuild control-plane`; `stop` then `rebuild control-plane` as the last resort); the migration gate and the named errors; activation gates and preconditions; env-table rows for `MIGRATIONS_MODE` and `CONTROL_PLANE_DEPLOY_PROFILE`.
- `docs/development.md`: the deploy-verb section.
- `CHANGELOG.md`.
- The comment block above the stop-first sequence in `ctl rebuild`.
- `.claude/rules/domains/migrations.md`: one sentence saying status and verify must never build the migrate driver.

## 8. Decomposition

| # | Task | Ordering |
|---|---|---|
| T0 | Characterization tests | Serial gate; lands first |
| T1 | Go: status, verify mode, subcommand, tests | Parallel |
| T2 | Overlay, validator, compose parity and contract tests | Parallel |
| T3 | Renderer, live-port reader, flip argument, lock re-entry | Parallel with T1 and T2; serial with T4 and T5 (same file) |
| T4 | Switch, refusals, guard widening, marker | After T3 |
| T5 | Cutover, recovery, rebuild hook, `wait-healthy` | Needs T1's exit-code contract, T2, T3, T4 |
| T6 | Real-nginx test | Once T3's contract is fixed |
| T7 | Docs and the `ctl` comment block | After T5 |

## 9. Risks and unverified points

| Risk | Likelihood | Mitigation |
|---|---|---|
| The recovery flow was written at reconciliation and was not reviewed by the panel | Medium | Security and code review of 4.6; one test per recovery row |
| Presence residual: a user with one device on the old colour who connects another device to the new colour before the old colour's shutdown write can end up shown offline and fenced. It fails closed. | Low per user, twice per deploy | Confirm in backend review. If confirmed, file a follow-up and add it as a third activation gate. Not fixed in this PR. |
| Deployment-owned compose layers make the rail inert | High where such layers exist | Validator warning; mirroring is activation work |
| Lock re-entry weakens serialization | Low | Four-part validation; six tests; security review |
| Both voice lifecycle subscribers apply every event during overlap | Unknown | Bounded by the 45 s split; backend review |
| CI runner lacks nginx, or 8080/8081 are busy | Unknown | Hard-fail; conditional `pr-ci.yml` edit |

**Not verified yet (each is a first step of the task that depends on it):**

- Compose name-targeting for `rm` and `ps` on a profiled service.
- That `voice_enforcement_rollout_guard_mutation` passes green's argv.
- That `control_plane_cli` stdout is clean in non-direct egress mode.
- `media-plane` behaviour on brief control-plane call failures.
- What consumes `deploy-manifest.txt`.
- That the host `nginx.conf` leaves `worker_shutdown_timeout` unset. Neither repo vhost file sets it.
- That building the golang-migrate Postgres driver takes the advisory lock and creates the version table in the pinned version.
- `/auth/ws-ticket` allows 10 per user per minute; two waves cost 2 tickets per device.

## 10. Out of scope

- Activating the rail in any deployment, and that deployment's own pipeline, health actor and runbook.
- The `session_drain` frame and close 4001 (Concord-Voice/Desktop#64); reconnect refetch (Concord-Voice/Desktop#68); WS state externalization (#79).
- The media plane, horizontal scaling, cross-host work, and stateful containers.
- A per-migration "overlap-safe" marker.
