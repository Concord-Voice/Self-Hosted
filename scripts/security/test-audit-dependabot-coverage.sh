#!/usr/bin/env bash
#
# Regression test for scripts/security/audit-dependabot-coverage.sh.
#
# The audit this script replaced lived as a shell snippet inside a rule file and
# collected five defects in two PRs — every one caught by a reviewer reading the
# prose, never by running it, because prose cannot be run. Each case below pins
# one of those defects, so the fix is held by something that executes.
#
# The load-bearing case is the malformed-manifest control (#3325): it proves the
# audit FAILS CLOSED rather than quietly omitting a root it could not read. A
# green audit over a tree it half-read is the single failure this file exists to
# make impossible.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AUDIT="$SCRIPT_DIR/audit-dependabot-coverage.sh"
WORK="$(mktemp -d)"
cleanup() { [ -n "${WORK:-}" ] && [ -d "$WORK" ] && chmod -R u+w "$WORK" 2>/dev/null; rm -rf "${WORK:?}"; }
trap cleanup EXIT

pass=0
fail=0

assert() {
  local name="$1" got="$2" want="$3" out="$4" pat="${5:-}" nopat="${6:-}"
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
  if [ -n "$nopat" ] && grep -qE "$nopat" <<<"$out"; then
    echo "FAIL: $name — output should NOT match /$nopat/"
    printf '    %s\n' "${out//$'\n'/$'\n'    }"
    fail=$((fail + 1))
    return
  fi
  echo "ok:   $name"
  pass=$((pass + 1))
}

# A fixture repository. `git ls-files` reads the INDEX, so `git add` is enough and
# no commit is needed; `add` runs no hooks, so a repo-level core.hooksPath cannot
# reach in here.
# Git exports GIT_DIR / GIT_INDEX_FILE to the hooks it runs, and they win over
# `-C`. This suite runs as a pre-commit hook, so without clearing them `git init`
# would target the OUTER repository's $GIT_DIR and `add` would stage the real
# worktree. That is not hypothetical: it is how the leak was found.
gitf() {
  env -u GIT_DIR -u GIT_WORK_TREE -u GIT_INDEX_FILE \
      -u GIT_OBJECT_DIRECTORY -u GIT_ALTERNATE_OBJECT_DIRECTORIES git "$@"
}

mkrepo() {
  # Two statements, not `local name=$1 r=$WORK/$name`: bash expands every word of
  # a simple command BEFORE the builtin assigns any of them, so the second RHS
  # reads `name` while it is still unset. Under `set -u` that aborts mkrepo, the
  # fixture is built at an absolute path outside $WORK, and the audit falls back
  # to the REAL repository — where it passes, turning this suite green on
  # fixtures it never created.
  local name="$1"
  local r="$WORK/$name"
  mkdir -p "$r"
  gitf -C "$r" init -q
  printf '%s\n' "$r"
}

