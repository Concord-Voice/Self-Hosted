#!/usr/bin/env bash
# Resolve the pinned native broker for real JetStream tests in either service.
set -euo pipefail
umask 077

readonly nats_version="2.15.1"
readonly nats_digest="2ae57dfba3063d5e25fecb75256118c0882a1a2d2cf7d0fba17a7c701df956f6" # pragma: allowlist secret -- public official NATS image manifest digest
readonly nats_image="nats:${nats_version}-alpine@sha256:${nats_digest}"
readonly acquisition_deadline=$((SECONDS + 115))
work_dir=""
owned_container=""
container_cleanup_attempted=false
active_timeout_pid=""
resolved_path=""
docker_command=()

fail() {
  printf 'NATS test broker: %s\n' "$*" >&2
  exit 1
}

run_bounded() {
  local limit="$1" remaining status
  shift
  remaining=$((acquisition_deadline - SECONDS))
  [[ "$remaining" -gt 0 ]] || return 124
  if [[ "$limit" -gt "$remaining" ]]; then limit="$remaining"; fi
  # Waiting on the owned child lets TERM interrupt immediately. GNU timeout
  # owns a separate process group, including the command's descendants.
  timeout --signal=TERM --kill-after=1 "${limit}s" "$@" &
  active_timeout_pid=$!
  if wait "$active_timeout_pid"; then status=0; else status=$?; fi
  active_timeout_pid=""
  return "$status"
}

stop_active_command() {
  if [[ -n "$active_timeout_pid" ]]; then
    if ! kill -KILL -- "-${active_timeout_pid}" 2>/dev/null; then
      kill -KILL "$active_timeout_pid" 2>/dev/null || true
    fi
    wait "$active_timeout_pid" 2>/dev/null || true
    active_timeout_pid=""
  fi
}

remove_owned_container() {
  if [[ -z "$owned_container" || "$container_cleanup_attempted" == true ]]; then return 0; fi
  container_cleanup_attempted=true
  if ! timeout --signal=TERM --kill-after=1 4s "${docker_command[@]}" rm "$owned_container" >&2; then
    printf 'NATS test broker: could not remove owned stopped container %s\n' "$owned_container" >&2
    return 1
  fi
  owned_container=""
}

cleanup_resources() {
  local status=0
  stop_active_command
  remove_owned_container || status=1
  if [[ -n "$work_dir" ]]; then
    if rm -rf -- "$work_dir"; then
      work_dir=""
    else
      printf 'NATS test broker: could not remove owned staging directory\n' >&2
      status=1
    fi
  fi
  return "$status"
}

cleanup_exit() {
  local status="$1"
  trap - EXIT
  trap '' TERM INT HUP
  cleanup_resources || status=1
  exit "$status"
}

trap 'cleanup_exit "$?"' EXIT
trap 'exit 143' TERM
trap 'exit 130' INT
trap 'exit 129' HUP

verify_version() {
  local candidate="$1" output
  if ! run_bounded 5 "$candidate" --version >"${work_dir}/version"; then
    fail "nats-server version probe failed or timed out (requires v${nats_version})"
  fi
  output="$(<"${work_dir}/version")"
  output="${output//[[:space:]]/}"
  [[ "$output" == "nats-server:v${nats_version}" ]] || fail "nats-server version must be v${nats_version}"
}

resolve_native() {
  local candidate="$1" source="$2" directory
  [[ -n "$candidate" && -f "$candidate" && -x "$candidate" ]] || fail "${source} must name an executable nats-server"
  [[ "$candidate" != *$'\n'* && "$candidate" != *$'\r'* ]] || fail "${source} path contains a newline"
  directory="$(cd -P -- "$(dirname -- "$candidate")" && pwd)" || fail "${source} directory is unavailable"
  resolved_path="${directory}/$(basename -- "$candidate")"
  verify_version "$resolved_path"
}

private_cache_directory() {
  local directory="$1" ownership
  [[ ! -L "$directory" ]] || fail "cache directory must not be a symlink"
  if ! mkdir -m 700 -- "$directory" 2>/dev/null && [[ ! -d "$directory" ]]; then
    fail "private cache directory is unavailable"
  fi
  [[ -d "$directory" && ! -L "$directory" ]] || fail "cache directory must be a real directory"
  ownership="$(stat -c '%u:%a' -- "$directory")" || fail "cache ownership cannot be verified"
  [[ "$ownership" == "${UID}:700" ]] || fail "cache directory must be owned by this user with mode 0700"
}

