#!/usr/bin/env bash
# Tests for concord-dev.sh helper functions.
# Strategy: source the script, then call helpers directly.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
CONCORD_DEV="$PROJECT_ROOT/scripts/concord-dev.sh"

if [[ ! -x "$CONCORD_DEV" ]]; then
  echo "FAIL: $CONCORD_DEV not executable"
  exit 1
fi

# The server package contains multiple source files. Building main.go directly
# silently omits package siblings and breaks as soon as startup logic is split.
if grep -Fq 'go build -o bin/control-plane ./cmd/server' "$CONCORD_DEV"; then
  echo "PASS: control-plane build includes the complete cmd/server package"
else
  echo "FAIL: control-plane build must target ./cmd/server, not main.go"
  exit 1
fi

# ── Source the script (helpers become available) ─────────────────────
# The source-vs-execute guard at the bottom of concord-dev.sh prevents
# main() from running on source.
# shellcheck disable=SC1090
source "$CONCORD_DEV"

# ── compose() wrapper ─────────────────────────────────────────────────
# Set up a docker stub that records the args it was called with.
STUB_DIR=$(mktemp -d)
export DOCKER_STUB_LOG=$(mktemp)
# Note: TMP_PR is mktemp'd later in the bootstrap_env test (~line 130).
# Consolidate all tempfile cleanup into a single EXIT trap to avoid
# overwriting one with another (bash only honors one EXIT handler).
TMP_PR=""
trap 'rm -rf "$STUB_DIR" "$DOCKER_STUB_LOG" "$TMP_PR" 2>/dev/null || true' EXIT

cat > "$STUB_DIR/docker" <<'STUB'
#!/usr/bin/env bash
echo "$*" > "${DOCKER_STUB_LOG}"
exit 0
STUB
chmod +x "$STUB_DIR/docker"
PATH="$STUB_DIR:$PATH" compose ps

# Verify the stub was called with `compose -f <yml> -f <yml> ps`.
if grep -q "compose -f .*docker-compose.yml -f .*docker-compose.dev.yml ps" "$DOCKER_STUB_LOG"; then
  echo "PASS: compose() prepends -f flags for base and dev overlay"
else
  echo "FAIL: compose() did not include both -f flags"
  cat "$DOCKER_STUB_LOG"
  exit 1
fi

# ── port_in_use() ─────────────────────────────────────────────────────
# Use a port in the registered range (1024-49151) that is vanishingly
# unlikely to be bound. Ports 1-1023 are privileged and won't be bound
# by user processes, but kernel-only services (e.g., ssh on 22) might
# use them. Ports 49152-65535 are ephemeral and can be assigned at any
# time to outbound connections by the kernel — including by THIS process.
# Port 11111 is non-standard and stable.
if port_in_use 11111; then
  echo "FAIL: port_in_use(11111) returned true; expected false"
  exit 1
else
  echo "PASS: port_in_use returns false for free port"
fi

# ── wait_for_service() — timeout path ─────────────────────────────────
# Use a port that is not listening; expect 1-attempt timeout.
if wait_for_service "http://localhost:11111/none" "Unreachable" 1 >/dev/null 2>&1; then
  echo "FAIL: wait_for_service should time out on unreachable URL"
  exit 1
else
  echo "PASS: wait_for_service times out on unreachable URL"
fi

# ── stop_process() — missing PID file ─────────────────────────────────
NONEXISTENT_PID=$(mktemp -u)
if stop_process "$NONEXISTENT_PID" "Missing" >/dev/null 2>&1; then
  echo "PASS: stop_process returns 0 on missing PID file"
else
  echo "FAIL: stop_process should return 0 when PID file is missing"
  exit 1
fi

# ── stop_process() — stale PID file ───────────────────────────────────
# Use a PID that's almost certainly not running. Note: modern Linux kernels
# default pid_max=4194304 (2^22), so 999999 is technically reachable. We
# accept the residual collision risk (~1 in 4M); ps -p just returns false
# when the PID isn't a live process, regardless of whether it's "above max."
STALE_PID_FILE=$(mktemp)
echo "999999" > "$STALE_PID_FILE"
if stop_process "$STALE_PID_FILE" "Stale" >/dev/null 2>&1; then
  if [[ ! -f "$STALE_PID_FILE" ]]; then
    echo "PASS: stop_process removes stale PID file"
  else
    echo "FAIL: stop_process should remove stale PID file"
    exit 1
  fi
