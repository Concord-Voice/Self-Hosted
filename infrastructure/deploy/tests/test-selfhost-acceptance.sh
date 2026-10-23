#!/usr/bin/env bash
# Pure unit entry for the #2505 observation/evidence validators.
# This script never invokes Docker or the fresh-host acceptance CLI.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
exec python3 "$ROOT/infrastructure/deploy/tests/test_selfhost_acceptance.py" "$@"