acquire_cached_binary() {
  local temp_base="$1" cache_root cache_dir cache_file lock_fd docker_binary docker_variable
  [[ "$(uname -s)" == Linux && "$(uname -m)" == x86_64 ]] || fail "Docker extraction supports only Linux amd64"
  command -v flock >/dev/null || fail "flock is required for bounded cache acquisition"
  cache_root="${temp_base}/concordvoice-test-tools-${UID}"
  cache_dir="${cache_root}/nats-server-${nats_version}-${nats_digest}-linux-amd64"
  private_cache_directory "$cache_root"
  private_cache_directory "$cache_dir"
  cache_file="${cache_dir}/nats-server"
  [[ ! -L "${cache_dir}/lock" ]] || fail "cache lock must not be a symlink"
  exec {lock_fd}>>"${cache_dir}/lock"
  run_bounded 115 flock -x "$lock_fd" || fail "cache acquisition lock failed or timed out"
  if [[ -e "$cache_file" || -L "$cache_file" ]]; then
    [[ -f "$cache_file" && -x "$cache_file" && ! -L "$cache_file" ]] || fail "cached nats-server must be a real executable"
    verify_version "$cache_file"
    resolved_path="$cache_file"
    return
  fi
  docker_binary="$(command -v docker)" || fail "Docker is required when no native nats-server is available"
  mkdir -m 700 -- "${work_dir}/docker-config" || fail "private Docker configuration could not be created"
  docker_command=(env)
  for docker_variable in "${!DOCKER@}"; do
    docker_command+=(-u "$docker_variable")
  done
  docker_command+=("$docker_binary" --host unix:///var/run/docker.sock --config "${work_dir}/docker-config")
  run_bounded 115 "${docker_command[@]}" pull --platform linux/amd64 "$nats_image" >&2 || fail "pinned Docker image pull failed or timed out"
  owned_container="concordvoice-nats-test-$(basename -- "$work_dir")"
  # Record the unique name before create: even an ambiguous create failure must
  # attempt removal of this invocation's container, and no other container.
  run_bounded 115 "${docker_command[@]}" create --platform linux/amd64 --name "$owned_container" "$nats_image" >&2 || fail "stopped Docker container creation failed or timed out"
  run_bounded 115 "${docker_command[@]}" cp "${owned_container}:/usr/local/bin/nats-server" "${work_dir}/nats-server" >&2 || fail "native broker copy failed or timed out"
  [[ -f "${work_dir}/nats-server" && ! -L "${work_dir}/nats-server" ]] || fail "copied broker must be a real file"
  chmod 700 -- "${work_dir}/nats-server" || fail "copied broker could not be made executable"
  verify_version "${work_dir}/nats-server"
  remove_owned_container || fail "stopped Docker container cleanup failed"
  # Both paths are in the same owned temporary filesystem. Publication is one
  # rename under the exclusive lock; a partial binary is never visible.
  mv -- "${work_dir}/nats-server" "$cache_file" || fail "validated broker cache publication failed"
  resolved_path="$cache_file"
}

main() {
  local temp_base candidate
  [[ "$#" -eq 0 ]] || fail "resolver accepts no arguments"
  command -v timeout >/dev/null || fail "timeout is required for bounded broker acquisition"
  temp_base="${TMPDIR:-/tmp}"
  [[ "$temp_base" != *$'\n'* && "$temp_base" != *$'\r'* ]] || fail "temporary directory path contains a newline"
  temp_base="$(cd -P -- "$temp_base" && pwd)" || fail "temporary directory is unavailable"
  work_dir="$(mktemp -d "${temp_base}/concordvoice-nats-server.XXXXXXXX")" || fail "private staging directory could not be created"
  if [[ "${NATS_SERVER_BINARY+x}" ]]; then
    resolve_native "$NATS_SERVER_BINARY" NATS_SERVER_BINARY
  elif candidate="$(command -v nats-server)"; then
    resolve_native "$candidate" PATH
  else
    acquire_cached_binary "$temp_base"
  fi
  cleanup_resources || fail "owned acquisition cleanup failed"
  trap - EXIT
  printf '%s\n' "$resolved_path"
}

main "$@"
