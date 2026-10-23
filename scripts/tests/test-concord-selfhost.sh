#!/usr/bin/env bash
# Unit tests for infrastructure/deploy/concord-selfhost.
# PATH-stubs docker so no real daemon is needed.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
SOURCE_TARGET="$ROOT/infrastructure/deploy/concord-selfhost"
BASE_ONLY=0
case "${1:-}" in
  --base-only) BASE_ONLY=1 ;;
  "") ;;
  *) printf '%s\n' 'Usage: test-concord-selfhost.sh [--base-only]' >&2; exit 2 ;;
esac

STUB="$(mktemp -d)"
trap 'rm -rf "$STUB" 2>/dev/null || true' EXIT

# #3561: `up` refuses unless the security-event log boundary root exists. Point
# every existing `up` case at a real temp dir so they keep exercising delegation.
export CONCORD_SECURITY_LOG_ROOT_OVERRIDE="$STUB/security-log-root"
mkdir "$STUB/security-log-root"

fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; fails=$((fails + 1)); }

# Every command is redirected before a copied wrapper/helper can run. No test
# executes from the real repository root or loads its default environment.
SIMPLE_ROOT="$STUB/wrapper-root"
mkdir -p "$SIMPLE_ROOT/infrastructure/deploy"
TARGET="$SIMPLE_ROOT/infrastructure/deploy/concord-selfhost"
cp "$SOURCE_TARGET" "$TARGET"
STORAGE_HELPER="$ROOT/infrastructure/deploy/selfhost-storage.sh"
if [[ ! -f "$STORAGE_HELPER" ]]; then
  printf '%s\n' 'FAIL: prerequisite missing: accepted selfhost-storage helper' >&2
  exit 1
fi
cp "$STORAGE_HELPER" "$SIMPLE_ROOT/infrastructure/deploy/selfhost-storage.sh"
printf 'INSTANCE_TYPE=self-hosted\n' >"$SIMPLE_ROOT/.env"
touch "$SIMPLE_ROOT/docker-compose.yml" "$SIMPLE_ROOT/docker-compose.production.yml" \
  "$SIMPLE_ROOT/docker-compose.selfhost.yml"
export SELFHOST_STUB_REJECT_LOG="$STUB/docker-reject.log"
: >"$SELFHOST_STUB_REJECT_LOG"
export ACTIVE_DOCKER_STUB="$STUB/docker" SIMPLE_DOCKER_LOG="$STUB/docker.log"
export SIMPLE_ROOT
docker() { "$ACTIVE_DOCKER_STUB" "$@"; }
export -f docker

# ── Fixture: Docker rejects every unrecognized call with exit 97 ────────
cat >"$STUB/docker" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$SIMPLE_DOCKER_LOG"
if [[ "${1:-}" == compose && " $* " == *" config --format json "* ]]; then
  identity=(compose --project-directory "$SIMPLE_ROOT" --env-file /dev/null -f - config --format json)
  if [[ "$*" == "${identity[*]}" ]]; then
    printf '%s\n' '{"name":"wrapper-root","services":{"identity":{"image":"scratch"}}}'
    exit 0
  fi
  selected=(compose --env-file "$SIMPLE_ROOT/.env" --project-directory "$SIMPLE_ROOT"
    --project-name wrapper-root --profile services
    -f "$SIMPLE_ROOT/docker-compose.yml"
    -f "$SIMPLE_ROOT/docker-compose.production.yml"
    -f "$SIMPLE_ROOT/docker-compose.selfhost.yml" config --format json)
  all_services=(compose --env-file "$SIMPLE_ROOT/.env" --project-directory "$SIMPLE_ROOT"
    --project-name wrapper-root --profile services
    -f "$SIMPLE_ROOT/docker-compose.yml"
    -f "$SIMPLE_ROOT/docker-compose.production.yml"
    -f "$SIMPLE_ROOT/docker-compose.selfhost.yml" --profile '*' config --format json)
  if [[ "$*" == "${all_services[*]}" ]]; then
    if [[ "${STUB_SELECTED_MODEL_FAIL:-}" == 1 ]]; then
      printf '%s\n' 'STUB-SELECTED-MODEL-ERROR: synthetic-secret' >&2
      exit 23
    fi
    printf '%s\n' '{"name":"wrapper-root","services":{"control-plane":{},"minio":{"profiles":["selfhost-bundled-minio-inactive"]}}}'
    exit 0
  fi
  if [[ "$*" != "${selected[*]}" ]]; then
    printf '%s\n' 'REJECT: unexpected Compose model query' >>"$SELFHOST_STUB_REJECT_LOG"
    exit 97
  fi
  printf '%s\n' '{"name":"wrapper-root","services":{"control-plane":{}}}'
  exit 0
fi
if [[ "${1:-}" == ps && " $* " == *' --filter label=com.docker.compose.project=wrapper-root '* ]]; then
  exit 0
fi
if [[ "${1:-}" == compose ]]; then
  args=("${@:2}")
  i=0
  profiles=()
  while (( i < ${#args[@]} )); do
    case "${args[i]}" in
      --profile) profiles+=("${args[i + 1]:-}"); i=$((i + 2)) ;;
      -f|--env-file|--project-directory|--project-name) i=$((i + 2)) ;;
      *) break ;;
    esac
  done
  case "${args[i]:-}" in
    down|stop|ps)
      [[ " ${profiles[*]} " == *' * '* ]] && {
        printf '%s\n' 'REJECT: all-profile option on mutating or untargeted operation' >>"$SELFHOST_STUB_REJECT_LOG"
        exit 97
      }
      exit 0 ;;
    logs)
      targeted=0 skip=0
      for ((j=i + 1; j<${#args[@]}; j++)); do
        if (( skip )); then skip=0; continue; fi
        case "${args[j]}" in --tail|-n|--since|--until|--index) skip=1 ;;
          --no-follow|-*) ;;
          *) targeted=1 ;;
        esac
      done
      if (( targeted )); then
        [[ " ${profiles[*]} " == ' services * ' ]] || {
          printf '%s\n' 'REJECT: targeted logs require all-profile model' >>"$SELFHOST_STUB_REJECT_LOG"
          exit 97
        }
      else
        [[ " ${profiles[*]} " == ' services ' ]] || {
          printf '%s\n' 'REJECT: unscoped logs changed active profile' >>"$SELFHOST_STUB_REJECT_LOG"
          exit 97
        }
      fi
      exit 0 ;;
  esac
fi
printf '%s\n' 'REJECT: unknown Docker command' >>"$SELFHOST_STUB_REJECT_LOG"
exit 97
STUB
chmod +x "$STUB/docker"
# Prove both interception layers fail closed before negative production cases.
for route in function executable; do
  if [[ "$route" == function ]]; then
    PATH="$STUB:$PATH" bash -c 'docker unrecognized-fixture-command' >/dev/null 2>&1
  else
    "$STUB/docker" unrecognized-fixture-command >/dev/null 2>&1
  fi
  stub_rc=$?
  if [[ "$stub_rc" == 97 ]] && grep -qx 'REJECT: unknown Docker command' "$SELFHOST_STUB_REJECT_LOG"; then
    pass "Docker $route stub rejects unknown commands with exit 97 and marker"
  else
    fail "Docker $route interception did not fail closed"
  fi
  : >"$SELFHOST_STUB_REJECT_LOG"
done

cat >"$STUB/concord-ctl" <<'STUB'
#!/usr/bin/env bash
printf 'mode=%s override=%s profiles=%s argv=%s\n' \
  "${ACTIVITY_HISTORY_CONTRACT_MODE-}" "${COMPOSE_OVERRIDE-}" \
  "${COMPOSE_PROFILES-}" "$*" >>"$SELFHOST_CTL_LOG"
STUB
chmod +x "$STUB/concord-ctl"

# ── Test 1: 'up' delegates to guarded ctl in strict self-host mode ─────
: >"$STUB/ctl.log"
( PATH="$STUB:$PATH" SELFHOST_CTL_LOG="$STUB/ctl.log" \
  CONCORD_CTL_OVERRIDE="$STUB/concord-ctl" bash "$TARGET" up )
if grep -q 'mode=selfhost override=docker-compose.production.yml profiles=services argv=rebuild' "$STUB/ctl.log"; then
  pass "up delegates to guarded ctl with strict self-host production contract"
else
  fail "up bypassed or misconfigured guarded ctl: $(cat "$STUB/ctl.log")"
fi

# ── Test 1b (#3561): 'up' refuses a missing security-event log root ────
: >"$STUB/ctl.log"; : >"$STUB/docker.log"
up_err=$( PATH="$STUB:$PATH" SELFHOST_CTL_LOG="$STUB/ctl.log" \
  CONCORD_CTL_OVERRIDE="$STUB/concord-ctl" \
  CONCORD_SECURITY_LOG_ROOT_OVERRIDE="$STUB/no-such-root" bash "$TARGET" up 2>&1 >/dev/null )
rc=$?
if [[ $rc -ne 0 ]] && grep -qF 'selfhost-security-event-logging.sh security-event-logging' <<<"$up_err" \
   && [[ ! -s "$STUB/ctl.log" && ! -s "$STUB/docker.log" ]]; then
  pass "up refuses a missing security-event log root before ctl/docker and names the fix"
else
  fail "up with missing log root: rc=$rc err=[$up_err] ctl=[$(cat "$STUB/ctl.log")]"
fi

# The refusal names the root it checked, so the operator can see WHICH path is
# missing (a custom override or a typo'd mount would otherwise be invisible).
if grep -qF "security-event log boundary missing: $STUB/no-such-root" <<<"$up_err"; then
  pass "up's refusal names the missing log root path"
else
  fail "up's refusal does not name the root: [$up_err]"
fi

# ── Test 1c (#3561): a symlinked log root is not a prepared boundary ───
ln -s "$STUB/security-log-root" "$STUB/security-log-link"
: >"$STUB/ctl.log"
up_err=$( PATH="$STUB:$PATH" SELFHOST_CTL_LOG="$STUB/ctl.log" \
  CONCORD_CTL_OVERRIDE="$STUB/concord-ctl" \
  CONCORD_SECURITY_LOG_ROOT_OVERRIDE="$STUB/security-log-link" bash "$TARGET" up 2>&1 >/dev/null )
rc=$?
if [[ $rc -ne 0 ]] && grep -qF 'security-event-logging' <<<"$up_err" && [[ ! -s "$STUB/ctl.log" ]]; then
  pass "up refuses a symlinked security-event log root"
else
  fail "up accepted a symlinked log root: rc=$rc ctl=[$(cat "$STUB/ctl.log")]"
fi

# ── Test 1c-2 (#3561 review): a regular FILE at the root is not a boundary ──
# `-d` must stay `-d`: a file passes an existence (`-e`) check and then Docker
# fails with its opaque bind-mount error. Refused before ctl or docker run.
: >"$STUB/security-log-file"
: >"$STUB/ctl.log"; : >"$STUB/docker.log"
up_err=$( PATH="$STUB:$PATH" SELFHOST_CTL_LOG="$STUB/ctl.log" \
  CONCORD_CTL_OVERRIDE="$STUB/concord-ctl" \
  CONCORD_SECURITY_LOG_ROOT_OVERRIDE="$STUB/security-log-file" bash "$TARGET" up 2>&1 >/dev/null )
rc=$?
if [[ $rc -ne 0 ]] && grep -qF 'selfhost-security-event-logging.sh security-event-logging' <<<"$up_err" \
   && [[ ! -s "$STUB/ctl.log" && ! -s "$STUB/docker.log" ]]; then
  pass "up refuses a regular file at the security-event log root before ctl/docker"
else
  fail "up accepted a regular file as the log root: rc=$rc err=[$up_err] ctl=[$(cat "$STUB/ctl.log")] docker=[$(cat "$STUB/docker.log")]"
fi

# ── Test 1d (#3561 review): the printed fix command survives a path with a space ─
# SCRIPT_DIR is readonly and derived from BASH_SOURCE, so run a COPY of the CLI
# placed at a checkout path containing a space. Evaluating the printed line (as
# the operator's root shell would) must yield exactly the intended argv.
CLONE="$STUB/clone dir"
mkdir -p "$CLONE/infrastructure/deploy"
cp "$TARGET" "$CLONE/infrastructure/deploy/concord-selfhost"
cp "$STORAGE_HELPER" "$CLONE/infrastructure/deploy/selfhost-storage.sh"
: >"$STUB/ctl.log"
up_err=$( PATH="$STUB:$PATH" SELFHOST_CTL_LOG="$STUB/ctl.log" \
  CONCORD_CTL_OVERRIDE="$STUB/concord-ctl" \
  CONCORD_SECURITY_LOG_ROOT_OVERRIDE="$STUB/no-such-root" \
  bash "$CLONE/infrastructure/deploy/concord-selfhost" up 2>&1 >/dev/null )
rc=$?
# Strip ANSI, take the printed "sudo <rest>" line, and print the argv a shell
# parses from <rest>, one element per line (eval runs in a subshell).
sudo_line="$(sed $'s/\x1b\\[[0-9;]*m//g' <<<"$up_err" | grep -F 'sudo ' | head -1)"
sudo_line="${sudo_line#*sudo }"
got="$( ( eval "set -- $sudo_line"; printf '%s\n' "$@" ) 2>/dev/null )"
want="$(printf '%s\n' "$CLONE/infrastructure/deploy/selfhost-security-event-logging.sh" security-event-logging)"
if [[ $rc -ne 0 && "$got" == "$want" && ! -s "$STUB/ctl.log" ]]; then
  pass "up's printed sudo command parses to the intended argv for a path with a space"
else
  fail "up's printed sudo command argv wrong: rc=$rc got=[$(tr '\n' '|' <<<"$got")] ctl=[$(cat "$STUB/ctl.log")]"
fi
rm -rf "$CLONE"

# ── Test 2: 'down' uses the production compose files ──────────────────
: >"$STUB/docker.log"
( PATH="$STUB:$PATH" bash "$TARGET" down )
if grep -q -- '-f.*docker-compose\.yml.*-f.*docker-compose\.production\.yml.*down' "$STUB/docker.log" \
  && grep -q -- '--profile services' "$STUB/docker.log"; then
  pass "down uses production compose + --profile services"
else
  fail "down argv wrong: $(cat "$STUB/docker.log")"
fi

# ── Test 3: 'stop' uses 'compose stop' (pause), NOT 'compose down' (teardown) ─
: >"$STUB/docker.log"
( PATH="$STUB:$PATH" bash "$TARGET" stop )
if grep -q -- '--profile.*services.*stop' "$STUB/docker.log" && ! grep -qw 'down' "$STUB/docker.log"; then
  pass "stop uses compose stop (not down)"
else
  fail "stop should call compose stop, not down: $(cat "$STUB/docker.log")"
fi

# ── Test 4: 'status' invokes compose ps ───────────────────────────────
: >"$STUB/docker.log"
( PATH="$STUB:$PATH" bash "$TARGET" status )
if grep -q -- '--profile.*services.*ps' "$STUB/docker.log"; then
  pass "status calls compose ps"
else
  fail "status argv wrong: $(cat "$STUB/docker.log")"
fi

# ── Test 5: 'logs' passes remaining args to compose logs -f ───────────
: >"$STUB/docker.log"
( PATH="$STUB:$PATH" bash "$TARGET" logs control-plane )
if grep -q -- '--profile.*services.*\*.*logs.*-f.*control-plane' "$STUB/docker.log"; then
  pass "logs passes service name through to compose logs -f"
else
  fail "logs argv wrong: $(cat "$STUB/docker.log")"
fi

# Unscoped logs retain the active profile; only a named target needs all profiles.
: >"$STUB/docker.log"
( PATH="$STUB:$PATH" bash "$TARGET" logs )
if grep -q -- '--profile services .* logs -f' "$STUB/docker.log" \
  && ! grep -qE -- 'compose .*--profile \*.* logs' "$STUB/docker.log"; then
  pass "unscoped logs retains the active service profile"
else
  fail "unscoped logs changed active profile: $(cat "$STUB/docker.log")"
fi

# A completed log sample preserves options and service scope without `-f`.
: >"$STUB/docker.log"
out="$(PATH="$STUB:$PATH" bash "$TARGET" logs --no-follow --tail 9 control-plane 2>&1)"
rc=$?
if [[ $rc -eq 0 ]] \
  && grep -q -- '--profile.*services.*\*.*logs.*--tail 9 control-plane' "$STUB/docker.log" \
  && ! grep -q -- 'logs.*-f' "$STUB/docker.log" \
  && ! grep -qE 'compose .* (up|down|stop|start|create|remove|rm)( |$)' "$STUB/docker.log"; then
  pass "logs --no-follow forwards options as a read-only completed sample"
else
  fail "logs --no-follow changed follow mode, argv or read-only behavior (rc=$rc): $out"
fi

for follow_flag in -f --follow --follow=true --follow=false -tf -ft; do
  : >"$STUB/docker.log"
  out="$(PATH="$STUB:$PATH" bash "$TARGET" logs --no-follow "$follow_flag" control-plane 2>&1)"
  rc=$?
  if [[ $rc -eq 1 ]] && grep -qF 'selfhost-storage: conflicting log follow options' <<<"$out" \
    && [[ ! -s "$STUB/docker.log" ]]; then
    pass "completed log sample rejects $follow_flag before any Compose query"
  else
    fail "completed log sample accepted $follow_flag or queried Compose before refusal"
  fi
done

for value_option in --since --until; do
  value=-f
  if [[ "$value_option" == --until ]]; then value=--follow; fi
  : >"$STUB/docker.log"
  out="$(PATH="$STUB:$PATH" bash "$TARGET" logs --no-follow "$value_option" "$value" control-plane 2>&1)"
  rc=$?
  if [[ $rc -eq 0 ]] && grep -qF -- "logs $value_option $value control-plane" "$STUB/docker.log" \
    && ! grep -qE 'compose .* (up|down|stop|start|create|remove|rm)( |$)' "$STUB/docker.log"; then
    pass "completed log sample preserves flag-like $value_option value"
  else
    fail "completed log sample confused $value_option value with follow flag"
  fi
done

# `minio` is declared by the resolved model even when its profile is inactive;
# declared services remain valid read-only log targets.
: >"$STUB/docker.log"
out="$(PATH="$STUB:$PATH" bash "$TARGET" logs minio 2>&1)"
rc=$?
if [[ $rc -eq 0 ]] && grep -q -- '--profile.*services.*\*.*logs.*-f.*minio' "$STUB/docker.log" \
  && ! grep -qE 'compose .* (up|down|stop|start|create|remove|rm)( |$)' "$STUB/docker.log"; then
  pass "logs permits read-only access to a declared service with an inactive profile"
else
  fail "logs refused or mutated a declared inactive-profile service (rc=$rc): $out"
fi

: >"$STUB/docker.log"
out="$(PATH="$STUB:$PATH" bash "$TARGET" logs not-a-service 2>&1)"
rc=$?
if [[ $rc -ne 0 ]] && grep -qF 'selfhost-storage: unknown log service' <<<"$out" \
  && grep -q -- 'compose .*--profile \* config --format json' "$STUB/docker.log" \
  && ! grep -q -- 'compose .* logs' "$STUB/docker.log" \
  && ! grep -qE 'compose .* (up|down|stop|start|create|remove|rm)( |$)' "$STUB/docker.log"; then
  pass "logs rejects an undeclared service before dispatch"
else
  fail "logs dispatched or failed to reject an undeclared service (rc=$rc): $out"
fi

: >"$STUB/docker.log"
out="$(STUB_SELECTED_MODEL_FAIL=1 PATH="$STUB:$PATH" \
  bash "$TARGET" logs control-plane 2>&1)"
rc=$?
if [[ $rc -ne 0 ]] && grep -qF 'selfhost-storage: Compose command failed' <<<"$out" \
  && ! grep -qF 'unknown log service' <<<"$out" \
  && ! grep -qF 'synthetic-secret' <<<"$out" \
  && grep -q -- 'compose .*--profile \* config --format json' "$STUB/docker.log" \
  && ! grep -q -- 'compose .* logs' "$STUB/docker.log"; then
  pass "logs preserves selected-model failure and sanitizes its diagnostic"
else
  fail "logs masked selected-model failure or dispatched anyway (rc=$rc): $out"
fi

if bash "$HERE/test-selfhost-log-tail-flags.sh"; then
  pass "clustered log tail flags preserve dispatch and refusal"
else
  fail "clustered log tail flag regression"
fi

# ── Test 6: '--help' lists verbs and exits 0 ──────────────────────────
out="$(bash "$TARGET" --help 2>&1)"
rc=$?
if [[ $rc -eq 0 ]] && echo "$out" | grep -qi 'status' && echo "$out" | grep -qi 'health'; then
  pass "--help lists verbs (status + health visible) and exits 0"
else
  fail "--help broken (rc=$rc, output missing expected verbs)"
fi

# ── Test 7: no-args prints usage (exit 0) ─────────────────────────────
out="$(bash "$TARGET" 2>&1)"
rc=$?
if [[ $rc -eq 0 ]] && echo "$out" | grep -qi 'usage'; then
  pass "no-args prints usage and exits 0"
else
  fail "no-args broken (rc=$rc)"
fi

# ── Test 8: unknown verb exits non-zero ───────────────────────────────
if ( PATH="$STUB:$PATH" bash "$TARGET" bogus ) >/dev/null 2>&1; then
  fail "bogus verb should exit non-zero"
else
  pass "unknown verb exits non-zero"
fi

# ── Test 9: direct compose argv always includes production files (spot check) ─
: >"$STUB/docker.log"
( PATH="$STUB:$PATH" bash "$TARGET" down )
if grep -q 'docker-compose\.production\.yml' "$STUB/docker.log"; then
  pass "compose argv includes docker-compose.production.yml"
else
  fail "docker-compose.production.yml missing from argv"
fi

# ── Test 10: direct compose argv always includes --profile services ───
: >"$STUB/docker.log"
( PATH="$STUB:$PATH" bash "$TARGET" down )
if grep -q -- '--profile services' "$STUB/docker.log" || grep -q -- '--profile.*services' "$STUB/docker.log"; then
  pass "compose argv includes --profile services"
else
  fail "--profile services missing from argv: $(cat "$STUB/docker.log")"
fi

# ── Guard behavior: real concord-selfhost -> real concord-ctl, stubbed Docker ─
BEHAVIOR_ROOT="$STUB/behavior-root"
BEHAVIOR_STUB="$STUB/behavior-stubs"
mkdir -p "$BEHAVIOR_ROOT/infrastructure/deploy" "$BEHAVIOR_STUB"
cp "$SOURCE_TARGET" "$BEHAVIOR_ROOT/infrastructure/deploy/concord-selfhost"
cp "$STORAGE_HELPER" "$BEHAVIOR_ROOT/infrastructure/deploy/selfhost-storage.sh"
SELFHOST_LOCK_PARENT="$STUB/selfhost-lock-parent"
lock_declaration='readonly DEPLOY_TRANSACTION_LOCK_PATH="/var/lib/concord/deploy-transaction.lock"'
controller_source="$(cat "$ROOT/infrastructure/deploy/concord-ctl.sh")"
if [[ $(grep -Fxc "$lock_declaration" <<<"$controller_source") -ne 1 ]]; then
  fail "controller fixture must rewrite exactly one deploy lock declaration"
  exit 1
