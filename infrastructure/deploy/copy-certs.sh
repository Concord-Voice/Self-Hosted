#!/usr/bin/env bash
# /etc/letsencrypt/renewal-hooks/deploy/copy-certs.sh
#
# Certbot deploy hook — runs after every successful certificate issuance or renewal.
# Copies the renewed certificate files into service-specific directories so each
# container is only bind-mounted the files it needs (no access to the full
# /etc/letsencrypt tree). Real file copies rather than symlinks ensure the files
# remain readable inside the container even if certbot rotates the archive path.
#
# Directories created:
#   /opt/concord/certs/coturn/   cert.pem + key.pem  (for coturn DTLS/TLS TURN)
#
# Extend with additional service directories here as needed (e.g. custom nginx
# certs, media-plane TLS, etc.).
#
# Installed by provision-production.sh to:
#   /etc/letsencrypt/renewal-hooks/deploy/copy-certs.sh
# and runs automatically as part of certbot's renewal lifecycle.

set -euo pipefail

DOMAIN="${RENEWED_DOMAINS%% *}"   # first domain in the renewed set
LIVE="/etc/letsencrypt/live/${DOMAIN}"

# Sanity check — exit silently if this isn't the concordvoice cert renewal
if [[ ! -f "${LIVE}/fullchain.pem" ]]; then
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
