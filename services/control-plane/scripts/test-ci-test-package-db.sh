#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
task_tmp="$(mktemp -d)"
trap 'rm -rf "$task_tmp"' EXIT
mkdir -p "$task_tmp/bin"

cat > "$task_tmp/bin/psql" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$PSQL_CALLS"
SH
cat > "$task_tmp/bin/fixture.test" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$DATABASE_URL" > "$TEST_URL_OUTPUT"
redis_present=0
if [[ -n "${REDIS_URL:-}" ]]; then
  redis_present=1
fi
printf '%s\n' "${WS_EVENT_BUDGET_REDIS_INTEGRATION:-}" \
  "${MANAGED_REDIS_INTEGRATION:-}" "${MANAGED_REDIS_TEST_IMAGE:-}" \
  "$redis_present" > "$TEST_FLAGS_OUTPUT"
printf '%s\0' "$@" > "$TEST_ARGS_OUTPUT"
exit "${TEST_BINARY_EXIT:-0}"
SH
chmod +x "$task_tmp/bin/psql" "$task_tmp/bin/fixture.test"

export PATH="$task_tmp/bin:$PATH"
export CI_PACKAGE_DB_ISOLATION=1
fixture_password="$(python3 -c 'import secrets; print(secrets.token_hex(12))')"
export DATABASE_URL="postgres://ci:${fixture_password}@localhost:5432/concord_test?sslmode=disable"
export PSQL_CALLS="$task_tmp/psql-calls"
export TEST_URL_OUTPUT="$task_tmp/test-url"
export TEST_FLAGS_OUTPUT="$task_tmp/test-flags"
export TEST_ARGS_OUTPUT="$task_tmp/test-args"
unset WS_EVENT_BUDGET_REDIS_INTEGRATION MANAGED_REDIS_INTEGRATION MANAGED_REDIS_TEST_IMAGE
export RUNNER_TEMP="$task_tmp"

