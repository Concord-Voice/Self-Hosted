#!/usr/bin/env bash
#
# Regression test for scripts/security/override-range-lint.mjs.
#
# Locks the lint's contract: it reports a consumer that an `overrides` entry
# forces outside the consumer's own declared range, it resolves versions the way
# Node does (nearest-ancestor node_modules), it fails closed rather than passing
# vacuously, and its baseline cannot mute a NEW skew on an already-listed
# package — the property that would have made a naive baseline absorb #2709 on
# day one.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LINT="$SCRIPT_DIR/override-range-lint.mjs"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0

assert() {
  local name="$1" got="$2" want="$3" out="$4" pat="${5:-}"
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
  echo "ok:   $name"
  pass=$((pass + 1))
}

# The lint loads `semver` from the package under test. Fixtures get a symlink to
# a real installed semver so the tests exercise the actual range engine rather
# than a stand-in whose behaviour could diverge from production.
SEMVER_SRC=""
for cand in \
  "$SCRIPT_DIR/../../client/desktop/node_modules/semver" \
  "$SCRIPT_DIR/../../client/admin/node_modules/semver" \
  "$SCRIPT_DIR/../../services/media-plane/node_modules/semver" \
  "$SCRIPT_DIR/../../[internal]workers/apple-sso-bridge/node_modules/semver" \
  "$SCRIPT_DIR/../../[internal]workers/invite-landing/node_modules/semver"; do
  [ -d "$cand" ] && { SEMVER_SRC="$(cd "$cand" && pwd)"; break; }
done
if [ -z "$SEMVER_SRC" ]; then
  echo "ERROR: no installed semver found in any workspace — cannot run override-range-lint tests"
  echo "       (run 'npm ci --ignore-scripts' in a listed workspace, then re-run)"
  exit 1
fi

# mkfixture <name> <package.json body> <package-lock.json body>
mkfixture() {
  local name="$1" pkg="$2" lock="$3"
  local d="$WORK/$name"
  mkdir -p "$d/node_modules"
  ln -s "$SEMVER_SRC" "$d/node_modules/semver"
  printf '%s\n' "$pkg" > "$d/package.json"
  printf '%s\n' "$lock" > "$d/package-lock.json"
  echo "$d"
}

# run <name> <fixture-dir> <expected-exit> [grep-pattern]
run() {
  local name="$1" dir="$2" want="$3" pat="${4:-}"
  local out got
  out="$(node "$LINT" --dir "$dir" 2>&1)"
  got=$?
  assert "$name" "$got" "$want" "$out" "$pat"
}

# run_stale_strict <name> <fixture-dir> <expected-exit> [grep-pattern]
run_stale_strict() {
  local name="$1" dir="$2" want="$3" pat="${4:-}"
  local out got
  out="$(node "$LINT" --dir "$dir" --fail-on-stale 2>&1)"
  got=$?
  assert "$name" "$got" "$want" "$out" "$pat"
}

run_minimum() {
  local name="$1" dir="$2" want="$3" pat="${4:-}"
  local out got
  out="$(node "$LINT" --dir "$dir" --minimum-version undici=7.29.1 2>&1)"
  got=$?
  assert "$name" "$got" "$want" "$out" "$pat"
}

run_minimum_specs() {
  local name="$1" dir="$2" want="$3" pat="${4:-}"
  shift 4
  local out got
  out="$(node "$LINT" --dir "$dir" "$@" 2>&1)"
  got=$?
  assert "$name" "$got" "$want" "$out" "$pat"
}

# ── 1. The founding case: a consumer forced a major BELOW what it declares.
D=$(mkfixture violation-below \
'{"name":"f","version":"1.0.0","overrides":{"undici":"^7.29.0"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/jsdom":{"version":"30.0.1","dependencies":{"undici":"^8.9.0"}},
  "node_modules/undici":{"version":"7.29.0"}
}}')
run "flags a consumer forced below its declared major (#2709)" "$D" 1 "major-mismatch"
run "names the offending package"                              "$D" 1 "undici"
run "shows the declared range and the resolution"              "$D" 1 "\\^8\\.9\\.0"

# ── 2. A consumer forced a major ABOVE what it declares — same class.
D=$(mkfixture violation-above \
'{"name":"f","version":"1.0.0","overrides":{"undici":"^7.29.0"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/node-gyp":{"version":"11.0.0","dependencies":{"undici":"^6.25.0"}},
  "node_modules/undici":{"version":"7.29.0"}
}}')
run "flags a consumer forced above its declared major" "$D" 1 "major-mismatch"

# ── 3. Override lands INSIDE every declared range — the healthy case.
D=$(mkfixture clean \
'{"name":"f","version":"1.0.0","overrides":{"undici":"^7.29.0"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/@electron/get":{"version":"5.0.0","dependencies":{"undici":"^7.24.4"}},
  "node_modules/undici":{"version":"7.29.0"}
}}')
run "passes when every consumer resolves inside its range" "$D" 0 "no new consumer forced"

