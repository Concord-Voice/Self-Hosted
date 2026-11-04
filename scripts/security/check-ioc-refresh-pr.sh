#!/usr/bin/env bash
# check-ioc-refresh-pr.sh — guarded auto-merge gate for Shai-Hulud IOC refresh PRs.
#
# Exit codes:
#   0  clean small additions-only delta; auto-merge may be requested
#   1  anomaly; leave PR for security review
#   2  package set unchanged, when caller passes --noop-exit-code 2
#   10 internal/tooling error

set -euo pipefail

BASE_REF="${BASE_REF:-}"
HEAD_FILE="scripts/security/iocs/shai-hulud-2.0.txt"
MAX_DELTA="100"
SKIP_PROVENANCE="false"
NOOP_EXIT_CODE="${NOOP_EXIT_CODE:-1}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --base-ref)
      BASE_REF="${2:-}"
      shift 2
      ;;
    --head-file)
      HEAD_FILE="${2:-}"
      shift 2
      ;;
    --max-delta)
      MAX_DELTA="${2:-}"
      shift 2
      ;;
    --skip-provenance)
      SKIP_PROVENANCE="true"
      shift
      ;;
    --noop-exit-code)
      NOOP_EXIT_CODE="${2:-}"
      shift 2
      ;;
    *)
      echo "unknown arg: $1" >&2
      exit 10
      ;;
  esac
done

if [[ -z "$BASE_REF" ]]; then
  echo "--base-ref is required" >&2
  exit 10
fi

python3 - "$BASE_REF" "$HEAD_FILE" "$MAX_DELTA" "$SKIP_PROVENANCE" "$NOOP_EXIT_CODE" <<'PY'
import csv
import hashlib
import json
import os
import re
import subprocess
import sys
import tempfile
import urllib.request
from pathlib import Path

base_ref, head_file, max_delta_raw, skip_raw, noop_exit_raw = sys.argv[1:6]
head_path = Path(head_file)
skip_provenance = skip_raw == "true"
try:
    max_delta = int(max_delta_raw)
except ValueError:
    print(f"invalid --max-delta: {max_delta_raw}", file=sys.stderr)
    sys.exit(10)
try:
    noop_exit_code = int(noop_exit_raw)
except ValueError:
    print(f"invalid --noop-exit-code: {noop_exit_raw}", file=sys.stderr)
    sys.exit(10)
if not 1 <= noop_exit_code <= 255:
    print(f"invalid --noop-exit-code: {noop_exit_raw}", file=sys.stderr)
    sys.exit(10)

IOC_PATH = "scripts/security/iocs/shai-hulud-2.0.txt"
ALLOWLIST = Path("scripts/security/iocs/false-positive-allowlist.txt")
# Every tracked npm lockfile in this repository, so the list cannot go stale.
LOCKFILES = [
    Path(p)
    for p in subprocess.run(
        ["git", "ls-files", "*package-lock.json"], capture_output=True, text=True, check=True
    ).stdout.split()
]
CSV_RELATIVE = "shai-hulud-2.0/shai-hulud-2.0.csv"
RAW_URL = "https://raw.githubusercontent.com/DataDog/indicators-of-compromise/{commit}/" + CSV_RELATIVE
PKG_RE = re.compile(r"^(?:@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._~-]*$")
SHA_RE = re.compile(r"^# SHA256 of source CSV:\s*([0-9a-fA-F]{64})\s*$", re.MULTILINE)
COMMIT_RE = re.compile(r"^# Source commit:\s*([0-9a-fA-F]{40}|main)\s*$", re.MULTILINE)


def die_internal(message: str) -> None:
    print(f"[ERROR] {message}", file=sys.stderr)
    sys.exit(10)


def git_show(ref: str, path: str) -> str:
    proc = subprocess.run(
        ["git", "show", f"{ref}:{path}"],
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    if proc.returncode != 0:
        die_internal(f"unable to read {path} at {ref}: {proc.stderr.strip()}")
    return proc.stdout


def names_from_text(text: str):
    return [line.strip() for line in text.splitlines() if line.strip() and not line.startswith("#")]


def parse_ioc_text(text: str, *, strict_headers: bool):
    failures = []
    sha_match = SHA_RE.search(text)
    commit_match = COMMIT_RE.search(text)
    sha = sha_match.group(1).lower() if sha_match else ""
    source_commit = commit_match.group(1) if commit_match else ""

    if strict_headers and not sha:
        failures.append("missing or invalid SHA256 header")
    if strict_headers and not source_commit:
        failures.append("missing or invalid Source commit header")
    if strict_headers and not skip_provenance and not re.fullmatch(r"[0-9a-fA-F]{40}", source_commit):
        failures.append("Source commit must be a pinned 40-hex commit")

    names = names_from_text(text)
    if not names:
        failures.append("body is empty")
    invalid = [name for name in names if not PKG_RE.fullmatch(name)]
    if invalid:
        failures.append("invalid package names: " + ", ".join(invalid[:10]))
    if names != sorted(names):
        failures.append("body is not sorted")
    seen = set()
    duplicates = []
    for name in names:
        if name in seen:
            duplicates.append(name)
        seen.add(name)
    if duplicates:
        failures.append("body contains duplicate package names: " + ", ".join(sorted(set(duplicates))[:10]))
    return sha, source_commit, names, failures


def load_allowlist() -> set[str]:
    if not ALLOWLIST.exists():
        return set()
    allowed = set()
    for line in ALLOWLIST.read_text().splitlines():
        name = line.split("#", 1)[0].strip()
        if name:
            allowed.add(name)
    return allowed


def package_names_from_lockfile(path: Path) -> set[str]:
    if not path.exists():
        return set()
    try:
        data = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError) as exc:
        die_internal(f"unable to parse lockfile {path}: {exc}")
    names = set()
    for key, entry in data.get("packages", {}).items():
        if key == "":
            continue
        idx = key.rfind("node_modules/")
        if idx >= 0:
            name = key[idx + len("node_modules/"):]
        else:
            name = entry.get("name") or key
        if name:
            names.add(name)
    return names


