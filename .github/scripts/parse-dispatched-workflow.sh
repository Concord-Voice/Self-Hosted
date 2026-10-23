#!/usr/bin/env bash
#
# parse-dispatched-workflow.sh — Extracts trust-model relevant data from a
# GitHub Actions workflow YAML file for the L3 sandbox (#921 G6 / #1003).
#
# Reads ONE workflow file path. Emits JSON with two keys:
#   .environments — array of distinct `environment:` declarations across all
#                   jobs in the file. Strings, case-preserved as written.
#                   Empty array if no jobs declare `environment:`.
#   .secrets      — array of distinct secret names referenced via
#                   `${{ secrets.X }}` anywhere in the file. Names are
#                   case-preserved.
#
# This script is the security boundary for Invariants V1 (staging-only
# dispatch) and V7 (staging-secret blast radius surfacing) per spec
# `docs/superpowers/specs/2026-05-14-921-workflow-validation-sandbox-design.md`
# §3.3.
#
# Pure parser. No I/O beyond reading the input file and printing to stdout.
# No network. No `gh` calls. Independently testable from fixtures.
#
# Dependencies: bash 4+, python3 with PyYAML, jq 1.6+. python3 is pre-installed
# on ubuntu-latest GHA runners; PyYAML is installed by pr-ci.yml's existing
# `pip install --user --quiet pyyaml` step in the workflow-lint job (and is
# pre-installed on most developer macOS/Linux systems).
#
# Matching the existing repo precedent — other lint scripts under
# .github/scripts/ (lint-pr-release-trust.sh, lint-multisecret-andcheck.sh,
# lint-always-needs-comment.sh) all use python3+PyYAML for YAML parsing.

set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <workflow-yaml-path>" >&2
  exit 2
fi

WORKFLOW="$1"

if [[ ! -f "$WORKFLOW" ]]; then
  echo "error: file not found: $WORKFLOW" >&2
  exit 2
fi

# Note: environment declarations are captured as literal strings (case-preserved).
# The V1 static FLAG (computed in workflow-validate.yml) marks any workflow whose
# env name contains "production" (case-insensitive SUBSTRING) — catching
# production, production-cloudflare, production-secrets, and expression-valued envs
# whose text contains the "production" token (e.g. `environment: ${{ inputs.x ==
# 'production' && ... }}`). A PURE expression env with no "production" token (e.g.
# `environment: ${{ inputs.target }}`) cannot be statically resolved and is NOT
# flagged. This is static analysis only — #921's dispatch half was collapsed out
# in PR #1371, so the flag is informational (reviewer attention), not a gate.

# Environments: every distinct `environment:` declaration across .jobs.*.
# `environment:` can be a bare string (`environment: staging`) or a mapping
# (`environment: { name: staging, url: ... }`); both forms resolve to a
# string name. python3+PyYAML handles both. Empty array if no jobs declare
# `environment:`.
ENVS_JSON=$(python3 - "$WORKFLOW" <<'PYEOF'
import sys, yaml, json

with open(sys.argv[1]) as f:
    try:
        doc = yaml.safe_load(f) or {}
    except yaml.YAMLError as e:
        print(f"error: YAML parse error in {sys.argv[1]}: {e}", file=sys.stderr)
        sys.exit(2)

envs = set()
for job in (doc.get("jobs") or {}).values():
    if not isinstance(job, dict):
        continue
    env = job.get("environment")
    if isinstance(env, str):
        envs.add(env)
    elif isinstance(env, dict) and isinstance(env.get("name"), str):
        envs.add(env["name"])

print(json.dumps(sorted(envs)))
PYEOF
)

# Secrets: extract every `secrets.NAME` reference from the entire file text.
# We grep the raw YAML rather than parsing because secrets can appear in
# `env:` blocks, `with:` inputs, `run:` blocks, and step `if:` conditions —
# parsing every possible location is fragile. The regex captures
# `secrets.IDENTIFIER` where IDENTIFIER matches GitHub's secret-name rules
# (uppercase letters, digits, underscores). GITHUB_TOKEN is reported if
# referenced because the labeler should still see it in the blast-radius
# table — it's a real grant even though it's the default token.
SECRETS_JSON=$(
  { grep -oE 'secrets\.[A-Za-z_][A-Za-z0-9_]*' "$WORKFLOW" || true; } \
    | sed 's/^secrets\.//' \
    | sort -u \
    | jq -R . \
    | jq -s '.'
)

# Combine into the contract output.
jq -n \
  --argjson envs "$ENVS_JSON" \
  --argjson secrets "$SECRETS_JSON" \
  '{environments: $envs, secrets: $secrets}'