# ── 4. Nearest-ancestor resolution: a nested copy satisfies its consumer even
#      though the hoisted copy would not. Getting this wrong would invent
#      violations that npm does not actually produce.
D=$(mkfixture nested \
'{"name":"f","version":"1.0.0","overrides":{"undici":"^7.29.0"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/jsdom":{"version":"30.0.1","dependencies":{"undici":"^8.9.0"}},
  "node_modules/jsdom/node_modules/undici":{"version":"8.10.0"},
  "node_modules/undici":{"version":"7.29.0"}
}}')
run "honours a nested copy over the hoisted one" "$D" 0 "no new consumer forced"

# ── 5. Packages with no override are none of this lint's business.
D=$(mkfixture unrelated \
'{"name":"f","version":"1.0.0","overrides":{"tar":"^7.5.16"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/jsdom":{"version":"30.0.1","dependencies":{"undici":"^8.9.0"}},
  "node_modules/undici":{"version":"7.29.0"}
}}')
run "ignores skew on packages that are not overridden" "$D" 0 "no new consumer forced"

# ── 5b. A caller may pin a minimum resolved version without declaring an
#      override. Older locks must fail closed with a floor diagnostic; the
#      exact floor must pass.
D=$(mkfixture minimum-version-below \
'{"name":"f","version":"1.0.0"}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/undici":{"version":"7.29.0"}
}}')
run_minimum "fails a resolved version below the requested floor" "$D" 1 "below minimum"
out=$(node "$LINT" --dir "$D" --minimum-version undici=7.29.1 --json 2>&1); got=$?
assert "blocks below-floor JSON output" "$got" 1 "$out" '"minimumVersionViolations": \['
assert "exposes the below-floor package in JSON" "$got" 1 "$out" '"dep": "undici"'
cat > "$D/package-lock.json" <<'LOCK'
{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/undici":{"version":"7.29.1"}
}}
LOCK
run_minimum "passes when the resolved version meets the requested floor" "$D" 0

D=$(mkfixture minimum-version-majors \
'{"name":"f","version":"1.0.0"}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/undici":{"version":"7.29.1"},
  "node_modules/other/node_modules/undici":{"version":"8.9.1"}
}}')
run_minimum_specs "accepts floors for separate undici majors" "$D" 0 "" \
  --minimum-version undici=7.29.1 --minimum-version undici=8.9.1
cat > "$D/package-lock.json" <<'LOCK'
{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/undici":{"version":"7.29.1"},
  "node_modules/other/node_modules/undici":{"version":"8.9.0"}
}}
LOCK
run_minimum_specs "evaluates each major floor independently" "$D" 1 "below minimum.*8\.9\.1" \
  --minimum-version undici=7.29.1 --minimum-version undici=8.9.1

D=$(mkfixture minimum-version-duplicate \
'{"name":"f","version":"1.0.0"}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/undici":{"version":"7.29.1"}
}}')
run_minimum_specs "rejects duplicate same-major floors" "$D" 2 "duplicate.*undici@7" \
  --minimum-version undici=7.29.1 --minimum-version undici=7.29.2

D=$(mkfixture minimum-version-malformed \
'{"name":"f","version":"1.0.0"}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/undici":{"version":"not-a-version"}
}}')
run_minimum "rejects a malformed matching resolution" "$D" 2 "malformed matching resolution"

D=$(mkfixture minimum-version-null-identity \
'{"name":"f","version":"1.0.0"}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/undici":{"name":null,"version":"7.29.0"}
}}')
run_minimum "fails closed on a null canonical package identity" "$D" 2 "malformed.*identity"

D=$(mkfixture minimum-version-unrelated-malformed-manifest \
'{"name":"f","version":"1.0.0"}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/undici":{"version":"7.29.1"},
  "node_modules/unrelated":{"version":"1.0.0"}
}}')
mkdir -p "$D/node_modules/unrelated"
printf '%s\n' '{malformed' > "$D/node_modules/unrelated/package.json"
run_minimum "ignores a malformed unrelated installed manifest" "$D" 0

D=$(mkfixture minimum-version-missing-package-argument \
'{"name":"f","version":"1.0.0","dependencies":{"http-client":"npm:undici@^7.29.0"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0","dependencies":{"http-client":"npm:undici@^7.29.0"}},
  "node_modules/http-client":{"version":"7.29.0"}
}}')
out=$(node --input-type=module - "$D" "$LINT" <<'NODE' 2>&1
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { resolve } from 'node:path';

const [dir, lintPath] = process.argv.slice(2);
const { findMinimumVersionViolations } = await import(lintPath);
const semver = createRequire(resolve(dir, 'noop.js'))('semver');
const lock = JSON.parse(readFileSync(resolve(dir, 'package-lock.json'), 'utf8'));
try {
  findMinimumVersionViolations(lock, [{ dep: 'undici', version: '7.29.1' }], semver, dir);
  process.exit(0);
} catch (err) {
  process.stderr.write(`${err.message}\n`);
  process.exit(2);
}
NODE
); got=$?
assert "rejects the old four-argument floor call without pkg" "$got" 2 "$out"