bash "$SCRIPT_DIR/ci-test-package-db.sh" "$task_tmp/bin/fixture.test"
[[ "$(wc -l < "$PSQL_CALLS")" -eq 2 ]]
created="$(sed -n '1p' "$PSQL_CALLS")"
dropped="$(sed -n '2p' "$PSQL_CALLS")"
[[ "$created" =~ CREATE\ DATABASE\ \"(concord_ci_[a-f0-9]{16}_test)\"\ TEMPLATE\ concord_test ]]
database="${BASH_REMATCH[1]}"
[[ "$dropped" == *"DROP DATABASE \"$database\" WITH (FORCE)"* ]]
[[ "$(cat "$TEST_URL_OUTPUT")" == "postgres://ci:${fixture_password}@localhost:5432/$database?sslmode=disable" ]]

: > "$PSQL_CALLS"
if TEST_BINARY_EXIT=7 bash "$SCRIPT_DIR/ci-test-package-db.sh" "$task_tmp/bin/fixture.test"; then
  echo 'FAIL: test binary failure was hidden' >&2
  exit 1
else
  [[ "$?" -eq 7 ]]
fi
[[ "$(wc -l < "$PSQL_CALLS")" -eq 2 ]]

: > "$PSQL_CALLS"
invalid_database_url="postgres://ci:${fixture_password}@localhost:5432/concord?sslmode=disable"
if DATABASE_URL="$invalid_database_url" \
    bash "$SCRIPT_DIR/ci-test-package-db.sh" "$task_tmp/bin/fixture.test" 2>/dev/null; then
  echo 'FAIL: non-template DATABASE_URL was accepted' >&2
  exit 1
fi
[[ ! -s "$PSQL_CALLS" ]]

# Both packages observe fixed NATS subjects. A second binary entering while
# the first still holds the shared lock would fail the atomic mkdir.
cat > "$task_tmp/bin/voice.test" <<'SH'
#!/usr/bin/env bash
mkdir "$NATS_BUSY"
sleep 0.5
rmdir "$NATS_BUSY"
SH
cp "$task_tmp/bin/voice.test" "$task_tmp/bin/dm.test"
chmod +x "$task_tmp/bin/voice.test" "$task_tmp/bin/dm.test"
export NATS_BUSY="$task_tmp/nats-busy"
bash "$SCRIPT_DIR/ci-test-package-db.sh" "$task_tmp/bin/voice.test" &
voice_pid=$!
bash "$SCRIPT_DIR/ci-test-package-db.sh" "$task_tmp/bin/dm.test" &
dm_pid=$!
wait "$voice_pid"
wait "$dm_pid"

# These binary fixtures exercise the same -exec boundary Go uses. They record
# only fixed flags and endpoint presence; neither endpoint nor password is logged.
cp "$task_tmp/bin/fixture.test" "$task_tmp/bin/websocket.test"
cp "$task_tmp/bin/fixture.test" "$task_tmp/bin/managedredis.test"
chmod +x "$task_tmp/bin/websocket.test" "$task_tmp/bin/managedredis.test"
export REDIS_URL="redis://ci:${fixture_password}@127.0.0.1:6379/0"

fail_bridge() {
  echo "FAIL: $1" >&2
  exit 1
}
check_bridge_flags() {
  local expected_ws="$1" expected_managed="$2" expected_image="$3"
  mapfile -t actual < "$TEST_FLAGS_OUTPUT"
  [[ "${actual[0]}" == "$expected_ws" ]] || fail_bridge 'incorrect websocket Redis opt-in'
  [[ "${actual[1]}" == "$expected_managed" ]] || fail_bridge 'incorrect managed Redis opt-in'
  [[ "${actual[2]}" == "$expected_image" ]] || fail_bridge 'incorrect managed Redis fixture image'
  [[ "${actual[3]}" == 1 ]] || fail_bridge 'fixture did not receive its explicit Redis endpoint'
}
check_bridge_args() {
  local verbose="$1"
  python3 - "$TEST_ARGS_OUTPUT" "$verbose" <<'PYARGS'
import pathlib
import sys

raw = pathlib.Path(sys.argv[1]).read_bytes()
actual = raw.rstrip(b'\0').split(b'\0') if raw else []
expected = [b'-test.run=FixtureCase', b'argument with spaces', b'literal\\argument']
if sys.argv[2] == 'yes':
    expected.insert(0, b'-test.v')
if actual != expected:
    raise SystemExit('FAIL: wrapper changed binary arguments or omitted package verbosity')
PYARGS
}
check_bridge_cleanup() {
  [[ "$(wc -l < "$PSQL_CALLS")" -eq 2 ]] || fail_bridge 'selected binary did not create and remove its database'
  [[ "$(sed -n '1p' "$PSQL_CALLS")" == *'CREATE DATABASE '* ]] || fail_bridge 'selected binary skipped database creation'
  [[ "$(sed -n '2p' "$PSQL_CALLS")" == *'DROP DATABASE '*' WITH (FORCE)'* ]] || fail_bridge 'selected binary leaked its package database'
}
check_bridge_log() {
  if grep -Fq -- "$fixture_password" "$1" || grep -Fq -- "$REDIS_URL" "$1"; then
    fail_bridge 'wrapper output exposed a credential or Redis endpoint'
  fi
}
for binary in websocket.test managedredis.test fixture.test; do
  : > "$PSQL_CALLS"
  bridge_log="$task_tmp/$binary.log"
  bash "$SCRIPT_DIR/ci-test-package-db.sh" "$task_tmp/bin/$binary" \
    '-test.run=FixtureCase' 'argument with spaces' 'literal\argument' > "$bridge_log" 2>&1
  case "$binary" in
    websocket.test) check_bridge_flags 1 '' ''; check_bridge_args yes ;;
    managedredis.test) check_bridge_flags '' 1 redis:8-alpine; check_bridge_args yes ;;
    fixture.test) check_bridge_flags '' '' ''; check_bridge_args no ;;
  esac
  check_bridge_cleanup
  check_bridge_log "$bridge_log"
done

# Failure still propagates through both selected package paths and cleanup.
for binary in websocket.test managedredis.test; do
  : > "$PSQL_CALLS"
  bridge_log="$task_tmp/$binary.failure.log"
  if TEST_BINARY_EXIT=7 bash "$SCRIPT_DIR/ci-test-package-db.sh" "$task_tmp/bin/$binary" \
      '-test.run=FixtureCase' 'argument with spaces' 'literal\argument' > "$bridge_log" 2>&1; then
    fail_bridge 'selected test binary failure was hidden'
  else
    [[ "$?" -eq 7 ]] || fail_bridge 'selected test binary exit status changed'
  fi
  check_bridge_cleanup
  check_bridge_args yes
  check_bridge_log "$bridge_log"
done

# Missing/empty endpoints must fail before websocket.test executes. This cannot
# silently choose redistest's developer default even though the binary exists.
for endpoint_state in missing empty; do
  : > "$PSQL_CALLS"
  rm -f "$TEST_FLAGS_OUTPUT" "$TEST_ARGS_OUTPUT" "$TEST_URL_OUTPUT"
  bridge_log="$task_tmp/websocket.$endpoint_state.log"
  if [[ "$endpoint_state" == missing ]]; then
    if env -u REDIS_URL bash "$SCRIPT_DIR/ci-test-package-db.sh" "$task_tmp/bin/websocket.test" > "$bridge_log" 2>&1; then
      fail_bridge 'websocket binary accepted an unset REDIS_URL'
    fi
  else
    if REDIS_URL='' bash "$SCRIPT_DIR/ci-test-package-db.sh" "$task_tmp/bin/websocket.test" > "$bridge_log" 2>&1; then
      fail_bridge 'websocket binary accepted an empty REDIS_URL'
    fi
  fi
  [[ ! -e "$TEST_FLAGS_OUTPUT" && ! -e "$TEST_ARGS_OUTPUT" && ! -e "$TEST_URL_OUTPUT" ]] || fail_bridge 'websocket binary ran without an explicit Redis endpoint'
  grep -Fq 'REDIS_URL' "$bridge_log" || fail_bridge 'missing Redis endpoint did not produce its own diagnostic'
  if [[ -s "$PSQL_CALLS" ]]; then
    check_bridge_cleanup
  fi
  check_bridge_log "$bridge_log"
