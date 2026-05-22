#!/usr/bin/env bash
# Concord Voice — Unified Dev Lifecycle CLI
#
# Mirrors infrastructure/deploy/concord-ctl.sh verb grammar where possible.
# This file is laptop-only — NEVER copied to /opt/concord on production servers.
#
# Usage: ./scripts/concord-dev.sh <verb> [options]
# Run with no args or `help` for the verb list.
#
# Exit codes:
#   0  success
#   1  generic / runtime error
#   2  pre-flight failure (port-in-use, missing .env, docker not running)
#   3  service startup failure (control-plane or media-plane /health timeout)

set -euo pipefail

# ── Constants ──────────────────────────────────────────────────────────

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
# LOG_DIR is overridable via env var so tests can sandbox log file ops to a
# tmpdir without polluting the real $PROJECT_ROOT/logs/. Production callers
# get the default $PROJECT_ROOT/logs behavior unchanged.
: "${LOG_DIR:=$PROJECT_ROOT/logs}"
readonly LOG_DIR

readonly RED='\033[0;31m'
readonly GREEN='\033[0;32m'
readonly YELLOW='\033[1;33m'
readonly BLUE='\033[0;34m'
readonly NC='\033[0m'

# Compose layering: base + dev overlay. Mirrors prod's base + production.yml pattern.
COMPOSE_FILES=(
  "-f" "$PROJECT_ROOT/docker-compose.yml"
  "-f" "$PROJECT_ROOT/docker-compose.dev.yml"
)

# Global flags (set by parse_global_flags before verb dispatch).
VERBOSE=0
QUIET=0

# ── Verb dispatch ──────────────────────────────────────────────────────

usage() {
  cat <<'USAGE'
Concord Voice — Dev Lifecycle CLI

Usage: ./scripts/concord-dev.sh <verb> [options]

Verbs:
  up [--skip-client] [--no-rebuild] [--clients N] [--reset-db]
                      Start all infra + control-plane + media-plane + electron clients
  down [--keep-docker] [--force]
                      Stop natives and (optionally) docker containers
  status              Show containers, health, PIDs, log sizes
  logs [<service>] [-n N] [--no-follow] [--errors] [--clear]
                      Tail logs (services: all, control-plane, media-plane, vite, client [N], docker [svc])
  restart             Stop + start without rebuild
  rebuild             Stop + start with rebuild (go build + npm install)
  freshstart          Wipe all data (DB, Redis, client data) and start fresh
  validate-config     Validate base + dev compose overlay

Global options (apply to any verb):
  --verbose           Echo docker/build commands to stderr
  --quiet             Suppress non-error output

Run with no verb for this help.
USAGE
}

main() {
  parse_global_flags "$@"
  local verb="${POSITIONAL[0]:-help}"
  set -- "${POSITIONAL[@]:1}"  # shift past verb

  case "$verb" in
    up)              verb_up "$@" ;;
    down|stop)       verb_down "$@" ;;
    status)          verb_status "$@" ;;
    logs)            verb_logs "$@" ;;
    restart)         verb_restart "$@" ;;
    rebuild)         verb_rebuild "$@" ;;
    freshstart)      verb_freshstart "$@" ;;
    validate-config) verb_validate_config "$@" ;;
    help|-h|--help)  usage ;;
    *)
      echo "Unknown verb: $verb" >&2
      usage >&2
      exit 1
      ;;
  esac
}

parse_global_flags() {
  POSITIONAL=()
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --verbose) VERBOSE=1; shift ;;
      --quiet)   QUIET=1; shift ;;
      *)         POSITIONAL+=("$1"); shift ;;
    esac
  done
}

# ── Helpers ────────────────────────────────────────────────────────────

# compose() — invoke docker compose with the standard file flags.
# Usage: compose ps
#        compose up -d postgres redis
#        compose logs control-plane
compose() {
  if [[ "$VERBOSE" -eq 1 ]]; then
    echo "+ docker compose ${COMPOSE_FILES[*]} $*" >&2
  fi
  docker compose "${COMPOSE_FILES[@]}" "$@"
}