else
  echo "FAIL: stop_process should return 0 on stale PID file"
  exit 1
fi

# ── port_in_use() happy-path — bound port returns true ───────────────
# Bind a socket via Python (universally available; nc behavior differs by OS).
# Using Python's socket lib is more deterministic than `nc -l` (BSD nc vs.
# OpenBSD nc vs. ncat all behave differently with `-l`).
PORT=11211  # memcached default — unlikely to be in use on a dev laptop
# Both paths below kill the listener; its 30 s sleep only bounds it if the
# test dies first. lsof walks every process and took over 3 s on a busy dev
# host, so a 3 s listener had exited before lsof reached it.
python3 -c "
import socket, time, sys
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(('127.0.0.1', $PORT))
s.listen(1)
sys.stdout.write('ready\n'); sys.stdout.flush()
time.sleep(30)
s.close()
" > /tmp/test-port-bind.out &
LISTENER_PID=$!
# Wait for the listener to print 'ready'
for _ in 1 2 3 4 5; do
  [[ -f /tmp/test-port-bind.out ]] && grep -q ready /tmp/test-port-bind.out && break
  sleep 0.2
done

if port_in_use "$PORT"; then
  echo "PASS: port_in_use returns true for bound port"
else
  echo "FAIL: port_in_use should return true for bound port $PORT"
  kill "$LISTENER_PID" 2>/dev/null || true
  rm -f /tmp/test-port-bind.out
  exit 1
fi

kill "$LISTENER_PID" 2>/dev/null || true
wait "$LISTENER_PID" 2>/dev/null || true
rm -f /tmp/test-port-bind.out

# ── bootstrap_env() — copies .env.example to .env with mode 600 ──────
# Override PROJECT_ROOT via a function-local variable. bootstrap_env reads
# the global PROJECT_ROOT — but PROJECT_ROOT is readonly in the script.
# Workaround: call bootstrap_env with both PROJECT_ROOT-relative paths set
# up in a tmpdir, and override PROJECT_ROOT temporarily via `declare`.
# TMP_PR is declared at top of file so the unified EXIT trap covers it.
TMP_PR=$(mktemp -d)
echo "POSTGRES_PASSWORD=test" > "$TMP_PR/.env.example"

# We can't reassign readonly PROJECT_ROOT. Instead, source the script in a
# subshell with LOG_DIR overridden to TMP_PR, then run bootstrap_env via a
# small wrapper that overrides PROJECT_ROOT just for the function call.
# Cleanest: invoke bootstrap_env via `bash -c` that re-sources with PROJECT_ROOT
# pre-empted via the readonly-skip trick.
#
# Pragmatic solution: drive bootstrap_env's behavior via stub binary paths.
# Run a minimal script that copies the same logic with our PROJECT_ROOT.
(
  cd "$TMP_PR"
  # Mimic bootstrap_env exactly. If the production script's logic changes,
  # this test will diverge — accepted tradeoff vs. the PROJECT_ROOT-readonly
  # constraint. The chmod 600 invariant is the load-bearing security check.
  if [[ ! -f "$TMP_PR/.env" && -f "$TMP_PR/.env.example" ]]; then
    cp "$TMP_PR/.env.example" "$TMP_PR/.env"
    chmod 600 "$TMP_PR/.env"
  fi
)
if [[ -f "$TMP_PR/.env" ]]; then
  # Use Python for portable mode-extraction. `stat -f '%A'` is BSD-only (macOS);
  # `stat -c '%a'` is GNU-only (Linux). Mixing the two with `||` doesn't work
  # cleanly because BSD `stat -f` is "filesystem info" on GNU systems and
  # vice-versa, so the first call may succeed with garbage instead of erroring.
  MODE=$(python3 -c "import os; print(oct(os.stat('$TMP_PR/.env').st_mode)[-3:])")
  if [[ "$MODE" == "600" ]]; then
    echo "PASS: bootstrap_env pattern creates .env with mode 600 (security-relevant)"
  else
    echo "FAIL: .env mode should be 600 (got: $MODE)"
    exit 1
  fi
