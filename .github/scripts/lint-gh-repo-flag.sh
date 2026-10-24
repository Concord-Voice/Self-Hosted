#!/usr/bin/env bash
#
# lint-gh-repo-flag.sh — flag `gh` CLI invocations in workflows that lack
# an `--repo` flag AND lack a git checkout context. Root cause of #875:
# without a checkout, the gh CLI cannot infer the repo from cwd and fails
# at runtime with "could not determine which branch / not a git repository".
#
# Algorithm (per workflow file):
#   1. Parse YAML structure.
#   2. For each job, compute `has_checkout_context`:
#         True if the job's own steps include any `actions/checkout@*` step
#         OR if any transitive `needs:` dependency does.
#   3. For each step with a `run:` block, walk its lines and look for `gh`
#      command invocations (after handling backslash line continuations and
#      skipping bash comment lines). For each call, check whether the same
#      logical command contains `--repo`. If neither --repo nor a checkout
#      context is present, flag with file:line.
#
# Pattern P13 from spec:
# [internal]specs/2026-05-14-921-workflow-validation-sandbox-design.md §5.2 / §6.5
#
# Usage:
#   lint-gh-repo-flag.sh <file.yml-or-dir/>
#   lint-gh-repo-flag.sh --help
#
# Exits:
#   0 — every `gh` call is either guarded by `--repo` or has a checkout context
#   1 — at least one unguarded `gh` call detected
#   2 — script error (invalid args, missing dependency, malformed YAML, etc.)

set -euo pipefail

usage() {
  awk 'NR==1{next} /^[^#]/{exit} {sub(/^# ?/,""); print}' "$0"
}

# ─── Argument parsing ─────────────────────────────────────────────────
TARGET=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --help|-h)
      usage
      exit 0
      ;;
    -*)
      echo "ERROR: unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
    *)
      if [[ -n "$TARGET" ]]; then
        echo "ERROR: only one target path may be passed (got '$TARGET' and '$1')" >&2
        exit 2
      fi
      TARGET="$1"
      shift
      ;;
  esac
done

if [[ -z "$TARGET" ]]; then
  echo "ERROR: a target file or directory is required" >&2
  usage >&2
  exit 2
fi

if [[ ! -e "$TARGET" ]]; then
  echo "ERROR: target does not exist: $TARGET" >&2
  exit 2
fi

command -v python3 >/dev/null \
  || { echo "ERROR: python3 not found" >&2; exit 2; }

python3 -c "import yaml" 2>/dev/null \
  || { echo "ERROR: python3 has no PyYAML module (install: pip install pyyaml)" >&2; exit 2; }

# ─── Collect files to scan ────────────────────────────────────────────
FILES=()
if [[ -d "$TARGET" ]]; then
  while IFS= read -r f; do
    FILES+=("$f")
  done < <(find "$TARGET" -type f -not -type l \( -name '*.yml' -o -name '*.yaml' \) | sort)
else
  FILES+=("$TARGET")
fi

if [[ "${#FILES[@]}" -eq 0 ]]; then
  echo "ERROR: no YAML files found under $TARGET" >&2
  exit 2
fi

# ─── Python driver ────────────────────────────────────────────────────
# All collected files are passed as positional args. The driver iterates
# and aggregates exit status: structural failures (parse errors) escalate
# to exit 2; unguarded gh calls produce exit 1; clean run is exit 0.
python3 - "${FILES[@]}" <<'PYEOF'
import re
import sys
from pathlib import Path

import yaml


# ─── Structural-anomaly tracking (exit 2 escalation, analogous to G2's
#     _PARSE_FAILURES). Tracks workflow files under .github/workflows/
#     that parse but don't have the expected shape ({jobs: <mapping>}).
#     Silent-clean on those files would erase signal that a workflow's
#     shape is invalid — which is the failure mode this lint exists to
#     prevent, just one level up the abstraction stack.
_STRUCTURAL_ANOMALIES: list = []


# ─── Bug regex ───────────────────────────────────────────────────────
# Match `gh` as a command (preceded by start-of-string, whitespace, or a
# shell-grouping character that introduces a new word). `\w` after the
# space ensures we matched `gh <subcommand>` rather than a stray "gh"
# token like a variable name. The leading set is non-capturing.
GH_CALL = re.compile(r"(?:^|[\s;|&()`])gh\s+\w")


# ─── YAML helpers ────────────────────────────────────────────────────
def find_key(map_node: yaml.MappingNode, key: str):
    """Return the value-node for `key` in a MappingNode, or None."""
    if not isinstance(map_node, yaml.MappingNode):
        return None
    for k_node, v_node in map_node.value:
        if isinstance(k_node, yaml.ScalarNode) and k_node.value == key:
            return v_node
    return None


