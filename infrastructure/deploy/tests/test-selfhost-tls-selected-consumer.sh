#!/usr/bin/env bash
# End-to-end selected installer/configuration/fan-out/coturn acceptance.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
command -v python3 >/dev/null 2>&1 || {
  printf '%s\n' 'FAIL: required Python 3 acceptance runner unavailable' >&2
  exit 1
}
[[ -r "$SCRIPT_DIR/selfhost-cert-test-tools.sh" ]] || {
  printf '%s\n' 'FAIL: shared public tooling image helper unavailable' >&2
  exit 1
}
source "$SCRIPT_DIR/selfhost-cert-test-tools.sh"
SELFHOST_CERT_TEST_IMAGE="$(selfhost_cert_prepare_tool_image)" || exit 1
export SELFHOST_CERT_TEST_IMAGE
exec python3 "$SCRIPT_DIR/test-selfhost-tls-selected-consumer.py"
