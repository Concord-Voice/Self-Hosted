#!/usr/bin/env bash
# Run the complete self-host hook suite on its supported Linux toolchain.
set -euo pipefail

# Hooks export git internals that would redirect fixture git operations to this worktree.
# shellcheck disable=SC2046 # git's variable list is intentionally word-split.
unset $(git rev-parse --local-env-vars)

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
SUITES=(
  scripts/tests/test-install-selfhost.sh
  scripts/tests/test-provision-cert.sh
  scripts/tests/test-concord-selfhost.sh
  infrastructure/deploy/tests/test-selfhost-storage.sh
  infrastructure/deploy/tests/test-selfhost-byo-s3.sh
  infrastructure/deploy/tests/test-selfhost-cert-delivery.sh
  infrastructure/deploy/tests/test-selfhost-tls-readiness.sh
  infrastructure/deploy/tests/test-selfhost-tls-callers.sh
  infrastructure/deploy/tests/test-copy-certs.sh
  infrastructure/deploy/tests/test-selfhost-tls-selected-consumer.sh
  infrastructure/deploy/tests/test-security-event-logging.sh
  infrastructure/deploy/tests/test-control-plane-secrets.sh
  infrastructure/deploy/tests/test-selfhost-acceptance.sh
)

if [[ $(uname -s) == Darwin ]]; then
  command -v docker >/dev/null || { printf 'FAIL: Docker CLI required for Linux self-host hook\n' >&2; exit 1; }
  docker info >/dev/null || { printf 'FAIL: Docker daemon required for Linux self-host hook\n' >&2; exit 1; }
  docker_host="$(docker context inspect --format '{{ .Endpoints.docker.Host }}')"
  [[ $docker_host == unix:///* ]] || { printf 'FAIL: Unix Docker socket required for Linux self-host hook\n' >&2; exit 1; }
  docker_socket="${docker_host#unix://}"
  [[ -S $docker_socket ]] || { printf 'FAIL: Docker socket unavailable\n' >&2; exit 1; }
  git_common="$(git -C "$ROOT" rev-parse --path-format=absolute --git-common-dir)"
  [[ $git_common == /* && -d $git_common ]] || { printf 'FAIL: git common directory unavailable\n' >&2; exit 1; }
  scratch="$(mktemp -d /private/tmp/concord-selfhost-hook.XXXXXX)"
  trap 'rm -rf -- "$scratch"' EXIT

  image=concord-selfhost-hook:ubuntu24-v2
  docker build -f "$ROOT/infrastructure/deploy/tests/Dockerfile.selfhost-hook" -t "$image" "$ROOT/infrastructure/deploy/tests"
  # shellcheck source=infrastructure/deploy/tests/selfhost-cert-test-tools.sh
  source "$ROOT/infrastructure/deploy/tests/selfhost-cert-test-tools.sh"
  cert_image="$(selfhost_cert_prepare_tool_image)"
  # The nested Docker daemon sees host paths. Keep source and scratch at their
  # identical absolute paths so config-only Compose and cert fixtures can bind them.
  for suite in "${SUITES[@]}"; do
    network=none
    bridge_env=()
    if [[ $suite == infrastructure/deploy/tests/test-copy-certs.sh ||
          $suite == infrastructure/deploy/tests/test-selfhost-tls-selected-consumer.sh ]]; then
      network=bridge
      bridge_env=(--env SELFHOST_HOOK_MAC_BRIDGE=1)
    fi
    suite_tmpdir="$scratch"
    if [[ $suite == scripts/tests/test-install-selfhost.sh ||
          $suite == scripts/tests/test-concord-selfhost.sh ||
          $suite == infrastructure/deploy/tests/test-selfhost-acceptance.sh ||
          $suite == infrastructure/deploy/tests/test-security-event-logging.sh ||
          $suite == infrastructure/deploy/tests/test-control-plane-secrets.sh ]]; then
      # Docker Desktop bind mounts corrupt fixture bytes and mode bits; these suites need local scratch.
      suite_tmpdir=/tmp
    fi
    printf 'Running %s\n' "$suite"
    docker run --rm --network "$network" --security-opt no-new-privileges \
      --user "$(id -u):$(id -g)" --group-add 0 --workdir "$ROOT" \
      --mount "type=bind,source=$ROOT,target=$ROOT,readonly" \
      --mount "type=bind,source=$git_common,target=$git_common,readonly" \
      --mount "type=bind,source=$scratch,target=$scratch" \
      --mount "type=bind,source=$docker_socket,target=/var/run/docker.sock" \
      --env DOCKER_HOST=unix:///var/run/docker.sock --env TMPDIR="$suite_tmpdir" \
      --env HOME="$scratch" --env SELFHOST_CERT_TEST_IMAGE="$cert_image" \
      "${bridge_env[@]}" "$image" bash "$suite"
  done
  exit 0
fi

cd "$ROOT"
for suite in "${SUITES[@]}"; do
  printf 'Running %s\n' "$suite"
  bash "$suite"
done
