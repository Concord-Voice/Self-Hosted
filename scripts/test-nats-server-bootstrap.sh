#!/usr/bin/env bash
# Hermetic contract tests for the mandatory native JetStream test broker.
set -euo pipefail

TEST_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RESOLVER="${TEST_ROOT}/scripts/test-tools/nats-server.sh"
[[ -f "$RESOLVER" ]] || {
  printf 'FAIL: NATS test broker resolver is missing: %s\n' "$RESOLVER" >&2
  exit 1
}
TEST_TEMP="$(mktemp -d)"
background_pids=()
cleanup() {
  local pid
  for pid in "${background_pids[@]}"; do kill -TERM "$pid" 2>/dev/null || true; done
  for pid in "${background_pids[@]}"; do wait "$pid" 2>/dev/null || true; done
  rm -rf "$TEST_TEMP"
}
trap cleanup EXIT
PIN_DIGEST="2ae57dfba3063d5e25fecb75256118c0882a1a2d2cf7d0fba17a7c701df956f6" # pragma: allowlist secret -- public official NATS image manifest digest
PIN_IMAGE="nats:2.15.1-alpine@sha256:${PIN_DIGEST}"
REAL_ENV="$(command -v env)"
CASE_DIR=""
CASE_BIN=""
CASE_TMP=""
RESOLVED=""

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

# Quoted heredoc markers use letters only for the pinned Semgrep parser.
setup_case() {
  CASE_DIR="${TEST_TEMP}/$1"
  CASE_BIN="${CASE_DIR}/bin"
  CASE_TMP="${CASE_DIR}/tmp"
  mkdir -p "$CASE_BIN" "$CASE_TMP"
  # A whitelist keeps a host-installed Docker or nats-server out of every case.
  local native native_path
  for native in bash basename cat chmod cp cut date dirname env find flock grep head id \
    mkdir mktemp mv od readlink realpath rm sed sha256sum sleep sort stat tail tee timeout \
    touch tr uname wc; do
    native_path="$(command -v "$native")" || fail "missing test prerequisite: $native"
    ln -s "$native_path" "${CASE_BIN}/${native}"
  done
  rm "${CASE_BIN}/uname"
  cat >"${CASE_BIN}/uname" <<'MOCKUNAME'
#!/bin/bash
case "${1:-}" in
  -s) printf '%s\n' "${NATS_BOOTSTRAP_TEST_OS:-Linux}" ;;
  -m) printf '%s\n' "${NATS_BOOTSTRAP_TEST_ARCH:-x86_64}" ;;
  *) printf 'unsupported fixture uname arguments\n' >&2; exit 64 ;;
esac
MOCKUNAME
  chmod +x "${CASE_BIN}/uname"
  cat >"${CASE_DIR}/fixture-server" <<'MOCKSERVER'
#!/bin/bash
[[ "$#" -eq 1 && "$1" == "--version" ]] || {
  printf 'unexpected fixture binary invocation\n' >&2
  exit 64
}
printf '%s\n' "$0" >>"$NATS_BOOTSTRAP_TEST_VERSION_LOG"
printf '%s\n' "${NATS_BOOTSTRAP_TEST_CALLER:-single}" >>"$NATS_BOOTSTRAP_TEST_CALLER_LOG"
if [[ "${NATS_BOOTSTRAP_TEST_VERSION_DELAY:-}" == true ]]; then sleep 6; fi
printf '%s\n' "${NATS_BOOTSTRAP_TEST_VERSION:-nats-server: v2.15.1}"
MOCKSERVER
  chmod +x "${CASE_DIR}/fixture-server"
  : >"${CASE_DIR}/docker.log"
  : >"${CASE_DIR}/versions.log"
}

