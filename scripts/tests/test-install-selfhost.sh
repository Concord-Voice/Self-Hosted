#!/usr/bin/env bash
# Unit tests for install-selfhost.sh — source the script + PATH-stub externals.
# Convention mirrors scripts/tests/test-concord-dev-helpers.sh.
set -uo pipefail

# Detach from any inherited git context (pre-commit runs hooks with GIT_DIR /
# GIT_WORK_TREE set). Without this, the isolated `git -C <tmp> init` calls below
# would honor the inherited GIT_DIR and re-initialize the REAL repo as bare,
# corrupting core.bare for the whole working tree.
unset $(git rev-parse --local-env-vars)

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
UDP_ONLY=false
UDP_SCOPED_ONLY=false
FEEDBACK_ONLY=false
CONSUMER_ONLY=false
STORAGE_INSTALLER_P2_ONLY=false
STORAGE_ENDPOINT_ONLY=false
SMTP_NONPEER_ONLY=false
DOMAIN_BOUNDARY_ONLY=false
UDP_SOURCE_ROOT="$ROOT"
if [[ ${1:-} == --udp-preflight-only || ${1:-} == --udp-scoped-endpoints-only ]]; then
  [[ $1 != --udp-scoped-endpoints-only ]] || UDP_SCOPED_ONLY=true
  UDP_ONLY=true; shift
  if [[ $# -eq 2 && $1 == --source-root && -n $2 ]]; then UDP_SOURCE_ROOT=$2; shift 2; fi
  [[ $# -eq 0 ]] || { printf 'Unsupported UDP test arguments\n' >&2; exit 2; }
fi
if [[ ${1:-} == --feedback-boundary-only ]]; then
  FEEDBACK_ONLY=true; shift
  if [[ $# -eq 2 && $1 == --source-root && -n $2 ]]; then UDP_SOURCE_ROOT=$2; shift 2; fi
  [[ $# -eq 0 ]] || { printf 'Unsupported feedback test arguments\n' >&2; exit 2; }
fi
if [[ ${1:-} == --consumer-input-boundary-only ]]; then
  FEEDBACK_ONLY=true; CONSUMER_ONLY=true; shift
  if [[ $# -eq 2 && $1 == --source-root && -n $2 ]]; then UDP_SOURCE_ROOT=$2; shift 2; fi
  [[ $# -eq 0 ]] || { printf 'Unsupported consumer-input test arguments\n' >&2; exit 2; }
fi
if [[ ${1:-} == --smtp-nonpeer-boundary-only ]]; then
  FEEDBACK_ONLY=true; SMTP_NONPEER_ONLY=true; shift
  if [[ $# -eq 2 && $1 == --source-root && -n $2 ]]; then UDP_SOURCE_ROOT=$2; shift 2; fi
  [[ $# -eq 0 ]] || { printf 'Unsupported SMTP non-peer test arguments\n' >&2; exit 2; }
fi
if [[ ${1:-} == --domain-boundary-only ]]; then
  FEEDBACK_ONLY=true; DOMAIN_BOUNDARY_ONLY=true; shift
  if [[ $# -eq 2 && $1 == --source-root && -n $2 ]]; then UDP_SOURCE_ROOT=$2; shift 2; fi
  [[ $# -eq 0 ]] || { printf 'Unsupported domain boundary test arguments\n' >&2; exit 2; }
fi
if [[ ${1:-} == --storage-installer-p2-only || ${1:-} == --storage-endpoint-boundary-only ]]; then
  [[ $1 != --storage-endpoint-boundary-only ]] || STORAGE_ENDPOINT_ONLY=true
  FEEDBACK_ONLY=true; STORAGE_INSTALLER_P2_ONLY=true; shift
  if [[ $# -eq 2 && $1 == --source-root && -n $2 ]]; then UDP_SOURCE_ROOT=$2; shift 2; fi
  [[ $# -eq 0 ]] || { printf 'Unsupported storage/installer P2 test arguments\n' >&2; exit 2; }
fi
# Saved-source selection belongs only to this test harness.
SOURCE_TARGET="$UDP_SOURCE_ROOT/infrastructure/deploy/install-selfhost.sh"
# Capture the real executable before PATH gains the Docker fixture. A standalone
# supported Compose CLI may be supplied explicitly with absolute COMPOSE_BIN.
COMPOSE_ORACLE=()
if [[ -n ${COMPOSE_BIN:-} ]]; then
  [[ "$COMPOSE_BIN" == /* && -x "$COMPOSE_BIN" ]] && COMPOSE_ORACLE=("$COMPOSE_BIN")
else
  REAL_DOCKER_BIN="$(type -P docker 2>/dev/null || true)"
  [[ "$REAL_DOCKER_BIN" == /* && -x "$REAL_DOCKER_BIN" ]] && COMPOSE_ORACLE=("$REAL_DOCKER_BIN" compose)
fi
STUB="$(mktemp -d)"
chmod 700 "$STUB"
FIX="$STUB/concordfixture"
STATE="$STUB/state"
mkdir -p "$FIX/infrastructure/deploy/nginx" "$STATE" "$STUB/bin"
cp -- "$SOURCE_TARGET" "$FIX/infrastructure/deploy/install-selfhost.sh"
if [[ -f "$ROOT/infrastructure/deploy/selfhost-storage.sh" ]]; then
  cp -- "$ROOT/infrastructure/deploy/selfhost-storage.sh" "$FIX/infrastructure/deploy/selfhost-storage.sh"
fi
TARGET="$FIX/infrastructure/deploy/install-selfhost.sh"
fixture_repo() {
  local destination=$1 model
  mkdir -p -- "$destination/infrastructure/deploy/nginx"
  cp -- "$SOURCE_TARGET" "$destination/infrastructure/deploy/install-selfhost.sh"
  cp -- "$ROOT/infrastructure/deploy/selfhost-storage.sh" "$destination/infrastructure/deploy/selfhost-storage.sh"
  cp -- "$ROOT/infrastructure/deploy/nginx/concordvoice.conf" "$destination/infrastructure/deploy/nginx/concordvoice.conf"
  for model in docker-compose.yml docker-compose.production.yml docker-compose.selfhost.yml docker-compose.byo-s3.yml; do
    cp -- "$ROOT/$model" "$destination/$model"
  done
  cp -- "$ROOT/.gitignore" "$destination/.gitignore"
}
printf 'server_name concordvoice.chat;\n' >"$FIX/infrastructure/deploy/nginx/concordvoice.conf"
for model in docker-compose.yml docker-compose.production.yml docker-compose.selfhost.yml docker-compose.byo-s3.yml; do
  cp -- "$ROOT/$model" "$FIX/$model"
done
printf '%s' empty >"$STATE/scenario"
: >"$STATE/calls.jsonl"
export FIX STATE
# Every Docker call, including command docker, resolves to this fail-closed fixture.
cat >"$STUB/bin/docker" <<'DOCKERPY'
#!/usr/bin/python3
import json, os, re, sys
from pathlib import Path
s = Path(os.environ['STATE']); root = os.environ['FIX']; a = sys.argv[1:]
controls = '''SELFHOST_STORAGE_MODE STORAGE_BACKEND STORAGE_ENDPOINT STORAGE_REGION STORAGE_ACCESS_KEY STORAGE_SECRET_KEY STORAGE_USE_SSL STORAGE_BUCKET MINIO_ENDPOINT MINIO_ACCESS_KEY MINIO_SECRET_KEY MINIO_USE_SSL MINIO_BUCKET MINIO_ROOT_USER MINIO_ROOT_PASSWORD COMPOSE_FILE COMPOSE_PROFILES COMPOSE_PROJECT_NAME COMPOSE_ENV_FILES COMPOSE_DISABLE_ENV_FILE INSTANCE_TYPE ACTIVITY_HISTORY_CLUSTER_ENABLED CONTROL_PLANE_REPLICA_COUNT ACTIVITY_HISTORY_OPERATOR_NAME ACTIVITY_HISTORY_PRIVACY_POLICY_URL ATTACHMENT_WRITE_BACKEND CONTROL_PLANE_IMAGE COMPOSE_OVERRIDE'''.split()
with (s/'calls.jsonl').open('a') as f: f.write(json.dumps({'argv':a,'cwd':os.getcwd(),'env':{k:os.environ[k] for k in controls if k in os.environ}})+'\n')
scenario = (s/'scenario').read_text()
def reject():
    (s/'rejected').touch(); print('stub: unrecognized Docker request', file=sys.stderr); sys.exit(97)
def error():
    (s/'error-reached').touch()
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
if not a: reject()
if scenario == 'unavailable': sys.exit(127)
if a in (['compose','version'], ['compose','version','--short']):
    print('2.29.1'); sys.exit(0)
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
        print(json.dumps({'name':name,'services':{'control-plane':{'environment':{'STORAGE_SECRET_KEY':'-'.join(('synthetic','secret','render','sentinel'))}}}})); sys.exit(0)
    if tail == ['logs','--tail','7','control-plane']:
        sys.exit(43 if scenario=='runner-fail' else 0)
    reject()
if a[0] == 'version' and len(a)==3 and a[1]=='--format': print('29.8.2'); sys.exit(0)
if a == ['info'] or (a[0]=='info' and len(a)==3 and a[1]=='--format'): sys.exit(0)
kind = a[0]; rest = a[1:]
if kind == 'container' and rest and rest[0] in ('ls','inspect'): kind = 'ps' if rest[0]=='ls' else 'inspect'; rest=rest[1:]
if kind == 'volume' and rest and rest[0] in ('ls','inspect'): kind='volume-'+rest[0]; rest=rest[1:]
if kind in ('ps','volume-ls'):
    opts=options(rest); filters=opts.get('--filter',[])+opts.get('-f',[])
    # Host-wide freshness discovery has two additional exact read-only forms.
    # They enumerate fixture objects without silently accepting arbitrary new
    # Docker options or broadening the legacy filtered discovery contract.
    hostwide_containers = kind=='ps' and rest==['--all','--quiet','--no-trunc']
    hostwide_volumes = kind=='volume-ls' and rest==['--quiet','--filter','label=com.docker.compose.project']
    project='label=com.docker.compose.project=concordfixture'
    working='label=com.docker.compose.project.working_dir='+root
    service='label=com.docker.compose.service=minio'
    allowed={project,working,service,'status=running'}
    if not (hostwide_containers or hostwide_volumes):
        if not filters or any(f not in allowed for f in filters): reject()
        if kind=='volume-ls' and filters != [project]: reject()
        if kind=='ps' and project not in filters and working not in filters: reject()
    if scenario in ('discovery-fail','minio-discovery-fail') or (scenario=='minio-verification-fail' and (s/'stopped').exists()): error()
    if scenario in ('discovery-malformed','minio-discovery-malformed'): print('malformed-id-sentinel'); sys.exit(0)
    vals=[]
    if kind=='volume-ls' and scenario=='volume': vals=['concordfixture_media']
    if kind=='ps':
        if hostwide_containers:
            if scenario in ('running','stopped','unrelated','inspect-fail','inspect-malformed'): vals=['a'*64]
            if scenario=='working-dir': vals=['c'*64]
        if scenario in ('running','unrelated','inspect-fail','inspect-malformed') and project in filters: vals=['aaaaaaaaaaaa']
        if scenario=='stopped' and project in filters and ('--all' in opts or '-a' in opts): vals=['aaaaaaaaaaaa']
        if scenario=='working-dir' and working in filters: vals=['cccccccccccc']
        if scenario.startswith('minio-') and service in filters:
            vals=['aaaaaaaaaaaa'] if scenario!='minio-empty' else []
            if scenario=='minio-many': vals+=['bbbbbbbbbbbb']
            if scenario=='minio-stopped' or ((s/'stopped').exists() and scenario!='minio-still-running'): vals=[]
    output_form(opts, vals); sys.exit(0)
if kind in ('inspect','volume-inspect'):
    fmt=None; ids=[]; i=0
    while i<len(rest):
        if rest[i] in ('--format','-f') and i+1<len(rest): fmt=rest[i+1]; i+=2
        elif rest[i]=='--type' and i+1<len(rest) and rest[i+1] in ('container','volume'): i+=2
        elif re.fullmatch(r'[a-c]{12}|[a-c]{64}|concordfixture_media',rest[i]): ids.append(rest[i]); i+=1
        else: reject()
    if not ids: reject()
    hostwide_template='{{json .Name}}|{{json .Config.Labels}}|{{json .State}}'
    if fmt==hostwide_template and (kind!='inspect' or len(ids)!=1 or
            rest!=['--type','container','--format',hostwide_template,ids[0]]): reject()
    if scenario in ('inspect-fail','minio-inspect-fail') or (scenario=='minio-verification-fail' and (s/'stopped').exists()): error()
    if scenario in ('inspect-malformed','minio-inspect-malformed'): print('{synthetic-secret-render-sentinel'); sys.exit(0)
    models=[]
    for ident in ids:
        project='unrelated' if scenario in ('working-dir','unrelated','minio-unrelated') else 'concordfixture'
        service='redis' if scenario=='minio-wrong-service' else 'minio'
        running = scenario not in ('stopped','minio-stopped') and not (s/'stopped').exists()
        if scenario=='minio-still-running': running=True
        labels={'com.docker.compose.project':project,'com.docker.compose.service':service,'com.docker.compose.project.working_dir':root if scenario=='working-dir' else '/synthetic/other-root'}
        if scenario=='minio-missing-label': labels.pop('com.docker.compose.project')
        models.append({'Id':ident,'Name':'/synthetic-container','Config':{'Labels':labels,'Env':['STORAGE_SECRET_KEY=synthetic-secret-render-sentinel']},'State':{'Running':running,'Status':'running' if running else 'exited'},'Labels':labels})
    if not fmt: print(json.dumps(models)); sys.exit(0)
    if fmt=='{{json .}}': print('\n'.join(json.dumps(m) for m in models)); sys.exit(0)
    # Bounded authoritative metadata templates; no Config.Env dumps permitted.
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
            elif token=='{{json .Name}}' and fmt==hostwide_template: value=json.dumps(m['Name'])
            else: reject()
            result=result.replace(token,value)
        print(result)
    sys.exit(0)
if kind=='stop':
    if not rest or any(i not in ('aaaaaaaaaaaa','bbbbbbbbbbbb') for i in rest): reject()
    if scenario=='minio-stop-fail': error()
    (s/'stopped').touch(); sys.exit(0)
reject()
DOCKERPY
chmod 700 "$STUB/bin/docker"
export PATH="$STUB/bin:$PATH"
docker() { command docker "$@"; }
export -f docker

trap 'rm -rf "$STUB" 2>/dev/null || true' EXIT
fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; fails=$(( fails + 1 )); }

# #1899 mutations caught: permissive answer execution/duplicates; swallowed reads;
# review before validation; inferred/fallback storage; masked captures; overwriting
# publication. Negative cells require their own successful, otherwise equal control.
reset_task3() {
  rm -f -- "$FIX/.env" "$FIX/.env.activity-history-image.local" "$FIX/.env.activity-history-receipt.local"
  rm -f -- "$STATE/rejected" "$STATE/stopped" "$STATE/rendered" "$STATE/tls" "$STATE/error-reached" "$STATE/publication-reached" "$STATE/staging-reached" "$STATE/preflight-reached" "$STATE/review-reached" "$STATE/terminal-result"
  rm -f -- "$STATE/writer-phase-output" "$STATE/writer-phase-reached" "$STATE/render-attempts" "$STATE/summary-reached" "$STATE/render-before-publication"
  rm -f -- "$FIX"/.env.new.*
  : >"$STATE/calls.jsonl"; : >"$STATE/events"; : >"$STATE/captures"
  printf '%s' empty >"$STATE/scenario"
}
contract() {
  local label=$1 rc=0; shift
  ( "$@" ) >"$STATE/output" 2>&1 || rc=$?
  if [[ $rc -eq 0 && ! -e "$STATE/rejected" ]] \
    && ! grep -qE 'command not found|unbound variable|syntax error|Traceback|AssertionError' "$STATE/output"; then
    LAST_CONTRACT_OK=yes; pass "$label"
  elif grep -qFx 'TASK1_ORACLE: generated coturn selector is missing' "$STATE/output"; then
    LAST_CONTRACT_OK=no; fail "$label (generated coturn selector is missing)"
  elif grep -qFx 'ENDPOINT_ORACLE: shared validator accepted refused endpoint' "$STATE/output"; then
    LAST_CONTRACT_OK=no; fail "$label (shared validator accepted refused endpoint)"
  elif grep -qFx 'ENDPOINT_ORACLE: refused endpoint was durably published' "$STATE/output"; then
    LAST_CONTRACT_OK=no; fail "$label (refused endpoint was durably published)"
  elif grep -qFx 'SMTP_NONPEER_ORACLE: shared validator accepted a non-peer SMTP literal' "$STATE/output"; then
    LAST_CONTRACT_OK=no; fail "$label (shared validator accepted a non-peer SMTP literal)"
  elif grep -qFx 'SMTP_NONPEER_ORACLE: non-peer SMTP literal was durably published' "$STATE/output"; then
    LAST_CONTRACT_OK=no; fail "$label (non-peer SMTP literal was durably published)"
  elif grep -qFx 'DOMAIN_LENGTH_ORACLE: validator accepted an unsupported generated DNS name' "$STATE/output"; then
    LAST_CONTRACT_OK=no; fail "$label (validator accepted an unsupported generated DNS name)"
  elif grep -qFx 'DOMAIN_LENGTH_ORACLE: unsupported domain was durably published' "$STATE/output"; then
    LAST_CONTRACT_OK=no; fail "$label (unsupported domain was durably published)"
  elif grep -qFx 'FEEDBACK_REPO_ORACLE: URL syntax was durably published as a repository slug' "$STATE/output"; then
    LAST_CONTRACT_OK=no; fail "$label (URL syntax was durably published as a repository slug)"
  elif grep -qFx 'FEEDBACK_DOT_SEGMENT_ORACLE: URL path-normalizing segment was durably published as a repository slug' "$STATE/output"; then
    LAST_CONTRACT_OK=no; fail "$label (URL path-normalizing segment was durably published as a repository slug)"
  else LAST_CONTRACT_OK=no; fail "$label (private output withheld)"; fi
}
config_fixture() {
  CFG_DOMAIN=example.com; CFG_PUBLIC_IP=203.0.113.10; CFG_TURN_REALM=example.com
  CFG_TLS_MODE=letsencrypt; CFG_SMTP_HOST=smtp.example.invalid; CFG_SMTP_PORT=587
  CFG_SMTP_USER=$' -n = $cash "smtp-user" \\trail '; CFG_SMTP_PASS=$' -n = $cash "smtp-pass" \\trail '
  CFG_FEEDBACK_PAT=$' -n = $cash "feedback" \\trail '
  CFG_SMTP_FROM=operator@example.invalid; CFG_TRUSTED_CIDRS=172.16.0.0/12
  CFG_FEEDBACK_ENABLED=yes; CFG_FEEDBACK_REPO=synthetic/feedback
  CFG_ACTIVITY_HISTORY_OPERATOR_NAME=$' -n = $cash "operator" \\trail '
  CFG_ACTIVITY_HISTORY_PRIVACY_POLICY_URL=https://privacy.example.invalid/policy
  CFG_STORAGE_MODE=${1:-byo-s3}
  CFG_STORAGE_BACKEND=s3; CFG_STORAGE_ENDPOINT=objects.example.invalid; CFG_STORAGE_REGION=''
  CFG_STORAGE_ACCESS_KEY=$' -n = $cash "access" \\trail '
  CFG_STORAGE_SECRET_KEY=$' -n = $cash "secret" \\trail '
  CFG_STORAGE_USE_SSL=true; CFG_STORAGE_BUCKET=concord-media
  if [[ ${CFG_STORAGE_MODE:-} == bundled ]]; then
    unset CFG_STORAGE_BACKEND CFG_STORAGE_ENDPOINT CFG_STORAGE_REGION CFG_STORAGE_ACCESS_KEY
    unset CFG_STORAGE_SECRET_KEY CFG_STORAGE_USE_SSL CFG_STORAGE_BUCKET
  fi
  ENV_OUT="$FIX/.env"; NGINX_OUT="$STATE/rendered"
}
answer_fixture() {
  printf '%s\n' 'CFG_DOMAIN=example.com' 'CFG_PUBLIC_IP=203.0.113.10' 'CFG_SMTP_HOST=smtp.example.invalid' \
    'CFG_SMTP_PORT=587' 'CFG_SMTP_FROM=operator@example.invalid' 'CFG_FEEDBACK_ENABLED=no' >"$STATE/answers"
  if [[ ${1:-byo-s3} == byo-s3 ]]; then
    printf '%s\n' 'CFG_STORAGE_MODE=byo-s3' 'CFG_STORAGE_ENDPOINT=objects.example.invalid' \
      'CFG_STORAGE_ACCESS_KEY= -n = $cash "access" \trail ' \
      'CFG_STORAGE_SECRET_KEY= -n = $cash "secret" \trail ' >>"$STATE/answers"
  fi
}
answer_success() {
  source "$TARGET"
  load_answers "$STATE/answers" || return 1
  [[ ${CFG_STORAGE_MODE:-} == ${1:-byo-s3} ]] || return 1
  if [[ ${CFG_STORAGE_MODE:-} == byo-s3 ]]; then
    [[ ${CFG_STORAGE_BACKEND:-} == ${ANSWER_BACKEND:-s3} && ${CFG_STORAGE_USE_SSL:-} == true && ${CFG_STORAGE_BUCKET:-} == concord-media
       && -z ${CFG_STORAGE_REGION:-} && ${CFG_STORAGE_ACCESS_KEY:-} == $' -n = $cash "access" \\trail '
       && ${CFG_STORAGE_SECRET_KEY:-} == $' -n = $cash "secret" \\trail ' ]] || return 1
  fi
  validate_collected_config
}
answer_refusal() {
  source "$TARGET"
  local status=0
  load_answers "$STATE/answers" >"$STATE/refusal" 2>&1 || status=$?
  [[ $status -eq 1 ]] || return 1
  # Diagnostics are fixed error class/line number only. Never key or input data.
  [[ -s "$STATE/refusal" ]] && ! grep -qE 'synthetic-rejected|CFG_[A-Z_]+|command not found|unbound variable' "$STATE/refusal"
}
writer_run() {
  source "$TARGET"
  # contract runs this fixture in a subshell. Keep every real stdlib validator
  # while avoiding unrelated site initialization for its short Python calls.
  python3() { command python3 -S "$@"; }
  config_fixture "${WRITER_MODE:-byo-s3}"
  CFG_TLS_MODE=${WRITER_TLS_MODE:-letsencrypt}
  [[ ${WRITER_FEEDBACK:-yes} == yes ]] || CFG_FEEDBACK_ENABLED=no
  # Each invocation writes its ordinal to a file: command substitutions run in
  # subshells, so shell-only counters would silently reset and mask later cells.
  gen_secret() {
    local n=1 line
    while IFS= read -r line; do n=$((n+1)); done <"$STATE/gen"
    printf '%s\n' "$n" >>"$STATE/gen"
    [[ ${FAIL_GENERATOR:-none} != "secret:$n" ]] || return 17
    printf '%s' 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
  }
  gen_hex_key() {
    local n=1 line
    while IFS= read -r line; do n=$((n+1)); done <"$STATE/hex"
    printf '%s\n' "$n" >>"$STATE/hex"
    [[ ${FAIL_GENERATOR:-none} != "hex:$n" ]] || return 17
    printf '%s' 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'
  }
  # Rename only trusted function source, keeping the production encoder body.
  source <(declare -f compose_dotenv_quote | sed '1s/compose_dotenv_quote/fixture_real_quote/')
  compose_dotenv_quote() {
    local n=1 line
    while IFS= read -r line; do n=$((n+1)); done <"$STATE/encode"
    printf '%s\n' "$n" >>"$STATE/encode"
    [[ ${FAIL_ENCODER:-0} != "$n" ]] || return 18
    fixture_real_quote "$@"
  }
  : >"$STATE/gen"; : >"$STATE/hex"; : >"$STATE/encode"
  if [[ ${WRITER_VIA_MAIN:-no} == yes ]]; then
    # Preserve real input, common validation, freshness, review, writer and main.
    # Only host preflight and later privileged effects are replaced.
    # Review metadata is allowed before generation. Capture only the real
    # writer's diagnostics separately, forwarding the same bytes and status.
    source <(declare -f write_env_file | sed '1s/write_env_file/fixture_phase_real_writer/')
    write_env_file() {
      local writer_status=0
      : >"$STATE/writer-phase-reached"
      fixture_phase_real_writer "$@" >"$STATE/writer-phase-output" 2>&1 || writer_status=$?
      cat -- "$STATE/writer-phase-output"
      return "$writer_status"
    }
    run_preflight() { return 0; }
    render_nginx() { : >"$STATE/rendered"; }
    # Observe the real publication seam without preventing or fabricating it.
    ln() {
      if [[ -e "$STATE/rendered" ]]; then printf yes; else printf no; fi >"$STATE/render-before-publication"
      command ln "$@"
    }
    provision_tls() { : >"$STATE/tls"; }
    source <(declare -f print_summary | sed '1s/print_summary/fixture_phase_real_summary/')
    print_summary() { : >"$STATE/summary-reached"; fixture_phase_real_summary; }
    : >"$STATE/writer-answers"
    local key
    for key in DOMAIN PUBLIC_IP TURN_REALM TLS_MODE SMTP_HOST SMTP_PORT SMTP_USER SMTP_PASS SMTP_FROM \
      TRUSTED_CIDRS FEEDBACK_ENABLED FEEDBACK_REPO FEEDBACK_PAT ACTIVITY_HISTORY_OPERATOR_NAME \
      ACTIVITY_HISTORY_PRIVACY_POLICY_URL STORAGE_MODE STORAGE_BACKEND STORAGE_ENDPOINT STORAGE_REGION \
      STORAGE_ACCESS_KEY STORAGE_SECRET_KEY STORAGE_USE_SSL STORAGE_BUCKET; do
      key=CFG_$key
      [[ ! -v $key ]] || printf '%s=%s\n' "$key" "${!key}" >>"$STATE/writer-answers"
    done
    main --env-file "$STATE/writer-answers"
  else write_env_file; fi
}
# Read the real emitted dotenv as data; never source credentials as shell code.
assert_ws_event_budget_defaults() {
  python3 - "$1" <<'WS_BUDGET_DEFAULTS'
import pathlib, sys
expected = {'WS_EVENT_BUDGET_ENABLED': 'true', 'WS_EVENT_BUDGET_SCALE_PERCENT': '100'}
observed = {key: [] for key in expected}
for line in pathlib.Path(sys.argv[1]).read_text().splitlines():
    if not line or line.startswith('#'):
        continue
    key, separator, value = line.partition('=')
    if separator and key in observed:
        observed[key].append(value)
for key, value in expected.items():
    if observed[key] != [value]:
        raise SystemExit('WS budget writer oracle: missing, duplicate or incorrect ' + key)
WS_BUDGET_DEFAULTS
}
writer_success() {
  writer_run || return 1
  assert_ws_event_budget_defaults "$FIX/.env" || return 1
  [[ -f "$FIX/.env" && $(stat -c '%a' "$FIX/.env") == 600 ]] || return 1
  if [[ ${WRITER_VIA_MAIN:-no} == yes ]]; then [[ -e "$STATE/rendered" && -e "$STATE/tls" && -e "$STATE/summary-reached" ]] || return 1; fi
  ! compgen -G "$FIX/.env.new.*" >/dev/null || return 1
python3 - "$FIX/.env" "${WRITER_MODE:-byo-s3}" "${WRITER_FEEDBACK:-yes}" "$STATE/calls.jsonl" <<'ENVASSERT'
import json, pathlib, sys
calls = [json.loads(line) for line in pathlib.Path(sys.argv[4]).read_text().splitlines()]
assert not any(token in {'up', 'start', 'restart', 'run', 'create'} for call in calls for token in call['argv']), 'fresh env writer must not start services'
lines=pathlib.Path(sys.argv[1]).read_text().splitlines(); fields={}
for line in lines:
    if line and not line.startswith('#'):
        k,v=line.split('=',1); assert k not in fields; fields[k]=v
assert fields['SELFHOST_STORAGE_MODE']==sys.argv[2]
def decode(v):
    if not v.startswith('"'): return v
    assert v.endswith('"'); out=''; i=1
    while i<len(v)-1:
        if v[i]=='\\': assert v[i+1] in '\\"'; out+=v[i+1]; i+=2
        elif v[i]=='$': assert v[i:i+2]=='$$'; out+='$'; i+=2
        else: out+=v[i]; i+=1
    return out
selectors=[(k,decode(v)) for k,v in fields.items() if k=='COTURN_CERTS_DIR']
if selectors != [('COTURN_CERTS_DIR','/opt/concord/certs/coturn')]:
    print('TASK1_ORACLE: generated coturn selector is missing', file=sys.stderr)
    sys.exit(1)
keys={'STORAGE_BACKEND','STORAGE_ENDPOINT','STORAGE_REGION','STORAGE_ACCESS_KEY','STORAGE_SECRET_KEY','STORAGE_USE_SSL','STORAGE_BUCKET'}
if sys.argv[2]=='bundled': assert not keys.intersection(fields)
else:
    assert keys.issubset(fields)
    assert fields['STORAGE_REGION'] in ('','""')
    assert fields['MINIO_ROOT_USER'] != fields['STORAGE_ACCESS_KEY']
    assert fields['MINIO_ROOT_PASSWORD'] != fields['STORAGE_SECRET_KEY']
    assert decode(fields['STORAGE_ACCESS_KEY'])==' -n = $cash "access" \\trail '
    assert decode(fields['STORAGE_SECRET_KEY'])==' -n = $cash "secret" \\trail '
    for key, value in (('SMTP_USERNAME',' -n = $cash "smtp-user" \\trail '),
                       ('SMTP_PASSWORD',' -n = $cash "smtp-pass" \\trail '),
                       ('ACTIVITY_HISTORY_OPERATOR_NAME',' -n = $cash "operator" \\trail ')):
        assert decode(fields[key])==value
    if sys.argv[3]=='yes': assert decode(fields['FEEDBACK_PAT'])==' -n = $cash "feedback" \\trail '

for k in ('POSTGRES_PASSWORD','REDIS_PASSWORD','JWT_SECRET','TURN_SECRET','MINIO_ROOT_PASSWORD','MFA_ENCRYPTION_KEY'):
    v=fields[k].strip('"'); assert len(v)==64 and all(c in 'abcdef0123456789' for c in v)
ENVASSERT
}
writer_failure() {
  local status=0
  writer_run >"$STATE/writer-output" 2>&1 || status=$?
  [[ $status -eq 1 && ! -e "$FIX/.env" && ! -L "$FIX/.env" ]] || return 1
  ! compgen -G "$FIX/.env.new.*" >/dev/null || return 1
  [[ ! -e "$STATE/tls" && ! -e "$STATE/summary-reached" && ! -e "$STATE/render-before-publication" ]] || return 1
  # The accepted pre-publication renderer may have completed. Its task-owned
  # fixture output is harmless; capture failure still cannot publish or run TLS.
  if [[ ${WRITER_VIA_MAIN:-no} == yes ]]; then
    [[ ! -e "$STATE/rendered" || ! -s "$STATE/rendered" ]] || return 1
  else [[ ! -e "$STATE/rendered" ]] || return 1; fi
  # Main may print validated endpoint metadata in the required pre-writer review.
  # Credentials and generated fixture values remain forbidden across every phase.
  ! grep -qE 'synthetic-|cash|aaaaaaaaaaaaaaaa|bbbbbbbbbbbbbbbb' "$STATE/writer-output" || return 1
  if [[ ${WRITER_VIA_MAIN:-no} == yes ]]; then
    [[ -e "$STATE/writer-phase-reached" && -f "$STATE/writer-phase-output" ]] || return 1
    ! grep -qE 'synthetic-|cash|aaaaaaaaaaaaaaaa|bbbbbbbbbbbbbbbb|objects[.]example[.]invalid' \
      "$STATE/writer-phase-output" || return 1
  fi
  if [[ ${FAIL_GENERATOR:-none} == secret:* ]]; then [[ $(wc -l <"$STATE/gen") == "${FAIL_GENERATOR#*:}" ]]
  elif [[ ${FAIL_GENERATOR:-none} == hex:* ]]; then [[ $(wc -l <"$STATE/hex") == "${FAIL_GENERATOR#*:}" ]]
  else [[ $(wc -l <"$STATE/encode") == "$FAIL_ENCODER" ]]; fi
}
main_render_before_publication() {
  writer_success || return 1
  [[ $(cat "$STATE/render-before-publication") == yes ]]
}
entry_trace() {
  source "$TARGET"; config_fixture
  # Accepted helper is fixture support even before installer wiring exists. It
  # does not replace the production caller: order tests still require main to
  # invoke the shared function. Missing Task 3 review is a named behavior RED.
  if ! declare -F selfhost_storage_assert_fresh >/dev/null; then
    source "$FIX/infrastructure/deploy/selfhost-storage.sh" || return 1
  fi
  if ! declare -F print_storage_review >/dev/null; then
    printf '%s\n' 'installer behavior absent: validated storage review' >&2
    return 1
  fi
  source <(declare -f validate_collected_config | sed '1s/validate_collected_config/fixture_real_validate/')
  source <(declare -f selfhost_storage_assert_fresh | sed '1s/selfhost_storage_assert_fresh/fixture_real_fresh/')
  validate_collected_config() { printf 'validate\n' >>"$STATE/events"; fixture_real_validate; }
  selfhost_storage_assert_fresh() {
    printf 'fresh:%s\n' "$2" >>"$STATE/events"
    fixture_real_fresh "$@" || return $?
    [[ ${FINAL_DISCOVERY_FAILURE:-0} != 1 || $2 != early ]] || printf discovery-fail >"$STATE/scenario"
  }
  run_preflight() { printf 'preflight\n' >>"$STATE/events"; }
  source <(declare -f print_storage_review | sed '1s/print_storage_review/fixture_real_review/')
  print_storage_review() { printf 'review\n' >>"$STATE/events"; fixture_real_review; }
  source <(declare -f write_env_file | sed '1s/write_env_file/fixture_real_writer/')
  write_env_file() { printf 'writer\n' >>"$STATE/events"; fixture_real_writer; }
  gen_secret() { printf '%s' aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; }
  gen_hex_key() { printf '%s' bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb; }
  render_nginx() {
    printf 'render\n' >>"$STATE/render-attempts"
    [[ ${NGINX_RENDER_FAILURE:-no} != yes ]] || return 37
    : >"$STATE/rendered"
  }
  provision_tls() { : >"$STATE/tls"; }
  if [[ ${ENTRY_PATH:-answers} == interactive ]]; then
    source <(declare -f collect_config | sed '1s/collect_config/fixture_real_collect/')
    collect_config() { printf 'collect\n' >>"$STATE/events"; fixture_real_collect; }
    detect_public_ip() { printf '%s' 203.0.113.10; }
    main <"$STATE/stdin"
  else
    source <(declare -f load_answers | sed '1s/load_answers/fixture_real_load/')
    load_answers() { printf 'load\n' >>"$STATE/events"; fixture_real_load "$@"; }
    main --env-file "$STATE/answers"
  fi
}
entry_success() {
  entry_trace >"$STATE/entry-output" 2>&1 || return 1
  python3 - "$STATE/events" "${ENTRY_PATH:-answers}" <<'ORDER'
import pathlib, sys
v=pathlib.Path(sys.argv[1]).read_text().splitlines(); collect='load' if sys.argv[2]=='answers' else 'collect'
want=['fresh:early',collect,'validate','preflight','fresh:final','review','writer']
i=-1
for x in want: i=v.index(x,i+1)
assert v.count('review')==1 and v.count('writer')==1
ORDER
  [[ -e "$FIX/.env" && -e "$STATE/rendered" && -e "$STATE/tls" ]] || return 1
  ! grep -qE 'cash|synthetic-access|synthetic-secret|"access"|"secret"' "$STATE/entry-output" || return 1
  grep -qF 'provided (hidden)' "$STATE/entry-output" || return 1
  grep -qF 'External storage has not been checked.' "$STATE/entry-output"
}
wizard_input() {
  printf '%s\n' example.com letsencrypt example.com 203.0.113.10 byo-s3 \
    objects.example.invalid true concord-media '' synthetic-access synthetic-secret \
    smtp.example.invalid 587 synthetic-user synthetic-smtp-password operator@example.invalid \
    172.16.0.0/12 '' '' no >"$STATE/stdin"
}
wizard_capture() {
  source "$TARGET"; config_fixture bundled
  detect_public_ip() { printf '%s' 203.0.113.10; }
  # Observe actual builtin read flags and literal values at the Bash boundary.
  read() {
    local silent=no arg
    for arg in "$@"; do [[ $arg != -*s* ]] || silent=yes; done
    builtin read "$@" || return $?
    if [[ ${v:-} == synthetic-access || ${v:-} == synthetic-secret ]]; then
      printf '%s:%s\n' "$v" "$silent" >>"$STATE/reads"
    fi
  }
  : >"$STATE/reads"
  collect_config <"$STATE/stdin" || return 1
  [[ ${CFG_STORAGE_MODE:-} == byo-s3 && ${CFG_STORAGE_BACKEND:-} == s3
     && ${CFG_STORAGE_ACCESS_KEY:-} == synthetic-access && ${CFG_STORAGE_SECRET_KEY:-} == synthetic-secret ]] || return 1
  [[ $(cat "$STATE/reads") == $'synthetic-access:yes\nsynthetic-secret:yes' ]]
}
read_contract() {
  source "$TARGET"
  local got status=0
  got=$(ask 'fixture prompt' default </dev/null) || status=$?
  [[ $status -ne 0 && -z $got ]] || return 1
  status=0; got=$(ask_secret 'fixture hidden' </dev/null) || status=$?
  [[ $status -ne 0 && -z $got ]] || return 1
  got=$(ask 'fixture prompt' < <(printf '%s\n' '-n')) || return 1
  [[ $got == -n ]] || return 1
  got=$(ask_secret 'fixture hidden' < <(printf '%s\n' '-n')) || return 1
  [[ $got == -n ]]
}
fresh_entry_refusal() {
  local status=0
  entry_trace >"$STATE/entry-output" 2>&1 || status=$?
  [[ $status -ne 0 && ! -e "$STATE/rendered" && ! -e "$STATE/tls" ]] || return 1
  ! grep -qE '^(load|collect|review|writer)$' "$STATE/events" || return 1
  ! grep -qE 'synthetic-|cash' "$STATE/entry-output"
}
publication_refusal() {
  source "$TARGET"; config_fixture
  gen_secret() { printf '%s' aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; }
  gen_hex_key() { printf '%s' bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb; }
  local status=0
  write_env_file >"$STATE/writer-output" 2>&1 || status=$?
  [[ $status -ne 0 ]] || return 1
  if [[ ${DESTINATION_KIND:-file} == symlink ]]; then [[ -L "$FIX/.env" && ! -e "$FIX/.env" ]]
  elif [[ ${DESTINATION_KIND:-file} == directory ]]; then
    [[ -d "$FIX/.env" && ! -L "$FIX/.env" ]] && cmp -s "$STATE/original" "$FIX/.env/retained"
  else cmp -s "$STATE/original" "$FIX/.env"; fi
}
# PR #3568 / #1899: inspect the validation property without executing bad input.
validation_ast_policy() {
  python3 - "$TARGET" "$1" <<'ASTPOLICY'
import ast, pathlib, re, sys
text = pathlib.Path(sys.argv[1]).read_text()
bodies = {}
for name in ('load_answers', 'validate_collected_config', 'compose_dotenv_quote'):
    function = re.search(r'(?ms)^' + name + r'\(\)\s*\{(.*?)^\}', text)
    if function is None: raise SystemExit('validation AST fixture: function unavailable')
    literals = re.findall(r"(?s)python3\s+-c\s+'(.*?)\n'", function.group(1))
    if len(literals) != 1: raise SystemExit('validation AST fixture: expected one Python literal')
    bodies[name] = ast.parse(literals[0])
def safe(tree):
    return not any(isinstance(node, ast.Assert) for node in ast.walk(tree))
if sys.argv[2] == 'control':
    # Both are benign syntax-only controls. Neither Python body is executed.
    explicit = ast.parse('if not True:\n    raise ValueError()')
    removable = ast.parse('assert True')
    raise SystemExit(0 if safe(explicit) and not safe(removable) else 1)
raise SystemExit(0 if safe(bodies[sys.argv[2]]) else 1)
ASTPOLICY
}
optimized_valid_input() {
  source "$TARGET"
  export PYTHONOPTIMIZE=1
  load_answers "$STATE/answers" || return 1
  validate_collected_config || return 1
  local quoted
  quoted=$(compose_dotenv_quote 'ordinary fixture text') || return 1
  [[ $quoted == '"ordinary fixture text"' ]]
}
staging_namespace_control() {
  # Read-only namespace query; neither pathname is created in the checkout.
  git -C "$ROOT" check-ignore -q -- "$ROOT/.env.ordinary-fixture.local"
}
staging_namespace_case() {
  : >"$STATE/captured-stage-path"
  mktemp() {
    local staged
    staged=$(command mktemp "$@") || return $?
    [[ $(stat -c '%a' "$staged") == 600 && ! -s "$staged" ]] || return 90
    printf '%s\n' "$staged" >>"$STATE/captured-stage-path"
    printf '%s\n' "$staged"
  }
  writer_success || return 1
  [[ $(wc -l <"$STATE/captured-stage-path") == 1 ]] || return 1
  local staged
  IFS= read -r staged <"$STATE/captured-stage-path"
  [[ ${staged%/*} == "$FIX" && ! -e "$staged" ]] || return 1
  git -C "$ROOT" check-ignore -q -- "$ROOT/${staged##*/}"
}
# Root-ratified retry contract: an ordinary required render failure cannot publish
# the canonical environment. Retry uses the same state without removing any file.
render_failure_boundary() {
  local NGINX_RENDER_FAILURE=yes status=0
  : >"$STATE/render-attempts"
  entry_trace >"$STATE/entry-output" 2>&1 || status=$?
  [[ $status == 37 && $(wc -l <"$STATE/render-attempts") == 1
     && ! -e "$STATE/rendered" && ! -e "$STATE/tls" ]] || return 1
  python3 - "$STATE/events" <<'RENDERBOUNDARY'
from pathlib import Path
import sys
v=Path(sys.argv[1]).read_text().splitlines()
for phase in ('fresh:early','load','preflight','fresh:final','review'):
    assert v.count(phase)==1
RENDERBOUNDARY
}
render_failure_retry_case() {
  render_failure_boundary || return 1
  [[ ! -e "$FIX/.env" && ! -L "$FIX/.env" ]] || return 1
  selfhost_storage_assert_fresh "$FIX" final || return 1
  # Actual main is already loaded by entry_trace; no re-source or fixture cleanup.
  main --env-file "$STATE/answers" >"$STATE/retry-output" 2>&1 || return 1
  [[ -f "$FIX/.env" && $(stat -c '%a' "$FIX/.env") == 600
     && -e "$STATE/rendered" && -e "$STATE/tls"
     && $(wc -l <"$STATE/render-attempts") == 2 ]]
}
task3_contracts() {
  for mode in bundled byo-s3; do
    reset_task3
    WRITER_MODE=$mode contract "actual $mode writer emits each WS event-budget default exactly once" writer_success
  done
  contract 'validation AST oracle accepts explicit gates and detects benign Assert syntax' validation_ast_policy control
  for validator in load_answers validate_collected_config compose_dotenv_quote; do
    contract "$validator validation remains enforced when Python optimization is enabled" validation_ast_policy "$validator"
  done
  reset_task3; answer_fixture
  contract 'ordinary valid input passes all three actual validation gates with PYTHONOPTIMIZE=1' optimized_valid_input
  contract 'existing ignored staging namespace query positive control' staging_namespace_control
  reset_task3
  contract 'staging namespace successful publication positive control' writer_success
  reset_task3
  contract 'actual generated staging pathname belongs to an existing gitignored namespace' staging_namespace_case
  reset_task3; answer_fixture
  contract 'valid fresh main reaches one required renderer and propagates its ordinary status37 failure' render_failure_boundary
  reset_task3; answer_fixture
  contract 'required nginx render failure leaves no durable environment and permits a retry without deletion' render_failure_retry_case
  reset_task3; answer_fixture
  contract 'canonical literal answers and BYO defaults' answer_success
  for backend in minio r2 b2; do
    reset_task3; answer_fixture; printf 'CFG_STORAGE_BACKEND=%s\n' "$backend" >>"$STATE/answers"
    ANSWER_BACKEND=$backend contract 'supported external backend retains BYO topology' answer_success
  done
  reset_task3; answer_fixture bundled
  contract 'omitted storage mode defaults to bundled' answer_success bundled
  for old in CFG_MINIO_MODE CFG_MINIO_ENDPOINT CFG_MINIO_USER CFG_MINIO_PASS; do
    reset_task3; answer_fixture
    case $old in
      CFG_MINIO_MODE) new=CFG_STORAGE_MODE;; CFG_MINIO_ENDPOINT) new=CFG_STORAGE_ENDPOINT;;
      CFG_MINIO_USER) new=CFG_STORAGE_ACCESS_KEY;; CFG_MINIO_PASS) new=CFG_STORAGE_SECRET_KEY;;
    esac
    sed -i.bak "s/^$new=/$old=/" "$STATE/answers"
    contract 'historical answer alias normalizes once' answer_success
    sed -n "s/^$old=/$new=/p" "$STATE/answers" >"$STATE/logical-duplicate"
    cat "$STATE/logical-duplicate" >>"$STATE/answers"
    # Equal-value old/new duplicates are still logical duplicates.
    contract 'old/new logical duplicate refuses without input diagnostics' answer_refusal
  done
  reset_task3; answer_fixture
  sed -i.bak -e 's/CFG_STORAGE_MODE=/CFG_MINIO_MODE=/' -e 's/CFG_STORAGE_ENDPOINT=/CFG_MINIO_ENDPOINT=/' \
    -e 's/CFG_STORAGE_ACCESS_KEY=/CFG_MINIO_USER=/' -e 's/CFG_STORAGE_SECRET_KEY=/CFG_MINIO_PASS=/' "$STATE/answers"
  contract 'complete historical fresh answers retain BYO topology' answer_success
  for shape in unknown duplicate identical nul cr invalid-utf8 malformed embedded-lf; do
    reset_task3; answer_fixture
    contract 'answer refusal positive control' answer_success
    [[ $LAST_CONTRACT_OK == yes ]] || continue
    case $shape in
      unknown) printf '%s=%s\n' CFG_SYNTHETIC_REJECTED synthetic-rejected-unknown >>"$STATE/answers";;
      duplicate) printf '%s=%s\n' CFG_STORAGE_SECRET_KEY synthetic-rejected-duplicate >>"$STATE/answers";;
      identical) tail -1 "$STATE/answers" >"$STATE/duplicate"; cat "$STATE/duplicate" >>"$STATE/answers";;
      nul) printf '\0synthetic-rejected-nul' >>"$STATE/answers";;
      cr) printf '\rsynthetic-rejected-cr' >>"$STATE/answers";;
      invalid-utf8) printf '\377synthetic-rejected-encoding' >>"$STATE/answers";;
      malformed) printf 'synthetic-rejected-malformed\n' >>"$STATE/answers";;
      embedded-lf) printf '%s=%s\n%s\n' CFG_STORAGE_SECRET_KEY synthetic-rejected continuation >>"$STATE/answers";;
    esac
    contract 'malformed/duplicate/unknown answer refuses with fixed diagnostics' answer_refusal
    contract 'byte-reader/parser refusal propagates through real installer caller' entry_validation_refusal
  done
  reset_task3; answer_fixture; python3 - "$STATE/answers" <<'TERMINATOR'
from pathlib import Path
import sys
p=Path(sys.argv[1]); p.write_bytes(p.read_bytes().rstrip(b'\n'))
TERMINATOR
  contract 'final answer without newline remains literal' answer_success
  reset_task3; wizard_input
  contract 'both actual wizard credential reads request terminal echo off' wizard_capture
  reset_task3; wizard_input
  contract 'wizard correction equal fixture positive control' wizard_capture
  if [[ $LAST_CONTRACT_OK == yes ]]; then
    sed -i.bak -e $'s#^objects.example.invalid$#https://synthetic-rejected-endpoint\\\nobjects.example.invalid#' \
      -e $'s/^true$/false\\\ntrue/' "$STATE/stdin"
    contract 'endpoint and TLS correct before credentials, without reasking secrets' wizard_capture
  fi
  reset_task3; wizard_input
  contract 'wizard EOF equal fixture positive control' wizard_capture
  [[ $LAST_CONTRACT_OK != yes ]] || contract 'wizard credential EOF propagates before writer' wizard_eof_contract
  reset_task3
  contract 'one additional blank selects bundled without extra picker or confirmation' wizard_bundled_contract
  contract 'hidden read disables actual terminal echo and interrupt propagates' terminal_secret_contract
  contract 'EOF propagates and option-shaped input remains literal' read_contract
  for field in mode backend endpoint access secret tls bucket; do
    reset_task3; answer_fixture
    contract 'common validation equal fixture positive control' entry_success
    [[ $LAST_CONTRACT_OK == yes ]] || continue
    reset_task3; answer_fixture
    case $field in
      mode) key=CFG_STORAGE_MODE;; backend) key=CFG_STORAGE_BACKEND;; endpoint) key=CFG_STORAGE_ENDPOINT;;
      access) key=CFG_STORAGE_ACCESS_KEY;; secret) key=CFG_STORAGE_SECRET_KEY;; tls) key=CFG_STORAGE_USE_SSL;; bucket) key=CFG_STORAGE_BUCKET;;
    esac
    sed -i.bak "/^$key=/d" "$STATE/answers"
    if [[ $field == endpoint ]]; then
      printf '%s=https://synthetic-rejected.invalid\n' "$key" >>"$STATE/answers"
    else
      printf '%s=synthetic-rejected-invalid\n' "$key" >>"$STATE/answers"
    fi
    if [[ $field == bucket || $field == access || $field == secret ]]; then sed -i.bak "s/^$key=.*/$key=/" "$STATE/answers"; fi
    contract 'answer entry refuses invalid storage before host preflight or review' entry_validation_refusal
  done
  for entry in answers interactive; do
    reset_task3; answer_fixture; wizard_input; ENTRY_PATH=$entry contract 'entrypoint validates/preflights/fresh-checks/reviews before publication' entry_success
  done
  for evidence in file symlink image receipt running stopped volume working-dir; do
    reset_task3; answer_fixture; wizard_input
    contract 'fresh-entry refusal fixture positive control' entry_success
    [[ $LAST_CONTRACT_OK == yes ]] || continue
    reset_task3; answer_fixture; wizard_input
    case $evidence in
      file) printf 'synthetic-original\n' >"$FIX/.env";;
      symlink) ln -s "$FIX/missing-target" "$FIX/.env";;
      image|receipt) printf 'synthetic-artifact\n' >"$FIX/.env.activity-history-$evidence.local";;
      *) printf '%s' "$evidence" >"$STATE/scenario";;
    esac
    contract 'existing instance evidence refuses before collection' fresh_entry_refusal
  done
  reset_task3; answer_fixture; printf unrelated >"$STATE/scenario"
  contract 'unrelated Docker project does not block fresh install' entry_success
  reset_task3; answer_fixture
  contract 'final discovery positive control' entry_success
  reset_task3; answer_fixture
  contract 'definitive discovery failure blocks review, writer and later actions' final_discovery_refusal
  reset_task3
  WRITER_VIA_MAIN=yes contract 'real main completes required rendering before successful environment publication' main_render_before_publication
  # Discover capture counts from an independently checked valid output, then fail
  # every ordinal. This covers added captures and existing SMTP/feedback/disclosure.
  for mode in bundled byo-s3; do for feedback in no yes; do
    reset_task3
    WRITER_MODE=$mode WRITER_FEEDBACK=$feedback contract 'writer matrix valid control' writer_success
    if [[ $LAST_CONTRACT_OK != yes || ! -f "$FIX/.env" ]]; then fail 'writer matrix reachability pending valid control'; continue; fi
    ng=$(wc -l <"$STATE/gen"); nh=$(wc -l <"$STATE/hex"); ne=$(wc -l <"$STATE/encode")
    [[ $ng -gt 0 && $nh -gt 0 && $ne -gt 0 ]] || { fail 'writer captures were not reached'; continue; }
    for kind in secret hex encoder; do
      case $kind in
        secret) count=$ng;; hex) count=$nh;; encoder) count=$ne;;
      esac
      for ((ordinal=1; ordinal<=count; ordinal++)); do
        reset_task3
        WRITER_MODE=$mode WRITER_FEEDBACK=$feedback WRITER_VIA_MAIN=yes contract 'capture failure equal fixture positive control' writer_success
        [[ $LAST_CONTRACT_OK == yes ]] || continue
        reset_task3
        if [[ $kind == encoder ]]; then
          WRITER_MODE=$mode WRITER_FEEDBACK=$feedback WRITER_VIA_MAIN=yes FAIL_ENCODER=$ordinal contract 'reached encoder failure aborts publication and cleans staging' writer_failure
        else
          WRITER_MODE=$mode WRITER_FEEDBACK=$feedback WRITER_VIA_MAIN=yes FAIL_GENERATOR=$kind:$ordinal contract 'reached generator failure aborts publication and cleans staging' writer_failure
        fi
      done
    done
  done; done
  # The real installer writer must select the same canonical consumer root for
  # each existing certificate choice and storage topology.
  for tls_mode in local import letsencrypt; do for mode in bundled byo-s3; do
    reset_task3
    WRITER_TLS_MODE=$tls_mode WRITER_MODE=$mode WRITER_VIA_MAIN=yes \
      contract "installer publishes canonical coturn selector for $tls_mode/$mode" writer_success
  done; done
  for destination in file symlink directory; do
    reset_task3; contract 'publication refusal equal fixture positive control' writer_success
    reset_task3; printf 'synthetic-original\n' >"$STATE/original"
    if [[ $destination == file ]]; then cp "$STATE/original" "$FIX/.env"
    elif [[ $destination == directory ]]; then mkdir "$FIX/.env"; cp "$STATE/original" "$FIX/.env/retained"
    else ln -s "$FIX/missing-target" "$FIX/.env"; fi
    DESTINATION_KIND=$destination contract 'writer refuses existing file, dangling symlink or directory without overwrite' publication_refusal
    if [[ $destination == directory ]]; then rm -- "$FIX/.env/retained"; rmdir -- "$FIX/.env"; fi
  done
  reset_task3; contract 'sequential publication equal fixture positive control' writer_success
  reset_task3
  contract 'destination appearing at publication is preserved and staging is private' sequential_publication
}
final_discovery_refusal() {
  local status=0
  FINAL_DISCOVERY_FAILURE=1 entry_trace >"$STATE/entry-output" 2>&1 || status=$?
  [[ $status -ne 0 && ! -e "$FIX/.env" && ! -e "$STATE/rendered" && ! -e "$STATE/tls" ]] || return 1
  grep -qF fresh:final "$STATE/events" || return 1
  ! grep -qE '^(review|writer)$' "$STATE/events" || return 1
  [[ -e "$STATE/error-reached" ]]
}
sequential_publication() {
  source "$TARGET"; config_fixture
  gen_secret() { printf '%s' aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; }
  gen_hex_key() { printf '%s' bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb; }
  # Deterministic filesystem seam: no racing process. Inspect the complete staged
  # secret file before invoking the actual no-replace publication operation.
  mktemp() {
    local staged
    staged=$(command mktemp "$@") || return $?
    [[ $(stat -c '%a' "$staged") == 600 && ! -s "$staged" ]] || return 90
    : >"$STATE/staging-reached"
    printf '%s\n' "$staged"
  }
  ln() {
    local staged=${@: -2:1} destination=${@: -1}
    [[ $(stat -c '%a' "$staged") == 600 ]] || return 91
    grep -q '^POSTGRES_PASSWORD=' "$staged" || return 92
    : >"$STATE/publication-reached"
    printf 'synthetic-original\n' >"$STATE/original"
    cp "$STATE/original" "$destination"
    command ln "$@"
  }
  local status=0
  write_env_file >"$STATE/writer-output" 2>&1 || status=$?
  [[ $status -ne 0 && -e "$STATE/publication-reached" && -e "$STATE/staging-reached" ]] || return 1
  cmp -s "$STATE/original" "$FIX/.env" || return 1
  ! compgen -G "$FIX/.env.new.*" >/dev/null
}

