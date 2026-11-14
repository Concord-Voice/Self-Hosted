#!/usr/bin/env python3
"""Fail when a lockfile resolves outside the approved registries (#271).

npm: every `resolved` URL in a package-lock.json or npm-shrinkwrap.json must be
https on a host in `npm_registry_hosts`. A workspace link (`"link": true`) whose
`resolved` is a relative path inside the lockfile's directory is not a download
and is allowed. Anything else fails: another host, plain http, git+ssh, file:,
a tarball path, or a link that leaves the directory.

Go: a go.mod `replace` to another module path is fetched through the module
proxy and checked against the checksum database, so it passes. A `replace` to a
filesystem path bypasses both, so it passes only when the directory it names
is inside the repository and listed in `go_local_replacements`. GOPROXY and
GOPRIVATE settings are out of scope.

The approved hosts and paths live in .github/ci/approved-registries.json, so a
change to them is a visible diff.

Usage:
  check-lockfile-registries.py [--config PATH] [--npm-lock FILE]... [--go-mod FILE]...
With no file option it checks every tracked package-lock.json,
npm-shrinkwrap.json and go.mod (`git ls-files`).

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
    for key, entry in lock["packages"].items():
        resolved = entry.get("resolved") if isinstance(entry, dict) else None
        if not isinstance(resolved, str):
            continue
        name = key.rsplit("node_modules/", 1)[-1] or "(root)"
        if entry.get("link") is True and "://" not in resolved and ":" not in resolved:
            if inside(".", resolved):
                continue
            out.append(f"{path}: {name}: link {resolved} leaves the lockfile directory")
            continue
        url = urlparse(resolved)
        if url.scheme == "https" and url.hostname in hosts:
            continue
        where = url.hostname or f"{url.scheme or 'no'} scheme"
        out.append(
            f"{path}: {name}: resolved from {where} ({resolved}); "
            f"approved npm hosts: {', '.join(sorted(hosts)) or 'none'}"
        )
    return out


REPLACE = re.compile(r"replace\b")


def go_replaces(text):
    """Yield (line_number, directive) for each replace directive in a go.mod."""
    block = False
    for number, raw in enumerate(text.splitlines(), 1):
        line = raw.split("//", 1)[0].strip()
        if not line:
            continue
        if block:
            if line == ")":
                block = False
            else:
                yield number, line
        elif REPLACE.match(line):
            rest = line[len("replace"):].strip()
            if rest == "(":
                block = True
            elif rest:
                yield number, rest


def go_violations(path, root, local_allowed):
    try:
        with open(path, encoding="utf-8") as fh:
            text = fh.read()
    except OSError as exc:
        raise CannotRun(f"cannot read {path}: {exc}") from exc
    out = []
    for number, directive in go_replaces(text):
        if "=>" not in directive:
            raise CannotRun(f"{path}:{number}: cannot parse replace directive: {directive}")
        target = directive.split("=>", 1)[1].split()
        if not target:
            raise CannotRun(f"{path}:{number}: replace directive has no target: {directive}")
        dest = target[0]
        if not (dest.startswith(("./", "../")) or os.path.isabs(dest)):
            continue  # module path: fetched through the module proxy
        resolved = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(path)), dest))
        rel = os.path.relpath(resolved, root)
        if not os.path.isabs(dest) and inside(root, rel) and rel in local_allowed:
            continue
        out.append(
            f"{path}:{number}: replace {directive} points at local path {dest}, "
            f"which is not in go_local_replacements"
        )
    return out


def tracked(root):
    try:
        files = subprocess.run(
            ["git", "-C", root, "ls-files", "-z"], check=True, capture_output=True
        ).stdout.decode().split("\0")
    except (OSError, subprocess.CalledProcessError) as exc:
        raise CannotRun(f"git ls-files failed: {exc}") from exc
    npm = [f for f in files if os.path.basename(f) in NPM_NAMES]
    gomod = [f for f in files if os.path.basename(f) == "go.mod"]
    if not npm and not gomod:
        raise CannotRun("git ls-files found no lockfile and no go.mod; refusing to pass an empty scan")
    # Paths relative to the working directory, so messages name them as a reader would.
    return ([os.path.relpath(os.path.join(root, f)) for f in npm],
            [os.path.relpath(os.path.join(root, f)) for f in gomod])


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    ap.add_argument("--config")
    ap.add_argument("--npm-lock", action="append", default=[])
    ap.add_argument("--go-mod", action="append", default=[])
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
        if args.npm_lock or args.go_mod:
            npm, gomod = args.npm_lock, args.go_mod
        else:
            npm, gomod = tracked(root)
        problems = []
        for path in npm:
            problems += npm_violations(path, hosts)
        for path in gomod:
            problems += go_violations(path, root, local_allowed)
    except (CannotRun, subprocess.CalledProcessError, OSError) as exc:
        print(f"::error::lockfile registry check could not run: {exc}")
        return 2
    for problem in problems:
        print(f"::error::{problem}")
    if problems:
        print(f"{len(problems)} off-registry source(s). Fix the lockfile or go.mod, or, if the "
              "source is deliberate, add it to .github/ci/approved-registries.json in this PR.")
        return 1
    print(f"ok: {len(npm)} npm lockfile(s) and {len(gomod)} go.mod file(s) use approved sources")
    return 0


if __name__ == "__main__":
    sys.exit(main())
