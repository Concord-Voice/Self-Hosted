# Control-Plane Blue-Green Rail Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A default-off rail in `concord-ctl.sh` that rebuilds the control-plane with no 502s by starting a transient green instance, flipping nginx to it and back, and falling back to today's stop-first path whenever the schema is not provably current.

**Architecture:** Home stays the service `control-plane` on 8080. `control-plane-green` on loopback 8081 lives in an opt-in compose overlay and exists only inside a locked cutover. The live nginx vhost is the only record of the active colour. A new read-only `migrations status` subcommand and a `MIGRATIONS_MODE=verify` startup mode implement the migration gate.

**Tech Stack:** Bash (`infrastructure/deploy/concord-ctl.sh`), Docker Compose, nginx, Go (golang-migrate v4, `database/sql`), Python 3 stdlib (stub upstream).

**Spec:** `docs/superpowers/specs/2026-11-11-253-control-plane-blue-green-design.md`. Read it first; this plan does not repeat its reasoning.

## Global Constraints

- **Default-off.** With `CONTROL_PLANE_DEPLOY_PROFILE` unset, the compose command string, every docker/compose argv, the nginx render and all output are identical to today. The only permitted delta is one `[[ -e "$CONTROL_PLANE_CUTOVER_MARKER" ]]` test.
- **Never fail open.** Any doubt about the schema, the model, or the live port takes the drained path or refuses. Nothing proceeds on an unparsed value.
- **Names are fixed:** service `control-plane-green`, container `concordvoice-control-plane-green`, port `127.0.0.1:8081:8080`, profile `control-plane-bluegreen`, switch values `control-plane-single` | `control-plane-bluegreen`.
- **Ports:** only 8080 and 8081 are ever valid upstream ports.
- **Public repo.** No hostnames, addresses, provider names or private issue numbers in code, comments, tests, docs or commit messages.
- **Protected paths.** Only these may be edited under the protected patterns: `infrastructure/deploy/concord-ctl.sh`, `infrastructure/deploy/tests/test-control-plane-bluegreen.sh`, `infrastructure/deploy/tests/test-control-plane-bluegreen-nginx.sh`, `infrastructure/deploy/tests/control_plane_bluegreen_upstream.py`; and conditionally `CODEOWNERS`, `infrastructure/deploy/deploy-manifest.txt`, `.github/workflows/pr-ci.yml`. If a task seems to need any other protected file, stop and report.
- **Do not edit:** the six existing compose files, `nginx/concordvoice.conf`, `nginx/admin-console.conf.template`, `selfhost-port-posture.py`, `.env.example`, `docker_reader.go`.
- **Tests** follow `.claude/rules/domains/tests.md`: every rejection test names, in a comment, the one-line mutation of production code that turns it red; cover the happy and the error path.
- **Go** follows `.claude/rules/domains/backend.md`: every error checked and wrapped; no driver error or DSN in any output.
- **Line numbers in the spec are leads.** Locate every site by symbol name.
- **Commits:** Conventional Commits with a bare `type:`, no scope in parentheses. Never `git add -A`; `.claude/state/` must stay uncommitted.
- **No cache overrides:** never set `GOCACHE`, `GOMODCACHE` or `GOLANGCI_LINT_CACHE`.

## Review Focus

1. **Live vhost with CRLF endings or trailing whitespace on the `server` line.** Expect `control_plane_bluegreen_live_port` to refuse, never to return a port. (Task 3)
2. **Switch value with surrounding whitespace or quotes in the deploy env file.** Expect exit 2 `invalid control-plane deployment profile`, never a silent fall-through to single. (Task 4)
3. **`migrations status` with `DATABASE_URL` empty or the database unreachable.** Expect exit 1, a fixed stderr line, and no DSN or driver text anywhere. (Task 1)
4. **Marker cannot be written (read-only or full state directory).** Expect the drained path with nothing started, not a half-begun cutover. (Task 5)
5. **Stale marker, rail on, unscoped `rebuild`, live port 8080.** Expect green reaped, marker cleared, and the unscoped rebuild continuing normally. (Task 5)

---

## File Structure