# Stage everything and run the audit. Prints combined output; returns its exit code.
run_audit() {
  local r="$1"
  # Structural guard, not tidying: if a fixture root is ever empty or outside
  # $WORK, the audit's own `git rev-parse` fallback aims it at the real
  # repository, where it passes — a whole suite of green assertions about
  # fixtures that were never built. Abort instead of reporting that.
  case "$r" in
    "$WORK"/*) [ -d "$r/.git" ] || { echo "ABORT: fixture has no .git: '$r'"; exit 1; } ;;
    *) echo "ABORT: fixture root outside \$WORK: '$r'"; exit 1 ;;
  esac
  gitf -C "$r" add -A >/dev/null 2>&1
  "$AUDIT" --root "$r" 2>&1
}

# `set -uo pipefail` is not `errexit`, and no caller checks these. A failed
# fixture write left a tree the case never described — and the `noconfig` case
# still saw the exit 2 it expected, from a DIFFERENT cause. Abort instead
# (CodeRabbit, PR #3325).
db() {
  mkdir -p "$1/.github" || { echo "ABORT: mkdir failed for $1/.github"; exit 1; }
  cat >"$1/.github/dependabot.yml" || { echo "ABORT: write failed for $1"; exit 1; }
}
manifest() {
  mkdir -p "$(dirname "$1")" || { echo "ABORT: mkdir failed for $1"; exit 1; }
  printf '%s\n' "$2" >"$1" || { echo "ABORT: write failed for $1"; exit 1; }
}

DEPS='{"dependencies":{"left-pad":"^1.0.0"}}'
NODEPS='{"name":"helper","private":true}'

# ---------------------------------------------------------------------------
# 1. Clean fixture: every ecosystem's tree set equals what is watched.
# ---------------------------------------------------------------------------
r="$(mkrepo clean)"
manifest "$r/app/package.json" "$DEPS"
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: "npm"
    directory: "/app"
YAML
out="$(run_audit "$r")"; rc=$?
assert "clean fixture exits 0" "$rc" 0 "$out" 'AUDIT CLEAN'

# ---------------------------------------------------------------------------
# 2. An update root with no block at all — the #3292 defect in miniature.
# ---------------------------------------------------------------------------
r="$(mkrepo uncovered)"
manifest "$r/app/package.json" "$DEPS"
manifest "$r/worker/package.json" "$DEPS"
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: "npm"
    directory: "/app"
YAML
out="$(run_audit "$r")"; rc=$?
assert "an unwatched update root is UNCOVERED" "$rc" 1 "$out" 'UNCOVERED npm: worker'

# ---------------------------------------------------------------------------
# 3. THE CONTROL (#3325, CodeRabbit). A malformed manifest must make the audit
#    fail closed — exit 2 — not silently drop the root the way a manifest that
#    declares nothing is dropped. An `&&` on jq's exit code passes this tree as
#    clean, which is the defect.
# ---------------------------------------------------------------------------
r="$(mkrepo malformed)"
manifest "$r/app/package.json" "$DEPS"
manifest "$r/broken/package.json" '{"dependencies": '
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: "npm"
    directory: "/app"
YAML
out="$(run_audit "$r")"; rc=$?
assert "a malformed manifest fails the audit closed" "$rc" 2 "$out" 'AUDIT ERROR: cannot read broken/package.json'
assert "…and says the result is not complete coverage" "$rc" 2 "$out" 'AUDIT INCOMPLETE'

# ---------------------------------------------------------------------------
# 4. A dependency-free nested helper is not an update root (#3284): reporting it
#    produced two coverage "gaps" that were not gaps.
# ---------------------------------------------------------------------------
r="$(mkrepo helper)"
manifest "$r/app/package.json" "$DEPS"
manifest "$r/app/scripts/probe/package.json" "$NODEPS"
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: "npm"
    directory: "/app"
YAML
out="$(run_audit "$r")"; rc=$?
assert "a dependency-free helper manifest is not a root" "$rc" 0 "$out" 'AUDIT CLEAN' 'probe'

# ---------------------------------------------------------------------------
# 5. A dependency-bearing LOCKLESS manifest IS a root (#3284→#3292). The first
#    guidance discriminated on a neighbouring package-lock.json and was right by
#    coincidence; this fixture is the divergence that criterion gets wrong.
# ---------------------------------------------------------------------------
r="$(mkrepo lockless)"
manifest "$r/app/package.json" "$DEPS"
printf '{}\n' >"$r/app/package-lock.json"
manifest "$r/lockless/package.json" "$DEPS"   # deliberately NO lockfile
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: "npm"
    directory: "/app"
YAML
out="$(run_audit "$r")"; rc=$?
assert "a lockless dependency-bearing manifest is a root" "$rc" 1 "$out" 'UNCOVERED npm: lockless'

# ---------------------------------------------------------------------------
# 6. Nested go.mod and requirements*.txt are found. A pathspec missing its
#    leading `*` is anchored at the repository root and returns nothing, which
#    surfaces as STALE on both blocks — defect 1, caught by Codex on #3284.
# ---------------------------------------------------------------------------
r="$(mkrepo nested)"
manifest "$r/services/api/go.mod" 'module example.com/api'
manifest "$r/.github/requirements-pyyaml.txt" 'PyYAML==6.0.2'
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: "gomod"
    directory: "/services/api"
  - package-ecosystem: "pip"
    directory: "/.github"
YAML
out="$(run_audit "$r")"; rc=$?
assert "nested go.mod and requirements are enumerated" "$rc" 0 "$out" 'AUDIT CLEAN'

# ---------------------------------------------------------------------------
# 7. The other drift direction: a watched directory absent from the tree. Easy to
#    miss, because nothing downstream errors on a path that does not exist.
# ---------------------------------------------------------------------------
r="$(mkrepo stale)"
manifest "$r/app/package.json" "$DEPS"
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: "npm"
    directories:
      - "/app"
      - "/deleted-workspace"
YAML
out="$(run_audit "$r")"; rc=$?
assert "a watched directory absent from the tree is STALE" "$rc" 1 "$out" 'STALE +npm: deleted-workspace'

# ---------------------------------------------------------------------------
# 8. `:(exclude)*/artifacts/**` — recorded measurement fixtures must not be
#    reported as uncovered deployable images, which would invite exactly the
#    base-image bump that invalidates the recorded result (#3284).
# ---------------------------------------------------------------------------
r="$(mkrepo artifacts)"
manifest "$r/svc/Dockerfile" 'FROM alpine:3.20'
manifest "$r/tooling/artifacts/1111-measurements/Dockerfile.basetest" 'FROM alpine:3.19'
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: "docker"
    directory: "/svc"
YAML
out="$(run_audit "$r")"; rc=$?
assert "a Dockerfile under */artifacts/ is excluded" "$rc" 0 "$out" 'AUDIT CLEAN' 'measurements'

# ---------------------------------------------------------------------------
# 9. Two Dockerfiles in one directory are ONE unit, and the documented minio
#    exclusion is subtracted from the tree rather than reported as a gap (#1220).
# ---------------------------------------------------------------------------
r="$(mkrepo docker-units)"
manifest "$r/svc/Dockerfile" 'FROM alpine:3.20'
manifest "$r/svc/Dockerfile.ops-agent" 'FROM alpine:3.20'
manifest "$r/infrastructure/docker/minio/Dockerfile" 'FROM minio/minio:RELEASE.X'
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: "docker"
    directory: "/svc"
YAML
out="$(run_audit "$r")"; rc=$?
assert "one directory with two Dockerfiles is one unit" "$rc" 0 "$out" 'docker +1 tree'
assert "the minio exclusion is subtracted, not reported" "$rc" 0 "$out" '1 deliberate exclusion' 'UNCOVERED'

# ---------------------------------------------------------------------------
# 10. github-actions: workflows are covered by `directory: "/"`, while a
#     composite action's own pins are invisible to that block and need their own
#     entry — the gap the 2026-08-15 audit found (#3284).
# ---------------------------------------------------------------------------
r="$(mkrepo actions)"
manifest "$r/.github/workflows/ci.yml" 'name: ci'
manifest "$r/.github/actions/track-deployment/action.yml" 'name: track'
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: "github-actions"
    directory: "/"
YAML
out="$(run_audit "$r")"; rc=$?
assert "a composite action directory is its own unwatched unit" "$rc" 1 "$out" 'UNCOVERED github-actions: \.github/actions/track-deployment'

r="$(mkrepo actions-covered)"
manifest "$r/.github/workflows/ci.yml" 'name: ci'
manifest "$r/.github/actions/track-deployment/action.yml" 'name: track'
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: "github-actions"
    directory: "/"
  - package-ecosystem: "github-actions"
    directory: "/.github/actions/track-deployment"
YAML
out="$(run_audit "$r")"; rc=$?
assert "both github-actions units watched is clean" "$rc" 0 "$out" 'AUDIT CLEAN'

# ---------------------------------------------------------------------------
# 11. No dependabot.yml at all: fail closed. Comparing against an empty set would
#     read every root as uncovered, or — if the tree enumeration were also empty —
#     report clean coverage of nothing.
# ---------------------------------------------------------------------------
r="$(mkrepo noconfig)"
manifest "$r/app/package.json" "$DEPS"
out="$(run_audit "$r")"; rc=$?
assert "an absent dependabot.yml fails closed" "$rc" 2 "$out" 'AUDIT INCOMPLETE: dependabot config not found'

# ---------------------------------------------------------------------------
# 12. `git ls-files` is the filter deliberately: an untracked scratch manifest is
#     not deployable surface and must not become a coverage gap.
# ---------------------------------------------------------------------------
r="$(mkrepo untracked)"
manifest "$r/app/package.json" "$DEPS"
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: "npm"
    directory: "/app"
YAML
gitf -C "$r" add -A >/dev/null 2>&1
manifest "$r/scratch/package.json" "$DEPS"   # created AFTER staging: untracked
out="$("$AUDIT" --root "$r" 2>&1)"; rc=$?
assert "an untracked manifest is not a coverage gap" "$rc" 0 "$out" 'AUDIT CLEAN' 'scratch'

# ---------------------------------------------------------------------------
# 13. `git -C` changes the directory, not the repository. Git exports GIT_DIR and
#     GIT_INDEX_FILE to every hook it runs and they OUTRANK cwd, so running as a
#     pre-commit hook made the audit enumerate the outer repository's index while
#     --root pointed elsewhere. It failed closed (every path was unreadable
#     relative to --root) rather than lying, which is how this was caught — but it
#     was auditing the wrong tree. This case runs with the leak simulated.
# ---------------------------------------------------------------------------
r="$(mkrepo envleak)"
manifest "$r/app/package.json" "$DEPS"
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: "npm"
    directory: "/app"
YAML
gitf -C "$r" add -A >/dev/null 2>&1
other="$(mkrepo envleak-other)"
manifest "$other/decoy/package.json" "$DEPS"
gitf -C "$other" add -A >/dev/null 2>&1
out="$(GIT_DIR="$other/.git" GIT_INDEX_FILE="$other/.git/index" "$AUDIT" --root "$r" 2>&1)"; rc=$?
assert "an inherited GIT_DIR does not redirect the audit" "$rc" 0 "$out" 'AUDIT CLEAN' 'decoy'

# ---------------------------------------------------------------------------
# 14. Two drifted directories in one ecosystem each get their own line. A single
#     printf whose format string is recycled over the list takes the SECOND
#     directory as an ecosystem name, so the report misnames what drifted.
# ---------------------------------------------------------------------------
r="$(mkrepo multidrift)"
manifest "$r/alpha/package.json" "$DEPS"
manifest "$r/beta/package.json" "$DEPS"
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: "npm"
    directory: "/nowhere"
YAML
out="$(run_audit "$r")"; rc=$?
assert "each uncovered directory gets its own npm line" "$rc" 1 "$out" 'UNCOVERED npm: alpha'
assert "…including the second one, still named npm" "$rc" 1 "$out" 'UNCOVERED npm: beta'
assert "…and no line is emitted with an empty directory" "$rc" 1 "$out" '' 'UNCOVERED [a-z-]+: *$'

# ---------------------------------------------------------------------------
# 15. YAML does not order mapping keys and Dependabot accepts them in any order.
#     An item beginning `- directory:` used to merge into the item above it, so
#     its directory was attributed to the previous block's ecosystem or lost —
#     false drift from a control that blocks commits on drift.
# ---------------------------------------------------------------------------
r="$(mkrepo keyorder)"
manifest "$r/alpha/package.json" "$DEPS"
manifest "$r/beta/package.json" "$DEPS"
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: "npm"
    directory: "/alpha"
  - directory: "/beta"
    package-ecosystem: "npm"
YAML
out="$(run_audit "$r")"; rc=$?
assert "an update item may start with any key" "$rc" 0 "$out" 'AUDIT CLEAN'

# ---------------------------------------------------------------------------
# 16. Dependabot's `directories:` accepts globs. Comparing a pattern literally
#     calls the pattern uncovered and everything it matches stale — drift
#     reported over a configuration that has none. Refuse to compare instead of
#     expanding: an expander that disagreed with Dependabot's own matching would
#     be a new way to be confidently wrong.
# ---------------------------------------------------------------------------
r="$(mkrepo glob)"
manifest "$r/packages/one/package.json" "$DEPS"
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: "npm"
    directories:
      - "/packages/*"
YAML
out="$(run_audit "$r")"; rc=$?
assert "a directory glob refuses to compare" "$rc" 2 "$out" 'declares a directory GLOB'

# ---------------------------------------------------------------------------
# 17. A regex extractor reads directories out of a document Dependabot cannot
#     load. Without a whole-document parse, valid-looking fields followed by
#     invalid YAML yield a full directory set and the audit reports CLEAN for a
#     config that watches nothing.
# ---------------------------------------------------------------------------
r="$(mkrepo badyaml)"
manifest "$r/app/package.json" "$DEPS"
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: "npm"
    directory: "/app"
  - this: [is, not
    valid: yaml
YAML
out="$(run_audit "$r")"; rc=$?
# No `if python3 -c 'import yaml'` guard any more, and that is the point: the
# parser IS the validator, so an absent PyYAML produces the same exit 2 by the
# same path rather than skipping the case. A case that can skip itself on the
# machine where the defect matters is not a control.
assert "invalid YAML fails the audit closed" "$rc" 2 "$out" 'could not be parsed'

# ---------------------------------------------------------------------------
# 18. A failing `git ls-files` returns no paths and a SUCCESSFUL loop, so a
#     broken index reads as "0 tree roots" — clean, or every watched directory
#     stale. Same shape as the jq defect: an error read as an answer.
# ---------------------------------------------------------------------------
notrepo="$WORK/notrepo"
mkdir -p "$notrepo"
db "$notrepo" <<'YAML'
version: 2
updates:
  - package-ecosystem: "npm"
    directory: "/app"
YAML
out="$("$AUDIT" --root "$notrepo" 2>&1)"; rc=$?
assert "a failing git ls-files fails the audit closed" "$rc" 2 "$out" 'git ls-files failed'

# ---------------------------------------------------------------------------
# 19. The per-ecosystem line must not claim OK for an ecosystem whose sources it
#     could not fully read. The overall AUDIT INCOMPLETE was already correct; the
#     summary line beside it asserted a set comparison that never happened.
# ---------------------------------------------------------------------------
r="$(mkrepo partial)"
manifest "$r/app/package.json" "$DEPS"
manifest "$r/broken/package.json" '{"dependencies": '
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: "npm"
    directory: "/app"
YAML
out="$(run_audit "$r")"; rc=$?
assert "the npm line reads INCOMPLETE, not OK" "$rc" 2 "$out" 'npm +[0-9]+ tree.*INCOMPLETE'
assert "…and no ecosystem line claims OK on that run" "$rc" 2 "$out" '' '^npm .*— OK'

# ---------------------------------------------------------------------------
# 20. The pre-commit filter is part of the control: a hook whose regex never
#     matches is not a gate. `.*/` REQUIRES a slash, so a root-level package.json
#     or go.mod never triggered the audit.
# ---------------------------------------------------------------------------
CFG="$SCRIPT_DIR/../../.pre-commit-config.yaml"
if python3 -c 'import yaml' 2>/dev/null && [ -f "$CFG" ]; then
  out="$(python3 - "$CFG" <<'PY'
import re, sys, yaml
cfg = yaml.safe_load(open(sys.argv[1]))
rx = None
for repo in cfg['repos']:
    for h in repo.get('hooks', []):
        if h.get('id') == 'dependabot-coverage-audit':
            rx = re.compile(h['files'])
if rx is None:
    print('MISSING HOOK'); raise SystemExit(1)
must = ['package.json', 'go.mod', 'client/admin/package.json',
        'services/control-plane/go.mod', 'services/control-plane/Dockerfile',
        '.github/dependabot.yml', '.github/requirements-pyyaml.txt']
bad = [p for p in must if not rx.match(p)]
if rx.match('README.md'):
    bad.append('README.md (should NOT match)')
print('UNMATCHED: ' + ', '.join(bad) if bad else 'FILTER OK')
raise SystemExit(1 if bad else 0)
PY
)"; rc=$?
  assert "the audit hook filter matches every update root" "$rc" 0 "$out" 'FILTER OK'
else
  echo "skip: hook-filter case needs python3 + PyYAML and the repo config"
fi

# ---------------------------------------------------------------------------
# 21. Flow-style `directories: ["/a", "/b"]` is valid YAML and Dependabot accepts
#     it. Matching only block style made it parse as ABSENT, so the ecosystem
#     looked like it watched nothing and every directory reported as drift.
# ---------------------------------------------------------------------------
r="$(mkrepo flowdirs)"
manifest "$r/alpha/package.json" "$DEPS"
manifest "$r/beta/package.json" "$DEPS"
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: "npm"
    directories: ["/alpha", "/beta"]
YAML
out="$(run_audit "$r")"; rc=$?
assert "a flow-style directories list is coverage" "$rc" 0 "$out" 'AUDIT CLEAN'

# ---------------------------------------------------------------------------
# 22. The same list across several lines — what a formatter produces once the
#     single line grows too long.
# ---------------------------------------------------------------------------
r="$(mkrepo flowdirsml)"
manifest "$r/alpha/package.json" "$DEPS"
manifest "$r/beta/package.json" "$DEPS"
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: "npm"
    directories: [
      "/alpha",
      "/beta"
    ]
YAML
out="$(run_audit "$r")"; rc=$?
assert "a multi-line flow sequence is coverage" "$rc" 0 "$out" 'AUDIT CLEAN'

# ---------------------------------------------------------------------------
# 23. A `directories:` shape the parser cannot read is an ERROR, never an empty
#     set. An empty set compares as "watches nothing", which is a confident wrong
#     answer; exit 2 is the only honest one.
# ---------------------------------------------------------------------------
r="$(mkrepo baddirs)"
manifest "$r/alpha/package.json" "$DEPS"
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: "npm"
    directories: "/alpha"
YAML
out="$(run_audit "$r")"; rc=$?
assert "an unreadable directories: shape fails closed" "$rc" 2 "$out" 'directories.*not a non-empty sequence'

# ---------------------------------------------------------------------------
# 24. A glob reaching the audit through the FLOW spelling must hit the same
#     refusal as the block spelling — the guard keys on the parsed value, so it
#     must not depend on which syntax produced it.
# ---------------------------------------------------------------------------
r="$(mkrepo flowglob)"
manifest "$r/packages/one/package.json" "$DEPS"
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: "npm"
    directories: ["/packages/*"]
YAML
out="$(run_audit "$r")"; rc=$?
assert "a glob in flow style also refuses to compare" "$rc" 2 "$out" 'declares a directory GLOB'

# ---------------------------------------------------------------------------
# 25. QUOTED KEYS. `'package-ecosystem': 'npm'` is valid YAML and Dependabot
#     accepts it; the regex scanner matched only bare keys, so the block was not
#     identified at all and the ecosystem read as watching nothing.
# ---------------------------------------------------------------------------
r="$(mkrepo quotedkeys)"
manifest "$r/alpha/package.json" "$DEPS"
db "$r" <<'YAML'
version: 2
updates:
  - 'package-ecosystem': 'npm'
    'directory': '/alpha'
YAML
out="$(run_audit "$r")"; rc=$?
assert "quoted mapping keys are read" "$rc" 0 "$out" 'AUDIT CLEAN'

# ---------------------------------------------------------------------------
# 26. FLOW MAPPINGS. `- { package-ecosystem: npm, directory: /alpha }` is the
#     same mapping written inline. Supporting the flow SEQUENCE form of a value
#     did not make the scanner accept the flow MAPPING that contains it — which
#     is the argument for parsing YAML rather than recognising its spellings.
# ---------------------------------------------------------------------------
r="$(mkrepo flowmapping)"
manifest "$r/alpha/package.json" "$DEPS"
db "$r" <<'YAML'
version: 2
updates:
  - { package-ecosystem: npm, directory: /alpha }
YAML
out="$(run_audit "$r")"; rc=$?
assert "a flow mapping update block is read" "$rc" 0 "$out" 'AUDIT CLEAN'

# ---------------------------------------------------------------------------
# 27. THE VALIDATOR IS NOT OPTIONAL. The previous version printed a note and
#     CONTINUED when python3/PyYAML was unavailable, so a run could reach AUDIT
#     CLEAN without ever establishing that Dependabot can load the config. The
#     parser is the validator now, so its absence is exit 2 by the same path as a
#     syntax error — there is no branch left that can skip it (CodeRabbit, #3325).
# ---------------------------------------------------------------------------
shim="$WORK/shim"
mkdir -p "$shim"
printf '#!/bin/sh\nexit 127\n' >"$shim/python3"
chmod +x "$shim/python3"
r="$(mkrepo nopyyaml)"
manifest "$r/app/package.json" "$DEPS"
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: "npm"
    directory: "/app"
YAML
gitf -C "$r" add -A >/dev/null 2>&1
out="$(PATH="$shim:$PATH" "$AUDIT" --root "$r" 2>&1)"; rc=$?
assert "an unavailable YAML parser fails closed" "$rc" 2 "$out" 'could not parse|could not be parsed'
assert "…and never reports the tree as clean" "$rc" 2 "$out" '' 'AUDIT CLEAN'

# ---------------------------------------------------------------------------
# 28. The hooks must get PyYAML from a PRE-COMMIT-MANAGED env, not the
#     contributor's ambient interpreter. `language: system` invoked ambient
#     python3, and pre-commit being a python application does NOT put PyYAML on
#     that interpreter — its own venv has it, the contributor's python3 need not.
#     A contributor with pre-commit, node and python but no global PyYAML was
#     blocked from committing: fail-closed, but on a dependency the hook config
#     never provisioned (CodeRabbit, #3325). This asserts the CONFIGURATION,
#     because the runtime behaviour is invisible on a machine that happens to
#     have PyYAML — which is every machine this was developed on.
# ---------------------------------------------------------------------------
REQ="$SCRIPT_DIR/../../.github/requirements-pyyaml.txt"
if python3 -c 'import yaml' 2>/dev/null && [ -f "$CFG" ] && [ -f "$REQ" ]; then
  out="$(python3 - "$CFG" "$REQ" <<'PYCFG'
import re, sys, yaml
cfg = yaml.safe_load(open(sys.argv[1]))
pinned = None
for line in open(sys.argv[2]):
    m = re.match(r'\s*PyYAML==([0-9.]+)', line)
    if m:
        pinned = m.group(1)
want = {'pin-exception-lint', 'test-pin-exception-lint',
        'dependabot-coverage-audit', 'test-dependabot-coverage-audit'}
seen, bad = set(), []
for repo in cfg['repos']:
    for h in repo.get('hooks', []):
        if h.get('id') not in want:
            continue
        seen.add(h['id'])
        deps = h.get('additional_dependencies') or []
        if h.get('language') != 'python':
            bad.append(h['id'] + ': language=' + str(h.get('language')) + ', not python')
        elif not any(str(d).startswith('PyYAML==') for d in deps):
            bad.append(h['id'] + ': no pinned PyYAML in additional_dependencies')
        elif pinned and ('PyYAML==' + pinned) not in deps:
            bad.append(h['id'] + ': pins ' + str(deps) + ' but requirements-pyyaml.txt pins ' + pinned)
for missing in sorted(want - seen):
    bad.append(missing + ': hook not found')
print('; '.join(bad) if bad else 'HOOK ENV OK')
raise SystemExit(1 if bad else 0)
PYCFG
)"; rc=$?
  assert "the YAML-parsing hooks provision PyYAML themselves" "$rc" 0 "$out" 'HOOK ENV OK'
