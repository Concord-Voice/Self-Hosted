#!/usr/bin/env bash
# check-narrowed-profile.sh — ADR-0017 / #1430 drift guard (READ-ONLY)
#
# Detects regression of the ADR-0017 "narrowed" SonarCloud quality profile.
# The narrowing (deactivating the `injection`-tagged VULNERABILITY taint rules
# in the project-assigned custom profile per language) is the mechanism that
# skips SonarCloud's redundant JS/TS/Go security sensors — that detection class
# is owned by CodeQL + Semgrep via GitHub Code Scanning (ADR-0017).
#
# A SonarCloud project recreation (e.g. 2026-06-14, see the FP register) mints a
# fresh project that reverts to the stock `Sonar way` profiles, silently undoing
# the narrowing. This guard catches that regression.
#
# For EACH language (js, ts, go):
#   1. Resolve the profile ASSIGNED to project Concord-Voice-Alpha and assert its
#      name == "Concord way (<lang>) — ADR-0017 narrowed".
#   2. Resolve that profile's key, then query rules/search SCOPED to that profile
#      (qprofile=<key>&activation=true) for taint-class VULNERABILITY rules
#      and assert total == 0.
#
# CRITICAL: the rules/search query MUST carry `qprofile=<key>&activation=true`.
# An org-wide query without those params returns the full Sonar catalog (where
# the taint rules always exist) and would FALSE-POSITIVE forever. This is the
# OPPOSITE of narrow-security-profiles.sh, which queries org-wide on purpose to
# ENUMERATE the rules to deactivate; here we assert none are active in the
# assigned profile.
#
# This script is READ-ONLY — it issues no mutating API calls. Remediation (the
# `--apply` op) is a deliberate human action.
#
# Usage:
#   SONAR_TOKEN=<token> ./scripts/sonar/check-narrowed-profile.sh
#
# Exit codes:
#   0 — narrowed/clean: every language's assigned profile is the narrowed one
#       with zero active taint-class VULNERABILITY rules.
#   1 — DRIFT: a wrong profile is assigned OR active taint rules were found.
#       Remediation: re-run scripts/sonar/narrow-security-profiles.sh --apply.
#   3 — operational error: missing SONAR_TOKEN, or an HTTP/API failure. Kept
#       distinct from drift so CI can tell a misconfigured run from real drift.

set -euo pipefail

ORG="concord-voice"
PROJECT="Concord-Voice-Alpha"
API="https://sonarcloud.io/api"
LANGUAGES=("js" "ts" "go")

if [[ -z "${SONAR_TOKEN:-}" ]]; then
  echo "ERROR(op): SONAR_TOKEN must be set (SonarCloud user token with read access to quality profiles)." >&2
  exit 3
fi

# Mirror narrow-security-profiles.sh's scurl helper: curl --fail with token auth.
# --fail makes curl exit non-zero on HTTP >= 400; under `set -e` that aborts the
# script. We trap that to emit an operational error (exit 3), distinct from drift.
scurl() { curl --fail --silent --show-error -u "${SONAR_TOKEN}:" "$@"; }

# op_fail <message>: operational error — HTTP/API/parse failure, NOT drift.
op_fail() {
  echo "ERROR(op): $1" >&2
  exit 3
}

drift=0

for lang in "${LANGUAGES[@]}"; do
  echo "── Language: ${lang} ─────────────────────────────────────────────"

  expected_name="Concord way (${lang}) — ADR-0017 narrowed"

  # 1. Resolve the profile ASSIGNED to the project for this language.
  #    `project=` filters the search to the profile actually in effect for the
  #    project (falls back to the language default if none custom-assigned).
  if ! search_json=$(scurl "${API}/qualityprofiles/search?organization=${ORG}&language=${lang}&project=${PROJECT}"); then
    op_fail "qualityprofiles/search failed for language '${lang}' (HTTP/API error)."
  fi

  assigned_name=$(printf '%s' "${search_json}" | jq -r '.profiles[0].name // empty')
  assigned_key=$(printf '%s' "${search_json}" | jq -r '.profiles[0].key // empty')

  if [[ -z "${assigned_name}" || -z "${assigned_key}" ]]; then
    op_fail "could not resolve assigned profile name/key for language '${lang}' (empty response)."
  fi

  echo "Assigned profile: ${assigned_name} (${assigned_key})"

  if [[ "${assigned_name}" != "${expected_name}" ]]; then
    echo "::error::DRIFT(${lang}): assigned profile is '${assigned_name}', expected '${expected_name}'." >&2
    echo "  → The ADR-0017 narrowed profile is NOT assigned (likely a SonarCloud project recreation)." >&2
    drift=1
    # Skip the rule-count check for this language: the wrong profile is already
    # drift, and its rule set isn't meaningful against the ADR-0017 invariant.
    continue
  fi

  # 2. Count ACTIVE taint-class VULNERABILITY rules SCOPED to this profile.
  #    qprofile=<key>&activation=true scopes to rules active IN the assigned
  #    profile — the load-bearing correction. Without these params the query
  #    returns the org-wide catalog and would false-positive forever.
  #    repositories=<lang>security selects the dedicated taint repositories
  #    (jssecurity / tssecurity / gosecurity). The modern SonarCloud taint rules
  #    DROPPED the legacy `injection` tag (they are CWE-tagged now: e.g.
  #    gosecurity:S3649 sysTags=[cwe,sql]), so a `tags=injection` filter matches
  #    ZERO and made this guard pass vacuously — verified live 2026-06-17
  #    (tags=injection => total 0; repositories=gosecurity/jssecurity/tssecurity
  #    => 9/49/49). See [internal]reports/2026-06-17-pr1583-ci-deepdive-design.md.
  if ! rules_json=$(scurl "${API}/rules/search?organization=${ORG}&qprofile=${assigned_key}&activation=true&types=VULNERABILITY&repositories=${lang}security&ps=1"); then
    op_fail "rules/search failed for profile '${assigned_key}' (language '${lang}', HTTP/API error)."
  fi

  active_total=$(printf '%s' "${rules_json}" | jq -r '.total // empty')

  if [[ -z "${active_total}" || ! "${active_total}" =~ ^[0-9]+$ ]]; then
    op_fail "could not parse rules/search total for language '${lang}' (got '${active_total}')."
  fi

  echo "Active taint-class VULNERABILITY rules in assigned profile: ${active_total}"

  if [[ "${active_total}" -ne 0 ]]; then
    echo "::error::DRIFT(${lang}): ${active_total} active taint-class VULNERABILITY rule(s) in the assigned profile." >&2
    echo "  → The narrowed profile has regained taint rules (narrowing was reverted)." >&2
    drift=1
  fi
done

echo "──────────────────────────────────────────────────────────────────"
if [[ "${drift}" -ne 0 ]]; then
  echo "::error::DRIFT detected: the ADR-0017 narrowed SonarCloud profile has regressed." >&2
  echo "Remediation: re-run the human --apply op:" >&2
  echo "  SONAR_TOKEN=<token> ./scripts/sonar/narrow-security-profiles.sh --apply" >&2
  echo "(See [internal]0017-ci-scanning-result-hub.md and the FP register's drift-guard note.)" >&2
  exit 1
fi

echo "OK: every language's assigned profile is 'Concord way (<lang>) — ADR-0017 narrowed'"
echo "    with zero active taint-class VULNERABILITY rules. No drift."
exit 0