| File | Responsibility |
|---|---|
| `infrastructure/deploy/tests/test-control-plane-bluegreen.sh` (create) | Hermetic harness: real `ctl`, PATH stubs for `docker`, `curl`, `nginx`, `systemctl`, `flock`. Characterization first, then every rail case. |
| `services/control-plane/internal/migrationrunner/status.go` (create) | Read-only schema status and verify. Never builds the migrate driver. |
| `services/control-plane/internal/database/postgres.go` (edit) | `MigrationStatus`, `VerifyMigrations` bound to `file://migrations`. |
| `services/control-plane/cmd/server/main.go` (edit) | `migrations` subcommand; `MIGRATIONS_MODE` parse before the database opens. |
| `docker-compose.control-plane-bluegreen.yml` (create) | The green service only. |
| `infrastructure/deploy/concord-ctl.sh` (edit) | Switch, guards, marker, renderer, reader, flip, cutover, recovery. All new functions are named `control_plane_bluegreen_*` and sit together directly after the `media_bluegreen_*` block. |
| `infrastructure/deploy/tests/test-control-plane-bluegreen-nginx.sh` + `control_plane_bluegreen_upstream.py` (create) | Real-nginx proof of zero 502 and WebSocket survival. |

---

### Task 0: Characterization tests (lands first, test file only)

**Files:**
- Create: `infrastructure/deploy/tests/test-control-plane-bluegreen.sh`

**Interfaces:**
- Produces: harness helpers used by every later ctl task: `run_ctl <args…>` (runs the fixture copy of `ctl` with the stub PATH, captures stdout+stderr to `$TMP/out` and the exit code to `$rc`), `transcript` (prints the ordered argv log every stub appends to `$TMP/calls`), `pass`/`fail`, and `stub_exit <tool> <pattern> <code>` (makes a stub return a code for a matching argv).

- [ ] **Step 1: Build the harness.** Copy the shape of `scripts/tests/test-concord-selfhost.sh` lines ~498-530: copy `ctl` and the files it sources into `$TMP/project`, rewrite its single deploy-lock path declaration to a `$TMP` path, put stubs first on `PATH`. Each stub appends `<tool> <argv>` to `$TMP/calls`. Use production-mode fixture inputs (`COMPOSE_OVERRIDE=docker-compose.production.yml`), not self-host mode.
- [ ] **Step 2: Write three characterization cases.**
  - `char_rebuild_control_plane_transcript`: `run_ctl rebuild control-plane` with the switch unset; assert `transcript` equals a golden heredoc inline in the test. The golden must show, in order: the build, `ps` for the foreign-project guard, `stop --timeout 45 control-plane`, the daemon-wide `docker ps --filter label=com.docker.compose.service=control-plane`, and `up -d --no-build --no-deps --wait --wait-timeout 180 control-plane`.
  - `char_wait_healthy_curl_argv`: assert every control-plane curl targets `http://127.0.0.1:8080/`.
  - `char_nginx_render_hash`: extract `encode_hsts_for_nginx` and `render_nginx_config` by name from `ctl` (awk from `name() {` to the matching closing brace at the same indent), run the renderer on the shipped `nginx/concordvoice.conf` with the default HSTS value, and assert the sha256 equals a constant recorded in the test.
- [ ] **Step 3: Run on the unmodified tree.** Run: `bash infrastructure/deploy/tests/test-control-plane-bluegreen.sh`. Expected: three `PASS` lines, exit 0. Record the golden and the hash from this run, then prove non-vacuity: change one golden line, see `FAIL`, restore.
- [ ] **Step 4: Lint.** Run: `shellcheck infrastructure/deploy/tests/test-control-plane-bluegreen.sh`. Expected: no output.
- [ ] **Step 5: Commit.** `test: characterize control-plane rebuild, wait-healthy and nginx render`

---

### Task 1: Go — `migrations status`, verify mode

**Files:**
- Create: `services/control-plane/internal/migrationrunner/status.go`, `status_test.go`
- Modify: `services/control-plane/internal/migrationrunner/runner.go` (package comment only), `services/control-plane/internal/database/postgres.go`, `services/control-plane/cmd/server/main.go`
- Test: `services/control-plane/cmd/server/main_subcommand_test.go`

