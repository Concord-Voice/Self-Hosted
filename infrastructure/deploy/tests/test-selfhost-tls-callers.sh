#!/usr/bin/env bash
# Actual concord-selfhost wrapper caller coverage for the TLS boundary.
# All model files, commands, and helper functions live in a private fixture.
set -euo pipefail
fails=0
pass() { printf 'PASS: %s\n' "$1"; }
fail() { printf 'FAIL: %s\n' "$1" >&2; fails=$((fails + 1)); }

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fixture="$TMP/project"
stub="$TMP/stub"
mkdir -p "$fixture/infrastructure/deploy" "$fixture/security-logs" "$stub"
cp "$ROOT/infrastructure/deploy/concord-selfhost" "$fixture/infrastructure/deploy/concord-selfhost"
cp "$ROOT/infrastructure/deploy/selfhost-storage.sh" "$fixture/infrastructure/deploy/selfhost-storage.sh"
for file in docker-compose.yml docker-compose.production.yml docker-compose.selfhost.yml; do
  cp "$ROOT/$file" "$fixture/$file"
done
printf '%s\n' 'INSTANCE_TYPE=self-hosted' 'SELFHOST_STORAGE_MODE=bundled' >"$fixture/.env"

# The wrapper's storage resolver sees a complete bundled selected model. The
# wrapper's separate controller port-check is an independent successful control;
# this fixture isolates whether wrapper health also consumes TLS readiness.
cat >"$fixture/infrastructure/deploy/selfhost-tls.sh" <<'TLS_STUB'
selfhost_tls_admit() {
  printf 'admit|%s\n' "$*" >>"${STUB_TLS_LOG:?}"
}
selfhost_tls_ready() {
  printf 'ready|%s\n' "$*" >>"${STUB_TLS_LOG:?}"
  echo 'selfhost-tls: fixture listener readiness refused' >&2
  return 1
}
TLS_STUB

cat >"$stub/docker" <<'DOCKER_STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_DOCKER_LOG"
if [[ " $* " == *" --env-file /dev/null -f - config --format json "* ]]; then
  printf '{"name":"%s"}\n' "$(basename "$STUB_PROJECT_ROOT")"
  exit 0
fi
if [[ " $* " == *" config --format json "* ]]; then
  printf '{"name":"%s","services":{}}\n' "$(basename "$STUB_PROJECT_ROOT")"
  exit 0