terminal_secret_contract() {
  python3 - "$TARGET" "$STATE" <<'PTY'
import os, pathlib, pty, select, signal, subprocess, sys, termios, time
source, state=sys.argv[1:]
# PTY is only local shell input. The installer source guard prevents service work.
for interrupt in (False, True):
    master, slave=pty.openpty(); output=pathlib.Path(state)/'terminal-result'
    output.unlink(missing_ok=True)
    script='source "$1"; value=$(ask_secret "synthetic hidden prompt") || exit 23; printf "%s" "$value" >"$2"'
    p=subprocess.Popen(['bash','-c',script,'_',source,str(output)],stdin=slave,stdout=slave,stderr=slave,
                       start_new_session=True,
                       preexec_fn=lambda: signal.signal(signal.SIGINT,signal.SIG_DFL))
    os.close(slave); transcript=b''; deadline=time.monotonic()+3
    try:
        while time.monotonic()<deadline:
            if select.select([master],[],[],0.05)[0]:
                chunk=os.read(master,4096)
                if not chunk: break
                transcript+=chunk
            if (b'synthetic hidden prompt' in transcript
                    and not termios.tcgetattr(master)[3] & termios.ECHO):
                break
        assert b'synthetic hidden prompt' in transcript
        assert not termios.tcgetattr(master)[3] & termios.ECHO
        if interrupt: os.killpg(p.pid,signal.SIGINT)
        else: os.write(master,b'-n = $cash "quoted" \\trail \n')
        p.wait(timeout=3)
        if interrupt:
            assert p.returncode != 0 and not output.exists()
        else:
            assert p.returncode == 0 and output.read_bytes()==b'-n = $cash "quoted" \\trail '
        while select.select([master],[],[],0)[0]:
            try: chunk=os.read(master,4096)
            except OSError: break
            if not chunk: break
            transcript+=chunk
        assert b'$cash' not in transcript
    finally:
        if p.poll() is None: os.killpg(p.pid,signal.SIGKILL); p.wait()
        os.close(master)
PTY
}
wizard_bundled_contract() {
  source "$TARGET"
  detect_public_ip() { printf '%s' 203.0.113.10; }
  printf '%s\n' example.com letsencrypt example.com 203.0.113.10 '' smtp.example.invalid \
    587 synthetic-user synthetic-password operator@example.invalid 172.16.0.0/12 '' '' no >"$STATE/stdin"
  collect_config <"$STATE/stdin" || return 1
  [[ ${CFG_STORAGE_MODE:-} == bundled && $CFG_SMTP_HOST == smtp.example.invalid
     && $CFG_FEEDBACK_ENABLED == no && -z ${CFG_STORAGE_ACCESS_KEY:-} && -z ${CFG_STORAGE_SECRET_KEY:-} ]]
}
wizard_eof_contract() {
  source "$TARGET"; detect_public_ip() { printf '%s' 203.0.113.10; }
  head -n 9 "$STATE/stdin" >"$STATE/truncated"
  local status=0
  collect_config <"$STATE/truncated" >"$STATE/eof-output" 2>&1 || status=$?
  [[ $status -ne 0 && ! -e "$FIX/.env" && ! -e "$STATE/rendered" && ! -e "$STATE/tls" ]]
}
entry_validation_refusal() {
  source "$TARGET"; config_fixture
  gen_secret() { printf '%s' aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; }
  gen_hex_key() { printf '%s' bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb; }
  run_preflight() { : >"$STATE/preflight-reached"; }
  print_storage_review() { : >"$STATE/review-reached"; }
  render_nginx() { : >"$STATE/rendered"; }; provision_tls() { : >"$STATE/tls"; }
  local status=0
  main --env-file "$STATE/answers" >"$STATE/validation-output" 2>&1 || status=$?
  [[ $status -ne 0 && ! -e "$FIX/.env" && ! -e "$STATE/preflight-reached"
     && ! -e "$STATE/review-reached" && ! -e "$STATE/rendered" && ! -e "$STATE/tls" ]] || return 1
  ! grep -qE 'synthetic-rejected|cash' "$STATE/validation-output"
}