**Interfaces:**
- Produces:
  - `type Status struct { Initialised bool; Version uint; Dirty bool; Latest uint }`
  - `func (s Status) Current() bool` — true only when `Initialised && !Dirty && Version == Latest`.
  - `func (s Status) Line() string` — exactly one of `current <n>`, `pending <db> <latest>`, `ahead <db> <latest>`, `dirty <db>`, `uninitialised <latest>`.
  - `func ReadStatus(ctx context.Context, db *sql.DB, sourceURL string) (Status, error)`
  - `func Verify(ctx context.Context, db *sql.DB, sourceURL string) error` — nil only when `Current()`; otherwise an error whose text is `schema not current for this binary (verify mode)`.
  - `func database.MigrationStatus(ctx context.Context, db *sql.DB) (migrationrunner.Status, error)` and `func database.VerifyMigrations(ctx context.Context, db *sql.DB) error`, both bound to `file://migrations`.
  - CLI contract for Task 5: `./main migrations status` prints `Line()` on stdout; exit 0 for `current`, 3 for the other four, 1 on any failure with the fixed stderr line `migrations status: unavailable`, 64 on a usage error. Total timeout 15 s.
  - Env contract for Task 2 and Task 5: `MIGRATIONS_MODE` empty or `apply` → `RunMigrations`; `verify` → `VerifyMigrations`; anything else → startup error `invalid MIGRATIONS_MODE` before any database connection.

- [ ] **Step 1: Confirm the driver side effect.** Read `database/postgres/postgres.go` of the pinned golang-migrate version in the module cache (`go list -m -f '{{.Dir}}' github.com/golang-migrate/migrate/v4`). Confirm `WithInstance` reaches `ensureVersionTable` (advisory lock + `CREATE TABLE IF NOT EXISTS`). If it does not, report before continuing; the design depends on it.
- [ ] **Step 2: Write failing tests in `status_test.go`** using the same real-Postgres helper `runner_test.go` uses. Cases: `TestReadStatus_Uninitialised`, `_Current`, `_Pending`, `_Ahead`, `_Dirty`, `_TwoRowsIsError`, `_EmptySourceIsError`, `TestReadStatus_WritesNothing` (snapshot `pg_class` row count and `pg_locks` advisory locks before and after: unchanged, and no `schema_migrations` table created on an empty schema), `TestVerify_ErrorsOnEveryNonCurrentCase`, `TestStatusLine_ExactFormats`.
- [ ] **Step 3: Run to see them fail.** Run: `cd services/control-plane && go test ./internal/migrationrunner/ -run 'ReadStatus|Verify|StatusLine' -p 1`. Expected: build failure, `undefined: ReadStatus`.
- [ ] **Step 4: Implement `status.go`.** Read path, in one transaction: `BEGIN READ ONLY`; `SET LOCAL statement_timeout='5s'`; `SELECT current_schema()`; `SELECT to_regclass($1)` on the schema-qualified, `pq.QuoteIdentifier`-quoted table name (NULL → not initialised); `SELECT version, dirty` (zero rows → not initialised; more than one → error). Latest: `source.Open(sourceURL)`, `First` then `Next` until `errors.Is(err, os.ErrNotExist)`; close the source. Do not call `postgres.WithInstance` or `migrate.New*`.
- [ ] **Step 5: Add the wrappers** in `postgres.go` and one sentence to the `runner.go` package comment: status and verify are read-only siblings of `Run` and must never build the migrate driver.
- [ ] **Step 6: Write failing subcommand tests** in `main_subcommand_test.go`: `migrations` routes to a new `migrations` runner in `controlPlaneSubcommandRunners`; `migrations` with no verb, an unknown verb, or extra args → 64; empty `DATABASE_URL` → 1 with stderr exactly `migrations status: unavailable\n` and empty stdout (Review Focus 3); unreachable database → 1 and the combined output contains no substring of the DSN; exit-code table for all five lines. Add `TestMigrationsMode_Parse` (`""`, `apply`, `verify`, `Verify`, `skip` → the last two are errors) and `TestMigrationsMode_VerifyNeverApplies` (inject the apply and verify functions; verify mode calls only verify).
- [ ] **Step 7: Implement** the `migrations` case in `dispatchControlPlaneSubcommandWithRunners`, the runner (reads `DATABASE_URL` from the environment, 15 s context), the usage string update, and `parseMigrationsMode(string) (mode, error)` called before `database.New` in the serve path. Replace the single `database.RunMigrations(db)` call with a switch on the mode; keep the existing `run migrations: %w` wrapping.
- [ ] **Step 8: Run.** `go test ./internal/migrationrunner/ ./internal/database/ ./cmd/server/ -p 1` → `ok` for all three. `golangci-lint run ./internal/migrationrunner/... ./cmd/server/... ./internal/database/...` → no findings.
- [ ] **Step 9: Commit.** `feat: add a read-only migrations status subcommand and verify startup mode`