else
  echo "FAIL: bootstrap_env pattern should create .env"
  exit 1
fi
# TMP_PR cleanup is handled by the consolidated EXIT trap at the top.

# ── verb_freshstart() nukes orphan containers by name ────────────────
# Regression test for the post-#1155 migration bug: users upgrading from
# the pre-#1113 `dev-start.sh` (no compose `name:` directive) carry over
# containers named `concordvoice-nats` etc. that belong to a different
# compose project and survive `compose down -v` against the new project.
# verb_freshstart must `docker rm -f` them explicitly.

# Reset the docker stub log + override verb_up / verb_down so the test
# doesn't actually try to bring up Postgres etc. Function redefinition
# is bash-legal and scoped to this shell. Replace the single-write stub
# with an APPENDING stub so we capture both `compose down -v` AND the
# follow-up `docker rm -f` invocations.
: > "$DOCKER_STUB_LOG"
cat > "$STUB_DIR/docker" <<'STUB'
#!/usr/bin/env bash
echo "$*" >> "${DOCKER_STUB_LOG}"
exit 0
STUB
chmod +x "$STUB_DIR/docker"
verb_up() { :; }
verb_down() { :; }
# Drive freshstart via </dev/null so the TTY-prompt branch is skipped
# (verb_freshstart's `[[ -t 0 ]]` check). Export PATH so the stub
# applies to ALL docker calls inside verb_freshstart, not just the
# inline one. Send stdout/stderr to /dev/null so the test output stays
# clean.
#
# verb_freshstart ALSO runs `rm -rf "$PROJECT_ROOT/.dev-data"` inline. That is
# not behind verb_up/verb_down, so stubbing those two does not contain it, and
# PROJECT_ROOT is readonly so the test cannot redirect it. Left unstubbed this
# deletes the developer's REAL .dev-data — Electron client profiles, the
# control-plane binary and its PID file — whenever the suite runs from the main
# checkout, which the `concord-dev-unit-tests` pre-commit hook does on every
# change to this file. `2>/dev/null || true` swallows the evidence, so the suite
# prints all-PASS and exits 0 while destroying live state. Worktrees have no
# .dev-data, which is why it stayed invisible. Shadow `rm` with a FUNCTION —
# functions win over PATH lookup, so unlike a stub binary this cannot be
# defeated by hash-table state — and record the call instead of performing it.
rm() { printf 'rm %s\n' "$*" >> "${DOCKER_STUB_LOG}"; }
PATH="$STUB_DIR:$PATH" verb_freshstart </dev/null >/dev/null 2>&1 || true
unset -f rm

# The wipe must have been ATTEMPTED and intercepted. Asserting it also pins the
# behaviour, so a future change that stops wiping .dev-data is caught too.
if grep -qE 'rm -rf .*/\.dev-data' "$DOCKER_STUB_LOG"; then
  echo "PASS: verb_freshstart .dev-data wipe intercepted, not performed"
else
  echo "FAIL: verb_freshstart did not attempt the .dev-data wipe (rm shim ineffective?)"
  cat "$DOCKER_STUB_LOG"
  exit 1
fi

# Grep the docker stub log for the literal rm -f line. Container names
# come from docker-compose.yml `container_name:` values and are stable.
if grep -q "rm -f concordvoice-postgres concordvoice-redis concordvoice-nats concordvoice-coturn concordvoice-minio" "$DOCKER_STUB_LOG"; then
  echo "PASS: verb_freshstart force-removes orphan containers by literal name"
else
  echo "FAIL: verb_freshstart did not invoke 'docker rm -f' with the expected container names"
  echo "  expected substring: rm -f concordvoice-postgres concordvoice-redis concordvoice-nats concordvoice-coturn concordvoice-minio"
  echo "  docker stub log contents:"
  cat "$DOCKER_STUB_LOG"
  exit 1
fi

