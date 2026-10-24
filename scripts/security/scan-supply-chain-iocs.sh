#!/usr/bin/env bash
# scan-supply-chain-iocs.sh — Shai-Hulud 2.0 supply-chain scanner.
#
# Subchecks (all run together unless a --*-only flag gates them):
#   1. Working-tree walk for bun_environment.js / setup_bun.js (always runs).
#   2. IOC-list match against lockfiles.
#   3. Install-script diff against the base commit (skips gracefully if the
#      base ref cannot be resolved).
# NOTE: Spec §6 proposes a short-circuit that skips subchecks 2+3 when the
# lockfiles are unchanged since the base ref; that optimization is tracked
# as a follow-up and is not implemented here. All lockfile subchecks run
# unconditionally in the current implementation.
#
# Exit codes:
#   0  — clean pass or only subcheck-3 warnings fired.
#   2  — payload file found.
#   3  — IOC match found.
#   10 — internal error (missing IOC list, malformed IOC/allowlist file,
#        malformed lockfile JSON, etc.).
#
# Flags:
#   --working-tree-only   Run subcheck 1 only.
#   --lockfile-only       Run subchecks 2+3 only.
#   --include-node-modules  Include node_modules/ in subcheck 1's walk.
#
# Assumes $PWD is the repo root (or the root of a test fixture tree).
# Runbook: [internal]supply-chain-defense.md. Spec: #715.

set -euo pipefail

# ── Flag parsing ────────────────────────────────────────────────────────────
MODE="all"                     # "all" | "working-tree-only" | "lockfile-only"
INCLUDE_NODE_MODULES="false"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --working-tree-only) MODE="working-tree-only"; shift;;
    --lockfile-only)     MODE="lockfile-only"; shift;;
    --include-node-modules) INCLUDE_NODE_MODULES="true"; shift;;
    *) echo "unknown arg: $1" >&2; exit 10;;
  esac
done

# ── Subcheck 1: payload-file walk ───────────────────────────────────────────
run_subcheck_1() {
  local exclude_args=(
    -not -path './.git/*'
    -not -path './dist/*'
    -not -path './out/*'
    -not -path './coverage/*'
    -not -path './scripts/security/tests/*'
    -not -path './scripts/security/iocs/*'
  )
  if [[ "$INCLUDE_NODE_MODULES" != "true" ]]; then
    exclude_args+=(-not -path './node_modules/*')
  fi

  local matches
  matches=$(find . \( -type f -o -type l \) \
    \( -name bun_environment.js -o -name setup_bun.js \) \
    "${exclude_args[@]}" 2>/dev/null || true)

  if [[ -n "$matches" ]]; then
    while IFS= read -r path; do
      echo "[FAIL] Shai-Hulud payload file: $path"
    done <<< "$matches"
    return 2
  fi
  return 0
}

# ── Subcheck 2: IOC-list match in lockfiles ─────────────────────────────────
# Self-Hosted: the root lockfile (packages/*) takes the desktop slot; the
# concord-bridge workspace stays with Desktop.
LOCKFILES=(
  "package-lock.json"
  "services/media-plane/package-lock.json"
  "client/admin/package-lock.json"
  ".github/tools/license-checker/package-lock.json"
)
# Pass inline Python with -c instead of stdin: Bash can deadlock a heredoc
# writer when the parser's command-substitution output fills its pipe.
# The required PR workflow supplies these from its protected workflow checkout;
# ordinary/local scans keep the repository-relative policy files.
IOC_LIST="${IOC_LIST_PATH:-scripts/security/iocs/shai-hulud-2.0.txt}"
FP_ALLOWLIST="${IOC_ALLOWLIST_PATH:-scripts/security/iocs/false-positive-allowlist.txt}"