else
  echo "skip: hook-env case needs python3 + PyYAML and the repo config"
fi

# ---------------------------------------------------------------------------
# an ignore: that is not a sequence. Valid YAML, invalid Dependabot config — and the declared directory
#     still MATCHES the tree, so the set comparison has nothing to complain about.
#     That is the point: the audit must fail on the shape, not be rescued by a
#     coincidence in the comparison (CodeRabbit, #3325).
# ---------------------------------------------------------------------------
r="$(mkrepo ignore_notlist)"
manifest "$r/app/package.json" "$DEPS"
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: npm
    directory: /app
    ignore: not-a-list
YAML
out="$(run_audit "$r")"; rc=$?
assert "an ignore: that is not a sequence fails closed" "$rc" 2 "$out" '"ignore:" is not a sequence'
assert "…and an ignore: that is not a sequence never reports clean" "$rc" 2 "$out" '' 'AUDIT CLEAN'

# ---------------------------------------------------------------------------
# an ignore: item that is not a mapping. Valid YAML, invalid Dependabot config — and the declared directory
#     still MATCHES the tree, so the set comparison has nothing to complain about.
#     That is the point: the audit must fail on the shape, not be rescued by a
#     coincidence in the comparison (CodeRabbit, #3325).
# ---------------------------------------------------------------------------
r="$(mkrepo ignore_item)"
manifest "$r/app/package.json" "$DEPS"
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: npm
    directory: /app
    ignore:
      - oops