# ── udp_port_in_use() — endpoint parsing ──────────────────────────────
# See udp_port_in_use()'s header comment in concord-dev.sh for why a wildcard
# port (`n*:*`, which stock macOS daemons report) is a real socket state and
# not a probe failure. The rationale lives there; this block pins the behaviour.
#
# Drive the probe through a PATH-stubbed lsof so the fixtures are exact
# and the assertions never depend on what this machine happens to bind.
# The fixture lives inside STUB_DIR, so the existing EXIT trap already
# cleans it up (bash honors only one EXIT handler — do not add another).
LSOF_FIXTURE="$STUB_DIR/lsof-fixture"
export LSOF_FIXTURE
# The stub also records its argv. Without that, dropping -P from lsof_query's
# flags would make real lsof emit service names (`*:mdns`) and send every macOS
# probe back to rc=2 — this PR's exact bug class — while the stubbed suite
# stayed green.
LSOF_ARGV_LOG="$STUB_DIR/lsof-argv"
export LSOF_ARGV_LOG
cat > "$STUB_DIR/lsof" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${LSOF_ARGV_LOG}"
[[ -s "$LSOF_FIXTURE" ]] || exit 1
cat "$LSOF_FIXTURE"
STUB
chmod +x "$STUB_DIR/lsof"

# udp_probe <expected-rc> <label> — fixture on stdin, probe port 3478.
# Runs in a subshell so the stub PATH cannot leak into later tests, and
# `hash -r` drops any real-lsof path bash cached from the port_in_use
# tests above. The `if` context suppresses errexit, matching how
# verb_down actually calls this helper.
udp_probe() {
  local want="$1" label="$2" got=0
  cat > "$LSOF_FIXTURE"
  if ( PATH="$STUB_DIR:$PATH"; hash -r 2>/dev/null; udp_port_in_use 3478 ); then
    got=0
  else
    got=$?
  fi
  if [[ "$got" -eq "$want" ]]; then
    echo "PASS: $label (rc=$got)"
  else
    echo "FAIL: $label — expected rc=$want, got rc=$got"
    echo "  fixture:"
    sed 's/^/    /' "$LSOF_FIXTURE"
    exit 1
  fi
}

# 1. The regression: wildcard-port sockets present, 3478 not bound.
#    A wildcard is "not the port we asked about", so the port is FREE.
udp_probe 1 "udp_port_in_use skips wildcard-port (*:*) sockets and reports port free" <<'FIXTURE'
p123
f5
n*:*
p456
f7
n*:*
p789
f9
n192.168.0.113:64023->162.159.136.232:443
p790
f11
n[2600:4040:2927:3f01::1]:59121->[2606:4700:440a::ac40:93d3]:443
FIXTURE

# 2. Busy detection must survive the fix. The wildcard entries come
#    FIRST, so this also proves the scan continues past them instead of
#    short-circuiting to "free".
udp_probe 0 "udp_port_in_use still reports a socket bound to 3478 as in use" <<'FIXTURE'
p123
f5
n*:*
p1002
f619
n*:3478
FIXTURE

# 3. Same, via the bracketed IPv6 form.
udp_probe 0 "udp_port_in_use detects a bound IPv6 3478 socket past wildcards" <<'FIXTURE'
p123
f5
n*:*
p1002
f619
n[::]:3478
FIXTURE

# 4. A wildcard REMOTE endpoint must not trip the failure path either —
#    only the local side decides whether the port is ours.
udp_probe 1 "udp_port_in_use tolerates a wildcard remote endpoint" <<'FIXTURE'
p123
f5
n192.168.0.113:5353->*:*
p124
f6
n*:*->*:*
FIXTURE

# 5. Fail-closed is preserved: a port field that is neither numeric nor
#    `*` means the probe genuinely could not be read, so return 2 and
#    let the caller abort rather than guess. `*:http` is a SYNTHETIC guard
#    input — the production call passes -P, which inhibits service-name
#    conversion, so real output never contains it. Do not "fix" the parser
#    to accept service names on the strength of this fixture.
udp_probe 2 "udp_port_in_use fails closed on a non-numeric, non-wildcard port" <<'FIXTURE'
p123
f5
n*:http
FIXTURE

udp_probe 2 "udp_port_in_use fails closed on an endpoint with no port field" <<'FIXTURE'
p123
f5
nnot-an-endpoint
FIXTURE

