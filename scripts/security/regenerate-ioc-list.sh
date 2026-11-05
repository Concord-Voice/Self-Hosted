#!/usr/bin/env bash
# regenerate-ioc-list.sh CSV_PATH SHA256_FILE
#
# Reads the Datadog Shai-Hulud 2.0 CSV at CSV_PATH, extracts the package-name
# column (prefers `package_name`, falls back to `package` for older/alternate
# CSV schemas), and writes a sorted, deduped, headered IOC list to
# scripts/security/iocs/shai-hulud-2.0.txt.
#
# SHA256_FILE is the output of `sha256sum CSV_PATH` (one line, two columns).
#
# Exit: 0 on success. Non-zero if the CSV doesn't have the expected column.
#
# Spec: #715.

set -euo pipefail

if [[ $# -lt 2 || $# -gt 3 ]]; then
  echo "usage: $0 CSV_PATH SHA256_FILE [SOURCE_COMMIT]" >&2
  exit 64
fi

CSV="$1"
SHA256_FILE="$2"
SOURCE_COMMIT="${3:-main}"
OUT="${OUT:-scripts/security/iocs/shai-hulud-2.0.txt}"

[[ -f "$CSV" ]] || { echo "[ERROR] CSV not found: $CSV" >&2; exit 1; }
[[ -f "$SHA256_FILE" ]] || { echo "[ERROR] SHA256 file not found: $SHA256_FILE" >&2; exit 1; }

SHA256=$(awk '{print $1}' "$SHA256_FILE")
# Extract sorted unique package names. The Datadog CSV uses `package_name`
# (verified against the live file 2026-04-20); we fall back to `package` for
# compatibility with any future column rename or alternative source.
NAMES=$(python3 - "$CSV" <<'PY'
import csv, sys
with open(sys.argv[1], newline="") as f:
    reader = csv.DictReader(f)
    fieldnames = reader.fieldnames or []
    col = None
    for candidate in ("package_name", "package"):
        if candidate in fieldnames:
            col = candidate
            break
    if col is None:
        print(f"[ERROR] shai-hulud-2.0.csv: expected column 'package_name' or 'package' not found in {fieldnames}", file=sys.stderr)
        sys.exit(2)
    names = sorted({row[col].strip() for row in reader if row.get(col, "").strip()})
    for n in names:
        print(n)
PY
)

mkdir -p "$(dirname "$OUT")"

{
  cat <<HEADER
# Shai-Hulud 2.0 npm supply-chain IOC list
# Source:  https://github.com/DataDog/indicators-of-compromise/blob/$SOURCE_COMMIT/shai-hulud-2.0/shai-hulud-2.0.csv
# Source commit:  $SOURCE_COMMIT
# SHA256 of source CSV:  $SHA256
# Refresh cadence: daily via .github/workflows/refresh-ioc-list.yml (automated PR)
# FP workflow: add the package to scripts/security/iocs/false-positive-allowlist.txt (CODEOWNERS-gated)

HEADER
  echo "$NAMES"
} > "$OUT"

echo "[OK] Wrote $(wc -l < "$OUT") lines to $OUT"