install_docker() {
  cat >"${CASE_BIN}/docker" <<'MOCKDOCKER'
#!/bin/bash
set -euo pipefail
while IFS= read -r variable; do
  [[ "$variable" != DOCKER* ]] || {
    printf 'Docker environment was not isolated: %s\n' "$variable" >&2
    exit 65
  }
done < <(compgen -e)
[[ "${1:-}" == "--host" && "${2:-}" == "unix:///var/run/docker.sock" ]] || {
  printf 'fixture requires the explicit local Unix Docker daemon\n' >&2
  exit 65
}
shift 2
[[ "${1:-}" == "--config" ]] || {
  printf 'fixture requires a private Docker config\n' >&2
  exit 65
}
docker_config="${2:-}"
[[ "$docker_config" == "$NATS_BOOTSTRAP_TEST_TMP/"* && -d "$docker_config" ]] || exit 65
[[ "$(stat -c '%a' "$docker_config")" == 700 ]] || exit 65
[[ -z "$(find "$docker_config" -mindepth 1 -maxdepth 1 -print -quit)" ]] || exit 65
shift 2
printf '%s\t' "$@" >>"$NATS_BOOTSTRAP_TEST_DOCKER_LOG"
printf '\n' >>"$NATS_BOOTSTRAP_TEST_DOCKER_LOG"
action="${1:-}"
shift
case "$action" in
  pull|create)
    pinned=false
    platform=false
    previous=""
    name=""
    for argument in "$@"; do
      [[ "$argument" != "--privileged" && "$argument" != "--mount" &&
        "$argument" != "--volume" && "$argument" != "-v" ]] || exit 66
      [[ "$argument" != "$NATS_BOOTSTRAP_TEST_IMAGE" ]] || pinned=true
      [[ "$previous" != "--platform" || "$argument" != "linux/amd64" ]] || platform=true
      [[ "$previous" != "--name" ]] || name="$argument"
      previous="$argument"
    done
    [[ "$pinned" == true && "$platform" == true ]] || {
      printf 'fixture expected digest-pinned Linux amd64 image\n' >&2
      exit 66
    }
    if [[ "$action" == pull ]]; then
      printf 'fixture image acquired\n'
    else
      [[ -n "$name" && ! -e "$NATS_BOOTSTRAP_TEST_CONTAINER" ]] || exit 66
      printf '%s\n' "$name" >"$NATS_BOOTSTRAP_TEST_CONTAINER"
      printf '%s\n' "$name" >>"$NATS_BOOTSTRAP_TEST_NAMES_LOG"
      printf 'fixture-created-%s\n' "$name"
    fi
    ;;
  cp)
    [[ "$#" -eq 2 ]] || exit 66
    [[ "$1" == "$(<"$NATS_BOOTSTRAP_TEST_CONTAINER"):/usr/local/bin/nats-server" ]] || exit 66
    destination="$2"
    if [[ "${NATS_BOOTSTRAP_TEST_COPY_MODE:-}" == fail ]]; then
      printf 'partial fixture copy\n' >"$destination"
      printf 'fixture copy failed\n' >&2
      exit 42
    fi
    if [[ "${NATS_BOOTSTRAP_TEST_COPY_MODE:-}" == hold ]]; then
      printf 'partial fixture copy\n' >"$destination"
      touch "$NATS_BOOTSTRAP_TEST_COPY_STARTED"
      for ((attempt = 0; attempt < 250; attempt++)); do
        [[ ! -e "$NATS_BOOTSTRAP_TEST_COPY_RELEASE" ]] || break
        sleep 0.02
      done
      [[ -e "$NATS_BOOTSTRAP_TEST_COPY_RELEASE" ]] || {
        printf 'fixture copy release deadline\n' >&2
        exit 43
      }
    fi
    cp "$NATS_BOOTSTRAP_TEST_BINARY" "$destination"
    ;;
  rm)
    target="${!#}"
    [[ "$target" == "$(<"$NATS_BOOTSTRAP_TEST_CONTAINER")" ]] || exit 66
    rm "$NATS_BOOTSTRAP_TEST_CONTAINER"
    ;;
  *)
    printf 'unsupported fixture Docker command: %s\n' "$action" >&2
    exit 66
    ;;
esac
MOCKDOCKER
  chmod +x "${CASE_BIN}/docker"
}

run_resolver() {
  local -a command=(
    "$REAL_ENV" -i "PATH=$CASE_BIN" "TMPDIR=$CASE_TMP"
    "NATS_BOOTSTRAP_TEST_TMP=$CASE_TMP"
    "NATS_BOOTSTRAP_TEST_DOCKER_LOG=${CASE_DIR}/docker.log"
    "NATS_BOOTSTRAP_TEST_VERSION_LOG=${CASE_DIR}/versions.log"
    "NATS_BOOTSTRAP_TEST_CALLER_LOG=${CASE_DIR}/callers.log"
    "NATS_BOOTSTRAP_TEST_NAMES_LOG=${TEST_TEMP}/container-names.log"
    "NATS_BOOTSTRAP_TEST_IMAGE=$PIN_IMAGE"
    "NATS_BOOTSTRAP_TEST_BINARY=${CASE_DIR}/fixture-server"
    "NATS_BOOTSTRAP_TEST_CONTAINER=${CASE_DIR}/container"
    "NATS_BOOTSTRAP_TEST_COPY_STARTED=${CASE_DIR}/copy-started"
    "NATS_BOOTSTRAP_TEST_COPY_RELEASE=${CASE_DIR}/copy-release"
  )
  if [[ "${1:-}" == --exec ]]; then
    shift
    exec "${command[@]}" "$@" /bin/bash "$RESOLVER"
  fi
  "${command[@]}" "$@" /bin/bash "$RESOLVER"
}

