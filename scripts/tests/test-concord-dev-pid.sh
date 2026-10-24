#!/usr/bin/env bash
# Tests for identity-bound PID lifecycle (stop_process via real subprocesses).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
CONCORD_DEV="$PROJECT_ROOT/scripts/concord-dev.sh"

# shellcheck disable=SC1090
source "$CONCORD_DEV"

TMP_DIR=$(mktemp -d)
PID_CWD=$(cd "$PROJECT_ROOT" && pwd -P)
FAILURES=0
TARGET_PIDS=()
TEST_KILL_LOG="$TMP_DIR/kill.log"
TEST_KILL_PID=""
TEST_KILL_FAIL_CALL=""
TEST_KILL_FAKE_CALL=""
TEST_KILL_FAKE=0
TEST_KILL_REAP_ON_KILL=0
TEST_PS_PID=""
TEST_PS_REUSE_AFTER=999
TEST_PS_ABSENT_AFTER=999
TEST_PS_WARN=0
TEST_PS_COUNT_FILE="$TMP_DIR/ps-count"
TEST_FAST_SLEEP=0
TEST_RM_FAIL_FILE=""
TEST_RM_LOG="$TMP_DIR/rm.log"

target_job_running() {
  local expected_pid="$1"
  local job_pid
  while IFS= read -r job_pid; do
    [[ "$job_pid" == "$expected_pid" ]] && return 0
  done < <(jobs -pr)
  return 1
}