# port_in_use() — return 0 if port is bound on localhost.
# Usage: if port_in_use 8080; then echo "8080 busy"; fi
port_in_use() {
  lsof -i ":$1" >/dev/null 2>&1
}

# wait_for_service() — block until URL returns HTTP 200, or timeout.
# Usage: wait_for_service "http://localhost:8080/health" "Control Plane"
# Returns: 0 if ready, 1 if timeout.
wait_for_service() {
  local url="$1"
  local name="$2"
  local max_attempts="${3:-30}"
  local attempt=1

  if [[ "$QUIET" -eq 0 ]]; then
    echo -ne "${YELLOW}⏳ Waiting for $name...${NC}"
  fi

  while [[ $attempt -le $max_attempts ]]; do
    if curl -sf "$url" >/dev/null 2>&1; then
      [[ "$QUIET" -eq 0 ]] && echo -e "\r${GREEN}✓${NC} $name is ready                    "
      return 0
    fi
    [[ "$QUIET" -eq 0 ]] && echo -ne "\r${YELLOW}⏳ Waiting for $name... ($attempt/$max_attempts)${NC}"
    sleep 1
    # Use $((var + 1)) form, NOT ((var++)) — post-increment returns the OLD
    # value as its expression result, so when var == 0 the return is 0 which
    # `set -e` treats as failure and exits the script.
    attempt=$((attempt + 1))
  done

  [[ "$QUIET" -eq 0 ]] && echo -e "\r${RED}✗${NC} $name failed to start (timeout)      "
  return 1
}

# read_valid_pid() — read + validate the PID from a PID file. Returns 0
# and prints the PID on stdout when valid (numeric, > 1). Returns 1 and
# prints nothing when invalid. Defends against corrupt PID files that
# could cause `kill` to target every user process (kill -1) or `ps -p`
# to produce confusing output.
# Usage: pid=$(read_valid_pid logs/control-plane.pid) || handle-invalid
read_valid_pid() {
  local pid_file="$1"
  [[ -f "$pid_file" ]] || return 1
  local pid
  pid=$(cat "$pid_file" 2>/dev/null)
  if [[ "$pid" =~ ^[0-9]+$ ]] && [[ "$pid" -gt 1 ]]; then
    printf '%s' "$pid"
    return 0
  fi
  return 1
}

# stop_process() — kill PID from file, escalate TERM → KILL if needed.
# Usage: stop_process logs/control-plane.pid "Control Plane"
#        stop_process logs/control-plane.pid "Control Plane" --force
stop_process() {
  local pid_file="$1"
  local name="$2"
  local force_kill=0
  [[ "${3:-}" == "--force" ]] && force_kill=1

  if [[ ! -f "$pid_file" ]]; then
    [[ "$QUIET" -eq 0 ]] && echo -e "${YELLOW}⚠${NC} $name PID file not found"
    return 0
  fi

  local pid
  if ! pid=$(read_valid_pid "$pid_file"); then
    [[ "$QUIET" -eq 0 ]] && echo -e "${YELLOW}⚠${NC} $name PID file contains invalid value — removing"
    rm -f "$pid_file"
    return 0
  fi
  if ! ps -p "$pid" >/dev/null 2>&1; then
    [[ "$QUIET" -eq 0 ]] && echo -e "${YELLOW}⚠${NC} $name was not running (stale PID file)"
    rm -f "$pid_file"
    return 0
  fi

  [[ "$QUIET" -eq 0 ]] && echo -ne "${YELLOW}⏳ Stopping $name (PID: $pid)...${NC}"
  if [[ "$force_kill" -eq 1 ]]; then
    kill -9 "$pid" 2>/dev/null || true
  else
    kill -TERM "$pid" 2>/dev/null || true
    local count=0
    while ps -p "$pid" >/dev/null 2>&1 && [[ $count -lt 10 ]]; do
      sleep 1
      count=$((count + 1))
    done
    if ps -p "$pid" >/dev/null 2>&1; then
      [[ "$QUIET" -eq 0 ]] && echo -e "\r${YELLOW}⚠${NC} $name didn't stop gracefully, force killing..."
      kill -9 "$pid" 2>/dev/null || true
    fi
  fi
  [[ "$QUIET" -eq 0 ]] && echo -e "\r${GREEN}✓${NC} $name stopped                              "
  rm -f "$pid_file"
}

