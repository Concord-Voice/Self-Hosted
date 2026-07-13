#!/usr/bin/env bash
# Concord service management helper
# Usage: ./concord-ctl.sh <command>
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="${SCRIPT_DIR}/../.."

# Capture the caller-requested contract before sourcing any host file. A
# managed deploy.env.local must never be able to switch the controller into the
# self-host single-writer mode (or switch an explicit wrapper request out of
# it). The wrapper also attests the only allowed Compose override/profile.
ACTIVITY_HISTORY_REQUESTED_CONTRACT_MODE="${ACTIVITY_HISTORY_CONTRACT_MODE-}"
ACTIVITY_HISTORY_REQUESTED_COMPOSE_OVERRIDE="${COMPOSE_OVERRIDE-}"
ACTIVITY_HISTORY_REQUESTED_COMPOSE_PROFILES="${COMPOSE_PROFILES-}"

if [[ "$ACTIVITY_HISTORY_REQUESTED_CONTRACT_MODE" == selfhost \
   && -e "${SCRIPT_DIR}/deploy.env.local" ]]; then
  echo "activity-history: self-host contract refuses deploy.env.local" >&2
  exit 1
fi

# Load runtime config. deploy.env (committed defaults like COMPOSE_OVERRIDE)
# is read first, then deploy.env.local (machine-specific overrides like
# VM_IP, DEPLOY_BRANCH) layers on top.
if [[ -f "${SCRIPT_DIR}/deploy.env" ]]; then
  source "${SCRIPT_DIR}/deploy.env"
fi
if [[ -f "${SCRIPT_DIR}/deploy.env.local" ]]; then
  source "${SCRIPT_DIR}/deploy.env.local"
fi

# Keep the values sourced from the canonical application-env writer separate
# from Compose's root .env. Activity History rollout commands require both
# writers and the effective rendered Compose config to agree.
ACTIVITY_HISTORY_DEPLOY_WRITER_GATE="${ACTIVITY_HISTORY_CLUSTER_ENABLED-}"
ACTIVITY_HISTORY_DEPLOY_WRITER_REPLICAS="${CONTROL_PLANE_REPLICA_COUNT-}"
ACTIVITY_HISTORY_DEPLOY_WRITER_OPERATOR_NAME="${ACTIVITY_HISTORY_OPERATOR_NAME-}"
ACTIVITY_HISTORY_DEPLOY_WRITER_PRIVACY_POLICY_URL="${ACTIVITY_HISTORY_PRIVACY_POLICY_URL-}"

ACTIVITY_HISTORY_CONTRACT_MODE="${ACTIVITY_HISTORY_REQUESTED_CONTRACT_MODE:-managed}"
if [[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]]; then
  # Caller environment wins over Compose .env interpolation. Remove every
  # self-host-owned value (and any ad-hoc image override) before rendering or
  # running containers; Compose then loads the installer-owned root .env.
  unset INSTANCE_TYPE ACTIVITY_HISTORY_CLUSTER_ENABLED \
    CONTROL_PLANE_REPLICA_COUNT ACTIVITY_HISTORY_OPERATOR_NAME \
    ACTIVITY_HISTORY_PRIVACY_POLICY_URL CONTROL_PLANE_IMAGE \
    COMPOSE_ENV_FILES COMPOSE_DISABLE_ENV_FILE
  COMPOSE_OVERRIDE=docker-compose.production.yml
  COMPOSE_PROFILES=services
fi

DEPLOY_REPO="${DEPLOY_REPO:-https://github.com/Concord-Voice/Self-Hosted.git}"
COMPOSE_OVERRIDE="${COMPOSE_OVERRIDE:-docker-compose.staging.yml}"
COMPOSE_ENV_FILE_ARG=""
[[ "$ACTIVITY_HISTORY_CONTRACT_MODE" == selfhost ]] \
  && COMPOSE_ENV_FILE_ARG="--env-file ${DEPLOY_DIR}/.env"
COMPOSE_CMD="docker compose ${COMPOSE_ENV_FILE_ARG} -f ${DEPLOY_DIR}/docker-compose.yml -f ${DEPLOY_DIR}/${COMPOSE_OVERRIDE} --profile ${COMPOSE_PROFILES:-services}"
ACTIVITY_HISTORY_IMAGE_SELECTOR="${SCRIPT_DIR}/.env.activity-history-image.local"
ACTIVITY_HISTORY_IMAGE_SELECTOR_LOADED=0
ACTIVITY_HISTORY_IMAGE_SELECTOR_SELECTED=0
ACTIVITY_HISTORY_ROLLBACK_CHECKPOINT_IMAGE=""
ACTIVITY_HISTORY_ROLLBACK_ADMIN_COMPLETE=""
ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_REQUESTED=""
ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_STATE=""

# Rollback pins an immutable old control-plane image and the completed admin
# phase in a dedicated local checkpoint. Parse it as data rather than sourcing
# it. restart/status keep consuming the pin; an explicit ordinary rebuild or
# freshstart removes it before building the current source again.
load_activity_history_image_selector() {
  local line value
  local image_count=0 admin_count=0 requested_count=0 state_count=0
  [[ -f "$ACTIVITY_HISTORY_IMAGE_SELECTOR" ]] || return 0
  ACTIVITY_HISTORY_ROLLBACK_CHECKPOINT_IMAGE=""
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ -z "$line" ]] && continue
    value="${line#*=}"
    case "$line" in
      CONTROL_PLANE_IMAGE=*)
        image_count=$((image_count + 1))
        ACTIVITY_HISTORY_ROLLBACK_CHECKPOINT_IMAGE="$value"
        ;;
      ACTIVITY_HISTORY_ROLLBACK_ADMIN_COMPLETE=*)
        admin_count=$((admin_count + 1))
        ACTIVITY_HISTORY_ROLLBACK_ADMIN_COMPLETE="$value"
        ;;
      ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_REQUESTED=*)
        requested_count=$((requested_count + 1))
        ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_REQUESTED="$value"
        ;;
      ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_STATE=*)
        state_count=$((state_count + 1))
        ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_STATE="$value"
        ;;
      *)
        echo "activity-history: rollback checkpoint is malformed; refusing to continue" >&2
        return 1
        ;;
    esac
  done <"$ACTIVITY_HISTORY_IMAGE_SELECTOR"
  if [[ $image_count -ne 1 || $admin_count -ne 1 \
     || $requested_count -ne 1 || $state_count -ne 1 ]]; then
    echo "activity-history: rollback checkpoint is malformed; refusing to continue" >&2
    return 1
  fi
  if [[ "$ACTIVITY_HISTORY_ROLLBACK_CHECKPOINT_IMAGE" == *[[:space:]]* \
     || ! "$ACTIVITY_HISTORY_ROLLBACK_CHECKPOINT_IMAGE" =~ ^(sha256:[[:xdigit:]]{64}|.+@sha256:[[:xdigit:]]{64})$ ]]; then
    echo "activity-history: rollback checkpoint image is not immutable; refusing to continue" >&2
    return 1
  fi
  if [[ "$ACTIVITY_HISTORY_ROLLBACK_ADMIN_COMPLETE" != true \
     || ! "$ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_REQUESTED" =~ ^(true|false)$ ]]; then
    echo "activity-history: rollback checkpoint completion state is invalid; refusing to continue" >&2
    return 1
  fi
  if [[ "$ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_REQUESTED" == true ]]; then
    [[ "$ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_STATE" =~ ^(attempting|complete|failed)$ ]] || {
      echo "activity-history: rollback checkpoint completion state is invalid; refusing to continue" >&2
      return 1
    }
  elif [[ "$ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_STATE" != not_requested ]]; then
    echo "activity-history: rollback checkpoint completion state is invalid; refusing to continue" >&2
    return 1
  fi
  ACTIVITY_HISTORY_IMAGE_SELECTOR_LOADED=1
  if [[ "$ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_STATE" =~ ^(complete|not_requested)$ ]]; then
    CONTROL_PLANE_IMAGE="$ACTIVITY_HISTORY_ROLLBACK_CHECKPOINT_IMAGE"
    ACTIVITY_HISTORY_IMAGE_SELECTOR_SELECTED=1
    export CONTROL_PLANE_IMAGE
  fi
}

