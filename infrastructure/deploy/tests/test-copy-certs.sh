#!/usr/bin/env bash
# Real-image regression test for the certbot coturn deploy hook.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SOURCE_HOOK="${SCRIPT_DIR}/../copy-certs.sh"
COMPOSE="${SCRIPT_DIR}/../../../docker-compose.yml"
SOURCE_CONFIG="${SCRIPT_DIR}/../../docker/coturn/turnserver.conf"
TLS_OBSERVER="${SCRIPT_DIR}/../selfhost-tls.py"
TOOL_HELPER="${SCRIPT_DIR}/selfhost-cert-test-tools.sh"
IMAGE="$(sed -nE 's/^    image: (coturn\/coturn:[^[:space:]]+).*$/\1/p' "$COMPOSE")"
FIXTURE_IMAGE="${SELFHOST_CERT_TEST_IMAGE:-}"
COTURN_TEST_IMAGE="${SELFHOST_CERT_TEST_COTURN_IMAGE:-$IMAGE}"

fail() { echo "FAIL: $*" >&2; exit 1; }
if [[ "$IMAGE" != "coturn/coturn:4.18.0-alpine@sha256:c29135c08565810d37053f384584392b7c24b69570fc81a721b878d6fe63e04e" ]]; then
  fail "the Compose coturn image must remain pinned to the reviewed 4.18.0 artifact (got ${IMAGE:-none})"
fi
[[ -r "$TOOL_HELPER" ]] || fail "shared public tooling image helper unavailable"
# shellcheck source=infrastructure/deploy/tests/selfhost-cert-test-tools.sh
source "$TOOL_HELPER"
FIXTURE_IMAGE="$(selfhost_cert_prepare_tool_image)" || fail "required tooling image setup failed"
command -v python3 >/dev/null 2>&1 || fail "required Python 3 TLS observer runtime unavailable"
command -v openssl >/dev/null 2>&1 || fail "required OpenSSL runtime unavailable for TURN REST fixture credentials"
command -v timeout >/dev/null 2>&1 || fail "required timeout utility unavailable for bounded TURN probes"
[[ -r "$TLS_OBSERVER" ]] || fail "bounded self-host TLS observer is unavailable"
if [[ -z "${SELFHOST_CERT_TEST_COTURN_IMAGE:-}" ]] && ! docker image inspect "$COTURN_TEST_IMAGE" >/dev/null 2>&1; then
  docker pull "$COTURN_TEST_IMAGE" >/dev/null 2>&1 || fail "required public coturn image could not be pulled: $COTURN_TEST_IMAGE"
fi
docker image inspect "$COTURN_TEST_IMAGE" >/dev/null 2>&1 || fail "required coturn test image unavailable: $COTURN_TEST_IMAGE"
COTURN_IMAGE_ID="$(docker image inspect --format '{{.Id}}' "$COTURN_TEST_IMAGE")" || fail "coturn test image identity unavailable"
SELFHOST_CERT_TEST_IMAGE="$FIXTURE_IMAGE" python3 "$SCRIPT_DIR/test-copy-certs-labels.py"

