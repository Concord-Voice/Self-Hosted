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
LINK="$HERE/../../.registry-test-link"
trap 'rm -rf "$WORK" "$LISTED" "$LINK"' EXIT

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
expect 'tracked lockfiles, go.mod and .npmrc pass' 0 '^ok: [1-9][0-9]* npm lockfile\(s\), [1-9][0-9]* go\.mod/go\.work file\(s\) and [0-9]+ \.npmrc'

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

# Parser-differential bypasses: npm (WHATWG URL) and Python disagree on these.
for url in 'https://evil.example\@registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz' \
           'https://registry.npmjs.org@evil.example/left-pad/-/left-pad-1.3.0.tgz' \
           'https://user:pw@registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz' \
           'https://registry.npmjs.org:8443/left-pad/-/left-pad-1.3.0.tgz' \
           'https://registry.npmjs.org/left-pad/..\..\evil/x.tgz'; do
  jq --arg u "$url" '.packages["node_modules/left-pad"].resolved = $u' \
    "$FIX/off-registry-lock.json" > "$WORK/diff-lock.json"
  expect "differential URL fails: $url" 1 'left-pad: resolved from' --npm-lock "$WORK/diff-lock.json"
done
# shellcheck disable=SC2016  # literal Go raw-string quoting, not a shell expansion
for target in '"./vendored"' '`../vendored`' '.' '..\vendored' 'vendored'; do
  printf 'module example.invalid/x\n\ngo 1.26\n\nreplace example.invalid/a => %s\n' "$target" > "$WORK/q.mod"
  expect "unversioned replace target is local: $target" 1 'points at local path' --go-mod "$WORK/q.mod"
done
printf 'module example.invalid/x\n\ngo 1.26\n\nreplace example.invalid/a => "./x\\"y"\n' > "$WORK/esc.mod"
expect 'escaped quoted target cannot run' 2 'cannot parse replace target' --go-mod "$WORK/esc.mod"
printf 'module example.invalid/x\n\ngo 1.26\n\nreplace example.invalid/a => "./my dir" v1.0.0\n' > "$WORK/sp.mod"
expect 'quoted path with a space stays one token' 1 'points at another module, \./my dir,' --go-mod "$WORK/sp.mod"
printf 'module example.invalid/x\n\ngo 1.26\n\nreplace example.invalid/a => "./x" "v1.0.0"\n' > "$WORK/qv.mod"
expect 'quoted version cannot run' 2 'is not a version' --go-mod "$WORK/qv.mod"
printf 'module example.invalid/x\n\ngo 1.26\n\nreplace example.invalid/a => ./x v1.0.0 extra\n' > "$WORK/x3.mod"
expect 'three tokens cannot run' 2 'cannot parse replace target' --go-mod "$WORK/x3.mod"

# An approved host is not enough: the path must be this entry's own tarball.
jq '.packages["node_modules/left-pad"] = {"version": "1.3.0", "resolved": "https://registry.npmjs.org/evil-pkg/-/evil-pkg-1.0.0.tgz"}' "$FIX/off-registry-lock.json" > "$WORK/swap-lock.json"
jq '.packages["node_modules/left-pad"] = {"version": "1.3.0/../../../is-number/-/is-number-7.0.0", "resolved": "https://registry.npmjs.org/left-pad/-/left-pad-1.3.0/../../../is-number/-/is-number-7.0.0.tgz"}' "$FIX/off-registry-lock.json" > "$WORK/dots-lock.json"
expect 'dot segments mirrored into version and resolved fail' 1 'left-pad: resolved .* is not the tarball of' --npm-lock "$WORK/dots-lock.json"
jq '.packages["node_modules/left-pad"] = {"version": "1.3.0", "resolved": "https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz%2F..%2F"}' "$FIX/off-registry-lock.json" > "$WORK/esc-lock.json"
expect 'an escaped path fails' 1 'left-pad: resolved .* is not the tarball of' --npm-lock "$WORK/esc-lock.json"
expect 'resolved on the approved host but for another package fails' 1 'left-pad: resolved .* is not the tarball of left-pad@1.3.0' --npm-lock "$WORK/swap-lock.json"

