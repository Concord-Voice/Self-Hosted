#!/usr/bin/env bash
#
# lint-workflow-call-contract.sh — enforce the two halves of the workflow_call
# contract that neither actionlint nor PR CI can see: PERMISSIONS and SECRETS.
#
# WHY THIS EXISTS. GitHub rejects the ENTIRE RUN when a `uses:` job grants less
# than the called workflow asks for. The failure is `startup_failure` with ZERO
# jobs and NO log — there is nothing to open on the run page, so it is close to
# undiagnosable without knowing this rule. It cost a silently-dead nightly on
# 2026-08-26: nightly-ci.yml's scorecard job carried scorecard.yml's
# contents/security-events/actions grants but dropped its issues, pull-requests,
# and checks reads. actionlint does not check this — it validates syntax and
# expressions, not a cross-file permission relation — and a schedule-only
# orchestrator never executes in PR CI, so nothing else could have caught it.
#
# SEMANTICS THIS ENCODES (both halves are load-bearing):
#   * Job-level `permissions:` REPLACE workflow-level, they do not merge. A job
#     that declares ANY permission gets EXACTLY that set.
#   * A job that declares NO `permissions:` INHERITS the workflow-level set.
#     Missing this half makes the linter reject working pipelines — pr-ci.yml's
#     build and sonar jobs are exactly that shape.
#
# A called workflow's requirement is the union, at max rank, of each of its
# jobs' EFFECTIVE permissions (job-level if present, else its own
# workflow-level). Local relative and this repository's SHA-pinned
# callees are followed recursively; other remote callees are skipped.

set -euo pipefail

if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
  sed -n '2,32p' "$0" | sed 's/^# \{0,1\}//'
  echo
  echo "Usage: lint-workflow-call-contract.sh [repo-root]"
  echo "Exits: 0 clean | 1 a caller under-grants | 2 script/parse error"
  exit 0
fi