YAML
out="$(run_audit "$r")"; rc=$?
assert "an ignore: item that is not a mapping fails closed" "$rc" 2 "$out" 'ignore\[0\] is not a mapping'
assert "…and an ignore: item that is not a mapping never reports clean" "$rc" 2 "$out" '' 'AUDIT CLEAN'

# ---------------------------------------------------------------------------
# An `updates:` item that is not a mapping was dropped by a `.filter()` that read
#     like defensiveness. A dropped block is a block whose directories are not
#     compared — invisible, and the audit still reports on the rest.
# ---------------------------------------------------------------------------
r="$(mkrepo notamapping)"
manifest "$r/app/package.json" "$DEPS"
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: npm
    directory: /app
  - just-a-string
YAML
out="$(run_audit "$r")"; rc=$?
assert "a non-mapping updates item fails closed" "$rc" 2 "$out" 'updates\[1\] is not a mapping'
assert "…and never reports clean" "$rc" 2 "$out" '' 'AUDIT CLEAN'

# ---------------------------------------------------------------------------
# A block naming no ecosystem, and a block naming no directory at all. Both are
#     required by Dependabot; both previously read as "a block that watches
#     nothing", which is indistinguishable from a correct block for another
#     ecosystem.
# ---------------------------------------------------------------------------
r="$(mkrepo noeco)"
manifest "$r/app/package.json" "$DEPS"
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: npm
    directory: /app
  - directory: /app