# `name` puts another package at an install location: only a declared npm: alias may.
jq '.packages["node_modules/left-pad"] = {"name": "evil-pkg", "version": "1.0.0", "resolved": "https://registry.npmjs.org/evil-pkg/-/evil-pkg-1.0.0.tgz"}' "$FIX/off-registry-lock.json" > "$WORK/alias-lock.json"
expect 'an undeclared alias fails' 1 "left-pad: entry names package 'evil-pkg', but no dependency declares" --npm-lock "$WORK/alias-lock.json"
jq '.packages[""].dependencies["left-pad"] = "npm:evil-pkg@^1.0.0"' "$WORK/alias-lock.json" > "$WORK/alias-ok-lock.json"
expect 'a declared npm: alias passes' 0 '^ok:' --npm-lock "$WORK/alias-ok-lock.json"

# package.json specs: npm ci re-resolves an edge the lockfile does not satisfy.
mkdir -p "$WORK/mf/stubs/x"
lockv3='{"lockfileVersion":3,"packages":{"":{"name":"m"}}}'
printf '%s\n' "$lockv3" > "$WORK/mf/package-lock.json"
for spec in 'https://npm.example.invalid/x.tgz' 'git+ssh://git@example.invalid/x.git' 'evil/x' 'npm:x@https://npm.example.invalid/x.tgz' 'file:../outside' \
            'evil.tgz' '.evil' '..\\x'; do
  jq -n --arg s "$spec" '{name: "m", dependencies: {x: $s}}' > "$WORK/mf/package.json"
  expect "package.json spec fails: $spec" 1 'dependencies\.x = .* is not a registry range' --npm-lock "$WORK/mf/package-lock.json"
