#!/usr/bin/env bash
# #1899 shared storage contracts. All roots, credentials and Docker state are synthetic.
# Mutations caught: inferred mode, permissive dotenv decoding, broad private-IP tests,
# ambient Compose drift, unchecked discovery, label/name confusion and stale-ID deletion.
set -euo pipefail
# The actual installer writer queried below must not inherit a hook's Git index
# or repository directory while checking its disposable environment destination.
# shellcheck disable=SC2046
unset $(git rev-parse --local-env-vars)
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
TEST_FILE="$ROOT/infrastructure/deploy/tests/test-selfhost-storage.sh"
SOURCE_ROOT="$ROOT"
if [[ ${SELFHOST_TEST_CHILD:-0} == 1 ]]; then
  SOURCE_ROOT="${SELFHOST_TEST_SOURCE_ROOT:-$ROOT}"
fi
SOURCE_VOLUME_ONLY=false
LOGICAL_VOLUME_ONLY=false
if [[ ${SELFHOST_TEST_CHILD:-0} != 1 && ( ${1:-} == --source-volume-freshness-only || ${1:-} == --logical-volume-names-only ) ]]; then
  if [[ $1 == --source-volume-freshness-only ]]; then SOURCE_VOLUME_ONLY=true
  else LOGICAL_VOLUME_ONLY=true; fi
  shift
  if [[ $# -eq 2 && $1 == --source-root && -n $2 ]]; then SOURCE_ROOT=$2; shift 2; fi
  [[ $# -eq 0 ]] || { printf 'Unsupported source-volume test arguments\n' >&2; exit 2; }
fi
# Test-only source selection replays saved helper bytes in the owned namespace.
HELPER="$SOURCE_ROOT/infrastructure/deploy/selfhost-storage.sh"

fail() { printf '%s\n' "FAIL: $1" >&2; exit 1; }
# Child assertions deliberately never print actual values, argv, environments or models.
require() { "$@" || fail 'contract assertion failed'; }
empty_ids() { [[ ${#SELFHOST_STORAGE_MINIO_IDS[@]} -eq 0 ]]; }
empty_resolver() {
  [[ -z ${SELFHOST_STORAGE_MODE_RESOLVED:-} && -z ${SELFHOST_STORAGE_PROJECT_ROOT:-}
     && -z ${SELFHOST_STORAGE_PROJECT_NAME:-} && ${#SELFHOST_STORAGE_COMPOSE_ARGS[@]} -eq 0 ]]
}
write_byo() {
  cat >"$FIX/.env" <<'ENV'
SELFHOST_STORAGE_MODE=byo-s3
STORAGE_BACKEND=s3
STORAGE_ENDPOINT=objects.example.invalid
STORAGE_REGION=
STORAGE_ACCESS_KEY="synthetic-access -n = $$cash \"quoted\" \\trail "
ENV
  printf '%s=%s\n' STORAGE_SECRET_KEY '"synthetic-secret -n = $$cash \"quoted\" \\trail "' >>"$FIX/.env"
  cat >>"$FIX/.env" <<'ENV'
STORAGE_USE_SSL=true
STORAGE_BUCKET=concord-media
UNRELATED=ordinary-ignored-value
ENV
}
reset_case() {
  rm -f -- "$FIX/.env" "$FIX/.env.activity-history-image.local" "$FIX/.env.activity-history-receipt.local" \
    "$FIX/infrastructure/deploy/.env.activity-history-image.local" \
    "$FIX/infrastructure/deploy/.env.activity-history-receipt.local"
  printf '%s' empty >"$STATE/scenario"
  rm -f -- "$STATE/stopped" "$STATE/rejected"
  rm -f -- "$STATE/fresh-rendered" "$STATE/fresh-generation" "$STATE/fresh-privileged" "$STATE/fresh-entry-output"
  rm -f -- "$STATE/source-volumes.json"
  : >"$STATE/fresh-events"
  : >"$STATE/calls.jsonl"
  : >"$STATE/calls.jsonl.models"
}
scenario() { printf '%s' "$1" >"$STATE/scenario"; }
ambient() {
  local key
  for key in SELFHOST_STORAGE_MODE STORAGE_BACKEND STORAGE_ENDPOINT STORAGE_REGION STORAGE_ACCESS_KEY \
    STORAGE_SECRET_KEY STORAGE_USE_SSL STORAGE_BUCKET MINIO_ENDPOINT MINIO_ACCESS_KEY MINIO_SECRET_KEY \
    MINIO_USE_SSL MINIO_BUCKET MINIO_ROOT_USER MINIO_ROOT_PASSWORD COMPOSE_FILE COMPOSE_PROFILES \
    COMPOSE_PROJECT_NAME COMPOSE_ENV_FILES COMPOSE_DISABLE_ENV_FILE INSTANCE_TYPE \
    ACTIVITY_HISTORY_CLUSTER_ENABLED CONTROL_PLANE_REPLICA_COUNT ACTIVITY_HISTORY_OPERATOR_NAME \
    ACTIVITY_HISTORY_PRIVACY_POLICY_URL ATTACHMENT_WRITE_BACKEND CONTROL_PLANE_IMAGE COMPOSE_OVERRIDE; do
    printf -v "$key" '%s' caller-sentinel
    export "$key"
  done
  COMPOSE_PROFILES='*'; COMPOSE_FILE=/synthetic/unapproved.yml; COMPOSE_PROJECT_NAME=unrelated
  export COMPOSE_PROFILES COMPOSE_FILE COMPOSE_PROJECT_NAME
}
read_mode() {
  local shape=$1
  case "$shape" in
    historical) printf 'DOMAIN=example.invalid\nMINIO_ROOT_USER=synthetic-root\nMINIO_ROOT_PASSWORD=synthetic-root-password\nMINIO_BUCKET=concord-media\n' >"$FIX/.env" ;;
    bundled) printf 'SELFHOST_STORAGE_MODE=bundled\n' >"$FIX/.env" ;;
    byo|minio|r2|b2|literal|unquoted|metadata-literal) write_byo
      case "$shape" in minio|r2|b2) sed -i "s/STORAGE_BACKEND=s3/STORAGE_BACKEND=$shape/" "$FIX/.env" ;;
        unquoted) sed -i '/^STORAGE_\(ACCESS\|SECRET\)_KEY=/d' "$FIX/.env"; printf 'STORAGE_ACCESS_KEY=synthetic-access\nSTORAGE_SECRET_KEY=synthetic-secret\n' >>"$FIX/.env" ;;
        metadata-literal) sed -i '/^STORAGE_\(REGION\|BUCKET\)=/d' "$FIX/.env"; printf '%s\n' 'STORAGE_REGION=" literal = $$cash \"quoted\" \\trail "' 'STORAGE_BUCKET="Concord_media:fixture"' >>"$FIX/.env" ;;
      esac ;;
  esac
  ambient
  selfhost_storage_read "$FIX" || return $?
  case "$shape" in historical|bundled) [[ $SELFHOST_STORAGE_MODE_RESOLVED == bundled ]] ;;
    *) [[ $SELFHOST_STORAGE_MODE_RESOLVED == byo-s3 ]] ;; esac
}
read_bad() {
  local shape=$1
  write_byo
  case "$shape" in
    bundled-block) printf 'SELFHOST_STORAGE_MODE=bundled\nSTORAGE_ENDPOINT=objects.example.invalid\n' >"$FIX/.env" ;;
    no-marker) sed -i '/^SELFHOST_STORAGE_MODE=/d' "$FIX/.env" ;;
    no-file) rm -f -- "$FIX/.env" ;;
    duplicate-mode) printf 'SELFHOST_STORAGE_MODE=byo-s3\n' >>"$FIX/.env" ;;
    empty-mode) sed -i 's/SELFHOST_STORAGE_MODE=byo-s3/SELFHOST_STORAGE_MODE=/' "$FIX/.env" ;;
    unknown-mode) sed -i "$(printf 's/SELFHOST_STORAGE_MODE=byo-s3/%s=%s/' SELFHOST_STORAGE_MODE unknown-mode-sentinel)" "$FIX/.env" ;;
    unknown-field) printf '%s=%s\n' STORAGE_UNKNOWN synthetic-extra >>"$FIX/.env" ;;
    missing-*) sed -i "/^${shape#missing-}=/d" "$FIX/.env" ;;
    empty-*) sed -i "s/^${shape#empty-}=.*/${shape#empty-}=/" "$FIX/.env" ;;
    duplicate-*) printf '%s=duplicate-field-sentinel\n' "${shape#duplicate-}" >>"$FIX/.env" ;;
    bad-escape) sed -i '/^STORAGE_SECRET_KEY=/d' "$FIX/.env"; printf '%s=%s\n' STORAGE_SECRET_KEY '"synthetic-secret\q"' >>"$FIX/.env" ;;
    unclosed) sed -i '/^STORAGE_SECRET_KEY=/c\STORAGE_SECRET_KEY="synthetic-secret' "$FIX/.env" ;;
    dollar) sed -i "$(printf '%s%s' '/^STORAGE_SECRET_KEY=/c\STORAGE_SECRET_KEY=' '"synthetic-secret$cash"')" "$FIX/.env" ;;
    single-quote) sed -i "$(printf '%s%s' '/^STORAGE_SECRET_KEY=/c\STORAGE_SECRET_KEY=' "'synthetic-secret'")" "$FIX/.env" ;;
    trailing-data) printf '%s=%s %s\n' STORAGE_SECRET_KEY '"synthetic-secret"' trailing-sentinel >"$STATE/line"; sed -i '/^STORAGE_SECRET_KEY=/d' "$FIX/.env"; cat "$STATE/line" >>"$FIX/.env" ;;
    nul) printf '\0' >>"$FIX/.env" ;;
    cr) printf '\r' >>"$FIX/.env" ;;
    invalid-utf8) printf '\377' >>"$FIX/.env" ;;
  esac
  SELFHOST_STORAGE_MODE_RESOLVED=byo-s3
  if [[ "$shape" == unknown-field ]]; then
    SELFHOST_STORAGE_PROJECT_ROOT=/synthetic/stale-root
    SELFHOST_STORAGE_PROJECT_NAME=stale-project
    SELFHOST_STORAGE_COMPOSE_ARGS=(-f /synthetic/stale-model.yml)
    SELFHOST_STORAGE_MINIO_IDS=(stale-id)
  fi
  local rc=0
  selfhost_storage_read "$FIX" || rc=$?
  [[ -z ${SELFHOST_STORAGE_MODE_RESOLVED:-} ]] || fail 'reader retained previous mode after refusal'
  if [[ "$shape" == unknown-field ]]; then require empty_resolver; require empty_ids; fi
  return "$rc"
}
validate_case() {
  local kind=$1 value=${2:-} mode=byo-s3 backend=s3 endpoint=objects.example.invalid region='' \
    access='synthetic-access' secret="$(printf '%s-%s' synthetic secret)" tls=true bucket=concord-media
  case "$kind" in
    bundled) mode=bundled; backend=''; endpoint=''; access=''; secret=''; tls=''; bucket='' ;;
    bundled-field) mode=bundled; backend=''; endpoint=$value; access=''; secret=''; tls=''; bucket='' ;;
    endpoint) endpoint=$value ;;
    private) endpoint=$value; tls=false ;;
    mode) mode=$value ;;
    backend) backend=$value ;;
    tls) tls=$value ;;
    region) region=$value ;;
    bucket) bucket=$value ;;
    access) access=$value ;;
    secret) secret=$value ;;
  esac
  selfhost_storage_validate "$mode" "$backend" "$endpoint" "$region" "$access" "$secret" "$tls" "$bucket"
}
# PR #3568 / 4162531678: classify a DNS spelling without rewriting persisted bytes.
private_dns_case() {
  local endpoint=$1 rc=0
  validate_case endpoint "$endpoint" || fail 'private DNS fixture is not syntactically accepted with TLS true'
  write_byo
  sed -i "/^STORAGE_ENDPOINT=/c\\STORAGE_ENDPOINT=$endpoint" "$FIX/.env"
  sed -i 's/^STORAGE_USE_SSL=true$/STORAGE_USE_SSL=false/' "$FIX/.env"
  cp "$FIX/.env" "$STATE/literal-storage-input"
  ambient
  selfhost_storage_read "$FIX" || rc=$?
  cmp -s "$FIX/.env" "$STATE/literal-storage-input" || fail 'private DNS classification rewrote persisted endpoint bytes'
  [[ "$rc" -ne 0 || "$SELFHOST_STORAGE_MODE_RESOLVED" == byo-s3 ]] || fail 'accepted private DNS did not resolve BYO mode'
  return "$rc"
}
project_case() {
  scenario "$1"; ambient
  SELFHOST_STORAGE_PROJECT_NAME=stale-result
  local rc=0
  selfhost_storage_project_identity "$FIX" || rc=$?
  if [[ "$rc" -eq 0 ]]; then [[ $SELFHOST_STORAGE_PROJECT_NAME == concordfixture ]] || fail 'wrong default project identity'
  else [[ -z ${SELFHOST_STORAGE_PROJECT_NAME:-} ]] || fail 'identity retained partial result'; fi
  return "$rc"
}
fresh_case() {
  local evidence=$1 phase=$2
  case "$evidence" in
    file) printf 'DOMAIN=example.invalid\n' >"$FIX/.env" ;;
    symlink) ln -s "$FIX/absent-target" "$FIX/.env" ;;
    image|receipt) printf 'synthetic-artifact\n' >"$FIX/.env.activity-history-$evidence.local" ;;
    deploy-image|deploy-receipt) printf 'synthetic-artifact\n' >"$FIX/infrastructure/deploy/.env.activity-history-${evidence#deploy-}.local" ;;
    *) scenario "$evidence" ;;
  esac
  ambient
  case "$evidence" in deploy-image|deploy-receipt)
    SELFHOST_STORAGE_MODE_RESOLVED=byo-s3
    SELFHOST_STORAGE_PROJECT_ROOT=/synthetic/stale-root
    SELFHOST_STORAGE_PROJECT_NAME=stale-project
    SELFHOST_STORAGE_COMPOSE_ARGS=(-f /synthetic/stale-model.yml)
    SELFHOST_STORAGE_MINIO_IDS=(stale-id)
    ;; esac
  local rc=0
  selfhost_storage_assert_fresh "$FIX" "$phase" || rc=$?
  case "$evidence" in
    running|stopped|volume|working-dir|unrelated|inspect-fail|inspect-malformed)
      python3 - "$STATE/calls.jsonl" "$evidence" "$FIX" <<'PY'