# pre_flight_checks() — verify environment before starting services.
# Exits with code 2 on failure (pre-flight class).
pre_flight_checks() {
  local errors=0

  # Check docker daemon
  if ! docker info >/dev/null 2>&1; then
    echo -e "${RED}✗${NC} Docker daemon not running. Start Docker Desktop." >&2
    errors=$((errors + 1))
  fi

  # Check ports
  for port in 8080 3000; do
    if port_in_use "$port"; then
      echo -e "${RED}✗${NC} Port $port already in use. Run 'concord-dev.sh down' first." >&2
      errors=$((errors + 1))
    fi
  done

  # Check .env or .env.example
  if [[ ! -f "$PROJECT_ROOT/.env" ]]; then
    if [[ ! -f "$PROJECT_ROOT/.env.example" ]]; then
      echo -e "${RED}✗${NC} No .env or .env.example found. Cannot bootstrap." >&2
      errors=$((errors + 1))
    fi
  fi

  if [[ $errors -gt 0 ]]; then
    exit 2
  fi
}

# bootstrap_env() — copy .env.example → .env if .env missing.
bootstrap_env() {
  if [[ ! -f "$PROJECT_ROOT/.env" && -f "$PROJECT_ROOT/.env.example" ]]; then
    cp "$PROJECT_ROOT/.env.example" "$PROJECT_ROOT/.env"
    chmod 600 "$PROJECT_ROOT/.env"
    [[ "$QUIET" -eq 0 ]] && echo -e "${GREEN}✓${NC} Created .env from .env.example"
  fi
}

# ── Verbs ──────────────────────────────────────────────────────────────

verb_up() {
  local skip_client=0
  local no_rebuild=0
  local num_clients=1
  local reset_db=0

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --skip-client) skip_client=1; shift ;;
      --no-rebuild)  no_rebuild=1; shift ;;
      --clients)
        if [[ $# -lt 2 ]]; then
          echo "verb_up: --clients requires a number" >&2
          return 2
        fi
        if ! [[ "$2" =~ ^[0-9]+$ ]]; then
          echo "verb_up: --clients must be a positive integer, got '$2'" >&2
          return 2
        fi
        num_clients="$2"; shift 2 ;;
      --reset-db)    reset_db=1; shift ;;
      *)
        echo "verb_up: unknown arg '$1'" >&2
        return 2
        ;;
    esac
  done

  pre_flight_checks
  bootstrap_env
  mkdir -p "$LOG_DIR"

  _up_infra "$reset_db"
  _up_control_plane "$no_rebuild"
  _up_media_plane "$no_rebuild"
  if [[ "$skip_client" -eq 0 ]]; then
    _up_clients "$no_rebuild" "$num_clients"
  fi
  _up_summary "$skip_client" "$num_clients"
}

