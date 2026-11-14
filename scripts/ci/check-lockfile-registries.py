#!/usr/bin/env python3
"""Fail when a lockfile resolves outside the approved registries (#271).

npm: every `resolved` URL in a package-lock.json or npm-shrinkwrap.json must be
https on a host in `npm_registry_hosts`. A workspace link (`"link": true`) whose
`resolved` is a relative path inside the lockfile's directory is not a download
and is allowed. Anything else fails: another host, plain http, git+ssh, file:,
a tarball path, or a link that leaves the directory.

npm parses URLs the WHATWG way, so a `resolved` URL must match a strict
`https://<host>/...` shape (no userinfo, port, backslash or escape in the
authority) before its host is compared.

Go: a go.mod `replace` whose target carries a version is a module path,
fetched through the module proxy and checked against the checksum database, so
it passes. A target without a version is a filesystem path (Go's own rule,
however it is spelled or quoted); it bypasses both, so it passes only when the
directory it names is inside the repository and listed in
`go_local_replacements`. GOPROXY and GOPRIVATE settings are out of scope.

Every npm install location must carry an approved `resolved` URL: without
one npm falls back to `version`, which may be a URL. A tracked .npmrc may set
`registry` or `@scope:registry` only to an approved host, because npm rewrites
registry.npmjs.org in `resolved` to the configured registry. A go.work is read
like a go.mod, and each `use` must name a directory inside the repository.

The approved hosts and paths live in .github/ci/approved-registries.json, so a
change to them is a visible diff.

Usage:
  check-lockfile-registries.py [--config PATH] [--npm-lock FILE]... [--go-mod FILE]...
                               [--go-work FILE]... [--npmrc FILE]...
With no file option it checks every tracked package-lock.json,
npm-shrinkwrap.json, go.mod, go.work and .npmrc (`git ls-files`).

Exit 0 = clean. Exit 1 = a violation. Exit 2 = the check could not run (bad
config, unreadable file, unsupported lockfile version); that is a failure too.
"""

import argparse
import json
import os
import re
import subprocess
import sys
from urllib.parse import urlparse

NPM_NAMES = {"package-lock.json", "npm-shrinkwrap.json"}
INSTALL_KEY = re.compile(r"(^|/)node_modules/(@[^/]+/)?[^/]+$")
SAFE_URL = re.compile(r"^https://(?P<host>[A-Za-z0-9.-]+)/[^\\\s]*$")


class CannotRun(Exception):
    pass


def load_json(path):
    try:
        with open(path, encoding="utf-8") as fh:
            return json.load(fh)
    except (OSError, ValueError) as exc:
        raise CannotRun(f"cannot read {path}: {exc}") from exc


def inside(base, rel):
    """True when rel, joined to base, stays inside base."""
    if os.path.isabs(rel):
        return False
    base = os.path.abspath(base)
    target = os.path.abspath(os.path.join(base, rel))
    return target == base or target.startswith(base + os.sep)


def npm_violations(path, hosts):
    lock = load_json(path)
    version = lock.get("lockfileVersion")
    if version not in (2, 3) or not isinstance(lock.get("packages"), dict):
        raise CannotRun(f"{path}: lockfileVersion {version!r} is not supported (need 2 or 3)")
    out = []
    base = os.path.dirname(path) or "."
    for key, entry in lock["packages"].items():
        if not isinstance(entry, dict):
            raise CannotRun(f"{path}: packages[{key!r}] is not an object")
        name = key.rsplit("node_modules/", 1)[-1] or "(root)"
        resolved = entry.get("resolved")
        if resolved is not None and not isinstance(resolved, str):
            out.append(f"{path}: {name}: resolved is not a string ({resolved!r})")
            continue
        if entry.get("link") is True:
            if isinstance(resolved, str) and ":" not in resolved and inside(base, resolved):
                continue
            out.append(f"{path}: {name}: link {resolved} leaves the lockfile directory")
            continue
        if resolved is None:
            # npm fetches every install location. Without `resolved` it falls
            # back to `version`, which may itself be a URL or git spec. Bundled
            # copies come inside their parent's tarball, and other keys
            # (workspace sources, directories inside a package) are not fetched.
            if INSTALL_KEY.search(key) and entry.get("inBundle") is not True:
                out.append(f"{path}: {name}: install location has no resolved URL "
                           f"(version {entry.get('version')!r})")
            continue
        # Anchored match, not urlparse alone: Python and npm (WHATWG URL) parse
        # `\`, `@`, ports and escapes in the authority differently, so
        # `https://evil.example\@registry.npmjs.org/x` reads as the approved
        # host to urlparse and as evil.example to npm. The authority must be a
        # bare host name, and nothing after it may hold a backslash.
        match = SAFE_URL.match(resolved)
        if match and match.group("host") in hosts:
            continue
        url = urlparse(resolved)
        where = (match and match.group("host")) or url.hostname or f"{url.scheme or 'no'} scheme"
        out.append(
            f"{path}: {name}: resolved from {where} ({resolved}); "
            f"approved npm hosts: {', '.join(sorted(hosts)) or 'none'}"
        )
    return out