# Load names from a list file. Strips comments (# and beyond) and blank lines.
# Echoes one name per line.
load_names() {
  local file="$1"
  [[ -f "$file" ]] || return 0
  awk 'BEGIN{FS="#"} {
    gsub(/^[[:space:]]+|[[:space:]]+$/, "", $1)
    if ($1 != "") print $1
  }' "$file"
}

run_subcheck_2() {
  if [[ ! -f "$IOC_LIST" ]]; then
    echo "[ERROR] vendored IOC list missing: $IOC_LIST" >&2
    return 10
  fi

  local ioc_names fp_names
  ioc_names=$(load_names "$IOC_LIST" | sort -u)
  fp_names=$(load_names "$FP_ALLOWLIST" | sort -u)

  if [[ -z "$ioc_names" ]]; then
    echo "[ERROR] vendored IOC list is empty: $IOC_LIST" >&2
    return 10
  fi

  # For each allowlisted name present in the IOC list, emit an INFO line.
  if [[ -n "$fp_names" ]]; then
    local allowlisted_iocs
    allowlisted_iocs=$(comm -12 <(printf '%s\n' "$ioc_names") <(printf '%s\n' "$fp_names"))
    while IFS= read -r name; do
      [[ -n "$name" ]] || continue
      local rationale
      # Match the allowlist entry exactly so "foo" cannot pick up "foobar".
      rationale=$(awk -v n="$name" '
        {
          line = $0
          entry = line
          sub(/[[:space:]]*#.*/, "", entry)
          gsub(/^[[:space:]]+|[[:space:]]+$/, "", entry)
          if (entry == n) {
            if (index(line, "#")) {
              sub(/^[^#]*#[[:space:]]*/, "", line)
              print line
            }
            exit
          }
        }
      ' "$FP_ALLOWLIST")
      if [[ -n "$rationale" ]]; then
        echo "[INFO] FP-allowlisted: $name — $rationale"
      else
        echo "[INFO] FP-allowlisted: $name"
      fi
    done <<< "$allowlisted_iocs"
  fi

  local effective_iocs
  effective_iocs=$(comm -23 <(echo "$ioc_names") <(echo "$fp_names"))

  local found=0
  for lockfile in "${LOCKFILES[@]}"; do
    [[ -f "$lockfile" ]] || continue
    # Extract installed package names from the lockfile.
    local lockfile_pkgs
    local pkg_rc=0
    lockfile_pkgs=$(python3 -c '
import json, sys
path = sys.argv[1]
try:
    with open(path) as f:
        data = json.load(f)
except (OSError, json.JSONDecodeError) as e:
    print(f"[ERROR] unable to parse lockfile {path}: {e}", file=sys.stderr)
    sys.exit(10)
if (not isinstance(data, dict) or data.get("lockfileVersion") not in (2, 3)
        or not isinstance(data.get("packages"), dict)):
    print(f"[ERROR] unsupported lockfile schema: {path}", file=sys.stderr)
    sys.exit(10)
names = set()
# Key forms seen in npm lockfiles:
#   "node_modules/lodash"                              → lodash
#   "node_modules/@babel/core"                         → @babel/core
#   "node_modules/@babel/core/node_modules/semver"     → semver   (nested dep)
#   "node_modules/foo/node_modules/@bar/baz"           → @bar/baz (nested scoped)
# Strategy: take the substring after the LAST "node_modules/" — that gives
# the final package identifier, correctly including any scope prefix.
for key, entry in data["packages"].items():
    if not isinstance(key, str) or not isinstance(entry, dict):
        print(f"[ERROR] invalid package entry in {path}", file=sys.stderr)
        sys.exit(10)
    declared_name = entry.get("name")
    if declared_name is not None and not isinstance(declared_name, str):
        print(f"[ERROR] invalid package name in {path}", file=sys.stderr)
        sys.exit(10)
    if key == "":
        continue
    idx = key.rfind("node_modules/")
    if idx >= 0:
        name = key[idx + len("node_modules/"):]
    else:
        name = declared_name or key
    if name:
        names.add(name)
    # npm aliases install under the key path while entry.name names the actual
    # package. Either identity may appear in the IOC list.
    if declared_name:
        names.add(declared_name)
for n in sorted(names):
    print(n)
' "$lockfile"
) || pkg_rc=$?
    # Any parser failure blocks; an unrecognized schema must not look clean.
    if [[ $pkg_rc -ne 0 ]]; then
      return 10
    fi
    local matching_iocs
    matching_iocs=$(comm -12 <(printf '%s\n' "$effective_iocs") <(printf '%s\n' "$lockfile_pkgs"))
    while IFS= read -r pkg; do
      [[ -n "$pkg" ]] || continue
      # Look up the version from the lockfile.
      local version
      version=$(python3 -c '
import json, sys
try:
    with open(sys.argv[1]) as f:
        data = json.load(f)
except (OSError, json.JSONDecodeError):
    # Subcheck 2 already surfaced the parse error via the names extractor;
    # here we just fall through to "unknown" so the FAIL line still prints
    # with the package name even if version lookup failed.
    sys.exit(0)
target = sys.argv[2]
for key, entry in data.get("packages", {}).items():
    if key == "":
        continue
    idx = key.rfind("node_modules/")
    key_name = key[idx + len("node_modules/"):] if idx >= 0 else (entry.get("name") or key)
    if key_name == target or entry.get("name") == target:
        print(entry.get("version", "unknown"))
        break
' "$lockfile" "$pkg"
)
      echo "[FAIL] IOC match: ${pkg}@${version:-unknown} in $lockfile"
      found=1
    done <<< "$matching_iocs"
  done

  if [[ $found -eq 1 ]]; then
    return 3
  fi
  return 0
}

# ── Subcheck 3: new install-script diff ─────────────────────────────────────
# Resolves a base ref to diff against. Echoes the ref on stdout if resolvable,
# otherwise echoes an empty string.
resolve_diff_base() {
  if [[ -n "${PRE_COMMIT_FROM_REF:-}" && -n "${PRE_COMMIT_TO_REF:-}" ]]; then
    echo "$PRE_COMMIT_FROM_REF"
    return
  fi
  if [[ -n "${GITHUB_BASE_REF:-}" ]]; then
    if git rev-parse --verify --quiet "origin/${GITHUB_BASE_REF}" >/dev/null; then
      echo "origin/${GITHUB_BASE_REF}"
      return
    fi
    if git rev-parse --verify --quiet "$GITHUB_BASE_REF" >/dev/null; then
      echo "$GITHUB_BASE_REF"
      return
    fi
  fi
  # Explicit caller-supplied base (ioc-scanning.yml sets github.event.before
  # on push events — ADR-0018 / #1472). DELIBERATELY ranked BELOW
  # GITHUB_BASE_REF: on pull_request synchronize events github.event.before
  # is the PREVIOUS HEAD SHA, and diffing against it instead of the PR base
  # would hide an install script added in an earlier push of the same PR.
  # The PR-base semantic ("what does this PR introduce vs main") always wins
  # when available; the override only serves base-ref-less events (push).
  # Validated before trusting: all-zeros on branch creation, may be
  # unresolvable after force-pushes.
  if [[ -n "${IOC_DIFF_BASE:-}" && "${IOC_DIFF_BASE}" != "0000000000000000000000000000000000000000" ]]; then
    if git rev-parse --verify --quiet "${IOC_DIFF_BASE}^{commit}" >/dev/null; then
      echo "$IOC_DIFF_BASE"
      return
    fi
  fi
  if git rev-parse --verify --quiet "HEAD~1" >/dev/null 2>&1; then
    echo "HEAD~1"
    return
  fi
  echo ""
}

# Extract (package, script-key, command) triples from a lockfile at a given git ref.
# If ref is empty, reads from the working tree (filesystem).
extract_install_scripts() {
  local ref="$1" lockfile="$2"
  local content_file
  content_file=$(mktemp)
  trap "rm -f $content_file" RETURN
  if [[ -n "$ref" ]]; then
    git show "${ref}:${lockfile}" > "$content_file" 2>/dev/null || return 0
  else
    [[ -f "$lockfile" ]] || return 0
    cat "$lockfile" > "$content_file"
  fi
  [[ -s "$content_file" ]] || return 0
  python3 -c '
import json, sys
try:
    with open(sys.argv[1]) as f:
        data = json.load(f)
except Exception:
    sys.exit(0)
for key, entry in data.get("packages", {}).items():
    if key == "":
        continue
    # Use rfind so nested keys like node_modules/@babel/core/node_modules/semver
    # yield "semver" rather than "@babel/core/node_modules/semver".
    idx = key.rfind("node_modules/")
    name = key[idx + len("node_modules/"):] if idx >= 0 else (entry.get("name") or key)
    scripts = entry.get("scripts") or {}
    for sk in ("preinstall", "postinstall", "install"):
        if sk in scripts:
            # json.dumps gives a guaranteed single-line, tab-free literal
            # for the command value. npm allows multi-line JSON strings
            # (embedded \n), which would otherwise split the TSV output
            # across rows and let a multi-line poisoned preinstall script
            # slip past the downstream `comm` / `read` parser. See #722.
            cmd_literal = json.dumps(scripts[sk])
            print(f"{name}\t{sk}\t{cmd_literal}")
' "$content_file"
}

run_subcheck_3() {
  local base
  base=$(resolve_diff_base)

  if [[ -z "$base" ]]; then
    echo "[INFO] Install-script diff skipped: no base commit resolvable"
    return 0
  fi

  for lockfile in "${LOCKFILES[@]}"; do
    [[ -f "$lockfile" ]] || continue

    local base_triples head_triples new_triples
    base_triples=$(extract_install_scripts "$base" "$lockfile")
    head_triples=$(extract_install_scripts "" "$lockfile")

    new_triples=$(comm -23 \
      <(echo "$head_triples" | sort -u) \
      <(echo "$base_triples" | sort -u))

    while IFS=$'\t' read -r pkg script_key cmd; do
      [[ -n "$pkg" ]] || continue
      local version
      version=$(python3 -c '
import json, sys
try:
    with open(sys.argv[1]) as f:
        data = json.load(f)
except (OSError, json.JSONDecodeError):
    sys.exit(0)
target = sys.argv[2]
for key, entry in data.get("packages", {}).items():
    if key == "":
        continue
    idx = key.rfind("node_modules/")
    kn = key[idx + len("node_modules/"):] if idx >= 0 else (entry.get("name") or key)
    if kn == target:
        print(entry.get("version", "unknown"))
        break
' "$lockfile" "$pkg"
)
      # ${cmd} is a JSON-encoded literal (always includes its own surrounding
      # quotes) produced by extract_install_scripts — see the json.dumps call
      # there. Do NOT double-wrap in quotes here.
      echo "[WARN] New install script: ${pkg}@${version:-unknown} has ${script_key} = ${cmd} in $lockfile"
    done <<< "$new_triples"
  done

  return 0
}

# ── Entry point ─────────────────────────────────────────────────────────────
FINAL_EXIT=0

if [[ "$MODE" == "all" || "$MODE" == "working-tree-only" ]]; then
  if ! run_subcheck_1; then
    FINAL_EXIT=2
  fi
fi

if [[ "$MODE" == "all" || "$MODE" == "lockfile-only" ]]; then
  set +e
  run_subcheck_2
  rc=$?
  set -e
  if [[ $rc -eq 10 ]]; then
    exit 10
  elif [[ $rc -eq 3 ]]; then
    FINAL_EXIT=3
  fi

  run_subcheck_3  # Warn-only.
fi

exit $FINAL_EXIT