_up_infra() {
  local reset_db="$1"

  [[ "$QUIET" -eq 0 ]] && echo -e "${BLUE}📦 Starting infrastructure (5 containers via base + dev overlay)${NC}"

  if [[ "$reset_db" -eq 1 ]]; then
    [[ "$QUIET" -eq 0 ]] && echo -e "${YELLOW}🗑  Resetting DB volumes (--reset-db)${NC}"
    compose down -v >/dev/null 2>&1 || true
    rm -rf "$PROJECT_ROOT/.dev-data" 2>/dev/null || true
  fi

  # Default compose profile = 5 infra services (postgres redis nats coturn minio).
  compose up -d >/dev/null

  # Wait up to 30s for all healthchecks to flip green.
  # Base compose has 4 healthcheck-bearing services (postgres/redis/nats/coturn);
  # MinIO has no healthcheck because the base image lacks curl/wget/mc (see
  # docker-compose.yml:128). MinIO readiness is verified by control-plane's own
  # 15s MinIO-client init timeout at startup. So we wait for >= 4 healthy here.
  local waited=0
  local healthy=0
  while [[ $waited -lt 30 ]]; do
    healthy=$(compose ps --format json 2>/dev/null | grep -c '"Health":"healthy"' || true)
    if [[ $healthy -ge 4 ]]; then
      [[ "$QUIET" -eq 0 ]] && echo -e "${GREEN}✓${NC} All infrastructure healthchecks green (4 services + minio)"
      return 0
    fi
    sleep 1
    waited=$((waited + 1))
  done

  echo -e "${RED}✗${NC} Infrastructure healthcheck timeout. Run '$0 status' to investigate." >&2
  exit 3
}

_up_control_plane() {
  local no_rebuild="$1"

  [[ "$QUIET" -eq 0 ]] && echo -e "${BLUE}🎛  Starting Control Plane (Go)${NC}"

  cd "$PROJECT_ROOT/services/control-plane"
  if [[ "$no_rebuild" -eq 0 || ! -f bin/control-plane ]]; then
    [[ "$QUIET" -eq 0 ]] && echo -e "${YELLOW}🔧 Building control-plane binary${NC}"
    go mod download
    mkdir -p bin
    go build -o bin/control-plane cmd/server/main.go
  fi

  nohup ./bin/control-plane > "$LOG_DIR/control-plane.log" 2>&1 &
  echo $! > "$LOG_DIR/control-plane.pid"
  cd "$PROJECT_ROOT"

  if ! wait_for_service "http://localhost:8080/health" "Control Plane"; then
    echo -e "${RED}✗${NC} Control Plane startup failed. Last 20 lines of log:" >&2
    tail -20 "$LOG_DIR/control-plane.log" >&2
    exit 3
  fi
}

_up_media_plane() {
  local no_rebuild="$1"

  [[ "$QUIET" -eq 0 ]] && echo -e "${BLUE}🎙  Starting Media Plane (Node.js)${NC}"

  cd "$PROJECT_ROOT/services/media-plane"
  # Install if --no-rebuild was NOT passed, OR if node_modules is missing
  # (fresh-checkout case — CI smoke + first-time developer).
  if [[ "$no_rebuild" -eq 0 || ! -d node_modules ]]; then
    [[ "$QUIET" -eq 0 ]] && echo -e "${YELLOW}🔧 npm install (media-plane, two-pass)${NC}"
    # Two-pass install defeats the mediasoup-postinstall ordering race
    # documented in `[internal]rules/media-plane.md` "Docker Build Context
    # Invariant" section.
    #
    # On a fresh checkout with no `node_modules/`, the standard `npm install`
    # runs postinstall scripts concurrently with dependency installation, so
    # mediasoup's postinstall (`node npm-scripts.mjs postinstall`) can fire
    # BEFORE npm has wired the `node-domexception` file:./stubs override
    # symlink. mediasoup's hook then imports something that pulls fetch-blob,
    # which imports node-domexception, and ERR_MODULE_NOT_FOUND aborts the
    # whole install. Locally-warm caches mask this because the symlink
    # persists across runs.
    #
    # Pass 1 (`--ignore-scripts`): resolve all deps + create override
    # symlinks, defer ALL lifecycle scripts.
    # Pass 2 (`npm rebuild`): re-run install hooks in a context where every
    # override target is already in place.
    #
    # Cost: ~3-5s extra on fresh `rm -rf node_modules`; zero cost on warm
    # cache (both passes short-circuit when deps + symlinks are current).
    npm install --ignore-scripts >/dev/null
    npm rebuild >/dev/null
  fi

  # Use npx tsx directly (not via npm run) so the PID file tracks tsx, not npm.
  nohup npx tsx watch src/index.ts > "$LOG_DIR/media-plane.log" 2>&1 &
  echo $! > "$LOG_DIR/media-plane.pid"
  cd "$PROJECT_ROOT"

  if ! wait_for_service "http://localhost:3000/health" "Media Plane"; then
    echo -e "${RED}✗${NC} Media Plane startup failed. Last 20 lines of log:" >&2
    tail -20 "$LOG_DIR/media-plane.log" >&2
    exit 3
  fi
}

