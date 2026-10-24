#!/usr/bin/env bash
#
# trivy-assert-os.test.sh — self-test for trivy-assert-os.sh and for the two
# silent-clean guards container-image-scan.yml relies on.
#
# Hermetic: the reports are built with jq in the shape Trivy 0.75.0 writes with
# `--list-all-pkgs --format json`. The fields used (.Metadata.OS.Family,
# .Results[].Class, .Results[].Packages) were checked against real 0.75.0
# reports of an alpine image, the same image with /lib/apk removed, and a
# FROM-scratch image. Re-check them after a Trivy major bump:
#
#   trivy image --image-src docker --list-all-pkgs --format json IMAGE \
#     | jq '{os: .Metadata.OS, res: [.Results[] | {Class, n: (.Packages | length)}]}'

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
SCRIPT="${REPO_ROOT}/.github/scripts/trivy-assert-os.sh"
WORKFLOW="${REPO_ROOT}/.github/workflows/container-image-scan.yml"

pass=0
fail=0
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

ok() { echo "  ✓ $1"; pass=$(( pass + 1 )); }
ko() { echo "  ✗ $1" >&2; fail=$(( fail + 1 )); }

# report NAME FAMILY PKG_COUNT — FAMILY empty means no OS detected (no os-pkgs result)
report() {
  jq -n --arg f "$2" --argjson n "$3" '
    { Metadata: (if $f == "" then {} else { OS: { Family: $f, Name: "3.24.2" } } end),
      Results: (if $f == "" then [] else
        [ { Class: "os-pkgs", Type: $f,
            Packages: [ range($n) | { Name: "pkg\(.)", Version: "1" } ] } ] end) }
  ' > "$WORK/$1.json"
}
report alpine   alpine 16
report nopkgdb  alpine 0
report scratch  ""     0
printf 'not json\n' > "$WORK/broken.json"

# check DESC REPORT EXPECT WANT_RC [STDERR_FRAGMENT]
check() {
  local rc
  set +e
  bash "$SCRIPT" "$WORK/$2.json" "$3" > /dev/null 2> "$WORK/stderr"
  rc=$?
  set -e
  if { [ "$4" = 0 ] && [ "$rc" -eq 0 ]; } \
    || { [ "$4" != 0 ] && [ "$rc" -ne 0 ] && grep -q -- "${5:-}" "$WORK/stderr"; }; then
    ok "$1"
  else
    ko "$1 (rc=$rc, stderr: $(head -c 200 "$WORK/stderr"))"
  fi
}

echo "── trivy-assert-os.sh ──"
check "an alpine report with OS packages is accepted"            alpine  alpine 0
check "a FROM-scratch report is accepted when none is expected"  scratch none   0
check "no OS detected when alpine is expected is refused"        scratch alpine 1 "detected OS 'none'"
check "alpine detected with zero OS packages is refused"         nopkgdb alpine 1 "no OS packages"
check "an OS appearing where none is expected is refused"        alpine  none   1 "detected OS 'alpine'"
check "an unreadable report fails rather than passing"           broken  alpine 1

echo ""
echo "── container-image-scan.yml guards ──"

# Every Trivy image scan reads the local daemon only. Without --image-src docker
# a failed daemon read falls through to pulling a registry image of the same
# name (scan/IMAGE:ci on Docker Hub), and that image is what gets scanned.
calls=$(grep -cxE "[[:space:]]+\"\\\$TRIVY_IMAGE\" image \\\\" "$WORKFLOW" || true)
srcs=$(grep -cE '^[[:space:]]+--image-src docker \\$' "$WORKFLOW" || true)
if [ "$calls" -ge 2 ] && [ "$calls" -eq "$srcs" ]; then
  ok "all $calls trivy image scans pass --image-src docker"
else
  ko "trivy image scans: $calls, with --image-src docker: $srcs"
fi

# The OS assertion needs the package list, and must run on every matrix leg.
if grep -qE '^[[:space:]]+--list-all-pkgs \\$' "$WORKFLOW"; then
  ok "the report scan writes the package list (--list-all-pkgs)"
else
  ko "the report scan dropped --list-all-pkgs, so every os-pkgs list reads as empty"
fi
if grep -qE '^[[:space:]]+EXPECT_OS: \$\{\{ matrix\.os \}\}$' "$WORKFLOW" \
  && grep -qxF "          bash .github/scripts/trivy-assert-os.sh \"\$RUNNER_TEMP/trivy-report.json\" \"\$EXPECT_OS\"" "$WORKFLOW"; then
  ok "the report step runs trivy-assert-os.sh with the matrix os"
else
  ko "the report step no longer runs trivy-assert-os.sh with the matrix os"
fi
legs=$(grep -cE '^[[:space:]]+- image: ' "$WORKFLOW" || true)
oses=$(grep -cE '^[[:space:]]+os: (alpine|none)$' "$WORKFLOW" || true)
if [ "$legs" -ge 1 ] && [ "$legs" -eq "$oses" ]; then
  ok "all $legs matrix legs declare an expected os"
else
  ko "matrix legs: $legs, with an expected os: $oses"
fi

echo ""
echo "─────────────────────────────────────────"
echo "PASS: $pass  FAIL: $fail"
[[ "$fail" == "0" ]]
