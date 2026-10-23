#!/bin/bash
# scripts/test-update-notice-md.sh
#
# Test fixture for scripts/update-notice-md.py.
# Run manually: ./scripts/test-update-notice-md.sh
# Exits non-zero if any case fails.
#
# Mirrors the harness pattern from scripts/test-check-label-references.sh.
#
# Hermetic: the script under test derives its repo root from its own location
# (Path(__file__).parent.parent), so copying it into $TMP/scripts/ re-roots it
# onto the fixture with no code seam. The locked tool binary and `go` are
# shimmed, while `npx` is a fail-closed tripwire; no real node_modules/network
# access or Go toolchain is needed.
#
# The two non-zero exits mean different things and the tests assert which:
#   1 — mechanical drift a regeneration fixes
#   2 — a change needing a human (a Go module with no recorded license)
# Collapsing them would hide the escalation path, which is the whole reason the
# script refuses to invent a license.

set -euo pipefail

SRC="$(cd "$(dirname "$0")" && pwd)/update-notice-md.py"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

PASSES=0
FAILS=0
FAIL_DETAILS=()

# ---------------------------------------------------------------- fixture

mkdir -p "$TMP/scripts" "$TMP/bin" \
         "$TMP/.github/tools/license-checker/node_modules/.bin" \
         "$TMP/client/desktop/node_modules" \
         "$TMP/services/media-plane/node_modules" \
         "$TMP/client/admin/node_modules" \
         "$TMP/services/control-plane"
cp "$SRC" "$TMP/scripts/update-notice-md.py"
chmod +x "$TMP/scripts/update-notice-md.py"

# Locked local CLI shim — emits a per-workspace CSV keyed off cwd and records
# the real command arguments at the process boundary.
LICENSE_CHECKER_BIN="$TMP/.github/tools/license-checker/node_modules/.bin/license-checker"
cat > "$LICENSE_CHECKER_BIN" <<'SHIM'
#!/bin/bash
printf '%s|%s\n' "$PWD" "$*" >> "${LOCKED_CALL_LOG:?}"
echo '"module name","license","repository"'
case "$PWD" in
  */client/desktop)        echo '"alpha@1.0.0","MIT","https://example.com/alpha"' ;;
  */services/media-plane)  echo '"beta@2.0.0","ISC","https://example.com/beta"'
                           echo '"dup@1.0.0","MIT","https://example.com/dup"'
                           echo '"dup@2.0.0","MIT","https://example.com/dup"'
                           echo '"gamma@3.0.0","MIT","https://example.com/gamma"' ;;
  */client/admin)          echo '"delta@4.0.0","MIT","https://example.com/delta"' ;;
esac
SHIM

# A fallback to npx would fetch executable code from the registry. Catch it
# locally and make the attempted fallback observable without network access.
cat > "$TMP/bin/npx" <<'SHIM'
#!/bin/bash
echo "npx fallback attempted: $*" >> "${NPX_TRAP_LOG:?}"
echo "npx fallback is forbidden in this test" >&2
exit 99
SHIM

# `go` shim — first line is the first-party module with an empty version, which
# the script must drop rather than tabulate.
cat > "$TMP/bin/go" <<'SHIM'
#!/bin/bash
echo "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane "
echo "github.com/example/one v1.1.0"
echo "github.com/example/two v2.2.0"
SHIM
chmod +x "$LICENSE_CHECKER_BIN" "$TMP/bin/npx" "$TMP/bin/go"
export PATH="$TMP/bin:$PATH"
export LOCKED_CALL_LOG="$TMP/license-checker-calls"
export NPX_TRAP_LOG="$TMP/npx-fallbacks"
: > "$LOCKED_CALL_LOG"
LOCKED_TOOL_SHIM="$TMP/license-checker-shim"
cp "$LICENSE_CHECKER_BIN" "$LOCKED_TOOL_SHIM"

