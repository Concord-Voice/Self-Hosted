#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
FIX="$(mktemp -d)"
trap 'rm -rf "$FIX"' EXIT
if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
  # The protected cache-cold builder supplies only the control-plane stub.
  compose_proxy_routes() {
    env -i PATH="$PATH" HOME="${HOME:-/tmp}" \
      POSTGRES_PASSWORD=ci-stub REDIS_PASSWORD=ci-stub JWT_SECRET=ci-stub \
      TURN_SECRET=ci-stub MINIO_ROOT_USER=ci-stub MINIO_ROOT_PASSWORD=ci-stub \
      MFA_ENCRYPTION_KEY=ci-stub DOMAIN=ci-stub.invalid SMTP_HOST=ci-stub.invalid \
      SMTP_FROM=ci-stub@invalid.example PUBLIC_IP=127.0.0.1 \
      FEEDBACK_PAT=ci-stub FEEDBACK_REPO=ci-stub/repo \
      WS_EVENT_BUDGET_ENABLED=true WS_EVENT_BUDGET_SCALE_PERCENT=100 \
      "$@" docker compose -p concord --env-file /dev/null \
      -f "$ROOT/docker-compose.yml" -f "$ROOT/docker-compose.production.yml" \
      --profile services config --format json \
      | python3 -c 'import json,sys; model=json.load(sys.stdin); print(model["services"]["control-plane"]["environment"]["TRUSTED_PROXY_CIDRS"], model["services"]["media-plane"]["environment"]["TRUSTED_PROXIES"])'
  }
  [[ "$(compose_proxy_routes TRUSTED_PROXY_CIDRS=10.0.0.0/8)" == '10.0.0.0/8 10.0.0.0/8' ]]
  [[ "$(compose_proxy_routes TRUSTED_PROXY_CIDRS=172.28.0.1/32 TRUSTED_PROXIES=172.28.0.1/32)" == '172.28.0.1/32 172.28.0.1/32' ]]
  if compose_proxy_routes >"$FIX/empty-proxy-render" 2>"$FIX/empty-proxy-error"; then
    echo 'FAIL: production Compose accepted missing proxy trust' >&2
    exit 1
  fi
  grep -q 'required variable TRUSTED_PROXY_CIDRS' "$FIX/empty-proxy-error"
  printf '%s\n' 'PASS: production Compose uses the control-plane stub only for config-only builds and fails when both proxy values are absent'
fi
printf 'TRUSTED_PROXY_CIDRS="172.16.0.0/12,10.0.0.0/8"\nSECRET=untouched\n' >"$FIX/.env"
chmod 600 "$FIX/.env"
original_owner="$(stat -c '%u:%g' "$FIX/.env" 2>/dev/null || stat -f '%u:%g' "$FIX/.env")"
DEPLOY_DIR="$FIX"
CONFIG_ROOT="$FIX"
COMPOSE_CMD='docker compose'
if [[ -f "$ROOT/infrastructure/deploy/selfhost-proxy.sh" ]]; then
  source "$ROOT/infrastructure/deploy/selfhost-proxy.sh"
else
  selfhost_prepare_trusted_proxy_provenance() { return 1; }
fi