def step_has_checkout(step_node) -> bool:
    """A step has checkout if its `uses:` value starts with actions/checkout@."""
    if not isinstance(step_node, yaml.MappingNode):
        return False
    uses = find_key(step_node, "uses")
    if not isinstance(uses, yaml.ScalarNode):
        return False
    return uses.value.startswith("actions/checkout@")


def job_has_checkout(job_node) -> bool:
    """A job has checkout if any of its steps does."""
    if not isinstance(job_node, yaml.MappingNode):
        return False
    steps = find_key(job_node, "steps")
    if not isinstance(steps, yaml.SequenceNode):
        return False
    return any(step_has_checkout(s) for s in steps.value)


def job_needs(job_node) -> list[str]:
    """Return the list of job-keys this job needs. Handles both string-
    scalar (single dependency) and sequence-of-scalars (multi) forms."""
    needs = find_key(job_node, "needs")
    if isinstance(needs, yaml.ScalarNode) and isinstance(needs.value, str):
        return [needs.value]
    if isinstance(needs, yaml.SequenceNode):
        return [n.value for n in needs.value
                if isinstance(n, yaml.ScalarNode) and isinstance(n.value, str)]
    return []


def transitive_needs(job_key: str, jobs: dict, visited=None) -> set:
    """Set of all transitive `needs:` dependencies for the given job.
    Cycle-safe via `visited`."""
    if visited is None:
        visited = set()
    if job_key in visited:
        return set()
    visited.add(job_key)
    job_node = jobs.get(job_key)
    if job_node is None:
        return set()
    direct = job_needs(job_node)
    result = set(direct)
    for dep in direct:
        result.update(transitive_needs(dep, jobs, visited))
    return result


# ─── Run-block scanning ──────────────────────────────────────────────
def join_continuations(lines: list[str]) -> list[tuple[int, str]]:
    """Collapse trailing backslash line-continuations into one logical
    command per entry. Returns [(start_line_offset, joined_command), ...]
    where start_line_offset is the 0-indexed offset of the first physical
    line within `lines`.

    Comment lines (those whose first non-whitespace char is `#`) are NEVER
    joined into a multi-line command, even when they end with `\\`. Bash
    treats `#` through `\\n` as a comment regardless of any trailing `\\`
    — the comment ends at the newline. Pre-fix, this function greedily
    joined backslashed-comments with the next physical line, silently
    swallowing real `gh` calls into the comment text and exiting clean
    on a bug the lint was meant to flag. The fix below restores the
    bash-semantic invariant: comment lines are NOT continuation sources."""
    cmds: list[tuple[int, str]] = []
    i = 0
    n = len(lines)
    while i < n:
        start = i
        cmd = lines[i]
        is_comment_line = cmd.lstrip().startswith("#")
        if not is_comment_line:
            while cmd.rstrip().endswith("\\") and i + 1 < n:
                # Strip trailing backslash + join next line with single space.
                cmd = cmd.rstrip()[:-1] + " " + lines[i + 1]
                i += 1
        cmds.append((start, cmd))
        i += 1
    return cmds


def scan_run_block(content: str, file_line_1based: int) -> list[tuple[int, str]]:
    """Return list of (file_line_1based, command_text) for gh calls in this
    run block that lack `--repo`. Bash comment lines (those whose first
    non-whitespace is `#`) are skipped."""
    findings: list[tuple[int, str]] = []
    lines = content.split("\n")
    # Drop a trailing empty line introduced by literal-block scalars
    # ending with a final newline. Otherwise the line count is one more
    # than the source had, which throws off the file-line offset for the
    # last continuation chain.
    if lines and lines[-1] == "":
        lines = lines[:-1]

    for offset, cmd in join_continuations(lines):
        stripped = cmd.lstrip()
        if stripped.startswith("#"):
            continue  # bash comment line
        if not GH_CALL.search(cmd):
            continue
        if "--repo" in cmd:
            continue
        findings.append((file_line_1based + offset, cmd.strip()))
    return findings


def run_block_first_line(run_value_node) -> int:
    """1-indexed file line of the first content line in a run: block.
    For block-style scalars (`|`, `>`), content starts on the line AFTER
    the `run:` line. For inline scalars (no style or quoted on same line),
    content is on the same line as `run:`."""
    if not isinstance(run_value_node, yaml.ScalarNode):
        return -1
    base = run_value_node.start_mark.line  # 0-indexed
    if run_value_node.style in ("|", ">"):
        return base + 2  # 0-indexed → +1, plus one more since block content starts on next line
    return base + 1  # inline scalar, same line as run:


