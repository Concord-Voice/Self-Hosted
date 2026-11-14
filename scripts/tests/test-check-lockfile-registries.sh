#!/usr/bin/env bash
#
# Regression test for scripts/ci/check-lockfile-registries.py (#271).
#
# Both committed fixtures must fail with a message naming the file and the
# offending package/host or directive. The repository's own lockfiles and go.mod
# must pass. The check must fail closed when it cannot read its input.

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK="$HERE/../ci/check-lockfile-registries.py"
FIX="$HERE/fixtures/registries"
WORK="$(mktemp -d)"
# A local replace is resolved against the repository, so that case needs a
# go.mod inside it. The trap removes it on every exit path.
LISTED="$HERE/../../.listed-replace-test.mod"
trap 'rm -rf "$WORK" "$LISTED"' EXIT

pass=0
fail=0

expect() {
  local name="$1" want="$2" pat="$3"; shift 3
  local out got
  out=$(python3 "$CHECK" "$@" 2>&1); got=$?
  if [ "$got" -ne "$want" ]; then
    echo "FAIL: $name: expected exit $want, got $got"; printf '    %s\n' "$out"; fail=$((fail + 1)); return
  fi
  if [ -n "$pat" ] && ! grep -qE -- "$pat" <<<"$out"; then
    echo "FAIL: $name: output missing /$pat/"; printf '    %s\n' "$out"; fail=$((fail + 1)); return
  fi
  echo "ok:   $name"; pass=$((pass + 1))
}

# Committed failing fixtures.
expect 'off-registry resolved fails and names file, package, host' 1 \
  "off-registry-lock\.json: left-pad: resolved from npm\.example\.invalid" --npm-lock "$FIX/off-registry-lock.json"
expect 'the on-registry package in the same lockfile is not reported' 1 \
  '^1 off-registry source' --npm-lock "$FIX/off-registry-lock.json"
expect 'local replace fails and names file and directive' 1 \
  "bad-replace\.mod:14: replace example\.invalid/vendored => \.\./\.\./\.\./\.\./vendored-copy points at local path" \
  --go-mod "$FIX/bad-replace.mod"
expect 'module-path replace is allowed' 1 '^1 off-registry source' --go-mod "$FIX/bad-replace.mod"

# The repository's own files.
expect 'tracked lockfiles and go.mod pass' 0 '^ok: [1-9][0-9]* npm lockfile\(s\) and [1-9][0-9]* go\.mod'

# Other off-registry shapes.
jq '.packages["node_modules/left-pad"].resolved = "git+ssh://git@github.com/example/left-pad.git#abc123"' \
  "$FIX/off-registry-lock.json" > "$WORK/git-lock.json"
expect 'git+ssh source fails' 1 'left-pad: resolved from github\.com' --npm-lock "$WORK/git-lock.json"
jq '.packages["node_modules/left-pad"].resolved = "http://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz"' \
  "$FIX/off-registry-lock.json" > "$WORK/http-lock.json"
expect 'plain http to the approved host fails' 1 'left-pad: resolved from registry\.npmjs\.org' --npm-lock "$WORK/http-lock.json"
jq '.packages["node_modules/left-pad"] = {"resolved": "../outside", "link": true}' \
  "$FIX/off-registry-lock.json" > "$WORK/link-lock.json"
expect 'link that leaves the directory fails' 1 'left-pad: link \.\./outside leaves' --npm-lock "$WORK/link-lock.json"
jq '.packages["node_modules/left-pad"] = {"resolved": "packages/left-pad", "link": true}' \
  "$FIX/off-registry-lock.json" > "$WORK/workspace-lock.json"
expect 'in-tree workspace link passes' 0 '^ok:' --npm-lock "$WORK/workspace-lock.json"

# A listed local replacement passes. The fixture's target resolves to a path
# outside the repository, so list a replacement that resolves inside it.
mkdir -p "$WORK/listed"
printf 'module example.invalid/x\n\ngo 1.26\n\nreplace (\n\texample.invalid/a => ./scripts\n)\n' > "$WORK/listed/go.mod"
cp "$WORK/listed/go.mod" "$LISTED"
printf '{"npm_registry_hosts":["registry.npmjs.org"],"go_local_replacements":["scripts"]}\n' > "$WORK/cfg.json"
expect 'listed in-repo local replace passes' 0 '^ok:' --config "$WORK/cfg.json" --go-mod "$LISTED"
expect 'the same replace fails when not listed' 1 'replace example\.invalid/a => \./scripts' --go-mod "$LISTED"

# Fail closed.
expect 'missing file cannot run' 2 'could not run' --npm-lock "$WORK/absent.json"
jq '.lockfileVersion = 1' "$FIX/off-registry-lock.json" > "$WORK/v1-lock.json"
expect 'lockfileVersion 1 cannot run' 2 'not supported' --npm-lock "$WORK/v1-lock.json"
printf '{"npm_registry_hosts":[]}\n' > "$WORK/empty.json"
expect 'empty host list cannot run' 2 'no npm_registry_hosts' --config "$WORK/empty.json"

echo "check-lockfile-registries: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
