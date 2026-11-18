#!/usr/bin/env bash
#
# Regression test for scripts/security/npm-audit-gate.mjs.
# Locks the gate's contract: only un-accepted HIGH/CRITICAL advisories block;
# the wall-clock alone never blocks (expired exceptions warn, not fail).

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GATE="$SCRIPT_DIR/npm-audit-gate.mjs"
WORK="$(mktemp -d)"
trap 'rm -f /tmp/_audit_gate_out.$$ ; rm -rf "$WORK"' EXIT

pass=0
fail=0

# assert <name> <got-exit> <want-exit> <output> [grep-pattern]
assert() {
  local name="$1" got="$2" want="$3" out="$4" pat="${5:-}"
  if [ "$got" -ne "$want" ]; then
    echo "FAIL: $name — expected exit $want, got $got"
    printf '    %s\n' "${out//$'\n'/$'\n'    }"
    fail=$((fail + 1))
    return
  fi
  if [ -n "$pat" ] && ! grep -qE "$pat" <<<"$out"; then
    echo "FAIL: $name — output missing /$pat/"
    printf '    %s\n' "${out//$'\n'/$'\n'    }"
    fail=$((fail + 1))
    return
  fi
  echo "ok:   $name"
  pass=$((pass + 1))
}

# run <name> <audit-json-file> <allowlist-json-file> <expected-exit> [grep-pattern]
# Gate-mode case, invoking the .mjs directly with a fixed --now for determinism.
run() {
  local name="$1" audit="$2" allow="$3" want="$4" pat="${5:-}"
  local out got
  out="$(node "$GATE" --audit "$audit" --allowlist "$allow" --now 2026-06-15T00:00:00Z 2>&1)"
  got=$?
  assert "$name" "$got" "$want" "$out" "$pat"
}

# run_prod <name> <audit> <prod-audit> <allowlist> <expected-exit> [grep-pattern]
# Same as `run` but also supplies the production-only report that a
# `"scope": "dev"` entry is verified against.
run_prod() {
  local name="$1" audit="$2" prod="$3" allow="$4" want="$5" pat="${6:-}"
  local out got
  out="$(node "$GATE" --audit "$audit" --audit-prod "$prod" --allowlist "$allow" --now 2026-06-15T00:00:00Z 2>&1)"
  got=$?
  assert "$name" "$got" "$want" "$out" "$pat"
}

# ── fixtures ────────────────────────────────────────────────────────────────
cat >"$WORK/empty-allow.json" <<'JSON'
{ "accepted": [] }
JSON

cat >"$WORK/clean.json" <<'JSON'
{ "auditReportVersion": 2, "vulnerabilities": {} }
JSON

cat >"$WORK/moderate-only.json" <<'JSON'
{ "auditReportVersion": 2, "vulnerabilities": {
  "js-yaml": { "name": "js-yaml", "severity": "moderate",
    "via": [ { "source": 1, "name": "js-yaml", "title": "DoS", "url": "https://github.com/advisories/GHSA-h67p-54hq-rp68", "severity": "moderate" } ] } } }
JSON

cat >"$WORK/high.json" <<'JSON'
{ "auditReportVersion": 2, "vulnerabilities": {
  "ws": { "name": "ws", "severity": "high",
    "via": [ { "source": 2, "name": "ws", "title": "Memory exhaustion DoS", "url": "https://github.com/advisories/GHSA-96hv-2xvq-fx4p", "severity": "high" } ] },
  "engine.io": { "name": "engine.io", "severity": "high", "via": [ "ws" ] } } }
JSON

cat >"$WORK/critical.json" <<'JSON'
{ "auditReportVersion": 2, "vulnerabilities": {
  "evilpkg": { "name": "evilpkg", "severity": "critical",
    "via": [ { "source": 3, "name": "evilpkg", "title": "RCE", "url": "https://github.com/advisories/GHSA-aaaa-bbbb-cccc", "severity": "critical" } ] } } }
JSON