import json, sys
c = [json.loads(x)['argv'] for x in open(sys.argv[1])]
assert any(a[0] == 'inspect' or a[:2] in (['container','inspect'],['volume','inspect']) for a in c)
if sys.argv[2] == 'working-dir':
    # Either targeted or complete inventory discovery is legitimate. This owned
    # container belongs to another project but the current working directory;
    # the authoritative labels and the refusal, rather than one query spelling,
    # must establish that working-directory ownership still counts.
    assert any((a[0]=='inspect' or a[:2] in (['container','inspect'],['volume','inspect']))
               and 'cccccccccccc' in a and '--format' in a
               and '{{json .Config.Labels}}' in a[a.index('--format')+1] for a in c)
PY
      ;;
    deploy-image|deploy-receipt)
      [[ ! -s "$STATE/calls.jsonl" ]] || fail 'deployment artifact reached Docker discovery'
      require empty_resolver
      require empty_ids
      ;;
    file|symlink|image|receipt) [[ ! -s "$STATE/calls.jsonl" ]] || fail 'existing artifact reached Docker discovery' ;;
  esac
  return "$rc"
}
# Versioned archives and arbitrary source clones have different Compose project
# identities. These resources are owned fixture metadata, never a live Docker query.
source_volume_seed() {
  python3 - "$STATE/source-volumes.json" "$1" <<'SOURCE_VOLUME_SEED'
import json, pathlib, sys

destination, evidence = pathlib.Path(sys.argv[1]), sys.argv[2]
assert evidence.startswith('source-volume-')
scope, logical = evidence.removeprefix('source-volume-').split('-', 1)
projects = {'alpha':'concord-voice-alpha', 'public':'concord-voice',
            'unrelated':'unrelated-app', 'near':'concord-voice-alpha-backup',
            'clone':'alpha-prod', 'renamed':'voice_stack', 'numeric':'7archive'}
assert scope in projects
labels = {'com.docker.compose.project':projects[scope],
          'fixture.conservation':'volume-owned-private-sentinel'}
if logical != 'missing':
    labels['com.docker.compose.volume'] = {'null':None, 'bool':True, 'int':7,
        'list':['volume-owned-private-sentinel'], 'object':{'private':'volume-owned-private-sentinel'},
        'invalid':'invalid logical volume', 'empty':''}.get(logical, logical)
destination.write_text(json.dumps({'retained-source-' + scope + '_' + logical:labels}, sort_keys=True))
SOURCE_VOLUME_SEED
}
source_volume_snapshot() {
  python3 - "$STATE/source-volumes.json" <<'SOURCE_VOLUME_SNAPSHOT'
import hashlib, pathlib, sys
path = pathlib.Path(sys.argv[1])
details = path.lstat()
print(details.st_mode, details.st_ino, details.st_uid, details.st_gid,
      details.st_size, details.st_mtime_ns, hashlib.sha256(path.read_bytes()).hexdigest())
SOURCE_VOLUME_SNAPSHOT
}
source_volume_conserved() {
  [[ "$(source_volume_snapshot)" == "$1" ]] || fail 'freshness changed retained owned volume metadata'
  python3 - "$STATE/calls.jsonl" "$STATE/source-volumes.json" <<'SOURCE_VOLUME_READS'
import json, sys
calls = [json.loads(line)['argv'] for line in open(sys.argv[1])]
volumes = json.load(open(sys.argv[2]))
assert not any(token in ('stop', 'rm', 'up', 'restart', 'down', 'create', 'prune')
               for call in calls for token in call)
for name in volumes:
    assert any(call[:2] == ['volume', 'inspect'] and call[-1] == name
               and call[2:4] == ['--format', '{{json .Labels}}'] for call in calls), 'owned volume never reached its authoritative typed labels'
assert all('{{json .}}' not in call and '{{json .Config.Env}}' not in call for call in calls)
SOURCE_VOLUME_READS
}
namespace_fresh_case() {
  local evidence=$1 phase=$2 rc=0
  local conserved=''
  if [[ $evidence == source-volume-* ]]; then
    source_volume_seed "$evidence"
    conserved="$(source_volume_snapshot)"
  fi
  scenario "fresh-namespace-$evidence"; ambient
  selfhost_storage_assert_fresh "$FIX" "$phase" || rc=$?
  [[ -z $conserved ]] || source_volume_conserved "$conserved"
  if [[ $rc -ne 0 ]]; then
    require empty_resolver; require empty_ids
  fi
  return "$rc"
}
namespace_installer_case() {
  local evidence=$1 entry=$2 expected=$3 transition=${4:-early} rc=0
  local conserved=''
  if [[ $evidence == source-volume-* ]]; then
    source_volume_seed "$evidence"
    conserved="$(source_volume_snapshot)"
  fi
  scenario "fresh-namespace-$evidence"
  [[ $transition != final ]] || scenario fresh-namespace-empty
  source "$FIX/infrastructure/deploy/install-selfhost.sh"
  ENV_OUT="$FIX/.env"; NGINX_OUT="$STATE/fresh-rendered"
  run_preflight() {
    printf 'preflight\n' >>"$STATE/fresh-events"
    [[ $transition != final ]] || scenario "fresh-namespace-$evidence"
  }
  gen_secret() { : >"$STATE/fresh-generation"; printf '%s' aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; }
  gen_hex_key() { : >"$STATE/fresh-generation"; printf '%s' bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb; }
  source <(declare -f print_storage_review | sed '1s/print_storage_review/namespace_real_review/')
  print_storage_review() { printf 'review\n' >>"$STATE/fresh-events"; namespace_real_review "$@"; }
  source <(declare -f render_nginx | sed '1s/render_nginx/namespace_real_render/')
  render_nginx() { printf 'render\n' >>"$STATE/fresh-events"; namespace_real_render "$@"; }
  source <(declare -f write_env_file | sed '1s/write_env_file/namespace_real_writer/')
  write_env_file() { printf 'writer\n' >>"$STATE/fresh-events"; namespace_real_writer "$@"; }
  source <(declare -f provision_tls | sed '1s/provision_tls/namespace_real_tls/')
  provision_tls() { printf 'tls\n' >>"$STATE/fresh-events"; namespace_real_tls "$@"; }
  source <(declare -f print_summary | sed '1s/print_summary/namespace_real_summary/')
  print_summary() { printf 'summary\n' >>"$STATE/fresh-events"; namespace_real_summary "$@"; }
  # Both paths carry a complete otherwise-valid configuration. Explicit IP input
  # avoids spending an echo-service request; a curl tripwire still forbids one.
  PUBLIC_IP=10.0.0.5; export PUBLIC_IP
  curl() { : >"$STATE/rejected"; return 97; }
  sudo() { : >"$STATE/fresh-privileged"; return 97; }
  if [[ $entry == answers ]]; then
    printf '%s\n' 'CFG_DOMAIN=example.com' 'CFG_TLS_MODE=local' 'CFG_TURN_REALM=example.com' \
      'CFG_PUBLIC_IP=10.0.0.5' 'CFG_STORAGE_MODE=bundled' 'CFG_SMTP_HOST=relay.lan' \
      'CFG_SMTP_PORT=587' 'CFG_SMTP_FROM=operator@example.invalid' \
      'CFG_TRUSTED_CIDRS=172.16.0.0/12' 'CFG_FEEDBACK_ENABLED=no' >"$STATE/fresh-answers"
    main --env-file "$STATE/fresh-answers" >"$STATE/fresh-entry-output" 2>&1 || rc=$?
  else
    printf '%s\n' example.com local example.com 10.0.0.5 bundled relay.lan 587 fixture-user \
      fixture-password operator@example.invalid 172.16.0.0/12 '' '' no >"$STATE/fresh-stdin"
    main <"$STATE/fresh-stdin" >"$STATE/fresh-entry-output" 2>&1 || rc=$?
  fi
  [[ -z $conserved ]] || source_volume_conserved "$conserved"
  [[ ! -e "$STATE/rejected" && ! -e "$STATE/fresh-privileged" ]] || fail 'installer reached a forbidden fixture effect'
  ! compgen -G "$FIX/.env.new.*" >/dev/null || fail 'installer retained secret staging'
  ! grep -qE 'command not found|unbound variable|syntax error|Traceback|AssertionError|fixture-password' \
    "$STATE/fresh-entry-output" || fail 'installer did not reach its named freshness boundary'
  if [[ $expected == accept ]]; then
    [[ $rc -eq 0 && -f "$FIX/.env" && $(stat -c '%a' "$FIX/.env") == 600 \
       && -f "$STATE/fresh-rendered" && -e "$STATE/fresh-generation" ]] || fail 'valid unrelated-resource installer control failed'
    [[ $(cat "$STATE/fresh-events") == $'preflight\nreview\nrender\nwriter\ntls\nsummary' ]] || fail 'positive installer did not spend its real publication consumers'
    grep -qFx 'SMTP_HOST="relay.lan"' "$FIX/.env" || fail 'positive installer environment bytes differ'
  else
    [[ $rc -eq 1 && ! -e "$FIX/.env" && ! -L "$FIX/.env" && ! -e "$STATE/fresh-rendered" \
       && ! -e "$STATE/fresh-generation" ]] || fail 'freshness refusal allowed durable installer publication'
    if [[ $transition == final ]]; then
      [[ $(cat "$STATE/fresh-events") == preflight ]] || fail 'final freshness refusal reached a later installer consumer'
    else
      [[ ! -s "$STATE/fresh-events" ]] || fail 'early freshness refusal reached an installer consumer'
    fi
    local diagnostic='selfhost-storage: existing installation refused'
    [[ $evidence != *-fail && $evidence != *-malformed ]] || diagnostic='selfhost-storage: installation discovery unavailable'
    case $evidence in source-volume-*-missing|source-volume-*-null|source-volume-*-bool|source-volume-*-int|source-volume-*-list|source-volume-*-object|source-volume-*-invalid|source-volume-*-empty)
      diagnostic='selfhost-storage: installation discovery unavailable' ;;
    esac
    grep -qFx "$diagnostic" "$STATE/fresh-entry-output" || fail 'installer freshness refusal diagnostic changed'
  fi
}
resolve_case() {
  local kind=$1; local rc=0
  case "$kind" in
    historical) printf 'DOMAIN=example.invalid\n' >"$FIX/.env" ;;
    bundled) printf 'SELFHOST_STORAGE_MODE=bundled\n' >"$FIX/.env" ;;
    *) write_byo ;;
  esac
  case "$kind" in identity-conflict|identity-fail|identity-malformed) scenario "$kind" ;;
    bad-read) printf 'SELFHOST_STORAGE_MODE=byo-s3\n' >"$FIX/.env" ;; esac
  ambient
  case "$kind" in bad-read|identity-conflict|identity-fail|identity-malformed)
    SELFHOST_STORAGE_MODE_RESOLVED=byo-s3
    SELFHOST_STORAGE_PROJECT_ROOT=/synthetic/stale-root
    SELFHOST_STORAGE_PROJECT_NAME=stale-project
    SELFHOST_STORAGE_COMPOSE_ARGS=(-f /synthetic/stale-model.yml)
    ;; esac
  selfhost_storage_resolve "$FIX" || rc=$?
  if [[ "$rc" -ne 0 ]]; then require empty_resolver; return "$rc"; fi
  [[ $SELFHOST_STORAGE_PROJECT_ROOT == "$FIX" && $SELFHOST_STORAGE_PROJECT_NAME == concordfixture ]] || fail 'resolver identity changed'
  # Assert each canonical option and order; independent of option-group order.
  python3 - "$FIX" "$kind" "${SELFHOST_STORAGE_COMPOSE_ARGS[@]}" <<'PY'