D=$(mkfixture minimum-version-alias \
'{"name":"f","version":"1.0.0"}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/http-client":{"name":"undici","version":"7.29.0"}
}}')
run_minimum "flags an npm alias by its lock identity" "$D" 1 "below minimum"

D=$(mkfixture minimum-version-alias-no-name \
'{"name":"f","version":"1.0.0","dependencies":{"http-client":"npm:undici@^7.29.0"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0","dependencies":{"http-client":"npm:undici@^7.29.0"}},
  "node_modules/http-client":{"version":"7.29.0"}
}}')
run_minimum "flags an npm alias without a lock name" "$D" 1 "below minimum"

D=$(mkfixture minimum-version-unversioned-alias \
'{"name":"f","version":"1.0.0","dependencies":{"http-client":"npm:undici"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0","dependencies":{"http-client":"npm:undici"}},
  "node_modules/http-client":{"version":"7.29.0"}
}}')
run_minimum "flags an unversioned npm alias without a lock name" "$D" 1 "below minimum"

D=$(mkfixture minimum-version-peer-alias \
'{"name":"f","version":"1.0.0","dependencies":{"parent":"1.0.0"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0","dependencies":{"parent":"1.0.0"}},
  "node_modules/parent":{"version":"1.0.0","peerDependencies":{"http-client":"npm:undici@^7.29.0"}},
  "node_modules/parent/node_modules/http-client":{"version":"7.29.0"}
}}')
run_minimum "flags a peer alias resolved from a nested scope" "$D" 1 "below minimum"

D=$(mkfixture minimum-version-tampered-alias \
'{"name":"f","version":"1.0.0","dependencies":{"http-client":"npm:undici@^7.29.0"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0","dependencies":{"http-client":"^7.29.0"}},
  "node_modules/http-client":{"version":"7.29.0"}
}}')
mkdir -p "$D/node_modules/http-client"
printf '%s\n' '{"name":"undici","version":"7.29.0"}' > "$D/node_modules/http-client/package.json"
run_minimum "flags a tampered alias edge using installed identity" "$D" 1 "below minimum"

D=$(mkfixture minimum-version-alias-malformed-manifest \
'{"name":"f","version":"1.0.0","dependencies":{"http-client":"npm:undici@^7.29.0"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0","dependencies":{"http-client":"^7.29.0"}},
  "node_modules/http-client":{"version":"7.29.0"}
}}')
mkdir -p "$D/node_modules/http-client"
printf '%s\n' '{malformed' > "$D/node_modules/http-client/package.json"
run_minimum "fails closed on a malformed installed alias manifest" "$D" 2 "malformed installed package manifest"

D=$(mkfixture minimum-version-root-lock-alias-malformed-manifest \
'{"name":"f","version":"1.0.0","dependencies":{"http-client":"^7.29.0"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0","dependencies":{"http-client":"npm:undici@7.29.0"}},
  "node_modules/http-client":{"version":"7.29.0"}
}}')
mkdir -p "$D/node_modules/http-client"
printf '%s\n' '{malformed' > "$D/node_modules/http-client/package.json"
run_minimum "fails closed on a root lock alias with a malformed manifest" "$D" 2 "malformed installed package manifest"

D=$(mkfixture minimum-version-conflicting-root-identity \
'{"name":"f","version":"1.0.0","dependencies":{"undici":"npm:not-undici@7.29.0"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0","dependencies":{"undici":"^7.29.0"}},
  "node_modules/undici":{"version":"7.29.0"}
}}')
run_minimum "fails closed on conflicting root dependency identities" "$D" 2 "conflicting.*declaration"

D=$(mkfixture minimum-version-conflicting-root-alias \
'{"name":"f","version":"1.0.0","dependencies":{"http-client":"npm:undici@7.29.0"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0","dependencies":{"http-client":"npm:not-undici@7.29.0"}},
  "node_modules/http-client":{"version":"7.29.0"}
}}')
run_minimum "fails closed on conflicting renamed root aliases" "$D" 2 "conflicting.*declaration"

D=$(mkfixture minimum-version-root-alias-identity-mismatch \
'{"name":"f","version":"1.0.0","dependencies":{"http-client":"npm:undici@7.29.1"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0","dependencies":{"http-client":"^7.29.1"}},
  "node_modules/http-client":{"version":"7.29.1"}
}}')
run_minimum "fails closed when the lock drops a root alias identity" "$D" 2 "conflicting.*declaration"