done
jq -n '{name: "m", dependencies: {x: "^1.2.0", y: "latest", z: "npm:@s/z@^2", a: "~1.2.3", b: "1.x || >=2.5.0", c: ""}, overrides: {q: {".": "1.0.0", r: "file:./stubs/x"}, w: "$x"}}' > "$WORK/mf/package.json"
expect 'registry ranges, tags, npm: aliases, override refs and in-tree file: overrides pass' 0 '^ok:' --npm-lock "$WORK/mf/package-lock.json"
jq -n '{name: "m", overrides: {q: {r: "https://npm.example.invalid/r.tgz"}}}' > "$WORK/mf/package.json"
expect 'a nested override with a URL fails' 1 'overrides\.q\.r = ' --npm-lock "$WORK/mf/package-lock.json"
jq -n '{name: "m", dependencies: {x: "file:~/outside"}}' > "$WORK/mf/package.json"
expect 'a home-relative file: spec fails' 1 'dependencies\.x = .file:~/outside' --npm-lock "$WORK/mf/package-lock.json"
# A workspace the lockfile does not list yet is still installed by npm.
jq -n '{name: "m", workspaces: ["packages/*"]}' > "$WORK/mf/package.json"
mkdir -p "$WORK/mf/packages/new"
jq -n '{name: "new", dependencies: {x: "https://npm.example.invalid/x.tgz"}}' > "$WORK/mf/packages/new/package.json"
expect 'a workspace missing from the lockfile is still checked' 1 'packages/new/package\.json: dependencies\.x' --npm-lock "$WORK/mf/package-lock.json"
mkdir -p "$WORK/mf/packages/group/deep"
jq -n '{name: "deep", dependencies: {y: "git+ssh://git@example.invalid/y.git"}}' > "$WORK/mf/packages/group/deep/package.json"
jq -n '{name: "m", workspaces: ["packages/**"]}' > "$WORK/mf/package.json"
expect 'a ** workspaces glob reaches nested workspaces' 1 'packages/group/deep/package\.json: dependencies\.y' --npm-lock "$WORK/mf/package-lock.json"
jq -n '{name: "deep"}' > "$WORK/mf/packages/group/deep/package.json"
mkdir -p "$WORK/outside-ws" && ln -s "$WORK/outside-ws" "$WORK/mf/packages/linked"
jq -n '{name: "m", workspaces: ["packages/*"]}' > "$WORK/mf/package.json"
expect 'a workspace symlinked outside the lockfile directory cannot run' 2 'resolves outside' --npm-lock "$WORK/mf/package-lock.json"
rm "$WORK/mf/packages/linked"
jq -n '{name: "m", workspaces: ["packages/{new,old}"]}' > "$WORK/mf/package.json"
expect 'a brace workspaces glob cannot run (npm and Python match it differently)' 2 'glob syntax this check' --npm-lock "$WORK/mf/package-lock.json"
# A file: dependency on an in-tree package installs that package's own dependencies.
jq -n '{name: "m", dependencies: {new: "file:./packages/new"}}' > "$WORK/mf/package.json"
expect 'a manifest reached through a file: dependency is checked' 1 'packages/new/package\.json: dependencies\.x' --npm-lock "$WORK/mf/package-lock.json"
jq -n '{name: "new", dependencies: {m: "file:../.."}}' > "$WORK/mf/packages/new/package.json"
expect 'a file: dependency cycle ends' 0 '^ok:' --npm-lock "$WORK/mf/package-lock.json"
# A local tarball's own package.json is not read, so a file: tarball is refused.
: > "$WORK/mf/a.tgz"
jq -n '{name: "m", dependencies: {a: "file:./a.tgz"}}' > "$WORK/mf/package.json"
expect 'an in-tree file: tarball fails' 1 'dependencies\.a = .file:\./a\.tgz' --npm-lock "$WORK/mf/package-lock.json"
jq -n '{name: "m", workspaces: ["../*"]}' > "$WORK/mf/package.json"
expect 'a workspaces glob outside the lockfile directory cannot run' 2 'workspaces entry' --npm-lock "$WORK/mf/package-lock.json"
jq -n '{name: "m"}' > "$WORK/mf/package.json"
# Dependency edges inside the lockfile are fetched too.
jq '.packages["node_modules/a"] = {version: "1.0.0", resolved: "https://registry.npmjs.org/a/-/a-1.0.0.tgz", dependencies: {x: "https://npm.example.invalid/x.tgz"}}' "$WORK/mf/package-lock.json" > "$WORK/edge-lock.json"
cp "$WORK/edge-lock.json" "$WORK/mf/package-lock.json"
expect 'an off-registry dependency edge inside the lockfile fails' 1 "packages\['node_modules/a'\]\.dependencies\.x" --npm-lock "$WORK/mf/package-lock.json"
# A file: directory named by a lockfile edge is installed, so its manifest is read
# (node_modules/a is not on disk, as in a CI checkout: npm resolves the path lexically).
mkdir -p "$WORK/fe/packages/b"
jq -n '{name: "b", dependencies: {x: "https://npm.example.invalid/x.tgz"}}' > "$WORK/fe/packages/b/package.json"
jq -n '{lockfileVersion: 3, packages: {"": {name: "m"}, "node_modules/a": {version: "1.0.0", resolved: "https://registry.npmjs.org/a/-/a-1.0.0.tgz", dependencies: {b: "file:../../packages/b"}}}}' > "$WORK/fe/package-lock.json"
expect 'a file: directory in a lockfile edge is followed' 1 'packages/b/package\.json: dependencies\.x' --npm-lock "$WORK/fe/package-lock.json"
jq -n '{name: "b"}' > "$WORK/fe/packages/b/package.json"
expect 'the same edge passes once that manifest is clean' 0 '^ok:' --npm-lock "$WORK/fe/package-lock.json"
# npm defaults an alias with no version to `*`.
jq -n '{lockfileVersion: 3, packages: {"": {name: "m", dependencies: {num: "npm:is-number", sc: "npm:@s/z"}}, "node_modules/num": {name: "is-number", version: "7.0.0", resolved: "https://registry.npmjs.org/is-number/-/is-number-7.0.0.tgz"}, "node_modules/sc": {name: "@s/z", version: "2.0.0", resolved: "https://registry.npmjs.org/@s/z/-/z-2.0.0.tgz"}}}' > "$WORK/fe/package-lock.json"
expect 'npm: aliases without a version pass' 0 '^ok:' --npm-lock "$WORK/fe/package-lock.json"