import sys
root, mode, *a = sys.argv[1:]
pairs = [a[i:i+2] for i in range(0, len(a), 2)]
files = [p[1] for p in pairs if len(p) == 2 and p[0] == '-f']
want = [root + '/' + n for n in ('docker-compose.yml', 'docker-compose.production.yml', 'docker-compose.selfhost.yml')]
if mode not in ('historical', 'bundled'): want += [root + '/docker-compose.byo-s3.yml']
assert files == want
for flag, value in (('--env-file', root+'/.env'), ('--project-directory', root), ('--profile', 'services')):
    assert pairs.count([flag, value]) == 1
assert pairs.count(['--project-name','concordfixture']) + pairs.count(['-p','concordfixture']) == 1
assert len(a) == 2 * (len(want) + 4)
PY
  # Resolve/read results must not become interpolation environment variables.
  export -p >"$STATE/exports"
  ! grep -qE 'declare -x SELFHOST_STORAGE_(MODE_RESOLVED|PROJECT_ROOT|PROJECT_NAME|COMPOSE_ARGS)=' "$STATE/exports"
}
scrub_case() {
  ambient
  SELFHOST_UNRELATED_SETTING=kept; export SELFHOST_UNRELATED_SETTING
  selfhost_storage_scrub_environment || return $?
  local key
  for key in SELFHOST_STORAGE_MODE STORAGE_BACKEND STORAGE_ENDPOINT STORAGE_REGION STORAGE_ACCESS_KEY \
    STORAGE_SECRET_KEY STORAGE_USE_SSL STORAGE_BUCKET MINIO_ENDPOINT MINIO_ACCESS_KEY MINIO_SECRET_KEY \
    MINIO_USE_SSL MINIO_BUCKET MINIO_ROOT_USER MINIO_ROOT_PASSWORD COMPOSE_FILE COMPOSE_PROFILES \
    COMPOSE_PROJECT_NAME COMPOSE_ENV_FILES COMPOSE_DISABLE_ENV_FILE INSTANCE_TYPE \
    ACTIVITY_HISTORY_CLUSTER_ENABLED CONTROL_PLANE_REPLICA_COUNT ACTIVITY_HISTORY_OPERATOR_NAME \
    ACTIVITY_HISTORY_PRIVACY_POLICY_URL ATTACHMENT_WRITE_BACKEND CONTROL_PLANE_IMAGE COMPOSE_OVERRIDE; do
    [[ ! -v "$key" ]] || fail 'caller control survived scrub'
  done
  [[ $SELFHOST_UNRELATED_SETTING == kept ]]
}
# Test-only oracle control: exercise the same real helper, then publish a fixed
# diagnostic instead of its captured private stderr. No production copy is changed.
daemon_guidance_positive_adapter() {
  local rc=0
  selfhost_storage_compose "$@" >"$STATE/control-stdout" 2>"$STATE/control-stderr" || rc=$?
  [[ ! -s "$STATE/control-stdout" ]] || fail 'daemon failure unexpectedly produced stdout'
  printf '%s\n' 'selfhost-storage: Docker daemon unavailable or inaccessible; check Docker service and socket permissions' >&2
  return "$rc"
}
runner_case() {
  local kind=$1
  write_byo
  selfhost_storage_resolve "$FIX" || return $?
  [[ $SELFHOST_STORAGE_MODE_RESOLVED == byo-s3 && $SELFHOST_STORAGE_PROJECT_ROOT == "$FIX"
     && $SELFHOST_STORAGE_PROJECT_NAME == concordfixture ]] || fail 'runner model resolution did not succeed'
  ambient
  local image="registry.example.invalid/control-plane@sha256:$(printf 'a%.0s' {1..64})"
  [[ "$kind" != trusted-ipv6 ]] || image="[2001:db8::1]:5000/concord/control-plane@sha256:$(printf 'a%.0s' {1..64})"
  [[ "$kind" != trusted-digest ]] || image="sha256:$(printf 'a%.0s' {1..64})"
  case "$kind" in
    trusted|trusted-ipv6|trusted-digest|clear) selfhost_storage_select_image "$image" || return $? ;;
  esac
  [[ "$kind" != clear ]] || selfhost_storage_select_image '' || return $?
  [[ "$kind" != docker-status ]] || scenario runner-fail
  case "$kind" in daemon-guidance|daemon-control) scenario runner-daemon ;; log-success) scenario runner-stdout ;; esac
  cd "$STATE"
  local rc=0 runner=selfhost_storage_compose
  [[ "$kind" != daemon-control ]] || runner=daemon_guidance_positive_adapter
  "$runner" logs --tail 7 'control-plane' >"$STATE/runner-stdout" 2>"$STATE/runner-stderr" || rc=$?
  if [[ "$kind" == log-success ]]; then
    cmp -s "$STATE/runner-stdout" <(printf 'synthetic-log-stdout\n') || fail 'successful logs stdout was changed'
    [[ ! -s "$STATE/runner-stderr" ]] || fail 'successful logs emitted a spurious failure diagnostic'
  else
    [[ ! -s "$STATE/runner-stdout" ]] || fail 'failure diagnostics reached stdout'
  fi
  cat "$STATE/runner-stdout"
  cat "$STATE/runner-stderr" >&2
  python3 - "$STATE/calls.jsonl" "$FIX" "$kind" "$image" <<'PY'