# UDP admission runs through actual main and run_preflight for both collectors.
# Only unrelated OS/tool/resource/logrotate admission is replaced. Socket
# inventories and credentials below are inert owned input, never host state.
udp_entry_contract() {
  local entry=$1 storage=$2 scenario=$3 expected=$4 status=0 effect
  local -a wizard=() answers=()
  reset_task3
  fixture_repo "$FIX"
  rm -f -- "$STATE/udp-generation" "$STATE/udp-privileged" "$STATE/udp-output" "$STATE/udp-nginx" "$STATE/udp-policy"
  : >"$STATE/udp-calls.jsonl"; : >"$STATE/udp-host-checks"
  printf '%s' "$scenario" >"$STATE/udp-scenario"
  mkdir -p "$STATE/udp-bin"
  cat >"$STATE/udp-bin/inventory.py" <<'UDP_INVENTORY'
import json, os, pathlib, sys
state = pathlib.Path(os.environ['STATE'])
name, args = pathlib.Path(sys.argv[0]).stem, sys.argv[1:]
with (state / 'udp-calls.jsonl').open('a') as out:
    out.write(json.dumps({'name':name, 'args':args, 'locale':os.environ.get('LC_ALL')}) + '\n')
def reject():
    (state / 'rejected').touch()
    sys.exit(97)
if name == 'lsof':
    if args not in [[f'-iTCP:{port}', '-sTCP:LISTEN'] for port in (8080, 3000, 3478, 5349)]:
        reject()
    sys.exit(1)
if name != 'ss':
    reject()
if args == ['-ltn']:
    sys.exit(0)
if args != ['-H', '-a', '-u', '-n'] or os.environ.get('LC_ALL') != 'C':
    reject()
scenario = (state / 'udp-scenario').read_text()
if scenario == 'collector-failure':
    print('udp-inventory-private-sentinel', file=sys.stderr)
    sys.exit(42)
rows = {
    'empty': '',
    'stun': 'UNCONN 0 0 *:3478 *:*',
    'rtc-start': 'UNCONN 0 0 [::]:40000 [::]:*',
    'rtc-base-end': 'ESTAB 0 0 10.0.0.5:40099 10.0.0.6:55000',
    'rtc-after-base': 'UNCONN 0 0 :::40100 :::*',
    'rtc-end': 'UNCONN 0 0 [fe80::1%eth0]:41999 [::]:*',
    'scoped-native-dhcp': 'UNCONN 0 0 [fe80::1]%eth0:546 [::]:*',
    'scoped-native-peer': 'ESTAB 0 0 [::1]:55000 [fe80::2]%eth0:3478',
    'scoped-inside-bracket': 'ESTAB 0 0 [fe80::1%eth0]:546 [fe80::2%eth0]:547',
    'scoped-native-busy': 'UNCONN 0 0 [fe80::1]%eth0:3478 [::]:*',
    'scoped-empty-scope': 'UNCONN 0 0 [fe80::1]%:546 [::]:*',
    'scoped-invalid-scope': 'UNCONN 0 0 [fe80::1]%eth0/2:546 [::]:*',
    'scoped-extra-bracket': 'UNCONN 0 0 [fe80::1]]%eth0:546 [::]:*',
    'scoped-missing-bracket': 'UNCONN 0 0 [fe80::1%eth0:546 [::]:*',
    'scoped-malformed-peer': 'ESTAB 0 0 [::1]:55000 [fe80::2]%eth0/2:547',
    'turn-start': 'UNCONN 0 0 0.0.0.0:49152 0.0.0.0:*',
    'turn-inside': 'UNCONN 0 0 *:49200 *:*',
    'turn-end': 'ESTAB 0 0 ::1:49252 ::1:55000',
    'outside': '\n'.join(f'UNCONN 0 0 0.0.0.0:{port} 0.0.0.0:*'
                         for port in (53, 3477, 3479, 39999, 42000, 49151, 49253, 5432))
               + '\nESTAB 0 0 [::1]:55000 [::1]:3478',
    'malformed-row': 'udp-inventory-private-sentinel',
    'malformed-state': 'LISTEN 0 0 0.0.0.0:53 0.0.0.0:*',
    'malformed-queue': 'UNCONN nope 0 0.0.0.0:53 0.0.0.0:*',
    'malformed-port': 'UNCONN 0 0 0.0.0.0:65536 0.0.0.0:*',
    'malformed-peer': 'UNCONN 0 0 0.0.0.0:53 udp-inventory-private-sentinel',
    'mixed-malformed': 'UNCONN 0 0 0.0.0.0:3478 0.0.0.0:*\nudp-inventory-private-sentinel',
}
if scenario not in rows:
    reject()
print(rows[scenario], end='')
UDP_INVENTORY
  for effect in ss lsof; do
    cp -- "$STATE/udp-bin/inventory.py" "$STATE/udp-bin/$effect.py"
    printf '#!/usr/bin/env bash\nexec /usr/bin/python3 "%s" "$@"\n' "$STATE/udp-bin/$effect.py" >"$STATE/udp-bin/$effect"
    chmod 700 "$STATE/udp-bin/$effect"
  done
  cat >"$STATE/udp-bin/privileged" <<'UDP_PRIVILEGED'
#!/usr/bin/env bash
: >"$STATE/udp-privileged"
: >"$STATE/rejected"
exit 97
UDP_PRIVILEGED
  chmod 700 "$STATE/udp-bin/privileged"
  for effect in sudo ufw systemctl curl; do cp -- "$STATE/udp-bin/privileged" "$STATE/udp-bin/$effect"; done
  export PATH="$STATE/udp-bin:$PATH" PUBLIC_IP=203.0.113.10
  source "$TARGET"
  ENV_OUT="$FIX/.env"; NGINX_OUT="$STATE/udp-nginx"
  preflight_os() { printf 'os\n' >>"$STATE/udp-host-checks"; }
  preflight_docker() { printf 'docker\n' >>"$STATE/udp-host-checks"; }
  preflight_resources() { printf 'resources\n' >>"$STATE/udp-host-checks"; }
  preflight_logrotate() { printf 'logrotate\n' >>"$STATE/udp-host-checks"; }
  command() {
    if [[ $# -eq 2 && $1 == -v && $2 == ss && $scenario == collector-missing ]]; then return 1; fi
    builtin command "$@"
  }
  gen_secret() { : >"$STATE/udp-generation"; printf '%s' aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; }
  gen_hex_key() { : >"$STATE/udp-generation"; printf '%s' bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb; }
  source <(declare -f run_preflight | sed '1s/run_preflight/udp_fixture_preflight/')
  run_preflight() { printf 'preflight\n' >>"$STATE/events"; udp_fixture_preflight "$@"; }
  source <(declare -f print_storage_review | sed '1s/print_storage_review/udp_fixture_review/')
  print_storage_review() { printf 'review\n' >>"$STATE/events"; udp_fixture_review "$@"; }
  source <(declare -f render_nginx | sed '1s/render_nginx/udp_fixture_render/')
  render_nginx() { printf 'render\n' >>"$STATE/events"; udp_fixture_render "$@"; }
  source <(declare -f write_env_file | sed '1s/write_env_file/udp_fixture_writer/')
  write_env_file() { printf 'writer\n' >>"$STATE/events"; udp_fixture_writer "$@"; }
  source <(declare -f provision_tls | sed '1s/provision_tls/udp_fixture_tls/')
  provision_tls() { printf 'tls\n' >>"$STATE/events"; udp_fixture_tls "$@"; }
  source <(declare -f print_summary | sed '1s/print_summary/udp_fixture_summary/')
  print_summary() { printf 'summary\n' >>"$STATE/events"; udp_fixture_summary "$@"; }
  if [[ $entry == answers ]]; then
    answers=('CFG_DOMAIN=example.com' 'CFG_PUBLIC_IP=203.0.113.10' 'CFG_TLS_MODE=local'
      "CFG_STORAGE_MODE=$storage" 'CFG_SMTP_HOST=smtp.example.invalid' 'CFG_FEEDBACK_ENABLED=no')
    if [[ $storage == byo-s3 ]]; then
      answers+=('CFG_STORAGE_ENDPOINT=objects.example.invalid' 'CFG_STORAGE_USE_SSL=true'
        'CFG_STORAGE_BUCKET=concord-media' 'CFG_STORAGE_ACCESS_KEY=udp-owned-access'
        'CFG_STORAGE_SECRET_KEY=udp-owned-secret')
    fi
    printf '%s\n' "${answers[@]}" >"$STATE/udp-answers"
    (main --env-file "$STATE/udp-answers") >"$STATE/udp-output" 2>&1 || status=$?
  else
    wizard=(example.com local example.com 203.0.113.10 "$storage")
    if [[ $storage == byo-s3 ]]; then
      wizard+=(objects.example.invalid true concord-media '' udp-owned-access udp-owned-secret)
    fi
    wizard+=(smtp.example.invalid 587 fixture-user fixture-password operator@example.invalid 172.16.0.0/12 '' '' no)
    printf '%s\n' "${wizard[@]}" >"$STATE/udp-stdin"
    (main <"$STATE/udp-stdin") >"$STATE/udp-output" 2>&1 || status=$?
  fi
  [[ ! -e "$STATE/rejected" && ! -e "$STATE/udp-privileged" ]] || return 1
  ! compgen -G "$FIX/.env.new.*" >/dev/null || return 1
  ! grep -qE 'command not found|unbound variable|syntax error|Traceback|AssertionError|udp-inventory-private-sentinel|udp-owned-secret' "$STATE/udp-output" || return 1
  [[ $(cat "$STATE/udp-host-checks") == $'os\ndocker\nresources\nlogrotate' ]] || return 1
  python3 - "$STATE/udp-calls.jsonl" "$scenario" "$expected" <<'UDP_CALLS' || return 1
import json, sys
calls = [json.loads(line) for line in open(sys.argv[1])]
tcp = [call for call in calls if call['args'] == ['-ltn'] or call['name'] == 'lsof']
udp = [call for call in calls if call['args'] == ['-H', '-a', '-u', '-n']]
if sys.argv[2] == 'collector-missing':
    assert [call['args'] for call in tcp] == [[f'-iTCP:{port}', '-sTCP:LISTEN'] for port in (8080, 3000, 3478, 5349)]
    assert not udp
else:
    assert len(tcp) == 4 and all(call['name'] == 'ss' and call['args'] == ['-ltn'] for call in tcp)
    # Baseline positives still prove the previously supported publication path;
    # all occupied/error negatives independently require the new inventory.
    assert len(udp) in ((0, 1) if sys.argv[3] == 'accept' and not sys.argv[2].startswith('scoped-') else (1,))
    assert all(call['locale'] == 'C' for call in udp)
assert len(calls) == len(tcp) + len(udp)
UDP_CALLS
  if [[ $expected == accept ]]; then
    [[ $status -eq 0 && -f "$FIX/.env" && $(stat -c '%a' "$FIX/.env") == 600
       && -f "$STATE/udp-nginx" && -e "$STATE/udp-generation" ]] || return 1
    [[ $(cat "$STATE/events") == $'preflight\nreview\nrender\nwriter\ntls\nsummary' ]] || return 1
    grep -qFx "SELFHOST_STORAGE_MODE=$storage" "$FIX/.env" || return 1
    grep -qF 'server_name example.com' "$STATE/udp-nginx" || return 1
    if [[ $storage == byo-s3 ]]; then grep -qFx 'STORAGE_SECRET_KEY="udp-owned-secret"' "$FIX/.env" || return 1; fi # pragma: allowlist secret -- inert owned fixture, never a provider key
  else
    [[ $status -eq 2 && ! -e "$FIX/.env" && ! -L "$FIX/.env" && ! -e "$STATE/udp-nginx"
       && ! -e "$STATE/udp-generation" && $(cat "$STATE/events") == preflight ]] || return 1
    if [[ $expected == busy ]]; then
      grep -qFx 'installer: required UDP ports already in use' "$STATE/udp-output" || return 1
    else
      grep -qFx 'installer: UDP socket inventory unavailable or unparseable' "$STATE/udp-output" || return 1
    fi
  fi
}
udp_scoped_endpoint_contracts() {
  # Regression for #1615 / PR #3584: native ss spells a zone after the closing
  # IPv6 bracket. DHCPv6 must not abort publication, and a scoped peer's port
  # must not be mistaken for an occupied local port.
  local scenario
  for scenario in outside scoped-inside-bracket scoped-native-dhcp scoped-native-peer; do
    contract "scoped UDP $scenario accepts inventory and publishes the fresh environment" \
      udp_entry_contract answers bundled "$scenario" accept
    if [[ $LAST_CONTRACT_OK == no ]] &&
       grep -qFx 'installer: UDP socket inventory unavailable or unparseable' "$STATE/udp-output"; then
      printf '%s\n' 'Observed: installer: UDP socket inventory unavailable or unparseable; fresh environment was not published'
    fi
  done
  for scenario in stun rtc-end scoped-native-busy; do
    contract "scoped UDP $scenario occupied required local port refuses with the conflict diagnostic before publication" \
      udp_entry_contract answers bundled "$scenario" busy
    if [[ $LAST_CONTRACT_OK == no ]] &&
       grep -qFx 'installer: UDP socket inventory unavailable or unparseable' "$STATE/udp-output"; then
      printf '%s\n' 'Observed: installer: UDP socket inventory unavailable or unparseable; expected the required-port conflict diagnostic'
    fi
  done
  for scenario in scoped-empty-scope scoped-invalid-scope scoped-extra-bracket scoped-missing-bracket scoped-malformed-peer; do
    contract "scoped UDP $scenario refuses as unparseable before publication" \
      udp_entry_contract answers bundled "$scenario" inventory
  done
}
udp_compose_parity() {
  local storage=$1 status=0
  [[ ${#COMPOSE_ORACLE[@]} -gt 0 ]] || return 1
  udp_entry_contract answers "$storage" empty accept || return 1
  mkdir -p "$STATE/udp-home" "$STATE/udp-docker-config"
  : >"$FIX/spa.env"
  local -a models=(-f "$FIX/docker-compose.yml" -f "$FIX/docker-compose.production.yml" -f "$FIX/docker-compose.selfhost.yml")
  [[ $storage != byo-s3 ]] || models+=(-f "$FIX/docker-compose.byo-s3.yml")
  # The captured supported Compose binary renders configuration only. The
  # Docker fixture continues to own every freshness query; no socket is used.
  env -i PATH=/usr/bin:/bin HOME="$STATE/udp-home" DOCKER_CONFIG="$STATE/udp-docker-config" \
    CONTROL_PLANE_IMAGE=concord-fixture:inert \
    "${COMPOSE_ORACLE[@]}" --env-file "$FIX/.env" --project-directory "$FIX" \
    "${models[@]}" --profile services config --format json >"$STATE/udp-model.json" 2>"$STATE/udp-compose-errors" || status=$?
  [[ $status -eq 0 ]] || return 1
  printf '%s\n' "${REQUIRED_UDP_RANGES[@]}" >"$STATE/udp-policy"
  python3 - "$STATE/udp-model.json" "$STATE/udp-policy" <<'UDP_PARITY'
import json, sys
model = json.load(open(sys.argv[1]))
services = model['services']
def interval(value):
    pieces = str(value).split('-')
    assert 1 <= len(pieces) <= 2 and all(piece.isdecimal() for piece in pieces)
    low, high = int(pieces[0]), int(pieces[-1])
    assert 0 < low <= high <= 65535
    return range(low, high + 1)
published = {}
for service, value in services.items():
    for port in value.get('ports', []):
        if port.get('protocol', 'tcp') != 'udp':
            continue
        targets, hosts = list(interval(port['target'])), list(interval(port['published']))
        assert targets == hosts and port.get('host_ip', '') in ('', '0.0.0.0', '::')
        published.setdefault(service, set()).update(hosts)
expected = {'coturn': {3478, *range(49152, 49253)}, 'media-plane': set(range(40000, 42000))}
assert published == expected, 'selected self-host publication windows changed'
media = services['media-plane']['environment']
assert str(media['RTC_MIN_PORT']) == '40000' and str(media['RTC_MAX_PORT']) == '41999'
policy = set()
for line in open(sys.argv[2]):
    pieces = line.strip().split(':')
    assert len(pieces) == 2 and all(piece.isdecimal() for piece in pieces)
    low, high = map(int, pieces)
    assert 0 < low <= high <= 65535
    policy.update(range(low, high + 1))
assert policy == set().union(*published.values()), 'UDP admission differs from the rendered selected stack'
UDP_PARITY
}
udp_preflight_contracts() {
  local entry storage scenario
  udp_scoped_endpoint_contracts
  [[ $UDP_SCOPED_ONLY != true ]] || return 0
  for storage in bundled byo-s3; do
    contract "$storage actual rendered Compose UDP publications and RTC bounds match admission" udp_compose_parity "$storage"
    for entry in answers interactive; do
      for scenario in empty outside; do
        contract "complete $entry $storage install accepts unrelated $scenario UDP inventory" udp_entry_contract "$entry" "$storage" "$scenario" accept
      done
      for scenario in stun rtc-start rtc-base-end rtc-after-base rtc-end turn-start turn-inside turn-end; do
        contract "complete $entry $storage install refuses occupied $scenario UDP before all secrets and publication" udp_entry_contract "$entry" "$storage" "$scenario" busy
      done
      for scenario in collector-failure collector-missing malformed-row malformed-state malformed-queue malformed-port malformed-peer mixed-malformed; do
        contract "complete $entry $storage install refuses $scenario UDP inventory before all secrets and publication" udp_entry_contract "$entry" "$storage" "$scenario" inventory
      done
    done
  done
}
if [[ $UDP_ONLY == true ]]; then
  udp_preflight_contracts
  [[ $fails -eq 0 ]] && { echo 'ALL UDP SOURCE-PROJECTION BOUNDARIES PASS'; exit 0; }
  echo "$fails UDP SOURCE-PROJECTION FAILURES"; exit 1
fi

if [[ $FEEDBACK_ONLY == true ]]; then
  export PCLOG="$STUB/pc.log"
  cat >"$STUB/provision-cert.sh" <<'FEEDBACKPC'
#!/usr/bin/env bash
printf 'unexpected certificate invocation\n' >>"$PCLOG"
exit 97
FEEDBACKPC
  chmod 700 "$STUB/provision-cert.sh"
else
# ── Task 1: Source guard + --help ──────────────────────────────────────────────

# Sourcing must NOT run main (source-guard).
out="$(source "$TARGET" 2>&1)"; rc=$?
[[ $rc -eq 0 && -z "$out" ]] \
  && pass "source does not run main" \
  || fail "source guard behavior failed"

# --help prints usage and exits 0.
out="$(bash "$TARGET" --help 2>&1)"; rc=$?
[[ $rc -eq 0 ]] && echo "$out" | grep -qi 'usage' \
  && pass "--help prints usage" \
  || fail "--help broken (rc=$rc)"

# ── Task 2: port_in_use + preflight_ports ─────────────────────────────────────

# port_in_use uses ss stub.
cat >"$STUB/ss" <<'EOF'
#!/usr/bin/env bash
# emit a fake listener for :8080 only
echo "LISTEN 0 0 0.0.0.0:8080 0.0.0.0:*"
EOF
chmod +x "$STUB/ss"

( source "$TARGET"; PATH="$STUB:$PATH" port_in_use 8080 ) \
  && pass "port_in_use detects bound 8080" \
  || fail "port_in_use missed 8080"

( source "$TARGET"; PATH="$STUB:$PATH" port_in_use 9999 ) \
  && fail "port_in_use false-positive 9999" \
  || pass "port_in_use clears 9999"

# preflight_ports aborts (rc=2) when a required port is bound.
( source "$TARGET"; PATH="$STUB:$PATH" preflight_ports ) \
  && fail "preflight_ports passed with 8080 bound" \
  || pass "preflight_ports aborts on bound port"

# ── Task 3: Secret gen + validators ───────────────────────────────────────────

# CSPRNG helpers — stub openssl to assert the exact flags used.
cat >"$STUB/openssl" <<'EOF'
#!/usr/bin/env bash
echo "stub:$*"
EOF
chmod +x "$STUB/openssl"

v="$(source "$TARGET"; PATH="$STUB:$PATH" gen_hex_key)"
[[ "$v" == "stub:rand -hex 32" ]] \
  && pass "gen_hex_key uses rand -hex 32" \
  || fail "gen_hex_key flags wrong"

# Successful self-host secret generation yields exactly 64 lowercase hexadecimal
# characters safe for an unquoted dotenv assignment, including when Base64
# encoding would consist only of stripped punctuation.
# Catch the real helper dropping encoded punctuation until its output is empty.
# Keep these fixtures separate from the argument-echo stub used by later tests.
SECRET_STUB="$STUB/secret-generation"
SECRET_ERROR_STUB="$STUB/secret-error"
mkdir -p "$SECRET_STUB" "$SECRET_ERROR_STUB"
cat >"$SECRET_STUB/openssl" <<'EOF'
#!/usr/bin/env bash
[[ $# -eq 3 ]] || exit 64
case "$*" in
  'rand -base64 48')
    printf '%s\n' '////////////////////////////////////////////////////////////////'
    ;;
  'rand -hex 32')
    printf '%s\n' 'ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff'
    ;;
  *) exit 64 ;;
esac
EOF
cat >"$SECRET_ERROR_STUB/openssl" <<'EOF'
#!/usr/bin/env bash
[[ $# -eq 3 ]] || exit 64
case "$*" in
  'rand -base64 48'|'rand -hex 32') exit 17 ;;
  *) exit 64 ;;
esac
EOF
chmod +x "$SECRET_STUB/openssl" "$SECRET_ERROR_STUB/openssl"

# Independently establish the exact bytes and encoding of both adversarial
# fixtures, including their trailing newline; do not use gen_secret's filters.
if "$SECRET_STUB/openssl" rand -base64 48 >"$STUB/secret-base64.fixture" \
   && "$SECRET_STUB/openssl" rand -hex 32 >"$STUB/secret-hex.fixture" \
   && python3 - "$STUB/secret-base64.fixture" "$STUB/secret-hex.fixture" <<'PY'; then
import base64
import pathlib
import sys

base64_fixture = pathlib.Path(sys.argv[1]).read_bytes()
hex_fixture = pathlib.Path(sys.argv[2]).read_bytes()
assert base64_fixture == b"/" * 64 + b"\n"
assert base64.b64decode(base64_fixture[:-1], validate=True) == b"\xff" * 48
assert hex_fixture == b"f" * 64 + b"\n"
assert bytes.fromhex(hex_fixture[:-1].decode("ascii")) == b"\xff" * 32
PY
  pass "secret fixtures encode all-0xff bytes with exact lengths and newlines"
else
  fail "secret fixture positive control failed"
fi

# The conditional captures failures without source's set -e ending the suite.
if secret="$(source "$TARGET"; PATH="$SECRET_STUB:$PATH" gen_secret)"; then
  secret_rc=0
else
  secret_rc=$?
fi
all_f_hex='ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff' # pragma: allowlist secret -- non-secret fixture: 64 'f' hex chars
[[ $secret_rc -eq 0 && "$secret" =~ ^[0-9a-f]{64}$ \
   && "$secret" == "$all_f_hex" ]] \
  && pass "gen_secret returns exactly 64 lowercase hex characters for all-strip Base64 fixture" \
  || fail "gen_secret returns exactly 64 lowercase hex characters for all-strip Base64 fixture (empty/short or invalid output: rc=$secret_rc, length=${#secret}, expected=64)"

if secret="$(source "$TARGET"; PATH="$SECRET_ERROR_STUB:$PATH" gen_secret)"; then
  secret_rc=0
else
  secret_rc=$?
fi
[[ $secret_rc -eq 17 && -z "$secret" ]] \
  && pass "gen_secret preserves OpenSSL exit 17 with empty stdout" \
  || fail "gen_secret preserves OpenSSL exit 17 with empty stdout (rc=$secret_rc, length=${#secret})"

# validate_domain
( source "$TARGET"; validate_domain "example.com" ) \
  && pass "validate_domain accepts example.com" \
  || fail "validate_domain rejected good domain"

( source "$TARGET"; validate_domain "media.example.com" ) \
  && fail "validate_domain accepted media.-prefixed apex" \
  || pass "validate_domain rejects media.-prefixed apex"

( source "$TARGET"; validate_domain "" ) \
  && fail "validate_domain accepted empty" \
  || pass "validate_domain rejects empty domain"

# validate_media_subdomain: derived media URL must be a subdomain, never root.
( source "$TARGET"; validate_media_subdomain "https://media.example.com" ) \
  && pass "validate_media_subdomain accepts media subdomain url" \
  || fail "validate_media_subdomain rejected media subdomain"

( source "$TARGET"; validate_media_subdomain "https://example.com" ) \
  && fail "validate_media_subdomain accepted root media url" \
  || pass "validate_media_subdomain rejects root media url"

# ── Task 4: load_answers (--env-file parity) ──────────────────────────────────

ans="$STUB/answers.env"
cat >"$ans" <<'EOF'
CFG_DOMAIN=example.com
CFG_PUBLIC_IP=203.0.113.10
CFG_SMTP_HOST=smtp.example.com
CFG_SMTP_FROM=noreply@example.com
CFG_FEEDBACK_ENABLED=no
CFG_MINIO_MODE=bundled
CFG_ACTIVITY_HISTORY_OPERATOR_NAME=Example #1 $Ops O'Brien
CFG_ACTIVITY_HISTORY_PRIVACY_POLICY_URL=https://privacy.example.com/policy#activity
EOF

( source "$TARGET"; load_answers "$ans"
  [[ "$CFG_DOMAIN" == "example.com" && "$CFG_PUBLIC_IP" == "203.0.113.10" \
     && "$CFG_ACTIVITY_HISTORY_OPERATOR_NAME" == "Example #1 \$Ops O'Brien" \
     && "$CFG_ACTIVITY_HISTORY_PRIVACY_POLICY_URL" == "https://privacy.example.com/policy#activity" ]] ) \
  && pass "load_answers parses answers" \
  || fail "load_answers parity broken"

# rejects a non-existent file
( source "$TARGET"; load_answers "$STUB/nope.env" ) \
  && fail "load_answers accepted missing answers" \
  || pass "load_answers rejects missing answers file"

# ── Task 5: write_env_file ────────────────────────────────────────────────────

out_env="$STUB/.env.out"
log_env="$STUB/writer.log"
expected_operator="Example #1 \$Ops \${TOKEN} \"quoted\" O'Brien A\\'B tail\\"
(
  source "$TARGET"
  ENV_OUT="$out_env"
  CFG_DOMAIN=example.com
  CFG_PUBLIC_IP=203.0.113.10
  CFG_TURN_REALM=example.com
  CFG_STORAGE_MODE=bundled
  CFG_SMTP_HOST=smtp.example.com
  CFG_SMTP_PORT=587
  CFG_SMTP_USER=u
  CFG_SMTP_PASS=p
  CFG_SMTP_FROM=noreply@example.com
  CFG_TRUSTED_CIDRS='172.16.0.0/12'
  CFG_FEEDBACK_ENABLED=no
  CFG_ACTIVITY_HISTORY_OPERATOR_NAME="$expected_operator"
  CFG_ACTIVITY_HISTORY_PRIVACY_POLICY_URL='https://privacy.example.com/policy#activity'
  PATH="$STUB:$PATH" write_env_file
) >"$log_env" 2>&1

all_vars_ok=1
for k in ENVIRONMENT DOMAIN PUBLIC_IP POSTGRES_PASSWORD REDIS_PASSWORD JWT_SECRET \
         MFA_ENCRYPTION_KEY TURN_SECRET TURN_REALM MINIO_ROOT_USER MINIO_ROOT_PASSWORD \
         MINIO_BUCKET TRUSTED_PROXY_CIDRS SMTP_HOST SMTP_PORT SMTP_USERNAME SMTP_PASSWORD \
         SMTP_FROM FEEDBACK_PAT FEEDBACK_REPO ACTIVITY_HISTORY_CLUSTER_ENABLED \
         CONTROL_PLANE_REPLICA_COUNT ACTIVITY_HISTORY_OPERATOR_NAME \
         ACTIVITY_HISTORY_PRIVACY_POLICY_URL; do
  grep -q "^${k}=" "$out_env" || { fail "env missing $k"; all_vars_ok=0; }
done
[[ $all_vars_ok -eq 1 ]] && pass "env writer populated all required vars"

assert_ws_event_budget_defaults "$out_env" \
  && pass "bundled writer emits each WS event-budget default exactly once" \
  || fail "bundled writer WS event-budget defaults are missing, duplicated or incorrect"

# Secret VALUES must not appear in stdout/log (only names).
# The openssl stub emits "stub:rand ..." which would expose the pattern if present.
# We also guard against any long alphanumeric run that would indicate a real secret leaked.
if grep -qE 'stub:rand' "$log_env" 2>/dev/null; then
  fail "secret value (stub:rand) leaked to log"
else
  pass "no secret value in log"
fi

# DOMAIN written correctly.
grep -qE '^DOMAIN=(example[.]com|"example[.]com")$' "$out_env" \
  && pass "domain written correctly" \
  || fail "domain not written"

grep -qE '^ACTIVITY_HISTORY_OPERATOR_NAME=".*"$' "$out_env" \
  && pass "operator name uses the double-quoted Compose dotenv encoder" \
  || fail "operator name is not encoded as a safe double-quoted dotenv value"
grep -qxF 'ACTIVITY_HISTORY_PRIVACY_POLICY_URL="https://privacy.example.com/policy#activity"' "$out_env" \
  && pass "privacy URL is double-quoted without fragment loss" \
  || fail "privacy URL special-character encoding is unsafe"

# This independent decoder checks the writer grammar only. Supported-version
# Docker-boundary equality has its own saved acceptance gate; this suite never
# reaches a daemon or claims this decoder establishes Compose behavior.
if python3 - "$out_env" "$expected_operator" <<'PYDOT'; then
import pathlib, sys
line = next(x for x in pathlib.Path(sys.argv[1]).read_text().splitlines() if x.startswith('ACTIVITY_HISTORY_OPERATOR_NAME='))
v = line.split('=', 1)[1]
assert v[0] == v[-1] == '"'
out = ''; i = 1
while i < len(v)-1:
    if v[i] == '\\':
        assert v[i+1] in '\\"'; out += v[i+1]; i += 2
    elif v[i] == '$':
        assert v[i:i+2] == '$$'; out += '$'; i += 2
    else: out += v[i]; i += 1
assert out == sys.argv[2]
PYDOT
pass "operator literal round-trips through sanctioned writer grammar"
else fail "operator literal writer grammar changed"; fi

if ( source "$TARGET"; ! compose_dotenv_quote $'line one\nline two' >/dev/null ); then
  pass "Compose dotenv encoder rejects embedded newlines"
else
  fail "Compose dotenv encoder accepted an embedded newline"
fi

( source "$TARGET"; validate_activity_history_disclosure "" "" ) \
  && pass "empty Activity History disclosure remains allowed/unavailable" \
  || fail "empty Activity History disclosure should be allowed"
( source "$TARGET"; validate_activity_history_disclosure "Example Operator" "https://example.com/privacy" ) \
  && pass "absolute HTTPS Activity History privacy URL accepted" \
  || fail "valid Activity History disclosure rejected"
if ( source "$TARGET"
     declare -F validate_activity_history_disclosure >/dev/null \
       && ! validate_activity_history_disclosure "Example Operator" "http://example.com/privacy" ); then
  pass "non-HTTPS Activity History privacy URL rejected"
else
  fail "non-HTTPS Activity History privacy URL validation is missing or fail-open"
fi
if ( source "$TARGET"; ! validate_activity_history_disclosure "Example Operator" "https://#fragment" ); then
  pass "hostless HTTPS Activity History privacy URL rejected"
else
  fail "hostless HTTPS Activity History privacy URL accepted"
fi
if ( source "$TARGET"; ! validate_activity_history_disclosure "Example Operator" "https://:" ); then
  pass "HTTPS privacy URL with a port delimiter but no hostname is rejected"
else
  fail "malformed HTTPS privacy URL without a hostname was accepted"
fi
if ( source "$TARGET"; ! validate_activity_history_disclosure "Example Operator" "https://[:::]/privacy" ); then
  pass "malformed bracketed IPv6 privacy host is rejected"
else
  fail "malformed bracketed IPv6 privacy host was accepted"
fi
if ( source "$TARGET"; ! validate_activity_history_disclosure "Example Operator" "https://[::1]/privacy" ); then
  pass "unsupported bracketed literal privacy host is rejected"
else
  fail "unsupported bracketed literal privacy host was accepted"
fi
if ( source "$TARGET"; ! validate_activity_history_disclosure "Example Operator" "https://example.com:65536/privacy" ); then
  pass "out-of-range HTTPS privacy port is rejected"
else
  fail "out-of-range HTTPS privacy port was accepted"
fi
if ( source "$TARGET"; ! validate_activity_history_disclosure "Example Operator" "https://exa%6dple..com/privacy" ); then
  pass "escaped or malformed DNS privacy authority is rejected"
else
  fail "escaped or malformed DNS privacy authority was accepted"
fi
if ( source "$TARGET"; ! validate_activity_history_disclosure "Example Operator" "https://example.com/privacy policy" ); then
  pass "Activity History privacy URL containing whitespace rejected"
else
  fail "Activity History privacy URL containing whitespace accepted"
fi

grep -qE 'CFG_ACTIVITY_HISTORY_OPERATOR_NAME="\$\(ask ' "$TARGET" \
  && pass "interactive installer prompts for Activity History operator name" \
  || fail "interactive installer lacks Activity History operator prompt"
grep -qE 'CFG_ACTIVITY_HISTORY_PRIVACY_POLICY_URL="\$\(ask ' "$TARGET" \
  && pass "interactive installer prompts for Activity History privacy URL" \
  || fail "interactive installer lacks Activity History privacy URL prompt"

# #3561 review: opaque values (TURN_REALM, SMTP_USERNAME, SMTP_PASSWORD)
# are written through compose_dotenv_quote. Raw, a `$`
# is interpolated by Compose, a ` #` starts a comment, and a password silently
# arrives altered with no hint of an encoding cause. Each opaque value carries a
# `$`, a ` #`, a space and `<>`. SMTP_HOST and SMTP_FROM exercise the supported
# bare host/mailbox grammar through the same real validator and literal writer.
out_free="$STUB/.env.free"
free_realm='realm$r #1 <r>'
free_host='SMTP-relay.lan.'
free_user='user$u #1 <u>'
free_pass='pa$word9 #tail <p>'       # pragma: allowlist secret -- non-secret test fixture
free_from='Ops.tag_1%route+alias-2@relay'
write_free() { # $1 = SMTP password; $2 = env file; runs the real writer, echoes rc
  ( source "$TARGET"
    ENV_OUT="$2"
    CFG_STORAGE_MODE=bundled
    CFG_DOMAIN=example.com; CFG_PUBLIC_IP=203.0.113.10; CFG_TURN_REALM="$free_realm"
    CFG_SMTP_HOST="$free_host"; CFG_SMTP_PORT=587; CFG_SMTP_USER="$free_user"
    CFG_SMTP_PASS="$1"; CFG_SMTP_FROM="$free_from"
    CFG_TRUSTED_CIDRS='172.16.0.0/12'; CFG_FEEDBACK_ENABLED=no
    PATH="$STUB:$PATH" write_env_file ) >/dev/null 2>&1
  echo $?
}
rm -f "$out_free"
[[ "$(write_free "$free_pass" "$out_free")" == 0 && -f "$out_free" ]] \
  && pass "write_env_file accepts free-text values with \$, ' #', spaces and <>" \
  || fail "write_env_file refused the free-text control config"
for pair in "TURN_REALM:$free_realm" "SMTP_HOST:$free_host" "SMTP_USERNAME:$free_user" \
            "SMTP_PASSWORD:$free_pass" "SMTP_FROM:$free_from"; do # pragma: allowlist secret -- non-secret fixture
  key="${pair%%:*}"; val="${pair#*:}"
  want="$( source "$TARGET"; compose_dotenv_quote "$val" )"
  grep -qxF "${key}=${want}" "$out_free" \
    && pass "write_env_file writes $key through compose_dotenv_quote" \
    || fail "write_env_file $key not encoded: got [$(grep "^${key}=" "$out_free")], want [${key}=${want}]"
done
# Independent of the encoder: Compose's literal-dollar form for the password.
pass_literal='SMTP_PASSWORD="pa$$word9 #tail <p>"' # pragma: allowlist secret -- non-secret fixture
grep -qxF "$pass_literal" "$out_free" \
  && pass "SMTP_PASSWORD is a double-quoted literal with \$\$ for the dollar" \
  || fail "SMTP_PASSWORD literal wrong: $(grep '^SMTP_PASSWORD=' "$out_free")"

# Actual supported-version Compose parser oracle, configuration-only: no daemon.
# The executable bypass is limited to this exact command, explicit synthetic
# envfile/model, and empty caller environment/private HOME and Docker config.
COMPOSE_ORACLE_ROOT="$STUB/compose-oracle"
mkdir -p "$COMPOSE_ORACLE_ROOT/home" "$COMPOSE_ORACLE_ROOT/docker-config"
cp -- "$ROOT/docker-compose.yml" "$COMPOSE_ORACLE_ROOT/docker-compose.yml"
# The copied control-plane model requires its relative env_file to exist.
# Create empty synthetic runtime metadata; never copy a real environment file.
: >"$COMPOSE_ORACLE_ROOT/spa.env"
[[ ${#COMPOSE_ORACLE[@]} -gt 0 ]] \
  || fail 'supported Compose literal parser prerequisite is unavailable (install Docker Compose or set absolute COMPOSE_BIN)'
rt_env() { # $1 = variable; $2 = optional known synthetic writer output
  [[ ${#COMPOSE_ORACLE[@]} -gt 0 ]] || return 1
  local rendered status=0
  rendered="$( cd "$COMPOSE_ORACLE_ROOT" && env -i PATH=/usr/bin:/bin \
    HOME="$COMPOSE_ORACLE_ROOT/home" DOCKER_CONFIG="$COMPOSE_ORACLE_ROOT/docker-config" \
    "${COMPOSE_ORACLE[@]}" --env-file "${2:-$out_free}" --project-directory "$COMPOSE_ORACLE_ROOT" \
    -f "$COMPOSE_ORACLE_ROOT/docker-compose.yml" --profile services config --environment \
    2>"$STUB/compose-oracle.err" )" || status=$?
  [[ $status -eq 0 ]] || return "$status"
  sed -n "s/^$1=//p" <<<"$rendered"
}
rt_pass="$(rt_env SMTP_PASSWORD)"; rt_pass_rc=$?
[[ $rt_pass_rc -eq 0 && "$rt_pass" == "$free_pass" ]] \
  && pass "SMTP_PASSWORD with \$ and ' #' round-trips through real Compose" \
  || fail "Compose literal parser changed SMTP_PASSWORD (synthetic output withheld)"
rt_from="$(rt_env SMTP_FROM)"; rt_from_rc=$?
[[ $rt_from_rc -eq 0 && "$rt_from" == "$free_from" ]] \
  && pass "SMTP_FROM with \$, ' #' and <> round-trips through real Compose" \
  || fail "Compose literal parser changed SMTP_FROM (synthetic output withheld)"

rt_operator="$(rt_env ACTIVITY_HISTORY_OPERATOR_NAME "$out_env")"; rt_operator_rc=$?
[[ $rt_operator_rc -eq 0 && "$rt_operator" == "$expected_operator" ]] \
  && pass "adjacent apostrophe/backslash, trailing backslash, dollars, braces, and quotes round-trip through real Compose" \
  || fail "Compose literal parser changed operator disclosure (synthetic output withheld)"

# A newline would become a SECOND dotenv line (an injected assignment). The writer
# must refuse before it opens ENV_OUT, so no partial file is left behind either.
out_nl="$STUB/.env.newline"
rm -f "$out_nl"
nl_rc="$(write_free $'a\nb' "$out_nl")"
[[ "$nl_rc" != 0 ]] \
  && pass "write_env_file exits non-zero when a value contains a newline" \
  || fail "write_env_file accepted a newline in CFG_SMTP_PASS (rc=0)"
[[ ! -e "$out_nl" ]] \
  && pass "write_env_file leaves ENV_OUT uncreated when a value is rejected" \
  || fail "write_env_file left a partial $out_nl after rejecting a newline"
rm -f "$out_free" "$out_nl"

# File should have mode 600.
mode="$(python3 -c "import os; print(oct(os.stat('$out_env').st_mode)[-3:])" 2>/dev/null || echo "unknown")"
[[ "$mode" == "600" ]] \
  && pass "env file has mode 600" \
  || fail "env file mode wrong: $mode"

# ── Task 6: render_nginx ──────────────────────────────────────────────────────

fake_src="$STUB/concordvoice.conf"
cat >"$fake_src" <<'EOF'
server_name concordvoice.chat api.concordvoice.chat;
ssl_certificate /etc/letsencrypt/live/concordvoice.chat/fullchain.pem;
EOF

rendered="$STUB/rendered.conf"
( source "$TARGET"
  NGINX_SRC="$fake_src"
  NGINX_OUT="$rendered"
  CFG_DOMAIN=example.com
  render_nginx )

grep -q 'concordvoice\.chat' "$rendered" \
  && fail "rendered still has concordvoice.chat" \
  || pass "domain fully substituted"

grep -q 'server_name example.com api.example.com;' "$rendered" \
  && pass "server_name templated" \
  || fail "server_name not templated"

grep -q '/etc/letsencrypt/live/example.com/fullchain.pem' "$rendered" \
  && pass "cert path templated" \
  || fail "cert path not templated"

# ── Task 7: preflight-before-mutation ordering ────────────────────────────────

# With a bound port, --env-file run must exit 2 and NOT create the .env.
# Reuse the ss stub that returns :8080 as bound (already written above).
rm -f "$STUB/should_not_exist.env"

( source "$TARGET"
  run_preflight() { preflight_ports; }
  PATH="$STUB:$PATH" ENV_OUT="$STUB/should_not_exist.env" main --env-file "$ans" )
rc=$?
[[ $rc -eq 2 && ! -f "$STUB/should_not_exist.env" ]] \
  && pass "preflight aborts before writing .env" \
  || fail "wrote .env despite failed preflight (rc=$rc, file_exists=$(test -f "$STUB/should_not_exist.env" && echo yes || echo no))"

# ── Review hardening: tightened domain regex (#1892 review) ───────────────────

( source "$TARGET"; validate_domain "sub.example.com" ) \
  && pass "validate_domain accepts multi-label domain" \
  || fail "rejected valid multi-label domain"
( source "$TARGET"; validate_domain "a..b.com" ) \
  && fail "validate_domain accepted consecutive-dot domain" \
  || pass "validate_domain rejects consecutive-dot domain"
( source "$TARGET"; validate_domain "-.example.com" ) \
  && fail "validate_domain accepted leading-hyphen label" \
  || pass "validate_domain rejects leading-hyphen label"

# ── Review hardening: TRUSTED_PROXY_CIDRS validation (fail-open guard) ─────────

( source "$TARGET"; validate_cidrs "172.16.0.0/12,10.0.0.0/8" ) \
  && pass "validate_cidrs accepts private CIDR list" \
  || fail "rejected valid CIDR list"
( source "$TARGET"; validate_cidrs "0.0.0.0/0" ) \
  && fail "validate_cidrs accepted 0.0.0.0/0 (fail-open)" \
  || pass "validate_cidrs rejects 0.0.0.0/0"
( source "$TARGET"; validate_cidrs "10.0.0.0/8,0.0.0.0/0" ) \
  && fail "validate_cidrs accepted list containing 0.0.0.0/0" \
  || pass "validate_cidrs rejects list containing 0.0.0.0/0"
( source "$TARGET"; validate_cidrs "::/0" ) \
  && fail "validate_cidrs accepted ::/0" \
  || pass "validate_cidrs rejects ::/0"
( source "$TARGET"; validate_cidrs "" ) \
  && pass "validate_cidrs empty ok (writer defaults)" \
  || fail "rejected empty CIDR"
( source "$TARGET"; validate_cidrs "not-a-cidr" ) \
  && fail "validate_cidrs accepted garbage" \
  || pass "validate_cidrs rejects malformed CIDR"
( source "$TARGET"; validate_cidrs "10.0.0.0/99" ) \
  && fail "validate_cidrs accepted out-of-range IPv4 prefix /99" \
  || pass "validate_cidrs rejects out-of-range IPv4 prefix"
( source "$TARGET"; validate_cidrs "1.2.3/8" ) \
  && fail "validate_cidrs accepted 3-octet IPv4" \
  || pass "validate_cidrs rejects malformed 3-octet IPv4"
( source "$TARGET"; validate_cidrs "fd00::/8" ) \
  && pass "validate_cidrs accepts valid IPv6 CIDR" \
  || fail "rejected valid IPv6 CIDR"

# ── Review hardening: SMTP port validation ────────────────────────────────────

( source "$TARGET"; validate_smtp_port 587 ) \
  && pass "validate_smtp_port accepts 587" \
  || fail "rejected valid port"
( source "$TARGET"; validate_smtp_port abc ) \
  && fail "validate_smtp_port accepted non-numeric" \
  || pass "validate_smtp_port rejects non-numeric"
( source "$TARGET"; validate_smtp_port 99999 ) \
  && fail "validate_smtp_port accepted out-of-range" \
  || pass "validate_smtp_port rejects out-of-range"

# ── Review hardening: secrets prompted with terminal echo OFF ─────────────────

grep -q 'read -rs' "$TARGET" \
  && pass "ask_secret uses silent read (read -rs)" \
  || fail "no silent read for secret prompts"
grep -qE 'CFG_SMTP_PASS="\$\(ask_secret' "$TARGET" \
  && pass "SMTP password prompted via ask_secret" \
  || fail "SMTP password not prompted silently"
grep -qE 'CFG_FEEDBACK_PAT="\$\(ask_secret' "$TARGET" \
  && pass "FEEDBACK_PAT prompted via ask_secret" \
  || fail "FEEDBACK_PAT not prompted silently"

# ── Review hardening: refuse to write secrets where .env is not gitignored ─────

GITREPO="$STUB/gitignore-fixture"
mkdir -p "$GITREPO"
git -C "$GITREPO" init -q >/dev/null 2>&1
write_env_with() {  # helper: run write_env_file with a full bundled config in $GITREPO
  ( source "$TARGET"
    ENV_OUT="$GITREPO/.env"
    git() {
      local target=${@: -1}
      if [[ ${1:-} == -C && ${3:-} == check-ignore && ${4:-} == -q && "$target" == "$GITREPO/.env.new."* ]]; then
        [[ -f "$target" && ! -s "$target" && $(stat -c '%a' "$target") == 600 ]] || return 94
        : >"$STATE/ignored-empty-stage"
      fi
      command git "$@"
    }
    CFG_STORAGE_MODE=bundled
    CFG_DOMAIN=example.com; CFG_PUBLIC_IP=203.0.113.10; CFG_TURN_REALM=example.com
    CFG_SMTP_HOST=smtp.example.com; CFG_SMTP_PORT=587; CFG_SMTP_USER=u; CFG_SMTP_PASS=p
    CFG_SMTP_FROM=noreply@example.com; CFG_TRUSTED_CIDRS='172.16.0.0/12'; CFG_FEEDBACK_ENABLED=no
    PATH="$STUB:$PATH" write_env_file ) >/dev/null 2>&1
}
# .env NOT ignored → must refuse (no file written)
rm -f "$GITREPO/.env"
write_env_with; rc=$?
[[ $rc -ne 0 && ! -f "$GITREPO/.env" ]] \
  && pass "write_env_file refuses when .env not gitignored" \
  || fail "wrote secrets despite .env not gitignored (rc=$rc)"
# Destination-only ignore is insufficient: query a harmless nonexistent stage.
echo '.env' > "$GITREPO/.gitignore"
if git -C "$GITREPO" check-ignore -q "$GITREPO/.env" \
    && ! git -C "$GITREPO" check-ignore -q "$GITREPO/.env.new.ordinary.env"; then
  pass "destination-only ignore does not cover the staging namespace (read-only control)"
else fail "destination-only ignore namespace control is not discriminating"; fi
# Supported checkout policy protects both destination and actual staging family.
cp -- "$ROOT/.gitignore" "$GITREPO/.gitignore"
rm -f -- "$STATE/ignored-empty-stage"
write_env_with
[[ -f "$GITREPO/.env" && $(stat -c '%a' "$GITREPO/.env") == 600 ]] \
  && pass "write_env_file writes when .env is gitignored" \
  || fail "did not write despite canonical environment ignore policy"
[[ -e "$STATE/ignored-empty-stage" ]] \
  && pass "writer verifies ignored empty mode0600 staging before writing bytes" \
  || fail "writer did not verify ignored empty mode0600 staging before writing bytes"
! compgen -G "$GITREPO/.env.new.*" >/dev/null \
  && pass "canonical namespace publication cleans every staging file" \
  || fail "canonical namespace publication retained staging"
rm -rf "$GITREPO"

# ── Task 1617: TLS mode selection ─────────────────────────────────────────────
( source "$TARGET"; validate_tls_mode letsencrypt ) && pass "tls mode letsencrypt ok" || fail "letsencrypt rejected"
( source "$TARGET"; validate_tls_mode local )       && pass "tls mode local ok"       || fail "local rejected"
( source "$TARGET"; validate_tls_mode import )      && pass "tls mode import ok"      || fail "import rejected"
( source "$TARGET"; validate_tls_mode bogus )       && fail "bogus tls mode accepted" || pass "bogus tls mode rejected"

# DR-1: CFG_TLS_MODE must NOT be written into .env (mode is a CLI arg, not config).
TLSREPO="$STUB/tls-fixture"; mkdir -p "$TLSREPO"; git -C "$TLSREPO" init -q >/dev/null 2>&1; echo '.env' > "$TLSREPO/.gitignore"
( source "$TARGET"
  ENV_OUT="$TLSREPO/.env"
  CFG_STORAGE_MODE=bundled
    CFG_DOMAIN=example.com; CFG_PUBLIC_IP=203.0.113.10; CFG_TURN_REALM=example.com
  CFG_SMTP_HOST=smtp.example.com; CFG_SMTP_PORT=587; CFG_SMTP_USER=u; CFG_SMTP_PASS=p
  CFG_SMTP_FROM=noreply@example.com; CFG_TRUSTED_CIDRS='172.16.0.0/12'; CFG_FEEDBACK_ENABLED=no
  CFG_TLS_MODE=local
  PATH="$STUB:$PATH" write_env_file ) >/dev/null 2>&1
grep -qiE '^(TLS_MODE|CERT_MODE)=' "$TLSREPO/.env" 2>/dev/null \
  && fail "TLS mode leaked into .env (DR-1 violation)" || pass "TLS mode not in .env (DR-1)"
rm -rf "$TLSREPO"

# provision_tls dispatches the right provision-cert.sh subcommand per mode.
cat >"$STUB/provision-cert.sh" <<'PCEOF'
#!/usr/bin/env bash
echo "pc $*" >>"$PCLOG"
PCEOF
chmod +x "$STUB/provision-cert.sh"
export PCLOG="$STUB/pc.log"
# letsencrypt is DEFERRED (certbot --webroot needs nginx-on-80 first) — must NOT
# auto-run, only print the command (Gitar #1919 ordering bug).
: >"$PCLOG"; le_out=$( source "$TARGET"; PROVISION_CERT="$STUB/provision-cert.sh" CFG_TLS_MODE=letsencrypt CFG_DOMAIN=example.com CFG_SMTP_FROM=ops@example.com provision_tls 2>&1 )
[[ ! -s "$PCLOG" ]] && pass "provision_tls letsencrypt deferred (no auto-run)" || fail "provision_tls letsencrypt auto-ran ($(cat "$PCLOG"))"
printf '%s' "$le_out" | grep -q 'provision-cert.sh letsencrypt --domain example.com' && pass "provision_tls letsencrypt prints command" || fail "provision_tls letsencrypt no command ($le_out)"
# local is DEFERRED too (#3561): provision-cert.sh local writes /etc/letsencrypt
# and coturn certs and reloads nginx, while the installer runs unprivileged.
: >"$PCLOG"; local_out=$( source "$TARGET"; PROVISION_CERT="$STUB/provision-cert.sh" CFG_TLS_MODE=local CFG_DOMAIN=homelab.lan CFG_PUBLIC_IP=10.0.0.5 provision_tls 2>&1 )
[[ ! -s "$PCLOG" ]] && pass "provision_tls local deferred (no auto-run)" || fail "provision_tls local auto-ran ($(cat "$PCLOG"))"
grep -qF "sudo $STUB/provision-cert.sh local --host homelab.lan --ip 10.0.0.5" <<<"$local_out" \
  && pass "provision_tls local prints the sudo command with --ip" \
  || fail "provision_tls local command missing ($local_out)"
# Both former call sites (with and without --ip) must stay un-run, not just one.
: >"$PCLOG"; local_noip=$( source "$TARGET"; PROVISION_CERT="$STUB/provision-cert.sh" CFG_TLS_MODE=local CFG_DOMAIN=homelab.lan CFG_PUBLIC_IP= provision_tls 2>&1 )
[[ ! -s "$PCLOG" ]] && grep -qF 'provision-cert.sh local --host homelab.lan' <<<"$local_noip" && ! grep -qF -- '--ip' <<<"$local_noip" \
  && pass "provision_tls local omits --ip without a public IP" \
  || fail "provision_tls local no-ip form wrong ($local_noip)"

: >"$PCLOG"
# Main removed the old skip flag. A leftover caller variable must not suppress
# the deferred command or change its output; it never restores an auto-run path.
unused_flag_out="$( source "$TARGET"; PROVISION_CERT="$STUB/provision-cert.sh" SELFHOST_SKIP_CERT=1 CFG_TLS_MODE=letsencrypt CFG_DOMAIN=example.com CFG_SMTP_FROM=ops@example.com provision_tls 2>&1 )"; unused_flag_rc=$?
[[ $unused_flag_rc -eq 0 && ! -s "$PCLOG" && "$unused_flag_out" == "$le_out" ]] \
  && pass "unused caller skip variable preserves deferred TLS instructions and no execution" \
  || fail "unused caller skip variable changed deferred TLS instructions"
# Both local call shapes are deferred even if the benign stub would fail.
printf '%s\n' '#!/usr/bin/env bash' 'printf "pc\n" >>"$PCLOG"' 'exit 7' >"$STUB/provision-cert-fail.sh"
chmod 700 "$STUB/provision-cert-fail.sh"
: >"$PCLOG"
fail_local_out="$( source "$TARGET"; PROVISION_CERT="$STUB/provision-cert-fail.sh" CFG_TLS_MODE=local CFG_DOMAIN=homelab.lan CFG_PUBLIC_IP=10.0.0.5 provision_tls 2>&1 )"; fail_local_rc=$?
[[ $fail_local_rc -eq 0 && ! -s "$PCLOG" ]] && grep -qF 'provision-cert-fail.sh local --host homelab.lan --ip 10.0.0.5' <<<"$fail_local_out" \
  && pass "provision_tls local defers a certificate provisioner that would fail" \
  || fail "provision_tls local executed or obscured a deferred failing provisioner"

# ── #3538: host-bound port posture, version floors, UFW summary ───────────────

# 1. The preflight set is exactly the HOST-BOUND ports (Postgres/Redis/NATS/MinIO
#    have no host socket on self-host, so they must not be required free).
ports_got="$( source "$TARGET"; echo "${REQUIRED_TCP_PORTS[*]}" )"
[[ "$ports_got" == "8080 3000 3478 5349" ]] \
  && pass "REQUIRED_TCP_PORTS is exactly the host-bound set" \
  || fail "REQUIRED_TCP_PORTS wrong: '$ports_got'"

# 2. A host-run Postgres on :5432 must NOT block the install.
PGSTUB="$(mktemp -d)"
cat >"$PGSTUB/ss" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  '-ltn') echo "LISTEN 0 0 0.0.0.0:5432 0.0.0.0:*" ;;
  '-H -a -u -n') ;; # Independent UDP inventory is empty in this TCP-only control.
  *) exit 97 ;;
esac
EOF
chmod +x "$PGSTUB/ss"
( source "$TARGET"; PATH="$PGSTUB:$PATH" preflight_ports ) >/dev/null 2>&1 \
  && pass "preflight_ports ignores a host-run Postgres on 5432" \
  || fail "preflight_ports blocked on 5432 (no longer a required port)"
rm -rf "$PGSTUB"

# Docker stub: version/compose answers come from STUB_ENGINE / STUB_COMPOSE.
DSTUB="$(mktemp -d)"
cat >"$DSTUB/docker" <<'EOF'
#!/usr/bin/env bash
if [[ "${1:-}" == version ]]; then
  printf '%s%s' "${STUB_ENGINE:-}" "${STUB_ENGINE_END-$'\n'}"
  exit "${STUB_ENGINE_EXIT:-0}"
elif [[ "${1:-}" == compose && "${2:-}" == version && "${3:-}" == --short ]]; then
  echo "${STUB_COMPOSE:-}"
elif [[ "${1:-}" == compose && "${2:-}" == version ]]; then
  echo "Docker Compose version ${STUB_COMPOSE:-}"
else
  exit 97
fi
exit 0
EOF
chmod +x "$DSTUB/docker"
# python3 stub so the version cases do not depend on the host having python3.
printf '%s\n' '#!/usr/bin/env bash' 'exit 0' >"$DSTUB/python3"
chmod +x "$DSTUB/python3"

# pf_docker_rc ENGINE COMPOSE — run preflight_docker against the stub, echo its rc.
pf_docker_rc() {
  ( source "$TARGET"; PATH="$DSTUB:$PATH" STUB_ENGINE="$1" STUB_COMPOSE="$2" STUB_ENGINE_EXIT="${3:-0}" STUB_ENGINE_END="${4-$'\n'}" preflight_docker ) >/dev/null 2>&1
  echo $?
}

# 3. Engine security boundary; Compose remains independently fixed at 2.29.1.
for case_ in "27.5.1:2" "28.3.3:2" "29.8.1:2" "29.8.1+dfsg1:2" "29.8.2:0" "29.8.2+dfsg1:0" "29.8.3:0" "30.0.0:0"; do
  ev="${case_%%:*}"; want="${case_##*:}"
  got="$(pf_docker_rc "$ev" "2.29.1")"
  [[ "$got" == "$want" ]] \
    && pass "engine floor: '$ev' -> rc $want" \
    || fail "engine floor: '$ev' gave rc $got, want $want"
done
[[ "$(pf_docker_rc '29.8.2' '2.29.1' 0 '')" == 0 ]] \
  && pass "engine accepts a stable response without terminal LF" \
  || fail "engine refused a stable response without terminal LF"
invalid_engines=("" "garbage" "v29.8.2" "029.8.2" "29.08.2" "29.8.02" "29.8" "29.8.2-rc1" "29.8.2~rc1" "29.8.2+" "29.8.2+bad_metadata" "29.8.2 REGISTRY_SENTINEL" " 29.8.2" "29.8.2 " $'29.8.2\r' $'29.8.2\n' $'29.8.2\n29.8.3')
for ev in "${invalid_engines[@]}"; do
  got="$(pf_docker_rc "$ev" '2.29.1')"
  [[ "$got" == 2 ]] \
    && pass "engine rejects malformed response $(printf %q "$ev")" \
    || fail "engine admitted malformed response $(printf %q "$ev") (rc=$got)"
done
[[ "$(pf_docker_rc '29.8.2' '2.29.1' 42)" == 2 ]] \
  && pass "engine refuses valid-looking stdout from a failed query" \
  || fail "engine accepted valid-looking stdout from a failed query"
engine_msg="$( source "$TARGET"; PATH="$DSTUB:$PATH" STUB_ENGINE=29.8.1 STUB_COMPOSE=2.29.1 preflight_docker 2>&1 )"
if grep -qF 'need >= 29.8.2' <<<"$engine_msg" && grep -qF 'CVE-2026-92543' <<<"$engine_msg" && grep -qF 'CVE-2025-54388' <<<"$engine_msg"; then
  pass "engine refusal names the registry and loopback security reasons"
else
  fail "engine refusal lacks the current floor or security reasons: $engine_msg"
fi
engine_msg="$( source "$TARGET"; PATH="$DSTUB:$PATH" STUB_ENGINE='29.8.2 REGISTRY_SENTINEL' STUB_COMPOSE=2.29.1 preflight_docker 2>&1 )"
if ! grep -qF 'REGISTRY_SENTINEL' <<<"$engine_msg"; then
  pass "engine refusal does not echo malformed daemon output"
else
  fail "engine refusal leaked malformed daemon output"
fi

# 4. Compose floor (Engine fixed at a passing 29.8.2).
for case_ in "2.24.4:2" "2.24.5:0" "v2.24.5:0" "5.5.1:0"; do
  cv="${case_%%:*}"; want="${case_##*:}"
  got="$(pf_docker_rc "29.8.2" "$cv")"
  [[ "$got" == "$want" ]] \
    && pass "compose floor: '$cv' -> rc $want" \
    || fail "compose floor: '$cv' gave rc $got, want $want"
done

# 5. Missing python3: PATH holds ONLY symlinks to the tools preflight_docker
#    needs plus the docker stub, so python3 is genuinely absent.
NOPY="$(mktemp -d)"
for tool in bash grep sort head tr; do
  tool_path="$(command -v "$tool")" && ln -s "$tool_path" "$NOPY/$tool"
done
cp "$DSTUB/docker" "$NOPY/docker"
# PATH is assigned on its OWN statement, not as a prefix to the function call:
# bash 3.2 (stock macOS /bin/bash) does not reset its command hash for a
# temporary PATH assignment in front of a function, so `command -v python3`
# kept resolving through the old PATH and this case passed vacuously there.
( source "$TARGET"; PATH="$NOPY"; STUB_ENGINE="29.8.2" STUB_COMPOSE="2.29.1" preflight_docker ) >/dev/null 2>&1
rc=$?
[[ $rc -eq 2 ]] \
  && pass "preflight_docker aborts (rc 2) when python3 is missing" \
  || fail "preflight_docker rc $rc without python3, want 2"
# Control: the identical PATH with a python3 added must pass, so the rc 2 above is
# attributable to python3 alone and not to a missing tool in NOPY.
NOPY_CTL="$(mktemp -d)"
for tool in bash grep sort head tr; do
  tool_path="$(command -v "$tool")" && ln -s "$tool_path" "$NOPY_CTL/$tool"
done
cp "$DSTUB/docker" "$NOPY_CTL/docker"; cp "$DSTUB/python3" "$NOPY_CTL/python3"
( source "$TARGET"; PATH="$NOPY_CTL"; STUB_ENGINE="29.8.2" STUB_COMPOSE="2.29.1" preflight_docker ) >/dev/null 2>&1
rc=$?
[[ $rc -eq 0 ]] \
  && pass "preflight_docker passes when python3 is present (control for the missing-python3 case)" \
  || fail "preflight_docker control rc $rc with python3 present, want 0"
rm -rf "$NOPY" "$NOPY_CTL" "$DSTUB"

# 5b. #3561 review: preflight_logrotate. The security-event boundary's rotation
# runs through logrotate, which a minimal Debian/Ubuntu host may not have. It
# checks "${LOGROTATE_BIN_OVERRIDE:-/usr/sbin/logrotate}" with -x (macOS has no
# /usr/sbin/logrotate, so every case goes through the override).
LRSTUB="$(mktemp -d)"
printf '%s\n' '#!/usr/bin/env bash' 'exit 0' >"$LRSTUB/logrotate"; chmod +x "$LRSTUB/logrotate"
printf '%s\n' '#!/usr/bin/env bash' 'exit 0' >"$LRSTUB/logrotate-noexec"; chmod -x "$LRSTUB/logrotate-noexec"

# rc exactly 2 (127 = function not defined would satisfy a bare "non-zero" check).
# `&& echo 0 || echo $?` because the sourced script has `set -e` on, which would end
# the subshell at a failing call before a plain `; echo $?` ran.
lr_err="$( source "$TARGET"; LOGROTATE_BIN_OVERRIDE="$LRSTUB/does-not-exist" preflight_logrotate 2>&1 >/dev/null )"
lr_rc=$( source "$TARGET"; LOGROTATE_BIN_OVERRIDE="$LRSTUB/does-not-exist" preflight_logrotate >/dev/null 2>&1 && echo 0 || echo $? )
[[ $lr_rc -eq 2 ]] \
  && pass "preflight_logrotate returns 2 when logrotate is missing" \
  || fail "preflight_logrotate rc=$lr_rc for a missing binary, want 2"
grep -qF 'apt-get install logrotate' <<<"$lr_err" \
  && pass "preflight_logrotate names 'apt-get install logrotate'" \
  || fail "preflight_logrotate missing-binary message lacks the install command ($lr_err)"

lr_err="$( source "$TARGET"; LOGROTATE_BIN_OVERRIDE="$LRSTUB/logrotate-noexec" preflight_logrotate 2>&1 >/dev/null )"
lr_rc=$( source "$TARGET"; LOGROTATE_BIN_OVERRIDE="$LRSTUB/logrotate-noexec" preflight_logrotate >/dev/null 2>&1 && echo 0 || echo $? )
[[ $lr_rc -eq 2 ]] \
  && pass "preflight_logrotate returns 2 when logrotate is not executable" \
  || fail "preflight_logrotate rc=$lr_rc for a non-executable binary, want 2"
grep -qF 'apt-get install logrotate' <<<"$lr_err" \
  && pass "preflight_logrotate non-executable message names the install command" \
  || fail "preflight_logrotate non-executable message lacks the install command ($lr_err)"

lr_rc=$( source "$TARGET"; LOGROTATE_BIN_OVERRIDE="$LRSTUB/logrotate" preflight_logrotate >/dev/null 2>&1 && echo 0 || echo $? )
[[ $lr_rc -eq 0 ]] \
  && pass "preflight_logrotate returns 0 for an executable logrotate" \
  || fail "preflight_logrotate rc=$lr_rc for an executable stub, want 0"

# Wiring: run_preflight must CALL it, after preflight_resources. Every other
# preflight is replaced by a recorder that passes, so the only thing that can fail
# the chain is preflight_logrotate (a sourced function overrides the original).
ORDERLOG="$STUB/preflight-order.log"
run_preflight_with() { # $1 = LOGROTATE_BIN_OVERRIDE; logs the call order, echoes rc
  : >"$ORDERLOG"
  ( source "$TARGET"
    preflight_os()        { printf 'os '        >>"$ORDERLOG"; }
    preflight_docker()    { printf 'docker '    >>"$ORDERLOG"; }
    preflight_resources() { printf 'resources ' >>"$ORDERLOG"; }
    preflight_ports()     { printf 'ports '     >>"$ORDERLOG"; }
    LOGROTATE_BIN_OVERRIDE="$1" run_preflight ) >/dev/null 2>&1
  echo $?
}
rp_rc="$(run_preflight_with "$LRSTUB/does-not-exist")"
[[ "$rp_rc" == 2 ]] \
  && pass "run_preflight aborts (rc 2) when logrotate is missing" \
  || fail "run_preflight rc=$rp_rc with logrotate missing, want 2 (preflight_logrotate not chained?)"
rp_rc="$(run_preflight_with "$LRSTUB/logrotate")"
[[ "$rp_rc" == 0 ]] \
  && pass "run_preflight passes when logrotate is present (control)" \
  || fail "run_preflight control rc=$rp_rc with logrotate present, want 0"
# Order: preflight_logrotate runs after preflight_resources.
: >"$ORDERLOG"
( source "$TARGET"
  preflight_os()        { printf 'os '        >>"$ORDERLOG"; }
  preflight_docker()    { printf 'docker '    >>"$ORDERLOG"; }
  preflight_resources() { printf 'resources ' >>"$ORDERLOG"; }
  preflight_ports()     { printf 'ports '     >>"$ORDERLOG"; }
  preflight_logrotate() { printf 'logrotate ' >>"$ORDERLOG"; }
  run_preflight ) >/dev/null 2>&1
grep -qF 'resources logrotate' "$ORDERLOG" \
  && pass "run_preflight calls preflight_logrotate right after preflight_resources" \
  || fail "run_preflight call order wrong: [$(cat "$ORDERLOG")] (want ... resources logrotate ...)"
rm -rf "$LRSTUB"

# 6. print_summary tells the operator the truth about UFW and the media range.
summary_out="$( source "$TARGET"; NGINX_OUT=/tmp/rendered.conf print_summary 2>&1 )"
grep -qF 'sudo ufw allow 40000:41999/udp' <<<"$summary_out" \
  && pass "print_summary opens the 40000:41999/udp media range" \
  || fail "print_summary lacks 'sudo ufw allow 40000:41999/udp'"
grep -qF 'not filtered by UFW' <<<"$summary_out" \
  && pass "print_summary states Docker-published ports are not filtered by UFW" \
  || fail "print_summary lacks 'not filtered by UFW'"

# 7. #3561: the summary names the security-event log boundary step before `up`.
grep -qF 'selfhost-security-event-logging.sh security-event-logging' <<<"$summary_out" \
  && pass "print_summary names the security-event log boundary step" \
  || fail "print_summary lacks the security-event-logging step"
boundary_line=$(grep -nF 'security-event-logging' <<<"$summary_out" | head -1 | cut -d: -f1)
start_line=$(grep -nF 'Start the stack' <<<"$summary_out" | head -1 | cut -d: -f1)
[[ -n "$boundary_line" && -n "$start_line" && "$boundary_line" -lt "$start_line" ]] \
  && pass "print_summary orders the boundary step before Start the stack" \
  || fail "boundary step (line ${boundary_line:-none}) not before Start the stack (line ${start_line:-none})"
! grep -qF 'provisioned now' <<<"$summary_out" \
  && pass "print_summary no longer claims local TLS is provisioned now" \
  || fail "print_summary still claims 'provisioned now'"

# 3b. #3561 review: the six steps appear in the order the operator must run them.
# The firewall comes before TLS because the ACME challenge needs port 80 open; the
# nginx vhost before the stack; and the log boundary before 'up', which refuses
# without it. Anchored to NUMBERED heading lines so prose mentioning a step name
# elsewhere cannot satisfy the order.
sum_plain="$(sed $'s/\x1b\\[[0-9;]*m//g' <<<"$summary_out")"
step_line() { grep -nE "^[[:space:]]*[0-9]+\\..*$1" <<<"$sum_plain" | head -1 | cut -d: -f1; }
prev=0; order_ok=1; order_desc=''
for step in 'Firewall \(UFW\)' 'TLS certs' 'Install the rendered nginx vhost' \
            'security-event log boundary' 'Start the stack' 'Check health'; do
  ln="$(step_line "$step")"
  order_desc+="[$step=${ln:-none}]"
  [[ -n "$ln" && "$ln" -gt "$prev" ]] || order_ok=0
  prev="${ln:-0}"
done
[[ $order_ok -eq 1 ]] \
  && pass "print_summary orders Firewall, TLS, nginx vhost, log boundary, Start, Check health" \
  || fail "print_summary step order wrong: $order_desc"

# ── #3561: IP validation and literal printed root-command arguments ─
# Production printed commands must keep each benign test value as one argument.
# Finite fixtures include spaces, backslashes, a literal dollar and punctuation.
# No printed command is executed; argv parsing and canonical %q checks are data-only.
INVALID_IP='203.0.113.10 notes'

# 1a. validate_public_ip rejects invalid values with rc 1 EXACTLY (127 = missing
# function would otherwise satisfy a bare "non-zero" check) and never echoes them.
for invalid in "$INVALID_IP" 'address$label'; do
  vp_out="$( source "$TARGET"; validate_public_ip "$invalid" 2>&1 )"; vp_rc=$?
  [[ $vp_rc -eq 1 ]] \
    && pass "validate_public_ip rejects '$invalid' with rc 1" \
    || fail "validate_public_ip rc=$vp_rc for '$invalid', want 1 (out: $vp_out)"
  [[ "$vp_out" != *"$invalid"* && "$vp_out" != *'notes'* && "$vp_out" != *'address$label'* ]] \
    && pass "validate_public_ip does not echo the rejected value" \
    || fail "validate_public_ip echoed the rejected value ($vp_out)"
done
[[ ! -s "$PCLOG" ]] \
  && pass "validate_public_ip rejection causes no privileged fixture effect" \
  || fail "validate_public_ip reached a privileged fixture"

# 1b. Basic self-host PUBLIC_IP is IPv4-only. Keep public and LAN positive
# controls; this restriction does not apply to trusted-proxy IPv6 CIDRs.
for good in 203.0.113.10 10.0.0.5 1.2.3.4; do
  ( source "$TARGET"; validate_public_ip "$good" ) >/dev/null 2>&1; vp_rc=$?
  [[ $vp_rc -eq 0 ]] \
    && pass "validate_public_ip accepts '${good:-"<empty>"}'" \
    || fail "validate_public_ip rc=$vp_rc for '${good:-"<empty>"}', want 0"
done

# 1b-2. These are syntactically valid IPv6 addresses, including IPv4-mapped
# and NAT64 forms. None may enter the IPv4-only publication contract merely
# because it has an embedded dotted IPv4 tail. Refusal must not print the value.
for ipv6 in 2001:db8::1 fd00::5 ::1 '::ffff:192.0.2.1' '64:ff9b::203.0.113.10'; do
  vp_out="$( source "$TARGET"; validate_public_ip "$ipv6" 2>&1 )"; vp_rc=$?
  [[ $vp_rc -eq 1 && "$vp_out" == *'IPv4'* ]] \
    && pass "validate_public_ip refuses unsupported IPv6 $ipv6 with the IPv4 contract diagnostic" \
    || fail "validate_public_ip did not refuse IPv6 $ipv6 with its contract diagnostic (rc=$vp_rc)"
  [[ "$vp_out" != *"$ipv6"* ]] \
    && pass "validate_public_ip IPv6 refusal does not echo the address" \
    || fail "validate_public_ip IPv6 refusal echoed the address"
done

# 1b-3. Rejects, rc exactly 1. 'cafe' is all hex digits with no colon: it is not an
# address. The others are shapes a loose "hex, colon and dot" class would let
# through; IPv4-only admission must retain these existing refusal controls.
for bad in '' 999.1.1.1 ':::' cafe '1.2.3.4;label' 'fe80::1%eth0' '1.2.3.4:' '[2001:db8::1]'; do
  vp_out="$( source "$TARGET"; validate_public_ip "$bad" 2>&1 )"; vp_rc=$?
  [[ $vp_rc -eq 1 ]] \
    && pass "validate_public_ip rejects '$bad' with rc 1" \
    || fail "validate_public_ip rc=$vp_rc for '$bad', want 1 (out: $vp_out)"
done

# 1b-4. detect_public_ip. Its value comes from an external echo service (or the
# PUBLIC_IP override), so a non-IP must be IGNORED, not offered as the wizard's
# default: stdout empty, a stderr note that says "ignoring" (this is not operator
# input, so it must NOT read "invalid public IP"), and nothing evaluated.
DETSTUB="$(mktemp -d)"
cat >"$DETSTUB/curl" <<'CURLEOF'
#!/usr/bin/env bash
printf '%s\n' "$STUB_CURL_OUT"
CURLEOF
chmod +x "$DETSTUB/curl"
detect_with() { # $1 = what the curl stub prints; $2 = PUBLIC_IP override ('' = unset); sets det_out det_err det_rc
  det_out="$( source "$TARGET"; PATH="$DETSTUB:$PATH" STUB_CURL_OUT="$1" PUBLIC_IP="$2" detect_public_ip 2>"$STUB/detect.err" )"
  det_rc=$?
  det_err="$(cat "$STUB/detect.err")"
}

# Positive controls first, so the invalid cases are attributable to the value.
detect_with '203.0.113.10' ''
[[ $det_rc -eq 0 && "$det_out" == '203.0.113.10' && -z "$det_err" ]] \
  && pass "detect_public_ip prints a valid curl-detected IP exactly" \
  || fail "detect_public_ip control: rc=$det_rc out=[$det_out] err=[$det_err]"
detect_with 'ignored-by-override' '198.51.100.7'
[[ $det_rc -eq 0 && "$det_out" == '198.51.100.7' ]] \
  && pass "detect_public_ip honours a valid PUBLIC_IP override" \
  || fail "detect_public_ip override control: rc=$det_rc out=[$det_out]"

detect_with "$INVALID_IP" ''
[[ -z "$det_out" ]] \
  && pass "detect_public_ip: an invalid curl response yields empty stdout" \
  || fail "detect_public_ip: an invalid curl response leaked to stdout [$det_out]"
grep -qF 'ignoring' <<<"$det_err" \
  && pass "detect_public_ip: an invalid curl response is reported as 'ignoring'" \
  || fail "detect_public_ip: stderr lacks 'ignoring' for an invalid curl response [$det_err]"
! grep -qF 'invalid public IP' <<<"$det_err" \
  && pass "detect_public_ip: auto-detected junk is not reported as 'invalid public IP' (operator-input wording)" \
  || fail "detect_public_ip: stderr uses the operator-input message for detected junk [$det_err]"
[[ ! -s "$PCLOG" ]] \
  && pass "detect_public_ip rejection causes no privileged fixture effect" \
  || fail "detect_public_ip reached a privileged fixture"

detect_with '203.0.113.10' "$INVALID_IP"
[[ -z "$det_out" ]] \
  && pass "detect_public_ip: an invalid PUBLIC_IP override yields empty stdout" \
  || fail "detect_public_ip: an invalid PUBLIC_IP override leaked to stdout [$det_out]"
grep -qF 'ignoring' <<<"$det_err" \
  && pass "detect_public_ip: an invalid PUBLIC_IP override is reported as 'ignoring'" \
  || fail "detect_public_ip: stderr lacks 'ignoring' for an invalid PUBLIC_IP override [$det_err]"
! grep -qF 'invalid public IP' <<<"$det_err" \
  && pass "detect_public_ip: a bad PUBLIC_IP override is not reported as 'invalid public IP'" \
  || fail "detect_public_ip: stderr uses the operator-input message for the override [$det_err]"
[[ ! -s "$PCLOG" ]] \
  && pass "detect_public_ip override rejection causes no privileged fixture effect" \
  || fail "detect_public_ip override reached a privileged fixture"

# A detected or overridden IPv6 value must not become an offered wizard
# default. Detection stays advisory and value-free; complete entrypoint tests
# below independently prove the publication refusal and absence of effects.
for ipv6 in 2001:db8::1 fd00::5 '::ffff:192.0.2.1' '64:ff9b::203.0.113.10'; do
  for origin in curl override; do
    if [[ $origin == curl ]]; then detect_with "$ipv6" ''; else detect_with 203.0.113.10 "$ipv6"; fi
    [[ $det_rc -eq 0 && -z "$det_out" ]] \
      && pass "detect_public_ip ignores unsupported IPv6 from $origin without offering a default" \
      || fail "detect_public_ip offered an IPv6 default from $origin (rc=$det_rc)"
    [[ "$det_err" == *ignoring* && "$det_err" != *'invalid public IP'* && "$det_err" != *"$ipv6"* ]] \
      && pass "detect_public_ip IPv6 $origin refusal keeps an advisory value-free diagnostic" \
      || fail "detect_public_ip IPv6 $origin refusal lacks its advisory diagnostic"
  done
done
[[ ! -s "$PCLOG" ]] \
  && pass "detect_public_ip IPv6 refusal causes no privileged fixture effect" \
  || fail "detect_public_ip IPv6 refusal reached a privileged fixture"
rm -rf "$DETSTUB"

# 1c. the GATE: validate_collected_config exits non-zero on an invalid PUBLIC_IP.
# Control first — the same otherwise-valid config with a good IP must pass, so a
# failure below is attributable to CFG_PUBLIC_IP and nothing else.
vcc() { # $1 = CFG_PUBLIC_IP; runs in a subshell because the gate calls exit
  ( source "$TARGET"
    CFG_STORAGE_MODE=bundled
    CFG_DOMAIN=example.com; CFG_TLS_MODE=letsencrypt; CFG_TRUSTED_CIDRS='172.16.0.0/12'
    CFG_SMTP_HOST=smtp.example.invalid; CFG_SMTP_PORT=587; CFG_FEEDBACK_ENABLED=no
    CFG_ACTIVITY_HISTORY_OPERATOR_NAME='Example Ops'
    CFG_ACTIVITY_HISTORY_PRIVACY_POLICY_URL='https://privacy.example.com/policy'
    CFG_PUBLIC_IP="$1"
    validate_collected_config ) >/dev/null 2>&1
}
vcc 203.0.113.10; vcc_rc=$?
[[ $vcc_rc -eq 0 ]] \
  && pass "validate_collected_config accepts a valid config (gate control)" \
  || fail "validate_collected_config control rc=$vcc_rc, want 0 — gate test would be vacuous"
vcc "$INVALID_IP"; vcc_rc=$?
[[ $vcc_rc -ne 0 ]] \
  && pass "validate_collected_config rejects an invalid CFG_PUBLIC_IP" \
  || fail "validate_collected_config accepted invalid CFG_PUBLIC_IP (rc=0)"
for ipv6 in 2001:db8::1 fd00::5 '::ffff:192.0.2.1' '64:ff9b::203.0.113.10'; do
  vcc "$ipv6"; vcc_rc=$?
  [[ $vcc_rc -eq 1 ]] \
    && pass "validate_collected_config refuses unsupported IPv6 $ipv6 through the common input gate" \
    || fail "validate_collected_config accepted IPv6 $ipv6 or failed outside its gate (rc=$vcc_rc)"
done

# Complete entrypoints must reject an absent, malformed or unsupported-family
# address before preflight, renderer, secret generation, environment writer or
# deferred privileged instructions are reached.
# Every case keeps the real input collector, common validation, storage freshness,
# renderer, writer, TLS deferral and summary. Host preflight alone is inert, and
# secret generation returns owned fixture bytes. An unavailable echo service is
# represented by a fail-closed curl stub; no network request is made. The same
# actual publication boundary also covers SMTP and enabled-feedback prerequisites.
fi
public_ip_entry_contract() {
  local entry=$1 ip=$2 expected=$3 status=0 effect
  local setting=${4:-public-ip} smtp_host=smtp.example.invalid feedback_enabled=no
  local storage_mode=${5:-bundled} storage_prompt_count=0
  local domain=${6:-example.com} bucket=${7:-concord-media}
  local storage_endpoint=${8:-objects.example.invalid} storage_tls=${9:-true}
  # Repeated-letter values are owned fixture canaries, never real credentials.
  local byo_access=aaaa byo_auth=bbbb
  local smtp_from=operator@example.invalid
  local feedback_repo=fixture/feedback feedback_pat=fixture-feedback-input
  local trusted_cidrs=172.16.0.0/12 input_path input_metadata template_metadata
  local -a answer_values=() wizard_values=()
  [[ $storage_mode == bundled || $storage_mode == byo-s3 ]] || return 2
  case $setting in
    public-ip|public-ipv6|domain-case|domain-length|bucket-name|storage-endpoint|feedback-disabled|feedback-default) ;;
    smtp-lan) smtp_host=relay.lan ;;
    smtp-relay) smtp_host=mail-relay ;;
    smtp-blank|smtp-missing) smtp_host='' ;;
    smtp-host-whitespace) smtp_host='   ' ;;
    smtp-host-padded) smtp_host=' relay.lan ' ;;
    smtp-host-interior-space) smtp_host='mail relay' ;;
    smtp-host-url) smtp_host=smtp://owned-smtp-host-canary.invalid ;;
    smtp-host-port) smtp_host=relay.lan:587 ;;
    smtp-host-userinfo) smtp_host=operator@relay.lan ;;
    smtp-host-path) smtp_host=relay.lan/path ;;
    smtp-host-invalid-v4) smtp_host=999.1.1.1 ;;
    smtp-host-leading-zero-v4) smtp_host=010.0.0.1 ;;
    smtp-host-numeric) smtp_host=123 ;;
    smtp-host-invalid-v6) smtp_host=::: ;;
    smtp-host-bracketed) smtp_host='[fd00::25]' ;;
    smtp-host-zone) smtp_host=fe80::1%owned0 ;;
    smtp-host-leading-hyphen) smtp_host=-relay.lan ;;
    smtp-host-trailing-hyphen) smtp_host=relay-.lan ;;
    smtp-host-empty-label) smtp_host=relay..lan ;;
    smtp-host-label-overflow)
      printf -v smtp_host '%064d.relay' 0
      smtp_host=${smtp_host//0/a}
      ;;
    smtp-host-total-overflow)
      printf -v smtp_host '%063d.%063d.%063d.%062d' 0 0 0 0
      smtp_host=${smtp_host//0/a}
      ;;
    smtp-host-unicode) smtp_host=$'r\xC3\xA9lay.lan' ;;
    smtp-host-double-root-dot) smtp_host=relay.lan.. ;;
    smtp-host-cr) smtp_host=$'relay.lan\r' ;;
    smtp-host-tab) smtp_host=$'relay\tlan' ;;
    smtp-host-underscore) smtp_host=_smtp.relay_lan ;;
    smtp-host-case-root-dot) smtp_host=MAIL-relay.Example.invalid. ;;
    smtp-host-v4) smtp_host=10.0.0.5 ;;
    smtp-host-v6) smtp_host=fd00::25 ;;
    smtp-host-mapped-v6) smtp_host=::ffff:192.0.2.25 ;;
    smtp-host-peer-boundary) smtp_host=${10:?SMTP peer boundary requires a literal fixture} ;;
    smtp-host-max-unrooted)
      printf -v smtp_host '%063d.%063d.%063d.%061d' 0 0 0 0
      smtp_host=${smtp_host//0/a}
      ;;
    smtp-host-max-rooted)
      printf -v smtp_host '%063d.%063d.%063d.%061d.' 0 0 0 0
      smtp_host=${smtp_host//0/a}
      ;;
    smtp-from-malformed) smtp_from=not-an-address ;;
    smtp-from-whitespace) smtp_from='   ' ;;
    smtp-from-display) smtp_from='Fixture Ops <ops@example.invalid>' ;;
    smtp-from-quoted) smtp_from='"ops"@example.invalid' ;;
    smtp-from-comment) smtp_from='ops@example.invalid (fixture)' ;;
    smtp-from-list) smtp_from=ops@example.invalid,other@example.invalid ;;
    smtp-from-unicode) smtp_from=$'op\xC3\xA9@example.invalid' ;;
    smtp-from-padded) smtp_from=' ops@example.invalid ' ;;
    smtp-from-other-atom) smtp_from=ops!fixture@example.invalid ;;
    smtp-from-extra-at) smtp_from=ops@@example.invalid ;;
    smtp-from-empty-local) smtp_from=@example.invalid ;;
    smtp-from-empty-domain) smtp_from=ops@ ;;
    smtp-from-local-leading-dot) smtp_from=.ops@example.invalid ;;
    smtp-from-local-trailing-dot) smtp_from=ops.@example.invalid ;;
    smtp-from-local-consecutive-dot) smtp_from=ops..fixture@example.invalid ;;
    smtp-from-domain-leading-dot) smtp_from=ops@.example.invalid ;;
    smtp-from-domain-trailing-dot) smtp_from=ops@example.invalid. ;;
    smtp-from-domain-consecutive-dot) smtp_from=ops@example..invalid ;;
    smtp-from-bare) smtp_from=ops+install@example.invalid ;;
    smtp-from-default) smtp_from='' ;;
    smtp-from-dot) smtp_from=ops.tag@example.invalid ;;
    smtp-from-lan-atoms) smtp_from=Ops+tag_1%route-2@relay ;;
    smtp-from-hyphen-domain) smtp_from=ops@-mail-.lan ;;
    feedback-enabled) feedback_enabled=yes ;;
    feedback-enabled-near) feedback_enabled=yes; feedback_repo=selfhost/disabled-extra ;;
    feedback-reserved-lower) feedback_enabled=yes; feedback_repo=selfhost/disabled ;;
    feedback-reserved-upper) feedback_enabled=yes; feedback_repo=SELFHOST/DISABLED ;;
    feedback-reserved-mixed) feedback_enabled=yes; feedback_repo=SelfHost/Disabled ;;
    feedback-reserved-ascii) feedback_enabled=yes; feedback_repo=' selfhost/disabled ' ;;
    # Explicit UTF-8 bytes keep U+2003 EM SPACE independent of the shell locale.
    feedback-reserved-emspace) feedback_enabled=yes; feedback_repo=$'\xE2\x80\x83selfhost/disabled\xE2\x80\x83' ;;
    feedback-mode-invalid) feedback_enabled=true ;;
    feedback-mode-empty) feedback_enabled='' ;;
    feedback-repo-blank|feedback-repo-missing) feedback_enabled=yes; feedback_repo='' ;;
    feedback-pat-blank|feedback-pat-missing) feedback_enabled=yes; feedback_pat='' ;;
    feedback-repo-no-slash) feedback_enabled=yes; feedback_repo=fixture-feedback-repository-canary ;;
    feedback-repo-empty-owner) feedback_enabled=yes; feedback_repo=/feedback ;;
    feedback-repo-empty-name) feedback_enabled=yes; feedback_repo=fixture/ ;;
    feedback-repo-extra-slash) feedback_enabled=yes; feedback_repo=fixture/feedback/extra ;;
    feedback-repo-interior-space) feedback_enabled=yes; feedback_repo='fixture /feedback' ;;
    feedback-repo-query-delimiter) feedback_enabled=yes; feedback_repo='fixture/feedback?x' ;;
    feedback-repo-fragment-delimiter) feedback_enabled=yes; feedback_repo='fixture/feedback#x' ;;
    feedback-repo-escaped-separator) feedback_enabled=yes; feedback_repo='fixture/feedback%2Fother' ;;
    feedback-repo-owner-dot-segment) feedback_enabled=yes; feedback_repo='./feedback' ;;
    feedback-repo-owner-parent-segment) feedback_enabled=yes; feedback_repo='../feedback' ;;
    feedback-repo-name-dot-segment) feedback_enabled=yes; feedback_repo='fixture/.' ;;
    feedback-repo-name-parent-segment) feedback_enabled=yes; feedback_repo='fixture/..' ;;
    feedback-repo-space-only) feedback_enabled=yes; feedback_repo='   ' ;;
    feedback-repo-emspace-only) feedback_enabled=yes; feedback_repo=$'\xE2\x80\x83' ;;
    feedback-pat-space-only) feedback_enabled=yes; feedback_pat='   ' ;;
    feedback-pat-emspace-only) feedback_enabled=yes; feedback_pat=$'\xE2\x80\x83' ;;
    feedback-enabled-short) feedback_enabled=yes; feedback_repo=a/b ;;
    feedback-enabled-punctuation) feedback_enabled=yes; feedback_repo=org.with.dots/repo_with-mix.123 ;;
    feedback-enabled-trimmed)
      feedback_enabled=yes
      feedback_repo=$' \xE2\x80\x83fixture/feedback\xE2\x80\x83 '
      feedback_pat=$' \xE2\x80\x83fixture-feedback-input\xE2\x80\x83 '
      ;;
    feedback-enabled-unicode-interior) feedback_enabled=yes; feedback_repo=$'fixture\xE2\x80\x83owner/feedback' ;;
    # Go strings.TrimSpace leaves these four characters intact. They are a
    # nonempty opaque PAT, unlike Python str.strip's broader whitespace set.
    feedback-enabled-control-pat) feedback_enabled=yes; feedback_pat=$'\x1c\x1d\x1e\x1f' ;;
    cidr-invalid-octets) trusted_cidrs=999.1.1.1/24 ;;
    cidr-leading-zero-octets) trusted_cidrs=010.0.0.1/24 ;;
    cidr-invalid-compression) trusted_cidrs=:::/64 ;;
    cidr-invalid-address) trusted_cidrs=deadbeef/64 ;;
    cidr-interior-space) trusted_cidrs='10.0 .0.1/24' ;;
    cidr-invalid-v4-prefix) trusted_cidrs=10.0.0.1/33 ;;
    cidr-invalid-v6-prefix) trusted_cidrs=2001:db8::1/129 ;;
    cidr-zone) trusted_cidrs=fe80::1%fixture0/64 ;;
    cidr-dotted-mask) trusted_cidrs=10.0.0.1/255.255.255.0 ;;
    cidr-plus-prefix) trusted_cidrs=10.0.0.1/+24 ;;
    cidr-minus-prefix) trusted_cidrs=10.0.0.1/-1 ;;
    cidr-nonascii-prefix) trusted_cidrs=$'10.0.0.1/\xD9\xA2\xD9\xA4' ;;
    cidr-space-only) trusted_cidrs='   ' ;;
    cidr-comma-only) trusted_cidrs=' , , ' ;;
    cidr-all-v4-alias) trusted_cidrs=10.0.0.1/0 ;;
    cidr-all-v6-alias) trusted_cidrs=::1/0 ;;
    cidr-all-mapped-canonical) trusted_cidrs=::ffff:0.0.0.0/96 ;;
    cidr-all-mapped-host-bits) trusted_cidrs=::ffff:192.0.2.1/96 ;;
    cidr-all-mapped-hex-alias) trusted_cidrs=0:0:0:0:0:FFFF:C000:0201/00096 ;;
    cidr-all-mapped-list) trusted_cidrs=172.16.0.0/12,::ffff:192.0.2.1/96 ;;
    cidr-union-all-v4) trusted_cidrs=0.0.0.0/1,128.0.0.0/1 ;;
    cidr-union-all-mapped) trusted_cidrs=::ffff:0.0.0.0/97,::ffff:128.0.0.0/97 ;;
    cidr-union-all-mixed) trusted_cidrs=0.0.0.0/1,::ffff:128.0.0.0/97 ;;
    cidr-union-all-v4-order-hostbits)
      trusted_cidrs=' 128.0.0.1/1,0.0.0.7/1,128.0.0.5/2,0.0.0.3/1 '
      ;;
    cidr-union-all-v6) trusted_cidrs=::/1,8000::/1 ;;
    cidr-union-all-v6-except-mapped)
      # Independent interval fixture: these 96 CIDRs cover [::, ::fffe:ffff:ffff]
      # and [::1:0:0:0, ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff]. The only
      # omitted block is mapped IPv4 /96, so every effective native IPv6 peer
      # is trusted. Powers construct the intervals, without a collapse oracle.
      trusted_cidrs=$(python3 -c 'from ipaddress import IPv6Address
upper = [f"{IPv6Address(1 << (128 - prefix))}/{prefix}" for prefix in range(1, 81)]
lower = (
    "::/81", "::8000:0:0/82", "::c000:0:0/83", "::e000:0:0/84",
    "::f000:0:0/85", "::f800:0:0/86", "::fc00:0:0/87", "::fe00:0:0/88",
    "::ff00:0:0/89", "::ff80:0:0/90", "::ffc0:0:0/91", "::ffe0:0:0/92",
    "::fff0:0:0/93", "::fff8:0:0/94", "::fffc:0:0/95", "::fffe:0:0/96",
)
assert len(upper) + len(lower) == 96
print(",".join((*upper, *lower)))
'
      ) || return 1
      ;;
    cidr-union-restricted-v4) trusted_cidrs=0.0.0.0/1,128.0.0.0/2 ;;
    cidr-union-restricted-mapped) trusted_cidrs=::ffff:0.0.0.0/97,::ffff:128.0.0.0/98 ;;
    cidr-union-overlap-duplicate) trusted_cidrs=' 10.0.0.9/8,10.1.0.5/16,10.0.0.9/8 ' ;;
    cidr-union-restricted-v6) trusted_cidrs=::/1,8000::/2 ;;
    cidr-union-one-gap-v4)
      # Exactly 32 power intervals cover [0.0.0.1, 255.255.255.255], leaving
      # one IPv4 address untrusted. This near-complete union must stay admitted.
      trusted_cidrs=$(python3 -c 'from ipaddress import IPv4Address
prefixes = [f"{IPv4Address(1 << (32 - prefix))}/{prefix}" for prefix in range(1, 33)]
assert len(prefixes) == 32
print(",".join(prefixes))
'
      ) || return 1
      ;;
    cidr-union-one-gap-v6)
      # Exactly 128 power intervals cover [::1, ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff].
      # The omitted :: is a genuine native IPv6 peer, preserving a real gap.
      trusted_cidrs=$(python3 -c 'from ipaddress import IPv6Address
prefixes = [f"{IPv6Address(1 << (128 - prefix))}/{prefix}" for prefix in range(1, 129)]
assert len(prefixes) == 128
print(",".join(prefixes))
'
      ) || return 1
      ;;
    cidr-valid-v4-host-bits) trusted_cidrs=10.0.0.17/24 ;;
    cidr-valid-v6-host-bits) trusted_cidrs=2001:db8::17/64 ;;
    cidr-valid-v4-zero-prefix) trusted_cidrs=10.0.0.17/032 ;;
    cidr-valid-v6-zero-prefix) trusted_cidrs=2001:db8::17/00128 ;;
    cidr-valid-mixed) trusted_cidrs='10.0.0.17/32,::1/128,::ffff:192.0.2.1/128' ;;
    cidr-valid-mapped-97) trusted_cidrs=::ffff:192.0.2.1/97 ;;
    cidr-valid-mapped-120) trusted_cidrs=::ffff:192.0.2.1/120 ;;
    cidr-valid-trimmed-empty-tokens) trusted_cidrs=$' ,\xE2\x80\x8310.0.0.17/24\xE2\x80\x83,, 2001:db8::17/64 , ' ;;
    *) return 2 ;;
  esac
  reset_task3
  rm -f -- "$STATE/ip-detection-calls" "$STATE/ip-generation" "$STATE/ip-privileged-effect"
  : >"$PCLOG"
  mkdir -p -- "$STATE/ip-bin"
  cat >"$STATE/ip-bin/curl" <<'IPCURL'