fi
fixture_lock_declaration="readonly DEPLOY_TRANSACTION_LOCK_PATH=\"$SELFHOST_LOCK_PARENT/deploy-transaction.lock\""
# Replacement left UNQUOTED: bash 3.2 (stock macOS /bin/bash) keeps the quotes of
# a quoted replacement literally, so the rewrite never matched there. Safe while
# the path holds no `&` or `\` (bash >=5.2 patsub_replacement); mktemp paths never do.
printf '%s\n' "${controller_source/"$lock_declaration"/$fixture_lock_declaration}" \
  >"$BEHAVIOR_ROOT/infrastructure/deploy/concord-ctl.sh"
if [[ $(grep -Fxc "$fixture_lock_declaration" \
  "$BEHAVIOR_ROOT/infrastructure/deploy/concord-ctl.sh") -ne 1 ]]; then
  fail "controller fixture must use the isolated deploy lock path"
  exit 1
fi
chmod +x "$BEHAVIOR_ROOT/infrastructure/deploy/concord-selfhost" \
  "$BEHAVIOR_ROOT/infrastructure/deploy/concord-ctl.sh"
cat >"$BEHAVIOR_ROOT/infrastructure/deploy/sync-releases.sh" <<'STUB'
sync_releases() { :; }
STUB
touch "$BEHAVIOR_ROOT/docker-compose.yml" \
  "$BEHAVIOR_ROOT/docker-compose.production.yml"
# The posture gate renders the three-file set and runs the real checker, so the
# overlay and the checker are the real files, not touch-only placeholders (#3538).
cp "$ROOT/docker-compose.selfhost.yml" "$BEHAVIOR_ROOT/docker-compose.selfhost.yml"
cp "$ROOT/infrastructure/deploy/selfhost-port-posture.py" \
  "$BEHAVIOR_ROOT/infrastructure/deploy/selfhost-port-posture.py"
# The public controller now requires the Task3 TLS interfaces for full-stack
# starts and health checks. Keep this older caller suite independent from the
# TLS observer by supplying a fixture-local, successful implementation. Task4's
# focused tests exercise refusal, ordering, and wait budgets separately.
cat >"$BEHAVIOR_ROOT/infrastructure/deploy/selfhost-tls.sh" <<'TLS_FIXTURE'
selfhost_tls_admit() { return 0; }
selfhost_tls_ready() { return 0; }
TLS_FIXTURE

cat >"$BEHAVIOR_ROOT/docker-compose.byo-s3.yml" <<'YAML'
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

cat >"$BEHAVIOR_STUB/docker" <<'STUB'
#!/usr/bin/env bash
# Structured records expose argv/model and variable NAMES, never credential values.
if [[ -n "${SELFHOST_DOCKER_JSON_LOG:-}" ]]; then
  python3 - "$@" <<'PYJSON'
import json, os, sys
names = os.environ.get("SELFHOST_SCRUB_NAMES", "").split()
record = {"argv": sys.argv[1:], "cwd": os.getcwd(),
    "inherited": [k for k in names if k in os.environ],
    "image": os.environ.get("CONTROL_PLANE_IMAGE", "")}
if os.environ.get("SELFHOST_AUTHORITY_PROBE") == "1" and sys.argv[1:2] == ["compose"]:
    # Finite Compose seam: caller interpolation beats --env-file. The fixtures
    # use only unquoted literal assignments; this is not a dotenv/YAML parser.
    # Record per-key verdicts, never synthetic or real credential values.
    args = sys.argv[2:]
    options, files, profiles, i = {}, [], [], 0
    while i < len(args) and args[i] in ("--env-file", "--project-directory", "--project-name", "--profile", "-f"):
        key, value = args[i:i+2]
        if key == "-f": files.append(value)
        elif key == "--profile": profiles.append(value)
        else: options[key] = value
        i += 2
    record["authority"] = {"options": options, "files": files,
        "profiles": profiles, "command": args[i:]}
    root = os.environ["SELFHOST_AUTHORITY_ROOT"]
    record["controls_preserved"] = all(os.environ.get(key) == value for key, value in {
        "DOCKER_HOST": "unix://" + root + "/fixture-docker.sock",
        "DOCKER_CONFIG": root + "/fixture-docker-config",
        "COMPOSE_PARALLEL_LIMIT": "3",
        "ACTIVITY_HISTORY_HEALTH_TIMEOUT": "1",
    }.items())
    if options.get("--env-file") == root + "/.env":
        durable = {}
        for line in open(root + "/.env"):
            line = line.rstrip("\n")
            if not line or line.startswith("#"): continue
            key, value = line.split("=", 1)
            if key in durable: raise ValueError("duplicate fixture key")
            durable[key] = value
        keys = os.environ["SELFHOST_INSTALLER_KEYS"].split()
        effective = {key: os.environ.get(key, durable.get(key)) for key in keys}
        # The selected base/repository-production SMTP consumer uses
        # ${RESEND_API_KEY:-${SMTP_PASSWORD:-}}. Pin that source expression below
        # and model its nonempty alias precedence, independently of the scrub.
        effective["SMTP_PASSWORD"] = (os.environ.get("RESEND_API_KEY",
            durable.get("RESEND_API_KEY", "")) or effective["SMTP_PASSWORD"] or "")
        record["durable_selected"] = {key: key in durable and
            effective[key] == durable[key] for key in keys}
with open(os.environ["SELFHOST_DOCKER_JSON_LOG"], "a") as out:
    out.write(json.dumps(record) + "\n")
PYJSON
fi
# The accepted resolver asks for identity with stdin, never the root environment.
if [[ "${1:-}" == compose && " $* " == *" --env-file /dev/null -f - config --format json "* ]]; then
  printf '{"name":"%s","services":{}}\n' "${STUB_RENDER_NAME:-concord-selfhost}"
  exit 0
fi
# Stale-MinIO fixture state is persisted because Docker runs in subprocesses.
if [[ "${STUB_BYO_MATRIX:-}" == 1 ]]; then
  if [[ "${1:-}" == ps && " $* " == *'label=com.docker.compose.service=minio'* ]]; then
    [[ "${STUB_MINIO_DISCOVERY_FAIL:-}" != 1 ]] || {
      printf '%s\n' 'STUB-MINIO-DISCOVERY-ERROR: synthetic-secret' >&2
      exit 1
    }
    [[ "${STUB_MINIO_MALFORMED_IDS:-}" != 1 ]] || { printf 'CONTAINER ID\n'; exit 0; }
    python3 - "$@" <<'PYSTALE'
import json, os, sys
state=json.load(open(os.environ["SELFHOST_MINIO_STATE"]))
show_inactive='--all' in sys.argv[1:] or '-a' in sys.argv[1:]
for item in state["containers"]:
    if (item["running"] or show_inactive) and (item["project"] == "concord-selfhost" or os.environ.get("STUB_MINIO_UNRELATED_CANDIDATE") == "1"):
        print(item["id"])
PYSTALE
    exit 0
  fi
  if [[ "${1:-}" == inspect && " $* " == *' --type container --format '* ]]; then
    [[ "${STUB_MINIO_INSPECT_FAIL:-}" != 1 ]] || exit 1
    [[ "${STUB_MINIO_MALFORMED_META:-}" != 1 ]] || { printf 'malformed\n'; exit 0; }
    python3 - "${*: -1}" <<'PYSTALE'
import json, os, sys
state=json.load(open(os.environ["SELFHOST_MINIO_STATE"]))
hits=[c for c in state["containers"] if c["id"] == sys.argv[1]]
if len(hits) != 1: sys.exit(1)
c=hits[0]
labels={"com.docker.compose.project":c["project"], "com.docker.compose.service":c["service"]}
print(json.dumps(labels)+"|"+json.dumps({"Running":c["running"],
    "Status":"running" if c["running"] else "exited", "Paused":False,"Restarting":False}))
PYSTALE
    exit $?
  fi
  if [[ "${1:-}" == stop ]]; then
    [[ "${STUB_MINIO_STOP_FAIL:-}" != 1 ]] || exit 1
    python3 - "${@:2}" <<'PYSTALE'
import json, os, sys
path=os.environ["SELFHOST_MINIO_STATE"]
state=json.load(open(path)); ids=sys.argv[1:]
expected=[c["id"] for c in state["containers"] if c["project"] == "concord-selfhost" and c["service"] == "minio" and c["running"]]
if ids != expected:
    with open(os.environ["SELFHOST_STUB_REJECT_LOG"],"a") as out:
        out.write("REJECT: stop scope mismatch\\n")
    sys.exit(97)
if os.environ.get("STUB_MINIO_STILL_RUNNING") != "1":
    for c in state["containers"]:
        if c["id"] in ids: c["running"]=False
with open(path,"w") as out: json.dump(state,out)
PYSTALE
    exit $?
  fi
fi
printf 'docker image_selector=%s inherited_instance=%s inherited_gate=%s inherited_replicas=%s inherited_operator=%s inherited_privacy=%s inherited_env_files=%s inherited_disable_env=%s %s\n' \
  "${CONTROL_PLANE_IMAGE:-}" "${INSTANCE_TYPE:-}" \
  "${ACTIVITY_HISTORY_CLUSTER_ENABLED:-}" "${CONTROL_PLANE_REPLICA_COUNT:-}" \
  "${ACTIVITY_HISTORY_OPERATOR_NAME:-}" "${ACTIVITY_HISTORY_PRIVACY_POLICY_URL:-}" \
  "${COMPOSE_ENV_FILES:-}" "${COMPOSE_DISABLE_ENV_FILE:-}" \
  "$*" >>"$SELFHOST_DOCKER_LOG"
if [[ "${STUB_FAIL_DISABLE_ALL:-}" == 1 && " $* " == *" disable-all "* ]]; then
  exit 1
fi
if [[ "${1:-}" == version ]]; then
  [[ "${STUB_VERSION_FAIL:-}" != 1 ]] || exit 1  # daemon unreachable: no output, non-zero
  printf '%s%s' "${STUB_ENGINE_VERSION-29.8.2}" "${STUB_ENGINE_END-$'\n'}"
  exit "${STUB_ENGINE_EXIT:-0}"
fi
# Media admission is a separate daemon-wide read, not an Activity History
# control-plane observation. Do not consume that fixture's ps sequence.
if [[ "${1:-}" == ps && " $* " == *'label=com.docker.compose.service=media-plane'* ]]; then
  if [[ "$*" != 'ps --all --quiet --no-trunc --filter label=com.docker.compose.service=media-plane' ]]; then
    printf '%s\n' 'REJECT: unexpected media discovery query' >>"$SELFHOST_STUB_REJECT_LOG"
    exit 97
  fi
  [[ "${STUB_MEDIA_PRESENT:-}" != 1 ]] || printf '%s\n' aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  exit 0
fi
if [[ "${1:-}" == inspect && "${3:-}" == '{{json .Config.Env}}' ]]; then
  if [[ $# != 4 || "${2:-}" != --format || "${STUB_MEDIA_PRESENT:-}" != 1 \
    || "${4:-}" != aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ]]; then
    printf '%s\n' 'REJECT: unexpected media environment query' >>"$SELFHOST_STUB_REJECT_LOG"
    exit 97
  fi
  printf '%s\n' "${STUB_MEDIA_ENV_JSON:-[]}"
  exit 0