# ─── Driver ──────────────────────────────────────────────────────────
def scan_file(path: Path) -> int:
    """Returns 0 (clean), 1 (drift detected), or 2 (script error)."""
    try:
        with path.open(encoding="utf-8") as fh:
            doc = yaml.compose(fh)
    except yaml.YAMLError as e:
        print(f"ERROR: cannot parse {path}: {e}", file=sys.stderr)
        return 2
    except (OSError, UnicodeDecodeError) as e:
        print(f"ERROR: cannot read {path}: {e}", file=sys.stderr)
        return 2

    if not isinstance(doc, yaml.MappingNode):
        # Structural anomaly: a workflow file under .github/workflows/ whose
        # root isn't a YAML mapping. Track for escalation in main() — don't
        # silently return 0 (that would mask the malformation).
        _STRUCTURAL_ANOMALIES.append(path)
        print(
            f"WARN: {path}: root is not a YAML mapping (workflow files must "
            "be mappings); marking as structural anomaly",
            file=sys.stderr,
        )
        return 0  # tracked; main() will escalate to exit 2

    jobs_node = find_key(doc, "jobs")
    if not isinstance(jobs_node, yaml.MappingNode):
        # Structural anomaly: no `jobs:` mapping. Valid GHA workflows always
        # have a top-level `jobs:` map (including reusable workflows). An
        # `action.yml` composite uses `runs:` and lives outside workflows/,
        # so it shouldn't reach this scanner. Track for exit-2 escalation.
        _STRUCTURAL_ANOMALIES.append(path)
        print(
            f"WARN: {path}: no top-level 'jobs:' mapping found "
            "(valid GHA workflows always have one); marking as structural anomaly",
            file=sys.stderr,
        )
        return 0  # tracked; main() will escalate to exit 2

    # Build job_key → MappingNode dict
    jobs: dict[str, yaml.MappingNode] = {}
    for k_node, v_node in jobs_node.value:
        if isinstance(k_node, yaml.ScalarNode) and isinstance(v_node, yaml.MappingNode):
            jobs[k_node.value] = v_node

    # Precompute the set of jobs that themselves contain `actions/checkout`.
    self_checkout: set[str] = {jk for jk, jn in jobs.items() if job_has_checkout(jn)}

    drift = 0
    for job_key, job_node in jobs.items():
        # has_checkout_context: own steps OR any transitive `needs:` job.
        deps = transitive_needs(job_key, jobs)
        has_context = (job_key in self_checkout) or any(d in self_checkout for d in deps)

        steps = find_key(job_node, "steps")
        if not isinstance(steps, yaml.SequenceNode):
            continue
        for step in steps.value:
            if not isinstance(step, yaml.MappingNode):
                continue
            run_value = find_key(step, "run")
            if not isinstance(run_value, yaml.ScalarNode):
                continue
            first_line = run_block_first_line(run_value)
            findings = scan_run_block(run_value.value, first_line)
            for line_no, cmd_text in findings:
                if has_context:
                    continue  # checkout in own job or upstream `needs:` job
                # Truncate the command for the error message to keep stderr readable.
                snippet = cmd_text if len(cmd_text) <= 120 else cmd_text[:117] + "..."
                print(
                    f"{path}:{line_no}: gh CLI invocation lacks --repo in job '{job_key}' "
                    f"which has no actions/checkout in own steps or `needs:` cascade: {snippet}",
                    file=sys.stderr,
                )
                drift = 1

    return drift


def main(argv: list[str]) -> int:
    paths = [Path(a) for a in argv[1:]]
    exit_code = 0
    for p in paths:
        rc = scan_file(p)
        if rc == 2:
            return 2  # any script error short-circuits
        if rc == 1:
            exit_code = 1

    # Structural anomalies escalate to exit 2 BEFORE the drift report.
    # Mirrors the G2 (#999) sibling lint's _PARSE_FAILURES / _MISSING_CALLEES
    # handling in lint-branch-protection.sh — script errors win over drift
    # because we can't trust a "clean" verdict on a malformed input.
    if _STRUCTURAL_ANOMALIES:
        n = len(_STRUCTURAL_ANOMALIES)
        print(
            f"\nERROR: {n} structural anomaly/anomalies — script error, not drift. "
            "Fix the workflow file shape(s) above before trusting any "
            "context-resolution result.",
            file=sys.stderr,
        )
        return 2

    if exit_code == 1:
        print("", file=sys.stderr)
        print(
            "ERROR: one or more `gh` CLI calls lack --repo in a no-checkout context. "
            "Either add `actions/checkout` upstream or pass `--repo $GITHUB_REPOSITORY`.",
            file=sys.stderr,
        )
    return exit_code


sys.exit(main(sys.argv))
PYEOF