YAML
out="$(run_audit "$r")"; rc=$?
assert "a block with no package-ecosystem fails closed" "$rc" 2 "$out" 'no string "package-ecosystem"'

r="$(mkrepo nodir)"
manifest "$r/app/package.json" "$DEPS"
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: npm
YAML
out="$(run_audit "$r")"; rc=$?
assert "a block naming no directory fails closed" "$rc" 2 "$out" 'neither "directory" nor "directories"'
assert "…and never reports clean" "$rc" 2 "$out" '' 'AUDIT CLEAN'

# ---------------------------------------------------------------------------
# LEAF VALUES, not just container shapes. Validating the containers left the same
#     silent-skip one level down. Each fixture below keeps the declared directory
#     MATCHING the tree, so nothing in the set comparison can rescue the run.
# ---------------------------------------------------------------------------
r="$(mkrepo ignnoname)"
manifest "$r/app/package.json" "$DEPS"
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: npm
    directory: /app
    ignore:
      - versions: [">=2.0.0"]
YAML
out="$(run_audit "$r")"; rc=$?
assert "an ignore rule with no dependency-name fails closed" "$rc" 2 "$out" 'no non-empty string "dependency-name"'
assert "…and never reports clean" "$rc" 2 "$out" '' 'AUDIT CLEAN'

