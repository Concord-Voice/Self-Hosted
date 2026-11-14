#!/usr/bin/env python3
"""Check that a self-host bundle SBOM lists what the bundle locks (#273).

The SBOM must be CycloneDX JSON, and it must list every npm package locked in
the tree's package-lock.json files and every Go module that a go.mod requires,
at the locked version. A syft upgrade that drops a cataloger, or a config that
drops dev dependencies, then fails the release instead of shipping a thin SBOM.

Usage: check-selfhost-sbom.py --sbom FILE --tree DIR
Exit 0 = complete. Exit 1 = something is missing or the file is not CycloneDX.
"""

import argparse
import json
import re
import sys
from pathlib import Path
from urllib.parse import unquote

INSTALL_KEY = re.compile(r"(^|/)node_modules/((@[^/]+/)?[^/]+)$")
REQUIRE = re.compile(r"^\s*(?:require\s+)?([^\s()]+)\s+(v\S+)")


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
        for key, entry in json.loads(lock.read_text()).get("packages", {}).items():
            m = INSTALL_KEY.search(key)
            if m and not entry.get("link") and entry.get("version"):
                want.add(("npm", entry.get("name") or m.group(2), entry["version"]))
    for mod in tree.rglob("go.mod"):
        block = False
        for line in mod.read_text().splitlines():
            line = line.split("//", 1)[0].strip()
            if line.startswith("require ("):
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
    args = ap.parse_args()
    sbom = json.loads(args.sbom.read_text())
    if sbom.get("bomFormat") != "CycloneDX":
        print(f"::error::{args.sbom.name}: bomFormat is {sbom.get('bomFormat')!r}, not CycloneDX")
        return 1
    want, have = locked(args.tree), listed(sbom)
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