fi
if [[ " $* " == *" compose "*" config --format json "* ]]; then
  all_profiles=0
  args=("$@")
  for ((i = 0; i + 1 < ${#args[@]}; i++)); do
    if [[ "${args[i]}" == --profile && "${args[i + 1]}" == '*' ]]; then
      all_profiles=1
    fi
  done
  if [[ "${STUB_BYO_MATRIX:-}" == 1 && $all_profiles == 1 \
    && "${STUB_LOG_MODEL_FAIL:-}" == 1 ]]; then
    echo 'STUB-ALL-PROFILE-COMPOSE-ERROR: synthetic-secret' >&2
    exit 1
  fi
  if [[ "${STUB_RENDER_FAIL:-}" == 1 ]]; then
    # Compose's dotenv errors can quote a whole .env line; the sentinel stands in for its value.
    echo 'STUB-COMPOSE-ERROR: required variable DOMAIN is missing a value (line: SECRET_SENTINEL_9f3)' >&2
    exit 1
  fi
  # Compose refuses a -f file that does not exist; model that.
  args=("$@")
  for ((i = 0; i < ${#args[@]} - 1; i++)); do
    if [[ "${args[i]}" == -f && ! -f "${args[i + 1]}" ]]; then
      exit 1
    fi
  done
  extra="${STUB_RENDER_EXTRA:-}"
  if [[ "${STUB_BYO_MATRIX:-}" == 1 && $all_profiles != 1 ]]; then
    extra=""
  fi
  printf '{"name":"%s","services":{"control-plane":{"environment":{"ACTIVITY_HISTORY_CLUSTER_ENABLED":"%s","CONTROL_PLANE_REPLICA_COUNT":"%s","ACTIVITY_HISTORY_OPERATOR_NAME":"%s","ACTIVITY_HISTORY_PRIVACY_POLICY_URL":"%s","INSTANCE_TYPE":"%s"},"image":"%s"}%s}}\n' \
    "${STUB_RENDER_NAME:-concord-selfhost}" \
    "${STUB_RENDER_GATE:-${ACTIVITY_HISTORY_CLUSTER_ENABLED:-${STUB_ROOT_GATE:-false}}}" \
    "${STUB_RENDER_REPLICAS:-${CONTROL_PLANE_REPLICA_COUNT:-${STUB_ROOT_REPLICAS:-1}}}" \
    "${STUB_RENDER_OPERATOR_NAME:-${STUB_ROOT_OPERATOR_NAME:-}}" \
    "${STUB_RENDER_PRIVACY_POLICY_URL:-${STUB_ROOT_PRIVACY_POLICY_URL:-}}" \
    "${STUB_RENDER_INSTANCE_TYPE:-${INSTANCE_TYPE:-${STUB_ROOT_INSTANCE_TYPE:-self-hosted}}}" \
    "${STUB_RENDER_IMAGE:-${CONTROL_PLANE_IMAGE:-concordvoice-control-plane:local}}" \
    "$extra"
  exit 0
fi
# Scoped-rebuild liveness probe: `compose ... ps --status running -q -- SVC` (the `--`
# distinguishes it from the media handoff's drain check, which must stay empty).
# STUB_COMPOSE_RUNNING lists the services that report a running container.
if [[ " $* " == *" compose "* && " $* " == *" ps --status running -q -- "* ]]; then
  [[ " ${STUB_COMPOSE_RUNNING:-} " != *" ${*: -1} "* ]] || printf 'ctr-%s\n' "${*: -1}"
  exit 0
fi
if [[ " $* " == *" compose "* && " $* " == *" ps -q nats "* ]]; then
  printf '%s\n' aaaaaaaaaaaa
  exit 0
fi
# Project-label listing used by the live port check (#3538). Narrow on purpose: only a
# label filter, so the existing `ps` cases keep their behaviour. It honours the flags the
# real CLI does: without -a a stopped container (STUB_STOPPED_IDS) is not listed, without
# -q a header line precedes the ids, and the filter value must equal the project name
# exactly (STUB_PROJECT_NAME) or nothing matches.
if [[ "${1:-}" == ps && "$*" == *label=com.docker.compose.project=* ]]; then
  [[ "${STUB_PS_FAIL:-}" != 1 ]] || { echo 'Cannot connect to the Docker daemon' >&2; exit 1; }
  ps_all=0 ps_quiet=0 ps_label=''
  ps_args=("$@")
  for ((i = 1; i < ${#ps_args[@]}; i++)); do
    case "${ps_args[i]}" in
      -a|--all) ps_all=1 ;;
      -q|--quiet) ps_quiet=1 ;;
      -aq|-qa) ps_all=1; ps_quiet=1 ;;
      --filter) ps_label="${ps_args[i + 1]:-}" ;;
    esac
  done
  [[ "$ps_label" == "label=com.docker.compose.project=${STUB_PROJECT_NAME:-concord-selfhost}" ]] || exit 0
  (( ps_quiet )) || printf 'CONTAINER ID   IMAGE\n'
  [[ -z "${STUB_PROJECT_IDS:-}" ]] || printf '%s\n' "$STUB_PROJECT_IDS"
  if [[ "${STUB_BYO_MATRIX:-}" == 1 ]]; then
    python3 - "$ps_all" <<'PYSTALEPORT'
import json,os,sys
state=json.load(open(os.environ["SELFHOST_MINIO_STATE"]))
for c in state["containers"]:
    if c["project"] == "concord-selfhost" and (c["running"] or sys.argv[1] == "1"):
        print(c["id"])
PYSTALEPORT
  fi
  if (( ps_all )) && [[ -n "${STUB_STOPPED_IDS:-}" ]]; then printf '%s\n' "$STUB_STOPPED_IDS"; fi
  exit 0
fi
if [[ "${1:-}" == ps ]]; then
  index="$(cat "$SELFHOST_PS_INDEX" 2>/dev/null || echo 0)"
  printf '%s\n' "$((index + 1))" >"$SELFHOST_PS_INDEX"
  case "$index" in
    0) printf '%b' "${STUB_PS_0:-}" ;;
    1) printf '%b' "${STUB_PS_1:-}" ;;
    *) printf '%b' "${STUB_PS_LATER-${STUB_PS_1:-}}" ;;
  esac
  exit 0
fi
# Whole-document inspect for the live port check; the --format forms below keep
# serving the env and image probes.
# STUB_INSPECT_JSON is an array positionally aligned with the ids the listing prints
# (running ids first, then STUB_STOPPED_IDS). Like the real CLI it prints a document only
# for ids it was given and fails on an id it does not know, so a malformed id list from the
# caller (a header line, a missing -a) cannot silently inspect everything.
if [[ "${1:-}" == inspect && "$*" != *--format* ]]; then
  [[ "${STUB_INSPECT_FAIL:-}" != 1 ]] || { echo 'Error: No such object' >&2; exit 1; }
  STUB_ALL_IDS="${STUB_PROJECT_IDS:-} ${STUB_STOPPED_IDS:-}" python3 -c 'import json, os, sys
ids = os.environ["STUB_ALL_IDS"].split()
doc = json.loads(os.environ.get("STUB_INSPECT_JSON", "[]"))
if os.environ.get("STUB_BYO_MATRIX") == "1":
    for c in json.load(open(os.environ["SELFHOST_MINIO_STATE"]))["containers"]:
        if c["project"] != "concord-selfhost": continue
        ids.append(c["id"])
        doc.append({"Config":{"Labels":{"com.docker.compose.project":c["project"],"com.docker.compose.service":c["service"]}},"HostConfig":{"NetworkMode":"default","PortBindings":{}},"NetworkSettings":{"Networks":{}}})
out = []
for want in sys.argv[1:]:
    if want not in ids or ids.index(want) >= len(doc):
        sys.stderr.write("Error: No such object: " + want + "\n")
        sys.exit(1)
    out.append(doc[ids.index(want)])
print(json.dumps(out))' "${@:2}"
  exit $?
fi
if [[ "${1:-}" == inspect && "${3:-}" == '{{json .HostConfig.PortBindings}}|{{.HostConfig.PublishAllPorts}}' \
  && "${4:-}" == aaaaaaaaaaaa ]]; then
  printf '%s\n' '{}|false'
  exit 0
fi
# Network ids of each container: `inspect --format '{{range .NetworkSettings.Networks}}...'`.
# Like the real CLI it prints one line per container, with a trailing space after each id,
# and fails (after printing the lines it could) when a container id is unknown.
if [[ "${1:-}" == inspect && "$*" == *NetworkSettings.Networks* ]]; then
  [[ "${STUB_INSPECT_NETIDS_FAIL:-}" != 1 ]] || { echo 'Error: No such object' >&2; exit 1; }
  # argv is: inspect --format TEMPLATE ID...
  STUB_ALL_IDS="${STUB_PROJECT_IDS:-} ${STUB_STOPPED_IDS:-}" python3 -c 'import json, os, sys
ids = os.environ["STUB_ALL_IDS"].split()
doc = json.loads(os.environ.get("STUB_INSPECT_JSON", "[]"))
if os.environ.get("STUB_BYO_MATRIX") == "1":
    for c in json.load(open(os.environ["SELFHOST_MINIO_STATE"]))["containers"]:
        if c["project"] != "concord-selfhost": continue
        ids.append(c["id"])
        doc.append({"Config":{"Labels":{"com.docker.compose.project":c["project"],"com.docker.compose.service":c["service"]}},"HostConfig":{"NetworkMode":"default","PortBindings":{}},"NetworkSettings":{"Networks":{}}})
rc = 0
for want in sys.argv[1:]:
    if want not in ids or ids.index(want) >= len(doc):
        sys.stderr.write("Error: No such object: " + want + "\n")
        rc = 1
        continue
    nets = (doc[ids.index(want)].get("NetworkSettings") or {}).get("Networks") or {}
    print("".join((n.get("NetworkID") or "") + " " for n in nets.values()))
sys.exit(rc)' "${@:4}"
  exit $?
fi
# `docker network inspect ID...`: a JSON array of the networks asked for. Like the real CLI it
# prints the ones it found and exits 1 when any id is unknown (STUB_NETWORK_INSPECT_PARTIAL_OK=1
# models a daemon that answers 0 with fewer networks than asked for). STUB_NETWORKS_JSON is
# the network list; the default is one plain bridge.
if [[ "${1:-}" == network && "${2:-}" == inspect ]]; then
  [[ "${STUB_NETWORK_INSPECT_FAIL:-}" != 1 ]] || { echo 'Error: daemon unavailable' >&2; exit 1; }
  python3 -c 'import json, os, sys
nets = json.loads(os.environ.get("STUB_NETWORKS_JSON") or json.dumps(
    [{"Id": "net-plain", "Name": "concord_default", "Driver": "bridge", "Options": {}}]))
out, rc = [], 0
for want in sys.argv[1:]:
    hit = [n for n in nets if n.get("Id") == want]
    if hit:
        out.append(hit[0])
    else:
        sys.stderr.write("Error: No such network: " + want + "\n")
        rc = 1
print(json.dumps(out))
sys.exit(0 if os.environ.get("STUB_NETWORK_INSPECT_PARTIAL_OK") == "1" else rc)' "${@:3}"
  exit $?
fi
if [[ "${1:-}" == inspect && ( " $* " == *'.Config.Image'* || " $* " == *'.Config.Env'* ) ]]; then
  id="${*: -1}"
  if [[ "$*" == *'.Config.Image'* ]]; then
    printf '%s\n' "${STUB_INSPECT_IMAGE:-${CONTROL_PLANE_IMAGE:-concordvoice-control-plane:local}}"
  elif [[ "$id" == cp-false || "$id" == cp-old ]]; then
    printf '%s\n' 'ACTIVITY_HISTORY_CLUSTER_ENABLED=false'
  else
    printf '%s\n' 'ACTIVITY_HISTORY_CLUSTER_ENABLED=true'
  fi
  exit 0
fi
if [[ "${1:-}" == image && "${2:-}" == inspect ]]; then
  exit 0
fi
if [[ "$*" == *"voice-enforcement-rollout status"* ]]; then
  printf '%s\n' \
    'voice-enforcement-rollout: inactive' \
    'voice-enforcement-rollout: registryRows=0' \
    'voice-enforcement-rollout: dmParentRows=0' \
    'voice-enforcement-rollout: credentialParentRows=0' \
    'voice-enforcement-rollout: dmReconciliationRows=0'
  exit 0
fi
if [[ "${1:-}" == compose ]]; then
  args=("${@:2}"); i=0
  while (( i < ${#args[@]} )); do
    case "${args[i]}" in
      -f|--env-file|--project-directory|--project-name|--profile) i=$((i + 2)) ;;
      *) break ;;
    esac
  done
  case "${args[i]:-}" in
    build|up|start|create|stop|down|restart|logs|config|pull|ps|run)
      # These are existing controller/wrapper dispatches. rm/volume/system
      # calls and unknown verbs never inherit a successful wildcard.
      if [[ "${STUB_BYO_MATRIX:-}" == 1 ]]; then
        case " $* " in
          *' --remove-orphans '*|*' -v '*|*' --volumes '*|*' selfhost-bundled-minio-inactive '*)
            printf '%s\n' 'REJECT: destructive/inactive BYO command' >>"$SELFHOST_STUB_REJECT_LOG"; exit 97 ;;
        esac
        if [[ "${args[i]}" == up && "${STUB_MINIO_APPEAR_AFTER_UP:-}" == 1 ]] \
          || [[ "${STUB_MINIO_APPEAR_AFTER_LIFECYCLE:-}" == "${args[i]}" ]]; then
          python3 - <<'PYSTALE'
import json, os
p=os.environ["SELFHOST_MINIO_STATE"]; state=json.load(open(p))
state["containers"][0]["running"]=True
with open(p,"w") as out: json.dump(state,out)
PYSTALE
        fi
      fi
      exit 0 ;;
  esac
fi
printf '%s\n' 'REJECT: unknown Docker command' >>"$SELFHOST_STUB_REJECT_LOG"
exit 97
STUB
cat >"$BEHAVIOR_STUB/curl" <<'STUB'
#!/usr/bin/env bash
printf 'curl %s\n' "$*" >>"$SELFHOST_DOCKER_LOG"
url="${@: -1}"
case "$url" in
  http://127.0.0.1:8080/readyz)
    exit 0
    ;;
  http://127.0.0.1:8080/api/v1/client/config)
    printf '%s' "${STUB_CONFIG_CODE:-200}"
    [[ "${STUB_CONFIG_CODE:-200}" == 200 ]] || exit 22
    exit 0
    ;;
  http://127.0.0.1:3000/readyz)
    body=''
    for ((i = 1; i <= $#; i++)); do
      if [[ "${!i}" == -o && $((i + 1)) -le $# ]]; then
        next=$((i + 1))
        body="${!next}"
        break
      fi
    done
    if [[ -n "$body" ]]; then
      printf '%s\n' '{"ready":true,"voiceEnforcementProtocol":3}' >"$body"
    else
      printf '%s\n' '{"ready":true,"voiceEnforcementProtocol":3}'
    fi
    printf '200'
    exit 0
    ;;
esac
exit 0
STUB
cat >"$BEHAVIOR_STUB/sleep" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
# The posture gate asks `pgrep` whether dockerd was started with --allow-direct-routing. The
# default is no match, so the host's real process table cannot decide a case; the argv is
# logged so a case can see the pattern the gate searched for.
cat >"$BEHAVIOR_STUB/pgrep" <<'STUB'
#!/usr/bin/env bash
printf 'pgrep %s\n' "$*" >>"$SELFHOST_DOCKER_LOG"
[[ "${STUB_PGREP_MATCH:-}" == 1 ]] && exit 0
exit 1
STUB
# `status` probes coturn with nc; a stub keeps it off the host's real loopback.
cat >"$BEHAVIOR_STUB/nc" <<'STUB'
#!/usr/bin/env bash
exit 1
STUB
chmod +x "$BEHAVIOR_STUB/docker" "$BEHAVIOR_STUB/curl" "$BEHAVIOR_STUB/sleep" \
  "$BEHAVIOR_STUB/pgrep" "$BEHAVIOR_STUB/nc"

export SELFHOST_DOCKER_LOG="$STUB/selfhost-docker.log"
export SELFHOST_PS_INDEX="$STUB/selfhost-ps.index"
export PATH="$BEHAVIOR_STUB:$PATH"
export ACTIVE_DOCKER_STUB="$BEHAVIOR_STUB/docker"
export ACTIVITY_HISTORY_HEALTH_TIMEOUT=1 ACTIVITY_HISTORY_HEALTH_INTERVAL=0 \
  VOICE_ENFORCEMENT_HEALTH_TIMEOUT=1
# The gate reads the Docker daemon config; point it at a path in the temp dir that does not
# exist, so the host's real /etc/docker/daemon.json cannot decide a case. A case that needs
# one writes it here, and reset_selfhost_behavior removes it.
export SELFHOST_DOCKER_DAEMON_JSON="$STUB/daemon.json"
BEHAVIOR_TARGET="$BEHAVIOR_ROOT/infrastructure/deploy/concord-selfhost"
BEHAVIOR_CTL="$BEHAVIOR_ROOT/infrastructure/deploy/concord-ctl.sh"

write_selfhost_contract() {
  local gate="$1" replicas="${2:-1}"
  {
    printf 'INSTANCE_TYPE=self-hosted\n'
    printf 'ACTIVITY_HISTORY_CLUSTER_ENABLED=%s\n' "$gate"
    printf 'CONTROL_PLANE_REPLICA_COUNT=%s\n' "$replicas"
    printf 'ACTIVITY_HISTORY_OPERATOR_NAME=\n'
    printf 'ACTIVITY_HISTORY_PRIVACY_POLICY_URL=\n'
  } >"$BEHAVIOR_ROOT/.env"
}

# Live-binding fixture for the post-up port check (#3538). Every self-host rebuild now
# ends by inspecting the project's containers, so the default stub project is a
# compliant one; the cases that need a violating or empty set override these.
# Both containers are attached to one network, a plain bridge by default (see the
# `docker network inspect` stub); the network cases pass another id and name.
live_inspect() {
  local postgres_binds="$1" net_id="${2:-net-plain}" net_name="${3:-concord_default}"
  printf '[{"Config":{"Labels":{"com.docker.compose.service":"control-plane"},"Env":["SENTINEL_ENV=T2-SENTINEL"]},"HostConfig":{"NetworkMode":"default","PortBindings":{"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":"8080"}]}},"NetworkSettings":{"Networks":{"%s":{"NetworkID":"%s"}}}},{"Config":{"Labels":{"com.docker.compose.service":"postgres"},"Env":["SENTINEL_ENV=T2-SENTINEL"]},"HostConfig":{"NetworkMode":"default","PortBindings":%s},"NetworkSettings":{"Networks":{"%s":{"NetworkID":"%s"}}}}]' \
    "$net_name" "$net_id" "$postgres_binds" "$net_name" "$net_id"
}

reset_selfhost_behavior() {
  : >"$SELFHOST_DOCKER_LOG"
  printf '0\n' >"$SELFHOST_PS_INDEX"
  export STUB_RENDER_GATE="$1" STUB_RENDER_REPLICAS="${2:-1}"
  export STUB_RENDER_INSTANCE_TYPE=self-hosted
  export STUB_RENDER_OPERATOR_NAME='' STUB_RENDER_PRIVACY_POLICY_URL=''
  export STUB_ROOT_GATE="$1" STUB_ROOT_REPLICAS="${2:-1}" \
    STUB_ROOT_OPERATOR_NAME='' STUB_ROOT_PRIVACY_POLICY_URL='' \
    STUB_ROOT_INSTANCE_TYPE=self-hosted
  export STUB_PS_0="${3:-}" STUB_PS_1="${4:-}" \
    STUB_PS_LATER="${5:-${4:-}}"
  unset STUB_RENDER_IMAGE STUB_INSPECT_IMAGE STUB_COMPOSE_RUNNING \
    STUB_VERSION_FAIL STUB_ENGINE_EXIT STUB_ENGINE_END STUB_PS_FAIL STUB_INSPECT_FAIL STUB_RENDER_FAIL STUB_LOG_MODEL_FAIL STUB_RENDER_NAME \
    STUB_PROJECT_NAME STUB_STOPPED_IDS STUB_CONFIG_CODE STUB_ENGINE_VERSION \
    STUB_NETWORKS_JSON STUB_NETWORK_INSPECT_FAIL STUB_NETWORK_INSPECT_PARTIAL_OK \
    STUB_INSPECT_NETIDS_FAIL STUB_PGREP_MATCH STUB_MEDIA_PRESENT STUB_MEDIA_ENV_JSON
  chmod 600 "$SELFHOST_DOCKER_DAEMON_JSON" 2>/dev/null || true
  rm -f "$SELFHOST_DOCKER_DAEMON_JSON"
  export STUB_PROJECT_IDS=$'cp1\npg1'
  STUB_INSPECT_JSON="$(live_inspect '{}')"
  export STUB_INSPECT_JSON
  rm -f "$BEHAVIOR_ROOT/infrastructure/deploy/deploy.env.local" \
    "$BEHAVIOR_ROOT/infrastructure/deploy/.env.activity-history-image.local" \
    "$BEHAVIOR_ROOT/infrastructure/deploy/.env.activity-history-receipt.local"
}

run_selfhost() {
  selfhost_output="$(bash "$BEHAVIOR_TARGET" "$@" 2>&1)"
  selfhost_rc=$?
}

# Pin the new read without letting a broader ps stub hide missing --all or
# advance the control-plane state machine used by activation/rollback below.
reset_selfhost_behavior false 1 'cp-false\n' ''
media_ids="$(docker ps --all --quiet --no-trunc --filter label=com.docker.compose.service=media-plane)"
if [[ -z "$media_ids" && $(cat "$SELFHOST_PS_INDEX") == 0 ]]; then
  pass "exact media discovery leaves the control-plane observation sequence untouched"
else
  fail "media discovery consumed a control-plane observation"
fi
docker ps --quiet --no-trunc --filter label=com.docker.compose.service=media-plane >/dev/null 2>&1
stub_rc=$?
if [[ $stub_rc == 97 ]] && grep -qx 'REJECT: unexpected media discovery query' "$SELFHOST_STUB_REJECT_LOG"; then
  pass "media discovery fixture rejects a query that omits stopped containers"
else
  fail "media discovery fixture accepted an unrecognized query"
fi
: >"$SELFHOST_STUB_REJECT_LOG"

write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
export STUB_MEDIA_PRESENT=1 STUB_MEDIA_ENV_JSON='["MEDIASOUP_SHARED_WEBRTC_SERVER=false"]'
run_selfhost up
if [[ $selfhost_rc -eq 0 ]] \
  && grep -qF 'inspect --format {{json .Config.Env}} aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' "$SELFHOST_DOCKER_LOG" \
  && grep -qE 'compose .* build media-plane' "$SELFHOST_DOCKER_LOG"; then
  pass "self-host up admits an inspected legacy media container"
else
  fail "self-host legacy media positive control failed: $selfhost_output"
fi

write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
export STUB_MEDIA_PRESENT=1 STUB_MEDIA_ENV_JSON='["MEDIASOUP_SHARED_WEBRTC_SERVER=true"]'
run_selfhost up
if [[ $selfhost_rc -ne 0 ]] \
  && grep -qF 'existing shared media requires coordinated rollback support' <<<"$selfhost_output" \
  && ! grep -qE 'compose .* (build|up|start|create|stop|down|restart|run) ' "$SELFHOST_DOCKER_LOG"; then
  pass "self-host up refuses existing shared media before service mutation"
else
  fail "self-host up lost the existing-shared-media refusal: $selfhost_output"
fi

write_selfhost_contract true
reset_selfhost_behavior true 1 '' ''
run_selfhost up
if [[ $selfhost_rc -ne 0 ]] && ! grep -qE 'compose .* up ' "$SELFHOST_DOCKER_LOG"; then
  pass "self-host up refuses first activation with zero current control-planes"
else
  fail "self-host up activated desired=true from zero current control-planes"
fi

write_selfhost_contract true
reset_selfhost_behavior true 1 'cp-false\n' ''
run_selfhost up
if [[ $selfhost_rc -ne 0 ]] && ! grep -qE 'compose .* up ' "$SELFHOST_DOCKER_LOG"; then
  pass "self-host up refuses desired=true when current gate is false"
else
  fail "self-host up recreated a false-gate control-plane into gate=true"
fi

write_selfhost_contract true
reset_selfhost_behavior true 1 'cp-true\ncp-false\n' ''
run_selfhost up
if [[ $selfhost_rc -ne 0 ]] && ! grep -qE 'compose .* up ' "$SELFHOST_DOCKER_LOG"; then
  pass "self-host up refuses mixed or multiple current control-planes"
else
  fail "self-host up accepted mixed/multiple current control-planes"
fi

write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
run_selfhost up
if [[ $selfhost_rc -eq 0 ]] \
  && grep -qE 'compose .* build control-plane' "$SELFHOST_DOCKER_LOG" \
  && grep -qE 'compose .* build media-plane' "$SELFHOST_DOCKER_LOG" \
  && grep -qE 'curl .*3000/readyz' "$SELFHOST_DOCKER_LOG"; then
  pass "self-host up allows desired gate=false through guarded rebuild"
else
  fail "self-host up failed to perform a safe gate=false rebuild"
fi

# Every gh boundary is fail-closed; no test can fall through to host credentials.
cat >"$BEHAVIOR_STUB/gh" <<'GH_FIXTURE'
#!/usr/bin/env bash
printf 'unexpected-gh-call\n' >>"$SELFHOST_DOCKER_LOG"
exit 93
GH_FIXTURE
chmod +x "$BEHAVIOR_STUB/gh"
# #2506: a public unpack has no managed recovery writer. Exercise real callers.
mv "$BEHAVIOR_ROOT/infrastructure/deploy/sync-releases.sh" "$STUB/sync-releases.saved"
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
selfhost_output="$(ACTIVITY_HISTORY_CONTRACT_MODE=selfhost COMPOSE_OVERRIDE=docker-compose.production.yml \
  COMPOSE_PROFILES=services bash "$BEHAVIOR_CTL" sync-releases 2>&1)"
selfhost_rc=$?
# Existing selected-model resolution may render config before dispatch. Permit
# only config, exact Engine version and direct-routing process reads; every
# other Docker/HTTP/gh/effect record refuses.
seam_readonly=1
seam_docker_prefix='docker image_selector= inherited_instance= inherited_gate= inherited_replicas= inherited_operator= inherited_privacy= inherited_env_files= inherited_disable_env='
while IFS= read -r seam_record; do
  [[ -z "$seam_record" ]] && continue
  # Defensive Engine-floor/posture checks read version and process flags before
  # dispatch. Permit only these exact fixture argv, with no alternate commands.
  if [[ "$seam_record" == "$seam_docker_prefix version --format {{.Server.Version}}" \
     || "$seam_record" == 'pgrep -f dockerd.*--allow-direct-routing' ]]; then
    continue
  fi
  if [[ "$seam_record" != docker\ * || "$seam_record" != *' compose '* ]]; then
    seam_readonly=0
    continue
  fi
  # Fixture argv records use space-free mktemp paths; consume only the known
  # Compose global option/value pairs before the exact read-only command tail.
  seam_tail="${seam_record#* compose }"
  read -r -a seam_args <<<"$seam_tail"
  seam_index=0
  while (( seam_index < ${#seam_args[@]} )); do
    case "${seam_args[$seam_index]}" in
      --env-file|--project-directory|--project-name|--profile|-f)
        seam_index=$((seam_index + 2)) ;;
      *) break ;;
    esac
  done
  if (( ${#seam_args[@]} - seam_index != 3 )); then
    seam_readonly=0
  elif [[ "${seam_args[$seam_index]}" != config \
       || "${seam_args[$((seam_index + 1))]}" != --format \
       || "${seam_args[$((seam_index + 2))]}" != json ]]; then
    seam_readonly=0
  fi
done <"$SELFHOST_DOCKER_LOG"
if [[ $selfhost_rc -ne 0 && $seam_readonly -eq 1 ]] \
  && grep -qF 'managed-host recovery only' <<<"$selfhost_output" \
  && ! grep -qx 'unexpected-gh-call' "$SELFHOST_DOCKER_LOG" \
  && [[ ! -e "$BEHAVIOR_ROOT/releases/.tag" && ! -e "$BEHAVIOR_ROOT/releases" ]]; then
  pass "self-host direct sync-releases refuses before loading the absent managed writer"
else
  fail "self-host sync-releases lacks managed-only refusal before source/mutation: $selfhost_output"
  # Records contain only synthetic fixture metadata; expose the failing oracle.
  printf 'sync-refusal fixture rc=%s read-only=%s releases-exists=%s tag-exists=%s\n' \
    "$selfhost_rc" "$seam_readonly" \
    "$(test -e "$BEHAVIOR_ROOT/releases" && echo yes || echo no)" \
    "$(test -e "$BEHAVIOR_ROOT/releases/.tag" && echo yes || echo no)"
  cat "$SELFHOST_DOCKER_LOG"
fi
for seam_verb in up health stop down logs; do
  write_selfhost_contract false
  reset_selfhost_behavior false 1 '' ''
  if [[ "$seam_verb" == logs ]]; then run_selfhost logs --no-follow control-plane
  else run_selfhost "$seam_verb"; fi
  case "$seam_verb" in
    up) seam_boundary='compose .* up ' ;;
    health) seam_boundary='curl .*8080/api/v1/client/config' ;;
    stop) seam_boundary='compose .* stop' ;;
    down) seam_boundary='compose .* down' ;;
    logs) seam_boundary='compose .* logs .*control-plane' ;;
  esac
  if [[ $selfhost_rc -eq 0 ]] && grep -qE "$seam_boundary" "$SELFHOST_DOCKER_LOG" \
    && ! grep -qF 'sync-releases' <<<"$selfhost_output"; then
    pass "public closure: $seam_verb reaches its boundary without sync-releases.sh"
  else
    fail "public closure: $seam_verb depends on absent managed writer or prints managed advice: $selfhost_output"
  fi
done
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
run_selfhost up
if [[ $selfhost_rc -eq 0 ]] && grep -qx 'Done.' <<<"$selfhost_output" \
  && ! grep -qF 'sync-releases' <<<"$selfhost_output"; then
  pass "self-host rebuild success omits managed release recovery advice"
else fail "self-host rebuild suggests managed recovery or did not finish: $selfhost_output"; fi
# The companion managed dispatch must actually load and invoke its writer.
cat >"$BEHAVIOR_ROOT/infrastructure/deploy/sync-releases.sh" <<'RECOVERY'
sync_releases() { printf 'managed-writer-called\n' >>"$SELFHOST_DOCKER_LOG"; }
RECOVERY
reset_selfhost_behavior false 1 '' ''
selfhost_output="$(ACTIVITY_HISTORY_CONTRACT_MODE=managed COMPOSE_OVERRIDE=docker-compose.production.yml \
  COMPOSE_PROFILES=services bash "$BEHAVIOR_CTL" sync-releases 2>&1)"
selfhost_rc=$?
if [[ $selfhost_rc -eq 0 ]] && grep -qx 'managed-writer-called' "$SELFHOST_DOCKER_LOG"; then
  pass "managed sync-releases loads and invokes the recovery writer"
else fail "managed sync-releases lost its writer dispatch: $selfhost_output"; fi
mv "$STUB/sync-releases.saved" "$BEHAVIOR_ROOT/infrastructure/deploy/sync-releases.sh"

# The lock matrix keeps real wrapper/controller dispatch and substitutes only
# the absolute backend executable. Never open the host's managed lock path.
cp "$BEHAVIOR_CTL" "$STUB/controller-before-lock-matrix.sh"
SELFHOST_LOCK_BACKEND="$BEHAVIOR_STUB/flock-backend"
export SELFHOST_LOCK_CALL_LOG="$STUB/selfhost-lock-calls.log"
cat >"$SELFHOST_LOCK_BACKEND" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$SELFHOST_LOCK_CALL_LOG"
[[ -e /dev/fd/9 ]] || exit 74
exit "${SELFHOST_LOCK_BACKEND_RC:-0}"
STUB
chmod +x "$SELFHOST_LOCK_BACKEND"
controller_source="$(cat "$BEHAVIOR_CTL")"
printf '%s\n' "${controller_source//\/usr\/bin\/flock/"$SELFHOST_LOCK_BACKEND"}" \
  >"$BEHAVIOR_CTL"
selfhost_lock_stdout="$STUB/selfhost-lock.stdout"
selfhost_lock_stderr="$STUB/selfhost-lock.stderr"

run_selfhost_lock_case() {
  write_selfhost_contract false
  reset_selfhost_behavior false 1 '' ''
  : >"$SELFHOST_LOCK_CALL_LOG"
  SELFHOST_LOCK_BACKEND_RC="$1" bash "$BEHAVIOR_TARGET" up \
    >"$selfhost_lock_stdout" 2>"$selfhost_lock_stderr"
  selfhost_rc=$?
}

run_selfhost_lock_case 0
if [[ $selfhost_rc -eq 0 && ! -s "$SELFHOST_LOCK_CALL_LOG" ]] \
  && grep -qF 'running WITHOUT the deploy transaction lock' "$selfhost_lock_stderr" \
  && ! grep -qF 'running WITHOUT the deploy transaction lock' "$selfhost_lock_stdout" \
  && grep -qE 'compose .* build control-plane' "$SELFHOST_DOCKER_LOG" \
  && grep -qE 'compose .* build media-plane' "$SELFHOST_DOCKER_LOG"; then
  pass "self-host up skips lock with absent parent and reaches both builds"
else
  fail "self-host up skips lock with absent parent and reaches both builds (rc=$selfhost_rc)"
fi

mkdir "$SELFHOST_LOCK_PARENT"
run_selfhost_lock_case 0
if [[ $selfhost_rc -eq 0 && $(cat "$SELFHOST_LOCK_CALL_LOG") == '-w 90 9' ]] \
  && ! grep -qF 'running WITHOUT the deploy transaction lock' "$selfhost_lock_stderr" \
  && ! grep -qF 'running WITHOUT the deploy transaction lock' "$selfhost_lock_stdout" \
  && grep -qE 'compose .* build control-plane' "$SELFHOST_DOCKER_LOG" \
  && grep -qE 'compose .* build media-plane' "$SELFHOST_DOCKER_LOG"; then
  pass "self-host up requires lock with present parent and reaches both builds"
else
  fail "self-host up requires lock with present parent and reaches both builds (rc=$selfhost_rc)"
fi

run_selfhost_lock_case 73
if [[ $selfhost_rc -ne 0 && $(cat "$SELFHOST_LOCK_CALL_LOG") == '-w 90 9' ]] \
  && grep -qF 'failed to acquire deploy transaction lock' "$selfhost_lock_stderr" \
  && grep -qF 'rc=73' "$selfhost_lock_stderr" \
  && ! grep -qF 'running WITHOUT the deploy transaction lock' "$selfhost_lock_stderr" \
  && ! grep -qF 'running WITHOUT the deploy transaction lock' "$selfhost_lock_stdout" \
  && ! grep -qE 'compose .* (build|up|stop|down|run)( |$)' "$SELFHOST_DOCKER_LOG"; then
  pass "self-host up obeys lock failure before any Docker mutation"
else
  fail "self-host up obeys lock failure before any Docker mutation (rc=$selfhost_rc)"
fi

cp "$STUB/controller-before-lock-matrix.sh" "$BEHAVIOR_CTL"
rm -f "$SELFHOST_LOCK_PARENT/deploy-transaction.lock"
rmdir "$SELFHOST_LOCK_PARENT"

# Managed mode must not borrow the self-host single-writer exception.
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
selfhost_output="$(ACTIVITY_HISTORY_CONTRACT_MODE=managed \
  COMPOSE_OVERRIDE=docker-compose.production.yml COMPOSE_PROFILES=services \
  bash "$BEHAVIOR_CTL" rebuild 2>&1)"
selfhost_rc=$?
if [[ $selfhost_rc -ne 0 ]] \
  && grep -q 'canonical writer pair is missing' <<<"$selfhost_output" \
  && ! grep -qE 'compose .* up ' "$SELFHOST_DOCKER_LOG"; then
  pass "managed ctl mode still requires both canonical writers"
else
  fail "managed ctl mode incorrectly accepted the self-host single-writer contract"
fi

write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
{
  printf 'export ACTIVITY_HISTORY_CONTRACT_MODE=managed\n'
  printf 'export ACTIVITY_HISTORY_CLUSTER_ENABLED=false\n'
  printf 'export CONTROL_PLANE_REPLICA_COUNT=1\n'
} >"$BEHAVIOR_ROOT/infrastructure/deploy/deploy.env.local"
run_selfhost up
if [[ $selfhost_rc -ne 0 ]] \
  && grep -q 'self-host contract refuses deploy.env.local' <<<"$selfhost_output" \
  && ! grep -qE 'compose .* up ' "$SELFHOST_DOCKER_LOG"; then
  pass "self-host mode refuses a managed-style second rollout writer"
else
  fail "self-host mode could downgrade a two-writer host to single-writer validation"
fi

write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
selfhost_output="$(ACTIVITY_HISTORY_CONTRACT_MODE=selfhost \
  COMPOSE_OVERRIDE=docker-compose.staging.yml COMPOSE_PROFILES=services \
  bash "$BEHAVIOR_CTL" rebuild 2>&1)"
selfhost_rc=$?
if [[ $selfhost_rc -ne 0 ]] \
  && grep -q 'self-host contract requires the production Compose override and services profile' <<<"$selfhost_output" \
  && ! grep -qE 'compose .* up ' "$SELFHOST_DOCKER_LOG"; then
  pass "self-host ctl mode rejects a non-production Compose contract"
else
  fail "self-host ctl mode accepted a non-production Compose contract"
fi

write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
unset STUB_RENDER_GATE STUB_RENDER_REPLICAS STUB_RENDER_INSTANCE_TYPE
selfhost_output="$(INSTANCE_TYPE=saas \
  ACTIVITY_HISTORY_CLUSTER_ENABLED=true CONTROL_PLANE_REPLICA_COUNT=9 \
  ACTIVITY_HISTORY_OPERATOR_NAME='Caller Concord' \
  ACTIVITY_HISTORY_PRIVACY_POLICY_URL='https://caller.invalid/privacy' \
  COMPOSE_ENV_FILES='/tmp/caller-invalid.env' COMPOSE_DISABLE_ENV_FILE=1 \
  CONTROL_PLANE_IMAGE='registry.invalid/old@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' \
  bash "$BEHAVIOR_TARGET" up 2>&1)"
selfhost_rc=$?
if [[ $selfhost_rc -eq 0 ]] \
  && grep -qE 'compose .* build control-plane' "$SELFHOST_DOCKER_LOG" \
  && grep -qE 'compose .* build media-plane' "$SELFHOST_DOCKER_LOG" \
  && grep -qE -- '--env-file .*/behavior-root(/[^ ]*)?/\.env' "$SELFHOST_DOCKER_LOG" \
  && ! grep -qE 'image_selector=registry|inherited_instance=saas|inherited_gate=true|inherited_replicas=9|inherited_operator=Caller|inherited_privacy=https://caller|inherited_env_files=/tmp|inherited_disable_env=1' "$SELFHOST_DOCKER_LOG"; then
  pass "self-host ctl sanitizes inherited config and pins the installer root env file"
else
  sed 's/inherited_operator=[^ ]*/inherited_operator=<redacted>/; s#inherited_privacy=[^ ]*#inherited_privacy=<redacted>#' \
    "$SELFHOST_DOCKER_LOG" >&2
  fail "caller environment or Compose env-file controls overrode the installer-owned self-host root contract"
fi

write_selfhost_contract true
reset_selfhost_behavior true 1 '' 'cp-true\n'
run_selfhost activity-history-activate --confirm-drained
if [[ $selfhost_rc -eq 0 ]] \
  && grep -qE 'compose .* run .*activity-history preflight --confirm-drained' "$SELFHOST_DOCKER_LOG" \
  && grep -qE 'compose .* up -d --no-deps --no-build .*control-plane' "$SELFHOST_DOCKER_LOG" \
  && [[ ! -e "$BEHAVIOR_ROOT/infrastructure/deploy/deploy.env.local" ]]; then
  pass "self-host drained activation works from installer root env without deploy.env.local"
else
  fail "self-host activation did not delegate through the guarded single-writer contract"
fi

old_image='registry.example/concord/control-plane@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
write_selfhost_contract false
reset_selfhost_behavior false 1 '' 'cp-old\n'
run_selfhost activity-history-rollback --confirm-drained --old-image "$old_image"
if [[ $selfhost_rc -eq 0 ]] \
  && grep -qE 'compose .* run .*activity-history disable-all --confirm-drained' "$SELFHOST_DOCKER_LOG" \
  && grep -qE 'compose .* up -d --no-deps --no-build .*control-plane' "$SELFHOST_DOCKER_LOG" \
  && [[ ! -e "$BEHAVIOR_ROOT/infrastructure/deploy/deploy.env.local" ]]; then
  pass "self-host drained rollback works from installer root env without deploy.env.local"
else
  fail "self-host rollback did not delegate through the guarded single-writer contract"
fi

# ── Activation receipt (#2256) ─────────────────────────────────────────
RECEIPT="$BEHAVIOR_ROOT/infrastructure/deploy/.env.activity-history-receipt.local"

write_receipt() {
  printf 'ACTIVITY_HISTORY_ACTIVATION_STATE=%s\nACTIVITY_HISTORY_ACTIVATION_CONTRACT_MODE=%s\nACTIVITY_HISTORY_ACTIVATION_INSTANCE_TYPE=%s\n' \
    "${1:-activated}" "${2:-selfhost}" "${3:-self-hosted}" >"$RECEIPT"
  chmod 600 "$RECEIPT"
}

# Activation persists a durable, 0600, exact-content receipt.
write_selfhost_contract true
reset_selfhost_behavior true 1 '' 'cp-true\n'
run_selfhost activity-history-activate --confirm-drained
receipt_perms="$(stat -c '%a' "$RECEIPT" 2>/dev/null || stat -f '%Lp' "$RECEIPT" 2>/dev/null)"
if [[ $selfhost_rc -eq 0 && -f "$RECEIPT" && "$receipt_perms" == 600 ]] \
  && printf 'ACTIVITY_HISTORY_ACTIVATION_STATE=activated\nACTIVITY_HISTORY_ACTIVATION_CONTRACT_MODE=selfhost\nACTIVITY_HISTORY_ACTIVATION_INSTANCE_TYPE=self-hosted\n' \
    | diff -q - "$RECEIPT" >/dev/null 2>&1; then
  pass "guarded activation persists a mode-0600 activation receipt with exact content"
else
  fail "activation did not persist the expected receipt (rc=$selfhost_rc perms=${receipt_perms:-missing})"
fi

# A FAILED activation (post-start invariant violated) writes no receipt.
write_selfhost_contract true
reset_selfhost_behavior true 1 '' 'cp-false\n'
run_selfhost activity-history-activate --confirm-drained
if [[ $selfhost_rc -ne 0 && ! -e "$RECEIPT" ]]; then
  pass "failed activation does not persist an activation receipt"
else
  fail "failed activation persisted a receipt or exited 0 (rc=$selfhost_rc)"
fi

# stop → up restarts a previously activated stack via the receipt.
write_selfhost_contract true
reset_selfhost_behavior true 1 '' 'cp-true\n'
export STUB_PS_LATER=''
run_selfhost activity-history-activate --confirm-drained
run_selfhost stop
: >"$SELFHOST_DOCKER_LOG"
run_selfhost up
if [[ $selfhost_rc -eq 0 ]] \
  && grep -qE 'compose .* build control-plane' "$SELFHOST_DOCKER_LOG" \
  && grep -qE 'compose .* build media-plane' "$SELFHOST_DOCKER_LOG"; then
  pass "stop → up restarts an activated gate-true stack via the durable receipt"
else
  fail "stop → up refused a previously activated deployment (rc=$selfhost_rc): $selfhost_output"
fi

# down → up restarts a previously activated stack via the receipt.
write_selfhost_contract true
reset_selfhost_behavior true 1 '' 'cp-true\n'
export STUB_PS_LATER=''
run_selfhost activity-history-activate --confirm-drained
run_selfhost down
: >"$SELFHOST_DOCKER_LOG"
run_selfhost up
if [[ $selfhost_rc -eq 0 ]] \
  && grep -qE 'compose .* build control-plane' "$SELFHOST_DOCKER_LOG" \
  && grep -qE 'compose .* build media-plane' "$SELFHOST_DOCKER_LOG"; then
  pass "down → up restarts an activated gate-true stack via the durable receipt"
else
  fail "down → up refused a previously activated deployment (rc=$selfhost_rc): $selfhost_output"
fi

# Malformed / wrong-environment proof is rejected fail-closed.
for variant in unknown-key wrong-mode wrong-state wrong-instance duplicate-key bad-perms; do
  write_selfhost_contract true
  reset_selfhost_behavior true 1 '' ''
  case "$variant" in
    unknown-key)
      write_receipt
      printf 'EXTRA_KEY=1\n' >>"$RECEIPT"
      ;;
    wrong-mode) write_receipt activated managed self-hosted ;;
    wrong-state) write_receipt pending selfhost self-hosted ;;
    wrong-instance) write_receipt activated selfhost some-other-type ;;
    bad-perms)
      write_receipt
      chmod 644 "$RECEIPT"
      ;;
    duplicate-key)
      write_receipt
      printf 'ACTIVITY_HISTORY_ACTIVATION_STATE=activated\n' >>"$RECEIPT"
      ;;
  esac
  run_selfhost up
  if [[ $selfhost_rc -ne 0 ]] && ! grep -qE 'compose .* up ' "$SELFHOST_DOCKER_LOG" \
    && grep -q 'activation receipt is malformed or bound to a different deployment' <<<"$selfhost_output"; then
    pass "receipt variant '$variant' is rejected fail-closed"
  else
    fail "receipt variant '$variant' was not rejected (rc=$selfhost_rc): $selfhost_output"
  fi