#!/usr/bin/env bash
if [[ $# -ne 4 || $1 != -fsS || $2 != --max-time || $3 != 5 || $4 != https://api.ipify.org ]]; then
  : >"$STATE/rejected"
  exit 97
fi
printf 'unavailable\n' >>"$STATE/ip-detection-calls"
exit 22
IPCURL
  cat >"$STATE/ip-bin/privileged" <<'IPPRIVILEGED'
#!/usr/bin/env bash
: >"$STATE/ip-privileged-effect"
: >"$STATE/rejected"
exit 97
IPPRIVILEGED
  chmod 700 "$STATE/ip-bin/curl" "$STATE/ip-bin/privileged"
  for effect in sudo ufw systemctl; do
    cp -- "$STATE/ip-bin/privileged" "$STATE/ip-bin/$effect"
  done
  export PATH="$STATE/ip-bin:$PATH"
  unset PUBLIC_IP
  source "$TARGET"
  if [[ $STORAGE_INSTALLER_P2_ONLY == true || $SMTP_NONPEER_ONLY == true ]]; then
    python3() { command python3 -S "$@"; }
  fi
  ENV_OUT="$FIX/.env"; NGINX_OUT="$STATE/rendered"
  PROVISION_CERT="$STUB/provision-cert.sh"
  run_preflight() { printf 'preflight\n' >>"$STATE/events"; }
  gen_secret() {
    : >"$STATE/ip-generation"
    printf '%s' aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  }
  gen_hex_key() {
    : >"$STATE/ip-generation"
    printf '%s' bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  }
  source <(declare -f print_storage_review | sed '1s/print_storage_review/ip_fixture_review/')
  print_storage_review() { printf 'review\n' >>"$STATE/events"; ip_fixture_review "$@"; }
  source <(declare -f render_nginx | sed '1s/render_nginx/ip_fixture_render/')
  render_nginx() { printf 'render\n' >>"$STATE/events"; ip_fixture_render "$@"; }
  source <(declare -f write_env_file | sed '1s/write_env_file/ip_fixture_writer/')
  write_env_file() { printf 'writer\n' >>"$STATE/events"; ip_fixture_writer "$@"; }
  source <(declare -f provision_tls | sed '1s/provision_tls/ip_fixture_tls/')
  provision_tls() { printf 'tls\n' >>"$STATE/events"; ip_fixture_tls "$@"; }
  source <(declare -f print_summary | sed '1s/print_summary/ip_fixture_summary/')
  print_summary() { printf 'summary\n' >>"$STATE/events"; ip_fixture_summary "$@"; }
  cp -p -- "$FIX/infrastructure/deploy/nginx/concordvoice.conf" "$STATE/ip-template-snapshot" || return 1
  template_metadata=$(stat -c '%a:%u:%g:%s:%i:%y:%z' "$FIX/infrastructure/deploy/nginx/concordvoice.conf") || return 1
  if [[ $entry == answers ]]; then
    answer_values=("CFG_DOMAIN=$domain" 'CFG_TLS_MODE=local' 'CFG_TURN_REALM=example.com'
      "CFG_PUBLIC_IP=$ip" "CFG_STORAGE_MODE=$storage_mode" 'CFG_SMTP_PORT=587'
      "CFG_SMTP_FROM=$smtp_from" "CFG_TRUSTED_CIDRS=$trusted_cidrs")
    if [[ $storage_mode == byo-s3 ]]; then
      answer_values+=('CFG_STORAGE_BACKEND=s3' "CFG_STORAGE_ENDPOINT=$storage_endpoint"
        'CFG_STORAGE_REGION=' "CFG_STORAGE_USE_SSL=$storage_tls" "CFG_STORAGE_BUCKET=$bucket"
        "CFG_STORAGE_ACCESS_KEY=$byo_access" "CFG_STORAGE_SECRET_KEY=$byo_auth")
    fi
    [[ $setting == feedback-default ]] || answer_values+=("CFG_FEEDBACK_ENABLED=$feedback_enabled")
    [[ $setting == smtp-missing ]] || answer_values+=("CFG_SMTP_HOST=$smtp_host")
    if [[ $feedback_enabled == yes ]]; then
      [[ $setting == feedback-repo-missing ]] || answer_values+=("CFG_FEEDBACK_REPO=$feedback_repo")
      [[ $setting == feedback-pat-missing ]] || answer_values+=("CFG_FEEDBACK_PAT=$feedback_pat")
    fi
    printf '%s\n' "${answer_values[@]}" >"$STATE/ip-answers"
    input_path="$STATE/ip-answers"
    chmod 600 "$input_path" || return 1
    cp -p -- "$input_path" "$STATE/ip-input-snapshot" || return 1
    input_metadata=$(stat -c '%a:%u:%g:%s:%i:%y:%z' "$input_path") || return 1
    main --env-file "$STATE/ip-answers" >"$STATE/ip-entry-output" 2>&1 || status=$?
    [[ ! -e "$STATE/ip-detection-calls" ]] || return 1
  else
    wizard_values=("$domain" local example.com "$ip" "$storage_mode")
    if [[ $storage_mode == byo-s3 ]]; then
      wizard_values+=("$storage_endpoint" "$storage_tls" "$bucket" '' "$byo_access" "$byo_auth")
      storage_prompt_count=6
    fi
    wizard_values+=("$smtp_host" 587 fixture-user fixture-password "$smtp_from" "$trusted_cidrs" '' '' "$feedback_enabled")
    [[ $setting != feedback-default ]] || wizard_values[$((13 + storage_prompt_count))]=''
    if [[ $feedback_enabled == yes ]]; then wizard_values+=("$feedback_repo" "$feedback_pat"); fi
    # An omitted interactive answer is EOF at its actual prompt; blank cells
    # keep the rest of the otherwise-valid wizard input complete.
    case $setting in
      smtp-missing) wizard_values=("${wizard_values[@]:0:$((5 + storage_prompt_count))}") ;;
      feedback-repo-missing) wizard_values=("${wizard_values[@]:0:$((14 + storage_prompt_count))}") ;;
      feedback-pat-missing) wizard_values=("${wizard_values[@]:0:$((15 + storage_prompt_count))}") ;;
    esac
    printf '%s\n' "${wizard_values[@]}" >"$STATE/ip-stdin"
    input_path="$STATE/ip-stdin"
    chmod 600 "$input_path" || return 1
    cp -p -- "$input_path" "$STATE/ip-input-snapshot" || return 1
    input_metadata=$(stat -c '%a:%u:%g:%s:%i:%y:%z' "$input_path") || return 1
    main <"$STATE/ip-stdin" >"$STATE/ip-entry-output" 2>&1 || status=$?
    if [[ $setting == domain-length && $expected == refuse && -f "$FIX/.env" ]] \
      && grep -qFx "DOMAIN=\"$domain\"" "$FIX/.env"; then
      printf '%s\n' 'DOMAIN_LENGTH_ORACLE: unsupported domain was durably published' >&2
      return 1
    fi
    if [[ ( $setting == domain-case || $setting == domain-length ) && $expected == refuse ]]; then
      [[ ! -e "$STATE/ip-detection-calls" ]] || return 1
    else
      [[ $(cat "$STATE/ip-detection-calls") == unavailable ]] || return 1
    fi
  fi
  cmp -s -- "$input_path" "$STATE/ip-input-snapshot" || return 1
  [[ $(stat -c '%a:%u:%g:%s:%i:%y:%z' "$input_path") == "$input_metadata" ]] || return 1
  cmp -s -- "$FIX/infrastructure/deploy/nginx/concordvoice.conf" "$STATE/ip-template-snapshot" || return 1
  [[ $(stat -c '%a:%u:%g:%s:%i:%y:%z' "$FIX/infrastructure/deploy/nginx/concordvoice.conf") == "$template_metadata" ]] || return 1
  [[ ! -e "$STATE/rejected" && ! -e "$STATE/ip-privileged-effect" && ! -s "$PCLOG" ]] || return 1
  ! compgen -G "$FIX/.env.new.*" >/dev/null || return 1
  ! grep -qE 'command not found|unbound variable|syntax error|Traceback|AssertionError' "$STATE/ip-entry-output" || return 1
  ! grep -qF 'fixture-feedback-input' "$STATE/ip-entry-output" || return 1
  ! grep -qF 'fixture-feedback-repository-canary' "$STATE/ip-entry-output" || return 1
  ! grep -qF 'owned-smtp-host-canary' "$STATE/ip-entry-output" || return 1
  ! grep -qF "$byo_auth" "$STATE/ip-entry-output" || return 1
  if [[ $expected == refuse ]]; then
    if [[ $setting == feedback-repo-owner-dot-segment || $setting == feedback-repo-owner-parent-segment || $setting == feedback-repo-name-dot-segment || $setting == feedback-repo-name-parent-segment ]] \
      && [[ -f "$FIX/.env" ]] && grep -qFx "FEEDBACK_REPO=\"$feedback_repo\"" "$FIX/.env"; then
      printf '%s\n' 'FEEDBACK_DOT_SEGMENT_ORACLE: URL path-normalizing segment was durably published as a repository slug' >&2
      return 1
    fi
    if [[ $setting == feedback-repo-query-delimiter || $setting == feedback-repo-fragment-delimiter || $setting == feedback-repo-escaped-separator ]] \
      && [[ -f "$FIX/.env" ]] && grep -qFx "FEEDBACK_REPO=\"$feedback_repo\"" "$FIX/.env"; then
      printf '%s\n' 'FEEDBACK_REPO_ORACLE: URL syntax was durably published as a repository slug' >&2
      return 1
    fi
    if [[ $setting == domain-length && -f "$FIX/.env" ]] \
      && grep -qFx "DOMAIN=\"$domain\"" "$FIX/.env"; then
      printf '%s\n' 'DOMAIN_LENGTH_ORACLE: unsupported domain was durably published' >&2
      return 1
    fi
    if [[ $setting == smtp-host-peer-boundary && -f "$FIX/.env" ]] \
      && grep -qFx "SMTP_HOST=\"$smtp_host\"" "$FIX/.env"; then
      printf '%s\n' 'SMTP_NONPEER_ORACLE: non-peer SMTP literal was durably published' >&2
      return 1
    fi
    if [[ $setting == storage-endpoint && -f "$FIX/.env" ]] \
      && grep -qFx "STORAGE_ENDPOINT=\"$storage_endpoint\"" "$FIX/.env" \
      && grep -qFx "STORAGE_USE_SSL=\"$storage_tls\"" "$FIX/.env"; then
      printf '%s\n' 'ENDPOINT_ORACLE: refused endpoint was durably published' >&2
      return 1
    fi
    [[ $status -eq 1 && ! -e "$FIX/.env" && ! -L "$FIX/.env" && ! -e "$STATE/rendered"
       && ! -e "$STATE/ip-generation" && ! -s "$STATE/events" ]] || return 1
    [[ -s "$STATE/ip-entry-output" ]] || return 1
    if [[ $setting == public-ip || $setting == public-ipv6 ]]; then
      if [[ $setting == public-ipv6 ]]; then
        grep -qF 'IPv4' "$STATE/ip-entry-output" || return 1
      else
        grep -qF 'invalid public IP' "$STATE/ip-entry-output" || return 1
      fi
      [[ -z $ip ]] || ! grep -qF -- "$ip" "$STATE/ip-entry-output"
    else
      case $entry:$setting in
        *:domain-case|*:domain-length)
          grep -qFx 'installer: invalid domain' "$STATE/ip-entry-output" || return 1
          ! grep -qF "$domain" "$STATE/ip-entry-output" || return 1
          ;;
        *:storage-endpoint)
          grep -qFx 'selfhost-storage: invalid storage configuration' "$STATE/ip-entry-output" || return 1
          ! grep -qF -- "$storage_endpoint" "$STATE/ip-entry-output" || return 1
          ;;
        *:bucket-name)
          grep -qFx 'selfhost-storage: invalid storage configuration' "$STATE/ip-entry-output" || return 1
          if [[ $bucket == ab ]]; then
            # This short value occurs inside fixed homelab/routable warnings.
            # A complete token still detects disclosure of the supplied bucket.
            ! grep -qFw -- "$bucket" "$STATE/ip-entry-output" || return 1
          else
            ! grep -qF "$bucket" "$STATE/ip-entry-output" || return 1
          fi
          ;;
        interactive:*-missing) ;; # Actual read EOF has no dedicated diagnostic.
        answers:smtp-host-cr) grep -qFx 'installer: invalid answers encoding' "$STATE/ip-entry-output" || return 1 ;;
        interactive:smtp-host-cr|*:smtp-host-tab)
          grep -qFx 'installer: invalid configuration text' "$STATE/ip-entry-output" || return 1
          ;;
        *:smtp-host-*) grep -qFx 'installer: invalid SMTP host' "$STATE/ip-entry-output" || return 1 ;;
        *:smtp-from-*) grep -qFx 'installer: invalid SMTP sender' "$STATE/ip-entry-output" || return 1 ;;
        *:smtp-*) grep -qFx 'installer: SMTP host is required' "$STATE/ip-entry-output" || return 1 ;;
        *:feedback-reserved-*)
          grep -qFx 'installer: disabled feedback repository is reserved' "$STATE/ip-entry-output" || return 1
          ! grep -qF "$feedback_repo" "$STATE/ip-entry-output" || return 1
          ;;
        *:feedback-mode-invalid|*:feedback-mode-empty)
          grep -qFx 'installer: invalid feedback mode (yes|no)' "$STATE/ip-entry-output" || return 1
          ;;
        *:feedback-repo-no-slash|*:feedback-repo-empty-owner|*:feedback-repo-empty-name|*:feedback-repo-extra-slash|*:feedback-repo-interior-space|*:feedback-repo-query-delimiter|*:feedback-repo-fragment-delimiter|*:feedback-repo-escaped-separator|*:feedback-repo-owner-dot-segment|*:feedback-repo-owner-parent-segment|*:feedback-repo-name-dot-segment|*:feedback-repo-name-parent-segment)
          grep -qFx 'installer: invalid feedback repository' "$STATE/ip-entry-output" || return 1
          ! grep -qF "$feedback_repo" "$STATE/ip-entry-output" || return 1
          ;;
        *:feedback-*)
          grep -qFx 'installer: enabled feedback requires a repository and PAT' "$STATE/ip-entry-output" || return 1
          ! grep -qF 'fixture/feedback' "$STATE/ip-entry-output" || return 1
          ;;
        *:cidr-all-*|*:cidr-union-all-*)
          grep -qF 'TRUSTED_PROXY_CIDRS must not include 0.0.0.0/0 or ::/0' "$STATE/ip-entry-output" || return 1
          ;;
        *:cidr-*) grep -qFx 'installer: invalid trusted proxy CIDR' "$STATE/ip-entry-output" || return 1 ;;
      esac
    fi
  else
    [[ $status -eq 0 && -f "$FIX/.env" && $(stat -c '%a' "$FIX/.env") == 600
       && -f "$STATE/rendered" && -e "$STATE/ip-generation" ]] || return 1
    [[ $(cat "$STATE/events") == $'preflight\nreview\nrender\nwriter\ntls\nsummary' ]] || return 1
    grep -qFx "PUBLIC_IP=\"$ip\"" "$FIX/.env" || return 1
    grep -qFx "DOMAIN=\"$domain\"" "$FIX/.env" || return 1
    grep -qFx "SMTP_HOST=\"$smtp_host\"" "$FIX/.env" || return 1
    grep -qFx "SMTP_FROM=\"${smtp_from:-noreply@example.com}\"" "$FIX/.env" || return 1
    grep -qFx "TRUSTED_PROXY_CIDRS=\"$trusted_cidrs\"" "$FIX/.env" || return 1
    grep -qFx "SELFHOST_STORAGE_MODE=$storage_mode" "$FIX/.env" || return 1
    if [[ $storage_mode == byo-s3 ]]; then
      grep -qFx 'STORAGE_BACKEND="s3"' "$FIX/.env" || return 1
      grep -qFx "STORAGE_ENDPOINT=\"$storage_endpoint\"" "$FIX/.env" || return 1
      grep -qFx "STORAGE_USE_SSL=\"$storage_tls\"" "$FIX/.env" || return 1
      grep -qFx "STORAGE_BUCKET=\"$bucket\"" "$FIX/.env" || return 1
      grep -qFx "STORAGE_ACCESS_KEY=\"$byo_access\"" "$FIX/.env" || return 1
      grep -qFx "STORAGE_SECRET_KEY=\"$byo_auth\"" "$FIX/.env" || return 1
    fi
    if [[ $feedback_enabled == yes ]]; then
      grep -qFx "FEEDBACK_REPO=\"$feedback_repo\"" "$FIX/.env" || return 1
      grep -qFx "FEEDBACK_PAT=\"$feedback_pat\"" "$FIX/.env" || return 1
    else
      grep -qFx 'FEEDBACK_REPO="selfhost/disabled"' "$FIX/.env" || return 1
      grep -qFx 'FEEDBACK_PAT="disabled-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"' "$FIX/.env" || return 1
    fi
    grep -qF "server_name $domain;" "$STATE/rendered"
  fi
}
domain_length_validator_contract() {
  local domain=$1 expected=$2 status=0
  source "$TARGET" || return 1
  validate_domain "$domain" >"$STATE/domain-validator-output" 2>&1 || status=$?
  if [[ $expected == accept ]]; then
    [[ $status -eq 0 && ! -s "$STATE/domain-validator-output" ]]
  elif [[ $status -eq 0 ]]; then
    printf '%s\n' 'DOMAIN_LENGTH_ORACLE: validator accepted an unsupported generated DNS name' >&2
    return 1
  else
    [[ $status -eq 1 && $(cat "$STATE/domain-validator-output") == 'installer: invalid domain' ]]
  fi
}
domain_boundary_contracts() {
  local entry domain label63 middle63 tld63 label64 middle64 tld64 max_apex overflow_apex full_apex hyphen63
  # Regression for PR #3584, native thread PRRT_kwDORQf_wc6p2mDQ.
  # Independent DNS wire contract: labels <=63; generated apex, api., media.,
  # turn. and spa. names <=253. The longest actual prefix is media. (6 bytes),
  # so a 247-byte apex is supported and a 248-byte apex is not.
  printf -v label63 '%063d' 0; label63=${label63//0/a}
  # Avoid the preexisting BYO-auth canary bbbb in the public configuration
  # preview: the no-disclosure assertion remains active for every control.
  printf -v middle63 '%063d' 0; middle63=${middle63//0/e}
  printf -v tld63 '%063d' 0; tld63=${tld63//0/c}
  label64=${label63}a; middle64=${middle63}e; tld64=${tld63}c
  printf -v domain '%055d' 0; domain=${domain//0/d}
  max_apex=$label63.$middle63.$tld63.$domain
  overflow_apex=${max_apex}d
  printf -v domain '%061d' 0; domain=${domain//0/d}
  full_apex=$label63.$middle63.$tld63.$domain
  printf -v hyphen63 '%061d' 0; hyphen63=a${hyphen63//0/-}z
  [[ ${#max_apex} -eq 247 && ${#overflow_apex} -eq 248 && ${#full_apex} -eq 253 ]] \
    || { fail 'domain fixture length prerequisite'; return; }
  for entry in validator answers interactive; do
    for domain in example.com "$label63.example" "safe.$middle63.example" "safe.$tld63" \
      "$hyphen63.example" "$max_apex"; do
      if [[ $entry == validator ]]; then
        contract "domain validator preserves supported label/apex bytes at ${#domain} bytes" \
          domain_length_validator_contract "$domain" accept
      else
        contract "domain complete $entry publishes supported label/apex bytes at ${#domain} bytes" \
          public_ip_entry_contract "$entry" 10.0.0.5 accept domain-length bundled "$domain"
      fi
    done
    for domain in "$label64.example" "safe.$middle64.example" "safe.$tld64" "$overflow_apex" "$full_apex"; do
      if [[ $entry == validator ]]; then
        contract "domain validator refuses overlong label/generated service at ${#domain} apex bytes" \
          domain_length_validator_contract "$domain" refuse
      else
        contract "domain complete $entry refuses overlong label/generated service at ${#domain} apex bytes before durable publication" \
          public_ip_entry_contract "$entry" 10.0.0.5 refuse domain-length bundled "$domain"
      fi
    done
  done
  if command python3 "$HERE/selfhost_domain_boundary_test.py" "$UDP_SOURCE_ROOT"; then
    pass 'standalone certificate modes preserve DNS label and derived service bounds'
  else
    fail 'standalone certificate modes accepted unsupported DNS label or derived service bounds'
  fi
}
smtp_nonpeer_consumer_contract() {
  local entry=$1 host=$2 expected=$3 status=0
  if [[ $entry != shared-validator ]]; then
    public_ip_entry_contract "$entry" 10.0.0.5 "$expected" smtp-host-peer-boundary \
      bundled example.com concord-media objects.example.invalid true "$host"
    return $?
  fi
  reset_task3
  source "$TARGET" || return 1
  python3() { command python3 -S "$@"; }
  validate_smtp_host "$host" >"$STATE/smtp-validator-output" 2>&1 || status=$?
  if [[ $expected == accept ]]; then
    [[ $status -eq 0 && ! -s "$STATE/smtp-validator-output" ]]
  elif [[ $status -eq 0 ]]; then
    printf '%s\n' 'SMTP_NONPEER_ORACLE: shared validator accepted a non-peer SMTP literal' >&2
    return 1
  else
    [[ $status -eq 1 && $(cat "$STATE/smtp-validator-output") == 'installer: invalid SMTP host' ]]
  fi
}
smtp_nonpeer_contracts() {
  local entry host
  # Regression for PR #3584, native review root 4204355268.
  # Oracle: Unspecified, multicast, and IPv4 limited-broadcast SMTP literals, including IPv4-mapped forms, must refuse before durable environment publication, while ordinary unicast LAN IPs and Go-compatible hostnames retain their literal bytes and publish.
  for entry in shared-validator answers interactive; do
    # Local/LAN and adjacent unicast controls prevent is_private or a final-255
    # shortcut from replacing the narrow peer policy; spelling must survive.
    for host in 10.0.0.5 172.16.0.5 192.168.1.5 127.0.0.1 ::1 fd00::25 \
      ::ffff:10.0.0.5 ::ffff:127.0.0.1 223.255.255.255 240.0.0.1 \
      255.255.255.254 192.168.1.255 relay.lan mail-relay _smtp.relay_lan \
      MAIL-relay.Example.invalid.; do
      contract "SMTP $entry admits unicast or Go-compatible host $host with literal bytes preserved" \
        smtp_nonpeer_consumer_contract "$entry" "$host" accept
    done
    # Native IPv4/IPv6, both multicast boundaries, and dotted/hex mapped
    # representations all follow the same actual publication consumers.
    for host in 0.0.0.0 :: 0:0:0:0:0:0:0:0 224.0.0.0 239.255.255.255 \
      ff00:: ff02::1 ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff 255.255.255.255 \
      ::ffff:0.0.0.0 ::ffff:0:0 ::ffff:224.0.0.0 ::ffff:e000:0 \
      ::ffff:239.255.255.255 ::ffff:255.255.255.255 ::ffff:ffff:ffff; do
      contract "SMTP $entry refuses non-peer literal $host before durable environment publication" \
        smtp_nonpeer_consumer_contract "$entry" "$host" refuse
    done
  done
}
consumer_input_contracts() {
  local entry setting
  # These tables follow Config.Load's actual trimmed feedback inputs and
  # parseTrustedProxyCIDRs/net.ParseCIDR semantics, rather than a second slug
  # or address parser. Every row reaches the complete production collector.
  for entry in answers interactive; do
    for setting in feedback-repo-no-slash feedback-repo-empty-owner feedback-repo-empty-name \
      feedback-repo-extra-slash feedback-repo-interior-space feedback-repo-query-delimiter \
      feedback-repo-fragment-delimiter feedback-repo-escaped-separator feedback-repo-space-only \
      feedback-repo-owner-dot-segment feedback-repo-owner-parent-segment \
      feedback-repo-name-dot-segment feedback-repo-name-parent-segment \
      feedback-repo-emspace-only feedback-pat-space-only feedback-pat-emspace-only; do
      contract "complete $entry main refuses consumer-invalid $setting before secrets, rendering or publication" \
        public_ip_entry_contract "$entry" 10.0.0.5 refuse "$setting"
    done
    for setting in feedback-enabled-short feedback-enabled-punctuation feedback-enabled-trimmed \
      feedback-enabled-unicode-interior feedback-enabled-control-pat; do
      contract "complete $entry main preserves consumer-valid $setting publication" \
        public_ip_entry_contract "$entry" 10.0.0.5 accept "$setting"
    done
    for setting in cidr-invalid-octets cidr-leading-zero-octets cidr-invalid-compression \
      cidr-invalid-address cidr-interior-space cidr-invalid-v4-prefix cidr-invalid-v6-prefix \
      cidr-zone cidr-dotted-mask cidr-plus-prefix cidr-minus-prefix cidr-nonascii-prefix \
      cidr-space-only cidr-comma-only cidr-all-v4-alias cidr-all-v6-alias \
      cidr-all-mapped-canonical cidr-all-mapped-host-bits cidr-all-mapped-hex-alias cidr-all-mapped-list; do
      contract "complete $entry main refuses consumer-invalid $setting before secrets, rendering or publication" \
        public_ip_entry_contract "$entry" 10.0.0.5 refuse "$setting"
    done
    for setting in cidr-valid-v4-host-bits cidr-valid-v6-host-bits cidr-valid-v4-zero-prefix \
      cidr-valid-v6-zero-prefix cidr-valid-mixed cidr-valid-trimmed-empty-tokens \
      cidr-valid-mapped-97 cidr-valid-mapped-120; do
      contract "complete $entry main preserves consumer-valid $setting publication" \
        public_ip_entry_contract "$entry" 10.0.0.5 accept "$setting"
    done
    # Go IPNet.Contains projects mapped CIDRs into IPv4 peer coverage. Complete
    # effective-family unions must refuse even when each individual CIDR is
    # restricted. Each admission independently preserves its raw input bytes.
    for setting in cidr-union-all-v4 cidr-union-all-mapped cidr-union-all-mixed \
      cidr-union-all-v4-order-hostbits cidr-union-all-v6 cidr-union-all-v6-except-mapped; do
      contract "complete $entry main refuses all-traffic $setting before secrets, rendering or publication" \
        public_ip_entry_contract "$entry" 10.0.0.5 refuse "$setting"
    done
    for setting in cidr-union-restricted-v4 cidr-union-restricted-mapped cidr-union-overlap-duplicate \
      cidr-union-restricted-v6 cidr-union-one-gap-v4 cidr-union-one-gap-v6; do
      contract "complete $entry main preserves restricted $setting bytes through publication" \
        public_ip_entry_contract "$entry" 10.0.0.5 accept "$setting"
    done
    # A representative external-storage pair reaches the same common guard;
    # storage mode must not change the trust-union admission policy.
    contract "complete $entry BYO main refuses an all-traffic mixed-family IPv4 union before effects" \
      public_ip_entry_contract "$entry" 10.0.0.5 refuse cidr-union-all-mixed byo-s3
    contract "complete $entry BYO main preserves a restricted mapped IPv4 union and owned storage bytes" \
      public_ip_entry_contract "$entry" 10.0.0.5 accept cidr-union-restricted-mapped byo-s3
    # SMTP_FROM is shared by net/mail.ParseAddress and raw Certbot --email.
    # Pin their supported bare ASCII subset without rewriting admitted bytes;
    # deliberately keep local/LAN domains and hyphen-shaped labels admitted.
    for setting in smtp-from-malformed smtp-from-whitespace smtp-from-display smtp-from-quoted \
      smtp-from-comment smtp-from-list smtp-from-unicode smtp-from-padded smtp-from-other-atom \
      smtp-from-extra-at smtp-from-empty-local smtp-from-empty-domain \
      smtp-from-local-leading-dot smtp-from-local-trailing-dot smtp-from-local-consecutive-dot \
      smtp-from-domain-leading-dot smtp-from-domain-trailing-dot smtp-from-domain-consecutive-dot; do
      contract "complete $entry main refuses $setting before secrets, rendering or publication" \
        public_ip_entry_contract "$entry" 10.0.0.5 refuse "$setting"
    done
    for setting in smtp-from-bare smtp-from-default smtp-from-dot smtp-from-lan-atoms smtp-from-hyphen-domain; do
      contract "complete $entry main preserves legitimate $setting publication" \
        public_ip_entry_contract "$entry" 10.0.0.5 accept "$setting"
    done
    # The SMTP consumer joins a raw host with a separate port. These are
    # syntactic admissions, with no live DNS, STARTTLS or delivery assertion.
    for setting in smtp-host-whitespace smtp-host-padded smtp-host-interior-space smtp-host-url \
      smtp-host-port smtp-host-userinfo smtp-host-path smtp-host-invalid-v4 smtp-host-leading-zero-v4 \
      smtp-host-numeric smtp-host-invalid-v6 smtp-host-bracketed smtp-host-zone \
      smtp-host-leading-hyphen smtp-host-trailing-hyphen smtp-host-empty-label \
      smtp-host-label-overflow smtp-host-total-overflow smtp-host-unicode smtp-host-double-root-dot \
      smtp-host-cr smtp-host-tab; do
      contract "complete $entry main refuses $setting before secrets, rendering or publication" \
        public_ip_entry_contract "$entry" 10.0.0.5 refuse "$setting"
    done
    for setting in smtp-lan smtp-relay smtp-host-underscore smtp-host-case-root-dot \
      smtp-host-v4 smtp-host-v6 smtp-host-mapped-v6 smtp-host-max-unrooted smtp-host-max-rooted; do
      contract "complete $entry main preserves supported $setting bytes through publication" \
        public_ip_entry_contract "$entry" 10.0.0.5 accept "$setting"
    done
  done
}
feedback_boundary_contracts() {
  local entry setting
  for entry in answers interactive; do
    for setting in feedback-enabled feedback-enabled-near feedback-disabled feedback-default; do
      contract "complete $entry main preserves legitimate $setting publication" \
        public_ip_entry_contract "$entry" 10.0.0.5 accept "$setting"
    done
    for setting in feedback-reserved-lower feedback-reserved-upper feedback-reserved-mixed \
      feedback-reserved-ascii feedback-reserved-emspace feedback-mode-invalid; do
      contract "complete $entry main refuses $setting before secrets, rendering or environment publication" \
        public_ip_entry_contract "$entry" 10.0.0.5 refuse "$setting"
    done
  done
  contract 'complete answers main refuses an explicitly empty feedback mode before secrets, rendering or environment publication' \
    public_ip_entry_contract answers 10.0.0.5 refuse feedback-mode-empty
}
storage_endpoint_consumer_contract() {
  local entry=$1 endpoint=$2 tls=$3 expected=$4 status=0
  if [[ $entry != shared-validator ]]; then
    public_ip_entry_contract "$entry" 10.0.0.5 "$expected" storage-endpoint byo-s3 example.com concord-media "$endpoint" "$tls"
    return $?
  fi
  # Exercise the shared production parser, including its safe refusal message;
  # the same remaining fields are valid in every control and refusal cell.
  source "$FIX/infrastructure/deploy/selfhost-storage.sh" || return 1
  python3() { command python3 -S "$@"; }
  selfhost_storage_validate byo-s3 s3 "$endpoint" '' aaaa bbbb "$tls" concord-media \
    >"$STATE/storage-validator-output" 2>&1 || status=$?
  if [[ $expected == accept ]]; then
    [[ $status -eq 0 && ! -s "$STATE/storage-validator-output" ]]
  elif [[ $status -eq 0 ]]; then
    printf '%s\n' 'ENDPOINT_ORACLE: shared validator accepted refused endpoint' >&2
    return 1
  else
    [[ $status -eq 1 && $(cat "$STATE/storage-validator-output") == 'selfhost-storage: invalid storage configuration' ]]
  fi
}
storage_endpoint_contracts() {
  local entry endpoint tls consumer positive_action positive_tail
  # Regression for PR #3584, native thread PRRT_kwDORQf_wc6py8kQ.
  # Oracle: Native IPv4/IPv6 multicast, IPv4 limited-broadcast, and IPv4-mapped equivalents refuse before durable install publication irrespective of TLS; valid public, private TLS, and explicitly authorized loopback peers still reach the same installer publication boundary.
  for entry in shared-validator answers interactive; do
    consumer=$entry
    [[ $entry == shared-validator ]] || consumer="complete $entry"
    positive_action=publishes; positive_tail=' unchanged'
    [[ $entry != shared-validator ]] || { positive_action=accepts; positive_tail=''; }
    for endpoint in objects.example.invalid LOCALHOST localhost. MINIO 127.0.0.1:9000 \
      10.0.0.1 192.168.1.1 '[::1]:9000' '[fc00::1]' '[::ffff:10.0.0.1]' \
      '[::ffff:127.0.0.1]:9000' '[2001:db8::1]:443' 203.0.113.1:443 \
      223.255.255.255:9000 240.0.0.1:9000 '[::ffff:203.0.113.1]:443'; do
      contract "storage $consumer TLS true $positive_action supported endpoint $endpoint$positive_tail" \
        storage_endpoint_consumer_contract "$entry" "$endpoint" true accept
    done
    for endpoint in minio localhost 127.0.0.1 10.0.0.1 '[::1]' '[fc00::1]' \
      '[::ffff:10.0.0.1]' '[::ffff:127.0.0.1]:9000'; do
      contract "storage $consumer TLS false $positive_action supported private endpoint $endpoint$positive_tail" \
        storage_endpoint_consumer_contract "$entry" "$endpoint" false accept
    done
    for endpoint in 0.0.0.0 0.0.0.0:9000 '[::]' '[0:0:0:0:0:0:0:0]:443' \
      169.254.1.1 169.254.255.254:9000 '[fe80::1]' '[febf:ffff::1]:443' \
      '[::ffff:0.0.0.0]' '[::ffff:0:0]:9000' '[::ffff:169.254.1.1]' '[::ffff:a9fe:101]:443'; do
      for tls in true false; do
        contract "storage $consumer TLS $tls refuses unusable endpoint $endpoint before publication" \
          storage_endpoint_consumer_contract "$entry" "$endpoint" "$tls" refuse
      done
    done
    # Pin both range boundaries and both mapped wire spellings. TLS protects a
    # transport; it cannot turn multicast or limited broadcast into a host peer.
    for endpoint in 224.0.0.0 239.255.255.255:9000 255.255.255.255:443 \
      '[ff00::]' '[ff02::1]:9000' '[ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff]:443' \
      '[::ffff:224.0.0.0]:9000' '[::ffff:e000:0]' \
      '[::ffff:239.255.255.255]' '[::ffff:efff:ffff]:443' \
      '[::ffff:255.255.255.255]:9000' '[::ffff:ffff:ffff]'; do
      for tls in true false; do
        contract "storage $consumer TLS $tls refuses multicast or limited-broadcast endpoint $endpoint before durable publication" \
          storage_endpoint_consumer_contract "$entry" "$endpoint" "$tls" refuse
      done
    done
  done
}
storage_installer_p2_contracts() {
  local entry ip domain bucket max_bucket long_bucket
  printf -v max_bucket '%063d' 0; max_bucket=${max_bucket//0/a}
  long_bucket=${max_bucket}a
  for entry in answers interactive; do
    for ip in 10.0.0.5 172.16.0.5 192.168.1.5 127.0.0.1 169.254.1.5 203.0.113.10; do
      contract "P2 complete $entry main preserves explicit host IPv4 $ip" \
        public_ip_entry_contract "$entry" "$ip" accept
    done
    # Do not turn host admission into an Internet reachability or is_global check.
    for ip in 0.0.0.0 224.0.0.1 239.255.255.255 255.255.255.255; do
      contract "P2 complete $entry main refuses non-host IPv4 $ip before effects" \
        public_ip_entry_contract "$entry" "$ip" refuse
    done
    for domain in example.com sub.example.com; do
      contract "P2 complete $entry main publishes canonical lowercase DOMAIN $domain" \
        public_ip_entry_contract "$entry" 10.0.0.5 accept domain-case bundled "$domain"
    done
    for domain in EXAMPLE.COM Example.com example.COM Media.example.com; do
      contract "P2 complete $entry main refuses uppercase DOMAIN $domain before effects" \
        public_ip_entry_contract "$entry" 10.0.0.5 refuse domain-case bundled "$domain"
    done
    # minio-go/v7 v7.3.0 s3utils.CheckValidBucketName is the NON-strict consumer:
    # uppercase, underscores and colons are accepted. Preserve its full grammar.
    for bucket in abc "$max_bucket" Concord-Media concord_media concord:media concord.media; do
      contract "P2 complete $entry BYO main preserves client-valid bucket $bucket" \
        public_ip_entry_contract "$entry" 10.0.0.5 accept bucket-name byo-s3 example.com "$bucket"
    done
    for bucket in ab "$long_bucket" 'concord media' concord/media concord..media \
      concord.-media concord-.media -concord concord- .concord concord. 192.0.2.1 999.1.1.1 'concord+media' 'concord@media' 'concord,media' 'média'; do
      contract "P2 complete $entry BYO main refuses client-invalid bucket $bucket before effects" \
        public_ip_entry_contract "$entry" 10.0.0.5 refuse bucket-name byo-s3 example.com "$bucket"
    done
  done
  storage_endpoint_contracts
}
if [[ $DOMAIN_BOUNDARY_ONLY == true ]]; then
  domain_boundary_contracts
  [[ $fails -eq 0 ]] && { echo 'ALL DOMAIN BOUNDARIES PASS'; exit 0; }
  echo "$fails DOMAIN BOUNDARY FAILURES"; exit 1
fi
if [[ $SMTP_NONPEER_ONLY == true ]]; then
  smtp_nonpeer_contracts
  [[ $fails -eq 0 ]] && { echo 'ALL SMTP NON-PEER BOUNDARIES PASS'; exit 0; }
  echo "$fails SMTP NON-PEER BOUNDARY FAILURES"; exit 1
fi
if [[ $STORAGE_ENDPOINT_ONLY == true ]]; then
  storage_endpoint_contracts
  [[ $fails -eq 0 ]] && { echo 'ALL STORAGE ENDPOINT BOUNDARIES PASS'; exit 0; }
  echo "$fails STORAGE ENDPOINT BOUNDARY FAILURES"; exit 1
fi
if [[ $STORAGE_INSTALLER_P2_ONLY == true ]]; then
  storage_installer_p2_contracts
  [[ $fails -eq 0 ]] && { echo 'ALL STORAGE/INSTALLER P2 BOUNDARIES PASS'; exit 0; }
  echo "$fails STORAGE/INSTALLER P2 BOUNDARY FAILURES"; exit 1
fi
if [[ $FEEDBACK_ONLY == true ]]; then
  if [[ $CONSUMER_ONLY == true ]]; then
    consumer_input_contracts
    [[ $fails -eq 0 ]] && { echo 'ALL INSTALLER CONSUMER INPUT BOUNDARIES PASS'; exit 0; }
    echo "$fails INSTALLER CONSUMER INPUT BOUNDARY FAILURES"; exit 1
  fi
  feedback_boundary_contracts
  [[ $fails -eq 0 ]] && { echo 'ALL FEEDBACK INPUT BOUNDARIES PASS'; exit 0; }
  echo "$fails FEEDBACK INPUT BOUNDARY FAILURES"; exit 1
fi
domain_boundary_contracts
storage_installer_p2_contracts
smtp_nonpeer_contracts
feedback_boundary_contracts
consumer_input_contracts
for entry in answers interactive; do
  for good in 10.0.0.5 203.0.113.10; do
    contract "complete $entry main accepts explicit IPv4 address $good despite unavailable detection" \
      public_ip_entry_contract "$entry" "$good" accept
  done
  for bad in '' 999.1.1.1 ':::'; do
    contract "complete $entry main refuses ${bad:-blank IP} before render, publication or privileged effects" \
      public_ip_entry_contract "$entry" "$bad" refuse
  done
  for ipv6 in 2001:db8::1 fd00::5 ::1 '::ffff:192.0.2.1' '64:ff9b::203.0.113.10'; do
    contract "complete $entry main refuses IPv6 $ipv6 before preflight, rendering, secrets or publication" \
      public_ip_entry_contract "$entry" "$ipv6" refuse public-ipv6
  done
done

# Missing and blank publication prerequisites are distinct answer-file cells.
# The wizard has matching complete blank-input cells and separate EOF controls.
# Disabled feedback remains installable with neither operator feedback field.
for entry in answers interactive; do
  for setting in smtp-lan smtp-relay feedback-enabled feedback-disabled; do
    contract "complete $entry main publishes valid $setting settings through the real writer" \
      public_ip_entry_contract "$entry" 10.0.0.5 accept "$setting"
  done
  for setting in smtp-missing smtp-blank feedback-repo-missing feedback-repo-blank feedback-pat-missing feedback-pat-blank; do
    contract "complete $entry main refuses $setting before render, publication or privileged effects" \
      public_ip_entry_contract "$entry" 10.0.0.5 refuse "$setting"
  done
done

# Data-only argv decoder for benign printable %q fixtures. Nothing is evaluated.
printed_command() { # stdin: text; $1 = fixed-string selector
  local line
  line="$(sed $'s/\x1b\\[[0-9;]*m//g' | grep -F 'sudo ' | grep -F -- "$1" | head -1)"
  line="${line#*sudo }"
  printf '%s' "${line%%' && '*}"
}
pasted_argv() {
  local line
  line="$(printed_command "$1")" || return 1
  python3 - "$line" <<'SAFEARGV'
import shlex, sys
for value in shlex.split(sys.argv[1], posix=True):
    print(value)
SAFEARGV
}
quoted_command_eq() { # $1 = actual printed line; remaining args = exact argv
  local actual=$1 expected='' arg quoted; shift
  for arg in "$@"; do
    printf -v quoted '%q' "$arg"
    expected+="${expected:+ }$quoted"
  done
  [[ "$actual" == "$expected" ]]
}

argv_eq() { # $1 = got (multi-line); remaining args = expected elements
  local got="$1"; shift
  [[ "$got" == "$(printf '%s\n' "$@")" ]]
}

# 2a. provision_tls local: a space-bearing script path stays one printed argument.
PCSPACE="$STUB/dir with space/provision-cert.sh"
local_inj="$( source "$TARGET"; PROVISION_CERT="$PCSPACE" CFG_TLS_MODE=local CFG_DOMAIN=homelab.lan CFG_PUBLIC_IP=203.0.113.10 provision_tls 2>&1 )"
got="$(pasted_argv 'local' <<<"$local_inj")"
argv_eq "$got" "$PCSPACE" local --host homelab.lan --ip 203.0.113.10 \
  && pass "provision_tls local: pasted line parses to the intended argv" \
  || fail "provision_tls local: pasted argv wrong: [$(tr '\n' '|' <<<"$got")]"
quoted_command_eq "$(printed_command 'local' <<<"$local_inj")" "$PCSPACE" local --host homelab.lan --ip 203.0.113.10 \
  && pass "printed local command preserves canonical shell quoting" \
  || fail "printed local command changed literal argument encoding"
[[ ! -s "$PCLOG" ]] \
  && pass "printed local command has no privileged fixture effect" \
  || fail "printed local command reached the privileged fixture"

# 2b. provision_tls letsencrypt — the --email value comes from CFG_SMTP_FROM.
LITERAL_EMAIL='Example Ops <ops@example.invalid> $label'
le_inj="$( source "$TARGET"; PROVISION_CERT="$PCSPACE" CFG_TLS_MODE=letsencrypt CFG_DOMAIN=example.com CFG_SMTP_FROM="$LITERAL_EMAIL" provision_tls 2>&1 )"
got="$(pasted_argv 'letsencrypt' <<<"$le_inj")"
argv_eq "$got" "$PCSPACE" letsencrypt --domain example.com --email "$LITERAL_EMAIL" \
  && pass "provision_tls letsencrypt: pasted line parses to the intended argv" \
  || fail "provision_tls letsencrypt: pasted argv wrong: [$(tr '\n' '|' <<<"$got")]"
quoted_command_eq "$(printed_command 'letsencrypt' <<<"$le_inj")" "$PCSPACE" letsencrypt --domain example.com --email "$LITERAL_EMAIL" \
  && pass "printed letsencrypt command preserves canonical shell quoting" \
  || fail "printed letsencrypt command changed literal argument encoding"
[[ ! -s "$PCLOG" ]] \
  && pass "printed letsencrypt command has no privileged fixture effect" \
  || fail "printed letsencrypt command reached the privileged fixture"

# 2c. provision_tls import — the printed line is pasteable as-is: no <cert.pem> /
# <key.pem> placeholders (a shell reads <x> as a redirect and the paste fails).
# The PEM paths are the documented ones from provision-cert.sh's own usage.
: >"$PCLOG"
imp_inj="$( source "$TARGET"; PROVISION_CERT="$PCSPACE" CFG_TLS_MODE=import CFG_DOMAIN=homelab.lan provision_tls 2>&1 )"
[[ ! -s "$PCLOG" ]] \
  && pass "provision_tls import prints only (nothing is run)" \
  || fail "provision_tls import auto-ran ($(cat "$PCLOG"))"
got="$(pasted_argv 'import' <<<"$imp_inj")"
argv_eq "$got" "$PCSPACE" import --cert /path/to/fullchain.pem --key /path/to/privkey.pem --host homelab.lan \
  && pass "provision_tls import: pasted line parses to the intended argv (no placeholders)" \
  || fail "provision_tls import: pasted argv wrong: [$(tr '\n' '|' <<<"$got")]"

# 3. print_summary with a checkout path containing a space. SCRIPT_DIR is
# readonly and derived from BASH_SOURCE, so source a COPY placed at that path.
CLONE="$STUB/clone dir"
mkdir -p "$CLONE/infrastructure/deploy"
fixture_repo "$CLONE"
NGX_SPACE="$STUB/out dir/rendered.conf"
sum_space="$( source "$CLONE/infrastructure/deploy/install-selfhost.sh"; NGINX_OUT="$NGX_SPACE" print_summary 2>&1 )"
got="$(pasted_argv 'security-event-logging' <<<"$sum_space")"
argv_eq "$got" "$CLONE/infrastructure/deploy/selfhost-security-event-logging.sh" security-event-logging \
  && pass "print_summary boundary step: pasted line parses to the intended argv" \
  || fail "print_summary boundary step: pasted argv wrong: [$(tr '\n' '|' <<<"$got")]"
got="$(pasted_argv 'sudo cp' <<<"$sum_space")"
argv_eq "$got" cp "$NGX_SPACE" /etc/nginx/conf.d/ \
  && pass "print_summary nginx step: 'sudo cp' line parses to the intended argv" \
  || fail "print_summary nginx step: pasted argv wrong: [$(tr '\n' '|' <<<"$got")]"
rm -rf "$CLONE"

# 3c. print_summary with a path holding BOTH a backslash and a space. shq turns a
# backslash into '\\', and an `echo -e` would collapse that back to one backslash
# (changing the path the root shell runs), so the printed lines must survive intact.
CLONE_BS="$STUB/clone\\dir x"
mkdir -p "$CLONE_BS/infrastructure/deploy"
fixture_repo "$CLONE_BS"
NGX_BS="$STUB/out\\dir x/rendered.conf"
sum_bs="$( source "$CLONE_BS/infrastructure/deploy/install-selfhost.sh"; NGINX_OUT="$NGX_BS" print_summary 2>&1 )"
got="$(pasted_argv 'security-event-logging' <<<"$sum_bs")"
argv_eq "$got" "$CLONE_BS/infrastructure/deploy/selfhost-security-event-logging.sh" security-event-logging \
  && pass "print_summary: boundary line survives a backslash-and-space checkout path" \
  || fail "print_summary: boundary argv wrong for a backslash path: [$(tr '\n' '|' <<<"$got")]"
got="$(pasted_argv 'sudo cp' <<<"$sum_bs")"
argv_eq "$got" cp "$NGX_BS" /etc/nginx/conf.d/ \
  && pass "print_summary: nginx 'sudo cp' line survives a backslash-and-space path" \
  || fail "print_summary: nginx cp argv wrong for a backslash path: [$(tr '\n' '|' <<<"$got")]"
rm -rf "$CLONE_BS"

# 3d. A benign semicolon-and-space rendered-vhost path stays exactly one argument.
# The printed text is never executed.
NGX_INJ="$STUB/out dir/report;archive"
sum_inj="$( source "$TARGET"; NGINX_OUT="$NGX_INJ" print_summary 2>&1 )"
got="$(pasted_argv 'sudo cp' <<<"$sum_inj")"
argv_eq "$got" cp "$NGX_INJ" /etc/nginx/conf.d/ \
  && pass "print_summary: nginx 'sudo cp' line keeps a ';'-bearing path as one argument" \
  || fail "print_summary: nginx cp argv wrong for a ';' path: [$(tr '\n' '|' <<<"$got")]"
quoted_command_eq "$(printed_command 'sudo cp' <<<"$sum_inj")" cp "$NGX_INJ" /etc/nginx/conf.d/ \
  && pass "printed sudo cp command preserves canonical shell quoting" \
  || fail "printed sudo cp command changed literal argument encoding"
[[ ! -s "$PCLOG" ]] \
  && pass "printed sudo cp command has no privileged fixture effect" \
  || fail "printed sudo cp command reached the privileged fixture"

# Task 3 behavior requires the independently accepted Task 2 helper. Absence is
# an integration prerequisite failure, never evidence of the installer feature RED.
if [[ ${1:-} != --base-only ]]; then
  if [[ -f "$FIX/infrastructure/deploy/selfhost-storage.sh" ]]; then
    task3_contracts
  else
    fail "Task 3 behavior pending accepted shared-helper integration"
  fi
fi
[[ ! -e "$STATE/rejected" ]] || fail "unexpected Docker request invalidated fixture"
udp_preflight_contracts
# ── Summary ───────────────────────────────────────────────────────────────────

echo ""
[[ $fails -eq 0 ]] && { echo "ALL PASS"; exit 0; } || { echo "$fails FAILED"; exit 1; }
