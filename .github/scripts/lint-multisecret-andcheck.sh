#!/usr/bin/env bash
#
# lint-multisecret-andcheck.sh — enforce multi-secret AND-check completeness
# on GitHub Actions workflows (PR-A bundle, plan §5.2; spec Invariant 5).
#
# Background: PR #904 shipped an Apple-signing step gated only on
# APPLE_CERTIFICATE_P12 but not APPLE_CERTIFICATE_PASSWORD. A half-rotated
# secret pair would have failed at runtime with an opaque error. The
# post-#904 pattern is: any step that gates on a multi-secret group must
# AND-check ALL members of that group, so the gate fails closed at
# if-eval time rather than at action runtime.
#
# GitHub Actions does NOT permit the `secrets` context inside `if:`
# directly. Workflows use one of two compatible patterns:
#   (a) `if:` clauses referencing `env.NAME != ''` after declaring an
#       `env:` mapping at job/step level that pipes `${{ secrets.NAME }}`
#       into the env var.
#   (b) `run:` bash that explicitly tests `[[ -n "$NAME" ]]` (or `-z`,
#       inverted) before invoking a signing action.
#
# This linter scans both surfaces and verifies that for every secret
# referenced from a known group, all OTHER members of that group are
# referenced in the same clause/block.
#
# Usage:
#   lint-multisecret-andcheck.sh <file.yml-or-dir/>
#   lint-multisecret-andcheck.sh --groups <path>  (override default)
#   lint-multisecret-andcheck.sh --help
#
# Exits:
#   0 — no violations (or no in-group secret refs)
#   1 — at least one in-group secret ref is missing companion members
#   2 — script error (invalid args, missing file, no Python/PyYAML)
#
# Regex limitations (documented per plan §5.2):
#   * The bash-block scan is regex-based, not AST-based. It does not
#     understand nested conditionals, alternative quoting forms beyond
#     the documented set, or heredoc bodies. A `[[ -n "$X" ]]` inside
#     an `if false; then ... fi` is still treated as a reference.
#   * The scan treats each `run:` block as a flat surface — references
#     in different branches of the same block all count as "co-present."
#     This is acceptable because the gating pattern lives at the top of
#     a `run:`, not inside branches.
#   * The scan does NOT follow `env:` indirection back to `${{
#     secrets.X }}`. If a workflow declares `env: { FOO: '${{
#     secrets.APPLE_CERTIFICATE_P12 }}' }` at job level and then tests
#     `[[ -n "$FOO" ]]`, this linter flags `FOO` as a non-secret. The
#     post-#904 convention is to use the secret name verbatim as the
#     env-var name, which this linter assumes. Workflows that rename
#     secrets via env-mapping must opt-in to that convention or be
#     restructured.
#   * Plain `${NAME}` / `$NAME` *uses* inside `run:` blocks are NOT
#     captured — only explicit *gating* patterns count (`[[ -n/-z
#     "$NAME" ]]`). A `run:` block that writes `${APPLE_API_KEY}` to
#     a file is a USE (data path), not a GATE (control path), and the
#     completeness rule only applies to gates. This narrows the scan
#     to the plan §5.2 intent: "the post-#904 pattern uses bash `[[
#     -n "$VAR" ]]` checks; the lint scans those too."
#
# Reference: [internal]plans/2026-05-11-920-desktop-build-pipeline.md §5.2

set -euo pipefail

usage() {
  awk 'NR==1{next} /^[^#]/{exit} {sub(/^# ?/,""); print}' "$0"
}

# ── Default paths (relative to repo root, resolved against caller's CWD) ──
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEFAULT_GROUPS="${SCRIPT_DIR}/multisecret-groups.json"

GROUPS_FILE="$DEFAULT_GROUPS"
TARGET=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --groups)
      GROUPS_FILE="${2:-}"
      shift 2
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    -*)
      echo "ERROR: unknown flag: $1" >&2
      usage >&2
      exit 2
      ;;
    *)
      if [[ -n "$TARGET" ]]; then
        echo "ERROR: only one positional target accepted (got: $TARGET, $1)" >&2
        exit 2
      fi
      TARGET="$1"
      shift
      ;;
  esac
done

if [[ -z "$TARGET" ]]; then
  echo "ERROR: target file or directory is required" >&2
  usage >&2
  exit 2
fi

if [[ ! -e "$TARGET" ]]; then
  echo "ERROR: target does not exist: $TARGET" >&2
  exit 2
fi

if [[ ! -f "$GROUPS_FILE" ]]; then
  echo "ERROR: groups file does not exist: $GROUPS_FILE" >&2
  exit 2
fi

# ── Pre-flight: tools we depend on ──
# python3 + PyYAML are the only runtime dependencies — jq is NOT used
# (groups.json is parsed by the embedded Python via json.load).
command -v python3 >/dev/null \
  || { echo "ERROR: python3 not found" >&2; exit 2; }

# Verify PyYAML is importable (faster failure than letting the inline
# script error). Suppresses output; only the exit code matters.
python3 -c "import yaml" 2>/dev/null \
  || { echo "ERROR: python3 has no PyYAML module (install: pip install pyyaml)" >&2; exit 2; }

# ── Collect target files ──
# `-not -type l` rejects symlinks so a malicious checkout cannot route
# the scanner to a file outside .github/workflows/ (e.g. ~/.aws/config).
declare -a TARGET_FILES=()
if [[ -d "$TARGET" ]]; then
  while IFS= read -r f; do
    TARGET_FILES+=("$f")
  done < <(find "$TARGET" -type f -not -type l \( -name '*.yml' -o -name '*.yaml' \) | sort)