done

# A pre-receipt activated stack (live one-container gate-true proof, no
# receipt) gets its receipt BACKFILLED by a routine up (#2326 Codex).
write_selfhost_contract true
reset_selfhost_behavior true 1 'cp-true\n' ''
run_selfhost up
if [[ $selfhost_rc -eq 0 && -f "$RECEIPT" ]] \
  && grep -qE 'compose .* build control-plane' "$SELFHOST_DOCKER_LOG" \
  && grep -qE 'compose .* build media-plane' "$SELFHOST_DOCKER_LOG" \
  && printf 'ACTIVITY_HISTORY_ACTIVATION_STATE=activated\nACTIVITY_HISTORY_ACTIVATION_CONTRACT_MODE=selfhost\nACTIVITY_HISTORY_ACTIVATION_INSTANCE_TYPE=self-hosted\n' \
    | diff -q - "$RECEIPT" >/dev/null 2>&1; then
  pass "routine up backfills the receipt from live gate-true proof"
else
  fail "backfill missing or wrong (rc=$selfhost_rc): $selfhost_output"
fi

# The receipt never authorizes freshstart's volume wipe: gate-true freshstart
# at zero running containers is refused and the receipt is retained (#2326 Codex).
write_selfhost_contract true
reset_selfhost_behavior true 1 '' ''
write_receipt
selfhost_output="$(printf 'y\n' | ACTIVITY_HISTORY_CONTRACT_MODE=selfhost \
  COMPOSE_OVERRIDE=docker-compose.production.yml COMPOSE_PROFILES=services \
  bash "$BEHAVIOR_CTL" freshstart 2>&1)"
selfhost_rc=$?
if [[ $selfhost_rc -ne 0 && -f "$RECEIPT" ]] \
  && ! grep -qE 'compose .* down -v' "$SELFHOST_DOCKER_LOG"; then
  pass "freshstart refuses receipt-only gate-true state and retains the receipt"
else
  fail "freshstart accepted receipt-only authorization for a volume wipe (rc=$selfhost_rc): $selfhost_output"
fi

# Rollback invalidates proof BEFORE destructive mutation (disable-all fails,
# receipt is already gone), and stale gate-true restart is then refused.
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
write_receipt
export STUB_FAIL_DISABLE_ALL=1
run_selfhost activity-history-rollback --confirm-drained --old-image "$old_image"
rollback_rc=$selfhost_rc
unset STUB_FAIL_DISABLE_ALL
if [[ $rollback_rc -ne 0 && ! -e "$RECEIPT" ]]; then
  pass "failed rollback still invalidated the receipt before mutation"
else
  fail "rollback left stale activation authorization behind (rc=$rollback_rc)"
fi
write_selfhost_contract true
export STUB_RENDER_GATE=true STUB_ROOT_GATE=true
printf '0\n' >"$SELFHOST_PS_INDEX"
: >"$SELFHOST_DOCKER_LOG"
run_selfhost up
if [[ $selfhost_rc -ne 0 ]] && ! grep -qE 'compose .* up ' "$SELFHOST_DOCKER_LOG"; then
  pass "post-rollback gate-true up is refused without a receipt"
else
  fail "post-rollback up restarted gate=true without proof"
fi

# freshstart invalidates the receipt before wiping volumes.
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
write_receipt
selfhost_output="$(printf 'y\n' | ACTIVITY_HISTORY_CONTRACT_MODE=selfhost \
  COMPOSE_OVERRIDE=docker-compose.production.yml COMPOSE_PROFILES=services \
  bash "$BEHAVIOR_CTL" freshstart 2>&1)"
selfhost_rc=$?
if [[ $selfhost_rc -eq 0 && ! -e "$RECEIPT" ]] \
  && grep -qE 'compose .* down -v' "$SELFHOST_DOCKER_LOG"; then
  pass "freshstart invalidates the receipt before wiping volumes"
else
  fail "freshstart preserved stale activation authorization (rc=$selfhost_rc): $selfhost_output"
fi

# ── Self-host published-port posture (#3538) ───────────────────────────
# Basenames of every `-f FILE` in whatever argv text arrives on stdin.
f_basenames() {
  grep -oE -- ' -f [^ ]+' | sed -e 's/^ -f //' -e 's#.*/##' | tr '\n' ' ' | sed 's/ $//'
}

# 1. The lifecycle verbs (down/stop/status/logs) and the guarded verbs address the
# SAME compose model: concord-selfhost's -f set equals concord-ctl's, in order.
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
wrapper_model_log="$STUB/bundled-wrapper-model.jsonl"
ctl_model_log="$STUB/bundled-controller-model.jsonl"
: >"$wrapper_model_log"
: >"$ctl_model_log"
SELFHOST_DOCKER_JSON_LOG="$wrapper_model_log" \
  bash -c 'source "$1"; compose config --format json' _ "$BEHAVIOR_TARGET" >/dev/null 2>&1
wrapper_rc=$?
SELFHOST_DOCKER_JSON_LOG="$ctl_model_log" run_selfhost up
if [[ $wrapper_rc -eq 0 && $selfhost_rc -eq 0 ]] \
  && python3 - "$BEHAVIOR_ROOT" "$wrapper_model_log" "$ctl_model_log" <<'ASSERT_BUNDLED_MODEL'
import json, sys
from pathlib import Path
root, *paths = sys.argv[1:]
files = [root + '/' + f for f in ('docker-compose.yml', 'docker-compose.production.yml', 'docker-compose.selfhost.yml')]
expected = {'--env-file': root + '/.env', '--project-directory': root,
            '--project-name': 'concord-selfhost', '--profile': 'services'}
for path in paths:
    seen = False
    for row in map(json.loads, Path(path).read_text().splitlines()):
        args = row['argv']
        if args[:1] != ['compose']: continue
        options, actual_files, i = {}, [], 1
        while i < len(args) and args[i] in ('--env-file', '--project-directory', '--project-name', '--profile', '-f'):
            key, value = args[i:i+2]
            if key == '-f': actual_files.append(value)
            else:
                assert key not in options
                options[key] = value
            i += 2
        if actual_files == ['-']:
            assert options == {'--project-directory': root, '--env-file': '/dev/null'}
            assert args[i:] == ['config', '--format', 'json']
            continue
        assert actual_files == files and options == expected and row['cwd'] == root
        if args[i:] == ['config', '--format', 'json']: seen = True
    assert seen
ASSERT_BUNDLED_MODEL
then
  pass "concord-selfhost and self-host concord-ctl use the same three-file compose set"
else
  fail "bundled wrapper/controller dispatch did not use the exact same isolated three-file model (wrapper_rc=$wrapper_rc ctl_rc=$selfhost_rc)"
fi

# 2. up is refused BEFORE any `compose up` when the posture gate cannot pass, and no
# environment value reaches the output. SENTINEL_ENV rides the render so a leak shows.
posture_refused() {
  [[ $selfhost_rc -ne 0 ]] \
    && ! grep -qE 'compose .* up ' "$SELFHOST_DOCKER_LOG" \
    && grep -q 'selfhost-posture' <<<"$selfhost_output" \
    && ! grep -q 'T2-SENTINEL' <<<"$selfhost_output"
}
POSTGRES_PUBLISHED=',"postgres":{"ports":[{"mode":"ingress","target":5432,"published":"5432","protocol":"tcp"}],"environment":{"SENTINEL_ENV":"T2-SENTINEL"}}'

# Control: the same fragment minus the port publication is legal, so the refusal
# below is caused by the port and not by the fragment plumbing.
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
export STUB_RENDER_EXTRA=',"postgres":{"environment":{"SENTINEL_ENV":"T2-SENTINEL"}}'
run_selfhost up
unset STUB_RENDER_EXTRA
if [[ $selfhost_rc -eq 0 ]] \
  && grep -qE 'compose .* build control-plane' "$SELFHOST_DOCKER_LOG" \
  && ! grep -q 'T2-SENTINEL' <<<"$selfhost_output"; then
  pass "self-host up allows a render with no published ports and prints no env value"
else
  fail "port-less extra service blocked up or leaked an env value (rc=$selfhost_rc)"
fi

write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
export STUB_RENDER_EXTRA="$POSTGRES_PUBLISHED"
run_selfhost up
unset STUB_RENDER_EXTRA
if posture_refused \
  && grep -qF 'or a compose file was changed to publish this. Revert that change.' <<<"$selfhost_output" \
  && grep -qF 'Compose is too old to honour !override' <<<"$selfhost_output"; then
  pass "self-host up refuses a render that publishes postgres 5432, without echoing env values"
else
  fail "up accepted a published postgres port or leaked env (rc=$selfhost_rc): $selfhost_output"
fi

write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
export STUB_ENGINE_VERSION=27.5.1
run_selfhost up
unset STUB_ENGINE_VERSION
if posture_refused; then
  pass "self-host up refuses Docker Engine 27.5.1"
else
  fail "up accepted Engine 27.5.1 (rc=$selfhost_rc): $selfhost_output"
fi

write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
mv "$BEHAVIOR_ROOT/docker-compose.selfhost.yml" "$BEHAVIOR_ROOT/docker-compose.selfhost.yml.away"
run_selfhost up
mv "$BEHAVIOR_ROOT/docker-compose.selfhost.yml.away" "$BEHAVIOR_ROOT/docker-compose.selfhost.yml"
if [[ $selfhost_rc -ne 0 ]] \
  && grep -qxF 'selfhost-storage: model resolution refused' <<<"$selfhost_output" \
  && ! grep -q 'T2-SENTINEL' <<<"$selfhost_output" \
  && ! grep -qE 'compose .* up ' "$SELFHOST_DOCKER_LOG"; then
  pass "self-host up refuses when docker-compose.selfhost.yml is missing"
else
  fail "up ran without the posture overlay (rc=$selfhost_rc): $selfhost_output"
fi

write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
STUB_INSPECT_JSON="$(live_inspect '{}')"
export STUB_PROJECT_IDS=$'cp1\npg1' STUB_INSPECT_JSON
run_selfhost health
if [[ $selfhost_rc -eq 0 ]] \
  && grep -q 'curl .*127\.0\.0\.1:8080/api/v1/client/config' "$SELFHOST_DOCKER_LOG" \
  && ! grep -q 'T2-SENTINEL' <<<"$selfhost_output"; then
  pass "health passes when every live binding is inside the posture"
else
  fail "health failed a clean live set (rc=$selfhost_rc): $selfhost_output"
fi

STUB_INSPECT_JSON="$(live_inspect '{"5432/tcp":[{"HostIp":"0.0.0.0","HostPort":"5432"}]}')"
export STUB_INSPECT_JSON
run_selfhost health
if [[ $selfhost_rc -ne 0 ]] \
  && grep -q 'postgres 5432/tcp host_ip=all' <<<"$selfhost_output" \
  && grep -qF 'Run ./infrastructure/deploy/concord-selfhost up to recreate the listed services' <<<"$selfhost_output" \
  && grep -qF 'an orphan' <<<"$selfhost_output" \
  && ! grep -q 'T2-SENTINEL' <<<"$selfhost_output"; then
  pass "health fails and names a postgres 5432 binding on all addresses"
else
  fail "health accepted a 0.0.0.0:5432 postgres binding (rc=$selfhost_rc): $selfhost_output"
fi

export STUB_PROJECT_IDS=''
run_selfhost health
if [[ $selfhost_rc -ne 0 ]]; then
  pass "health fails when the project has no containers"
else
  fail "health passed with no containers to inspect"
fi
unset STUB_PROJECT_IDS STUB_INSPECT_JSON

# 5. Every self-host rebuild ends with the live check, AFTER the compose up (#3538).
# The check judges what is actually running: a container a rebuild did not recreate
# keeps whatever it was published with, and the render cannot see it.
NONCOMPLIANT_PG='{"5432/tcp":[{"HostIp":"0.0.0.0","HostPort":"5432"}]}'
# Line numbers of the last compose `up` and of the first project-label listing.
up_before_inspect() {
  local up_line inspect_line
  up_line="$(grep -nE 'compose .* up ' "$SELFHOST_DOCKER_LOG" | tail -1 | cut -d: -f1)"
  inspect_line="$(grep -nE 'ps -aq --filter label=com\.docker\.compose\.project=' "$SELFHOST_DOCKER_LOG" | head -1 | cut -d: -f1)"
  [[ -n "$up_line" && -n "$inspect_line" && "$up_line" -lt "$inspect_line" ]]
}
live_refused() {
  [[ $selfhost_rc -ne 0 ]] \
    && grep -qF 'posture: postgres 5432/tcp host_ip=all not allowed' <<<"$selfhost_output" \
    && grep -qF 'Run ./infrastructure/deploy/concord-selfhost up to recreate the listed services' <<<"$selfhost_output" \
    && ! grep -qx 'Done.' <<<"$selfhost_output" \
    && ! grep -q 'T2-SENTINEL' <<<"$selfhost_output" \
    && up_before_inspect
}