expect_success() {
  if ! run_resolver "$@" >"${CASE_DIR}/stdout" 2>"${CASE_DIR}/stderr"; then
    cat "${CASE_DIR}/stderr" >&2
    fail "resolver unexpectedly failed in $(basename "$CASE_DIR")"
  fi
  RESOLVED="$(<"${CASE_DIR}/stdout")"
  [[ "$RESOLVED" == /* && -x "$RESOLVED" ]] || fail "stdout was not an absolute executable path"
  [[ "$RESOLVED" != *$'\n'* && "$(wc -l <"${CASE_DIR}/stdout")" -eq 1 ]] ||
    fail "resolver stdout contained more than its one path"
}

expect_failure() {
  local diagnostic="$1"
  shift
  if run_resolver "$@" >"${CASE_DIR}/stdout" 2>"${CASE_DIR}/stderr"; then
    fail "resolver unexpectedly succeeded in $(basename "$CASE_DIR")"
  fi
  [[ ! -s "${CASE_DIR}/stdout" ]] || fail "failed acquisition published a path"
  if ! grep -Ei -- "$diagnostic" "${CASE_DIR}/stderr" >/dev/null; then
    cat "${CASE_DIR}/stderr" >&2
    fail "missing expected failure diagnosis: $diagnostic"
  fi
}

expect_no_docker() {
  [[ ! -s "${CASE_DIR}/docker.log" ]] || fail "native resolution unexpectedly reached Docker"
}

expect_owned_lifecycle() {
  local -a actions
  mapfile -t actions < <(cut -f1 "${CASE_DIR}/docker.log")
  [[ "${actions[*]}" == "pull create cp rm" ]] ||
    fail "expected stopped-container pull/create/copy/remove order, got ${actions[*]}"
  [[ ! -e "${CASE_DIR}/container" ]] || fail "owned stopped container was not removed"
}

cache_path() {
  printf '%s/concordvoice-test-tools-%s/nats-server-2.15.1-%s-linux-amd64/nats-server\n' \
    "$CASE_TMP" "$(id -u)" "$PIN_DIGEST"
}

setup_case explicit
install_docker
cp "${CASE_DIR}/fixture-server" "${CASE_BIN}/nats-server"
expect_success NATS_SERVER_BINARY="${CASE_DIR}/fixture-server" \
  NATS_BOOTSTRAP_TEST_VERSION='  nats-server: v2.15.1  '
[[ "$RESOLVED" == "${CASE_DIR}/fixture-server" ]] || fail "explicit override lost precedence"
[[ "$(<"${CASE_DIR}/versions.log")" == "${CASE_DIR}/fixture-server" ]] ||
  fail "explicit override did not receive the version probe"
expect_no_docker
printf 'PASS: explicit override takes precedence and validates the pinned version\n'

setup_case path
cp "${CASE_DIR}/fixture-server" "${CASE_BIN}/nats-server"
expect_success NATS_BOOTSTRAP_TEST_OS=Darwin NATS_BOOTSTRAP_TEST_ARCH=arm64
[[ "$RESOLVED" == "${CASE_BIN}/nats-server" ]] || fail "PATH binary was not selected"
expect_no_docker
printf 'PASS: validated native PATH binary works without Docker on other platforms\n'

for invalid in empty absent nonexecutable version; do
  setup_case "explicit-${invalid}"
  install_docker
  cp "${CASE_DIR}/fixture-server" "${CASE_BIN}/nats-server"
  case "$invalid" in
    empty) candidate="" ;;
    absent) candidate="${CASE_DIR}/missing-server" ;;
    nonexecutable)
      candidate="${CASE_DIR}/fixture-server"
      chmod -x "$candidate"
      ;;
    version) candidate="${CASE_DIR}/fixture-server" ;;
  esac
  if [[ "$invalid" == version ]]; then
    expect_failure 'version|2\.15\.1' NATS_SERVER_BINARY="$candidate" \
      NATS_BOOTSTRAP_TEST_VERSION='nats-server: v2.15.10'
  else
    expect_failure 'NATS_SERVER_BINARY' NATS_SERVER_BINARY="$candidate"
  fi
  expect_no_docker
done
printf 'PASS: empty/invalid explicit override fails without fallback\n'

setup_case path-version
install_docker
cp "${CASE_DIR}/fixture-server" "${CASE_BIN}/nats-server"
expect_failure 'version|2\.15\.1' NATS_BOOTSTRAP_TEST_VERSION='nats-server: v2.15.0'
expect_no_docker
printf 'PASS: wrong-version PATH binary fails without Docker fallback\n'

setup_case native-version-timeout
install_docker
expect_failure 'version|timed out|timeout' NATS_SERVER_BINARY="${CASE_DIR}/fixture-server" \
  NATS_BOOTSTRAP_TEST_VERSION_DELAY=true
expect_no_docker
[[ -s "${CASE_DIR}/versions.log" ]] || fail "version timeout case never reached the probe"
printf 'PASS: native version probe times out with a diagnosed hard failure\n'

setup_case cold
install_docker
expect_success DOCKER_HOST=tcp://fixture.invalid:2375 DOCKER_CONTEXT=fixture-remote \
  DOCKER_TLS_VERIFY=1 DOCKER_CERT_PATH=/fixture-unavailable DOCKER_FIXTURE_EXTRA=discard-me
[[ "$RESOLVED" == "$(cache_path)" ]] || fail "cold acquisition returned an unexpected cache path"
expect_owned_lifecycle
cache_root="${CASE_TMP}/concordvoice-test-tools-$(id -u)"
[[ "$(stat -c '%a' "$cache_root")" == 700 ]] || fail "cache root was not private"
[[ "$(stat -c '%a' "$(dirname "$RESOLVED")")" == 700 ]] || fail "cache key was not private"
[[ "$(stat -c '%u' "$cache_root")" == "$(id -u)" ]] || fail "cache root owner did not match"
[[ -s "${CASE_DIR}/versions.log" ]] || fail "copied native binary was not version-checked"
: >"${CASE_DIR}/docker.log"
: >"${CASE_DIR}/versions.log"
expect_success
expect_no_docker
[[ "$(<"${CASE_DIR}/versions.log")" == "$RESOLVED" ]] || fail "cache hit bypassed version validation"
printf 'PASS: pinned stopped-container bootstrap publishes a private validated cache; warm hit revalidates\n'

setup_case missing-docker
expect_failure 'docker'
printf 'PASS: missing Docker is a diagnosed hard failure\n'

setup_case unsupported-platform
install_docker
expect_failure 'platform|linux|amd64' NATS_BOOTSTRAP_TEST_OS=Darwin NATS_BOOTSTRAP_TEST_ARCH=arm64
expect_no_docker
printf 'PASS: unsupported Docker extraction platform fails before container creation\n'

setup_case copy-failure
install_docker
expect_failure 'copy|cp|acquisition' NATS_BOOTSTRAP_TEST_COPY_MODE=fail
expect_owned_lifecycle
[[ ! -e "$(cache_path)" ]] || fail "partial copy was published"
printf 'PASS: copy failure removes the owned stopped container and publishes no binary\n'

setup_case copied-version
install_docker
expect_failure 'version|2\.15\.1' NATS_BOOTSTRAP_TEST_VERSION='nats-server: v2.15.0'
expect_owned_lifecycle
[[ ! -e "$(cache_path)" ]] || fail "wrong-version copied binary was published"
printf 'PASS: copied wrong-version binary fails validation without publication\n'

setup_case cached-version
install_docker
expect_success
: >"${CASE_DIR}/docker.log"
expect_failure 'version|2\.15\.1' NATS_BOOTSTRAP_TEST_VERSION='nats-server: v2.15.0'
expect_no_docker
printf 'PASS: cached wrong-version binary fails without returning a path\n'

setup_case symlink-cache
install_docker
mkdir "${CASE_DIR}/cache-target"
printf 'unchanged\n' >"${CASE_DIR}/cache-target/marker"
ln -s "${CASE_DIR}/cache-target" "${CASE_TMP}/concordvoice-test-tools-$(id -u)"
expect_failure 'symlink|cache'
expect_no_docker
[[ "$(<"${CASE_DIR}/cache-target/marker")" == unchanged ]] || fail "symlink cache target was modified"
printf 'PASS: cache root symlink is refused\n'

setup_case concurrent
install_docker
expected_path="$(cache_path)"
run_resolver --exec NATS_BOOTSTRAP_TEST_COPY_MODE=hold NATS_BOOTSTRAP_TEST_CALLER=first \
  >"${CASE_DIR}/first.stdout" \
  2>"${CASE_DIR}/first.stderr" &
first_pid=$!
background_pids=("$first_pid")
for ((attempt = 0; attempt < 250; attempt++)); do
  [[ ! -e "${CASE_DIR}/copy-started" ]] || break
  kill -0 "$first_pid" 2>/dev/null || {
    wait "$first_pid" || true
    cat "${CASE_DIR}/first.stderr" >&2
    fail "first cold acquisition exited before its copy barrier"
  }
  sleep 0.02
done
[[ -e "${CASE_DIR}/copy-started" ]] || fail "copy barrier was not reached"
run_resolver --exec NATS_BOOTSTRAP_TEST_CALLER=second \
  >"${CASE_DIR}/second.stdout" 2>"${CASE_DIR}/second.stderr" &
second_pid=$!
background_pids+=("$second_pid")
sleep 0.1
[[ ! -e "$expected_path" ]] || fail "partial binary became visible at the published cache path"
[[ ! -s "${CASE_DIR}/first.stdout" && ! -s "${CASE_DIR}/second.stdout" ]] ||
  fail "a concurrent caller returned during an incomplete copy"
kill -0 "$second_pid" 2>/dev/null || fail "concurrent caller did not wait for completed publication"
touch "${CASE_DIR}/copy-release"
if ! wait "$first_pid"; then
  cat "${CASE_DIR}/first.stderr" >&2
  fail "first concurrent acquisition failed"
fi
if ! wait "$second_pid"; then
  cat "${CASE_DIR}/second.stderr" >&2
  fail "second concurrent acquisition failed"
fi
background_pids=()
[[ "$(<"${CASE_DIR}/first.stdout")" == "$expected_path" &&
  "$(<"${CASE_DIR}/second.stdout")" == "$expected_path" ]] ||
  fail "concurrent callers did not receive the same completed cache binary"
expect_owned_lifecycle
grep -Fx first "${CASE_DIR}/callers.log" >/dev/null ||
  fail "first caller did not validate its native binary"
grep -Fx second "${CASE_DIR}/callers.log" >/dev/null ||
  fail "second caller did not validate the published cache binary"
printf 'PASS: concurrent cold acquisition waits for atomic validated publication\n'

setup_case cancellation
install_docker
run_resolver --exec NATS_BOOTSTRAP_TEST_COPY_MODE=hold >"${CASE_DIR}/stdout" \
  2>"${CASE_DIR}/stderr" &
cancellation_pid=$!
background_pids=("$cancellation_pid")
for ((attempt = 0; attempt < 250; attempt++)); do
  [[ ! -e "${CASE_DIR}/copy-started" ]] || break
  kill -0 "$cancellation_pid" 2>/dev/null || {
    wait "$cancellation_pid" || true
    cat "${CASE_DIR}/stderr" >&2
    fail "cancellation acquisition exited before staged copy"
  }
  sleep 0.02
done
[[ -e "${CASE_DIR}/copy-started" ]] || fail "cancellation copy barrier was not reached"
cancellation_started=$SECONDS
kill -TERM "$cancellation_pid"
if wait "$cancellation_pid"; then fail "cancelled acquisition returned success"; fi
background_pids=()
[[ "$((SECONDS - cancellation_started))" -le 5 ]] || fail "SIGTERM cleanup exceeded its bounded grace"
[[ ! -s "${CASE_DIR}/stdout" ]] || fail "cancelled acquisition published a path"
expect_owned_lifecycle
[[ ! -e "$(cache_path)" ]] || fail "cancelled partial binary was published"
printf 'PASS: SIGTERM during staged copy cleans only the owned container before returning failure\n'
[[ "$(sort -u "${TEST_TEMP}/container-names.log" | wc -l)" -eq \
  "$(wc -l <"${TEST_TEMP}/container-names.log")" ]] ||
  fail "separate cold acquisitions reused a stopped-container name"
printf 'PASS: every cold acquisition uses a unique stopped-container name\n'
printf 'test-nats-server-bootstrap: PASS\n'