---

### Task 2: Compose overlay and contract tests

**Files:**
- Create: `docker-compose.control-plane-bluegreen.yml`
- Modify: `infrastructure/deploy/tests/test-control-plane-bluegreen.sh`

**Interfaces:**
- Consumes: `MIGRATIONS_MODE` (Task 1).
- Produces: service `control-plane-green` whose rendered config equals `control-plane`'s except exactly these keys: `container_name: concordvoice-control-plane-green`; `ports: ["127.0.0.1:8081:8080"]`; `networks.<services network>.aliases: [control-plane]`; `environment.MIGRATIONS_MODE: verify`; `environment.OPS_METRICS_ENABLED: "false"`; `profiles: [control-plane-bluegreen]`; no `build`; `pull_policy: never`. The overlay also sets `environment.MIGRATIONS_MODE: ${CONCORD_CONTROL_PLANE_MIGRATIONS_MODE:-apply}` on `control-plane`.

- [ ] **Step 1: Write failing tests** (skip with a `SKIP` line when `docker compose` is absent locally; hard-fail when `CI` is set):
  - `compose_green_parity`: `docker compose -f docker-compose.yml -f docker-compose.production.yml -f docker-compose.control-plane-bluegreen.yml --profile control-plane-bluegreen config --format json`; a python3 one-off asserts the key-difference set between the two services is exactly the list above. Supply dummy values for required env through an env file in `$TMP`.
  - `compose_green_not_default`: the same render without `--profile` contains no `control-plane-green`.
  - `compose_base_unchanged`: `config` of base+production with and without the overlay (no profile) differs only in `control-plane.environment.MIGRATIONS_MODE == "apply"`.
  - `compose_named_service_activates_profile`: with a throwaway two-service project in `$TMP` (image `busybox`, one service behind a profile), prove `up -d`, `ps -q`, `stop` and `rm -f` work when the profiled service is named and no profile is active. Requires a running daemon; `SKIP` locally without one, hard-fail under `CI`.
- [ ] **Step 2: Run to see them fail** (`overlay file not found`).
- [ ] **Step 3: Write the overlay.** Use YAML merge only if the rendered parity test passes; otherwise restate keys. Find the services network name in `docker-compose.yml`; do not guess it. Confirm in the Go source that `OPS_METRICS_ENABLED=false` means the reader is never opened or closed; if the variable has a different name, use the real one and report it.
- [ ] **Step 4: Run** → all `PASS` (or `SKIP` locally for the daemon case).
- [ ] **Step 5: Decide the conditional `CODEOWNERS` line.** If `CODEOWNERS` lists `docker-compose.production.yml`, add the overlay on the same line pattern. Otherwise leave it.
- [ ] **Step 6: Commit.** `feat: add the opt-in control-plane green compose overlay`

---

### Task 3: nginx renderer, live-port reader, flip argument, lock re-entry

**Files:**
- Modify: `infrastructure/deploy/concord-ctl.sh`, `infrastructure/deploy/tests/test-control-plane-bluegreen.sh`

**Interfaces:**
- Consumes: `CONTROL_PLANE_BLUEGREEN` (`0`|`1`) and `CONTROL_PLANE_CUTOVER_MARKER` — defined in Task 4. In this task declare both near the other deploy-profile variables with `CONTROL_PLANE_BLUEGREEN=0` hard-coded, so the task is testable alone; tests set it by rewriting that one line in the fixture copy.
- Produces:
  - `control_plane_bluegreen_live_port` — prints `8080` or `8081` and returns 0; returns 1 with no stdout if the live vhost is unparseable. Missing file → `8080`.
  - `render_nginx_config <hsts-policy> [<cp-port>]` — second argument is used only when `CONTROL_PLANE_BLUEGREEN=1`.
  - `readonly CONTROL_PLANE_FLIP_REENTRY_MARKER="--concord-ctl-control-plane-flip-v1"` and `validate_control_plane_flip_reentry`.
  - Internal argv form: `"$0" "$CONTROL_PLANE_FLIP_REENTRY_MARKER" nginx-reload --control-plane-upstream <8080|8081>`.
  - Error `control-plane-bluegreen: live-upstream-unparseable` (exit 1).