# Control: a compliant project completes and prints Done., so the refusals below are
# caused by the live binding and not by the plumbing.
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
run_selfhost up
if [[ $selfhost_rc -eq 0 ]] && grep -qx 'Done.' <<<"$selfhost_output" && up_before_inspect; then
  pass "self-host up with a compliant live set prints Done. after the compose up"
else
  fail "compliant self-host up did not complete after the live check (rc=$selfhost_rc): $selfhost_output"
fi

# Unscoped exit: concord-selfhost up -> guarded rebuild.
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
STUB_INSPECT_JSON="$(live_inspect "$NONCOMPLIANT_PG")"
export STUB_INSPECT_JSON
run_selfhost up
if live_refused; then
  pass "self-host up fails after the compose up when a live container publishes postgres 5432"
else
  fail "up exited 0 / printed Done. / checked before the up with postgres on 0.0.0.0:5432 (rc=$selfhost_rc): $selfhost_output"
fi

# Scoped exit (generic): rebuild <svc> -> `compose up --no-deps --wait <svc>`.
# nats must be a rendered service and running for the scope to stay scoped.
SCOPED_ENV=(ACTIVITY_HISTORY_CONTRACT_MODE=selfhost COMPOSE_OVERRIDE=docker-compose.production.yml COMPOSE_PROFILES=services)
write_selfhost_contract false
reset_selfhost_behavior false 1 'cp-false\n' 'cp-false\n'
export STUB_RENDER_EXTRA=',"nats":{}' STUB_COMPOSE_RUNNING=nats
selfhost_output="$(env "${SCOPED_ENV[@]}" bash "$BEHAVIOR_CTL" rebuild nats 2>&1)"
selfhost_rc=$?
if [[ $selfhost_rc -eq 0 ]] && grep -qE 'compose .* up -d --build --no-deps --wait .* nats' "$SELFHOST_DOCKER_LOG" \
  && ! grep -qE 'escalating|remaining services' <<<"$selfhost_output" \
  && grep -qx 'Done.' <<<"$selfhost_output"; then
  pass "scoped self-host rebuild (control, compliant live set) takes the generic scoped exit and prints Done."
else
  fail "scoped control did not reach the generic scoped exit (rc=$selfhost_rc): $selfhost_output"
fi
# This is the branch that historically printed managed recovery advice.
if [[ $selfhost_rc -eq 0 ]] && grep -qx 'Done.' <<<"$selfhost_output" \
  && ! grep -qF 'sync-releases' <<<"$selfhost_output"; then
  pass "scoped self-host rebuild success omits managed release recovery advice"
else fail "scoped self-host rebuild suggests managed recovery or did not finish: $selfhost_output"; fi
write_selfhost_contract false
reset_selfhost_behavior false 1 'cp-false\n' 'cp-false\n'
STUB_INSPECT_JSON="$(live_inspect "$NONCOMPLIANT_PG")"
export STUB_INSPECT_JSON STUB_RENDER_EXTRA=',"nats":{}' STUB_COMPOSE_RUNNING=nats
selfhost_output="$(env "${SCOPED_ENV[@]}" bash "$BEHAVIOR_CTL" rebuild nats 2>&1)"
selfhost_rc=$?
unset STUB_RENDER_EXTRA STUB_COMPOSE_RUNNING
if live_refused && grep -qE 'compose .* up -d --build --no-deps --wait .* nats' "$SELFHOST_DOCKER_LOG" \
  && ! grep -qE 'escalating|remaining services' <<<"$selfhost_output"; then
  pass "scoped self-host rebuild fails after the scoped up when a live container publishes postgres 5432"
else
  fail "scoped rebuild exited 0 / printed Done. / skipped the live check (rc=$selfhost_rc): $selfhost_output"
fi

# Scoped exit (media-plane): rebuild media-plane -> voice_enforcement_rollout_media_rebuild.
write_selfhost_contract false
reset_selfhost_behavior false 1 'cp-false\n' 'cp-false\n'
export STUB_RENDER_EXTRA=',"media-plane":{}' STUB_COMPOSE_RUNNING=media-plane
selfhost_output="$(env "${SCOPED_ENV[@]}" bash "$BEHAVIOR_CTL" rebuild media-plane 2>&1)"
selfhost_rc=$?
if [[ $selfhost_rc -eq 0 ]] && grep -q 'voice-enforcement-rollout: draining the old media-plane' <<<"$selfhost_output" \
  && ! grep -qE 'compose .* build control-plane' "$SELFHOST_DOCKER_LOG" \
  && ! grep -qE 'escalating|remaining services' <<<"$selfhost_output" \
  && grep -qx 'Done.' <<<"$selfhost_output"; then
  pass "media-plane-scoped self-host rebuild (control, compliant live set) takes the media handoff exit and prints Done."
else
  fail "media-plane scoped control did not reach the media handoff exit (rc=$selfhost_rc): $selfhost_output"
fi
write_selfhost_contract false
reset_selfhost_behavior false 1 'cp-false\n' 'cp-false\n'
STUB_INSPECT_JSON="$(live_inspect "$NONCOMPLIANT_PG")"
export STUB_INSPECT_JSON STUB_RENDER_EXTRA=',"media-plane":{}' STUB_COMPOSE_RUNNING=media-plane
selfhost_output="$(env "${SCOPED_ENV[@]}" bash "$BEHAVIOR_CTL" rebuild media-plane 2>&1)"
selfhost_rc=$?
unset STUB_RENDER_EXTRA STUB_COMPOSE_RUNNING
if live_refused && grep -q 'voice-enforcement-rollout: draining the old media-plane' <<<"$selfhost_output" \
  && ! grep -qE 'compose .* build control-plane' "$SELFHOST_DOCKER_LOG" \
  && ! grep -qE 'escalating|remaining services' <<<"$selfhost_output"; then
  pass "media-plane-scoped self-host rebuild fails after the media handoff when a live container publishes postgres 5432"
else
  fail "media-plane scoped rebuild exited 0 / printed Done. / skipped the live check (rc=$selfhost_rc): $selfhost_output"
fi

# Managed mode checks NATS bindings, but never runs the self-host live-port check:
# no whole-document inspect and no project-label listing.
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
{
  printf 'ACTIVITY_HISTORY_CLUSTER_ENABLED=false\n'
  printf 'CONTROL_PLANE_REPLICA_COUNT=1\n'
  printf 'ACTIVITY_HISTORY_OPERATOR_NAME=\n'
  printf 'ACTIVITY_HISTORY_PRIVACY_POLICY_URL=\n'
} >"$BEHAVIOR_ROOT/infrastructure/deploy/deploy.env.local"
STUB_INSPECT_JSON="$(live_inspect "$NONCOMPLIANT_PG")"
export STUB_INSPECT_JSON
export STUB_RENDER_EXTRA=',"nats":{}'
selfhost_output="$(ACTIVITY_HISTORY_CONTRACT_MODE=managed \
  COMPOSE_OVERRIDE=docker-compose.production.yml COMPOSE_PROFILES=services \
  bash "$BEHAVIOR_CTL" rebuild 2>&1)"
selfhost_rc=$?
rm -f "$BEHAVIOR_ROOT/infrastructure/deploy/deploy.env.local"
if [[ $selfhost_rc -eq 0 ]] && grep -qx 'Done.' <<<"$selfhost_output" \
  && grep -qE 'compose .* up ' "$SELFHOST_DOCKER_LOG" \
  && ! grep -qE 'docker .* inspect [^-]' "$SELFHOST_DOCKER_LOG" \
  && ! grep -qE 'ps -aq --filter label=com\.docker\.compose\.project=' "$SELFHOST_DOCKER_LOG" \
  && ! grep -q 'selfhost-posture' <<<"$selfhost_output"; then
  pass "managed-mode rebuild reaches Done. without the self-host whole-project live check"
else
  fail "managed-mode rebuild ran the self-host whole-project live check or did not complete (rc=$selfhost_rc): $selfhost_output"
fi
# Preserve the unscoped test above; advice exists only on generic scoped rebuild.
write_selfhost_contract false
reset_selfhost_behavior false 1 'cp-false\n' 'cp-false\n'
{
  printf 'ACTIVITY_HISTORY_CLUSTER_ENABLED=false\n'
  printf 'CONTROL_PLANE_REPLICA_COUNT=1\n'
  printf 'ACTIVITY_HISTORY_OPERATOR_NAME=\n'
  printf 'ACTIVITY_HISTORY_PRIVACY_POLICY_URL=\n'
} >"$BEHAVIOR_ROOT/infrastructure/deploy/deploy.env.local"
export STUB_RENDER_EXTRA=',"nats":{}' STUB_COMPOSE_RUNNING=nats
selfhost_output="$(ACTIVITY_HISTORY_CONTRACT_MODE=managed \
  COMPOSE_OVERRIDE=docker-compose.production.yml COMPOSE_PROFILES=services \
  bash "$BEHAVIOR_CTL" rebuild nats 2>&1)"
selfhost_rc=$?
rm -f "$BEHAVIOR_ROOT/infrastructure/deploy/deploy.env.local"
if [[ $selfhost_rc -eq 0 ]] && grep -qx 'Done.' <<<"$selfhost_output" \
  && grep -qE 'compose .* up -d --build --no-deps --wait .* nats' "$SELFHOST_DOCKER_LOG" \
  && ! grep -qE 'escalating|remaining services' <<<"$selfhost_output" \
  && grep -qF 'concord-ctl.sh sync-releases' <<<"$selfhost_output" \
  && grep -qF 'sync-releases.yml' <<<"$selfhost_output"; then
  pass "managed scoped rebuild retains release recovery command and workflow advice"
else fail "managed scoped rebuild lost release recovery advice or branch reachability: $selfhost_output"; fi
unset STUB_RENDER_EXTRA STUB_COMPOSE_RUNNING
unset STUB_PROJECT_IDS STUB_INSPECT_JSON

# ── Fail-closed paths of the live check and the gate (#3538 review) ─────
# Each case asserts a non-zero exit, the message that names the cause, and that the
# success line never printed. The stub honours -a/-q and the exact label value, and
# prints inspect JSON only for the ids it was given, so a wrong flag or a wrong project
# name in the controller surfaces here instead of being absorbed by a lenient stub.
refused_cleanly() { # MESSAGE — rc != 0, MESSAGE present, no Done., no env value
  [[ $selfhost_rc -ne 0 ]] \
    && grep -qF -- "$1" <<<"$selfhost_output" \
    && ! grep -qx 'Done.' <<<"$selfhost_output" \
    && ! grep -q 'T2-SENTINEL' <<<"$selfhost_output"
}
no_compose_up() { ! grep -qE 'compose .* up ' "$SELFHOST_DOCKER_LOG"; }

# Engine unreadable (daemon down) -> refuse before any compose up.
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
export STUB_VERSION_FAIL=1
run_selfhost up
if refused_cleanly 'Docker Engine server version unavailable or invalid' && no_compose_up; then
  pass "self-host up refuses when the Docker Engine version is unreadable"
else
  fail "up ran with an unreadable Engine version (rc=$selfhost_rc): $selfhost_output"
fi

# Engine boundary in the controller gate, including supported distribution metadata.
for case_ in "29.8.2:0" "29.8.2+dfsg1:0" "29.8.3:0" "30.0.0:0" "29.8.1:1" "29.8.1+dfsg1:1" "28.3.3:1"; do
  ev="${case_%%:*}"; want="${case_##*:}"
  write_selfhost_contract false
  reset_selfhost_behavior false 1 '' ''
  export STUB_ENGINE_VERSION="$ev"
  run_selfhost up
  if [[ "$want" == 0 ]]; then
    if [[ $selfhost_rc -eq 0 ]] && grep -qx 'Done.' <<<"$selfhost_output"; then
      pass "controller gate: Docker Engine $ev is accepted"
    else
      fail "controller gate refused Engine $ev (rc=$selfhost_rc): $selfhost_output"
    fi
  else
    if refused_cleanly 'older than 29.8.2' && grep -qF 'CVE-2026-92543' <<<"$selfhost_output" && no_compose_up; then
      pass "controller gate: Docker Engine $ev is refused (older than 29.8.2, CVE-2026-92543)"
    else
      fail "controller gate accepted Engine $ev (rc=$selfhost_rc): $selfhost_output"
    fi
  fi
done

# These cases reach the real controller entrypoint and must stop before Compose up.
invalid_engines=("" "garbage" "v29.8.2" "029.8.2" "29.08.2" "29.8.02" "29.8" "29.8.2-rc1" "29.8.2~rc1" "29.8.2+" "29.8.2+bad_metadata" "29.8.2 REGISTRY_SENTINEL" " 29.8.2" "29.8.2 " $'29.8.2\r' $'29.8.2\n' $'29.8.2\n29.8.3')
for ev in "${invalid_engines[@]}"; do
  write_selfhost_contract false
  reset_selfhost_behavior false 1 '' ''
  export STUB_ENGINE_VERSION="$ev"
  run_selfhost up
  if refused_cleanly 'Docker Engine' && no_compose_up && ! grep -qF 'REGISTRY_SENTINEL' <<<"$selfhost_output"; then
    pass "controller refuses malformed Engine response $(printf %q "$ev")"
  else
    fail "controller admitted malformed Engine response $(printf %q "$ev") (rc=$selfhost_rc): $selfhost_output"
  fi
done
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
export STUB_ENGINE_VERSION=29.8.2 STUB_ENGINE_EXIT=42
run_selfhost up
if refused_cleanly 'Docker Engine' && no_compose_up; then
  pass "controller refuses valid-looking Engine stdout from failed query"
else
  fail "controller admitted valid-looking Engine stdout from failed query (rc=$selfhost_rc): $selfhost_output"
fi
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
export STUB_ENGINE_VERSION=29.8.2 STUB_ENGINE_END=''
run_selfhost up
if [[ $selfhost_rc -eq 0 ]] && grep -qx 'Done.' <<<"$selfhost_output"; then
  pass "controller admits stable Engine output without terminal LF"
else
  fail "controller refused stable Engine output without terminal LF: $selfhost_output"
fi

# python3 missing: PATH holds only symlinks to the tools the controller needs plus the
# docker stub, so python3 is genuinely absent from the gate's point of view.
NOPY="$STUB/nopy-bin"
mkdir -p "$NOPY"
for tool in bash env dirname basename cat sed grep egrep head tail sort tr mktemp rm date cut \
            mkdir touch chmod ls wc awk readlink uname id printf sleep cp mv tee; do
  tool_path="$(command -v "$tool" 2>/dev/null)" && [[ -x "$tool_path" ]] && ln -sf "$tool_path" "$NOPY/$tool"
done
cp "$BEHAVIOR_STUB/docker" "$BEHAVIOR_STUB/curl" "$NOPY/"
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
selfhost_output="$(PATH="$NOPY" "$(command -v bash)" "$BEHAVIOR_TARGET" up 2>&1)"
selfhost_rc=$?
if refused_cleanly 'python3 is required' && no_compose_up; then
  pass "self-host up refuses when python3 is missing"
else
  fail "up ran without python3 (rc=$selfhost_rc): $selfhost_output"
fi

# Render failure: Compose's error is NOT echoed (it can quote an .env line, value and all);
# the shared resolver gives its fixed model-resolution diagnostic, and nothing starts.
# The stub's error carries a secret-shaped sentinel standing in for that value.
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
export STUB_RENDER_FAIL=1
run_selfhost up
if refused_cleanly 'selfhost-storage: model resolution refused' \
  && grep -qxF 'selfhost-storage: model resolution refused' <<<"$selfhost_output" \
  && ! grep -qF 'SECRET_SENTINEL_9f3' <<<"$selfhost_output" \
  && ! grep -qF 'STUB-COMPOSE-ERROR' <<<"$selfhost_output" \
  && ! grep -qF 'Compose said:' <<<"$selfhost_output" \
  && ! grep -qF 'validate-config' <<<"$selfhost_output" && no_compose_up; then
  pass "render failure gives the fixed model-resolution diagnostic and never echoes Compose's error"
else
  fail "render failure echoed Compose's error or omitted the fixed diagnostic (rc=$selfhost_rc): $selfhost_output"
fi

# `docker ps` fails during the live check -> refuse (the up already ran).
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
export STUB_PS_FAIL=1
run_selfhost up
if refused_cleanly 'could not list containers for project concord-selfhost'; then
  pass "live check refuses when docker ps fails"
else
  fail "live check passed with a failing docker ps (rc=$selfhost_rc): $selfhost_output"
fi

# `docker inspect` fails -> refuse.
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
export STUB_INSPECT_FAIL=1
run_selfhost up
if refused_cleanly 'docker inspect failed'; then
  pass "live check refuses when docker inspect fails"
else
  fail "live check passed with a failing docker inspect (rc=$selfhost_rc): $selfhost_output"
fi

# The checker exits 2 (a container with no compose service label cannot be judged): the
# controller refuses through the `*)` arm of selfhost_posture_verdict, naming no value.
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
export STUB_PROJECT_IDS=cp1
export STUB_INSPECT_JSON='[{"Config":{"Labels":{},"Env":["SENTINEL_ENV=T2-SENTINEL"]},"HostConfig":{"PortBindings":{},"NetworkMode":"default"}}]'
run_selfhost up
if refused_cleanly 'the live port model could not be judged'; then
  pass "controller refuses when the checker exits 2 (unjudgeable live model)"
else
  fail "controller accepted an unjudgeable live model (rc=$selfhost_rc): $selfhost_output"
fi

# Render returns a project name other than the default: the listing must use THAT name.
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
export STUB_RENDER_NAME=customproj STUB_PROJECT_NAME=customproj
run_selfhost up
if [[ $selfhost_rc -eq 0 ]] && grep -qx 'Done.' <<<"$selfhost_output" \
  && grep -qF 'ps -aq --filter label=com.docker.compose.project=customproj' "$SELFHOST_DOCKER_LOG"; then
  pass "live check lists containers by the rendered project name"
else
  fail "live check ignored the rendered project name (rc=$selfhost_rc): $selfhost_output"
fi
# ...and a mismatch (containers exist under another name) is a refusal, not a pass.
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
export STUB_RENDER_NAME=customproj
run_selfhost up
if refused_cleanly 'no containers for project customproj'; then
  pass "live check refuses when the rendered project has no containers"
else
  fail "live check passed for a project with no containers (rc=$selfhost_rc): $selfhost_output"
fi

# A STOPPED container keeps its bindings: it must be listed (-a) and judged.
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
export STUB_PROJECT_IDS=cp1 STUB_STOPPED_IDS=pg1
STUB_INSPECT_JSON="$(live_inspect "$NONCOMPLIANT_PG")"
export STUB_INSPECT_JSON
run_selfhost up
if live_refused; then
  pass "live check judges a stopped container's bindings"
else
  fail "live check skipped a stopped container publishing postgres 5432 (rc=$selfhost_rc): $selfhost_output"
fi

# Non-self-host mode: selfhost-port-check is refused with exit 2 (it is self-host only).
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
selfhost_output="$(ACTIVITY_HISTORY_CONTRACT_MODE=managed COMPOSE_OVERRIDE=docker-compose.production.yml \
  COMPOSE_PROFILES=services bash "$BEHAVIOR_CTL" selfhost-port-check 2>&1)"
selfhost_rc=$?
if [[ $selfhost_rc -eq 2 ]] && grep -qF 'selfhost-port-check runs only in the self-host contract mode' <<<"$selfhost_output" \
  && ! grep -qE 'ps -aq --filter label=com\.docker\.compose\.project=' "$SELFHOST_DOCKER_LOG"; then
  pass "selfhost-port-check exits 2 outside self-host mode without listing containers"
else
  fail "selfhost-port-check ran or exited wrong outside self-host mode (rc=$selfhost_rc): $selfhost_output"
fi

# health: the HTTP probe failing is a failure even when the live set is clean.
for code in 503 000; do
  write_selfhost_contract false
  reset_selfhost_behavior false 1 '' ''
  export STUB_CONFIG_CODE="$code"
  run_selfhost health
  if [[ $selfhost_rc -ne 0 ]] \
    && grep -qF "control-plane unhealthy (HTTP $code)" <<<"$selfhost_output" \
    && grep -qF 'published ports match the self-host posture' <<<"$selfhost_output"; then
    pass "health fails on HTTP $code from the control-plane despite a clean live set"
  else
    fail "health passed with the control-plane answering $code (rc=$selfhost_rc): $selfhost_output"
  fi
done

# ── Post-up check on the verbs that start containers outside `rebuild` ──
# activate / rollback / freshstart each end by judging the live set, AFTER their compose up.
write_selfhost_contract true
reset_selfhost_behavior true 1 '' 'cp-true\n'
STUB_INSPECT_JSON="$(live_inspect "$NONCOMPLIANT_PG")"
export STUB_INSPECT_JSON
run_selfhost activity-history-activate --confirm-drained
if live_refused; then
  pass "activity-history-activate fails after its compose up when a live container publishes postgres 5432"
else
  fail "activity-history-activate skipped the live check (rc=$selfhost_rc): $selfhost_output"
fi

write_selfhost_contract false
reset_selfhost_behavior false 1 '' 'cp-old\n'
STUB_INSPECT_JSON="$(live_inspect "$NONCOMPLIANT_PG")"
export STUB_INSPECT_JSON
run_selfhost activity-history-rollback --confirm-drained --old-image "$old_image"
if live_refused; then
  pass "activity-history-rollback fails after its compose up when a live container publishes postgres 5432"
else
  fail "activity-history-rollback skipped the live check (rc=$selfhost_rc): $selfhost_output"
fi

run_freshstart() { # prints "y" to the confirmation prompt
  write_selfhost_contract false
  reset_selfhost_behavior false 1 '' ''
  selfhost_output="$(printf 'y\n' | ACTIVITY_HISTORY_CONTRACT_MODE=selfhost \
    COMPOSE_OVERRIDE=docker-compose.production.yml COMPOSE_PROFILES=services \
    bash "$BEHAVIOR_CTL" freshstart 2>&1)"
  selfhost_rc=$?
}
run_freshstart
if [[ $selfhost_rc -eq 0 ]] && grep -qF 'Fresh start complete.' <<<"$selfhost_output" && up_before_inspect; then
  pass "freshstart with a compliant live set completes after the live check"
else
  fail "compliant freshstart did not complete after the live check (rc=$selfhost_rc): $selfhost_output"
fi
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
STUB_INSPECT_JSON="$(live_inspect "$NONCOMPLIANT_PG")"
export STUB_INSPECT_JSON
selfhost_output="$(printf 'y\n' | ACTIVITY_HISTORY_CONTRACT_MODE=selfhost \
  COMPOSE_OVERRIDE=docker-compose.production.yml COMPOSE_PROFILES=services \
  bash "$BEHAVIOR_CTL" freshstart 2>&1)"
selfhost_rc=$?
if [[ $selfhost_rc -ne 0 ]] \
  && grep -qF 'posture: postgres 5432/tcp host_ip=all not allowed' <<<"$selfhost_output" \
  && ! grep -qF 'Fresh start complete.' <<<"$selfhost_output" \
  && up_before_inspect; then
  pass "freshstart fails without its success line when a live container publishes postgres 5432"
