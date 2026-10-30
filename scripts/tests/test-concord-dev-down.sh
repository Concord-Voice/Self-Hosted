#!/usr/bin/env bash
# Regression coverage for `down` cleanup and safe force recovery.
set -euo pipefail

# Homebrew Bash 5 on macOS can deadlock writing larger pipe-backed heredocs.
if [[ "$OSTYPE" == darwin* && ${BASH_VERSINFO[0]} -ge 5 ]]; then
  exec /bin/bash "$0" "$@"
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
CONCORD_DEV="$PROJECT_ROOT/scripts/concord-dev.sh"

TMP_DIR=$(mktemp -d)
TMP_DIR=$(cd "$TMP_DIR" && pwd -P)
STUB_DIR="$TMP_DIR/stubs"
DOCKER_LOG="$TMP_DIR/docker.log"
KILL_LOG="$TMP_DIR/kill.log"
KILL_MARKER="$TMP_DIR/listener-killed"
PS_SEEN_MARKER="$TMP_DIR/ps-seen"
EXITING_POLL_COUNT="$TMP_DIR/exiting-polls"
PS_STAT_CALL_COUNT="$TMP_DIR/ps-stat-calls"
PS_START_CALL_COUNT="$TMP_DIR/ps-start-calls"
mkdir -p "$STUB_DIR" "$TMP_DIR/logs"
trap 'rm -rf "$TMP_DIR"' EXIT
export DOCKER_LOG KILL_LOG KILL_MARKER PS_SEEN_MARKER EXITING_POLL_COUNT PS_STAT_CALL_COUNT PS_START_CALL_COUNT

cat > "$STUB_DIR/docker" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$DOCKER_LOG"
exit "${DOCKER_EXIT_CODE:-0}"
STUB

cat > "$STUB_DIR/lsof" <<'STUB'
#!/usr/bin/env bash
[[ "${LSOF_EXEC_FAIL:-0}" == 1 ]] && exit 2
if [[ "${LSOF_REQUIRE_WARNING_SUPPRESSION:-0}" == 1 && "$*" != *"-w"* ]]; then
  echo 'simulated lsof warning' >&2
  exit 0
fi

pid=""
port=""
previous=""
listener_query=0
for arg in "$@"; do
  [[ "$previous" == "-p" ]] && pid="$arg"
  [[ "$arg" =~ ^-p([0-9]+)$ ]] && pid="${BASH_REMATCH[1]}"
  [[ "$arg" =~ :([0-9]+) ]] && port="${BASH_REMATCH[1]}"
  [[ "$arg" == "-sTCP:LISTEN" ]] && listener_query=1
  previous="$arg"
done

if [[ -n "$pid" && -n "$port" && "$pid" == "${LSOF_LISTENER_PID:-}" ]]; then
  # An exiting listener can hold its socket for a few polls after TERM.
  if [[ -e "$KILL_MARKER" && -n "${LSOF_EXITING_LISTEN_POLLS:-}" ]]; then
    polls=0
    [[ -f "$EXITING_POLL_COUNT" ]] && read -r polls < "$EXITING_POLL_COUNT"
    polls=$((polls + 1))
    printf '%s\n' "$polls" > "$EXITING_POLL_COUNT"
    if [[ $polls -le $LSOF_EXITING_LISTEN_POLLS ]]; then
      printf '%s\n' "$pid"
      exit 0
    fi
  fi
  [[ -e "$KILL_MARKER" ]] && exit 1
  printf '%s\n' "$pid"
  exit 0
fi

if [[ -n "$pid" && "$pid" == "${LSOF_LISTENER_PID:-}" ]]; then
  printf 'p%s\nfcwd\nn%s\n' "$pid" "$LSOF_LISTENER_CWD"
  exit 0
fi

if [[ -n "$pid" && "$pid" == "${LSOF_PARENT_PID:-}" ]]; then
  printf 'p%s\nfcwd\nn%s\n' "$pid" "$LSOF_PARENT_CWD"
  exit 0
fi

if [[ -n "${LSOF_LISTENER_PID:-}" && "$port" == "${LSOF_LISTENER_PORT:-8080}" ]]; then
  [[ -e "$KILL_MARKER" ]] && exit 1
  printf '%s\n' "$LSOF_LISTENER_PID"
  exit 0
fi

if [[ -n "${LSOF_REMOTE_PORT:-}" && "$port" == "$LSOF_REMOTE_PORT" && $listener_query -eq 0 ]]; then
  exit 0
fi

if [[ "$*" == *"-iUDP"* && -n "${LSOF_UDP_LOCAL_PORT:-}" ]]; then
  printf 'p9001\nf9\nn*:%s\n' "$LSOF_UDP_LOCAL_PORT"
  exit 0
fi

if [[ "$*" == *"-iUDP"* && -n "${LSOF_UDP_REMOTE_PORT:-}" ]]; then
  printf 'p9002\nf9\nn127.0.0.1:55000->127.0.0.1:%s\n' "$LSOF_UDP_REMOTE_PORT"
  exit 0
fi

if [[ "$*" == *"-iUDP"* && "${LSOF_UDP_MALFORMED:-0}" == 1 ]]; then
  printf 'p9003\nf9\nnmalformed\n'
  exit 0
fi

if [[ -n "${LSOF_BUSY_PORT:-}" && "$port" == "$LSOF_BUSY_PORT" ]]; then
  printf '%s\n' 9999
  exit 0
fi
exit 1
STUB

cat > "$STUB_DIR/ps" <<'STUB'
#!/usr/bin/env bash
pid=""
previous=""
for arg in "$@"; do
  [[ "$previous" == "-p" ]] && pid="$arg"
  previous="$arg"
done
[[ "$pid" =~ ^[0-9]+$ ]] || exit 1
if [[ "$*" == *"ppid="* ]]; then
  [[ "$pid" == "${LSOF_LISTENER_PID:-}" && -n "${LISTENER_PARENT_PID:-}" ]] || exit 1
  printf '%s\n' "$LISTENER_PARENT_PID"
  exit 0
fi
if [[ -e "$KILL_MARKER" && "$pid" == "${LEGACY_STOPPED_PID:-}" ]]; then
  exit 1
fi

# After TERM, an exiting listener's thread-group leader is a zombie: `ps`
# reports it as `Z` with the command `[comm] <defunct>` until it is reaped.
exiting=0
if [[ "${PS_EXITING_AFTER_KILL:-0}" == 1 && -e "$KILL_MARKER" && "$pid" == "${LSOF_LISTENER_PID:-}" ]]; then
  exiting=1
fi
# PS_EXITING_PID models a leader that was already a zombie before down looked.
[[ -n "${PS_EXITING_PID:-}" && "$pid" == "$PS_EXITING_PID" ]] && exiting=1
if [[ "$*" == *"stat="* ]]; then
  # PS_STAT_LAG_CALLS models the leader turning into a zombie between probes:
  # the first N stat probes after TERM still read it as running, while every
  # later probe of any kind already sees the exiting process.
  if [[ $exiting -eq 1 && -n "${PS_STAT_LAG_CALLS:-}" && -e "$KILL_MARKER" ]]; then
    stat_calls=0
    [[ -f "$PS_STAT_CALL_COUNT" ]] && read -r stat_calls < "$PS_STAT_CALL_COUNT"
    stat_calls=$((stat_calls + 1))
    printf '%s\n' "$stat_calls" > "$PS_STAT_CALL_COUNT"
    if [[ $stat_calls -le $PS_STAT_LAG_CALLS ]]; then
      printf 'S\n'
      exit 0
    fi
  fi
  if [[ $exiting -eq 1 ]]; then
    printf 'Zl\n'
  else
    printf 'S\n'
  fi
  exit 0
fi

if [[ "$*" == *"command="* ]]; then
  if [[ $exiting -eq 1 ]]; then
    printf '[MainThread] <defunct>\n'
  elif [[ -n "${PS_COMMAND_AFTER_KILL:-}" && -e "$KILL_MARKER" ]]; then
    # The same process (same start time) reading a different command after
    # TERM, without ever exiting.
    printf '%s\n' "$PS_COMMAND_AFTER_KILL"
  elif [[ -n "${PS_COMMAND:-}" ]]; then
    printf '%s\n' "$PS_COMMAND"
  elif [[ "${LSOF_LISTENER_CWD:-}" == */services/media-plane ]]; then
    printf 'node ./node_modules/.bin/tsx watch src/index.ts\n'
  elif [[ "${LSOF_LISTENER_CWD:-}" == */client/desktop ]]; then
    printf 'node ./node_modules/.bin/vite --port 3001 --strictPort\n'
  else
    printf './bin/control-plane\n'
  fi
  exit 0
