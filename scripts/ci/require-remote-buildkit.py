#!/usr/bin/env python3
"""Require one ready remote node without exposing builder endpoints or names."""

import os
import re
import subprocess
import sys
import tempfile


VERSION = re.compile(
    r"v?(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)"
    r"(?:-[A-Za-z0-9]+(?:[.-][A-Za-z0-9]+)*)?"
    r"(?:\+[A-Za-z0-9]+(?:[.-][A-Za-z0-9]+)*)?"
)


def parse_inspect(raw: bytes) -> str:
    """Return the selected remote node's version; reject ambiguous identity."""
    if (not raw or len(raw) > 65536
            or any(c not in (9, 10) and not 32 <= c <= 126 for c in raw)):
        raise ValueError("invalid inspect response")
    sections = [{}, {}]
    section = 0
    for line in raw.decode("ascii").splitlines():
        # Buildx indents device names and other metadata. Those records must
        # neither satisfy a missing node identity nor count as another node.
        if not line or line[0] in " \t":
            continue
        key, sep, value = line.partition(":")
        if not sep:
            continue
        value = value.strip()
        if key == "Error":
            raise ValueError("builder error")
        if key == "Nodes":
            if section or value:
                raise ValueError("ambiguous nodes")
            section = 1
        elif key in {"Name", "Driver", "Status", "BuildKit version"}:
            if key in sections[section]:
                raise ValueError("duplicate identity")
            sections[section][key] = value
    top, node = sections
    if (section != 1 or set(top) != {"Name", "Driver"}
            or set(node) != {"Name", "Status", "BuildKit version"}
            or not top["Name"] or not node["Name"]
            or top["Driver"] != "remote" or node["Status"] != "running"):
        raise ValueError("unready selected remote builder")
    version = node["BuildKit version"]
    if len(version) > 96 or not VERSION.fullmatch(version):
        raise ValueError("invalid version")
    return version.removeprefix("v")


def main() -> int:
    try:
        if len(sys.argv) != 1:
            raise ValueError("arguments refused")
        # Bound accepted output and query time. Temporary storage avoids an
        # unbounded in-memory capture; this is not a disk-output quota.
        with tempfile.TemporaryFile() as capture:
            subprocess.run(
                ["docker", "buildx", "inspect"], check=True,
                stdout=capture, stderr=subprocess.DEVNULL,
                env={**os.environ, "LC_ALL": "C"}, timeout=15,
            )
            capture.seek(0)
            version = parse_inspect(capture.read(65537))
    except (OSError, ValueError, subprocess.SubprocessError):
        print("error: selected BuildKit must be one ready remote node with a valid version",
              file=sys.stderr)
        return 1
    print(f"BuildKit selected driver remote; status running; version {version}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