else
  fail "freshstart completed or skipped the live check with postgres on 0.0.0.0:5432 (rc=$selfhost_rc): $selfhost_output"
fi

# Managed mode: freshstart inspects NATS, but skips the self-host project-wide check.
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
{
  printf 'ACTIVITY_HISTORY_CLUSTER_ENABLED=false\n'
  printf 'CONTROL_PLANE_REPLICA_COUNT=1\n'
  printf 'ACTIVITY_HISTORY_OPERATOR_NAME=\n'
  printf 'ACTIVITY_HISTORY_PRIVACY_POLICY_URL=\n'
} >"$BEHAVIOR_ROOT/infrastructure/deploy/deploy.env.local"
STUB_INSPECT_JSON="$(live_inspect "$NONCOMPLIANT_PG")"
export STUB_INSPECT_JSON
selfhost_output="$(printf 'y\n' | ACTIVITY_HISTORY_CONTRACT_MODE=managed STUB_RENDER_EXTRA=',"nats":{}' \
  COMPOSE_OVERRIDE=docker-compose.production.yml COMPOSE_PROFILES=services \
  bash "$BEHAVIOR_CTL" freshstart 2>&1)"
selfhost_rc=$?
rm -f "$BEHAVIOR_ROOT/infrastructure/deploy/deploy.env.local"
if [[ $selfhost_rc -eq 0 ]] && grep -qF 'Fresh start complete.' <<<"$selfhost_output" \
  && grep -qE 'compose .* up -d --no-deps --no-build --pull missing --wait --wait-timeout 180 nats' "$SELFHOST_DOCKER_LOG" \
  && ! grep -qE 'ps -aq --filter label=com\.docker\.compose\.project=' "$SELFHOST_DOCKER_LOG" \
  && ! grep -E ' inspect ' "$SELFHOST_DOCKER_LOG" | grep -vqE -- '--format| image inspect ' \
  && ! grep -q 'selfhost-posture' <<<"$selfhost_output"; then
  pass "managed-mode freshstart reconciles NATS without a self-host project walk"
else
  fail "managed-mode freshstart missed NATS reconciliation or ran the self-host live check (rc=$selfhost_rc): $selfhost_output"
fi

# ── Docker daemon routing config (#3538) ────────────────────────────────
# allow-direct-routing opens every published port to direct routing, so a port published on
# 127.0.0.1 is reachable through the container's own address. The gate reads daemon.json
# (SELFHOST_DOCKER_DAEMON_JSON, pointed at the temp dir above) and asks pgrep for a dockerd
# started with the flag. Every refusal below must land before any `compose up`.
direct_routing_refused() { # rc != 0, the message names the setting, no up, no success line
  refused_cleanly 'allow-direct-routing' && no_compose_up
}
pgrep_asked() { grep -qF 'pgrep -f dockerd.*--allow-direct-routing' "$SELFHOST_DOCKER_LOG"; }

write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
printf '{"allow-direct-routing": true}\n' >"$SELFHOST_DOCKER_DAEMON_JSON"
run_selfhost up
if direct_routing_refused; then
  pass "self-host up refuses allow-direct-routing in daemon.json before any compose up"
else
  fail "up ran with allow-direct-routing enabled in daemon.json (rc=$selfhost_rc): $selfhost_output"
fi
run_selfhost health
if refused_cleanly 'allow-direct-routing'; then
  pass "self-host health refuses allow-direct-routing in daemon.json"
else
  fail "health passed with allow-direct-routing enabled in daemon.json (rc=$selfhost_rc): $selfhost_output"
fi

# Controls: the same file with the flag off, or with only unrelated keys, is legal, so the
# refusal above is caused by the flag and not by the file existing.
for body in '{"allow-direct-routing": false}' '{"log-driver": "json-file", "live-restore": true}'; do
  write_selfhost_contract false
  reset_selfhost_behavior false 1 '' ''
  printf '%s\n' "$body" >"$SELFHOST_DOCKER_DAEMON_JSON"
  run_selfhost up
  if [[ $selfhost_rc -eq 0 ]] && grep -qx 'Done.' <<<"$selfhost_output" && pgrep_asked; then
    pass "self-host up proceeds with daemon.json $body"
  else
    fail "up refused a daemon.json that does not enable direct routing: $body (rc=$selfhost_rc): $selfhost_output"
  fi
done

# A daemon.json that exists but cannot be read cannot rule the flag out: refuse.
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
printf '{"log-driver": "json-file"}\n' >"$SELFHOST_DOCKER_DAEMON_JSON"
chmod 000 "$SELFHOST_DOCKER_DAEMON_JSON"
if [[ -r "$SELFHOST_DOCKER_DAEMON_JSON" ]]; then
  echo "SKIP: an unreadable daemon.json is still readable here (running as root)"
else
  run_selfhost up
  if direct_routing_refused && grep -qF 'cannot read' <<<"$selfhost_output"; then
    pass "self-host up refuses an unreadable daemon.json before any compose up"
  else
    fail "up ran past an unreadable daemon.json (rc=$selfhost_rc): $selfhost_output"
  fi
fi
chmod 600 "$SELFHOST_DOCKER_DAEMON_JSON"

# A daemon.json the gate cannot parse cannot rule the flag out either: dockerd reads the file
# only at start, so a broken file can sit beside a daemon still running with the setting.
for body in '{bad json' '[1]'; do
  write_selfhost_contract false
  reset_selfhost_behavior false 1 '' ''
  printf '%s\n' "$body" >"$SELFHOST_DOCKER_DAEMON_JSON"
  run_selfhost up
  if refused_cleanly 'cannot parse' && grep -qF 'allow-direct-routing' <<<"$selfhost_output" && no_compose_up; then
    pass "self-host up refuses an unparseable or non-object daemon.json ($body) before any compose up"
  else
    fail "up ran past a daemon.json the gate cannot parse: $body (rc=$selfhost_rc): $selfhost_output"
  fi
done

# Anything but absent or false counts as set, so the string "true" is refused too.
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
printf '{"allow-direct-routing": "true"}\n' >"$SELFHOST_DOCKER_DAEMON_JSON"
run_selfhost up
if direct_routing_refused && grep -qF 'Docker is configured with allow-direct-routing' <<<"$selfhost_output"; then
  pass "self-host up refuses allow-direct-routing set to the string \"true\" before any compose up"
else
  fail "up ran with allow-direct-routing set to a string (rc=$selfhost_rc): $selfhost_output"
fi

# dockerd started with --allow-direct-routing on its command line, no daemon.json at all.
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
export STUB_PGREP_MATCH=1
run_selfhost up
if direct_routing_refused && pgrep_asked; then
  pass "self-host up refuses a dockerd running with --allow-direct-routing before any compose up"
else
  fail "up ran with a dockerd matching --allow-direct-routing (rc=$selfhost_rc): $selfhost_output"
fi

# ── Networks that expose a container with nothing published (#3538) ────
# The live check also judges every network a project container is attached to: a routed or
# unprotected bridge, a macvlan network, or an unreadable network list each make a container
# reachable (or unjudgeable) without any PortBindings entry. Each case runs `health`, and
# `up` whose post-up check must land AFTER the compose up and print no success line.
net_setup() { # NETWORKS_JSON NET_ID NET_NAME — a project whose containers sit on that network
  write_selfhost_contract false
  reset_selfhost_behavior false 1 '' ''
  export STUB_NETWORKS_JSON="$1"
  STUB_INSPECT_JSON="$(live_inspect '{}' "$2" "$3")"
  export STUB_INSPECT_JSON
}
net_refused() { # MESSAGE — rc != 0, MESSAGE named, no success line, no env value
  refused_cleanly "$1"
}
# net_case LABEL NETWORKS_JSON NET_ID NET_NAME MESSAGE [ENV=VALUE...] — refused by both verbs.
net_case() {
  local label="$1" nets="$2" id="$3" name="$4" message="$5"; shift 5
  net_setup "$nets" "$id" "$name"
  local pair; for pair in "$@"; do export "${pair?}"; done
  run_selfhost health
  if net_refused "$message"; then
    pass "health refuses $label"
  else
    fail "health accepted $label (rc=$selfhost_rc): $selfhost_output"
  fi
  net_setup "$nets" "$id" "$name"
  for pair in "$@"; do export "${pair?}"; done
  run_selfhost up
  if net_refused "$message" && up_before_inspect; then
    pass "self-host up refuses $label after the compose up"
  else
    fail "self-host up accepted $label or checked before the compose up (rc=$selfhost_rc): $selfhost_output"
  fi
}
REFUSED_TEXT='refused (reachable without a publication)'
GW4='com.docker.network.bridge.gateway_mode_ipv4'
GW6='com.docker.network.bridge.gateway_mode_ipv6'

net_case "a bridge with gateway_mode_ipv4=routed" \
  "[{\"Id\":\"net-routed\",\"Name\":\"lan\",\"Driver\":\"bridge\",\"Options\":{\"$GW4\":\"routed\"}}]" \
  net-routed lan "posture: control-plane network=lan $REFUSED_TEXT"
net_case "a bridge with gateway_mode_ipv6=routed" \
  "[{\"Id\":\"net-routed6\",\"Name\":\"lan6\",\"Driver\":\"bridge\",\"Options\":{\"$GW6\":\"routed\"}}]" \
  net-routed6 lan6 "posture: control-plane network=lan6 $REFUSED_TEXT"
net_case "a bridge with gateway_mode_ipv4=nat-unprotected" \
  "[{\"Id\":\"net-unprot\",\"Name\":\"lanu\",\"Driver\":\"bridge\",\"Options\":{\"$GW4\":\"nat-unprotected\"}}]" \
  net-unprot lanu "posture: control-plane network=lanu $REFUSED_TEXT"
net_case "a macvlan network" \
  '[{"Id":"net-mac","Name":"maclan","Driver":"macvlan","Options":{}}]' \
  net-mac maclan "posture: control-plane network=maclan $REFUSED_TEXT"

# Fail closed when the networks cannot be listed or judged.
net_case "a failing docker inspect of the container network ids" \
  '[{"Id":"net-plain","Name":"concord_default","Driver":"bridge","Options":{}}]' \
  net-plain concord_default 'docker inspect failed' STUB_INSPECT_NETIDS_FAIL=1
net_case "a failing docker network inspect" \
  '[{"Id":"net-plain","Name":"concord_default","Driver":"bridge","Options":{}}]' \
  net-plain concord_default 'docker network inspect failed' STUB_NETWORK_INSPECT_FAIL=1
net_case "a container attached to a network the daemon does not know" \
  '[{"Id":"net-other","Name":"other","Driver":"bridge","Options":{}}]' \
  net-gone gone 'docker network inspect failed'
# A daemon that answers 0 with fewer networks than asked for: the checker still refuses.
net_case "a container whose network is missing from the network list" \
  '[{"Id":"net-other","Name":"other","Driver":"bridge","Options":{}}]' \
  net-gone gone 'the live port model could not be judged' STUB_NETWORK_INSPECT_PARTIAL_OK=1

# Controls: plain bridges pass, so the refusals above are caused by the network and not by
# the plumbing; and one network shared by every container is asked for once.
for nets in \
  '[{"Id":"net-plain","Name":"concord_default","Driver":"bridge","Options":{}}]' \
  "[{\"Id\":\"net-plain\",\"Name\":\"concord_default\",\"Driver\":\"bridge\",\"Options\":{\"$GW4\":\"nat\",\"$GW6\":\"isolated\"}}]"; do
  net_setup "$nets" net-plain concord_default
  run_selfhost up
  if [[ $selfhost_rc -eq 0 ]] && grep -qx 'Done.' <<<"$selfhost_output" && up_before_inspect \
    && [[ "$(grep -cE 'network inspect net-plain$' "$SELFHOST_DOCKER_LOG")" == 1 ]]; then
    pass "self-host up passes a plain bridge and asks for its id once"
  else
    fail "self-host up refused a plain bridge or asked for its id more than once (rc=$selfhost_rc): $selfhost_output"
  fi
done
net_setup '[{"Id":"net-plain","Name":"concord_default","Driver":"bridge","Options":{}}]' net-plain concord_default
run_selfhost health
if [[ $selfhost_rc -eq 0 ]] && grep -qF 'published ports match the self-host posture' <<<"$selfhost_output"; then
  pass "health passes when the containers sit on a plain bridge"
else
  fail "health refused a plain bridge (rc=$selfhost_rc): $selfhost_output"
fi

# ── Remediation hints follow the KIND of violation (#3538) ─────────────
# A publication is fixed by recreating the service; a network attachment is not (a network
# attached by hand survives a recreate). The refusal prints each hint only for the kind of
# violation line it is about, and both when the lines are mixed.
RECREATE_HINT='to recreate the listed services'
ORPHAN_HINT='an orphan'
TOO_OLD_HINT='Compose is too old'
NETWORK_HINT='A listed network makes the container reachable'
DISCONNECT_HINT='docker network disconnect <network> <container>'
has() { grep -qF -- "$1" <<<"$selfhost_output"; }
ROUTED_NET="[{\"Id\":\"net-routed\",\"Name\":\"lan\",\"Driver\":\"bridge\",\"Options\":{\"$GW4\":\"routed\"}}]"

# 1. A live network violation: the network hint and the disconnect command, no recreate advice.
net_setup "$ROUTED_NET" net-routed lan
run_selfhost health
if [[ $selfhost_rc -ne 0 ]] && has 'selfhost-posture: refusing — outside the self-host allowlist:' \
  && has "posture: control-plane network=lan $REFUSED_TEXT" \
  && has "$NETWORK_HINT" && has "$DISCONNECT_HINT" \
  && ! has "$RECREATE_HINT" && ! has "$ORPHAN_HINT" && ! has "$TOO_OLD_HINT"; then
  pass "live network refusal tells the operator to disconnect the network, not to recreate"
else
  fail "live network refusal printed the wrong hints (rc=$selfhost_rc): $selfhost_output"
fi
net_setup "$ROUTED_NET" net-routed lan
run_selfhost up
if [[ $selfhost_rc -ne 0 ]] && has "$NETWORK_HINT" && has "$DISCONNECT_HINT" && ! has "$RECREATE_HINT"; then
  pass "self-host up's post-up network refusal prints the disconnect hint, not the recreate hint"
else
  fail "up's network refusal printed the wrong hints (rc=$selfhost_rc): $selfhost_output"
fi

# 2. A rendered network violation (host networking). The stub renders services only, so a
# network_mode=host service is the rendered network path T2 can reach; the macvlan and
# gateway-mode renders are covered by test-selfhost-port-posture.sh.
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
export STUB_RENDER_EXTRA=',"nats":{"network_mode":"host"}'
run_selfhost up
unset STUB_RENDER_EXTRA
if [[ $selfhost_rc -ne 0 ]] && no_compose_up && has 'posture: nats network_mode=host refused' \
  && has "$NETWORK_HINT" && has "$DISCONNECT_HINT" \
  && ! has "$TOO_OLD_HINT" && ! has 'to honour !override' && ! has "$RECREATE_HINT"; then
  pass "rendered network_mode=host refusal prints the network hint and not the Compose-version hint"
else
  fail "rendered network refusal printed the wrong hints (rc=$selfhost_rc): $selfhost_output"
fi

# A port violation keeps its own hints and gains no network hint (both modes).
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
export STUB_RENDER_EXTRA="$POSTGRES_PUBLISHED"
run_selfhost up
unset STUB_RENDER_EXTRA
if [[ $selfhost_rc -ne 0 ]] && has "$TOO_OLD_HINT" && ! has "$NETWORK_HINT" && ! has 'docker network disconnect'; then
  pass "rendered port refusal prints the Compose-version hint and no network hint"
else
  fail "rendered port refusal printed the wrong hints (rc=$selfhost_rc): $selfhost_output"
fi
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
STUB_INSPECT_JSON="$(live_inspect "$NONCOMPLIANT_PG")"
export STUB_INSPECT_JSON
run_selfhost health
if [[ $selfhost_rc -ne 0 ]] && has "$RECREATE_HINT" && has "$ORPHAN_HINT" \
  && ! has "$NETWORK_HINT" && ! has 'docker network disconnect' && ! has "$TOO_OLD_HINT"; then
  pass "live port refusal prints the recreate and orphan hints and no network hint"
else
  fail "live port refusal printed the wrong hints (rc=$selfhost_rc): $selfhost_output"
fi

# 3. Mixed: one container publishes postgres on 0.0.0.0 and sits on a routed network.
net_setup "$ROUTED_NET" net-routed lan
STUB_INSPECT_JSON="$(live_inspect "$NONCOMPLIANT_PG" net-routed lan)"
export STUB_INSPECT_JSON
run_selfhost health
if [[ $selfhost_rc -ne 0 ]] \
  && has 'posture: postgres 5432/tcp host_ip=all not allowed' \
  && has "posture: postgres network=lan $REFUSED_TEXT" \
  && has "$RECREATE_HINT" && has "$ORPHAN_HINT" \
  && has "$NETWORK_HINT" && has "$DISCONNECT_HINT" \
  && ! has 'T2-SENTINEL'; then
  pass "a mixed live refusal prints both the recreate and the network hints"
else
  fail "a mixed live refusal printed the wrong hints (rc=$selfhost_rc): $selfhost_output"
fi

# ── status probes 127.0.0.1 ─────────────────────────────────────────────
# The control-plane and media-plane publish on 127.0.0.1 only, so `localhost` would try
# [::1] first. The curl stub answers any URL with success: only the request log can see it.
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
selfhost_output="$(ACTIVITY_HISTORY_CONTRACT_MODE=selfhost COMPOSE_OVERRIDE=docker-compose.production.yml \
  COMPOSE_PROFILES=services bash "$BEHAVIOR_CTL" status 2>&1)"
selfhost_rc=$?
if [[ $selfhost_rc -eq 0 ]] \
  && grep -qE 'curl .*127\.0\.0\.1:8080/health' "$SELFHOST_DOCKER_LOG" \
  && grep -qE 'curl .*127\.0\.0\.1:3000/health' "$SELFHOST_DOCKER_LOG" \
  && ! grep -qE 'localhost:(8080|3000)' "$SELFHOST_DOCKER_LOG"; then
  pass "status probes both services over 127.0.0.1, never localhost"
else
  fail "status probed localhost or skipped a probe (rc=$selfhost_rc): $(grep -E 'curl ' "$SELFHOST_DOCKER_LOG" | sort -u | head -4)"
fi

# ── Unknown verbs fail; help does not ───────────────────────────────────
# concord-selfhost health treats exit 0 from the controller as "the check ran and passed".
reset_selfhost_behavior false 1 '' ''
selfhost_output="$(bash "$BEHAVIOR_CTL" no-such-verb 2>&1)"
selfhost_rc=$?
if [[ $selfhost_rc -eq 2 ]] && grep -qi 'usage' <<<"$selfhost_output"; then
  pass "concord-ctl exits 2 on an unknown verb"
else
  fail "concord-ctl exited $selfhost_rc on an unknown verb, want 2: $selfhost_output"
fi
for helparg in help -h --help ""; do
  if [[ -n "$helparg" ]]; then
    selfhost_output="$(bash "$BEHAVIOR_CTL" "$helparg" 2>&1)"; selfhost_rc=$?
  else
    selfhost_output="$(bash "$BEHAVIOR_CTL" 2>&1)"; selfhost_rc=$?
  fi
  if [[ $selfhost_rc -eq 0 ]] && grep -qi 'usage' <<<"$selfhost_output"; then
    pass "concord-ctl exits 0 and prints usage for '${helparg:-<no argument>}'"
  else
    fail "concord-ctl exited $selfhost_rc for '${helparg:-<no argument>}', want 0: $selfhost_output"
  fi
done

# ── No HTTP probe names localhost ───────────────────────────────────────
# The overlay binds 127.0.0.1 only, so `localhost` resolves to [::1] first and a local user
# listening there could answer the probe and fake readiness. The curl stub answers ANY
# unknown URL with success, so only the request log can see a regression.
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
run_selfhost up
run_selfhost health
cat "$SELFHOST_DOCKER_LOG" >"$STUB/probe-log.copy"
write_selfhost_contract true
reset_selfhost_behavior true 1 '' 'cp-true\n'
run_selfhost activity-history-activate --confirm-drained
cat "$SELFHOST_DOCKER_LOG" >>"$STUB/probe-log.copy"
run_freshstart
cat "$SELFHOST_DOCKER_LOG" >>"$STUB/probe-log.copy"
write_selfhost_contract false
reset_selfhost_behavior false 1 '' ''
ACTIVITY_HISTORY_CONTRACT_MODE=selfhost COMPOSE_OVERRIDE=docker-compose.production.yml \
  COMPOSE_PROFILES=services bash "$BEHAVIOR_CTL" status >/dev/null 2>&1
cat "$SELFHOST_DOCKER_LOG" >>"$STUB/probe-log.copy"
if grep -qE 'curl .*127\.0\.0\.1:8080/readyz' "$STUB/probe-log.copy" \
  && grep -qE 'curl .*127\.0\.0\.1:3000/readyz' "$STUB/probe-log.copy" \
  && grep -qE 'curl .*127\.0\.0\.1:8080/health' "$STUB/probe-log.copy" \
  && grep -qE 'curl .*127\.0\.0\.1:3000/health' "$STUB/probe-log.copy" \
  && ! grep -qE 'localhost:(8080|3000)' "$STUB/probe-log.copy"; then
  pass "no HTTP probe requests localhost (up, health, activate, freshstart and status probe 127.0.0.1)"
else
  fail "a probe requested localhost or a loopback probe is missing: $(grep -E 'curl ' "$STUB/probe-log.copy" | sort -u | head -8)"
fi

precommit="$ROOT/.pre-commit-config.yaml"
if grep -qF 'entry: bash scripts/tests/run-selfhost-deploy-tests.sh' "$precommit" \
  && grep -qF 'scripts/tests/test-concord-selfhost.sh' "$ROOT/scripts/tests/run-selfhost-deploy-tests.sh" \
  && grep -qE 'files:.*concord-selfhost' "$precommit"; then
  pass "self-host lifecycle regression is wired into pre-commit triggers"
else
  fail "pre-commit does not run concord-selfhost lifecycle regressions"
fi

# ── BYO shared-model and stale-container integration (#1899) ────────────
# The fourth file is the exact synthetic spec candidate above. These tests
# establish dispatch/scope; the real overlay model and teardown are Task 5 gates.
write_byo_storage_block() {
  cat >>"$BEHAVIOR_ROOT/.env" <<'ENV'
SELFHOST_STORAGE_MODE=byo-s3
STORAGE_BACKEND=minio
STORAGE_ENDPOINT=objects.example.invalid
STORAGE_REGION=
STORAGE_ACCESS_KEY=synthetic-access
STORAGE_SECRET_KEY=synthetic-secret
STORAGE_USE_SSL=true
STORAGE_BUCKET=concord-media
ENV
}

byo_state() { # running|stopped|absent; unrelated running MinIO always exists
  python3 - "$SELFHOST_MINIO_STATE" "$1" <<'PYSTATE'
import json, sys
containers=[{"id":"f"*64,"project":"unrelated","service":"minio","running":True}]
if sys.argv[2] != "absent":
    containers.insert(0,{"id":"a"*64,"project":"concord-selfhost","service":"minio","running":sys.argv[2] == "running"})
# State has no publications; a running inactive service can pass the port guard.
with open(sys.argv[1],"w") as out:
    json.dump({"containers":containers,"inactive_volume":"concord-selfhost_minio_data"},out)
PYSTATE
}