- [ ] **Step 1: Write failing renderer tests** (extract the function by name as in Task 0, with `CONTROL_PLANE_BLUEGREEN=1`): `render_rail_on_8080_equals_char_hash`; `render_rail_on_8081_changes_only_the_server_line` (diff against the 8080 render is exactly one line); rejects, each asserting return 1, temp file removed, target file untouched: zero `upstream control_plane {` blocks, two blocks, two `server` lines, a `server … backup;` line, `server 10.0.0.1:8080;`, a tab-indented server line, an unclosed block, port `9999`. `render_rail_off_ignores_port_argument` (hash equals Task 0's).
- [ ] **Step 2: Write failing reader tests:** `live_port_missing_file_is_8080`, `live_port_8081`, `live_port_9999_refuses`, `live_port_garbage_refuses`, `live_port_two_blocks_refuses`, `live_port_crlf_refuses`, `live_port_trailing_space_refuses` (Review Focus 1), `live_port_roundtrip` (render at 8081 then read → `8081`). The reader takes the vhost path from the same variable `nginx-reload` uses; tests point it into `$TMP`.
- [ ] **Step 3: Write failing re-entry tests,** using the real `flock` (hard-fail under `CI` if absent, `SKIP` locally): no fd 9 → rejected; fd 9 open on another file → rejected; fd 9 on the lock file but lock not held → rejected; lock held by an unrelated process → rejected; genuine parent but argv not exactly the flip form → rejected; genuine → accepted and the child does not re-acquire. Also `flip_form_refused_when_rail_off` and `flip_form_refused_without_marker_file` → today's `nginx-reload` usage error, exit 2.
- [ ] **Step 4: Run to see them fail.**
- [ ] **Step 5: Implement.** Renderer: add the block state machine inside the existing loop, guarded by `(( CONTROL_PLANE_BLUEGREEN ))`; extend the final invariant check only on that branch. Reader: a top-level function using the same line patterns. Re-entry: mirror `validate_lockf_reentry_parent` and its argv-marker normalization block; the four checks are in spec 4.6 "Nested flip"; on success `acquire_deploy_transaction_lock` returns before opening fd 9. `nginx-reload` argv parsing: accept `--control-plane-upstream <port>` only when re-entry is validated, the rail is on and the marker file exists; with the rail on and no flip argument, call the reader and refuse with `live-upstream-unparseable` on failure; pass the port to `render_nginx_config`.
- [ ] **Step 6: Run** → all `PASS`; Task 0 cases still `PASS`. `shellcheck infrastructure/deploy/concord-ctl.sh` → no new findings (`git show HEAD:infrastructure/deploy/concord-ctl.sh | shellcheck -` gives the baseline count).
- [ ] **Step 7: Commit.** `feat: render and read the active control-plane upstream port`

---

### Task 4: Switch, refusals, guard widening, marker

**Files:**
- Modify: `infrastructure/deploy/concord-ctl.sh`, `infrastructure/deploy/tests/test-control-plane-bluegreen.sh`

**Interfaces:**
- Produces:
  - `CONTROL_PLANE_DEPLOY_PROFILE` read from the deploy env file exactly as `MEDIA_DEPLOY_PROFILE` is; `CONTROL_PLANE_BLUEGREEN=1` only for the value `control-plane-bluegreen` in the production/staging branch; `0` in every other branch.
  - `CONTROL_PLANE_CUTOVER_MARKER="${CONFIG_DEPLOY_DIR}/.control-plane-cutover.local"`.
  - `control_plane_bluegreen_marker_guard <verb>` — returns 0 to proceed. Rail off + marker → `control-plane-bluegreen: rail-off-with-cutover-marker`, exit 1. Rail on + marker → `control-plane-bluegreen: cutover-incomplete`, exit 1, except for `rebuild`, `nginx-reload` and `stop`.
  - With the rail on, `COMPOSE_CMD` gains ` -f ${DEPLOY_DIR}/docker-compose.control-plane-bluegreen.yml` and `CONCORD_CONTROL_PLANE_MIGRATIONS_MODE=apply` is exported.
  - Refusals, exit 2: `control-plane-bluegreen: invalid control-plane deployment profile`; `rail-refused-media-bluegreen`; `rail-refused-no-flock`; `rail-refused-no-state-dir`; `rail-refused-overlay-missing`.

- [ ] **Step 1: Write failing tests:** `switch_unset_compose_cmd_identical` (the Task 0 transcript still matches byte for byte); `switch_single_identical`; `switch_bluegreen_appends_overlay` (transcript's compose argv contains the overlay `-f`); `switch_invalid_exits_2`; `switch_quoted_or_padded_value_exits_2` for `" control-plane-bluegreen"` and `'"control-plane-bluegreen"'` (Review Focus 2); `switch_forced_single_in_selfhost_mode` (self-host fixture + switch set → transcript identical to self-host baseline, exit 0); one test per `rail-refused-*`; `guard_rail_on_counts_green` (stub the second `docker ps` to return an ID → the drained check refuses); `guard_rail_on_second_ps_failure_refuses`; `guard_rail_on_green_is_own` (foreign-project guard accepts an ID that `compose ps -q -- control-plane control-plane-green` lists); `guard_rail_on_foreign_still_refused`; `guard_rail_off_single_docker_ps` (transcript shows exactly one daemon-wide `docker ps`); `marker_rail_off_refuses_rebuild`; `marker_rail_on_refuses_restart_allows_nginx_reload_and_stop`; `rebuild_scope_control_plane_green_rejected` (pins existing behaviour); `posture_rejects_rendered_green` (feed `selfhost-port-posture.py` a model containing the green publish → non-zero); `managed_redis_makes_no_green_call`.
- [ ] **Step 2: Run to see them fail.**
- [ ] **Step 3: Implement.** Replace Task 3's hard-coded `CONTROL_PLANE_BLUEGREEN=0`. Widen `activity_history_refresh_daemon_ids` with a second `docker ps` only under the rail; widen the own-set in `privacy_defaults_refuse_foreign_control_plane` only under the rail. Call `control_plane_bluegreen_marker_guard` once, right after the deploy lock is acquired, for every verb that takes the lock. `stop` additionally stops and removes green when a marker exists.
- [ ] **Step 4: Run** → all `PASS`, Task 0 unchanged. shellcheck clean.
- [ ] **Step 5: Commit.** `feat: add the control-plane blue-green switch, guard coverage and cutover marker`

---

### Task 5: Cutover, recovery, rebuild hook, wait-healthy

**Files:**
- Modify: `infrastructure/deploy/concord-ctl.sh`, `infrastructure/deploy/tests/test-control-plane-bluegreen.sh`

**Interfaces:**
- Consumes: everything from Tasks 1-4.
- Produces:
  - `control_plane_bluegreen_validate_model` — 0 valid; 1 invalid, printing `control-plane-bluegreen: model differs from control-plane in: <key names>` (names only).
  - `control_plane_bluegreen_schema_current` — 0 only on exit 0 and stdout matching `^current [0-9]+$` from `control_plane_cli migrations status`.
  - `control_plane_bluegreen_flip <port>`; `control_plane_bluegreen_reap_green`.
  - `control_plane_bluegreen_cutover` — returns 0 when the round trip completed (the caller skips the drained sequence); returns 10 for "take the drained path, nothing was changed"; exits 1 with a named error otherwise.
  - `control_plane_bluegreen_recover <scope…>` — implements the spec 4.6 recovery table.
  - `activity_history_wait_control_plane [<full-url>]`.
  - `control_plane_cli`: `migrations` added to the forced-fresh-container condition.

- [ ] **Step 1: Verify three spec unknowns first** and note each result in the commit body: `control_plane_cli` stdout is only the subcommand's stdout in non-direct egress mode; `voice_enforcement_rollout_guard_mutation` (if it wraps compose mutations) accepts argv naming `control-plane-green`; the vhost and any nginx config in this repo leave `worker_shutdown_timeout` unset.
- [ ] **Step 2: Write failing happy-path tests:** `cutover_happy_transcript` — golden of the full ordered argv for one round trip (model render, status run, foreign guard, `up … control-plane-green`, curl `http://127.0.0.1:8081/readyz`, flip 8081, `stop --timeout 45 control-plane`, `up … control-plane` with `CONCORD_CONTROL_PLANE_MIGRATIONS_MODE=verify` in its environment, curl `http://127.0.0.1:8080/readyz`, flip 8080, `stop --timeout 45 control-plane-green`, `rm -f control-plane-green`), marker absent afterwards, exit 0; `cutover_gates_on_readyz_not_health`; `cutover_only_for_exact_scope` (`rebuild control-plane media-plane` with the rail on → the Task 0-style drained transcript).
- [ ] **Step 3: Write failing gate tests** — each asserts the five drained calls appear in their original order and no `control-plane-green` argv appears: status stdout `current` (no number); `current 12\nextra`; exit 3 `pending 11 12`; exit 64; exit 1; status timeout; validator failure for a dropped env key, a `0.0.0.0` publish and a second port; `validator_output_has_no_env_values` (plant `SENTINEL_SECRET_VALUE` in a differing key and assert it is absent from all output); `marker_write_failure_takes_drained_path` (make the marker directory read-only → drained transcript, no green call) (Review Focus 4).
- [ ] **Step 4: Write failing order and failure tests:** `stop_follows_reload_leg1` and `_leg2` (the very next mutating call after each successful flip is the old colour's stop); `int_during_flip_still_stops` (the `nginx` stub sleeps, the test sends INT to `ctl`, the stop still appears); one test per named error asserting the message, exit 1 and the end state from the spec (`standby-image-mismatch`, `standby-not-ready` → no flip call and green reaped, `flip-failed` on each leg, `old-colour-stop-failed` → marker kept, `home-not-ready` → marker kept and no stop of green).
- [ ] **Step 5: Write failing recovery tests,** one per table row: `recover_unparseable_refuses_nothing_changed`; `recover_live_8080_reaps_and_continues`; `recover_stale_marker_unscoped_rebuild_continues` (Review Focus 5); `recover_live_8081_green_ready_current_resumes_steps_8_9`; `recover_live_8081_green_ready_pending_runs_apply_then_flips_home`; `recover_live_8081_green_ready_wrong_scope_is_cutover_incomplete`; `recover_live_8081_green_absent_runs_drained_then_flips_home`; `recover_runs_before_routine_guard`.
- [ ] **Step 6: Write failing `wait-healthy` tests:** `wait_healthy_rail_on_uses_live_port` (live vhost at 8081 → curls target 8081); `wait_healthy_rail_on_unparseable_exits_1`; `wait_healthy_rail_off_unchanged` (Task 0 case).
- [ ] **Step 7: Run to see them fail.**
- [ ] **Step 8: Implement** per spec 4.6, steps 1-10 and the recovery table. Hook placement: in `rebuild`, after the build and before the "draining the previous control-plane" echo: `if rail on and scope is exactly control-plane: control_plane_bluegreen_cutover; rc=$?; (0 → skip the five drained lines) (10 → fall through)`. Leave the five drained lines byte-identical. Signal handling around each flip+stop pair: `trap '' HUP INT TERM` before, restore the previous traps after. Marker write: `umask 077`, write to a temp name in the same directory, `mv`. Update the comment block above the drained sequence to describe both paths.
- [ ] **Step 9: Run** → every case `PASS`; Task 0 cases unchanged; shellcheck clean.
- [ ] **Step 10: Commit.** `feat: add the control-plane blue-green cutover and its recovery`

---

### Task 6: Real-nginx proof

**Files:**
- Create: `infrastructure/deploy/tests/test-control-plane-bluegreen-nginx.sh`, `infrastructure/deploy/tests/control_plane_bluegreen_upstream.py`

**Interfaces:**
- Consumes: `render_nginx_config` with a port argument (Task 3).
- Produces: nothing consumed by other tasks.

- [ ] **Step 1: Write the stub upstream** (python3 stdlib only): `control_plane_bluegreen_upstream.py --port <n> --name <blue|green>`. `GET /health` → 200 with body `<name>`. A WebSocket echo endpoint at the path the shipped vhost proxies (do the RFC 6455 handshake and unmasked text echo by hand). On SIGTERM: stop accepting, finish in-flight requests, keep open WebSockets for up to 5 s, then exit: the Go `Shutdown` shape.
- [ ] **Step 2: Write the test.** Preflight: `nginx` on `PATH` (missing → `SKIP` locally, hard-fail when `CI` is set); ports 8080 and 8081 free (busy → same rule). Build a minimal `nginx.conf` in `$TMP` (own pid file, unprivileged listen port, `daemon on`) whose `upstream control_plane` block is produced by the real `render_nginx_config` and whose WebSocket `location` block is extracted from the shipped vhost. Cases:
  - `flip_zero_non_200`: one fresh-connection loop and one keep-alive loop request `/health` continuously while the test renders 8081, runs `nginx -t`, reloads, then SIGTERMs the blue stub. Assert zero non-200 across both loops and that the last responses say `green`.
  - `websocket_survives_reload`: open a WebSocket through nginx before the flip; after reload and before the blue stub exits, send a frame and get the echo.
  - `new_websocket_lands_on_new_colour`: a WebSocket opened after the reload is answered by `green`.
  - `negative_control`: SIGTERM blue *before* the reload and assert at least one non-200 — proves the probe can see a failure.
- [ ] **Step 3: Run.** `bash infrastructure/deploy/tests/test-control-plane-bluegreen-nginx.sh` → four `PASS`, or `SKIP` with a stated reason when nginx is absent locally.
- [ ] **Step 4: Check the CI runner.** Read the deploy-test job in `.github/workflows/pr-ci.yml`. If its runner image has no nginx and no step installs it, add one install step to that job (conditional protected edit) and say so in the commit body. Otherwise do not touch the workflow.
- [ ] **Step 5: Commit.** `test: prove zero 502s and WebSocket survival across an nginx upstream flip`

---

### Task 7: Docs

**Files:**
- Modify: `docs/architecture.md`, `docs/development.md`, `CHANGELOG.md`, `.claude/rules/domains/migrations.md`

- [ ] **Step 1: `docs/architecture.md`.** Add a "Control-plane blue-green deploys" section next to the existing deploy-verb material: the round-trip colour model; the switch and where it is honoured (managed production/staging only; forced off in self-host mode; not reached under managed Redis); how to tell the active colour (the `server` line in the live vhost's `upstream control_plane` block; the marker file); the migration gate; every named error with the state each leaves and the operator action; forcing back (rerun `rebuild control-plane`; last resort `stop` then `rebuild control-plane`); the SIGKILL-between-flip-and-stop case; the warning against a hand-run `docker compose up` after an overlap; deployment-owned compose layers must be mirrored onto green; activation preconditions (Concord-Voice/Desktop#68, Concord-Voice/Desktop#64). Add env-table rows for `MIGRATIONS_MODE` and `CONTROL_PLANE_DEPLOY_PROFILE`.
- [ ] **Step 2: `docs/development.md`.** In the deploy-verb section: the `migrations status` subcommand with its output and exit-code table, and how to run the two new test scripts.
- [ ] **Step 3: `CHANGELOG.md`.** One entry under the unreleased heading, in the file's existing style: default-off rail, `migrations status`, `MIGRATIONS_MODE`.
- [ ] **Step 4: `.claude/rules/domains/migrations.md`.** One sentence beside the runner contract: status and verify are read-only and must never build the migrate driver, because building it takes the advisory lock and can create the version table.
- [ ] **Step 5: Verify.** Every identifier named in the docs exists in the code: `grep -c` each of `CONTROL_PLANE_DEPLOY_PROFILE`, `MIGRATIONS_MODE`, `control-plane-green`, and each named error in `infrastructure/deploy/concord-ctl.sh` or `services/control-plane` → non-zero. Run the repo's markdown lint if `.pre-commit-config.yaml` has one: `pre-commit run --files docs/architecture.md docs/development.md CHANGELOG.md`.
- [ ] **Step 6: Commit.** `docs: document the control-plane blue-green rail and its migration gate`
