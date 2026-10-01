#!/usr/bin/env bash
# Concord Voice — self-hosted TLS/cert provisioning (#1617).
# Root-only deploy script. Three modes converge on the canonical cert dir
# /etc/letsencrypt/live/${DOMAIN}/ (consumed identically by the rendered nginx
# vhost and copy-certs.sh -> coturn). No custom crypto (openssl/certbot only).
# No private key / PEM bytes are ever logged (paths + SHA-256 fingerprints only).
#
# Usage:
#   provision-cert.sh local       --host <h> [--ip <a>] [--days <n>]
#   provision-cert.sh letsencrypt --domain <d> --email <e>
#   provision-cert.sh import      --cert <p> --key <p> --host <h>
#
# Exit codes: 0 ok | 1 runtime/validation | 2 usage
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
: "${LE_LIVE_ROOT:=/etc/letsencrypt/live}"   # overridable for tests
: "${HOOK_DIR:=/etc/letsencrypt/renewal-hooks/deploy}"

readonly RED='\033[0;31m' GREEN='\033[0;32m' YELLOW='\033[1;33m' NC='\033[0m'
log()  { echo -e "${GREEN}$*${NC}"; }
warn() { echo -e "${YELLOW}$*${NC}" >&2; }
err()  { echo -e "${RED}$*${NC}" >&2; }

usage() {
  cat <<'USAGE'
Concord Voice TLS/cert provisioning

Usage:
  provision-cert.sh local       --host <h> [--ip <a>] [--days <n>]
  provision-cert.sh letsencrypt --domain <d> --email <e>
  provision-cert.sh import      --cert <p> --key <p> --host <h>

Exit codes: 0 ok | 1 runtime/validation | 2 usage
USAGE
}

# derive_sans HOST [IP] — SAN string covering the host + api/media/turn/spa
# subdomains (superset of the names nginx presents), plus IP when non-empty.
derive_sans() {
  local h="$1" ip="${2:-}"
  local s="DNS:${h},DNS:api.${h},DNS:media.${h},DNS:turn.${h},DNS:spa.${h}"
  [[ -n "$ip" ]] && s="${s},IP:${ip}"
  printf '%s' "$s"
}