cat >"$WORK/two-high.json" <<'JSON'
{ "auditReportVersion": 2, "vulnerabilities": {
  "ws": { "name": "ws", "severity": "high",
    "via": [ { "source": 2, "name": "ws", "title": "Memory exhaustion DoS", "url": "https://github.com/advisories/GHSA-96hv-2xvq-fx4p", "severity": "high" } ] },
  "tmp": { "name": "tmp", "severity": "high",
    "via": [ { "source": 4, "name": "tmp", "title": "Path traversal", "url": "https://github.com/advisories/GHSA-7c78-jf6q-g5cm", "severity": "high" } ] } } }
JSON

# Same GHSA reported for the same package (via.name) under two nodes: counted
# once. Compare the multi-pkg fixtures below, where via.name differs.
cat >"$WORK/dup-high.json" <<'JSON'
{ "auditReportVersion": 2, "vulnerabilities": {
  "ws": { "name": "ws", "severity": "high",
    "via": [ { "source": 2, "name": "ws", "title": "Memory exhaustion DoS", "url": "https://github.com/advisories/GHSA-96hv-2xvq-fx4p", "severity": "high" } ] },
  "ws-other": { "name": "ws-other", "severity": "high",
    "via": [ { "source": 2, "name": "ws", "title": "Memory exhaustion DoS", "url": "https://github.com/advisories/GHSA-96hv-2xvq-fx4p", "severity": "high" } ] } } }
JSON

# One GHSA that npm reports for two DIFFERENT packages. An exception scoped to
# one of them must not waive the other, in either report order.
cat >"$WORK/multi-pkg-allowed-first.json" <<'JSON'
{ "auditReportVersion": 2, "vulnerabilities": {
  "ws": { "name": "ws", "severity": "high",
    "via": [ { "source": 2, "name": "ws", "title": "Memory exhaustion DoS", "url": "https://github.com/advisories/GHSA-96hv-2xvq-fx4p", "severity": "high" } ] },
  "zz-other": { "name": "zz-other", "severity": "high",
    "via": [ { "source": 5, "name": "zz-other", "title": "Memory exhaustion DoS", "url": "https://github.com/advisories/GHSA-96hv-2xvq-fx4p", "severity": "high" } ] } } }
JSON

cat >"$WORK/multi-pkg-allowed-last.json" <<'JSON'
{ "auditReportVersion": 2, "vulnerabilities": {
  "aa-other": { "name": "aa-other", "severity": "high",
    "via": [ { "source": 5, "name": "aa-other", "title": "Memory exhaustion DoS", "url": "https://github.com/advisories/GHSA-96hv-2xvq-fx4p", "severity": "high" } ] },
  "ws": { "name": "ws", "severity": "high",
    "via": [ { "source": 2, "name": "ws", "title": "Memory exhaustion DoS", "url": "https://github.com/advisories/GHSA-96hv-2xvq-fx4p", "severity": "high" } ] } } }
JSON

# Scope values that are not exactly "dev" (after trimming and lowercasing).
cat >"$WORK/allow-ws-scope-space.json" <<'JSON'
{ "accepted": [ { "id": "GHSA-96hv-2xvq-fx4p", "package": "ws", "scope": "dev ", "reason": "dev-only build chain", "review_by": "2026-12-31" } ] }
JSON
cat >"$WORK/allow-ws-scope-development.json" <<'JSON'
{ "accepted": [ { "id": "GHSA-96hv-2xvq-fx4p", "package": "ws", "scope": "development", "reason": "dev-only build chain", "review_by": "2026-12-31" } ] }
JSON
# "dеv" with a Cyrillic е (U+0435).
cat >"$WORK/allow-ws-scope-lookalike.json" <<'JSON'
{ "accepted": [ { "id": "GHSA-96hv-2xvq-fx4p", "package": "ws", "scope": "d\u0435v", "reason": "dev-only build chain", "review_by": "2026-12-31" } ] }
JSON

# One entry per package for a GHSA npm reports for two packages.
cat >"$WORK/allow-ws-and-other.json" <<'JSON'
{ "accepted": [
  { "id": "GHSA-96hv-2xvq-fx4p", "package": "ws", "reason": "no fix yet", "review_by": "2026-12-31" },
  { "id": "GHSA-96hv-2xvq-fx4p", "package": "zz-other", "reason": "no fix yet", "review_by": "2026-12-31" } ] }
JSON