clear_activity_history_image_selector() {
  if [[ -f "$ACTIVITY_HISTORY_IMAGE_SELECTOR" ]]; then
    rm -f "$ACTIVITY_HISTORY_IMAGE_SELECTOR"
    echo "Activity History rollback image pin cleared; rebuilding the current source."
  fi
  if [[ $ACTIVITY_HISTORY_IMAGE_SELECTOR_SELECTED -eq 1 ]]; then
    unset CONTROL_PLANE_IMAGE
  fi
  ACTIVITY_HISTORY_IMAGE_SELECTOR_LOADED=0
  ACTIVITY_HISTORY_IMAGE_SELECTOR_SELECTED=0
  ACTIVITY_HISTORY_ROLLBACK_CHECKPOINT_IMAGE=""
  ACTIVITY_HISTORY_ROLLBACK_ADMIN_COMPLETE=""
  ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_REQUESTED=""
  ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_STATE=""
}

load_activity_history_image_selector

# Shared release sync logic
# shellcheck source=sync-releases.sh
source "${SCRIPT_DIR}/sync-releases.sh"

dump_health_diagnostics() {
  echo "" >&2
  echo "=== docker compose ps (safe fields) ===" >&2
  if ! (cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --all --format 'table {{.Service}}\t{{.State}}\t{{.Health}}\t{{.ExitCode}}') >&2; then
    echo "  ⚠ failed to collect docker compose ps" >&2
  fi

  echo "" >&2
  echo "=== docker compose logs ===" >&2
  echo "  omitted from CI diagnostics; runtime logs may contain host-side secrets" >&2
  echo "  use 'concord-ctl.sh logs <service>' on the host for manual incident diagnostics" >&2
}

activity_history_read_root_value() {
  local key="$1" file="${DEPLOY_DIR}/.env" count value
  [[ -f "$file" ]] || return 1
  count="$(grep -cE "^${key}=" "$file" 2>/dev/null || true)"
  [[ "$count" == 1 ]] || return 1
  value="$(grep -E "^${key}=" "$file" | cut -d= -f2-)"
  if [[ ${#value} -ge 2 && "$value" == \'*\' ]]; then
    value="${value:1:${#value}-2}"
  fi
  printf '%s' "$value"
}

activity_history_render_contract() {
  local rendered parsed
  # Resolve the root .env contract independently of deploy.env.local. The
  # latter is sourced for concord-ctl settings, and inherited shell values
  # otherwise override Compose's root dotenv values during rendering.
  if ! rendered="$(cd "$DEPLOY_DIR" && env \
      -u ACTIVITY_HISTORY_CLUSTER_ENABLED \
      -u CONTROL_PLANE_REPLICA_COUNT \
      -u ACTIVITY_HISTORY_OPERATOR_NAME \
      -u ACTIVITY_HISTORY_PRIVACY_POLICY_URL \
      $COMPOSE_CMD config --format json 2>/dev/null)"; then
    echo "activity-history: failed to render Compose configuration" >&2
    return 1
  fi
  if ! parsed="$(python3 -c '
import json, sys
try:
    service = json.load(sys.stdin)["services"]["control-plane"]
    environment = service.get("environment", {})
    if isinstance(environment, list):
        environment = dict(item.split("=", 1) for item in environment)
    gate = str(environment["ACTIVITY_HISTORY_CLUSTER_ENABLED"])
    replicas = str(environment["CONTROL_PLANE_REPLICA_COUNT"])
    operator_name = str(environment["ACTIVITY_HISTORY_OPERATOR_NAME"])
    privacy_policy_url = str(environment["ACTIVITY_HISTORY_PRIVACY_POLICY_URL"])
    instance_type = str(environment.get("INSTANCE_TYPE", ""))
    image = str(service["image"])
    values = (gate, replicas, image, instance_type, operator_name, privacy_policy_url)
    if any(any(ord(char) < 32 or ord(char) == 127 for char in value) for value in values):
        raise ValueError("invalid control character")
    print("\x1f".join(values))
except (KeyError, TypeError, ValueError, json.JSONDecodeError):
    sys.exit(1)
' <<<"$rendered" 2>/dev/null)"; then
    echo "activity-history: rendered Compose contract is missing or invalid" >&2
    return 1
  fi
  IFS=$'\x1f' read -r ACTIVITY_HISTORY_RENDERED_GATE \
    ACTIVITY_HISTORY_RENDERED_REPLICAS ACTIVITY_HISTORY_RENDERED_IMAGE \
    ACTIVITY_HISTORY_RENDERED_INSTANCE_TYPE \
    ACTIVITY_HISTORY_RENDERED_OPERATOR_NAME \
    ACTIVITY_HISTORY_RENDERED_PRIVACY_POLICY_URL <<<"$parsed"
  if [[ ! "$ACTIVITY_HISTORY_RENDERED_GATE" =~ ^(true|false)$ \
     || ! "$ACTIVITY_HISTORY_RENDERED_REPLICAS" =~ ^[1-9][0-9]*$ \
     || -z "$ACTIVITY_HISTORY_RENDERED_IMAGE" \
     || "$ACTIVITY_HISTORY_RENDERED_IMAGE" == *[[:space:]]* ]]; then
    echo "activity-history: rendered Compose contract has invalid values" >&2
    return 1
  fi
}

activity_history_load_contract() {
  local root_gate root_replicas instance_type
  local mode="${ACTIVITY_HISTORY_CONTRACT_MODE:-managed}"
  local deploy_file="${SCRIPT_DIR}/deploy.env.local"
  if ! root_gate="$(activity_history_read_root_value ACTIVITY_HISTORY_CLUSTER_ENABLED)" \
     || ! root_replicas="$(activity_history_read_root_value CONTROL_PLANE_REPLICA_COUNT)" \
     || [[ "$(grep -cE '^ACTIVITY_HISTORY_OPERATOR_NAME=' "${DEPLOY_DIR}/.env" 2>/dev/null || true)" != 1 ]] \
     || [[ "$(grep -cE '^ACTIVITY_HISTORY_PRIVACY_POLICY_URL=' "${DEPLOY_DIR}/.env" 2>/dev/null || true)" != 1 ]]; then
    echo "activity-history: canonical root .env contract is missing or duplicated" >&2
    return 1
  fi
  if [[ ! "$root_gate" =~ ^(true|false)$ || ! "$root_replicas" =~ ^[1-9][0-9]*$ ]]; then
    echo "activity-history: canonical rollout values are invalid" >&2
    return 1
  fi

  case "$mode" in
    managed)
      if [[ ! -f "$deploy_file" \
         || "$(grep -cE '^ACTIVITY_HISTORY_CLUSTER_ENABLED=' "$deploy_file" 2>/dev/null || true)" != 1 \
         || "$(grep -cE '^CONTROL_PLANE_REPLICA_COUNT=' "$deploy_file" 2>/dev/null || true)" != 1 \
         || "$(grep -cE '^ACTIVITY_HISTORY_OPERATOR_NAME=' "$deploy_file" 2>/dev/null || true)" != 1 \
         || "$(grep -cE '^ACTIVITY_HISTORY_PRIVACY_POLICY_URL=' "$deploy_file" 2>/dev/null || true)" != 1 ]]; then
        echo "activity-history: canonical deploy.env.local contract is missing or duplicated" >&2
        return 1
      fi
      if [[ ! "$ACTIVITY_HISTORY_DEPLOY_WRITER_GATE" =~ ^(true|false)$ \
         || ! "$ACTIVITY_HISTORY_DEPLOY_WRITER_REPLICAS" =~ ^[1-9][0-9]*$ ]]; then
        echo "activity-history: canonical rollout values are invalid" >&2
        return 1
      fi
      if [[ "$root_gate" != "$ACTIVITY_HISTORY_DEPLOY_WRITER_GATE" \
         || "$root_replicas" != "$ACTIVITY_HISTORY_DEPLOY_WRITER_REPLICAS" ]]; then
        echo "activity-history: canonical rollout writers disagree; rerun provision-secrets" >&2
        return 1
      fi
      ;;
    selfhost)
      if [[ "$ACTIVITY_HISTORY_REQUESTED_COMPOSE_OVERRIDE" != docker-compose.production.yml \
         || "$ACTIVITY_HISTORY_REQUESTED_COMPOSE_PROFILES" != services ]]; then
        echo "activity-history: self-host contract requires the production Compose override and services profile" >&2
        return 1
      fi
      if ! instance_type="$(activity_history_read_root_value INSTANCE_TYPE)" \
         || [[ "$instance_type" != self-hosted ]]; then
        echo "activity-history: self-host contract requires INSTANCE_TYPE=self-hosted in root .env" >&2
        return 1
      fi
      if [[ -e "$deploy_file" ]]; then
        echo "activity-history: self-host contract refuses deploy.env.local" >&2
        return 1
      fi
      ;;
    *)
      echo "activity-history: unknown rollout contract mode; refusing to continue" >&2
      return 1
      ;;
  esac

  activity_history_render_contract || return 1
  if [[ "$mode" == selfhost \
     && "$ACTIVITY_HISTORY_RENDERED_INSTANCE_TYPE" != self-hosted ]]; then
    echo "activity-history: rendered self-host instance type is invalid" >&2
    return 1
  fi
  if [[ "$root_gate" != "$ACTIVITY_HISTORY_RENDERED_GATE" \
     || "$root_replicas" != "$ACTIVITY_HISTORY_RENDERED_REPLICAS" ]]; then
    echo "activity-history: rendered Compose rollout contract disagrees with canonical writers" >&2
    return 1
  fi
  if [[ "$mode" == managed \
     && ( "$ACTIVITY_HISTORY_DEPLOY_WRITER_OPERATOR_NAME" != "$ACTIVITY_HISTORY_RENDERED_OPERATOR_NAME" \
       || "$ACTIVITY_HISTORY_DEPLOY_WRITER_PRIVACY_POLICY_URL" != "$ACTIVITY_HISTORY_RENDERED_PRIVACY_POLICY_URL" ) ]]; then
    echo "activity-history: canonical disclosure writers disagree; rerun provision-secrets" >&2
    return 1
  fi
}