TEST_ROOT="$(mktemp -d)"
LINEAGE="$TEST_ROOT/lineage"
PUBLIC="$TEST_ROOT/public"
DOCKER_STATE="$TEST_ROOT/docker-state"
CERT_VOLUME="concord-copy-certs-${RANDOM}-$$"
STUB_ROOT="$TEST_ROOT/stubs"
OUTPUT="$TEST_ROOT/hook-output.log"
CONTAINER="concord-copy-certs-${RANDOM}-$$"
PROXY_PID=""
PUBLIC_NETWORK=""
PUBLIC_SERVER=""
MAPPED_PUBLIC_SERVER=""
PUBLIC_PEER=""
MANAGED_TURN_COMMAND=()
TEST_COMPOSE_PROJECT="copy-certs-checkout-2504"
TEST_CONSUMER_ID="2504c0ffee51"
INSPECT_TEMPLATE='{{printf "{\"Name\":%q,\"Image\":%q,\"Project\":%q,\"Service\":%q,\"Status\":%q,\"Running\":%t,\"Paused\":%t,\"Restarting\":%t}" .Name .Config.Image (index .Config.Labels "com.docker.compose.project") (index .Config.Labels "com.docker.compose.service") .State.Status .State.Running .State.Paused .State.Restarting}}'
SELECTED_PS_TEMPLATE='{"ID":{{json .ID}},"Names":{{json .Names}},"Labels":{"com.docker.compose.project":{{json (.Label "com.docker.compose.project")}},"com.docker.compose.service":{{json (.Label "com.docker.compose.service")}}}}'
cleanup() {
  if [[ -n $PROXY_PID ]]; then
    kill "$PROXY_PID" >/dev/null 2>&1 || true
    wait "$PROXY_PID" 2>/dev/null || true
  fi
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  if [[ -n $PUBLIC_PEER ]]; then docker rm -f "$PUBLIC_PEER" >/dev/null 2>&1 || true; fi
  if [[ -n $MAPPED_PUBLIC_SERVER ]]; then docker rm -f "$MAPPED_PUBLIC_SERVER" >/dev/null 2>&1 || true; fi
  if [[ -n $PUBLIC_SERVER ]]; then docker rm -f "$PUBLIC_SERVER" >/dev/null 2>&1 || true; fi
  if [[ -n $PUBLIC_NETWORK ]]; then docker network rm "$PUBLIC_NETWORK" >/dev/null 2>&1 || true; fi
  docker volume rm "$CERT_VOLUME" "${MISSING_VOLUME:-}" >/dev/null 2>&1 || true
  rm -rf "$TEST_ROOT"
}
trap cleanup EXIT INT TERM
mkdir -p "$LINEAGE" "$STUB_ROOT" "$PUBLIC" "$DOCKER_STATE"
install -m 0644 "$SOURCE_HOOK" "$PUBLIC/copy-certs.sh"
install -m 0644 "$SOURCE_CONFIG" "$PUBLIC/turnserver.conf"
HOOK="$PUBLIC/copy-certs.sh"
CONFIG="$PUBLIC/turnserver.conf"
docker volume create "$CERT_VOLUME" >/dev/null