done

# Preserve the original wrapper's usage boundary, including no accidental DB
# work or fixture execution when the CI opt-in or executable is missing.
for invalid_case in no_arguments non_executable isolation_disabled; do
  : > "$PSQL_CALLS"
  rm -f "$TEST_FLAGS_OUTPUT"
  bridge_log="$task_tmp/$invalid_case.log"
  case "$invalid_case" in
    no_arguments) command_args=(bash "$SCRIPT_DIR/ci-test-package-db.sh") ;;
    non_executable) command_args=(bash "$SCRIPT_DIR/ci-test-package-db.sh" "$task_tmp/absent.test") ;;
    isolation_disabled) command_args=(env CI_PACKAGE_DB_ISOLATION=0 bash "$SCRIPT_DIR/ci-test-package-db.sh" "$task_tmp/bin/fixture.test") ;;
  esac
  if "${command_args[@]}" > "$bridge_log" 2>&1; then
    fail_bridge 'invalid wrapper invocation succeeded'
  else
    [[ "$?" -eq 2 ]] || fail_bridge 'original wrapper usage status changed'
  fi
  [[ ! -s "$PSQL_CALLS" && ! -e "$TEST_FLAGS_OUTPUT" ]] || fail_bridge 'invalid invocation performed work'
  check_bridge_log "$bridge_log"
done

# Go's own build-tag evaluator must select the ACL canary under the tag already
# used by coverage CI. A source-string assertion would miss equivalent tags.
(cd "$SCRIPT_DIR/.." && go list -tags integration -f '{{join .TestGoFiles "\n"}}' ./internal/managedredis) > "$task_tmp/integration-files"
grep -Fxq 'acl_integration_test.go' "$task_tmp/integration-files" || fail_bridge 'existing integration tag excludes the Redis ACL canary'

# The outer go command must opt out of result caching before -exec is reached
# and show successful inner-binary output instead of suppressing its audit logs.
# This is a narrow source contract on the one existing merged coverage run;
# a -test.count flag added inside the wrapper cannot satisfy it.
python3 - "$SCRIPT_DIR/../../../.github/workflows/build.yml" <<'PYWORKFLOW'
import pathlib
import shlex
import sys

def command_flags(command):
    tokens = shlex.split(command, comments=True)
    if tokens[:2] != ['go', 'test']:
        return []
    value_flags = {'-p', '-timeout', '-exec', '-tags', '-coverprofile', '-count'}
    flags = []
    index = 2
    while index < len(tokens):
        token = tokens[index]
        if token in {'--', '-args'} or not token.startswith('-'):
            break  # Package/test arguments cannot satisfy the outer flag contract.
        flags.append(token)
        index += 1
        if token in value_flags:
            if index == len(tokens):
                return []
            index += 1  # A flag's value is not another flag.
    return flags


def auditable(command):
    flags = command_flags(command)
    return '-count=1' in flags and '-v' in flags


base = ('go test -race -trimpath -p 2 -timeout 20m '
        '-exec "$PWD/scripts/ci-test-package-db.sh" '
        '-tags integration -coverprofile=coverage.out ')
parser_cases = [
    ('genuine flags', base + '-v -count=1 ./...', True),
    ('commented flags', base + './... # -v -count=1', False),
    ('flags after package arguments', base + './... -v -count=1', False),
    ('flags after explicit separator', base + '-- -v -count=1 ./...', False),
    ('flags after test-argument marker', base + '-args -v -count=1', False),
    ('verbosity only', base + '-v ./...', False),
    ('cache suppression only', base + '-count=1 ./...', False),
    ('flags inside exec value', 'go test -exec "-v -count=1" ./...', False),
]
for name, command, expected in parser_cases:
    if auditable(command) != expected:
        raise SystemExit('FAIL: outer-command parser fixture: ' + name)

source = pathlib.Path(sys.argv[1]).read_text().replace('\\\n', ' ')
commands = [line.strip() for line in source.splitlines()
            if line.strip().startswith('go test ')
            and '-coverprofile=coverage.out' in command_flags(line.strip())
            and any('scripts/ci-test-package-db.sh' in token
                    for token in shlex.split(line.strip(), comments=True))]
if len(commands) != 1 or not auditable(commands[0]):
    raise SystemExit('FAIL: the existing outer coverage go test invocation must use -count=1 and -v')
PYWORKFLOW

echo 'PASS: package DB wrapper preserves isolation and failures; selected Redis suites require explicit auditable fresh execution'