fi
if [[ "${1:-}" == compose ]]; then
  root="$STUB_PROJECT_ROOT"
  expected=(compose --env-file "$root/.env" --project-directory "$root"
    --project-name "${root##*/}" --profile services
    -f "$root/docker-compose.yml" -f "$root/docker-compose.production.yml"
    -f "$root/docker-compose.selfhost.yml" down)
  args=("$@")
  if (( ${#args[@]} == ${#expected[@]} )); then
    exact=1
    for ((i = 0; i < ${#expected[@]}; i++)); do
      [[ "${args[$i]}" == "${expected[$i]}" ]] || exact=0
    done
    if [[ $exact == 1 ]]; then
      printf 'selected bundled compose down\n' >>"$STUB_DOCKER_LOG"
      exit 0
    fi
  fi
fi
if [[ "${1:-}" == compose && " ${*: -1} " == *" ps "* ]]; then exit 0; fi
printf 'unexpected fixture Docker request\n' >&2
exit 97
DOCKER_STUB
chmod +x "$stub/docker"

cat >"$stub/curl" <<'CURL_STUB'
#!/usr/bin/env bash
url="${*: -1}"
if [[ "$url" == http://127.0.0.1:8080/api/v1/client/config ]]; then
  printf '200'
  exit 0
fi
exit 1
CURL_STUB
chmod +x "$stub/curl"

cat >"$stub/controller" <<'CTL_STUB'
#!/usr/bin/env bash
printf 'contract=%s override=%s profile=%s argv=%s\n' \
  "${ACTIVITY_HISTORY_CONTRACT_MODE-}" "${COMPOSE_OVERRIDE-}" \
  "${COMPOSE_PROFILES-}" "$*" >>"$STUB_CTL_LOG"
case "$*" in
  selfhost-port-check) echo 'selfhost-posture: live port bindings match the self-host allowlist'; exit 0 ;;
  rebuild) exit 0 ;;
  *) exit 2 ;;
esac
CTL_STUB
chmod +x "$stub/controller"

export PATH="$stub:$PATH"
export STUB_PROJECT_ROOT="$fixture"
export STUB_DOCKER_LOG="$TMP/docker.log"
export STUB_CTL_LOG="$TMP/controller.log"
export STUB_TLS_LOG="$TMP/tls.log"
export CONCORD_SECURITY_LOG_ROOT_OVERRIDE="$fixture/security-logs"
export CONCORD_CTL_OVERRIDE="$stub/controller"
: >"$STUB_DOCKER_LOG"
: >"$STUB_CTL_LOG"
: >"$STUB_TLS_LOG"

wrapper="$fixture/infrastructure/deploy/concord-selfhost"
set +e
health_output="$(bash "$wrapper" health 2>&1)"
health_rc=$?
set -e
if [[ $health_rc -ne 0 ]] \
  && grep -qF 'control-plane healthy (HTTP 200)' <<<"$health_output" \
  && grep -qF 'published ports match the self-host posture' <<<"$health_output" \
  && grep -qF 'selfhost-tls: fixture listener readiness refused' <<<"$health_output" \
  && grep -qF 'selfhost-port-check' "$STUB_CTL_LOG" \
  && grep -q '^ready|' "$STUB_TLS_LOG"; then
  pass 'actual wrapper health aggregates TLS after successful HTTP and port-posture controls'
else
  fail "wrapper health omitted or hid required TLS proof (rc=$health_rc output=$health_output ready_log=$(cat "$STUB_TLS_LOG"))"
fi

# `up` belongs to the controller's single unscoped rebuild gate. The wrapper
# delegates once after its existing security-log admission and owns no second
# TLS observation window of its own.
: >"$STUB_CTL_LOG"
: >"$STUB_TLS_LOG"
set +e
up_output="$(bash "$wrapper" up 2>&1)"
up_rc=$?
set -e
if [[ $up_rc == 0 ]] && [[ "$(cat "$STUB_CTL_LOG")" == \
  'contract=selfhost override=docker-compose.production.yml profile=services argv=rebuild' ]] \
  && [[ ! -s "$STUB_TLS_LOG" ]]; then
  pass 'actual wrapper up delegates one unscoped rebuild without a duplicate TLS wait'
else
  fail "wrapper up duplicated or bypassed the controller TLS boundary (rc=$up_rc output=$up_output ctl=$(cat "$STUB_CTL_LOG"), tls=$(cat "$STUB_TLS_LOG"))"
fi

# Down is an explicit recovery operation and remains available with the fake
# TLS listener refusing. It reaches the actual wrapper's selected compose model.
: >"$STUB_DOCKER_LOG"
: >"$STUB_TLS_LOG"
set +e
down_output="$(bash "$wrapper" down 2>&1)"
down_rc=$?
set -e
if [[ $down_rc == 0 ]] \
  && grep -q '^selected bundled compose down$' "$STUB_DOCKER_LOG" \
  && grep -q -- '--profile services' "$STUB_DOCKER_LOG" \
  && [[ ! -s "$STUB_TLS_LOG" ]]; then
  pass 'actual wrapper down remains usable against the selected model while TLS readiness refuses'
else
  fail "wrapper down was TLS-gated or missed selected compose down (rc=$down_rc output=$down_output docker=$(cat "$STUB_DOCKER_LOG"), tls=$(cat "$STUB_TLS_LOG"))"
fi

if (( fails > 0 )); then exit 1; fi