ACTIVITY_HISTORY_DAEMON_IDS=()
activity_history_refresh_daemon_ids() {
  local output id
  ACTIVITY_HISTORY_DAEMON_IDS=()
  if ! output="$(docker ps \
      --filter label=com.docker.compose.service=control-plane \
      --format '{{.ID}}' 2>/dev/null)"; then
    echo "activity-history: failed to inspect daemon-wide control-plane state" >&2
    return 1
  fi
  while IFS= read -r id; do
    [[ -n "$id" ]] && ACTIVITY_HISTORY_DAEMON_IDS+=("$id")
  done <<<"$output"
  return 0
}

activity_history_container_gate_matches() {
  local container_id="$1" expected="$2" container_env gate_count
  [[ "$expected" =~ ^(true|false)$ ]] || return 1
  if ! container_env="$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' \
      "$container_id" 2>/dev/null)"; then
    echo "activity-history: failed to inspect the running control-plane contract" >&2
    return 1
  fi
  gate_count="$(grep -cE '^ACTIVITY_HISTORY_CLUSTER_ENABLED=' <<<"$container_env" || true)"
  [[ "$gate_count" == 1 ]] \
    && grep -qxF "ACTIVITY_HISTORY_CLUSTER_ENABLED=$expected" <<<"$container_env"
}

activity_history_container_image_matches() {
  local container_id="$1" expected="$2" actual
  if ! actual="$(docker inspect --format '{{.Config.Image}}' "$container_id" 2>/dev/null)"; then
    echo "activity-history: failed to inspect the running control-plane image" >&2
    return 1
  fi
  [[ -n "$expected" && "$actual" == "$expected" ]]
}

