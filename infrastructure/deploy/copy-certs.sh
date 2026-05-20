#!/usr/bin/env bash
# /etc/letsencrypt/renewal-hooks/deploy/copy-certs.sh
#
# Certbot deploy hook — runs after every successful certificate issuance or
# renewal. Copies the renewed certificate files into service-specific
# directories so each container is only bind-mounted the files it needs (no
# access to the full /etc/letsencrypt tree). Real file copies rather than
# symlinks ensure the files remain readable inside the container even if
# certbot rotates the archive path.
#
# Also restarts coturn (if the container is running) so it picks up the new
# TLS material without waiting for a manual redeploy. coturn does not reload
# certificates on its own.
#
# Directories created:
#   /opt/concord/certs/coturn/   cert.pem + key.pem  (for coturn DTLS/TLS TURN)
#
# Extend with additional service directories here as needed (e.g. custom
# nginx certs, media-plane TLS, etc.).
#
# Installed by provision-production.sh to:
#   /etc/letsencrypt/renewal-hooks/deploy/copy-certs.sh
# and runs automatically as part of certbot's renewal lifecycle.

set -euo pipefail

# Certbot provides RENEWED_LINEAGE as the canonical path to the renewed cert's
# live directory. Prefer it over deriving from RENEWED_DOMAINS, which is
# brittle when the cert name doesn't match the first domain or when domain
# ordering changes. Fall back to RENEWED_DOMAINS only if RENEWED_LINEAGE is
# unset (e.g. when the script is invoked manually for initial provisioning).
if [[ -n "${RENEWED_LINEAGE:-}" ]]; then
  LIVE="${RENEWED_LINEAGE}"
else
  DOMAIN="${RENEWED_DOMAINS%% *}"   # first domain in the renewed set
  LIVE="/etc/letsencrypt/live/${DOMAIN}"
fi

# Sanity check — cert files don't exist yet.
#
# Legitimate during initial provisioning: provision-production.sh invokes the
# deploy hook manually before certbot has issued the first cert. Exit 0 is
# correct (no error, nothing to do). The stderr line leaves a forensic trail
# so an operator investigating "why did the cert renew but coturn still has
# the old cert?" can grep journalctl and see WHY the hook short-circuited
# (helps catch RENEWED_LINEAGE typos, filesystem-full anomalies, etc.).
if [[ ! -f "${LIVE}/fullchain.pem" || ! -f "${LIVE}/privkey.pem" ]]; then
  echo "[copy-certs] skipping — cert files not found at ${LIVE} (expected during initial provisioning before first certbot run)" >&2
  exit 0
fi

# ── coturn (DTLS/TLS TURN on port 5349) ──────────────────────────────────────
COTURN_CERT_DIR="/opt/concord/certs/coturn"
mkdir -p "${COTURN_CERT_DIR}"
cp "${LIVE}/fullchain.pem" "${COTURN_CERT_DIR}/cert.pem"
cp "${LIVE}/privkey.pem"   "${COTURN_CERT_DIR}/key.pem"
# Readable by root + the concord group only; coturn runs as root inside its
# container but the host-side restriction limits what other processes can read.
chmod 640 "${COTURN_CERT_DIR}/cert.pem" "${COTURN_CERT_DIR}/key.pem"
chown root:concord "${COTURN_CERT_DIR}/cert.pem" "${COTURN_CERT_DIR}/key.pem"

echo "[copy-certs] coturn certs updated in ${COTURN_CERT_DIR}"

# ── Restart coturn so it picks up the new certs ──────────────────────────────
# coturn doesn't reload certs on its own. Restart the container if it's
# running; skip silently if docker isn't available or the container isn't up
# (e.g. during initial provisioning before the first compose up).
if command -v docker >/dev/null 2>&1; then
  if docker ps --format '{{.Names}}' 2>/dev/null | grep -q '^concordvoice-coturn$'; then
    if docker restart concordvoice-coturn >/dev/null 2>&1; then
      echo "[copy-certs] concordvoice-coturn restarted"
    else
      echo "[copy-certs] ERROR: failed to restart concordvoice-coturn — restart manually to load new certs" >&2
      exit 1
    fi
  fi
fi

# ── nginx (host-side reverse proxy) ──────────────────────────────────────────
# Per #881: nginx config references /etc/letsencrypt/live/${DOMAIN}/fullchain.pem
# directly (no copy needed). But nginx caches the cert in memory at startup
# and won't pick up the renewed file until reload. systemctl reload sends
# SIGHUP, which is graceful — existing connections survive, new ones use the
# fresh cert. Skip silently if nginx isn't running (e.g., during initial
# provisioning before nginx is started).
if systemctl is-active --quiet nginx 2>/dev/null; then
  if systemctl reload nginx 2>/dev/null; then
    echo "[copy-certs] nginx reloaded — new TLS handshakes will serve the renewed cert"
  else
    echo "[copy-certs] ERROR: failed to reload nginx — clients will see stale cert until manual reload" >&2
    exit 1
  fi
fi