calls="$FIX/calls"
: >"$calls"
docker() {
  printf '%s\n' "$*" >>"$calls"
  case "$*" in
    'compose --env-file '* )
      printf 'CIDRS=%s PROXIES=%s\n' "${TRUSTED_PROXY_CIDRS-<unset>}" "${TRUSTED_PROXIES-<unset>}" >"$FIX/storage-compose-env"
      case "$*" in
        *' config --format json') printf '{"name":"fixture"}\n' ;;
        *' up') : ;;
        * ) return 97 ;;
      esac ;;
    'compose version --short') printf '%s\n' "${FIX_COMPOSE_VERSION:-2.33.1}" ;;
    'network ls '* )
      if [[ "${FIX_NETWORK_MISSING:-0}" == 1 && ! -f "$FIX/seeded" ]]; then :
      else printf '%s\n' "${FIX_NETWORK_IDS:-network-1}"; fi ;;
    'ps -a '* ) [[ "${FIX_PROJECT_CONTAINER:-0}" == 1 ]] && printf '%s\n' container-1 || : ;;
    'network inspect network-1 '* )
      local containers='{"aaaaaaaaaaaa":{}}'
      [[ -z "${FIX_NETWORK_CONTAINERS+x}" ]] || containers="$FIX_NETWORK_CONTAINERS"
      printf '{"Name":"fixture_concordvoice-services","Labels":{"com.docker.compose.project":"fixture","com.docker.compose.network":"concordvoice-services"},"Driver":"bridge","IPAM":{"Config":[{"Subnet":"172.28.0.0/16","Gateway":"%s"}]},"Containers":%s}\n' "${FIX_GATEWAY:-172.28.0.1}" "$containers" ;;
    'inspect '* )
      case "$*" in
        *'{{json .Config.Labels}}'*)
          case "$*" in
            *'aaaaaaaaaaaa '* ) printf '{"com.docker.compose.project":"%s","com.docker.compose.service":"nats"}\n' "${FIX_MEMBER_PROJECT:-fixture}" ;;
            *'bbbbbbbbbbbb '* ) printf '{"com.docker.compose.project":"fixture","com.docker.compose.service":"control-plane"}\n' ;;
            *'cccccccccccc '* ) printf '{"com.docker.compose.project":"fixture","com.docker.compose.service":"media-plane"}\n' ;;
            *'media-1 '* ) printf '{"com.docker.compose.project":"fixture","com.docker.compose.service":"media-plane"}\n' ;;
            * ) printf '{"com.docker.compose.project":"fixture","com.docker.compose.service":"control-plane"}\n' ;;
          esac ;;
        *'{{json .NetworkSettings.Networks}}'*) printf '{"fixture_concordvoice-services":{"GwPriority":1,"Gateway":"%s"},"fixture_concordvoice-data":{"GwPriority":%s,"Gateway":"172.29.0.1"}}\n' "${FIX_ENDPOINT_GATEWAY:-172.28.0.1}" "${FIX_COMPETITOR_PRIORITY:-0}" ;;
        *'{{json .Config.Env}}'*)
          case "$*" in
            *'media-1 '* ) printf '["TRUSTED_PROXIES=%s"]\n' "${FIX_CONTAINER_ROUTE:-172.28.0.1/32}" ;;
            * ) printf '["TRUSTED_PROXY_CIDRS=%s"]\n' "${FIX_CONTAINER_ROUTE:-172.28.0.1/32}" ;;
          esac ;;
      esac ;;
    * ) return 97 ;;
  esac
}
selfhost_proxy_compose() {
  printf 'compose:%s\n' "$*" >>"$calls"
  case "$*" in
    'config --format json')
      printf '{"name":"fixture","networks":{"concordvoice-services":{"name":"fixture_concordvoice-services"}},"services":{"control-plane":{"networks":{"concordvoice-services":{"gw_priority":1},"concordvoice-data":{}},"environment":{"TRUSTED_PROXY_CIDRS":"%s"}},"media-plane":{"networks":{"concordvoice-services":{"gw_priority":1},"concordvoice-data":{}},"environment":{"TRUSTED_PROXIES":"%s"}}}}\n' "${TRUSTED_PROXY_CIDRS:-127.0.0.1/32}" "${TRUSTED_PROXIES:-127.0.0.1/32}" ;;
    'up --no-start --no-deps --pull missing nats') touch "$FIX/seeded" ;;
    'up -d --no-deps --no-build --pull missing --wait --wait-timeout 180 nats')
      FIX_NETWORK_CONTAINERS='{"aaaaaaaaaaaa":{}}' ;;
    'ps -q -- control-plane') printf '%s\n' control-1 ;;
    'ps -q -- media-plane') printf '%s\n' media-1 ;;
    * ) return 97 ;;
  esac
}

( source "$ROOT/infrastructure/deploy/selfhost-storage.sh"
  # shellcheck disable=SC2034 # consumed by sourced selfhost-storage helper
  SELFHOST_STORAGE_PROJECT_ROOT="$FIX"
  # shellcheck disable=SC2034 # consumed by sourced selfhost-storage helper
  SELFHOST_STORAGE_PROJECT_NAME=fixture
  # shellcheck disable=SC2034 # consumed by sourced selfhost-storage helper
  SELFHOST_STORAGE_COMPOSE_ARGS=(--env-file "$FIX/.env" --project-directory "$FIX" --project-name fixture --profile services -f "$FIX/mini.yml")
  selfhost_storage_compose config --format json >/dev/null
  grep -qFx 'CIDRS=127.0.0.1/32 PROXIES=127.0.0.1/32' "$FIX/storage-compose-env"
  selfhost_storage_compose up >/dev/null
  grep -qFx 'CIDRS=<unset> PROXIES=<unset>' "$FIX/storage-compose-env"
)
printf '%s\n' 'PASS: legacy config render uses only transient exact placeholders'

