#!/usr/bin/env bash
# Concord service management helper
# Usage: ./concord-ctl.sh [status|logs|restart|rebuild|freshstart|sync-releases|verify-registration|docker-clean|stop]
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="${SCRIPT_DIR}/../.."

# Load runtime config. deploy.env (committed defaults like COMPOSE_OVERRIDE)
# is read first, then deploy.env.local (machine-specific overrides like
# VM_IP, DEPLOY_BRANCH) layers on top.
if [[ -f "${SCRIPT_DIR}/deploy.env" ]]; then
  source "${SCRIPT_DIR}/deploy.env"
fi
if [[ -f "${SCRIPT_DIR}/deploy.env.local" ]]; then
  source "${SCRIPT_DIR}/deploy.env.local"
fi

DEPLOY_REPO="${DEPLOY_REPO:-https://github.com/markdrogersjr/Concord.git}"
COMPOSE_OVERRIDE="${COMPOSE_OVERRIDE:-docker-compose.staging.yml}"
COMPOSE_CMD="docker compose -f ${DEPLOY_DIR}/docker-compose.yml -f ${DEPLOY_DIR}/${COMPOSE_OVERRIDE} --profile ${COMPOSE_PROFILES:-services}"

# Shared release sync logic
# shellcheck source=sync-releases.sh
source "${SCRIPT_DIR}/sync-releases.sh"

case "${1:-help}" in
  status)
    echo "=== Concord Service Status ==="
    # NOTE: Branch + commit info no longer surfaced — server has no .git/ post-#848.
    # See `gh api repos/markdrogersjr/Concord/deployments` for deploy history.
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
    echo "Syncing release assets..."
    sync_releases
    echo "Restarting services..."
    cd "$DEPLOY_DIR" && $COMPOSE_CMD restart
    echo "Done."
    ;;

  rebuild)
    echo "Syncing release assets..."
    sync_releases
    echo "Rebuilding and restarting..."
    cd "$DEPLOY_DIR" && $COMPOSE_CMD up -d --build
    echo "Done."
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
    exit 1
    ;;

  freshstart)
    echo "=== Fresh Start: wiping all data and rebuilding ==="
    echo "This will DELETE all databases, caches, and user data."
    read -rp "Are you sure? (y/N) " confirm
    if [[ "${confirm,,}" != "y" ]]; then
      echo "Aborted."
      exit 0
    fi
    echo ""
    echo "Stopping all services and removing volumes..."
    cd "$DEPLOY_DIR" && $COMPOSE_CMD down -v
    echo ""
    echo "Syncing release assets..."
    sync_releases
    echo ""
    echo "Rebuilding and starting fresh..."
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
    echo "  restart        Sync releases + restart all services"
    echo "  rebuild        Sync releases + force rebuild + restart all services"
    echo "  validate-config  §F: pre-rebuild guard via 'docker compose config --quiet'"
    echo "  wait-healthy   Block until all services healthy (--timeout SECONDS, default 60)"
    echo "  freshstart     Wipe all data (DB, Redis), sync releases, rebuild from scratch"
    echo "  sync-releases  Download latest desktop release assets from GitHub"
    echo "  verify-registration  Post-deploy smoke test for registration flow"
    echo "  docker-clean   Prune unused images + builder cache (weekly cron target)"
    echo "                 Add --aggressive to drop 7-day retention + prune stopped containers"
    echo "  stop           Stop all services"
    ;;
esac