def go_directives(text, verb, path):
    """Yield (line_number, arguments) for each `verb` directive in a go.mod or go.work."""
    start = re.compile(rf"{verb}\b")
    block = False
    opened = 0
    for number, raw in enumerate(text.splitlines(), 1):
        line = raw.split("//", 1)[0].strip()
        if not line:
            continue
        if block:
            if line == ")":
                block = False
            else:
                yield number, line
        elif start.match(line):
            rest = line[len(verb):].strip()
            if rest == "(":
                block, opened = True, number
            elif rest:
                yield number, rest
    if block:
        raise CannotRun(f"{path}:{opened}: unterminated {verb} block")


# Go's tokens: an interpreted "..." string (escapes refused, not guessed), a raw
# `...` string, or a bare word. The whole right-hand side must be one target and
# at most one version, or the check cannot run.
GO_TOKEN = r'"[^"\\]*"|`[^`]*`|[^\s"`]+'
GO_TARGET = re.compile(rf"\s*({GO_TOKEN})(?:\s+({GO_TOKEN}))?\s*")
GO_VERSION = re.compile(r"v[0-9][^\s\"`]*")


def parse_target(rhs, path, number):
    """Return (dest, version or None) for a replace right-hand side."""
    match = GO_TARGET.fullmatch(rhs)
    if not match:
        raise CannotRun(f"{path}:{number}: cannot parse replace target: {rhs.strip()}")
    dest, version = match.group(1), match.group(2)
    if dest[:1] in ('"', "`"):
        dest = dest[1:-1]
    if version is not None and not GO_VERSION.fullmatch(version):
        raise CannotRun(f"{path}:{number}: replace version is not a version: {version}")
    return dest, version


def go_violations(path, root, local_allowed, work=False):
    try:
        with open(path, encoding="utf-8") as fh:
            text = fh.read()
    except OSError as exc:
        raise CannotRun(f"cannot read {path}: {exc}") from exc
    out = []
    if work:
        # go.work `use` adds a module directory to the build. Inside the
        # repository its code is in the diff; outside it is not.
        for number, arg in go_directives(text, "use", path):
            dest, version = parse_target(arg, path, number)
            here = os.path.dirname(os.path.abspath(path))
            if version is not None or os.path.isabs(dest) or not inside(root, os.path.relpath(os.path.join(here, dest), root)):
                out.append(f"{path}:{number}: use {arg} is not a directory inside the repository")
    for number, directive in go_directives(text, "replace", path):
        if "=>" not in directive:
            raise CannotRun(f"{path}:{number}: cannot parse replace directive: {directive}")
        dest, version = parse_target(directive.split("=>", 1)[1], path, number)
        # Go's own rule: a replacement with a version is a module path, fetched
        # through the module proxy. One without a version is a filesystem path,
        # whatever it looks like (`.`, `"./x"`, `..\x`, an absolute path).
        if version is not None:
            continue
        resolved = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(path)), dest))
        rel = os.path.relpath(resolved, root)
        if not os.path.isabs(dest) and inside(root, rel) and rel in local_allowed:
            continue
        out.append(
            f"{path}:{number}: replace {directive} points at local path {dest}, "
            f"which is not in go_local_replacements"
        )
    return out


