#!/usr/bin/env bash
# Concord Voice — Basic self-hosted installer (#1616).
# Build-from-source. Run from a repo clone on a Debian-12 / Ubuntu-24.04 host.
#
# Usage:
#   ./infrastructure/deploy/install-selfhost.sh            # interactive wizard
#   ./infrastructure/deploy/install-selfhost.sh --env-file answers.env   # non-interactive
#   ./infrastructure/deploy/install-selfhost.sh --help
#
# Exit codes: 0 ok | 1 runtime | 2 preflight failure
set -euo pipefail

# ── Constants ──────────────────────────────────────────────────────────────────

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
readonly PROJECT_ROOT
: "${NGINX_SRC:=$SCRIPT_DIR/nginx/concordvoice.conf}"    # overridable for tests
: "${ENV_OUT:=$PROJECT_ROOT/.env}"                       # overridable for tests
: "${NGINX_OUT:=$SCRIPT_DIR/nginx/concordvoice.rendered.conf}"  # overridable for tests

readonly RED='\033[0;31m'
readonly GREEN='\033[0;32m'
readonly YELLOW='\033[1;33m'
readonly BLUE='\033[0;34m'
readonly NC='\033[0m'

# ── Usage ──────────────────────────────────────────────────────────────────────

usage() {
  cat <<'USAGE'
Concord Voice self-hosted installer

Usage: install-selfhost.sh [--env-file <path>] [--help]
  (no args)        interactive install wizard
  --env-file PATH  non-interactive install from an answers file
  --help           show this help

Exit codes: 0 ok | 1 runtime | 2 preflight failure
USAGE
}

# ── Preflight checks ────────────────────────────────────────────────────────────

# port_in_use PORT — returns 0 if a TCP listener is bound on the port.
# Prefer ss (iproute2), fall back to lsof (macOS / older Linux).
port_in_use() {
  local p="$1"
  if command -v ss >/dev/null 2>&1; then
    ss -ltn 2>/dev/null | grep -qE "[:.]${p}[[:space:]]"
  else
    lsof -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1
  fi
}

# Only HOST-BOUND ports (#3538): Postgres/Redis/NATS/MinIO have no host socket on
# self-host, so a host-run Postgres or Redis must not block the install.
readonly REQUIRED_TCP_PORTS=(8080 3000 3478 5349)