# npm falls back to `version` when an install location has no `resolved`.
L='.packages["node_modules/left-pad"]'
jq "$L |= (del(.resolved) | .version = \"https://npm.example.invalid/left-pad.tgz\")" "$FIX/off-registry-lock.json" > "$WORK/nores-lock.json"
expect 'install location without resolved fails' 1 'left-pad: install location has no resolved URL' --npm-lock "$WORK/nores-lock.json"
jq '.packages["node_modules/@scope/x"] = {"version": "github:evil/x"}' "$WORK/workspace-lock.json" > "$WORK/scoped-lock.json"
expect 'scoped install location without resolved fails' 1 '@scope/x: install location has no resolved' --npm-lock "$WORK/scoped-lock.json"
jq '.packages["node_modules/@scope/x"] = {"version": "1.0.0"}' "$WORK/workspace-lock.json" > "$WORK/plainver-lock.json"
expect 'no resolved but a plain version passes (npm fetches it from the registry)' 0 '^ok:' --npm-lock "$WORK/plainver-lock.json"
jq '.packages["node_modules/is-number/node_modules/inner"] = {"version": "1.0.0", "inBundle": true} | .packages["node_modules/is-number"].bundleDependencies = ["inner"] | .packages["packages/ws"] = {"version": "0.0.0"} | .packages["node_modules/left-pad"].resolved = "https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz"' \
  "$FIX/off-registry-lock.json" > "$WORK/bundled-lock.json"
expect 'bundled copy and workspace source without resolved pass' 0 '^ok:' --npm-lock "$WORK/bundled-lock.json"
jq '.packages["node_modules/is-number/node_modules/inner"] = {"version": "https://npm.example.invalid/x.tgz", "inBundle": true}' "$WORK/bundled-lock.json" \
  | jq 'del(.packages["node_modules/is-number"].bundleDependencies)' > "$WORK/fakebundle-lock.json"
expect 'inBundle without a bundling parent fails' 1 'inner: install location has no resolved' --npm-lock "$WORK/fakebundle-lock.json"
jq '.packages["node_modules/evil"] = {"version": "https://npm.example.invalid/x.tgz", "inBundle": true}' "$WORK/bundled-lock.json" > "$WORK/topbundle-lock.json"
expect 'top-level inBundle fails' 1 'evil: install location has no resolved' --npm-lock "$WORK/topbundle-lock.json"
jq "$L.resolved = 7" "$FIX/off-registry-lock.json" > "$WORK/num-lock.json"
expect 'non-string resolved fails' 1 'resolved is not a string' --npm-lock "$WORK/num-lock.json"
mkdir -p "$WORK/nested/sub"
jq '.packages["node_modules/left-pad"] = {"resolved": "../outside", "link": true}' "$FIX/off-registry-lock.json" > "$WORK/nested/sub/package-lock.json"
expect 'link is resolved against the lockfile directory' 1 'link \.\./outside leaves' --npm-lock "$WORK/nested/sub/package-lock.json"

# A tracked .npmrc can redirect every download.
printf 'registry=https://npm.example.invalid/\n' > "$WORK/evil.npmrc"
expect '.npmrc registry off the list fails' 1 'evil\.npmrc:1: registry = https://npm\.example\.invalid/ is not an approved' --npmrc "$WORK/evil.npmrc"
printf '; comment\n@acme:registry = "https://npm.example.invalid"\n' > "$WORK/scope.npmrc"
expect '.npmrc scoped registry off the list fails' 1 '@acme:registry' --npmrc "$WORK/scope.npmrc"
# shellcheck disable=SC2016  # a literal ${...} the .npmrc must not be trusted with
printf 'registry=${EVIL_REGISTRY}\n' > "$WORK/var.npmrc"
expect '.npmrc registry from a variable fails' 1 'registry = \$\{EVIL_REGISTRY\}' --npmrc "$WORK/var.npmrc"
for line in 'https-proxy=http://proxy.example.invalid' 'strict-ssl=false' 'replace-registry-host=always' \
            '//npm.example.invalid/:_authToken=x' 'userconfig=/tmp/x' '"registry"=https://npm.example.invalid/' \
            '[section]' 'registry[]=https://npm.example.invalid/' \
            'package-lock=false' 'ignore-scripts=false' 'audit=false' 'min-release-age=0'; do
  printf '%s\n' "$line" > "$WORK/k.npmrc"
  expect ".npmrc line fails: $line" 1 'k\.npmrc:1: ' --npmrc "$WORK/k.npmrc"