D=$(mkfixture minimum-version-canonical-null-manifest \
'{"name":"f","version":"1.0.0"}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/undici":{"version":"7.29.1"}
}}')
mkdir -p "$D/node_modules/undici"
printf '%s\n' 'null' > "$D/node_modules/undici/package.json"
run_minimum "fails closed on a null canonical installed manifest" "$D" 2 "malformed installed package manifest"

D=$(mkfixture minimum-version-alias-null-manifest \
'{"name":"f","version":"1.0.0","dependencies":{"http-client":"npm:undici@7.29.0"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0","dependencies":{"http-client":"npm:undici@7.29.0"}},
  "node_modules/http-client":{"version":"7.29.1"}
}}')
mkdir -p "$D/node_modules/http-client"
printf '%s\n' 'null' > "$D/node_modules/http-client/package.json"
run_minimum "fails closed on a null declared alias manifest" "$D" 2 "malformed installed package manifest"

D=$(mkfixture minimum-version-alias-renamed-manifest \
'{"name":"f","version":"1.0.0","dependencies":{"http-client":"npm:undici@^7.29.1"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0","dependencies":{"http-client":"^7.29.1"}},
  "node_modules/http-client":{"version":"7.29.1"}
}}')
mkdir -p "$D/node_modules/http-client"
printf '%s\n' '{"name":"not-undici","version":"7.29.1"}' > "$D/node_modules/http-client/package.json"
run_minimum "fails closed on an installed alias identity mismatch" "$D" 2 "conflicting.*declaration"

D=$(mkfixture minimum-version-reverse-alias \
'{"name":"f","version":"1.0.0","dependencies":{"undici":"npm:not-undici@7.29.0"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0","dependencies":{"undici":"npm:not-undici@7.29.0"}},
  "node_modules/undici":{"name":"not-undici","version":"7.29.0"}
}}')
run_minimum "ignores a reverse alias with an explicit wrong lock name" "$D" 0

D=$(mkfixture minimum-version-tampered-lock-name \
'{"name":"f","version":"1.0.0","dependencies":{"undici":"7.29.0"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0","dependencies":{"undici":"7.29.0"}},
  "node_modules/undici":{"name":"not-undici","version":"7.29.0"}
}}')
mkdir -p "$D/node_modules/undici"
printf '%s\n' '{"name":"undici","version":"7.29.0"}' > "$D/node_modules/undici/package.json"
run_minimum "flags a canonical dependency with a tampered lock name" "$D" 1 "below minimum"

D=$(mkfixture minimum-version-empty-alias-target \
'{"name":"f","version":"1.0.0","dependencies":{"undici":"npm:"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0","dependencies":{"undici":"npm:"}},
  "node_modules/undici":{"version":"7.29.0"}
}}')
run_minimum "fails closed on an empty npm alias target" "$D" 2 "malformed.*alias"

D=$(mkfixture minimum-version-parent-alias-spec \
'{"name":"f","version":"1.0.0","dependencies":{"undici":"npm:.."}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0","dependencies":{"undici":"npm:.."}},
  "node_modules/undici":{"version":"7.29.0"}
}}')
run_minimum "fails closed on a parent-directory npm alias target" "$D" 2 "malformed.*alias"

D=$(mkfixture minimum-version-file-alias-spec \
'{"name":"f","version":"1.0.0","dependencies":{"undici":"npm:not-undici@file:../x"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0","dependencies":{"undici":"npm:not-undici@file:../x"}},
  "node_modules/undici":{"version":"7.29.0"}
}}')
run_minimum "fails closed on a file-target npm alias spec" "$D" 2 "malformed.*alias"

D=$(mkfixture minimum-version-unrelated-conflicting-edges \
'{"name":"f","version":"1.0.0","devDependencies":{"foo":"1.2.3"},"peerDependencies":{"foo":">=1"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0","devDependencies":{"foo":"1.2.3"},"peerDependencies":{"foo":">=1"}},
  "node_modules/undici":{"version":"7.29.1"}
}}')
run_minimum "ignores conflicting declarations unrelated to the floor" "$D" 0

D=$(mkfixture minimum-version-optional-dependency-precedence \
'{"name":"f","version":"1.0.0","dependencies":{"undici":"^7.29.0"},"optionalDependencies":{"undici":"7.29.1"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0","dependencies":{"undici":"^7.29.0"},"optionalDependencies":{"undici":"7.29.1"}},
  "node_modules/undici":{"version":"7.29.1"}
}}')
run_minimum "honors optional dependency precedence for the floor" "$D" 0

D=$(mkfixture minimum-version-optional-dependency-alias-swap \
'{"name":"f","version":"1.0.0","dependencies":{"undici":"^7.29.0"},"optionalDependencies":{"undici":"npm:not-undici@1.0.0"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0","dependencies":{"undici":"^7.29.0"},"optionalDependencies":{"undici":"npm:not-undici@1.0.0"}},
  "node_modules/undici":{"version":"7.10.0"}
}}')
run_minimum "fails closed on an optional dependency alias swap" "$D" 2 "conflicting.*declaration"