import json, sys
path, root, kind, image = sys.argv[1:]
c = [json.loads(x) for x in open(path)]
assert len(c) == 3, 'runner must resolve twice and launch the operation exactly once'
assert all(x['argv'][0] == 'compose' and x['argv'][-3:] == ['config','--format','json'] for x in c[:2])
assert c[0]['argv'].count('-') == 1 and '-' not in c[1]['argv']
last = c[-1]
assert last['argv'][1:-4] == c[1]['argv'][1:-3], 'operation changed the selected model'
assert last['cwd'] == root
a = last['argv']; assert a[0] == 'compose' and a[-4:] == ['logs','--tail','7','control-plane']
assert last['env'] == ({'CONTROL_PLANE_IMAGE': image} if kind in ('trusted','trusted-ipv6','trusted-digest') else {})
assert not any('synthetic-access' in x or 'synthetic-secret' in x for x in a)
PY
  return "$rc"
}
ambient_coturn_consumer_case() {
  write_byo
  # Set the only changed input before actual model resolution. Both control and
  # alternate rows retain the same valid BYO file/model and caller controls.
  ambient
  if [[ ${1:-alternate} == alternate ]]; then
    COTURN_CERTS_DIR=/synthetic/caller-coturn-certs; export COTURN_CERTS_DIR
  else
    unset COTURN_CERTS_DIR
  fi
  local rc=0
  selfhost_storage_resolve "$FIX" || return $?
  selfhost_storage_compose logs --tail 7 control-plane >/dev/null 2>&1 || return $?
  python3 - "$STATE/calls.jsonl" <<'PY'
import json, sys
calls=[json.loads(line) for line in open(sys.argv[1])]
checked=[c for c in calls if c['argv'][-3:]==['config','--format','json']]
executed=[c for c in calls if c['argv'][-4:]==['logs','--tail','7','control-plane']]
assert len(checked)>=2 and len(executed)==1, 'real model check and operation were not both reached'
models=[json.loads(line) for line in open(sys.argv[1]+'.models')]
consumer=[m for m in models if 'coturn' in m.get('services',{})]
assert consumer and all(m['services']['coturn']['volumes']==[{'type':'bind','source':'./infrastructure/docker/coturn/certs','target':'/etc/coturn/certs','read_only':True}] for m in consumer), 'ambient selector changed the checked coturn mount'
assert all('COTURN_CERTS_DIR' not in c.get('ambient',{}) for c in checked+executed), 'ambient coturn selector reached checked or executed Compose'
PY
}
unresolved_runner() {
  SELFHOST_STORAGE_COMPOSE_ARGS=(); SELFHOST_STORAGE_PROJECT_ROOT=''
  local rc=0
  selfhost_storage_compose ps || rc=$?
  [[ ! -s "$STATE/calls.jsonl" ]] || fail 'unresolved runner called Docker'
  return "$rc"
}
minio_case() {
  local operation=$1 evidence=$2
  write_byo; selfhost_storage_resolve "$FIX" || return $?
  scenario "$evidence"
  SELFHOST_STORAGE_MINIO_IDS=(stale-result)
  local rc=0
  case "$operation" in
    find) selfhost_storage_find_minio || rc=$? ;;
    guard) selfhost_storage_assert_no_running_minio || rc=$? ;;
    stop) selfhost_storage_stop_stale_minio || rc=$? ;;
    bundled-guard|bundled-stop)
      SELFHOST_STORAGE_MODE_RESOLVED=bundled
      : >"$STATE/calls.jsonl"
      if [[ "$operation" == bundled-guard ]]; then selfhost_storage_assert_no_running_minio || rc=$?
      else selfhost_storage_stop_stale_minio || rc=$?; fi
      [[ ! -s "$STATE/calls.jsonl" ]] || fail 'bundled stale operation called Docker'
      return "$rc" ;;
  esac
  if [[ "$operation" == find && "$rc" -eq 0 ]]; then
    if [[ "$evidence" == minio-running || "$evidence" == minio-many || "$evidence" == minio-paused || "$evidence" == minio-restarting || "$evidence" == minio-restarting-transient ]]; then
      local -a want=(aaaaaaaaaaaa); [[ "$evidence" != minio-many ]] || want+=(bbbbbbbbbbbb)
      [[ "${SELFHOST_STORAGE_MINIO_IDS[*]}" == "${want[*]}" ]] || fail 'discovery did not return precise running IDs'
    else require empty_ids; fi
  elif [[ "$rc" -ne 0 ]]; then require empty_ids; fi
  if [[ "$operation" == stop && "$rc" -eq 0 ]]; then
    python3 - "$STATE/calls.jsonl" "$evidence" <<'PY'
import json, sys
c = [json.loads(x) for x in open(sys.argv[1])]
stops = [x['argv'][1:] for x in c if x['argv'][0] == 'stop']
want = ['aaaaaaaaaaaa','bbbbbbbbbbbb'] if sys.argv[2] == 'minio-many' else (['aaaaaaaaaaaa'] if sys.argv[2] in ('minio-running','minio-paused','minio-restarting','minio-restarting-transient') else [])
assert [i for s in stops for i in s] == want
if want:
    final_stop = max(i for i,x in enumerate(c) if x['argv'][0] == 'stop')
    assert any(x['argv'][0] in ('inspect','ps','container') for x in c[final_stop+1:])
PY
  fi
  return "$rc"
}
# Reach the inspect predicate independently of candidate-list filtering.
minio_metadata_case() {
  local evidence=$1 wanted=${2:-} verdict='' rc=0
  write_byo; selfhost_storage_resolve "$FIX" || return $?
  scenario "$evidence"
  verdict="$(_selfhost_storage_inspect_container aaaaaaaaaaaa minio-container "$SELFHOST_STORAGE_PROJECT_NAME")" || rc=$?
  python3 - "$STATE/calls.jsonl" <<'PY'
import json, sys
calls=[json.loads(line)['argv'] for line in open(sys.argv[1])]
assert len(calls)==3 and calls[-1][0]=='inspect', 'metadata predicate did not reach one authoritative inspect'
assert not any(token in ('stop','rm','up','restart','down') for call in calls for token in call)
PY
  if [[ "$rc" -eq 0 ]]; then
    [[ -z "$wanted" || "$verdict" == "$wanted" ]] || fail 'authoritative restarting snapshot was classified inactive'
  else
    [[ -z "$verdict" ]] || fail 'refused metadata retained a partial verdict'
  fi
  return "$rc"
}

if [[ ${SELFHOST_TEST_CHILD:-0} == 1 ]]; then
  # Reassert the stub function before source; command docker also resolves only to the stub.
  docker() { "$STUB_DOCKER" "$@"; }
  export -f docker
  export SELFHOST_STORAGE_TRUSTED_IMAGE=caller-sentinel SELFHOST_STORAGE_IMAGE=caller-sentinel
  source "$HELPER"
  "$@"
  exit $?
fi

[[ -f "$HELPER" ]] || fail 'shared storage helper is absent (selfhost-storage.sh)'
command -v python3 >/dev/null || fail 'python3 is required for isolated boundary fixtures'
WORK="$(mktemp -d /tmp/selfhost-storage-tests.XXXXXX)"
trap 'rm -rf -- "$WORK"' EXIT
chmod 700 "$WORK"
FIX="$WORK/concordfixture"; STATE="$WORK/state"; mkdir -p "$FIX/infrastructure/deploy" "$STATE" "$WORK/bin"
# The freshness entrypoint oracle sources the real installer and renderer from
# this owned root; no user environment or installation metadata is copied.
mkdir -p "$FIX/infrastructure/deploy/nginx"
cp -- "$ROOT/infrastructure/deploy/install-selfhost.sh" "$FIX/infrastructure/deploy/install-selfhost.sh"
cp -- "$HELPER" "$FIX/infrastructure/deploy/selfhost-storage.sh"
cp -- "$ROOT/infrastructure/deploy/nginx/concordvoice.conf" "$FIX/infrastructure/deploy/nginx/concordvoice.conf"
# Whitelist only model source files; never copy a checkout directory or real environment.
for model in docker-compose.yml docker-compose.production.yml docker-compose.selfhost.yml; do cp -- "$ROOT/$model" "$FIX/$model"; done
printf '' >"$FIX/spa.env"
# Exact proved candidate is fixture data until Task 5 supplies the production overlay.
cat >"$FIX/docker-compose.byo-s3.yml" <<'YAML'
services:
  minio:
    profiles:
      - selfhost-bundled-minio-inactive
  control-plane:
    depends_on: !override
      postgres:
        condition: service_healthy
      redis:
        condition: service_healthy
      nats:
        condition: service_healthy
    environment:
      STORAGE_BACKEND: "${STORAGE_BACKEND:?STORAGE_BACKEND must be set for BYO storage}"
      STORAGE_ENDPOINT: "${STORAGE_ENDPOINT:?STORAGE_ENDPOINT must be set for BYO storage}"
      STORAGE_REGION: "${STORAGE_REGION:-}"
      STORAGE_ACCESS_KEY: "${STORAGE_ACCESS_KEY:?STORAGE_ACCESS_KEY must be set for BYO storage}"
      STORAGE_SECRET_KEY: "${STORAGE_SECRET_KEY:?STORAGE_SECRET_KEY must be set for BYO storage}"
      STORAGE_USE_SSL: "${STORAGE_USE_SSL:?STORAGE_USE_SSL must be set for BYO storage}"
      STORAGE_BUCKET: "${STORAGE_BUCKET:?STORAGE_BUCKET must be set for BYO storage}"