if [[ $# -gt 1 ]]; then
  echo "ERROR: lint-workflow-call-contract.sh: expected zero or one repository-root argument" >&2
  exit 2
fi

ROOT="${1:-.}"
if [[ ! -d "$ROOT/.github/workflows" ]]; then
  echo "ERROR: lint-workflow-call-contract.sh: no .github/workflows under: $ROOT" >&2
  exit 2
fi

if ! command -v python3 >/dev/null 2>&1; then
  echo "ERROR: lint-workflow-call-contract.sh: python3 is required" >&2
  exit 2
fi

python3 - "$ROOT" <<'PY'
from __future__ import annotations

import pathlib
import os
import re
import subprocess
import sys

try:
    import yaml
except ImportError:
    print("ERROR: PyYAML is required", file=sys.stderr)
    raise SystemExit(2)

ROOT = pathlib.Path(sys.argv[1]).resolve()
WORKFLOWS = ROOT / ".github" / "workflows"

# `none` is a real, declarable value and must rank below `read`.
RANK = {"none": 0, "read": 1, "write": 2}

failures: list[str] = []
_cache: dict[tuple[pathlib.Path, str | None], dict] = {}


def bootstrap_workflow_source(path: pathlib.Path, ref: str) -> str | None:
    """Use the proposed callee until protected main contains this first port."""
    env = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
    def git(*args: str) -> subprocess.CompletedProcess[str]:
        return subprocess.run(["git", "-C", str(ROOT), *args], capture_output=True,
                              text=True, env=env)

    if (git("cat-file", "-e", "origin/main:.github/workflows/required-pr-build.yml").returncode == 0
            or git("merge-base", "--is-ancestor", ref, "origin/main").returncode != 0
            or git("cat-file", "-e", f"{ref}:{path.relative_to(ROOT)}").returncode == 0):
        return None
    return path.read_text() if path.is_file() else None


def local_callee(uses: str, ref: str | None = None) -> tuple[pathlib.Path, str | None] | None:
    if uses.startswith("./"):
        return ROOT / uses[2:], ref
    prefix = "Concord-Voice/Self-Hosted/"
    if uses.startswith(prefix + ".github/workflows/"):
        path, _, pinned_ref = uses[len(prefix):].rpartition("@")
        if re.fullmatch(r"[0-9a-f]{40}", pinned_ref):
            return ROOT / path, pinned_ref
    return None


def load(path: pathlib.Path, ref: str | None = None) -> dict:
    key = path, ref
    if key not in _cache:
        try:
            if ref:
                pinned = subprocess.run(
                    ["git", "-C", str(ROOT), "show", f"{ref}:{path.relative_to(ROOT)}"],
                    capture_output=True, text=True,
                    env={k: v for k, v in os.environ.items() if not k.startswith("GIT_")},
                )
                source = pinned.stdout if pinned.returncode == 0 else bootstrap_workflow_source(path, ref)
                if source is None:
                    pinned.check_returncode()
            else:
                source = path.read_text()
            doc = yaml.safe_load(source) or {}
        except (yaml.YAMLError, OSError, subprocess.CalledProcessError) as exc:
            print(f"ERROR: {path.relative_to(ROOT)}: workflow unavailable or unparseable at {ref or 'checkout'}: {exc}", file=sys.stderr)
            raise SystemExit(2)
        if not isinstance(doc, dict):
            print(f"ERROR: {path.relative_to(ROOT)}: workflow is not a mapping", file=sys.stderr)
            raise SystemExit(2)
        _cache[key] = doc
    return _cache[key]


def as_perm_map(value) -> dict | None:
    """Normalise a `permissions:` value. None means 'not declared'."""
    if value is None:
        return None
    if isinstance(value, str):
        # `permissions: read-all` / `write-all` — a blanket grant.
        if value == "read-all":
            return {"__all__": "read"}
        if value == "write-all":
            return {"__all__": "write"}
        return {}
    if isinstance(value, dict):
        return {str(k): str(v) for k, v in value.items()}
    return {}


def covers(grant: dict, scope: str, level: str) -> bool:
    have = grant.get(scope)
    if have is None:
        blanket = grant.get("__all__")
        if blanket is None:
            return False
        have = blanket
    return RANK.get(have, 0) >= RANK.get(level, 0)


def required_by(path: pathlib.Path, ref: str | None, seen: frozenset[tuple[pathlib.Path, str | None]]) -> dict:
    """Union, at max rank, of every job's EFFECTIVE permissions in `path`."""
    key = path, ref
    if key in seen:
        return {}
    seen = seen | {key}
    doc = load(path, ref)
    top = as_perm_map(doc.get("permissions"))
    need: dict[str, str] = {}

    def merge(perms: dict | None) -> None:
        for scope, level in (perms or {}).items():
            if RANK.get(level, 0) > RANK.get(need.get(scope, "none"), 0):
                need[scope] = level

    for job in (doc.get("jobs") or {}).values():
        if not isinstance(job, dict):
            continue
        # job-level REPLACES workflow-level; absent means inherit
        own = as_perm_map(job.get("permissions"))
        merge(own if own is not None else top)
        uses = job.get("uses")
        nested = local_callee(uses, ref) if isinstance(uses, str) else None
        if nested is not None and (nested[1] or nested[0].is_file()):
            merge(required_by(*nested, seen))
    return need


def workflow_call_secrets(doc: dict) -> set[str]:
    on = doc.get("on", doc.get(True)) or {}
    if not isinstance(on, dict):
        return set()
    wc = on.get("workflow_call")
    if not isinstance(wc, dict):
        return set()
    declared = wc.get("secrets")
    return set(declared) if isinstance(declared, dict) else set()


def check_secret_consistency() -> None:
    """Flag a caller that omits a secret its SIBLING callers pass to the same callee.

    A secret declared `required: false` may be legitimately omitted, so a bare
    "caller does not pass X" is not a defect and actionlint rightly stays quiet.
    What IS a defect is DISAGREEMENT: when pr-ci.yml and main-ci.yml both pass
    SONAR_TOKEN to sonarcloud.yml and nightly-ci.yml passes nothing, the odd one
    out is an omission, not a decision. That exact case shipped on 2026-08-26 and
    surfaced as SonarCloud's misleading "Not authorized or project not found".
    """
    # callee -> secret -> {passers}, {omitters}
    passers: dict[tuple[str, str], set[str]] = {}
    omitters: dict[tuple[str, str], set[str]] = {}
    for wf in sorted(WORKFLOWS.glob("*.y*ml")):
        doc = load(wf)
        for job_id, job in (doc.get("jobs") or {}).items():
            if not isinstance(job, dict):
                continue
            uses = job.get("uses")
            callee = local_callee(uses) if isinstance(uses, str) else None
            if callee is None:
                continue
            if not callee[1] and not callee[0].is_file():
                continue
            declared = workflow_call_secrets(load(*callee))
            if not declared:
                continue
            passed = job.get("secrets")
            if passed == "inherit":
                continue  # inherit forwards everything
            passed_names = set(passed) if isinstance(passed, dict) else set()
            who = f"{wf.relative_to(ROOT)}:{job_id}"
            for secret in declared:
                key = (str(callee[0].relative_to(ROOT)) + (f"@{callee[1]}" if callee[1] else ""), secret)
                (passers if secret in passed_names else omitters).setdefault(key, set()).add(who)

    for key, missing in sorted(omitters.items()):
        callee, secret = key
        have = passers.get(key)
        if not have:
            continue  # nobody passes it — consistent, so not a disagreement
        failures.append(
            f"{', '.join(sorted(missing))} omits secret '{secret}' when calling {callee}, "
            f"but {', '.join(sorted(have))} passes it. Secrets are NOT inherited through "
            f"workflow_call; the callee receives an empty value."
        )


check_secret_consistency()

for wf in sorted(WORKFLOWS.glob("*.y*ml")):
    doc = load(wf)
    jobs = doc.get("jobs")
    if not isinstance(jobs, dict):
        continue
    top = as_perm_map(doc.get("permissions"))
    for job_id, job in jobs.items():
        if not isinstance(job, dict):
            continue
        uses = job.get("uses")
        callee = local_callee(uses) if isinstance(uses, str) else None
        if callee is None:
            continue  # remote callee: contents not resolvable from the tree
        if not callee[1] and not callee[0].is_file():
            failures.append(f"{wf.relative_to(ROOT)}: job '{job_id}' uses missing workflow {uses}")
            continue
        own = as_perm_map(job.get("permissions"))
        grant = own if own is not None else top
        if grant is None:
            failures.append(
                f"{wf.relative_to(ROOT)}: job '{job_id}' declares no permissions and the "
                f"workflow declares none either — the effective grant is the repository "
                f"default, which cannot be verified here. Declare one explicitly."
            )
            continue
        need = required_by(*callee, frozenset())
        missing = {s: l for s, l in need.items() if not covers(grant, s, l)}
        if missing:
            pretty = ", ".join(f"{s}: {l}" for s, l in sorted(missing.items()))
            failures.append(
                f"{wf.relative_to(ROOT)}: job '{job_id}' under-grants "
                f"{callee[0].relative_to(ROOT)} — missing {{{pretty}}}. "
                f"GitHub fails the WHOLE RUN at startup (startup_failure, zero jobs, no log)."
            )

if failures:
    for line in failures:
        print(f"ERROR: {line}", file=sys.stderr)
    raise SystemExit(1)

print("OK: workflow_call contract holds — permissions are supersets, secrets agree across callers")
PY