selfhost_prepare_trusted_proxy_provenance || { echo 'FAIL: gateway preparation unavailable' >&2; exit 1; }
grep -qFx 'TRUSTED_PROXY_CIDRS="172.28.0.1/32"' "$FIX/.env"
grep -qFx 'TRUSTED_PROXIES="172.28.0.1/32"' "$FIX/.env"
grep -qFx 'SECRET=untouched' "$FIX/.env"
printf 'TRUSTED_PROXY_CIDRS="172.28.0.1/32"\nSECRET=untouched\nTRUSTED_PROXIES="172.28.0.1/32"\n' >"$FIX/expected-env"
cmp -s "$FIX/expected-env" "$FIX/.env"
[[ "$(stat -c '%u:%g' "$FIX/.env" 2>/dev/null || stat -f '%u:%g' "$FIX/.env")" == "$original_owner" ]]
[[ $(stat -c '%a' "$FIX/.env" 2>/dev/null || stat -f '%Lp' "$FIX/.env") == 600 ]]
! compgen -G "$FIX/.env.proxy.*" >/dev/null
! grep -q 'up --no-start' "$calls"
printf '%s\n' 'PASS: measured exact gateway persisted without starting services'
selfhost_proxy_live_check
printf '%s\n' 'PASS: live application endpoints match exact gateway'

# Upgrade discovery sees running app network members but performs no app
# mutation before the guarded rebuild owns the handoff.
printf 'TRUSTED_PROXY_CIDRS="172.16.0.0/12,10.0.0.0/8"\nSECRET=untouched\n' >"$FIX/.env"
FIX_NETWORK_CONTAINERS='{"aaaaaaaaaaaa":{},"bbbbbbbbbbbb":{},"cccccccccccc":{}}'
: >"$calls"
selfhost_prepare_trusted_proxy_provenance
! grep -Eq 'compose:(up|build|create|stop|start)|^stop |^start ' "$calls"
printf '%s\n' 'PASS: running application containers remain untouched before handoff'
unset FIX_NETWORK_CONTAINERS

FIX_COMPOSE_VERSION=v2.33.1-desktop.1
selfhost_prepare_trusted_proxy_provenance
printf '%s\n' 'PASS: stable Docker Desktop Compose suffix admitted'

FIX_COMPOSE_VERSION=2.33.0
if selfhost_prepare_trusted_proxy_provenance; then
  echo 'FAIL: old Compose accepted' >&2; exit 1
fi
printf '%s\n' 'PASS: old Compose refused before network mutation'
FIX_COMPOSE_VERSION=2.33.1

# A hand-edited broad policy is not one of the known installer defaults.
printf 'TRUSTED_PROXY_CIDRS="10.0.0.0/8"\nSECRET=untouched\n' >"$FIX/.env"
before="$(cat "$FIX/.env")"
: >"$calls"
if selfhost_prepare_trusted_proxy_provenance; then
  echo 'FAIL: custom broad trust migrated' >&2; exit 1
fi
[[ "$(cat "$FIX/.env")" == "$before" ]]
! grep -q 'compose:up ' "$calls"
printf '%s\n' 'PASS: custom broad trust refused without env rewrite'

printf 'TRUSTED_PROXY_CIDRS="127.0.0.1/32"\nSECRET=untouched\n' >"$FIX/original-env"
ln -s "$FIX/original-env" "$FIX/symlink-env"
if selfhost_proxy_write_env "$FIX/symlink-env" 172.28.0.1/32; then
  echo 'FAIL: symlink environment accepted' >&2; exit 1
fi
grep -qFx 'TRUSTED_PROXY_CIDRS="127.0.0.1/32"' "$FIX/original-env"
printf '%s\n' 'PASS: symlink environment refused without replacing target'
! compgen -G "$FIX/.env.proxy.*" >/dev/null

writer_files="$(rg -l '^write_env_file\(\)|^selfhost_proxy_write_env\(\)' "$ROOT/infrastructure/deploy" --glob '*.sh' | sed "s|$ROOT/||" | sort)"
[[ "$writer_files" == $'infrastructure/deploy/install-selfhost.sh\ninfrastructure/deploy/selfhost-proxy.sh' ]]
printf '%s\n' 'PASS: root-env writer census remains installer plus locked two-key migrator'

# A fresh project may seed only a stopped pinned NATS container before trust
# publication. Existing project containers without the network are ambiguous.
printf 'TRUSTED_PROXY_CIDRS="127.0.0.1/32"\nSECRET=untouched\n' >"$FIX/.env"
FIX_NETWORK_MISSING=1 FIX_PROJECT_CONTAINER=1 FIX_NETWORK_CONTAINERS='{}'
: >"$calls"
if selfhost_prepare_trusted_proxy_provenance; then
  echo 'FAIL: orphan project containers admitted' >&2; exit 1
fi
! grep -q 'compose:up --no-start' "$calls"
FIX_PROJECT_CONTAINER=0
selfhost_prepare_trusted_proxy_provenance
grep -q 'compose:up --no-start --no-deps --pull missing nats' "$calls"
grep -q 'compose:up -d --no-deps --no-build --pull missing --wait --wait-timeout 180 nats' "$calls"
grep -qFx 'TRUSTED_PROXY_CIDRS="172.28.0.1/32"' "$FIX/.env"
printf '%s\n' 'PASS: fresh network materializes then starts only NATS after ambiguity check'