YAML
cat >"$WORK/bin/docker" <<'PY'
#!/usr/bin/python3
import json, os, re, sys
from pathlib import Path
s = Path(os.environ['STATE']); root = os.environ['FIX']; a = sys.argv[1:]
controls = '''SELFHOST_STORAGE_MODE STORAGE_BACKEND STORAGE_ENDPOINT STORAGE_REGION STORAGE_ACCESS_KEY STORAGE_SECRET_KEY STORAGE_USE_SSL STORAGE_BUCKET MINIO_ENDPOINT MINIO_ACCESS_KEY MINIO_SECRET_KEY MINIO_USE_SSL MINIO_BUCKET MINIO_ROOT_USER MINIO_ROOT_PASSWORD COMPOSE_FILE COMPOSE_PROFILES COMPOSE_PROJECT_NAME COMPOSE_ENV_FILES COMPOSE_DISABLE_ENV_FILE INSTANCE_TYPE ACTIVITY_HISTORY_CLUSTER_ENABLED CONTROL_PLANE_REPLICA_COUNT ACTIVITY_HISTORY_OPERATOR_NAME ACTIVITY_HISTORY_PRIVACY_POLICY_URL ATTACHMENT_WRITE_BACKEND CONTROL_PLANE_IMAGE COMPOSE_OVERRIDE'''.split()
with (s/'calls.jsonl').open('a') as f: f.write(json.dumps({'argv':a,'cwd':os.getcwd(),'env':{k:os.environ[k] for k in controls if k in os.environ},'ambient':{k:os.environ[k] for k in ('COTURN_CERTS_DIR',) if k in os.environ}})+'\n')
scenario = (s/'scenario').read_text()
def reject():
    (s/'rejected').touch(); print('stub: unrecognized Docker request', file=sys.stderr); sys.exit(97)
def error():
    print('synthetic-secret-render-sentinel', file=sys.stderr); sys.exit(42)
def options(args):
    result = {}; i = 0
    while i < len(args):
        arg = args[i]
        if arg in ('-a','--all','-q','--quiet','--no-trunc'): result[arg] = True; i += 1
        elif arg in ('--filter','-f','--format') and i+1 < len(args): result.setdefault(arg, []).append(args[i+1]); i += 2
        elif arg.startswith('--filter='): result.setdefault('--filter', []).append(arg.split('=',1)[1]); i += 1
        else: reject()
    return result
def output_form(opts, values):
    fmt = opts.get('--format',[''])[0]
    if not fmt: print('\n'.join(values)); return
    if fmt not in ('{{.ID}}','{{.Name}}','{{.Names}}','{{json .}}','json'): reject()
    if fmt in ('{{json .}}','json'):
        for v in values: print(json.dumps({'ID':v,'Name':v,'Names':v}))
    else: print('\n'.join(values))
namespace = scenario.removeprefix('fresh-namespace-') if scenario.startswith('fresh-namespace-') else None
namespace_containers = {}; namespace_volumes = {}
if namespace is not None:
    old_project='concord-selfhost-0246'
    old_labels={'com.docker.compose.project':old_project,'com.docker.compose.service':'postgres',
        'com.docker.compose.project.working_dir':'/synthetic/concord-selfhost-0.2.46'}
    if namespace in ('old-container','container-inspect-fail','container-inspect-malformed'):
        # A legitimate retained container can be renamed; project ownership must
        # survive that rename and a different version's extraction directory.
        namespace_containers['aaaaaaaaaaaa']=('/archived-postgres',old_labels)
    elif namespace=='fixed-name':
        namespace_containers['bbbbbbbbbbbb']=('/concordvoice-postgres',{})
    elif namespace in ('unrelated','near-name'):
        name='/concordvoice-postgres-exporter' if namespace=='near-name' else '/unrelated-postgres'
        namespace_containers['cccccccccccc']=(name,{'com.docker.compose.project':'unrelated',
            'com.docker.compose.service':'postgres','com.docker.compose.project.working_dir':'/synthetic/unrelated'})
    elif namespace=='unlabeled-unrelated':
        namespace_containers['cccccccccccc']=('/unrelated-postgres',None)
    if namespace in ('old-volume','volume-inspect-fail','volume-inspect-malformed'):
        namespace_volumes[old_project+'_postgres-data']={'com.docker.compose.project':old_project,
            'com.docker.compose.volume':'postgres-data'}
    elif namespace in ('unrelated','near-name'):
        # Preserve the independent unrelated-container admission controls with
        # a nonpersistent key. Generic persistent keys now require ambiguity
        # refusal and are exercised with conservation by source-volume cases.
        namespace_volumes['unrelated_cache-data']={'com.docker.compose.project':'unrelated',
            'com.docker.compose.volume':'cache-data'}
    elif namespace=='unlabeled-unrelated':
        namespace_volumes['unrelated-cache']=None
    elif namespace.startswith('source-volume-'):
        # Persistent owned fixture state models volumes retained after down,
        # without containers or a working-directory label to rescue discovery.
        namespace_volumes=json.loads((s/'source-volumes.json').read_text())
def namespace_matches(filters, name, labels):
    for value in filters:
        if value.startswith('label='):
            key,separator,want=value[6:].partition('=')
            if key not in ('com.docker.compose.project','com.docker.compose.project.working_dir',
                           'com.docker.compose.service','com.docker.compose.volume'): reject()
            if not isinstance(labels,dict) or key not in labels or separator and labels[key]!=want: return False
        elif value.startswith('name='):
            try: matches=re.search(value[5:],name) or re.search(value[5:],name.lstrip('/'))
            except re.error: reject()
            if not matches: return False
        elif value=='status=running': return False # All retained containers are stopped.
        else: reject()
    return True
if not a: reject()
if scenario == 'unavailable': sys.exit(127)
if a[0] == 'compose':
    b = a[1:]; pairs = []; i = 0
    while i < len(b) and b[i] in ('--env-file','--project-directory','--project-name','-p','-f','--profile'):
        if i+1 >= len(b): reject()
        pairs.append(b[i:i+2]); i += 2
    tail = b[i:]
    identity = ['-f','-'] in pairs
    if identity:
        if ['--project-directory',root] not in pairs or ['--env-file','/dev/null'] not in pairs or tail != ['config','--format','json']: reject()
        if any(k in os.environ for k in controls): reject()
        sys.stdin.read()
    else:
        expected = [root+'/'+x for x in ('docker-compose.yml','docker-compose.production.yml','docker-compose.selfhost.yml')]
        text = (Path(root)/'.env').read_text()
        if 'SELFHOST_STORAGE_MODE=byo-s3' in text: expected.append(root+'/docker-compose.byo-s3.yml')
        if [p[1] for p in pairs if p[0]=='-f'] != expected: reject()
        for p in (['--env-file',root+'/.env'],['--project-directory',root],['--profile','services']):
            if pairs.count(p)!=1: reject()
        if sum(p in (['--project-name','concordfixture'],['-p','concordfixture']) for p in pairs)!=1: reject()
        if len(pairs)!=len(expected)+4: reject()
        if any(k in os.environ for k in controls if k!='CONTROL_PLANE_IMAGE'): reject()
    if tail == ['config','--format','json']:
        if scenario == 'identity-fail': error()
        if scenario == 'identity-malformed': print('{synthetic-secret-render-sentinel'); sys.exit(0)
        name = 'unrelated' if scenario == 'identity-invalid' or (scenario == 'identity-conflict' and not identity) else 'concordfixture'
        if scenario == 'identity-invalid': name = 'invalid name'
        model={'name':name,'services':{'control-plane':{'environment':{'STORAGE_SECRET_KEY':'-'.join(('synthetic','secret','render','sentinel'))}}}}
        if not identity:
            selector=os.environ.get('COTURN_CERTS_DIR','./infrastructure/docker/coturn/certs')
            model['services']['coturn']={'volumes':[{'type':'bind','source':selector,'target':'/etc/coturn/certs','read_only':True}]}
        with (s/'calls.jsonl.models').open('a') as f: f.write(json.dumps(model)+'\n')
        print(json.dumps(model)); sys.exit(0)
    if tail == ['logs','--tail','7','control-plane']:
        if scenario == 'runner-daemon':
            print('Cannot connect to the Docker daemon. Is the docker daemon running?', file=sys.stderr)
            print('opaque-stderr-sentinel', file=sys.stderr); sys.exit(43)
        if scenario == 'runner-fail':
            print('Unrecognized synthetic operation failure: opaque-stderr-sentinel', file=sys.stderr); sys.exit(43)
        if scenario == 'runner-stdout': print('synthetic-log-stdout')
        sys.exit(0)
    reject()
