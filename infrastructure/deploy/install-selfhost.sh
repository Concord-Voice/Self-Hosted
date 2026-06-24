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

readonly REQUIRED_TCP_PORTS=(8080 3000 5432 6379 4222 3478 5349 9000 9001)

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

preflight_docker() {
  command -v docker >/dev/null 2>&1 || {
    echo -e "${RED}✗ Docker Engine not found${NC}" >&2
    return 2
  }
  docker compose version >/dev/null 2>&1 || {
    echo -e "${RED}✗ Docker Compose v2 plugin not found${NC}" >&2
    return 2
  }
  echo -e "${GREEN}✓ docker + compose v2${NC}"
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

run_preflight() {
  echo -e "${BLUE}Running preflight (no files written yet)...${NC}"
  if ! { preflight_os && preflight_docker && preflight_resources && preflight_ports; }; then
    echo -e "${RED}Preflight failed — aborting before any change.${NC}" >&2
    exit 2
  fi
}

# ── Secret generation ──────────────────────────────────────────────────────────

# gen_secret — CSPRNG, ≥40 chars guaranteed (48 raw bytes → ≥48 base64 chars after
# stripping +/=, then truncated to 40). ~238 bits worst case.
gen_secret()  { openssl rand -base64 48 | tr -d '\n=/+' | cut -c1-40; }
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

# warn_public_ip IP — non-fatal advisory for an empty/private/NAT PUBLIC_IP.
warn_public_ip() {
  local ip="${1:-}"
  if [[ -z "$ip" ]]; then
    echo -e "${YELLOW}⚠ PUBLIC_IP is empty — TURN relay needs a routable public IP${NC}" >&2
  elif [[ "$ip" =~ ^(10\.|192\.168\.|172\.(1[6-9]|2[0-9]|3[01])\.|127\.) ]]; then
    echo -e "${YELLOW}⚠ PUBLIC_IP '$ip' looks private/NAT — TURN relay will not function without a routable public IP${NC}" >&2
  fi
}

# validate_collected_config — single validation gate for BOTH the interactive and
# --env-file paths, run before any mutation. Field shape checks that the wizard's
# per-prompt validation would catch but the --env-file path otherwise would not.
validate_collected_config() {
  validate_domain "${CFG_DOMAIN:-}" || exit 1
  validate_cidrs "${CFG_TRUSTED_CIDRS:-}" || exit 1
  validate_smtp_port "${CFG_SMTP_PORT:-}" || exit 1
  warn_public_ip "${CFG_PUBLIC_IP:-}"
}

# ── Public IP detection ────────────────────────────────────────────────────────

# detect_public_ip — auto-detect via HTTPS echo service; warn on private/empty.
# Honors PUBLIC_IP env var override.
detect_public_ip() {
  local ip="${PUBLIC_IP:-}"
  [[ -z "$ip" ]] && ip="$(curl -fsS --max-time 5 https://api.ipify.org 2>/dev/null || true)"
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
  : "${CFG_FEEDBACK_ENABLED:=no}" "${CFG_TURN_REALM:=${CFG_DOMAIN:-}}"
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
  CFG_TURN_REALM="$(ask 'TURN realm' "$CFG_DOMAIN")"
  CFG_PUBLIC_IP="$(ask 'Public IP (blank = auto-detect)' "$(detect_public_ip)")"
  echo -e "Object storage: bundled MinIO (external BYO-S3 is a deferred follow-up)."
  CFG_SMTP_HOST="$(ask 'SMTP host (required for email verification)')"
  CFG_SMTP_PORT="$(ask 'SMTP port' 587)"
  CFG_SMTP_USER="$(ask 'SMTP username')"
  CFG_SMTP_PASS="$(ask_secret 'SMTP password')"
  CFG_SMTP_FROM="$(ask 'SMTP from address' "noreply@$CFG_DOMAIN")"
  CFG_TRUSTED_CIDRS="$(ask 'Trusted proxy CIDRs' '172.16.0.0/12,10.0.0.0/8')"
  CFG_FEEDBACK_ENABLED="$(ask 'Enable in-app feedback (posts to a GitHub repo)? yes/no' no)"
  if [[ "$CFG_FEEDBACK_ENABLED" == yes ]]; then
    CFG_FEEDBACK_REPO="$(ask 'Feedback repo (owner/repo)')"
    CFG_FEEDBACK_PAT="$(ask_secret 'Feedback PAT')"
  fi
}

# ── .env writer ────────────────────────────────────────────────────────────────

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

  umask 077
  {
    echo "# Concord Voice self-hosted env — generated by install-selfhost.sh ($(date -u +%FT%TZ))"
    echo "# Self-hosted writer surface (NOT the managed /opt/concord/.env). See [internal]rules/deployment.md."
    echo "ENVIRONMENT=production"
    echo "DOMAIN=${CFG_DOMAIN}"
    echo "PUBLIC_IP=${CFG_PUBLIC_IP:-}"
    echo "POSTGRES_PASSWORD=$(gen_secret)"
    echo "REDIS_PASSWORD=$(gen_secret)"
    echo "JWT_SECRET=$(gen_secret)"
    echo "MFA_ENCRYPTION_KEY=$(gen_hex_key)"
    echo "TURN_SECRET=$(gen_secret)"
    echo "TURN_REALM=${CFG_TURN_REALM:-${CFG_DOMAIN}}"
    echo "MINIO_ROOT_USER=concord"
    echo "MINIO_ROOT_PASSWORD=$(gen_secret)"
    echo "MINIO_BUCKET=concord-media"
    echo "TRUSTED_PROXY_CIDRS=${CFG_TRUSTED_CIDRS:-172.16.0.0/12,10.0.0.0/8}"
    echo "SMTP_HOST=${CFG_SMTP_HOST:-}"
    echo "SMTP_PORT=${CFG_SMTP_PORT:-587}"
    echo "SMTP_USERNAME=${CFG_SMTP_USER:-}"
    echo "SMTP_PASSWORD=${CFG_SMTP_PASS:-}"
    echo "SMTP_FROM=${CFG_SMTP_FROM:-noreply@${CFG_DOMAIN}}"
    echo "FEEDBACK_REPO=${feedback_repo}"
    echo "FEEDBACK_PAT=${feedback_pat}"
  } >"$ENV_OUT"
  chmod 600 "$ENV_OUT"
  # Log var names only — never values (risk: security / observability rule).
  echo -e "${GREEN}✓ wrote $ENV_OUT${NC} (vars: DOMAIN PUBLIC_IP POSTGRES_PASSWORD REDIS_PASSWORD JWT_SECRET MFA_ENCRYPTION_KEY TURN_SECRET TURN_REALM MINIO_ROOT_USER MINIO_ROOT_PASSWORD MINIO_BUCKET TRUSTED_PROXY_CIDRS SMTP_HOST SMTP_PORT SMTP_USERNAME SMTP_PASSWORD SMTP_FROM FEEDBACK_REPO FEEDBACK_PAT)"
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

# ── Summary ────────────────────────────────────────────────────────────────────

print_summary() {
  # shellcheck disable=SC2028
  echo -e "
${GREEN}Install configured.${NC} Next steps:
  1. Get TLS certs (ACME recommended — a working desktop client needs a publicly-trusted cert).
     See docs/runbooks/selfhost-quickstart.md for certbot + self-signed guidance.
  2. Install the rendered nginx vhost:
       sudo cp $NGINX_OUT /etc/nginx/conf.d/ && sudo nginx -t && sudo systemctl reload nginx
  3. Open firewall ports (UFW):
       sudo ufw allow 80,443/tcp
       sudo ufw allow 3478,5349/tcp
       sudo ufw allow 3478/udp
       sudo ufw allow 49152:49252/udp
     (keep MinIO 9000/9001 host-internal — do NOT expose to the internet)
  4. Start the stack:  ./infrastructure/deploy/concord-selfhost up
  5. Check health:     ./infrastructure/deploy/concord-selfhost health"
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
  print_summary
}

# ── Source guard ───────────────────────────────────────────────────────────────

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