else
  TARGET_FILES+=("$TARGET")
fi

if [[ ${#TARGET_FILES[@]} -eq 0 ]]; then
  echo "::notice::No .yml/.yaml files found under $TARGET"
  exit 0
fi

# ── Hand off to Python helper ──
# The helper reads each file, walks the YAML for `if:` strings and
# `run:` blocks (both scalars and folded/block scalars), then applies
# the group-completeness check using the groups JSON.
python3 - "$GROUPS_FILE" "${TARGET_FILES[@]}" <<'PYEOF'
import json
import re
import sys
from pathlib import Path

import yaml


def load_groups(path: str) -> dict[str, list[str]]:
    with open(path, encoding="utf-8") as fh:
        data = json.load(fh)
    return data.get("groups", {})


def secret_to_group(groups: dict[str, list[str]]) -> dict[str, str]:
    """Build a reverse index: SECRET_NAME -> group_name."""
    rev: dict[str, str] = {}
    for group_name, members in groups.items():
        for member in members:
            rev[member] = group_name
    return rev


# Regex catalog — GATING patterns only. Each captures the name in group 1.
# Plain bash `${NAME}` / `$NAME` USES are deliberately NOT in this catalog
# (see header §"Regex limitations" — uses are not gates).
RE_ENV_DOT = re.compile(r"env\.([A-Z][A-Z0-9_]*)")
RE_SECRETS_DOT = re.compile(r"secrets\.([A-Z][A-Z0-9_]*)")
# bash: [[ -n "$NAME" ]] / [[ -z "$NAME" ]] / [[ -n "${NAME}" ]] — the
# explicit existence-checks the post-#904 pattern uses. These are
# control-flow gates: the surrounding `if/then/else` block runs based on
# the result. Compare to `${NAME}` in a heredoc body, which is data flow.
RE_BASH_TEST = re.compile(r"\[\[\s+-[nz]\s+\"\$\{?([A-Z][A-Z0-9_]*)\}?\"\s+\]\]")


def extract_referenced_names(text: str) -> set[str]:
    """Collect every CAPITALIZED name *gated on* in the text. Uses are
    intentionally excluded — only control-flow gating patterns count."""
    found: set[str] = set()
    for pattern in (RE_ENV_DOT, RE_SECRETS_DOT, RE_BASH_TEST):
        for m in pattern.finditer(text):
            found.add(m.group(1))
    return found


def walk_steps(node, parent_path: str = ""):
    """Yield (path, kind, text) tuples for every `if:` or `run:` in the
    workflow. `kind` is one of {'if', 'run'}. `path` is a breadcrumb
    for error messages."""
    if isinstance(node, dict):
        for key, value in node.items():
            sub_path = f"{parent_path}.{key}" if parent_path else key
            if key == "if" and isinstance(value, str):
                yield sub_path, "if", value
            elif key == "run" and isinstance(value, str):
                yield sub_path, "run", value
            else:
                yield from walk_steps(value, sub_path)
    elif isinstance(node, list):
        for i, item in enumerate(node):
            yield from walk_steps(item, f"{parent_path}[{i}]")


class YamlParseFailure(Exception):
    """Raised on malformed YAML — caller maps to exit 2 (script error)
    to honor the contract documented in the script header. Distinct
    from policy violations (exit 1)."""


def scan_file(path: Path, secret_index: dict[str, str], groups: dict[str, list[str]]) -> list[str]:
    """Returns a list of human-readable violation strings (empty = clean).
    Raises YamlParseFailure on malformed YAML so the caller can exit 2."""
    try:
        with path.open(encoding="utf-8") as fh:
            doc = yaml.safe_load(fh)
    except yaml.YAMLError as e:
        raise YamlParseFailure(f"{path}: YAML parse error: {e}") from e

    if doc is None:
        return []

    violations: list[str] = []

    for breadcrumb, kind, text in walk_steps(doc):
        names = extract_referenced_names(text)
        # Filter to names that are members of a known group.
        in_group = {n: secret_index[n] for n in names if n in secret_index}
        if not in_group:
            continue

        # For each implicated group, verify every member is referenced.
        implicated_groups = set(in_group.values())
        for group_name in implicated_groups:
            members = groups[group_name]
            missing = [m for m in members if m not in names]
            if missing:
                violations.append(
                    f"{path}:{breadcrumb} ({kind}:) incomplete '{group_name}' "
                    f"AND-check — references {sorted(n for n in names if n in members)} "
                    f"but does not reference {sorted(missing)}"
                )

    return violations


def main(argv: list[str]) -> int:
    groups_path = argv[1]
    file_paths = [Path(p) for p in argv[2:]]

    groups = load_groups(groups_path)
    if not groups:
        # Reachable when the groups file exists (bash pre-flight passed)
        # but has no `groups:` key or an empty mapping. Treated as a
        # script error rather than a policy violation.
        print("ERROR: groups file has no 'groups' key or is empty", file=sys.stderr)
        return 2
    secret_index = secret_to_group(groups)

    all_violations: list[str] = []
    for path in file_paths:
        try:
            all_violations.extend(scan_file(path, secret_index, groups))
        except YamlParseFailure as e:
            # Exit 2 distinct from policy violations (exit 1) per the
            # contract documented in the script header.
            print(f"ERROR: {e}", file=sys.stderr)
            return 2

    if all_violations:
        for v in all_violations:
            print(v, file=sys.stderr)
        print(
            f"\nERROR: {len(all_violations)} multi-secret AND-check violation(s)",
            file=sys.stderr,
        )
        return 1

    return 0


sys.exit(main(sys.argv))
PYEOF