FIX_NETWORK_MISSING=0 FIX_NETWORK_CONTAINERS='{}'
: >"$calls"
selfhost_prepare_trusted_proxy_provenance
grep -q 'compose:up --no-start --no-deps --pull missing nats' "$calls"
grep -q 'compose:up -d --no-deps --no-build --pull missing --wait --wait-timeout 180 nats' "$calls"
printf '%s\n' 'PASS: empty existing bridge is held by running NATS before application rebuild'
FIX_NETWORK_CONTAINERS='{"aaaaaaaaaaaa":{}}'

FIX_MEMBER_PROJECT=foreign
if selfhost_prepare_trusted_proxy_provenance; then
  echo 'FAIL: foreign network member admitted' >&2; exit 1
fi
FIX_MEMBER_PROJECT=fixture FIX_NETWORK_IDS=$'network-1\nnetwork-2'
if selfhost_prepare_trusted_proxy_provenance; then
  echo 'FAIL: multiple owned networks admitted' >&2; exit 1
fi
unset FIX_NETWORK_IDS
printf '%s\n' 'PASS: foreign network member and multiple networks refused'

FIX_NETWORK_MISSING=0 FIX_COMPETITOR_PRIORITY=1
if selfhost_proxy_live_check; then
  echo 'FAIL: equal-priority competing endpoint admitted' >&2; exit 1
fi
FIX_COMPETITOR_PRIORITY=0 FIX_ENDPOINT_GATEWAY=172.29.0.1
if selfhost_proxy_live_check; then
  echo 'FAIL: wrong live endpoint gateway admitted' >&2; exit 1
fi
printf '%s\n' 'PASS: competing or mismatched live gateway refused'
FIX_ENDPOINT_GATEWAY=172.28.0.1 FIX_GATEWAY=172.28.0.2
if selfhost_proxy_live_check; then
  echo 'FAIL: changed bridge IPAM gateway admitted' >&2; exit 1
fi
printf '%s\n' 'PASS: changed bridge gateway refused after rebuild'
FIX_GATEWAY=172.28.0.1 FIX_NETWORK_IDS=network-2
if selfhost_proxy_live_check; then
  echo 'FAIL: recreated bridge admitted' >&2; exit 1
fi
unset FIX_NETWORK_IDS
printf '%s\n' 'PASS: network recreation refused even with the same gateway'

# Multiple IPAM gateways leave the published-port source family ambiguous.
dual_stack='{"Name":"fixture_concordvoice-services","Labels":{"com.docker.compose.project":"fixture","com.docker.compose.network":"concordvoice-services"},"Driver":"bridge","IPAM":{"Config":[{"Subnet":"fd00:28::/64","Gateway":"fd00:28::1"},{"Subnet":"172.28.0.0/16","Gateway":"172.28.0.1"}]},"Containers":{"aaaaaaaaaaaa":{}}}'
if selfhost_proxy_network fixture fixture_concordvoice-services <<<"$dual_stack"; then
  echo 'FAIL: multiple bridge gateways admitted' >&2; exit 1
fi
v6_only='{"Name":"fixture_concordvoice-services","Labels":{"com.docker.compose.project":"fixture","com.docker.compose.network":"concordvoice-services"},"Driver":"bridge","IPAM":{"Config":[{"Subnet":"fd00:28::/64","Gateway":"fd00:28::1"}]},"Containers":{"aaaaaaaaaaaa":{}}}'
[[ "$(selfhost_proxy_network fixture fixture_concordvoice-services <<<"$v6_only")" == $'fd00:28::1/128\tpinned\taaaaaaaaaaaa' ]]
printf '%s\n' 'PASS: one exact IPv6 gateway admitted; multiple gateways refused'

FIX_COMPETITOR_PRIORITY=0 FIX_ENDPOINT_GATEWAY=172.28.0.1 FIX_GATEWAY=172.28.0.1
selfhost_proxy_restart_preflight
printf '%s\n' 'PASS: exact persisted and live proxy state admits direct restart'
printf 'TRUSTED_PROXY_CIDRS="172.16.0.0/12"\nTRUSTED_PROXIES="172.28.0.1/32"\n' >"$FIX/.env"
if selfhost_proxy_restart_preflight; then
  echo 'FAIL: broad persisted trust admitted for restart' >&2; exit 1
fi
printf 'TRUSTED_PROXY_CIDRS="172.28.0.1/32"\nTRUSTED_PROXIES="172.28.0.1/32"\n' >"$FIX/.env"
FIX_CONTAINER_ROUTE=172.29.0.1/32
if selfhost_proxy_restart_preflight; then
  echo 'FAIL: stale container trust admitted for restart' >&2; exit 1
fi
printf '%s\n' 'PASS: restart refuses broad persisted or stale container trust'