# A null member became the literal directory "null"; 42 became "42". String()
# coercion turned a malformed leaf into a declared path.
r="$(mkrepo dirnull)"
manifest "$r/app/package.json" "$DEPS"
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: npm
    directories: ["/app", null]
YAML
out="$(run_audit "$r")"; rc=$?
assert "a null directories member fails closed" "$rc" 2 "$out" 'directories\[1\] is not a non-empty string'
assert "…and never reports clean" "$rc" 2 "$out" '' 'AUDIT CLEAN'

# An empty list read as "watches nothing", which compares as drift over every
# directory the block was meant to name.
r="$(mkrepo dirempty)"
manifest "$r/app/package.json" "$DEPS"
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: npm
    directories: []
YAML
out="$(run_audit "$r")"; rc=$?
assert "an empty directories list fails closed" "$rc" 2 "$out" 'not a non-empty sequence'

# `directory: ""` normalised to the ROOT sentinel — a fabricated claim that the
# repository root is watched, which is the most dangerous of these because it
# ADDS coverage rather than removing it.
r="$(mkrepo diremptystr)"
manifest "$r/app/package.json" "$DEPS"
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: npm
    directory: ""
YAML
out="$(run_audit "$r")"; rc=$?
assert "an empty directory string fails closed" "$rc" 2 "$out" '"directory" is not a non-empty string'
assert "…and never reports clean" "$rc" 2 "$out" '' 'AUDIT CLEAN'