activity_history_routine_guard() {
  activity_history_load_contract || return 1
  [[ "$ACTIVITY_HISTORY_RENDERED_GATE" == false ]] && return 0
  if [[ "$ACTIVITY_HISTORY_RENDERED_REPLICAS" != 1 ]]; then
    echo "activity-history: gate=true requires exactly one configured control-plane replica" >&2
    return 1
  fi
  activity_history_refresh_daemon_ids || return 1
  if [[ ${#ACTIVITY_HISTORY_DAEMON_IDS[@]} -ne 1 ]] \
     || ! activity_history_container_gate_matches "${ACTIVITY_HISTORY_DAEMON_IDS[0]:-}" true; then
    echo "activity-history: routine deployment cannot perform first activation or repair ambiguous state" >&2
    echo "Run concord-ctl.sh activity-history-activate --confirm-drained after draining traffic." >&2
    return 1
  fi
}

activity_history_image_is_local() {
  local image="$1"
  if ! docker image inspect "$image" >/dev/null 2>&1; then
    echo "activity-history: required control-plane image is not present locally" >&2
    return 1
  fi
}

activity_history_run_admin() {
  local verb="$1"
  (cd "$DEPLOY_DIR" && $COMPOSE_CMD run --rm --no-deps --pull never \
    control-plane ./main activity-history "$verb" --confirm-drained)
}

activity_history_start_control_plane() {
  (cd "$DEPLOY_DIR" && $COMPOSE_CMD up -d --no-deps --no-build --pull never control-plane)
}

activity_history_stop_control_plane() {
  (cd "$DEPLOY_DIR" && $COMPOSE_CMD stop --timeout 45 control-plane)
}

activity_history_wait_control_plane() {
  local timeout="${ACTIVITY_HISTORY_HEALTH_TIMEOUT:-60}"
  local interval="${ACTIVITY_HISTORY_HEALTH_INTERVAL:-2}"
  local deadline now remaining probe_limit sleep_for
  if [[ ! "$timeout" =~ ^[1-9][0-9]*$ || ! "$interval" =~ ^[0-9]+$ ]]; then
    echo "activity-history: health timeout/interval must be integer seconds" >&2
    return 1
  fi
  deadline=$(( $(date +%s) + timeout ))
  while true; do
    now="$(date +%s)"
    remaining=$((deadline - now))
    (( remaining > 0 )) || break
    probe_limit="$remaining"
    if (( interval > 0 && interval < probe_limit )); then
      probe_limit="$interval"
    fi
    if curl --connect-timeout "$probe_limit" --max-time "$probe_limit" \
      -sf 'http://localhost:8080/health' >/dev/null 2>&1; then
      return 0
    fi
    (( interval == 0 )) && break
    now="$(date +%s)"
    remaining=$((deadline - now))
    (( remaining > 0 )) || break
    sleep_for="$interval"
    (( sleep_for > remaining )) && sleep_for="$remaining"
    (( sleep_for > 0 )) && sleep "$sleep_for"
  done
  echo "activity-history: control-plane health check timed out" >&2
  return 1
}

activity_history_write_rollback_checkpoint() {
  local image="$1" requested="$2" state="$3"
  local temp="${ACTIVITY_HISTORY_IMAGE_SELECTOR}.new.$$"
  if [[ ! "$requested" =~ ^(true|false)$ ]] \
    || { [[ "$requested" == true ]] && [[ ! "$state" =~ ^(attempting|complete|failed)$ ]]; } \
    || { [[ "$requested" == false ]] && [[ "$state" != not_requested ]]; }; then
    echo "activity-history: invalid internal rollback checkpoint transition" >&2
    return 1
  fi
  umask 077
  if ! {
       printf 'CONTROL_PLANE_IMAGE=%s\n' "$image"
       printf 'ACTIVITY_HISTORY_ROLLBACK_ADMIN_COMPLETE=true\n'
       printf 'ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_REQUESTED=%s\n' "$requested"
       printf 'ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_STATE=%s\n' "$state"
     } >"$temp" \
     || ! chmod 600 "$temp" \
     || ! mv "$temp" "$ACTIVITY_HISTORY_IMAGE_SELECTOR"; then
    rm -f "$temp"
    echo "activity-history: failed to persist rollback checkpoint state" >&2
    return 1
  fi
  ACTIVITY_HISTORY_IMAGE_SELECTOR_LOADED=1
  ACTIVITY_HISTORY_ROLLBACK_CHECKPOINT_IMAGE="$image"
  ACTIVITY_HISTORY_ROLLBACK_ADMIN_COMPLETE=true
  ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_REQUESTED="$requested"
  ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_STATE="$state"
}

activity_history_select_checkpoint_image() {
  if [[ $ACTIVITY_HISTORY_IMAGE_SELECTOR_LOADED -ne 1 \
     || ! "$ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_STATE" =~ ^(complete|not_requested)$ ]]; then
    echo "activity-history: rollback checkpoint is not resumable" >&2
    return 1
  fi
  CONTROL_PLANE_IMAGE="$ACTIVITY_HISTORY_ROLLBACK_CHECKPOINT_IMAGE"
  ACTIVITY_HISTORY_IMAGE_SELECTOR_SELECTED=1
  export CONTROL_PLANE_IMAGE
}

activity_history_finish_rollback_start() {
  if ! activity_history_start_control_plane; then
    activity_history_stop_control_plane >/dev/null 2>&1 || true
    echo "activity-history: rollback old-image start failed; checkpoint retained for exact retry" >&2
    return 1
  fi
  if ! activity_history_wait_control_plane; then
    activity_history_stop_control_plane >/dev/null 2>&1 || true
    echo "activity-history: rollback health failed; checkpoint retained for exact retry" >&2
    return 1
  fi
  if ! activity_history_refresh_daemon_ids \
     || [[ ${#ACTIVITY_HISTORY_DAEMON_IDS[@]} -ne 1 ]] \
     || ! activity_history_container_image_matches \
       "${ACTIVITY_HISTORY_DAEMON_IDS[0]:-}" "$ACTIVITY_HISTORY_ROLLBACK_CHECKPOINT_IMAGE" \
     || ! activity_history_container_gate_matches \
       "${ACTIVITY_HISTORY_DAEMON_IDS[0]:-}" false; then
    activity_history_stop_control_plane >/dev/null 2>&1 || true
    echo "activity-history: rollback post-start invariant failed; checkpoint retained for exact retry" >&2
    return 1
  fi
  echo "Activity History rollback completed with one healthy control-plane."
}

activity_history_activate() {
  if [[ $# -ne 1 || "$1" != --confirm-drained ]]; then
    echo "Usage: concord-ctl.sh activity-history-activate --confirm-drained" >&2
    return 2
  fi
  activity_history_load_contract || return 1
  if [[ "$ACTIVITY_HISTORY_RENDERED_GATE" != true \
     || "$ACTIVITY_HISTORY_RENDERED_REPLICAS" != 1 ]]; then
    echo "activity-history: activation requires rendered gate=true and replica count=1" >&2
    return 1
  fi
  activity_history_refresh_daemon_ids || return 1
  if [[ ${#ACTIVITY_HISTORY_DAEMON_IDS[@]} -ne 0 ]]; then
    echo "activity-history: activation requires zero running control-plane containers daemon-wide" >&2
    return 1
  fi
  activity_history_image_is_local "$ACTIVITY_HISTORY_RENDERED_IMAGE" || return 1
  echo "Activity History activation: zero daemon-wide control-planes confirmed; running non-serving preflight, CP-only start, health, and final count."
  activity_history_run_admin preflight || return 1
  if ! activity_history_start_control_plane; then
    activity_history_stop_control_plane >/dev/null 2>&1 || true
    echo "activity-history: control-plane start failed; cleanup attempted" >&2
    return 1
  fi
  if ! activity_history_wait_control_plane; then
    activity_history_stop_control_plane >/dev/null 2>&1 || true
    return 1
  fi
  if ! activity_history_refresh_daemon_ids \
     || [[ ${#ACTIVITY_HISTORY_DAEMON_IDS[@]} -ne 1 ]] \
     || ! activity_history_container_gate_matches "${ACTIVITY_HISTORY_DAEMON_IDS[0]:-}" true; then
    activity_history_stop_control_plane >/dev/null 2>&1 || true
    echo "activity-history: activation post-start invariant failed; control-plane stopped" >&2
    return 1
  fi
  echo "Activity History activation completed with one healthy control-plane."
}

activity_history_parse_rollback_args() {
  if [[ $# -ne 3 && $# -ne 4 ]]; then
    echo "Usage: concord-ctl.sh activity-history-rollback --confirm-drained --old-image <immutable-image-ref> [--downgrade-schema]" >&2
    return 2
  fi
  if [[ "$1" != --confirm-drained || "$2" != --old-image \
     || -z "$3" || "$3" == --* \
     || ( $# -eq 4 && "$4" != --downgrade-schema ) ]]; then
    echo "activity-history-rollback: flags must match the documented command exactly" >&2
    return 2
  fi
  ACTIVITY_HISTORY_ROLLBACK_OLD_IMAGE="$3"
  ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE=$(( $# == 4 ? 1 : 0 ))
}

activity_history_rollback() {
  local old_image requested=false
  activity_history_parse_rollback_args "$@" || return $?
  old_image="$ACTIVITY_HISTORY_ROLLBACK_OLD_IMAGE"
  [[ $ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE -eq 1 ]] && requested=true
  if [[ "$old_image" == *[[:space:]]* \
     || ! "$old_image" =~ ^(sha256:[[:xdigit:]]{64}|.+@sha256:[[:xdigit:]]{64})$ ]]; then
    echo "activity-history: --old-image must be an immutable sha256 digest reference" >&2
    return 2
  fi

  # A checkpoint exists only after disable-all completed. Exact matching
  # retries resume the old-image start and never repeat maintenance. An
  # attempting state is deliberately ambiguous across host loss: the exact
  # step may already have reached 84, so repeating it is unsafe, while an older
  # migrator may not recognize v85. Automatic recovery therefore refuses both
  # downgrade and old-image start.
  if [[ $ACTIVITY_HISTORY_IMAGE_SELECTOR_LOADED -eq 1 ]]; then
    if [[ "$old_image" != "$ACTIVITY_HISTORY_ROLLBACK_CHECKPOINT_IMAGE" ]]; then
      echo "activity-history: checkpoint does not match the requested old image; refusing before mutation" >&2
      return 1
    fi
    if [[ "$requested" != "$ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_REQUESTED" ]]; then
      echo "activity-history: checkpoint downgrade intent does not match the requested command; refusing before mutation" >&2
      return 1
    fi
    if [[ "$ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_STATE" == failed ]]; then
      echo "activity-history: checkpoint records an explicit downgrade failure; inspect recovery state or rebuild current source before retrying" >&2
      return 1
    fi
    if [[ "$ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE_STATE" == attempting ]]; then
      echo "activity-history: downgrade completion is ambiguous; automatic resume is refused" >&2
      echo "Keep the current image selected and inspect migration state; use a guarded current-source rebuild for recovery. Do not repeat exact downgrade automatically." >&2
      return 1
    fi
    activity_history_image_is_local "$old_image" || return 1
    activity_history_load_contract || return 1
    if [[ "$ACTIVITY_HISTORY_RENDERED_GATE" != false ]]; then
      echo "activity-history: rollback retry requires rendered gate=false" >&2
      return 1
    fi
    activity_history_refresh_daemon_ids || return 1
    if [[ ${#ACTIVITY_HISTORY_DAEMON_IDS[@]} -ne 0 ]]; then
      echo "activity-history: rollback retry requires zero running control-plane containers" >&2
      return 1
    fi
    echo "Activity History rollback checkpoint matches: admin work is complete; resuming pinned old-image start, health, and final count without repeating disable or downgrade."
    activity_history_finish_rollback_start
    return
  fi

  activity_history_image_is_local "$old_image" || return 1
  activity_history_load_contract || return 1
  if [[ "$ACTIVITY_HISTORY_RENDERED_GATE" != false ]]; then
    echo "activity-history: rollback requires rendered gate=false" >&2
    return 1
  fi
  # The currently rendered image contains the administrative subcommands used
  # while the service is drained. Refuse before stopping if it is unavailable.
  activity_history_image_is_local "$ACTIVITY_HISTORY_RENDERED_IMAGE" || return 1
  echo "Activity History rollback: stopping CP with a 45s drain, asserting zero daemon-wide, disabling durable state, journaling optional exact downgrade, selecting the pinned old image, and requiring one healthy CP."
  activity_history_stop_control_plane || return 1
  activity_history_refresh_daemon_ids || return 1
  if [[ ${#ACTIVITY_HISTORY_DAEMON_IDS[@]} -ne 0 ]]; then
    echo "activity-history: rollback requires zero running control-plane containers after stop" >&2
    return 1
  fi
  activity_history_run_admin disable-all || return 1
  if [[ $ACTIVITY_HISTORY_ROLLBACK_DOWNGRADE -eq 1 ]]; then
    activity_history_write_rollback_checkpoint \
      "$old_image" true attempting || return 1
    if ! activity_history_run_admin downgrade-schema; then
      if ! activity_history_write_rollback_checkpoint \
        "$old_image" true failed; then
        echo "activity-history: exact downgrade failed and failure-state journaling also failed; attempting checkpoint retained" >&2
      else
        echo "activity-history: exact downgrade failed; failed checkpoint retained and automatic resume is refused" >&2
      fi
      return 1
    fi
    activity_history_write_rollback_checkpoint \
      "$old_image" true complete || return 1
  else
    activity_history_write_rollback_checkpoint \
      "$old_image" false not_requested || return 1
  fi
  activity_history_select_checkpoint_image || return 1
  activity_history_finish_rollback_start
}

case "${1:-help}" in
  status)
    echo "=== Concord Service Status ==="
    # NOTE: Branch + commit info no longer surfaced — server has no .git/ post-#848.
    # See `gh api repos/Concord-Voice/Concord-Voice-Alpha/deployments` for deploy history.
    if [[ -f "${DEPLOY_DIR}/.deploy-meta" ]]; then
      echo "Last deploy:"
      cat "${DEPLOY_DIR}/.deploy-meta" | sed 's/^/  /'
    else
      echo "Last deploy: (no metadata file — first deploy not yet completed)"
    fi
    echo ""
    cd "$DEPLOY_DIR" && $COMPOSE_CMD ps
    echo ""
    echo "=== Health Checks ==="
    VM_IP="${VM_IP:-localhost}"
    for svc in "Control Plane:8080" "Media Plane:3000"; do
      name="${svc%%:*}"
      port="${svc##*:}"
      if curl -sf "http://localhost:${port}/health" > /dev/null 2>&1; then
        echo "  ✓ ${name} (http://${VM_IP}:${port})"
      else
        echo "  ✗ ${name} (port ${port})"
      fi
    done
    # coturn check — verify STUN port is listening
    if nc -z -w2 localhost 3478 2>/dev/null; then
      echo "  ✓ coturn STUN/TURN (${VM_IP}:3478)"
    else
      echo "  ✗ coturn STUN/TURN (port 3478)"
    fi
    # Release sync status
    echo ""
    echo "=== Release Sync ==="
    if [[ -f "${DEPLOY_DIR}/releases/.tag" ]]; then
      echo "  Cached: $(cat "${DEPLOY_DIR}/releases/.tag") ($(ls "${DEPLOY_DIR}/releases/" 2>/dev/null | wc -l) files)"
    else
      echo "  No releases synced"
    fi
    ;;

  logs)
    shift || true
    cd "$DEPLOY_DIR" && $COMPOSE_CMD logs -f --tail=100 "$@"
    ;;

  restart)
    # #1015: sync_releases no longer auto-called. The sync-releases.yml
    # workflow is the canonical writer for /opt/concord/releases/.
    # If you need to refresh release assets, trigger the workflow OR
    # invoke `concord-ctl.sh sync-releases` (manual recovery).
    echo "Restarting services..."
    echo "Note: release assets NOT refreshed automatically (#1015). Run 'concord-ctl.sh sync-releases' or trigger sync-releases.yml for a refresh."
    cd "$DEPLOY_DIR" && $COMPOSE_CMD restart
    echo "Done."
    ;;

  rebuild)
    activity_history_routine_guard
    clear_activity_history_image_selector
    # #1015: sync_releases no longer auto-called (see restart case comment).
    echo "Rebuilding and restarting..."
    echo "Note: release assets NOT refreshed automatically (#1015). Run 'concord-ctl.sh sync-releases' or trigger sync-releases.yml for a refresh."
    cd "$DEPLOY_DIR" && $COMPOSE_CMD up -d --build
    echo "Done."
    ;;

  activity-history-routine-guard)
    shift
    if [[ $# -ne 0 ]]; then
      echo "Usage: concord-ctl.sh activity-history-routine-guard" >&2
      exit 2
    fi
    activity_history_routine_guard
    ;;

  activity-history-activate)
    shift
    activity_history_activate "$@"
    ;;

  activity-history-rollback)
    shift
    activity_history_rollback "$@"
    ;;

  validate-config)
    # §F validation guard — invoke `docker compose config --quiet` to detect
    # missing env vars BEFORE attempting `up -d --build`. Exits non-zero with
    # operator-friendly message if interpolation fails.
    #
    # Spec ref: 2026-05-06-848-push-based-deploys-design.md §5.4
    if cd "$DEPLOY_DIR" && $COMPOSE_CMD config --quiet; then
      echo "  ✓ docker compose config validates"
    else
      echo "" >&2
      echo "  ✗ docker compose config failed — invalid YAML, missing compose file, or missing env var." >&2
      echo "    See docker output above for the specific failure." >&2
      echo "    If the error is a missing env var, re-run \`gh workflow run provision-secrets.yml\` to provision." >&2
      exit 1
    fi
    ;;

  nginx-reload)
    # §5.9 (#919): stage the source-of-truth nginx config from
    # /opt/concord/[internal]nginx/concordvoice.conf into
    # /etc/nginx/sites-available/concordvoice, validate, and reload/start nginx.
    #
    # Sudoers entry /etc/sudoers.d/concord-deploy (NOPASSWD) permits these
    # command shapes for the concord user:
    #   1. /usr/bin/install -o root -g root -m 0644 .../concordvoice.conf .../concordvoice
    #   2. /usr/sbin/nginx -t
    #   3. /bin/systemctl reload nginx
    #   4. /bin/systemctl start nginx (only after nginx -t passes and reload
    #      shows nginx is inactive)
    #
    # Sequence:
    #   a. Stage with install(1) (atomic create + chmod + chown).
    #   b. nginx -t — fail-fast on syntax error (DOES NOT reload if -t fails).
    #   c. systemctl reload nginx — graceful reload, no dropped connections.
    #   d. If reload fails because nginx is inactive, start nginx from the same
    #      syntax-validated config.
    #
    # Idempotency: re-running with no source change is a no-op at the
    # filesystem level (install(1) overwrites with byte-identical content)
    # and at the service level (systemctl reload SIGHUPs nginx workers).
    #
    # Spec ref: 2026-05-09-919-spa-deploy-trust-chain-design.md §5.9
    NGINX_SRC="${DEPLOY_DIR}/[internal]nginx/concordvoice.conf"
    NGINX_DST="/etc/nginx/sites-available/concordvoice"
    if [[ ! -f "$NGINX_SRC" ]]; then
      echo "" >&2
      echo "  ✗ nginx-reload: source-of-truth config not found at $NGINX_SRC" >&2
      echo "    Did the rsync of [internal]nginx/ complete? Check deploy.sh logs." >&2
      exit 1
    fi
    echo "  Staging $NGINX_SRC → $NGINX_DST"
    if ! sudo /usr/bin/install -o root -g root -m 0644 "$NGINX_SRC" "$NGINX_DST"; then
      echo "" >&2
      echo "  ✗ nginx-reload: install(1) staging failed (sudoers entry missing or wrong permissions?)" >&2
      exit 1
    fi
    # #1692: render/remove the admin-console vhost from runtime env. The
    # codename host stays out of git; ADMIN_WEBAUTHN_RP_ID in /opt/concord/.env
    # is the deploy-time source, and only ${ADMIN_DOMAIN} is envsubst-expanded.
    ADMIN_TMPL="${DEPLOY_DIR}/[internal]nginx/admin-console.conf.template"
    ADMIN_RENDER_DIR="${DEPLOY_DIR}/[internal]nginx/.rendered"
    ADMIN_RENDER="${ADMIN_RENDER_DIR}/concord-admin.conf"
    ADMIN_DST="/etc/nginx/conf.d/concord-admin.conf"
    ADMIN_ORIGIN_CERT="${ADMIN_ORIGIN_CERT:-/etc/nginx/cloudflare/origin.pem}"
    ADMIN_ORIGIN_KEY="${ADMIN_ORIGIN_KEY:-/etc/nginx/cloudflare/origin.key}"
    CONCORD_ENV_FILE="${CONCORD_ENV_FILE:-/opt/concord/.env}"
    read_concord_env() {
      local key="$1"
      if [[ -f "$CONCORD_ENV_FILE" ]]; then
        grep -E "^${key}=" "$CONCORD_ENV_FILE" 2>/dev/null | tail -1 | cut -d= -f2- | tr -d "'\"" || true
      fi
    }
    admin_enabled="$(read_concord_env ADMIN_CONSOLE_ENABLED)"
    admin_domain="$(read_concord_env ADMIN_WEBAUTHN_RP_ID)"
    if [[ "$admin_enabled" == "true" && -n "$admin_domain" ]]; then
      if [[ ! -e "$ADMIN_ORIGIN_CERT" || ! -e "$ADMIN_ORIGIN_KEY" ]]; then
        echo "  ⚠ admin vhost requested but Cloudflare Origin Certificate files are missing:" >&2
        echo "    cert: $ADMIN_ORIGIN_CERT" >&2
        echo "    key:  $ADMIN_ORIGIN_KEY" >&2
        echo "    Keeping admin closed; install the cert/key and rerun deploy to enable it." >&2
        # ponytail: fail closed but keep service deploys green; hard-fail here if admin availability becomes deploy-critical.
        echo "  Removing disabled admin vhost $ADMIN_DST"
        if ! sudo /bin/rm -f "$ADMIN_DST"; then
          echo "" >&2
          echo "  ✗ nginx-reload: admin vhost removal failed (sudoers entry missing or wrong permissions?)" >&2
          exit 1
        fi
      elif [[ ! -f "$ADMIN_TMPL" ]]; then
        echo "" >&2
        echo "  ✗ nginx-reload: admin template not found at $ADMIN_TMPL" >&2
        exit 1
      else
        mkdir -p "$ADMIN_RENDER_DIR"
        if ! ADMIN_DOMAIN="$admin_domain" envsubst '${ADMIN_DOMAIN}' < "$ADMIN_TMPL" > "$ADMIN_RENDER"; then
          echo "" >&2
          echo "  ✗ nginx-reload: failed to render admin vhost template" >&2
          exit 1
        fi
        echo "  Staging admin vhost $ADMIN_RENDER → $ADMIN_DST"
        if ! sudo /usr/bin/install -o root -g root -m 0644 "$ADMIN_RENDER" "$ADMIN_DST"; then
          echo "" >&2
          echo "  ✗ nginx-reload: admin vhost install failed (sudoers entry missing or wrong permissions?)" >&2
          exit 1
        fi
      fi
    else
      echo "  Removing disabled admin vhost $ADMIN_DST"
      if ! sudo /bin/rm -f "$ADMIN_DST"; then
        echo "" >&2
        echo "  ✗ nginx-reload: admin vhost removal failed (sudoers entry missing or wrong permissions?)" >&2
        exit 1
      fi
    fi

    echo "  Validating with nginx -t"
    if ! sudo /usr/sbin/nginx -t 2>&1; then
      echo "" >&2
      echo "  ✗ nginx-reload: nginx -t REJECTED the new config — REFUSING to reload" >&2
      echo "    Live config is unchanged (sites-enabled/ still points at old sites-available/)." >&2
      echo "    Inspect $NGINX_DST and re-run after fixing." >&2
      exit 1
    fi
    echo "  Reloading nginx"
    if sudo /bin/systemctl reload nginx; then
      echo "  ✓ nginx config staged + validated + reloaded"
    elif systemctl is-active --quiet nginx; then
      echo "" >&2
      echo "  ✗ nginx-reload: systemctl reload nginx failed" >&2
      exit 1
    else
      echo "  nginx service is inactive; starting nginx"
      if ! sudo /bin/systemctl start nginx; then
        echo "" >&2
        echo "  ✗ nginx-reload: systemctl start nginx failed" >&2
        exit 1
      fi
      echo "  ✓ nginx config staged + validated + started"
    fi
    ;;

  sudoers-sync)
    # Stage the source-of-truth sudoers file from
    # /opt/concord/[internal]sudoers.d/concord-deploy into
    # /etc/sudoers.d/concord-deploy via the narrow CONCORD_SUDOERS_INSTALL
    # entry. Required because nginx-reload above depends on the sudoers entry
    # being current — if a deploy ships a new nginx Cmnd_Alias and we haven't
    # synced sudoers, nginx-reload fails (#919 review finding H1).
    #
    # First-time install on a fresh VM: operator runs ONCE manually:
    #   sudo install -m 0440 -o root -g root \
    #     /opt/concord/[internal]sudoers.d/concord-deploy \
    #     /etc/sudoers.d/concord-deploy
    # After that, this subcommand handles all subsequent updates.
    SUDOERS_SRC="${SUDOERS_SRC_OVERRIDE:-/opt/concord/[internal]sudoers.d/concord-deploy}"
    SUDOERS_DEST="/etc/sudoers.d/concord-deploy"

    if [[ ! -f "$SUDOERS_SRC" ]]; then
      echo "  ✗ sudoers-sync: source-of-truth not found at $SUDOERS_SRC" >&2
      exit 1
    fi

    # Validate syntax BEFORE installing. visudo -cf returns non-zero if the
    # sudoers file would be rejected by sudo at load time. Refusing to
    # install a broken sudoers file is critical — sudo refuses to load any
    # file in /etc/sudoers.d/ if any entry is malformed, locking out concord
    # from the entire sudo surface.
    if ! visudo -cf "$SUDOERS_SRC" >/dev/null; then
      echo "  ✗ sudoers-sync: visudo -cf REJECTED $SUDOERS_SRC — refusing to install" >&2
      exit 1
    fi

    # If destination exists and is identical, no-op (idempotent).
    if [[ -f "$SUDOERS_DEST" ]] && cmp -s "$SUDOERS_SRC" "$SUDOERS_DEST"; then
      echo "  ✓ sudoers-sync: $SUDOERS_DEST already matches source (no-op)"
      exit 0
    fi

    if ! sudo -n /usr/bin/install -o root -g root -m 0440 "$SUDOERS_SRC" "$SUDOERS_DEST" 2>/dev/null; then
      echo "  ✗ sudoers-sync: install(1) staging failed" >&2
      echo "    Likely cause: bootstrap-install of /etc/sudoers.d/concord-deploy never ran." >&2
      echo "    First-time install: operator runs ONCE as root:" >&2
      echo "      sudo install -m 0440 -o root -g root \\" >&2
      echo "        /opt/concord/[internal]sudoers.d/concord-deploy \\" >&2
      echo "        /etc/sudoers.d/concord-deploy" >&2
      exit 1
    fi
    echo "  ✓ sudoers-sync: staged $SUDOERS_DEST (mode 0440, owner root:root)"
    ;;

  wait-healthy)
    # Server-side health gate for #848 deploy.yml. Wraps the loop pattern
    # from the now-deleted deploy.sh:117-134 with a CLI flag for timeout.
    #
    # Healthy = ALL of:
    #   - control-plane /health returns 200
    #   - media-plane /health returns 200
    #   - coturn STUN port (3478) accepts TCP connection
    #     (smoke test only — STUN is UDP, but coturn also listens TCP-3478;
    #      a TCP handshake confirms the daemon is alive but does not validate
    #      STUN responsiveness. For protocol-level STUN testing use stunclient.)
    #
    # Spec ref: 2026-05-06-848-push-based-deploys-design.md §5.4
    shift  # consume "wait-healthy"
    TIMEOUT=60
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --timeout) TIMEOUT="$2"; shift 2 ;;
        --timeout=*) TIMEOUT="${1#--timeout=}"; shift ;;
        *) echo "wait-healthy: unknown arg '$1'" >&2; exit 2 ;;
      esac
    done
    if ! [[ "$TIMEOUT" =~ ^[0-9]+$ ]]; then
      echo "wait-healthy: --timeout must be integer seconds, got '$TIMEOUT'" >&2
      exit 2
    fi
    INTERVAL=2
    DEADLINE=$(( $(date +%s) + TIMEOUT ))
    cp_ok=0; mp_ok=0; coturn_ok=0
    while [[ $(date +%s) -lt $DEADLINE ]]; do
      cp_ok=0; mp_ok=0; coturn_ok=0
      curl -sf "http://localhost:8080/health" >/dev/null 2>&1 && cp_ok=1
      curl -sf "http://localhost:3000/health" >/dev/null 2>&1 && mp_ok=1
      nc -z -w2 localhost 3478 >/dev/null 2>&1 && coturn_ok=1
      if [[ $cp_ok -eq 1 && $mp_ok -eq 1 && $coturn_ok -eq 1 ]]; then
        echo "  ✓ all services healthy (control-plane + media-plane + coturn)"
        exit 0
      fi
      sleep "$INTERVAL"
    done
    echo "  ✗ wait-healthy timed out after ${TIMEOUT}s" >&2
    [[ $cp_ok -eq 0 ]] && echo "    control-plane (:8080/health) failed" >&2
    [[ $mp_ok -eq 0 ]] && echo "    media-plane (:3000/health) failed" >&2
    [[ $coturn_ok -eq 0 ]] && echo "    coturn STUN (:3478) failed" >&2
    dump_health_diagnostics
    exit 1
    ;;

  freshstart)
    activity_history_routine_guard
    echo "=== Fresh Start: wiping all data and rebuilding ==="
    echo "This will DELETE all databases, caches, and user data."
    read -rp "Are you sure? (y/N) " confirm
    if [[ "${confirm,,}" != "y" ]]; then
      echo "Aborted."
      exit 0
    fi
    clear_activity_history_image_selector
    echo ""
    echo "Stopping all services and removing volumes..."
    cd "$DEPLOY_DIR" && $COMPOSE_CMD down -v
    echo ""
    # #1015: sync_releases no longer auto-called. If the server's
    # /opt/concord/releases/ is empty or stale, trigger sync-releases.yml
    # OR run `concord-ctl.sh sync-releases` before invoking freshstart.
    echo "Rebuilding and starting fresh..."
    echo "Note: release assets NOT refreshed automatically (#1015). Run 'concord-ctl.sh sync-releases' or trigger sync-releases.yml AFTER freshstart completes to populate /opt/concord/releases/."
    cd "$DEPLOY_DIR" && $COMPOSE_CMD up -d --build
    echo ""
    echo "Fresh start complete. All data has been wiped."
    ;;

  sync-releases)
    sync_releases
    ;;

  verify-registration)
    echo "=== Registration Flow Verification ==="
    failed=0

    # 1. pending_registrations table exists
    if docker exec concordvoice-postgres psql -U concord -d concord -Atc \
         "SELECT 1 FROM information_schema.tables WHERE table_name = 'pending_registrations';" \
         2>/dev/null | grep -q "1"; then
      echo "  ✓ pending_registrations table exists"
    else
      echo "  ✗ pending_registrations table missing"
      failed=$((failed + 1))
    fi

    # 2. No leftover unverified users
    UNVERIFIED=$(docker exec concordvoice-postgres psql -U concord -d concord -Atc \
                  "SELECT COUNT(*) FROM users WHERE email_verified = FALSE;" 2>/dev/null)
    if [[ "$UNVERIFIED" == "0" ]]; then
      echo "  ✓ no unverified users in users table"
    else
      echo "  ✗ ${UNVERIFIED} unverified users still present"
      failed=$((failed + 1))
    fi

    # 3. /register endpoint responds (400 expected on empty body)
    HTTP=$(curl -sf -o /dev/null -w "%{http_code}" \
             -X POST http://localhost:8080/api/v1/auth/register \
             -H "Content-Type: application/json" -d '{}' 2>/dev/null || echo "000")
    if [[ "$HTTP" == "400" ]]; then
      echo "  ✓ /register endpoint responding (400 on empty body expected)"
    else
      echo "  ✗ /register endpoint returned ${HTTP} (expected 400)"
      failed=$((failed + 1))
    fi

    if [[ $failed -gt 0 ]]; then
      echo ""
      echo "verify-registration: ${failed} check(s) failed"
      exit 1
    fi
    ;;

  docker-clean)
    # Reject unknown arguments loudly — a typo like `--agressive` must not
    # silently downgrade to conservative mode during incident response.
    case "${2:-}" in
      "") AGGRESSIVE=0 ;;
      --aggressive) AGGRESSIVE=1 ;;
      *) echo "docker-clean: unknown argument '${2}'. Only --aggressive is supported." >&2; exit 2 ;;
    esac

    # Timestamp in the banner so week-to-week cron log entries are distinguishable.
    echo "=== Docker Cleanup — $(date '+%Y-%m-%d %H:%M:%S %Z') ==="
    echo "Before:"
    docker system df
    echo ""

    if [[ $AGGRESSIVE -eq 1 ]]; then
      echo "Mode: aggressive (no time filter)"
      docker image prune -af
      docker builder prune -af
      # 24h filter gives operators a rollback window on containers they
      # may have stopped minutes earlier for debugging.
      docker container prune -f --filter "until=24h"
    else
      echo "Mode: conservative (retain last 168h)"
      docker image prune -af --filter "until=168h"
      docker builder prune -af --filter "until=168h"
    fi

    echo ""
    echo "After:"
    docker system df
    # Completion marker so operators/monitoring can grep for successful runs
    # versus runs that aborted mid-prune via set -euo pipefail.
    echo "=== Docker Cleanup — completed successfully ==="
    ;;

  stop)
    echo "Stopping all services..."
    cd "$DEPLOY_DIR" && $COMPOSE_CMD down
    echo "Done."
    ;;

  *)
    echo "Concord Service Manager"
    echo ""
    echo "Usage: $0 <command>"
    echo ""
    echo "Commands:"
    echo "  status         Show running containers, health checks, last deploy metadata"
    echo "  logs           Tail service logs (pass service name to filter, e.g. 'logs control-plane')"
    echo "  restart        Restart all services (no auto-sync since #1015)"
    echo "  rebuild        Force rebuild + restart all services (no auto-sync since #1015)"
    echo "                 Also clears a persisted Activity History rollback image pin"
    echo "  activity-history-routine-guard"
    echo "                 Verify canonical/rendered config and safe routine-deploy state"
    echo "  activity-history-activate --confirm-drained"
    echo "                 First activation: require zero CPs, preflight, CP-only start + health"
    echo "  activity-history-rollback --confirm-drained --old-image <immutable-image-ref> [--downgrade-schema]"
    echo "                 Drain/disable, optionally downgrade, and start one pinned old CP image"
    echo "  validate-config  §F: pre-rebuild guard via 'docker compose config --quiet'"
    echo "  nginx-reload   Stage infrastructure/deploy/nginx/concordvoice.conf into /etc/nginx/,"
    echo "                 validate via 'nginx -t', reload nginx (#919 §5.9)"
    echo "  wait-healthy   Block until all services healthy (--timeout SECONDS, default 60)"
    echo "  freshstart     Wipe all data, clear rollback image pin, rebuild from scratch"
    echo "  sync-releases  Manual operator-recovery — normal sync happens via GitHub Actions sync-releases.yml"
    echo "  verify-registration  Post-deploy smoke test for registration flow"
    echo "  docker-clean   Prune unused images + builder cache (weekly cron target)"
    echo "                 Add --aggressive to drop 7-day retention + prune stopped containers"
    echo "  stop           Stop all services"
    ;;
esac