# Two entries for the same (advisory, package), one dev-scoped and one not.
cat >"$WORK/allow-ws-twice.json" <<'JSON'
{ "accepted": [
  { "id": "GHSA-96hv-2xvq-fx4p", "package": "ws", "scope": "dev", "reason": "dev-only build chain", "review_by": "2026-12-31" },
  { "id": "GHSA-96hv-2xvq-fx4p", "package": "ws", "reason": "no fix yet", "review_by": "2026-12-31" } ] }
JSON

# A package-less entry next to a packaged one, for the same GHSA.
cat >"$WORK/allow-ws-plus-nopackage.json" <<'JSON'
{ "accepted": [
  { "id": "GHSA-96hv-2xvq-fx4p", "package": "ws", "reason": "no fix yet", "review_by": "2026-12-31" },
  { "id": "GHSA-96hv-2xvq-fx4p", "scope": "dev", "reason": "dev-only build chain", "review_by": "2026-12-31" } ] }
JSON
# Two entries that are the same package once trimmed and lowercased.
cat >"$WORK/allow-ws-twice-normalised.json" <<'JSON'
{ "accepted": [
  { "id": "GHSA-96hv-2xvq-fx4p", "package": "ws", "reason": "no fix yet", "review_by": "2026-12-31" },
  { "id": "GHSA-96hv-2xvq-fx4p", "package": " WS ", "reason": "no fix yet", "review_by": "2026-12-31" } ] }
JSON
# Entries for two packages, neither of which is the one npm reported.
cat >"$WORK/allow-ws-and-aa.json" <<'JSON'
{ "accepted": [
  { "id": "GHSA-96hv-2xvq-fx4p", "package": "ws", "reason": "no fix yet", "review_by": "2026-12-31" },
  { "id": "GHSA-96hv-2xvq-fx4p", "package": "aa-other", "reason": "no fix yet", "review_by": "2026-12-31" } ] }
JSON
# Scopes that are not strings, or blank.
cat >"$WORK/allow-ws-scope-false.json" <<'JSON'
{ "accepted": [ { "id": "GHSA-96hv-2xvq-fx4p", "package": "ws", "scope": false, "reason": "dev-only build chain", "review_by": "2026-12-31" } ] }
JSON
cat >"$WORK/allow-ws-scope-blank.json" <<'JSON'
{ "accepted": [ { "id": "GHSA-96hv-2xvq-fx4p", "package": "ws", "scope": "  ", "reason": "dev-only build chain", "review_by": "2026-12-31" } ] }
JSON
cat >"$WORK/allow-ws-scope-null.json" <<'JSON'
{ "accepted": [ { "id": "GHSA-96hv-2xvq-fx4p", "package": "ws", "scope": null, "reason": "no fix yet", "review_by": "2026-12-31" } ] }
JSON
# A package value with a line break, followed by a workflow command.
cat >"$WORK/allow-injected-package.json" <<'JSON'
{ "accepted": [ { "id": "GHSA-96hv-2xvq-fx4p", "package": "zz\n::warning::INJECTED", "reason": "no fix yet", "review_by": "2026-12-31" } ] }
JSON

# Production report where the same GHSA reaches production only through
# zz-other, not through ws.
cat >"$WORK/prod-other-pkg.json" <<'JSON'
{ "auditReportVersion": 2, "vulnerabilities": {
  "zz-other": { "name": "zz-other", "severity": "high",
    "via": [ { "source": 5, "name": "zz-other", "title": "Memory exhaustion DoS", "url": "https://github.com/advisories/GHSA-96hv-2xvq-fx4p", "severity": "high" } ] } } }
JSON

# `vulnerabilities: null` parses, but it is a broken report, not an empty one.
cat >"$WORK/null-vulns.json" <<'JSON'
{ "auditReportVersion": 2, "vulnerabilities": null }
JSON

cat >"$WORK/allow-ws-valid.json" <<'JSON'
{ "accepted": [ { "id": "GHSA-96hv-2xvq-fx4p", "package": "ws", "reason": "no fix yet; tracked in #9999", "review_by": "2026-12-31" } ] }
JSON

cat >"$WORK/allow-ws-expired.json" <<'JSON'
{ "accepted": [ { "id": "GHSA-96hv-2xvq-fx4p", "package": "ws", "reason": "no fix yet; tracked in #9999", "review_by": "2026-01-01" } ] }
JSON