fi

# PS_START_GONE_AFTER models the listener being reaped mid-poll: after TERM the
# first N start-time probes still answer, and every later one finds no PID.
if [[ -n "${PS_START_GONE_AFTER:-}" && -e "$KILL_MARKER" && "$*" == *"lstart="* && "$pid" == "${LSOF_LISTENER_PID:-}" ]]; then
  start_calls=0
  [[ -f "$PS_START_CALL_COUNT" ]] && read -r start_calls < "$PS_START_CALL_COUNT"
  start_calls=$((start_calls + 1))
  printf '%s\n' "$start_calls" > "$PS_START_CALL_COUNT"
  [[ $start_calls -gt $PS_START_GONE_AFTER ]] && exit 1
fi

if [[ "$pid" == "${PS_REUSE_PID:-}" ]]; then
  seen=0
  [[ -f "$PS_SEEN_MARKER" ]] && read -r seen < "$PS_SEEN_MARKER"
  seen=$((seen + 1))
  printf '%s\n' "$seen" > "$PS_SEEN_MARKER"
  if [[ $seen -gt ${PS_REUSE_AFTER:-1} ]]; then
    printf 'replacement-%s\n' "$pid"
  else
    printf 'original-%s\n' "$pid"
  fi
else
  printf 'stable-%s\n' "$pid"
fi
STUB

cat > "$STUB_DIR/git" <<'STUB'
#!/usr/bin/env bash
cwd="$PWD"
previous=""
for arg in "$@"; do
  [[ "$previous" == "-C" ]] && cwd="$arg"
  previous="$arg"
done

[[ "$*" == *"--path-format=absolute"* ]] && exit 2

if [[ "$*" == *"worktree list --porcelain"* ]]; then
  printf 'worktree %s\n\n' "$TEST_PROJECT_ROOT"
  if [[ "${GIT_WORKTREE_LIST_OMIT_SAME:-0}" != 1 ]]; then
    printf 'worktree %s\n' "$TEST_SAME_WORKTREE"
    [[ "${GIT_WORKTREE_PRUNABLE_SAME:-0}" == 1 ]] && printf 'prunable stale registration\n'
    printf '\n'
  fi
  exit 0
fi

if [[ -n "${GIT_DIR:-}${GIT_WORK_TREE:-}${GIT_COMMON_DIR:-}" ]]; then
  if [[ "$*" == *"--show-toplevel"* ]]; then
    printf '%s\n' "$TEST_PROJECT_ROOT"
  else
    printf '%s\n' "$TEST_GIT_COMMON"
  fi
  exit 0
fi

