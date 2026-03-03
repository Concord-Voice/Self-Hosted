#!/usr/bin/env bash
# Concord service management helper
# Usage: ./concord-ctl.sh [status|logs|restart|rebuild|freshstart|branch|stop]
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

COMPOSE_CMD="docker compose -f ${DEPLOY_DIR}/docker-compose.yml -f ${DEPLOY_DIR}/docker-compose.staging.yml --profile services"

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
    ;;

  logs)
    shift || true
    cd "$DEPLOY_DIR" && $COMPOSE_CMD logs -f --tail=100 "$@"
    ;;

  restart)
    echo "Restarting services..."
    cd "$DEPLOY_DIR" && $COMPOSE_CMD restart
    echo "Done."
    ;;

  rebuild)
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
    echo "Stopping all services..."
    cd "$DEPLOY_DIR" && $COMPOSE_CMD down
    echo ""
    echo "Removing data volumes..."
    docker volume rm concordvoice-postgres-data concordvoice-redis-data 2>/dev/null || true
    echo ""
    echo "Rebuilding and starting fresh..."
    cd "$DEPLOY_DIR" && $COMPOSE_CMD up -d --build
    echo ""
    echo "Fresh start complete. All data has been wiped."
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
    echo "  status      Show running containers, health checks, current branch"
    echo "  logs        Tail service logs (pass service name to filter, e.g. 'logs control-plane')"
    echo "  restart     Restart all services"
    echo "  rebuild     Force rebuild + restart all services"
    echo "  freshstart  Wipe all data (DB, Redis) and rebuild from scratch"
    echo "  branch      Show current deploy branch and git info"
    echo "  stop        Stop all services"
    ;;
esac