reset_byo_case() {
  write_selfhost_contract false
  write_byo_storage_block
  reset_selfhost_behavior false 1 '' ''
  export STUB_BYO_MATRIX=1
  # Match docker-compose.byo-s3.yml: the lingering bundled MinIO service is
  # declared in the rendered model with its inactive profile.
  export STUB_RENDER_EXTRA=',"minio":{"profiles":["selfhost-bundled-minio-inactive"]}'
  unset STUB_MINIO_DISCOVERY_FAIL STUB_MINIO_MALFORMED_IDS STUB_MINIO_INSPECT_FAIL \
    STUB_MINIO_MALFORMED_META STUB_MINIO_STOP_FAIL STUB_MINIO_STILL_RUNNING \
    STUB_MINIO_UNRELATED_CANDIDATE STUB_MINIO_APPEAR_AFTER_UP \
    STUB_MINIO_APPEAR_AFTER_LIFECYCLE STUB_RENDER_FAIL STUB_LOG_MODEL_FAIL
  : >"$SELFHOST_DOCKER_JSON_LOG"
  : >"$SELFHOST_STUB_REJECT_LOG"
  byo_state "${1:-stopped}"
}

byo_invoke() { # cli|compose|restart, then argv
  local surface="$1"; shift
  # Poison every caller-controlled selector/client/legacy/root/image/rollout
  # field together. Docker records names only, so diagnostics never echo values.
  selfhost_output="$(
    exec 2>&1
    export SELFHOST_STORAGE_MODE=bundled STORAGE_BACKEND=s3 \
      STORAGE_ENDPOINT=wrong.example.invalid STORAGE_REGION=caller-region \
      STORAGE_ACCESS_KEY=caller-access STORAGE_SECRET_KEY=caller-secret \
      STORAGE_USE_SSL=false STORAGE_BUCKET=caller-bucket \
      MINIO_ENDPOINT=wrong.example.invalid MINIO_ACCESS_KEY=caller-access \
      MINIO_SECRET_KEY=caller-secret MINIO_USE_SSL=false MINIO_BUCKET=caller-bucket \
      MINIO_ROOT_USER=caller-root MINIO_ROOT_PASSWORD=caller-password \
      COMPOSE_FILE=/synthetic/unapproved.yml COMPOSE_PROFILES='*' \
      COMPOSE_PROJECT_NAME=unrelated COMPOSE_ENV_FILES=/synthetic/unapproved.env \
      COMPOSE_DISABLE_ENV_FILE=1 COMPOSE_OVERRIDE=/synthetic/unapproved.yml \
      INSTANCE_TYPE=saas ACTIVITY_HISTORY_CLUSTER_ENABLED=true \
      CONTROL_PLANE_REPLICA_COUNT=9 ACTIVITY_HISTORY_OPERATOR_NAME=caller-operator \
      ACTIVITY_HISTORY_PRIVACY_POLICY_URL=https://caller.invalid/privacy \
      ATTACHMENT_WRITE_BACKEND=r2 \
      CONTROL_PLANE_IMAGE=registry.invalid/caller@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
      SELFHOST_STORAGE_TRUSTED_IMAGE=registry.invalid/caller@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
    case "$surface" in
      cli) bash "$BEHAVIOR_TARGET" "$@" ;;
      compose) bash -c 'source "$1"; shift; compose "$@"' _ "$BEHAVIOR_TARGET" "$@" ;;
      restart) bash -c 'source "$1"; guarded_ctl restart' _ "$BEHAVIOR_TARGET" ;;
    esac
  )"
  selfhost_rc=$?
}

byo_model_matches() { # expected command token, optional trusted image
  [[ ! -s "$SELFHOST_STUB_REJECT_LOG" ]] || return 1
  python3 - "$SELFHOST_DOCKER_JSON_LOG" "$BEHAVIOR_ROOT" "$1" "${2:-}" <<'PYMODEL'
import json, sys
path,root,wanted,trusted=sys.argv[1:]
try: records=[json.loads(line) for line in open(path)]
except (ValueError,OSError): sys.exit(1)
files=[root+"/"+f for f in ["docker-compose.yml","docker-compose.production.yml","docker-compose.selfhost.yml","docker-compose.byo-s3.yml"]]
seen=False; models=0
for r in records:
    a=r["argv"]
    if r["inherited"] or (r["image"] and r["image"] != trusted): sys.exit(1)
    if not a or a[0] != "compose": continue
    opts={}; actual_files=[]; profiles=[]; i=1
    while i < len(a) and a[i].startswith("-"):
        if a[i] not in ["--env-file","--project-directory","--project-name","--profile","-f"] or i+1 >= len(a): sys.exit(1)
        key,value=a[i:i+2]
        if key == "-f": actual_files.append(value)
        elif key == "--profile": profiles.append(value)
        else:
            if key in opts: sys.exit(1)
            opts[key]=value
        i+=2
    tail=a[i:]
    if actual_files == ["-"]:
        if profiles or opts != {"--project-directory":root,"--env-file":"/dev/null"} or tail != ["config","--format","json"]: sys.exit(1)
        continue
    if actual_files != files or opts != {"--env-file":root+"/.env","--project-directory":root,"--project-name":"concord-selfhost"} or r["cwd"] != root: sys.exit(1)
    if profiles[:1] != ["services"]: sys.exit(1)
    all_profile_read = False
    if tail and tail[0] == "config":
        if profiles == ["services","*"]:
            if tail != ["config","--format","json"]: sys.exit(1)
            all_profile_read = True
        elif profiles == ["services"]:
            if tail not in (["config"],["config","--format","json"]): sys.exit(1)
        else: sys.exit(1)
    elif tail and tail[0] == "logs":
        target=False; skip=False
        for value in tail[1:]:
            if skip: skip=False; continue
            if value in ["--tail","-n","--since","--until","--index"]: skip=True
            elif value == "--no-follow" or value.startswith("-"): continue
            else: target=True
        expected_profiles=["services","*"] if target else ["services"]
        if profiles != expected_profiles: sys.exit(1)
        all_profile_read = target
    elif profiles != ["services"]: sys.exit(1)
    models+=1
    expected_command="config" if wanted == "logs-model" else wanted
    if tail and tail[0] == expected_command and (wanted != "logs-model" or all_profile_read): seen=True
sys.exit(0 if seen and models else 1)
PYMODEL
}

byo_config_profiles_ordered() {
  python3 - "$SELFHOST_DOCKER_JSON_LOG" <<'PYORDER'
import json, sys
profiles=[]
for line in open(sys.argv[1]):
    a=json.loads(line)["argv"]
    if not a or a[0] != "compose" or a[-3:] != ["config","--format","json"]: continue
    values=[]; i=1
    while i+1 < len(a):
        if a[i] == "--profile": values.append(a[i+1]); i += 2
        else: i += 1
    profiles.append(values)
try:
    base=profiles.index(["services"])
    expanded=profiles.index(["services","*"])
except ValueError:
    sys.exit(1)
sys.exit(0 if base < expanded else 1)
PYORDER
}

byo_no_mutation() {
  python3 - "$SELFHOST_DOCKER_JSON_LOG" <<'PYNOMUTATION'
import json, sys
for line in open(sys.argv[1]):
    a=json.loads(line)["argv"]
    if not a: continue
    if a[0] in ["stop","rm","volume"]: sys.exit(1)
    if a[0] != "compose": continue
    i=1
    while i < len(a) and a[i] in ["-f","--env-file","--project-directory","--project-name","--profile"]: i+=2
    if i < len(a) and a[i] in ["up","build","start","create","restart","stop","down","run","pull"]: sys.exit(1)
PYNOMUTATION
}

byo_refused() { # fixed expected diagnostic, no fixture rejection/shell error/value
  [[ $selfhost_rc -ne 0 && ! -s "$SELFHOST_STUB_REJECT_LOG" ]] \
    && grep -qF "$1" <<<"$selfhost_output" \
    && ! grep -qE 'command not found|unbound variable|Traceback|synthetic-secret|caller-secret|T2-SENTINEL' <<<"$selfhost_output"
}

byo_stale_labels_verified() {
  python3 - "$SELFHOST_DOCKER_JSON_LOG" "${1:-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}" <<'PYLABELS'
import json,sys
records=[json.loads(x)["argv"] for x in open(sys.argv[1])]
filters=["label=com.docker.compose.project=concord-selfhost","label=com.docker.compose.service=minio"]
seen=False
for a in records:
    if not a or a[0] != "ps": continue
    actual=[a[i+1] for i,x in enumerate(a[:-1]) if x == "--filter"]
    # Preserve exact label/ID authority for both the historical filtered list
    # and complete-candidate discovery; lifecycle state comes from inspect.
    if actual in (filters, filters+["status=running"]) and "--quiet" in a and "--no-trunc" in a: seen=True
inspected=any(a and a[0] == "inspect" and "--type" in a and "--format" in a and a[-1] == sys.argv[2] for a in records)
sys.exit(0 if seen and inspected else 1)
PYLABELS
}

byo_recovery_matches() { # stop|down: stop IDs -> authoritative stopped inspect -> model operation
  python3 - "$SELFHOST_DOCKER_JSON_LOG" "$SELFHOST_MINIO_STATE" "$1" <<'PYRECOVERY'
import json, sys
records=[json.loads(x)["argv"] for x in open(sys.argv[1])]
state=json.load(open(sys.argv[2])); wanted=sys.argv[3]; own="a"*64; unrelated="f"*64
stops=[i for i,a in enumerate(records) if a and a[0] == "stop"]
if len(stops) != 1 or records[stops[0]] != ["stop",own]: sys.exit(1)
verifies=[i for i,a in enumerate(records) if a and a[0] == "inspect" and "--type" in a and a[-1] == own and i > stops[0]]
model_ops=[]
for i,a in enumerate(records):
    if not a or a[0] != "compose": continue
    j=1
    while j < len(a) and a[j] in ["-f","--env-file","--project-directory","--project-name","--profile"]: j+=2
    if j < len(a) and a[j] == wanted: model_ops.append(i)
if len(model_ops) != 1 or not verifies or not stops[0] < verifies[0] < model_ops[0]: sys.exit(1)
if any(a and (a[0] in ["rm","volume"] or "--remove-orphans" in a or "-v" in a or "--volumes" in a or "selfhost-bundled-minio-inactive" in a or unrelated in a) for a in records): sys.exit(1)
cs=state["containers"]
if len(cs) != 2 or cs[0]["id"] != own or cs[0]["running"] or cs[1]["id"] != unrelated or not cs[1]["running"] or state["inactive_volume"] != "concord-selfhost_minio_data": sys.exit(1)
PYRECOVERY
}

run_managed_command_snapshot() {
  export SELFHOST_DOCKER_JSON_LOG="$STUB/managed-docker.jsonl"
  # Managed execution retains its two-file model and never loads the storage
  # resolver, root environment or self-host overlay.
  reset_selfhost_behavior false 1 '' ''
  : >"$SELFHOST_DOCKER_JSON_LOG"
  selfhost_output="$(ACTIVITY_HISTORY_CONTRACT_MODE=managed \
    COMPOSE_OVERRIDE=docker-compose.production.yml COMPOSE_PROFILES=services \
    bash "$BEHAVIOR_CTL" status 2>&1)"; selfhost_rc=$?
  if [[ $selfhost_rc -eq 0 ]] && python3 - "$SELFHOST_DOCKER_JSON_LOG" "$BEHAVIOR_ROOT" <<'PYMANAGED'
import json,sys
rs=[json.loads(x) for x in open(sys.argv[1])]
actual=[r["argv"] for r in rs if r["argv"] and r["argv"][0] == "compose"]
root=sys.argv[2]
# Managed DEPLOY_DIR historically includes infrastructure/deploy/../..;
# retain its exact path bytes while services includes the legacy media plane.
prefix=root+"/infrastructure/deploy/../.."
expected=["compose","-f",prefix+"/docker-compose.yml","-f",prefix+"/docker-compose.production.yml",
          "--profile","services","ps"]
sys.exit(0 if actual == [expected] else 1)
PYMANAGED
  then pass "managed status keeps two Compose files and the services profile"
  else fail "managed status changed the two-file single-media Compose argv"; fi

  unset SELFHOST_DOCKER_JSON_LOG
}

byo_health_live_port_checked() {
  python3 - "$SELFHOST_DOCKER_JSON_LOG" <<'PYHEALTHPORT'
import json, sys
records=[json.loads(line)["argv"] for line in open(sys.argv[1])]
sys.exit(0 if ["inspect","cp1","pg1","a"*64] in records
         and ["network","inspect","net-plain"] in records else 1)
PYHEALTHPORT
  [[ $? -eq 0 ]] \
    && grep -qxF 'selfhost-posture: live port bindings match the self-host allowlist' <<<"$selfhost_output" \
    && grep -qF 'published ports match the self-host posture' <<<"$selfhost_output"
}

run_byo_integration_matrix() {
  export SELFHOST_DOCKER_JSON_LOG="$STUB/byo-docker.jsonl" \
    SELFHOST_MINIO_STATE="$STUB/byo-minio-state.json"
  # CONTROL_PLANE_IMAGE is checked separately because only validated rollback
  # checkpoint images may be reintroduced by the trusted internal selector.
  export SELFHOST_SCRUB_NAMES='SELFHOST_STORAGE_MODE STORAGE_BACKEND STORAGE_ENDPOINT STORAGE_REGION STORAGE_ACCESS_KEY STORAGE_SECRET_KEY STORAGE_USE_SSL STORAGE_BUCKET MINIO_ENDPOINT MINIO_ACCESS_KEY MINIO_SECRET_KEY MINIO_USE_SSL MINIO_BUCKET MINIO_ROOT_USER MINIO_ROOT_PASSWORD COMPOSE_FILE COMPOSE_PROFILES COMPOSE_PROJECT_NAME COMPOSE_ENV_FILES COMPOSE_DISABLE_ENV_FILE COMPOSE_OVERRIDE INSTANCE_TYPE ACTIVITY_HISTORY_CLUSTER_ENABLED CONTROL_PLANE_REPLICA_COUNT ACTIVITY_HISTORY_OPERATOR_NAME ACTIVITY_HISTORY_PRIVACY_POLICY_URL ATTACHMENT_WRITE_BACKEND SELFHOST_STORAGE_TRUSTED_IMAGE'

  # Every public wrapper dispatch and every underlying Compose verb shares the
  # root/env/project/file/profile selection, including guarded up's internal calls.
  for verb in up stop down status logs health; do
    reset_byo_case stopped
    byo_invoke cli "$verb"
    expected="$verb"; [[ "$verb" != status ]] || expected='ps'
    [[ "$verb" != health ]] || expected=config
    [[ "$verb" != up ]] || expected=up
    if [[ $selfhost_rc -eq 0 && ! -s "$SELFHOST_STUB_REJECT_LOG" ]] && byo_model_matches "$expected"; then
      pass "BYO wrapper $verb uses exact shared model with all caller controls scrubbed"
    else fail "BYO wrapper $verb missed exact shared model or failed positive control (rc=$selfhost_rc)"; fi
    if [[ "$verb" == health ]]; then
      if [[ $selfhost_rc -eq 0 ]] && byo_no_mutation && byo_health_live_port_checked \
        && grep -qF 'control-plane healthy (HTTP 200)' <<<"$selfhost_output" \
        && ! grep -qE 'synthetic-secret|caller-secret|T2-SENTINEL' <<<"$selfhost_output"; then
        pass "BYO healthy stopped-MinIO control completes actual live port and network inspection"
      else fail "BYO healthy stopped-MinIO live inspection positive control failed"; fi
    fi
  done
  for verb in up build stop down restart logs config pull ps; do
    reset_byo_case stopped
    byo_invoke compose "$verb"
    if [[ $selfhost_rc -eq 0 && ! -s "$SELFHOST_STUB_REJECT_LOG" ]] && byo_model_matches "$verb"; then
      pass "BYO wrapper compose helper $verb uses exact shared model"
    else fail "BYO wrapper compose helper $verb missed exact shared model (rc=$selfhost_rc)"; fi
  done
  # start/create are real Compose lifecycle verbs: a profile alone must not
  # admit explicit inactive targets or bypass the stale-state lifecycle guard.
  for verb in start create; do
    reset_byo_case stopped
    byo_invoke compose "$verb" control-plane
    if [[ $selfhost_rc -eq 0 ]] && byo_model_matches "$verb" \
      && python3 - "$SELFHOST_DOCKER_JSON_LOG" "$verb" <<'PYLIFECYCLE_ARGS'
import json, sys
commands=[]
for line in open(sys.argv[1]):
    a=json.loads(line)["argv"]
    if a[:1] != ["compose"]: continue
    i=1
    while i < len(a) and a[i] in ["-f","--env-file","--project-directory","--project-name","--profile"]: i+=2
    if i < len(a) and a[i] == sys.argv[2]: commands.append(a[i:])
assert commands == [[sys.argv[2],"control-plane"]]
PYLIFECYCLE_ARGS
    then
      pass "BYO compose $verb forwards valid control-plane target with exact four-file model"
    else fail "BYO compose $verb failed valid target/shared-model positive control (rc=$selfhost_rc)"; fi

    reset_byo_case stopped
    byo_invoke compose "$verb" minio
    if byo_refused 'selfhost-storage: explicit MinIO target refused' && byo_no_mutation; then
      pass "BYO compose $verb refuses explicit MinIO target before mutation"
    else fail "BYO compose $verb missing explicit-MinIO guard: target reached mutation or lacked fixed refusal"; fi

    reset_byo_case running
    byo_invoke compose "$verb" control-plane
    if byo_refused 'selfhost-storage: running bundled MinIO refused' && byo_no_mutation \
      && byo_stale_labels_verified; then
      pass "BYO compose $verb refuses label-verified unpublished running MinIO before mutation"
    else fail "BYO compose $verb missing pre-dispatch stale-MinIO guard"; fi

    for fault in STUB_MINIO_DISCOVERY_FAIL STUB_MINIO_MALFORMED_IDS STUB_MINIO_INSPECT_FAIL STUB_MINIO_MALFORMED_META; do
      reset_byo_case running
      export "$fault=1"
      byo_invoke compose "$verb" control-plane
      if byo_refused 'selfhost-storage: MinIO discovery unavailable' && byo_no_mutation \
        && ! grep -qF 'STUB-MINIO-DISCOVERY-ERROR' <<<"$selfhost_output"; then
        pass "BYO compose $verb fails closed on $fault before mutation without raw errors"
      else fail "BYO compose $verb missing fail-closed discovery guard ($fault)"; fi
    done

    reset_byo_case stopped
    export STUB_MINIO_APPEAR_AFTER_LIFECYCLE="$verb"
    byo_invoke compose "$verb" control-plane
    if byo_refused 'selfhost-storage: running bundled MinIO refused' && byo_model_matches "$verb" \
      && byo_stale_labels_verified && ! grep -qx 'Done.' <<<"$selfhost_output"; then
      pass "BYO compose $verb rechecks running MinIO after dispatch and withholds success"
    else fail "BYO compose $verb missing post-dispatch stale-MinIO guard"; fi
  done

  for verb in up stop down status logs health; do
    reset_byo_case stopped
    printf 'SELFHOST_STORAGE_MODE=bundled\n' >>"$BEHAVIOR_ROOT/.env"
    byo_invoke cli "$verb"
    if byo_refused 'selfhost-storage: storage environment refused' && byo_no_mutation; then
      pass "BYO wrapper $verb refuses duplicate/conflicting persisted mode before mutation"
    else fail "BYO wrapper $verb bypassed persisted mode reader or lacked fixed refusal"; fi
  done
  for verb in up build restart; do
    reset_byo_case stopped
    byo_invoke compose "$verb" minio
    if byo_refused 'MinIO' && byo_no_mutation; then
      pass "BYO explicit MinIO $verb target refuses before mutation"
    else fail "BYO explicit MinIO $verb target reached mutation or lacked fixed refusal"; fi
  done

  for verb in up health status activity-history-activate activity-history-rollback restart; do
    reset_byo_case running
    case "$verb" in
      activity-history-activate)
        write_selfhost_contract true; write_byo_storage_block
        export STUB_RENDER_GATE=true STUB_ROOT_GATE=true STUB_PS_1='cp-true\n'
        byo_invoke cli "$verb" --confirm-drained ;;
      activity-history-rollback)
        export STUB_PS_1='cp-old\n'
        byo_invoke cli "$verb" --confirm-drained --old-image "$old_image" ;;
      restart) byo_invoke restart ;;
      *) byo_invoke cli "$verb" ;;
    esac
    if byo_refused 'selfhost-storage: running bundled MinIO refused' && byo_no_mutation \
      && byo_stale_labels_verified; then
      pass "BYO $verb refuses unpublished label-verified same-project running MinIO before mutation"
    else fail "BYO $verb ignored stale running MinIO or failed through another branch"; fi
  done
  # Ordinary health keeps diagnosing clean live bindings after stale-state refusal.
  reset_byo_case running
  byo_invoke cli health
  if byo_refused 'selfhost-storage: running bundled MinIO refused' && byo_no_mutation \
    && byo_stale_labels_verified \
    && grep -qxF 'selfhost-storage: running bundled MinIO refused' <<<"$selfhost_output" \
    && grep -qF 'control-plane healthy (HTTP 200)' <<<"$selfhost_output" \
    && grep -q 'curl .*127\.0\.0\.1:8080/api/v1/client/config' "$SELFHOST_DOCKER_LOG"; then
    pass "BYO running-MinIO health keeps exact refusal, HTTP diagnosis and private read-only controls"
  else fail "BYO running-MinIO health fixture lost refusal, HTTP diagnosis or read-only controls"; fi
  if byo_health_live_port_checked; then
    pass "BYO running-MinIO health completes clean live port and network inspection despite refusal"
  else fail "BYO running-MinIO health skips clean live port and network inspection after refusal"; fi

  # Read-only logs for declared services remain available while stale MinIO
  # runs, including MinIO when its bundled profile is inactive.
  reset_byo_case running
  byo_invoke cli logs --no-follow --tail 12 control-plane
  if [[ $selfhost_rc -eq 0 ]] && byo_model_matches logs && byo_no_mutation \
    && grep -q -- 'compose .* logs --tail 12 control-plane' "$SELFHOST_DOCKER_LOG" \
    && ! grep -q -- 'compose .* logs -f' "$SELFHOST_DOCKER_LOG"; then
    pass "BYO selected-service logs remain read-only and available while same-project stale MinIO runs"
  else fail "BYO stale MinIO blocked selected-service read-only logs or changed follow mode"; fi

  reset_byo_case running
  byo_invoke cli logs minio
  if [[ $selfhost_rc -eq 0 ]] && byo_model_matches logs && byo_no_mutation; then
    pass "BYO logs remains available while same-project stale MinIO runs"
  else fail "BYO stale MinIO made read-only logs unavailable"; fi

  reset_byo_case running
  byo_invoke cli logs not-a-service
  if byo_refused 'selfhost-storage: unknown log service' && byo_no_mutation \
    && byo_model_matches logs-model && ! grep -q -- 'compose .* logs' "$SELFHOST_DOCKER_LOG"; then
    pass "BYO logs rejects an undeclared service before dispatch"
  else fail "BYO logs dispatched an undeclared target or missed model validation"; fi

  reset_byo_case running
  export STUB_LOG_MODEL_FAIL=1
  byo_invoke cli logs control-plane
  if [[ $selfhost_rc -eq 1 ]] && byo_refused 'selfhost-storage: Compose command failed' && byo_no_mutation \
    && ! grep -qF 'unknown log service' <<<"$selfhost_output" \
    && ! grep -qF 'synthetic-secret' <<<"$selfhost_output" \
    && byo_model_matches logs-model && byo_config_profiles_ordered \
    && ! grep -q -- 'compose .* logs' "$SELFHOST_DOCKER_JSON_LOG"; then
    pass "BYO logs preserves wildcard-model failure after canonical resolution without leaking or dispatching"
  else fail "BYO logs confused model failure with service refusal or exposed diagnostic"; fi

  # Accepted controls ensure the stale refusals are attached to running state
  # and authoritative labels, rather than mere service/container existence.
  for state in stopped absent; do
    reset_byo_case "$state"
    export STUB_MINIO_UNRELATED_CANDIDATE=1
    byo_invoke cli up
    if [[ $selfhost_rc -eq 0 ]] && byo_model_matches up \
      && byo_stale_labels_verified ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff; then
      pass "BYO startup accepts $state same-project MinIO and inspected unrelated running MinIO"
    else fail "BYO startup refused $state/unrelated stale-state control"; fi
  done
  reset_byo_case stopped
  export STUB_MINIO_APPEAR_AFTER_UP=1
  byo_invoke cli up
  if byo_refused 'selfhost-storage: running bundled MinIO refused' && byo_model_matches up \
    && ! grep -qx 'Done.' <<<"$selfhost_output"; then
    pass "BYO up rechecks running MinIO after start and withholds success"
  else fail "BYO up omitted the post-start stale-MinIO check"; fi

  for verb in stop down; do
    reset_byo_case running
    byo_invoke cli "$verb"
    if [[ $selfhost_rc -eq 0 && ! -s "$SELFHOST_STUB_REJECT_LOG" ]] \
      && byo_model_matches "$verb" && byo_recovery_matches "$verb"; then
      pass "BYO $verb stops only matching IDs, verifies stopped, preserves inactive container/volume"
    else fail "BYO $verb failed stop-only recovery order/scope/preservation"; fi
    reset_byo_case stopped
    byo_invoke cli "$verb"
    if [[ $selfhost_rc -eq 0 ]] && byo_model_matches "$verb" \
      && ! grep -qE '^docker .* stop [a-f0-9]{64}' "$SELFHOST_DOCKER_LOG"; then
      pass "BYO $verb preserves already-stopped MinIO without issuing Docker stop"
    else fail "BYO $verb tried to stop already-stopped MinIO or omitted model operation"; fi
    for fault in STUB_MINIO_DISCOVERY_FAIL STUB_MINIO_MALFORMED_IDS STUB_MINIO_INSPECT_FAIL STUB_MINIO_MALFORMED_META STUB_MINIO_STOP_FAIL STUB_MINIO_STILL_RUNNING; do
      reset_byo_case running
      export "$fault=1"
      byo_invoke cli "$verb"
      if byo_refused 'selfhost-storage: MinIO' \
        && ! grep -qE "compose .* (stop|down)( |$)" "$SELFHOST_DOCKER_LOG"; then
        pass "BYO $verb refuses $fault before follow-on model operation"
      else fail "BYO $verb continued after $fault or lacked fixed recovery refusal"; fi
    done
  done
  for fault in STUB_MINIO_DISCOVERY_FAIL STUB_MINIO_MALFORMED_IDS STUB_MINIO_INSPECT_FAIL STUB_MINIO_MALFORMED_META; do
    for verb in up health status; do
      reset_byo_case running
      export "$fault=1"
      byo_invoke cli "$verb"
      if byo_refused 'selfhost-storage: MinIO' && byo_no_mutation; then
        pass "BYO $verb refuses failed/malformed stale discovery ($fault)"
      else fail "BYO $verb accepted $fault or failed through another branch"; fi
    done
  done

  # Both guarded Activity History operations must reach the same model and
  # retain their independent existing receipt/image/readiness behavior above.
  reset_byo_case stopped
  write_selfhost_contract true; write_byo_storage_block
  export STUB_RENDER_GATE=true STUB_ROOT_GATE=true STUB_PS_1='cp-true\n'
  byo_invoke cli activity-history-activate --confirm-drained
  if [[ $selfhost_rc -eq 0 ]] && byo_model_matches run && [[ -f "$RECEIPT" ]]; then
    pass "BYO Activity History activation uses shared model and records successful receipt"
  else fail "BYO Activity History activation failed shared-model positive control"; fi
  reset_byo_case stopped
  export STUB_PS_1='cp-old\n'
  byo_invoke cli activity-history-rollback --confirm-drained --old-image "$old_image"
  if [[ $selfhost_rc -eq 0 ]] && byo_model_matches run "$old_image"; then
    pass "BYO Activity History rollback keeps trusted image within shared model"
  else fail "BYO Activity History rollback lost shared model/trusted checkpoint image"; fi
  unset STUB_BYO_MATRIX SELFHOST_DOCKER_JSON_LOG SELFHOST_SCRUB_NAMES
}