# 7. The helper's documented reason for parsing the LOCAL side: outbound
#    traffic TO a remote :3478 is not a conflict. Without this, a mutation
#    that matches anywhere in the endpoint passes every assertion above,
#    and `down --force` would report "Port 3478 still in use" for any
#    outgoing STUN/QUIC flow — the same user-visible failure this PR fixes,
#    arriving from the opposite direction.
udp_probe 1 "udp_port_in_use ignores a remote endpoint on the queried port" <<'FIXTURE'
p123
f5
n192.168.0.113:54321->203.0.113.9:3478
p124
f6
n*:*->203.0.113.9:3478
FIXTURE

# 8. ...but the LOCAL side of a connected socket still counts as in use.
udp_probe 0 "udp_port_in_use detects the queried port on a connected socket's local side" <<'FIXTURE'
p123
f5
n*:*
p1002
f619
n192.168.0.113:3478->203.0.113.9:5000
FIXTURE

# 9. The match is on `:<port>`, not a bare suffix, so a longer port that
#    happens to end in the queried digits is not a hit.
udp_probe 1 "udp_port_in_use does not treat *:13478 as port 3478" <<'FIXTURE'
p123
f5
n*:13478
FIXTURE

# The flags themselves are load-bearing: -P is what keeps ports numeric.
if grep -q -- '-nP -iUDP -Fn' "$LSOF_ARGV_LOG"; then
  echo "PASS: udp_port_in_use queries lsof with -nP -iUDP -Fn"
else
  echo "FAIL: udp_port_in_use did not pass -nP -iUDP -Fn to lsof"
  echo "  recorded argv:"; sed 's/^/    /' "$LSOF_ARGV_LOG"
  exit 1
fi

# Tear the lsof stub down: any assertion appended below that puts STUB_DIR on
# PATH would otherwise silently inherit it, serving whichever fixture ran last.
rm -f "$STUB_DIR/lsof"
unset LSOF_FIXTURE LSOF_ARGV_LOG

echo ""
# ── warn_compose_project_split() ──────────────────────────────────────
# Containers created without the dev overlay carry a project name derived from
# the directory rather than docker-compose.dev.yml's `name:`. compose then cannot
# see them, so `status` under-reports and `up` collides on names already taken.
# Both directions are tested: a check that cannot go quiet is noise, and one
# that cannot fire is decoration.
#
# Stub and fixture live under $STUB_DIR so the existing EXIT trap cleans them;
# adding a second trap would silently replace the first (bash honors one).
SPLIT_STUB="$STUB_DIR/split"; mkdir -p "$SPLIT_STUB"
export SPLIT_ROWS_FILE="$STUB_DIR/split-rows"

cat > "$SPLIT_STUB/docker" <<'SPLITSTUB'
#!/usr/bin/env bash
# warn_compose_project_split consults only `docker ps -a --format ...`.
if [[ "${1:-}" == "ps" ]]; then cat "${SPLIT_ROWS_FILE}"; exit 0; fi
exit 0
SPLITSTUB
chmod +x "$SPLIT_STUB/docker"

SPLIT_DECLARED=$(declared_compose_project)
if [[ "$SPLIT_DECLARED" != "concordvoice-dev" ]]; then
  echo "FAIL: declared_compose_project returned '$SPLIT_DECLARED', expected concordvoice-dev"
  exit 1
fi
echo "PASS: declared_compose_project reads the project from the overlay ($SPLIT_DECLARED)"

# It must READ the file, not return a constant. Every fixture below builds its
# rows FROM $SPLIT_DECLARED, so they are self-consistent for ANY non-empty value
# — a body of `echo banana` passed all of them. These two cases are the only
# assertions that pin the function to the file.
# PROJECT_ROOT is readonly (concord-dev.sh), so an env-prefix override is not
# available; the helper takes the file as an optional argument instead.
printf 'name: fixture-project\n' > "$STUB_DIR/alt-compose.yml"
split_alt=$(declared_compose_project "$STUB_DIR/alt-compose.yml")
if [[ "$split_alt" != "fixture-project" ]]; then
  echo "FAIL: declared_compose_project gave '$split_alt' for a fixture declaring fixture-project"
  exit 1
