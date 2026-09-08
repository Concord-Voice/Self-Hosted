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
    ACTIVITY_HISTORY_PRIVACY_POLICY_URL ATTACHMENT_WRITE_BACKEND CONTROL_PLANE_IMAGE \
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
ACTIVITY_HISTORY_ACTIVATION_RECEIPT="${SCRIPT_DIR}/.env.activity-history-receipt.local"
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
  if [[ ${#value} -ge 2 && ( "$value" == \'*\' || "$value" == \"*\" ) ]]; then
    value="${value:1:${#value}-2}"
  fi
  printf '%s' "$value"
}

install_attachment_probe_cron() {
  local marker='concord-ctl.sh attachment-probe'
  local current
  local line="30 2 * * * ${DEPLOY_DIR}/[internal]concord-ctl.sh attachment-probe --backend r2-useast >> ${DEPLOY_DIR}/logs/attachment-probe.log 2>&1 && rm -f ${DEPLOY_DIR}/logs/attachment-probe-alert.flag || echo \"attachment-probe failed at \$(date -Is)\" > ${DEPLOY_DIR}/logs/attachment-probe-alert.flag"
  if ! current="$(LC_ALL=C crontab -l 2>&1)"; then
    if [[ "$current" != *"no crontab for"* ]]; then
      echo "attachment-probe-cron-install: could not read the existing crontab: $current" >&2
      return 1
    fi
    current=""
  fi
  {
    if [[ -n "$current" ]]; then
      grep -Fv "$marker" <<<"$current" || true
    fi
    printf '%s\n' "$line"
  } | crontab -
  echo "  ✓ nightly attachment probe cron installed"
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

# Optional $1 selects receipt handling: the default 'consult-receipt' lets a
# valid durable receipt continue a previously activated deployment at zero
# running containers; any other value (freshstart passes 'skip-receipt') keeps
# the pre-receipt fail-closed behavior — the receipt proves activation of the
# EXISTING durable state, so it never authorizes a wipe-and-recreate (#2326).
activity_history_routine_guard() {
  local receipt_mode="${1:-consult-receipt}"
  activity_history_load_contract || return 1
  [[ "$ACTIVITY_HISTORY_RENDERED_GATE" == false ]] && return 0
  if [[ "$ACTIVITY_HISTORY_RENDERED_REPLICAS" != 1 ]]; then
    echo "activity-history: gate=true requires exactly one configured control-plane replica" >&2
    return 1
  fi
  activity_history_refresh_daemon_ids || return 1
  if [[ ${#ACTIVITY_HISTORY_DAEMON_IDS[@]} -eq 1 ]] \
     && activity_history_container_gate_matches "${ACTIVITY_HISTORY_DAEMON_IDS[0]:-}" true; then
    # Backfill: a stack activated before the receipt feature proves itself with
    # live one-container gate-true evidence; mint the durable receipt so
    # stop→up / down→up recovery works after this deploy. Best-effort — a
    # failed write warns (the writer's stderr) but never blocks the deploy,
    # whose authorization comes from the live container, not the receipt.
    activity_history_activation_receipt_valid \
      || activity_history_write_activation_receipt \
      || true
    return 0
  fi
  if [[ ${#ACTIVITY_HISTORY_DAEMON_IDS[@]} -eq 0 && "$receipt_mode" == consult-receipt ]]; then
    if activity_history_activation_receipt_valid; then
      echo "Activity History: durable activation receipt accepted; resuming the previously activated deployment."
      return 0
    fi
    if [[ -e "$ACTIVITY_HISTORY_ACTIVATION_RECEIPT" ]]; then
      echo "activity-history: activation receipt is malformed or bound to a different deployment; refusing to continue" >&2
      return 1
    fi
  fi
  echo "activity-history: routine deployment cannot perform first activation or repair ambiguous state" >&2
  echo "Run concord-ctl.sh activity-history-activate --confirm-drained after draining traffic." >&2
  return 1
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
  local deadline now remaining probe_limit sleep_for probed=0
  if [[ ! "$timeout" =~ ^[1-9][0-9]*$ || ! "$interval" =~ ^[0-9]+$ ]]; then
    echo "activity-history: health timeout/interval must be integer seconds" >&2
    return 1
  fi
  deadline=$(( $(date +%s) + timeout ))
  while true; do
    now="$(date +%s)"
    remaining=$((deadline - now))
    if (( remaining <= 0 )); then
      # ALWAYS probe at least once. `date +%s` is whole-second granularity, so the
      # clock can tick between computing `deadline` above and reading `now` here.
      # With a small timeout that made `remaining` 0 on the FIRST pass, and the
      # function returned "timed out" having never called curl at all — a health
      # wait that reports a timeout without probing is wrong on its own terms.
      #
      # Reproduced deterministically with a `date` stub controlling the tick:
      #   no tick  -> rc=0, curl invoked once
      #   one tick -> rc=1, curl invoked ZERO times
      # It surfaced as a rare flake in test-activity-history-config.sh, which pins
      # ACTIVITY_HISTORY_HEALTH_TIMEOUT=1 and so straddles the boundary most often.
      (( probed )) && break
      remaining=1
    fi
    probed=1
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

# Durable activation receipt (#2256). Written only after a guarded activation's
# health checks and final gate-true daemon invariant succeed. Parsed as data
# (never sourced); consulted by the routine guard only when zero control-plane
# containers are running, so it can never override live container evidence.
# Rollback and freshstart invalidate it before their destructive mutation.
activity_history_write_activation_receipt() {
  local temp="${ACTIVITY_HISTORY_ACTIVATION_RECEIPT}.new.$$"
  umask 077
  if ! {
       printf 'ACTIVITY_HISTORY_ACTIVATION_STATE=activated\n'
       printf 'ACTIVITY_HISTORY_ACTIVATION_CONTRACT_MODE=%s\n' "$ACTIVITY_HISTORY_CONTRACT_MODE"
       printf 'ACTIVITY_HISTORY_ACTIVATION_INSTANCE_TYPE=%s\n' "$ACTIVITY_HISTORY_RENDERED_INSTANCE_TYPE"
     } >"$temp" \
     || ! chmod 600 "$temp" \
     || ! mv "$temp" "$ACTIVITY_HISTORY_ACTIVATION_RECEIPT"; then
    rm -f "$temp"
    echo "activity-history: control-plane is running and healthy, but the activation receipt could not be persisted; stop→up / down→up recovery will require re-activation" >&2
    return 1
  fi
}

# Valid only when the receipt exists, is owned by the invoking user with mode
# 0600 (a restored/hand-copied receipt with looser permissions or foreign
# ownership is NOT trusted), parses cleanly (exactly the three known keys, each
# once), and is bound to the current contract mode and rendered instance type.
# Any deviation is invalid — callers fail closed.
activity_history_activation_receipt_valid() {
  local line value state="" mode="" instance="" perms
  local state_count=0 mode_count=0 instance_count=0
  [[ -f "$ACTIVITY_HISTORY_ACTIVATION_RECEIPT" \
     && -O "$ACTIVITY_HISTORY_ACTIVATION_RECEIPT" ]] || return 1
  perms="$(stat -c '%a' "$ACTIVITY_HISTORY_ACTIVATION_RECEIPT" 2>/dev/null \
    || stat -f '%Lp' "$ACTIVITY_HISTORY_ACTIVATION_RECEIPT" 2>/dev/null)" || true
  [[ "$perms" == 600 ]] || return 1
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ -z "$line" ]] && continue
    value="${line#*=}"
    case "$line" in
      ACTIVITY_HISTORY_ACTIVATION_STATE=*)
        state_count=$((state_count + 1)); state="$value" ;;
      ACTIVITY_HISTORY_ACTIVATION_CONTRACT_MODE=*)
        mode_count=$((mode_count + 1)); mode="$value" ;;
      ACTIVITY_HISTORY_ACTIVATION_INSTANCE_TYPE=*)
        instance_count=$((instance_count + 1)); instance="$value" ;;
      *)
        return 1
        ;;
    esac
  done <"$ACTIVITY_HISTORY_ACTIVATION_RECEIPT"
  [[ $state_count -eq 1 && $mode_count -eq 1 && $instance_count -eq 1 ]] || return 1
  [[ "$state" == activated ]] || return 1
  [[ "$mode" == "$ACTIVITY_HISTORY_CONTRACT_MODE" ]] || return 1
  [[ "$instance" == "$ACTIVITY_HISTORY_RENDERED_INSTANCE_TYPE" ]]
}

activity_history_invalidate_activation_receipt() {
  rm -f "$ACTIVITY_HISTORY_ACTIVATION_RECEIPT" 2>/dev/null || true
  if [[ -e "$ACTIVITY_HISTORY_ACTIVATION_RECEIPT" ]]; then
    echo "activity-history: failed to invalidate the activation receipt; refusing to continue" >&2
    return 1
  fi
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
  activity_history_write_activation_receipt || return 1
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
  # step may already have reached 86, so repeating it is unsafe, while an older
  # migrator may not recognize v87. Automatic recovery therefore refuses both
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
    activity_history_invalidate_activation_receipt || return 1
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
  activity_history_invalidate_activation_receipt || return 1
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

# nginx-reload and sudoers-sync both use fixed root-owned candidate paths.
# Serialize them on one host-global inode so a copied or symlinked controller
# cannot overlap post-rsync (or another direct invocation). Provisioning creates
# this file inside a root-owned directory, owned by concord but not unlinkable by
# concord. Keeping that inode is load-bearing: kernel lock ownership, not file
# existence, controls admission.
readonly DEPLOY_TRANSACTION_LOCK_PATH="/var/lib/concord/deploy-transaction.lock"
# Non-blocking by default; the deploy verbs raise it just before acquiring.
DEPLOY_LOCK_WAIT_SECS=0
readonly LOCKF_REENTRY_MARKER="--concord-ctl-lockf-reentry-v1"
CONCORD_CTL_LOCKF_REENTRY_INTERNAL=0

validate_lockf_reentry_parent() {
  local probe_rc=0

  # The outer wrapper alone opens status descriptor 8. It also holds the fixed
  # pathname through lockf, so an immediate second non-blocking acquisition
  # must report EX_TEMPFAIL. This avoids relying on ps(1), which is unavailable
  # in several hardened CI/macOS sandboxes.
  [[ -e /dev/fd/8 ]] || return 1
  /usr/bin/lockf -s -k -t 0 "$DEPLOY_TRANSACTION_LOCK_PATH" /usr/bin/true \
    || probe_rc=$?
  [[ "$probe_rc" -eq 75 ]]
}

# Normalize the guarded macOS re-entry before ordinary command dispatch. This
# is an argv marker rather than a caller-controlled environment bypass, and its
# lockf ancestry is revalidated before it is honored.
if [[ "${1:-}" == "$LOCKF_REENTRY_MARKER" ]]; then
  if ! validate_lockf_reentry_parent; then
    echo "  ✗ rejected invalid internal deploy-lock re-entry" >&2
    exit 1
  fi
  CONCORD_CTL_LOCKF_REENTRY_INTERNAL=1
  shift
fi

acquire_deploy_transaction_lock() {
  local operation="$1" lock_rc=0 lock_backend=""

  # SELF-HOSTED HOSTS HAVE NO LOCK, AND MUST NOT BE REFUSED FOR IT.
  # /var/lib/concord is created by provision-production.sh's
  # install_sudoers_boundary -- a MANAGED-host step. A self-hosted operator runs
  # install-selfhost.sh instead and never has that directory, so requiring the
  # lock here would break `concord-selfhost up|stop|down|freshstart` outright.
  #
  # Skipping is correct rather than merely tolerable: the same provisioning step
  # that creates this directory is the one that installs the #3107 healthwatch
  # actor and its /var/lib/concord/healthwatch state. No directory means no
  # actor, which means no contender -- the interlock is protecting against
  # something that cannot exist on that host.
  #
  # Scoped to the WAITING callers on purpose. nginx-reload and sudoers-sync are
  # managed-only paths that legitimately require the lock, and their strict
  # behaviour is untouched: they pass no wait, so they never take this branch.
  # The skip is ANNOUNCED, never silent. A deploy that quietly runs without the
  # interlock it was told it has is the worse failure of the two: it looks
  # identical to one that took the lock. Self-hosted hosts are single-operator
  # and have no healthwatch actor today, so the exposure is nil -- but that is a
  # property of TODAY'S self-host topology, not a guarantee, and the notice is
  # what makes it re-checkable when that changes.
  if (( ${DEPLOY_LOCK_WAIT_SECS:-0} > 0 )) \
     && [[ ! -d "$(dirname "$DEPLOY_TRANSACTION_LOCK_PATH")" ]]; then
    echo "  ℹ ${operation}: no $(dirname "$DEPLOY_TRANSACTION_LOCK_PATH") — running WITHOUT the deploy transaction lock." >&2
    echo "    Expected on a self-hosted host: that directory and the #3107 healthwatch actor are both" >&2
    echo "    created by provision-production.sh, which self-hosted installs never run. No actor means" >&2
    echo "    no contender. A self-hosted interlock of its own is tracked at #3161 (epic #1615)." >&2
    return 0
  fi
  # Bounded wait in seconds, taken from DEPLOY_LOCK_WAIT_SECS rather than a
  # positional argument: callers invoke this as `"$@"`, and the lockf backend
  # re-executes the controller with that same argv, so a trailing wait argument
  # would both be misread ($2 is the VERB's first argument) and corrupt the
  # re-exec. Defaulted to 0 at declaration so an inherited environment value
  # cannot silently make a deploy block.
  local wait_secs="${DEPLOY_LOCK_WAIT_SECS:-0}"
  [[ "$wait_secs" =~ ^[0-9]+$ ]] || wait_secs=0
  local lock_path="$DEPLOY_TRANSACTION_LOCK_PATH"
  local status_path="" child_status=""

  if (( CONCORD_CTL_LOCKF_REENTRY_INTERNAL )); then
    return 0
  fi

  if [[ -x /usr/bin/flock ]]; then
    lock_backend=flock
    if ! exec 9>"$lock_path"; then
      echo "  ✗ ${operation}: cannot open deploy transaction lock at $lock_path" >&2
      exit 1
    fi
    if (( wait_secs > 0 )); then
      /usr/bin/flock -w "$wait_secs" 9 || lock_rc=$?
    else
      /usr/bin/flock -n 9 || lock_rc=$?
    fi
  elif [[ -x /usr/bin/lockf ]]; then
    lock_backend=lockf
    # BSD/macOS lockf portably locks a pathname while it runs a command. Let
    # that parent retain the lock while a guarded child runs the controller.
    # -k retains the inode. The wrapper records the controller status on an
    # inherited descriptor and exits zero, so a real child exit 75 cannot be
    # confused with lockf's EX_TEMPFAIL contention status.
    status_path="$(/usr/bin/mktemp /tmp/concord-ctl-lock-status.XXXXXX)" || {
      echo "  ✗ ${operation}: cannot create lock status channel" >&2
      exit 1
    }
    /bin/chmod 0600 "$status_path"
    exec 8>"$status_path"
    /usr/bin/lockf -s -k -t 0 "$lock_path" /bin/bash -c '
      "$@"
      child_rc=$?
      printf "%s\n" "$child_rc" >&8 || exit 125
      exit 0
    ' _ "$0" "$LOCKF_REENTRY_MARKER" "$@" || lock_rc=$?
    exec 8>&-

    if [[ -s "$status_path" ]]; then
      IFS= read -r child_status < "$status_path" || child_status=""
      /bin/rm -f -- "$status_path"
      if [[ ! "$child_status" =~ ^([0-9]|[1-9][0-9]|1[0-9]{2}|2[0-4][0-9]|25[0-5])$ ]]; then
        echo "  ✗ ${operation}: invalid child status from deploy lock" >&2
        exit 1
      fi
      exit "$child_status"
    fi
    /bin/rm -f -- "$status_path"
    if (( lock_rc == 0 )); then
      lock_rc=125
    fi
  else
    echo "  ✗ ${operation}: no supported deploy lock utility found" >&2
    echo "    Expected /usr/bin/flock (Linux) or /usr/bin/lockf (macOS)." >&2
    exit 1
  fi

  if (( lock_rc != 0 )); then
    if [[ ( "$lock_backend" == flock && "$lock_rc" -eq 1 ) \
       || ( "$lock_backend" == lockf && "$lock_rc" -eq 75 ) ]]; then
      echo "  ✗ ${operation}: deploy transaction lock is busy at $lock_path" >&2
      echo "    Another deploy, nginx-reload, sudoers-sync, or the healthwatch actor" >&2
      echo "    (#3107) holds it; no changes were made." >&2
    else
      echo "  ✗ ${operation}: failed to acquire deploy transaction lock at $lock_path (rc=$lock_rc)" >&2
    fi
    exit 1
  fi
}

case "${1:-help}" in
  nginx-reload|sudoers-sync)
    acquire_deploy_transaction_lock "$@"
    ;;
  rebuild|restart|stop|activity-history-activate|activity-history-rollback)
    # The two activity-history verbs belong here for the SAME reason the deploy
    # verbs do, and leaving them out was an omission rather than a decision: both
    # stop and start the control plane, and both then gate on health and image
    # checks afterwards. An actor cycle holding pre-transition unhealthy evidence
    # can sample the freshly started container and restart it mid-gate, failing a
    # guarded activation or rollback for a reason nothing in their output would
    # explain.
    #
    # Bounded wait, NOT fail-fast. The healthwatch actor (#3107) takes this same
    # inode with `flock -n` and yields immediately on contention, so its hold is
    # bounded by one cycle -- but that cycle can include a control-plane restart
    # carrying a 45s stop_grace_period. Failing the deploy there would surface as
    # an unexplained "lock is busy" on a few percent of deploys and read as CI
    # flake. 90s comfortably exceeds the actor's worst-case hold.
    DEPLOY_LOCK_WAIT_SECS=90
    acquire_deploy_transaction_lock "$@"
    ;;
esac

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
    shift  # consume "rebuild"
    # Optional service scope (#2178). `rebuild` with NO arguments is byte-for-byte
    # the pre-#2178 behavior; every out-of-band caller (operator SSH,
    # provision-production.sh, an older post-rsync.sh) therefore keeps working.
    #
    # Scope tokens originate from a `git diff` on the CI runner and cross an SSH
    # env boundary before landing here, then get interpolated into a `docker
    # compose` invocation. Validate and REJECT — never sanitize.
    REBUILD_SCOPE=()
    for _svc in "$@"; do
      if [[ ! "$_svc" =~ ^[a-z][a-z0-9-]*$ ]]; then
        echo "rebuild: invalid service name '$_svc'" >&2
        exit 2
      fi
      case "$_svc" in
        control-plane|media-plane|ops-agent|coturn|postgres|redis|nats|minio) ;;
        *)
          echo "rebuild: unknown compose service '$_svc'" >&2
          exit 2
          ;;
      esac
      REBUILD_SCOPE+=("$_svc")
    done

    # `--no-deps` skips dependency startup AND the `depends_on` conditions, so it
    # is only correct when the dependency containers are already up. Escalate to
    # a full rebuild otherwise; the fail-safe direction is always "rebuild more",
    # never "fail".
    #
    # Checking only the scoped service is NOT sufficient (#2178 review, HIGH —
    # found independently by three reviewers). media-plane declares
    # `nats: service_healthy`, and that gate was doing real work: the initial
    # NATS connect is one-shot (services/media-plane/src/lib/nats.ts assigns
    # `this.nc` only inside the try, and nothing retries it), so a media-plane
    # that starts while NATS is down runs PERMANENTLY with all four
    # `voice.enforce.*` subscriptions silently absent — server mute/deafen,
    # mid-session RBAC revocation, temporary-SBAC revocation, and DM membership
    # removal all stop arriving. Voice audio keeps working, so a banned or
    # server-muted member keeps transmitting until they leave voluntarily.
    # `/health` answers 200 throughout, so neither `--wait` nor `wait-healthy`
    # can see it.
    #
    # Escalating on a down dependency restores what `depends_on` used to
    # guarantee. Dependencies are read from the rendered compose config rather
    # than hardcoded, so this cannot drift from the compose files.
    # ERROR HANDLING DIRECTION — read before editing. Every failure to DETERMINE
    # the dependency set must escalate to a full rebuild. Suppressing an error
    # here does NOT mean "check fewer things"; it means fewer chances for the
    # probe below to fire, so the scoped `--no-deps` path runs — which is exactly
    # the fail-open this block exists to close. This mirrors the same operation
    # done correctly for the Activity History contract ~750 lines above.
    _rebuild_scoped=0
    if [[ ${#REBUILD_SCOPE[@]} -gt 0 ]]; then
      _rebuild_scoped=1
      _scope_closure=()
      # stderr goes to a SEPARATE file, never merged into the JSON. `docker
      # compose` writes warnings to stderr while still exiting 0, so `2>&1`
      # would prefix the JSON with a routine warning, fail the parse, and
      # escalate to a full rebuild on EVERY deploy on that host — the scoping
      # would silently never engage, with nothing in the log to say why.
      # A good diagnostic and a parseable stream are different jobs.
      _stderr_file="$(mktemp)"
      if ! _rendered="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD config --format json 2>"$_stderr_file")"; then
        echo "rebuild: cannot render Compose configuration to resolve dependencies — escalating to a full rebuild." >&2
        sed 's/^/         /' "$_stderr_file" >&2
        rm -f "$_stderr_file"
        _rebuild_scoped=0
      else
        rm -f "$_stderr_file"
        _dep_err="$(mktemp)"
        for _svc in "${REBUILD_SCOPE[@]}"; do
          _scope_closure+=("$_svc")
          # `depends_on` renders as an object (service -> {condition:...}) under
          # `config --format json`, or as a plain list in the short form. Emit
          # "<name> <condition>" pairs so the probe can require health where the
          # compose file requires health.
          if ! _deps="$(python3 -c '
import json, sys
svc = json.load(sys.stdin)["services"][sys.argv[1]]
dep = svc.get("depends_on") or {}
if isinstance(dep, dict):
    for name, spec in dep.items():
        cond = (spec or {}).get("condition", "service_started") if isinstance(spec, dict) else "service_started"
        print(name, cond)
else:
    for name in dep:
        print(name, "service_started")
' "$_svc" <<<"$_rendered" 2>"$_dep_err")"; then
            echo "rebuild: cannot resolve depends_on for '$_svc' — escalating to a full rebuild." >&2
            sed 's/^/         /' "$_dep_err" >&2
            rm -f "$_dep_err"
            _rebuild_scoped=0
            break
          fi
          rm -f "$_dep_err"
          while IFS= read -r _dep_line; do
            [[ -n "$_dep_line" ]] && _scope_closure+=("$_dep_line")
          done <<<"$_deps"
        done
      fi
    fi

    # Probe every member of the closure. A dependency declared `service_healthy`
    # must be HEALTHY, not merely running — `ps --status running` carries no
    # health axis, and a running-but-unhealthy NATS would otherwise pass while
    # media-plane came up against a broken broker.
    if [[ $_rebuild_scoped -eq 1 ]]; then
      for _entry in "${_scope_closure[@]}"; do
        _svc="${_entry%% *}"
        _cond="${_entry#* }"
        [[ "$_cond" == "$_svc" ]] && _cond="service_started"
        # stderr separated here too, and for a sharper reason than the JSON
        # cases: `_ids` is tested with `-z`, so a Compose warning merged into it
        # would make the result NON-EMPTY and the service would read as running
        # even when it is not — a fail-open in the guard's own liveness check.
        _probe_err="$(mktemp)"
        if ! _ids="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --status running -q "$_svc" 2>"$_probe_err")"; then
          echo "rebuild: cannot probe '$_svc' — escalating to a full rebuild." >&2
          sed 's/^/         /' "$_probe_err" >&2
          rm -f "$_probe_err"
          _rebuild_scoped=0
          break
        fi
        rm -f "$_probe_err"
        if [[ -z "$_ids" ]]; then
          echo "rebuild: '$_svc' (scoped service or one of its dependencies) has no running container — escalating to a full rebuild." >&2
          _rebuild_scoped=0
          break
        fi
        if [[ "$_cond" == "service_healthy" ]]; then
          # Same stderr discipline as the config render above: a Compose warning
          # merged into this stream would fail the JSON parse and escalate every
          # deploy, making the scoping permanently inert.
          _health_err="$(mktemp)"
          if ! _health="$(cd "$DEPLOY_DIR" && $COMPOSE_CMD ps --format json "$_svc" 2>"$_health_err")"; then
            echo "rebuild: cannot read health for '$_svc' — escalating to a full rebuild." >&2
            sed 's/^/         /' "$_health_err" >&2
            rm -f "$_health_err"
            _rebuild_scoped=0
            break
          fi
          # EVERY row must be healthy. Reading only rows[0] lets one healthy
          # container mask an unhealthy sibling if a dependency is ever scaled.
          if ! _state="$(python3 -c '
import json, sys
raw = sys.stdin.read().strip()
rows = json.loads(raw) if raw.startswith("[") else [
    json.loads(line) for line in raw.splitlines() if line.strip()
]
if not rows:
    print("")
else:
    bad = [r.get("Health", "") for r in rows if r.get("Health", "") != "healthy"]
    print(bad[0] if bad else "healthy")
' <<<"$_health" 2>"$_health_err")"; then
            echo "rebuild: cannot parse health for '$_svc' — escalating to a full rebuild." >&2
            sed 's/^/         /' "$_health_err" >&2
            rm -f "$_health_err"
            _rebuild_scoped=0
            break
          fi
          rm -f "$_health_err"
          if [[ "$_state" != "healthy" ]]; then
            echo "rebuild: '$_svc' is declared service_healthy but reports '${_state:-unknown}' — escalating to a full rebuild." >&2
            _rebuild_scoped=0
            break
          fi
        fi
      done
    fi

    # Unconditional, and deliberately BEFORE any scope branch: this is the
    # Activity History first-activation boundary. A scope-conditional guard here
    # would be a first-activation bypass. It takes an optional receipt_mode
    # argument, so the scope list must never reach it.
    activity_history_routine_guard

    # ...but the image-selector clear IS scope-gated (#2178). It rm's the
    # control-plane rollback image pin and its own echo asserts "rebuilding the
    # current source" — an assertion a media-only scope does not honor. Clearing
    # it unscoped would delete an operator's active pin while leaving the running
    # control-plane on the pinned image: a silent divergence that surfaces later
    # as an unexplained control-plane jump.
    #
    # Membership is tested in pure bash, NOT `printf ... | grep -qx`. Under
    # `set -o pipefail` (line 4) `grep -q` exits on first match and closes the
    # read end, so a producer still writing takes SIGPIPE and the pipeline
    # reports 141 despite the match. The window is vanishingly small for a
    # builtin writing a few short tokens, but the failure DIRECTION is severe: a
    # false negative skips the clear while control-plane IS in scope, leaving
    # CONTROL_PLANE_IMAGE exported, so the rebuild deploys the pinned ROLLBACK
    # image instead of the new code — and then stamps .deploy-meta with the new
    # SHA, hiding the change from every future scope diff.
    _has_control_plane=0
    for _s in ${REBUILD_SCOPE[@]+"${REBUILD_SCOPE[@]}"}; do
      if [[ "$_s" == "control-plane" ]]; then
        _has_control_plane=1
        break
      fi
    done
    if [[ $_rebuild_scoped -eq 0 || $_has_control_plane -eq 1 ]]; then
      clear_activity_history_image_selector
    else
      echo "Keeping the Activity History rollback image pin: control-plane is not in this rebuild's scope."
    fi

    # #1015: sync_releases no longer auto-called (see restart case comment).
    echo "Rebuilding and restarting..."
    echo "Note: release assets NOT refreshed automatically (#1015). Run 'concord-ctl.sh sync-releases' or trigger sync-releases.yml for a refresh."
    if [[ $_rebuild_scoped -eq 1 ]]; then
      # --wait-timeout is MANDATORY, not decorative: `--wait` installs a deadline
      # only when WaitTimeout > 0, so a bare --wait blocks forever and would hang
      # the deploy's SSH session with the stack in an indeterminate state.
      echo "Scope: ${REBUILD_SCOPE[*]} (--no-deps --wait)"
      cd "$DEPLOY_DIR" && $COMPOSE_CMD up -d --build --no-deps --wait --wait-timeout 180 "${REBUILD_SCOPE[@]}"
    else
      # Unscoped stays WITHOUT --wait. Adding it here would pull every project
      # service into the wait set — including coturn, whose STUN probe flaking
      # would abort a full deploy that previously succeeded — and would duplicate
      # post-rsync.sh's wait-healthy, which has far better diagnostics.
      cd "$DEPLOY_DIR" && $COMPOSE_CMD up -d --build
    fi
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

  attachment-probe)
    shift
    attachment_write_backend=legacy
    if [[ -f "${DEPLOY_DIR}/.env" ]]; then
      attachment_write_backend_count="$(grep -cE '^ATTACHMENT_WRITE_BACKEND=' "${DEPLOY_DIR}/.env" 2>/dev/null || true)"
      if [[ "$attachment_write_backend_count" == 1 ]]; then
        if ! attachment_write_backend="$(activity_history_read_root_value ATTACHMENT_WRITE_BACKEND)"; then
          echo "attachment-probe: could not read ATTACHMENT_WRITE_BACKEND from ${DEPLOY_DIR}/.env" >&2
          exit 1
        fi
        attachment_write_backend="${attachment_write_backend#"${attachment_write_backend%%[![:space:]]*}"}"
        attachment_write_backend="${attachment_write_backend%"${attachment_write_backend##*[![:space:]]}"}"
        attachment_write_backend="${attachment_write_backend:-legacy}"
      elif [[ "$attachment_write_backend_count" != 0 ]]; then
        echo "attachment-probe: ATTACHMENT_WRITE_BACKEND is duplicated in ${DEPLOY_DIR}/.env" >&2
        exit 1
      fi
    fi
    # With explicit arguments, storage-probe owns validation and backend selection.
    if [[ $# -eq 0 ]]; then
      if [[ "$attachment_write_backend" == "legacy" ]]; then
        echo "  ✓ attachment probe dormant: ATTACHMENT_WRITE_BACKEND=legacy"
        exit 0
      fi
      if [[ "$attachment_write_backend" != "legacy" && "$attachment_write_backend" != "r2-useast" ]]; then
        echo "attachment-probe: invalid ATTACHMENT_WRITE_BACKEND '$attachment_write_backend'" >&2
        exit 1
      fi
    fi
    cd "$DEPLOY_DIR" && $COMPOSE_CMD run --rm --no-deps --pull never control-plane ./main storage-probe "$@"
    ;;

  attachment-probe-cron-install)
    shift
    if [[ $# -ne 0 ]]; then
      echo "Usage: concord-ctl.sh attachment-probe-cron-install" >&2
      exit 2
    fi
    install_attachment_probe_cron
    ;;

  nginx-reload)
    # §5.9 (#919): render the source-of-truth nginx config with the validated
    # API HSTS policy, transactionally activate the fixed main/admin paths,
    # validate, reload/start, and probe the active public-edge policy.
    #
    # Sequence:
    #   a. Validate/decode all unprivileged input and render inactive files.
    #   b. Root-stage fixed candidates, hard-link prior inodes, atomically swap.
    #   c. nginx -t, graceful reload/start, and local /nginx-health HSTS probe.
    #   d. On any failure after swapping, restore both exact prior files and
    #      reload/start the old configuration when activation was attempted.
    #
    # Idempotency: re-running with no source change is a no-op at the
    # filesystem level (fixed candidates replace the same live paths) and at
    # the service level (systemctl reload SIGHUPs nginx workers).
    #
    # Spec ref: 2026-05-09-919-spa-deploy-trust-chain-design.md §5.9
    NGINX_SRC="${DEPLOY_DIR}/[internal]nginx/concordvoice.conf"
    NGINX_RENDER_DIR="${DEPLOY_DIR}/[internal]nginx/.rendered"
    NGINX_RENDER="${NGINX_RENDER_DIR}/concordvoice.conf"
    NGINX_DST="/etc/nginx/sites-available/concordvoice"
    NGINX_CANDIDATE="/etc/nginx/sites-available/.concordvoice.candidate"
    NGINX_BACKUP="/etc/nginx/sites-available/.concordvoice.backup"
    ADMIN_TMPL="${DEPLOY_DIR}/[internal]nginx/admin-console.conf.template"
    ADMIN_RENDER="${NGINX_RENDER_DIR}/concord-admin.conf"
    ADMIN_DST="/etc/nginx/conf.d/concord-admin.conf"
    ADMIN_CANDIDATE="/etc/nginx/conf.d/.concord-admin.candidate"
    ADMIN_BACKUP="/etc/nginx/conf.d/.concord-admin.backup"
    ADMIN_ORIGIN_CERT="${ADMIN_ORIGIN_CERT:-/etc/nginx/cloudflare/origin.pem}"
    ADMIN_ORIGIN_KEY="${ADMIN_ORIGIN_KEY:-/etc/nginx/cloudflare/origin.key}"
    CONCORD_ENV_FILE="${CONCORD_ENV_FILE:-/opt/concord/.env}"
    HSTS_DEFAULT='max-age=63072000; includeSubDomains; preload'
    if [[ ! -f "$NGINX_SRC" ]]; then
      echo "" >&2
      echo "  ✗ nginx-reload: source-of-truth config not found at $NGINX_SRC" >&2
      echo "    Did the rsync of [internal]nginx/ complete? Check deploy.sh logs." >&2
      exit 1
    fi

    # Decode only canonical writer data forms; never source the runtime env
    # file. Supports plain data, Bash @Q single/ANSI-C quoting, and Compose's
    # double-quoted dotenv form (\", \\, and $$). No eval or expansion.
    decode_concord_env_value() {
      local raw="$1" decoded="" char escape inner
      local i=0 length=${#1}
      if [[ "$raw" == \'* ]]; then
        (( length >= 2 )) || return 1
        i=1
        while (( i < length )); do
          char="${raw:i:1}"
          if [[ "$char" != "'" ]]; then
            decoded+="$char"
            i=$((i + 1))
          elif (( i == length - 1 )); then
            printf '%s' "$decoded"
            return 0
          elif [[ "${raw:i:4}" == "'\\''" ]]; then
            decoded+="'"
            i=$((i + 4))
          else
            return 1
          fi
        done
        return 1
      fi
      if [[ "$raw" == \$\'* ]]; then
        (( length >= 3 )) && [[ "${raw: -1}" == "'" ]] || return 1
        inner="${raw:2:$((length - 3))}"
        length=${#inner}
        i=0
        while (( i < length )); do
          char="${inner:i:1}"
          if [[ "$char" == \\ ]]; then
            (( i + 1 < length )) || return 1
            escape="${inner:i+1:1}"
            case "$escape" in
              t) decoded+=$'\t' ;;
              "'") decoded+="'" ;;
              *) return 1 ;;
            esac
            i=$((i + 2))
          elif [[ "$char" == "'" ]]; then
            return 1
          else
            decoded+="$char"
            i=$((i + 1))
          fi
        done
        printf '%s' "$decoded"
        return 0
      fi
      if [[ "$raw" == '"'* ]]; then
        (( length >= 2 )) && [[ "${raw: -1}" == '"' ]] || return 1
        inner="${raw:1:$((length - 2))}"
        length=${#inner}
        i=0
        while (( i < length )); do
          char="${inner:i:1}"
          if [[ "$char" == \\ ]]; then
            (( i + 1 < length )) || return 1
            escape="${inner:i+1:1}"
            case "$escape" in
              \\) decoded+=\\ ;;
              '"') decoded+='"' ;;
              *) return 1 ;;
            esac
            i=$((i + 2))
          elif [[ "$char" == '$' ]]; then
            (( i + 1 < length )) && [[ "${inner:i+1:1}" == '$' ]] || return 1
            decoded+='$'
            i=$((i + 2))
          elif [[ "$char" == '"' ]]; then
            return 1
          else
            decoded+="$char"
            i=$((i + 1))
          fi
        done
        printf '%s' "$decoded"
        return 0
      fi
      printf '%s' "$raw"
    }

    read_concord_env_value() {
      local key="$1" line raw="" count=0
      [[ -f "$CONCORD_ENV_FILE" ]] || { printf ''; return 0; }
      cat "$CONCORD_ENV_FILE" >/dev/null 2>&1 || return 1
      while IFS= read -r line || [[ -n "$line" ]]; do
        case "$line" in
          "${key}"=*)
            count=$((count + 1))
            raw="${line#*=}"
            ;;
        esac
      done < "$CONCORD_ENV_FILE"
      (( count <= 1 )) || return 1
      (( count == 1 )) || { printf ''; return 0; }
      decode_concord_env_value "$raw"
    }

    # Keep this in lockstep with config.invalidHSTSHeaderValue: printable ASCII
    # plus TAB, semicolon-separated unique directives, exactly one decimal
    # max-age, valueless includeSubDomains/preload, and token/quoted values.
    validate_hsts_header_value() {
      local value="$1" remaining directive name val inner lower
      local has_value last=false
      local LC_ALL=C
      local printable_re=$'^[\t -~]*$'
      local -A seen=()
      [[ -z "$value" ]] && return 0
      [[ "$value" =~ $printable_re ]] || return 1
      remaining="$value"
      while :; do
        if [[ "$remaining" == *';'* ]]; then
          directive="${remaining%%;*}"
          remaining="${remaining#*;}"
        else
          directive="$remaining"
          last=true
        fi
        directive="${directive#"${directive%%[![:blank:]]*}"}"
        directive="${directive%"${directive##*[![:blank:]]}"}"
        if [[ -n "$directive" ]]; then
          has_value=0
          if [[ "$directive" == *=* ]]; then
            name="${directive%%=*}"
            val="${directive#*=}"
            has_value=1
          else
            name="$directive"
            val=""
          fi
          [[ "$name" =~ ^[A-Za-z0-9-]+$ ]] || return 1
          if (( has_value )); then
            if [[ "$val" =~ ^[A-Za-z0-9._-]+$ ]]; then
              :
            elif (( ${#val} >= 2 )) \
              && [[ "${val:0:1}" == '"' && "${val: -1}" == '"' ]]; then
              inner="${val:1:$(( ${#val} - 2 ))}"
              [[ "$inner" != *'"'* && "$inner" != *\\* ]] || return 1
            else
              return 1
            fi
          fi
          lower="${name,,}"
          [[ -z "${seen[$lower]+present}" ]] || return 1
          seen["$lower"]=1
          if [[ "$lower" == max-age ]]; then
            (( has_value )) && [[ "$val" =~ ^[0-9]+$ ]] || return 1
          elif (( has_value )) && [[ "$lower" == includesubdomains || "$lower" == preload ]]; then
            return 1
          fi
        fi
        [[ "$last" == true ]] && break
      done
      [[ -n "${seen[max-age]+present}" ]]
    }

    encode_hsts_for_nginx() {
      local value="$1" encoded="" char i
      for ((i = 0; i < ${#value}; i++)); do
        char="${value:i:1}"
        # Append the literal nginx variable; do not expand it in Bash.
        # shellcheck disable=SC2016
        case "$char" in
          '"') encoded+='\"' ;;
          '$') encoded+='${concord_hsts_dollar}' ;;
          *) encoded+="$char" ;;
        esac
      done
      printf '%s' "$encoded"
    }

    render_nginx_config() {
      local policy="$1" encoded line tmp
      local in_api=0 scoped_replacements=0 owner_markers=0
      encoded="$(encode_hsts_for_nginx "$policy")" || return 1
      mkdir -p "$NGINX_RENDER_DIR" || return 1
      tmp="$(mktemp "${NGINX_RENDER}.tmp.XXXXXX")" || return 1
      if ! while IFS= read -r line || [[ -n "$line" ]]; do
        case "$line" in
          '# HTTPS — API + WebSocket Hub'*) in_api=1 ;;
          '# HTTPS — Media Plane'*) in_api=0 ;;
        esac
        if (( in_api )) && [[ "$line" == "    # CONCORD_MANAGED_HSTS_OWNER" ]]; then
          printf '%s\n' "$line"
          printf '    proxy_hide_header Strict-Transport-Security;\n'
          printf '    add_header Strict-Transport-Security "%s" always;\n' "$encoded"
          owner_markers=$((owner_markers + 1))
        elif (( in_api )) && [[ "$line" == "        add_header Strict-Transport-Security "* ]]; then
          printf '        add_header Strict-Transport-Security "%s" always;\n' "$encoded"
          scoped_replacements=$((scoped_replacements + 1))
        else
          printf '%s\n' "$line"
        fi
      done < "$NGINX_SRC" > "$tmp"; then
        rm -f "$tmp"
        return 1
      fi
      if (( owner_markers != 1 || scoped_replacements != 3 )); then
        rm -f "$tmp"
        return 1
      fi
      if ! chmod 0644 "$tmp" || ! mv -f "$tmp" "$NGINX_RENDER"; then
        rm -f "$tmp"
        return 1
      fi
    }

    if ! hsts_header_value="$(read_concord_env_value HSTS_HEADER_VALUE)"; then
      echo "  ✗ nginx-reload: could not safely read HSTS_HEADER_VALUE — REFUSING to stage nginx" >&2
      exit 1
    fi
    hsts_header_value="${hsts_header_value:-$HSTS_DEFAULT}"
    if ! validate_hsts_header_value "$hsts_header_value"; then
      echo "  ✗ nginx-reload: HSTS_HEADER_VALUE is malformed — REFUSING to stage nginx" >&2
      exit 1
    fi
    if ! render_nginx_config "$hsts_header_value"; then
      echo "  ✗ nginx-reload: failed to atomically render the nginx HSTS policy — REFUSING to stage nginx" >&2
      exit 1
    fi

    # #1692: render/remove the admin-console vhost from runtime env. The
    # codename host stays out of git; ADMIN_WEBAUTHN_RP_ID in /opt/concord/.env
    # is the deploy-time source, and only ${ADMIN_DOMAIN} is envsubst-expanded.
    if ! admin_enabled="$(read_concord_env_value ADMIN_CONSOLE_ENABLED)" \
       || ! admin_domain="$(read_concord_env_value ADMIN_WEBAUTHN_RP_ID)"; then
      echo "  ✗ nginx-reload: could not safely read admin-console settings — REFUSING to stage nginx" >&2
      exit 1
    fi

    admin_enabled="${admin_enabled:-false}"
    if [[ ! "$admin_enabled" =~ ^(true|false)$ ]]; then
      echo "  ✗ nginx-reload: ADMIN_CONSOLE_ENABLED must be true or false — REFUSING to stage nginx" >&2
      exit 1
    fi

    admin_desired=absent
    if [[ "$admin_enabled" == true ]]; then
      # envsubst is deliberately limited to ADMIN_DOMAIN, but validating the
      # value first is still required: nginx metacharacters must never reach a
      # generated directive. Accept only an ordinary DNS hostname here.
      if [[ -z "$admin_domain" \
         || ! "$admin_domain" =~ ^([A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?\.)+[A-Za-z]{2,}$ ]]; then
        echo "  ✗ nginx-reload: ADMIN_WEBAUTHN_RP_ID must be a valid FQDN — REFUSING to stage nginx" >&2
        exit 1
      fi
      if [[ ! -e "$ADMIN_ORIGIN_CERT" || ! -e "$ADMIN_ORIGIN_KEY" ]]; then
        echo "  ⚠ admin vhost requested but Cloudflare Origin Certificate files are missing:" >&2
        echo "    cert: $ADMIN_ORIGIN_CERT" >&2
        echo "    key:  $ADMIN_ORIGIN_KEY" >&2
        echo "    Keeping admin closed; install the cert/key and rerun deploy to enable it." >&2
        # ponytail: fail closed but keep service deploys green; hard-fail here if admin availability becomes deploy-critical.
      elif [[ ! -f "$ADMIN_TMPL" ]]; then
        echo "  ✗ nginx-reload: admin template not found at $ADMIN_TMPL" >&2
        exit 1
      else
        mkdir -p "$NGINX_RENDER_DIR"
        if ! ADMIN_DOMAIN="$admin_domain" envsubst '${ADMIN_DOMAIN}' < "$ADMIN_TMPL" > "$ADMIN_RENDER" \
           || ! chmod 0644 "$ADMIN_RENDER"; then
          echo "  ✗ nginx-reload: failed to render admin vhost template" >&2
          exit 1
        fi
        admin_desired=present
      fi
    fi

    # Root-owned candidates and hard-link backups live beside their targets.
    # The candidates are inactive until mv(1) atomically replaces each fixed
    # live pathname; hard links preserve the exact previous inode for rollback.
    main_swapped=0
    admin_mutated=0
    admin_was_present=0
    activation_attempted=0
    transaction_pending=0

    cleanup_nginx_candidates() {
      local cleanup_failed=0
      sudo /bin/rm -f -- "$NGINX_CANDIDATE" || cleanup_failed=1
      sudo /bin/rm -f -- "$ADMIN_CANDIDATE" || cleanup_failed=1
      return "$cleanup_failed"
    }

    cleanup_nginx_backups() {
      local cleanup_failed=0
      sudo /bin/rm -f -- "$NGINX_BACKUP" || cleanup_failed=1
      sudo /bin/rm -f -- "$ADMIN_BACKUP" || cleanup_failed=1
      return "$cleanup_failed"
    }

    require_nginx_backup_absent() {
      local backup="$1"
      if sudo /usr/bin/test ! -e "$backup"; then
        return 0
      fi
      if sudo /usr/bin/test -e "$backup"; then
        echo "  ✗ nginx-reload: prior rollback artifact exists at $backup" >&2
        echo "    Refusing to mutate nginx; inspect and restore/remove it as root." >&2
        return 1
      fi
      echo "  ✗ nginx-reload: could not determine rollback-artifact state for $backup" >&2
      return 1
    }

    restore_nginx_transaction() {
      local restore_failed=0
      echo "  Restoring the previous nginx configuration" >&2
      if (( main_swapped )); then
        sudo /usr/bin/mv -fT -- "$NGINX_BACKUP" "$NGINX_DST" || restore_failed=1
      fi
      if (( admin_mutated )); then
        if (( admin_was_present )); then
          sudo /usr/bin/mv -fT -- "$ADMIN_BACKUP" "$ADMIN_DST" || restore_failed=1
        else
          sudo /bin/rm -f -- "$ADMIN_DST" || restore_failed=1
        fi
      fi

      # A successful reload/start can make the rejected transaction active.
      # After restoring the files, load the old configuration back into nginx.
      if (( activation_attempted )) && (( restore_failed == 0 )); then
        if ! sudo /usr/sbin/nginx -t; then
          restore_failed=1
        elif systemctl is-active --quiet nginx; then
          sudo /bin/systemctl reload nginx || restore_failed=1
        else
          sudo /bin/systemctl start nginx || restore_failed=1
        fi
      fi
      if (( restore_failed )); then
        # Candidates are never recovery material, but backups are. Preserve
        # every surviving backup when any restore/re-activation step fails so
        # an operator still has the exact prior inode.
        cleanup_nginx_candidates || true
        echo "  ✗ nginx-reload: automatic rollback was incomplete; operator intervention is required" >&2
        return 1
      fi
      cleanup_nginx_candidates || restore_failed=1
      cleanup_nginx_backups || restore_failed=1
      if (( restore_failed )); then
        echo "  ✗ nginx-reload: rollback succeeded, but transaction cleanup was incomplete" >&2
        return 1
      fi
      echo "  ✓ previous nginx configuration restored" >&2
    }

    rollback_nginx_on_exit() {
      local rc=$?
      trap - EXIT INT TERM
      if (( transaction_pending )); then
        restore_nginx_transaction || true
      fi
      exit "$rc"
    }

    verify_active_hsts() {
      local expected="$1" headers line value
      local count=0
      # HTTP field values use optional whitespace at their boundaries. curl (or
      # nginx) may normalize that OWS, so compare the semantic value rather
      # than requiring the deploy-time spelling to survive byte-for-byte.
      expected="${expected#"${expected%%[![:blank:]]*}"}"
      expected="${expected%"${expected##*[![:blank:]]}"}"
      if ! headers="$(curl --noproxy '*' --silent --show-error --insecure \
          --head --http1.1 --max-time 10 \
          --resolve api.concordvoice.chat:443:127.0.0.1 \
          https://api.concordvoice.chat/nginx-health)"; then
        return 1
      fi
      while IFS= read -r line || [[ -n "$line" ]]; do
        line="${line%$'\r'}"
        if [[ "${line,,}" == strict-transport-security:* ]]; then
          count=$((count + 1))
          value="${line#*:}"
          value="${value#"${value%%[![:blank:]]*}"}"
          value="${value%"${value##*[![:blank:]]}"}"
          [[ "$value" == "$expected" ]] || return 1
        fi
      done <<< "$headers"
      (( count == 1 ))
    }

    # A backup can be the only remaining copy after an interrupted rollback.
    # Never classify it as debris: fail closed before any privileged mutation.
    if ! require_nginx_backup_absent "$NGINX_BACKUP" \
       || ! require_nginx_backup_absent "$ADMIN_BACKUP"; then
      exit 1
    fi

    # Candidates are inactive and never recovery material, so stale candidates
    # may be cleared after both backup paths are proven absent.
    if ! cleanup_nginx_candidates; then
      echo "  ✗ nginx-reload: could not clear stale candidate files" >&2
      exit 1
    fi
    echo "  Staging $NGINX_RENDER → $NGINX_CANDIDATE"
    if ! sudo /usr/bin/install -o root -g root -m 0644 "$NGINX_RENDER" "$NGINX_CANDIDATE"; then
      echo "  ✗ nginx-reload: main candidate staging failed (sudoers entry missing or wrong permissions?)" >&2
      cleanup_nginx_candidates || true
      exit 1
    fi
    if [[ "$admin_desired" == present ]]; then
      echo "  Staging admin vhost $ADMIN_RENDER → $ADMIN_CANDIDATE"
      if ! sudo /usr/bin/install -o root -g root -m 0644 "$ADMIN_RENDER" "$ADMIN_CANDIDATE"; then
        echo "  ✗ nginx-reload: admin candidate staging failed (sudoers entry missing or wrong permissions?)" >&2
        cleanup_nginx_candidates || true
        exit 1
      fi
    fi
    if ! sudo /usr/bin/ln -fT -- "$NGINX_DST" "$NGINX_BACKUP"; then
      echo "  ✗ nginx-reload: could not preserve the current main config" >&2
      cleanup_nginx_candidates || true
      cleanup_nginx_backups || true
      exit 1
    fi
    if sudo /usr/bin/test -e "$ADMIN_DST"; then
      admin_was_present=1
      if ! sudo /usr/bin/ln -fT -- "$ADMIN_DST" "$ADMIN_BACKUP"; then
        echo "  ✗ nginx-reload: could not preserve the current admin config" >&2
        cleanup_nginx_candidates || true
        cleanup_nginx_backups || true
        exit 1
      fi
    elif ! sudo /usr/bin/test ! -e "$ADMIN_DST"; then
      # Distinguish an absent file from sudo/test execution failure. Treating
      # a denied/failed existence check as "absent" could destroy an admin
      # config without first preserving it.
      echo "  ✗ nginx-reload: could not determine current admin config state" >&2
      cleanup_nginx_candidates || true
      cleanup_nginx_backups || true
      exit 1
    fi

    transaction_pending=1
    trap rollback_nginx_on_exit EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM

    # Set each rollback flag before the mutating command so INT/TERM cannot
    # land after a successful syscall but before the transaction records it.
    # Restoring a backup is safe even when the attempted mutation fails.
    main_swapped=1
    if ! sudo /usr/bin/mv -fT -- "$NGINX_CANDIDATE" "$NGINX_DST"; then
      echo "  ✗ nginx-reload: could not atomically activate the main candidate" >&2
      exit 1
    fi
    admin_mutated=1
    if [[ "$admin_desired" == present ]]; then
      if ! sudo /usr/bin/mv -fT -- "$ADMIN_CANDIDATE" "$ADMIN_DST"; then
        echo "  ✗ nginx-reload: could not atomically activate the admin candidate" >&2
        exit 1
      fi
    else
      echo "  Removing disabled admin vhost $ADMIN_DST"
      if ! sudo /bin/rm -f -- "$ADMIN_DST"; then
        echo "  ✗ nginx-reload: admin vhost removal failed" >&2
        exit 1
      fi
    fi

    echo "  Validating with nginx -t"
    if ! sudo /usr/sbin/nginx -t 2>&1; then
      echo "  ✗ nginx-reload: nginx -t rejected the candidate; rolling back" >&2
      exit 1
    fi

    activation_attempted=1
    echo "  Reloading nginx"
    if sudo /bin/systemctl reload nginx; then
      activation_verb=reloaded
    elif systemctl is-active --quiet nginx; then
      echo "  ✗ nginx-reload: systemctl reload nginx failed; rolling back" >&2
      exit 1
    else
      echo "  nginx service is inactive; starting nginx"
      if ! sudo /bin/systemctl start nginx; then
        echo "  ✗ nginx-reload: systemctl start nginx failed; rolling back" >&2
        exit 1
      fi
      activation_verb=started
    fi

    echo "  Probing active HSTS ownership"
    if ! verify_active_hsts "$hsts_header_value"; then
      echo "  ✗ nginx-reload: active /nginx-health did not return exactly one expected HSTS field; rolling back" >&2
      exit 1
    fi

    transaction_pending=0
    trap - EXIT INT TERM
    cleanup_failed=0
    cleanup_nginx_candidates || cleanup_failed=1
    cleanup_nginx_backups || cleanup_failed=1
    if (( cleanup_failed )); then
      echo "  ✗ nginx config is active, but transaction-file cleanup failed" >&2
      echo "    Active configuration was not rolled back; resolve the stale artifact before retrying." >&2
      exit 1
    fi
    echo "  ✓ nginx config staged + validated + $activation_verb + probed"
    ;;

  sudoers-sync)
    # The only delegated capability is one fixed, root-owned helper. It reads a
    # root-owned approved snapshot under /usr/local/share; /opt/concord is never
    # a runtime sudoers input. Policy changes require explicit operator action.
    if ! sudo -n /usr/local/sbin/concord-sudoers-sync; then
      echo "  ✗ sudoers-sync: root-owned sync helper is unavailable or failed" >&2
      echo "    Install from a reviewed root-owned staging directory; see:" >&2
      echo "      [internal]deploys.md#sudoers-boundary-bootstrap-and-updates" >&2
      exit 1
    fi
    echo "  ✓ sudoers-sync: root-approved policy is active"
    ;;

  security-event-logging)
    if ! sudo -n /usr/local/sbin/concord-security-event-logging; then
      echo "  ✗ security-event-logging: root-owned preparation helper is unavailable or failed" >&2
      echo "    Bootstrap from a reviewed root-owned staging directory before rebuilding; see:" >&2
      echo "      [internal]deploys.md#sudoers-boundary-bootstrap-and-updates" >&2
      exit 1
    fi
    echo "  ✓ security-event-logging: dedicated application log boundary is prepared"
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
    # Single assignment so the blue/green colour flip (#3108) has ONE line to
    # make colour-aware rather than one per probe site.
    CP_PROBE_BASE="http://localhost:8080"
    cp_ok=0; mp_ok=0; coturn_ok=0
    while [[ $(date +%s) -lt $DEADLINE ]]; do
      cp_ok=0; mp_ok=0; coturn_ok=0
      # Gates on READINESS, not liveness (#3106). wait-healthy is the real
      # answer to "did this deploy succeed", and a control-plane that bound the
      # port but cannot reach Redis returns 401 on every authenticated request
      # while /health answers 200 throughout — a failed deploy this previously
      # called green. The compose healthcheck keeps probing /health, because
      # that bit is #3107's restart predicate; see
      # [internal]tests/test-compose-health-contract.sh.
      curl -sf "${CP_PROBE_BASE}/readyz" >/dev/null 2>&1 && cp_ok=1
      curl -sf "http://localhost:3000/health" >/dev/null 2>&1 && mp_ok=1
      nc -z -w2 localhost 3478 >/dev/null 2>&1 && coturn_ok=1
      if [[ $cp_ok -eq 1 && $mp_ok -eq 1 && $coturn_ok -eq 1 ]]; then
        echo "  ✓ all services healthy (control-plane + media-plane + coturn)"
        exit 0
      fi
      sleep "$INTERVAL"
    done
    echo "  ✗ wait-healthy timed out after ${TIMEOUT}s" >&2
    if [[ $cp_ok -eq 0 ]]; then
      echo "    control-plane (:8080/readyz) failed" >&2
      # Liveness on the DIAGNOSTIC path only: it distinguishes "the process is
      # dead or not listening" from "the process is listening but not ready",
      # which is a materially better first line for whoever reads this.
      if curl -sf "${CP_PROBE_BASE}/health" >/dev/null 2>&1; then
        echo "    ...but :8080/health answers, so the process is UP and NOT READY." >&2
        # The body names which dependency is down, so a failed deploy is
        # diagnosable from the workflow log without logging into the host. It
        # carries dependency names and up/down only — never values.
        # Capture BEFORE printing: piping straight to head meant a failed or
        # timed-out fetch printed the label followed by a blank line, which is
        # indistinguishable from the endpoint returning an empty body -- a
        # diagnostic silent about its own failure.
        # `|| true` is load-bearing, not defensive. The script runs under
        # `set -euo pipefail` (line 4), and errexit DOES fire on a failing
        # command substitution in an ASSIGNMENT (it does not in an argument --
        # the same asymmetry [internal]rules/deployment.md documents for the
        # Compose dotenv writer). So a curl that fails or times out, or a head
        # that closes early on a >512B body, would abort the whole script here
        # -- turning a diagnostic into a fatal, and skipping the very branches
        # below that exist to explain the failure.
        readyz_body="$(curl -s --max-time 2 "${CP_PROBE_BASE}/readyz" 2>/dev/null | head -c 512 || true)"
        if [[ -n "$readyz_body" ]]; then
          echo "    /readyz reported: ${readyz_body}" >&2
        else
          echo "    /readyz returned no body within 2s (fetch failed or timed out)." >&2
        fi
      else
        echo "    ...and :8080/health does not answer either, so the process is DOWN." >&2
      fi
    fi
    [[ $mp_ok -eq 0 ]] && echo "    media-plane (:3000/health) failed" >&2
    [[ $coturn_ok -eq 0 ]] && echo "    coturn STUN (:3478) failed" >&2
    dump_health_diagnostics
    exit 1
    ;;

  freshstart)
    # skip-receipt: the receipt never authorizes freshstart's volume wipe — a
    # gate-true freshstart still requires live one-container proof (#2326).
    activity_history_routine_guard skip-receipt
    echo "=== Fresh Start: wiping all data and rebuilding ==="
    echo "This will DELETE all databases, caches, and user data."
    read -rp "Are you sure? (y/N) " confirm
    if [[ "${confirm,,}" != "y" ]]; then
      echo "Aborted."
      exit 0
    fi
    # ACQUIRED HERE, not in the pre-dispatch block, and the difference is an
    # unbounded hold. `flock -w` bounds how long acquisition WAITS; once taken,
    # the descriptor-form lock is held by this shell until it exits. Acquiring
    # before the prompt above meant an operator who paused or walked away from
    # the confirmation left healthwatch deferring every cycle and every other
    # deploy verb timing out at 90s, indefinitely, with no indication why.
    # After the confirmation and before the first mutation is the only correct
    # place: freshstart still holds the interlock for everything destructive.
    DEPLOY_LOCK_WAIT_SECS=90
    acquire_deploy_transaction_lock "$@"

    clear_activity_history_image_selector
    activity_history_invalidate_activation_receipt
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
    echo "  rebuild [SERVICE...]"
    echo "                 Force rebuild + restart (no auto-sync since #1015). With no"
    echo "                 arguments, rebuilds every service exactly as before. With a"
    echo "                 service list, rebuilds only those with --no-deps --wait, and"
    echo "                 escalates to a full rebuild if any named service is not running"
    echo "                 Clears a persisted Activity History rollback image pin only when"
    echo "                 unscoped or when control-plane is in scope"
    echo "  activity-history-routine-guard"
    echo "                 Verify canonical/rendered config and safe routine-deploy state"
    echo "  activity-history-activate --confirm-drained"
    echo "                 First activation: require zero CPs, preflight, CP-only start + health"
    echo "  activity-history-rollback --confirm-drained --old-image <immutable-image-ref> [--downgrade-schema]"
    echo "                 Drain/disable, optionally downgrade, and start one pinned old CP image"
    echo "  validate-config  §F: pre-rebuild guard via 'docker compose config --quiet'"
    echo "  attachment-probe  Validate the armed attachment backend without pulling images"
    echo "  attachment-probe-cron-install  Idempotently install the nightly attachment probe"
    echo "  nginx-reload   Validate/render HSTS, atomically swap main/admin nginx files,"
    echo "                 nginx -t, reload/start, active-probe, and rollback on failure"
    echo "  wait-healthy   Block until all services healthy (--timeout SECONDS, default 60)"
    echo "  freshstart     Wipe all data, clear rollback image pin, rebuild from scratch"
    echo "  sync-releases  Manual operator-recovery — normal sync happens via GitHub Actions sync-releases.yml"
    echo "  verify-registration  Post-deploy smoke test for registration flow"
    echo "  docker-clean   Prune unused images + builder cache (weekly cron target)"
    echo "                 Add --aggressive to drop 7-day retention + prune stopped containers"
    echo "  stop           Stop all services"
    ;;
esac
