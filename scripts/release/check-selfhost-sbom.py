#!/usr/bin/env python3
"""Check that a self-host bundle SBOM lists what the bundle locks (#273).

The SBOM must be CycloneDX JSON, and it must list every npm package locked in
the tree's package-lock.json files and every Go module that a go.mod requires,
at the locked version. A syft upgrade that drops a cataloger, or a config that
drops dev dependencies, then fails the release instead of shipping a thin SBOM.

It also checks that the file is one JSON value whose metadata names
concord-selfhost at the given version. A go.mod `replace` or a lockfile older
than version 2 fails closed: this check does not model them.

Usage: check-selfhost-sbom.py --sbom FILE --tree DIR --version X.Y.Z
Exit 0 = complete. Exit 1 = missing packages, wrong metadata, or a tree this
check cannot read.
"""

import argparse
import json
import re
import sys
from pathlib import Path
from urllib.parse import unquote

INSTALL_KEY = re.compile(r"(^|/)node_modules/((@[^/]+/)?[^/]+)$")
REQUIRE = re.compile(r"^\s*(?:require\s+)?([^\s()]+)\s+(v\S+)")


class Unsupported(Exception):
    pass


def listed(sbom):
    out = set()
    for comp in sbom.get("components") or []:
        purl = comp.get("purl") or ""
        m = re.match(r"^pkg:(npm|golang)/([^@?#]+)@([^?#]+)", purl)
        if m:
            out.add((m.group(1), unquote(m.group(2)), unquote(m.group(3))))
    return out


def locked(tree):
    want = set()
    for lock in tree.rglob("package-lock.json"):
        if "node_modules" in lock.parts:
            continue
        data = json.loads(lock.read_text())
        if data.get("lockfileVersion") not in (2, 3) or not isinstance(data.get("packages"), dict):
            raise Unsupported(f"{lock}: lockfile version {data.get('lockfileVersion')!r} is not 2 or 3")
        for key, entry in data["packages"].items():
            m = INSTALL_KEY.search(key)
            if not m or entry.get("link"):
                continue
            if not entry.get("version"):
                raise Unsupported(f"{lock}: {key} has no version")
            want.add(("npm", entry.get("name") or m.group(2), entry["version"]))
    for mod in tree.rglob("go.mod"):
        block = False
        for line in mod.read_text().splitlines():
            line = line.split("//", 1)[0].strip()
            if re.match(r"replace\b", line):
                raise Unsupported(f"{mod}: replace is not modelled by this check")
            if re.match(r"require\s*\($", line):
                block = True
                continue
            if block and line == ")":
                block = False
                continue
            if block or line.startswith("require "):
                m = REQUIRE.match(line)
                if m:
                    want.add(("golang", m.group(1), m.group(2)))
    return want


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    ap.add_argument("--sbom", required=True, type=Path)
    ap.add_argument("--tree", required=True, type=Path)
    ap.add_argument("--version", required=True)
    args = ap.parse_args()
    try:
        sbom = json.loads(args.sbom.read_text())  # one JSON value: trailing data fails
        want = locked(args.tree)
    except (ValueError, OSError, Unsupported) as exc:
        print(f"::error::{args.sbom.name}: cannot check: {exc}")
        return 1
    if not isinstance(sbom, dict) or sbom.get("bomFormat") != "CycloneDX":
        print(f"::error::{args.sbom.name}: not a CycloneDX document")
        return 1
    comp = (sbom.get("metadata") or {}).get("component") or {}
    if (comp.get("name"), comp.get("version")) != ("concord-selfhost", args.version):
        print(f"::error::{args.sbom.name}: metadata names {comp.get('name')!r} {comp.get('version')!r}, "
              f"not 'concord-selfhost' {args.version!r}")
        return 1
    have = listed(sbom)
    missing = sorted(want - have)
    for kind, name, version in missing[:50]:
        print(f"::error::{args.sbom.name}: missing {kind} {name}@{version}")
    if missing:
        print(f"{len(missing)} of {len(want)} locked packages are not in the SBOM")
        return 1
    kinds = {k for k, _, _ in want}
    if not {"npm", "golang"} <= kinds:
        print(f"::error::the tree locks no {' or '.join(sorted({'npm', 'golang'} - kinds))} packages")
        return 1
    print(f"ok: {args.sbom.name} lists all {len(want)} locked npm packages and Go modules")
    return 0


if __name__ == "__main__":
    sys.exit(main())