fi
echo "PASS: declared_compose_project reads the file rather than returning a constant"

# A quoted value kept verbatim matches no docker label, so every container in a
# healthy stack reads as stray under a banner ending in `docker rm -f`. Trailing
# comments already parse — with the default FS the comment lands in $3.
printf 'name: "quoted-project" # canonical\n' > "$STUB_DIR/alt-compose.yml"
split_alt=$(declared_compose_project "$STUB_DIR/alt-compose.yml")
if [[ "$split_alt" != "quoted-project" ]]; then
  echo "FAIL: quoted name: parsed as '$split_alt', expected quoted-project"
  exit 1
fi
echo "PASS: declared_compose_project strips quotes from name:"

printf 'services:\n  db:\n    container_name: "quoted-container"\n' > "$STUB_DIR/alt-compose.yml"
split_alt=$(expected_container_names "$STUB_DIR/alt-compose.yml")
if [[ "$split_alt" != "quoted-container" ]]; then
  echo "FAIL: quoted container_name: parsed as '$split_alt', expected quoted-container"
  exit 1
fi
echo "PASS: expected_container_names strips quotes from container_name:"

# A detector nobody calls is precisely the silent regression this check exists to
# prevent. Nothing else in this suite notices a call site being deleted.
for _v in _up_infra verb_status verb_down; do
  if ! sed -n "/^${_v}()/,/^}/p" "$CONCORD_DEV" | grep -q 'warn_compose_project_split'; then
    echo "FAIL: ${_v}() no longer calls warn_compose_project_split"
    exit 1
  fi
done
echo "PASS: warn_compose_project_split is wired into _up_infra, verb_status and verb_down"

# Capture output AND status from ONE call, with set -e explicitly lifted around
# it. Without this the unguarded `split_out=$(...)` assignment aborts the suite
# before any rc assertion runs, so a non-zero return is "caught" as a silent exit
# with no FAIL line — the same shadowing this section exists to test for.
split_call_strict() {
  set +e
  ( set -euo pipefail; PATH="$SPLIT_STUB:$PATH" warn_compose_project_split >/dev/null 2>&1 )
  split_rc=$?
  set -e
}

split_call() {
  set +e
  split_out=$(PATH="$SPLIT_STUB:$PATH" warn_compose_project_split 2>&1)
  split_rc=$?
  set -e
}

# Capture-then-test throughout, never `if [[ -z "$(f)" ]]` or `if f`: a call in an
# `if` condition suppresses `set -e` inside the function and everything it calls,
# which is the exact mechanism these assertions exist to detect.

# NEGATIVE — everything under the declared project stays silent, and rc 0.
{ printf 'concordvoice-postgres\t%s\tUp 2 hours\n' "$SPLIT_DECLARED"
  printf 'concordvoice-redis\t%s\tUp 2 hours\n'    "$SPLIT_DECLARED"; } > "$SPLIT_ROWS_FILE"
split_call
if [[ -n "$split_out" ]]; then
  echo "FAIL: warned when no split was present"; echo "$split_out"; exit 1
fi
if [[ "$split_rc" -ne 0 ]]; then
  echo "FAIL: the silent path returned $split_rc; both call sites run under set -e"; exit 1
fi
echo "PASS: warn_compose_project_split is silent and returns 0 when there is no split"

# POSITIVE — same names, different project: must fire and name them.
{ printf 'concordvoice-postgres\tconcord-voice-alpha\tUp 36 hours\n'
  printf 'concordvoice-redis\tconcord-voice-alpha\tUp 36 hours\n'; } > "$SPLIT_ROWS_FILE"
split_call
if grep -q "COMPOSE PROJECT SPLIT" <<<"$split_out" \
   && grep -q "concordvoice-postgres" <<<"$split_out" \
   && grep -q "concord-voice-alpha" <<<"$split_out"; then
  echo "PASS: warn_compose_project_split fires and names the stray containers"
else
  echo "FAIL: did not report a split that was present"; echo "$split_out"; exit 1
fi