if a[0] == 'version' and len(a)==3 and a[1]=='--format': print('28.4.0'); sys.exit(0)
if a == ['info'] or (a[0]=='info' and len(a)==3 and a[1]=='--format'): sys.exit(0)
kind = a[0]; rest = a[1:]
if kind == 'container' and rest and rest[0] in ('ls','inspect'): kind = 'ps' if rest[0]=='ls' else 'inspect'; rest=rest[1:]
if kind == 'volume' and rest and rest[0] in ('ls','inspect'): kind='volume-'+rest[0]; rest=rest[1:]
if kind in ('ps','volume-ls'):
    opts=options(rest); filters=opts.get('--filter',[])+opts.get('-f',[])
    if namespace is not None:
        if namespace==('container-list-fail' if kind=='ps' else 'volume-list-fail'): error()
        if kind=='ps':
            values=[ident for ident,(name,labels) in namespace_containers.items()
                    if ('--all' in opts or '-a' in opts) and namespace_matches(filters,name,labels)]
        else:
            values=[name for name,labels in namespace_volumes.items() if namespace_matches(filters,name,labels)]
        output_form(opts,values); sys.exit(0)
    project='label=com.docker.compose.project=concordfixture'
    working='label=com.docker.compose.project.working_dir='+root
    service='label=com.docker.compose.service=minio'
    allowed={project,working,service,'status=running','label=com.docker.compose.project'}
    if any(f not in allowed for f in filters): reject()
    if kind=='volume-ls' and filters not in ([],[project],['label=com.docker.compose.project']): reject()
    if kind=='ps' and filters and project not in filters and working not in filters and 'label=com.docker.compose.project' not in filters: reject()
    if scenario in ('discovery-fail','minio-discovery-fail') or (scenario=='minio-verification-fail' and (s/'stopped').exists()): error()
    if scenario in ('discovery-malformed','minio-discovery-malformed'): print('malformed-id-sentinel'); sys.exit(0)
    vals=[]
    if kind=='volume-ls' and scenario=='volume': vals=['concordfixture_media']
    if kind=='ps':
        broad=not filters or filters==['label=com.docker.compose.project']
        if scenario in ('running','unrelated','inspect-fail','inspect-malformed') and (project in filters or broad): vals=['aaaaaaaaaaaa']
        if scenario=='stopped' and (project in filters or broad) and ('--all' in opts or '-a' in opts): vals=['aaaaaaaaaaaa']
        if scenario=='working-dir' and (working in filters or broad): vals=['cccccccccccc']
        if scenario.startswith('minio-') and service in filters:
            vals=['aaaaaaaaaaaa'] if scenario!='minio-empty' else []
            if scenario=='minio-many': vals+=['bbbbbbbbbbbb']
            # A lexical running filter omits ordinary paused/restarting states.
            # One restart-cycle snapshot has Running=false, Restarting=true.
            # No real lifecycle or restart loop is simulated.
            if scenario in ('minio-paused','minio-restarting','minio-restarting-transient') and 'status=running' in filters: vals=[]
            if scenario in ('minio-stopped','minio-created') and 'status=running' in filters: vals=[]
            if (s/'stopped').exists() and scenario!='minio-still-running' and 'status=running' in filters: vals=[]
    output_form(opts, vals); sys.exit(0)
if kind in ('inspect','volume-inspect'):
    fmt=None; ids=[]; i=0
    while i<len(rest):
        if rest[i] in ('--format','-f') and i+1<len(rest): fmt=rest[i+1]; i+=2
        elif rest[i]=='--type' and i+1<len(rest) and rest[i+1] in ('container','volume'): i+=2
        elif re.fullmatch(r'[a-c]{12}|concordfixture_media',rest[i]) or rest[i] in namespace_volumes: ids.append(rest[i]); i+=1
        else: reject()
    if not ids: reject()
    if scenario in ('inspect-fail','minio-inspect-fail') or (scenario=='minio-verification-fail' and (s/'stopped').exists()): error()
    if scenario in ('inspect-malformed','minio-inspect-malformed'): print('{synthetic-secret-render-sentinel'); sys.exit(0)
    if namespace is not None:
        failure='container-inspect-fail' if kind=='inspect' else 'volume-inspect-fail'
        malformed='container-inspect-malformed' if kind=='inspect' else 'volume-inspect-malformed'
        if namespace==failure: error()
        if namespace==malformed: print('{synthetic-secret-render-sentinel'); sys.exit(0)
    models=[]
    for ident in ids:
        if namespace is not None:
            if kind=='inspect':
                if ident not in namespace_containers: reject()
                name,labels=namespace_containers[ident]
            else:
                if ident not in namespace_volumes: reject()
                name,labels=ident,namespace_volumes[ident]
            models.append({'Id':ident,'Name':name,'Config':{'Labels':labels},'Labels':labels,
                'State':{'Running':False,'Status':'exited','Paused':False,'Restarting':False}})
            continue
        project='unrelated' if scenario in ('working-dir','unrelated','minio-unrelated') else 'concordfixture'
        service='redis' if scenario=='minio-wrong-service' else 'minio'
        running = scenario not in ('stopped','minio-stopped','minio-created','minio-restarting-transient') and not (s/'stopped').exists()
        if scenario=='minio-still-running': running=True
        labels={'com.docker.compose.project':project,'com.docker.compose.service':service,'com.docker.compose.project.working_dir':root if scenario=='working-dir' else '/synthetic/other-root'}
        if scenario=='minio-missing-label': labels.pop('com.docker.compose.project')
        if scenario in ('minio-restarting','minio-restarting-transient') and not (s/'stopped').exists(): status='restarting'
        elif scenario=='minio-paused' and running: status='paused'
        elif scenario=='minio-created' and not (s/'stopped').exists(): status='created'
        else: status='running' if running else 'exited'
        state={'Running':running,'Status':status,'Paused':status=='paused','Restarting':status=='restarting'}
        if scenario=='minio-state-missing': state.pop('Status')
        if scenario=='minio-state-unknown': state.update(Running=False,Status='unknown',Paused=False,Restarting=False)
        if scenario=='minio-state-contradictory': state.update(Running=False,Status='exited',Paused=True,Restarting=False)
        models.append({'Id':ident,'Name':'/synthetic-container','Config':{'Labels':labels,'Env':['STORAGE_SECRET_KEY=synthetic-secret-render-sentinel']},'State':state,'Labels':labels})
    if not fmt:
        if namespace is not None: reject()
        print(json.dumps(models)); sys.exit(0)
    if fmt=='{{json .}}':
        if namespace is not None: reject()
        print('\n'.join(json.dumps(m) for m in models)); sys.exit(0)
    # Bounded authoritative metadata templates; no Config.Env/name dumps permitted.
    tokens=re.findall(r'{{.*?}}',fmt)
    if not tokens or re.sub(r'{{.*?}}','',fmt).strip(' \t|\\t') != '': reject()
    for m in models:
        result=fmt
        for token in tokens:
            label=re.fullmatch(r'{{\s*index \.Config\.Labels "(com\.docker\.compose\.(?:project|service|project\.working_dir))"\s*}}',token)
            label2=re.fullmatch(r'{{\s*index \.Labels "(com\.docker\.compose\.project)"\s*}}',token)
            if label or label2: value=m['Config']['Labels'].get((label or label2).group(1),'')
            elif token in ('{{.State.Running}}','{{ .State.Running }}'): value=str(m['State']['Running']).lower()
            elif token in ('{{json .Config.Labels}}','{{json .Labels}}'): value=json.dumps(m['Config']['Labels'])
            elif token=='{{json .State}}': value=json.dumps(m['State'])
            elif token=='{{json .Name}}': value=json.dumps(m['Name'])
            elif token in ('{{.Name}}','{{ .Name }}'): value=m['Name']
            else: reject()
            result=result.replace(token,value)
        print(result)
    sys.exit(0)
if kind=='stop':
    if not rest or any(i not in ('aaaaaaaaaaaa','bbbbbbbbbbbb') for i in rest): reject()
    if scenario=='minio-stop-fail': error()
    (s/'stopped').touch(); sys.exit(0)