cat >"$STUB_ROOT/docker" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$COPY_TEST_DOCKER_CALLS"
container_name=concordvoice-coturn
case "${1:-}" in
  ps)
    shift
    all=0; format=""; project_filter=""; service_filter=""; name_filter=""
    while (($#)); do
      case "$1" in
        --all) [[ "$all" == 0 ]] || exit 97; all=1; shift ;;
        --filter)
          [[ $# -ge 2 ]] || exit 97
          filter="$2"; shift 2
          case "$filter" in
            label=com.docker.compose.project=*)
              [[ -z "$project_filter" ]] || exit 97
              project_filter="${filter#label=com.docker.compose.project=}" ;;
            label=com.docker.compose.service=*)
              [[ -z "$service_filter" ]] || exit 97
              service_filter="${filter#label=com.docker.compose.service=}" ;;
            name=*) [[ -z "$name_filter" ]] || exit 97; name_filter="${filter#name=}" ;;
            *) exit 97 ;;
          esac ;;
        --format) [[ $# -ge 2 && -z "$format" ]] || exit 97; format="$2"; shift 2 ;;
        *) exit 97 ;;
      esac
    done
    [[ "$all" == 1 && "$format" == "$COPY_TEST_PS_TEMPLATE" && -n "$service_filter" ]] || exit 97
    # This is a running consumer in the test's actual custom project. Valid
    # filters that do not match its real labels/name return Docker's empty set.
    [[ "$service_filter" == coturn &&
       ( -z "$project_filter" || "$project_filter" == "$COPY_TEST_PROJECT" ) &&
       ( -z "$name_filter" || "$container_name" =~ $name_filter ) ]] || exit 0
    printf '{"ID":"%s","Names":"%s","Labels":{"com.docker.compose.project":"%s","com.docker.compose.service":"coturn"}}\n' \
      "$COPY_TEST_CONSUMER_ID" "$container_name" "$COPY_TEST_PROJECT"
    ;;
  inspect)
    [[ $# -eq 4 && "$2" == --format && "$3" == "$COPY_TEST_INSPECT_TEMPLATE" &&
      "$4" == "$COPY_TEST_CONSUMER_ID" ]] || exit 97
    printf '{"Name":"/%s","Image":"coturn/coturn:4.18.0-alpine@sha256:c29135c08565810d37053f384584392b7c24b69570fc81a721b878d6fe63e04e","Project":"%s","Service":"coturn","Status":"running","Running":true,"Paused":false,"Restarting":false}\n' \
      "$container_name" "$COPY_TEST_PROJECT"
    ;;
  restart)
    [[ $# -eq 2 && "$2" == "$COPY_TEST_CONSUMER_ID" ]] || exit 97
    exit 0
    ;;
  *) exit 97 ;;
esac
STUB
cat >"$STUB_ROOT/systemctl" <<'STUB'
#!/usr/bin/env bash
[[ $# -eq 3 && "$1" == is-active && "$2" == --quiet && "$3" == nginx ]] && exit 1
exit 97
STUB
chmod +x "$STUB_ROOT/docker" "$STUB_ROOT/systemctl"

run_hook() {
  : >"$DOCKER_STATE/docker.calls"
  docker run --rm --network none --security-opt no-new-privileges --pids-limit 64 --user 0:0 \
    --volume "$LINEAGE:/lineage:ro" \
    --volume "$CERT_VOLUME:/opt/concord/certs" \
    --volume "$STUB_ROOT:/stubs:ro" \
    --volume "$DOCKER_STATE:/state:rw" \
    --volume "$HOOK:/hook:ro" \
    "$FIXTURE_IMAGE" bash -c 'PATH=/stubs:/usr/bin:/bin RENEWED_LINEAGE=/lineage COTURN_CERT_ROOT=/opt/concord/certs COPY_TEST_PROJECT="$1" COPY_TEST_CONSUMER_ID="$2" COPY_TEST_INSPECT_TEMPLATE="$3" COPY_TEST_PS_TEMPLATE="$4" COPY_TEST_DOCKER_CALLS=/state/docker.calls bash /hook' \
    hook "$TEST_COMPOSE_PROJECT" "$TEST_CONSUMER_ID" "$INSPECT_TEMPLATE" "$SELECTED_PS_TEMPLATE" \
    >>"$OUTPUT" 2>&1
}

assert_custom_project_restart() {
  [[ "$(grep -c "^restart $TEST_CONSUMER_ID$" "$DOCKER_STATE/docker.calls" || true)" == 1 &&
     "$(grep -c "^inspect --format .* $TEST_CONSUMER_ID$" "$DOCKER_STATE/docker.calls" || true)" == 1 ]] || \
    fail "running coturn in the custom checkout project must restart its exact inspected ID"
}

make_pair() {
  local common_name="$1"
  docker run --rm --network none --security-opt no-new-privileges --pids-limit 64 \
    --user 0:0 --volume "$LINEAGE:/lineage" "$FIXTURE_IMAGE" \
    sh -eu -c 'openssl req -x509 -newkey rsa:2048 -nodes -days 1 -sha256 \
      -subj "/CN=$1" -addext "subjectAltName=DNS:$1" \
      -keyout /lineage/privkey.pem -out /lineage/fullchain.pem >/dev/null 2>&1 \
      && chown 0:0 /lineage/privkey.pem /lineage/fullchain.pem \
      && chmod 600 /lineage/privkey.pem && chmod 644 /lineage/fullchain.pem' \
    sh "$common_name" || fail "root namespace certificate fixture generation failed"
}

make_pair example.com
run_hook || fail "the real hook must process a complete renewal fixture"
assert_custom_project_restart

image_uid="$(docker run --rm --network none --security-opt no-new-privileges --pids-limit 64 --entrypoint id "$COTURN_TEST_IMAGE" -u)"
image_gid="$(docker run --rm --network none --security-opt no-new-privileges --pids-limit 64 --entrypoint id "$COTURN_TEST_IMAGE" -g)"
[[ "$image_uid:$image_gid" == 65534:65533 ]] || \
  fail "the pinned coturn image default identity must be 65534:65533 (got ${image_uid}:${image_gid})"

read_file_as() {
  local identity="$1" path="$2"
  docker run --rm --network none --security-opt no-new-privileges --pids-limit 64 --user "$identity" --entrypoint sh --volume "$CERT_VOLUME:/certs:ro" \
    "$COTURN_TEST_IMAGE" -c "cat /certs/coturn/${path}" >/dev/null
}

# This is deliberately before the desired metadata assertions: the old hook
# must fail at the consumer boundary, proving the regression is non-vacuous.
read_file_as 0:0 key.pem || fail "the root positive control must read the renewed private key"
read_file_as 65534:65533 key.pem || fail "the default coturn identity must read the renewed private key"
read_file_as 65534:65533 cert.pem || fail "the default coturn identity must read the renewed certificate"

stat_triplet() {
  docker run --rm --network none --security-opt no-new-privileges --pids-limit 64 --entrypoint stat --volume "$CERT_VOLUME:/certs:ro" "$FIXTURE_IMAGE" \
    -c '%a %u %g' "$1"
}
[[ "$(stat_triplet /certs)" == "711 0 0" ]] || fail "the certificate root must be mode 0711 and owned by root:root"
[[ "$(stat_triplet /certs/coturn)" == "711 0 0" ]] || fail "the coturn directory must be mode 0711 and owned by root:root"
[[ "$(stat_triplet /certs/coturn/cert.pem)" == "444 0 0" ]] || fail "cert.pem must be mode 0444 and owned by root:root"
[[ "$(stat_triplet /certs/coturn/key.pem)" == "400 65534 65533" ]] || fail "key.pem must be mode 0400 and owned by 65534:65533"

if read_file_as 12345:12345 key.pem 2>/dev/null; then
  fail "an unrelated identity must not read the coturn private key"
fi

# A second renewal must repair deliberately damaged metadata.
  docker run --rm --network none --security-opt no-new-privileges --pids-limit 64 --user 0:0 --volume "$CERT_VOLUME:/certs" "$FIXTURE_IMAGE" \
  chmod 777 /certs /certs/coturn /certs/coturn/cert.pem /certs/coturn/key.pem || \
  fail "the fixture must be able to damage renewal metadata before renewal B"
make_pair example.com
run_hook || fail "the real hook must process the second renewal fixture"
assert_custom_project_restart
[[ "$(stat_triplet /certs)" == "711 0 0" ]] || fail "renewal B must restore certificate-root mode 0711 root:root"
[[ "$(stat_triplet /certs/coturn)" == "711 0 0" ]] || fail "renewal B must restore coturn-directory mode 0711 root:root"
[[ "$(stat_triplet /certs/coturn/cert.pem)" == "444 0 0" ]] || fail "renewal B must restore cert.pem mode 0444 root:root"
[[ "$(stat_triplet /certs/coturn/key.pem)" == "400 65534 65533" ]] || fail "renewal B must restore key.pem mode 0400 image identity"

MISSING_ROOT="$TEST_ROOT/missing"
mkdir -p "$MISSING_ROOT"
MISSING_VOLUME="concord-copy-certs-missing-${RANDOM}-$$"
docker volume create "$MISSING_VOLUME" >/dev/null
docker run --rm --network none --security-opt no-new-privileges --pids-limit 64 --user 0:0 --volume "$MISSING_ROOT:/lineage:ro" --volume "$STUB_ROOT:/stubs:ro" \
  --volume "$HOOK:/hook:ro" --volume "$MISSING_VOLUME:/opt/concord/certs" \
  "$FIXTURE_IMAGE" bash -c 'PATH=/stubs:/usr/bin:/bin RENEWED_LINEAGE=/lineage COTURN_CERT_ROOT=/opt/concord/certs bash /hook' \
  >>"$OUTPUT" 2>&1 || fail "missing source certificates must preserve exit status 0"
if docker run --rm --network none --security-opt no-new-privileges --pids-limit 64 --entrypoint sh --volume "$MISSING_VOLUME:/certs:ro" "$FIXTURE_IMAGE" -c 'test -e /certs/coturn'; then
  fail "missing source certificates must not create the coturn target"
fi

if grep -Eiq 'BEGIN (CERTIFICATE|.*PRIVATE KEY)' "$OUTPUT"; then
  fail "hook output must not contain PEM or private-key markers"
fi

docker run --detach --rm --security-opt no-new-privileges --pids-limit 64 --name "$CONTAINER" \
  --publish 127.0.0.1::5349 \
  --mount "type=volume,src=${CERT_VOLUME},dst=/etc/coturn/certs,volume-subpath=coturn,readonly" \
  --volume "$CONFIG:/etc/coturn/turnserver.conf:ro" \
  --entrypoint turnserver "$COTURN_TEST_IMAGE" -c /etc/coturn/turnserver.conf \
  --static-auth-secret=test-secret --realm=concordvoice.chat \
  --listening-ip=0.0.0.0 --external-ip=127.0.0.1 --relay-ip=127.0.0.1 \
  --allow-loopback-peers --allowed-peer-ip=127.0.0.1 \
  --cert=/etc/coturn/certs/cert.pem --pkey=/etc/coturn/certs/key.pem >/dev/null || \
  fail "the pinned coturn image must start its TLS listener with the renewed pair"
PORT="$(docker port "$CONTAINER" 5349/tcp | sed -n '1s/.*://p')"
[[ "$PORT" =~ ^[0-9]+$ ]] || fail "coturn TLS listener must publish a dynamic host port"
if [[ ${SELFHOST_HOOK_MAC_BRIDGE:-} == 1 ]]; then
  command -v socat >/dev/null 2>&1 || fail "required Docker Desktop loopback forwarder unavailable"
  socat "TCP-LISTEN:${PORT},bind=127.0.0.1,reuseaddr,fork" "TCP:host.docker.internal:${PORT}" \
    >"$TEST_ROOT/loopback-proxy.log" 2>&1 &
  PROXY_PID=$!
fi
python3 - "$TLS_OBSERVER" "$LINEAGE/fullchain.pem" "$PORT" <<'PY' || fail "coturn TLS listener must complete bounded verified handshake and serve the exact selected leaf"
import importlib.util
import sys

path, cert_path, port = sys.argv[1:]
spec = importlib.util.spec_from_file_location("selfhost_tls_observer", path)
if spec is None or spec.loader is None:
    raise SystemExit("bounded TLS observer could not be loaded")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
try:
    module.observe_tls(cert_path, "example.com", int(port), 15)
except module.TLSRefusalError:
    print("selfhost-tls: TLSRefusalError", file=sys.stderr)
    raise SystemExit(1)
PY
docker run --rm --network "container:$CONTAINER" --entrypoint python3 "$FIXTURE_IMAGE" -c '
import errno, socket, sys
probe = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
probe.settimeout(2)
refused = probe.connect_ex(("127.0.0.1", 5766)) == errno.ECONNREFUSED
probe.close()
sys.exit(0 if refused else 1)
' || \
  fail "coturn CLI must remain disabled by default"
docker logs "$CONTAINER" >"$TEST_ROOT/coturn.log" 2>&1 || fail "coturn startup logs must be readable"
CONFIG_ERROR_PATTERN='unknown (option|directive|configuration)|unknown keyword|bad (configuration )?format|wrong format|failed to parse|error parsing'
grep -Eiq "$CONFIG_ERROR_PATTERN" <<< 'Bad configuration format: stale-directive' || \
  fail "configuration error matcher must catch coturn's bad-format startup diagnostic"
if grep -Eiq "$CONFIG_ERROR_PATTERN" "$TEST_ROOT/coturn.log"; then
  fail "coturn startup must not report unknown or malformed configuration directives"
fi

TURN_TEST_SECRET=test-secret
TURN_TEST_USERNAME="$(( $(date +%s) + 300 )):fixture"
TURN_TEST_CREDENTIAL="$(printf '%s' "$TURN_TEST_USERNAME" | openssl dgst -binary -sha1 -hmac "$TURN_TEST_SECRET" | base64 | tr -d '\n')" || \
  fail "TURN REST fixture credential generation failed"
run_turn_probe() {
  local label="$1" port="$2" log="$TEST_ROOT/$1-turn.log"
  shift 2
  timeout 25s docker run --rm --network "container:$CONTAINER" --entrypoint turnutils_uclient \
    "$COTURN_TEST_IMAGE" -u "$TURN_TEST_USERNAME" -w "$TURN_TEST_CREDENTIAL" \
    -p "$port" -n 5 -y "$@" 127.0.0.1 >"$log" 2>&1 || \
    fail "bounded authenticated $label TURN allocation and relay probe failed"
  grep -Eq 'tot_send_msgs=[1-9][0-9]*, tot_recv_msgs=[1-9][0-9]*' "$log" && \
    grep -Eq 'Total lost packets 0 \([^)]*%\)' "$log" || \
    { tail -n 40 "$log" >&2; fail "authenticated $label TURN probe did not report successful relay round trips"; }
}
run_turn_probe UDP 3478
# shortcut: uclient counts coalesced TLS frames as one; pace until its stream parser is fixed.
run_turn_probe TURNS 5349 -S -t -z 100

# The loopback whitelist above bypasses deny ranges; exercise the shipped ACL without it.
# RFC 5737 addresses stay inside an unpublished, internal bridge. Try disjoint
# /29s so concurrent fixtures can each own their network.
for offset in {0..31}; do
  base=$((offset * 8))
  if PUBLIC_NETWORK="$(docker network create --internal --subnet "192.0.2.$base/29" \
      "${CONTAINER}-public" 2>"$TEST_ROOT/network.log")"; then
    break
  fi
done
[[ -n $PUBLIC_NETWORK ]] || { cat "$TEST_ROOT/network.log" >&2; fail "isolated public-peer network unavailable"; }
server_ip="192.0.2.$((base + 2))"
peer_ip="192.0.2.$((base + 3))"
MANAGED_COMPOSE="$TEST_ROOT/managed-compose.json"
env PUBLIC_IP="$peer_ip" TURN_SECRET=test-secret TURN_REALM=concordvoice.chat \
  POSTGRES_PASSWORD=test REDIS_PASSWORD=test JWT_SECRET=test MINIO_ROOT_USER=test \
  MINIO_ROOT_PASSWORD=test DOMAIN=example.com FEEDBACK_PAT=test FEEDBACK_REPO=fixture/example \
  TRUSTED_PROXY_CIDRS=127.0.0.1/32 MFA_ENCRYPTION_KEY=test \
  SMTP_HOST=smtp.example.invalid SMTP_FROM=noreply@example.invalid \
  docker compose --env-file /dev/null --project-directory "$SCRIPT_DIR/../../.." \
  -f "$SCRIPT_DIR/../../../docker-compose.yml" -f "$SCRIPT_DIR/../../../docker-compose.production.yml" \
  config --format json >"$MANAGED_COMPOSE" || fail "managed Coturn Compose command must render"
while IFS= read -r argument; do MANAGED_TURN_COMMAND+=("$argument"); done \
  < <(jq -er '.services.coturn.command[]' "$MANAGED_COMPOSE")
[[ ${MANAGED_TURN_COMMAND[0]:-} == turnserver ]] || fail "managed Coturn command must start with turnserver"
PUBLIC_SERVER="$(docker run --detach --network "$PUBLIC_NETWORK" --ip "$server_ip" \
  --security-opt no-new-privileges --pids-limit 64 \
  --volume "$CONFIG:/etc/coturn/turnserver.conf:ro" --entrypoint turnserver \
  "$COTURN_TEST_IMAGE" -c /etc/coturn/turnserver.conf --static-auth-secret=test-secret \
  --listening-ip="$server_ip" --relay-ip="$server_ip" --relay-ip=::1 --no-tls)" || \
  fail "unwhitelisted public-peer TURN server must start"
PUBLIC_PEER="$(docker run --detach --network "$PUBLIC_NETWORK" --ip "$peer_ip" \
  --security-opt no-new-privileges --pids-limit 64 --entrypoint turnutils_peer \
  "$COTURN_TEST_IMAGE" -L "$peer_ip" -p 3480)" || fail "separate UDP echo peer must start"
docker run --rm --network "container:$PUBLIC_SERVER" --entrypoint python3 "$FIXTURE_IMAGE" -c '
import socket, sys, time
deadline = time.monotonic() + 10
while time.monotonic() < deadline:
    try:
        with socket.create_connection((sys.argv[1], 3478), timeout=1):
            break
    except OSError:
        time.sleep(0.1)
else:
    sys.exit(1)
' "$server_ip" || { docker logs "$PUBLIC_SERVER" >&2; fail "unwhitelisted TURN TCP listener must become ready"; }
public_log="$TEST_ROOT/public-peer-turn.log"
timeout 25s docker exec "$PUBLIC_PEER" turnutils_uclient \
  -u "$TURN_TEST_USERNAME" -w "$TURN_TEST_CREDENTIAL" -p 3478 -n 3 -m 1 \
  -t -c --no-even-port -e "$peer_ip" -r 3480 "$server_ip" >"$public_log" 2>&1 || \
  { tail -n 40 "$public_log" >&2; fail "TCP TURN must relay to an unwhitelisted public peer"; }
if ! grep -Eq 'tot_send_msgs=3, tot_recv_msgs=3' "$public_log" || \
    ! grep -Eq 'Total lost packets 0 \([^)]*%\)' "$public_log"; then
  tail -n 40 "$public_log" >&2
  fail "unwhitelisted TCP relay must deliver all three test messages"
fi
mapped_server_ip="192.0.2.$((base + 4))"
MAPPED_PUBLIC_SERVER="$(docker run --detach --network "$PUBLIC_NETWORK" --ip "$mapped_server_ip" \
  --security-opt no-new-privileges --pids-limit 64 \
  --mount "type=volume,src=${CERT_VOLUME},dst=/etc/coturn/certs,volume-subpath=coturn,readonly" \
  --volume "$CONFIG:/etc/coturn/turnserver.conf:ro" \
  --entrypoint "${MANAGED_TURN_COMMAND[0]}" "$COTURN_TEST_IMAGE" "${MANAGED_TURN_COMMAND[@]:1}")" || \
  fail "managed Coturn command with the shared public test address must start"
docker run --rm --network "container:$MAPPED_PUBLIC_SERVER" --entrypoint python3 "$FIXTURE_IMAGE" -c '
import socket, sys, time
deadline = time.monotonic() + 10
while time.monotonic() < deadline:
    try:
        with socket.create_connection((sys.argv[1], 3478), timeout=1):
            break
    except OSError:
        time.sleep(0.1)
else:
    sys.exit(1)
' "$mapped_server_ip" || { docker logs "$MAPPED_PUBLIC_SERVER" >&2; fail "managed Coturn TCP listener must become ready"; }
mapped_log="$TEST_ROOT/managed-cohost-peer-turn.log"
timeout 25s docker exec "$PUBLIC_PEER" turnutils_uclient \
  -u "$TURN_TEST_USERNAME" -w "$TURN_TEST_CREDENTIAL" -p 3478 -n 3 -m 1 \
  -t -c --no-even-port -e "$peer_ip" -r 3480 "$mapped_server_ip" >"$mapped_log" 2>&1 || \
  { tail -n 40 "$mapped_log" >&2; fail "managed same-public-address TCP TURN must relay to the direct echo peer"; }
if ! grep -Eq 'tot_send_msgs=3, tot_recv_msgs=3' "$mapped_log" || \
    ! grep -Eq 'Total lost packets 0 \([^)]*%\)' "$mapped_log"; then
  tail -n 40 "$mapped_log" >&2
  fail "managed same-public-address TCP relay must deliver all three test messages"
fi
for denied_peer in 10.1.2.3 127.0.0.1 0.0.0.0; do
  denied_log="$TEST_ROOT/denied-$denied_peer.log"
  if timeout 25s docker exec "$PUBLIC_PEER" turnutils_uclient \
      -u "$TURN_TEST_USERNAME" -w "$TURN_TEST_CREDENTIAL" -p 3478 -n 3 -m 1 \
      -t -c --no-even-port -e "$denied_peer" -r 3480 "$mapped_server_ip" >"$denied_log" 2>&1; then
    fail "private, loopback and unspecified TURN peers must remain denied"
  fi
  grep -Eq 'error 403' "$denied_log" || \
    { tail -n 40 "$denied_log" >&2; fail "local/unspecified peer refusal must be an explicit TURN 403"; }
done
ipv6_denied_log="$TEST_ROOT/denied-ipv6-unspecified.log"
if timeout 25s docker exec "$PUBLIC_PEER" turnutils_uclient \
    -u "$TURN_TEST_USERNAME" -w "$TURN_TEST_CREDENTIAL" -p 3478 -n 3 -m 1 \
    -t -c --no-even-port -e :: -r 3480 "$server_ip" >"$ipv6_denied_log" 2>&1; then
  fail "unspecified IPv6 TURN peer must remain denied"
fi
grep -Eq 'error 403' "$ipv6_denied_log" || \
  { tail -n 40 "$ipv6_denied_log" >&2; fail "unspecified IPv6 peer refusal must be an explicit TURN 403"; }
echo "Separate-public and managed same-public TCP relays: 3/3 messages; IPv4 private/loopback/unspecified and IPv6 unspecified peers: TURN 403."
echo "Copy-certs consumer image: $COTURN_TEST_IMAGE ($COTURN_IMAGE_ID)"
echo "All copy-certs coturn permission, TLS, CLI-disabled, authenticated allocation and relay tests passed."