write_notice() {
  cat > "$TMP/NOTICE.md" <<EOF
# NOTICE — fixture

**Generated:** 2020-01-01

Concord Voice distributes ${1:-8} third-party software components across its four
CVSL application artifact families:

- **${2:-2}** Go modules linked into the control-plane runtime binary.
- **${3:-1}** npm packages in the desktop client production bundle (plus Electron and its bundled components)
- **${4:-4}** npm packages in the media-plane production bundle
- **${5:-1}** npm packages in the admin portal production bundle (\`client/admin\`),
  compiled by the admin-ui-builder stage.

| License family | Share | Compatible with CVSL 1.0 + AGPL-3.0 |
|---|---|---|
| MIT | 75.0% (6) | ✓ |
| Apache-2.0 | 12.5% (1) | ✓ |
| ISC | 12.5% (1) | ✓ |
| MPL-2.0 (build-time only) | not in the count above | ✓ |

Shares are exact counts over the ${1:-8} components tabulated below, not estimates.
They are not part of that ${1:-8} and therefore carry no share of it.

It is not part of the ${1:-8}-component count.

## Go Control-Plane Runtime Dependencies

| Module | Version | License |
|---|---|---|
| github.com/example/one | ${6:-v1.1.0} | MIT |
| github.com/example/two | v2.2.0 | Apache-2.0 |

---

## Desktop Client Production Dependencies

| Package | Version | License | Repository |
|---|---|---|---|
| alpha | 1.0.0 | MIT | https://example.com/alpha |

---

## Media-Plane Production Dependencies

| Package | Version | License | Repository |
|---|---|---|---|
| beta | 2.0.0 | ISC | https://example.com/beta |
| dup | 1.0.0 | MIT | https://example.com/dup |
| dup | 2.0.0 | MIT | https://example.com/dup |
| gamma | 3.0.0 | MIT | https://example.com/gamma |

---

## Admin Portal Production Dependencies

| Package | Version | License | Repository |
|---|---|---|---|
| delta | 4.0.0 | MIT | https://example.com/delta |

---

## Contact
EOF
}

# expect <name> <wanted-exit> <args...>
#
# Writes the run's combined output to $TMP/last-output for callers that also
# want to assert on the message. It must NEVER be invoked inside $(...): a
# command substitution runs it in a subshell, so every PASSES/FAILS increment
# is discarded and the exit assertion silently evaporates. An earlier draft did
# exactly that and three cases were vacuous — passing whatever the script did.
expect() {
  local name="$1" want="$2"; shift 2
  local rc
  set +e
  "$TMP/scripts/update-notice-md.py" "$@" >"$TMP/last-output" 2>&1
  rc=$?
  set -e
  if [[ "$rc" -eq "$want" ]]; then
    PASSES=$((PASSES + 1))
  else
    FAILS=$((FAILS + 1))
    FAIL_DETAILS+=("$name: exit $rc, wanted $want"$'\n'"$(head -c 400 "$TMP/last-output")")
  fi
}

# assert_output <name> <pattern> — greps the last expect run's output.
assert_output() {
  if grep -q "$2" "$TMP/last-output"; then
    PASSES=$((PASSES + 1))
  else
    FAILS=$((FAILS + 1))
    FAIL_DETAILS+=("$1: output did not match /$2/"$'\n'"$(head -c 400 "$TMP/last-output")")
  fi
}

# ---------------------------------------------------------------- cases

# 1. A fixture matching its shims is clean.
write_notice
expect "clean fixture passes --check" 0 --check
assert_output "clean fixture" "No changes needed"
if [[ "$(wc -l < "$LOCKED_CALL_LOG" | tr -d ' ')" == 3 ]] \
  && ! [[ -e "$NPX_TRAP_LOG" ]] \
  && awk -F'|' '$2 == "--production --csv --excludePrivatePackages" && $1 ~ "/(client/desktop|services/media-plane|client/admin)$" { n++ } END { exit n != 3 }' "$LOCKED_CALL_LOG"; then
  PASSES=$((PASSES + 1))
else
  FAILS=$((FAILS + 1))
  FAIL_DETAILS+=("locked license CLI receives CSV flags from each workspace cwd; npx is never used"$'\n'"$(cat "$LOCKED_CALL_LOG" 2>/dev/null || true)"$'\n'"$(cat "$NPX_TRAP_LOG" 2>/dev/null || true)")
fi

# Missing local tooling must stop with actionable setup guidance and must not
# fall back to a registry fetch.
write_notice
rm "$LICENSE_CHECKER_BIN"
: > "$NPX_TRAP_LOG"
expect "missing locked license tool fails closed" 2 --check --scope npm
assert_output "missing tool explains locked setup" "license-checker.*\.github/tools/license-checker"
if [[ ! -s "$NPX_TRAP_LOG" ]]; then PASSES=$((PASSES + 1)); else
  FAILS=$((FAILS + 1)); FAIL_DETAILS+=("missing locked tool attempted npx fallback"$'\n'"$(cat "$NPX_TRAP_LOG")")
fi
cp "$LOCKED_TOOL_SHIM" "$LICENSE_CHECKER_BIN"
chmod +x "$LICENSE_CHECKER_BIN"

# 2. A stale version is bookkeeping, not an attribution defect: the component
#    is still attributed and its licence is unchanged. It must warn, not fail,
#    or every routine Dependabot bump turns License Check red.
write_notice 8 2 1 4 1 v0.0.1
expect "stale Go version warns but does not block" 0 --check --scope go
assert_output "stale version emits a warning annotation" "::warning::"

# A scoped npm check must consult only the named workspace, while the shared
# summary/count validation still sees every existing NOTICE table.
write_notice
: > "$LOCKED_CALL_LOG"
expect "single-workspace npm check passes" 0 --check --scope npm --workspace client/admin
if [[ "$(wc -l < "$LOCKED_CALL_LOG" | tr -d ' ')" == 1 && "$(cat "$LOCKED_CALL_LOG")" == *"/client/admin"* ]]; then
  PASSES=$((PASSES + 1))
else
  FAILS=$((FAILS + 1))
  FAIL_DETAILS+=("single-workspace check invoked unrelated npm workspaces"$'\n'"$(cat "$LOCKED_CALL_LOG")")
fi

expect "workspace requires npm scope" 2 --check --scope go --workspace client/admin
assert_output "invalid workspace scope is explicit" "requires.*--scope npm"
expect "unknown workspace is rejected" 2 --check --scope npm --workspace future/workspace
assert_output "unknown workspace error is explicit" "invalid choice"
expect "workspace apply remains unsupported" 2 --apply --workspace client/admin

# Workspace scoping limits dependency inspection, never global NOTICE integrity.
write_notice 99 2 1 4 1
: > "$LOCKED_CALL_LOG"
expect "scoped workspace still checks global total" 1 --check --scope npm --workspace client/admin
assert_output "global consistency failure remains visible" "::error::"
if [[ "$(wc -l < "$LOCKED_CALL_LOG" | tr -d ' ')" == 1 ]]; then
  PASSES=$((PASSES + 1))
else
  FAILS=$((FAILS + 1))
  FAIL_DETAILS+=("global consistency check broadened scoped npm invocation"$'\n'"$(cat "$LOCKED_CALL_LOG")")
fi

# 3. Deleting a Go row removes that module's only recorded license, so the
#    script must refuse to invent one and escalate instead.
write_notice
python3 - "$TMP/NOTICE.md" <<'PY'
import sys
p = sys.argv[1]
t = open(p).read()
open(p, "w").write(t.replace("| github.com/example/two | v2.2.0 | Apache-2.0 |\n", ""))
PY
expect "deleted Go row -> exit 2 (escalate)" 2 --check --scope go
assert_output "escalation names the offending module" "github.com/example/two"

# 4. Deleting an npm row is drift: license-checker supplies the license, so a
#    regeneration restores it without any human decision. This is the shape of
#    the jose@6.2.10 omission that issue #2886 was filed for.
write_notice
python3 - "$TMP/NOTICE.md" <<'PY'
import sys
p = sys.argv[1]
t = open(p).read()
open(p, "w").write(t.replace("| gamma | 3.0.0 | MIT | https://example.com/gamma |\n", ""))
PY
expect "deleted npm row -> exit 1 (attribution defect)" 1 --check --scope npm
assert_output "missing row is reported as an error annotation" "::error::"

# 5. A summary count that disagrees with its own table is drift.
write_notice 8 2 1 99 1
expect "wrong summary count -> exit 1" 1 --check --scope npm

# 6. A wrong total is caught even under a scoped run, because the total is
#    re-derived from the file's four tables rather than from what was scoped.
write_notice 99 2 1 4 1
expect "wrong total -> exit 1" 1 --check --scope go

# 7. --apply repairs drift, and the repaired file then passes --check.
write_notice 8 2 1 4 1 v0.0.1
expect "--apply repairs drift" 0 --apply
expect "repaired file passes --check" 0 --check
grep -q "| github.com/example/one | v1.1.0 | MIT |" "$TMP/NOTICE.md" || {
  FAILS=$((FAILS + 1)); FAIL_DETAILS+=("--apply did not write the corrected Go version")
}
grep -q "^\*\*Generated:\*\* 20" "$TMP/NOTICE.md" || {
  FAILS=$((FAILS + 1)); FAIL_DETAILS+=("--apply did not stamp the Generated date")
}

# 8. --check must not report drift merely because today is not the generation
#    date. Without this the guard would go red on every repo, every day.
write_notice
expect "stale Generated date alone is not drift" 0 --check
assert_output "date-only difference is not drift" "No changes needed"

# 9. --apply regenerates the whole file, so a partial scope is refused rather
#    than silently writing counts derived from one ecosystem.
write_notice
expect "--apply --scope npm is rejected" 2 --apply --scope npm

# 10. The first-party module line carries an empty version and must be dropped,
#     not tabulated. Its presence would inflate every count by one.
write_notice
"$TMP/scripts/update-notice-md.py" --apply >/dev/null 2>&1 || true
if grep -q "Concord-Voice-Alpha/services/control-plane |" "$TMP/NOTICE.md"; then
  FAILS=$((FAILS + 1)); FAIL_DETAILS+=("first-party Go module was tabulated")
else
  PASSES=$((PASSES + 1))
fi

# ---- Regression cases for the 2026-09-08 review of PR #3208 --------------
#
# Every case below covers a defect the original 16 assertions missed. The root
# cause of the first four was that NO fixture had a duplicated package name, so
# a name-keyed row map looked correct in every test while failing open in the
# real file, where eight rows collapse.

# 11. A missing row for one version of a duplicated name, with the summary
#     counts CORRECTED so that gap is the only defect. This is advisory by
#     design: `dup` is still attributed under MIT, so no obligation is
#     unrecorded — only the version list is stale.
#
#     The previous version of this case passed vacuously. It used the default
#     counts, so deleting a row also broke the summary-vs-table check, which is
#     keying-independent — mutating classify() back to name-keying left the
#     whole suite green. Counts are now corrected so the keying is what is
#     under test.
write_notice 7 2 1 3 1
# Alphabetic heredoc labels keep this fixture compatible with the pinned Semgrep Bash parser.
python3 - "$TMP/NOTICE.md" <<'PYX'
import sys
p = sys.argv[1]; t = open(p).read()
open(p, "w").write(t.replace("| dup | 1.0.0 | MIT | https://example.com/dup |\n", ""))
PYX
expect "missing version of a duplicated name -> advisory" 0 --check --scope npm
assert_output "version gap is reported as a warning" "::warning::"

# 11b. THE case the pair key exists for, and the one that dies under a
#      name-keyed map: two versions of one name where the SECOND carries a
#      different licence. Name-keyed, the last row wins and the licences
#      compare equal; pair-keyed, the mismatch blocks.
write_notice
cat > "$LICENSE_CHECKER_BIN" <<'SHIM'
#!/bin/bash
printf '%s|%s\n' "$PWD" "$*" >> "${LOCKED_CALL_LOG:?}"
echo '"module name","license","repository"'
case "$PWD" in
  */client/desktop)        echo '"alpha@1.0.0","MIT","https://example.com/alpha"' ;;
  */services/media-plane)  echo '"beta@2.0.0","ISC","https://example.com/beta"'
                           echo '"dup@1.0.0","Apache-2.0","https://example.com/dup"'
                           echo '"dup@2.0.0","MIT","https://example.com/dup"'
                           echo '"gamma@3.0.0","MIT","https://example.com/gamma"' ;;
  */client/admin)          echo '"delta@4.0.0","MIT","https://example.com/delta"' ;;
esac
SHIM
chmod +x "$LICENSE_CHECKER_BIN"
expect "licence change on a duplicated name -> exit 1" 1 --check --scope npm
assert_output "reported as an attribution defect" "::error::"

# 11c. Licence PERMUTATION across two versions of one name: the licence SET and
#      the version SET are both unchanged, only the pairing moved. A
#      set-vs-set comparison reports nothing; NOTICE.md would record the wrong
#      licence against a specific shipped version.
write_notice
python3 - "$TMP/NOTICE.md" <<'PYX'
import sys
p = sys.argv[1]; t = open(p).read()
t = t.replace("| dup | 1.0.0 | MIT | https://example.com/dup |",
              "| dup | 1.0.0 | Apache-2.0 | https://example.com/dup |")
t = t.replace("| dup | 2.0.0 | MIT | https://example.com/dup |",
              "| dup | 2.0.0 | MIT | https://example.com/dup |")
open(p, "w").write(t)
PYX
cat > "$LICENSE_CHECKER_BIN" <<'SHIM'
#!/bin/bash
printf '%s|%s\n' "$PWD" "$*" >> "${LOCKED_CALL_LOG:?}"
echo '"module name","license","repository"'
case "$PWD" in
  */client/desktop)        echo '"alpha@1.0.0","MIT","https://example.com/alpha"' ;;
  */services/media-plane)  echo '"beta@2.0.0","ISC","https://example.com/beta"'
                           echo '"dup@1.0.0","MIT","https://example.com/dup"'
                           echo '"dup@2.0.0","Apache-2.0","https://example.com/dup"'
                           echo '"gamma@3.0.0","MIT","https://example.com/gamma"' ;;
  */client/admin)          echo '"delta@4.0.0","MIT","https://example.com/delta"' ;;
esac
SHIM
chmod +x "$LICENSE_CHECKER_BIN"
expect "licence permutation across versions -> exit 1" 1 --check --scope npm
assert_output "permutation reported as a defect" "::error::"

# restore the canonical shim
cat > "$LICENSE_CHECKER_BIN" <<'SHIM'
#!/bin/bash
printf '%s|%s\n' "$PWD" "$*" >> "${LOCKED_CALL_LOG:?}"
echo '"module name","license","repository"'
case "$PWD" in
  */client/desktop)        echo '"alpha@1.0.0","MIT","https://example.com/alpha"' ;;
  */services/media-plane)  echo '"beta@2.0.0","ISC","https://example.com/beta"'
                           echo '"dup@1.0.0","MIT","https://example.com/dup"'
                           echo '"dup@2.0.0","MIT","https://example.com/dup"'
                           echo '"gamma@3.0.0","MIT","https://example.com/gamma"' ;;
  */client/admin)          echo '"delta@4.0.0","MIT","https://example.com/delta"' ;;
esac
SHIM
chmod +x "$LICENSE_CHECKER_BIN"

# 12. Removing EVERY row for a name is a name-level removal and must block.
write_notice
python3 - "$TMP/NOTICE.md" <<'PYX'
import sys
p = sys.argv[1]; t = open(p).read()
for v in ("1.0.0", "2.0.0"):
    t = t.replace(f"| dup | {v} | MIT | https://example.com/dup |\n", "")
open(p, "w").write(t)
PYX
expect "whole duplicated name removed -> exit 1" 1 --check --scope npm

# 13. A shim emitting a second version at the SAME licence is bookkeeping, not
#     an attribution defect — the component is still attributed under that
#     licence. Must stay advisory, or duplicate-heavy trees block constantly.
write_notice
cat > "$LICENSE_CHECKER_BIN" <<'SHIM'
#!/bin/bash
printf '%s|%s\n' "$PWD" "$*" >> "${LOCKED_CALL_LOG:?}"
echo '"module name","license","repository"'
case "$PWD" in
  */client/desktop)        echo '"alpha@1.0.0","MIT","https://example.com/alpha"' ;;
  */services/media-plane)  echo '"beta@2.0.0","ISC","https://example.com/beta"'
                           echo '"dup@1.0.0","MIT","https://example.com/dup"'
                           echo '"dup@2.0.0","MIT","https://example.com/dup"'
                           echo '"dup@3.0.0","MIT","https://example.com/dup"'
                           echo '"gamma@3.0.0","MIT","https://example.com/gamma"' ;;
  */client/admin)          echo '"delta@4.0.0","MIT","https://example.com/delta"' ;;
esac
SHIM
chmod +x "$LICENSE_CHECKER_BIN"
expect "extra same-licence version stays advisory" 0 --check --scope npm
assert_output "reported as a warning" "::warning::"

# 14. license-checker could not identify a licence. Writing UNKNOWN or a
#     Custom: URL into a legal artifact asserts as much as inventing one, so it
#     escalates — symmetric with the Go path.
write_notice
cat > "$LICENSE_CHECKER_BIN" <<'SHIM'
#!/bin/bash
printf '%s|%s\n' "$PWD" "$*" >> "${LOCKED_CALL_LOG:?}"
echo '"module name","license","repository"'
case "$PWD" in
  */client/desktop)        echo '"alpha@1.0.0","UNKNOWN","https://example.com/alpha"' ;;
  */services/media-plane)  echo '"beta@2.0.0","ISC","https://example.com/beta"' ;;
  */client/admin)          echo '"delta@4.0.0","MIT","https://example.com/delta"' ;;
esac
SHIM
chmod +x "$LICENSE_CHECKER_BIN"
expect "UNKNOWN npm licence -> exit 2 (escalate)" 2 --check --scope npm
assert_output "escalation names the package" "alpha"

# restore the canonical shim for the remaining cases
cat > "$LICENSE_CHECKER_BIN" <<'SHIM'
#!/bin/bash
printf '%s|%s\n' "$PWD" "$*" >> "${LOCKED_CALL_LOG:?}"
echo '"module name","license","repository"'
case "$PWD" in
  */client/desktop)        echo '"alpha@1.0.0","MIT","https://example.com/alpha"' ;;
  */services/media-plane)  echo '"beta@2.0.0","ISC","https://example.com/beta"'
                           echo '"dup@1.0.0","MIT","https://example.com/dup"'
                           echo '"dup@2.0.0","MIT","https://example.com/dup"'
                           echo '"gamma@3.0.0","MIT","https://example.com/gamma"' ;;
  */client/admin)          echo '"delta@4.0.0","MIT","https://example.com/delta"' ;;
esac
SHIM
chmod +x "$LICENSE_CHECKER_BIN"

# 15. An internal error must NOT exit 1. Exit 1 is reserved for an attribution
#     defect, so a parser crash returning it would be annotated by CI as a
#     compliance failure — and a guard with its checks deleted would fail
#     indistinguishably from a guard that found something.
write_notice
python3 - "$TMP/NOTICE.md" <<'PYX'
import sys
p = sys.argv[1]; t = open(p).read()
# A stray rule inside a section truncates it before its table.
open(p, "w").write(t.replace("## Desktop Client Production Dependencies\n",
                             "## Desktop Client Production Dependencies\n\n---\n"))
PYX
expect "malformed section -> exit 2, never 1" 2 --check --scope npm

# 16. `## X` must not match inside `### X`, which would silently retarget the
#     rewrite onto the wrong region and leave an orphaned '#'.
write_notice
python3 - "$TMP/NOTICE.md" <<'PYX'
import sys
p = sys.argv[1]; t = open(p).read()
open(p, "w").write(t.replace("## Contact",
                             "### Desktop Client Production Dependencies\n\nprose\n\n## Contact"))
PYX
expect "### heading does not retarget the rewrite" 0 --check --scope npm
assert_output "still finds the real section" "No changes needed"

# 17. The licence-share table is regenerated, and its prose asserts the shares
#     are exact counts over the total. A stale share row is drift.
write_notice
python3 - "$TMP/NOTICE.md" <<'PYX'
import sys
p = sys.argv[1]; t = open(p).read()
open(p, "w").write(t.replace("| MIT | 75.0% (6) | ✓ |", "| MIT | 99.9% (999) | ✓ |"))
PYX
expect "stale licence-share row is drift" 0 --check --scope npm
assert_output "share drift is reported" "::warning::"
"$TMP/scripts/update-notice-md.py" --apply >/dev/null 2>&1 || true
if grep -q "| MIT | 75.0% (6) | ✓ |" "$TMP/NOTICE.md"; then PASSES=$((PASSES + 1)); else
  FAILS=$((FAILS + 1)); FAIL_DETAILS+=("--apply did not rebuild the licence-share table")
fi
if grep -q "MPL-2.0 (build-time only)" "$TMP/NOTICE.md"; then PASSES=$((PASSES + 1)); else
  FAILS=$((FAILS + 1)); FAIL_DETAILS+=("--apply dropped the build-time share rows")
fi

# 18. --apply must leave NO stale copy of the old total anywhere in the summary.
#     An incomplete TOTAL_PATTERNS once wrote 401 in one sentence and left 398
#     in two others, authoring the very defect the guard claims to block.
write_notice
cat > "$LICENSE_CHECKER_BIN" <<'SHIM'
#!/bin/bash
printf '%s|%s\n' "$PWD" "$*" >> "${LOCKED_CALL_LOG:?}"
echo '"module name","license","repository"'
case "$PWD" in
  */client/desktop)        echo '"alpha@1.0.0","MIT","https://example.com/alpha"'
                           echo '"epsilon@5.0.0","MIT","https://example.com/epsilon"' ;;
  */services/media-plane)  echo '"beta@2.0.0","ISC","https://example.com/beta"'
                           echo '"dup@1.0.0","MIT","https://example.com/dup"'
                           echo '"dup@2.0.0","MIT","https://example.com/dup"'
                           echo '"gamma@3.0.0","MIT","https://example.com/gamma"' ;;
  */client/admin)          echo '"delta@4.0.0","MIT","https://example.com/delta"' ;;