def csv_names(csv_bytes: bytes) -> list[str]:
    decoded = csv_bytes.decode("utf-8")
    reader = csv.DictReader(decoded.splitlines())
    fieldnames = reader.fieldnames or []
    column = None
    for candidate in ("package_name", "package"):
        if candidate in fieldnames:
            column = candidate
            break
    if column is None:
        die_internal(f"upstream CSV missing package_name/package column: {fieldnames}")
    return sorted({(row.get(column) or "").strip() for row in reader if (row.get(column) or "").strip()})


def run_provenance_checks(sha: str, source_commit: str, head_names: list[str], head_text: str, failures: list[str]) -> None:
    url = RAW_URL.format(commit=source_commit)
    try:
        with urllib.request.urlopen(url, timeout=30) as response:
            csv_bytes = response.read()
    except Exception as exc:  # network failures should fail the gate loudly
        die_internal(f"unable to fetch pinned upstream CSV {url}: {exc}")

    actual_sha = hashlib.sha256(csv_bytes).hexdigest()
    if actual_sha != sha:
        failures.append(f"pinned CSV SHA256 mismatch: header {sha}, fetched {actual_sha}")

    upstream_names = csv_names(csv_bytes)
    if upstream_names != head_names:
        failures.append("committed package set does not match pinned upstream CSV")

    with tempfile.TemporaryDirectory() as td:
        tmp = Path(td)
        csv_path = tmp / "source.csv"
        sha_path = tmp / "source.sha256"
        out_path = tmp / "out.txt"
        csv_path.write_bytes(csv_bytes)
        sha_path.write_text(f"{actual_sha}  {csv_path}\n")
        env = os.environ.copy()
        env["OUT"] = str(out_path)
        proc = subprocess.run(
            ["bash", "scripts/security/regenerate-ioc-list.sh", str(csv_path), str(sha_path), source_commit],
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
        )
        if proc.returncode != 0:
            die_internal(f"regenerator failed during byte-identical check: {proc.stderr.strip() or proc.stdout.strip()}")
        generated = out_path.read_text()
        if generated != head_text:
            failures.append("regenerated IOC list is not byte-identical to committed file")


if not head_path.exists():
    die_internal(f"head file missing: {head_path}")

head_text = head_path.read_text()
base_text = git_show(base_ref, IOC_PATH)
sha, source_commit, head_names, failures = parse_ioc_text(head_text, strict_headers=True)
_, _, base_names, base_parse_failures = parse_ioc_text(base_text, strict_headers=False)
if base_parse_failures:
    die_internal("base IOC list is structurally invalid: " + "; ".join(base_parse_failures))

head_set = set(head_names)
base_set = set(base_names)
additions = sorted(head_set - base_set)
removals = sorted(base_set - head_set)

package_set_unchanged = not additions and not removals
if package_set_unchanged:
    failures.append("package set is unchanged")
if removals:
    failures.append("removals require security review")
if len(additions) > max_delta:
    failures.append(f"additions {len(additions)} exceeds max {max_delta}")
if len(removals) > max_delta:
    failures.append(f"removals {len(removals)} exceeds max {max_delta}")

allowlist = load_allowlist()
collisions = []
for lockfile in LOCKFILES:
    packages = package_names_from_lockfile(lockfile)
    for name in additions:
        if name in packages and name not in allowlist:
            collisions.append(f"{name} in {lockfile}")
if collisions:
    failures.append("lockfile collision: " + "; ".join(collisions[:20]))

if not skip_provenance and not failures:
    run_provenance_checks(sha, source_commit, head_names, head_text, failures)

print("# IOC refresh guard")
print(f"Guard result: {'fail' if failures else 'pass'}")
print(f"Base ref: {base_ref}")
print(f"Head file: {head_file}")
print(f"Additions: {len(additions)}")
print(f"Removals: {len(removals)}")
if additions:
    print("Added names:")
    for name in additions[:50]:
        print(f"- {name}")
    if len(additions) > 50:
        print(f"- ... {len(additions) - 50} more")
if removals:
    print("Removed names:")
    for name in removals[:50]:
        print(f"- {name}")
    if len(removals) > 50:
        print(f"- ... {len(removals) - 50} more")
if failures:
    print("Failures:")
    for failure in failures:
        print(f"- {failure}")
    if package_set_unchanged and len(failures) == 1:
        sys.exit(noop_exit_code)
    sys.exit(1)
print("Failures: none")
sys.exit(0)
PY
