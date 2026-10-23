#!/usr/bin/env bash
# Keep the license job on go.mod's selected toolchain, installed directly by
# setup-go into GOROOT. A later toolchain module download can break stdlib
# resolution in go-licenses (google/go-licenses#128, origin: PR #1326).
#
# go-version-file installs the exact selected patch; require its canonical path
# and reject hardcoded go-version overrides through the shared alignment guard.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
exec python3 "$SCRIPT_DIR/check-go-toolchain.py" . --check-license-workflow