D=$(mkfixture minimum-version-unrelated-alias-tag \
'{"name":"f","version":"1.0.0","dependencies":{"helper":"npm:legacy@BETA"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0","dependencies":{"helper":"npm:legacy@BETA"}},
  "node_modules/undici":{"version":"7.29.1"}
}}')
run_minimum "ignores an unrelated npm alias tag" "$D" 0

D=$(mkfixture minimum-version-conflicting-edges \
'{"name":"f","version":"1.0.0","dependencies":{"undici":"7.29.0"},"peerDependencies":{"undici":"npm:not-undici"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0","dependencies":{"undici":"7.29.0"},"peerDependencies":{"undici":"npm:not-undici"}},
  "node_modules/undici":{"version":"7.29.0"}
}}')
run_minimum "fails closed on conflicting dependency declarations" "$D" 2 "conflicting.*declaration"

D=$(mkfixture minimum-version-stale-installed-manifest \
'{"name":"f","version":"1.0.0"}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/undici":{"name":"undici","version":"7.29.1"}
}}')
mkdir -p "$D/node_modules/undici"
printf '%s\n' '{"name":"undici","version":"7.29.0"}' > "$D/node_modules/undici/package.json"
run_minimum "trusts the lock version over a stale installed manifest" "$D" 0
run_minimum_specs "strict install verification rejects a forged lock version" "$D" 2 "does not match lockfile" \
  --minimum-version undici=7.29.1 --verify-installed-versions

D=$(mkfixture minimum-version-stale-installed-alias-identity \
'{"name":"f","version":"1.0.0","dependencies":{"http-client":"npm:got@7.0.0"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0","dependencies":{"http-client":"npm:got@7.0.0"}},
  "node_modules/http-client":{"name":"got","version":"7.0.0"}
}}')
mkdir -p "$D/node_modules/http-client"
printf '%s\n' '{"name":"undici","version":"7.0.0"}' > "$D/node_modules/http-client/package.json"
run_minimum "ignores stale installed alias identity when lock identity is current" "$D" 0

D=$(mkfixture minimum-version-strict-installed-alias-identity \
'{"name":"f","version":"1.0.0","dependencies":{"http-client":"npm:undici@7.29.1"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0","dependencies":{"http-client":"npm:undici@7.29.1"}},
  "node_modules/http-client":{"name":"undici","version":"7.29.1"}
}}')
mkdir -p "$D/node_modules/http-client"
printf '%s\n' '{"name":"got","version":"7.29.1"}' > "$D/node_modules/http-client/package.json"
run_minimum_specs "strict install verification rejects a forged alias identity" "$D" 2 "identity.*does not match" \
  --minimum-version undici=7.29.1 --verify-installed-versions

D=$(mkfixture minimum-version-path-escape \
'{"name":"f","version":"1.0.0"}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "../outside":{"version":"7.29.0"}
}}')
mkdir -p "$WORK/outside"
printf '%s\n' '{"name":"undici","version":"7.29.0"}' > "$WORK/outside/package.json"
run_minimum "rejects lock paths that escape the fixture" "$D" 2 "escape"

D=$(mkfixture minimum-version-path-normalization \
'{"name":"f","version":"1.0.0"}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/../shadow":{"version":"7.29.0"}
}}')
mkdir -p "$D/shadow"
printf '%s\n' '{"name":"undici","version":"7.29.0"}' > "$D/shadow/package.json"
run_minimum "rejects lock paths with node_modules traversal" "$D" 2 "invalid.*path|normal"

D=$(mkfixture minimum-version-root-metadata \
'{"name":"f","version":"1.0.0"}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"undici","version":"7.29.0"}
}}')
run_minimum "ignores root package metadata when checking resolved floors" "$D" 0

D=$(mkfixture minimum-version-workspace-metadata \
'{"name":"f","version":"1.0.0"}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "packages/local":{"name":"undici","version":"7.29.0"}
}}')
run_minimum "ignores non-root workspace metadata without an installed copy" "$D" 0

# ── 6. A file:/link: replacement is a deliberate substitution, not a skew.
D=$(mkfixture stub \
'{"name":"f","version":"1.0.0","overrides":{"node-domexception":"file:./stubs/nde"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/fetch-blob":{"version":"3.2.0","dependencies":{"node-domexception":"^1.0.0"}},
  "node_modules/node-domexception":{"resolved":"stubs/nde","link":true}
}}')
run "skips non-semver (file:/link:) resolutions" "$D" 0 "skipped"

# ── 7. No overrides at all → nothing to check, and that is a pass not an error.
D=$(mkfixture nooverrides \
'{"name":"f","version":"1.0.0"}' \
'{"lockfileVersion":3,"packages":{"":{"name":"f","version":"1.0.0"}}}')
run "no-op when the package declares no overrides" "$D" 0 "nothing to check"