esac
SHIM
chmod +x "$LICENSE_CHECKER_BIN"
expect "--apply with a changed total" 0 --apply
if grep -q "\b8\b.*component\|exact counts over the 8 " "$TMP/NOTICE.md"; then
  FAILS=$((FAILS + 1)); FAIL_DETAILS+=("stale old total 8 survived --apply")
else PASSES=$((PASSES + 1)); fi
expect "file is self-consistent after --apply" 0 --check

# ---- Regression cases for the security review of PR #3208 -----------------

# 19. The share table's third column asserts compatibility with CVSL 1.0 +
#     AGPL-3.0. A licence outside the allowlist must ESCALATE, never receive a
#     machine-written checkmark — certifying is a larger claim than naming, and
#     the script already refuses to name.
write_notice
python3 - "$TMP/NOTICE.md" <<'PYX'
import sys
p = sys.argv[1]; t = open(p).read()
open(p, "w").write(t.replace("| github.com/example/two | v2.2.0 | Apache-2.0 |",
                             "| github.com/example/two | v2.2.0 | GPL-3.0 |"))
PYX
expect "copyleft licence -> exit 2, never a certified row" 2 --check --scope go
assert_output "escalation names the uncertified licence" "GPL-3.0"
if grep -q "| GPL-3.0 | .*✓ |" "$TMP/NOTICE.md"; then
  FAILS=$((FAILS + 1)); FAIL_DETAILS+=("a copyleft licence was certified compatible")