# ---------------------------------------------------------------------------
# `directory` and `directories` are ALTERNATIVES in Dependabot, so a block naming
#     both has no single valid declared set. blockDirectories returned their
#     UNION — a coverage claim nothing supports. Both tree roots below exist, so
#     the union MATCHES the tree and the set comparison is clean: the only thing
#     that can fail this fixture is refusing the shape (CodeRabbit, #3325).
# ---------------------------------------------------------------------------
r="$(mkrepo bothkeys)"
manifest "$r/app/package.json" "$DEPS"
manifest "$r/worker/package.json" "$DEPS"
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: npm
    directory: /app
    directories: ["/worker"]
YAML
out="$(run_audit "$r")"; rc=$?
assert "a block naming both directory keys fails closed" "$rc" 2 "$out" 'declares both "directory" and "directories"'
assert "…and never reports clean" "$rc" 2 "$out" '' 'AUDIT CLEAN'

# Key PRESENCE, not value truthiness: `block.directory != null` read an explicit
# null as "absent", so this passed validation and was silently treated as a
# directories-only block.
r="$(mkrepo nulldir)"
manifest "$r/app/package.json" "$DEPS"
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: npm
    directory: null
    directories: ["/app"]
YAML
out="$(run_audit "$r")"; rc=$?
assert "an explicit null directory is present, not absent" "$rc" 2 "$out" 'declares both'
assert "…and never reports clean" "$rc" 2 "$out" '' 'AUDIT CLEAN'

