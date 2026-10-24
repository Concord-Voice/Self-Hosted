#!/bin/bash
# Pin the only new-tier-2 write callers. Tier-1 must never follow this selector.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

expected=$'services/control-plane/internal/media/handlers.go\nservices/control-plane/internal/media/upload_session.go'
actual=$(rg -l --glob '*.go' --glob '!*_test.go' 'h\.requireAttachmentWriteStore\(' services/control-plane | sort || true)

if [[ "$actual" != "$expected" ]]; then
  echo "check-attachment-write-callers: requireAttachmentWriteStore callers changed" >&2
  echo "expected:" >&2
  echo "$expected" >&2
  echo "actual:" >&2
  echo "$actual" >&2
  exit 1
fi

echo "check-attachment-write-callers: tier-2 caller set is pinned"