reject()
PY
chmod 700 "$WORK/bin/docker"
STUB_DOCKER="$WORK/bin/docker"
# Optional root-run coverage keeps the same isolated child boundary. Only synthetic
# data enters traces. No installed-tool or hardcoded /tmp tooling dependency by default.
COVERAGE_INDEX=0
if [[ -n ${SELFHOST_TEST_KCOV:-} ]]; then
  [[ "$SELFHOST_TEST_KCOV" == /* && -x "$SELFHOST_TEST_KCOV" ]] || fail 'coverage binary must be an absolute executable'
  [[ ${SELFHOST_TEST_COVERAGE_ROOT:-} == /tmp/* && ! -e "$SELFHOST_TEST_COVERAGE_ROOT" ]] || fail 'coverage requires a new absolute temporary output root'
  [[ -z ${SELFHOST_TEST_KCOV_LIBRARY_PATH:-} || "$SELFHOST_TEST_KCOV_LIBRARY_PATH" == /* ]] || fail 'coverage library path must be absolute'
  mkdir -- "$SELFHOST_TEST_COVERAGE_ROOT"
  chmod 700 "$SELFHOST_TEST_COVERAGE_ROOT"
fi
PASS=0; FAIL=0
expect() {
  local name=$1 want=$2; shift 2
  reset_case
  local rc=0
  local -a invocation=(bash "$TEST_FILE" "$@") coverage_env=()
  if [[ -n ${SELFHOST_TEST_KCOV:-} ]]; then
    COVERAGE_INDEX=$((COVERAGE_INDEX+1))
    local case_dir
    printf -v case_dir '%s/case-%04d' "$SELFHOST_TEST_COVERAGE_ROOT" "$COVERAGE_INDEX"
    mkdir -m 700 -- "$case_dir"
    invocation=("$SELFHOST_TEST_KCOV" --bash-method=PS4 --bash-dont-parse-binary-dir "--bash-parse-files-in-dir=$(dirname "$HELPER")" "--include-path=$HELPER" "$case_dir" "$TEST_FILE" "$@")
    [[ -z ${SELFHOST_TEST_KCOV_LIBRARY_PATH:-} ]] || coverage_env=("LD_LIBRARY_PATH=$SELFHOST_TEST_KCOV_LIBRARY_PATH")
  fi
  env -i PATH="$WORK/bin:/usr/bin:/bin" HOME="$WORK" SELFHOST_TEST_CHILD=1 \
    SELFHOST_TEST_SOURCE_ROOT="$SOURCE_ROOT" \
    FIX="$FIX" STATE="$STATE" STUB_DOCKER="$STUB_DOCKER" \
    "${coverage_env[@]}" "${invocation[@]}" >"$STATE/output" 2>&1 || rc=$?
  if [[ "$rc" -ne "$want" ]] || [[ -e "$STATE/rejected" ]] \
      || grep -qE '^FAIL:|command not found|unbound variable|syntax error|Traceback|AssertionError' "$STATE/output" \
      || grep -qE 'synthetic-(access|secret|root-password)|objects\.example\.invalid|unknown-mode-sentinel|duplicate-field-sentinel|trailing-sentinel|malformed-id-sentinel|caller-sentinel|synthetic-extra|opaque-stderr-sentinel|volume-owned-private-sentinel|Cannot connect to the Docker daemon|Unrecognized synthetic operation failure' "$STATE/output"; then
    printf 'FAIL: %s (status=%s expected=%s; private output withheld)\n' "$name" "$rc" "$want"
    FAIL=$((FAIL+1))
  else printf 'PASS: %s\n' "$name"; PASS=$((PASS+1)); fi
}
check_diagnostic() {
  local name=$1 expected=$2
  if cmp -s "$STATE/output" <(printf '%s\n' "$expected"); then
    printf 'PASS: %s\n' "$name"; PASS=$((PASS+1))
  else
    printf 'FAIL: %s (fixed diagnostic missing or changed; private output withheld)\n' "$name"
    FAIL=$((FAIL+1))
  fi
}
namespace_freshness_contracts() {
  local phase evidence entry
  for phase in early final; do
    for evidence in empty unrelated near-name unlabeled-unrelated; do
      expect "version-independent freshness $phase accepts $evidence resource control" 0 namespace_fresh_case "$evidence" "$phase"
    done
    for evidence in old-container old-volume fixed-name; do
      expect "version-independent freshness $phase refuses $evidence" 1 namespace_fresh_case "$evidence" "$phase"
      check_diagnostic "version-independent $phase/$evidence emits its existing-installation boundary" \
        'selfhost-storage: existing installation refused'
    done
    for evidence in container-list-fail volume-list-fail container-inspect-fail volume-inspect-fail container-inspect-malformed volume-inspect-malformed; do
      expect "version-independent freshness $phase fails closed on $evidence" 1 namespace_fresh_case "$evidence" "$phase"
      check_diagnostic "version-independent $phase/$evidence emits its unavailable-discovery boundary" \
        'selfhost-storage: installation discovery unavailable'
    done
  done
  for entry in answers interactive; do
    for evidence in unrelated near-name unlabeled-unrelated; do
      expect "real $entry installer admits genuinely unrelated $evidence resources" 0 namespace_installer_case "$evidence" "$entry" accept
    done
    for phase in early final; do
      for evidence in old-container old-volume fixed-name container-list-fail volume-list-fail container-inspect-fail volume-inspect-fail container-inspect-malformed volume-inspect-malformed; do
        expect "real $entry installer $phase/$evidence refusal precedes durable environment and rendering" \
          0 namespace_installer_case "$evidence" "$entry" refuse "$phase"
      done
    done
  done
}
source_volume_freshness_contracts() {
  local project logical evidence entry phase
  local -a projects=(alpha public unrelated near clone renamed numeric)
  # Policy change: a generic Concord persistent key no longer authorizes fresh
  # installation under an unrelated or arbitrary project. Labels cannot prove
  # application identity; refusal conserves ambiguous data without attributing it.
  for project in "${projects[@]}"; do
    expect "source volume freshness accepts $project unrelated logical key" 0 namespace_fresh_case "source-volume-$project-cache-data" early
    for logical in postgres-data redis-data minio-data nats-data; do
      evidence="source-volume-$project-$logical"
      expect "source volume freshness refuses ambiguous retained $project/$logical after down" 1 namespace_fresh_case "$evidence" early
      check_diagnostic "source volume $project/$logical emits existing-installation refusal" 'selfhost-storage: existing installation refused'
    done
    for logical in missing null bool int list object invalid empty; do
      evidence="source-volume-$project-$logical"
      expect "source volume freshness refuses unavailable typed $project/$logical labels" 1 namespace_fresh_case "$evidence" early
      check_diagnostic "source volume $project/$logical emits unavailable-discovery refusal" 'selfhost-storage: installation discovery unavailable'
    done
  done
  for evidence in source-volume-clone-postgres-data source-volume-renamed-redis-data source-volume-numeric-nats-data; do
    expect "final source volume guard refuses arbitrary project $evidence" 1 namespace_fresh_case "$evidence" final
    check_diagnostic "final source volume $evidence emits existing-installation refusal" 'selfhost-storage: existing installation refused'
  done
  for evidence in source-volume-clone-missing source-volume-renamed-object source-volume-numeric-invalid; do
    expect "final source volume guard refuses arbitrary project metadata $evidence" 1 namespace_fresh_case "$evidence" final
    check_diagnostic "final source volume $evidence emits unavailable-discovery refusal" 'selfhost-storage: installation discovery unavailable'
  done
  for entry in answers interactive; do
    for project in "${projects[@]}"; do
      evidence="source-volume-$project-cache-data"
      expect "real $entry installer publishes with conserved nonpersistent $evidence volume" 0 namespace_installer_case "$evidence" "$entry" accept
    done
    for phase in early final; do
      for evidence in source-volume-alpha-postgres-data source-volume-public-redis-data \
          source-volume-unrelated-minio-data source-volume-near-nats-data \
          source-volume-clone-postgres-data source-volume-renamed-redis-data \
          source-volume-numeric-minio-data source-volume-clone-nats-data \
          source-volume-alpha-missing source-volume-public-null source-volume-clone-bool \
          source-volume-unrelated-int source-volume-near-list source-volume-renamed-object \
          source-volume-numeric-invalid source-volume-clone-empty; do
        expect "real $entry installer $phase/$evidence refuses before secrets, renderer and environment publication" \
          0 namespace_installer_case "$evidence" "$entry" refuse "$phase"
      done
    done
  done
  source_volume_logical_name_contracts
}
source_volume_logical_name_contracts() {
  local evidence entry
  # Logical Compose volume keys have their own spelling rules. Dots and
  # uppercase are valid, independent of the lowercase project identity.
  for evidence in source-volume-unrelated-audit.data source-volume-unrelated-Report_Data \
      source-volume-alpha-audit.data source-volume-public-Report_Data \
      source-volume-unrelated-Cache.v2 source-volume-unrelated-Scratch_DATA \
      source-volume-clone-Cache.v2 source-volume-renamed-Scratch_DATA source-volume-numeric-Cache.v2; do
    expect "actual freshness accepts conserved valid logical name $evidence" 0 namespace_fresh_case "$evidence" early
    for entry in answers interactive; do
      expect "real $entry installer publishes with conserved valid logical name $evidence" \
        0 namespace_installer_case "$evidence" "$entry" accept
    done
  done
}
if [[ $LOGICAL_VOLUME_ONLY == true ]]; then
  source_volume_logical_name_contracts
  printf 'passed: %s  failed: %s\n' "$PASS" "$FAIL"
  [[ "$FAIL" -eq 0 ]]
  exit
fi
if [[ $SOURCE_VOLUME_ONLY == true ]]; then
  source_volume_freshness_contracts
  printf 'passed: %s  failed: %s\n' "$PASS" "$FAIL"
  [[ "$FAIL" -eq 0 ]]
  exit
fi
if [[ $# -gt 0 ]]; then
  [[ $# -eq 1 && $1 == --freshness-only ]] || fail 'expected no arguments or --freshness-only'
  namespace_freshness_contracts
  printf 'passed: %s  failed: %s\n' "$PASS" "$FAIL"
  [[ "$FAIL" -eq 0 ]]
  exit
fi
# Gate ALL negative assertions on a working source and historical happy path.
expect 'historical bundled positive control' 0 read_mode historical
[[ "$FAIL" -eq 0 ]] || fail 'historical bundled positive control failed; negative cases were not executed'
for interface in selfhost_storage_validate selfhost_storage_read selfhost_storage_scrub_environment \
  selfhost_storage_project_identity selfhost_storage_resolve selfhost_storage_select_image \
  selfhost_storage_compose selfhost_storage_assert_fresh selfhost_storage_find_minio \
  selfhost_storage_assert_no_running_minio selfhost_storage_stop_stale_minio; do
  # Function availability is only a gate, not behavior evidence.
  env -i PATH="$WORK/bin:/usr/bin:/bin" bash -c 'source "$1"; declare -F "$2" >/dev/null' _ "$HELPER" "$interface" >"$STATE/output" 2>&1 \
    || fail 'required shared helper interface is absent'
done
expect 'bundled refuses any external field without disclosure' 1 read_bad bundled-block
for shape in bundled byo minio r2 b2 literal unquoted metadata-literal; do expect "reader valid $shape ignores caller mode" 0 read_mode "$shape"; done
for shape in no-file no-marker duplicate-mode empty-mode unknown-mode bad-escape unclosed dollar single-quote trailing-data nul cr invalid-utf8; do expect "reader refuses $shape and clears mode" 1 read_bad "$shape"; done
expect 'reader refuses unknown persisted storage field and clears resolution' 1 read_bad unknown-field
check_diagnostic 'unknown persisted storage field emits exact fixed refusal' 'selfhost-storage: storage environment refused'
for field in STORAGE_BACKEND STORAGE_ENDPOINT STORAGE_REGION STORAGE_ACCESS_KEY STORAGE_SECRET_KEY STORAGE_USE_SSL STORAGE_BUCKET; do
  expect "reader requires $field assignment" 1 read_bad "missing-$field"
  expect "reader rejects duplicate $field" 1 read_bad "duplicate-$field"
  [[ "$field" == STORAGE_REGION ]] || expect "reader requires nonempty $field" 1 read_bad "empty-$field"
done
expect 'validation accepts bundled empty block' 0 validate_case bundled
expect 'validation refuses bundled external field' 1 validate_case bundled-field objects.example.invalid
for backend in s3 minio r2 b2; do expect "validation accepts backend $backend" 0 validate_case backend "$backend"; done
for pair in 'mode unknown' 'backend unknown' 'tls TRUE' 'tls 0'; do read -r kind value <<<"$pair"; expect "validation refuses $pair" 1 validate_case "$kind" "$value"; done
for field in access secret bucket backend endpoint tls; do expect "validation requires nonempty $field" 1 validate_case "$field" ''; done
literal=$' -n = $cash "quoted" \\trail '
expect 'opaque access preserves spaces and punctuation' 0 validate_case access "$literal"
expect 'opaque secret preserves spaces and punctuation' 0 validate_case secret "$literal"
expect 'empty region is valid' 0 validate_case region ''
expect 'metadata region allows spaces and punctuation' 0 validate_case region ' literal = $cash "quoted" \trail '
expect 'bucket permits non-strict client-valid uppercase underscore and colon' 0 validate_case bucket 'Concord_media:fixture'
expect 'bucket refuses spaces and punctuation outside client grammar' 1 validate_case bucket ' literal = $cash "quoted" \trail '
for field in region bucket; do
  for control in $'bad\tmetadata' $'bad\rmetadata' $'bad\nmetadata' $'bad\001metadata' $'bad\177metadata'; do expect "metadata $field refuses control bytes" 1 validate_case "$field" "$control"; done
done
for field in access secret; do
  for control in $'bad\rcredential' $'bad\ncredential'; do expect "credential $field refuses line controls" 1 validate_case "$field" "$control"; done
done
for endpoint in objects.example.invalid minio localhost 192.0.2.10 192.0.2.10:1 objects.example.invalid:65535 '[2001:db8::1]' '[2001:db8::1]:443' '[::1]:9000'; do expect 'endpoint accepts supported DNS/IP/port form' 0 validate_case endpoint "$endpoint"; done
for endpoint in 'https://objects.example.invalid' 'user@objects.example.invalid' 'objects.example.invalid/path' 'objects.example.invalid?q=x' 'objects.example.invalid#x' ' objects.example.invalid' 'objects.example.invalid ' $'objects.\texample.invalid' 'objects\example.invalid' 'objects.example.invalid:0' 'objects.example.invalid:65536' 'objects.example.invalid:-1' 'objects.example.invalid:abc' 'objects.example.invalid:' 'bad..example' '-bad.example' 'bad_.example' '999.1.1.1' '[::1' '::1' '[::1]:0' '[]' '[not-ip]' 'objects.éxample.invalid'; do expect 'endpoint refuses malformed or URL-shaped form' 1 validate_case endpoint "$endpoint"; done
for endpoint in minio localhost 127.0.0.1 127.255.255.254:9000 10.0.0.1 10.255.255.254 172.16.0.1 172.31.255.254 192.168.0.1 192.168.255.254 '[::1]' '[fc00::1]' '[fdff:ffff::1]:9000'; do expect 'TLS false accepts exact Go private classification' 0 validate_case private "$endpoint"; done
# The ordinary spellings are positive controls for the same persisted-reader path.
for endpoint in localhost minio; do expect "private DNS persisted positive control preserves $endpoint" 0 private_dns_case "$endpoint"; done
for endpoint in LOCALHOST localhost. MINIO; do expect "private DNS ASCII classification accepts and preserves $endpoint" 0 private_dns_case "$endpoint"; done
expect 'private DNS classification trims only one trailing dot' 1 validate_case private 'localhost..'
for endpoint in objects.example.invalid minio.example.invalid localhost.example.invalid 8.8.8.8 192.0.2.10 198.51.100.10 203.0.113.10 169.254.1.1 0.0.0.0 172.15.255.254 172.32.0.1 192.167.255.254 100.64.0.1 198.18.0.1 '[::]' '[fe80::1]' '[2001:db8::1]' '[fbff::1]' '[fe00::1]'; do expect 'TLS false refuses public/docs/link-local/unspecified/DNS' 1 validate_case private "$endpoint"; done
expect 'TLS false accepts IPv4-mapped RFC1918 destination' 0 validate_case private '[::ffff:10.0.0.1]'
expect 'TLS false accepts IPv4-mapped loopback destination' 0 validate_case private '[::ffff:127.0.0.1]:9000'
expect 'TLS false refuses IPv4-mapped public destination' 1 validate_case private '[::ffff:8.8.8.8]'
expect 'TLS false refuses IPv4-mapped documentation destination' 1 validate_case private '[::ffff:192.0.2.10]'
expect 'endpoint refuses scoped IPv6 zone with TLS true' 1 validate_case endpoint '[fc00::1%synthetic-zone]'
expect 'endpoint refuses scoped private IPv6 zone with TLS false' 1 validate_case private '[fc00::1%synthetic-zone]'
expect 'project identity uses canonical root and scrubbed environment' 0 project_case empty
for evidence in identity-fail identity-malformed identity-invalid; do expect "identity refuses $evidence and clears result" 1 project_case "$evidence"; done
for phase in early final; do
  for evidence in file symlink image receipt deploy-image deploy-receipt running stopped volume working-dir; do expect "fresh $phase refuses $evidence" 1 fresh_case "$evidence" "$phase"; done
  expect "fresh $phase accepts empty exact project" 0 fresh_case empty "$phase"
  expect "fresh $phase accepts unrelated workloads" 0 fresh_case unrelated "$phase"
done
expect 'early unavailable Docker postpones definitive check' 0 fresh_case unavailable early
for evidence in unavailable discovery-fail discovery-malformed inspect-fail inspect-malformed; do
  # Inspect failures need discovered IDs to reach the authoritative metadata boundary.
  expect "final freshness refuses $evidence" 1 fresh_case "$evidence" final
done
namespace_freshness_contracts
source_volume_freshness_contracts
for mode in historical bundled byo; do expect "resolver canonical file/env/root/profile argv for $mode" 0 resolve_case "$mode"; done
for evidence in bad-read identity-fail identity-malformed identity-conflict; do expect "resolver refuses $evidence and clears results" 1 resolve_case "$evidence"; done
expect 'explicit scrub removes controls and preserves unrelated setting' 0 scrub_case
for kind in caller trusted clear; do expect "runner subprocess environment and image $kind" 0 runner_case "$kind"; done
expect 'trusted immutable bracketed IPv6 registry reaches resolved Compose with caller image scrubbed' 0 runner_case trusted-ipv6
expect 'trusted immutable digest-only image reaches resolved Compose with caller image scrubbed' 0 runner_case trusted-digest
expect 'runner propagates Docker status' 43 runner_case docker-status
check_diagnostic 'runner failure emits exact fixed generic diagnostic' 'selfhost-storage: Compose command failed'
# #1899 / PR #3568 regression: status/privacy already pass; actionable guidance must exist.
expect 'daemon failure resolves model and launches exactly once with unchanged status/identity' 43 runner_case daemon-guidance
check_diagnostic 'Docker daemon failure emits exact actionable guidance' 'selfhost-storage: Docker daemon unavailable or inaccessible; check Docker service and socket permissions'
expect 'private guidance adapter preserves the real resolved operation and status' 43 runner_case daemon-control
check_diagnostic 'same guidance oracle recognizes the positive adapter' 'selfhost-storage: Docker daemon unavailable or inaccessible; check Docker service and socket permissions'
expect 'successful real resolved logs preserve stdout without a failure diagnostic' 0 runner_case log-success
expect 'coturn Compose model and operation positive control without ambient selector' 0 ambient_coturn_consumer_case control
expect 'ambient coturn selector cannot alter checked model or executed Compose operation' 0 ambient_coturn_consumer_case alternate
expect 'runner refuses unresolved state before Docker' 1 unresolved_runner
for evidence in minio-empty minio-stopped minio-created minio-unrelated minio-wrong-service; do expect "MinIO discovery excludes $evidence" 0 minio_case find "$evidence"; done
for evidence in minio-running minio-many; do
  expect "MinIO discovery exact IDs $evidence" 0 minio_case find "$evidence"
  expect "BYO stale guard refuses $evidence" 1 minio_case guard "$evidence"
  expect "BYO stop-only recovery verifies $evidence" 0 minio_case stop "$evidence"
done
# PR #3568 / 4162531690: existing running cases above are positive controls.
for evidence in minio-paused minio-restarting minio-restarting-transient; do
  expect "MinIO authoritative discovery includes exact IDs for $evidence" 0 minio_case find "$evidence"
  expect "BYO authoritative stale guard refuses $evidence" 1 minio_case guard "$evidence"
  check_diagnostic "BYO $evidence emits the exact stale refusal" 'selfhost-storage: running bundled MinIO refused'
  expect "BYO exact stop-only recovery verifies $evidence" 0 minio_case stop "$evidence"
done
expect 'authoritative MinIO inspect positive control recognizes running state' 0 minio_metadata_case minio-running running
expect 'authoritative MinIO inspect positive control recognizes coherent created state' 0 minio_metadata_case minio-created stopped
expect 'authoritative MinIO inspect positive control recognizes coherent exited state' 0 minio_metadata_case minio-stopped stopped
expect 'authoritative MinIO inspect treats Restarting true with Running false as active' 0 minio_metadata_case minio-restarting-transient running
for evidence in minio-state-missing minio-state-unknown minio-state-contradictory; do
  expect "authoritative MinIO inspect refuses $evidence without a partial verdict" 1 minio_metadata_case "$evidence"
done
for evidence in minio-empty minio-stopped minio-created minio-unrelated minio-wrong-service; do
  expect "BYO stale guard accepts $evidence" 0 minio_case guard "$evidence"
  expect "BYO recovery preserves $evidence" 0 minio_case stop "$evidence"
done
for evidence in minio-discovery-fail minio-discovery-malformed minio-inspect-fail minio-inspect-malformed minio-missing-label; do
  for operation in find guard stop; do expect "MinIO $operation fails closed on $evidence and clears IDs" 1 minio_case "$operation" "$evidence"; done
done
for evidence in minio-stop-fail minio-verification-fail minio-still-running; do expect "stop recovery refuses $evidence" 1 minio_case stop "$evidence"; done
expect 'bundled guard performs no stale Docker discovery' 0 minio_case bundled-guard empty
expect 'bundled recovery performs no stale Docker discovery' 0 minio_case bundled-stop empty
printf 'passed: %s  failed: %s\n' "$PASS" "$FAIL"
[[ "$FAIL" -eq 0 ]]