else PASSES=$((PASSES + 1)); fi

# 20. A `|` in a licence forges a table column: every later cell shifts, both
#     sides then parse cells[2] identically, and the licence-change block — the
#     guard's most important condition — is silently bypassed.
write_notice
cat > "$LICENSE_CHECKER_BIN" <<'SHIM'
#!/bin/bash
printf '%s|%s\n' "$PWD" "$*" >> "${LOCKED_CALL_LOG:?}"
echo '"module name","license","repository"'
case "$PWD" in
  */client/desktop)        echo '"alpha@1.0.0","MIT | GPL-3.0","https://example.com/alpha"' ;;
  */services/media-plane)  echo '"beta@2.0.0","ISC","https://example.com/beta"' ;;
  */client/admin)          echo '"delta@4.0.0","MIT","https://example.com/delta"' ;;
esac
SHIM
chmod +x "$LICENSE_CHECKER_BIN"
expect "pipe in a licence -> exit 2 (escalate)" 2 --check --scope npm
assert_output "escalation names the offending package" "alpha"

# Restore the canonical shim — a leftover hostile shim silently changes what
# every later case is actually testing.
cat > "$LICENSE_CHECKER_BIN" <<'SHIM'
#!/bin/bash
printf '%s|%s\n' "$PWD" "$*" >> "${LOCKED_CALL_LOG:?}"
echo '"module name","license","repository"'
case "$PWD" in
  */client/desktop)        echo '"alpha@1.0.0","MIT","https://example.com/alpha"' ;;
  */services/media-plane)  echo '"beta@2.0.0","ISC","https://example.com/beta"'
                           echo '"dup@1.0.0","MIT","https://example.com/dup"'
                           echo '"dup@2.0.0","MIT","https://example.com/dup"'
                           echo '"gamma@3.0.0","MIT","https://example.com/gamma"' ;;
  */client/admin)          echo '"delta@4.0.0","MIT","https://example.com/delta"' ;;