# ── 8. FAIL CLOSED: an unparseable declared range must be reported, never
#      silently treated as satisfied.
D=$(mkfixture badrange \
'{"name":"f","version":"1.0.0","overrides":{"undici":"^7.29.0"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/weird":{"version":"1.0.0","dependencies":{"undici":"not-a-range"}},
  "node_modules/undici":{"version":"7.29.0"}
}}')
run "reports an unparseable declared range rather than skipping it" "$D" 1 "unparseable-range"
# Raised in review on #2768: an unevaluatable declaration must not be presented
# as a measured skew. It still blocks — the consumer asked for something
# off-registry and the override substituted a registry version, which is the
# harm at its most surprising — but it reports under its own heading.
run "does not present it as a measured skew"    "$D" 1 "cannot evaluate"
run "says npm accepts it but semver cannot"     "$D" 1 "semver cannot parse it"

# A real skew and an unevaluatable declaration in the same tree must be counted
# and reported separately, not merged into one number.
D=$(mkfixture mixedkinds \
'{"name":"f","version":"1.0.0","overrides":{"undici":"^7.29.0"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/jsdom":{"version":"30.0.1","dependencies":{"undici":"^8.9.0"}},
  "node_modules/weird":{"version":"1.0.0","dependencies":{"undici":"github:nodejs/undici"}},
  "node_modules/undici":{"version":"7.29.0"}
}}')
run "separates skews from unevaluatable declarations" "$D" 1 "1 consumer\\(s\\) forced outside"
run "and counts the unevaluatable ones on their own"  "$D" 1 "1 declaration\\(s\\) this lint cannot evaluate"

# ── 9. FAIL CLOSED: a lockfile with no packages map is exit 2, not a pass.
D=$(mkfixture nopackages \
'{"name":"f","version":"1.0.0","overrides":{"undici":"^7.29.0"}}' \
'{"lockfileVersion":1}')
run "exits 2 on a lockfile with no packages map" "$D" 2 "packages"

# ── 10. FAIL CLOSED: malformed JSON is exit 2.
D=$(mkfixture badjson \
'{"name":"f","version":"1.0.0","overrides":{"undici":"^7.29.0"}}' \
'{ not json')
run "exits 2 on an unparseable lockfile" "$D" 2 "not valid JSON"

# ── 10b. FAIL CLOSED: `packages: []` must not read as a clean tree.
#     `typeof [] === 'object'`, so an array slipped past the guard and
#     Object.entries found no consumers — a vacuous pass on a malformed
#     lockfile. Raised in review on #2768.
D=$(mkfixture packagesarray \
'{"name":"f","version":"1.0.0","overrides":{"undici":"^7.29.0"}}' \
'{"lockfileVersion":3,"packages":[]}')
run "exits 2 when packages is an array, not a vacuous pass" "$D" 2 "packages"

# ── 11. Nested override blocks: names are collected from the nested key too.
#     NOTE the lockfile here is deliberately synthetic — it shows undici forced
#     tree-wide, which npm would NOT produce from a parent-scoped override. The
#     case pins name COLLECTION (the nested key is seen at all); case 11b pins
#     the behaviour that actually matters on a real tree.
D=$(mkfixture nestedoverride \
'{"name":"f","version":"1.0.0","overrides":{"foo":{"undici":"^7.29.0"}}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/jsdom":{"version":"30.0.1","dependencies":{"undici":"^8.9.0"}},
  "node_modules/undici":{"version":"7.29.0"}
}}')
run "collects packages named only in a nested override block" "$D" 1 "undici"

# ── 11b. The real-tree counterpart, and the answer to the false-positive
#     concern raised in review on #2768. A parent-scoped override leaves an
#     unrelated consumer resolving inside its OWN declared range, so flattening
#     the selector cannot manufacture a finding. Mirrors an actual npm install:
#     `overrides: {"some-unrelated-parent": {"undici": "^7.29.0"}}` + jsdom@30
#     really does resolve jsdom's undici to 8.10.0.
D=$(mkfixture nestedscoped \
'{"name":"f","version":"1.0.0","overrides":{"some-unrelated-parent":{"undici":"^7.29.0"}}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/jsdom":{"version":"30.0.1","dependencies":{"undici":"^8.9.0"}},
  "node_modules/undici":{"version":"8.10.0"}
}}')
run "no false positive when a parent-scoped override leaves a consumer alone" "$D" 0 "no new consumer forced"

# ── 12..14. BASELINE — the property that matters most.
D=$(mkfixture baseline \
'{"name":"f","version":"1.0.0","overrides":{"undici":"^7.29.0"}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/node-gyp":{"version":"11.0.0","dependencies":{"undici":"^6.25.0"}},
  "node_modules/undici":{"version":"7.29.0"}
}}')
out=$(node "$LINT" --dir "$D" --update-baseline 2>&1); got=$?
assert "writes a baseline" "$got" 0 "$out" "wrote 1 accepted"
run "a baselined skew stops failing" "$D" 0 "1 baselined"

