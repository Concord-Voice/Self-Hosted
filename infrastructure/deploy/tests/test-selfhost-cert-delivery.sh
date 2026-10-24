#!/usr/bin/env bash
# #2504 Task 2 root-only deterministic acceptance for source-to-coturn delivery.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY="$(cd "$HERE/.." && pwd)"
if [[ "${SELFHOST_CERT_TEST_INNER:-}" != 1 ]]; then
  set -e
  command -v docker >/dev/null 2>&1 || { echo "FAIL: required Docker runtime unavailable" >&2; exit 1; }
  docker info >/dev/null 2>&1 || { echo "FAIL: required Docker daemon unavailable" >&2; exit 1; }
  stage="$(mktemp -d)"
  tooling_build="$(mktemp -d)"
  if [[ -n "${SELFHOST_CERT_TEST_IMAGE:-}" ]]; then
    image="$SELFHOST_CERT_TEST_IMAGE"
  else
    image="concord-2504-cert-test-tools:ubuntu24"
    cat >"$tooling_build/Dockerfile" <<'DOCKERFILE'
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends bash coreutils openssl util-linux findutils gawk grep sed python3 \
    && rm -rf /var/lib/apt/lists/*
DOCKERFILE
    docker build --network default -t "$image" "$tooling_build" >/dev/null || {
      echo "FAIL: public tooling bootstrap image build failed" >&2; rm -rf "$stage" "$tooling_build"; exit 1;
    }
  fi
  docker image inspect "$image" >/dev/null 2>&1 || { echo "FAIL: prepared public-tool fixture image unavailable: $image" >&2; rm -rf "$stage" "$tooling_build"; exit 1; }
  trap 'rm -rf "$stage" "$tooling_build"' EXIT
  mkdir -p "$stage/infrastructure/deploy/tests" "$stage/infrastructure/deploy"
  install -m 0644 "$HERE/test-selfhost-cert-delivery.sh" "$stage/infrastructure/deploy/tests/test-selfhost-cert-delivery.sh"
  install -m 0644 "$DEPLOY/copy-certs.sh" "$stage/infrastructure/deploy/copy-certs.sh"
  chmod -R a+rX "$stage"
  docker run --rm --network none --user 0:0 -e SELFHOST_CERT_TEST_INNER=1 \
    -v "$stage:/fixture:ro" "$image" bash /fixture/infrastructure/deploy/tests/test-selfhost-cert-delivery.sh
  rc=$?
  rm -rf "$stage"
  exit "$rc"
fi
[[ "$EUID" -eq 0 ]] || { echo "FAIL: delivery acceptance requires actual uid 0" >&2; exit 1; }
for utility in bash python3 openssl setpriv cmp stat sha256sum cut grep install ln mkdir chown chmod cp rmdir; do
  command -v "$utility" >/dev/null 2>&1 || { echo "FAIL: fixture image lacks required public utility: $utility" >&2; exit 1; }
done

HOOK="$DEPLOY/copy-certs.sh"
WORK="$(mktemp -d)"
STUB="$WORK/stubs"
LINEAGE="$WORK/lineage"
CUSTOM_PROJECT="checkout-fixture-2504"
mkdir -p "$STUB" "$LINEAGE"
chmod 711 "$WORK"
chmod 755 "$STUB"
trap 'rm -rf "$WORK"' EXIT INT TERM
fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; fails=$((fails + 1)); }

openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 30 \
  -keyout "$LINEAGE/privkey.pem" -out "$LINEAGE/fullchain.pem" \
  -subj '/CN=delivery.test' -addext 'subjectAltName=DNS:delivery.test,DNS:turn.delivery.test' >/dev/null 2>&1 || {
    echo "FAIL: valid runtime certificate fixture generation failed" >&2; exit 1;
  }
chmod 600 "$LINEAGE/privkey.pem"
chmod 644 "$LINEAGE/fullchain.pem"
chmod 711 "$LINEAGE"
printf 'application-secret-fixture\n' >"$WORK/application-secret.fixture"

# The recorder accepts ordinary Docker discovery/inspection forms. It records
# which seam was reached and emits bounded, authoritative state only; it never
# exposes environment values or credential material.
cat >"$STUB/docker" <<'STUB'
#!/bin/bash
printf '%s\n' "$*" >>"$DOCKER_CALLS"
case "${DOCKER_MODE:-absent}" in
  unavailable) exit 127 ;;
  discovery-fail) [[ "$1" == restart ]] && exit 0; exit 41 ;;
esac
case "${1:-}" in
  ps)
    all=0; want_json=0; prev=""; filters=(); ps_format=""; records=""
    selected_ps_format='{"ID":{{json .ID}},"Names":{{json .Names}},"Labels":{"com.docker.compose.project":{{json (.Label "com.docker.compose.project")}},"com.docker.compose.service":{{json (.Label "com.docker.compose.service")}}}}'
    for arg in "$@"; do
      [[ "$arg" == -a || "$arg" == --all ]] && all=1
      if [[ "$prev" == --format ]]; then ps_format="$arg"; fi
      if [[ "$arg" == --format=* ]]; then ps_format="${arg#--format=}"; fi
      if [[ "$prev" == --filter || "$prev" == -f ]]; then filters+=("$arg"); fi
      if [[ "$arg" == --filter=* || "$arg" == -f* ]]; then filters+=("${arg#*=}"); fi
      prev="$arg"
    done
    if [[ "$ps_format" == "$selected_ps_format" ]]; then
      want_json=1
    elif [[ "$ps_format" != *'.ID'* && "$ps_format" != *'.Names'* ]]; then
      exit 97
    fi
    record_selected() {
      local name="$1" state="$2" project="$3" service="$4" filter spec value actual key
      for filter in "${filters[@]}"; do
        case "$filter" in
          label=*)
            spec="${filter#label=}"; key="${spec%%=*}"; value=""
            [[ "$spec" == *=* ]] && value="${spec#*=}"
            case "$key" in
              com.docker.compose.project) actual="$project" ;;
              com.docker.compose.service) actual="$service" ;;
              *) return 1 ;;
            esac
            [[ -z "$value" || "$actual" == "$value" ]] || return 1
            ;;
          name=*) value="${filter#name=}"; [[ "$name" =~ $value ]] || return 1 ;;
          status=*) value="${filter#status=}"; [[ "$state" == "$value" ]] || return 1 ;;
          exited=*) [[ "$all" == 1 ]] || return 1 ;;
        esac
      done
      return 0
    }
    emit_record() {
      local name="$1" state="$2" project="$3" service="$4" id="$5"
      record_selected "$name" "$state" "$project" "$service" || return 0
      if [[ "$want_json" == 1 ]]; then
        records+="$(printf '{\"ID\":\"%s\",\"Names\":\"%s\",\"Labels\":{\"com.docker.compose.project\":\"%s\",\"com.docker.compose.service\":\"%s\"}}' \
          "$id" "$name" "$project" "$service")"$'\n'
      elif [[ "$ps_format" == *'.ID'* ]]; then records+="$id"$'\n'
      else records+="$name"$'\n'; fi
    }
    case "${DOCKER_MODE:-absent}" in
      running|restart-fail|paused|restarting|malformed|contradictory|nginx-fail|inspect-fail|inspect-fail-valid|inspect-empty|inspect-partial|inspect-project-drift) emit_record concordvoice-coturn running concordvoice coturn 2504c0ffee01 ;;
      running-custom) emit_record concordvoice-coturn running "$CUSTOM_PROJECT" coturn 2504c0ffee11 ;;
      unrelated)
        emit_record concordvoice-coturn running concordvoice coturn 2504c0ffee01
        emit_record concordvoice-coturn-copy running other-project sidecar 2504c0ffee02 ;;
      discovery-labels-string|discovery-labels-list|discovery-project-missing|discovery-service-invalid)
        # Deliberately violate the selected Docker projection after the stub
        # has accepted the requested filters, so discovery validation itself
        # must reject the record before inspect or delivery.
        case "${DOCKER_MODE}" in
          discovery-labels-string)
            records+='{"ID":"2504c0ffee01","Names":"concordvoice-coturn","Labels":"com.docker.compose.project=concordvoice,com.docker.compose.service=coturn"}'$'\n' ;;
          discovery-labels-list)
            records+='{"ID":"2504c0ffee01","Names":"concordvoice-coturn","Labels":["concordvoice","coturn"]}'$'\n' ;;
          discovery-project-missing)
            records+='{"ID":"2504c0ffee01","Names":"concordvoice-coturn","Labels":{"com.docker.compose.service":"coturn"}}'$'\n' ;;
          discovery-service-invalid)
            records+='{"ID":"2504c0ffee01","Names":"concordvoice-coturn","Labels":{"com.docker.compose.project":"concordvoice","com.docker.compose.service":"sidecar"}}'$'\n' ;;
        esac ;;
      created|exited)
        if [[ "$all" == 1 ]]; then emit_record concordvoice-coturn "${DOCKER_MODE}" concordvoice coturn 2504c0ffee01; fi ;;
      ambiguous)
        emit_record concordvoice-coturn running concordvoice coturn 2504c0ffee01
        # Impossible as real Docker names; this malformed daemon projection
        # deliberately duplicates the canonical name to exercise ambiguity.
        emit_record concordvoice-coturn running concordvoice coturn 2504c0ffee03 ;;
      *) : ;;
    esac
    printf '%s' "$records"
    ;;
  inspect)
    inspect_format=""; inspect_prev=""; requested=()
    for arg in "${@:2}"; do
      if [[ "$inspect_prev" == --format || "$inspect_prev" == -f ]]; then inspect_format="$arg"; inspect_prev=""; continue; fi
      if [[ "$inspect_prev" == --type || "$inspect_prev" == --platform ]]; then inspect_prev=""; continue; fi
      case "$arg" in
        --format=*) inspect_format="${arg#--format=}" ;;
        --format|-f|--type|--platform) inspect_prev="$arg" ;;
        -*) ;;
        *) requested+=("$arg") ;;
      esac
    done
    inspect_projection=raw
    if [[ -n "$inspect_format" ]]; then
      if [[ "$inspect_format" == *'.Name'* && "$inspect_format" == *'.Config.Image'* &&
            "$inspect_format" == *'com.docker.compose.project'* &&
            "$inspect_format" == *'com.docker.compose.service'* &&
            "$inspect_format" == *'.State.Status'* && "$inspect_format" == *'.State.Running'* &&
            "$inspect_format" == *'.State.Paused'* && "$inspect_format" == *'.State.Restarting'* ]]; then
        inspect_projection=selected-fields
      else
        printf 'unsupported-inspect-format-projection\n'
        exit 0
      fi
    fi
    is_requested() {
      local wanted="$1" requested_id
      (( ${#requested[@]} == 0 )) && return 0
      for requested_id in "${requested[@]}"; do
        [[ "$requested_id" == "$wanted" || "$requested_id" == "/$wanted" ]] && return 0
      done
      return 1
    }
    emit_inspect_record() {
      local id="$1" name="$2" image="$3" project="$4" service="$5" status="$6" running="$7" paused="$8" restarting="$9"
      is_requested "$id" || is_requested "$name" || return 0
      if [[ "$inspect_projection" == selected-fields ]]; then
        printf '{"Name":"/%s","Image":"%s","Project":"%s","Service":"%s","Status":"%s","Running":%s,"Paused":%s,"Restarting":%s}\n' \
          "$name" "$image" "$project" "$service" "$status" "$running" "$paused" "$restarting"
      else
        [[ -n "$inspect_records" ]] && inspect_records+=','
        inspect_records+="$(printf '{\"Id\":\"%s\",\"Name\":\"/%s\",\"Config\":{\"Image\":\"%s\",\"Labels\":{\"com.docker.compose.project\":\"%s\",\"com.docker.compose.service\":\"%s\"}},\"State\":{\"Status\":\"%s\",\"Running\":%s,\"Paused\":%s,\"Restarting\":%s}}' \
          "$id" "$name" "$image" "$project" "$service" "$status" "$running" "$paused" "$restarting")"
      fi
    }
    emit_inspect_set() {
      local image="coturn/coturn:4.18.0-alpine@sha256:c29135c08565810d37053f384584392b7c24b69570fc81a721b878d6fe63e04e" status=running running=true paused=false restarting=false inspect_records=""
      local project=concordvoice id=2504c0ffee01
      case "${DOCKER_MODE:-}" in
        created) status=created; running=false ;;
        exited) status=exited; running=false ;;
        paused) status=paused; running=true; paused=true ;;
        restarting) status=restarting; running=false; restarting=true ;;
        contradictory) ;;
        inspect-project-drift) project=valid-but-different-project ;;
        running-custom) project="$CUSTOM_PROJECT"; id=2504c0ffee11 ;;
      esac
      local canonical_name=concordvoice-coturn
      [[ "${DOCKER_MODE:-}" == contradictory ]] && canonical_name=other
      emit_inspect_record "$id" "$canonical_name" "$image" "$project" coturn "$status" "$running" "$paused" "$restarting"
      if [[ "${DOCKER_MODE:-}" == ambiguous ]]; then
        emit_inspect_record 2504c0ffee03 concordvoice-coturn "$image" concordvoice coturn running true false false
      fi
      if [[ "${DOCKER_MODE:-}" == unrelated ]]; then
        emit_inspect_record 2504c0ffee02 concordvoice-coturn-copy alpine:3.20 other-project sidecar running true false false
      fi
      [[ "$inspect_projection" == selected-fields ]] || printf '[%s]\n' "$inspect_records"
    }
    case "${DOCKER_MODE:-}" in
      inspect-fail) exit 44 ;;
      inspect-fail-valid) emit_inspect_set; exit 44 ;;
      inspect-empty) exit 0 ;;
      inspect-partial) printf '{"Name":"/concordvoice-coturn"' ;;
      malformed) printf 'not-json\n' ;;
      *) emit_inspect_set ;;
    esac
    ;;
  restart)
    if [[ "${DOCKER_MODE:-}" == running-custom &&
          ( $# -ne 2 || "$2" != 2504c0ffee11 ) ]]; then exit 43; fi
    [[ "${DOCKER_MODE:-}" == restart-fail ]] && exit 42
    exit 0 ;;
  *) exit 43 ;;
esac
STUB
cat >"$STUB/systemctl" <<'STUB'
#!/bin/bash
printf '%s\n' "$*" >>"$SYSTEMCTL_CALLS"
case "${1:-}" in
  is-active) [[ "${NGINX_ACTIVE:-0}" == 1 ]] ;;
  reload) [[ "${NGINX_RELOAD_FAIL:-0}" != 1 ]] ;;
  *) exit 1 ;;
esac
STUB
chmod 755 "$STUB/docker" "$STUB/systemctl"
export PATH="$STUB:/usr/bin:/bin" RENEWED_LINEAGE="$LINEAGE" COTURN_CERT_ROOT="$WORK/certs" CUSTOM_PROJECT
export DOCKER_CALLS="$WORK/docker.calls" SYSTEMCTL_CALLS="$WORK/systemctl.calls"

run_hook() {
  : >"$DOCKER_CALLS"; : >"$SYSTEMCTL_CALLS"
  DOCKER_MODE="${1:-absent}" NGINX_ACTIVE="${2:-0}" NGINX_RELOAD_FAIL="${3:-0}" \
    bash "$HOOK" >"$WORK/out" 2>&1
}
has_safe_reason() { grep -Fq -- "$2" "$1"; }
no_sensitive_output() {
  ! grep -Eiq 'BEGIN (CERTIFICATE|.*PRIVATE KEY)|privkey\.pem[[:space:]]*[:=]|tool failed:' "$1"
}
NO_DOCKER_PATH="$WORK/no-docker-path"; mkdir -p "$NO_DOCKER_PATH"
ln -s /usr/bin/install "$NO_DOCKER_PATH/install"
ln -s "$STUB/systemctl" "$NO_DOCKER_PATH/systemctl"
metadata_ok() {
  [[ "$(stat -c '%u:%g:%a' "$COTURN_CERT_ROOT")" == 0:0:711 &&
     "$(stat -c '%u:%g:%a' "$COTURN_CERT_ROOT/coturn")" == 0:0:711 &&
     "$(stat -c '%u:%g:%a' "$COTURN_CERT_ROOT/coturn/cert.pem")" == 0:0:444 &&
     "$(stat -c '%u:%g:%a' "$COTURN_CERT_ROOT/coturn/key.pem")" == 65534:65533:400 ]]
}
assert_reject() {
  local mode="$1" reason="$2"; rm -rf "$COTURN_CERT_ROOT"
  if run_hook "$mode"; then fail "$mode was accepted"; else
    has_safe_reason "$WORK/out" "$reason" && pass "$mode reaches $reason" || fail "$mode lacked its fixed failure class"
  fi
  grep -Eq '^(ps|inspect)' "$DOCKER_CALLS" && pass "$mode reached Docker observation seam" || fail "$mode did not reach Docker observation seam"
  no_sensitive_output "$WORK/out" && pass "$mode output is private-safe" || fail "$mode output leaked sensitive diagnostics"
}
assert_inspect_reject() {
  local mode="$1" reason="$2"
  assert_reject "$mode" "$reason"
  grep -Eq '^inspect([[:space:]]|$)' "$DOCKER_CALLS" && pass "$mode reached inspect result seam" || fail "$mode did not reach inspect result seam"
}
assert_discovery_reject() {
  local mode="$1"; rm -rf "$COTURN_CERT_ROOT"
  if run_hook "$mode" 1 0; then
    fail "$mode was accepted"
  else
    has_safe_reason "$WORK/out" 'coturn discovery failed; delivery not complete' &&
      pass "$mode refuses malformed discovery projection" || fail "$mode lacked its fixed discovery failure class"
  fi
  grep -Eq '^ps([[:space:]]|$)' "$DOCKER_CALLS" && pass "$mode reached Docker discovery seam" || fail "$mode did not reach Docker discovery seam"
  if grep -Eq '^inspect([[:space:]]|$)' "$DOCKER_CALLS"; then
    fail "$mode inspected a malformed discovery candidate"
  else pass "$mode refuses before inspect"; fi
  [[ ! -e "$COTURN_CERT_ROOT" ]] && pass "$mode refuses before certificate fan-out mutation" || fail "$mode mutated certificate fan-out"
  if grep -Eq '^restart([[:space:]]|$)' "$DOCKER_CALLS"; then
    fail "$mode restarted a malformed discovery candidate"
  else pass "$mode refuses before coturn restart"; fi
  if grep -q '^reload nginx$' "$SYSTEMCTL_CALLS"; then
    fail "$mode reloaded nginx after malformed discovery"
  else pass "$mode refuses before nginx reload"; fi
  no_sensitive_output "$WORK/out" && pass "$mode output is private-safe" || fail "$mode output leaked sensitive diagnostics"
}

# Managed pre-issuance bootstrap remains a quiet no-target success, including
# with Docker absent.
EMPTY="$WORK/empty"; mkdir -p "$EMPTY"
if RENEWED_LINEAGE="$EMPTY" COTURN_CERT_ROOT="$WORK/skip-certs" PATH="$STUB:/usr/bin:/bin" \
    DOCKER_MODE=unavailable bash "$HOOK" >"$WORK/out" 2>&1; then
  [[ ! -e "$WORK/skip-certs/coturn" ]] && pass "missing-pair managed bootstrap skips without target" || fail "missing-pair created target"
else fail "missing-pair managed bootstrap must exit zero"; fi

# A valid complete pair with conclusively absent coturn is a prepared-delivery
# control. The same source bytes must land as regular files with exact metadata.
if run_hook absent; then
  cmp -s "$LINEAGE/fullchain.pem" "$COTURN_CERT_ROOT/coturn/cert.pem" &&
    cmp -s "$LINEAGE/privkey.pem" "$COTURN_CERT_ROOT/coturn/key.pem" &&
    [[ -f "$COTURN_CERT_ROOT/coturn/cert.pem" && ! -L "$COTURN_CERT_ROOT/coturn/cert.pem" &&
       -f "$COTURN_CERT_ROOT/coturn/key.pem" && ! -L "$COTURN_CERT_ROOT/coturn/key.pem" ]] &&
    metadata_ok && pass "absent coturn control copies the real pair and exact metadata" || fail "absent control copy/metadata mismatch"
else fail "coherently absent coturn should permit prepared delivery"; fi
for coherent_state in created exited; do
  if run_hook "$coherent_state"; then
    grep -qE 'inspect([[:space:]]|$)' "$DOCKER_CALLS" &&
      ! grep -qE 'restart([[:space:]]|$)' "$DOCKER_CALLS" &&
      pass "coherent $coherent_state coturn permits prepared delivery without restart" || fail "$coherent_state was not authoritatively observed"
  else fail "coherent $coherent_state coturn control failed"; fi
done

# Root admission is tied to actual EUID. The pre-created paths let this case
# detect mutation without substituting an EUID variable or shell function.
if command -v setpriv >/dev/null 2>&1; then
  rm -rf "$WORK/nonroot-certs"; mkdir -p "$WORK/nonroot-certs"
  chown 65534:65534 "$WORK/nonroot-certs"; chmod 700 "$WORK/nonroot-certs"
  if setpriv --reuid=65534 --regid=65534 --clear-groups mkdir "$WORK/nonroot-certs/proof" &&
     [[ -d "$WORK/nonroot-certs/proof" ]]; then
    rmdir "$WORK/nonroot-certs/proof"
    pass "actual nonroot identity can mutate its isolated destination control"
  else fail "nonroot destination control could not be prepared"; fi
  if setpriv --reuid=65534 --regid=65534 --clear-groups env \
      RENEWED_LINEAGE="$LINEAGE" COTURN_CERT_ROOT="$WORK/nonroot-certs/out" \
      PATH="$STUB:/usr/bin:/bin" bash "$HOOK" >"$WORK/out" 2>&1; then
    fail "complete-pair nonroot copy hook accepted"
  else
    has_safe_reason "$WORK/out" 'root required; delivery not started' &&
      [[ ! -e "$WORK/nonroot-certs/out" ]] && pass "nonroot refuses before fan-out mutation" || fail "nonroot refusal was late or misclassified"
  fi
else fail "setpriv is required for actual nonroot namespace control"; fi

# Running canonical consumer is observed and restarted; ordinary absent
# created/exited consumers are projected as absent by discovery and stay safe.
DOCKER_MODE=running
if run_hook running; then grep -qE 'restart([[:space:]]|$)' "$DOCKER_CALLS" && pass "running canonical coturn restart reached" || fail "running control omitted restart"; else fail "running coturn control failed"; fi
if run_hook unrelated; then
  grep -qE 'restart([[:space:]]|$)' "$DOCKER_CALLS" && pass "similarly named unrelated container does not create ambiguity" || fail "unrelated control omitted canonical restart"
else fail "unrelated similarly named container blocked canonical control"; fi

# A normal checkout can carry a project label derived from its checkout name.
# The adapter applies the hook's project/service filters to that actual label;
# asking for the old concordvoice project therefore returns no candidate.
if run_hook running-custom; then
  [[ "$(grep -c '^restart 2504c0ffee11$' "$DOCKER_CALLS" || true)" == 1 &&
     "$(grep -c '^inspect --format .* 2504c0ffee11$' "$DOCKER_CALLS" || true)" == 1 ]] &&
    pass "custom checkout project restarts the exact inspected coturn identity" ||
    fail "running coturn under the custom checkout project omitted exact-identity restart"
else fail "custom checkout project running coturn control failed"; fi

assert_reject unavailable 'Docker unavailable; delivery not complete'
rm -rf "$COTURN_CERT_ROOT"
if PATH="$NO_DOCKER_PATH" RENEWED_LINEAGE="$LINEAGE" COTURN_CERT_ROOT="$COTURN_CERT_ROOT" \
    /bin/bash "$HOOK" >"$WORK/out" 2>&1; then
  fail "complete-pair copy accepted with Docker absent from PATH"
else
  has_safe_reason "$WORK/out" 'Docker unavailable; delivery not complete' &&
    [[ ! -e "$COTURN_CERT_ROOT/coturn/cert.pem" ]] && pass "missing Docker executable fails complete-pair delivery" || fail "missing Docker failure late or misclassified"
fi
assert_reject discovery-fail 'coturn discovery failed; delivery not complete'
assert_discovery_reject discovery-labels-string
assert_discovery_reject discovery-labels-list
assert_discovery_reject discovery-project-missing
assert_discovery_reject discovery-service-invalid
assert_inspect_reject inspect-fail 'coturn discovery failed; delivery not complete'
assert_inspect_reject inspect-fail-valid 'coturn discovery failed; delivery not complete'
assert_inspect_reject inspect-empty 'coturn discovery failed; delivery not complete'
assert_inspect_reject inspect-partial 'coturn discovery failed; delivery not complete'
assert_inspect_reject inspect-project-drift 'coturn state invalid; delivery not complete'
if grep -q '^restart ' "$DOCKER_CALLS"; then
  fail "inspect project mismatch restarted a consumer"
else
  pass "inspect project mismatch refuses before restart"
fi
assert_reject malformed 'coturn discovery failed; delivery not complete'
assert_reject ambiguous 'coturn identity ambiguous; delivery not complete'
assert_reject contradictory 'coturn state invalid; delivery not complete'
assert_reject paused 'coturn state invalid; delivery not complete'
assert_reject restarting 'coturn state invalid; delivery not complete'
assert_reject restart-fail 'coturn restart failed; delivery not complete'

# nginx is independently active; its successful reload control and failure
# case must each reach the actual reload seam after an otherwise-valid copy.
if run_hook absent 1 0; then
  grep -q '^reload nginx$' "$SYSTEMCTL_CALLS" && pass "active nginx success reaches reload seam" || fail "active nginx success omitted reload"
else fail "active nginx reload success control failed"; fi
if run_hook absent 1 1; then fail "active nginx reload failure accepted"; else
  has_safe_reason "$WORK/out" 'nginx reload failed; delivery not complete' &&
    grep -q 'reload nginx' "$SYSTEMCTL_CALLS" && pass "active nginx failure reaches reload seam" || fail "nginx reload failure not classified/reached"
fi

# Downstream restart failure leaves source and an application-secret fixture
# byte-for-byte untouched; copy-only retry then completes without reissuance.
cert_before="$(sha256sum "$LINEAGE/fullchain.pem" | cut -d' ' -f1)"
key_before="$(sha256sum "$LINEAGE/privkey.pem" | cut -d' ' -f1)"
secret_before="$(sha256sum "$WORK/application-secret.fixture" | cut -d' ' -f1)"
if run_hook restart-fail; then fail "downstream failure control unexpectedly succeeded"; fi
[[ "$(sha256sum "$LINEAGE/fullchain.pem" | cut -d' ' -f1)" == "$cert_before" &&
   "$(sha256sum "$LINEAGE/privkey.pem" | cut -d' ' -f1)" == "$key_before" &&
   "$(sha256sum "$WORK/application-secret.fixture" | cut -d' ' -f1)" == "$secret_before" ]] &&
  pass "failed downstream attempt preserves source and application-secret fixture" || fail "failed attempt mutated source fixture bytes"
if run_hook running; then
  cmp -s "$LINEAGE/fullchain.pem" "$COTURN_CERT_ROOT/coturn/cert.pem" &&
    cmp -s "$LINEAGE/privkey.pem" "$COTURN_CERT_ROOT/coturn/key.pem" &&
    [[ "$(sha256sum "$LINEAGE/fullchain.pem" | cut -d' ' -f1)" == "$cert_before" &&
       "$(sha256sum "$LINEAGE/privkey.pem" | cut -d' ' -f1)" == "$key_before" &&
       "$(sha256sum "$WORK/application-secret.fixture" | cut -d' ' -f1)" == "$secret_before" ]] &&
    pass "copy-only retry delivers same source while preserving fixture bytes" || fail "copy-only retry changed source/fixture bytes"
else fail "copy-only retry of valid source failed"; fi

no_sensitive_output "$WORK/out" && pass "final hook output contains no PEM/key diagnostics" || fail "final hook output unsafe"
echo "-- $fails delivery failures --"
exit $(( fails > 0 ))