done
printf 'registry=https://registry.npmjs.org/\nsave-exact=true\nignore-scripts=true\nmin-release-age=7\n' > "$WORK/ok.npmrc"
expect '.npmrc with the approved registry passes' 0 '^ok:' --npmrc "$WORK/ok.npmrc"

# go.work overrides go.mod.
printf 'go 1.26\n\nuse ./scripts\n\nreplace example.invalid/dep => ../evil\n' > "$LISTED"
expect 'go.work local replace fails' 1 'replace example\.invalid/dep => \.\./evil points at local path' --go-work "$LISTED"
printf 'go 1.26\n\nuse (\n\t./scripts\n\t../../outside\n)\n' > "$LISTED"
expect 'go.work use outside the repository fails' 1 'use \.\./\.\./outside is not a directory inside' --go-work "$LISTED"
printf 'go 1.26\n\nuse ./scripts\n' > "$LISTED"
expect 'go.work use inside the repository passes' 0 '^ok:' --go-work "$LISTED"
mkdir -p "$WORK/outside-mod" && ln -s "$WORK/outside-mod" "$LINK"
printf 'go 1.26\n\nuse ./.registry-test-link\n' > "$LISTED"
expect 'go.work use through a symlink to outside the repository fails' 1 'use \./\.registry-test-link is not a directory inside' --go-work "$LISTED"
printf 'module example.invalid/x\n\ngo 1.26\n\nreplace (\n\texample.invalid/a => ./x\n' > "$WORK/open.mod"
expect 'unterminated replace block cannot run' 2 'unterminated replace block' --go-mod "$WORK/open.mod"

# A listed local replacement passes. The fixture's target resolves to a path
# outside the repository, so list a replacement that resolves inside it.
mkdir -p "$WORK/listed"
printf 'module example.invalid/x\n\ngo 1.26\n\nreplace (\n\texample.invalid/a => ./scripts\n)\n' > "$WORK/listed/go.mod"
cp "$WORK/listed/go.mod" "$LISTED"
printf '{"npm_registry_hosts":["registry.npmjs.org"],"go_local_replacements":["scripts"]}\n' > "$WORK/cfg.json"
expect 'listed in-repo local replace passes' 0 '^ok:' --config "$WORK/cfg.json" --go-mod "$LISTED"
expect 'the same replace fails when not listed' 1 'replace example\.invalid/a => \./scripts' --go-mod "$LISTED"
# With GOPROXY ending in direct, Go fetches an unlisted module path from its origin.
printf 'module example.invalid/x\n\ngo 1.26\n\nreplace example.invalid/a => attacker.invalid/mod v1.0.0\n' > "$WORK/xmod.mod"
expect 'versioned replace to another module fails' 1 'points at another module, attacker\.invalid/mod' --go-mod "$WORK/xmod.mod"
printf '{"npm_registry_hosts":["registry.npmjs.org"],"go_module_replacements":["attacker.invalid/mod"]}\n' > "$WORK/cfgmod.json"
expect 'a listed module replacement passes' 0 '^ok:' --config "$WORK/cfgmod.json" --go-mod "$WORK/xmod.mod"

# Fail closed.
expect 'missing file cannot run' 2 'could not run' --npm-lock "$WORK/absent.json"
jq '.lockfileVersion = 1' "$FIX/off-registry-lock.json" > "$WORK/v1-lock.json"
expect 'lockfileVersion 1 cannot run' 2 'not supported' --npm-lock "$WORK/v1-lock.json"
printf '{"npm_registry_hosts":[]}\n' > "$WORK/empty.json"
expect 'empty host list cannot run' 2 'no npm_registry_hosts' --config "$WORK/empty.json"

echo "check-lockfile-registries: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
