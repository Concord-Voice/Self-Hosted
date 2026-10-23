#!/usr/bin/env bash
# Construction/source-safety contracts for #2504 Task 3.
# Root executes this later in the public test fixture; this author did not run it.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
STORAGE="$ROOT/infrastructure/deploy/selfhost-storage.sh"
TLS_SHELL="$ROOT/infrastructure/deploy/selfhost-tls.sh"
TLS_PY="$ROOT/infrastructure/deploy/selfhost-tls.py"
MANIFEST="$ROOT/infrastructure/deploy/deploy-manifest.txt"

# CI invokes test-*.sh directly as a nonroot host user. Bootstrap tooling over
# the public network once, stage only this test's allowlisted source inputs, then
# run both inner suites once in a network-none root container (no checkout,
# socket, credentials, or host production mounts).
if [[ ${SELFHOST_TLS_TEST_INNER:-} != 1 ]]; then
  command -v docker >/dev/null 2>&1 || { printf 'FAIL: required Docker runtime unavailable\n' >&2; exit 1; }
  docker info >/dev/null 2>&1 || { printf 'FAIL: required Docker daemon unavailable\n' >&2; exit 1; }
  outer_stage="$(mktemp -d)"
  tooling_build="$(mktemp -d)"
  if [[ -n ${SELFHOST_TLS_TEST_IMAGE:-} ]]; then
    image=$SELFHOST_TLS_TEST_IMAGE
  else
    image=concord-2504-tls-test-tools:ubuntu24
    cat >"$tooling_build/Dockerfile" <<'DOCKERFILE'
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends bash coreutils grep sed openssl python3 \
    && rm -rf /var/lib/apt/lists/*
DOCKERFILE
    docker build --network default -t "$image" "$tooling_build" >/dev/null || {
      printf 'FAIL: public tooling image bootstrap failed\n' >&2
      rm -rf "$outer_stage" "$tooling_build"; exit 1;
    }
  fi
  docker image inspect "$image" >/dev/null 2>&1 || {
    printf 'FAIL: required TLS test image unavailable\n' >&2
    rm -rf "$outer_stage" "$tooling_build"; exit 1;
  }
  trap 'rm -rf "$outer_stage" "$tooling_build"' EXIT
  mkdir -p "$outer_stage/infrastructure/deploy/tests" \
    "$outer_stage/infrastructure/deploy"
  install -m 0644 "$BASH_SOURCE" "$outer_stage/infrastructure/deploy/tests/test-selfhost-tls-readiness.sh"
  install -m 0644 "$(dirname "$BASH_SOURCE")/test_selfhost_tls.py" \
    "$outer_stage/infrastructure/deploy/tests/test_selfhost_tls.py"
  install -m 0644 "$(dirname "$BASH_SOURCE")/test-selfhost-tls-selected-consumer.py" \
    "$outer_stage/infrastructure/deploy/tests/test-selfhost-tls-selected-consumer.py"
  for name in selfhost-storage.sh selfhost-tls.sh selfhost-tls.py deploy-manifest.txt \
    docker-compose.yml docker-compose.production.yml docker-compose.selfhost.yml docker-compose.byo-s3.yml; do
    case "$name" in
      docker-compose*) source_file="$ROOT/$name"; destination="$outer_stage/$name" ;;
      *) source_file="$ROOT/infrastructure/deploy/$name"; destination="$outer_stage/infrastructure/deploy/$name" ;;
    esac
    install -m 0644 "$source_file" "$destination"
  done
  chmod -R a+rX "$outer_stage"
  docker run --rm --network none --user 0:0 \
    -e SELFHOST_TLS_TEST_INNER=1 -e SELFHOST_TLS_OWN_ROOT_FIXTURE=1 \
    -v "$outer_stage:/fixture:ro" "$image" bash -c \
    'SELFHOST_TLS_TEST_INNER=1 SELFHOST_TLS_OWN_ROOT_FIXTURE=1 bash /fixture/infrastructure/deploy/tests/test-selfhost-tls-readiness.sh && python3 /fixture/infrastructure/deploy/tests/test_selfhost_tls.py'
  rc=$?
  rm -rf "$outer_stage" "$tooling_build"
  exit "$rc"
fi

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
[[ -f "$TLS_SHELL" && -f "$TLS_PY" ]] || fail 'SETUP: Task 3 helper modules are not present yet'
[[ -f "$STORAGE" ]] || fail 'shared storage authority is unavailable'

# Sourcing definitions must not execute a subprocess, access Docker or print.
stage="$(mktemp -d "${TMPDIR:-/tmp}/concord-tls-source.XXXXXX")"
trap 'rm -rf -- "$stage"' EXIT
mkdir -p "$stage/bin"
cat >"$stage/bin/python3" <<'SH'
#!/usr/bin/env bash
printf 'subprocess-called\n' >>"$TLS_SOURCE_CALLS"
exit 97
SH
cat >"$stage/bin/docker" <<'SH'
#!/usr/bin/env bash
printf 'docker-called\n' >>"$TLS_SOURCE_DOCKER_CALLS"
exit 97
SH
chmod 0755 "$stage/bin/python3"
chmod 0755 "$stage/bin/docker"
export TLS_SOURCE_CALLS="$stage/calls" TLS_SOURCE_DOCKER_CALLS="$stage/source-docker-calls"
: >"$TLS_SOURCE_CALLS"; : >"$TLS_SOURCE_DOCKER_CALLS"
source_output="$(PATH="$stage/bin:/usr/bin:/bin" bash -c 'source "$1"; source "$2"; declare -F selfhost_tls_admit selfhost_tls_ready >/dev/null' _ "$STORAGE" "$TLS_SHELL" 2>&1)" \
  || fail 'SETUP/contract: modules did not source cleanly or required functions are absent'
[[ -z "$source_output" && ! -s "$TLS_SOURCE_CALLS" && ! -s "$TLS_SOURCE_DOCKER_CALLS" ]] \
  || fail 'sourcing TLS/storage helpers performed observable work'

grep -Fqx 'infrastructure/deploy/selfhost-storage.sh' "$MANIFEST" \
  || fail 'shared storage authority is absent from deploy manifest'
grep -Fqx 'infrastructure/deploy/selfhost-tls.sh' "$MANIFEST" \
  || fail 'TLS shell helper is absent from deploy manifest'
grep -Fqx 'infrastructure/deploy/selfhost-tls.py' "$MANIFEST" \
  || fail 'TLS Python observer is absent from deploy manifest'

# Exercise the existing literal data boundary, never source/eval the .env file.
source "$STORAGE"
fixture="$stage/project"
mkdir -p "$fixture"
printf '%s\n' 'SELFHOST_STORAGE_MODE=bundled' 'DOMAIN=example.invalid' \
  'COTURN_CERTS_DIR=/opt/concord/certs/coturn' >"$fixture/.env"
tls_read() { _selfhost_storage_data read-tls "$fixture/.env" >"$stage/read-tls.out" 2>"$stage/read-tls.err"; }
assert_read_tls_data() {
  python3 - "$stage/read-tls.out" "$1" <<'PY'
import json, pathlib, sys
raw = pathlib.Path(sys.argv[1]).read_bytes()
want = {"example.invalid", "/opt/concord/certs/coturn"}
def leaves(value):
    if isinstance(value, dict):
        for item in value.values(): yield from leaves(item)
    elif isinstance(value, list):
        for item in value: yield from leaves(item)
    elif isinstance(value, str):
        yield value
    elif value is not None:
        yield "<non-string-data>"
try:
    parsed = json.loads(raw.decode("utf-8"))
except (UnicodeError, json.JSONDecodeError):
    text = raw.decode("utf-8", "strict")
    values = [part for part in text.replace("\x00", "\n").splitlines() if part]
    values = [item.split("=", 1)[1] if "=" in item else item for item in values]
else:
    values = list(leaves(parsed))
if len(values) != 2 or set(values) != want:
    sys.exit("FAIL: read-tls success data did not contain exactly the current domain and canonical selector")
PY
}
tls_read || fail 'SETUP/contract: unique literal TLS values were not admitted by read-tls'
assert_read_tls_data "$stage/read-tls.out" || fail 'read-tls successful data was not limited to admitted values'
printf '%s\n' 'SELFHOST_STORAGE_MODE=bundled' 'DOMAIN=other.example.invalid' \
  'COTURN_CERTS_DIR=/opt/concord/certs/coturn' >"$fixture/.env"
tls_read || fail 'read-tls refused a second otherwise-valid domain fixture'
python3 - "$stage/read-tls.out" <<'PY'
import json, pathlib, sys
raw=pathlib.Path(sys.argv[1]).read_bytes()
want={"other.example.invalid","/opt/concord/certs/coturn"}
def flatten(value):
    if isinstance(value,dict):
        for item in value.values(): yield from flatten(item)
    elif isinstance(value,list):
        for item in value: yield from flatten(item)
    elif isinstance(value,str): yield value
    elif value is not None: yield "<non-string-data>"
try: values=list(flatten(json.loads(raw.decode("utf-8"))))
except (UnicodeError,json.JSONDecodeError):
    values=[part for part in raw.decode("utf-8").replace("\x00","\n").splitlines() if part]
    values=[item.split("=",1)[1] if "=" in item else item for item in values]
if len(values)!=2 or set(values)!=want: sys.exit("FAIL: read-tls data was hardcoded or included extra fields")
PY
printf '%s\n' 'SELFHOST_STORAGE_MODE=bundled' 'DOMAIN=example.invalid' \
  'COTURN_CERTS_DIR=/opt/concord/certs/coturn' >"$fixture/.env"
for bad in missing-domain duplicate-domain missing-selector duplicate-selector noncanonical-selector; do
  printf '%s\n' 'SELFHOST_STORAGE_MODE=bundled' 'DOMAIN=example.invalid' \
    'COTURN_CERTS_DIR=/opt/concord/certs/coturn' >"$fixture/.env"
  case "$bad" in
    missing-domain) sed -i '/^DOMAIN=/d' "$fixture/.env" ;;
    duplicate-domain) printf '%s\n' 'DOMAIN=second.example.invalid' >>"$fixture/.env" ;;
    missing-selector) sed -i '/^COTURN_CERTS_DIR=/d' "$fixture/.env" ;;
    duplicate-selector) printf '%s\n' 'COTURN_CERTS_DIR=/opt/concord/certs/coturn' >>"$fixture/.env" ;;
    noncanonical-selector) sed -i 's#COTURN_CERTS_DIR=.*#COTURN_CERTS_DIR=/tmp/alternate#' "$fixture/.env" ;;
  esac
  if tls_read; then fail "read-tls accepted $bad"; fi
  [[ ! -s "$stage/read-tls.out" ]] || fail "read-tls leaked partial success data for $bad"
done

# Unresolved authority refuses before invoking Compose or Python.  This is a
# real negative control without inventing an internal CLI/output convention.
source "$TLS_SHELL"
unset SELFHOST_STORAGE_MODE_RESOLVED SELFHOST_STORAGE_PROJECT_ROOT \
  SELFHOST_STORAGE_PROJECT_NAME SELFHOST_STORAGE_COMPOSE_ARGS
export COTURN_CERTS_DIR=/tmp/caller-selected-certificate-directory
cat >"$stage/bin/python3" <<'SH'
#!/usr/bin/env bash
printf 'python-called\n' >>"$TLS_SOURCE_PYTHON_CALLS"
exit 97
SH
cat >"$stage/bin/docker" <<'SH'
#!/usr/bin/env bash
printf 'docker-called\n' >>"$TLS_SOURCE_DOCKER_CALLS"
exit 97
SH
chmod 0755 "$stage/bin/python3" "$stage/bin/docker"
export TLS_SOURCE_PYTHON_CALLS="$stage/python-calls" TLS_SOURCE_DOCKER_CALLS="$stage/docker-calls"
: >"$TLS_SOURCE_PYTHON_CALLS"; : >"$TLS_SOURCE_DOCKER_CALLS"
saved_path=$PATH
assert_shell_refusal() {
  local operation=$1 expected=$2 output rc
  if output="$(
    (
      set -euo pipefail
      source "$TLS_SHELL"
      export COTURN_CERTS_DIR=/tmp/caller-selected-certificate-directory
      PATH="$stage/bin:/usr/bin:/bin"; export PATH
      case "$operation" in
        admit|ready)
          unset SELFHOST_STORAGE_MODE_RESOLVED SELFHOST_STORAGE_PROJECT_ROOT \
            SELFHOST_STORAGE_PROJECT_NAME SELFHOST_STORAGE_COMPOSE_ARGS
          if [[ $operation == admit ]]; then selfhost_tls_admit; else selfhost_tls_ready 5; fi
          ;;
        empty-admit|empty-ready|unset-array-admit|unset-array-ready)
          SELFHOST_STORAGE_MODE_RESOLVED=bundled
          SELFHOST_STORAGE_PROJECT_ROOT=/tmp/concord-empty-authority
          SELFHOST_STORAGE_PROJECT_NAME=concordfixture
          if [[ $operation == unset-array-admit || $operation == unset-array-ready ]]; then
            unset SELFHOST_STORAGE_COMPOSE_ARGS
            declare -a SELFHOST_STORAGE_COMPOSE_ARGS
          else
            SELFHOST_STORAGE_COMPOSE_ARGS=()
          fi
          if [[ $operation == empty-admit || $operation == unset-array-admit ]]; then
            selfhost_tls_admit
          else
            selfhost_tls_ready 5
          fi
          ;;
        expired) selfhost_tls_ready 0 ;;
      esac
    ) 2>&1
  )"; then rc=0; else rc=$?; fi
  [[ $rc -eq 1 && $output == "$expected" ]] \
    || fail "$operation did not return its fixed safe refusal in an isolated subshell"
}
assert_shell_refusal admit 'selfhost-tls: resolved storage authority required'
assert_shell_refusal ready 'selfhost-tls: resolved storage authority required'
assert_shell_refusal empty-admit 'selfhost-tls: resolved storage authority required'
assert_shell_refusal empty-ready 'selfhost-tls: resolved storage authority required'
assert_shell_refusal unset-array-admit 'selfhost-tls: resolved storage authority required'
assert_shell_refusal unset-array-ready 'selfhost-tls: resolved storage authority required'
assert_shell_refusal expired 'selfhost-tls: caller deadline expired'
PATH=$saved_path
[[ ! -s "$TLS_SOURCE_PYTHON_CALLS" && ! -s "$TLS_SOURCE_DOCKER_CALLS" ]] \
  || fail 'unresolved/expired TLS calls reached Python or Docker'
unset COTURN_CERTS_DIR
# The detector served its source and unresolved-context purpose. Later
# construction uses real Python for the loopback listener and Python suite.
rm -f "$stage/bin/python3"

# Full selected-authority construction control. This section writes the
# canonical /opt fixture only inside the explicitly marked disposable root
# container used by the root-owned test command.
[[ ${SELFHOST_TLS_OWN_ROOT_FIXTURE:-} == 1 ]] \
  || fail 'SETUP: run full TLS shell integration inside the authorized disposable container'
[[ $(id -u) == 0 ]] || fail 'SETUP: full TLS shell integration requires container root'
CERT_PARENT=/opt/concord/certs
CERT_ROOT="$CERT_PARENT/coturn"
[[ ! -e "$CERT_PARENT" && ! -L "$CERT_PARENT" ]] \
  || fail 'SETUP: canonical certificate fixture path already exists in container'
mkdir -p "$CERT_PARENT"
mkdir -m 0711 "$CERT_ROOT"
created_cert_fixture=1
cleanup_tls_fixture() {
  if [[ -n ${listener_pid:-} ]]; then kill "$listener_pid" 2>/dev/null || true; wait "$listener_pid" 2>/dev/null || true; fi
  if [[ ${created_cert_fixture:-0} == 1 ]]; then
    rm -rf -- "$CERT_ROOT"
    rmdir -- "$CERT_PARENT" 2>/dev/null || true
  fi
}
trap 'cleanup_tls_fixture; rm -rf -- "$stage"' EXIT
chmod 0711 "$CERT_PARENT"
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -sha256 \
  -subj '/CN=ignored.invalid' -addext 'subjectAltName=DNS:example.invalid' \
  -keyout "$CERT_ROOT/key.pem" -out "$CERT_ROOT/cert.pem" >/dev/null 2>&1 \
  || fail 'SETUP: OpenSSL could not create benign TLS fixture'
chown 0:0 "$CERT_ROOT/cert.pem"; chmod 0444 "$CERT_ROOT/cert.pem"
chown 65534:65533 "$CERT_ROOT/key.pem"; chmod 0400 "$CERT_ROOT/key.pem"
test -f "$CERT_ROOT/cert.pem" && test -f "$CERT_ROOT/key.pem" \
  || fail 'SETUP: expected public/private fixture pair is missing'

fixture="$stage/project"
state="$stage/state"
mkdir -p "$fixture" "$state" "$stage/bin"
mkdir -p "$fixture/infrastructure/docker/coturn"
: >"$fixture/infrastructure/docker/coturn/turnserver.conf"
printf '%s\n' 'SELFHOST_STORAGE_MODE=bundled' 'DOMAIN=example.invalid' \
  'COTURN_CERTS_DIR=/opt/concord/certs/coturn' >"$fixture/.env"
for file in docker-compose.yml docker-compose.production.yml docker-compose.selfhost.yml docker-compose.byo-s3.yml; do : >"$fixture/$file"; done
cat >"$state/model.json" <<JSON
{"name":"concordfixture","services":{"coturn":{"image":"coturn/coturn:4.18.0-alpine@sha256:c29135c08565810d37053f384584392b7c24b69570fc81a721b878d6fe63e04e","command":["turnserver","-c","/etc/coturn/turnserver.conf","--static-auth-secret=fixture-turn-secret","--realm=concordvoice.chat","--listening-ip=0.0.0.0","--external-ip=192.0.2.10","--relay-ip=192.0.2.10","--cert=/etc/coturn/certs/cert.pem","--pkey=/etc/coturn/certs/key.pem"],"volumes":[{"type":"bind","source":"/opt/concord/certs/coturn","target":"/etc/coturn/certs","read_only":true},{"type":"bind","source":"$fixture/infrastructure/docker/coturn/turnserver.conf","target":"/etc/coturn/turnserver.conf","read_only":true}],"ports":[{"target":5349,"published":"5349","protocol":"tcp"}]}}}
JSON
cat >"$state/container.json" <<JSON
{"Config":{"Image":"coturn/coturn:4.18.0-alpine@sha256:c29135c08565810d37053f384584392b7c24b69570fc81a721b878d6fe63e04e","User":"nobody:nogroup","Labels":{"com.docker.compose.project":"concordfixture","com.docker.compose.service":"coturn"},"Cmd":["turnserver","-c","/etc/coturn/turnserver.conf","--static-auth-secret=fixture-turn-secret","--realm=concordvoice.chat","--listening-ip=0.0.0.0","--external-ip=192.0.2.10","--relay-ip=192.0.2.10","--cert=/etc/coturn/certs/cert.pem","--pkey=/etc/coturn/certs/key.pem"]},"State":{"Status":"running","Running":true,"Paused":false,"Restarting":false},"Mounts":[{"Type":"bind","Source":"/opt/concord/certs/coturn","Destination":"/etc/coturn/certs","RW":false},{"Type":"bind","Source":"$fixture/infrastructure/docker/coturn/turnserver.conf","Destination":"/etc/coturn/turnserver.conf","RW":false}],"NetworkSettings":{"Ports":{"5349/tcp":[{"HostIp":"0.0.0.0","HostPort":"5349"}]}}}
JSON
cat >"$state/inspect-template.txt" <<'TEMPLATE'
{"Config":{"Image":{{json .Config.Image}},"User":{{json .Config.User}},"Labels":{"com.docker.compose.project":{{json (index .Config.Labels "com.docker.compose.project")}},"com.docker.compose.service":{{json (index .Config.Labels "com.docker.compose.service")}}},"Cmd":{{json .Config.Cmd}}},"State":{"Status":{{json .State.Status}},"Running":{{json .State.Running}},"Paused":{{json .State.Paused}},"Restarting":{{json .State.Restarting}}},"Mounts":{{json .Mounts}},"NetworkSettings":{"Ports":{{json .NetworkSettings.Ports}}}}
TEMPLATE
cat >"$stage/listener.py" <<'PY'
import pathlib, socket, ssl, sys, time
sock=socket.socket(); sock.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
sock.bind(("127.0.0.1",5349)); sock.listen(20)
ctx=ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(sys.argv[1],sys.argv[2])
pathlib.Path(sys.argv[3]).touch()
while True:
    conn,_=sock.accept()
    try:
        conn.settimeout(4)
        if pathlib.Path(sys.argv[4]).exists():
            with open(sys.argv[5], "a", encoding="ascii") as events: events.write("tls\n")
            time.sleep(0.35)
        with ctx.wrap_socket(conn,server_side=True): pass
    except (OSError,ssl.SSLError):
        try: conn.close()
        except OSError: pass
PY
cat >"$stage/bin/docker" <<'SH'
#!/usr/bin/env bash
set -eu
if [[ ${COTURN_CERTS_DIR:-} == /tmp/caller-selected-certificate-directory ]]; then : >"$TLS_STATE/ambient-leak"; fi
printf '%s\0' "$@" >>"$TLS_STATE/docker-argv"
if [[ ${TLS_SCENARIO:-ok} == slow ]]; then
  sleep 2 & sleeper=$!
  trap 'kill "$sleeper" 2>/dev/null || true' TERM INT
  wait "$sleeper"
fi
if [[ "$1" == compose ]]; then
  shift
  verify_selected_authority() {
    local joined=" $* " expected_count=3 file_count=0 env_count=0 token
    [[ "$joined" == *' --project-name concordfixture '* && "$joined" == *' --profile services '* \
      && "$joined" == *"--env-file $TLS_FIXTURE/.env"* && "$joined" == *"--project-directory $TLS_FIXTURE"* ]] || return 1
    grep -q '^SELFHOST_STORAGE_MODE=byo-s3$' "$TLS_FIXTURE/.env" && expected_count=4
    for token in "$@"; do
      [[ $token == -f ]] && ((file_count+=1))
      [[ $token == --env-file ]] && ((env_count+=1))
    done
    [[ $file_count -eq $expected_count && $env_count -eq 1 ]] || return 1
    [[ "$joined" == *"-f $TLS_FIXTURE/docker-compose.yml -f $TLS_FIXTURE/docker-compose.production.yml -f $TLS_FIXTURE/docker-compose.selfhost.yml"* ]] || return 1
    if (( expected_count == 4 )); then
      [[ "$joined" == *"-f $TLS_FIXTURE/docker-compose.selfhost.yml -f $TLS_FIXTURE/docker-compose.byo-s3.yml"* ]] || return 1
    else
      [[ "$joined" != *docker-compose.byo-s3.yml* ]] || return 1
    fi
  }
  if [[ " $* " == *' -f - config --format json '* ]]; then
    printf '{"name":"concordfixture"}\n'; exit 0
  fi
  if [[ " $* " == *' config --format json '* ]]; then
    verify_selected_authority "$@" || { printf 'wrong-selected-authority\n' >&2; exit 43; }
    if [[ ${TLS_SCENARIO:-ok} == staged-slow ]]; then
      printf 'render\n' >>"$TLS_STATE/staged-events"
      sleep 0.10
    fi
    if [[ ${TLS_SCENARIO:-ok} == render-error ]]; then
      printf 'opaque-render-secret-sentinel\n' >&2; exit 41
    fi
    cat "$TLS_STATE/model.json"; exit 0
  fi
  if [[ " $* " == *' ps '* ]]; then
    verify_selected_authority "$@" || { printf 'wrong-selected-authority\n' >&2; exit 43; }
    [[ " $* " == *'--project-name concordfixture'* && " $* " == *'--profile services'* \
      && " $* " == *' ps --all --quiet coturn '* || " $* " == *' ps -a -q coturn '* ]] || exit 43
    printf '%s\n' 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'; exit 0
  fi
fi
if [[ "$1" == inspect || ( "$1" == container && ${2:-} == inspect ) ]]; then
  if [[ ${TLS_SCENARIO:-ok} == inspect-error ]]; then
    printf 'opaque-inspect-secret-sentinel\n' >&2; exit 42
  fi
  args=("$@")
  format='';
  for ((i=1; i<${#args[@]}-1; i++)); do
    if [[ ${args[$i]} == --format ]]; then format=${args[$((i+1))]}; fi
  done
  requested_id=${args[$((${#args[@]}-1))]}
  wanted_format="$(cat "$TLS_STATE/inspect-template.txt")"
  [[ "$format" == "$wanted_format" && "$requested_id" == aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ]] \
    || { printf 'unapproved-inspect-request\n' >&2; exit 43; }
  if [[ ${TLS_SCENARIO:-ok} == staged-slow ]]; then
    printf 'inspect\n' >>"$TLS_STATE/staged-events"
    sleep 0.10
  fi
  cat "$TLS_STATE/container.json"; exit 0
fi
printf 'unexpected-private-docker-call\n' >&2
exit 43
SH
chmod 0755 "$stage/bin/docker"
export TLS_STATE="$state"
export TLS_FIXTURE="$fixture"
export PATH="$stage/bin:/usr/bin:/bin"
source "$STORAGE"
source "$TLS_SHELL"
python3 "$stage/listener.py" "$CERT_ROOT/cert.pem" "$CERT_ROOT/key.pem" "$state/listener-ready" \
  "$state/staged-slow" "$state/staged-events" \
  >"$state/listener.log" 2>&1 &
listener_pid=$!
for _ in {1..100}; do [[ -e "$state/listener-ready" ]] && break; sleep 0.02; done
[[ -e "$state/listener-ready" ]] || fail 'SETUP: benign loopback TLS listener did not bind 5349'
export COTURN_CERTS_DIR=/tmp/caller-selected-certificate-directory
selfhost_storage_resolve "$fixture" >/dev/null 2>&1 \
  || fail 'SETUP: selected root/env/project/profile/file authority did not resolve'
expected_files=("$fixture/docker-compose.yml" "$fixture/docker-compose.production.yml" "$fixture/docker-compose.selfhost.yml")
actual_files=()
for ((i=0; i<${#SELFHOST_STORAGE_COMPOSE_ARGS[@]}; i++)); do
  [[ ${SELFHOST_STORAGE_COMPOSE_ARGS[$i]} == -f ]] && actual_files+=("${SELFHOST_STORAGE_COMPOSE_ARGS[$((i+1))]}")
done
[[ ${#actual_files[@]} -eq 3 && ${actual_files[0]} == "${expected_files[0]}" \
  && ${actual_files[1]} == "${expected_files[1]}" && ${actual_files[2]} == "${expected_files[2]}" ]] \
  || fail 'resolved model did not retain base, production, self-host order'
[[ ${SELFHOST_STORAGE_PROJECT_ROOT} == "$fixture" && ${SELFHOST_STORAGE_PROJECT_NAME} == concordfixture ]] \
  || fail 'resolved model root/project changed before TLS admission'
[[ ${SELFHOST_STORAGE_COMPOSE_ARGS[*]} == *'--profile services'* ]] \
  || fail 'resolved services profile was not retained'
selfhost_tls_admit >"$state/admit.out" 2>"$state/admit.err" \
  || fail 'ordinary selected model and canonical metadata did not admit'
selfhost_tls_ready >"$state/ready.out" 2>"$state/ready.err" \
  || fail 'ordinary inspected coturn and real loopback TLS did not become ready'
if [[ -e "$state/ambient-leak" ]]; then fail 'caller-selected certificate directory reached Compose/Docker'; fi
if grep -qE 'fixture-turn-secret|opaque-.*sentinel|-----BEGIN|PRIVATE KEY|/opt/concord/|example\.invalid' \
  "$state/admit.out" "$state/admit.err" "$state/ready.out" "$state/ready.err"; then
  fail 'private output contained secret, raw diagnostic, or key material'
fi

# Every TLS consumer call must preserve the exact currently resolved authority.
# Validate each ordinary config/ps invocation, not merely the earlier resolver.
python3 - "$state/docker-argv" "$fixture" <<'PY'
import pathlib, sys
raw=pathlib.Path(sys.argv[1]).read_bytes().split(b"\0")
tokens=[x.decode() for x in raw if x]
if not tokens or tokens[-1] != "": pass
# The stub independently checks each call below; this catches calls which
# bypassed the stub's recorded argument stream entirely.
if not tokens: sys.exit("FAIL: no selected Docker authority calls were recorded")
PY

# The alternate selected authority appends the BYO overlay as file four while
# preserving the same operation. Ambient values remain caller controls only.
cat >"$fixture/.env" <<'ENV'
SELFHOST_STORAGE_MODE=byo-s3
STORAGE_BACKEND=s3
STORAGE_ENDPOINT=objects.example.invalid
STORAGE_REGION=
STORAGE_ACCESS_KEY=synthetic-access
STORAGE_SECRET_KEY=synthetic-secret
STORAGE_USE_SSL=true
STORAGE_BUCKET=concord-media
DOMAIN=example.invalid
COTURN_CERTS_DIR=/opt/concord/certs/coturn
ENV
selfhost_storage_resolve "$fixture" >/dev/null 2>&1 \
  || fail 'SETUP: BYO selected root/env/project/profile/file authority did not resolve'
actual_files=()
for ((i=0; i<${#SELFHOST_STORAGE_COMPOSE_ARGS[@]}; i++)); do
  [[ ${SELFHOST_STORAGE_COMPOSE_ARGS[$i]} == -f ]] && actual_files+=("${SELFHOST_STORAGE_COMPOSE_ARGS[$((i+1))]}")
done
[[ ${#actual_files[@]} -eq 4 && ${actual_files[0]} == "$fixture/docker-compose.yml" \
  && ${actual_files[1]} == "$fixture/docker-compose.production.yml" \
  && ${actual_files[2]} == "$fixture/docker-compose.selfhost.yml" \
  && ${actual_files[3]} == "$fixture/docker-compose.byo-s3.yml" ]] \
  || fail 'BYO authority did not append its fourth overlay in order'
[[ ${SELFHOST_STORAGE_PROJECT_ROOT} == "$fixture" && ${SELFHOST_STORAGE_PROJECT_NAME} == concordfixture \
  && ${SELFHOST_STORAGE_COMPOSE_ARGS[*]} == *'--profile services'* \
  && ${SELFHOST_STORAGE_COMPOSE_ARGS[*]} == *"$fixture/.env"* ]] \
  || fail 'BYO TLS operation lost selected root/env/project/profile'
selfhost_tls_admit >"$state/byo-admit.out" 2>"$state/byo-admit.err" \
  || fail 'BYO selected model and canonical metadata did not admit'
selfhost_tls_ready >"$state/byo-ready.out" 2>"$state/byo-ready.err" \
  || fail 'BYO selected authority did not reach the TLS readiness consumer'
if grep -qE 'synthetic-access|synthetic-secret|fixture-turn-secret|PRIVATE KEY|/opt/concord/|example\.invalid' \
  "$state/byo-admit.out" "$state/byo-admit.err" "$state/byo-ready.out" "$state/byo-ready.err"; then fail 'BYO readiness leaked private fixture values'; fi

# Same otherwise-valid resolved control reaches each private refusal boundary.
export TLS_SCENARIO=render-error
rc=0; selfhost_tls_admit >"$state/refusal-one" 2>&1 || rc=$?
[[ $rc -ne 0 ]] || fail 'admission accepted failed Compose model render'
[[ -s "$state/refusal-one" ]] || fail 'render refusal omitted its fixed safe reason'
rc=0; selfhost_tls_admit >"$state/refusal-two" 2>&1 || rc=$?
[[ $rc -ne 0 ]] || fail 'admission accepted repeated failed Compose model render'
cmp -s "$state/refusal-one" "$state/refusal-two" || fail 'render refusal reason was not stable'
if grep -qE 'opaque-render-secret-sentinel|fixture-turn-secret|/opt/concord/|example\.invalid' "$state/refusal-one"; then fail 'render refusal leaked private diagnostics'; fi
export TLS_SCENARIO=inspect-error
rc=0; selfhost_tls_ready 5 >"$state/inspect-refusal" 2>&1 || rc=$?
[[ $rc -ne 0 ]] || fail 'readiness accepted failed selected-container inspection'
[[ -s "$state/inspect-refusal" ]] || fail 'inspection refusal omitted its fixed safe reason'
if grep -qE 'opaque-inspect-secret-sentinel|fixture-turn-secret|/opt/concord/|example\.invalid' "$state/inspect-refusal"; then fail 'inspect refusal leaked private diagnostics'; fi

# A caller-remainder expiry must bound the whole selected inspection path even
# when the fake Docker child is stalled; GNU date supplies subsecond evidence.
export TLS_SCENARIO=slow
before="$(date +%s%N)"
rc=0; selfhost_tls_ready 0.25 >"$state/deadline-refusal" 2>&1 || rc=$?
after="$(date +%s%N)"
[[ $rc -ne 0 ]] || fail 'readiness accepted expired caller remainder'
[[ -s "$state/deadline-refusal" ]] || fail 'deadline refusal omitted its fixed safe reason'
[[ $(cat "$state/deadline-refusal") == 'self-host TLS deadline exceeded' ]] \
  || fail 'stalled observation did not retain the fixed deadline diagnosis'
elapsed_ns=$((after-before))
(( elapsed_ns < 1500000000 )) || fail 'readiness exceeded caller deadline while inspection was stalled'
if grep -qE 'unexpected-private-docker-call|opaque-.*sentinel|fixture-turn-secret|/opt/concord/|example\.invalid' "$state/deadline-refusal"; then
  fail 'deadline refusal exposed raw tool diagnostics'
fi
[[ -s "$state/docker-argv" ]] || fail 'selected test never reached the Docker/Compose observation seam'

# Independent render, inspect, and TLS delays each fit a fresh 0.50-second
# budget, while their combined cost exceeds it. Reaching TLS proves one shared
# caller deadline spans later stages instead of restarting per operation.
export TLS_SCENARIO=staged-slow
: >"$state/staged-events"
: >"$state/staged-slow"
before="$(date +%s%N)"
rc=0; selfhost_tls_ready 0.50 >"$state/staged-refusal" 2>&1 || rc=$?
after="$(date +%s%N)"
rm -f "$state/staged-slow"
[[ $rc -ne 0 ]] || fail 'readiness reset its shared budget across render, inspect, and TLS'
[[ -s "$state/staged-refusal" ]] || fail 'shared staged deadline omitted its safe refusal'
[[ $(cat "$state/staged-refusal") == 'self-host TLS deadline exceeded' ]] \
  || fail 'staged shared budget did not report its deadline diagnosis'
grep -Fxq render "$state/staged-events" && grep -Fxq inspect "$state/staged-events" \
  && grep -Fxq tls "$state/staged-events" || fail 'shared deadline did not reach all staged observation steps'
elapsed_ns=$((after-before))
(( elapsed_ns < 1500000000 )) || fail 'staged shared deadline exceeded bounded failure time'
if grep -qE 'opaque-.*sentinel|fixture-turn-secret|/opt/concord/|example\.invalid' "$state/staged-refusal"; then
  fail 'staged deadline refusal exposed raw diagnostics'
fi

printf 'PASS: self-host TLS source and decoder contracts\n'