# validate_host HOST — strict FQDN (mirrors install-selfhost.sh validate_domain).
# This is the single charset gate for the standalone entry point: rejecting
# anything that isn't an FQDN blocks '/' (openssl -subj RDN injection), ',' and
# ':' (SAN-list injection via derive_sans / -addext), regex metachars (the SAN
# ERE-match bypass), whitespace, and single-label hosts — at ONE point, before
# the value reaches any openssl/grep. install-selfhost.sh pre-validates CFG_DOMAIN,
# but provision-cert.sh is also invoked directly by operators (per the runbook).
validate_host() {
  [[ "${1:-}" =~ ^([A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?\.)+[A-Za-z]{2,}$ ]] || {
    err "invalid host: ${1:-} (expected an FQDN, e.g. example.com or homelab.lan)"; return 1; }
}

# validate_ip IP — literal IPv4 or IPv6 only, so a value with ',' / 'DNS:' /
# metachars can never inject extra SAN entries through derive_sans.
# Same predicate as install-selfhost.sh's validate_public_ip: a shape check, not
# a parser. IPv6 needs a ':' and may end in an embedded IPv4.
validate_ip() {
  local ip="${1:-}"
  [[ -z "$ip" || "$ip" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ || "$ip" =~ ^[0-9A-Fa-f:]*:[0-9A-Fa-f:.]*$ ]] || {
    err "invalid --ip: $ip (expected an IPv4 or IPv6 address)"; return 1; }
}

# canonical_dir DOMAIN — the single cert dir all three modes write/read.
canonical_dir() { printf '%s/%s' "$LE_LIVE_ROOT" "$1"; }

# fingerprint CERT — SHA-256 of the cert (safe to log; never the key).
fingerprint() { openssl x509 -in "$1" -noout -fingerprint -sha256 2>/dev/null | cut -d= -f2; }

# run_copy_certs DIR — invoke the existing deploy hook to fan certs to coturn
# + reload nginx (its documented manual-invocation path). COPY_CERTS_HOOK is
# overridable for tests (default: the real sibling copy-certs.sh).
run_copy_certs() {
  local dir="$1" hook="${COPY_CERTS_HOOK:-${SCRIPT_DIR}/copy-certs.sh}"
  [[ -f "$hook" ]] || { warn "copy-certs hook not found at $hook — skipping coturn/nginx fan-out"; return 0; }
  RENEWED_LINEAGE="$dir" bash "$hook"
}

# mode_local --host <h> [--ip <a>] [--days <n>] — generate a self-signed leaf
# (LAN/homelab convenience; NO MITM protection on untrusted networks).
mode_local() {
  local host="" ip="" days=825
  while (( $# )); do case "$1" in
    --host) host="$2"; shift 2 ;;
    --ip)   ip="$2";   shift 2 ;;
    --days) days="$2"; shift 2 ;;
    *) err "local: unknown flag $1"; return 2 ;;
  esac; done
  [[ -n "$host" ]] || { err "local: --host required"; return 2; }
  validate_host "$host" || return 2
  [[ -n "$ip" ]] && { validate_ip "$ip" || return 2; }
  { [[ "$days" =~ ^[0-9]+$ ]] && (( days >= 1 )); } || { err "local: --days must be a positive integer (>= 1), got: $days"; return 2; }

  local dir; dir="$(canonical_dir "$host")"
  mkdir -p "$dir"
  local cert="${dir}/fullchain.pem" key="${dir}/privkey.pem"
  local sans; sans="$(derive_sans "$host" "$ip")"

  # Self-signed leaf (leaf == chain for self-signed). RSA-4096 (project minimum).
  # -nodes: unencrypted key (consumed by nginx/coturn non-interactively).
  umask 077
  openssl req -x509 -newkey rsa:4096 -sha256 -nodes -days "$days" \
    -keyout "$key" -out "$cert" \
    -subj "/CN=${host}" -addext "subjectAltName=${sans}" >/dev/null 2>&1 || {
      err "local: openssl self-signed generation failed"; return 1; }
  chmod 600 "$key"; chmod 644 "$cert"

  log "✓ self-signed cert written to ${dir} (SHA-256: $(fingerprint "$cert"))"
  warn "⚠ self-signed: NO MITM protection on untrusted networks — trusted LAN/homelab only."
  warn "  For any internet-reachable host use: provision-cert.sh letsencrypt --domain <d> --email <e>"
  run_copy_certs "$dir"
}
# mode_letsencrypt --domain <d> --email <e> — behavior-preserving extraction of
# the inline certbot flow from provision-production.sh. --reuse-key is
# LOAD-BEARING (SPKI-pin stability, #658). DO NOT remove. certonly --webroot
# (#881) avoids the in-place nginx ssl_certificate auto-edit.
mode_letsencrypt() {
  local domain="" email=""
  while (( $# )); do case "$1" in
    --domain) domain="$2"; shift 2 ;;
    --email)  email="$2";  shift 2 ;;
    *) err "letsencrypt: unknown flag $1"; return 2 ;;
  esac; done
  [[ -n "$domain" && -n "$email" ]] || { err "letsencrypt: --domain and --email required"; return 2; }
  validate_host "$domain" || return 2

  if certbot certonly \
      --webroot -w /var/www/html \
      -d "$domain" -d "api.${domain}" -d "media.${domain}" -d "turn.${domain}" \
      --email "$email" --agree-tos --non-interactive --reuse-key 2>&1 | tail -5; then
    log "✓ TLS certificate obtained at $(canonical_dir "$domain")"
  else
    err "Let's Encrypt failed. Ensure DNS A records for ${domain}, api/media/turn.${domain} -> this host, then re-run."
    return 1
  fi

  # Install the deploy hooks for certbot's renewal lifecycle (copy-certs fans to
  # coturn; CF upload self-skips without /etc/concord/cloudflare.env — self-host
  # has none, SaaS does).
  mkdir -p "$HOOK_DIR"
  install -m 0755 "${SCRIPT_DIR}/copy-certs.sh" "${HOOK_DIR}/copy-certs.sh" 2>/dev/null || true
  [[ -f "${SCRIPT_DIR}/upload-cert-to-cloudflare.sh" ]] && \
    install -m 0755 "${SCRIPT_DIR}/upload-cert-to-cloudflare.sh" "${HOOK_DIR}/upload-cert-to-cloudflare.sh" 2>/dev/null || true

  # Immediate fan-out (via the same run_copy_certs helper the other modes use).
  local dir; dir="$(canonical_dir "$domain")"
  if [[ -f "${dir}/fullchain.pem" && -f "${dir}/privkey.pem" ]]; then
    run_copy_certs "$dir"
    # CF custom-cert upload (SaaS SPKI-pin, #658). Non-fatal — self-host self-skips
    # without /etc/concord/cloudflare.env. Preserve the original success/failure
    # observability (the hook self-reports, but keep the wrapper remediation line).
    if [[ -f "${HOOK_DIR}/upload-cert-to-cloudflare.sh" ]]; then
      if RENEWED_LINEAGE="$dir" bash "${HOOK_DIR}/upload-cert-to-cloudflare.sh"; then
        log "✓ CloudFlare custom cert uploaded (or self-skipped on self-host)"
      else
        warn "CloudFlare upload failed. After creating /etc/concord/cloudflare.env run:"
        warn "  sudo RENEWED_LINEAGE=${dir} bash ${HOOK_DIR}/upload-cert-to-cloudflare.sh"
      fi
    fi
  else
    warn "Let's Encrypt reported success but cert files not found at ${dir}; skipping cert fan-out + CF upload."
    warn "Re-run the hooks manually after obtaining certs:"
    warn "  RENEWED_LINEAGE=${dir} bash ${HOOK_DIR}/copy-certs.sh"
  fi
}
# mode_import --cert <p> --key <p> --host <h> — validate a BYO PEM pair, then
# install. Fail-closed: ALL checks pass before any file is written (no partial
# install). openssl only — no custom parsing.
mode_import() {
  local cert="" key="" host=""
  while (( $# )); do case "$1" in
    --cert) cert="$2"; shift 2 ;;
    --key)  key="$2";  shift 2 ;;
    --host) host="$2"; shift 2 ;;
    *) err "import: unknown flag $1"; return 2 ;;
  esac; done
  [[ -n "$cert" && -n "$key" && -n "$host" ]] || { err "import: --cert, --key, --host all required"; return 2; }
  validate_host "$host" || return 2
  [[ -f "$cert" && -f "$key" ]] || { err "import: cert/key file not found"; return 1; }

  # (a) key/cert SPKI match — algorithm-agnostic (RSA + EC). openssl only.
  # Extract the SPKI PEM in a SEPARATE step and assert it is non-empty BEFORE
  # hashing — so a silent openssl failure on either side cannot make both
  # produce sha256("") (= e3b0c442…, a non-empty *string* that would otherwise
  # satisfy `==`). This does not rely on `set -o pipefail` to stay fail-closed.
  local cspki kspki cpub kpub
  cspki="$(openssl x509 -in "$cert" -noout -pubkey 2>/dev/null)" || true
  kspki="$(openssl pkey -in "$key" -pubout 2>/dev/null)" || true
  [[ -n "$cspki" ]] || { err "import: cannot read cert public key"; return 1; }
  [[ -n "$kspki" ]] || { err "import: cannot read private key"; return 1; }
  cpub="$(printf '%s' "$cspki" | openssl sha256 2>/dev/null)"
  kpub="$(printf '%s' "$kspki" | openssl sha256 2>/dev/null)"
  [[ -n "$cpub" && "$cpub" == "$kpub" ]] || { err "import: key does not match cert (SPKI mismatch)"; return 1; }

  # (b) not expired (and not expiring within 0s).
  openssl x509 -in "$cert" -noout -checkend 0 >/dev/null 2>&1 || { err "import: certificate is expired"; return 1; }

  # (c) SAN covers host — literal token comparison (NO regex). Avoids two
  # bypasses: (1) interpolating $host into an ERE makes `.` match any char
  # (DNS:exampleXcom would satisfy example.com); (2) deriving `*.${host#*.}`
  # yields an over-broad `*.com` for an apex host, accepting a *.tld cert. A
  # `*.parent` wildcard covers host only when host is a SINGLE-label child of
  # parent AND parent is itself a domain (has a dot) — so *.com / *.lan never
  # cover a bare apex, and a wildcard matches exactly one label.
  local sans entry parent label covered=0
  sans="$(openssl x509 -in "$cert" -noout -ext subjectAltName 2>/dev/null)"
  while IFS= read -r entry; do
    [[ -z "$entry" ]] && continue
    if [[ "$entry" == "$host" ]]; then covered=1; break; fi
    if [[ "$entry" == '*.'* ]]; then
      parent="${entry#\*.}"
      label="${host%."$parent"}"
      if [[ "$parent" == *.* && "$host" == *."$parent" && -n "$label" && "$label" != "$host" && "$label" != *.* ]]; then
        covered=1; break
      fi
    fi
  done < <(printf '%s\n' "$sans" | tr ',' '\n' | sed -nE 's/.*DNS:([^,[:space:]]+).*/\1/p')
  [[ "$covered" -eq 1 ]] || { err "import: certificate SAN does not cover ${host}"; return 1; }

  # All checks passed — install (no partial install before this point).
  local dir; dir="$(canonical_dir "$host")"; mkdir -p "$dir"
  umask 077
  cp "$cert" "${dir}/fullchain.pem"; cp "$key" "${dir}/privkey.pem"
  chmod 600 "${dir}/privkey.pem"; chmod 644 "${dir}/fullchain.pem"
  log "✓ imported cert installed to ${dir} (SHA-256: $(fingerprint "${dir}/fullchain.pem"))"
  run_copy_certs "$dir"
}

main() {
  case "${1:-}" in
    --help|-h) usage; return 0 ;;
    "")        usage >&2; return 2 ;;
    local)        shift; mode_local "$@" ;;
    letsencrypt)  shift; mode_letsencrypt "$@" ;;
    import)       shift; mode_import "$@" ;;
    *)            err "unknown mode: $1"; usage >&2; return 2 ;;
  esac
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
