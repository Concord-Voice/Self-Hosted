#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="$ROOT/scripts/ci/check-oci-tag.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail=0
pass() { printf 'PASS: %s\n' "$1"; }
fail_case() { printf 'FAIL: %s\n' "$1"; fail=$((fail + 1)); }

mkdir -p "$TMP/bin"
cat >"$TMP/bin/oras" <<'STUB'
#!/usr/bin/env bash
case "${ORAS_STUB_MODE:?}" in
  absent)
    echo 'Error response from registry: failed to find "ghcr.io/concord-voice/minio:test": ghcr.io/concord-voice/minio:test: not found' >&2
    exit 1
    ;;
  exists)
    printf '{"digest":"sha256:%064d"}\n' 1
    ;;
  other)
    echo 'Error: unauthorized: authentication required' >&2
    exit 1
    ;;
  hidden)
    echo 'Error: unauthorized: repository not found' >&2
    exit 1
    ;;
  mixed)
    echo 'Error: unauthorized; MANIFEST_UNKNOWN: manifest unknown' >&2
    exit 1
    ;;
esac
STUB
chmod +x "$TMP/bin/oras"

run_case() {
  local mode="$1" expected_status="$2" description="$3" allowed_digest="${4:-}"
  local status=0 output
  set +e
  output="$(PATH="$TMP/bin:$PATH" ORAS_STUB_MODE="$mode" "$SCRIPT" \
    ghcr.io/concord-voice/minio:test "$allowed_digest" 2>&1)"
  status=$?
  set -e
  if [[ "$status" -eq "$expected_status" ]]; then
    pass "$description"
  else
    fail_case "$description (exit $status: $output)"
  fi
  printf '%s\n' "$output"
}

run_case absent 0 'exact ORAS 1.3.3 GHCR missing-tag result is accepted as absent'
run_case exists 1 'existing tag is rejected when no digest is allowed'
run_case exists 0 'matching existing tag is resumable' "sha256:$(printf '%064d' 1)"
run_case exists 1 'mismatched existing tag is rejected' "sha256:$(printf '%064d' 2)"
run_case other 2 'registry/auth failures fail closed'
run_case hidden 2 'ambiguous repository-not-found auth failures fail closed'
run_case mixed 2 'mixed auth and manifest-unknown text fails closed'

if [[ "$fail" -ne 0 ]]; then
  printf '%d FAILED\n' "$fail"
  exit 1
fi

printf 'ALL PASS\n'