esac
SHIM
chmod +x "$LICENSE_CHECKER_BIN"

# 21/22. The locked tool manifest and lockfile are the dependency source of
#        truth; the generator must not carry a second version constant.
WF="$(cd "$(dirname "$0")/.." && pwd)/.github/workflows/license-check.yml"
if [ -f "$WF" ]; then
  WF_ALLOW=$(grep -oE "LICENSE_ALLOWLIST: '[^']+'" "$WF" | sed "s/LICENSE_ALLOWLIST: '//; s/'$//")
  PY_ALLOW=$(python3 - "$SRC" <<'PYX'
import importlib.util, sys
spec = importlib.util.spec_from_file_location("u", sys.argv[1])
m = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)
print(m.LICENCE_ALLOWLIST)
PYX
)
  if [ "$WF_ALLOW" = "$PY_ALLOW" ]; then PASSES=$((PASSES + 1)); else
    FAILS=$((FAILS + 1))
    FAIL_DETAILS+=("licence allowlist drifted between script and workflow"$'\n'"  workflow: $WF_ALLOW"$'\n'"  script:   $PY_ALLOW")
  fi

  if python3 - "$(dirname "$WF")/../tools/license-checker" "$WF" "$SRC" <<'PYX'; then
import json, re, sys
from pathlib import Path
root, workflow, source = map(Path, sys.argv[1:])
manifest = json.loads((root / 'package.json').read_text())
lock = json.loads((root / 'package-lock.json').read_text())
assert manifest.get('private') is True, 'locked tool project must remain private'
assert manifest.get('devDependencies') == {'license-checker': '25.0.1'}, 'manifest must pin license-checker 25.0.1'
entry = lock.get('packages', {}).get('node_modules/license-checker', {})
assert entry.get('version') == '25.0.1' and entry.get('integrity'), 'lock must pin version and integrity'
text = workflow.read_text()
assert re.search(r'working-directory:\s*\.github/tools/license-checker', text), 'workflow installs from tool root'
assert re.search(r'npm ci --ignore-scripts --no-audit --no-fund', text), 'workflow uses frozen script-disabled install'
assert 'LICENSE_CHECKER_VERSION' not in source.read_text(), 'generator must not duplicate manifest version'
PYX
  PASSES=$((PASSES + 1)); else
    FAILS=$((FAILS + 1))
    FAIL_DETAILS+=("locked license-tool manifest, integrity lockfile and npm-ci workflow contract")
  fi
