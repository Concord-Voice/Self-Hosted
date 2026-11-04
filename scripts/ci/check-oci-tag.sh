#!/usr/bin/env bash
# Fail-closed OCI tag state check.
# Usage: check-oci-tag.sh <reference> [allowed-existing-digest]
set -euo pipefail

reference="${1:?usage: check-oci-tag.sh <reference> [allowed-existing-digest]}"
allowed_digest="${2:-}"

if [[ -n "$allowed_digest" && ! "$allowed_digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  printf 'error: invalid allowed digest: %s\n' "$allowed_digest" >&2
  exit 2
fi

set +e
descriptor="$(oras manifest fetch --descriptor "$reference" 2>&1)"
status=$?
set -e

if [[ "$status" -eq 0 ]]; then
  actual_digest="$(jq -er '.digest' <<<"$descriptor" 2>/dev/null || true)"
  if [[ ! "$actual_digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    printf 'error: registry returned an invalid descriptor for %s\n' "$reference" >&2
    exit 2
  fi
  if [[ -n "$allowed_digest" && "$actual_digest" == "$allowed_digest" ]]; then
    printf 'matching\n'
    exit 0
  fi
  printf 'error: OCI tag already exists with digest %s: %s\n' "$actual_digest" "$reference" >&2
  exit 1
fi

expected_absent="Error response from registry: failed to find \"$reference\": $reference: not found"
if [[ "$descriptor" == "$expected_absent" ]]; then
  printf 'absent\n'
  exit 0
fi

printf 'error: unable to determine OCI tag state for %s\n%s\n' "$reference" "$descriptor" >&2
exit 2