cat >"$WORK/allow-ws-only.json" <<'JSON'
{ "accepted": [ { "id": "GHSA-96hv-2xvq-fx4p", "package": "ws", "reason": "no fix yet", "review_by": "2026-12-31" } ] }
JSON

# ── scoping fixtures (an exception is scoped to a package, not a bare id) ────
# No `package` at all — unverifiable against npm's report, must fail closed.
cat >"$WORK/allow-ws-nopackage.json" <<'JSON'
{ "accepted": [ { "id": "GHSA-96hv-2xvq-fx4p", "reason": "no fix yet", "review_by": "2026-12-31" } ] }
JSON

# Scoped to a DIFFERENT package than npm reported the advisory for.
cat >"$WORK/allow-ws-wrongpackage.json" <<'JSON'
{ "accepted": [ { "id": "GHSA-96hv-2xvq-fx4p", "package": "tmp", "reason": "no fix yet", "review_by": "2026-12-31" } ] }
JSON

# Claims the dependency is dev-only; honoured ONLY when a production-only
# report proves the advisory is absent from the runtime graph.
cat >"$WORK/allow-ws-devscope.json" <<'JSON'
{ "accepted": [ { "id": "GHSA-96hv-2xvq-fx4p", "package": "ws", "scope": "dev", "reason": "dev-only build chain", "review_by": "2026-12-31" } ] }
JSON

# A production-only report with nothing in it — the advisory is genuinely dev-only.
cat >"$WORK/prod-clean.json" <<'JSON'
{ "auditReportVersion": 2, "vulnerabilities": {} }
JSON