# Now add jsdom — a NEW consumer of the SAME already-baselined package. A
# baseline keyed on package name would swallow this; #2709 is exactly that.
cat > "$D/package-lock.json" <<'LOCK'
{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/node-gyp":{"version":"11.0.0","dependencies":{"undici":"^6.25.0"}},
  "node_modules/jsdom":{"version":"30.0.1","dependencies":{"undici":"^8.9.0"}},
  "node_modules/undici":{"version":"7.29.0"}
}}
LOCK
run "baseline does NOT mute a new consumer of a baselined package (#2709)" "$D" 1 "jsdom"

# A baseline row that no longer occurs warns, but must not fail the build.
cat > "$D/package-lock.json" <<'LOCK'
{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/@electron/get":{"version":"5.0.0","dependencies":{"undici":"^7.24.4"}},
  "node_modules/undici":{"version":"7.29.0"}
}}
LOCK
run "warns on a stale baseline row without failing" "$D" 0 "WARN  baseline row no longer occurs"
run_stale_strict "can make a stale baseline merge-blocking" "$D" 1 "stale baseline row"
out=$(node "$LINT" --dir "$D" --fail-on-stale --json 2>&1); got=$?
assert "fails stale baselines in JSON mode too" "$got" 1 "$out" '"stale"'

# The strict stale-baseline control must also catch an override removed without
# removing its baseline — otherwise a cleanup that forgets the baseline bypasses
# the intended removal trigger by taking the no-overrides fast path.
cat > "$D/package.json" <<'PKG'
{"name":"f","version":"1.0.0"}
PKG
run "keeps the no-overrides fast path non-strict" "$D" 0 "nothing to check"
run_stale_strict "fails a stale baseline after its override is removed" "$D" 1 "stale baseline row"
out=$(node "$LINT" --dir "$D" --fail-on-stale --json 2>&1); got=$?
assert "fails a stale no-overrides baseline in JSON mode too" "$got" 1 "$out" '"stale"'
out=$(node "$LINT" --dir "$D" --json 2>&1); got=$?
assert "keeps no-overrides JSON mode non-strict" "$got" 0 "$out" "nothing to check"
printf '%s\n' '{invalid' > "$D/.override-range-baseline.json"
run "ignores a malformed no-overrides baseline non-strict" "$D" 0 "nothing to check"

# A temporary parent-scoped containment must also expire if a parent upgrade
# accepts the forced patch and someone deleted the paired baseline first.
D=$(mkfixture expirednested \
'{"name":"f","version":"1.0.0","overrides":{"miniflare":{"undici":"7.29.1"}}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/miniflare":{"version":"5.0.0","dependencies":{"undici":"^7.29.1"}},
  "node_modules/undici":{"version":"7.29.1"}
}}')
run "allows a compatible nested override outside strict Worker CI" "$D" 0 "no new consumer forced"
run_stale_strict "fails an expired nested containment without a baseline" "$D" 1 "EXPIRED nested override"
out=$(node "$LINT" --dir "$D" --fail-on-stale --json 2>&1); got=$?
assert "fails expired nested containment in JSON mode too" "$got" 1 "$out" '"expiredNestedOverrides"'

# A parent release can remove the contained dependency outright. The nested
# override is then dormant but must still expire in strict Worker CI.
D=$(mkfixture removednestededge \
'{"name":"f","version":"1.0.0","overrides":{"miniflare":{"undici":"7.29.1"}}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/miniflare":{"version":"6.0.0","dependencies":{"ws":"8.21.0"}},
  "node_modules/ws":{"version":"8.21.0"}
}}')
run "allows an unused nested override outside strict Worker CI" "$D" 0 "no new consumer forced"
run_stale_strict "expires containment after the parent removes its child edge" "$D" 1 "no installed miniflare declares undici"

# Removing the parent itself is the same dormant-containment condition.
D=$(mkfixture removednestedparent \
'{"name":"f","version":"1.0.0","overrides":{"miniflare":{"undici":"7.29.1"}}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/undici":{"version":"7.29.1"}
}}')
run_stale_strict "expires containment after the parent leaves the graph" "$D" 1 "no installed miniflare declares undici"

# One older installed parent copy still needs the containment, so a newer
# compatible copy must not make the shared override expire early. The older
# edge is optional to match the union used by findViolations.
D=$(mkfixture mixednested \
'{"name":"f","version":"1.0.0","overrides":{"miniflare":{"undici":"7.29.1"}}}' \
'{"lockfileVersion":3,"packages":{
  "":{"name":"f","version":"1.0.0"},
  "node_modules/miniflare":{"version":"5.0.0","dependencies":{"undici":"^7.29.1"}},
  "node_modules/wrangler/node_modules/miniflare":{"version":"4.0.0","optionalDependencies":{"undici":"7.29.0"}},
  "node_modules/undici":{"version":"7.29.1"},
  "node_modules/wrangler/node_modules/undici":{"version":"7.29.1"}
}}')
out=$(node "$LINT" --dir "$D" --update-baseline 2>&1); got=$?
assert "baselines the still-required nested containment" "$got" 0 "$out" "wrote 1 accepted"
run_stale_strict "does not expire containment while one optional parent still needs it" "$D" 0 "1 baselined"