# ── Installer-owned interpolation authority (#4197915622) ─────────────
# These 17 fields come from the installer's durable writer. Keep this list
# independent of the producer's scrub list so an omitted field remains visible.
SELFHOST_INSTALLER_KEYS='ENVIRONMENT DOMAIN PUBLIC_IP POSTGRES_PASSWORD REDIS_PASSWORD JWT_SECRET MFA_ENCRYPTION_KEY TURN_SECRET TURN_REALM TRUSTED_PROXY_CIDRS SMTP_HOST SMTP_PORT SMTP_USERNAME SMTP_PASSWORD SMTP_FROM FEEDBACK_REPO FEEDBACK_PAT'
INSTALLER_AUTHORITY_CONTROLS='SELFHOST_STORAGE_MODE STORAGE_BACKEND STORAGE_ENDPOINT STORAGE_REGION STORAGE_ACCESS_KEY STORAGE_SECRET_KEY STORAGE_USE_SSL STORAGE_BUCKET MINIO_ENDPOINT MINIO_ACCESS_KEY MINIO_SECRET_KEY MINIO_USE_SSL MINIO_BUCKET MINIO_ROOT_USER MINIO_ROOT_PASSWORD COMPOSE_FILE COMPOSE_PROFILES COMPOSE_PROJECT_NAME COMPOSE_ENV_FILES COMPOSE_DISABLE_ENV_FILE COMPOSE_OVERRIDE INSTANCE_TYPE ACTIVITY_HISTORY_CLUSTER_ENABLED CONTROL_PLANE_REPLICA_COUNT ACTIVITY_HISTORY_OPERATOR_NAME ACTIVITY_HISTORY_PRIVACY_POLICY_URL ATTACHMENT_WRITE_BACKEND COTURN_CERTS_DIR SELFHOST_STORAGE_TRUSTED_IMAGE RESEND_API_KEY'

installer_authority_invoke() { # wrapper|controller|checkpoint, ambient|clean|smtp-alias
  local surface="$1" inheritance="$2"
  selfhost_output="$(
    exec 2>&1
    export SELFHOST_AUTHORITY_PROBE=1 SELFHOST_AUTHORITY_ROOT="$BEHAVIOR_ROOT" \
      SELFHOST_INSTALLER_KEYS SELFHOST_SCRUB_NAMES \
      DOCKER_HOST="unix://$BEHAVIOR_ROOT/fixture-docker.sock" \
      DOCKER_CONFIG="$BEHAVIOR_ROOT/fixture-docker-config" \
      COMPOSE_PARALLEL_LIMIT=3 ACTIVITY_HISTORY_HEALTH_TIMEOUT=1
    # A second invocation has exactly the same durable file, with no ambient
    # writer values. Do not let the runner's environment decide either case.
    for key in $SELFHOST_INSTALLER_KEYS $INSTALLER_AUTHORITY_CONTROLS CONTROL_PLANE_IMAGE; do
      unset "$key"
    done
    if [[ "$inheritance" == ambient ]]; then
      for key in $SELFHOST_INSTALLER_KEYS; do export "$key=fixture-ambient-$key"; done
      export ENVIRONMENT=development DOMAIN=ambient.example.invalid PUBLIC_IP=198.51.100.42 \
        TURN_REALM=ambient.example.invalid TRUSTED_PROXY_CIDRS=10.24.0.0/16 \
        SMTP_HOST=smtp.ambient.example.invalid SMTP_PORT=587 \
        SMTP_FROM=fixture@ambient.example.invalid FEEDBACK_REPO=selfhost/ambient
      export SELFHOST_STORAGE_MODE=bundled STORAGE_BACKEND=s3 \
        STORAGE_ENDPOINT=caller.example.invalid STORAGE_REGION=caller-region \
        STORAGE_ACCESS_KEY=fixture-caller-access STORAGE_SECRET_KEY=fixture-caller-secret \
        STORAGE_USE_SSL=false STORAGE_BUCKET=caller-bucket \
        MINIO_ENDPOINT=caller.example.invalid MINIO_ACCESS_KEY=fixture-caller-access \
        MINIO_SECRET_KEY=fixture-caller-secret MINIO_USE_SSL=false MINIO_BUCKET=caller-bucket \
        MINIO_ROOT_USER=caller-root MINIO_ROOT_PASSWORD=fixture-caller-password \
        COMPOSE_FILE=/fixture/unapproved.yml COMPOSE_PROFILES='*' \
        COMPOSE_PROJECT_NAME=unrelated COMPOSE_ENV_FILES=/fixture/unapproved.env \
        COMPOSE_DISABLE_ENV_FILE=1 COMPOSE_OVERRIDE=/fixture/unapproved.yml \
        INSTANCE_TYPE=saas ACTIVITY_HISTORY_CLUSTER_ENABLED=true \
        CONTROL_PLANE_REPLICA_COUNT=9 ACTIVITY_HISTORY_OPERATOR_NAME=caller-operator \
        ACTIVITY_HISTORY_PRIVACY_POLICY_URL=https://caller.invalid/privacy \
        ATTACHMENT_WRITE_BACKEND=r2 COTURN_CERTS_DIR=/fixture/caller-certs \
        CONTROL_PLANE_IMAGE=registry.invalid/caller@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
        SELFHOST_STORAGE_TRUSTED_IMAGE=registry.invalid/caller@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
    elif [[ "$inheritance" == smtp-alias ]]; then
      export RESEND_API_KEY=fixture-ambient-smtp-alias
    fi
    case "$surface" in
      wrapper) bash "$BEHAVIOR_TARGET" status ;;
      controller)
        ACTIVITY_HISTORY_CONTRACT_MODE=selfhost COMPOSE_OVERRIDE=docker-compose.production.yml \
          COMPOSE_PROFILES=services bash "$BEHAVIOR_CTL" rebuild ;;
      checkpoint)
        bash "$BEHAVIOR_TARGET" activity-history-rollback --confirm-drained --old-image "$old_image" ;;
    esac
  )"
  selfhost_rc=$?
}

installer_authority_matches() { # mode, expected command, optional trusted image
  python3 - "$SELFHOST_DOCKER_JSON_LOG" "$BEHAVIOR_ROOT" "$1" "$2" "${3:-}" \
    "$SELFHOST_INSTALLER_KEYS" <<'PYAUTHORITY'
import json, sys
path, root, mode, wanted, trusted, key_text = sys.argv[1:]
keys = set(key_text.split())
files = [root + "/" + name for name in
    ("docker-compose.yml", "docker-compose.production.yml", "docker-compose.selfhost.yml")]
if mode == "byo-s3": files.append(root + "/docker-compose.byo-s3.yml")
seen_config, seen_command = False, False
for record in map(json.loads, open(path)):
    if record["argv"][:1] != ["compose"]: continue
    assert not record["inherited"], "ambient installer/authority key reached Compose: " + ",".join(record["inherited"])
    assert record["controls_preserved"], "unrelated Docker/runtime control was erased"
    authority = record["authority"]
    command = authority["command"]
    if authority["files"] == ["-"]:
        assert authority["options"] == {"--project-directory": root, "--env-file": "/dev/null"}
        assert authority["profiles"] == [] and command == ["config", "--format", "json"]
        continue
    assert authority["files"] == files and record["cwd"] == root
    assert authority["options"] == {"--env-file": root + "/.env",
        "--project-directory": root, "--project-name": "concord-selfhost"}
    assert authority["profiles"] == ["services"]
    selected = record["durable_selected"]
    assert set(selected) == keys and all(selected.values()), "durable value lost: " + ",".join(k for k in keys if not selected.get(k))
    assert record["image"] in ("", trusted), "untrusted image override survived"
    if command == ["config", "--format", "json"]: seen_config = True
    if command and command[0] == wanted:
        assert record["image"] == trusted, "validated checkpoint image was not reapplied"
        seen_command = True
assert seen_config and seen_command, "config/actual command consumer was not reached"
PYAUTHORITY
}

installer_authority_env_metadata() {
  python3 - "$BEHAVIOR_ROOT/.env" <<'PYENVMETADATA'
import json, os, stat, sys
metadata = os.lstat(sys.argv[1])
if not stat.S_ISREG(metadata.st_mode): sys.exit(1)
# Reads may update atime. Preserve content, type/permissions, owner, identity,
# and write/change timestamps; rollout receipts are separate allowed effects.
fields = ("st_mode", "st_uid", "st_gid", "st_dev", "st_ino", "st_size",
          "st_mtime_ns", "st_ctime_ns")
print(json.dumps({field: getattr(metadata, field) for field in fields}, sort_keys=True))
PYENVMETADATA
}

run_installer_authority_matrix() { # bundled|byo-s3
  local mode="$1" surface inheritance expected trusted key value snapshot
  export SELFHOST_DOCKER_JSON_LOG="$STUB/installer-authority.jsonl" \
    SELFHOST_MINIO_STATE="$STUB/installer-authority-minio.json"
  export SELFHOST_SCRUB_NAMES="$SELFHOST_INSTALLER_KEYS $INSTALLER_AUTHORITY_CONTROLS"
  for surface in wrapper controller checkpoint; do
    # Ambient then clean uses the same persisted configuration. Recreate only
    # owned fixture state and rollout receipts between the two command paths.
    write_selfhost_contract false
    if [[ "$mode" == byo-s3 ]]; then write_byo_storage_block
    else printf 'SELFHOST_STORAGE_MODE=bundled\n' >>"$BEHAVIOR_ROOT/.env"; fi
    for key in $SELFHOST_INSTALLER_KEYS; do
      value="fixture-durable-$key"
      case "$key" in
        ENVIRONMENT) value=production ;;
        DOMAIN|TURN_REALM) value=durable.example.invalid ;;
        PUBLIC_IP) value=203.0.113.42 ;;
        TRUSTED_PROXY_CIDRS) value=10.23.0.0/16 ;;
        SMTP_HOST) value=smtp.durable.example.invalid ;;
        SMTP_PORT) value=2525 ;;
        SMTP_FROM) value=fixture@durable.example.invalid ;;
        FEEDBACK_REPO) value=selfhost/durable ;;
      esac
      printf '%s=%s\n' "$key" "$value" >>"$BEHAVIOR_ROOT/.env"
    done
    if ! chmod 600 "$BEHAVIOR_ROOT/.env"; then
      fail "installer authority: $mode $surface environment permission setup failed"
      continue
    fi
    for inheritance in ambient clean smtp-alias; do
      # Alias precedence is exercised at the direct wrapper and real controller
      # consumers; checkpoint/image controls retain their existing paired cells.
      [[ "$surface" != checkpoint || "$inheritance" != smtp-alias ]] || continue
      reset_selfhost_behavior false 1 '' ''
      unset STUB_RENDER_EXTRA STUB_BYO_MATRIX
      if [[ "$mode" == byo-s3 ]]; then
        export STUB_BYO_MATRIX=1
        export STUB_RENDER_EXTRA=',"minio":{"profiles":["selfhost-bundled-minio-inactive"]}'
      fi
      byo_state stopped
      [[ "$surface" != checkpoint ]] || export STUB_PS_1='cp-old\n'
      : >"$SELFHOST_DOCKER_JSON_LOG"
      : >"$SELFHOST_STUB_REJECT_LOG"
      snapshot="$STUB/installer-authority-$mode-$surface-$inheritance"
      if ! cp "$BEHAVIOR_ROOT/.env" "$snapshot.env" \
        || ! installer_authority_env_metadata >"$snapshot.before-metadata"; then
        fail "installer authority: $mode $surface $inheritance environment snapshot setup failed"
        continue
      fi
      installer_authority_invoke "$surface" "$inheritance"
      if cmp -s "$snapshot.env" "$BEHAVIOR_ROOT/.env" \
        && installer_authority_env_metadata >"$snapshot.after-metadata" \
        && cmp -s "$snapshot.before-metadata" "$snapshot.after-metadata"; then
        pass "installer authority: $mode $surface $inheritance preserves durable env bytes and metadata"
      else
        fail "installer authority: $mode $surface $inheritance changed durable env bytes/type/permissions/owner/identity/timestamps"
      fi
      expected=up; trusted=''
      [[ "$surface" != wrapper ]] || expected='ps'
      [[ "$surface" != checkpoint ]] || trusted="$old_image"
      if [[ $selfhost_rc -eq 0 && ! -s "$SELFHOST_STUB_REJECT_LOG" ]] \
        && installer_authority_matches "$mode" "$expected" "$trusted"; then
        pass "installer authority: $mode $surface $inheritance consumes all 17 durable values and preserves runtime controls"
      else
        fail "installer authority: $mode $surface $inheritance lost durable selection/command/runtime/image control (rc=$selfhost_rc)"
      fi
    done
  done
  unset STUB_BYO_MATRIX STUB_RENDER_EXTRA SELFHOST_DOCKER_JSON_LOG SELFHOST_SCRUB_NAMES
}

run_installer_readonly_scrub_matrix() {
  local key mutability snapshot scrub_output scrub_rc
  export SELFHOST_DOCKER_JSON_LOG="$STUB/readonly-scrub.jsonl" \
    SELFHOST_SCRUB_NAMES="$SELFHOST_INSTALLER_KEYS $INSTALLER_AUTHORITY_CONTROLS"
  reset_selfhost_behavior false 1 '' ''
  for key in DOMAIN SMTP_PASSWORD; do
    for mutability in readonly mutable; do
      : >"$SELFHOST_DOCKER_JSON_LOG"
      : >"$SELFHOST_STUB_REJECT_LOG"
      snapshot="$STUB/readonly-scrub-$key-$mutability"
      if ! cp "$BEHAVIOR_ROOT/.env" "$snapshot.env" \
        || ! installer_authority_env_metadata >"$snapshot.before-metadata"; then
        fail "sourced scrub: $key $mutability environment snapshot setup failed"
        continue
      fi
      # Source the actual helper. Its real identity caller must obey scrub's
      # result before even the read-only Compose render. A mutable sibling uses
      # the same root/parser/command seam, preventing an unrelated refusal pass.
      scrub_output="$(bash -c '
        source "$1" || exit 1
        for name in $SELFHOST_SCRUB_NAMES CONTROL_PLANE_IMAGE; do unset "$name"; done
        export "$2=fixture-sourced-scrub-value"
        if [[ "$3" == readonly ]]; then readonly "$2"; fi
        selfhost_storage_project_identity "$4"
      ' _ "$STORAGE_HELPER" "$key" "$mutability" "$BEHAVIOR_ROOT" 2>&1)"
      scrub_rc=$?
      if cmp -s "$snapshot.env" "$BEHAVIOR_ROOT/.env" \
        && installer_authority_env_metadata >"$snapshot.after-metadata" \
        && cmp -s "$snapshot.before-metadata" "$snapshot.after-metadata"; then
        pass "sourced scrub: $key $mutability preserves durable env bytes and metadata"
      else
        fail "sourced scrub: $key $mutability changed durable env bytes or metadata"
      fi
      if [[ ! -s "$SELFHOST_STUB_REJECT_LOG" ]] \
        && { [[ "$mutability" == mutable && $scrub_rc -eq 0 ]] \
          || { [[ "$mutability" == readonly && $scrub_rc -ne 0 ]] \
            && grep -qF 'selfhost-storage: project identity unavailable' <<<"$scrub_output"; }; } \
        && python3 - "$SELFHOST_DOCKER_JSON_LOG" "$BEHAVIOR_ROOT" "$mutability" <<'PYREADONLY'
import json, sys
records = [json.loads(line) for line in open(sys.argv[1])]
compose = [record for record in records if record["argv"][:1] == ["compose"]]
if sys.argv[3] == "readonly":
    assert not compose, "failed scrub reached Compose"
else:
    assert [record["argv"] for record in compose] == [["compose",
        "--project-directory", sys.argv[2], "--env-file", "/dev/null",
        "-f", "-", "config", "--format", "json"]]
    assert not compose[0]["inherited"], "mutable key was not scrubbed"
PYREADONLY
      then
        pass "sourced scrub: $key $mutability obeys scrub status at the actual Compose caller"
      else
        fail "sourced scrub: $key $mutability lost scrub status/refusal/control before Compose (rc=$scrub_rc)"
      fi
    done
  done
  unset SELFHOST_DOCKER_JSON_LOG SELFHOST_SCRUB_NAMES
}

# Pin the consumer independently of the environment scrub and finite adapter.
# The public replacement overlay resets this field; these are the actual base
# and repository-production files selected by the checkout wrapper/controller.
if python3 - "$ROOT" <<'PYSMTPALIAS'
from pathlib import Path
import sys
expression = '${RESEND_API_KEY:-${SMTP_PASSWORD:-}}'
expected = ['SMTP_PASSWORD: ' + expression, 'SMTP_PASSWORD: "' + expression + '"']
for name in ('docker-compose.yml', 'docker-compose.production.yml'):
    lines = [line.strip() for line in (Path(sys.argv[1]) / name).read_text().splitlines()
             if line.lstrip().startswith('SMTP_PASSWORD:')]
    assert len(lines) == 1 and lines[0] in expected, 'selected SMTP alias consumer changed: ' + name
PYSMTPALIAS
then pass "selected base/repository-production SMTP consumer pins exact legacy alias precedence"
else fail "SMTP alias seam differs from the selected Compose consumer"; fi

run_managed_command_snapshot
run_installer_authority_matrix bundled
run_installer_readonly_scrub_matrix

# Failures from unrecognized Docker calls cannot satisfy negative assertions.
if [[ -s "$SELFHOST_STUB_REJECT_LOG" ]]; then
  fail "baseline fixture rejected an unrecognized Docker command"
fi

# Task 4 future integration preparation: --base-only deliberately excludes this
# matrix and is never reported as a GREEN result for the BYO feature.
if (( BASE_ONLY )); then
  printf '%s\n' 'SKIP: Task 4 BYO integration matrix (--base-only; feature behavior unexecuted)'
else
  run_byo_integration_matrix
  run_installer_authority_matrix byo-s3
fi

# ── Summary ───────────────────────────────────────────────────────────
if [[ $fails -eq 0 ]]; then
  if (( BASE_ONLY )); then
    echo "BASELINE PASS: Task 4 BYO integration behavior unexecuted"
  else
    echo "ALL PASS"
  fi
  exit 0
else
  echo "$fails FAILED"
  exit 1
fi
