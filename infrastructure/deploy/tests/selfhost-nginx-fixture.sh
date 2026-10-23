#!/usr/bin/env bash
# Prepare an isolated nginx unit configuration on the disposable acceptance VM.
set -euo pipefail
umask 077

if [[ $# != 0 || ${GITHUB_ACTIONS:-} != true ||
      ${GITHUB_REPOSITORY:-} != Concord-Voice/Concord-Voice-Alpha ||
      ${RUNNER_ENVIRONMENT:-} != self-hosted ||
      ${SELFHOST_ACCEPTANCE_RUNNER_PROFILE:-} != blacksmith-4vcpu-ubuntu-2404 ]]; then
  echo 'Invalid disposable acceptance CI context' >&2
  exit 1
fi
fixture_uid=$(id -u)
if [[ ! "$fixture_uid" =~ ^[1-9][0-9]{0,9}$ ]] || (( fixture_uid > 2147483647 )); then
  echo 'Acceptance fixture requires a nonroot CI user' >&2
  exit 1
fi
if [[ ${RUNNER_TEMP:-} != /* || ! -d "$RUNNER_TEMP" || -L "$RUNNER_TEMP" ]]; then
  echo 'Acceptance fixture requires an existing absolute runner temporary directory' >&2
  exit 1
fi
fixture_load_state=$(sudo -n systemctl show nginx --property=LoadState --value)
fixture_active_state=$(sudo -n systemctl show nginx --property=ActiveState --value)
fixture_main_pid=$(sudo -n systemctl show nginx --property=MainPID --value)
if [[ "$fixture_load_state" != loaded ||
      ( "$fixture_active_state" != inactive && "$fixture_active_state" != failed ) ||
      "$fixture_main_pid" != 0 ]]; then
  echo 'Acceptance fixture requires a loaded inactive nginx unit with no main PID' >&2
  exit 1
fi
fixture_stage=$(mktemp -d "$RUNNER_TEMP/concord-selfhost-nginx.XXXXXXXX")
trap 'rm -rf -- "$fixture_stage"' EXIT

cat > "$fixture_stage/nginx.conf" <<'CONFIG'
user www-data;
worker_processes auto;
pid /run/concord-selfhost-nginx.pid;
error_log stderr;
events {
    worker_connections 1024;
}
http {
    include /etc/nginx/mime.types;
    default_type application/octet-stream;
    include /etc/nginx/conf.d/concordvoice.conf;
}
CONFIG
cat > "$fixture_stage/nginx.service.conf" <<'UNIT'
[Service]
PIDFile=/run/concord-selfhost-nginx.pid
ExecStartPre=
ExecStartPre=/usr/sbin/nginx -t -q -c /etc/nginx/concord-selfhost-ci.conf
ExecStart=
ExecStart=/usr/sbin/nginx -c /etc/nginx/concord-selfhost-ci.conf
ExecReload=
ExecReload=/usr/sbin/nginx -s reload -c /etc/nginx/concord-selfhost-ci.conf
ExecStop=
ExecStop=/usr/sbin/nginx -s quit -c /etc/nginx/concord-selfhost-ci.conf
UNIT

sudo -n install -o root -g root -m 0644 "$fixture_stage/nginx.conf" /etc/nginx/concord-selfhost-ci.conf
sudo -n install -d -o root -g root -m 0755 /run/systemd/system/nginx.service.d
sudo -n install -o root -g root -m 0644 "$fixture_stage/nginx.service.conf" /run/systemd/system/nginx.service.d/zz-concord-selfhost-ci.conf
sudo -n systemctl daemon-reload