cleanup() {
  local pid
  for pid in ${TARGET_PIDS[@]+"${TARGET_PIDS[@]}"}; do
    [[ -n "$pid" ]] || continue
    target_job_running "$pid" && builtin kill -9 "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  done
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT

# Override only the deliberately simulated process; all ordinary calls still
# exercise the platform builtins and commands.
kill() {
  local target="${!#}"
  if [[ -n "$TEST_KILL_PID" && "$target" == "$TEST_KILL_PID" ]]; then
    printf 'kill %s\n' "$*" >> "$TEST_KILL_LOG"
    [[ "$*" == "$TEST_KILL_FAIL_CALL" ]] && return 1
    [[ "$*" == "$TEST_KILL_FAKE_CALL" ]] && return 0
    [[ "$TEST_KILL_FAKE" -eq 1 ]] && return 0
    builtin kill "$@"
    local rc=$?
    if [[ $rc -eq 0 && "$TEST_KILL_REAP_ON_KILL" -eq 1 && "$1" == "-9" ]]; then
      wait "$target" 2>/dev/null || true
    fi
    return "$rc"
  fi
  builtin kill "$@"
}

ps() {
  if [[ -n "$TEST_PS_PID" && "$*" == *"-p $TEST_PS_PID "* && "$*" == *"lstart="* ]]; then
    [[ "$TEST_PS_WARN" -eq 1 ]] && echo 'simulated ps warning' >&2
    local count=0
    [[ -f "$TEST_PS_COUNT_FILE" ]] && read -r count < "$TEST_PS_COUNT_FILE"
    count=$((count + 1))
    printf '%s\n' "$count" > "$TEST_PS_COUNT_FILE"
    if [[ $count -gt $TEST_PS_ABSENT_AFTER ]]; then
      return 1
    fi
    if [[ $count -gt $TEST_PS_REUSE_AFTER ]]; then
      printf 'replacement-start\n'
      return 0
    fi
  fi
  command ps "$@"
}

rm() {
  local arg
  for arg in "$@"; do
    if [[ -n "$TEST_RM_FAIL_FILE" && "$arg" == "$TEST_RM_FAIL_FILE" ]]; then
      printf 'rm %s\n' "$*" >> "$TEST_RM_LOG"
      return 1
    fi
  done
  command rm "$@"
}

sleep() {
  [[ "$TEST_FAST_SLEEP" -eq 1 ]] && return 0
  command sleep "$@"
}

start_target() {
  (cd "$PID_CWD" && exec sleep 60) &
  TARGET_PID=$!
  TARGET_PIDS+=("$TARGET_PID")
  command sleep 0.05
  TARGET_START=$(LC_ALL=C command ps -p "$TARGET_PID" -o lstart=)
  PID_FILE="$TMP_DIR/$TARGET_PID.pid"
  : > "$TEST_KILL_LOG"
  rm -f "$TEST_PS_COUNT_FILE"
  TEST_KILL_PID=""
  TEST_KILL_FAIL_CALL=""
  TEST_KILL_FAKE_CALL=""
  TEST_KILL_FAKE=0
  TEST_KILL_REAP_ON_KILL=0
  TEST_PS_PID=""
  TEST_PS_REUSE_AFTER=999
  TEST_PS_ABSENT_AFTER=999
  TEST_PS_WARN=0
  TEST_FAST_SLEEP=0
  TEST_RM_FAIL_FILE=""
  : > "$TEST_RM_LOG"
}

write_record() {
  printf '%s\n%s\n%s\n' "$TARGET_PID" "$TARGET_START" "$PID_CWD" > "$PID_FILE"
}

target_alive() {
  local state
  state=$(command ps -o state= -p "$TARGET_PID" 2>/dev/null) || return 1
  [[ "$state" != Z* ]]
}

finish_target() {
  local index
  target_job_running "$TARGET_PID" && builtin kill -9 "$TARGET_PID" 2>/dev/null || true
  wait "$TARGET_PID" 2>/dev/null || true
  for index in "${!TARGET_PIDS[@]}"; do
    [[ "${TARGET_PIDS[$index]}" == "$TARGET_PID" ]] && TARGET_PIDS[index]=""
  done
}

fail() {
  echo "FAIL: $1"
  FAILURES=$((FAILURES + 1))
}

# An exit between the start-time and cwd probes is definitive absence, not an
# unverified identity that would block cleanup.
start_target
original_process_cwd_identity=$(declare -f process_cwd_identity)
process_cwd_identity() { return 1; }
set +e
process_identity_state "$TARGET_PID" "$TARGET_START" "$PID_CWD"
rc=$?
set -e
eval "$original_process_cwd_identity"
if [[ $rc -eq 1 ]]; then
  echo "PASS: process identity preserves an exit between probes"
else
  fail "process identity collapsed an absent cwd probe to rc=$rc"
fi
finish_target

# A complete identity record authorizes graceful termination.
start_target
write_record
TEST_PS_PID="$TARGET_PID"
TEST_PS_WARN=1
if stop_process "$PID_FILE" "Sleep" >/dev/null 2>&1 && ! target_alive && [[ ! -f "$PID_FILE" ]]; then
  echo "PASS: stop_process terminates the process named by a complete identity record"
else
  fail "stop_process rejected or failed to terminate a complete identity record"
fi
finish_target

# --force authorizes immediate KILL only for the same complete identity.
start_target
write_record
if stop_process "$PID_FILE" "ForceSleep" --force >/dev/null 2>&1 && ! target_alive && [[ ! -f "$PID_FILE" ]]; then
  echo "PASS: stop_process --force kills the process named by a complete identity record"
else
  fail "stop_process --force rejected or failed to kill a complete identity record"
fi
finish_target

# A tracked process that survives TERM is revalidated before KILL, then its
# record is removed only after the forced stop is confirmed.
start_target
write_record
TEST_KILL_PID="$TARGET_PID"
TEST_KILL_FAKE_CALL="-TERM $TARGET_PID"
TEST_KILL_REAP_ON_KILL=1
TEST_FAST_SLEEP=1
if stop_process "$PID_FILE" "EscalatedSleep" >/dev/null 2>&1; then
  escalation_rc=0
else
  escalation_rc=$?
fi
escalation_calls=$(grep '^kill ' "$TEST_KILL_LOG" || true)
if [[ $escalation_rc -eq 0 && "$escalation_calls" == $'kill -TERM '"$TARGET_PID"$'\nkill -9 '"$TARGET_PID" && ! -f "$PID_FILE" ]] && ! target_alive; then
  echo "PASS: stop_process revalidates and completes TERM-to-KILL escalation"
else
  fail "stop_process failed TERM-to-KILL escalation (rc=$escalation_rc, calls=${escalation_calls:-none})"
fi
finish_target

# A signal can lose a normal exit race. When the frozen process identity is
# then absent, cleanup succeeds and removes its record instead of lying about
# a signal failure.
start_target
write_record
TEST_KILL_PID="$TARGET_PID"
TEST_KILL_FAIL_CALL="-9 $TARGET_PID"
TEST_PS_PID="$TARGET_PID"
TEST_PS_ABSENT_AFTER=1
if stop_process "$PID_FILE" "ExitBeforeSignal" --force >/dev/null 2>&1; then
  exit_race_rc=0
else
  exit_race_rc=$?
fi
exit_race_calls=$(grep '^kill ' "$TEST_KILL_LOG" || true)
if [[ $exit_race_rc -eq 0 && "$exit_race_calls" == "kill -9 $TARGET_PID" && ! -f "$PID_FILE" ]]; then
  echo "PASS: stop_process accepts confirmed exit after a failed signal"
else
  fail "stop_process mishandled exit after failed signal (rc=$exit_race_rc, calls=${exit_race_calls:-none})"
fi
finish_target

# The process may exit after the graceful wait times out but before SIGKILL.
# That is successful cleanup, and the departed PID must never receive KILL.
start_target
write_record
TEST_KILL_PID="$TARGET_PID"
TEST_KILL_FAKE_CALL="-TERM $TARGET_PID"
TEST_PS_PID="$TARGET_PID"
TEST_PS_ABSENT_AFTER=11
TEST_FAST_SLEEP=1
if stop_process "$PID_FILE" "ExitBeforeEscalation" >/dev/null 2>&1; then
  escalation_race_rc=0
else
  escalation_race_rc=$?
fi
escalation_race_calls=$(grep '^kill ' "$TEST_KILL_LOG" || true)
if [[ $escalation_race_rc -eq 0 && "$escalation_race_calls" == "kill -TERM $TARGET_PID" && ! -f "$PID_FILE" ]]; then
  echo "PASS: stop_process accepts exit immediately before escalation"
else
  fail "stop_process mishandled exit before escalation (rc=$escalation_race_rc, calls=${escalation_race_calls:-none})"
fi
finish_target

# A live legacy numeric-only record is unbound: never signal it, never discard
# it, and report the uncertain cleanup state.
start_target
printf '%s\n' "$TARGET_PID" > "$PID_FILE"
TEST_KILL_PID="$TARGET_PID"
TEST_KILL_FAKE=1
set +e
stop_process "$PID_FILE" "Legacy" --force >/dev/null 2>&1
rc=$?
set -e
if [[ $rc -ne 0 && ! -s "$TEST_KILL_LOG" && -f "$PID_FILE" ]] && target_alive; then
  echo "PASS: stop_process retains and rejects a live unbound legacy PID"
else
  fail "stop_process signaled, discarded, or accepted a live legacy PID (rc=$rc)"
fi
finish_target

# A failed record deletion is a failed cleanup even when the owned process is
# gone; retain the record so a later down can reconcile it.
start_target
write_record
TEST_KILL_PID="$TARGET_PID"
TEST_KILL_REAP_ON_KILL=1
TEST_RM_FAIL_FILE="$PID_FILE"
set +e
stop_process "$PID_FILE" "UndeletableRecord" --force >/dev/null 2>&1
rc=$?
set -e
if [[ $rc -ne 0 && -f "$PID_FILE" && -s "$TEST_RM_LOG" ]] && ! target_alive; then
  echo "PASS: stop_process propagates PID-record deletion failure"
else
  fail "stop_process hid PID-record deletion failure (rc=$rc)"
fi
TEST_RM_FAIL_FILE=""
command rm -f "$PID_FILE"
finish_target

# If atomic PID-record creation fails, the just-started child is killed and
# confirmed gone rather than becoming an untracked orphan.
start_target
PID_FILE="$TMP_DIR/missing-parent/record.pid"
TEST_KILL_PID="$TARGET_PID"
TEST_KILL_REAP_ON_KILL=1
set +e
track_started_process "$PID_FILE" "$TARGET_PID" "$PID_CWD" "UnrecordedSleep" >/dev/null 2>&1
rc=$?
set -e
record_failure_calls=$(grep '^kill ' "$TEST_KILL_LOG" || true)
if [[ $rc -ne 0 && "$record_failure_calls" == "kill -9 $TARGET_PID" && ! -e "$PID_FILE" ]] && ! target_alive; then
  echo "PASS: failed PID-record creation cleans up the spawned process"
else
  fail "failed PID-record creation leaked its spawned process (rc=$rc, calls=${record_failure_calls:-none})"
fi
finish_target

# Identity capture can fail before a complete record exists. The direct child
# remains identifiable through Bash's job table and must still be reaped.
start_target
TEST_KILL_PID="$TARGET_PID"
TEST_KILL_REAP_ON_KILL=1
TEST_PS_PID="$TARGET_PID"
TEST_PS_ABSENT_AFTER=0
set +e
track_started_process "$PID_FILE" "$TARGET_PID" "$PID_CWD" "UnidentifiedSleep" >/dev/null 2>&1
rc=$?
set -e
identity_failure_calls=$(grep '^kill ' "$TEST_KILL_LOG" || true)
if [[ $rc -ne 0 && "$identity_failure_calls" == "kill -9 $TARGET_PID" && ! -e "$PID_FILE" ]] && ! target_alive; then
  echo "PASS: failed identity capture cleans up the spawned process"
else
  fail "failed identity capture leaked its spawned process (rc=$rc, calls=${identity_failure_calls:-none})"
fi
finish_target

# Stored start time and cwd are both part of the authorization boundary.
for mismatch in start cwd; do
  start_target
  if [[ "$mismatch" == start ]]; then
    printf '%s\n%s\n%s\n' "$TARGET_PID" "wrong-start" "$PID_CWD" > "$PID_FILE"
  else
    printf '%s\n%s\n%s\n' "$TARGET_PID" "$TARGET_START" "$TMP_DIR" > "$PID_FILE"
  fi
  TEST_KILL_PID="$TARGET_PID"
  TEST_KILL_FAKE=1
  set +e
  stop_process "$PID_FILE" "WrongIdentity" --force >/dev/null 2>&1
  rc=$?
  set -e
  if [[ $rc -ne 0 && ! -s "$TEST_KILL_LOG" && -f "$PID_FILE" ]] && target_alive; then
    echo "PASS: stop_process rejects a PID with the wrong $mismatch identity"
  else
    fail "stop_process signaled, discarded, or accepted a PID with wrong $mismatch (rc=$rc)"
  fi
  finish_target
done

# Revalidate immediately before the first signal so a just-reused PID cannot
# inherit authorization from the tracking record.
start_target
write_record
TEST_KILL_PID="$TARGET_PID"
TEST_KILL_FAKE=1
TEST_PS_PID="$TARGET_PID"
TEST_PS_REUSE_AFTER=0
set +e
stop_process "$PID_FILE" "PreSignalReuse" >/dev/null 2>&1
rc=$?
set -e
if [[ $rc -ne 0 && ! -s "$TEST_KILL_LOG" && -f "$PID_FILE" ]] && target_alive; then
  echo "PASS: stop_process refuses a PID reused before the first signal"
else
  fail "stop_process signaled or forgot a PID reused before the first signal (rc=$rc)"
fi
finish_target

# Revalidate again after TERM. The replacement may not receive KILL, and the
# retained record plus nonzero status exposes the uncertain termination.
start_target
write_record
TEST_KILL_PID="$TARGET_PID"
TEST_KILL_FAKE=1
TEST_PS_PID="$TARGET_PID"
TEST_PS_REUSE_AFTER=1
TEST_FAST_SLEEP=1
set +e
stop_process "$PID_FILE" "PostTermReuse" >/dev/null 2>&1
rc=$?
set -e
reuse_calls=$(grep '^kill ' "$TEST_KILL_LOG" || true)
if [[ $rc -ne 0 && "$reuse_calls" == "kill -TERM $TARGET_PID" && -f "$PID_FILE" ]] && target_alive; then
  echo "PASS: stop_process never escalates to a PID reused after TERM"
else
  fail "stop_process mishandled PID reuse after TERM (rc=$rc, calls=${reuse_calls:-none})"
fi
finish_target

# Signal failures are cleanup failures. Preserve the live process and tracking
# record on both graceful and forced paths.
for signal in TERM 9; do
  start_target
  write_record
  TEST_KILL_PID="$TARGET_PID"
  TEST_KILL_FAIL_CALL="-$signal $TARGET_PID"
  TEST_KILL_FAKE=1
  TEST_FAST_SLEEP=1
  set +e
  if [[ "$signal" == TERM ]]; then
    stop_process "$PID_FILE" "FailedSignal" >/dev/null 2>&1
  else
    stop_process "$PID_FILE" "FailedSignal" --force >/dev/null 2>&1
  fi
  rc=$?
  set -e
  failure_calls=$(grep '^kill ' "$TEST_KILL_LOG" || true)
  if [[ $rc -ne 0 && "$failure_calls" == "kill -$signal $TARGET_PID" && -f "$PID_FILE" ]] && target_alive; then
    echo "PASS: stop_process reports failed -$signal and preserves tracking"
  else
    fail "stop_process swallowed failed -$signal (rc=$rc, calls=${failure_calls:-none})"
  fi
  finish_target
done

# A successful kill(2) return is not proof that the process disappeared. Keep
# the record and fail when the same identity survives the forced stop.
start_target
write_record
TEST_KILL_PID="$TARGET_PID"
TEST_KILL_FAKE=1
TEST_FAST_SLEEP=1
set +e
stop_process "$PID_FILE" "SurvivingKill" --force >/dev/null 2>&1
rc=$?
set -e
survivor_calls=$(grep '^kill ' "$TEST_KILL_LOG" || true)
if [[ $rc -ne 0 && "$survivor_calls" == "kill -9 $TARGET_PID" && -f "$PID_FILE" ]] && target_alive; then
  echo "PASS: stop_process preserves tracking when SIGKILL does not end the process"
else
  fail "stop_process trusted SIGKILL without confirming exit (rc=$rc, calls=${survivor_calls:-none})"
fi
finish_target

if [[ $FAILURES -ne 0 ]]; then
  echo "FAIL: $FAILURES PID lifecycle regression(s) remain"
  exit 1
fi

echo "All PID tests passed."