preflight_ports() {
  local busy=() p
  for p in "${REQUIRED_TCP_PORTS[@]}"; do
    port_in_use "$p" && busy+=("$p") || true
  done
  if (( ${#busy[@]} > 0 )); then
    echo -e "${RED}✗ preflight: ports already in use: ${busy[*]}${NC}" >&2
    return 2
  fi
  echo -e "${GREEN}✓ ports free${NC}"
}

preflight_os() {
  local id ver
  # shellcheck source=/dev/null
  id="$(. /etc/os-release 2>/dev/null && echo "${ID:-}")" || true
  # shellcheck source=/dev/null
  ver="$(. /etc/os-release 2>/dev/null && echo "${VERSION_ID:-}")" || true
  case "$id:$ver" in
    debian:12|ubuntu:24.04)
      echo -e "${GREEN}✓ OS $id $ver${NC}"
      ;;
    *)
      echo -e "${YELLOW}⚠ unsupported/untested OS '$id $ver' (supported: Debian 12, Ubuntu 24.04)${NC}" >&2
      return 2
      ;;
  esac
}

# Version floors (#3538). Engine: before 28.0.0, LAN neighbours can reach ports
# published on 127.0.0.1, and from 28.2.0 to 28.3.2 a firewalld reload drops the
# rule that blocks them (CVE-2025-54388). Compose: 2.24.5 is the oldest release
# measured honouring `!override` in docker-compose.selfhost.yml (2.20 and 2.21
# ignore it and merge the lists back to 0.0.0.0). MIN_ENGINE must equal
# SELFHOST_MIN_ENGINE in concord-ctl.sh (pinned by test-selfhost-port-posture.sh).
readonly MIN_ENGINE=28.3.3
readonly MIN_COMPOSE=2.24.5

# version_at_least HAVE NEED — dotted-version comparison via sort -V.
version_at_least() { [[ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -1)" == "$2" ]]; }

preflight_docker() {
  local engine compose
  command -v docker >/dev/null 2>&1 || {
    echo -e "${RED}✗ Docker Engine not found${NC}" >&2
    return 2
  }
  docker compose version >/dev/null 2>&1 || {
    echo -e "${RED}✗ Docker Compose v2 plugin not found${NC}" >&2
    return 2
  }
  engine="$(docker version --format '{{.Server.Version}}' 2>/dev/null | grep -oE '^[0-9]+\.[0-9]+\.[0-9]+' || true)"
  if [[ -z "$engine" ]] || ! version_at_least "$engine" "$MIN_ENGINE"; then
    echo -e "${RED}✗ Docker Engine ${engine:-unknown (is the daemon running and accessible?)} — need >= $MIN_ENGINE (older engines can let LAN neighbours reach loopback-published ports, CVE-2025-54388). Install Docker CE from download.docker.com.${NC}" >&2
    return 2
  fi
  compose="$(docker compose version --short 2>/dev/null | tr -d 'v' | grep -oE '^[0-9]+\.[0-9]+\.[0-9]+' || true)"
  if [[ -z "$compose" ]] || ! version_at_least "$compose" "$MIN_COMPOSE"; then
    echo -e "${RED}✗ Docker Compose ${compose:-unknown} — need >= $MIN_COMPOSE (older Compose ignores !override in docker-compose.selfhost.yml)${NC}" >&2
    return 2
  fi
  command -v python3 >/dev/null 2>&1 || {
    echo -e "${RED}✗ python3 not found (the deploy controller needs it)${NC}" >&2
    return 2
  }
  echo -e "${GREEN}✓ docker ${engine} + compose ${compose} + python3${NC}"
}

# Thresholds: 2 vCPU, 2GiB RAM, 10GiB free disk (conservative BASIC floor).
preflight_resources() {
  local cpus mem_kb disk_kb fail=0
  cpus="$(nproc 2>/dev/null || echo 1)"
  mem_kb="$(awk '/MemTotal/{print $2}' /proc/meminfo 2>/dev/null || echo 0)"
  disk_kb="$(df -Pk "$PROJECT_ROOT" 2>/dev/null | awk 'NR==2{print $4}' || echo 0)"
  (( cpus >= 2 ))          || { echo -e "${YELLOW}⚠ <2 vCPU ($cpus)${NC}" >&2; fail=1; }
  (( mem_kb >= 2000000 ))  || { echo -e "${YELLOW}⚠ <2GiB RAM${NC}" >&2; fail=1; }
  (( disk_kb >= 10000000 )) || { echo -e "${RED}✗ <10GiB free disk${NC}" >&2; fail=1; }
  (( fail == 0 )) && echo -e "${GREEN}✓ resources ok${NC}" || return 2
}

# preflight_logrotate — the security-event log boundary's rotation timer runs
# /usr/sbin/logrotate, and minimal images omit it. Without it the timer fails
# silently and events are dropped once a stream fills (#3561). Checked by path,
# not PATH: /usr/sbin is often absent from an unprivileged PATH.
preflight_logrotate() {
  local bin="${LOGROTATE_BIN_OVERRIDE:-/usr/sbin/logrotate}"
  [[ -x "$bin" ]] || {
    echo -e "${RED}✗ logrotate not found at $bin — sudo apt-get install logrotate${NC}" >&2
    return 2
  }
  echo -e "${GREEN}✓ logrotate${NC}"
}

run_preflight() {
  echo -e "${BLUE}Running preflight (no files written yet)...${NC}"
  if ! { preflight_os && preflight_docker && preflight_resources && preflight_logrotate && preflight_ports; }; then
    echo -e "${RED}Preflight failed — aborting before any change.${NC}" >&2
    exit 2
  fi
}

# ── Secret generation ──────────────────────────────────────────────────────────

# gen_secret — CSPRNG, 32 random bytes → 64 safe hexadecimal characters (256 bits).
gen_secret()  { openssl rand -hex 32; }
gen_hex_key() { openssl rand -hex 32; }                                # 32-byte hex (MFA key shape)

# ── Validators ─────────────────────────────────────────────────────────────────

# validate_domain DOMAIN — strict per-label FQDN; reject empty, leading/trailing/
# consecutive dots, leading/trailing hyphens, and media./api./turn.-prefixed apex.
validate_domain() {
  local d="${1:-}"
  [[ -n "$d" ]] || { echo -e "${RED}domain required${NC}" >&2; return 1; }
  [[ "$d" =~ ^([A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?\.)+[A-Za-z]{2,}$ ]] || {
    echo -e "${RED}invalid domain: $d${NC}" >&2
    return 1
  }
  case "$d" in
    media.*|api.*|turn.*)
      echo -e "${RED}use the apex (e.g. example.com), not a media./api./turn. host${NC}" >&2
      return 1
      ;;
  esac
  return 0
}

# validate_media_subdomain URL — assert the derived MEDIA_PLANE_URL is a media.<apex>
# subdomain, not the root zone (mirrors config.go #725 guard).
validate_media_subdomain() {
  local url="${1:-}" host
  host="${url#https://}"
  host="${host%%/*}"
  [[ "$host" == media.*.* ]] || {
    echo -e "${RED}MEDIA_PLANE_URL must be a media. subdomain, got: $url${NC}" >&2
    return 1
  }
  return 0
}

# validate_cidrs LIST — comma-separated CIDR shape; REJECT all-traffic CIDRs
# (0.0.0.0/0, ::/0) which would make TRUSTED_PROXY_CIDRS trust every source —
# a spoofable X-Forwarded-For / rate-limit-bypass fail-open (CWE-348). Empty is OK
# (the writer applies the private-range default).
validate_cidrs() {
  local cidrs="${1:-}"
  [[ -z "$cidrs" ]] && return 0
  case ",${cidrs// /}," in
    *,0.0.0.0/0,*|*,::/0,*)
      echo -e "${RED}TRUSTED_PROXY_CIDRS must not include 0.0.0.0/0 or ::/0 (would trust every source — X-Forwarded-For spoofing / rate-limit bypass)${NC}" >&2
      return 1
      ;;
  esac
  local c
  IFS=',' read -ra _parsed <<<"${cidrs// /}"
  for c in "${_parsed[@]}"; do
    # Shape + prefix-length bound (IPv4 /0-32, IPv6 /0-128). Octet ranges and the
    # split-range all-traffic case (e.g. 0.0.0.0/1,128.0.0.0/1) are left to the
    # downstream config.go net.ParseCIDR parser — this is an early shape guard and
    # the operator owns their own .env.
    [[ "$c" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}/(3[0-2]|[12][0-9]|[0-9])$ \
      || "$c" =~ ^[0-9A-Fa-f:]+/(12[0-8]|1[01][0-9]|[1-9][0-9]|[0-9])$ ]] || {
      echo -e "${RED}invalid CIDR in TRUSTED_PROXY_CIDRS: $c${NC}" >&2
      return 1
    }
  done
  return 0
}

# validate_smtp_port PORT — numeric 1-65535 (empty OK, writer defaults to 587).
validate_smtp_port() {
  local p="${1:-}"
  [[ -z "$p" ]] && return 0
  { [[ "$p" =~ ^[0-9]+$ ]] && (( p >= 1 && p <= 65535 )); } || {
    echo -e "${RED}invalid SMTP port: $p${NC}" >&2
    return 1
  }
  return 0
}

# validate_https_privacy_url URL — lowercase absolute HTTPS with a real host,
# no userinfo, and an optional valid TCP port. Mirrors the managed workflow.
validate_https_privacy_url() {
  local url="${1:-}" authority host port
  [[ -n "$url" && "$url" == https://* && "$url" != *[[:space:]]* ]] || return 1
  authority="${url#https://}"
  authority="${authority%%[/?#]*}"
  [[ -n "$authority" && "$authority" != *@* && "$authority" != *\\* ]] || return 1
  # ponytail: reject bracketed literal hosts; add an IP parser if raw IPv6
  # privacy-policy URLs become a real operator requirement.
  [[ "$authority" != \[* && "$authority" != *:*:* ]] || return 1
  host="${authority%%:*}"
  [[ "$host" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ \
     && "$host" != *..* && "$host" != *.-* && "$host" != *-.* ]] || return 1
  if [[ "$authority" == *:* ]]; then
    port="${authority#*:}"
    [[ "$port" =~ ^[0-9]{1,5}$ ]] || return 1
    (( 10#$port >= 1 && 10#$port <= 65535 )) || return 1
  fi
}

# validate_activity_history_disclosure OPERATOR URL — both values are optional,
# so an unconfigured self-host remains visibly unavailable. A non-empty privacy
# URL must be absolute HTTPS. Values are single-line because they cross a dotenv
# boundary; arbitrary printable punctuation in the operator name is preserved.
validate_activity_history_disclosure() {
  local operator="${1:-}" url="${2:-}"
  if [[ "$operator" == *$'\n'* || "$operator" == *$'\r'* \
     || "$url" == *$'\n'* || "$url" == *$'\r'* ]]; then
    echo -e "${RED}Activity History disclosure values must be single-line${NC}" >&2
    return 1
  fi
  [[ -z "$url" ]] && return 0
  validate_https_privacy_url "$url" || {
    echo -e "${RED}Activity History privacy URL must be absolute HTTPS with a valid hostname${NC}" >&2
    return 1
  }
}

# validate_tls_mode MODE — one of letsencrypt|local|import (TLS provisioning
# mode handed to provision-cert.sh; #1617). A CLI/wizard choice, never an .env
# var (DR-1: no config.go consumer, no 5-surface discipline).
validate_tls_mode() {
  case "${1:-}" in
    letsencrypt|local|import) return 0 ;;
    *) echo -e "${RED}invalid TLS mode: ${1:-} (letsencrypt|local|import)${NC}" >&2; return 1 ;;
  esac
}

# warn_public_ip IP — non-fatal advisory for an empty/private/NAT PUBLIC_IP.
warn_public_ip() {
  local ip="${1:-}"
  if [[ -z "$ip" ]]; then
    echo -e "${YELLOW}⚠ PUBLIC_IP is empty — TURN relay needs a routable public IP${NC}" >&2
  elif [[ "$ip" =~ ^(10\.|192\.168\.|172\.(1[6-9]|2[0-9]|3[01])\.|127\.) ]]; then
    echo -e "${YELLOW}⚠ PUBLIC_IP '$ip' looks private/NAT — TURN relay will not function without a routable public IP${NC}" >&2
  fi
}

# validate_public_ip IP — empty, or IPv4/IPv6-shaped (the same predicate as
# provision-cert.sh's validate_ip). The value is written to .env and printed inside
# a root command the operator pastes, and its default comes from an external echo
# service, so a shell metacharacter must never get through (#3561). The rejected
# value is not echoed: it is untrusted terminal input.
# ponytail: a shape check, not a parser — it admits malformed addresses such as
# ':::' or '999.1.1.1', which coturn and openssl then reject. IPv6 needs a ':' and
# may end in an embedded IPv4 ('64:ff9b::203.0.113.10').
validate_public_ip() {
  local ip="${1:-}"
  [[ -z "$ip" || "$ip" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ || "$ip" =~ ^[0-9A-Fa-f:]*:[0-9A-Fa-f:.]*$ ]] && return 0
  echo -e "${RED}invalid public IP: expected an IPv4 or IPv6 address${NC}" >&2
  return 1
}

# validate_collected_config — single validation gate for BOTH the interactive and
# --env-file paths, run before any mutation. Field shape checks that the wizard's
# per-prompt validation would catch but the --env-file path otherwise would not.
validate_collected_config() {
  validate_domain "${CFG_DOMAIN:-}" || exit 1
  validate_tls_mode "${CFG_TLS_MODE:-letsencrypt}" || exit 1
  validate_cidrs "${CFG_TRUSTED_CIDRS:-}" || exit 1
  validate_smtp_port "${CFG_SMTP_PORT:-}" || exit 1
  validate_activity_history_disclosure \
    "${CFG_ACTIVITY_HISTORY_OPERATOR_NAME:-}" \
    "${CFG_ACTIVITY_HISTORY_PRIVACY_POLICY_URL:-}" || exit 1
  validate_public_ip "${CFG_PUBLIC_IP:-}" || exit 1
  warn_public_ip "${CFG_PUBLIC_IP:-}"
}

# ── Public IP detection ────────────────────────────────────────────────────────

# detect_public_ip — auto-detect via HTTPS echo service; warn on private/empty.
# Honors PUBLIC_IP env var override.
detect_public_ip() {
  local ip="${PUBLIC_IP:-}"
  [[ -z "$ip" ]] && ip="$(curl -fsS --max-time 5 https://api.ipify.org 2>/dev/null || true)"
  # A non-IP response must never become the wizard's default (#3561). Its own
  # message: the operator typed nothing, so "invalid public IP" would mislead.
  validate_public_ip "$ip" 2>/dev/null || {
    echo -e "${YELLOW}⚠ auto-detected public IP is not an IP address; ignoring it${NC}" >&2
    ip=""
  }
  warn_public_ip "$ip"
  echo "$ip"
}

# ── Config collection ──────────────────────────────────────────────────────────

# load_answers PATH — non-interactive: source a CFG_*-only answers file.
# Each line is validated to be CFG_KEY=value before assignment (no arbitrary exec).
load_answers() {
  local f="${1:-}"
  [[ -f "$f" ]] || { echo -e "${RED}answers file not found: $f${NC}" >&2; return 1; }
  local line key val
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ -z "$line" || "$line" == \#* ]] && continue
    [[ "$line" =~ ^CFG_[A-Z_]+=.*$ ]] || {
      echo -e "${RED}bad answers line: $line${NC}" >&2
      return 1
    }
    key="${line%%=*}"
    val="${line#*=}"
    printf -v "$key" '%s' "$val"
  done <"$f"
  : "${CFG_FEEDBACK_ENABLED:=no}" "${CFG_TURN_REALM:=${CFG_DOMAIN:-}}" \
    "${CFG_TLS_MODE:=letsencrypt}" "${CFG_ACTIVITY_HISTORY_OPERATOR_NAME:=}" \
    "${CFG_ACTIVITY_HISTORY_PRIVACY_POLICY_URL:=}"
  validate_domain "${CFG_DOMAIN:-}" || return 1
}

# ask PROMPT [DEFAULT] — read a non-secret value from stdin; return default if blank.
ask() {
  local p="$1" d="${2:-}" v
  read -r -p "$p${d:+ [$d]}: " v
  echo "${v:-$d}"
}

# ask_secret PROMPT — read a SECRET with terminal echo OFF (read -rs). The prompt
# and the trailing newline go to stderr so only the captured value reaches stdout.
ask_secret() {
  local p="$1" v
  read -rs -p "$p: " v
  echo >&2
  echo "$v"
}

# collect_config — interactive wizard. Populates CFG_* vars. Object storage is the
# bundled MinIO container (external BYO-S3 is a deferred follow-up — see runbook).
collect_config() {
  echo -e "${BLUE}Concord Voice self-hosted install${NC}"
  CFG_DOMAIN="$(ask 'Primary domain (apex, e.g. example.com)')"
  validate_domain "$CFG_DOMAIN" || exit 1
  CFG_TLS_MODE="$(ask 'TLS mode (letsencrypt recommended / local / import)' letsencrypt)"
  validate_tls_mode "$CFG_TLS_MODE" || exit 1
  if [[ "$CFG_TLS_MODE" == local ]]; then
    echo -e "${YELLOW}⚠ 'local' self-signed has NO MITM protection on untrusted networks — trusted LAN/homelab only. Use 'letsencrypt' for any internet-reachable host.${NC}" >&2
  fi
  CFG_TURN_REALM="$(ask 'TURN realm' "$CFG_DOMAIN")"
  CFG_PUBLIC_IP="$(ask 'Public IP (blank = auto-detect)' "$(detect_public_ip)")"
  echo -e "Object storage: bundled MinIO (external BYO-S3 is a deferred follow-up)."
  CFG_SMTP_HOST="$(ask 'SMTP host (required for email verification)')"
  CFG_SMTP_PORT="$(ask 'SMTP port' 587)"
  CFG_SMTP_USER="$(ask 'SMTP username')"
  CFG_SMTP_PASS="$(ask_secret 'SMTP password')"
  CFG_SMTP_FROM="$(ask 'SMTP from address' "noreply@$CFG_DOMAIN")"
  CFG_TRUSTED_CIDRS="$(ask 'Trusted proxy CIDRs' '172.16.0.0/12,10.0.0.0/8')"
  CFG_ACTIVITY_HISTORY_OPERATOR_NAME="$(ask 'Activity History operator name (optional)')"
  CFG_ACTIVITY_HISTORY_PRIVACY_POLICY_URL="$(ask 'Activity History privacy policy URL (optional, HTTPS)')"
  CFG_FEEDBACK_ENABLED="$(ask 'Enable in-app feedback (posts to a GitHub repo)? yes/no' no)"
  if [[ "$CFG_FEEDBACK_ENABLED" == yes ]]; then
    CFG_FEEDBACK_REPO="$(ask 'Feedback repo (owner/repo)')"
    CFG_FEEDBACK_PAT="$(ask_secret 'Feedback PAT')"
  fi
}

# ── .env writer ────────────────────────────────────────────────────────────────

# compose_dotenv_quote VALUE — Compose double-quoted dotenv literal. Encode one
# character at a time so adjacent backslashes/quotes cannot interact through
# Bash replacement semantics. Compose uses $$ for a literal dollar inside a
# double-quoted value; backslash and double quote use their standard escapes.
compose_dotenv_quote() {
  local value="${1:-}" encoded="" char i
  if [[ "$value" == *$'\n'* || "$value" == *$'\r'* ]]; then
    echo "Compose dotenv values must be single-line" >&2
    return 1
  fi
  for ((i = 0; i < ${#value}; i++)); do
    char="${value:i:1}"
    case "$char" in
      \\) encoded+="\\\\" ;;
      \") encoded+="\\\"" ;;
      '$') encoded+="\$\$" ;;
      *) encoded+="$char" ;;
    esac
  done
  printf '"%s"' "$encoded"
}

# write_env_file — write repo-root .env (compose auto-loads it). chmod 600.
# Logs var NAMES only, never values (observability rule — no secret values to stdout/log).
write_env_file() {
  validate_media_subdomain "https://media.${CFG_DOMAIN}" || exit 1

  # Refuse to write CSPRNG secrets into a working tree where .env is NOT gitignored —
  # otherwise a later `git add` could commit live production secrets (CWE-538/312).
  # Skipped outside a git work tree (e.g. test tmpdirs).
  local env_dir
  env_dir="$(dirname "$ENV_OUT")"
  if git -C "$env_dir" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    if ! git -C "$env_dir" check-ignore -q "$ENV_OUT"; then
      echo -e "${RED}✗ $ENV_OUT is not gitignored — refusing to write generated secrets that could be committed${NC}" >&2
      exit 1
    fi
  fi

  local feedback_repo feedback_pat
  if [[ "${CFG_FEEDBACK_ENABLED:-no}" == yes ]]; then
    feedback_repo="${CFG_FEEDBACK_REPO:-}"
    feedback_pat="${CFG_FEEDBACK_PAT:-}"
  else
    feedback_repo="selfhost/disabled"
    feedback_pat="disabled-$(gen_secret)"   # satisfies non-empty + owner/repo shape
  fi

  # Operator free text goes through the Compose encoder: written raw, a '$' is
  # interpolated and ' #' starts a comment, so 'pa$word9 #x' reached the
  # container as 'pa' (#3561). Encoded BEFORE the file is opened, with an
  # explicit exit, so a rejected value leaves no partial .env behind.
  local turn_realm_q smtp_host_q smtp_user_q smtp_pass_q smtp_from_q operator_q policy_q
  turn_realm_q="$(compose_dotenv_quote "${CFG_TURN_REALM:-${CFG_DOMAIN}}")" || exit 1
  smtp_host_q="$(compose_dotenv_quote "${CFG_SMTP_HOST:-}")" || exit 1
  smtp_user_q="$(compose_dotenv_quote "${CFG_SMTP_USER:-}")" || exit 1
  smtp_pass_q="$(compose_dotenv_quote "${CFG_SMTP_PASS:-}")" || exit 1
  smtp_from_q="$(compose_dotenv_quote "${CFG_SMTP_FROM:-noreply@${CFG_DOMAIN}}")" || exit 1
  operator_q="$(compose_dotenv_quote "${CFG_ACTIVITY_HISTORY_OPERATOR_NAME:-}")" || exit 1
  policy_q="$(compose_dotenv_quote "${CFG_ACTIVITY_HISTORY_PRIVACY_POLICY_URL:-}")" || exit 1

  umask 077
  {
    echo "# Concord Voice self-hosted env — generated by install-selfhost.sh ($(date -u +%FT%TZ))"
    echo "# Self-hosted writer surface (NOT the managed /opt/concord/.env). See [internal]rules/deployment.md."
    echo "ENVIRONMENT=production"
    echo "INSTANCE_TYPE=self-hosted"
    echo "DOMAIN=${CFG_DOMAIN}"
    echo "PUBLIC_IP=${CFG_PUBLIC_IP:-}"
    echo "POSTGRES_PASSWORD=$(gen_secret)"
    echo "REDIS_PASSWORD=$(gen_secret)"
    echo "JWT_SECRET=$(gen_secret)"
    echo "MFA_ENCRYPTION_KEY=$(gen_hex_key)"
    echo "TURN_SECRET=$(gen_secret)"
    printf 'TURN_REALM=%s\n' "$turn_realm_q"
    echo "MINIO_ROOT_USER=concord"
    echo "MINIO_ROOT_PASSWORD=$(gen_secret)"
    echo "MINIO_BUCKET=concord-media"
    echo "TRUSTED_PROXY_CIDRS=${CFG_TRUSTED_CIDRS:-172.16.0.0/12,10.0.0.0/8}"
    printf 'SMTP_HOST=%s\n' "$smtp_host_q"
    echo "SMTP_PORT=${CFG_SMTP_PORT:-587}"
    printf 'SMTP_USERNAME=%s\n' "$smtp_user_q"
    printf 'SMTP_PASSWORD=%s\n' "$smtp_pass_q"
    printf 'SMTP_FROM=%s\n' "$smtp_from_q"
    echo "FEEDBACK_REPO=${feedback_repo}"
    echo "FEEDBACK_PAT=${feedback_pat}"
    echo "ACTIVITY_HISTORY_CLUSTER_ENABLED=false"
    echo "CONTROL_PLANE_REPLICA_COUNT=1"
    printf 'ACTIVITY_HISTORY_OPERATOR_NAME=%s\n' "$operator_q"
    printf 'ACTIVITY_HISTORY_PRIVACY_POLICY_URL=%s\n' "$policy_q"
  } >"$ENV_OUT"
  chmod 600 "$ENV_OUT"
  # Log var names only — never values (risk: security / observability rule).
  echo -e "${GREEN}✓ wrote $ENV_OUT${NC} (vars: INSTANCE_TYPE DOMAIN PUBLIC_IP POSTGRES_PASSWORD REDIS_PASSWORD JWT_SECRET MFA_ENCRYPTION_KEY TURN_SECRET TURN_REALM MINIO_ROOT_USER MINIO_ROOT_PASSWORD MINIO_BUCKET TRUSTED_PROXY_CIDRS SMTP_HOST SMTP_PORT SMTP_USERNAME SMTP_PASSWORD SMTP_FROM FEEDBACK_REPO FEEDBACK_PAT ACTIVITY_HISTORY_CLUSTER_ENABLED CONTROL_PLANE_REPLICA_COUNT ACTIVITY_HISTORY_OPERATOR_NAME ACTIVITY_HISTORY_PRIVACY_POLICY_URL)"
}

# ── nginx render ───────────────────────────────────────────────────────────────

# render_nginx — substitute concordvoice.chat -> $CFG_DOMAIN across the canonical
# vhost (single source of truth; managed deploy path untouched). The domain charset
# is restricted by validate_domain, so the sed substitution is injection-safe.
# nginx -t is a manual/integration step (needs cert files present), per the runbook.
render_nginx() {
  [[ -f "$NGINX_SRC" ]] || {
    echo -e "${RED}nginx source not found: $NGINX_SRC${NC}" >&2
    return 1
  }
  sed "s/concordvoice\.chat/${CFG_DOMAIN}/g" "$NGINX_SRC" >"$NGINX_OUT"
  echo -e "${GREEN}✓ rendered nginx vhost -> $NGINX_OUT${NC} (run: sudo cp it into /etc/nginx/... then nginx -t)"
}

# ── TLS cert provisioning (#1617) ────────────────────────────────────────────

# shq VALUE — shell-quote VALUE for a command the operator pastes into a root
# shell (#3561). Domain and IP are validated, but $SCRIPT_DIR comes from the
# checkout path (spaces are ordinary) and SMTP_FROM is free text.
shq() { printf '%q' "$1"; }

# say_root_cmd TEXT — print a ready-to-paste root command. printf, never echo -e,
# so a backslash that shq produced is not re-interpreted.
say_root_cmd() { printf '%b    %s%b\n' "$YELLOW" "$1" "$NC" >&2; }

# provision_tls — print the provision-cert.sh command for the selected mode; it
# never runs it. Every mode writes root-owned paths (/etc/letsencrypt, coturn
# certs, an nginx reload) and this installer runs unprivileged (#3561). Mode is
# a wizard answer / CLI arg, NEVER written to .env (DR-1). PROVISION_CERT is
# overridable for tests (default: the sibling script).
provision_tls() {
  local pc="${PROVISION_CERT:-$SCRIPT_DIR/provision-cert.sh}"
  case "${CFG_TLS_MODE:-letsencrypt}" in
    local)
      # Self-signed needs no webroot or port 80, so it can run straight away.
      echo -e "${YELLOW}⚠ local: run as root now (self-signed, trusted LAN only):${NC}" >&2
      say_root_cmd "sudo $(shq "$pc") local --host $(shq "$CFG_DOMAIN")${CFG_PUBLIC_IP:+ --ip $(shq "$CFG_PUBLIC_IP")}"
      ;;
    letsencrypt)
      # certbot --webroot needs DNS pointing here and /var/www/html served on port
      # 80, which nginx's stock default site does. Every mode runs before the vhost
      # is installed (summary step 3): nginx -t needs the cert files. Unlike
      # provision-production.sh, which bootstraps ufw/webroot/nginx-on-80 before
      # delegating, the installer does not.
      echo -e "${YELLOW}⚠ letsencrypt: run as root once DNS points here and port 80 is open, before installing the vhost (steps below):${NC}" >&2
      say_root_cmd "sudo $(shq "$pc") letsencrypt --domain $(shq "$CFG_DOMAIN") --email $(shq "${CFG_SMTP_FROM:-admin@$CFG_DOMAIN}")"
      ;;
    import)
      # Path placeholders, not <cert.pem>: pasted as-is, '<' and '>' are
      # redirections that create files named after the next flag.
      echo -e "${YELLOW}⚠ import: run as root with your PEM pair, before installing the vhost:${NC}" >&2
      say_root_cmd "sudo $(shq "$pc") import --cert /path/to/fullchain.pem --key /path/to/privkey.pem --host $(shq "$CFG_DOMAIN")"
      ;;
  esac
}

# ── Summary ────────────────────────────────────────────────────────────────────

print_summary() {
  # Paste-safe paths (#3561): shq output reaches the terminal byte-for-byte
  # through a heredoc. echo -e re-read it, so a backslash in the checkout path
  # vanished and the pasted command named a different file.
  local nginx_q boundary_q
  nginx_q="$(shq "$NGINX_OUT")"
  boundary_q="$(shq "$SCRIPT_DIR/provision-production.sh")"
  printf '\n%bInstall configured.%b Next steps:\n' "$GREEN" "$NC"
  cat <<EOF
  1. Firewall (UFW). UFW filters HOST processes (nginx, sshd):
       sudo ufw allow 80,443/tcp
     Docker-published ports are not filtered by UFW on IPv4 (Docker's rules run first).
     The stack publishes only TURN and media publicly; allow them so IPv6 and UFW's own
     view stay correct:
       sudo ufw allow 3478,5349/tcp
       sudo ufw allow 3478/udp
       sudo ufw allow 49152:49252/udp
       sudo ufw allow 40000:41999/udp
     Postgres, Redis and NATS have no host socket; 8080/3000 listen on 127.0.0.1 only.
     Check: docs/runbooks/selfhost-quickstart.md § Verify your exposure
  2. TLS certs (mode '${CFG_TLS_MODE:-letsencrypt}'): run the provision-cert.sh command printed
     above, as root, BEFORE step 3: nginx -t needs the cert files. 'letsencrypt' also needs
     DNS pointing here and port 80 open (step 1); nginx's stock default site answers the
     challenge. Self-signed 'local' is trusted-LAN-only (no MITM protection).
     Guide: docs/runbooks/self-hosted-tls.md
  3. Install the rendered nginx vhost:
       sudo cp $nginx_q /etc/nginx/conf.d/ && sudo nginx -t && sudo systemctl reload nginx
  4. Prepare the security-event log boundary (root, once per host; safe to re-run):
       sudo $boundary_q security-event-logging
     'up' refuses until it exists: the control-plane cannot start without its log stream.
  5. Start the stack:  ./infrastructure/deploy/concord-selfhost up
  6. Check health:     ./infrastructure/deploy/concord-selfhost health
EOF
}

# ── Main ───────────────────────────────────────────────────────────────────────

main() {
  case "${1:-}" in
    --help|-h)
      usage
      return 0
      ;;
    --env-file)
      [[ -n "${2:-}" ]] || { echo -e "${RED}--env-file needs a path${NC}" >&2; return 2; }
      load_answers "$2"
      ;;
    "")
      collect_config
      ;;
    *)
      echo -e "${RED}unknown arg: $1${NC}" >&2
      usage >&2
      return 2
      ;;
  esac
  validate_collected_config
  run_preflight
  write_env_file
  render_nginx
  provision_tls
  print_summary
}

# ── Source guard ───────────────────────────────────────────────────────────────

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
