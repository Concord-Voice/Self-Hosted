#!/usr/bin/env bash
# check-self-hosted-runners.sh — fail on non-allowlisted self-hosted runners.
#
# Walks .github/workflows/*.yml and checks every job's runs-on against
# scripts/security/self-hosted-runner-allowlist.txt. A spec is allowed iff
# every label in it appears in the allowlist.
#
# Exit codes:
#   0  — all runners pass (no self-hosted, or fully allowlisted).
#   1  — one or more self-hosted jobs have labels not in the allowlist.
#   2  — one or more runs-on: values are expressions (e.g. ${{ matrix.x }})
#        and cannot be statically verified. Always a warn-worthy case.
#   10 — workflow enumeration/parsing failed (e.g. PyYAML missing); refuses
#        to report clean.
#
# Runbook: [internal]supply-chain-defense.md. Spec: #715.

set -euo pipefail

# The required PR workflow supplies the protected-source policy path.
ALLOWLIST="${RUNNER_ALLOWLIST_PATH:-scripts/security/self-hosted-runner-allowlist.txt}"
WORKFLOWS_GLOB=".github/workflows"

load_allowlist() {
  [[ -f "$ALLOWLIST" ]] || { echo ""; return 0; }
  awk 'BEGIN{FS="#"} { gsub(/^[[:space:]]+|[[:space:]]+$/, "", $1); if ($1 != "") print $1 }' "$ALLOWLIST"
}

enumerate_runs_on() {
  local wf
  for wf in "$WORKFLOWS_GLOB"/*.yml "$WORKFLOWS_GLOB"/*.yaml; do
    [[ -f "$wf" ]] || continue
    # The caller's `||` disables set -e here and the loop returns its last
    # command's status, so a parser failure must leave the function itself.
    python3 - "$wf" <<'PY' || return
import json, re, sys
try:
    import yaml
except ImportError:
    print("[ERROR] PyYAML not available for workflow parsing", file=sys.stderr)
    sys.exit(10)

path = sys.argv[1]
with open(path) as f:
    doc = yaml.safe_load(f)
if not isinstance(doc, dict):
    sys.exit(0)
jobs = doc.get("jobs") or {}
expr_re = re.compile(r"\$\{\{.*?\}\}")
for job_name, job in jobs.items():
    if not isinstance(job, dict):
        continue
    ro = job.get("runs-on")
    if ro is None:
        continue
    if isinstance(ro, str):
        if expr_re.search(ro):
            payload = {"expr": True, "raw": ro}
        else:
            payload = ro
    elif isinstance(ro, list):
        payload = list(ro)
    else:
        payload = {"expr": True, "raw": repr(ro)}
    print(f"{path}\t{job_name}\t{json.dumps(payload)}")
PY
  done
}

main() {
  local allowlist
  allowlist=$(load_allowlist)

  local had_fail=0
  local had_expr=0

  # Run enumerate_runs_on into a temp file so we can check its exit code.
  # Process substitution (< <(...)) does NOT propagate child exit codes under
  # `set -e`, which would silently mask parser failures (e.g. missing PyYAML)
  # and cause the linter to report a false-clean result on workflows it can't
  # actually parse.
  local enum_output enum_rc=0
  enum_output=$(mktemp)
  # shellcheck disable=SC2064  # intentional: capture $enum_output at trap-set
  trap "rm -f $enum_output" EXIT
  enumerate_runs_on > "$enum_output" || enum_rc=$?
  if [[ $enum_rc -ne 0 ]]; then
    echo "[ERROR] runner-linter: workflow enumeration failed (rc=$enum_rc) — refusing to report clean" >&2
    exit 10
  fi

  while IFS=$'\t' read -r file job runs_on_json; do
    [[ -n "$file" ]] || continue

    if python3 -c '
import json, sys
v = json.loads(sys.argv[1])
sys.exit(0 if isinstance(v, dict) and v.get("expr") else 1)
' "$runs_on_json"; then
      local raw
      raw=$(python3 -c 'import json, sys; print(json.loads(sys.argv[1])["raw"])' "$runs_on_json")
      echo "[WARN] $file (job \"$job\"): runs-on is an expression, cannot statically verify: $raw"
      had_expr=1
      continue
    fi

    local labels
    labels=$(python3 -c '
import json, sys
v = json.loads(sys.argv[1])
if isinstance(v, str):
    labels = [v]
elif isinstance(v, list):
    labels = [str(x) for x in v]
else:
    labels = []
for lab in labels:
    print(lab)
' "$runs_on_json")

    if ! grep -Fxq "self-hosted" <<< "$labels"; then
      continue
    fi

    local missing
    missing=$(comm -23 <(echo "$labels" | sort -u) <(echo "$allowlist" | sort -u))
    if [[ -n "$missing" ]]; then
      local labels_pretty
      labels_pretty=$(echo "$labels" | tr '\n' ',' | sed 's/,$//')
      echo "[FAIL] $file (job \"$job\"): runs-on includes non-allowlisted self-hosted labels [$labels_pretty]; missing from allowlist: $(echo "$missing" | tr '\n' ',' | sed 's/,$//')"
      had_fail=1
    fi
  done < "$enum_output"

  if [[ $had_fail -eq 1 ]]; then
    exit 1
  fi
  if [[ $had_expr -eq 1 ]]; then
    exit 2
  fi
  exit 0
}

main "$@"
