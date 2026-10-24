#!/usr/bin/env bash
#
# trivy-assert-os.sh — refuse a Trivy JSON report that did not see the image's
# OS packages, so a scan that looked at nothing cannot read as clean.
#
# Usage: trivy-assert-os.sh REPORT_JSON EXPECTED_OS
#   REPORT_JSON comes from `trivy image --list-all-pkgs --format json`.
#   EXPECTED_OS is an OS family such as alpine, or none for a FROM-scratch image.
#
# Two ways a scan finds zero OS packages, both reported as "0: Clean":
#   * Trivy does not recognise the OS, so .Metadata.OS.Family is absent.
#   * Trivy recognises the OS from os-release but the package database is gone
#     (a runtime stage that deletes /lib/apk to drop the package manager). The
#     family is still detected; only the os-pkgs result's package list is empty.
#     Checked with Trivy 0.75.0 against such an image.
# If a base image changes family, this fails until the matrix os in
# container-image-scan.yml is updated, which is the point.
set -euo pipefail

# No angle brackets or parentheses in a ${var:?word}: Semgrep's bash grammar reads
# the word as shell, and the protected scan fails on that parse error.
report="${1:?usage: trivy-assert-os.sh REPORT_JSON EXPECTED_OS}"
expect="${2:?usage: trivy-assert-os.sh REPORT_JSON EXPECTED_OS}"

got="$(jq -r '.Metadata.OS.Family // "" | if . == "" then "none" else . end' "$report")"
pkgs="$(jq '[.Results[]? | select(.Class == "os-pkgs") | .Packages[]?] | length' "$report")"

if [ "$got" != "$expect" ]; then
  echo "::error::Trivy detected OS '$got', expected '$expect': Trivy did not recognise the base image, or the base image changed and the matrix os needs updating" >&2
  exit 1
fi
if [ "$expect" != none ] && [ "$pkgs" -eq 0 ]; then
  echo "::error::Trivy detected OS '$got' but no OS packages: the package database is missing from the image, so a clean result means nothing" >&2
  exit 1
fi
echo "Trivy detected OS '$got' with $pkgs OS packages, as expected."