# ---------------------------------------------------------------------------
# 29. docker-compose: a directory whose compose file DIGEST-pins an image is a
#     unit, and a block naming it is clean.
# ---------------------------------------------------------------------------
PIN="redis:7.4.11-alpine@sha256:$(printf 'a%.0s' $(seq 64))"
r="$(mkrepo compose-pinned)"
manifest "$r/docker-compose.yml" "services:
  redis:
    image: $PIN"
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: "docker-compose"
    directory: "/"
YAML
out="$(run_audit "$r")"; rc=$?
assert "a watched compose digest pin is clean" "$rc" 0 "$out" 'docker-compose +1 tree · +1 watched — OK'

# ---------------------------------------------------------------------------
# 30. The same pin with no docker-compose block is UNCOVERED: a digest pin
#     nobody refreshes goes stale silently, which is what this leg catches.
# ---------------------------------------------------------------------------
r="$(mkrepo compose-unwatched)"
manifest "$r/docker-compose.yml" "services:
  redis:
    image: $PIN"
db "$r" <<'YAML'
version: 2
updates: []
YAML
out="$(run_audit "$r")"; rc=$?
assert "an unwatched compose digest pin is UNCOVERED" "$rc" 1 "$out" 'UNCOVERED docker-compose: \.'

# ---------------------------------------------------------------------------
# 31. Floating, `latest` and local build tags give Dependabot nothing to move,
#     so they are not units; a pin under */artifacts/ is excluded as for docker;
#     and a commented-out pin does not count.
# ---------------------------------------------------------------------------
r="$(mkrepo compose-unpinned)"
manifest "$r/docker-compose.yml" "services:
  a:
    image: nats:2-alpine
  b:
    image: dpage/pgadmin4:latest
  c:
    image: concordvoice-postgres:16-alpine
  d:
    image: nats:2-alpine # was $PIN"
manifest "$r/tooling/artifacts/x/docker-compose.yml" "services:
  r:
    image: $PIN"
db "$r" <<'YAML'
version: 2
updates: []
YAML
out="$(run_audit "$r")"; rc=$?
assert "unpinned, artifact and commented-out compose images are not units" "$rc" 0 "$out" 'docker-compose +0 tree' 'UNCOVERED'

# ---------------------------------------------------------------------------
# 32. A compose file the audit cannot read fails CLOSED. A tracked dangling
#     symlink is unreadable even to root, so this holds on a root CI runner.
# ---------------------------------------------------------------------------
r="$(mkrepo compose-unreadable)"
manifest "$r/docker-compose.yml" "services:
  redis:
    image: $PIN"
ln -s missing.yml "$r/docker-compose.override.yml" || { echo "ABORT: symlink failed"; exit 1; }
db "$r" <<'YAML'
version: 2
updates:
  - package-ecosystem: "docker-compose"
    directory: "/"
YAML
out="$(run_audit "$r")"; rc=$?
assert "an unreadable compose file fails the audit closed" "$rc" 2 "$out" 'AUDIT ERROR: cannot read docker-compose.override.yml'
assert "…and its ecosystem line is not OK" "$rc" 2 "$out" 'docker-compose .* INCOMPLETE'

# ---------------------------------------------------------------------------
echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