if [[ "$*" == *"--show-toplevel"* ]]; then
  case "$cwd" in
    "$TEST_PROJECT_ROOT"|"$TEST_PROJECT_ROOT"/*) printf '%s\n' "$TEST_PROJECT_ROOT" ;;
    "$TEST_SAME_WORKTREE"|"$TEST_SAME_WORKTREE"/*) printf '%s\n' "$TEST_SAME_WORKTREE" ;;
    "$TEST_FOREIGN_WORKTREE"|"$TEST_FOREIGN_WORKTREE"/*) printf '%s\n' "$TEST_FOREIGN_WORKTREE" ;;
    *) exit 1 ;;
  esac
  exit 0
fi

case "$cwd" in
  "$TEST_PROJECT_ROOT"|"$TEST_PROJECT_ROOT"/*)
    printf '%s\n' "$TEST_GIT_COMMON"
    ;;
  "$TEST_SAME_WORKTREE"|"$TEST_SAME_WORKTREE"/*)
    printf '%s\n' "${TEST_SAME_COMMON:-$TEST_GIT_COMMON}"
    ;;
  "$TEST_FOREIGN_WORKTREE"|"$TEST_FOREIGN_WORKTREE"/*)
    printf '%s\n' "$TEST_FOREIGN_COMMON"
    ;;
  *) exit 1 ;;
esac
STUB

chmod +x "$STUB_DIR/docker" "$STUB_DIR/lsof" "$STUB_DIR/ps" "$STUB_DIR/git"

# `kill` is a bash builtin, so PATH cannot replace it. BASH_ENV installs safe
# no-op process stubs before the non-interactive target script starts.
cat > "$TMP_DIR/bash-env" <<'STUB'
kill() {
  printf 'kill %s\n' "$*" >> "$KILL_LOG"
  if [[ "$*" == "${KILL_FAIL_CALL:-}" ]]; then
    [[ "${KILL_DISAPPEAR_ON_FAIL:-0}" == 1 ]] && : > "$KILL_MARKER"
    return 1
  fi
  [[ "$*" == "${KILL_MARK_CALL:-}" ]] && : > "$KILL_MARKER"
  [[ "$*" == "-9 4242" || "$*" == "-TERM 8484" || "$*" == "-9 8585" ]] && : > "$KILL_MARKER"
  return 0
}
pkill() {
  printf 'pkill %s\n' "$*" >> "$KILL_LOG"
  return 0
}
sleep() {
  [[ "${STUB_SLEEP:-0}" == 1 ]] && return 0
  command sleep "$@"
}
export -f kill pkill sleep
STUB

set +e
BASH_ENV="$TMP_DIR/bash-env" \
  LOG_DIR="$TMP_DIR/logs" \
  LSOF_BUSY_PORT=8080 \
  PATH="$STUB_DIR:$PATH" \
  "$CONCORD_DEV" down >"$TMP_DIR/busy-port.out" 2>&1
rc=$?
set -e

if [[ $rc -eq 0 ]] || ! grep -Fq 'Port 8080 still in use' "$TMP_DIR/busy-port.out"; then
  echo "FAIL: down did not report busy port 8080"
  exit 1
fi

echo "PASS: down exits nonzero while port 8080 remains busy"

# Run from a hermetic project copy with no .env. Compose teardown must bypass
# implicit project-env loading by naming the tracked, parse-safe example file.
FIXTURE_ROOT="$TMP_DIR/project"
mkdir -p "$FIXTURE_ROOT/scripts" "$FIXTURE_ROOT/logs" "$FIXTURE_ROOT/infrastructure/deploy"
cp "$CONCORD_DEV" "$FIXTURE_ROOT/scripts/concord-dev.sh"
cp "$PROJECT_ROOT/infrastructure/deploy/voice-boot-containment.py" \
  "$FIXTURE_ROOT/infrastructure/deploy/voice-boot-containment.py"
if [[ ! -f "$PROJECT_ROOT/.env.example" ]]; then
  echo "FAIL: repository .env.example fixture is missing"
  exit 1
fi
cp "$PROJECT_ROOT/.env.example" "$FIXTURE_ROOT/.env.example"
touch "$FIXTURE_ROOT/docker-compose.yml" "$FIXTURE_ROOT/docker-compose.dev.yml"
: > "$DOCKER_LOG"

BASH_ENV="$TMP_DIR/bash-env" \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  PATH="$STUB_DIR:$PATH" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down >/dev/null 2>&1

if grep -Fq -- "--env-file $FIXTURE_ROOT/.env.example" "$DOCKER_LOG"; then
  echo "PASS: down uses .env.example explicitly when project .env is absent"
else
  echo "FAIL: down did not pass --env-file $FIXTURE_ROOT/.env.example to Docker Compose"
  echo "docker argv: $(tr '\n' ';' < "$DOCKER_LOG")"
  exit 1
fi

# A stale .env may be missing newly required keys. Load the tracked example as
# defaults first, then the developer's file as the higher-precedence override.
: > "$FIXTURE_ROOT/.env"
: > "$DOCKER_LOG"
BASH_ENV="$TMP_DIR/bash-env" \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  PATH="$STUB_DIR:$PATH" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down >/dev/null 2>&1

if grep -Fq -- "--env-file $FIXTURE_ROOT/.env.example --env-file $FIXTURE_ROOT/.env down" "$DOCKER_LOG"; then
  echo "PASS: down fills stale .env keys from .env.example before applying overrides"
else
  echo "FAIL: down did not layer .env over .env.example"
  echo "docker argv: $(tr '\n' ';' < "$DOCKER_LOG")"
  exit 1
fi
rm -f "$FIXTURE_ROOT/.env"

# Teardown must still reach Docker Compose in a partial checkout containing no
# environment files; Compose can apply defaults from the manifest itself.
rm -f "$FIXTURE_ROOT/.env.example"
: > "$DOCKER_LOG"
BASH_ENV="$TMP_DIR/bash-env" \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  PATH="$STUB_DIR:$PATH" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down >/dev/null 2>&1

if grep -Fq -- '--env-file' "$DOCKER_LOG" || ! grep -Fq -- 'down' "$DOCKER_LOG"; then
  echo "FAIL: down did not run Compose without missing environment files"
  exit 1
fi
echo "PASS: down omits absent environment files"

# lsof warnings must never become parsed match output.
set +e
BASH_ENV="$TMP_DIR/bash-env" \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_REQUIRE_WARNING_SUPPRESSION=1 \
  PATH="$STUB_DIR:$PATH" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down --keep-docker >/dev/null 2>&1
rc=$?
set -e

if [[ $rc -ne 0 ]]; then
  echo "FAIL: down parsed lsof warnings as listener output"
  exit 1
fi
echo "PASS: down keeps lsof warnings out of listener output"

# A teardown command that never reached Docker must not be reported as success.
set +e
BASH_ENV="$TMP_DIR/bash-env" \
  DOCKER_EXIT_CODE=1 \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  PATH="$STUB_DIR:$PATH" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down >/dev/null 2>&1
rc=$?
set -e

if [[ $rc -eq 0 ]]; then
  echo "FAIL: down returned 0 after Docker Compose teardown failed"
  exit 1
fi

echo "PASS: down exits nonzero after Docker Compose teardown fails"

# Keeping Docker intentionally leaves coturn on 3478; that is not failed cleanup.
set +e
BASH_ENV="$TMP_DIR/bash-env" \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT=3478 \
  PATH="$STUB_DIR:$PATH" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down --keep-docker >/dev/null 2>&1
rc=$?
set -e

if [[ $rc -ne 0 ]]; then
  echo "FAIL: down --keep-docker treated retained port 3478 as failed cleanup"
  exit 1
fi

echo "PASS: down --keep-docker allows retained port 3478"

# Coturn publishes UDP as well as TCP. A local UDP-only bind on 3478 is still
# failed cleanup, while an outbound UDP connection to remote 3478 is not.
set +e
BASH_ENV="$TMP_DIR/bash-env" \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_UDP_LOCAL_PORT=3478 \
  PATH="$STUB_DIR:$PATH" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down >/dev/null 2>&1
rc=$?
set -e

if [[ $rc -eq 0 ]]; then
  echo "FAIL: down ignored a local UDP bind on coturn port 3478"
  exit 1
fi

set +e
BASH_ENV="$TMP_DIR/bash-env" \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_UDP_REMOTE_PORT=3478 \
  PATH="$STUB_DIR:$PATH" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down >/dev/null 2>&1
rc=$?
set -e

if [[ $rc -ne 0 ]]; then
  echo "FAIL: down treated outbound UDP to remote port 3478 as a local bind"
  exit 1
fi

echo "PASS: down distinguishes local and remote UDP port 3478"

# Unexpected lsof endpoint records are probe failures, not proof that the UDP
# port is free.
set +e
BASH_ENV="$TMP_DIR/bash-env" \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_UDP_MALFORMED=1 \
  PATH="$STUB_DIR:$PATH" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down >/dev/null 2>&1
rc=$?
set -e

if [[ $rc -eq 0 ]]; then
  echo "FAIL: down treated malformed UDP ownership output as a free port"
  exit 1
fi
echo "PASS: down fails closed on malformed UDP ownership output"

# An outbound connection to a remote service port is not a local listener and
# must not make cleanup fail.
set +e
BASH_ENV="$TMP_DIR/bash-env" \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_REMOTE_PORT=8080 \
  PATH="$STUB_DIR:$PATH" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down >/dev/null 2>&1
rc=$?
set -e

if [[ $rc -ne 0 ]]; then
  echo "FAIL: down treated an outbound connection to remote port 8080 as a listener"
  exit 1
fi

echo "PASS: down ignores outbound connections to expected service ports"

regression_failures=0

# An lsof execution error is not evidence that a port is free. Teardown must
# fail closed so the caller knows cleanup could not be verified.
set +e
BASH_ENV="$TMP_DIR/bash-env" \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_EXEC_FAIL=1 \
  PATH="$STUB_DIR:$PATH" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down >/dev/null 2>&1
rc=$?
set -e

if [[ $rc -ne 0 ]]; then
  echo "PASS: down fails when lsof cannot verify port state"
else
  echo "FAIL: down treated an lsof execution error as a free port"
  regression_failures=$((regression_failures + 1))
fi

# With no PID file, explicit --force may recover a native listener from a
# sibling worktree only when it belongs to this repository and service path.
SAME_WORKTREE="$TMP_DIR/sibling-worktree"
TEST_GIT_COMMON="$TMP_DIR/common.git"
mkdir -p \
  "$FIXTURE_ROOT/services/control-plane" \
  "$FIXTURE_ROOT/services/media-plane" \
  "$FIXTURE_ROOT/client/desktop" \
  "$SAME_WORKTREE/services/control-plane" \
  "$TEST_GIT_COMMON"
: > "$KILL_LOG"
rm -f "$KILL_MARKER" "$PS_SEEN_MARKER"

# Neither a legacy media watcher record nor an orphan listener binds the
# retained executable closure to a boot. Both plain and forced down must keep
# the control plane, Docker transport, and closure for operator recovery.
for media_shape in legacy-watcher orphan-listener; do
  for force_flag in '' --force; do
    mkdir -p "$FIXTURE_ROOT/logs/voice-media-runtime" "$FIXTURE_ROOT/logs/voice-control-runtime"
    printf 'retained-media\n' > "$FIXTURE_ROOT/logs/voice-media-runtime/closure"
    printf 'retained-control\n' > "$FIXTURE_ROOT/logs/voice-control-runtime/closure"
    printf '7777\nstable-7777\n%s\n' "$FIXTURE_ROOT/services/control-plane" \
      > "$FIXTURE_ROOT/logs/control-plane.pid"
    if [[ "$media_shape" == legacy-watcher ]]; then
      printf '5555\noriginal-5555\n%s\n' "$FIXTURE_ROOT/services/media-plane" \
        > "$FIXTURE_ROOT/logs/media-plane.pid"
      media_listener=5656
    else
      rm -f "$FIXTURE_ROOT/logs/media-plane.pid"
      media_listener=8484
    fi
    : > "$KILL_LOG"
    : > "$DOCKER_LOG"
    rm -f "$KILL_MARKER" "$PS_SEEN_MARKER"
    set +e
    BASH_ENV="$TMP_DIR/bash-env" \
      KILL_MARK_CALL='-TERM 5656' \
      LEGACY_STOPPED_PID=5555 \
      LOG_DIR="$FIXTURE_ROOT/logs" \
      LSOF_BUSY_PORT='' \
      LSOF_LISTENER_PID="$media_listener" \
      LSOF_LISTENER_PORT=3000 \
      LSOF_LISTENER_CWD="$FIXTURE_ROOT/services/media-plane" \
      LSOF_PARENT_CWD="$FIXTURE_ROOT/services/media-plane" \
      LSOF_PARENT_PID=5555 \
      PATH="$STUB_DIR:$PATH" \
      PS_REUSE_AFTER=1 \
      PS_REUSE_PID=5555 \
      TEST_GIT_COMMON="$TEST_GIT_COMMON" \
      TEST_PROJECT_ROOT="$FIXTURE_ROOT" \
      TEST_SAME_WORKTREE="$SAME_WORKTREE" \
      "$FIXTURE_ROOT/scripts/concord-dev.sh" --quiet down ${force_flag:+"$force_flag"} \
      >"$TMP_DIR/media-refusal.out" 2>&1
    rc=$?
    set -e
    if [[ $rc -ne 0 && ! -s "$KILL_LOG" && ! -s "$DOCKER_LOG" && ! -e "$KILL_MARKER" ]] \
      && [[ "$(cat "$FIXTURE_ROOT/logs/control-plane.pid")" == $'7777\nstable-7777\n'"$FIXTURE_ROOT/services/control-plane" ]] \
      && [[ "$(cat "$FIXTURE_ROOT/logs/voice-media-runtime/closure")" == retained-media ]] \
      && [[ "$(cat "$FIXTURE_ROOT/logs/voice-control-runtime/closure")" == retained-control ]] \
      && grep -Fq 'voice lifecycle: media retirement unverified; control plane and executable closure retained' "$TMP_DIR/media-refusal.out" \
      && { [[ "$media_shape" == orphan-listener && ! -e "$FIXTURE_ROOT/logs/media-plane.pid" ]] \
        || { grep -Fq 'voice lifecycle: observation refused' "$TMP_DIR/media-refusal.out" \
          && [[ "$media_shape" == legacy-watcher && "$(cat "$FIXTURE_ROOT/logs/media-plane.pid")" == $'5555\noriginal-5555\n'"$FIXTURE_ROOT/services/media-plane" ]]; }; }; then
      echo "PASS: down ${force_flag:-plain} retains media $media_shape without boot binding"
    else
      echo "FAIL: down ${force_flag:-plain} mishandled media $media_shape without boot binding (rc=$rc)"
      sed 's/^/  /' "$TMP_DIR/media-refusal.out"
      exit 1
    fi
  done
done
rm -rf "$FIXTURE_ROOT/logs/voice-media-runtime" "$FIXTURE_ROOT/logs/voice-control-runtime"
rm -f "$FIXTURE_ROOT/logs/media-plane.pid" "$FIXTURE_ROOT/logs/control-plane.pid"
: > "$KILL_LOG"
rm -f "$KILL_MARKER" "$PS_SEEN_MARKER"

# Generic watcher/listener cleanup remains supported for Vite. Media now
# requires its exact boot/artifact association and a verified retirement fence;
# legacy media watchers must never inherit these generic recovery expectations.
# A tracked watcher can exit while its listening child survives. If the
# verified listener cleanup also proves the watcher is now gone, reconcile the
# stale identity-bound record instead of preserving the earlier race as a
# teardown failure.
printf '%s\n%s\n%s\n' \
  5555 \
  original-5555 \
  "$FIXTURE_ROOT/client/desktop" > "$FIXTURE_ROOT/logs/vite-renderer.pid"
set +e
BASH_ENV="$TMP_DIR/bash-env" \
  KILL_MARK_CALL='-TERM 5656' \
  LEGACY_STOPPED_PID=5555 \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_LISTENER_PID=5656 \
  LSOF_LISTENER_PORT=3001 \
  LSOF_LISTENER_CWD="$FIXTURE_ROOT/client/desktop" \
  LSOF_PARENT_CWD="$FIXTURE_ROOT/client/desktop" \
  LSOF_PARENT_PID=5555 \
  PATH="$STUB_DIR:$PATH" \
  PS_REUSE_AFTER=1 \
  PS_REUSE_PID=5555 \
  TEST_GIT_COMMON="$TEST_GIT_COMMON" \
  TEST_PROJECT_ROOT="$FIXTURE_ROOT" \
  TEST_SAME_WORKTREE="$SAME_WORKTREE" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" --quiet down >"$TMP_DIR/watcher-exit.out" 2>&1
rc=$?
set -e

watcher_exit_calls=$(grep '^kill ' "$KILL_LOG" || true)
if [[ $rc -eq 0 && "$watcher_exit_calls" == $'kill -TERM 5555\nkill -TERM 5656' && \
  ! -e "$FIXTURE_ROOT/logs/vite-renderer.pid" ]]; then
  echo "PASS: down (Vite) reconciles a watcher exit after verified listener cleanup"
else
  echo "FAIL: down (Vite) preserved a resolved watcher exit (rc=$rc, calls=${watcher_exit_calls:-none})"
  sed 's/^/  /' "$TMP_DIR/watcher-exit.out"
  regression_failures=$((regression_failures + 1))
fi
rm -f "$FIXTURE_ROOT/logs/vite-renderer.pid"

: > "$KILL_LOG"
rm -f "$KILL_MARKER" "$PS_SEEN_MARKER"

# A tracked watcher can exit before its listening child. Plain down may stop
# that exact current-worktree service child gracefully without broad pkill.
set +e
BASH_ENV="$TMP_DIR/bash-env" \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_LISTENER_PID=8484 \
  LSOF_LISTENER_PORT=3001 \
  LSOF_LISTENER_CWD="$FIXTURE_ROOT/client/desktop" \
  PATH="$STUB_DIR:$PATH" \
  TEST_GIT_COMMON="$TEST_GIT_COMMON" \
  TEST_PROJECT_ROOT="$FIXTURE_ROOT" \
  TEST_SAME_WORKTREE="$SAME_WORKTREE" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" --quiet down >/dev/null 2>&1
rc=$?
set -e

current_listener_kills=$(grep '^kill ' "$KILL_LOG" || true)
if [[ $rc -eq 0 && "$current_listener_kills" == "kill -TERM 8484" && -e "$KILL_MARKER" ]]; then
  echo "PASS: down (Vite) gracefully stops an orphaned current-worktree listener"
else
  echo "FAIL: down (Vite) did not stop current-worktree listener 8484 (rc=$rc, calls=${current_listener_kills:-none})"
  exit 1
fi

# A listener merely launched from a service directory is not necessarily that
# service. Plain down must not signal an unrelated command.
: > "$KILL_LOG"
rm -f "$KILL_MARKER" "$PS_SEEN_MARKER"
set +e
BASH_ENV="$TMP_DIR/bash-env" \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_LISTENER_PID=8494 \
  LSOF_LISTENER_PORT=3001 \
  LSOF_LISTENER_CWD="$FIXTURE_ROOT/client/desktop" \
  PATH="$STUB_DIR:$PATH" \
  PS_COMMAND='python -m http.server 3001' \
  TEST_GIT_COMMON="$TEST_GIT_COMMON" \
  TEST_PROJECT_ROOT="$FIXTURE_ROOT" \
  TEST_SAME_WORKTREE="$SAME_WORKTREE" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" --quiet down >"$TMP_DIR/unrelated-command.out" 2>&1
rc=$?
set -e

if [[ $rc -ne 0 && ! -s "$KILL_LOG" ]] && grep -Fq 'PID 8494 on port 3001' "$TMP_DIR/unrelated-command.out"; then
  echo "PASS: down (Vite) rejects an unrelated current-worktree listener"
else
  echo "FAIL: down (Vite) signaled or silently rejected unrelated listener 8494"
  regression_failures=$((regression_failures + 1))
fi

# A stubborn current-worktree listener gets one graceful attempt followed by
# one escalation, with identity checks guarding both signals.
: > "$KILL_LOG"
rm -f "$KILL_MARKER" "$PS_SEEN_MARKER"
set +e
BASH_ENV="$TMP_DIR/bash-env" \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_LISTENER_PID=8585 \
  LSOF_LISTENER_PORT=3001 \
  LSOF_LISTENER_CWD="$FIXTURE_ROOT/client/desktop" \
  PATH="$STUB_DIR:$PATH" \
  STUB_SLEEP=1 \
  TEST_GIT_COMMON="$TEST_GIT_COMMON" \
  TEST_PROJECT_ROOT="$FIXTURE_ROOT" \
  TEST_SAME_WORKTREE="$SAME_WORKTREE" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" --quiet down >/dev/null 2>&1
rc=$?
set -e

stubborn_listener_calls=$(grep '^kill ' "$KILL_LOG" || true)
if [[ $rc -eq 0 && "$stubborn_listener_calls" == $'kill -TERM 8585\nkill -9 8585' && -e "$KILL_MARKER" ]]; then
  echo "PASS: down (Vite) escalates a stubborn current-worktree listener TERM to KILL"
else
  echo "FAIL: down (Vite) mishandled stubborn listener 8585 (rc=$rc, calls=${stubborn_listener_calls:-none})"
  regression_failures=$((regression_failures + 1))
fi

# A listener can disappear between its final identity check and signal. A
# failed signal followed by confirmed absence is successful cleanup.
: > "$KILL_LOG"
rm -f "$KILL_MARKER" "$PS_SEEN_MARKER"
set +e
BASH_ENV="$TMP_DIR/bash-env" \
  KILL_DISAPPEAR_ON_FAIL=1 \
  KILL_FAIL_CALL='-9 8787' \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_LISTENER_PID=8787 \
  LSOF_LISTENER_PORT=3001 \
  LSOF_LISTENER_CWD="$FIXTURE_ROOT/client/desktop" \
  PATH="$STUB_DIR:$PATH" \
  TEST_GIT_COMMON="$TEST_GIT_COMMON" \
  TEST_PROJECT_ROOT="$FIXTURE_ROOT" \
  TEST_SAME_WORKTREE="$SAME_WORKTREE" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" --quiet down --force >/dev/null 2>&1
rc=$?
set -e

listener_exit_race_calls=$(grep '^kill ' "$KILL_LOG" || true)
if [[ $rc -eq 0 && "$listener_exit_race_calls" == "kill -9 8787" && -e "$KILL_MARKER" ]]; then
  echo "PASS: down (Vite) accepts confirmed listener exit after a failed signal"
else
  echo "FAIL: down (Vite) mishandled listener exit race 8787 (rc=$rc, calls=${listener_exit_race_calls:-none})"
  regression_failures=$((regression_failures + 1))
fi

# After TERM, a node listener's thread-group leader becomes a zombie before it
# is reaped. For that window `ps` reports its command as `[MainThread] <defunct>`
# while lsof still resolves its cwd, so a command comparison reads as PID reuse.
# It is not: the kernel does not recycle a PID until the zombie is reaped. The
# unit-smoke job failed `down` this way on 2026-09-30 ("identity changed after
# SIGTERM"), and the same state was measured locally on a real node process.
: > "$KILL_LOG"
rm -f "$KILL_MARKER" "$PS_SEEN_MARKER" "$EXITING_POLL_COUNT"
set +e
BASH_ENV="$TMP_DIR/bash-env" \
  KILL_MARK_CALL='-TERM 8690' \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_LISTENER_PID=8690 \
  LSOF_LISTENER_PORT=3001 \
  LSOF_LISTENER_CWD="$FIXTURE_ROOT/client/desktop" \
  PATH="$STUB_DIR:$PATH" \
  PS_EXITING_AFTER_KILL=1 \
  STUB_SLEEP=1 \
  TEST_GIT_COMMON="$TEST_GIT_COMMON" \
  TEST_PROJECT_ROOT="$FIXTURE_ROOT" \
  TEST_SAME_WORKTREE="$SAME_WORKTREE" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down >"$TMP_DIR/exiting-listener.out" 2>&1
rc=$?
set -e

exiting_listener_calls=$(grep '^kill ' "$KILL_LOG" || true)
if [[ $rc -eq 0 && "$exiting_listener_calls" == "kill -TERM 8690" ]] \
  && ! grep -Fq 'identity changed' "$TMP_DIR/exiting-listener.out"; then
  echo "PASS: down (Vite) accepts a listener that is exiting as a zombie after TERM"
else
  echo "FAIL: down (Vite) rejected exiting listener 8690 (rc=$rc, calls=${exiting_listener_calls:-none})"
  grep -F 'PID 8690' "$TMP_DIR/exiting-listener.out" || true
  regression_failures=$((regression_failures + 1))
fi

# An exiting leader whose other threads still hold the socket is still the
# listener. down waits for the port to be released, and does not escalate.
: > "$KILL_LOG"
rm -f "$KILL_MARKER" "$PS_SEEN_MARKER" "$EXITING_POLL_COUNT"
set +e
BASH_ENV="$TMP_DIR/bash-env" \
  KILL_MARK_CALL='-TERM 8691' \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_EXITING_LISTEN_POLLS=2 \
  LSOF_LISTENER_PID=8691 \
  LSOF_LISTENER_PORT=3001 \
  LSOF_LISTENER_CWD="$FIXTURE_ROOT/client/desktop" \
  PATH="$STUB_DIR:$PATH" \
  PS_EXITING_AFTER_KILL=1 \
  STUB_SLEEP=1 \
  TEST_GIT_COMMON="$TEST_GIT_COMMON" \
  TEST_PROJECT_ROOT="$FIXTURE_ROOT" \
  TEST_SAME_WORKTREE="$SAME_WORKTREE" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" --quiet down >/dev/null 2>&1
rc=$?
set -e

exiting_holder_calls=$(grep '^kill ' "$KILL_LOG" || true)
exiting_holder_polls=0
[[ -f "$EXITING_POLL_COUNT" ]] && read -r exiting_holder_polls < "$EXITING_POLL_COUNT"
if [[ $rc -eq 0 && "$exiting_holder_calls" == "kill -TERM 8691" && $exiting_holder_polls -ge 3 ]]; then
  echo "PASS: down (Vite) waits for an exiting listener to release its port"
else
  echo "FAIL: down (Vite) mishandled exiting listener 8691 (rc=$rc, calls=${exiting_holder_calls:-none}, polls=$exiting_holder_polls)"
  regression_failures=$((regression_failures + 1))
fi

# A zombie at the listener's PID whose start time differs is not the listener:
# the original was reaped and the PID reused. That still fails closed.
: > "$KILL_LOG"
rm -f "$KILL_MARKER" "$PS_SEEN_MARKER" "$EXITING_POLL_COUNT"
set +e
BASH_ENV="$TMP_DIR/bash-env" \
  KILL_MARK_CALL='-TERM 8692' \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_LISTENER_PID=8692 \
  LSOF_LISTENER_PORT=3001 \
  LSOF_LISTENER_CWD="$FIXTURE_ROOT/client/desktop" \
  PATH="$STUB_DIR:$PATH" \
  PS_EXITING_AFTER_KILL=1 \
  PS_REUSE_AFTER=2 \
  PS_REUSE_PID=8692 \
  STUB_SLEEP=1 \
  TEST_GIT_COMMON="$TEST_GIT_COMMON" \
  TEST_PROJECT_ROOT="$FIXTURE_ROOT" \
  TEST_SAME_WORKTREE="$SAME_WORKTREE" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" --quiet down >/dev/null 2>&1
rc=$?
set -e

exiting_reuse_calls=$(grep '^kill ' "$KILL_LOG" || true)
if [[ $rc -ne 0 && "$exiting_reuse_calls" == "kill -TERM 8692" ]]; then
  echo "PASS: down (Vite) rejects a zombie at a reused listener PID"
else
  echo "FAIL: down (Vite) accepted a zombie at reused listener PID 8692 (rc=$rc, calls=${exiting_reuse_calls:-none})"
  regression_failures=$((regression_failures + 1))
fi

# The leader can become a zombie between the stat probe and the identity probes
# after it: stat still reads it as running while its command already reads
# `[MainThread] <defunct>`. It is the same exiting listener, so down rechecks
# the exit state instead of reporting an identity change.
: > "$KILL_LOG"
rm -f "$KILL_MARKER" "$PS_SEEN_MARKER" "$EXITING_POLL_COUNT" "$PS_STAT_CALL_COUNT"
set +e
BASH_ENV="$TMP_DIR/bash-env" \
  KILL_MARK_CALL='-TERM 8693' \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_LISTENER_PID=8693 \
  LSOF_LISTENER_PORT=3001 \
  LSOF_LISTENER_CWD="$FIXTURE_ROOT/client/desktop" \
  PATH="$STUB_DIR:$PATH" \
  PS_EXITING_AFTER_KILL=1 \
  PS_STAT_LAG_CALLS=1 \
  STUB_SLEEP=1 \
  TEST_GIT_COMMON="$TEST_GIT_COMMON" \
  TEST_PROJECT_ROOT="$FIXTURE_ROOT" \
  TEST_SAME_WORKTREE="$SAME_WORKTREE" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down >"$TMP_DIR/exiting-race.out" 2>&1
rc=$?
set -e

exiting_race_calls=$(grep '^kill ' "$KILL_LOG" || true)
exiting_race_stats=0
[[ -f "$PS_STAT_CALL_COUNT" ]] && read -r exiting_race_stats < "$PS_STAT_CALL_COUNT"
# At least two stat probes after TERM: the lagging one this case exists for,
# and the recheck that sees the zombie.
if [[ $rc -eq 0 && "$exiting_race_calls" == "kill -TERM 8693" && $exiting_race_stats -ge 2 ]] \
  && ! grep -Fq 'identity changed' "$TMP_DIR/exiting-race.out"; then
  echo "PASS: down (Vite) rechecks the exit state when the leader exits between probes"
else
  echo "FAIL: down (Vite) rejected listener 8693 that exited between probes (rc=$rc, calls=${exiting_race_calls:-none}, stat probes=$exiting_race_stats)"
  grep -F 'PID 8693' "$TMP_DIR/exiting-race.out" || true
  regression_failures=$((regression_failures + 1))
fi

# Regression from #3515's own CI run: a node listener tearing down after TERM
# loses its command line before `ps` reports it as a zombie, and that window
# can outlast both the first stat probe and the recheck after the identity
# mismatch. It keeps its start time throughout, so it is the same process,
# still exiting: down waits for it rather than reporting an identity change.
: > "$KILL_LOG"
rm -f "$KILL_MARKER" "$PS_SEEN_MARKER" "$EXITING_POLL_COUNT" "$PS_STAT_CALL_COUNT"
set +e
BASH_ENV="$TMP_DIR/bash-env" \
  KILL_MARK_CALL='-TERM 8694' \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_LISTENER_PID=8694 \
  LSOF_LISTENER_PORT=3001 \
  LSOF_LISTENER_CWD="$FIXTURE_ROOT/client/desktop" \
  PATH="$STUB_DIR:$PATH" \
  PS_EXITING_AFTER_KILL=1 \
  PS_STAT_LAG_CALLS=3 \
  STUB_SLEEP=1 \
  TEST_GIT_COMMON="$TEST_GIT_COMMON" \
  TEST_PROJECT_ROOT="$FIXTURE_ROOT" \
  TEST_SAME_WORKTREE="$SAME_WORKTREE" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down >"$TMP_DIR/slow-teardown.out" 2>&1
rc=$?
set -e

slow_teardown_calls=$(grep '^kill ' "$KILL_LOG" || true)
slow_teardown_stats=0
[[ -f "$PS_STAT_CALL_COUNT" ]] && read -r slow_teardown_stats < "$PS_STAT_CALL_COUNT"
# Three lagging stat probes, then at least one that sees the zombie: the
# teardown spans a full poll, not only the recheck.
if [[ $rc -eq 0 && "$slow_teardown_calls" == "kill -TERM 8694" && $slow_teardown_stats -ge 4 ]] \
  && ! grep -Fq 'identity changed' "$TMP_DIR/slow-teardown.out"; then
  echo "PASS: down (Vite) waits out a teardown that outlasts the exit-state recheck"
else
  echo "FAIL: down (Vite) rejected listener 8694 whose teardown outlasted the recheck (rc=$rc, calls=${slow_teardown_calls:-none}, stat probes=$slow_teardown_stats)"
  grep -F 'PID 8694' "$TMP_DIR/slow-teardown.out" || true
  regression_failures=$((regression_failures + 1))
fi

# The wait above re-reads the start time, and the listener can be reaped
# between the identity probe and that read. A PID that is gone is a listener
# that stopped, not one whose identity changed.
: > "$KILL_LOG"
rm -f "$KILL_MARKER" "$PS_SEEN_MARKER" "$EXITING_POLL_COUNT" "$PS_STAT_CALL_COUNT" "$PS_START_CALL_COUNT"
set +e
BASH_ENV="$TMP_DIR/bash-env" \
  KILL_MARK_CALL='-TERM 8696' \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_LISTENER_PID=8696 \
  LSOF_LISTENER_PORT=3001 \
  LSOF_LISTENER_CWD="$FIXTURE_ROOT/client/desktop" \
  PATH="$STUB_DIR:$PATH" \
  PS_EXITING_AFTER_KILL=1 \
  PS_STAT_LAG_CALLS=99 \
  PS_START_GONE_AFTER=1 \
  STUB_SLEEP=1 \
  TEST_GIT_COMMON="$TEST_GIT_COMMON" \
  TEST_PROJECT_ROOT="$FIXTURE_ROOT" \
  TEST_SAME_WORKTREE="$SAME_WORKTREE" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down >"$TMP_DIR/reaped-mid-poll.out" 2>&1
rc=$?
set -e

reaped_calls=$(grep '^kill ' "$KILL_LOG" || true)
reaped_starts=0
[[ -f "$PS_START_CALL_COUNT" ]] && read -r reaped_starts < "$PS_START_CALL_COUNT"
# Two start-time probes after TERM: the identity probe that still answered,
# and the wait's re-read that found the PID gone.
if [[ $rc -eq 0 && "$reaped_calls" == "kill -TERM 8696" && $reaped_starts -ge 2 ]] \
  && ! grep -Fq 'identity changed' "$TMP_DIR/reaped-mid-poll.out"; then
  echo "PASS: down (Vite) accepts a listener reaped while it waits out the teardown"
else
  echo "FAIL: down (Vite) rejected listener 8696 reaped mid-poll (rc=$rc, calls=${reaped_calls:-none}, start probes=$reaped_starts)"
  grep -F 'PID 8696' "$TMP_DIR/reaped-mid-poll.out" || true
  regression_failures=$((regression_failures + 1))
fi

# Control for waiting out a teardown: a process that keeps its start time but
# reads a different command after TERM, and never exits or releases the port,
# is not a listener down may escalate against. It must fail closed with no
# SIGKILL, however long down waits.
: > "$KILL_LOG"
rm -f "$KILL_MARKER" "$PS_SEEN_MARKER" "$EXITING_POLL_COUNT" "$PS_STAT_CALL_COUNT"
set +e
BASH_ENV="$TMP_DIR/bash-env" \
  KILL_MARK_CALL='-TERM 8695' \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_EXITING_LISTEN_POLLS=99 \
  LSOF_LISTENER_PID=8695 \
  LSOF_LISTENER_PORT=3001 \
  LSOF_LISTENER_CWD="$FIXTURE_ROOT/client/desktop" \
  PATH="$STUB_DIR:$PATH" \
  PS_COMMAND_AFTER_KILL='node ./unrelated.js' \
  STUB_SLEEP=1 \
  TEST_GIT_COMMON="$TEST_GIT_COMMON" \
  TEST_PROJECT_ROOT="$FIXTURE_ROOT" \
  TEST_SAME_WORKTREE="$SAME_WORKTREE" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down >"$TMP_DIR/changed-command.out" 2>&1
rc=$?
set -e

changed_command_calls=$(grep '^kill ' "$KILL_LOG" || true)
if [[ $rc -ne 0 && "$changed_command_calls" == "kill -TERM 8695" ]] \
  && grep -Eq 'PID 8695 on port 3001: identity changed (after SIGTERM|before escalation)' "$TMP_DIR/changed-command.out"; then
  echo "PASS: down (Vite) never escalates against a listener whose command changed after TERM"
else
  echo "FAIL: down (Vite) mishandled listener 8695 whose command changed after TERM (rc=$rc, calls=${changed_command_calls:-none})"
  grep -F 'PID 8695' "$TMP_DIR/changed-command.out" || true
  regression_failures=$((regression_failures + 1))
fi

# If the listener PID is reused after TERM, leave the replacement untouched
# and report that the original listener's termination could not be confirmed.
: > "$KILL_LOG"
rm -f "$KILL_MARKER" "$PS_SEEN_MARKER"
set +e
BASH_ENV="$TMP_DIR/bash-env" \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_LISTENER_PID=8686 \
  LSOF_LISTENER_PORT=3001 \
  LSOF_LISTENER_CWD="$FIXTURE_ROOT/client/desktop" \
  PATH="$STUB_DIR:$PATH" \
  PS_REUSE_AFTER=2 \
  PS_REUSE_PID=8686 \
  STUB_SLEEP=1 \
  TEST_GIT_COMMON="$TEST_GIT_COMMON" \
  TEST_PROJECT_ROOT="$FIXTURE_ROOT" \
  TEST_SAME_WORKTREE="$SAME_WORKTREE" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" --quiet down >"$TMP_DIR/listener-reuse.out" 2>&1
rc=$?
set -e

# down also exits nonzero when the port is merely still busy, so the exit code
# alone cannot show that the reused PID was refused. The reason must say so.
listener_reuse_calls=$(grep '^kill ' "$KILL_LOG" || true)
if [[ $rc -ne 0 && "$listener_reuse_calls" == "kill -TERM 8686" ]] \
  && grep -Fq 'PID 8686 on port 3001: identity changed after SIGTERM' "$TMP_DIR/listener-reuse.out"; then
  echo "PASS: down (Vite) never escalates to a listener PID reused after TERM"
else
  echo "FAIL: down (Vite) mishandled listener PID 8686 after TERM (rc=$rc, calls=${listener_reuse_calls:-none})"
  regression_failures=$((regression_failures + 1))
fi

: > "$KILL_LOG"
rm -f "$KILL_MARKER"

set +e
BASH_ENV="$TMP_DIR/bash-env" \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_LISTENER_PID=4242 \
  LSOF_LISTENER_CWD="$SAME_WORKTREE/services/control-plane" \
  PATH="$STUB_DIR:$PATH" \
  TEST_GIT_COMMON="$TEST_GIT_COMMON" \
  TEST_PROJECT_ROOT="$FIXTURE_ROOT" \
  TEST_SAME_WORKTREE="$SAME_WORKTREE" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down >/dev/null 2>&1
rc=$?
set -e

unforced_kill_calls=$(grep -E '^(kill|pkill) ' "$KILL_LOG" || true)
if [[ $rc -ne 0 && -z "$unforced_kill_calls" ]]; then
  echo "PASS: down never kills untracked processes without explicit --force"
else
  echo "FAIL: down killed untracked processes without --force (${unforced_kill_calls:-none})"
  exit 1
fi

: > "$KILL_LOG"

set +e
BASH_ENV="$TMP_DIR/bash-env" \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_LISTENER_PID=4242 \
  LSOF_LISTENER_CWD="$SAME_WORKTREE/services/control-plane" \
  PATH="$STUB_DIR:$PATH" \
  TEST_GIT_COMMON="$TEST_GIT_COMMON" \
  TEST_PROJECT_ROOT="$FIXTURE_ROOT" \
  TEST_SAME_WORKTREE="$SAME_WORKTREE" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down --force >/dev/null 2>&1
rc=$?
set -e

listener_kill_calls=$(grep '^kill ' "$KILL_LOG" || true)
if [[ $rc -eq 0 && "$listener_kill_calls" == "kill -9 4242" && -e "$KILL_MARKER" ]]; then
  echo "PASS: down --force safely recovers a same-repo cross-worktree listener"
else
  echo "FAIL: down --force did not exclusively kill same-repo listener 4242 (rc=$rc, calls=${listener_kill_calls:-none})"
  exit 1
fi

# A numeric-only record can be identity-bound only when its PID is itself the
# independently verified listener.
printf '%s\n' 4444 > "$FIXTURE_ROOT/logs/control-plane.pid"
: > "$KILL_LOG"
rm -f "$KILL_MARKER"
set +e
BASH_ENV="$TMP_DIR/bash-env" \
  KILL_MARK_CALL='-9 4444' \
  LEGACY_STOPPED_PID=4444 \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_LISTENER_PID=4444 \
  LSOF_LISTENER_CWD="$FIXTURE_ROOT/services/control-plane" \
  PATH="$STUB_DIR:$PATH" \
  TEST_GIT_COMMON="$TEST_GIT_COMMON" \
  TEST_PROJECT_ROOT="$FIXTURE_ROOT" \
  TEST_SAME_WORKTREE="$SAME_WORKTREE" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down --force >"$TMP_DIR/legacy-listener.out" 2>&1
rc=$?
set -e

legacy_listener_calls=$(grep '^kill ' "$KILL_LOG" || true)
if [[ $rc -eq 0 && "$legacy_listener_calls" == "kill -9 4444" && \
  ! -e "$FIXTURE_ROOT/logs/control-plane.pid" ]]; then
  echo "PASS: down --force binds a legacy PID only when it is the verified listener"
else
  echo "FAIL: down --force mishandled legacy listener record 4444 (rc=$rc, calls=${legacy_listener_calls:-none})"
  sed 's/^/  /' "$TMP_DIR/legacy-listener.out"
  regression_failures=$((regression_failures + 1))
fi

# Even a same-cwd wrapper that launched the listener is not direct signal
# authority. Stop only the verified listener and retain the legacy record.
printf '%s\n' 4646 > "$FIXTURE_ROOT/logs/control-plane.pid"
: > "$KILL_LOG"
rm -f "$KILL_MARKER"
set +e
BASH_ENV="$TMP_DIR/bash-env" \
  KILL_MARK_CALL='-9 4747' \
  LISTENER_PARENT_PID=4646 \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_LISTENER_PID=4747 \
  LSOF_LISTENER_CWD="$FIXTURE_ROOT/services/control-plane" \
  LSOF_PARENT_CWD="$FIXTURE_ROOT/services/control-plane" \
  LSOF_PARENT_PID=4646 \
  PATH="$STUB_DIR:$PATH" \
  TEST_GIT_COMMON="$TEST_GIT_COMMON" \
  TEST_PROJECT_ROOT="$FIXTURE_ROOT" \
  TEST_SAME_WORKTREE="$SAME_WORKTREE" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down --force >/dev/null 2>&1
rc=$?
set -e

unrelated_legacy_calls=$(grep '^kill ' "$KILL_LOG" || true)
if [[ $rc -ne 0 && "$unrelated_legacy_calls" == "kill -9 4747" && \
  -e "$FIXTURE_ROOT/logs/control-plane.pid" ]]; then
  echo "PASS: down --force never promotes a same-cwd legacy wrapper PID"
else
  echo "FAIL: down --force trusted same-cwd legacy wrapper PID 4646 (rc=$rc, calls=${unrelated_legacy_calls:-none})"
  regression_failures=$((regression_failures + 1))
fi
rm -f "$FIXTURE_ROOT/logs/control-plane.pid"

# Matching Git metadata is not enough: only roots registered in this
# repository's worktree list are eligible for cross-worktree force cleanup.
: > "$KILL_LOG"
rm -f "$KILL_MARKER"
set +e
BASH_ENV="$TMP_DIR/bash-env" \
  GIT_WORKTREE_LIST_OMIT_SAME=1 \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_LISTENER_PID=4343 \
  LSOF_LISTENER_CWD="$SAME_WORKTREE/services/control-plane" \
  PATH="$STUB_DIR:$PATH" \
  TEST_GIT_COMMON="$TEST_GIT_COMMON" \
  TEST_PROJECT_ROOT="$FIXTURE_ROOT" \
  TEST_SAME_WORKTREE="$SAME_WORKTREE" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down --force >/dev/null 2>&1
rc=$?
set -e

if [[ $rc -ne 0 && ! -s "$KILL_LOG" ]]; then
  echo "PASS: down --force rejects an unregistered same-repo worktree"
else
  echo "FAIL: down --force signaled unregistered worktree listener 4343 (rc=$rc, calls=$(tr '\n' ';' < "$KILL_LOG"))"
  regression_failures=$((regression_failures + 1))
fi

# A matching service path in a different repository never authorizes a kill.
FOREIGN_WORKTREE="$TMP_DIR/foreign-worktree"
FOREIGN_COMMON="$TMP_DIR/foreign.git"
mkdir -p "$FOREIGN_WORKTREE/services/control-plane" "$FOREIGN_COMMON"
: > "$KILL_LOG"
rm -f "$KILL_MARKER"

set +e
BASH_ENV="$TMP_DIR/bash-env" \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_LISTENER_PID=5252 \
  LSOF_LISTENER_CWD="$FOREIGN_WORKTREE/services/control-plane" \
  PATH="$STUB_DIR:$PATH" \
  TEST_FOREIGN_COMMON="$FOREIGN_COMMON" \
  TEST_FOREIGN_WORKTREE="$FOREIGN_WORKTREE" \
  TEST_GIT_COMMON="$TEST_GIT_COMMON" \
  TEST_PROJECT_ROOT="$FIXTURE_ROOT" \
  TEST_SAME_WORKTREE="$SAME_WORKTREE" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down --force >/dev/null 2>&1
rc=$?
set -e

if [[ $rc -ne 0 && ! -s "$KILL_LOG" ]]; then
  echo "PASS: down --force leaves a foreign-repo listener untouched"
else
  echo "FAIL: down --force mishandled foreign-repo listener 5252 (rc=$rc, calls=$(tr '\n' ';' < "$KILL_LOG"))"
  exit 1
fi

# An exiting listener is judged by its start time, but it must still belong to
# this repository. A foreign-repo listener whose leader is already a zombie,
# with another thread still holding the port, is not --force's to SIGKILL.
: > "$KILL_LOG"
rm -f "$KILL_MARKER" "$PS_SEEN_MARKER"
set +e
BASH_ENV="$TMP_DIR/bash-env" \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_LISTENER_PID=5353 \
  LSOF_LISTENER_CWD="$FOREIGN_WORKTREE/services/control-plane" \
  PATH="$STUB_DIR:$PATH" \
  PS_EXITING_PID=5353 \
  TEST_FOREIGN_COMMON="$FOREIGN_COMMON" \
  TEST_FOREIGN_WORKTREE="$FOREIGN_WORKTREE" \
  TEST_GIT_COMMON="$TEST_GIT_COMMON" \
  TEST_PROJECT_ROOT="$FIXTURE_ROOT" \
  TEST_SAME_WORKTREE="$SAME_WORKTREE" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down --force >/dev/null 2>&1
rc=$?
set -e

if [[ $rc -ne 0 && ! -s "$KILL_LOG" ]]; then
  echo "PASS: down --force leaves an exiting foreign-repo listener untouched"
else
  echo "FAIL: down --force signaled exiting foreign-repo listener 5353 (rc=$rc, calls=$(tr '\n' ';' < "$KILL_LOG"))"
  regression_failures=$((regression_failures + 1))
fi

# Control for the case above: the same exiting leader in a registered sibling
# worktree is still this repository's listener, and --force stops it.
: > "$KILL_LOG"
rm -f "$KILL_MARKER" "$PS_SEEN_MARKER"
set +e
BASH_ENV="$TMP_DIR/bash-env" \
  KILL_MARK_CALL='-9 5454' \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_LISTENER_PID=5454 \
  LSOF_LISTENER_CWD="$SAME_WORKTREE/services/control-plane" \
  PATH="$STUB_DIR:$PATH" \
  PS_EXITING_PID=5454 \
  STUB_SLEEP=1 \
  TEST_GIT_COMMON="$TEST_GIT_COMMON" \
  TEST_PROJECT_ROOT="$FIXTURE_ROOT" \
  TEST_SAME_WORKTREE="$SAME_WORKTREE" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down --force >/dev/null 2>&1
rc=$?
set -e

exiting_sibling_calls=$(grep '^kill ' "$KILL_LOG" || true)
if [[ $rc -eq 0 && "$exiting_sibling_calls" == "kill -9 5454" ]]; then
  echo "PASS: down --force stops an exiting listener in a registered sibling worktree"
else
  echo "FAIL: down --force mishandled exiting sibling listener 5454 (rc=$rc, calls=${exiting_sibling_calls:-none})"
  regression_failures=$((regression_failures + 1))
fi

# A stale registration may still list a path that has since become a worktree
# of another repository. Common-dir equality must fail before any signal.
: > "$KILL_LOG"
rm -f "$KILL_MARKER"
set +e
BASH_ENV="$TMP_DIR/bash-env" \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_LISTENER_PID=5353 \
  LSOF_LISTENER_CWD="$SAME_WORKTREE/services/control-plane" \
  PATH="$STUB_DIR:$PATH" \
  TEST_FOREIGN_COMMON="$FOREIGN_COMMON" \
  TEST_FOREIGN_WORKTREE="$FOREIGN_WORKTREE" \
  TEST_GIT_COMMON="$TEST_GIT_COMMON" \
  TEST_PROJECT_ROOT="$FIXTURE_ROOT" \
  TEST_SAME_COMMON="$FOREIGN_COMMON" \
  TEST_SAME_WORKTREE="$SAME_WORKTREE" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down --force >/dev/null 2>&1
rc=$?
set -e

if [[ $rc -ne 0 && ! -s "$KILL_LOG" ]]; then
  echo "PASS: down --force rejects a registered path owned by another repository"
else
  echo "FAIL: down --force signaled stale-registration listener 5353 (rc=$rc, calls=$(tr '\n' ';' < "$KILL_LOG"))"
  regression_failures=$((regression_failures + 1))
fi

: > "$KILL_LOG"
rm -f "$KILL_MARKER"
set +e
BASH_ENV="$TMP_DIR/bash-env" \
  GIT_WORKTREE_PRUNABLE_SAME=1 \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_LISTENER_PID=5454 \
  LSOF_LISTENER_CWD="$SAME_WORKTREE/services/control-plane" \
  PATH="$STUB_DIR:$PATH" \
  TEST_GIT_COMMON="$TEST_GIT_COMMON" \
  TEST_PROJECT_ROOT="$FIXTURE_ROOT" \
  TEST_SAME_WORKTREE="$SAME_WORKTREE" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down --force >/dev/null 2>&1
rc=$?
set -e

if [[ $rc -ne 0 && ! -s "$KILL_LOG" ]]; then
  echo "PASS: down --force rejects a prunable worktree registration"
else
  echo "FAIL: down --force signaled prunable listener 5454"
  regression_failures=$((regression_failures + 1))
fi

# Git must identify the cwd itself as the worktree root plus service path. A
# nested lookalike directory in the same repository is not a Concord service.
NESTED_CWD="$SAME_WORKTREE/tmp/services/control-plane"
mkdir -p "$NESTED_CWD"
: > "$KILL_LOG"
rm -f "$KILL_MARKER"

set +e
BASH_ENV="$TMP_DIR/bash-env" \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_LISTENER_PID=6262 \
  LSOF_LISTENER_CWD="$NESTED_CWD" \
  PATH="$STUB_DIR:$PATH" \
  TEST_GIT_COMMON="$TEST_GIT_COMMON" \
  TEST_PROJECT_ROOT="$FIXTURE_ROOT" \
  TEST_SAME_WORKTREE="$SAME_WORKTREE" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down --force >/dev/null 2>&1
rc=$?
set -e

if [[ $rc -ne 0 && ! -s "$KILL_LOG" ]]; then
  echo "PASS: down --force rejects nested same-repo service lookalikes"
else
  echo "FAIL: down --force killed nested same-repo listener 6262"
  exit 1
fi

# Caller-supplied Git repository selectors must not override cwd-based
# ownership discovery for a foreign listener.
POISON_GIT_COMMON="$TMP_DIR/poison.git"
mkdir -p "$POISON_GIT_COMMON"
: > "$KILL_LOG"
rm -f "$KILL_MARKER"

set +e
BASH_ENV="$TMP_DIR/bash-env" \
  GIT_COMMON_DIR="$POISON_GIT_COMMON" \
  GIT_DIR="$POISON_GIT_COMMON" \
  GIT_WORK_TREE="$FIXTURE_ROOT" \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_LISTENER_PID=6363 \
  LSOF_LISTENER_CWD="$FOREIGN_WORKTREE/services/control-plane" \
  PATH="$STUB_DIR:$PATH" \
  TEST_FOREIGN_COMMON="$FOREIGN_COMMON" \
  TEST_FOREIGN_WORKTREE="$FOREIGN_WORKTREE" \
  TEST_GIT_COMMON="$POISON_GIT_COMMON" \
  TEST_PROJECT_ROOT="$FIXTURE_ROOT" \
  TEST_SAME_WORKTREE="$SAME_WORKTREE" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down --force >/dev/null 2>&1
rc=$?
set -e

if [[ $rc -ne 0 && ! -s "$KILL_LOG" ]]; then
  echo "PASS: down --force ignores inherited Git repository selectors"
else
  echo "FAIL: inherited Git selectors authorized foreign listener 6363"
  exit 1
fi

# If the PID's start identity changes during validation, it was reused; never
# signal the replacement even if it now reports the same cwd and port.
: > "$KILL_LOG"
rm -f "$KILL_MARKER" "$PS_SEEN_MARKER"

set +e
BASH_ENV="$TMP_DIR/bash-env" \
  LOG_DIR="$FIXTURE_ROOT/logs" \
  LSOF_BUSY_PORT='' \
  LSOF_LISTENER_PID=7373 \
  LSOF_LISTENER_CWD="$SAME_WORKTREE/services/control-plane" \
  PATH="$STUB_DIR:$PATH" \
  PS_REUSE_PID=7373 \
  TEST_GIT_COMMON="$TEST_GIT_COMMON" \
  TEST_PROJECT_ROOT="$FIXTURE_ROOT" \
  TEST_SAME_WORKTREE="$SAME_WORKTREE" \
  "$FIXTURE_ROOT/scripts/concord-dev.sh" down --force >/dev/null 2>&1
rc=$?
set -e

if [[ $rc -ne 0 && ! -s "$KILL_LOG" ]]; then
  echo "PASS: down --force refuses a PID reused during validation"
else
  echo "FAIL: down --force killed reused PID 7373"
  exit 1
fi

if [[ $regression_failures -ne 0 ]]; then
  echo "FAIL: $regression_failures new down safety regression(s) remain"
  exit 1
fi

echo "All down tests passed."