# Exercise the actual four production commands, not a copied floor list. The
# small extractor deliberately accepts only their current inline YAML format;
# refactoring a command must update this coverage instead of silently skipping it.
if node --input-type=module - "$SCRIPT_DIR/../.." > "$WORK/configured-commands.tsv" 2> "$WORK/configured-commands.err" <<'NODE'; then
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

const root = process.argv[2];
const sources = [
  ['.pre-commit-config.yaml', /^\s+- id: ([\w-]+)\s*$/, /^\s+entry: (.+)$/, [
    'override-range-lint-desktop', 'override-range-lint-admin',
  ]],
  ['.github/workflows/build.yml', /^  ([\w-]+):\s*$/, /^\s+(?:- )?run: (.+)$/, [
    'desktop', 'admin',
  ]],
];
const rows = [];
for (const [file, sectionPattern, commandPattern, sections] of sources) {
  const commands = new Map(sections.map(section => [section, []]));
  let section;
  for (const line of readFileSync(resolve(root, file), 'utf8').split('\n')) {
    const heading = line.match(sectionPattern);
    if (heading) section = heading[1];
    const command = line.match(commandPattern)?.[1];
    if (commands.has(section) && command &&
        (file === '.pre-commit-config.yaml' || command.includes('override-range-lint.mjs'))) {
      commands.get(section).push(command);
    }
  }
  for (const [section, entries] of commands) {
    const label = `${file}:${section}`;
    if (entries.length !== 1) throw new Error(`${label}: expected one lint command, found ${entries.length}`);
    const tokens = entries[0].trim().split(/\s+/);
    if (tokens.shift() !== 'node' || !/^(?:\.\.\/)*scripts\/security\/override-range-lint\.mjs$/.test(tokens.shift())) {
      throw new Error(`${label}: unparseable lint command`);
    }
    const args = [];
    let dirs = 0;
    for (let i = 0; i < tokens.length; i++) {
      const token = tokens[i];
      if (token === '--dir' && /^[\w./-]+$/.test(tokens[i + 1] ?? '')) {
        dirs++;
        i++; // Only redirect the production workspace to the fixture.
      } else if (token === '--minimum-version' && /^undici=\d+\.\d+\.\d+$/.test(tokens[i + 1] ?? '')) {
        args.push(token, tokens[++i]);
      } else if (token === '--fail-on-stale' || token === '--verify-installed-versions') {
        args.push(token);
      } else {
        throw new Error(`${label}: unsupported argument ${token}`);
      }
    }
    if (dirs !== 1) throw new Error(`${label}: expected exactly one --dir`);
    rows.push(`${label}\t${args.join(' ')}`);
  }
}
if (rows.length !== 4) throw new Error(`expected four configured commands, found ${rows.length}`);
console.log(rows.join('\n'));
NODE
  assert "extracts both pre-commit and both build floor commands" 0 0 ""
  while IFS=$'\t' read -r configured_name configured_args; do
    # read splits the validated argument tokens without evaluating shell text.
    read -r -a configured_flags <<< "$configured_args"
    for boundary in 6.29.0:1 6.29.1:0 7.29.1:1 7.30.1:0 8.12.1:1 8.12.2:0; do
      version="${boundary%:*}"
      want="${boundary#*:}"
      D=$(mkfixture "configured-$version-$pass-$fail" \
        "{\"name\":\"floor-fixture\",\"version\":\"1.0.0\",\"dependencies\":{\"undici\":\"$version\"}}" \
        "{\"lockfileVersion\":3,\"packages\":{\"\":{\"name\":\"floor-fixture\",\"version\":\"1.0.0\",\"dependencies\":{\"undici\":\"$version\"}},\"node_modules/undici\":{\"name\":\"undici\",\"version\":\"$version\"}}}")
      mkdir -p "$D/node_modules/undici"
      printf '{"name":"undici","version":"%s"}\n' "$version" > "$D/node_modules/undici/package.json"
      pattern="no new consumer forced"
      if [ "$want" -eq 1 ]; then
        pattern="BELOW MINIMUM  undici@${version//./\\.} .*below minimum"
      fi
      run_minimum_specs "$configured_name checks undici@$version" "$D" "$want" "$pattern" "${configured_flags[@]}"
    done
  done < "$WORK/configured-commands.tsv"
else
  assert "extracts both pre-commit and both build floor commands" 1 0 "$(cat "$WORK/configured-commands.err")"
fi

echo
echo "override-range-lint tests: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