REGISTRY_URL = re.compile(r"^https://(?P<host>[A-Za-z0-9.-]+)/?$")


def npmrc_violations(path, hosts):
    """A tracked .npmrc may point `registry` or `@scope:registry` only at an approved host.

    npm rewrites registry.npmjs.org in every `resolved` URL to the configured
    registry (replace-registry-host defaults to npmjs), so an .npmrc next to an
    untouched lockfile can redirect every download.
    """
    try:
        with open(path, encoding="utf-8") as fh:
            lines = fh.read().splitlines()
    except OSError as exc:
        raise CannotRun(f"cannot read {path}: {exc}") from exc
    out = []
    for number, raw in enumerate(lines, 1):
        line = raw.strip()
        if not line or line[0] in "#;" or "=" not in line:
            continue
        key, value = (part.strip() for part in line.split("=", 1))
        if key != "registry" and not key.endswith(":registry"):
            continue
        value = value.strip("'\"")
        match = REGISTRY_URL.match(value)
        if not (match and match.group("host") in hosts):
            out.append(f"{path}:{number}: {key} = {value} is not an approved npm registry")
    return out


def tracked(root):
    try:
        files = subprocess.run(
            ["git", "-C", root, "ls-files", "-z"], check=True, capture_output=True
        ).stdout.decode().split("\0")
    except (OSError, subprocess.CalledProcessError) as exc:
        raise CannotRun(f"git ls-files failed: {exc}") from exc
    npm = [f for f in files if os.path.basename(f) in NPM_NAMES]
    gomod = [f for f in files if os.path.basename(f) in ("go.mod", "go.work")]
    npmrc = [f for f in files if os.path.basename(f) == ".npmrc"]
    if not npm and not gomod:
        raise CannotRun("git ls-files found no lockfile and no go.mod; refusing to pass an empty scan")
    # Paths relative to the working directory, so messages name them as a reader would.
    rel = lambda names: [os.path.relpath(os.path.join(root, f)) for f in names]
    return rel(npm), rel(gomod), rel(npmrc)


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    ap.add_argument("--config")
    ap.add_argument("--npm-lock", action="append", default=[])
    ap.add_argument("--go-mod", action="append", default=[])
    ap.add_argument("--go-work", action="append", default=[])
    ap.add_argument("--npmrc", action="append", default=[])
    args = ap.parse_args()
    try:
        root = subprocess.run(
            ["git", "rev-parse", "--show-toplevel"], check=True, capture_output=True, text=True
        ).stdout.strip()
        config = load_json(args.config or os.path.join(root, ".github/ci/approved-registries.json"))
        hosts = set(config.get("npm_registry_hosts", []))
        local_allowed = {os.path.normpath(p) for p in config.get("go_local_replacements", [])}
        if not hosts:
            raise CannotRun("config lists no npm_registry_hosts")
        explicit = args.npm_lock or args.go_mod or args.go_work or args.npmrc
        if explicit:
            npm, npmrc = args.npm_lock, args.npmrc
            gomod = args.go_mod + args.go_work
            work = set(args.go_work)
        else:
            npm, gomod, npmrc = tracked(root)
            work = {f for f in gomod if os.path.basename(f) == "go.work"}
        problems = []
        for path in npm:
            problems += npm_violations(path, hosts)
        for path in gomod:
            problems += go_violations(path, root, local_allowed, work=path in work)
        for path in npmrc:
            problems += npmrc_violations(path, hosts)
    except (CannotRun, subprocess.CalledProcessError, OSError) as exc:
        print(f"::error::lockfile registry check could not run: {exc}")
        return 2
    for problem in problems:
        print(f"::error::{problem}")
    if problems:
        print(f"{len(problems)} off-registry source(s). Fix the lockfile or go.mod, or, if the "
              "source is deliberate, add it to .github/ci/approved-registries.json in this PR.")
        return 1
    print(f"ok: {len(npm)} npm lockfile(s), {len(gomod)} go.mod/go.work file(s) and "
          f"{len(npmrc)} .npmrc file(s) use approved sources")
    return 0


if __name__ == "__main__":
    sys.exit(main())