_up_clients() {
  local no_rebuild="$1"
  local num_clients="$2"

  [[ "$QUIET" -eq 0 ]] && echo -e "${BLUE}🖥  Starting Vite dev server + ${num_clients} Electron client(s)${NC}"

  cd "$PROJECT_ROOT/client/desktop"
  # See _up_media_plane note — install when missing node_modules even with --no-rebuild.
  if [[ "$no_rebuild" -eq 0 || ! -d node_modules ]]; then
    [[ "$QUIET" -eq 0 ]] && echo -e "${YELLOW}🔧 npm install (client/desktop)${NC}"
    npm install >/dev/null
  fi

  # Always rebuild preload + main (Electron requires fresh JS).
  npm run build:preload >/dev/null
  npx tsc -p tsconfig.main.json >/dev/null

  nohup npx vite --port 3001 --strictPort > "$LOG_DIR/vite-renderer.log" 2>&1 &
  echo $! > "$LOG_DIR/vite-renderer.pid"

  if ! wait_for_service "http://localhost:3001" "Vite Dev Server"; then
    echo -e "${RED}✗${NC} Vite startup failed" >&2
    exit 3
  fi

  for i in $(seq 1 "$num_clients"); do
    local user_data="$PROJECT_ROOT/.dev-data/client-$i"
    mkdir -p "$user_data"
    NODE_ENV=development nohup npx electron . --user-data-dir="$user_data" \
      > "$LOG_DIR/desktop-client-$i.log" 2>&1 &
    echo $! > "$LOG_DIR/desktop-client-$i.pid"
    [[ "$QUIET" -eq 0 ]] && echo -e "  ${GREEN}→${NC} Client $i: PID $! (data: $user_data)"
    sleep 1
  done

  cd "$PROJECT_ROOT"
}

