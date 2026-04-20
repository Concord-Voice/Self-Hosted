#!/usr/bin/env bash
# Concord service management helper
# Usage: ./concord-ctl.sh [status|logs|restart|rebuild|freshstart|sync-releases|verify-registration|docker-clean|branch|stop]
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="${SCRIPT_DIR}/../.."

# Load config
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
    echo "Branch: $(grep '^DEPLOY_BRANCH=' "${SCRIPT_DIR}/deploy.env" | cut -d= -f2)"
    echo "Commit: $(cd "$DEPLOY_DIR" && git rev-parse --short HEAD) ($(cd "$DEPLOY_DIR" && git log -1 --format='%s'))"
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

  branch)
    echo "Deploy branch: $(grep '^DEPLOY_BRANCH=' "${SCRIPT_DIR}/deploy.env" | cut -d= -f2)"
    echo "Git branch:    $(cd "$DEPLOY_DIR" && git rev-parse --abbrev-ref HEAD)"
    echo "Git commit:    $(cd "$DEPLOY_DIR" && git rev-parse --short HEAD)"
    echo ""
    echo "To switch: ./switch-branch.sh <branch-name>"
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
    echo "  status         Show running containers, health checks, current branch"
    echo "  logs           Tail service logs (pass service name to filter, e.g. 'logs control-plane')"
    echo "  restart        Sync releases + restart all services"
    echo "  rebuild        Sync releases + force rebuild + restart all services"
    echo "  freshstart     Wipe all data (DB, Redis), sync releases, rebuild from scratch"
    echo "  sync-releases  Download latest desktop release assets from GitHub"
    echo "  verify-registration  Post-deploy smoke test for registration flow"
    echo "  docker-clean   Prune unused images + builder cache (weekly cron target)"
    echo "                 Add --aggressive to drop 7-day retention + prune stopped containers"
    echo "  branch         Show current deploy branch and git info"
    echo "  stop           Stop all services"
    ;;
esac
