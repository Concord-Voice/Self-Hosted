#!/usr/bin/env bash
# Assert the static source-bucket contract shared by the shipped vhost and the
# blue/green real-nginx fixture. Runtime requests are covered by deploy CI's
# nginx harness, which supplies the same binary map.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
VHOST="$ROOT/infrastructure/deploy/nginx/concordvoice.conf"
BLUEGREEN="$ROOT/infrastructure/deploy/tests/test-control-plane-bluegreen-nginx.sh"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

# shellcheck disable=SC2016 # literal nginx variable names and capture group
grep -qF 'map $binary_remote_addr $source_addr_bucket {' "$VHOST" || fail 'source bucket map missing'
# shellcheck disable=SC2016 # literal nginx regex capture
grep -qF '"~^(\C{8})" $1;' "$VHOST" || fail 'source bucket /64 binary capture missing'

for zone in api auth ws_handshake; do
  grep -qE "^limit_req_zone \\\$source_addr_bucket zone=${zone}:" "$VHOST" \
    || fail "request zone ${zone} does not use source bucket"
done
for zone in addr turn_addr; do
  grep -qE "^limit_conn_zone \\\$source_addr_bucket zone=${zone}:" "$VHOST" \
    || fail "connection zone ${zone} does not use source bucket"
done

# shellcheck disable=SC2016 # literal nginx variable name
if grep -Eq '^limit_(req|conn)_zone \$binary_remote_addr ' "$VHOST"; then
  fail 'a production limit zone still uses raw binary remote address'
fi
# shellcheck disable=SC2016 # literal nginx variable names
grep -qF 'map $binary_remote_addr $source_addr_bucket {' "$BLUEGREEN" \
  || fail 'blue-green fixture does not mirror source bucket map'

printf 'PASS: nginx source address bucket contract\n'
