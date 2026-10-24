#!/usr/bin/env bash
# Shared fail-closed public tooling image preparation for self-host cert tests.
selfhost_cert_prepare_tool_image() {
  command -v docker >/dev/null 2>&1 || {
    printf '%s\n' 'FAIL: required Docker CLI unavailable' >&2
    return 1
  }
  docker info >/dev/null 2>&1 || {
    printf '%s\n' 'FAIL: required Docker daemon unavailable' >&2
    return 1
  }
  local image build_dir
  if [[ -n "${SELFHOST_CERT_TEST_IMAGE:-}" ]]; then
    image="$SELFHOST_CERT_TEST_IMAGE"
  else
    # Bump the recipe tag so a cached image without the validators is not reused.
    image=concord-2504-cert-test-tools:ubuntu24-v2
    if ! docker image inspect "$image" >/dev/null 2>&1; then
      build_dir="$(mktemp -d)"
      cat >"$build_dir/Dockerfile" <<'DOCKERFILE'
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends bash coreutils openssl util-linux findutils gawk grep sed python3 logrotate systemd \
    && rm -rf /var/lib/apt/lists/*
DOCKERFILE
      if ! docker build --network default -t "$image" "$build_dir" >/dev/null; then
        rm -rf "$build_dir"
        printf '%s\n' 'FAIL: public acceptance tooling image bootstrap failed' >&2
        return 1
      fi
      rm -rf "$build_dir"
    fi
  fi
  docker image inspect "$image" >/dev/null 2>&1 || {
    printf '%s\n' 'FAIL: required acceptance tooling image unavailable' >&2
    return 1
  }
  # Check cached and explicitly supplied images with the real, executable tools.
  if ! docker run --rm --network none --security-opt no-new-privileges --pids-limit 64 --user 0:0 \
    "$image" bash -euc '
      for utility in logrotate systemd-analyze; do
        command -v "$utility" >/dev/null || {
          printf "FAIL: required validator unavailable: %s\n" "$utility" >&2
          exit 1
        }
        "$utility" --version >/dev/null
      done
    '; then
    printf '%s\n' 'FAIL: acceptance tooling image must provide working logrotate and systemd-analyze validators' >&2
    return 1
  fi
  printf '%s' "$image"
}