_up_summary() {
  local skip_client="$1"
  local num_clients="$2"

  [[ "$QUIET" -eq 1 ]] && return 0

  echo ""
  echo -e "${GREEN}✅ Concord Voice dev environment is up${NC}"
  echo ""
  echo -e "${BLUE}Services:${NC}"
  echo -e "  ${GREEN}✓${NC} PostgreSQL    → localhost:5432"
  echo -e "  ${GREEN}✓${NC} Redis         → localhost:6379"
  echo -e "  ${GREEN}✓${NC} NATS          → localhost:4222"
  echo -e "  ${GREEN}✓${NC} coturn        → localhost:3478 (STUN/TURN)"
  echo -e "  ${GREEN}✓${NC} MinIO         → localhost:9000 (S3) / localhost:9001 (console)"
  echo -e "  ${GREEN}✓${NC} Control Plane → http://localhost:8080"
  echo -e "  ${GREEN}✓${NC} Media Plane   → http://localhost:3000"
  if [[ "$skip_client" -eq 0 ]]; then
    echo -e "  ${GREEN}✓${NC} Vite Renderer → http://localhost:3001"
    echo -e "  ${GREEN}✓${NC} Desktop Clients × $num_clients"
  fi
  echo ""
  echo -e "${BLUE}Quick:${NC}"
  echo -e "  Status:   ${YELLOW}$0 status${NC}"
  echo -e "  Logs:     ${YELLOW}$0 logs control-plane${NC}"
  echo -e "  Stop:     ${YELLOW}$0 down${NC}"
}
verb_down() {
  local keep_docker=0
  local force_flag=""

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --keep-docker) keep_docker=1; shift ;;
      --force)       force_flag="--force"; shift ;;
      *)
        echo "verb_down: unknown arg '$1'" >&2
        return 2
        ;;
    esac
  done

  mkdir -p "$LOG_DIR"

  [[ "$QUIET" -eq 0 ]] && echo -e "${BLUE}🖥  Stopping desktop clients${NC}"
  for pid_file in "$LOG_DIR"/desktop-client-*.pid; do
    [[ -f "$pid_file" ]] || continue
    local num
    # Extract the index from the basename, not the full path. The path may
    # contain digits (e.g., worktree dir 'inspiring-mestorf-6dc965/'); the
    # old `grep -oE '[0-9]+' | head -1` would pick the first digit run anywhere
    # in the path. Strip everything up to and including 'desktop-client-' then
    # the '.pid' suffix to get the bare numeric index.
    local base="${pid_file##*/desktop-client-}"
    num="${base%.pid}"
    stop_process "$pid_file" "Desktop Client $num" $force_flag
  done
  stop_process "$LOG_DIR/vite-renderer.pid" "Vite Dev Server" $force_flag

  [[ "$QUIET" -eq 0 ]] && echo -e "${BLUE}🎙  Stopping media-plane${NC}"
  stop_process "$LOG_DIR/media-plane.pid" "Media Plane" $force_flag
  pkill -f "tsx watch src/index.ts" 2>/dev/null || true

  [[ "$QUIET" -eq 0 ]] && echo -e "${BLUE}🎛  Stopping control-plane${NC}"
  stop_process "$LOG_DIR/control-plane.pid" "Control Plane" $force_flag

  if [[ "$keep_docker" -eq 0 ]]; then
    [[ "$QUIET" -eq 0 ]] && echo -e "${BLUE}📦 Stopping infrastructure containers${NC}"
    compose down >/dev/null 2>&1 || true
    [[ "$QUIET" -eq 0 ]] && echo -e "${GREEN}✓${NC} Docker containers stopped"
  else
    [[ "$QUIET" -eq 0 ]] && echo -e "${BLUE}📦 Keeping Docker containers (--keep-docker)${NC}"
  fi

  # Verify ports are free.
  local ports_busy=0
  for port in 8080 3000 3001 3478; do
    if port_in_use "$port"; then
      echo -e "${RED}✗${NC} Port $port still in use" >&2
      ports_busy=1
    fi
  done

  if [[ $ports_busy -eq 1 ]]; then
    [[ "$QUIET" -eq 0 ]] && echo -e "${YELLOW}Try: $0 down --force${NC}"
  else
    [[ "$QUIET" -eq 0 ]] && echo -e "${GREEN}✅ All services stopped${NC}"
  fi
}
verb_status() {
  [[ $# -gt 0 ]] && { echo "verb_status: takes no args, got '$1'" >&2; return 2; }

  echo "=== Concord Voice Dev — Status ==="
  echo ""
  echo "--- Containers ---"
  compose ps 2>/dev/null || echo "(compose ps failed — Docker may not be running)"
  echo ""
  echo "--- Native processes ---"
  for svc in control-plane media-plane vite-renderer; do
    local pid_file="$LOG_DIR/$svc.pid"
    local pid
    if pid=$(read_valid_pid "$pid_file") && ps -p "$pid" >/dev/null 2>&1; then
      printf "  ✓ %-18s PID %s\n" "$svc" "$pid"
    elif [[ -f "$pid_file" ]]; then
      printf "  ✗ %-18s stale or invalid PID file\n" "$svc"
    else
      printf "  ✗ %-18s not running\n" "$svc"
    fi
  done
  for pid_file in "$LOG_DIR"/desktop-client-*.pid; do
    [[ -f "$pid_file" ]] || continue
    # Extract index from basename, not full path (worktree dirs have digits).
    local base="${pid_file##*/desktop-client-}"
    local num="${base%.pid}"
    local pid
    if pid=$(read_valid_pid "$pid_file") && ps -p "$pid" >/dev/null 2>&1; then
      printf "  ✓ %-18s PID %s\n" "desktop-client-$num" "$pid"
    else
      printf "  ✗ %-18s stale or invalid PID file\n" "desktop-client-$num"
    fi
  done

  echo ""
  echo "--- Health checks ---"
  for svc in "Control Plane:8080" "Media Plane:3000"; do
    local name="${svc%%:*}"
    local port="${svc##*:}"
    if curl -sf "http://localhost:$port/health" >/dev/null 2>&1; then
      printf "  ✓ %s (localhost:%s)\n" "$name" "$port"
    else
      printf "  ✗ %s (port %s)\n" "$name" "$port"
    fi
  done

  echo ""
  echo "--- Log file sizes ---"
  if [[ -d "$LOG_DIR" ]]; then
    ls -lhS "$LOG_DIR" 2>/dev/null | awk 'NR>1 && /\.log$/ {printf "  %-30s %s\n", $9, $5}'
  fi
}
verb_logs() {
  local service="all"
  local lines=50
  local follow=1
  local errors_only=0
  local clear=0
  local -a positionals=()

  # First positional may be a service name OR a flag.
  if [[ $# -gt 0 && "${1:0:1}" != "-" ]]; then
    service="$1"; shift
  fi

  # Parse flags + collect remaining positionals (e.g., 'client 2' or 'docker postgres').
  # Flags can appear anywhere in the arg list — order-independent per usage docs.
  while [[ $# -gt 0 ]]; do
    case "$1" in
      -n|--lines)
        if [[ $# -lt 2 ]]; then
          echo "verb_logs: -n/--lines requires a number" >&2
          return 2
        fi
        if ! [[ "$2" =~ ^[0-9]+$ ]]; then
          echo "verb_logs: -n/--lines must be a non-negative integer, got '$2'" >&2
          return 2
        fi
        lines="$2"; shift 2 ;;
      -f|--follow)  follow=1; shift ;;
      --no-follow)  follow=0; shift ;;
      --clear)      clear=1; shift ;;
      --errors)     errors_only=1; shift ;;
      *)
        if [[ "$service" == "docker" || "$service" == "client" ]]; then
          positionals+=("$1")
          shift
        else
          echo "verb_logs: unknown arg '$1'" >&2
          return 2
        fi
        ;;
    esac
  done

  # Docker logs delegate. Build args as an array so quoting survives.
  if [[ "$service" == "docker" ]]; then
    local docker_args=("--tail=$lines")
    [[ $follow -eq 1 ]] && docker_args+=("-f")
    local docker_svc="${positionals[0]:-}"
    if [[ -n "$docker_svc" ]]; then
      docker_args+=("$docker_svc")
    fi
    # `compose logs -f` blocks until SIGINT and returns 130. Caller may
    # interpret 130 as success; that's intentional for the follow path.
    compose logs "${docker_args[@]}"
    return $?
  fi

  # Map service name to log file FIRST so --clear can use the resolved path.
  # Otherwise vite→vite-renderer.log and client→desktop-client-N.log would
  # be silently bypassed and --clear would create useless empty files like
  # `vite.log` while the real log stayed full (Gitar finding, 2026-05-22).
  local log_file
  case "$service" in
    all)           log_file="" ;;
    control-plane) log_file="$LOG_DIR/control-plane.log" ;;
    media-plane)   log_file="$LOG_DIR/media-plane.log" ;;
    vite)          log_file="$LOG_DIR/vite-renderer.log" ;;
    client)
      local n="${positionals[0]:-1}"
      if ! [[ "$n" =~ ^[0-9]+$ ]]; then
        echo "verb_logs: client index must be numeric, got '$n'" >&2
        return 2
      fi
      log_file="$LOG_DIR/desktop-client-$n.log"
      ;;
    *)
      echo "verb_logs: unknown service '$service'" >&2
      echo "Services: all, control-plane, media-plane, vite, client [N], docker [<svc>]" >&2
      return 2
      ;;
  esac

  if [[ "$clear" -eq 1 ]]; then
    if [[ "$service" == "all" ]]; then
      for f in "$LOG_DIR"/*.log; do
        [[ -f "$f" ]] && : > "$f"
      done
    else
      : > "$log_file" 2>/dev/null || true
    fi
  fi

  local tail_args=("-n" "$lines")
  [[ $follow -eq 1 ]] && tail_args+=("-f")

  # Build the file list as an array so paths with spaces / metacharacters
  # survive — replaces a prior `eval $cmd` that was vulnerable to command
  # injection when $LOG_DIR contained shell metacharacters (PR review,
  # 2026-05-22).
  local -a log_files
  if [[ "$service" == "all" ]]; then
    log_files=("$LOG_DIR"/*.log)
    if [[ ${#log_files[@]} -eq 0 || ! -e "${log_files[0]}" ]]; then
      echo "verb_logs: no log files found in $LOG_DIR" >&2
      return 1
    fi
  else
    log_files=("$log_file")
  fi

  if [[ $errors_only -eq 1 ]]; then
    tail "${tail_args[@]}" "${log_files[@]}" 2>/dev/null | grep -E 'ERROR|error|FATAL|panic'
  else
    tail "${tail_args[@]}" "${log_files[@]}" 2>/dev/null
  fi
}
verb_restart() {
  [[ $# -gt 0 ]] && { echo "verb_restart: takes no args, got '$1'" >&2; return 2; }
  verb_down --keep-docker
  verb_up --no-rebuild
}

verb_rebuild() {
  [[ $# -gt 0 ]] && { echo "verb_rebuild: takes no args, got '$1'" >&2; return 2; }
  verb_down --keep-docker
  verb_up  # rebuild = default
}

verb_freshstart() {
  [[ $# -gt 0 ]] && { echo "verb_freshstart: takes no args, got '$1'" >&2; return 2; }

  [[ "$QUIET" -eq 0 ]] && echo -e "${RED}=== Fresh Start: this WIPES ALL DEV DATA ===${NC}"
  [[ "$QUIET" -eq 0 ]] && echo "Databases, Redis, client user data — all gone."
  if [[ -t 0 ]]; then  # only prompt if stdin is a TTY
    read -rp "Continue? (y/N) " confirm
    # Portable lowercase compare — `${var,,}` is bash 4+ and breaks macOS bash 3.2.
    if ! [[ "$confirm" =~ ^[Yy]$ ]]; then
      echo "Aborted."
      exit 0
    fi
  fi

  verb_down --force >/dev/null 2>&1 || true
  compose down -v >/dev/null 2>&1 || true
  # `compose down -v` only removes containers belonging to the current compose
  # project (`concordvoice-dev` per docker-compose.dev.yml `name:` directive).
  # `container_name:` in docker-compose.yml uses GLOBAL Docker names — not
  # project-prefixed — so orphans from a prior project (e.g., users migrating
  # from the pre-#1113 `dev-start.sh` which had no `name:` and defaulted to
  # `concord`) survive `compose down` and then conflict during `up` with
  # `Error response from daemon: Conflict. The container name "..." is already
  # in use`. Force-remove by literal name; missing containers are a no-op.
  docker rm -f \
    concordvoice-postgres \
    concordvoice-redis \
    concordvoice-nats \
    concordvoice-coturn \
    concordvoice-minio \
    >/dev/null 2>&1 || true
  rm -rf "$PROJECT_ROOT/.dev-data" 2>/dev/null || true
  verb_up
}
verb_validate_config() {
  if compose config --quiet 2>&1; then
    [[ "$QUIET" -eq 0 ]] && echo -e "  ${GREEN}✓${NC} docker compose config validates (base + dev overlay)"
    return 0
  else
    echo "" >&2
    echo -e "  ${RED}✗${NC} docker compose config failed — invalid YAML or missing env var." >&2
    echo "    See docker output above for the specific failure." >&2
    echo "    If a required env var is missing, run: cp .env.example .env" >&2
    return 1
  fi
}

# ── Source-vs-execute guard ────────────────────────────────────────────
# Tests do `source concord-dev.sh` to call helpers without re-running main.

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