else
  FAILS=$((FAILS + 1)); FAIL_DETAILS+=("could not locate license-check.yml for parity assertions")
fi

# 23. An unrecognised row in the licence-share table must be REFUSED, never
#     silently deleted on rebuild — destroying a hand-added row inside a
#     routine-looking regeneration diff is the hazard GO_PACKAGES' own comment
#     warns about.
write_notice
python3 - "$TMP/NOTICE.md" <<'PYX'
import sys
p = sys.argv[1]; t = open(p).read()
open(p, "w").write(t.replace("| MPL-2.0 (build-time only) | not in the count above | ✓ |",
                             "| CC0-1.0 (font data only) | not in the count above | ✓ |\n"
                             "| MPL-2.0 (build-time only) | not in the count above | ✓ |"))
PYX
expect "unrecognised share row -> exit 2, not deleted" 2 --check --scope npm
assert_output "names the row it refuses to delete" "font data only"

# 24. A missing NOTICE.md is an internal error, not an attribution defect.
#     Exit 1 there would have CI annotate a crash as a compliance failure.
write_notice
mv "$TMP/NOTICE.md" "$TMP/NOTICE.md.bak"
expect "missing NOTICE.md -> exit 2, never 1" 2 --check --scope npm
mv "$TMP/NOTICE.md.bak" "$TMP/NOTICE.md"

# ---------------------------------------------------------------- tally

echo
echo "── scripts/test-update-notice-md.sh ──"
echo "passed: $PASSES"
echo "failed: $FAILS"
if [[ "$FAILS" -gt 0 ]]; then
  echo
  for d in "${FAIL_DETAILS[@]}"; do echo "FAIL: $d"; echo; done
  exit 1
fi
echo "All cases passed."