cat >"$WORK/bad.json" <<'JSON'
{ not valid json
JSON

# Valid JSON, but NOT a real npm-audit result — an `{error:...}` object (npm
# emits this on registry/network/auth failure). Must FAIL CLOSED, not pass.
cat >"$WORK/error-report.json" <<'JSON'
{ "error": { "code": "ENETUNREACH", "summary": "request to registry failed" } }
JSON

# Valid JSON missing the `vulnerabilities` key entirely (truncated/partial
# report). Must FAIL CLOSED.
cat >"$WORK/no-vulns-key.json" <<'JSON'
{ "auditReportVersion": 2 }
JSON

# Allowlist with a single EXPIRED exception, for the --expiry-scan mode.
cat >"$WORK/allow-expired-scan.json" <<'JSON'
{ "accepted": [ { "id": "GHSA-96hv-2xvq-fx4p", "reason": "no fix yet", "review_by": "2026-01-01" } ] }
JSON

# Allowlist with a single FUTURE-dated exception (not expired).
cat >"$WORK/allow-future-scan.json" <<'JSON'
{ "accepted": [ { "id": "GHSA-96hv-2xvq-fx4p", "reason": "no fix yet", "review_by": "2099-01-01" } ] }
JSON

# ── npm shim (for end-to-end .sh invocation) ────────────────────────────────
# A fake `npm` that emits a fixture for `npm audit --json`, so the .sh wrapper
# (and the main-guard) are exercised end-to-end without the real registry.
SHIM_BIN="$WORK/shim-bin"
mkdir -p "$SHIM_BIN"
cat >"$SHIM_BIN/npm" <<'SH'
#!/usr/bin/env bash
if [ "${1:-}" = "audit" ]; then
  [ -n "${NPM_SHIM_STDERR:-}" ] && printf '%s\n' "$NPM_SHIM_STDERR" >&2
  [ -n "${NPM_SHIM_OUTPUT:-}" ] && cat "$NPM_SHIM_OUTPUT"
  exit "${NPM_SHIM_EXIT:-1}"
fi
exit 0
SH
chmod +x "$SHIM_BIN/npm"

# A package dir the .sh runs inside (needs a local .audit-allowlist.json).
PKG="$WORK/pkg"
mkdir -p "$PKG"
cp "$WORK/empty-allow.json" "$PKG/.audit-allowlist.json"

# A symlink to the .mjs, to prove the direct-invocation guard fires through a
# symlinked path (the old string idiom silently no-op'd here).
LINK_DIR="$WORK/linked"
mkdir -p "$LINK_DIR"
ln -s "$GATE" "$LINK_DIR/npm-audit-gate.mjs"

# ── cases ───────────────────────────────────────────────────────────────────
run "no vulnerabilities passes"           "$WORK/clean.json"        "$WORK/empty-allow.json"   0 "Gate passed"
run "moderate-only passes (below gate)"   "$WORK/moderate-only.json" "$WORK/empty-allow.json"  0 "Gate passed"
run "un-accepted HIGH blocks"             "$WORK/high.json"         "$WORK/empty-allow.json"   1 "::error::.*GHSA-96HV-2XVQ-FX4P"
run "un-accepted CRITICAL blocks"         "$WORK/critical.json"     "$WORK/empty-allow.json"   1 "::error::.*GHSA-AAAA-BBBB-CCCC"
run "accepted HIGH passes"                "$WORK/high.json"         "$WORK/allow-ws-valid.json" 0 "accepted"
run "expired exception warns not blocks"  "$WORK/high.json"         "$WORK/allow-ws-expired.json" 0 "::warning::.*review_by"
run "partial allowlist still blocks rest" "$WORK/two-high.json"     "$WORK/allow-ws-only.json"  1 "::error::.*GHSA-7C78-JF6Q-G5CM"
run "same GHSA, same package, counted once"         "$WORK/dup-high.json"     "$WORK/allow-ws-only.json"  0 "1 accepted, 0 blocking"
run "invalid audit JSON errors (exit 2)"  "$WORK/bad.json"          "$WORK/empty-allow.json"    2 "not valid JSON"

# Fail-closed: a valid-JSON report that is not a real npm-audit result must NOT pass.
run "error-object report fails closed"    "$WORK/error-report.json" "$WORK/empty-allow.json"    2 "refusing to pass"
run "missing vulnerabilities key fails closed" "$WORK/no-vulns-key.json" "$WORK/empty-allow.json" 2 "refusing to pass"
run "null vulnerabilities fails closed"   "$WORK/null-vulns.json"   "$WORK/empty-allow.json"    2 "refusing to pass"

# ── exception scoping ───────────────────────────────────────────────────────
# A bare GHSA id is a standing waiver much broader than any reviewed reason:
# advisories are amended in place upstream, and the same id reappears when a
# package moves onto a runtime path. So an exception must name its package, and
# a dev-only claim must be verifiable. Every failure mode here BLOCKS.
run "entry without package fails closed"  "$WORK/high.json" "$WORK/allow-ws-nopackage.json"    1 'no "package" field'
run "entry scoped to another package blocks" "$WORK/high.json" "$WORK/allow-ws-wrongpackage.json" 1 'scoped to package "tmp" but npm reported it for "ws"'
run "one GHSA, two packages: the other still blocks (allowed package first)" "$WORK/multi-pkg-allowed-first.json" "$WORK/allow-ws-valid.json" 1 'scoped to package "ws" but npm reported it for "zz-other"'
# The old id-only dedupe kept the first record, so the blocking case above is
# the one that catches it. This one catches a dedupe that keeps the last record
# instead. The two summary cases pin that ws is still accepted: the old gate
# printed "1 ...; 1 accepted, 0 blocking" and "1 ...; 0 accepted, 1 blocking".
run "one GHSA, two packages: the other still blocks (allowed package last)" "$WORK/multi-pkg-allowed-last.json" "$WORK/allow-ws-valid.json" 1 'scoped to package "ws" but npm reported it for "aa-other"'
run "one GHSA, two packages: ws still accepted (allowed package first)" "$WORK/multi-pkg-allowed-first.json" "$WORK/allow-ws-valid.json" 1 '2 HIGH/CRITICAL advisory\(ies\); 1 accepted, 1 blocking'
run "one GHSA, two packages: ws still accepted (allowed package last)" "$WORK/multi-pkg-allowed-last.json" "$WORK/allow-ws-valid.json" 1 '2 HIGH/CRITICAL advisory\(ies\); 1 accepted, 1 blocking'
run "dev-scoped entry with no prod report fails closed" "$WORK/high.json" "$WORK/allow-ws-devscope.json" 1 "no production-only audit report was supplied"
run_prod "dev-scoped entry passes when advisory is absent from prod graph" \
  "$WORK/high.json" "$WORK/prod-clean.json" "$WORK/allow-ws-devscope.json" 0 "1 accepted, 0 blocking"
run_prod "dev-scoped entry blocks when advisory IS in the prod graph" \
  "$WORK/high.json" "$WORK/high.json" "$WORK/allow-ws-devscope.json" 1 "reachable in the PRODUCTION dependency graph"
# One entry per package accepts both; two entries for one package accept none.
run "one GHSA, one entry per package: both accepted" "$WORK/multi-pkg-allowed-first.json" "$WORK/allow-ws-and-other.json" 0 '2 HIGH/CRITICAL advisory\(ies\); 2 accepted, 0 blocking'
run "two entries for one (GHSA, package) block" "$WORK/high.json" "$WORK/allow-ws-twice.json" 1 'allowlist has 2 entries for GHSA-96HV-2XVQ-FX4P scoped to package "ws"; keep one'
run "package-less entry next to a packaged one blocks the id" "$WORK/high.json" "$WORK/allow-ws-plus-nopackage.json" 1 'no "package" field'
run "entries equal after normalising count as duplicates" "$WORK/high.json" "$WORK/allow-ws-twice-normalised.json" 1 'allowlist has 2 entries for GHSA-96HV-2XVQ-FX4P'
run "mismatch names every scoped package" "$WORK/multi-pkg-allowed-first.json" "$WORK/allow-ws-and-aa.json" 1 'scoped to package "ws", "aa-other" but npm reported it for "zz-other"'
run "allowlist text cannot start a workflow command line" "$WORK/high.json" "$WORK/allow-injected-package.json" 1 'scoped to package "zz ::warning::INJECTED"'
run "non-string scope blocks as unrecognized" "$WORK/high.json" "$WORK/allow-ws-scope-false.json" 1 'unrecognized scope false'
run "blank scope blocks as unrecognized" "$WORK/high.json" "$WORK/allow-ws-scope-blank.json" 1 'unrecognized scope "  "'
run "null scope means no scope" "$WORK/high.json" "$WORK/allow-ws-scope-null.json" 0 "1 accepted, 0 blocking"
# A scope that is not "dev" must not read as no scope. "dev " is trimmed and
# then verified like "dev"; anything else blocks.
run_prod "scope \"dev \" is verified like \"dev\" (in prod: blocks)" \
  "$WORK/high.json" "$WORK/high.json" "$WORK/allow-ws-scope-space.json" 1 "reachable in the PRODUCTION dependency graph"
run_prod "scope \"dev \" is verified like \"dev\" (absent from prod: accepted)" \
  "$WORK/high.json" "$WORK/prod-clean.json" "$WORK/allow-ws-scope-space.json" 0 "1 accepted, 0 blocking"
run_prod "scope \"development\" blocks as unrecognized" \
  "$WORK/high.json" "$WORK/high.json" "$WORK/allow-ws-scope-development.json" 1 'unrecognized scope "development"'
run_prod "look-alike scope blocks as unrecognized" \
  "$WORK/high.json" "$WORK/prod-clean.json" "$WORK/allow-ws-scope-lookalike.json" 1 'unrecognized scope'
# The prod check is per package too: the GHSA reaching production through
# zz-other must not void the dev waiver for ws. zz-other blocks on its own
# record; ws stays accepted (the id-only check printed "0 accepted, 2 blocking").
run_prod "dev-scoped entry holds when only another package carries the GHSA in prod" \
  "$WORK/multi-pkg-allowed-first.json" "$WORK/prod-other-pkg.json" "$WORK/allow-ws-devscope.json" 1 '2 HIGH/CRITICAL advisory\(ies\); 1 accepted, 1 blocking'
run_prod "dev-scoped entry blocks on a null-vulnerabilities prod report" \
  "$WORK/high.json" "$WORK/null-vulns.json" "$WORK/allow-ws-devscope.json" 1 "production-only audit report was unusable"
# FAIL-OPEN REGRESSION: an `{error:...}` or truncated PRODUCTION report is truthy
# and parses, but carries no `vulnerabilities` key — so a naive extract yields an
# EMPTY set and every dev-scoped entry reads as "absent from production" and
# PASSES. A transient npm/registry outage would then silently un-gate every
# dev-scoped exception. "Unusable" must be distinguished from "empty".
run_prod "dev-scoped entry blocks on an {error} prod report (not fail-open)" \
  "$WORK/high.json" "$WORK/error-report.json" "$WORK/allow-ws-devscope.json" 1 "production-only audit report was unusable"
run_prod "dev-scoped entry blocks on a truncated prod report (not fail-open)" \
  "$WORK/high.json" "$WORK/no-vulns-key.json" "$WORK/allow-ws-devscope.json" 1 "production-only audit report was unusable"
# An unusable prod report must NOT disturb entries that make no dev claim.
run_prod "unusable prod report does not affect a non-dev-scoped entry" \
  "$WORK/high.json" "$WORK/error-report.json" "$WORK/allow-ws-valid.json" 0 "1 accepted, 0 blocking"
# Regression: scoping must not break the ordinary un-scoped accept path.
run_prod "package-scoped entry still passes with a prod report present" \
  "$WORK/high.json" "$WORK/prod-clean.json" "$WORK/allow-ws-valid.json" 0 "1 accepted, 0 blocking"

# ── end-to-end .sh + main-guard ─────────────────────────────────────────────
out="$(cd "$PKG" && PATH="$SHIM_BIN:$PATH" NPM_SHIM_OUTPUT="$WORK/high.json" NPM_SHIM_EXIT=1 \
  bash "$SCRIPT_DIR/npm-audit-gate.sh" --allowlist .audit-allowlist.json 2>&1)"; got=$?
assert ".sh end-to-end: HIGH blocks (main-guard fired)" "$got" 1 "$out" "Gate FAILED"

out="$(cd "$PKG" && PATH="$SHIM_BIN:$PATH" NPM_SHIM_OUTPUT="$WORK/clean.json" NPM_SHIM_EXIT=0 \
  bash "$SCRIPT_DIR/npm-audit-gate.sh" --allowlist .audit-allowlist.json 2>&1)"; got=$?
assert ".sh end-to-end: clean passes" "$got" 0 "$out" "Gate passed"

out="$(cd "$PKG" && PATH="$SHIM_BIN:$PATH" NPM_SHIM_OUTPUT="$WORK/error-report.json" NPM_SHIM_EXIT=1 \
  bash "$SCRIPT_DIR/npm-audit-gate.sh" --allowlist .audit-allowlist.json 2>&1)"; got=$?
assert ".sh end-to-end: error report fails closed + surfaces stderr" "$got" 2 "$out" "refusing to pass"

# Direct invocation through a SYMLINKED path — guard must still fire (exit 1 on HIGH).
out="$(node "$LINK_DIR/npm-audit-gate.mjs" --audit "$WORK/high.json" --allowlist "$WORK/empty-allow.json" --now 2026-06-15T00:00:00Z 2>&1)"; got=$?
assert "main-guard fires through symlinked path" "$got" 1 "$out" "Gate FAILED"

# ── --expiry-scan mode ──────────────────────────────────────────────────────
out="$(node "$GATE" --expiry-scan --allowlist "$WORK/allow-future-scan.json" --now 2026-06-15T00:00:00Z 2>&1)"; got=$?
assert "expiry-scan: future-dated exception is clean" "$got" 0 "$out" "no expired exceptions"

out="$(node "$GATE" --expiry-scan --allowlist "$WORK/allow-expired-scan.json" --now 2026-06-15T00:00:00Z 2>&1)"; got=$?
assert "expiry-scan: expired exception fails (exit 1)" "$got" 1 "$out" "::warning::.*passed its review_by"

out="$(node "$GATE" --expiry-scan --allowlist "$WORK/allow-future-scan.json" --allowlist "$WORK/allow-expired-scan.json" --now 2026-06-15T00:00:00Z 2>&1)"; got=$?
assert "expiry-scan: multiple allowlists, one expired fails" "$got" 1 "$out" "1 expired exception"

out="$(node "$GATE" --expiry-scan --allowlist "$WORK/empty-allow.json" --now 2026-06-15T00:00:00Z 2>&1)"; got=$?
assert "expiry-scan: empty allowlist is clean" "$got" 0 "$out" "no expired exceptions"

echo "─────────────────────────────────"
echo "npm-audit-gate test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