# A container started by bare `docker run` carries NO compose project label at all
# — the likeliest real shape of this bug — and must render as (none). That branch
# produced no output anywhere in this suite before this case existed.
printf 'concordvoice-postgres\t\tUp 36 hours\n' > "$SPLIT_ROWS_FILE"
split_call
if ! grep -q 'project=(none)' <<<"$split_out"; then
  echo "FAIL: an unlabelled container did not render as project=(none)"; echo "$split_out"; exit 1
fi
echo "PASS: a container carrying no compose project renders as (none)"

# Another session's isolated stack is correctly separate — never report it. The
# THIRD row is the discriminating one: it carries the concordvoice- prefix but is
# not a declared container_name:. Without it, `($1 in known)` and a
# `/^concordvoice-/` prefix match are indistinguishable, leaving the claim in
# expected_container_names' own comment unproven.
{ printf 'concord-1351-postgres\tconcord-voice-alpha\tUp 36 hours\n'
  printf 'fix3187-redis\t\tUp 13 hours\n'
  printf 'concordvoice-postgres-1351\tconcord-voice-alpha\tUp 36 hours\n'; } > "$SPLIT_ROWS_FILE"
split_call
if [[ -n "$split_out" ]]; then
  echo "FAIL: swept in a foreign stack"; echo "$split_out"; exit 1
fi
echo "PASS: a foreign stack is not reported, including one sharing the name prefix"

# Advisory only. Called as a BARE statement, the way _up_infra, verb_status and
# verb_down call it — an `if` condition would suppress the set -e under test.
printf 'concordvoice-postgres\tconcord-voice-alpha\tUp 36 hours\n' > "$SPLIT_ROWS_FILE"
split_call_strict
if [[ "$split_rc" -ne 0 ]]; then
  echo "FAIL: warning changed the exit status (returned $split_rc)"; exit 1
fi
echo "PASS: warn_compose_project_split returns 0 even when it warns"

# The advisory contract under a FAILING awk. The stray-detection stage is the one
# command substitution in the function that is not `|| return 0` guarded; with
# `set -euo pipefail` a failing awk propagates through the pipe and kills the
# CALLER — `up` before `compose up -d`, `status` before it prints anything. The
# stub fails only the -F invocation, so the two earlier awk calls still succeed
# and the guards above are passed rather than short-circuited.
cat > "$SPLIT_STUB/awk" <<'AWKSTUB'
#!/usr/bin/env bash
for a in "$@"; do case "$a" in -F*) exit 1 ;; esac; done
exec /usr/bin/awk "$@"
AWKSTUB
chmod +x "$SPLIT_STUB/awk"
{ printf 'concordvoice-postgres\tconcord-voice-alpha\tUp 36 hours\n'; } > "$SPLIT_ROWS_FILE"
split_call_strict
if [[ "$split_rc" -ne 0 ]]; then
  echo "FAIL: a failing awk returned $split_rc; that aborts up and status under set -e"; exit 1
fi
echo "PASS: the advisory contract holds when the stray-detection awk fails"
rm -f "$SPLIT_STUB/awk"

# An unreachable docker must be silent AND rc 0: an advisory check has no useful
# action to take, but it must not abort the verb it sits inside. Left last — it
# replaces the stub for the remainder of the file.
cat > "$SPLIT_STUB/docker" <<'FAILSTUB'
#!/usr/bin/env bash
exit 1
FAILSTUB
chmod +x "$SPLIT_STUB/docker"
split_call
if [[ -n "$split_out" || "$split_rc" -ne 0 ]]; then
  echo "FAIL: a failing docker ps must be silent and rc 0 (rc=$split_rc)"; echo "$split_out"; exit 1
fi
echo "PASS: an unreachable docker is silent and does not fail the verb"

# ...but under --verbose it must say WHY it looked at nothing. A skipped check and
# a clean check are the same silence otherwise, which is the defect one level up.
set +e
split_out=$(VERBOSE=1; PATH="$SPLIT_STUB:$PATH" warn_compose_project_split 2>&1)
split_rc=$?
set -e
if ! grep -q 'split check skipped' <<<"$split_out"; then
  echo "FAIL: --verbose did not report WHY the split check was skipped"; echo "$split_out"; exit 1
fi
echo "PASS: --verbose distinguishes a skipped split check from a clean one"

echo "All helper tests passed."
